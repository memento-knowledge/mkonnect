package gitsync_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

// tokenSentinel is a distinctive credential that must never appear in an error, log line, or
// formatted entry produced while loading it.
const tokenSentinel = "SENTINEL-token-0123456789"

// tokenFileConfig renders a config with one bearer provider that reads its token from tokenFile.
func tokenFileConfig(t *testing.T, provider, tokenFile string) string {
	t.Helper()
	b, err := json.Marshal(map[string]map[string]string{provider: {
		"base_url":   "https://git.example.test/",
		"auth":       "bearer",
		"token_file": tokenFile,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeFile writes content at exactly mode (the umask cannot loosen or tighten it).
func writeFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// captureSlog routes the default logger to a buffer for the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// assertNoSecret fails if any of the texts contains the sentinel token.
func assertNoSecret(t *testing.T, what string, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if strings.Contains(s, tokenSentinel) {
			t.Errorf("%s leaked the credential: %q", what, s)
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestNewTokenFileAccepts(t *testing.T) {
	const tok = tokenSentinel + "-ok"
	tests := []struct {
		name     string
		content  string
		mode     os.FileMode
		want     string
		wantWarn bool
	}{
		{"owner only", tok, 0o600, tok, false},
		{"read only owner", tok, 0o400, tok, false},
		{"one trailing newline is trimmed", tok + "\n", 0o600, tok, false},
		{"group read (Kubernetes Secret)", tok, 0o440, tok, true},
		{"group read 0640", tok, 0o640, tok, true},
		{"world readable (Docker secret default)", tok, 0o444, tok, true},
		{"world readable 0644", tok, 0o644, tok, true},
		{"inner space is part of the token", "pass phrase " + tok, 0o600, "pass phrase " + tok, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureSlog(t)
			dir := t.TempDir()
			tf := writeFile(t, dir, "token", tt.content, tt.mode)
			s, err := gitsync.New(writeConfig(t, tokenFileConfig(t, "internal-git", tf)))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			e, ok := s.Get("internal-git")
			if !ok || e.Token.Reveal() != tt.want || e.TokenFile != tf {
				t.Fatalf("entry = %+v ok=%v, want token %q from %q", e, ok, tt.want, tf)
			}
			warned := strings.Contains(logs.String(), "readable by group or other users")
			if warned != tt.wantWarn {
				t.Errorf("group/other-read warning = %v, want %v (logs %q)", warned, tt.wantWarn, logs)
			}
			assertNoSecret(t, "logs", logs.String())
			assertNoSecret(t, "formatted entry", fmt.Sprintf("%v %+v %#v", e, e, e))
		})
	}
}

func TestNewTokenFileRejects(t *testing.T) {
	long := strings.Repeat("a", 4097)
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		want    string
	}{
		{"group writable", tokenSentinel, 0o620, "writable by group or others"},
		{"group writable 0660", tokenSentinel, 0o660, "writable by group or others"},
		{"other writable", tokenSentinel, 0o602, "writable by group or others"},
		{"world writable", tokenSentinel, 0o666, "writable by group or others"},
		{"empty", "", 0o600, "is empty"},
		{"only a newline", "\n", 0o600, "is empty"},
		{"only spaces", "      \n", 0o600, "is empty"},
		{"CRLF line ending", tokenSentinel + "\r\n", 0o600, "carriage return"},
		{"lone trailing CR", tokenSentinel + "\r", 0o600, "carriage return"},
		{"second trailing newline", tokenSentinel + "\n\n", 0o600, "more than one line"},
		{"two lines", tokenSentinel + "\n" + tokenSentinel + "\n", 0o600, "more than one line"},
		{"embedded NUL", tokenSentinel + "\x00" + tokenSentinel, 0o600, "control or invisible"},
		{"embedded tab", tokenSentinel + "\t" + tokenSentinel, 0o600, "control or invisible"},
		{"byte-order mark", "\xef\xbb\xbf" + tokenSentinel, 0o600, "control or invisible"},
		{"zero-width space", tokenSentinel + "\u200b", 0o600, "control or invisible"},
		{"invalid UTF-8", tokenSentinel + "\xff\xfe", 0o600, "not valid UTF-8"},
		{"leading space", " " + tokenSentinel, 0o600, "leading or trailing whitespace"},
		{"trailing space", tokenSentinel + " ", 0o600, "leading or trailing whitespace"},
		{"too short", "short", 0o600, "at least 16 characters"},
		{"oversized", long, 0o600, "larger than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureSlog(t)
			tf := writeFile(t, t.TempDir(), "token", tt.content, tt.mode)
			s, err := gitsync.New(writeConfig(t, tokenFileConfig(t, "internal-git", tf)))
			if err == nil {
				t.Fatalf("load succeeded (store %v), want an error containing %q", s, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), `"internal-git"`) {
				t.Errorf("error %q should name the provider", err)
			}
			assertNoSecret(t, "error", err.Error())
			assertNoSecret(t, "logs", logs.String())
		})
	}
}

func TestNewTokenFileFileKinds(t *testing.T) {
	const tok = tokenSentinel + "-kinds"

	t.Run("symlink to an acceptable file is followed", func(t *testing.T) {
		dir := t.TempDir()
		target := writeFile(t, dir, "target", tok, 0o600)
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		s, err := gitsync.New(writeConfig(t, tokenFileConfig(t, "p", link)))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if e, _ := s.Get("p"); e.Token.Reveal() != tok {
			t.Errorf("token not read through the symlink")
		}
	})

	t.Run("a symlink does not hide an unsafe target", func(t *testing.T) {
		dir := t.TempDir()
		// The link itself is 0777 on most systems; the target's mode is what counts.
		target := writeFile(t, dir, "target", tok, 0o666)
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		_, err := gitsync.New(writeConfig(t, tokenFileConfig(t, "p", link)))
		if err == nil || !strings.Contains(err.Error(), "writable by group or others") {
			t.Fatalf("want the target's mode rejected, got %v", err)
		}
	})

	t.Run("a directory is not a regular file", func(t *testing.T) {
		_, err := gitsync.New(writeConfig(t, tokenFileConfig(t, "p", t.TempDir())))
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a FIFO is rejected without blocking", func(t *testing.T) {
		fifo := filepath.Join(t.TempDir(), "fifo")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Skipf("cannot create a FIFO here: %v", err)
		}
		cfg := writeConfig(t, tokenFileConfig(t, "p", fifo))
		done := make(chan error, 1)
		go func() { _, err := gitsync.New(cfg); done <- err }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("loading blocked on a FIFO")
		}
	})

	t.Run("a missing token file is a hard error, not an empty store", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "absent")
		s, err := gitsync.New(writeConfig(t, tokenFileConfig(t, "p", missing)))
		if err == nil {
			t.Fatalf("load succeeded with %v providers; a missing token file must fail", s.Names())
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the cause should stay recognisable as not-exist, got %v", err)
		}
		if !strings.Contains(err.Error(), `provider "p"`) || !strings.Contains(err.Error(), "could not be opened") {
			t.Errorf("error should name the provider and say the file could not be opened: %v", err)
		}
		if strings.Count(err.Error(), missing) != 1 {
			t.Errorf("the path should appear once in %q", err)
		}
	})
}

