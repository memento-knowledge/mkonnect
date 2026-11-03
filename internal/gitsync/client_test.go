package gitsync_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/gateway"
	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// stubSender records the messages a handler sends, so a test can call the handler's callbacks
// directly without a live connection. Safe for concurrent use (the -race test exercises it).
type stubSender struct {
	mu   sync.Mutex
	sent []any
}

func (s *stubSender) Send(_ context.Context, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, v)
	return nil
}

func (s *stubSender) messages() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]any(nil), s.sent...)
}

// fakeProber returns a result (or error) for every provider. A non-nil block channel makes Probe
// wait for ctx cancellation (or the channel) so a test can exercise a cancelled probe. The result
// is mutex-guarded so a test can change it while a probe may be running.
type fakeProber struct {
	mu     sync.Mutex
	result gitsync.OriginResult
	err    error
	calls  atomic.Int32
	block  chan struct{}
}

func (f *fakeProber) Probe(ctx context.Context, _ gitsync.LocalEntry) (gitsync.OriginResult, error) {
	f.calls.Add(1)
	if f.block != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-f.block:
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.result, f.err
}

func (f *fakeProber) setResult(r gitsync.OriginResult) {
	f.mu.Lock()
	f.result = r
	f.mu.Unlock()
}

// testClock is an advanceable clock for exercising the probe TTL/refresh deterministically.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func fixedNow() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

func newTestClient(t *testing.T, localJSON string) *gitsync.Client {
	return newTestClientP(t, localJSON, &fakeProber{result: gitsync.OriginReachable})
}

func newTestClientP(t *testing.T, localJSON string, p gitsync.OriginProber) *gitsync.Client {
	return newTestClientClock(t, localJSON, p, fixedNow)
}

func newTestClientClock(t *testing.T, localJSON string, p gitsync.OriginProber, now func() time.Time) *gitsync.Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git-sync.json")
	if localJSON != "" {
		if err := os.WriteFile(path, []byte(localJSON), 0o600); err != nil {
			t.Fatalf("write local config: %v", err)
		}
	}
	local, err := gitsync.New(path)
	if err != nil {
		t.Fatalf("load local config: %v", err)
	}
	cfg := &config.Config{GatewayURL: "wss://gw.example.com", ConnectorID: "gitsync-test", ProtocolVersion: "v1"}
	return gitsync.NewClient(cfg, auth.NewKeyStore(filepath.Join(t.TempDir(), "key")), local, p, now, "boot-test")
}

// latestOriginCheck returns the origin_check from the most recent status that carried one.
func latestOriginCheck(msgs []any) *proto.OriginCheck {
	for i := len(msgs) - 1; i >= 0; i-- {
		if st, ok := msgs[i].(proto.GitSyncStatusMsg); ok && st.OriginCheck != nil {
			return st.OriginCheck
		}
	}
	return nil
}

func lastStatus(t *testing.T, msgs []any) proto.GitSyncStatusMsg {
	t.Helper()
	for i := len(msgs) - 1; i >= 0; i-- {
		if st, ok := msgs[i].(proto.GitSyncStatusMsg); ok {
			return st
		}
	}
	t.Fatalf("no git_sync_status among %d sent messages", len(msgs))
	return proto.GitSyncStatusMsg{}
}

