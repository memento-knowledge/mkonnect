package gitsync_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

const (
	pinnedHost    = "replica.example.com"
	goodOrigin    = "https://git.example.com/team/a.git"
	goodReplica   = "https://replica.example.com/r/a.git"
	goodReplicaCN = "https://replica.example.com:443/r/repo-a.git"
)

// total counts every git invocation, whatever the subcommand.
func (g *scriptedGit) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.argvs)
}

// anyCallMentions reports whether any git argv or env contains sub.
func (g *scriptedGit) anyCallMentions(sub string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.argvs {
		if contains(g.argvs[i], sub) || contains(g.envs[i], sub) {
			return true
		}
	}
	return false
}

// pinTest is a client wired to a scripted git, with the pin chosen by the test. Its cleanup waits
// for any sync it started to stop, so no background work outlives the test.
type pinTest struct {
	c *gitsync.Client
	g *scriptedGit
	s *stubSender
}

func newPinTest(t *testing.T, opts ...gitsync.Option) *pinTest {
	t.Helper()
	g := &scriptedGit{}
	c := newTestClientOpts(t, providerWithToken, &fakeProber{result: gitsync.OriginReachable}, fixedNow,
		append([]gitsync.Option{gitsync.WithGitRunner(g.run)}, opts...)...)
	p := &pinTest{c: c, g: g, s: &stubSender{}}
	p.c.OnConnected(context.Background(), p.s)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.Wait(ctx)
	})
	return p
}

func (p *pinTest) send(t *testing.T, raw string) proto.GitSyncStatusMsg {
	t.Helper()
	p.c.HandleMessage(context.Background(), p.s, "git_sync_config", json.RawMessage(raw))
	return lastStatus(t, p.s.messages())
}

func (p *pinTest) status(t *testing.T) proto.GitSyncStatusMsg {
	t.Helper()
	return lastStatus(t, p.s.messages())
}

func (p *pinTest) settle(t *testing.T) { settleClient(t, p.c) }

// settleClient waits for every started sync to stop, so a "git never ran" assertion made afterwards
// cannot pass merely because a sync had not reached git yet.
func settleClient(t *testing.T, c *gitsync.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Wait(ctx)
}

func goodRepo(id string) string {
	return repoJSON(id, "https://git.example.com/team/"+id+".git", "", "https://replica.example.com/r/"+id+".git", testPushToken)
}

// newBareClient builds a client with NO replica-host option, as production does when the pin is
// absent, so a test can prove the default fails closed. localStore false passes a nil store.
func newBareClient(t *testing.T, localStore bool, opts ...gitsync.Option) *gitsync.Client {
	t.Helper()
	var local *gitsync.Store
	if localStore {
		path := filepath.Join(t.TempDir(), "git-sync.json")
		if err := os.WriteFile(path, []byte(providerWithToken), 0o600); err != nil {
			t.Fatal(err)
		}
		var err error
		if local, err = gitsync.New(path); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{GatewayURL: "wss://gw.example.com", ConnectorID: "gitsync-test", ProtocolVersion: "v1"}
	c := gitsync.NewClient(cfg, auth.NewKeyStore(filepath.Join(t.TempDir(), "key")), local,
		&fakeProber{result: gitsync.OriginReachable}, fixedNow, "boot-test",
		append([]gitsync.Option{gitsync.WithMirrorsRoot(t.TempDir())}, opts...)...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.Wait(ctx)
	})
	return c
}

// wantConfigError asserts the status carries config_error{version, reason}; version "null" means
// the wire value is null (no usable version).
func wantConfigError(t *testing.T, st proto.GitSyncStatusMsg, version, reason string) {
	t.Helper()
	if st.ConfigError == nil {
		t.Fatalf("status has no config_error, want reason %q", reason)
	}
	if st.ConfigError.Reason != reason {
		t.Errorf("config_error.reason = %q, want %q", st.ConfigError.Reason, reason)
	}
	b, err := json.Marshal(st.ConfigError.ConfigVersion)
	if err != nil {
		t.Fatalf("marshal config_error.config_version: %v", err)
	}
	if string(b) != version {
		t.Errorf("config_error.config_version on the wire = %s, want %s", b, version)
	}
}

// lockedBuffer is a log sink safe for concurrent writers.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLogs routes the default slog logger to a buffer for the test's duration.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// --- No pin: fail closed ---

