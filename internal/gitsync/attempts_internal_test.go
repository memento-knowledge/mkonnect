package gitsync

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
)

// A finisher that is no longer the repository's current attempt must not end the current one, clear
// its syncing mark, or record anything: the registration and the repository's state belong to the
// newer attempt.
func TestStaleFinisherLeavesTheCurrentAttemptAlone(t *testing.T) {
	store := &Store{data: map[string]LocalEntry{"origin": {
		BaseURL: "https://git.example.com/", Auth: "bearer", Token: Secret("origin-token-one-aaaaaaaa"),
	}}}
	cfg := &config.Config{GatewayURL: "wss://gw.example.com", ConnectorID: "gitsync-test", ProtocolVersion: "v1"}
	c := NewClient(cfg, auth.NewKeyStore(filepath.Join(t.TempDir(), "key")), store, nil, time.Now, "boot",
		WithReplicaHost("replica.example.com"), WithMirrorsRoot(t.TempDir()))
	raw, _ := json.Marshal(map[string]any{"type": "git_sync_config", "config_version": 1, "repositories": []map[string]any{{
		"repo_id": "repo-a", "origin_url": "https://git.example.com/a.git",
		"replica_push_url": "https://replica.example.com/r/a.git", "push_token": "push-token-abcdefghij",
	}}})
	if !c.applyConfig(raw) {
		t.Fatal("config rejected")
	}

	c.beginSync("repo-a")
	stale, _, ok := c.claimTask(context.Background(), "repo-a")
	if !ok {
		t.Fatal("first claim refused")
	}
	// The older attempt is gone from the registry and a newer one took its place (a state the manager
	// does not produce today; the guard must not depend on that).
	c.mu.Lock()
	delete(c.attempts, "repo-a")
	c.mu.Unlock()
	fresh, freshCtx, ok := c.claimTask(context.Background(), "repo-a")
	if !ok {
		t.Fatal("second claim refused")
	}
	if fresh.AttemptSeq == stale.AttemptSeq {
		t.Fatal("two claims share an identity")
	}

	if c.finishSync("repo-a", stale, syncResult{State: StateOK}) {
		t.Error("a stale finisher's result was kept")
	}
	if freshCtx.Err() != nil {
		t.Error("the stale finisher cancelled the current attempt")
	}
	c.mu.Lock()
	registered := c.attempts["repo-a"] != nil
	r := c.repos["repo-a"]
	c.mu.Unlock()
	if !registered {
		t.Error("the stale finisher removed the current attempt's registration")
	}
	if !r.syncing || r.result.State != "" {
		t.Errorf("repository state = syncing %v, result %q: the stale finisher changed what belongs to the current attempt", r.syncing, r.result.State)
	}

	// The current attempt still finishes normally.
	if !c.finishSync("repo-a", fresh, syncResult{State: StateOK}) {
		t.Error("the current attempt's result was dropped")
	}
}
