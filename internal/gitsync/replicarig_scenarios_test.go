package gitsync

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// End-to-end scenarios for the replica rig (see replicarig_test.go): the real sync path, real git,
// a replica behind a TLS front end. They check the replica's refs, the typed outcome, the mirror
// and that no credential leaked, as far as each scenario applies (the helpers are in replicarig_test.go).
// All are small and, except where noted, run in parallel.

// A sync that finds the origin's history rewritten (an amended commit) or rolled back to an ancestor
// must make the replica's branch follow it: both are forced updates.
func TestReplicaRigFollowsAForcePushedBranch(t *testing.T) {
	t.Parallel()
	r := newReplicaRig(t, 2)
	first := r.syncOK()
	r.wantSameTips()
	r.wantRefsPushed(first, 8) // main, b00, b01 and five tags
	base := tip(t, r.origin, "refs/heads/main")

	second := r.commit("two")
	r.syncOK()
	r.wantSameTips()

	rewritten := r.amend("two, rewritten")
	if rewritten == second {
		t.Fatal("setup: the amend did not change the commit")
	}
	pushes := r.front.Pushes()
	res := r.syncOK()
	if got := tip(t, r.replica, "refs/heads/main"); got != rewritten {
		t.Errorf("replica main = %s, want the rewritten tip %s", got, rewritten)
	}
	r.wantSameTips()
	r.wantMirrorEqualsOrigin()
	r.wantRefsPushed(res, 1)
	if got := r.front.Pushes() - pushes; got != 1 {
		t.Errorf("the sync made %d pushes, want 1", got)
	}
	if out := git(t, "", "--git-dir="+r.replica, "cat-file", "-t", rewritten); out != "commit" {
		t.Errorf("rewritten tip on the replica is a %q, want a commit", out)
	}

	r.rollback(base)
	res = r.syncOK()
	if got := tip(t, r.replica, "refs/heads/main"); got != base {
		t.Errorf("replica main = %s after the rollback, want %s", got, base)
	}
	r.wantSameTips()
	r.wantRefsPushed(res, 1)
}

// A tag the origin moved (a forced update, lightweight or annotated) is moved on the replica too.
func TestReplicaRigFollowsAMovedTag(t *testing.T) {
	t.Parallel()
	r := newReplicaRig(t, 1)
	git(t, r.work, "tag", "-a", "-m", "release", "rel", r.sha)
	git(t, r.work, "push", r.origin, "refs/tags/rel")
	r.syncOK()
	r.wantSameTips()

	moved := r.commit("two")
	git(t, r.work, "tag", "-f", "t0", moved)
	git(t, r.work, "tag", "-f", "-a", "-m", "release again", "rel", moved)
	git(t, r.work, "push", "--force", r.origin, "refs/tags/t0", "refs/tags/rel")
	res := r.syncOK()
	for _, tag := range []string{"refs/tags/t0", "refs/tags/rel"} {
		if got, want := tip(t, r.replica, tag), tip(t, r.origin, tag); got != want {
			t.Errorf("replica %s = %s, want %s", tag, got, want)
		}
	}
	if got := git(t, "", "--git-dir="+r.replica, "cat-file", "-t", "refs/tags/rel"); got != "tag" {
		t.Errorf("replica rel is a %q, want an annotated tag", got)
	}
	r.wantSameTips()
	r.wantRefsPushed(res, 3) // main, t0 and rel
}

// A branch and a tag deleted at the origin are deleted on the replica, and the statistic counts them.
func TestReplicaRigPrunesADeletedBranchAndTag(t *testing.T) {
	t.Parallel()
	r := newReplicaRig(t, 3)
	r.syncOK()

	r.updateOriginRefs("delete refs/heads/b01", "delete refs/tags/t3")
	res := r.syncOK()
	got := refs(t, r.replica)
	for _, gone := range []string{"refs/heads/b01", "refs/tags/t3"} {
		if slices.Contains(got, gone) {
			t.Errorf("replica still has %s", gone)
		}
	}
	r.wantSameTips()
	r.wantMirrorEqualsOrigin()
	r.wantRefsPushed(res, 2)
}

