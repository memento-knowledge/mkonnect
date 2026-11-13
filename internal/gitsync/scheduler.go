package gitsync

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"
)

const (
	// schedulerTick is how often the scheduler looks for repositories that are due. It is far below
	// the shortest accepted interval (a minute), so it only bounds how late a sync can start.
	schedulerTick = 5 * time.Second

	// maxStagger bounds the spread of a repository's first scheduled run: it falls uniformly in
	// [interval, interval+min(interval, maxStagger)), so a configuration of many repositories does
	// not make them all fall due together. The same spread is applied when syncing resumes after a
	// suspension.
	maxStagger = 2 * time.Minute

	// backoffCap is how far consecutive failures stretch the delay: it doubles per failure up to this,
	// and never drops below the interval. The jitter added on top (see delay) can lengthen it by 20%.
	backoffCap = time.Hour
)

// credentialErrors are the failures retrying sooner than backoffCap cannot fix: the credential or
// the configuration has to change first, and a retry only adds a failed login to the account's
// history (which can lock a service account out). They wait max(interval, backoffCap) from the
// first failure rather than climbing the ladder. A failure that also needs a fix but adds no failed
// login stays on the ladder: origin_url_redirected (a corrected URL is a new target and starts over
// at once), replica_rate_limited and replica_repository_not_found. origin_empty is no failed login
// either (the fetch was served), and a retry is what picks up the first commit.
var credentialErrors = []string{ErrOriginAccessDenied, ErrCredentialRejected, ErrPushTokenExpired}

// targetKey identifies what a repository syncs from and to, the same identity targetMatches uses. A
// repository whose key changes is a new sync target: its schedule and backoff start over, whereas a
// configuration that re-sends the same key (a push-token refresh) leaves them alone.
type targetKey struct {
	canonicalURL string
	replicaURL   string
	// bind is the provider credential the repository syncs with: a failure history built up under one
	// credential says nothing about another, so a repository bound to a new one starts over.
	bind binding
}

// schedHooks connect the scheduler to the connector without it knowing about the Client.
type schedHooks struct {
	// repos returns the repositories that can be synced and their sync targets.
	repos func() map[string]targetKey
	// enqueue starts a scheduled sync of one repository and reports whether it did. It must not start
	// one for a repository that is already syncing, and must not queue a rerun.
	enqueue func(ctx context.Context, repoID string) bool
	// suspended reports whether syncing is currently suspended (a critical update is required).
	suspended func() bool
	// limits returns the effective limits; only the interval is read, and on every use, so a changed
	// interval applies at once.
	limits func() Limits
	// now returns the current time. For production it must be time.Now unmodified, so due times keep
	// their monotonic reading and survive a wall-clock step. now and limits are also called by the
	// claim check, with the client's lock held, so they must never take it.
	now func() time.Time
	// rand returns a value in [0, 1) for jitter and staggering.
	rand func() float64
}

// schedEntry is the schedule of one repository. The next-due time is derived from it on demand (see
// due) rather than stored, so a changed interval applies immediately.
type schedEntry struct {
	key targetKey
	// base is when the delay is measured from: the time the repository was configured, then the time
	// of its last sync or dispatch.
	base time.Time
	// r is the random draw behind this entry's jitter or stagger, redrawn whenever base moves.
	r float64
	// staggered selects the spread for a repository's first scheduled run: true from configuration
	// until its second sync completes, and not at all once it has failed.
	staggered bool
	// synced records that a sync has completed since the target was configured.
	synced bool
	// failures counts consecutive failed syncs and lastErr is the latest one's error type.
	failures int
	lastErr  string
	// notBefore, when set, holds the entry back after a suspension so that repositories that fell
	// due meanwhile do not all start at once.
	notBefore time.Time
}

