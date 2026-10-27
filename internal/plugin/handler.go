package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/httpclient"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// hopByHopHeaders are headers that describe a single transport hop and must not be
// forwarded to the upstream service. Forwarding Connection/Transfer-Encoding in
// particular enables HTTP request-smuggling (CL.TE) attacks.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Proxy-Connection":    true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// sensitiveResponseHeaders may carry credentials created or used inside the
// customer's network. They must not be sent back through the bridge.
var sensitiveResponseHeaders = map[string]bool{
	"Authorization":       true,
	"Cookie":              true,
	"Proxy-Authorization": true,
	"Set-Cookie":          true,
}

// stripHeaderKey normalises a header name for credential-strip matching: it folds the
// underscore spelling (e.g. Private_token) onto the dash form that some upstreams treat as
// equivalent, then canonicalises — so a lookup matches regardless of casing or separator,
// and a configured name and a caller's name match whichever spelling each uses.
func stripHeaderKey(name string) string {
	return http.CanonicalHeaderKey(strings.ReplaceAll(name, "_", "-"))
}

// stripHeaderSet builds a set of header names keyed by stripHeaderKey.
func stripHeaderSet(names ...string) map[string]bool {
	s := make(map[string]bool, len(names))
	for _, n := range names {
		s[stripHeaderKey(n)] = true
	}
	return s
}

// defaultCredentialHeaders are inbound credential-bearing headers that must never reach
// the upstream when mkonnect applies its own local credential: a buggy or hostile message
// could otherwise add or override credentials the upstream honours. They are always
// stripped for a credentialed plugin; a plugin may strip additional headers via its
// StripHeaders config. Proxy-Authorization is also hop-by-hop; it is listed here to make
// the intent explicit and survive any future edit to hopByHopHeaders. This is not an
// exhaustive list of every service's credential header — add service-specific ones (e.g.
// X-Vault-Token, X-Amz-Security-Token) per plugin via StripHeaders.
var defaultCredentialHeaders = stripHeaderSet(
	"Authorization",
	"Proxy-Authorization",
	"Cookie",
	"PRIVATE-TOKEN",
	"JOB-TOKEN",
	"X-Api-Key",
	"X-Auth-Token",
)

// defaultCredentialQueryParams are inbound URL query parameters that carry credentials for
// some upstreams (e.g. a REST API that honours ?private_token= / ?access_token=, sometimes
// in preference to the Authorization header). They must never reach the upstream when
// mkonnect applies its own local credential — the header defaults above close the header
// vector, these close the query-string vector, so the local credential is the only one the
// upstream honours. Keys are lowercase; matching folds case (see stripQueryParamsFromRaw).
// Like the header list this is not exhaustive — add service-specific names (e.g. a trigger
// token) per plugin via StripQueryParams. Request BODIES are forwarded verbatim and are the
// upstream's boundary (an upstream that also reads a credential from a form or JSON body is
// outside this control), mirroring how the decoded-once path is the boundary for RawPath.
var defaultCredentialQueryParams = map[string]bool{
	"private_token": true,
	"job_token":     true,
	"access_token":  true,
	"token":         true,
}

