package gitsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	testOriginURL  = "https://git.example.com/group/repo-a"
	testReplicaURL = "https://replica.example.com/r/repo-a.git"
	testOriginTok  = "origin-token-abcdefghij"
	testPushTok    = "push-token-abcdefghij"
)

var syncNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// gitCall is one recorded git invocation.
type gitCall struct {
	argv []string
	env  []string
	dir  string
}

// sub returns which git subcommand the call is.
func (c gitCall) sub() string {
	for _, a := range c.argv {
		switch a {
		case "clone", "fetch", "push", "show-ref":
			return a
		}
	}
	return ""
}

// exitCode1 returns the error a process that exited with status 1 yields, which is how git
// show-ref reports "no matching refs".
func exitCode1(t *testing.T) error {
	t.Helper()
	err := exec.Command("false").Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("could not produce an exit-status-1 error: %v", err)
	}
	return err
}

func (c gitCall) envHas(sub string) bool {
	return slices.ContainsFunc(c.env, func(kv string) bool { return strings.Contains(kv, sub) })
}

// fakeGit records invocations and answers each from script, so a test controls git's outcome without
// a subprocess.
type fakeGit struct {
	calls  []gitCall
	script func(c gitCall) ([]byte, error)
}

func (f *fakeGit) run(_ context.Context, argv, env []string, dir string) ([]byte, error) {
	c := gitCall{argv: argv, env: env, dir: dir}
	f.calls = append(f.calls, c)
	if f.script == nil {
		return nil, nil
	}
	return f.script(c)
}

func (f *fakeGit) subs() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, c.sub())
	}
	return out
}

func newTestDeps(t *testing.T, f *fakeGit) syncDeps {
	t.Helper()
	return syncDeps{
		mirrors: Mirrors{Root: t.TempDir()},
		run:     f.run,
		now:     func() time.Time { return syncNow },
	}
}

func testTask() syncTask {
	return syncTask{
		RepoID:         "repo-a",
		CanonicalURL:   testOriginURL,
		Origin:         LocalEntry{BaseURL: "https://git.example.com", Auth: "bearer", Token: Secret(testOriginTok)},
		ReplicaURL:     testReplicaURL,
		PushToken:      Secret(testPushTok),
		TokenExpiresAt: "2030-01-01T00:00:00Z",
	}
}

var errExit = errors.New("exit status 128")

func TestSyncRepoCloneThenPushHappyPath(t *testing.T) {
	f := &fakeGit{}
	d := newTestDeps(t, f)
	got := syncRepo(context.Background(), d, testTask())

	if got.State != StateOK || got.ErrorType != "" {
		t.Fatalf("result = %+v, want ok", got)
	}
	if want := []string{"clone", "show-ref", "push"}; !slices.Equal(f.subs(), want) {
		t.Fatalf("git sequence = %v, want %v", f.subs(), want)
	}

	clone, push := f.calls[0], f.calls[2]
	if !slices.Contains(clone.argv, "--mirror") || !slices.Contains(clone.argv, testOriginURL) || !slices.Contains(clone.argv, d.mirrors.Dir("repo-a")) {
		t.Errorf("clone argv = %v, want --mirror <canonical origin URL> <mirror dir>", clone.argv)
	}
	if !slices.Contains(push.argv, testReplicaURL) || !slices.Contains(push.argv, "--prune") {
		t.Errorf("push argv = %v, want --prune and the replica URL", push.argv)
	}

	// Each credential is scoped to its own URL and appears only on its own invocation.
	if !clone.envHas("http."+testOriginURL+"/.extraHeader") || !clone.envHas("Bearer "+testOriginTok) {
		t.Error("clone must carry the origin credential scoped to the origin URL")
	}
	if clone.envHas(testPushTok) {
		t.Error("the push token must never reach the origin clone")
	}
	if !push.envHas("http."+testReplicaURL+"/.extraHeader") || !push.envHas("Bearer "+testPushTok) {
		t.Error("push must carry the push token scoped to the replica URL")
	}
	if push.envHas(testOriginTok) {
		t.Error("the origin credential must never reach the replica push")
	}
	for _, c := range f.calls {
		for _, a := range c.argv {
			if strings.Contains(a, testOriginTok) || strings.Contains(a, testPushTok) {
				t.Errorf("a credential leaked into argv: %v", c.argv)
			}
		}
	}
}

