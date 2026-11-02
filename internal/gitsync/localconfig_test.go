package gitsync_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

// writeConfig writes content to a temp file at 0600 and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git-sync.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadLocalConfigMissingFileIsEmpty(t *testing.T) {
	cfg, err := gitsync.LoadLocalConfig(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(cfg.Repos()) != 0 {
		t.Errorf("Repos() = %v, want empty", cfg.Repos())
	}
	if _, ok := cfg.Get("anything"); ok {
		t.Error("Get on empty config returned ok=true")
	}
}

func TestLoadLocalConfigReposSortedAndGet(t *testing.T) {
	path := writeConfig(t, `{
		"repo-b": {"origin_url": "https://git.example.com/b"},
		"repo-a": {"origin_url": "https://git.example.com/a", "auth": "bearer", "token": "tok-aaaaaaaaaaaaaa"}
	}`)
	cfg, err := gitsync.LoadLocalConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Repos(); !reflect.DeepEqual(got, []string{"repo-a", "repo-b"}) {
		t.Errorf("Repos() = %v, want sorted [repo-a repo-b]", got)
	}
	a, ok := cfg.Get("repo-a")
	if !ok || a.OriginURL != "https://git.example.com/a" || a.Auth != "bearer" {
		t.Errorf("Get(repo-a) = %+v, ok=%v", a, ok)
	}
	if a.Token.Reveal() != "tok-aaaaaaaaaaaaaa" {
		t.Errorf("token did not round-trip through load")
	}
	if _, ok := cfg.Get("repo-z"); ok {
		t.Error("Get(repo-z) returned ok=true for an absent key")
	}
}

func TestLoadLocalConfigMalformedJSONErrorsWithoutLeakingContent(t *testing.T) {
	secret := "super-secret-token-value-xyz"
	// Valid-looking token, then a trailing comma + EOF makes the document malformed.
	path := writeConfig(t, fmt.Sprintf(`{"repo-a": {"origin_url": "https://git.example.com/a", "auth": "bearer", "token": %q},`, secret))
	_, err := gitsync.LoadLocalConfig(path)
	if err == nil {
		t.Fatal("malformed JSON should error")
	}
	if containsSecret(err.Error(), secret) {
		t.Errorf("error message leaked the token: %q", err.Error())
	}
}

func TestLoadLocalConfigRejectsInvalidEntry(t *testing.T) {
	// A token without an auth mode is invalid; the error names the repo but not the token.
	secret := "orphan-token-aaaaaaaaaaaa"
	path := writeConfig(t, fmt.Sprintf(`{"repo-a": {"origin_url": "https://git.example.com/a", "token": %q}}`, secret))
	_, err := gitsync.LoadLocalConfig(path)
	if err == nil {
		t.Fatal("entry with a token but no auth mode should be rejected")
	}
	if containsSecret(err.Error(), secret) {
		t.Errorf("validation error leaked the token: %q", err.Error())
	}
}

func TestLoadLocalConfigRejectsUnknownField(t *testing.T) {
	// A mistyped field must not be silently dropped (it could be a security-relevant setting).
	path := writeConfig(t, `{"repo-a": {"origin_url": "https://git.example.com/a", "orign_url": "x"}}`)
	if _, err := gitsync.LoadLocalConfig(path); err == nil {
		t.Fatal("unknown field should be rejected")
	}
}

func TestLoadLocalConfigRejectsEmptyRepoKey(t *testing.T) {
	path := writeConfig(t, `{"": {"origin_url": "https://git.example.com/a"}}`)
	if _, err := gitsync.LoadLocalConfig(path); err == nil {
		t.Fatal("empty repo key should be rejected")
	}
}

