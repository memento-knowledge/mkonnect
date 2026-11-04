package gitsync

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ticketGit is a fake git whose fetches each wait for a ticket, so a test decides exactly which
// in-flight sync finishes next.
type ticketGit struct {
	tickets chan struct{}
	started atomic.Int32
	running atomic.Int32
	maxRun  atomic.Int32

	mu   sync.Mutex
	envs []string
}

func newTicketGit() *ticketGit { return &ticketGit{tickets: make(chan struct{}, 64)} }

// finishOne lets one in-flight fetch complete.
func (g *ticketGit) finishOne() { g.tickets <- struct{}{} }

func (g *ticketGit) run(ctx context.Context, argv, env []string, _ string) ([]byte, error) {
	if (gitCall{argv: argv}).sub() != "fetch" {
		return nil, nil
	}
	g.mu.Lock()
	g.envs = append(g.envs, strings.Join(env, "\n"))
	g.mu.Unlock()
	g.started.Add(1)
	n := g.running.Add(1)
	for {
		m := g.maxRun.Load()
		if n <= m || g.maxRun.CompareAndSwap(m, n) {
			break
		}
	}
	defer g.running.Add(-1)
	select {
	case <-g.tickets:
		return nil, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("signal: killed")
	}
}

func newTicketManager(t *testing.T, limit int, g *ticketGit, h *hookLog) *manager {
	t.Helper()
	deps := syncDeps{mirrors: Mirrors{Root: t.TempDir()}, run: g.run, now: func() time.Time { return syncNow }}
	m := newManager(limit, deps, h.hooks())
	t.Cleanup(func() {
		for range 64 {
			g.finishOne()
		}
		ctx, cancel := context.WithTimeout(context.Background(), syncStopTimeout)
		defer cancel()
		m.drain(ctx)
		if ctx.Err() != nil {
			t.Errorf("a sync was still running %v after the test ended", syncStopTimeout)
		}
	})
	return m
}

// steady asserts the count stays at want for a moment, so "nothing else started" is a claim about a
// quiet system rather than a race.
func steady(t *testing.T, what string, got func() int32, want int32) {
	t.Helper()
	time.Sleep(40 * time.Millisecond)
	if n := got(); n != want {
		t.Fatalf("%s = %d, want %d", what, n, want)
	}
}

func TestManagerResizeUpStartsQueuedSyncsWithoutAnyFinishing(t *testing.T) {
	g, h := newTicketGit(), newHookLog()
	m := newTicketManager(t, 1, g, h)

	m.enqueue(context.Background(), []string{"a"})
	eventually(t, func() bool { return g.started.Load() == 1 })
	m.enqueue(context.Background(), []string{"b", "c"})
	steady(t, "running syncs at limit 1", g.running.Load, 1)

	m.resize(3)
	eventually(t, func() bool { return g.running.Load() == 3 })
	if len(h.results("a"))+len(h.results("b"))+len(h.results("c")) != 0 {
		t.Error("growing the limit must not need, or cause, any sync to finish")
	}
}

func TestManagerResizeDownNeitherDropsNorStartsAnythingUntilBelowTheLimit(t *testing.T) {
	g, h := newTicketGit(), newHookLog()
	m := newTicketManager(t, 3, g, h)

	m.enqueue(context.Background(), []string{"a", "b", "c"})
	eventually(t, func() bool { return g.running.Load() == 3 })
	m.resize(1)
	m.enqueue(context.Background(), []string{"d", "e"})
	steady(t, "running syncs right after shrinking", g.running.Load, 3)
	if n := g.started.Load(); n != 3 {
		t.Fatalf("started = %d, want 3: queued repositories must wait for the smaller limit", n)
	}

	g.finishOne() // 2 running
	steady(t, "started after one finished (2 running, limit 1)", g.started.Load, 3)
	g.finishOne() // 1 running
	steady(t, "started after two finished (1 running, limit 1)", g.started.Load, 3)
	g.finishOne() // 0 running: d may start now
	eventually(t, func() bool { return g.started.Load() == 4 })
	g.finishOne()
	eventually(t, func() bool { return g.started.Load() == 5 })
	g.finishOne()
	eventually(t, func() bool { return m.idle() })

	if max := g.maxRun.Load(); max > 3 {
		t.Errorf("max concurrent syncs = %d, want <= 3", max)
	}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if n := len(h.results(id)); n != 1 {
			t.Errorf("%s finished %d times, want exactly once (nothing dropped)", id, n)
		}
	}
	// d and e only ever ran one at a time.
	if len(g.envs) != 5 {
		t.Errorf("fetches = %d, want 5", len(g.envs))
	}
}

