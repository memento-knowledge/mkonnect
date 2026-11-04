package gitsync_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// statsGit is a thread-safe fake git for the run-state tests. Unlike scriptedGit it also produces the
// standard output a sync's statistics are read from, and can advance a clock while a push "runs" so an
// attempt has a duration.
type statsGit struct {
	mu     sync.Mutex
	clock  *testClock
	subs   []string
	stdout map[string]string // by subcommand
	fail   map[string][]byte // by subcommand: stderr of a failing call
	exit1  map[string]bool   // by subcommand: fail with exit status 1 (show-ref: no refs)
	gate   chan struct{}     // held fetches wait for it (or their context)
	took   time.Duration     // how far a push advances the clock
}

func newStatsGit(clock *testClock) *statsGit {
	return &statsGit{
		clock: clock,
		stdout: map[string]string{
			"for-each-ref": "1767225600|\n", // 2026-01-01T00:00:00Z
			"push":         "To x\n*\trefs/heads/main:refs/heads/main\t[new branch]\n \trefs/tags/v1:refs/tags/v1\t1..2\nDone\n",
			"ls-remote":    "ref: refs/heads/trunk\tHEAD\n5dcfa042fb9b025599cd26145fd324dce4651d5d\tHEAD\n",
		},
		fail:  map[string][]byte{},
		exit1: map[string]bool{},
		took:  2500 * time.Millisecond,
	}
}

func subCmd(argv []string) string {
	for _, a := range argv {
		switch a {
		case "init", "fetch", "push", "show-ref", "for-each-ref", "ls-remote":
			return a
		}
	}
	return ""
}

func (g *statsGit) set(f func()) { g.mu.Lock(); defer g.mu.Unlock(); f() }

func (g *statsGit) count(sub string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, s := range g.subs {
		if s == sub {
			n++
		}
	}
	return n
}

func (g *statsGit) run(ctx context.Context, argv, env []string, dir string) ([]byte, error) {
	return g.runOut(ctx, argv, env, dir, io.Discard)
}

