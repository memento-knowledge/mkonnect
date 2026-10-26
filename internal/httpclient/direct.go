// Package httpclient provides HTTP transport configuration for local plugin calls.
package httpclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
)

// maxCABundleBytes caps how much of a configured CA bundle is read. A PEM bundle of
// a handful of roots is a few KiB; the cap only guards against a path that points at
// an endless or enormous file (e.g. a device node) stalling or exhausting memory.
const maxCABundleBytes = 1 << 20 // 1 MiB

// NewDirectTransport returns a clone of Go's default transport that does not
// consult HTTP_PROXY, HTTPS_PROXY, or NO_PROXY. Local credentials must travel
// directly to the configured plugin endpoint. It trusts the system store only;
// use NewTransport to add a custom CA bundle.
func NewDirectTransport() *http.Transport {
	// caBundlePath is empty, so NewTransport cannot fail here.
	t, _ := NewTransport(true, "")
	return t
}

// NewTransport builds an HTTP transport for upstream plugin calls.
//
//   - direct=true clones Go's default transport and clears Proxy, so the request
//     (carrying a local credential) travels straight to the host and never through
//     an ambient HTTP(S)_PROXY. direct=false keeps the default proxy behaviour.
//   - When caBundlePath is non-empty, the named PEM file's certificates are ADDED to
//     the system trust store and installed as the transport's TLS RootCAs, so an
//     internal CA (e.g. a self-managed GitLab's) is trusted in addition to the public
//     roots. An unreadable, non-regular, oversized, or certificate-free bundle is a
//     hard error: the transport is never returned with verification disabled or
//     silently falling back to the system store.
//
// caBundlePath is operator configuration (the credentials file or CLI / CA_BUNDLE
// env), never a value from an inbound gateway message, so it is not attacker-chosen.
func NewTransport(direct bool, caBundlePath string) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if direct {
		transport.Proxy = nil
	}
	if caBundlePath == "" {
		return transport, nil
	}
	pool, err := loadRootCAs(caBundlePath)
	if err != nil {
		return nil, err
	}
	// Clone the default transport's TLS config (if any) before mutating so we never
	// alter shared state, then install the pool with verification enabled.
	cfg := transport.TLSClientConfig
	if cfg == nil {
		cfg = &tls.Config{}
	} else {
		cfg = cfg.Clone()
	}
	cfg.RootCAs = pool
	if cfg.MinVersion < tls.VersionTLS12 {
		cfg.MinVersion = tls.VersionTLS12
	}
	transport.TLSClientConfig = cfg
	return transport, nil
}

// loadRootCAs reads a PEM bundle and returns a cert pool holding the system roots
// plus the bundle's certificates. It fails closed: any read error, a non-regular or
// oversized file, a failure to obtain the system pool, or a bundle with no usable
// certificate returns an error rather than a weaker pool. Error messages reference
// only the path, never the file's contents.
func loadRootCAs(path string) (*x509.CertPool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open CA bundle %q: %w", path, err)
	}
	defer f.Close() //nolint:errcheck
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat CA bundle %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("CA bundle %q is not a regular file", path)
	}
	if info.Size() > maxCABundleBytes {
		return nil, fmt.Errorf("CA bundle %q exceeds %d bytes", path, maxCABundleBytes)
	}
	pemBytes, err := io.ReadAll(io.LimitReader(f, maxCABundleBytes))
	if err != nil {
		return nil, fmt.Errorf("read CA bundle %q: %w", path, err)
	}
	// SystemCertPool returns a non-nil copy on success; an error here (rather than a
	// nil pool) is the only failure mode, and we propagate it instead of falling back
	// to an empty pool, which would silently narrow trust to the bundle alone.
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system cert pool: %w", err)
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("CA bundle %q contains no valid certificate", path)
	}
	return pool, nil
}
