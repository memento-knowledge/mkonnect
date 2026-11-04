package gitsync

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// newStatsDeps is newTestDeps with the standard-output seam wired to the fake, which is what turns
// statistics collection on (a nil runOut leaves git running as it always has, and collects nothing).
func newStatsDeps(t *testing.T, f *fakeGit) syncDeps {
	t.Helper()
	d := newTestDeps(t, f)
	d.runOut = f.runOut
	return d
}

// outputs scripts what each subcommand prints.
type outputs struct{ forEachRef, push, lsRemote string }

func (o outputs) fn(c gitCall) string {
	switch c.sub() {
	case "for-each-ref":
		return o.forEachRef
	case "push":
		return o.push
	case "ls-remote":
		return o.lsRemote
	}
	return ""
}

const (
	pushedTwo   = "To https://replica.example.com/r/repo-a.git\n*\trefs/heads/main:refs/heads/main\t[new branch]\n \trefs/tags/v1:refs/tags/v1\t1111111..2222222\n=\trefs/tags/v0:refs/tags/v0\t[up to date]\nDone\n"
	headIsTrunk = "ref: refs/heads/trunk\tHEAD\n5dcfa042fb9b025599cd26145fd324dce4651d5d\tHEAD\n"
)

func TestSyncRepoCollectsStatistics(t *testing.T) {
	f := &fakeGit{stdout: outputs{forEachRef: "1577836800|\n|1609459200\n", push: pushedTwo, lsRemote: headIsTrunk}.fn}
	got := syncRepo(context.Background(), newStatsDeps(t, f), testTask())

	if got.State != StateOK {
		t.Fatalf("result = %+v, want ok", got)
	}
	if got.Stats.RefsPushed == nil || *got.Stats.RefsPushed != 2 {
		t.Errorf("RefsPushed = %v, want 2 (new + fast-forward; up to date is not counted)", got.Stats.RefsPushed)
	}
	if want := time.Unix(1609459200, 0).UTC(); !got.Stats.NewestCommitAt.Equal(want) {
		t.Errorf("NewestCommitAt = %v, want %v", got.Stats.NewestCommitAt, want)
	}
	if got.Stats.DefaultBranch != "trunk" {
		t.Errorf("DefaultBranch = %q, want trunk", got.Stats.DefaultBranch)
	}
}

func TestSyncRepoStatisticsCallsAreScopedAndOrdered(t *testing.T) {
	f := &fakeGit{stdout: outputs{forEachRef: "1577836800|\n", push: pushedTwo, lsRemote: headIsTrunk}.fn}
	d := newStatsDeps(t, f)
	syncRepo(context.Background(), d, testTask())

	// The newest-commit read is local and precedes the push; the origin lookup follows the push, so it
	// never delays the push or the push token's use, and only happens once the origin has been reached.
	if want := []string{"init", "fetch", "show-ref", "for-each-ref", "push", "ls-remote"}; !slices.Equal(f.subs(), want) {
		t.Fatalf("git sequence = %v, want %v", f.subs(), want)
	}
	dir := d.mirrors.Dir("repo-a")

	fer := f.calls[3]
	for _, want := range []string{"--git-dir=" + dir, "--format=" + newestFormat, "refs/heads", "refs/tags"} {
		if !slices.Contains(fer.argv, want) {
			t.Errorf("for-each-ref argv = %v, want %q", fer.argv, want)
		}
	}
	if fer.envHas(testOriginTok) || fer.envHas(testPushTok) || fer.envHas("extraHeader") {
		t.Error("for-each-ref is local and must carry no credential")
	}

	ls := f.calls[5]
	if i := slices.Index(ls.argv, "--"); i < 0 || !slices.Equal(ls.argv[i+1:], []string{testOriginURL, "HEAD"}) || !slices.Contains(ls.argv, "--symref") {
		t.Errorf("ls-remote argv = %v, want `--symref -- <canonical origin URL> HEAD`", ls.argv)
	}
	if !ls.envHas("http."+testOriginURL+"/.extraHeader") || !ls.envHas("Bearer "+testOriginTok) {
		t.Error("ls-remote must carry the origin credential scoped to the origin URL")
	}
	if ls.envHas(testPushTok) || ls.envHas(testReplicaURL) {
		t.Error("the push token and replica must never reach the origin")
	}
	for _, c := range f.calls {
		for _, a := range c.argv {
			if strings.Contains(a, testOriginTok) || strings.Contains(a, testPushTok) {
				t.Errorf("a credential leaked into argv: %v", c.argv)
			}
		}
	}
}

