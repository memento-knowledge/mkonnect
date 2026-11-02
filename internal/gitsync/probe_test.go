package gitsync

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestClassifyLsRemote(t *testing.T) {
	anErr := errors.New("exit status 128")
	tests := []struct {
		name   string
		stderr string
		err    error
		want   OriginResult
	}{
		{"success", "", nil, OriginReachable},
		{"401 status", "fatal: unable to access 'https://git.example.com/': The requested URL returned error: 401", anErr, OriginAccessDenied},
		{"403 status", "fatal: unable to access 'https://git.example.com/': The requested URL returned error: 403", anErr, OriginAccessDenied},
		{"auth failed text", "fatal: Authentication failed for 'https://git.example.com/'", anErr, OriginAccessDenied},
		{"terminal prompts disabled", "fatal: could not read Username for 'https://git.example.com': terminal prompts disabled", anErr, OriginAccessDenied},
		{"404 returned error reachable", "fatal: unable to access 'https://git.example.com/': The requested URL returned error: 404", anErr, OriginReachable},
		{"404 repository not found reachable", "fatal: repository 'https://git.example.com/' not found", anErr, OriginReachable},
		{"500 reachable", "fatal: unable to access 'https://git.example.com/': The requested URL returned error: 500", anErr, OriginReachable},
		{"resolve host", "fatal: unable to access 'https://git.example.com/': Could not resolve host: git.example.com", anErr, OriginUnreachable},
		{"refused", "fatal: unable to access 'https://git.example.com/': Failed to connect to git.example.com port 443: Connection refused", anErr, OriginUnreachable},
		{"tls", "fatal: unable to access 'https://git.example.com/': SSL certificate problem: unable to get local issuer certificate", anErr, OriginUnreachable},
		{"unknown", "fatal: something unexpected went wrong", anErr, OriginUnreachable},
		// Hostname-poisoning: a reachable host whose name contains a connection keyword must not be
		// misclassified because git echoes the URL in the message.
		{"tls in hostname not found", "fatal: repository 'https://tls.example.com/' not found", anErr, OriginReachable},
		{"ssl in hostname not found", "fatal: repository 'https://ssl-git.example.com/' not found", anErr, OriginReachable},
		{"tls in hostname 302", "fatal: unable to access 'https://tls.example.com/': The requested URL returned error: 302", anErr, OriginReachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyLsRemote(tt.err, []byte(tt.stderr)); got != tt.want {
				t.Errorf("classifyLsRemote(%q) = %q, want %q", tt.stderr, got, tt.want)
			}
		})
	}
}

func TestGitProberInjectsBearerSafely(t *testing.T) {
	token := "secret-token-abcdefghij"
	var gotArgv, gotEnv []string
	p := &gitProber{run: func(_ context.Context, argv, env []string, _ string) ([]byte, error) {
		gotArgv, gotEnv = argv, env
		return nil, nil
	}}
	e := LocalEntry{BaseURL: "https://git.example.com/", Auth: "bearer", Token: Secret(token)}
	if _, err := p.Probe(context.Background(), e); err != nil {
		t.Fatalf("probe: %v", err)
	}

	for _, a := range gotArgv {
		if strings.Contains(a, token) {
			t.Fatalf("token leaked into argv: %v", gotArgv)
		}
	}
	joined := strings.Join(gotArgv, " ")
	if !strings.Contains(joined, "https://git.example.com/") || !strings.Contains(joined, " -- ") {
		t.Errorf("argv missing url or -- separator: %v", gotArgv)
	}

	foundHeader := false
	for _, kv := range gotEnv {
		if strings.HasPrefix(kv, "GIT_CONFIG_KEY_") || strings.HasPrefix(kv, "GIT_CONFIG_VALUE_") {
			if strings.Contains(kv, "Authorization: Bearer "+token) {
				foundHeader = true
			}
			continue
		}
		if strings.Contains(kv, token) {
			t.Errorf("token leaked into env entry: %q", kv)
		}
	}
	if !foundHeader {
		t.Error("bearer token not injected via a GIT_CONFIG_* Authorization header")
	}
	if !strings.Contains(strings.Join(gotEnv, "\n"), "http.followRedirects") {
		t.Error("http.followRedirects=false not set")
	}
}

func TestGitProberBasicAuthHeader(t *testing.T) {
	var gotEnv []string
	p := &gitProber{run: func(_ context.Context, _, env []string, _ string) ([]byte, error) { gotEnv = env; return nil, nil }}
	e := LocalEntry{BaseURL: "https://git.example.com/", Auth: "basic", Username: "svc", Token: Secret("pw-abcdefghijklmnop")}
	if _, err := p.Probe(context.Background(), e); err != nil {
		t.Fatalf("probe: %v", err)
	}
	want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("svc:pw-abcdefghijklmnop"))
	if !strings.Contains(strings.Join(gotEnv, "\n"), want) {
		t.Errorf("basic auth header not found in env")
	}
}

func TestGitProberLocalFaultOnMissingBinary(t *testing.T) {
	p := &gitProber{run: func(_ context.Context, _, _ []string, _ string) ([]byte, error) {
		return nil, &exec.Error{Name: "git", Err: exec.ErrNotFound}
	}}
	res, err := p.Probe(context.Background(), LocalEntry{BaseURL: "https://git.example.com/"})
	if err == nil {
		t.Fatal("a missing git binary should be a local fault (error), not an origin result")
	}
	if res != "" {
		t.Errorf("result should be empty on a local fault, got %q", res)
	}
}

// TestGitProberIntegration exercises the real git binary against a controlled HTTP server. It is
// skipped where git is unavailable.
func TestGitProberIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tests := []struct {
		name   string
		status int
		want   OriginResult
	}{
		{"unauthorized", http.StatusUnauthorized, OriginAccessDenied},
		{"forbidden", http.StatusForbidden, OriginAccessDenied},
		{"not found", http.StatusNotFound, OriginReachable},
		{"server error", http.StatusInternalServerError, OriginReachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			res, err := NewGitProber().Probe(context.Background(), LocalEntry{BaseURL: srv.URL + "/"})
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if res != tt.want {
				t.Errorf("HTTP %d -> %q, want %q", tt.status, res, tt.want)
			}
		})
	}

	t.Run("connection refused", func(t *testing.T) {
		// Port 1 is reserved and not listening: a connection-level failure.
		res, err := NewGitProber().Probe(context.Background(), LocalEntry{BaseURL: "http://127.0.0.1:1/"})
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res != OriginUnreachable {
			t.Errorf("refused connection -> %q, want unreachable", res)
		}
	})
}
