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
	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/plugin"
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
// critical_patch_required in handshake_ok, inbound http_request messages get a 503
// http_response instead of being forwarded to the HTTP handler (ADR-063 §7.4), with the
// responder_id echoed back so the platform can still route the reply.
func TestCriticalPatchRequiredSuspendsDispatch(t *testing.T) {
	serverKey := genPrivKey(t)
	keyBytes := make([]byte, mldsa65.PrivateKeySize)
	serverKey.Pack((*[mldsa65.PrivateKeySize]byte)(keyBytes))

	var receivedResponse proto.HTTPResponseMsg
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

		if err := wsjson.Write(r.Context(), conn, proto.HTTPRequestMsg{
			Type: "http_request", RequestID: "req-1", ResponderID: "resp-1",
			ProviderKey: "jenkins", Method: "GET", Path: "/",
		}); err != nil {
			t.Errorf("write http_request: %v", err)
			return
		}
		// Read messages until we see the http_response (connector also sends connection_status).
		for {
			var raw json.RawMessage
			if err := wsjson.Read(r.Context(), conn, &raw); err != nil {
				t.Errorf("read response: %v", err)
				return
			}
			var tm proto.TypedMessage
			if err := json.Unmarshal(raw, &tm); err != nil {
				continue
			}
			if tm.Type == "http_response" {
				if err := json.Unmarshal(raw, &receivedResponse); err != nil {
					t.Errorf("unmarshal response: %v", err)
				}
				break
			}
		}
		close(responseReceived)
		_, _, _ = conn.Read(r.Context())
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "key")
	cfg := newTestConfig(srv.URL, keyFile)
	keyStore := auth.NewKeyStore(keyFile)
	client := ws.NewClient(cfg, keyStore)

	var httpHandlerCalled atomic.Bool
	client.SetHTTPPluginHandler(func(_ context.Context, _ proto.HTTPRequestMsg) (int, map[string]string, *string, error) {
		httpHandlerCalled.Store(true)
		body := "ok"
		return 200, nil, &body, nil
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

	if httpHandlerCalled.Load() {
		t.Error("HTTP handler was called despite critical_patch_required")
	}
	if receivedResponse.StatusCode != 503 {
		t.Errorf("status code = %d, want 503", receivedResponse.StatusCode)
	}
	if receivedResponse.RequestID != "req-1" {
		t.Errorf("request_id = %q, want %q", receivedResponse.RequestID, "req-1")
	}
	if receivedResponse.ResponderID != "resp-1" {
		t.Errorf("responder_id = %q, want %q (must be echoed even on the 503 path)", receivedResponse.ResponderID, "resp-1")
	}
}

// TestHeartbeatSendsHealthMessage verifies the connector sends an application-level
// {"type":"health"} message on the heartbeat tick — the only thing the gateway's Route()
// loop actually reads to refresh last_health_at (a WebSocket ping/pong control frame is
// invisible to it; the server's wsjson.Read here only ever sees the health text frame,
// since coder/websocket handles ping/pong transparently beneath it). Shortens the
// interval via SetHeartbeatIntervalForTest rather than waiting the real 30s.
func TestHeartbeatSendsHealthMessage(t *testing.T) {
	restore := ws.SetHeartbeatIntervalForTest(20 * time.Millisecond)
	defer restore()

	serverKey := genPrivKey(t)
	keyBytes := make([]byte, mldsa65.PrivateKeySize)
	serverKey.Pack((*[mldsa65.PrivateKeySize]byte)(keyBytes))

	healthReceived := make(chan struct{})

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
			Type:       "handshake_ok",
			PrivateKey: base64.StdEncoding.EncodeToString(keyBytes),
		}
		if err := wsjson.Write(r.Context(), conn, ok); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}

		// The connector emits connection_status right after the handshake, then sends health
		// messages on the heartbeat tick. Read until we see "health".
		for {
			var raw json.RawMessage
			if err := wsjson.Read(r.Context(), conn, &raw); err != nil {
				t.Errorf("read heartbeat: %v", err)
				return
			}
			var typed proto.TypedMessage
			if err := json.Unmarshal(raw, &typed); err != nil {
				t.Errorf("unmarshal heartbeat type: %v", err)
				return
			}
			if typed.Type == "health" {
				close(healthReceived)
				break
			}
			// Skip connection_status and any other interleaved messages.
		}

		if err := wsjson.Write(r.Context(), conn, map[string]string{"type": "going_away"}); err != nil {
			t.Errorf("write going_away: %v", err)
			return
		}
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

	select {
	case <-healthReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a health heartbeat message")
	}

	select {
	case err := <-connectDone:
		if err != nil {
			t.Errorf("Connect returned an error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Connect to return after going_away")
	}
}

