package gitsync

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// testTarget is a well-formed target fingerprint; a record without one is not valid.
const testTarget = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func sampleRecord() runRecord {
	d, refs := int64(8250), 3
	return runRecord{
		Target:               targetFingerprint("https://git.example.com/group/repo-a", "https://replica.example.com/r/repo-a.git"),
		LastAttemptAt:        time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC),
		LastSuccessAt:        time.Date(2026, 1, 2, 3, 0, 8, 0, time.UTC),
		DurationMS:           &d,
		RefsPushed:           &refs,
		NewestOriginCommitAt: time.Date(2026, 1, 1, 22, 15, 30, 0, time.UTC),
		OriginDefaultBranch:  "main",
	}
}

func saveRecords(t *testing.T, s *runStateStore, m map[string]runRecord) {
	t.Helper()
	if err := s.save(func() map[string]runRecord { return m }); err != nil {
		t.Fatalf("save: %v", err)
	}
}

func writeStateFile(t *testing.T, root, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(root, runStateFile)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // WriteFile is subject to the umask
		t.Fatal(err)
	}
	return p
}

func TestRunStateRoundTripAcrossARestart(t *testing.T) {
	root := t.TempDir()
	rec := sampleRecord()
	saveRecords(t, newRunStateStore(root), map[string]runRecord{"repo-a": rec, "repo-b": {Target: testTarget, OriginDefaultBranch: "trunk"}})

	got := newRunStateStore(root).load()
	if len(got) != 2 {
		t.Fatalf("loaded %d records, want 2: %+v", len(got), got)
	}
	a := got["repo-a"]
	if a.Target != rec.Target || !a.LastAttemptAt.Equal(rec.LastAttemptAt) || !a.LastSuccessAt.Equal(rec.LastSuccessAt) ||
		a.DurationMS == nil || *a.DurationMS != 8250 || a.RefsPushed == nil || *a.RefsPushed != 3 ||
		!a.NewestOriginCommitAt.Equal(rec.NewestOriginCommitAt) || a.OriginDefaultBranch != "main" {
		t.Errorf("repo-a round-tripped to %+v, want %+v", a, rec)
	}
	b := got["repo-b"]
	if !b.LastSuccessAt.IsZero() || b.DurationMS != nil || b.RefsPushed != nil || b.OriginDefaultBranch != "trunk" {
		t.Errorf("repo-b round-tripped to %+v; unset fields must stay unset", b)
	}
}

func TestRunStateZeroRefsPushedSurvivesARestart(t *testing.T) {
	root := t.TempDir()
	zero := 0
	saveRecords(t, newRunStateStore(root), map[string]runRecord{"repo-a": {Target: testTarget, RefsPushed: &zero}})
	got := newRunStateStore(root).load()["repo-a"]
	if got.RefsPushed == nil || *got.RefsPushed != 0 {
		t.Errorf("refs_pushed = %v, want a present 0 (a no-op success), not absent", got.RefsPushed)
	}
}

func TestRunStateMissingFileIsEmptyAndSilent(t *testing.T) {
	logs := captureLogs(t)
	if got := newRunStateStore(filepath.Join(t.TempDir(), "does-not-exist")).load(); len(got) != 0 {
		t.Errorf("load = %v, want empty", got)
	}
	if logs.String() != "" {
		t.Errorf("a missing file is the normal first start and must not be logged: %q", logs)
	}
}