// stripQueryParamsFromRaw removes query parameters whose name is in strip from a raw
// (still-percent-encoded) query string, returning the result. It preserves the original
// order, bytes, AND separators of the parameters it keeps, and returns rawQuery unchanged
// when nothing matches — so the common no-match path forwards the caller's query verbatim.
//
// Matching mirrors how a permissive upstream reads a parameter name (see
// canonicalQueryParamName), so a credential cannot hide behind an encoding, casing, bracket,
// key-mangling, or separator quirk of the framework behind the plugin. A name that fails to
// percent-decode is dropped (fail closed): forwarding a parameter this filter could not
// interpret would defeat it, and such a value is rejected by strict upstreams anyway.
//
// Separators: pairs are delimited by both '&' and ';'. Go does not treat ';' as a separator,
// but some upstreams (Rack 2.x) do, so a credential hidden after ';' is still removed. When a
// pair is kept, the exact separator byte that preceded it in the input is preserved, so a
// literal ';' inside a surviving value or key (legal per RFC 3986) is not rewritten to '&';
// only when a ';'-delimited sub-token is itself a credential is it dropped (over-stripping
// toward safety for that case). All occurrences of a matched name are removed, so a repeated
// parameter cannot survive. Empty tokens (e.g. from "a&&b") are dropped only once something
// else is stripped; an otherwise-unchanged query is returned verbatim. Defense in depth stops
// at a single decode: an upstream that decodes twice is outside this contract, as for the path.
func stripQueryParamsFromRaw(rawQuery string, strip map[string]bool) string {
	if rawQuery == "" || len(strip) == 0 {
		return rawQuery
	}
	var b strings.Builder
	b.Grow(len(rawQuery))
	changed := false
	start := 0     // start index of the current token
	sep := byte(0) // separator byte that preceded the current token (0 for the first)
	flush := func(end int, nextSep byte) {
		token := rawQuery[start:end]
		if token == "" {
			// An empty token can never be a credential; skip it without marking a change, so
			// an otherwise-untouched query is still returned verbatim below.
			sep = nextSep
			return
		}
		key := token
		if e := strings.IndexByte(token, '='); e >= 0 {
			key = token[:e]
		}
		name, ok := canonicalQueryParamName(key)
		if !ok || strip[name] {
			changed = true
		} else {
			if b.Len() > 0 && sep != 0 {
				b.WriteByte(sep)
			}
			b.WriteString(token)
		}
		sep = nextSep
	}
	for i := 0; i < len(rawQuery); i++ {
		if c := rawQuery[i]; c == '&' || c == ';' {
			flush(i, c)
			start = i + 1
		}
	}
	flush(len(rawQuery), 0)
	if !changed {
		return rawQuery
	}
	return b.String()
}

// canonicalQueryParamName decodes a raw query-parameter key to the normalised form used for
// strip matching, folding the key the way the sloppiest in-scope upstream would so a
// credential cannot slip through on a spelling the upstream still honours:
//   - percent-decoded (so %74oken matches token), failing closed (ok=false) on invalid
//     encoding, which the caller treats as a parameter to drop;
//   - any leading run of '['/']' is removed and the name is cut at the next '[' or ']', so
//     array/operator syntax folds to the base name — token[], token[$ne], [token], ]token
//     and the encoded forms all match token, matching Rack's and Express/qs's own nesting;
//   - '.' and ' ' are folded to '_', which PHP always does to request-parameter names;
//   - lower-cased, since query names are case-sensitive per spec but some upstreams (e.g.
//     ASP.NET) read them case-insensitively.
//
// (The header denylist instead folds '_'<->'-'; the two sides mirror their respective
// upstream-parsing quirks, which is why the normalisations differ.)
func canonicalQueryParamName(key string) (name string, ok bool) {
	decoded, err := url.QueryUnescape(key)
	if err != nil {
		return "", false
	}
	decoded = strings.TrimLeft(decoded, "[]")
	if i := strings.IndexAny(decoded, "[]"); i >= 0 {
		decoded = decoded[:i]
	}
	decoded = strings.NewReplacer(".", "_", " ", "_").Replace(decoded)
	return strings.ToLower(decoded), true
}

// queryStripSet builds the set of query-parameter names (lowercase) to strip for a request.
// A plugin's explicit StripQueryParams are honoured whenever the entry exists; the
// credential-parameter defaults are added only when a local credential is actually applied
// (useDirectTransport), mirroring the header denylist. Returns nil when nothing is to be
// stripped, so stripQueryParamsFromRaw short-circuits and forwards the query verbatim.
func queryStripSet(cred creds.Credential, haveCred, useDirectTransport bool) map[string]bool {
	var set map[string]bool
	if haveCred && len(cred.StripQueryParams) > 0 {
		set = make(map[string]bool, len(cred.StripQueryParams)+len(defaultCredentialQueryParams))
		for _, n := range cred.StripQueryParams {
			set[strings.ToLower(n)] = true
		}
	}
	if useDirectTransport {
		if set == nil {
			set = make(map[string]bool, len(defaultCredentialQueryParams))
		}
		for n := range defaultCredentialQueryParams {
			set[n] = true
		}
	}
	return set
}

const unsafeCredentialResponse = `{"error":"upstream response contains local credentials"}`

