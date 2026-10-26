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

func TestStoreNewAcceptsGroupReadableFile(t *testing.T) {
	// A credentials file delivered group-readable (as a Kubernetes Secret is,
	// with defaultMode 0440/0640) must be accepted. On a writable volume New
	// also tightens it back to owner-only.
	path := filepath.Join(t.TempDir(), "credentials.json")
	data, err := json.Marshal(map[string]creds.Credential{
		"jenkins": {BaseURL: "http://jenkins:8080", Auth: "bearer", Token: "test-token-value-123"},
	})
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatalf("write credentials: %v", err)
	}

	s, err := creds.New(path)
	if err != nil {
		t.Fatalf("New with group-readable file: %v", err)
	}
	if _, ok := s.Get("jenkins"); !ok {
		t.Fatal("expected credential to load")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected writable credentials file to be tightened to 0600, got %#o", perm)
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

func TestStoreStripHeadersRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, err := creds.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Set("gitscm", creds.Credential{
		BaseURL:      "http://scm.internal",
		Auth:         "bearer",
		Token:        "token-value-abcdef12",
		StripHeaders: []string{"X-Custom-Auth", "X-Gitlab-Token"},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Reload from disk in a fresh store to confirm the field persists.
	s2, err := creds.New(path)
	if err != nil {
		t.Fatalf("New after write: %v", err)
	}
	got, ok := s2.Get("gitscm")
	if !ok {
		t.Fatal("Get: not found")
	}
	if len(got.StripHeaders) != 2 || got.StripHeaders[0] != "X-Custom-Auth" || got.StripHeaders[1] != "X-Gitlab-Token" {
		t.Fatalf("strip_headers not persisted: %v", got.StripHeaders)
	}
}

func TestStoreCABundleRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, err := creds.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Set("gitscm", creds.Credential{
		BaseURL:  "https://scm.internal",
		Auth:     "bearer",
		Token:    "token-value-abcdef12",
		CABundle: "/etc/mkonnect-ca/ca-bundle.pem",
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	s2, err := creds.New(path)
	if err != nil {
		t.Fatalf("New after write: %v", err)
	}
	got, ok := s2.Get("gitscm")
	if !ok {
		t.Fatal("Get: not found")
	}
	if got.CABundle != "/etc/mkonnect-ca/ca-bundle.pem" {
		t.Fatalf("ca_bundle not persisted: %q", got.CABundle)
	}
}

func TestCredentialValidateRejectsRelativeCABundle(t *testing.T) {
	bad := creds.Credential{BaseURL: "https://x", CABundle: "relative/ca.pem"}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected Validate to reject a relative ca_bundle path")
	}
	empty := creds.Credential{BaseURL: "https://x", CABundle: "   "}
	if err := empty.Validate(); err == nil {
		t.Fatal("expected Validate to reject a whitespace-only ca_bundle path")
	}
	nul := creds.Credential{BaseURL: "https://x", CABundle: "/etc/ca\x00.pem"}
	if err := nul.Validate(); err == nil {
		t.Fatal("expected Validate to reject a ca_bundle path with a NUL byte")
	}
	ok := creds.Credential{BaseURL: "https://x", CABundle: "/etc/mkonnect-ca/ca-bundle.pem"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid ca_bundle rejected: %v", err)
	}
}

func TestStoreGenerationChangesOnMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, err := creds.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g0 := s.Generation()
	if err := s.Set("p", creds.Credential{BaseURL: "https://x"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	g1 := s.Generation()
	if g1 == g0 {
		t.Error("Generation did not change after Set")
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if s.Generation() == g1 {
		t.Error("Generation did not change after Reload")
	}
	g2 := s.Generation()
	if err := s.Remove("p"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if s.Generation() == g2 {
		t.Error("Generation did not change after Remove")
	}
}

func TestReloadMissingFileBumpsGeneration(t *testing.T) {
	// A store whose file never existed: Reload must succeed (empty state) and still bump
	// the generation, so a global CA bundle is re-read on SIGHUP without a creds file.
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, err := creds.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g0 := s.Generation()
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload with no file must not error, got %v", err)
	}
	if s.Generation() == g0 {
		t.Error("Reload with no file did not bump the generation")
	}
}

func TestCredentialValidateRejectsBadStripHeaders(t *testing.T) {
	cases := []struct {
		name  string
		strip []string
	}{
		{"empty", []string{""}},
		{"whitespace", []string{"   "}},
		{"embedded space", []string{"X Bad"}},
		{"colon", []string{"X-Bad: y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := creds.Credential{BaseURL: "http://x", Auth: "bearer", Token: "token-value-abcdef12", StripHeaders: tc.strip}
			if err := c.Validate(); err == nil {
				t.Fatalf("expected Validate to reject strip_headers %v", tc.strip)
			}
		})
	}
	ok := creds.Credential{BaseURL: "http://x", Auth: "bearer", Token: "token-value-abcdef12", StripHeaders: []string{"X-Custom-Auth"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid strip_headers rejected: %v", err)
	}
}
