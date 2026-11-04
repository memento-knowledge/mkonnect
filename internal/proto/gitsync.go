package proto

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// This file defines the wire messages for the git-sync connector kind. The shapes implement the
// confirmed git-sync protocol contract (see the connector's tracking issue), with a few fields
// still marked INFERRED where the connector models something the contract implies but does not
// pin exactly; the golden fixtures exist so the finalised shapes can be diffed against these.
//
// Direction mirrors the API connector's message set:
//   - GitSyncConfigMsg  platform -> connector : the desired set of repositories, with identity
//   - GitSyncStatusMsg  connector -> platform : connector-level envelope + per-repository state
//   - GitSyncNowMsg     platform -> connector : request an immediate sync
//
// The connector kind is never carried on the wire; the gateway assigns it server-side at
// enrollment, so no message here names it.

// Secret is a string whose value is redacted on every output path (fmt verbs, slog, and JSON
// marshalling) and revealed only via Reveal. It is used for inbound credential material — the
// platform push token — so an accidental log or re-marshal of a message cannot leak it. The real
// value is still read from the wire through UnmarshalText.
type Secret string

const secretRedaction = "<redacted>"

// Format handles every fmt verb so none can print the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, secretRedaction) }

// LogValue redacts for slog. The signature must be LogValue() slog.Value to satisfy slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(secretRedaction) }

// MarshalText redacts for encoding/json and any text encoder. These messages are inbound only, so
// the connector never needs to re-marshal a real push token onto the wire.
func (Secret) MarshalText() ([]byte, error) { return []byte(secretRedaction), nil }

// UnmarshalText reads the raw value from the wire.
func (s *Secret) UnmarshalText(b []byte) error { *s = Secret(b); return nil }

// Reveal returns the underlying value. It is the only path that does.
func (s Secret) Reveal() string { return string(s) }

var (
	_ slog.LogValuer = Secret("")
	_ fmt.Formatter  = Secret("")
)

// GitSyncConfigMsg is sent by the platform to declare the full desired set of repositories to
// replicate. The platform supplies each repository's identity and push material; the connector
// supplies the origin credential locally and binds the two by host.
type GitSyncConfigMsg struct {
	Type string `json:"type"` // "git_sync_config"
	// ConfigVersion identifies this configuration; the connector echoes the last one it accepted
	// in git_sync_status so the platform can detect a connector running stale config. It is treated
	// opaquely (any JSON scalar). INFERRED: the contract requires the echo but does not pin where
	// the version is carried; a top-level field is the connector's assumption.
	ConfigVersion json.RawMessage `json:"config_version"`
	// Repositories is the full desired set on each push. A nil slice (the field absent on the wire)
	// is rejected by the connector rather than treated as "sync nothing".
	Repositories []GitSyncRepoConfig `json:"repositories"`
	// Schedule tunes how often the connector re-syncs each repository. INFERRED: the field name and
	// shape are the connector's guess. Absent means "use the built-in default".
	Schedule *GitSyncSchedule `json:"schedule,omitempty"`
	// Limits bounds the resources a sync may use. INFERRED: the field name and shape are the
	// connector's guess. Absent means "use the built-in defaults".
	Limits *GitSyncLimits `json:"limits,omitempty"`
}

// GitSyncSchedule is the platform-supplied sync cadence within a GitSyncConfigMsg. INFERRED.
type GitSyncSchedule struct {
	// IntervalSeconds is the delay between periodic re-syncs of each repository, in seconds. Nil
	// (absent) means the built-in default; the connector accepts 60 through 86400.
	IntervalSeconds *int `json:"interval_seconds,omitempty"`
}

