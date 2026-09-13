// Package ws implements the WebSocket client that connects mkonnect to the Bridge Gateway.
package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/proto"
	"github.com/memento-knowledge/mkonnect/internal/version"
)

// PluginHandler processes an inbound DataMsg and returns a status code and body.
type PluginHandler func(context.Context, proto.DataMsg) (statusCode int, body []byte, err error)

// Client connects mkonnect to the Bridge Gateway over WebSocket.
type Client struct {
	cfg                   *config.Config
	keyStore              *auth.KeyStore
	pluginHandler         PluginHandler
	writeMu               sync.Mutex
	workerSem             chan struct{}
	criticalPatchRequired atomic.Bool
}

// writeMsg serialises v as JSON and writes it to conn under a mutex,
// preventing concurrent writes that coder/websocket does not allow.
func (c *Client) writeMsg(ctx context.Context, conn *websocket.Conn, v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return wsjson.Write(ctx, conn, v)
}

// NewClient creates a new Client.
func NewClient(cfg *config.Config, keyStore *auth.KeyStore) *Client {
	return &Client{
		cfg:       cfg,
		keyStore:  keyStore,
		workerSem: make(chan struct{}, 4), // 4 workers × 32 MiB max body ≈ 128 MiB, matching the default container memory limit
	}
}

// SetPluginHandler sets the function that handles inbound DataMsg requests.
func (c *Client) SetPluginHandler(fn PluginHandler) {
	c.pluginHandler = fn
}

// Run connects to the gateway and maintains the connection with exponential backoff.
// It blocks until ctx is cancelled.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 60 * time.Second

	for {
		if err := c.Connect(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("gateway connection failed", "err", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		// Clean disconnect — reset backoff
		backoff = time.Second
		if ctx.Err() != nil {
			return
		}
	}
}

// handshakeTimeout mirrors the gateway's own handshakeTimeout (registration.go) — the gateway
// gives up on a stalled handshake after 30s, so mkonnect should too rather than blocking Connect
// (and therefore Run's reconnect backoff) forever on a stuck read.
const handshakeTimeout = 30 * time.Second

// Connect dials the gateway, performs the handshake, and runs the message loop.
// It returns when the connection closes or ctx is cancelled.
func (c *Client) Connect(ctx context.Context) error {
	wsURL := strings.TrimRight(c.cfg.GatewayURL, "/")

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{},
	})
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.CloseNow() //nolint:errcheck
	// Allow messages up to 64 MiB (32 MiB body + ~33% base64/JSON overhead).
	conn.SetReadLimit(64 << 20)

	priv, found, err := c.keyStore.Load()
	if err != nil {
		return fmt.Errorf("load key: %w", err)
	}
	if !found && c.cfg.RegistrationToken == "" {
		// Without this check, HelloMsg would carry neither a token nor a signature; the
		// gateway would take the reconnect path and reject with "connector not registered" —
		// a confusing error for what's actually a missing REGISTRATION_TOKEN.
		return fmt.Errorf("no saved key and REGISTRATION_TOKEN is not set — cannot register or reconnect")
	}

	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, handshakeTimeout)
	defer cancelHandshake()

	// The gateway always sends the challenge first, before reading HELLO — for both
	// first-run registration (which ignores it, using the registration token instead)
	// and reconnect (which signs it). See handler.go's Handle() in bridge-gateway.
	challenge, err := c.readChallenge(handshakeCtx, conn)
	if err != nil {
		return fmt.Errorf("read challenge: %w", err)
	}

	if !found {
		if err := c.register(handshakeCtx, conn); err != nil {
			return fmt.Errorf("registration: %w", err)
		}
	} else {
		if err := c.reconnect(handshakeCtx, conn, priv, challenge); err != nil {
			return fmt.Errorf("reconnect: %w", err)
		}
	}

	// Heartbeat: a WebSocket-level ping (waits for a pong; this is what actually detects a
	// half-open/zombie TCP connection the gateway has stopped reading from — e.g. router.go's
	// SQS-init-failure path returns without ever reading again, so only a pong-requiring ping
	// notices) AND an application-level health message (the only thing the gateway's Route()
	// loop actually reads to refresh last_health_at — a ping/pong frame is invisible to it).
	// Both run on the same 30s tick; either failing closes the connection. heartbeatCtx is a
	// child of ctx so the goroutine can't outlive this Connect call (e.g. after runLoop returns
	// due to a read error unrelated to ctx cancellation).
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				tickCtx, cancel := context.WithTimeout(heartbeatCtx, 10*time.Second)
				pingErr := conn.Ping(tickCtx)
				var healthErr error
				if pingErr == nil {
					healthErr = c.writeMsg(tickCtx, conn, proto.HealthMsg{Type: "health"})
				}
				cancel()
				if pingErr != nil || healthErr != nil {
					slog.Warn("heartbeat failed, closing connection", "ping_err", pingErr, "health_err", healthErr)
					conn.CloseNow() //nolint:errcheck
					return
				}
			}
		}
	}()

	return c.runLoop(ctx, conn)
}

