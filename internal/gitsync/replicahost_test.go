package gitsync_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

func TestParseReplicaHostCanonicalises(t *testing.T) {
	for raw, want := range map[string]string{
		"replica.example.com":                "replica.example.com:443",
		"Replica.Example.COM:8443":           "replica.example.com:8443",
		"replica.example.com.":               "replica.example.com:443",
		"replica.example.com:443":            "replica.example.com:443",
		"replica.example.com:0443":           "replica.example.com:443",
		"192.0.2.10:8443":                    "192.0.2.10:8443",
		"[2001:db8::1]":                      "[2001:db8::1]:443",
		"[2001:DB8:0:0:0:0:0:1]:8443":        "[2001:db8::1]:8443",
		"[::ffff:1.2.3.4]":                   "[::ffff:1.2.3.4]:443",
		"replica-1.internal_host.example.io": "replica-1.internal_host.example.io:443",
	} {
		got, err := gitsync.ParseReplicaHost(raw)
		if err != nil || got != want {
			t.Errorf("ParseReplicaHost(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestParseReplicaHostRejects(t *testing.T) {
	for _, raw := range []string{
		"https://replica.example.com",
		"https://replica.example.com:8443/",
		"http://replica.example.com",
		"replica.example.com/",
		"replica.example.com/path",
		"user@replica.example.com",
		"user:pw@replica.example.com",
		"replica.example.com?x=1",
		"replica.example.com#frag",
		" replica.example.com",
		"replica.example.com ",
		"replica.example.com\n",
		"replica.example.com\r\n",
		"replica example.com",
		"replica.example.com\x00",
		"replica.example.com:",
		"replica.example.com:0",
		"replica.example.com:65536",
		"replica.example.com:-1",
		"replica.example.com:abc",
		"replica.example.com:443:443",
		"replica.example.com..",
		"-",
		"-replica.example.com",
		"replica-.example.com",
		"replica.-example.com",
		"replica.example.com-:8443",
		".replica.example.com",
		"*.example.com",
		"replica%2eexample.com",
		"repl%C4%B0ca.example.com",
		"replica.example.com\\evil.example",
		"repl\u0130ca.example.com",               // U+0130 LATIN CAPITAL I WITH DOT ABOVE
		"p\u212a.example.com",                    // U+212A KELVIN SIGN
		"\uff52eplica.example.com",               // fullwidth r
		"replica\u3002example.com",               // ideographic full stop
		"replica\u200b.example.com",              // zero-width space
		"2130706433",                             // inet_aton decimal
		"0x7f000001",                             // inet_aton hex
		"010.0.0.1",                              // inet_aton octal
		"::1",                                    // IPv6 literals need brackets
		"[::1",                                   // unbalanced bracket
		"[fe80::1%25eth0]",                       // zone
		"[replica.example.com]",                  // brackets are for IPv6 only
		strings.Repeat("a", 64) + ".example.com", // label too long
	} {
		got, err := gitsync.ParseReplicaHost(raw)
		if err == nil {
			t.Errorf("ParseReplicaHost(%q) = %q, want an error", raw, got)
			continue
		}
		if got != "" {
			t.Errorf("ParseReplicaHost(%q) returned %q alongside an error", raw, got)
		}
		if len(raw) >= 8 && strings.Contains(err.Error(), raw) {
			t.Errorf("ParseReplicaHost(%q) error echoes the value: %v", raw, err)
		}
	}
}

func TestParseReplicaHostEmptyIsNotSet(t *testing.T) {
	if _, err := gitsync.ParseReplicaHost(""); !errors.Is(err, gitsync.ErrReplicaHostNotSet) {
		t.Errorf("empty value: err = %v, want ErrReplicaHostNotSet", err)
	}
	if _, err := gitsync.ParseReplicaHost("   "); err == nil || errors.Is(err, gitsync.ErrReplicaHostNotSet) {
		t.Errorf("whitespace-only value: err = %v, want an invalid-value error distinct from not-set", err)
	}
}

func TestReplicaHostFromEnv(t *testing.T) {
	t.Setenv(gitsync.ReplicaHostEnv, "")
	if _, err := gitsync.ReplicaHostFromEnv(); !errors.Is(err, gitsync.ErrReplicaHostNotSet) {
		t.Errorf("empty env: err = %v, want ErrReplicaHostNotSet", err)
	}
	t.Setenv(gitsync.ReplicaHostEnv, "Replica.Example.com:9443")
	if got, err := gitsync.ReplicaHostFromEnv(); err != nil || got != "replica.example.com:9443" {
		t.Errorf("env = %q, %v; want replica.example.com:9443", got, err)
	}
	t.Setenv(gitsync.ReplicaHostEnv, "replica.example.com\n")
	if _, err := gitsync.ReplicaHostFromEnv(); err == nil || errors.Is(err, gitsync.ErrReplicaHostNotSet) {
		t.Errorf("trailing newline: err = %v, want an explicit invalid-value error", err)
	}
}

const testPin = "replica.example.com:443"

func TestCheckReplicaURL(t *testing.T) {
	type tc struct {
		pin, raw, verdict, canonical string
	}
	cases := []tc{
		// Accepted, and re-serialised from the validated host.
		{testPin, "https://replica.example.com/r/a.git", "ok", "https://replica.example.com:443/r/a.git"},
		{testPin, "https://REPLICA.Example.COM/r/a.git", "ok", "https://replica.example.com:443/r/a.git"},
		{testPin, "https://replica.example.com:443/r/a.git", "ok", "https://replica.example.com:443/r/a.git"},
		{testPin, "https://replica.example.com:0443/r/a.git", "ok", "https://replica.example.com:443/r/a.git"},
		{testPin, "https://replica.example.com/", "ok", "https://replica.example.com:443/"},
		{testPin, "https://replica.example.com./r/a.git", "ok", "https://replica.example.com:443/r/a.git"},
		{testPin, "https://replica.example.com:/r/a.git", "ok", "https://replica.example.com:443/r/a.git"},
		{testPin, "https://replica.example.com:443//evil.example/x", "ok", "https://replica.example.com:443//evil.example/x"},
		{"replica.example.com:8443", "https://replica.example.com:8443/r", "ok", "https://replica.example.com:8443/r"},
		{"[2001:db8::1]:8443", "https://[2001:DB8:0:0:0:0:0:1]:8443/r", "ok", "https://[2001:db8::1]:8443/r"},
		{"[2001:db8::1]:443", "https://[2001:db8::1]/r", "ok", "https://[2001:db8::1]:443/r"},

		// Names another host (or cannot be proven to name the pin): the whole config is rejected.
		{testPin, "https://evil.example/r/a.git", "mismatch", ""},
		{testPin, "https://a.replica.example.com/r", "mismatch", ""}, // subdomain
		{testPin, "https://example.com/r", "mismatch", ""},           // parent domain
		{testPin, "https://xreplica.example.com/r", "mismatch", ""},  // prefix
		{testPin, "https://replica.example.com.evil.example/r", "mismatch", ""},
		{testPin, "https://replica.example.com:8443/r", "mismatch", ""}, // different port
		{"replica.example.com:8443", "https://replica.example.com/r", "mismatch", ""},
		{testPin, "http://replica.example.com/r", "mismatch", ""}, // wrong scheme
		{testPin, "http://replica.example.com:443/r", "mismatch", ""},
		{testPin, "ftp://replica.example.com/r", "mismatch", ""},
		{testPin, "//replica.example.com/r", "mismatch", ""}, // scheme-relative
		{testPin, "https://replica.example.com@evil.example/r", "mismatch", ""},
		{testPin, "https://replica.example.com:pw@evil.example/r", "mismatch", ""},
		{testPin, "https://evil.example#@replica.example.com/r", "mismatch", ""},
		{testPin, "https://evil.example\\@replica.example.com/r", "mismatch", ""},
		{testPin, "https://evil.example%2e/r", "mismatch", ""},
		{testPin, "https://%72eplica.example.com/r", "mismatch", ""}, // percent-encoded ASCII
		{testPin, "https://repl%C4%B0ca.example.com/r", "mismatch", ""},
		{testPin, "https://replica.example.com../r", "mismatch", ""}, // second trailing dot
		{testPin, "https://[::1]/r", "mismatch", ""},
		{testPin, "https://127.0.0.1/r", "mismatch", ""},
		{testPin, "https://2130706433/r", "mismatch", ""},
		{testPin, "https://repl\u0130ca.example.com/r", "mismatch", ""},         // U+0130
		{testPin, "https://\uff52eplica.example.com/r", "mismatch", ""},         // fullwidth
		{testPin, "https://replica\u3002example.com/r", "mismatch", ""},         // ideographic full stop
		{"pk.example.com:443", "https://p\u212a.example.com/r", "mismatch", ""}, // Kelvin sign
		{"[::ffff:1.2.3.4]:443", "https://1.2.3.4/r", "mismatch", ""},
		{testPin, "https://replica.example.com:99999/r", "mismatch", ""},
		// Names no host the connector can prove is the pin: never "just invalid", because git may
		// still read such a value as a host (scp-like) or a local path.
		{testPin, "evil.example/x", "mismatch", ""},
		{testPin, "evil.example:r.git", "mismatch", ""},
		{testPin, "/just/a/path", "mismatch", ""},
		{testPin, "not a url", "mismatch", ""},
		{testPin, "https:///r", "mismatch", ""},
		{testPin, "https://replica.example.com/x\n", "mismatch", ""}, // control character

		// Targets the pinned host but is unusable: stays a per-repository failure.
		{testPin, "", "invalid", ""},
		{testPin, "https://replica.example.com/r g.git", "invalid", ""},                      // space in path
		{testPin, "https://replica.example.com/" + strings.Repeat("a", 3000), "invalid", ""}, // over the URL length bound
		{testPin, "https://user:pw@replica.example.com/r", "invalid", ""},
		{testPin, "https://a@b@replica.example.com/r", "invalid", ""},
		{testPin, "https://replica.example.com/r?token=1", "invalid", ""},
		{testPin, "https://replica.example.com?@evil.example/", "invalid", ""},
		{testPin, "https://replica.example.com/r#frag", "invalid", ""},
	}
	for _, c := range cases {
		canon, verdict := gitsync.CheckReplicaURLForTest(c.raw, c.pin)
		if verdict != c.verdict {
			t.Errorf("pin %q url %q: verdict = %s, want %s", c.pin, c.raw, verdict, c.verdict)
			continue
		}
		if verdict == "ok" && canon != c.canonical {
			t.Errorf("pin %q url %q: canonical = %q, want %q", c.pin, c.raw, canon, c.canonical)
		}
		if verdict != "ok" && canon != "" {
			t.Errorf("pin %q url %q: %s verdict returned a URL %q that could be dialled", c.pin, c.raw, verdict, canon)
		}
	}
}

// TestCheckReplicaURLWithoutPinNeverAccepts: an empty pin matches nothing.
func TestCheckReplicaURLWithoutPinNeverAccepts(t *testing.T) {
	for _, raw := range []string{"https://replica.example.com/r", "https://:443/r", "https:///r"} {
		if canon, v := gitsync.CheckReplicaURLForTest(raw, ""); v == "ok" {
			t.Errorf("%q accepted against an empty pin as %q", raw, canon)
		}
	}
}

// FuzzCheckReplicaURL pins the safety invariant directly: whatever string arrives, an accepted URL
// parses back to exactly the pinned authority over https.
func FuzzCheckReplicaURL(f *testing.F) {
	for _, s := range []string{
		"https://replica.example.com/r", "https://evil.example\\@replica.example.com/", "https://a@b@replica.example.com/",
		"https://replica.example.com:/x", "https://replica%2eexample.com/", "HTTPS://REPLICA.EXAMPLE.COM./", "https://[::1]/",
		"https://replica.example.com#@evil.example/", "https://replica.example.com:443//evil.example/x", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		canon, verdict := gitsync.CheckReplicaURLForTest(raw, testPin)
		if verdict != "ok" {
			if canon != "" {
				t.Fatalf("%q: verdict %s returned dialable URL %q", raw, verdict, canon)
			}
			return
		}
		u, err := url.Parse(canon)
		if err != nil || u.Scheme != "https" || u.Host != testPin || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("%q accepted as %q, which is not a clean https URL on %s (err %v)", raw, canon, testPin, err)
		}
	})
}