// GitSyncLimits is the platform-supplied set of resource bounds within a GitSyncConfigMsg. INFERRED:
// every name below is the connector's guess. Each field is a pointer so that nil (absent) can mean
// "use the built-in default", which is distinct from an explicit 0 where 0 is itself meaningful
// (for example "unbatched" or "no ceiling"). The connector rejects the whole configuration, rather
// than clamping, if any supplied value is out of range. The accepted ranges quoted below are
// enforced, and defined authoritatively, by gitsync.ResolveLimits.
type GitSyncLimits struct {
	// MaxConcurrency is how many repositories sync at once; accepted 1 through 32.
	MaxConcurrency *int `json:"max_concurrency,omitempty"`
	// PushBatchRefs is the maximum number of refs sent per push, so a very large mirror is pushed in
	// batches; 0 means unbatched, otherwise accepted 100 through 100000.
	PushBatchRefs *int `json:"push_batch_refs,omitempty"`
	// PackThreads is the thread count handed to git when packing; 0 means git's own default,
	// otherwise accepted up to 64.
	PackThreads *int `json:"pack_threads,omitempty"`
	// PackWindowMemoryMB is the per-thread delta-window memory bound handed to git when packing, in
	// MiB; 0 means git's own default, otherwise accepted 16 through 65536.
	PackWindowMemoryMB *int `json:"pack_window_memory_mb,omitempty"`
	// MaxRepositoryMB is the on-disk size ceiling for one mirrored repository, in MiB; 0 means no
	// ceiling, otherwise accepted 1 through 10485760.
	MaxRepositoryMB *int `json:"max_repository_mb,omitempty"`
	// PushTimeoutSeconds bounds a single push, in seconds; accepted 60 through 86400.
	PushTimeoutSeconds *int `json:"push_timeout_seconds,omitempty"`
}

// GitSyncRepoConfig is one repository entry within a GitSyncConfigMsg.
type GitSyncRepoConfig struct {
	// RepoID is the stable replica repository name that correlates a repository across the platform
	// config, the connector's status report, and an immediate-sync request.
	RepoID string `json:"repo_id"`
	// OriginURL is the repository's full origin URL (the repository identity, from the project
	// configuration). The connector uses the local credential against it only if its scheme, host
	// and port match a locally configured provider.
	OriginURL string `json:"origin_url"`
	// ReplicaPushURL is where the connector pushes the mirror. It must be an https URL on exactly the
	// host and port the connector was pinned to at install (GIT_SYNC_REPLICA_HOST), because the push
	// token travels to it: a URL naming any other host rejects the whole configuration, and with no
	// pin every configuration is rejected. A missing URL, or one that names the pinned host but carries
	// userinfo, a query, a fragment or whitespace, fails only that repository.
	ReplicaPushURL string `json:"replica_push_url"`
	// PushToken authenticates the push to the replica, sent as an Authorization Bearer token
	// (INFERRED: the contract names the token but not the scheme). It is a Secret so it cannot leak
	// through a log or re-marshal; the connector keeps it in memory only, and only for a repository
	// it can sync.
	PushToken Secret `json:"push_token"`
	// PushTokenExpiresAt is the push token's expiry (RFC3339). The connector reports the earliest it
	// holds so the platform can refresh before expiry.
	PushTokenExpiresAt string `json:"push_token_expires_at"`
	// Branch is the project's configured working branch. It is informational only and is NOT a ref
	// filter: the connector replicates all branches and tags.
	Branch string `json:"branch,omitempty"`
}

