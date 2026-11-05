package cli_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/gitsync/cli"
	"github.com/memento-knowledge/mkonnect/internal/testca"
)

// scriptedReplica answers a probe of the replica host with a fixed outcome and records the entry.
type scriptedReplica struct {
	mu     sync.Mutex
	result gitsync.OriginResult
	err    error
	seen   []gitsync.LocalEntry
}

func (p *scriptedReplica) Probe(_ context.Context, e gitsync.LocalEntry) (gitsync.OriginResult, error) {
	p.mu.Lock()
	p.seen = append(p.seen, e)
	p.mu.Unlock()
	return p.result, p.err
}

func TestPreflightReplicaOutcomes(t *testing.T) {
	tests := []struct {
		name   string
		result gitsync.OriginResult
		err    error
		status string
	}{
		{"answered", gitsync.OriginReachable, nil, "pass"},
		// A 401 or 403 is the replica asking for the push token this request does not carry.
		{"asks for authentication", gitsync.OriginAccessDenied, nil, "pass"},
		{"unreachable", gitsync.OriginUnreachable, nil, "fail"},
		{"git cannot start", "", errors.New("exec: git: not found"), "fail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readyEnv(t)
			p := &scriptedReplica{result: tt.result, err: tt.err}
			t.Cleanup(cli.SetReplicaProberForTest(p))
			doc, out, err := runPreflightJSON(t)
			wantStatus(t, doc, "replica-dns", "pass")
			c := wantStatus(t, doc, "replica-https", tt.status)
			if tt.status == "fail" && (err == nil || c.Remedy == nil) {
				t.Error("a failure needs a remedy and a non-zero exit")
			}
			if tt.status == "pass" && err != nil {
				t.Errorf("unexpected failure: %v", err)
			}
			if len(p.seen) != 1 {
				t.Fatalf("the replica was probed %d times, want once", len(p.seen))
			}
			// No credential can be in the request: the entry has no authentication mode at all.
			e := p.seen[0]
			if e.BaseURL != "https://replica.example.net:443" || e.Auth != "" || e.Username != "" || e.Token.Reveal() != "" || e.TokenFile != "" {
				t.Errorf("the probe entry carries more than the pinned host: %+v", e)
			}
			if strings.Contains(out, "exec: git") {
				t.Error("a raw error must not be shown")
			}
		})
	}
}

func TestPreflightReplicaPortAndIPLiteral(t *testing.T) {
	t.Run("port", func(t *testing.T) {
		readyEnv(t)
		t.Setenv(gitsync.ReplicaHostEnv, "Replica.Example.NET:8443")
		p := &scriptedReplica{result: gitsync.OriginReachable}
		t.Cleanup(cli.SetReplicaProberForTest(p))
		runPreflightJSON(t)
		if len(p.seen) != 1 || p.seen[0].BaseURL != "https://replica.example.net:8443" {
			t.Errorf("probed %+v", p.seen)
		}
	})
	t.Run("ip literal needs no lookup", func(t *testing.T) {
		readyEnv(t)
		t.Setenv(gitsync.ReplicaHostEnv, "[2001:db8::1]:9443")
		t.Cleanup(cli.SetPreflightEnvForTest(nil, nil, func(context.Context, string) ([]string, error) {
			t.Error("an IP literal must not be looked up")
			return nil, nil
		}))
		doc, _, _ := runPreflightJSON(t)
		c := wantStatus(t, doc, "replica-dns", "pass")
		if !strings.Contains(c.Detail, "IP address") {
			t.Errorf("detail: %q", c.Detail)
		}
	})
}

