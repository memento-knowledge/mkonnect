package gitsync

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
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
