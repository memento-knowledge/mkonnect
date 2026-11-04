package gitsync

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
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
// The raw origin_url string is deliberately NOT retained. When a repository host-matches a local
// provider, applyConfig resolves and stores the matched provider name plus the validated,
// re-serialized canonical origin URL. The sync dials canonicalURL directly and never re-parses an
// unvalidated string with git's URL parser (whose host interpretation can diverge from ours), so
// the host that was match-checked is byte-for-byte the host that gets contacted.
//
// The push token is retained, in memory only and as a self-redacting Secret, and only for a
// repository that is host-matched and has a usable replica URL: a repository that can never be
// synced holds no secret. Hardening it at rest is Phase 4.
type repoState struct {
	repoID       string
	provider     string // matched local provider name, "" when unmatched
	canonicalURL string // validated, re-serialized origin URL the sync dials, "" when unmatched
	expiresAt    string // push_token_expires_at (RFC3339), "" when none
	matched      bool   // origin_url binds to a local provider (resolved once, at applyConfig)

	replicaURL string // validated, re-serialized https replica URL, "" when unmatched or unusable
	pushToken  Secret // platform push token for replicaURL; empty exactly when replicaURL is
	// result is the recorded outcome of the last sync attempt, or the reason the configuration is
	// unusable. A zero State means no outcome yet, which is reported as pending.
	result  syncResult
	syncing bool // a sync is queued or running
}

// errInvalidReplica is the detail reported for a matched repository whose replica URL or push token
// is unusable. It is deliberately generic: the status must never echo the platform-supplied URL.
const errInvalidReplica = "invalid replica configuration"

// validReplicaURL validates a platform-supplied replica push URL and returns its re-serialized
// canonical form. The push token travels to this URL, so it must be https; it gets the same host
// validation as an origin so the host that is validated is the host that is dialled.
func validReplicaURL(raw string) (string, bool) {
	_, full, err := canonicalRemoteURL(raw)
	if err != nil || !strings.HasPrefix(full, "https://") {
		return "", false
	}
	return full, true
}

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
	return a.matched == b.matched && a.provider == b.provider &&
		a.canonicalURL == b.canonicalURL && a.replicaURL == b.replicaURL
}

// applyConfig validates an inbound git_sync_config and, only if the whole message is acceptable,
// swaps in the new desired set and config version. A rejected message leaves the previous
// configuration untouched and does not advance the version. It returns whether the config was
// accepted (and a status should follow). It never logs a credential or push token.
func (c *Client) applyConfig(raw json.RawMessage) bool {
	var msg proto.GitSyncConfigMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		slog.Warn("malformed git_sync_config message", "err", err)
		return false
	}
	if msg.Repositories == nil {
		slog.Warn("git_sync_config has no repositories field; leaving configuration unchanged")
		return false
	}
	if len(msg.Repositories) > maxConfigRepos {
		slog.Warn("git_sync_config exceeds the repository cap; ignoring", "count", len(msg.Repositories), "cap", maxConfigRepos)
		return false
	}
	if msg.ConfigVersion != nil && (len(msg.ConfigVersion) > maxConfigVersionSize || !isScalarJSON(msg.ConfigVersion)) {
		slog.Warn("git_sync_config has an invalid config_version; leaving configuration unchanged")
		return false
	}
	limits, err := ResolveLimits(msg.Schedule, msg.Limits)
	if err != nil {
		// The error names the offending field and carries no wire value, so it is safe to log.
		slog.Warn("git_sync_config has a schedule or limit out of range; leaving configuration unchanged", "err", err)
		return false
	}
	next := make(map[string]repoState, len(msg.Repositories))
	for _, r := range msg.Repositories {
		if !validRepoID(r.RepoID) {
			// A value outside the contract charset (or a "." / ".." traversal component) is rejected
			// here rather than carried into logs and statuses. Not logged verbatim: a hostile id
			// could carry log-injection bytes.
			slog.Warn("git_sync_config has an entry with an invalid repo_id; leaving configuration unchanged")
			return false
		}
		if _, dup := next[r.RepoID]; dup {
			slog.Warn("git_sync_config has a duplicate repo_id; leaving configuration unchanged", "repo_id", r.RepoID)
			return false
		}
		// Resolve the host-match once here: the local provider set is fixed for the daemon's life
		// (loaded at startup, no live reload), so the result is stable until the next config and
		// need not be recomputed on every status. The origin is parsed once: its canonical host key
		// selects the provider and its canonical URL is retained for the sync to dial.
		var provider, canonicalURL string
		matched := false
		if c.local != nil {
			if hostKey, fullURL, err := canonicalRemoteURL(r.OriginURL); err == nil {
				if name, _, ok := c.local.matchByHostKey(hostKey); ok {
					provider, canonicalURL, matched = name, fullURL, true
				}
			}
		}
		st := repoState{repoID: r.RepoID, provider: provider, canonicalURL: canonicalURL, expiresAt: r.PushTokenExpiresAt, matched: matched}
		if matched {
			// A bad replica fails this one repository, not the whole message: the others are
			// independent and halting them all would be disproportionate.
			if replicaURL, ok := validReplicaURL(r.ReplicaPushURL); ok && validPushToken(r.PushToken) {
				st.replicaURL, st.pushToken = replicaURL, r.PushToken
			} else {
				st.result = syncResult{State: StateError, ErrorType: ErrUnclassified, ErrorDetail: errInvalidReplica}
			}
		}
		next[r.RepoID] = st
	}
	if msg.ConfigVersion == nil {
		slog.Warn("git_sync_config has no config_version; status will report null until the platform includes one")
	}
	c.mu.Lock()
	// Carry a prior result and in-flight marker across a configuration that leaves a repository's
	// sync target unchanged (the platform re-sends config on every token refresh), so a healthy
	// repository is not reset to pending. A changed target starts over.
	for id, st := range next {
		if old, ok := c.repos[id]; ok && sameSyncIdentity(old, st) {
			if st.result.State == "" {
				st.result = old.result
			}
			st.syncing = old.syncing
			next[id] = st
		}
	}
	c.repos = next
	c.configVersion = cloneRaw(msg.ConfigVersion)
	c.configAccepted = true
	// The configuration is a full replace, so limits it leaves out revert to their defaults.
	c.limits.Store(&limits)
	c.mu.Unlock()
	slog.Info("git_sync_config accepted", "repo_count", len(next))
	return true
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
