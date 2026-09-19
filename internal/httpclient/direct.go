// Package httpclient provides HTTP transport configuration for local plugin calls.
package httpclient

import "net/http"

// NewDirectTransport returns a clone of Go's default transport that does not
// consult HTTP_PROXY, HTTPS_PROXY, or NO_PROXY. Local credentials must travel
// directly to the configured plugin endpoint.
func NewDirectTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}
