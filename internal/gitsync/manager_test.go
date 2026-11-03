package gitsync

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hookLog is a thread-safe recorder of what the manager reports through its hooks.
type hookLog struct {
	mu       sync.Mutex
	begun    []string
	finished map[string][]syncResult
	changed  atomic.Int32
	skip     map[string]bool // repos resolve reports as no longer syncable
}

func newHookLog() *hookLog {
	return &hookLog{finished: map[string][]syncResult{}, skip: map[string]bool{}}
}

func (h *hookLog) hooks() syncHooks {
	return syncHooks{
		resolve: func(id string) (syncTask, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.skip[id] {
				return syncTask{}, false
			}
			t := testTask()
			t.RepoID = id
			return t, true
		},
		begin: func(id string, _ syncTask) {
			h.mu.Lock()
			h.begun = append(h.begun, id)
			h.mu.Unlock()
		},
		finish: func(id string, _ syncTask, r syncResult) {
			h.mu.Lock()
			h.finished[id] = append(h.finished[id], r)
			h.mu.Unlock()
		},
		changed: func() { h.changed.Add(1) },
	}
}

func (h *hookLog) results(id string) []syncResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]syncResult(nil), h.finished[id]...)
}

// gatedGit is a thread-safe fake git whose clone blocks until released, so a test can hold a sync
// in flight.
type gatedGit struct {
	gate    chan struct{}
	clones  atomic.Int32
	running atomic.Int32
	maxRun  atomic.Int32
	fail    []byte // when set, clone fails with this stderr
}

func newGatedGit() *gatedGit { return &gatedGit{gate: make(chan struct{})} }

func (g *gatedGit) release() { close(g.gate) }

func (g *gatedGit) run(ctx context.Context, argv, _ []string, _ string) ([]byte, error) {
	if (gitCall{argv: argv}).sub() != "clone" {
		return nil, nil
	}
	g.clones.Add(1)
	n := g.running.Add(1)
	for {
		m := g.maxRun.Load()
		if n <= m || g.maxRun.CompareAndSwap(m, n) {
			break
		}
	}
	defer g.running.Add(-1)
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, errors.New("signal: killed")
	}
	if g.fail != nil {
		return g.fail, errExit
	}
	return nil, nil
}

func newTestManager(t *testing.T, limit int, g *gatedGit, h *hookLog) *manager {
	t.Helper()
	deps := syncDeps{mirrors: Mirrors{Root: t.TempDir()}, run: g.run, now: func() time.Time { return syncNow }}
	return newManager(limit, deps, h.hooks())
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestManagerEnqueueRunsSyncAndRecordsResult(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	g.release() // never blocks
	m := newTestManager(t, 2, g, h)

	m.enqueue(context.Background(), []string{"repo-a"})

	// begin ran synchronously, before enqueue returned, so a status built right after sees "syncing".
	h.mu.Lock()
	begun := slices.Clone(h.begun)
	h.mu.Unlock()
	if !slices.Equal(begun, []string{"repo-a"}) {
		t.Fatalf("begin hooks = %v, want [repo-a] before enqueue returns", begun)
	}
	eventually(t, func() bool { return len(h.results("repo-a")) == 1 })
	if got := h.results("repo-a")[0]; got.State != StateOK {
		t.Errorf("recorded result = %+v, want ok", got)
	}
	eventually(t, func() bool { return h.changed.Load() == 1 && m.idle() })
}

// A trigger that arrives while a repository is syncing must not start a second git: it is folded
// into exactly one trailing rerun, which picks up whatever changed (for instance a refreshed token).
func TestManagerCoalescesConcurrentEnqueueForSameRepo(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	m := newTestManager(t, 4, g, h)

	m.enqueue(context.Background(), []string{"repo-a"})
	eventually(t, func() bool { return g.clones.Load() == 1 })
	m.enqueue(context.Background(), []string{"repo-a"})
	m.enqueue(context.Background(), []string{"repo-a"})
	m.enqueue(context.Background(), []string{"repo-a"})

	time.Sleep(30 * time.Millisecond)
	if n := g.clones.Load(); n != 1 {
		t.Fatalf("clones while the first sync is in flight = %d, want 1", n)
	}
	if max := g.maxRun.Load(); max != 1 {
		t.Errorf("max concurrent syncs of one repo = %d, want 1", max)
	}

	g.release()
	eventually(t, func() bool { return m.idle() })
	if n := g.clones.Load(); n != 2 {
		t.Errorf("total clones = %d, want exactly 2 (the sync plus ONE trailing rerun for three triggers)", n)
	}
	if got := len(h.results("repo-a")); got != 2 {
		t.Errorf("recorded results = %d, want 2", got)
	}
}

func TestManagerBoundsConcurrency(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	m := newTestManager(t, 2, g, h)

	m.enqueue(context.Background(), []string{"a", "b", "c", "d", "e"})
	eventually(t, func() bool { return g.running.Load() == 2 })
	time.Sleep(30 * time.Millisecond)
	if n := g.running.Load(); n != 2 {
		t.Fatalf("running syncs = %d, want the limit of 2", n)
	}

	g.release()
	eventually(t, func() bool { return m.idle() })
	if max := g.maxRun.Load(); max > 2 {
		t.Errorf("max concurrent syncs = %d, want <= 2", max)
	}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if len(h.results(id)) != 1 {
			t.Errorf("%s results = %v, want exactly one", id, h.results(id))
		}
	}
}

