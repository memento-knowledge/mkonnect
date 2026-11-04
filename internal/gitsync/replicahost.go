package gitsync

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
)

// The replica host pin.
//
// The platform supplies each repository's replica_push_url, and the push token travels to it. Left
// unchecked, a compromised or misconfigured configuration channel could steer a customer's whole
// repository, and that token, to any https host. The operator therefore pins the one replica host
// the connector may ever push to, at install time, through GIT_SYNC_REPLICA_HOST. The pin is
// authoritative and read-only for the life of the process: it is not part of the platform
// configuration and has no writable storage.
//
// Enforcement lives in applyConfig (a configuration naming any other host is rejected as a whole)
// and, as a second layer, in resolveTask (a sync is never started against a host other than the
// pin). With no valid pin the connector fails closed: it rejects every configuration and never
// syncs, while still connecting, reporting status and heartbeating.

// ReplicaHostEnv names the environment variable that pins the replica host. Its value is `host` or
// `host:port` (https is implied; an IPv6 literal needs brackets). Anything else - a scheme, path,
// userinfo, query, whitespace, a non-ASCII or ambiguous host - makes the pin invalid, which is
// treated exactly like an unset pin.
const ReplicaHostEnv = "GIT_SYNC_REPLICA_HOST"

// ErrReplicaHostNotSet reports that no replica host is pinned (the variable is unset or empty).
var ErrReplicaHostNotSet = errors.New(ReplicaHostEnv + " is not set")

// maxReplicaHostLength bounds the pin: a DNS name is at most 253 bytes, plus a port and brackets.
const maxReplicaHostLength = 262

// ParseReplicaHost validates a pinned replica host value and returns its canonical "host:port" form:
// ASCII-lowercased, the trailing dot stripped, the default port 443 made explicit, and an IPv6
// literal bracketed and normalised. It uses the same host rules as every other URL the connector
// dials (see canonicalASCIIHost), so a Unicode, fullwidth, percent-encoded or inet_aton-style host
// is rejected rather than folded onto an ASCII one.
//
// An empty value is ErrReplicaHostNotSet. Every other failure is a fixed message: the value is never
// echoed, so it cannot inject into a log.
func ParseReplicaHost(raw string) (string, error) {
	if raw == "" {
		return "", ErrReplicaHostNotSet
	}
	if len(raw) > maxReplicaHostLength {
		return "", errors.New(ReplicaHostEnv + " is too long")
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c <= ' ' || c >= 0x7f {
			return "", errors.New(ReplicaHostEnv + " must be ASCII without whitespace or control characters (check for a trailing newline)")
		}
	}
	if strings.Contains(raw, "://") {
		return "", errors.New(ReplicaHostEnv + " must be host or host:port with no scheme (https is implied)")
	}
	if strings.ContainsAny(raw, "/?#@\\%") || strings.HasSuffix(raw, ":") {
		return "", errors.New(ReplicaHostEnv + " must be host or host:port only: no path, userinfo, query, percent-encoding or empty port")
	}
	u, err := url.Parse("https://" + raw)
	if err != nil || u.User != nil || u.Path != "" || u.Hostname() == "" {
		return "", errors.New(ReplicaHostEnv + " is not a valid host or host:port (an IPv6 literal needs brackets)")
	}
	host, err := canonicalASCIIHost(strings.TrimSuffix(u.Hostname(), "."))
	if err != nil {
		return "", errors.New(ReplicaHostEnv + " " + err.Error())
	}
	port, err := canonicalPort(u)
	if err != nil {
		return "", errors.New(ReplicaHostEnv + " " + err.Error())
	}
	return net.JoinHostPort(host, port), nil
}

// ReplicaHostFromEnv reads and canonicalises the pin from ReplicaHostEnv. The daemon calls it once at
// startup; the operator CLI calls it to show the effective value.
func ReplicaHostFromEnv() (string, error) {
	return ParseReplicaHost(os.Getenv(ReplicaHostEnv))
}

// replicaVerdict classifies a platform-supplied replica_push_url against the pin.
type replicaVerdict int

const (
	// replicaOK: the URL is a clean https URL whose canonical host:port is exactly the pin.
	replicaOK replicaVerdict = iota
	// replicaInvalid: the URL is missing, names no host, or names the pinned host but is unusable
	// (userinfo, query, fragment). It fails that one repository, as before the pin existed.
	replicaInvalid
	// replicaMismatch: the URL names a host and port other than the pin, or one that cannot be proven
	// to be the pin. It rejects the whole configuration.
	replicaMismatch
)

// checkReplicaURL classifies raw against pin (the canonical "host:port" from ParseReplicaHost, or ""
// when unpinned, in which case nothing is ever replicaOK). For replicaOK it also returns the
// re-serialised URL that is dialled; for any other verdict the returned URL is empty, so a caller
// cannot dial something it did not accept.
//
// The match is exact: scheme, host and port must all equal the pin's. There are no wildcards,
// subdomains or suffix matches. The accepting comparison is made on the output of the strict
// canonicalRemoteURL, the same value that is later dialled, so the host that was checked is the host
// that is contacted. A lenient parse is used only to decide how a URL that failed the strict check
// is classified: one that parses to the pinned host is merely unusable (replicaInvalid), anything
// else - including a URL that does not parse at all, or whose host is non-ASCII, percent-encoded or
// otherwise ambiguous - is treated as naming another host, so a hostile value is surfaced loudly
// instead of being quietly skipped.
func checkReplicaURL(raw, pin string) (string, replicaVerdict) {
	if raw == "" {
		return "", replicaInvalid
	}
	pinKey := "https://" + pin
	if key, full, err := canonicalRemoteURL(raw); err == nil {
		if pin != "" && key == pinKey {
			return full, replicaOK
		}
		return "", replicaMismatch
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", replicaMismatch
	}
	if u.Hostname() == "" {
		return "", replicaInvalid
	}
	host, err := canonicalASCIIHost(strings.TrimSuffix(u.Hostname(), "."))
	if err != nil {
		return "", replicaMismatch
	}
	port, err := canonicalPort(u)
	if err != nil {
		return "", replicaMismatch
	}
	if pin != "" && strings.ToLower(u.Scheme)+"://"+net.JoinHostPort(host, port) == pinKey {
		return "", replicaInvalid
	}
	return "", replicaMismatch
}
