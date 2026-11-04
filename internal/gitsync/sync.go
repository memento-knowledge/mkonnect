package gitsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

const (
	// defaultFetchTimeout and defaultPushTimeout bound a single fetch and a single push, so a
	// wedged connection cannot hold a sync slot forever. They are generous because the first fetch
	// of a large repository is legitimately slow. The push timeout is replaced by the platform's
	// push_timeout_seconds limit when it supplies one; the fetch timeout is not configurable.
	defaultFetchTimeout = 30 * time.Minute
	defaultPushTimeout  = 30 * time.Minute

	// replicaAuth is how the platform push token authenticates to the replica. INFERRED: the
	// contract names the token but not the scheme; Bearer is the connector's assumption, to be
	// confirmed (see the design's open items).
	replicaAuth = "bearer"

	// newestCommitTimeout bounds reading the newest commit time from the local mirror, which visits
	// every branch and tag tip. defaultBranchTimeout bounds the one extra origin request a sync makes
	// to learn the default branch. Both are informational, so a slow one gives up rather than hold a
	// sync slot.
	newestCommitTimeout  = 5 * time.Minute
	defaultBranchTimeout = 30 * time.Second

	// maxCommitClockSkew is how far past now a committer date may be and still be believed.
	maxCommitClockSkew = 24 * time.Hour
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
	// WarningsUndecided says this attempt could not determine its warnings (see detectLFS), so
	// finishSync keeps the previous attempt's rather than reporting none.
	WarningsUndecided bool
	Stats             syncStats
}

// syncStats is what one attempt measured. StartedAt and Duration are stamped by whoever runs the
// attempt (the manager, which knows when it began); the rest is learned by syncRepo, and only when
// the standard-output seam (syncDeps.runOut) is present. Zero values mean "not learned this time".
type syncStats struct {
	StartedAt time.Time
	Duration  time.Duration
	// RefsPushed is how many refs the push created, updated or deleted on the replica: 0 when the
	// attempt ended before pushing or found nothing new, nil when it was not measured.
	RefsPushed *int
	// NewestCommitAt is the newest commit time across the origin's branches and tags, from the mirror
	// just fetched; zero when the attempt did not get that far.
	NewestCommitAt time.Time
	// DefaultBranch is the origin's default branch; empty when it could not be resolved.
	DefaultBranch string
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
	// Bind is the provider, and generation of its credential, Origin was taken from; the result of
	// the attempt is kept only while the repository is still bound to it.
	Bind binding
}

// syncDeps are the seams a sync needs. Zero timeouts use the defaults.
type syncDeps struct {
	// limits returns the limits in force. It is read once at the start of each attempt, so a
	// configuration that arrives mid-sync applies from the next attempt and never mixes two settings
	// within one. Nil means DefaultLimits.
	limits  func() Limits
	mirrors Mirrors
	run     runFunc
	// runOut runs git with its standard output streamed, which statistics need (the push's porcelain
	// report, the newest commit, the origin's default branch) and so does push batching (it reads the
	// planning push's report). When nil no statistics are collected, pushes are never batched, and git
	// runs through run alone, exactly as before.
	runOut       outRunFunc
	now          func() time.Time
	fetchTimeout time.Duration
}

// syncRepo mirrors one repository: fetch the origin's branches and tags into the local bare mirror
// (creating it on the first run), then push them to the replica. The origin credential is attached only to
// the origin invocation and the push token only to the push, each scoped to its own URL. Failures
// are classified into typed error codes; a cancelled context (shutdown) is not a failure and leaves
// the repository pending.
func syncRepo(ctx context.Context, d syncDeps, t syncTask) syncResult {
	var stats syncStats
	if d.runOut != nil {
		// Measured, and so far nothing was pushed: an attempt that ends early reports 0, not unknown.
		stats.RefsPushed = new(int)
	}
	res := d.syncAttempt(ctx, t, &stats)
	res.Stats = stats
	return res
}

