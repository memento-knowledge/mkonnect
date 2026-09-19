// internal/creds/store.go
package creds

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// Credential holds a plugin's base URL and optional local HTTP authentication.
type Credential struct {
	BaseURL  string `json:"base_url"`
	Auth     string `json:"auth,omitempty"` // "bearer", "basic", or ""
	Username string `json:"username,omitempty"`
	Token    string `json:"token,omitempty"`
}

// ApplyAuthorization adds this credential's Authorization header to req when its
// configured authentication mode has all required local credential fields.
func (c Credential) ApplyAuthorization(req *http.Request) {
	if c.Token == "" {
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
	// Ensure the credentials file is not readable by group or others.
	// If chmod fails, verify the existing mode is already safe before continuing;
	// an unreadable-by-others file is acceptable, a world-readable one is not.
	if err := os.Chmod(path, 0600); err != nil {
		info, statErr := os.Stat(path)
		if statErr != nil || info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("credentials file %s has unsafe permissions and chmod failed: %w", path, err)
		}
	}
	return s, nil
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
