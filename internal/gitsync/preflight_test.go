package gitsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseGitVersion(t *testing.T) {
	tests := []struct {
		out          string
		ver          string
		major, minor int
		ok           bool
	}{
		{"git version 2.39.5\n", "2.39.5", 2, 39, true},
		{"git version 2.31.0", "2.31.0", 2, 31, true},
		{"git version 2.31", "2.31", 2, 31, true},
		{"git version 2.39.5 (Apple Git-154)", "2.39.5", 2, 39, true},
		{"git version 2.43.0.windows.1", "2.43.0", 2, 43, true},
		{"git version 2.34.1-1ubuntu1.11", "2.34.1", 2, 34, true},
		{"git version 2.31.0-rc1", "2.31.0", 2, 31, true},
		{"git version 2.40.rc0", "2.40", 2, 40, true},
		{"git version 2.31-rc1", "2.31", 2, 31, true},
		{"git version 10.0.1", "10.0.1", 10, 0, true},
		{"git version 2", "", 0, 0, false},
		{"git version two.thirty", "", 0, 0, false},
		{"git version 99999999.1", "", 0, 0, false},
		{"version 2.39.5", "", 0, 0, false},
		{"hello", "", 0, 0, false},
		{"", "", 0, 0, false},
	}
	for _, tt := range tests {
		ver, major, minor, ok := ParseGitVersion(tt.out)
		if ver != tt.ver || major != tt.major || minor != tt.minor || ok != tt.ok {
			t.Errorf("ParseGitVersion(%q) = (%q, %d, %d, %v), want (%q, %d, %d, %v)", tt.out, ver, major, minor, ok, tt.ver, tt.major, tt.minor, tt.ok)
		}
	}
}

func TestGitVersionSupported(t *testing.T) {
	for _, tt := range []struct {
		major, minor int
		want         bool
	}{{2, 30, false}, {2, 31, true}, {2, 50, true}, {3, 0, true}, {1, 99, false}} {
		if got := GitVersionSupported(tt.major, tt.minor); got != tt.want {
			t.Errorf("GitVersionSupported(%d, %d) = %v", tt.major, tt.minor, got)
		}
	}
}

// The daemon logs GitVersionOK's detail at startup; its wording is part of what operators see.
func TestGitVersionOKMessages(t *testing.T) {
	stub := func(out string) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\necho '"+out+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
	}
	stub("git version 2.39.3")
	if ok, detail := GitVersionOK(); !ok || detail != "2.39.3" {
		t.Errorf("= (%v, %q)", ok, detail)
	}
	stub("git version 2.30.2")
	if ok, detail := GitVersionOK(); ok || detail != "git 2.30.2 is older than the required 2.31" {
		t.Errorf("= (%v, %q)", ok, detail)
	}
	stub("not git")
	if ok, detail := GitVersionOK(); ok || detail != "unrecognised `git --version` output" {
		t.Errorf("= (%v, %q)", ok, detail)
	}
	t.Setenv("PATH", t.TempDir())
	if ok, detail := GitVersionOK(); ok || detail != "git was not found on PATH or did not respond" {
		t.Errorf("= (%v, %q)", ok, detail)
	}
}

func TestTempRootPathDoesNotCreate(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	t.Setenv("GIT_SYNC_TMP_DIR", missing)
	path, source, err := TempRootPath()
	if err != nil || path != missing || source != "GIT_SYNC_TMP_DIR" {
		t.Fatalf("= (%q, %q, %v)", path, source, err)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("TempRootPath created the directory")
	}
	t.Setenv("GIT_SYNC_TMP_DIR", "")
	t.Setenv("TMPDIR", "relative")
	if _, source, err := TempRootPath(); err == nil || source != "TMPDIR" {
		t.Errorf("a relative TMPDIR must be rejected, got (%q, %v)", source, err)
	}
}

