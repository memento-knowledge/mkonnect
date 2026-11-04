package gitsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Store is the provider-keyed, file-backed local config with a write path (used by the operator
// CLI) and read accessors (used by the connector). It mirrors internal/creds/store.go, with the
// redaction-safe persistence above.
type Store struct {
	mu      sync.RWMutex
	path    string
	data    map[string]LocalEntry
	sources sourceSet
}

// New loads the provider config from path. A missing file yields an empty store and no error. A
// present file is vetted (see readProviderConfig), tightened to owner-only, decoded strictly
// (unknown fields rejected), and every entry validated; a duplicate host (two providers that
// resolve to the same scheme+host+port) is rejected so credential selection can never be ambiguous.
//
// A provider's credential is either an inline token or a token_file, the path of a file holding it
// (see readTokenFile for the checks on that file). Every provider is validated before any token file
// is opened, so a token file is read only for a configuration that is otherwise acceptable. No error
// includes a credential value or token-file content; errors name the provider and the problem.
//
// Any failure after the file was located is a *LoadError (use errors.As) carrying the files involved.
// The files of a successful load are available from Sources.
func New(path string) (*Store, error) {
	s := &Store{path: path, data: map[string]LocalEntry{}}
	sources := sourceSet{}
	fail := func(err error) (*Store, error) { return nil, &LoadError{Sources: sources.sorted(), err: err} }

	cfg, err := readProviderConfig(path, maxConfigFileSize, rejectForeignConfigOwner)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.sources = sourceSet{path: {Path: path}}
			return s, nil
		}
		sources.add(Source{Path: path})
		return fail(err)
	}
	sources.add(newSource(path, cfg.data))

	dec := json.NewDecoder(bytes.NewReader(cfg.data))
	dec.DisallowUnknownFields()
	var dc map[string]diskEntry
	if err := dec.Decode(&dc); err != nil {
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return fail(fmt.Errorf("git-sync config %s is not valid JSON (at byte offset %d)", path, syntaxErr.Offset))
		}
		return fail(fmt.Errorf("git-sync config %s is not valid: %w", path, err))
	}
	// Reject anything after the first JSON value (a second value or trailing garbage), so a
	// malformed file cannot be silently half-accepted. Trailing whitespace yields io.EOF.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fail(fmt.Errorf("git-sync config %s has unexpected trailing data after the configuration object", path))
	}
	names := slices.Sorted(maps.Keys(dc))

	// Record every well-formed token file now, so a failed load still reports the files a retry
	// should watch (including one that does not exist yet).
	usesTokenFile := false
	for _, name := range names {
		if tf := dc[name].TokenFile; tf != "" {
			usesTokenFile = true
			if validateTokenFilePath(tf) == nil {
				sources.add(Source{Path: tf})
			}
		}
	}
	// A config that names token files decides which local file is sent to which host, so it must not
	// be editable by anyone else. This is checked on the descriptor the config was read from, before
	// any token file is opened. A config with only inline tokens is judged by rejectForeignConfigOwner.
	warnWhile(cfg.foreignOwner && !usesTokenFile, "owner", path,
		"credential file is owned by another user; restrict it to the user running the connector (a future release will refuse to start)", "file", "git-sync config", "path", path)
	if cfg.foreignOwner && usesTokenFile {
		return fail(fmt.Errorf("git-sync config %s uses token_file but is owned by another user; it must be owned by the user running the connector or by root", path))
	}

	// First pass: everything that does not need a credential.
	hostKeys := make(map[string]string, len(dc))
	entries := make(map[string]LocalEntry, len(dc))
	for _, name := range names {
		de := dc[name]
		if err := validateProviderName(name); err != nil {
			return fail(fmt.Errorf("git-sync config %s: %w", path, err))
		}
		e := LocalEntry{BaseURL: de.BaseURL, Auth: de.Auth, Username: de.Username, Token: Secret(de.Token), TokenFile: de.TokenFile}
		if err := validateCredentialSource(e); err != nil {
			return fail(fmt.Errorf("provider %q is invalid: %w", name, err))
		}
		if err := e.validateSettings(); err != nil {
			return fail(fmt.Errorf("provider %q is invalid: %w", name, err))
		}
		if e.TokenFile == "" {
			if err := e.validateToken(); err != nil {
				return fail(fmt.Errorf("provider %q is invalid: %w", name, err))
			}
		}
		hk, err := canonicalHostKey(e.BaseURL)
		if err != nil {
			return fail(fmt.Errorf("provider %q is invalid: base_url %w", name, err))
		}
		if other, ok := hostKeys[hk]; ok {
			return fail(fmt.Errorf("git-sync config %s: providers %q and %q use the same host; each host may be configured once", path, other, name))
		}
		hostKeys[hk] = name
		entries[name] = e
	}

	// Second pass: read the token files. A file shared by several providers is read once, so they
	// all see the same bytes.
	contents := map[string][]byte{}
	for _, name := range names {
		e := entries[name]
		if e.TokenFile == "" {
			continue
		}
		what := fmt.Sprintf("provider %q token_file", name)
		raw, ok := contents[e.TokenFile]
		if !ok {
			raw, err = tokenFileReader(e.TokenFile, what)
			if err != nil {
				return fail(err)
			}
			contents[e.TokenFile] = raw
			sources.add(newSource(e.TokenFile, raw))
		}
		token, problem := tokenFileValue(raw)
		if problem != "" {
			return fail(fmt.Errorf("%s %s %s", what, e.TokenFile, problem))
		}
		e.Token = Secret(token)
		if err := e.validateToken(); err != nil {
			return fail(fmt.Errorf("%s %s: %w", what, e.TokenFile, err))
		}
		entries[name] = e
	}
	s.data = entries
	s.sources = sources
	return s, nil
}

