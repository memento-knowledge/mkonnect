package gitsync

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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

// maxProbeStderr caps how much of git's stderr is retained for classification.
const maxProbeStderr = 8 << 10

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
// available on PATH. When it returns false, the reachability probe cannot inject the credential and
// the git-sync image is misconfigured — a packaging concern, surfaced as a startup warning.
func GitVersionOK() (ok bool, detail string) {
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return false, "git was not found on PATH"
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
	home, err := os.MkdirTemp("", "gitsync-probe-home-")
	if err != nil {
		return "", fmt.Errorf("probe setup failed: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()

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

// probeEnv builds a minimal, explicit environment for git. It never copies os.Environ() wholesale:
// an inherited GIT_SSL_NO_VERIFY, url.*.insteadOf, http.proxy, GIT_TRACE, or a credential helper
// could weaken TLS, redirect the target, or dump the Authorization header. Only PATH, an empty
// HOME, locale, and explicitly chosen proxy/CA variables are passed through. The credential is
// injected as an http.extraHeader via GIT_CONFIG_* (git >= 2.31) so it stays out of argv and the
// URL. Note: GIT_CONFIG_VALUE_* is readable via /proc/<pid>/environ (mode 0400) by the same user —
// the same exposure as the connector's own environment, not secret from the owner, but not visible
// to other users or in a process list the way an argv would be.
func probeEnv(e LocalEntry, home string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"LC_ALL=C",
		"LANGUAGE=",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
	}
	// Pass through only proxy and CA configuration, deliberately.
	for _, k := range []string{
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "GIT_SSL_CAINFO",
	} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}

	cfg := [][2]string{
		// Never let the Authorization header follow a redirect to another host or to http.
		{"http.followRedirects", "false"},
		{"credential.helper", ""},
		{"core.askPass", ""},
	}
	switch e.Auth {
	case "bearer":
		cfg = append(cfg, [2]string{"http.extraHeader", "Authorization: Bearer " + e.Token.Reveal()})
	case "basic":
		creds := base64.StdEncoding.EncodeToString([]byte(e.Username + ":" + e.Token.Reveal()))
		cfg = append(cfg, [2]string{"http.extraHeader", "Authorization: Basic " + creds})
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]))
		env = append(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

var (
	httpStatusRE = regexp.MustCompile(`returned error: (\d{3})`)
	// quotedRE matches the single-quoted URL git echoes in its errors. Stripping it before keyword
	// matching stops a hostname that contains "ssl", "tls", or "timed out" from being mistaken for a
	// connection failure.
	quotedRE = regexp.MustCompile(`'[^']*'`)
)

// classifyLsRemote maps git's exit and stderr to an OriginResult. A received HTTP response (even a
// 404 against a base URL that is not itself a repository) means the origin is reachable; 401/403/407
// mean the credential was rejected or missing; a connection/DNS/TLS failure means unreachable. It
// never reports "reachable" for an unrecognised failure.
func classifyLsRemote(err error, stderr []byte) OriginResult {
	if err == nil {
		return OriginReachable
	}
	// Remove the echoed URL so its host cannot trip a keyword below.
	s := strings.ToLower(string(quotedRE.ReplaceAll(stderr, []byte("''"))))

	// Access-denied markers first: these also contain "unable to access", so they must win over the
	// generic reachable classification below.
	for _, m := range []string{
		"authentication failed", "could not read username", "terminal prompts disabled",
		"401 unauthorized", "403 forbidden",
	} {
		if strings.Contains(s, m) {
			return OriginAccessDenied
		}
	}
	if m := httpStatusRE.FindStringSubmatch(s); m != nil {
		switch m[1] {
		case "401", "403", "407":
			return OriginAccessDenied
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
	for _, m := range []string{
		"could not resolve host", "couldn't resolve host", "connection refused",
		"connection timed out", "timed out", "could not connect", "failed to connect",
		"ssl", "tls", "certificate",
	} {
		if strings.Contains(s, m) {
			return OriginUnreachable
		}
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
	stderr := &limitedBuffer{max: maxProbeStderr}
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

// limitedBuffer retains at most max bytes but reports every write as fully consumed, so git never
// sees a short-write error on its stderr.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if rem := l.max - l.buf.Len(); rem > 0 {
		if len(p) > rem {
			l.buf.Write(p[:rem])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}

func (l *limitedBuffer) Bytes() []byte { return l.buf.Bytes() }