// TestGoingAwayDrainsInFlightRequest verifies that a going_away disconnect waits for an
// in-flight handleHTTPRequest call to finish and write its response before the connection
// is torn down, instead of dropping it.
func TestGoingAwayDrainsInFlightRequest(t *testing.T) {
	serverKey := genPrivKey(t)
	keyBytes := make([]byte, mldsa65.PrivateKeySize)
	serverKey.Pack((*[mldsa65.PrivateKeySize]byte)(keyBytes))

	var receivedResponse proto.HTTPResponseMsg
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
			Type:       "handshake_ok",
			PrivateKey: base64.StdEncoding.EncodeToString(keyBytes),
		}
		if err := wsjson.Write(r.Context(), conn, ok); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}

		// Send an http_request, then going_away immediately after — before the handler
		// (which sleeps briefly) has had a chance to respond. If the drain works, the
		// response still arrives; if going_away just tore the connection down, this read
		// would time out or error instead.
		if err := wsjson.Write(r.Context(), conn, proto.HTTPRequestMsg{
			Type: "http_request", RequestID: "req-1", ResponderID: "resp-1",
			ProviderKey: "jenkins", Method: "GET", Path: "/",
		}); err != nil {
			t.Errorf("write http_request: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, map[string]string{"type": "going_away"}); err != nil {
			t.Errorf("write going_away: %v", err)
			return
		}

		// Read until we see http_response (connector may also send connection_status).
		for {
			var raw json.RawMessage
			if err := wsjson.Read(r.Context(), conn, &raw); err != nil {
				t.Errorf("read response: %v", err)
				return
			}
			var tm proto.TypedMessage
			if err := json.Unmarshal(raw, &tm); err != nil {
				continue
			}
			if tm.Type == "http_response" {
				if err := json.Unmarshal(raw, &receivedResponse); err != nil {
					t.Errorf("unmarshal response: %v", err)
				}
				break
			}
		}
		close(responseReceived)
		_, _, _ = conn.Read(r.Context())
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "key")
	cfg := newTestConfig(srv.URL, keyFile)
	keyStore := auth.NewKeyStore(keyFile)
	client := ws.NewClient(cfg, keyStore)
	client.SetHTTPPluginHandler(func(_ context.Context, _ proto.HTTPRequestMsg) (int, map[string]string, *string, error) {
		time.Sleep(100 * time.Millisecond) // simulate a slow downstream call
		body := `{"ok":true}`
		return 200, nil, &body, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connectDone := make(chan error, 1)
	go func() { connectDone <- client.Connect(ctx) }()

	select {
	case <-responseReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the in-flight response to be drained through")
	}
	<-connectDone

	if receivedResponse.StatusCode != 200 {
		t.Errorf("status code = %d, want 200 (in-flight request should have completed, not been dropped)", receivedResponse.StatusCode)
	}
	if receivedResponse.RequestID != "req-1" {
		t.Errorf("request_id = %q, want %q", receivedResponse.RequestID, "req-1")
	}
}

// TestRunLoopDispatchesHTTPRequest verifies that an inbound http_request message is
// forwarded to the HTTPPluginHandler and the http_response is sent back with the correct
// request_id and status_code.
func TestRunLoopDispatchesHTTPRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`pong`)) //nolint:errcheck
	}))
	defer upstream.Close()

	store, err := creds.New(t.TempDir() + "/credentials.json")
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	if err := store.Set("svc", creds.Credential{BaseURL: upstream.URL}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}

	serverKey := genPrivKey(t)
	keyBytes := make([]byte, mldsa65.PrivateKeySize)
	serverKey.Pack((*[mldsa65.PrivateKeySize]byte)(keyBytes))

	var gotResponse proto.HTTPResponseMsg
	done := make(chan struct{})

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
			Type:       "handshake_ok",
			PrivateKey: base64.StdEncoding.EncodeToString(keyBytes),
		}
		if err := wsjson.Write(r.Context(), conn, ok); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}

		// Send http_request after handshake.
		if err := wsjson.Write(r.Context(), conn, proto.HTTPRequestMsg{
			Type: "http_request", RequestID: "r42",
			ProviderKey: "svc", Method: "GET", Path: "/",
		}); err != nil {
			t.Logf("write http_request: %v", err)
			return
		}
		// Read messages until we get http_response.
		for {
			var raw json.RawMessage
			if err := wsjson.Read(r.Context(), conn, &raw); err != nil {
				return
			}
			var tm proto.TypedMessage
			if err := json.Unmarshal(raw, &tm); err != nil {
				continue
			}
			if tm.Type == "http_response" {
				if err := json.Unmarshal(raw, &gotResponse); err != nil {
					t.Logf("unmarshal http_response: %v", err)
				}
				close(done)
				return
			}
		}
	}))
	defer srv.Close()

	reg, err := plugin.Load(&config.Config{})
	if err != nil {
		t.Fatalf("plugin.Load: %v", err)
	}

	keyFile := filepath.Join(t.TempDir(), "key")
	cfg := newTestConfig(srv.URL, keyFile)
	client := ws.NewClient(cfg, auth.NewKeyStore(keyFile))
	client.SetHTTPPluginHandler(plugin.HTTPHandler(reg, store))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go client.Connect(ctx) //nolint:errcheck

	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for http_response")
	}
	cancel()

	if gotResponse.RequestID != "r42" {
		t.Fatalf("expected request_id r42, got %q", gotResponse.RequestID)
	}
	if gotResponse.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", gotResponse.StatusCode)
	}
}

