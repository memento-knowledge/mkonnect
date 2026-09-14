// internal/cli/config.go
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/memento-knowledge/mkonnect/internal/creds"
)

const defaultCredsFile = "/data/credentials.json"

func credsPath() string {
	if p := os.Getenv("CREDS_FILE"); p != "" {
		return p
	}
	return defaultCredsFile
}

// Run dispatches CLI subcommands for the `connector` binary. Output is written to w.
func Run(args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: connector <config|status> [args...]")
	}
	switch args[0] {
	case "config":
		return runConfig(args[1:], w)
	case "status":
		return runStatus(args[1:], w)
	default:
		return fmt.Errorf("unknown command: %q", args[0])
	}
}

func runConfig(args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: connector config <set|list|remove> [args...]")
	}
	switch args[0] {
	case "set":
		return runConfigSet(args[1:], w)
	case "list":
		return runConfigList(w)
	case "remove":
		return runConfigRemove(args[1:], w)
	default:
		return fmt.Errorf("unknown config subcommand: %q", args[0])
	}
}

func runConfigSet(args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: connector config set <plugin> --base-url <url> [--auth bearer --token <token>]")
	}
	plugin := args[0]
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	fs.SetOutput(w)
	baseURL := fs.String("base-url", "", "Base URL of the plugin service (required)")
	auth := fs.String("auth", "", "Auth type: bearer")
	token := fs.String("token", "", "Bearer token (required when --auth bearer)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *baseURL == "" {
		return errors.New("--base-url is required")
	}
	if *auth == "bearer" && *token == "" {
		return errors.New("--token is required when --auth bearer")
	}
	if *auth != "" && *auth != "bearer" {
		return fmt.Errorf("unsupported auth type: %q (only 'bearer' is supported)", *auth)
	}

	store, err := creds.New(credsPath())
	if err != nil {
		return fmt.Errorf("open creds store: %w", err)
	}
	if err := store.Set(plugin, creds.Credential{
		BaseURL: *baseURL,
		Auth:    *auth,
		Token:   *token,
	}); err != nil {
		return fmt.Errorf("save credential: %w", err)
	}
	fmt.Fprintf(w, "Credential for %q saved.\n", plugin)
	return nil
}

func runConfigList(w io.Writer) error {
	store, err := creds.New(credsPath())
	if err != nil {
		return fmt.Errorf("open creds store: %w", err)
	}
	list := store.List()
	if len(list) == 0 {
		fmt.Fprintln(w, "No plugins configured.")
		return nil
	}
	fmt.Fprintf(w, "%-20s  %-40s  %s\n", "PLUGIN", "BASE URL", "AUTH")
	for name, cred := range list {
		auth := "(none)"
		if cred.Auth == "bearer" {
			auth = "bearer ***"
		}
		fmt.Fprintf(w, "%-20s  %-40s  %s\n", name, cred.BaseURL, auth)
	}
	return nil
}

func runConfigRemove(args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: connector config remove <plugin>")
	}
	plugin := args[0]
	store, err := creds.New(credsPath())
	if err != nil {
		return fmt.Errorf("open creds store: %w", err)
	}
	if err := store.Remove(plugin); err != nil {
		return fmt.Errorf("remove credential: %w", err)
	}
	fmt.Fprintf(w, "Credential for %q removed.\n", plugin)
	return nil
}

func runStatus(_ []string, w io.Writer) error {
	store, err := creds.New(credsPath())
	if err != nil {
		return fmt.Errorf("open creds store: %w", err)
	}
	list := store.List()
	fmt.Fprintln(w, "=== Configured plugins ===")
	if len(list) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for name, cred := range list {
		authDesc := "no auth"
		if cred.Auth == "bearer" {
			authDesc = "bearer"
		}
		fmt.Fprintf(w, "  %-20s  %s  [%s]\n", name, cred.BaseURL, authDesc)
	}
	fmt.Fprintln(w, "\nBridge connection status is visible in the Memento portal.")
	return nil
}
