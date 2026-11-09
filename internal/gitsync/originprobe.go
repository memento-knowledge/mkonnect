package gitsync

import (
	"context"
	"log/slog"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// probeRun is the origin check now running: how to stop it, and what it is checking.
type probeRun struct {
	cancel context.CancelFunc
	keys   []probeKey
}

// providerOutcome is how one provider's check ended: with a check to keep, or cancelled (which says
// nothing and keeps nothing).
type providerOutcome struct {
	name      string
	check     providerCheck
	cancelled bool
}

// maybeProbe launches an origin check on a goroutine for the providers that are due, if the connector
// has at least one local provider and is not already checking. See probeInterval for the cadence: the
// gateway health cadence while no configuration has been accepted (setup feedback), and afterwards
// every 15 minutes for a provider whose last check was clean and every 5 minutes for one that was
// not. A provider is due at once when its last check is no longer about what is configured (a first
// configuration, a changed repository URL, a replaced credential).
//
// Once a configuration has been accepted a provider is checked through a repository it lists, with
// the credential and the classifier the sync uses (see probeStateLocked); before that, and for a
// provider with no repository to dial, through its base URL.
//
// The check and its status send run under the CONNECTION context captured in OnConnected (not a
// per-heartbeat context, which the gateway cancels on return), so a heartbeat-triggered refresh is
// not killed the instant OnHeartbeat returns. Every attempt, including one that ended in a local
// fault, is recorded with its time and counts against the cadence, so a persistent fault (git
// missing) does not exec git on every heartbeat. A check is committed only while its connection is
// still the current one and still open, and the providers it was made about are still in force
// (checked together under mu): one cut short by its connection closing or by a provider reload
// stores nothing, clears nothing and sends no status. One cut short by a configuration that changed
// what it was dialling keeps the providers it had finished, if their target did not change, and
// checks the rest again at once. Cancellation
// comes from outside this package, so a connection can still close an instant after the check; a
// check that ran to completion is a true observation of the origin and may be kept then. What must
// never happen is a killed git being read as "unreachable".
func (c *Client) maybeProbe() {
	// Everything the check is made from, and the registration of how to stop it, is taken in one hold
	// of c.mu. A replacement of the providers (setProviders) and an accepted configuration also hold
	// it, so each either finds this check registered and cancels it if it is no longer about what is
	// in force, or runs first and the check is made from the new state.
	c.mu.Lock()
	accepted := c.configAccepted
	ctx := c.connCtx
	s := c.connSender
	gen := c.connGen
	if ctx == nil || s == nil || ctx.Err() != nil || len(c.providers.byName) == 0 {
		c.mu.Unlock()
		return
	}
	if !c.probing.CompareAndSwap(false, true) {
		// A check is running, and may be about something that has just changed: have it look again
		// when it ends (under mu, so it cannot end unseen between this and the flag).
		c.probePending = true
		c.mu.Unlock()
		return
	}
	due := c.dueProbesLocked(c.probeStateLocked(), c.now(), accepted)
	if len(due) == 0 {
		c.probing.Store(false)
		c.mu.Unlock()
		return
	}
	epoch := c.providerEpoch
	probeCtx, cancel := context.WithCancel(ctx)
	run := &probeRun{cancel: cancel}
	for _, p := range due {
		run.keys = append(run.keys, p.key())
	}
	c.probeRun = run
	c.mu.Unlock()

	go func() {
		rerun := false
		defer func() {
			cancel()
			c.mu.Lock()
			c.probing.Store(false)
			again := c.probePending
			c.probePending = false
			c.mu.Unlock()
			if rerun || again {
				// What the check was about changed under it: its result is discarded and the new state
				// is checked now, rather than when the cadence next allows.
				c.maybeProbe()
			}
		}()
		outcomes := make([]providerOutcome, 0, len(due))
		for _, p := range due {
			o := c.probeProvider(probeCtx, p)
			if o.cancelled {
				break
			}
			outcomes = append(outcomes, o)
		}

		c.mu.Lock()
		if c.probeRun == run {
			c.probeRun = nil
		}
		live := c.connGen == gen && ctx.Err() == nil
		current := c.providerEpoch == epoch
		cancelledHere := probeCtx.Err() != nil
		changed := false
		if live && current {
			// Providers that were fully checked before the check was cancelled are kept if they are
			// still about what is configured: they are true observations, and dialling them again would
			// only cost the origin another login.
			st := c.probeStateLocked()
			for _, o := range outcomes {
				if !st.valid(o.check.key) {
					rerun = true
					continue
				}
				c.provChecks[o.name] = o.check
			}
			rerun = rerun || cancelledHere
			changed = c.publishOriginCheckLocked()
		} else {
			rerun = live && !current
		}
		c.mu.Unlock()
		if changed {
			c.emitStatus(ctx, s, dispatchStatusTimeout)
		}
	}()
}

// cancelStaleProbeLocked stops the check now running if any provider it is dialling is no longer what
// the configuration says (see probeState.valid). The check's goroutine then checks again. c.mu must be
// held.
func (c *Client) cancelStaleProbeLocked() {
	if c.probeRun == nil {
		return
	}
	st := c.probeStateLocked()
	for _, k := range c.probeRun.keys {
		if !st.valid(k) {
			c.probeRun.cancel()
			return
		}
	}
}

// probeProvider checks one provider and says how it ended.
//
// A provider whose credential the origin has just refused (see providerGates) is not dialled: the
// check would only add another failed login every few minutes, and the answer is already known. The
// gate is consulted before every dial, because a sync can close it between two of them.
//
// With repositories to dial it checks the first, and only when that answers 403, 404 or a redirect (a
// signal about that repository) a second; a clean answer from either is a clean pass, and a
// provider-level answer (the credential refused, the server not reachable) from either is the
// provider's result at once. A 401 closes the provider's gate through armFromProbe, whether or not
// the result is later kept: the origin refused the credential, and the next check must not try it
// again.
//
// A local fault (git cannot run) gives a check with no report. An error while the context has ended
// says nothing and gives a cancelled outcome. Only the kind of fault is logged, never git's output.
func (c *Client) probeProvider(ctx context.Context, p probePlan) providerOutcome {
	o := providerOutcome{name: p.name, check: providerCheck{key: p.key()}}
	cancelled := func() providerOutcome { o.cancelled = true; return o }
	done := func(r originReport) providerOutcome {
		// held records what the check acted on: a gate a sync closed while the dial was in flight leaves a
		// reachable answer standing, which the next heartbeat then finds out of step with the gate and
		// replaces (without a dial) with the held answer.
		o.check.report, o.check.at, o.check.held = r, c.now(), r.result == OriginAccessDenied && c.sched.gates.held(p.bind)
		return o
	}
	fault := func(err error) providerOutcome {
		if ctx.Err() != nil {
			return cancelled()
		}
		slog.Warn("origin reachability check could not run", "err", err)
		o.check.fault, o.check.at = true, c.now()
		return o
	}
	held := originReport{result: OriginAccessDenied}

	if len(p.repos) == 0 {
		if c.sched.gates.held(p.bind) {
			return done(held)
		}
		res, err := c.prober.Probe(ctx, p.entry)
		if err != nil {
			return fault(err)
		}
		if ctx.Err() != nil {
			return cancelled() // a killed git is not an answer
		}
		return done(originReport{result: res, scope: proto.OriginCheckScopeHost})
	}

	var first *originReport
	var dialled []string
	// What the check is kept as being about is what it dialled: a second repository that was never
	// asked is not part of it.
	finish := func(r originReport) providerOutcome {
		if len(dialled) > 0 {
			o.check.key.urls = dialled
		}
		return done(r)
	}
	for _, t := range p.repos {
		if c.sched.gates.held(p.bind) {
			return finish(held)
		}
		started := c.now()
		dialled = append(dialled, t.url)
		rc, err := p.repo.ProbeRepository(ctx, t)
		if err != nil {
			if first != nil && ctx.Err() == nil {
				// The second repository could not be asked, but the first was answered.
				dialled = dialled[:len(dialled)-1]
				return finish(*first)
			}
			dialled = dialled[:len(dialled)-1]
			return fault(err)
		}
		if rc.CredentialRejected {
			c.armFromProbe(p.bind, started)
		}
		r := reportOf(rc)
		if r.failing() || r.errorType == "" {
			return finish(r)
		}
		if first == nil {
			first = &r
		}
	}
	return finish(*first)
}

// armFromProbe closes the gate of b after a check saw the origin refuse its credential, from when that
// check began. It does nothing when the refusal is no longer evidence about anything the connector
// syncs:
//   - no repository the connector can sync is bound to that credential any more: a configuration
//     took the last one away while the check was dialling, or the credential was replaced (a binding
//     names a generation, and repositories are rebound with the providers under c.mu). Nothing would
//     ever clear such a gate, and the provider would read access_denied until something unrelated
//     changed;
//   - a sync of a repository bound to it, that began no earlier than the check, has succeeded since,
//     which shows the credential works;
//   - its gate is already closed.
//
// It holds c.mu, and the gates' leaf lock under it, as the scheduler's own arm path does under its
// lock.
func (c *Client) armFromProbe(b binding, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sched.gates.held(b) {
		return
	}
	bound := false
	for _, r := range c.repos {
		if !r.schedulable() || r.bind != b {
			continue
		}
		bound = true
		if r.result.State == StateOK && !r.result.Stats.StartedAt.Before(at) {
			return
		}
	}
	if !bound {
		return
	}
	_, tier := c.sched.armGate(b, at)
	slog.Warn("origin rejected the provider credential in the reachability check; its repositories wait before retrying", "retry_after", tier)
}
