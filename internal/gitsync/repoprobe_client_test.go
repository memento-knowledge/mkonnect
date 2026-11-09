package gitsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gateway"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// These tests run a real Client, with its scheduler and sync manager over a fake git, against a prober
// that records what it is asked to dial and with which credential, and a clock the test advances.
// The prober's own classification is tested against real git in repoprobe_prober_test.go.

const (
	tokenA = "0123456789abcdef-A"
	tokenB = "0123456789abcdef-B"

	twoProvidersJSON = `{
  "a": {"base_url":"https://git.example.com/",  "auth":"bearer","token":"` + tokenA + `"},
  "b": {"base_url":"https://git2.example.com/", "auth":"bearer","token":"` + tokenB + `"}
}`
	oneCredentialedProviderJSON = `{"a": {"base_url":"https://git.example.com/", "auth":"bearer","token":"` + tokenA + `"}}`
)

// dialRec is one dial the prober was asked to make.
type dialRec struct {
	url   string
	token string
	repo  bool // a repository probe; otherwise the base URL
}

// scriptedRepoProber answers every dial from tables, and records them. It implements both Probe and
// ProbeRepository.
type scriptedRepoProber struct {
	mu    sync.Mutex
	dials []dialRec
	repo  map[string]RepoCheck    // by repository URL; absent means a clean pass
	base  map[string]OriginResult // by base URL; absent means reachable
	fault error                   // every dial fails with it
	// hold, when non-nil, makes a dial wait for it to be closed or its context to end.
	hold chan struct{}
	// holdOnly, when set, limits hold to dials of that URL.
	holdOnly string
	// deaf makes a held dial ignore its context, like a git that finished its request while it was
	// being cancelled.
	deaf bool
	// onDial runs at the start of every dial, outside the lock.
	onDial    func(rec dialRec)
	cancelled atomic.Int32 // dials whose context ended while they waited
}

