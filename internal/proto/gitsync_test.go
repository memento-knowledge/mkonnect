package proto_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// The git-sync golden fixtures in testdata/ are hand-authored, NOT generated from the structs
// under test, and there is deliberately no -update flag: a golden written by the code it checks
// only proves the code equals itself. Each test instead builds a struct value independently,
// marshals it, and compares the resulting JSON object to the hand-authored golden object
// (key set and values), while a second direction unmarshals the golden into the struct. A json
// tag typo makes the two objects diverge and fails the test.
//
// These fixtures pin PROVISIONAL shapes (the git-sync protocol is not yet finalised). They
// prove the connector marshals/unmarshals the shapes this repo currently expects; they do NOT
// prove compatibility with any server, because no authoritative counterpart type exists to
// decode against yet — unlike the handshake messages, whose tests decode into an independent
// copy of the real gateway shape.

// readGolden reads the raw bytes of testdata/<name>.
func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

// readGoldenObject reads testdata/<name> and decodes it into a generic JSON object.
func readGoldenObject(t *testing.T, name string) map[string]any {
	t.Helper()
	return toObject(t, readGolden(t, name))
}

// toObject decodes JSON bytes into a generic object for structural comparison.
func toObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode object: %v\nbytes: %s", err, b)
	}
	return m
}

// marshalToObject marshals v and decodes the result into a generic object, so a comparison
// against a golden object is by key set and value, independent of field or whitespace order.
func marshalToObject(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return toObject(t, b)
}

func TestGitSyncConfigMsgGolden(t *testing.T) {
	// Built independently of the golden; marshalling this must reproduce the golden object.
	msg := proto.GitSyncConfigMsg{
		Type: "git_sync_config",
		Repositories: []proto.GitSyncRepoConfig{
			{RepoKey: "repo-a", Branches: []string{"main", "release/1.x"}},
			{RepoKey: "repo-b"},
		},
	}
	golden := readGoldenObject(t, "git_sync_config.json")
	if got := marshalToObject(t, msg); !reflect.DeepEqual(got, golden) {
		t.Errorf("marshalled object does not match golden\n got: %v\nwant: %v", got, golden)
	}

	// Reverse direction: the golden bytes decode into the struct with the expected values.
	var decoded proto.GitSyncConfigMsg
	if err := json.Unmarshal(readGolden(t, "git_sync_config.json"), &decoded); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	if decoded.Type != "git_sync_config" {
		t.Errorf("type = %q, want git_sync_config", decoded.Type)
	}
	if len(decoded.Repositories) != 2 {
		t.Fatalf("repositories len = %d, want 2", len(decoded.Repositories))
	}
	if decoded.Repositories[0].RepoKey != "repo-a" || len(decoded.Repositories[0].Branches) != 2 {
		t.Errorf("repo[0] = %+v, want repo-a with 2 branches", decoded.Repositories[0])
	}
	// repo-b omits branches: it must decode to an empty branch list, not fail.
	if decoded.Repositories[1].RepoKey != "repo-b" || len(decoded.Repositories[1].Branches) != 0 {
		t.Errorf("repo[1] = %+v, want repo-b with no branches", decoded.Repositories[1])
	}
}

func TestGitSyncStatusMsgGolden(t *testing.T) {
	msg := proto.GitSyncStatusMsg{
		Type: "git_sync_status",
		Repositories: []proto.GitSyncRepoStatus{
			{RepoKey: "repo-a", State: "pending", OriginDefaultBranch: "main"},
			{RepoKey: "repo-b", State: "error", ErrorDetail: "authentication failed"},
		},
	}
	golden := readGoldenObject(t, "git_sync_status.json")
	if got := marshalToObject(t, msg); !reflect.DeepEqual(got, golden) {
		t.Errorf("marshalled object does not match golden\n got: %v\nwant: %v", got, golden)
	}

	// Reverse direction: the golden decodes into the struct, and origin_default_branch (whose
	// presence in git_sync_status is required) round-trips under its exact wire key.
	var decoded proto.GitSyncStatusMsg
	if err := json.Unmarshal(readGolden(t, "git_sync_status.json"), &decoded); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	if len(decoded.Repositories) == 0 || decoded.Repositories[0].OriginDefaultBranch != "main" {
		t.Errorf("origin_default_branch did not decode from the golden: %+v", decoded.Repositories)
	}
}

