package plugin

// Write-forwarding contract suite (T1–T7).
//
// This file consolidates the end-to-end guarantees of the request-forwarding handler so the
// contract cannot silently regress. Most items were already pinned by the behaviour PRs that
// introduced them; this file adds the two that were not exercised anywhere — a POST request
// body forwarded byte-for-byte (T1) and the no-retry guarantee (T7) — and serves as the index
// that maps every contract item to the test(s) that lock it in:
//
//   T1  POST body + Content-Type forwarded byte-for-byte, no re-encoding or re-framing:
//         TestContractPOSTForwardsBodyContentTypeAndCredential (this file),
//         TestContractEmptyWriteBodyForwarded (this file).
//   T2  Escaped path preserved while traversal / authority / userinfo is rejected:
//         TestHTTPHandlerPreservesEscapedPath, TestHTTPHandlerPathTraversalBlocked,
//         TestHTTPHandlerRejectsAuthoritySmuggling, TestHTTPHandlerRejectsCallerPathUserinfo
//         (handler_test.go); TestPathBlockedCarriesMarker (handler_marker_test.go).
//   T3  Local credential replaces any caller-supplied credential header:
//         TestHTTPHandlerStripsCallerCredentialHeadersWhenLocalCredentialApplied and the
//         other TestHTTPHandler*Credential* tests (handler_test.go); also asserted on a POST
//         carrying a body by TestContractPOSTForwardsBodyContentTypeAndCredential (this file).
//   T4  Request / response size limits and the per-plugin upstream timeout:
//         TestRequestBodyTooLargeCarriesMarker, TestResponseTooLargeCarriesMarker
//         (handler_marker_test.go); the handler_timeout_test.go suite. The accepted edges are
//         pinned here: a request body of exactly the maximum is forwarded (the at-max rows of
//         TestContractPOSTForwardsBodyContentTypeAndCredential) and a response of exactly the
//         maximum is relayed (TestContractMaxResponseBodyRelayed).
//   T5  Request expiry (expires_at): the handler_expiry_test.go suite.
//   T6  X-Mkonnect-Error markers on synthesised errors vs no marker on relayed responses:
//         the handler_marker_test.go suite; the no-retry POST failure here additionally pins
//         the upstream_error marker on a reused-connection failure.
//   T7  A write is not retried once request bytes have been written:
//         TestContractWriteRequestNotRetried (this file).
//
// T7 is stated precisely on purpose, scoped to HTTP/1.1 — which this harness exercises over a
// plaintext httptest upstream. Over HTTP/1.1 Go's transport never retries a request on a *fresh*
// connection; the only retry path is a *reused* idle connection that the server closed. On that
// path a GET (or any method the transport deems replayable) is retried, but a plain POST is not
// — so a POST write is delivered to the upstream at most once even when its connection fails
// mid-flight. TestContractWriteRequestNotRetried exercises exactly this path.
//
// Two transport behaviours are documented exceptions to "a POST is not retried":
//   - A request that failed before a single byte was written (the server saw nothing) is
//     replayed safely. This is documented here but NOT exercised — staging the nothing-written
//     window deterministically is impractical.
//   - A POST the caller explicitly marked idempotent (Idempotency-Key / X-Idempotency-Key) is
//     replayed. This IS pinned, by a subtest, so the exception stays intentional.
//
// Over HTTP/2 the rule differs: the transport may replay any method, but only when the server
// has signalled the stream was not processed (GOAWAY, REFUSED_STREAM, or an unusable
// connection) — i.e. still only when no write can have taken effect. That path is not pinned
// here, since the plaintext httptest harness never negotiates h2.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// contractBody builds a deterministic request body of exactly size bytes whose leading bytes
// are chosen to catch any re-encoding regression: reserved and percent-encoded characters that
// url.Values/ParseForm would rewrite ('+', '%2F', '&', '='), multi-byte UTF-8 runes, and a
// CR/LF and NUL that a line- or C-string-oriented transform would mangle. The value stays
// valid UTF-8 deliberately: the wire contract (proto.HTTPRequestMsg.Body) is plain UTF-8 text,
// so invalid UTF-8 is out of contract and is not exercised here.
func contractBody(size int) string {
	const prefix = "k=a+b%2F%26%3D&x=€✓\r\n\x00tail="
	if size <= len(prefix) {
		return prefix[:size]
	}
	var b strings.Builder
	b.Grow(size)
	b.WriteString(prefix)
	const pad = "0123456789abcdef"
	for b.Len() < size {
		if n := size - b.Len(); n < len(pad) {
			b.WriteString(pad[:n])
		} else {
			b.WriteString(pad)
		}
	}
	return b.String()
}

