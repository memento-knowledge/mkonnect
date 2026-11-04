package gitsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stubGit writes an executable that records its own pid and its helper's, then waits. onTerm is the
// shell trap body for SIGTERM ("" ignores it).
func stubGit(t *testing.T, dir, onTerm string) (script, pidFile, helperFile, marker string) {
	t.Helper()
	pidFile = filepath.Join(dir, "pid")
	helperFile = filepath.Join(dir, "helper")
	marker = filepath.Join(dir, "terminated")
	body := "#!/bin/sh\n" +
		"echo $$ > " + pidFile + "\n" +
		"trap '" + onTerm + "' TERM\n" +
		"sleep 300 &\n" +
		"echo $! > " + helperFile + "\n" +
		"wait\n"
	if onTerm == "" {
		body = "#!/bin/sh\ntrap '' TERM\necho $$ > " + pidFile + "\nsleep 300 &\necho $! > " + helperFile + "\nwhile true; do sleep 1; done\n"
	}
	script = filepath.Join(dir, "git")
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, pidFile, helperFile, marker
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
	return 0
}

func processGone(pid int) bool {
	// kill(pid, 0) succeeds for a zombie too, which a reaped-by-init helper is not for long.
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func waitGone(t *testing.T, what string, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("%s (pid %d) is still running", what, pid)
}

// Cancelling a git asks its group to terminate first, so git can remove its lock files, and the whole
// group (a helper included) is gone afterwards.
func TestCancelledGitIsTerminatedGracefullyAndItsHelperDies(t *testing.T) {
	dir := t.TempDir()
	script, pidFile, helperFile, marker := stubGit(t, dir, "echo term > "+filepath.Join(dir, "terminated")+"; exit 143")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runGitOut(ctx, []string{script}, []string{"PATH=/usr/bin:/bin"}, dir, nil)
		done <- err
	}()
	pid, helper := readPid(t, pidFile), readPid(t, helperFile)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runGitOut did not return after cancellation")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("git was not given SIGTERM before SIGKILL: %v", err)
	}
	waitGone(t, "git", pid)
	waitGone(t, "git's helper", helper)
}

// A git that ignores SIGTERM is killed once the grace has passed.
func TestCancelledGitThatIgnoresTermIsKilledAfterTheGrace(t *testing.T) {
	dir := t.TempDir()
	script, pidFile, helperFile, _ := stubGit(t, dir, "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runGitOut(ctx, []string{script}, []string{"PATH=/usr/bin:/bin"}, dir, nil)
	}()
	pid, helper := readPid(t, pidFile), readPid(t, helperFile)
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runGitOut did not return after cancellation")
	}
	if took := time.Since(start); took < gitKillGrace/2 {
		t.Errorf("returned in %v: SIGKILL arrived without the grace", took)
	}
	waitGone(t, "git", pid)
	waitGone(t, "git's helper", helper)
}

// A sync cancelled between two git invocations must not run the next one.
func TestCancelledSyncRunsNoFurtherGit(t *testing.T) {
	f := &fakeGit{}
	ctx, cancel := context.WithCancel(context.Background())
	d := newTestDeps(t, f)
	d.run = func(c context.Context, argv, env []string, dir string) ([]byte, error) {
		out, err := f.run(c, argv, env, dir)
		if (gitCall{argv: argv}).sub() == "fetch" {
			cancel() // the credential is superseded as the fetch completes
		}
		return out, err
	}
	got := syncRepo(ctx, d, testTask())
	if got.State != StatePending {
		t.Errorf("result = %+v, want pending", got)
	}
	if subs := f.subs(); len(subs) != 2 || subs[0] != "init" || subs[1] != "fetch" {
		t.Errorf("git calls = %v, want only init and fetch", subs)
	}
}

// A fetch killed because its sync was cancelled says nothing about the mirror, even when its stderr
// reads like corruption: the mirror must not be deleted and rebuilt.
func TestCancelledFetchNeverRebuildsTheMirror(t *testing.T) {
	f := &fakeGit{}
	ctx, cancel := context.WithCancel(context.Background())
	d := newTestDeps(t, f)
	task := testTask()
	dir := d.mirrors.Dir(task.RepoID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "keep")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.run = func(c context.Context, argv, env []string, dir string) ([]byte, error) {
		f.calls = append(f.calls, gitCall{argv: argv, env: env, dir: dir})
		cancel()
		return []byte("fatal: not a git repository"), errExit
	}
	got := syncRepo(ctx, d, task)
	if got.State != StatePending {
		t.Errorf("result = %+v, want pending", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the mirror was removed: %v", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("git calls = %v, want only the cancelled fetch", f.subs())
	}
}
