package gitsync

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var mirrorDirNameRE = regexp.MustCompile(`^[0-9a-f]{64}\.git$`)

func TestMirrorDirIsDeterministicAndSafe(t *testing.T) {
	m := Mirrors{Root: "/var/lib/gitsync/mirrors"}

	a := m.Dir("repo-a")
	if a != m.Dir("repo-a") {
		t.Error("Dir must be deterministic")
	}
	if filepath.Dir(a) != m.Root {
		t.Errorf("mirror must sit directly under Root, got %q", a)
	}
	base := filepath.Base(a)
	if !mirrorDirNameRE.MatchString(base) {
		t.Errorf("mirror dir name %q is not <64 hex>.git", base)
	}
	if strings.Contains(base, "repo-a") {
		t.Errorf("repo_id must not appear in the directory name: %q", base)
	}
	if a == m.Dir("repo-b") {
		t.Error("different repo_ids must map to different directories")
	}
}

func TestMirrorDirHandlesLongAndTrickyIDs(t *testing.T) {
	m := Mirrors{Root: "/r"}
	long := strings.Repeat("a", 256) // above the 255-byte NAME_MAX
	ids := []string{long, "Repo-A", "repo-a", "..", "a..b", "-rf", "repo.git"}

	seen := map[string]string{}
	for _, id := range ids {
		dir := m.Dir(id)
		if filepath.Dir(dir) != "/r" || !mirrorDirNameRE.MatchString(filepath.Base(dir)) {
			t.Errorf("Dir(%q) = %q is not a flat, fixed-format path", id, dir)
		}
		if other, dup := seen[dir]; dup {
			t.Errorf("ids %q and %q collide on %q", id, other, dir)
		}
		seen[dir] = id
	}
}

func TestMirrorsExists(t *testing.T) {
	m := Mirrors{Root: t.TempDir()}
	if m.Exists("repo-a") {
		t.Fatal("Exists must be false before the mirror is created")
	}
	if err := os.MkdirAll(m.Dir("repo-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !m.Exists("repo-a") {
		t.Error("Exists must be true once the directory exists")
	}
	if m.Exists("repo-b") {
		t.Error("Exists must be per repo_id")
	}
}

func TestMirrorsEnsureRootCreatesOwnerOnlyDir(t *testing.T) {
	m := Mirrors{Root: filepath.Join(t.TempDir(), "nested", "mirrors")}
	if err := m.EnsureRoot(); err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	info, err := os.Stat(m.Root)
	if err != nil || !info.IsDir() {
		t.Fatalf("root not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("mirror root must be owner-only, got %v", perm)
	}
}

func TestDefaultMirrorsRoot(t *testing.T) {
	t.Run("override", func(t *testing.T) {
		t.Setenv("GIT_SYNC_MIRRORS_DIR", "/custom/mirrors")
		if got := DefaultMirrorsRoot(); got != "/custom/mirrors" {
			t.Errorf("DefaultMirrorsRoot = %q, want the override", got)
		}
	})
	t.Run("beside the config file", func(t *testing.T) {
		t.Setenv("GIT_SYNC_MIRRORS_DIR", "")
		t.Setenv("GIT_SYNC_CONFIG_FILE", "/data/volume/git-sync.json")
		if got, want := DefaultMirrorsRoot(), "/data/volume/mirrors"; got != want {
			t.Errorf("DefaultMirrorsRoot = %q, want %q", got, want)
		}
	})
}
