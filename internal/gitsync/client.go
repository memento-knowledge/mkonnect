package gitsync

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/gateway"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// Repository lifecycle states reported in git_sync_status. These strings are part of the wire
// contract, so a test pins them against accidental drift.
const (
	// StatePending: the repository is configured and host-matched but no sync has started or
	// completed yet, or syncing is suspended (a critical security update is required).
	StatePending = "pending"
	// StateSyncing: a sync is queued or running. A repository waiting behind the concurrency limit
	// is reported as syncing too.
	StateSyncing = "syncing"
	StateOK      = "ok"
	StateError   = "error"
)

// Typed error codes carried as error_type when State is StateError. A single list covers every
// failure. ErrOriginAccessDenied also covers a repository whose host is not configured locally (the
// host-match guard); ErrUnclassified is the fallback that carries a sanitised detail so no failure
// is dropped. See classifyGitFailure for how a failed git invocation maps onto them.
//
// ErrReplicaRejected includes a replica refusal over its own size or quota limit. The separate
// ErrRepositoryOverLimit is reserved for the connector's own repository-size ceiling, a local check
// that is never inferred from git output (it is not enforced yet).
const (
	ErrOriginUnreachable   = "origin_unreachable"
	ErrOriginAccessDenied  = "origin_access_denied"
	ErrReplicaUnreachable  = "replica_unreachable"
	ErrCredentialRejected  = "credential_rejected"
	ErrReplicaRejected     = "replica_rejected"
	ErrPushTimeout         = "push_timeout"
	ErrLocalDiskExhausted  = "local_disk_exhausted"
	ErrJobOutOfMemory      = "job_out_of_memory"
	ErrRepositoryOverLimit = "repository_over_limit"
	ErrPushTokenExpired    = "push_token_expired"
	ErrUnclassified        = "unclassified"
)

// WarnLFSNotReplicated marks a repository that synced but skipped LFS objects — a warning, not an
// error, so the repository stays StateOK.
const WarnLFSNotReplicated = "lfs_not_replicated"

// gitSyncReadLimit caps a single INBOUND message. git-sync inbound frames (git_sync_config,
// git_sync_now) are control-sized; repository pack/bundle data flows OUTBOUND, which this does
// not bound, so a modest cap avoids a denial-of-service surface.
const gitSyncReadLimit int64 = 1 << 20 // 1 MiB

// defaultDrainTimeout bounds how long OnDraining waits for in-flight syncs. A gateway going_away
// means a platform restart is coming, so a sync that can finish promptly may report its result over
// this connection; one that cannot is left running and reports over the next. The wait is short
// because OnDraining blocks the gateway read loop while it waits, and a drain never cancels a sync,
// so a longer wait buys little.
const defaultDrainTimeout = 5 * time.Second

// Option customises a Client. The defaults suit production; the options exist so tests (and a
// packaging layer) can point the connector at a different git, mirror location, or drain bound.
type Option func(*clientOptions)

type clientOptions struct {
	run runFunc
	// runOut is how git is executed when its standard output is needed. Unset, it defaults to the
	// real git unless run was replaced (see WithGitRunner).
	runOut          outRunFunc
	runOverridden   bool
	runSaveInterval time.Duration
	mirrorsRoot     string
	drainTimeout    time.Duration
	statusInterval  time.Duration
}

// defaultStatusInterval is the minimum spacing between statuses sent because syncs finished. Each
// status echoes every repository, so one per completion would cost O(N²) bytes across N repositories.
const defaultStatusInterval = time.Second

// defaultRunSaveInterval is the minimum spacing between writes of the run state. Each write is the
// whole file, so one per finished sync would cost O(N²) bytes across N repositories. The first write
// after a quiet period is immediate and any that follow are folded into one trailing write; a crash
// can therefore lose at most this much of the most recent attempts' bookkeeping, which the next
// attempt replaces.
const defaultRunSaveInterval = 5 * time.Second

// WithStatusInterval sets the minimum spacing between completion-triggered statuses. The first is
// sent immediately and the last state is always flushed within one interval. The default is one second.
func WithStatusInterval(d time.Duration) Option {
	return func(o *clientOptions) { o.statusInterval = d }
}

