// Package ws implements the WebSocket client that connects mkonnect to the Bridge Gateway.
package ws

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/httpclient"
	"github.com/memento-knowledge/mkonnect/internal/plugin"
	"github.com/memento-knowledge/mkonnect/internal/proto"
	"github.com/memento-knowledge/mkonnect/internal/version"
)

// Client connects mkonnect to the Bridge Gateway over WebSocket.
type Client struct {
	cfg                   *config.Config
	keyStore              *auth.KeyStore
	writeMu               sync.Mutex
	workerSem             chan struct{}
	inFlight              sync.WaitGroup
	criticalPatchRequired atomic.Bool
	httpHandler           plugin.HTTPPluginHandler
	credsStore            *creds.Store
	pluginReg             *plugin.Registry
	statusCache           map[string]proto.PluginStatus
	statusMu              sync.RWMutex
	statusCh              chan struct{}
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
		cfg:         cfg,
		keyStore:    keyStore,
		workerSem:   make(chan struct{}, 4), // 4 workers × 32 MiB max body ≈ 128 MiB, matching the default container memory limit
		statusCache: make(map[string]proto.PluginStatus),
		statusCh:    make(chan struct{}, 1),
	}
}

// SetHTTPPluginHandler sets the handler for inbound http_request messages.
func (c *Client) SetHTTPPluginHandler(fn plugin.HTTPPluginHandler) {
	c.httpHandler = fn
}

// SetCredsStore provides the credential store used for connection_status and connectivity tests.
func (c *Client) SetCredsStore(s *creds.Store) {
	c.credsStore = s
}

// SetPluginRegistry provides the registry used to enumerate known plugins for connection_status.
func (c *Client) SetPluginRegistry(r *plugin.Registry) {
	c.pluginReg = r
}

