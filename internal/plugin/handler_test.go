package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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
		Token:   "bearer-token-value",
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
	if gotAuth != "Bearer bearer-token-value" {
		t.Fatalf("expected Bearer authorization, got %q", gotAuth)
	}
}

// captureHeaders returns an httptest server that records the inbound request headers.
func captureHeaders(t *testing.T, got *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
}

func TestHTTPHandlerStripsCallerCredentialHeadersWhenLocalCredentialApplied(t *testing.T) {
	var got http.Header
	srv := captureHeaders(t, &got)
	defer srv.Close()

	reg := newRegistry("gitscm", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	if err := store.Set("gitscm", creds.Credential{
		BaseURL:      srv.URL,
		Auth:         "bearer",
		Token:        "local-token-value-abc",
		StripHeaders: []string{"X-Custom-Auth"}, // configured extra
	}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	h := HTTPHandler(reg, store)

	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "gitscm", Method: http.MethodPost, Path: "/api",
		Headers: map[string]string{
			"Authorization": "Bearer attacker",
			"aUtHoRiZaTiOn": "attacker-odd-case", // casing must not bypass the strip
			"PRIVATE-TOKEN": "p",
			"JOB-TOKEN":     "j",
			"Cookie":        "session=x",
			"X-Api-Key":     "k",
			"X-Auth-Token":  "a",
			"Private_token": "u", // underscore spelling of a default
			"X-Custom-Auth": "c", // the configured extra
			"Content-Type":  "application/json",
			"Accept":        "application/json",
		},
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}

	if got.Get("Authorization") != "Bearer local-token-value-abc" {
		t.Fatalf("Authorization reaching upstream = %q, want only the local credential", got.Get("Authorization"))
	}
	for _, name := range []string{"Private-Token", "Job-Token", "Cookie", "X-Api-Key", "X-Auth-Token", "X-Custom-Auth", "Private_token"} {
		if vs := got.Values(name); len(vs) != 0 {
			t.Errorf("caller credential header %s leaked to upstream (value redacted, %d value(s))", name, len(vs))
		}
	}
	if got.Get("Content-Type") != "application/json" || got.Get("Accept") != "application/json" {
		t.Errorf("non-credential headers not forwarded: Content-Type=%q Accept=%q", got.Get("Content-Type"), got.Get("Accept"))
	}
}

func TestHTTPHandlerStripsCallerCredentialHeadersForBasicAuth(t *testing.T) {
	var got http.Header
	srv := captureHeaders(t, &got)
	defer srv.Close()

	reg := newRegistry("build-service", srv.URL)
	store := mustStoreBasicCredential(t, "build-service", srv.URL, "local-user", "basic-token-value-123")
	h := HTTPHandler(reg, store)

	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "build-service", Method: http.MethodGet, Path: "/",
		Headers: map[string]string{"PRIVATE-TOKEN": "p", "Cookie": "s=x", "Proxy-Authorization": "Basic zzz"},
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("local-user:basic-token-value-123"))
	if got.Get("Authorization") != expected {
		t.Fatalf("Authorization = %q, want the local basic credential", got.Get("Authorization"))
	}
	for _, name := range []string{"Private-Token", "Cookie", "Proxy-Authorization"} {
		if vs := got.Values(name); len(vs) != 0 {
			t.Errorf("caller credential header %s leaked to upstream (%d value(s))", name, len(vs))
		}
	}
}

func TestHTTPHandlerKeepsCallerHeadersWithoutLocalCredential(t *testing.T) {
	var got http.Header
	srv := captureHeaders(t, &got)
	defer srv.Close()

	// Registry-only plugin: no local credential, so caller-supplied headers (platform-held
	// credentials) are forwarded unchanged — the issue's "keep current behaviour" case.
	reg := newRegistry("open", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	h := HTTPHandler(reg, store)

	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "open", Method: http.MethodGet, Path: "/",
		Headers: map[string]string{"Authorization": "Bearer bridge", "PRIVATE-TOKEN": "p"},
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}
	if got.Get("Authorization") != "Bearer bridge" {
		t.Errorf("Authorization = %q, want the caller header forwarded unchanged", got.Get("Authorization"))
	}
	if got.Get("Private-Token") != "p" {
		t.Errorf("Private-Token = %q, want the caller header forwarded unchanged", got.Get("Private-Token"))
	}
}