func (g *statsGit) runOut(ctx context.Context, argv, _ []string, _ string, w io.Writer) ([]byte, error) {
	sub := subCmd(argv)
	g.mu.Lock()
	g.subs = append(g.subs, sub)
	gate, stderr, exit1, out := g.gate, g.fail[sub], g.exit1[sub], g.stdout[sub]
	g.mu.Unlock()

	if gate != nil && sub == "fetch" {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if exit1 {
		return nil, exec.Command("false").Run()
	}
	if stderr != nil {
		return stderr, errors.New("exit status 128")
	}
	if sub == "init" {
		if err := os.MkdirAll(argv[len(argv)-1], 0o700); err != nil {
			return nil, err
		}
	}
	if sub == "push" {
		g.clock.advance(g.took)
	}
	_, _ = io.WriteString(w, out)
	return nil, nil
}

// statsHarness is a client over a mirrors root that outlives it, so a test can "restart" the connector.
type statsHarness struct {
	t      *testing.T
	root   string
	clock  *testClock
	git    *statsGit
	c      *gitsync.Client
	s      *stubSender
	ctx    context.Context
	cancel context.CancelFunc
}

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

const repoAOrigin = "https://git.example.com/team/a.git"

func newStatsHarness(t *testing.T, root string, clock *testClock, g *statsGit, extra ...gitsync.Option) *statsHarness {
	return newStatsHarnessWith(t, providerWithToken, root, clock, g, extra...)
}

func newStatsHarnessWith(t *testing.T, provider, root string, clock *testClock, g *statsGit, extra ...gitsync.Option) *statsHarness {
	t.Helper()
	h := &statsHarness{t: t, root: root, clock: clock, git: g, s: &stubSender{}}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	opts := append([]gitsync.Option{
		gitsync.WithMirrorsRoot(root), gitsync.WithGitRunner(g.run), gitsync.WithGitOutputRunnerForTest(g.runOut),
		gitsync.WithRunSaveIntervalForTest(time.Millisecond),
	}, extra...)
	h.c = newTestClientOpts(t, provider, &fakeProber{result: gitsync.OriginReachable}, clock.now, opts...)
	h.c.OnConnected(h.ctx, h.s)
	// Registered after the client's own temp dirs, so it runs first: every sync has stopped before
	// the directories it writes into are removed.
	t.Cleanup(func() {
		h.cancel()
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer wcancel()
		h.c.Wait(wctx)
	})
	return h
}

func newHarness(t *testing.T) *statsHarness {
	t.Helper()
	clock := &testClock{t: t0}
	return newStatsHarness(t, t.TempDir(), clock, newStatsGit(clock))
}

// restart builds a second client over the same mirrors root, as a process restart would.
func (h *statsHarness) restart() *statsHarness {
	h.t.Helper()
	h.cancel()
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	h.c.Wait(wctx)
	return newStatsHarness(h.t, h.root, h.clock, newStatsGit(h.clock))
}

func (h *statsHarness) config(repos ...[3]string) {
	h.c.HandleMessage(h.ctx, h.s, "git_sync_config", configJSON("1", repos...))
}

func (h *statsHarness) configA() { h.config([3]string{"repo-a", repoAOrigin, ""}) }

func (h *statsHarness) syncNow() {
	h.c.HandleMessage(h.ctx, h.s, "git_sync_now", rawJSON(`{"type":"git_sync_now","repo_id":"repo-a"}`))
}

func (h *statsHarness) status(id string) proto.GitSyncRepoStatus { return repoStatusNow(h.t, h.s, id) }

// wire returns the repository's status object exactly as it is marshalled, so a test can tell an
// absent field from a null one.
func (h *statsHarness) wire(id string) map[string]json.RawMessage {
	h.t.Helper()
	for _, r := range lastStatus(h.t, h.s.messages()).Repositories {
		if r.RepoID == id {
			b, err := json.Marshal(r)
			if err != nil {
				h.t.Fatal(err)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(b, &m); err != nil {
				h.t.Fatal(err)
			}
			return m
		}
	}
	h.t.Fatalf("no status for %s", id)
	return nil
}

// settle waits for repo-a's sync to finish (any state but syncing) and for the status carrying it.
func (h *statsHarness) settle() proto.GitSyncRepoStatus {
	h.t.Helper()
	waitFor(h.t, func() bool {
		st := h.status("repo-a").State
		return st != gitsync.StateSyncing
	})
	waitFor(h.t, func() bool { return h.c.IdleForTest() })
	return h.status("repo-a")
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestLastSuccessIsNullUntilTheFirstSuccess(t *testing.T) {
	h := newHarness(t)
	h.git.set(func() {
		h.git.fail["fetch"] = []byte("fatal: unable to access 'https://git.example.com/team/a.git/': The requested URL returned error: 403")
	})

	h.configA()
	r := h.settle()
	if r.State != gitsync.StateError {
		t.Fatalf("state = %q, want error", r.State)
	}
	if raw, ok := h.wire("repo-a")["last_success_at"]; !ok || string(raw) != "null" {
		t.Errorf("last_success_at on the wire = %s (present: %v), want present as null before the first success", raw, ok)
	}
	if str(r.LastAttemptAt) != "2026-03-04T05:06:07Z" {
		t.Errorf("last_attempt_at = %s: a failed attempt is still an attempt", str(r.LastAttemptAt))
	}
	if r.DurationMS == nil || *r.DurationMS != 0 {
		t.Errorf("duration_ms = %v, want the failed attempt's duration (0 on a frozen clock)", r.DurationMS)
	}
}

func TestSuccessIsReportedWithItsStatistics(t *testing.T) {
	h := newHarness(t)
	h.configA()
	r := h.settle()

	if r.State != gitsync.StateOK {
		t.Fatalf("state = %q, want ok", r.State)
	}
	if str(r.LastAttemptAt) != "2026-03-04T05:06:07Z" {
		t.Errorf("last_attempt_at = %s, want the start of the attempt", str(r.LastAttemptAt))
	}
	if str(r.LastSuccessAt) != "2026-03-04T05:06:09Z" {
		t.Errorf("last_success_at = %s, want when the attempt finished (start + 2.5s)", str(r.LastSuccessAt))
	}
	if r.DurationMS == nil || *r.DurationMS != 2500 {
		t.Errorf("duration_ms = %v, want 2500", r.DurationMS)
	}
	if r.RefsPushed == nil || *r.RefsPushed != 2 {
		t.Errorf("refs_pushed = %v, want 2", r.RefsPushed)
	}
	if str(r.NewestOriginCommitAt) != "2026-01-01T00:00:00Z" {
		t.Errorf("newest_origin_commit_at = %s", str(r.NewestOriginCommitAt))
	}
	if r.OriginDefaultBranch != "trunk" {
		t.Errorf("origin_default_branch = %q, want trunk", r.OriginDefaultBranch)
	}
}

func TestARunThatFindsNothingNewIsStillASuccess(t *testing.T) {
	h := newHarness(t)
	h.git.set(func() { h.git.stdout["push"] = "To x\n=\trefs/heads/main:refs/heads/main\t[up to date]\nDone\n" })
	h.configA()
	r := h.settle()

	if r.State != gitsync.StateOK || r.LastSuccessAt == nil {
		t.Fatalf("status = %+v, want ok with a last_success_at", r)
	}
	if r.RefsPushed == nil || *r.RefsPushed != 0 {
		t.Errorf("refs_pushed = %v, want a present 0", r.RefsPushed)
	}
	if raw := h.wire("repo-a")["refs_pushed"]; string(raw) != "0" {
		t.Errorf("refs_pushed on the wire = %s, want 0 (present, not omitted)", raw)
	}
}

func TestAFailureKeepsTheLastSuccessButRecordsTheAttempt(t *testing.T) {
	h := newHarness(t)
	h.configA()
	ok := h.settle()
	if ok.State != gitsync.StateOK {
		t.Fatalf("setup: %+v", ok)
	}

	h.clock.advance(time.Hour)
	h.git.set(func() {
		h.git.fail["fetch"] = []byte("fatal: unable to access 'https://git.example.com/team/a.git/': The requested URL returned error: 403")
	})
	h.syncNow()
	waitFor(t, func() bool { return h.status("repo-a").State == gitsync.StateError })
	r := h.settle()

	if str(r.LastSuccessAt) != str(ok.LastSuccessAt) {
		t.Errorf("last_success_at = %s, want the earlier success %s carried across the failure", str(r.LastSuccessAt), str(ok.LastSuccessAt))
	}
	if str(r.LastAttemptAt) != "2026-03-04T06:06:09Z" {
		t.Errorf("last_attempt_at = %s, want the failed attempt's start", str(r.LastAttemptAt))
	}
	if r.DurationMS == nil || *r.DurationMS != 0 {
		t.Errorf("duration_ms = %v, want the failed attempt's own duration", r.DurationMS)
	}
	if r.RefsPushed == nil || *r.RefsPushed != 0 {
		t.Errorf("refs_pushed = %v, want 0: the failed attempt pushed nothing", r.RefsPushed)
	}
	// What was learned about the origin earlier is kept, not erased by an attempt that learned nothing.
	if r.OriginDefaultBranch != "trunk" || str(r.NewestOriginCommitAt) != "2026-01-01T00:00:00Z" {
		t.Errorf("status = %+v, want the origin facts of the last attempt that learned them", r)
	}
}

func TestAnEmptyOriginIsAFailedAttempt(t *testing.T) {
	h := newHarness(t)
	h.git.set(func() { h.git.exit1["show-ref"] = true })
	h.configA()
	r := h.settle()

	if r.State != gitsync.StateError || r.LastSuccessAt != nil || r.LastAttemptAt == nil {
		t.Errorf("status = %+v, want an error with an attempt recorded and no success", r)
	}
}

func TestACancelledSyncRecordsNothing(t *testing.T) {
	h := newHarness(t)
	h.configA()
	ok := h.settle()

	h.git.set(func() { h.git.gate = make(chan struct{}) })
	h.clock.advance(time.Hour)
	h.syncNow()
	waitFor(t, func() bool { return h.git.count("fetch") == 2 })
	h.cancel()
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	h.c.Wait(wctx)
	// A cancelled sync reports nothing by itself; the next heartbeat carries the state.
	if err := h.c.OnHeartbeat(context.Background(), h.s); err != nil {
		t.Fatal(err)
	}

	r := h.status("repo-a")
	if str(r.LastAttemptAt) != str(ok.LastAttemptAt) || str(r.LastSuccessAt) != str(ok.LastSuccessAt) ||
		*r.DurationMS != *ok.DurationMS {
		t.Errorf("status after a cancelled sync = %+v, want the previous attempt's record unchanged (%+v)", r, ok)
	}
	if r.State != gitsync.StateOK {
		t.Errorf("state = %q, want the previous result kept", r.State)
	}
}

func TestAnInFlightAttemptShowsItsStartButThePreviousDuration(t *testing.T) {
	h := newHarness(t)
	h.configA()
	ok := h.settle()

	gate := make(chan struct{})
	h.git.set(func() { h.git.gate = gate })
	h.clock.advance(time.Hour)
	h.syncNow()
	waitFor(t, func() bool { return h.git.count("fetch") == 2 })
	h.c.OnHeartbeat(context.Background(), h.s)

	r := h.status("repo-a")
	if r.State != gitsync.StateSyncing {
		t.Fatalf("state = %q, want syncing", r.State)
	}
	if str(r.LastAttemptAt) != "2026-03-04T06:06:09Z" {
		t.Errorf("last_attempt_at = %s, want the start of the attempt in flight", str(r.LastAttemptAt))
	}
	if r.DurationMS == nil || *r.DurationMS != 2500 || r.RefsPushed == nil || *r.RefsPushed != 2 || str(r.LastSuccessAt) != str(ok.LastSuccessAt) {
		t.Errorf("status = %+v, want the previous finished attempt's duration, refs and success", r)
	}
	close(gate)
	h.settle()
}

func TestRunStateSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.configA()
	ok := h.settle()
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(h.root, "run-state.json")); return err == nil })

	h2 := h.restart()
	// The new process has not synced anything. Its first status after the configuration must already
	// carry what the old one knew, not report every repository as never having succeeded.
	h2.git.set(func() { h2.git.gate = make(chan struct{}) })
	h2.configA()
	r := h2.status("repo-a")
	if r.LastSuccessAt == nil || str(r.LastSuccessAt) != str(ok.LastSuccessAt) {
		t.Errorf("last_success_at after restart = %s, want %s", str(r.LastSuccessAt), str(ok.LastSuccessAt))
	}
	if r.DurationMS == nil || *r.DurationMS != 2500 || r.RefsPushed == nil || *r.RefsPushed != 2 ||
		r.OriginDefaultBranch != "trunk" || str(r.NewestOriginCommitAt) != "2026-01-01T00:00:00Z" {
		t.Errorf("status after restart = %+v, want the persisted statistics", r)
	}
}

