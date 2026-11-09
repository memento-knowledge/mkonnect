package gitsync

import (
	"maps"
	"slices"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gateway"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

const (
	// healthyProbeInterval is the spacing between origin checks of a provider whose last check was
	// clean (reachable, or reachable with a repository-level signal such as a 404), once a
	// configuration has been accepted. A check is a git subprocess talking to the origin with the
	// provider's credential, so a healthy provider is asked rarely.
	healthyProbeInterval = 15 * time.Minute
	// failingProbeInterval is the spacing for a provider whose last check was access_denied or
	// unreachable, or could not run, so that recovery shows quickly. A provider whose credential the
	// origin refused is held by its gate and is not dialled, so this costs nothing at the origin.
	failingProbeInterval = 5 * time.Minute

	// maxRepoProbes is how many repositories of one provider a single check may dial: the one the
	// selection prefers and, when that one answers 403, 404 or a redirect, one other.
	maxRepoProbes = 2
)

// probeInterval is the spacing after chk. Before a configuration has been accepted every provider is
// checked at the gateway health cadence, so setup feedback is quick.
func probeInterval(accepted bool, chk providerCheck) time.Duration {
	switch {
	case !accepted:
		return gateway.DefaultHeartbeatInterval
	case chk.fault || chk.report.failing():
		return failingProbeInterval
	}
	return healthyProbeInterval
}

// originReport is what one provider's origin check found.
type originReport struct {
	result OriginResult
	// scope is what the check exercised (proto.OriginCheckScope*), empty when nothing was dialled.
	scope string
	// errorType is the repository error_type naming a check that was not a clean pass.
	errorType string
}

// failing says the check found a provider-level problem. A repository-level signal (a 403, a 404 or a
// redirect on a listed repository) is reachable, and only a configuration change fixes it.
func (r originReport) failing() bool { return r.result != OriginReachable }

// rank orders reports for the connector-level result, worst first: access_denied over unreachable over
// a reachable result that carries an error_type over a clean pass.
func (r originReport) rank() int {
	switch {
	case r.result == OriginAccessDenied:
		return 3
	case r.result == OriginUnreachable:
		return 2
	case r.errorType != "":
		return 1
	}
	return 0
}

func reportOf(rc RepoCheck) originReport {
	return originReport{result: rc.Result, scope: proto.OriginCheckScopeRepository, errorType: rc.ErrorType}
}

// probeKey says what a check was made against, so a result is only ever kept or published while it is
// still about what the connector is configured with.
type probeKey struct {
	bind binding
	// base is the base URL probed (no repository to dial); urls are the repository URLs dialled.
	base string
	urls []string
}

// providerCheck is the latest check of one provider.
type providerCheck struct {
	key    probeKey
	report originReport
	// fault says the check could not run (git missing, no space): a local fault says nothing about the
	// origin, so it contributes no report, but it still counts against the cadence.
	fault bool
	// held says the provider's gate was closed when the check ended, so a gate that a sync closed (or
	// opened) since is noticed at the next heartbeat rather than when the interval next elapses.
	held bool
	at   time.Time
}

// repoCandidate is a repository a provider's check may dial.
type repoCandidate struct {
	id  string
	url string
}

// probePlan is what one provider's next check dials: repos (at most maxRepoProbes, in the order to try
// them) or, with none, the provider's base URL.
type probePlan struct {
	name  string
	entry LocalEntry
	bind  binding
	repos []RepoTarget
	// repo dials repos; it is set exactly when repos is not empty.
	repo RepositoryProber
}

func (p probePlan) key() probeKey {
	k := probeKey{bind: p.bind}
	if len(p.repos) == 0 {
		k.base = p.entry.BaseURL
		return k
	}
	for _, t := range p.repos {
		k.urls = append(k.urls, t.url)
	}
	return k
}

// probeState is the connector's view for planning checks: one consistent snapshot, taken with c.mu
// held, of the providers and of the repositories each may dial.
type probeState struct {
	plans []probePlan
	// cands holds every repository of a provider that its check may dial, in the order to prefer them.
	cands map[string][]repoCandidate
	// byName is the providers in force.
	byName map[string]providerEntry
}

// probeStateLocked plans the next check of every provider. c.mu must be held.
//
// A repository is a candidate only if it is bound to the provider and the connector may sync it
// (schedulable): the URL is the canonical one the host-match guard produced for it, never a raw
// origin_url, and the gate a 401 arms is one the scheduler keeps (see providerGates.retain). The order
// is deterministic and has no rotation: repositories whose last sync succeeded come first, then the
// others, each group by repo_id, so the target changes only when a repository's outcome does.
//
// Without a prober that can check a repository, or with no candidate, a provider is checked at its
// base URL.
func (c *Client) probeStateLocked() probeState {
	st := probeState{cands: map[string][]repoCandidate{}, byName: c.providers.byName}
	rp, canRepo := c.prober.(RepositoryProber)
	if canRepo {
		for _, id := range slices.Sorted(maps.Keys(c.repos)) {
			if r := c.repos[id]; r.schedulable() {
				st.cands[r.provider] = append(st.cands[r.provider], repoCandidate{id: id, url: r.canonicalURL})
			}
		}
		known := func(id string) bool { return c.repos[id].result.State == StateOK }
		for _, cs := range st.cands {
			// Stable, so the ids stay ascending within each group.
			slices.SortStableFunc(cs, func(a, b repoCandidate) int {
				switch {
				case known(a.id) == known(b.id):
					return 0
				case known(a.id):
					return -1
				}
				return 1
			})
		}
	}
	for _, t := range c.providers.probeTargets() {
		p := probePlan{name: t.bind.provider, entry: t.entry, bind: t.bind}
		for _, cand := range st.cands[p.name][:min(maxRepoProbes, len(st.cands[p.name]))] {
			p.repos = append(p.repos, RepoTarget{entry: t.entry, url: cand.url})
		}
		if len(p.repos) > 0 {
			p.repo = rp
		}
		st.plans = append(st.plans, p)
	}
	return st
}

// valid reports whether a check made against k is still about the current configuration: the
// credential is the same generation of the same provider, and what it dialled is still what the
// provider would dial: a base URL while the provider has no repository to dial, or repository URLs
// that are all still bound to it. Which repository the plan prefers is not part of it, so a change of
// preference alone costs no probe.
func (s probeState) valid(k probeKey) bool {
	pe, ok := s.byName[k.bind.provider]
	if !ok || pe.gen != k.bind.gen {
		return false
	}
	cands := s.cands[k.bind.provider]
	if len(k.urls) == 0 {
		return len(cands) == 0 && pe.entry.BaseURL == k.base
	}
	bound := func(u string) bool {
		return slices.ContainsFunc(cands, func(c repoCandidate) bool { return c.url == u })
	}
	return !slices.ContainsFunc(k.urls, func(u string) bool { return !bound(u) })
}

// due returns the plans whose provider has no valid check, or whose check is older than its interval
// (a tenth early, because heartbeats do not arrive at exactly the cadence; a clock that stepped back
// counts as elapsed).
func (c *Client) dueProbesLocked(st probeState, now time.Time, accepted bool) []probePlan {
	var due []probePlan
	for _, p := range st.plans {
		chk, ok := c.provChecks[p.name]
		// A change of the gate costs no dial (a held provider is not dialled), so it is noticed at once.
		if !ok || !st.valid(chk.key) || chk.held != c.sched.gates.held(p.bind) {
			due = append(due, p)
			continue
		}
		iv := probeInterval(accepted, chk)
		if since := now.Sub(chk.at); since < 0 || since >= iv-iv/10 {
			due = append(due, p)
		}
	}
	return due
}

// publishOriginCheckLocked recomputes the connector-level origin_check from the providers' latest
// checks and reports whether it changed. The result is the worst across the providers that could be
// checked (of equals, the first by name), reported with that provider's scope, error_type and the time
// that provider was checked. None, for example when git cannot run, leaves it absent. c.mu must be held.
func (c *Client) publishOriginCheckLocked() bool {
	var best *providerCheck
	for _, name := range slices.Sorted(maps.Keys(c.providers.byName)) {
		chk, ok := c.provChecks[name]
		if !ok || chk.fault {
			continue
		}
		// The worst wins; of equals the first by name, so what is published does not alternate between
		// providers from one check to the next.
		if best == nil || chk.report.rank() > best.report.rank() {
			best = &chk
		}
	}
	var next *proto.OriginCheck
	if best != nil {
		next = &proto.OriginCheck{
			Result:    string(best.report.result),
			CheckedAt: best.at.UTC().Format(rfc3339Nano),
			Scope:     best.report.scope,
			ErrorType: best.report.errorType,
		}
	}
	prev := c.originCheck.Swap(next)
	return (prev == nil) != (next == nil) || (prev != nil && *prev != *next)
}
