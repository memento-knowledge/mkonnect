package gitsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// defaultFetchTimeout and defaultPushTimeout bound a single clone/fetch and a single push, so a
	// wedged connection cannot hold a sync slot forever. They are generous because the first clone
	// of a large repository is legitimately slow. (Per-deployment tuning is a Phase 3b item.)
	defaultFetchTimeout = 30 * time.Minute
	defaultPushTimeout  = 30 * time.Minute

	// replicaAuth is how the platform push token authenticates to the replica. INFERRED: the
	// contract names the token but not the scheme; Bearer is the connector's assumption, to be
	// confirmed (see the design's open items).
	replicaAuth = "bearer"
)

// fetchRefspec updates every ref from the origin (what `clone --mirror` fetched), pruning refs the
// origin has deleted. pushRefspecs sends only branches and tags to the replica: other namespaces an
// origin may carry (for example refs/pull/*) are hidden refs that forges refuse, and the contract
// is "all branches and tags".
const fetchRefspec = "+refs/*:refs/*"

var pushRefspecs = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}

// corruptMirrorMarkers identify a mirror directory that is not a usable repository (for example
// left behind by a crash), which is repaired by recloning rather than reported as an origin fault.
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

// syncRepo mirrors one repository: clone-or-fetch every ref from the origin into the local bare
// mirror, then push its branches and tags to the replica. The origin credential is attached only to
// the origin invocation and the push token only to the push, each scoped to its own URL. Failures
// are classified into typed error codes; a cancelled context (shutdown) is not a failure and leaves
// the repository pending.
func syncRepo(ctx context.Context, d syncDeps, t syncTask) syncResult {
	if ctx.Err() != nil {
		return syncResult{State: StatePending}
	}
	// A token already past its expiry cannot push; say so before spending a clone on it.
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

	if stderr, err := d.fetchOrClone(ctx, home, t); err != nil {
		if ctx.Err() != nil {
			return syncResult{State: StatePending}
		}
		typ, detail := classifyGitFailure(originSide, err, stderr, false)
		return syncResult{State: StateError, ErrorType: typ, ErrorDetail: detail}
	}

	pushArgv := append([]string{"git", "--git-dir=" + d.mirrors.Dir(t.RepoID), "push", "--prune", "--force", "--", t.ReplicaURL}, pushRefspecs...)
	pushEnv := gitConfigEnv(gitBaseEnv(home), credConfig(t.ReplicaURL, replicaAuth, "", t.PushToken))
	stderr, err := d.runBounded(ctx, d.pushTimeoutOrDefault(), "push", pushArgv, pushEnv, home)
	if err != nil {
		if ctx.Err() != nil {
			return syncResult{State: StatePending}
		}
		// Re-evaluated now: a long clone can outlive the token, and a 401 then means "expired".
		typ, detail := classifyGitFailure(replicaSide, err, stderr, d.tokenExpired(t))
		return syncResult{State: StateError, ErrorType: typ, ErrorDetail: detail}
	}
	return syncResult{State: StateOK}
}

// fetchOrClone brings the local mirror up to date with the origin: a fetch when a mirror exists, a
// clone otherwise. A mirror directory that is not a usable repository is removed and recloned once.
func (d syncDeps) fetchOrClone(ctx context.Context, home string, t syncTask) ([]byte, error) {
	dir := d.mirrors.Dir(t.RepoID)
	env := gitConfigEnv(gitBaseEnv(home), credConfig(t.CanonicalURL, t.Origin.Auth, t.Origin.Username, t.Origin.Token))

	if d.mirrors.Exists(t.RepoID) {
		fetchArgv := []string{"git", "--git-dir=" + dir, "fetch", "--prune", "--force", "--", t.CanonicalURL, fetchRefspec}
		stderr, err := d.runBounded(ctx, d.fetchTimeoutOrDefault(), "fetch", fetchArgv, env, home)
		if err == nil {
			return nil, nil
		}
		if !containsAny(strings.ToLower(string(stderr)), corruptMirrorMarkers) {
			return stderr, err
		}
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return []byte(rmErr.Error()), rmErr
		}
	}

	cloneArgv := []string{"git", "clone", "--mirror", "--", t.CanonicalURL, dir}
	stderr, err := d.runBounded(ctx, d.fetchTimeoutOrDefault(), "clone", cloneArgv, env, home)
	if err != nil {
		// A failed clone can leave a partial directory that the next attempt would mistake for a
		// mirror; remove it.
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
