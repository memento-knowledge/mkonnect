package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// captureRequest returns an httptest server that records the inbound request's raw query
// string (what actually reaches the upstream on the wire) and, optionally, its headers.
func captureRequest(t *testing.T, rawQuery *string, hdr *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*rawQuery = r.URL.RawQuery
		if hdr != nil {
			*hdr = r.Header.Clone()
		}
		w.WriteHeader(http.StatusOK)
	}))
}

// --- Unit tests for the query-param stripper itself (package-internal) ---

func TestStripQueryParamsFromRaw(t *testing.T) {
	strip := map[string]bool{"token": true, "private_token": true, "job_token": true, "access_token": true}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"single credential dropped", "token=a", ""},
		{"repeated credential all dropped", "token=a&token=b", ""},
		{"non-credential preserved", "ref=main&per_page=20", "ref=main&per_page=20"},
		{"mixed keeps order and verbatim bytes", "a=1&token=x&b=2", "a=1&b=2"},
		{"verbatim encoding of survivors preserved", "a=%2F&b=c+d", "a=%2F&b=c+d"},
		{"percent-encoded key decoded then matched", "%74oken=x", ""},
		{"encoded equals in key is not a match", "token%3Dx=1", "token%3Dx=1"},
		{"bracket array syntax truncated to base name", "private_token[]=x", ""},
		{"bracket operator syntax truncated to base name", "token[$ne]=x", ""},
		{"encoded brackets truncated too", "private_token%5B%5D=x", ""},
		{"leading bracket key folds to base name", "[token]=x", ""},
		{"leading encoded bracket key folds to base name", "%5Btoken%5D=x", ""},
		{"leading close-bracket key folds to base name", "]token=x", ""},
		{"plural name is not a prefix match", "tokens=1", "tokens=1"},
		{"uppercase folded to match", "TOKEN=x", ""},
		{"mixed case folded to match", "Private_Token=x", ""},
		{"php dot-mangled name folds to underscore", "private.token=x", ""},
		{"php plus-mangled name folds to underscore", "private+token=x", ""},
		{"php encoded-space-mangled name folds to underscore", "private%20token=x", ""},
		{"semicolon separator inspected (Rack 2.x)", "a=1;token=x&b=2", "a=1&b=2"},
		{"semicolon with no credential left verbatim", "a=1;b=2", "a=1;b=2"},
		{"literal semicolon in surviving value preserved", "filter=a;b&token=x", "filter=a;b"},
		{"empty token dropped only once something is stripped", "a=1&&token=x", "a=1"},
		{"undecodable key fails closed (dropped)", "tok%zzen=1", ""},
		{"undecodable key among others dropped, rest kept", "keep=1&tok%zzen=2", "keep=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripQueryParamsFromRaw(tc.in, strip); got != tc.want {
				t.Fatalf("stripQueryParamsFromRaw(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripQueryParamsFromRawEmptyStripSetIsNoOp(t *testing.T) {
	const in = "token=a&private_token=b"
	if got := stripQueryParamsFromRaw(in, nil); got != in {
		t.Fatalf("empty strip set must leave query unchanged: got %q", got)
	}
}

// --- Handler (on-the-wire) tests ---

func TestHTTPHandlerStripsCredentialQueryParamsWhenLocalCredentialApplied(t *testing.T) {
	var rawQuery string
	var hdr http.Header
	srv := captureRequest(t, &rawQuery, &hdr)
	defer srv.Close()

	reg := newRegistry("gitscm", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	if err := store.Set("gitscm", creds.Credential{
		BaseURL:          srv.URL,
		Auth:             "bearer",
		Token:            "local-token-value-abc",
		StripQueryParams: []string{"x_custom"}, // configured extra
	}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	h := HTTPHandler(reg, store, "")

	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "gitscm", Method: http.MethodGet,
		Path: "/api?private_token=p&job_token=j&access_token=a&token=t&x_custom=c&ref=main&per_page=20",
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}

	// The local credential is the only credential the upstream sees.
	if hdr.Get("Authorization") != "Bearer local-token-value-abc" {
		t.Fatalf("Authorization = %q, want only the local credential", hdr.Get("Authorization"))
	}
	// Credential-bearing query params (defaults + the configured extra) are gone; the
	// non-credential params survive, in order.
	if rawQuery != "ref=main&per_page=20" {
		t.Fatalf("forwarded query = %q, want %q", rawQuery, "ref=main&per_page=20")
	}
	for _, leaked := range []string{"private_token", "job_token", "access_token", "token=", "x_custom"} {
		if strings.Contains(rawQuery, leaked) {
			t.Errorf("credential query parameter %q leaked to upstream", leaked)
		}
	}
}

func TestHTTPHandlerStripsCredentialQueryParamBypassVariants(t *testing.T) {
	var rawQuery string
	srv := captureRequest(t, &rawQuery, nil)
	defer srv.Close()

	reg := newRegistry("gitscm", srv.URL)
	store := mustStoreBasicCredential(t, "gitscm", srv.URL, "local-user", "basic-token-value-123")
	h := HTTPHandler(reg, store, "")

	// Casing, repeated params, encoded keys and array/operator bracket syntax must all be
	// stripped; only the genuine non-credential parameter survives.
	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "gitscm", Method: http.MethodGet,
		Path: "/api?token=a&token=b&TOKEN=c&%74oken=d&private_token[]=e&keep=ok",
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}
	if rawQuery != "keep=ok" {
		t.Fatalf("forwarded query = %q, want %q", rawQuery, "keep=ok")
	}
}

func TestHTTPHandlerStripsCredentialQueryParamsAcrossSemicolonSeparator(t *testing.T) {
	var rawQuery string
	srv := captureRequest(t, &rawQuery, nil)
	defer srv.Close()

	reg := newRegistry("gitscm", srv.URL)
	store := mustStoreBasicCredential(t, "gitscm", srv.URL, "local-user", "basic-token-value-123")
	h := HTTPHandler(reg, store, "")

	// A ';' separator is honoured by some upstreams (Rack 2.x, PHP) even though Go does not
	// split on it; the credential hiding after ';' must still be removed.
	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "gitscm", Method: http.MethodGet,
		Path: "/api?a=1;token=secret&b=2",
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}
	if rawQuery != "a=1&b=2" {
		t.Fatalf("forwarded query = %q, want %q", rawQuery, "a=1&b=2")
	}
	if strings.Contains(rawQuery, "secret") {
		t.Errorf("credential value leaked to upstream in query")
	}
}

