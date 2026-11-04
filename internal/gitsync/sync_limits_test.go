package gitsync

import (
	"context"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func limitsDeps(t *testing.T, f *fakeGit, l Limits) syncDeps {
	t.Helper()
	d := newStatsDeps(t, f)
	d.limits = func() Limits { return l }
	return d
}

// --- pack settings ---

func TestPackConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		in   Limits
		want [][2]string
	}{
		"defaults add nothing": {Limits{}, nil},
		"threads":              {Limits{PackThreads: 3}, [][2]string{{"pack.threads", "3"}}},
		"window memory":        {Limits{PackWindowMemoryMB: 64}, [][2]string{{"pack.windowMemory", "64m"}}},
		"both":                 {Limits{PackThreads: 2, PackWindowMemoryMB: 256}, [][2]string{{"pack.threads", "2"}, {"pack.windowMemory", "256m"}}},
		"negative is ignored":  {Limits{PackThreads: -1, PackWindowMemoryMB: -5}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := packConfig(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("packConfig = %v, want %v", got, tc.want)
			}
		})
	}
}

// envConfig returns the GIT_CONFIG_KEY/VALUE pairs in env as "key=value".
func envConfig(env []string) []string {
	vals := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && (strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_")) {
			vals[k] = v
		}
	}
	var out []string
	for i := 0; ; i++ {
		k, ok := vals["GIT_CONFIG_KEY_"+strconv.Itoa(i)]
		if !ok {
			return out
		}
		out = append(out, k+"="+vals["GIT_CONFIG_VALUE_"+strconv.Itoa(i)])
	}
}

func TestPackLimitsReachFetchAndPushButNotOtherGit(t *testing.T) {
	f := &fakeGit{}
	d := limitsDeps(t, f, Limits{PackThreads: 3, PackWindowMemoryMB: 128})
	if got := syncRepo(context.Background(), d, testTask()); got.State != StateOK {
		t.Fatalf("sync = %+v", got)
	}
	want := []string{"pack.threads=3", "pack.windowMemory=128m"}
	seen := map[string]bool{}
	for _, c := range f.calls {
		seen[c.sub()] = true
		cfg := envConfig(c.env)
		has := slices.ContainsFunc(cfg, func(kv string) bool { return strings.HasPrefix(kv, "pack.") })
		switch c.sub() {
		case "fetch", "push":
			for _, w := range want {
				if !slices.Contains(cfg, w) {
					t.Errorf("%s config = %v, want %q", c.sub(), cfg, w)
				}
			}
		default:
			if has {
				t.Errorf("%q must not carry pack settings: %v", c.sub(), cfg)
			}
		}
	}
	if !seen["fetch"] || !seen["push"] {
		t.Fatalf("expected a fetch and a push, saw %v", f.subs())
	}
}

func TestNoPackSettingsWhenTheLimitsAreUnset(t *testing.T) {
	f := &fakeGit{}
	d := limitsDeps(t, f, DefaultLimits())
	syncRepo(context.Background(), d, testTask())
	for _, c := range f.calls {
		for _, kv := range envConfig(c.env) {
			if strings.HasPrefix(kv, "pack.") || strings.HasPrefix(kv, "gc.") || strings.HasPrefix(kv, "maintenance.") {
				t.Errorf("%s carries %q with no limits set", c.sub(), kv)
			}
		}
	}
}

// Pack settings travel in the environment, never in argv, so they cannot be mistaken for a flag or
// reach a shell.
func TestPackSettingsStayOutOfArgv(t *testing.T) {
	f := &fakeGit{}
	d := limitsDeps(t, f, Limits{PackThreads: 3, PackWindowMemoryMB: 128})
	syncRepo(context.Background(), d, testTask())
	for _, c := range f.calls {
		if slices.ContainsFunc(c.argv, func(a string) bool { return strings.Contains(a, "pack.") }) {
			t.Errorf("%s argv = %v, want pack settings out of argv", c.sub(), c.argv)
		}
	}
}

// Real git must accept the value as the size it is meant to be: 64 MiB, not 64 bytes.
func TestPackSettingsAreUnderstoodByRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	d := syncDeps{}
	env := d.replicaEnv(home, testTask(), Limits{PackThreads: 3, PackWindowMemoryMB: 64})
	for key, want := range map[string]string{"pack.windowMemory": strconv.Itoa(64 << 20), "pack.threads": "3"} {
		out, _, err := runGitCapture(t, []string{"git", "config", "--type=int", "--get", key}, env, home)
		if err != nil {
			t.Fatalf("git config %s: %v", key, err)
		}
		if strings.TrimSpace(out) != want {
			t.Errorf("git reads %s as %q, want %q", key, strings.TrimSpace(out), want)
		}
	}
}

