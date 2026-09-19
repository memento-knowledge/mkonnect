// internal/creds/store_test.go
package creds_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/creds"
)

func TestStoreRejectsShortPersistedTokensWithoutLeakingThem(t *testing.T) {
	tests := []struct {
		name       string
		credential creds.Credential
	}{
		{
			name:       "bearer",
			credential: creds.Credential{BaseURL: "https://service.internal", Auth: "bearer", Token: "short-token"},
		},
		{
			name:       "basic",
			credential: creds.Credential{BaseURL: "https://service.internal", Auth: "basic", Username: "local-user", Token: "short-token"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.json")
			data, err := json.Marshal(map[string]creds.Credential{"service": tt.credential})
			if err != nil {
				t.Fatalf("marshal credentials: %v", err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatalf("write credentials: %v", err)
			}

			_, err = creds.New(path)
			if err == nil {
				t.Fatal("expected persisted short token to be rejected")
			}
			if strings.Contains(err.Error(), "short-token") {
				t.Fatal("persisted token must not be repeated in an error")
			}
		})
	}
}

func TestStoreSetAndGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, err := creds.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Set("jenkins", creds.Credential{BaseURL: "http://jenkins:8080", Auth: "bearer", Token: "test-token-value-123"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok := s.Get("jenkins")
	if !ok {
		t.Fatal("Get: not found")
	}
	if got.BaseURL != "http://jenkins:8080" || got.Token != "test-token-value-123" {
		t.Fatalf("unexpected credential: %+v", got)
	}
}

func TestStorePersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, err := creds.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Set("prom", creds.Credential{BaseURL: "http://prom:9090"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	s2, err := creds.New(path)
	if err != nil {
		t.Fatalf("New after write: %v", err)
	}
	got, ok := s2.Get("prom")
	if !ok || got.BaseURL != "http://prom:9090" {
		t.Fatalf("credential not persisted: %+v, ok=%v", got, ok)
	}
}

func TestStoreRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, err := creds.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Set("jenkins", creds.Credential{BaseURL: "http://jenkins:8080"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Remove("jenkins"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := s.Get("jenkins"); ok {
		t.Fatal("expected credential to be removed")
	}
}

func TestStoreReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, err := creds.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Set("a", creds.Credential{BaseURL: "http://a:1"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Simulate external write (another CLI invocation)
	s2, err := creds.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s2.Set("b", creds.Credential{BaseURL: "http://b:2"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := s.Get("b"); !ok {
		t.Fatal("expected b after reload")
	}
}

func TestStoreNewMissingFileOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, err := creds.New(path) // file does not exist yet
	if err != nil {
		t.Fatalf("New with missing file should not error: %v", err)
	}
	if list := s.List(); len(list) != 0 {
		t.Fatalf("expected empty list, got %d entries", len(list))
	}
}
