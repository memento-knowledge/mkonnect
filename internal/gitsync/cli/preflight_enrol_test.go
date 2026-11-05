package cli_test

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/gitsync/cli"
)

// readyEnv is basicsEnv with an (empty) provider configuration, so that nothing but the check under
// test can fail the run.
func readyEnv(t *testing.T) {
	t.Helper()
	basicsEnv(t)
	writeProviders(t, os.Getenv("GIT_SYNC_CONFIG_FILE"), "{}")
}

// notRoot skips a test that relies on file permissions being enforced, which root bypasses.
func notRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("file permissions are not enforced for root")
	}
}

// enrolledKey saves a real key at the path KEY_FILE names, as registration would.
func enrolledKey(t *testing.T) string {
	t.Helper()
	path := os.Getenv("KEY_FILE")
	_, priv, err := mldsa65.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.NewKeyStore(path).Save(priv); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTokenFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registration-token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// secretsAbsent fails if any synthetic secret, or the path a token was read from, is in the output.
func secretsAbsent(t *testing.T, out string, extra ...string) {
	t.Helper()
	for _, s := range append([]string{testRegToken, "env-registration-token-9876543210"}, extra...) {
		if strings.Contains(out, s) {
			t.Errorf("the output contains %q", s)
		}
	}
}

func TestPreflightConnectorID(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		readyEnv(t)
		doc, out, err := runPreflightJSON(t)
		wantStatus(t, doc, "connector-id", "pass")
		if err != nil {
			t.Errorf("unexpected failure: %v", err)
		}
		if strings.Contains(out, testConnectorID) {
			t.Error("the connector ID value must not be printed")
		}
	})
	t.Run("unset", func(t *testing.T) {
		basicsEnv(t)
		t.Setenv("CONNECTOR_ID", "")
		doc, _, err := runPreflightJSON(t)
		c := wantStatus(t, doc, "connector-id", "fail")
		if err == nil || c.Remedy == nil {
			t.Error("expected a failure with a remedy and a non-zero exit")
		}
	})
}

func TestPreflightRegistrationTokenFile(t *testing.T) {
	notRoot(t)
	tests := []struct {
		name   string
		file   func(t *testing.T) string
		status string
	}{
		{"owner only", func(t *testing.T) string { return writeTokenFile(t, testRegToken, 0o600) }, "pass"},
		{"group read", func(t *testing.T) string { return writeTokenFile(t, testRegToken, 0o640) }, "pass"},
		{"kubernetes secret mode", func(t *testing.T) string { return writeTokenFile(t, testRegToken, 0o440) }, "pass"},
		{"no trailing newline", func(t *testing.T) string { return writeTokenFile(t, testRegToken, 0o600) }, "pass"},
		{"world readable", func(t *testing.T) string { return writeTokenFile(t, testRegToken, 0o644) }, "fail"},
		{"group writable", func(t *testing.T) string { return writeTokenFile(t, testRegToken, 0o660) }, "fail"},
		{"world writable", func(t *testing.T) string { return writeTokenFile(t, testRegToken, 0o666) }, "fail"},
		{"other read only", func(t *testing.T) string { return writeTokenFile(t, testRegToken, 0o604) }, "fail"},
		{"empty", func(t *testing.T) string { return writeTokenFile(t, "", 0o600) }, "fail"},
		{"only a newline", func(t *testing.T) string { return writeTokenFile(t, "\n", 0o600) }, "fail"},
		{"too large", func(t *testing.T) string { return writeTokenFile(t, strings.Repeat("x", 5000), 0o600) }, "fail"},
		{"unreadable", func(t *testing.T) string { return writeTokenFile(t, testRegToken, 0o000) }, "fail"},
		{"missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }, "fail"},
		{"a directory", func(t *testing.T) string { return t.TempDir() }, "fail"},
		{"relative path", func(t *testing.T) string { return "relative-token-file" }, "fail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			basicsEnv(t)
			path := tt.file(t)
			t.Setenv("REGISTRATION_TOKEN_FILE", path)
			doc, out, err := runPreflightJSON(t)
			c := wantStatus(t, doc, "registration-token", tt.status)
			if tt.status == "fail" && (err == nil || c.Remedy == nil) {
				t.Error("a failure must have a remedy and a non-zero exit")
			}
			secretsAbsent(t, out, path)
		})
	}
}

