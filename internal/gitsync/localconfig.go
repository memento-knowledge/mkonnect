// Package gitsync implements the git-sync connector kind: the gateway.Handler that will mirror
// customer-internal git repositories over the Bridge Gateway, plus its own local configuration.
// The connection lifecycle (dial, enrollment, handshake, heartbeat, reconnect) lives in
// internal/gateway, which this package drives — exactly as the API connector in internal/ws does.
//
// This is a Phase 2 skeleton: the sync algorithm itself is not implemented (see the
// TODO(phase-3) markers), and the local credential surface here is intentionally minimal —
// full credential hardening is Phase 4.
package gitsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// minTokenLength mirrors creds.MinTokenLength: a short string is too ambiguous to be a usable
// token. It is duplicated rather than imported because git-sync's local config is deliberately
// independent of the API connector's credential store (internal/creds); the two surfaces may
// diverge as the git-sync credential model is hardened in Phase 4.
const minTokenLength = 16

// DefaultConfigFile is where the provider config is read from when GIT_SYNC_CONFIG_FILE is unset.
// Nothing mounts this path yet; packaging is tracked separately.
const DefaultConfigFile = "/data/git-sync.json"

// DefaultConfigPath returns the provider-config path from GIT_SYNC_CONFIG_FILE, or the default. It
// is shared by the daemon and the operator CLI so the two never read or write different files.
func DefaultConfigPath() string {
	if p := os.Getenv("GIT_SYNC_CONFIG_FILE"); p != "" {
		return p
	}
	return DefaultConfigFile
}

// Secret aliases proto.Secret so the local token uses the same self-redacting type as the inbound
// push token — one implementation, no drift. Its value is redacted on every output path (fmt, slog,
// JSON) and revealed only via Reveal; the Store never marshals it directly, persisting through an
// explicit plain-string on-disk struct instead (see save).
type Secret = proto.Secret

// chmod is os.Chmod, overridable in tests to exercise the read-only-mount branch of New (as the
// file's owner, chmod cannot be made to fail portably). Mirrors the seam in internal/creds.
var chmod = os.Chmod

// ensureOwnerOnly tightens the config file to owner-only before its contents are trusted. If
// chmod fails — most often a read-only mount such as a Kubernetes Secret — the existing mode is
// accepted only when it denies world access and group write (group read is allowed for fsGroup
// Secret mounts). This mirrors the API connector's credential store so the two behave alike.
func ensureOwnerOnly(path string) error {
	if err := chmod(path, 0o600); err != nil {
		info, statErr := os.Stat(path)
		if statErr != nil || !readOnlyModeAcceptable(info.Mode()) {
			return fmt.Errorf("git-sync config %s has unsafe permissions and chmod failed; restrict it to the owner, allowing at most group read (e.g. 0600 or 0640)", path)
		}
	}
	return nil
}

// readOnlyModeAcceptable reports whether a file whose mode could not be corrected to 0600 is
// still safe: no group-write (0o020) and no "other" bits (0o007).
func readOnlyModeAcceptable(mode os.FileMode) bool {
	return mode.Perm()&0o027 == 0
}

// --- Provider-keyed local config (the confirmed Phase-2 model) ---
//
// The confirmed platform contract keys the local configuration by an operator-chosen provider
// (remote) name carrying an origin BASE URL + credential, mirroring the API connector's
// `config set <name> --base-url ... --token-stdin`. A repository that the platform sends in
// git_sync_config carries its own full origin_url; the connector binds it to a local provider by
// canonical scheme+host+port (see MatchProvider) before it would ever use the credential.

// maxURLLength bounds a configured or platform-supplied URL so a hostile or malformed value cannot
// define an absurdly long string that then appears in status, logs, or a subprocess environment.
const maxURLLength = 2048

// providerNameRE constrains an operator-chosen provider name to a safe, log-injection-free set.
var providerNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func validateProviderName(name string) error {
	if !providerNameRE.MatchString(name) {
		return errors.New("provider name must be 1-64 characters of letters, digits, '.', '_', or '-'")
	}
	return nil
}

