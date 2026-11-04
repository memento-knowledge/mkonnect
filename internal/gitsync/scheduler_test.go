package gitsync

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

var schedT0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// schedHarness drives a scheduler with a fake clock, a settable random draw and fake repositories,
// so every due time is exact. Nothing in it sleeps.
type schedHarness struct {
	t *testing.T
	s *scheduler

	mu        sync.Mutex
	now       time.Time
	rv        float64
	draws     int
	repos     map[string]targetKey
	busy      map[string]bool // repos the manager would report as already syncing
	refuse    map[string]bool // repos the manager would not start (not resolvable)
	enqueued  []string
	suspended bool
	limits    Limits
}

func newSchedHarness(t *testing.T) *schedHarness {
	t.Helper()
	h := &schedHarness{
		t:      t,
		now:    schedT0,
		repos:  map[string]targetKey{},
		busy:   map[string]bool{},
		refuse: map[string]bool{},
		limits: DefaultLimits(),
	}
	h.s = newScheduler(schedHooks{
		repos: func() map[string]targetKey {
			h.mu.Lock()
			defer h.mu.Unlock()
			out := make(map[string]targetKey, len(h.repos))
			for k, v := range h.repos {
				out[k] = v
			}
			return out
		},
		enqueue: func(_ context.Context, id string) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.busy[id] || h.refuse[id] {
				return false
			}
			h.enqueued = append(h.enqueued, id)
			return true
		},
		suspended: func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.suspended },
		limits:    func() Limits { h.mu.Lock(); defer h.mu.Unlock(); return h.limits },
		now:       func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now },
		rand:      func() float64 { h.mu.Lock(); defer h.mu.Unlock(); h.draws++; return h.rv },
	})
	return h
}

func (h *schedHarness) setNow(t time.Time)      { h.mu.Lock(); h.now = t; h.mu.Unlock() }
func (h *schedHarness) advance(d time.Duration) { h.mu.Lock(); h.now = h.now.Add(d); h.mu.Unlock() }
func (h *schedHarness) setRand(v float64)       { h.mu.Lock(); h.rv = v; h.mu.Unlock() }
func (h *schedHarness) setSuspended(v bool)     { h.mu.Lock(); h.suspended = v; h.mu.Unlock() }
func (h *schedHarness) setInterval(d time.Duration) {
	h.mu.Lock()
	h.limits.Interval = d
	h.mu.Unlock()
}
func (h *schedHarness) setBusy(id string, v bool) { h.mu.Lock(); h.busy[id] = v; h.mu.Unlock() }
func (h *schedHarness) drawCount() int            { h.mu.Lock(); defer h.mu.Unlock(); return h.draws }
func (h *schedHarness) dispatched() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.enqueued)
}
func (h *schedHarness) resetDispatched()       { h.mu.Lock(); h.enqueued = nil; h.mu.Unlock() }
func (h *schedHarness) currentTime() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }

func key(n string) targetKey {
	return targetKey{canonicalURL: "https://git.example.com/" + n + ".git", replicaURL: "https://replica.example.com/" + n + ".git"}
}

// configure sets the syncable set the way an accepted configuration would and reconciles.
func (h *schedHarness) configure(ids ...string) {
	h.t.Helper()
	h.mu.Lock()
	h.repos = map[string]targetKey{}
	for _, id := range ids {
		h.repos[id] = key(id)
	}
	h.mu.Unlock()
	h.s.reconcile()
}

func (h *schedHarness) retarget(id, replica string) {
	h.mu.Lock()
	k := h.repos[id]
	k.replicaURL = replica
	h.repos[id] = k
	h.mu.Unlock()
	h.s.reconcile()
}

func (h *schedHarness) taskFor(id string) syncTask {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := h.repos[id]
	return syncTask{RepoID: id, CanonicalURL: k.canonicalURL, ReplicaURL: k.replicaURL}
}

func (h *schedHarness) complete(id string, res syncResult) {
	h.t.Helper()
	h.s.completed(id, h.taskFor(id), res)
}

