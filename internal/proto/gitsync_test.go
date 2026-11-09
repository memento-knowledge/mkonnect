package proto_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// The git-sync golden fixtures in testdata/ are hand-authored, NOT generated from the structs
// under test, and there is deliberately no -update flag. Each test builds a value independently (or
// decodes the golden) and compares against the hand-authored golden, so a json-tag typo diverges
// and fails. These fixtures pin the git-sync protocol shapes this connector currently expects; they
// prove the connector marshals/unmarshals them, not compatibility with any server, because no
// authoritative counterpart type exists here to decode against.

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

func readGoldenObject(t *testing.T, name string) map[string]any {
	t.Helper()
	return toObject(t, readGolden(t, name))
}

func toObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode object: %v\nbytes: %s", err, b)
	}
	return m
}

func marshalToObject(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return toObject(t, b)
}

// git_sync_config is inbound only and carries a redacting push_token, so marshalling it would emit
// "<redacted>"; the golden is therefore validated by DECODING it and checking the fields.
func TestGitSyncConfigMsgGoldenDecode(t *testing.T) {
	var msg proto.GitSyncConfigMsg
	if err := json.Unmarshal(readGolden(t, "git_sync_config.json"), &msg); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	if msg.Type != "git_sync_config" {
		t.Errorf("type = %q, want git_sync_config", msg.Type)
	}
	if string(msg.ConfigVersion) != "7" {
		t.Errorf("config_version = %s, want 7", msg.ConfigVersion)
	}
	if len(msg.Repositories) != 2 {
		t.Fatalf("repositories len = %d, want 2", len(msg.Repositories))
	}
	a := msg.Repositories[0]
	if a.RepoID != "repo-a" || a.OriginURL != "https://git.example.com/team/repo-a.git" ||
		a.ReplicaPushURL != "https://replica.example.com/mirror/repo-a.git" ||
		a.PushTokenExpiresAt != "2026-01-01T00:00:00Z" || a.Branch != "main" {
		t.Errorf("repo-a decoded unexpectedly: %+v", a)
	}
	if a.PushToken.Reveal() != "synthetic-push-token-aaaa1111" {
		t.Errorf("push_token did not decode to its real value")
	}
	if msg.Repositories[1].Branch != "" {
		t.Errorf("repo-b should have no branch, got %q", msg.Repositories[1].Branch)
	}
}

func TestGitSyncStatusMsgGolden(t *testing.T) {
	cv := json.RawMessage("7")
	exp := "2026-01-01T00:00:00Z"
	lastSuccess := "2026-01-02T03:00:00Z"
	msg := proto.GitSyncStatusMsg{
		Type:                       "git_sync_status",
		ConfigVersion:              cv,
		EarliestPushTokenExpiresAt: &exp,
		SentAt:                     "2026-01-02T03:04:05.000000000Z",
		Sequence:                   3,
		BootID:                     "boot-synthetic-0001",
		OriginCheck:                &proto.OriginCheck{Result: "reachable", CheckedAt: "2026-01-02T03:04:00.000000000Z"},
		Repositories: []proto.GitSyncRepoStatus{
			{RepoID: "repo-a", State: "pending", OriginDefaultBranch: "main"},
			{RepoID: "repo-b", State: "error", ErrorType: "origin_access_denied", ErrorDetail: "host not configured locally"},
			{RepoID: "repo-c", State: "ok", LastSuccessAt: &lastSuccess, Warnings: []string{"lfs_not_replicated"}},
		},
	}
	if got := marshalToObject(t, msg); !reflect.DeepEqual(got, readGoldenObject(t, "git_sync_status.json")) {
		t.Errorf("marshalled status does not match golden\n got: %v\nwant: %v", got, readGoldenObject(t, "git_sync_status.json"))
	}

	var decoded proto.GitSyncStatusMsg
	if err := json.Unmarshal(readGolden(t, "git_sync_status.json"), &decoded); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	if decoded.BootID != "boot-synthetic-0001" || decoded.Sequence != 3 || decoded.OriginCheck == nil ||
		decoded.OriginCheck.Result != "reachable" || decoded.Repositories[0].OriginDefaultBranch != "main" {
		t.Errorf("status golden decoded unexpectedly: %+v", decoded)
	}
}