// An origin that advertises no branches or tags (an empty repository, or a transient bad
// advertisement) must never be pushed with --prune: that would delete every branch and tag on the
// replica.
func TestSyncRepoOriginWithNoRefsDoesNotPushOrPrune(t *testing.T) {
	f := &fakeGit{script: func(c gitCall) ([]byte, error) {
		if c.sub() == "show-ref" {
			return nil, exitCode1(t)
		}
		return nil, nil
	}}
	got := syncRepo(context.Background(), newTestDeps(t, f), testTask())

	if got.State != StateError || got.ErrorType != ErrUnclassified || !strings.Contains(got.ErrorDetail, "no branches or tags") {
		t.Fatalf("result = %+v, want error/unclassified naming the empty origin", got)
	}
	if slices.Contains(f.subs(), "push") {
		t.Errorf("nothing may be pushed when the origin has no refs; sequence = %v", f.subs())
	}
}

// A show-ref failure that is not "no refs" (exit 1) says nothing about the origin; it must not be
// mistaken for an empty one, and must not push either.
func TestSyncRepoRefCheckFailureIsNotAnEmptyOrigin(t *testing.T) {
	f := &fakeGit{script: func(c gitCall) ([]byte, error) {
		if c.sub() == "show-ref" {
			return []byte("fatal: not a git repository"), errExit
		}
		return nil, nil
	}}
	got := syncRepo(context.Background(), newTestDeps(t, f), testTask())

	if got.State != StateError || strings.Contains(got.ErrorDetail, "no branches or tags") {
		t.Fatalf("result = %+v, want an error that does not claim the origin is empty", got)
	}
	if slices.Contains(f.subs(), "push") {
		t.Errorf("no push after a failed ref check; sequence = %v", f.subs())
	}
}

func TestSyncRepoPushesOnlyBranchesAndTags(t *testing.T) {
	f := &fakeGit{}
	syncRepo(context.Background(), newTestDeps(t, f), testTask())

	push := f.calls[len(f.calls)-1]
	if push.sub() != "push" {
		t.Fatalf("last call = %v, want push", push.argv)
	}
	for _, want := range []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"} {
		if !slices.Contains(push.argv, want) {
			t.Errorf("push argv %v missing refspec %q", push.argv, want)
		}
	}
	if slices.Contains(push.argv, "--mirror") {
		t.Error("push must not use --mirror: it would push hidden namespaces such as refs/pull/*")
	}
}

func TestSyncRepoFetchesWhenMirrorExists(t *testing.T) {
	f := &fakeGit{}
	d := newTestDeps(t, f)
	if err := os.MkdirAll(d.mirrors.Dir("repo-a"), 0o700); err != nil {
		t.Fatal(err)
	}

	got := syncRepo(context.Background(), d, testTask())
	if got.State != StateOK {
		t.Fatalf("result = %+v, want ok", got)
	}
	if want := []string{"fetch", "show-ref", "push"}; !slices.Equal(f.subs(), want) {
		t.Fatalf("git sequence = %v, want %v", f.subs(), want)
	}
	fetch := f.calls[0]
	if !slices.Contains(fetch.argv, "--prune") || !slices.Contains(fetch.argv, testOriginURL) || !slices.Contains(fetch.argv, "--git-dir="+d.mirrors.Dir("repo-a")) {
		t.Errorf("fetch argv = %v, want --prune, the canonical origin URL and the mirror's --git-dir", fetch.argv)
	}
}

func TestSyncRepoOriginDeniedStops(t *testing.T) {
	f := &fakeGit{script: func(c gitCall) ([]byte, error) {
		return []byte("fatal: unable to access 'https://git.example.com/group/repo-a/': The requested URL returned error: 403"), errExit
	}}
	got := syncRepo(context.Background(), newTestDeps(t, f), testTask())

	if got.State != StateError || got.ErrorType != ErrOriginAccessDenied {
		t.Fatalf("result = %+v, want error/origin_access_denied", got)
	}
	if slices.Contains(f.subs(), "push") {
		t.Errorf("no push may be attempted after an origin failure; sequence = %v", f.subs())
	}
}