func TestRunStateFileIsOwnerOnly(t *testing.T) {
	root := t.TempDir()
	saveRecords(t, newRunStateStore(root), map[string]runRecord{"repo-a": sampleRecord()})
	info, err := os.Stat(filepath.Join(root, runStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestRunStateSaveCreatesTheRootOwnerOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirrors")
	saveRecords(t, newRunStateStore(root), map[string]runRecord{"repo-a": sampleRecord()})
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("root mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestRunStateUnusableFilesLoadAsEmpty(t *testing.T) {
	good := `{"version":1,"repositories":{"repo-a":{"target":"` + testTarget + `","origin_default_branch":"main"}}}`
	tooMany := new(strings.Builder)
	tooMany.WriteString(`{"version":1,"repositories":{`)
	for i := 0; i <= maxConfigRepos; i++ {
		if i > 0 {
			tooMany.WriteByte(',')
		}
		fmt.Fprintf(tooMany, `"repo-%d":{}`, i)
	}
	tooMany.WriteString(`}}`)

	tests := []struct {
		name, content, reason string
	}{
		{"garbage", "this is not json", "corrupt"},
		{"truncated", good[:len(good)/2], "corrupt"},
		{"trailing garbage", good + "x", "corrupt"},
		{"empty file", "", "empty"},
		{"not an object", "[1,2,3]", "corrupt"},
		{"future version", `{"version":2,"repositories":{}}`, "unsupported_version"},
		{"no version", `{"repositories":{}}`, "unsupported_version"},
		{"too many entries", tooMany.String(), "too_many_entries"},
		{"oversized", `{"version":1,"repositories":{},"padding":"` + strings.Repeat("x", maxRunStateBytes) + `"}`, "oversized"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			root := t.TempDir()
			writeStateFile(t, root, tc.content, 0o600)

			if got := newRunStateStore(root).load(); len(got) != 0 {
				t.Fatalf("load = %d records, want none", len(got))
			}
			out := logs.String()
			if !strings.Contains(out, "reason="+tc.reason) {
				t.Errorf("log %q does not name reason %q", out, tc.reason)
			}
			// The log is bounded and never echoes file content, the path, or an error that might.
			if len(out) > 400 || strings.Contains(out, "this is not json") || strings.Contains(out, root) {
				t.Errorf("log echoes content or path, or is unbounded: %q", out)
			}
		})
	}
}

func TestRunStateDropsOnlyInvalidEntries(t *testing.T) {
	logs := captureLogs(t)
	root := t.TempDir()
	writeStateFile(t, root, `{"version":1,"repositories":{
		"repo-a":{"target":"`+testTarget+`","origin_default_branch":"main","last_success_at":"2026-01-02T03:00:08Z"},
		"../escape":{"target":"`+testTarget+`","origin_default_branch":"main"},
		"repo-bad-time":{"target":"`+testTarget+`","last_success_at":"yesterday"},
		"repo-neg-duration":{"target":"`+testTarget+`","duration_ms":-1},
		"repo-neg-refs":{"target":"`+testTarget+`","refs_pushed":-4},
		"repo-bad-branch":{"target":"`+testTarget+`","origin_default_branch":"ma\u001bin"},
		"repo-bad-target":{"target":"not-a-fingerprint"},
		"repo-year-1":{"target":"`+testTarget+`","newest_origin_commit_at":"0001-01-01T00:00:01Z"},
		"repo-not-an-object":7,
		"repo-no-target":{"origin_default_branch":"main"},
		"repo-b":{"target":"`+testTarget+`","duration_ms":5}
	}}`, 0o600)

	got := newRunStateStore(root).load()
	if len(got) != 2 || got["repo-a"].OriginDefaultBranch != "main" || got["repo-b"].DurationMS == nil {
		t.Errorf("load = %+v, want exactly repo-a and repo-b", got)
	}
	if !strings.Contains(logs.String(), "dropped=9") {
		t.Errorf("log %q should count the 9 dropped entries", logs)
	}
	if strings.Contains(logs.String(), "escape") {
		t.Errorf("a dropped entry's key must not be echoed: %q", logs)
	}
}

func TestRunStateIgnoresUnknownFields(t *testing.T) {
	root := t.TempDir()
	writeStateFile(t, root, `{"version":1,"added_later":true,"repositories":{"repo-a":{"target":"`+testTarget+`","origin_default_branch":"main","added_later":1}}}`, 0o600)
	if got := newRunStateStore(root).load(); got["repo-a"].OriginDefaultBranch != "main" {
		t.Errorf("load = %+v, want repo-a kept", got)
	}
}

func TestRunStatePermissionPolicy(t *testing.T) {
	const content = `{"version":1,"repositories":{"repo-a":{"target":"` + testTarget + `","origin_default_branch":"main"}}}`
	tests := []struct {
		name   string
		mode   os.FileMode
		accept bool
	}{
		{"owner only", 0o600, true},
		{"group readable", 0o640, true},
		// A volume mounted with an fsGroup is group-writable; refusing it would discard the state at
		// every restart. Only a file anyone can write is refused: its content may not be ours.
		{"group writable", 0o660, true},
		{"world readable", 0o644, true},
		{"world writable", 0o666, false},
		{"world writable only", 0o602, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			root := t.TempDir()
			writeStateFile(t, root, content, tc.mode)
			got := newRunStateStore(root).load()
			if tc.accept != (len(got) == 1) {
				t.Errorf("mode %v: loaded %d records, accept = %v", tc.mode, len(got), tc.accept)
			}
			if !tc.accept && !strings.Contains(logs.String(), "reason=world_writable") {
				t.Errorf("log %q should say the file was refused for being world-writable", logs)
			}
		})
	}
}

func TestRunStateRefusesASymlinkAndLeavesItsTargetAlone(t *testing.T) {
	logs := captureLogs(t)
	root := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.json")
	const content = `{"version":1,"repositories":{"repo-a":{"target":"` + testTarget + `","origin_default_branch":"main"}}}`
	if err := os.WriteFile(elsewhere, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, runStateFile)); err != nil {
		t.Fatal(err)
	}

	s := newRunStateStore(root)
	if got := s.load(); len(got) != 0 {
		t.Errorf("load followed a symlink: %+v", got)
	}
	if !strings.Contains(logs.String(), "reason=not_regular") {
		t.Errorf("log %q should name reason not_regular", logs)
	}
	// Saving replaces the link itself (rename does not follow it) and never writes through it.
	saveRecords(t, s, map[string]runRecord{"repo-b": {Target: testTarget, OriginDefaultBranch: "trunk"}})
	if b, _ := os.ReadFile(elsewhere); string(b) != content {
		t.Errorf("the symlink's target was modified: %q", b)
	}
	if got := newRunStateStore(root).load(); got["repo-b"].OriginDefaultBranch != "trunk" {
		t.Errorf("after save the state file should be a regular file holding repo-b: %+v", got)
	}
}

