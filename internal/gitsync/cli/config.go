// Package cli implements the git-sync connector's operator command line: `config set`, `list`,
// `remove`, `check`, `status`, and `preflight`. It mirrors the API connector's CLI (internal/cli) —
// the credential comes on stdin, never on the command line — but writes git-sync's own
// provider-keyed local config.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

const setUsage = `usage: mkonnect-gitsync connector config set <name> --base-url <url> [--auth bearer|basic --token-stdin [--username <username>]]

The token is read from stdin so it never appears in the process list or shell history. In a
container, run:
  docker exec -i <container> /mkonnect-gitsync connector config set <name> --base-url <url> --auth bearer --token-stdin`

// maxTokenBytes bounds the credential read from stdin. A real token is tiny; anything larger is a
// misdirected pipe and is rejected rather than truncated.
const maxTokenBytes = 64 << 10

// Run dispatches the git-sync operator commands. args is everything after "connector".
func Run(args []string, stdin io.Reader, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mkonnect-gitsync connector <config|status|preflight> [args...]")
	}
	switch args[0] {
	case "config":
		return runConfig(args[1:], stdin, w)
	case "status":
		return runStatus(args[1:], w)
	case "preflight":
		return runPreflight(args[1:], w)
	default:
		return fmt.Errorf("unknown command %q; use config, status or preflight", args[0])
	}
}

func runConfig(args []string, stdin io.Reader, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mkonnect-gitsync connector config <set|list|remove|check> [args...]")
	}
	switch args[0] {
	case "set":
		return runConfigSet(args[1:], stdin, w)
	case "list":
		return runConfigList(w)
	case "remove":
		return runConfigRemove(args[1:], w)
	case "check":
		return runConfigCheck(args[1:], w)
	default:
		return fmt.Errorf("unknown config command %q; use set, list, remove, or check", args[0])
	}
}

func runConfigSet(args []string, stdin io.Reader, w io.Writer) error {
	if len(args) == 0 {
		return errors.New(setUsage)
	}
	name := args[0]
	if strings.HasPrefix(name, "-") {
		return errors.New("the provider name must come before the flags, e.g. `config set <name> --base-url ...`")
	}

	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	// Discard the flag package's own output so a parse error is reported once — by the caller, which
	// prints the returned error to stderr — rather than also to w here.
	fs.SetOutput(io.Discard)
	baseURL := fs.String("base-url", "", "Origin base URL, e.g. https://git.example.com/")
	auth := fs.String("auth", "", "Authentication mode: bearer or basic")
	username := fs.String("username", "", "Username (basic auth only)")
	tokenStdin := fs.Bool("token-stdin", false, "Read the token/password from stdin")
	fs.String("token", "", "Unsupported; use --token-stdin")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(w, setUsage)
			return nil
		}
		// The flag package's text quotes the offending argument ("invalid boolean value "<x>"" for
		// --token-stdin=<x>), which here may be a token typed in the wrong place, so it is not repeated.
		return errors.New("invalid flags; run `config set -h` for usage, and pass the token on stdin with --token-stdin")
	}

	// Reject a token passed on the command line, where it would be visible in the process list.
	tokenFlagUsed := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "token" {
			tokenFlagUsed = true
		}
	})
	if tokenFlagUsed {
		return errors.New("--token is not supported; pass the token through stdin with --token-stdin")
	}
	if *baseURL == "" {
		return errors.New("--base-url is required")
	}

	// Flag consistency only; URL and token content are validated by the Store, so the two cannot
	// drift.
	switch *auth {
	case "":
		if *tokenStdin || *username != "" {
			return errors.New("--token-stdin and --username require --auth bearer or --auth basic")
		}
	case "bearer":
		if *username != "" {
			return errors.New("--username is not valid with --auth bearer")
		}
		if !*tokenStdin {
			return errors.New("--auth bearer requires the token on stdin (--token-stdin)")
		}
	case "basic":
		if *username == "" {
			return errors.New("--auth basic requires --username")
		}
		if !*tokenStdin {
			return errors.New("--auth basic requires the password on stdin (--token-stdin)")
		}
	default:
		return errors.New("--auth must be bearer or basic")
	}

	var token string
	if *tokenStdin {
		// Bound the read so a misdirected pipe cannot exhaust memory; a real token is tiny. Read one
		// byte past the limit so an oversized token is rejected rather than silently truncated into a
		// credential that would always fail to authenticate.
		b, err := io.ReadAll(io.LimitReader(stdin, maxTokenBytes+1))
		if err != nil {
			return fmt.Errorf("reading token from stdin: %w", err)
		}
		if len(b) > maxTokenBytes {
			return fmt.Errorf("token is too long (over %d bytes); pass the token alone on stdin", maxTokenBytes)
		}
		token = strings.TrimRight(string(b), "\r\n")
		if token == "" {
			return errors.New("--token-stdin was set but stdin was empty")
		}
	}

	store, err := gitsync.New(gitsync.DefaultConfigPath())
	if err != nil {
		return err
	}
	if err := store.Set(name, gitsync.LocalEntry{BaseURL: *baseURL, Auth: *auth, Username: *username, Token: gitsync.Secret(token)}); err != nil {
		return err
	}
	fmt.Fprintf(w, "Provider %q saved.\n", name)
	printReloadReminder(w)
	return nil
}