func TestCheckTempRootWritable(t *testing.T) {
	root := t.TempDir()
	if err := CheckTempRootWritable(root); err != nil {
		t.Fatalf("writable root: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("left %d entries behind", len(entries))
	}
	if err := CheckTempRootWritable(filepath.Join(root, "absent")); !os.IsNotExist(err) {
		t.Errorf("missing root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "absent")); err == nil {
		t.Error("the root was created")
	}
	f := filepath.Join(root, "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckTempRootWritable(f); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("a file: %v", err)
	}
}

type fakeProber struct {
	mu   sync.Mutex
	seen []LocalEntry
}

func (p *fakeProber) Probe(_ context.Context, e LocalEntry) (OriginResult, error) {
	p.mu.Lock()
	p.seen = append(p.seen, e)
	p.mu.Unlock()
	return OriginReachable, nil
}

func TestReadConfigNeverChangesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "git-sync.json")
	body := `{"a": {"base_url": "https://a.example.test/", "auth": "bearer", "token": "tok-0123456789abcdef"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := ReadConfig(path)
	// A loose mode is a failure (see TestReadConfigLooseModeIsAFailure) and is also reported as a
	// warning with the mode it has; the provider itself is still read.
	if len(cfg.Report.Issues) != 1 || len(cfg.Report.Providers) != 1 {
		t.Fatalf("report = %+v", cfg.Report)
	}
	if len(cfg.Report.Warnings) != 1 {
		t.Errorf("the loose mode should be named in a warning: %v", cfg.Report.Warnings)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("ReadConfig changed the mode to %o", info.Mode().Perm())
	}
	// CheckConfig, by contrast, tightens it as the daemon does.
	CheckConfig(path)
	info, _ = os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("CheckConfig no longer tightens the file (mode %o)", info.Mode().Perm())
	}
}

func TestProbeOriginsOnlyProbesAnAcceptedConfig(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{
  "b": {"base_url": "https://b.example.test/"},
  "a": {"base_url": "https://a.example.test:8443/", "auth": "bearer", "token": "tok-0123456789abcdef"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &fakeProber{}
	out := ReadConfig(good).ProbeOrigins(context.Background(), p)
	if len(out) != 2 || out[0].Name != "a" || out[1].Name != "b" {
		t.Fatalf("outcomes = %+v", out)
	}
	if out[0].HostKey != "https://a.example.test:8443" || !out[0].HasCredential || out[1].HasCredential {
		t.Errorf("outcomes = %+v", out)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"a": {"base_url": "https://a.example.test/"}, "b": {"base_url": "ftp://b.example.test/"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p = &fakeProber{}
	if out := ReadConfig(bad).ProbeOrigins(context.Background(), p); out != nil || len(p.seen) != 0 {
		t.Errorf("a refused configuration was probed: %+v, %d probes", out, len(p.seen))
	}
	p = &fakeProber{}
	if out := ReadConfig(filepath.Join(dir, "absent.json")).ProbeOrigins(context.Background(), p); out != nil || len(p.seen) != 0 {
		t.Errorf("a missing configuration was probed")
	}
}

func TestCanCreateDir(t *testing.T) {
	root := t.TempDir()
	if !CanCreateDir(filepath.Join(root, "a", "b", "c")) {
		t.Error("a missing path under a writable directory can be created")
	}
	if _, err := os.Stat(filepath.Join(root, "a")); err == nil {
		t.Error("CanCreateDir created something")
	}
	f := filepath.Join(root, "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if CanCreateDir(filepath.Join(f, "sub")) {
		t.Error("nothing can be created under a file")
	}
	if os.Geteuid() != 0 {
		ro := filepath.Join(root, "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		if CanCreateDir(filepath.Join(ro, "x")) {
			t.Error("nothing can be created under a read-only directory")
		}
	}
}

func TestReadConfigLooseModeIsAFailure(t *testing.T) {
	body := `{"a": {"base_url": "https://a.example.test/", "auth": "bearer", "token": "tok-0123456789abcdef"}}`
	for _, tt := range []struct {
		name     string
		mode     os.FileMode
		wantFail bool
	}{
		{"owner only", 0o600, false},
		{"group read, as a mounted Secret has", 0o640, false},
		{"owner read only", 0o400, false},
		{"world readable", 0o644, true},
		{"group writable", 0o660, true},
		{"world writable", 0o666, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, body)
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}
			cfg := ReadConfig(path)
			p := &fakeProber{}
			out := cfg.ProbeOrigins(context.Background(), p)
			if tt.wantFail {
				if len(cfg.Report.Issues) != 1 || cfg.Report.Issues[0].Class != IssueFile {
					t.Fatalf("issues = %+v, want one file issue", cfg.Report.Issues)
				}
				if out != nil || len(p.seen) != 0 {
					t.Error("a loose file must not be probed")
				}
			} else {
				if len(cfg.Report.Issues) != 0 || len(out) != 1 {
					t.Errorf("issues=%v outcomes=%v, want an accepted config that is probed", cfg.Report.Issues, out)
				}
			}
			// Whatever the verdict, the file is exactly as it was.
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != tt.mode {
				t.Errorf("the file's mode changed to %o (%v), want %o", info.Mode().Perm(), err, tt.mode)
			}
		})
	}
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
