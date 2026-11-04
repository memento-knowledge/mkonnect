package gitsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// OriginResult is the outcome of a reachability + access probe against an origin base URL. The
// three values map to the origin_check.result wire values.
type OriginResult string

const (
	OriginReachable    OriginResult = "reachable"
	OriginAccessDenied OriginResult = "access_denied"
	OriginUnreachable  OriginResult = "unreachable"
)

// probeTimeout bounds a single ls-remote probe so a hung network call cannot stall the caller.
const probeTimeout = 15 * time.Second

// maxGitStderr caps how much of git's stderr is retained for classification.
const maxGitStderr = 8 << 10

// OriginProber probes an origin's reachability and access. A non-nil error is a LOCAL fault — for
// example the git binary is missing — which the caller MUST NOT report as an origin result;
// instead it should omit origin_check and log the fault, because "git is not installed" is not a
// statement about the customer's origin.
type OriginProber interface {
	Probe(ctx context.Context, e LocalEntry) (OriginResult, error)
}

// gitProber probes with `git ls-remote`. run is a seam so tests can assert the argv/env without a
// real subprocess; it defaults to runGit.
type gitProber struct {
	run func(ctx context.Context, argv, env []string, dir string) (stderr []byte, err error)
}

// NewGitProber returns a prober backed by the git binary.
func NewGitProber() *gitProber { return &gitProber{run: runGit} }

// GitVersionOK reports whether a git that supports GIT_CONFIG_* credential injection (>= 2.31) is
// available on PATH. When it returns false, neither the reachability probe nor the mirror sync can
// inject its credential and the git-sync image is misconfigured — a packaging concern, surfaced as
// a startup warning.
func GitVersionOK() (ok bool, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		return false, "git was not found on PATH or did not respond"
	}
	fields := strings.Fields(string(out)) // e.g. "git version 2.39.3"
	if len(fields) < 3 {
		return false, "unrecognised `git --version` output"
	}
	ver := fields[2]
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) < 2 {
		return false, "unrecognised git version " + ver
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false, "unrecognised git version " + ver
	}
	if major > 2 || (major == 2 && minor >= 31) {
		return true, ver
	}
	return false, "git " + ver + " is older than the required 2.31"
}

// Probe runs `git ls-remote` against the entry's base URL. The credential is injected through the
// environment (never argv or the URL, so it cannot appear in `ps`/other users' process listings),
// and git is run with a locked-down environment so no ambient config, credential helper, or askpass
// can change the target or weaken TLS.
func (p *gitProber) Probe(ctx context.Context, e LocalEntry) (OriginResult, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	// An empty HOME keeps git from reading the invoking user's ~/.gitconfig or credential store.
	home, cleanup, err := newTempHome()
	if err != nil {
		return "", fmt.Errorf("probe setup failed: %w", err)
	}
	defer cleanup()

	argv := []string{"git", "ls-remote", "--", e.BaseURL, "HEAD"}
	// Run in the empty HOME so git cannot discover an ambient repository's .git/config (url.insteadOf,
	// http.* overrides) from the daemon's working directory.
	stderr, err := p.run(ctx, argv, probeEnv(e, home), home)

	var execErr *exec.Error
	var pathErr *fs.PathError
	if errors.As(err, &execErr) || errors.As(err, &pathErr) {
		// git itself could not be started (missing or non-executable binary): a local packaging
		// fault, not an origin result.
		return "", fmt.Errorf("git ls-remote could not run: %w", err)
	}
	return classifyLsRemote(err, stderr), nil
}

// probeEnv builds the locked-down environment for probing e: the shared base environment plus the
// credential as a header scoped to e's base URL (see credConfig), so the origin credential is never
// attached to a request for any other URL.
func probeEnv(e LocalEntry, home string) []string {
	return gitConfigEnv(gitBaseEnv(home), credConfig(e.BaseURL, e.Auth, e.Username, e.Token))
}

