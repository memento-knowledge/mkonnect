package gitsync_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/gitsync/cli"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// The canary test runs the connector end to end — the real Client, the real git binary, real HTTPS
// servers speaking git's smart protocol — with a unique random credential for each end of the sync, and
// then searches everything the run produced for any part of those credentials. The only places a
// credential may appear are the one place it is meant to be: the Authorization header git is given as a
// URL-scoped http.<url>/.extraHeader config entry (and so the request the matching server receives),
// and, for the origin token, the provider config file the operator wrote.

const canaryUser = "svc-user"

// canaryServer is a TLS git server (git http-backend) requiring one Authorization header. It records
// the header of every request, and can instead misbehave in the ways a hostile or buggy server might.
type canaryServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string

	wantAuth string
	// reflect answers every ref advertisement with a git ERR packet carrying the Authorization header it
	// received: git prints it as "fatal: remote error: <text>".
	reflect bool
	// redirectTo, when set, answers every request with a redirect to that base URL.
	redirectTo string
}

// update changes how the server behaves; the handler runs on other goroutines.
func (s *canaryServer) update(f func(s *canaryServer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *canaryServer) headers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

func newCanaryServer(t *testing.T, root, wantAuth string) *canaryServer {
	t.Helper()
	execPath := runGitCmd(t, "", "--exec-path")
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not available: %v", err)
	}
	cgiHandler := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=test",
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + t.TempDir(),
		},
		InheritEnv: []string{"PATH"},
		Stderr:     io.Discard,
	}
	s := &canaryServer{wantAuth: wantAuth}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		s.mu.Lock()
		s.seen = append(s.seen, got)
		wantAuth, reflect, redirectTo := s.wantAuth, s.reflect, s.redirectTo
		s.mu.Unlock()
		switch svc := r.URL.Query().Get("service"); {
		case redirectTo != "":
			http.Redirect(w, r, redirectTo+r.URL.RequestURI(), http.StatusMovedPermanently)
		case reflect && svc != "":
			pkt := func(line string) string { return fmt.Sprintf("%04x%s", len(line)+4, line) }
			w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
			_, _ = io.WriteString(w, pkt("# service="+svc+"\n")+"0000"+pkt("ERR rejected: "+got+"\n"))
		case got != wantAuth:
			http.Error(w, "denied", http.StatusUnauthorized)
		default:
			cgiHandler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// runGitCmd runs the real git with an isolated environment and returns its trimmed output.
func runGitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// invocation is one git process the connector started, as it was handed to git, plus what its private
// home directory held when it ended (the connector deletes it afterwards).
type invocation struct {
	argv []string
	env  []string
	dir  string
	home string
}

// gitRecorder wraps the real git runners to record every invocation.
type gitRecorder struct {
	mu   sync.Mutex
	invs []invocation
}

func (g *gitRecorder) record(argv, env []string, dir string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.invs = append(g.invs, invocation{argv: slices.Clone(argv), env: slices.Clone(env), dir: dir})
	return len(g.invs) - 1
}

func (g *gitRecorder) snapshot(i int, dir string) {
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if data, rerr := os.ReadFile(path); rerr == nil {
				b.Write(data)
			}
		}
		return nil
	})
	g.mu.Lock()
	g.invs[i].home = b.String()
	g.mu.Unlock()
}

func (g *gitRecorder) all() []invocation {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.invs)
}

func (g *gitRecorder) runners() (func(context.Context, []string, []string, string) ([]byte, error), func(context.Context, []string, []string, string, io.Writer) ([]byte, error)) {
	run, runOut := gitsync.RealGitRunnersForTest()
	return func(ctx context.Context, argv, env []string, dir string) ([]byte, error) {
			i := g.record(argv, env, dir)
			defer g.snapshot(i, dir)
			return run(ctx, argv, env, dir)
		}, func(ctx context.Context, argv, env []string, dir string, stdout io.Writer) ([]byte, error) {
			i := g.record(argv, env, dir)
			defer g.snapshot(i, dir)
			return runOut(ctx, argv, env, dir, stdout)
		}
}

