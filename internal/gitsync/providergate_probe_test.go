package gitsync

import (
	"context"
	"sync"
	"testing"
	"time"
)

type countingProber struct {
	mu    sync.Mutex
	calls []string
	res   OriginResult
}

func (p *countingProber) Probe(_ context.Context, e LocalEntry) (OriginResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, e.BaseURL)
	return p.res, nil
}

func (p *countingProber) called() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

const gateTwoProvidersJSON = `{
  "held":  {"base_url":"https://git.example.com/",  "auth":"bearer","token":"0123456789abcdef-A"},
  "other": {"base_url":"https://git2.example.com/", "auth":"bearer","token":"0123456789abcdef-B"}
}`

// The reachability check logs in with the same credential as the syncs, so it must not keep doing so
// while that credential is held back; it reports what is known, that the origin refused it.
func TestOriginCheckDoesNotLogInWithAHeldBackCredential(t *testing.T) {
	e := newSchedEnvProviders(t, nil, gateTwoProvidersJSON)
	prober := &countingProber{res: OriginReachable}
	e.c.prober = prober

	e.c.mu.Lock()
	plans := e.c.probeStateLocked().plans
	e.c.mu.Unlock()
	var held probePlan
	for _, p := range plans {
		if p.entry.BaseURL == "https://git.example.com/" {
			held = p
		}
	}
	if held.name != "held" {
		t.Fatalf("held provider not found in %+v", plans)
	}
	probeAll := func() map[string]originReport {
		out := map[string]originReport{}
		for _, p := range plans {
			o := e.c.probeProvider(context.Background(), p)
			out[p.name] = o.check.report
		}
		return out
	}

	got := probeAll()
	if got["held"].result != OriginReachable || got["other"].result != OriginReachable || len(prober.called()) != 2 {
		t.Fatalf("with no gate: %+v, probed %v; want both reachable and both providers probed", got, prober.called())
	}

	armed := e.clock.now()
	e.c.sched.gates.arm(held.bind, armed, time.Hour)
	got = probeAll()
	if got["held"].result != OriginAccessDenied || got["held"].scope != "" || got["other"].result != OriginReachable {
		t.Fatalf("with a held credential: %+v, want access_denied for it only", got)
	}
	if calls := prober.called(); len(calls) != 3 || calls[2] != "https://git2.example.com/" {
		t.Fatalf("probed %v; the held provider must not be dialled and the other must still be", calls)
	}

	e.c.sched.gates.succeeded(held.bind, armed.Add(time.Second))
	probeAll()
	if len(prober.called()) != 5 {
		t.Fatalf("probed %v after the gate cleared, want both providers again", prober.called())
	}
}