func TestHTTPHandlerStripsExplicitHeadersWithoutLocalCredential(t *testing.T) {
	var got http.Header
	srv := captureHeaders(t, &got)
	defer srv.Close()

	// A plugin entry with no auth but an explicit StripHeaders list: the explicit header is
	// dropped, while the credential-header defaults are NOT applied (no local credential), so
	// a caller Authorization still passes through.
	reg := newRegistry("open", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	if err := store.Set("open", creds.Credential{BaseURL: srv.URL, StripHeaders: []string{"X-Custom-Auth"}}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	h := HTTPHandler(reg, store)

	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "open", Method: http.MethodGet, Path: "/",
		Headers: map[string]string{"Authorization": "Bearer bridge", "X-Custom-Auth": "c"},
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}
	if vs := got.Values("X-Custom-Auth"); len(vs) != 0 {
		t.Errorf("explicitly stripped header leaked to upstream (%d value(s))", len(vs))
	}
	if got.Get("Authorization") != "Bearer bridge" {
		t.Errorf("Authorization = %q, want the caller header forwarded (defaults must not apply without a local credential)", got.Get("Authorization"))
	}
}

func TestHTTPHandlerInjectsBasicCredentialsOnlyForConfiguredPlugin(t *testing.T) {
	const username = "local-user"
	const token = "basic-token-value-123"
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

func TestHTTPHandlerRejectsCredentialBearingBaseURL(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	const secret = "api-token"
	urls := []string{
		strings.Replace(upstream.URL, "://", "://local-user:"+secret+"@", 1),
		upstream.URL + "?access_token=" + secret,
		upstream.URL + "#access_token=" + secret,
	}

	for _, baseURL := range urls {
		t.Run(baseURL, func(t *testing.T) {
			store := mustStore(t)
			if err := store.Set("svc", creds.Credential{BaseURL: baseURL}); err != nil {
				t.Fatalf("store.Set: %v", err)
			}

			code, _, body, err := HTTPHandler(newRegistry("svc", upstream.URL), store)(context.Background(), proto.HTTPRequestMsg{
				ProviderKey: "svc", Method: http.MethodGet, Path: "/",
			})
			if err != nil || code != http.StatusInternalServerError {
				t.Fatalf("code=%d err=%v, want 500 without an error", code, err)
			}
			if body != nil && strings.Contains(*body, secret) {
				t.Fatalf("URL credential must not be included in bridge response: %q", *body)
			}
		})
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

func TestHTTPHandlerBlocksBridgeResponseContainingLocalCredentials(t *testing.T) {
	tests := []struct {
		name      string
		store     func(t *testing.T, url string) *creds.Store
		forbidden []string
	}{
		{
			name: "bearer",
			store: func(t *testing.T, url string) *creds.Store {
				store := mustStore(t)
				if err := store.Set("svc", creds.Credential{BaseURL: url, Auth: "bearer", Token: "bearer-secret-value"}); err != nil {
					t.Fatalf("store.Set: %v", err)
				}
				return store
			},
			forbidden: []string{"bearer-secret-value", "Bearer bearer-secret-value"},
		},
		{
			name: "basic",
			store: func(t *testing.T, url string) *creds.Store {
				return mustStoreBasicCredential(t, "svc", url, "local-user", "basic-token-value-123")
			},
			forbidden: []string{
				"local-user",
				"basic-token-value-123",
				"Basic " + base64.StdEncoding.EncodeToString([]byte("local-user:basic-token-value-123")),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authorization := r.Header.Get("Authorization")
				username, token, _ := r.BasicAuth()
				w.Header().Set("Authorization", authorization)
				w.Header().Set("X-Reflected-Authorization", authorization)
				w.Header().Set("X-Reflected-Username", username)
				w.Header().Set("X-Reflected-Token", token)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("authorization=" + authorization + " username=" + username + " token=" + token))
			}))
			defer srv.Close()

			h := HTTPHandler(newRegistry("svc", srv.URL), tt.store(t, srv.URL))
			code, headers, body, err := h(context.Background(), proto.HTTPRequestMsg{
				ProviderKey: "svc", Method: http.MethodGet, Path: "/",
			})
			if err != nil || code != http.StatusBadGateway {
				t.Fatalf("code=%d err=%v, want 502 without an error", code, err)
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

func TestHTTPHandlerBlocksBridgeResponseContainingEncodedLocalCredential(t *testing.T) {
	const token = "token/with-slash"
	authorization := "Bearer " + token
	escapedAuthorization := url.QueryEscape(authorization)
	encodedAuthorization := base64.StdEncoding.EncodeToString([]byte(authorization))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Reflected-Authorization", escapedAuthorization)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(encodedAuthorization))
	}))
	defer srv.Close()

	store := mustStore(t)
	if err := store.Set("svc", creds.Credential{BaseURL: srv.URL, Auth: "bearer", Token: token}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}

	code, headers, body, err := HTTPHandler(newRegistry("svc", srv.URL), store)(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err != nil || code != http.StatusBadGateway {
		t.Fatalf("code=%d err=%v, want 502 without an error", code, err)
	}
	message, err := json.Marshal(proto.HTTPResponseMsg{Type: "http_response", StatusCode: code, Headers: headers, Body: body})
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	for _, value := range []string{token, authorization, escapedAuthorization, encodedAuthorization} {
		if strings.Contains(string(message), value) {
			t.Fatalf("encoded local credential leaked into bridge response: %q", value)
		}
	}
}

func TestHTTPHandlerBlocksBridgeResponseContainingRawLocalToken(t *testing.T) {
	const token = "long-token-value-123"
	tests := []struct {
		name  string
		store func(t *testing.T, url string) *creds.Store
	}{
		{
			name: "bearer",
			store: func(t *testing.T, url string) *creds.Store {
				store := mustStore(t)
				if err := store.Set("svc", creds.Credential{BaseURL: url, Auth: "bearer", Token: token}); err != nil {
					t.Fatalf("store.Set: %v", err)
				}
				return store
			},
		},
		{
			name: "basic",
			store: func(t *testing.T, url string) *creds.Store {
				return mustStoreBasicCredential(t, "svc", url, "local-user", token)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Reflected-Token", token)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(token))
			}))
			defer srv.Close()

			code, headers, body, err := HTTPHandler(newRegistry("svc", srv.URL), tt.store(t, srv.URL))(context.Background(), proto.HTTPRequestMsg{
				ProviderKey: "svc", Method: http.MethodGet, Path: "/",
			})
			if err != nil || code != http.StatusBadGateway {
				t.Fatalf("code=%d err=%v, want 502 without an error", code, err)
			}
			message, err := json.Marshal(proto.HTTPResponseMsg{Type: "http_response", StatusCode: code, Headers: headers, Body: body})
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}
			if strings.Contains(string(message), token) {
				t.Fatal("raw local token leaked into bridge response")
			}
		})
	}
}

