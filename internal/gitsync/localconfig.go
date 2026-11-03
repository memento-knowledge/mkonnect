// Package gitsync implements the git-sync connector kind: the gateway.Handler that will mirror
// customer-internal git repositories over the Bridge Gateway, plus its own local configuration.
// The connection lifecycle (dial, enrollment, handshake, heartbeat, reconnect) lives in
// internal/gateway, which this package drives — exactly as the API connector in internal/ws does.
//
// The local credential surface here is intentionally minimal — full credential hardening (at-rest
// protection, live reload) is future work.
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

// --- Provider-keyed local config ---
//
// The platform contract keys the local configuration by an operator-chosen provider (remote) name
// carrying an origin BASE URL + credential, mirroring the API connector's
// `config set <name> --base-url ... --token-stdin`. A repository that the platform sends in
// git_sync_config carries its own full origin_url; the connector binds it to a local provider by
// canonical scheme+host+port (see canonicalRemoteURL and matchByHostKey) before it would ever use
// the credential, and the sync dials the re-serialized canonical URL, never the raw origin string.

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

// numericHostLabelRE matches the inet_aton numeric forms a host's final label must not take: a
// decimal or leading-zero-octal digit run, or a "0x"/"0X" hex run. It is applied only to the
// rightmost label (see canonicalASCIIHost), which is where inet_aton decides whether to read the
// whole host as an IPv4 address.
var numericHostLabelRE = regexp.MustCompile(`^(?:[0-9]+|0[xX][0-9a-fA-F]*)$`)

// asciiToLower lowercases ASCII letters only, leaving every other byte untouched. It is the
// defense-in-depth half of the host guard: the PRIMARY guard is canonicalASCIIHost's ASCII
// validation, which rejects every non-ASCII host before any folding happens. ASCII-only folding
// ensures that even if that validation were later loosened, host comparison could never Unicode
// case-fold — strings.ToLower maps characters such as U+0130 ('İ') and U+212A ('K', Kelvin sign)
// onto ASCII letters, which would let a non-ASCII origin fold onto a configured ASCII host while
// git and curl resolve it to a different, attacker-registrable host.
func asciiToLower(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// canonicalASCIIHost validates a URL host and returns its canonical comparison form. An IP literal
// (IPv4 or IPv6, as netip accepts it, but never with a zone) is canonicalized via netip. Otherwise
// the host must be an ASCII letter-digit-hyphen-underscore name, which is ASCII-lowercased.
// Everything else is rejected, so the host that is match-checked is exactly the host git will dial.
// Rejected forms include: non-ASCII and internationalized hosts (e.g. U+0130, U+212A, fullwidth
// letters/digits), Unicode "dot" lookalikes such as the ideographic full stop U+3002, zero-width
// characters (U+200B), any percent-encoding, an IPv6 zone (e.g. "fe80::1%eth0"), and the ambiguous
// numeric forms that netip rejects but curl/inet_aton dial as an IPv4 address when the host's final
// label is numeric — a bare integer like "2130706433", a leading-zero octal octet like "010.0.0.1",
// or a hex run like "0x7f000001" or "127.0.0.0x1". If internationalized domains are ever required,
// normalize BOTH sides via golang.org/x/net/idna (idna.Lookup.ToASCII), never with strings.ToLower.
func canonicalASCIIHost(host string) (string, error) {
	if host == "" {
		return "", errors.New("must include a host")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		// An IPv6 zone is attacker-chosen text that is neither charset-checked nor ASCII-lowercased and
		// can address a link-local interface, so reject it rather than carry it into the dialed URL.
		if ip.Zone() != "" {
			return "", errors.New("must not include an IPv6 zone")
		}
		return ip.String(), nil
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return "", errors.New("has an invalid host")
		}
		for j := 0; j < len(label); j++ {
			switch c := label[j]; {
			case c >= '0' && c <= '9':
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			case c == '-', c == '_':
			default:
				return "", errors.New("host must be an ASCII name (letters, digits, hyphen, underscore) or an IP literal; use the punycode (xn--) form for internationalized domains")
			}
		}
	}
	// A real DNS top-level label is never purely numeric or a "0x" hex run; those are the inet_aton
	// forms that netip rejected above but curl/git still dial as an address. inet_aton reinterprets a
	// host as an address only when its FINAL label is numeric, so rejecting that label is sufficient.
	if numericHostLabelRE.MatchString(labels[len(labels)-1]) {
		return "", errors.New("has an ambiguous numeric host")
	}
	return asciiToLower(host), nil
}