func TestNewTokenFileOwner(t *testing.T) {
	const euid = 4242
	owners := func(m map[string]int) func(string) (int, bool) {
		return func(name string) (int, bool) {
			uid, ok := m[name]
			return uid, ok
		}
	}
	tests := []struct {
		name    string
		owners  map[string]int
		wantErr string // "" = loads
	}{
		{"everything owned by the running user", map[string]int{"git-sync.json": euid, "token": euid}, ""},
		{"token file owned by root", map[string]int{"git-sync.json": euid, "token": 0}, ""},
		{"token file owned by another user", map[string]int{"git-sync.json": euid, "token": 1001}, "token_file"},
		{"owner of the token file cannot be determined", map[string]int{"git-sync.json": euid}, "token_file"},
		{"config owned by root", map[string]int{"git-sync.json": 0, "token": euid}, ""},
		{"config owned by another user and naming a token file", map[string]int{"git-sync.json": 1001, "token": euid}, "uses token_file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer gitsync.SetOwnerForTest(owners(tt.owners), euid)()
			tf := writeFile(t, t.TempDir(), "token", tokenSentinel+"-owner", 0o600)
			_, err := gitsync.New(writeConfig(t, tokenFileConfig(t, "p", tf)))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("load: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), "owned by another user") || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("want an owner error mentioning %q, got %v", tt.wantErr, err)
			}
			assertNoSecret(t, "error", errText(err))
		})
	}

	t.Run("a foreign config naming a token file never opens the token file", func(t *testing.T) {
		defer gitsync.SetOwnerForTest(owners(map[string]int{"git-sync.json": 1001}), euid)()
		missing := filepath.Join(t.TempDir(), "never-opened")
		_, err := gitsync.New(writeConfig(t, tokenFileConfig(t, "p", missing)))
		if err == nil || !strings.Contains(err.Error(), "owned by another user") || strings.Contains(err.Error(), "could not be opened") {
			t.Fatalf("want the config owner rejected before any token file is opened, got %v", err)
		}
	})

	t.Run("a foreign config with only inline tokens still loads, with a warning", func(t *testing.T) {
		logs := captureSlog(t)
		defer gitsync.SetOwnerForTest(owners(map[string]int{"git-sync.json": 1001}), euid)()
		cfg := writeConfig(t, `{"p": {"base_url": "https://git.example.test/", "auth": "bearer", "token": "`+tokenSentinel+`-inline"}}`)
		if _, err := gitsync.New(cfg); err != nil {
			t.Fatalf("this release must keep accepting it: %v", err)
		}
		if !strings.Contains(logs.String(), "owned by another user") {
			t.Errorf("expected an ownership warning, got %q", logs)
		}
		assertNoSecret(t, "logs", logs.String())
	})
}

