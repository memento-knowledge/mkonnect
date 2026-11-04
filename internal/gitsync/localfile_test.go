package gitsync

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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
		if got := isForeignUID(tc.uid, tc.euid); got != tc.want {
			t.Errorf("%s: isForeignUID(%d, %d) = %v, want %v", tc.name, tc.uid, tc.euid, got, tc.want)
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

// readConfig reads a file under the provider-config policy with the shipped owner setting.
func readConfig(path, _ string, maxSize int64) ([]byte, error) {
	cfg, err := readProviderConfig(path, maxSize, rejectForeignConfigOwner)
	return cfg.data, err
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

func TestReadConfigTightensLooseMode(t *testing.T) {
	p := writeFile(t, t.TempDir(), "f", "x", 0o644)
	if _, err := readConfig(p, "test file", 1024); err != nil {
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

func TestReadConfigRejectsNonRegularAndOversized(t *testing.T) {
	dir := t.TempDir()
	if _, err := readConfig(dir, "test file", 1024); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a directory should be rejected as not a regular file, got %v", err)
	}

	big := writeFile(t, dir, "big", strings.Repeat("SECRET", 1000), 0o600)
	_, err := readConfig(big, "test file", 1024)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("an oversized file should be rejected, got %v", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error leaks file content: %v", err)
	}

	exact := writeFile(t, dir, "exact", strings.Repeat("a", 1024), 0o600)
	if _, err := readConfig(exact, "test file", 1024); err != nil {
		t.Errorf("a file exactly at the limit should be accepted: %v", err)
	}
}

func TestReadConfigFollowsSymlink(t *testing.T) {
	// Kubernetes projects Secret and ConfigMap volumes through symlinks.
	dir := t.TempDir()
	target := writeFile(t, dir, "target", "hello", 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	b, err := readConfig(link, "test file", 1024)
	if err != nil || string(b) != "hello" {
		t.Fatalf("symlinked file: got %q, %v", b, err)
	}
}

func TestReadConfigMissingKeepsNotExist(t *testing.T) {
	_, err := readConfig(filepath.Join(t.TempDir(), "nope"), "test file", 1024)
	if !os.IsNotExist(err) {
		t.Fatalf("a missing file must stay recognisable with os.IsNotExist, got %v", err)
	}
}

func TestReadConfigUnsafeModeWhenChmodFails(t *testing.T) {
	prev := fchmod
	fchmod = func(*os.File, os.FileMode) error { return os.ErrPermission }
	defer func() { fchmod = prev }()

	dir := t.TempDir()
	safe := writeFile(t, dir, "safe", "x", 0o640)
	if _, err := readConfig(safe, "test file", 1024); err != nil {
		t.Errorf("0640 on a mount that cannot be chmod-ed should be accepted: %v", err)
	}
	for _, mode := range []os.FileMode{0o660, 0o604, 0o666} {
		p := writeFile(t, dir, "unsafe", "x", mode)
		if _, err := readConfig(p, "test file", 1024); err == nil {
			t.Errorf("mode %o that cannot be chmod-ed should be rejected", mode)
		}
	}
}

func TestReadConfigWarnsAboutWorldWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	p := writeFile(t, dir, "f", "x", 0o600)

	logs := captureLogs(t)
	if _, err := readConfig(p, "test file", 1024); err != nil {
		t.Fatalf("a world-writable directory must warn, not reject: %v", err)
	}
	if !strings.Contains(logs.String(), "directory is writable by other users") {
		t.Errorf("expected a directory warning, got logs %q", logs.String())
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	quiet := captureLogs(t)
	if _, err := readConfig(p, "test file", 1024); err != nil {
		t.Fatal(err)
	}
	if quiet.String() != "" {
		t.Errorf("a private directory and own file should not warn, got %q", quiet.String())
	}
}

// Opening a FIFO for reading blocks until a writer appears; the file must be rejected as not
// regular without ever waiting for one.
func TestReadConfigRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{fifo, link} {
		done := make(chan error, 1)
		go func() {
			_, err := readConfig(p, "test file", 1024)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Errorf("%s: want a not-a-regular-file error, got %v", filepath.Base(p), err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: readSecureFile blocked on a FIFO", filepath.Base(p))
		}
	}
}

// withOwner presents every file as owned by uid while the running user is euid.
func withOwner(t *testing.T, uid, euid int) {
	t.Helper()
	prevOwner, prevEUID := fileOwnerUID, effectiveUID
	fileOwnerUID = func(os.FileInfo) (int, bool) { return uid, true }
	effectiveUID = func() int { return euid }
	t.Cleanup(func() { fileOwnerUID, effectiveUID = prevOwner, prevEUID })
}

func TestReadProviderConfigOwnerPolicy(t *testing.T) {
	for _, tc := range []struct {
		name          string
		uid, euid     int
		rejectForeign bool
		wantErr       bool
		wantForeign   bool
	}{
		{"own file", 1000, 1000, true, false, false},
		{"root-owned", 0, 1000, true, false, false},
		{"foreign, reported only", 1001, 1000, false, false, true},
		{"foreign, rejected", 1001, 1000, true, true, true},
		{"foreign while running as root", 1001, 0, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withOwner(t, tc.uid, tc.euid)
			p := writeFile(t, t.TempDir(), "f", "SECRET", 0o644)
			cfg, err := readProviderConfig(p, 1024, tc.rejectForeign)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error leaks file content: %v", err)
			}
			if cfg.foreignOwner != tc.wantForeign {
				t.Errorf("foreignOwner = %v, want %v", cfg.foreignOwner, tc.wantForeign)
			}
			info, statErr := os.Stat(p)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if tc.wantErr && info.Mode().Perm() != 0o644 {
				t.Errorf("a rejected config must not be chmod-ed first; mode = %o", info.Mode().Perm())
			}
			if !tc.wantErr && info.Mode().Perm() != 0o600 {
				t.Errorf("an accepted config is tightened to 0600; mode = %o", info.Mode().Perm())
			}
		})
	}
}

func TestOwnerThatCannotBeDeterminedFailsClosed(t *testing.T) {
	prev := fileOwnerUID
	fileOwnerUID = func(os.FileInfo) (int, bool) { return 0, false }
	t.Cleanup(func() { fileOwnerUID = prev })

	p := writeFile(t, t.TempDir(), "f", "x", 0o600)
	if cfg, err := readProviderConfig(p, 1024, false); err != nil || !cfg.foreignOwner {
		t.Errorf("config: foreignOwner = %v, err = %v; want foreign and no error", cfg.foreignOwner, err)
	}
	if _, err := readTokenFile(p, "test token_file"); err == nil || !strings.Contains(err.Error(), "owned by another user") {
		t.Errorf("token file with an unknown owner should be rejected, got %v", err)
	}
}

// The token file belongs to whoever provisioned the secret, often a read-only mount: it is vetted
// but never modified.
func TestReadTokenFileDoesNotChangeMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o400, 0o440, 0o640, 0o644} {
		p := writeFile(t, t.TempDir(), "t", "x", mode)
		if _, err := readTokenFile(p, "test token_file"); err != nil {
			t.Fatalf("mode %o: %v", mode, err)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("mode %o was changed to %o", mode, info.Mode().Perm())
		}
	}
}

func TestReadTokenFileSizeBoundary(t *testing.T) {
	dir := t.TempDir()
	exact := writeFile(t, dir, "exact", strings.Repeat("a", maxTokenFileSize), 0o600)
	if b, err := readTokenFile(exact, "test token_file"); err != nil || len(b) != maxTokenFileSize {
		t.Errorf("a file exactly at the limit should be read: %d bytes, %v", len(b), err)
	}
	over := writeFile(t, dir, "over", strings.Repeat("SECRET", 1000), 0o600)
	_, err := readTokenFile(over, "test token_file")
	if err == nil || !strings.Contains(err.Error(), "limit") || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("an oversized file should be rejected without its content, got %v", err)
	}
}
