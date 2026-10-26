package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// TestEffectiveUpstreamTimeout covers the resolver in isolation: an unset or not-found
// credential yields the connector default, a configured value yields the parsed duration.
func TestEffectiveUpstreamTimeout(t *testing.T) {
	orig := upstreamTimeout
	upstreamTimeout = 7 * time.Second
	t.Cleanup(func() { upstreamTimeout = orig })

	if got := effectiveUpstreamTimeout(creds.Credential{}, false); got != 7*time.Second {
		t.Errorf("no credential: got %s, want default 7s", got)
	}
	if got := effectiveUpstreamTimeout(creds.Credential{}, true); got != 7*time.Second {
		t.Errorf("credential without override: got %s, want default 7s", got)
	}
	if got := effectiveUpstreamTimeout(creds.Credential{UpstreamTimeout: "45s"}, true); got != 45*time.Second {
		t.Errorf("configured override: got %s, want 45s", got)
	}
}

// mustStoreWithTimeout returns a store holding a no-auth plugin with the given per-plugin
// upstream timeout pointed at baseURL.
func mustStoreWithTimeout(t *testing.T, plugin, baseURL, timeout string) *creds.Store {
	t.Helper()
	store := mustStore(t)
	if err := store.Set(plugin, creds.Credential{BaseURL: baseURL, UpstreamTimeout: timeout}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	return store
}

// TestPerPluginUpstreamTimeoutHonoured proves the configured per-plugin timeout is used
// rather than the (longer) connector default: the global default stays at a long value, the
// plugin sets a short one, and a slow upstream trips the short one with an upstream_timeout
// marker. This is also the regression guard against shadowing the request context: if the
// own-deadline path were misrouted through the platform-cancellation branch, the response
// would be an unmarked status 0 and this assertion would fail.
func TestPerPluginUpstreamTimeoutHonoured(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Default left untouched (10s); the plugin's 1s override is what must trip here.
	store := mustStoreWithTimeout(t, "slow", srv.URL, "1s")
	h := HTTPHandler(newRegistry("slow", srv.URL), store, "")

	start := time.Now()
	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "slow", Method: http.MethodGet, Path: "/",
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a transport error on the per-plugin timeout")
	}
	if status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", status)
	}
	if got := marker(headers); got != ErrCodeUpstreamTimeout {
		t.Fatalf("%s = %q, want %q", ErrorHeader, got, ErrCodeUpstreamTimeout)
	}
	// It must trip near the 1s override, well before the 2s upstream sleep or the 10s default.
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("took %s; the 1s per-plugin timeout was not honoured", elapsed)
	}
}

// TestPerPluginUpstreamTimeoutLongerThanDefaultSucceeds proves the timeout lives on the
// per-request context, not the shared client: with the connector default shortened to 50ms,
// a plugin that configures a longer timeout still completes a request that takes longer than
// the default would have allowed. Before the per-request-deadline design, the shared client's
// Timeout would have capped this at the default and the request would have failed.
func TestPerPluginUpstreamTimeoutLongerThanDefaultSucceeds(t *testing.T) {
	// Mutates the global default, so it must not run in parallel with tests that rely on it.
	orig := upstreamTimeout
	upstreamTimeout = 50 * time.Millisecond
	t.Cleanup(func() { upstreamTimeout = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	store := mustStoreWithTimeout(t, "svc", srv.URL, "2s")
	h := HTTPHandler(newRegistry("svc", srv.URL), store, "")

	status, headers, body, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the 2s per-plugin timeout should allow a 300ms response despite the 50ms default)", status)
	}
	if got := marker(headers); got != "" {
		t.Fatalf("unexpected error marker %q on a successful response", got)
	}
	if body == nil || *body != "ok" {
		t.Fatalf("body = %v, want \"ok\"", body)
	}
}

// TestUpstreamTimeoutDefaultWhenUnset confirms a plugin with no per-plugin override falls
// back to the connector default: with the default shortened, a slow upstream still trips it.
func TestUpstreamTimeoutDefaultWhenUnset(t *testing.T) {
	orig := upstreamTimeout
	upstreamTimeout = 100 * time.Millisecond
	t.Cleanup(func() { upstreamTimeout = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A plugin configured WITHOUT an upstream_timeout (CABundle-free, no override).
	store := mustStore(t)
	if err := store.Set("svc", creds.Credential{BaseURL: srv.URL}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	h := HTTPHandler(newRegistry("svc", srv.URL), store, "")

	status, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err == nil {
		t.Fatal("expected a transport error on the default timeout")
	}
	if status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", status)
	}
	if got := marker(headers); got != ErrCodeUpstreamTimeout {
		t.Fatalf("%s = %q, want %q", ErrorHeader, got, ErrCodeUpstreamTimeout)
	}
}

// TestPlatformCancelStaysUnmarked confirms a cancellation of the platform-supplied request
// context (not our own deadline) is propagated unmarked: no synthesised status, no marker.
// The connector must not claim an upstream outcome it did not observe. The per-plugin and
// default timeouts are left long so only the parent cancellation can fire.
func TestPlatformCancelStaysUnmarked(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := mustStoreWithTimeout(t, "svc", srv.URL, "10m") // far longer than the cancel
	h := HTTPHandler(newRegistry("svc", srv.URL), store, "")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	status, headers, body, err := h(ctx, proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if status != 0 || headers != nil || body != nil {
		t.Fatalf("platform cancellation must be unmarked: got status=%d headers=%v body=%v", status, headers, body)
	}
}

// TestPerPluginReadTimeoutCarriesMarker covers the response-body read branch under a
// per-plugin timeout (the new per-request-context code path, not the connector default):
// headers arrive, then the upstream stalls mid-body, and the per-plugin deadline trips with
// an upstream_timeout marker. This is the case where a write may already have taken effect,
// so it must stay distinguishable.
func TestPerPluginReadTimeoutCarriesMarker(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		time.Sleep(3 * time.Second)
	}))
	defer srv.Close()

	store := mustStoreWithTimeout(t, "svc", srv.URL, "1s")
	h := HTTPHandler(newRegistry("svc", srv.URL), store, "")

	status, headers, body, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err == nil {
		t.Fatal("expected a read error on the per-plugin timeout")
	}
	if status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", status)
	}
	if got := marker(headers); got != ErrCodeUpstreamTimeout {
		t.Fatalf("%s = %q, want %q", ErrorHeader, got, ErrCodeUpstreamTimeout)
	}
	// Pin that the read branch (not the header-wait branch) produced this response.
	if body == nil || !strings.Contains(*body, "error reading upstream response") {
		t.Fatalf("body = %v, want the read-branch error body", body)
	}
}
