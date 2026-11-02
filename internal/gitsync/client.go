package gitsync

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/gateway"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// Repository lifecycle states reported in git_sync_status. The set is provisional until the
// git-sync protocol is finalised.
const (
	// StatePending means the connector knows about the repository but no sync has run yet —
	// deliberately NOT a healthy "in sync" value, so a stubbed connector never reports as if it
	// had synced.
	StatePending = "pending"
	// StateNotConfigured means the platform asked to sync the repository but the connector has
	// no local configuration for it, so it cannot act.
	StateNotConfigured = "not_configured"
	// StateSyncing and StateError are part of the intended vocabulary but are only emitted once
	// the Phase 3 sync loop exists.
	StateSyncing = "syncing"
	StateError   = "error"
)

// statusWriteTimeout bounds the initial connection-time status write, which runs before the
// heartbeat exists to tear down a wedged connection. Mirrors the API connector's equivalent.
const statusWriteTimeout = 30 * time.Second

// dispatchStatusTimeout bounds a status write made in response to an inbound message. Those
// run on the read loop (process-lifetime context), so an unbounded write could stall the loop
// and delay going_away handling — the same reasoning as the API connector's status_request.
const dispatchStatusTimeout = 10 * time.Second

// gitSyncReadLimit caps a single INBOUND message. The git-sync inbound messages are the small
// git_sync_config / git_sync_now control frames, so a modest cap is right: a large cap would be
// a denial-of-service surface on a memory-constrained connector (each frame is buffered as a
// RawMessage and decoded twice). Phase 3 should raise this ONLY if it adds an inbound payload
// that genuinely needs it; repository pack/bundle data flows OUTBOUND via Sender.Send, which the
// read limit does not bound.
const gitSyncReadLimit int64 = 1 << 20 // 1 MiB

// Client is the git-sync connector. It implements gateway.Handler and drives a gateway.Client
// for the connection lifecycle, exactly as the API connector does. The sync algorithm itself is
// not implemented in Phase 2 (see the TODO(phase-3) markers).
type Client struct {
	cfg   *config.Config
	gw    *gateway.Client
	local *LocalConfig

	// criticalPatchRequired gates sync triggers: when the gateway signals a required critical
	// update, the connector must not start new work until it is upgraded.
	criticalPatchRequired atomic.Bool

	// mu guards configuredRepos, which OnHeartbeat (its own goroutine) and HandleMessage (the
	// read loop) both reach — the gateway documents that those two can overlap.
	mu              sync.Mutex
	configuredRepos map[string]proto.GitSyncRepoConfig // latest git_sync_config, by repo_key

	// statusMu serialises build-and-send of a status so a heartbeat cannot send a stale snapshot
	// after the read loop has already sent a newer one (the two writes serialise on the gateway's
	// write mutex, but that does not preserve build order).
	statusMu sync.Mutex
}

// NewClient creates a git-sync connector that enrols with the given key store and reads its
// per-repository configuration from local.
func NewClient(cfg *config.Config, keyStore *auth.KeyStore, local *LocalConfig) *Client {
	c := &Client{
		cfg:             cfg,
		local:           local,
		configuredRepos: map[string]proto.GitSyncRepoConfig{},
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

// Run connects to the gateway and maintains the connection with exponential backoff. It blocks
// until ctx is cancelled.
func (c *Client) Run(ctx context.Context) { c.gw.Run(ctx) }

// Connect performs a single connection attempt and returns when it closes or ctx is cancelled.
func (c *Client) Connect(ctx context.Context) error { return c.gw.Connect(ctx) }

// --- gateway.Handler ---

// OnHandshake records the critical-patch gate before any message is dispatched.
func (c *Client) OnHandshake(info gateway.HandshakeInfo) {
	c.criticalPatchRequired.Store(info.CriticalPatch)
	if info.CriticalPatch {
		slog.Warn("gateway requires a critical security update — suspending git sync until updated")
	}
}

// OnConnected emits the initial git_sync_status, bounded so a stalled first write cannot hang
// before the heartbeat exists to tear the connection down.
func (c *Client) OnConnected(ctx context.Context, s gateway.Sender) {
	wctx, cancel := context.WithTimeout(ctx, statusWriteTimeout)
	defer cancel()
	if err := c.sendStatus(wctx, s); err != nil {
		slog.Warn("failed to send initial git_sync_status", "err", err)
	}
}

// OnHeartbeat pushes the current git_sync_status on each heartbeat tick, mirroring how the API
// connector pushes connection_status. Returning the send error closes the connection.
func (c *Client) OnHeartbeat(ctx context.Context, s gateway.Sender) error {
	return c.sendStatus(ctx, s)
}

// OnDraining runs on a going_away before teardown.
func (c *Client) OnDraining(_ context.Context) {
	// TODO(phase-3): wait for in-flight sync goroutines to finish (bounded) before the
	// connection is torn down, as the API connector waits for in-flight requests.
}

// HandleMessage dispatches the git-sync inbound messages.
func (c *Client) HandleMessage(ctx context.Context, s gateway.Sender, typ string, raw json.RawMessage) {
	switch typ {
	case "git_sync_config":
		var msg proto.GitSyncConfigMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			slog.Warn("malformed git_sync_config message", "err", err)
			return
		}
		c.applyConfig(msg)
		// TODO(phase-3): reconcile the desired repository set against local state and
		// (re)schedule syncs. Phase 2 only records the config and reports status.
		c.emitStatusBounded(ctx, s)

	case "git_sync_now":
		var msg proto.GitSyncNowMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			slog.Warn("malformed git_sync_now message", "err", err)
			return
		}
		c.handleSyncNow(msg)
		c.emitStatusBounded(ctx, s)

	default:
		slog.Debug("ignoring message", "type", typ)
	}
}

