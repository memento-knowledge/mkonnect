package gitsync

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// These tests drive the per-provider gate through the scheduler with the harness's fake clock. With the
// default limits the interval is 15 minutes and a rejected credential waits an hour; the random draw is
// 0, so no jitter or stagger moves a due time.

var (
	gateP1 = binding{provider: "p1", gen: 1}
	gateP2 = binding{provider: "p2", gen: 1}
)

const gateTier = time.Hour

// bindRepos sets the syncable set, each repository bound as given, and reconciles.
func (h *schedHarness) bindRepos(m map[string]binding) {
	h.t.Helper()
	h.mu.Lock()
	h.repos = map[string]targetKey{}
	for id, b := range m {
		k := key(id)
		k.bind = b
		h.repos[id] = k
	}
	h.mu.Unlock()
	h.s.reconcile()
}

// finish reports a sync of id that started at started and ended now.
func (h *schedHarness) finish(id string, started time.Time, res syncResult) {
	h.t.Helper()
	res.Stats.StartedAt = started
	h.s.completed(id, h.taskFor(id), res)
}

// credentialRejected is a sync whose origin fetch was refused for the credential itself.
func credentialRejected() syncResult {
	return syncResult{State: StateError, ErrorType: ErrOriginAccessDenied, OriginCredentialRejected: true}
}

// gated sets up three repositories on p1 (a, b, c) and one on p2 (d), runs them once so they are all
// synced, then has a fail with a rejected credential. It returns the time of that failure.
func gated(t *testing.T) (*schedHarness, time.Time) {
	t.Helper()
	h := newSchedHarness(t)
	h.bindRepos(map[string]binding{"a": gateP1, "b": gateP1, "c": gateP1, "d": gateP2})
	h.advance(15 * time.Minute)
	h.poll()
	if got := h.dispatched(); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("first run dispatched %v", got)
	}
	h.resetDispatched()
	start := h.currentTime()
	h.advance(time.Second)
	h.finish("b", start, okResult())
	h.finish("c", start, okResult())
	h.finish("d", start, okResult())
	h.finish("a", start, credentialRejected())
	return h, h.currentTime()
}

func TestGateDefersTheSiblingsOfARejectedCredentialButNotOtherProviders(t *testing.T) {
	h, failedAt := gated(t)

	h.advance(20 * time.Minute) // b, c and d are due again; a waits out its own hour
	h.poll()
	if got := h.dispatched(); !slices.Equal(got, []string{"d"}) {
		t.Fatalf("dispatched %v, want only d: b and c share a's rejected credential", got)
	}

	h.setNow(failedAt.Add(gateTier - time.Second))
	h.resetDispatched()
	h.poll()
	if got := slices.DeleteFunc(h.dispatched(), func(id string) bool { return id == "d" }); len(got) != 0 {
		t.Fatalf("dispatched %v before the gate elapsed, want none of the p1 repositories", got)
	}
}

func TestGateIsOpenedOnlyByAFailureThatRejectsTheCredential(t *testing.T) {
	for name, res := range map[string]syncResult{
		"origin_access_denied for one repository": {State: StateError, ErrorType: ErrOriginAccessDenied},
		"credential_rejected (replica side)":      {State: StateError, ErrorType: ErrCredentialRejected},
		"push_token_expired":                      {State: StateError, ErrorType: ErrPushTokenExpired},
		"origin_unreachable":                      {State: StateError, ErrorType: ErrOriginUnreachable},
		"origin_url_redirected":                   {State: StateError, ErrorType: ErrOriginURLRedirected},
		"replica_rate_limited":                    {State: StateError, ErrorType: ErrReplicaRateLimited},
		"replica_repository_not_found":            {State: StateError, ErrorType: ErrReplicaRepositoryNotFound},
		"origin_empty":                            {State: StateError, ErrorType: ErrOriginEmpty},
		"a flagged result that succeeded":         {State: StateOK, OriginCredentialRejected: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newSchedHarness(t)
			h.bindRepos(map[string]binding{"a": gateP1, "b": gateP1})
			h.advance(15 * time.Minute)
			h.poll()
			h.resetDispatched()
			start := h.currentTime()
			h.advance(time.Second)
			h.finish("b", start, okResult())
			h.finish("a", start, res)

			h.advance(20 * time.Minute)
			h.poll()
			if got := h.dispatched(); !slices.Contains(got, "b") {
				t.Fatalf("b was held back by %s: dispatched %v", name, got)
			}
		})
	}
}