func TestNoPinRejectsEveryConfigAndNeverRunsGit(t *testing.T) {
	configs := map[string]string{
		"good":            string(configJSONRaw("1", goodRepo("repo-a"))),
		"empty set":       string(configJSONRaw("2")),
		"malformed":       `{"type":"git_sync_config","config_version":3,"repositories":`,
		"no repositories": `{"type":"git_sync_config","config_version":4}`,
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			p := newPinTest(t, gitsync.WithReplicaHost(""))
			st := p.send(t, cfg)
			if st.ConfigError == nil || st.ConfigError.Reason != proto.GitSyncConfigErrReplicaHostNotPinned {
				t.Fatalf("config_error = %+v, want reason %q", st.ConfigError, proto.GitSyncConfigErrReplicaHostNotPinned)
			}
			if st.ConfigVersion != nil || len(st.Repositories) != 0 {
				t.Errorf("an unpinned connector accepted state: version=%s repos=%d", st.ConfigVersion, len(st.Repositories))
			}
			p.c.HandleMessage(context.Background(), p.s, "git_sync_now", rawJSON(`{"type":"git_sync_now"}`))
			p.settle(t)
			if n := p.g.total(); n != 0 {
				t.Errorf("git ran %d times with no pinned replica host", n)
			}
		})
	}
}

// TestDefaultClientIsNotPinned proves the production default (no WithReplicaHost option) fails
// closed, independent of the pin the shared test helper installs.
func TestDefaultClientIsNotPinned(t *testing.T) {
	g := &scriptedGit{}
	c := newBareClient(t, true, gitsync.WithGitRunner(g.run))
	s := &stubSender{}
	c.HandleMessage(context.Background(), s, "git_sync_config", configJSONRaw("1", goodRepo("repo-a")))
	wantConfigError(t, lastStatus(t, s.messages()), "1", proto.GitSyncConfigErrReplicaHostNotPinned)
	settleClient(t, c)
	if g.total() != 0 {
		t.Error("git ran on a client with no pin option")
	}
}

func TestInvalidPinBehavesAsNoPinAndLogsNoValue(t *testing.T) {
	for _, bad := range []string{
		"https://replica.example.com/leak-marker", "replica.example.com\n", "repl\u0130ca.example.com", "replica.example.com/x",
		"user:leak-marker@replica.example.com", "replica.example.com:99999",
	} {
		logs := captureLogs(t)
		p := newPinTest(t, gitsync.WithReplicaHost(bad))
		st := p.send(t, string(configJSONRaw("1", goodRepo("repo-a"))))
		if st.ConfigError == nil || st.ConfigError.Reason != proto.GitSyncConfigErrReplicaHostNotPinned {
			t.Errorf("pin %q: config_error = %+v, want replica_host_not_pinned", bad, st.ConfigError)
		}
		p.settle(t)
		if p.g.total() != 0 {
			t.Errorf("pin %q: git ran", bad)
		}
		out := logs.String()
		if !strings.Contains(out, gitsync.ReplicaHostEnv) {
			t.Errorf("pin %q: no startup error naming %s in logs: %q", bad, gitsync.ReplicaHostEnv, out)
		}
		if strings.Contains(out, "leak-marker") || strings.Contains(out, bad) {
			t.Errorf("pin %q: the invalid value was echoed to the logs: %q", bad, out)
		}
	}
}

// --- Pin set: matching URL syncs, and only to the pin ---

func TestPinnedMatchingReplicaSyncsAndTokenGoesOnlyToThePin(t *testing.T) {
	p := newPinTest(t, gitsync.WithReplicaHost(pinnedHost))
	st := p.send(t, string(configJSONRaw("1", goodRepo("repo-a"))))
	if st.ConfigError != nil {
		t.Fatalf("a matching replica was rejected: %+v", st.ConfigError)
	}
	waitFor(t, func() bool { return repoStatusNow(t, p.s, "repo-a").State == gitsync.StateOK })

	argv, env := p.g.call(t, "push")
	if !contains(argv, goodReplicaCN) || !contains(env, "http."+goodReplicaCN+"/.extraHeader") {
		t.Errorf("push must dial and scope the token to %s; argv=%v", goodReplicaCN, argv)
	}
	// Every URL any git invocation saw is the matched origin or the pinned replica.
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	for i := range p.g.argvs {
		for _, a := range append(append([]string{}, p.g.argvs[i]...), p.g.envs[i]...) {
			for _, field := range strings.Fields(strings.NewReplacer("=", " ", ",", " ").Replace(a)) {
				if strings.Contains(field, "://") && !strings.Contains(field, "git.example.com") && !strings.Contains(field, pinnedHost) {
					t.Errorf("git saw a URL that is neither the origin nor the pin: %q", field)
				}
			}
		}
	}
}

