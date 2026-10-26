package plugin

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// marker returns the X-Mkonnect-Error value from a response header map ("" if absent).
func marker(headers map[string]string) string { return headers[ErrorHeader] }

// TestSynthesisedResponsesCarryMarker checks that every connector-synthesised
// response that rejects a request before (or instead of) relaying an upstream
// response carries the expected X-Mkonnect-Error code.
func TestSynthesisedResponsesCarryMarker(t *testing.T) {
	// A reachable upstream for the cases where the rejection happens after base-URL
	// resolution but still before/instead of relaying a real upstream response.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	tests := []struct {
		name       string
		registry   *Registry
		msg        proto.HTTPRequestMsg
		wantStatus int
		wantCode   string
	}{
		{
			name:       "provider not found",
			registry:   newRegistry("known", upstream.URL),
			msg:        proto.HTTPRequestMsg{ProviderKey: "unknown", Method: http.MethodGet, Path: "/"},
			wantStatus: 404,
			wantCode:   ErrCodeProviderNotFound,
		},
		{
			name:       "method not allowed",
			registry:   newRegistry("svc", upstream.URL),
			msg:        proto.HTTPRequestMsg{ProviderKey: "svc", Method: "TRACE", Path: "/"},
			wantStatus: 405,
			wantCode:   ErrCodeMethodNotAllowed,
		},
		{
			name:       "path missing leading slash",
			registry:   newRegistry("svc", upstream.URL),
			msg:        proto.HTTPRequestMsg{ProviderKey: "svc", Method: http.MethodGet, Path: "noslash"},
			wantStatus: 400,
			wantCode:   ErrCodeInvalidPath,
		},
		{
			name:       "invalid base URL",
			registry:   newRegistry("svc", "ftp://service.invalid/path"),
			msg:        proto.HTTPRequestMsg{ProviderKey: "svc", Method: http.MethodGet, Path: "/"},
			wantStatus: 500,
			wantCode:   ErrCodeInvalidBaseURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := HTTPHandler(tt.registry, mustStore(t))
			status, headers, _, err := h(context.Background(), tt.msg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if got := marker(headers); got != tt.wantCode {
				t.Fatalf("%s = %q, want %q", ErrorHeader, got, tt.wantCode)
			}
		})
	}
}

// TestPathBlockedCarriesMarker checks the path-confinement rejection is marked
// path_blocked (distinct from a merely malformed path).
func TestPathBlockedCarriesMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	baseWithPrefix := srv.URL + "/api/v1"
	store := mustStore(t)
	if err := store.Set("svc", creds.Credential{BaseURL: baseWithPrefix}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	h := HTTPHandler(newRegistry("svc", baseWithPrefix), store)

	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/../../etc/passwd",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != 400 {
		t.Fatalf("status = %d, want 400", status)
	}
	if got := marker(headers); got != ErrCodePathBlocked {
		t.Fatalf("%s = %q, want %q", ErrorHeader, got, ErrCodePathBlocked)
	}
}

// TestRequestBodyTooLargeCarriesMarker checks an over-limit request body is
// rejected with the request_too_large marker before being forwarded.
func TestRequestBodyTooLargeCarriesMarker(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := HTTPHandler(newRegistry("svc", srv.URL), mustStore(t))
	big := strings.Repeat("a", (32<<20)+1)
	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodPost, Path: "/", Body: &big,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != 413 {
		t.Fatalf("status = %d, want 413", status)
	}
	if got := marker(headers); got != ErrCodeRequestTooLarge {
		t.Fatalf("%s = %q, want %q", ErrorHeader, got, ErrCodeRequestTooLarge)
	}
	if reached {
		t.Fatal("over-limit request body must not reach the upstream")
	}
}

// TestResponseTooLargeCarriesMarker checks an over-limit upstream response is
// withheld with the response_too_large marker.
func TestResponseTooLargeCarriesMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, (32<<20)+1))
	}))
	defer srv.Close()

	h := HTTPHandler(newRegistry("svc", srv.URL), mustStore(t))
	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != 413 {
		t.Fatalf("status = %d, want 413", status)
	}
	if got := marker(headers); got != ErrCodeResponseTooLarge {
		t.Fatalf("%s = %q, want %q", ErrorHeader, got, ErrCodeResponseTooLarge)
	}
}

// TestUpstreamCredentialLeakCarriesMarker checks that withholding a response that
// reflects local credentials is marked upstream_credential_leak.
func TestUpstreamCredentialLeakCarriesMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reflect the injected Authorization header back in the body.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("authorization=" + r.Header.Get("Authorization")))
	}))
	defer srv.Close()

	store := mustStore(t)
	if err := store.Set("svc", creds.Credential{BaseURL: srv.URL, Auth: "bearer", Token: "a-secret-token-value"}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	h := HTTPHandler(newRegistry("svc", srv.URL), store)

	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if got := marker(headers); got != ErrCodeUpstreamCredentialLeak {
		t.Fatalf("%s = %q, want %q", ErrorHeader, got, ErrCodeUpstreamCredentialLeak)
	}
}

