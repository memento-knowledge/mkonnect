package gateway_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/gateway"
)

// recordingHandler records, in order, the gateway lifecycle callbacks it receives, so a
// test can assert the Handler contract a second connector kind will rely on: the callback
// ordering, that going_away becomes OnDraining (never HandleMessage), that an unknown
// message reaches HandleMessage with its type and raw bytes intact, and that an
// OnHeartbeat error closes the connection.
type recordingHandler struct {
	mu       sync.Mutex
	events   []string
	lastType string
	lastRaw  json.RawMessage
	hbCalls  int
	hbErr    error // returned from OnHeartbeat when non-nil
}

func (h *recordingHandler) record(e string) {
	h.mu.Lock()
	h.events = append(h.events, e)
	h.mu.Unlock()
}

func (h *recordingHandler) OnHandshake(gateway.HandshakeInfo)           { h.record("OnHandshake") }
func (h *recordingHandler) OnConnected(context.Context, gateway.Sender) { h.record("OnConnected") }
func (h *recordingHandler) OnDraining(context.Context)                  { h.record("OnDraining") }

func (h *recordingHandler) OnHeartbeat(context.Context, gateway.Sender) error {
	h.mu.Lock()
	h.hbCalls++
	err := h.hbErr
	h.mu.Unlock()
	h.record("OnHeartbeat")
	return err
}

func (h *recordingHandler) HandleMessage(_ context.Context, _ gateway.Sender, typ string, raw json.RawMessage) {
	h.mu.Lock()
	h.lastType = typ
	h.lastRaw = append(json.RawMessage(nil), raw...)
	h.mu.Unlock()
	h.record("HandleMessage:" + typ)
}

func (h *recordingHandler) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

// handshakeOkWithKey returns a handshake_ok carrying a freshly generated private key, as
// the gateway does on first-run registration (the no-saved-key path these tests take).
func handshakeOkWithKey(t *testing.T) gatewayHandshakeOkMsg {
	t.Helper()
	serverKey := genPrivKey(t)
	keyBytes := make([]byte, mldsa65.PrivateKeySize)
	serverKey.Pack((*[mldsa65.PrivateKeySize]byte)(keyBytes))
	return gatewayHandshakeOkMsg{Type: "handshake_ok", PrivateKey: base64.StdEncoding.EncodeToString(keyBytes)}
}

// TestHandlerCallbackOrderAndDispatch pins the Handler contract: OnHandshake then
// OnConnected run before any dispatch, an application message the gateway does not consume
// reaches HandleMessage with its type and payload intact, and going_away is delivered as
// OnDraining rather than to HandleMessage. The default 30s heartbeat does not fire within
// the test window, so the callback order here is deterministic.
func TestHandlerCallbackOrderAndDispatch(t *testing.T) {
	ok := handshakeOkWithKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck

		var nonce [32]byte
		if err := writeGatewayChallenge(r.Context(), conn, nonce); err != nil {
			t.Errorf("write challenge: %v", err)
			return
		}
		var hello gatewayHelloMsg
		if err := wsjson.Read(r.Context(), conn, &hello); err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, ok); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}
		// An application message the gateway does not consume itself: it must be dispatched
		// to HandleMessage with its type and raw bytes intact.
		if err := wsjson.Write(r.Context(), conn, map[string]string{"type": "custom_thing", "note": "bar"}); err != nil {
			t.Errorf("write custom message: %v", err)
			return
		}
		// going_away must reach OnDraining, never HandleMessage.
		if err := wsjson.Write(r.Context(), conn, map[string]string{"type": "going_away"}); err != nil {
			t.Errorf("write going_away: %v", err)
			return
		}
		_, _, _ = conn.Read(r.Context())
	}))
	defer srv.Close()

	h := &recordingHandler{}
	keyFile := filepath.Join(t.TempDir(), "key")
	client := gateway.NewClient(newTestConfig(srv.URL), auth.NewKeyStore(keyFile), h)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect returned an error: %v", err)
	}

	events := h.snapshot()
	if len(events) < 4 {
		t.Fatalf("events = %v, want at least OnHandshake, OnConnected, HandleMessage, OnDraining", events)
	}
	if events[0] != "OnHandshake" {
		t.Errorf("first event = %q, want OnHandshake", events[0])
	}
	if events[1] != "OnConnected" {
		t.Errorf("second event = %q, want OnConnected (before any dispatch)", events[1])
	}
	if last := events[len(events)-1]; last != "OnDraining" {
		t.Errorf("last event = %q, want OnDraining", last)
	}
	for _, e := range events {
		if e == "HandleMessage:going_away" {
			t.Error("going_away was dispatched to HandleMessage; it must be consumed as OnDraining")
		}
	}

	h.mu.Lock()
	typ, raw := h.lastType, h.lastRaw
	h.mu.Unlock()
	if typ != "custom_thing" {
		t.Errorf("HandleMessage type = %q, want custom_thing", typ)
	}
	if !strings.Contains(string(raw), "bar") {
		t.Errorf("HandleMessage raw = %s, want it to carry the original payload", raw)
	}
}

