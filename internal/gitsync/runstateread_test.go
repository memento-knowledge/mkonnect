package gitsync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRunStateReportsEachProblem(t *testing.T) {
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		want    RunStateProblem
	}{
		{"empty", "", 0o600, RunStateEmpty},
		{"corrupt", "this is not json", 0o600, RunStateCorrupt},
		{"future version", `{"version":2,"repositories":{}}`, 0o600, RunStateUnsupported},
		{"world writable", `{"version":1,"repositories":{}}`, 0o666, RunStateWorldWritable},
		{"ok", `{"version":1,"repositories":{}}`, 0o600, RunStateOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeStateFile(t, root, tc.content, tc.mode)
			if recs, _, got := readRunStateAt(root); got != tc.want || recs == nil {
				t.Errorf("problem = %q (records nil: %v), want %q with non-nil records", got, recs == nil, tc.want)
			}
		})
	}
	if _, _, got := readRunStateAt(filepath.Join(t.TempDir(), "absent")); got != RunStateMissing {
		t.Errorf("a missing directory should be %q, got %q", RunStateMissing, got)
	}
}

func TestReadRunStateClearsTheTargetAndCountsDroppedEntries(t *testing.T) {
	root := t.TempDir()
	s := newRunStateStore(root)
	good, bad := sampleRecord(), sampleRecord()
	bad.OriginDefaultBranch = "ma\x1bin"
	saveRecords(t, s, map[string]runRecord{"repo-a": good, "repo-b": bad})

	recs, dropped, problem := readRunStateAt(root)
	if problem != RunStateOK || dropped != 1 || len(recs) != 1 {
		t.Fatalf("problem=%q dropped=%d records=%v, want ok, 1 dropped, 1 record", problem, dropped, recs)
	}
	if got := recs["repo-a"]; got.Target != "" || got.OriginDefaultBranch != "main" {
		t.Errorf("record = %+v, want the target cleared and the rest kept", got)
	}
}

func TestReadRunStateLeavesTempFilesAndLogsNothing(t *testing.T) {
	root := t.TempDir()
	writeStateFile(t, root, "garbage", 0o600)
	leftover := filepath.Join(root, runStateTempPrefix+"123")
	if err := os.WriteFile(leftover, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)
	readRunStateAt(root)
	if _, err := os.Stat(leftover); err != nil {
		t.Errorf("reading removed the daemon's temp file: %v", err)
	}
	if logs.String() != "" {
		t.Errorf("reading logged: %q", logs.String())
	}
}

func TestReadRunStateUsesTheDaemonsRoot(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GIT_SYNC_MIRRORS_DIR", "relative")
	if err := os.Mkdir(filepath.Join(dir, "relative"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeStateFile(t, filepath.Join(dir, "relative"), `{"version":1,"repositories":{}}`, 0o600)
	if _, _, problem := ReadRunState(); problem != RunStateOK {
		t.Errorf("problem = %q, want ok for a relative root resolved against the working directory", problem)
	}
}
