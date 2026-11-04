package gitsync

import (
	"log/slog"
	"slices"
	"time"
)

// supersededStopWait bounds how long setProviders waits, after cancelling attempts, for their git
// processes to be gone. Cancelling only signals them; this wait exists to confirm to the operator
// (and to the tests) that they stopped, and it never holds up the providers already being in force.
var supersededStopWait = 3 * time.Second

// setProviders makes next the local provider configuration in force, immediately: from the moment it
// returns, no new sync or probe is made from the previous one.
//
// Under c.mu, which every reader of the bindings also takes, it replaces the provider set, rebinds
// every configured repository against it (host-match is recomputed, not cached), and cancels every
// running attempt whose provider is gone or whose credential differs, which kills its git processes.
// Attempts of unrelated providers are untouched. A repository that is newly bound, or bound to a
// different credential, then gets one sync (coalesced with the rerun of the attempt just cancelled)
// with the new credential; one that is no longer bound reports origin_access_denied ("host not
// configured locally"). The cached origin check is cleared and a probe using the previous providers is
// cancelled, so a stale "reachable" is never reported and a revoked credential is not used by a probe.
//
// It does nothing, and logs nothing, when next equals the configuration already in force. It logs
// counts and repo_ids only: never a provider's credential, URL or name.
func (c *Client) setProviders(next *Store) {
	c.mu.Lock()
	set := buildProviderSet(next, c.providers, c.nextProviderGen)
	if set.equal(c.providers) {
		c.mu.Unlock()
		return
	}
	prev := c.providers
	c.providers = set
	c.providerEpoch++

	var resync, stopped []string
	var waits []*attempt
	for id, r := range c.repos {
		nr := bindRepo(r, set)
		c.repos[id] = nr
		if nr.matched && nr.bind != r.bind {
			resync = append(resync, id)
		}
	}
	for id, att := range c.attempts {
		if pe, ok := set.byName[att.bind.provider]; !ok || pe.gen != att.bind.gen {
			att.superseded = true
			att.cancel(errCredentialSuperseded)
			stopped = append(stopped, id)
			waits = append(waits, att)
		}
	}
	if c.probeCancel != nil {
		c.probeCancel()
	}
	c.originCheck.Store(nil)
	c.lastProbe.Store(0)
	runCtx := c.runCtx
	c.mu.Unlock()

	slices.Sort(resync)
	slices.Sort(stopped)
	for _, id := range stopped {
		slog.Info("cancelled a sync: its provider was removed or its credential changed", "repo_id", id)
	}
	slog.Info("local provider configuration reloaded",
		"providers", len(set.byName), "previous_providers", len(prev.byName),
		"repositories_resynced", len(resync), "syncs_cancelled", len(stopped))

	// The schedule follows the repositories that can now be synced; a repository that became
	// syncable is first synced now rather than after its interval, as when the platform configures it.
	c.sched.reconcile()
	if runCtx != nil {
		c.startSyncs(runCtx, resync)
	}
	c.notifyStatus()
	c.maybeProbe()

	if len(waits) > 0 {
		stopWait := time.NewTimer(supersededStopWait)
		defer stopWait.Stop()
		for _, att := range waits {
			select {
			case <-att.done:
			case <-stopWait.C:
				slog.Warn("a cancelled sync has not stopped yet", "wait", supersededStopWait)
				return
			}
		}
		slog.Info("cancelled syncs stopped", "count", len(waits))
	}
}
