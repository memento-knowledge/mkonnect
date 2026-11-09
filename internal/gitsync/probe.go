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
	"sync"
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

// RepositoryProber is an OriginProber that can also check one repository the configuration lists,
// which exercises the credential and the repository path rather than only the server's base URL. The
// connector uses it, when the prober offers it, once a configuration has been accepted; a prober
// without it keeps checking the base URL.
type RepositoryProber interface {
	ProbeRepository(ctx context.Context, t RepoTarget) (RepoCheck, error)
}

// RepoTarget is a repository a RepositoryProber may dial, paired with the provider credential to dial
// it with. Only this package can make one, and only from a URL the host-match guard produced for a
// repository bound to that provider, so a credential can never be paired with a URL on another host.
type RepoTarget struct {
	entry LocalEntry
	url   string
}

// URL is the canonical repository URL the probe dials.
func (t RepoTarget) URL() string { return t.url }

// RepoCheck is what a repository probe found, in the same terms the sync reports a failure in (see
// classifyGitFailure, origin side).
type RepoCheck struct {
	// Result is the wire result: access_denied for a 401 or another sign that the credential was
	// refused (or is missing), unreachable for a failure to reach the server or one git's output
	// cannot name (never reachable for those), reachable otherwise.
	Result OriginResult
	// ErrorType is the repository error_type naming a failure that is not a clean pass: it is set
	// for every Result other than a clean reachable. A 403 or a 404 is reachable with
	// origin_access_denied: a forge answers either for a repository the credential cannot see.
	ErrorType string
	// CredentialRejected says the origin refused the provider credential itself (a 401, never a
	// 403 or 404) and a credential was sent. It implies Result access_denied, but not the converse: a
	// server that wants a credential nobody configured answers access_denied without it. It is the
	// evidence that closes the provider's gate (see providerGates).
	CredentialRejected bool
}

// gitProber probes with `git ls-remote`. run is a seam so tests can assert the argv/env without a
// real subprocess; it defaults to runGit. timeout bounds one probe and defaults to probeTimeout.
type gitProber struct {
	run     func(ctx context.Context, argv, env []string, dir string) (stderr []byte, err error)
	timeout time.Duration
}

var _ RepositoryProber = (*gitProber)(nil)

// NewGitProber returns a prober backed by the git binary.
func NewGitProber() *gitProber { return &gitProber{run: runGit} }

func (p *gitProber) limit() time.Duration {
	if p.timeout > 0 {
		return p.timeout
	}
	return probeTimeout
}

// MinGitMajor and MinGitMinor are the oldest git the connector supports: GIT_CONFIG_COUNT
// credential injection needs git >= 2.31.
const (
	MinGitMajor = 2
	MinGitMinor = 31
)

// gitVersionRE reads the version out of `git --version`. It takes the leading numeric components and
// ignores whatever follows, so a distribution or platform suffix ("2.39.5 (Apple Git-154)",
// "2.43.0.windows.1", "2.34.1-1ubuntu1", a release candidate "2.31.0-rc1" or "2.40.rc0") does not
// stop it from reading the version.
var gitVersionRE = regexp.MustCompile(`^git version (([0-9]{1,6})\.([0-9]{1,6})(?:\.[0-9]{1,6})?)`)

// ParseGitVersion reads the output of `git --version`. It returns the leading dotted numbers of the
// version (up to three, for display; a suffix is dropped) and its major and minor numbers; ok is false
// for anything it does not recognise.
func ParseGitVersion(out string) (version string, major, minor int, ok bool) {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	m := gitVersionRE.FindStringSubmatch(line)
	if m == nil {
		return "", 0, 0, false
	}
	major, err1 := strconv.Atoi(m[2])
	minor, err2 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil {
		return "", 0, 0, false
	}
	return m[1], major, minor, true
}

// GitVersionSupported reports whether major.minor is at least MinGitMajor.MinGitMinor.
func GitVersionSupported(major, minor int) bool {
	return major > MinGitMajor || (major == MinGitMajor && minor >= MinGitMinor)
}

// GitVersionOK reports whether a git that supports GIT_CONFIG_* credential injection (>= 2.31) is
// available on PATH. When it returns false, neither the reachability probe nor the mirror sync can
// inject its credential and the git-sync image is misconfigured — a packaging concern, surfaced as
// a startup warning. On success detail is the version; otherwise it says what is wrong.
func GitVersionOK() (ok bool, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		return false, "git was not found on PATH or did not respond"
	}
	ver, major, minor, parsed := ParseGitVersion(string(out))
	if !parsed {
		return false, "unrecognised `git --version` output"
	}
	if !GitVersionSupported(major, minor) {
		return false, "git " + ver + " is older than the required 2.31"
	}
	return true, ver
}

