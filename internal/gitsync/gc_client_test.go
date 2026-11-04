package gitsync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
)

// gcClock is a settable clock for the client under test.
type gcClock struct{ ns atomic.Int64 }

func newGCClock(t time.Time) *gcClock {
	c := &gcClock{}
	c.set(t)
	return c
}
func (c *gcClock) set(t time.Time)     { c.ns.Store(t.UnixNano()) }
func (c *gcClock) now() time.Time      { return time.Unix(0, c.ns.Load()).UTC() }
func (c *gcClock) add(d time.Duration) { c.set(c.now().Add(d)) }

type gcSender struct{}

func (gcSender) Send(context.Context, any) error { return nil }

// newGCClient builds a client with no local providers (so no repository is host-matched and no sync
// ever starts) whose mirrors root is h.root. The gateway address refuses connections immediately.
func newGCClient(t *testing.T, h *gcHarness, clock *gcClock) *Client {
	t.Helper()
	cfg := &config.Config{GatewayURL: "ws://127.0.0.1:1", ConnectorID: "gc-test", ProtocolVersion: "v1"}
	c := NewClient(cfg, auth.NewKeyStore(filepath.Join(t.TempDir(), "key")), nil, nil, clock.now, "boot-test", WithMirrorsRoot(h.root))
	return c
}

func gcConfigMsg(ids ...string) json.RawMessage {
	repos := make([]string, 0, len(ids))
	for _, id := range ids {
		repos = append(repos, fmt.Sprintf(`{"repo_id":%q,"origin_url":"https://git.example.com/%s.git"}`, id, id))
	}
	return json.RawMessage(`{"type":"git_sync_config","config_version":1,"repositories":[` + strings.Join(repos, ",") + `]}`)
}

func (c *Client) handleConfig(raw json.RawMessage) {
	c.HandleMessage(context.Background(), gcSender{}, "git_sync_config", raw)
}

// overdueOrphan creates a mirror for repoID that has been marked orphaned for twice the grace period.
func (h *gcHarness) overdueOrphan(clock *gcClock, repoID string) string {
	h.t.Helper()
	name := h.mirror(repoID)
	h.mark(name, clock.now().Add(-2*mirrorGCGrace))
	return name
}

func gcEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestMirrorGCNeverSweepsWithoutAnAcceptedNonEmptyConfig(t *testing.T) {
	t.Run("no configuration accepted in this process", func(t *testing.T) {
		h := newGCHarness(t)
		clock := newGCClock(gcNow)
		c := newGCClient(t, h, clock)
		orphan := h.overdueOrphan(clock, "repo-b")
		fresh := h.mirror("repo-c")

		c.collectMirrors(context.Background())

		if !h.exists(orphan, "HEAD") || h.exists(fresh, orphanMarkerName) {
			t.Fatal("swept (removed or marked) before any configuration was accepted")
		}
	})
	t.Run("accepted configuration with zero repositories", func(t *testing.T) {
		h := newGCHarness(t)
		clock := newGCClock(gcNow)
		c := newGCClient(t, h, clock)
		c.handleConfig(gcConfigMsg())
		orphan := h.overdueOrphan(clock, "repo-b")
		fresh := h.mirror("repo-c")

		c.collectMirrors(context.Background())

		if !h.exists(orphan, "HEAD") || h.exists(fresh, orphanMarkerName) {
			t.Fatal("swept on an empty configuration")
		}
	})
	t.Run("empty configuration after a non-empty one", func(t *testing.T) {
		h := newGCHarness(t)
		clock := newGCClock(gcNow)
		c := newGCClient(t, h, clock)
		c.handleConfig(gcConfigMsg("repo-a"))
		kept := h.overdueOrphan(clock, "repo-a")
		c.handleConfig(gcConfigMsg())

		c.collectMirrors(context.Background())

		if !h.exists(kept, "HEAD") {
			t.Fatal("an empty configuration removed a mirror")
		}
	})
}