func TestNewTokenFileSettingErrors(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, dir, "good", tokenSentinel+"-settings", 0o600)
	missing := filepath.Join(dir, "absent")
	entry := func(extra string) string {
		return `{"p": {"base_url": "https://git.example.test/", ` + extra + `}}`
	}
	tests := []struct {
		name    string
		config  string
		want    string
		notWant string
	}{
		{"both token and token_file", entry(`"auth": "bearer", "token": "` + tokenSentinel + `-inline", "token_file": "` + good + `"`), "only one of token and token_file", ""},
		{"neither with an auth mode", entry(`"auth": "bearer"`), "requires a token or a token_file", ""},
		{"token_file without an auth mode", entry(`"token_file": "` + good + `"`), "credentials require an authentication mode", ""},
		{"relative path", entry(`"auth": "bearer", "token_file": "secrets/token"`), "must be an absolute path", ""},
		{"path with a control character", entry(`"auth": "bearer", "token_file": "/run/x\ny"`), "control or invisible", ""},
		{"path with an invisible character", entry(`"auth": "bearer", "token_file": "/run/x\u200by"`), "control or invisible", ""},
		{"overlong path", entry(`"auth": "bearer", "token_file": "/` + strings.Repeat("a", 4100) + `"`), "at most 4096 bytes", ""},
		{"http base URL is rejected before the file is opened", `{"p": {"base_url": "http://git.example.test/", "auth": "bearer", "token_file": "` + missing + `"}}`, "https", "could not be opened"},
		{"bad username is rejected before the file is opened", entry(`"auth": "bearer", "username": "u", "token_file": "` + missing + `"`), "must not include a username", "could not be opened"},
		{"basic without a username is rejected before the file is opened", entry(`"auth": "basic", "token_file": "` + missing + `"`), "requires a username", "could not be opened"},
		{"unknown auth is rejected before the file is opened", entry(`"auth": "oauth", "token_file": "` + missing + `"`), "unsupported authentication mode", "could not be opened"},
		{"duplicate host is rejected before any file is opened", `{"a": {"base_url": "https://git.example.test/", "auth": "bearer", "token_file": "` + missing + `"}, "b": {"base_url": "https://git.example.test/x"}}`, "same host", "could not be opened"},
		{"an unknown field is still rejected", entry(`"auth": "bearer", "token_files": "` + good + `"`), "token_files", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureSlog(t)
			_, err := gitsync.New(writeConfig(t, tt.config))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want an error containing %q, got %v", tt.want, err)
			}
			if tt.notWant != "" && strings.Contains(err.Error(), tt.notWant) {
				t.Errorf("error %q must not contain %q", err, tt.notWant)
			}
			assertNoSecret(t, "error", err.Error())
			assertNoSecret(t, "logs", logs.String())
		})
	}
}

