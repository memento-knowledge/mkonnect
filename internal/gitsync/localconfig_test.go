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

func TestNewMalformedJSONErrorsWithoutLeakingContent(t *testing.T) {
	secret := "super-secret-token-value-xyz"
	// Valid-looking token, then a trailing comma + EOF makes the document malformed.
	path := writeConfig(t, fmt.Sprintf(`{"origin": {"base_url": "https://git.example.com/", "auth": "bearer", "token": %q},`, secret))
	_, err := gitsync.New(path)
	if err == nil {
		t.Fatal("malformed JSON should error")
	}
	if containsSecret(err.Error(), secret) {
		t.Errorf("error message leaked the token: %q", err.Error())
	}
}

func TestNewRejectsUnknownField(t *testing.T) {
	// A mistyped field must not be silently dropped (it could be a security-relevant setting).
	path := writeConfig(t, `{"origin": {"base_url": "https://git.example.com/", "bsae_url": "x"}}`)
	if _, err := gitsync.New(path); err == nil {
		t.Fatal("unknown field should be rejected")
	}
}

// TestNewReadOnlyMount exercises the fallback for a file whose mode cannot be chmod-ed (a read-only
// Secret mount): a safe mode is accepted, an unsafe one is rejected.
func TestNewReadOnlyMount(t *testing.T) {
	chmodFails := func(string, os.FileMode) error { return errors.New("read-only file system") }

	t.Run("safe mode accepted", func(t *testing.T) {
		restore := gitsync.SetChmodForTest(chmodFails)
		defer restore()
		path := writeConfig(t, `{"origin": {"base_url": "https://git.example.com/"}}`)
		if err := os.Chmod(path, 0o640); err != nil { // group read only, no world, no group write
			t.Fatalf("chmod: %v", err)
		}
		if _, err := gitsync.New(path); err != nil {
			t.Errorf("a 0640 read-only file should be accepted: %v", err)
		}
	})

	t.Run("unsafe mode rejected", func(t *testing.T) {
		restore := gitsync.SetChmodForTest(chmodFails)
		defer restore()
		path := writeConfig(t, `{"origin": {"base_url": "https://git.example.com/"}}`)
		if err := os.Chmod(path, 0o666); err != nil { // world-writable
			t.Fatalf("chmod: %v", err)
		}
		if _, err := gitsync.New(path); err == nil {
			t.Error("a world-writable file that cannot be chmod-ed should be rejected")
		}
	})
}

// TestSecretRedaction verifies the token never reaches fmt, slog, or json output, yet Reveal
// still returns it.
func TestSecretRedaction(t *testing.T) {
	secret := "do-not-print-this-token"
	cred := gitsync.LocalEntry{BaseURL: "https://git.example.com/a", Auth: "bearer", Token: gitsync.Secret(secret)}

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

// --- Provider-keyed Store tests ---

// newStorePath returns a path to a not-yet-existing provider config file in a fresh temp dir.
func newStorePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "git-sync.json")
}

