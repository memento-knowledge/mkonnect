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
	// seq identifies this registration: the task carries it, and finishSync ends only the attempt
	// whose seq it still holds, so a finisher that outlived its attempt can never end a newer one.
	seq    uint64
	bind   binding
	cancel context.CancelCauseFunc
	// superseded is set, under Client.mu, when the attempt's credential was replaced while it ran. The
	// binding comparison in finishSync catches the same thing (a restored credential is a new
	// generation, see buildProviderSet); the flag is the direct statement of it, so the drop does not
	// rest on how generations are numbered.
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
	return ok && r.schedulable()
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
	c.attemptSeq++
	task.AttemptSeq = c.attemptSeq
	if c.afterClaimRead != nil {
		c.afterClaimRead()
	}
	if c.attempts[repoID] != nil {
		// The manager runs at most one attempt per repository, so this does not happen. If it ever
		// did, replacing the registration would leave the older attempt unreachable by a revocation
		// and let its finish end the newer one: refuse instead.
		slog.Error("refusing to sync: an attempt is already running for the repository", "repo_id", repoID)
		return syncTask{}, nil, false
	}
	// A credential the origin refused holds back every repository that uses it, including the syncs a
	// configuration queued behind this one. Checked here, where the sync starts, not when it was
	// queued, and last, so a refusal above does not use up a repository's pass.
	if !c.sched.mayClaim(repoID, r.bind) {
		return syncTask{}, nil, false
	}
	ctx, cancel := context.WithCancelCause(parent)
	c.attempts[repoID] = &attempt{seq: task.AttemptSeq, bind: r.bind, cancel: cancel, done: make(chan struct{})}
	return task, ctx, true
}

// endAttemptLocked ends the attempt a finishing task claimed: it removes the registration, releases
// its context, and reports whether its credential was superseded while it ran. stale is true, and
// nothing is touched, when the registered attempt is not the task's (a newer one has been
// registered since): the finisher must then leave the repository's state alone, because it now
// belongs to the newer attempt. No registration at all is not stale. c.mu must be held.
func (c *Client) endAttemptLocked(repoID string, seq uint64) (superseded, stale bool) {
	att := c.attempts[repoID]
	if att == nil {
		return false, false
	}
	if att.seq != seq {
		return false, true
	}
	delete(c.attempts, repoID)
	att.cancel(nil)
	close(att.done)
	return att.superseded, false
}