// The pre-existing config golden carries neither schedule nor limits; it must keep decoding with both
// nil, which the connector reads as "use the built-in defaults".
func TestGitSyncConfigWithoutScheduleOrLimits(t *testing.T) {
	var msg proto.GitSyncConfigMsg
	if err := json.Unmarshal(readGolden(t, "git_sync_config.json"), &msg); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	if msg.Schedule != nil || msg.Limits != nil {
		t.Errorf("schedule/limits = %+v / %+v, want both nil when absent", msg.Schedule, msg.Limits)
	}
	if _, ok := marshalToObject(t, proto.GitSyncConfigMsg{Type: "git_sync_config"})["schedule"]; ok {
		t.Error("nil Schedule must be omitted")
	}
	if _, ok := marshalToObject(t, proto.GitSyncConfigMsg{Type: "git_sync_config"})["limits"]; ok {
		t.Error("nil Limits must be omitted")
	}
}

func TestGitSyncConfigScheduleLimitsGoldenDecode(t *testing.T) {
	var msg proto.GitSyncConfigMsg
	if err := json.Unmarshal(readGolden(t, "git_sync_config_limits.json"), &msg); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	if string(msg.ConfigVersion) != `"2026-01-02.1"` || len(msg.Repositories) != 1 {
		t.Errorf("envelope decoded unexpectedly: version=%s repos=%d", msg.ConfigVersion, len(msg.Repositories))
	}
	if msg.Schedule == nil || msg.Schedule.IntervalSeconds == nil || *msg.Schedule.IntervalSeconds != 900 {
		t.Errorf("schedule = %+v, want interval_seconds 900", msg.Schedule)
	}
	if msg.Limits == nil {
		t.Fatal("limits missing")
	}
	got := map[string]*int{
		"max_concurrency":       msg.Limits.MaxConcurrency,
		"push_batch_refs":       msg.Limits.PushBatchRefs,
		"pack_threads":          msg.Limits.PackThreads,
		"pack_window_memory_mb": msg.Limits.PackWindowMemoryMB,
		"max_repository_mb":     msg.Limits.MaxRepositoryMB,
		"push_timeout_seconds":  msg.Limits.PushTimeoutSeconds,
	}
	want := map[string]int{
		"max_concurrency": 2, "push_batch_refs": 5000, "pack_threads": 0,
		"pack_window_memory_mb": 256, "max_repository_mb": 20480, "push_timeout_seconds": 1800,
	}
	for k, w := range want {
		if got[k] == nil || *got[k] != w {
			t.Errorf("limits.%s = %v, want %d", k, got[k], w)
		}
	}
}

// TestGitSyncLimitsAbsentVersusZero pins that an absent limit is nil while an explicit 0 survives as a
// non-nil pointer, because 0 is meaningful for several limits (unbatched, no ceiling, git default).
func TestGitSyncLimitsAbsentVersusZero(t *testing.T) {
	var l proto.GitSyncLimits
	if err := json.Unmarshal([]byte(`{"pack_threads":0,"max_concurrency":3}`), &l); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if l.PackThreads == nil || *l.PackThreads != 0 {
		t.Errorf("explicit pack_threads 0 = %v, want non-nil 0", l.PackThreads)
	}
	if l.MaxConcurrency == nil || *l.MaxConcurrency != 3 {
		t.Errorf("max_concurrency = %v, want 3", l.MaxConcurrency)
	}
	if l.PushBatchRefs != nil || l.PackWindowMemoryMB != nil || l.MaxRepositoryMB != nil || l.PushTimeoutSeconds != nil {
		t.Errorf("absent limits must stay nil: %+v", l)
	}
	if got := marshalToObject(t, l); !reflect.DeepEqual(got, map[string]any{"pack_threads": float64(0), "max_concurrency": float64(3)}) {
		t.Errorf("marshalled limits = %v, want only the two supplied fields", got)
	}
}

func TestGitSyncConfigWrongTypeLimitRejected(t *testing.T) {
	for _, raw := range []string{
		`{"type":"git_sync_config","limits":{"max_concurrency":"four"}}`,
		`{"type":"git_sync_config","schedule":{"interval_seconds":1.5}}`,
		`{"type":"git_sync_config","limits":[]}`,
	} {
		var msg proto.GitSyncConfigMsg
		if err := json.Unmarshal([]byte(raw), &msg); err == nil {
			t.Errorf("expected unmarshal error for %s, got none: %+v", raw, msg)
		}
	}
}