// A FIFO swapped in for the state file would block a plain open forever, hanging startup.
func TestRunStateRefusesAFifoWithoutBlocking(t *testing.T) {
	captureLogs(t)
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, runStateFile), 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan map[string]runRecord, 1)
	go func() { done <- newRunStateStore(root).load() }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Errorf("load = %+v, want empty", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("load blocked on a FIFO")
	}
}

func TestRunStateADirectoryInPlaceOfTheFileIsIgnored(t *testing.T) {
	captureLogs(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, runStateFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := newRunStateStore(root).load(); len(got) != 0 {
		t.Errorf("load = %+v, want empty", got)
	}
}

func TestRunStateLoadSweepsOnlyItsOwnLeftoverTempFiles(t *testing.T) {
	root := t.TempDir()
	leftover := filepath.Join(root, runStateTempPrefix+"123456")
	other := filepath.Join(root, "unrelated.tmp")
	mirror := filepath.Join(root, strings.Repeat("a", 64)+".git")
	for _, p := range []string{leftover, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "HEAD"), []byte("ref"), 0o600); err != nil {
		t.Fatal(err)
	}

	newRunStateStore(root).load()

	if _, err := os.Lstat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("our own leftover temp file should be removed, err = %v", err)
	}
	for _, p := range []string{other, filepath.Join(mirror, "HEAD")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s is not ours and must be left alone: %v", filepath.Base(p), err)
		}
	}
}