func TestHTTPHandlerPreservesResponseWithoutCredentialReflection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Note", "ordinary response")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":42}`))
	}))
	defer srv.Close()

	h := HTTPHandler(newRegistry("svc", srv.URL), mustStoreBasicCredential(t, "svc", srv.URL, "id", "long-token-value-123"))
	code, headers, body, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if headers["X-Note"] != "ordinary response" {
		t.Fatalf("response header was altered: %q", headers["X-Note"])
	}
	if body == nil || *body != `{"id":42}` {
		t.Fatalf("response body was altered: %v", body)
	}
}

func TestHTTPHandlerPreservesResponseWithoutFullAuthorizationReflection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Note", "id")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":42}`))
	}))
	defer srv.Close()

	store := mustStore(t)
	if err := store.Set("svc", creds.Credential{BaseURL: srv.URL, Auth: "bearer", Token: "long-token-value-123"}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	code, headers, body, err := HTTPHandler(newRegistry("svc", srv.URL), store)(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if headers["X-Note"] != "id" {
		t.Fatalf("response header was altered: %q", headers["X-Note"])
	}
	if body == nil || *body != `{"id":42}` {
		t.Fatalf("response body was altered: %v", body)
	}
}

func TestHTTPHandlerDoesNotUseAmbientProxy(t *testing.T) {
	if os.Getenv("MKONNECT_PROXY_TEST_CHILD") == "1" {
		store := mustStore(t)
		if err := store.Set("svc", creds.Credential{
			BaseURL: "http://example.invalid",
			Auth:    "bearer",
			Token:   "local-token-value-123",
		}); err != nil {
			t.Fatalf("store.Set: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		_, _, _, _ = HTTPHandler(newRegistry("svc", "http://example.invalid"), store)(ctx, proto.HTTPRequestMsg{
			ProviderKey: "svc", Method: http.MethodGet, Path: "/",
		})
		return
	}

	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestHTTPHandlerDoesNotUseAmbientProxy$", "-test.v")
	cmd.Env = proxyTestEnvironment(proxy.URL)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("proxy child failed: %v\n%s", err, output)
	}
	if proxyCalls.Load() != 0 {
		t.Fatal("HTTP handler sent a local credential-bearing request through HTTP_PROXY")
	}
}