// credentialRepresentations returns encoded and complete-authorization forms of
// local credentials that an upstream service could reflect. A username alone is
// not included because it is an identifier rather than an authentication secret.
func credentialRepresentations(cred creds.Credential) []string {
	if cred.Token == "" {
		return nil
	}

	values := []string{cred.Token}
	appendEncoded := func(value string) {
		for _, encoded := range []string{
			url.QueryEscape(value),
			url.PathEscape(value),
			base64.StdEncoding.EncodeToString([]byte(value)),
			base64.RawStdEncoding.EncodeToString([]byte(value)),
			base64.URLEncoding.EncodeToString([]byte(value)),
			base64.RawURLEncoding.EncodeToString([]byte(value)),
		} {
			if encoded != value {
				values = append(values, encoded)
			}
		}
	}
	appendEncoded(cred.Token)
	switch cred.Auth {
	case "bearer":
		authorization := "Bearer " + cred.Token
		values = append(values, authorization)
		appendEncoded(authorization)
	case "basic":
		if cred.Username != "" {
			encoded := base64.StdEncoding.EncodeToString([]byte(cred.Username + ":" + cred.Token))
			raw := cred.Username + ":" + cred.Token
			authorization := "Basic " + encoded
			values = append(values, raw, authorization, encoded)
			appendEncoded(raw)
			appendEncoded(authorization)
			appendEncoded(encoded)
		}
	}
	return values
}

func containsCredentialRepresentation(value string, representations []string) bool {
	for _, representation := range representations {
		if strings.Contains(value, representation) {
			return true
		}
	}
	return false
}

// responseContainsLocalCredentials checks every response field before it can
// become a bridge message. An unsafe response is withheld intact rather than
// applying substring replacements that could corrupt unrelated response data.
func responseContainsLocalCredentials(headers http.Header, body string, cred creds.Credential) bool {
	representations := credentialRepresentations(cred)
	if len(representations) == 0 {
		return false
	}
	if containsCredentialRepresentation(body, representations) {
		return true
	}
	for key, values := range headers {
		if containsCredentialRepresentation(key, representations) {
			return true
		}
		for _, value := range values {
			if containsCredentialRepresentation(value, representations) {
				return true
			}
		}
	}
	return false
}

// isTimeoutError reports whether err, or any error it wraps, is a timeout. Unlike
// os.IsTimeout it walks the %w chain, so it stays correct even after the error has
// been wrapped with fmt.Errorf.
func isTimeoutError(err error) bool {
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) {
		return timeout.Timeout()
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// transportErrorCode classifies an error from client.Do (reached only when the
// request context is not itself done) into an error-marker code:
//   - A dial-phase failure — connection refused or name-resolution failure —
//     means the connection was never established, so the request did not reach
//     the upstream: upstream_unreachable.
//   - Any timeout leaves the outcome unknown: upstream_timeout. Which timeout trips
//     depends on the per-request budget: under a short budget a connect stall is the
//     context deadline (preferred over the dial OpError once the context is done), so a
//     black-holed host is reported as upstream_timeout, not upstream_unreachable — the
//     conservative direction. Under a budget longer than the transport's own connect
//     (30s) or TLS-handshake (10s) ceilings, those ceilings trip first instead: a connect
//     timeout then surfaces as a dial OpError (upstream_unreachable, which is accurate —
//     no connection was established) and a TLS-handshake timeout as upstream_timeout.
//   - Any other transport error — a TLS handshake failure, a reset connection, or
//     a failed proxy CONNECT (whose outer OpError Op is "proxyconnect", so the
//     dial check below does not match it) — means the request may already have
//     been sent: upstream_error.
func transportErrorCode(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return ErrCodeUpstreamUnreachable
	}
	if isTimeoutError(err) {
		return ErrCodeUpstreamTimeout
	}
	return ErrCodeUpstreamError
}

// requestExpired reports whether the inbound request carries an expires_at deadline that has
// already passed at now. It returns the parsed deadline and any parse error so the caller can
// log a skew/format diagnostic without re-parsing:
//
//   - expiresAt == ""       -> (false, zero, nil): no deadline (absent field / older messages).
//   - unparseable value     -> (false, zero, err): malformed; the caller forwards (fail open).
//   - parseable, in future  -> (false, deadline, nil).
//   - parseable, in the past -> (true, deadline, nil).
//
// A value equal to now is NOT expired (now.After is strict). expires_at is RFC 3339 and must
// carry a UTC offset; a value without one does not parse and is treated as absent. Malformed
// values fail open deliberately: expiry is a safety default, not an authorization control (a
// hostile sender would simply omit the field), and rejecting on a platform formatting bug
// would take the write path offline — worse than the earlier behaviour (before expiry
// support) of always forwarding, which the platform's writes already tolerate.
func requestExpired(expiresAt string, now time.Time) (expired bool, deadline time.Time, err error) {
	if expiresAt == "" {
		return false, time.Time{}, nil
	}
	deadline, err = time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return false, time.Time{}, err
	}
	return now.After(deadline), deadline, nil
}