func TestPersistedStateIsNotReportedBeforeAConfigurationNamesTheRepository(t *testing.T) {
	// Until a configuration is accepted there are no repositories to report, so nothing leaks early.
	h := newHarness(t)
	h.configA()
	h.settle()
	h2 := h.restart()
	if got := len(lastStatus(t, h2.s.messages()).Repositories); got != 0 {
		t.Errorf("status before any configuration lists %d repositories, want 0", got)
	}
}

func TestChangedTargetDoesNotInheritTheOldSuccess(t *testing.T) {
	cases := map[string]func(h *statsHarness){
		"replica moved": func(h *statsHarness) {
			h.c.HandleMessage(h.ctx, h.s, "git_sync_config", configJSONRaw("2",
				repoJSON("repo-a", repoAOrigin, "", "https://replica2.example.com/r/repo-a.git", testPushToken)))
		},
		"origin moved": func(h *statsHarness) {
			h.config([3]string{"repo-a", "https://git.example.com/team/other.git", ""})
		},
	}
	for name, change := range cases {
		for _, restart := range []bool{false, true} {
			t.Run(name+map[bool]string{false: " in process", true: " across restart"}[restart], func(t *testing.T) {
				h := newHarness(t)
				h.configA()
				h.settle()
				waitFor(t, func() bool { _, err := os.Stat(filepath.Join(h.root, "run-state.json")); return err == nil })
				if restart {
					h = h.restart()
				}
				h.git.set(func() { h.git.gate = make(chan struct{}) })
				change(h)
				r := h.status("repo-a")
				if r.LastSuccessAt != nil || r.OriginDefaultBranch != "" || r.NewestOriginCommitAt != nil || r.DurationMS != nil {
					t.Errorf("status = %+v: a different origin or replica must not report the old replication's success", r)
				}
				if raw := h.wire("repo-a")["last_success_at"]; string(raw) != "null" {
					t.Errorf("last_success_at on the wire = %s, want null", raw)
				}
			})
		}
	}
}

