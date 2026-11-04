package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

const checkUsage = `usage: mkonnect-gitsync connector config check

Loads the provider configuration exactly as the connector does at startup and reports every problem
in it, without contacting any server. It prints each provider's canonical host key, authentication
mode and where its credential comes from, but never a credential. Exits non-zero if the connector
would refuse the file. A missing file is not an error at the default path (the connector starts
with no providers) but is at any other.

The file is GIT_SYNC_CONFIG_FILE if set, else /data/git-sync.json. Run it as the user the connector
runs as, with the same environment, so it sees what the connector sees.`

// configPathSource says where the config path came from, so an operator who set the variable in one
// shell and runs the connector in another can see which file was read.
func configPathSource() string {
	if os.Getenv("GIT_SYNC_CONFIG_FILE") != "" {
		return "from GIT_SYNC_CONFIG_FILE"
	}
	return "the default; GIT_SYNC_CONFIG_FILE is not set"
}

func runConfigCheck(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("config check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(w, checkUsage)
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("config check takes no arguments; set GIT_SYNC_CONFIG_FILE to check another file")
	}

	rep := gitsync.CheckConfig(gitsync.DefaultConfigPath())
	printConfigReport(w, rep)
	// A missing file at the default path is normal (the connector starts with no providers, as on a
	// fresh install). At any other path it is almost certainly a typo, and exiting 0 would pass a gate
	// that should fail.
	if rep.Missing && rep.Path != gitsync.DefaultConfigFile {
		return errors.New("the configuration file does not exist; check GIT_SYNC_CONFIG_FILE")
	}
	if n := len(rep.Issues); n > 0 {
		return fmt.Errorf("config check found %d %s; the connector would refuse this configuration", n, plural(n, "problem", "problems"))
	}
	return nil
}

// printConfigReport writes the whole report. Everything printed is either a fixed string, the config
// path, or a field of the report, which holds no value from the file beyond names that passed the
// provider-name rule and canonical host keys.
func printConfigReport(w io.Writer, rep gitsync.ConfigReport) {
	fmt.Fprintln(w, "=== git-sync provider config ===")
	fmt.Fprintf(w, "  File: %s (%s)\n", rep.Path, configPathSource())
	fmt.Fprintf(w, "  Checked as uid %d; the connector must run as a user that can read this file.\n", os.Geteuid())
	if rep.Missing {
		fmt.Fprintln(w, "  The file does not exist. The connector starts with no providers, so every repository is reported as not host-matched.")
	}

	fmt.Fprintln(w, "\n=== Providers ===")
	if len(rep.Providers) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tHOST KEY\tAUTH\tCREDENTIAL")
		for _, p := range rep.Providers {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", p.Name, p.HostKey, authMode(p), credentialSource(p))
		}
		_ = tw.Flush()
	}

	if len(rep.Warnings) > 0 {
		fmt.Fprintln(w, "\n=== Warnings ===")
		for _, msg := range rep.Warnings {
			fmt.Fprintf(w, "  - %s\n", msg)
		}
	}

	if rep.Missing {
		return
	}
	if len(rep.Issues) == 0 {
		fmt.Fprintf(w, "\nOK: %d %s valid.\n", len(rep.Providers), plural(len(rep.Providers), "provider is", "providers are"))
		return
	}
	fmt.Fprintln(w, "\n=== Errors ===")
	for _, issue := range rep.Issues {
		fmt.Fprintf(w, "  %s\n", describeIssue(issue))
	}
}

// describeIssue renders one issue as `[class] provider "x" (field f): message`.
func describeIssue(i gitsync.ConfigIssue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] ", i.Class)
	switch i.Class {
	case gitsync.IssueSyntax, gitsync.IssueSchema:
		b.WriteString("the config file ")
	}
	if i.Provider != "" {
		fmt.Fprintf(&b, "provider %q", i.Provider)
		if i.Field != "" {
			fmt.Fprintf(&b, " (field %s)", i.Field)
		}
		b.WriteString(": ")
	}
	b.WriteString(i.Message)
	return b.String()
}

// authMode names the authentication mode.
func authMode(p gitsync.ProviderReport) string {
	if p.Auth == "" {
		return "none"
	}
	return p.Auth
}

// credentialSource says where a provider's credential comes from, never what it is. Today that is
// the config file itself; a provider that does not authenticate has none.
func credentialSource(p gitsync.ProviderReport) string {
	if p.HasToken {
		return "inline token"
	}
	return "-"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
