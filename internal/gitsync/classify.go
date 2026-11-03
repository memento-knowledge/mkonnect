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
	originSide  gitSide = iota // clone / fetch from the customer origin
	replicaSide                // push to the Memento replica
)

// maxErrorDetail bounds the raw detail carried for an unclassified failure, so a verbose git error
// cannot bloat every status message.
const maxErrorDetail = 200

var (
	// urlRE and pathRE find the remote locations and local paths git echoes in its errors. They are
	// scrubbed from any raw detail: error_detail must never be a URL, host, or path.
	urlRE  = regexp.MustCompile(`https?://\S+`)
	pathRE = regexp.MustCompile(`(^|[\s(])/[^\s)]+`)
)

var (
	authMarkers = []string{
		"authentication failed", "could not read username", "could not read password",
		"terminal prompts disabled", "401 unauthorized", "403 forbidden", "http basic: access denied",
	}
	connMarkers = []string{
		"could not resolve host", "couldn't resolve host", "connection refused", "connection timed out",
		"timed out", "could not connect", "failed to connect", "network is unreachable",
		"no route to host", "connection reset", "ssl", "tls", "certificate",
	}
	diskMarkers   = []string{"no space left on device", "disk quota exceeded"}
	memoryMarkers = []string{"out of memory", "cannot allocate memory", "std::bad_alloc"}
	// overLimitMarkers are the replica refusing the push because the repository is too big.
	overLimitMarkers = []string{"size limit", "maximum allowed size", "too large", "quota"}
	rejectedMarkers  = []string{"remote rejected", "hook declined", "pre-receive hook", "unpacker error"}
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
// push_timeout, a clone or fetch that does is origin_unreachable.
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

	s := strings.ToLower(string(quotedRE.ReplaceAll(stderr, []byte("''"))))

	// Local faults come first and apply to either side. Lines prefixed "remote:" are the far end
	// speaking, so a replica reporting its own full disk is not our disk exhaustion.
	local := localLines(s)
	if containsAny(local, diskMarkers) {
		return ErrLocalDiskExhausted, ""
	}
	// SIGKILL with no deadline is the kernel's OOM killer (a deadline kill returned above).
	if containsAny(local, memoryMarkers) || strings.Contains(err.Error(), "signal: killed") {
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
	case status == "413", containsAny(s, overLimitMarkers):
		return ErrRepositoryOverLimit, ""
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
	line := pickDetailLine(string(stderr))
	if line == "" && err != nil {
		line = err.Error()
	}
	line = quotedRE.ReplaceAllString(line, "''")
	line = urlRE.ReplaceAllString(line, "<url>")
	line = pathRE.ReplaceAllString(line, "${1}<path>")
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
