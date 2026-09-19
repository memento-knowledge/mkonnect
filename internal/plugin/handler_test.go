package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// mustStore returns an empty credential store backed by a temp file.
func mustStore(t *testing.T) *creds.Store {
	t.Helper()
	s, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	return s
}

func mustStoreBasicCredential(t *testing.T, plugin, baseURL, username, token string) *creds.Store {
	t.Helper()
	store := mustStore(t)
	if err := store.Set(plugin, creds.Credential{
		BaseURL:  baseURL,
		Auth:     "basic",
		Username: username,
		Token:    token,
	}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	return store
}

func TestHTTPHandlerUpstreamTimeout(t *testing.T) {
	// Shorten the per-request timeout so the slow upstream trips it quickly.
	orig := upstreamTimeout
	upstreamTimeout = 20 * time.Millisecond
	defer func() { upstreamTimeout = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	reg := newRegistry("slow", srv.URL)
	h := HTTPHandler(reg, mustStore(t))

	status, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "slow", Method: http.MethodGet, Path: "/",
	})
	if err == nil {
		t.Error("expected a transport error on upstream timeout, got nil")
	}
	if status != 504 {
		t.Errorf("status = %d, want 504", status)
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

func TestHTTPHandlerInjectsBasicCredentialsOnlyForConfiguredPlugin(t *testing.T) {
	const username = "local-user"
	const token = "api-token"
	expectedBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+token))

	gotAuth := make(map[string]string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth[r.URL.Path] = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	reg := &Registry{plugins: map[string]string{
		"build-service": srv.URL,
		"metrics":       srv.URL,
	}}
	store := mustStoreBasicCredential(t, "build-service", srv.URL, username, token)
	h := HTTPHandler(reg, store)

	for _, providerKey := range []string{"build-service", "metrics"} {
		code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
			ProviderKey: providerKey,
			Method:      http.MethodGet,
			Path:        "/" + providerKey,
			Headers:     map[string]string{"Authorization": "Bearer bridge-token"},
		})
		if err != nil || code != http.StatusOK {
			t.Fatalf("request for %q: code=%d err=%v", providerKey, code, err)
		}
	}

	if gotAuth["/build-service"] != expectedBasic {
		t.Fatalf("configured plugin Authorization = %q, want %q", gotAuth["/build-service"], expectedBasic)
	}
	if gotAuth["/metrics"] != "Bearer bridge-token" {
		t.Fatalf("unconfigured plugin Authorization = %q, want bridge header unchanged", gotAuth["/metrics"])
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

func TestHTTPHandlerDoesNotReturnInjectedAuthorizationHeader(t *testing.T) {
	tests := []struct {
		name      string
		store     func(t *testing.T, url string) *creds.Store
		forbidden []string
	}{
		{
			name: "bearer",
			store: func(t *testing.T, url string) *creds.Store {
				store := mustStore(t)
				if err := store.Set("svc", creds.Credential{BaseURL: url, Auth: "bearer", Token: "bearer-secret"}); err != nil {
					t.Fatalf("store.Set: %v", err)
				}
				return store
			},
			forbidden: []string{"bearer-secret", "Bearer bearer-secret"},
		},
		{
			name: "basic",
			store: func(t *testing.T, url string) *creds.Store {
				return mustStoreBasicCredential(t, "svc", url, "local-user", "api-token")
			},
			forbidden: []string{
				"local-user",
				"api-token",
				"Basic " + base64.StdEncoding.EncodeToString([]byte("local-user:api-token")),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Authorization", r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			h := HTTPHandler(newRegistry("svc", srv.URL), tt.store(t, srv.URL))
			code, headers, body, err := h(context.Background(), proto.HTTPRequestMsg{
				ProviderKey: "svc", Method: http.MethodGet, Path: "/",
			})
			if err != nil || code != http.StatusOK {
				t.Fatalf("code=%d err=%v", code, err)
			}

			message, err := json.Marshal(proto.HTTPResponseMsg{
				Type: "http_response", RequestID: "request", StatusCode: code, Headers: headers, Body: body,
			})
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}
			for _, value := range tt.forbidden {
				if strings.Contains(string(message), value) {
					t.Fatalf("local credential leaked into bridge response: %q", value)
				}
			}
		})
	}
}

// TestHTTPHandlerPathTraversalBlocked verifies that ".." segments in msg.Path, including
// percent-encoded variants (%2e%2e), cannot escape the configured base path prefix.
func TestHTTPHandlerPathTraversalBlocked(t *testing.T) {
	var calledPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPath = r.URL.Path
		w.WriteHeader(200)
	}))
	defer srv.Close()

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

	cases := []string{
		"/../../etc/passwd",  // literal dots
		"/%2e%2e/etc/passwd", // percent-encoded single dot-dot
		"/%2e%2e/%2e%2e/etc", // double-encoded
	}
	for _, p := range cases {
		code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
			ProviderKey: "svc", Method: "GET", Path: p,
		})
		if err != nil {
			t.Fatalf("path %q: unexpected error: %v", p, err)
		}
		if code != 400 {
			t.Fatalf("path %q: expected 400, got %d (upstream saw path: %q)", p, code, calledPath)
		}
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
