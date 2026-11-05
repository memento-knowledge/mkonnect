package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

const preflightUsage = `usage: mkonnect-gitsync connector preflight [--json]

Checks, before the connector is enrolled, that this host can run it. It reports PASS, WARN or FAIL
for each check with how to fix it, and exits non-zero if any check FAILs:
  git            git is installed and is version 2.31 or newer
  tmpdir         the temporary directory (GIT_SYNC_TMP_DIR, else TMPDIR) exists and is writable
  replica-host   GIT_SYNC_REPLICA_HOST is set and well formed
  gateway-url    GATEWAY_URL is a valid gateway URL
  gateway-dns    the gateway's name resolves
  gateway-tls    a TLS connection to the gateway can be made, through the same proxy and with the
                 same trust store the connector uses
  provider-config  the provider configuration is one the connector accepts
  origin:<name>  each configured git server answers (git ls-remote against its base URL)

It does not enrol, authenticate to the gateway, send connector data, or contact the Memento replica
(its only request to the platform is one body-less, unauthenticated HEAD to the gateway), and it is
not a dry-run sync. A provider's credential is sent only to that provider's own git server. It
never prints a credential.
  --json  print the result as a single JSON object instead of text

Run it as the user the connector runs as, with the connector's environment and the same volumes,
from the place the connector will run (for Kubernetes, inside the pod: kubectl exec deploy/<name> --
/mkonnect-gitsync connector preflight). A container started only for this check has its own network policy, DNS and
proxy settings, and may not see what the connector will see.`

// preflightJSONVersion is the version of the --json document. Fields are only ever added within a
// version.
const preflightJSONVersion = 1

// preflightTimeout bounds the whole run: the gateway check, and the origin probes, each of which
// has its own shorter limit.
const preflightTimeout = 3 * time.Minute

type checkStatus string

const (
	statusPass checkStatus = "pass"
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "fail"
	// statusSkip is a check that was not run because an earlier one it depends on failed. It is not a
	// pass: the reason says what to fix first.
	statusSkip checkStatus = "skip"
)

// checkResult is one line of the report. Detail and Remedy are built only from fixed strings, host
// names and ports, versions, and provider names and canonical host keys that passed the config
// rules; never from a credential or from a raw error that could echo a URL.
type checkResult struct {
	ID     string
	Status checkStatus
	Detail string
	Remedy string
}

// preflightEnv holds what a test replaces: the origin prober, and the pieces of the gateway check
// that read process-wide state Go caches (the proxy environment, the system trust store).
type preflightEnv struct {
	prober     gitsync.OriginProber
	transport  func() *http.Transport
	lookupHost func(ctx context.Context, host string) ([]string, error)
}

func defaultPreflightEnv() preflightEnv {
	return preflightEnv{
		prober:     gitsync.NewGitProber(),
		transport:  newGatewayTransport,
		lookupHost: net.DefaultResolver.LookupHost,
	}
}

// newPreflightEnv builds the production environment; a test replaces it.
var newPreflightEnv = defaultPreflightEnv

func runPreflight(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(w, preflightUsage)
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), preflightUsage)
	}

	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()
	results := runPreflightChecks(ctx, newPreflightEnv())

	counts := tally(results)
	if *asJSON {
		if err := writePreflightJSON(w, results, counts); err != nil {
			return err
		}
	} else {
		printPreflight(w, results, counts)
	}
	if n := counts[statusFail]; n > 0 {
		return fmt.Errorf("preflight found %d failing %s", n, plural(n, "check", "checks"))
	}
	return nil
}

