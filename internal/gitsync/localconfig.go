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
	"log/slog"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// minTokenLength mirrors creds.MinTokenLength: a short string is too ambiguous to be a usable
// token. It is duplicated rather than imported because git-sync's local config is deliberately
// independent of the API connector's credential store (internal/creds); the two surfaces may
// diverge as the git-sync credential model is hardened in Phase 4.
const minTokenLength = 16

// maxRepoKeyLength bounds a repository key so a malformed or hostile config cannot define an
// absurdly long identifier that would then appear in status and logs.
const maxRepoKeyLength = 256

// secretRedaction is what a Secret renders as everywhere except an explicit Reveal.
const secretRedaction = "<redacted>"

// Secret is a string whose value never appears in logs, formatted output, or marshalled JSON:
// every rendering path redacts it, and only Reveal returns the underlying value. Keeping the
// protection on the type — rather than on each struct that happens to hold one — means it
// survives new fields and struct copies. The value is still read normally from config through
// UnmarshalText.
type Secret string

// Format handles every fmt verb (%v, %s, %q, %#v, %d, ...), so no verb can print the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, secretRedaction) }

// LogValue redacts the value for slog. The signature MUST be LogValue() slog.Value to satisfy
// slog.LogValuer; a LogValue() any is silently ignored and the value would leak.
func (Secret) LogValue() slog.Value { return slog.StringValue(secretRedaction) }

// MarshalText redacts the value for encoding/json and any text encoder. Phase 2 has no write
// path, so redacting on marshal cannot lose a real value; a Phase 4 persistence path will use
// Reveal explicitly.
func (Secret) MarshalText() ([]byte, error) { return []byte(secretRedaction), nil }

// UnmarshalText reads the raw value from config.
func (s *Secret) UnmarshalText(b []byte) error { *s = Secret(b); return nil }

// Reveal returns the underlying secret value. It is the only path that does.
func (s Secret) Reveal() string { return string(s) }

var (
	_ slog.LogValuer = Secret("")
	_ fmt.Formatter  = Secret("")
)

// RepoCredential is the local, operator-supplied configuration for one repository: where its
// origin lives and, optionally, how to authenticate to it. It is keyed in the config file by
// the same repo_key the platform uses in git_sync_config, so the platform never needs to hold
// a customer's internal repository URL — the correlation happens locally.
//
// This is the Phase 2 surface only. Phase 4 owns the hardened credential model (for example at
// rest, rotation, and a write path with the same guarantees as the CLI's creds flow).
type RepoCredential struct {
	// OriginURL is the repository's origin. Phase 2 accepts only absolute http/https URLs;
	// SSH remotes are deferred to Phase 4 (scp-style "git@host:path" does not parse as a URL,
	// and an ssh:// remote needs key material this skeleton does not model).
	OriginURL string `json:"origin_url"`
	// Auth selects the local authentication mode: "" (none), "basic", or "bearer". It mirrors
	// the API connector's modes so operators meet one vocabulary.
	Auth string `json:"auth,omitempty"`
	// Username is required for and only valid with basic auth.
	Username string `json:"username,omitempty"`
	// Token is the bearer token or basic-auth password. Its Secret type keeps it out of logs,
	// formatted output, and marshalled JSON by construction.
	Token Secret `json:"token,omitempty"`
}