// TestUpstreamTimeoutCarriesMarker checks a slow upstream (headers never sent in
// time) is marked upstream_timeout.
func TestUpstreamTimeoutCarriesMarker(t *testing.T) {
	orig := upstreamTimeout
	upstreamTimeout = 20 * time.Millisecond
	defer func() { upstreamTimeout = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := HTTPHandler(newRegistry("slow", srv.URL), mustStore(t))
	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "slow", Method: http.MethodGet, Path: "/",
	})
	if err == nil {
		t.Fatal("expected a transport error on timeout")
	}
	if status != 504 {
		t.Fatalf("status = %d, want 504", status)
	}
	if got := marker(headers); got != ErrCodeUpstreamTimeout {
		t.Fatalf("%s = %q, want %q", ErrorHeader, got, ErrCodeUpstreamTimeout)
	}
}

// TestUpstreamReadTimeoutCarriesMarker checks a stall during the response-body
// read (after headers arrive) is also marked upstream_timeout. This is the case
// where a write may already have taken effect, so it must be distinguishable.
func TestUpstreamReadTimeoutCarriesMarker(t *testing.T) {
	orig := upstreamTimeout
	upstreamTimeout = 40 * time.Millisecond
	defer func() { upstreamTimeout = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Headers are delivered; stall before sending the body so the read times out.
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	}))
	defer srv.Close()

	h := HTTPHandler(newRegistry("svc", srv.URL), mustStore(t))
	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err == nil {
		t.Fatal("expected a transport error on body-read timeout")
	}
	if status != 504 {
		t.Fatalf("status = %d, want 504", status)
	}
	if got := marker(headers); got != ErrCodeUpstreamTimeout {
		t.Fatalf("%s = %q, want %q", ErrorHeader, got, ErrCodeUpstreamTimeout)
	}
}

// TestUpstreamUnreachableCarriesMarker checks that a failure to establish the
// connection (dial failure) is marked upstream_unreachable — the request was
// never sent.
func TestUpstreamUnreachableCarriesMarker(t *testing.T) {
	// Reserve a loopback port, then release it so the dial is refused deterministically.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	h := HTTPHandler(newRegistry("svc", "http://"+addr), mustStore(t))
	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err == nil {
		t.Fatal("expected a transport error on connection refused")
	}
	if status != 504 {
		t.Fatalf("status = %d, want 504", status)
	}
	if got := marker(headers); got != ErrCodeUpstreamUnreachable {
		t.Fatalf("%s = %q, want %q", ErrorHeader, got, ErrCodeUpstreamUnreachable)
	}
}

// TestRelayedUpstreamResponsesCarryNoMarker checks that responses relayed from the
// upstream — including upstream-originated 5xx — never carry the marker.
func TestRelayedUpstreamResponsesCarryNoMarker(t *testing.T) {
	for _, upstreamStatus := range []int{200, 404, 500, 502, 503} {
		t.Run(http.StatusText(upstreamStatus), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(upstreamStatus)
				_, _ = w.Write([]byte("upstream body"))
			}))
			defer srv.Close()

			h := HTTPHandler(newRegistry("svc", srv.URL), mustStore(t))
			status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
				ProviderKey: "svc", Method: http.MethodGet, Path: "/",
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != upstreamStatus {
				t.Fatalf("status = %d, want %d", status, upstreamStatus)
			}
			if got := marker(headers); got != "" {
				t.Fatalf("relayed upstream response must carry no marker, got %s=%q", ErrorHeader, got)
			}
		})
	}
}

// TestUpstreamCannotSpoofMarker checks that an X-Mkonnect-Error set by the upstream
// is stripped, so a compromised or hostile upstream cannot forge the marker while
// an ordinary response header still passes through.
func TestUpstreamCannotSpoofMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Set via a non-canonical key to prove canonicalisation covers it.
		w.Header().Set("x-mkonnect-error", ErrCodePathBlocked)
		w.Header().Set("X-Ordinary", "kept")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := HTTPHandler(newRegistry("svc", srv.URL), mustStore(t))
	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	for k, v := range headers {
		if http.CanonicalHeaderKey(k) == ErrorHeader {
			t.Fatalf("upstream-set marker must be stripped, got %q=%q", k, v)
		}
	}
	if headers["X-Ordinary"] != "kept" {
		t.Fatalf("ordinary upstream header must pass through, got %v", headers)
	}
}
