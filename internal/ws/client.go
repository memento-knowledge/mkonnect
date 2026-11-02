// Package ws implements the mkonnect API connector. It is the gateway.Handler that forwards
// provider HTTP calls over the Bridge Gateway: it dispatches http_request, status_request and
// test_connection messages and reports connection_status. The connection lifecycle itself
// (dial, enrollment, handshake, heartbeat, reconnect) lives in internal/gateway, which this
// package drives.
package ws

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/gateway"
	"github.com/memento-knowledge/mkonnect/internal/httpclient"
	"github.com/memento-knowledge/mkonnect/internal/plugin"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// drainTimeout bounds how long a going_away disconnect waits for in-flight request
// goroutines to finish and write their responses before the connection is torn down.
const drainTimeout = 10 * time.Second

// connectionStatusTimeout bounds the initial connection_status write (run before the
// heartbeat exists to tear down a wedged connection) and the status_request response.
const connectionStatusTimeout = 30 * time.Second

// Client is the mkonnect API connector. It implements gateway.Handler and drives a
// gateway.Client for the connection lifecycle.
type Client struct {
	cfg                   *config.Config
	gw                    *gateway.Client
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

// NewClient creates a new API connector that enrols with the given key store.
func NewClient(cfg *config.Config, keyStore *auth.KeyStore) *Client {
	c := &Client{
		cfg:         cfg,
		workerSem:   make(chan struct{}, 4), // 4 workers × 32 MiB max body ≈ 128 MiB, matching the default container memory limit
		statusCache: make(map[string]proto.PluginStatus),
		statusCh:    make(chan struct{}, 1),
	}
	c.gw = gateway.NewClient(gateway.Config{
		GatewayURL:        cfg.GatewayURL,
		ConnectorID:       cfg.ConnectorID,
		ProtocolVersion:   cfg.ProtocolVersion,
		RegistrationToken: cfg.RegistrationToken,
	}, keyStore, c)
	return c
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

// Run connects to the gateway and maintains the connection with exponential backoff.
// It blocks until ctx is cancelled.
func (c *Client) Run(ctx context.Context) {
	c.gw.Run(ctx)
}

// Connect performs a single connection attempt (dial, handshake, heartbeat, message loop)
// and returns when the connection closes or ctx is cancelled. Run wraps it with backoff.
func (c *Client) Connect(ctx context.Context) error {
	return c.gw.Connect(ctx)
}

// SignalStatusPush queues an immediate connection_status push to the gateway.
// Safe to call from any goroutine; non-blocking (drops if already pending). Used after a
// connectivity probe and on SIGHUP (after a credential reload).
func (c *Client) SignalStatusPush() {
	select {
	case c.statusCh <- struct{}{}:
	default:
	}
}

// --- gateway.Handler ---

// OnHandshake records the critical-patch gate before any message is dispatched.
func (c *Client) OnHandshake(info gateway.HandshakeInfo) {
	c.criticalPatchRequired.Store(info.CriticalPatch)
	if info.CriticalPatch {
		slog.Warn("gateway requires a critical security update — suspending new tool dispatch")
	}
}

// OnConnected emits the initial connection_status and starts the status-push goroutine for
// the lifetime of the connection. ctx is cancelled when the connection ends.
func (c *Client) OnConnected(ctx context.Context, s gateway.Sender) {
	// Bound this first write: it runs before the heartbeat exists to notice and tear down a
	// wedged connection, so a stalled write here would otherwise hang until teardown.
	wctx, cancel := context.WithTimeout(ctx, connectionStatusTimeout)
	c.emitConnectionStatus(wctx, s)
	cancel()

	// Drain immediate status-push signals (SignalStatusPush) until the connection ends.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.statusCh:
				pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				c.emitConnectionStatus(pctx, s)
				cancel()
			}
		}
	}()
}

// OnHeartbeat pushes connection_status on each heartbeat tick. ctx is the tick's 10s bound;
// returning its error closes the connection, exactly as a failed health write does.
func (c *Client) OnHeartbeat(ctx context.Context, s gateway.Sender) error {
	return s.Send(ctx, c.buildConnectionStatus())
}

// OnDraining waits for in-flight request goroutines before the connection is torn down.
func (c *Client) OnDraining(_ context.Context) {
	c.waitForInFlight(drainTimeout)
}

