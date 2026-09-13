package ws_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"
	"strings"
	"testing"

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
	_, priv, err := mldsa65.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return priv
}

// TestRegistrationHandshake verifies the first-run registration flow:
// the client sends HelloMsg with token, the server replies HandshakeOkMsg,
// and the client persists the private key.
func TestRegistrationHandshake(t *testing.T) {
	// Generate a key to hand back to the client.
	serverKey := genPrivKey(t)
	keyBytes := make([]byte, mldsa65.PrivateKeySize)
	serverKey.Pack((*[mldsa65.PrivateKeySize]byte)(keyBytes))

	var receivedHello proto.HelloMsg
	keySent := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck

		// Read hello
		if err := wsjson.Read(r.Context(), conn, &receivedHello); err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		// Send handshake_ok
		ok := proto.HandshakeOkMsg{
			Type:       "handshake_ok",
			PrivateKey: keyBytes,
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
	if receivedHello.Token != "tok-test" {
		t.Errorf("hello token = %q, want %q", receivedHello.Token, "tok-test")
	}
	if receivedHello.ConnectorID != "test-connector" {
		t.Errorf("hello connector_id = %q, want %q", receivedHello.ConnectorID, "test-connector")
	}
	if len(receivedHello.Signature) != 0 {
		t.Errorf("expected no signature on first-run, got %d bytes", len(receivedHello.Signature))
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

// TestReconnectHandshake verifies the reconnect flow:
// the server sends ChallengeMsg, the client replies HelloMsg with a valid signature.
func TestReconnectHandshake(t *testing.T) {
	// Pre-generate a key and save it so the client thinks it's registered.
	existingKey := genPrivKey(t)
	keyFile := filepath.Join(t.TempDir(), "key")
	keyStore := auth.NewKeyStore(keyFile)
	if err := keyStore.Save(existingKey); err != nil {
		t.Fatalf("save existing key: %v", err)
	}

	nonce := proto.Nonce{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}

	var receivedHello proto.HelloMsg
	helloDone := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck

		// Read identify hello (no token, no signature)
		var identHello proto.HelloMsg
		if err := wsjson.Read(r.Context(), conn, &identHello); err != nil {
			t.Errorf("read identify hello: %v", err)
			return
		}
		// Send challenge
		challenge := proto.ChallengeMsg{
			Type:  "challenge",
			Nonce: nonce,
		}
		if err := wsjson.Write(r.Context(), conn, challenge); err != nil {
			t.Errorf("write challenge: %v", err)
			return
		}
		// Read hello with signature
		if err := wsjson.Read(r.Context(), conn, &receivedHello); err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		close(helloDone)
		// Send handshake_ok
		ok := proto.HandshakeOkMsg{Type: "handshake_ok"}
		if err := wsjson.Write(r.Context(), conn, ok); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}
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

	// Wait until server received the hello, then cancel.
	<-helloDone
	cancel()
	<-connectDone

	if receivedHello.Type != "hello" {
		t.Fatalf("did not receive hello from client")
	}
	if receivedHello.ConnectorID != "test-connector" {
		t.Errorf("connector_id = %q, want %q", receivedHello.ConnectorID, "test-connector")
	}
	if receivedHello.Token != "" {
		t.Errorf("expected no token on reconnect, got %q", receivedHello.Token)
	}
	if len(receivedHello.Signature) == 0 {
		t.Fatal("expected signature on reconnect, got none")
	}

	// Verify the signature using the public key.
	pubKey := existingKey.Public().(*mldsa65.PublicKey)
	msg := make([]byte, 32+len("test-connector"))
	copy(msg[:32], nonce[:])
	copy(msg[32:], "test-connector")
	if !mldsa65.Verify(pubKey, msg, nil, receivedHello.Signature) {
		t.Error("signature verification failed")
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
