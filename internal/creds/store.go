// internal/creds/store.go
package creds

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MinTokenLength prevents ambiguous, short strings from being used as local
// credentials or mistaken for ordinary response content.
const MinTokenLength = 16

// MinUpstreamTimeout and MaxUpstreamTimeout bound a per-plugin upstream timeout
// (Credential.UpstreamTimeout). The feature exists to RAISE the timeout for
// legitimately slow upstreams (large commits, a slow self-managed instance), so a
// sub-second value is treated as a misconfiguration. The upper bound is a guardrail,
// not a tuning knob: it caps the blast radius of a typo (e.g. "10h"), keeps a single
// request from occupying one of the connector's few worker slots for an unreasonable
// time, and stays within what the calling platform could plausibly wait for. It is
// deliberately not configurable — a configurable cap is no cap.
const (
	MinUpstreamTimeout = 1 * time.Second
	MaxUpstreamTimeout = 10 * time.Minute
)

// Credential is the per-plugin configuration entry: a plugin's base URL, its optional
// local HTTP authentication, and optional per-plugin policy. It is the home for
// per-plugin settings generally — future per-plugin options (e.g. a custom CA bundle or
// an upstream timeout) extend this struct.
type Credential struct {
	BaseURL  string `json:"base_url"`
	Auth     string `json:"auth,omitempty"` // "bearer", "basic", or ""
	Username string `json:"username,omitempty"`
	Token    string `json:"token,omitempty"`
	// StripHeaders lists extra inbound request headers to drop before forwarding, in
	// addition to the credential-header defaults the plugin handler always enforces when a
	// local credential is applied. Names are matched case-insensitively. Entries are
	// honoured whenever this entry exists, so a no-auth plugin can opt into stripping too.
	StripHeaders []string `json:"strip_headers,omitempty"`
	// StripQueryParams lists extra inbound URL query-parameter names to drop before
	// forwarding, in addition to the credential-parameter defaults the plugin handler
	// enforces when a local credential is applied. A configured name is normalised the same
	// way as an inbound key before matching — case-folded, and with '.'/space folded to '_'
	// (PHP mangles those) — so whatever spelling the operator writes (private.token,
	// private_token, Private_Token) matches whatever spelling the caller sends. The matching
	// is deliberately broad because query-parameter names are case-sensitive per the URL spec
	// yet some upstreams (e.g. ASP.NET) read them case-insensitively, so the denylist must be
	// at least as broad as the sloppiest upstream — the only cost is that a non-credential
	// parameter whose normalised name equals a stripped name is also dropped when a local
	// credential is applied. Entries are honoured whenever this entry exists, so a no-auth
	// plugin can opt into stripping too.
	StripQueryParams []string `json:"strip_query_params,omitempty"`
	// CABundle is the absolute path to a PEM file of additional root CA(s) to trust when
	// verifying this plugin's upstream TLS certificate — e.g. an internal CA for a
	// self-managed service. Its certificates are ADDED to the system trust store, never a
	// replacement for it, and there is deliberately no option to disable verification.
	// Empty means the system trust store only (optionally a global CA_BUNDLE default). This
	// value is operator configuration and is never taken from an inbound gateway message,
	// so the path is not attacker-chosen; the file is read lazily by the plugin handler,
	// which fails closed if it cannot be loaded.
	CABundle string `json:"ca_bundle,omitempty"`
	// UpstreamTimeout overrides, for this plugin only, the connector's default per-request
	// upstream timeout. It is a Go duration string with a unit (e.g. "30s", "2m"); the
	// operator's literal is stored verbatim. Empty means the built-in default applies. A set
	// value must parse and fall in [MinUpstreamTimeout, MaxUpstreamTimeout] (see Validate).
	// It bounds the whole forwarded request (connect, send, and reading the response); the
	// dialer's connect timeout and the TLS-handshake timeout keep their own fixed ceilings,
	// so a value above those does not extend them. It is operator configuration, never taken
	// from an inbound message, and applies only to forwarded http_request calls.
	UpstreamTimeout string `json:"upstream_timeout,omitempty"`
}

