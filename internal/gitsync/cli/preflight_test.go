package cli_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/gitsync/cli"
	"github.com/memento-knowledge/mkonnect/internal/testca"
)

// Synthetic credentials the preflight output must never contain.
const (
	preflightToken = "preflight-token-0123456789abcdef"
	gatewayUserPW  = "gateway-userinfo-pw-9876543210"
	proxyUserPW    = "proxy-userinfo-pw-5555555555"
)

type jsonCheck struct {
	ID     string  `json:"id"`
	Status string  `json:"status"`
	Detail string  `json:"detail"`
	Remedy *string `json:"remedy"`
}

type jsonDoc struct {
	Version int  `json:"version"`
	Ok      bool `json:"ok"`
	Summary struct {
		Pass, Warn, Fail, Skip int
	} `json:"summary"`
	Checks []jsonCheck `json:"checks"`
}

// gatewayFixture is a fake gateway: a TLS server with a throwaway CA that records what reaches it.
type gatewayFixture struct {
	srv  *httptest.Server
	pool *x509.CertPool
	mu   sync.Mutex
	reqs []*http.Request
}

func newGateway(t *testing.T) *gatewayFixture {
	t.Helper()
	caPEM, leaf, err := testca.GenerateCA("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	g := &gatewayFixture{pool: x509.NewCertPool()}
	g.pool.AppendCertsFromPEM(caPEM)
	g.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.reqs = append(g.reqs, r.Clone(context.Background()))
		g.mu.Unlock()
		http.Error(w, "upgrade required", http.StatusUpgradeRequired)
	}))
	g.srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	g.srv.StartTLS()
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gatewayFixture) host() string { return strings.TrimPrefix(g.srv.URL, "https://") }

func (g *gatewayFixture) requests() []*http.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*http.Request(nil), g.reqs...)
}

// trusting is a gateway transport factory that trusts the fixture's CA and uses no proxy.
func (g *gatewayFixture) trusting() func() *http.Transport {
	return func() *http.Transport {
		tr := cli.GatewayTransportForTest()
		tr.Proxy = nil
		tr.TLSClientConfig = &tls.Config{RootCAs: g.pool}
		return tr
	}
}

// reachableReplica answers every probe of the replica host as reachable, and records the entries it
// was given.
type reachableReplica struct {
	mu   sync.Mutex
	seen []gitsync.LocalEntry
}

func (p *reachableReplica) Probe(_ context.Context, e gitsync.LocalEntry) (gitsync.OriginResult, error) {
	p.mu.Lock()
	p.seen = append(p.seen, e)
	p.mu.Unlock()
	return gitsync.OriginReachable, nil
}

// syntheticLookup resolves any name to a documentation address, so no test depends on real DNS.
func syntheticLookup(context.Context, string) ([]string, error) { return []string{"192.0.2.1"}, nil }

// Synthetic registration values; never real ones.
const (
	testConnectorID = "synthetic-git-sync-connector"
	testRegToken    = "synthetic-registration-token-0123456789"
)

