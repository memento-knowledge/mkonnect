package gitsync

// Test seams for the replica-host pin, kept apart from export_test.go.

// CheckReplicaURLForTest exposes the replica URL classification: the canonical URL that would be
// dialled and the verdict ("ok", "invalid" or "mismatch").
func CheckReplicaURLForTest(raw, pin string) (canonical, verdict string) {
	canon, v := checkReplicaURL(raw, pin)
	switch v {
	case replicaOK:
		return canon, "ok"
	case replicaInvalid:
		return canon, "invalid"
	default:
		return canon, "mismatch"
	}
}

// ForceReplicaURLForTest overwrites a retained repository's replica URL, bypassing applyConfig, so a
// test can prove the sync-time guard holds even if some other path stored an unpinned URL.
func (c *Client) ForceReplicaURLForTest(repoID, replicaURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.repos[repoID]; ok {
		r.replicaURL = replicaURL
		c.repos[repoID] = r
	}
}