func TestGateIgnoresAnUnboundRepository(t *testing.T) {
	h := newSchedHarness(t)
	h.bindRepos(map[string]binding{"a": {}, "b": {}})
	h.advance(15 * time.Minute)
	h.poll()
	h.resetDispatched()
	start := h.currentTime()
	h.advance(time.Second)
	h.finish("b", start, okResult())
	h.finish("a", start, credentialRejected())
	h.advance(20 * time.Minute)
	h.poll()
	if got := h.dispatched(); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("dispatched %v, want b: the zero binding is no provider", got)
	}
}

func TestGateLetsExactlyOneRepositoryProbeWhenItElapses(t *testing.T) {
	h, failedAt := gated(t)
	h.setNow(failedAt.Add(gateTier))
	h.resetDispatched()
	h.poll()
	got := h.dispatched()
	p1 := slices.DeleteFunc(slices.Clone(got), func(id string) bool { return id == "d" })
	if !slices.Equal(p1, []string{"b"}) {
		t.Fatalf("dispatched %v, want exactly one p1 repository, b: a failed before and c is later in order", got)
	}

	// While the probe is out nobody else on p1 goes, however often it is polled.
	for range 10 {
		h.advance(5 * time.Second)
		h.resetDispatched()
		h.poll()
		if got := h.dispatched(); slices.Contains(got, "a") || slices.Contains(got, "c") {
			t.Fatalf("dispatched %v while the probe was out", got)
		}
	}
}

func TestGateProbePrefersARepositoryThatHasNotJustFailed(t *testing.T) {
	h, failedAt := gated(t)
	// a sorts first, but it is the repository whose failure opened the gate.
	h.setNow(failedAt.Add(gateTier))
	h.resetDispatched()
	h.poll()
	if slices.Contains(h.dispatched(), "a") {
		t.Fatalf("a probed its own gate: %v", h.dispatched())
	}
}

func TestGateProbeSuccessClearsTheGate(t *testing.T) {
	h, failedAt := gated(t)
	h.setNow(failedAt.Add(gateTier))
	h.poll()
	h.resetDispatched()
	start := h.currentTime()
	h.advance(10 * time.Second)
	h.finish("b", start, okResult())

	h.advance(time.Minute)
	h.poll()
	got := h.dispatched()
	for _, id := range []string{"a", "c"} {
		if !slices.Contains(got, id) {
			t.Fatalf("dispatched %v after the probe succeeded, want %s to be released", got, id)
		}
	}
}

func TestGateProbeFailureRearmsFromThatFailureAndDoesNotStack(t *testing.T) {
	h, failedAt := gated(t)
	h.setNow(failedAt.Add(gateTier))
	h.poll()
	h.resetDispatched()
	start := h.currentTime()
	h.advance(10 * time.Second)
	rearmed := h.currentTime()
	h.finish("b", start, credentialRejected())

	// One tier from the new failure, not from the end of the old gate and not a second tier on top.
	h.setNow(rearmed.Add(gateTier - time.Second))
	h.poll()
	if got := h.dispatched(); slices.Contains(got, "c") {
		t.Fatalf("dispatched %v before the re-armed gate elapsed", got)
	}
	h.setNow(rearmed.Add(gateTier + 6*time.Second)) // one scheduler tick of slack
	h.poll()
	if got := h.dispatched(); !slices.Contains(got, "c") {
		t.Fatalf("dispatched %v one tier after the second rejection, want c to probe", got)
	}
}