// preflightEnv isolates a preflight run: a writable temp dir, a pinned synthetic replica host that
// answers as reachable, a provider config at a fresh path, a connector ID, a registration token file
// and a key directory of its own (and a home directory, so the default key path is never the
// developer's), no proxy variables, and the given gateway.
func preflightEnv(t *testing.T, gatewayURL string) (configPath, tmpDir string) {
	t.Helper()
	configPath = withConfig(t)
	tmpDir = t.TempDir()
	t.Setenv("GIT_SYNC_TMP_DIR", tmpDir)
	t.Setenv(gitsync.ReplicaHostEnv, "replica.example.net")
	t.Setenv("GATEWAY_URL", gatewayURL)
	t.Setenv("ALLOW_INSECURE_GATEWAY", "")
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "GIT_SSL_CAINFO"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONNECTOR_ID", testConnectorID)
	t.Setenv("KEY_FILE", filepath.Join(t.TempDir(), "gitsync-key"))
	tokenFile := filepath.Join(t.TempDir(), "registration-token")
	if err := os.WriteFile(tokenFile, []byte(testRegToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REGISTRATION_TOKEN_FILE", tokenFile)
	t.Setenv("REGISTRATION_TOKEN", "")
	t.Cleanup(cli.SetReplicaProberForTest(&reachableReplica{}))
	t.Cleanup(cli.SetPreflightEnvForTest(nil, nil, syntheticLookup))
	return configPath, tmpDir
}

func writeProviders(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runPreflightJSON runs `preflight --json` and decodes the document. The returned text is the raw
// output, for checks that something is absent.
func runPreflightJSON(t *testing.T) (jsonDoc, string, error) {
	t.Helper()
	out, err := runCLI(t, "", "preflight", "--json")
	var doc jsonDoc
	if jerr := json.Unmarshal([]byte(out), &doc); jerr != nil {
		t.Fatalf("output is not the JSON document: %v\n%s", jerr, out)
	}
	return doc, out, err
}

func (d jsonDoc) check(t *testing.T, id string) jsonCheck {
	t.Helper()
	for _, c := range d.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", id, d.Checks)
	return jsonCheck{}
}

func wantStatus(t *testing.T, d jsonDoc, id, status string) jsonCheck {
	t.Helper()
	c := d.check(t, id)
	if c.Status != status {
		t.Errorf("check %s = %s (%s), want %s", id, c.Status, c.Detail, status)
	}
	return c
}

// stubGit puts a fake git on PATH, and only it, that prints out for any invocation.
func stubGit(t *testing.T, out string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho '" + out + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// onlyBasics replaces everything but git and the temp dir, so those checks can be read alone.
func basicsEnv(t *testing.T) {
	t.Helper()
	g := newGateway(t)
	preflightEnv(t, "wss://"+g.host())
	t.Cleanup(cli.SetPreflightEnvForTest(nil, g.trusting(), nil))
}

func TestPreflightGitVersion(t *testing.T) {
	tests := []struct {
		name, out, status string
	}{
		{"too old", "git version 2.30.2", "fail"},
		{"too old by minor", "git version 2.7.4", "fail"},
		{"too old major", "git version 1.9.1", "fail"},
		{"exact minimum", "git version 2.31.0", "pass"},
		{"minimum without patch", "git version 2.31", "pass"},
		{"plain", "git version 2.39.5", "pass"},
		{"newer major", "git version 3.0.1", "pass"},
		{"apple suffix", "git version 2.39.5 (Apple Git-154)", "pass"},
		{"windows suffix", "git version 2.43.0.windows.1", "pass"},
		{"debian suffix", "git version 2.34.1-1ubuntu1.11", "pass"},
		{"release candidate", "git version 2.31.0-rc1", "pass"},
		{"release candidate without patch", "git version 2.40.rc0", "pass"},
		{"old release candidate", "git version 2.30.0-rc2", "fail"},
		{"not git", "hello", "fail"},
		{"no numbers", "git version unknown", "fail"},
		{"one component", "git version 2", "fail"},
		{"empty", "", "fail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			basicsEnv(t)
			stubGit(t, tt.out)
			doc, _, err := runPreflightJSON(t)
			c := wantStatus(t, doc, "git", tt.status)
			if tt.status == "fail" {
				if err == nil {
					t.Error("a failing check must give a non-zero exit")
				}
				if c.Remedy == nil {
					t.Error("a failure needs a remedy")
				}
			}
		})
	}
}

func TestPreflightGitMissing(t *testing.T) {
	basicsEnv(t)
	t.Setenv("PATH", t.TempDir())
	doc, _, err := runPreflightJSON(t)
	c := wantStatus(t, doc, "git", "fail")
	if !strings.Contains(c.Detail, "not found") || err == nil {
		t.Errorf("detail %q, err %v", c.Detail, err)
	}
	// Nothing is probed with a git that is not there.
	wantStatus(t, doc, "origins", "skip")
}

func TestPreflightTempDir(t *testing.T) {
	t.Run("writable and nothing left behind", func(t *testing.T) {
		basicsEnv(t)
		tmp := os.Getenv("GIT_SYNC_TMP_DIR")
		doc, _, _ := runPreflightJSON(t)
		wantStatus(t, doc, "tmpdir", "pass")
		if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
			t.Errorf("preflight left %d entries behind in the temp dir", len(entries))
		}
	})
	t.Run("missing but creatable warns and is not created", func(t *testing.T) {
		basicsEnv(t)
		missing := filepath.Join(t.TempDir(), "absent", "deeper")
		t.Setenv("GIT_SYNC_TMP_DIR", missing)
		doc, _, _ := runPreflightJSON(t)
		c := wantStatus(t, doc, "tmpdir", "warn")
		if c.Remedy == nil {
			t.Error("a warning needs a remedy")
		}
		if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
			t.Error("preflight must not create the temp dir")
		}
		wantStatus(t, doc, "origins", "skip")
	})
	t.Run("missing and not creatable fails", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can create anywhere")
		}
		basicsEnv(t)
		ro := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GIT_SYNC_TMP_DIR", filepath.Join(ro, "tmp"))
		doc, _, err := runPreflightJSON(t)
		wantStatus(t, doc, "tmpdir", "fail")
		if err == nil {
			t.Error("expected a non-zero exit")
		}
	})
	t.Run("relative path", func(t *testing.T) {
		basicsEnv(t)
		t.Setenv("GIT_SYNC_TMP_DIR", "relative/tmp")
		doc, out, _ := runPreflightJSON(t)
		wantStatus(t, doc, "tmpdir", "fail")
		if strings.Contains(out, "relative/tmp") {
			t.Error("the value of the variable must not be echoed")
		}
	})
	t.Run("a file, not a directory", func(t *testing.T) {
		basicsEnv(t)
		f := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GIT_SYNC_TMP_DIR", f)
		doc, _, _ := runPreflightJSON(t)
		wantStatus(t, doc, "tmpdir", "fail")
	})
	t.Run("not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can write anywhere")
		}
		basicsEnv(t)
		ro := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GIT_SYNC_TMP_DIR", ro)
		doc, _, _ := runPreflightJSON(t)
		c := wantStatus(t, doc, "tmpdir", "fail")
		if strings.Contains(c.Detail, "gitsync-home-") {
			t.Errorf("the failure should not name internal probe paths: %q", c.Detail)
		}
	})
}

