package gitsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
)

// This file is the one place the provider config file is parsed and validated. New (the daemon and
// the operator CLI's read paths) and CheckConfig (`connector config check`) both call loadConfig, so
// what `config check` reports is by construction what the daemon would do at startup: they cannot
// disagree about what is valid. The difference is only in how many problems are reported: New stops
// at the first, CheckConfig reports all of them.
//
// No issue, and no error built here, includes a value from the file: not a credential, not a URL,
// and not a key the file defined (a key, such as an unknown field or an invalid provider name, is
// operator-typed text and may be a mis-pasted secret). A problem is located by provider name (only
// once the name has passed the name rule) and by field name (only one of the known ones), or, for a
// problem that cannot be placed that way, by byte offset.

// IssueClass says what kind of problem a ConfigIssue is, so an operator (or a script reading the
// output) can tell a damaged file from a bad setting.
type IssueClass string

const (
	// IssueFile: the file could not be opened or read, is not a regular file, is too large, or has
	// permissions the connector refuses.
	IssueFile IssueClass = "file"
	// IssueSyntax: the content is not one JSON value (empty, truncated, malformed, or followed by
	// extra data).
	IssueSyntax IssueClass = "syntax"
	// IssueSchema: the JSON is well-formed but is not a provider map: an unknown field, or a value of
	// the wrong type.
	IssueSchema IssueClass = "schema"
	// IssueProviderName: a provider name breaks the name rule.
	IssueProviderName IssueClass = "provider_name"
	// IssueBaseURL: a provider's base_url is missing or unusable.
	IssueBaseURL IssueClass = "base_url"
	// IssueCredentials: a provider's authentication mode, username or token is inconsistent or
	// unusable.
	IssueCredentials IssueClass = "credentials"
	// IssueDuplicateHost: two providers resolve to the same scheme, host and port.
	IssueDuplicateHost IssueClass = "duplicate_host"
)

// ConfigIssue is one problem found in the provider config.
type ConfigIssue struct {
	Class IssueClass
	// Provider names the provider the issue is about; empty when it concerns the file as a whole or a
	// provider name that is itself invalid (which is never echoed).
	Provider string
	// Field names the setting at fault (base_url, auth, username, token), or is empty.
	Field string
	// Message says what is wrong, without any value from the file.
	Message string

	// err is exactly the error New returns for this problem.
	err error
}

// Error returns the message New reports for this problem.
func (i ConfigIssue) Error() string { return i.err.Error() }

// Unwrap exposes the underlying error, so os.IsNotExist-style checks keep working through it.
func (i ConfigIssue) Unwrap() error { return i.err }

// fieldError ties a validation failure to the configuration field it is about.
type fieldError struct {
	field string
	msg   string
}

func (e *fieldError) Error() string { return e.msg }

func fieldErr(field, msg string) error { return &fieldError{field: field, msg: msg} }

// loadedConfig is the outcome of reading the provider config file.
type loadedConfig struct {
	entries map[string]LocalEntry
	// hostKeys maps each valid provider to its canonical scheme://host:port.
	hostKeys map[string]string
	// missing says the file does not exist, which is not an error: the connector then has no providers.
	missing bool
	issues  []ConfigIssue
}

// knownConfigFields are the settings a provider may carry, the only field names an issue may repeat.
var knownConfigFields = []string{"base_url", "auth", "username", "token"}

