package gitsync_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

const (
	tokenOne = "origin-token-one-aaaaaaaa"
	tokenTwo = "origin-token-two-bbbbbbbb"
)

// providersJSON is a provider file with a bearer provider for each (name, host, token).
func providersJSON(entries ...[3]string) string {
	var parts []string
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf(`%q:{"base_url":"https://%s/","auth":"bearer","token":%q}`, e[0], e[1], e[2]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// storeOf loads a Store from provider-file JSON, as a reload would.
func storeOf(t *testing.T, js string) *gitsync.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git-sync.json")
	if err := os.WriteFile(path, []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := gitsync.New(path)
	if err != nil {
		t.Fatalf("load providers: %v", err)
	}
	return s
}

// assertNoSecrets fails if any of secrets appears in text, raw or as the base64 a basic-auth header
// would carry.
func assertNoSecrets(t *testing.T, what, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		for _, form := range []string{s, base64.StdEncoding.EncodeToString([]byte("user:" + s))} {
			if strings.Contains(text, form) {
				t.Errorf("%s contains a credential", what)
			}
		}
	}
}

// stubGit is a real executable standing in for git, so the real runner (process groups, signals) is
// what the tests exercise. It appends "<subcommand> <pid> <tokens present in its environment>" to a
// calls file, creates the mirror on init, and blocks in fetch while the "block" file exists.
type stubGit struct {
	dir string
}

func newStubGit(t *testing.T, tokens ...string) *stubGit {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
D=%q
sub=""
last=""
for a in "$@"; do
  last="$a"
  if [ -z "$sub" ]; then
    case "$a" in init|fetch|push|show-ref|ls-remote|for-each-ref) sub="$a";; esac
  fi
done
tok=""
for t in %s; do
  if env | grep -qF -- "$t"; then tok="$tok $t"; fi
done
echo "$sub $$ ${tok:-none}" >> "$D/calls"
case "$sub" in
init) mkdir -p "$last"; exit 0;;
fetch)
  if [ -e "$D/block" ]; then
    echo $$ > "$D/fetch.pid"
    trap 'echo terminated >> "$D/calls"; exit 143' TERM
    sleep 300 &
    echo $! > "$D/helper.pid"
    wait
  fi
  exit 0;;
