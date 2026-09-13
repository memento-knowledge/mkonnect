package plugin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

	status, body, err := h(msg)
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

	status, body, err := h(msg)
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

	status, _, err := h(msg)
	// Expect either an error or a 504 gateway timeout.
	if err == nil && status != 504 {
		t.Fatalf("expected timeout error or 504, got status=%d err=%v", status, err)
	}
}
