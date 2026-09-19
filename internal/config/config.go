// Package config loads mkonnect runtime configuration from environment variables.
package config

import (
	"fmt"
	"net"
	"net/url"
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
	gatewayURL, err := url.Parse(cfg.GatewayURL)
	if err != nil || gatewayURL.Host == "" || (gatewayURL.Scheme != "ws" && gatewayURL.Scheme != "wss") {
		return nil, fmt.Errorf("GATEWAY_URL must be an absolute ws:// or wss:// URL")
	}
	if gatewayURL.Scheme == "ws" {
		if os.Getenv("ALLOW_INSECURE_GATEWAY") != "true" {
			return nil, fmt.Errorf("GATEWAY_URL must use wss://; for local development only, set ALLOW_INSECURE_GATEWAY=true with a loopback ws:// URL")
		}
		host := gatewayURL.Hostname()
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("ALLOW_INSECURE_GATEWAY only permits ws:// loopback URLs")
		}
		fmt.Fprintln(os.Stderr, "WARNING: plaintext loopback gateway explicitly enabled for local development")
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
