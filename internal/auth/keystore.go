// Package auth handles ML-DSA-65 key storage and challenge signing for mkonnect.
package auth

import (
	"errors"
	"fmt"
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
	data, err := os.ReadFile(ks.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
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
func (ks *KeyStore) Save(priv *mldsa65.PrivateKey) error {
	if err := os.MkdirAll(filepath.Dir(ks.path), 0700); err != nil {
		return fmt.Errorf("creating key directory: %w", err)
	}
	var buf [mldsa65.PrivateKeySize]byte
	priv.Pack(&buf)
	if err := os.WriteFile(ks.path, buf[:], 0600); err != nil {
		return fmt.Errorf("writing key file: %w", err)
	}
	return nil
}
