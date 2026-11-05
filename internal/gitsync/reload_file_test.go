package gitsync_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

// replaceFile swaps the file's contents the way an editor or a secret mount does: a new file renamed
// over the old one.
func replaceFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// fileReload starts the live reload of c's provider file with short intervals.
func fileReload(t *testing.T, c *gitsync.Client, poll, settle time.Duration) {
	t.Helper()
	c.SetReloadTimingForTest(poll, settle)
	ctx, cancel := context.WithCancel(context.Background())
	stop := c.StartReloadForTest(ctx)
	t.Cleanup(func() { cancel(); stop() })
}

func envHas(env []string, tok string) bool {
	for _, e := range env {
		if strings.Contains(e, tok) {
			return true
		}
	}
	return false
}

// Rotating the credential in the file is noticed, kills the sync that is using the old one, and
// reruns the repository with the new one.
func TestEditingTheProviderFileRotatesTheCredentialLive(t *testing.T) {
	logs := captureLogs(t)
	g := &scriptedGit{gate: make(chan struct{})}
	c, path := newFileClient(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}),
		&fakeProber{result: gitsync.OriginReachable}, gitsync.WithGitRunner(g.run))
	t.Cleanup(g.release)
	configureRepo(c, &stubSender{}, "repo-a", "https://git.example.com/team/a.git")
	waitFor(t, func() bool { return g.count("fetch") == 1 })
	fileReload(t, c, 10*time.Millisecond, 10*time.Millisecond)

	replaceFile(t, path, providersJSON([3]string{"origin", "git.example.com", tokenTwo}), 0o600)

	waitFor(t, func() bool { return g.count("fetch") == 2 })
	var fetchEnvs [][]string
	g.mu.Lock()
	for i, a := range g.argvs {
		if subOf(a) == "fetch" {
			fetchEnvs = append(fetchEnvs, g.envs[i])
		}
	}
	g.mu.Unlock()
	first, second := fetchEnvs[0], fetchEnvs[1]
	if !envHas(first, tokenOne) || envHas(first, tokenTwo) {
		t.Error("the first fetch did not use the old credential")
	}
	if !envHas(second, tokenTwo) || envHas(second, tokenOne) {
		t.Error("the rerun's fetch did not use only the new credential")
	}
	assertNoSecrets(t, "logs", logs.String(), tokenOne, tokenTwo, testPushToken)
}

// Deleting the provider file revokes everything once the settle has passed.
func TestDeletingTheProviderFileFailsClosed(t *testing.T) {
	g := &scriptedGit{gate: make(chan struct{})}
	c, path := newFileClient(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}),
		&fakeProber{result: gitsync.OriginReachable}, gitsync.WithGitRunner(g.run))
	t.Cleanup(g.release)
	configureRepo(c, &stubSender{}, "repo-a", "https://git.example.com/team/a.git")
	waitFor(t, func() bool { return g.count("fetch") == 1 })
	fileReload(t, c, 10*time.Millisecond, 10*time.Millisecond)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, bound, _ := c.BindingForTest("repo-a")
		return !bound && c.AttemptsForTest() == 0
	})

	// The file coming back restores service.
	replaceFile(t, path, providersJSON([3]string{"origin", "git.example.com", tokenOne}), 0o600)
	waitFor(t, func() bool { _, bound, _ := c.BindingForTest("repo-a"); return bound })
}

// A file that cannot be used leaves the last good configuration in force and is reported once, by
// category only: nothing in the file, including a credential-looking key, reaches the log.
func TestInvalidProviderFileKeepsTheLastGoodAndLeaksNothing(t *testing.T) {
	logs := captureLogs(t)
	g := &scriptedGit{gate: make(chan struct{})}
	c, path := newFileClient(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}),
		&fakeProber{result: gitsync.OriginReachable}, gitsync.WithGitRunner(g.run))
	t.Cleanup(g.release)
	configureRepo(c, &stubSender{}, "repo-a", "https://git.example.com/team/a.git")
	waitFor(t, func() bool { return g.count("fetch") == 1 })
	fileReload(t, c, 10*time.Millisecond, 10*time.Millisecond)

	canary := "pasted-token-canary-0123456789"
	replaceFile(t, path, `{"origin":{"base_url":"https://git.example.com/","`+canary+`":1}}`, 0o600)
	waitFor(t, func() bool { return strings.Contains(logs.String(), "cannot be used") })
	time.Sleep(300 * time.Millisecond) // many more polls of the same content

	if _, bound, _ := c.BindingForTest("repo-a"); !bound {
		t.Error("an unusable file revoked the configuration in force")
	}
	if n := c.AttemptsForTest(); n != 1 {
		t.Errorf("%d attempts, want the running sync untouched", n)
	}
	out := logs.String()
	if n := strings.Count(out, "cannot be used"); n != 1 {
		t.Errorf("reported %d times, want once:\n%s", n, out)
	}
	assertNoSecrets(t, "logs", out, canary, tokenOne, testPushToken)
}