func TestGitSyncStatusFullGolden(t *testing.T) {
	ts := func(s string) *string { return &s }
	i := func(n int) *int { return &n }
	d := int64(8250)
	d2 := int64(120)
	msg := proto.GitSyncStatusMsg{
		Type:                       "git_sync_status",
		ConfigVersion:              json.RawMessage(`"2026-01-02.1"`),
		EarliestPushTokenExpiresAt: ts("2026-01-01T00:00:00Z"),
		SentAt:                     "2026-01-02T03:04:05.000000000Z",
		Sequence:                   4,
		BootID:                     "boot-synthetic-0001",
		Repositories: []proto.GitSyncRepoStatus{
			{
				RepoID: "repo-a", State: "ok", OriginDefaultBranch: "main",
				LastAttemptAt: ts("2026-01-02T03:00:00Z"), LastSuccessAt: ts("2026-01-02T03:00:00Z"),
				DurationMS: &d, RefsPushed: i(0), NewestOriginCommitAt: ts("2026-01-01T22:15:30Z"),
			},
			{
				RepoID: "repo-b", State: "error", LastAttemptAt: ts("2026-01-02T03:01:00Z"),
				DurationMS: &d2, ErrorType: "origin_unreachable",
			},
			{RepoID: "repo-c", State: "pending"},
		},
		ConfigError: &proto.GitSyncConfigError{ConfigVersion: json.RawMessage("8"), Reason: proto.GitSyncConfigErrLimitsOutOfRange},
		Suspended:   proto.GitSyncSuspendedCriticalUpdate,
	}
	want := readGoldenObject(t, "git_sync_status_full.json")
	if got := marshalToObject(t, msg); !reflect.DeepEqual(got, want) {
		t.Errorf("marshalled status does not match golden\n got: %v\nwant: %v", got, want)
	}

	var decoded proto.GitSyncStatusMsg
	if err := json.Unmarshal(readGolden(t, "git_sync_status_full.json"), &decoded); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	a := decoded.Repositories[0]
	if a.RefsPushed == nil || *a.RefsPushed != 0 || a.DurationMS == nil || *a.DurationMS != 8250 ||
		a.LastSuccessAt == nil || *a.LastSuccessAt != "2026-01-02T03:00:00Z" ||
		a.LastAttemptAt == nil || a.NewestOriginCommitAt == nil || *a.NewestOriginCommitAt != "2026-01-01T22:15:30Z" {
		t.Errorf("repo-a decoded unexpectedly: %+v", a)
	}
	if decoded.Repositories[1].LastSuccessAt != nil || decoded.Repositories[2].LastAttemptAt != nil {
		t.Errorf("null/absent timestamps must decode to nil: %+v", decoded.Repositories)
	}
	if decoded.ConfigError == nil || string(decoded.ConfigError.ConfigVersion) != "8" ||
		decoded.ConfigError.Reason != "limits_out_of_range" || decoded.Suspended != "critical_update_required" {
		t.Errorf("config_error/suspended decoded unexpectedly: %+v / %q", decoded.ConfigError, decoded.Suspended)
	}
}

