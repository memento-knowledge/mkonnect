package ws_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/proto"
	"github.com/memento-knowledge/mkonnect/internal/ws"
)

// newTestConfig builds a config pointing at the given test server URL.
func newTestConfig(serverURL, keyFile string) *config.Config {
	// Convert http:// -> ws:// for config validation
	wsURL := strings.Replace(serverURL, "http://", "ws://", 1)
	return &config.Config{
		GatewayURL:        wsURL,
		ConnectorID:       "test-connector",
		RegistrationToken: "tok-test",
		KeyFile:           keyFile,
		ProtocolVersion:   "v1",
	}
}

// genPrivKey generates a fresh ML-DSA-65 key pair for tests.
func genPrivKey(t *testing.T) *mldsa65.PrivateKey {
	t.Helper()
	_, priv, err := mldsa65.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return priv
}

// gatewayHelloMsg, gatewayHandshakeOkMsg, and gatewayHandshakeErrorMsg are independent,
// hand-maintained copies of the real Bridge Gateway's actual wire shapes
// (services/bridge-gateway/internal/ws/registration.go) — Signature and PrivateKey are
// base64 strings there, not []byte. Mock servers below use these, not mkonnect's own
// proto package types, so a bug in proto's encoding can't be masked by using the same
// (possibly buggy) type on both ends of the test.
type gatewayHelloMsg struct {
	Type              string `json:"type"`
	ConnectorID       string `json:"connector_id"`
	ProtocolVersion   string `json:"protocol_version"`
	ConnectorVersion  string `json:"connector_version"`
	RegistrationToken string `json:"registration_token,omitempty"`
	Signature         string `json:"signature,omitempty"`
}

type gatewayHandshakeOkMsg struct {
	Type              string `json:"type"`
	DeprecationNotice string `json:"deprecation_notice,omitempty"`
	CriticalPatch     bool   `json:"critical_patch_required,omitempty"`
	PrivateKey        string `json:"private_key,omitempty"`
}

type gatewayHandshakeErrorMsg struct {
	Type   string `json:"type"`
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

// writeGatewayChallenge writes the challenge frame exactly as the gateway does: a plain
// map with the nonce base64-encoded manually — independent of proto.ChallengeMsg/Nonce.
func writeGatewayChallenge(ctx context.Context, conn *websocket.Conn, nonce [32]byte) error {
	return wsjson.Write(ctx, conn, map[string]string{
		"type":      "challenge",
		"challenge": base64.StdEncoding.EncodeToString(nonce[:]),
	})
}

// TestRegistrationHandshake verifies the first-run registration flow: the gateway sends the
// challenge first (ignored on this path), the client sends HelloMsg with a registration
// token, the server replies handshake_ok, and the client persists the private key.
func TestRegistrationHandshake(t *testing.T) {
	// Generate a key to hand back to the client.
	serverKey := genPrivKey(t)
	keyBytes := make([]byte, mldsa65.PrivateKeySize)
	serverKey.Pack((*[mldsa65.PrivateKeySize]byte)(keyBytes))

	var receivedHello gatewayHelloMsg
	keySent := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck

		// Gateway always sends the challenge first, even though registration ignores it.
		var challengeNonce [32]byte
		for i := range challengeNonce {
			challengeNonce[i] = byte(i)
		}
		if err := writeGatewayChallenge(r.Context(), conn, challengeNonce); err != nil {
			t.Errorf("write challenge: %v", err)
			return
		}

		// Read hello using the gateway's own literal shape, not mkonnect's HelloMsg.
		if err := wsjson.Read(r.Context(), conn, &receivedHello); err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		// Send handshake_ok with the private key base64-encoded, matching the gateway.
		ok := gatewayHandshakeOkMsg{
			Type:       "handshake_ok",
			PrivateKey: base64.StdEncoding.EncodeToString(keyBytes),
		}
		if err := wsjson.Write(r.Context(), conn, ok); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}
		close(keySent)
		// Wait for connection to close
		_, _, _ = conn.Read(r.Context())
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "key")
	cfg := newTestConfig(srv.URL, keyFile)
	keyStore := auth.NewKeyStore(keyFile)
	client := ws.NewClient(cfg, keyStore)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectDone := make(chan error, 1)
	go func() {
		connectDone <- client.Connect(ctx)
	}()

	// Wait until the server sent the key, then poll until client saves it.
	<-keySent
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(keyFile); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-connectDone

	// Verify hello fields
	if receivedHello.Type != "hello" {
		t.Errorf("hello type = %q, want %q", receivedHello.Type, "hello")
	}
	if receivedHello.RegistrationToken != "tok-test" {
		t.Errorf("hello registration_token = %q, want %q", receivedHello.RegistrationToken, "tok-test")
	}
	if receivedHello.ConnectorVersion == "" {
		t.Error("hello connector_version is empty, want a non-empty version string")
	}
	if receivedHello.ConnectorID != "test-connector" {
		t.Errorf("hello connector_id = %q, want %q", receivedHello.ConnectorID, "test-connector")
	}
	if receivedHello.Signature != "" {
		t.Errorf("expected no signature on first-run, got %q", receivedHello.Signature)
	}

	// Verify key was saved
	_, found, err := keyStore.Load()
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	if !found {
		t.Fatal("key was not saved after registration")
	}
}