func TestManagerSkipsRepoThatIsNoLongerSyncable(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	g.release()
	h.skip["gone"] = true
	m := newTestManager(t, 2, g, h)

	m.enqueue(context.Background(), []string{"gone"})
	time.Sleep(20 * time.Millisecond)
	if g.clones.Load() != 0 || len(h.begun) != 0 || !m.idle() {
		t.Errorf("a repo that is not syncable must not start: clones=%d begun=%v", g.clones.Load(), h.begun)
	}
}

func TestManagerRecordsTypedFailure(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	g.fail = []byte("fatal: unable to access 'https://git.example.com/group/repo-a/': The requested URL returned error: 403")
	g.release()
	m := newTestManager(t, 2, g, h)

	m.enqueue(context.Background(), []string{"repo-a"})
	eventually(t, func() bool { return len(h.results("repo-a")) == 1 })
	if got := h.results("repo-a")[0]; got.State != StateError || got.ErrorType != ErrOriginAccessDenied {
		t.Errorf("recorded result = %+v, want error/origin_access_denied", got)
	}
}

func TestManagerDrainWaitsThenReturnsAtItsBound(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	m := newTestManager(t, 2, g, h)
	m.enqueue(context.Background(), []string{"repo-a"})
	eventually(t, func() bool { return g.clones.Load() == 1 })

	dctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	m.drain(dctx)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("drain returned after %v, want it to wait for its ~60ms bound and then return", elapsed)
	}
	if m.idle() {
		t.Error("drain must not cancel or abandon the in-flight sync")
	}

	g.release()
	eventually(t, func() bool { return m.idle() })
	if got := h.results("repo-a"); len(got) != 1 || got[0].State != StateOK {
		t.Errorf("the sync that outlived the drain bound should still finish ok, got %v", got)
	}
}

func TestManagerDrainReturnsPromptlyWhenSyncsFinish(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	m := newTestManager(t, 2, g, h)
	m.enqueue(context.Background(), []string{"repo-a"})
	eventually(t, func() bool { return g.clones.Load() == 1 })

	go func() { time.Sleep(30 * time.Millisecond); g.release() }()
	dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	m.drain(dctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("drain took %v although the sync finished after ~30ms", elapsed)
	}
	if !m.idle() {
		t.Error("drain returned before the in-flight sync finished")
	}
}

func TestManagerCancelledContextLeavesReposPending(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	m := newTestManager(t, 1, g, h)
	ctx, cancel := context.WithCancel(context.Background())

	// "a" holds the only slot; "b" is queued behind it.
	m.enqueue(ctx, []string{"a", "b"})
	eventually(t, func() bool { return g.running.Load() == 1 })
	cancel()

	eventually(t, func() bool { return m.idle() })
	for _, id := range []string{"a", "b"} {
		got := h.results(id)
		if len(got) != 1 || got[0].State != StatePending || got[0].ErrorType != "" {
			t.Errorf("%s results = %v, want a single pending result (shutdown is not a failure)", id, got)
		}
	}
	if g.clones.Load() != 1 {
		t.Errorf("clones = %d, want only the one that held the slot", g.clones.Load())
	}
}
