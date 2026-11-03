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

func TestNewRejectsTrailingData(t *testing.T) {
	for name, content := range map[string]string{
		"trailing garbage": `{"origin": {"base_url": "https://git.example.com/"}} nonsense`,
		"second object":    `{"origin": {"base_url": "https://git.example.com/"}}{"x":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := gitsync.New(writeConfig(t, content)); err == nil {
				t.Fatal("content after the configuration object should be rejected")
			}
		})
	}
	// Trailing whitespace/newline is fine.
	if _, err := gitsync.New(writeConfig(t, `{"origin": {"base_url": "https://git.example.com/"}}`+"\n\n")); err != nil {
		t.Errorf("trailing whitespace should be accepted: %v", err)
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

// TestCanonicalHostRejectsAmbiguous asserts that the host/port canonicalization underpinning the
// match guard REJECTS (errors on) non-ASCII, Unicode-lookalike, IPv6-zone, ambiguous-numeric,
// over-long, and out-of-range forms. The rejection is done by canonicalASCIIHost's ASCII-charset
// validation, its label-length and final-label numeric checks, and canonicalPort's range check (not
// by the ASCII-only lowercasing, which only ever sees already-ASCII input). Each ambiguous host
// would otherwise resolve, in git/curl, to a different host than any ASCII provider it textually
// resembles — a bypass of the "match-checked host == dialed host" invariant.
func TestCanonicalHostRejectsAmbiguous(t *testing.T) {
	bad := []struct {
		name, url string
	}{
		{"capital-dotted-I U+0130", "https://gİthub.com/x"},    // ToLower("İ") == "i"
		{"kelvin-sign U+212A", "https://Kelvin.example.com/x"}, // ToLower("K") == "k"
		{"fullwidth letters", "https://ｇithub.com/x"},          // U+FF47 fullwidth g
		{"ideographic-full-stop U+3002", "https://github。com/x"},
		{"zero-width-space U+200B", "https://git\u200bhub.com/x"},
		{"percent-encoded non-ASCII", "https://git%C4%B0hub.com/x"},
		{"bare integer IPv4", "https://2130706433/x"},
		{"bare hex integer", "https://0x7f000001/x"},
		{"hex run final label", "https://127.0.0.0x1/x"},
		{"hex octet mid-dotted", "https://1.2.3.0x4/x"},
		// These reject on the final label itself being a numeric/hex/octal run (earlier labels are
		// non-numeric names), so they exercise the final-label rule rather than a numeric octet.
		{"numeric final label (decimal)", "https://a.0.0.1/x"},
		{"octal final label", "https://a.010/x"},
		{"hex final label", "https://a.0x7f/x"},
		{"uppercase hex final label", "https://a.0X7F/x"},
		{"empty-hex 0x final label", "https://example.0x/x"},
		{"numeric final label after trailing dot", "https://127.1./x"},
		// Earlier-label numeric/octal octets that only decode as inet_aton because the WHOLE host is
		// numeric; the final label is still what the rule keys on.
		{"leading-zero octal octet", "https://010.0.0.1/x"},
		{"hex leading octet", "https://0x7f.0.0.1/x"},
		{"empty label (double dot)", "https://github..com/x"},
		{"trailing empty label (double trailing dot)", "https://github.com../x"},
		{"over-long label (64 chars)", "https://" + strings.Repeat("a", 64) + ".example.com/x"},
		{"port zero", "https://git.example.com:0/x"},
		{"port over max", "https://git.example.com:65536/x"},
		{"ipv6 zone id", "https://[fe80::1%25eth0]/x"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if hk, err := gitsync.CanonicalHostKeyForTest(tt.url); err == nil {
				t.Errorf("CanonicalHostKey(%q) = %q, nil; want an error (host must be rejected)", tt.url, hk)
			}
		})
	}
}

// TestCanonicalHostAcceptsLegitimate pins legitimate hosts that must NOT be false-rejected, so a
// future tightening of the charset or numeric rules cannot silently break valid operator configs or
// origins. Underscores appear in internal (non-public-DNS) git hostnames and are safe here because
// matching is ASCII string equality, not DNS resolution.
func TestCanonicalHostAcceptsLegitimate(t *testing.T) {
	good := []string{
		"https://git.example.com/x",
		"https://123.example.com/x", // all-digit NON-final label is a valid name
		"https://localhost/x",
		"https://git_server/x",                                  // underscore: internal host
		"https://" + strings.Repeat("a", 63) + ".example.com/x", // max-length label
		"https://git.example.com./x",                            // trailing FQDN dot
		"https://[2001:db8::1]/x",                               // IPv6 literal
		"https://127.0.0.1/x",                                   // IPv4 literal
		"https://0x.example.com/x",                              // "0x" is fine as a NON-final label
		"https://example.1e3/x",                                 // "1e3" final label is not a numeric/hex run
		"https://git.example.com:65535/x",                       // max port
		"https://git.example.com:/x",                            // empty port falls back to the default
	}
	for _, u := range good {
		if _, err := gitsync.CanonicalHostKeyForTest(u); err != nil {
			t.Errorf("CanonicalHostKey(%q) errored, want accepted: %v", u, err)
		}
	}
}

// TestHostMatchNoUnicodeBypass is the end-to-end guard: a provider configured at an ASCII host must
// never be matched by a platform-supplied origin whose host only Unicode-folds or numeric-decodes to
// that host. The canonicalization rejects each lookalike outright, so none can bind the provider.
func TestHostMatchNoUnicodeBypass(t *testing.T) {
	s, err := gitsync.New(newStorePath(t))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.Set("gh", gitsync.LocalEntry{BaseURL: "https://github.com/"}); err != nil {
		t.Fatalf("set gh: %v", err)
	}
	// A second provider whose host contains 'k', so the Kelvin sign (U+212A, which strings.ToLower
	// folds to "k") has an ASCII twin it could try to bind to end-to-end.
	if err := s.Set("ks", gitsync.LocalEntry{BaseURL: "https://keyserver.example.com/"}); err != nil {
		t.Fatalf("set ks: %v", err)
	}
	lookalikes := []string{
		"https://gİthub.com/owner/repo.git",       // İ folds to i
		"https://github。com/owner/repo.git",       // ideographic dot
		"https://git\u200bhub.com/owner/repo.git", // zero-width space
		"https://git%C4%B0hub.com/owner/repo.git", // percent-encoded
		"https://Keyserver.example.com/o/r.git",   // Kelvin sign folds to "k" -> keyserver
	}
	for _, origin := range lookalikes {
		if name, _, ok := s.MatchProvider(origin); ok {
			t.Errorf("MatchProvider(%q) matched provider %q; a Unicode/encoded lookalike must not bind", origin, name)
		}
	}
	// A configured base_url with such a host must itself be rejected, so a bypass host can never be
	// stored as a provider either.
	if err := s.Set("evil", gitsync.LocalEntry{BaseURL: "https://gİthub.com/"}); err == nil {
		t.Error("Set accepted a provider with a non-ASCII host; want rejection")
	}
}

// TestCanonicalRemoteURLReserialized asserts the dial URL: the host is canonicalized to the
// exact string that was match-checked (ASCII-lowercased, trailing dot stripped, explicit port) and
// the repository path is preserved, so git dials byte-for-byte the host that was authorized.
func TestCanonicalRemoteURLReserialized(t *testing.T) {
	tests := []struct {
		name, origin, wantKey, wantURL string
	}{
		{"mixed case + default port", "https://GitHub.com/owner/repo.git", "https://github.com:443", "https://github.com:443/owner/repo.git"},
		{"uppercase scheme + host", "HTTPS://GitHub.COM/o/r.git", "https://github.com:443", "https://github.com:443/o/r.git"},
		{"trailing dot + explicit port", "https://github.com.:443/o/r.git", "https://github.com:443", "https://github.com:443/o/r.git"},
		{"empty port falls back to default", "https://git.example.com:/o/r.git", "https://git.example.com:443", "https://git.example.com:443/o/r.git"},
		{"ipv6 literal", "https://[2001:DB8::1]/o/r.git", "https://[2001:db8::1]:443", "https://[2001:db8::1]:443/o/r.git"},
		{"ipv6 literal + explicit port", "https://[::1]:8443/o/r.git", "https://[::1]:8443", "https://[::1]:8443/o/r.git"},
		{"http default port", "http://git.example.com/o/r.git", "http://git.example.com:80", "http://git.example.com:80/o/r.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hk, full, err := gitsync.CanonicalRemoteURLForTest(tt.origin)
			if err != nil {
				t.Fatalf("CanonicalRemoteURL(%q) error: %v", tt.origin, err)
			}
			if hk != tt.wantKey {
				t.Errorf("hostKey = %q, want %q", hk, tt.wantKey)
			}
			if full != tt.wantURL {
				t.Errorf("fullURL = %q, want %q", full, tt.wantURL)
			}
		})
	}
}