func TestPinMatchesCanonicalForms(t *testing.T) {
	for _, replica := range []string{
		"https://REPLICA.example.COM/r/a.git", "https://replica.example.com:443/r/a.git", "https://replica.example.com./r/a.git",
	} {
		p := newPinTest(t, gitsync.WithReplicaHost("Replica.Example.com."))
		st := p.send(t, string(configJSONRaw("1", repoJSON("repo-a", goodOrigin, "", replica, testPushToken))))
		if st.ConfigError != nil {
			t.Errorf("%q rejected: %+v", replica, st.ConfigError)
			continue
		}
		if got, _, _ := p.c.ReplicaMaterialForTest("repo-a"); got != "https://replica.example.com:443/r/a.git" {
			t.Errorf("%q retained %q, want the canonical form", replica, got)
		}
	}
}

func TestPinMayCarryAnExplicitPort(t *testing.T) {
	p := newPinTest(t, gitsync.WithReplicaHost("replica.example.com:8443"))
	st := p.send(t, string(configJSONRaw("1",
		repoJSON("repo-a", goodOrigin, "", "https://replica.example.com:8443/r/a.git", testPushToken))))
	if st.ConfigError != nil {
		t.Fatalf("pinned port rejected: %+v", st.ConfigError)
	}
	st = p.send(t, string(configJSONRaw("2", goodRepo("repo-a")))) // default 443 is not the pinned 8443
	wantConfigError(t, st, "2", proto.GitSyncConfigErrReplicaHostMismatch)
}

func TestPinnedIPv6(t *testing.T) {
	p := newPinTest(t, gitsync.WithReplicaHost("[2001:db8::1]:8443"))
	st := p.send(t, string(configJSONRaw("1",
		repoJSON("repo-a", goodOrigin, "", "https://[2001:DB8:0:0:0:0:0:1]:8443/r/a.git", testPushToken))))
	if st.ConfigError != nil {
		t.Fatalf("IPv6 pin did not match its own address: %+v", st.ConfigError)
	}
	st = p.send(t, string(configJSONRaw("2",
		repoJSON("repo-a", goodOrigin, "", "https://[2001:db8::2]:8443/r/a.git", testPushToken))))
	wantConfigError(t, st, "2", proto.GitSyncConfigErrReplicaHostMismatch)
}

// --- Mismatch: whole-config rejection ---

var mismatchReplicas = map[string]string{
	"other host":        "https://evil.example/r/a.git",
	"subdomain":         "https://a.replica.example.com/r/a.git",
	"parent domain":     "https://example.com/r/a.git",
	"different port":    "https://replica.example.com:8443/r/a.git",
	"http scheme":       "http://replica.example.com/r/a.git",
	"userinfo":          "https://replica.example.com@evil.example/r/a.git",
	"double trailing":   "https://replica.example.com../r/a.git",
	"ipv6":              "https://[::1]/r/a.git",
	"kelvin sign":       "https://replica.exampl\u212a.com/r/a.git",
	"dotted capital I":  "https://repl\u0130ca.example.com/r/a.git",
	"fullwidth":         "https://\uff52eplica.example.com/r/a.git",
	"ideographic stop":  "https://replica\u3002example.com/r/a.git",
	"percent-encoded":   "https://repl%C4%B0ca.example.com/r/a.git",
	"percent-encoded .": "https://evil.example%2e/r/a.git",
	"backslash":         "https://evil.example\\@replica.example.com/r/a.git",
}

func TestMismatchRejectsWholeConfigAndNothingSyncs(t *testing.T) {
	for name, bad := range mismatchReplicas {
		t.Run(name, func(t *testing.T) {
			p := newPinTest(t, gitsync.WithReplicaHost(pinnedHost))
			st := p.send(t, string(configJSONRaw("9",
				goodRepo("repo-a"),
				repoJSON("repo-b", "https://git.example.com/team/b.git", "", bad, testPushToken),
				goodRepo("repo-c"))))
			wantConfigError(t, st, "9", proto.GitSyncConfigErrReplicaHostMismatch)
			if st.ConfigVersion != nil || len(st.Repositories) != 0 {
				t.Errorf("a rejected config left state behind: version=%s repos=%d", st.ConfigVersion, len(st.Repositories))
			}
			for _, id := range []string{"repo-a", "repo-b", "repo-c"} {
				if _, _, _, found := p.c.RepoStateForTest(id); found {
					t.Errorf("%s retained from a rejected config", id)
				}
			}
			p.c.HandleMessage(context.Background(), p.s, "git_sync_now", rawJSON(`{"type":"git_sync_now"}`))
			p.settle(t)
			if n := p.g.total(); n != 0 {
				t.Errorf("git ran %d times after a rejected config", n)
			}
		})
	}
}

