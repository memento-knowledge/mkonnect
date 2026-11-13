package gitsync

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests run the batched push against real git, with failures injected at exact points between
// pushes. Each one asserts how many pushes of which kind actually ran, so none of them can pass by
// quietly exercising the unbatched path.

var batchBaseline = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

// pushRecorder wraps the real git runner, records every push, and lets a test act after the n-th
// completed push.
type pushRecorder struct {
	mu    sync.Mutex
	calls []gitCall
	real  int // completed non-dry-run pushes
	// afterPush, when set, runs after each successful real push with how many have completed.
	afterPush func(n int)
}

func (r *pushRecorder) runOut(ctx context.Context, argv, env []string, dir string, w io.Writer) ([]byte, error) {
	stderr, err := runGitOut(ctx, argv, env, dir, w)
	c := gitCall{argv: argv, env: env, dir: dir}
	if c.sub() != "push" {
		return stderr, err
	}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	n := 0
	if !slices.Contains(argv, "--dry-run") && err == nil {
		r.real++
		n = r.real
	}
	r.mu.Unlock()
	if n > 0 && r.afterPush != nil {
		r.afterPush(n)
	}
	return stderr, err
}

func (r *pushRecorder) run(ctx context.Context, argv, env []string, dir string) ([]byte, error) {
	return r.runOut(ctx, argv, env, dir, io.Discard)
}

// kinds names each push in order: "dry" (the plan), "batch" (explicit refspecs, no --prune), "final"
// (--prune).
func (r *pushRecorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		switch {
		case slices.Contains(c.argv, "--dry-run"):
			out = append(out, "dry")
		case slices.Contains(c.argv, "--prune"):
			out = append(out, "final")
		default:
			out = append(out, "batch")
		}
	}
	return out
}

func (r *pushRecorder) pushes() []gitCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// refspecsOf returns the refspecs of a push: everything after "--" and the replica URL.
func refspecsOf(c gitCall) []string {
	i := slices.Index(c.argv, "--")
	if i < 0 || i+2 > len(c.argv) {
		return nil
	}
	return c.argv[i+2:]
}

// batchEnv is an origin and a replica on disk, a recorder around the real git runner, and the knobs a
// test turns: the limits in force and the injected clock.
type batchEnv struct {
	t                           *testing.T
	root, origin, replica, work string
	sha                         string
	rec                         *pushRecorder
	d                           syncDeps
	task                        syncTask
	lim                         Limits
	skew                        atomic.Int64 // nanoseconds added to the clock after batchBaseline
}

