// Package gitsync implements the git-sync connector kind: the gateway.Handler that will mirror
// customer-internal git repositories over the Bridge Gateway, plus its own local configuration.
// The connection lifecycle (dial, enrollment, handshake, heartbeat, reconnect) lives in
// internal/gateway, which this package drives — exactly as the API connector in internal/ws does.
//
// The local credential surface here is intentionally minimal: a credential is an inline token or
// the path of a file holding it, vetted as described on readProviderConfig and readTokenFile.
// At-rest encryption is future work. The provider configuration, token files included, is reloaded
// while the daemon runs; see reload.go.
package gitsync

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// DefaultConfigFile is where the provider config is read from when GIT_SYNC_CONFIG_FILE is unset.
// The container image creates /data for the non-root runtime user; mount a volume there.
const DefaultConfigFile = "/data/git-sync.json"

// DefaultConfigPath returns the provider-config path from GIT_SYNC_CONFIG_FILE, or the default. It
// is shared by the daemon and the operator CLI so the two never read or write different files.
func DefaultConfigPath() string {
	if p := os.Getenv("GIT_SYNC_CONFIG_FILE"); p != "" {
		return p
	}
	return DefaultConfigFile
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
