package gitsync_test

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

// fuzzCanary stands in for a credential in the fuzz inputs. It holds characters a provider name may not
// (so a mutated input cannot make it a valid, and therefore legitimately echoed, name).
const fuzzCanary = "fz+7f3a9c1e5b2d4086+canary"

// singleLine reports whether s could be written into a log or a terminal as one line.
func singleLine(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || unicode.IsControl(r) })
}

// FuzzLoadConfig feeds arbitrary bytes to the real provider-config loader (through both New and the
// `config check` path, which share one parser). It must never panic; New and the check must agree on
// whether the file is acceptable; every message must be one line; and when the input contains the
// canary credential, no message may repeat it.
func FuzzLoadConfig(f *testing.F) {
	for _, seed := range []string{
		``, `{}`, `null`, `[]`, `"x"`, `{"a":`, `{"a":{}}`, `{"a":{}} {"b":{}}`,
		`{"origin":{"base_url":"https://git.example.test/"}}`,
		`{"origin":{"base_url":"https://git.example.test/","auth":"bearer","token":"` + fuzzCanary + `"}}`,
		`{"origin":{"base_url":"https://git.example.test/","auth":"basic","username":"svc","token":"` + fuzzCanary + `"}}`,
		`{"origin":{"base_url":"http://git.example.test/","auth":"bearer","token":"` + fuzzCanary + `"}}`,
		`{"origin":{"base_url":"https://u:` + fuzzCanary + `@git.example.test/"}}`,
		`{"origin":{"base_url":"https://git.example.test/?` + fuzzCanary + `"}}`,
		`{"origin":{"base_url":"https://git.example.test/","` + fuzzCanary + `":"x"}}`,
		`{"` + fuzzCanary + `":{"base_url":"https://git.example.test/"}}`,
		`{"origin":{"base_url":"https://git.example.test/","token":` + fuzzCanary + `}}`,
		`{"origin":{"base_url":7,"token":"` + fuzzCanary + `"}}`,
		`{"a":{"base_url":"https://git.example.test/"},"b":{"base_url":"https://GIT.example.test:443/x"}}`,
		`{"origin":{"base_url":"https://git.example.test/","auth":"bearer","token":"` + fuzzCanary + "\\u0000\"}}",
		"\xff\xfe{\"origin\":{}}", `{"origin":{"base_url":"https://git.example.test/"}} ` + fuzzCanary,
	} {
		f.Add([]byte(seed))
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(dir, "git-sync.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, newErr := gitsync.New(path)
		rep := gitsync.CheckConfig(path)
		if (newErr != nil) != (len(rep.Issues) > 0) {
			t.Fatalf("New returned %v but the check found %d problem(s)", newErr, len(rep.Issues))
		}
		if newErr != nil && newErr.Error() != rep.Issues[0].Error() {
			t.Fatalf("New reports %q, the check's first problem is %q", newErr, rep.Issues[0])
		}
		if _, again := gitsync.New(path); (again == nil) != (newErr == nil) || (again != nil && again.Error() != newErr.Error()) {
			t.Fatalf("loading twice gave %v then %v", newErr, again)
		}

		hasCanary := bytes.Contains(data, []byte(fuzzCanary))
		var texts []string
		if newErr != nil {
			texts = append(texts, newErr.Error())
		}
		for _, i := range rep.Issues {
			texts = append(texts, i.Error(), i.Message, i.Provider, i.Field, string(i.Class))
		}
		for _, p := range rep.Providers {
			texts = append(texts, p.Name, p.HostKey, p.Auth)
		}
		for _, s := range texts {
			if !singleLine(strings.ReplaceAll(s, path, "")) {
				t.Fatalf("a message is not a single line: %q", s)
			}
			if hasCanary && strings.Contains(s, fuzzCanary) {
				t.Fatalf("a message repeats the input: %q", s)
			}
		}
		for _, p := range rep.Providers {
			if hk, err := gitsync.CanonicalHostKeyForTest(p.HostKey); err != nil || hk != p.HostKey {
				t.Fatalf("host key %q does not canonicalize to itself: %q, %v", p.HostKey, hk, err)
			}
		}
	})
}

// FuzzCanonicalRemoteURL feeds arbitrary strings to the URL canonicalizer behind the host-match guard.
// It must never panic and never echo its input in an error; and what it accepts must be a plain
// http(s) URL whose authority is exactly the host key, which canonicalizes to itself.
func FuzzCanonicalRemoteURL(f *testing.F) {
	for _, seed := range []string{
		``, `https://git.example.test`, `https://git.example.test/team/repo.git`, `HTTPS://GIT.EXAMPLE.TEST:443/`,
		`https://git.example.test.:8443/x`, `http://127.0.0.1:80/`, `https://[::1]:443/x`, `https://[fe80::1%25eth0]/`,
		`https://2130706433/`, `https://0x7f000001/`, `https://010.0.0.1/`, `https://127.0.0.0x1/`,
		`https://u:` + fuzzCanary + `@git.example.test/`, `https://git.example.test/?t=` + fuzzCanary,
		`https://git.example.test/#` + fuzzCanary, `https://git.example.test/` + fuzzCanary,
		`https://` + fuzzCanary + `/`, `https://git.example.test:` + fuzzCanary + `/`,
		"https://git.example.test/a b", "https://git.example.test\\@evil.test/", "https://git.example.test:0/",
		"https://git.example.test:99999/", "https://gıt.example.test/", "https://git。example.test/",
		"https://git.example.test/%2e%2e/x", "https://git.example.test/\x00", "ssh://git.example.test/x", "git@git.example.test:x/y.git",
		"file:///etc/passwd", "//git.example.test/x", "https:///x", "https://:443/x",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		hostKey, full, err := gitsync.CanonicalRemoteURLForTest(raw)
		if err != nil {
			if hostKey != "" || full != "" {
				t.Fatalf("an error came with a result: %q %q %v", hostKey, full, err)
			}
			if strings.Contains(raw, fuzzCanary) && strings.Contains(err.Error(), fuzzCanary) {
				t.Fatalf("the error repeats the input: %q", err)
			}
			if !singleLine(err.Error()) {
				t.Fatalf("the error is not a single line: %q", err)
			}
			return
		}
		u, perr := url.Parse(full)
		if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("accepted %q but produced an unusable URL %q (%v)", raw, full, perr)
		}
		if hostKey != u.Scheme+"://"+u.Host {
			t.Fatalf("host key %q is not the scheme and authority of %q", hostKey, full)
		}
		if !singleLine(full) || strings.ContainsAny(full, " \\") {
			t.Fatalf("the dialed URL %q has whitespace, control characters or a backslash", full)
		}
		hk2, full2, err := gitsync.CanonicalRemoteURLForTest(full)
		if err != nil || hk2 != hostKey || full2 != full {
			t.Fatalf("canonicalizing %q again gave %q, %q, %v", full, hk2, full2, err)
		}
	})
}
