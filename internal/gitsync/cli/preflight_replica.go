package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

// The replica check asks whether the pinned replica host can be reached from here: its name
// resolves, and an unauthenticated HTTPS request is answered over a TLS connection git trusts. It
// is made the way the push is, by git, with the same proxy and CA settings (HTTPS_PROXY, NO_PROXY,
// GIT_SSL_CAINFO) and the same locked-down environment, not through the Go transport the gateway
// check uses: git is what pushes, and its trust store is not Go's.
//
// It never carries a credential. The request is built from a provider entry with no authentication
// mode, so git is given no Authorization header and no credential helper, and this process holds no
// push token to send in any case (the platform issues one per repository over the control channel,
// which preflight never opens). Like the origin probe, it reads any HTTP answer as reachable,
// including 401, 403 and 404: a server that asks for a token is exactly what an unauthenticated
// request to a git push endpoint is expected to meet.

// replicaPrereqs are the results the replica checks depend on: the pin must be valid, and, as for
// the origins, git and the temporary directory must work, because the probe runs git in a temporary
// home.
type replicaPrereqs struct {
	replica checkResult // replica-host
	git     checkResult
	tmp     checkResult
}

// checkReplica reports replica-dns and replica-https. A check whose prerequisite failed is skipped
// with the reason rather than run to produce a misleading second failure.
func checkReplica(ctx context.Context, env preflightEnv, pre replicaPrereqs) []checkResult {
	const notChecked = "not checked: "
	dnsSkip := func(why string) checkResult {
		return checkResult{ID: "replica-dns", Status: statusSkip, Detail: notChecked + why}
	}
	httpsSkip := func(why string) checkResult {
		return checkResult{ID: "replica-https", Status: statusSkip, Detail: notChecked + why}
	}

	if pre.replica.Status != statusPass {
		why := gitsync.ReplicaHostEnv + " is not valid (see replica-host)"
		return []checkResult{dnsSkip(why), httpsSkip(why)}
	}
	pin, err := gitsync.ReplicaHostFromEnv()
	if err != nil {
		why := gitsync.ReplicaHostEnv + " is not valid (see replica-host)"
		return []checkResult{dnsSkip(why), httpsSkip(why)}
	}
	host, port, err := net.SplitHostPort(pin)
	if err != nil {
		why := gitsync.ReplicaHostEnv + " is not valid (see replica-host)"
		return []checkResult{dnsSkip(why), httpsSkip(why)}
	}

	// Which proxy applies is read from the same environment variables git reads. If the settings
	// cannot be parsed, the gateway check reports it; this one says only that it is not checked.
	tr := env.transport()
	defer tr.CloseIdleConnections()
	var proxyURL *url.URL
	if tr.Proxy != nil {
		req := &http.Request{URL: &url.URL{Scheme: "https", Host: pin}}
		var perr error
		if proxyURL, perr = tr.Proxy(req); perr != nil {
			why := "the proxy settings are not valid (see gateway-tls)"
			return []checkResult{dnsSkip(why), httpsSkip(why)}
		}
	}

	// Go and git (libcurl) do not read NO_PROXY identically, so whenever an HTTPS proxy is configured
	// at all the name is not looked up from here: whether the proxy is used for this host is git's
	// decision, and replica-https shows what git can reach.
	var dnsRes checkResult
	if proxyURL == nil && (os.Getenv("HTTPS_PROXY") != "" || os.Getenv("https_proxy") != "") {
		dnsRes = checkResult{ID: "replica-dns", Status: statusPass,
			Detail: "nothing to look up from here: an HTTPS proxy is configured, and whether git uses it for this host depends on NO_PROXY (replica-https shows whether it can be reached)"}
	} else {
		dnsRes = checkNameResolves(ctx, env, nameCheck{
			id: "replica-dns", subject: "the replica host", setting: gitsync.ReplicaHostEnv, connID: "replica-https",
		}, host, proxyURL)
	}
	if dnsRes.Status == statusFail {
		return []checkResult{dnsRes, httpsSkip("the replica host's name does not resolve (see replica-dns)")}
	}

	switch {
	case pre.git.Status != statusPass:
		return []checkResult{dnsRes, httpsSkip("git is not usable (see git)")}
	case pre.tmp.Status != statusPass:
		return []checkResult{dnsRes, httpsSkip("the temporary directory is not usable (see tmpdir)")}
	}
	return []checkResult{dnsRes, checkReplicaHTTPS(ctx, env.replicaProber, pin, port, proxyURL)}
}

// checkReplicaHTTPS makes the unauthenticated request with the connector's own origin prober.
func checkReplicaHTTPS(ctx context.Context, prober gitsync.OriginProber, pin, port string, proxyURL *url.URL) checkResult {
	r := checkResult{ID: "replica-https"}
	via := ""
	if proxyURL != nil {
		via = " through the proxy at " + proxyURL.Host
	}
	// No Auth and no token: the entry carries nothing that could become a credential.
	res, err := prober.Probe(ctx, gitsync.LocalEntry{BaseURL: "https://" + pin})
	switch {
	case ctx.Err() != nil && (err != nil || res == gitsync.OriginUnreachable):
		r.Status = statusSkip
		r.Detail = fmt.Sprintf("%s: not checked, because the time limit for this command ran out", pin)
	case err != nil:
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s: the check could not run: %s", pin, localFault(err))
		r.Remedy = "fix the problem named above (see the git and tmpdir checks), then run preflight again"
	case res == gitsync.OriginReachable || res == gitsync.OriginAccessDenied:
		// A 401 or 403 is the replica asking for the push token this request does not carry.
		r.Status = statusPass
		r.Detail = fmt.Sprintf("%s answered an unauthenticated request over HTTPS%s, with a certificate git trusts. Any HTTP answer counts, so this does not show that the server is the replica, and the push itself, which needs a token from the platform, was not tested. No credential was sent", pin, via)
	default:
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s could not be reached over HTTPS%s (connection, proxy or TLS)", pin, via)
		r.Remedy = fmt.Sprintf("allow outbound TCP to the replica host on port %s from where the connector runs (firewall, security group, network policy), and check the proxy settings (HTTPS_PROXY, NO_PROXY); if git reports a certificate problem, note that GIT_SSL_CAINFO replaces git's trust store for the replica too, so its bundle must also contain the public CA that signs the replica's certificate", port)
	}
	return r
}
