// internal/cli/config.go
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/memento-knowledge/mkonnect/internal/creds"
)

const defaultCredsFile = "/data/credentials.json"

func credsPath() string {
	if p := os.Getenv("CREDS_FILE"); p != "" {
		return p
	}
	return defaultCredsFile
}

// Run dispatches CLI subcommands for the `connector` binary. stdin supplies the token for
// `config set --token-stdin`; output is written to w.
func Run(args []string, stdin io.Reader, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: connector <config|status> [args...]")
	}
	switch args[0] {
	case "config":
		return runConfig(args[1:], stdin, w)
	case "status":
		return runStatus(args[1:], w)
	default:
		return fmt.Errorf("unknown command: %q", args[0])
	}
}

func runConfig(args []string, stdin io.Reader, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: connector config <set|list|remove> [args...]")
	}
	switch args[0] {
	case "set":
		return runConfigSet(args[1:], stdin, w)
	case "list":
		return runConfigList(w)
	case "remove":
		return runConfigRemove(args[1:], w)
	default:
		return fmt.Errorf("unknown config subcommand: %q", args[0])
	}
}

func runConfigSet(args []string, stdin io.Reader, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: connector config set <plugin> --base-url <url> [--auth bearer (--token <token> | --token-stdin)]")
	}
	plugin := args[0]
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	fs.SetOutput(w)
	baseURL := fs.String("base-url", "", "Base URL of the plugin service (required)")
	auth := fs.String("auth", "", "Auth type: bearer")
	token := fs.String("token", "", "Bearer token (exposed via process args/shell history — prefer --token-stdin)")
	tokenStdin := fs.Bool("token-stdin", false, "Read the bearer token from stdin instead of --token")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *baseURL == "" {
		return errors.New("--base-url is required")
	}
	if u, err := url.Parse(*baseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("--base-url must be an absolute http or https URL, got %q", *baseURL)
	}
	if *auth != "" && *auth != "bearer" {
		return fmt.Errorf("unsupported auth type: %q (only 'bearer' is supported)", *auth)
	}

	// Resolve the token from exactly one source.
	if *token != "" && *tokenStdin {
		return errors.New("provide either --token or --token-stdin, not both")
	}
	tokenVal := *token
	if *tokenStdin {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("read token from stdin: %w", err)
		}
		// Trim a trailing newline so `echo tok | ... --token-stdin` works cleanly.
		tokenVal = strings.TrimRight(string(b), "\r\n")
		if tokenVal == "" {
			return errors.New("--token-stdin was set but stdin was empty")
		}
	}

	// An inline --token is exposed via the process list / shell history regardless of
	// --auth; warn whenever it's used.
	if *token != "" {
		fmt.Fprintln(w, "Warning: --token is visible in the process list and shell history; prefer --token-stdin.")
	}
	switch {
	case *auth == "bearer" && tokenVal == "":
		return errors.New("a token is required when --auth bearer (use --token-stdin, or --token)")
	case tokenVal != "" && *auth != "bearer":
		fmt.Fprintf(w, "Warning: a token was provided but --auth is %q; it will be stored but not used.\n", *auth)
	}

	store, err := creds.New(credsPath())
	if err != nil {
		return fmt.Errorf("open creds store: %w", err)
	}
	if err := store.Set(plugin, creds.Credential{
		BaseURL: *baseURL,
		Auth:    *auth,
		Token:   tokenVal,
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
	names := make([]string, 0, len(list))
	for name := range list {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cred := list[name]
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
	names := make([]string, 0, len(list))
	for name := range list {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cred := list[name]
		authDesc := "no auth"
		if cred.Auth == "bearer" {
			authDesc = "bearer"
		}
		fmt.Fprintf(w, "  %-20s  %s  [%s]\n", name, cred.BaseURL, authDesc)
	}
	fmt.Fprintln(w, "\nBridge connection status is visible in the Memento portal.")
	return nil
}
