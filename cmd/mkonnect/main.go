// mkonnect connects customer-internal tools to the Memento platform
// over a secure WebSocket tunnel authenticated with ML-DSA-65.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/plugin"
	"github.com/memento-knowledge/mkonnect/internal/ws"
)

func main() {
	// Handle Kubernetes health probe
	if len(os.Args) > 1 && os.Args[1] == "--health" {
		os.Exit(0)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkonnect: configuration error: %v\n", err)
		os.Exit(1)
	}

	log.Printf("mkonnect starting — connector=%s gateway=%s protocol=%s",
		cfg.ConnectorID, cfg.GatewayURL, cfg.ProtocolVersion)

	reg, err := plugin.Load(cfg)
	if err != nil {
		log.Fatalf("mkonnect: plugin registry load failed: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := ws.NewClient(cfg, auth.NewKeyStore(cfg.KeyFile))
	client.SetPluginHandler(plugin.Handler(reg))
	client.Run(ctx)
}
