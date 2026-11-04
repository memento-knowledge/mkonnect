package gitsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// statsFixture is a real origin, replica and mirrors root, with statistics collection on.
type statsFixture struct {
	t                     *testing.T
	root, origin, replica string
	work                  string
	deps                  syncDeps
	task                  syncTask
}

func newStatsFixture(t *testing.T) *statsFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	f := &statsFixture{t: t, root: root, origin: filepath.Join(root, "origin.git"), replica: filepath.Join(root, "replica.git"), work: filepath.Join(root, "work")}
	for _, d := range []string{f.origin, f.replica} {
		git(t, "", "init", "--bare", "-b", "trunk", d)
	}
	git(t, "", "init", "-b", "trunk", f.work)
	f.deps = syncDeps{mirrors: Mirrors{Root: filepath.Join(root, "mirrors")}, run: runGit, runOut: runGitOut, now: time.Now}
	f.task = syncTask{RepoID: "repo-a", CanonicalURL: "file://" + f.origin, ReplicaURL: "file://" + f.replica, PushToken: Secret("push-token-abcdefghij")}
	return f
}

// commitAt makes a commit whose committer date is when.
func (f *statsFixture) commitAt(when time.Time) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.work, "file.txt"), []byte(when.String()), 0o600); err != nil {
		f.t.Fatal(err)
	}
	git(f.t, f.work, "add", "file.txt")
	stamp := when.Format(time.RFC3339)
	cmd := exec.Command("git", "commit", "-m", "c", "--date="+stamp)
	cmd.Dir = f.work
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + f.t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_COMMITTER_DATE=" + stamp,
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("commit: %v\n%s", err, out)
	}
}

// tagAt makes an annotated tag whose own (tagger) date is when.
func (f *statsFixture) tagAt(name string, when time.Time, target string) {
	f.t.Helper()
	cmd := exec.Command("git", "tag", "-a", "-m", "t", name, target)
	cmd.Dir = f.work
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + f.t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com", "GIT_COMMITTER_DATE=" + when.Format(time.RFC3339),
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=advice.nestedTag", "GIT_CONFIG_VALUE_0=false",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("tag: %v\n%s", err, out)
	}
}

func (f *statsFixture) pushOrigin(refs ...string) {
	f.t.Helper()
	git(f.t, f.work, append([]string{"push", f.origin}, refs...)...)
}

func TestSyncRepoAgainstRealGitReportsStatistics(t *testing.T) {
	f := newStatsFixture(t)
	jan2020 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	jan2021 := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	jan2030 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	f.commitAt(jan2020)
	// An annotated tag made far in the future of the code it names, and a nested one: the tag
	// objects' own dates must not be taken for the age of the code.
	f.tagAt("v1", jan2030, "HEAD")
	f.tagAt("v1-nested", jan2030, "v1")
	git(t, f.work, "tag", "light")
	git(t, f.work, "branch", "feature")
	f.pushOrigin("trunk", "feature", "v1", "v1-nested", "light")

	got := syncRepo(context.Background(), f.deps, f.task)
	if got.State != StateOK {
		t.Fatalf("first sync = %+v, want ok", got)
	}
	if got.Stats.RefsPushed == nil || *got.Stats.RefsPushed != 5 {
		t.Errorf("first sync RefsPushed = %v, want 5 new refs", got.Stats.RefsPushed)
	}
	if !got.Stats.NewestCommitAt.Equal(jan2020) {
		t.Errorf("NewestCommitAt = %v, want %v: an annotated tag's own date (2030) must not win", got.Stats.NewestCommitAt, jan2020)
	}
	if got.Stats.DefaultBranch != "trunk" {
		t.Errorf("DefaultBranch = %q, want trunk", got.Stats.DefaultBranch)
	}

	// Nothing changed: still a success, and nothing was pushed.
	got = syncRepo(context.Background(), f.deps, f.task)
	if got.State != StateOK || got.Stats.RefsPushed == nil || *got.Stats.RefsPushed != 0 {
		t.Errorf("no-op sync = %+v, want ok with refs_pushed 0", got)
	}

	// A newer commit on a branch moves newest forward and is a fast-forward; a branch deleted at the
	// origin is a deletion.
	f.commitAt(jan2021)
	f.pushOrigin("trunk")
	git(t, "", "--git-dir="+f.origin, "branch", "-D", "feature")
	got = syncRepo(context.Background(), f.deps, f.task)
	if got.State != StateOK || got.Stats.RefsPushed == nil || *got.Stats.RefsPushed != 2 {
		t.Errorf("third sync = %+v, want ok with refs_pushed 2 (one fast-forward, one deletion)", got)
	}
	if !got.Stats.NewestCommitAt.Equal(jan2021) {
		t.Errorf("NewestCommitAt = %v, want %v", got.Stats.NewestCommitAt, jan2021)
	}

	// The origin's default branch changes.
	git(t, "", "--git-dir="+f.origin, "symbolic-ref", "HEAD", "refs/heads/other")
	git(t, f.work, "branch", "other")
	f.pushOrigin("other")
	got = syncRepo(context.Background(), f.deps, f.task)
	if got.Stats.DefaultBranch != "other" {
		t.Errorf("DefaultBranch = %q, want other", got.Stats.DefaultBranch)
	}
}

// A tag of a tree names no commit, and a mirror with only such a tag besides a branch still reports
// the branch's time.
func TestSyncRepoAgainstRealGitIgnoresTagsOfNonCommits(t *testing.T) {
	f := newStatsFixture(t)
	jan2020 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	f.commitAt(jan2020)
	f.tagAt("tree-tag", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), "HEAD^{tree}")
	f.pushOrigin("trunk", "tree-tag")

	got := syncRepo(context.Background(), f.deps, f.task)
	if got.State != StateOK || !got.Stats.NewestCommitAt.Equal(jan2020) {
		t.Errorf("result = %+v, want ok with newest %v", got, jan2020)
	}
}

// A real replica hook that refuses the push proves the whole porcelain path against git itself: the
// exit status, the rejection moved to stdout, and the classification that depends on it.
func TestSyncRepoAgainstRealGitReplicaRejectionIsClassified(t *testing.T) {
	f := newStatsFixture(t)
	f.commitAt(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	f.pushOrigin("trunk")
	hook := "#!/bin/sh\necho 'repository size limit exceeded' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(f.replica, "hooks", "pre-receive"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}

	got := syncRepo(context.Background(), f.deps, f.task)
	if got.State != StateError || got.ErrorType != ErrReplicaRejected {
		t.Fatalf("result = %+v, want error/replica_rejected", got)
	}
	if got.Stats.RefsPushed == nil || *got.Stats.RefsPushed != 0 {
		t.Errorf("RefsPushed = %v, want 0: the replica refused every ref", got.Stats.RefsPushed)
	}
	// The origin was reachable, so what it said about itself is still recorded.
	if got.Stats.DefaultBranch != "trunk" || got.Stats.NewestCommitAt.IsZero() {
		t.Errorf("stats = %+v, want the origin facts learned before the push failed", got.Stats)
	}

	// The same failure with no replica hook but a refused (non-existent) location classifies too, so a
	// failed push that never produced a porcelain report is handled as before.
	f.task.ReplicaURL = "file://" + filepath.Join(f.root, "nope.git")
	if got := syncRepo(context.Background(), f.deps, f.task); got.State != StateError {
		t.Errorf("push to a missing replica = %+v, want an error", got)
	}
}