func TestPreflightRegistrationTokenSources(t *testing.T) {
	const envToken = "env-registration-token-9876543210"
	t.Run("none and not enrolled", func(t *testing.T) {
		basicsEnv(t)
		t.Setenv("REGISTRATION_TOKEN_FILE", "")
		doc, _, err := runPreflightJSON(t)
		c := wantStatus(t, doc, "registration-token", "fail")
		if err == nil || c.Remedy == nil {
			t.Error("a connector that cannot register must fail preflight")
		}
	})
	t.Run("environment variable only warns", func(t *testing.T) {
		readyEnv(t)
		t.Setenv("REGISTRATION_TOKEN_FILE", "")
		t.Setenv("REGISTRATION_TOKEN", envToken)
		doc, out, err := runPreflightJSON(t)
		c := wantStatus(t, doc, "registration-token", "warn")
		if err != nil || c.Remedy == nil {
			t.Errorf("a warning needs a remedy and must not fail the run: %v", err)
		}
		secretsAbsent(t, out)
	})
	t.Run("both set: the variable is ignored", func(t *testing.T) {
		basicsEnv(t)
		t.Setenv("REGISTRATION_TOKEN", envToken)
		doc, out, _ := runPreflightJSON(t)
		c := wantStatus(t, doc, "registration-token", "pass")
		if !strings.Contains(c.Detail, "ignored") {
			t.Errorf("detail should say the variable is ignored: %q", c.Detail)
		}
		secretsAbsent(t, out)
	})
	t.Run("a bad file is not rescued by the variable", func(t *testing.T) {
		basicsEnv(t)
		t.Setenv("REGISTRATION_TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))
		t.Setenv("REGISTRATION_TOKEN", envToken)
		doc, out, err := runPreflightJSON(t)
		wantStatus(t, doc, "registration-token", "fail")
		if err == nil {
			t.Error("expected a failure")
		}
		secretsAbsent(t, out)
	})
}

func TestPreflightEnrolledNeedsNoToken(t *testing.T) {
	t.Run("no token configured", func(t *testing.T) {
		readyEnv(t)
		enrolledKey(t)
		t.Setenv("REGISTRATION_TOKEN_FILE", "")
		doc, _, err := runPreflightJSON(t)
		wantStatus(t, doc, "key-file", "pass")
		wantStatus(t, doc, "registration-token", "pass")
		if err != nil || !doc.Ok {
			t.Errorf("an enrolled connector without a token is ready: %v", err)
		}
	})
	t.Run("a spent token still configured warns", func(t *testing.T) {
		readyEnv(t)
		enrolledKey(t)
		doc, out, err := runPreflightJSON(t)
		c := wantStatus(t, doc, "registration-token", "warn")
		if err != nil || c.Remedy == nil {
			t.Errorf("expected a warning with a remedy, no failure: %v", err)
		}
		secretsAbsent(t, out)
	})
	t.Run("an unusable token file still stops the daemon", func(t *testing.T) {
		notRoot(t)
		basicsEnv(t)
		enrolledKey(t)
		t.Setenv("REGISTRATION_TOKEN_FILE", writeTokenFile(t, testRegToken, 0o644))
		doc, _, err := runPreflightJSON(t)
		wantStatus(t, doc, "registration-token", "fail")
		if err == nil {
			t.Error("the daemon refuses to start with an unsafe token file even when enrolled")
		}
	})
}

func TestPreflightKeyFilePresent(t *testing.T) {
	notRoot(t)
	t.Run("owner only", func(t *testing.T) {
		readyEnv(t)
		path := enrolledKey(t)
		doc, out, err := runPreflightJSON(t)
		wantStatus(t, doc, "key-file", "pass")
		if err != nil {
			t.Errorf("unexpected failure: %v", err)
		}
		_ = path
		if strings.Contains(out, "PRIVATE") {
			t.Error("key material in the output")
		}
	})
	for _, mode := range []os.FileMode{0o640, 0o644, 0o660, 0o604, 0o666, 0o604} {
		t.Run(fmt.Sprintf("mode %04o fails closed and is left alone", mode), func(t *testing.T) {
			basicsEnv(t)
			path := enrolledKey(t)
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			doc, _, err := runPreflightJSON(t)
			c := wantStatus(t, doc, "key-file", "fail")
			if err == nil || c.Remedy == nil || !strings.Contains(*c.Remedy, "do not delete") {
				t.Errorf("want a failure whose remedy says not to delete the key, got err=%v remedy=%v", err, c.Remedy)
			}
			// Treated as enrolled, so the registration token is judged as for an enrolled connector.
			wantStatus(t, doc, "registration-token", "warn")
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Errorf("preflight must not change the key file's mode: %v %v", info.Mode().Perm(), err)
			}
		})
	}
	t.Run("a symbolic link is not accepted", func(t *testing.T) {
		basicsEnv(t)
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, os.Getenv("KEY_FILE")); err != nil {
			t.Fatal(err)
		}
		doc, _, err := runPreflightJSON(t)
		wantStatus(t, doc, "key-file", "fail")
		wantStatus(t, doc, "registration-token", "skip")
		if err == nil {
			t.Error("expected a failure")
		}
	})
	t.Run("a damaged key leaves enrolment unknown", func(t *testing.T) {
		basicsEnv(t)
		if err := os.WriteFile(os.Getenv("KEY_FILE"), []byte("not a key"), 0o600); err != nil {
			t.Fatal(err)
		}
		doc, _, err := runPreflightJSON(t)
		c := wantStatus(t, doc, "key-file", "fail")
		wantStatus(t, doc, "registration-token", "skip")
		if err == nil || c.Remedy == nil || !strings.Contains(*c.Remedy, "Do not delete") {
			t.Errorf("want a failure whose remedy says not to delete the key: %v %v", err, c.Remedy)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		basicsEnv(t)
		path := enrolledKey(t)
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
		doc, _, _ := runPreflightJSON(t)
		wantStatus(t, doc, "key-file", "fail")
		wantStatus(t, doc, "registration-token", "skip")
	})
}