// TestReconnectHandshake verifies the reconnect flow: the gateway sends the challenge
// first, the client replies with a single signed HelloMsg (no separate "identify" step),
// and the client only returns cleanly once handshake_ok has actually been processed.
func TestReconnectHandshake(t *testing.T) {
	// Pre-generate a key and save it so the client thinks it's registered.
	existingKey := genPrivKey(t)
	keyFile := filepath.Join(t.TempDir(), "key")
	keyStore := auth.NewKeyStore(keyFile)
	if err := keyStore.Save(existingKey); err != nil {
		t.Fatalf("save existing key: %v", err)
	}

	nonce := [32]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}

	var receivedHello gatewayHelloMsg
	helloDone := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck

		// Send challenge first (unconditionally).
		if err := writeGatewayChallenge(r.Context(), conn, nonce); err != nil {
			t.Errorf("write challenge: %v", err)
			return
		}
		// Read the signed hello.
		if err := wsjson.Read(r.Context(), conn, &receivedHello); err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		// Send handshake_ok, then going_away so the client's runLoop returns cleanly on
		// its own (Task 3's going_away handling) — this avoids a race between
		// test-driven context cancellation and the client still reading the handshake_ok
		// bytes already in flight on the wire, and it's what actually lets the test
		// verify the client processes handshake_ok, not just that it sent a hello.
		ok := gatewayHandshakeOkMsg{Type: "handshake_ok"}
		if err := wsjson.Write(r.Context(), conn, ok); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, map[string]string{"type": "going_away"}); err != nil {
			t.Errorf("write going_away: %v", err)
			return
		}
		close(helloDone)
		_, _, _ = conn.Read(r.Context())
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL, keyFile)
	client := ws.NewClient(cfg, keyStore)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectDone := make(chan error, 1)
	go func() {
		connectDone <- client.Connect(ctx)
	}()

	<-helloDone
	select {
	case err := <-connectDone:
		if err != nil {
			t.Errorf("Connect returned an error after a clean handshake: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Connect to return after going_away")
	}

	if receivedHello.Type != "hello" {
		t.Fatalf("did not receive hello from client")
	}
	if receivedHello.ConnectorID != "test-connector" {
		t.Errorf("connector_id = %q, want %q", receivedHello.ConnectorID, "test-connector")
	}
	if receivedHello.RegistrationToken != "" {
		t.Errorf("expected no registration_token on reconnect, got %q", receivedHello.RegistrationToken)
	}
	if receivedHello.Signature == "" {
		t.Fatal("expected signature on reconnect, got none")
	}

	sigBytes, err := base64.StdEncoding.DecodeString(receivedHello.Signature)
	if err != nil {
		t.Fatalf("signature is not valid base64: %v", err)
	}

	// Verify the signature using the public key.
	pubKey := existingKey.Public().(*mldsa65.PublicKey)
	msg := make([]byte, 32+len("test-connector"))
	copy(msg[:32], nonce[:])
	copy(msg[32:], "test-connector")
	if !mldsa65.Verify(pubKey, msg, nil, sigBytes) {
		t.Error("signature verification failed")
	}
}