func TestStoreMissingFileIsEmpty(t *testing.T) {
	s, err := gitsync.New(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(s.Names()) != 0 {
		t.Errorf("Names() = %v, want empty", s.Names())
	}
}

func TestStoreLoadNamesSortedAndGet(t *testing.T) {
	path := writeConfig(t, `{
		"beta":  {"base_url": "https://git-b.example.com/"},
		"alpha": {"base_url": "https://git-a.example.com/", "auth": "bearer", "token": "tok-aaaaaaaaaaaaaa"}
	}`)
	s, err := gitsync.New(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := s.Names(); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Errorf("Names() = %v, want [alpha beta]", got)
	}
	a, ok := s.Get("alpha")
	if !ok || a.BaseURL != "https://git-a.example.com/" || a.Auth != "bearer" {
		t.Errorf("Get(alpha) = %+v ok=%v", a, ok)
	}
	if a.Token.Reveal() != "tok-aaaaaaaaaaaaaa" {
		t.Error("token did not round-trip through load")
	}
	if _, ok := s.Get("missing"); ok {
		t.Error("Get(missing) returned ok=true")
	}
}

func TestStoreRejectsProviderName(t *testing.T) {
	path := writeConfig(t, `{"bad name!": {"base_url": "https://git.example.com/"}}`)
	if _, err := gitsync.New(path); err == nil {
		t.Fatal("an invalid provider name should be rejected")
	}
}

// TestStoreOnDiskTokenIsRevealedNotRedacted is the load-bearing persistence test: the Secret type
// redacts on marshal, so the Store must persist through an explicit plain-string on-disk form.
func TestStoreOnDiskTokenIsRevealedNotRedacted(t *testing.T) {
	path := newStorePath(t)
	s, err := gitsync.New(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	token := "disk-token-abcdefghij"
	if err := s.Set("origin", gitsync.LocalEntry{BaseURL: "https://git.example.com/", Auth: "bearer", Token: gitsync.Secret(token)}); err != nil {
		t.Fatalf("set: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(raw), token) {
		t.Errorf("on-disk config does not contain the real token: %s", raw)
	}
	if strings.Contains(string(raw), "<redacted>") {
		t.Errorf("on-disk config persisted the redaction instead of the token: %s", raw)
	}
	// And a reload recovers the real value.
	s2, err := gitsync.New(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if e, ok := s2.Get("origin"); !ok || e.Token.Reveal() != token {
		t.Errorf("reloaded token = %q ok=%v, want %q", e.Token.Reveal(), ok, token)
	}
}

func TestStoreDuplicateHostRejectedAtNew(t *testing.T) {
	path := writeConfig(t, `{
		"a": {"base_url": "https://git.example.com/x"},
		"b": {"base_url": "https://git.example.com/y"}
	}`)
	_, err := gitsync.New(path)
	if err == nil {
		t.Fatal("two providers on the same host should be rejected at load")
	}
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Errorf("error should name both offending providers: %v", err)
	}
}

func TestStoreDuplicateHostRejectedAtSet(t *testing.T) {
	s, err := gitsync.New(newStorePath(t))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.Set("origin", gitsync.LocalEntry{BaseURL: "https://git.example.com/team"}); err != nil {
		t.Fatalf("first set: %v", err)
	}
	// A different name on the same host (different path, explicit default port) must be rejected.
	if err := s.Set("other", gitsync.LocalEntry{BaseURL: "https://git.example.com:443/elsewhere"}); err == nil {
		t.Error("a second provider on the same host should be rejected at Set")
	}
	// Replacing the SAME name's entry on its own host is allowed.
	if err := s.Set("origin", gitsync.LocalEntry{BaseURL: "https://git.example.com/renamed"}); err != nil {
		t.Errorf("replacing a provider's own entry should be allowed: %v", err)
	}
}

func TestStoreRemove(t *testing.T) {
	s, err := gitsync.New(newStorePath(t))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.Set("origin", gitsync.LocalEntry{BaseURL: "https://git.example.com/"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.Remove("origin"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := s.Get("origin"); ok {
		t.Error("entry still present after remove")
	}
	if err := s.Remove("origin"); err == nil {
		t.Error("removing a non-existent provider should error")
	}
}

func TestLocalEntryValidate(t *testing.T) {
	tests := []struct {
		name    string
		entry   gitsync.LocalEntry
		wantErr bool
	}{
		{"bearer ok", gitsync.LocalEntry{BaseURL: "https://git.example.com/", Auth: "bearer", Token: "tok-aaaaaaaaaaaaaa"}, false},
		{"basic ok", gitsync.LocalEntry{BaseURL: "https://git.example.com/", Auth: "basic", Username: "u", Token: "pw-aaaaaaaaaaaaaa"}, false},
		{"no auth http ok", gitsync.LocalEntry{BaseURL: "http://git.example.com/"}, false},
		{"empty base", gitsync.LocalEntry{}, true},
		{"userinfo", gitsync.LocalEntry{BaseURL: "https://u:p@git.example.com/"}, true},
		{"backslash", gitsync.LocalEntry{BaseURL: "https://git.example.com\\@evil.com/"}, true},
		{"query", gitsync.LocalEntry{BaseURL: "https://git.example.com/?x=1"}, true},
		{"ssh deferred", gitsync.LocalEntry{BaseURL: "ssh://git@git.example.com/"}, true},
		{"token without mode", gitsync.LocalEntry{BaseURL: "https://git.example.com/", Token: "x"}, true},
		{"short token", gitsync.LocalEntry{BaseURL: "https://git.example.com/", Auth: "bearer", Token: "short"}, true},
		{"http with auth", gitsync.LocalEntry{BaseURL: "http://git.example.com/", Auth: "bearer", Token: "tok-aaaaaaaaaaaaaa"}, true},
		{"control char", gitsync.LocalEntry{BaseURL: "https://git.example.com/", Auth: "bearer", Token: "tok-aaaaaa\naaaaaa"}, true},
		{"basic no username", gitsync.LocalEntry{BaseURL: "https://git.example.com/", Auth: "basic", Token: "pw-aaaaaaaaaaaaaa"}, true},
		{"unknown mode", gitsync.LocalEntry{BaseURL: "https://git.example.com/", Auth: "oauth"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.entry.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestMatchProvider(t *testing.T) {
	s, err := gitsync.New(newStorePath(t))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.Set("origin", gitsync.LocalEntry{BaseURL: "https://git.example.com/team"}); err != nil {
		t.Fatalf("set origin: %v", err)
	}
	if err := s.Set("six", gitsync.LocalEntry{BaseURL: "https://[2001:db8::1]/"}); err != nil {
		t.Fatalf("set six: %v", err)
	}
	tests := []struct {
		name    string
		origin  string
		wantOK  bool
		wantHit string
	}{
		{"same host default port", "https://git.example.com/team/repo.git", true, "origin"},
		{"mixed case host", "https://GIT.Example.com/x", true, "origin"},
		{"trailing dot + explicit 443", "https://git.example.com.:443/x", true, "origin"},
		{"different port", "https://git.example.com:8443/x", false, ""},
		{"scheme downgrade", "http://git.example.com/x", false, ""},
		{"lookalike suffix", "https://git.example.com.evil.com/x", false, ""},
		{"lookalike glued", "https://git.example.comevil.com/x", false, ""},
		{"userinfo", "https://u:p@git.example.com/x", false, ""},
		{"fragment", "https://git.example.com/x#@evil.com", false, ""},
		{"garbage", "not a url", false, ""},
		{"ipv6 case + explicit port", "https://[2001:DB8::1]:443/x", true, "six"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, _, ok := s.MatchProvider(tt.origin)
			if ok != tt.wantOK || (ok && name != tt.wantHit) {
				t.Errorf("MatchProvider(%q) = (%q, %v), want (%q, %v)", tt.origin, name, ok, tt.wantHit, tt.wantOK)
			}
		})
	}
}
