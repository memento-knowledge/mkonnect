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
		strings.NewReader("bearer-token-value"), &stdout); err != nil {
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
	if strings.Contains(out, "bearer-token-value") {
		t.Fatalf("token must be masked in list output:\n%s", out)
	}
}

func TestConfigSetAndRemovePrintReloadReminder(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDS_FILE", filepath.Join(dir, "credentials.json"))

	// assertReminder checks the reload hint is present without leaking the token.
	assertReminder := func(t *testing.T, out, verb, token string) {
		t.Helper()
		if !strings.Contains(out, verb) {
			t.Fatalf("expected %q in output:\n%s", verb, out)
		}
		for _, want := range []string{"reload", "docker kill -s HUP", "kubectl rollout restart", "docs/setup.md"} {
			if !strings.Contains(out, want) {
				t.Fatalf("expected reload reminder to contain %q:\n%s", want, out)
			}
		}
		if token != "" && strings.Contains(out, token) {
			t.Fatalf("reminder output must not contain the token:\n%s", out)
		}
	}

	const token = "reminder-token-1234" // >= 16 chars
	var stdout bytes.Buffer
	if err := cli.Run([]string{"config", "set", "jenkins",
		"--base-url", "http://jenkins:8080",
		"--auth", "bearer", "--token-stdin"},
		strings.NewReader(token), &stdout); err != nil {
		t.Fatalf("config set: %v", err)
	}
	assertReminder(t, stdout.String(), "saved", token)

	stdout.Reset()
	if err := cli.Run([]string{"config", "remove", "jenkins"}, noStdin(), &stdout); err != nil {
		t.Fatalf("config remove: %v", err)
	}
	assertReminder(t, stdout.String(), "removed", "")
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
		strings.NewReader("s3cr3t-token-value\n"), &stdout); err != nil {
		t.Fatalf("config set: %v", err)
	}
	if strings.Contains(stdout.String(), "s3cr3t-token-value") {
		t.Errorf("token from stdin must not appear in output:\n%s", stdout.String())
	}
	// The trailing newline must be trimmed, and the token stored verbatim.
	b, err := os.ReadFile(credsPath)
	if err != nil {
		t.Fatalf("read creds file: %v", err)
	}
	if !strings.Contains(string(b), `"token": "s3cr3t-token-value"`) {
		t.Errorf("token from stdin not stored (trimmed) as expected:\n%s", b)
	}
	if strings.Contains(string(b), `s3cr3t-token-value\n`) {
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

func TestConfigSetRejectsShortTokenWithoutLeakingIt(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "bearer",
			args: []string{"config", "set", "service", "--base-url", "https://service.internal", "--auth", "bearer", "--token-stdin"},
		},
		{
			name: "basic",
			args: []string{"config", "set", "service", "--base-url", "https://service.internal", "--auth", "basic", "--username", "local-user", "--token-stdin"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			credsPath := filepath.Join(t.TempDir(), "credentials.json")
			t.Setenv("CREDS_FILE", credsPath)
			const token = "short-token"

			var stdout bytes.Buffer
			err := cli.Run(tt.args, strings.NewReader(token), &stdout)
			if err == nil || !strings.Contains(err.Error(), "at least 16") {
				t.Fatalf("expected minimum-token-length error, got %v", err)
			}
			if strings.Contains(err.Error(), token) || strings.Contains(stdout.String(), token) {
				t.Fatal("short token must not be repeated in an error or output")
			}
			if _, err := os.Stat(credsPath); !os.IsNotExist(err) {
				t.Fatalf("credential file was created after rejected token: %v", err)
			}
		})
	}
}

func TestConfigSetCountsTokenCharactersRatherThanBytes(t *testing.T) {
	credsPath := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("CREDS_FILE", credsPath)
	shortToken := strings.Repeat("界", 15)

	var stdout bytes.Buffer
	err := cli.Run([]string{"config", "set", "service",
		"--base-url", "https://service.internal", "--auth", "bearer", "--token-stdin"},
		strings.NewReader(shortToken), &stdout)
	if err == nil || !strings.Contains(err.Error(), "at least 16") {
		t.Fatalf("expected minimum-token-length error, got %v", err)
	}
	if strings.Contains(err.Error(), shortToken) || strings.Contains(stdout.String(), shortToken) {
		t.Fatal("short token must not be repeated in an error or output")
	}
	if _, err := os.Stat(credsPath); !os.IsNotExist(err) {
		t.Fatalf("credential file was created after rejected token: %v", err)
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
		strings.NewReader("api-token-value-123\n"), &stdout); err != nil {
		t.Fatalf("config set basic: %v", err)
	}
	if strings.Contains(stdout.String(), "local-user") || strings.Contains(stdout.String(), "api-token-value-123") {
		t.Fatalf("credentials must not appear in output:\n%s", stdout.String())
	}

	data, err := os.ReadFile(credsPath)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if !strings.Contains(string(data), `"username": "local-user"`) || !strings.Contains(string(data), `"token": "api-token-value-123"`) {
		t.Fatalf("basic credentials were not stored locally:\n%s", data)
	}

	stdout.Reset()
	if err := cli.Run([]string{"config", "list"}, noStdin(), &stdout); err != nil {
		t.Fatalf("config list: %v", err)
	}
	if !strings.Contains(stdout.String(), "basic ***") {
		t.Fatalf("expected masked basic auth in list output:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "local-user") || strings.Contains(stdout.String(), "api-token-value-123") {
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
