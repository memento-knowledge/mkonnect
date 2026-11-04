package gitsync

import (
	"context"
	"errors"
	"log/slog"
)

// errCredentialSuperseded is the cause a sync attempt's context is cancelled with when the provider it
// is using is removed or its credential changes.
var errCredentialSuperseded = errors.New("credential superseded")

// attempt is the sync attempt running for one repository. It exists from claimTask until finishSync,
// and is what a replaced credential reaches to stop the git processes using it.
type attempt struct {
	bind   binding
	cancel context.CancelCauseFunc
	// superseded is set, under Client.mu, when the attempt's credential was replaced while it ran. The
	// binding comparison in finishSync would also catch that, but not a credential that was replaced and
	// then restored; this flag does.
	superseded bool
	// done is closed once finishSync has ended the attempt.
	done chan struct{}
}

// nextProviderGen draws a credential generation. Client.mu must be held, or the client not yet shared.
func (c *Client) nextProviderGen() uint64 {
	c.providerGen++
	return c.providerGen
}

// syncable reports whether repoID could be synced now: it is bound to a provider and has a usable
// replica. It gates what the manager starts; claimTask is the authority when the sync actually does.
func (c *Client) syncable(repoID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.repos[repoID]
	return ok && r.matched && r.replicaURL != ""
}

// claimTask snapshots what a sync of repoID needs, or reports that the repository is not (or no
// longer) syncable, and registers the attempt that will use it. The returned context is cancelled if
// the credential in the task is superseded.
//
// The repository's binding, the credential it names, and the registration all happen under one hold
// of c.mu, which is also what setProviders holds while it replaces the providers and cancels the
// attempts using a superseded one. So every attempt is either seen by that replacement (and cancelled)
// or claimed after it (and handed the new credential): there is no moment at which a task carries a
// credential that nothing can revoke. The context is derived from parent, the process-lifetime
// context, so a connection closing or draining does not cancel it (only shutdown or a replaced
// credential does).
func (c *Client) claimTask(parent context.Context, repoID string) (syncTask, context.Context, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.repos[repoID]
	if !ok || !r.matched || r.replicaURL == "" {
		return syncTask{}, nil, false
	}
	// Second layer of the replica-host pin: applyConfig only retains a URL on the pinned host, but a
	// sync is the point at which the push token leaves the process, so re-check it here.
	if _, v := checkReplicaURL(r.replicaURL, c.replicaHost); v != replicaOK {
		slog.Error("refusing to sync: the replica URL is not on the pinned replica host", "repo_id", repoID)
		return syncTask{}, nil, false
	}
	pe, ok := c.providers.byName[r.provider]
	if !ok || pe.gen != r.bind.gen {
		// bindRepo and setProviders keep a bound repository's binding in step with c.providers, so
		// this does not happen; if it ever did, the credential must not be used.
		slog.Error("refusing to sync: the repository's provider is out of step", "repo_id", repoID)
		return syncTask{}, nil, false
	}
	task := syncTask{
		RepoID:         repoID,
		CanonicalURL:   r.canonicalURL,
		Origin:         pe.entry,
		ReplicaURL:     r.replicaURL,
		PushToken:      r.pushToken,
		TokenExpiresAt: r.expiresAt,
		Bind:           r.bind,
	}
	if c.afterClaimRead != nil {
		c.afterClaimRead()
	}
	ctx, cancel := context.WithCancelCause(parent)
	if old := c.attempts[repoID]; old != nil {
		// The manager runs at most one attempt per repository, so this does not happen.
		old.superseded = true
		old.cancel(errCredentialSuperseded)
	}
	c.attempts[repoID] = &attempt{bind: r.bind, cancel: cancel, done: make(chan struct{})}
	return task, ctx, true
}

// endAttemptLocked removes repoID's attempt, releases its context, and reports whether its credential
// was superseded while it ran. c.mu must be held.
func (c *Client) endAttemptLocked(repoID string) (superseded bool) {
	att := c.attempts[repoID]
	if att == nil {
		return false
	}
	delete(c.attempts, repoID)
	att.cancel(nil)
	close(att.done)
	return att.superseded
}