// canonicalPort normalizes u's port to an explicit 1-65535 integer string, making the scheme's
// default explicit when none is present, so https://h, https://h:443 and https://h:0443 all compare
// equal while http:// never matches https://.
func canonicalPort(u *url.URL) (string, error) {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			return "443", nil
		}
		return "80", nil
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return "", errors.New("has an invalid port")
	}
	return strconv.Itoa(n), nil
}

// canonicalRemoteURL validates raw and returns both its canonical host key (scheme://host:port, used
// for provider matching and duplicate-host detection) and a re-serialized absolute URL whose
// authority is exactly that canonical host:port, with the trailing FQDN dot stripped and the path
// preserved. The sync dials fullURL rather than re-parsing the raw origin string: git's URL parser
// and ours can diverge, so re-serializing from the validated host guarantees the host that was
// match-checked is byte-for-byte the host that gets contacted.
//
// Two normalizations are deliberate and safe here: the trailing FQDN dot is stripped and the
// scheme's default port is made explicit. Both hostKey and fullURL are built from the SAME
// canonicalized authority, so the match key and the dialed URL can never diverge from each other.
// (Credentials are therefore passed to git explicitly, scoped to fullURL, rather than through a git
// credential helper keying on the un-normalized "scheme://host" form.)
func canonicalRemoteURL(raw string) (hostKey, fullURL string, err error) {
	u, err := validateRemoteURL(raw)
	if err != nil {
		return "", "", err
	}
	host, err := canonicalASCIIHost(strings.TrimSuffix(u.Hostname(), "."))
	if err != nil {
		return "", "", err
	}
	port, err := canonicalPort(u)
	if err != nil {
		return "", "", err
	}
	authority := net.JoinHostPort(host, port)
	cu := &url.URL{Scheme: u.Scheme, Host: authority, Path: u.Path, RawPath: u.RawPath}
	return u.Scheme + "://" + authority, cu.String(), nil
}

// canonicalHostKey reduces a URL to its comparable scheme://host:port identity (see
// canonicalRemoteURL). It is the basis for both the host-match guard and duplicate-host detection.
func canonicalHostKey(raw string) (string, error) {
	hostKey, _, err := canonicalRemoteURL(raw)
	return hostKey, err
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
	// subprocess environment (the reachability probe and the mirror fetch); CR/LF/NUL there is an
	// injection vector.
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
//
// Do NOT use this to obtain a URL to dial: it discards the re-serialized canonical URL and keeps
// only the raw origin. Resolve the match via canonicalRemoteURL (keeping its fullURL) and
// matchByHostKey instead, so the host that was match-checked is exactly the host that is contacted.
// This entry point remains for a caller that only needs to know whether a provider exists.
func (s *Store) MatchProvider(originURL string) (name string, e LocalEntry, ok bool) {
	hk, err := canonicalHostKey(originURL)
	if err != nil {
		return "", LocalEntry{}, false
	}
	return s.matchByHostKey(hk)
}

// matchByHostKey returns the provider whose base URL canonicalizes to hostKey. The caller passes a
// host key already derived by canonicalRemoteURL/canonicalHostKey, so the origin is parsed once.
// Iteration is sorted for deterministic selection.
func (s *Store) matchByHostKey(hostKey string) (name string, e LocalEntry, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, n := range slices.Sorted(maps.Keys(s.data)) {
		if ehk, err := canonicalHostKey(s.data[n].BaseURL); err == nil && ehk == hostKey {
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