// SignalStatusPush queues an immediate connection_status push to the gateway.
// Safe to call from any goroutine; non-blocking (drops if already pending).
func (c *Client) SignalStatusPush() {
	select {
	case c.statusCh <- struct{}{}:
	default:
	}
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

// handshakeTimeout mirrors the gateway's own handshakeTimeout (registration.go) — the gateway
// gives up on a stalled handshake after 30s, so mkonnect should too rather than blocking Connect
// (and therefore Run's reconnect backoff) forever on a stuck read.
const handshakeTimeout = 30 * time.Second

// dialTimeout bounds a single websocket.Dial attempt (TCP connect, TLS, and the HTTP upgrade
// request/response). Without it the dial runs on Run's process-lifetime context, so a dial that
// stalls mid-upgrade — e.g. landing on a not-yet-ready gateway pod during a rolling update, where
// the TCP connection is accepted but the upgrade response never arrives — blocks Connect, and thus
// Run's reconnect backoff, forever with no further log output. Bounding it turns such a stuck dial
// into a normal failure that re-enters the backoff loop. A var so tests can shorten it.
var dialTimeout = 30 * time.Second

// heartbeatInterval is a var, not a const, so export_test.go can shorten it for tests that
// need to observe a heartbeat without waiting 30s of real time.
var heartbeatInterval = 30 * time.Second

// drainTimeout bounds how long a going_away disconnect waits for in-flight request
// goroutines to finish and write their responses before the connection is torn down.
const drainTimeout = 10 * time.Second

// Connect dials the gateway, performs the handshake, and runs the message loop.
// It returns when the connection closes or ctx is cancelled.
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

	// Bound this first post-handshake write: it runs before the heartbeat goroutine (below)
	// exists to notice and tear down a wedged connection, so on the unbounded parent ctx a
	// stalled write here would hang Connect with no backstop.
	statusCtx, cancelStatus := context.WithTimeout(ctx, handshakeTimeout)
	c.emitConnectionStatus(statusCtx, conn)
	cancelStatus()

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
	hbInterval := heartbeatInterval // snapshot before goroutine; avoids data race with SetHeartbeatIntervalForTest
	go func() {
		ticker := time.NewTicker(hbInterval)
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
				var statusErr error
				if pingErr == nil && healthErr == nil {
					statusErr = c.writeMsg(tickCtx, conn, c.buildConnectionStatus())
				}
				cancel()
				if pingErr != nil || healthErr != nil || statusErr != nil {
					slog.Warn("heartbeat failed, closing connection",
						"ping_err", pingErr, "health_err", healthErr, "status_err", statusErr)
					conn.CloseNow() //nolint:errcheck
					return
				}
			}
		}
	}()

	go func() {
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-c.statusCh:
				pushCtx, cancel := context.WithTimeout(heartbeatCtx, 10*time.Second)
				c.emitConnectionStatus(pushCtx, conn)
				cancel()
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

// runLoop reads inbound gateway messages and dispatches them to the appropriate handler.
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
			slog.Info("gateway is draining for a rolling update, waiting for in-flight requests before disconnecting")
			c.waitForInFlight(drainTimeout)
			return nil
		case "http_request":
			var msg proto.HTTPRequestMsg
			if err := json.Unmarshal(raw, &msg); err != nil {
				slog.Warn("malformed http_request message", "err", err)
				continue
			}
			select {
			case c.workerSem <- struct{}{}:
			default:
				s := `{"error":"too many concurrent requests"}`
				if err := c.writeMsg(ctx, conn, proto.HTTPResponseMsg{
					Type: "http_response", RequestID: msg.RequestID, ResponderID: msg.ResponderID,
					StatusCode: 429, Body: &s,
				}); err != nil {
					slog.Warn("failed to send 429 http_response", "request_id", msg.RequestID, "err", err)
				}
				continue
			}
			c.inFlight.Add(1)
			go func(m proto.HTTPRequestMsg) {
				defer c.inFlight.Done()
				defer func() { <-c.workerSem }()
				c.handleHTTPRequest(ctx, conn, m)
			}(msg)
		case "status_request":
			// Handled inline (no goroutine): building status is a cache read and the write
			// is already serialized by writeMu, so spawning a goroutine per request would
			// only add an unbounded-goroutine vector for a flood of status_requests. The
			// 10s bound keeps a stuck write from stalling the read loop (and thus delaying
			// going_away handling) indefinitely — the heartbeat ping is the backstop.
			sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			c.emitConnectionStatus(sctx, conn)
			cancel()

		case "test_connection":
			var msg proto.TestConnectionMsg
			if err := json.Unmarshal(raw, &msg); err != nil {
				slog.Warn("malformed test_connection", "err", err)
				continue
			}
			// Use the same semaphore as http_request so probes cannot create unbounded
			// goroutines that make outbound HTTP calls to the customer's internal network.
			select {
			case c.workerSem <- struct{}{}:
			default:
				slog.Warn("test_connection dropped: too many concurrent requests", "provider_key", msg.ProviderKey)
				continue
			}
			go func(key string) {
				defer func() { <-c.workerSem }()
				probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				result := c.probePlugin(probeCtx, key)
				c.updatePluginStatus(result)
				if err := c.writeMsg(ctx, conn, result); err != nil {
					slog.Warn("failed to send test_result", "err", err)
				}
				c.SignalStatusPush()
			}(msg.ProviderKey)

		default:
			slog.Debug("ignoring message", "type", typed.Type)
		}
	}
}