esac
exit 0
`, dir, strings.Join(tokens, " "))
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := &stubGit{dir: dir}
	s.setBlocking(true)
	return s
}

func (s *stubGit) setBlocking(on bool) {
	p := filepath.Join(s.dir, "block")
	if on {
		_ = os.WriteFile(p, nil, 0o600)
	} else {
		_ = os.Remove(p)
	}
}

// mark appends a line to the calls file, so a test can order its own steps against the stub's.
func (s *stubGit) mark(label string) {
	f, err := os.OpenFile(filepath.Join(s.dir, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = f.WriteString("MARK " + label + "\n")
		_ = f.Close()
	}
}

// calls returns the recorded lines, each split into fields.
func (s *stubGit) calls() [][]string {
	b, _ := os.ReadFile(filepath.Join(s.dir, "calls"))
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" {
			out = append(out, strings.Fields(line))
		}
	}
	return out
}

// after returns the calls recorded after the given mark.
func (s *stubGit) after(label string) [][]string {
	all := s.calls()
	for i, c := range all {
		if len(c) == 2 && c[0] == "MARK" && c[1] == label {
			return all[i+1:]
		}
	}
	return nil
}

func countSub(calls [][]string, sub string) int {
	n := 0
	for _, c := range calls {
		if c[0] == sub {
			n++
		}
	}
	return n
}

func (s *stubGit) pid(t *testing.T, name string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join(s.dir, name)); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return p
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", name)
	return 0
}

func gone(pid int) bool { return syscall.Kill(pid, 0) == syscall.ESRCH }

func waitProcessGone(t *testing.T, what string, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if gone(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s (pid %d) is still running", what, pid)
}

// newReloadClient builds a client that runs the real git runner against whatever git is first on PATH.
func newReloadClient(t *testing.T, providers string, p gitsync.OriginProber, opts ...gitsync.Option) *gitsync.Client {
	t.Helper()
	c, _ := newFileClient(t, providers, p, opts...)
	return c
}

// newFileClient is newReloadClient that also returns the path of the provider file the client was
// started from, which the live reload watches.
func newFileClient(t *testing.T, providers string, p gitsync.OriginProber, opts ...gitsync.Option) (*gitsync.Client, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git-sync.json")
	if providers != "" {
		if err := os.WriteFile(path, []byte(providers), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	local, err := gitsync.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defaults := []gitsync.Option{
		gitsync.WithReplicaHost("replica.example.com"),
		gitsync.WithMirrorsRoot(t.TempDir()),
		gitsync.WithStatusInterval(10 * time.Millisecond),
	}
	cfg := &config.Config{GatewayURL: "wss://gw.example.com", ConnectorID: "gitsync-test", ProtocolVersion: "v1"}
	c := gitsync.NewClient(cfg, auth.NewKeyStore(filepath.Join(t.TempDir(), "key")), local, p, fixedNow, "boot-test", append(defaults, opts...)...)
	t.Cleanup(func() { waitForSyncsToStop(t, c) })
	return c, path
}

func configureRepo(c *gitsync.Client, s *stubSender, id, origin string) {
	c.HandleMessage(context.Background(), s, "git_sync_config", configJSONRaw("1",
		repoJSON(id, origin, "", "https://replica.example.com/r/"+id+".git", testPushToken)))
}

// Removing a provider while a sync that uses it is running kills its git (the whole process group),
// records nothing, never reaches the push, and leaves the repository reporting that its host is not
// configured locally.
func TestRevokingAProviderMidSyncKillsTheGitAndPushesNothing(t *testing.T) {
	logs := captureLogs(t)
	stub := newStubGit(t, tokenOne)
	c := newReloadClient(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}), &fakeProber{result: gitsync.OriginReachable})
	s := &stubSender{}
	configureRepo(c, s, "repo-a", "https://git.example.com/team/a.git")

	fetchPID := stub.pid(t, "fetch.pid")
	helperPID := stub.pid(t, "helper.pid")

	stub.mark("revoke")
	c.SetProvidersForTest(storeOf(t, "{}"))

	waitProcessGone(t, "git fetch", fetchPID)
	waitProcessGone(t, "git's helper", helperPID)
	// Give a wrongly surviving sync the time to push.
	time.Sleep(300 * time.Millisecond)

	if n := countSub(stub.calls(), "push"); n != 0 {
		t.Errorf("git push ran %d times; nothing may be pushed after the provider is revoked", n)
	}
	if n := countSub(stub.after("revoke"), "fetch"); n != 0 {
		t.Errorf("%d git fetches started after the revocation", n)
	}
	waitFor(t, func() bool { return c.AttemptsForTest() == 0 })
	_ = c.OnHeartbeat(context.Background(), s)
	st := repoStatusNow(t, s, "repo-a")
	if st.State != gitsync.StateError || st.ErrorType != gitsync.ErrOriginAccessDenied || st.ErrorDetail != "host not configured locally" {
		t.Errorf("status = %+v, want origin_access_denied / host not configured locally", st)
	}
	if st.LastSuccessAt != nil || st.LastAttemptAt != nil {
		t.Errorf("the cancelled sync was recorded: %+v", st)
	}
	raw, _ := json.Marshal(lastStatus(t, s.messages()))
	assertNoSecrets(t, "logs", logs.String(), tokenOne, testPushToken)
	assertNoSecrets(t, "status", string(raw), tokenOne, testPushToken)
}

// Rotating a credential while a sync is running kills that sync and reruns the repository once, with
// the new credential. After the swap the old credential is never in the environment of any git.
func TestRotatingACredentialMidSyncRerunsOnceWithTheNewOne(t *testing.T) {
	logs := captureLogs(t)
	stub := newStubGit(t, tokenOne, tokenTwo)
	c := newReloadClient(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}), &fakeProber{result: gitsync.OriginReachable})
	s := &stubSender{}
	configureRepo(c, s, "repo-a", "https://git.example.com/team/a.git")
	fetchPID := stub.pid(t, "fetch.pid")

	stub.setBlocking(false) // the rerun must be able to finish
	stub.mark("swap")
	c.SetProvidersForTest(storeOf(t, providersJSON([3]string{"origin", "git.example.com", tokenTwo})))
	waitProcessGone(t, "the sync that used the old credential", fetchPID)

	waitFor(t, func() bool {
		_ = c.OnHeartbeat(context.Background(), s)
		return repoStatusNow(t, s, "repo-a").State == gitsync.StateOK
	})
	after := stub.after("swap")
	if n := countSub(after, "fetch"); n != 1 {
		t.Errorf("%d fetches after the rotation, want exactly the one rerun", n)
	}
	for _, call := range after {
		if len(call) < 3 {
			continue // a marker or the stub's own note
		}
		for _, f := range call[2:] {
			if f == tokenOne {
				t.Errorf("git %s started after the rotation with the old credential", call[0])
			}
		}
	}
	for _, call := range after {
		if call[0] == "fetch" && (len(call) < 3 || call[2] != tokenTwo) {
			t.Errorf("the rerun's fetch = %v, want the new credential", call)
		}
	}
	raw, _ := json.Marshal(lastStatus(t, s.messages()))
	assertNoSecrets(t, "logs", logs.String(), tokenOne, tokenTwo, testPushToken)
	assertNoSecrets(t, "status", string(raw), tokenOne, tokenTwo, testPushToken)
}

// gatedOutput is a git runner whose first ls-remote (the last step of a sync, after the push) holds
// until released and ignores cancellation, as a process that had already passed the point of no
// return would.
type gatedOutput struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedOutput() *gatedOutput {
	return &gatedOutput{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedOutput) run(_ context.Context, argv, _ []string, _ string) ([]byte, error) {
	for _, a := range argv {
		if a == "ls-remote" {
			first := false
			g.once.Do(func() { first = true })
			if first {
				close(g.entered)
				<-g.release
			}
		}
	}
	return nil, nil
}

// The result of a sync that was started under a credential that has since been replaced is dropped,
// even when the sync ran to completion regardless of the cancellation: it is neither reported as a
// success nor persisted. Replacing the credential and then restoring it (A, B, A) does not make the
// result legitimate again. (Syncing is suspended after the sync starts, so no rerun can record a
// success of its own and mask a wrongly kept one.)
func TestResultOfASupersededSyncIsDroppedEvenWhenItCompleted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		swaps []string
	}{
		{"rotated", []string{tokenTwo}},
		{"rotated and restored", []string{tokenTwo, tokenOne}},
		{"removed", []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(gitsync.SetSupersededStopWaitForTest(50 * time.Millisecond))
			g := newGatedOutput()
			c := newReloadClient(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}), &fakeProber{result: gitsync.OriginReachable},
				gitsync.WithGitRunner(g.run), gitsync.WithGitOutputRunnerForTest(func(ctx context.Context, argv, env []string, dir string, _ io.Writer) ([]byte, error) {
					return g.run(ctx, argv, env, dir)
				}))
			s := &stubSender{}
			configureRepo(c, s, "repo-a", "https://git.example.com/team/a.git")
			<-g.entered
			c.SetCriticalPatchForTest(true)
			for _, tok := range tc.swaps {
				js := "{}"
				if tok != "" {
					js = providersJSON([3]string{"origin", "git.example.com", tok})
				}
				c.SetProvidersForTest(storeOf(t, js))
			}
			close(g.release)
			waitFor(t, func() bool { return c.AttemptsForTest() == 0 })
			waitForSyncsToStop(t, c)
			_ = c.OnHeartbeat(context.Background(), s)
			st := repoStatusNow(t, s, "repo-a")
			if st.LastSuccessAt != nil || st.LastAttemptAt != nil || st.State == gitsync.StateOK {
				t.Errorf("status = %+v: the result of the superseded sync was recorded", st)
			}
		})
	}
}

// A provider added later makes a repository that was denied for lack of one syncable at once, with
// the replica material the platform sent earlier: no new platform configuration is needed.
func TestProviderAddedLaterUnblocksADeniedRepositoryLive(t *testing.T) {
	g := &scriptedGit{}
	c := newTestClientOpts(t, "", &fakeProber{result: gitsync.OriginReachable}, fixedNow, gitsync.WithGitRunner(g.run))
	s := &stubSender{}
	configureRepo(c, s, "repo-a", "https://git.example.com/team/a.git")
	st := repoStatusNow(t, s, "repo-a")
	if st.State != gitsync.StateError || st.ErrorType != gitsync.ErrOriginAccessDenied {
		t.Fatalf("status = %+v, want denied while no provider matches", st)
	}
	if n := g.count("fetch"); n != 0 {
		t.Fatalf("%d fetches before any provider exists", n)
	}

	c.SetProvidersForTest(storeOf(t, providersJSON([3]string{"origin", "git.example.com", tokenOne})))

	waitFor(t, func() bool {
		_ = c.OnHeartbeat(context.Background(), s)
		return repoStatusNow(t, s, "repo-a").State == gitsync.StateOK
	})
	if n := g.count("fetch"); n != 1 {
		t.Errorf("%d fetches, want the one the new provider triggered", n)
	}
	_, pushEnv := g.call(t, "push")
	if !contains(pushEnv, testPushToken) {
		t.Error("the push did not use the push token the platform sent before the provider existed")
	}
	if p, bound, _ := c.BindingForTest("repo-a"); !bound || p != "origin" {
		t.Errorf("binding = %q, %v", p, bound)
	}
}

// Changing one provider leaves a sync that uses another alone: it is not cancelled, not rerun.
func TestUnrelatedProviderChangeCancelsAndRerunsNothing(t *testing.T) {
	g := &scriptedGit{gate: make(chan struct{})}
	c := newTestClientOpts(t, providersJSON([3]string{"a", "git.example.com", tokenOne}, [3]string{"b", "other.example.com", tokenTwo}),
		&fakeProber{result: gitsync.OriginReachable}, fixedNow, gitsync.WithGitRunner(g.run))
	t.Cleanup(g.release)
	s := &stubSender{}
	configureRepo(c, s, "repo-a", "https://git.example.com/team/a.git")
	waitFor(t, func() bool { return g.count("fetch") == 1 })

	c.SetProvidersForTest(storeOf(t, providersJSON(
		[3]string{"a", "git.example.com", tokenOne}, [3]string{"b", "other.example.com", "origin-token-three-cccccc"})))
	c.SetProvidersForTest(storeOf(t, providersJSON(
		[3]string{"a", "git.example.com", tokenOne}, [3]string{"b", "other.example.com", "origin-token-three-cccccc"}, [3]string{"c", "third.example.com", tokenTwo})))
	time.Sleep(100 * time.Millisecond)
	if n := c.AttemptsForTest(); n != 1 {
		t.Fatalf("%d attempts registered, want the untouched one still running", n)
	}
	g.release()
	waitFor(t, func() bool {
		_ = c.OnHeartbeat(context.Background(), s)
		return repoStatusNow(t, s, "repo-a").State == gitsync.StateOK
	})
	time.Sleep(100 * time.Millisecond)
	if n := g.count("fetch"); n != 1 {
		t.Errorf("%d fetches, want 1: an unrelated change must not rerun the repository", n)
	}
}

// Reloading an unchanged configuration does nothing: no cancellation, no rerun, and the cached
// origin check is kept.
func TestReloadOfAnUnchangedConfigurationChangesNothing(t *testing.T) {
	p := &fakeProber{result: gitsync.OriginReachable}
	c := newTestClientP(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}), p)
	s := &stubSender{}
	c.OnConnected(context.Background(), s)
	waitFor(t, func() bool { return latestOriginCheck(s.messages()) != nil })
	waitFor(t, func() bool { return !c.ProbeInFlightForTest() })

	c.SetProvidersForTest(storeOf(t, providersJSON([3]string{"origin", "git.example.com", tokenOne})))
	time.Sleep(50 * time.Millisecond)
	if err := c.OnHeartbeat(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if lastStatus(t, s.messages()).OriginCheck == nil {
		t.Error("the origin check was discarded by a reload that changed nothing")
	}
	if n := p.calls.Load(); n != 1 {
		t.Errorf("%d probes, want 1", n)
	}
}

// versionedProber answers per credential: a probe made with tokenOne holds until released and
// ignores cancellation (as one that had already passed its last check would), then reports
// access_denied; any other answers at once.
type versionedProber struct {
	release chan struct{}
	mu      sync.Mutex
	seen    []string
}

func (p *versionedProber) Probe(_ context.Context, e gitsync.LocalEntry) (gitsync.OriginResult, error) {
	p.mu.Lock()
	p.seen = append(p.seen, e.Token.Reveal())
	p.mu.Unlock()
	if e.Token.Reveal() == tokenOne {
		<-p.release
		return gitsync.OriginAccessDenied, nil
	}
	return gitsync.OriginReachable, nil
}

func (p *versionedProber) tokens() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

// A probe that was made from the previous providers never commits its result, even if it finishes
// after the swap, and the new providers are probed at once rather than when the cadence allows.
func TestProbeOfReplacedProvidersIsDiscardedAndRepeated(t *testing.T) {
	p := &versionedProber{release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-p.release:
		default:
			close(p.release)
		}
	})
	c := newTestClientP(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}), p)
	s := &stubSender{}
	c.OnConnected(context.Background(), s)
	waitFor(t, func() bool { return len(p.tokens()) == 1 })

	c.SetProvidersForTest(storeOf(t, providersJSON([3]string{"origin", "git.example.com", tokenTwo})))
	close(p.release) // the old probe now finishes, late, with a verdict about the old credential

	waitFor(t, func() bool {
		oc := latestOriginCheck(s.messages())
		return oc != nil && oc.Result == string(gitsync.OriginReachable)
	})
	for _, m := range s.messages() {
		if st, ok := m.(proto.GitSyncStatusMsg); ok && st.OriginCheck != nil && st.OriginCheck.Result == string(gitsync.OriginAccessDenied) {
			t.Error("the superseded probe's result was reported")
		}
	}
	if got := p.tokens(); len(got) != 2 || got[1] != tokenTwo {
		t.Errorf("probed credentials = %v, want the old one then the new one", got)
	}
}

// A credential change that lands after a sync has read its credential but before it registers is
// not missed: the lock the claim holds spans both, so the change either sees the attempt or is
// seen by the claim.
func TestCredentialChangeCannotSlipBetweenClaimAndRegistration(t *testing.T) {
	g := &scriptedGit{gate: make(chan struct{})}
	c := newTestClientOpts(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}),
		&fakeProber{result: gitsync.OriginReachable}, fixedNow, gitsync.WithGitRunner(g.run))
	t.Cleanup(g.release)

	entered, swapped := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.SetAfterClaimReadForTest(func() {
		once.Do(func() {
			close(entered)
			// Hold the claim at the dangerous point. If the replacement can complete now, it has
			// found no attempt to cancel; give it the chance to.
			select {
			case <-swapped:
			case <-time.After(150 * time.Millisecond):
			}
		})
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-entered
		c.SetProvidersForTest(storeOf(t, "{}"))
		close(swapped)
	}()
	configureRepo(c, &stubSender{}, "repo-a", "https://git.example.com/team/a.git")
	<-done

	// The sync that read the old credential must have been cancelled, so it ends without the gate.
	waitFor(t, func() bool { return c.AttemptsForTest() == 0 })
	if n := g.count("push"); n != 0 {
		t.Errorf("%d pushes", n)
	}
}

// Whatever order configurations and provider reloads interleave in, a repository ends up bound
// exactly when a provider serves its host: no stale binding survives either order.
func TestConcurrentConfigAndReloadNeverLeaveAStaleBinding(t *testing.T) {
	c := newTestClient(t, "")
	c.SetCriticalPatchForTest(true) // bindings only; no syncs
	with := storeOf(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}))
	without := storeOf(t, "{}")
	var repos []string
	for i := 0; i < 8; i++ {
		repos = append(repos, repoJSON(fmt.Sprintf("repo-%d", i), fmt.Sprintf("https://git.example.com/t/%d.git", i), "",
			fmt.Sprintf("https://replica.example.com/r/%d.git", i), testPushToken))
	}
	cfg := configJSONRaw("1", repos...)

	for round := 0; round < 40; round++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				c.HandleMessage(context.Background(), &stubSender{}, "git_sync_config", cfg)
			}
		}()
		final := with
		if round%2 == 0 {
			final = without
		}
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				c.SetProvidersForTest(without)
				c.SetProvidersForTest(with)
			}
			c.SetProvidersForTest(final)
		}()
		wg.Wait()
		wantBound := final == with
		for i := 0; i < 8; i++ {
			if _, bound, found := c.BindingForTest(fmt.Sprintf("repo-%d", i)); found && bound != wantBound {
				t.Fatalf("round %d: repo-%d bound=%v, want %v", round, i, bound, wantBound)
			}
		}
	}
}

// A provider reload that lands after a configuration has been validated but before it is installed
// must not leave the repositories bound to the provider that was just removed.
func TestReloadBetweenValidatingAndInstallingAConfigLeavesNoStaleBinding(t *testing.T) {
	g := &scriptedGit{}
	c := newTestClientOpts(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}),
		&fakeProber{result: gitsync.OriginReachable}, fixedNow, gitsync.WithGitRunner(g.run))
	c.SetBeforeConfigInstallForTest(func() { c.SetProvidersForTest(storeOf(t, "{}")) })
	s := &stubSender{}
	configureRepo(c, s, "repo-a", "https://git.example.com/team/a.git")

	if _, bound, found := c.BindingForTest("repo-a"); !found || bound {
		t.Errorf("repo-a bound=%v found=%v, want present and unbound: its provider was removed first", bound, found)
	}
	time.Sleep(50 * time.Millisecond)
	if n := g.count("fetch"); n != 0 {
		t.Errorf("%d fetches for a repository whose provider no longer exists", n)
	}
}

// What the last attempt reported was said about a credential that is no longer in force: after a
// rotation the repository starts over (pending) instead of showing the old failure, even when nothing
// can sync it yet.
func TestRotationResetsTheOutcomeOfTheOldCredential(t *testing.T) {
	g := &scriptedGit{fail: map[string][]byte{"fetch": []byte("fatal: unable to access 'https://git.example.com/x/': The requested URL returned error: 403")}}
	c := newTestClientOpts(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}),
		&fakeProber{result: gitsync.OriginReachable}, fixedNow, gitsync.WithGitRunner(g.run))
	s := &stubSender{}
	configureRepo(c, s, "repo-a", "https://git.example.com/team/a.git")
	waitFor(t, func() bool {
		_ = c.OnHeartbeat(context.Background(), s)
		return repoStatusNow(t, s, "repo-a").ErrorType == gitsync.ErrOriginAccessDenied
	})
	waitForSyncsToStop(t, c)

	c.SetCriticalPatchForTest(true) // the rotation's own sync is held back
	c.SetProvidersForTest(storeOf(t, providersJSON([3]string{"origin", "git.example.com", tokenTwo})))
	_ = c.OnHeartbeat(context.Background(), s)
	if st := repoStatusNow(t, s, "repo-a"); st.State != gitsync.StatePending || st.ErrorType != "" {
		t.Errorf("status = %+v, want pending: the failure belongs to the credential that was replaced", st)
	}
}

// A change that leaves every credential as it was (here, the path of a base URL) cancels and reruns
// nothing, but the origin is checked again because the check dials that URL.
func TestBaseURLPathChangeReprobesButCancelsNothing(t *testing.T) {
	g := &scriptedGit{gate: make(chan struct{})}
	p := &fakeProber{result: gitsync.OriginReachable}
	c := newTestClientOpts(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}), p, fixedNow, gitsync.WithGitRunner(g.run))
	t.Cleanup(g.release)
	s := &stubSender{}
	c.OnConnected(context.Background(), s)
	waitFor(t, func() bool { return latestOriginCheck(s.messages()) != nil })
	waitFor(t, func() bool { return !c.ProbeInFlightForTest() })
	configureRepo(c, s, "repo-a", "https://git.example.com/team/a.git")
	waitFor(t, func() bool { return g.count("fetch") == 1 })

	c.SetProvidersForTest(storeOf(t, `{"origin":{"base_url":"https://git.example.com/team/","auth":"bearer","token":"`+tokenOne+`"}}`))

	waitFor(t, func() bool { return p.calls.Load() == 2 })
	if n := c.AttemptsForTest(); n != 1 {
		t.Errorf("%d attempts, want the running sync left alone", n)
	}
	if n := g.count("fetch"); n != 1 {
		t.Errorf("%d fetches, want 1", n)
	}
}
