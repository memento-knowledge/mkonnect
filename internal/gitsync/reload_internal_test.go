package gitsync

import (
	"context"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedSource answers each read from a list, then repeats the last answer. It records how many
// reads were made and which were confirming ones.
type scriptedSource struct {
	mu      sync.Mutex
	answers []configBundle
	reads   int
	confirm []bool
	onRead  func(n int) // called with the 1-based read number, before it answers
}

func (s *scriptedSource) read(confirm bool) configBundle {
	s.mu.Lock()
	s.reads++
	n := s.reads
	s.confirm = append(s.confirm, confirm)
	hook := s.onRead
	s.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := n - 1
	if i >= len(s.answers) {
		i = len(s.answers) - 1
	}
	return s.answers[i]
}

func (s *scriptedSource) set(answers ...configBundle) {
	s.mu.Lock()
	s.answers, s.reads = answers, 0
	s.mu.Unlock()
}

func (s *scriptedSource) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func okBundle(label string) configBundle {
	return configBundle{kind: bundleOK, digest: contentDigest([]byte(label)), store: &Store{data: map[string]LocalEntry{label: {}}}}
}

func badBundle(label, category string) configBundle {
	return configBundle{kind: bundleInvalid, digest: contentDigest([]byte(label)), category: category}
}

func missingBundle() configBundle { return configBundle{kind: bundleMissing, digest: missingDigest()} }

// applyLog records what the reloader applied. A nil store is the empty configuration.
type applyLog struct {
	mu     sync.Mutex
	stores []*Store
}

func (a *applyLog) apply(s *Store) {
	a.mu.Lock()
	a.stores = append(a.stores, s)
	a.mu.Unlock()
}

func (a *applyLog) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.stores)
}

func (a *applyLog) names(i int) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stores[i] == nil {
		return nil
	}
	return a.stores[i].Names()
}

// startReloader runs a reloader with short timings until the test ends.
func startReloader(t *testing.T, src configSource, applied digest, poll, settle time.Duration) (*reloader, *applyLog) {
	t.Helper()
	log := &applyLog{}
	return startReloaderLogging(t, src, applied, poll, settle, log), log
}