// WithGitRunner replaces how git is executed. The default runs the real git binary in the locked-down
// environment (see runGit). A runner that returns only stderr cannot supply git's standard output,
// which the sync statistics (refs_pushed, newest_origin_commit_at, origin_default_branch) and a push's
// per-ref rejection reasons are read from, so replacing it turns those off; the other run-state
// fields are unaffected.
func WithGitRunner(run func(ctx context.Context, argv, env []string, dir string) ([]byte, error)) Option {
	return func(o *clientOptions) { o.run, o.runOverridden = run, true }
}

// WithMirrorsRoot sets the directory the bare mirrors live under. The default is DefaultMirrorsRoot.
func WithMirrorsRoot(root string) Option {
	return func(o *clientOptions) { o.mirrorsRoot = root }
}

// WithDrainTimeout bounds how long OnDraining waits for in-flight syncs. The default is 5 seconds.
func WithDrainTimeout(d time.Duration) Option {
	return func(o *clientOptions) { o.drainTimeout = d }
}

// Client is the git-sync connector. It implements gateway.Handler and drives a gateway.Client for
// the connection lifecycle, and runs mirror syncs for its host-matched repositories through a
// bounded manager.
type Client struct {
	cfg    *config.Config
	gw     *gateway.Client
	local  *Store
	prober OriginProber
	now    func() time.Time
	bootID string

	mgr          *manager
	sched        *scheduler
	drainTimeout time.Duration

	// limits holds the effective schedule and resource limits. It starts as DefaultLimits and is
	// replaced as a whole by whatever resolves a configuration's limits (ResolveLimits); until that
	// is wired in, the defaults apply. Read it through effectiveLimits.
	limits atomic.Pointer[Limits]

	// Completion-triggered statuses are coalesced: see notifyStatus.
	statusInterval  time.Duration
	notifyMu        sync.Mutex
	lastNotify      time.Time // wall clock, not c.now: it paces real timers
	notifyScheduled bool

	// criticalPatchRequired gates sync triggers while the gateway signals a required update.
	criticalPatchRequired atomic.Bool

	// seq is the monotonic status sequence within this process run; incremented under statusMu so
	// wire order matches sequence order.
	seq atomic.Uint64

	// originCheck caches the most recent pre-config reachability result; probing is guarded so only
	// one probe runs at a time and no more often than the health cadence.
	originCheck atomic.Pointer[proto.OriginCheck]
	probing     atomic.Bool
	lastProbe   atomic.Int64 // UnixNano of the last probe start

	// mu guards the configured set and version, which OnHeartbeat (its own goroutine) and
	// HandleMessage (the read loop) both reach.
	mu             sync.Mutex
	configAccepted bool
	configVersion  json.RawMessage
	repos          map[string]repoState // by repo_id
	// runs is what is remembered of each repository's attempts (see runRecord), by repo_id. It is
	// loaded from runStore at startup and written back after every change. It is guarded by mu, like
	// repos, so a status is built from one consistent view of both.
	runs     map[string]runRecord
	runStore *runStateStore

	// Writes of runs are coalesced: see markRunsDirty.
	runSaveEvery  time.Duration
	saveMu        sync.Mutex
	runsDirty     bool
	lastRunSave   time.Time // wall clock, not c.now: it paces real timers
	saveScheduled bool
	saveTimer     *time.Timer
	// connCtx/connSender are the current connection's context and sender, captured in OnConnected.
	// A probe started from a heartbeat must run under the connection's lifetime, NOT the
	// per-heartbeat context the gateway cancels as soon as OnHeartbeat returns.
	connCtx    context.Context
	connSender gateway.Sender

	// statusMu serialises build-and-send so a heartbeat cannot send a stale snapshot after the read
	// loop has already sent a newer one, and so the sequence number matches wire order.
	statusMu sync.Mutex
}

