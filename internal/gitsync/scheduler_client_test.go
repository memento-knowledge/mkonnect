package gitsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// These tests wire a real Client, manager and scheduler together over a fake git, with a clock the
// test advances. They keep their own fixtures so they depend on no other test file's helpers.

type schedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *schedClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *schedClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type schedStubSender struct{}

func (schedStubSender) Send(context.Context, any) error { return nil }

// schedEnv is a Client whose git is a counter: fetches are counted, fail with a 403 while failing is
// set, and block while gate is non-nil.
type schedEnv struct {
	c       *Client
	clock   *schedClock
	fetches atomic.Int32
	failing atomic.Bool
	// failStatus is the HTTP status a failing fetch reports; zero means 403.
	failStatus atomic.Int32
	gate       atomic.Pointer[chan struct{}]
	ctx        context.Context
	// providerPath is the provider configuration file the client was started from.
	providerPath string
	// limits, when set, is sent as the configuration's "limits" object.
	limits string
	// originURL, when set, gives the origin_url a configuration sends for a repository.
	originURL func(id string) string
	// redirectNoGit makes a fetch of a URL without the ".git" suffix fail with a 301, as a forge does.
	redirectNoGit atomic.Bool
	// fetchedURLs are the URLs the fetches were made against, in order.
	urlMu       sync.Mutex
	fetchedURLs []string
}

func (e *schedEnv) fetched() []string {
	e.urlMu.Lock()
	defer e.urlMu.Unlock()
	return slices.Clone(e.fetchedURLs)
}

const schedProviderJSON = `{"origin":{"base_url":"https://git.example.com/"}}`

func newSchedEnv(t *testing.T, now func() time.Time) *schedEnv {
	t.Helper()
	return newSchedEnvProviders(t, now, schedProviderJSON)
}

