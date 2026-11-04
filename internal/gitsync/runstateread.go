package gitsync

import "path/filepath"

// RunRecord is what the connector remembers about one repository's attempts, as persisted in the run
// state file. It is exported so the operator CLI reads the daemon's own type rather than a copy that
// could drift. Records handed out by ReadRunState carry no Target: the sync-target fingerprint is
// bookkeeping for the daemon, not something to report.
type RunRecord = runRecord

// absRoot makes a mirrors root absolute, as the daemon does for the root it was configured with, so
// a relative GIT_SYNC_MIRRORS_DIR means the same directory to the daemon and to the operator CLI
// when both are started from the same working directory.
func absRoot(root string) string {
	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}
	return root
}

// ReadRunState reads the run state file from the same directory the daemon keeps it in. It is for
// the operator CLI: it never writes, removes, logs, or contacts anything, and it applies exactly the
// trust checks the daemon applies at startup, so it reports what the daemon would load. The records
// are those valid entries, by repo_id, with Target cleared; dropped counts the entries that were
// invalid. A file that cannot be used yields no records and the reason as problem; a missing file is
// RunStateMissing. The file lags the daemon by up to a few seconds, and an attempt in progress is
// not in it.
func ReadRunState() (records map[string]RunRecord, dropped int, problem RunStateProblem) {
	return readRunStateAt(absRoot(DefaultMirrorsRoot()))
}

func readRunStateAt(root string) (map[string]RunRecord, int, RunStateProblem) {
	records, dropped, problem := newRunStateStore(root).read()
	for id, rec := range records {
		rec.Target = ""
		records[id] = rec
	}
	return records, dropped, problem
}
