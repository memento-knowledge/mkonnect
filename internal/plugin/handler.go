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
