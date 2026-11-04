package gitsync

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// defaultSyncConcurrency is how many repositories sync at once. Each sync is a git subprocess that
// may be memory- and bandwidth-heavy, so the default is deliberately small. It is not yet
// configurable per deployment.
const defaultSyncConcurrency = 4

// syncHooks connect the manager to the connector's state without the manager knowing about it.
type syncHooks struct {
	// resolve returns a snapshot of the task to run for repoID, or false when the repository is not
	// (or no longer) syncable. It is called only once a concurrency slot is held, immediately before
	// the sync starts, so a repository that queued behind others runs with the token and target it
	// has when it runs, not the ones it had when it was queued.
	resolve func(repoID string) (syncTask, bool)
	// begin marks the repository as syncing (queued or running). It runs synchronously inside
	// enqueue, so a status built right after enqueue returns already reports it.
	begin func(repoID string)
	// started records that the attempt for repoID is now running, beginning at the given time. It runs
	// once a concurrency slot is held, immediately before the sync, so a repository that is merely
	// queued has no attempt in flight. May be nil.
	started func(repoID string, t syncTask, at time.Time)
	// finish records the outcome and clears the syncing mark. A pending result means the sync was
	// cancelled and nothing should be recorded. The implementation must ignore a task whose target
	// no longer matches the repository's current configuration.
	finish func(repoID string, t syncTask, r syncResult)
	// release clears the syncing mark of a repository that did not run (cancelled while queued, no
	// longer syncable, or suspended), recording nothing.
	release func(repoID string)
	// suspended reports whether all syncing is currently suspended (a critical security update is
	// required). It is checked when a queued sync or a rerun is about to start, so work queued
	// before the gate closed does not run after it.
	suspended func() bool
	// changed reports that a repository's state changed in a way worth sending a status for.
	changed func()
}

// manager runs repository syncs with bounded concurrency. At most one sync per repository is ever
// in flight: a trigger that arrives meanwhile is folded into a single trailing rerun rather than
// dropped (so a refreshed push token is never ignored) or stacked (so a burst of triggers is one
// extra sync, not many).
type manager struct {
	deps  syncDeps
	hooks syncHooks
	sem   chan struct{}

	mu       sync.Mutex
	inflight map[string]struct{}
	rerun    map[string]bool
	// idleCh is closed whenever no sync is in flight and replaced when one starts, so drain can wait
	// for idle without a WaitGroup (whose Add-from-zero must not race a Wait).
	idleCh chan struct{}
}

func newManager(limit int, deps syncDeps, hooks syncHooks) *manager {
	if limit < 1 {
		limit = 1
	}
	idle := make(chan struct{})
	close(idle)
	return &manager{
		deps:     deps,
		hooks:    hooks,
		sem:      make(chan struct{}, limit),
		inflight: map[string]struct{}{},
		rerun:    map[string]bool{},
		idleCh:   idle,
	}
}

// enqueue starts a sync for each repository that is syncable and not already in flight, and marks a
// trailing rerun for each that is. ctx bounds the syncs it starts: it is the process-lifetime
// context, not a connection's, so a reconnect does not kill a long fetch.
func (m *manager) enqueue(ctx context.Context, repoIDs []string) {
	for _, id := range repoIDs {
		if _, ok := m.hooks.resolve(id); !ok {
			continue
		}
		m.launch(ctx, id, true)
	}
}

// enqueueScheduled starts a periodic sync of one repository and reports whether it did. Unlike
// enqueue it never marks a trailing rerun: a repository that is already syncing (queued or running)
// is simply skipped, because that run already covers what the schedule wants, and a rerun would make
// a repository slower than its interval sync back to back. It also declines when syncing is
// suspended, ctx is done, or the repository is no longer syncable.
func (m *manager) enqueueScheduled(ctx context.Context, id string) bool {
	if ctx.Err() != nil || m.hooks.suspended() || m.busy(id) {
		return false
	}
	if _, ok := m.hooks.resolve(id); !ok {
		return false
	}
	return m.launch(ctx, id, false)
}

// launch marks id in flight and starts its sync, or reports false when it already is. rerunIfBusy
// then records a trailing rerun instead of dropping the request.
func (m *manager) launch(ctx context.Context, id string, rerunIfBusy bool) bool {
	m.mu.Lock()
	if _, running := m.inflight[id]; running {
		if rerunIfBusy {
			m.rerun[id] = true
		}
		m.mu.Unlock()
		return false
	}
	if len(m.inflight) == 0 {
		m.idleCh = make(chan struct{})
	}
	m.inflight[id] = struct{}{}
	m.mu.Unlock()

	m.hooks.begin(id)
	go m.runRepo(ctx, id)
	return true
}

// busy reports whether id is queued or running.
func (m *manager) busy(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, running := m.inflight[id]
	return running
}

// runRepo runs a repository's sync, then its trailing rerun if one was requested meanwhile.
func (m *manager) runRepo(ctx context.Context, id string) {
	for {
		m.runOnce(ctx, id)

		m.mu.Lock()
		again := m.rerun[id] && ctx.Err() == nil
		delete(m.rerun, id)
		if !again {
			m.removeInflightLocked(id)
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		m.hooks.begin(id)
	}
}

// runOnce waits for a concurrency slot, then resolves the task and runs one sync. Cancellation
// while waiting, like cancellation during the sync, is not a failure: nothing is recorded.
func (m *manager) runOnce(ctx context.Context, id string) {
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		m.hooks.release(id)
		return
	}
	defer func() { <-m.sem }()

	if ctx.Err() != nil {
		m.hooks.release(id)
		return
	}
	task, ok := m.hooks.resolve(id)
	if !ok || m.hooks.suspended() {
		m.hooks.release(id)
		m.hooks.changed()
		return
	}
	// The attempt is timed with the injected clock (which carries the monotonic reading in
	// production), so its start and duration are what the status reports.
	start := m.deps.now()
	if m.hooks.started != nil {
		m.hooks.started(id, task, start)
	}
	res := syncRepo(ctx, m.deps, task)
	res.Stats.StartedAt = start
	res.Stats.Duration = m.deps.now().Sub(start)
	m.hooks.finish(id, task, res)
	if res.State != StatePending {
		logOutcome(id, res, res.Stats.Duration)
		m.hooks.changed()
	}
}

// logOutcome writes one line per finished sync. The portal shows the same outcome, but an operator
// debugging on the machine (the CLI `status` defers to the portal) has nothing else. It carries only
// the repo_id (restricted to a safe charset), the typed code, the sanitised detail and the duration:
// never a URL, host, path or credential.
func logOutcome(repoID string, res syncResult, took time.Duration) {
	took = took.Round(time.Millisecond)
	if res.State == StateError {
		slog.Warn("sync failed", "repo_id", repoID, "error_type", res.ErrorType, "detail", res.ErrorDetail, "duration", took)
		return
	}
	slog.Info("sync complete", "repo_id", repoID, "duration", took)
}

// removeInflightLocked drops id from the in-flight set and, when that leaves none, signals idle.
// m.mu must be held.
func (m *manager) removeInflightLocked(id string) {
	delete(m.inflight, id)
	if len(m.inflight) == 0 {
		close(m.idleCh)
	}
}

// idle reports whether no sync is in flight.
func (m *manager) idle() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.inflight) == 0
}

// drain waits until no sync is in flight or ctx ends, whichever is first. It never cancels or
// abandons a sync: one that outlives the wait simply keeps running.
func (m *manager) drain(ctx context.Context) {
	m.mu.Lock()
	idle := m.idleCh
	m.mu.Unlock()
	select {
	case <-idle:
	case <-ctx.Done():
	}
}
