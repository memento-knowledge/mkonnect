package gitsync

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
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

	"github.com/memento-knowledge/mkonnect/internal/testca"
)

// THE REPLICA RIG (fast).
//
// This file is the shared, test-only harness for scenarios in which the connector pushes to a replica
// that sits behind a load balancer. It is a *_test.go file: it is compiled into the test binary only
// and is excluded from every image and package (see RIG.md and scripts/check-artifact-contents.sh).
// Other tests in this package, including slower ones, build on it.
//
// replicaFront is the load balancer: a TLS server (a throwaway CA from internal/testca) in front of a
// stock `git http-backend` serving one bare repository. Everything the real git binary sends goes
// through it, so the strings the classifier sees are what git really prints. Its knobs are:
//
//	token / RotateToken      the Bearer token it accepts; any other Authorization answers 401
//	SetMaxBody(n)            a request-body limit: a push whose body exceeds n bytes answers 413
//	StallPushes()            requests of a push (the receive-pack advertisement and the POST) hang
//	                         until the client gives up, as a replica that stopped answering does
//	UnstallPushes()          the stall ends
//	FailPushes(status)       requests of a push are answered at once with that status (for example a
//	                         504 from a gateway that gave up on the replica); 0 ends it
//	RevokeAfterPushes(n)     after n completed pushes every request answers 401 (the credential is
//	                         lost in the middle of a multi-batch push); Revoke / Restore do it by hand
//	RevokePushAuth()         only the POSTs of pushes answer 401, the advertisement still passes (a
//	                         token that expires between the two requests of one push); Restore ends it
//	OnPush(f)                f(n) runs after the n-th completed push, before its response is sent, so
//	                         a test can look at the replica exactly between two pushes
//	Requests / Pushes /      what the front saw, for assertions
//	Stalled
//
// replicaRig combines the front with batchEnv (an origin and a replica on disk, a recorder around the
// real git runner, the limits and the injected clock) so a scenario is a few lines.
//
// The connector reaches a custom CA the way the daemon does: through GIT_SSL_CAINFO, which gitBaseEnv
// passes on from its own environment. Tests that run in parallel cannot set the environment, so the
// rig wraps the git runners and adds the variable to the environment git is started with (trust);
// TestReplicaRigTrustsACustomCAThroughTheDaemonEnvironment proves the real path with t.Setenv.
//
// The front mimics a load balancer and stock git; it is NOT the production replica service, which may
// answer differently (see RIG.md).

// frontRequest is one request the front received.
type frontRequest struct {
	Method  string
	Path    string
	Service string // the ?service= of a ref advertisement, "" otherwise
	Status  int    // 0 when the request was abandoned (a stall that the client gave up on)
	Auth    string // the Authorization header exactly as received
	Push    bool   // a POST to git-receive-pack
}

// replicaFront is the load balancer; see the comment at the top of the file.
type replicaFront struct {
	*httptest.Server
	// CAFile is a PEM file holding the CA that signed the front's certificate.
	CAFile string

	prefix  string // "/<repository directory name>/", the only path the front forwards
	backend *cgi.Handler
	release chan struct{} // closed on cleanup, ends every stall

	mu          sync.Mutex
	token       string
	tokens      []string // every token the front has accepted, oldest first
	maxBody     int64
	revoked     bool
	revokeAfter int
	stall       bool
	failStatus  int
	revokePost  bool
	pushes      int
	onPush      func(n int)
	reqs        []frontRequest
	stalled     int

	cgiErr lockedBuffer
}

// lockedBuffer is a bytes.Buffer safe for the concurrent writes of a CGI child.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newReplicaFront starts a TLS front end for the bare repository at replicaDir, which is reachable at
// ReplicaURL(). It skips the test when git-http-backend is not installed.
func newReplicaFront(t *testing.T, replicaDir string) *replicaFront {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	backend := filepath.Join(strings.TrimSpace(git(t, "", "--exec-path")), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not available: %v", err)
	}
	caPEM, leaf, err := testca.GenerateCA("127.0.0.1")
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	f := &replicaFront{
		prefix:  "/" + filepath.Base(replicaDir) + "/",
		release: make(chan struct{}),
		CAFile:  filepath.Join(t.TempDir(), "front-ca.pem"),
	}
	if err := os.WriteFile(f.CAFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	f.token = newRigToken()
	f.tokens = []string{f.token}
	f.backend = &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Dir(replicaDir), "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=test",
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + t.TempDir(),
		},
		InheritEnv: []string{"PATH"},
		Stderr:     &f.cgiErr,
	}
	f.Server = httptest.NewUnstartedServer(f)
	f.Server.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12}
	// A client that rejects the certificate aborts the handshake; that is an expected event, not noise.
	f.Server.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.Server.StartTLS()
	t.Cleanup(func() {
		close(f.release)
		f.Server.Close()
		if t.Failed() {
			if s := f.cgiErr.String(); s != "" {
				t.Logf("git http-backend stderr:\n%s", s)
			}
		}
	})
	return f
}