func TestRunStateSaveTouchesOnlyItsOwnFiles(t *testing.T) {
	root := t.TempDir()
	other := filepath.Join(root, "something-else")
	if err := os.WriteFile(other, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	saveRecords(t, newRunStateStore(root), map[string]runRecord{"repo-a": sampleRecord()})

	entries, _ := os.ReadDir(root)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 || !contains2(names, runStateFile) || !contains2(names, "something-else") {
		t.Errorf("root holds %v, want only run-state.json and the unrelated file (no temp file left)", names)
	}
	if b, _ := os.ReadFile(other); string(b) != "keep" {
		t.Errorf("unrelated file changed: %q", b)
	}
}

func contains2(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// A failed write must leave the previous state byte-for-byte intact and no temp file behind.
func TestRunStateFailedWriteKeepsTheOldFileAndLeavesNoTemp(t *testing.T) {
	failures := map[string]func(*runStateStore){
		"rename": func(s *runStateStore) {
			s.ops.rename = func(string, string) error { return errors.New("injected rename failure") }
		},
		"dir sync": func(s *runStateStore) {
			s.ops.syncDir = func(string) error { return errors.New("injected sync failure") }
		},
	}
	for name, inject := range failures {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			root := t.TempDir()
			s := newRunStateStore(root)
			saveRecords(t, s, map[string]runRecord{"repo-a": {Target: testTarget, OriginDefaultBranch: "main"}})
			before, _ := os.ReadFile(filepath.Join(root, runStateFile))

			inject(s)
			err := s.save(func() map[string]runRecord {
				return map[string]runRecord{"repo-a": {Target: testTarget, OriginDefaultBranch: "other"}}
			})
			if err == nil {
				t.Fatal("save should report the failure to its caller")
			}
			if name == "rename" {
				if after, _ := os.ReadFile(filepath.Join(root, runStateFile)); !bytes.Equal(before, after) {
					t.Errorf("a failed rename changed the state file: %q -> %q", before, after)
				}
			}
			entries, _ := os.ReadDir(root)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), runStateTempPrefix) {
					t.Errorf("temp file %q left behind", e.Name())
				}
			}
			if !strings.Contains(logs.String(), "run state write failed") || strings.Contains(logs.String(), root) {
				t.Errorf("failure should be logged without the path: %q", logs)
			}
		})
	}
}

func TestRunStateWriteIntoAnUnwritableRootFailsQuietlyForTheCaller(t *testing.T) {
	logs := captureLogs(t)
	root := t.TempDir()
	// A regular file where the directory should be: MkdirAll fails whoever runs the test.
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := newRunStateStore(filepath.Join(blocker, "mirrors"))
	if err := s.save(func() map[string]runRecord { return map[string]runRecord{"repo-a": sampleRecord()} }); err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(logs.String(), "run state write failed") {
		t.Errorf("failure not logged: %q", logs)
	}
}

