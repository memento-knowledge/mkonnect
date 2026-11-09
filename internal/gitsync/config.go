package gitsync

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// Caps on an inbound git_sync_config so a hostile or malformed push cannot amplify into every
// status (each accepted repository is echoed in every heartbeat status).
const (
	maxConfigRepos = 4096
	// maxRepoIDLength is the platform-contract bound on repo_id. It is at or above the 255-byte
	// NAME_MAX of common filesystems (ext4, xfs, APFS), so repo_id is never used directly as a
	// filename: Mirrors.Dir maps it to a fixed-format hashed directory name. Must stay <= 1000 (see
	// repoIDRE).
	maxRepoIDLength      = 256
	maxConfigVersionSize = 256
)

// repoIDRE constrains a platform-supplied repo_id to a safe charset and length. repo_id appears in
// logs and statuses and is the key of the configured set; it never reaches a path (the mirror
// directory is a hash of it) or a git argument. Anything outside this set — path separators, NUL,
// control characters, whitespace — is nonetheless rejected at config-validation time, as defence in
// depth and so it cannot be used for log injection. The length bound is single-sourced from
// maxRepoIDLength, which must stay <= 1000: RE2 caps a bounded repetition at 1000, so a larger value
// would panic in MustCompile at package init.
var repoIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,` + strconv.Itoa(maxRepoIDLength) + `}$`)

// validRepoID reports whether a repo_id conforms to the platform contract: the charset and length
// of repoIDRE, with the two pure path-traversal components "." and ".." additionally rejected
// (they name a directory rather than a repository and cannot be a safe path component anywhere).
//
// It deliberately does NOT reject other contract-valid ids — a leading "-", ".git", a trailing ".",
// an embedded ".." such as "a..b", or ids that differ only in case. Those are safe to STORE, and the
// risks they carry at the point of USE are closed there: repo_id is never a git argument or a path
// component (Mirrors.Dir hashes it, so length, case and ".."/".git" shapes cannot matter).
// Over-restricting here instead would reject platform-valid configurations whole and halt sync for
// every repository in the message, so the guard is kept to the agreed contract.
func validRepoID(id string) bool {
	return repoIDRE.MatchString(id) && id != "." && id != ".."
}

// repoState is the retained state of a configured repository.
//
// The raw origin_url string is deliberately NOT retained. applyConfig validates it once and keeps the
// re-serialized canonical URL (originURL) and its host key (originHostKey); which local provider, if
// any, the repository is bound to is derived from the host key by bindRepo, against whichever provider
// set is current, and is redone when the provider configuration is reloaded. The sync dials
// canonicalURL (the originURL of a bound repository) directly and never re-parses an unvalidated
// string with git's URL parser (whose host interpretation can diverge from ours), so the host that
// was match-checked is byte-for-byte the host that gets contacted.
//
// The push token is retained, in memory only and as a self-redacting Secret, for every repository
// whose replica configuration is usable, bound or not: a provider added later by a reload makes a
// repository syncable without the platform sending its configuration again. It is copied into a
// syncTask only for a bound repository. Hardening it at rest is Phase 4.
type repoState struct {
	repoID    string
	expiresAt string // push_token_expires_at (RFC3339), "" when none
	// originURL is the validated, re-serialized origin URL, and originHostKey its scheme://host:port;
	// both are "" when origin_url is not a valid remote URL.
	originURL     string
	originHostKey string
	// target fingerprints the origin and replica this repository replicates between (see
	// targetFingerprint), whether or not it can currently be synced.
	target string

	// replicaUsable says the replica URL is on the pinned host and the push token is usable. The
	// replica material below is set exactly when it is.
	replicaUsable bool
	replicaURL    string // validated, re-serialized https replica URL on the pinned host, "" when unusable
	pushToken     Secret // platform push token for replicaURL; empty exactly when replicaURL is

	// Derived by bindRepo from originHostKey and the current provider set; never set elsewhere.
	bind         binding
	provider     string // bound local provider name, "" when unbound
	canonicalURL string // origin URL the sync dials, "" when unbound
	matched      bool   // origin_url binds to a local provider

	// result is the recorded outcome of the last sync attempt, or the reason the configuration is
	// unusable. A zero State means no outcome yet, which is reported as pending.
	result  syncResult
	syncing bool // a sync is queued or running
	// attemptStartedAt is when the attempt now running began, and zero while none is: a queued
	// repository has not started one. It is in memory only; only finished attempts are persisted.
	attemptStartedAt time.Time
}

// schedulable says the connector may sync r: it is bound to a local provider and has a usable replica.
// The gates of a provider's credential are kept for exactly these repositories (see
// providerGates.retain), so the origin check only dials repositories that satisfy it.
func (r repoState) schedulable() bool { return r.matched && r.replicaURL != "" }

// bindRepo returns r bound against set: matched exactly when a provider serves its origin's host. A
// repository whose binding changes (bound, unbound, or bound to another provider or another
// generation of its credential) starts over: what its last attempt reported was said about a
// different binding. The invalid-replica result is a property of the configuration rather than of
// any attempt, so a bound repository with an unusable replica always carries it.
func bindRepo(r repoState, set *providerSet) repoState {
	old := r.bind
	if pe, name, ok := set.match(r.originHostKey); ok {
		r.bind = binding{provider: name, gen: pe.gen}
		r.provider, r.canonicalURL, r.matched = name, r.originURL, true
	} else {
		r.bind = binding{}
		r.provider, r.canonicalURL, r.matched = "", "", false
	}
	if r.bind != old {
		r.result = syncResult{}
	}
	if r.matched && !r.replicaUsable {
		r.result = syncResult{State: StateError, ErrorType: ErrUnclassified, ErrorDetail: errInvalidReplica}
	}
	return r
}

// errInvalidReplica is the detail reported for a matched repository whose replica URL or push token
// is unusable (the URL missing, or on the pinned host but carrying userinfo, a query, a fragment or
// whitespace; or the token empty or containing control characters). It is
// deliberately generic: the status must never echo the platform-supplied URL.
const errInvalidReplica = "invalid replica configuration"

// validPushToken reports whether a platform-supplied push token is usable. It becomes an
// Authorization header value in a subprocess environment, so it must be non-empty and free of
// control characters: a CR or LF would inject headers into the replica request and a NUL would make
// the process fail to start. (The operator's own origin token gets the same check; see
// LocalEntry.Validate.)
func validPushToken(tok Secret) bool {
	s := tok.Reveal()
	return s != "" && !strings.ContainsFunc(s, unicode.IsControl)
}

// sameSyncIdentity reports whether two states describe the same sync target, so a prior result is
// still meaningful. The push token is deliberately not part of it: a refreshed token for the same
// repository does not invalidate its last result.
func sameSyncIdentity(a, b repoState) bool {
	return a.bind == b.bind && a.canonicalURL == b.canonicalURL && a.replicaURL == b.replicaURL
}

// rejectConfig records that the most recent git_sync_config was rejected as a whole, so the status
// carries config_error{config_version, reason} until a later configuration is accepted. reason is
// one of the proto.GitSyncConfigErr* values. version is the rejected message's config_version; it is
// echoed only when it is a bounded JSON scalar and is otherwise reported as null, so a hostile value
// cannot ride back to the platform. Exactly one line is logged, carrying the reason and nothing
// taken from the message. The caller leaves the previous configuration untouched.
func (c *Client) rejectConfig(version json.RawMessage, reason string) {
	if len(version) > maxConfigVersionSize || !isScalarJSON(version) {
		version = nil
	}
	c.mu.Lock()
	c.configErr = &proto.GitSyncConfigError{ConfigVersion: cloneRaw(version), Reason: reason}
	c.mu.Unlock()
	slog.Warn("git_sync_config rejected; leaving configuration unchanged", "reason", reason)
}

// replicaCheck is the outcome of checking one repository's replica_push_url against the pin.
type replicaCheck struct {
	url     string // the canonical URL to dial; set only for replicaOK
	verdict replicaVerdict
}

// applyConfig validates an inbound git_sync_config and, only if the whole message is acceptable,
// swaps in the new desired set and config version. A rejected message leaves the previous
// configuration untouched, does not advance the version, and is reported in the status as
// config_error (see rejectConfig). It returns whether the config was accepted (and syncs should
// follow). It never logs a credential, push token, or value taken from the message.
//
// The replica host pin is enforced first and for every repository, whether or not its origin
// host-matches a local provider: with no valid pin every configuration is rejected
// (replica_host_not_pinned), and with one, a configuration in which any replica_push_url names a
// different host and port is rejected whole (replica_host_mismatch). Nothing is built from a
// configuration before that check has passed.
func (c *Client) applyConfig(raw json.RawMessage) bool {
	// The version is read separately so a rejection can echo it even when the rest of the message
	// does not decode.
	var head struct {
		ConfigVersion json.RawMessage `json:"config_version"`
	}
	_ = json.Unmarshal(raw, &head)
	version := head.ConfigVersion

	if c.replicaHost == "" {
		c.rejectConfig(version, proto.GitSyncConfigErrReplicaHostNotPinned)
		return false
	}
	var msg proto.GitSyncConfigMsg
	if err := json.Unmarshal(raw, &msg); err != nil || msg.Repositories == nil {
		c.rejectConfig(version, proto.GitSyncConfigErrMalformedMessage)
		return false
	}
	if len(msg.Repositories) > maxConfigRepos {
		c.rejectConfig(version, proto.GitSyncConfigErrTooManyRepositories)
		return false
	}
	// Check every replica URL before any per-repo state is built, so a hostile URL is reported as such
	// rather than hidden behind a later structural error.
	replicas := make([]replicaCheck, len(msg.Repositories))
	for i, r := range msg.Repositories {
		canonical, verdict := checkReplicaURL(r.ReplicaPushURL, c.replicaHost)
		if verdict == replicaMismatch {
			c.rejectConfig(version, proto.GitSyncConfigErrReplicaHostMismatch)
			return false
		}
		replicas[i] = replicaCheck{url: canonical, verdict: verdict}
	}
	// Further structural checks (for example schedule and limits ranges) belong after the replica
	// pass, so a hostile replica URL is never hidden behind them.
	if msg.ConfigVersion != nil && (len(msg.ConfigVersion) > maxConfigVersionSize || !isScalarJSON(msg.ConfigVersion)) {
		c.rejectConfig(version, proto.GitSyncConfigErrInvalidConfigVersion)
		return false
	}
	limits, err := ResolveLimits(msg.Schedule, msg.Limits)
	if err != nil {
		// The error names the offending field and carries no wire value, so it is safe to log.
		slog.Warn("git_sync_config has a schedule or limit out of range", "err", err)
		c.rejectConfig(version, proto.GitSyncConfigErrLimitsOutOfRange)
		return false
	}
	next := make(map[string]repoState, len(msg.Repositories))
	for i, r := range msg.Repositories {
		if !validRepoID(r.RepoID) {
			// A value outside the contract charset (or a "." / ".." traversal component) is rejected
			// here rather than carried into logs and statuses.
			c.rejectConfig(version, proto.GitSyncConfigErrInvalidRepoID)
			return false
		}
		if _, dup := next[r.RepoID]; dup {
			c.rejectConfig(version, proto.GitSyncConfigErrDuplicateRepoID)
			return false
		}
		// Only the origin is validated here. Which provider serves it is decided under c.mu, together
		// with installing the result, so a reload of the providers cannot slip in between and leave a
		// stale binding behind.
		var originURL, originHostKey string
		if hostKey, fullURL, err := canonicalRemoteURL(r.OriginURL); err == nil {
			originURL, originHostKey = fullURL, hostKey
		}
		// A repository whose replica is not usable fails on its own, once it is bound (see bindRepo):
		// the others are independent and halting them all would be disproportionate. (A replica on
		// another host has already rejected the whole message above.)
		usable := replicas[i].verdict == replicaOK && validPushToken(r.PushToken)
		st := repoState{
			repoID: r.RepoID, expiresAt: r.PushTokenExpiresAt, originURL: originURL, originHostKey: originHostKey,
			// The replica is empty unless usable, so a repository whose replica is not is a different
			// target from the one that was synced.
			target:        targetFingerprint(originURL, replicas[i].url),
			replicaUsable: usable,
		}
		if usable {
			st.replicaURL, st.pushToken = replicas[i].url, r.PushToken
		}
		next[r.RepoID] = st
	}
	if msg.ConfigVersion == nil {
		slog.Warn("git_sync_config has no config_version; status will report null until the platform includes one")
	}
	if c.beforeConfigInstall != nil {
		c.beforeConfigInstall()
	}
	c.mu.Lock()
	// Carry a prior result and in-flight marker across a configuration that leaves a repository's
	// sync target unchanged (the platform re-sends config on every token refresh), so a healthy
	// repository is not reset to pending. A changed target starts over.
	for id, st := range next {
		st = bindRepo(st, c.providers)
		next[id] = st
		if old, ok := c.repos[id]; ok && sameSyncIdentity(old, st) {
			if st.result.State == "" {
				st.result = old.result
			}
			st.syncing = old.syncing
			st.attemptStartedAt = old.attemptStartedAt
			next[id] = st
		}
	}
	c.repos = next
	c.configVersion = cloneRaw(msg.ConfigVersion)
	c.configAccepted = true
	c.configErr = nil
	// The configuration is a full replace, so limits it leaves out revert to their defaults.
	c.limits.Store(&limits)
	c.mgr.resize(limits.MaxConcurrency)
	runsChanged := c.reconcileRunsLocked(next)
	// next is c.repos now, which syncs update under mu: count it before letting go of the lock.
	repoCount := len(next)
	// A check that is dialling what this configuration no longer lists (the base URL it replaces, a
	// repository whose URL changed) is stopped, and a provider whose check is no longer about what is
	// configured is checked now, so a result about the base URL does not stand in for the repository
	// check for a whole interval. A configuration that leaves every provider's targets as they were
	// (the platform re-sends it on a token refresh) changes neither.
	c.cancelStaleProbeLocked()
	c.mu.Unlock()
	if runsChanged {
		c.markRunsDirty()
	}
	slog.Info("git_sync_config accepted", "repo_count", repoCount)
	c.maybeProbe()
	return true
}

// reconcileRunsLocked brings the run records in line with an accepted configuration and reports
// whether any changed. A record made against a different origin or replica than the repository now
// has (including one loaded from before a restart) describes a different replication: a success is
// about the replica that was pushed to, so it is dropped, whether or not the repository can currently
// be synced. A repository that cannot be synced but whose origin and replica are unchanged (for
// example its provider was removed locally) keeps its record, since the replica still holds what was
// last pushed. Records of repositories the configuration no longer lists are pruned, except when it
// lists none at all: an empty set is far likelier a platform mishap than a decision to forget every
// success. c.mu must be held.
func (c *Client) reconcileRunsLocked(next map[string]repoState) bool {
	changed := false
	for id, rec := range c.runs {
		st, listed := next[id]
		if (len(next) > 0 && !listed) || (listed && rec.Target != st.target) {
			delete(c.runs, id)
			changed = true
		}
	}
	return changed
}

// cloneRaw copies a RawMessage so the stored/echoed value does not alias a decoder's buffer.
func cloneRaw(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	return append(json.RawMessage(nil), b...)
}

// isScalarJSON reports whether b is a valid JSON scalar (not an object or array). config_version is
// treated opaquely but constrained to a scalar so a hostile nested value cannot be echoed verbatim.
func isScalarJSON(b json.RawMessage) bool {
	if !json.Valid(b) {
		return false
	}
	s := bytes.TrimSpace(b)
	if len(s) == 0 {
		return false
	}
	switch s[0] {
	case '{', '[':
		return false
	}
	return true
}