// newRigToken returns a fresh random token, so a search for it in the connector's output and files
// cannot match anything else.
func newRigToken() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "rig-token-" + hex.EncodeToString(b)
}

// ReplicaURL is the URL of the repository behind the front.
func (f *replicaFront) ReplicaURL() string { return f.URL + f.prefix[:len(f.prefix)-1] }

// Token is the Bearer token the front accepts now.
func (f *replicaFront) Token() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.token
}

// RotateToken makes the front accept only a new token, and returns it.
func (f *replicaFront) RotateToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = newRigToken()
	f.tokens = append(f.tokens, f.token)
	return f.token
}

// KnownTokens is every token the front has accepted so far.
func (f *replicaFront) KnownTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tokens)
}

// SetMaxBody sets the request-body limit; 0 removes it.
func (f *replicaFront) SetMaxBody(n int64) { f.update(func() { f.maxBody = n }) }

// StallPushes makes the requests of a push hang; see the comment at the top of the file.
func (f *replicaFront) StallPushes() { f.update(func() { f.stall = true }) }

// FailPushes answers the requests of a push at once with status; 0 ends it.
func (f *replicaFront) FailPushes(status int) { f.update(func() { f.failStatus = status }) }

// RevokePushAuth makes the POSTs of pushes answer 401 until Restore.
func (f *replicaFront) RevokePushAuth() { f.update(func() { f.revokePost = true }) }

// UnstallPushes ends the stall for later requests.
func (f *replicaFront) UnstallPushes() { f.update(func() { f.stall = false }) }

// RevokeAfterPushes makes every request answer 401 once n pushes have completed (counting from now).
func (f *replicaFront) RevokeAfterPushes(n int) {
	f.update(func() { f.revokeAfter = f.pushes + n })
}

// Revoke makes every request answer 401 until Restore.
func (f *replicaFront) Revoke() { f.update(func() { f.revoked = true }) }

// Restore ends a revocation, whether by hand or by RevokeAfterPushes.
func (f *replicaFront) Restore() {
	f.update(func() { f.revoked, f.revokePost, f.revokeAfter = false, false, 0 })
}

// OnPush sets a hook that runs after each completed push, before the client gets the response.
func (f *replicaFront) OnPush(h func(n int)) { f.update(func() { f.onPush = h }) }

func (f *replicaFront) update(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

// Requests returns every request received so far, in order of completion.
func (f *replicaFront) Requests() []frontRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// Pushes is how many receive-pack POSTs completed successfully. A push that sends objects is one; a
// push of a large pack is preceded by a small probe POST that counts as well.
func (f *replicaFront) Pushes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pushes
}

// Stalled is how many requests reached a stall.
func (f *replicaFront) Stalled() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stalled
}

// Count is how many requests were answered with status.
func (f *replicaFront) Count(status int) int {
	n := 0
	for _, r := range f.Requests() {
		if r.Status == status {
			n++
		}
	}
	return n
}

// capture buffers a CGI response so the front can act on it (count the push, run the hook, revoke)
// before the client sees it. Responses on the replica side are tiny.
type capture struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (c *capture) Header() http.Header { return c.h }
func (c *capture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}
func (c *capture) Write(p []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	return c.body.Write(p)
}

// statusWriter remembers the status of a response written straight to the client.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// maxDrain bounds how much of a refused request body the front reads before answering.
const maxDrain = 16 << 20

// maxBuffer bounds the push body the front buffers when no body limit is set.
const maxBuffer = 64 << 20