func TestRefreshedTokenKeepsTheSuccess(t *testing.T) {
	h := newHarness(t)
	h.configA()
	ok := h.settle()
	h.git.set(func() { h.git.gate = make(chan struct{}) })
	h.c.HandleMessage(h.ctx, h.s, "git_sync_config", configJSONRaw("2",
		repoJSON("repo-a", repoAOrigin, "", "https://replica.example.com/r/repo-a.git", "a-newer-push-token-abcdef")))
	if r := h.status("repo-a"); str(r.LastSuccessAt) != str(ok.LastSuccessAt) {
		t.Errorf("last_success_at = %s, want it kept across a token refresh", str(r.LastSuccessAt))
	}
}

// A repository that cannot currently be synced (no local provider matches its host) keeps the success
// it earned while it could, provided its origin and replica are what they were: the replica still holds
// what was pushed.
func TestUnsyncableRepositoryKeepsItsSuccessWhileItsTargetIsUnchanged(t *testing.T) {
	h := newHarness(t)
	h.configA()
	ok := h.settle()

	// A restart under a local configuration that no longer covers the origin's host.
	h.cancel()
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	h.c.Wait(wctx)
	const otherProvider = `{"origin":{"base_url":"https://other.example.org/","auth":"bearer","token":"origin-token-abcdefghij"}}`
	h2 := newStatsHarnessWith(t, otherProvider, h.root, h.clock, newStatsGit(h.clock))
	h2.configA()

	r := h2.status("repo-a")
	if r.State != gitsync.StateError || r.ErrorType != gitsync.ErrOriginAccessDenied {
		t.Fatalf("status = %+v, want origin_access_denied", r)
	}
	if str(r.LastSuccessAt) != str(ok.LastSuccessAt) {
		t.Errorf("last_success_at = %s, want %s kept while the repository cannot be synced", str(r.LastSuccessAt), str(ok.LastSuccessAt))
	}
}