func TestMirrorGCPausesWhileTheLatestConfigurationIsRejected(t *testing.T) {
	h := newGCHarness(t)
	clock := newGCClock(gcNow)
	c := newGCClient(t, h, clock)
	c.handleConfig(gcConfigMsg("repo-a"))
	orphan := h.overdueOrphan(clock, "repo-b")

	c.handleConfig(json.RawMessage(`{"type":"git_sync_config","config_version":2,"repositories":[{"repo_id":"bad id"}]}`))
	c.collectMirrors(context.Background())
	if !h.exists(orphan, "HEAD") {
		t.Fatal("swept while the latest configuration was rejected")
	}

	c.handleConfig(gcConfigMsg("repo-a"))
	c.collectMirrors(context.Background())
	if h.exists(orphan) {
		t.Fatal("an overdue orphan was not removed once a valid configuration was accepted")
	}
}

func TestMirrorGCKeepsEveryConfiguredMirror(t *testing.T) {
	h := newGCHarness(t)
	clock := newGCClock(gcNow)
	c := newGCClient(t, h, clock)
	// No provider is configured locally, so these repositories are unmatched; their mirrors still stay.
	c.handleConfig(gcConfigMsg("repo-a", "repo-b"))
	a := h.overdueOrphan(clock, "repo-a") // a stale mark from an earlier absence
	b := h.mirror("repo-b")
	gone := h.overdueOrphan(clock, "repo-gone")
	if err := os.WriteFile(h.path("run-state.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	c.collectMirrors(context.Background())

	if !h.exists("run-state.json") {
		t.Fatal("run-state.json was removed")
	}
	if !h.exists(a, "HEAD") || !h.exists(b, "HEAD") {
		t.Fatal("a configured repository's mirror was removed")
	}
	if h.exists(a, orphanMarkerName) {
		t.Fatal("the stale orphan mark of a configured repository was not cleared")
	}
	if h.exists(gone) {
		t.Fatal("the overdue orphan was not removed")
	}
}

func TestMirrorGCSkipsWhileACriticalUpdateIsRequired(t *testing.T) {
	h := newGCHarness(t)
	clock := newGCClock(gcNow)
	c := newGCClient(t, h, clock)
	c.handleConfig(gcConfigMsg("repo-a"))
	orphan := h.overdueOrphan(clock, "repo-b")
	c.criticalPatchRequired.Store(true)

	c.collectMirrors(context.Background())

	if !h.exists(orphan, "HEAD") {
		t.Fatal("swept while a critical security update is required")
	}
}

func TestMirrorGCKillSwitch(t *testing.T) {
	h := newGCHarness(t)
	clock := newGCClock(gcNow)
	c := newGCClient(t, h, clock)
	c.handleConfig(gcConfigMsg("repo-a"))
	orphan := h.overdueOrphan(clock, "repo-b")
	t.Setenv("GIT_SYNC_MIRROR_GC", "off")

	c.collectMirrors(context.Background())

	if !h.exists(orphan, "HEAD") {
		t.Fatal("GIT_SYNC_MIRROR_GC=off did not disable mirror cleanup")
	}
}

func TestMirrorGCLeavesMirrorsWithAQueuedOrRunningSync(t *testing.T) {
	h := newGCHarness(t)
	clock := newGCClock(gcNow)
	c := newGCClient(t, h, clock)
	c.handleConfig(gcConfigMsg("repo-a"))
	busy := h.overdueOrphan(clock, "repo-busy")
	fresh := h.mirror("repo-busy-fresh")
	// repo-busy was dropped from the configuration while its sync was running.
	c.mgr.mu.Lock()
	c.mgr.inflight["repo-busy"] = struct{}{}
	c.mgr.inflight["repo-busy-fresh"] = struct{}{}
	c.mgr.mu.Unlock()

	c.collectMirrors(context.Background())

	if !h.exists(busy, "HEAD") {
		t.Fatal("removed a mirror whose sync is queued or running")
	}
	if h.exists(fresh, orphanMarkerName) {
		t.Fatal("marked a mirror whose sync is queued or running")
	}
}

func TestMirrorGCClaimDeclinesWhenTheRepositoryReturned(t *testing.T) {
	h := newGCHarness(t)
	clock := newGCClock(gcNow)
	c := newGCClient(t, h, clock)
	c.handleConfig(gcConfigMsg("repo-a"))
	name := gcMirrorName("repo-b")

	moved := false
	claimed, err := c.claimMirror(name, func() error { moved = true; return nil })
	if err != nil || !claimed || !moved {
		t.Fatalf("an orphan should be claimable: claimed=%v err=%v moved=%v", claimed, err, moved)
	}

	moved = false
	c.handleConfig(gcConfigMsg("repo-a", "repo-b")) // repo-b returned
	claimed, err = c.claimMirror(name, func() error { moved = true; return nil })
	if err != nil || claimed || moved {
		t.Fatalf("a repository back in the configuration must not be claimed: claimed=%v err=%v moved=%v", claimed, err, moved)
	}

	moved = false
	c.handleConfig(gcConfigMsg("repo-a"))
	c.mgr.mu.Lock()
	c.mgr.inflight["repo-b"] = struct{}{}
	c.mgr.mu.Unlock()
	claimed, _ = c.claimMirror(name, func() error { moved = true; return nil })
	if claimed || moved {
		t.Fatal("a mirror with a sync queued or running must not be claimed")
	}
}

// TestMirrorGCRepositoryReturningDuringRemovalGetsAFreshMirror pins the reason removal moves the mirror
// aside first: a repository that returns while a large mirror is still being deleted must be able to
// sync into a new directory, which the removal must not touch.
func TestMirrorGCRepositoryReturningDuringRemovalGetsAFreshMirror(t *testing.T) {
	h := newGCHarness(t)
	clock := newGCClock(gcNow)
	c := newGCClient(t, h, clock)
	c.handleConfig(gcConfigMsg("repo-a"))
	old := h.overdueOrphan(clock, "repo-b")

	inRemove := make(chan struct{})
	release := make(chan struct{})
	c.gc.remove = func(r *os.Root, name string) error {
		close(inRemove)
		<-release
		return r.RemoveAll(name)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.collectMirrors(context.Background())
	}()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	})
	select {
	case <-inRemove:
	case <-time.After(5 * time.Second):
		t.Fatal("the removal never started")
	}

	// While the old mirror is being deleted, repo-b returns and a sync creates its fresh mirror.
	c.handleConfig(gcConfigMsg("repo-a", "repo-b"))
	h.mkdir(old)
	if err := os.WriteFile(h.path(old, "fresh-fetch"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done

	if !h.exists(old, "fresh-fetch") || !h.exists(old, "HEAD") {
		t.Fatal("the removal of the old mirror destroyed the fresh mirror of the returned repository")
	}
}

func TestConfigAcceptanceWakesMirrorGCOnlyWhenAccepted(t *testing.T) {
	h := newGCHarness(t)
	c := newGCClient(t, h, newGCClock(gcNow))

	c.handleConfig(json.RawMessage(`{"type":"git_sync_config","config_version":1,"repositories":[{"repo_id":"bad id"}]}`))
	select {
	case <-c.gc.wake:
		t.Fatal("a rejected configuration woke the mirror cleanup")
	default:
	}
	if !c.gc.rejected.Load() {
		t.Fatal("a rejected configuration was not recorded")
	}

	c.handleConfig(gcConfigMsg("repo-a"))
	select {
	case <-c.gc.wake:
	default:
		t.Fatal("an accepted configuration did not wake the mirror cleanup")
	}
	if c.gc.rejected.Load() {
		t.Fatal("an accepted configuration did not clear the rejected flag")
	}
}

func TestMirrorGCLoopSweepsAfterConfigAndOnTheTickerAndStopsOnCancel(t *testing.T) {
	h := newGCHarness(t)
	clock := newGCClock(gcNow)
	c := newGCClient(t, h, clock)
	c.gc.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		c.Wait(context.Background())
	})
	orphan := h.mirror("repo-b")
	c.startMirrorGC(ctx)

	// Nothing happens before a configuration is accepted, ticker or not.
	time.Sleep(60 * time.Millisecond)
	if h.exists(orphan, orphanMarkerName) {
		t.Fatal("the ticker swept before a configuration was accepted")
	}

	c.handleConfig(gcConfigMsg("repo-a"))
	gcEventually(t, "the orphan to be marked", func() bool { return h.exists(orphan, orphanMarkerName) })

	clock.add(mirrorGCGrace + time.Hour)
	gcEventually(t, "the overdue orphan to be removed by the ticker", func() bool { return !h.exists(orphan) })

	cancel()
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	c.Wait(wctx)
	if wctx.Err() != nil {
		t.Fatal("Wait did not return after the context was cancelled")
	}
	select {
	case <-c.gc.currentDone():
	default:
		t.Fatal("the cleanup loop is still running after Wait returned")
	}
}