func repoStates(st proto.GitSyncStatusMsg) map[string]proto.GitSyncRepoStatus {
	m := make(map[string]proto.GitSyncRepoStatus, len(st.Repositories))
	for _, r := range st.Repositories {
		m[r.RepoID] = r
	}
	return m
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

const oneProvider = `{"origin":{"base_url":"https://git.example.com/"}}`

// configJSON builds a git_sync_config frame. version is a JSON literal ("7", "null", "" to omit).
// Each repo is {repo_id, origin_url, push_token_expires_at}; an empty expiry is omitted.
func configJSON(version string, repos ...[3]string) json.RawMessage {
	var sb strings.Builder
	sb.WriteString(`{"type":"git_sync_config"`)
	if version != "" {
		sb.WriteString(`,"config_version":` + version)
	}
	sb.WriteString(`,"repositories":[`)
	for i, r := range repos {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"repo_id":%q,"origin_url":%q`, r[0], r[1])
		if r[2] != "" {
			fmt.Fprintf(&sb, `,"push_token_expires_at":%q`, r[2])
		}
		sb.WriteString("}")
	}
	sb.WriteString("]}")
	return json.RawMessage(sb.String())
}

func TestConfigAcceptedStatusEnvelope(t *testing.T) {
	c := newTestClient(t, oneProvider)
	s := &stubSender{}
	c.HandleMessage(context.Background(), s, "git_sync_config",
		configJSON("7", [3]string{"repo-a", "https://git.example.com/team/a.git", "2026-01-01T00:00:00Z"}))

	st := lastStatus(t, s.messages())
	if st.Type != "git_sync_status" {
		t.Errorf("type = %q", st.Type)
	}
	if string(st.ConfigVersion) != "7" {
		t.Errorf("config_version = %s, want 7", st.ConfigVersion)
	}
	if st.SentAt != "2026-01-02T03:04:05.000000000Z" {
		t.Errorf("sent_at = %q, want fixed UTC with fraction", st.SentAt)
	}
	if st.BootID != "boot-test" {
		t.Errorf("boot_id = %q", st.BootID)
	}
	if st.Sequence != 1 {
		t.Errorf("sequence = %d, want 1 for the first status", st.Sequence)
	}
	if st.EarliestPushTokenExpiresAt == nil || *st.EarliestPushTokenExpiresAt != "2026-01-01T00:00:00Z" {
		t.Errorf("earliest_push_token_expires_at = %v", st.EarliestPushTokenExpiresAt)
	}
	if got := repoStates(st)["repo-a"].State; got != gitsync.StatePending {
		t.Errorf("repo-a state = %q, want pending", got)
	}
}

func TestHostMatchStates(t *testing.T) {
	c := newTestClient(t, oneProvider)
	s := &stubSender{}
	c.HandleMessage(context.Background(), s, "git_sync_config", configJSON("1",
		[3]string{"repo-a", "https://git.example.com/a.git", ""},
		[3]string{"repo-b", "https://other.example.com/b.git", ""}))

	rs := repoStates(lastStatus(t, s.messages()))
	if rs["repo-a"].State != gitsync.StatePending {
		t.Errorf("matched repo-a state = %q, want pending", rs["repo-a"].State)
	}
	b := rs["repo-b"]
	if b.State != gitsync.StateError || b.ErrorType != gitsync.ErrOriginAccessDenied || b.ErrorDetail != "host not configured locally" {
		t.Errorf("unmatched repo-b = %+v, want error/origin_access_denied/host not configured locally", b)
	}
	// The error detail must never carry the actual host.
	if strings.Contains(b.ErrorDetail, "other.example.com") {
		t.Error("error_detail leaked the origin host")
	}
}

// TestRepoStateStoresCanonicalURL asserts Fix #2: a host-matched repository retains the matched
// provider name and the validated, re-serialized canonical origin URL (not the raw origin string),
// so Phase 3 dials exactly the authorized host. An unmatched repository retains neither.
func TestRepoStateStoresCanonicalURL(t *testing.T) {
	c := newTestClient(t, oneProvider) // provider "origin" at https://git.example.com/
	c.HandleMessage(context.Background(), &stubSender{}, "git_sync_config", configJSON("1",
		[3]string{"repo-a", "https://GIT.example.com/team/a.git", ""},
		[3]string{"repo-b", "https://other.example.com/b.git", ""}))

	provider, canonURL, matched, found := c.RepoStateForTest("repo-a")
	if !found || !matched {
		t.Fatalf("repo-a: found=%v matched=%v, want both true", found, matched)
	}
	if provider != "origin" {
		t.Errorf("repo-a provider = %q, want %q", provider, "origin")
	}
	if want := "https://git.example.com:443/team/a.git"; canonURL != want {
		t.Errorf("repo-a canonicalURL = %q, want %q (ASCII-lowercased host, explicit port, path kept)", canonURL, want)
	}

	provider, canonURL, matched, found = c.RepoStateForTest("repo-b")
	if !found {
		t.Fatal("repo-b not retained")
	}
	if matched || provider != "" || canonURL != "" {
		t.Errorf("unmatched repo-b: matched=%v provider=%q canonicalURL=%q, want false/\"\"/\"\"", matched, provider, canonURL)
	}
}

func TestStatusNullBeforeConfig(t *testing.T) {
	c := newTestClient(t, "") // empty local config: no probe fires
	s := &stubSender{}
	if err := c.OnHeartbeat(context.Background(), s); err != nil {
		t.Fatalf("OnHeartbeat: %v", err)
	}
	st := lastStatus(t, s.messages())
	if st.ConfigVersion != nil || st.EarliestPushTokenExpiresAt != nil {
		t.Errorf("config_version/earliest should be nil before any config, got %s / %v", st.ConfigVersion, st.EarliestPushTokenExpiresAt)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"config_version":null`, `"earliest_push_token_expires_at":null`, `"repositories":[]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("status missing %s: %s", want, b)
		}
	}
}

func TestSequenceIncrementsBootStable(t *testing.T) {
	c := newTestClient(t, "")
	s := &stubSender{}
	_ = c.OnHeartbeat(context.Background(), s)
	_ = c.OnHeartbeat(context.Background(), s)
	msgs := s.messages()
	st1, st2 := msgs[0].(proto.GitSyncStatusMsg), msgs[1].(proto.GitSyncStatusMsg)
	if st1.Sequence != 1 || st2.Sequence != 2 {
		t.Errorf("sequences = %d,%d, want 1,2", st1.Sequence, st2.Sequence)
	}
	if st1.BootID != "boot-test" || st2.BootID != "boot-test" {
		t.Errorf("boot_id not stable: %q %q", st1.BootID, st2.BootID)
	}
}

func TestApplyConfigValidationRejected(t *testing.T) {
	cases := map[string]string{
		"absent repositories": `{"type":"git_sync_config","config_version":1}`,
		"duplicate repo_id":   `{"type":"git_sync_config","config_version":1,"repositories":[{"repo_id":"x","origin_url":"https://git.example.com/a"},{"repo_id":"x","origin_url":"https://git.example.com/b"}]}`,
		"empty repo_id":       `{"type":"git_sync_config","config_version":1,"repositories":[{"repo_id":"","origin_url":"https://git.example.com/a"}]}`,
		"non-scalar version":  `{"type":"git_sync_config","config_version":{"n":1},"repositories":[{"repo_id":"x","origin_url":"https://git.example.com/a"}]}`,
		// repo_id charset: traversal, control, and bare-dot ids are rejected as a whole message, since
		// Phase 3 uses repo_id in filesystem paths and replica URLs.
		"traversal repo_id":  `{"type":"git_sync_config","config_version":1,"repositories":[{"repo_id":"../../x","origin_url":"https://git.example.com/a"}]}`,
		"dotdot repo_id":     `{"type":"git_sync_config","config_version":1,"repositories":[{"repo_id":"..","origin_url":"https://git.example.com/a"}]}`,
		"dot repo_id":        `{"type":"git_sync_config","config_version":1,"repositories":[{"repo_id":".","origin_url":"https://git.example.com/a"}]}`,
		"nul repo_id":        `{"type":"git_sync_config","config_version":1,"repositories":[{"repo_id":"a\u0000b","origin_url":"https://git.example.com/a"}]}`,
		"control repo_id":    `{"type":"git_sync_config","config_version":1,"repositories":[{"repo_id":"a\u0001b","origin_url":"https://git.example.com/a"}]}`,
		"whitespace repo_id": `{"type":"git_sync_config","config_version":1,"repositories":[{"repo_id":"a b","origin_url":"https://git.example.com/a"}]}`,
	}
	oversized := `{"type":"git_sync_config","config_version":1,"repositories":[{"repo_id":"` + strings.Repeat("a", 257) + `","origin_url":"https://git.example.com/a"}]}`
	cases["oversized repo_id"] = oversized

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, "")
			s := &stubSender{}
			c.HandleMessage(context.Background(), s, "git_sync_config", json.RawMessage(raw))
			// A rejected config emits no status and leaves the connector unconfigured.
			if n := len(s.messages()); n != 0 {
				t.Errorf("rejected config emitted %d status messages, want 0", n)
			}
			if err := c.OnHeartbeat(context.Background(), s); err != nil {
				t.Fatalf("OnHeartbeat: %v", err)
			}
			st := lastStatus(t, s.messages())
			if len(st.Repositories) != 0 || st.ConfigVersion != nil {
				t.Errorf("configuration should be unchanged after rejection: repos=%v version=%s", st.Repositories, st.ConfigVersion)
			}
		})
	}
}

func TestSyncNowMatchableOnly(t *testing.T) {
	c := newTestClient(t, oneProvider)
	c.HandleMessage(context.Background(), &stubSender{}, "git_sync_config", configJSON("1",
		[3]string{"repo-a", "https://git.example.com/a.git", ""},
		[3]string{"repo-b", "https://other.example.com/b.git", ""}))

	if got := c.HandleSyncNowForTest(proto.GitSyncNowMsg{Type: "git_sync_now"}); !reflect.DeepEqual(got, []string{"repo-a"}) {
		t.Errorf("empty repo_id targets = %v, want [repo-a] (host-matched only)", got)
	}
	for _, id := range []string{"repo-b", "repo-unknown"} {
		if got := c.HandleSyncNowForTest(proto.GitSyncNowMsg{Type: "git_sync_now", RepoID: id}); got != nil {
			t.Errorf("repo %q should not be a target, got %v", id, got)
		}
	}
}

func TestSyncNowSuppressedByCriticalPatch(t *testing.T) {
	c := newTestClient(t, oneProvider)
	c.HandleMessage(context.Background(), &stubSender{}, "git_sync_config", configJSON("1", [3]string{"repo-a", "https://git.example.com/a.git", ""}))
	c.SetCriticalPatchForTest(true)
	if got := c.HandleSyncNowForTest(proto.GitSyncNowMsg{Type: "git_sync_now"}); got != nil {
		t.Errorf("critical patch must suppress sync, got %v", got)
	}
}

func TestCriticalPatchGateViaHandleMessage(t *testing.T) {
	c := newTestClient(t, oneProvider)
	c.HandleMessage(context.Background(), &stubSender{}, "git_sync_config", configJSON("1", [3]string{"repo-a", "https://git.example.com/a.git", ""}))
	c.SetCriticalPatchForTest(true)

	s := &stubSender{}
	c.HandleMessage(context.Background(), s, "git_sync_now", json.RawMessage(`{"type":"git_sync_now"}`))
	if n := len(s.messages()); n != 1 {
		t.Errorf("git_sync_now under critical patch sent %d messages, want 1 status", n)
	}
	if got := c.HandleSyncNowForTest(proto.GitSyncNowMsg{Type: "git_sync_now"}); got != nil {
		t.Errorf("critical patch must suppress targets, got %v", got)
	}
}

func TestMalformedMessagesAreIgnored(t *testing.T) {
	c := newTestClient(t, oneProvider)
	s := &stubSender{}
	c.HandleMessage(context.Background(), s, "git_sync_config", json.RawMessage(`{"repositories":"nope"}`))
	c.HandleMessage(context.Background(), s, "git_sync_now", json.RawMessage(`{"repo_id":123}`))
	c.HandleMessage(context.Background(), s, "something_else", json.RawMessage(`{}`))
	if n := len(s.messages()); n != 0 {
		t.Errorf("malformed/unknown messages sent %d replies, want 0", n)
	}
	// A valid config still applies afterwards.
	c.HandleMessage(context.Background(), s, "git_sync_config", configJSON("1", [3]string{"repo-a", "https://git.example.com/a.git", ""}))
	if got := c.HandleSyncNowForTest(proto.GitSyncNowMsg{Type: "git_sync_now"}); !reflect.DeepEqual(got, []string{"repo-a"}) {
		t.Errorf("after a malformed config, a valid one still applies: targets = %v", got)
	}
}

func TestPushTokenNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	c := newTestClient(t, oneProvider)
	token := "secret-push-token-zzzz9999"
	accepted := fmt.Sprintf(`{"type":"git_sync_config","config_version":1,"repositories":[{"repo_id":"repo-a","origin_url":"https://git.example.com/a.git","push_token":%q}]}`, token)
	// A duplicate repo_id is rejected; the warning must not echo the token either.
	rejected := fmt.Sprintf(`{"type":"git_sync_config","config_version":1,"repositories":[{"repo_id":"x","origin_url":"https://git.example.com/a","push_token":%q},{"repo_id":"x","origin_url":"https://git.example.com/b","push_token":%q}]}`, token, token)
	// A malformed frame carrying the token must not leak it through the decode error.
	malformed := fmt.Sprintf(`{"type":"git_sync_config","repositories":[{"repo_id":123,"push_token":%q}]}`, token)
	for _, raw := range []string{accepted, rejected, malformed} {
		c.HandleMessage(context.Background(), &stubSender{}, "git_sync_config", json.RawMessage(raw))
	}
	if strings.Contains(buf.String(), token) {
		t.Errorf("push token leaked into logs: %s", buf.String())
	}
}

func TestWireStringsPinned(t *testing.T) {
	if gitsync.StatePending != "pending" || gitsync.StateSyncing != "syncing" || gitsync.StateOK != "ok" || gitsync.StateError != "error" {
		t.Error("repository state wire strings drifted")
	}
	gotErrs := []string{
		gitsync.ErrOriginUnreachable, gitsync.ErrOriginAccessDenied, gitsync.ErrReplicaUnreachable,
		gitsync.ErrCredentialRejected, gitsync.ErrReplicaRejected, gitsync.ErrPushTimeout,
		gitsync.ErrLocalDiskExhausted, gitsync.ErrJobOutOfMemory, gitsync.ErrRepositoryOverLimit,
		gitsync.ErrPushTokenExpired, gitsync.ErrUnclassified,
	}
	wantErrs := []string{
		"origin_unreachable", "origin_access_denied", "replica_unreachable", "credential_rejected",
		"replica_rejected", "push_timeout", "local_disk_exhausted", "job_out_of_memory",
		"repository_over_limit", "push_token_expired", "unclassified",
	}
	if !reflect.DeepEqual(gotErrs, wantErrs) {
		t.Errorf("error_type wire strings drifted:\n got %v\nwant %v", gotErrs, wantErrs)
	}
	if gitsync.WarnLFSNotReplicated != "lfs_not_replicated" {
		t.Errorf("warning string drifted: %q", gitsync.WarnLFSNotReplicated)
	}
}

func TestOriginCheckPreConfigThenAbsent(t *testing.T) {
	prober := &fakeProber{result: gitsync.OriginAccessDenied}
	c := newTestClientP(t, oneProvider, prober)
	s := &stubSender{}
	ctx := context.Background()
	c.OnConnected(ctx, s)

	waitFor(t, func() bool {
		for _, m := range s.messages() {
			if st, ok := m.(proto.GitSyncStatusMsg); ok && st.OriginCheck != nil {
				return true
			}
		}
		return false
	})
	st := lastStatus(t, s.messages())
	if st.OriginCheck == nil || st.OriginCheck.Result != "access_denied" || st.OriginCheck.CheckedAt == "" {
		t.Errorf("origin_check = %+v, want access_denied with a checked_at", st.OriginCheck)
	}

	// Once configuration is accepted, origin_check is no longer reported.
	c.HandleMessage(ctx, s, "git_sync_config", configJSON("1", [3]string{"repo-a", "https://git.example.com/a.git", ""}))
	if lastStatus(t, s.messages()).OriginCheck != nil {
		t.Error("origin_check should be absent once config is accepted")
	}
}

func TestOriginCheckAbsentWithoutLocalEntries(t *testing.T) {
	prober := &fakeProber{result: gitsync.OriginReachable}
	c := newTestClientP(t, "", prober)
	s := &stubSender{}
	c.OnConnected(context.Background(), s)
	time.Sleep(50 * time.Millisecond)
	if prober.calls.Load() != 0 {
		t.Errorf("prober ran %d times with no local providers, want 0", prober.calls.Load())
	}
	for _, m := range s.messages() {
		if st, ok := m.(proto.GitSyncStatusMsg); ok && st.OriginCheck != nil {
			t.Error("origin_check should be absent without local providers")
		}
	}
}

func TestOriginCheckNotCachedOnCancel(t *testing.T) {
	prober := &fakeProber{result: gitsync.OriginReachable, block: make(chan struct{})}
	c := newTestClientP(t, oneProvider, prober)
	s := &stubSender{}
	ctx, cancel := context.WithCancel(context.Background())
	c.OnConnected(ctx, s)
	waitFor(t, func() bool { return prober.calls.Load() >= 1 })
	cancel()
	time.Sleep(50 * time.Millisecond)
	for _, m := range s.messages() {
		if st, ok := m.(proto.GitSyncStatusMsg); ok && st.OriginCheck != nil {
			t.Error("a cancelled probe must not be cached as an origin_check")
		}
	}
}

// TestHeartbeatProbeUsesConnectionContext is the regression for the bug where a heartbeat-triggered
// probe inherited the per-tick context the gateway cancels on return, so the origin_check could
// never refresh after connect. The probe must run under the connection context captured in
// OnConnected and refresh even when the heartbeat's own context is cancelled immediately.
func TestHeartbeatProbeUsesConnectionContext(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	prober := &fakeProber{result: gitsync.OriginUnreachable}
	c := newTestClientClock(t, oneProvider, prober, clk.now)
	s := &stubSender{}
	connCtx, connCancel := context.WithCancel(context.Background())
	defer connCancel()

	c.OnConnected(connCtx, s)
	waitFor(t, func() bool {
		oc := latestOriginCheck(s.messages())
		return oc != nil && oc.Result == "unreachable"
	})

	// The origin recovers; advance past the TTL and deliver a heartbeat whose own context is
	// cancelled the instant the call returns (as the gateway does).
	prober.setResult(gitsync.OriginReachable)
	clk.advance(gateway.DefaultHeartbeatInterval + time.Second)
	tickCtx, tickCancel := context.WithCancel(context.Background())
	if err := c.OnHeartbeat(tickCtx, s); err != nil {
		t.Fatalf("OnHeartbeat: %v", err)
	}
	tickCancel()

	waitFor(t, func() bool {
		oc := latestOriginCheck(s.messages())
		return oc != nil && oc.Result == "reachable"
	})
}

// TestConcurrentConfigAndHeartbeat exercises the documented overlap of OnHeartbeat (its own
// goroutine) and HandleMessage (the read loop). Empty local config keeps the probe out of the way.
func TestConcurrentConfigAndHeartbeat(t *testing.T) {
	c := newTestClient(t, "")
	s := &stubSender{}
	cfgRaw := configJSON("1", [3]string{"repo-b", "https://b.example.com/b", ""}, [3]string{"repo-c", "https://c.example.com/c", ""})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			c.HandleMessage(context.Background(), s, "git_sync_config", cfgRaw)
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			if err := c.OnHeartbeat(context.Background(), s); err != nil {
				t.Errorf("OnHeartbeat: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	// Because build-and-send is serialised under statusMu, sent order must equal sequence order.
	var prev uint64
	for _, m := range s.messages() {
		st, ok := m.(proto.GitSyncStatusMsg)
		if !ok {
			continue
		}
		if st.Sequence <= prev {
			t.Fatalf("sequence not strictly increasing in send order: %d after %d", st.Sequence, prev)
		}
		prev = st.Sequence
	}
}

// --- end-to-end enrollment through the gateway ---

type gatewayHelloMsg struct {
	Type              string `json:"type"`
	ConnectorID       string `json:"connector_id"`
	ProtocolVersion   string `json:"protocol_version"`
	ConnectorVersion  string `json:"connector_version"`
	RegistrationToken string `json:"registration_token,omitempty"`
	Signature         string `json:"signature,omitempty"`
}

type gatewayHandshakeOkMsg struct {
	Type       string `json:"type"`
	PrivateKey string `json:"private_key,omitempty"`
}

// TestEnrollsAndDispatchesStatus runs the real gateway client against a test server: the connector
// registers, emits an initial git_sync_status, and after the server pushes a git_sync_config emits a
// status reporting the configured repository as pending.
func TestEnrollsAndDispatchesStatus(t *testing.T) {
	_, serverKey, err := mldsa65.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyBytes := make([]byte, mldsa65.PrivateKeySize)
	serverKey.Pack((*[mldsa65.PrivateKeySize]byte)(keyBytes))

	statuses := make(chan proto.GitSyncStatusMsg, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck

		var nonce [32]byte
		if err := wsjson.Write(r.Context(), conn, map[string]string{
			"type": "challenge", "challenge": base64.StdEncoding.EncodeToString(nonce[:]),
		}); err != nil {
			return
		}
		var hello gatewayHelloMsg
		if err := wsjson.Read(r.Context(), conn, &hello); err != nil {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gatewayHandshakeOkMsg{
			Type: "handshake_ok", PrivateKey: base64.StdEncoding.EncodeToString(keyBytes),
		}); err != nil {
			return
		}
		// Push a configuration naming one repository whose origin matches the local provider.
		if err := wsjson.Write(r.Context(), conn, map[string]any{
			"type":           "git_sync_config",
			"config_version": 1,
			"repositories":   []map[string]any{{"repo_id": "repo-a", "origin_url": "https://git.example.com/a.git"}},
		}); err != nil {
			return
		}
		for {
			var raw json.RawMessage
			if err := wsjson.Read(r.Context(), conn, &raw); err != nil {
				return
			}
			var typed proto.TypedMessage
			if err := json.Unmarshal(raw, &typed); err != nil {
				return
			}
			if typed.Type == "git_sync_status" {
				var st proto.GitSyncStatusMsg
				if err := json.Unmarshal(raw, &st); err != nil {
					return
				}
				statuses <- st
			}
		}
	}))
	defer srv.Close()

	localPath := filepath.Join(t.TempDir(), "git-sync.json")
	if err := os.WriteFile(localPath, []byte(oneProvider), 0o600); err != nil {
		t.Fatalf("write local config: %v", err)
	}
	local, err := gitsync.New(localPath)
	if err != nil {
		t.Fatalf("local config: %v", err)
	}
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1)
	cfg := &config.Config{GatewayURL: wsURL, ConnectorID: "gitsync-test", ProtocolVersion: "v1", RegistrationToken: "tok-test"}
	c := gitsync.NewClient(cfg, auth.NewKeyStore(filepath.Join(t.TempDir(), "key")), local,
		&fakeProber{result: gitsync.OriginReachable}, fixedNow, "boot-test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connectDone := make(chan error, 1)
	go func() { connectDone <- c.Connect(ctx) }()

	// Wait for a status that reports repo-a as pending (the one emitted after the config push).
	deadline := time.After(3 * time.Second)
	for {
		select {
		case st := <-statuses:
			if st.Type != "git_sync_status" || st.BootID != "boot-test" {
				t.Errorf("unexpected status envelope: %+v", st)
			}
			if rs := repoStates(st); rs["repo-a"].State == gitsync.StatePending {
				cancel()
				select {
				case <-connectDone:
				case <-time.After(2 * time.Second):
					t.Fatal("Connect did not return after cancel")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for a git_sync_status reporting repo-a pending")
		}
	}
}