// ...but a repository repointed at another origin or replica while it cannot be synced is a different
// replication, however it is reported.
func TestUnsyncableRepositoryDoesNotKeepASuccessFromAnotherTarget(t *testing.T) {
	cases := map[string]func(h *statsHarness){
		"origin moved to an unmatched host": func(h *statsHarness) {
			h.config([3]string{"repo-a", "https://elsewhere.example.org/team/a.git", ""})
		},
		"replica moved while the origin is unmatched": func(h *statsHarness) {
			h.c.HandleMessage(h.ctx, h.s, "git_sync_config", configJSONRaw("2",
				repoJSON("repo-a", "https://elsewhere.example.org/team/a.git", "", "https://replica2.example.com/r/repo-a.git", testPushToken)))
		},
		"replica URL no longer valid": func(h *statsHarness) {
			h.c.HandleMessage(h.ctx, h.s, "git_sync_config", configJSONRaw("2",
				repoJSON("repo-a", repoAOrigin, "", "http://replica.example.com/r/repo-a.git", testPushToken)))
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.configA()
			h.settle()
			change(h)
			r := h.status("repo-a")
			if r.State != gitsync.StateError {
				t.Fatalf("status = %+v, want an error state", r)
			}
			if r.LastSuccessAt != nil {
				t.Errorf("last_success_at = %s, want null: the old success describes another target", str(r.LastSuccessAt))
			}
		})
	}
}

func TestFinishedAttemptsAreSavedInBatches(t *testing.T) {
	clock := &testClock{t: t0}
	h := newStatsHarness(t, t.TempDir(), clock, newStatsGit(clock), gitsync.WithRunSaveIntervalForTest(time.Hour))

	// An attempt that finishes after a quiet period is saved at once.
	h.config([3]string{"repo-a", repoAOrigin, ""})
	waitFor(t, func() bool { return h.git.count("push") == 1 })
	waitFor(t, func() bool { return h.c.IdleForTest() })
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(h.root, "run-state.json")); return err == nil })

	// The ones right behind it wait for the interval rather than rewriting the whole file each time.
	h.config([3]string{"repo-a", repoAOrigin, ""}, [3]string{"repo-b", "https://git.example.com/team/b.git", ""})
	waitFor(t, func() bool { return h.git.count("push") == 3 })
	waitFor(t, func() bool { return h.c.IdleForTest() })
	got := runStateOnDisk(t, h.root)
	if _, ok := got["repo-b"]; ok || len(got) != 1 {
		t.Fatalf("run state holds %v before the interval has passed, want only the first save's repo-a", got)
	}

	// Wait is the shutdown path: it flushes what is still pending.
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	h.c.Wait(wctx)
	if n := len(runStateOnDisk(t, h.root)); n != 2 {
		t.Errorf("run state holds %d repositories after Wait, want 2", n)
	}
}

func TestPendingSaveIsFlushedByTheTrailingTimer(t *testing.T) {
	clock := &testClock{t: t0}
	h := newStatsHarness(t, t.TempDir(), clock, newStatsGit(clock), gitsync.WithRunSaveIntervalForTest(150*time.Millisecond))
	h.config([3]string{"repo-a", repoAOrigin, ""}, [3]string{"repo-b", "https://git.example.com/team/b.git", ""})
	waitFor(t, func() bool { return len(runStateOnDisk(t, h.root)) == 2 })
}

