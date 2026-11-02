package gitsync_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
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

// newTestClient builds a git-sync client with the given local config JSON (empty string = no
// config file, i.e. an empty local config).
func newTestClient(t *testing.T, localJSON string) *gitsync.Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git-sync.json")
	if localJSON != "" {
		if err := os.WriteFile(path, []byte(localJSON), 0o600); err != nil {
			t.Fatalf("write local config: %v", err)
		}
	}
	local, err := gitsync.LoadLocalConfig(path)
	if err != nil {
		t.Fatalf("load local config: %v", err)
	}
	cfg := &config.Config{GatewayURL: "wss://gw.example.com", ConnectorID: "gitsync-test", ProtocolVersion: "v1"}
	return gitsync.NewClient(cfg, auth.NewKeyStore(filepath.Join(t.TempDir(), "key")), local)
}

// lastStatus returns the most recent git_sync_status among the recorded messages.
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

const oneLocalRepo = `{"repo-a":{"origin_url":"https://git.example.com/a"}}`

func TestHandleGitSyncConfigEmitsUnionStatus(t *testing.T) {
	c := newTestClient(t, oneLocalRepo)
	s := &stubSender{}
	// repo-a is in local config; repo-b is platform-configured only.
	raw := json.RawMessage(`{"type":"git_sync_config","repositories":[{"repo_key":"repo-a"},{"repo_key":"repo-b"}]}`)
	c.HandleMessage(context.Background(), s, "git_sync_config", raw)

	st := lastStatus(t, s.messages())
	if st.Type != "git_sync_status" {
		t.Errorf("type = %q, want git_sync_status", st.Type)
	}
	want := []proto.GitSyncRepoStatus{
		{RepoKey: "repo-a", State: gitsync.StatePending},
		{RepoKey: "repo-b", State: gitsync.StateNotConfigured},
	}
	if !reflect.DeepEqual(st.Repositories, want) {
		t.Errorf("repositories = %+v, want %+v", st.Repositories, want)
	}
}

