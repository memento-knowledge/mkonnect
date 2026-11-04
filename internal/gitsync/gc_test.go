package gitsync

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var gcNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// gcMirrorName returns the directory name Mirrors.Dir gives repoID.
func gcMirrorName(repoID string) string {
	return filepath.Base(Mirrors{Root: "/unused"}.Dir(repoID))
}

// gcHarness drives sweepMirrors against a real temporary root with injectable time, remove and
// claim, and captures the log.
type gcHarness struct {
	t              *testing.T
	root           string
	keep           map[string]struct{}
	now            time.Time
	busy           map[string]bool
	lastConfigured map[string]time.Time
	claim          func(name string, do func() error) (bool, error)
	remove         func(r *os.Root, name string) error
	removed        []string // names handed to remove, root-relative
	log            bytes.Buffer
}

func newGCHarness(t *testing.T) *gcHarness {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &gcHarness{t: t, root: root, keep: map[string]struct{}{}, now: gcNow, busy: map[string]bool{}}
}

func (h *gcHarness) sweep() sweepResult {
	h.t.Helper()
	return h.sweepCtx(context.Background())
}

func (h *gcHarness) sweepCtx(ctx context.Context) sweepResult {
	h.t.Helper()
	claim := h.claim
	if claim == nil {
		claim = func(_ string, do func() error) (bool, error) { return true, do() }
	}
	return sweepMirrors(ctx, sweepParams{
		Root:           h.root,
		Keep:           h.keep,
		Now:            h.now,
		Grace:          mirrorGCGrace,
		Busy:           func(name string) bool { return h.busy[name] },
		LastConfigured: func(name string) (time.Time, bool) { t, ok := h.lastConfigured[name]; return t, ok },
		Claim:          claim,
		Remove: func(r *os.Root, name string) error {
			h.removed = append(h.removed, name)
			if h.remove != nil {
				return h.remove(r, name)
			}
			return r.RemoveAll(name)
		},
		Log: slog.New(slog.NewTextHandler(&h.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
}

// mirror creates a mirror-shaped directory for repoID and returns its name.
func (h *gcHarness) mirror(repoID string) string {
	h.t.Helper()
	name := gcMirrorName(repoID)
	h.mkdir(name)
	return name
}

// mkdir creates a directory (with a file inside, so removal must be recursive) under the root.
func (h *gcHarness) mkdir(name string) {
	h.t.Helper()
	dir := filepath.Join(h.root, name)
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *gcHarness) mark(name string, at time.Time) {
	h.t.Helper()
	h.writeMarkerRaw(name, at.UTC().Format(time.RFC3339Nano))
}

func (h *gcHarness) writeMarkerRaw(name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.root, name, orphanMarkerName), []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *gcHarness) path(parts ...string) string {
	return filepath.Join(append([]string{h.root}, parts...)...)
}

func (h *gcHarness) exists(parts ...string) bool {
	_, err := os.Lstat(h.path(parts...))
	return err == nil
}

func (h *gcHarness) markerTime(name string) (time.Time, bool) {
	b, err := os.ReadFile(h.path(name, orphanMarkerName))
	if err != nil {
		return time.Time{}, false
	}
	tm, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	return tm, err == nil
}

func (h *gcHarness) logged() string { return h.log.String() }

func TestSweepFirstSightMarksButNeverDeletes(t *testing.T) {
	h := newGCHarness(t)
	name := h.mirror("repo-a")
	h.keep[gcMirrorName("repo-b")] = struct{}{}

	res := h.sweep()

	if !h.exists(name) {
		t.Fatal("an orphan seen for the first time must not be deleted")
	}
	if res.Marked != 1 || res.Removed != 0 {
		t.Fatalf("result = %+v, want one mark and no removal", res)
	}
	got, ok := h.markerTime(name)
	if !ok || !got.Equal(gcNow) {
		t.Fatalf("marker = %v (valid=%v), want %v", got, ok, gcNow)
	}
	info, err := os.Stat(h.path(name, orphanMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("marker mode = %v, want 0600", info.Mode().Perm())
	}
	if !strings.Contains(h.logged(), name) {
		t.Fatalf("first mark should be logged with the mirror name; log: %s", h.logged())
	}
}

func TestSweepGraceBoundary(t *testing.T) {
	h := newGCHarness(t)
	justUnder := h.mirror("under")
	exactly := h.mirror("exactly")
	h.mark(justUnder, gcNow.Add(-mirrorGCGrace+time.Second))
	h.mark(exactly, gcNow.Add(-mirrorGCGrace))
	h.keep[gcMirrorName("other")] = struct{}{}

	res := h.sweep()

	if !h.exists(justUnder) {
		t.Error("an orphan one second short of the grace period was deleted")
	}
	if h.exists(exactly) {
		t.Error("an orphan at exactly the grace period was not deleted")
	}
	if res.Removed != 1 {
		t.Fatalf("result = %+v, want one removal", res)
	}
	if got := h.logged(); !strings.Contains(got, exactly) || !strings.Contains(got, "age") {
		t.Fatalf("deletion log should carry the mirror name and age; log: %s", got)
	}
}

func TestSweepReturnToConfigClearsMarkAndRestartsClock(t *testing.T) {
	h := newGCHarness(t)
	name := h.mirror("repo-a")
	h.mark(name, gcNow.Add(-mirrorGCGrace-time.Hour)) // long overdue
	h.keep[name] = struct{}{}

	res := h.sweep()
	if !h.exists(name) {
		t.Fatal("a mirror that is back in the configuration was deleted")
	}
	if h.exists(name, orphanMarkerName) {
		t.Fatal("the orphan mark must be cleared when the repository returns to the configuration")
	}
	if res.Cleared != 1 {
		t.Fatalf("result = %+v, want one cleared mark", res)
	}

	// Dropped again: the clock starts over, so it is marked, not deleted.
	delete(h.keep, name)
	h.keep[gcMirrorName("other")] = struct{}{}
	h.now = gcNow.Add(time.Hour)
	res = h.sweep()
	if !h.exists(name) || res.Marked != 1 {
		t.Fatalf("re-orphaned mirror should be re-marked, not deleted (exists=%v result=%+v)", h.exists(name), res)
	}
}

func TestSweepNeverTouchesAnythingButMirrorDirectories(t *testing.T) {
	h := newGCHarness(t)
	h.keep[gcMirrorName("other")] = struct{}{}
	valid := strings.Repeat("a", 64)
	names := []string{
		strings.ToUpper(valid) + ".git",  // uppercase hex
		valid[:63] + ".git",              // too short
		valid + "a.git",                  // too long
		valid,                            // no .git suffix
		valid + ".git.bak",               // trailing junk
		"x" + valid[1:] + ".git",         // non-hex digit
		".git",                           // bare suffix
		"repo-a.git",                     // a repo_id-shaped name
		gcTrashDirName + "x",             // not our trash dir
		strings.Repeat("g", 64) + ".git", // 64 chars but not hex
		" " + valid + ".git",             // leading space
		valid + ".git ",                  // trailing space
		valid + ".GIT",                   // wrong suffix case
		"a" + valid + ".git",             // 65 hex
		valid + "\u200b" + ".git",        // zero-width character
		"sub",                            // an ordinary directory
	}
	for _, n := range names {
		h.mkdir(n)
		h.mark(n, gcNow.Add(-365*24*time.Hour)) // would be long overdue if it were a mirror
	}
	if err := os.WriteFile(h.path("run-state.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path(valid[:60]+"bbbb.git"), []byte("file, not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	names = append(names, "run-state.json", valid[:60]+"bbbb.git")

	h.sweep()

	for _, n := range names {
		if !h.exists(n) {
			t.Errorf("%q was removed; only 64-hex .git directories may be", n)
		}
	}
	if len(h.removed) != 0 {
		t.Fatalf("remove was called for %v", h.removed)
	}
}

func TestSweepNeverFollowsOrDeletesSymlinks(t *testing.T) {
	h := newGCHarness(t)
	h.keep[gcMirrorName("other")] = struct{}{}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "precious"), []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, orphanMarkerName), []byte(gcNow.Add(-400*24*time.Hour).Format(time.RFC3339)), 0o600); err != nil {
		t.Fatal(err)
	}
	real := h.mirror("real") // a genuine orphan the symlink could point at
	h.mark(real, gcNow)

	toOutside := gcMirrorName("link-out")
	toInside := gcMirrorName("link-in")
	if err := os.Symlink(outside, h.path(toOutside)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.path(real), h.path(toInside)); err != nil {
		t.Fatal(err)
	}
	dangling := gcMirrorName("link-dangling")
	if err := os.Symlink(h.path("does-not-exist"), h.path(dangling)); err != nil {
		t.Fatal(err)
	}
	// A regular file with a mirror name.
	file := gcMirrorName("a-file")
	if err := os.WriteFile(h.path(file), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := h.sweep()

	for _, n := range []string{toOutside, toInside, dangling, file} {
		if _, err := os.Lstat(h.path(n)); err != nil {
			t.Errorf("%s was touched: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "precious")); err != nil {
		t.Fatalf("the symlink target outside the root was modified: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, orphanMarkerName)); err != nil {
		t.Fatalf("a marker outside the root was modified: %v", err)
	}
	if !h.exists(real, "HEAD") {
		t.Fatal("the symlink target inside the root was modified")
	}
	if res.Skipped != 4 {
		t.Fatalf("result = %+v, want 4 skipped candidates", res)
	}
	for _, n := range []string{toOutside, toInside, dangling, file} {
		if !strings.Contains(h.logged(), n) {
			t.Errorf("skipped unsafe candidate %s was not logged", n)
		}
	}
	if strings.Contains(h.logged(), outside) {
		t.Fatalf("the log leaked a path outside the root: %s", h.logged())
	}
}

func TestSweepSkipsBusyMirrorsWithoutMarkingThem(t *testing.T) {
	h := newGCHarness(t)
	h.keep[gcMirrorName("other")] = struct{}{}
	busy := h.mirror("busy")
	h.mark(busy, gcNow.Add(-2*mirrorGCGrace))
	fresh := h.mirror("fresh-busy")
	h.busy[busy], h.busy[fresh] = true, true

	res := h.sweep()

	if !h.exists(busy) {
		t.Fatal("a mirror with a sync queued or running was deleted")
	}
	if h.exists(fresh, orphanMarkerName) {
		t.Fatal("a busy mirror must not be marked orphaned")
	}
	if res.Busy != 2 || res.Removed != 0 || res.Marked != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestSweepClaimDeclinedLeavesMirror(t *testing.T) {
	h := newGCHarness(t)
	h.keep[gcMirrorName("other")] = struct{}{}
	name := h.mirror("repo-a")
	h.mark(name, gcNow.Add(-2*mirrorGCGrace))
	h.claim = func(string, func() error) (bool, error) { return false, nil }

	res := h.sweep()

	if !h.exists(name, "HEAD") || len(h.removed) != 0 {
		t.Fatal("a mirror whose claim was declined (sync started or repo returned) must be left alone")
	}
	if res.Busy != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestSweepCorruptMarkerIsFirstSeenNow(t *testing.T) {
	long := strings.Repeat("9", 4096)
	tests := []struct {
		name  string
		setup func(h *gcHarness, mirror string)
	}{
		{"garbage", func(h *gcHarness, m string) { h.writeMarkerRaw(m, "not a time") }},
		{"empty", func(h *gcHarness, m string) { h.writeMarkerRaw(m, "") }},
		{"oversized", func(h *gcHarness, m string) { h.writeMarkerRaw(m, long) }},
		{"future", func(h *gcHarness, m string) { h.mark(m, gcNow.Add(48*time.Hour)) }},
		{"empty directory", func(h *gcHarness, m string) {
			if err := os.Mkdir(h.path(m, orphanMarkerName), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink to old marker", func(h *gcHarness, m string) {
			target := filepath.Join(t.TempDir(), "old")
			old := gcNow.Add(-400 * 24 * time.Hour).Format(time.RFC3339Nano)
			if err := os.WriteFile(target, []byte(old), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, h.path(m, orphanMarkerName)); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newGCHarness(t)
			h.keep[gcMirrorName("other")] = struct{}{}
			m := h.mirror("repo-a")
			tc.setup(h, m)

			res := h.sweep()

			if !h.exists(m, "HEAD") || len(h.removed) != 0 {
				t.Fatalf("a mirror with an unusable marker must never be deleted (result %+v)", res)
			}
			got, ok := h.markerTime(m)
			if !ok || !got.Equal(gcNow) {
				t.Fatalf("marker not rewritten as first seen now: %v valid=%v", got, ok)
			}
			if res.Marked != 1 {
				t.Fatalf("result = %+v, want one mark", res)
			}
		})
	}
}

func TestSweepMarkerThatCannotBeReplacedFailsSafe(t *testing.T) {
	h := newGCHarness(t)
	h.keep[gcMirrorName("other")] = struct{}{}
	m := h.mirror("repo-a")
	// A non-empty directory squats on the marker name and cannot be removed by Remove.
	if err := os.MkdirAll(h.path(m, orphanMarkerName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path(m, orphanMarkerName, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	h.sweep()
	h.now = gcNow.Add(10 * mirrorGCGrace)
	h.sweep()

	if !h.exists(m, "HEAD") || len(h.removed) != 0 {
		t.Fatal("a mirror whose marker cannot be written must never be deleted")
	}
}

func TestSweepRemoveFailureIsLoggedAndRetried(t *testing.T) {
	h := newGCHarness(t)
	h.keep[gcMirrorName("other")] = struct{}{}
	m := h.mirror("repo-a")
	h.mark(m, gcNow.Add(-mirrorGCGrace-time.Hour))
	fail := true
	h.remove = func(r *os.Root, name string) error {
		if fail {
			return &fs.PathError{Op: "unlinkat", Path: "/secret/elsewhere/" + name, Err: syscall.EBUSY}
		}
		return r.RemoveAll(name)
	}

	res := h.sweep()
	if res.Failed != 1 || res.Removed != 0 {
		t.Fatalf("result = %+v, want one failure", res)
	}
	if strings.Contains(h.logged(), "/secret/elsewhere") {
		t.Fatalf("a path from the error leaked into the log: %s", h.logged())
	}

	fail = false
	h.removed = nil
	res = h.sweep()
	if res.Removed != 1 {
		t.Fatalf("second sweep result = %+v, want the retry to succeed", res)
	}
	entries, err := os.ReadDir(h.path(gcTrashDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("trash not emptied after a successful retry: %v", entries)
	}
	if h.exists(m) {
		t.Fatal("the orphan is still in place after the retry")
	}
}

func TestSweepFreesMirrorNameBeforeRemoving(t *testing.T) {
	h := newGCHarness(t)
	h.keep[gcMirrorName("other")] = struct{}{}
	m := h.mirror("repo-a")
	h.mark(m, gcNow.Add(-mirrorGCGrace))
	var nameFreeDuringRemove bool
	h.remove = func(r *os.Root, name string) error {
		_, err := os.Lstat(h.path(m))
		nameFreeDuringRemove = errors.Is(err, fs.ErrNotExist) && strings.HasPrefix(name, gcTrashDirName+"/"+m+".")
		return r.RemoveAll(name)
	}

	h.sweep()

	if !nameFreeDuringRemove {
		t.Fatal("the mirror must be moved aside atomically before it is removed, so a sync that starts meanwhile gets a fresh directory")
	}
}

func TestSweepRecoversOnlyWellFormedLeftoverTombstones(t *testing.T) {
	h := newGCHarness(t)
	h.keep[gcMirrorName("other")] = struct{}{}
	if err := os.Mkdir(h.path(gcTrashDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	good := gcMirrorName("repo-a") + ".0123456789abcdef"
	h.mkdir(filepath.Join(gcTrashDirName, good))
	badNames := []string{
		gcMirrorName("repo-b"),                       // no random suffix
		gcMirrorName("repo-b") + ".0123456789ABCDEF", // uppercase suffix
		gcMirrorName("repo-b") + ".0123456789abcde",  // short suffix
		"notes", // arbitrary
	}
	for _, n := range badNames {
		h.mkdir(filepath.Join(gcTrashDirName, n))
	}
	outside := t.TempDir()
	link := gcMirrorName("repo-c") + ".0123456789abcdef"
	if err := os.Symlink(outside, h.path(gcTrashDirName, link)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "precious"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	file := gcMirrorName("repo-d") + ".0123456789abcdef"
	if err := os.WriteFile(h.path(gcTrashDirName, file), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	h.sweep()

	if h.exists(gcTrashDirName, good) {
		t.Error("a well-formed leftover tombstone was not recovered")
	}
	for _, n := range append(badNames, link, file) {
		if !h.exists(gcTrashDirName, n) {
			t.Errorf("trash entry %q must not be removed", n)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "precious")); err != nil {
		t.Fatal("a symlink in the trash was followed")
	}
}

func TestSweepRefusesTrashThatIsNotARealDirectory(t *testing.T) {
	h := newGCHarness(t)
	h.keep[gcMirrorName("other")] = struct{}{}
	m := h.mirror("repo-a")
	h.mark(m, gcNow.Add(-mirrorGCGrace))
	elsewhere := h.path("elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, h.path(gcTrashDirName)); err != nil {
		t.Fatal(err)
	}

	res := h.sweep()

	if !h.exists(m, "HEAD") || len(h.removed) != 0 {
		t.Fatal("must not move mirrors through a trash symlink")
	}
	if res.Failed != 1 {
		t.Fatalf("result = %+v, want one failure", res)
	}
	entries, _ := os.ReadDir(elsewhere)
	if len(entries) != 0 {
		t.Fatal("something was written through the trash symlink")
	}
}

func TestSweepRootHandling(t *testing.T) {
	t.Run("relative or empty root is refused", func(t *testing.T) {
		for _, root := range []string{"", ".", "mirrors", "../mirrors"} {
			h := newGCHarness(t)
			h.root = root
			res := h.sweep()
			if res != (sweepResult{}) {
				t.Errorf("root %q: result = %+v, want no action", root, res)
			}
		}
	})
	t.Run("filesystem root is refused", func(t *testing.T) {
		h := newGCHarness(t)
		h.root = string(filepath.Separator)
		if res := h.sweep(); res != (sweepResult{}) {
			t.Fatalf("result = %+v, want no action", res)
		}
	})
	t.Run("a root named like a mirror is never removed", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, strings.Repeat("c", 64)+".git")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		h := newGCHarness(t)
		h.root = root
		h.keep[gcMirrorName("other")] = struct{}{}
		m := h.mirror("repo-a")
		h.mark(m, gcNow.Add(-mirrorGCGrace))
		h.sweep()
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("root was removed: %v", err)
		}
	})
	t.Run("a missing root is not an error and creates nothing", func(t *testing.T) {
		h := newGCHarness(t)
		h.root = filepath.Join(h.root, "missing")
		h.sweep()
		if _, err := os.Lstat(h.root); err == nil {
			t.Fatal("the sweep created the root")
		}
	})
	t.Run("a root that is itself a symlink works", func(t *testing.T) {
		h := newGCHarness(t)
		link := filepath.Join(t.TempDir(), "mirrors-link")
		if err := os.Symlink(h.root, link); err != nil {
			t.Fatal(err)
		}
		real := h.root
		h.keep[gcMirrorName("other")] = struct{}{}
		m := h.mirror("repo-a")
		h.mark(m, gcNow.Add(-mirrorGCGrace))
		h.root = link
		if res := h.sweep(); res.Removed != 1 {
			t.Fatalf("result = %+v", res)
		}
		if _, err := os.Lstat(filepath.Join(real, m)); err == nil {
			t.Fatal("orphan behind a symlinked root was not removed")
		}
	})
}

func TestSweepLogsCarryNoPathsOrRepoIDs(t *testing.T) {
	h := newGCHarness(t)
	h.keep[gcMirrorName("other")] = struct{}{}
	m := h.mirror("super-secret-repo-id")
	h.mark(m, gcNow.Add(-mirrorGCGrace))
	h.remove = func(r *os.Root, name string) error {
		return &fs.PathError{Op: "unlinkat", Path: h.root + "/" + name, Err: syscall.EACCES}
	}

	h.sweep()

	got := h.logged()
	if strings.Contains(got, h.root) || strings.Contains(got, "super-secret-repo-id") {
		t.Fatalf("log leaks a path or repo_id: %s", got)
	}
	if !strings.Contains(got, m) || !strings.Contains(got, "permission denied") {
		t.Fatalf("log should name the mirror and a classified reason: %s", got)
	}
}

func TestSweepStopsWhenContextIsCancelled(t *testing.T) {
	h := newGCHarness(t)
	h.keep[gcMirrorName("other")] = struct{}{}
	m := h.mirror("repo-a")
	h.mark(m, gcNow.Add(-mirrorGCGrace))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h.sweepCtx(ctx)

	if !h.exists(m, "HEAD") || len(h.removed) != 0 {
		t.Fatal("a cancelled sweep must not delete anything")
	}
}

func TestSweepMarkOlderThanTheLastTimeConfiguredIsStale(t *testing.T) {
	h := newGCHarness(t)
	h.keep[gcMirrorName("other")] = struct{}{}
	stale := h.mirror("returned-then-left")
	current := h.mirror("left-and-stayed-away")
	h.mark(stale, gcNow.Add(-2*mirrorGCGrace))
	h.mark(current, gcNow.Add(-2*mirrorGCGrace))
	// The first came back (and left again) after its mark was written; the second never did.
	h.lastConfigured = map[string]time.Time{stale: gcNow.Add(-mirrorGCGrace)}

	res := h.sweep()

	if !h.exists(stale, "HEAD") {
		t.Fatal("a mirror whose repository was configured again after the mark was written lost its grace period")
	}
	got, ok := h.markerTime(stale)
	if !ok || !got.Equal(gcNow) {
		t.Fatalf("stale mark not restarted: %v valid=%v", got, ok)
	}
	if h.exists(current) {
		t.Fatal("an overdue orphan that never came back was not removed")
	}
	if res.Marked != 1 || res.Removed != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestSweepDistrustsAClockBeforeTheSanityFloor(t *testing.T) {
	t.Run("a sweep with an implausible current time does nothing", func(t *testing.T) {
		h := newGCHarness(t)
		h.keep[gcMirrorName("other")] = struct{}{}
		m := h.mirror("repo-a")
		h.mark(m, time.Unix(0, 0).Add(-time.Hour)) // before the epoch: "overdue" under any clock
		h.now = time.Unix(0, 0).Add(10 * mirrorGCGrace)
		fresh := h.mirror("repo-b")

		res := h.sweep()

		if !h.exists(m, "HEAD") || h.exists(fresh, orphanMarkerName) || res != (sweepResult{}) {
			t.Fatalf("swept with the clock before the sanity floor (result %+v)", res)
		}
	})
	t.Run("a mark dated before the floor is not trusted", func(t *testing.T) {
		h := newGCHarness(t)
		h.keep[gcMirrorName("other")] = struct{}{}
		m := h.mirror("repo-a")
		h.mark(m, time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)) // written while the clock was wrong

		h.sweep()

		if !h.exists(m, "HEAD") {
			t.Fatal("removed a mirror on the strength of a mark dated before the sanity floor")
		}
		if got, ok := h.markerTime(m); !ok || !got.Equal(gcNow) {
			t.Fatalf("mark not restarted: %v valid=%v", got, ok)
		}
	})
}