func TestSyncRepoPushAlwaysUsesPorcelain(t *testing.T) {
	for name, deps := range map[string]func(*testing.T, *fakeGit) syncDeps{"with statistics": newStatsDeps, "without": newTestDeps} {
		t.Run(name, func(t *testing.T) {
			f := &fakeGit{}
			syncRepo(context.Background(), deps(t, f), testTask())
			for _, c := range f.calls {
				if c.sub() == "push" && !slices.Contains(c.argv, "--porcelain") {
					t.Errorf("push argv = %v, want --porcelain so there is one push path whether or not statistics are on", c.argv)
				}
			}
		})
	}
}

func TestSyncRepoWithoutStatisticsRunsExactlyTheOriginalSequence(t *testing.T) {
	f := &fakeGit{}
	got := syncRepo(context.Background(), newTestDeps(t, f), testTask())
	if want := []string{"init", "fetch", "show-ref", "push"}; !slices.Equal(f.subs(), want) {
		t.Errorf("git sequence = %v, want %v", f.subs(), want)
	}
	if got.State != StateOK || got.Stats.RefsPushed != nil || !got.Stats.NewestCommitAt.IsZero() || got.Stats.DefaultBranch != "" {
		t.Errorf("result = %+v, want ok with no statistics", got)
	}
}

func TestSyncRepoNoOpPushCountsZeroRefs(t *testing.T) {
	f := &fakeGit{stdout: outputs{push: "To x\n=\trefs/heads/main:refs/heads/main\t[up to date]\nDone\n"}.fn}
	got := syncRepo(context.Background(), newStatsDeps(t, f), testTask())
	if got.State != StateOK || got.Stats.RefsPushed == nil || *got.Stats.RefsPushed != 0 {
		t.Errorf("result = %+v, want ok with a present refs_pushed of 0", got)
	}
}

func TestSyncRepoDefaultBranchFailureIsNotFatal(t *testing.T) {
	f := &fakeGit{
		stdout: outputs{push: pushedTwo, lsRemote: "fatal: noise\n"}.fn,
		script: func(c gitCall) ([]byte, error) {
			if c.sub() == "ls-remote" {
				return []byte("fatal: unable to access 'https://git.example.com/': The requested URL returned error: 500"), errExit
			}
			return nil, nil
		},
	}
	got := syncRepo(context.Background(), newStatsDeps(t, f), testTask())
	if got.State != StateOK || got.ErrorType != "" {
		t.Fatalf("result = %+v, want ok: failing to resolve the default branch must not fail the sync", got)
	}
	if got.Stats.DefaultBranch != "" {
		t.Errorf("DefaultBranch = %q, want it omitted", got.Stats.DefaultBranch)
	}
	if got.Stats.RefsPushed == nil || *got.Stats.RefsPushed != 2 {
		t.Errorf("RefsPushed = %v, want the push's 2 regardless", got.Stats.RefsPushed)
	}
}

func TestSyncRepoDefaultBranchOfADetachedOriginHeadIsOmitted(t *testing.T) {
	f := &fakeGit{stdout: outputs{lsRemote: "5dcfa042fb9b025599cd26145fd324dce4651d5d\tHEAD\n"}.fn}
	got := syncRepo(context.Background(), newStatsDeps(t, f), testTask())
	if got.State != StateOK || got.Stats.DefaultBranch != "" {
		t.Errorf("result = %+v, want ok with no default branch", got)
	}
}