func okResult() syncResult             { return syncResult{State: StateOK} }
func failResult(typ string) syncResult { return syncResult{State: StateError, ErrorType: typ} }

func (h *schedHarness) poll() { h.s.poll(context.Background()) }

func (h *schedHarness) due(id string) time.Time {
	h.t.Helper()
	d, ok := h.s.nextDue(id)
	if !ok {
		h.t.Fatalf("no schedule for %s", id)
	}
	return d
}

// since returns how long after the current time id is next due.
func (h *schedHarness) dueIn(id string) time.Duration {
	h.t.Helper()
	return h.due(id).Sub(h.currentTime())
}

// Draws just below 1 make the upper edge of every range testable without ever reaching it.
const justBelowOne = 0.999999999

func TestSchedulerFirstRunIsStaggered(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		spread   time.Duration
	}{
		{"short interval spreads over the interval", time.Minute, time.Minute},
		{"default interval spreads over two minutes", 15 * time.Minute, 2 * time.Minute},
		{"long interval is capped at two minutes", 24 * time.Hour, 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSchedHarness(t)
			h.setInterval(tc.interval)
			h.setRand(0)
			h.configure("repo-a")
			if got := h.dueIn("repo-a"); got != tc.interval {
				t.Errorf("lowest first run = %v, want the interval %v", got, tc.interval)
			}
			h2 := newSchedHarness(t)
			h2.setInterval(tc.interval)
			h2.setRand(justBelowOne)
			h2.configure("repo-a")
			got := h2.dueIn("repo-a")
			if got < tc.interval || got >= tc.interval+tc.spread {
				t.Errorf("highest first run = %v, want in [%v, %v)", got, tc.interval, tc.interval+tc.spread)
			}
			if got < tc.interval+tc.spread-time.Second {
				t.Errorf("highest first run = %v, want close to the top of the range %v", got, tc.interval+tc.spread)
			}
		})
	}
}

func TestSchedulerDispatchesOnlyOnceDue(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0)
	h.configure("repo-a")

	h.setNow(schedT0.Add(15*time.Minute - time.Nanosecond))
	h.poll()
	if got := h.dispatched(); len(got) != 0 {
		t.Fatalf("dispatched before the due time: %v", got)
	}
	h.setNow(schedT0.Add(15 * time.Minute))
	h.poll()
	if got := h.dispatched(); !slices.Equal(got, []string{"repo-a"}) {
		t.Fatalf("dispatched at the due time = %v, want [repo-a]", got)
	}
}

// Dispatching moves the next-due forward even if the run never reports (for example it is released
// without recording anything), so a repository is never re-dispatched on every tick.
func TestSchedulerDispatchDefersTheNextRunUntilCompletion(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0)
	h.configure("repo-a")
	h.setNow(schedT0.Add(15 * time.Minute))
	h.poll()
	h.resetDispatched()

	h.advance(5 * time.Second)
	h.poll()
	if got := h.dispatched(); len(got) != 0 {
		t.Fatalf("re-dispatched while the earlier run has not reported: %v", got)
	}
}

func TestSchedulerPlainReschedulingUsesTenPercentJitter(t *testing.T) {
	for _, tc := range []struct {
		name string
		rv   float64
		want time.Duration
	}{
		{"low edge", 0, 13*time.Minute + 30*time.Second},
		{"middle", 0.5, 15 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSchedHarness(t)
			h.setRand(0)
			h.configure("repo-a")
			h.complete("repo-a", okResult()) // the first run after configuration: staggered
			h.setRand(tc.rv)
			h.advance(15 * time.Minute)
			h.poll() // a scheduled run
			h.advance(7 * time.Second)
			h.complete("repo-a", okResult())
			if got := h.dueIn("repo-a"); got != tc.want {
				t.Errorf("next due in %v, want %v", got, tc.want)
			}
		})
	}

	h := newSchedHarness(t)
	h.setRand(0)
	h.configure("repo-a")
	h.complete("repo-a", okResult())
	h.complete("repo-a", okResult())
	h.setRand(justBelowOne)
	h.complete("repo-a", okResult())
	got := h.dueIn("repo-a")
	if got <= 16*time.Minute+25*time.Second || got >= 16*time.Minute+30*time.Second {
		t.Errorf("highest jitter = %v, want just under 16m30s", got)
	}
}