func (p *scriptedRepoProber) record(rec dialRec) (hold chan struct{}, fault error) {
	p.mu.Lock()
	p.dials = append(p.dials, rec)
	on := p.onDial
	p.mu.Unlock()
	if on != nil {
		on(rec)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.holdOnly != "" && p.holdOnly != rec.url {
		return nil, p.fault
	}
	return p.hold, p.fault
}

func (p *scriptedRepoProber) wait(ctx context.Context, hold chan struct{}) error {
	if hold == nil {
		return nil
	}
	p.mu.Lock()
	deaf := p.deaf
	p.mu.Unlock()
	if deaf {
		<-hold
		return nil
	}
	select {
	case <-hold:
		return nil
	case <-ctx.Done():
		p.cancelled.Add(1)
		return ctx.Err()
	}
}

func (p *scriptedRepoProber) Probe(ctx context.Context, e LocalEntry) (OriginResult, error) {
	hold, fault := p.record(dialRec{url: e.BaseURL, token: e.Token.Reveal()})
	if err := p.wait(ctx, hold); err != nil {
		return "", err
	}
	if fault != nil {
		return "", fault
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if r, ok := p.base[e.BaseURL]; ok {
		return r, nil
	}
	return OriginReachable, nil
}

func (p *scriptedRepoProber) ProbeRepository(ctx context.Context, t RepoTarget) (RepoCheck, error) {
	hold, fault := p.record(dialRec{url: t.URL(), token: t.entry.Token.Reveal(), repo: true})
	// The answer is the one scripted when the dial began, as a real server's is.
	p.mu.Lock()
	answer, ok := p.repo[t.URL()]
	p.mu.Unlock()
	if !ok {
		answer = RepoCheck{Result: OriginReachable}
	}
	if err := p.wait(ctx, hold); err != nil {
		return RepoCheck{}, err
	}
	if fault != nil {
		return RepoCheck{}, fault
	}
	return answer, nil
}

func (p *scriptedRepoProber) set(f func(p *scriptedRepoProber)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

func (p *scriptedRepoProber) all() []dialRec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.dials)
}

func (p *scriptedRepoProber) repoURLs() []string {
	var out []string
	for _, d := range p.all() {
		if d.repo {
			out = append(out, d.url)
		}
	}
	return out
}

// recSender records the statuses a connection is sent.
type recSender struct {
	mu   sync.Mutex
	msgs []proto.GitSyncStatusMsg
	// onSend, when set, runs (outside the lock) for every status sent.
	onSend func(m proto.GitSyncStatusMsg)
}

func (s *recSender) Send(_ context.Context, v any) error {
	m, ok := v.(proto.GitSyncStatusMsg)
	s.mu.Lock()
	if ok {
		s.msgs = append(s.msgs, m)
	}
	hook := s.onSend
	s.mu.Unlock()
	if ok && hook != nil {
		hook(m)
	}
	return nil
}

func (s *recSender) origins() []*proto.OriginCheck {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*proto.OriginCheck
	for _, m := range s.msgs {
		if m.OriginCheck != nil {
			oc := *m.OriginCheck
			out = append(out, &oc)
		}
	}
	return out
}

type originEnvT struct {
	*schedEnv
	p *scriptedRepoProber
	s *recSender
}

// repoURL is the canonical URL of a repository, which is what the connector dials (an explicit port).
func repoURL(host, id string) string { return "https://" + host + ":443/group/" + id + ".git" }

func newProbeEnv(t *testing.T, providers string) *originEnvT {
	t.Helper()
	e := &originEnvT{schedEnv: newSchedEnvProviders(t, nil, providers), p: &scriptedRepoProber{}, s: &recSender{}}
	e.c.prober = e.p
	e.originURL = func(id string) string { return repoURL("git.example.com", id) }
	// No sync finishes during these tests: a finished sync is evidence about a credential, and what
	// the check does with that must not depend on how quickly a fake git answers. A test that wants
	// syncs to run releases them (see releaseSyncs).
	e.holdSyncs()
	return e
}

// settle has nothing to wait for: the syncs are held.
func (e *originEnvT) settle(*testing.T) {}

// releaseSyncs lets the held syncs run and waits for them.
func (e *originEnvT) releaseSyncs(t *testing.T) {
	t.Helper()
	if g := e.gate.Swap(nil); g != nil {
		close(*g)
	}
	e.schedEnv.settle(t)
}

func (e *originEnvT) connect() { e.c.OnConnected(e.ctx, e.s) }

func (e *originEnvT) heartbeat(t *testing.T) {
	t.Helper()
	if err := e.c.OnHeartbeat(e.ctx, e.s); err != nil {
		t.Fatal(err)
	}
}

// settleProbe waits until no check is running and none has started for a moment: a check whose
// result is discarded starts the next one as it ends.
func (e *originEnvT) settleProbe(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for stable := 0; stable < 15; {
		if e.c.probing.Load() {
			stable = 0
		} else {
			stable++
		}
		if time.Now().After(deadline) {
			t.Fatal("the origin check did not settle")
		}
		time.Sleep(4 * time.Millisecond)
	}
}

func (e *originEnvT) check() *proto.OriginCheck { return e.c.originCheck.Load() }

func (e *originEnvT) plan(t *testing.T, name string) probePlan {
	t.Helper()
	e.c.mu.Lock()
	defer e.c.mu.Unlock()
	for _, p := range e.c.probeStateLocked().plans {
		if p.name == name {
			return p
		}
	}
	t.Fatalf("no provider %q", name)
	return probePlan{}
}

func (e *originEnvT) held(t *testing.T, name string) bool {
	t.Helper()
	return e.c.sched.gates.held(e.plan(t, name).bind)
}

// startConfigured connects, lets the first (base URL) check finish, accepts a configuration of ids and
// lets the check that follows finish.
func (e *originEnvT) startConfigured(t *testing.T, ids ...string) {
	t.Helper()
	e.connect()
	e.settleProbe(t)
	e.configure("", ids...)
	e.settle(t)
	e.settleProbe(t)
}

func assertWireResult(t *testing.T, oc *proto.OriginCheck) {
	t.Helper()
	switch oc.Result {
	case "reachable", "access_denied", "unreachable":
	default:
		t.Fatalf("origin_check.result = %q, want one of the three wire values", oc.Result)
	}
}

func TestOriginCheckProbesTheBaseURLBeforeAnyConfiguration(t *testing.T) {
	e := newProbeEnv(t, twoProvidersJSON)
	e.connect()
	e.settleProbe(t)

	want := []dialRec{{url: "https://git.example.com/", token: tokenA}, {url: "https://git2.example.com/", token: tokenB}}
	if got := e.p.all(); !slices.Equal(got, want) {
		t.Fatalf("dials = %+v, want the base URLs %+v", got, want)
	}
	oc := e.check()
	if oc == nil || oc.Result != "reachable" || oc.Scope != proto.OriginCheckScopeHost || oc.ErrorType != "" {
		t.Fatalf("origin_check = %+v, want reachable, scope host", oc)
	}
}

// Once a configuration is accepted a provider is checked through a repository it lists, at once (not
// after the cadence), with its own credential; a provider with no repository stays on the base URL.
func TestOriginCheckSwitchesToARepositoryWhenAConfigurationIsAccepted(t *testing.T) {
	e := newProbeEnv(t, twoProvidersJSON)
	e.connect()
	e.settleProbe(t)
	before := len(e.p.all())

	e.configure("", "r2", "r1")
	e.settle(t)
	e.settleProbe(t)

	got := e.p.all()[before:]
	want := []dialRec{{url: repoURL("git.example.com", "r1"), token: tokenA, repo: true}}
	if !slices.Equal(got, want) {
		t.Fatalf("dials after the configuration = %+v, want only provider a's repository %+v (provider b has no repository and is still fresh)", got, want)
	}
	oc := e.check()
	if oc == nil || oc.Result != "reachable" || oc.Scope != proto.OriginCheckScopeRepository || oc.ErrorType != "" {
		t.Fatalf("origin_check = %+v, want a clean repository-scoped pass", oc)
	}

	// The platform re-sends an unchanged configuration on a token refresh: nothing is dialled.
	e.configure("", "r2", "r1")
	e.settle(t)
	e.settleProbe(t)
	if n := len(e.p.all()); n != before+1 {
		t.Fatalf("%d dials after an unchanged configuration was re-sent, want none more", n-before-1)
	}
}

func TestOriginCheckRepositorySelection(t *testing.T) {
	e := newProbeEnv(t, oneCredentialedProviderJSON)
	e.configure("", "r3", "r1", "r2", "r4")

	setOK := func(ids ...string) {
		e.c.mu.Lock()
		defer e.c.mu.Unlock()
		for id, r := range e.c.repos {
			r.result = syncResult{}
			if slices.Contains(ids, id) {
				r.result = syncResult{State: StateOK}
			}
			e.c.repos[id] = r
		}
	}
	order := func() []string {
		var out []string
		for _, r := range e.plan(t, "a").repos {
			out = append(out, r.URL()[strings.LastIndex(r.URL(), "/")+1:])
		}
		return out
	}
	for _, tt := range []struct {
		name string
		ok   []string
		want []string
	}{
		{"nothing synced: the lowest ids", nil, []string{"r1.git", "r2.git"}},
		{"a repository whose last sync succeeded comes first", []string{"r3"}, []string{"r3.git", "r1.git"}},
		{"then the lowest id among them", []string{"r4", "r2"}, []string{"r2.git", "r4.git"}},
		{"at most two", []string{"r1", "r2", "r3", "r4"}, []string{"r1.git", "r2.git"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setOK(tt.ok...)
			if got := order(); !slices.Equal(got, tt.want) {
				t.Fatalf("plan = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOriginCheckFallbackAndTwoProbeCap(t *testing.T) {
	notFound := RepoCheck{Result: OriginReachable, ErrorType: ErrOriginAccessDenied}
	moved := RepoCheck{Result: OriginReachable, ErrorType: ErrOriginURLRedirected}
	unreachable := RepoCheck{Result: OriginUnreachable, ErrorType: ErrOriginUnreachable}
	denied := RepoCheck{Result: OriginAccessDenied, ErrorType: ErrOriginAccessDenied, CredentialRejected: true}
	r1, r2, r3 := repoURL("git.example.com", "r1"), repoURL("git.example.com", "r2"), repoURL("git.example.com", "r3")

	tests := []struct {
		name      string
		script    map[string]RepoCheck
		wantDials []string
		want      proto.OriginCheck // Result, Scope and ErrorType
		wantGate  bool
	}{
		{"the first repository passes", nil, []string{r1}, proto.OriginCheck{Result: "reachable", Scope: "repository"}, false},
		{"first 404, second passes: one repository is gone, the provider is fine", map[string]RepoCheck{r1: notFound}, []string{r1, r2},
			proto.OriginCheck{Result: "reachable", Scope: "repository"}, false},
		{"both 404: reachable, with the repository error", map[string]RepoCheck{r1: notFound, r2: notFound}, []string{r1, r2},
			proto.OriginCheck{Result: "reachable", Scope: "repository", ErrorType: "origin_access_denied"}, false},
		{"both redirected", map[string]RepoCheck{r1: moved, r2: moved}, []string{r1, r2},
			proto.OriginCheck{Result: "reachable", Scope: "repository", ErrorType: "origin_url_redirected"}, false},
		{"a provider-level answer on the second wins", map[string]RepoCheck{r1: notFound, r2: unreachable}, []string{r1, r2},
			proto.OriginCheck{Result: "unreachable", Scope: "repository", ErrorType: "origin_unreachable"}, false},
		{"unreachable on the first ends the check", map[string]RepoCheck{r1: unreachable}, []string{r1},
			proto.OriginCheck{Result: "unreachable", Scope: "repository", ErrorType: "origin_unreachable"}, false},
		{"a refused credential ends the check and closes the gate", map[string]RepoCheck{r1: denied}, []string{r1},
			proto.OriginCheck{Result: "access_denied", Scope: "repository", ErrorType: "origin_access_denied"}, true},
		{"a refused credential on the second", map[string]RepoCheck{r1: notFound, r2: denied}, []string{r1, r2},
			proto.OriginCheck{Result: "access_denied", Scope: "repository", ErrorType: "origin_access_denied"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newProbeEnv(t, oneCredentialedProviderJSON)
			e.p.set(func(p *scriptedRepoProber) { p.repo = tt.script })
			e.connect()
			e.settleProbe(t)
			e.configure("", "r1", "r2", "r3")
			e.settle(t)
			e.settleProbe(t)

			if got := e.p.repoURLs(); !slices.Equal(got, tt.wantDials) {
				t.Errorf("repositories dialled = %v, want %v (never %s)", got, tt.wantDials, r3)
			}
			oc := e.check()
			if oc == nil || oc.Result != tt.want.Result || oc.Scope != tt.want.Scope || oc.ErrorType != tt.want.ErrorType {
				t.Fatalf("origin_check = %+v, want %+v", oc, tt.want)
			}
			assertWireResult(t, oc)
			if got := e.held(t, "a"); got != tt.wantGate {
				t.Errorf("gate held = %v, want %v", got, tt.wantGate)
			}
		})
	}
}

func TestOriginCheckGate(t *testing.T) {
	denied := RepoCheck{Result: OriginAccessDenied, ErrorType: ErrOriginAccessDenied, CredentialRejected: true}
	r1 := repoURL("git.example.com", "r1")

	t.Run("a probe 401 closes the gate, and a held gate is not dialled", func(t *testing.T) {
		e := newProbeEnv(t, oneCredentialedProviderJSON)
		e.p.set(func(p *scriptedRepoProber) { p.repo = map[string]RepoCheck{r1: denied} })
		e.startConfigured(t, "r1")
		if !e.held(t, "a") {
			t.Fatal("the gate is open after the origin refused the credential")
		}
		dials := len(e.p.all())

		// Every cadence that comes round finds the gate held: nothing is dialled, access_denied stays.
		for range 4 {
			e.clock.advance(failingProbeInterval + time.Second)
			e.heartbeat(t)
			e.settleProbe(t)
		}
		if n := len(e.p.all()); n != dials {
			t.Fatalf("%d more dials while the gate was held", n-dials)
		}
		if oc := e.check(); oc == nil || oc.Result != "access_denied" || oc.Scope != "" || oc.ErrorType != "" {
			t.Fatalf("origin_check while held = %+v, want access_denied with no scope", oc)
		}

		// A sync that succeeds clears the gate; the next check dials again.
		e.c.sched.gates.succeeded(e.plan(t, "a").bind, e.clock.now().Add(time.Second))
		e.p.set(func(p *scriptedRepoProber) { p.repo = nil })
		e.clock.advance(failingProbeInterval + time.Second)
		e.heartbeat(t)
		e.settleProbe(t)
		if n := len(e.p.all()); n != dials+1 {
			t.Fatalf("%d dials after the gate cleared, want 1", n-dials)
		}
		if oc := e.check(); oc == nil || oc.Result != "reachable" {
			t.Fatalf("origin_check after recovery = %+v", oc)
		}
	})

	t.Run("the gate is consulted before the second dial", func(t *testing.T) {
		e := newProbeEnv(t, oneCredentialedProviderJSON)
		e.p.set(func(p *scriptedRepoProber) {
			p.repo = map[string]RepoCheck{r1: {Result: OriginReachable, ErrorType: ErrOriginAccessDenied}}
		})
		e.connect()
		e.settleProbe(t)
		// A sync closes the gate while the first repository is being dialled.
		e.p.set(func(p *scriptedRepoProber) {
			p.onDial = func(d dialRec) {
				if d.repo && d.url == r1 {
					e.c.sched.gates.arm(e.plan(t, "a").bind, e.clock.now(), time.Hour)
				}
			}
		})
		e.configure("", "r1", "r2")
		e.settle(t)
		e.settleProbe(t)
		if got := e.p.repoURLs(); !slices.Equal(got, []string{r1}) {
			t.Fatalf("dialled %v: the second repository must not be dialled once the gate is closed", got)
		}
		if oc := e.check(); oc == nil || oc.Result != "access_denied" {
			t.Fatalf("origin_check = %+v, want access_denied", oc)
		}
	})

	t.Run("a provider without a credential is told it needs one, but never gated", func(t *testing.T) {
		e := newProbeEnv(t, `{"a": {"base_url":"https://git.example.com/"}}`)
		e.p.set(func(p *scriptedRepoProber) {
			p.repo = map[string]RepoCheck{r1: {Result: OriginAccessDenied, ErrorType: ErrOriginAccessDenied}}
		})
		e.startConfigured(t, "r1")
		if oc := e.check(); oc == nil || oc.Result != "access_denied" || oc.Scope != "repository" {
			t.Fatalf("origin_check = %+v, want access_denied", oc)
		}
		if e.held(t, "a") {
			t.Error("a gate was closed for a provider that has no credential to be refused")
		}
	})

	t.Run("the refusal closes the gate even when the connection dropped before the result could be kept", func(t *testing.T) {
		e := newProbeEnv(t, oneCredentialedProviderJSON)
		e.connect()
		e.settleProbe(t)
		hold := make(chan struct{})
		e.p.set(func(p *scriptedRepoProber) {
			p.repo = map[string]RepoCheck{r1: denied}
			p.hold = hold
		})
		e.configure("", "r1")
		eventually(t, func() bool { return len(e.p.repoURLs()) == 1 })
		e.c.mu.Lock()
		e.c.connGen++ // another connection took over
		e.c.mu.Unlock()
		close(hold)
		e.settleProbe(t)
		if !e.held(t, "a") {
			t.Fatal("the gate is open: the next check, and every sync, would log in with a credential the origin just refused")
		}
	})

	t.Run("a credential that was replaced meanwhile is not gated", func(t *testing.T) {
		e := newProbeEnv(t, oneCredentialedProviderJSON)
		e.configure("", "r1")
		cur := e.plan(t, "a").bind
		e.c.armFromProbe(binding{provider: cur.provider, gen: cur.gen + 1}, e.clock.now())
		e.c.armFromProbe(binding{provider: "gone", gen: cur.gen}, e.clock.now())
		if e.c.sched.gates.size() != 0 {
			t.Fatal("a gate was closed for a credential generation that is not in force")
		}
		e.c.armFromProbe(cur, e.clock.now())
		if !e.held(t, "a") {
			t.Fatal("the current generation was not gated")
		}
	})
}

// Arming from the check takes the client's lock and then the gates' leaf lock; the scheduler's poll
// takes its own lock and then the client's (a due repository is handed to the manager, which asks the
// client). The check must never take the scheduler's lock under the client's. Entries are made due on
// every poll, so poll really does reach the client while the other goroutine arms and clears.
func TestOriginCheckGateArmingDoesNotDeadlockWithTheScheduler(t *testing.T) {
	e := newProbeEnv(t, oneCredentialedProviderJSON)
	e.configure("", "r1", "r2")
	e.releaseSyncs(t)
	b := e.plan(t, "a").bind
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 300 {
			e.clock.advance(3 * time.Hour)
			e.c.sched.poll(e.ctx)
		}
	}()
	go func() {
		defer wg.Done()
		for range 300 {
			at := e.clock.now().Add(24 * time.Hour) // after every sync, so nothing vouches for the credential
			e.c.armFromProbe(b, at)
			e.c.sched.gates.succeeded(b, at.Add(time.Second))
		}
	}()
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock between the check's gate arming and the scheduler")
	}
}

// Only a repository bound to the probing provider is dialled, and always with that provider's own
// credential; a repository whose host matches no provider is never dialled; a repository the connector
// cannot sync (no usable replica) is not a candidate either.
func TestOriginCheckNeverPairsACredentialWithAnotherHost(t *testing.T) {
	e := newProbeEnv(t, twoProvidersJSON)
	e.connect()
	e.settleProbe(t)
	repos := []struct{ id, host, replica string }{
		{"ra", "git.example.com", "https://replica.example.com/r/ra.git"},
		{"rb", "git2.example.com", "https://replica.example.com/r/rb.git"},
		{"rc", "git3.example.com", "https://replica.example.com/r/rc.git"}, // no provider
		{"rd", "git.example.com", ""},                                      // no usable replica
	}
	var rs []string
	for _, r := range repos {
		rs = append(rs, fmt.Sprintf(`{"repo_id":%q,"origin_url":%q,"replica_push_url":%q,"push_token":"push-token-abcdefghij"}`, r.id, repoURL(r.host, r.id), r.replica))
	}
	raw := `{"type":"git_sync_config","config_version":1,"repositories":[` + strings.Join(rs, ",") + `]}`
	e.c.HandleMessage(e.ctx, schedStubSender{}, "git_sync_config", []byte(raw))
	e.settle(t)
	e.settleProbe(t)

	tokenOf := map[string]string{"git.example.com": tokenA, "git2.example.com": tokenB}
	repoDials := 0
	for _, d := range e.p.all() {
		u, err := url.Parse(d.url)
		if err != nil {
			t.Fatal(err)
		}
		if want, ok := tokenOf[u.Hostname()]; !ok || d.token != want {
			t.Errorf("dialled %s with token %q: a credential was paired with a host that is not its provider's", d.url, d.token)
		}
		if d.repo {
			repoDials++
		}
	}
	want := []string{repoURL("git.example.com", "ra"), repoURL("git2.example.com", "rb")}
	if got := e.p.repoURLs(); !slices.Equal(got, want) {
		t.Errorf("repositories dialled = %v, want %v (not rc: no provider; not rd: it cannot sync)", got, want)
	}

	// Provider a has a repository now, but one that cannot sync: with nothing to dial it is checked at
	// its base URL, as before any configuration.
	e.c.mu.Lock()
	for id := range e.c.repos {
		if id != "rd" {
			delete(e.c.repos, id)
		}
	}
	plans := e.c.probeStateLocked().plans
	e.c.mu.Unlock()
	for _, p := range plans {
		if len(p.repos) != 0 {
			t.Errorf("provider %s would dial %v, want its base URL", p.name, p.repos)
		}
	}
}

func TestOriginCheckInFlightProbeIsCancelledWhenItsTargetChanges(t *testing.T) {
	t.Run("the first configuration replaces a base URL check", func(t *testing.T) {
		e := newProbeEnv(t, oneCredentialedProviderJSON)
		e.p.set(func(p *scriptedRepoProber) {
			p.hold = make(chan struct{})
			p.base = map[string]OriginResult{"https://git.example.com/": OriginUnreachable}
		})
		e.connect()
		eventually(t, func() bool { return len(e.p.all()) == 1 })

		// What the cancelled check was about is no longer what is configured: it is stopped, nothing it
		// found is published, and the repository is checked in its place.
		e.p.set(func(p *scriptedRepoProber) { p.hold = nil })
		e.configure("", "r1")
		eventually(t, func() bool { return e.p.cancelled.Load() == 1 })
		e.settle(t)
		e.settleProbe(t)

		if got := e.p.repoURLs(); !slices.Equal(got, []string{repoURL("git.example.com", "r1")}) {
			t.Fatalf("repositories dialled = %v", got)
		}
		for _, oc := range e.s.origins() {
			if oc.Scope == proto.OriginCheckScopeHost || oc.Result == "unreachable" {
				t.Errorf("a status carried %+v, the result of a check that was cancelled", oc)
			}
		}
		if oc := e.check(); oc == nil || oc.Scope != "repository" || oc.Result != "reachable" {
			t.Fatalf("origin_check = %+v, want the repository's", oc)
		}
	})

	t.Run("a configuration that leaves the target alone does not stop it", func(t *testing.T) {
		e := newProbeEnv(t, oneCredentialedProviderJSON)
		e.connect()
		e.settleProbe(t)
		hold := make(chan struct{})
		e.p.set(func(p *scriptedRepoProber) { p.hold = hold })
		e.configure("", "r1")
		eventually(t, func() bool { return len(e.p.repoURLs()) == 1 })

		e.configure("", "r1") // a token refresh
		e.configure("", "r1", "r2")
		time.Sleep(50 * time.Millisecond)
		if n := e.p.cancelled.Load(); n != 0 {
			t.Fatalf("the check was cancelled %d times by configurations that kept its repository", n)
		}
		close(hold)
		e.settle(t)
		e.settleProbe(t)
		if oc := e.check(); oc == nil || oc.Scope != "repository" {
			t.Fatalf("origin_check = %+v", oc)
		}
		if n := len(e.p.repoURLs()); n != 1 {
			t.Errorf("%d repository dials, want 1: the configurations that kept the target must not cause another", n)
		}
	})

	t.Run("a configuration that drops the repository being dialled stops the check", func(t *testing.T) {
		e := newProbeEnv(t, oneCredentialedProviderJSON)
		e.connect()
		e.settleProbe(t)
		hold := make(chan struct{})
		e.p.set(func(p *scriptedRepoProber) { p.hold = hold })
		e.configure("", "r1")
		eventually(t, func() bool { return len(e.p.repoURLs()) == 1 })

		e.p.set(func(p *scriptedRepoProber) { p.hold = nil })
		e.configure("", "r2")
		eventually(t, func() bool { return e.p.cancelled.Load() == 1 })
		e.settle(t)
		e.settleProbe(t)
		if got := e.p.repoURLs(); !slices.Equal(got, []string{repoURL("git.example.com", "r1"), repoURL("git.example.com", "r2")}) {
			t.Fatalf("repositories dialled = %v, want r1 (cancelled) then r2", got)
		}
	})
}

func TestProbeKeyValidity(t *testing.T) {
	b := binding{provider: "a", gen: 1}
	pe := providerEntry{entry: LocalEntry{BaseURL: "https://git.example.com/v1/"}, gen: 1}
	cand := []repoCandidate{{id: "r1", url: "https://git.example.com/g/r1.git"}}
	st := probeState{byName: map[string]providerEntry{"a": pe}, cands: map[string][]repoCandidate{}}
	stRepo := probeState{byName: st.byName, cands: map[string][]repoCandidate{"a": cand}}

	for _, tt := range []struct {
		name string
		st   probeState
		k    probeKey
		want bool
	}{
		{"the base URL, unchanged", st, probeKey{bind: b, base: "https://git.example.com/v1/"}, true},
		{"the base URL, its path changed (the credential is the same generation)", st, probeKey{bind: b, base: "https://git.example.com/v2/"}, false},
		{"the base URL, once a repository can be dialled", stRepo, probeKey{bind: b, base: "https://git.example.com/v1/"}, false},
		{"a repository, still bound", stRepo, probeKey{bind: b, urls: []string{cand[0].url}}, true},
		{"a repository, no longer listed", st, probeKey{bind: b, urls: []string{cand[0].url}}, false},
		{"a repository, its URL changed", stRepo, probeKey{bind: b, urls: []string{"https://git.example.com/g/renamed.git"}}, false},
		{"one of two repositories gone", stRepo, probeKey{bind: b, urls: []string{cand[0].url, "https://git.example.com/g/gone.git"}}, false},
		{"another credential generation", stRepo, probeKey{bind: binding{provider: "a", gen: 2}, urls: []string{cand[0].url}}, false},
		{"a provider that was removed", stRepo, probeKey{bind: binding{provider: "x", gen: 1}, urls: []string{cand[0].url}}, false},
	} {
		if got := tt.st.valid(tt.k); got != tt.want {
			t.Errorf("%s: valid = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// due is when a check falls due: a tenth early, as the heartbeats that trigger it are uneven.
func due(iv time.Duration) time.Duration { return iv - iv/10 }

func TestOriginCheckCadence(t *testing.T) {
	unreachable := RepoCheck{Result: OriginUnreachable, ErrorType: ErrOriginUnreachable}
	r1 := repoURL("git.example.com", "r1")

	t.Run("every 30 seconds until a configuration is accepted", func(t *testing.T) {
		e := newProbeEnv(t, oneCredentialedProviderJSON)
		e.connect()
		e.settleProbe(t)
		e.clock.advance(due(gateway.DefaultHeartbeatInterval) - time.Second)
		e.heartbeat(t)
		e.settleProbe(t)
		if n := len(e.p.all()); n != 1 {
			t.Fatalf("%d dials before the health cadence elapsed, want 1", n)
		}
		e.clock.advance(2 * time.Second)
		e.heartbeat(t)
		e.settleProbe(t)
		if n := len(e.p.all()); n != 2 {
			t.Fatalf("%d dials once it elapsed, want 2", n)
		}
	})

	t.Run("every 15 minutes while healthy", func(t *testing.T) {
		e := newProbeEnv(t, oneCredentialedProviderJSON)
		e.startConfigured(t, "r1")
		dials := len(e.p.all())
		for _, step := range []time.Duration{due(healthyProbeInterval) - time.Second, time.Second} {
			e.clock.advance(step)
			e.heartbeat(t)
			e.settleProbe(t)
		}
		if n := len(e.p.all()); n != dials+1 {
			t.Fatalf("%d dials in a healthy provider's first %v, want exactly one, at the end", n-dials, due(healthyProbeInterval))
		}
	})

	t.Run("a repository-level signal is healthy: still every 15 minutes", func(t *testing.T) {
		e := newProbeEnv(t, oneCredentialedProviderJSON)
		e.p.set(func(p *scriptedRepoProber) {
			p.repo = map[string]RepoCheck{r1: {Result: OriginReachable, ErrorType: ErrOriginAccessDenied}}
		})
		e.startConfigured(t, "r1")
		dials := len(e.p.all())
		e.clock.advance(due(failingProbeInterval) + time.Second)
		e.heartbeat(t)
		e.settleProbe(t)
		if n := len(e.p.all()); n != dials {
			t.Fatalf("%d dials after 5 minutes, want none: a 403 or 404 on one repository is not worth a login every 5 minutes", n-dials)
		}
	})

	t.Run("every 5 minutes while a provider is failing", func(t *testing.T) {
		e := newProbeEnv(t, oneCredentialedProviderJSON)
		e.p.set(func(p *scriptedRepoProber) { p.repo = map[string]RepoCheck{r1: unreachable} })
		e.startConfigured(t, "r1")
		dials := len(e.p.all())
		e.clock.advance(due(failingProbeInterval) - time.Second)
		e.heartbeat(t)
		e.settleProbe(t)
		if n := len(e.p.all()); n != dials {
			t.Fatalf("%d dials before 5 minutes, want none", n-dials)
		}
		e.p.set(func(p *scriptedRepoProber) { p.repo = nil })
		e.clock.advance(2 * time.Second)
		e.heartbeat(t)
		e.settleProbe(t)
		if n := len(e.p.all()); n != dials+1 {
			t.Fatalf("%d dials at 5 minutes, want 1", n-dials)
		}
		if oc := e.check(); oc == nil || oc.Result != "reachable" {
			t.Fatalf("origin_check = %+v: recovery must show at the first check", oc)
		}
	})

	t.Run("each provider on its own", func(t *testing.T) {
		e := newProbeEnv(t, twoProvidersJSON)
		e.originURL = func(id string) string {
			return repoURL(map[string]string{"ra": "git.example.com", "rb": "git2.example.com"}[id], id)
		}
		e.p.set(func(p *scriptedRepoProber) {
			p.repo = map[string]RepoCheck{repoURL("git.example.com", "ra"): unreachable}
		})
		e.startConfigured(t, "ra", "rb")
		dials := len(e.p.all())
		e.clock.advance(due(failingProbeInterval) + time.Second)
		e.heartbeat(t)
		e.settleProbe(t)
		got := e.p.all()[dials:]
		if want := []dialRec{{url: repoURL("git.example.com", "ra"), token: tokenA, repo: true}}; !slices.Equal(got, want) {
			t.Fatalf("dials after 5 minutes = %+v, want only the failing provider's %+v", got, want)
		}
	})

	t.Run("a clock that stepped back does not stall the cadence", func(t *testing.T) {
		e := newProbeEnv(t, oneCredentialedProviderJSON)
		e.startConfigured(t, "r1")
		dials := len(e.p.all())
		e.clock.advance(-time.Hour)
		e.heartbeat(t)
		e.settleProbe(t)
		if n := len(e.p.all()); n != dials+1 {
			t.Fatalf("%d dials after the clock stepped back, want 1", n-dials)
		}
	})
}

// A local fault (git cannot run) is an attempt like any other for the cadence, so it does not exec git
// on every heartbeat, and it leaves origin_check absent.
func TestOriginCheckLocalFaultIsThrottled(t *testing.T) {
	e := newProbeEnv(t, oneCredentialedProviderJSON)
	e.p.set(func(p *scriptedRepoProber) { p.fault = errors.New("git could not be started") })
	e.connect()
	e.settleProbe(t)
	for range 6 {
		e.heartbeat(t)
		e.settleProbe(t)
	}
	if n := len(e.p.all()); n != 1 {
		t.Fatalf("%d dials across 6 heartbeats with a persistent fault, want 1", n)
	}
	if oc := e.check(); oc != nil {
		t.Fatalf("origin_check = %+v, want it absent: a local fault says nothing about the origin", oc)
	}
	e.clock.advance(due(gateway.DefaultHeartbeatInterval) + time.Second)
	e.heartbeat(t)
	e.settleProbe(t)
	if n := len(e.p.all()); n != 2 {
		t.Fatalf("%d dials once the cadence elapsed, want 2", n)
	}
}

func TestOriginCheckAggregatesTheWorstProvider(t *testing.T) {
	ra, rb := repoURL("git.example.com", "ra"), repoURL("git2.example.com", "rb")
	notFound := RepoCheck{Result: OriginReachable, ErrorType: ErrOriginAccessDenied}
	unreachable := RepoCheck{Result: OriginUnreachable, ErrorType: ErrOriginUnreachable}
	refused := RepoCheck{Result: OriginAccessDenied, ErrorType: ErrOriginAccessDenied} // no credential sent: no gate

	for _, tt := range []struct {
		name   string
		script map[string]RepoCheck
		want   proto.OriginCheck
		by     string // the provider whose check it is
	}{
		{"both clean: of equals, the first by name", nil, proto.OriginCheck{Result: "reachable", Scope: "repository"}, "a"},
		{"a repository signal beats a clean pass", map[string]RepoCheck{rb: notFound},
			proto.OriginCheck{Result: "reachable", Scope: "repository", ErrorType: "origin_access_denied"}, "b"},
		{"unreachable beats it", map[string]RepoCheck{ra: notFound, rb: unreachable},
			proto.OriginCheck{Result: "unreachable", Scope: "repository", ErrorType: "origin_unreachable"}, "b"},
		{"access_denied beats unreachable", map[string]RepoCheck{ra: unreachable, rb: refused},
			proto.OriginCheck{Result: "access_denied", Scope: "repository", ErrorType: "origin_access_denied"}, "b"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newProbeEnv(t, twoProvidersJSON)
			e.originURL = func(id string) string { return map[string]string{"ra": ra, "rb": rb}[id] }
			e.p.set(func(p *scriptedRepoProber) {
				p.repo = tt.script
				// Each dial takes a second, so the time of each check tells whose it is.
				p.onDial = func(dialRec) { e.clock.advance(time.Second) }
			})
			e.startConfigured(t, "ra", "rb")
			oc := e.check()
			if oc == nil || oc.Result != tt.want.Result || oc.Scope != tt.want.Scope || oc.ErrorType != tt.want.ErrorType {
				t.Fatalf("origin_check = %+v, want %+v", oc, tt.want)
			}
			assertWireResult(t, oc)
			e.c.mu.Lock()
			at := e.c.provChecks[tt.by].at.UTC().Format(rfc3339Nano)
			e.c.mu.Unlock()
			if oc.CheckedAt != at {
				t.Errorf("checked_at = %s, want the time provider %s was checked, %s", oc.CheckedAt, tt.by, at)
			}
		})
	}
}

// The refusal arrives after a configuration took the last repository of the provider away: nothing
// syncs with that credential any more, so a gate armed now would be one nothing ever clears.
func TestOriginCheckRefusalAfterTheLastRepositoryLeftClosesNoGate(t *testing.T) {
	e := newProbeEnv(t, oneCredentialedProviderJSON)
	r1 := repoURL("git.example.com", "r1")
	e.connect()
	e.settleProbe(t)
	var once sync.Once
	e.p.set(func(p *scriptedRepoProber) {
		p.repo = map[string]RepoCheck{r1: {Result: OriginAccessDenied, ErrorType: ErrOriginAccessDenied, CredentialRejected: true}}
		p.onDial = func(d dialRec) {
			if d.repo {
				once.Do(func() { e.configure("") }) // the platform lists no repository any more
			}
		}
	})
	e.configure("", "r1")
	eventually(t, func() bool { return len(e.p.repoURLs()) == 1 })
	e.settleProbe(t)

	if e.held(t, "a") {
		t.Fatal("a gate was closed for a credential no repository is bound to: nothing would ever clear it")
	}
	// The provider is checked at its base URL again and, the gate being open, really dialled.
	e.p.set(func(p *scriptedRepoProber) { p.repo, p.onDial = nil, nil })
	dials := len(e.p.all())
	e.clock.advance(healthyProbeInterval)
	e.heartbeat(t)
	e.settleProbe(t)
	if got := e.p.all()[dials:]; len(got) != 1 || got[0].repo {
		t.Fatalf("dials = %+v, want the base URL once", got)
	}
}

// A sync of a repository that began after the check did, and succeeded, shows the credential works: a
// refusal the check meets late must not hold the credential back.
func TestOriginCheckRefusalAfterASuccessfulSyncClosesNoGate(t *testing.T) {
	e := newProbeEnv(t, oneCredentialedProviderJSON)
	e.configure("", "r1")
	b := e.plan(t, "a").bind
	probeStart := e.clock.now()
	e.releaseSyncs(t) // r1 syncs successfully at probeStart's second or later

	e.c.armFromProbe(b, probeStart)
	if e.held(t, "a") {
		t.Fatal("the gate was closed although a sync that began after the check succeeded")
	}
	e.c.armFromProbe(b, probeStart.Add(10*time.Second))
	if !e.held(t, "a") {
		t.Fatal("the gate was not closed by a refusal newer than every sync")
	}
}

// A refusal that comes back after the credential was replaced is about the old credential.
func TestOriginCheckRefusalAfterAReloadClosesNoGate(t *testing.T) {
	e := newProbeEnv(t, oneCredentialedProviderJSON)
	r1 := repoURL("git.example.com", "r1")
	e.connect()
	e.settleProbe(t)
	hold := make(chan struct{})
	e.p.set(func(p *scriptedRepoProber) {
		p.hold, p.deaf = hold, true
		p.repo = map[string]RepoCheck{r1: {Result: OriginAccessDenied, ErrorType: ErrOriginAccessDenied, CredentialRejected: true}}
	})
	e.configure("", "r1")
	eventually(t, func() bool { return len(e.p.repoURLs()) == 1 })
	e.p.set(func(p *scriptedRepoProber) { p.repo = nil }) // the new credential works; the dial in flight keeps its answer

	// The operator rotates the credential while the old one is being refused.
	rotated := `{"a": {"base_url":"https://git.example.com/", "auth":"bearer","token":"0123456789abcdef-NEW"}}`
	if err := os.WriteFile(e.providerPath, []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(e.providerPath)
	if err != nil {
		t.Fatal(err)
	}
	e.c.setProviders(store)
	e.p.set(func(p *scriptedRepoProber) { p.hold = nil }) // dials that start from now on are the new credential's
	close(hold)
	e.settleProbe(t)
	if e.c.sched.gates.size() != 0 {
		t.Fatal("the refusal of the credential that was replaced closed a gate")
	}
}

// A gate that a sync closes or opens is noticed by the next heartbeat: it costs no request, so it does
// not wait for the interval.
func TestOriginCheckNoticesAGateThatASyncClosedOrOpened(t *testing.T) {
	e := newProbeEnv(t, oneCredentialedProviderJSON)
	e.startConfigured(t, "r1")
	if oc := e.check(); oc == nil || oc.Result != "reachable" {
		t.Fatalf("setup: origin_check = %+v", oc)
	}
	b := e.plan(t, "a").bind
	dials := len(e.p.all())

	e.c.sched.gates.arm(b, e.clock.now(), time.Hour) // a sync's 401
	e.heartbeat(t)
	e.settleProbe(t)
	if oc := e.check(); oc == nil || oc.Result != "access_denied" {
		t.Fatalf("origin_check = %+v after a sync's 401, want access_denied at the next heartbeat, not in 15 minutes", oc)
	}
	if n := len(e.p.all()); n != dials {
		t.Fatalf("%d dials to notice a closed gate, want none", n-dials)
	}

	e.c.sched.gates.succeeded(b, e.clock.now().Add(time.Second)) // a sync got through
	e.heartbeat(t)
	e.settleProbe(t)
	if oc := e.check(); oc == nil || oc.Result != "reachable" {
		t.Fatalf("origin_check = %+v after the gate opened, want reachable at the next heartbeat", oc)
	}
	if n := len(e.p.all()); n != dials+1 {
		t.Fatalf("%d dials to confirm recovery, want 1", n-dials)
	}
}

// A configuration that arrives while the previous check is sending its status is not lost: the base URL
// result it invalidated does not stand for another heartbeat.
func TestOriginCheckKickIsNotLostWhileTheLastCheckIsFinishing(t *testing.T) {
	e := newProbeEnv(t, oneCredentialedProviderJSON)
	var once sync.Once
	e.s.onSend = func(m proto.GitSyncStatusMsg) {
		if m.OriginCheck != nil && m.OriginCheck.Scope == proto.OriginCheckScopeHost {
			once.Do(func() {
				// The platform answers the status with a configuration. It is applied from another
				// goroutine (HandleMessage sends a status of its own, which waits for this one's lock),
				// and awaited, so it lands while this check is still sending.
				go e.configure("", "r1")
				for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
					e.c.mu.Lock()
					accepted := e.c.configAccepted
					e.c.mu.Unlock()
					if accepted {
						break
					}
				}
			})
		}
	}
	e.connect()
	eventually(t, func() bool { return len(e.p.repoURLs()) == 1 })
	e.settleProbe(t)
	if oc := e.check(); oc == nil || oc.Scope != proto.OriginCheckScopeRepository {
		t.Fatalf("origin_check = %+v, want the repository's without waiting for a heartbeat", oc)
	}
}

// What a check is kept as being about is what it dialled: a second repository that was planned but
// never asked leaving the configuration does not cost a login.
func TestOriginCheckIsNotRedialledWhenAnUnaskedRepositoryLeaves(t *testing.T) {
	e := newProbeEnv(t, oneCredentialedProviderJSON)
	e.startConfigured(t, "r1", "r2")
	if got := e.p.repoURLs(); len(got) != 1 {
		t.Fatalf("setup: dialled %v, want only r1 (it passed)", got)
	}
	e.configure("", "r1")
	e.settleProbe(t)
	if got := e.p.repoURLs(); len(got) != 1 {
		t.Fatalf("dialled %v: r2 was never asked, so its leaving is no reason to ask r1 again", got)
	}
}

// A repository signal from the first repository is kept when the second cannot be asked.
func TestOriginCheckKeepsTheFirstAnswerWhenTheSecondDialFaults(t *testing.T) {
	e := newProbeEnv(t, oneCredentialedProviderJSON)
	r1 := repoURL("git.example.com", "r1")
	e.connect()
	e.settleProbe(t)
	e.p.set(func(p *scriptedRepoProber) {
		p.repo = map[string]RepoCheck{r1: {Result: OriginReachable, ErrorType: ErrOriginAccessDenied}}
		p.onDial = func(d dialRec) {
			if d.url == repoURL("git.example.com", "r2") {
				p.mu.Lock()
				p.fault = errors.New("git could not be started")
				p.mu.Unlock()
			}
		}
	})
	e.configure("", "r1", "r2")
	e.settleProbe(t)
	if oc := e.check(); oc == nil || oc.Result != "reachable" || oc.ErrorType != "origin_access_denied" {
		t.Fatalf("origin_check = %+v, want the first repository's answer", oc)
	}
}

// Nothing a check logs or sends carries a credential, however it ends.
func TestOriginCheckLogsAndStatusesCarryNoCredential(t *testing.T) {
	var logs lockedLog
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newProbeEnv(t, twoProvidersJSON)
	e.originURL = func(id string) string {
		return map[string]string{"ra": repoURL("git.example.com", "ra"), "rb": repoURL("git2.example.com", "rb")}[id]
	}
	e.p.set(func(p *scriptedRepoProber) {
		p.repo = map[string]RepoCheck{repoURL("git.example.com", "ra"): {Result: OriginAccessDenied, ErrorType: ErrOriginAccessDenied, CredentialRejected: true}}
		p.fault = nil
	})
	e.startConfigured(t, "ra", "rb") // a refusal, which logs that a gate was closed
	e.p.set(func(p *scriptedRepoProber) { p.fault = errors.New("git could not be started") })
	e.clock.advance(healthyProbeInterval)
	e.heartbeat(t)
	e.settleProbe(t) // and a local fault, which logs why the check could not run

	out := logs.String()
	if !strings.Contains(out, "origin rejected the provider credential") || !strings.Contains(out, "could not run") {
		t.Fatalf("setup: the log does not show both events:\n%s", out)
	}
	sent := ""
	for _, oc := range e.s.origins() {
		sent += fmt.Sprintf("%+v %#v ", *oc, *oc)
	}
	for _, secret := range []string{tokenA, tokenB} {
		if strings.Contains(out, secret) || strings.Contains(sent, secret) {
			t.Errorf("a credential reached the log or a status")
		}
	}
	for _, leak := range []string{"git.example.com", "group/"} {
		if strings.Contains(out, leak) {
			t.Errorf("the log names %q: a host or repository", leak)
		}
	}
}

type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A check that is cancelled keeps what it already found, for the providers whose targets did not
// change: dialling them again would only cost the origin another login.
func TestOriginCheckCancelledPassKeepsTheProvidersItFinished(t *testing.T) {
	e := newProbeEnv(t, twoProvidersJSON)
	ra, rb, rb2 := repoURL("git.example.com", "ra"), repoURL("git2.example.com", "rb"), repoURL("git2.example.com", "rb2")
	e.originURL = func(id string) string { return map[string]string{"ra": ra, "rb": rb, "rb2": rb2}[id] }
	e.connect()
	e.settleProbe(t)
	e.p.set(func(p *scriptedRepoProber) { p.hold, p.holdOnly = make(chan struct{}), rb }) // provider b's dial waits
	e.configure("", "ra", "rb")
	eventually(t, func() bool { return slices.Contains(e.p.repoURLs(), rb) })

	// Provider b's repository is replaced while it is being dialled: that check is stopped; provider
	// a's, which finished, is not about anything that changed.
	e.p.set(func(p *scriptedRepoProber) { p.hold = nil })
	e.configure("", "ra", "rb2")
	eventually(t, func() bool { return e.p.cancelled.Load() == 1 })
	e.settleProbe(t)

	count := map[string]int{}
	for _, u := range e.p.repoURLs() {
		count[u]++
	}
	if count[ra] != 1 || count[rb] != 1 || count[rb2] != 1 {
		t.Fatalf("repository dials = %v, want each of ra, rb (cancelled) and rb2 once", count)
	}
}

// A gate a sync closes while the check's dial is in flight does not leave a reachable answer standing
// for a whole interval: the next heartbeat replaces it with the held answer, without a dial.
func TestOriginCheckGateClosedDuringTheDialIsStillNoticed(t *testing.T) {
	e := newProbeEnv(t, oneCredentialedProviderJSON)
	e.connect()
	e.settleProbe(t)
	bind := e.plan(t, "a").bind
	e.p.set(func(p *scriptedRepoProber) {
		p.onDial = func(d dialRec) {
			if d.repo {
				e.c.sched.gates.arm(bind, e.clock.now(), time.Hour) // a sync's 401, while this dial is in flight
			}
		}
	})
	e.configure("", "r1")
	e.settleProbe(t)
	if oc := e.check(); oc == nil || oc.Result != "reachable" {
		t.Fatalf("setup: origin_check = %+v, want the dial's own answer", oc)
	}
	dials := len(e.p.all())
	e.heartbeat(t)
	e.settleProbe(t)
	if oc := e.check(); oc == nil || oc.Result != "access_denied" {
		t.Fatalf("origin_check = %+v at the next heartbeat, want access_denied while the credential is held", oc)
	}
	if n := len(e.p.all()); n != dials {
		t.Fatalf("%d dials to notice the gate, want none", n-dials)
	}
}