// TestConnectionStatusEmittedOnConnect verifies that the connector sends a connection_status
// message after the handshake, and that a credential stored in the creds store appears in
// the plugins list.
func TestConnectionStatusEmittedOnConnect(t *testing.T) {
	store, err := creds.New(t.TempDir() + "/credentials.json")
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	if err := store.Set("jenkins", creds.Credential{BaseURL: "http://jenkins:8080", Auth: "bearer", Token: "test-token-value-123"}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}

	serverKey := genPrivKey(t)
	keyBytes := make([]byte, mldsa65.PrivateKeySize)
	serverKey.Pack((*[mldsa65.PrivateKeySize]byte)(keyBytes))

	var gotStatus proto.ConnectionStatusMsg
	done := make(chan struct{})

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
			Type:       "handshake_ok",
			PrivateKey: base64.StdEncoding.EncodeToString(keyBytes),
		}
		if err := wsjson.Write(r.Context(), conn, ok); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}

		// Read messages until we see connection_status
		for {
			var raw json.RawMessage
			if err := wsjson.Read(r.Context(), conn, &raw); err != nil {
				return
			}
			var tm proto.TypedMessage
			if err := json.Unmarshal(raw, &tm); err != nil {
				continue
			}
			if tm.Type == "connection_status" {
				if err := json.Unmarshal(raw, &gotStatus); err != nil {
					t.Logf("unmarshal connection_status: %v", err)
				}
				close(done)
				return
			}
		}
	}))
	defer srv.Close()

	reg, err := plugin.Load(&config.Config{})
	if err != nil {
		t.Fatalf("plugin.Load: %v", err)
	}

	keyFile := filepath.Join(t.TempDir(), "key")
	cfg := newTestConfig(srv.URL, keyFile)
	client := ws.NewClient(cfg, auth.NewKeyStore(keyFile))
	client.SetCredsStore(store)
	client.SetPluginRegistry(reg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go client.Connect(ctx) //nolint:errcheck

	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for connection_status")
	}
	cancel()

	if gotStatus.Type != "connection_status" {
		t.Fatalf("unexpected type: %q", gotStatus.Type)
	}
	// Jenkins is in creds store → should appear in the plugin list
	found := false
	for _, p := range gotStatus.Plugins {
		if p.ProviderKey == "jenkins" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected jenkins in connection_status plugins, got %+v", gotStatus.Plugins)
	}
}

// newFakeGateway is a helper that starts a fake WebSocket gateway server.
// It performs the registration handshake and then calls afterHandshake with the open conn.
func newFakeGateway(t *testing.T, afterHandshake func(ctx context.Context, conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	serverKey := genPrivKey(t)
	keyBytes := make([]byte, mldsa65.PrivateKeySize)
	serverKey.Pack((*[mldsa65.PrivateKeySize]byte)(keyBytes))

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			Type:       "handshake_ok",
			PrivateKey: base64.StdEncoding.EncodeToString(keyBytes),
		}
		if err := wsjson.Write(r.Context(), conn, ok); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}
		afterHandshake(r.Context(), conn)
	}))
}

// readNextOfType reads from conn, skipping any messages not matching wantType.
func readNextOfType(ctx context.Context, t *testing.T, conn *websocket.Conn, wantType string) json.RawMessage {
	t.Helper()
	for {
		var raw json.RawMessage
		if err := wsjson.Read(ctx, conn, &raw); err != nil {
			t.Fatalf("read: %v", err)
		}
		var tm proto.TypedMessage
		if err := json.Unmarshal(raw, &tm); err != nil {
			continue
		}
		if tm.Type == wantType {
			return raw
		}
	}
}