func TestSchedulerFirstSuccessKeepsTheStagger(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0)
	h.configure("repo-a")
	h.advance(40 * time.Second) // the configuration's immediate sync takes a while
	h.setRand(justBelowOne)
	h.complete("repo-a", okResult())
	got := h.dueIn("repo-a")
	if got < 15*time.Minute || got >= 17*time.Minute || got < 16*time.Minute+59*time.Second {
		t.Errorf("next due in %v after the first success, want the top of [15m, 17m)", got)
	}
}

// The scheduler never makes a repository that is already syncing run again: the run in flight
// already covers it, and a trailing rerun would make a repository slower than its interval sync
// back to back.
func TestSchedulerSkipsABusyRepositoryWithoutAdvancing(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0)
	h.configure("repo-a")
	due := h.due("repo-a")
	h.setBusy("repo-a", true)

	h.setNow(due.Add(10 * time.Minute))
	for range 3 {
		h.poll()
	}
	if got := h.dispatched(); len(got) != 0 {
		t.Fatalf("dispatched a busy repository: %v", got)
	}
	if got := h.due("repo-a"); !got.Equal(due) {
		t.Errorf("a skip moved the due time to %v, want it unchanged at %v", got, due)
	}

	h.setBusy("repo-a", false)
	h.poll()
	if got := h.dispatched(); !slices.Equal(got, []string{"repo-a"}) {
		t.Errorf("dispatched after the run ended = %v, want [repo-a]", got)
	}
}

func TestSchedulerDoesNotAdvanceWhenTheManagerDeclines(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0)
	h.configure("repo-a")
	due := h.due("repo-a")
	h.mu.Lock()
	h.refuse["repo-a"] = true
	h.mu.Unlock()
	h.setNow(due)
	h.poll()
	if got := h.due("repo-a"); !got.Equal(due) {
		t.Errorf("due moved to %v after a declined dispatch, want %v", got, due)
	}
}

func TestSchedulerBackoffLadder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		want     []time.Duration
	}{
		{"default interval", 15 * time.Minute, []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, time.Hour, time.Hour}},
		{"short interval", time.Minute, []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}},
		{"interval above the cap is never shortened", 2 * time.Hour, []time.Duration{2 * time.Hour, 2 * time.Hour, 2 * time.Hour}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSchedHarness(t)
			h.setInterval(tc.interval)
			h.setRand(0)
			h.configure("repo-a")
			for i, want := range tc.want {
				h.complete("repo-a", failResult(ErrOriginUnreachable))
				if got := h.dueIn("repo-a"); got != want {
					t.Errorf("after failure %d, next due in %v, want %v", i+1, got, want)
				}
			}
		})
	}
}

func TestSchedulerBackoffDoesNotOverflow(t *testing.T) {
	for _, interval := range []time.Duration{time.Minute, 24 * time.Hour} {
		h := newSchedHarness(t)
		h.setInterval(interval)
		h.setRand(0)
		h.configure("repo-a")
		for range 2000 {
			h.s.completed("repo-a", h.taskFor("repo-a"), failResult(ErrReplicaUnreachable))
		}
		want := max(interval, time.Hour)
		if got := h.dueIn("repo-a"); got != want {
			t.Errorf("interval %v after 2000 failures: next due in %v, want %v", interval, got, want)
		}
	}
}

