package gitsync

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hookLog is a thread-safe recorder of what the manager reports through its hooks, and the source of
// the task a repository resolves to (which a test can change while work is queued).
type hookLog struct {
	mu        sync.Mutex
	begun     []string
	released  []string
	finished  map[string][]syncResult
	tokens    map[string]string // origin token each repo currently resolves to
	skip      map[string]bool   // repos resolve reports as no longer syncable
	changed   atomic.Int32
	suspended atomic.Bool
}

func newHookLog() *hookLog {
	return &hookLog{finished: map[string][]syncResult{}, tokens: map[string]string{}, skip: map[string]bool{}}
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
			if tok, ok := h.tokens[id]; ok {
				t.Origin.Token = Secret(tok)
			}
			return t, true
		},
		begin: func(id string) {
			h.mu.Lock()
			h.begun = append(h.begun, id)
			h.mu.Unlock()
		},
		finish: func(id string, _ syncTask, r syncResult) {
			h.mu.Lock()
			h.finished[id] = append(h.finished[id], r)
			h.mu.Unlock()
		},
		release: func(id string) {
			h.mu.Lock()
			h.released = append(h.released, id)
			h.mu.Unlock()
		},
		suspended: func() bool { return h.suspended.Load() },
		changed:   func() { h.changed.Add(1) },
	}
}

func (h *hookLog) setToken(id, tok string) {
	h.mu.Lock()
	h.tokens[id] = tok
	h.mu.Unlock()
}

func (h *hookLog) setSkip(id string, skip bool) {
	h.mu.Lock()
	h.skip[id] = skip
	h.mu.Unlock()
}

func (h *hookLog) results(id string) []syncResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]syncResult(nil), h.finished[id]...)
}

func (h *hookLog) releasedIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.released)
}

func (h *hookLog) begunIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.begun)
}

// gatedGit is a thread-safe fake git whose first-run fetch blocks until released, so a test can hold a sync
// in flight.
type gatedGit struct {
	gate    chan struct{}
	fetches atomic.Int32
	running atomic.Int32
	maxRun  atomic.Int32
	fail    []byte // when set, the fetch fails with this stderr

	mu        sync.Mutex
	fetchEnvs []string // each fetch's environment, joined
}

func newGatedGit() *gatedGit { return &gatedGit{gate: make(chan struct{})} }

func (g *gatedGit) release() { close(g.gate) }

func (g *gatedGit) envs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.fetchEnvs)
}

func (g *gatedGit) run(ctx context.Context, argv, env []string, _ string) ([]byte, error) {
	if (gitCall{argv: argv}).sub() != "fetch" {
		return nil, nil
	}
	g.mu.Lock()
	g.fetchEnvs = append(g.fetchEnvs, strings.Join(env, "\n"))
	g.mu.Unlock()
	g.fetches.Add(1)
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

// syncBuffer is a goroutine-safe log sink (syncs from earlier tests may still be logging).
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// The CLI `status` defers to the portal, so without a local log an operator chasing a failed sync has
// nothing on the machine. Each finished sync logs its repo_id, outcome and duration — never a URL,
// host, or credential.
func TestManagerLogsEachSyncOutcome(t *testing.T) {
	logs := captureLogs(t)

	bad, h := newGatedGit(), newHookLog()
	bad.fail = []byte("fatal: unable to access 'https://git.example.com/group/repo-a/': The requested URL returned error: 403")
	bad.release()
	m := newTestManager(t, 2, bad, h)
	m.enqueue(context.Background(), []string{"repo-a"})
	eventually(t, func() bool { return len(h.results("repo-a")) == 1 })

	good, h2 := newGatedGit(), newHookLog()
	good.release()
	m2 := newTestManager(t, 2, good, h2)
	m2.enqueue(context.Background(), []string{"repo-b"})
	eventually(t, func() bool { return len(h2.results("repo-b")) == 1 })

	out := logs.String()
	for _, want := range []string{"repo_id=repo-a", "origin_access_denied", "level=WARN", "repo_id=repo-b", "sync complete", "duration="} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{testOriginTok, testPushTok, "git.example.com", "replica.example.com", "https://"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks %q:\n%s", leak, out)
		}
	}
}

