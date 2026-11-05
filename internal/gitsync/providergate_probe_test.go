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
	targets := e.c.providers.probeTargets()
	e.c.mu.Unlock()
	var held binding
	for _, tg := range targets {
		if tg.entry.BaseURL == "https://git.example.com/" {
			held = tg.bind
		}
	}
	if held.provider != "held" {
		t.Fatalf("held provider not found in %+v", targets)
	}

	res, ok := e.c.runProbe(context.Background(), targets)
	if !ok || res != OriginReachable || len(prober.called()) != 2 {
		t.Fatalf("with no gate: %q %v, probed %v; want reachable and both providers probed", res, ok, prober.called())
	}

	armed := e.clock.now()
	e.c.sched.gates.arm(held, armed, time.Hour)
	res, ok = e.c.runProbe(context.Background(), targets)
	if !ok || res != OriginAccessDenied {
		t.Fatalf("with a held credential: %q %v, want access_denied", res, ok)
	}
	if calls := prober.called(); len(calls) != 3 || calls[2] != "https://git2.example.com/" {
		t.Fatalf("probed %v; the held provider must not be dialled and the other must still be", calls)
	}

	e.c.sched.gates.succeeded(held, armed.Add(time.Second))
	e.c.runProbe(context.Background(), targets)
	if len(prober.called()) != 5 {
		t.Fatalf("probed %v after the gate cleared, want both providers again", prober.called())
	}
}