// NewClient creates a git-sync connector. prober runs the origin reachability check; now and bootID
// are injectable so tests are deterministic (now defaults to time.Now).
func NewClient(cfg *config.Config, keyStore *auth.KeyStore, local *Store, prober OriginProber, now func() time.Time, bootID string, opts ...Option) *Client {
	if now == nil {
		now = time.Now
	}
	o := clientOptions{run: runGit, mirrorsRoot: DefaultMirrorsRoot(), drainTimeout: defaultDrainTimeout, statusInterval: defaultStatusInterval, runSaveInterval: defaultRunSaveInterval}
	for _, opt := range opts {
		opt(&o)
	}
	if o.runOut == nil && !o.runOverridden {
		o.runOut = runGitOut
	}
	// git runs in a temporary directory, so a relative mirrors root would be resolved against that
	// (and lost with it) by git while Exists resolved it against the daemon's working directory.
	root := o.mirrorsRoot
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	c := &Client{
		cfg:            cfg,
		local:          local,
		prober:         prober,
		now:            now,
		bootID:         bootID,
		repos:          map[string]repoState{},
		runStore:       newRunStateStore(root),
		runSaveEvery:   o.runSaveInterval,
		drainTimeout:   o.drainTimeout,
		statusInterval: o.statusInterval,
	}
	c.runs = c.runStore.load()
	c.mgr = newManager(defaultSyncConcurrency,
		syncDeps{mirrors: Mirrors{Root: root}, run: o.run, runOut: o.runOut, now: now},
		syncHooks{
			resolve:   c.resolveTask,
			begin:     c.beginSync,
			started:   c.startAttempt,
			finish:    c.finishAndReschedule,
			release:   c.releaseSync,
			suspended: c.criticalPatchRequired.Load,
			changed:   c.notifyStatus,
		})
	defaults := DefaultLimits()
	c.limits.Store(&defaults)
	c.sched = newScheduler(schedHooks{
		repos:     c.schedulableTargets,
		enqueue:   c.mgr.enqueueScheduled,
		suspended: c.criticalPatchRequired.Load,
		limits:    c.effectiveLimits,
		now:       now,
		rand:      rand.Float64,
	})
	c.gw = gateway.NewClient(gateway.Config{
		GatewayURL:        cfg.GatewayURL,
		ConnectorID:       cfg.ConnectorID,
		ProtocolVersion:   cfg.ProtocolVersion,
		RegistrationToken: cfg.RegistrationToken,
		ReadLimit:         gitSyncReadLimit,
	}, keyStore, c)
	return c
}

// Run connects and maintains the connection with backoff until ctx is cancelled. It also runs the
// periodic resync scheduler for as long as it runs, and returns only after that has stopped.
func (c *Client) Run(ctx context.Context) {
	stop := c.sched.start(ctx)
	defer stop()
	c.gw.Run(ctx)
}

// effectiveLimits returns the limits in force: the defaults until a configuration supplies others.
func (c *Client) effectiveLimits() Limits { return *c.limits.Load() }

// Connect performs a single connection attempt.
func (c *Client) Connect(ctx context.Context) error { return c.gw.Connect(ctx) }

// --- gateway.Handler ---

// OnHandshake records the critical-patch gate before any message is dispatched.
func (c *Client) OnHandshake(info gateway.HandshakeInfo) {
	c.criticalPatchRequired.Store(info.CriticalPatch)
	if info.CriticalPatch {
		slog.Warn("gateway requires a critical security update — suspending git sync until updated")
	}
}

// OnConnected sends an immediate status (so the platform sees the connector promptly) and starts a
// pre-config origin reachability probe off the connection goroutine; the probe's result arrives in
// a later status, so the first write is never blocked on the network. The connection context and
// sender are captured so a later heartbeat-triggered probe runs under the connection's lifetime.
func (c *Client) OnConnected(ctx context.Context, s gateway.Sender) {
	c.mu.Lock()
	c.connCtx = ctx
	c.connSender = s
	c.mu.Unlock()
	c.lastProbe.Store(0) // allow an immediate probe on each new connection
	c.emitStatus(ctx, s, statusWriteTimeout)
	c.maybeProbe()
}

// OnHeartbeat pushes the current status each tick (the status resend cadence is the gateway health
// interval), refreshing the pre-config origin check no more often than that interval.
func (c *Client) OnHeartbeat(ctx context.Context, s gateway.Sender) error {
	c.maybeProbe()
	return c.sendStatus(ctx, s)
}

