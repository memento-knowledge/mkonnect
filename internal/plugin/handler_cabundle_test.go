package plugin

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/proto"
	"github.com/memento-knowledge/mkonnect/internal/testca"
)

// newTLSServerWithTestCA starts an HTTPS test server presenting a leaf signed by a
// freshly generated throwaway CA, and writes that CA to a bundle file in t.TempDir().
// It returns the server and the bundle path. No key material is persisted beyond the
// test's temp dir.
func newTLSServerWithTestCA(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	caPEM, leaf, err := testca.GenerateCA("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	srv := httptest.NewUnstartedServer(handler)
	// Set Certificates before StartTLS: httptest installs its own cert only when the
	// config has none.
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	bundle := filepath.Join(t.TempDir(), "ca-bundle.pem")
	if err := os.WriteFile(bundle, caPEM, 0o644); err != nil {
		t.Fatalf("write CA bundle: %v", err)
	}
	return srv, bundle
}

// TestHTTPHandlerCABundleUnauthenticated covers the acceptance criterion for a plugin
// with no local credential (global CA_BUNDLE default): the request fails without the
// bundle and succeeds with it.
func TestHTTPHandlerCABundleUnauthenticated(t *testing.T) {
	srv, bundle := newTLSServerWithTestCA(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	reg := newRegistry("svc", srv.URL)

	// Without the bundle, the internal CA is untrusted and the TLS handshake fails.
	status, headers, _, err := HTTPHandler(reg, mustStore(t), "")(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err == nil {
		t.Error("expected a TLS error without the CA bundle")
	}
	if status != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504 without the CA bundle", status)
	}
	if got := headers[ErrorHeader]; got != ErrCodeUpstreamError {
		t.Errorf("error marker = %q, want %q", got, ErrCodeUpstreamError)
	}

	// With the global default bundle, verification succeeds.
	status, _, _, err = HTTPHandler(reg, mustStore(t), bundle)(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err != nil {
		t.Fatalf("request with CA bundle failed: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 with the CA bundle", status)
	}
}

// TestHTTPHandlerCABundleCredentialed covers a credentialed plugin (direct, proxy-
// bypassing transport) with a per-plugin ca_bundle: fails without, succeeds with, and
// the local credential still reaches the upstream.
func TestHTTPHandlerCABundleCredentialed(t *testing.T) {
	var gotAuth string
	srv, bundle := newTLSServerWithTestCA(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))

	mkStore := func(t *testing.T, caBundle string) *creds.Store {
		s := mustStore(t)
		if err := s.Set("svc", creds.Credential{
			BaseURL:  srv.URL,
			Auth:     "bearer",
			Token:    "long-token-value-123",
			CABundle: caBundle,
		}); err != nil {
			t.Fatalf("store.Set: %v", err)
		}
		return s
	}

	// Without a bundle: TLS handshake fails.
	status, _, _, err := HTTPHandler(newRegistry("svc", srv.URL), mkStore(t, ""), "")(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err == nil || status != http.StatusGatewayTimeout {
		t.Errorf("expected 504 + error without the CA bundle, got status=%d err=%v", status, err)
	}

	// With the per-plugin bundle: succeeds and the injected credential is present.
	gotAuth = ""
	status, _, _, err = HTTPHandler(newRegistry("svc", srv.URL), mkStore(t, bundle), "")(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err != nil {
		t.Fatalf("request with CA bundle failed: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if gotAuth != "Bearer long-token-value-123" {
		t.Errorf("Authorization = %q, want the injected local credential", gotAuth)
	}
}

// TestHTTPHandlerCABundleFailsClosed verifies that a configured-but-unloadable bundle
// is rejected before the request is forwarded (no silent fallback to the system store).
func TestHTTPHandlerCABundleFailsClosed(t *testing.T) {
	var hit bool
	srv, _ := newTLSServerWithTestCA(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))

	store := mustStore(t)
	if err := store.Set("svc", creds.Credential{
		BaseURL:  srv.URL,
		CABundle: filepath.Join(t.TempDir(), "missing.pem"),
	}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}

	status, headers, _, err := HTTPHandler(newRegistry("svc", srv.URL), store, "")(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err != nil {
		t.Errorf("fail-closed is a synthesised response, want nil transport err, got %v", err)
	}
	if status != 500 {
		t.Errorf("status = %d, want 500 for an unloadable CA bundle", status)
	}
	if got := headers[ErrorHeader]; got != ErrCodeInvalidCABundle {
		t.Errorf("error marker = %q, want %q", got, ErrCodeInvalidCABundle)
	}
	if hit {
		t.Error("request must not reach the upstream when the CA bundle cannot be loaded")
	}
}

// TestCAClientCacheReuseAndInvalidation checks that the cache returns the same client
// for repeated lookups under one generation and rebuilds when the generation changes.
func TestCAClientCacheReuseAndInvalidation(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "ca-bundle.pem")
	caPEM, _, err := testca.GenerateCA("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	if err := os.WriteFile(bundle, caPEM, 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	cache := &caClientCache{newClient: newUpstreamClient}
	c1, err := cache.clientFor(true, bundle, 1)
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	c2, err := cache.clientFor(true, bundle, 1)
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	if c1 != c2 {
		t.Error("cache must return the same client for repeated lookups under one generation")
	}
	c3, err := cache.clientFor(true, bundle, 2)
	if err != nil {
		t.Fatalf("clientFor after generation bump: %v", err)
	}
	if c3 == c1 {
		t.Error("cache must rebuild the client when the generation changes")
	}
}
