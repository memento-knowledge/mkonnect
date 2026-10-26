package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/httpclient"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// hopByHopHeaders are headers that describe a single transport hop and must not be
// forwarded to the upstream service. Forwarding Connection/Transfer-Encoding in
// particular enables HTTP request-smuggling (CL.TE) attacks.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Proxy-Connection":    true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// sensitiveResponseHeaders may carry credentials created or used inside the
// customer's network. They must not be sent back through the bridge.
var sensitiveResponseHeaders = map[string]bool{
	"Authorization":       true,
	"Cookie":              true,
	"Proxy-Authorization": true,
	"Set-Cookie":          true,
}

// stripHeaderKey normalises a header name for credential-strip matching: it folds the
// underscore spelling (e.g. Private_token) onto the dash form that some upstreams treat as
// equivalent, then canonicalises — so a lookup matches regardless of casing or separator,
// and a configured name and a caller's name match whichever spelling each uses.
func stripHeaderKey(name string) string {
	return http.CanonicalHeaderKey(strings.ReplaceAll(name, "_", "-"))
}

// stripHeaderSet builds a set of header names keyed by stripHeaderKey.
func stripHeaderSet(names ...string) map[string]bool {
	s := make(map[string]bool, len(names))
	for _, n := range names {
		s[stripHeaderKey(n)] = true
	}
	return s
}

// defaultCredentialHeaders are inbound credential-bearing headers that must never reach
// the upstream when mkonnect applies its own local credential: a buggy or hostile message
// could otherwise add or override credentials the upstream honours. They are always
// stripped for a credentialed plugin; a plugin may strip additional headers via its
// StripHeaders config. Proxy-Authorization is also hop-by-hop; it is listed here to make
// the intent explicit and survive any future edit to hopByHopHeaders. This is not an
// exhaustive list of every service's credential header — add service-specific ones (e.g.
// X-Vault-Token, X-Amz-Security-Token) per plugin via StripHeaders.
var defaultCredentialHeaders = stripHeaderSet(
	"Authorization",
	"Proxy-Authorization",
	"Cookie",
	"PRIVATE-TOKEN",
	"JOB-TOKEN",
	"X-Api-Key",
	"X-Auth-Token",
)

const unsafeCredentialResponse = `{"error":"upstream response contains local credentials"}`

// credentialRepresentations returns encoded and complete-authorization forms of
// local credentials that an upstream service could reflect. A username alone is
// not included because it is an identifier rather than an authentication secret.
func credentialRepresentations(cred creds.Credential) []string {
	if cred.Token == "" {
		return nil
	}

	values := []string{cred.Token}
	appendEncoded := func(value string) {
		for _, encoded := range []string{
			url.QueryEscape(value),
			url.PathEscape(value),
			base64.StdEncoding.EncodeToString([]byte(value)),
			base64.RawStdEncoding.EncodeToString([]byte(value)),
			base64.URLEncoding.EncodeToString([]byte(value)),
			base64.RawURLEncoding.EncodeToString([]byte(value)),
		} {
			if encoded != value {
				values = append(values, encoded)
			}
		}
	}
	appendEncoded(cred.Token)
	switch cred.Auth {
	case "bearer":
		authorization := "Bearer " + cred.Token
		values = append(values, authorization)
		appendEncoded(authorization)
	case "basic":
		if cred.Username != "" {
			encoded := base64.StdEncoding.EncodeToString([]byte(cred.Username + ":" + cred.Token))
			raw := cred.Username + ":" + cred.Token
			authorization := "Basic " + encoded
			values = append(values, raw, authorization, encoded)
			appendEncoded(raw)
			appendEncoded(authorization)
			appendEncoded(encoded)
		}
	}
	return values
}

func containsCredentialRepresentation(value string, representations []string) bool {
	for _, representation := range representations {
		if strings.Contains(value, representation) {
			return true
		}
	}
	return false
}

// responseContainsLocalCredentials checks every response field before it can
// become a bridge message. An unsafe response is withheld intact rather than
// applying substring replacements that could corrupt unrelated response data.
func responseContainsLocalCredentials(headers http.Header, body string, cred creds.Credential) bool {
	representations := credentialRepresentations(cred)
	if len(representations) == 0 {
		return false
	}
	if containsCredentialRepresentation(body, representations) {
		return true
	}
	for key, values := range headers {
		if containsCredentialRepresentation(key, representations) {
			return true
		}
		for _, value := range values {
			if containsCredentialRepresentation(value, representations) {
				return true
			}
		}
	}
	return false
}

