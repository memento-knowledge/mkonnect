// mkonnect connects customer-internal tools to the Memento platform
// over a secure WebSocket tunnel authenticated with ML-DSA-65.
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/gateway"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkonnect: configuration error: %v\n", err)
		os.Exit(1)
	}

	log.Printf("mkonnect starting — connector=%s gateway=%s protocol=%s",
		cfg.ConnectorID, cfg.GatewayURL, cfg.ProtocolVersion)

	if err := gateway.Connect(cfg); err != nil {
		log.Fatalf("mkonnect: gateway connection failed: %v", err)
	}
}