// HTTPPluginHandler processes an inbound HTTPRequestMsg, returning status, response headers,
// body (plain text, nil for empty), and any transport-level error.
type HTTPPluginHandler func(context.Context, proto.HTTPRequestMsg) (statusCode int, headers map[string]string, body *string, err error)

// upstreamTimeout is the default bound on a single proxied request to an internal service,
// applied when a plugin has no per-plugin override (creds.Credential.UpstreamTimeout). The
// effective per-request bound is set as a context deadline, not on the shared client, so one
// cached client can serve plugins with different timeouts (see effectiveUpstreamTimeout and
// HTTPHandler). It is a var (not a const) so tests can shorten the default to exercise the
// timeout path without a real wait.
var upstreamTimeout = 10 * time.Second

// newUpstreamClient builds an *http.Client for proxied plugin calls with the no-follow-
// redirects policy. The per-request timeout is applied by the caller as a context deadline
// (so a shared client can serve plugins with different per-plugin timeouts); Client.Timeout
// is set to creds.MaxUpstreamTimeout only as a belt-and-braces ceiling, so a request that
// somehow reaches Do without a deadline cannot pin a worker slot forever. It can never fire
// before a valid per-request deadline, and if it did the outcome (a timeout) classifies the
// same way. A nil transport uses Go's default transport (which honours the ambient proxy).
// It is the single place that upstream-client construction lives, so the base clients and the
// per-plugin CA-bundle clients stay configured identically.
func newUpstreamClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:   creds.MaxUpstreamTimeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// effectiveUpstreamTimeout returns the per-request upstream timeout for a plugin: the
// plugin's own UpstreamTimeout when configured and valid, otherwise the connector default.
// haveCred reports whether cred was found in the store (a zero Credential has no override).
func effectiveUpstreamTimeout(cred creds.Credential, haveCred bool) time.Duration {
	if haveCred {
		if d, ok := cred.UpstreamTimeoutDuration(); ok {
			return d
		}
	}
	return upstreamTimeout
}

// caClientCacheKey identifies a cached upstream client by transport mode and the
// configured CA-bundle path.
type caClientCacheKey struct {
	direct bool
	path   string
}

type caClientCacheEntry struct {
	client *http.Client
	err    error
}

// caClientCache memoises the *http.Client used for a given (transport mode, CA-bundle
// path), so a per-plugin TLS transport — each with its own connection pool — is built
// once rather than per request. It is invalidated wholesale when the credential store's
// generation changes (a SIGHUP reload or a CLI change), so a rotated or corrected bundle
// is re-read on the next request. Build failures are cached too and logged once per
// generation, so a bad bundle does not log on every request.
type caClientCache struct {
	mu        sync.Mutex
	gen       uint64
	entries   map[caClientCacheKey]caClientCacheEntry
	newClient func(http.RoundTripper) *http.Client
}

// clientFor returns the client for (direct, path) under store generation gen, building
// and caching it (or its build error) on first use. The whole operation holds the lock
// so concurrent first use cannot build two transports with two idle connection pools.
func (c *caClientCache) clientFor(direct bool, path string, gen uint64) (*http.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Only ever move the generation forward: a stale request that observed an older
	// generation than one a concurrent request already applied must not reset the cache
	// back and thrash the transports.
	if c.entries == nil || gen > c.gen {
		// Release idle connections held by the evicted clients so a reload does not leak
		// pooled connections until IdleConnTimeout.
		for _, e := range c.entries {
			if e.client != nil {
				if tr, ok := e.client.Transport.(*http.Transport); ok {
					tr.CloseIdleConnections()
				}
			}
		}
		c.entries = make(map[caClientCacheKey]caClientCacheEntry)
		c.gen = gen
	}
	key := caClientCacheKey{direct: direct, path: path}
	if e, ok := c.entries[key]; ok {
		return e.client, e.err
	}
	transport, err := httpclient.NewTransport(direct, path)
	var entry caClientCacheEntry
	if err != nil {
		entry.err = err
		// Log once per generation per bundle: later requests hit the cached entry. Only
		// the path and error class are logged (consistent with the connector's policy of
		// never surfacing bundle contents); the request sees a generic error body.
		slog.Error("upstream CA bundle could not be loaded; requests for affected plugins will fail",
			"ca_bundle", path, "err", err)
	} else {
		entry.client = c.newClient(transport)
	}
	c.entries[key] = entry
	return entry.client, entry.err
}