func TestNewTokenFileSharedByProviders(t *testing.T) {
	tf := writeFile(t, t.TempDir(), "token", tokenSentinel+"-shared\n", 0o600)
	cfg := writeConfig(t, fmt.Sprintf(`{
		"a": {"base_url": "https://git-a.example.test/", "auth": "bearer", "token_file": %[1]q},
		"b": {"base_url": "https://git-b.example.test/", "auth": "basic", "username": "svc", "token_file": %[1]q}
	}`, tf))
	s, err := gitsync.New(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, n := range []string{"a", "b"} {
		if e, _ := s.Get(n); e.Token.Reveal() != tokenSentinel+"-shared" {
			t.Errorf("provider %s token wrong", n)
		}
	}
	var n int
	for _, src := range s.Sources() {
		if src.Path == tf {
			n++
		}
	}
	if n != 1 {
		t.Errorf("shared token file listed %d times in Sources, want once: %v", n, s.Sources())
	}
}

func TestSources(t *testing.T) {
	t.Run("a missing config is listed as not read", func(t *testing.T) {
		path := newStorePath(t)
		s, err := gitsync.New(path)
		if err != nil {
			t.Fatal(err)
		}
		got := s.Sources()
		if len(got) != 1 || got[0].Path != path || got[0].Read {
			t.Errorf("Sources = %v, want the config path as not read", got)
		}
	})

	t.Run("config and token files, sorted, with change detection", func(t *testing.T) {
		dir := t.TempDir()
		tfB := writeFile(t, dir, "b-token", tokenSentinel+"-b", 0o600)
		tfA := writeFile(t, dir, "a-token", tokenSentinel+"-a", 0o600)
		cfg := writeConfig(t, fmt.Sprintf(`{
			"b": {"base_url": "https://git-b.example.test/", "auth": "bearer", "token_file": %q},
			"a": {"base_url": "https://git-a.example.test/", "auth": "bearer", "token_file": %q},
			"c": {"base_url": "https://git-c.example.test/"}
		}`, tfB, tfA))
		first, err := gitsync.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, s := range first.Sources() {
			if !s.Read {
				t.Errorf("%v should have been read", s)
			}
			paths = append(paths, s.Path)
		}
		if want := []string{cfg, tfA, tfB}; !slices.Equal(paths, slices.Sorted(slices.Values(want))) {
			t.Errorf("paths = %v, want %v (sorted)", paths, want)
		}

		same, err := gitsync.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !sourcesEqual(first.Sources(), same.Sources()) {
			t.Error("an unchanged configuration must produce equal Sources")
		}

		if err := os.WriteFile(tfA, []byte(tokenSentinel+"-a-rotated"), 0o600); err != nil {
			t.Fatal(err)
		}
		rotated, err := gitsync.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if sourcesEqual(first.Sources(), rotated.Sources()) {
			t.Error("a rotated token file must change Sources")
		}
	})

	t.Run("Sources never expose a fingerprint when formatted", func(t *testing.T) {
		tf := writeFile(t, t.TempDir(), "token", tokenSentinel+"-fmt", 0o600)
		s, err := gitsync.New(writeConfig(t, tokenFileConfig(t, "p", tf)))
		if err != nil {
			t.Fatal(err)
		}
		for _, src := range s.Sources() {
			for _, rendered := range []string{fmt.Sprint(src), fmt.Sprintf("%v", src), fmt.Sprintf("%+v", src), src.String()} {
				if strings.ContainsAny(rendered, "[]") || len(rendered) > len(src.Path)+len(" (not read)") {
					t.Errorf("formatted Source %q has more than the path", rendered)
				}
			}
		}
	})

	t.Run("a failed load reports the files involved, including one that is not there yet", func(t *testing.T) {
		dir := t.TempDir()
		present := writeFile(t, dir, "present", tokenSentinel+"-present", 0o600)
		later := filepath.Join(dir, "later")
		cfg := writeConfig(t, fmt.Sprintf(`{
			"a": {"base_url": "https://git-a.example.test/", "auth": "bearer", "token_file": %q},
			"b": {"base_url": "https://git-b.example.test/", "auth": "bearer", "token_file": %q}
		}`, present, later))
		_, err := gitsync.New(cfg)
		var le *gitsync.LoadError
		if !errors.As(err, &le) {
			t.Fatalf("want a *LoadError, got %T %v", err, err)
		}
		state := map[string]bool{}
		for _, s := range le.Sources {
			state[s.Path] = s.Read
		}
		if got, ok := state[cfg]; !ok || !got {
			t.Errorf("config should be listed as read: %v", le.Sources)
		}
		if got, ok := state[present]; !ok || !got {
			t.Errorf("the readable token file should be listed as read: %v", le.Sources)
		}
		if got, ok := state[later]; !ok || got {
			t.Errorf("the absent token file should be listed as not read: %v", le.Sources)
		}
	})

	t.Run("a failed load of an unreadable config still names it", func(t *testing.T) {
		cfg := writeConfig(t, `{not json`)
		_, err := gitsync.New(cfg)
		var le *gitsync.LoadError
		if !errors.As(err, &le) || len(le.Sources) != 1 || le.Sources[0].Path != cfg || !le.Sources[0].Read {
			t.Fatalf("got %v / %#v", err, le)
		}
	})

	t.Run("Set and Remove keep Sources describing the file on disk", func(t *testing.T) {
		path := newStorePath(t)
		s, err := gitsync.New(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Set("p", gitsync.LocalEntry{BaseURL: "https://git.example.test/"}); err != nil {
			t.Fatal(err)
		}
		reloaded, err := gitsync.New(path)
		if err != nil {
			t.Fatal(err)
		}
		if !sourcesEqual(s.Sources(), reloaded.Sources()) {
			t.Errorf("after Set: %v vs reloaded %v", s.Sources(), reloaded.Sources())
		}
		if err := s.Remove("p"); err != nil {
			t.Fatal(err)
		}
		reloaded, err = gitsync.New(path)
		if err != nil {
			t.Fatal(err)
		}
		if !sourcesEqual(s.Sources(), reloaded.Sources()) {
			t.Errorf("after Remove: %v vs reloaded %v", s.Sources(), reloaded.Sources())
		}
	})
}

func sourcesEqual(a, b []gitsync.Source) bool { return slices.Equal(a, b) }

// A token-file provider must be written back as its path: rewriting the config for an unrelated
// `config set` or `config remove` must never copy the credential into git-sync.json.
func TestStoreSavePreservesTokenFile(t *testing.T) {
	const tok = tokenSentinel + "-save"
	tf := writeFile(t, t.TempDir(), "token", tok+"\n", 0o600)
	path := writeConfig(t, fmt.Sprintf(`{
		"filed": {"base_url": "https://git-f.example.test/", "auth": "bearer", "token_file": %q},
		"inline": {"base_url": "https://git-i.example.test/", "auth": "bearer", "token": "inline-token-0123456789"}
	}`, tf))
	s, err := gitsync.New(path)
	if err != nil {
		t.Fatal(err)
	}

	check := func(step string) {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), tok) {
			t.Errorf("%s: the token-file credential was copied into the config: %s", step, raw)
		}
		if !strings.Contains(string(raw), tf) {
			t.Errorf("%s: token_file path was not preserved: %s", step, raw)
		}
		reloaded, err := gitsync.New(path)
		if err != nil {
			t.Fatalf("%s: reload: %v", step, err)
		}
		if e, _ := reloaded.Get("filed"); e.Token.Reveal() != tok || e.TokenFile != tf {
			t.Errorf("%s: reloaded entry = %+v", step, e)
		}
	}

	if err := s.Set("other", gitsync.LocalEntry{BaseURL: "https://git-o.example.test/"}); err != nil {
		t.Fatal(err)
	}
	check("after Set")
	if err := s.Remove("inline"); err != nil {
		t.Fatal(err)
	}
	check("after Remove")
}

