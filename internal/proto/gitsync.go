package proto

// This file defines the wire messages for the git-sync connector kind. The shapes here are
// provisional and subject to change until the git-sync protocol is finalised: they are
// implemented ahead of the authoritative protocol definition so the connector skeleton can be
// built and exercised, and the accompanying golden fixtures exist so the finalised shapes can
// be diffed against these. Treat a field's presence here as a proposal, not a guarantee.
//
// Direction and purpose mirror the API connector's message set:
//   - GitSyncConfigMsg  platform -> connector : which repositories to sync (akin to a config push)
//   - GitSyncStatusMsg  connector -> platform : per-repository state (akin to connection_status)
//   - GitSyncNowMsg     platform -> connector : request an immediate sync (akin to status_request)
//
// As with every connector kind, the kind itself is never carried on the wire; the gateway
// derives it server-side from the registration-token claim, so no message here names it.

// GitSyncConfigMsg is sent by the platform to tell the connector which repositories to sync.
// Repositories are referenced by an opaque repo_key; this connector expects the origin URL and
// any credentials to come from its own local configuration rather than from this message, which
// mirrors how the API connector takes plugin base URLs from local config.
type GitSyncConfigMsg struct {
	Type string `json:"type"` // "git_sync_config"
	// Repositories is the full desired set on each push. A nil slice (the field absent on the
	// wire) is preserved distinct from an empty slice so a later phase can decide whether an
	// omitted field means "no change" or "sync nothing"; the skeleton treats both as "no
	// repositories requested".
	Repositories []GitSyncRepoConfig `json:"repositories"`
}

// GitSyncRepoConfig is one repository entry within a GitSyncConfigMsg.
type GitSyncRepoConfig struct {
	// RepoKey is the stable identifier that correlates a repository across the platform
	// config, the connector's local credential entry, and the status report.
	RepoKey string `json:"repo_key"`
	// Branches optionally restricts which branches to sync; an empty list is left off the wire
	// and a later phase defines its meaning (for example "all branches" versus "default only").
	Branches []string `json:"branches,omitempty"`
}

// GitSyncStatusMsg is emitted by the connector to report the state of each repository it knows
// about — the union of the repositories the platform has configured and those present in the
// connector's local configuration — so the platform can see a repository it configured even
// when the connector cannot yet act on it.
type GitSyncStatusMsg struct {
	Type         string              `json:"type"` // "git_sync_status"
	Repositories []GitSyncRepoStatus `json:"repositories"`
}

// GitSyncRepoStatus is the connector's report for one repository.
type GitSyncRepoStatus struct {
	// RepoKey matches the corresponding GitSyncRepoConfig.RepoKey.
	RepoKey string `json:"repo_key"`
	// State is a coarse lifecycle state. See the state constants in the gitsync package for the
	// values the connector emits; the full set is provisional until the protocol is finalised.
	State string `json:"state"`
	// OriginDefaultBranch is the repository's default branch at the origin (for example "main").
	// It is omitted while unknown — the connector cannot report it until it has contacted the
	// origin, which the skeleton does not yet do.
	OriginDefaultBranch string `json:"origin_default_branch,omitempty"`
	// ErrorDetail is a short, human-readable failure CLASS only — never a URL, host, path, or
	// any other value that could reveal the customer's internal topology to the platform. It is
	// omitted when there is no error. A later phase populates it via a sanitiser analogous to
	// the API connector's safeDiagnostic.
	ErrorDetail string `json:"error_detail,omitempty"`
}

// GitSyncNowMsg is sent by the platform to request an immediate sync. An empty repo_key is
// provisionally interpreted as "every configured repository"; because that is a fan-out
// trigger, a later phase must coalesce and rate-limit sync requests, and the platform side of
// this contract should prefer an explicit all-repositories signal to a dropped key.
type GitSyncNowMsg struct {
	Type    string `json:"type"` // "git_sync_now"
	RepoKey string `json:"repo_key,omitempty"`
}
