package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/creds"
)

func TestProbePluginRejectsCredentialBaseURLWithUserinfo(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	const secret = "api-token"
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	if err := store.Set("svc", creds.Credential{
		BaseURL: strings.Replace(upstream.URL, "://", "://local-user:"+secret+"@", 1),
	}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}

	result := (&Client{credsStore: store}).probePlugin(context.Background(), "svc")
	if result.Status != "unreachable" {
		t.Fatalf("status = %q, want unreachable", result.Status)
	}
	if strings.Contains(result.Diagnostic, secret) {
		t.Fatalf("URL password must not be included in diagnostic: %q", result.Diagnostic)
	}
}