func TestPreflightReplicaSkipsAndDNS(t *testing.T) {
	t.Run("an invalid pin skips both", func(t *testing.T) {
		readyEnv(t)
		t.Setenv(gitsync.ReplicaHostEnv, "https://replica.example.net/x")
		p := &scriptedReplica{result: gitsync.OriginReachable}
		t.Cleanup(cli.SetReplicaProberForTest(p))
		doc, _, err := runPreflightJSON(t)
		wantStatus(t, doc, "replica-host", "fail")
		wantStatus(t, doc, "replica-dns", "skip")
		wantStatus(t, doc, "replica-https", "skip")
		if err == nil || len(p.seen) != 0 {
			t.Error("nothing is probed without a valid pin")
		}
	})
	t.Run("a name that does not resolve", func(t *testing.T) {
		readyEnv(t)
		p := &scriptedReplica{result: gitsync.OriginReachable}
		t.Cleanup(cli.SetReplicaProberForTest(p))
		t.Cleanup(cli.SetPreflightEnvForTest(nil, nil, func(context.Context, string) ([]string, error) {
			return nil, errors.New("no such host")
		}))
		doc, _, err := runPreflightJSON(t)
		c := wantStatus(t, doc, "replica-dns", "fail")
		wantStatus(t, doc, "replica-https", "skip")
		if err == nil || c.Remedy == nil || len(p.seen) != 0 {
			t.Error("a failed lookup is a failure with a remedy, and stops the probe")
		}
	})
	t.Run("git unusable skips only the request", func(t *testing.T) {
		readyEnv(t)
		stubGit(t, "git version 2.30.0")
		p := &scriptedReplica{result: gitsync.OriginReachable}
		t.Cleanup(cli.SetReplicaProberForTest(p))
		doc, _, _ := runPreflightJSON(t)
		wantStatus(t, doc, "replica-dns", "pass")
		wantStatus(t, doc, "replica-https", "skip")
		if len(p.seen) != 0 {
			t.Error("git is not usable, so the probe must not run")
		}
	})
	t.Run("an HTTPS proxy means no lookup from here", func(t *testing.T) {
		readyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://proxy.example.net:3128")
		t.Setenv("NO_PROXY", "replica.example.net")
		t.Cleanup(cli.SetPreflightEnvForTest(nil, nil, func(context.Context, string) ([]string, error) {
			t.Error("the name must not be looked up while a proxy is configured")
			return nil, nil
		}))
		doc, _, _ := runPreflightJSON(t)
		c := wantStatus(t, doc, "replica-dns", "pass")
		if !strings.Contains(c.Detail, "proxy") {
			t.Errorf("detail: %q", c.Detail)
		}
	})
}

// A real git against a local TLS server standing in for the replica: what the push stack would meet,
// with every kind of HTTP answer, and proof that nothing identifying a credential is sent.
func TestPreflightReplicaWithRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	caPEM, leaf, err := testca.GenerateCA("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	statuses := []struct {
		code   int
		status string
	}{
		{http.StatusOK, "pass"}, // not a git response, but an answer
		{http.StatusUnauthorized, "pass"},
		{http.StatusForbidden, "pass"},
		{http.StatusNotFound, "pass"},
		{http.StatusServiceUnavailable, "pass"},
	}
	for _, tt := range statuses {
		t.Run(http.StatusText(tt.code), func(t *testing.T) {
			var mu sync.Mutex
			var reqs []*http.Request
			srv := tlsOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				reqs = append(reqs, r.Clone(context.Background()))
				mu.Unlock()
				if tt.code == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", `Basic realm="replica"`)
				}
				w.WriteHeader(tt.code)
			}), caPEM, leaf)

			readyEnv(t)
			t.Setenv(gitsync.ReplicaHostEnv, strings.TrimPrefix(srv.URL, "https://"))
			trustCA(t, caPEM)
			t.Cleanup(cli.SetReplicaProberForTest(gitsync.NewGitProber()))
			doc, out, _ := runPreflightJSON(t)
			wantStatus(t, doc, "replica-dns", "pass")
			wantStatus(t, doc, "replica-https", tt.status)

			mu.Lock()
			defer mu.Unlock()
			if len(reqs) == 0 {
				t.Fatal("the replica was never contacted")
			}
			for _, r := range reqs {
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					t.Errorf("unexpected %s request", r.Method)
				}
				for _, h := range []string{"Authorization", "Proxy-Authorization", "Cookie"} {
					if v := r.Header.Get(h); v != "" {
						t.Errorf("the request carried %s", h)
					}
				}
			}
			secretsAbsent(t, out)
		})
	}

	t.Run("a certificate git does not trust", func(t *testing.T) {
		srv := tlsOrigin(t, http.NotFoundHandler(), caPEM, leaf)
		readyEnv(t)
		t.Setenv(gitsync.ReplicaHostEnv, strings.TrimPrefix(srv.URL, "https://"))
		t.Cleanup(cli.SetReplicaProberForTest(gitsync.NewGitProber()))
		doc, _, err := runPreflightJSON(t)
		c := wantStatus(t, doc, "replica-https", "fail")
		if err == nil || c.Remedy == nil || !strings.Contains(*c.Remedy, "GIT_SSL_CAINFO") {
			t.Errorf("want a failure whose remedy mentions GIT_SSL_CAINFO: %v %v", err, c.Remedy)
		}
	})

	t.Run("nothing listening", func(t *testing.T) {
		dead := httptest.NewTLSServer(http.NotFoundHandler())
		addr := strings.TrimPrefix(dead.URL, "https://")
		dead.Close()
		readyEnv(t)
		t.Setenv(gitsync.ReplicaHostEnv, addr)
		trustCA(t, caPEM)
		t.Cleanup(cli.SetReplicaProberForTest(gitsync.NewGitProber()))
		doc, _, err := runPreflightJSON(t)
		wantStatus(t, doc, "replica-https", "fail")
		if err == nil {
			t.Error("expected a failure")
		}
	})
}