// isTimeoutError reports whether err, or any error it wraps, is a timeout. Unlike
// os.IsTimeout it walks the %w chain, so it stays correct even after the error has
// been wrapped with fmt.Errorf.
func isTimeoutError(err error) bool {
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) {
		return timeout.Timeout()
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// transportErrorCode classifies an error from client.Do (reached only when the
// request context is not itself done) into an error-marker code:
//   - A dial-phase failure — connection refused or name-resolution failure —
//     means the connection was never established, so the request did not reach
//     the upstream: upstream_unreachable.
//   - Any timeout leaves the outcome unknown: upstream_timeout. Note that a
//     connect timeout under http.Client.Timeout surfaces as a deadline error
//     ("awaiting headers"), not a dial OpError, so a black-holed host is reported
//     as upstream_timeout, not upstream_unreachable — the conservative direction,
//     and the two cannot be told apart at this layer anyway.
//   - Any other transport error — a TLS handshake failure, a reset connection, or
//     a failed proxy CONNECT (whose outer OpError Op is "proxyconnect", so the
//     dial check below does not match it) — means the request may already have
//     been sent: upstream_error.
func transportErrorCode(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return ErrCodeUpstreamUnreachable
	}
	if isTimeoutError(err) {
		return ErrCodeUpstreamTimeout
	}
	return ErrCodeUpstreamError
}

// HTTPPluginHandler processes an inbound HTTPRequestMsg, returning status, response headers,
// body (plain text, nil for empty), and any transport-level error.
type HTTPPluginHandler func(context.Context, proto.HTTPRequestMsg) (statusCode int, headers map[string]string, body *string, err error)

// upstreamTimeout bounds a single proxied request to an internal service. It is a var (not a
// const) so tests can shorten it to exercise the timeout path without a real wait.
var upstreamTimeout = 10 * time.Second

