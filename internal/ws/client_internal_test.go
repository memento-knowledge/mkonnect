package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/plugin"
)

func TestProbePluginRejectsCredentialBearingBaseURL(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	const secret = "api-token"
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	urls := []string{
		strings.Replace(upstream.URL, "://", "://local-user:"+secret+"@", 1),
		upstream.URL + "?access_token=" + secret,
		upstream.URL + "#access_token=" + secret,
	}
	for _, baseURL := range urls {
		t.Run(baseURL, func(t *testing.T) {
			if err := store.Set("svc", creds.Credential{BaseURL: baseURL}); err != nil {
				t.Fatalf("store.Set: %v", err)
			}

			result := (&Client{credsStore: store}).probePlugin(context.Background(), "svc")
			if result.Status != "unreachable" {
				t.Fatalf("status = %q, want unreachable", result.Status)
			}
			if strings.Contains(result.Diagnostic, secret) {
				t.Fatalf("URL credential must not be included in diagnostic: %q", result.Diagnostic)
			}
		})
	}
}

func TestProbePluginDoesNotUseAmbientProxy(t *testing.T) {
	if os.Getenv("MKONNECT_PROXY_TEST_CHILD") == "1" {
		store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
		if err != nil {
			t.Fatalf("creds.New: %v", err)
		}
		if err := store.Set("svc", creds.Credential{
			BaseURL:  "http://example.invalid",
			Auth:     "basic",
			Username: "local-user",
			Token:    "local-token",
		}); err != nil {
			t.Fatalf("store.Set: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		_ = (&Client{credsStore: store}).probePlugin(ctx, "svc")
		return
	}

	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestProbePluginDoesNotUseAmbientProxy$", "-test.v")
	cmd.Env = proxyTestEnvironment(proxy.URL)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("proxy child failed: %v\n%s", err, output)
	}
	if proxyCalls.Load() != 0 {
		t.Fatal("connectivity probe sent a local credential-bearing request through HTTP_PROXY")
	}
}

func TestProbePluginUsesAmbientProxyWithoutLocalCredentials(t *testing.T) {
	if os.Getenv("MKONNECT_PROXY_TEST_CHILD") == "1" {
		t.Setenv("PLUGIN_SVC", "http://example.invalid")
		registry, err := plugin.Load(nil)
		if err != nil {
			t.Fatalf("plugin.Load: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		result := (&Client{pluginReg: registry}).probePlugin(ctx, "svc")
		if result.Status != "connected" {
			t.Fatalf("status = %q, want connected through proxy", result.Status)
		}
		return
	}

	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestProbePluginUsesAmbientProxyWithoutLocalCredentials$", "-test.v")
	cmd.Env = proxyTestEnvironment(proxy.URL)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("proxy child failed: %v\n%s", err, output)
	}
	if proxyCalls.Load() != 1 {
		t.Fatalf("proxy calls = %d, want 1 for unauthenticated plugin", proxyCalls.Load())
	}
}

func proxyTestEnvironment(proxyURL string) []string {
	env := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "ALL_PROXY", "HTTP_PROXY", "HTTPS_PROXY", "MKONNECT_PROXY_TEST_CHILD", "NO_PROXY":
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"HTTP_PROXY="+proxyURL,
		"HTTPS_PROXY=",
		"ALL_PROXY=",
		"NO_PROXY=",
		"MKONNECT_PROXY_TEST_CHILD=1",
	)
}