func TestGitSyncNowMsgGolden(t *testing.T) {
	msg := proto.GitSyncNowMsg{Type: "git_sync_now", RepoKey: "repo-a"}
	golden := readGoldenObject(t, "git_sync_now.json")
	if got := marshalToObject(t, msg); !reflect.DeepEqual(got, golden) {
		t.Errorf("marshalled object does not match golden\n got: %v\nwant: %v", got, golden)
	}

	var decoded proto.GitSyncNowMsg
	if err := json.Unmarshal(readGolden(t, "git_sync_now.json"), &decoded); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	if decoded.Type != "git_sync_now" || decoded.RepoKey != "repo-a" {
		t.Errorf("decoded = %+v, want git_sync_now/repo-a", decoded)
	}
}

// TestGitSyncOmitEmptyFields pins which fields drop off the wire when empty, so the encoding of
// a minimal message stays stable and a future change to an omitempty tag is deliberate.
func TestGitSyncOmitEmptyFields(t *testing.T) {
	statusObj := marshalToObject(t, proto.GitSyncRepoStatus{RepoKey: "r", State: "pending"})
	for _, k := range []string{"origin_default_branch", "error_detail"} {
		if _, ok := statusObj[k]; ok {
			t.Errorf("empty GitSyncRepoStatus must omit %q, got %v", k, statusObj)
		}
	}
	if _, ok := marshalToObject(t, proto.GitSyncRepoConfig{RepoKey: "r"})["branches"]; ok {
		t.Error("empty GitSyncRepoConfig must omit branches")
	}
	if _, ok := marshalToObject(t, proto.GitSyncNowMsg{Type: "git_sync_now"})["repo_key"]; ok {
		t.Error("empty GitSyncNowMsg must omit repo_key")
	}
}

// TestGitSyncConfigAbsentRepositories checks a config frame with no repositories field decodes
// to a nil slice without error — the connector must treat it as "no repositories", not panic.
func TestGitSyncConfigAbsentRepositories(t *testing.T) {
	var msg proto.GitSyncConfigMsg
	if err := json.Unmarshal([]byte(`{"type":"git_sync_config"}`), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Repositories != nil {
		t.Errorf("repositories = %v, want nil for an absent field", msg.Repositories)
	}
}

// TestGitSyncNowAbsentRepoKey checks an absent repo_key decodes to the empty string (the
// provisional "all repositories" signal), not an error.
func TestGitSyncNowAbsentRepoKey(t *testing.T) {
	var msg proto.GitSyncNowMsg
	if err := json.Unmarshal([]byte(`{"type":"git_sync_now"}`), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.RepoKey != "" {
		t.Errorf("repo_key = %q, want empty", msg.RepoKey)
	}
}

// TestGitSyncConfigIgnoresUnknownField checks forward compatibility: an unknown field a newer
// platform might add is ignored, leaving known fields intact (mirrors the handshake tests).
func TestGitSyncConfigIgnoresUnknownField(t *testing.T) {
	raw := `{"type":"git_sync_config","repositories":[{"repo_key":"repo-a","future_field":true}]}`
	var msg proto.GitSyncConfigMsg
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msg.Repositories) != 1 || msg.Repositories[0].RepoKey != "repo-a" {
		t.Errorf("decoded = %+v, want one repo-a entry", msg.Repositories)
	}
}

// TestGitSyncConfigWrongTypeRejected documents the frame contract: repositories is typed, so a
// non-array value fails to decode the whole frame rather than being coerced (mirrors
// TestHTTPRequestMsgNumericExpiresAtRejected).
func TestGitSyncConfigWrongTypeRejected(t *testing.T) {
	raw := `{"type":"git_sync_config","repositories":"nope"}`
	var msg proto.GitSyncConfigMsg
	if err := json.Unmarshal([]byte(raw), &msg); err == nil {
		t.Fatalf("expected unmarshal error for non-array repositories, got none: %+v", msg)
	}
}
