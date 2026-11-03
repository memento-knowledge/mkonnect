package gitsync_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

const (
	providerWithToken = `{"origin":{"base_url":"https://git.example.com/","auth":"bearer","token":"origin-token-abcdefghij"}}`
	originToken       = "origin-token-abcdefghij"
)

// scriptedGit is a thread-safe fake git for client tests. A non-nil gate holds every clone until
// released; fail makes a subcommand exit non-zero with the given stderr.
type scriptedGit struct {
	mu    sync.Mutex
	argvs [][]string
	envs  [][]string
	gate  chan struct{}
	fail  map[string][]byte
}

func subOf(argv []string) string {
	for _, a := range argv {
		switch a {
		case "clone", "fetch", "push":
			return a
		}
	}
	return ""
}

func (g *scriptedGit) run(ctx context.Context, argv, env []string, _ string) ([]byte, error) {
	g.mu.Lock()
	g.argvs = append(g.argvs, argv)
	g.envs = append(g.envs, env)
	gate, stderr := g.gate, g.fail[subOf(argv)]
	g.mu.Unlock()

	if gate != nil && subOf(argv) == "clone" {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if stderr != nil {
		return stderr, errors.New("exit status 128")
	}
	if subOf(argv) == "clone" {
		// A real clone creates the mirror, which is what makes the next sync a fetch.
		if err := os.MkdirAll(argv[len(argv)-1], 0o700); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (g *scriptedGit) count(sub string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, a := range g.argvs {
		if subOf(a) == sub {
			n++
		}
	}
	return n
}

// call returns the argv and env of the first invocation of sub.
func (g *scriptedGit) call(t *testing.T, sub string) (argv, env []string) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, a := range g.argvs {
		if subOf(a) == sub {
			return a, g.envs[i]
		}
	}
	t.Fatalf("git %s was never run", sub)
	return nil, nil
}

func rawJSON(s string) json.RawMessage { return json.RawMessage(s) }

func contains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func repoStatusNow(t *testing.T, s *stubSender, id string) proto.GitSyncRepoStatus {
	t.Helper()
	return repoStates(lastStatus(t, s.messages()))[id]
}

func TestConfigStartsSyncAndStatusReflectsResult(t *testing.T) {
	g := &scriptedGit{gate: make(chan struct{})}
	c := newTestClientOpts(t, providerWithToken, &fakeProber{result: gitsync.OriginReachable}, fixedNow, gitsync.WithGitRunner(g.run))
	s := &stubSender{}
	ctx := context.Background()
	c.OnConnected(ctx, s) // gives the client the connection it reports a finished sync over

	c.HandleMessage(ctx, s, "git_sync_config", configJSON("1", [3]string{"repo-a", "https://git.example.com/team/a.git", ""}))
	if got := repoStatusNow(t, s, "repo-a").State; got != gitsync.StateSyncing {
		t.Fatalf("state right after config = %q, want syncing", got)
	}

	waitFor(t, func() bool { return g.count("clone") == 1 })
	argv, env := g.call(t, "clone")
	if !contains(argv, "https://git.example.com:443/team/a.git") {
		t.Errorf("clone argv %v must dial the validated canonical URL", argv)
	}
	if !contains(env, "Authorization: Bearer "+originToken) || !contains(env, "http.https://git.example.com:443/team/a.git/.extraHeader") {
		t.Error("the matched provider's credential must be injected, scoped to the repository URL")
	}

	close(g.gate)
	// Completion sends a status over the current connection without waiting for a heartbeat.
	waitFor(t, func() bool { return repoStatusNow(t, s, "repo-a").State == gitsync.StateOK })
	_, pushEnv := g.call(t, "push")
	if !contains(pushEnv, "Bearer "+testPushToken) || contains(pushEnv, originToken) {
		t.Error("the push must carry the push token only, never the origin credential")
	}
	if contains(env, testPushToken) {
		t.Error("the clone must never carry the push token")
	}
}

func TestUnmatchedRepoIsNeverSynced(t *testing.T) {
	g := &scriptedGit{}
	c := newTestClientOpts(t, providerWithToken, &fakeProber{result: gitsync.OriginReachable}, fixedNow, gitsync.WithGitRunner(g.run))
	s := &stubSender{}
	ctx := context.Background()

	c.HandleMessage(ctx, s, "git_sync_config", configJSON("1", [3]string{"repo-b", "https://other.example.com/b.git", ""}))
	c.HandleMessage(ctx, s, "git_sync_now", rawJSON(`{"type":"git_sync_now"}`))
	c.HandleMessage(ctx, s, "git_sync_now", rawJSON(`{"type":"git_sync_now","repo_id":"repo-b"}`))

	time.Sleep(50 * time.Millisecond)
	if n := len(g.argvs); n != 0 {
		t.Errorf("git ran %d times for a repository with no matching local provider; the credential must never be used", n)
	}
	r := repoStatusNow(t, s, "repo-b")
	if r.State != gitsync.StateError || r.ErrorType != gitsync.ErrOriginAccessDenied {
		t.Errorf("repo-b = %+v, want error/origin_access_denied", r)
	}
}

func TestSyncNowRunsAnotherSync(t *testing.T) {
	g := &scriptedGit{}
	c := newTestClientOpts(t, providerWithToken, &fakeProber{result: gitsync.OriginReachable}, fixedNow, gitsync.WithGitRunner(g.run))
	s := &stubSender{}
	ctx := context.Background()
	c.OnConnected(ctx, s)

	c.HandleMessage(ctx, s, "git_sync_config", configJSON("1", [3]string{"repo-a", "https://git.example.com/team/a.git", ""}))
	waitFor(t, func() bool { return repoStatusNow(t, s, "repo-a").State == gitsync.StateOK })
	pushes := g.count("push")

	c.HandleMessage(ctx, s, "git_sync_now", rawJSON(`{"type":"git_sync_now","repo_id":"repo-a"}`))
	waitFor(t, func() bool { return g.count("push") == pushes+1 })
	waitFor(t, func() bool { return repoStatusNow(t, s, "repo-a").State == gitsync.StateOK })
	if g.count("fetch") == 0 {
		t.Error("a second sync of a repository with a mirror must fetch, not clone again")
	}
}

func TestCriticalPatchSuppressesSyncs(t *testing.T) {
	g := &scriptedGit{}
	c := newTestClientOpts(t, providerWithToken, &fakeProber{result: gitsync.OriginReachable}, fixedNow, gitsync.WithGitRunner(g.run))
	c.SetCriticalPatchForTest(true)
	s := &stubSender{}
	ctx := context.Background()

	c.HandleMessage(ctx, s, "git_sync_config", configJSON("1", [3]string{"repo-a", "https://git.example.com/team/a.git", ""}))
	c.HandleMessage(ctx, s, "git_sync_now", rawJSON(`{"type":"git_sync_now"}`))

	time.Sleep(50 * time.Millisecond)
	if n := len(g.argvs); n != 0 {
		t.Errorf("git ran %d times while a critical security update is required", n)
	}
	if got := repoStatusNow(t, s, "repo-a").State; got != gitsync.StatePending {
		t.Errorf("state = %q, want pending (config is accepted but nothing syncs)", got)
	}
}

func TestSyncFailureIsReportedAsTypedError(t *testing.T) {
	g := &scriptedGit{fail: map[string][]byte{
		"clone": []byte("fatal: unable to access 'https://git.example.com/team/a.git/': The requested URL returned error: 403"),
	}}
	c := newTestClientOpts(t, providerWithToken, &fakeProber{result: gitsync.OriginReachable}, fixedNow, gitsync.WithGitRunner(g.run))
	s := &stubSender{}
	ctx := context.Background()
	c.OnConnected(ctx, s)

	c.HandleMessage(ctx, s, "git_sync_config", configJSON("1", [3]string{"repo-a", "https://git.example.com/team/a.git", ""}))
	waitFor(t, func() bool { return repoStatusNow(t, s, "repo-a").State == gitsync.StateError })
	r := repoStatusNow(t, s, "repo-a")
	if r.ErrorType != gitsync.ErrOriginAccessDenied {
		t.Errorf("error_type = %q, want origin_access_denied", r.ErrorType)
	}
	if strings.Contains(r.ErrorDetail, "example.com") {
		t.Errorf("error_detail leaked a host: %q", r.ErrorDetail)
	}
	if g.count("push") != 0 {
		t.Error("nothing may be pushed after the origin failed")
	}
}

func TestOnDrainingWaitsBrieflyForInFlightSync(t *testing.T) {
	g := &scriptedGit{gate: make(chan struct{})}
	c := newTestClientOpts(t, providerWithToken, &fakeProber{result: gitsync.OriginReachable}, fixedNow,
		gitsync.WithGitRunner(g.run), gitsync.WithDrainTimeout(80*time.Millisecond))
	s := &stubSender{}
	ctx := context.Background()
	c.OnConnected(ctx, s)
	c.HandleMessage(ctx, s, "git_sync_config", configJSON("1", [3]string{"repo-a", "https://git.example.com/team/a.git", ""}))
	waitFor(t, func() bool { return g.count("clone") == 1 })

	start := time.Now()
	c.OnDraining(ctx)
	if elapsed := time.Since(start); elapsed < 70*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("OnDraining returned after %v, want it to wait for its ~80ms bound and then return", elapsed)
	}
	if got := repoStatusNow(t, s, "repo-a").State; got != gitsync.StateSyncing {
		t.Errorf("state after drain = %q, want the sync left running", got)
	}

	close(g.gate)
	waitFor(t, func() bool { return repoStatusNow(t, s, "repo-a").State == gitsync.StateOK })
	start = time.Now()
	c.OnDraining(ctx)
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("OnDraining with nothing in flight took %v, want it immediate", elapsed)
	}
}