func TestHTTPHandlerUsesAmbientProxyWithoutLocalCredentials(t *testing.T) {
	if os.Getenv("MKONNECT_PROXY_TEST_CHILD") == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		code, _, _, err := HTTPHandler(newRegistry("svc", "http://example.invalid"), mustStore(t))(ctx, proto.HTTPRequestMsg{
			ProviderKey: "svc", Method: http.MethodGet, Path: "/",
		})
		if err != nil || code != http.StatusOK {
			t.Fatalf("code=%d err=%v, want proxy response", code, err)
		}
		return
	}

	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestHTTPHandlerUsesAmbientProxyWithoutLocalCredentials$", "-test.v")
	cmd.Env = proxyTestEnvironment(proxy.URL)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("proxy child failed: %v\n%s", err, output)
	}
	if proxyCalls.Load() != 1 {
		t.Fatalf("proxy calls = %d, want 1 for unauthenticated plugin", proxyCalls.Load())
	}
}

func proxyTestEnvironment(proxyURL string) []string {
	env := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "ALL_PROXY", "HTTP_PROXY", "HTTPS_PROXY", "MKONNECT_PROXY_TEST_CHILD", "NO_PROXY":
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"HTTP_PROXY="+proxyURL,
		"HTTPS_PROXY=",
		"ALL_PROXY=",
		"NO_PROXY=",
		"MKONNECT_PROXY_TEST_CHILD=1",
	)
}

// TestHTTPHandlerPathTraversalBlocked verifies that ".." segments in msg.Path — whether
// literal, percent-encoded (%2e%2e), or reached across a percent-encoded slash (..%2f) —
// cannot escape the configured base path prefix. Confinement is enforced on the decoded,
// cleaned path, so preserving the escaped form for forwarding (see
// TestHTTPHandlerPreservesEscapedPath) does not weaken it: a blocked request is rejected
// before it is forwarded and the upstream is never contacted.
func TestHTTPHandlerPathTraversalBlocked(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
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
		"/../../etc/passwd",   // literal dots
		"/%2e%2e/etc/passwd",  // percent-encoded dot-dot
		"/%2e%2e/%2e%2e/etc",  // repeated percent-encoded dot-dot
		"/..%2fsecret",        // dot-dot across a percent-encoded slash
		"/%2e%2e%2fsecret",    // fully percent-encoded ../secret
		"/..%2f..%2fetc%2fpw", // repeated, all separators encoded
	}
	for _, p := range cases {
		called = false
		code, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
			ProviderKey: "svc", Method: "GET", Path: p,
		})
		if err != nil {
			t.Fatalf("path %q: unexpected error: %v", p, err)
		}
		if code != 400 {
			t.Fatalf("path %q: expected 400, got %d", p, code)
		}
		if headers[ErrorHeader] != ErrCodePathBlocked {
			t.Fatalf("path %q: expected %s=%q, got %q", p, ErrorHeader, ErrCodePathBlocked, headers[ErrorHeader])
		}
		if called {
			t.Fatalf("path %q: upstream was contacted for a blocked traversal", p)
		}
	}
}

