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
	return gitStdin(t, dir, "", args...)
}

// gitStdin is git with the given standard input (for example `update-ref --stdin`).
func gitStdin(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
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
// fakes only assume: every branch and tag arrives, hidden namespaces reach neither the local mirror
// nor the replica, a later change propagates, and a ref deleted at the origin is pruned from both.
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
	// The mirror holds only what is pushed: a forge's hidden namespaces would only cost disk.
	if got := refs(t, d.mirrors.Dir("repo-a")); !slices.Equal(got, want) {
		t.Fatalf("mirror refs after first sync = %v, want %v (no refs/pull/*)", got, want)
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
	if got := refs(t, d.mirrors.Dir("repo-a")); !slices.Equal(got, want) {
		t.Errorf("mirror refs after the origin deleted a branch = %v, want %v", got, want)
	}
}

// Mirrors created before the mirror was narrowed to branches and tags came from `clone --mirror` and
// hold every namespace. They must keep syncing, and the narrower fetch must neither fail on them nor
// delete what they hold: the extra refs stay on disk until the mirror is rebuilt.
func TestSyncRepoAgainstRealGitReusesAMirrorMadeByCloneMirror(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "file.txt")
	git(t, work, "commit", "-m", "one")
	git(t, work, "tag", "v1")
	git(t, work, "push", origin, "main", "v1")
	head := git(t, work, "rev-parse", "HEAD")
	git(t, "", "--git-dir="+origin, "update-ref", "refs/pull/1/head", head)

	d := syncDeps{mirrors: Mirrors{Root: filepath.Join(root, "mirrors")}, run: runGit, now: time.Now}
	if err := d.mirrors.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	git(t, "", "clone", "--mirror", "file://"+origin, d.mirrors.Dir("repo-a"))
	legacy := []string{"refs/heads/main", "refs/pull/1/head", "refs/tags/v1"}
	if got := refs(t, d.mirrors.Dir("repo-a")); !slices.Equal(got, legacy) {
		t.Fatalf("fixture mirror refs = %v, want %v", got, legacy)
	}

	task := syncTask{
		RepoID: "repo-a", CanonicalURL: "file://" + origin, ReplicaURL: "file://" + replica,
		PushToken: Secret("push-token-abcdefghij"),
	}
	// The origin moves on and drops the hidden ref; the old mirror's copy of it is not pruned.
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "commit", "-am", "two")
	git(t, work, "push", origin, "main")
	git(t, "", "--git-dir="+origin, "update-ref", "-d", "refs/pull/1/head")

	if got := syncRepo(context.Background(), d, task); got.State != StateOK {
		t.Fatalf("sync of a legacy mirror = %+v, want ok", got)
	}
	if got := refs(t, replica); !slices.Equal(got, []string{"refs/heads/main", "refs/tags/v1"}) {
		t.Errorf("replica refs = %v, want branches and tags only", got)
	}
	if a, b := git(t, "", "--git-dir="+d.mirrors.Dir("repo-a"), "rev-parse", "main"), git(t, "", "--git-dir="+origin, "rev-parse", "main"); a != b {
		t.Errorf("mirror main = %s, want origin main %s", a, b)
	}
	if got := refs(t, d.mirrors.Dir("repo-a")); !slices.Equal(got, legacy) {
		t.Errorf("legacy mirror refs after sync = %v, want the extra namespace left in place: %v", got, legacy)
	}
}