func TestSchedulerCredentialFailuresWaitAnHourFromTheFirst(t *testing.T) {
	for _, typ := range []string{ErrOriginAccessDenied, ErrCredentialRejected, ErrPushTokenExpired} {
		t.Run(typ, func(t *testing.T) {
			h := newSchedHarness(t)
			h.setRand(0)
			h.configure("repo-a")
			h.complete("repo-a", failResult(typ))
			if got := h.dueIn("repo-a"); got != time.Hour {
				t.Errorf("first failure: next due in %v, want 1h", got)
			}
			h.complete("repo-a", failResult(typ))
			if got := h.dueIn("repo-a"); got != time.Hour {
				t.Errorf("second failure: next due in %v, want 1h", got)
			}
		})
	}

	t.Run("a long interval is kept", func(t *testing.T) {
		h := newSchedHarness(t)
		h.setInterval(24 * time.Hour)
		h.setRand(0)
		h.configure("repo-a")
		h.complete("repo-a", failResult(ErrCredentialRejected))
		if got := h.dueIn("repo-a"); got != 24*time.Hour {
			t.Errorf("next due in %v, want the 24h interval", got)
		}
	})

	t.Run("the latest error type decides the tier", func(t *testing.T) {
		h := newSchedHarness(t)
		h.setRand(0)
		h.configure("repo-a")
		h.complete("repo-a", failResult(ErrOriginUnreachable))
		if got := h.dueIn("repo-a"); got != 15*time.Minute {
			t.Errorf("transient failure: next due in %v, want 15m", got)
		}
		h.complete("repo-a", failResult(ErrOriginAccessDenied))
		if got := h.dueIn("repo-a"); got != time.Hour {
			t.Errorf("then a credential failure: next due in %v, want 1h", got)
		}
		h.complete("repo-a", failResult(ErrReplicaUnreachable))
		if got := h.dueIn("repo-a"); got != time.Hour {
			t.Errorf("then a transient failure at count 3: next due in %v, want the ladder's 1h", got)
		}
	})
}

// Jitter must never make a retry earlier than the backoff floor: for a failing credential that
// would defeat the point of the delay.
func TestSchedulerFailureJitterNeverShortensTheDelay(t *testing.T) {
	for _, tc := range []struct {
		typ   string
		floor time.Duration
	}{
		{ErrOriginUnreachable, 15 * time.Minute},
		{ErrOriginAccessDenied, time.Hour},
	} {
		h := newSchedHarness(t)
		h.setRand(0)
		h.configure("repo-a")
		h.complete("repo-a", failResult(tc.typ))
		if got := h.dueIn("repo-a"); got != tc.floor {
			t.Errorf("%s: lowest delay = %v, want exactly the floor %v", tc.typ, got, tc.floor)
		}

		h = newSchedHarness(t)
		h.setRand(justBelowOne)
		h.configure("repo-a")
		h.complete("repo-a", failResult(tc.typ))
		got := h.dueIn("repo-a")
		if got <= tc.floor || got >= tc.floor+tc.floor/5 {
			t.Errorf("%s: highest delay = %v, want in (%v, %v)", tc.typ, got, tc.floor, tc.floor+tc.floor/5)
		}
	}
}

func TestSchedulerSuccessResetsBackoff(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0)
	h.configure("repo-a")
	h.complete("repo-a", failResult(ErrOriginAccessDenied))
	h.complete("repo-a", failResult(ErrOriginAccessDenied))
	h.complete("repo-a", syncResult{State: StateOK, Warnings: []string{WarnLFSNotReplicated}})
	if got := h.dueIn("repo-a"); got != 13*time.Minute+30*time.Second {
		t.Errorf("after a success (with a warning) next due in %v, want the plain jittered interval", got)
	}
	h.complete("repo-a", failResult(ErrOriginUnreachable))
	if got := h.dueIn("repo-a"); got != 15*time.Minute {
		t.Errorf("first failure after the success: next due in %v, want 15m (the ladder restarted)", got)
	}
}

