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
	for _, k := range []string{"GATEWAY_URL", "CONNECTOR_ID", "REGISTRATION_TOKEN", "REGISTRATION_TOKEN_FILE", "KEY_FILE", "PROTOCOL_VERSION", "ALLOW_INSECURE_GATEWAY", "CA_BUNDLE"} {
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

func TestLoadCABundle(t *testing.T) {
	clearEnv(t)
	t.Setenv("GATEWAY_URL", "wss://gateway.example.com/ws")
	t.Setenv("CONNECTOR_ID", "c1")

	// Absolute path is accepted and stored.
	t.Setenv("CA_BUNDLE", "/etc/mkonnect-ca/ca-bundle.pem")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load with absolute CA_BUNDLE: %v", err)
	}
	if cfg.CABundle != "/etc/mkonnect-ca/ca-bundle.pem" {
		t.Errorf("CABundle = %q, want the absolute path", cfg.CABundle)
	}

	// Relative path is rejected.
	t.Setenv("CA_BUNDLE", "relative/ca.pem")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected Load to reject a relative CA_BUNDLE")
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

func TestParseGatewayURL(t *testing.T) {
	tests := []struct {
		name, raw, insecure string
		wantErr             string
	}{
		{name: "wss", raw: "wss://gateway.example.test/ws"},
		{name: "wss with port", raw: "wss://gateway.example.test:8443"},
		{name: "empty", raw: "", wantErr: "GATEWAY_URL is required"},
		{name: "https", raw: "https://gateway.example.test", wantErr: "ws:// or wss://"},
		{name: "no host", raw: "wss:///x", wantErr: "ws:// or wss://"},
		{name: "ws without opt-in", raw: "ws://127.0.0.1:8080", wantErr: "must use wss://"},
		{name: "ws loopback", raw: "ws://127.0.0.1:8080", insecure: "true"},
		{name: "ws localhost", raw: "ws://localhost:8080", insecure: "true"},
		{name: "ws not loopback", raw: "ws://gateway.example.test", insecure: "true", wantErr: "loopback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ALLOW_INSECURE_GATEWAY", tt.insecure)
			u, err := config.ParseGatewayURL(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if tt.raw != "" && strings.Contains(err.Error(), tt.raw) {
					t.Errorf("the error repeats the value: %v", err)
				}
				return
			}
			if err != nil || u == nil {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
