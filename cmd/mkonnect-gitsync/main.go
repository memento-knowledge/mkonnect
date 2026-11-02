// Command mkonnect-gitsync is the git-sync connector: it bridges customer-internal git
// repositories to the Memento platform over the same secure, ML-DSA-65-authenticated WebSocket
// tunnel as the API connector. It is a separate binary because it is a separate process with
// its own registration-token kind, its own key, and (from Phase 3) its own toolchain needs.
//
// This is a Phase 2 skeleton: it enrols, handshakes, heartbeats, and dispatches the git-sync
// protocol messages, but the sync algorithm itself is not yet implemented. Packaging and
// release of this binary are tracked separately (memento-knowledge/mkonnect#48); nothing builds
// or ships it as an image yet.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/version"
)

// defaultLocalConfigFile is where the git-sync local repository config is read from when
// GIT_SYNC_CONFIG_FILE is unset. It sits alongside the API connector's credentials.json on the
// same data volume. NOTE (provisional): the env var name and this path are not yet finalised,
// and nothing mounts this file yet — see memento-knowledge/mkonnect#48 for packaging.
const defaultLocalConfigFile = "/data/git-sync.json"

func main() {
	// Kubernetes health probe, matching the API connector's no-op liveness contract.
	if len(os.Args) > 1 && os.Args[1] == "--health" {
		os.Exit(0)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkonnect-gitsync: configuration error: %v\n", err)
		os.Exit(1)
	}

	// Use a distinct default key path so running alongside the API connector with default
	// settings does not make the two share one key file (where the second registration would
	// clobber the first). An explicit KEY_FILE still wins. Operators must also give this
	// connector its own CONNECTOR_ID — that is required configuration and cannot be defaulted.
	if os.Getenv("KEY_FILE") == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cfg.KeyFile = filepath.Join(home, ".mkonnect", "gitsync-key")
		}
	}

	localPath := os.Getenv("GIT_SYNC_CONFIG_FILE")
	if localPath == "" {
		localPath = defaultLocalConfigFile
	}
	local, err := gitsync.New(localPath)
	if err != nil {
		log.Fatalf("mkonnect-gitsync: local config load failed: %v", err)
	}

	// The reachability probe shells out to git; warn loudly if the image lacks a usable git so the
	// operator sees a packaging problem rather than silent "unreachable" results.
	if ok, detail := gitsync.GitVersionOK(); !ok {
		log.Printf("mkonnect-gitsync: WARNING: %s; the origin reachability check needs git >= 2.31", detail)
	}

	bootID, err := randomBootID()
	if err != nil {
		log.Fatalf("mkonnect-gitsync: could not generate a boot id: %v", err)
	}

	log.Printf("mkonnect-gitsync starting — connector=%s gateway=%s protocol=%s version=%s providers=%d",
		cfg.ConnectorID, cfg.GatewayURL, cfg.ProtocolVersion, version.Version, len(local.Names()))

	// No SIGHUP reload in Phase 2: there is nothing to reload yet (the local config is read
	// once and the sync loop does not exist). A reload path arrives with the Phase 3/4 work
	// that gives it something to pick up.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := gitsync.NewClient(cfg, auth.NewKeyStore(cfg.KeyFile), local, gitsync.NewGitProber(), nil, bootID)
	client.Run(ctx)
}

// randomBootID returns a random per-process identifier so the platform can tell a status stream
// apart from the one before a restart (a sequence number alone restarts at a lower value).
func randomBootID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