// SIGHUP applies a change without waiting for the poll or the settle.
func TestReloadRequestAppliesWithoutWaiting(t *testing.T) {
	g := &scriptedGit{gate: make(chan struct{})}
	c, path := newFileClient(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}),
		&fakeProber{result: gitsync.OriginReachable}, gitsync.WithGitRunner(g.run))
	t.Cleanup(g.release)
	configureRepo(c, &stubSender{}, "repo-a", "https://git.example.com/team/a.git")
	fileReload(t, c, time.Hour, time.Hour)

	replaceFile(t, path, "{}", 0o600)
	time.Sleep(50 * time.Millisecond)
	if _, bound, _ := c.BindingForTest("repo-a"); !bound {
		t.Fatal("applied without being asked")
	}
	c.Reload()
	waitFor(t, func() bool { _, bound, _ := c.BindingForTest("repo-a"); return !bound })
}

// A replacement file with a mode that is not owner-only is tightened and applied, as at startup; the
// polls that merely look for a change never touch the mode.
func TestReplacementFileWithLooseModeIsTightenedAndApplied(t *testing.T) {
	g := &scriptedGit{gate: make(chan struct{})}
	c, path := newFileClient(t, "{}", &fakeProber{result: gitsync.OriginReachable}, gitsync.WithGitRunner(g.run))
	t.Cleanup(g.release)
	configureRepo(c, &stubSender{}, "repo-a", "https://git.example.com/team/a.git")
	fileReload(t, c, 10*time.Millisecond, 10*time.Millisecond)

	replaceFile(t, path, providersJSON([3]string{"origin", "git.example.com", tokenOne}), 0o644)
	waitFor(t, func() bool { _, bound, _ := c.BindingForTest("repo-a"); return bound })
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v (%v), want 0600 after the confirming read", info.Mode().Perm(), err)
	}
}

// A deliberately group-readable file is left alone by the polls that look for changes.
func TestPollingDoesNotChangeAFilesMode(t *testing.T) {
	c, path := newFileClient(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}), &fakeProber{result: gitsync.OriginReachable})
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	fileReload(t, c, 5*time.Millisecond, 5*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v (%v): polling changed a file nobody changed", info.Mode().Perm(), err)
	}
}

// A client built without a provider file has nothing to watch: starting and asking for a reload are
// harmless.
func TestClientWithoutAProviderFileHasNoWatcher(t *testing.T) {
	cfg := &config.Config{GatewayURL: "wss://gw.example.com", ConnectorID: "gitsync-test", ProtocolVersion: "v1"}
	c := gitsync.NewClient(cfg, auth.NewKeyStore(filepath.Join(t.TempDir(), "key")), nil, &fakeProber{}, fixedNow, "boot")
	stop := c.StartReloadForTest(context.Background())
	c.Reload()
	stop()
}

// A file that could not be read at all, and is then fixed to something readable, is picked up: the
// unreadable state must not hide its own repair.
func TestFileRepairedAfterBeingUnreadableIsPickedUp(t *testing.T) {
	logs := captureLogs(t)
	c, path := newFileClient(t, providersJSON([3]string{"origin", "git.example.com", tokenOne}), &fakeProber{result: gitsync.OriginReachable})
	configureRepo(c, &stubSender{}, "repo-a", "https://git.example.com/team/a.git")
	fileReload(t, c, 10*time.Millisecond, 10*time.Millisecond)

	replaceFile(t, path, providersJSON([3]string{"origin", "git.example.com", tokenOne}), 0o000)
	waitFor(t, func() bool { return strings.Contains(logs.String(), "cannot be used") })
	if _, bound, _ := c.BindingForTest("repo-a"); !bound {
		t.Fatal("an unreadable file revoked the configuration")
	}

	replaceFile(t, path, providersJSON([3]string{"origin", "git.example.com", tokenTwo}), 0o644)
	waitFor(t, func() bool {
		return c.ProviderTokenForTest("origin") == tokenTwo
	})
}