func TestOnHeartbeatEmptyStatusMarshalsEmptyArray(t *testing.T) {
	c := newTestClient(t, "")
	s := &stubSender{}
	if err := c.OnHeartbeat(context.Background(), s); err != nil {
		t.Fatalf("OnHeartbeat: %v", err)
	}
	st := lastStatus(t, s.messages())
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"repositories":[]`) {
		t.Errorf("empty status must marshal repositories as [], got %s", b)
	}
}

// configRepos is a git_sync_config raw frame naming the given repo keys.
func configRepos(keys ...string) json.RawMessage {
	msg := proto.GitSyncConfigMsg{Type: "git_sync_config"}
	for _, k := range keys {
		msg.Repositories = append(msg.Repositories, proto.GitSyncRepoConfig{RepoKey: k})
	}
	b, _ := json.Marshal(msg)
	return b
}

func TestSyncNowEmptyKeyTargetsSyncableOnly(t *testing.T) {
	// Local config has repo-a and repo-b; the platform configures repo-a and repo-c.
	// Syncable = platform-configured AND local = {repo-a}. repo-b (local only) and repo-c
	// (configured, no local creds) are NOT sync targets.
	c := newTestClient(t, `{"repo-a":{"origin_url":"https://git.example.com/a"},"repo-b":{"origin_url":"https://git.example.com/b"}}`)
	c.HandleMessage(context.Background(), &stubSender{}, "git_sync_config", configRepos("repo-a", "repo-c"))

	got := c.HandleSyncNowForTest(proto.GitSyncNowMsg{Type: "git_sync_now"})
	if !reflect.DeepEqual(got, []string{"repo-a"}) {
		t.Errorf("empty-key targets = %v, want [repo-a] (configured ∩ local)", got)
	}
}

func TestSyncNowRejectsNonSyncableRepo(t *testing.T) {
	c := newTestClient(t, oneLocalRepo) // local: repo-a, but no platform config yet
	c.HandleMessage(context.Background(), &stubSender{}, "git_sync_config", configRepos("repo-c"))
	// repo-a is local-only (not configured) -> not syncable; repo-x is unknown; repo-c has no
	// local creds. None resolve to a target.
	for _, key := range []string{"repo-a", "repo-c", "repo-x"} {
		if got := c.HandleSyncNowForTest(proto.GitSyncNowMsg{Type: "git_sync_now", RepoKey: key}); got != nil {
			t.Errorf("repo %q should not be a sync target, got %v", key, got)
		}
	}
}

func TestSyncNowSuppressedByCriticalPatch(t *testing.T) {
	c := newTestClient(t, oneLocalRepo)
	c.SetCriticalPatchForTest(true)
	if got := c.HandleSyncNowForTest(proto.GitSyncNowMsg{Type: "git_sync_now"}); got != nil {
		t.Errorf("critical patch must suppress sync, got targets %v", got)
	}
}

func TestMalformedMessagesAreIgnored(t *testing.T) {
	c := newTestClient(t, oneLocalRepo)
	s := &stubSender{}
	// A malformed config or now frame is logged and dropped before any status is emitted, and
	// an unknown type is ignored entirely — none of these send a message or panic.
	c.HandleMessage(context.Background(), s, "git_sync_config", json.RawMessage(`{"repositories":"nope"}`))
	c.HandleMessage(context.Background(), s, "git_sync_now", json.RawMessage(`{"repo_key":123}`))
	c.HandleMessage(context.Background(), s, "something_else", json.RawMessage(`{}`))
	if n := len(s.messages()); n != 0 {
		t.Errorf("malformed/unknown messages sent %d replies, want 0", n)
	}
	// The desired repo set is unchanged: a well-formed git_sync_now still targets the local repo
	// once the platform configures it.
	c.HandleMessage(context.Background(), s, "git_sync_config", configRepos("repo-a"))
	if got := c.HandleSyncNowForTest(proto.GitSyncNowMsg{Type: "git_sync_now"}); !reflect.DeepEqual(got, []string{"repo-a"}) {
		t.Errorf("after a malformed config, a valid one still applies: targets = %v, want [repo-a]", got)
	}
}

// TestCriticalPatchGateViaHandleMessage exercises the gate through the real dispatch path, not
// only the test seam: git_sync_now under a critical patch still emits a status (acknowledgement)
// but must not resolve any sync targets.
func TestCriticalPatchGateViaHandleMessage(t *testing.T) {
	c := newTestClient(t, oneLocalRepo)
	c.HandleMessage(context.Background(), &stubSender{}, "git_sync_config", configRepos("repo-a"))
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

// TestConcurrentConfigAndHeartbeat exercises the documented overlap of OnHeartbeat (its own
// goroutine) and HandleMessage (the read loop). Run under -race.
func TestConcurrentConfigAndHeartbeat(t *testing.T) {
	c := newTestClient(t, oneLocalRepo)
	s := &stubSender{}
	configRaw := json.RawMessage(`{"type":"git_sync_config","repositories":[{"repo_key":"repo-b"},{"repo_key":"repo-c"}]}`)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			c.HandleMessage(context.Background(), s, "git_sync_config", configRaw)
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
}

// --- end-to-end enrollment through the gateway ---

// gatewayHelloMsg / gatewayHandshakeOkMsg are independent copies of the Bridge Gateway's wire
// shapes (Signature/PrivateKey are base64 strings there), so a bug in mkonnect's proto encoding
// cannot be masked by sharing a type across both ends — the same discipline the gateway tests use.
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

// TestEnrollsAndDispatchesStatus runs the real gateway client against a test server: the
// connector registers, emits an initial git_sync_status on connect, and emits a second one in
// response to a git_sync_now. health frames are filtered out.
func TestEnrollsAndDispatchesStatus(t *testing.T) {
	_, serverKey, err := mldsa65.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyBytes := make([]byte, mldsa65.PrivateKeySize)
	serverKey.Pack((*[mldsa65.PrivateKeySize]byte)(keyBytes))

	statuses := make(chan proto.GitSyncStatusMsg, 4)

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
			t.Errorf("write challenge: %v", err)
			return
		}
		var hello gatewayHelloMsg
		if err := wsjson.Read(r.Context(), conn, &hello); err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, gatewayHandshakeOkMsg{
			Type: "handshake_ok", PrivateKey: base64.StdEncoding.EncodeToString(keyBytes),
		}); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}
		// Ask for an immediate sync; the connector should answer with a git_sync_status.
		if err := wsjson.Write(r.Context(), conn, map[string]string{"type": "git_sync_now"}); err != nil {
			t.Errorf("write git_sync_now: %v", err)
			return
		}
		// Read frames, forwarding git_sync_status and ignoring health, until the client is told
		// to go away (after the test has what it needs).
		for {
			var raw json.RawMessage
			if err := wsjson.Read(r.Context(), conn, &raw); err != nil {
				return
			}
			var typed proto.TypedMessage
			if err := json.Unmarshal(raw, &typed); err != nil {
				t.Errorf("unmarshal type: %v", err)
				return
			}
			if typed.Type == "git_sync_status" {
				var st proto.GitSyncStatusMsg
				if err := json.Unmarshal(raw, &st); err != nil {
					t.Errorf("unmarshal git_sync_status: %v", err)
					return
				}
				statuses <- st
			}
		}
	}))
	defer srv.Close()

	localPath := filepath.Join(t.TempDir(), "git-sync.json")
	if err := os.WriteFile(localPath, []byte(oneLocalRepo), 0o600); err != nil {
		t.Fatalf("write local config: %v", err)
	}
	local, err := gitsync.LoadLocalConfig(localPath)
	if err != nil {
		t.Fatalf("local config: %v", err)
	}
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1)
	cfg := &config.Config{GatewayURL: wsURL, ConnectorID: "gitsync-test", ProtocolVersion: "v1", RegistrationToken: "tok-test"}
	c := gitsync.NewClient(cfg, auth.NewKeyStore(filepath.Join(t.TempDir(), "key")), local)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connectDone := make(chan error, 1)
	go func() { connectDone <- c.Connect(ctx) }()

	// Expect two statuses: one from OnConnected, one answering git_sync_now.
	for i := 0; i < 2; i++ {
		select {
		case st := <-statuses:
			if st.Type != "git_sync_status" {
				t.Errorf("status type = %q", st.Type)
			}
			if !reflect.DeepEqual(reposKeys(st), []string{"repo-a"}) {
				t.Errorf("status repos = %v, want [repo-a]", reposKeys(st))
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for git_sync_status #%d", i+1)
		}
	}
	cancel()
	select {
	case <-connectDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Connect did not return after context cancel")
	}
}

func reposKeys(st proto.GitSyncStatusMsg) []string {
	keys := make([]string, 0, len(st.Repositories))
	for _, r := range st.Repositories {
		keys = append(keys, r.RepoKey)
	}
	return keys
}
