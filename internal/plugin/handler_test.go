package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

func newRegistry(name, url string) *Registry {
	return &Registry{plugins: map[string]string{name: url}}
}

func TestHandlerRoutesToPlugin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	}))
	defer srv.Close()

	reg := newRegistry("test-plugin", srv.URL)
	h := Handler(reg)

	msg := proto.DataMsg{
		Plugin: "test-plugin",
		Method: http.MethodGet,
		Path:   "/health",
	}

	status, body, err := h(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != 200 {
		t.Fatalf("expected 200, got %d", status)
	}
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandlerUnknownPlugin(t *testing.T) {
	reg := newRegistry("known", "http://localhost:9999")
	h := Handler(reg)

	msg := proto.DataMsg{
		Plugin: "unknown-plugin",
		Method: http.MethodGet,
		Path:   "/",
	}

	status, body, err := h(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != 404 {
		t.Fatalf("expected 404, got %d", status)
	}
	if !strings.Contains(string(body), "not found") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandlerTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(15 * time.Second)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	reg := newRegistry("slow-plugin", srv.URL)
	h := Handler(reg)

	msg := proto.DataMsg{
		Plugin: "slow-plugin",
		Method: http.MethodGet,
		Path:   "/",
	}

	status, _, err := h(context.Background(), msg)
	if err == nil {
		t.Errorf("expected error on timeout, got nil")
	}
	if status != 504 {
		t.Errorf("expected status 504, got %d", status)
	}
}

func TestHTTPHandlerInjectsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		w.Write([]byte(`ok`)) //nolint:errcheck
	}))
	defer srv.Close()

	reg := newRegistry("jenkins", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	if err := store.Set("jenkins", creds.Credential{
		BaseURL: srv.URL,
		Auth:    "bearer",
		Token:   "secret",
	}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	h := HTTPHandler(reg, store)

	msg := proto.HTTPRequestMsg{
		Type: "http_request", RequestID: "r1",
		ProviderKey: "jenkins", Method: "GET", Path: "/",
	}
	code, _, _, err := h(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("expected 'Bearer secret', got %q", gotAuth)
	}
}

func TestHTTPHandlerNoAuthPassthrough(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	reg := newRegistry("prom", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	if err := store.Set("prom", creds.Credential{BaseURL: srv.URL}); err != nil { // no auth
		t.Fatalf("store.Set: %v", err)
	}
	h := HTTPHandler(reg, store)

	msg := proto.HTTPRequestMsg{ProviderKey: "prom", Method: "GET", Path: "/"}
	code, _, _, err := h(context.Background(), msg)
	if err != nil || code != 200 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if gotAuth != "" {
		t.Fatalf("expected no Authorization header, got %q", gotAuth)
	}
}

func TestHTTPHandlerFallsBackToRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer srv.Close()

	// Plugin is in registry (env-var style) but NOT in creds store
	reg := newRegistry("prom", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	h := HTTPHandler(reg, store)

	msg := proto.HTTPRequestMsg{ProviderKey: "prom", Method: "GET", Path: "/"}
	code, _, _, err := h(context.Background(), msg)
	if err != nil || code != 204 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestHTTPHandlerUnknownProviderKey(t *testing.T) {
	reg := newRegistry("known", "http://localhost:9999")
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	h := HTTPHandler(reg, store)

	msg := proto.HTTPRequestMsg{ProviderKey: "unknown", Method: "GET", Path: "/"}
	code, _, _, err := h(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 404 {
		t.Fatalf("expected 404, got %d", code)
	}
}

// TestViaHeaderCannotBeOverwrittenByGateway verifies the security invariant that
// "Via: 1.1 mkonnect" is set AFTER forwarding gateway headers, so a gateway-supplied
// Via value cannot overwrite the connector's proxy marker.
func TestViaHeaderCannotBeOverwrittenByGateway(t *testing.T) {
	var gotVia string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVia = r.Header.Get("Via")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	reg := newRegistry("svc", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	h := HTTPHandler(reg, store)

	// Gateway supplies a Via header — connector must overwrite it.
	msg := proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: "GET", Path: "/",
		Headers: map[string]string{"Via": "1.1 gateway-supplied"},
	}
	code, _, _, err := h(context.Background(), msg)
	if err != nil || code != 200 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if gotVia != "1.1 mkonnect" {
		t.Fatalf("expected Via: 1.1 mkonnect, got %q — ordering broken", gotVia)
	}
}

// TestCredentialsNeverInHTTPResponseMsg verifies the security invariant that the
// bearer token never appears in any proto message sent over the bridge.
func TestCredentialsNeverInHTTPResponseMsg(t *testing.T) {
	const secret = "super-secret-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{}`)) //nolint:errcheck
	}))
	defer srv.Close()

	reg := newRegistry("svc", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	if err := store.Set("svc", creds.Credential{BaseURL: srv.URL, Auth: "bearer", Token: secret}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	h := HTTPHandler(reg, store)

	msg := proto.HTTPRequestMsg{ProviderKey: "svc", Method: "GET", Path: "/"}
	code, headers, body, err := h(context.Background(), msg)
	if err != nil || code != 200 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	// The token must not appear in headers or body returned to the caller
	// (which would be serialised into HTTPResponseMsg and sent over the bridge).
	for k, v := range headers {
		if strings.Contains(v, secret) {
			t.Fatalf("token found in response header %s: %q", k, v)
		}
	}
	if body != nil && strings.Contains(*body, secret) {
		t.Fatalf("token found in response body: %q", *body)
	}
}

// TestHTTPHandlerPathTraversalBlocked verifies that ".." segments in msg.Path cannot
// escape the configured base path prefix on the upstream service.
func TestHTTPHandlerPathTraversalBlocked(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// Base URL includes a path prefix that should confine requests.
	baseWithPrefix := srv.URL + "/api/v1"
	reg := newRegistry("svc", baseWithPrefix)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	if err := store.Set("svc", creds.Credential{BaseURL: baseWithPrefix}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	h := HTTPHandler(reg, store)

	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: "GET", Path: "/../../etc/passwd",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 400 {
		t.Fatalf("expected 400 for traversal attempt, got %d (called upstream: %v)", code, called)
	}
}

// TestHTTPHandlerHopByHopHeadersNotForwarded verifies that hop-by-hop headers
// (CL.TE smuggling vectors) are not forwarded to the upstream service.
func TestHTTPHandlerHopByHopHeadersNotForwarded(t *testing.T) {
	var gotTE, gotTransferEncoding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTE = r.Header.Get("Te")
		gotTransferEncoding = r.Header.Get("Transfer-Encoding")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	reg := newRegistry("svc", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	h := HTTPHandler(reg, store)

	msg := proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: "GET", Path: "/",
		Headers: map[string]string{
			"Te":                "trailers",
			"Transfer-Encoding": "chunked",
			"X-Custom":          "keep-me",
		},
	}
	code, _, _, err := h(context.Background(), msg)
	if err != nil || code != 200 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if gotTE != "" {
		t.Fatalf("Te header should not be forwarded, got %q", gotTE)
	}
	if gotTransferEncoding != "" {
		t.Fatalf("Transfer-Encoding header should not be forwarded, got %q", gotTransferEncoding)
	}
}

func TestHTTPHandlerResponseHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "value")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	reg := newRegistry("svc", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	h := HTTPHandler(reg, store)

	msg := proto.HTTPRequestMsg{ProviderKey: "svc", Method: "GET", Path: "/"}
	_, headers, _, err := h(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if headers["X-Custom"] != "value" {
		t.Fatalf("expected X-Custom response header, got %v", headers)
	}
}