// loadConfig reads and validates the provider config at path, collecting every problem it can find.
// A problem with the file itself (unreadable, not JSON, wrong shape) ends the load, since nothing
// after it can be interpreted; problems with individual providers are all collected, in provider-name
// order so the first is the same on every run.
func loadConfig(path string) loadedConfig {
	res := loadedConfig{entries: map[string]LocalEntry{}, hostKeys: map[string]string{}}
	fail := func(class IssueClass, msg string, err error) loadedConfig {
		res.issues = append(res.issues, ConfigIssue{Class: class, Message: msg, err: err})
		return res
	}

	data, err := readSecureFile(path, "git-sync config", maxConfigFileSize)
	if err != nil {
		if os.IsNotExist(err) {
			res.missing = true
			return res
		}
		return fail(IssueFile, err.Error(), err)
	}

	dc, issue := decodeConfig(path, data)
	if issue != nil {
		res.issues = append(res.issues, *issue)
		return res
	}

	owners := make(map[string]string, len(dc))
	for _, name := range slices.Sorted(maps.Keys(dc)) {
		if err := validateProviderName(name); err != nil {
			res.issues = append(res.issues, ConfigIssue{
				Class: IssueProviderName, Message: describeBadProviderName(name),
				err: fmt.Errorf("git-sync config %s: %w", path, err),
			})
			continue
		}
		de := dc[name]
		e := LocalEntry{BaseURL: de.BaseURL, Auth: de.Auth, Username: de.Username, Token: Secret(de.Token)}
		if err := e.Validate(); err != nil {
			class, field := IssueCredentials, ""
			var fe *fieldError
			if errors.As(err, &fe) {
				field = fe.field
				if field == "base_url" {
					class = IssueBaseURL
				}
			}
			res.issues = append(res.issues, ConfigIssue{
				Class: class, Provider: name, Field: field, Message: err.Error(),
				err: fmt.Errorf("provider %q is invalid: %w", name, err),
			})
			continue
		}
		hk, err := canonicalHostKey(e.BaseURL)
		if err != nil {
			res.issues = append(res.issues, ConfigIssue{
				Class: IssueBaseURL, Provider: name, Field: "base_url", Message: "base_url " + err.Error(),
				err: fmt.Errorf("provider %q is invalid: base_url %w", name, err),
			})
			continue
		}
		if other, ok := owners[hk]; ok {
			res.issues = append(res.issues, ConfigIssue{
				Class: IssueDuplicateHost, Provider: name, Field: "base_url",
				Message: fmt.Sprintf("uses the same host as provider %q; each host may be configured once", other),
				err:     fmt.Errorf("git-sync config %s: providers %q and %q use the same host; each host may be configured once", path, other, name),
			})
			continue
		}
		owners[hk] = name
		res.hostKeys[name] = hk
		res.entries[name] = e
	}
	return res
}

// decodeConfig parses the file content strictly: unknown fields are rejected and nothing may follow
// the configuration object. It reports a problem without quoting any of the content.
func decodeConfig(path string, data []byte) (map[string]diskEntry, *ConfigIssue) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var dc map[string]diskEntry
	if err := dec.Decode(&dc); err != nil {
		return nil, decodeIssue(path, err, dec.InputOffset())
	}
	// Reject anything after the first JSON value (a second value or trailing garbage), so a
	// malformed file cannot be silently half-accepted. Trailing whitespace yields io.EOF.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &ConfigIssue{
			Class: IssueSyntax, Message: "has unexpected trailing data after the configuration object",
			err: fmt.Errorf("git-sync config %s has unexpected trailing data after the configuration object", path),
		}
	}
	return dc, nil
}

