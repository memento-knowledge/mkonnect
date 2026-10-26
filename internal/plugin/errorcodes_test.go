package plugin

import (
	"net/http"
	"testing"
)

// TestErrorHeaderIsCanonical guards the invariant documented on ErrorHeader: it is
// already in canonical form, so code that stores or strips it by canonical key
// matches the constant directly.
func TestErrorHeaderIsCanonical(t *testing.T) {
	if got := http.CanonicalHeaderKey(ErrorHeader); got != ErrorHeader {
		t.Fatalf("ErrorHeader %q is not canonical; canonical form is %q", ErrorHeader, got)
	}
}

// TestErrorCodeValues pins the wire values. These strings are a published contract
// consumed by the platform, so a change here is a change consumers must coordinate
// with — the test makes an accidental edit fail loudly.
func TestErrorCodeValues(t *testing.T) {
	want := map[string]string{
		"ErrCodeUpstreamTimeout":        "upstream_timeout",
		"ErrCodeUpstreamUnreachable":    "upstream_unreachable",
		"ErrCodeUpstreamError":          "upstream_error",
		"ErrCodeResponseTooLarge":       "response_too_large",
		"ErrCodeRequestTooLarge":        "request_too_large",
		"ErrCodePathBlocked":            "path_blocked",
		"ErrCodeInvalidPath":            "invalid_path",
		"ErrCodeProviderNotFound":       "provider_not_found",
		"ErrCodeMethodNotAllowed":       "method_not_allowed",
		"ErrCodeInvalidBaseURL":         "invalid_base_url",
		"ErrCodeUpstreamCredentialLeak": "upstream_credential_leak",
		"ErrCodeExpired":                "expired",
	}
	got := map[string]string{
		"ErrCodeUpstreamTimeout":        ErrCodeUpstreamTimeout,
		"ErrCodeUpstreamUnreachable":    ErrCodeUpstreamUnreachable,
		"ErrCodeUpstreamError":          ErrCodeUpstreamError,
		"ErrCodeResponseTooLarge":       ErrCodeResponseTooLarge,
		"ErrCodeRequestTooLarge":        ErrCodeRequestTooLarge,
		"ErrCodePathBlocked":            ErrCodePathBlocked,
		"ErrCodeInvalidPath":            ErrCodeInvalidPath,
		"ErrCodeProviderNotFound":       ErrCodeProviderNotFound,
		"ErrCodeMethodNotAllowed":       ErrCodeMethodNotAllowed,
		"ErrCodeInvalidBaseURL":         ErrCodeInvalidBaseURL,
		"ErrCodeUpstreamCredentialLeak": ErrCodeUpstreamCredentialLeak,
		"ErrCodeExpired":                ErrCodeExpired,
	}
	for name, wantVal := range want {
		if got[name] != wantVal {
			t.Errorf("%s = %q, want %q", name, got[name], wantVal)
		}
	}

	// All codes must be distinct: a collision would make two conditions
	// indistinguishable on the wire, defeating the marker's purpose.
	seen := make(map[string]string, len(got))
	for name, val := range got {
		if prev, dup := seen[val]; dup {
			t.Errorf("duplicate code %q used by both %s and %s", val, prev, name)
		}
		seen[val] = name
	}
}

// TestErrHeaders verifies the marker helper produces exactly one header.
func TestErrHeaders(t *testing.T) {
	h := errHeaders(ErrCodePathBlocked)
	if len(h) != 1 || h[ErrorHeader] != ErrCodePathBlocked {
		t.Fatalf("errHeaders = %v, want single %s=%s", h, ErrorHeader, ErrCodePathBlocked)
	}
}
