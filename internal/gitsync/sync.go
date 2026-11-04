package gitsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

const (
	// defaultFetchTimeout and defaultPushTimeout bound a single fetch and a single push, so a
	// wedged connection cannot hold a sync slot forever. They are generous because the first fetch
	// of a large repository is legitimately slow. They are not yet configurable per deployment.
	defaultFetchTimeout = 30 * time.Minute
	defaultPushTimeout  = 30 * time.Minute

	// replicaAuth is how the platform push token authenticates to the replica. INFERRED: the
	// contract names the token but not the scheme; Bearer is the connector's assumption, to be
	// confirmed (see the design's open items).
	replicaAuth = "bearer"
)

// mirrorRefspecs are the only refs the local mirror holds, and the only ones pushed to the replica:
// the contract is "all branches and tags". Other namespaces an origin may carry (a forge's refs/pull/*
// and the like) are hidden refs that the replica refuses, so mirroring them would only cost disk and
// bandwidth. The same refspecs fetch from the origin (with --prune, which only ever removes refs
// inside these namespaces) and push to the replica.
var mirrorRefspecs = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}

// mirrorInitialBranch names the unborn branch HEAD points at in a new mirror. It is explicit so the
// result does not depend on the git version or on a configured init.defaultBranch; nothing reads it,
// because only refs/heads/* and refs/tags/* are ever fetched or pushed.
const mirrorInitialBranch = "main"

// corruptMirrorMarkers identify a mirror directory that is not a usable repository (for example
// left behind by a crash), which is repaired by rebuilding it rather than reported as an origin fault.
var corruptMirrorMarkers = []string{"not a git repository", "corrupt", "bad object"}

// runFunc runs git with the given argv and environment in dir and returns its (capped) stderr.
type runFunc func(ctx context.Context, argv, env []string, dir string) ([]byte, error)

// syncResult is the outcome of one repository sync, recorded and reported in git_sync_status.
type syncResult struct {
	State       string
	ErrorType   string
	ErrorDetail string
	Warnings    []string
}

// syncTask is everything one repository sync needs. CanonicalURL is the validated, re-serialized
// origin URL the host-match guard produced; the sync dials exactly that and never a raw origin_url.
type syncTask struct {
	RepoID         string
	CanonicalURL   string
	Origin         LocalEntry // the matched provider's credential
	ReplicaURL     string
	PushToken      Secret
	TokenExpiresAt string // RFC3339; "" when unknown
}

// syncDeps are the seams a sync needs. Zero timeouts use the defaults.
type syncDeps struct {
	mirrors      Mirrors
	run          runFunc
	now          func() time.Time
	fetchTimeout time.Duration
	pushTimeout  time.Duration
}

// syncRepo mirrors one repository: fetch the origin's branches and tags into the local bare mirror
// (creating it on the first run), then push them to the replica. The origin credential is attached only to
// the origin invocation and the push token only to the push, each scoped to its own URL. Failures
// are classified into typed error codes; a cancelled context (shutdown) is not a failure and leaves
// the repository pending.
func syncRepo(ctx context.Context, d syncDeps, t syncTask) syncResult {
	if ctx.Err() != nil {
		return syncResult{State: StatePending}
	}
	// A token already past its expiry cannot push; say so before spending a fetch on it.
	if d.tokenExpired(t) {
		return syncResult{State: StateError, ErrorType: ErrPushTokenExpired}
	}

	if err := d.mirrors.EnsureRoot(); err != nil {
		return localFault(err)
	}
	home, cleanup, err := newTempHome()
	if err != nil {
		return localFault(err)
	}
	defer cleanup()

	if stderr, err := d.updateMirror(ctx, home, t); err != nil {
		if ctx.Err() != nil {
			return syncResult{State: StatePending}
		}
		typ, detail := classifyGitFailure(originSide, err, stderr, false)
		return syncResult{State: StateError, ErrorType: typ, ErrorDetail: detail}
	}

	// Never prune the replica from a mirror with nothing to send: an origin that advertises no
	// branches or tags (an empty repository, or a transient bad advertisement) leaves the mirror
	// with no refs, and `push --prune` would then delete every branch and tag on the replica. This
	// also covers a mirror that was initialised but never fetched into (for example after a crash).
	if stderr, err := d.hasPushableRefs(ctx, home, t); err != nil {
		if ctx.Err() != nil {
			return syncResult{State: StatePending}
		}
		if errors.Is(err, errNoRefs) {
			return syncResult{State: StateError, ErrorType: ErrUnclassified, ErrorDetail: errOriginEmpty}
		}
		typ, detail := classifyGitFailure(originSide, err, stderr, false)
		return syncResult{State: StateError, ErrorType: typ, ErrorDetail: detail}
	}

	// The fetch can outlive the token. Re-check immediately before using it: a token the
	// platform has declared expired is not used, whether or not the replica would still accept it.
	// (A token that expires while the push itself is running is the replica's to judge; it is not
	// cut short by the connector's clock, which may be skewed.)
	if d.tokenExpired(t) {
		return syncResult{State: StateError, ErrorType: ErrPushTokenExpired}
	}

	pushArgv := append([]string{"git", "--git-dir=" + d.mirrors.Dir(t.RepoID), "push", "--prune", "--force", "--", t.ReplicaURL}, mirrorRefspecs...)
	pushEnv := gitConfigEnv(gitBaseEnv(home), credConfig(t.ReplicaURL, replicaAuth, "", t.PushToken))
	stderr, err := d.runBounded(ctx, d.pushTimeoutOrDefault(), "push", pushArgv, pushEnv, home)
	if err != nil {
		if ctx.Err() != nil {
			return syncResult{State: StatePending}
		}
		// Re-evaluated now: a long fetch can outlive the token, and a 401 then means "expired".
		typ, detail := classifyGitFailure(replicaSide, err, stderr, d.tokenExpired(t))
		return syncResult{State: StateError, ErrorType: typ, ErrorDetail: detail}
	}
	return syncResult{State: StateOK}
}

