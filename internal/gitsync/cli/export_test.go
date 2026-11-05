package cli

import (
	"context"
	"net/http"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

// SetNowForTest replaces the clock relative timestamps are measured against and returns a function
// that restores it.
func SetNowForTest(t time.Time) (restore func()) {
	prev := now
	now = func() time.Time { return t }
	return func() { now = prev }
}

// GatewayTransportForTest returns the transport the preflight gateway check builds, so a test can
// adjust its trust store or proxy (Go caches both from the environment once per process).
func GatewayTransportForTest() *http.Transport { return newGatewayTransport() }

// SetPreflightEnvForTest replaces the origin prober, the gateway transport and the name lookup the
// preflight command uses; a nil argument keeps the production one. It returns a function that
// restores them.
func SetPreflightEnvForTest(prober gitsync.OriginProber, transport func() *http.Transport, lookup func(ctx context.Context, host string) ([]string, error)) (restore func()) {
	prev := newPreflightEnv
	newPreflightEnv = func() preflightEnv {
		env := defaultPreflightEnv()
		if prober != nil {
			env.prober = prober
		}
		if transport != nil {
			env.transport = transport
		}
		if lookup != nil {
			env.lookupHost = lookup
		}
		return env
	}
	return func() { newPreflightEnv = prev }
}