// Every finished sync used to send a full status that echoes every repository, so a fan-out over N
// repositories cost N statuses of O(N) size each. Completions are coalesced: the first is sent at
// once, the rest within one interval, and the final state is always flushed.
func TestCompletionStatusesAreCoalesced(t *testing.T) {
	g := &scriptedGit{}
	c := newTestClientOpts(t, providerWithToken, &fakeProber{result: gitsync.OriginReachable}, fixedNow,
		gitsync.WithGitRunner(g.run), gitsync.WithStatusInterval(150*time.Millisecond))
	s := &stubSender{}
	ctx := context.Background()
	c.OnConnected(ctx, s)

	const n = 40
	objs := make([]string, n)
	for i := range objs {
		id := fmt.Sprintf("repo-%02d", i)
		objs[i] = repoJSON(id, "https://git.example.com/team/"+id+".git", "", "https://replica.example.com/r/"+id+".git", testPushToken)
	}
	c.HandleMessage(ctx, s, "git_sync_config", configJSONRaw("1", objs...))

	waitFor(t, func() bool {
		for _, r := range lastStatus(t, s.messages()).Repositories {
			if r.State != gitsync.StateOK {
				return false
			}
		}
		return true
	})
	statuses := 0
	for _, m := range s.messages() {
		if _, ok := m.(proto.GitSyncStatusMsg); ok {
			statuses++
		}
	}
	if statuses > 12 {
		t.Errorf("sent %d statuses for %d completions; want them coalesced (an uncoalesced run sends ~%d)", statuses, n, n+2)
	}
}