func newSchedEnvProviders(t *testing.T, now func() time.Time, providerJSON string) *schedEnv {
	t.Helper()
	e := &schedEnv{clock: &schedClock{t: schedT0}}
	if now == nil {
		now = e.clock.now
	}
	runner := func(ctx context.Context, argv, _ []string, _ string) ([]byte, error) {
		if (gitCall{argv: argv}).sub() != "fetch" {
			return nil, nil
		}
		e.fetches.Add(1)
		var fetchURL string
		if i := slices.Index(argv, "--"); i >= 0 && i+1 < len(argv) {
			fetchURL = argv[i+1]
		}
		e.urlMu.Lock()
		e.fetchedURLs = append(e.fetchedURLs, fetchURL)
		e.urlMu.Unlock()
		if g := e.gate.Load(); g != nil {
			select {
			case <-*g:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if e.redirectNoGit.Load() && !strings.HasSuffix(fetchURL, ".git") {
			return []byte("fatal: unable to access 'https://git.example.com/group/repo-a/': The requested URL returned error: 301"), errExit
		}
		if e.failing.Load() {
			status := e.failStatus.Load()
			if status == 0 {
				status = 403
			}
			return []byte(fmt.Sprintf("fatal: unable to access 'https://git.example.com/group/repo-a/': The requested URL returned error: %d", status)), errExit
		}
		return nil, nil
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "git-sync.json")
	if err := os.WriteFile(path, []byte(providerJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	e.providerPath = path
	local, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{GatewayURL: "ws://127.0.0.1:1", ConnectorID: "gitsync-test", ProtocolVersion: "v1"}
	e.c = NewClient(cfg, auth.NewKeyStore(filepath.Join(dir, "key")), local, nil, now, "boot-test",
		WithMirrorsRoot(t.TempDir()), WithGitRunner(runner), WithStatusInterval(time.Millisecond),
		WithReplicaHost("replica.example.com"))
	e.c.sched.hooks.rand = func() float64 { return 0 }

	ctx, cancel := context.WithCancel(context.Background())
	e.ctx = ctx
	// Registered after the temp dirs, so it runs before their removal: every sync has stopped by then.
	t.Cleanup(func() {
		if g := e.gate.Swap(nil); g != nil {
			close(*g)
		}
		cancel()
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer wcancel()
		e.c.Wait(wctx)
	})
	return e
}

// configure sends a configuration for the given repositories, as the platform would.
func (e *schedEnv) configure(replicaSuffix string, ids ...string) {
	repos := make([]string, len(ids))
	for i, id := range ids {
		origin := fmt.Sprintf("https://git.example.com/group/%s.git", id)
		if e.originURL != nil {
			origin = e.originURL(id)
		}
		repos[i] = fmt.Sprintf(`{"repo_id":%q,"origin_url":%q,"replica_push_url":"https://replica.example.com/r/%s%s.git","push_token":"push-token-abcdefghij"}`, id, origin, id, replicaSuffix)
	}
	limits := ""
	if e.limits != "" {
		limits = `"limits":` + e.limits + `,`
	}
	raw := `{"type":"git_sync_config","config_version":1,` + limits + `"repositories":[` + strings.Join(repos, ",") + `]}`
	e.c.HandleMessage(e.ctx, schedStubSender{}, "git_sync_config", []byte(raw))
}

func (e *schedEnv) holdSyncs() chan struct{} {
	g := make(chan struct{})
	e.gate.Store(&g)
	return g
}

func (e *schedEnv) settle(t *testing.T) { t.Helper(); eventually(t, e.c.mgr.idle) }

func (e *schedEnv) dueIn(t *testing.T, id string) time.Duration {
	t.Helper()
	d, ok := e.c.sched.nextDue(id)
	if !ok {
		t.Fatalf("no schedule for %s", id)
	}
	return d.Sub(e.clock.now())
}

func TestClientDefaultLimitsAreInEffect(t *testing.T) {
	e := newSchedEnv(t, nil)
	if got := e.c.effectiveLimits(); got != DefaultLimits() {
		t.Errorf("effectiveLimits = %+v, want the defaults %+v", got, DefaultLimits())
	}
	l := DefaultLimits()
	l.Interval = time.Hour
	e.c.limits.Store(&l)
	if got := e.c.effectiveLimits().Interval; got != time.Hour {
		t.Errorf("interval = %v after the limits were replaced, want 1h", got)
	}
}

// The schedule exists as soon as a configuration is accepted, before the sync it starts can finish:
// otherwise that first sync's outcome, which may be a credential failure, would not count.
func TestClientConfigCreatesTheScheduleBeforeItsSyncFinishes(t *testing.T) {
	e := newSchedEnv(t, nil)
	gate := e.holdSyncs()

	e.configure("", "repo-a")
	if got := e.dueIn(t, "repo-a"); got != 15*time.Minute {
		t.Errorf("first scheduled run in %v, want the staggered first run (15m with a zero draw)", got)
	}
	close(gate)
	e.gate.Store(nil)
	e.settle(t)
}

func TestClientFirstSyncFailureCountsTowardsBackoff(t *testing.T) {
	e := newSchedEnv(t, nil)
	e.failing.Store(true)
	e.configure("", "repo-a")
	eventually(t, func() bool { return e.fetches.Load() == 1 })
	e.settle(t)
	if got := e.dueIn(t, "repo-a"); got != time.Hour {
		t.Errorf("after a rejected credential next run in %v, want 1h", got)
	}
}

func TestClientRepeatedConfigurationKeepsBackoffUnlessTheTargetChanges(t *testing.T) {
	e := newSchedEnv(t, nil)
	e.failing.Store(true)
	e.configure("", "repo-a")
	e.settle(t)
	if got := e.dueIn(t, "repo-a"); got != time.Hour {
		t.Fatalf("setup: next run in %v, want 1h", got)
	}

	gate := e.holdSyncs() // look at the schedule while the sync a configuration starts is running
	e.configure("", "repo-a")
	if got := e.dueIn(t, "repo-a"); got != time.Hour {
		t.Errorf("identical configuration: next run in %v, want the backoff kept at 1h", got)
	}
	close(gate)
	e.gate.Store(nil)
	e.settle(t)

	gate = e.holdSyncs()
	e.configure("-moved", "repo-a")
	if got := e.dueIn(t, "repo-a"); got != 15*time.Minute {
		t.Errorf("new replica: next run in %v, want a fresh first run in 15m", got)
	}
	close(gate)
	e.gate.Store(nil)
	e.settle(t)
}

func TestClientConfigRemovingARepositoryDropsItsSchedule(t *testing.T) {
	e := newSchedEnv(t, nil)
	e.configure("", "repo-a", "repo-b")
	e.settle(t)
	e.configure("", "repo-a")
	e.settle(t)
	if _, ok := e.c.sched.nextDue("repo-b"); ok {
		t.Error("repo-b is still scheduled after it left the configuration")
	}
	if _, ok := e.c.sched.nextDue("repo-a"); !ok {
		t.Error("repo-a lost its schedule")
	}
}

func TestClientScheduledSyncRunsWhenDueAndNotWhileSuspended(t *testing.T) {
	e := newSchedEnv(t, nil)
	e.configure("", "repo-a")
	eventually(t, func() bool { return e.fetches.Load() == 1 })
	e.settle(t)

	e.clock.advance(time.Hour)
	e.c.criticalPatchRequired.Store(true)
	e.c.sched.poll(e.ctx)
	if !e.c.mgr.idle() || e.fetches.Load() != 1 {
		t.Fatalf("a sync started while a critical update is required (fetches=%d)", e.fetches.Load())
	}

	// Start the scheduled sync and hold it in flight.
	gate := e.holdSyncs()
	e.c.criticalPatchRequired.Store(false)
	e.c.sched.poll(e.ctx)
	eventually(t, func() bool { return e.fetches.Load() == 2 })

	// While it runs the repository falls due again; the scheduler must skip it, leaving no rerun.
	e.clock.advance(4 * time.Hour)
	e.c.sched.poll(e.ctx)
	close(gate)
	e.gate.Store(nil)
	e.settle(t)
	time.Sleep(20 * time.Millisecond)
	if n := e.fetches.Load(); n != 2 {
		t.Errorf("fetches = %d, want exactly 2: one configured, one scheduled, no rerun", n)
	}
}

func TestClientRunDrivesTheSchedulerAndStopsItOnShutdown(t *testing.T) {
	e := newSchedEnv(t, time.Now)
	e.c.sched.tick = 2 * time.Millisecond
	e.configure("", "repo-a")
	// Set after configuring: an accepted configuration is a full replace of the limits, and one that
	// leaves them out restores the defaults. 20ms is also below what a configuration may ask for.
	l := DefaultLimits()
	l.Interval = 20 * time.Millisecond
	e.c.limits.Store(&l)

	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { e.c.Run(ctx); close(done) }()

	eventually(t, func() bool { return e.fetches.Load() >= 3 })
	if !schedulerGoroutineRunning() {
		t.Error("no scheduler goroutine while Run is active")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	if schedulerGoroutineRunning() {
		t.Error("the scheduler goroutine outlived Run")
	}
	e.c.Wait(context.Background())
	n := e.fetches.Load()
	time.Sleep(60 * time.Millisecond)
	if got := e.fetches.Load(); got != n {
		t.Errorf("a sync was scheduled after shutdown: %d -> %d", n, got)
	}
}

func schedulerGoroutineRunning() bool {
	buf := make([]byte, 1<<20)
	return strings.Contains(string(buf[:runtime.Stack(buf, true)]), "(*scheduler).start.func")
}

// repoStatus is the repository's entry in the status the client would send now.
func (e *schedEnv) repoStatus(t *testing.T, id string) proto.GitSyncRepoStatus {
	t.Helper()
	for _, r := range e.c.buildStatus().Repositories {
		if r.RepoID == id {
			return r
		}
	}
	t.Fatalf("no status for %s", id)
	return proto.GitSyncRepoStatus{}
}

// A repository whose origin URL redirects fails as origin_url_redirected and backs off. When the
// platform sends the corrected URL for the same repository id, that is a new sync target: the error
// and the backoff of the old one do not carry over, and the accepted configuration syncs every
// syncable repository at once, the unchanged ones included.
func TestClientCorrectedOriginURLStartsOverAndSyncsEveryRepositoryAtOnce(t *testing.T) {
	e := newSchedEnv(t, nil)
	var corrected atomic.Bool
	e.originURL = func(id string) string {
		url := "https://git.example.com/group/" + id
		if id != "repo-a" || corrected.Load() {
			url += ".git"
		}
		return url
	}
	e.redirectNoGit.Store(true)

	e.configure("", "repo-a", "repo-b")
	e.settle(t)
	a := e.repoStatus(t, "repo-a")
	if a.State != StateError || a.ErrorType != ErrOriginURLRedirected || a.ErrorDetail != detailOriginURLRedirected {
		t.Fatalf("redirected repository = %+v, want error/%s with its hint", a, ErrOriginURLRedirected)
	}
	if b := e.repoStatus(t, "repo-b"); b.State != StateOK {
		t.Fatalf("repo-b = %+v, want ok: a redirect on one repository is its own", b)
	}
	if got := e.dueIn(t, "repo-a"); got != 15*time.Minute {
		t.Fatalf("first failure: next run in %v, want 15m on the ordinary ladder", got)
	}

	// The same configuration again: still redirected, and the failure count keeps growing.
	e.configure("", "repo-a", "repo-b")
	e.settle(t)
	if got := e.dueIn(t, "repo-a"); got != 30*time.Minute {
		t.Fatalf("second failure: next run in %v, want 30m", got)
	}

	corrected.Store(true)
	before := e.fetches.Load()
	gate := e.holdSyncs() // look at the repository while the sync the configuration starts is running
	e.configure("", "repo-a", "repo-b")
	eventually(t, func() bool { return e.fetches.Load() == before+2 })
	if a := e.repoStatus(t, "repo-a"); a.ErrorType != "" || a.ErrorDetail != "" || a.State == StateError {
		t.Errorf("repo-a after the corrected URL = %+v, want the old error gone", a)
	}
	if got := e.dueIn(t, "repo-a"); got != 15*time.Minute {
		t.Errorf("corrected URL: next run in %v, want a fresh first run in 15m, not the old 30m/1h backoff", got)
	}
	close(gate)
	e.gate.Store(nil)
	e.settle(t)

	if a := e.repoStatus(t, "repo-a"); a.State != StateOK || a.ErrorType != "" {
		t.Errorf("repo-a after syncing the corrected URL = %+v, want ok", a)
	}
	urls := e.fetched()
	if got := urls[len(urls)-2:]; !slices.ContainsFunc(got, func(u string) bool { return u == "https://git.example.com:443/group/repo-a.git" }) {
		t.Errorf("last fetches %v never used the corrected URL", got)
	}
}

// Correcting the URL while a sync of the old one is still running: the old attempt's outcome is about a
// target that is gone and is dropped, and the corrected URL is synced right after it, not on the old
// schedule.
func TestClientCorrectedOriginURLWhileTheOldOneIsSyncing(t *testing.T) {
	e := newSchedEnv(t, nil)
	var corrected atomic.Bool
	e.originURL = func(id string) string {
		if corrected.Load() {
			return "https://git.example.com/group/" + id + ".git"
		}
		return "https://git.example.com/group/" + id
	}
	e.redirectNoGit.Store(true)

	gate := e.holdSyncs()
	e.configure("", "repo-a")
	eventually(t, func() bool { return e.fetches.Load() == 1 })
	corrected.Store(true)
	e.configure("", "repo-a") // folds into one trailing rerun of the new target
	close(gate)
	e.gate.Store(nil)
	e.settle(t)
	eventually(t, func() bool { return e.fetches.Load() == 2 })
	e.settle(t)

	urls := e.fetched()
	want := []string{"https://git.example.com:443/group/repo-a", "https://git.example.com:443/group/repo-a.git"}
	if !slices.Equal(urls, want) {
		t.Fatalf("fetched %v, want the old URL once and then the corrected one", urls)
	}
	if a := e.repoStatus(t, "repo-a"); a.State != StateOK || a.ErrorType != "" || a.ErrorDetail != "" {
		t.Errorf("repo-a = %+v, want ok: the old attempt's redirect must not be attributed to the corrected URL", a)
	}
	if got := e.dueIn(t, "repo-a"); got != 15*time.Minute {
		t.Errorf("next run in %v, want the fresh schedule (15m), not a backoff", got)
	}
}