func TestSyncRepoNewestCommitFailureIsNotFatal(t *testing.T) {
	f := &fakeGit{
		stdout: outputs{push: pushedTwo, lsRemote: headIsTrunk}.fn,
		script: func(c gitCall) ([]byte, error) {
			if c.sub() == "for-each-ref" {
				return []byte("fatal: bad"), errExit
			}
			return nil, nil
		},
	}
	got := syncRepo(context.Background(), newStatsDeps(t, f), testTask())
	if got.State != StateOK || !got.Stats.NewestCommitAt.IsZero() || got.Stats.DefaultBranch != "trunk" {
		t.Errorf("result = %+v, want ok, no newest commit, default branch still found", got)
	}
}

func TestSyncRepoFailureBeforeThePushPushedNothing(t *testing.T) {
	f := &fakeGit{script: func(c gitCall) ([]byte, error) {
		if c.sub() == "fetch" {
			return []byte("fatal: unable to access 'https://git.example.com/group/repo-a': The requested URL returned error: 403"), errExit
		}
		return nil, nil
	}}
	got := syncRepo(context.Background(), newStatsDeps(t, f), testTask())
	if got.State != StateError || got.ErrorType != ErrOriginAccessDenied {
		t.Fatalf("result = %+v, want origin_access_denied", got)
	}
	if got.Stats.RefsPushed == nil || *got.Stats.RefsPushed != 0 {
		t.Errorf("RefsPushed = %v, want a present 0: the attempt pushed nothing", got.Stats.RefsPushed)
	}
	if !got.Stats.NewestCommitAt.IsZero() || got.Stats.DefaultBranch != "" {
		t.Errorf("stats = %+v: a failed fetch learned nothing about the origin", got.Stats)
	}
	if slices.Contains(f.subs(), "ls-remote") || slices.Contains(f.subs(), "for-each-ref") {
		t.Errorf("sequence = %v: the origin must not be asked twice after it refused the first request", f.subs())
	}
}

func TestSyncRepoFailedPushCountsWhatItDidChange(t *testing.T) {
	const out = "To x\n*\trefs/heads/a:refs/heads/a\t[new branch]\n!\trefs/heads/b:refs/heads/b\t[remote rejected] (hook declined)\nDone\n"
	f := &fakeGit{
		stdout: outputs{push: out, lsRemote: headIsTrunk}.fn,
		script: func(c gitCall) ([]byte, error) {
			if c.sub() == "push" {
				return []byte("error: failed to push some refs to 'https://replica.example.com/r/repo-a.git'"), errExit
			}
			return nil, nil
		},
	}
	got := syncRepo(context.Background(), newStatsDeps(t, f), testTask())
	if got.State != StateError || got.ErrorType != ErrReplicaRejected {
		t.Fatalf("result = %+v, want replica_rejected (read from the porcelain rejection line)", got)
	}
	if got.Stats.RefsPushed == nil || *got.Stats.RefsPushed != 1 {
		t.Errorf("RefsPushed = %v, want 1: one ref did change on the replica", got.Stats.RefsPushed)
	}
}