func runStateOnDisk(t *testing.T, root string) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "run-state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil // not written yet; callers poll
	}
	if err != nil {
		t.Fatalf("read run state: %v", err)
	}
	var f struct {
		Repositories map[string]json.RawMessage `json:"repositories"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("run state is not JSON: %v", err)
	}
	return f.Repositories
}

func TestAcceptedConfigurationPrunesRepositoriesItNoLongerListsButNotWhenEmpty(t *testing.T) {
	h := newHarness(t)
	h.config([3]string{"repo-a", repoAOrigin, ""}, [3]string{"repo-b", "https://git.example.com/team/b.git", ""})
	waitFor(t, func() bool { return h.git.count("push") == 2 })
	waitFor(t, func() bool { return h.c.IdleForTest() })
	waitFor(t, func() bool { return len(runStateOnDisk(t, h.root)) == 2 })

	h.config() // an empty set must not wipe every success time
	if got := runStateOnDisk(t, h.root); len(got) != 2 {
		t.Fatalf("an empty configuration pruned the state: %v", got)
	}

	h.config([3]string{"repo-a", repoAOrigin, ""})
	waitFor(t, func() bool { return h.c.IdleForTest() })
	waitFor(t, func() bool { got := runStateOnDisk(t, h.root); _, hasB := got["repo-b"]; return !hasB })
	if _, ok := runStateOnDisk(t, h.root)["repo-a"]; !ok {
		t.Error("repo-a is still configured and must be kept")
	}
}

func TestACorruptStateFileDoesNotStopTheConnectorSyncing(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "run-state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: t0}
	h := newStatsHarness(t, root, clock, newStatsGit(clock))
	h.configA()
	r := h.settle()
	if r.State != gitsync.StateOK || r.LastSuccessAt == nil {
		t.Fatalf("status = %+v, want a normal first sync", r)
	}
	waitFor(t, func() bool { return len(runStateOnDisk(t, root)) == 1 })
}

func TestAFailingStateWriteNeverFailsASync(t *testing.T) {
	root := t.TempDir()
	// A directory where the state file belongs: every write is refused, whoever runs the test.
	if err := os.Mkdir(filepath.Join(root, "run-state.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "run-state.json", "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: t0}
	h := newStatsHarness(t, root, clock, newStatsGit(clock))
	h.configA()
	r := h.settle()
	if r.State != gitsync.StateOK || r.LastSuccessAt == nil {
		t.Errorf("status = %+v, want ok: bookkeeping that cannot be saved must not fail the sync", r)
	}
}

func TestStatisticsAreSkippedWhenOnlyAPlainRunnerIsGiven(t *testing.T) {
	// WithGitRunner alone (the default in these tests) has no standard output, so refs_pushed, the
	// newest commit and the default branch stay unknown while the success is still recorded.
	clock := &testClock{t: t0}
	g := newStatsGit(clock)
	c := newTestClientOpts(t, providerWithToken, &fakeProber{result: gitsync.OriginReachable}, clock.now,
		gitsync.WithGitRunner(g.run))
	s := &stubSender{}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer wcancel()
		c.Wait(wctx)
	}()
	c.OnConnected(ctx, s)
	c.HandleMessage(ctx, s, "git_sync_config", configJSON("1", [3]string{"repo-a", repoAOrigin, ""}))
	waitFor(t, func() bool { return repoStatusNow(t, s, "repo-a").State == gitsync.StateOK })

	r := repoStatusNow(t, s, "repo-a")
	if r.LastSuccessAt == nil || r.RefsPushed != nil || r.NewestOriginCommitAt != nil || r.OriginDefaultBranch != "" {
		t.Errorf("status = %+v, want a success with no statistics", r)
	}
	if g.count("for-each-ref") != 0 || g.count("ls-remote") != 0 {
		t.Error("no extra git calls may be made without the output runner")
	}
}

func TestRunStateFileHoldsNoCredentialOrLocation(t *testing.T) {
	h := newHarness(t)
	h.configA()
	h.settle()
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(h.root, "run-state.json")); return err == nil })
	b, _ := os.ReadFile(filepath.Join(h.root, "run-state.json"))
	for _, forbidden := range []string{originToken, testPushToken, "example.com", "https://"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("run state file contains %q: %s", forbidden, b)
		}
	}
	info, _ := os.Stat(filepath.Join(h.root, "run-state.json"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}
