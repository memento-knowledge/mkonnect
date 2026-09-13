package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// hopByHopHeaders are headers that must not be forwarded to the upstream service.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

// Handler returns a closure that proxies DataMsg requests to the registered plugin services.
func Handler(registry *Registry) func(ctx context.Context, msg proto.DataMsg) (int, []byte, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	return func(ctx context.Context, msg proto.DataMsg) (int, []byte, error) {
		baseURL, ok := registry.Get(msg.Plugin)
		if !ok {
			body, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("plugin %q not found", msg.Plugin)})
			return 404, body, nil
		}

		target := strings.TrimRight(baseURL, "/") + msg.Path

		req, err := http.NewRequestWithContext(ctx, msg.Method, target, bytes.NewReader(msg.Body))
		if err != nil {
			return 0, nil, fmt.Errorf("build request: %w", err)
		}

		// Strip hop-by-hop headers and add Via for proxy identification.
		for _, h := range hopByHopHeaders {
			req.Header.Del(h)
		}
		req.Header.Set("Via", "1.1 mkonnect")

		resp, err := client.Do(req)
		if err != nil {
			return 504, []byte(`{"error":"upstream timeout or connection error"}`), err
		}
		defer resp.Body.Close() //nolint:errcheck

		const maxBodyBytes = 32 << 20 // 32 MiB
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		if err != nil {
			return 0, nil, fmt.Errorf("read response: %w", err)
		}

		return resp.StatusCode, respBody, nil
	}
}
