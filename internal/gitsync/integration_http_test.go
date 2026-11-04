package gitsync

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests drive the real git binary over HTTP, against `git http-backend`, so the credentials,
// the URL-scoped headers and — above all — git's ACTUAL error output are exercised end to end. The
// classifier's unit tests feed it stderr strings written from memory; these prove the strings are
// what git really prints.

// gitHTTPServer serves the bare repositories under a root over smart HTTP, requiring a fixed
// Authorization header and recording every one it sees.
type gitHTTPServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func (s *gitHTTPServer) authHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

// newGitHTTPServer starts the server; denyStatus is what a request without wantAuth receives.
func newGitHTTPServer(t *testing.T, root, wantAuth string, denyStatus int) *gitHTTPServer {
	t.Helper()
	backend := filepath.Join(strings.TrimSpace(git(t, "", "--exec-path")), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not available: %v", err)
	}
	handler := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=test",
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + t.TempDir(),
		},
		InheritEnv: []string{"PATH"},
	}
	s := &gitHTTPServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		s.mu.Lock()
		s.seen = append(s.seen, got)
		s.mu.Unlock()
		if got != wantAuth {
			http.Error(w, "denied", denyStatus)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

const (
	httpOriginToken = "origin-token-abcdefghij"
	httpPushToken   = "push-token-abcdefghij"
)

// httpFixture is an origin and a replica, each behind its own credential-checking HTTP server.
type httpFixture struct {
	origin, replica *gitHTTPServer
	originDir       string
	replicaDir      string
	task            syncTask
	deps            syncDeps
}

func newHTTPFixture(t *testing.T) *httpFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	f := &httpFixture{originDir: filepath.Join(root, "o", "origin.git"), replicaDir: filepath.Join(root, "r", "replica.git")}
	work := filepath.Join(root, "work")
	for _, d := range []string{f.originDir, f.replicaDir} {
		git(t, "", "init", "--bare", "-b", "main", d)
	}
	git(t, "", "init", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "file.txt")
	git(t, work, "commit", "-m", "one")
	git(t, work, "tag", "v1")
	git(t, work, "push", f.originDir, "main", "v1")
	// A hidden namespace, as a forge keeps for pull requests: the origin advertises it, the sync
	// must neither mirror nor push it.
	git(t, "", "--git-dir="+f.originDir, "update-ref", "refs/pull/1/head", git(t, work, "rev-parse", "HEAD"))

	f.origin = newGitHTTPServer(t, filepath.Join(root, "o"), "Bearer "+httpOriginToken, http.StatusUnauthorized)
	f.replica = newGitHTTPServer(t, filepath.Join(root, "r"), "Bearer "+httpPushToken, http.StatusUnauthorized)
	f.task = syncTask{
		RepoID:       "repo-a",
		CanonicalURL: f.origin.URL + "/origin.git",
		Origin:       LocalEntry{BaseURL: f.origin.URL, Auth: "bearer", Token: Secret(httpOriginToken)},
		ReplicaURL:   f.replica.URL + "/replica.git",
		PushToken:    Secret(httpPushToken),
	}
	f.deps = syncDeps{mirrors: Mirrors{Root: filepath.Join(root, "mirrors")}, run: runGit, now: time.Now}
	return f
}

func (f *httpFixture) sync() syncResult {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return syncRepo(ctx, f.deps, f.task)
}

// The first fetch into the freshly initialised mirror, a later fetch and the push all authenticate
// over real HTTP, each credential is scoped to its own host, and neither server ever sees the other's
// credential.
func TestSyncRepoOverHTTPKeepsEachCredentialToItsOwnHost(t *testing.T) {
	f := newHTTPFixture(t)

	if got := f.sync(); got.State != StateOK {
		t.Fatalf("first sync (init + fetch + push) = %+v, want ok", got)
	}
	want := []string{"refs/heads/main", "refs/tags/v1"}
	if got := refs(t, f.replicaDir); !slices.Equal(got, want) {
		t.Fatalf("replica refs = %v, want %v", got, want)
	}
	if got := refs(t, f.deps.mirrors.Dir("repo-a")); !slices.Equal(got, want) {
		t.Fatalf("mirror refs = %v, want %v (no refs/pull/*)", got, want)
	}
	if got := f.sync(); got.State != StateOK { // the mirror exists now: a fetch
		t.Fatalf("second sync (fetch + push) = %+v, want ok", got)
	}

	for _, h := range f.origin.authHeaders() {
		if h != "Bearer "+httpOriginToken {
			t.Errorf("the origin server received %q, want only its own credential", h)
		}
	}
	for _, h := range f.replica.authHeaders() {
		if h != "Bearer "+httpPushToken {
			t.Errorf("the replica server received %q, want only the push token", h)
		}
	}
	if len(f.origin.authHeaders()) == 0 || len(f.replica.authHeaders()) == 0 {
		t.Error("a server was never contacted; the test proved nothing")
	}
}

// The classifier's keywords are matched against what git really prints for each failure.
func TestSyncRepoClassifiesRealGitHTTPFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(f *httpFixture)
		want   string
	}{
		{"origin rejects the credential (401)", func(f *httpFixture) { f.task.Origin.Token = "wrong-origin-token-xyz" }, ErrOriginAccessDenied},
		{"origin repository does not exist (404)", func(f *httpFixture) { f.task.CanonicalURL = f.origin.URL + "/missing.git" }, ErrOriginAccessDenied},
		{"origin is down", func(f *httpFixture) { f.origin.Close() }, ErrOriginUnreachable},
		{"replica rejects the push token (401)", func(f *httpFixture) { f.task.PushToken = "wrong-push-token-xyz1" }, ErrCredentialRejected},
		{"replica is down", func(f *httpFixture) { f.replica.Close() }, ErrReplicaUnreachable},
		{"replica rejects an expired token", func(f *httpFixture) {
			f.task.PushToken = "wrong-push-token-xyz1"
			f.task.TokenExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		}, ErrPushTokenExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newHTTPFixture(t)
			tt.mutate(f)

			got := f.sync()
			if got.State != StateError || got.ErrorType != tt.want {
				t.Fatalf("result = %+v, want error/%s", got, tt.want)
			}
			if strings.Contains(got.ErrorDetail, "127.0.0.1") || strings.Contains(got.ErrorDetail, "http") {
				t.Errorf("error_detail leaks a location: %q", got.ErrorDetail)
			}
		})
	}
}
