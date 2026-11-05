// Command mkonnect-gitsync is the git-sync connector: it bridges customer-internal git
// repositories to the Memento platform over the same secure, ML-DSA-65-authenticated WebSocket
// tunnel as the API connector. It is a separate binary because it is a separate process with
// its own registration-token kind, its own key, and its own toolchain needs (a git binary).
//
// It enrols, handshakes, heartbeats, and mirrors each configured repository: a fetch of its
// branches and tags from the customer origin into a local mirror, then a push of them to the
// Memento replica. It ships as its own container image, built from Dockerfile.gitsync.
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
	"time"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/gitsync/cli"
	"github.com/memento-knowledge/mkonnect/internal/version"
)

func main() {
	// Operator CLI: `mkonnect-gitsync connector <config|status|preflight> ...`. Handled before loading daemon
	// configuration so an admin can configure credentials without a running connector.
	if len(os.Args) > 1 && os.Args[1] == "connector" {
		if err := cli.Run(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "connector: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Kubernetes health probe, matching the API connector's no-op liveness contract.
	if len(os.Args) > 1 && os.Args[1] == "--health" {
		os.Exit(0)
	}

	// SIGHUP re-reads the provider configuration (see the reload loop below). It is claimed this early
	// so that one arriving during the slower startup steps is held until the client exists, rather than
	// taking the signal's default action, which is to terminate the process.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

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

	local, err := gitsync.New(gitsync.DefaultConfigPath())
	if err != nil {
		log.Fatalf("mkonnect-gitsync: local config load failed: %v", err)
	}

	// Both the reachability probe and the mirror sync shell out to git; warn loudly if the image
	// lacks a usable git so the operator sees a packaging problem rather than silent failures.
	if ok, detail := gitsync.GitVersionOK(); !ok {
		log.Printf("mkonnect-gitsync: WARNING: %s; syncing and the origin reachability check need git >= 2.31", detail)
	}

	// Resolve (and create) the temp root now so a bad GIT_SYNC_TMP_DIR fails at startup rather than
	// on the first sync, then clear temp homes a previous crash left behind. Best effort.
	tmpRoot, err := gitsync.TempRoot()
	if err != nil {
		log.Fatalf("mkonnect-gitsync: %v", err)
	}
	n, err := gitsync.SweepStaleTempHomes(tmpRoot, staleTempHomeAge)
	if n > 0 {
		log.Printf("mkonnect-gitsync: removed %d stale temp directories from %s", n, tmpRoot)
	}
	if err != nil {
		log.Printf("mkonnect-gitsync: WARNING: could not fully clean stale temp directories in %s: %v", tmpRoot, err)
	}

	bootID, err := randomBootID()
	if err != nil {
		log.Fatalf("mkonnect-gitsync: could not generate a boot id: %v", err)
	}

	log.Printf("mkonnect-gitsync starting — connector=%s gateway=%s protocol=%s version=%s providers=%d",
		cfg.ConnectorID, cfg.GatewayURL, cfg.ProtocolVersion, version.Version, len(local.Names()))

	// The provider config is watched while the daemon runs: a change to it (a revoked or rotated
	// credential included) takes effect within seconds, and SIGHUP applies one at once. The replica
	// host pin, the registration token file and the key file are read once, here, and need a restart.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The replica host is pinned once, here, from the environment: the connector pushes only to that
	// host and, with no valid pin, rejects every configuration (see gitsync.ReplicaHostEnv). The
	// client logs the effective pin, or why there is none, without echoing the raw value.
	client := gitsync.NewClient(cfg, auth.NewKeyStore(cfg.KeyFile), local, gitsync.NewGitProber(), nil, bootID,
		gitsync.WithReplicaHost(os.Getenv(gitsync.ReplicaHostEnv)))
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				client.Reload()
			}
		}
	}()
	client.Run(ctx)

	// Cancelling ctx kills each running git (its own process group, so a terminal Ctrl-C does not
	// reach it), but asynchronously. Wait, briefly, for those syncs to stop so no git process that
	// holds a credential outlives the daemon.
	wctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()
	client.Wait(wctx)
}

// shutdownWait bounds how long the daemon waits, after cancellation, for syncs to stop.
const shutdownWait = 10 * time.Second

// staleTempHomeAge is how old a leftover temp home must be before the startup sweep removes it.
const staleTempHomeAge = time.Minute

// randomBootID returns a random per-process identifier so the platform can tell a status stream
// apart from the one before a restart (a sequence number alone restarts at a lower value).
func randomBootID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