func TestHTTPHandlerLeavesSemicolonQueryVerbatimWhenNoCredentialPresent(t *testing.T) {
	var rawQuery string
	srv := captureRequest(t, &rawQuery, nil)
	defer srv.Close()

	reg := newRegistry("gitscm", srv.URL)
	store := mustStoreBasicCredential(t, "gitscm", srv.URL, "local-user", "basic-token-value-123")
	h := HTTPHandler(reg, store, "")

	// No credential parameter is present, so the query is forwarded byte-for-byte — the ';'
	// is not rewritten to '&'.
	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "gitscm", Method: http.MethodGet,
		Path: "/api?a=1;b=2",
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}
	if rawQuery != "a=1;b=2" {
		t.Fatalf("forwarded query = %q, want it verbatim %q", rawQuery, "a=1;b=2")
	}
}

func TestHTTPHandlerKeepsQueryParamsWithoutLocalCredential(t *testing.T) {
	var rawQuery string
	srv := captureRequest(t, &rawQuery, nil)
	defer srv.Close()

	// Registry-only plugin: no local credential, so a caller-supplied credential parameter
	// (a platform-held credential) is forwarded unchanged — the issue's "keep current
	// behaviour" case.
	reg := newRegistry("open", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	h := HTTPHandler(reg, store, "")

	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "open", Method: http.MethodGet,
		Path: "/?token=caller&ref=x",
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}
	if rawQuery != "token=caller&ref=x" {
		t.Fatalf("forwarded query = %q, want the caller query unchanged", rawQuery)
	}
}

