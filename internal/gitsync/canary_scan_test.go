package gitsync_test

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// This file is the leak detector the canary tests share. A canary is a unique, high-entropy credential
// generated per test: if any part of it shows up somewhere it should not, the test fails, whatever
// transformed it on the way.

// canarySpan is the length of the fragment of a canary that counts as a leak. A fragment that long of
// a random string cannot occur by chance, and it catches a credential that was truncated or split
// (by a length limit, a scrubber that rewrote part of it, or a buffer cut).
const canarySpan = 12

// newCanary returns a random credential of 40 characters, drawn from the URL-safe base64 alphabet
// (which includes '-' and '_'). With dotted it carries a '.' too, the character a scrubber that
// treats dotted words as hostnames would split it on: a credential that gets partly rewritten that
// way must still be caught, but one without a dot must be caught whole, so a scenario uses each.
func newCanary(t testing.TB, dotted bool) string {
	t.Helper()
	b := make([]byte, 30)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	s := base64.RawURLEncoding.EncodeToString(b)
	if dotted {
		return s[:20] + "." + s[21:]
	}
	return s
}

// canaryNeedles are the strings to search for: every canarySpan-character window of the canary
// itself, and of its base64 encoding as the secret half of "user:secret" (basic authentication) at
// each of the three alignments base64 can place it. A window of the encoding is searched only where
// it is determined by the secret alone.
func canaryNeedles(secret, user string) []string {
	var needles []string
	windows := func(s string) {
		for i := 0; i+canarySpan <= len(s); i++ {
			needles = append(needles, s[i:i+canarySpan])
		}
	}
	windows(secret)
	prefix := user + ":"
	for shift := 0; shift < 3; shift++ {
		enc := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", shift) + prefix + secret))
		skip := ((shift+len(prefix)-1)/3 + 1) * 4 // characters that also depend on the prefix
		if skip+4 < len(enc) {
			windows(enc[skip : len(enc)-4])
		}
	}
	return needles
}

// leak reports the first canary needle found in data, or "".
func leak(data string, needles []string) string {
	for _, n := range needles {
		if strings.Contains(data, n) {
			return n
		}
	}
	return ""
}

// assertNoLeak fails the test if data carries any part of the canary.
func assertNoLeak(t testing.TB, where, data string, needles []string) {
	t.Helper()
	if n := leak(data, needles); n != "" {
		t.Errorf("%s contains part of a credential (%q...):\n%s", where, n[:4], truncate(data, 1500))
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// The detector must see what it is meant to see, or a passing canary test proves nothing.
func TestCanaryScannerFindsEveryForm(t *testing.T) {
	secret, user := newCanary(t, true), "svc-user"
	needles := canaryNeedles(secret, user)

	for name, hay := range map[string]string{
		"raw":                  "Authorization: Bearer " + secret,
		"raw, mid-line":        "x" + secret + "y",
		"truncated tail":       "Bearer " + secret[:30],
		"truncated head":       secret[10:] + " rest",
		"basic":                "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+secret)),
		"basic, shifted by 1":  "Basic " + base64.StdEncoding.EncodeToString([]byte("a"+user+":"+secret)),
		"basic, shifted by 2":  "Basic " + base64.StdEncoding.EncodeToString([]byte("ab"+user+":"+secret)),
		"basic, longer prefix": "Basic " + base64.StdEncoding.EncodeToString([]byte("zz"+user+":"+secret+"/more")),
	} {
		if leak(hay, needles) == "" {
			t.Errorf("scanner missed the %s form: %q", name, hay)
		}
	}
	for name, hay := range map[string]string{
		"unrelated":  "fatal: unable to access: Could not resolve host",
		"short part": secret[:canarySpan-1],
		"other":      newCanary(t, false),
	} {
		if n := leak(hay, needles); n != "" {
			t.Errorf("scanner flagged the %s text: found %q in %q", name, n, hay)
		}
	}
}