// The pin is checked for every repository, independent of whether its origin host-matches a local
// provider or a local store exists at all.
func TestMismatchRejectedForUnmatchedOriginRepo(t *testing.T) {
	p := newPinTest(t, gitsync.WithReplicaHost(pinnedHost))
	st := p.send(t, string(configJSONRaw("1",
		goodRepo("repo-a"),
		repoJSON("repo-b", "https://other.example.com/b.git", "", "https://evil.example/r/b.git", testPushToken))))
	wantConfigError(t, st, "1", proto.GitSyncConfigErrReplicaHostMismatch)
	p.settle(t)
	if p.g.total() != 0 {
		t.Error("git ran")
	}
}

func TestMismatchRejectedWithoutLocalStore(t *testing.T) {
	g := &scriptedGit{}
	c := newBareClient(t, false, gitsync.WithReplicaHost(pinnedHost), gitsync.WithGitRunner(g.run))
	s := &stubSender{}
	c.HandleMessage(context.Background(), s, "git_sync_config", configJSONRaw("1",
		repoJSON("repo-a", goodOrigin, "", "https://evil.example/r/a.git", testPushToken)))
	wantConfigError(t, lastStatus(t, s.messages()), "1", proto.GitSyncConfigErrReplicaHostMismatch)
	settleClient(t, c)
	if g.total() != 0 {
		t.Error("git ran")
	}
}

// A mismatch outranks the other structural rejections: a hostile URL must not be hidden behind them.
func TestMismatchTakesPriorityOverLaterChecks(t *testing.T) {
	p := newPinTest(t, gitsync.WithReplicaHost(pinnedHost))
	st := p.send(t, `{"type":"git_sync_config","config_version":{"a":1},"repositories":[`+
		repoJSON("bad/id", goodOrigin, "", "https://evil.example/r", testPushToken)+`]}`)
	wantConfigError(t, st, "null", proto.GitSyncConfigErrReplicaHostMismatch)
}

// --- Unusable replica on the pinned host stays a per-repository failure ---

func TestUnusableReplicaOnPinnedHostFailsOnlyThatRepo(t *testing.T) {
	p := newPinTest(t, gitsync.WithReplicaHost(pinnedHost))
	st := p.send(t, string(configJSONRaw("1",
		goodRepo("good"),
		repoJSON("missing", goodOrigin, "", "", testPushToken),
		repoJSON("userinfo", goodOrigin, "", "https://user:pw@replica.example.com/r", testPushToken),
		repoJSON("query", goodOrigin, "", "https://replica.example.com/r?x=1", testPushToken),
		repoJSON("no-token", goodOrigin, "", goodReplica, ""))))
	if st.ConfigError != nil {
		t.Fatalf("config rejected as a whole: %+v", st.ConfigError)
	}
	rs := repoStates(st)
	for _, id := range []string{"missing", "userinfo", "query", "no-token"} {
		if r := rs[id]; r.State != gitsync.StateError || r.ErrorDetail != "invalid replica configuration" {
			t.Errorf("%s = %+v, want the per-repo invalid replica error", id, r)
		}
		if replica, _, _ := p.c.ReplicaMaterialForTest(id); replica != "" {
			t.Errorf("%s retained %q", id, replica)
		}
	}
	waitFor(t, func() bool { return repoStatusNow(t, p.s, "good").State == gitsync.StateOK })
}

// --- config_error lifecycle ---

func TestConfigErrorAppearsAndClearsOnNextGoodConfig(t *testing.T) {
	p := newPinTest(t, gitsync.WithReplicaHost(pinnedHost))
	st := p.send(t, string(configJSONRaw("3", repoJSON("repo-a", goodOrigin, "", "https://evil.example/r", testPushToken))))
	wantConfigError(t, st, "3", proto.GitSyncConfigErrReplicaHostMismatch)

	// It persists across heartbeats while the latest config is the rejected one.
	if err := p.c.OnHeartbeat(context.Background(), p.s); err != nil {
		t.Fatalf("OnHeartbeat: %v", err)
	}
	wantConfigError(t, p.status(t), "3", proto.GitSyncConfigErrReplicaHostMismatch)

	st = p.send(t, string(configJSONRaw("4", goodRepo("repo-a"))))
	if st.ConfigError != nil {
		t.Errorf("config_error not cleared by an accepted config: %+v", st.ConfigError)
	}
	if string(st.ConfigVersion) != "4" {
		t.Errorf("config_version = %s, want 4", st.ConfigVersion)
	}
}

