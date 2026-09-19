package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/config"
)

// clearEnv blanks every variable Load reads so a test starts from a known-empty base
// regardless of the ambient environment; the test then sets only what it needs.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GATEWAY_URL", "CONNECTOR_ID", "REGISTRATION_TOKEN", "KEY_FILE", "PROTOCOL_VERSION", "ALLOW_INSECURE_GATEWAY"} {
		t.Setenv(k, "")
	}
}

func TestLoadRequiresGatewayURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("CONNECTOR_ID", "c1")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error when GATEWAY_URL is missing")
	}
}

func TestLoadRejectsNonWebSocketScheme(t *testing.T) {
	clearEnv(t)
	t.Setenv("CONNECTOR_ID", "c1")
	t.Setenv("GATEWAY_URL", "https://gateway.example.com/ws")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error when GATEWAY_URL is not ws:// or wss://")
	}
}

func TestLoadRequiresConnectorID(t *testing.T) {
	clearEnv(t)
	t.Setenv("GATEWAY_URL", "wss://gateway.example.com/ws")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error when CONNECTOR_ID is missing")
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("GATEWAY_URL", "wss://gateway.example.com/ws")
	t.Setenv("CONNECTOR_ID", "c1")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ProtocolVersion != config.DefaultProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want default %q", cfg.ProtocolVersion, config.DefaultProtocolVersion)
	}
	wantSuffix := filepath.Join(".mkonnect", "key")
	if !strings.HasSuffix(cfg.KeyFile, wantSuffix) {
		t.Errorf("KeyFile = %q, want it to default under ~/%s", cfg.KeyFile, wantSuffix)
	}
}

func TestLoadPreservesExplicitValues(t *testing.T) {
	clearEnv(t)
	t.Setenv("GATEWAY_URL", "wss://gateway.example.com/ws")
	t.Setenv("CONNECTOR_ID", "c1")
	t.Setenv("REGISTRATION_TOKEN", "tok")
	t.Setenv("KEY_FILE", "/data/key")
	t.Setenv("PROTOCOL_VERSION", "v2")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RegistrationToken != "tok" {
		t.Errorf("RegistrationToken = %q, want %q", cfg.RegistrationToken, "tok")
	}
	if cfg.KeyFile != "/data/key" {
		t.Errorf("KeyFile = %q, want %q", cfg.KeyFile, "/data/key")
	}
	if cfg.ProtocolVersion != "v2" {
		t.Errorf("ProtocolVersion = %q, want %q", cfg.ProtocolVersion, "v2")
	}
}

func TestLoadRejectsPlaintextWebSocketByDefault(t *testing.T) {
	clearEnv(t)
	t.Setenv("GATEWAY_URL", "ws://localhost:8080/ws")
	t.Setenv("CONNECTOR_ID", "c1")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected ws:// to be rejected without an explicit development opt-in")
	}
}

func TestLoadRejectsPlaintextWebSocketForNonLoopbackHost(t *testing.T) {
	clearEnv(t)
	t.Setenv("GATEWAY_URL", "ws://gateway.example.com/ws")
	t.Setenv("CONNECTOR_ID", "c1")
	t.Setenv("ALLOW_INSECURE_GATEWAY", "true")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected ws:// to be rejected for a non-loopback host")
	}
}

func TestLoadAllowsExplicitPlaintextLoopbackGateway(t *testing.T) {
	clearEnv(t)
	t.Setenv("GATEWAY_URL", "ws://127.0.0.1:8080/ws")
	t.Setenv("CONNECTOR_ID", "c1")
	t.Setenv("ALLOW_INSECURE_GATEWAY", "true")
	if _, err := config.Load(); err != nil {
		t.Fatalf("explicitly enabled loopback ws:// should be allowed, got error: %v", err)
	}
}
