package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/config"
)

// tokenValue is a synthetic secret used to assert that it is never echoed in errors.
const tokenValue = "synthetic-registration-token-0123456789"

// baseEnv sets the minimum valid environment so Load fails only on token handling.
func baseEnv(t *testing.T) {
	t.Helper()
	clearEnv(t)
	t.Setenv("GATEWAY_URL", "wss://gateway.example.com/ws")
	t.Setenv("CONNECTOR_ID", "c1")
}

// writeTokenFile writes content with the given mode, chmod-ing explicitly so the
// process umask cannot affect the result.
func writeTokenFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registration.token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadRegistrationTokenUnsetIsUnchanged pins the behaviour both binaries relied
// on before REGISTRATION_TOKEN_FILE existed: with the file variable unset, the token
// comes from REGISTRATION_TOKEN verbatim, and an absent token stays empty.
func TestLoadRegistrationTokenUnsetIsUnchanged(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string
	}{
		{"env token passes through", "tok", "tok"},
		{"env token is not trimmed", "tok\n", "tok\n"},
		{"absent token stays empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("REGISTRATION_TOKEN", tt.env)
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.RegistrationToken != tt.want {
				t.Errorf("RegistrationToken = %q, want %q", cfg.RegistrationToken, tt.want)
			}
		})
	}
}

func TestLoadRegistrationTokenFile(t *testing.T) {
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		want    string
	}{
		{"plain", tokenValue, 0o600, tokenValue},
		{"owner read only", tokenValue, 0o400, tokenValue},
		{"trailing LF trimmed", tokenValue + "\n", 0o600, tokenValue},
		{"trailing CRLF trimmed", tokenValue + "\r\n", 0o600, tokenValue},
		{"lone trailing CR kept", tokenValue + "\r", 0o600, tokenValue + "\r"},
		{"only one trailing newline trimmed", tokenValue + "\n\n", 0o600, tokenValue + "\n"},
		{"group readable allowed", tokenValue, 0o640, tokenValue},
		{"group read-only allowed", tokenValue, 0o440, tokenValue},
		{"exactly at size limit", strings.Repeat("a", 4096), 0o600, strings.Repeat("a", 4096)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("REGISTRATION_TOKEN_FILE", writeTokenFile(t, tt.content, tt.mode))
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.RegistrationToken != tt.want {
				t.Errorf("RegistrationToken = %q, want %q", cfg.RegistrationToken, tt.want)
			}
		})
	}
}

func TestLoadRegistrationTokenFileRejected(t *testing.T) {
	dir := t.TempDir()
	symlinkTo := func(t *testing.T, target string) string {
		t.Helper()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		return link
	}
	tests := []struct {
		name    string
		path    func(t *testing.T) string
		wantMsg string
	}{
		{"empty file", func(t *testing.T) string { return writeTokenFile(t, "", 0o600) }, "is empty"},
		{"only a newline", func(t *testing.T) string { return writeTokenFile(t, "\n", 0o600) }, "is empty"},
		{"only a CRLF", func(t *testing.T) string { return writeTokenFile(t, "\r\n", 0o600) }, "is empty"},
		{"too large", func(t *testing.T) string { return writeTokenFile(t, strings.Repeat("a", 4097), 0o600) }, "larger than 4096 bytes"},
		{"relative path", func(t *testing.T) string {
			// A valid file exists at the relative path, so only the absolute-path
			// check can reject it.
			cwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(cwd, "registration.token"), []byte(tokenValue), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(cwd)
			return "registration.token"
		}, "must be an absolute path"},
		{"missing file", func(t *testing.T) string { return filepath.Join(dir, "does-not-exist") }, "no such file"},
		{"world readable", func(t *testing.T) string { return writeTokenFile(t, tokenValue, 0o644) }, "unsafe permissions"},
		{"world writable", func(t *testing.T) string { return writeTokenFile(t, tokenValue, 0o662) }, "unsafe permissions"},
		{"group writable", func(t *testing.T) string { return writeTokenFile(t, tokenValue, 0o620) }, "unsafe permissions"},
		{"directory", func(t *testing.T) string { return dir }, "not a regular file"},
		{"symlink to directory", func(t *testing.T) string { return symlinkTo(t, dir) }, "not a regular file"},
		{"symlink to world readable file", func(t *testing.T) string {
			return symlinkTo(t, writeTokenFile(t, tokenValue, 0o644))
		}, "unsafe permissions"},
		{"fifo", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "registration.fifo")
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Skipf("fifos unavailable: %v", err)
			}
			return path
		}, "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("REGISTRATION_TOKEN_FILE", tt.path(t))
			// A valid env token must not mask the failure: fail closed.
			t.Setenv("REGISTRATION_TOKEN", "env-token")

			// Load runs in a goroutine so that a regression that blocks (for example
			// opening a FIFO with no writer) fails the test instead of hanging it.
			type result struct {
				cfg *config.Config
				err error
			}
			done := make(chan result, 1)
			go func() {
				cfg, err := config.Load()
				done <- result{cfg, err}
			}()
			var res result
			select {
			case res = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Load did not return within 5s")
			}

			if res.err == nil {
				t.Fatalf("expected an error, got RegistrationToken %q", res.cfg.RegistrationToken)
			}
			msg := res.err.Error()
			if !strings.Contains(msg, "REGISTRATION_TOKEN_FILE") {
				t.Errorf("error %q should name REGISTRATION_TOKEN_FILE", msg)
			}
			if !strings.Contains(msg, tt.wantMsg) {
				t.Errorf("error %q should contain %q", msg, tt.wantMsg)
			}
			for _, secret := range []string{tokenValue, "env-token"} {
				if strings.Contains(msg, secret) {
					t.Errorf("error leaks a token value: %q", msg)
				}
			}
		})
	}
}