// validateRemoteURL parses and defensively validates an http/https remote URL. It rejects the
// shapes that differ between Go's parser and git/curl (userinfo, query, fragment, backslashes,
// whitespace, control characters), so a platform-supplied origin_url cannot smuggle a different
// host past the match guard. It never includes the raw value in an error.
func validateRemoteURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("is required")
	}
	if len(raw) > maxURLLength {
		return nil, fmt.Errorf("must be at most %d characters", maxURLLength)
	}
	if strings.ContainsFunc(raw, func(r rune) bool {
		return r == '\\' || unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return nil, errors.New("must not contain whitespace, control characters, or backslashes")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, errors.New("must be an absolute http or https URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must not include userinfo, query parameters, or a fragment")
	}
	return u, nil
}

// canonicalHostKey reduces a URL to a comparable "scheme://host:port" identity: lowercased host,
// trailing FQDN dot stripped, IP literals canonicalized, and the port normalized to an explicit
// integer (default ports made explicit) so that https://h, https://h:443 and https://h.:0443 all
// compare equal while http:// never matches https://. It is the basis for both the host-match
// guard and duplicate-host detection.
func canonicalHostKey(raw string) (string, error) {
	u, err := validateRemoteURL(raw)
	if err != nil {
		return "", err
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", errors.New("must include a host")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	} else {
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || n > 65535 {
			return "", errors.New("has an invalid port")
		}
		port = strconv.Itoa(n)
	}
	return u.Scheme + "://" + net.JoinHostPort(host, port), nil
}

// LocalEntry is the operator-supplied local configuration for one provider: an origin base URL
// and, optionally, how to authenticate to it. The Token keeps the self-redacting Secret type in
// memory and is persisted through an explicit on-disk struct (see save/New) rather than being
// marshalled directly — marshalling a Secret would write "<redacted>" and destroy the token.
type LocalEntry struct {
	BaseURL  string
	Auth     string // "" (none) | "bearer" | "basic"
	Username string // required for and only valid with basic auth
	Token    Secret
}

// Validate checks one entry without including any credential value in a returned error.
func (e LocalEntry) Validate() error {
	u, err := validateRemoteURL(e.BaseURL)
	if err != nil {
		return fmt.Errorf("base_url %w", err)
	}
	token := e.Token.Reveal()
	switch e.Auth {
	case "":
		if e.Username != "" || token != "" {
			return errors.New("credentials require an authentication mode")
		}
		return nil
	case "bearer":
		if e.Username != "" {
			return errors.New("bearer authentication must not include a username")
		}
	case "basic":
		if e.Username == "" {
			return errors.New("basic authentication requires a username")
		}
		if strings.ContainsRune(e.Username, ':') {
			return errors.New("basic authentication username must not contain ':'")
		}
	default:
		return errors.New("unsupported authentication mode")
	}
	if utf8.RuneCountInString(token) < minTokenLength {
		return fmt.Errorf("token must be at least %d characters", minTokenLength)
	}
	// Control characters are rejected because the token becomes an Authorization header value in a
	// subprocess environment (Phase 2 probe, Phase 3 fetch); CR/LF/NUL there is an injection vector.
	if strings.ContainsFunc(token, func(r rune) bool { return unicode.IsControl(r) }) {
		return errors.New("token must not contain control characters")
	}
	if u.Scheme != "https" {
		return errors.New("base_url must use https when an authentication mode is set")
	}
	return nil
}

// diskEntry is the on-disk JSON shape. Token is a plain string here (written via Secret.Reveal),
// so the file holds the real value; the in-memory LocalEntry keeps the self-redacting Secret.
type diskEntry struct {
	BaseURL  string `json:"base_url"`
	Auth     string `json:"auth,omitempty"`
	Username string `json:"username,omitempty"`
	Token    string `json:"token,omitempty"`
}

// Store is the provider-keyed, file-backed local config with a write path (used by the operator
// CLI) and read accessors (used by the connector). It mirrors internal/creds/store.go, with the
// redaction-safe persistence above.
type Store struct {
	mu   sync.RWMutex
	path string
	data map[string]LocalEntry
}