// TestHandshakeErrorRejectsRegistration verifies a handshake_error response during
// registration surfaces the gateway's code and reason, and does not save a key. The mock
// mirrors the gateway's actual reject() (registration.go): write the error frame, then
// close with the numeric status code and reason — not just CloseNow().
func TestHandshakeErrorRejectsRegistration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck

		var challengeNonce [32]byte
		if err := writeGatewayChallenge(r.Context(), conn, challengeNonce); err != nil {
			t.Errorf("write challenge: %v", err)
			return
		}
		var hello gatewayHelloMsg
		if err := wsjson.Read(r.Context(), conn, &hello); err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		const code, reason = 4001, "invalid registration token"
		errMsg := gatewayHandshakeErrorMsg{Type: "handshake_error", Code: code, Reason: reason}
		if err := wsjson.Write(r.Context(), conn, errMsg); err != nil {
			t.Errorf("write handshake_error: %v", err)
			return
		}
		_ = conn.Close(websocket.StatusCode(code), reason)
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "key")
	cfg := newTestConfig(srv.URL, keyFile)
	keyStore := auth.NewKeyStore(keyFile)
	client := ws.NewClient(cfg, keyStore)

	err := client.Connect(context.Background())
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid registration token") {
		t.Errorf("error = %v, want it to contain the gateway's reason", err)
	}
	if _, found, _ := keyStore.Load(); found {
		t.Error("key was saved despite a handshake_error response")
	}
}

// TestHandshakeErrorRejectsReconnect covers the reconnect path — an unregistered or
// signature-mismatched connector is the more common production failure mode than a bad
// registration token (registration only happens once).
func TestHandshakeErrorRejectsReconnect(t *testing.T) {
	existingKey := genPrivKey(t)
	keyFile := filepath.Join(t.TempDir(), "key")
	keyStore := auth.NewKeyStore(keyFile)
	if err := keyStore.Save(existingKey); err != nil {
		t.Fatalf("save existing key: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck

		var challengeNonce [32]byte
		if err := writeGatewayChallenge(r.Context(), conn, challengeNonce); err != nil {
			t.Errorf("write challenge: %v", err)
			return
		}
		var hello gatewayHelloMsg
		if err := wsjson.Read(r.Context(), conn, &hello); err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		const code, reason = 4001, "connector not registered"
		errMsg := gatewayHandshakeErrorMsg{Type: "handshake_error", Code: code, Reason: reason}
		if err := wsjson.Write(r.Context(), conn, errMsg); err != nil {
			t.Errorf("write handshake_error: %v", err)
			return
		}
		_ = conn.Close(websocket.StatusCode(code), reason)
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL, keyFile)
	client := ws.NewClient(cfg, keyStore)

	err := client.Connect(context.Background())
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "connector not registered") {
		t.Errorf("error = %v, want it to contain the gateway's reason", err)
	}
}

// TestDeprecationNoticeIsNonFatal verifies a deprecation_notice in handshake_ok does not
// fail the connection — it's a warning, not a rejection (ADR-063 §10). Uses the same
// keySent-then-poll pattern as TestRegistrationHandshake rather than an arbitrary sleep, so
// it doesn't burn wall-clock time and doesn't need to distinguish "no error yet" from "no
// error ever" via a timeout race.
func TestDeprecationNoticeIsNonFatal(t *testing.T) {
	serverKey := genPrivKey(t)
	keyBytes := make([]byte, mldsa65.PrivateKeySize)
	serverKey.Pack((*[mldsa65.PrivateKeySize]byte)(keyBytes))
	keySent := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck

		var challengeNonce [32]byte
		if err := writeGatewayChallenge(r.Context(), conn, challengeNonce); err != nil {
			t.Errorf("write challenge: %v", err)
			return
		}
		var hello gatewayHelloMsg
		if err := wsjson.Read(r.Context(), conn, &hello); err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		ok := gatewayHandshakeOkMsg{
			Type:              "handshake_ok",
			DeprecationNotice: "protocol_version v1 deprecated, update by 2026-12-01",
			PrivateKey:        base64.StdEncoding.EncodeToString(keyBytes),
		}
		if err := wsjson.Write(r.Context(), conn, ok); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}
		close(keySent)
		_, _, _ = conn.Read(r.Context())
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "key")
	cfg := newTestConfig(srv.URL, keyFile)
	keyStore := auth.NewKeyStore(keyFile)
	client := ws.NewClient(cfg, keyStore)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connectDone := make(chan error, 1)
	go func() { connectDone <- client.Connect(ctx) }()

	<-keySent
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, found, _ := keyStore.Load(); found {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, found, _ := keyStore.Load(); !found {
		t.Fatal("key was not saved despite a deprecation_notice-only (non-fatal) handshake_ok")
	}
	cancel()
	<-connectDone
}