func TestPreviousConfigKeepsSyncingAfterRejection(t *testing.T) {
	p := newPinTest(t, gitsync.WithReplicaHost(pinnedHost))
	p.send(t, string(configJSONRaw("1", goodRepo("repo-a"))))
	waitFor(t, func() bool { return repoStatusNow(t, p.s, "repo-a").State == gitsync.StateOK })
	pushes := p.g.count("push")

	st := p.send(t, string(configJSONRaw("2", goodRepo("repo-b"), repoJSON("repo-c", goodOrigin, "", "https://evil.example/r", testPushToken))))
	wantConfigError(t, st, "2", proto.GitSyncConfigErrReplicaHostMismatch)
	if string(st.ConfigVersion) != "1" {
		t.Errorf("a rejected config advanced the version to %s", st.ConfigVersion)
	}
	rs := repoStates(st)
	if _, ok := rs["repo-a"]; !ok || len(rs) != 1 {
		t.Errorf("repositories = %v, want only the previous repo-a", rs)
	}

	p.c.HandleMessage(context.Background(), p.s, "git_sync_now", rawJSON(`{"type":"git_sync_now"}`))
	waitFor(t, func() bool { return p.g.count("push") == pushes+1 })
	if p.g.anyCallMentions("evil.example") {
		t.Error("the rejected config's host reached git")
	}
}

