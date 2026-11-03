package gitsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// git runs the real git binary with an isolated environment and returns its trimmed stdout.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func refs(t *testing.T, gitDir string) []string {
	t.Helper()
	out := git(t, "", "--git-dir="+gitDir, "for-each-ref", "--format=%(refname)")
	if out == "" {
		return nil
	}
	r := strings.Split(out, "\n")
	slices.Sort(r)
	return r
}

// TestSyncRepoAgainstRealGit mirrors a real origin into a real replica and proves the behaviour the
// fakes only assume: every branch and tag arrives, hidden namespaces do not, a later change
// propagates, and a ref deleted at the origin is pruned from the replica.
func TestSyncRepoAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	replica := filepath.Join(root, "replica.git")
	work := filepath.Join(root, "work")
	for _, d := range []string{origin, replica} {
		git(t, "", "init", "--bare", "-b", "main", d)
	}
	git(t, "", "init", "-b", "main", work)

	commit := func(msg string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte(msg), 0o600); err != nil {
			t.Fatal(err)
		}
		git(t, work, "add", "file.txt")
		git(t, work, "commit", "-m", msg)
	}
	commit("one")
	git(t, work, "tag", "v1")
	git(t, work, "branch", "feature")
	git(t, work, "push", origin, "main", "feature", "v1")
	// A hidden namespace, as a forge keeps for pull requests: it must not reach the replica.
	git(t, "", "--git-dir="+origin, "update-ref", "refs/pull/1/head", git(t, work, "rev-parse", "HEAD"))

	d := syncDeps{
		mirrors: Mirrors{Root: filepath.Join(root, "mirrors")},
		run:     runGit,
		now:     time.Now,
	}
	task := syncTask{
		RepoID:       "repo-a",
		CanonicalURL: "file://" + origin,
		ReplicaURL:   "file://" + replica,
		PushToken:    Secret("push-token-abcdefghij"),
	}

	if got := syncRepo(context.Background(), d, task); got.State != StateOK {
		t.Fatalf("first sync = %+v, want ok", got)
	}
	want := []string{"refs/heads/feature", "refs/heads/main", "refs/tags/v1"}
	if got := refs(t, replica); !slices.Equal(got, want) {
		t.Fatalf("replica refs after first sync = %v, want %v (no refs/pull/*)", got, want)
	}
	if !d.mirrors.Exists("repo-a") {
		t.Fatal("the mirror directory was not created")
	}

	// The origin moves on: a new commit on main, and the feature branch deleted.
	commit("two")
	git(t, work, "push", origin, "main")
	git(t, "", "--git-dir="+origin, "branch", "-D", "feature")

	if got := syncRepo(context.Background(), d, task); got.State != StateOK {
		t.Fatalf("second sync = %+v, want ok", got)
	}
	if a, b := git(t, "", "--git-dir="+replica, "rev-parse", "main"), git(t, "", "--git-dir="+origin, "rev-parse", "main"); a != b {
		t.Errorf("replica main = %s, want origin main %s", a, b)
	}
	want = []string{"refs/heads/main", "refs/tags/v1"}
	if got := refs(t, replica); !slices.Equal(got, want) {
		t.Errorf("replica refs after the origin deleted a branch = %v, want %v", got, want)
	}
}

func TestSyncRepoAgainstRealGitMissingOriginIsAnError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	d := syncDeps{mirrors: Mirrors{Root: filepath.Join(root, "mirrors")}, run: runGit, now: time.Now}
	task := syncTask{
		RepoID:       "repo-a",
		CanonicalURL: "file://" + filepath.Join(root, "does-not-exist.git"),
		ReplicaURL:   "file://" + filepath.Join(root, "replica.git"),
		PushToken:    Secret("push-token-abcdefghij"),
	}

	got := syncRepo(context.Background(), d, task)
	if got.State != StateError || got.ErrorType == "" {
		t.Fatalf("result = %+v, want a typed error for an origin that does not exist", got)
	}
	if d.mirrors.Exists("repo-a") {
		t.Error("a failed clone must not leave a partial mirror behind")
	}
	if strings.Contains(got.ErrorDetail, root) {
		t.Errorf("error_detail leaks a local path: %q", got.ErrorDetail)
	}
}
