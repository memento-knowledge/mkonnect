package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// TestRequestExpired exercises the pure deadline check with a fixed clock so the
// boundary (equal == not expired), the fail-open cases (empty, malformed, missing
// offset), fractional seconds, and offset handling are all deterministic.
func TestRequestExpired(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		expiresAt   string
		wantExpired bool
		wantErr     bool
	}{
		{"absent", "", false, false},
		{"past", "2026-10-20T11:00:00Z", true, false},
		{"future", "2026-10-20T13:00:00Z", false, false},
		{"equal is not expired", "2026-10-20T12:00:00Z", false, false},
		{"malformed", "not-a-timestamp", false, true},
		{"missing offset is malformed", "2026-10-20T11:00:00", false, true},
		{"fractional seconds past", "2026-10-20T11:00:00.5Z", true, false},
		// Past instant written in a far-east offset: the literal date/time reads like the
		// future, so a naive string/local comparison would get it wrong; the UTC instant
		// (2026-10-20T11:00:00Z) is in the past, so it must be expired.
		{"past instant via +14:00 offset", "2026-10-21T01:00:00+14:00", true, false},
		// Future instant written in a far-west offset: the literal reads like the past, but
		// the UTC instant (2026-10-20T13:00:00Z) is in the future, so it must NOT be expired.
		{"future instant via -12:00 offset", "2026-10-20T01:00:00-12:00", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expired, _, err := requestExpired(tt.expiresAt, now)
			if expired != tt.wantExpired {
				t.Errorf("expired = %v, want %v", expired, tt.wantExpired)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// recordingUpstream returns an httptest server and a flag that reports whether the
// upstream was ever contacted, so expiry tests can assert the request was not forwarded.
func recordingUpstream(t *testing.T) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	var hit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &hit
}

// TestHTTPHandlerExpiredRequestNotForwarded checks that a request whose expires_at has
// already passed is rejected with a synthesised, marked response and never reaches the
// upstream.
func TestHTTPHandlerExpiredRequestNotForwarded(t *testing.T) {
	srv, hit := recordingUpstream(t)
	h := HTTPHandler(newRegistry("svc", srv.URL), mustStore(t), "")

	past := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	status, headers, body, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodPost, Path: "/write", ExpiresAt: past,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hit.Load() {
		t.Fatal("expired request must not reach the upstream")
	}
	if status != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want %d", status, http.StatusGatewayTimeout)
	}
	if got := marker(headers); got != ErrCodeExpired {
		t.Errorf("%s = %q, want %q", ErrorHeader, got, ErrCodeExpired)
	}
	if len(headers) != 1 {
		t.Errorf("expired response headers = %v, want only the marker", headers)
	}
	if body == nil || *body != `{"error":"request expired before forwarding"}` {
		t.Errorf("body = %v, want the synthesised expired body", body)
	}
}

// TestHTTPHandlerUnexpiredRequestForwarded checks that a future expires_at does not block
// the request: it reaches the upstream and the response carries no marker.
func TestHTTPHandlerUnexpiredRequestForwarded(t *testing.T) {
	srv, hit := recordingUpstream(t)
	h := HTTPHandler(newRegistry("svc", srv.URL), mustStore(t), "")

	future := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/", ExpiresAt: future,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hit.Load() {
		t.Fatal("unexpired request must reach the upstream")
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if got := marker(headers); got != "" {
		t.Errorf("relayed response must not be marked, got %s = %q", ErrorHeader, got)
	}
}

// TestHTTPHandlerAbsentExpiresAtUnchanged checks that omitting expires_at preserves the
// pre-T5 behaviour: the request is forwarded normally.
func TestHTTPHandlerAbsentExpiresAtUnchanged(t *testing.T) {
	srv, hit := recordingUpstream(t)
	h := HTTPHandler(newRegistry("svc", srv.URL), mustStore(t), "")

	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hit.Load() {
		t.Fatal("request without expires_at must reach the upstream")
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if got := marker(headers); got != "" {
		t.Errorf("relayed response must not be marked, got %s = %q", ErrorHeader, got)
	}
}

// TestHTTPHandlerMalformedExpiresAtForwarded checks the fail-open decision: an unparseable
// expires_at is treated as absent and the request is forwarded rather than rejected, so a
// platform formatting bug cannot take the write path offline.
func TestHTTPHandlerMalformedExpiresAtForwarded(t *testing.T) {
	srv, hit := recordingUpstream(t)
	h := HTTPHandler(newRegistry("svc", srv.URL), mustStore(t), "")

	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/", ExpiresAt: "not-a-timestamp",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hit.Load() {
		t.Fatal("request with a malformed expires_at must still reach the upstream (fail open)")
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if got := marker(headers); got != "" {
		t.Errorf("forwarded response must not be marked, got %s = %q", ErrorHeader, got)
	}
}

// TestHTTPHandlerExpiryPrecedesProviderResolution pins the ordering: expiry is terminal and
// is evaluated before provider resolution, so an expired request for an unknown provider is
// rejected as expired, not provider_not_found. Both are "not forwarded", but the marker must
// reflect the reason the request will never run.
func TestHTTPHandlerExpiryPrecedesProviderResolution(t *testing.T) {
	h := HTTPHandler(newRegistry("known", "http://unused.invalid"), mustStore(t), "")

	past := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "unknown", Method: http.MethodGet, Path: "/", ExpiresAt: past,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want %d", status, http.StatusGatewayTimeout)
	}
	if got := marker(headers); got != ErrCodeExpired {
		t.Errorf("%s = %q, want %q", ErrorHeader, got, ErrCodeExpired)
	}
}
