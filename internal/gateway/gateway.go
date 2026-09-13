// Package gateway provides the WebSocket connection to the Bridge Gateway.
package gateway

import (
	"context"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/ws"
)

// Connect establishes and maintains a connection to the Bridge Gateway.
// It blocks until ctx is cancelled or an unrecoverable error occurs.
func Connect(ctx context.Context, cfg *config.Config) error {
	keyStore := auth.NewKeyStore(cfg.KeyFile)
	client := ws.NewClient(cfg, keyStore)
	client.Run(ctx)
	return ctx.Err()
}
