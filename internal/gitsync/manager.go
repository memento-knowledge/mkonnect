package gitsync

import (
	"context"
	"sync"
)

// defaultSyncConcurrency is how many repositories sync at once. Each sync is a git subprocess that
// may be memory- and bandwidth-heavy, so the default is deliberately small. (Making it
// configurable per deployment is a Phase 3b item.)
const defaultSyncConcurrency = 4

// syncHooks connect the manager to the connector's state without the manager knowing about it.
type syncHooks struct {
	// resolve returns a snapshot of the task to run for repoID, or false when the repository is not
	// (or no longer) syncable. It is called again before every rerun so a refreshed token or changed
	// target is picked up.
	resolve func(repoID string) (syncTask, bool)
	// begin marks the repository as syncing. It runs synchronously inside enqueue, so a status built
	// right after enqueue returns already reports it.
	begin func(repoID string, t syncTask)
	// finish records the outcome and clears the syncing mark. A pending result means the sync was
	// cancelled and nothing should be recorded. The implementation must ignore a task whose target
	// no longer matches the repository's current configuration.
	finish func(repoID string, t syncTask, r syncResult)
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
// context, not a connection's, so a reconnect does not kill a long clone.
func (m *manager) enqueue(ctx context.Context, repoIDs []string) {
	for _, id := range repoIDs {
		task, ok := m.hooks.resolve(id)
		if !ok {
			continue
		}
		m.mu.Lock()
		if _, running := m.inflight[id]; running {
			m.rerun[id] = true
			m.mu.Unlock()
			continue
		}
		if len(m.inflight) == 0 {
			m.idleCh = make(chan struct{})
		}
		m.inflight[id] = struct{}{}
		m.mu.Unlock()

		m.hooks.begin(id, task)
		go m.runRepo(ctx, id, task)
	}
}

// runRepo runs a repository's sync, then its trailing rerun if one was requested meanwhile.
func (m *manager) runRepo(ctx context.Context, id string, task syncTask) {
	for {
		res := m.runOne(ctx, task)
		m.hooks.finish(id, task, res)
		if res.State != StatePending {
			m.hooks.changed()
		}

		m.mu.Lock()
		again := m.rerun[id] && ctx.Err() == nil
		delete(m.rerun, id)
		if !again {
			m.removeInflightLocked(id)
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()

		next, ok := m.hooks.resolve(id)
		if !ok {
			m.mu.Lock()
			delete(m.rerun, id)
			m.removeInflightLocked(id)
			m.mu.Unlock()
			return
		}
		m.hooks.begin(id, next)
		task = next
	}
}

// runOne waits for a concurrency slot and runs one sync. Cancellation while waiting, like
// cancellation during the sync, is reported as pending rather than as a failure.
func (m *manager) runOne(ctx context.Context, task syncTask) syncResult {
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return syncResult{State: StatePending}
	}
	defer func() { <-m.sem }()
	return syncRepo(ctx, m.deps, task)
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
