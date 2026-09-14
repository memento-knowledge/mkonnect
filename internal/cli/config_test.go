// internal/cli/config_test.go
package cli_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/cli"
)

func TestConfigSetAndList(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "credentials.json")
	t.Setenv("CREDS_FILE", credsPath)

	var stdout bytes.Buffer
	if err := cli.Run([]string{"config", "set", "jenkins",
		"--base-url", "http://jenkins:8080",
		"--auth", "bearer",
		"--token", "secret"},
		&stdout); err != nil {
		t.Fatalf("config set: %v", err)
	}

	stdout.Reset()
	if err := cli.Run([]string{"config", "list"}, &stdout); err != nil {
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
		"--base-url", "http://prom:9090"}, &stdout); err != nil {
		t.Fatalf("config set: %v", err)
	}
	if err := cli.Run([]string{"config", "list"}, &stdout); err != nil {
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
	if err := cli.Run([]string{"config", "set", "jenkins", "--base-url", "http://j:80"}, &stdout); err != nil {
		t.Fatalf("config set: %v", err)
	}
	if err := cli.Run([]string{"config", "remove", "jenkins"}, &stdout); err != nil {
		t.Fatalf("config remove: %v", err)
	}
	stdout.Reset()
	if err := cli.Run([]string{"config", "list"}, &stdout); err != nil {
		t.Fatalf("config list: %v", err)
	}
	if strings.Contains(stdout.String(), "jenkins") {
		t.Fatal("expected jenkins to be removed")
	}
}

func TestConfigSetMissingBaseURL(t *testing.T) {
	t.Setenv("CREDS_FILE", filepath.Join(t.TempDir(), "credentials.json"))
	var stdout bytes.Buffer
	err := cli.Run([]string{"config", "set", "jenkins"}, &stdout)
	if err == nil {
		t.Fatal("expected error when --base-url is missing")
	}
}

func TestConfigSetBearerRequiresToken(t *testing.T) {
	t.Setenv("CREDS_FILE", filepath.Join(t.TempDir(), "credentials.json"))
	var stdout bytes.Buffer
	err := cli.Run([]string{"config", "set", "jenkins",
		"--base-url", "http://j:80", "--auth", "bearer"}, &stdout)
	if err == nil {
		t.Fatal("expected error when --auth bearer given without --token")
	}
}

func TestStatusShowsConfiguredPlugins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDS_FILE", filepath.Join(dir, "credentials.json"))

	var stdout bytes.Buffer
	if err := cli.Run([]string{"config", "set", "jenkins", "--base-url", "http://j:80"}, &stdout); err != nil {
		t.Fatalf("config set: %v", err)
	}
	stdout.Reset()
	if err := cli.Run([]string{"status"}, &stdout); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(stdout.String(), "jenkins") {
		t.Fatalf("expected jenkins in status output:\n%s", stdout.String())
	}
}
