// internal/creds/store.go
package creds

import (
	"encoding/json"
	"os"
	"sync"
)

// Credential holds a plugin's base URL and optional bearer auth.
type Credential struct {
	BaseURL string `json:"base_url"`
	Auth    string `json:"auth,omitempty"`   // "bearer" or ""
	Token   string `json:"token,omitempty"`
}

// Store is a thread-safe, file-backed map of plugin name → Credential.
type Store struct {
	mu   sync.RWMutex
	path string
	data map[string]Credential
}

// New loads or creates a credential store at path.
// If the file does not exist, an empty store is returned (not an error).
func New(path string) (*Store, error) {
	s := &Store{path: path, data: make(map[string]Credential)}
	if err := s.load(); err != nil && !os.IsNotExist(err) {
		return nil, err
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
	s.data = m
	return nil
}

// save writes the in-memory map to disk atomically via a .tmp rename. Caller must hold s.mu.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
