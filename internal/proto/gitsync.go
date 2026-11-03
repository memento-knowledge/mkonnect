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
	// ReplicaPushURL is where the connector pushes the mirror. It must be an https URL: the push
	// token travels to it.
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