func runGitCapture(t *testing.T, argv, env []string, dir string) (string, []byte, error) {
	t.Helper()
	var out strings.Builder
	stderr, err := runGitOut(context.Background(), argv, env, dir, &out)
	return out.String(), stderr, err
}

// --- push timeout ---

func TestPushDeadlineComesFromTheLimit(t *testing.T) {
	var got time.Duration
	d := newTestDeps(t, &fakeGit{})
	d.limits = func() Limits { return Limits{PushTimeout: 7 * time.Minute} }
	d.run = func(ctx context.Context, argv, _ []string, _ string) ([]byte, error) {
		if (gitCall{argv: argv}).sub() == "push" {
			dl, ok := ctx.Deadline()
			if !ok {
				t.Error("the push has no deadline")
			}
			got = time.Until(dl)
		}
		return nil, nil
	}
	syncRepo(context.Background(), d, testTask())
	if got < 6*time.Minute || got > 7*time.Minute {
		t.Errorf("push deadline in %v, want just under the 7m limit", got)
	}
}

func TestPushDeadlineDefaultsWithoutALimit(t *testing.T) {
	var got time.Duration
	d := newTestDeps(t, &fakeGit{})
	d.run = func(ctx context.Context, argv, _ []string, _ string) ([]byte, error) {
		if (gitCall{argv: argv}).sub() == "push" {
			dl, _ := ctx.Deadline()
			got = time.Until(dl)
		}
		return nil, nil
	}
	syncRepo(context.Background(), d, testTask())
	if got < 29*time.Minute || got > defaultPushTimeout {
		t.Errorf("push deadline in %v, want just under the %v default", got, defaultPushTimeout)
	}
}

// The limits are read once per attempt: a change made while one runs applies from the next.
func TestLimitsAreSnapshottedPerAttempt(t *testing.T) {
	var calls int
	d := newTestDeps(t, &fakeGit{})
	d.limits = func() Limits { calls++; return DefaultLimits() }
	syncRepo(context.Background(), d, testTask())
	if calls != 1 {
		t.Errorf("limits read %d times in one attempt, want once", calls)
	}
}

// --- repository ceiling ---

