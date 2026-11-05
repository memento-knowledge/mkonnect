package gitsync

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// gitSide says which end of the sync a failing git invocation was talking to, because the same
// symptom means different things on each: a 403 from the origin is the customer's credential, a 403
// from the replica is Memento's push token.
type gitSide int

const (
	originSide  gitSide = iota // init / fetch from the customer origin
	replicaSide                // push to the Memento replica
)

// maxErrorDetail bounds the raw detail carried for an unclassified failure, so a verbose git error
// cannot bloat every status message.
const maxErrorDetail = 200

var (
	// urlRE, pathRE and hostRE find the remote locations, local paths and bare hostnames/addresses
	// git (or curl, for a proxy) echoes in its errors. They are scrubbed from any raw detail:
	// error_detail must never be a URL, host, or path, because each is customer infrastructure.
	urlRE  = regexp.MustCompile(`https?://\S+`)
	pathRE = regexp.MustCompile(`(^|[\s(])/[^\s)]+`)
	hostRE = regexp.MustCompile(`\b(?:[A-Za-z0-9-]+\.)+[A-Za-z0-9-]+(?::\d+)?\b`)

	// refStatusRE matches the per-ref status lines git prints for a fetch or push (" * [new tag] v1
	// -> v1", " ! [remote rejected] main -> main (reason)"). Their ref NAMES are arbitrary text the
	// remote chose, so they must not steer classification or reach error_detail.
	refStatusRE    = regexp.MustCompile(`^ [!*+=-] `)
	remoteHeaderRE = regexp.MustCompile(`^(To|From) \S+$`)
	bracketRE      = regexp.MustCompile(`\[[^\]]*\]`)
	reasonRE       = regexp.MustCompile(`\(([^()]*)\)\s*$`)

	// remoteTextRE finds where a line turns into text the remote end chose: "fatal: remote error: <msg>"
	// (an ERR packet), "error: remote unpack failed: <msg>", or a "remote: <msg>" sideband line. A
	// server can put anything there, including the credential it was sent, so rawDetail keeps the
	// label and withholds the text.
	remoteTextRE = regexp.MustCompile(`(?i)(^remote:|\bremote (?:error|unpack failed):)\s*\S.*$`)
)

var (
	authMarkers = []string{
		"authentication failed", "could not read username", "could not read password",
		"terminal prompts disabled", "401 unauthorized", "403 forbidden", "http basic: access denied",
	}
	connMarkers = []string{
		"could not resolve host", "couldn't resolve host", "could not resolve proxy", "connection refused", "connection timed out",
		"timed out", "could not connect", "failed to connect", "network is unreachable",
		"no route to host", "connection reset", "ssl", "tls", "certificate",
	}
	diskMarkers   = []string{"no space left on device", "disk quota exceeded"}
	memoryMarkers = []string{"out of memory", "cannot allocate memory", "std::bad_alloc"}
	// replicaQuotaMarkers are the replica refusing the push over a size or quota limit. That is the
	// replica's own limit, so it is replica_rejected: the fix is to raise the replica's quota, not
	// the connector's repository ceiling (ErrRepositoryOverLimit).
	replicaQuotaMarkers = []string{"size limit", "maximum allowed size", "too large", "quota"}
	rejectedMarkers     = []string{"remote rejected", "hook declined", "pre-receive hook", "unpacker error"}

	// childKilledRE matches git itself reporting that one of its child processes (index-pack,
	// pack-objects, git-remote-https) was SIGKILLed: "error: index-pack died of signal 9". With no
	// deadline that is the kernel's OOM killer. It is anchored to git's own "error:"/"fatal:" prefix
	// followed directly by the command, so text a server relays ("error: remote unpack failed: ...",
	// a rejected-ref reason) cannot match; and it is signal 9 as a whole number ("signal 90" is not).
	childKilledRE = regexp.MustCompile(`(?m)^\s*(?:error|fatal): \S+ died of signal 9\b`)
)

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// classifyGitFailure maps a failed git invocation to a typed error_type and, for the fallback
// "unclassified" type only, a sanitized single-line detail. It returns ("", "") when err is nil. It
// never reports success for a failure it does not recognise. As in classifyLsRemote, the URL git
// echoes (single-quoted) is removed before keyword matching so a hostname containing "tls" or "ssl"
// cannot be mistaken for a connection failure.
//
// A 404 / "repository not found" from the origin is reported as origin_access_denied: forges answer
// 404 for a repository the credential cannot see, and the contract has no separate "not found" code.
// An HTTP 407 (proxy authentication) is not the customer's credential being rejected, so it is
// origin_unreachable. A deadline is attributed by side: a push that outlives its deadline is
// push_timeout, a fetch that does is origin_unreachable.
//
// A replica refusal over size or quota (HTTP 413, "size limit", "quota", ...) is replica_rejected,
// whether it arrives as a status or as the reason in a rejected-ref line. repository_over_limit is
// never produced here: it is reserved for the connector's own repository-size ceiling.
func classifyGitFailure(side gitSide, err error, stderr []byte, tokenExpired bool) (errorType, detail string) {
	if err == nil {
		return "", ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if side == replicaSide {
			return ErrPushTimeout, ""
		}
		return ErrOriginUnreachable, ""
	}

	s := matchText(stderr)

	// Local faults come first and apply to either side. Lines prefixed "remote:" are the far end
	// speaking, so a replica reporting its own full disk is not our disk exhaustion.
	local := localLines(s)
	if containsAny(local, diskMarkers) {
		return ErrLocalDiskExhausted, ""
	}
	// SIGKILL with no deadline is the kernel's OOM killer (a deadline kill returned above). That is
	// either git itself ("signal: killed") or one of its children, which git reports on a local line.
	if containsAny(local, memoryMarkers) || childKilledRE.MatchString(local) ||
		strings.Contains(err.Error(), "signal: killed") {
		return ErrJobOutOfMemory, ""
	}

	status := ""
	if m := httpStatusRE.FindStringSubmatch(s); m != nil {
		status = m[1]
	}
	serverError := len(status) == 3 && status[0] == '5'
	authFailed := status == "401" || status == "403" || containsAny(s, authMarkers)

	if side == originSide {
		switch {
		case authFailed,
			status == "404",
			strings.Contains(s, "repository") && strings.Contains(s, "not found"):
			return ErrOriginAccessDenied, ""
		case status == "407", serverError, containsAny(s, connMarkers):
			return ErrOriginUnreachable, ""
		}
		return ErrUnclassified, rawDetail(err, stderr)
	}

	switch {
	case status == "413", containsAny(s, replicaQuotaMarkers):
		return ErrReplicaRejected, ""
	case authFailed:
		if tokenExpired {
			return ErrPushTokenExpired, ""
		}
		return ErrCredentialRejected, ""
	case containsAny(s, rejectedMarkers):
		return ErrReplicaRejected, ""
	case status == "407", serverError, containsAny(s, connMarkers):
		return ErrReplicaUnreachable, ""
	}
	return ErrUnclassified, rawDetail(err, stderr)
}