// validateCredentialSource checks, on a provider as written in the config file, that its credential
// comes from exactly one place: with an authentication mode, one of token and token_file (never
// both). It also checks the token_file path, which is printed in messages.
func validateCredentialSource(e LocalEntry) error {
	hasToken, hasFile := e.Token.Reveal() != "", e.TokenFile != ""
	switch {
	case hasToken && hasFile:
		return errors.New("set only one of token and token_file")
	case e.Auth != "" && !hasToken && !hasFile:
		return errors.New("an authentication mode requires a token or a token_file")
	case hasFile:
		return validateTokenFilePath(e.TokenFile)
	}
	return nil
}

// Sources returns the files the last load read, sorted by path: the config file (listed even when it
// does not exist, so a reload notices it appearing) and each token file. See Source.
func (s *Store) Sources() []Source {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sources.sorted()
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
	if e.TokenFile != "" {
		return errors.New("a token_file provider is configured by editing the configuration file, not through Set")
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
// form reveals an inline token (the file must hold the real value), never the Secret's redaction.
// A token-file provider is written back as its path alone: the credential belongs to that file and
// must not be copied into the config.
func (s *Store) save(data map[string]LocalEntry) error {
	dc := make(map[string]diskEntry, len(data))
	for name, e := range data {
		de := diskEntry{BaseURL: e.BaseURL, Auth: e.Auth, Username: e.Username}
		if e.TokenFile != "" {
			de.TokenFile = e.TokenFile
		} else {
			de.Token = e.Token.Reveal()
		}
		dc[name] = de
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
	// The config now on disk is what this store holds, so Sources must describe it: the new config,
	// plus the token files that the entries now saved still name (a replaced or removed token-file
	// provider no longer contributes one). Their fingerprints are those of the load that read them.
	next := sourceSet{s.path: newSource(s.path, b)}
	for _, e := range data {
		if e.TokenFile != "" {
			if src, ok := s.sources[e.TokenFile]; ok {
				next[e.TokenFile] = src
			}
		}
	}
	s.sources = next
	return nil
}