// A result recorded for a target the platform has since reconfigured must never be attributed to the
// new target: a repository moved to a new replica mid-sync is not "ok" because the OLD push was.
func TestResultForAReconfiguredTargetIsIgnored(t *testing.T) {
	c := newTestClient(t, oneProvider)
	c.SetCriticalPatchForTest(true) // nothing syncs, so only the explicit results below are recorded
	s := &stubSender{}
	ctx := context.Background()
	apply := func(replica string) {
		c.HandleMessage(ctx, s, "git_sync_config", configJSONRaw("1",
			repoJSON("repo-a", "https://git.example.com/a.git", "", replica, testPushToken)))
	}
	state := func() proto.GitSyncRepoStatus {
		_ = c.OnHeartbeat(ctx, s)
		return repoStatusNow(t, s, "repo-a")
	}

	apply("https://replica.example.com/r/old.git")
	_, canon, _, _ := c.RepoStateForTest("repo-a")
	oldReplica, _, _ := c.ReplicaMaterialForTest("repo-a")
	apply("https://replica.example.com/r/new.git") // reconfigured while the old sync was running
	newReplica, _, _ := c.ReplicaMaterialForTest("repo-a")

	c.FinishSyncForTest("repo-a", canon, oldReplica, gitsync.StateError, gitsync.ErrReplicaRejected)
	if got := state(); got.State != gitsync.StatePending || got.ErrorType != "" {
		t.Errorf("a stale result was attributed to the new target: %+v, want pending", got)
	}
	c.FinishSyncForTest("repo-a", canon, newReplica, gitsync.StateOK, "")
	if got := state(); got.State != gitsync.StateOK {
		t.Errorf("a result for the current target was ignored: %+v, want ok", got)
	}
}

