package gitsync

import (
	"os"
	"testing"
	"time"
)

// These tests run a real Client over a fake git to check that the provider gate is wired to what
// starts a sync: the queue a configuration fills, an immediate-sync request, and a credential reload.

const gateProviderJSON = `{"origin":{"base_url":"https://git.example.com/","auth":"bearer","token":"0123456789abcdef-A"}}`

// gateEnv starts a client with one slot, four repositories and a first sync of r1 that is held in
// its fetch, so r2..r4 wait in the queue as they do after a restart. Releasing it makes the fetch
// fail with the given status.
func gateEnv(t *testing.T, providers string, status int32) *schedEnv {
	t.Helper()
	e := newSchedEnvProviders(t, nil, providers)
	e.limits = `{"max_concurrency":1}`
	e.failStatus.Store(status)
	release := e.holdSyncs()
	e.configure("", "r1", "r2", "r3", "r4")
	eventually(t, func() bool { return e.fetches.Load() == 1 })
	e.failing.Store(true)
	close(release)
	e.gate.Store(nil)
	e.settle(t)
	return e
}

func (e *schedEnv) syncNow(repoID string) {
	raw := `{"type":"git_sync_now"`
	if repoID != "" {
		raw += `,"repo_id":"` + repoID + `"`
	}
	e.c.HandleMessage(e.ctx, schedStubSender{}, "git_sync_now", []byte(raw+`}`))
}

func TestClientRejectedCredentialStopsTheSyncsStillQueued(t *testing.T) {
	e := gateEnv(t, gateProviderJSON, 401)
	if got := e.fetches.Load(); got != 1 {
		t.Fatalf("%d fetches: the queued repositories should not log in after the first was rejected", got)
	}
}

func TestClientRepositoryAccessDeniedDoesNotStopTheQueue(t *testing.T) {
	// A 403 is the credential lacking access to one repository, not the credential being refused.
	e := gateEnv(t, gateProviderJSON, 403)
	if got := e.fetches.Load(); got != 4 {
		t.Fatalf("%d fetches, want all 4", got)
	}
}

func TestClientProviderWithoutACredentialIsNeverGated(t *testing.T) {
	// Without a credential a 401 only says the repository is private.
	e := gateEnv(t, schedProviderJSON, 401)
	if got := e.fetches.Load(); got != 4 {
		t.Fatalf("%d fetches, want all 4", got)
	}
}

func TestClientImmediateSyncOfOneRepositoryGetsOneAttemptAndEveryoneElseWaits(t *testing.T) {
	e := gateEnv(t, gateProviderJSON, 401)
	before := e.fetches.Load()

	e.syncNow("") // for all of them: not told apart from a loop
	e.settle(t)
	if got := e.fetches.Load(); got != before {
		t.Fatalf("a request for every repository made %d fetches while the credential is held back", got-before)
	}

	e.syncNow("r3")
	e.settle(t)
	if got := e.fetches.Load(); got != before+1 {
		t.Fatalf("a request for r3 made %d fetches, want 1", got-before)
	}

	e.syncNow("r3") // the same moment: rate limited
	e.settle(t)
	if got := e.fetches.Load(); got != before+1 {
		t.Fatalf("a repeated request made %d fetches in all, want 1", got-before)
	}

	e.clock.advance(manualPassGap + time.Second)
	e.syncNow("r3")
	e.settle(t)
	if got := e.fetches.Load(); got != before+2 {
		t.Fatalf("a request after the gap made %d fetches in all, want 2", got-before)
	}
}

func TestClientCredentialChangeReleasesTheGate(t *testing.T) {
	e := gateEnv(t, gateProviderJSON, 401)
	before := e.fetches.Load()
	e.failing.Store(false)

	rotated := `{"origin":{"base_url":"https://git.example.com/","auth":"bearer","token":"0123456789abcdef-B"}}`
	if err := os.WriteFile(e.providerPath, []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(e.providerPath)
	if err != nil {
		t.Fatal(err)
	}
	e.c.setProviders(store)
	e.settle(t)
	if got := e.fetches.Load() - before; got != 4 {
		t.Fatalf("%d repositories synced with the new credential, want all 4", got)
	}
}