// scheduler re-syncs configured repositories periodically: the platform sends no timer and never
// sends a scheduled git_sync_now, so without it nothing syncs after the first run following a
// configuration.
//
// One goroutine ticks; each tick starts the repositories whose next-due time has passed. The next
// run of a repository is its last completion plus the interval (with jitter); consecutive failures
// stretch it (see backoffFloor). A repository that is already syncing is skipped, never queued
// behind itself. The repositories that share a credential the origin refused are held back together
// (see providerGates).
//
// The mutex is held while hooks.enqueue runs, so a dispatch and the entry update that follows it
// are atomic with respect to a completion. The lock order is always scheduler -> client/manager,
// never the reverse, and enqueue never blocks (it hands the sync to a goroutine). completed is
// called by the sync's goroutine with no other lock held.
type scheduler struct {
	hooks schedHooks
	tick  time.Duration

	mu           sync.Mutex
	entries      map[string]*schedEntry
	wasSuspended bool

	// gates holds back the repositories of a credential the origin refused; see providerGates. It has
	// its own lock, below mu in the lock order, so the manager's claim check can reach it too.
	gates *providerGates
}

func newScheduler(h schedHooks) *scheduler {
	return &scheduler{hooks: h, tick: schedulerTick, entries: map[string]*schedEntry{}, gates: newProviderGates()}
}

