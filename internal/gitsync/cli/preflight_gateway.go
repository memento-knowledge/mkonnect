package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/version"
)

// gatewayCheckTimeout bounds each of the name lookup and the TLS connection to the gateway.
const gatewayCheckTimeout = 15 * time.Second

// newGatewayTransport returns the transport the check connects with: a clone of Go's default, which
// is what the connector's own gateway connection uses, so it honours the same proxy variables
// (HTTPS_PROXY, NO_PROXY) and trusts the same roots (the system store, plus SSL_CERT_FILE and
// SSL_CERT_DIR where Go reads them). The CA bundle that applies to git (GIT_SSL_CAINFO) does not
// apply to it, exactly as for the connector. HTTP/2 is switched off because the gateway connection
// is an HTTP/1.1 upgrade, so a load balancer is exercised on the same path.
func newGatewayTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	t.DisableKeepAlives = true
	t.TLSHandshakeTimeout = gatewayCheckTimeout
	return t
}

// checkGateway reports on GATEWAY_URL, the gateway's name, and a TLS connection to it. The
// connection is one HEAD request with no body, no credential and no upgrade, to the gateway's own
// address with any user information, query and fragment removed: it proves the path to the gateway,
// not that the gateway would accept this connector.
func checkGateway(ctx context.Context, env preflightEnv) []checkResult {
	urlRes := checkResult{ID: "gateway-url"}
	u, err := config.ParseGatewayURL(os.Getenv("GATEWAY_URL"))
	if err != nil {
		urlRes.Status = statusFail
		urlRes.Detail = err.Error()
		urlRes.Remedy = "set GATEWAY_URL to the wss:// URL from your installation instructions, copied exactly"
		return []checkResult{urlRes,
			{ID: "gateway-dns", Status: statusSkip, Detail: "not checked: GATEWAY_URL is not valid (see gateway-url)"},
			{ID: "gateway-tls", Status: statusSkip, Detail: "not checked: GATEWAY_URL is not valid (see gateway-url)"}}
	}

	scheme := "https"
	if u.Scheme == "ws" {
		scheme = "http"
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[scheme]
	}
	hostPort := net.JoinHostPort(u.Hostname(), port)
	// The request URL is rebuilt from parts: Go would send URL user information as a Basic
	// Authorization header on its own.
	reqURL := &url.URL{Scheme: scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}

	urlRes.Status = statusPass
	urlRes.Detail = fmt.Sprintf("%s://%s", u.Scheme, hostPort)
	if u.Scheme == "ws" {
		urlRes.Status = statusWarn
		urlRes.Detail += " is a plaintext loopback gateway (ALLOW_INSECURE_GATEWAY=true), for local development only; TLS is not checked"
		urlRes.Remedy = "use the wss:// URL from your installation instructions for a real installation"
	}

	tr := env.transport()
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, reqURL.String(), nil)
	if err != nil {
		return []checkResult{urlRes,
			{ID: "gateway-dns", Status: statusSkip, Detail: "not checked: the request could not be built"},
			{ID: "gateway-tls", Status: statusFail, Detail: "the request could not be built from GATEWAY_URL", Remedy: "check GATEWAY_URL"}}
	}
	req.Header.Set("User-Agent", "mkonnect-gitsync-preflight/"+version.Version)

	var proxyURL *url.URL
	if tr.Proxy != nil {
		var perr error
		if proxyURL, perr = tr.Proxy(req); perr != nil {
			// Go's error repeats the setting, which may carry credentials, so it is not shown.
			return []checkResult{urlRes,
				{ID: "gateway-dns", Status: statusSkip, Detail: "not checked: the proxy settings are not valid (see gateway-tls)"},
				{ID: "gateway-tls", Status: statusFail, Detail: "HTTPS_PROXY / NO_PROXY could not be parsed",
					Remedy: "set HTTPS_PROXY to a URL such as http://proxy.example.net:3128, without credentials, and check NO_PROXY"}}
		}
	}
	dnsRes := checkGatewayDNS(ctx, env, u.Hostname(), proxyURL)
	if dnsRes.Status == statusFail {
		return []checkResult{urlRes, dnsRes, {ID: "gateway-tls", Status: statusSkip, Detail: "not checked: the gateway's name does not resolve (see gateway-dns)"}}
	}
	return []checkResult{urlRes, dnsRes, checkGatewayConn(ctx, tr, req, proxyURL, hostPort, u.Scheme == "ws")}
}