func (f *replicaFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := frontRequest{
		Method: r.Method, Path: r.URL.Path, Service: r.URL.Query().Get("service"),
		Auth: r.Header.Get("Authorization"),
		Push: r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack"),
	}
	sw := &statusWriter{ResponseWriter: w}
	defer func() {
		rec.Status = sw.status
		f.update(func() { f.reqs = append(f.reqs, rec) })
	}()

	f.mu.Lock()
	token, revoked, maxBody, stall, failStatus, revokePost := f.token, f.revoked, f.maxBody, f.stall, f.failStatus, f.revokePost
	f.mu.Unlock()

	switch {
	case !strings.HasPrefix(r.URL.Path, f.prefix):
		http.NotFound(sw, r)
		return
	case revoked || (revokePost && rec.Push) || rec.Auth != "Bearer "+token:
		http.Error(sw, "denied", http.StatusUnauthorized)
		return
	}

	if rec.Push {
		// Like a proxy that buffers requests, the front reads a push in full and forwards it with a
		// length: CGI (net/http/cgi) cannot take a chunked body.
		limit := maxBody
		if limit <= 0 {
			limit = maxBuffer
		}
		body, tooLarge := readLimited(r.Body, r.ContentLength, limit)
		if tooLarge {
			// Read the rest first so the answer is not racing a client still sending: what git
			// reports is then the status, as with a proxy that buffers requests.
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxDrain))
			http.Error(sw, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		r.TransferEncoding = nil // net/http/cgi refuses chunked requests
	}

	isPushRequest := rec.Push || rec.Service == "git-receive-pack"
	if isPushRequest && failStatus != 0 {
		http.Error(sw, "upstream failure", failStatus)
		return
	}
	if isPushRequest && stall {
		f.update(func() { f.stalled++ })
		// With the body read, the server notices the client going away and ends r.Context().
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxDrain))
		select {
		case <-r.Context().Done(): // the client gave up
		case <-f.release:
		}
		return
	}

	c := &capture{h: http.Header{}}
	f.backend.ServeHTTP(c, r)
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if rec.Push && c.status/100 == 2 {
		f.pushDone()
	}
	for k, v := range c.h {
		sw.Header()[k] = v
	}
	sw.WriteHeader(c.status)
	_, _ = sw.Write(c.body.Bytes())
}

// pushDone counts a completed push, runs the hook and applies a pending revocation. It runs before
// the client sees the response, so what a test observes in the hook, and the 401 the next request
// gets, are the same on every run.
func (f *replicaFront) pushDone() {
	f.mu.Lock()
	f.pushes++
	n, hook := f.pushes, f.onPush
	if f.revokeAfter > 0 && n >= f.revokeAfter {
		f.revoked = true
	}
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
}

// readLimited reads a request body of the given declared length (-1 when unknown, as for a chunked
// body), at most limit bytes of it. A body of declared length is judged without being read.
func readLimited(body io.Reader, declared, limit int64) (data []byte, tooLarge bool) {
	if declared > limit {
		return nil, true
	}
	data, _ = io.ReadAll(io.LimitReader(body, limit+1))
	return data, int64(len(data)) > limit
}

// trust makes a git runner trust the front's CA, as the daemon's GIT_SSL_CAINFO setting does.
func (f *replicaFront) trust(env []string) []string {
	return append(slices.Clone(env), "GIT_SSL_CAINFO="+f.CAFile)
}

func (f *replicaFront) trustRun(run runFunc) runFunc {
	return func(ctx context.Context, argv, env []string, dir string) ([]byte, error) {
		return run(ctx, argv, f.trust(env), dir)
	}
}

func (f *replicaFront) trustRunOut(run outRunFunc) outRunFunc {
	return func(ctx context.Context, argv, env []string, dir string, w io.Writer) ([]byte, error) {
		return run(ctx, argv, f.trust(env), dir, w)
	}
}

// replicaRig is a batchEnv whose replica is reached through a replicaFront, over TLS, with the
// connector trusting the front's CA.
type replicaRig struct {
	*batchEnv
	front *replicaFront
}

// newReplicaRig builds an origin with main, the given number of branches and five tags, and an empty
// replica behind a front. Callers decide whether the test runs in parallel.
func newReplicaRig(t *testing.T, branches int) *replicaRig {
	t.Helper()
	e := newBatchEnv(t, branches)
	front := newReplicaFront(t, e.replica)
	e.task.ReplicaURL = front.ReplicaURL()
	e.task.PushToken = Secret(front.Token())
	e.d.run, e.d.runOut = front.trustRun(e.d.run), front.trustRunOut(e.d.runOut)
	return &replicaRig{batchEnv: e, front: front}
}

// syncOnce runs one sync with a generous deadline (it only guards against a hang) and checks the
// result and everything the sync left behind for the front's token.
func (r *replicaRig) syncOnce() syncResult {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res := r.sync(ctx)
	r.wantNoCredentialLeak(res)
	return res
}

// syncOK is syncOnce for a sync that must succeed.
func (r *replicaRig) syncOK() syncResult {
	r.t.Helper()
	res := r.syncOnce()
	if res.State != StateOK {
		r.t.Fatalf("sync = %+v, want ok", res)
	}
	return res
}

// wantError checks that a sync failed with the given error type and no detail (a classified failure
// carries none), and returns it.
func (r *replicaRig) wantError(res syncResult, errorType string) {
	r.t.Helper()
	if res.State != StateError || res.ErrorType != errorType || res.ErrorDetail != "" {
		r.t.Fatalf("sync = %+v, want error/%s with no detail", res, errorType)
	}
}

// wantRefsPushed checks the refs_pushed statistic.
func (r *replicaRig) wantRefsPushed(res syncResult, want int) {
	r.t.Helper()
	if res.Stats.RefsPushed == nil || *res.Stats.RefsPushed != want {
		got := "unmeasured"
		if res.Stats.RefsPushed != nil {
			got = fmt.Sprint(*res.Stats.RefsPushed)
		}
		r.t.Errorf("refs pushed = %s, want %d", got, want)
	}
}

