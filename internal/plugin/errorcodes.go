package plugin

// ErrorHeader is the response header that marks an HTTP response as synthesised by
// the connector's request-forwarding handler rather than relayed from the upstream
// service. Its value is one of the machine-readable Err* codes below. Responses
// relayed from the upstream never carry this header (and any value the upstream
// sets is stripped), so the platform can reliably tell a forwarding error the
// connector produced from one the upstream returned — for example to decide whether
// a write request may have reached the upstream.
//
// It does not cover connector responses produced outside the forwarding path, such
// as the backpressure 429 or lifecycle responses; those are not marked yet.
//
// The name is already in canonical form (http.CanonicalHeaderKey(ErrorHeader) ==
// ErrorHeader); consumers must match it case-insensitively per RFC 9110.
const ErrorHeader = "X-Mkonnect-Error"

// Error codes carried by ErrorHeader. Each value is a short, stable, snake_case
// string. They are defined here as constants so other request-forwarding contract
// work can reuse the same vocabulary without restating the literals.
//
// Grouped by what the code tells the platform about whether the request reached
// the upstream:
//
//   - Not forwarded (the connector rejected the request before contacting the
//     upstream, so no write can have happened): ErrCodeProviderNotFound,
//     ErrCodeMethodNotAllowed, ErrCodeInvalidPath, ErrCodePathBlocked,
//     ErrCodeInvalidBaseURL, ErrCodeInvalidCABundle, ErrCodeRequestTooLarge,
//     ErrCodeExpired, and ErrCodeUpstreamUnreachable (connection never established).
//   - Forwarded, response withheld (the upstream was contacted and may have acted,
//     but the connector did not relay its response): ErrCodeResponseTooLarge,
//     ErrCodeUpstreamCredentialLeak.
//   - Forwarded, outcome unknown (the connector cannot tell whether the upstream
//     completed the request): ErrCodeUpstreamTimeout, ErrCodeUpstreamError.
const (
	// ErrCodeUpstreamTimeout: the upstream did not respond within the per-request
	// timeout (connecting, sending, or reading the response). Outcome unknown.
	ErrCodeUpstreamTimeout = "upstream_timeout"
	// ErrCodeUpstreamUnreachable: the connection to the upstream could not be
	// established (dial failure, e.g. connection refused or name resolution
	// failure), so the request almost certainly did not reach the upstream.
	ErrCodeUpstreamUnreachable = "upstream_unreachable"
	// ErrCodeUpstreamError: a transport-level error other than a timeout or a dial
	// failure (e.g. TLS handshake failure, connection reset, or a malformed
	// response). The request may already have been sent, so the outcome is unknown.
	ErrCodeUpstreamError = "upstream_error"
	// ErrCodeResponseTooLarge: the upstream response exceeded the size limit and
	// was withheld. The upstream may already have acted on the request.
	ErrCodeResponseTooLarge = "response_too_large"
	// ErrCodeRequestTooLarge: the request body exceeded the size limit; the request
	// was rejected before being forwarded.
	ErrCodeRequestTooLarge = "request_too_large"
	// ErrCodePathBlocked: the request path was rejected by path-confinement checks
	// (e.g. traversal outside the configured base path). Not forwarded.
	ErrCodePathBlocked = "path_blocked"
	// ErrCodeInvalidPath: the request path was malformed or did not resolve to the
	// configured host. Not forwarded.
	ErrCodeInvalidPath = "invalid_path"
	// ErrCodeProviderNotFound: no plugin backend is configured for the requested
	// provider key. Not forwarded.
	ErrCodeProviderNotFound = "provider_not_found"
	// ErrCodeMethodNotAllowed: the request used an HTTP method the connector does
	// not permit. Not forwarded.
	ErrCodeMethodNotAllowed = "method_not_allowed"
	// ErrCodeInvalidBaseURL: the configured base URL for the provider is invalid
	// (connector misconfiguration). Not forwarded.
	ErrCodeInvalidBaseURL = "invalid_base_url"
	// ErrCodeInvalidCABundle: a CA bundle is configured for the provider's upstream
	// TLS but could not be loaded (missing, unreadable, or contains no certificate).
	// The connector fails closed rather than falling back to the system trust store,
	// so the request is not forwarded (connector misconfiguration).
	ErrCodeInvalidCABundle = "invalid_ca_bundle"
	// ErrCodeUpstreamCredentialLeak: the upstream response reflected locally-held
	// credentials and was withheld to keep them on-prem. The upstream may already
	// have acted on the request.
	ErrCodeUpstreamCredentialLeak = "upstream_credential_leak"
	// ErrCodeExpired: the request carried an expires_at deadline (RFC 3339) that had
	// already passed when the connector received it, so it was not forwarded to the
	// upstream. This guards against the outbound queue redelivering a request the caller
	// has already abandoned after its timeout. A request with no expires_at, or one whose
	// value cannot be parsed, is handled normally (the field is optional and fails open).
	ErrCodeExpired = "expired"
)

// errHeaders returns the response-header map that marks a synthesised response
// with the given code. It is the single place a synthesised response acquires its
// marker, so every connector-produced error is tagged consistently.
func errHeaders(code string) map[string]string {
	return map[string]string{ErrorHeader: code}
}