// Hundreds of branches deleted at the origin, together with some updated ones, converge through the
// plan, the batches that carry the updates and ONE final push that carries every deletion. Nothing is
// deleted on the replica before that last push.
func TestReplicaRigConvergesHundredsOfDeletionsInTheFinalPush(t *testing.T) {
	t.Parallel()
	const total, deleted, updated = 400, 250, 150
	r := newReplicaRig(t, total)
	r.lim.PushBatchRefs = 100
	// The replica starts in step with the origin, which saves a first sync of 406 refs.
	var all []string
	for i := range total {
		all = append(all, fmt.Sprintf("refs/heads/b%02d", i))
	}
	for i := range 5 {
		all = append(all, fmt.Sprintf("refs/tags/t%d", i))
	}
	r.seedReplica(append(all, "refs/heads/main")...)

	// The origin deletes b00..b249 and moves b250..b399 to a new commit.
	next := r.commit("two")
	var cmds []string
	for i := range total {
		switch name := fmt.Sprintf("refs/heads/b%02d", i); {
		case i < deleted:
			cmds = append(cmds, "delete "+name)
		default:
			cmds = append(cmds, fmt.Sprintf("update %s %s", name, next))
		}
	}
	r.updateOriginRefs(cmds...)

	// What the replica holds after each push, taken before the client sees the response.
	var mu sync.Mutex
	var counts []int
	r.front.OnPush(func(int) {
		names, err := replicaRefNames(r.replica)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			counts = append(counts, -1)
			return
		}
		counts = append(counts, len(names))
	})

	res := r.syncOK()
	// 150 moved branches and main are 151 updates: two batches of at most 100.
	if want := []string{"dry", "batch", "batch", "final"}; !slices.Equal(r.rec.kinds(), want) {
		t.Fatalf("pushes = %v, want %v", r.rec.kinds(), want)
	}
	r.wantSameTips()
	r.wantMirrorEqualsOrigin()
	r.wantRefsPushed(res, updated+1+deleted) // the moved branches, main, and the deletions
	mu.Lock()
	defer mu.Unlock()
	start := total + 6 // the branches, main and five tags
	if want := []int{start, start, start - deleted}; !slices.Equal(counts, want) {
		t.Errorf("replica ref counts after each push = %v, want %v: no deletion before the final push", counts, want)
	}
}

// A push batched into several explicit pushes plus the final prune push goes through the TLS front
// end: every refspec arrives, the stale refs are pruned, and the connector never sends anything but
// its own token.
func TestReplicaRigBatchedPushThroughTLS(t *testing.T) {
	t.Parallel()
	r := newReplicaRig(t, 250)
	r.lim.PushBatchRefs = 100
	r.seedReplica("refs/heads/stale-1", "refs/tags/stale-t")
	if !strings.HasPrefix(r.task.ReplicaURL, "https://") {
		t.Fatalf("replica URL = %q, want https", r.task.ReplicaURL)
	}

	res := r.syncOK()
	if want := []string{"dry", "batch", "batch", "batch", "final"}; !slices.Equal(r.rec.kinds(), want) {
		t.Fatalf("pushes = %v, want %v", r.rec.kinds(), want)
	}
	// 256 refs in three batches (100, 100, 56); the final push only deletes the two stale refs.
	if got := r.front.Pushes(); got != 4 {
		t.Errorf("the front completed %d pushes, want 4", got)
	}
	r.wantSameTips()
	r.wantMirrorEqualsOrigin()
	r.wantRefsPushed(res, 258)
}

// An origin without branches or tags never wipes the replica: not one that was always empty, and not
// one whose refs have all disappeared since a healthy sync.
func TestReplicaRigEmptyOriginNeverWipesTheReplica(t *testing.T) {
	t.Parallel()
	r := newReplicaRig(t, 2)
	r.syncOK()
	want := refs(t, r.replica)
	pushes, requests := r.front.Pushes(), len(r.front.Requests())

	r.updateOriginRefs(deleteAll(refs(t, r.origin))...)
	res := r.syncOnce()
	if res.State != StateError || res.ErrorType != ErrUnclassified || res.ErrorDetail != errOriginEmpty {
		t.Fatalf("sync of an emptied origin = %+v, want error/%s %q", res, ErrUnclassified, errOriginEmpty)
	}
	if got := refs(t, r.replica); !slices.Equal(got, want) {
		t.Errorf("replica refs = %v, want them untouched: %v", got, want)
	}
	if r.front.Pushes() != pushes || len(r.front.Requests()) != requests {
		t.Errorf("the replica was contacted although the origin had nothing to send: %d new requests", len(r.front.Requests())-requests)
	}

	// The origin's refs come back: the next sync converges.
	git(t, r.work, "push", "--force", r.origin, "main")
	r.syncOK()
	if got := refs(t, r.replica); !slices.Equal(got, []string{"refs/heads/main"}) {
		t.Errorf("replica refs after the origin recovered = %v, want only main", got)
	}
}