func TestGateSiblingsRunAtTheGateNotAfterTheirOwnBackoffOnTopOfIt(t *testing.T) {
	h := newSchedHarness(t)
	h.bindRepos(map[string]binding{"a": gateP1, "b": gateP1})
	h.advance(15 * time.Minute)
	h.poll()
	h.resetDispatched()
	start := h.currentTime()
	h.advance(time.Second)
	h.finish("b", start, okResult())
	failedAt := h.currentTime()
	h.finish("a", start, credentialRejected())

	// b was due long ago; nothing it does while deferred may push it past the gate.
	for h.currentTime().Before(failedAt.Add(gateTier - 10*time.Second)) {
		h.advance(5 * time.Second)
		h.poll()
	}
	if got := h.dispatched(); len(got) != 0 {
		t.Fatalf("dispatched %v while the gate was closed", got)
	}
	h.setNow(failedAt.Add(gateTier))
	h.poll()
	if got := h.dispatched(); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("dispatched %v at the moment the gate elapsed, want b", got)
	}
}

func TestGateProbeThatCannotBeStartedLeavesTheNextRepositoryToTry(t *testing.T) {
	h, failedAt := gated(t)
	h.setBusy("b", true) // the manager is already running it
	h.setNow(failedAt.Add(gateTier))
	h.resetDispatched()
	h.poll()
	p1 := slices.DeleteFunc(h.dispatched(), func(id string) bool { return id == "d" })
	if !slices.Equal(p1, []string{"c"}) {
		t.Fatalf("dispatched %v, want c to take the probe b could not", p1)
	}
}

func TestGateProbeThatNeverReportsDoesNotWedgeIt(t *testing.T) {
	h, failedAt := gated(t)
	h.setNow(failedAt.Add(gateTier))
	h.poll() // b is the probe and is then cancelled: no result ever comes
	h.resetDispatched()
	h.advance(15*time.Minute + 5*time.Second)
	h.poll()
	p1 := slices.DeleteFunc(h.dispatched(), func(id string) bool { return id == "d" })
	if len(p1) != 1 {
		t.Fatalf("dispatched %v, want one new probe an interval after the lost one", p1)
	}
}

func TestGateSuccessOfASyncThatBeganBeforeTheGateDoesNotClearIt(t *testing.T) {
	h := newSchedHarness(t)
	h.bindRepos(map[string]binding{"a": gateP1, "b": gateP1, "c": gateP1})
	h.advance(15 * time.Minute)
	h.poll()
	h.resetDispatched()
	long := h.currentTime() // b's fetch logs in now with the old credential and runs on
	h.advance(time.Second)
	started := h.currentTime()
	h.finish("c", started, okResult())
	h.finish("a", started, credentialRejected())
	h.advance(time.Minute)
	h.finish("b", long, okResult()) // finishes after the gate opened, but began before it

	h.advance(20 * time.Minute)
	h.poll()
	if got := h.dispatched(); len(got) != 0 {
		t.Fatalf("dispatched %v: a success from before the rejection says nothing about the credential now", got)
	}
}

func TestGateCredentialChangeReleasesTheProvider(t *testing.T) {
	h, _ := gated(t)
	h.advance(20 * time.Minute)
	h.resetDispatched()
	h.poll()
	if slices.Contains(h.dispatched(), "b") {
		t.Fatal("b should still be deferred")
	}

	gen2 := binding{provider: "p1", gen: 2}
	h.bindRepos(map[string]binding{"a": gen2, "b": gen2, "c": gen2, "d": gateP2})
	if n := h.s.gates.size(); n != 0 {
		t.Fatalf("%d gates survive a credential change", n)
	}
	h.s.completed("b", h.taskFor("b"), okResult()) // the resync the reload starts
	h.advance(time.Hour)
	h.resetDispatched()
	h.poll()
	for _, id := range []string{"a", "b", "c"} {
		if !slices.Contains(h.dispatched(), id) {
			t.Fatalf("dispatched %v after the credential changed, want %s", h.dispatched(), id)
		}
	}
}

