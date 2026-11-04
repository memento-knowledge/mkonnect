package gitsync

import (
	"context"
	"log/slog"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gateway"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// maybeProbe launches a pre-config origin reachability probe on a goroutine if the connector has no
// accepted configuration, has at least one local provider, is not already probing, and has not
// probed within the health cadence. The probe and its status send run under the CONNECTION context
// captured in OnConnected (not a per-heartbeat context, which the gateway cancels on return), so a
// heartbeat-triggered refresh is not killed the instant OnHeartbeat returns. The throttle applies to
// every attempt — including failed ones — so a persistent local fault (e.g. git missing) does not
// exec git on every heartbeat. A probe cancelled by a closing connection is never cached.
func (c *Client) maybeProbe() {
	c.mu.Lock()
	accepted := c.configAccepted
	ctx := c.connCtx
	s := c.connSender
	c.mu.Unlock()
	if accepted || ctx == nil || s == nil || ctx.Err() != nil {
		return
	}
	if c.local == nil || len(c.local.Names()) == 0 {
		return
	}
	if !c.probing.CompareAndSwap(false, true) {
		return
	}
	if c.now().Sub(time.Unix(0, c.lastProbe.Load())) < gateway.DefaultHeartbeatInterval {
		c.probing.Store(false)
		return
	}
	c.lastProbe.Store(c.now().UnixNano())
	go func() {
		defer c.probing.Store(false)
		res, ok := c.runProbe(ctx)
		if !ok || ctx.Err() != nil {
			return
		}
		c.originCheck.Store(&proto.OriginCheck{Result: string(res), CheckedAt: c.now().UTC().Format(rfc3339Nano)})
		c.emitStatus(ctx, s, dispatchStatusTimeout)
	}()
}

// runProbe probes every configured provider and aggregates worst-to-best (access_denied over
// unreachable over reachable). ok is false when no provider yielded a usable result (for example
// git is not installed), in which case origin_check is left absent.
func (c *Client) runProbe(ctx context.Context) (OriginResult, bool) {
	rank := map[OriginResult]int{OriginReachable: 0, OriginUnreachable: 1, OriginAccessDenied: 2}
	worst := OriginReachable
	any := false
	for _, e := range c.local.List() {
		res, err := c.prober.Probe(ctx, e)
		if err != nil {
			slog.Warn("origin reachability check could not run", "err", err)
			continue
		}
		any = true
		if rank[res] > rank[worst] {
			worst = res
		}
	}
	return worst, any
}