func TestSyncRepoExpiredTokenSkipsPush(t *testing.T) {
	f := &fakeGit{}
	task := testTask()
	task.TokenExpiresAt = syncNow.Add(-time.Minute).Format(time.RFC3339)

	got := syncRepo(context.Background(), newTestDeps(t, f), task)
	if got.State != StateError || got.ErrorType != ErrPushTokenExpired {
		t.Fatalf("result = %+v, want error/push_token_expired", got)
	}
	if len(f.calls) != 0 {
		t.Errorf("an expired token must short-circuit before any git runs; ran %v", f.subs())
	}
}

func TestSyncRepoUnknownExpiryIsNotTreatedAsExpired(t *testing.T) {
	f := &fakeGit{}
	task := testTask()
	task.TokenExpiresAt = ""
	if got := syncRepo(context.Background(), newTestDeps(t, f), task); got.State != StateOK {
		t.Errorf("result = %+v, want ok when the expiry is unknown", got)
	}
}

func TestSyncRepoReplicaAuthRejectedVersusExpired(t *testing.T) {
	deny := func(c gitCall) ([]byte, error) {
		if c.sub() == "push" {
			return []byte("fatal: unable to access 'https://replica.example.com/r/repo-a.git/': The requested URL returned error: 401"), errExit
		}
		return nil, nil
	}
	f := &fakeGit{script: deny}
	got := syncRepo(context.Background(), newTestDeps(t, f), testTask())
	if got.ErrorType != ErrCredentialRejected {
		t.Errorf("a 401 with an unexpired token = %q, want credential_rejected", got.ErrorType)
	}
}

// A long clone can outlive the token: valid when the sync started, expired by the time the push is
// refused, so the 401 must read as an expired token rather than a rejected credential.
func TestSyncRepoTokenExpiringDuringCloneIsReportedExpired(t *testing.T) {
	clock := syncNow
	f := &fakeGit{}
	f.script = func(c gitCall) ([]byte, error) {
		switch c.sub() {
		case "clone":
			clock = clock.Add(2 * time.Hour) // the clone ran past the token's expiry
		case "push":
			return []byte("fatal: unable to access 'https://replica.example.com/r/repo-a.git/': The requested URL returned error: 401"), errExit
		}
		return nil, nil
	}
	d := newTestDeps(t, f)
	d.now = func() time.Time { return clock }
	task := testTask()
	task.TokenExpiresAt = syncNow.Add(time.Hour).Format(time.RFC3339)

	got := syncRepo(context.Background(), d, task)
	if got.State != StateError || got.ErrorType != ErrPushTokenExpired {
		t.Errorf("result = %+v, want error/push_token_expired", got)
	}
}

func TestSyncRepoCorruptMirrorReclones(t *testing.T) {
	d := newTestDeps(t, nil)
	dir := d.mirrors.Dir("repo-a")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeGit{}
	f.script = func(c gitCall) ([]byte, error) {
		switch c.sub() {
		case "fetch":
			return []byte("fatal: not a git repository: '" + dir + "'"), errExit
		case "clone":
			if _, err := os.Stat(dir); err == nil {
				t.Error("the corrupt mirror must be removed before the reclone")
			}
		}
		return nil, nil
	}
	d.run = f.run

	got := syncRepo(context.Background(), d, testTask())
	if got.State != StateOK {
		t.Fatalf("result = %+v, want ok after one reclone", got)
	}
	if want := []string{"fetch", "clone", "show-ref", "push"}; !slices.Equal(f.subs(), want) {
		t.Errorf("git sequence = %v, want %v", f.subs(), want)
	}
}