// canaryRun is everything one scenario produced.
type canaryRun struct {
	base                  string // the one directory every file the run wrote lives under
	cfgPath               string
	origin, replica       *canaryServer
	originURL, replicaURL string // the canonical URLs the sync dials
	originCanary          string
	pushCanary            string
	originNeedles         []string
	pushNeedles           []string

	logs     *lockedBuffer
	git      *gitRecorder
	sender   *stubSender
	status   proto.GitSyncRepoStatus
	cliOut   map[string]string
	cliError map[string]string
}

type canaryScenario struct {
	name string
	// auth is the origin provider's authentication mode: "bearer" or "basic".
	auth string
	// dotted gives the credentials a '.' (see newCanary).
	dotted bool
	// tweak adjusts the servers or tokens before the sync.
	tweak func(r *canaryRun)
	// wantState and wantType are the expected outcome of the sync.
	wantState, wantType string
}

func TestCanaryNeverLeaks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	scenarios := []canaryScenario{
		{name: "success with bearer", auth: "bearer", wantState: gitsync.StateOK},
		{name: "success with basic", auth: "basic", wantState: gitsync.StateOK},
		{
			name: "origin rejects the token", auth: "bearer", wantState: gitsync.StateError, wantType: gitsync.ErrOriginAccessDenied,
			tweak: func(r *canaryRun) {
				r.origin.update(func(s *canaryServer) { s.wantAuth = "Bearer some-other-token-0123456789" })
			},
		},
		{
			name: "replica rejects the push token", auth: "bearer", wantState: gitsync.StateError, wantType: gitsync.ErrCredentialRejected,
			tweak: func(r *canaryRun) {
				r.replica.update(func(s *canaryServer) { s.wantAuth = "Bearer some-other-token-0123456789" })
			},
		},
		{
			// The credential a server was sent comes back in the text of its own error. The error type is
			// not asserted: the reflected text is random, and a run of it can happen to spell a keyword the
			// classifier reads ("tls", "ssl"), which is a different (harmless) type of failure.
			name: "origin reflects a bearer token", auth: "bearer", wantState: gitsync.StateError,
			tweak: func(r *canaryRun) { r.origin.update(func(s *canaryServer) { s.reflect = true }) },
		},
		{
			name: "origin reflects a dotted bearer token", auth: "bearer", dotted: true, wantState: gitsync.StateError,
			tweak: func(r *canaryRun) { r.origin.update(func(s *canaryServer) { s.reflect = true }) },
		},
		{
			name: "origin reflects a basic credential", auth: "basic", wantState: gitsync.StateError,
			tweak: func(r *canaryRun) { r.origin.update(func(s *canaryServer) { s.reflect = true }) },
		},
		{
			name: "replica reflects the push token", auth: "bearer", wantState: gitsync.StateError,
			tweak: func(r *canaryRun) { r.replica.update(func(s *canaryServer) { s.reflect = true }) },
		},
		{
			// The origin tries to send the connector to the replica, hoping the origin credential follows.
			name: "origin redirects to the replica", auth: "bearer", wantState: gitsync.StateError,
			tweak: func(r *canaryRun) { r.origin.update(func(s *canaryServer) { s.redirectTo = r.replica.URL }) },
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) { runCanaryScenario(t, sc) })
	}
}

