package gitsync

import (
	"os"

	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// Test seams exposing unexported behaviour to the external gitsync_test package, following the
// repo's export_test.go convention (see internal/ws/export_test.go).

// HandleSyncNowForTest exposes handleSyncNow so a test can assert which repositories a
// git_sync_now addresses, and that a critical-patch gate or an unknown key suppresses it.
func (c *Client) HandleSyncNowForTest(msg proto.GitSyncNowMsg) []string { return c.handleSyncNow(msg) }

// SetCriticalPatchForTest sets the critical-patch gate without a handshake.
func (c *Client) SetCriticalPatchForTest(v bool) { c.criticalPatchRequired.Store(v) }

// CanonicalHostKeyForTest exposes the host-canonicalization used by the match guard so a test can
// assert that a bad host is rejected (an error), not merely that it fails to match a provider.
func CanonicalHostKeyForTest(raw string) (string, error) { return canonicalHostKey(raw) }

// CanonicalRemoteURLForTest exposes the host key and the re-serialized canonical URL Phase 3 dials,
// so a test can assert both the match identity and the exact string that would be contacted.
func CanonicalRemoteURLForTest(raw string) (hostKey, fullURL string, err error) {
	return canonicalRemoteURL(raw)
}

// RepoStateForTest returns the retained provider name, canonical URL, and match flag for a repo_id
// after applyConfig, so a test can assert Phase 3 will dial the validated canonical URL.
func (c *Client) RepoStateForTest(repoID string) (provider, canonicalURL string, matched, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.repos[repoID]
	return r.provider, r.canonicalURL, r.matched, ok
}

// SetChmodForTest overrides the chmod used by LoadLocalConfig so a test can exercise the
// read-only-mount fallback (chmod cannot be made to fail portably as the file's owner). It
// returns a function that restores the real chmod.
func SetChmodForTest(fn func(string, os.FileMode) error) func() {
	prev := chmod
	chmod = fn
	return func() { chmod = prev }
}