// Validate checks the local authentication fields without including credential
// values in any returned error.
func (c Credential) Validate() error {
	switch c.Auth {
	case "":
		if c.Username != "" || c.Token != "" {
			return errors.New("credentials require an authentication mode")
		}
	case "bearer":
		if c.Username != "" {
			return errors.New("bearer authentication must not include a username")
		}
		if utf8.RuneCountInString(c.Token) < MinTokenLength {
			return fmt.Errorf("token must be at least %d characters", MinTokenLength)
		}
	case "basic":
		if c.Username == "" {
			return errors.New("basic authentication requires a username")
		}
		if strings.ContainsRune(c.Username, ':') {
			return errors.New("basic authentication username must not contain ':'")
		}
		if utf8.RuneCountInString(c.Token) < MinTokenLength {
			return fmt.Errorf("token must be at least %d characters", MinTokenLength)
		}
	default:
		return errors.New("unsupported authentication mode")
	}
	// Validate (but do not mutate — value receiver) any extra headers to strip. The
	// plugin handler canonicalises these at lookup time, so only reject names that are
	// not usable header names at all. HasAuthorization depends on Validate, but Set and
	// load both validate before persisting, so the store never holds an invalid entry.
	for _, h := range c.StripHeaders {
		if strings.TrimSpace(h) == "" {
			return errors.New("strip_headers entries must not be empty")
		}
		if strings.ContainsAny(h, " \t\r\n:") {
			return errors.New("strip_headers entries must be valid header names")
		}
	}
	// Validate any extra query-parameter names to strip. They are matched against the
	// percent-DECODED, bracket-trimmed, case-folded parameter name at request time, so an
	// entry is required to be a plain parameter name: reject separators, percent-escapes, and
	// brackets. A bracket entry truly could never match (names are cut at a bracket); the
	// others could match a percent-encoded caller key, but rejecting them keeps the config
	// unambiguous and turns a likely typo into a clear error rather than a silent surprise in
	// a security control.
	for _, q := range c.StripQueryParams {
		if strings.TrimSpace(q) == "" {
			return errors.New("strip_query_params entries must not be empty")
		}
		if strings.ContainsAny(q, " \t\r\n&=?#%+;[]") {
			return errors.New("strip_query_params entries must be plain query-parameter names (no separators, percent-escapes, or brackets)")
		}
	}
	// A CA bundle path, if given, must be absolute: the connector daemon's working
	// directory differs from the CLI's, so a relative path would resolve unpredictably.
	// The file itself is read (and failures surfaced) lazily at request time, not here,
	// so Validate stays filesystem-free and a not-yet-mounted bundle does not block load.
	if c.CABundle != "" {
		if strings.TrimSpace(c.CABundle) == "" || strings.ContainsRune(c.CABundle, '\x00') {
			return errors.New("ca_bundle must be a non-empty path without NUL bytes")
		}
		if !filepath.IsAbs(c.CABundle) {
			return errors.New("ca_bundle must be an absolute path")
		}
	}
	// A per-plugin upstream timeout, if given, must be a duration with a unit and within the
	// supported range. The error is the operator's only hint, so it names the accepted form
	// and the bounds explicitly (as literals, so the hint stays human-readable rather than
	// printing MaxUpstreamTimeout as "10m0s"). ParseDuration rejects a unitless value ("30"),
	// which is the most likely typo and must not be silently reinterpreted.
	if c.UpstreamTimeout != "" {
		d, err := time.ParseDuration(c.UpstreamTimeout)
		if err != nil || d < MinUpstreamTimeout || d > MaxUpstreamTimeout {
			return errors.New(`upstream_timeout must be a duration with a unit (e.g. "30s", "2m"), at least 1s and at most 10m`)
		}
	}
	return nil
}

// UpstreamTimeoutDuration returns the configured per-plugin upstream timeout and whether
// one is set. An absent value returns (0, false) and the caller applies its own default.
// Set, New (load), and Reload all Validate every entry, so the store never holds an
// unparseable or out-of-range value and this normally cannot fail; the defensive (0, false)
// on a parse error is kept only so a future caller that bypasses the store cannot make
// request handling panic.
func (c Credential) UpstreamTimeoutDuration() (time.Duration, bool) {
	if c.UpstreamTimeout == "" {
		return 0, false
	}
	d, err := time.ParseDuration(c.UpstreamTimeout)
	if err != nil {
		return 0, false
	}
	return d, true
}

// HasAuthorization reports whether this credential has the local fields needed
// to construct an Authorization header.
func (c Credential) HasAuthorization() bool {
	return c.Auth != "" && c.Validate() == nil
}

// ApplyAuthorization adds this credential's Authorization header to req when its
// configured authentication mode has all required local credential fields.
func (c Credential) ApplyAuthorization(req *http.Request) {
	if !c.HasAuthorization() {
		return
	}
	switch c.Auth {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+c.Token)
	case "basic":
		if c.Username != "" {
			req.SetBasicAuth(c.Username, c.Token)
		}
	}
}

