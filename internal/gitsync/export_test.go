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

// SetChmodForTest overrides the chmod used by LoadLocalConfig so a test can exercise the
// read-only-mount fallback (chmod cannot be made to fail portably as the file's owner). It
// returns a function that restores the real chmod.
func SetChmodForTest(fn func(string, os.FileMode) error) func() {
	prev := chmod
	chmod = fn
	return func() { chmod = prev }
}