func writeSparse(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorExceeds(t *testing.T) {
	dir := t.TempDir()
	writeSparse(t, filepath.Join(dir, "objects", "pack", "a.pack"), 600)
	writeSparse(t, filepath.Join(dir, "objects", "b"), 400)
	for _, tc := range []struct {
		ceiling int64
		want    bool
	}{{1000, false}, {1001, false}, {999, true}, {0, true}, {1 << 40, false}} {
		got, err := mirrorExceeds(dir, tc.ceiling)
		if err != nil || got != tc.want {
			t.Errorf("mirrorExceeds(1000 bytes, ceiling %d) = %v, %v; want %v", tc.ceiling, got, err, tc.want)
		}
	}
}

func TestMirrorExceedsFollowsNoSymlinkAndToleratesAMissingDir(t *testing.T) {
	outside := t.TempDir()
	writeSparse(t, filepath.Join(outside, "huge"), 10_000)
	dir := t.TempDir()
	writeSparse(t, filepath.Join(dir, "small"), 10)
	if err := os.Symlink(filepath.Join(outside, "huge"), filepath.Join(dir, "link-to-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link-to-dir")); err != nil {
		t.Fatal(err)
	}
	if over, err := mirrorExceeds(dir, 100); err != nil || over {
		t.Errorf("mirrorExceeds with symlinks out of the mirror = %v, %v; want false (links are not followed)", over, err)
	}
	if over, err := mirrorExceeds(filepath.Join(dir, "absent"), 0); err != nil || over {
		t.Errorf("mirrorExceeds of a missing dir = %v, %v; want false", over, err)
	}
}

func TestOverTheCeilingNothingIsPushed(t *testing.T) {
	f := &fakeGit{}
	d := limitsDeps(t, f, Limits{MaxRepositoryMB: 1})
	task := testTask()
	writeSparse(t, filepath.Join(d.mirrors.Dir(task.RepoID), "objects", "pack", "big.pack"), mebibyte+1)

	got := syncRepo(context.Background(), d, task)
	if got.State != StateError || got.ErrorType != ErrRepositoryOverLimit {
		t.Fatalf("result = %+v, want error/repository_over_limit", got)
	}
	if got.ErrorDetail != "" {
		t.Errorf("detail = %q, want none: nothing local should leak", got.ErrorDetail)
	}
	if slices.Contains(f.subs(), "push") {
		t.Errorf("git calls = %v: an oversized mirror must not be pushed", f.subs())
	}
	if got.Stats.RefsPushed == nil || *got.Stats.RefsPushed != 0 {
		t.Errorf("refs pushed = %v, want a measured 0", got.Stats.RefsPushed)
	}
}

func TestExactlyAtTheCeilingIsNotOver(t *testing.T) {
	f := &fakeGit{}
	d := limitsDeps(t, f, Limits{MaxRepositoryMB: 1})
	task := testTask()
	writeSparse(t, filepath.Join(d.mirrors.Dir(task.RepoID), "objects", "x"), mebibyte)
	if got := syncRepo(context.Background(), d, task); got.State != StateOK {
		t.Errorf("a mirror exactly at the ceiling = %+v, want ok", got)
	}
}

func TestNoCeilingMeansNoSizeCheck(t *testing.T) {
	f := &fakeGit{}
	d := limitsDeps(t, f, Limits{})
	task := testTask()
	writeSparse(t, filepath.Join(d.mirrors.Dir(task.RepoID), "objects", "x"), 8*mebibyte)
	if got := syncRepo(context.Background(), d, task); got.State != StateOK {
		t.Errorf("with no ceiling = %+v, want ok", got)
	}
}

// The ceiling is a gate on pushing, after the fetch and the empty-origin guard.
func TestCeilingIsCheckedAfterTheFetchAndTheEmptyOriginGuard(t *testing.T) {
	f := &fakeGit{script: func(c gitCall) ([]byte, error) {
		if c.sub() == "show-ref" {
			return nil, exitCode1(t)
		}
		return nil, nil
	}}
	d := limitsDeps(t, f, Limits{MaxRepositoryMB: 1})
	task := testTask()
	writeSparse(t, filepath.Join(d.mirrors.Dir(task.RepoID), "objects", "x"), 2*mebibyte)
	got := syncRepo(context.Background(), d, task)
	if got.ErrorType != ErrUnclassified || !strings.Contains(got.ErrorDetail, "no branches or tags") {
		t.Errorf("result = %+v, want the empty-origin error (the guard comes first)", got)
	}
	if !slices.Equal(f.subs(), []string{"fetch", "show-ref"}) {
		t.Errorf("git calls = %v, want fetch then show-ref only", f.subs())
	}
}

func TestCeilingMakesFetchMaintenanceForeground(t *testing.T) {
	for _, tc := range []struct {
		name string
		lim  Limits
		want bool
	}{{"ceiling set", Limits{MaxRepositoryMB: 10}, true}, {"no ceiling", Limits{}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGit{}
			d := limitsDeps(t, f, tc.lim)
			syncRepo(context.Background(), d, testTask())
			for _, c := range f.calls {
				has := slices.Contains(envConfig(c.env), "gc.autoDetach=false")
				if want := tc.want && c.sub() == "fetch"; has != want {
					t.Errorf("%s: gc.autoDetach=false present = %v, want %v", c.sub(), has, want)
				}
			}
		})
	}
}