func TestGateSurvivesAReloadThatLeavesTheProviderAlone(t *testing.T) {
	h, _ := gated(t)
	// d's provider rotates; p1 is untouched, so it keeps its generation and its gate.
	h.bindRepos(map[string]binding{"a": gateP1, "b": gateP1, "c": gateP1, "d": {provider: "p2", gen: 2}})
	if n := h.s.gates.size(); n != 1 {
		t.Fatalf("%d gates after an unrelated reload, want the one for p1", n)
	}
}

func TestGateForgetsAProviderNoRepositoryUsesAnymore(t *testing.T) {
	h, _ := gated(t)
	h.bindRepos(map[string]binding{"d": gateP2})
	if n := h.s.gates.size(); n != 0 {
		t.Fatalf("%d gates for a provider nothing uses", n)
	}
}

func TestGateResultForASupersededCredentialDoesNotArmIt(t *testing.T) {
	h := newSchedHarness(t)
	h.bindRepos(map[string]binding{"a": {provider: "p1", gen: 2}})
	h.advance(15 * time.Minute)
	h.poll()
	task := h.taskFor("a")
	task.Bind = gateP1 // the attempt used generation 1, which has been replaced
	res := credentialRejected()
	res.Stats.StartedAt = h.currentTime()
	h.s.completed("a", task, res)
	if n := h.s.gates.size(); n != 0 {
		t.Fatalf("%d gates armed by a result for a credential no longer in force", n)
	}
}

// What a queued sync does once its provider is gated: the claim check is what stops the sync a
// configuration or a restart queued for every repository behind the first rejected login.
func TestGateBlocksQueuedSyncsAtClaimTime(t *testing.T) {
	h, _ := gated(t)
	for _, id := range []string{"a", "b", "c"} {
		if h.s.mayClaim(id, gateP1) {
			t.Fatalf("%s was allowed to claim while its provider is gated", id)
		}
	}
	if !h.s.mayClaim("d", gateP2) {
		t.Fatal("d, on another provider, was held back")
	}
	if !h.s.mayClaim("e", binding{}) {
		t.Fatal("an unbound repository was held back")
	}
}

func TestGateImmediateSyncPassesOnceForThatRepositoryOnly(t *testing.T) {
	h, _ := gated(t)
	if !h.s.admitManual("b") {
		t.Fatal("git_sync_now for b was refused")
	}
	if h.s.mayClaim("c", gateP1) {
		t.Fatal("a pass for b let c through")
	}
	if !h.s.mayClaim("b", gateP1) {
		t.Fatal("b's pass was not honoured")
	}
	if h.s.mayClaim("b", gateP1) {
		t.Fatal("b's pass was honoured twice")
	}
}

func TestGateImmediateSyncIsRateLimitedPerRepository(t *testing.T) {
	h, _ := gated(t)
	if !h.s.admitManual("b") || !h.s.mayClaim("b", gateP1) {
		t.Fatal("first request was not honoured")
	}
	h.advance(manualPassGap - time.Second)
	if h.s.admitManual("b") {
		t.Fatal("a second request within the gap passed")
	}
	h.advance(2 * time.Second)
	if !h.s.admitManual("b") {
		t.Fatal("a request after the gap was refused")
	}
}

func TestGateImmediateSyncOnAnUngatedProviderNeedsNoPass(t *testing.T) {
	h := newSchedHarness(t)
	h.bindRepos(map[string]binding{"a": gateP1})
	if !h.s.admitManual("a") || !h.s.mayClaim("a", gateP1) || !h.s.mayClaim("a", gateP1) {
		t.Fatal("an ungated repository was held back")
	}
	if !h.s.admitManual("not-scheduled") {
		t.Fatal("a repository the scheduler does not know was refused")
	}
}

func TestGateImmediateSyncThatSucceedsClearsIt(t *testing.T) {
	h, _ := gated(t)
	if !h.s.admitManual("b") || !h.s.mayClaim("b", gateP1) {
		t.Fatal("not admitted")
	}
	h.advance(time.Second)
	start := h.currentTime()
	h.advance(5 * time.Second)
	h.finish("b", start, okResult())
	if !h.s.mayClaim("c", gateP1) {
		t.Fatal("a successful manual sync should have lifted the gate for c")
	}
}

