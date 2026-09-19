// Package auth handles ML-DSA-65 key storage and challenge signing for mkonnect.
package auth

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
)

// KeyStore manages the on-disk ML-DSA-65 private key.
type KeyStore struct {
	path string
}

// NewKeyStore creates a KeyStore backed by the given file path.
func NewKeyStore(path string) *KeyStore {
	return &KeyStore{path: path}
}

// Load reads the private key from disk.
// Returns (privKey, true, nil) if the file exists and is valid.
// Returns (nil, false, nil) if the file does not exist.
// Returns (nil, false, err) on read or parse errors.
func (ks *KeyStore) Load() (*mldsa65.PrivateKey, bool, error) {
	f, err := os.Open(ks.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading key file: %w", err)
	}
	defer f.Close() //nolint:errcheck

	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("stat key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("key file must be a regular file")
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, false, fmt.Errorf("key file has unsafe permissions %04o; require owner-only access", perm)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false, fmt.Errorf("reading key file: %w", err)
	}
	priv, err := UnmarshalPrivateKey(data)
	if err != nil {
		return nil, false, fmt.Errorf("parsing key file: %w", err)
	}
	return priv, true, nil
}

// Save writes the private key to disk with 0600 permissions.
// It creates parent directories as needed.
func (k *KeyStore) Save(priv *mldsa65.PrivateKey) error {
	if err := os.MkdirAll(filepath.Dir(k.path), 0700); err != nil {
		return fmt.Errorf("create key dir: %w", err)
	}
	data := make([]byte, mldsa65.PrivateKeySize)
	priv.Pack((*[mldsa65.PrivateKeySize]byte)(data))

	// Write to temp file then rename for atomicity
	dir := filepath.Dir(k.path)
	tmp, err := os.CreateTemp(dir, ".key-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp key file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op if rename succeeded
	}()
	if err := tmp.Chmod(0600); err != nil {
		return fmt.Errorf("set key file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write key data: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp key file: %w", err)
	}
	if err := os.Rename(tmpName, k.path); err != nil {
		return fmt.Errorf("install key file: %w", err)
	}
	return nil
}
