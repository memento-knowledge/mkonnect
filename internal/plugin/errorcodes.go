package plugin

// ErrorHeader is the response header that marks an HTTP response as synthesised by
// the connector itself rather than relayed from the upstream service. Its value is
// one of the machine-readable Err* codes below. Responses relayed from the upstream
// never carry this header (and any value the upstream sets is stripped), so the
// platform can reliably tell a connector-produced error from an upstream one — for
// example to decide whether a write request may have reached the upstream.
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
//     ErrCodeInvalidBaseURL, ErrCodeRequestTooLarge, ErrCodeExpired, and
//     ErrCodeUpstreamUnreachable (connection never established).
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
	// failure). The request was not sent.
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
	// ErrCodeUpstreamCredentialLeak: the upstream response reflected locally-held
	// credentials and was withheld to keep them on-prem. The upstream may already
	// have acted on the request.
	ErrCodeUpstreamCredentialLeak = "upstream_credential_leak"
	// ErrCodeExpired is reserved for a request that is rejected because it arrived
	// after its deadline. It is part of the vocabulary so consumers can handle it,
	// but the connector does not emit it yet; a later contract item adds the
	// expiry check that produces it.
	ErrCodeExpired = "expired"
)

// errHeaders returns the response-header map that marks a synthesised response
// with the given code. It is the single place a synthesised response acquires its
// marker, so every connector-produced error is tagged consistently.
func errHeaders(code string) map[string]string {
	return map[string]string{ErrorHeader: code}
}