func TestSchedulerResetsOnlyWhenTheSyncTargetChanges(t *testing.T) {
	setup := func() *schedHarness {
		h := newSchedHarness(t)
		h.setRand(0)
		h.configure("repo-a")
		h.complete("repo-a", failResult(ErrOriginAccessDenied))
		return h
	}

	h := setup()
	h.configure("repo-a") // an identical configuration re-sent (a token refresh)
	if got := h.dueIn("repo-a"); got != time.Hour {
		t.Errorf("identical config: next due in %v, want the backoff kept at 1h", got)
	}
	h.complete("repo-a", failResult(ErrUnclassified))
	if got := h.dueIn("repo-a"); got != 30*time.Minute {
		t.Errorf("identical config kept the failure count: next due in %v, want the ladder's 30m at count 2", got)
	}

	h = setup()
	h.retarget("repo-a", "https://replica.example.com/other.git")
	if got := h.dueIn("repo-a"); got != 15*time.Minute {
		t.Errorf("new replica: next due in %v, want a fresh schedule (15m)", got)
	}
	h.complete("repo-a", failResult(ErrOriginUnreachable))
	if got := h.dueIn("repo-a"); got != 15*time.Minute {
		t.Errorf("first failure of the new target: next due in %v, want 15m (count restarted)", got)
	}

	h = setup()
	h.mu.Lock()
	k := h.repos["repo-a"]
	k.canonicalURL = "https://git.example.com/moved.git"
	h.repos["repo-a"] = k
	h.mu.Unlock()
	h.s.reconcile()
	h.complete("repo-a", failResult(ErrOriginUnreachable))
	if got := h.dueIn("repo-a"); got != 15*time.Minute {
		t.Errorf("new origin: first failure next due in %v, want 15m (count restarted)", got)
	}
}

func TestSchedulerIgnoresResultsThatAreNotForTheCurrentTarget(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0)
	h.configure("repo-a")
	due := h.due("repo-a")

	h.complete("repo-a", syncResult{State: StatePending}) // cancelled: nothing happened
	h.s.completed("repo-a", syncTask{RepoID: "repo-a", CanonicalURL: key("repo-a").canonicalURL, ReplicaURL: "https://replica.example.com/stale.git"}, failResult(ErrOriginAccessDenied))
	h.s.completed("repo-unknown", syncTask{RepoID: "repo-unknown"}, failResult(ErrOriginAccessDenied))

	if got := h.due("repo-a"); !got.Equal(due) {
		t.Errorf("due moved to %v by an ignorable result, want %v", got, due)
	}
	if _, ok := h.s.nextDue("repo-unknown"); ok {
		t.Error("a result for an unknown repository created a schedule")
	}
}

func TestSchedulerDoesNothingWhileSuspended(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0)
	h.configure("repo-a", "repo-b")
	h.complete("repo-a", okResult())
	before := h.due("repo-a")
	h.setSuspended(true)
	draws := h.drawCount()

	h.setNow(schedT0.Add(48 * time.Hour))
	for range 5 {
		h.poll()
	}
	if got := h.dispatched(); len(got) != 0 {
		t.Fatalf("dispatched while suspended: %v", got)
	}
	if got := h.drawCount(); got != draws {
		t.Errorf("drew %d random values while suspended, want none", got-draws)
	}
	if got := h.due("repo-a"); !got.Equal(before) {
		t.Errorf("due moved to %v while suspended, want %v", got, before)
	}
}

// When the gate lifts, everything that fell due meanwhile must not start in the same instant.
func TestSchedulerSpreadsOverdueRepositoriesAfterSuspension(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0)
	ids := []string{"repo-a", "repo-b", "repo-c", "repo-d"}
	h.configure(ids...)
	h.setSuspended(true)
	h.setNow(schedT0.Add(48 * time.Hour))
	h.poll()

	h.setSuspended(false)
	// A different draw per repository, as production would give; the overdue repositories are
	// restaggered, in repo_id order, before any is dispatched.
	i := 0
	draws := []float64{0, 0.25, 0.5, justBelowOne}
	h.s.hooks.rand = func() float64 { v := draws[i%len(draws)]; i++; return v }
	h.poll()

	if got := h.dispatched(); !slices.Equal(got, []string{"repo-a"}) {
		t.Fatalf("dispatched %v at the moment the gate lifted, want just the one with the lowest offset", got)
	}
	for id, want := range map[string]time.Duration{"repo-b": 30 * time.Second, "repo-c": time.Minute} {
		if got := h.dueIn(id); got != want {
			t.Errorf("%s is due in %v after resume, want %v", id, got, want)
		}
	}
	if got := h.dueIn("repo-d"); got < 119*time.Second || got >= 2*time.Minute {
		t.Errorf("repo-d is due in %v after resume, want just under 2m", got)
	}
	h.advance(2 * time.Minute)
	h.poll()
	if got := h.dispatched(); len(got) != len(ids) {
		t.Errorf("after the spread window dispatched %v, want all %d repositories", got, len(ids))
	}
}

