// internal/cli/config_test.go
package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/cli"
)

// noStdin is the stdin reader for tests that don't use --token-stdin.
func noStdin() *strings.Reader { return strings.NewReader("") }

func TestConfigSetAndList(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "credentials.json")
	t.Setenv("CREDS_FILE", credsPath)

	var stdout bytes.Buffer
	if err := cli.Run([]string{"config", "set", "jenkins",
		"--base-url", "http://jenkins:8080",
		"--auth", "bearer",
		"--token", "secret"},
		noStdin(), &stdout); err != nil {
		t.Fatalf("config set: %v", err)
	}

	stdout.Reset()
	if err := cli.Run([]string{"config", "list"}, noStdin(), &stdout); err != nil {
		t.Fatalf("config list: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "jenkins") || !strings.Contains(out, "http://jenkins:8080") {
		t.Fatalf("expected jenkins in list output:\n%s", out)
	}
	// Token must be masked
	if strings.Contains(out, "secret") {
		t.Fatalf("token must be masked in list output:\n%s", out)
	}
}

func TestConfigSetNoAuth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDS_FILE", filepath.Join(dir, "credentials.json"))

	var stdout bytes.Buffer
	if err := cli.Run([]string{"config", "set", "prom",
		"--base-url", "http://prom:9090"}, noStdin(), &stdout); err != nil {
		t.Fatalf("config set: %v", err)
	}
	if err := cli.Run([]string{"config", "list"}, noStdin(), &stdout); err != nil {
		t.Fatalf("config list: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "prom") {
		t.Fatalf("expected prom in list output:\n%s", out)
	}
}

func TestConfigRemove(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDS_FILE", filepath.Join(dir, "credentials.json"))

	var stdout bytes.Buffer
	if err := cli.Run([]string{"config", "set", "jenkins", "--base-url", "http://j:80"}, noStdin(), &stdout); err != nil {
		t.Fatalf("config set: %v", err)
	}
	if err := cli.Run([]string{"config", "remove", "jenkins"}, noStdin(), &stdout); err != nil {
		t.Fatalf("config remove: %v", err)
	}
	stdout.Reset()
	if err := cli.Run([]string{"config", "list"}, noStdin(), &stdout); err != nil {
		t.Fatalf("config list: %v", err)
	}
	if strings.Contains(stdout.String(), "jenkins") {
		t.Fatal("expected jenkins to be removed")
	}
}

func TestConfigSetMissingBaseURL(t *testing.T) {
	t.Setenv("CREDS_FILE", filepath.Join(t.TempDir(), "credentials.json"))
	var stdout bytes.Buffer
	err := cli.Run([]string{"config", "set", "jenkins"}, noStdin(), &stdout)
	if err == nil {
		t.Fatal("expected error when --base-url is missing")
	}
}

func TestConfigSetBearerRequiresToken(t *testing.T) {
	t.Setenv("CREDS_FILE", filepath.Join(t.TempDir(), "credentials.json"))
	var stdout bytes.Buffer
	err := cli.Run([]string{"config", "set", "jenkins",
		"--base-url", "http://j:80", "--auth", "bearer"}, noStdin(), &stdout)
	if err == nil {
		t.Fatal("expected error when --auth bearer given without a token")
	}
}

func TestConfigSetInlineTokenWarns(t *testing.T) {
	t.Setenv("CREDS_FILE", filepath.Join(t.TempDir(), "credentials.json"))
	var stdout bytes.Buffer
	if err := cli.Run([]string{"config", "set", "jenkins",
		"--base-url", "http://j:80", "--auth", "bearer", "--token", "secret"},
		noStdin(), &stdout); err != nil {
		t.Fatalf("config set: %v", err)
	}
	if !strings.Contains(stdout.String(), "--token-stdin") {
		t.Errorf("expected a warning steering the user to --token-stdin, got:\n%s", stdout.String())
	}
}

func TestConfigSetTokenStdin(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "credentials.json")
	t.Setenv("CREDS_FILE", credsPath)

	var stdout bytes.Buffer
	if err := cli.Run([]string{"config", "set", "jenkins",
		"--base-url", "http://j:80", "--auth", "bearer", "--token-stdin"},
		strings.NewReader("s3cr3t\n"), &stdout); err != nil {
		t.Fatalf("config set: %v", err)
	}
	// The inline-token warning must NOT appear for the stdin path.
	if strings.Contains(stdout.String(), "process list") {
		t.Errorf("--token-stdin should not trigger the inline-token warning:\n%s", stdout.String())
	}
	// The trailing newline must be trimmed, and the token stored verbatim.
	b, err := os.ReadFile(credsPath)
	if err != nil {
		t.Fatalf("read creds file: %v", err)
	}
	if !strings.Contains(string(b), `"token": "s3cr3t"`) {
		t.Errorf("token from stdin not stored (trimmed) as expected:\n%s", b)
	}
	if strings.Contains(string(b), `s3cr3t\n`) {
		t.Errorf("trailing newline was not trimmed from the stdin token")
	}
}

func TestConfigSetTokenAndStdinConflict(t *testing.T) {
	t.Setenv("CREDS_FILE", filepath.Join(t.TempDir(), "credentials.json"))
	var stdout bytes.Buffer
	err := cli.Run([]string{"config", "set", "jenkins",
		"--base-url", "http://j:80", "--auth", "bearer",
		"--token", "x", "--token-stdin"},
		strings.NewReader("y"), &stdout)
	if err == nil {
		t.Fatal("expected error when both --token and --token-stdin are given")
	}
}

func TestConfigSetTokenStdinEmpty(t *testing.T) {
	t.Setenv("CREDS_FILE", filepath.Join(t.TempDir(), "credentials.json"))
	var stdout bytes.Buffer
	err := cli.Run([]string{"config", "set", "jenkins",
		"--base-url", "http://j:80", "--auth", "bearer", "--token-stdin"},
		strings.NewReader(""), &stdout)
	if err == nil {
		t.Fatal("expected error when --token-stdin is set but stdin is empty")
	}
}

func TestStatusShowsConfiguredPlugins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDS_FILE", filepath.Join(dir, "credentials.json"))

	var stdout bytes.Buffer
	if err := cli.Run([]string{"config", "set", "jenkins", "--base-url", "http://j:80"}, noStdin(), &stdout); err != nil {
		t.Fatalf("config set: %v", err)
	}
	stdout.Reset()
	if err := cli.Run([]string{"status"}, noStdin(), &stdout); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(stdout.String(), "jenkins") {
		t.Fatalf("expected jenkins in status output:\n%s", stdout.String())
	}
}