// TestGitSyncNewStatusFieldPresence pins which of the new status fields are omitted when unset and
// which are present as null: a config_error without a usable version still carries config_version
// as null. last_success_at is present as null until the first success, so the platform can tell
// "never succeeded" from "not reported"; the other new fields are omitted while unset.
func TestGitSyncNewStatusFieldPresence(t *testing.T) {
	repo := marshalToObject(t, proto.GitSyncRepoStatus{RepoID: "r", State: "pending"})
	for _, k := range []string{"last_attempt_at", "duration_ms", "refs_pushed", "newest_origin_commit_at"} {
		if _, ok := repo[k]; ok {
			t.Errorf("empty GitSyncRepoStatus must omit %q, got %v", k, repo)
		}
	}
	if v, ok := repo["last_success_at"]; !ok || v != nil {
		t.Errorf("empty GitSyncRepoStatus must carry last_success_at as null, got %v (present: %v)", v, ok)
	}
	var decoded proto.GitSyncRepoStatus
	if err := json.Unmarshal([]byte(`{"repo_id":"r","state":"pending","last_success_at":null}`), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.LastSuccessAt != nil {
		t.Errorf("last_success_at null decoded to %q, want nil", *decoded.LastSuccessAt)
	}

	msg := marshalToObject(t, proto.GitSyncStatusMsg{Type: "git_sync_status"})
	for _, k := range []string{"config_error", "suspended"} {
		if _, ok := msg[k]; ok {
			t.Errorf("empty GitSyncStatusMsg must omit %q", k)
		}
	}

	ce := marshalToObject(t, proto.GitSyncStatusMsg{
		Type: "git_sync_status", ConfigError: &proto.GitSyncConfigError{Reason: proto.GitSyncConfigErrMalformedMessage},
	})["config_error"].(map[string]any)
	if v, ok := ce["config_version"]; !ok || v != nil {
		t.Errorf("config_error.config_version should be present as null, got %v ok=%v", v, ok)
	}
	if ce["reason"] != "malformed_message" {
		t.Errorf("config_error.reason = %v, want malformed_message", ce["reason"])
	}
}

// TestGitSyncConfigErrorWireStringsPinned fails if a config_error reason or the suspended value
// drifts; these are the strings the platform matches on, so they change only deliberately.
func TestGitSyncConfigErrorWireStringsPinned(t *testing.T) {
	got := []string{
		proto.GitSyncConfigErrTooManyRepositories, proto.GitSyncConfigErrInvalidRepoID,
		proto.GitSyncConfigErrDuplicateRepoID, proto.GitSyncConfigErrInvalidConfigVersion,
		proto.GitSyncConfigErrLimitsOutOfRange, proto.GitSyncConfigErrMalformedMessage,
		proto.GitSyncConfigErrReplicaHostMismatch, proto.GitSyncConfigErrReplicaHostNotPinned,
	}
	want := []string{
		"too_many_repositories", "invalid_repo_id", "duplicate_repo_id", "invalid_config_version",
		"limits_out_of_range", "malformed_message", "replica_host_mismatch", "replica_host_not_pinned",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("config_error reason wire strings drifted:\n got %v\nwant %v", got, want)
	}
	if proto.GitSyncSuspendedCriticalUpdate != "critical_update_required" {
		t.Errorf("suspended wire string drifted: %q", proto.GitSyncSuspendedCriticalUpdate)
	}
}

// TestGitSyncWarningWireStringsPinned fails if a warning code drifts; the platform matches on it.
func TestGitSyncWarningWireStringsPinned(t *testing.T) {
	if proto.GitSyncWarnLFSNotReplicated != "lfs_not_replicated" {
		t.Errorf("warning wire string drifted: %q", proto.GitSyncWarnLFSNotReplicated)
	}
}

func TestGitSyncNowMsgGolden(t *testing.T) {
	msg := proto.GitSyncNowMsg{Type: "git_sync_now", RepoID: "repo-a"}
	if got := marshalToObject(t, msg); !reflect.DeepEqual(got, readGoldenObject(t, "git_sync_now.json")) {
		t.Errorf("marshalled now does not match golden\n got: %v", got)
	}
	var decoded proto.GitSyncNowMsg
	if err := json.Unmarshal(readGolden(t, "git_sync_now.json"), &decoded); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	if decoded.Type != "git_sync_now" || decoded.RepoID != "repo-a" {
		t.Errorf("decoded = %+v, want git_sync_now/repo-a", decoded)
	}
}

// TestGitSyncStatusPresentAsNull pins that config_version and earliest_push_token_expires_at are
// PRESENT as JSON null when the connector holds none (the platform distinguishes "null" from
// "absent"), while origin_check is omitted when nil.
func TestGitSyncStatusPresentAsNull(t *testing.T) {
	msg := proto.GitSyncStatusMsg{Type: "git_sync_status", SentAt: "2026-01-02T03:04:05.000000000Z", Sequence: 1, BootID: "b"}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"config_version", "earliest_push_token_expires_at"} {
		v, ok := raw[k]
		if !ok || string(v) != "null" {
			t.Errorf("%s should be present as null, got %q ok=%v", k, v, ok)
		}
	}
	if _, ok := raw["origin_check"]; ok {
		t.Error("origin_check should be omitted when nil")
	}
	if _, ok := raw["repositories"]; !ok {
		t.Error("repositories should always be present")
	}
}

func TestGitSyncOmitEmptyFields(t *testing.T) {
	statusObj := marshalToObject(t, proto.GitSyncRepoStatus{RepoID: "r", State: "pending"})
	for _, k := range []string{"origin_default_branch", "error_type", "error_detail", "warnings"} {
		if _, ok := statusObj[k]; ok {
			t.Errorf("empty GitSyncRepoStatus must omit %q, got %v", k, statusObj)
		}
	}
	if _, ok := marshalToObject(t, proto.GitSyncRepoConfig{RepoID: "r"})["branch"]; ok {
		t.Error("empty GitSyncRepoConfig must omit branch")
	}
	if _, ok := marshalToObject(t, proto.GitSyncNowMsg{Type: "git_sync_now"})["repo_id"]; ok {
		t.Error("empty GitSyncNowMsg must omit repo_id")
	}
}