func TestSchedulerReconcileDropsRemovedAndNewcomersStartFresh(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0)
	h.configure("repo-a", "repo-b")
	h.complete("repo-b", failResult(ErrOriginAccessDenied))

	h.configure("repo-a")
	if _, ok := h.s.nextDue("repo-b"); ok {
		t.Error("a repository removed from the configuration kept a schedule")
	}
	h.advance(time.Minute)
	h.configure("repo-a", "repo-b")
	if got := h.dueIn("repo-b"); got != 15*time.Minute {
		t.Errorf("a re-added repository is due in %v, want a fresh first run in 15m", got)
	}
	h.complete("repo-b", failResult(ErrOriginUnreachable))
	if got := h.dueIn("repo-b"); got != 15*time.Minute {
		t.Errorf("first failure after re-adding: next due in %v, want 15m (no memory of the old backoff)", got)
	}
}

func TestSchedulerIntervalChangeAppliesImmediately(t *testing.T) {
	h := newSchedHarness(t)
	h.setInterval(24 * time.Hour)
	h.setRand(0)
	h.configure("repo-a")
	if got := h.dueIn("repo-a"); got != 24*time.Hour {
		t.Fatalf("due in %v, want 24h", got)
	}
	h.setInterval(time.Minute)
	if got := h.dueIn("repo-a"); got != time.Minute {
		t.Errorf("after the interval was shortened due in %v, want 1m", got)
	}
}

func TestSchedulerNonPositiveIntervalFallsBackToTheDefault(t *testing.T) {
	h := newSchedHarness(t)
	h.setInterval(0)
	h.setRand(0)
	h.configure("repo-a")
	if got := h.dueIn("repo-a"); got != defaultSyncInterval {
		t.Errorf("due in %v with a zero interval, want the default %v", got, defaultSyncInterval)
	}
}

func TestSchedulerClampsAnOutOfRangeRandomSource(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(7)
	h.configure("repo-a")
	if got := h.dueIn("repo-a"); got < 15*time.Minute || got >= 17*time.Minute {
		t.Errorf("due in %v with a broken random source, want within the stagger range", got)
	}
	h.setRand(-3)
	h.configure("repo-b")
	if got := h.dueIn("repo-b"); got != 15*time.Minute {
		t.Errorf("due in %v with a negative draw, want clamped to the interval", got)
	}
}

func TestSchedulerPollDoesNothingOnACancelledContext(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0)
	h.configure("repo-a")
	h.setNow(schedT0.Add(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.s.poll(ctx)
	if got := h.dispatched(); len(got) != 0 {
		t.Errorf("dispatched on a cancelled context: %v", got)
	}
}

func TestSchedulerDispatchesInRepoIDOrder(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0)
	h.configure("repo-c", "repo-a", "repo-b")
	h.setNow(schedT0.Add(time.Hour))
	h.poll()
	if got := h.dispatched(); !slices.Equal(got, []string{"repo-a", "repo-b", "repo-c"}) {
		t.Errorf("dispatch order = %v, want sorted by repo_id", got)
	}
}

func TestSchedulerStopWaitsForTheLoopAndIsIdempotent(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0)
	h.configure("repo-a")
	h.setNow(schedT0.Add(time.Hour))
	h.s.tick = time.Millisecond

	stop := h.s.start(context.Background())
	eventually(t, func() bool { return len(h.dispatched()) == 1 })
	stop()
	stop()

	h.advance(48 * time.Hour)
	n := len(h.dispatched())
	time.Sleep(20 * time.Millisecond)
	if got := len(h.dispatched()); got != n {
		t.Errorf("the scheduler kept dispatching after stop: %d -> %d", n, got)
	}
}