// checkGatewayDNS resolves the gateway's name the way the connector's dial would, unless a proxy is
// in the way, in which case the proxy resolves it and there is nothing to test from here.
func checkGatewayDNS(ctx context.Context, env preflightEnv, host string, proxyURL *url.URL) checkResult {
	return checkNameResolves(ctx, env, nameCheck{id: "gateway-dns", subject: "the gateway", setting: "GATEWAY_URL", connID: "gateway-tls"}, host, proxyURL)
}

// nameCheck says what a name lookup is for, so that the gateway and the replica host share the
// lookup and differ only in the words around it.
type nameCheck struct {
	id      string // the check's id
	subject string // "the gateway"
	setting string // the setting that names the host
	connID  string // the check that shows whether a proxy can reach it
}

func checkNameResolves(ctx context.Context, env preflightEnv, n nameCheck, host string, proxyURL *url.URL) checkResult {
	r := checkResult{ID: n.id}
	switch {
	case proxyURL != nil:
		r.Status = statusPass
		r.Detail = fmt.Sprintf("nothing to look up from here: the proxy at %s resolves %s's name (%s shows whether it can)", proxyURL.Host, n.subject, n.connID)
		return r
	case net.ParseIP(strings.Trim(host, "[]")) != nil:
		r.Status = statusPass
		r.Detail = fmt.Sprintf("nothing to look up: %s names an IP address", n.setting)
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, gatewayCheckTimeout)
	defer cancel()
	addrs, err := env.lookupHost(ctx, host)
	if err != nil || len(addrs) == 0 {
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s did not resolve (%s)", host, dnsReason(err))
		r.Remedy = fmt.Sprintf("check the container's or pod's DNS settings and that the name is reachable from this network; if %s is reached through a proxy, set HTTPS_PROXY (and NO_PROXY) as for the connector", n.subject)
		return r
	}
	r.Status = statusPass
	r.Detail = fmt.Sprintf("%s resolves to %d %s", host, len(addrs), plural(len(addrs), "address", "addresses"))
	return r
}

func dnsReason(err error) string {
	var de *net.DNSError
	switch {
	case err == nil:
		return "no addresses"
	case errors.As(err, &de) && de.IsNotFound:
		return "no such host"
	case errors.As(err, &de) && de.IsTimeout, errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	return "lookup failed"
}

// checkGatewayConn makes the one request and reads the outcome of the connection from it. Any HTTP
// status counts: the gateway is expected to refuse a request that is not a WebSocket upgrade.
func checkGatewayConn(ctx context.Context, tr *http.Transport, req *http.Request, proxyURL *url.URL, hostPort string, plaintext bool) checkResult {
	r := checkResult{ID: "gateway-tls"}
	ctx, cancel := context.WithTimeout(ctx, gatewayCheckTimeout)
	defer cancel()
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		r.Status = statusFail
		r.Detail, r.Remedy = describeGatewayError(err, hostPort, proxyURL)
		return r
	}
	defer resp.Body.Close() //nolint:errcheck

	via := ""
	if proxyURL != nil {
		via = " through the proxy at " + proxyURL.Host
	}
	if plaintext || resp.TLS == nil {
		r.Status = statusWarn
		r.Detail = fmt.Sprintf("%s answered with HTTP %d%s, without TLS", hostPort, resp.StatusCode, via)
		r.Remedy = "use the wss:// URL from your installation instructions for a real installation"
		return r
	}
	r.Status = statusPass
	r.Detail = fmt.Sprintf("%s: TLS %s established%s, certificate trusted; the gateway answered HTTP %d (a refusal of this plain request is expected)",
		hostPort, tlsVersionName(resp.TLS.Version), via, resp.StatusCode)
	return r
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "1.3"
	case tls.VersionTLS12:
		return "1.2"
	}
	return "(older version)"
}