// TestHTTPHandlerPreservesEscapedPath verifies that the caller's escaped path form is
// forwarded verbatim — an escaped slash (%2F) and an escaped percent (%25) reach the
// upstream unchanged — while a path that normalises differently (traversal, "." / ".."
// segments, "//", or a value mixing a raw escape with an unencoded reserved byte) falls
// back to the decoded, cleaned path. The escaped form is kept only when it single-decodes
// to exactly the confined path, so forwarding it cannot deliver a different path than the
// one the confinement check approved.
func TestHTTPHandlerPreservesEscapedPath(t *testing.T) {
	var sentURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentURI = r.RequestURI // raw request-target, before net/http decodes it
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cases := []struct {
		name    string
		base    string // appended to srv.URL
		path    string
		wantURI string
	}{
		{"escaped slash survives", "", "/group%2Fsub/repo", "/group%2Fsub/repo"},
		{"escaped percent survives", "", "/a%25b/c", "/a%25b/c"},
		{"lower-case escaped slash preserved verbatim", "", "/group%2fsub", "/group%2fsub"},
		{"escaped slash under base prefix", "/api/v1", "/group%2Fsub", "/api/v1/group%2Fsub"},
		{"escaped slash under base prefix with trailing slash", "/api/v1/", "/group%2Fsub", "/api/v1/group%2Fsub"},
		{"escaped slash in base path preserved verbatim", "/a%2Fb", "/x", "/a%2Fb/x"},
		{"plain path unchanged", "", "/plain/path", "/plain/path"},
		{"query preserved alongside escaped slash", "", "/group%2Fsub?page=2", "/group%2Fsub?page=2"},
		// Fallbacks to the decoded, cleaned path (escaped slash does not survive):
		{"dot segment normalised", "", "/a/./b", "/a/b"},
		{"traversal across escaped slash normalised", "", "/foo%2F..%2Fbar", "/bar"},
		{"escaped double slash collapses under base", "/api/v1", "/x%2F%2Fy", "/api/v1/x/y"},
		{"escaped leading double slash is not an authority", "", "/%2F%2Fevil.com", "/evil.com"},
		{"raw escape mixed with unencoded space falls back", "", "/a%2Fb c", "/a/b%20c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := srv.URL + tc.base
			reg := newRegistry("svc", base)
			store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
			if err != nil {
				t.Fatalf("creds.New: %v", err)
			}
			if err := store.Set("svc", creds.Credential{BaseURL: base}); err != nil {
				t.Fatalf("store.Set: %v", err)
			}
			h := HTTPHandler(reg, store)

			sentURI = ""
			code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
				ProviderKey: "svc", Method: "GET", Path: tc.path,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if code != 200 {
				t.Fatalf("code = %d, want 200", code)
			}
			if sentURI != tc.wantURI {
				t.Fatalf("upstream request-target = %q, want %q", sentURI, tc.wantURI)
			}
		})
	}
}

// TestHTTPHandlerRejectsAuthoritySmuggling verifies that a caller path beginning with
// "//" — which url.Parse reads as a network-path reference carrying an authority — cannot
// redirect the request to a different host. The path still begins with "/", so the
// HasPrefix("/") guard passes; the host check after ResolveReference is what rejects it.
// Preserving the escaped path form does not weaken this: the check runs on the decoded
// target before any escaped form is re-attached. (The escaped form "/%2F%2Fevil.example"
// is covered by TestHTTPHandlerPreservesEscapedPath: it decodes to a same-host path and
// cannot name an authority at all.)
func TestHTTPHandlerRejectsAuthoritySmuggling(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer srv.Close()

	reg := newRegistry("svc", srv.URL)
	store := mustStore(t)
	if err := store.Set("svc", creds.Credential{BaseURL: srv.URL}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	h := HTTPHandler(reg, store)

	for _, p := range []string{
		"//evil.example/x",      // different host
		"//evil.example:8080/x", // different host and port
	} {
		called = false
		code, headers, _, err := h(context.Background(), proto.HTTPRequestMsg{
			ProviderKey: "svc", Method: "GET", Path: p,
		})
		if err != nil {
			t.Fatalf("path %q: unexpected error: %v", p, err)
		}
		if code != 400 || headers[ErrorHeader] != ErrCodeInvalidPath {
			t.Fatalf("path %q: expected 400 invalid_path, got code=%d marker=%q", p, code, headers[ErrorHeader])
		}
		if called {
			t.Fatalf("path %q: a request was sent to a foreign host", p)
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