func runCanaryScenario(t *testing.T, sc canaryScenario) {
	r := &canaryRun{base: t.TempDir(), logs: captureLogs(t), git: &gitRecorder{}, sender: &stubSender{}}
	r.originCanary, r.pushCanary = newCanary(t, sc.dotted), newCanary(t, sc.dotted)
	user := ""
	if sc.auth == "basic" {
		user = canaryUser
	}
	r.originNeedles = canaryNeedles(r.originCanary, user)
	r.pushNeedles = canaryNeedles(r.pushCanary, "")

	// A hostile parent environment: none of this may reach git (see gitBaseEnv).
	tmpRoot := filepath.Join(r.base, "tmp")
	t.Setenv("GIT_SYNC_TMP_DIR", tmpRoot)
	t.Setenv("GIT_TRACE", "1")
	t.Setenv("GIT_CURL_VERBOSE", "1")
	t.Setenv("GIT_TRACE_CURL", "1")
	t.Setenv("GIT_TRACE2_EVENT", filepath.Join(r.base, "trace2.json"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'http.extraHeader=X-Hostile: 1'")

	// The servers and their repositories.
	work := filepath.Join(r.base, "work")
	originDir, replicaDir := filepath.Join(r.base, "o", "origin.git"), filepath.Join(r.base, "r", "replica.git")
	for _, d := range []string{originDir, replicaDir} {
		runGitCmd(t, "", "init", "--bare", "-b", "main", d)
	}
	runGitCmd(t, "", "init", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, work, "add", "file.txt")
	runGitCmd(t, work, "commit", "-m", "one")
	runGitCmd(t, work, "push", originDir, "main")

	originAuth := "Bearer " + r.originCanary
	if sc.auth == "basic" {
		originAuth = "Basic " + b64(canaryUser+":"+r.originCanary)
	}
	r.origin = newCanaryServer(t, filepath.Join(r.base, "o"), originAuth)
	r.replica = newCanaryServer(t, filepath.Join(r.base, "r"), "Bearer "+r.pushCanary)
	// git must trust the test servers' certificate.
	caFile := filepath.Join(r.base, "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.origin.Certificate().Raw})
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", caFile)
	if _, err := x509.ParseCertificate(r.origin.Certificate().Raw); err != nil {
		t.Fatal(err)
	}

	// The operator's provider config, the only file allowed to hold the origin token.
	r.cfgPath = filepath.Join(r.base, "cfg", "git-sync.json")
	entry := map[string]string{"base_url": r.origin.URL, "auth": sc.auth, "token": r.originCanary}
	if sc.auth == "basic" {
		entry["username"] = canaryUser
	}
	writeFile(t, r.cfgPath, mustJSON(t, map[string]any{"origin": entry}), 0o600)
	r.originURL = r.origin.URL + "/origin.git"
	r.replicaURL = r.replica.URL + "/replica.git"
	if sc.tweak != nil {
		sc.tweak(r)
	}

	// The connector.
	local, err := gitsync.New(r.cfgPath)
	if err != nil {
		t.Fatalf("load provider config: %v", err)
	}
	run, runOut := r.git.runners()
	mirrors := filepath.Join(r.base, "mirrors")
	replicaHost := strings.TrimPrefix(r.replica.URL, "https://")
	cfg := &config.Config{GatewayURL: "wss://gw.example.test", ConnectorID: "gitsync-canary", ProtocolVersion: "v1"}
	c := gitsync.NewClient(cfg, auth.NewKeyStore(filepath.Join(r.base, "key")), local, gitsync.NewGitProber(), time.Now, "boot-canary",
		gitsync.WithReplicaHost(replicaHost), gitsync.WithMirrorsRoot(mirrors), gitsync.WithStatusInterval(10*time.Millisecond),
		gitsync.WithGitRunner(run), gitsync.WithGitOutputRunnerForTest(runOut), gitsync.WithRunSaveIntervalForTest(time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.OnConnected(ctx, r.sender)
	t.Cleanup(func() {
		cancel()
		wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer wcancel()
		c.Wait(wctx)
	})

	// The platform's configuration, naming the repository and carrying the push token.
	frame := configJSONRaw("1", repoJSON("repo-a", r.originURL, "", r.replicaURL, r.pushCanary))
	c.HandleMessage(ctx, r.sender, "git_sync_config", frame)
	deadline := time.Now().Add(60 * time.Second)
	for {
		c.OnHeartbeat(ctx, r.sender)
		st := repoStatusNow(t, r.sender, "repo-a")
		if st.State != gitsync.StateSyncing && st.State != gitsync.StatePending && c.IdleForTest() {
			r.status = st
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sync did not finish; last status %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Wait for the finished attempt to reach the run-state file.
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(mirrors, "run-state.json")); return err == nil })
	c.OnHeartbeat(ctx, r.sender)
	r.status = repoStatusNow(t, r.sender, "repo-a")

	if r.status.State != sc.wantState || (sc.wantType != "" && r.status.ErrorType != sc.wantType) {
		t.Fatalf("outcome = %s/%s (%q), want %s/%s", r.status.State, r.status.ErrorType, r.status.ErrorDetail, sc.wantState, sc.wantType)
	}

	// What the operator CLI prints, with the connector's environment.
	t.Setenv("GIT_SYNC_CONFIG_FILE", r.cfgPath)
	t.Setenv("GIT_SYNC_MIRRORS_DIR", mirrors)
	t.Setenv(gitsync.ReplicaHostEnv, replicaHost)
	r.cliOut, r.cliError = map[string]string{}, map[string]string{}
	for _, args := range [][]string{{"status"}, {"status", "--json"}, {"config", "list"}, {"config", "check"}} {
		var out bytes.Buffer
		err := cli.Run(args, strings.NewReader(""), &out)
		key := strings.Join(args, " ")
		r.cliOut[key] = out.String()
		if err != nil {
			r.cliError[key] = err.Error()
		}
	}

	t.Logf("outcome %s/%s detail=%q", r.status.State, r.status.ErrorType, r.status.ErrorDetail)
	r.assertNothingLeaked(t, sc)
}

func (r *canaryRun) assertNothingLeaked(t *testing.T, sc canaryScenario) {
	t.Helper()
	both := append(slices.Clone(r.originNeedles), r.pushNeedles...)

	// Every message sent to the platform, as it goes on the wire and as Go would print it.
	msgs := r.sender.messages()
	for i, m := range msgs {
		wire, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		assertNoLeak(t, fmt.Sprintf("message %d on the wire", i), string(wire), both)
		assertNoLeak(t, fmt.Sprintf("message %d printed with %%+v", i), fmt.Sprintf("%+v %#v", m, m), both)
	}
	if len(msgs) == 0 {
		t.Error("no status was sent; the test proved nothing")
	}
	assertNoLeak(t, "status error_detail", r.status.ErrorDetail, both)

	// Logs.
	assertNoLeak(t, "the log", r.logs.String(), both)

	// The CLI, in every output and error.
	for cmd, out := range r.cliOut {
		assertNoLeak(t, "`"+cmd+"` output", out, both)
	}
	for cmd, msg := range r.cliError {
		assertNoLeak(t, "`"+cmd+"` error", msg, both)
	}

	// Everything on disk: the only file that may hold a credential is the provider config, and only the
	// origin token.
	_ = filepath.WalkDir(r.base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(r.base, path)
		needles := both
		if path == r.cfgPath {
			needles = r.pushNeedles
		}
		assertNoLeak(t, "file "+rel, string(data), needles)
		return nil
	})
	if cfg, err := os.ReadFile(r.cfgPath); err != nil || leak(string(cfg), r.originNeedles) == "" {
		t.Error("the provider config no longer holds the origin token: the scanner is not looking at what it thinks")
	}

	// Git: argv, working directory, home directory, and environment.
	r.assertGitInvocations(t, sc)

	// What each server actually received. The origin token went to the origin only, the push token to
	// the replica only, and a redirect carried neither anywhere.
	for _, h := range r.origin.headers() {
		assertNoLeak(t, "a request to the origin", h, r.pushNeedles)
	}
	for _, h := range r.replica.headers() {
		assertNoLeak(t, "a request to the replica", h, r.originNeedles)
	}
	if sc.name == "success with bearer" || sc.name == "success with basic" {
		// Nothing writes wantAuth once the sync has run, so reading it needs no lock.
		if !slices.Contains(r.origin.headers(), r.origin.wantAuth) || !slices.Contains(r.replica.headers(), r.replica.wantAuth) {
			t.Error("a server never received its credential; the test proved nothing")
		}
	}
	if sc.name == "origin redirects to the replica" && len(r.replica.headers()) != 0 {
		t.Errorf("the replica received %d request(s) after the origin redirected to it", len(r.replica.headers()))
	}
}

// networkSubcommands are the git commands that talk to a server; every other command gets no credential.
var networkSubcommands = []string{"fetch", "push", "ls-remote"}

func subcommand(argv []string) string {
	for _, a := range argv[1:] {
		if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "/") {
			return a
		}
	}
	return ""
}

// assertGitInvocations checks what the connector handed to git. A credential may appear only in the
// value of the one http.<url>/.extraHeader entry, whose url is exactly the server the invocation talks
// to, and only the matching credential: the origin token for the origin, the push token for the replica.
func (r *canaryRun) assertGitInvocations(t *testing.T, sc canaryScenario) {
	t.Helper()
	invs := r.git.all()
	if len(invs) == 0 {
		t.Fatal("git never ran")
	}
	seenOrigin, seenReplica := false, false
	for _, inv := range invs {
		sub := subcommand(inv.argv)
		what := "git " + sub
		both := append(slices.Clone(r.originNeedles), r.pushNeedles...)
		assertNoLeak(t, what+" argv", strings.Join(inv.argv, "\x00"), both)
		assertNoLeak(t, what+" working directory", inv.dir, both)
		assertNoLeak(t, what+" home directory contents", inv.home, both)
		for _, kv := range inv.env {
			k, _, _ := strings.Cut(kv, "=")
			if strings.HasPrefix(k, "GIT_TRACE") || k == "GIT_CURL_VERBOSE" || k == "GIT_CONFIG_PARAMETERS" {
				t.Errorf("%s inherited %s from the connector's environment", what, k)
			}
		}

		// Pair up the injected config entries.
		cfgKeys := map[string]string{}
		for _, kv := range inv.env {
			if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "GIT_CONFIG_KEY_") {
				cfgKeys[strings.TrimPrefix(k, "GIT_CONFIG_KEY_")] = v
			}
		}
		for _, kv := range inv.env {
			k, v, _ := strings.Cut(kv, "=")
			if !strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
				assertNoLeak(t, what+" environment variable "+k, kv, both)
				continue
			}
			key := cfgKeys[strings.TrimPrefix(k, "GIT_CONFIG_VALUE_")]
			needles := both
			switch {
			case !slices.Contains(networkSubcommands, sub):
			case key == "http."+r.originURL+"/.extraHeader" && argvHas(inv.argv, r.originURL):
				needles = r.pushNeedles // the origin token belongs here
				seenOrigin = seenOrigin || leak(v, r.originNeedles) != ""
			case key == "http."+r.replicaURL+"/.extraHeader" && argvHas(inv.argv, r.replicaURL):
				needles = r.originNeedles // the push token belongs here
				seenReplica = seenReplica || leak(v, r.pushNeedles) != ""
			}
			assertNoLeak(t, what+" config value for "+safeKey(key), v, needles)
		}
	}
	if sc.wantState == gitsync.StateOK && (!seenOrigin || !seenReplica) {
		t.Errorf("no git invocation carried the credential in its scoped extraHeader (origin %v, replica %v); the test proved nothing", seenOrigin, seenReplica)
	}
}

func argvHas(argv []string, s string) bool { return slices.Contains(argv, s) }

// safeKey names a config key for a failure message without repeating the URL.
func safeKey(k string) string {
	if u, err := url.Parse(strings.TrimPrefix(k, "http.")); err == nil && u.Host != "" {
		return "http.<url>"
	}
	return k
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}
