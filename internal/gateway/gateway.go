// Package gateway implements the reusable Bridge Gateway client shared by every
// connector kind: connection, enrollment, the HELLO handshake, heartbeat, reconnect
// with backoff, and dispatch of inbound messages to a pluggable Handler.
//
// The gateway owns the connection and the protocol lifecycle. A Handler owns what a
// particular connector kind does with it (the mkonnect API connector, the git-sync job).
// The package has no knowledge of connector kinds — the Bridge Gateway enforces the kind
// server-side from the registration token claim.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/proto"
	"github.com/memento-knowledge/mkonnect/internal/version"
)

// Config holds the enrollment inputs common to every connector kind.
type Config struct {
	GatewayURL        string
	ConnectorID       string
	ProtocolVersion   string
	RegistrationToken string // used only on first run to register
}

// Sender writes a message to the gateway, serialised against the concurrent writes
// coder/websocket does not allow. A Handler is given a Sender and never touches the
// raw connection, so every write on a connection shares one mutex.
type Sender interface {
	Send(ctx context.Context, v any) error
}

// HandshakeInfo carries flags from a successful handshake for the Handler to act on.
type HandshakeInfo struct {
	CriticalPatch bool
}

// Handler receives the lifecycle callbacks a connector kind implements.
type Handler interface {
	// OnHandshake runs synchronously right after a successful handshake, before
	// OnConnected and the read loop, so the connector can set state (for example a
	// critical-patch gate) before any message is dispatched.
	OnHandshake(info HandshakeInfo)

	// OnConnected runs once after the handshake, before the heartbeat and read loop.
	// ctx is cancelled when the connection ends, so the Handler may start
	// connection-lifetime goroutines on it; it must bound its own writes.
	OnConnected(ctx context.Context, s Sender)

	// OnHeartbeat runs on each heartbeat tick after the ping and health message have
	// succeeded, on the tick's 10s-bounded ctx. Returning an error closes the
	// connection, exactly as a failed health write does.
	OnHeartbeat(ctx context.Context, s Sender) error

	// HandleMessage dispatches one inbound message the gateway does not consume
	// itself. The gateway consumes challenge, handshake_* and going_away.
	HandleMessage(ctx context.Context, s Sender, typ string, raw json.RawMessage)

	// OnDraining runs on a going_away message before the connection is torn down.
	OnDraining(ctx context.Context)
}

// handshakeTimeout mirrors the gateway's own handshakeTimeout (registration.go) — the gateway
// gives up on a stalled handshake after 30s, so a connector should too rather than blocking
// Connect (and therefore Run's reconnect backoff) forever on a stuck read.
const handshakeTimeout = 30 * time.Second

// dialTimeout bounds a single websocket.Dial attempt (TCP connect, TLS, and the HTTP upgrade
// request/response). Without it the dial runs on Run's process-lifetime context, so a dial that
// stalls mid-upgrade — e.g. landing on a not-yet-ready gateway pod during a rolling update, where
// the TCP connection is accepted but the upgrade response never arrives — blocks Connect, and thus
// Run's reconnect backoff, forever with no further log output. Bounding it turns such a stuck dial
// into a normal failure that re-enters the backoff loop. It is a var, not a const, so
// export_test.go can shorten it for tests that need to observe a stuck dial timing out.
var dialTimeout = 30 * time.Second

// heartbeatInterval is a var, not a const, so export_test.go can shorten it for tests that
// need to observe a heartbeat without waiting 30s of real time.
var heartbeatInterval = 30 * time.Second

// Client is the reusable gateway connection for one connector kind.
type Client struct {
	cfg      Config
	keyStore *auth.KeyStore
	handler  Handler
	writeMu  sync.Mutex
}

// NewClient creates a gateway client that enrols with the given key store and dispatches
// to handler.
func NewClient(cfg Config, keyStore *auth.KeyStore, handler Handler) *Client {
	return &Client{cfg: cfg, keyStore: keyStore, handler: handler}
}

// connSender binds a connection to the client's write mutex as a Sender.
type connSender struct {
	c    *Client
	conn *websocket.Conn
}

func (s connSender) Send(ctx context.Context, v any) error { return s.c.writeMsg(ctx, s.conn, v) }

// writeMsg serialises v as JSON and writes it to conn under a mutex, preventing concurrent
// writes that coder/websocket does not allow.
func (c *Client) writeMsg(ctx context.Context, conn *websocket.Conn, v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return wsjson.Write(ctx, conn, v)
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
		// A brief jittered pause before reconnecting avoids immediately bouncing back onto
		// a gateway pod that just told us it's draining (going_away): the pod keeps
		// accepting new connections until its own GracefulDrainSecs window elapses, so an
		// instant reconnect can land right back on it and get force-closed again.
		select {
		case <-time.After(reconnectPause()):
		case <-ctx.Done():
			return
		}
	}
}