// The task is resolved when a slot is won, not when the repository queued; resizing must not
// resolve early, or a refreshed token would be lost.
func TestManagerResizeKeepsSlotThenResolveOrdering(t *testing.T) {
	g, h := newTicketGit(), newHookLog()
	h.setToken("b", "token-when-queued-abcde")
	m := newTicketManager(t, 1, g, h)

	m.enqueue(context.Background(), []string{"a"})
	eventually(t, func() bool { return g.started.Load() == 1 })
	m.enqueue(context.Background(), []string{"b"})
	h.setToken("b", "token-when-it-runs-xyz1")
	m.resize(2) // b is granted a slot only now

	eventually(t, func() bool { return g.started.Load() == 2 })
	g.mu.Lock()
	bEnv := g.envs[1]
	g.mu.Unlock()
	if !strings.Contains(bEnv, "token-when-it-runs-xyz1") || strings.Contains(bEnv, "token-when-queued-abcde") {
		t.Error("a sync started by a resize ran with the material it had when queued, not when it got its slot")
	}
}

// A burst of triggers for a repository that is busy is one trailing rerun, however the limit moves
// meanwhile, and the rerun uses what is current when it starts.
func TestManagerResizeKeepsTrailingRerunCoalescing(t *testing.T) {
	g, h := newTicketGit(), newHookLog()
	h.setToken("a", "token-before-refresh-abc")
	m := newTicketManager(t, 1, g, h)

	m.enqueue(context.Background(), []string{"a"})
	eventually(t, func() bool { return g.started.Load() == 1 })
	m.resize(2)
	h.setToken("a", "token-after-refresh-xyz")
	for range 5 {
		m.enqueue(context.Background(), []string{"a"})
	}
	m.resize(1)
	m.resize(3)

	g.finishOne()
	eventually(t, func() bool { return g.started.Load() == 2 })
	g.finishOne()
	eventually(t, func() bool { return m.idle() })

	if n := len(h.results("a")); n != 2 {
		t.Errorf("results = %d, want 2: the first run plus exactly one coalesced rerun", n)
	}
	if n := g.started.Load(); n != 2 {
		t.Errorf("fetches = %d, want 2", n)
	}
	if !strings.Contains(g.envs[1], "token-after-refresh-xyz") {
		t.Error("the rerun did not use the refreshed token")
	}
}

// Shutdown while a sync waits for a slot in a resized pool releases it cleanly and leaks nothing.
func TestManagerResizeThenCancelWhileQueuedReleasesTheRepository(t *testing.T) {
	g, h := newTicketGit(), newHookLog()
	m := newTicketManager(t, 1, g, h)
	ctx, cancel := context.WithCancel(context.Background())

	m.enqueue(ctx, []string{"a"})
	eventually(t, func() bool { return g.started.Load() == 1 })
	m.enqueue(ctx, []string{"b"}) // queued behind a
	m.resize(1)
	cancel()
	eventually(t, func() bool { return m.idle() })

	if got := h.releasedIDs(); len(got) != 1 || got[0] != "b" {
		t.Errorf("released = %v, want only the queued b", got)
	}
	// The slot a cancelled sync held is free again.
	m.enqueue(context.Background(), []string{"c"})
	eventually(t, func() bool { return g.started.Load() == 2 })
}

// An accepted configuration resizes the running manager, a later one that omits max_concurrency puts
// it back to the default, and a rejected one leaves it alone.
func TestClientConfigResizesTheManager(t *testing.T) {
	e := newSchedEnv(t, nil)
	e.holdSyncs() // released by the environment's cleanup
	send := func(limits string) {
		repos := make([]string, 6)
		for i := range repos {
			id := fmt.Sprintf("repo-%d", i)
			repos[i] = fmt.Sprintf(`{"repo_id":%q,"origin_url":"https://git.example.com/group/%s.git","replica_push_url":"https://replica.example.com/r/%s.git","push_token":"push-token-abcdefghij"}`, id, id, id)
		}
		raw := `{"type":"git_sync_config","config_version":1,"repositories":[` + strings.Join(repos, ",") + `]` + limits + `}`
		e.c.HandleMessage(e.ctx, schedStubSender{}, "git_sync_config", []byte(raw))
	}

	send(`,"limits":{"max_concurrency":1}`)
	eventually(t, func() bool { return e.fetches.Load() == 1 })
	steady(t, "fetches at max_concurrency 1", e.fetches.Load, 1)

	send(`,"limits":{"max_concurrency":3}`)
	eventually(t, func() bool { return e.fetches.Load() == 3 })
	steady(t, "fetches at max_concurrency 3", e.fetches.Load, 3)

	// Out of range: rejected as a whole, so the limit in force does not move.
	send(`,"limits":{"max_concurrency":99}`)
	steady(t, "fetches after a rejected config", e.fetches.Load, 3)

	// Omitting the limit reverts to the default of 4.
	send(``)
	eventually(t, func() bool { return e.fetches.Load() == 4 })
	steady(t, "fetches at the default limit", e.fetches.Load, 4)
}