// wantMirrorEqualsOrigin checks that the connector's mirror holds exactly the origin's branches and
// tags.
func (r *replicaRig) wantMirrorEqualsOrigin() {
	r.t.Helper()
	if got, want := refs(r.t, r.d.mirrors.Dir(r.task.RepoID)), refs(r.t, r.origin); !slices.Equal(got, want) {
		r.t.Errorf("mirror refs = %v\nwant the origin's %v", got, want)
	}
}

// wantSameTips checks that the replica, the mirror and the origin agree on where every ref points.
func (r *replicaRig) wantSameTips() {
	r.t.Helper()
	tips := func(dir string) string {
		return git(r.t, "", "--git-dir="+dir, "for-each-ref", "--format=%(refname) %(objectname)")
	}
	want := tips(r.origin)
	if got := tips(r.replica); got != want {
		r.t.Errorf("replica tips:\n%s\nwant the origin's:\n%s", got, want)
	}
	if got := tips(r.d.mirrors.Dir(r.task.RepoID)); got != want {
		r.t.Errorf("mirror tips:\n%s\nwant the origin's:\n%s", got, want)
	}
}

// wantNoCredentialLeak checks that no token the front has accepted appears in the sync result, in any
// file of the mirror, or in the argument list of any push, and that every request the front saw
// carried exactly one of the connector's own tokens (nothing else was sent to it).
func (r *replicaRig) wantNoCredentialLeak(res syncResult) {
	r.t.Helper()
	tokens := r.front.KnownTokens()
	if cur := r.task.PushToken.Reveal(); !slices.Contains(tokens, cur) {
		tokens = append(tokens, cur)
	}
	text := fmt.Sprintf("%+v %#v", res, res)
	for _, c := range r.rec.pushes() {
		text += "\x00" + strings.Join(c.argv, "\x00")
	}
	for _, tok := range tokens {
		if strings.Contains(text, tok) {
			r.t.Errorf("a credential appears in the sync result or in a push's arguments: %q", text)
		}
	}
	_ = filepath.WalkDir(r.d.mirrors.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil {
			for _, tok := range tokens {
				if bytes.Contains(data, []byte(tok)) {
					r.t.Errorf("a credential was written to the mirror: %s", path)
				}
			}
		}
		return nil
	})
	for _, req := range r.front.Requests() {
		if !slices.ContainsFunc(tokens, func(tok string) bool { return req.Auth == "Bearer "+tok }) {
			r.t.Errorf("the front received Authorization %q, want only the connector's own token", req.Auth)
		}
	}
}

// commit commits content on the origin's main (through the work tree) and force-pushes it, and returns
// the new tip.
func (r *replicaRig) commit(content string) string {
	r.t.Helper()
	r.writeWork(content)
	git(r.t, r.work, "add", "file.txt")
	git(r.t, r.work, "commit", "-m", content)
	git(r.t, r.work, "push", "--force", r.origin, "main")
	return git(r.t, r.work, "rev-parse", "HEAD")
}

// amend rewrites the tip of the origin's main with different content and force-pushes it.
func (r *replicaRig) amend(content string) string {
	r.t.Helper()
	r.writeWork(content)
	git(r.t, r.work, "add", "file.txt")
	git(r.t, r.work, "commit", "--amend", "-m", content)
	git(r.t, r.work, "push", "--force", r.origin, "main")
	return git(r.t, r.work, "rev-parse", "HEAD")
}

// rollback force-pushes the origin's main back to an earlier commit.
func (r *replicaRig) rollback(sha string) {
	r.t.Helper()
	git(r.t, r.work, "reset", "--hard", sha)
	git(r.t, r.work, "push", "--force", r.origin, "main")
}

func (r *replicaRig) writeWork(content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.work, "file.txt"), []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// updateOriginRefs runs `git update-ref --stdin` on the origin with one command per line.
func (r *replicaRig) updateOriginRefs(commands ...string) {
	r.t.Helper()
	gitStdin(r.t, "", strings.Join(commands, "\n")+"\n", "--git-dir="+r.origin, "update-ref", "--stdin")
}

// tip returns what ref points at in the repository at gitDir.
func tip(t *testing.T, gitDir, ref string) string {
	t.Helper()
	return git(t, "", "--git-dir="+gitDir, "rev-parse", ref)
}

// replicaRefNames lists the replica's refs without failing the test, so a front hook (which runs on
// the server's goroutine) can call it.
func replicaRefNames(gitDir string) ([]string, error) {
	cmd := exec.Command("git", "--git-dir="+gitDir, "for-each-ref", "--format=%(refname)")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	names := strings.Fields(string(out))
	slices.Sort(names)
	return names, nil
}
