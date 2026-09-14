package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// Handler returns a closure that proxies DataMsg requests to the registered plugin services.
func Handler(registry *Registry) func(ctx context.Context, msg proto.DataMsg) (int, []byte, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		// Do not follow redirects; let the caller decide.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return func(ctx context.Context, msg proto.DataMsg) (int, []byte, error) {
		baseURL, ok := registry.Get(msg.Plugin)
		if !ok {
			body, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("plugin %q not found", msg.Plugin)})
			return 404, body, nil
		}

		// Validate and normalise the HTTP method.
		method := strings.ToUpper(strings.TrimSpace(msg.Method))
		if method == "" {
			method = http.MethodGet
		}
		switch method {
		case "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT":
			// allowed
		default:
			body, _ := json.Marshal(map[string]string{"error": "method not allowed"})
			return 405, body, nil
		}

		// Validate the path and resolve it against the configured base URL.
		// Requiring a leading "/" prevents host-rewriting via "@" or "//".
		if !strings.HasPrefix(msg.Path, "/") {
			body, _ := json.Marshal(map[string]string{"error": "path must begin with /"})
			return 400, body, nil
		}
		base, _ := url.Parse(baseURL) // validated at registry load time
		target := base.ResolveReference(&url.URL{Path: msg.Path})
		if target.Host != base.Host {
			body, _ := json.Marshal(map[string]string{"error": "invalid path"})
			return 400, body, nil
		}

		const maxBodyBytes = 32 << 20 // 32 MiB — enforced on both request and response
		if len(msg.Body) > maxBodyBytes {
			return 413, []byte(`{"error":"request body too large"}`), nil
		}

		req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(msg.Body))
		if err != nil {
			return 0, nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Via", "1.1 mkonnect")

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			return 504, []byte(`{"error":"upstream timeout or connection error"}`), err
		}
		defer resp.Body.Close() //nolint:errcheck

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
		if err != nil {
			return 0, nil, fmt.Errorf("read response: %w", err)
		}
		if len(respBody) > maxBodyBytes {
			return 413, []byte(`{"error":"upstream response too large"}`), nil
		}

		return resp.StatusCode, respBody, nil
	}
}

// HTTPPluginHandler processes an inbound HTTPRequestMsg, returning status, response headers,
// body (plain text, nil for empty), and any transport-level error.
type HTTPPluginHandler func(context.Context, proto.HTTPRequestMsg) (statusCode int, headers map[string]string, body *string, err error)

// HTTPHandler returns an HTTPPluginHandler that:
//  1. Resolves the plugin's base URL from the creds store first, then the registry.
//  2. Injects an Authorization: Bearer header if a local bearer credential is configured.
//  3. Forwards the HTTP request and returns status, headers, and body.
func HTTPHandler(registry *Registry, store *creds.Store) HTTPPluginHandler {
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	errBody := func(s string) *string { return &s }

	return func(ctx context.Context, msg proto.HTTPRequestMsg) (int, map[string]string, *string, error) {
		// Resolve base URL: creds store takes priority over registry.
		var baseURL string
		if cred, ok := store.Get(msg.ProviderKey); ok && cred.BaseURL != "" {
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

		// Validate path and resolve against base URL.
		if !strings.HasPrefix(msg.Path, "/") {
			return 400, nil, errBody(`{"error":"path must begin with /"}`), nil
		}
		base, err := url.Parse(baseURL)
		if err != nil {
			return 500, nil, errBody(`{"error":"invalid base URL"}`), nil
		}
		target := base.ResolveReference(&url.URL{Path: msg.Path, RawQuery: base.RawQuery})
		if target.Host != base.Host {
			return 400, nil, errBody(`{"error":"invalid path"}`), nil
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

		// Forward inbound headers from the gateway (cloud-side credentials arrive here).
		for k, v := range msg.Headers {
			req.Header.Set(k, v)
		}

		req.Header.Set("Via", "1.1 mkonnect")

		// Inject local bearer credential if configured (local-side mode).
		if cred, ok := store.Get(msg.ProviderKey); ok && cred.Auth == "bearer" && cred.Token != "" {
			req.Header.Set("Authorization", "Bearer "+cred.Token)
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
			if len(vs) > 0 {
				respHeaders[k] = vs[0]
			}
		}

		s := string(respBytes)
		return resp.StatusCode, respHeaders, &s, nil
	}
}