// OnDraining runs on a going_away before teardown. It waits a bounded time for in-flight syncs so a
// result that is about to land is reported over this connection. It never cancels them: syncs run
// under the process-lifetime context, not the connection's, so one that outlasts the wait simply
// keeps going across the reconnect and reports over the next connection.
func (c *Client) OnDraining(ctx context.Context) {
	dctx, cancel := context.WithTimeout(ctx, c.drainTimeout)
	defer cancel()
	c.mgr.drain(dctx)
}

// HandleMessage dispatches the git-sync inbound messages.
func (c *Client) HandleMessage(ctx context.Context, s gateway.Sender, typ string, raw json.RawMessage) {
	switch typ {
	case "git_sync_config":
		if c.applyConfig(raw) {
			// The schedule is brought up to date first, so the outcome of the sync started below,
			// even a failed one, is recorded against it.
			c.sched.reconcile()
			// Every accepted configuration syncs the syncable set: the platform re-sends it on a
			// token refresh, and a fetch of an unchanged repository is cheap. Syncs are marked
			// before the status is built, so the status already reports them as syncing.
			c.startSyncs(ctx, c.syncableRepos())
			c.emitStatus(ctx, s, dispatchStatusTimeout)
		}
	case "git_sync_now":
		var msg proto.GitSyncNowMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			slog.Warn("malformed git_sync_now message", "err", err)
			return
		}
		c.startSyncs(ctx, c.handleSyncNow(msg))
		c.emitStatus(ctx, s, dispatchStatusTimeout)
	default:
		slog.Debug("ignoring message", "type", typ)
	}
}

// startSyncs hands repositories to the sync manager unless a critical security update is required,
// which suspends all syncing. ctx is the one the gateway gives HandleMessage: the process-lifetime
// Connect context, NOT a connection's, so a drain or reconnect does not cancel a sync (only shutdown
// does). That is part of the gateway's Handler contract and is pinned by its
// TestHandlerContextLifetimes; this package's TestSyncAndItsRerunSurviveLosingTheConnection pins
// the consequence. Status replies use the connection's own context, captured in OnConnected.
func (c *Client) startSyncs(ctx context.Context, repoIDs []string) {
	if len(repoIDs) == 0 {
		return
	}
	if c.criticalPatchRequired.Load() {
		slog.Warn("not syncing: a critical security update is required first")
		return
	}
	c.mgr.enqueue(ctx, repoIDs)
}

// resolveTask snapshots what a sync of repoID needs, or reports that the repository is not (or no
// longer) syncable. It is the manager's view of the configured set, taken under mu.
func (c *Client) resolveTask(repoID string) (syncTask, bool) {
	c.mu.Lock()
	r, ok := c.repos[repoID]
	c.mu.Unlock()
	if !ok || !r.matched || r.replicaURL == "" || c.local == nil {
		return syncTask{}, false
	}
	origin, ok := c.local.Get(r.provider)
	if !ok {
		return syncTask{}, false
	}
	return syncTask{
		RepoID:         repoID,
		CanonicalURL:   r.canonicalURL,
		Origin:         origin,
		ReplicaURL:     r.replicaURL,
		PushToken:      r.pushToken,
		TokenExpiresAt: r.expiresAt,
	}, true
}

// beginSync marks repoID as syncing (queued or running), if it is still configured. The task itself
// is resolved later, when a slot is free, so only the repository is identified here.
func (c *Client) beginSync(repoID string) {
	c.setSyncing(repoID, true)
}

// releaseSync clears repoID's syncing mark for a sync that did not run, recording nothing.
func (c *Client) releaseSync(repoID string) {
	c.setSyncing(repoID, false)
}

func (c *Client) setSyncing(repoID string, syncing bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.repos[repoID]; ok {
		r.syncing = syncing
		if !syncing {
			r.attemptStartedAt = time.Time{}
		}
		c.repos[repoID] = r
	}
}