func TestPreflightReplicaHost(t *testing.T) {
	tests := []struct {
		name, value, status string
	}{
		{"valid", "replica.example.net", "pass"},
		{"valid with port", "replica.example.net:8443", "pass"},
		{"unset", "", "fail"},
		{"scheme", "https://replica.example.net", "fail"},
		{"path", "replica.example.net/x", "fail"},
		{"userinfo", "user:" + proxyUserPW + "@replica.example.net", "fail"},
		{"trailing newline", "replica.example.net\n", "fail"},
		{"unbracketed ipv6", "::1", "fail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			basicsEnv(t)
			t.Setenv(gitsync.ReplicaHostEnv, tt.value)
			doc, out, err := runPreflightJSON(t)
			c := wantStatus(t, doc, "replica-host", tt.status)
			if tt.status == "fail" {
				if err == nil || c.Remedy == nil {
					t.Error("expected a failure with a remedy and a non-zero exit")
				}
				if strings.Contains(out, proxyUserPW) {
					t.Error("an invalid replica host value must not be echoed")
				}
			}
		})
	}
}

func TestPreflightGateway(t *testing.T) {
	t.Run("reachable with a trusted certificate", func(t *testing.T) {
		g := newGateway(t)
		preflightEnv(t, "wss://"+g.host()+"/ws")
		t.Cleanup(cli.SetPreflightEnvForTest(nil, g.trusting(), nil))
		doc, _, _ := runPreflightJSON(t)
		wantStatus(t, doc, "gateway-url", "pass")
		wantStatus(t, doc, "gateway-dns", "pass") // an IP literal has no name to look up
		c := wantStatus(t, doc, "gateway-tls", "pass")
		if !strings.Contains(c.Detail, "426") {
			t.Errorf("the status the gateway answered with should be reported: %q", c.Detail)
		}
	})

	t.Run("sends nothing but a bare HEAD", func(t *testing.T) {
		g := newGateway(t)
		preflightEnv(t, "wss://svc:"+gatewayUserPW+"@"+g.host()+"/ws?token="+preflightToken+"#frag")
		t.Cleanup(cli.SetPreflightEnvForTest(nil, g.trusting(), nil))
		doc, out, _ := runPreflightJSON(t)
		wantStatus(t, doc, "gateway-tls", "pass")
		reqs := g.requests()
		if len(reqs) != 1 {
			t.Fatalf("the gateway saw %d requests, want 1", len(reqs))
		}
		r := reqs[0]
		if r.Method != http.MethodHead || r.URL.Path != "/ws" || r.URL.RawQuery != "" {
			t.Errorf("request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		for _, h := range []string{"Authorization", "Upgrade", "Connection", "Cookie", "Sec-Websocket-Key"} {
			if v := r.Header.Get(h); v != "" && !(h == "Connection" && v == "close") {
				t.Errorf("header %s = %q must not be sent", h, v)
			}
		}
		if r.ContentLength > 0 {
			t.Error("no body may be sent")
		}
		if strings.Contains(out, gatewayUserPW) || strings.Contains(out, preflightToken) {
			t.Error("userinfo or query from GATEWAY_URL appeared in the output")
		}
	})

	t.Run("untrusted certificate", func(t *testing.T) {
		g := newGateway(t)
		preflightEnv(t, "wss://"+g.host())
		// The production trust store does not know the throwaway CA.
		t.Cleanup(cli.SetPreflightEnvForTest(nil, func() *http.Transport {
			tr := cli.GatewayTransportForTest()
			tr.Proxy = nil
			return tr
		}, nil))
		doc, _, err := runPreflightJSON(t)
		c := wantStatus(t, doc, "gateway-tls", "fail")
		if !(strings.Contains(c.Detail, "trusted authority") || strings.Contains(c.Detail, "could not be verified")) || c.Remedy == nil || !strings.Contains(*c.Remedy, "SSL_CERT_FILE") || err == nil {
			t.Errorf("unexpected failure: %+v err=%v", c, err)
		}
		if g.srv.URL != "" && len(g.requests()) != 0 {
			t.Error("no request may be sent when the certificate is not trusted")
		}
	})

	t.Run("wrong host name in the certificate", func(t *testing.T) {
		g := newGateway(t)
		// Same server, reached by a name the certificate does not cover.
		_, port, _ := net.SplitHostPort(g.host())
		preflightEnv(t, "wss://gateway.example.test:"+port)
		t.Cleanup(cli.SetPreflightEnvForTest(nil, func() *http.Transport {
			tr := g.trusting()()
			tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, g.host())
			}
			return tr
		}, func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }))
		doc, _, _ := runPreflightJSON(t)
		wantStatus(t, doc, "gateway-dns", "pass")
		c := wantStatus(t, doc, "gateway-tls", "fail")
		if !strings.Contains(c.Detail, "not valid for this host name") {
			t.Errorf("detail = %q", c.Detail)
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		g := newGateway(t)
		host := g.host()
		g.srv.Close()
		preflightEnv(t, "wss://"+host)
		t.Cleanup(cli.SetPreflightEnvForTest(nil, g.trusting(), nil))
		doc, _, err := runPreflightJSON(t)
		c := wantStatus(t, doc, "gateway-tls", "fail")
		if !strings.Contains(c.Detail, "refused") || err == nil {
			t.Errorf("detail = %q err=%v", c.Detail, err)
		}
	})

	t.Run("name does not resolve", func(t *testing.T) {
		g := newGateway(t)
		preflightEnv(t, "wss://gateway.example.test")
		t.Cleanup(cli.SetPreflightEnvForTest(nil, g.trusting(), func(context.Context, string) ([]string, error) {
			return nil, &net.DNSError{Err: "no such host", Name: "gateway.example.test", IsNotFound: true}
		}))
		doc, _, err := runPreflightJSON(t)
		c := wantStatus(t, doc, "gateway-dns", "fail")
		if !strings.Contains(c.Detail, "no such host") || err == nil {
			t.Errorf("detail = %q err=%v", c.Detail, err)
		}
		wantStatus(t, doc, "gateway-tls", "skip")
	})

	t.Run("through a proxy", func(t *testing.T) {
		g := newGateway(t)
		var cmu sync.Mutex
		var connects []string
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cmu.Lock()
			connects = append(connects, r.Method+" "+r.Host)
			cmu.Unlock()
			if r.Method != http.MethodConnect {
				http.Error(w, "no", http.StatusBadRequest)
				return
			}
			up, err := net.Dial("tcp", r.Host)
			if err != nil {
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			hj := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			_, _ = c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
			go func() { defer up.Close(); defer c.Close(); _, _ = copyConn(up, c) }()
			go func() { _, _ = copyConn(c, up) }()
		}))
		defer proxy.Close()
		pu, _ := url.Parse(proxy.URL)
		pu.User = url.UserPassword("svc", proxyUserPW)

		preflightEnv(t, "wss://"+g.host())
		t.Cleanup(cli.SetPreflightEnvForTest(nil, func() *http.Transport {
			tr := g.trusting()()
			tr.Proxy = http.ProxyURL(pu)
			return tr
		}, func(context.Context, string) ([]string, error) {
			t.Error("the name must not be looked up locally when a proxy resolves it")
			return nil, nil
		}))
		doc, out, _ := runPreflightJSON(t)
		dns := wantStatus(t, doc, "gateway-dns", "pass")
		if !strings.Contains(dns.Detail, "proxy") {
			t.Errorf("dns detail = %q", dns.Detail)
		}
		c := wantStatus(t, doc, "gateway-tls", "pass")
		if !strings.Contains(c.Detail, "through the proxy at "+pu.Host) {
			t.Errorf("detail = %q", c.Detail)
		}
		cmu.Lock()
		defer cmu.Unlock()
		if len(connects) != 1 || !strings.HasPrefix(connects[0], "CONNECT ") {
			t.Errorf("proxy saw %v", connects)
		}
		if strings.Contains(out, proxyUserPW) {
			t.Error("proxy credentials appeared in the output")
		}
	})

	t.Run("proxy that refuses", func(t *testing.T) {
		g := newGateway(t)
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "denied", http.StatusForbidden)
		}))
		defer proxy.Close()
		pu, _ := url.Parse(proxy.URL)
		pu.User = url.UserPassword("svc", proxyUserPW)
		preflightEnv(t, "wss://"+g.host())
		t.Cleanup(cli.SetPreflightEnvForTest(nil, func() *http.Transport {
			tr := g.trusting()()
			tr.Proxy = http.ProxyURL(pu)
			return tr
		}, nil))
		doc, out, _ := runPreflightJSON(t)
		c := wantStatus(t, doc, "gateway-tls", "fail")
		if !strings.Contains(c.Detail, "did not open a tunnel") {
			t.Errorf("detail = %q", c.Detail)
		}
		if strings.Contains(out, proxyUserPW) {
			t.Error("proxy credentials appeared in the output")
		}
	})

	t.Run("unparsable proxy settings", func(t *testing.T) {
		g := newGateway(t)
		preflightEnv(t, "wss://"+g.host())
		t.Cleanup(cli.SetPreflightEnvForTest(nil, func() *http.Transport {
			tr := g.trusting()()
			tr.Proxy = func(*http.Request) (*url.URL, error) {
				return nil, errors.New(`invalid proxy address "http://svc:` + proxyUserPW + `@"`)
			}
			return tr
		}, nil))
		doc, out, err := runPreflightJSON(t)
		wantStatus(t, doc, "gateway-dns", "skip")
		c := wantStatus(t, doc, "gateway-tls", "fail")
		if !strings.Contains(c.Detail, "could not be parsed") || err == nil || strings.Contains(out, proxyUserPW) {
			t.Errorf("detail = %q err=%v", c.Detail, err)
		}
	})

	t.Run("proxy that cannot be reached", func(t *testing.T) {
		g := newGateway(t)
		dead := httptest.NewServer(http.NotFoundHandler())
		deadURL, _ := url.Parse(dead.URL)
		dead.Close()
		deadURL.User = url.UserPassword("svc", proxyUserPW)
		preflightEnv(t, "wss://"+g.host())
		t.Cleanup(cli.SetPreflightEnvForTest(nil, func() *http.Transport {
			tr := g.trusting()()
			tr.Proxy = http.ProxyURL(deadURL)
			return tr
		}, nil))
		doc, out, _ := runPreflightJSON(t)
		c := wantStatus(t, doc, "gateway-tls", "fail")
		if !strings.Contains(c.Detail, "could not be reached") || strings.Contains(out, proxyUserPW) {
			t.Errorf("detail = %q", c.Detail)
		}
	})

	t.Run("invalid URL", func(t *testing.T) {
		basicsEnv(t)
		t.Setenv("GATEWAY_URL", "https://gateway.example.test")
		doc, _, err := runPreflightJSON(t)
		wantStatus(t, doc, "gateway-url", "fail")
		wantStatus(t, doc, "gateway-dns", "skip")
		wantStatus(t, doc, "gateway-tls", "skip")
		if err == nil {
			t.Error("expected a non-zero exit")
		}
	})

	t.Run("unset", func(t *testing.T) {
		basicsEnv(t)
		t.Setenv("GATEWAY_URL", "")
		doc, _, _ := runPreflightJSON(t)
		wantStatus(t, doc, "gateway-url", "fail")
	})

	t.Run("plaintext loopback is a warning", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
		}))
		defer srv.Close()
		cfg, _ := preflightEnv(t, "ws://"+strings.TrimPrefix(srv.URL, "http://"))
		writeProviders(t, cfg, `{}`)
		t.Setenv("ALLOW_INSECURE_GATEWAY", "true")
		doc, out, err := runPreflightJSON(t)
		wantStatus(t, doc, "gateway-url", "warn")
		wantStatus(t, doc, "gateway-tls", "warn")
		if strings.Contains(out, "WARNING: plaintext loopback gateway explicitly enabled") {
			t.Error("the daemon's startup banner must not be printed by preflight")
		}
		if err != nil {
			t.Errorf("a warning must not fail the run: %v", err)
		}
	})

	t.Run("plaintext not allowed without the opt-in", func(t *testing.T) {
		basicsEnv(t)
		t.Setenv("GATEWAY_URL", "ws://127.0.0.1:9")
		doc, _, _ := runPreflightJSON(t)
		wantStatus(t, doc, "gateway-url", "fail")
	})
}