// errOriginEmpty is the detail reported when the origin has no branches or tags to replicate. The
// replica is left exactly as it was.
const errOriginEmpty = "origin has no branches or tags; replica left unchanged"

var errNoRefs = errors.New("mirror has no branches or tags")

// hasPushableRefs reports, as a nil error, that the mirror holds at least one branch or tag.
// `git show-ref --quiet` exits 1 when nothing matches, which is returned as errNoRefs; any other
// failure is returned as-is and says nothing about whether the origin is empty.
func (d syncDeps) hasPushableRefs(ctx context.Context, home string, t syncTask) ([]byte, error) {
	argv := []string{"git", "--git-dir=" + d.mirrors.Dir(t.RepoID), "show-ref", "--quiet", "--heads", "--tags"}
	stderr, err := d.runBounded(ctx, d.fetchTimeoutOrDefault(), "ref check", argv, gitBaseEnv(home), home)
	if err == nil {
		return nil, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil, errNoRefs
	}
	return stderr, err
}

// updateMirror brings the local mirror up to date with the origin's branches and tags. A mirror that
// exists is fetched into; otherwise it is created with `git init --bare` and fetched into. A mirror
// directory that is not a usable repository is removed and rebuilt once.
//
// Mirrors created before the mirror was narrowed were made by `git clone --mirror` and hold every
// namespace the origin has. The fetch below works on them unchanged and neither rewrites nor deletes
// those extra refs (--prune only touches refs inside the fetched namespaces), so they keep occupying
// disk until the mirror is rebuilt; nothing here reclaims that space.
func (d syncDeps) updateMirror(ctx context.Context, home string, t syncTask) ([]byte, error) {
	dir := d.mirrors.Dir(t.RepoID)
	env := gitConfigEnv(gitBaseEnv(home), credConfig(t.CanonicalURL, t.Origin.Auth, t.Origin.Username, t.Origin.Token))
	fetchArgv := append([]string{"git", "--git-dir=" + dir, "fetch", "--prune", "--force", "--", t.CanonicalURL}, mirrorRefspecs...)

	if d.mirrors.Exists(t.RepoID) {
		stderr, err := d.runBounded(ctx, d.fetchTimeoutOrDefault(), "fetch", fetchArgv, env, home)
		if err == nil {
			return nil, nil
		}
		if !isCorruptMirror(stderr) {
			return stderr, err
		}
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return []byte(rmErr.Error()), rmErr
		}
	}

	// git init talks to no remote, so it gets no credential.
	initArgv := []string{"git", "init", "--bare", "--initial-branch=" + mirrorInitialBranch, "--", dir}
	stderr, err := d.runBounded(ctx, d.fetchTimeoutOrDefault(), "init", initArgv, gitBaseEnv(home), home)
	if err == nil {
		stderr, err = d.runBounded(ctx, d.fetchTimeoutOrDefault(), "fetch", fetchArgv, env, home)
	}
	if err != nil {
		// A failed init or first fetch leaves a partial or empty repository that the next attempt would
		// mistake for a mirror; remove it.
		_ = os.RemoveAll(dir)
	}
	return stderr, err
}

// runBounded runs git under timeout. A process killed because its deadline passed returns a bare
// "signal: killed"; it is re-wrapped around context.DeadlineExceeded so the classifier attributes it
// to the timeout rather than to an out-of-memory kill.
func (d syncDeps) runBounded(ctx context.Context, timeout time.Duration, what string, argv, env []string, dir string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stderr, err := d.run(cctx, argv, env, dir)
	if err != nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%s timed out: %w", what, context.DeadlineExceeded)
	}
	return stderr, err
}

// tokenExpired reports whether t's push token is known to be past its expiry. An unknown or
// unparseable expiry is not treated as expired: the replica remains the judge.
func (d syncDeps) tokenExpired(t syncTask) bool {
	exp, ok := parseExpiry(t.TokenExpiresAt)
	return ok && !exp.After(d.now())
}

func (d syncDeps) fetchTimeoutOrDefault() time.Duration {
	if d.fetchTimeout > 0 {
		return d.fetchTimeout
	}
	return defaultFetchTimeout
}

func (d syncDeps) pushTimeoutOrDefault() time.Duration {
	if d.pushTimeout > 0 {
		return d.pushTimeout
	}
	return defaultPushTimeout
}

// localFault reports a failure to prepare the sync (the mirror directory or temp home could not be
// created) through the same classifier, so a full disk reads as local_disk_exhausted.
func localFault(err error) syncResult {
	typ, detail := classifyGitFailure(originSide, err, []byte(err.Error()), false)
	return syncResult{State: StateError, ErrorType: typ, ErrorDetail: detail}
}