// TestStatusRequestTriggersEmission verifies that a status_request from the gateway
// causes the connector to reply with a connection_status message.
func TestStatusRequestTriggersEmission(t *testing.T) {
	store, err := creds.New(t.TempDir() + "/credentials.json")
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	if err := store.Set("svc", creds.Credential{BaseURL: "http://svc:9000"}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}

	var gotStatus proto.ConnectionStatusMsg
	done := make(chan struct{})

	srv := newFakeGateway(t, func(ctx context.Context, conn *websocket.Conn) {
		// Drain the initial connection_status emitted right after handshake.
		readNextOfType(ctx, t, conn, "connection_status")

		// Send status_request.
		if err := wsjson.Write(ctx, conn, map[string]string{"type": "status_request"}); err != nil {
			t.Errorf("write status_request: %v", err)
			return
		}

		// Expect another connection_status in response.
		raw := readNextOfType(ctx, t, conn, "connection_status")
		if err := json.Unmarshal(raw, &gotStatus); err != nil {
			t.Errorf("unmarshal connection_status: %v", err)
		}
		close(done)
		_, _, _ = conn.Read(ctx)
	})
	defer srv.Close()

	reg, err := plugin.Load(&config.Config{})
	if err != nil {
		t.Fatalf("plugin.Load: %v", err)
	}

	keyFile := filepath.Join(t.TempDir(), "key")
	cfg := newTestConfig(srv.URL, keyFile)
	client := ws.NewClient(cfg, auth.NewKeyStore(keyFile))
	client.SetCredsStore(store)
	client.SetPluginRegistry(reg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go client.Connect(ctx) //nolint:errcheck

	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for connection_status after status_request")
	}

	if gotStatus.Type != "connection_status" {
		t.Errorf("type = %q, want connection_status", gotStatus.Type)
	}
	found := false
	for _, p := range gotStatus.Plugins {
		if p.ProviderKey == "svc" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected svc in plugins, got %+v", gotStatus.Plugins)
	}
}

// TestTestConnectionProbesPlugin verifies that a test_connection message triggers a real
// HTTP probe and produces a test_result followed by a connection_status update.
func TestTestConnectionProbesPlugin(t *testing.T) {
	const username = "local-user"
	const token = "basic-token-value-123"
	expectedAuthorization := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+token))

	// Start a real upstream HTTP server so the probe actually performs the local
	// Basic authentication configured for this plugin.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != expectedAuthorization {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	store, err := creds.New(t.TempDir() + "/credentials.json")
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	if err := store.Set("svc", creds.Credential{
		BaseURL:  upstream.URL,
		Auth:     "basic",
		Username: username,
		Token:    token,
	}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}

	var gotResult proto.TestResultMsg
	var gotStatus proto.ConnectionStatusMsg
	done := make(chan struct{})

	srv := newFakeGateway(t, func(ctx context.Context, conn *websocket.Conn) {
		// Drain the initial connection_status.
		readNextOfType(ctx, t, conn, "connection_status")

		// Send test_connection for our plugin.
		if err := wsjson.Write(ctx, conn, proto.TestConnectionMsg{
			Type:        "test_connection",
			ProviderKey: "svc",
		}); err != nil {
			t.Errorf("write test_connection: %v", err)
			return
		}

		// Expect test_result.
		rawResult := readNextOfType(ctx, t, conn, "test_result")
		if err := json.Unmarshal(rawResult, &gotResult); err != nil {
			t.Errorf("unmarshal test_result: %v", err)
		}

		// Expect connection_status emitted after the test.
		rawStatus := readNextOfType(ctx, t, conn, "connection_status")
		if err := json.Unmarshal(rawStatus, &gotStatus); err != nil {
			t.Errorf("unmarshal connection_status: %v", err)
		}
		close(done)
		_, _, _ = conn.Read(ctx)
	})
	defer srv.Close()

	reg, err := plugin.Load(&config.Config{})
	if err != nil {
		t.Fatalf("plugin.Load: %v", err)
	}

	keyFile := filepath.Join(t.TempDir(), "key")
	cfg := newTestConfig(srv.URL, keyFile)
	client := ws.NewClient(cfg, auth.NewKeyStore(keyFile))
	client.SetCredsStore(store)
	client.SetPluginRegistry(reg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go client.Connect(ctx) //nolint:errcheck

	select {
	case <-done:
	case <-time.After(9 * time.Second):
		t.Fatal("timed out waiting for test_result and connection_status")
	}

	if gotResult.Type != "test_result" {
		t.Errorf("test_result type = %q, want test_result", gotResult.Type)
	}
	if gotResult.ProviderKey != "svc" {
		t.Errorf("provider_key = %q, want svc", gotResult.ProviderKey)
	}
	if gotResult.Status != "connected" {
		t.Errorf("status = %q, want connected (diagnostic: %q)", gotResult.Status, gotResult.Diagnostic)
	}
	if gotStatus.Type != "connection_status" {
		t.Errorf("connection_status type = %q, want connection_status", gotStatus.Type)
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