// firstDiff returns the first byte offset at which got and want differ, or -1 when they are
// identical. It compares []byte against string without allocating a copy, so it stays cheap on
// the multi-megabyte bodies, and it never logs the bodies themselves (an offset, not a dump).
func firstDiff(got []byte, want string) int {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			return i
		}
	}
	if len(got) != len(want) {
		return n
	}
	return -1
}

// TestContractPOSTForwardsBodyContentTypeAndCredential pins T1 (and, on a real write request,
// T3 and the T4 accept-boundary): a POST/PUT body is forwarded to the upstream byte-for-byte
// with its Content-Type intact and correct framing, while the local credential replaces any
// caller-supplied credential header. It covers ~100 KB and a body of exactly the maximum
// accepted size, for both application/json and application/x-www-form-urlencoded.
func TestContractPOSTForwardsBodyContentTypeAndCredential(t *testing.T) {
	// Mirrors the unexported maxBodyBytes in handler.go: a body of exactly this length is
	// accepted and forwarded, and maxBodyBytes+1 is rejected (TestRequestBodyTooLargeCarriesMarker).
	const maxAcceptedBody = 32 << 20
	const localToken = "local-bearer-contract-token"

	cases := []struct {
		name        string
		method      string
		contentType string
		size        int
	}{
		{"json 100KB", http.MethodPost, "application/json; charset=utf-8", 100 * 1024},
		{"form 100KB", http.MethodPost, "application/x-www-form-urlencoded", 100 * 1024},
		{"json at max accepted", http.MethodPost, "application/json; charset=utf-8", maxAcceptedBody},
		{"form at max accepted", http.MethodPost, "application/x-www-form-urlencoded", maxAcceptedBody},
		{"put forwards body too", http.MethodPut, "application/json", 100 * 1024},
		{"patch forwards body too", http.MethodPatch, "application/json", 100 * 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := contractBody(tc.size)

			var (
				mu  sync.Mutex
				rec struct {
					hits                int
					method              string
					contentType         string
					gotBody             []byte
					contentLength       int64
					transferEncoding    []string
					authorization       []string
					privateTokenPresent bool
				}
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				rec.hits++
				rec.method = r.Method
				rec.contentType = r.Header.Get("Content-Type")
				rec.gotBody = b
				rec.contentLength = r.ContentLength
				rec.transferEncoding = r.TransferEncoding
				rec.authorization = r.Header.Values("Authorization")
				_, rec.privateTokenPresent = r.Header["Private-Token"]
				mu.Unlock() // unlock before responding so the post-h() read happens-after this write
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			store := mustStore(t)
			if err := store.Set("svc", creds.Credential{BaseURL: srv.URL, Auth: "bearer", Token: localToken}); err != nil {
				t.Fatalf("store.Set: %v", err)
			}
			h := HTTPHandler(newRegistry("svc", srv.URL), store, "")

			status, respHeaders, _, err := h(context.Background(), proto.HTTPRequestMsg{
				ProviderKey: "svc", Method: tc.method, Path: "/write",
				Headers: map[string]string{
					"Content-Type":  tc.contentType,
					"Authorization": "Bearer caller-supplied-should-be-replaced",
					"PRIVATE-TOKEN": "caller-private-token",
					// A caller that lies about the length must not be able to desync framing:
					// Go's request writer ignores a Content-Length header and uses the body length.
					"Content-Length": "1",
				},
				Body: &body,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if m := marker(respHeaders); m != "" {
				t.Fatalf("relayed response must not be marked, got %s=%q", ErrorHeader, m)
			}

			mu.Lock()
			defer mu.Unlock()
			if rec.hits != 1 {
				t.Fatalf("upstream received %d requests, want exactly 1", rec.hits)
			}
			if rec.method != tc.method {
				t.Fatalf("upstream method = %q, want %q", rec.method, tc.method)
			}
			if rec.contentType != tc.contentType {
				t.Fatalf("upstream Content-Type = %q, want %q (must be forwarded verbatim)", rec.contentType, tc.contentType)
			}
			if len(rec.gotBody) != len(body) {
				t.Fatalf("upstream body length = %d, want %d — body not forwarded intact", len(rec.gotBody), len(body))
			}
			if off := firstDiff(rec.gotBody, body); off != -1 {
				t.Fatalf("upstream body differs from the sent body at byte offset %d (length %d) — not forwarded byte-for-byte", off, len(body))
			}
			if rec.contentLength != int64(len(body)) {
				t.Fatalf("upstream Content-Length = %d, want %d (a lying caller Content-Length must be ignored and the body must not be chunked)", rec.contentLength, len(body))
			}
			if len(rec.transferEncoding) != 0 {
				t.Fatalf("upstream Transfer-Encoding = %v, want none; a chunked body means req.GetBody was dropped, which disables the safe nothing-written replay and the Idempotency-Key exception", rec.transferEncoding)
			}
			wantAuth := "Bearer " + localToken
			if len(rec.authorization) != 1 || rec.authorization[0] != wantAuth {
				t.Fatalf("upstream Authorization has %d value(s), want exactly 1 equal to the local credential — it must replace the caller's, with no duplicate (value redacted)", len(rec.authorization))
			}
			if rec.privateTokenPresent {
				t.Fatal("caller PRIVATE-TOKEN leaked to the upstream")
			}
		})
	}
}

// TestContractEmptyWriteBodyForwarded pins that an empty write (nil or empty body) is forwarded
// with Content-Length: 0 and no chunked framing, for POST and PUT. Strict upstreams reject a
// bodyless POST that omits Content-Length, so the explicit zero length is part of the contract.
func TestContractEmptyWriteBodyForwarded(t *testing.T) {
	empty := ""
	cases := []struct {
		name   string
		method string
		body   *string
	}{
		{"post nil body", http.MethodPost, nil},
		{"post empty body", http.MethodPost, &empty},
		{"put nil body", http.MethodPut, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu       sync.Mutex
				hits     int
				gotLen   int
				cl       int64
				clHeader []string
				te       []string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				hits++
				gotLen = len(b)
				cl = r.ContentLength
				// The server leaves a received Content-Length in r.Header; r.ContentLength alone
				// cannot distinguish "sent Content-Length: 0" from "sent no length at all" (both
				// parse to 0), so assert the header value explicitly.
				clHeader = r.Header.Values("Content-Length")
				te = r.TransferEncoding
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			h := HTTPHandler(newRegistry("svc", srv.URL), mustStore(t), "")
			status, respHeaders, _, err := h(context.Background(), proto.HTTPRequestMsg{
				ProviderKey: "svc", Method: tc.method, Path: "/write", Body: tc.body,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if m := marker(respHeaders); m != "" {
				t.Fatalf("relayed response must not be marked, got %s=%q", ErrorHeader, m)
			}

			mu.Lock()
			defer mu.Unlock()
			if hits != 1 {
				t.Fatalf("upstream received %d requests, want exactly 1", hits)
			}
			if gotLen != 0 {
				t.Fatalf("upstream body length = %d, want 0", gotLen)
			}
			if len(clHeader) != 1 || clHeader[0] != "0" {
				t.Fatalf("upstream Content-Length header = %v, want exactly [\"0\"] — an empty write must send an explicit Content-Length: 0 that strict upstreams require", clHeader)
			}
			if cl != 0 {
				t.Fatalf("upstream r.ContentLength = %d, want 0", cl)
			}
			if len(te) != 0 {
				t.Fatalf("upstream Transfer-Encoding = %v, want none", te)
			}
		})
	}
}

// TestContractMaxResponseBodyRelayed pins the accepted edge of the response-size limit: an
// upstream response of exactly the maximum size is relayed with status 200 and no marker, not
// rejected. The over-limit side (maxBodyBytes+1 -> 413 response_too_large) is pinned by
// TestResponseTooLargeCarriesMarker.
func TestContractMaxResponseBodyRelayed(t *testing.T) {
	const maxAcceptedBody = 32 << 20 // mirrors the unexported maxBodyBytes in handler.go.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, maxAcceptedBody))
	}))
	defer srv.Close()

	h := HTTPHandler(newRegistry("svc", srv.URL), mustStore(t), "")
	status, respHeaders, body, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a response of exactly the maximum size must be relayed, not rejected", status)
	}
	if m := marker(respHeaders); m != "" {
		t.Fatalf("relayed response must not be marked, got %s=%q", ErrorHeader, m)
	}
	gotLen := -1
	if body != nil {
		gotLen = len(*body)
	}
	if gotLen != maxAcceptedBody {
		t.Fatalf("relayed body length = %d, want %d", gotLen, maxAcceptedBody)
	}
}