// startAttempt records when the attempt now running for t began, so a status sent while it runs
// reports it as last_attempt_at. A task whose target no longer matches the repository's current
// configuration is ignored, as finishSync ignores its result.
func (c *Client) startAttempt(repoID string, t syncTask, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.repos[repoID]; ok && targetMatches(r, t) {
		r.attemptStartedAt = at
		c.repos[repoID] = r
	}
}

// finishSync records a sync's outcome and clears the syncing mark. A task whose target no longer
// matches the repository's configuration (the platform reconfigured it mid-sync) is ignored, so a
// stale result is never attributed to the new target. A pending result means the sync was
// cancelled; nothing is recorded. Every other result, a failure as much as a success, is a finished
// attempt and updates the repository's run record, which is then saved: a failure to save is logged
// by the store and never affects the sync.
func (c *Client) finishSync(repoID string, t syncTask, res syncResult) {
	c.mu.Lock()
	r, ok := c.repos[repoID]
	if !ok || !targetMatches(r, t) {
		c.mu.Unlock()
		return
	}
	r.syncing = false
	r.attemptStartedAt = time.Time{}
	record := res.State != StatePending
	if record {
		r.result = res
		c.runs[repoID] = c.recordAttempt(c.runs[repoID], r.target, res)
	}
	c.repos[repoID] = r
	c.mu.Unlock()
	if record {
		c.markRunsDirty()
	}
}

// recordAttempt folds a finished attempt into a repository's run record. last_success_at moves only
// on a success (a run that found nothing new is one) and is otherwise carried; the attempt's own
// start, duration and push count always replace the previous attempt's; what the origin looked like
// is replaced only when this attempt learned it.
func (c *Client) recordAttempt(rec runRecord, target string, res syncResult) runRecord {
	if rec.Target != "" && rec.Target != target {
		rec = runRecord{}
	}
	rec.Target = target
	started, took := res.Stats.StartedAt, res.Stats.Duration
	if started.IsZero() {
		started = c.now().Add(-took)
	}
	rec.LastAttemptAt = utcSeconds(started)
	ms := took.Milliseconds()
	rec.DurationMS = &ms
	rec.RefsPushed = nil
	if res.Stats.RefsPushed != nil {
		n := *res.Stats.RefsPushed
		rec.RefsPushed = &n
	}
	if res.State == StateOK {
		rec.LastSuccessAt = utcSeconds(started.Add(took))
	}
	if !res.Stats.NewestCommitAt.IsZero() {
		rec.NewestOriginCommitAt = utcSeconds(res.Stats.NewestCommitAt)
	}
	if res.Stats.DefaultBranch != "" {
		rec.OriginDefaultBranch = res.Stats.DefaultBranch
	}
	return rec
}