var (
	httpStatusRE = regexp.MustCompile(`returned error: (\d{3})`)
	// quotedRE matches the single-quoted URL git echoes in its errors. Stripping it before keyword
	// matching stops a hostname that contains "ssl", "tls", or "timed out" from being mistaken for a
	// connection failure.
	quotedRE = regexp.MustCompile(`'[^']*'`)
)

// classifyLsRemote maps git's exit and stderr to an OriginResult. A received HTTP response (even a
// 404 against a base URL that is not itself a repository) means the origin is reachable; 401/403
// mean the credential was rejected or missing; a connection/DNS/TLS failure, or a proxy's 407,
// means unreachable. It never reports "reachable" for an unrecognised failure.
//
// It shares authMarkers and connMarkers with classifyGitFailure, so a given git error is read the
// same way for the connector-level check (here) and for each repository's sync; the portal must not say
// "access denied" and later "unreachable" for one problem. They differ deliberately only where the
// target does: this probes a base URL that need not be a repository, so a server that answers with
// a 404 or a 5xx is "reachable", whereas a missing repository is a failed sync.
func classifyLsRemote(err error, stderr []byte) OriginResult {
	if err == nil {
		return OriginReachable
	}
	s := matchText(stderr)

	// Access-denied markers first: these also contain "unable to access", so they must win over the
	// generic reachable classification below.
	if containsAny(s, authMarkers) {
		return OriginAccessDenied
	}
	if m := httpStatusRE.FindStringSubmatch(s); m != nil {
		switch m[1] {
		case "401", "403":
			return OriginAccessDenied
		case "407":
			// A proxy demanding authentication is not the customer's origin credential being
			// rejected; it is the connector failing to reach the origin.
			return OriginUnreachable
		default:
			// 404, 3xx, 5xx, anything else: the server answered, so the origin is reachable.
			return OriginReachable
		}
	}
	// The server answered but the base URL is not itself a repository: git reports
	// "repository '<url>' not found" (its rendering of a 404), or an invalid smart-HTTP response.
	// Checked before the connection markers so it wins regardless of the message's other words.
	for _, m := range []string{"not found", "invalid server response", "not a valid repository"} {
		if strings.Contains(s, m) {
			return OriginReachable
		}
	}
	if containsAny(s, connMarkers) {
		return OriginUnreachable
	}
	// Unknown failure: never claim reachable.
	return OriginUnreachable
}

// runGit executes git with a discarded stdout and a size-capped stderr, in its own process group so
// a lingering git-remote-https helper is killed with the parent rather than outliving the timeout.
// dir is the working directory (an empty temp dir), so git cannot pick up an ambient repository's
// configuration from the daemon's cwd.
func runGit(ctx context.Context, argv, env []string, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	stderr := &limitedBuffer{max: maxGitStderr}
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	err := cmd.Run()
	return stderr.Bytes(), err
}

// truncationMarker stands in for the middle of a limitedBuffer's output when it was cut.
const truncationMarker = "\n[... output truncated ...]\n"

// limitedBuffer retains at most max bytes of what is written to it — the first half and the last
// half — but reports every write as fully consumed, so git never sees a short-write error on its
// stderr. Both ends are kept because git prints its explanation of a failed push LAST, after a
// status line for every ref: a buffer that kept only the beginning would lose the one line that
// says why it failed.
type limitedBuffer struct {
	max     int
	head    []byte
	tail    []byte
	dropped bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	headCap := l.max / 2
	if room := headCap - len(l.head); room > 0 {
		take := min(room, len(p))
		l.head = append(l.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > 0 {
		tailCap := l.max - headCap
		l.tail = append(l.tail, p...)
		if len(l.tail) > tailCap {
			l.dropped = true
			l.tail = append(l.tail[:0], l.tail[len(l.tail)-tailCap:]...)
		}
	}
	return n, nil
}

func (l *limitedBuffer) Bytes() []byte {
	out := append([]byte(nil), l.head...)
	if l.dropped {
		out = append(out, truncationMarker...)
	}
	return append(out, l.tail...)
}
