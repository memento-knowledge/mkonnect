package cli_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/gitsync/cli"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// statusNow is the fixed clock the relative timestamps are measured against.
var statusNow = time.Date(2030, 1, 15, 12, 0, 0, 0, time.UTC)

// target is a syntactically valid sync-target fingerprint (64 lowercase hex characters). The CLI must
// never print it.
var target = strings.Repeat("ab", 32)

// statusEnv isolates a status run: a fresh provider config (no providers), a fresh mirrors root, a
// pinned synthetic replica host, and the fixed clock. It returns the mirrors root.
func statusEnv(t *testing.T) string {
	t.Helper()
	withConfig(t)
	root := filepath.Join(t.TempDir(), "mirrors")
	t.Setenv("GIT_SYNC_MIRRORS_DIR", root)
	t.Setenv(gitsync.ReplicaHostEnv, "replica.example.net")
	t.Cleanup(cli.SetNowForTest(statusNow))
	return root
}

func writeRunState(t *testing.T, root, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "run-state.json")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// goldenCompare compares got with testdata/<name>, or rewrites it under -update.
func goldenCompare(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// stateFile renders a run-state file from per-repository JSON bodies.
func stateFile(repos map[string]string) string {
	var b strings.Builder
	b.WriteString(`{"version":1,"repositories":{`)
	first := true
	for id, body := range repos {
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(`"` + id + `":` + body)
	}
	b.WriteString(`}}`)
	return b.String()
}

func record(fields string) string {
	return `{"target":"` + target + `"` + fields + `}`
}

// mixedState covers a repository that is up to date, one whose latest attempt failed after an
// earlier success, one that has never succeeded, and one with an empty record.
var mixedState = stateFile(map[string]string{
	"repo-alpha": record(`,"last_attempt_at":"2030-01-15T11:59:30Z","last_success_at":"2030-01-15T11:59:31Z","duration_ms":1500,"refs_pushed":3,"newest_origin_commit_at":"2030-01-15T11:00:00Z","origin_default_branch":"main"`),
	"repo-beta":  record(`,"last_attempt_at":"2030-01-15T11:00:00Z","last_success_at":"2030-01-10T08:30:00Z","duration_ms":800,"refs_pushed":0,"origin_default_branch":"trunk"`),
	"repo-gamma": record(`,"last_attempt_at":"2030-01-15T11:58:00Z","duration_ms":120`),
	"repo-delta": record(``),
})

func TestStatusRunStateGolden(t *testing.T) {
	tests := []struct {
		name    string
		content *string // nil: no file
		mode    os.FileMode
		args    []string
	}{
		{"missing", nil, 0, []string{"status"}},
		{"missing_json", nil, 0, []string{"status", "--json"}},
		{"empty", ptr(""), 0o600, []string{"status"}},
		{"empty_json", ptr(""), 0o600, []string{"status", "--json"}},
		{"corrupt", ptr("{not json"), 0o600, []string{"status"}},
		{"corrupt_json", ptr("{not json"), 0o600, []string{"status", "--json"}},
		{"unsupported_version", ptr(`{"version":2,"repositories":{}}`), 0o600, []string{"status"}},
		{"world_writable", ptr(mixedState), 0o666, []string{"status"}},
		{"world_writable_json", ptr(mixedState), 0o666, []string{"status", "--json"}},
		{"no_repositories", ptr(`{"version":1,"repositories":{}}`), 0o600, []string{"status"}},
		{"no_repositories_json", ptr(`{"version":1,"repositories":{}}`), 0o600, []string{"status", "--json"}},
		{"mixed", ptr(mixedState), 0o600, []string{"status"}},
		{"mixed_absolute", ptr(mixedState), 0o600, []string{"status", "--absolute"}},
		{"mixed_json", ptr(mixedState), 0o640, []string{"status", "--json"}},
		{"relative_edges", ptr(stateFile(map[string]string{
			"repo-now":    record(`,"last_attempt_at":"2030-01-15T12:00:00Z"`),
			"repo-secs":   record(`,"last_attempt_at":"2030-01-15T11:59:15Z"`),
			"repo-hours":  record(`,"last_attempt_at":"2030-01-15T07:00:00Z"`),
			"repo-days":   record(`,"last_attempt_at":"2030-01-01T12:00:00Z"`),
			"repo-future": record(`,"last_attempt_at":"2030-01-15T13:00:00Z"`),
		})), 0o600, []string{"status"}},
		// An entry with a control character in its branch name is dropped, and counted.
		{"invalid_entry_dropped", ptr(stateFile(map[string]string{
			"repo-good": record(`,"last_attempt_at":"2030-01-15T11:59:00Z","origin_default_branch":"main"`),
			"repo-evil": record(`,"last_attempt_at":"2030-01-15T11:59:00Z","origin_default_branch":"ma\u001b[31min"`),
		})), 0o600, []string{"status"}},
		{"invalid_entry_dropped_json", ptr(stateFile(map[string]string{
			"repo-good": record(`,"last_attempt_at":"2030-01-15T11:59:00Z","origin_default_branch":"main"`),
			"repo-evil": record(`,"last_attempt_at":"2030-01-15T11:59:00Z","origin_default_branch":"ma\u001b[31min"`),
		})), 0o600, []string{"status", "--json"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := statusEnv(t)
			if tc.content != nil {
				writeRunState(t, root, *tc.content, tc.mode)
			}
			out, err := runCLI(t, "", tc.args...)
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if strings.Contains(out, "\x1b") || strings.Contains(out, target) || strings.Contains(out, root) {
				t.Errorf("output leaks a control character, the target fingerprint, or the state path:\n%s", out)
			}
			goldenCompare(t, "status_"+tc.name+".golden", out)
		})
	}
}

func ptr(s string) *string { return &s }

func TestStatusJSONIsOnlyTheStateAndHasStableShape(t *testing.T) {
	root := statusEnv(t)
	writeRunState(t, root, mixedState, 0o600)
	// A broken provider config must not stop the JSON form, which does not show providers.
	if err := os.WriteFile(os.Getenv("GIT_SYNC_CONFIG_FILE"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "", "status", "--json")
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var doc struct {
		Version  int `json:"version"`
		RunState struct {
			Status       string                       `json:"status"`
			Dropped      int                          `json:"dropped"`
			Repositories []map[string]json.RawMessage `json:"repositories"`
		} `json:"run_state"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not one JSON document: %v\n%s", err, out)
	}
	if doc.Version != 1 || doc.RunState.Status != "ok" || len(doc.RunState.Repositories) != 4 {
		t.Fatalf("unexpected document: %+v", doc)
	}
	wantKeys := []string{"repo_id", "last_attempt_at", "last_success_at", "duration_ms", "refs_pushed", "newest_origin_commit_at", "origin_default_branch"}
	for _, r := range doc.RunState.Repositories {
		if len(r) != len(wantKeys) {
			t.Errorf("repository has %d keys, want exactly %v: %v", len(r), wantKeys, r)
		}
		for _, k := range wantKeys {
			if _, ok := r[k]; !ok {
				t.Errorf("repository is missing %q: %v", k, r)
			}
		}
	}
	// Sorted by repo_id; an empty record has nulls, not omitted fields.
	if string(doc.RunState.Repositories[0]["repo_id"]) != `"repo-alpha"` || string(doc.RunState.Repositories[3]["repo_id"]) != `"repo-gamma"` {
		t.Errorf("repositories are not sorted by repo_id: %v", doc.RunState.Repositories)
	}
	if string(doc.RunState.Repositories[2]["last_success_at"]) != "null" || string(doc.RunState.Repositories[2]["refs_pushed"]) != "null" {
		t.Errorf("an empty record should have null fields: %v", doc.RunState.Repositories[2])
	}
}

func TestStatusReadsOnlyAndLeavesTheStateDirectoryAlone(t *testing.T) {
	root := statusEnv(t)
	writeRunState(t, root, mixedState, 0o600)
	// A crashed write's leftover is the daemon's to sweep at startup; status must not remove it.
	leftover := filepath.Join(root, ".run-state.json.tmp-123")
	if err := os.WriteFile(leftover, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var b strings.Builder
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filepath.Join(root, e.Name()))
			b.WriteString(e.Name() + "|" + info.ModTime().String() + "|" + info.Mode().String() + "|" + string(data) + "\n")
		}
		return b.String()
	}
	before := snapshot()
	for _, args := range [][]string{{"status"}, {"status", "--json"}} {
		if _, err := runCLI(t, "", args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if after := snapshot(); after != before {
		t.Errorf("status changed the state directory:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestStatusReportsASymlinkedStateFileAsNotRegular(t *testing.T) {
	root := statusEnv(t)
	writeRunState(t, root, mixedState, 0o600)
	real := filepath.Join(root, "run-state.json")
	moved := filepath.Join(root, "elsewhere.json")
	if err := os.Rename(real, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, real); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, `"status": "not_regular"`) || strings.Contains(out, "repo-alpha") {
		t.Errorf("a symlinked state file should be refused as the daemon refuses it:\n%s", out)
	}
}

func TestStatusStateDirectoryResolvesLikeTheDaemon(t *testing.T) {
	const body = `{"version":1,"repositories":{}}`
	t.Run("beside the provider config by default", func(t *testing.T) {
		statusEnv(t)
		t.Setenv("GIT_SYNC_MIRRORS_DIR", "")
		dir := t.TempDir()
		t.Setenv("GIT_SYNC_CONFIG_FILE", filepath.Join(dir, "git-sync.json"))
		writeRunState(t, filepath.Join(dir, "mirrors"), body, 0o600)
		out, err := runCLI(t, "", "status", "--json")
		if err != nil || !strings.Contains(out, `"status": "ok"`) {
			t.Errorf("did not read mirrors/ beside the config file: err=%v out=%s", err, out)
		}
	})
	t.Run("a relative GIT_SYNC_MIRRORS_DIR is taken from the working directory", func(t *testing.T) {
		statusEnv(t)
		dir := t.TempDir()
		t.Chdir(dir)
		t.Setenv("GIT_SYNC_MIRRORS_DIR", "relative-mirrors")
		writeRunState(t, filepath.Join(dir, "relative-mirrors"), body, 0o600)
		out, err := runCLI(t, "", "status", "--json")
		if err != nil || !strings.Contains(out, `"status": "ok"`) {
			t.Errorf("did not read the relative directory: err=%v out=%s", err, out)
		}
	})
}

func TestStatusNeverPrintsTheProviderToken(t *testing.T) {
	root := statusEnv(t)
	writeRunState(t, root, mixedState, 0o600)
	if _, err := runCLI(t, bearerToken+"\n", "config", "set", "origin", "--base-url", "https://git.example.com/", "--auth", "bearer", "--token-stdin"); err != nil {
		t.Fatalf("set: %v", err)
	}
	for _, args := range [][]string{{"status"}, {"status", "--absolute"}, {"status", "--json"}} {
		out, err := runCLI(t, "", args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(out, bearerToken) {
			t.Errorf("%v leaked the token:\n%s", args, out)
		}
	}
}

func TestStatusArguments(t *testing.T) {
	root := statusEnv(t)
	writeRunState(t, root, mixedState, 0o600)

	if out, err := runCLI(t, "", "status", "-h"); err != nil || !strings.Contains(out, "usage: mkonnect-gitsync connector status") {
		t.Errorf("-h should print usage and succeed: err=%v out=%q", err, out)
	}
	for _, args := range [][]string{{"status", "stray"}, {"status", "--unknown"}, {"status", "--json=maybe"}} {
		out, err := runCLI(t, "", args...)
		if err == nil {
			t.Errorf("%v should fail", args)
		}
		if out != "" {
			t.Errorf("%v should print nothing to stdout, got %q", args, out)
		}
	}
}
