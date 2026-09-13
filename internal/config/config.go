// Package config loads mkonnect runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultProtocolVersion = "v1"
)

// Config holds all runtime configuration for the mkonnect connector.
type Config struct {
	// GatewayURL is the WebSocket URL of the Bridge Gateway (required).
	GatewayURL string

	// ConnectorID is the stable identifier for this connector instance (required).
	ConnectorID string

	// RegistrationToken is used only on first run to register with the gateway.
	// After registration it is no longer needed.
	RegistrationToken string

	// KeyFile is the path to the ML-DSA key store file.
	// Defaults to ~/.mkonnect/key.
	KeyFile string

	// ProtocolVersion is the wire protocol version to negotiate.
	// Defaults to "v1".
	ProtocolVersion string
}

// Load reads configuration from environment variables and applies defaults.
// It returns an error if required variables are missing.
func Load() (*Config, error) {
	cfg := &Config{
		GatewayURL:        os.Getenv("GATEWAY_URL"),
		ConnectorID:       os.Getenv("CONNECTOR_ID"),
		RegistrationToken: os.Getenv("REGISTRATION_TOKEN"),
		KeyFile:           os.Getenv("KEY_FILE"),
		ProtocolVersion:   os.Getenv("PROTOCOL_VERSION"),
	}

	if cfg.GatewayURL == "" {
		return nil, fmt.Errorf("GATEWAY_URL is required")
	}
	if !strings.HasPrefix(cfg.GatewayURL, "ws://") && !strings.HasPrefix(cfg.GatewayURL, "wss://") {
		return nil, fmt.Errorf("GATEWAY_URL must start with ws:// or wss://")
	}
	if cfg.ConnectorID == "" {
		return nil, fmt.Errorf("CONNECTOR_ID is required")
	}

	if cfg.KeyFile == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolving home dir for KEY_FILE default: %w", err)
		}
		cfg.KeyFile = filepath.Join(home, ".mkonnect", "key")
	}

	if cfg.ProtocolVersion == "" {
		cfg.ProtocolVersion = DefaultProtocolVersion
	}

	return cfg, nil
}