func TestSchedulerLoopExitsWhenItsContextIsCancelled(t *testing.T) {
	h := newSchedHarness(t)
	h.s.tick = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	stop := h.s.start(ctx)
	cancel()
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the scheduler loop did not exit after its context was cancelled")
	}
}

// Poll, completion, reconcile and the accessors all run on different goroutines in production.
func TestSchedulerIsSafeForConcurrentUse(t *testing.T) {
	h := newSchedHarness(t)
	h.setRand(0.5)
	ids := []string{"repo-a", "repo-b", "repo-c"}
	h.configure(ids...)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	run := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					f()
				}
			}
		}()
	}
	run(func() { h.advance(time.Minute); h.poll() })
	run(func() { h.complete("repo-a", failResult(ErrOriginUnreachable)); h.complete("repo-b", okResult()) })
	run(func() { h.configure(ids...); h.retarget("repo-c", "https://replica.example.com/x.git") })
	run(func() { h.s.nextDue("repo-a"); h.setSuspended(false) })
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// --- manager.enqueueScheduled ---

func TestManagerEnqueueScheduledStartsAnIdleRepository(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	g.release()
	m := newTestManager(t, 2, g, h)

	if !m.enqueueScheduled(context.Background(), "repo-a") {
		t.Fatal("enqueueScheduled = false for an idle, syncable repository")
	}
	if got := h.begunIDs(); !slices.Equal(got, []string{"repo-a"}) {
		t.Errorf("begin hooks = %v, want [repo-a] before it returns", got)
	}
	eventually(t, func() bool { return len(h.results("repo-a")) == 1 && m.idle() })
}

// A scheduled run that finds the repository already syncing is dropped: unlike enqueue it must not
// leave a trailing rerun behind.
func TestManagerEnqueueScheduledSkipsBusyRepositoryWithoutARerun(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	m := newTestManager(t, 2, g, h)
	m.enqueue(context.Background(), []string{"repo-a"})
	eventually(t, func() bool { return g.fetches.Load() == 1 })

	for range 3 {
		if m.enqueueScheduled(context.Background(), "repo-a") {
			t.Fatal("enqueueScheduled started a second sync of a repository already in flight")
		}
	}
	g.release()
	eventually(t, func() bool { return len(h.results("repo-a")) == 1 && m.idle() })
	time.Sleep(20 * time.Millisecond)
	if n := g.fetches.Load(); n != 1 {
		t.Errorf("fetches = %d, want 1: a scheduled skip must not cause a trailing rerun", n)
	}
	if got := h.begunIDs(); len(got) != 1 {
		t.Errorf("begin hooks = %v, want exactly one", got)
	}
}

func TestManagerEnqueueScheduledSkipsQueuedRepository(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	m := newTestManager(t, 1, g, h)
	m.enqueue(context.Background(), []string{"repo-a", "repo-b"}) // repo-b waits for the slot
	eventually(t, func() bool { return g.fetches.Load() == 1 })
	if m.enqueueScheduled(context.Background(), "repo-b") {
		t.Error("enqueueScheduled started a repository that is already queued")
	}
	g.release()
	eventually(t, func() bool { return m.idle() })
}

func TestManagerEnqueueScheduledDeclinesWhenItCannotRun(t *testing.T) {
	g, h := newGatedGit(), newHookLog()
	g.release()
	m := newTestManager(t, 2, g, h)

	h.setSkip("repo-a", true)
	if m.enqueueScheduled(context.Background(), "repo-a") {
		t.Error("enqueueScheduled = true for a repository that is not syncable")
	}
	h.suspended.Store(true)
	if m.enqueueScheduled(context.Background(), "repo-b") {
		t.Error("enqueueScheduled = true while syncing is suspended")
	}
	h.suspended.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if m.enqueueScheduled(ctx, "repo-c") {
		t.Error("enqueueScheduled = true on a cancelled context")
	}
	if got := h.begunIDs(); len(got) != 0 {
		t.Errorf("begin hooks = %v, want none: a declined schedule must leave no trace", got)
	}
	if !m.idle() {
		t.Error("manager is not idle after only declined schedules")
	}
}