// GitSyncStatusMsg is emitted by the connector to report its state. Besides per-repository state it
// carries a connector-level envelope the platform uses for ordering and freshness; the platform,
// not the connector, defines any ordering rule over (boot_id, sequence).
type GitSyncStatusMsg struct {
	Type string `json:"type"` // "git_sync_status"
	// ConfigVersion is the last accepted config version, echoed back; present as null when none has
	// been accepted (deliberately not omitempty).
	ConfigVersion json.RawMessage `json:"config_version"`
	// EarliestPushTokenExpiresAt is the earliest push-token expiry the connector holds (RFC3339 UTC),
	// present as null when it holds none.
	EarliestPushTokenExpiresAt *string `json:"earliest_push_token_expires_at"`
	// SentAt is when this status was built (RFC3339 UTC, sub-second); for display and to keep
	// otherwise-identical bodies distinct.
	SentAt string `json:"sent_at"`
	// Sequence is monotonic within one process run.
	Sequence uint64 `json:"sequence"`
	// BootID is a random id created at each process start, so a sequence that restarts at a lower
	// value after a crash is not mistaken for an older status.
	BootID string `json:"boot_id"`
	// OriginCheck reports pre-configuration origin reachability at the connector level; present only
	// before any configuration has been accepted.
	OriginCheck *OriginCheck `json:"origin_check,omitempty"`
	// Repositories is the per-repository state; always a (possibly empty) array, never null.
	Repositories []GitSyncRepoStatus `json:"repositories"`
	// ConfigError reports that the most recent git_sync_config was rejected as a whole, naming the
	// version that was refused and why; omitted when the last configuration was accepted or none has
	// arrived. INFERRED: the connector's guess at how a rejection is surfaced.
	ConfigError *GitSyncConfigError `json:"config_error,omitempty"`
	// Suspended, when non-empty, says the connector has stopped syncing for a connector-wide reason
	// rather than a per-repository one; omitted while running. The only value is
	// GitSyncSuspendedCriticalUpdate. INFERRED: the connector's guess.
	Suspended string `json:"suspended,omitempty"`
}

// Wire values for GitSyncConfigError.Reason. INFERRED: the platform may rename them, so they are
// pinned by a test and defined only here.
const (
	// GitSyncConfigErrTooManyRepositories: the configuration listed more repositories than the
	// connector accepts.
	GitSyncConfigErrTooManyRepositories = "too_many_repositories"
	// GitSyncConfigErrInvalidRepoID: a repo_id was empty or not a valid repository name.
	GitSyncConfigErrInvalidRepoID = "invalid_repo_id"
	// GitSyncConfigErrDuplicateRepoID: two entries carried the same repo_id.
	GitSyncConfigErrDuplicateRepoID = "duplicate_repo_id"
	// GitSyncConfigErrInvalidConfigVersion: config_version was not a JSON scalar or exceeded the
	// connector's size cap.
	GitSyncConfigErrInvalidConfigVersion = "invalid_config_version"
	// GitSyncConfigErrLimitsOutOfRange: a schedule or limits value was outside its accepted range.
	GitSyncConfigErrLimitsOutOfRange = "limits_out_of_range"
	// GitSyncConfigErrMalformedMessage: the message could not be decoded, or a required field was
	// missing or of the wrong type.
	GitSyncConfigErrMalformedMessage = "malformed_message"
	// GitSyncConfigErrReplicaHostMismatch: a repository's replica_push_url names a host or port other
	// than the replica host pinned at install (GIT_SYNC_REPLICA_HOST), a scheme other than https, or
	// any non-empty value that cannot be shown to name the pinned host (does not parse, no host,
	// non-ASCII or percent-encoded host). The match is exact: no wildcards, subdomains or suffixes. One such repository rejects the whole
	// configuration, whether or not its origin matches a local provider; the previous configuration
	// stays in effect and nothing from the rejected one is synced.
	GitSyncConfigErrReplicaHostMismatch = "replica_host_mismatch"
	// GitSyncConfigErrReplicaHostNotPinned: no valid replica host is pinned at install
	// (GIT_SYNC_REPLICA_HOST is absent, empty or malformed). The connector fails closed: it rejects
	// every configuration with this reason and never pushes, while still connecting, reporting
	// status and heartbeating. It is a separate value from a mismatch so an operator can tell an
	// incomplete install from a bad URL.
	GitSyncConfigErrReplicaHostNotPinned = "replica_host_not_pinned"
)

// GitSyncSuspendedCriticalUpdate is the GitSyncStatusMsg.Suspended value meaning the connector has
// stopped syncing until it is upgraded. INFERRED.
const GitSyncSuspendedCriticalUpdate = "critical_update_required"