// Validate checks a single entry without including any credential value in a returned error.
// The rules parallel the API connector's credential validation but stay independent of it.
func (c RepoCredential) Validate() error {
	if strings.TrimSpace(c.OriginURL) == "" {
		return errors.New("origin_url is required")
	}
	u, err := url.Parse(c.OriginURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("origin_url must be an absolute http or https URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		// Userinfo in a remote URL leaks into `git remote -v` and process listings; query and
		// fragment are meaningless for a git origin and usually signal a malformed value.
		return errors.New("origin_url must not include userinfo, query parameters, or a fragment")
	}

	token := c.Token.Reveal()
	switch c.Auth {
	case "":
		if c.Username != "" || token != "" {
			return errors.New("credentials require an authentication mode")
		}
		return nil
	case "bearer":
		if c.Username != "" {
			return errors.New("bearer authentication must not include a username")
		}
	case "basic":
		if c.Username == "" {
			return errors.New("basic authentication requires a username")
		}
		if strings.ContainsRune(c.Username, ':') {
			return errors.New("basic authentication username must not contain ':'")
		}
	default:
		return errors.New("unsupported authentication mode")
	}

	// bearer/basic both need a usable token, and the origin must be https so it is never sent in
	// cleartext. Control characters are rejected now because they become a header-injection
	// vector once Phase 3 builds an Authorization header from the token.
	if utf8.RuneCountInString(token) < minTokenLength {
		return fmt.Errorf("token must be at least %d characters", minTokenLength)
	}
	if strings.ContainsFunc(token, func(r rune) bool { return unicode.IsControl(r) }) {
		return errors.New("token must not contain control characters")
	}
	if u.Scheme != "https" {
		return errors.New("origin_url must use https when an authentication mode is set")
	}
	return nil
}

// LocalConfig is the read-only, file-backed set of per-repository local configuration. Phase 2
// loads it once at startup; there is no live reload or write path yet (Phase 4 owns those), so
// the map is not mutated after construction and needs no lock.
type LocalConfig struct {
	repos map[string]RepoCredential
}

// chmod is os.Chmod, overridable in tests to exercise the read-only-mount branch of
// LoadLocalConfig (as the file's owner, chmod cannot be made to fail portably). Mirrors the
// seam in internal/creds.
var chmod = os.Chmod

// LoadLocalConfig reads the git-sync local config from path. A missing file yields an empty
// config and no error, mirroring the API connector's credential store: an operator may run the
// connector before any repository is configured. A present-but-invalid file is an error whose
// message never includes a credential value or raw file content.
func LoadLocalConfig(path string) (*LocalConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &LocalConfig{repos: map[string]RepoCredential{}}, nil
		}
		return nil, err
	}

	if err := ensureOwnerOnly(path); err != nil {
		return nil, err
	}

	// DisallowUnknownFields turns a mistyped key (e.g. "orign_url") into an error rather than a
	// silently dropped setting — important when a dropped field could be a security-relevant one.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m map[string]RepoCredential
	if err := dec.Decode(&m); err != nil {
		// A SyntaxError can quote the offending bytes, which may be a credential value, so report
		// only its position. The other decoder errors (type mismatch, unknown field) name a type
		// or an operator-chosen field, never a value, so they are safe to surface.
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return nil, fmt.Errorf("git-sync config %s is not valid JSON (at byte offset %d)", path, syntaxErr.Offset)
		}
		return nil, fmt.Errorf("git-sync config %s is not valid: %w", path, err)
	}
	if m == nil {
		m = map[string]RepoCredential{}
	}
	for repoKey, cred := range m {
		switch {
		case repoKey == "":
			return nil, fmt.Errorf("git-sync config %s has an empty repository key", path)
		case len(repoKey) > maxRepoKeyLength:
			return nil, fmt.Errorf("git-sync config %s has a repository key longer than %d characters", path, maxRepoKeyLength)
		}
		if err := cred.Validate(); err != nil {
			// cred.Validate never includes a credential value; repoKey is an operator-chosen
			// identifier, not a secret.
			return nil, fmt.Errorf("repository %q is invalid: %w", repoKey, err)
		}
	}
	return &LocalConfig{repos: m}, nil
}

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

// Get returns the local configuration for repoKey, or false if none is configured.
func (c *LocalConfig) Get(repoKey string) (RepoCredential, bool) {
	cred, ok := c.repos[repoKey]
	return cred, ok
}

// Repos returns the configured repo keys in sorted order.
func (c *LocalConfig) Repos() []string {
	return slices.Sorted(maps.Keys(c.repos))
}