// reconnectPause returns a small jittered delay (500ms-2s) applied after a clean
// disconnect before reconnecting.
func reconnectPause() time.Duration {
	return 500*time.Millisecond + rand.N(1500*time.Millisecond)
}

// Connect dials the gateway, performs the handshake, starts the heartbeat, and runs the
// message loop. It returns when the connection closes or ctx is cancelled.
func (c *Client) Connect(ctx context.Context) error {
	priv, found, err := c.keyStore.Load()
	if err != nil {
		return fmt.Errorf("load key: %w", err)
	}
	if !found {
		if c.cfg.RegistrationToken == "" {
			// Checked before dialing: without this, HelloMsg would carry neither a token nor a
			// signature; the gateway would take the reconnect path and reject with "connector
			// not registered" — a confusing error for what's actually a missing
			// REGISTRATION_TOKEN, and one a misconfigured connector would redial the gateway
			// for on every backoff cycle just to receive.
			return fmt.Errorf("no saved key and REGISTRATION_TOKEN is not set — cannot register or reconnect")
		}
		// Pre-flight, before dialing: registration makes the gateway consume the one-time
		// token and issue the private key, so if we can't persist it we would burn the
		// token and every retry would then fail with "token already consumed". Verify the
		// key directory is writable first and fail with a clear, actionable error instead.
		if err := c.keyStore.EnsureWritable(); err != nil {
			return fmt.Errorf("cannot register: %w", err)
		}
	}

	wsURL := strings.TrimRight(c.cfg.GatewayURL, "/")

	// Bound the dial with its own timeout so a stuck upgrade cannot block Run's reconnect
	// loop forever (see dialTimeout). coder/websocket uses this context only for the dial and
	// handshake, not for the returned connection, so it is cancelled as soon as Dial returns.
	dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{},
	})
	cancelDial()
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.CloseNow() //nolint:errcheck
	// Allow messages up to 64 MiB (32 MiB body + ~33% base64/JSON overhead).
	conn.SetReadLimit(64 << 20)

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

	// connCtx is a child of ctx so connection-lifetime goroutines (the heartbeat, and any the
	// Handler starts in OnConnected) cannot outlive this Connect call.
	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()
	sender := connSender{c: c, conn: conn}

	// Handler's connection-time setup (e.g. the connector's initial connection_status write
	// and its status-push goroutine) runs before the heartbeat and read loop.
	c.handler.OnConnected(connCtx, sender)

	// Heartbeat: a WebSocket-level ping (waits for a pong; this is what actually detects a
	// half-open/zombie TCP connection the gateway has stopped reading from — e.g. router.go's
	// SQS-init-failure path returns without ever reading again, so only a pong-requiring ping
	// notices) AND an application-level health message (the only thing the gateway's Route()
	// loop actually reads to refresh last_health_at — a ping/pong frame is invisible to it).
	// After both succeed, the Handler's OnHeartbeat runs on the same tick. Any failure closes
	// the connection.
	hbInterval := heartbeatInterval // snapshot before goroutine; avoids a race with the test seam
	go func() {
		ticker := time.NewTicker(hbInterval)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-ticker.C:
				tickCtx, cancel := context.WithTimeout(connCtx, 10*time.Second)
				pingErr := conn.Ping(tickCtx)
				var healthErr error
				if pingErr == nil {
					healthErr = c.writeMsg(tickCtx, conn, proto.HealthMsg{Type: "health"})
				}
				var handlerErr error
				if pingErr == nil && healthErr == nil {
					handlerErr = c.handler.OnHeartbeat(tickCtx, sender)
				}
				cancel()
				if pingErr != nil || healthErr != nil || handlerErr != nil {
					slog.Warn("heartbeat failed, closing connection",
						"ping_err", pingErr, "health_err", healthErr, "handler_err", handlerErr)
					conn.CloseNow() //nolint:errcheck
					return
				}
			}
		}
	}()

	return c.runLoop(ctx, conn, sender)
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
		c.handler.OnHandshake(HandshakeInfo{CriticalPatch: msg.CriticalPatch})
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

// runLoop reads inbound gateway messages. It consumes the control messages (going_away)
// and delegates every other message to the Handler.
func (c *Client) runLoop(ctx context.Context, conn *websocket.Conn, sender Sender) error {
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

		if typed.Type == "going_away" {
			slog.Info("gateway is draining for a rolling update, waiting for in-flight requests before disconnecting")
			c.handler.OnDraining(ctx)
			return nil
		}
		c.handler.HandleMessage(ctx, sender, typed.Type, raw)
	}
}
