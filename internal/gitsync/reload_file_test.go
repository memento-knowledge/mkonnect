package gitsync_test

import (
	"context"
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
