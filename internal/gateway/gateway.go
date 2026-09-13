// Package gateway provides the WebSocket connection to the Bridge Gateway.
// Full implementation is added in Task 3.
package gateway

import (
	_ "github.com/cloudflare/circl"
	_ "github.com/coder/websocket"
	_ "github.com/golang-jwt/jwt/v5"

	"github.com/memento-knowledge/mkonnect/internal/config"
)

// Connect establishes and maintains a connection to the Bridge Gateway.
// This is a placeholder; the full implementation arrives in Task 3.
func Connect(cfg *config.Config) error {
	// TODO(task-3): implement ML-DSA-65 challenge-response auth and
	// WebSocket tunnel establishment.
	// - Use circl for ML-DSA-65 cryptographic operations
	// - Use websocket for WebSocket tunnel
	// - Use jwt/v5 for token authentication
	_ = cfg
	return nil
}