// A real over-the-ceiling repository against real git: not pushed, replica untouched, and the same
// repository under a roomier ceiling syncs.
func TestRealGitRepositoryOverTheCeiling(t *testing.T) {
	e := newBatchEnv(t, 2)
	blob := make([]byte, 2*mebibyte)
	rng := rand.NewChaCha8([32]byte{1})
	_, _ = rng.Read(blob) // incompressible, so the pack really is about 2 MiB
	if err := os.WriteFile(filepath.Join(e.work, "blob.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, e.work, "add", "blob.bin")
	git(t, e.work, "commit", "-m", "big")
	git(t, e.work, "push", e.origin, "main")

	e.lim.MaxRepositoryMB = 1
	res := e.sync(context.Background())
	if res.State != StateError || res.ErrorType != ErrRepositoryOverLimit {
		t.Fatalf("sync = %+v, want error/repository_over_limit", res)
	}
	if got := refs(t, e.replica); len(got) != 0 {
		t.Errorf("replica refs = %v, want none pushed", got)
	}
	if len(e.rec.kinds()) != 0 {
		t.Errorf("pushes = %v, want none", e.rec.kinds())
	}
	if !e.d.mirrors.Exists(e.task.RepoID) {
		t.Error("the mirror should be kept so the next fetch is incremental")
	}

	e.lim.MaxRepositoryMB = 64
	if res := e.sync(context.Background()); res.State != StateOK {
		t.Fatalf("sync under a roomier ceiling = %+v, want ok", res)
	}
	e.wantReplicaEqualsOrigin()
}

// --- unbatched push is unchanged ---

func pushCalls(f *fakeGit) []gitCall {
	var out []gitCall
	for _, c := range f.calls {
		if c.sub() == "push" {
			out = append(out, c)
		}
	}
	return out
}

func assertSinglePrunePush(t *testing.T, f *fakeGit, dir string) {
	t.Helper()
	pushes := pushCalls(f)
	if len(pushes) != 1 {
		t.Fatalf("pushes = %d, want exactly one", len(pushes))
	}
	want := append([]string{"git", "--git-dir=" + dir, "push", "--porcelain", "--prune", "--force", "--", testReplicaURL}, mirrorRefspecs...)
	if !slices.Equal(pushes[0].argv, want) {
		t.Errorf("push argv = %v\nwant %v", pushes[0].argv, want)
	}
}

func TestPushIsUnchangedWhenBatchingIsOff(t *testing.T) {
	f := &fakeGit{}
	d := limitsDeps(t, f, Limits{PushBatchRefs: 0})
	syncRepo(context.Background(), d, testTask())
	assertSinglePrunePush(t, f, d.mirrors.Dir("repo-a"))
}

func TestPushIsNeverBatchedWithoutTheOutputSeam(t *testing.T) {
	f := &fakeGit{}
	d := newTestDeps(t, f) // no runOut: the plan cannot be read
	d.limits = func() Limits { return Limits{PushBatchRefs: 1} }
	syncRepo(context.Background(), d, testTask())
	assertSinglePrunePush(t, f, d.mirrors.Dir("repo-a"))
}

// manyRefs makes the fake mirror report n refs when they are counted for batching (a count of at most
// the batch size skips the plan), and answers every other call with next, if any.
func manyRefs(n int, next func(gitCall) string) func(gitCall) string {
	return func(c gitCall) string {
		if c.sub() == "for-each-ref" && slices.Contains(c.argv, "--format=x") {
			return strings.Repeat("x\n", n)
		}
		if next != nil {
			return next(c)
		}
		return ""
	}
}

func planOutput(upserts, deletes int) func(c gitCall) string {
	return manyRefs(1000, func(c gitCall) string {
		if c.sub() != "push" || !slices.Contains(c.argv, "--dry-run") {
			return ""
		}
		var b strings.Builder
		b.WriteString("To https://replica.example.com/r/repo-a.git\n")
		for i := range upserts {
			b.WriteString("*\trefs/heads/n" + strconv.Itoa(i) + ":refs/heads/n" + strconv.Itoa(i) + "\t[new branch]\n")
		}
		for i := range deletes {
			b.WriteString("-\t:refs/heads/d" + strconv.Itoa(i) + "\t[deleted]\n")
		}
		return b.String() + "Done\n"
	})
}

// A change that fits one batch is the plan followed by today's single pruning push: deletions never
// count towards the batch size.
func TestSmallChangeIsPlanThenSinglePrunePush(t *testing.T) {
	f := &fakeGit{stdout: planOutput(5, 500)}
	d := limitsDeps(t, f, Limits{PushBatchRefs: 5})
	if got := syncRepo(context.Background(), d, testTask()); got.State != StateOK {
		t.Fatalf("sync = %+v", got)
	}
	pushes := pushCalls(f)
	if len(pushes) != 2 || !slices.Contains(pushes[0].argv, "--dry-run") {
		t.Fatalf("pushes = %v, want the plan then one push", pushes)
	}
	if last := pushes[1].argv; slices.Contains(last, "--dry-run") || !slices.Contains(last, "--prune") ||
		!slices.Equal(refspecsOf(pushes[1]), mirrorRefspecs) {
		t.Errorf("second push argv = %v, want today's full pruning push", last)
	}
}

// The guard runs before the plan, which is itself a request to the replica: an empty mirror reaches
// no push of any kind.
func TestEmptyMirrorReachesNoPushEvenWhenBatching(t *testing.T) {
	f := &fakeGit{script: func(c gitCall) ([]byte, error) {
		if c.sub() == "show-ref" {
			return nil, exitCode1(t)
		}
		return nil, nil
	}, stdout: planOutput(50, 50)}
	d := limitsDeps(t, f, Limits{PushBatchRefs: 5})
	got := syncRepo(context.Background(), d, testTask())
	if got.State != StateError || !strings.Contains(got.ErrorDetail, "no branches or tags") {
		t.Fatalf("result = %+v, want the empty-origin error", got)
	}
	if want := []string{"init", "fetch", "show-ref"}; !slices.Equal(f.subs(), want) {
		t.Errorf("git calls = %v, want %v", f.subs(), want)
	}
}

func TestUnreadablePlanPushesNothing(t *testing.T) {
	for name, out := range map[string]string{
		"ref outside branches and tags": "*\trefs/pull/1/head:refs/pull/1/head\t[new reference]\n",
		"space in a name":               "*\trefs/heads/a b:refs/heads/a b\t[new branch]\n",
		"refspec metacharacter":         "*\trefs/heads/a:b:refs/heads/a:b\t[new branch]\n",
		"source differs from target":    "*\trefs/heads/a:refs/heads/b\t[new branch]\n",
		"deletion with a source":        "-\trefs/heads/a:refs/heads/a\t[deleted]\n",
		"no colon":                      "*\trefs/heads/a\t[new branch]\n",
		"empty name":                    "*\trefs/heads/:refs/heads/\t[new branch]\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeGit{stdout: manyRefs(1000, func(c gitCall) string {
				if c.sub() == "push" && slices.Contains(c.argv, "--dry-run") {
					return out
				}
				return ""
			})}
			d := limitsDeps(t, f, Limits{PushBatchRefs: 1})
			got := syncRepo(context.Background(), d, testTask())
			if got.State != StateError || got.ErrorType != ErrUnclassified || got.ErrorDetail != errPlanDetail {
				t.Errorf("result = %+v, want the fixed unusable-plan error", got)
			}
			if n := len(pushCalls(f)); n != 1 {
				t.Errorf("pushes = %d, want only the plan", n)
			}
		})
	}
}

