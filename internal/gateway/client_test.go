package gateway_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/gateway"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// noopHandler is a Handler that does nothing — it lets these tests exercise the gateway's
// own lifecycle (dial, handshake, heartbeat) without any connector-specific behavior.
type noopHandler struct{}

func (noopHandler) OnHandshake(gateway.HandshakeInfo)                                      {}
func (noopHandler) OnConnected(context.Context, gateway.Sender)                            {}
func (noopHandler) OnHeartbeat(context.Context, gateway.Sender) error                      { return nil }
func (noopHandler) HandleMessage(context.Context, gateway.Sender, string, json.RawMessage) {}
func (noopHandler) OnDraining(context.Context)                                             {}

// newTestConfig builds a gateway.Config pointing at the given test server URL.
func newTestConfig(serverURL string) gateway.Config {
	wsURL := strings.Replace(serverURL, "http://", "ws://", 1) // config validation elsewhere expects ws://
	return gateway.Config{
		GatewayURL:        wsURL,
		ConnectorID:       "test-connector",
		RegistrationToken: "tok-test",
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

// gatewayHelloMsg / gatewayHandshakeOkMsg are independent, hand-maintained copies of the
// real Bridge Gateway's wire shapes, so a bug in mkonnect's proto encoding can't be masked
// by using the same type on both ends.
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

// writeGatewayChallenge writes the challenge frame exactly as the gateway does.
func writeGatewayChallenge(ctx context.Context, conn *websocket.Conn, nonce [32]byte) error {
	return wsjson.Write(ctx, conn, map[string]string{
		"type":      "challenge",
		"challenge": base64.StdEncoding.EncodeToString(nonce[:]),
	})
}

// TestHeartbeatSendsHealthMessage verifies the gateway client sends an application-level
// {"type":"health"} message on the heartbeat tick — the only thing the gateway's Route()
// loop reads to refresh last_health_at (a WebSocket ping/pong control frame is invisible to
// it). Shortens the interval via SetHeartbeatIntervalForTest rather than waiting 30s.
func TestHeartbeatSendsHealthMessage(t *testing.T) {
	restore := gateway.SetHeartbeatIntervalForTest(20 * time.Millisecond)
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

		// Read until we see a "health" message (a no-op handler emits nothing else).
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
		}

		if err := wsjson.Write(r.Context(), conn, map[string]string{"type": "going_away"}); err != nil {
			t.Errorf("write going_away: %v", err)
			return
		}
		_, _, _ = conn.Read(r.Context())
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "key")
	client := gateway.NewClient(newTestConfig(srv.URL), auth.NewKeyStore(keyFile), noopHandler{})

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

// TestConnectTimesOutOnStuckDial verifies that a dial which stalls mid-upgrade (TCP accepted,
// HTTP upgrade response never sent) fails within the dial timeout and re-enters Run's backoff,
// instead of blocking Connect forever (issue #18).
func TestConnectTimesOutOnStuckDial(t *testing.T) {
	restore := gateway.SetDialTimeoutForTest(200 * time.Millisecond)
	defer restore()

	var contacted atomic.Bool
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		// Accept the request but never respond, so the dial blocks on the upgrade response
		// until its dial context expires. Unblocked only at cleanup.
		contacted.Store(true)
		<-hold
	}))
	// LIFO: close(hold) runs first to release the handler, then srv.Close() can return.
	defer srv.Close()
	defer close(hold)

	keyFile := filepath.Join(t.TempDir(), "key") // writable dir, no key -> first-run path reaches the dial
	client := gateway.NewClient(newTestConfig(srv.URL), auth.NewKeyStore(keyFile), noopHandler{})

	done := make(chan error, 1)
	go func() { done <- client.Connect(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Connect to fail on a stuck dial")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want it to wrap context.DeadlineExceeded (the dial timeout)", err)
		}
		if !strings.Contains(err.Error(), "ws dial") {
			t.Errorf("error = %v, want it to identify the dial stage", err)
		}
		if !contacted.Load() {
			t.Error("black-hole server was never contacted — the test did not exercise a real dial")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Connect did not return within 2s on a stuck dial — the reconnect hang (issue #18) has regressed")
	}
}