func TestReplicaRigOriginEmptiedBeforeTheFirstSyncLeavesAStockedReplicaAlone(t *testing.T) {
	t.Parallel()
	r := newReplicaRig(t, 0)
	r.seedReplica("refs/heads/kept")
	r.updateOriginRefs(deleteAll(refs(t, r.origin))...)

	res := r.syncOnce()
	if res.State != StateError || res.ErrorDetail != errOriginEmpty {
		t.Fatalf("sync = %+v, want the empty-origin error", res)
	}
	if got := refs(t, r.replica); !slices.Contains(got, "refs/heads/kept") {
		t.Errorf("replica refs = %v, lost refs/heads/kept", got)
	}
	if n := len(r.front.Requests()); n != 0 {
		t.Errorf("the front saw %d requests, want none", n)
	}
}

func deleteAll(names []string) []string {
	cmds := make([]string, len(names))
	for i, n := range names {
		cmds[i] = "delete " + n
	}
	return cmds
}

// The LFS warning is reported by a healthy sync into a replica behind the front end, and cleared by
// the sync that finds the rule gone.
func TestReplicaRigLFSWarningPresentThenCleared(t *testing.T) {
	t.Parallel()
	o := newLFSOrigin(t, "main")
	front := newReplicaFront(t, filepath.Join(filepath.Dir(o.bare), "replica.git"))
	o.task.ReplicaURL = front.ReplicaURL()
	o.task.PushToken = Secret(front.Token())
	o.deps.run, o.deps.runOut = front.trustRun(o.deps.run), front.trustRunOut(o.deps.runOut)

	o.commit(map[string]string{".gitattributes": lfsRule, "a.txt": "a"})
	o.push("main")
	if got := o.sync(); !slices.Equal(got.Warnings, []string{WarnLFSNotReplicated}) {
		t.Fatalf("Warnings = %v, want the LFS warning", got.Warnings)
	}
	if got := refs(t, filepath.Join(filepath.Dir(o.bare), "replica.git")); !slices.Equal(got, []string{"refs/heads/main"}) {
		t.Errorf("replica refs = %v, want main", got)
	}

	o.commit(map[string]string{".gitattributes": "* text=auto\n"})
	o.push("main")
	if got := o.sync(); got.Warnings != nil {
		t.Errorf("Warnings after the rule was replaced = %v, want none", got.Warnings)
	}
	if got, want := refs(t, o.mirror.Dir(o.task.RepoID)), []string{"refs/heads/main"}; !slices.Equal(got, want) {
		t.Errorf("mirror refs = %v, want %v", got, want)
	}
	for _, req := range front.Requests() {
		if req.Auth != "Bearer "+front.Token() {
			t.Errorf("the front received Authorization %q", req.Auth)
		}
	}
}

