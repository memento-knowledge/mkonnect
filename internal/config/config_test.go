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
	for _, k := range []string{"GATEWAY_URL", "CONNECTOR_ID", "REGISTRATION_TOKEN", "KEY_FILE", "PROTOCOL_VERSION"} {
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

// TestLoadAllowsPlaintextWebSocket confirms ws:// is accepted (with a stderr warning, not
// asserted here) so a plaintext dev/test gateway still works.
func TestLoadAllowsPlaintextWebSocket(t *testing.T) {
	clearEnv(t)
	t.Setenv("GATEWAY_URL", "ws://localhost:8080/ws")
	t.Setenv("CONNECTOR_ID", "c1")
	if _, err := config.Load(); err != nil {
		t.Fatalf("ws:// should be allowed, got error: %v", err)
	}
}