// The snapshot is taken after the write lock is held, so an older snapshot can never be written
// after a newer one.
func TestRunStateSnapshotIsTakenUnderTheWriteLock(t *testing.T) {
	root := t.TempDir()
	s := newRunStateStore(root)

	var mu sync.Mutex
	version := 0
	snapshot := func() map[string]runRecord {
		mu.Lock()
		defer mu.Unlock()
		return map[string]runRecord{"repo-a": {Target: testTarget, OriginDefaultBranch: fmt.Sprintf("v%03d", version)}}
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			version++
			mu.Unlock()
			if err := s.save(snapshot); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := newRunStateStore(root).load()["repo-a"].OriginDefaultBranch; got != "v020" {
		t.Errorf("last write left %q on disk, want the newest state v020", got)
	}
}

// The largest state the connector can legitimately hold must be written and read back: a file it
// can write but not read would silently lose every success time at the next restart.
func TestRunStateWorstCaseFileLoadsBack(t *testing.T) {
	root := t.TempDir()
	m := make(map[string]runRecord, maxConfigRepos)
	nastyBranch := strings.Repeat(`"`, maxBranchNameLength) // every byte needs escaping in JSON
	d, refs := int64(1<<40), 1<<30
	for i := range maxConfigRepos {
		id := fmt.Sprintf("%0*d", maxRepoIDLength, i)
		m[id] = runRecord{
			Target:               targetFingerprint("a", "b"),
			LastAttemptAt:        time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
			LastSuccessAt:        time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
			DurationMS:           &d,
			RefsPushed:           &refs,
			NewestOriginCommitAt: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
			OriginDefaultBranch:  nastyBranch,
		}
	}
	s := newRunStateStore(root)
	saveRecords(t, s, m)
	if got := newRunStateStore(root).load(); len(got) != maxConfigRepos {
		t.Fatalf("loaded %d of %d records", len(got), maxConfigRepos)
	}
}

func TestRunStateRefusesToWriteMoreThanItWillRead(t *testing.T) {
	logs := captureLogs(t)
	root := t.TempDir()
	m := map[string]runRecord{}
	for i := range maxConfigRepos * 4 { // more entries than a configuration may hold
		m[fmt.Sprintf("%0*d", maxRepoIDLength, i)] = runRecord{Target: testTarget, OriginDefaultBranch: strings.Repeat("é", 120)}
	}
	s := newRunStateStore(root)
	if err := s.save(func() map[string]runRecord { return m }); err == nil {
		t.Fatal("save should refuse a state its own load would reject")
	}
	if _, err := os.Stat(filepath.Join(root, runStateFile)); err == nil {
		t.Error("nothing should have been written")
	}
	if !strings.Contains(logs.String(), "run state write failed") {
		t.Errorf("refusal not logged: %q", logs)
	}
}

func TestRunStateWritesNoHTMLEscapes(t *testing.T) {
	root := t.TempDir()
	saveRecords(t, newRunStateStore(root), map[string]runRecord{"repo-a": {Target: testTarget, OriginDefaultBranch: "a<b>&c"}})
	b, _ := os.ReadFile(filepath.Join(root, runStateFile))
	if !strings.Contains(string(b), "a<b>&c") {
		t.Errorf("state file %q escaped <, > or &, which inflates it past what the reader allows", b)
	}
}

func TestRunStateStoresNoSecretsOrLocations(t *testing.T) {
	root := t.TempDir()
	rec := sampleRecord()
	saveRecords(t, newRunStateStore(root), map[string]runRecord{"repo-a": rec})
	b, _ := os.ReadFile(filepath.Join(root, runStateFile))
	for _, forbidden := range []string{"git.example.com", "replica.example.com", "http", "token", "Bearer"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("state file contains %q: %s", forbidden, b)
		}
	}
}

func TestTargetFingerprint(t *testing.T) {
	a := targetFingerprint("https://o.example.com/a", "https://r.example.com/a")
	if len(a) != 64 {
		t.Fatalf("fingerprint %q is not 64 hex characters", a)
	}
	if a != targetFingerprint("https://o.example.com/a", "https://r.example.com/a") {
		t.Error("fingerprint is not deterministic")
	}
	for _, other := range [][2]string{
		{"https://o.example.com/b", "https://r.example.com/a"},
		{"https://o.example.com/a", "https://r.example.com/b"},
		{"https://o.example.com/a" + "\n", "https://r.example.com/a"},
	} {
		if a == targetFingerprint(other[0], other[1]) {
			t.Errorf("fingerprint collides for %v", other)
		}
	}
	// The two parts must not be able to trade characters across the boundary.
	if targetFingerprint("ab", "c") == targetFingerprint("a", "bc") {
		t.Error("fingerprint is ambiguous about where the origin ends")
	}
}