func TestManagerEnqueueRunsSyncAndRecordsResult(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	g.release() // never blocks
	m := newTestManager(t, 2, g, h)

	m.enqueue(context.Background(), []string{"repo-a"})

	// begin ran synchronously, before enqueue returned, so a status built right after sees "syncing".
	if got := h.begunIDs(); !slices.Equal(got, []string{"repo-a"}) {
		t.Fatalf("begin hooks = %v, want [repo-a] before enqueue returns", got)
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
	eventually(t, func() bool { return g.fetches.Load() == 1 })
	m.enqueue(context.Background(), []string{"repo-a"})
	m.enqueue(context.Background(), []string{"repo-a"})
	m.enqueue(context.Background(), []string{"repo-a"})

	time.Sleep(30 * time.Millisecond)
	if n := g.fetches.Load(); n != 1 {
		t.Fatalf("fetches while the first sync is in flight = %d, want 1", n)
	}
	if max := g.maxRun.Load(); max != 1 {
		t.Errorf("max concurrent syncs of one repo = %d, want 1", max)
	}

	g.release()
	eventually(t, func() bool { return m.idle() })
	if n := g.fetches.Load(); n != 2 {
		t.Errorf("total fetches = %d, want exactly 2 (the sync plus ONE trailing rerun for three triggers)", n)
	}
	if got := len(h.results("repo-a")); got != 2 {
		t.Errorf("recorded results = %d, want 2", got)
	}
}

// The rerun exists so that a refreshed push token or changed target is not lost: it must resolve
// its task afresh, not reuse the first sync's snapshot.
func TestManagerTrailingRerunUsesRefreshedMaterial(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	h.setToken("repo-a", "token-before-refresh-abc")
	m := newTestManager(t, 4, g, h)

	m.enqueue(context.Background(), []string{"repo-a"})
	eventually(t, func() bool { return g.fetches.Load() == 1 })
	h.setToken("repo-a", "token-after-refresh-xyz")
	m.enqueue(context.Background(), []string{"repo-a"})

	g.release()
	eventually(t, func() bool { return m.idle() })
	envs := g.envs()
	if len(envs) != 2 {
		t.Fatalf("fetches = %d, want 2", len(envs))
	}
	if !strings.Contains(envs[0], "token-before-refresh-abc") || strings.Contains(envs[0], "token-after-refresh-xyz") {
		t.Error("the first sync must use the material it started with")
	}
	if !strings.Contains(envs[1], "token-after-refresh-xyz") || strings.Contains(envs[1], "token-before-refresh-abc") {
		t.Error("the trailing rerun must use the refreshed token, not the first sync's snapshot")
	}
}

// A repository can wait a long time behind the concurrency limit; whatever the platform changed
// meanwhile (a refreshed token, a moved replica) must be what it runs with, not what it was when
// it was queued.
func TestManagerQueuedSyncUsesMaterialResolvedWhenItRuns(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	h.setToken("b", "token-when-queued-abcde")
	m := newTestManager(t, 1, g, h)

	m.enqueue(context.Background(), []string{"a"}) // "a" takes the only slot...
	eventually(t, func() bool { return g.fetches.Load() == 1 })
	m.enqueue(context.Background(), []string{"b"}) // ...so "b" is queued behind it
	h.setToken("b", "token-when-it-runs-xyz1")

	g.release()
	eventually(t, func() bool { return m.idle() })
	var bEnv string
	for _, e := range g.envs() {
		if strings.Contains(e, "token-when-") {
			bEnv = e
		}
	}
	if !strings.Contains(bEnv, "token-when-it-runs-xyz1") || strings.Contains(bEnv, "token-when-queued-abcde") {
		t.Error("a queued sync ran with the material it had when it was queued, not when it ran")
	}
}

func TestManagerQueuedSyncThatBecomesUnsyncableIsNotRun(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	m := newTestManager(t, 1, g, h)

	m.enqueue(context.Background(), []string{"a"})
	eventually(t, func() bool { return g.fetches.Load() == 1 })
	m.enqueue(context.Background(), []string{"b"}) // queued behind "a"
	h.setSkip("b", true)                           // the platform removed it while it waited

	g.release()
	eventually(t, func() bool { return m.idle() })
	if n := g.fetches.Load(); n != 1 {
		t.Errorf("fetches = %d, want only \"a\": a repository withdrawn while queued must not be synced", n)
	}
	if !slices.Contains(h.releasedIDs(), "b") {
		t.Errorf("released = %v, want b's syncing mark cleared", h.releasedIDs())
	}
}

// While a critical security update is required nothing may start: not a new trigger (the client
// checks that), and not work that was queued, or a trailing rerun, before the gate closed.
func TestManagerSuspendedStopsQueuedWorkAndReruns(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	m := newTestManager(t, 1, g, h)

	m.enqueue(context.Background(), []string{"a"})
	eventually(t, func() bool { return g.fetches.Load() == 1 })
	m.enqueue(context.Background(), []string{"b"}) // queued behind "a"
	m.enqueue(context.Background(), []string{"a"}) // a trailing rerun of "a"
	h.suspended.Store(true)

	g.release()
	eventually(t, func() bool { return m.idle() })
	if n := g.fetches.Load(); n != 1 {
		t.Errorf("fetches = %d, want 1: the queued repo and the rerun must not start while suspended", n)
	}
	for _, id := range []string{"a", "b"} {
		if !slices.Contains(h.releasedIDs(), id) && len(h.results(id)) == 0 {
			t.Errorf("%s was left marked syncing", id)
		}
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
	h.setSkip("gone", true)
	m := newTestManager(t, 2, g, h)

	m.enqueue(context.Background(), []string{"gone"})
	time.Sleep(20 * time.Millisecond)
	if g.fetches.Load() != 0 || len(h.begunIDs()) != 0 || !m.idle() {
		t.Errorf("a repo that is not syncable must not start: fetches=%d begun=%v", g.fetches.Load(), h.begunIDs())
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
	eventually(t, func() bool { return g.fetches.Load() == 1 })

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
	eventually(t, func() bool { return g.fetches.Load() == 1 })

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
		for _, r := range h.results(id) {
			if r.State != StatePending || r.ErrorType != "" {
				t.Errorf("%s recorded %+v, want no failure (shutdown is not a failure)", id, r)
			}
		}
		if len(h.results(id)) == 0 && !slices.Contains(h.releasedIDs(), id) {
			t.Errorf("%s was left marked syncing after cancellation", id)
		}
	}
	if g.fetches.Load() != 1 {
		t.Errorf("fetches = %d, want only the one that held the slot", g.fetches.Load())
	}
}
