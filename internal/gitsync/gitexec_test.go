package gitsync

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// cfgValue returns the value of the first config pair with the given key.
func cfgValue(cfg [][2]string, key string) (string, bool) {
	for _, kv := range cfg {
		if kv[0] == key {
			return kv[1], true
		}
	}
	return "", false
}

func TestGitBaseEnvLocksDownGit(t *testing.T) {
	env := gitBaseEnv("/tmp/empty-home")
	for _, want := range []string{
		"HOME=/tmp/empty-home",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"LC_ALL=C",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("base env missing %q: %v", want, env)
		}
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_CONFIG_KEY_") || strings.HasPrefix(kv, "GIT_CONFIG_VALUE_") || strings.HasPrefix(kv, "GIT_CONFIG_COUNT") {
			t.Errorf("base env must carry no injected config or credential, found %q", kv)
		}
	}
}

func TestCredConfigScopesHeaderToURL(t *testing.T) {
	const token = "secret-token-abcdefghij"
	const remote = "https://git.example.com/repo-a"

	t.Run("bearer", func(t *testing.T) {
		cfg := credConfig(remote, "bearer", "", Secret(token))
		got, ok := cfgValue(cfg, "http.https://git.example.com/repo-a/.extraHeader")
		if !ok || got != "Authorization: Bearer "+token {
			t.Errorf("bearer header = %q (found=%v), want scoped Bearer header; cfg keys: %v", got, ok, cfg)
		}
		if _, global := cfgValue(cfg, "http.extraHeader"); global {
			t.Error("credential must never be set as the global http.extraHeader")
		}
	})

	t.Run("basic", func(t *testing.T) {
		cfg := credConfig(remote, "basic", "svc-user", Secret(token))
		got, ok := cfgValue(cfg, "http.https://git.example.com/repo-a/.extraHeader")
		want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("svc-user:"+token))
		if !ok || got != want {
			t.Errorf("basic header = %q (found=%v), want %q", got, ok, want)
		}
	})

	t.Run("trailing slash is not doubled", func(t *testing.T) {
		cfg := credConfig("https://git.example.com/", "bearer", "", Secret(token))
		if _, ok := cfgValue(cfg, "http.https://git.example.com/.extraHeader"); !ok {
			t.Errorf("root URL must scope to http.https://git.example.com/.extraHeader, got %v", cfg)
		}
	})

	t.Run("no auth yields only hardening", func(t *testing.T) {
		cfg := credConfig(remote, "", "", "")
		if len(cfg) != 3 {
			t.Fatalf("want exactly the 3 hardening pairs, got %v", cfg)
		}
		for _, key := range []string{"http.followRedirects", "credential.helper", "core.askPass"} {
			if _, ok := cfgValue(cfg, key); !ok {
				t.Errorf("hardening pair %q missing", key)
			}
		}
		if v, _ := cfgValue(cfg, "http.followRedirects"); v != "false" {
			t.Errorf("http.followRedirects = %q, want false", v)
		}
	})
}

func TestCredConfigNeverInArgv(t *testing.T) {
	const token = "secret-token-abcdefghij"
	remote := "https://git.example.com/repo-a"
	argv := []string{"git", "--git-dir=/var/mirrors/abc.git", "fetch", "--prune", "--force", "--", remote, "+refs/heads/*:refs/heads/*"}
	env := gitConfigEnv(gitBaseEnv("/tmp/h"), credConfig(remote, "bearer", "", Secret(token)))

	for _, a := range argv {
		if strings.Contains(a, token) {
			t.Fatalf("token leaked into argv: %v", argv)
		}
	}
	found := false
	for _, kv := range env {
		if strings.Contains(kv, token) {
			if !strings.HasPrefix(kv, "GIT_CONFIG_VALUE_") {
				t.Errorf("token present outside a GIT_CONFIG_VALUE_ entry: %q", kv)
			}
			found = true
		}
	}
	if !found {
		t.Error("token was not injected through the environment")
	}
	if !slices.Contains(env, "GIT_CONFIG_COUNT=4") {
		t.Errorf("GIT_CONFIG_COUNT should be 4 (3 hardening + 1 header), env: %v", env)
	}
}

// TestCredentialHeaderIsScopedToConfiguredURL drives real git to prove the URL-scoped header both
// authenticates a request to the configured URL and is NOT sent to a different host.
func TestCredentialHeaderIsScopedToConfiguredURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	const token = "secret-token-abcdefghij"

	var mu sync.Mutex
	auth := map[string][]string{} // server name -> Authorization headers seen
	record := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			auth[name] = append(auth[name], r.Header.Get("Authorization"))
			mu.Unlock()
			http.NotFound(w, r)
		})
	}
	origin := httptest.NewServer(record("origin"))
	defer origin.Close()
	other := httptest.NewServer(record("other"))
	defer other.Close()

	home, cleanup, err := newTempHome()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	originURL := origin.URL + "/group/repo-a"
	env := gitConfigEnv(gitBaseEnv(home), credConfig(originURL, "bearer", "", Secret(token)))

	// Same env, two targets: the configured URL and a different host.
	_, _ = runGit(context.Background(), []string{"git", "ls-remote", "--", originURL}, env, home)
	_, _ = runGit(context.Background(), []string{"git", "ls-remote", "--", other.URL + "/group/repo-a"}, env, home)

	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(auth["origin"], "Bearer "+token) {
		t.Errorf("configured URL never received the scoped credential; saw %v", auth["origin"])
	}
	for _, h := range auth["other"] {
		if h != "" {
			t.Errorf("credential leaked to a different host: %q", h)
		}
	}
	if len(auth["other"]) == 0 {
		t.Error("test did not exercise the other host")
	}
}