func copyConn(dst, src net.Conn) (int64, error) {
	buf := make([]byte, 32<<10)
	var n int64
	for {
		m, err := src.Read(buf)
		if m > 0 {
			if _, werr := dst.Write(buf[:m]); werr != nil {
				return n, werr
			}
			n += int64(m)
		}
		if err != nil {
			return n, err
		}
	}
}

func TestPreflightProviderConfig(t *testing.T) {
	t.Run("a missing file at a custom path fails", func(t *testing.T) {
		basicsEnv(t)
		doc, _, err := runPreflightJSON(t)
		wantStatus(t, doc, "provider-config", "fail")
		wantStatus(t, doc, "origins", "skip")
		if err == nil {
			t.Error("expected a non-zero exit")
		}
	})
	t.Run("an invalid file fails and nothing is probed", func(t *testing.T) {
		basicsEnv(t)
		path := os.Getenv("GIT_SYNC_CONFIG_FILE")
		writeProviders(t, path, `{"origin": {"base_url": "ftp://git.example.test/", "auth": "bearer", "token": "`+preflightToken+`"}}`)
		doc, out, _ := runPreflightJSON(t)
		c := wantStatus(t, doc, "provider-config", "fail")
		if !strings.Contains(c.Detail, "base_url") {
			t.Errorf("detail = %q", c.Detail)
		}
		wantStatus(t, doc, "origins", "skip")
		if strings.Contains(out, preflightToken) {
			t.Error("a credential appeared in the output")
		}
	})
	t.Run("no providers warns", func(t *testing.T) {
		basicsEnv(t)
		writeProviders(t, os.Getenv("GIT_SYNC_CONFIG_FILE"), `{}`)
		doc, _, err := runPreflightJSON(t)
		wantStatus(t, doc, "provider-config", "pass")
		wantStatus(t, doc, "origins", "warn")
		if err != nil {
			t.Errorf("warnings must not fail the run: %v", err)
		}
	})
	t.Run("a loose file mode fails, skips the origins, and the file is left alone", func(t *testing.T) {
		basicsEnv(t)
		path := os.Getenv("GIT_SYNC_CONFIG_FILE")
		writeProviders(t, path, `{}`)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		doc, _, _ := runPreflightJSON(t)
		wantStatus(t, doc, "provider-config", "fail")
		wantStatus(t, doc, "origins", "skip")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Errorf("preflight changed the config file's mode to %o", info.Mode().Perm())
		}
	})
}