// start runs the scheduling loop until ctx is cancelled or the returned stop is called. stop waits
// for the loop to exit and is safe to call more than once. ctx is the process-lifetime context: it
// also bounds the syncs the scheduler starts, exactly as for the ones a configuration starts, so
// stopping the loop does not cancel a sync already running.
func (s *scheduler) start(ctx context.Context) (stop func()) {
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(s.tick)
		defer t.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-t.C:
				s.poll(ctx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// reconcile brings the schedules in line with the repositories that can be synced. A repository that
// is gone is forgotten; one that is new, or whose sync target changed, starts fresh with a staggered
// first run; one whose target is unchanged is left exactly as it was, including any backoff.
func (s *scheduler) reconcile() {
	want := s.hooks.repos()

	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.entries {
		if _, ok := want[id]; !ok {
			delete(s.entries, id)
		}
	}
	now := s.hooks.now()
	for _, id := range slices.Sorted(maps.Keys(want)) {
		if e, ok := s.entries[id]; ok && e.key == want[id] {
			continue
		}
		s.entries[id] = &schedEntry{key: want[id], base: now, r: s.draw(), staggered: true}
	}
	// A replaced credential is a new binding, so its repositories never see the old gate; this only
	// stops the gates of bindings nothing uses from piling up.
	inUse := make(map[binding]struct{}, len(want))
	for _, k := range want {
		inUse[k.bind] = struct{}{}
	}
	s.gates.retain(inUse, want)
}

// completed records a finished sync. A cancelled sync (State pending) says nothing about the
// repository and is ignored, as is a result for a repository, or a target, that is no longer
// scheduled.
func (s *scheduler) completed(repoID string, t syncTask, res syncResult) {
	if res.State != StateOK && res.State != StateError {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[repoID]
	if !ok || e.key != (targetKey{canonicalURL: t.CanonicalURL, replicaURL: t.ReplicaURL, bind: t.Bind}) {
		return
	}
	success := res.State == StateOK
	now := s.hooks.now()
	if success {
		e.failures, e.lastErr = 0, ""
		if s.gates.succeeded(e.key.bind, res.Stats.StartedAt) {
			slog.Info("provider credential accepted again; its repositories resume", "repo_id", repoID)
		}
	} else {
		e.failures++
		e.lastErr = res.ErrorType
		if res.OriginCredentialRejected {
			existed, tier := s.armGate(e.key.bind, now)
			if existed {
				slog.Info("origin rejected the provider credential again; its repositories keep waiting", "repo_id", repoID, "retry_after", tier)
			} else {
				slog.Warn("origin rejected the provider credential; its other repositories wait before retrying", "repo_id", repoID, "retry_after", tier)
			}
		}
	}
	// The first sync after configuration is the immediate one the configuration triggered. Its
	// success keeps the staggered spread, so repositories configured together do not fall due
	// together just because their first syncs finished at similar times.
	e.staggered = success && !e.synced
	e.synced = true
	e.base, e.r = now, s.draw()
}

// armGate closes b's gate from at, for the tier that follows a rejected credential: max(interval,
// backoffCap). It reports whether the gate already existed. The scheduler's sync completions and the
// origin reachability check both use it, so a rejection seen by either is the same evidence.
//
// It takes only the gates' own lock, which is a leaf, and reads the limits through a hook that must
// not take a lock: never s.mu. The reachability check calls it with the client's lock held, and the
// lock order is scheduler -> manager -> client, so taking s.mu here would invert it.
func (s *scheduler) armGate(b binding, at time.Time) (existed bool, tier time.Duration) {
	tier = max(effectiveInterval(s.hooks.limits()), backoffCap)
	return s.gates.arm(b, at, tier), tier
}

// poll starts every repository that is due. While syncing is suspended it does nothing at all: it
// reads no schedule and advances none, so the work waiting behind the gate is neither lost nor
// spun on. When the gate lifts, what fell due meanwhile is spread out rather than started together.
func (s *scheduler) poll(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	suspended := s.hooks.suspended()

	s.mu.Lock()
	defer s.mu.Unlock()
	if suspended {
		s.wasSuspended = true
		return
	}
	now, l := s.hooks.now(), s.hooks.limits()
	due := s.dueIDs(now, l)
	if s.wasSuspended {
		s.wasSuspended = false
		spread := min(effectiveInterval(l), maxStagger)
		for _, id := range due {
			s.entries[id].notBefore = now.Add(time.Duration(s.draw() * float64(spread)))
		}
		due = s.dueIDs(now, l)
	}
	// A repository whose credential the origin refused waits for the gate. When the gate has elapsed
	// the repositories behind it are candidates for its single probe, tried after the rest.
	var halfOpen []string
	for _, id := range due {
		switch s.gates.state(s.entries[id].key.bind, now) {
		case gateClosed:
			continue
		case gateHalfOpen:
			halfOpen = append(halfOpen, id)
			continue
		}
		s.dispatch(ctx, id, now, nil)
	}
	// Prefer a repository that has not itself just failed: if the credential is fine and the rejection
	// was about one repository, the probe that succeeds is what releases the others.
	slices.SortStableFunc(halfOpen, func(a, b string) int { return s.entries[a].failures - s.entries[b].failures })
	for _, id := range halfOpen {
		b := s.entries[id].key.bind
		if s.gates.state(b, now) != gateHalfOpen {
			continue // a probe on this credential was already started this tick
		}
		window := effectiveInterval(l)
		undo := s.gates.beginProbe(b, id, now, window)
		if !s.dispatch(ctx, id, now, undo) {
			continue
		}
		slog.Info("probing a provider credential the origin refused", "repo_id", id)
	}
}

// dispatch starts a scheduled sync of id and reports whether the manager did. If it did not, undo (if
// any) takes back what was granted for it.
func (s *scheduler) dispatch(ctx context.Context, id string, now time.Time, undo func()) bool {
	if !s.hooks.enqueue(ctx, id) {
		if undo != nil {
			undo()
		}
		return false
	}
	slog.Debug("scheduled sync started", "repo_id", id)
	// Measure from the dispatch until the run reports, so a run that never does (it was released, or
	// cancelled) is not dispatched again on every tick.
	e := s.entries[id]
	e.base, e.r, e.notBefore = now, s.draw(), time.Time{}
	return true
}

// dueIDs returns, sorted, the repositories whose next run is at or before now. Only those are
// sorted, so a tick where nothing is due costs one pass over the entries.
func (s *scheduler) dueIDs(now time.Time, l Limits) []string {
	var ids []string
	for id, e := range s.entries {
		if !now.Before(e.due(l)) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// nextDue reports when repoID is next due, or false when it is not scheduled.
func (s *scheduler) nextDue(repoID string) (time.Time, bool) {
	l := s.hooks.limits()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[repoID]
	if !ok {
		return time.Time{}, false
	}
	return e.due(l), true
}

// admitManual reports whether an immediate-sync request for repoID may start a sync now, and if its
// provider's gate is closed grants the one pass that lets exactly that sync claim (see
// providerGates.admitManual). A repository the scheduler does not know is not held back. The request
// is not a dispatch, so it neither advances the repository's schedule nor is held to it.
func (s *scheduler) admitManual(repoID string) bool {
	s.mu.Lock()
	e, ok := s.entries[repoID]
	var b binding
	if ok {
		b = e.key.bind
	}
	s.mu.Unlock()
	if !ok {
		return true
	}
	admitted := s.gates.admitManual(b, repoID, s.hooks.now())
	if !admitted {
		slog.Warn("immediate sync not started: the provider credential was rejected and this repository was just let through once", "repo_id", repoID)
	}
	return admitted
}

// mayClaim reports whether repoID may start a sync with the credential b now: it may unless that
// credential's gate is closed and the repository has no pass. The manager asks as a sync claims its
// task, so a sync that waited for a slot sees a rejection that arrived while it waited.
func (s *scheduler) mayClaim(repoID string, b binding) bool {
	if !s.gates.claim(b, repoID, s.hooks.now(), effectiveInterval(s.hooks.limits())) {
		slog.Info("sync deferred: the provider credential was rejected by the origin", "repo_id", repoID)
		return false
	}
	return true
}

// releasePass drops the pass of a repository whose sync did not run.
func (s *scheduler) releasePass(repoID string) { s.gates.dropPass(repoID) }

// draw returns a random value in [0, 1), whatever the source returns.
func (s *scheduler) draw() float64 {
	r := s.hooks.rand()
	switch {
	case !(r > 0): // also NaN
		return 0
	case r >= 1:
		return 1 - 1e-9
	}
	return r
}

// effectiveInterval is the effective re-sync interval. A non-positive value (which validation already
// rejects) falls back to the default instead of making every repository permanently due.
func effectiveInterval(l Limits) time.Duration {
	if l.Interval <= 0 {
		return defaultSyncInterval
	}
	return l.Interval
}

// due is when the entry is next due under the limits l.
func (e *schedEntry) due(l Limits) time.Time {
	return later(e.base.Add(e.delay(l)), e.notBefore)
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// delay is how long after base the entry falls due:
//   - after a failure, the backoff floor plus up to 20% on top. Jitter only ever lengthens a retry:
//     shortening it below the floor would defeat the delay;
//   - for the first scheduled run, a uniform draw from [interval, interval+min(interval, maxStagger));
//   - otherwise the interval +-10%.
func (e *schedEntry) delay(l Limits) time.Duration {
	iv := effectiveInterval(l)
	switch {
	case e.failures > 0:
		floor := backoffFloor(iv, e.failures, e.lastErr)
		return floor + time.Duration(e.r*float64(floor/5))
	case e.staggered:
		return iv + time.Duration(e.r*float64(min(iv, maxStagger)))
	default:
		return iv - iv/10 + time.Duration(e.r*float64(iv/5))
	}
}

// backoffFloor is the shortest delay before retrying after failures consecutive failures, the last
// of which had error type errType: max(interval, min(interval*2^(failures-1), backoffCap)), or
// max(interval, backoffCap) from the first failure for a credential error.
func backoffFloor(iv time.Duration, failures int, errType string) time.Duration {
	if slices.Contains(credentialErrors, errType) {
		return max(iv, backoffCap)
	}
	d := iv
	// The loop stops doubling once d reaches the cap, so it cannot overflow however many failures.
	for i := 1; i < failures && d < backoffCap; i++ {
		d *= 2
	}
	return max(iv, min(d, backoffCap))
}
