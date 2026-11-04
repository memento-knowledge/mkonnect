package gitsync_test

import (
	"context"
	"slices"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

const (
	lfsClientSHA   = "5dcfa042fb9b025599cd26145fd324dce4651d5d"
	lfsClientRule  = "*.psd filter=lfs diff=lfs merge=lfs -text\n"
	lfsClientEntry = "100644 blob " + lfsClientSHA + "      45\t.gitattributes\x00"
)

// TestLFSWarningLifecycleInStatus follows one repository through the states the warning can be in:
// reported by a healthy sync, kept while a re-sync runs and when a sync cannot tell, and cleared by a
// sync that finds no rule or by one that fails.
func TestLFSWarningLifecycleInStatus(t *testing.T) {
	want := []string{gitsync.WarnLFSNotReplicated}
	h := newHarness(t)
	h.git.set(func() {
		h.git.stdout["ls-tree"] = lfsClientEntry
		h.git.stdout["cat-file"] = lfsClientRule
	})
	h.configA()
	if r := h.settle(); r.State != gitsync.StateOK || !slices.Equal(r.Warnings, want) {
		t.Fatalf("status = %+v, want ok carrying %v", r, want)
	}
	if raw := string(h.wire("repo-a")["warnings"]); raw != `["lfs_not_replicated"]` {
		t.Errorf("warnings on the wire = %s", raw)
	}

	// A re-sync in flight neither drops nor invents the warning.
	gate := make(chan struct{})
	h.git.set(func() { h.git.gate = gate })
	h.syncNow()
	waitFor(t, func() bool { return h.git.count("fetch") == 2 })
	h.c.OnHeartbeat(context.Background(), h.s)
	if r := h.status("repo-a"); r.State != gitsync.StateSyncing || !slices.Equal(r.Warnings, want) {
		t.Errorf("status while re-syncing = %+v, want syncing still carrying %v", r, want)
	}
	close(gate)
	h.settle()

	// A sync that could not look (git failed) says nothing new, so the warning stays.
	h.git.set(func() { h.git.fail["ls-tree"] = []byte("fatal: bad object") })
	h.syncNow()
	if r := h.settle(); r.State != gitsync.StateOK || !slices.Equal(r.Warnings, want) {
		t.Errorf("status after an undecided sync = %+v, want ok still carrying %v", r, want)
	}

	// A sync that looked and found no rule clears it, and the field is omitted rather than empty.
	h.git.set(func() { delete(h.git.fail, "ls-tree"); h.git.stdout["ls-tree"] = "" })
	h.syncNow()
	if r := h.settle(); r.State != gitsync.StateOK || len(r.Warnings) != 0 {
		t.Errorf("status after the rule went away = %+v, want ok with no warnings", r)
	}
	if _, ok := h.wire("repo-a")["warnings"]; ok {
		t.Error("warnings must be omitted when there are none")
	}

	// It returns with the rule, and a failed sync does not carry it: it is for healthy repositories.
	h.git.set(func() { h.git.stdout["ls-tree"] = lfsClientEntry })
	h.syncNow()
	if r := h.settle(); !slices.Equal(r.Warnings, want) {
		t.Fatalf("status after the rule came back = %+v, want %v", r, want)
	}
	h.git.set(func() {
		h.git.fail["fetch"] = []byte("fatal: unable to access 'https://git.example.com/team/a.git/': The requested URL returned error: 403")
	})
	h.syncNow()
	if r := h.settle(); r.State != gitsync.StateError || len(r.Warnings) != 0 {
		t.Errorf("status after a failed sync = %+v, want an error with no warnings", r)
	}
}

// An undecided first sync has nothing to keep, and a pending repository carries no warning after a
// restart until a sync has run.
func TestLFSWarningAbsentWhenNeverDecided(t *testing.T) {
	h := newHarness(t)
	h.git.set(func() {
		h.git.stdout["ls-tree"] = lfsClientEntry
		h.git.stdout["cat-file"] = lfsClientRule
	})
	h.configA()
	if r := h.settle(); len(r.Warnings) != 1 {
		t.Fatalf("setup: %+v", r)
	}

	h2 := h.restart()
	h2.git.set(func() { h2.git.gate = make(chan struct{}) })
	h2.configA()
	waitFor(t, func() bool { return h2.git.count("fetch") == 1 })
	if r := h2.status("repo-a"); r.State != gitsync.StateSyncing || len(r.Warnings) != 0 {
		t.Errorf("status after a restart = %+v, want no warning before the first sync has finished", r)
	}
}
