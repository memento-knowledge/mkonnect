package ws_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/ws"
)

// TestSafeDiagnosticStripsInternalAddress verifies that a connection-refused error — whose
// raw text embeds the internal host:port — is reduced to an address-free class string. The
// connector's premise is that internal topology never leaves the network, and probe
// diagnostics are sent to Memento's cloud via test_result/connection_status.
func TestSafeDiagnosticStripsInternalAddress(t *testing.T) {
	// A just-closed server gives us a guaranteed-refused address on localhost.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target := srv.URL
	srv.Close()

	u, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	host, port := u.Hostname(), u.Port()

	_, doErr := (&http.Client{}).Head(target)
	if doErr == nil {
		t.Fatal("expected a connection error to the closed server, got nil")
	}

	// Sanity: the raw error really does leak the address (otherwise this test proves nothing).
	if !strings.Contains(doErr.Error(), port) {
		t.Skipf("raw error unexpectedly omits the port on this platform: %v", doErr)
	}

	diag := ws.SafeDiagnosticForTest(doErr)
	if diag == "" {
		t.Error("diagnostic is empty; want a non-empty failure class")
	}
	if strings.Contains(diag, host) {
		t.Errorf("diagnostic %q leaks the internal host %q", diag, host)
	}
	if strings.Contains(diag, port) {
		t.Errorf("diagnostic %q leaks the internal port %q", diag, port)
	}
	if strings.Contains(diag, target) {
		t.Errorf("diagnostic %q leaks the full target URL", diag)
	}
}

// TestSafeDiagnosticDNSHidesHostname verifies a DNS failure does not surface the hostname
// being resolved (net.DNSError.Error() includes it).
func TestSafeDiagnosticDNSHidesHostname(t *testing.T) {
	const bogus = "mkonnect-nonexistent-host.invalid."
	_, doErr := (&http.Client{}).Head("http://" + bogus + "/")
	if doErr == nil {
		t.Fatal("expected a DNS error for a .invalid host, got nil")
	}

	diag := ws.SafeDiagnosticForTest(doErr)
	if strings.Contains(diag, "invalid") || strings.Contains(diag, bogus) {
		t.Errorf("diagnostic %q leaks the resolved hostname", diag)
	}
	if diag != "DNS resolution failed" {
		t.Errorf("diagnostic = %q, want %q", diag, "DNS resolution failed")
	}
}

// TestSafeDiagnosticNil confirms a nil error yields an empty diagnostic.
func TestSafeDiagnosticNil(t *testing.T) {
	if got := ws.SafeDiagnosticForTest(nil); got != "" {
		t.Errorf("safeDiagnostic(nil) = %q, want empty", got)
	}
}
