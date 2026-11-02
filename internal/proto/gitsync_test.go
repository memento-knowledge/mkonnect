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
			{RepoID: "repo-c", State: "ok", Warnings: []string{"lfs_not_replicated"}},
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