// runPreflightChecks runs every check, in the order they are reported. A check whose prerequisite
// failed is skipped with the reason, rather than run to produce a misleading second failure.
func runPreflightChecks(ctx context.Context, env preflightEnv) []checkResult {
	var out []checkResult
	add := func(r ...checkResult) { out = append(out, r...) }

	gitRes := checkGit(ctx)
	tmpRes := checkTempDir()
	add(gitRes, tmpRes, checkReplicaHost())
	add(checkGateway(ctx, env)...)

	cfg := gitsync.ReadConfig(gitsync.DefaultConfigPath())
	cfgRes := checkProviderConfig(cfg.Report)
	add(cfgRes)

	// The origin probe runs git, in a temp home it creates under the temp root (creating the root
	// too, were it missing, as whatever user runs this). Probing only when both are known to work also
	// keeps a missing git from being reported once per provider.
	switch {
	case cfgRes.Status == statusFail:
		add(checkResult{ID: "origins", Status: statusSkip, Detail: "not checked: the provider configuration has a problem (see provider-config)"})
	case gitRes.Status != statusPass:
		add(checkResult{ID: "origins", Status: statusSkip, Detail: "not checked: git is not usable (see git)"})
	case tmpRes.Status != statusPass:
		add(checkResult{ID: "origins", Status: statusSkip, Detail: "not checked: the temporary directory is not usable (see tmpdir)"})
	default:
		add(checkOrigins(ctx, env.prober, cfg)...)
	}
	return out
}

func tally(results []checkResult) map[checkStatus]int {
	counts := map[checkStatus]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	return counts
}

func printPreflight(w io.Writer, results []checkResult, counts map[checkStatus]int) {
	fmt.Fprintln(w, "=== git-sync preflight ===")
	fmt.Fprintf(w, "  Checked as uid %d, with this process's environment. Nothing is sent to the Memento platform,\n", os.Geteuid())
	fmt.Fprintln(w, "  no credential is sent anywhere but to the git server it belongs to, and the replica is not contacted.")
	fmt.Fprintln(w)
	for _, r := range results {
		label := strings.ToUpper(string(r.Status))
		lines := strings.Split(r.Detail, "\n")
		fmt.Fprintf(w, "  %-4s  %-16s %s\n", label, r.ID, lines[0])
		for _, l := range lines[1:] {
			fmt.Fprintf(w, "        %-16s %s\n", "", l)
		}
		if r.Remedy != "" {
			fmt.Fprintf(w, "        %-16s Fix: %s\n", "", r.Remedy)
		}
	}
	fmt.Fprintf(w, "\nSummary: %d passed, %d %s, %d failed, %d skipped.\n",
		counts[statusPass], counts[statusWarn], plural(counts[statusWarn], "warning", "warnings"), counts[statusFail], counts[statusSkip])
	switch {
	case counts[statusFail] > 0:
		fmt.Fprintln(w, "Not ready: fix the failed checks and run preflight again.")
	case counts[statusSkip] > 0:
		fmt.Fprintln(w, "Some checks were skipped, so this host is not yet shown to be ready.")
	default:
		fmt.Fprintln(w, "No failures. The replica push is not part of this check; once enrolled, it is reported by `connector status` and in the portal.")
	}
}

// preflightJSON is the --json document. It is the CLI's own type so what is printed is chosen field
// by field. Every field is always present; a check with no remedy has a null one.
type preflightJSON struct {
	Version int                 `json:"version"`
	Ok      bool                `json:"ok"`
	Summary preflightJSONCounts `json:"summary"`
	Checks  []checkJSON         `json:"checks"`
}

type preflightJSONCounts struct {
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
}

type checkJSON struct {
	ID     string  `json:"id"`
	Status string  `json:"status"`
	Detail string  `json:"detail"`
	Remedy *string `json:"remedy"`
}

func writePreflightJSON(w io.Writer, results []checkResult, counts map[checkStatus]int) error {
	doc := preflightJSON{
		Version: preflightJSONVersion,
		Ok:      counts[statusFail] == 0,
		Summary: preflightJSONCounts{Pass: counts[statusPass], Warn: counts[statusWarn], Fail: counts[statusFail], Skip: counts[statusSkip]},
		Checks:  make([]checkJSON, 0, len(results)),
	}
	for _, r := range results {
		c := checkJSON{ID: r.ID, Status: string(r.Status), Detail: r.Detail}
		if r.Remedy != "" {
			remedy := r.Remedy
			c.Remedy = &remedy
		}
		doc.Checks = append(doc.Checks, c)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(doc)
}
