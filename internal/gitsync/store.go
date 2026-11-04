package gitsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

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