// t7Hit records one delivery the upstream saw during the no-retry scenario.
type t7Hit struct {
	path    string
	method  string
	addr    string // RemoteAddr: the client's ephemeral port, i.e. which connection carried it
	bodyLen int    // request body bytes the upstream read for this delivery
}

// runNoRetryScenario drives the exact transport state in which Go retries a replayable request
// but not a plain POST: a reused idle connection that the server closes after the request has
// been written. It primes a live pooled keep-alive connection with a GET to /prime, then issues
// one write to /real whose first delivery the upstream hijacks and closes without responding. It
// returns the handler's result for the /real request and every delivery the upstream saw (across
// both paths and any retry), so callers can assert both the outcome and that the primed
// connection was genuinely reused — without which a "not retried" assertion would pass vacuously.
func runNoRetryScenario(t *testing.T, method string, headers map[string]string, body *string) (status int, mk string, hits []t7Hit, err error) {
	t.Helper()

	var (
		mu           sync.Mutex
		recorded     []t7Hit
		realAttempts atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body first: it puts the whole request on the wire (so the failure below is
		// the "bytes written, server closed a reused conn" case, not the safe nothing-written
		// case) and lets the close be a clean FIN rather than a connection reset. The byte count
		// lets the POST subtests confirm the full body was delivered (and, on a retry, re-sent
		// intact from req.GetBody).
		n, _ := io.Copy(io.Discard, r.Body)
		mu.Lock()
		recorded = append(recorded, t7Hit{path: r.URL.Path, method: r.Method, addr: r.RemoteAddr, bodyLen: int(n)})
		mu.Unlock()

		if r.URL.Path != "/real" {
			// Priming request. The response MUST be bodyless: only then does the transport
			// return the connection to the idle pool before delivering the response, so by the
			// time the priming h() call returns the connection is reliably pooled and alive for
			// /real to reuse. A response with any body would pool the connection asynchronously
			// and race the next request, making reuse — and therefore this whole test — flaky.
			w.WriteHeader(http.StatusOK)
			return
		}
		if realAttempts.Add(1) == 1 {
			// First delivery of /real: hijack and close without responding, so the client's
			// round trip fails after it has written the request on the reused connection. This
			// is the case gated by Request.isReplayable — retried for a GET, not for a plain POST.
			conn, _, hErr := w.(http.Hijacker).Hijack()
			if hErr != nil {
				t.Errorf("hijack: %v", hErr)
				return
			}
			_ = conn.Close()
			return
		}
		// Any later delivery only happens because the transport retried on a fresh connection:
		// acknowledge it so the handler observes a successful round trip.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := mustStore(t)
	if err := store.Set("svc", creds.Credential{BaseURL: srv.URL, Auth: "bearer", Token: "local-contract-token"}); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	// One handler, so both requests share the single connection pool captured in its closure.
	h := HTTPHandler(newRegistry("svc", srv.URL), store, "")

	if code, _, _, perr := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: http.MethodGet, Path: "/prime",
	}); perr != nil || code != http.StatusOK {
		t.Fatalf("prime request: code=%d err=%v", code, perr)
	}

	status, respHeaders, _, err := h(context.Background(), proto.HTTPRequestMsg{
		ProviderKey: "svc", Method: method, Path: "/real", Headers: headers, Body: body,
	})
	mu.Lock()
	hits = append([]t7Hit(nil), recorded...)
	mu.Unlock()
	return status, marker(respHeaders), hits, err
}