func TestPlanPushFailureIsClassifiedAsAPushFailure(t *testing.T) {
	f := &fakeGit{stdout: manyRefs(1000, nil), script: func(c gitCall) ([]byte, error) {
		if c.sub() == "push" {
			return []byte("fatal: unable to access 'https://replica.example.com/r/repo-a.git/': The requested URL returned error: 403"), errExit
		}
		return nil, nil
	}}
	d := limitsDeps(t, f, Limits{PushBatchRefs: 5})
	got := syncRepo(context.Background(), d, testTask())
	if got.ErrorType != ErrCredentialRejected {
		t.Errorf("result = %+v, want credential_rejected", got)
	}
}

// --- plan parsing, conflicts and partitioning ---

func parsePlan(in string) *pushPlan {
	p := &pushPlan{}
	_, _ = p.Write([]byte(in))
	return p
}

func TestPushPlanParsing(t *testing.T) {
	p := parsePlan("To file:///x/replica.git\n" +
		"*\trefs/heads/new:refs/heads/new\t[new branch]\n" +
		" \trefs/heads/ff:refs/heads/ff\t1111111..2222222\n" +
		"+\trefs/tags/moved:refs/tags/moved\t1111111...2222222 (forced update)\n" +
		"=\trefs/heads/same:refs/heads/same\t[up to date]\n" +
		"-\t:refs/heads/gone\t[deleted]\n" +
		"Done\n")
	if p.unusable() {
		t.Fatal("a well-formed plan was refused")
	}
	if want := []string{"refs/heads/new", "refs/heads/ff", "refs/tags/moved"}; !slices.Equal(p.upserts, want) {
		t.Errorf("upserts = %v, want %v", p.upserts, want)
	}
	if want := []string{"refs/heads/gone"}; !slices.Equal(p.deletes, want) {
		t.Errorf("deletes = %v, want %v", p.deletes, want)
	}
}