// GitSyncConfigError describes a whole-configuration rejection. INFERRED.
type GitSyncConfigError struct {
	// ConfigVersion is the version of the rejected configuration, echoed opaquely (any JSON scalar);
	// present as null when the rejected message carried no usable version (deliberately not
	// omitempty, as on GitSyncStatusMsg).
	ConfigVersion json.RawMessage `json:"config_version"`
	// Reason is one of the GitSyncConfigErr* values. It never carries a value taken from the
	// rejected message.
	Reason string `json:"reason"`
}

// OriginCheck is the connector-level reachability result reported before any repository is
// configured, so the portal can show "origin reachable" / "access denied" during setup.
type OriginCheck struct {
	Result    string `json:"result"`     // "reachable" | "access_denied" | "unreachable"
	CheckedAt string `json:"checked_at"` // RFC3339
}

// GitSyncRepoStatus is the connector's report for one repository.
type GitSyncRepoStatus struct {
	// RepoID matches the corresponding GitSyncRepoConfig.RepoID.
	RepoID string `json:"repo_id"`
	// State is the coarse lifecycle state: "pending" | "syncing" | "ok" | "error".
	State string `json:"state"`
	// OriginDefaultBranch is the repository's default branch at the origin, omitted while unknown.
	OriginDefaultBranch string `json:"origin_default_branch,omitempty"`
	// LastAttemptAt is when the most recent sync attempt started (RFC3339 UTC); omitted when the
	// repository has never been attempted. INFERRED.
	LastAttemptAt *string `json:"last_attempt_at,omitempty"`
	// LastSuccessAt is when the most recent successful sync finished (RFC3339 UTC). INFERRED.
	//
	// The intended wire form is present as null until the first success, so the platform can tell
	// "never succeeded" from "field not supported", the same convention as ConfigVersion. It is
	// omitempty for now only because the connector does not yet record success times: emitting null
	// would wrongly tell the platform that healthy repositories have never succeeded. The change
	// that starts populating it must drop omitempty here and update the status goldens and
	// TestGitSyncNewStatusFieldPresence.
	LastSuccessAt *string `json:"last_success_at,omitempty"`
	// DurationMS is how long the most recently FINISHED attempt took, in milliseconds; omitted until
	// an attempt has finished. While a re-sync is in flight, LastAttemptAt names that attempt but
	// this and the two fields below still describe the previous finished one. INFERRED.
	DurationMS *int64 `json:"duration_ms,omitempty"`
	// RefsPushed is how many refs the most recently finished attempt pushed; omitted until an
	// attempt has finished. Zero is a valid value (nothing changed), which is why this is a pointer.
	// INFERRED.
	RefsPushed *int `json:"refs_pushed,omitempty"`
	// NewestOriginCommitAt is the committer time of the newest commit seen at the origin by the
	// most recently finished attempt (RFC3339 UTC); omitted while unknown. INFERRED.
	NewestOriginCommitAt *string `json:"newest_origin_commit_at,omitempty"`
	// ErrorType is a typed failure code when State is "error"; omitted otherwise.
	ErrorType string `json:"error_type,omitempty"`
	// ErrorDetail is a short, sanitised detail (never a URL, host, or path). It carries the raw
	// detail for the "unclassified" error type; omitted when there is no error.
	ErrorDetail string `json:"error_detail,omitempty"`
	// Warnings are non-fatal codes (for example "lfs_not_replicated") a healthy repository can carry.
	Warnings []string `json:"warnings,omitempty"`
}

// GitSyncNowMsg is sent by the platform to request an immediate sync. An empty repo_id is
// provisionally interpreted as "every syncable repository", and the platform contract should prefer
// an explicit all-repositories signal. The connector folds triggers for a repository that is already
// syncing into a single trailing rerun, but does not otherwise rate-limit them.
type GitSyncNowMsg struct {
	Type   string `json:"type"` // "git_sync_now"
	RepoID string `json:"repo_id,omitempty"`
}