func runConfigList(w io.Writer) error {
	store, err := gitsync.New(gitsync.DefaultConfigPath())
	if err != nil {
		return err
	}
	names := store.Names()
	if len(names) == 0 {
		fmt.Fprintln(w, "No providers configured.")
		return nil
	}
	fmt.Fprintf(w, "%-20s  %-40s  %s\n", "PROVIDER", "BASE URL", "AUTH")
	for _, name := range names {
		e, _ := store.Get(name)
		fmt.Fprintf(w, "%-20s  %-40s  %s\n", name, e.BaseURL, maskedAuth(e))
	}
	return nil
}

func runConfigRemove(args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mkonnect-gitsync connector config remove <name>")
	}
	store, err := gitsync.New(gitsync.DefaultConfigPath())
	if err != nil {
		return err
	}
	if err := store.Remove(args[0]); err != nil {
		return err
	}
	fmt.Fprintf(w, "Provider %q removed.\n", args[0])
	printReloadReminder(w)
	return nil
}

// printProviders prints the replica host pin and the configured providers, the part of `status`
// that needs the provider config to load.
func printProviders(w io.Writer) error {
	// The replica host pin comes first and does not depend on the provider config loading: without
	// it the connector rejects every configuration, which is the first thing an operator must know.
	printReplicaHostPin(w)
	store, err := gitsync.New(gitsync.DefaultConfigPath())
	if err != nil {
		return err
	}
	names := store.Names()
	fmt.Fprintln(w, "=== Configured git-sync providers ===")
	if len(names) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, name := range names {
		e, _ := store.Get(name)
		fmt.Fprintf(w, "  %-20s  %s  [%s]\n", name, e.BaseURL, maskedAuth(e))
	}
	return nil
}

// printReplicaHostPin shows the replica host the connector is pinned to, as read from THIS
// process's environment (the daemon reads the same variable once at startup, so a container started
// with a different value is not reflected here). An unset or invalid value is flagged loudly: the
// connector then rejects every configuration and never syncs. The value is never echoed when
// invalid.
func printReplicaHostPin(w io.Writer) {
	fmt.Fprintln(w, "=== Replica host pin ===")
	host, err := gitsync.ReplicaHostFromEnv()
	switch {
	case err == nil:
		fmt.Fprintf(w, "  %s=%s (from this process's environment)\n", gitsync.ReplicaHostEnv, host)
		fmt.Fprintln(w, "  The connector pushes only to this host and rejects any configuration naming another.")
	case errors.Is(err, gitsync.ErrReplicaHostNotSet):
		fmt.Fprintf(w, "  %s: NOT SET - the connector will reject every configuration and will not sync.\n", gitsync.ReplicaHostEnv)
		fmt.Fprintln(w, "  Set it to the replica host (host or host:port) and restart the connector.")
	default:
		fmt.Fprintf(w, "  %s: INVALID - the connector will reject every configuration and will not sync.\n", gitsync.ReplicaHostEnv)
		fmt.Fprintf(w, "  %v\n", err)
	}
	fmt.Fprintln(w)
}

// maskedAuth describes the authentication mode without ever printing the token. A provider whose
// credential is read from a file says so (the path is not printed), which tells an operator that
// rotating it means updating the file, and that `config set` for this provider would replace it
// with an inline token.
func maskedAuth(e gitsync.LocalEntry) string {
	if e.Auth == "bearer" || e.Auth == "basic" {
		if e.TokenFile != "" {
			return e.Auth + " *** (token_file)"
		}
		return e.Auth + " ***"
	}
	return "(none)"
}

// printReloadReminder tells the operator how a change is applied. The running daemon watches the
// file, so no restart is needed; SIGHUP skips the few seconds it takes to notice.
func printReloadReminder(w io.Writer) {
	fmt.Fprint(w, `
A running git-sync connector picks up this change within a few seconds; it needs no restart. To apply it at once:
  Docker:      docker kill --signal=HUP <container>
  Kubernetes:  kubectl exec deploy/mkonnect-gitsync -- kill -HUP 1
`)
}
