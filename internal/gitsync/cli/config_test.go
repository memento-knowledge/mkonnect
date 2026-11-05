package cli_test

import (
	"bytes"
	"os"
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

func TestStatusShowsPinnedReplicaHost(t *testing.T) {
	withConfig(t)
	t.Setenv(gitsync.ReplicaHostEnv, "Replica.Example.com:9443")
	out, err := runCLI(t, "", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "replica.example.com:9443") || !strings.Contains(out, gitsync.ReplicaHostEnv) {
		t.Errorf("status does not show the effective pin and its source: %q", out)
	}
	if strings.Contains(out, "NOT SET") || strings.Contains(out, "INVALID") {
		t.Errorf("a valid pin is flagged as a problem: %q", out)
	}
}

func TestStatusWarnsLoudlyWhenNoReplicaHostIsPinned(t *testing.T) {
	withConfig(t)
	t.Setenv(gitsync.ReplicaHostEnv, "")
	out, err := runCLI(t, "", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "NOT SET - the connector will reject every configuration") {
		t.Errorf("status must flag the missing pin: %q", out)
	}
}

func TestStatusFlagsInvalidReplicaHostWithoutEchoingIt(t *testing.T) {
	withConfig(t)
	t.Setenv(gitsync.ReplicaHostEnv, "https://pin-leak-marker.example.com/path")
	out, err := runCLI(t, "", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "INVALID") || !strings.Contains(out, "reject every configuration") {
		t.Errorf("status must flag an invalid pin: %q", out)
	}
	if strings.Contains(out, "pin-leak-marker") {
		t.Errorf("status echoed the invalid value: %q", out)
	}
}

// The pin is the most important line for an operator, so it prints even when the provider config
// cannot be loaded.
func TestStatusShowsPinEvenWhenProviderConfigIsBroken(t *testing.T) {
	path := withConfig(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(gitsync.ReplicaHostEnv, "replica.example.com")
	out, err := runCLI(t, "", "status")
	if err == nil {
		t.Error("a broken provider config should still be reported as an error")
	}
	if !strings.Contains(out, "replica.example.com:443") {
		t.Errorf("pin line missing when the provider config is broken: %q", out)
	}
}

// A provider whose credential lives in a token file is listed as such, without the token or path,
// and an unrelated `config set` keeps it as a token_file provider.
func TestConfigListMarksTokenFileProviderAndSetKeepsIt(t *testing.T) {
	path := withConfig(t)
	dir := filepath.Dir(path)
	tokenPath := filepath.Join(dir, "git-token")
	if err := os.WriteFile(tokenPath, []byte(bearerToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `{"filed": {"base_url": "https://git.example.test/", "auth": "bearer", "token_file": "` + tokenPath + `"}}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	listOut, err := runCLI(t, "", "config", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(listOut, "bearer *** (token_file)") {
		t.Errorf("list should mark the token_file provider: %q", listOut)
	}
	if strings.Contains(listOut, bearerToken) || strings.Contains(listOut, tokenPath) {
		t.Errorf("list leaked the token or the token file path: %q", listOut)
	}

	if _, err := runCLI(t, "", "config", "set", "plain", "--base-url", "https://git-plain.example.test/"); err != nil {
		t.Fatalf("set: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), bearerToken) || !strings.Contains(string(raw), tokenPath) {
		t.Errorf("config set must keep the token_file reference and not inline the token: %s", raw)
	}
}

// A flag the parser rejects must not be echoed back: a token typed where a flag value belongs would
// otherwise reach the error output.
func TestConfigSetFlagErrorsDoNotEchoArguments(t *testing.T) {
	withConfig(t)
	const typed = "typed-in-the-wrong-place-0123456789"
	for _, arg := range []string{"--token-stdin=" + typed, "--" + typed, "--base-url"} {
		_, err := runCLI(t, "", "config", "set", "origin", "--base-url", "https://git.example.com/", arg)
		if err == nil {
			t.Fatalf("%q should be rejected", arg)
		}
		if strings.Contains(err.Error(), typed) {
			t.Errorf("error for %q repeats the argument: %v", arg, err)
		}
	}
}