// warmCACache primes the cache with the CA transports the current credentials imply, so
// a misconfigured bundle is reported at construction rather than only on the first request
// that needs it. It runs once when the handler is built; after a later reload the cache is
// rebuilt lazily and a bad bundle is logged (once per generation) on first use. It is
// best-effort: failures are logged by clientFor and never block startup, since each
// request still fails closed on its own.
func warmCACache(cache *caClientCache, store *creds.Store, defaultCABundle string) {
	gen := store.Generation()
	seen := make(map[caClientCacheKey]bool)
	warm := func(direct bool, path string) {
		if path == "" {
			return
		}
		key := caClientCacheKey{direct: direct, path: path}
		if seen[key] {
			return
		}
		seen[key] = true
		_, _ = cache.clientFor(direct, path, gen)
	}
	// The global default can apply to either transport mode.
	warm(false, defaultCABundle)
	warm(true, defaultCABundle)
	for _, cred := range store.List() {
		path := defaultCABundle
		if cred.CABundle != "" {
			path = cred.CABundle
		}
		warm(cred.HasAuthorization(), path)
	}
}

// HTTPHandler returns an HTTPPluginHandler that:
//  1. Resolves the plugin's base URL from the creds store first, then the registry.
//  2. Strips caller-supplied credential headers and query parameters, then injects an
//     Authorization header when local credentials are configured, so the local credential
//     is the only one used.
//  3. Applies a custom CA bundle (per-plugin ca_bundle, else the global defaultCABundle)
//     for upstream TLS verification, failing closed if a configured bundle cannot load.
//  4. Forwards the HTTP request and returns status, headers, and body.
//
// defaultCABundle is the global CA-bundle path (CA_BUNDLE env) applied to any plugin
// without its own ca_bundle; empty means the system trust store only.
func HTTPHandler(registry *Registry, store *creds.Store, defaultCABundle string) HTTPPluginHandler {
	httpClient := newUpstreamClient(nil)
	directHTTPClient := newUpstreamClient(httpclient.NewDirectTransport())
	caClients := &caClientCache{newClient: newUpstreamClient}
	warmCACache(caClients, store, defaultCABundle)

	errBody := func(s string) *string { return &s }

	return func(ctx context.Context, msg proto.HTTPRequestMsg) (int, map[string]string, *string, error) {
		// Reject a request whose caller-supplied deadline has already passed before doing
		// anything else: it must not reach the upstream even though the outbound queue may
		// still be redelivering it after the platform's caller timeout. This is terminal and
		// independent of the provider, method, or path, so it is evaluated first and belongs
		// to the "not forwarded" marker group. An absent or unparseable deadline falls
		// through to normal handling (see requestExpired).
		if msg.ExpiresAt != "" {
			now := time.Now()
			expired, deadline, perr := requestExpired(msg.ExpiresAt, now)
			switch {
			case perr != nil:
				// Fail open on a malformed deadline, but surface it: clock skew and platform
				// serialization bugs are only diagnosable on-prem here. The raw value is never
				// logged (only its length) so a large or hostile field cannot amplify logs or
				// inject content.
				slog.Warn("ignoring malformed expires_at on http_request; forwarding as if absent",
					"request_id", msg.RequestID, "expires_at_len", len(msg.ExpiresAt))
			case expired:
				// Logged at Info, not Warn: rejecting an expired request is expected, handled
				// behaviour, and post-outage queue drains can deliver these in bursts — Warn
				// would storm exactly when the feature is doing its job. A constant late_by
				// across many requests is the signature of a skewed connector clock; deadline
				// and late_by are Go-formatted, never the raw value.
				slog.Info("request expired before forwarding",
					"request_id", msg.RequestID, "deadline", deadline, "late_by", now.Sub(deadline))
				return http.StatusGatewayTimeout, errHeaders(ErrCodeExpired),
					errBody(`{"error":"request expired before forwarding"}`), nil
			}
		}

		// Resolve the credential once and reuse it for both base-URL resolution and
		// Authorization injection below (avoids a second store lock per request).
		cred, haveCred := store.Get(msg.ProviderKey)

		// Effective CA bundle for this plugin's upstream TLS: a per-plugin ca_bundle
		// overrides the global default. Both are operator configuration, never a value
		// from the inbound message.
		caBundle := defaultCABundle
		if haveCred && cred.CABundle != "" {
			caBundle = cred.CABundle
		}

		// Resolve base URL: creds store takes priority over registry.
		var baseURL string
		if haveCred && cred.BaseURL != "" {
			baseURL = cred.BaseURL
		} else if u, ok := registry.Get(msg.ProviderKey); ok {
			baseURL = u
		} else {
			b, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("provider %q not found", msg.ProviderKey)})
			s := string(b)
			return 404, errHeaders(ErrCodeProviderNotFound), &s, nil
		}

		// Validate method.
		method := strings.ToUpper(strings.TrimSpace(msg.Method))
		if method == "" {
			method = http.MethodGet
		}
		switch method {
		case "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT":
		default:
			return 405, errHeaders(ErrCodeMethodNotAllowed), errBody(`{"error":"method not allowed"}`), nil
		}

		// Validate path and resolve against base URL, preserving the base path prefix
		// (confinement) and any query string supplied by the caller.
		if !strings.HasPrefix(msg.Path, "/") {
			return 400, errHeaders(ErrCodeInvalidPath), errBody(`{"error":"path must begin with /"}`), nil
		}
		base, err := url.Parse(baseURL)
		if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
			return 500, errHeaders(ErrCodeInvalidBaseURL), errBody(`{"error":"invalid base URL"}`), nil
		}
		// url.Parse correctly splits path and raw query from msg.Path (e.g. "/api?k=v").
		reqRef, err := url.Parse(msg.Path)
		if err != nil {
			return 400, errHeaders(ErrCodeInvalidPath), errBody(`{"error":"invalid path"}`), nil
		}
		// Reject a caller path that carries URL userinfo (e.g. //user:pw@host/x). It begins
		// with "/" so it passes the prefix guard above, and ResolveReference carries ref.User
		// into the resolved target, where Go's HTTP client would turn it into a Basic
		// Authorization header when none is set — letting a caller inject upstream credentials
		// via the path. base.User is rejected above (invalid_base_url); this is the symmetric
		// check on the caller-supplied path.
		if reqRef.User != nil {
			return 400, errHeaders(ErrCodeInvalidPath), errBody(`{"error":"invalid path"}`), nil
		}
		// Capture the caller's escaped path form before we mutate reqRef.Path below. Once
		// Path is reassigned, EscapedPath() would no longer match it and would recompute from
		// the decoded path, losing escapes such as %2F. We re-attach this escaped form to the
		// outbound request further down, but only after the decoded path clears every check.
		callerRawPath := reqRef.EscapedPath()
		// Prepend the base path so a base URL of https://host/api/v1 + /jobs stays confined
		// under /api/v1/ and doesn't resolve to /jobs on the root.
		reqRef.Path = base.Path + reqRef.Path
		target := base.ResolveReference(reqRef)
		// Run every security check on the DECODED, normalized path. Clearing RawPath makes
		// target.String() emit the canonical re-encoding of the decoded path; the caller's
		// escaped form is re-attached only at the end, and only when it is provably
		// equivalent to this checked path.
		target.RawPath = ""
		target.Path = path.Clean(target.Path)
		if target.Host != base.Host {
			return 400, errHeaders(ErrCodeInvalidPath), errBody(`{"error":"invalid path"}`), nil
		}
		// Verify that the normalized path still starts with the configured base path prefix.
		if base.Path != "" && base.Path != "/" {
			if !strings.HasPrefix(target.Path+"/", strings.TrimRight(base.Path, "/")+"/") {
				return 400, errHeaders(ErrCodePathBlocked), errBody(`{"error":"path traversal not allowed"}`), nil
			}
		}

		// Forward the caller's escaped path verbatim so an escaped slash (%2F) or escaped
		// percent (%25) survives to the upstream — some providers encode a reserved byte
		// inside a single path segment (e.g. a nested project path), and the decoded,
		// normalized path built above would otherwise flatten it. This is applied only after all
		// checks on the decoded path have passed, and is gated so it can never widen them:
		//
		// url.URL.EscapedPath() (used by target.String()) returns RawPath verbatim only when
		// RawPath is a valid encoding AND unescape(RawPath) == target.Path; otherwise it
		// recomputes from the decoded Path. We assign the candidate and keep it only if that
		// gate accepts it. So a kept RawPath is guaranteed to single-decode to exactly the
		// confined path we just checked — a single-decoding upstream sees that path and
		// nothing else. A ".."/"."/"//" that path.Clean collapsed, or a byte that is not
		// validly encoded, makes unescape differ and the candidate is dropped, falling back
		// to the decoded path. (A base path that ends in "/" is trimmed so the candidate does
		// not gain a "//" that Clean removed from the decoded join; a base path whose own
		// escaped form is preserved reaches the upstream verbatim too.) Defense in depth: an
		// upstream that decodes twice is outside this contract — the decoded-once path is the
		// boundary — and such input (e.g. %252e%252e) is forwarded verbatim by this handler
		// regardless, exactly as before this change.
		candidateRawPath := strings.TrimSuffix(base.EscapedPath(), "/") + callerRawPath
		target.RawPath = candidateRawPath
		if target.EscapedPath() != candidateRawPath {
			target.RawPath = ""
		}

		// Compute once whether a local credential will be applied; reused for both the
		// query-parameter and header credential-strip decisions below so they cannot drift.
		useDirectTransport := haveCred && cred.HasAuthorization()

		// Strip caller-supplied credential-bearing query parameters before building the
		// outbound request, mirroring the header denylist: when a local credential is applied
		// the defaults are enforced, and a plugin may strip additional names via
		// StripQueryParams (honoured even without a local credential). The local credential is
		// then the only credential the upstream sees in both the headers and the query string.
		// The path allow-rules above still run on the decoded path exactly as before.
		target.RawQuery = stripQueryParamsFromRaw(target.RawQuery, queryStripSet(cred, haveCred, useDirectTransport))
		// A fragment never reaches the wire (RequestURI omits it), but clear it defensively so
		// a future change to request construction cannot forward anything after '#'.
		target.Fragment = ""

		const maxBodyBytes = 32 << 20

		// Build the request body reader from the plain-text string field.
		var reqBodyReader *strings.Reader
		if msg.Body != nil {
			if len(*msg.Body) > maxBodyBytes {
				return 413, errHeaders(ErrCodeRequestTooLarge), errBody(`{"error":"request body too large"}`), nil
			}
			reqBodyReader = strings.NewReader(*msg.Body)
		} else {
			reqBodyReader = strings.NewReader("")
		}

		req, err := http.NewRequestWithContext(ctx, method, target.String(), reqBodyReader)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("build request: %w", err)
		}

		// Decide which inbound credential-bearing headers to drop before forwarding.
		// A plugin's explicit StripHeaders are honoured whenever the entry exists. The
		// credential-header defaults are added only when a local credential is actually
		// applied, so the local credential is the one credential the upstream sees; for a
		// plugin without a local credential a caller-supplied Authorization is intentional
		// (platform-held credentials) and is left untouched.
		var stripHeaders map[string]bool
		if haveCred && len(cred.StripHeaders) > 0 {
			stripHeaders = stripHeaderSet(cred.StripHeaders...)
		}
		if useDirectTransport {
			if stripHeaders == nil {
				stripHeaders = make(map[string]bool, len(defaultCredentialHeaders))
			}
			for h := range defaultCredentialHeaders {
				stripHeaders[h] = true
			}
		}

		// Forward inbound headers from the gateway (cloud-side credentials arrive here),
		// excluding hop-by-hop headers (CL.TE smuggling risk) and the credential-bearing
		// headers selected above. The underscore spelling (e.g. Private_token) is matched
		// against the dash form too, since some upstreams treat the two as equivalent.
		for k, v := range msg.Headers {
			if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
				continue
			}
			if stripHeaders[stripHeaderKey(k)] {
				continue
			}
			req.Header.Set(k, v)
		}

		req.Header.Set("Via", "1.1 mkonnect")

		// Inject local credentials for this configured plugin. The credential-bearing
		// inbound headers were stripped above, so the local credential is the only
		// credential the upstream receives.
		client := httpClient
		if useDirectTransport {
			cred.ApplyAuthorization(req)
			client = directHTTPClient
		}

		// Apply a custom CA bundle for upstream TLS when one is configured for this
		// plugin (or globally). Fail closed: a configured-but-unloadable bundle must
		// never silently fall back to the system trust store. The store generation keys
		// the cache so a reload (SIGHUP) or CLI change re-reads the bundle.
		if caBundle != "" {
			caClient, caErr := caClients.clientFor(useDirectTransport, caBundle, store.Generation())
			if caErr != nil {
				return 500, errHeaders(ErrCodeInvalidCABundle), errBody(`{"error":"invalid CA bundle"}`), nil
			}
			client = caClient
		}

		// Bound the whole exchange (connect, send, and reading the response) with a
		// per-request deadline, honouring this plugin's configured timeout and otherwise the
		// connector default. It is a child of ctx, so platform cancellation still fires; it is
		// deliberately NOT assigned back onto ctx, so the ctx.Err() checks below keep
		// distinguishing a platform cancellation (ctx itself done -> unmarked) from our own
		// deadline (only reqCtx done -> classified as upstream_timeout). The deadline is set
		// here, immediately before Do, so the clock starts where Client.Timeout's did; cancel
		// runs on return, after the response body is fully read inside this closure.
		reqCtx, cancel := context.WithTimeout(ctx, effectiveUpstreamTimeout(cred, haveCred))
		defer cancel()
		req = req.WithContext(reqCtx)

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, nil, ctx.Err()
			}
			// Distinguish the error so the platform can judge whether the request may
			// have reached the upstream: a dial failure means it never did
			// (upstream_unreachable); a timeout or any other transport error means the
			// outcome is unknown. The status stays 504 for compatibility; the marker
			// carries the distinction.
			return http.StatusGatewayTimeout, errHeaders(transportErrorCode(err)), errBody(`{"error":"upstream timeout or connection error"}`), err
		}
		defer resp.Body.Close() //nolint:errcheck

		respBytes, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBodyBytes)+1))
		if err != nil {
			// A done request context is platform-initiated cancellation, not an
			// upstream failure; propagate it as the client.Do path does (unmarked).
			if ctx.Err() != nil {
				return 0, nil, nil, ctx.Err()
			}
			// The per-request timeout also covers reading the response body, so a stall
			// here (after headers arrived) is a case where a write may already have
			// taken effect. Mark it so the platform can tell it apart from a response
			// relayed from the upstream: a read timeout is upstream_timeout, any other
			// read failure upstream_error. The status stays 504 (as on the client.Do
			// path) for compatibility; the marker carries the distinction. The non-nil
			// err is still returned so the ws layer logs it.
			code := ErrCodeUpstreamError
			if isTimeoutError(err) {
				code = ErrCodeUpstreamTimeout
			}
			return http.StatusGatewayTimeout, errHeaders(code), errBody(`{"error":"error reading upstream response"}`), fmt.Errorf("read response: %w", err)
		}
		if len(respBytes) > maxBodyBytes {
			return 413, errHeaders(ErrCodeResponseTooLarge), errBody(`{"error":"upstream response too large"}`), nil
		}
		body := string(respBytes)
		if useDirectTransport && responseContainsLocalCredentials(resp.Header, body, cred) {
			return http.StatusBadGateway, errHeaders(ErrCodeUpstreamCredentialLeak), errBody(unsafeCredentialResponse), nil
		}

		// Collect response headers (first value per header name). ErrorHeader is
		// stripped here so a hostile or compromised upstream cannot forge the
		// connector's synthesised-error marker on a relayed response.
		respHeaders := make(map[string]string, len(resp.Header))
		for k, vs := range resp.Header {
			canonical := http.CanonicalHeaderKey(k)
			if len(vs) > 0 && !sensitiveResponseHeaders[canonical] && canonical != ErrorHeader {
				respHeaders[k] = vs[0]
			}
		}

		return resp.StatusCode, respHeaders, &body, nil
	}
}