// scrubRefLines removes what a remote controls but is not an explanation: the "To <url>" / "From
// <url>" header lines, and the ref names in per-ref status lines. A rejected ref keeps its status
// and git's parenthesised reason (" ! [remote rejected] main -> main (hook declined)" becomes
// "[remote rejected] (hook declined)"), which is the part that explains the failure.
func scrubRefLines(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		switch {
		case remoteHeaderRE.MatchString(line):
		case refStatusRE.MatchString(line) && strings.Contains(line, "->"):
			kept := bracketRE.FindString(line)
			if m := reasonRE.FindStringSubmatch(line); m != nil {
				kept += " (" + m[1] + ")"
			}
			if kept = strings.TrimSpace(kept); kept != "" {
				out = append(out, kept)
			}
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// matchText is the text keyword matching runs on: lower-cased, with ref names and remote headers
// scrubbed and the single-quoted URL git echoes removed (so a hostname or branch containing "tls"
// or "quota" cannot be mistaken for a failure of that kind).
func matchText(stderr []byte) string {
	return strings.ToLower(quotedRE.ReplaceAllString(scrubRefLines(string(stderr)), "''"))
}

// isCorruptMirror reports whether a failed fetch says the LOCAL mirror is unusable, which is
// repaired by rebuilding the mirror. Lines the remote sent ("remote:") and ref names are excluded:
// an origin reporting its own corruption, or a branch named "fix-corrupt-cache", must not make the connector
// delete a healthy mirror and download the whole repository again.
func isCorruptMirror(stderr []byte) bool {
	return containsAny(localLines(matchText(stderr)), corruptMirrorMarkers)
}

// localLines returns s with every "remote:"-prefixed line removed.
func localLines(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "remote:") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// rawDetail returns a short, single-line, scrubbed description of an unrecognised failure: git's
// first "fatal:"/"error:" line (else its last non-empty line, else the process error), with quoted
// text, URLs and absolute paths removed, control characters dropped, and the length bounded.
func rawDetail(err error, stderr []byte) string {
	line := pickDetailLine(scrubRefLines(string(stderr)))
	if line == "" && err != nil {
		line = err.Error()
	}
	line = remoteTextRE.ReplaceAllString(line, "${1} <text from the remote withheld>")
	line = quotedRE.ReplaceAllString(line, "''")
	line = urlRE.ReplaceAllString(line, "<url>")
	line = pathRE.ReplaceAllString(line, "${1}<path>")
	line = hostRE.ReplaceAllString(line, "<host>")
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, line)
	line = strings.Join(strings.Fields(line), " ")
	if len(line) > maxErrorDetail {
		line = line[:maxErrorDetail]
		for !utf8.ValidString(line) {
			line = line[:len(line)-1]
		}
	}
	if line == "" {
		return "git failed"
	}
	return line
}

func pickDetailLine(stderr string) string {
	var last string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		last = line
		l := strings.ToLower(line)
		if strings.HasPrefix(l, "fatal:") || strings.HasPrefix(l, "error:") {
			return line
		}
	}
	return last
}