// utcSeconds is t in UTC at the second precision the status reports.
func utcSeconds(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

// markRunsDirty notes that the run records changed and schedules their write. The first change after a
// quiet period is written at once, so an isolated sync survives a crash; changes that follow within
// the interval are folded into one trailing write, and Wait flushes whatever is still pending. It must
// be called without c.mu held.
func (c *Client) markRunsDirty() {
	c.saveMu.Lock()
	c.runsDirty = true
	wait := c.runSaveEvery - time.Since(c.lastRunSave)
	if wait <= 0 && !c.saveScheduled {
		c.lastRunSave = time.Now()
		c.saveMu.Unlock()
		c.flushRuns()
		return
	}
	if !c.saveScheduled {
		c.saveScheduled = true
		c.saveTimer = time.AfterFunc(wait, c.trailingSave)
	}
	c.saveMu.Unlock()
}

func (c *Client) trailingSave() {
	c.saveMu.Lock()
	c.saveScheduled = false
	c.lastRunSave = time.Now()
	c.saveMu.Unlock()
	c.flushRuns()
}

// flushRuns writes the run records if they changed since the last write. A failed write (logged by
// the store) leaves them marked changed, so the next flush retries it. The snapshot is taken by the
// store once it holds its write lock, so concurrent flushes can never write an older state last.
func (c *Client) flushRuns() {
	c.saveMu.Lock()
	if !c.runsDirty {
		c.saveMu.Unlock()
		return
	}
	c.runsDirty = false
	c.saveMu.Unlock()
	err := c.runStore.save(func() map[string]runRecord {
		c.mu.Lock()
		defer c.mu.Unlock()
		return maps.Clone(c.runs)
	})
	if err != nil {
		c.saveMu.Lock()
		c.runsDirty = true
		c.saveMu.Unlock()
	}
}

// stopRunSaves cancels a pending trailing write and flushes it now.
func (c *Client) stopRunSaves() {
	c.saveMu.Lock()
	if c.saveTimer != nil {
		c.saveTimer.Stop()
	}
	c.saveScheduled = false
	c.saveMu.Unlock()
	c.flushRuns()
}

// finishAndReschedule records a sync's outcome, then lets the scheduler plan the repository's next
// run from it.
func (c *Client) finishAndReschedule(repoID string, t syncTask, res syncResult) {
	c.finishSync(repoID, t, res)
	c.sched.completed(repoID, t, res)
}

// schedulableTargets returns the sync target of every repository the scheduler may start: exactly
// those resolveTask would accept. It copies no secret.
func (c *Client) schedulableTargets() map[string]targetKey {
	type candidate struct {
		id, provider string
		key          targetKey
	}
	c.mu.Lock()
	candidates := make([]candidate, 0, len(c.repos))
	for id, r := range c.repos {
		if r.matched && r.replicaURL != "" {
			candidates = append(candidates, candidate{id, r.provider, targetKey{r.canonicalURL, r.replicaURL}})
		}
	}
	c.mu.Unlock()

	out := make(map[string]targetKey, len(candidates))
	if c.local == nil {
		return out
	}
	for _, cand := range candidates {
		if _, ok := c.local.Get(cand.provider); ok {
			out[cand.id] = cand.key
		}
	}
	return out
}

// targetMatches reports whether t was resolved from r's current sync target.
func targetMatches(r repoState, t syncTask) bool {
	return r.canonicalURL == t.CanonicalURL && r.replicaURL == t.ReplicaURL
}

// Wait blocks until no sync is in flight or ctx ends, then writes any run state still pending. The
// daemon calls it after cancelling the process-lifetime context, which kills each git, so it can
// confirm they have stopped before exiting instead of leaving a process that holds a credential
// behind.
func (c *Client) Wait(ctx context.Context) {
	c.mgr.drain(ctx)
	c.stopRunSaves()
}

// handleSyncNow resolves the repositories an immediate-sync request addresses. A request is
// suppressed entirely while a critical patch is required. The caller starts the syncs.
func (c *Client) handleSyncNow(msg proto.GitSyncNowMsg) []string {
	if c.criticalPatchRequired.Load() {
		slog.Warn("ignoring git_sync_now: a critical security update is required before syncing", "repo_id", msg.RepoID)
		return nil
	}
	targets := c.syncTargets(msg.RepoID)
	if msg.RepoID != "" && len(targets) == 0 {
		slog.Warn("git_sync_now for a repository that cannot be synced", "repo_id", msg.RepoID)
		return nil
	}
	slog.Info("git_sync_now received", "repo_count", len(targets))
	return targets
}

// syncTargets resolves a git_sync_now repo_id to the repositories it addresses, restricted to the
// syncable set (configured, host-matched to a local provider, with a usable replica). An empty
// repo_id is the provisional "all syncable repositories" fan-out.
func (c *Client) syncTargets(repoID string) []string {
	syncable := c.syncableRepos()
	if repoID == "" {
		return syncable
	}
	if slices.Contains(syncable, repoID) {
		return []string{repoID}
	}
	return nil
}

// syncableRepos returns the sorted repo_ids that can be synced: host-matched to a local provider
// (resolved at applyConfig time) and carrying a usable replica URL.
func (c *Client) syncableRepos() []string {
	snapshot := c.snapshotRepos()
	var out []string
	for _, r := range snapshot {
		if r.matched && r.replicaURL != "" {
			out = append(out, r.repoID)
		}
	}
	slices.Sort(out)
	return out
}

// snapshotRepos returns a copy of the configured repositories taken under mu.
func (c *Client) snapshotRepos() []repoState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]repoState, 0, len(c.repos))
	for _, r := range c.repos {
		out = append(out, r)
	}
	return out
}