// recordingProber answers from a table keyed by base URL and records every entry it was given.
type recordingProber struct {
	mu      sync.Mutex
	results map[string]gitsync.OriginResult
	err     error
	seen    []gitsync.LocalEntry
}

func (p *recordingProber) Probe(_ context.Context, e gitsync.LocalEntry) (gitsync.OriginResult, error) {
	p.mu.Lock()
	p.seen = append(p.seen, e)
	p.mu.Unlock()
	if p.err != nil {
		return "", p.err
	}
	return p.results[e.BaseURL], nil
}

func TestPreflightOriginsOutcomes(t *testing.T) {
	basicsEnv(t)
	writeProviders(t, os.Getenv("GIT_SYNC_CONFIG_FILE"), `{
  "alpha": {"base_url": "https://alpha.example.test/", "auth": "bearer", "token": "`+preflightToken+`"},
  "bravo": {"base_url": "https://bravo.example.test/", "auth": "basic", "username": "svc", "token": "`+preflightToken+`"},
  "charlie": {"base_url": "https://charlie.example.test/"},
  "delta": {"base_url": "https://delta.example.test/"},
  "echo": {"base_url": "https://echo.example.test/", "auth": "bearer", "token": "`+preflightToken+`"}
}`)
	p := &recordingProber{results: map[string]gitsync.OriginResult{
		"https://alpha.example.test/":   gitsync.OriginReachable,
		"https://bravo.example.test/":   gitsync.OriginAccessDenied,
		"https://charlie.example.test/": gitsync.OriginReachable,
		"https://delta.example.test/":   gitsync.OriginAccessDenied,
		"https://echo.example.test/":    gitsync.OriginUnreachable,
	}}
	g := newGateway(t)
	t.Setenv("GATEWAY_URL", "wss://"+g.host())
	t.Cleanup(cli.SetPreflightEnvForTest(p, g.trusting(), nil))

	doc, out, err := runPreflightJSON(t)
	if _, e := exec.LookPath("git"); e != nil {
		t.Skip("git not installed")
	}
	a := wantStatus(t, doc, "origin:alpha", "pass")
	if !strings.Contains(a.Detail, "https://alpha.example.test:443") || !strings.Contains(a.Detail, "only if the server asked") {
		t.Errorf("alpha detail = %q", a.Detail)
	}
	b := wantStatus(t, doc, "origin:bravo", "fail")
	if !strings.Contains(b.Detail, "rejected the credential") || b.Remedy == nil || !strings.Contains(*b.Remedy, "config set bravo") {
		t.Errorf("bravo = %+v", b)
	}
	wantStatus(t, doc, "origin:charlie", "pass")
	d := wantStatus(t, doc, "origin:delta", "fail")
	if !strings.Contains(d.Detail, "no credential is configured") {
		t.Errorf("delta detail = %q", d.Detail)
	}
	e := wantStatus(t, doc, "origin:echo", "fail")
	if !strings.Contains(e.Detail, "could not be reached") {
		t.Errorf("echo detail = %q", e.Detail)
	}
	if err == nil {
		t.Error("a failed origin must give a non-zero exit")
	}
	if strings.Contains(out, preflightToken) {
		t.Error("a credential appeared in the output")
	}
	// Reported in provider-name order, whatever order the probes finished in.
	var ids []string
	for _, c := range doc.Checks {
		if strings.HasPrefix(c.ID, "origin:") {
			ids = append(ids, c.ID)
		}
	}
	if strings.Join(ids, ",") != "origin:alpha,origin:bravo,origin:charlie,origin:delta,origin:echo" {
		t.Errorf("order = %v", ids)
	}
	// Each provider's own entry, and no other, is what was probed.
	if len(p.seen) != 5 {
		t.Errorf("probed %d entries, want 5", len(p.seen))
	}
}

