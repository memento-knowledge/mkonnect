package gitsync

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// These tests drive the real git binary against servers that answer a fixed HTTP status, to prove that
// what git ACTUALLY prints for a redirect, a missing repository and a rate limit is what the
// classifier reads, on each side of the sync.

func TestSyncRepoClassifiesRealGitStatusAnswers(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(f *httpFixture)
		wantType   string
		wantDetail string
		// wantDetailContains is for unclassified, whose detail is git's own scrubbed line.
		wantDetailContains string
	}{
		{"origin 301", func(f *httpFixture) { f.origin.forceStatus.Store(301) }, ErrOriginURLRedirected, detailOriginURLRedirected, ""},
		{"origin 302", func(f *httpFixture) { f.origin.forceStatus.Store(302) }, ErrOriginURLRedirected, detailOriginURLRedirected, ""},
		{"origin 307", func(f *httpFixture) { f.origin.forceStatus.Store(307) }, ErrOriginURLRedirected, detailOriginURLRedirected, ""},
		{"origin 308", func(f *httpFixture) { f.origin.forceStatus.Store(308) }, ErrOriginURLRedirected, detailOriginURLRedirected, ""},
		{"origin 404 stays access denied", func(f *httpFixture) { f.origin.forceStatus.Store(404) }, ErrOriginAccessDenied, "", ""},
		{"origin 429 has no type of its own", func(f *httpFixture) { f.origin.forceStatus.Store(429) }, ErrUnclassified, "", "returned error: 429"},

		{"replica 429", func(f *httpFixture) { f.replica.forceStatus.Store(429) }, ErrReplicaRateLimited, detailReplicaRateLimited, ""},
		{"replica 404", func(f *httpFixture) { f.replica.forceStatus.Store(404) }, ErrReplicaRepositoryNotFound, detailReplicaRepositoryNotFound, ""},
		{"replica 503", func(f *httpFixture) { f.replica.forceStatus.Store(503) }, ErrReplicaUnreachable, "", ""},
		{"replica 403", func(f *httpFixture) { f.replica.forceStatus.Store(403) }, ErrCredentialRejected, "", ""},
		{"replica 301 has no type of its own", func(f *httpFixture) { f.replica.forceStatus.Store(301) }, ErrUnclassified, "", "returned error: 301"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newHTTPFixture(t)
			tt.mutate(f)

			got := f.sync()
			if got.State != StateError || got.ErrorType != tt.wantType {
				t.Fatalf("result = %+v, want error/%s", got, tt.wantType)
			}
			if tt.wantDetailContains == "" && got.ErrorDetail != tt.wantDetail {
				t.Errorf("error_detail = %q, want %q", got.ErrorDetail, tt.wantDetail)
			}
			if !strings.Contains(got.ErrorDetail, tt.wantDetailContains) {
				t.Errorf("error_detail = %q, want it to contain %q", got.ErrorDetail, tt.wantDetailContains)
			}
			for _, leak := range []string{"127.0.0.1", "http", "other.example.net", "/moved/", "location", "origin.git", "replica.git"} {
				if strings.Contains(strings.ToLower(got.ErrorDetail), leak) {
					t.Errorf("error_detail %q leaks %q", got.ErrorDetail, leak)
				}
			}
			if got.OriginCredentialRejected {
				t.Error("a status answer other than a refused credential must not close the provider's gate")
			}
		})
	}
}

// redirectServer answers every request with a 301 to target, as a forge does for a repository URL
// without its ".git" suffix, and counts the requests it received.
func redirectServer(t *testing.T, target string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusMovedPermanently)
	}))
	t.Cleanup(s.Close)
	return s, &hits
}