func TestSyncRepoCorruptMirrorRecloneFailureIsACleanError(t *testing.T) {
	d := newTestDeps(t, nil)
	if err := os.MkdirAll(d.mirrors.Dir("repo-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fakeGit{}
	f.script = func(c gitCall) ([]byte, error) {
		if c.sub() == "fetch" {
			return []byte("fatal: not a git repository"), errExit
		}
		return []byte("fatal: unable to access 'https://git.example.com/group/repo-a/': The requested URL returned error: 403"), errExit
	}
	d.run = f.run

	got := syncRepo(context.Background(), d, testTask())
	if got.State != StateError || got.ErrorType != ErrOriginAccessDenied {
		t.Fatalf("result = %+v, want error/origin_access_denied", got)
	}
	if want := []string{"fetch", "clone"}; !slices.Equal(f.subs(), want) {
		t.Errorf("git sequence = %v, want exactly one reclone attempt: %v", f.subs(), want)
	}
}

func TestSyncRepoNonCorruptFetchFailureDoesNotReclone(t *testing.T) {
	d := newTestDeps(t, nil)
	if err := os.MkdirAll(d.mirrors.Dir("repo-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fakeGit{script: func(c gitCall) ([]byte, error) {
		return []byte("fatal: unable to access 'https://git.example.com/group/repo-a/': Could not resolve host: git.example.com"), errExit
	}}
	d.run = f.run

	got := syncRepo(context.Background(), d, testTask())
	if got.ErrorType != ErrOriginUnreachable {
		t.Fatalf("result = %+v, want origin_unreachable", got)
	}
	if want := []string{"fetch"}; !slices.Equal(f.subs(), want) {
		t.Errorf("an unreachable origin must not trigger a reclone; sequence = %v", f.subs())
	}
}

func TestSyncRepoPushDeadlineIsPushTimeout(t *testing.T) {
	f := &fakeGit{script: func(c gitCall) ([]byte, error) {
		if c.sub() == "push" {
			return nil, fmt.Errorf("push timed out: %w", context.DeadlineExceeded)
		}
		return nil, nil
	}}
	got := syncRepo(context.Background(), newTestDeps(t, f), testTask())
	if got.ErrorType != ErrPushTimeout {
		t.Errorf("result = %+v, want push_timeout", got)
	}
}

// A real push timeout surfaces from git as a killed process, not a context error; syncRepo must
// attribute it to the deadline rather than letting it read as an out-of-memory kill.
func TestSyncRepoPushRunningPastItsDeadlineIsPushTimeout(t *testing.T) {
	d := newTestDeps(t, &fakeGit{})
	d.pushTimeout = 20 * time.Millisecond
	d.run = func(ctx context.Context, argv, env []string, dir string) ([]byte, error) {
		if (gitCall{argv: argv}).sub() == "push" {
			<-ctx.Done()
			return nil, errors.New("signal: killed")
		}
		return nil, nil
	}

	got := syncRepo(context.Background(), d, testTask())
	if got.State != StateError || got.ErrorType != ErrPushTimeout {
		t.Errorf("result = %+v, want error/push_timeout", got)
	}
}

func TestSyncRepoFetchRunningPastItsDeadlineIsOriginUnreachable(t *testing.T) {
	d := newTestDeps(t, &fakeGit{})
	d.fetchTimeout = 20 * time.Millisecond
	d.run = func(ctx context.Context, argv, env []string, dir string) ([]byte, error) {
		<-ctx.Done()
		return nil, errors.New("signal: killed")
	}

	got := syncRepo(context.Background(), d, testTask())
	if got.State != StateError || got.ErrorType != ErrOriginUnreachable {
		t.Errorf("result = %+v, want error/origin_unreachable", got)
	}
}

// Shutdown cancels the context; that is not a failure of the repository and must not be recorded as
// one (a SIGKILL from the cancel would otherwise read as an out-of-memory kill).
func TestSyncRepoCancellationIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := newTestDeps(t, &fakeGit{})
	d.run = func(ctx context.Context, argv, env []string, dir string) ([]byte, error) {
		cancel()
		return nil, errors.New("signal: killed")
	}

	got := syncRepo(ctx, d, testTask())
	if got.State != StatePending || got.ErrorType != "" {
		t.Errorf("result = %+v, want pending with no error", got)
	}
}
