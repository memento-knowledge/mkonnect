package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/creds"
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

// HTTPPluginHandler processes an inbound HTTPRequestMsg, returning status, response headers,
// body (plain text, nil for empty), and any transport-level error.
type HTTPPluginHandler func(context.Context, proto.HTTPRequestMsg) (statusCode int, headers map[string]string, body *string, err error)

// upstreamTimeout bounds a single proxied request to an internal service. It is a var (not a
// const) so tests can shorten it to exercise the timeout path without a real wait.
var upstreamTimeout = 10 * time.Second

// HTTPHandler returns an HTTPPluginHandler that:
//  1. Resolves the plugin's base URL from the creds store first, then the registry.
//  2. Injects an Authorization header if local credentials are configured.
//  3. Forwards the HTTP request and returns status, headers, and body.
func HTTPHandler(registry *Registry, store *creds.Store) HTTPPluginHandler {
	httpClient := &http.Client{
		Timeout: upstreamTimeout,
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
			return 404, nil, &s, nil
		}

		// Validate method.
		method := strings.ToUpper(strings.TrimSpace(msg.Method))
		if method == "" {
			method = http.MethodGet
		}
		switch method {
		case "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT":
		default:
			return 405, nil, errBody(`{"error":"method not allowed"}`), nil
		}

		// Validate path and resolve against base URL, preserving the base path prefix
		// (confinement) and any query string supplied by the caller.
		if !strings.HasPrefix(msg.Path, "/") {
			return 400, nil, errBody(`{"error":"path must begin with /"}`), nil
		}
		base, err := url.Parse(baseURL)
		if err != nil {
			return 500, nil, errBody(`{"error":"invalid base URL"}`), nil
		}
		// url.Parse correctly splits path and raw query from msg.Path (e.g. "/api?k=v").
		reqRef, err := url.Parse(msg.Path)
		if err != nil {
			return 400, nil, errBody(`{"error":"invalid path"}`), nil
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
			return 400, nil, errBody(`{"error":"invalid path"}`), nil
		}
		// Verify that the normalized path still starts with the configured base path prefix.
		if base.Path != "" && base.Path != "/" {
			if !strings.HasPrefix(target.Path+"/", strings.TrimRight(base.Path, "/")+"/") {
				return 400, nil, errBody(`{"error":"path traversal not allowed"}`), nil
			}
		}

		const maxBodyBytes = 32 << 20

		// Build the request body reader from the plain-text string field.
		var reqBodyReader *strings.Reader
		if msg.Body != nil {
			if len(*msg.Body) > maxBodyBytes {
				return 413, nil, errBody(`{"error":"request body too large"}`), nil
			}
			reqBodyReader = strings.NewReader(*msg.Body)
		} else {
			reqBodyReader = strings.NewReader("")
		}

		req, err := http.NewRequestWithContext(ctx, method, target.String(), reqBodyReader)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("build request: %w", err)
		}

		// Forward inbound headers from the gateway (cloud-side credentials arrive here),
		// excluding hop-by-hop headers that must not be forwarded (CL.TE smuggling risk).
		for k, v := range msg.Headers {
			if !hopByHopHeaders[http.CanonicalHeaderKey(k)] {
				req.Header.Set(k, v)
			}
		}

		req.Header.Set("Via", "1.1 mkonnect")

		// Inject local credentials only for this configured plugin. This happens
		// after bridge headers are copied so the local credential cannot be replaced.
		if haveCred {
			cred.ApplyAuthorization(req)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, nil, ctx.Err()
			}
			return 504, nil, errBody(`{"error":"upstream timeout or connection error"}`), err
		}
		defer resp.Body.Close() //nolint:errcheck

		respBytes, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBodyBytes)+1))
		if err != nil {
			return 0, nil, nil, fmt.Errorf("read response: %w", err)
		}
		if len(respBytes) > maxBodyBytes {
			return 413, nil, errBody(`{"error":"upstream response too large"}`), nil
		}

		// Collect response headers (first value per header name).
		respHeaders := make(map[string]string, len(resp.Header))
		for k, vs := range resp.Header {
			if len(vs) > 0 && !sensitiveResponseHeaders[http.CanonicalHeaderKey(k)] {
				respHeaders[k] = vs[0]
			}
		}

		s := string(respBytes)
		return resp.StatusCode, respHeaders, &s, nil
	}
}