// realDeliveries returns only the deliveries to /real, in order.
func realDeliveries(hits []t7Hit) []t7Hit {
	var out []t7Hit
	for _, h := range hits {
		if h.path == "/real" {
			out = append(out, h)
		}
	}
	return out
}

// primeAddr returns the connection (RemoteAddr) the priming request used.
func primeAddr(hits []t7Hit) string {
	for _, h := range hits {
		if h.path == "/prime" {
			return h.addr
		}
	}
	return ""
}

// TestContractWriteRequestNotRetried pins T7. Under one transport state — a reused idle
// connection the server closes after the request is written — a GET is replayed on a fresh
// connection while a plain POST is not, so a POST write reaches the upstream at most once.
// A POST that the caller explicitly marks idempotent (Idempotency-Key) IS replayed; that
// exception is pinned here too so it stays intentional.
func TestContractWriteRequestNotRetried(t *testing.T) {
	// A rewindable body is sent on the write subtests: req.GetBody is then set (the state that
	// could enable a replay), so "not retried once bytes were written" is pinned on a real body,
	// not on headers alone.
	writeBody := contractBody(256)

	t.Run("plain POST is not retried", func(t *testing.T) {
		status, mk, hits, err := runNoRetryScenario(t, http.MethodPost, nil, &writeBody)
		reals := realDeliveries(hits)
		if len(reals) != 1 {
			t.Fatalf("upstream saw %d POST deliveries to /real, want exactly 1 — a POST must not be retried: %+v", len(reals), reals)
		}
		if reals[0].addr != primeAddr(hits) {
			t.Fatalf("test precondition broke: /real was not sent on the primed connection (addr %q vs primed %q), so the reused-connection retry path was never exercised", reals[0].addr, primeAddr(hits))
		}
		if reals[0].bodyLen != len(writeBody) {
			t.Fatalf("upstream read %d body bytes, want %d — the full request body must reach the upstream once", reals[0].bodyLen, len(writeBody))
		}
		if err == nil {
			t.Fatal("expected a transport error on the failed POST")
		}
		if status != http.StatusGatewayTimeout {
			t.Fatalf("status = %d, want 504", status)
		}
		if mk != ErrCodeUpstreamError {
			t.Fatalf("%s = %q, want %q (a server-closed reused connection is a non-dial, non-timeout transport error)", ErrorHeader, mk, ErrCodeUpstreamError)
		}
	})

	t.Run("GET is retried (proves the retry path fires)", func(t *testing.T) {
		status, mk, hits, err := runNoRetryScenario(t, http.MethodGet, nil, nil)
		reals := realDeliveries(hits)
		if len(reals) != 2 {
			t.Fatalf("upstream saw %d GET deliveries to /real, want 2 — a GET is replayable and must be retried (this is what proves the harness exercises the retry path): %+v", len(reals), reals)
		}
		if reals[0].addr != primeAddr(hits) {
			t.Fatalf("test precondition broke: the first /real delivery was not on the primed connection (addr %q vs primed %q)", reals[0].addr, primeAddr(hits))
		}
		if reals[1].addr == reals[0].addr {
			t.Fatalf("the retry was delivered on the same connection %q; a retry must use a fresh connection", reals[1].addr)
		}
		if err != nil {
			t.Fatalf("unexpected error after a successful retry: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 after the retry succeeded", status)
		}
		if mk != "" {
			t.Fatalf("a retried-then-succeeded response must carry no marker, got %s=%q", ErrorHeader, mk)
		}
	})

	t.Run("POST with Idempotency-Key is retried", func(t *testing.T) {
		// The handler forwards a caller's Idempotency-Key (it is neither hop-by-hop nor a
		// credential header), which makes Go's transport treat the POST as replayable. This is
		// the documented exception to T7: the caller has declared the write safe to repeat. Go
		// treats X-Idempotency-Key identically; the unprefixed spelling stands in for both here.
		status, mk, hits, err := runNoRetryScenario(t, http.MethodPost, map[string]string{"Idempotency-Key": "contract-key-1"}, &writeBody)
		reals := realDeliveries(hits)
		if len(reals) != 2 {
			t.Fatalf("upstream saw %d POST deliveries to /real, want 2 — a POST carrying Idempotency-Key is caller-declared idempotent and IS replayed: %+v", len(reals), reals)
		}
		if reals[0].addr != primeAddr(hits) {
			t.Fatalf("test precondition broke: the first /real delivery was not on the primed connection (addr %q vs primed %q)", reals[0].addr, primeAddr(hits))
		}
		// Both deliveries must carry the full body: the retry re-sends it intact from req.GetBody.
		for i, d := range reals {
			if d.bodyLen != len(writeBody) {
				t.Fatalf("/real delivery %d read %d body bytes, want %d — the body must be re-sent intact on replay", i, d.bodyLen, len(writeBody))
			}
		}
		if err != nil {
			t.Fatalf("unexpected error after a successful retry: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 after the retry succeeded", status)
		}
		if mk != "" {
			t.Fatalf("a retried-then-succeeded response must carry no marker, got %s=%q", ErrorHeader, mk)
		}
	})
}