func TestPreflightOriginLocalFault(t *testing.T) {
	basicsEnv(t)
	writeProviders(t, os.Getenv("GIT_SYNC_CONFIG_FILE"), `{"alpha": {"base_url": "https://alpha.example.test/", "auth": "bearer", "token": "`+preflightToken+`"}}`)
	g := newGateway(t)
	t.Setenv("GATEWAY_URL", "wss://"+g.host())
	t.Cleanup(cli.SetPreflightEnvForTest(&recordingProber{err: &exec.Error{Name: "git", Err: exec.ErrNotFound}}, g.trusting(), nil))
	doc, out, err := runPreflightJSON(t)
	if _, e := exec.LookPath("git"); e != nil {
		t.Skip("git not installed")
	}
	c := wantStatus(t, doc, "origin:alpha", "fail")
	if !strings.Contains(c.Detail, "could not run") || err == nil || strings.Contains(out, preflightToken) {
		t.Errorf("detail=%q err=%v", c.Detail, err)
	}
}

// tlsOrigin starts a TLS server whose certificate is signed by a throwaway CA and returns it with the
// CA in PEM form.
func tlsOrigin(t *testing.T, h http.Handler, caPEM []byte, leaf tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// trustCA points git at a bundle holding caPEM, the way an operator trusts an internal CA.
func trustCA(t *testing.T, caPEM []byte) {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(bundle, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", bundle)
}

// Real git against local TLS servers: the whole path from the config file to the verdict, and the
// guarantee that a provider's credential goes only to that provider's own server.
func TestPreflightOriginsWithRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	caPEM, leaf, err := testca.GenerateCA("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var otherAuth []string
	other := tlsOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		otherAuth = append(otherAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		http.NotFound(w, r)
	}), caPEM, leaf)

	// One server per provider: the configuration holds one provider per host.
	var authSeen []string
	serve := func(h func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
		return tlsOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			authSeen = append(authSeen, r.Header.Get("Authorization"))
			mu.Unlock()
			h(w, r)
		}), caPEM, leaf)
	}
	found := serve(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	denied := serve(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	redirect := serve(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/stolen", http.StatusFound)
	})

	g := newGateway(t)
	path, tmp := preflightEnv(t, "wss://"+g.host())
	trustCA(t, caPEM)
	t.Cleanup(cli.SetPreflightEnvForTest(nil, g.trusting(), nil))
	dead := httptest.NewTLSServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	writeProviders(t, path, `{
  "found": {"base_url": "`+found.URL+`/", "auth": "bearer", "token": "`+preflightToken+`"},
  "denied": {"base_url": "`+denied.URL+`/", "auth": "bearer", "token": "`+preflightToken+`"},
  "redirect": {"base_url": "`+redirect.URL+`/", "auth": "bearer", "token": "`+preflightToken+`"},
  "refused": {"base_url": "`+deadURL+`/", "auth": "bearer", "token": "`+preflightToken+`"}
}`)
	doc, out, err := runPreflightJSON(t)
	wantStatus(t, doc, "git", "pass")
	wantStatus(t, doc, "origin:found", "pass")
	wantStatus(t, doc, "origin:denied", "fail")
	wantStatus(t, doc, "origin:redirect", "pass") // git is told not to follow the redirect
	wantStatus(t, doc, "origin:refused", "fail")
	if err == nil {
		t.Error("expected a non-zero exit")
	}
	if strings.Contains(out, preflightToken) {
		t.Error("a credential appeared in the output")
	}
	mu.Lock()
	defer mu.Unlock()
	// A redirect to another host is not followed, so the other host never sees the credential.
	for _, a := range otherAuth {
		if a != "" {
			t.Errorf("the credential reached a host the provider is not configured for: %q", a)
		}
	}
	if len(otherAuth) != 0 {
		t.Errorf("the redirect target was contacted %d times; redirects must not be followed", len(otherAuth))
	}
	sawAuth := false
	for _, a := range authSeen {
		sawAuth = sawAuth || a != ""
	}
	if !sawAuth {
		t.Error("the configured host never received the credential, so the probe was not authenticating")
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("preflight left %d entries behind in the temp dir", len(entries))
	}
}