// probePlugin performs a real HTTP connectivity test for the given plugin.
func (c *Client) probePlugin(ctx context.Context, providerKey string) proto.TestResultMsg {
	result := proto.TestResultMsg{
		Type:        "test_result",
		ProviderKey: providerKey,
	}

	// Resolve base URL: creds store first, then registry (mirrors HTTPHandler).
	var baseURL string
	var credential creds.Credential
	var haveCredential bool
	if c.credsStore != nil {
		if cred, ok := c.credsStore.Get(providerKey); ok {
			baseURL = cred.BaseURL
			credential = cred
			haveCredential = true
		}
	}
	if baseURL == "" && c.pluginReg != nil {
		baseURL, _ = c.pluginReg.Get(providerKey)
	}
	if baseURL == "" {
		result.Status = "unreachable"
		result.Diagnostic = "not configured"
		return result
	}
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		result.Status = "unreachable"
		result.Diagnostic = "invalid base URL"
		return result
	}

	probeURL := strings.TrimRight(base.String(), "/") + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, probeURL, nil)
	if err != nil {
		result.Status = "unreachable"
		result.Diagnostic = safeDiagnostic(err)
		return result
	}
	useDirectTransport := haveCredential && credential.HasAuthorization()
	if useDirectTransport {
		credential.ApplyAuthorization(req)
	}

	// Do not follow redirects: a redirect to an auth page (e.g. SSO) would succeed
	// with a 200 and hide an auth_failure, and following redirects could forward the
	// local credentials to a third-party host.
	probeClient := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if useDirectTransport {
		probeClient.Transport = httpclient.NewDirectTransport()
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || os.IsTimeout(err) {
			result.Status = "timeout"
		} else {
			result.Status = "unreachable"
		}
		result.Diagnostic = safeDiagnostic(err)
		return result
	}
	defer resp.Body.Close() //nolint:errcheck

	code := resp.StatusCode
	switch {
	case code == 401 || code == 403:
		result.Status = "auth_failure"
		result.Diagnostic = fmt.Sprintf("HTTP %d", code)
	case code >= 200 && code < 400 || code == 404 || code == 405:
		// 404: server answered (plugin is reachable, path just doesn't exist at root)
		// 405: server answered HEAD but doesn't allow it — still reachable
		result.Status = "connected"
	default:
		result.Status = "unreachable"
		result.Diagnostic = fmt.Sprintf("HTTP %d", code)
	}
	return result
}

// safeDiagnostic maps a probe error to a short, address-free description safe to send to
// the platform. Raw net/http error strings embed the target host:port (and DNS errors the
// hostname), so returning err.Error() verbatim would leak the customer's internal network
// topology to Memento's cloud via test_result/connection_status — directly against the
// connector's premise that internal details never leave the network. We surface only the
// failure class. The status field (connected/auth_failure/unreachable/timeout) already
// carries the actionable signal.
func safeDiagnostic(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return "connection timed out"
	}
	// DNS errors' Error() includes the hostname being resolved — never surface it.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS resolution failed"
	}
	// TLS verification failures are common with internal/self-signed certs. Report the
	// class only — the wrapped x509 error can embed the hostname.
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "TLS certificate verification failed"
	}
	// os.SyscallError (e.g. connect: ECONNREFUSED) stringifies to the errno text alone
	// ("connection refused", "no route to host") with no address.
	var syscallErr *os.SyscallError
	if errors.As(err, &syscallErr) && syscallErr.Err != nil {
		return syscallErr.Err.Error()
	}
	// Anything else — including *net.OpError and *net.AddrError, whose Error()/wrapped
	// cause can embed the target address or hostname — collapses to a fixed class. The
	// address-free causes worth surfacing (errno, DNS, timeout, TLS) are all matched above.
	return "connection failed"
}

// updatePluginStatus maps a TestResultMsg to a PluginStatus and stores it in the cache.
func (c *Client) updatePluginStatus(result proto.TestResultMsg) {
	var status string
	switch result.Status {
	case "connected":
		status = "configured_connected"
	case "auth_failure", "unreachable", "timeout":
		status = "configured_error"
	default:
		status = "not_configured"
	}
	ps := proto.PluginStatus{
		ProviderKey:  result.ProviderKey,
		Status:       status,
		LastTestedAt: time.Now().UTC().Format(time.RFC3339),
		ErrorDetail:  result.Diagnostic,
	}
	c.statusMu.Lock()
	c.statusCache[result.ProviderKey] = ps
	c.statusMu.Unlock()
}