// With --porcelain the per-ref rejection moves from stderr to stdout, and a rejection must still be
// classified exactly as the same failure printed the classic way.
func TestSyncRepoClassifiesPorcelainRejectionsLikeClassicOnes(t *testing.T) {
	tests := []struct {
		name, porcelain, classic string
		want                     string
	}{
		{"hook declined", "!\trefs/heads/main:refs/heads/main\t[remote rejected] (pre-receive hook declined)", " ! [remote rejected] main -> main (pre-receive hook declined)", ErrReplicaRejected},
		{"size limit reason", "!\trefs/heads/main:refs/heads/main\t[remote rejected] (push size limit exceeded)", " ! [remote rejected] main -> main (push size limit exceeded)", ErrReplicaRejected},
		{"unpacker error", "!\trefs/heads/main:refs/heads/main\t[remote rejected] (unpacker error)", " ! [remote rejected] main -> main (unpacker error)", ErrReplicaRejected},
		// A ref NAME must not steer classification, in either shape.
		{"ref named after a disk fault", "!\trefs/heads/no-space-left-on-device:refs/heads/no-space-left-on-device\t[remote rejected] (hook declined)", " ! [remote rejected] no-space-left-on-device -> no-space-left-on-device (hook declined)", ErrReplicaRejected},
		{"ref named after a quota", "!\trefs/heads/quota:refs/heads/quota\t[remote rejected] (hook declined)", " ! [remote rejected] quota -> quota (hook declined)", ErrReplicaRejected},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			viaPorcelain := &fakeGit{
				stdout: outputs{push: "To x\n" + tc.porcelain + "\nDone\n"}.fn,
				script: func(c gitCall) ([]byte, error) {
					if c.sub() == "push" {
						return []byte("error: failed to push some refs to 'https://replica.example.com/r.git'"), errExit
					}
					return nil, nil
				},
			}
			got := syncRepo(context.Background(), newStatsDeps(t, viaPorcelain), testTask())

			classic := &fakeGit{script: func(c gitCall) ([]byte, error) {
				if c.sub() == "push" {
					return []byte("To https://replica.example.com/r.git\n" + tc.classic + "\nerror: failed to push some refs to 'https://replica.example.com/r.git'"), errExit
				}
				return nil, nil
			}}
			want := syncRepo(context.Background(), newTestDeps(t, classic), testTask())

			if got.ErrorType != tc.want || want.ErrorType != tc.want || got.ErrorDetail != want.ErrorDetail {
				t.Errorf("porcelain = %q/%q, classic = %q/%q, want both %q", got.ErrorType, got.ErrorDetail, want.ErrorType, want.ErrorDetail, tc.want)
			}
		})
	}
}

// A push that reports a rejected ref has failed even if git's exit status says otherwise: recording it
// as a success would tell the platform the replica is current.
func TestSyncRepoRejectedRefWithZeroExitIsAFailure(t *testing.T) {
	f := &fakeGit{stdout: outputs{push: "To x\n!\trefs/heads/b:refs/heads/b\t[remote rejected] (pre-receive hook declined)\nDone\n"}.fn}
	got := syncRepo(context.Background(), newStatsDeps(t, f), testTask())
	if got.State != StateError || got.ErrorType != ErrReplicaRejected {
		t.Errorf("result = %+v, want error/replica_rejected", got)
	}
}

func TestSyncRepoCancelledWhileReadingTheNewestCommitIsPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeGit{script: func(c gitCall) ([]byte, error) {
		if c.sub() == "for-each-ref" {
			cancel()
			return nil, context.Canceled
		}
		return nil, nil
	}}
	got := syncRepo(ctx, newStatsDeps(t, f), testTask())
	if got.State != StatePending {
		t.Errorf("result = %+v, want pending (a cancelled sync records nothing)", got)
	}
	if slices.Contains(f.subs(), "push") {
		t.Errorf("sequence = %v: nothing may be pushed after cancellation", f.subs())
	}
}

// The default-branch lookup runs after the push has finished, so a shutdown that lands during it must
// not turn a completed push into a cancelled one.
func TestSyncRepoCancelledAfterThePushKeepsThePushOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeGit{
		stdout: outputs{push: pushedTwo}.fn,
		script: func(c gitCall) ([]byte, error) {
			if c.sub() == "ls-remote" {
				cancel()
				return nil, context.Canceled
			}
			return nil, nil
		},
	}
	got := syncRepo(ctx, newStatsDeps(t, f), testTask())
	if got.State != StateOK || got.Stats.RefsPushed == nil || *got.Stats.RefsPushed != 2 {
		t.Errorf("result = %+v, want the completed push reported as ok", got)
	}
}

// A committer date comes from the origin and can be anything. One that is further ahead than any
// clock skew explains is not reported as the newest commit.
func TestSyncRepoIgnoresCommitDatesFromTheFarFuture(t *testing.T) {
	f := &fakeGit{stdout: outputs{forEachRef: "1609459200|\n4102444800|\n|4102444801\n"}.fn}
	got := syncRepo(context.Background(), newStatsDeps(t, f), testTask())
	if want := time.Unix(1609459200, 0).UTC(); !got.Stats.NewestCommitAt.Equal(want) {
		t.Errorf("NewestCommitAt = %v, want %v (the year-2100 dates are dropped)", got.Stats.NewestCommitAt, want)
	}
}
