package gitsync

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gateway"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

const (
	// statusWriteTimeout bounds the connection-time status write, before the heartbeat exists to
	// tear down a wedged connection.
	statusWriteTimeout = 30 * time.Second
	// dispatchStatusTimeout bounds a status write made from the read loop or a probe goroutine,
	// whose context is the process-lifetime Connect context.
	dispatchStatusTimeout = 10 * time.Second
	// rfc3339Nano is a fixed-width RFC3339 layout that always carries a sub-second fraction, so
	// sent_at is stable even when the nanosecond component is zero.
	rfc3339Nano = "2006-01-02T15:04:05.000000000Z07:00"
)

// notifyStatus reports that a sync finished. Each status echoes every repository, so sending one per
// completion would cost O(N²) bytes for N repositories; instead the first is sent at once and any
// that follow within statusInterval are folded into a single trailing flush, which also guarantees
// the final state is reported.
func (c *Client) notifyStatus() {
	c.notifyMu.Lock()
	wait := c.statusInterval - time.Since(c.lastNotify)
	if wait <= 0 && !c.notifyScheduled {
		c.lastNotify = time.Now()
		c.notifyMu.Unlock()
		c.sendCurrentStatus()
		return
	}
	if !c.notifyScheduled {
		c.notifyScheduled = true
		time.AfterFunc(wait, c.flushNotify)
	}
	c.notifyMu.Unlock()
}

func (c *Client) flushNotify() {
	c.notifyMu.Lock()
	c.notifyScheduled = false
	c.lastNotify = time.Now()
	c.notifyMu.Unlock()
	c.sendCurrentStatus()
}

// sendCurrentStatus sends the current status over the current connection, if there is a live one. A
// sync that finishes mid-reconnect simply skips it: the next heartbeat reports the result anyway.
func (c *Client) sendCurrentStatus() {
	c.mu.Lock()
	ctx, s := c.connCtx, c.connSender
	c.mu.Unlock()
	if ctx == nil || s == nil || ctx.Err() != nil {
		return
	}
	c.emitStatus(ctx, s, dispatchStatusTimeout)
}

// buildStatus assembles the current git_sync_status. repositories is always non-nil so it marshals
// as [] rather than null even when empty. Called under statusMu, so the sequence increment here
// matches wire order.
func (c *Client) buildStatus() proto.GitSyncStatusMsg {
	c.mu.Lock()
	cv := cloneRaw(c.configVersion)
	accepted := c.configAccepted
	snapshot := make([]repoState, 0, len(c.repos))
	for _, r := range c.repos {
		snapshot = append(snapshot, r)
	}
	runs := maps.Clone(c.runs)
	c.mu.Unlock()

	slices.SortFunc(snapshot, func(a, b repoState) int { return strings.Compare(a.repoID, b.repoID) })

	repos := make([]proto.GitSyncRepoStatus, 0, len(snapshot))
	var earliest *time.Time
	for _, r := range snapshot {
		st := runStatus(r.repoID, runs[r.repoID], r.attemptStartedAt)
		switch {
		case !r.matched:
			st.State = StateError
			st.ErrorType = ErrOriginAccessDenied
			st.ErrorDetail = "host not configured locally"
		case r.syncing:
			st.State = StateSyncing
		case r.result.State != "":
			st.State = r.result.State
			st.ErrorType = r.result.ErrorType
			st.ErrorDetail = r.result.ErrorDetail
			st.Warnings = r.result.Warnings
		default:
			st.State = StatePending
		}
		repos = append(repos, st)
		if t, ok := parseExpiry(r.expiresAt); ok && (earliest == nil || t.Before(*earliest)) {
			tt := t
			earliest = &tt
		}
	}

	status := proto.GitSyncStatusMsg{
		Type:          "git_sync_status",
		ConfigVersion: cv,
		SentAt:        c.now().UTC().Format(rfc3339Nano),
		Sequence:      c.seq.Add(1),
		BootID:        c.bootID,
		Repositories:  repos,
	}
	if earliest != nil {
		s := earliest.UTC().Format(time.RFC3339)
		status.EarliestPushTokenExpiresAt = &s
	}
	// origin_check is a connector-level, pre-configuration signal only.
	if !accepted {
		if oc := c.originCheck.Load(); oc != nil {
			status.OriginCheck = oc
		}
	}
	return status
}

// runStatus fills a repository's status from its run record: when it last tried and last succeeded,
// how the last finished attempt went, and what the origin looked like then. While an attempt is in
// flight its start replaces last_attempt_at and everything else keeps describing the previous
// finished attempt. last_success_at is always a present field, null until the first success, so the
// platform can tell "never succeeded" from "not reported".
func runStatus(repoID string, rec runRecord, inFlightSince time.Time) proto.GitSyncRepoStatus {
	st := proto.GitSyncRepoStatus{RepoID: repoID, OriginDefaultBranch: rec.OriginDefaultBranch}
	attempt := rec.LastAttemptAt
	if !inFlightSince.IsZero() {
		attempt = inFlightSince
	}
	st.LastAttemptAt = rfc3339Seconds(attempt)
	st.LastSuccessAt = rfc3339Seconds(rec.LastSuccessAt)
	st.NewestOriginCommitAt = rfc3339Seconds(rec.NewestOriginCommitAt)
	if rec.DurationMS != nil {
		d := *rec.DurationMS
		st.DurationMS = &d
	}
	if rec.RefsPushed != nil {
		n := *rec.RefsPushed
		st.RefsPushed = &n
	}
	return st
}

// rfc3339Seconds renders t as RFC3339 UTC, or nil for the zero time.
func rfc3339Seconds(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// sendStatus builds and sends the current status under statusMu, so concurrent heartbeat and read
// loop sends cannot reorder a stale snapshot ahead of a fresh one.
func (c *Client) sendStatus(ctx context.Context, s gateway.Sender) error {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	return s.Send(ctx, c.buildStatus())
}

// emitStatus sends a status with its own timeout, for callers whose context is the process-lifetime
// Connect context rather than a per-write bound.
func (c *Client) emitStatus(ctx context.Context, s gateway.Sender, timeout time.Duration) {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.sendStatus(wctx, s); err != nil {
		slog.Warn("failed to send git_sync_status", "err", err)
	}
}

// parseExpiry parses an RFC3339 timestamp, reporting ok=false for an empty or unparseable value, so
// a malformed expiry is skipped when computing the earliest rather than being misread as a time.
func parseExpiry(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
