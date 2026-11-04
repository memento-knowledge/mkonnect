package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

const statusUsage = `usage: mkonnect-gitsync connector status [--json] [--absolute]

Shows the replica host pin, the configured providers, and per-repository sync state as last
recorded by the connector (read from its run-state file; the connector need not be running).
  --json      print only the sync state, as a single JSON object with RFC 3339 UTC timestamps
  --absolute  print RFC 3339 UTC timestamps in the table instead of relative ones`

// statusJSONVersion is the version of the --json document. Fields are only ever added within a
// version.
const statusJSONVersion = 1

// now is the clock relative timestamps are measured against; a test replaces it.
var now = time.Now

func runStatus(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print the sync state as JSON")
	absolute := fs.Bool("absolute", false, "print timestamps as RFC 3339 instead of relative")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(w, statusUsage)
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), statusUsage)
	}

	records, dropped, problem := gitsync.ReadRunState()
	if *asJSON {
		return writeRunStateJSON(w, records, dropped, problem)
	}

	if err := printProviders(w); err != nil {
		return err
	}
	printRunState(w, records, dropped, problem, *absolute)
	// The CLI and the daemon are separate processes and do not share live state, so enrollment and
	// the reasons attempts failed are shown in the portal, not here.
	fmt.Fprintln(w, "\nEnrollment status and failure reasons are visible in the Memento portal.")
	return nil
}

// problemText says what an unusable run state file means to the operator. The connector ignores such
// a file and starts from nothing, so every message ends in the same consequence.
var problemText = map[gitsync.RunStateProblem]string{
	gitsync.RunStateEmpty:          "the file is empty",
	gitsync.RunStateCorrupt:        "the file is not valid run state",
	gitsync.RunStateUnsupported:    "the file is from an unsupported version",
	gitsync.RunStateTooManyEntries: "the file has more repositories than the connector accepts",
	gitsync.RunStateOversized:      "the file is larger than the connector accepts",
	gitsync.RunStateWorldWritable:  "the file is writable by anyone",
	gitsync.RunStateNotRegular:     "the path is not a regular file",
	gitsync.RunStateUnreadable:     "the file cannot be read",
}

func printRunState(w io.Writer, records map[string]gitsync.RunRecord, dropped int, problem gitsync.RunStateProblem, absolute bool) {
	fmt.Fprintln(w, "\n=== Repository sync state ===")
	switch problem {
	case gitsync.RunStateOK:
	case gitsync.RunStateMissing:
		fmt.Fprintln(w, "  No run state recorded yet: the connector has not finished a sync on this volume.")
		return
	default:
		fmt.Fprintf(w, "  Run state unusable (%s): %s. The connector ignores it and records new state after its next sync.\n", problem, problemText[problem])
		return
	}
	if dropped > 0 {
		fmt.Fprintf(w, "  Ignored invalid run state entries: %d.\n", dropped)
	}
	if len(records) == 0 {
		fmt.Fprintln(w, "  (no repositories recorded)")
		return
	}

	at := now()
	when := func(t time.Time) string {
		switch {
		case t.IsZero():
			return "-"
		case absolute:
			return t.UTC().Format(time.RFC3339)
		}
		return relative(at, t)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  REPOSITORY\tLAST ATTEMPT\tLAST SUCCESS\tDURATION\tREFS PUSHED\tNEWEST ORIGIN COMMIT\tORIGIN DEFAULT BRANCH")
	for _, id := range slices.Sorted(maps.Keys(records)) {
		rec := records[id]
		success := "never"
		if !rec.LastSuccessAt.IsZero() {
			success = when(rec.LastSuccessAt)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", id, when(rec.LastAttemptAt), success,
			duration(rec.DurationMS), count(rec.RefsPushed), when(rec.NewestOriginCommitAt), orDash(rec.OriginDefaultBranch))
	}
	_ = tw.Flush()
	fmt.Fprintln(w, `
  As last recorded by the connector after each finished attempt: it can lag by a few seconds, omits
  attempts in progress and repositories not yet attempted, and may list repositories no longer
  configured. LAST SUCCESS is kept across failed attempts, so a LAST ATTEMPT later than it means the
  latest attempt did not succeed.`)
}

// relative renders how long before at t was, to one unit.
func relative(at, t time.Time) string {
	age := at.Sub(t)
	switch {
	case age < 0:
		return "in the future"
	case age < time.Second:
		return "just now"
	case age < time.Minute:
		return fmt.Sprintf("%ds ago", int(age/time.Second))
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age/time.Minute))
	case age < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(age/(24*time.Hour)))
}

func duration(ms *int64) string {
	if ms == nil {
		return "-"
	}
	return (time.Duration(*ms) * time.Millisecond).String()
}

func count(n *int) string {
	if n == nil {
		return "-"
	}
	return fmt.Sprint(*n)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// statusJSON is the --json document. It is the CLI's own type, never the persisted record, so that
// what is printed is chosen field by field. Every field is always present; unknown values are null.
type statusJSON struct {
	Version  int          `json:"version"`
	RunState runStateJSON `json:"run_state"`
}

type runStateJSON struct {
	// Status is "ok" or the gitsync.RunStateProblem that made the file unusable.
	Status string `json:"status"`
	// Dropped is how many entries of a usable file were invalid and ignored.
	Dropped      int        `json:"dropped"`
	Repositories []repoJSON `json:"repositories"`
}

type repoJSON struct {
	RepoID               string  `json:"repo_id"`
	LastAttemptAt        *string `json:"last_attempt_at"`
	LastSuccessAt        *string `json:"last_success_at"`
	DurationMS           *int64  `json:"duration_ms"`
	RefsPushed           *int    `json:"refs_pushed"`
	NewestOriginCommitAt *string `json:"newest_origin_commit_at"`
	OriginDefaultBranch  *string `json:"origin_default_branch"`
}

func writeRunStateJSON(w io.Writer, records map[string]gitsync.RunRecord, dropped int, problem gitsync.RunStateProblem) error {
	doc := statusJSON{Version: statusJSONVersion, RunState: runStateJSON{Status: "ok", Dropped: dropped, Repositories: []repoJSON{}}}
	if problem != gitsync.RunStateOK {
		doc.RunState.Status = string(problem)
	}
	for _, id := range slices.Sorted(maps.Keys(records)) {
		rec := records[id]
		r := repoJSON{
			RepoID:               id,
			LastAttemptAt:        rfc3339(rec.LastAttemptAt),
			LastSuccessAt:        rfc3339(rec.LastSuccessAt),
			DurationMS:           rec.DurationMS,
			RefsPushed:           rec.RefsPushed,
			NewestOriginCommitAt: rfc3339(rec.NewestOriginCommitAt),
		}
		if rec.OriginDefaultBranch != "" {
			r.OriginDefaultBranch = &rec.OriginDefaultBranch
		}
		doc.RunState.Repositories = append(doc.RunState.Repositories, r)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(doc)
}

func rfc3339(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}