// readChallenge reads the challenge frame the gateway sends unconditionally as the first
// message on every connection attempt.
func (c *Client) readChallenge(ctx context.Context, conn *websocket.Conn) (proto.Nonce, error) {
	var raw json.RawMessage
	if err := wsjson.Read(ctx, conn, &raw); err != nil {
		return proto.Nonce{}, fmt.Errorf("read: %w", err)
	}
	var tm proto.TypedMessage
	if err := json.Unmarshal(raw, &tm); err != nil {
		return proto.Nonce{}, fmt.Errorf("unmarshal type: %w", err)
	}
	if tm.Type != "challenge" {
		return proto.Nonce{}, fmt.Errorf("expected challenge, got %q", tm.Type)
	}
	var challenge proto.ChallengeMsg
	if err := json.Unmarshal(raw, &challenge); err != nil {
		return proto.Nonce{}, fmt.Errorf("unmarshal challenge: %w", err)
	}
	return challenge.Challenge, nil
}

// register performs the first-run flow: send HelloMsg with the registration token,
// receive handshake_ok, and persist the private key the gateway generated for us.
func (c *Client) register(ctx context.Context, conn *websocket.Conn) error {
	hello := proto.HelloMsg{
		Type:              "hello",
		ConnectorID:       c.cfg.ConnectorID,
		ProtocolVersion:   c.cfg.ProtocolVersion,
		ConnectorVersion:  version.Version,
		RegistrationToken: c.cfg.RegistrationToken,
	}
	if err := c.writeMsg(ctx, conn, hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	return c.readHandshakeResult(ctx, conn, true)
}

// reconnect performs the challenge-response flow for an already-registered connector,
// signing the challenge the caller already read via readChallenge.
func (c *Client) reconnect(ctx context.Context, conn *websocket.Conn, privKey *mldsa65.PrivateKey, challenge proto.Nonce) error {
	sig := auth.Sign(privKey, challenge, c.cfg.ConnectorID)
	hello := proto.HelloMsg{
		Type:             "hello",
		ConnectorID:      c.cfg.ConnectorID,
		ProtocolVersion:  c.cfg.ProtocolVersion,
		ConnectorVersion: version.Version,
		Signature:        sig,
	}
	if err := c.writeMsg(ctx, conn, hello); err != nil {
		return fmt.Errorf("send signed hello: %w", err)
	}
	return c.readHandshakeResult(ctx, conn, false)
}

// readHandshakeResult reads the gateway's response to HELLO — handshake_ok or
// handshake_error — common to both registration and reconnect. savePrivateKey is true
// only for registration, where the gateway includes a freshly generated private key.
func (c *Client) readHandshakeResult(ctx context.Context, conn *websocket.Conn, savePrivateKey bool) error {
	var raw json.RawMessage
	if err := wsjson.Read(ctx, conn, &raw); err != nil {
		return fmt.Errorf("read handshake response: %w", err)
	}

	var typed proto.TypedMessage
	if err := json.Unmarshal(raw, &typed); err != nil {
		return fmt.Errorf("parse message type: %w", err)
	}

	switch typed.Type {
	case "handshake_ok":
		var msg proto.HandshakeOkMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			return fmt.Errorf("parse handshake_ok: %w", err)
		}
		if msg.DeprecationNotice != "" {
			slog.Warn("gateway reports this connector's protocol_version is deprecated", "notice", msg.DeprecationNotice)
		}
		c.criticalPatchRequired.Store(msg.CriticalPatch)
		if msg.CriticalPatch {
			slog.Warn("gateway requires a critical security update — suspending new tool dispatch")
		}
		if savePrivateKey {
			priv, err := auth.UnmarshalPrivateKey(msg.PrivateKey)
			if err != nil {
				return fmt.Errorf("unmarshal private key: %w", err)
			}
			if err := c.keyStore.Save(priv); err != nil {
				return fmt.Errorf("save private key: %w", err)
			}
		}
		return nil
	case "handshake_error":
		var msg proto.HandshakeErrorMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			return fmt.Errorf("parse handshake_error: %w", err)
		}
		return fmt.Errorf("gateway rejected handshake (%d): %s", msg.Code, msg.Reason)
	default:
		return fmt.Errorf("unexpected message type during handshake: %q", typed.Type)
	}
}

