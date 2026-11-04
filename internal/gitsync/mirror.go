package gitsync

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// Mirrors locates the bare mirror repositories the connector keeps on its local volume. Each
// repository maps to one fixed-format directory under Root.
//
// The directory name is the SHA-256 of the repo_id, never the repo_id itself: a repo_id may be up to
// 256 bytes (above the 255-byte NAME_MAX of common filesystems), may differ from another only in
// case (collapsing on a case-insensitive filesystem), and may be "..", ".git"-suffixed, or begin
// with "-". A 64-hex-character name has none of those problems and cannot be steered by the platform.
type Mirrors struct {
	Root string
}

// DefaultMirrorsRoot returns where mirrors are kept: GIT_SYNC_MIRRORS_DIR if set, otherwise a
// "mirrors" directory beside the provider config file, so the one mounted volume holds both.
func DefaultMirrorsRoot() string {
	if p := os.Getenv("GIT_SYNC_MIRRORS_DIR"); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(DefaultConfigPath()), "mirrors")
}

// Dir returns the mirror directory for repoID.
func (m Mirrors) Dir(repoID string) string {
	sum := sha256.Sum256([]byte(repoID))
	return filepath.Join(m.Root, hex.EncodeToString(sum[:])+".git")
}

// EnsureRoot creates the mirror root (owner-only) if it does not exist.
func (m Mirrors) EnsureRoot() error {
	return os.MkdirAll(m.Root, 0o700)
}

// Exists reports whether a mirror directory for repoID is present. It says nothing about whether the
// directory is a valid repository; the sync treats an unusable one as a failed fetch and rebuilds it.
func (m Mirrors) Exists(repoID string) bool {
	info, err := os.Stat(m.Dir(repoID))
	return err == nil && info.IsDir()
}