// HandleMessage dispatches the connector's inbound messages.
func (c *Client) HandleMessage(ctx context.Context, s gateway.Sender, typ string, raw json.RawMessage) {
	switch typ {
	case "http_request":
		var msg proto.HTTPRequestMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			slog.Warn("malformed http_request message", "err", err)
			return
		}
		select {
		case c.workerSem <- struct{}{}:
		default:
			body := `{"error":"too many concurrent requests"}`
			if err := s.Send(ctx, proto.HTTPResponseMsg{
				Type: "http_response", RequestID: msg.RequestID, ResponderID: msg.ResponderID,
				StatusCode: 429, Body: &body,
			}); err != nil {
				slog.Warn("failed to send 429 http_response", "request_id", msg.RequestID, "err", err)
			}
			return
		}
		c.inFlight.Add(1)
		go func(m proto.HTTPRequestMsg) {
			defer c.inFlight.Done()
			defer func() { <-c.workerSem }()
			c.handleHTTPRequest(ctx, s, m)
		}(msg)

	case "status_request":
		// Handled inline (no goroutine): building status is a cache read and the write is
		// serialized by the Sender, so a goroutine per request would only add an
		// unbounded-goroutine vector for a flood of status_requests. The 10s bound keeps a
		// stuck write from stalling the read loop (and thus delaying going_away handling)
		// indefinitely — the heartbeat ping is the backstop.
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		c.emitConnectionStatus(sctx, s)
		cancel()

	case "test_connection":
		var msg proto.TestConnectionMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			slog.Warn("malformed test_connection", "err", err)
			return
		}
		// Use the same semaphore as http_request so probes cannot create unbounded goroutines
		// that make outbound HTTP calls to the customer's internal network.
		select {
		case c.workerSem <- struct{}{}:
		default:
			slog.Warn("test_connection dropped: too many concurrent requests", "provider_key", msg.ProviderKey)
			return
		}
		go func(key string) {
			defer func() { <-c.workerSem }()
			probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			result := c.probePlugin(probeCtx, key)
			c.updatePluginStatus(result)
			if err := s.Send(ctx, result); err != nil {
				slog.Warn("failed to send test_result", "err", err)
			}
			c.SignalStatusPush()
		}(msg.ProviderKey)

	default:
		slog.Debug("ignoring message", "type", typ)
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

	// Effective CA bundle: a per-plugin ca_bundle overrides the global default, so the
	// probe verifies an internal CA the same way a forwarded request does — otherwise a
	// reachable internal-CA host would be reported as a TLS failure and push operators
	// toward disabling verification.
	var caBundle string
	if c.cfg != nil {
		caBundle = c.cfg.CABundle
	}
	if haveCredential && credential.CABundle != "" {
		caBundle = credential.CABundle
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
	switch {
	case caBundle != "":
		transport, err := httpclient.NewTransport(useDirectTransport, caBundle)
		if err != nil {
			// Log locally (path and error class are operator-facing, as in the handler)
			// but fail closed on the wire with an address-free diagnostic: the path may
			// embed internal detail, and the error class is all the platform needs.
			slog.Warn("probe: upstream CA bundle could not be loaded", "ca_bundle", caBundle, "err", err)
			result.Status = "unreachable"
			result.Diagnostic = "CA bundle could not be loaded"
			return result
		}
		probeClient.Transport = transport
	case useDirectTransport:
		probeClient.Transport = httpclient.NewDirectTransport()
	}
	// Release any idle connections this one-shot probe leaves pooled.
	if tr, ok := probeClient.Transport.(*http.Transport); ok {
		defer tr.CloseIdleConnections()
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

// emitConnectionStatus writes the current connection_status through the sender.
func (c *Client) emitConnectionStatus(ctx context.Context, s gateway.Sender) {
	if err := s.Send(ctx, c.buildConnectionStatus()); err != nil {
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

func (c *Client) handleHTTPRequest(ctx context.Context, s gateway.Sender, msg proto.HTTPRequestMsg) {
	strPtr := func(v string) *string { return &v }

	if c.criticalPatchRequired.Load() {
		resp := proto.HTTPResponseMsg{
			Type: "http_response", RequestID: msg.RequestID, ResponderID: msg.ResponderID,
			StatusCode: 503, Body: strPtr(`{"error":"connector has suspended tool dispatch pending a critical security update"}`),
		}
		if err := s.Send(ctx, resp); err != nil {
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
	if err := s.Send(ctx, resp); err != nil {
		slog.Warn("failed to send http_response", "request_id", msg.RequestID, "err", err)
	}
}
