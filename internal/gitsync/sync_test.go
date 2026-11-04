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
		case "init", "fetch", "push", "show-ref":
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

// wantFetchRefspecs are the only refs the mirror is fetched with: the namespaces that are pushed.
var wantFetchRefspecs = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}

// assertMirrorFetch checks a fetch call dials exactly the canonical origin URL, with the mirror as its
// --git-dir, bounded to branches and tags, and with the origin credential scoped to that URL.
func assertMirrorFetch(t *testing.T, fetch gitCall, mirrorDir string) {
	t.Helper()
	for _, want := range []string{"--git-dir=" + mirrorDir, "--prune", "--force"} {
		if !slices.Contains(fetch.argv, want) {
			t.Errorf("fetch argv = %v, want %q", fetch.argv, want)
		}
	}
	// "--" ends option parsing, then the canonical URL, then exactly the two refspecs.
	i := slices.Index(fetch.argv, "--")
	if i < 0 || !slices.Equal(fetch.argv[i+1:], append([]string{testOriginURL}, wantFetchRefspecs...)) {
		t.Errorf("fetch argv = %v, want `-- <canonical origin URL> %v` at the end", fetch.argv, wantFetchRefspecs)
	}
	if slices.Contains(fetch.argv, "+refs/*:refs/*") {
		t.Errorf("fetch argv = %v: the mirror must not fetch every namespace", fetch.argv)
	}
	if !fetch.envHas("http."+testOriginURL+"/.extraHeader") || !fetch.envHas("Bearer "+testOriginTok) {
		t.Error("fetch must carry the origin credential scoped to the origin URL")
	}
	if fetch.envHas(testPushTok) {
		t.Error("the push token must never reach the origin fetch")
	}
}

func TestSyncRepoInitsMirrorThenFetchesThenPushes(t *testing.T) {
	f := &fakeGit{}
	d := newTestDeps(t, f)
	got := syncRepo(context.Background(), d, testTask())

	if got.State != StateOK || got.ErrorType != "" {
		t.Fatalf("result = %+v, want ok", got)
	}
	if want := []string{"init", "fetch", "show-ref", "push"}; !slices.Equal(f.subs(), want) {
		t.Fatalf("git sequence = %v, want %v", f.subs(), want)
	}

	initCall, fetch, push := f.calls[0], f.calls[1], f.calls[3]
	dir := d.mirrors.Dir("repo-a")
	// The initial branch is explicit so the result does not depend on the git version or on
	// init.defaultBranch, and the init never touches the network or carries a credential.
	for _, want := range []string{"--bare", "--initial-branch=main"} {
		if !slices.Contains(initCall.argv, want) {
			t.Errorf("init argv = %v, want %q", initCall.argv, want)
		}
	}
	if n := len(initCall.argv); n < 2 || initCall.argv[n-2] != "--" || initCall.argv[n-1] != dir {
		t.Errorf("init argv = %v, want it to end `-- <mirror dir>`", initCall.argv)
	}
	if initCall.envHas(testOriginTok) || initCall.envHas(testPushTok) || initCall.envHas("extraHeader") {
		t.Error("git init talks to no remote and must carry no credential")
	}
	assertMirrorFetch(t, fetch, dir)

	if !slices.Contains(push.argv, testReplicaURL) || !slices.Contains(push.argv, "--prune") {
		t.Errorf("push argv = %v, want --prune and the replica URL", push.argv)
	}
	// Each credential is scoped to its own URL and appears only on its own invocation.
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
	assertMirrorFetch(t, f.calls[0], d.mirrors.Dir("repo-a"))
}

// A failed first fetch leaves an initialised but empty repository. It must not survive to be mistaken
// for a healthy mirror, so it is removed and the next attempt starts from init again.
func TestSyncRepoFailedFirstFetchRemovesTheInitialisedMirror(t *testing.T) {
	d := newTestDeps(t, nil)
	dir := d.mirrors.Dir("repo-a")
	f := &fakeGit{script: func(c gitCall) ([]byte, error) {
		switch c.sub() {
		case "init":
			return nil, os.MkdirAll(dir, 0o700) // what a real git init leaves behind
		case "fetch":
			return []byte("fatal: unable to access 'https://git.example.com/group/repo-a/': Could not resolve host: git.example.com"), errExit
		}
		return nil, nil
	}}
	d.run = f.run

	got := syncRepo(context.Background(), d, testTask())
	if got.State != StateError || got.ErrorType != ErrOriginUnreachable {
		t.Fatalf("result = %+v, want error/origin_unreachable", got)
	}
	if want := []string{"init", "fetch"}; !slices.Equal(f.subs(), want) {
		t.Errorf("git sequence = %v, want %v", f.subs(), want)
	}
	if d.mirrors.Exists("repo-a") {
		t.Error("an initialised mirror whose first fetch failed must be removed")
	}
}