// runLoop reads DataMsg messages and dispatches them to the plugin handler.
func (c *Client) runLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		var raw json.RawMessage
		if err := wsjson.Read(ctx, conn, &raw); err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown; backoff resets in Run
			}
			return fmt.Errorf("read: %w", err)
		}

		var typed proto.TypedMessage
		if err := json.Unmarshal(raw, &typed); err != nil {
			slog.Warn("unreadable message", "err", err)
			continue
		}

		switch typed.Type {
		case "going_away":
			slog.Info("gateway is draining for a rolling update, disconnecting to reconnect elsewhere")
			return nil
		case "data":
			var msg proto.DataMsg
			if err := json.Unmarshal(raw, &msg); err != nil {
				slog.Warn("malformed data message", "err", err)
				continue
			}
			select {
			case c.workerSem <- struct{}{}:
			default:
				// Backpressure: write 429 synchronously. Spawning a goroutine here
				// would create unbounded goroutines under load, defeating the semaphore.
				if err := c.writeMsg(ctx, conn, proto.ResponseMsg{
					Type: "response", RequestID: msg.RequestID,
					StatusCode: 429, Body: []byte(`{"error":"too many concurrent requests"}`),
				}); err != nil {
					slog.Warn("failed to send 429", "request_id", msg.RequestID, "err", err)
				}
				continue
			}
			go func(m proto.DataMsg) {
				defer func() { <-c.workerSem }()
				c.handleData(ctx, conn, m)
			}(msg)
		default:
			slog.Debug("ignoring message", "type", typed.Type)
		}
	}
}

func (c *Client) handleData(ctx context.Context, conn *websocket.Conn, msg proto.DataMsg) {
	if c.criticalPatchRequired.Load() {
		// NOTE: this 503 path is presently unreachable against the real gateway — the
		// gateway forwards "data"/DataMsg-shaped traffic nowhere (its inboundAllowlist
		// expects http_response, not response) until #2212 replaces DataMsg/ResponseMsg
		// with http_request/http_response. The flag and gate are still correct and
		// reusable as-is; #2212 should keep this check and just change the response shape
		// to match spec §7.4's tool_unavailable.
		resp := proto.ResponseMsg{
			Type: "response", RequestID: msg.RequestID,
			StatusCode: 503, Body: []byte(`{"error":"connector has suspended tool dispatch pending a critical security update"}`),
		}
		if err := c.writeMsg(ctx, conn, resp); err != nil {
			slog.Warn("failed to send critical-patch 503", "request_id", msg.RequestID, "err", err)
		}
		return
	}

	var statusCode int
	var body []byte

	if c.pluginHandler != nil {
		var err error
		statusCode, body, err = c.pluginHandler(ctx, msg)
		if err != nil {
			slog.Warn("plugin handler error", "request_id", msg.RequestID, "err", err)
			if statusCode == 0 {
				statusCode = 500
			}
		}
	} else {
		statusCode = 501
		body = []byte(`{"error":"no plugin handler configured"}`)
	}

	resp := proto.ResponseMsg{
		Type:       "response",
		RequestID:  msg.RequestID,
		StatusCode: statusCode,
		Body:       body,
	}
	if err := c.writeMsg(ctx, conn, resp); err != nil {
		slog.Warn("failed to send response", "request_id", msg.RequestID, "err", err)
	}
}
