// Package ws implements the WebSocket client that connects mkonnect to the Bridge Gateway.
package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// PluginHandler processes an inbound DataMsg and returns a status code and body.
type PluginHandler func(proto.DataMsg) (statusCode int, body []byte, err error)

// Client connects mkonnect to the Bridge Gateway over WebSocket.
type Client struct {
	cfg           *config.Config
	keyStore      *auth.KeyStore
	pluginHandler PluginHandler
}

// NewClient creates a new Client.
func NewClient(cfg *config.Config, keyStore *auth.KeyStore) *Client {
	return &Client{
		cfg:      cfg,
		keyStore: keyStore,
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
	httpURL := strings.Replace(wsURL, "ws://", "http://", 1)
	httpURL = strings.Replace(httpURL, "wss://", "https://", 1)
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
	if err := wsjson.Write(ctx, conn, hello); err != nil {
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
func (c *Client) reconnect(ctx context.Context, conn *websocket.Conn, priv *mldsa65.PrivateKey) error {
	// The gateway sends a ChallengeMsg first.
	var raw json.RawMessage
	if err := wsjson.Read(ctx, conn, &raw); err != nil {
		return fmt.Errorf("read challenge: %w", err)
	}

	var typed proto.TypedMessage
	if err := json.Unmarshal(raw, &typed); err != nil {
		return fmt.Errorf("parse message type: %w", err)
	}
	if typed.Type == "error" {
		var msg proto.ErrorMsg
		_ = json.Unmarshal(raw, &msg)
		return fmt.Errorf("gateway error %s: %s", msg.Code, msg.Message)
	}
	if typed.Type != "challenge" {
		return fmt.Errorf("expected challenge, got %q", typed.Type)
	}

	var challenge proto.ChallengeMsg
	if err := json.Unmarshal(raw, &challenge); err != nil {
		return fmt.Errorf("parse challenge: %w", err)
	}

	sig := auth.Sign(priv, challenge.Nonce, c.cfg.ConnectorID)

	hello := proto.HelloMsg{
		Type:            "hello",
		ConnectorID:     c.cfg.ConnectorID,
		ProtocolVersion: c.cfg.ProtocolVersion,
		Signature:       sig,
	}
	if err := wsjson.Write(ctx, conn, hello); err != nil {
		return fmt.Errorf("send hello with signature: %w", err)
	}

	// Read the handshake_ok acknowledgement.
	var raw2 json.RawMessage
	if err := wsjson.Read(ctx, conn, &raw2); err != nil {
		return fmt.Errorf("read reconnect ack: %w", err)
	}
	var typed2 proto.TypedMessage
	if err := json.Unmarshal(raw2, &typed2); err != nil {
		return fmt.Errorf("parse reconnect ack type: %w", err)
	}
	if typed2.Type == "error" {
		var msg proto.ErrorMsg
		_ = json.Unmarshal(raw2, &msg)
		return fmt.Errorf("gateway error %s: %s", msg.Code, msg.Message)
	}
	if typed2.Type != "handshake_ok" {
		return fmt.Errorf("expected handshake_ok, got %q", typed2.Type)
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
			go c.handleData(ctx, conn, msg)
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
		statusCode, body, err = c.pluginHandler(msg)
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
	if err := wsjson.Write(ctx, conn, resp); err != nil {
		slog.Warn("failed to send response", "request_id", msg.RequestID, "err", err)
	}
}
