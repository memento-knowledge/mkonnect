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
		"--token-stdin"},
		strings.NewReader("secret"), &stdout); err != nil {
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

func TestConfigSetRejectsInlineTokenWithoutLeakingIt(t *testing.T) {
	t.Setenv("CREDS_FILE", filepath.Join(t.TempDir(), "credentials.json"))
	var stdout bytes.Buffer
	err := cli.Run([]string{"config", "set", "jenkins",
		"--base-url", "http://j:80", "--auth", "bearer", "--token", "inline-secret"},
		noStdin(), &stdout)
	if err == nil {
		t.Fatal("expected inline token to be rejected")
	}
	if strings.Contains(err.Error(), "inline-secret") || strings.Contains(stdout.String(), "inline-secret") {
		t.Fatal("inline token must not be repeated in an error or output")
	}
}

func TestConfigSetRejectsCredentialBearingBaseURLWithoutLeakingIt(t *testing.T) {
	const secret = "api-token"
	urls := []string{
		"https://local-user:" + secret + "@service.internal",
		"https://service.internal/?access_token=" + secret,
		"https://service.internal/#access_token=" + secret,
	}

	for _, baseURL := range urls {
		t.Run(baseURL, func(t *testing.T) {
			credsPath := filepath.Join(t.TempDir(), "credentials.json")
			t.Setenv("CREDS_FILE", credsPath)

			var stdout bytes.Buffer
			err := cli.Run([]string{"config", "set", "service", "--base-url", baseURL}, noStdin(), &stdout)
			if err == nil {
				t.Fatal("expected credential-bearing URL to be rejected")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(stdout.String(), secret) {
				t.Fatal("URL credential must not be repeated in an error or output")
			}
			if _, err := os.Stat(credsPath); !os.IsNotExist(err) {
				t.Fatalf("credential file was created after rejected URL: %v", err)
			}
		})
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
	if strings.Contains(stdout.String(), "s3cr3t") {
		t.Errorf("token from stdin must not appear in output:\n%s", stdout.String())
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

func TestConfigSetBasicTokenFromStdinAndMasksCredentials(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "credentials.json")
	t.Setenv("CREDS_FILE", credsPath)

	var stdout bytes.Buffer
	if err := cli.Run([]string{"config", "set", "build-service",
		"--base-url", "https://build.internal",
		"--auth", "basic", "--username", "local-user", "--token-stdin"},
		strings.NewReader("api-token\n"), &stdout); err != nil {
		t.Fatalf("config set basic: %v", err)
	}
	if strings.Contains(stdout.String(), "local-user") || strings.Contains(stdout.String(), "api-token") {
		t.Fatalf("credentials must not appear in output:\n%s", stdout.String())
	}

	data, err := os.ReadFile(credsPath)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if !strings.Contains(string(data), `"username": "local-user"`) || !strings.Contains(string(data), `"token": "api-token"`) {
		t.Fatalf("basic credentials were not stored locally:\n%s", data)
	}

	stdout.Reset()
	if err := cli.Run([]string{"config", "list"}, noStdin(), &stdout); err != nil {
		t.Fatalf("config list: %v", err)
	}
	if !strings.Contains(stdout.String(), "basic ***") {
		t.Fatalf("expected masked basic auth in list output:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "local-user") || strings.Contains(stdout.String(), "api-token") {
		t.Fatalf("credentials must be masked in list output:\n%s", stdout.String())
	}
}

func TestConfigSetBasicRequiresUsername(t *testing.T) {
	t.Setenv("CREDS_FILE", filepath.Join(t.TempDir(), "credentials.json"))
	var stdout bytes.Buffer
	err := cli.Run([]string{"config", "set", "build-service",
		"--base-url", "https://build.internal", "--auth", "basic", "--token-stdin"},
		strings.NewReader("api-token"), &stdout)
	if err == nil || !strings.Contains(err.Error(), "--username") {
		t.Fatalf("expected a --username validation error, got %v", err)
	}
}

func TestConfigSetRejectsTokenStdinWithoutAuth(t *testing.T) {
	t.Setenv("CREDS_FILE", filepath.Join(t.TempDir(), "credentials.json"))
	var stdout bytes.Buffer
	err := cli.Run([]string{"config", "set", "service",
		"--base-url", "https://service.internal", "--token-stdin"},
		strings.NewReader("api-token"), &stdout)
	if err == nil || !strings.Contains(err.Error(), "--auth") {
		t.Fatalf("expected an auth validation error, got %v", err)
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
