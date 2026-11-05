package gitsync

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// minTokenLength mirrors creds.MinTokenLength: a short string is too ambiguous to be a usable
// token. It is duplicated rather than imported because git-sync's local config is deliberately
// independent of the API connector's credential store (internal/creds); the two surfaces may
// diverge as the git-sync credential model is hardened in Phase 4.
const minTokenLength = 16

// Secret aliases proto.Secret so the local token uses the same self-redacting type as the inbound
// push token — one implementation, no drift. Its value is redacted on every output path (fmt, slog,
// JSON) and revealed only via Reveal; the Store never marshals it directly, persisting through an
// explicit plain-string on-disk struct instead (see save).
type Secret = proto.Secret

// LocalEntry is the operator-supplied local configuration for one provider: an origin base URL
// and, optionally, how to authenticate to it. The Token keeps the self-redacting Secret type in
// memory and is persisted through an explicit on-disk struct (see save/New) rather than being
// marshalled directly — marshalling a Secret would write "<redacted>" and destroy the token.
//
// TokenFile, when set, is the absolute path the credential was read from at load time; Token then
// holds the value read from it. A provider has either an inline token or a token file, never both
// (New enforces this on the file as written), and save persists only the path for a token-file
// provider so rewriting the config never copies the secret into it.
type LocalEntry struct {
	BaseURL   string
	Auth      string // "" (none) | "bearer" | "basic"
	Username  string // required for and only valid with basic auth
	Token     Secret
	TokenFile string
}

// Validate checks one entry without including any credential value in a returned error. For a
// token-file entry it checks the resolved Token, so it is the final check after the file was read.
// Each failure carries the field it is about (see fieldErr).
func (e LocalEntry) Validate() error {
	if err := e.validateSettings(); err != nil {
		return err
	}
	return e.validateToken()
}

// validateSettings checks everything about an entry except the token's value: the URL, the
// authentication mode and its username rule, and the https requirement. It never needs the
// credential, so the loader runs it on every provider before opening any token file.
func (e LocalEntry) validateSettings() error {
	u, err := validateRemoteURL(e.BaseURL)
	if err != nil {
		return fieldErr("base_url", "base_url "+err.Error())
	}
	switch e.Auth {
	case "":
		if e.Username != "" || e.Token.Reveal() != "" || e.TokenFile != "" {
			return fieldErr("auth", "credentials require an authentication mode")
		}
		return nil
	case "bearer":
		if e.Username != "" {
			return fieldErr("username", "bearer authentication must not include a username")
		}
	case "basic":
		if e.Username == "" {
			return fieldErr("username", "basic authentication requires a username")
		}
		if strings.ContainsRune(e.Username, ':') {
			return fieldErr("username", "basic authentication username must not contain ':'")
		}
	default:
		return fieldErr("auth", "unsupported authentication mode")
	}
	if u.Scheme != "https" {
		return fieldErr("base_url", "base_url must use https when an authentication mode is set")
	}
	return nil
}

// validateToken checks the credential value when an authentication mode is set.
func (e LocalEntry) validateToken() error {
	if e.Auth == "" {
		return nil
	}
	token := e.Token.Reveal()
	if utf8.RuneCountInString(token) < minTokenLength {
		return fieldErr("token", fmt.Sprintf("token must be at least %d characters", minTokenLength))
	}
	// Control characters are rejected because the token becomes an Authorization header value in a
	// subprocess environment (the reachability probe and the mirror fetch); CR/LF/NUL there is an
	// injection vector.
	if strings.ContainsFunc(token, func(r rune) bool { return unicode.IsControl(r) }) {
		return fieldErr("token", "token must not contain control characters")
	}
	return nil
}

// diskEntry is the on-disk JSON shape. Token is a plain string here (written via Secret.Reveal),
// so the file holds the real value; the in-memory LocalEntry keeps the self-redacting Secret. Never
// format a diskEntry: it holds the plain token.
//
// Token and TokenFile are pointers so that a key written with an empty value ("token_file": "") is
// distinguishable from an absent key: an explicit empty value must be rejected, not read as "no
// credential". A JSON null reads as absent. When writing, a field is set only if it has a value.
type diskEntry struct {
	BaseURL   string  `json:"base_url"`
	Auth      string  `json:"auth,omitempty"`
	Username  string  `json:"username,omitempty"`
	Token     *string `json:"token,omitempty"`
	TokenFile *string `json:"token_file,omitempty"`
}

// strVal returns the string a presence-tracking field holds, or "" when the key was absent.
func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// strPtr returns a pointer to s, or nil for "" so that an empty field is omitted when written.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