func TestHTTPHandlerStripsExplicitQueryParamsWithoutLocalCredential(t *testing.T) {
	var rawQuery string
	srv := captureRequest(t, &rawQuery, nil)
	defer srv.Close()

	// A plugin entry with no auth but an explicit strip_query_params list: the explicit
	// parameter is dropped, while the credential-param defaults are NOT applied (no local
	// credential), so a caller-supplied token still passes through.
	reg := newRegistry("open", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	if err := store.Set("open", creds.Credential{BaseURL: srv.URL, StripQueryParams: []string{"x_custom"}}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	h := HTTPHandler(reg, store, "")

	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "open", Method: http.MethodGet,
		Path: "/?x_custom=c&token=caller&ref=x",
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}
	if rawQuery != "token=caller&ref=x" {
		t.Fatalf("forwarded query = %q, want explicit param dropped but defaults not applied", rawQuery)
	}
}

func TestHTTPHandlerDropsQueryAndFragmentWithLocalCredential(t *testing.T) {
	var rawQuery string
	srv := captureRequest(t, &rawQuery, nil)
	defer srv.Close()

	reg := newRegistry("gitscm", srv.URL)
	store := mustStoreBasicCredential(t, "gitscm", srv.URL, "local-user", "basic-token-value-123")
	h := HTTPHandler(reg, store, "")

	// Stripping the sole credential parameter empties the query (the '?' is dropped). A
	// fragment is never placed on the wire by Go's client regardless of this change; this
	// asserts the observable result (empty query, and nothing after '#' reaching the server).
	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "gitscm", Method: http.MethodGet,
		Path: "/api?token=a#token=secret",
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}
	if rawQuery != "" {
		t.Fatalf("forwarded query = %q, want empty", rawQuery)
	}
}

func TestHTTPHandlerStripsLeadingBracketAndMangledQueryParams(t *testing.T) {
	var rawQuery string
	srv := captureRequest(t, &rawQuery, nil)
	defer srv.Close()

	reg := newRegistry("gitscm", srv.URL)
	store := mustStoreBasicCredential(t, "gitscm", srv.URL, "local-user", "basic-token-value-123")
	h := HTTPHandler(reg, store, "")

	// A leading-bracket key (Rack 2.x / Express+qs fold it to the base name) and a PHP
	// dot-mangled key must both be stripped; only the genuine parameter survives.
	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "gitscm", Method: http.MethodGet,
		Path: "/api?[token]=a&private.token=b&keep=ok",
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}
	if rawQuery != "keep=ok" {
		t.Fatalf("forwarded query = %q, want %q", rawQuery, "keep=ok")
	}
}

func TestHTTPHandlerDropsUndecodableQueryKeyWhenCredentialApplied(t *testing.T) {
	var rawQuery string
	srv := captureRequest(t, &rawQuery, nil)
	defer srv.Close()

	reg := newRegistry("gitscm", srv.URL)
	store := mustStoreBasicCredential(t, "gitscm", srv.URL, "local-user", "basic-token-value-123")
	h := HTTPHandler(reg, store, "")

	// A key that is not valid percent-encoding cannot be interpreted, so it is dropped (fail
	// closed) rather than forwarded past the credential filter.
	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "gitscm", Method: http.MethodGet,
		Path: "/api?tok%zzen=1&keep=ok",
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}
	if rawQuery != "keep=ok" {
		t.Fatalf("forwarded query = %q, want %q", rawQuery, "keep=ok")
	}
}

func TestHTTPHandlerExplicitQueryParamMatchesCaseInsensitively(t *testing.T) {
	var rawQuery string
	srv := captureRequest(t, &rawQuery, nil)
	defer srv.Close()

	reg := newRegistry("gitscm", srv.URL)
	store, err := creds.New(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("creds.New: %v", err)
	}
	// Configured with mixed-case; a lower-case caller key must still match (matching folds
	// case on both the configured name and the caller's).
	if err := store.Set("gitscm", creds.Credential{
		BaseURL:          srv.URL,
		Auth:             "bearer",
		Token:            "local-token-value-abc",
		StripQueryParams: []string{"X_Custom"},
	}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	h := HTTPHandler(reg, store, "")

	code, _, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "gitscm", Method: http.MethodGet,
		Path: "/api?x_custom=c&keep=ok",
	})
	if err != nil || code != http.StatusOK {
		t.Fatalf("request: code=%d err=%v", code, err)
	}
	if rawQuery != "keep=ok" {
		t.Fatalf("forwarded query = %q, want %q", rawQuery, "keep=ok")
	}
}