// describeGatewayError turns a transport error into a fixed message and a remedy. The raw text of
// such an error repeats the request or proxy URL, which may carry user information, so it is never
// shown.
func describeGatewayError(err error, hostPort string, proxyURL *url.URL) (detail, remedy string) {
	via := ""
	if proxyURL != nil {
		via = " through the proxy at " + proxyURL.Host
	}
	var (
		proxyOp     *net.OpError
		unknownAuth x509.UnknownAuthorityError
		hostErr     x509.HostnameError
		invalid     x509.CertificateInvalidError
		verifyErr   *tls.CertificateVerificationError
		dnsErr      *net.DNSError
	)
	switch {
	case proxyURL != nil && errors.As(err, &proxyOp) && proxyOp.Op == "proxyconnect":
		return fmt.Sprintf("the proxy at %s could not be reached", proxyURL.Host),
			"check the proxy address in HTTPS_PROXY and that this environment may connect to it"
	case errors.As(err, &unknownAuth):
		return fmt.Sprintf("%s%s: the server's certificate is not signed by a trusted authority", hostPort, via),
			"if the gateway is reached through a TLS-inspecting proxy or has an internal CA, add that CA to the trust store of the environment that runs the connector (mount a PEM file and set SSL_CERT_FILE, or SSL_CERT_DIR); GIT_SSL_CAINFO does not apply to the gateway connection"
	case errors.As(err, &hostErr):
		return fmt.Sprintf("%s%s: the server's certificate is not valid for this host name", hostPort, via),
			"check that GATEWAY_URL is copied exactly as given, and that no proxy or load balancer in between presents another certificate"
	case errors.As(err, &invalid):
		return fmt.Sprintf("%s%s: the server's certificate is not valid (expired, not yet valid, or not allowed for this use)", hostPort, via),
			"check the host's clock first; otherwise the gateway's certificate needs attention, which Memento can confirm"
	case errors.As(err, &verifyErr):
		// What the platform's own verifier reports (for example macOS's) when it is none of the above.
		return fmt.Sprintf("%s%s: the server's certificate could not be verified", hostPort, via),
			"if the gateway is reached through a TLS-inspecting proxy or has an internal CA, add that CA to the trust store of the environment that runs the connector (mount a PEM file and set SSL_CERT_FILE, or SSL_CERT_DIR); GIT_SSL_CAINFO does not apply to the gateway connection"
	case errors.As(err, &dnsErr):
		return fmt.Sprintf("%s%s: the name did not resolve", hostPort, via),
			"check DNS from this environment (see gateway-dns)"
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Sprintf("%s%s: the connection was refused", hostPort, via),
			"allow outbound TCP to this host and port from this environment (firewall, security group, Kubernetes network policy)"
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return fmt.Sprintf("%s%s: the connection timed out", hostPort, via),
			"outbound traffic to the gateway is probably dropped: allow it in the firewall or network policy, or set HTTPS_PROXY if your network requires a proxy"
	case proxyURL != nil && !strings.Contains(err.Error(), "tls:"):
		// Reaching the proxy worked (a refusal or timeout would have matched above), and nothing was
		// negotiated with the gateway, so the proxy did not open the tunnel.
		return fmt.Sprintf("the proxy at %s did not open a tunnel to %s", proxyURL.Host, hostPort),
			"the proxy must allow CONNECT to the gateway; check its allow list and that it needs no credentials (do not put credentials in the proxy URL)"
	case strings.Contains(err.Error(), "tls:"):
		return fmt.Sprintf("%s%s: the TLS handshake failed", hostPort, via),
			"a proxy or middlebox may be interfering with TLS; check what sits between this environment and the gateway"
	}
	return fmt.Sprintf("%s%s: the connection failed", hostPort, via),
		"check the network path from this environment to the gateway: DNS, firewall or network policy, and proxy settings"
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