// applyConfig replaces the desired repository set from a git_sync_config push. Entries with an
// empty repo_key are skipped (the empty key is the fan-out sentinel for git_sync_now and must
// never become a real repository).
func (c *Client) applyConfig(msg proto.GitSyncConfigMsg) {
	next := make(map[string]proto.GitSyncRepoConfig, len(msg.Repositories))
	for _, r := range msg.Repositories {
		if r.RepoKey == "" {
			slog.Warn("ignoring git_sync_config entry with an empty repo_key")
			continue
		}
		next[r.RepoKey] = r
	}
	c.mu.Lock()
	c.configuredRepos = next
	c.mu.Unlock()
}

// handleSyncNow is the Phase 2 stub for an immediate-sync request. It resolves and returns the
// repositories the request addresses (for tests and future phases) but does not yet sync.
func (c *Client) handleSyncNow(msg proto.GitSyncNowMsg) []string {
	if c.criticalPatchRequired.Load() {
		slog.Warn("ignoring git_sync_now: a critical security update is required before syncing", "repo_key", msg.RepoKey)
		return nil
	}
	targets := c.syncTargets(msg.RepoKey)
	if msg.RepoKey != "" && len(targets) == 0 {
		// A request for a repository that is not syncable (unknown, or configured without local
		// credentials): log and ignore, never fall back to a fan-out. (repo_key is a
		// platform-chosen identifier, not secret.)
		slog.Warn("git_sync_now for a repository that cannot be synced", "repo_key", msg.RepoKey)
		return nil
	}
	// TODO(phase-3): trigger a bounded, rate-limited sync for each target on a tracked goroutine
	// (see OnDraining). Phase 2 only acknowledges via the status push that follows.
	slog.Info("git_sync_now received; sync not yet implemented", "repo_count", len(targets))
	return targets
}

// syncTargets resolves a git_sync_now repo_key to the repositories it addresses, restricted to
// the SYNCABLE set (configured by the platform AND present in local config). An empty key is the
// provisional "all syncable repositories" signal — a fan-out that Phase 3 must coalesce and
// rate-limit, and that the platform contract should prefer to express explicitly rather than by
// a dropped key.
func (c *Client) syncTargets(repoKey string) []string {
	syncable := c.syncableRepos()
	if repoKey == "" {
		return syncable
	}
	if slices.Contains(syncable, repoKey) {
		return []string{repoKey}
	}
	return nil
}

// syncableRepos returns the sorted intersection of the platform-configured repositories and the
// locally-configured ones: a repository can be synced only when the platform has asked for it
// and the connector holds its origin/credentials. Used for sync targets, not for status.
func (c *Client) syncableRepos() []string {
	c.mu.Lock()
	configured := maps.Clone(c.configuredRepos)
	c.mu.Unlock()
	var out []string
	for key := range configured {
		if c.local != nil {
			if _, ok := c.local.Get(key); ok {
				out = append(out, key)
			}
		}
	}
	slices.Sort(out)
	return out
}

// knownRepos returns the sorted union of the repositories the platform has configured and those
// present in local config, so status can surface a platform-configured repository the connector
// cannot yet act on. It mirrors the API connector's union of registry and credential-store keys.
// Note: this discloses locally-configured repo keys (operator-chosen identifiers) to the
// platform, exactly as the API connector discloses configured plugin keys.
func (c *Client) knownRepos() []string {
	seen := make(map[string]struct{})
	c.mu.Lock()
	for k := range c.configuredRepos {
		seen[k] = struct{}{}
	}
	c.mu.Unlock()
	if c.local != nil {
		for _, k := range c.local.Repos() {
			seen[k] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// buildStatus assembles the current git_sync_status. The repositories slice is always non-nil so
// it marshals as [] rather than null even when empty.
func (c *Client) buildStatus() proto.GitSyncStatusMsg {
	keys := c.knownRepos()
	repos := make([]proto.GitSyncRepoStatus, 0, len(keys))
	for _, k := range keys {
		state := StateNotConfigured
		if c.local != nil {
			if _, ok := c.local.Get(k); ok {
				state = StatePending
			}
		}
		repos = append(repos, proto.GitSyncRepoStatus{RepoKey: k, State: state})
		// TODO(phase-3): populate OriginDefaultBranch once the origin has been contacted, and
		// ErrorDetail with a failure CLASS only — via a sanitiser analogous to the API
		// connector's safeDiagnostic, never a URL, host, or path.
	}
	return proto.GitSyncStatusMsg{Type: "git_sync_status", Repositories: repos}
}

// sendStatus builds and sends the current status under statusMu, so a concurrent heartbeat and
// read-loop send cannot reorder a stale snapshot ahead of a fresh one.
func (c *Client) sendStatus(ctx context.Context, s gateway.Sender) error {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	return s.Send(ctx, c.buildStatus())
}

// emitStatusBounded sends a status with its own short timeout, for the read-loop callers whose
// context is the process-lifetime Connect context rather than a per-write bound.
func (c *Client) emitStatusBounded(ctx context.Context, s gateway.Sender) {
	wctx, cancel := context.WithTimeout(ctx, dispatchStatusTimeout)
	defer cancel()
	if err := c.sendStatus(wctx, s); err != nil {
		slog.Warn("failed to send git_sync_status", "err", err)
	}
}