// Store is a thread-safe, file-backed map of plugin name → Credential.
type Store struct {
	mu   sync.RWMutex
	path string
	data map[string]Credential
	// generation increments on every change to data (load, Set, Remove). Consumers
	// that cache state derived from a credential — such as the plugin handler's
	// per-plugin TLS transports — read it to detect when a SIGHUP reload or a CLI
	// change means their cache must be rebuilt.
	generation uint64
}

// New loads or creates a credential store at path.
// If the file does not exist, an empty store is returned (not an error).
// If the file exists, its permissions are corrected to 0600.
func New(path string) (*Store, error) {
	s := &Store{path: path, data: make(map[string]Credential)}
	if err := s.load(); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		return s, nil
	}
	// On a writable store, tighten the file to 0600. If chmod fails — most often
	// because the file is on a read-only mount, e.g. a Kubernetes Secret — fall
	// back to verifying the existing mode is safe (see readOnlyModeAcceptable).
	if err := chmod(path, 0600); err != nil {
		info, statErr := os.Stat(path)
		if statErr != nil || !readOnlyModeAcceptable(info.Mode()) {
			return nil, fmt.Errorf("credentials file %s has unsafe permissions and chmod failed; restrict it to the owner, allowing at most group read (e.g. 0600 or 0640): %w", path, err)
		}
	}
	return s, nil
}

// chmod is os.Chmod, overridable in tests to exercise the read-only-mount branch
// of New (as the file's owner, chmod cannot be made to fail portably).
var chmod = os.Chmod

// readOnlyModeAcceptable reports whether a credentials file whose mode could not
// be corrected to 0600 (typically a read-only mount) is still safe to use.
//
// It forbids any access by "other" (world) and any write by "group", while
// allowing group *read*. Group read is required because Kubernetes mounts a
// Secret volume owned by root with group set to the pod's fsGroup, so a non-root
// connector can only read its own credential file through the group bit; a
// Secret mounted read-only cannot be chmod-ed to 0600 at runtime.
func readOnlyModeAcceptable(mode os.FileMode) bool {
	// 0o027 = group-write (0o020) plus all "other" bits (0o007).
	return mode.Perm()&0o027 == 0
}

// Generation returns a counter that increments whenever the credential set changes
// (via load, Set, or Remove). It never decreases within a process. A consumer that
// caches work derived from credentials can compare it to the value it last built
// under and rebuild when it differs.
func (s *Store) Generation() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generation
}

// Get returns the credential for plugin, or false if not found.
func (s *Store) Get(plugin string) (Credential, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.data[plugin]
	return c, ok
}

// List returns a snapshot of all credentials (safe to range over).
func (s *Store) List() map[string]Credential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Credential, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

// Set writes or overwrites the credential for plugin and persists to disk.
func (s *Store) Set(plugin string, cred Credential) error {
	if err := cred.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[plugin] = cred
	s.generation++
	return s.save()
}

// Remove deletes the credential for plugin and persists to disk.
func (s *Store) Remove(plugin string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, plugin)
	s.generation++
	return s.save()
}

// Reload re-reads credentials.json from disk, replacing the in-memory map. A missing
// file is a valid state (e.g. every plugin configured via PLUGIN_* env with only a
// global CA_BUNDLE): it resets the store to empty and still bumps the generation, so a
// SIGHUP picks up a rotated CA bundle even when no credentials file exists.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		if os.IsNotExist(err) {
			s.data = make(map[string]Credential)
			s.generation++
			return nil
		}
		return err
	}
	return nil
}

// load reads and parses the JSON file. Caller must hold s.mu.
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var m map[string]Credential
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	if m == nil {
		m = make(map[string]Credential)
	}
	for plugin, cred := range m {
		if err := cred.Validate(); err != nil {
			return fmt.Errorf("credential %q is invalid: %w", plugin, err)
		}
	}
	s.data = m
	s.generation++
	return nil
}

// save writes the in-memory map to disk atomically via a temp-file rename.
// Uses os.CreateTemp (O_EXCL, unique name) to prevent symlink attacks, explicitly
// chmods to 0600 regardless of umask or stale temp-file permissions, and syncs
// before rename so a crash does not leave a truncated credentials file.
// Caller must hold s.mu.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".credentials-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp) //nolint:errcheck
		}
	}()
	if err := os.Chmod(tmp, 0600); err != nil {
		f.Close() //nolint:errcheck
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close() //nolint:errcheck
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close() //nolint:errcheck
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ok = true
	return os.Rename(tmp, s.path)
}