func TestStoreSetRejectsTokenFile(t *testing.T) {
	s, err := gitsync.New(newStorePath(t))
	if err != nil {
		t.Fatal(err)
	}
	err = s.Set("p", gitsync.LocalEntry{BaseURL: "https://git.example.test/", Auth: "bearer", TokenFile: "/run/secrets/x"})
	if err == nil || !strings.Contains(err.Error(), "token_file") {
		t.Fatalf("want a token_file error, got %v", err)
	}
	if len(s.Names()) != 0 {
		t.Error("a rejected Set must not store the entry")
	}
}

// Replacing a token-file provider through Set is an explicit operator action and persists an inline
// token, as it always has.
func TestStoreSetReplacesTokenFileProviderWithInlineToken(t *testing.T) {
	tf := writeFile(t, t.TempDir(), "token", tokenSentinel+"-replace", 0o600)
	path := writeConfig(t, tokenFileConfig(t, "p", tf))
	s, err := gitsync.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("p", gitsync.LocalEntry{BaseURL: "https://git.example.test/", Auth: "bearer", Token: "replacement-token-0123456789"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "token_file") || !strings.Contains(string(raw), "replacement-token-0123456789") {
		t.Errorf("config = %s", raw)
	}
}

// The group/other-read warning is logged once per file for the life of the process, so a loop that
// reloads the config does not repeat it, and it comes back if the file is loosened again.
func TestNewTokenFileReadWarningIsLoggedOnce(t *testing.T) {
	logs := captureSlog(t)
	tf := writeFile(t, t.TempDir(), "token", tokenSentinel+"-once", 0o644)
	cfg := writeConfig(t, tokenFileConfig(t, "p", tf))
	load := func() {
		t.Helper()
		if _, err := gitsync.New(cfg); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int { return strings.Count(logs.String(), "readable by group or other users") }

	load()
	load()
	load()
	if n := count(); n != 1 {
		t.Fatalf("warnings after three loads = %d, want 1: %q", n, logs)
	}
	if err := os.Chmod(tf, 0o600); err != nil {
		t.Fatal(err)
	}
	load()
	if n := count(); n != 1 {
		t.Fatalf("a tightened file must not warn: %d", n)
	}
	if err := os.Chmod(tf, 0o640); err != nil {
		t.Fatal(err)
	}
	load()
	if n := count(); n != 2 {
		t.Errorf("loosening the file again should warn again: %d", n)
	}
	assertNoSecret(t, "logs", logs.String())
}

func TestNewTokenFileDirectoryWarning(t *testing.T) {
	logs := captureSlog(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	tf := writeFile(t, dir, "token", tokenSentinel+"-dir", 0o600)
	if _, err := gitsync.New(writeConfig(t, tokenFileConfig(t, "p", tf))); err != nil {
		t.Fatalf("a world-writable directory must warn, not reject: %v", err)
	}
	if !strings.Contains(logs.String(), "directory is writable by other users") {
		t.Errorf("expected a directory warning, got %q", logs)
	}
}
