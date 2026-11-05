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
	// Field names the setting at fault (base_url, auth, username, token, token_file), or is empty.
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
	// sources are the files involved: the config (listed even when missing) and every token file it
	// names, with a fingerprint of the bytes actually read. It is filled in even when the load fails.
	sources sourceSet
}

// knownConfigFields are the settings a provider may carry, the only field names an issue may repeat.
var knownConfigFields = []string{"base_url", "auth", "username", "token", "token_file"}

// loadConfig reads and validates the provider config at path, collecting every problem it can find.
// A problem with the file itself (unreadable, not JSON, wrong shape, owned by another user while
// naming a token file) ends the load, since nothing after it can be trusted or interpreted; problems
// with individual providers are all collected, in provider-name order so the first is the same on
// every run.
//
// The load has two passes over the providers. The first checks everything that does not need a
// credential (name, credential source, settings, inline token, host). The second reads the token
// files, and runs only if the first found nothing wrong, so a configuration that will be refused
// anyway opens no token file at all.
func loadConfig(path string) loadedConfig {
	res := loadedConfig{entries: map[string]LocalEntry{}, hostKeys: map[string]string{}, sources: sourceSet{}}
	fail := func(class IssueClass, msg string, err error) loadedConfig {
		res.issues = append(res.issues, ConfigIssue{Class: class, Message: msg, err: err})
		return res
	}

	cfg, err := readProviderConfig(path, maxConfigFileSize, rejectForeignConfigOwner)
	if err != nil {
		res.sources.add(Source{Path: path})
		if os.IsNotExist(err) {
			res.missing = true
			return res
		}
		return fail(IssueFile, err.Error(), err)
	}
	res.sources.add(newSource(path, cfg.data))

	dc, issue := decodeConfig(path, cfg.data)
	if issue != nil {
		res.issues = append(res.issues, *issue)
		return res
	}
	names := slices.Sorted(maps.Keys(dc))

	// Record every well-formed token file now, so a failed load still reports the files a retry should
	// watch (including one that does not exist yet).
	usesTokenFile := false
	for _, name := range names {
		// Any token_file key counts, even an empty one: that is an error for its provider below, and
		// until then the config is held to the stricter owner rule.
		if tf := dc[name].TokenFile; tf != nil {
			usesTokenFile = true
			if validateTokenFilePath(*tf) == nil {
				res.sources.add(Source{Path: *tf})
			}
		}
	}
	// A config that names token files decides which local file is sent to which host, so it must not
	// be editable by anyone else. This is checked on the descriptor the config was read from, before
	// any token file is opened. A config with only inline tokens is judged by rejectForeignConfigOwner.
	warnWhile(cfg.foreignOwner && !usesTokenFile, "owner", path,
		"credential file is owned by another user; restrict it to the user running the connector (a future release will refuse to start)", "file", "git-sync config", "path", path)
	if cfg.foreignOwner && usesTokenFile {
		const msg = "uses token_file but is owned by another user; it must be owned by the user running the connector or by root"
		return fail(IssueFile, msg, fmt.Errorf("git-sync config %s %s", path, msg))
	}

	entryIssue := func(name string, err error) {
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
	}

	owners := make(map[string]string, len(dc))
	pending := map[string]LocalEntry{} // providers whose credential is still to be read from a file
	for _, name := range names {
		if err := validateProviderName(name); err != nil {
			res.issues = append(res.issues, ConfigIssue{
				Class: IssueProviderName, Message: describeBadProviderName(name),
				err: fmt.Errorf("git-sync config %s: %w", path, err),
			})
			continue
		}
		de := dc[name]
		e := LocalEntry{BaseURL: de.BaseURL, Auth: de.Auth, Username: de.Username, Token: Secret(strVal(de.Token)), TokenFile: strVal(de.TokenFile)}
		if err := e.validateSettings(); err != nil {
			entryIssue(name, err)
			continue
		}
		if err := validateCredentialSource(de); err != nil {
			entryIssue(name, err)
			continue
		}
		if e.TokenFile == "" {
			if err := e.validateToken(); err != nil {
				entryIssue(name, err)
				continue
			}
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
		if e.TokenFile != "" {
			pending[name] = e
		} else {
			res.entries[name] = e
		}
	}
	if len(res.issues) > 0 {
		return res
	}

	// Second pass: read the token files. A file shared by several providers is read once, so they all
	// see the same bytes.
	contents := map[string][]byte{}
	for _, name := range names {
		e, ok := pending[name]
		if !ok {
			continue
		}
		tokenFileIssue := func(msg string) {
			res.issues = append(res.issues, ConfigIssue{
				Class: IssueCredentials, Provider: name, Field: "token_file", Message: msg,
				err: fmt.Errorf("provider %q %s", name, msg),
			})
		}
		raw, ok := contents[e.TokenFile]
		if !ok {
			raw, err = tokenFileReader(e.TokenFile, "token_file")
			if err != nil {
				tokenFileIssue(err.Error())
				continue
			}
			contents[e.TokenFile] = raw
			res.sources.add(newSource(e.TokenFile, raw))
		}
		token, problem := tokenFileValue(raw)
		if problem != "" {
			tokenFileIssue(fmt.Sprintf("token_file %s %s", e.TokenFile, problem))
			continue
		}
		e.Token = Secret(token)
		if err := e.validateToken(); err != nil {
			tokenFileIssue(fmt.Sprintf("token_file %s: %s", e.TokenFile, err))
			continue
		}
		res.entries[name] = e
	}
	return res
}

// validateCredentialSource checks, on a provider as written in the config file, that its credential
// comes from exactly one place: with an authentication mode, one of token and token_file (never
// both). It goes by which keys are present, not by whether they hold a value, so that an explicitly
// empty token_file is an error and cannot pass as an absent one. It also checks the token_file path,
// which is printed in messages.
func validateCredentialSource(de diskEntry) error {
	hasToken, hasFile := de.Token != nil, de.TokenFile != nil
	switch {
	case hasToken && hasFile:
		return fieldErr("token_file", "set only one of token and token_file")
	case hasFile && *de.TokenFile == "":
		return fieldErr("token_file", "token_file must not be empty")
	case hasFile:
		if err := validateTokenFilePath(*de.TokenFile); err != nil {
			return fieldErr("token_file", err.Error())
		}
	case de.Auth != "" && !hasToken:
		return fieldErr("token", "an authentication mode requires a token or a token_file")
	}
	return nil
}

// decodeConfig parses the file content strictly: unknown fields are rejected and nothing may follow
// the configuration object. It reports a problem without quoting any of the content.
func decodeConfig(path string, data []byte) (map[string]diskEntry, *ConfigIssue) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var dc map[string]diskEntry
	if err := dec.Decode(&dc); err != nil {
		return nil, decodeIssue(path, err)
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
func decodeIssue(path string, err error) *ConfigIssue {
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
		// The decoder's offset after this error is the end of the whole value, not the field, so none is given.
		msg := fmt.Sprintf("has an unknown field (a provider may set only %s)", strings.Join(knownConfigFields, ", "))
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
	// TokenFile says the credential is read from a file the config names (token_file); neither its
	// path nor its content is carried.
	TokenFile bool
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
			"the file's mode is %04o, wider than the owner-only access the connector expects (group read alone is tolerated, for mounted Secrets). The connector tightens it to 0600 where it is allowed to, and otherwise refuses a file that lets others access it.",
			info.Mode().Perm()))
	}
	res := loadConfig(path)
	rep.Missing = res.missing
	rep.Issues = res.issues
	for _, name := range slices.Sorted(maps.Keys(res.entries)) {
		e := res.entries[name]
		rep.Providers = append(rep.Providers, ProviderReport{Name: name, HostKey: res.hostKeys[name], Auth: e.Auth, HasToken: e.Token.Reveal() != "" && e.TokenFile == "", TokenFile: e.TokenFile != ""})
	}
	return rep
}
