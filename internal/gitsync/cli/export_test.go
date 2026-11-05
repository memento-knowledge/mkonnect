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

// The replacements a test has installed; a nil one keeps the production value.
var (
	testProber        gitsync.OriginProber
	testReplicaProber gitsync.OriginProber
	testTransport     func() *http.Transport
	testLookup        func(ctx context.Context, host string) ([]string, error)
)

func installPreflightEnv() {
	newPreflightEnv = func() preflightEnv {
		env := defaultPreflightEnv()
		if testProber != nil {
			env.prober = testProber
		}
		if testReplicaProber != nil {
			env.replicaProber = testReplicaProber
		}
		if testTransport != nil {
			env.transport = testTransport
		}
		if testLookup != nil {
			env.lookupHost = testLookup
		}
		return env
	}
}

// SetPreflightEnvForTest replaces the origin prober, the gateway transport and the name lookup the
// preflight command uses; a nil argument keeps the one already in place (the production one, unless
// an earlier call replaced it). It returns a function that restores them.
func SetPreflightEnvForTest(prober gitsync.OriginProber, transport func() *http.Transport, lookup func(ctx context.Context, host string) ([]string, error)) (restore func()) {
	prevProber, prevTransport, prevLookup := testProber, testTransport, testLookup
	if prober != nil {
		testProber = prober
	}
	if transport != nil {
		testTransport = transport
	}
	if lookup != nil {
		testLookup = lookup
	}
	installPreflightEnv()
	return func() { testProber, testTransport, testLookup = prevProber, prevTransport, prevLookup }
}

// SetReplicaProberForTest replaces the prober that makes the request to the replica host, and
// returns a function that restores the previous one.
func SetReplicaProberForTest(p gitsync.OriginProber) (restore func()) {
	prev := testReplicaProber
	testReplicaProber = p
	installPreflightEnv()
	return func() { testReplicaProber = prev }
}

// KeyProbeVerdictForTest exposes how the outcome of the key directory probe is reported.
func KeyProbeVerdictForTest(err error, dir string) (status, detail, remedy string) {
	st, d, r := keyProbeVerdict(err, dir)
	return string(st), d, r
}
