package gitsync

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
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
	// StatePending: the repository is configured and host-matched but no sync has completed this run
	// (Phase 2 never advances past this, so a stubbed connector never reports a healthy "ok").
	StatePending = "pending"
	StateSyncing = "syncing"
	StateOK      = "ok"
	StateError   = "error"
)

// Typed error codes carried as error_type when State is StateError. A single list covers every
// failure; only ErrOriginAccessDenied is exercised in Phase 2 (the host-match guard and the
// reachability probe). The remainder are reserved for the Phase 3 sync work.
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
	// NAME_MAX of common filesystems (ext4, xfs, APFS), so Phase 3 MUST NOT use repo_id directly as a
	// filename (and never suffixed, e.g. "<id>.git") — it has to map it to a bounded directory name;
	// see the note on validRepoID. Must stay <= 1000 (see repoIDRE).
	maxRepoIDLength      = 256
	maxConfigVersionSize = 256
)

// repoIDRE constrains a platform-supplied repo_id to a safe charset and length. repo_id is used in
// local filesystem paths and replica push URLs in Phase 3, so anything outside this set — path
// separators, NUL, control characters, whitespace — is a traversal or injection vector and is
// rejected at config-validation time. The length bound is single-sourced from maxRepoIDLength,
// which must stay <= 1000: RE2 caps a bounded repetition at 1000, so a larger value would panic in
// MustCompile at package init.
var repoIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,` + strconv.Itoa(maxRepoIDLength) + `}$`)

// validRepoID reports whether a repo_id is safe to use as a path component. Beyond the charset:
//   - A leading "-" could be read as an option flag when repo_id is later passed as a git argv
//     element.
//   - A leading or trailing "." (so "." and "..", but also ".git" and "a.") is rejected: a leading
//     dot hides the entry and ".git" would collide with git's own metadata if Phase 3 roots a
//     repository at "<root>/<id>", and a trailing dot is dropped by some filesystems.
//   - Any embedded ".." is rejected so no id — even one like "a..b" — can form a traversal segment.
//
// This is deliberately stricter than the bare charset regex: restricting now is cheap, whereas
// loosening a stored-config contract later is not. Phase 3 should still map repo_id to a
// fixed-format directory name rather than rely on this charset alone.
func validRepoID(id string) bool {
	if !repoIDRE.MatchString(id) {
		return false
	}
	if strings.HasPrefix(id, "-") || strings.HasPrefix(id, ".") || strings.HasSuffix(id, ".") {
		return false
	}
	return !strings.Contains(id, "..")
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

// repoState is the Phase-2 retained subset of a configured repository. The push token is
// deliberately NOT retained: Phase 2 never pushes, and holding it only widens the leak surface.
//
// The raw origin_url string is deliberately NOT retained either. When a repository host-matches a
// local provider, applyConfig resolves and stores the matched provider name plus the validated,
// re-serialized canonical origin URL. Phase 3 dials canonicalURL directly and never re-parses an
// unvalidated string with git's URL parser (whose host interpretation can diverge from ours), so
// the host that was match-checked is byte-for-byte the host that gets contacted.
type repoState struct {
	repoID       string
	provider     string // matched local provider name, "" when unmatched
	canonicalURL string // validated, re-serialized origin URL for Phase 3 to dial, "" when unmatched
	expiresAt    string // push_token_expires_at (RFC3339), "" when none
	matched      bool   // origin_url binds to a local provider (resolved once, at applyConfig)
}

// Client is the git-sync connector. It implements gateway.Handler and drives a gateway.Client for
// the connection lifecycle. The sync algorithm is not implemented in Phase 2 (TODO(phase-3)).
type Client struct {
	cfg    *config.Config
	gw     *gateway.Client
	local  *Store
	prober OriginProber
	now    func() time.Time
	bootID string

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
func NewClient(cfg *config.Config, keyStore *auth.KeyStore, local *Store, prober OriginProber, now func() time.Time, bootID string) *Client {
	if now == nil {
		now = time.Now
	}
	c := &Client{
		cfg:    cfg,
		local:  local,
		prober: prober,
		now:    now,
		bootID: bootID,
		repos:  map[string]repoState{},
	}
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

// OnDraining runs on a going_away before teardown.
func (c *Client) OnDraining(_ context.Context) {
	// TODO(phase-3): wait (bounded) for in-flight sync goroutines before teardown.
}

// HandleMessage dispatches the git-sync inbound messages.
func (c *Client) HandleMessage(ctx context.Context, s gateway.Sender, typ string, raw json.RawMessage) {
	switch typ {
	case "git_sync_config":
		if c.applyConfig(raw) {
			// TODO(phase-3): reconcile the desired set against local state and (re)schedule syncs.
			c.emitStatus(ctx, s, dispatchStatusTimeout)
		}
	case "git_sync_now":
		var msg proto.GitSyncNowMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			slog.Warn("malformed git_sync_now message", "err", err)
			return
		}
		c.handleSyncNow(msg)
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
	// seen detects duplicates case-INsensitively: repo_id becomes a filesystem path component in
	// Phase 3, and ids differing only in case ("Repo" vs "repo") are distinct map keys but the same
	// directory on case-insensitive filesystems (APFS, Windows), so both must be rejected as a clash.
	// The id is already ASCII-validated by validRepoID, so strings.ToLower folds ASCII only here.
	seen := make(map[string]struct{}, len(msg.Repositories))
	for _, r := range msg.Repositories {
		if !validRepoID(r.RepoID) {
			// repo_id will be used in local filesystem paths and replica push URLs in Phase 3, so a
			// value outside the safe charset (or a traversal / dot component) is rejected here, not
			// silently carried. Not logged verbatim: a hostile id could carry log-injection bytes.
			slog.Warn("git_sync_config has an entry with an invalid repo_id; leaving configuration unchanged")
			return false
		}
		if _, dup := seen[strings.ToLower(r.RepoID)]; dup {
			slog.Warn("git_sync_config has a duplicate repo_id; leaving configuration unchanged", "repo_id", r.RepoID)
			return false
		}
		seen[strings.ToLower(r.RepoID)] = struct{}{}
		// Resolve the host-match once here: the local provider set is fixed for the daemon's life
		// (loaded at startup, no live reload), so the result is stable until the next config and
		// need not be recomputed on every status. The origin is parsed once: its canonical host key
		// selects the provider and its canonical URL is retained for Phase 3 to dial.
		var provider, canonicalURL string
		matched := false
		if c.local != nil {
			if hostKey, fullURL, err := canonicalRemoteURL(r.OriginURL); err == nil {
				if name, _, ok := c.local.matchByHostKey(hostKey); ok {
					provider, canonicalURL, matched = name, fullURL, true
				}
			}
		}
		next[r.RepoID] = repoState{repoID: r.RepoID, provider: provider, canonicalURL: canonicalURL, expiresAt: r.PushTokenExpiresAt, matched: matched}
	}
	if msg.ConfigVersion == nil {
		slog.Warn("git_sync_config has no config_version; status will report null until the platform includes one")
	}
	c.mu.Lock()
	c.repos = next
	c.configVersion = cloneRaw(msg.ConfigVersion)
	c.configAccepted = true
	c.mu.Unlock()
	slog.Info("git_sync_config accepted", "repo_count", len(next))
	return true
}

// handleSyncNow resolves the repositories an immediate-sync request addresses (and, in Phase 2,
// does not yet sync). A request is suppressed entirely while a critical patch is required.
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
	slog.Info("git_sync_now received; sync not yet implemented", "repo_count", len(targets))
	return targets
}

// syncTargets resolves a git_sync_now repo_id to the repositories it addresses, restricted to the
// matchable set (configured AND host-matched to a local provider). An empty repo_id is the
// provisional "all matchable repositories" fan-out.
func (c *Client) syncTargets(repoID string) []string {
	matchable := c.matchableRepos()
	if repoID == "" {
		return matchable
	}
	if slices.Contains(matchable, repoID) {
		return []string{repoID}
	}
	return nil
}

// matchableRepos returns the sorted repo_ids that host-match a local provider, using the match
// resolved at applyConfig time.
func (c *Client) matchableRepos() []string {
	snapshot := c.snapshotRepos()
	var out []string
	for _, r := range snapshot {
		if r.matched {
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
		if r.matched {
			st.State = StatePending
		} else {
			st.State = StateError
			st.ErrorType = ErrOriginAccessDenied
			st.ErrorDetail = "host not configured locally"
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