// newBatchEnv builds an origin with main, branches b00..b(branches-1) and tags t0..t4, and an empty
// replica.
func newBatchEnv(t *testing.T, branches int) *batchEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	e := &batchEnv{t: t, root: root, origin: filepath.Join(root, "origin.git"), replica: filepath.Join(root, "replica.git"), work: filepath.Join(root, "work"), rec: &pushRecorder{}}
	for _, d := range []string{e.origin, e.replica} {
		git(t, "", "init", "--bare", "-b", "main", d)
	}
	git(t, "", "init", "-b", "main", e.work)
	if err := os.WriteFile(filepath.Join(e.work, "file.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, e.work, "add", "file.txt")
	git(t, e.work, "commit", "-m", "one")
	e.sha = git(t, e.work, "rev-parse", "HEAD")
	git(t, e.work, "push", e.origin, "main")
	var cmds strings.Builder
	for i := range branches {
		fmt.Fprintf(&cmds, "create refs/heads/b%02d %s\n", i, e.sha)
	}
	for i := range 5 {
		fmt.Fprintf(&cmds, "create refs/tags/t%d %s\n", i, e.sha)
	}
	gitStdin(t, "", cmds.String(), "--git-dir="+e.origin, "update-ref", "--stdin")

	e.lim = DefaultLimits()
	e.d = syncDeps{
		mirrors: Mirrors{Root: filepath.Join(root, "mirrors")},
		run:     e.rec.run,
		runOut:  e.rec.runOut,
		now:     func() time.Time { return batchBaseline.Add(time.Duration(e.skew.Load())) },
		limits:  func() Limits { return e.lim },
	}
	e.task = syncTask{
		RepoID:         "repo-a",
		CanonicalURL:   "file://" + e.origin,
		ReplicaURL:     "file://" + e.replica,
		PushToken:      Secret("push-token-abcdefghij"),
		TokenExpiresAt: batchBaseline.Add(time.Hour).Format(time.RFC3339),
	}
	return e
}

// seedReplica puts refs on the replica that the origin does not have.
func (e *batchEnv) seedReplica(refs ...string) {
	e.t.Helper()
	var specs []string
	for _, r := range refs {
		specs = append(specs, e.sha+":"+r)
	}
	git(e.t, e.work, append([]string{"push", e.replica}, specs...)...)
}

func (e *batchEnv) sync(ctx context.Context) syncResult { return syncRepo(ctx, e.d, e.task) }

func (e *batchEnv) installReplicaHook(script string) {
	e.t.Helper()
	path := filepath.Join(e.replica, "hooks", "pre-receive")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil { //nolint:gosec // a test hook must be executable
		e.t.Fatal(err)
	}
}

func (e *batchEnv) removeReplicaHook() {
	e.t.Helper()
	if err := os.Remove(filepath.Join(e.replica, "hooks", "pre-receive")); err != nil {
		e.t.Fatal(err)
	}
}

func (e *batchEnv) wantReplicaEqualsOrigin() {
	e.t.Helper()
	if got, want := refs(e.t, e.replica), refs(e.t, e.origin); !slices.Equal(got, want) {
		e.t.Errorf("replica refs = %v\nwant the origin's %v", got, want)
	}
}

func (e *batchEnv) wantStaleSurvive(stale ...string) {
	e.t.Helper()
	got := refs(e.t, e.replica)
	for _, s := range stale {
		if !slices.Contains(got, s) {
			e.t.Errorf("replica lost %s although the run did not complete: no deletion may happen before every batch succeeded", s)
		}
	}
}

func TestBatchedPushEndToEnd(t *testing.T) {
	e := newBatchEnv(t, 24) // 24 branches + main + 5 tags = 30 refs
	e.lim.PushBatchRefs = 10
	e.seedReplica("refs/heads/stale-1", "refs/tags/stale-t")

	res := e.sync(context.Background())
	if res.State != StateOK {
		t.Fatalf("sync = %+v, want ok", res)
	}
	if want := []string{"dry", "batch", "batch", "batch", "final"}; !slices.Equal(e.rec.kinds(), want) {
		t.Fatalf("pushes = %v, want %v", e.rec.kinds(), want)
	}
	var sent int
	for _, c := range e.rec.pushes() {
		switch {
		case slices.Contains(c.argv, "--dry-run"):
		case slices.Contains(c.argv, "--prune"):
			if !slices.Equal(refspecsOf(c), mirrorRefspecs) {
				t.Errorf("final push refspecs = %v, want the full %v", refspecsOf(c), mirrorRefspecs)
			}
		default:
			specs := refspecsOf(c)
			if len(specs) > 10 {
				t.Errorf("a batch carried %d refs, want at most 10", len(specs))
			}
			sent += len(specs)
		}
	}
	if sent != 30 {
		t.Errorf("batches carried %d refs in total, want all 30", sent)
	}
	e.wantReplicaEqualsOrigin()
	// 30 written plus 2 deleted by the final push.
	if res.Stats.RefsPushed == nil || *res.Stats.RefsPushed != 32 {
		t.Errorf("refs pushed = %v, want 32 (30 written + 2 pruned)", res.Stats.RefsPushed)
	}
}

// Nothing changed since the last sync: however large the mirror, a sync is the plan plus one push, not
// a push per batch of refs.
func TestBatchedPushOfAnUnchangedRepositoryIsAPlanAndOnePush(t *testing.T) {
	e := newBatchEnv(t, 24)
	e.lim.PushBatchRefs = 10
	if res := e.sync(context.Background()); res.State != StateOK {
		t.Fatalf("first sync = %+v", res)
	}
	before := len(e.rec.kinds())
	if res := e.sync(context.Background()); res.State != StateOK {
		t.Fatalf("second sync = %+v", res)
	}
	if got, want := e.rec.kinds()[before:], []string{"dry", "final"}; !slices.Equal(got, want) {
		t.Errorf("pushes of an unchanged repository = %v, want %v", got, want)
	}
	e.wantReplicaEqualsOrigin()
}

// A change small enough for one batch is pushed exactly as without batching: the plan, then the
// single pruning push, and no explicit refspecs.
func TestBatchedPushOfASmallChangeIsOnePrunePush(t *testing.T) {
	e := newBatchEnv(t, 24)
	e.lim.PushBatchRefs = 10
	if res := e.sync(context.Background()); res.State != StateOK {
		t.Fatal(res)
	}
	gitStdin(t, "", "update b00 "+e.sha+"\ncreate refs/heads/fresh "+e.sha+"\n", "--git-dir="+e.origin, "update-ref", "--stdin")
	git(t, "", "--git-dir="+e.origin, "update-ref", "-d", "refs/tags/t0")
	before := len(e.rec.kinds())
	if res := e.sync(context.Background()); res.State != StateOK {
		t.Fatal(res)
	}
	if got, want := e.rec.kinds()[before:], []string{"dry", "final"}; !slices.Equal(got, want) {
		t.Errorf("pushes = %v, want %v", got, want)
	}
	e.wantReplicaEqualsOrigin()
}

// A batch the replica refuses stops the run before any deletion: the replica keeps every ref the
// origin no longer has, nothing after the failing batch is pushed, and the next run converges.
func TestBatchedPushFailingBatchDeletesNothingAndStopsTheRun(t *testing.T) {
	e := newBatchEnv(t, 24)
	e.lim.PushBatchRefs = 10
	e.seedReplica("refs/heads/stale-1", "refs/tags/stale-t")
	// refs sort as b00..b23, main, t0..t4: batch 2 holds b10..b19. Refuse b15.
	e.installReplicaHook(`while read old new ref; do
  case "$ref" in refs/heads/b15) echo "refused by policy" >&2; exit 1;; esac
done
`)

	res := e.sync(context.Background())
	if res.State != StateError || res.ErrorType != ErrReplicaRejected {
		t.Fatalf("sync = %+v, want error/replica_rejected", res)
	}
	if want := []string{"dry", "batch", "batch"}; !slices.Equal(e.rec.kinds(), want) {
		t.Errorf("pushes = %v, want %v: nothing after the failed batch, and no pruning push", e.rec.kinds(), want)
	}
	got := refs(t, e.replica)
	if !slices.Contains(got, "refs/heads/b05") {
		t.Error("the first batch should have landed")
	}
	if slices.Contains(got, "refs/heads/b15") || slices.Contains(got, "refs/heads/main") {
		t.Errorf("replica holds refs from the failed or later batches: %v", got)
	}
	e.wantStaleSurvive("refs/heads/stale-1", "refs/tags/stale-t")

	e.removeReplicaHook()
	if res := e.sync(context.Background()); res.State != StateOK {
		t.Fatalf("sync after the fault cleared = %+v", res)
	}
	e.wantReplicaEqualsOrigin()
}

func TestBatchedPushTokenExpiringBetweenPushesStopsBeforeTheNextOne(t *testing.T) {
	// After the first, second and third (last) batch: the final pruning push must not start either.
	for completed := 1; completed <= 3; completed++ {
		t.Run(fmt.Sprintf("after batch %d", completed), func(t *testing.T) {
			e := newBatchEnv(t, 24)
			e.lim.PushBatchRefs = 10
			e.seedReplica("refs/heads/stale-1", "refs/tags/stale-t")
			e.rec.afterPush = func(n int) {
				if n == completed {
					e.skew.Store(int64(2 * time.Hour))
				}
			}

			res := e.sync(context.Background())
			if res.State != StateError || res.ErrorType != ErrPushTokenExpired {
				t.Fatalf("sync = %+v, want error/push_token_expired", res)
			}
			want := append([]string{"dry"}, slices.Repeat([]string{"batch"}, completed)...)
			if !slices.Equal(e.rec.kinds(), want) {
				t.Errorf("pushes = %v, want %v", e.rec.kinds(), want)
			}
			e.wantStaleSurvive("refs/heads/stale-1", "refs/tags/stale-t")
		})
	}
}

func TestBatchedPushCancelledBetweenPushesIsPendingAndDeletesNothing(t *testing.T) {
	for completed := 1; completed <= 3; completed++ {
		t.Run(fmt.Sprintf("after batch %d", completed), func(t *testing.T) {
			e := newBatchEnv(t, 24)
			e.lim.PushBatchRefs = 10
			e.seedReplica("refs/heads/stale-1", "refs/tags/stale-t")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.rec.afterPush = func(n int) {
				if n == completed {
					cancel()
				}
			}

			res := e.sync(ctx)
			if res.State != StatePending {
				t.Fatalf("sync = %+v, want pending", res)
			}
			want := append([]string{"dry"}, slices.Repeat([]string{"batch"}, completed)...)
			if !slices.Equal(e.rec.kinds(), want) {
				t.Errorf("pushes = %v, want %v", e.rec.kinds(), want)
			}
			e.wantStaleSurvive("refs/heads/stale-1", "refs/tags/stale-t")
		})
	}
}

// The replica holds refs/heads/dd/sub and refs/heads/q while the origin now has refs/heads/dd and
// refs/heads/q/new. An explicit push of dd or q/new fails to lock; the pruning push, which deletes and
// creates together, succeeds. Those refs must be left to it and the run must converge in one go.
func TestBatchedPushDirectoryFileConflictsConvergeInOneRun(t *testing.T) {
	e := newBatchEnv(t, 24)
	e.lim.PushBatchRefs = 10
	e.seedReplica("refs/heads/dd/sub", "refs/heads/q")
	gitStdin(t, "", "create refs/heads/dd "+e.sha+"\ncreate refs/heads/q/new "+e.sha+"\n", "--git-dir="+e.origin, "update-ref", "--stdin")

	res := e.sync(context.Background())
	if res.State != StateOK {
		t.Fatalf("sync = %+v, want ok", res)
	}
	for _, c := range e.rec.pushes() {
		if slices.Contains(c.argv, "--dry-run") || slices.Contains(c.argv, "--prune") {
			continue
		}
		for _, spec := range refspecsOf(c) {
			if strings.Contains(spec, "refs/heads/dd:") || strings.Contains(spec, "refs/heads/q/new:") {
				t.Errorf("batch carries %q, which conflicts with a ref the replica must first lose", spec)
			}
		}
	}
	e.wantReplicaEqualsOrigin()
}

// A batching limit must not weaken the empty-origin guard: with batching on, an origin that lost
// every ref leaves the replica as it was and nothing, not even the plan, is pushed.
func TestBatchedPushEmptyOriginNeverWipesReplica(t *testing.T) {
	e := newBatchEnv(t, 24)
	e.lim.PushBatchRefs = 10
	if res := e.sync(context.Background()); res.State != StateOK {
		t.Fatal(res)
	}
	populated := refs(t, e.replica)
	pushes := len(e.rec.kinds())

	cmds := new(strings.Builder)
	for _, r := range refs(t, e.origin) {
		fmt.Fprintf(cmds, "delete %s\n", r)
	}
	gitStdin(t, "", cmds.String(), "--git-dir="+e.origin, "update-ref", "--stdin")

	res := e.sync(context.Background())
	if res.State != StateError || res.ErrorType != ErrOriginEmpty || res.ErrorDetail != detailOriginEmpty {
		t.Fatalf("sync of an emptied origin = %+v, want the empty-origin error", res)
	}
	if got := len(e.rec.kinds()); got != pushes {
		t.Errorf("%d push(es) ran against an empty mirror, want none", got-pushes)
	}
	if got := refs(t, e.replica); !slices.Equal(got, populated) {
		t.Errorf("replica refs = %v, want them untouched: %v", got, populated)
	}
}

// The same against a replica that really is behind HTTP: the planning push and the batches
// authenticate with the push token, and a replica that drops out between two batches leaves its stale
// refs alone.
func TestBatchedPushOverHTTP(t *testing.T) {
	f := newHTTPFixture(t)
	rec := &pushRecorder{}
	f.deps.run, f.deps.runOut = rec.run, rec.runOut
	f.deps.limits = func() Limits { l := DefaultLimits(); l.PushBatchRefs = 10; return l }
	sha := git(t, "", "--git-dir="+f.originDir, "rev-parse", "main")
	var cmds strings.Builder
	for i := range 24 {
		fmt.Fprintf(&cmds, "create refs/heads/b%02d %s\n", i, sha)
	}
	gitStdin(t, "", cmds.String(), "--git-dir="+f.originDir, "update-ref", "--stdin")
	git(t, "", "--git-dir="+f.replicaDir, "fetch", f.originDir, "refs/heads/main:refs/heads/stale-1")

	// Receive-pack advertisements, in order: the plan (1), batch 1 (2), batch 2 (3): drop the third.
	f.replica.failAdvert.Store(3)
	res := f.sync()
	if res.State != StateError || res.ErrorType != ErrReplicaUnreachable {
		t.Fatalf("sync = %+v, want error/replica_unreachable", res)
	}
	// The plan, batch 1, and batch 2 which was refused; no third batch and no pruning push.
	if want := []string{"dry", "batch", "batch"}; !slices.Equal(rec.kinds(), want) {
		t.Errorf("pushes started = %v, want %v", rec.kinds(), want)
	}
	if rec.real != 1 {
		t.Errorf("pushes completed = %d, want only batch 1", rec.real)
	}
	got := refs(t, f.replicaDir)
	if !slices.Contains(got, "refs/heads/stale-1") {
		t.Errorf("replica lost its stale ref although a batch failed: %v", got)
	}
	if !slices.Contains(got, "refs/heads/b00") {
		t.Errorf("the first batch should have landed: %v", got)
	}

	f.replica.failAdvert.Store(0)
	if res := f.sync(); res.State != StateOK {
		t.Fatalf("sync after the replica recovered = %+v", res)
	}
	if got := refs(t, f.replicaDir); slices.Contains(got, "refs/heads/stale-1") || !slices.Contains(got, "refs/heads/b23") {
		t.Errorf("replica refs = %v, want the origin's, stale-1 pruned", got)
	}
	// Both credentials stayed on their own server.
	for _, h := range f.replica.authHeaders() {
		if h != "Bearer "+httpPushToken {
			t.Errorf("replica saw Authorization %q", h)
		}
	}
}

// batchRefspecs returns every refspec carried by the batch pushes (not the plan, not the final push).
func (r *pushRecorder) batchRefspecs() []string {
	var out []string
	for _, c := range r.pushes() {
		if slices.Contains(c.argv, "--dry-run") || slices.Contains(c.argv, "--prune") {
			continue
		}
		out = append(out, refspecsOf(c)...)
	}
	return out
}

// Git refuses the explicit refspec refs/heads/foo when the repository also has
// refs/heads/refs/heads/foo ("matches more than one"), though the glob push handles both. Such refs
// must be left to the final push, or the repository would fail every batched sync. The longer name is
// first seen as a change (first sync) and then only as an up-to-date ref (second sync): both must count.
func TestBatchedPushWithNamesGitFindsAmbiguousConverges(t *testing.T) {
	e := newBatchEnv(t, 24)
	e.lim.PushBatchRefs = 10
	update := func(lines ...string) {
		t.Helper()
		gitStdin(t, "", strings.Join(lines, "\n")+"\n", "--git-dir="+e.origin, "update-ref", "--stdin")
	}
	update("create refs/heads/refs/heads/foo "+e.sha, "create refs/heads/refs/tags/fresh "+e.sha, "create refs/tags/refs/tags/t1x "+e.sha)

	for phase, create := range [][]string{
		nil, // phase 0: the shadows are new
		{ // phase 1: the shadows are up to date; the shorter names arrive with enough other changes to batch
			"create refs/heads/foo " + e.sha, "create refs/tags/fresh " + e.sha, "create refs/tags/t1x " + e.sha,
		},
	} {
		if phase == 1 {
			var more []string
			for i := range 12 {
				more = append(more, fmt.Sprintf("create refs/heads/extra%02d %s", i, e.sha))
			}
			update(append(create, more...)...)
			// The premise: an explicit push of the shorter name really is refused by this git.
			probe := t.TempDir()
			git(t, "", "init", "--bare", probe)
			if out, err := exec.Command("git", "--git-dir="+e.origin, "push", "--dry-run", probe, "+refs/heads/foo:refs/heads/foo").CombinedOutput(); err == nil {
				t.Skipf("this git accepts the ambiguous refspec, so there is nothing to guard against: %s", out)
			}
		}
		before := len(e.rec.batchRefspecs())
		if res := e.sync(context.Background()); res.State != StateOK {
			t.Fatalf("phase %d: sync = %+v, want ok", phase, res)
		}
		batched := e.rec.batchRefspecs()[before:]
		if len(batched) == 0 {
			t.Errorf("phase %d: no batch push ran: the test did not exercise batching", phase)
		}
		for _, spec := range batched {
			for _, held := range []string{"refs/heads/foo:", "refs/tags/fresh:", "refs/tags/t1x:"} {
				if strings.HasPrefix(spec[1:], held) {
					t.Errorf("phase %d: batch carries %q, which git finds ambiguous", phase, spec)
				}
			}
		}
		e.wantReplicaEqualsOrigin()
	}
}

// A ref git can store but that is too long to share an argument list is left to the final push, and
// does not make the plan unusable.
func TestBatchedPushWithAVeryLongRefConverges(t *testing.T) {
	e := newBatchEnv(t, 24)
	e.lim.PushBatchRefs = 10
	var parts []string
	for range 18 {
		parts = append(parts, strings.Repeat("d", 100))
	}
	long := "refs/heads/" + strings.Join(parts, "/")
	if len(long) <= maxBatchRefLen {
		t.Fatalf("test ref is only %d bytes", len(long))
	}
	// Loose refs are files, so a name this long needs a filesystem with long paths; macOS caps a
	// path at 1024 bytes, which the origin, the mirror and the replica would each exceed.
	if runtime.GOOS != "linux" {
		t.Skip("needs a platform whose path limit allows a ref name over 1 KiB")
	}
	gitStdin(t, "", "create "+long+" "+e.sha+"\n", "--git-dir="+e.origin, "update-ref", "--stdin")

	res := e.sync(context.Background())
	if res.State != StateOK {
		t.Fatalf("sync = %+v, want ok", res)
	}
	for _, spec := range e.rec.batchRefspecs() {
		if strings.Contains(spec, long) {
			t.Error("the over-long ref was placed in a batch")
		}
	}
	if len(e.rec.batchRefspecs()) == 0 {
		t.Error("no batch push ran: the test did not exercise batching")
	}
	e.wantReplicaEqualsOrigin()
}

// A mirror that fits in one batch is pushed once, as without batching, with no plan.
func TestBatchedPushOfASmallMirrorIsNotPlanned(t *testing.T) {
	e := newBatchEnv(t, 4) // 4 branches + main + 5 tags = 10 refs
	e.lim.PushBatchRefs = 50
	if res := e.sync(context.Background()); res.State != StateOK {
		t.Fatalf("sync = %+v", res)
	}
	if want := []string{"final"}; !slices.Equal(e.rec.kinds(), want) {
		t.Errorf("pushes = %v, want %v", e.rec.kinds(), want)
	}
	e.wantReplicaEqualsOrigin()
}