func TestEveryRejectionPathReportsItsReason(t *testing.T) {
	many := make([]string, gitsync.MaxConfigReposForTest+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"repo_id":"r%d"}`, i)
	}
	cases := []struct {
		name, raw, version, reason string
	}{
		{"too many", `{"config_version":5,"repositories":[` + strings.Join(many, ",") + `]}`, "5", proto.GitSyncConfigErrTooManyRepositories},
		{"invalid repo id", string(configJSONRaw("6", repoJSON("a/b", goodOrigin, "", goodReplica, testPushToken))), "6", proto.GitSyncConfigErrInvalidRepoID},
		{"dot-dot repo id", string(configJSONRaw("6", repoJSON("..", goodOrigin, "", goodReplica, testPushToken))), "6", proto.GitSyncConfigErrInvalidRepoID},
		{"duplicate", string(configJSONRaw(`"v7"`, goodRepo("x"), goodRepo("x"))), `"v7"`, proto.GitSyncConfigErrDuplicateRepoID},
		{"object version", `{"config_version":{"a":1},"repositories":[]}`, "null", proto.GitSyncConfigErrInvalidConfigVersion},
		{"array version", `{"config_version":[1],"repositories":[]}`, "null", proto.GitSyncConfigErrInvalidConfigVersion},
		{"oversized version", `{"config_version":"` + strings.Repeat("v", 300) + `","repositories":[]}`, "null", proto.GitSyncConfigErrInvalidConfigVersion},
		{"undecodable", `{"config_version":8,"repositories":`, "null", proto.GitSyncConfigErrMalformedMessage},
		{"not an object", `[1,2]`, "null", proto.GitSyncConfigErrMalformedMessage},
		{"repositories wrong type", `{"config_version":9,"repositories":"x"}`, "9", proto.GitSyncConfigErrMalformedMessage},
		{"missing repositories", `{"config_version":10}`, "10", proto.GitSyncConfigErrMalformedMessage},
		{"null repositories", `{"config_version":11,"repositories":null}`, "11", proto.GitSyncConfigErrMalformedMessage},
		{"literal null version", `{"config_version":null,"repositories":"x"}`, "null", proto.GitSyncConfigErrMalformedMessage},
		{"empty repo id", string(configJSONRaw("12", repoJSON("", goodOrigin, "", goodReplica, testPushToken))), "12", proto.GitSyncConfigErrInvalidRepoID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPinTest(t, gitsync.WithReplicaHost(pinnedHost))
			st := p.send(t, tc.raw)
			wantConfigError(t, st, tc.version, tc.reason)
			if st.ConfigVersion != nil {
				t.Errorf("a rejected config set config_version=%s", st.ConfigVersion)
			}
			p.settle(t)
			if p.g.total() != 0 {
				t.Error("git ran")
			}
		})
	}
}

// TestRejectionSendsAStatusAndDoesNotLogHostileValues: the platform must see a rejection promptly,
// and nothing taken from the rejected message may reach the logs.
func TestRejectionSendsAStatusAndDoesNotLogHostileValues(t *testing.T) {
	logs := captureLogs(t)
	p := newPinTest(t, gitsync.WithReplicaHost(pinnedHost))
	before := len(p.s.messages())
	p.send(t, string(configJSONRaw(`"leak-version"`,
		repoJSON("leak-repo", "https://leak-origin.example/x", "", "https://leak-host.example/leak-path", "leak-token-abcdefghij"))))
	if len(p.s.messages()) <= before {
		t.Error("no status was sent for a rejected config")
	}
	out := logs.String()
	if !strings.Contains(out, proto.GitSyncConfigErrReplicaHostMismatch) {
		t.Errorf("rejection not logged with its reason: %q", out)
	}
	if strings.Count(out, "rejected") != 1 {
		t.Errorf("want exactly one rejection log line, got: %q", out)
	}
	for _, leak := range []string{"leak-version", "leak-repo", "leak-origin", "leak-host", "leak-path", "leak-token"} {
		if strings.Contains(out, leak) {
			t.Errorf("logs echoed %q: %q", leak, out)
		}
	}
}

func TestConfigErrorWireShape(t *testing.T) {
	p := newPinTest(t, gitsync.WithReplicaHost(pinnedHost))
	st := p.send(t, `{"config_version":{"a":1},"repositories":[]}`)
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"config_error":{"config_version":null,"reason":"invalid_config_version"}`; !strings.Contains(string(b), want) {
		t.Errorf("status JSON lacks %s: %s", want, b)
	}
	// And is omitted entirely when no config was rejected.
	p2 := newPinTest(t, gitsync.WithReplicaHost(pinnedHost))
	p2.send(t, string(configJSONRaw("1", goodRepo("repo-a"))))
	b, _ = json.Marshal(p2.status(t))
	if strings.Contains(string(b), "config_error") {
		t.Errorf("config_error present after an accepted config: %s", b)
	}
}

// --- Defense in depth at sync time ---

func TestSyncRefusesAReplicaURLOutsideThePin(t *testing.T) {
	p := newPinTest(t, gitsync.WithReplicaHost(pinnedHost))
	p.send(t, string(configJSONRaw("1", goodRepo("repo-a"))))
	waitFor(t, func() bool { return repoStatusNow(t, p.s, "repo-a").State == gitsync.StateOK })
	pushes := p.g.count("push")

	// Simulate any other path storing an unpinned URL: the sync-time guard must still refuse it.
	p.c.ForceReplicaURLForTest("repo-a", "https://evil.example:443/r/a.git")
	p.c.HandleMessage(context.Background(), p.s, "git_sync_now", rawJSON(`{"type":"git_sync_now"}`))
	p.settle(t)
	if p.g.count("push") != pushes || p.g.anyCallMentions("evil.example") {
		t.Error("a sync ran against a replica URL outside the pin")
	}
}

func TestRejectedConfigDoesNotDisturbInFlightSyncs(t *testing.T) {
	g := &scriptedGit{gate: make(chan struct{})}
	var release sync.Once
	releaseGate := func() { release.Do(func() { close(g.gate) }) }
	p := newPinTest(t, gitsync.WithReplicaHost(pinnedHost), gitsync.WithGitRunner(g.run))
	// Registered after newPinTest's own cleanup so it runs first: a failed wait below must not leave
	// the gated fetch blocking that cleanup's Wait.
	t.Cleanup(releaseGate)
	p.send(t, string(configJSONRaw("1", goodRepo("repo-a"))))
	waitFor(t, func() bool { return g.count("fetch") == 1 })

	p.send(t, string(configJSONRaw("2", repoJSON("repo-a", goodOrigin, "", "https://evil.example/r", testPushToken))))
	if got := repoStatusNow(t, p.s, "repo-a").State; got != gitsync.StateSyncing {
		t.Errorf("in-flight repo-a = %q after a rejected config, want syncing", got)
	}
	releaseGate()
	waitFor(t, func() bool { return repoStatusNow(t, p.s, "repo-a").State == gitsync.StateOK })
	if g.anyCallMentions("evil.example") {
		t.Error("the rejected host reached git")
	}
}