// A sync that finishes after its connection has gone must not write to that connection (its sender
// is bound to it); the next heartbeat on the new connection reports the result instead.
func TestCompletionAfterConnectionClosedSendsNothingOnIt(t *testing.T) {
	g := &scriptedGit{gate: make(chan struct{})}
	c := newTestClientOpts(t, providerWithToken, &fakeProber{result: gitsync.OriginReachable}, fixedNow, gitsync.WithGitRunner(g.run))
	connCtx, closeConn := context.WithCancel(context.Background())
	conn := &stubSender{}
	c.OnConnected(connCtx, conn)

	c.HandleMessage(context.Background(), &stubSender{}, "git_sync_config",
		configJSON("1", [3]string{"repo-a", "https://git.example.com/team/a.git", ""}))
	waitFor(t, func() bool { return g.count("clone") == 1 })

	before := len(conn.messages())
	closeConn()
	close(g.gate)
	waitFor(t, func() bool { return g.count("push") == 1 })
	time.Sleep(50 * time.Millisecond)
	if after := len(conn.messages()); after != before {
		t.Errorf("a finished sync wrote %d message(s) to a closed connection", after-before)
	}
}

func TestMirrorsRootIsMadeAbsolute(t *testing.T) {
	c := newTestClientOpts(t, oneProvider, &fakeProber{result: gitsync.OriginReachable}, fixedNow,
		gitsync.WithMirrorsRoot("relative/mirrors"))
	root := c.MirrorsRootForTest()
	if !filepath.IsAbs(root) || !strings.HasSuffix(root, filepath.Join("relative", "mirrors")) {
		t.Errorf("mirrors root = %q, want an absolute path: git runs in a temp dir, so a relative root would clone into it and lose the mirror", root)
	}
}

// At shutdown the process-lifetime context is cancelled, which kills each git; Wait lets the daemon
// confirm they have stopped before it exits rather than orphan a process that holds a credential.
func TestWaitReturnsOnceCancelledSyncsHaveStopped(t *testing.T) {
	g := &scriptedGit{gate: make(chan struct{})}
	c := newTestClientOpts(t, providerWithToken, &fakeProber{result: gitsync.OriginReachable}, fixedNow, gitsync.WithGitRunner(g.run))
	ctx, cancel := context.WithCancel(context.Background())
	c.HandleMessage(ctx, &stubSender{}, "git_sync_config",
		configJSON("1", [3]string{"repo-a", "https://git.example.com/team/a.git", ""}))
	waitFor(t, func() bool { return g.count("clone") == 1 })
	cancel()

	wctx, wcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer wcancel()
	start := time.Now()
	c.Wait(wctx)
	if wctx.Err() != nil || time.Since(start) > 2*time.Second {
		t.Errorf("Wait ran out its bound (%v) although the cancelled sync had stopped", time.Since(start))
	}
}

func TestApplyConfigRetainsReplicaMaterialForMatched(t *testing.T) {
	c := newTestClient(t, oneProvider)
	c.HandleMessage(context.Background(), &stubSender{}, "git_sync_config", configJSONRaw("1",
		repoJSON("repo-a", "https://git.example.com/team/a.git", "", "https://REPLICA.example.com/r/a.git", testPushToken),
		repoJSON("repo-b", "https://other.example.com/b.git", "", "https://replica.example.com/r/b.git", testPushToken)))

	replica, token, found := c.ReplicaMaterialForTest("repo-a")
	if !found {
		t.Fatal("repo-a not retained")
	}
	if want := "https://replica.example.com:443/r/a.git"; replica != want {
		t.Errorf("replica URL = %q, want the validated canonical form %q", replica, want)
	}
	if token != testPushToken {
		t.Error("the push token was not retained for a matched repository")
	}

	// A repository that cannot be synced must not hold a push token it will never use.
	replica, token, found = c.ReplicaMaterialForTest("repo-b")
	if !found || replica != "" || token != "" {
		t.Errorf("unmatched repo-b retained replica=%q token-present=%v, want neither", replica, token != "")
	}
}