// TestLoadLocalConfigReadOnlyMountAccepted exercises the fallback for a file whose mode cannot
// be chmod-ed (a read-only Secret mount): a safe mode is accepted, an unsafe one is rejected.
func TestLoadLocalConfigReadOnlyMount(t *testing.T) {
	chmodFails := func(string, os.FileMode) error { return errors.New("read-only file system") }

	t.Run("safe mode accepted", func(t *testing.T) {
		restore := gitsync.SetChmodForTest(chmodFails)
		defer restore()
		path := writeConfig(t, `{"repo-a": {"origin_url": "https://git.example.com/a"}}`)
		if err := os.Chmod(path, 0o640); err != nil { // group read only, no world, no group write
			t.Fatalf("chmod: %v", err)
		}
		if _, err := gitsync.LoadLocalConfig(path); err != nil {
			t.Errorf("a 0640 read-only file should be accepted: %v", err)
		}
	})

	t.Run("unsafe mode rejected", func(t *testing.T) {
		restore := gitsync.SetChmodForTest(chmodFails)
		defer restore()
		path := writeConfig(t, `{"repo-a": {"origin_url": "https://git.example.com/a"}}`)
		if err := os.Chmod(path, 0o666); err != nil { // world-writable
			t.Fatalf("chmod: %v", err)
		}
		if _, err := gitsync.LoadLocalConfig(path); err == nil {
			t.Error("a world-writable file that cannot be chmod-ed should be rejected")
		}
	})
}

func TestRepoCredentialValidate(t *testing.T) {
	tests := []struct {
		name    string
		cred    gitsync.RepoCredential
		wantErr bool
	}{
		{"bearer ok", gitsync.RepoCredential{OriginURL: "https://git.example.com/a", Auth: "bearer", Token: "tok-aaaaaaaaaaaaaa"}, false},
		{"basic ok", gitsync.RepoCredential{OriginURL: "https://git.example.com/a", Auth: "basic", Username: "u", Token: "pw-aaaaaaaaaaaaaa"}, false},
		{"no auth ok (http)", gitsync.RepoCredential{OriginURL: "http://git.example.com/a"}, false},
		{"empty origin", gitsync.RepoCredential{}, true},
		{"origin with userinfo", gitsync.RepoCredential{OriginURL: "https://user:pw@git.example.com/a"}, true},
		{"origin with empty host", gitsync.RepoCredential{OriginURL: "https://:8080/"}, true},
		{"ssh origin deferred", gitsync.RepoCredential{OriginURL: "ssh://git@git.example.com/a"}, true},
		{"token without mode", gitsync.RepoCredential{OriginURL: "https://git.example.com/a", Token: "x"}, true},
		{"short bearer token", gitsync.RepoCredential{OriginURL: "https://git.example.com/a", Auth: "bearer", Token: "short"}, true},
		{"http with auth rejected", gitsync.RepoCredential{OriginURL: "http://git.example.com/a", Auth: "bearer", Token: "tok-aaaaaaaaaaaaaa"}, true},
		{"control char in token", gitsync.RepoCredential{OriginURL: "https://git.example.com/a", Auth: "bearer", Token: "tok-aaaaaaaa\naaaa"}, true},
		{"basic without username", gitsync.RepoCredential{OriginURL: "https://git.example.com/a", Auth: "basic", Token: "pw-aaaaaaaaaaaaaa"}, true},
		{"unknown mode", gitsync.RepoCredential{OriginURL: "https://git.example.com/a", Auth: "oauth"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cred.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// TestSecretRedaction verifies the token never reaches fmt, slog, or json output, yet Reveal
// still returns it.
func TestSecretRedaction(t *testing.T) {
	secret := "do-not-print-this-token"
	cred := gitsync.RepoCredential{OriginURL: "https://git.example.com/a", Auth: "bearer", Token: gitsync.Secret(secret)}

	// Every fmt verb routes through Secret.Format.
	for _, rendered := range []string{
		fmt.Sprintf("%v", cred),
		fmt.Sprintf("%+v", cred),
		fmt.Sprintf("%#v", cred),
		fmt.Sprintf("%d", cred.Token),
		fmt.Sprintf("%s", cred.Token),
	} {
		if containsSecret(rendered, secret) {
			t.Errorf("fmt rendering leaked the token: %q", rendered)
		}
	}

	// json.Marshal redacts via MarshalText.
	b, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if containsSecret(string(b), secret) {
		t.Errorf("json.Marshal leaked the token: %s", b)
	}

	// A real slog JSON handler must not emit the token.
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("test", "cred", cred, "token", cred.Token)
	if containsSecret(buf.String(), secret) {
		t.Errorf("slog leaked the token: %s", buf.String())
	}

	// Reveal is the one path that returns the real value.
	if cred.Token.Reveal() != secret {
		t.Errorf("Reveal() = %q, want the original secret", cred.Token.Reveal())
	}
}

// containsSecret reports whether s contains the secret substring (never a no-op on "").
func containsSecret(s, secret string) bool {
	return secret != "" && strings.Contains(s, secret)
}