// tokenFileProvidersJSON is a provider file whose providers read their credential from files.
func tokenFileProvidersJSON(entries ...[3]string) string {
	var parts []string
	for _, e := range entries { // name, host, token file path
		parts = append(parts, fmt.Sprintf(`%q:{"base_url":"https://%s/","auth":"bearer","token_file":%q}`, e[0], e[1], e[2]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func writeTokenFile(t *testing.T, path, token string) {
	t.Helper()
	replaceFile(t, path, token+"\n", 0o600)
}

// Rotating the credential by overwriting its token file, which the provider file does not change, is
// noticed: the sync using the old credential is stopped and the repository is synced with the new one.
func TestRotatingATokenFileRotatesTheCredentialLive(t *testing.T) {
	logs := captureLogs(t)
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "git-token")
	writeTokenFile(t, tokenPath, tokenOne)
	g := &scriptedGit{gate: make(chan struct{})}
	c, _ := newFileClient(t, tokenFileProvidersJSON([3]string{"origin", "git.example.com", tokenPath}),
		&fakeProber{result: gitsync.OriginReachable}, gitsync.WithGitRunner(g.run))
	t.Cleanup(g.release)
	configureRepo(c, &stubSender{}, "repo-a", "https://git.example.com/team/a.git")
	waitFor(t, func() bool { return g.count("fetch") == 1 })
	fileReload(t, c, 10*time.Millisecond, 10*time.Millisecond)

	writeTokenFile(t, tokenPath, tokenTwo)

	waitFor(t, func() bool { return g.count("fetch") == 2 })
	var fetchEnvs [][]string
	g.mu.Lock()
	for i, a := range g.argvs {
		if subOf(a) == "fetch" {
			fetchEnvs = append(fetchEnvs, g.envs[i])
		}
	}
	g.mu.Unlock()
	if !envHas(fetchEnvs[0], tokenOne) || !envHas(fetchEnvs[1], tokenTwo) || envHas(fetchEnvs[1], tokenOne) {
		t.Error("the rerun did not use only the new credential")
	}
	assertNoSecrets(t, "logs", logs.String(), tokenOne, tokenTwo, testPushToken)
}

// A Kubernetes Secret volume updates by swapping a symlink; the swap is a change of the token.
func TestSwappingATokenFileSymlinkIsNoticed(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []struct{ name, token string }{{"v1", tokenOne}, {"v2", tokenTwo}} {
		if err := os.MkdirAll(filepath.Join(dir, v.name), 0o700); err != nil {
			t.Fatal(err)
		}
		writeTokenFile(t, filepath.Join(dir, v.name, "token"), v.token)
	}
	link := filepath.Join(dir, "..data")
	if err := os.Symlink("v1", link); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, "git-token")
	if err := os.Symlink(filepath.Join("..data", "token"), tokenPath); err != nil {
		t.Fatal(err)
	}
	c, _ := newFileClient(t, tokenFileProvidersJSON([3]string{"origin", "git.example.com", tokenPath}), &fakeProber{result: gitsync.OriginReachable})
	fileReload(t, c, 10*time.Millisecond, 10*time.Millisecond)

	if err := os.Symlink("v2", link+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(link+".tmp", link); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return c.ProviderTokenForTest("origin") == tokenTwo })
}

// A token file that is deleted drops its provider only: that provider's syncs are stopped and its
// repositories are denied, while the other providers carry on. The file coming back restores it.
func TestDeletingATokenFileDropsOnlyItsProvider(t *testing.T) {
	logs := captureLogs(t)
	dir := t.TempDir()
	goneToken, keptToken := filepath.Join(dir, "gone-token"), filepath.Join(dir, "kept-token")
	writeTokenFile(t, goneToken, tokenOne)
	writeTokenFile(t, keptToken, tokenTwo)
	g := &scriptedGit{gate: make(chan struct{})}
	c, _ := newFileClient(t, tokenFileProvidersJSON([3]string{"gone", "git.example.com", goneToken}, [3]string{"kept", "other.example.com", keptToken}),
		&fakeProber{result: gitsync.OriginReachable}, gitsync.WithGitRunner(g.run))
	t.Cleanup(g.release)
	s := &stubSender{}
	c.HandleMessage(context.Background(), s, "git_sync_config", configJSONRaw("1",
		repoJSON("repo-gone", "https://git.example.com/a.git", "", "https://replica.example.com/r/a.git", testPushToken),
		repoJSON("repo-kept", "https://other.example.com/b.git", "", "https://replica.example.com/r/b.git", testPushToken)))
	waitFor(t, func() bool { return c.AttemptsForTest() == 2 || g.count("fetch") == 2 })
	fileReload(t, c, 10*time.Millisecond, 10*time.Millisecond)

	if err := os.Remove(goneToken); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, bound, _ := c.BindingForTest("repo-gone"); return !bound })
	if _, bound, _ := c.BindingForTest("repo-kept"); !bound {
		t.Error("the provider whose token file is intact was dropped too")
	}
	waitFor(t, func() bool { return strings.Contains(logs.String(), "no longer exists") })
	assertNoSecrets(t, "logs", logs.String(), tokenOne, tokenTwo)

	writeTokenFile(t, goneToken, tokenOne)
	waitFor(t, func() bool { _, bound, _ := c.BindingForTest("repo-gone"); return bound })
}

// A token file that exists but cannot be used (here empty, as while it is being rewritten) is not a
// revocation: the last good configuration stays in force.
func TestUnusableTokenFileKeepsTheLastGood(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "git-token")
	writeTokenFile(t, tokenPath, tokenOne)
	c, _ := newFileClient(t, tokenFileProvidersJSON([3]string{"origin", "git.example.com", tokenPath}), &fakeProber{result: gitsync.OriginReachable})
	configureRepo(c, &stubSender{}, "repo-a", "https://git.example.com/team/a.git")
	fileReload(t, c, 10*time.Millisecond, 10*time.Millisecond)

	replaceFile(t, tokenPath, "", 0o600)
	time.Sleep(300 * time.Millisecond)
	if _, bound, _ := c.BindingForTest("repo-a"); !bound || c.ProviderTokenForTest("origin") != tokenOne {
		t.Error("an empty token file revoked or replaced the credential in force")
	}
	writeTokenFile(t, tokenPath, tokenTwo)
	waitFor(t, func() bool { return c.ProviderTokenForTest("origin") == tokenTwo })
}