func TestRunStartsMirrorGC(t *testing.T) {
	h := newGCHarness(t)
	clock := newGCClock(gcNow)
	c := newGCClient(t, h, clock)
	orphan := h.mirror("repo-b")
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		c.Wait(context.Background())
	})

	c.handleConfig(gcConfigMsg("repo-a"))
	gcEventually(t, "Run's cleanup loop to mark the orphan", func() bool { return h.exists(orphan, orphanMarkerName) })
}

// TestMirrorGCReturnThenLeaveRestartsTheGrace pins the rule "returning to the configuration clears the
// mark" for the cases where no sweep sees the mirror configured: sweeps are paused, or two
// configurations arrive before the loop wakes.
func TestMirrorGCReturnThenLeaveRestartsTheGrace(t *testing.T) {
	cases := []struct {
		name  string
		pause func(c *Client) (resume func())
	}{
		{"sweeps paused by a critical update", func(c *Client) func() {
			c.criticalPatchRequired.Store(true)
			return func() { c.criticalPatchRequired.Store(false) }
		}},
		{"two configurations before one sweep", func(*Client) func() { return func() {} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newGCHarness(t)
			clock := newGCClock(gcNow)
			c := newGCClient(t, h, clock)
			c.handleConfig(gcConfigMsg("repo-a"))
			r := h.overdueOrphan(clock, "repo-r") // marked long ago, when it was last absent

			resume := tc.pause(c)
			c.handleConfig(gcConfigMsg("repo-a", "repo-r")) // repo-r comes back ...
			c.handleConfig(gcConfigMsg("repo-a"))           // ... and leaves again before any sweep saw it
			resume()
			c.collectMirrors(context.Background())

			if !h.exists(r, "HEAD") {
				t.Fatal("a repository that came back and left again was removed without a fresh grace period")
			}
			clock.add(mirrorGCGrace - time.Hour)
			c.collectMirrors(context.Background())
			if !h.exists(r, "HEAD") {
				t.Fatal("removed before the restarted grace period elapsed")
			}
			clock.add(2 * time.Hour)
			c.collectMirrors(context.Background())
			if h.exists(r) {
				t.Fatal("not removed after the restarted grace period elapsed")
			}
		})
	}
}

func TestStartMirrorGCTwiceKeepsASingleLoop(t *testing.T) {
	h := newGCHarness(t)
	c := newGCClient(t, h, newGCClock(gcNow))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		c.Wait(context.Background())
	})

	c.startMirrorGC(ctx)
	first := c.gc.currentDone()
	c.startMirrorGC(ctx)

	if c.gc.currentDone() != first {
		t.Fatal("a second start replaced the first loop's done channel, so Wait would not cover it")
	}
}
