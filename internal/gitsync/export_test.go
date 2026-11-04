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

// CanonicalRemoteURLForTest exposes the host key and the re-serialized canonical URL the sync dials,
// so a test can assert both the match identity and the exact string that would be contacted.
func CanonicalRemoteURLForTest(raw string) (hostKey, fullURL string, err error) {
	return canonicalRemoteURL(raw)
}

// RepoStateForTest returns the retained provider name, canonical URL, and match flag for a repo_id
// after applyConfig, so a test can assert the sync dials the validated canonical URL.
func (c *Client) RepoStateForTest(repoID string) (provider, canonicalURL string, matched, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.repos[repoID]
	return r.provider, r.canonicalURL, r.matched, ok
}

// ReplicaMaterialForTest returns the replica push URL and push token retained for a repo_id, so a
// test can assert what a host-matched repository holds for the sync to push with. The token is
// revealed only here, never by production code paths that log.
func (c *Client) ReplicaMaterialForTest(repoID string) (replicaURL, pushToken string, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.repos[repoID]
	return r.replicaURL, r.pushToken.Reveal(), ok
}

// RecordResultForTest records a sync outcome for a repository the way a completed sync does, so a
// test can assert how a later configuration treats an existing result.
func (c *Client) RecordResultForTest(repoID, state, errType string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.repos[repoID]; ok {
		r.result = syncResult{State: state, ErrorType: errType}
		c.repos[repoID] = r
	}
}

// FinishSyncForTest reports a sync result for the given target as a finishing sync would, so a test
// can check that a result for a target the platform has since reconfigured is ignored.
func (c *Client) FinishSyncForTest(repoID, canonicalURL, replicaURL, state, errType string) {
	c.finishSync(repoID, syncTask{RepoID: repoID, CanonicalURL: canonicalURL, ReplicaURL: replicaURL},
		syncResult{State: state, ErrorType: errType})
}

// MirrorsRootForTest returns the directory the client keeps its mirrors under.
func (c *Client) MirrorsRootForTest() string { return c.mgr.deps.mirrors.Root }

// SetChmodForTest overrides the chmod used by LoadLocalConfig so a test can exercise the
// read-only-mount fallback (chmod cannot be made to fail portably as the file's owner). It
// returns a function that restores the real chmod.
func SetChmodForTest(fn func(string, os.FileMode) error) func() {
	prev := fchmod
	fchmod = func(f *os.File, mode os.FileMode) error { return fn(f.Name(), mode) }
	return func() { fchmod = prev }
}
