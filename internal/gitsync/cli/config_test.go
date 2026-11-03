package cli_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/gitsync/cli"
)

// runCLI invokes the CLI with the given stdin and args against a per-test config file.
func runCLI(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := cli.Run(args, strings.NewReader(stdin), &out)
	return out.String(), err
}

// withConfig points GIT_SYNC_CONFIG_FILE at a fresh per-test path and returns it.
func withConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git-sync.json")
	t.Setenv("GIT_SYNC_CONFIG_FILE", path)
	return path
}

const bearerToken = "bearer-token-abcdef"

func TestConfigSetPersistsAndListMasks(t *testing.T) {
	path := withConfig(t)
	out, err := runCLI(t, bearerToken+"\n", "config", "set", "origin", "--base-url", "https://git.example.com/", "--auth", "bearer", "--token-stdin")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !strings.Contains(out, `Provider "origin" saved.`) || !strings.Contains(out, "restart it to apply") {
		t.Errorf("set output unexpected: %q", out)
	}

	// Persisted with the real token recoverable.
	store, err := gitsync.New(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if e, ok := store.Get("origin"); !ok || e.Token.Reveal() != bearerToken {
		t.Errorf("token did not persist: ok=%v token=%q", ok, e.Token.Reveal())
	}

	// list masks the token.
	listOut, err := runCLI(t, "", "config", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(listOut, bearerToken) {
		t.Errorf("list leaked the token: %q", listOut)
	}
	if !strings.Contains(listOut, "bearer ***") {
		t.Errorf("list did not mask auth: %q", listOut)
	}
}

func TestConfigSetRejectsBareToken(t *testing.T) {
	withConfig(t)
	_, err := runCLI(t, "", "config", "set", "origin", "--base-url", "https://git.example.com/", "--auth", "bearer", "--token", bearerToken)
	if err == nil || !strings.Contains(err.Error(), "--token-stdin") {
		t.Errorf("bare --token should be rejected with a stdin hint, got %v", err)
	}
}

func TestConfigSetRejectsOversizedToken(t *testing.T) {
	withConfig(t)
	huge := strings.Repeat("a", (64<<10)+1)
	_, err := runCLI(t, huge, "config", "set", "origin", "--base-url", "https://git.example.com/", "--auth", "bearer", "--token-stdin")
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("an oversized token should be rejected, not truncated; got %v", err)
	}
}

func TestConfigSetEmptyStdin(t *testing.T) {
	withConfig(t)
	_, err := runCLI(t, "", "config", "set", "origin", "--base-url", "https://git.example.com/", "--auth", "bearer", "--token-stdin")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty stdin should error, got %v", err)
	}
}

func TestConfigSetBearerRequiresStdin(t *testing.T) {
	withConfig(t)
	_, err := runCLI(t, "", "config", "set", "origin", "--base-url", "https://git.example.com/", "--auth", "bearer")
	if err == nil || !strings.Contains(err.Error(), "token-stdin") {
		t.Errorf("bearer without --token-stdin should error, got %v", err)
	}
}

func TestConfigSetBasicRequiresUsername(t *testing.T) {
	withConfig(t)
	_, err := runCLI(t, bearerToken+"\n", "config", "set", "origin", "--base-url", "https://git.example.com/", "--auth", "basic", "--token-stdin")
	if err == nil || !strings.Contains(err.Error(), "username") {
		t.Errorf("basic without --username should error, got %v", err)
	}
}

func TestConfigSetDuplicateHostRejected(t *testing.T) {
	withConfig(t)
	if _, err := runCLI(t, "", "config", "set", "a", "--base-url", "https://git.example.com/one"); err != nil {
		t.Fatalf("first set: %v", err)
	}
	_, err := runCLI(t, "", "config", "set", "b", "--base-url", "https://git.example.com/two")
	if err == nil || !strings.Contains(err.Error(), "same host") {
		t.Errorf("second provider on the same host should be rejected, got %v", err)
	}
}

func TestConfigRemove(t *testing.T) {
	withConfig(t)
	if _, err := runCLI(t, "", "config", "set", "origin", "--base-url", "https://git.example.com/"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if out, err := runCLI(t, "", "config", "remove", "origin"); err != nil || !strings.Contains(out, `removed`) {
		t.Errorf("remove: out=%q err=%v", out, err)
	}
	if _, err := runCLI(t, "", "config", "remove", "origin"); err == nil {
		t.Error("removing a non-existent provider should error")
	}
}

func TestStatusMasksAndDefersToPortal(t *testing.T) {
	withConfig(t)
	if _, err := runCLI(t, bearerToken+"\n", "config", "set", "origin", "--base-url", "https://git.example.com/", "--auth", "bearer", "--token-stdin"); err != nil {
		t.Fatalf("set: %v", err)
	}
	out, err := runCLI(t, "", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(out, bearerToken) {
		t.Errorf("status leaked the token: %q", out)
	}
	if !strings.Contains(out, "origin") || !strings.Contains(out, "portal") {
		t.Errorf("status missing provider or portal note: %q", out)
	}
}

func TestSetUsageShowsContainerHint(t *testing.T) {
	withConfig(t)
	_, err := runCLI(t, "", "config", "set")
	if err == nil || !strings.Contains(err.Error(), "docker exec -i") {
		t.Errorf("set usage should include the docker exec hint, got %v", err)
	}
}