func TestGitSyncConfigAbsentRepositories(t *testing.T) {
	var msg proto.GitSyncConfigMsg
	if err := json.Unmarshal([]byte(`{"type":"git_sync_config"}`), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Repositories != nil {
		t.Errorf("repositories = %v, want nil for an absent field", msg.Repositories)
	}
}

func TestGitSyncNowAbsentRepoID(t *testing.T) {
	var msg proto.GitSyncNowMsg
	if err := json.Unmarshal([]byte(`{"type":"git_sync_now"}`), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.RepoID != "" {
		t.Errorf("repo_id = %q, want empty", msg.RepoID)
	}
}

func TestGitSyncConfigIgnoresUnknownField(t *testing.T) {
	raw := `{"type":"git_sync_config","repositories":[{"repo_id":"repo-a","future_field":true}]}`
	var msg proto.GitSyncConfigMsg
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msg.Repositories) != 1 || msg.Repositories[0].RepoID != "repo-a" {
		t.Errorf("decoded = %+v, want one repo-a entry", msg.Repositories)
	}
}

func TestGitSyncConfigWrongTypeRejected(t *testing.T) {
	raw := `{"type":"git_sync_config","repositories":"nope"}`
	var msg proto.GitSyncConfigMsg
	if err := json.Unmarshal([]byte(raw), &msg); err == nil {
		t.Fatalf("expected unmarshal error for non-array repositories, got none: %+v", msg)
	}
}

// TestProtoSecretRedaction verifies the push token never reaches fmt, slog, or json output, yet
// decodes and Reveals correctly.
func TestProtoSecretRedaction(t *testing.T) {
	secret := "do-not-print-this-push-token"
	cfg := proto.GitSyncRepoConfig{RepoID: "r", PushToken: proto.Secret(secret)}
	for _, rendered := range []string{
		fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), fmt.Sprintf("%s", cfg.PushToken),
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("fmt rendering leaked the push token: %q", rendered)
		}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Errorf("json.Marshal leaked the push token: %s", b)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("t", "cfg", cfg, "token", cfg.PushToken)
	if strings.Contains(buf.String(), secret) {
		t.Errorf("slog leaked the push token: %s", buf.String())
	}
	var decoded proto.GitSyncRepoConfig
	if err := json.Unmarshal([]byte(`{"repo_id":"r","push_token":"`+secret+`"}`), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.PushToken.Reveal() != secret {
		t.Errorf("push token did not decode to its real value")
	}
}

// TestOriginCheckWireShape pins origin_check for a consumer written before scope and error_type
// existed: result is one of exactly three values, a check without the optional fields marshals to
// exactly today's two keys, and a consumer that decodes only those two reads the same values from a
// check that carries the new ones.
func TestOriginCheckWireShape(t *testing.T) {
	plain := marshalToObject(t, proto.OriginCheck{Result: "reachable", CheckedAt: "2026-01-02T03:04:05.000000000Z"})
	if len(plain) != 2 || plain["result"] != "reachable" || plain["checked_at"] != "2026-01-02T03:04:05.000000000Z" {
		t.Errorf("a check without scope and error_type = %v, want exactly result and checked_at", plain)
	}

	rich := proto.OriginCheck{Result: "access_denied", CheckedAt: "2026-01-02T03:04:05.000000000Z",
		Scope: proto.OriginCheckScopeRepository, ErrorType: "origin_access_denied"}
	b, err := json.Marshal(rich)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var old struct {
		Result    string `json:"result"`
		CheckedAt string `json:"checked_at"`
	}
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatalf("decode as the old shape: %v", err)
	}
	if old.Result != "access_denied" || old.CheckedAt != rich.CheckedAt {
		t.Errorf("an old consumer reads %+v from %s", old, b)
	}
	obj := marshalToObject(t, rich)
	if obj["scope"] != "repository" || obj["error_type"] != "origin_access_denied" {
		t.Errorf("scope and error_type = %v", obj)
	}
	if proto.OriginCheckScopeHost != "host" || proto.OriginCheckScopeRepository != "repository" {
		t.Error("origin_check scope wire strings drifted")
	}
}
