package gitsync

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

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

// Caps on an inbound git_sync_config so a hostile or malformed push cannot amplify into every
// status (each accepted repository is echoed in every heartbeat status).
const (
	maxConfigRepos = 4096
	// maxRepoIDLength is the platform-contract bound on repo_id. It is at or above the 255-byte
	// NAME_MAX of common filesystems (ext4, xfs, APFS), so repo_id is never used directly as a
	// filename: Mirrors.Dir maps it to a fixed-format hashed directory name. Must stay <= 1000 (see
	// repoIDRE).
	maxRepoIDLength      = 256
	maxConfigVersionSize = 256
)

// repoIDRE constrains a platform-supplied repo_id to a safe charset and length. repo_id appears in
// logs and statuses and is the key of the configured set; it never reaches a path (the mirror
// directory is a hash of it) or a git argument. Anything outside this set — path separators, NUL,
// control characters, whitespace — is nonetheless rejected at config-validation time, as defence in
// depth and so it cannot be used for log injection. The length bound is single-sourced from
// maxRepoIDLength, which must stay <= 1000: RE2 caps a bounded repetition at 1000, so a larger value
// would panic in MustCompile at package init.
var repoIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,` + strconv.Itoa(maxRepoIDLength) + `}$`)

// validRepoID reports whether a repo_id conforms to the platform contract: the charset and length
// of repoIDRE, with the two pure path-traversal components "." and ".." additionally rejected
// (they name a directory rather than a repository and cannot be a safe path component anywhere).
//
// It deliberately does NOT reject other contract-valid ids — a leading "-", ".git", a trailing ".",
// an embedded ".." such as "a..b", or ids that differ only in case. Those are safe to STORE, and the
// risks they carry at the point of USE are closed there: repo_id is never a git argument or a path
// component (Mirrors.Dir hashes it, so length, case and ".."/".git" shapes cannot matter).
// Over-restricting here instead would reject platform-valid configurations whole and halt sync for
// every repository in the message, so the guard is kept to the agreed contract.
func validRepoID(id string) bool {
	return repoIDRE.MatchString(id) && id != "." && id != ".."
}

const (
	// statusWriteTimeout bounds the connection-time status write, before the heartbeat exists to
	// tear down a wedged connection.
	statusWriteTimeout = 30 * time.Second
	// dispatchStatusTimeout bounds a status write made from the read loop or a probe goroutine,
	// whose context is the process-lifetime Connect context.
	dispatchStatusTimeout = 10 * time.Second
	// gitSyncReadLimit caps a single INBOUND message. git-sync inbound frames (git_sync_config,
	// git_sync_now) are control-sized; repository pack/bundle data flows OUTBOUND, which this does
	// not bound, so a modest cap avoids a denial-of-service surface.
	gitSyncReadLimit int64 = 1 << 20 // 1 MiB
	// rfc3339Nano is a fixed-width RFC3339 layout that always carries a sub-second fraction, so
	// sent_at is stable even when the nanosecond component is zero.
	rfc3339Nano = "2006-01-02T15:04:05.000000000Z07:00"
)

// repoState is the retained state of a configured repository.
//
// The raw origin_url string is deliberately NOT retained. When a repository host-matches a local
// provider, applyConfig resolves and stores the matched provider name plus the validated,
// re-serialized canonical origin URL. The sync dials canonicalURL directly and never re-parses an
// unvalidated string with git's URL parser (whose host interpretation can diverge from ours), so
// the host that was match-checked is byte-for-byte the host that gets contacted.
//
// The push token is retained, in memory only and as a self-redacting Secret, and only for a
// repository that is host-matched and has a usable replica URL: a repository that can never be
// synced holds no secret. Hardening it at rest is Phase 4.
type repoState struct {
	repoID       string
	provider     string // matched local provider name, "" when unmatched
	canonicalURL string // validated, re-serialized origin URL the sync dials, "" when unmatched
	expiresAt    string // push_token_expires_at (RFC3339), "" when none
	matched      bool   // origin_url binds to a local provider (resolved once, at applyConfig)

	replicaURL string // validated, re-serialized https replica URL, "" when unmatched or unusable
	pushToken  Secret // platform push token for replicaURL; empty exactly when replicaURL is
	// result is the recorded outcome of the last sync attempt, or the reason the configuration is
	// unusable. A zero State means no outcome yet, which is reported as pending.
	result  syncResult
	syncing bool // a sync is queued or running
}

// errInvalidReplica is the detail reported for a matched repository whose replica URL or push token
// is unusable. It is deliberately generic: the status must never echo the platform-supplied URL.
const errInvalidReplica = "invalid replica configuration"

// validReplicaURL validates a platform-supplied replica push URL and returns its re-serialized
// canonical form. The push token travels to this URL, so it must be https; it gets the same host
// validation as an origin so the host that is validated is the host that is dialled.
func validReplicaURL(raw string) (string, bool) {
	_, full, err := canonicalRemoteURL(raw)
	if err != nil || !strings.HasPrefix(full, "https://") {
		return "", false
	}
	return full, true
}

// validPushToken reports whether a platform-supplied push token is usable. It becomes an
// Authorization header value in a subprocess environment, so it must be non-empty and free of
// control characters: a CR or LF would inject headers into the replica request and a NUL would make
// the process fail to start. (The operator's own origin token gets the same check; see
// LocalEntry.Validate.)
func validPushToken(tok Secret) bool {
	s := tok.Reveal()
	return s != "" && !strings.ContainsFunc(s, unicode.IsControl)
}

// sameSyncIdentity reports whether two states describe the same sync target, so a prior result is
// still meaningful. The push token is deliberately not part of it: a refreshed token for the same
// repository does not invalidate its last result.
func sameSyncIdentity(a, b repoState) bool {
	return a.matched == b.matched && a.provider == b.provider &&
		a.canonicalURL == b.canonicalURL && a.replicaURL == b.replicaURL
}

// defaultDrainTimeout bounds how long OnDraining waits for in-flight syncs. A gateway going_away
// means a platform restart is coming, so a sync that can finish promptly may report its result over
// this connection; one that cannot is left running and reports over the next.
const defaultDrainTimeout = 30 * time.Second

// Option customises a Client. The defaults suit production; the options exist so tests (and a
// packaging layer) can point the connector at a different git, mirror location, or drain bound.
type Option func(*clientOptions)

type clientOptions struct {
	run            runFunc
	mirrorsRoot    string
	drainTimeout   time.Duration
	statusInterval time.Duration
}

// defaultStatusInterval is the minimum spacing between statuses sent because syncs finished. Each
// status echoes every repository, so one per completion would cost O(N²) bytes across N repositories.
const defaultStatusInterval = time.Second

// WithStatusInterval sets the minimum spacing between completion-triggered statuses. The first is
// sent immediately and the last state is always flushed within one interval. The default is one second.
func WithStatusInterval(d time.Duration) Option {
	return func(o *clientOptions) { o.statusInterval = d }
}

// WithGitRunner replaces how git is executed. The default runs the real git binary in the locked-down
// environment (see runGit).
func WithGitRunner(run func(ctx context.Context, argv, env []string, dir string) ([]byte, error)) Option {
	return func(o *clientOptions) { o.run = run }
}

// WithMirrorsRoot sets the directory the bare mirrors live under. The default is DefaultMirrorsRoot.
func WithMirrorsRoot(root string) Option {
	return func(o *clientOptions) { o.mirrorsRoot = root }
}

// WithDrainTimeout bounds how long OnDraining waits for in-flight syncs. The default is 30 seconds.
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
	drainTimeout time.Duration

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
	o := clientOptions{run: runGit, mirrorsRoot: DefaultMirrorsRoot(), drainTimeout: defaultDrainTimeout, statusInterval: defaultStatusInterval}
	for _, opt := range opts {
		opt(&o)
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
		drainTimeout:   o.drainTimeout,
		statusInterval: o.statusInterval,
	}
	c.mgr = newManager(defaultSyncConcurrency,
		syncDeps{mirrors: Mirrors{Root: root}, run: o.run, now: now},
		syncHooks{
			resolve:   c.resolveTask,
			begin:     c.beginSync,
			finish:    c.finishSync,
			release:   c.releaseSync,
			suspended: c.criticalPatchRequired.Load,
			changed:   c.notifyStatus,
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

// Run connects and maintains the connection with backoff until ctx is cancelled.
func (c *Client) Run(ctx context.Context) { c.gw.Run(ctx) }

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

// applyConfig validates an inbound git_sync_config and, only if the whole message is acceptable,
// swaps in the new desired set and config version. A rejected message leaves the previous
// configuration untouched and does not advance the version. It returns whether the config was
// accepted (and a status should follow). It never logs a credential or push token.
func (c *Client) applyConfig(raw json.RawMessage) bool {
	var msg proto.GitSyncConfigMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		slog.Warn("malformed git_sync_config message", "err", err)
		return false
	}
	if msg.Repositories == nil {
		slog.Warn("git_sync_config has no repositories field; leaving configuration unchanged")
		return false
	}
	if len(msg.Repositories) > maxConfigRepos {
		slog.Warn("git_sync_config exceeds the repository cap; ignoring", "count", len(msg.Repositories), "cap", maxConfigRepos)
		return false
	}
	if msg.ConfigVersion != nil && (len(msg.ConfigVersion) > maxConfigVersionSize || !isScalarJSON(msg.ConfigVersion)) {
		slog.Warn("git_sync_config has an invalid config_version; leaving configuration unchanged")
		return false
	}
	next := make(map[string]repoState, len(msg.Repositories))
	for _, r := range msg.Repositories {
		if !validRepoID(r.RepoID) {
			// A value outside the contract charset (or a "." / ".." traversal component) is rejected
			// here rather than carried into logs and statuses. Not logged verbatim: a hostile id
			// could carry log-injection bytes.
			slog.Warn("git_sync_config has an entry with an invalid repo_id; leaving configuration unchanged")
			return false
		}
		if _, dup := next[r.RepoID]; dup {
			slog.Warn("git_sync_config has a duplicate repo_id; leaving configuration unchanged", "repo_id", r.RepoID)
			return false
		}
		// Resolve the host-match once here: the local provider set is fixed for the daemon's life
		// (loaded at startup, no live reload), so the result is stable until the next config and
		// need not be recomputed on every status. The origin is parsed once: its canonical host key
		// selects the provider and its canonical URL is retained for the sync to dial.
		var provider, canonicalURL string
		matched := false
		if c.local != nil {
			if hostKey, fullURL, err := canonicalRemoteURL(r.OriginURL); err == nil {
				if name, _, ok := c.local.matchByHostKey(hostKey); ok {
					provider, canonicalURL, matched = name, fullURL, true
				}
			}
		}
		st := repoState{repoID: r.RepoID, provider: provider, canonicalURL: canonicalURL, expiresAt: r.PushTokenExpiresAt, matched: matched}
		if matched {
			// A bad replica fails this one repository, not the whole message: the others are
			// independent and halting them all would be disproportionate.
			if replicaURL, ok := validReplicaURL(r.ReplicaPushURL); ok && validPushToken(r.PushToken) {
				st.replicaURL, st.pushToken = replicaURL, r.PushToken
			} else {
				st.result = syncResult{State: StateError, ErrorType: ErrUnclassified, ErrorDetail: errInvalidReplica}
			}
		}
		next[r.RepoID] = st
	}
	if msg.ConfigVersion == nil {
		slog.Warn("git_sync_config has no config_version; status will report null until the platform includes one")
	}
	c.mu.Lock()
	// Carry a prior result and in-flight marker across a configuration that leaves a repository's
	// sync target unchanged (the platform re-sends config on every token refresh), so a healthy
	// repository is not reset to pending. A changed target starts over.
	for id, st := range next {
		if old, ok := c.repos[id]; ok && sameSyncIdentity(old, st) {
			if st.result.State == "" {
				st.result = old.result
			}
			st.syncing = old.syncing
			next[id] = st
		}
	}
	c.repos = next
	c.configVersion = cloneRaw(msg.ConfigVersion)
	c.configAccepted = true
	c.mu.Unlock()
	slog.Info("git_sync_config accepted", "repo_count", len(next))
	return true
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
		c.repos[repoID] = r
	}
}

// finishSync records a sync's outcome and clears the syncing mark. A task whose target no longer
// matches the repository's configuration (the platform reconfigured it mid-sync) is ignored, so a
// stale result is never attributed to the new target. A pending result means the sync was
// cancelled; nothing is recorded.
func (c *Client) finishSync(repoID string, t syncTask, res syncResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.repos[repoID]
	if !ok || !targetMatches(r, t) {
		return
	}
	r.syncing = false
	if res.State != StatePending {
		r.result = res
	}
	c.repos[repoID] = r
}

// targetMatches reports whether t was resolved from r's current sync target.
func targetMatches(r repoState, t syncTask) bool {
	return r.canonicalURL == t.CanonicalURL && r.replicaURL == t.ReplicaURL
}

// notifyStatus reports that a sync finished. Each status echoes every repository, so sending one per
// completion would cost O(N²) bytes for N repositories; instead the first is sent at once and any
// that follow within statusInterval are folded into a single trailing flush, which also guarantees
// the final state is reported.
func (c *Client) notifyStatus() {
	c.notifyMu.Lock()
	wait := c.statusInterval - time.Since(c.lastNotify)
	if wait <= 0 && !c.notifyScheduled {
		c.lastNotify = time.Now()
		c.notifyMu.Unlock()
		c.sendCurrentStatus()
		return
	}
	if !c.notifyScheduled {
		c.notifyScheduled = true
		time.AfterFunc(wait, c.flushNotify)
	}
	c.notifyMu.Unlock()
}

func (c *Client) flushNotify() {
	c.notifyMu.Lock()
	c.notifyScheduled = false
	c.lastNotify = time.Now()
	c.notifyMu.Unlock()
	c.sendCurrentStatus()
}

// Wait blocks until no sync is in flight or ctx ends. The daemon calls it after cancelling the
// process-lifetime context, which kills each git, so it can confirm they have stopped before exiting
// instead of leaving a process that holds a credential behind.
func (c *Client) Wait(ctx context.Context) { c.mgr.drain(ctx) }

// sendCurrentStatus sends the current status over the current connection, if there is a live one. A
// sync that finishes mid-reconnect simply skips it: the next heartbeat reports the result anyway.
func (c *Client) sendCurrentStatus() {
	c.mu.Lock()
	ctx, s := c.connCtx, c.connSender
	c.mu.Unlock()
	if ctx == nil || s == nil || ctx.Err() != nil {
		return
	}
	c.emitStatus(ctx, s, dispatchStatusTimeout)
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

// buildStatus assembles the current git_sync_status. repositories is always non-nil so it marshals
// as [] rather than null even when empty. Called under statusMu, so the sequence increment here
// matches wire order.
func (c *Client) buildStatus() proto.GitSyncStatusMsg {
	c.mu.Lock()
	cv := cloneRaw(c.configVersion)
	accepted := c.configAccepted
	snapshot := make([]repoState, 0, len(c.repos))
	for _, r := range c.repos {
		snapshot = append(snapshot, r)
	}
	c.mu.Unlock()

	slices.SortFunc(snapshot, func(a, b repoState) int { return strings.Compare(a.repoID, b.repoID) })

	repos := make([]proto.GitSyncRepoStatus, 0, len(snapshot))
	var earliest *time.Time
	for _, r := range snapshot {
		st := proto.GitSyncRepoStatus{RepoID: r.repoID}
		switch {
		case !r.matched:
			st.State = StateError
			st.ErrorType = ErrOriginAccessDenied
			st.ErrorDetail = "host not configured locally"
		case r.syncing:
			st.State = StateSyncing
		case r.result.State != "":
			st.State = r.result.State
			st.ErrorType = r.result.ErrorType
			st.ErrorDetail = r.result.ErrorDetail
			st.Warnings = r.result.Warnings
		default:
			st.State = StatePending
		}
		repos = append(repos, st)
		if t, ok := parseExpiry(r.expiresAt); ok && (earliest == nil || t.Before(*earliest)) {
			tt := t
			earliest = &tt
		}
	}

	status := proto.GitSyncStatusMsg{
		Type:          "git_sync_status",
		ConfigVersion: cv,
		SentAt:        c.now().UTC().Format(rfc3339Nano),
		Sequence:      c.seq.Add(1),
		BootID:        c.bootID,
		Repositories:  repos,
	}
	if earliest != nil {
		s := earliest.UTC().Format(time.RFC3339)
		status.EarliestPushTokenExpiresAt = &s
	}
	// origin_check is a connector-level, pre-configuration signal only.
	if !accepted {
		if oc := c.originCheck.Load(); oc != nil {
			status.OriginCheck = oc
		}
	}
	return status
}

// maybeProbe launches a pre-config origin reachability probe on a goroutine if the connector has no
// accepted configuration, has at least one local provider, is not already probing, and has not
// probed within the health cadence. The probe and its status send run under the CONNECTION context
// captured in OnConnected (not a per-heartbeat context, which the gateway cancels on return), so a
// heartbeat-triggered refresh is not killed the instant OnHeartbeat returns. The throttle applies to
// every attempt — including failed ones — so a persistent local fault (e.g. git missing) does not
// exec git on every heartbeat. A probe cancelled by a closing connection is never cached.
func (c *Client) maybeProbe() {
	c.mu.Lock()
	accepted := c.configAccepted
	ctx := c.connCtx
	s := c.connSender
	c.mu.Unlock()
	if accepted || ctx == nil || s == nil || ctx.Err() != nil {
		return
	}
	if c.local == nil || len(c.local.Names()) == 0 {
		return
	}
	if !c.probing.CompareAndSwap(false, true) {
		return
	}
	if c.now().Sub(time.Unix(0, c.lastProbe.Load())) < gateway.DefaultHeartbeatInterval {
		c.probing.Store(false)
		return
	}
	c.lastProbe.Store(c.now().UnixNano())
	go func() {
		defer c.probing.Store(false)
		res, ok := c.runProbe(ctx)
		if !ok || ctx.Err() != nil {
			return
		}
		c.originCheck.Store(&proto.OriginCheck{Result: string(res), CheckedAt: c.now().UTC().Format(rfc3339Nano)})
		c.emitStatus(ctx, s, dispatchStatusTimeout)
	}()
}

// runProbe probes every configured provider and aggregates worst-to-best (access_denied over
// unreachable over reachable). ok is false when no provider yielded a usable result (for example
// git is not installed), in which case origin_check is left absent.
func (c *Client) runProbe(ctx context.Context) (OriginResult, bool) {
	rank := map[OriginResult]int{OriginReachable: 0, OriginUnreachable: 1, OriginAccessDenied: 2}
	worst := OriginReachable
	any := false
	for _, e := range c.local.List() {
		res, err := c.prober.Probe(ctx, e)
		if err != nil {
			slog.Warn("origin reachability check could not run", "err", err)
			continue
		}
		any = true
		if rank[res] > rank[worst] {
			worst = res
		}
	}
	return worst, any
}

// sendStatus builds and sends the current status under statusMu, so concurrent heartbeat and read
// loop sends cannot reorder a stale snapshot ahead of a fresh one.
func (c *Client) sendStatus(ctx context.Context, s gateway.Sender) error {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	return s.Send(ctx, c.buildStatus())
}

// emitStatus sends a status with its own timeout, for callers whose context is the process-lifetime
// Connect context rather than a per-write bound.
func (c *Client) emitStatus(ctx context.Context, s gateway.Sender, timeout time.Duration) {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.sendStatus(wctx, s); err != nil {
		slog.Warn("failed to send git_sync_status", "err", err)
	}
}

// cloneRaw copies a RawMessage so the stored/echoed value does not alias a decoder's buffer.
func cloneRaw(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	return append(json.RawMessage(nil), b...)
}

// isScalarJSON reports whether b is a valid JSON scalar (not an object or array). config_version is
// treated opaquely but constrained to a scalar so a hostile nested value cannot be echoed verbatim.
func isScalarJSON(b json.RawMessage) bool {
	if !json.Valid(b) {
		return false
	}
	s := bytes.TrimSpace(b)
	if len(s) == 0 {
		return false
	}
	switch s[0] {
	case '{', '[':
		return false
	}
	return true
}

// parseExpiry parses an RFC3339 timestamp, reporting ok=false for an empty or unparseable value, so
// a malformed expiry is skipped when computing the earliest rather than being misread as a time.
func parseExpiry(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