func TestTempRootDefaultsToOSTempDir(t *testing.T) {
	t.Setenv("GIT_SYNC_TMP_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())
	got, err := TempRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != os.TempDir() {
		t.Errorf("TempRoot() = %q, want os.TempDir() = %q", got, os.TempDir())
	}
}

func TestTempRootHonoursEnvAndCreatesPrivateDir(t *testing.T) {
	want := filepath.Join(t.TempDir(), "a", "b", "tmp")
	t.Setenv("GIT_SYNC_TMP_DIR", want)
	got, err := TempRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("TempRoot() = %q, want %q", got, want)
	}
	fi, err := os.Stat(want)
	if err != nil || !fi.IsDir() {
		t.Fatalf("temp root was not created: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("created temp root mode = %o, want 700", perm)
	}
}

func TestTempRootRejectsRelativePath(t *testing.T) {
	t.Setenv("GIT_SYNC_TMP_DIR", "relative/tmp")
	if _, err := TempRoot(); err == nil || !strings.Contains(err.Error(), "GIT_SYNC_TMP_DIR") {
		t.Fatalf("TempRoot() err = %v, want an error naming GIT_SYNC_TMP_DIR", err)
	}
	// newTempHome must fail too, never silently fall back to the default location.
	if _, _, err := newTempHome(); err == nil {
		t.Fatal("newTempHome() accepted a relative GIT_SYNC_TMP_DIR")
	}
}

func TestNewTempHomeLivesUnderConfiguredRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tmp")
	t.Setenv("GIT_SYNC_TMP_DIR", root)
	home, cleanup, err := newTempHome()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(home) != root || !strings.HasPrefix(filepath.Base(home), tempHomePrefix) {
		t.Errorf("home = %q, want a %q* directory directly under %q", home, tempHomePrefix, root)
	}
	cleanup()
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove %q: %v", home, err)
	}
}

func TestGitBaseEnvForwardsTMPDIRAsParentOfHome(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tmp")
	t.Setenv("GIT_SYNC_TMP_DIR", root)
	t.Setenv("TMPDIR", "/should/not/leak")
	home, cleanup, err := newTempHome()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	env := gitBaseEnv(home)
	if want := "TMPDIR=" + filepath.Dir(home); !slices.Contains(env, want) {
		t.Errorf("base env missing %q: %v", want, env)
	}
	if want := "TMPDIR=" + root; !slices.Contains(env, want) {
		t.Errorf("base env missing %q: %v", want, env)
	}
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMPDIR=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want exactly one TMPDIR entry, got %d: %v", n, env)
	}
}

// age sets path's modification time to d in the past.
func age(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestSweepStaleTempHomes(t *testing.T) {
	root := t.TempDir()
	mkdir := func(name string, d time.Duration) string {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(p, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "nested", "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		age(t, p, d)
		return p
	}
	oldA := mkdir("gitsync-home-aaa", time.Hour)
	oldB := mkdir("gitsync-home-bbb", 10*time.Minute)
	young := mkdir("gitsync-home-young", time.Second)
	other := mkdir("unrelated-dir", time.Hour)
	nearMiss := mkdir("xgitsync-home-aaa", time.Hour)

	file := filepath.Join(root, "gitsync-home-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(t, file, time.Hour)

	removed, err := SweepStaleTempHomes(root, time.Minute)
	if err != nil {
		t.Fatalf("SweepStaleTempHomes: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	for _, p := range []string{oldA, oldB} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("stale %q was not removed: %v", p, err)
		}
	}
	for _, p := range []string{young, other, nearMiss, file} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%q must survive the sweep: %v", p, err)
		}
	}
}

func TestSweepStaleTempHomesNeverFollowsSymlinks(t *testing.T) {
	outside := t.TempDir()
	keep := filepath.Join(outside, "precious")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	link := filepath.Join(root, "gitsync-home-x")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	removed, err := SweepStaleTempHomes(root, 0)
	if err != nil {
		t.Fatalf("SweepStaleTempHomes: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 (a symlink is not a temp home)", removed)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("symlink target was touched: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the symlink itself should be left alone: %v", err)
	}
}

func TestSweepStaleTempHomesMissingRootIsNotAnError(t *testing.T) {
	removed, err := SweepStaleTempHomes(filepath.Join(t.TempDir(), "absent"), time.Minute)
	if err != nil || removed != 0 {
		t.Errorf("got (%d, %v), want (0, nil)", removed, err)
	}
}

func TestSweepStaleTempHomesRejectsRelativeRoot(t *testing.T) {
	if _, err := SweepStaleTempHomes("relative", time.Minute); err == nil {
		t.Error("want an error for a relative root")
	}
	if _, err := SweepStaleTempHomes("", time.Minute); err == nil {
		t.Error("want an error for an empty root")
	}
}