func TestGateAPassIsDroppedWhenItsSyncDoesNotRun(t *testing.T) {
	h, _ := gated(t)
	h.s.admitManual("b")
	h.s.releasePass("b") // cancelled while queued
	if h.s.mayClaim("b", gateP1) {
		t.Fatal("a later scheduled claim used a pass granted to an earlier request")
	}
}

func TestGateClaimingAProbePassHoldsTheOthersForTheVerdict(t *testing.T) {
	h, failedAt := gated(t)
	h.setNow(failedAt.Add(gateTier))
	h.poll() // b gets the probe pass
	// The probe waited in the queue for longer than an interval before it claimed.
	h.advance(20 * time.Minute)
	if !h.s.mayClaim("b", gateP1) {
		t.Fatal("the probe was refused its own pass")
	}
	h.advance(time.Minute)
	h.resetDispatched()
	h.poll()
	if got := h.dispatched(); slices.Contains(got, "c") {
		t.Fatalf("dispatched %v: the window opens when the probe claims, so c waits for it", got)
	}
}

func TestGateIsSafeForConcurrentUse(t *testing.T) {
	h := newSchedHarness(t)
	ids := map[string]binding{"a": gateP1, "b": gateP1, "c": gateP2}
	h.bindRepos(ids)
	var wg sync.WaitGroup
	run := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				f(i)
			}
		}()
	}
	run(func(int) { h.advance(30 * time.Second); h.poll() })
	run(func(i int) {
		h.s.completed("a", h.taskFor("a"), syncResult{State: StateError, ErrorType: ErrOriginAccessDenied, OriginCredentialRejected: true, Stats: syncStats{StartedAt: h.currentTime()}})
	})
	run(func(i int) {
		h.s.completed("b", h.taskFor("b"), syncResult{State: StateOK, Stats: syncStats{StartedAt: h.currentTime()}})
	})
	run(func(int) { h.s.admitManual("b") })
	run(func(int) { h.s.mayClaim("b", gateP1); h.s.releasePass("b") })
	run(func(i int) {
		if i%20 == 0 {
			h.bindRepos(ids)
		}
	})
	wg.Wait()
}

// An immediate sync that passes a closed gate must not postpone the probe: a repository that keeps
// failing for its own reasons would otherwise hold the whole provider back for as long as someone
// presses the button.
func TestGateImmediateSyncDoesNotPostponeTheProbe(t *testing.T) {
	h, failedAt := gated(t)
	h.advance(gateTier - 5*time.Minute) // a claim now would reach past the end of the gate
	if !h.s.admitManual("b") || !h.s.mayClaim("b", gateP1) {
		t.Fatal("not admitted")
	}
	h.setNow(failedAt.Add(gateTier))
	h.resetDispatched()
	h.poll()
	p1 := slices.DeleteFunc(h.dispatched(), func(id string) bool { return id == "d" })
	if len(p1) != 1 {
		t.Fatalf("dispatched %v when the gate elapsed, want one probe", p1)
	}
}

// A pass belongs to one request for one repository. If the repository is removed and a repository of
// the same ID is added again while the gate stays closed, the old pass must not let it through.
func TestGateRemovedRepositoryLeavesNoPassBehind(t *testing.T) {
	h, _ := gated(t)
	if !h.s.admitManual("b") {
		t.Fatal("not admitted")
	}
	h.bindRepos(map[string]binding{"a": gateP1, "c": gateP1, "d": gateP2}) // b removed; p1 stays gated
	h.bindRepos(map[string]binding{"a": gateP1, "b": gateP1, "c": gateP1, "d": gateP2})
	if h.s.mayClaim("b", gateP1) {
		t.Fatal("a pass granted to the removed b let the re-added b through the closed gate")
	}
}