// A push the front refuses as too large (413, whether the body has a length or is chunked) is
// replica_rejected, leaves the replica as it was and the mirror intact, and succeeds once the limit is
// lifted. A push under the limit is unaffected by it.
func TestReplicaRigOversizedPushIsRejectedThenSucceedsOnceTheLimitIsLifted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		size    int // bytes of incompressible content in the new commit
		chunked bool
	}{
		{"a body with a length", 96 << 10, false},
		{"a chunked body", 2 << 20, true}, // beyond git's http.postBuffer of 1 MiB
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newReplicaRig(t, 1)
			r.front.SetMaxBody(32 << 10)
			r.syncOK() // small enough: the limit does not touch it
			before := refs(t, r.replica)
			beforeTip := tip(t, r.replica, "refs/heads/main")

			big := r.commitBlob(tc.size)
			res := r.syncOnce()
			r.wantError(res, ErrReplicaRejected)
			var refused []frontRequest
			for _, req := range r.front.Requests() {
				if req.Status == 413 {
					refused = append(refused, req)
				}
			}
			if len(refused) == 0 {
				t.Fatalf("the front never answered 413: %v", r.front.Requests())
			}
			if got := refused[0]; got.Chunked != tc.chunked {
				t.Errorf("the refused push had chunked=%v (length %d), want chunked=%v: git's http.postBuffer changed?", got.Chunked, got.Length, tc.chunked)
			}
			if got := refs(t, r.replica); !slices.Equal(got, before) || tip(t, r.replica, "refs/heads/main") != beforeTip {
				t.Errorf("replica changed although its push was refused: %v", got)
			}
			if got := tip(t, r.d.mirrors.Dir(r.task.RepoID), "refs/heads/main"); got != big {
				t.Errorf("mirror main = %s, want the origin's %s: the fetch succeeded and must be kept", got, big)
			}
			// Refused again, the same way.
			r.wantError(r.syncOnce(), ErrReplicaRejected)

			r.front.SetMaxBody(0)
			r.syncOK()
			r.wantSameTips()
		})
	}
}

// commitBlob commits size bytes of incompressible data on the origin's main.
func (r *replicaRig) commitBlob(size int) string {
	r.t.Helper()
	data := make([]byte, size)
	rng := rand.New(rand.NewPCG(uint64(size), 7))
	for i := range data {
		data[i] = byte(rng.Uint32())
	}
	if err := os.WriteFile(filepath.Join(r.work, "blob.bin"), data, 0o600); err != nil {
		r.t.Fatal(err)
	}
	git(r.t, r.work, "add", "blob.bin")
	git(r.t, r.work, "commit", "-m", "blob")
	git(r.t, r.work, "push", "--force", r.origin, "main")
	return git(r.t, r.work, "rev-parse", "HEAD")
}

// A push the replica stalls on is cut off by the connector's own push timeout (push_timeout), whatever
// the front would have done later. The timeout is injected short; the test waits for the outcome, not
// for a fixed time, and checks the front saw the stall.
func TestReplicaRigStalledPushIsAPushTimeout(t *testing.T) {
	t.Parallel()
	r := newReplicaRig(t, 1)
	r.syncOK()
	before := refs(t, r.replica)
	r.commit("two")
	r.front.StallPushes()
	r.lim.PushTimeout = 3 * time.Second // the stall starts at the first request of the push

	res := r.syncOnce()
	r.wantError(res, ErrPushTimeout)
	if r.front.Stalled() == 0 {
		t.Error("the push never reached the stall, so the timeout proved nothing")
	}
	if got := refs(t, r.replica); !slices.Equal(got, before) || tip(t, r.replica, "refs/heads/main") == tip(t, r.origin, "refs/heads/main") {
		t.Errorf("replica changed although the push never completed: %v", got)
	}

	// The replica comes back: the next sync converges.
	r.front.UnstallPushes()
	r.lim.PushTimeout = defaultPushTimeout
	r.syncOK()
	r.wantSameTips()
}

// A load balancer that gave up on the replica answers 504; that is the replica being unreachable, not
// a push timeout.
func TestReplicaRigGatewayTimeoutIsReplicaUnreachable(t *testing.T) {
	t.Parallel()
	r := newReplicaRig(t, 1)
	r.syncOK()
	r.commit("two")
	r.front.FailPushes(http.StatusGatewayTimeout)

	res := r.syncOnce()
	r.wantError(res, ErrReplicaUnreachable)
	if r.front.Count(504) == 0 {
		t.Errorf("the front never answered 504: %v", r.front.Requests())
	}
	r.front.FailPushes(0)
	r.syncOK()
	r.wantSameTips()
}

