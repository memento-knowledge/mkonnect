package gitsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForeignOwner(t *testing.T) {
	for _, tc := range []struct {
		name      string
		uid, euid int
		want      bool
	}{
		{"own file", 1000, 1000, false},
		{"root-owned (Secret mount)", 0, 1000, false},
		{"other user", 1001, 1000, true},
		{"running as root, other user's file", 1001, 0, true},
	} {
		if got := foreignOwner(tc.uid, tc.euid); got != tc.want {
			t.Errorf("%s: foreignOwner(%d, %d) = %v, want %v", tc.name, tc.uid, tc.euid, got, tc.want)
		}
	}
}

func TestDirLetsOthersRename(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
		want bool
	}{
		{"private", os.ModeDir | 0o700, false},
		{"group/other read only", os.ModeDir | 0o755, false},
		{"group writable only", os.ModeDir | 0o770, false},
		{"world writable", os.ModeDir | 0o777, true},
		{"world writable with sticky bit (like /tmp)", os.ModeDir | os.ModeSticky | 0o777, false},
	} {
		if got := dirLetsOthersRename(tc.mode); got != tc.want {
			t.Errorf("%s: dirLetsOthersRename(%v) = %v, want %v", tc.name, tc.mode, got, tc.want)
		}
	}
}

func writeFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadSecureFileTightensLooseMode(t *testing.T) {
	p := writeFile(t, t.TempDir(), "f", "x", 0o644)
	if _, err := readSecureFile(p, "test file", 1024); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

func TestReadSecureFileRejectsNonRegularAndOversized(t *testing.T) {
	dir := t.TempDir()
	if _, err := readSecureFile(dir, "test file", 1024); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a directory should be rejected as not a regular file, got %v", err)
	}

	big := writeFile(t, dir, "big", strings.Repeat("SECRET", 1000), 0o600)
	_, err := readSecureFile(big, "test file", 1024)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("an oversized file should be rejected, got %v", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error leaks file content: %v", err)
	}

	exact := writeFile(t, dir, "exact", strings.Repeat("a", 1024), 0o600)
	if _, err := readSecureFile(exact, "test file", 1024); err != nil {
		t.Errorf("a file exactly at the limit should be accepted: %v", err)
	}
}

func TestReadSecureFileFollowsSymlink(t *testing.T) {
	// Kubernetes projects Secret and ConfigMap volumes through symlinks.
	dir := t.TempDir()
	target := writeFile(t, dir, "target", "hello", 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	b, err := readSecureFile(link, "test file", 1024)
	if err != nil || string(b) != "hello" {
		t.Fatalf("symlinked file: got %q, %v", b, err)
	}
}

func TestReadSecureFileMissingKeepsNotExist(t *testing.T) {
	_, err := readSecureFile(filepath.Join(t.TempDir(), "nope"), "test file", 1024)
	if !os.IsNotExist(err) {
		t.Fatalf("a missing file must stay recognisable with os.IsNotExist, got %v", err)
	}
}

func TestReadSecureFileUnsafeModeWhenChmodFails(t *testing.T) {
	prev := fchmod
	fchmod = func(*os.File, os.FileMode) error { return os.ErrPermission }
	defer func() { fchmod = prev }()

	dir := t.TempDir()
	safe := writeFile(t, dir, "safe", "x", 0o640)
	if _, err := readSecureFile(safe, "test file", 1024); err != nil {
		t.Errorf("0640 on a mount that cannot be chmod-ed should be accepted: %v", err)
	}
	for _, mode := range []os.FileMode{0o660, 0o604, 0o666} {
		p := writeFile(t, dir, "unsafe", "x", mode)
		if _, err := readSecureFile(p, "test file", 1024); err == nil {
			t.Errorf("mode %o that cannot be chmod-ed should be rejected", mode)
		}
	}
}

func TestReadSecureFileWarnsAboutWorldWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	p := writeFile(t, dir, "f", "x", 0o600)

	logs := captureLogs(t)
	if _, err := readSecureFile(p, "test file", 1024); err != nil {
		t.Fatalf("a world-writable directory must warn, not reject: %v", err)
	}
	if !strings.Contains(logs.String(), "directory is writable by other users") {
		t.Errorf("expected a directory warning, got logs %q", logs.String())
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	quiet := captureLogs(t)
	if _, err := readSecureFile(p, "test file", 1024); err != nil {
		t.Fatal(err)
	}
	if quiet.String() != "" {
		t.Errorf("a private directory and own file should not warn, got %q", quiet.String())
	}
}