// ctxHandler captures the context the gateway hands each callback.
type ctxHandler struct {
	mu                          sync.Mutex
	connected, handled, drained context.Context
}

func (h *ctxHandler) OnHandshake(gateway.HandshakeInfo) {}
func (h *ctxHandler) OnConnected(ctx context.Context, _ gateway.Sender) {
	h.mu.Lock()
	h.connected = ctx
	h.mu.Unlock()
}
func (h *ctxHandler) OnHeartbeat(context.Context, gateway.Sender) error { return nil }
func (h *ctxHandler) HandleMessage(ctx context.Context, _ gateway.Sender, _ string, _ json.RawMessage) {
	h.mu.Lock()
	h.handled = ctx
	h.mu.Unlock()
}
func (h *ctxHandler) OnDraining(ctx context.Context) {
	h.mu.Lock()
	h.drained = ctx
	h.mu.Unlock()
}

// TestHandlerContextLifetimes pins which context each callback receives, because a connector kind
// builds on it: HandleMessage and OnDraining get the process-lifetime Connect context, so work
// started from them (a long repository sync) is NOT cancelled when the connection drops or the
// gateway drains and can finish across a reconnect; OnConnected gets a connection-scoped context
// that ends with the connection. Both end when the process-lifetime context does.
func TestHandlerContextLifetimes(t *testing.T) {
	ok := handshakeOkWithKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck

		var nonce [32]byte
		if err := writeGatewayChallenge(r.Context(), conn, nonce); err != nil {
			t.Errorf("write challenge: %v", err)
			return
		}
		var hello gatewayHelloMsg
		if err := wsjson.Read(r.Context(), conn, &hello); err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, ok); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}
		for _, typ := range []string{"custom_thing", "going_away"} {
			if err := wsjson.Write(r.Context(), conn, map[string]string{"type": typ}); err != nil {
				t.Errorf("write %s: %v", typ, err)
				return
			}
		}
		_, _, _ = conn.Read(r.Context())
	}))
	defer srv.Close()

	h := &ctxHandler{}
	client := gateway.NewClient(newTestConfig(srv.URL), auth.NewKeyStore(filepath.Join(t.TempDir(), "key")), h)
	process, shutdown := context.WithCancel(context.Background())
	defer shutdown()

	if err := client.Connect(process); err != nil {
		t.Fatalf("Connect returned an error: %v", err)
	}

	h.mu.Lock()
	connected, handled, drained := h.connected, h.handled, h.drained
	h.mu.Unlock()
	if connected == nil || handled == nil || drained == nil {
		t.Fatalf("a callback did not run: OnConnected=%v HandleMessage=%v OnDraining=%v", connected != nil, handled != nil, drained != nil)
	}

	// The connection is over (the gateway drained).
	if connected.Err() == nil {
		t.Error("OnConnected's context must end with the connection")
	}
	if err := handled.Err(); err != nil {
		t.Errorf("HandleMessage's context ended with the connection (%v); work started from it would be cancelled by every drain or reconnect", err)
	}
	if err := drained.Err(); err != nil {
		t.Errorf("OnDraining's context ended with the connection: %v", err)
	}

	// Shutdown ends them.
	shutdown()
	if handled.Err() == nil || drained.Err() == nil {
		t.Error("HandleMessage's and OnDraining's contexts must end when the process-lifetime context does")
	}
}

// TestOnHeartbeatErrorClosesConnection verifies the documented contract that a Handler can
// force a reconnect by returning an error from OnHeartbeat: the gateway closes the
// connection and Connect returns, just as a failed health write would.
func TestOnHeartbeatErrorClosesConnection(t *testing.T) {
	restore := gateway.SetHeartbeatIntervalForTest(20 * time.Millisecond)
	defer restore()

	ok := handshakeOkWithKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck

		var nonce [32]byte
		if err := writeGatewayChallenge(r.Context(), conn, nonce); err != nil {
			t.Errorf("write challenge: %v", err)
			return
		}
		var hello gatewayHelloMsg
		if err := wsjson.Read(r.Context(), conn, &hello); err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, ok); err != nil {
			t.Errorf("write handshake_ok: %v", err)
			return
		}
		// Keep reading so the client's heartbeat ping gets a pong and its health message is
		// drained, until the client closes the connection after OnHeartbeat returns an error.
		for {
			var raw json.RawMessage
			if err := wsjson.Read(r.Context(), conn, &raw); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	h := &recordingHandler{hbErr: errors.New("handler requests reconnect")}
	keyFile := filepath.Join(t.TempDir(), "key")
	client := gateway.NewClient(newTestConfig(srv.URL), auth.NewKeyStore(keyFile), h)

	done := make(chan error, 1)
	go func() { done <- client.Connect(context.Background()) }()

	select {
	case <-done:
		// Connect returned because OnHeartbeat's error closed the connection.
	case <-time.After(2 * time.Second):
		t.Fatal("Connect did not return after OnHeartbeat returned an error")
	}

	h.mu.Lock()
	calls := h.hbCalls
	h.mu.Unlock()
	if calls == 0 {
		t.Error("OnHeartbeat was never called")
	}
}