// allPluginKeys returns the union of plugin names from the registry and the creds store.
func (c *Client) allPluginKeys() []string {
	seen := make(map[string]struct{})
	var keys []string
	if c.pluginReg != nil {
		for _, k := range c.pluginReg.Plugins() {
			if _, ok := seen[k]; !ok {
				seen[k] = struct{}{}
				keys = append(keys, k)
			}
		}
	}
	if c.credsStore != nil {
		for k := range c.credsStore.List() {
			if _, ok := seen[k]; !ok {
				seen[k] = struct{}{}
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// cachedPluginStatus returns the cached probe result for a plugin, or a conservative
// "not_configured" initial state if no probe has run yet. The platform should send
// test_connection to obtain the real connectivity status.
func (c *Client) cachedPluginStatus(key string) proto.PluginStatus {
	c.statusMu.RLock()
	s, ok := c.statusCache[key]
	c.statusMu.RUnlock()
	if ok {
		return s
	}
	return proto.PluginStatus{ProviderKey: key, Status: "not_configured"}
}

// buildConnectionStatus assembles the current connection_status message.
func (c *Client) buildConnectionStatus() proto.ConnectionStatusMsg {
	keys := c.allPluginKeys()
	plugins := make([]proto.PluginStatus, 0, len(keys))
	for _, k := range keys {
		plugins = append(plugins, c.cachedPluginStatus(k))
	}
	return proto.ConnectionStatusMsg{Type: "connection_status", Plugins: plugins}
}

// emitConnectionStatus writes the current connection_status to conn.
func (c *Client) emitConnectionStatus(ctx context.Context, conn *websocket.Conn) {
	if err := c.writeMsg(ctx, conn, c.buildConnectionStatus()); err != nil {
		slog.Warn("failed to send connection_status", "err", err)
	}
}

// waitForInFlight blocks until all in-flight request goroutines finish (so their
// responses get written while conn is still open) or timeout elapses, whichever comes
// first — a going_away drain should not hang indefinitely on a stuck downstream call.
func (c *Client) waitForInFlight(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		c.inFlight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("timed out waiting for in-flight requests to finish before drain disconnect", "timeout", timeout)
	}
}

func (c *Client) handleHTTPRequest(ctx context.Context, conn *websocket.Conn, msg proto.HTTPRequestMsg) {
	strPtr := func(s string) *string { return &s }

	if c.criticalPatchRequired.Load() {
		resp := proto.HTTPResponseMsg{
			Type: "http_response", RequestID: msg.RequestID, ResponderID: msg.ResponderID,
			StatusCode: 503, Body: strPtr(`{"error":"connector has suspended tool dispatch pending a critical security update"}`),
		}
		if err := c.writeMsg(ctx, conn, resp); err != nil {
			slog.Warn("failed to send critical-patch 503", "request_id", msg.RequestID, "err", err)
		}
		return
	}

	var statusCode int
	var headers map[string]string
	var body *string

	if c.httpHandler != nil {
		var err error
		statusCode, headers, body, err = c.httpHandler(ctx, msg)
		if err != nil {
			slog.Warn("http_request handler error", "request_id", msg.RequestID, "err", err)
			if statusCode == 0 {
				statusCode = 500
			}
		}
	} else {
		statusCode = 501
		body = strPtr(`{"error":"no http handler configured"}`)
	}

	resp := proto.HTTPResponseMsg{
		Type:        "http_response",
		RequestID:   msg.RequestID,
		ResponderID: msg.ResponderID,
		StatusCode:  statusCode,
		Headers:     headers,
		Body:        body,
	}
	if err := c.writeMsg(ctx, conn, resp); err != nil {
		slog.Warn("failed to send http_response", "request_id", msg.RequestID, "err", err)
	}
}