// New loads the provider config from path. A missing file yields an empty store and no error. A
// present file is tightened to owner-only, decoded strictly (unknown fields rejected), and every
// entry validated; a duplicate host (two providers that resolve to the same scheme+host+port) is
// rejected so credential selection can never be ambiguous. No error includes a credential value.
func New(path string) (*Store, error) {
	s := &Store{path: path, data: map[string]LocalEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := ensureOwnerOnly(path); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var dc map[string]diskEntry
	if err := dec.Decode(&dc); err != nil {
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return nil, fmt.Errorf("git-sync config %s is not valid JSON (at byte offset %d)", path, syntaxErr.Offset)
		}
		return nil, fmt.Errorf("git-sync config %s is not valid: %w", path, err)
	}
	// Reject anything after the first JSON value (a second value or trailing garbage), so a
	// malformed file cannot be silently half-accepted. Trailing whitespace yields io.EOF.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("git-sync config %s has unexpected trailing data after the configuration object", path)
	}
	hostKeys := make(map[string]string, len(dc))
	for name, de := range dc {
		if err := validateProviderName(name); err != nil {
			return nil, fmt.Errorf("git-sync config %s: %w", path, err)
		}
		e := LocalEntry{BaseURL: de.BaseURL, Auth: de.Auth, Username: de.Username, Token: Secret(de.Token)}
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("provider %q is invalid: %w", name, err)
		}
		hk, err := canonicalHostKey(e.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("provider %q is invalid: base_url %w", name, err)
		}
		if other, ok := hostKeys[hk]; ok {
			return nil, fmt.Errorf("git-sync config %s: providers %q and %q use the same host; each host may be configured once", path, other, name)
		}
		hostKeys[hk] = name
		s.data[name] = e
	}
	return s, nil
}

// Get returns the entry for name, or false if none.
func (s *Store) Get(name string) (LocalEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data[name]
	return e, ok
}

// List returns a defensive copy of all entries.
func (s *Store) List() map[string]LocalEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]LocalEntry, len(s.data))
	maps.Copy(out, s.data)
	return out
}

// Names returns the configured provider names in sorted order.
func (s *Store) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Sorted(maps.Keys(s.data))
}

// Set validates and stores an entry under name, rejecting a host already used by a different
// provider, then persists atomically. Replacing the same name's own entry is allowed.
func (s *Store) Set(name string, e LocalEntry) error {
	if err := validateProviderName(name); err != nil {
		return err
	}
	if err := e.Validate(); err != nil {
		return err
	}
	hk, err := canonicalHostKey(e.BaseURL)
	if err != nil {
		return fmt.Errorf("base_url %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for other, oe := range s.data {
		if other == name {
			continue
		}
		if ohk, err := canonicalHostKey(oe.BaseURL); err == nil && ohk == hk {
			return fmt.Errorf("provider %q already uses the same host; each host may be configured once", other)
		}
	}
	next := make(map[string]LocalEntry, len(s.data)+1)
	maps.Copy(next, s.data)
	next[name] = e
	if err := s.save(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

// Remove deletes the named provider, erroring if it does not exist, then persists atomically.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[name]; !ok {
		return fmt.Errorf("no provider named %q is configured", name)
	}
	next := make(map[string]LocalEntry, len(s.data))
	maps.Copy(next, s.data)
	delete(next, name)
	if err := s.save(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

// MatchProvider returns the provider whose base URL matches originURL by canonical
// scheme+host+port. A malformed or non-matching origin_url returns ok=false, so the caller rejects
// the repository (origin_access_denied) rather than ever using a credential against an unexpected
// host. Iteration is sorted for deterministic selection.
func (s *Store) MatchProvider(originURL string) (name string, e LocalEntry, ok bool) {
	hk, err := canonicalHostKey(originURL)
	if err != nil {
		return "", LocalEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, n := range slices.Sorted(maps.Keys(s.data)) {
		if ehk, err := canonicalHostKey(s.data[n].BaseURL); err == nil && ehk == hk {
			return n, s.data[n], true
		}
	}
	return "", LocalEntry{}, false
}

// save writes data atomically: a temp file in the same directory, tightened to 0600, synced,
// then renamed over the target, with the directory synced so the rename is durable. The on-disk
// form reveals the token (the file must hold the real value), never the Secret's redaction.
func (s *Store) save(data map[string]LocalEntry) error {
	dc := make(map[string]diskEntry, len(data))
	for name, e := range data {
		dc[name] = diskEntry{BaseURL: e.BaseURL, Auth: e.Auth, Username: e.Username, Token: e.Token.Reveal()}
	}
	b, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".git-sync-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // harmless no-op once the rename has moved it
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