func TestApplyConfigMalformedReplicaMarksRepoNotWholeConfig(t *testing.T) {
	c := newTestClient(t, oneProvider)
	s := &stubSender{}
	c.HandleMessage(context.Background(), s, "git_sync_config", configJSONRaw("5",
		repoJSON("good", "https://git.example.com/g.git", "", "https://replica.example.com/r/g.git", testPushToken),
		repoJSON("plain-http", "https://git.example.com/h.git", "", "http://replica.example.com/r/h.git", testPushToken),
		repoJSON("no-replica", "https://git.example.com/n.git", "", "", testPushToken),
		repoJSON("no-token", "https://git.example.com/t.git", "", "https://replica.example.com/r/t.git", ""),
		repoJSON("userinfo", "https://git.example.com/u.git", "", "https://user:pw@replica.example.com/r/u.git", testPushToken),
		// The token becomes an Authorization header value: a CR/LF would inject headers.
		repoJSON("ctl-token", "https://git.example.com/c.git", "", "https://replica.example.com/r/c.git", "push-token-abcdef\r\nX-Evil: yes")))

	st := lastStatus(t, s.messages())
	if string(st.ConfigVersion) != "5" {
		t.Fatalf("config_version = %s, want 5: one bad repository must not reject the whole config", st.ConfigVersion)
	}
	rs := repoStates(st)
	if rs["good"].State != gitsync.StateSyncing {
		t.Errorf("good = %+v, want syncing", rs["good"])
	}
	for _, id := range []string{"plain-http", "no-replica", "no-token", "userinfo", "ctl-token"} {
		r := rs[id]
		if r.State != gitsync.StateError || r.ErrorType != gitsync.ErrUnclassified || r.ErrorDetail != "invalid replica configuration" {
			t.Errorf("%s = %+v, want error/unclassified/invalid replica configuration", id, r)
		}
		if strings.Contains(r.ErrorDetail, "replica.example.com") {
			t.Errorf("%s: error_detail leaked the replica host", id)
		}
		if replica, _, _ := c.ReplicaMaterialForTest(id); replica != "" {
			t.Errorf("%s retained an unusable replica URL %q", id, replica)
		}
	}
}

func TestApplyConfigCarriesForwardResultForUnchangedRepo(t *testing.T) {
	apply := func(c *gitsync.Client, origin, replica string) proto.GitSyncRepoStatus {
		s := &stubSender{}
		c.HandleMessage(context.Background(), s, "git_sync_config", configJSONRaw("1",
			repoJSON("repo-a", origin, "", replica, testPushToken)))
		return repoStates(lastStatus(t, s.messages()))["repo-a"]
	}
	const origin, replica = "https://git.example.com/a.git", "https://replica.example.com/r/a.git"

	// With syncing suspended nothing overwrites the recorded result, so what a re-sent configuration
	// reports is purely whether it carried the result forward.
	c := newTestClient(t, oneProvider)
	c.SetCriticalPatchForTest(true)
	apply(c, origin, replica)
	c.RecordResultForTest("repo-a", gitsync.StateOK, "")

	if got := apply(c, origin, replica); got.State != gitsync.StateOK {
		t.Errorf("an unchanged repository was reset to %q; want its prior ok result carried forward", got.State)
	}
	if got := apply(c, "https://git.example.com/other.git", replica); got.State != gitsync.StatePending {
		t.Errorf("a changed origin kept a stale result: %+v, want pending", got)
	}

	c.RecordResultForTest("repo-a", gitsync.StateOK, "")
	if got := apply(c, "https://git.example.com/other.git", "https://replica.example.com/r/moved.git"); got.State != gitsync.StatePending {
		t.Errorf("a changed replica kept a stale result: %+v, want pending", got)
	}
}