func TestPreflightKeyDirectory(t *testing.T) {
	t.Run("existing and writable", func(t *testing.T) {
		readyEnv(t)
		dir := filepath.Dir(os.Getenv("KEY_FILE"))
		doc, _, err := runPreflightJSON(t)
		c := wantStatus(t, doc, "key-file", "pass")
		if err != nil || c.Remedy != nil {
			t.Errorf("unexpected: %v %v", err, c.Remedy)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("the probe left %d entries behind", len(entries))
		}
	})
	t.Run("missing but creatable warns and is not created", func(t *testing.T) {
		readyEnv(t)
		dir := filepath.Join(t.TempDir(), "not-yet")
		t.Setenv("KEY_FILE", filepath.Join(dir, "gitsync-key"))
		doc, _, err := runPreflightJSON(t)
		c := wantStatus(t, doc, "key-file", "warn")
		if err != nil || c.Remedy == nil {
			t.Errorf("a warning needs a remedy and must not fail the run: %v", err)
		}
		if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
			t.Error("preflight must not create the key directory")
		}
	})
	t.Run("not writable", func(t *testing.T) {
		notRoot(t)
		basicsEnv(t)
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		t.Setenv("KEY_FILE", filepath.Join(dir, "gitsync-key"))
		doc, _, err := runPreflightJSON(t)
		c := wantStatus(t, doc, "key-file", "fail")
		if err == nil || c.Remedy == nil {
			t.Error("expected a failure with a remedy")
		}
	})
	t.Run("missing and not creatable", func(t *testing.T) {
		notRoot(t)
		basicsEnv(t)
		parent := t.TempDir()
		if err := os.Chmod(parent, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
		dir := filepath.Join(parent, "keys")
		t.Setenv("KEY_FILE", filepath.Join(dir, "gitsync-key"))
		doc, _, err := runPreflightJSON(t)
		wantStatus(t, doc, "key-file", "fail")
		if err == nil {
			t.Error("expected a failure")
		}
		if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
			t.Error("preflight must not create the key directory")
		}
	})
	t.Run("the directory is a file", func(t *testing.T) {
		basicsEnv(t)
		notADir := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(notADir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KEY_FILE", filepath.Join(notADir, "gitsync-key"))
		doc, _, _ := runPreflightJSON(t)
		wantStatus(t, doc, "key-file", "fail")
	})
	t.Run("the default path is the daemon's", func(t *testing.T) {
		basicsEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("KEY_FILE", "")
		if err := os.MkdirAll(filepath.Join(home, ".mkonnect"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, priv, err := mldsa65.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := auth.NewKeyStore(filepath.Join(home, ".mkonnect", "gitsync-key")).Save(priv); err != nil {
			t.Fatal(err)
		}
		doc, _, _ := runPreflightJSON(t)
		c := wantStatus(t, doc, "key-file", "pass")
		if !strings.Contains(c.Detail, "already enrolled") {
			t.Errorf("the default key was not found: %q", c.Detail)
		}
	})
	t.Run("no key path and no home", func(t *testing.T) {
		basicsEnv(t)
		t.Setenv("KEY_FILE", "")
		t.Setenv("HOME", "")
		doc, _, err := runPreflightJSON(t)
		wantStatus(t, doc, "key-file", "fail")
		wantStatus(t, doc, "registration-token", "skip")
		if err == nil {
			t.Error("expected a failure")
		}
	})
}

func TestKeyProbeVerdict(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status string
		want   string
	}{
		{"writable", nil, "pass", "has room"},
		{"no space", fmt.Errorf("write: %w", syscall.ENOSPC), "fail", "no free space"},
		{"quota", fmt.Errorf("write: %w", syscall.EDQUOT), "fail", "no free space"},
		{"read-only file system", fmt.Errorf("create: %w", syscall.EROFS), "fail", "read-only"},
		{"permission", fmt.Errorf("create: %w", syscall.EACCES), "fail", "not writable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, detail, remedy := cli.KeyProbeVerdictForTest(tt.err, "/data")
			if status != tt.status || !strings.Contains(detail, tt.want) {
				t.Errorf("got %s %q", status, detail)
			}
			if (status == "fail") != (remedy != "") {
				t.Errorf("a failure has a remedy and a pass has none: %q", remedy)
			}
		})
	}
}
