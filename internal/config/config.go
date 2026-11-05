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
	// After registration it is no longer needed. It comes from REGISTRATION_TOKEN, or
	// from the file named by REGISTRATION_TOKEN_FILE, which takes precedence.
	RegistrationToken string

	// KeyFile is the path to the ML-DSA key store file.
	// Defaults to ~/.mkonnect/key.
	KeyFile string

	// ProtocolVersion is the wire protocol version to negotiate.
	// Defaults to "v1".
	ProtocolVersion string

	// CABundle is an optional absolute path to a PEM file of additional root CA(s) to
	// trust for ALL upstream plugin TLS, used for any plugin that does not set its own
	// per-plugin ca_bundle. Its certificates are added to the system trust store, never
	// a replacement, and there is no option to disable verification. Empty means the
	// system trust store only.
	CABundle string
}

// ParseGatewayURL validates a GATEWAY_URL value by the rules the connector applies at startup: an
// absolute wss:// URL, or, for local development only, a ws:// loopback URL with
// ALLOW_INSECURE_GATEWAY=true. It prints nothing and its errors never repeat the value. The operator
// preflight command shares it so it judges the URL exactly as the daemon does.
func ParseGatewayURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("GATEWAY_URL is required")
	}
	gatewayURL, err := url.Parse(raw)
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
	}
	return gatewayURL, nil
}

// Load reads configuration from environment variables and applies defaults.
// It returns an error if required variables are missing.
func Load() (*Config, error) {
	cfg := &Config{
		GatewayURL:      os.Getenv("GATEWAY_URL"),
		ConnectorID:     os.Getenv("CONNECTOR_ID"),
		KeyFile:         os.Getenv("KEY_FILE"),
		ProtocolVersion: os.Getenv("PROTOCOL_VERSION"),
		CABundle:        os.Getenv("CA_BUNDLE"),
	}

	gatewayURL, err := ParseGatewayURL(cfg.GatewayURL)
	if err != nil {
		return nil, err
	}
	if gatewayURL.Scheme == "ws" {
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

	// A global CA bundle, if set, must be an absolute path: it is resolved by the
	// long-running daemon, whose working directory a relative path cannot be relied on
	// to match. The file itself is read (and failures surfaced) at request time.
	if cfg.CABundle != "" && !filepath.IsAbs(cfg.CABundle) {
		return nil, fmt.Errorf("CA_BUNDLE must be an absolute path")
	}

	cfg.RegistrationToken, err = registrationToken()
	if err != nil {
		return nil, err
	}

	return cfg, nil
}