func TestPushPlanHandlesALastLineWithoutNewlineAndSplitWrites(t *testing.T) {
	p := &pushPlan{}
	for _, chunk := range []string{"*\trefs/he", "ads/a:refs/heads/a\t[new", " branch]\n-\t:refs/heads/", "z\t[deleted]"} {
		_, _ = p.Write([]byte(chunk))
	}
	if p.unusable() || !slices.Equal(p.upserts, []string{"refs/heads/a"}) || !slices.Equal(p.deletes, []string{"refs/heads/z"}) {
		t.Errorf("plan = %+v", p)
	}
}

func TestPushPlanRefusesAnOverlongStatusLineButNotAnOverlongHeader(t *testing.T) {
	long := strings.Repeat("a", maxPlanLine)
	if p := parsePlan("To https://x/" + long + "\nDone\n"); p.unusable() {
		t.Error("a long header line is not a status line")
	}
	if p := parsePlan("*\trefs/heads/" + long + ":refs/heads/" + long + "\t[new branch]\n"); !p.unusable() {
		t.Error("an overlong status line was accepted")
	}
}

func TestValidPushRef(t *testing.T) {
	for ref, want := range map[string]bool{
		"refs/heads/main":        true,
		"refs/heads/feature/x-1": true,
		"refs/tags/v1.0":         true,
		"refs/heads/":            false,
		"refs/heads":             false,
		"refs/remotes/origin/x":  false,
		"refs/pull/1/head":       false,
		"-refs/heads/x":          false,
		"refs/heads/a b":         false,
		"refs/heads/a:b":         false,
		"refs/heads/a*":          false,
		"refs/heads/a?":          false,
		"refs/heads/a[":          false,
		"refs/heads/a\\b":        false,
		"refs/heads/a^":          false,
		"refs/heads/a~1":         false,
		"refs/heads/a\x00b":      false,
		"refs/heads/a\nb":        false,
		"refs/heads/a\x7f":       false,
		"refs/heads/" + strings.Repeat("a", maxPlanRefLen-len("refs/heads/")):   true,
		"refs/heads/" + strings.Repeat("a", maxPlanRefLen-len("refs/heads/")+1): false,
	} {
		if got := validPushRef(ref); got != want {
			t.Errorf("validPushRef(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestSplitConflicts(t *testing.T) {
	for name, tc := range map[string]struct {
		upserts, deletes []string
		shadows          []string
		free, held       []string
	}{
		"nothing to delete":           {[]string{"refs/heads/a", "refs/heads/b"}, nil, nil, []string{"refs/heads/a", "refs/heads/b"}, nil},
		"unrelated delete":            {[]string{"refs/heads/a"}, []string{"refs/heads/b"}, nil, []string{"refs/heads/a"}, nil},
		"new file over stale dir":     {[]string{"refs/heads/x", "refs/heads/y"}, []string{"refs/heads/x/y"}, nil, []string{"refs/heads/y"}, []string{"refs/heads/x"}},
		"new dir under stale file":    {[]string{"refs/heads/x/y", "refs/heads/z"}, []string{"refs/heads/x"}, nil, []string{"refs/heads/z"}, []string{"refs/heads/x/y"}},
		"deep dir":                    {[]string{"refs/heads/a/b"}, []string{"refs/heads/a/b/c/d"}, nil, nil, []string{"refs/heads/a/b"}},
		"deep file":                   {[]string{"refs/heads/a/b/c"}, []string{"refs/heads/a/b"}, nil, nil, []string{"refs/heads/a/b/c"}},
		"shared prefix is no clash":   {[]string{"refs/heads/xy", "refs/heads/x-1"}, []string{"refs/heads/x"}, nil, []string{"refs/heads/xy", "refs/heads/x-1"}, nil},
		"tags and heads are apart":    {[]string{"refs/tags/x"}, []string{"refs/heads/x/y"}, nil, []string{"refs/tags/x"}, nil},
		"branch shadowed by a branch": {[]string{"refs/heads/foo", "refs/heads/bar"}, nil, []string{"refs/heads/refs/heads/foo"}, []string{"refs/heads/bar"}, []string{"refs/heads/foo"}},
		"tag shadowed by a branch":    {[]string{"refs/tags/v1", "refs/tags/v2"}, nil, []string{"refs/heads/refs/tags/v1"}, []string{"refs/tags/v2"}, []string{"refs/tags/v1"}},
		"tag shadowed by a tag":       {[]string{"refs/tags/v1"}, nil, []string{"refs/tags/refs/tags/v1"}, nil, []string{"refs/tags/v1"}},
		"the shadow itself is fine":   {[]string{"refs/heads/refs/heads/foo"}, nil, []string{"refs/heads/refs/heads/foo"}, []string{"refs/heads/refs/heads/foo"}, nil},
		"a ref too long for a batch":  {[]string{"refs/heads/" + strings.Repeat("a", maxBatchRefLen), "refs/heads/b"}, nil, nil, []string{"refs/heads/b"}, []string{"refs/heads/" + strings.Repeat("a", maxBatchRefLen)}},
	} {
		t.Run(name, func(t *testing.T) {
			shadowSet := map[string]struct{}{}
			for _, sh := range tc.shadows {
				shadowSet[sh] = struct{}{}
			}
			free, held := splitConflicts(tc.upserts, tc.deletes, shadowSet)
			if !slices.Equal(free, tc.free) || !slices.Equal(held, tc.held) {
				t.Errorf("free, held = %v, %v; want %v, %v", free, held, tc.free, tc.held)
			}
		})
	}
}

func TestPartitionBatches(t *testing.T) {
	names := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "refs/heads/" + strconv.Itoa(i)
		}
		return out
	}
	sizes := func(bs [][]string) []int {
		var out []int
		for _, b := range bs {
			out = append(out, len(b))
		}
		return out
	}
	for name, tc := range map[string]struct {
		n, maxRefs, maxBytes int
		want                 []int
	}{
		"empty":                        {0, 10, 1 << 20, nil},
		"fewer than a batch":           {3, 10, 1 << 20, []int{3}},
		"exactly a batch":              {10, 10, 1 << 20, []int{10}},
		"one over":                     {11, 10, 1 << 20, []int{10, 1}},
		"several":                      {25, 10, 1 << 20, []int{10, 10, 5}},
		"byte cap bites":               {10, 100, 2 * (2*len("refs/heads/0") + 3), []int{2, 2, 2, 2, 2}},
		"ref bigger than the byte cap": {3, 10, 1, []int{1, 1, 1}},
	} {
		t.Run(name, func(t *testing.T) {
			refs := names(tc.n)
			got := partitionBatches(refs, tc.maxRefs, tc.maxBytes)
			if !slices.Equal(sizes(got), tc.want) {
				t.Errorf("batch sizes = %v, want %v", sizes(got), tc.want)
			}
			var flat []string
			for _, b := range got {
				flat = append(flat, b...)
			}
			if !slices.Equal(flat, refs) {
				t.Error("batches must hold every ref once, in order")
			}
		})
	}
}

// Batches stay under the byte budget even when the count limit alone would allow far more.
func TestBatchArgumentsStayUnderTheByteBudget(t *testing.T) {
	var refs []string
	for i := range 5000 {
		refs = append(refs, "refs/heads/"+strings.Repeat("x", 200)+strconv.Itoa(i))
	}
	for _, b := range partitionBatches(refs, 100000, maxBatchArgBytes) {
		total := 0
		for _, r := range b {
			total += 2*len(r) + 3
		}
		if total > maxBatchArgBytes {
			t.Fatalf("a batch of %d refs takes %d bytes of refspec text, over the %d budget", len(b), total, maxBatchArgBytes)
		}
	}
}

// A mirror with no more refs than a batch cannot need batching, so it is pushed as it always was,
// without the extra request to the replica that planning costs.
func TestMirrorThatFitsOneBatchIsNotPlanned(t *testing.T) {
	f := &fakeGit{stdout: manyRefs(5, planOutput(500, 0))}
	d := limitsDeps(t, f, Limits{PushBatchRefs: 5})
	if got := syncRepo(context.Background(), d, testTask()); got.State != StateOK {
		t.Fatalf("sync = %+v", got)
	}
	assertSinglePrunePush(t, f, d.mirrors.Dir("repo-a"))
}

// When the refs cannot be counted the plan decides instead.
func TestUncountableMirrorIsPlanned(t *testing.T) {
	f := &fakeGit{stdout: planOutput(1, 0), script: func(c gitCall) ([]byte, error) {
		if c.sub() == "for-each-ref" && slices.Contains(c.argv, "--format=x") {
			return nil, errExit
		}
		return nil, nil
	}}
	d := limitsDeps(t, f, Limits{PushBatchRefs: 5})
	if got := syncRepo(context.Background(), d, testTask()); got.State != StateOK {
		t.Fatalf("sync = %+v", got)
	}
	if pushes := pushCalls(f); len(pushes) != 2 || !slices.Contains(pushes[0].argv, "--dry-run") {
		t.Errorf("pushes = %v, want the plan then one push", pushes)
	}
}

// The token is judged again before the plan, which is a request to the replica: one that expired
// during the fetch is not used, and no push of any kind runs.
func TestTokenExpiringDuringTheFetchStopsBeforeThePlan(t *testing.T) {
	var expired bool
	f := &fakeGit{stdout: planOutput(50, 0), script: func(c gitCall) ([]byte, error) {
		if c.sub() == "fetch" {
			expired = true
		}
		return nil, nil
	}}
	d := limitsDeps(t, f, Limits{PushBatchRefs: 5})
	d.now = func() time.Time {
		if expired {
			return syncNow.Add(10 * 365 * 24 * time.Hour)
		}
		return syncNow
	}
	got := syncRepo(context.Background(), d, testTask())
	if got.State != StateError || got.ErrorType != ErrPushTokenExpired {
		t.Fatalf("result = %+v, want error/push_token_expired", got)
	}
	if n := len(pushCalls(f)); n != 0 {
		t.Errorf("%d push(es) ran with an expired token, want none", n)
	}
}

// A valid ref too long to share an argument list is not a reason to refuse the plan: it is written
// by the final push, and the others go in batches. (The real-git version of this needs a platform
// whose path limit allows such a name.)
func TestLongRefIsLeftToTheFinalPush(t *testing.T) {
	long := "refs/heads/" + strings.Repeat("d/", maxBatchRefLen/2)
	long = strings.TrimSuffix(long, "/")
	f := &fakeGit{stdout: manyRefs(1000, func(c gitCall) string {
		if c.sub() != "push" || !slices.Contains(c.argv, "--dry-run") {
			return ""
		}
		out := "*\t" + long + ":" + long + "\t[new branch]\n"
		for i := range 11 {
			out += "*\trefs/heads/n" + strconv.Itoa(i) + ":refs/heads/n" + strconv.Itoa(i) + "\t[new branch]\n"
		}
		return out
	})}
	d := limitsDeps(t, f, Limits{PushBatchRefs: 5})
	if got := syncRepo(context.Background(), d, testTask()); got.State != StateOK {
		t.Fatalf("sync = %+v, want ok", got)
	}
	var batched int
	for _, c := range pushCalls(f) {
		if slices.Contains(c.argv, "--dry-run") || slices.Contains(c.argv, "--prune") {
			continue
		}
		batched += len(refspecsOf(c))
		for _, spec := range refspecsOf(c) {
			if len(spec) > maxBatchRefLen {
				t.Errorf("a batch carries a %d byte refspec", len(spec))
			}
		}
	}
	if batched != 11 {
		t.Errorf("batches carried %d refs, want the 11 short ones", batched)
	}
	pushes := pushCalls(f)
	if last := pushes[len(pushes)-1]; !slices.Contains(last.argv, "--prune") || !slices.Equal(refspecsOf(last), mirrorRefspecs) {
		t.Errorf("the last push = %v, want the full pruning push", last.argv)
	}
}

// An unchanged mirror is a plan of nothing but up-to-date lines. They are not kept, so they must not
// count against the plan's size bound however many there are.
func TestHugeAllUpToDatePlanIsStillUsable(t *testing.T) {
	name := "refs/heads/" + strings.Repeat("a", 200)
	line := "=\t" + name + ":" + name + "\t[up to date]\n"
	p := &pushPlan{}
	chunk := []byte(strings.Repeat(line, 1000))
	for range maxPlanBytes/(200*1000) + 2 {
		_, _ = p.Write(chunk)
	}
	if p.unusable() {
		t.Error("a plan of only up-to-date refs was refused as too big")
	}
	if len(p.upserts) != 0 || len(p.deletes) != 0 {
		t.Errorf("up-to-date refs were kept as changes: %d upserts, %d deletes", len(p.upserts), len(p.deletes))
	}
}

// The bound still applies to what is kept.
func TestPlanOfTooManyChangesIsRefused(t *testing.T) {
	p := &pushPlan{}
	name := "refs/heads/" + strings.Repeat("a", 200)
	chunk := []byte(strings.Repeat("*\t"+name+":"+name+"\t[new branch]\n", 1000))
	for range maxPlanBytes/(len(name)*1000) + 2 {
		_, _ = p.Write(chunk)
	}
	if !p.unusable() {
		t.Error("a plan retaining more than the bound was accepted")
	}
}