// TestCriticalPatchRequiredSuspendsDispatch verifies that once the gateway sets
// critical_patch_required in handshake_ok, inbound "data" messages get a 503 instead of
// being forwarded to the plugin handler (ADR-063 §7.4). This exercises mkonnect-internal
// behavior only: a real gateway can't currently deliver "data" or read "response" at all
// (its inboundAllowlist expects http_response) until #2212 replaces DataMsg/ResponseMsg —
// see the NOTE on this code path in client.go.
func TestCriticalPatchRequiredSuspendsDispatch(t *testing.T) {
	serverKey := genPrivKey(t)
	keyBytes := make([]byte, mldsa65.PrivateKeySize)
	serverKey.Pack((*[mldsa65.PrivateKeySize]byte)(keyBytes))

	var receivedResponse proto.ResponseMsg
	responseReceived := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck

		var challengeNonce [32]byte
		if err := writeGatewayChallenge(r.Context(), conn, challengeNonce); err != nil {
			t.Errorf("write challenge: %v", err)
			return
		}
		var hello gatewayHelloMsg
		if err := wsjson.Read(r.Context(), conn, &hello); err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		ok := gatewayHandshakeOkMsg{
			Type: "handshake_ok", CriticalPatch: true,
			PrivateKey: base64.StdEncoding.EncodeToString(keyBytes),
		}
		if err := wsjson.Write(r.Context(), conn, ok); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}

		if err := wsjson.Write(r.Context(), conn, proto.DataMsg{
			Type: "data", RequestID: "req-1", Plugin: "jenkins", Method: "GET", Path: "/",
		}); err != nil {
			t.Errorf("write data: %v", err)
			return
		}
		if err := wsjson.Read(r.Context(), conn, &receivedResponse); err != nil {
			t.Errorf("read response: %v", err)
			return
		}
		close(responseReceived)
		_, _, _ = conn.Read(r.Context())
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "key")
	cfg := newTestConfig(srv.URL, keyFile)
	keyStore := auth.NewKeyStore(keyFile)
	client := ws.NewClient(cfg, keyStore)

	var pluginHandlerCalled atomic.Bool
	client.SetPluginHandler(func(_ context.Context, _ proto.DataMsg) (int, []byte, error) {
		pluginHandlerCalled.Store(true)
		return 200, []byte("ok"), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connectDone := make(chan error, 1)
	go func() { connectDone <- client.Connect(ctx) }()

	select {
	case <-responseReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
	cancel()
	<-connectDone

	if pluginHandlerCalled.Load() {
		t.Error("plugin handler was called despite critical_patch_required")
	}
	if receivedResponse.StatusCode != 503 {
		t.Errorf("status code = %d, want 503", receivedResponse.StatusCode)
	}
	if receivedResponse.RequestID != "req-1" {
		t.Errorf("request_id = %q, want %q", receivedResponse.RequestID, "req-1")
	}
}

// TestNonceMarshal verifies the Nonce type round-trips through JSON correctly.
func TestNonceMarshal(t *testing.T) {
	var n proto.Nonce
	for i := range n {
		n[i] = byte(i)
	}
	data, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var n2 proto.Nonce
	if err := json.Unmarshal(data, &n2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if n != n2 {
		t.Errorf("nonce round-trip mismatch: got %v, want %v", n2, n)
	}
}