// A repository id whose origin URL was wrong when it first synced (a redirected URL) syncs once the
// platform sends the corrected one. The fetch names the URL itself, so nothing about the mirror (its
// existence, its shape, a stale remote.origin.url) binds the repository to the URL it was made for.
func TestCorrectedOriginURLSyncsAnExistingMirror(t *testing.T) {
	branchesAndTags := []string{"refs/heads/main", "refs/tags/v1"}

	setup := func(t *testing.T) (f *httpFixture, wrongURL string, wrongHits *atomic.Int32) {
		f = newHTTPFixture(t)
		// Statistics run extra origin requests (the default branch); they must follow the corrected URL too.
		f.deps.runOut = runGitOut
		srv, hits := redirectServer(t, f.origin.URL)
		// No ".git" suffix: the forge redirects it to the canonical clone URL.
		return f, srv.URL + "/origin", hits
	}
	corrected := func(f *httpFixture) string { return f.origin.URL + "/origin.git" }
	mirror := func(f *httpFixture) string { return f.deps.mirrors.Dir(f.task.RepoID) }
	wantRedirected := func(t *testing.T, got syncResult) {
		t.Helper()
		if got.State != StateError || got.ErrorType != ErrOriginURLRedirected || got.ErrorDetail != detailOriginURLRedirected {
			t.Fatalf("sync of the redirected URL = %+v, want error/%s with its hint", got, ErrOriginURLRedirected)
		}
	}
	wantSynced := func(t *testing.T, f *httpFixture, got syncResult) {
		t.Helper()
		if got.State != StateOK || got.ErrorType != "" || got.ErrorDetail != "" {
			t.Fatalf("sync of the corrected URL = %+v, want ok with no error carried over", got)
		}
		if refs := refs(t, f.replicaDir); !slices.Equal(refs, branchesAndTags) {
			t.Fatalf("replica refs = %v, want %v", refs, branchesAndTags)
		}
		if got.Stats.DefaultBranch != "main" {
			t.Errorf("default branch = %q, want main: the extra origin request did not follow the corrected URL", got.Stats.DefaultBranch)
		}
	}

	t.Run("a fresh mirror: the first sync failed and left nothing behind", func(t *testing.T) {
		f, wrong, hits := setup(t)
		f.task.CanonicalURL = wrong
		wantRedirected(t, f.sync())
		if f.deps.mirrors.Exists(f.task.RepoID) {
			t.Fatal("a failed first fetch left a mirror behind")
		}
		seen := hits.Load()
		if seen == 0 {
			t.Fatal("the redirecting server was never contacted; the test proved nothing")
		}

		f.task.CanonicalURL = corrected(f)
		wantSynced(t, f, f.sync())
		if hits.Load() != seen {
			t.Errorf("the redirecting server was contacted again after the URL was corrected")
		}
	})

	t.Run("a mirror made by an earlier sync", func(t *testing.T) {
		f, wrong, hits := setup(t)
		wantSynced(t, f, f.sync())
		before := refs(t, mirror(f))

		f.task.CanonicalURL = wrong
		wantRedirected(t, f.sync())
		if !f.deps.mirrors.Exists(f.task.RepoID) || !slices.Equal(refs(t, mirror(f)), before) {
			t.Fatal("a redirect must not cost the repository its mirror")
		}
		seen := hits.Load()

		f.task.CanonicalURL = corrected(f)
		wantSynced(t, f, f.sync())
		if hits.Load() != seen {
			t.Errorf("the redirecting server was contacted again after the URL was corrected")
		}
	})

	t.Run("a legacy clone --mirror mirror whose remote.origin.url is the stale URL", func(t *testing.T) {
		f, wrong, hits := setup(t)
		if err := f.deps.mirrors.EnsureRoot(); err != nil {
			t.Fatal(err)
		}
		git(t, "", "clone", "--mirror", "file://"+f.originDir, mirror(f))
		git(t, "", "--git-dir="+mirror(f), "config", "remote.origin.url", wrong)

		f.task.CanonicalURL = corrected(f)
		wantSynced(t, f, f.sync())
		if hits.Load() != 0 {
			t.Errorf("the stale remote.origin.url was contacted %d time(s); the fetch must name the URL itself", hits.Load())
		}
		if got := git(t, "", "--git-dir="+mirror(f), "config", "remote.origin.url"); got != wrong {
			t.Errorf("remote.origin.url = %q, want it left alone (%q): it is never read", got, wrong)
		}
		// The legacy namespaces stay on disk and still never reach the replica.
		if !slices.Contains(refs(t, mirror(f)), "refs/pull/1/head") {
			t.Error("the legacy mirror lost its extra namespace")
		}
	})

	t.Run("an empty bare repository left by an interrupted first sync", func(t *testing.T) {
		f, _, _ := setup(t)
		if err := f.deps.mirrors.EnsureRoot(); err != nil {
			t.Fatal(err)
		}
		git(t, "", "init", "--bare", "-b", "main", mirror(f))

		f.task.CanonicalURL = corrected(f)
		wantSynced(t, f, f.sync())
	})

	// The URL is the source of truth for what a repository mirrors. Pointing the same id at another
	// repository makes the mirror and the replica follow it; an empty one is refused and the replica is
	// left as it was.
	t.Run("a corrected URL that names a different repository", func(t *testing.T) {
		f, _, _ := setup(t)
		wantSynced(t, f, f.sync())

		other := filepath.Join(filepath.Dir(f.originDir), "other.git")
		git(t, "", "init", "--bare", "-b", "main", other)
		work := t.TempDir()
		git(t, "", "init", "-b", "main", work)
		if err := os.WriteFile(filepath.Join(work, "other.txt"), []byte("unrelated"), 0o600); err != nil {
			t.Fatal(err)
		}
		git(t, work, "add", "other.txt")
		git(t, work, "commit", "-m", "unrelated")
		git(t, work, "push", other, "main")

		f.task.CanonicalURL = f.origin.URL + "/other.git"
		if got := f.sync(); got.State != StateOK {
			t.Fatalf("sync of the other repository = %+v, want ok", got)
		}
		if a, b := git(t, "", "--git-dir="+f.replicaDir, "rev-parse", "main"), git(t, "", "--git-dir="+other, "rev-parse", "main"); a != b {
			t.Errorf("replica main = %s, want the other repository's %s", a, b)
		}

		empty := filepath.Join(filepath.Dir(f.originDir), "empty.git")
		git(t, "", "init", "--bare", "-b", "main", empty)
		before := git(t, "", "--git-dir="+f.replicaDir, "rev-parse", "main")
		f.task.CanonicalURL = f.origin.URL + "/empty.git"
		if got := f.sync(); got.State != StateError || got.ErrorType != ErrOriginEmpty || got.ErrorDetail != detailOriginEmpty {
			t.Fatalf("sync of an empty repository = %+v, want the empty-origin error", got)
		}
		if after := git(t, "", "--git-dir="+f.replicaDir, "rev-parse", "main"); after != before {
			t.Errorf("replica main moved from %s to %s for an empty origin", before, after)
		}
	})
}