// HTTPHandler returns an HTTPPluginHandler that:
//  1. Resolves the plugin's base URL from the creds store first, then the registry.
//  2. Strips caller-supplied credential headers, then injects an Authorization header
//     when local credentials are configured, so the local credential is the only one used.
//  3. Forwards the HTTP request and returns status, headers, and body.
func HTTPHandler(registry *Registry, store *creds.Store) HTTPPluginHandler {
	httpClient := &http.Client{
		Timeout: upstreamTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	directHTTPClient := &http.Client{
		Timeout:   upstreamTimeout,
		Transport: httpclient.NewDirectTransport(),
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	errBody := func(s string) *string { return &s }

	return func(ctx context.Context, msg proto.HTTPRequestMsg) (int, map[string]string, *string, error) {
		// Resolve the credential once and reuse it for both base-URL resolution and
		// Authorization injection below (avoids a second store lock per request).
		cred, haveCred := store.Get(msg.ProviderKey)

		// Resolve base URL: creds store takes priority over registry.
		var baseURL string
		if haveCred && cred.BaseURL != "" {
			baseURL = cred.BaseURL
		} else if u, ok := registry.Get(msg.ProviderKey); ok {
			baseURL = u
		} else {
			b, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("provider %q not found", msg.ProviderKey)})
			s := string(b)
			return 404, errHeaders(ErrCodeProviderNotFound), &s, nil
		}

		// Validate method.
		method := strings.ToUpper(strings.TrimSpace(msg.Method))
		if method == "" {
			method = http.MethodGet
		}
		switch method {
		case "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT":
		default:
			return 405, errHeaders(ErrCodeMethodNotAllowed), errBody(`{"error":"method not allowed"}`), nil
		}

		// Validate path and resolve against base URL, preserving the base path prefix
		// (confinement) and any query string supplied by the caller.
		if !strings.HasPrefix(msg.Path, "/") {
			return 400, errHeaders(ErrCodeInvalidPath), errBody(`{"error":"path must begin with /"}`), nil
		}
		base, err := url.Parse(baseURL)
		if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
			return 500, errHeaders(ErrCodeInvalidBaseURL), errBody(`{"error":"invalid base URL"}`), nil
		}
		// url.Parse correctly splits path and raw query from msg.Path (e.g. "/api?k=v").
		reqRef, err := url.Parse(msg.Path)
		if err != nil {
			return 400, errHeaders(ErrCodeInvalidPath), errBody(`{"error":"invalid path"}`), nil
		}
		// Reject a caller path that carries URL userinfo (e.g. //user:pw@host/x). It begins
		// with "/" so it passes the prefix guard above, and ResolveReference carries ref.User
		// into the resolved target, where Go's HTTP client would turn it into a Basic
		// Authorization header when none is set — letting a caller inject upstream credentials
		// via the path. base.User is rejected above (invalid_base_url); this is the symmetric
		// check on the caller-supplied path.
		if reqRef.User != nil {
			return 400, errHeaders(ErrCodeInvalidPath), errBody(`{"error":"invalid path"}`), nil
		}
		// Prepend the base path so a base URL of https://host/api/v1 + /jobs stays confined
		// under /api/v1/ and doesn't resolve to /jobs on the root.
		reqRef.Path = base.Path + reqRef.Path
		target := base.ResolveReference(reqRef)
		// Clear RawPath so the outbound request always uses the decoded, normalized path.
		// url.Parse preserves encoded segments (e.g. %2e%2e) in RawPath; target.String()
		// emits them verbatim, letting an upstream that decodes %2e%2e to ".." escape the
		// configured prefix even after our traversal check.
		target.RawPath = ""
		target.Path = path.Clean(target.Path)
		if target.Host != base.Host {
			return 400, errHeaders(ErrCodeInvalidPath), errBody(`{"error":"invalid path"}`), nil
		}
		// Verify that the normalized path still starts with the configured base path prefix.
		if base.Path != "" && base.Path != "/" {
			if !strings.HasPrefix(target.Path+"/", strings.TrimRight(base.Path, "/")+"/") {
				return 400, errHeaders(ErrCodePathBlocked), errBody(`{"error":"path traversal not allowed"}`), nil
			}
		}

		const maxBodyBytes = 32 << 20

		// Build the request body reader from the plain-text string field.
		var reqBodyReader *strings.Reader
		if msg.Body != nil {
			if len(*msg.Body) > maxBodyBytes {
				return 413, errHeaders(ErrCodeRequestTooLarge), errBody(`{"error":"request body too large"}`), nil
			}
			reqBodyReader = strings.NewReader(*msg.Body)
		} else {
			reqBodyReader = strings.NewReader("")
		}

		req, err := http.NewRequestWithContext(ctx, method, target.String(), reqBodyReader)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("build request: %w", err)
		}

		// Decide which inbound credential-bearing headers to drop before forwarding.
		// A plugin's explicit StripHeaders are honoured whenever the entry exists. The
		// credential-header defaults are added only when a local credential is actually
		// applied, so the local credential is the one credential the upstream sees; for a
		// plugin without a local credential a caller-supplied Authorization is intentional
		// (platform-held credentials) and is left untouched.
		useDirectTransport := haveCred && cred.HasAuthorization()
		var stripHeaders map[string]bool
		if haveCred && len(cred.StripHeaders) > 0 {
			stripHeaders = stripHeaderSet(cred.StripHeaders...)
		}
		if useDirectTransport {
			if stripHeaders == nil {
				stripHeaders = make(map[string]bool, len(defaultCredentialHeaders))
			}
			for h := range defaultCredentialHeaders {
				stripHeaders[h] = true
			}
		}

		// Forward inbound headers from the gateway (cloud-side credentials arrive here),
		// excluding hop-by-hop headers (CL.TE smuggling risk) and the credential-bearing
		// headers selected above. The underscore spelling (e.g. Private_token) is matched
		// against the dash form too, since some upstreams treat the two as equivalent.
		for k, v := range msg.Headers {
			if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
				continue
			}
			if stripHeaders[stripHeaderKey(k)] {
				continue
			}
			req.Header.Set(k, v)
		}

		req.Header.Set("Via", "1.1 mkonnect")

		// Inject local credentials for this configured plugin. The credential-bearing
		// inbound headers were stripped above, so the local credential is the only
		// credential the upstream receives.
		client := httpClient
		if useDirectTransport {
			cred.ApplyAuthorization(req)
			client = directHTTPClient
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, nil, ctx.Err()
			}
			// Distinguish the error so the platform can judge whether the request may
			// have reached the upstream: a dial failure means it never did
			// (upstream_unreachable); a timeout or any other transport error means the
			// outcome is unknown. The status stays 504 for compatibility; the marker
			// carries the distinction.
			return http.StatusGatewayTimeout, errHeaders(transportErrorCode(err)), errBody(`{"error":"upstream timeout or connection error"}`), err
		}
		defer resp.Body.Close() //nolint:errcheck

		respBytes, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBodyBytes)+1))
		if err != nil {
			// A done request context is platform-initiated cancellation, not an
			// upstream failure; propagate it as the client.Do path does (unmarked).
			if ctx.Err() != nil {
				return 0, nil, nil, ctx.Err()
			}
			// The per-request timeout also covers reading the response body, so a stall
			// here (after headers arrived) is a case where a write may already have
			// taken effect. Mark it so the platform can tell it apart from a response
			// relayed from the upstream: a read timeout is upstream_timeout, any other
			// read failure upstream_error. The status stays 504 (as on the client.Do
			// path) for compatibility; the marker carries the distinction. The non-nil
			// err is still returned so the ws layer logs it.
			code := ErrCodeUpstreamError
			if isTimeoutError(err) {
				code = ErrCodeUpstreamTimeout
			}
			return http.StatusGatewayTimeout, errHeaders(code), errBody(`{"error":"error reading upstream response"}`), fmt.Errorf("read response: %w", err)
		}
		if len(respBytes) > maxBodyBytes {
			return 413, errHeaders(ErrCodeResponseTooLarge), errBody(`{"error":"upstream response too large"}`), nil
		}
		body := string(respBytes)
		if useDirectTransport && responseContainsLocalCredentials(resp.Header, body, cred) {
			return http.StatusBadGateway, errHeaders(ErrCodeUpstreamCredentialLeak), errBody(unsafeCredentialResponse), nil
		}

		// Collect response headers (first value per header name). ErrorHeader is
		// stripped here so a hostile or compromised upstream cannot forge the
		// connector's synthesised-error marker on a relayed response.
		respHeaders := make(map[string]string, len(resp.Header))
		for k, vs := range resp.Header {
			canonical := http.CanonicalHeaderKey(k)
			if len(vs) > 0 && !sensitiveResponseHeaders[canonical] && canonical != ErrorHeader {
				respHeaders[k] = vs[0]
			}
		}

		return resp.StatusCode, respHeaders, &body, nil
	}
}
