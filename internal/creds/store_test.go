// internal/creds/store_test.go
package creds_test

import (
	"path/filepath"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/creds"
)

func TestStoreSetAndGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, err := creds.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Set("jenkins", creds.Credential{BaseURL: "http://jenkins:8080", Auth: "bearer", Token: "tok"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok := s.Get("jenkins")
	if !ok {
		t.Fatal("Get: not found")
	}
	if got.BaseURL != "http://jenkins:8080" || got.Token != "tok" {
		t.Fatalf("unexpected credential: %+v", got)
	}
}

func TestStorePersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, _ := creds.New(path)
	_ = s.Set("prom", creds.Credential{BaseURL: "http://prom:9090"})

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
	s, _ := creds.New(path)
	_ = s.Set("jenkins", creds.Credential{BaseURL: "http://jenkins:8080"})
	if err := s.Remove("jenkins"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := s.Get("jenkins"); ok {
		t.Fatal("expected credential to be removed")
	}
}

func TestStoreReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, _ := creds.New(path)
	_ = s.Set("a", creds.Credential{BaseURL: "http://a:1"})

	// Simulate external write (another CLI invocation)
	s2, _ := creds.New(path)
	_ = s2.Set("b", creds.Credential{BaseURL: "http://b:2"})

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
