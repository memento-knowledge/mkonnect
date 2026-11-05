package gitsync

import (
	"context"
	"log/slog"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gateway"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// originCheckInterval is the minimum spacing between origin reachability probes once a configuration
// has been accepted. Before that the probe runs at the gateway health cadence, so setup feedback is
// quick; afterwards the result only needs to stay fresh, and a probe is a git subprocess talking to
// the origin.
const originCheckInterval = 5 * time.Minute

// maybeProbe launches an origin reachability probe on a goroutine if the connector has at least one
// local provider, is not already probing, and the previous probe started long enough ago: the
// gateway health cadence while no configuration has been accepted (setup feedback), originCheckInterval
// afterwards. The probe and its status send run under the CONNECTION context captured in OnConnected
// (not a per-heartbeat context, which the gateway cancels on return), so a heartbeat-triggered
// refresh is not killed the instant OnHeartbeat returns. The throttle applies to every attempt —
// including failed ones — so a persistent local fault (e.g. git missing) does not exec git on every
// heartbeat. A probe is committed only while its connection is still the current one and still open
// (checked together under mu, with the store): one cut short by its connection closing, or
// overtaken by a newer connection, never caches a result, clears one, or sends a status, and it
// does not count against the cadence, so the next connection probes at its first opportunity.
// Cancellation comes from outside this package, so a connection can still close an instant after
// the check; a probe that ran to completion is a true observation of the origin and may be kept
// then. What must never happen is a killed git being read as "unreachable".
func (c *Client) maybeProbe() {
	// Everything the probe is made from, and the registration of how to stop it, is taken in one hold
	// of c.mu. A replacement of the providers (setProviders) also holds it, so it either finds this
	// probe registered and cancels it, or runs first and the probe is made from the new set: a probe
	// can never be running with a provider that was already replaced and out of reach of the
	// cancellation.
	c.mu.Lock()
	accepted := c.configAccepted
	ctx := c.connCtx
	s := c.connSender
	gen := c.connGen
	if ctx == nil || s == nil || ctx.Err() != nil || len(c.providers.byName) == 0 {
		c.mu.Unlock()
		return
	}
	spacing := gateway.DefaultHeartbeatInterval
	if accepted {
		spacing = originCheckInterval
	}
	if !c.probing.CompareAndSwap(false, true) {
		c.mu.Unlock()
		return
	}
	start := c.now().UnixNano()
	// Heartbeats do not arrive at exactly the health interval, so a probe is due a tenth early; a
	// tick that lands just under the spacing would otherwise push the probe out to the next one.
	// A negative gap means the clock stepped back, which must not stall the cadence until it
	// catches up: it counts as elapsed.
	if since := time.Duration(start - c.lastProbe.Load()); since >= 0 && since < spacing-spacing/10 {
		c.probing.Store(false)
		c.mu.Unlock()
		return
	}
	c.lastProbe.Store(start)
	targets := c.providers.probeTargets()
	epoch := c.providerEpoch
	probeCtx, cancel := context.WithCancel(ctx)
	c.probeCancel = cancel
	c.mu.Unlock()

	go func() {
		stale := false
		defer func() {
			cancel()
			c.probing.Store(false)
			if stale {
				// The providers changed under this probe: its result is discarded and the new set is
				// probed now, rather than when the cadence next allows.
				c.maybeProbe()
			}
		}()
		res, ok := c.runProbe(probeCtx, targets)
		// Commit only while this probe's connection is still the current one and still open, and the
		// providers it probed are still the ones in force. A probe cut short by its connection
		// closing, or one overtaken by a newer connection or a changed provider set, changes nothing
		// and gives its cadence slot back, so the next probe is not held off by it.
		c.mu.Lock()
		live := c.connGen == gen && ctx.Err() == nil
		current := c.providerEpoch == epoch
		changed := false
		if live && current {
			if ok {
				c.originCheck.Store(&proto.OriginCheck{Result: string(res), CheckedAt: c.now().UTC().Format(rfc3339Nano)})
				changed = true
			} else {
				// A local fault says nothing about the origin, so the previous result can no longer
				// be vouched for: drop it rather than keep reporting it as current.
				changed = c.originCheck.Swap(nil) != nil
			}
		}
		c.mu.Unlock()
		if !live || !current {
			c.lastProbe.CompareAndSwap(start, 0)
			stale = live && !current
			return
		}
		if changed {
			c.emitStatus(ctx, s, dispatchStatusTimeout)
		}
	}()
}

// runProbe probes each of targets and aggregates worst-to-best (access_denied over unreachable over
// reachable). A provider whose credential the origin has just refused (see providerGates) is not
// dialled: the check would only add another failed login every few minutes, and the answer is already
// known. ok is false when no provider yielded a usable result (for example git is not
// installed): a local fault, which maybeProbe logs through here and answers by leaving origin_check
// absent. A provider that faults while another yields a result is skipped, so the aggregate covers
// only the providers that could be checked. A probe whose context ended says nothing and logs nothing.
func (c *Client) runProbe(ctx context.Context, targets []probeTarget) (OriginResult, bool) {
	rank := map[OriginResult]int{OriginReachable: 0, OriginUnreachable: 1, OriginAccessDenied: 2}
	worst := OriginReachable
	any := false
	for _, t := range targets {
		if ctx.Err() != nil {
			return "", false
		}
		res := OriginAccessDenied
		var err error
		if !c.sched.gates.held(t.bind) {
			res, err = c.prober.Probe(ctx, t.entry)
		}
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("origin reachability check could not run", "err", err)
			}
			continue
		}
		any = true
		if rank[res] > rank[worst] {
			worst = res
		}
	}
	return worst, any
}