// startReloaderLogging is startReloader with the log of applies made by the caller, for a test whose
// source needs to look at it.
func startReloaderLogging(t *testing.T, src configSource, applied digest, poll, settle time.Duration, log *applyLog) *reloader {
	t.Helper()
	r := newReloader(src, applied, log.apply)
	r.poll, r.settle = poll, settle
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

func eventuallyTrue(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func quietly(d time.Duration) { time.Sleep(d) }

// A change is applied only once a second sighting, a settle later, shows the same thing; what the
// first sighting saw is not applied if the second sees something else.
func TestReloadAppliesOnlyWhatHoldsAcrossTheSettle(t *testing.T) {
	src := &scriptedSource{}
	// Sighting 1 sees B, the settle read sees C (the file moved on), and C then holds.
	src.set(okBundle("b"), okBundle("c"), okBundle("c"))
	got := &applyLog{}
	var appliedBeforeThirdRead int
	src.onRead = func(n int) {
		if n == 3 {
			appliedBeforeThirdRead = got.count()
		}
	}
	startReloaderLogging(t, src, contentDigest([]byte("a")), 5*time.Millisecond, 5*time.Millisecond, got)
	eventuallyTrue(t, "an apply", func() bool { return got.count() >= 1 })
	quietly(60 * time.Millisecond)
	if got.count() != 1 {
		t.Fatalf("applied %d times, want once", got.count())
	}
	if names := got.names(0); len(names) != 1 || names[0] != "c" {
		t.Errorf("applied %v, want the configuration that held (c), never the intermediate one", names)
	}
	if appliedBeforeThirdRead != 0 {
		t.Error("c was applied on its first sighting, before it had held for a settle")
	}
	// Both sightings that were looking for a change were silent reads; only the settle reads confirm.
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.confirm[0] {
		t.Error("the first sighting must be a read with no side effects")
	}
	if !src.confirm[1] {
		t.Error("the settle read must be the confirming one")
	}
}

// A file that changes and changes back within the settle is never applied.
func TestReloadNeverAppliesAFlappingFile(t *testing.T) {
	src := &scriptedSource{}
	a := okBundle("a")
	src.set(okBundle("b"), a) // B at the first sighting, back to A at the settle read
	_, got := startReloader(t, src, a.digest, 5*time.Millisecond, 5*time.Millisecond)
	eventuallyTrue(t, "several polls", func() bool { return src.readCount() >= 8 })
	if got.count() != 0 {
		t.Errorf("applied %d times; a file that went back must not be applied", got.count())
	}
}

// A file that keeps changing is never applied while it keeps changing, and is applied once it stops.
func TestReloadWaitsOutAFileThatKeepsChanging(t *testing.T) {
	src := &scriptedSource{}
	src.set(okBundle("1"), okBundle("2"), okBundle("3"), okBundle("4"), okBundle("5"), okBundle("5"))
	_, got := startReloader(t, src, contentDigest([]byte("a")), 5*time.Millisecond, 5*time.Millisecond)
	eventuallyTrue(t, "an apply", func() bool { return got.count() >= 1 })
	if got.count() != 1 || got.names(0)[0] != "5" {
		t.Errorf("applied %d times, first %v; want only the configuration that finally held", got.count(), got.names(0))
	}
}

// SIGHUP applies at once, without waiting for a poll or the settle.
func TestSIGHUPAppliesWithoutTheSettle(t *testing.T) {
	src := &scriptedSource{}
	src.set(okBundle("b"))
	r, got := startReloader(t, src, contentDigest([]byte("a")), time.Hour, time.Hour)
	r.requestReload()
	eventuallyTrue(t, "an apply", func() bool { return got.count() == 1 })
	if r.pending {
		t.Error("a forced reload left a settle pending")
	}
}

// A file that is missing when SIGHUP arrives still waits the settle, so an editor's delete-and-recreate
// cannot revoke everything; it is applied as empty only if it is still missing afterwards.
func TestSIGHUPOnAMissingFileStillSettles(t *testing.T) {
	src := &scriptedSource{}
	var appliedAtSecondRead int
	got := &applyLog{}
	src.onRead = func(n int) {
		if n == 2 {
			appliedAtSecondRead = got.count()
		}
	}
	src.set(missingBundle())
	r := startReloaderLogging(t, src, contentDigest([]byte("a")), time.Hour, 20*time.Millisecond, got)
	r.requestReload()
	eventuallyTrue(t, "the file to be treated as empty", func() bool { return got.count() == 1 })
	if appliedAtSecondRead != 0 {
		t.Error("the empty configuration was applied before the file had been missing for the settle")
	}
	if got.names(0) != nil {
		t.Errorf("applied %v, want the empty configuration", got.names(0))
	}
}

// ...and if the file is back by then, nothing is revoked.
func TestSIGHUPOnAFileThatReappearsRevokesNothing(t *testing.T) {
	src := &scriptedSource{}
	a := okBundle("a")
	src.set(missingBundle(), a)
	r, got := startReloader(t, src, a.digest, time.Hour, 20*time.Millisecond)
	r.requestReload()
	eventuallyTrue(t, "the settle read", func() bool { return src.readCount() >= 2 })
	quietly(60 * time.Millisecond)
	if got.count() != 0 {
		t.Errorf("applied %d times; a file that reappeared unchanged must revoke nothing", got.count())
	}
}

// A SIGHUP that arrives while a change is waiting out its settle applies it, and the timer that was
// waiting does not apply it again.
func TestSIGHUPDuringASettleAppliesOnce(t *testing.T) {
	src := &scriptedSource{}
	src.set(okBundle("b"))
	r, got := startReloader(t, src, contentDigest([]byte("a")), 5*time.Millisecond, 80*time.Millisecond)
	eventuallyTrue(t, "a first sighting", func() bool { return src.readCount() >= 1 })
	r.requestReload()
	eventuallyTrue(t, "an apply", func() bool { return got.count() >= 1 })
	quietly(200 * time.Millisecond)
	if got.count() != 1 {
		t.Errorf("applied %d times, want once", got.count())
	}
}

// An unusable file leaves the last good configuration in force, however long it stays unusable, and
// is reported once, with a fixed category and nothing from the file or its digest.
func TestInvalidFileKeepsLastGoodAndLogsOnce(t *testing.T) {
	logs := captureLogs(t)
	src := &scriptedSource{}
	bad := badBundle("canary-token-0123456789 {not json", loadInvalidJSON)
	src.set(bad)
	a := contentDigest([]byte("a"))
	_, got := startReloader(t, src, a, 3*time.Millisecond, 3*time.Millisecond)
	eventuallyTrue(t, "many polls", func() bool { return src.readCount() >= 12 })
	if got.count() != 0 {
		t.Errorf("applied %d times; an unusable file must never be applied", got.count())
	}
	out := logs.String()
	if n := strings.Count(out, "cannot be used"); n != 1 {
		t.Errorf("reported %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, loadInvalidJSON) {
		t.Errorf("log lacks the category:\n%s", out)
	}
	for _, leak := range []string{"canary-token", hex.EncodeToString(bad.digest[:]), hex.EncodeToString(bad.digest[:4])} {
		if strings.Contains(out, leak) {
			t.Errorf("log contains %q", leak)
		}
	}
	// The same file fixed is applied; the same unusable content returning is reported again.
	src.set(okBundle("b"))
	eventuallyTrue(t, "the fix to be applied", func() bool { return got.count() == 1 })
}

// Different unusable contents are each reported.
func TestEachDistinctUnusableContentIsReported(t *testing.T) {
	logs := captureLogs(t)
	src := &scriptedSource{}
	src.set(badBundle("x", loadInvalidJSON))
	_, _ = startReloader(t, src, contentDigest([]byte("a")), 3*time.Millisecond, 3*time.Millisecond)
	eventuallyTrue(t, "the first report", func() bool { return strings.Count(logs.String(), "cannot be used") == 1 })
	src.set(badBundle("y", loadInvalidConfig))
	eventuallyTrue(t, "the second report", func() bool { return strings.Count(logs.String(), "cannot be used") == 2 })
	quietly(40 * time.Millisecond)
	if n := strings.Count(logs.String(), "cannot be used"); n != 2 {
		t.Errorf("reported %d times, want 2", n)
	}
}

// A provider file that is deleted after startup fails closed: after the settle the configuration is
// empty, and the log says so loudly, once.
func TestDeletedFileFailsClosedAfterTheSettleWithALoudLog(t *testing.T) {
	logs := captureLogs(t)
	src := &scriptedSource{}
	src.set(missingBundle())
	_, got := startReloader(t, src, contentDigest([]byte("a")), 3*time.Millisecond, 3*time.Millisecond)
	eventuallyTrue(t, "an apply", func() bool { return got.count() == 1 })
	quietly(60 * time.Millisecond)
	if got.count() != 1 || got.names(0) != nil {
		t.Fatalf("applied %d times (%v), want the empty configuration once", got.count(), got.names(0))
	}
	out := logs.String()
	if n := strings.Count(out, "no longer exists"); n != 1 || !strings.Contains(out, "level=ERROR") {
		t.Errorf("want one ERROR line about the missing file, got:\n%s", out)
	}
}

// A file that was never there is not an event: starting without one logs nothing and revokes nothing.
func TestFileMissingAtStartupIsNotAnAlarm(t *testing.T) {
	logs := captureLogs(t)
	src := &scriptedSource{}
	src.set(missingBundle())
	_, got := startReloader(t, src, missingDigest(), 3*time.Millisecond, 3*time.Millisecond)
	eventuallyTrue(t, "several polls", func() bool { return src.readCount() >= 8 })
	if got.count() != 0 || logs.String() != "" {
		t.Errorf("applied %d times, logged %q; want nothing", got.count(), logs.String())
	}
}

// A confirming read tightens a file's mode, which changes what an unusable file reads as: that is a
// new first sighting, not a confirmation of the old one.
func TestConfirmingReadThatSeesSomethingElseStartsTheWaitOver(t *testing.T) {
	src := &scriptedSource{}
	src.set(badBundle("mode", loadUnreadable), okBundle("fixed"), okBundle("fixed"))
	_, got := startReloader(t, src, contentDigest([]byte("a")), 5*time.Millisecond, 5*time.Millisecond)
	eventuallyTrue(t, "an apply", func() bool { return got.count() == 1 })
	if names := got.names(0); len(names) != 1 || names[0] != "fixed" {
		t.Errorf("applied %v", names)
	}
}

// A SIGHUP for a configuration that has not changed says so and applies nothing.
func TestSIGHUPWithNoChangeAppliesNothing(t *testing.T) {
	logs := captureLogs(t)
	src := &scriptedSource{}
	a := okBundle("a")
	src.set(a)
	r, got := startReloader(t, src, a.digest, time.Hour, time.Hour)
	r.requestReload()
	eventuallyTrue(t, "the report", func() bool { return strings.Contains(logs.String(), "unchanged") })
	if got.count() != 0 {
		t.Errorf("applied %d times", got.count())
	}
}
