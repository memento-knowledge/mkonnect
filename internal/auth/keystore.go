// Package auth handles ML-DSA-65 key storage and challenge signing for mkonnect.
package auth

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"golang.org/x/sys/unix"
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
	fd, err := unix.Open(ks.path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading key file: %w", err)
	}
	f := os.NewFile(uintptr(fd), ks.path)
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

// EnsureWritable verifies the connector can create the key file in its directory,
// creating the directory if needed. It is used as a pre-flight check before first-run
// registration: the gateway consumes the one-time registration token and issues the
// private key as soon as the connector registers, so if the key cannot be persisted the
// token would be burned for nothing. It writes and removes a temporary probe file,
// mirroring what Save does, so a success here predicts that Save will succeed.
func (k *KeyStore) EnsureWritable() error {
	dir := filepath.Dir(k.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create key dir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".keycheck-*.tmp")
	if err != nil {
		return fmt.Errorf("key directory %s is not writable (fix the volume/mount permissions): %w", dir, err)
	}
	name := f.Name()
	// Write a full key-sized payload, not just create the file: inode creation can
	// succeed on a device that is out of space where the subsequent write fails, and
	// Save writes the whole key — so the probe should too.
	_, writeErr := f.Write(make([]byte, mldsa65.PrivateKeySize))
	closeErr := f.Close()
	_ = os.Remove(name) // best-effort cleanup of the probe file
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("key directory %s is not writable (fix the volume/mount permissions): %w", dir, err)
	}
	return nil
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