// syncAttempt is syncRepo's body; it records what it measures in stats as it goes.
func (d syncDeps) syncAttempt(ctx context.Context, t syncTask, stats *syncStats) syncResult {
	if ctx.Err() != nil {
		return syncResult{State: StatePending}
	}
	// A token already past its expiry cannot push; say so before spending a fetch on it.
	if d.tokenExpired(t) {
		return syncResult{State: StateError, ErrorType: ErrPushTokenExpired}
	}
	lim := d.currentLimits()

	if err := d.mirrors.EnsureRoot(); err != nil {
		return localFault(err)
	}
	home, cleanup, err := newTempHome()
	if err != nil {
		return localFault(err)
	}
	defer cleanup()

	if stderr, err := d.updateMirror(ctx, home, t, lim); err != nil {
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
	// It is the only guard of that kind, and it precedes every push, the planning push included.
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

	// The connector's own repository ceiling: an oversized mirror is not pushed. It is checked after
	// the fetch, so a repository far over the ceiling is still downloaded once. The mirror is kept, so
	// the next fetch stays incremental; the result repeats until git reclaims enough space or the
	// platform raises the ceiling.
	if lim.MaxRepositoryMB > 0 {
		over, err := mirrorExceeds(d.mirrors.Dir(t.RepoID), int64(lim.MaxRepositoryMB)*mebibyte)
		if err != nil {
			return localFault(err)
		}
		if over {
			return syncResult{State: StateError, ErrorType: ErrRepositoryOverLimit}
		}
	}

	if d.runOut != nil {
		// The mirror was just fetched, so this is the origin's newest commit as of this attempt. A
		// failure to read it costs the statistic, not the sync.
		stats.NewestCommitAt = d.newestCommit(ctx, home, t)
		if ctx.Err() != nil {
			return syncResult{State: StatePending}
		}
	}

	if res := d.pushStage(ctx, home, t, lim, stats); res.State != StateOK {
		return res
	}
	d.learnDefaultBranch(ctx, home, t, stats)
	// Only a healthy sync carries the warning; it is recomputed from this sync's mirror, so it clears
	// itself once the repository stops using LFS. A sync that could not tell says so, and keeps what
	// the previous one reported.
	warnings, decided := d.detectLFS(ctx, home, t, stats.DefaultBranch)
	return syncResult{State: StateOK, Warnings: warnings, WarningsUndecided: !decided}
}

// errOriginEmpty is the detail reported when the origin has no branches or tags to replicate. The
// replica is left exactly as it was.
const errOriginEmpty = "origin has no branches or tags; replica left unchanged"

var (
	errNoRefs       = errors.New("mirror has no branches or tags")
	errPushRejected = errors.New("push reported rejected refs")
)

// newestCommit reads the newest commit time across the mirror's branches and tags, or the zero time
// when it cannot (git failed, or no ref has a usable commit date).
func (d syncDeps) newestCommit(ctx context.Context, home string, t syncTask) time.Time {
	newest := newestTracker{ceiling: d.now().Add(maxCommitClockSkew).Unix()}
	argv := []string{"git", "--git-dir=" + d.mirrors.Dir(t.RepoID), "for-each-ref", "--format=" + newestFormat, "refs/heads", "refs/tags"}
	if _, err := d.runBoundedOut(ctx, newestCommitTimeout, "newest commit", argv, gitBaseEnv(home), home, &newest); err != nil {
		return time.Time{}
	}
	at, _ := newest.Newest()
	return at
}

// learnDefaultBranch asks the origin which branch its HEAD names and records it in stats. It is one
// extra origin request per sync, made after the push so it can never delay it, and only once a fetch
// has already reached the origin. The mirror's own HEAD says nothing about the origin's (it holds only
// branches and tags), hence the request. Failing to resolve it is not a sync failure: the field is
// simply left unknown, and nothing git said is logged.
func (d syncDeps) learnDefaultBranch(ctx context.Context, home string, t syncTask, stats *syncStats) {
	if d.runOut == nil || ctx.Err() != nil {
		return
	}
	out := &limitedBuffer{max: maxSymrefOutput}
	argv := []string{"git", "ls-remote", "--symref", "--", t.CanonicalURL, "HEAD"}
	if _, err := d.runBoundedOut(ctx, defaultBranchTimeout, "default branch", argv, d.originEnv(home, t), home, out); err != nil {
		slog.Debug("origin default branch not resolved", "repo_id", t.RepoID)
		return
	}
	if name, ok := parseSymrefHead(out.Bytes()); ok {
		stats.DefaultBranch = name
	}
}

// originEnv is the environment for a git invocation that talks to the origin: the locked-down base
// plus the origin credential scoped to the canonical URL.
func (d syncDeps) originEnv(home string, t syncTask) []string {
	return gitConfigEnv(gitBaseEnv(home), credConfig(t.CanonicalURL, t.Origin.Auth, t.Origin.Username, t.Origin.Token))
}

// fetchEnv is originEnv plus the limits that bear on a fetch: the pack settings and, when a size
// ceiling is set, foreground maintenance so the mirror is measured settled.
func (d syncDeps) fetchEnv(home string, t syncTask, lim Limits) []string {
	cfg := credConfig(t.CanonicalURL, t.Origin.Auth, t.Origin.Username, t.Origin.Token)
	cfg = append(cfg, packConfig(lim)...)
	cfg = append(cfg, maintenanceConfig()...)
	return gitConfigEnv(gitBaseEnv(home), cfg)
}

// replicaEnv is the environment for a push: the locked-down base, the push token scoped to the
// replica URL, and the pack settings.
func (d syncDeps) replicaEnv(home string, t syncTask, lim Limits) []string {
	cfg := credConfig(t.ReplicaURL, replicaAuth, "", t.PushToken)
	return gitConfigEnv(gitBaseEnv(home), append(cfg, packConfig(lim)...))
}

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
func (d syncDeps) updateMirror(ctx context.Context, home string, t syncTask, lim Limits) ([]byte, error) {
	dir := d.mirrors.Dir(t.RepoID)
	env := d.fetchEnv(home, t, lim)
	fetchArgv := append([]string{"git", "--git-dir=" + dir, "fetch", "--prune", "--force", "--", t.CanonicalURL}, mirrorRefspecs...)

	if d.mirrors.Exists(t.RepoID) {
		stderr, err := d.runBounded(ctx, d.fetchTimeoutOrDefault(), "fetch", fetchArgv, env, home)
		if err == nil {
			return nil, nil
		}
		// A fetch that was cancelled says nothing about the mirror: its stderr may still match a
		// corruption marker, and rebuilding would throw away a healthy mirror.
		if ctx.Err() != nil || !isCorruptMirror(stderr) {
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
	return d.runBoundedOut(ctx, timeout, what, argv, env, dir, io.Discard)
}

// runBoundedOut is runBounded with git's standard output streamed to stdout. Without the output seam
// it runs through run and the output is discarded.
func (d syncDeps) runBoundedOut(ctx context.Context, timeout time.Duration, what string, argv, env []string, dir string, stdout io.Writer) ([]byte, error) {
	// A cancelled sync must not start another git: with a superseded credential in its environment,
	// that would be one more process using it.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var stderr []byte
	var err error
	if d.runOut != nil {
		stderr, err = d.runOut(cctx, argv, env, dir, stdout)
	} else {
		stderr, err = d.run(cctx, argv, env, dir)
	}
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

// currentLimits is the limits in force for an attempt that is starting.
func (d syncDeps) currentLimits() Limits {
	if d.limits != nil {
		return d.limits()
	}
	return DefaultLimits()
}

// pushTimeout bounds one push process: the platform's limit when set, else the default.
func (lim Limits) pushTimeout() time.Duration {
	if lim.PushTimeout > 0 {
		return lim.PushTimeout
	}
	return defaultPushTimeout
}

// localFault reports a failure to prepare the sync (the mirror directory or temp home could not be
// created) through the same classifier, so a full disk reads as local_disk_exhausted.
func localFault(err error) syncResult {
	typ, detail := classifyGitFailure(originSide, err, []byte(err.Error()), false)
	return syncResult{State: StateError, ErrorType: typ, ErrorDetail: detail}
}