// decodeIssue turns a JSON decoding failure into an issue. The decoder's own text is never used: for
// an unknown field or a mistyped value it quotes the offending key or value.
func decodeIssue(path string, err error, offset int64) *ConfigIssue {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntaxErr):
		return &ConfigIssue{
			Class: IssueSyntax, Message: fmt.Sprintf("is not valid JSON (at byte offset %d)", syntaxErr.Offset),
			err: fmt.Errorf("git-sync config %s is not valid JSON (at byte offset %d)", path, syntaxErr.Offset),
		}
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return &ConfigIssue{
			Class: IssueSyntax, Message: "is empty or ends before the configuration object is complete",
			err: fmt.Errorf("git-sync config %s is empty or ends before the configuration object is complete", path),
		}
	case errors.As(err, &typeErr):
		// Only a mistyped setting of a provider names a field; the path before it holds the (not yet
		// validated) provider name and must not be repeated.
		field := ""
		if typeErr.Type != nil && typeErr.Type.Kind() == reflect.String {
			last := typeErr.Field
			if i := strings.LastIndexByte(last, '.'); i >= 0 {
				last = last[i+1:]
			}
			if slices.Contains(knownConfigFields, last) {
				field = last
			}
		}
		msg := fmt.Sprintf("has a value of the wrong type (at byte offset %d); each provider must be an object of string settings", typeErr.Offset)
		if field != "" {
			msg = fmt.Sprintf("has a %s setting that is not a string (at byte offset %d)", field, typeErr.Offset)
		}
		return &ConfigIssue{Class: IssueSchema, Field: field, Message: msg, err: fmt.Errorf("git-sync config %s %s", path, msg)}
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		msg := fmt.Sprintf("has an unknown field near byte offset %d (a provider may set only %s)", offset, strings.Join(knownConfigFields, ", "))
		return &ConfigIssue{Class: IssueSchema, Message: msg, err: fmt.Errorf("git-sync config %s %s", path, msg)}
	}
	return &ConfigIssue{
		Class: IssueSchema, Message: "is not a valid provider configuration",
		err: fmt.Errorf("git-sync config %s is not a valid provider configuration", path),
	}
}

// describeBadProviderName says why a provider name breaks the name rule without repeating it.
func describeBadProviderName(name string) string {
	if name == "" {
		return "a provider name is empty"
	}
	if len(name) > 64 {
		return fmt.Sprintf("a provider name is %d bytes long; the limit is 64", len(name))
	}
	for i, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return fmt.Sprintf("a provider name has a character at byte %d outside letters, digits, '.', '_' and '-'", i)
		}
	}
	return "a provider name is invalid"
}

// ProviderReport describes one valid provider without its credential.
type ProviderReport struct {
	Name string
	// HostKey is the canonical scheme://host:port a repository's origin_url must resolve to for this
	// provider to be used.
	HostKey string
	// Auth is "", "bearer" or "basic".
	Auth string
	// HasToken says a credential is configured inline in the file; its value is not carried.
	HasToken bool
}

// ConfigReport is what loading the provider config found.
type ConfigReport struct {
	Path string
	// Missing says the file does not exist; the connector then starts with no providers.
	Missing bool
	// Providers are the valid providers, by name.
	Providers []ProviderReport
	// Issues are all the problems found, in the order New would report the first.
	Issues []ConfigIssue
	// Warnings are conditions that do not stop the connector but that an operator should know about.
	Warnings []string
}

// CheckConfig loads the provider config at path exactly as New does and reports everything it
// found instead of stopping at the first problem. It makes no network call and its report holds no
// credential value. Like New it tightens the file to 0600 where it can; Warnings records the mode the
// file had before.
func CheckConfig(path string) ConfigReport {
	rep := ConfigReport{Path: path}
	// Group read alone is tolerated (a mounted Kubernetes Secret has it); any other access beyond the
	// owner is what readSecureFile either tightens or refuses.
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o027 != 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"the file's mode is %04o, wider than the owner-only access the connector expects (group read alone is tolerated, for mounted Secrets). The connector tightens it to 0600 where it is allowed to (loading it just did), and otherwise refuses a file that lets others access it.",
			info.Mode().Perm()))
	}
	res := loadConfig(path)
	rep.Missing = res.missing
	rep.Issues = res.issues
	for _, name := range slices.Sorted(maps.Keys(res.entries)) {
		e := res.entries[name]
		rep.Providers = append(rep.Providers, ProviderReport{Name: name, HostKey: res.hostKeys[name], Auth: e.Auth, HasToken: e.Token.Reveal() != ""})
	}
	return rep
}