// TestSyncRepoAgainstRealGitEmptyOriginNeverWipesReplica reproduces the data-loss case: once an
// origin advertises no branches or tags, `fetch --prune` empties the mirror (and exits 0), and a
// `push --prune` from it would delete everything on the replica. Both a previously-synced origin
// that lost its refs and an origin that is empty on the very first sync must leave the replica as it
// was and report an error.
func TestSyncRepoAgainstRealGitEmptyOriginNeverWipesReplica(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "file.txt")
	git(t, work, "commit", "-m", "one")
	git(t, work, "tag", "v1")
	git(t, work, "push", origin, "main", "v1")

	d := syncDeps{mirrors: Mirrors{Root: filepath.Join(root, "mirrors")}, run: runGit, now: time.Now}
	task := syncTask{
		RepoID: "repo-a", CanonicalURL: "file://" + origin, ReplicaURL: "file://" + replica,
		PushToken: Secret("push-token-abcdefghij"),
	}
	populated := []string{"refs/heads/main", "refs/tags/v1"}

	if got := syncRepo(context.Background(), d, task); got.State != StateOK {
		t.Fatalf("first sync = %+v, want ok", got)
	}
	if got := refs(t, replica); !slices.Equal(got, populated) {
		t.Fatalf("replica refs = %v, want %v", got, populated)
	}

	// The origin loses every ref (a restore, a permission change, a bad proxy...).
	git(t, "", "--git-dir="+origin, "update-ref", "-d", "refs/heads/main")
	git(t, "", "--git-dir="+origin, "update-ref", "-d", "refs/tags/v1")

	got := syncRepo(context.Background(), d, task)
	if got.State != StateError || got.ErrorType != ErrOriginEmpty || got.ErrorDetail != detailOriginEmpty {
		t.Errorf("sync of an emptied origin = %+v, want an error naming the empty origin", got)
	}
	if refs := refs(t, replica); !slices.Equal(refs, populated) {
		t.Errorf("replica refs after the origin was emptied = %v, want them untouched: %v", refs, populated)
	}

	// An origin that is empty on the very first sync, against a replica that already has refs.
	task.RepoID = "repo-b"
	if got := syncRepo(context.Background(), d, task); got.State != StateError || got.ErrorType != ErrOriginEmpty || got.ErrorDetail != detailOriginEmpty {
		t.Errorf("first sync of an empty origin = %+v, want an error naming the empty origin", got)
	}
	if refs := refs(t, replica); !slices.Equal(refs, populated) {
		t.Errorf("replica refs after syncing an empty origin = %v, want them untouched: %v", refs, populated)
	}

	// An initialised but empty mirror, as a crash between init and the first fetch would leave, must
	// not be taken for a healthy one: against an empty origin it is still the empty-origin error.
	task.RepoID = "repo-c"
	git(t, "", "init", "--bare", "-b", "main", d.mirrors.Dir("repo-c"))
	if got := syncRepo(context.Background(), d, task); got.State != StateError || got.ErrorType != ErrOriginEmpty || got.ErrorDetail != detailOriginEmpty {
		t.Errorf("sync with a leftover empty mirror = %+v, want an error naming the empty origin", got)
	}
	if refs := refs(t, replica); !slices.Equal(refs, populated) {
		t.Errorf("replica refs after syncing with a leftover empty mirror = %v, want them untouched: %v", refs, populated)
	}
}

// An origin without a branch or tag is origin_empty, with the replica untouched and nothing pushed;
// once the origin has a first commit the same repository syncs normally on its next run, from the same
// mirror, and the replica is populated. The error is never sticky.
func TestSyncRepoAgainstRealGitEmptyOriginThenFirstCommitSyncs(t *testing.T) {
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
	d := syncDeps{mirrors: Mirrors{Root: filepath.Join(root, "mirrors")}, run: runGit, runOut: runGitOut, now: time.Now}
	task := syncTask{
		RepoID: "repo-a", CanonicalURL: "file://" + origin, ReplicaURL: "file://" + replica,
		PushToken: Secret("push-token-abcdefghij"),
	}

	// A branch only the replica has: it proves the empty attempt pushed and pruned nothing, and it is
	// the one the first real sync removes.
	git(t, "", "init", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "file.txt")
	git(t, work, "commit", "-m", "one")
	git(t, work, "push", replica, "main:refs/heads/replica-only")

	got := syncRepo(context.Background(), d, task)
	if got.State != StateError || got.ErrorType != ErrOriginEmpty || got.ErrorDetail != detailOriginEmpty {
		t.Fatalf("sync of an empty origin = %+v, want error/%s with its fixed hint", got, ErrOriginEmpty)
	}
	if got.Stats.RefsPushed == nil || *got.Stats.RefsPushed != 0 || got.Stats.DefaultBranch != "" || !got.Stats.NewestCommitAt.IsZero() {
		t.Errorf("stats = %+v, want 0 refs pushed and nothing learned about the origin", got.Stats)
	}
	if r := refs(t, replica); !slices.Equal(r, []string{"refs/heads/replica-only"}) {
		t.Fatalf("replica refs = %v, want only the replica-only branch, untouched", r)
	}

	git(t, work, "push", origin, "main")

	got = syncRepo(context.Background(), d, task)
	if got.State != StateOK || got.ErrorType != "" || got.ErrorDetail != "" {
		t.Fatalf("sync after the first commit = %+v, want ok", got)
	}
	if r := refs(t, replica); !slices.Equal(r, []string{"refs/heads/main"}) {
		t.Errorf("replica refs = %v, want main only: the replica-only branch is pruned like any branch the origin lacks", r)
	}
	if a, b := git(t, "", "--git-dir="+replica, "rev-parse", "main"), git(t, "", "--git-dir="+origin, "rev-parse", "main"); a != b {
		t.Errorf("replica main = %s, want the origin's %s", a, b)
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
		t.Error("a failed first fetch must not leave an initialised, empty mirror behind")
	}
	if strings.Contains(got.ErrorDetail, root) {
		t.Errorf("error_detail leaks a local path: %q", got.ErrorDetail)
	}
}