// The replica's credential check turning into 401 in the middle of a multi-batch push stops the sync
// as credential_rejected. Nothing is deleted on the replica, because the deleting push never ran, and
// once the replica accepts the credential again the next sync converges.
func TestReplicaRigCredentialLostMidPushDeletesNothingAndRecovers(t *testing.T) {
	t.Parallel()
	r := newReplicaRig(t, 250)
	r.lim.PushBatchRefs = 100
	r.seedReplica("refs/heads/stale-1", "refs/tags/stale-t")
	r.front.RevokeAfterPushes(1)

	res := r.syncOnce()
	r.wantError(res, ErrCredentialRejected)
	r.wantStaleSurvive("refs/heads/stale-1", "refs/tags/stale-t")
	if slices.Contains(r.rec.kinds(), "final") {
		t.Errorf("pushes = %v: the pruning push must not run after a batch failed", r.rec.kinds())
	}
	r.wantRefsPushed(res, 100)
	if got := len(refs(t, r.replica)); got != 100+2 {
		t.Errorf("replica holds %d refs, want the 100 of the first batch and the stale pair", got)
	}
	if r.front.Count(401) == 0 {
		t.Error("the front never answered 401")
	}

	r.front.Restore()
	res = r.syncOK()
	r.wantSameTips()
	r.wantMirrorEqualsOrigin()
	r.wantRefsPushed(res, 156+2) // the rest of the 256, and the two deletions
}

// The front checks the Bearer token it issued: after a rotation the old one is refused as
// credential_rejected and the new one is accepted.
func TestReplicaRigRotatedTokenReplacesTheOldOne(t *testing.T) {
	t.Parallel()
	r := newReplicaRig(t, 1)
	r.syncOK()

	fresh := r.front.RotateToken()
	r.commit("two")
	r.wantError(r.syncOnce(), ErrCredentialRejected)
	if got := tip(t, r.replica, "refs/heads/main"); got == tip(t, r.origin, "refs/heads/main") {
		t.Error("replica was updated with a refused token")
	}

	r.task.PushToken = Secret(fresh)
	r.syncOK()
	r.wantSameTips()
}

// A CA the connector does not trust makes the replica unreachable (a TLS failure, not a rejected
// credential); the CA named in GIT_SSL_CAINFO, which the daemon passes to git, is trusted. This test
// sets the environment, so it cannot run in parallel; it uses the unwrapped git runners the daemon
// uses.
func TestReplicaRigTrustsACustomCAThroughTheDaemonEnvironment(t *testing.T) {
	r := newReplicaRig(t, 1)
	// The recorder runs the real git without the trust wrapper, as the daemon does.
	r.d.run, r.d.runOut = r.rec.run, r.rec.runOut
	for _, name := range []string{"GIT_SSL_CAINFO", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}

	r.wantError(r.syncOnce(), ErrReplicaUnreachable)
	if len(r.front.Requests()) != 0 {
		t.Errorf("the front received %d requests over a connection the client should have refused", len(r.front.Requests()))
	}

	t.Setenv("GIT_SSL_CAINFO", r.front.CAFile)
	r.syncOK()
	r.wantSameTips()
}

// A token that is refused only when the pack is sent (the advertisement before it passed) is the same
// credential_rejected, and it too deletes nothing.
func TestReplicaRigCredentialRefusedAtThePostIsRejected(t *testing.T) {
	t.Parallel()
	r := newReplicaRig(t, 1)
	r.seedReplica("refs/heads/stale-1")
	r.front.RevokePushAuth()

	r.wantError(r.syncOnce(), ErrCredentialRejected)
	r.wantStaleSurvive("refs/heads/stale-1")
	if r.front.Count(401) == 0 || r.front.Pushes() != 0 {
		t.Errorf("requests = %v, want a 401 and no completed push", r.front.Requests())
	}

	r.front.Restore()
	r.syncOK()
	r.wantReplicaEqualsOrigin()
}

// The front can throttle the body of a push, so a test can make a push take a known time without a
// large repository, and it reports the size of each push body it saw.
func TestReplicaRigThrottlesAPushAndReportsItsSize(t *testing.T) {
	t.Parallel()
	r := newReplicaRig(t, 0)
	r.front.SetPushThrottle(100 << 10)
	r.commitBlob(48 << 10)

	start := time.Now()
	r.syncOK()
	var seen int64
	for _, req := range r.front.Requests() {
		if req.Push {
			seen = max(seen, req.Bytes)
		}
	}
	if seen < 48<<10 {
		t.Errorf("the largest push body seen is %d bytes, want at least the 48 KiB blob", seen)
	}
	// 48 KiB at 100 KiB/s cannot take less than 0.45 s, whatever the machine.
	if took := time.Since(start); took < 450*time.Millisecond {
		t.Errorf("the sync took %v, want the throttle to hold the push for at least 450ms", took)
	}
	r.wantSameTips()
}