// An origin served over TLS from an internal CA, trusted through git's own CA setting.
func TestPreflightOriginOverTLSWithInternalCA(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	caPEM, leaf, err := testca.GenerateCA("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	origin := tlsOrigin(t, http.NotFoundHandler(), caPEM, leaf)

	g := newGateway(t)
	path, _ := preflightEnv(t, "wss://"+g.host())
	t.Cleanup(cli.SetPreflightEnvForTest(nil, g.trusting(), nil))
	writeProviders(t, path, `{"internal": {"base_url": "`+origin.URL+`/"}}`)

	doc, _, _ := runPreflightJSON(t)
	wantStatus(t, doc, "origin:internal", "fail") // not trusted yet

	trustCA(t, caPEM)
	doc, _, _ = runPreflightJSON(t)
	wantStatus(t, doc, "origin:internal", "pass")
}

func TestPreflightAllPassTextAndExit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	caPEM, leaf, err := testca.GenerateCA("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	origin := tlsOrigin(t, http.NotFoundHandler(), caPEM, leaf)
	g := newGateway(t)
	path, _ := preflightEnv(t, "wss://"+g.host())
	trustCA(t, caPEM)
	t.Cleanup(cli.SetPreflightEnvForTest(nil, g.trusting(), nil))
	writeProviders(t, path, `{"internal": {"base_url": "`+origin.URL+`/", "auth": "bearer", "token": "`+preflightToken+`"}}`)

	out, err := runCLI(t, "", "preflight")
	if err != nil {
		t.Fatalf("preflight: %v\n%s", err, out)
	}
	for _, want := range []string{"PASS  git", "PASS  tmpdir", "PASS  replica-host", "PASS  connector-id", "PASS  key-file", "PASS  registration-token", "PASS  gateway-url", "PASS  gateway-tls", "PASS  replica-dns", "PASS  replica-https", "PASS  provider-config", "PASS  origin:internal", "0 failed, 0 skipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, preflightToken) {
		t.Error("a credential appeared in the output")
	}

	doc, _, err := runPreflightJSON(t)
	if err != nil || !doc.Ok || doc.Version != 1 || doc.Summary.Fail != 0 {
		t.Errorf("json: ok=%v version=%d summary=%+v err=%v", doc.Ok, doc.Version, doc.Summary, err)
	}
	for _, c := range doc.Checks {
		if c.Status == "pass" && c.Remedy != nil {
			t.Errorf("check %s passed but has a remedy", c.ID)
		}
	}
}

func TestPreflightJSONIsStableOnFailure(t *testing.T) {
	basicsEnv(t)
	t.Setenv(gitsync.ReplicaHostEnv, "")
	out, err := runCLI(t, "", "preflight", "--json")
	if err == nil {
		t.Fatal("expected a failure")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "ok", "summary", "checks"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("document lacks %q", k)
		}
	}
	if !strings.Contains(out, `"remedy": null`) {
		t.Error("a check without a remedy must carry an explicit null")
	}
	if !strings.Contains(out, `"ok": false`) {
		t.Error("ok must be false when a check failed")
	}
}

func TestPreflightArguments(t *testing.T) {
	out, err := runCLI(t, "", "preflight", "-h")
	if err != nil || !strings.Contains(out, "usage: mkonnect-gitsync connector preflight") {
		t.Errorf("help: %v %q", err, out)
	}
	if _, err := runCLI(t, "", "preflight", "extra"); err == nil {
		t.Error("an extra argument must be rejected")
	}
	if _, err := runCLI(t, "", "preflight", "--bogus"); err == nil {
		t.Error("an unknown flag must be rejected")
	}
	if _, err := runCLI(t, "", "nope"); err == nil || !strings.Contains(err.Error(), "preflight") {
		t.Errorf("the unknown-command message should list preflight: %v", err)
	}
}
