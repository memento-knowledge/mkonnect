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
	"unicode/utf8"
)

// MinTokenLength prevents ambiguous, short strings from being used as local
// credentials or mistaken for ordinary response content.
const MinTokenLength = 16

// Credential holds a plugin's base URL and optional local HTTP authentication.
type Credential struct {
	BaseURL  string `json:"base_url"`
	Auth     string `json:"auth,omitempty"` // "bearer", "basic", or ""
	Username string `json:"username,omitempty"`
	Token    string `json:"token,omitempty"`
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
	return nil
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
	return s.save()
}

// Remove deletes the credential for plugin and persists to disk.
func (s *Store) Remove(plugin string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, plugin)
	return s.save()
}

// Reload re-reads credentials.json from disk, replacing the in-memory map.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
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
