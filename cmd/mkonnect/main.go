// mkonnect connects customer-internal tools to the Memento platform
// over a secure WebSocket tunnel authenticated with ML-DSA-65.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/cli"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/creds"
	"github.com/memento-knowledge/mkonnect/internal/plugin"
	"github.com/memento-knowledge/mkonnect/internal/version"
	"github.com/memento-knowledge/mkonnect/internal/ws"
)

func main() {
	// Handle connector subcommand dispatch before anything else.
	if len(os.Args) > 1 && os.Args[1] == "connector" {
		if err := cli.Run(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "connector: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Handle Kubernetes health probe
	if len(os.Args) > 1 && os.Args[1] == "--health" {
		os.Exit(0)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkonnect: configuration error: %v\n", err)
		os.Exit(1)
	}

	log.Printf("mkonnect starting — connector=%s gateway=%s protocol=%s version=%s",
		cfg.ConnectorID, cfg.GatewayURL, cfg.ProtocolVersion, version.Version)

	reg, err := plugin.Load(cfg)
	if err != nil {
		log.Fatalf("mkonnect: plugin registry load failed: %v", err)
	}

	credsPath := os.Getenv("CREDS_FILE")
	if credsPath == "" {
		credsPath = "/data/credentials.json"
	}
	store, err := creds.New(credsPath)
	if err != nil {
		log.Fatalf("mkonnect: creds store load failed: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sighupCh := make(chan os.Signal, 1)
	signal.Notify(sighupCh, syscall.SIGHUP)

	client := ws.NewClient(cfg, auth.NewKeyStore(cfg.KeyFile))
	client.SetHTTPPluginHandler(plugin.HTTPHandler(reg, store))
	client.SetCredsStore(store)
	client.SetPluginRegistry(reg)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sighupCh:
				if err := store.Reload(); err != nil {
					slog.Warn("creds reload failed", "err", err)
				}
				client.SignalStatusPush()
			}
		}
	}()

	client.Run(ctx)
}