// A Kubernetes Secret mount exposes each key as a symlink into a ..data directory;
// the token must be readable through such a link.
func TestLoadRegistrationTokenFileThroughSymlink(t *testing.T) {
	baseEnv(t)
	target := writeTokenFile(t, tokenValue+"\n", 0o640)
	link := filepath.Join(t.TempDir(), "registration-token")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("REGISTRATION_TOKEN_FILE", link)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RegistrationToken != tokenValue {
		t.Errorf("RegistrationToken = %q, want %q", cfg.RegistrationToken, tokenValue)
	}
}

func TestLoadRegistrationTokenFileWinsOverEnv(t *testing.T) {
	baseEnv(t)
	t.Setenv("REGISTRATION_TOKEN", "env-token")
	t.Setenv("REGISTRATION_TOKEN_FILE", writeTokenFile(t, tokenValue, 0o600))
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RegistrationToken != tokenValue {
		t.Errorf("RegistrationToken = %q, want the file's value", cfg.RegistrationToken)
	}
}

// An empty variable counts as unset, matching how every other variable is read.
func TestLoadRegistrationTokenFileEmptyVariableIsUnset(t *testing.T) {
	baseEnv(t)
	t.Setenv("REGISTRATION_TOKEN", "env-token")
	t.Setenv("REGISTRATION_TOKEN_FILE", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RegistrationToken != "env-token" {
		t.Errorf("RegistrationToken = %q, want the env value", cfg.RegistrationToken)
	}
}

func TestCheckRegistrationTokenReportsSourceWithoutValue(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		t.Setenv("REGISTRATION_TOKEN_FILE", "")
		t.Setenv("REGISTRATION_TOKEN", "")
		if src, err := config.CheckRegistrationToken(); err != nil || src != config.RegistrationTokenNone {
			t.Errorf("got %q, %v", src, err)
		}
	})
	t.Run("environment", func(t *testing.T) {
		t.Setenv("REGISTRATION_TOKEN_FILE", "")
		t.Setenv("REGISTRATION_TOKEN", "synthetic-token-value")
		if src, err := config.CheckRegistrationToken(); err != nil || src != config.RegistrationTokenEnv {
			t.Errorf("got %q, %v", src, err)
		}
	})
	t.Run("file wins over the environment", func(t *testing.T) {
		t.Setenv("REGISTRATION_TOKEN", "synthetic-env-value")
		t.Setenv("REGISTRATION_TOKEN_FILE", writeTokenFile(t, "synthetic-file-value", 0o600))
		if src, err := config.CheckRegistrationToken(); err != nil || src != config.RegistrationTokenFile {
			t.Errorf("got %q, %v", src, err)
		}
	})
	t.Run("an unusable file is an error even with the variable set", func(t *testing.T) {
		t.Setenv("REGISTRATION_TOKEN", "synthetic-env-value")
		t.Setenv("REGISTRATION_TOKEN_FILE", writeTokenFile(t, "synthetic-file-value", 0o644))
		if _, err := config.CheckRegistrationToken(); err == nil {
			t.Error("expected an error")
		}
	})
}
