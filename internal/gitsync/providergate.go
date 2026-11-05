package gitsync

import (
	"log/slog"
	"maps"
	"sync"
	"time"
)

// manualPassGap is the least time between two immediate-sync requests for one repository that are let
// through a closed provider gate. It bounds what a request loop can cost the origin to one login per
// repository per gap.
const manualPassGap = time.Minute

// providerGate holds back the repositories of one provider credential after the origin refused it.
type providerGate struct {
	// armedAt is when the refusal was recorded. A success counts as evidence that the credential works
	// again only if its attempt began after this: one that was already running when the credential was
	// revoked may have logged in before.
	armedAt time.Time
	// until is when the gate next lets one repository try. It is also moved forward when that
	// repository is handed the turn (and again when it starts), so the others wait for its verdict.
	until time.Time
}

// providerGates is the per-credential side of retry backoff.
//
// Origin credentials belong to a local provider, not to a repository, so when one is refused every
// repository using it would otherwise keep retrying on its own schedule, each adding a failed login to
// the account's history (which can lock a service account out). A gate defers all of them together:
// a failure that says the credential itself was refused (see originCredentialRejected) closes the gate
// for the credential's binding; a success after that clears it; a changed credential is a new binding
// and never sees it.
//
// The gate holds back two things. The scheduler's dispatches are decided against it (poll), and so is
// every sync at the moment it claims its task (mayClaim), because a configuration or a restart queues
// a sync of every repository at once and the first rejection must stop the ones still waiting for a
// slot. When it elapses it is half open: one repository is given a pass to try, and its outcome
// decides. A pass is a one-shot permission for one repository to claim while the gate is closed; an
// immediate-sync request gets one too, at most once per manualPassGap.
//
// It is not persisted. The run state holds no failure counters, so per-repository backoff already
// starts over on a restart, and the only identity of a credential is binding.gen, a counter of this
// process: a gate read back from disk could not tell that the credential was rotated while the
// connector was down, and would hold back repositories that hold a working one. After a restart each
// repository may therefore try once more; the queue check keeps that to the repositories that get a
// slot before the first rejection arrives.
//
// All state is behind mu, which is a leaf lock: nothing is called while holding it, so it can be taken
// under the scheduler's lock and under the client's.
type providerGates struct {
	mu     sync.Mutex
	gates  map[binding]*providerGate
	passes map[string]binding   // repository -> the gate its pass is for
	manual map[string]time.Time // repository -> when its last immediate-sync pass was granted
}

func newProviderGates() *providerGates {
	return &providerGates{
		gates:  map[binding]*providerGate{},
		passes: map[string]binding{},
		manual: map[string]time.Time{},
	}
}

// gateState is where a binding's gate stands at a moment.
type gateState int

const (
	gateNone     gateState = iota // no gate: the binding is not held back
	gateClosed                    // held back until the gate elapses
	gateHalfOpen                  // elapsed: one repository may try
)

func (g *providerGates) size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.gates)
}

func (g *providerGates) state(b binding, now time.Time) gateState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stateLocked(b, now)
}

func (g *providerGates) stateLocked(b binding, now time.Time) gateState {
	gate, ok := g.gates[b]
	switch {
	case !ok:
		return gateNone
	case now.Before(gate.until):
		return gateClosed
	}
	return gateHalfOpen
}

// arm closes b's gate for tier from now. A repeated rejection moves the end to tier after the latest
// one, never past it, so rejections do not add up. It reports whether the gate already existed.
func (g *providerGates) arm(b binding, now time.Time, tier time.Duration) (existed bool) {
	if b.provider == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	gate, ok := g.gates[b]
	if !ok {
		gate = &providerGate{}
		g.gates[b] = gate
	}
	gate.armedAt, gate.until = now, now.Add(tier)
	return ok
}

// succeeded opens b's gate for good if the sync that succeeded began after the gate closed.
func (g *providerGates) succeeded(b binding, startedAt time.Time) (cleared bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	gate, ok := g.gates[b]
	if !ok || !startedAt.After(gate.armedAt) {
		return false
	}
	delete(g.gates, b)
	g.dropPassesLocked(b)
	return true
}

// beginProbe hands repo the turn on a half-open gate: it gets a pass and the others wait window for its
// verdict. The returned function undoes it, for when the manager turns out not to start the sync.
func (g *providerGates) beginProbe(b binding, repo string, now time.Time, window time.Duration) (undo func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	gate, ok := g.gates[b]
	if !ok {
		return func() {}
	}
	prevUntil := gate.until
	gate.until = now.Add(window)
	prevPass, hadPass := g.passes[repo]
	g.passes[repo] = b
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if gate, ok := g.gates[b]; ok && gate.until.Equal(now.Add(window)) {
			gate.until = prevUntil
		}
		if hadPass {
			g.passes[repo] = prevPass
		} else {
			delete(g.passes, repo)
		}
	}
}

// admitManual lets an immediate-sync request for repo, whose credential is b, through a closed gate: it
// grants one pass, unless repo was granted one less than manualPassGap ago. An open binding needs none.
func (g *providerGates) admitManual(b binding, repo string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.gates[b]; !ok {
		return true
	}
	if last, ok := g.manual[repo]; ok && now.Sub(last) < manualPassGap {
		return false
	}
	g.manual[repo] = now
	g.passes[repo] = b
	return true
}

// claim decides whether repo, about to start a sync with credential b, may. Without a gate it may. With
// one, only with a pass, which is used up; and the claim of a pass on a half-open gate moves its end so
// the others wait for this sync's verdict from the moment it starts rather than from when it was
// queued.
func (g *providerGates) claim(b binding, repo string, now time.Time, window time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	gate, ok := g.gates[b]
	if !ok {
		return true
	}
	if pb, ok := g.passes[repo]; !ok || pb != b {
		return false
	}
	delete(g.passes, repo)
	if !now.Before(gate.until) {
		gate.until = now.Add(window)
	}
	return true
}

// dropPass forgets repo's pass, for a sync that was granted one but did not run.
func (g *providerGates) dropPass(repo string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.passes, repo)
}

func (g *providerGates) dropPassesLocked(b binding) {
	for repo, pb := range g.passes {
		if pb == b {
			delete(g.passes, repo)
		}
	}
}

// retain keeps only what concerns the bindings in use and the repositories in repos, so a credential
// that was replaced, a provider that was removed and a repository that was deleted leave nothing behind.
func (g *providerGates) retain(inUse map[binding]struct{}, repos map[string]targetKey) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for b := range g.gates {
		if _, ok := inUse[b]; !ok {
			delete(g.gates, b)
			slog.Info("provider gate dropped: its credential is no longer in use")
		}
	}
	for repo, b := range maps.Clone(g.passes) {
		if _, ok := g.gates[b]; !ok {
			delete(g.passes, repo)
		}
	}
	for repo := range g.manual {
		if _, ok := repos[repo]; !ok {
			delete(g.manual, repo)
		}
	}
}
