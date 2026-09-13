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
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// PluginHandler processes an inbound DataMsg and returns a status code and body.
type PluginHandler func(context.Context, proto.DataMsg) (statusCode int, body []byte, err error)

// Client connects mkonnect to the Bridge Gateway over WebSocket.
type Client struct {
	cfg           *config.Config
	keyStore      *auth.KeyStore
	pluginHandler PluginHandler
	writeMu       sync.Mutex
	workerSem     chan struct{}
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
		workerSem: make(chan struct{}, 32), // max 32 concurrent plugin requests
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

// Connect dials the gateway, performs the handshake, and runs the message loop.
// It returns when the connection closes or ctx is cancelled.
func (c *Client) Connect(ctx context.Context) error {
	wsURL := strings.TrimRight(c.cfg.GatewayURL, "/")
	// Convert ws:// → http:// so coder/websocket can dial it correctly.
	httpURL := strings.ReplaceAll(wsURL, "ws://", "http://")
	httpURL = strings.ReplaceAll(httpURL, "wss://", "https://")
	httpURL += "/ws"

	conn, _, err := websocket.Dial(ctx, httpURL, &websocket.DialOptions{
		HTTPHeader: http.Header{},
	})
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.CloseNow() //nolint:errcheck

	priv, found, err := c.keyStore.Load()
	if err != nil {
		return fmt.Errorf("load key: %w", err)
	}

	if !found {
		// First-run registration
		if err := c.register(ctx, conn); err != nil {
			return fmt.Errorf("registration: %w", err)
		}
	} else {
		// Reconnect with challenge-response
		if err := c.reconnect(ctx, conn, priv); err != nil {
			return fmt.Errorf("reconnect: %w", err)
		}
	}

	return c.runLoop(ctx, conn)
}

// register performs the first-run flow: send HelloMsg with token, receive HandshakeOkMsg.
func (c *Client) register(ctx context.Context, conn *websocket.Conn) error {
	hello := proto.HelloMsg{
		Type:            "hello",
		ConnectorID:     c.cfg.ConnectorID,
		ProtocolVersion: c.cfg.ProtocolVersion,
		Token:           c.cfg.RegistrationToken,
	}
	if err := c.writeMsg(ctx, conn, hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

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
		priv, err := auth.UnmarshalPrivateKey(msg.PrivateKey)
		if err != nil {
			return fmt.Errorf("unmarshal private key: %w", err)
		}
		if err := c.keyStore.Save(priv); err != nil {
			return fmt.Errorf("save private key: %w", err)
		}
		return nil
	case "error":
		var msg proto.ErrorMsg
		_ = json.Unmarshal(raw, &msg)
		return fmt.Errorf("gateway error %s: %s", msg.Code, msg.Message)
	default:
		return fmt.Errorf("unexpected message type during registration: %q", typed.Type)
	}
}

// reconnect performs the challenge-response flow for an already-registered connector.
func (c *Client) reconnect(ctx context.Context, conn *websocket.Conn, privKey *mldsa65.PrivateKey) error {
	// Step 1: identify connector so gateway can look up our public key.
	if err := c.writeMsg(ctx, conn, proto.HelloMsg{
		Type:            "hello",
		ConnectorID:     c.cfg.ConnectorID,
		ProtocolVersion: c.cfg.ProtocolVersion,
	}); err != nil {
		return fmt.Errorf("reconnect identify: %w", err)
	}

	// Step 2: receive challenge.
	var raw json.RawMessage
	if err := wsjson.Read(ctx, conn, &raw); err != nil {
		return fmt.Errorf("read challenge: %w", err)
	}
	var tm proto.TypedMessage
	if err := json.Unmarshal(raw, &tm); err != nil {
		return fmt.Errorf("unmarshal challenge type: %w", err)
	}
	if tm.Type == "error" {
		var msg proto.ErrorMsg
		_ = json.Unmarshal(raw, &msg)
		return fmt.Errorf("gateway error %s: %s", msg.Code, msg.Message)
	}
	if tm.Type != "challenge" {
		return fmt.Errorf("expected challenge, got %s", tm.Type)
	}
	var challenge proto.ChallengeMsg
	if err := json.Unmarshal(raw, &challenge); err != nil {
		return fmt.Errorf("unmarshal challenge: %w", err)
	}

	// Step 3: sign and respond.
	sig := auth.Sign(privKey, challenge.Nonce, c.cfg.ConnectorID)
	if err := c.writeMsg(ctx, conn, proto.HelloMsg{
		Type:            "hello",
		ConnectorID:     c.cfg.ConnectorID,
		ProtocolVersion: c.cfg.ProtocolVersion,
		Signature:       sig,
	}); err != nil {
		return fmt.Errorf("send signed hello: %w", err)
	}

	// Step 4: await ack and validate.
	if err := wsjson.Read(ctx, conn, &raw); err != nil {
		return fmt.Errorf("read reconnect ack: %w", err)
	}
	var ack proto.TypedMessage
	if err := json.Unmarshal(raw, &ack); err != nil {
		return fmt.Errorf("unmarshal reconnect ack: %w", err)
	}
	if ack.Type == "error" {
		var errMsg proto.ErrorMsg
		_ = json.Unmarshal(raw, &errMsg)
		return fmt.Errorf("gateway rejected reconnect: %s: %s", errMsg.Code, errMsg.Message)
	}
	if ack.Type != "handshake_ok" {
		return fmt.Errorf("unexpected message type on reconnect ack: %s", ack.Type)
	}
	return nil
}

// runLoop reads DataMsg messages and dispatches them to the plugin handler.
func (c *Client) runLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		var raw json.RawMessage
		if err := wsjson.Read(ctx, conn, &raw); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read: %w", err)
		}

		var typed proto.TypedMessage
		if err := json.Unmarshal(raw, &typed); err != nil {
			slog.Warn("unreadable message", "err", err)
			continue
		}

		switch typed.Type {
		case "data":
			var msg proto.DataMsg
			if err := json.Unmarshal(raw, &msg); err != nil {
				slog.Warn("malformed data message", "err", err)
				continue
			}
			select {
			case c.workerSem <- struct{}{}:
			default:
				// backpressure: drop request and send 429
				go func(m proto.DataMsg) {
					_ = c.writeMsg(ctx, conn, proto.ResponseMsg{
						Type: "response", RequestID: m.RequestID,
						StatusCode: 429, Body: []byte(`{"error":"too many concurrent requests"}`),
					})
				}(msg)
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