// Probe runs `git ls-remote` against the entry's base URL. The credential is injected through the
// environment (never argv or the URL, so it cannot appear in `ps`/other users' process listings),
// and git is run with a locked-down environment so no ambient config, credential helper, or askpass
// can change the target or weaken TLS.
func (p *gitProber) Probe(ctx context.Context, e LocalEntry) (OriginResult, error) {
	ctx, cancel := context.WithTimeout(ctx, p.limit())
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

// ProbeRepository runs `git ls-remote --heads -- <url>` against one repository, in the environment a
// sync uses for the origin (originGitEnv: the credential in a header scoped to exactly that URL,
// redirects not followed, an empty HOME), and reads the outcome with the sync's own classifier so a
// check and a sync cannot disagree about one failure.
//
// --heads is what keeps the advertisement small: git speaks protocol v2 (the default of every
// supported git, and nothing in this environment can change it), under which it then asks the server
// for refs/heads/ only, so the refs a forge keeps for merge requests are not sent. A pattern such as
// HEAD would not do it: git 2.50 sends no ref-prefix for a pattern and the server lists every ref. The
// 15 second limit still bounds a repository with very many branches; its standard output is discarded
// and its standard error capped.
//
// A non-nil error is a local fault or a cancellation, never an origin result: git cannot start, the
// temp home cannot be made, the disk or memory ran out, or ctx ended. The detail of git's own message
// is never carried in it.
func (p *gitProber) ProbeRepository(ctx context.Context, t RepoTarget) (RepoCheck, error) {
	cctx, cancel := context.WithTimeout(ctx, p.limit())
	defer cancel()
	home, cleanup, err := newTempHome()
	if err != nil {
		return RepoCheck{}, fmt.Errorf("probe setup failed: %w", err)
	}
	defer cleanup()

	argv := []string{"git", "ls-remote", "--heads", "--", t.url}
	stderr, err := p.run(cctx, argv, originGitEnv(home, t.url, t.entry), home)
	if err != nil {
		if ctx.Err() != nil {
			// Stopped from outside: whatever git printed while it was killed says nothing about the origin.
			return RepoCheck{}, ctx.Err()
		}
		var execErr *exec.Error
		var pathErr *fs.PathError
		if errors.As(err, &execErr) || errors.As(err, &pathErr) {
			return RepoCheck{}, fmt.Errorf("git ls-remote could not run: %w", err)
		}
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			// A git killed at the deadline reports a bare "signal: killed"; the sync wraps it the same
			// way (see runBounded) so it reads as a timeout and not as an out-of-memory kill.
			err = fmt.Errorf("git ls-remote: %w", context.DeadlineExceeded)
		}
	}
	return repoCheckOf(t.entry, err, stderr)
}

// repoCheckOf reads a finished repository probe with the sync's classifier (classifyGitFailure, origin
// side). The error is non-nil for a local fault, which says nothing about the origin.
func repoCheckOf(e LocalEntry, err error, stderr []byte) (RepoCheck, error) {
	typ, _ := classifyGitFailure(originSide, err, stderr, false)
	switch typ {
	case "":
		return RepoCheck{Result: OriginReachable}, nil
	case ErrOriginAccessDenied:
		if originCredentialRejected(stderr) {
			return RepoCheck{Result: OriginAccessDenied, ErrorType: typ, CredentialRejected: e.Auth != ""}, nil
		}
		return RepoCheck{Result: OriginReachable, ErrorType: typ}, nil
	case ErrOriginURLRedirected:
		return RepoCheck{Result: OriginReachable, ErrorType: typ}, nil
	case ErrLocalDiskExhausted, ErrJobOutOfMemory:
		return RepoCheck{}, errors.New("git reported a local fault: " + typ)
	}
	// origin_unreachable, and anything the classifier cannot name: never reachable.
	return RepoCheck{Result: OriginUnreachable, ErrorType: ErrOriginUnreachable}, nil
}

// probeEnv builds the locked-down environment for probing e: the shared base environment plus the
// credential as a header scoped to e's base URL (see credConfig), so the origin credential is never
// attached to a request for any other URL.
func probeEnv(e LocalEntry, home string) []string {
	return originGitEnv(home, e.BaseURL, e)
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

// runGit executes git with a discarded stdout and a size-capped stderr; see runGitOut.
func runGit(ctx context.Context, argv, env []string, dir string) ([]byte, error) {
	return runGitOut(ctx, argv, env, dir, io.Discard)
}

// runGitOut executes git with its standard output streamed to stdout (which must consume everything
// it is given without blocking, as the writers in syncstats.go do) and a size-capped stderr, in its
// own process group so a lingering git-remote-https helper is killed with the parent rather than
// outliving the timeout. dir is the working directory (an empty temp dir), so git cannot pick up an
// ambient repository's configuration from the daemon's cwd.
func runGitOut(ctx context.Context, argv, env []string, dir string, stdout io.Writer) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Stdout = stdout
	stderr := &limitedBuffer{max: maxGitStderr}
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cancellation asks the whole group to stop (SIGTERM) and kills whatever is left after a short
	// grace. A bare SIGKILL skips git's cleanup of its lock files, and a lock left behind in the
	// mirror fails the next fetch ("cannot lock ref"), which is the very sync a credential rotation
	// starts straight after cancelling this one.
	var (
		mu        sync.Mutex
		cancelled bool
		pgid      int
		grace     *time.Timer
	)
	cmd.Cancel = func() error {
		mu.Lock()
		defer mu.Unlock()
		if cmd.Process == nil {
			return nil
		}
		cancelled, pgid = true, cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		grace = time.AfterFunc(gitKillGrace, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
		return nil
	}
	err := cmd.Run()
	mu.Lock()
	if cancelled {
		// The parent is gone; sweep any helper that ignored SIGTERM so none outlives the sync.
		grace.Stop()
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	mu.Unlock()
	return stderr.Bytes(), err
}

// gitKillGrace is how long a cancelled git has, after SIGTERM, to clean up before it is killed.
const gitKillGrace = time.Second

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
