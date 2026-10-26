package httpclient_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/httpclient"
	"github.com/memento-knowledge/mkonnect/internal/testca"
)

// writeBundle writes PEM to a temp file and returns its path.
func writeBundle(t *testing.T, pem []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca-bundle.pem")
	if err := os.WriteFile(path, pem, 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	return path
}

func TestNewDirectTransportHasNoProxy(t *testing.T) {
	tr := httpclient.NewDirectTransport()
	if tr.Proxy != nil {
		t.Error("direct transport must not consult the ambient proxy settings")
	}
	// Cloning the default transport must preserve HTTP/2 negotiation.
	if !tr.ForceAttemptHTTP2 {
		t.Error("direct transport lost ForceAttemptHTTP2")
	}
}

func TestNewTransportProxyBehaviour(t *testing.T) {
	// direct=false keeps the default transport's Proxy func (honours HTTP(S)_PROXY);
	// direct=true clears it. We assert on the func presence rather than env-driven
	// routing because http.ProxyFromEnvironment caches the environment on first use.
	proxied, err := httpclient.NewTransport(false, "")
	if err != nil {
		t.Fatalf("NewTransport(false, \"\"): %v", err)
	}
	if proxied.Proxy == nil {
		t.Error("non-direct transport must keep the ambient proxy func")
	}
	direct, err := httpclient.NewTransport(true, "")
	if err != nil {
		t.Fatalf("NewTransport(true, \"\"): %v", err)
	}
	if direct.Proxy != nil {
		t.Error("direct transport must not consult the ambient proxy settings")
	}
}

func TestNewTransportCABundleEnablesVerification(t *testing.T) {
	caPEM, leaf, err := testca.GenerateCA("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	defer srv.Close()

	// Without the bundle the internal CA is untrusted: the handshake must fail.
	noCA, err := httpclient.NewTransport(true, "")
	if err != nil {
		t.Fatalf("NewTransport(true, \"\"): %v", err)
	}
	if _, err := (&http.Client{Transport: noCA}).Get(srv.URL); err == nil {
		t.Fatal("expected TLS verification to fail without the CA bundle")
	}

	// With the bundle the server's cert verifies.
	bundlePath := writeBundle(t, caPEM)
	withCA, err := httpclient.NewTransport(true, bundlePath)
	if err != nil {
		t.Fatalf("NewTransport with CA bundle: %v", err)
	}
	resp, err := (&http.Client{Transport: withCA}).Get(srv.URL)
	if err != nil {
		t.Fatalf("request with CA bundle failed: %v", err)
	}
	resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	// The custom bundle must enable verification, never disable it.
	if withCA.TLSClientConfig == nil || withCA.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("CA bundle must set RootCAs with verification enabled, never InsecureSkipVerify")
	}
	if withCA.TLSClientConfig.RootCAs == nil {
		t.Error("CA bundle must install a RootCAs pool")
	}
	if withCA.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Error("CA bundle transport must pin a minimum TLS version of 1.2")
	}
}

func TestNewTransportCABundleErrors(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "does-not-exist.pem")
	if _, err := httpclient.NewTransport(true, missing); err == nil {
		t.Error("expected an error for a missing CA bundle")
	}

	// A directory is not a regular file.
	if _, err := httpclient.NewTransport(true, dir); err == nil {
		t.Error("expected an error for a non-regular CA bundle path")
	}

	// PEM with no certificate (only an unrelated/private block) is rejected, and the
	// error must not echo the file's contents.
	secret := "-----BEGIN PRIVATE KEY-----\nc2VjcmV0\n-----END PRIVATE KEY-----\n"
	noCerts := writeBundle(t, []byte(secret))
	_, err := httpclient.NewTransport(true, noCerts)
	if err == nil {
		t.Fatal("expected an error for a bundle with no certificate")
	}
	for _, leak := range []string{"c2VjcmV0", "PRIVATE KEY", secret} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("CA bundle error leaked file contents: %v", err)
		}
	}
}