func TestSyncRepoInitFailureIsClassifiedAndStops(t *testing.T) {
	d := newTestDeps(t, nil)
	dir := d.mirrors.Dir("repo-a")
	f := &fakeGit{script: func(c gitCall) ([]byte, error) {
		if c.sub() == "init" {
			// A real init that runs out of disk part-way leaves the directory behind.
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
			return []byte("fatal: cannot mkdir /var/mirrors/abc.git/objects: No space left on device"), errExit
		}
		return nil, nil
	}}
	d.run = f.run
	got := syncRepo(context.Background(), d, testTask())

	if got.State != StateError || got.ErrorType != ErrLocalDiskExhausted {
		t.Fatalf("result = %+v, want error/local_disk_exhausted", got)
	}
	if want := []string{"init"}; !slices.Equal(f.subs(), want) {
		t.Errorf("git sequence = %v: nothing may run after a failed init", f.subs())
	}
	if d.mirrors.Exists("repo-a") {
		t.Error("a failed init must not leave a partial mirror behind")
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

// A long fetch can outlive the token: valid when the sync started, expired by the time the fetch is
// done. The connector knows the token is expired, so it must not push with it — even if the replica
// would (wrongly, or by clock skew) still accept it, in which case the sync would report ok having
// used a credential the platform has declared dead.
func TestSyncRepoTokenExpiringDuringFetchIsNotUsedToPush(t *testing.T) {
	clock := syncNow
	f := &fakeGit{}
	f.script = func(c gitCall) ([]byte, error) {
		if c.sub() == "fetch" {
			clock = clock.Add(2 * time.Hour) // the fetch ran past the token's expiry
		}
		return nil, nil // a replica that would happily accept the push
	}
	d := newTestDeps(t, f)
	d.now = func() time.Time { return clock }
	task := testTask()
	task.TokenExpiresAt = syncNow.Add(time.Hour).Format(time.RFC3339)

	got := syncRepo(context.Background(), d, task)
	if got.State != StateError || got.ErrorType != ErrPushTokenExpired {
		t.Fatalf("result = %+v, want error/push_token_expired", got)
	}
	if slices.Contains(f.subs(), "push") {
		t.Errorf("a push was run with a token known to be expired; sequence = %v", f.subs())
	}
}

// A token that is valid when the push starts but expires while it runs is the replica's to judge:
// when it refuses, the 401 reads as an expired token rather than a rejected credential.
func TestSyncRepoTokenExpiringDuringPushIsReportedExpired(t *testing.T) {
	clock := syncNow
	f := &fakeGit{}
	f.script = func(c gitCall) ([]byte, error) {
		if c.sub() == "push" {
			clock = clock.Add(2 * time.Hour) // the push ran past the token's expiry
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

func TestSyncRepoCorruptMirrorIsRebuilt(t *testing.T) {
	d := newTestDeps(t, nil)
	dir := d.mirrors.Dir("repo-a")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeGit{}
	fetches := 0
	f.script = func(c gitCall) ([]byte, error) {
		switch c.sub() {
		case "fetch":
			if fetches++; fetches == 1 {
				return []byte("fatal: not a git repository: '" + dir + "'"), errExit
			}
		case "init":
			if _, err := os.Stat(dir); err == nil {
				t.Error("the corrupt mirror must be removed before it is rebuilt")
			}
		}
		return nil, nil
	}
	d.run = f.run

	got := syncRepo(context.Background(), d, testTask())
	if got.State != StateOK {
		t.Fatalf("result = %+v, want ok after one rebuild", got)
	}
	if want := []string{"fetch", "init", "fetch", "show-ref", "push"}; !slices.Equal(f.subs(), want) {
		t.Errorf("git sequence = %v, want %v", f.subs(), want)
	}
	assertMirrorFetch(t, f.calls[2], dir)
}

func TestSyncRepoCorruptMirrorRebuildFailureIsACleanError(t *testing.T) {
	d := newTestDeps(t, nil)
	dir := d.mirrors.Dir("repo-a")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fakeGit{}
	fetches := 0
	f.script = func(c gitCall) ([]byte, error) {
		switch c.sub() {
		case "init":
			return nil, os.MkdirAll(dir, 0o700)
		case "fetch":
			if fetches++; fetches == 1 {
				return []byte("fatal: not a git repository"), errExit
			}
		}
		return []byte("fatal: unable to access 'https://git.example.com/group/repo-a/': The requested URL returned error: 403"), errExit
	}
	d.run = f.run

	got := syncRepo(context.Background(), d, testTask())
	if got.State != StateError || got.ErrorType != ErrOriginAccessDenied {
		t.Fatalf("result = %+v, want error/origin_access_denied", got)
	}
	if want := []string{"fetch", "init", "fetch"}; !slices.Equal(f.subs(), want) {
		t.Errorf("git sequence = %v, want exactly one rebuild attempt: %v", f.subs(), want)
	}
	if d.mirrors.Exists("repo-a") {
		t.Error("a rebuild whose fetch failed must not leave an empty mirror behind")
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
