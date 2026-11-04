package gitsync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// This file persists what the connector knows about each repository's last attempts, so a restart
// does not reset a repository to "never succeeded": last_success_at says when the replica was last
// brought up to date, which must outlive the process that did it.
//
// The state is one small JSON file in the mirrors root. It holds timestamps, counters and a branch
// name: no credential, URL, host, path or error text. Its target field is a hash of the sync target,
// not the URLs themselves.

const (
	// runStateFile is the state file's name within the mirrors root. A mirror directory is a 64-hex
	// name ending in ".git", so the two cannot collide.
	runStateFile = "run-state.json"
	// runStateTempPrefix names the temporary file a write goes through. A crash between creating it and
	// renaming it leaves it behind; the next load removes such leftovers, and nothing else.
	runStateTempPrefix = ".run-state.json.tmp-"
	// runStateVersion is the file format. A file of any other version is ignored, not interpreted.
	runStateVersion = 1
	// maxRunStateBytes bounds the file read and written. The largest state a configuration can produce
	// (maxConfigRepos entries, each with a maxRepoIDLength id and a maxBranchNameLength branch name
	// that needs escaping in every byte) is under 5 MiB, so this leaves headroom while still bounding
	// what a corrupt or hostile file can make the connector read.
	maxRunStateBytes = 8 << 20
)

// runRecord is what is remembered about one repository. Every field is optional; the zero value says
// nothing is known. Times are UTC.
type runRecord struct {
	// Target fingerprints the origin and replica the record was made against (see targetFingerprint).
	// A record whose target differs from the repository's current one describes a different
	// replication and is discarded; a record without one cannot be attributed and is invalid.
	Target string `json:"target,omitempty"`
	// LastAttemptAt is when the most recent finished attempt started.
	LastAttemptAt time.Time `json:"last_attempt_at,omitzero"`
	// LastSuccessAt is when the most recent successful attempt finished. A run that found nothing new
	// is a success. It is carried across failures until the next success.
	LastSuccessAt time.Time `json:"last_success_at,omitzero"`
	// DurationMS is the wall time of the most recent finished attempt, success or failure.
	DurationMS *int64 `json:"duration_ms,omitempty"`
	// RefsPushed is how many refs the most recent finished attempt created, updated or deleted on the
	// replica. Zero is a valid value.
	RefsPushed *int `json:"refs_pushed,omitempty"`
	// NewestOriginCommitAt and OriginDefaultBranch are the last values an attempt learned. An attempt
	// that could not learn them leaves the previous ones.
	NewestOriginCommitAt time.Time `json:"newest_origin_commit_at,omitzero"`
	OriginDefaultBranch  string    `json:"origin_default_branch,omitempty"`
}

// targetFingerprint identifies the origin and replica a record was made against without storing
// either URL. The lengths keep the two parts from trading characters across the boundary.
func targetFingerprint(canonicalOrigin, replicaURL string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s", len(canonicalOrigin), canonicalOrigin, len(replicaURL), replicaURL)))
	return hex.EncodeToString(sum[:])
}

// valid reports whether a record read from disk is sane. A file that was edited or damaged must not
// put an impossible value into a status.
func (r runRecord) valid() bool {
	if b, err := hex.DecodeString(r.Target); err != nil || len(b) != sha256.Size || r.Target != strings.ToLower(r.Target) {
		return false
	}
	for _, t := range []time.Time{r.LastAttemptAt, r.LastSuccessAt, r.NewestOriginCommitAt} {
		if !t.IsZero() && (t.Unix() <= 0 || t.Unix() > maxCommitUnix) {
			return false
		}
	}
	if r.DurationMS != nil && (*r.DurationMS < 0 || *r.DurationMS > 1<<40) {
		return false
	}
	if r.RefsPushed != nil && (*r.RefsPushed < 0 || *r.RefsPushed > 1<<30) {
		return false
	}
	return r.OriginDefaultBranch == "" || validBranchName(r.OriginDefaultBranch)
}

// stateOps are the filesystem steps of a write that a test replaces to inject a failure.
type stateOps struct {
	rename  func(oldpath, newpath string) error
	syncDir func(dir string) error
}

// runStateStore is the on-disk side of the run state: it loads the file at startup and writes
// snapshots of the in-memory records back. It holds no records itself; the client owns them under its
// own lock, so a status is always built from one consistent view.
type runStateStore struct {
	root string
	path string
	ops  stateOps

	// writeMu serialises writes, and the snapshot is taken only once it is held, so an older snapshot
	// can never be written after a newer one.
	writeMu sync.Mutex
}

func newRunStateStore(root string) *runStateStore {
	return &runStateStore{
		root: root,
		path: filepath.Join(root, runStateFile),
		ops:  stateOps{rename: os.Rename, syncDir: syncDir},
	}
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// RunStateProblem says why a run state file yielded no usable records. The values are logged by the
// daemon and reported by the operator CLI (including in its JSON), so they are part of the surface:
// add new ones, never rename.
type RunStateProblem string

const (
	// RunStateOK means the file was read; individual entries may still have been dropped.
	RunStateOK RunStateProblem = ""
	// RunStateMissing means there is no file: the connector has not recorded an attempt here.
	RunStateMissing        RunStateProblem = "missing"
	RunStateEmpty          RunStateProblem = "empty"
	RunStateCorrupt        RunStateProblem = "corrupt"
	RunStateUnsupported    RunStateProblem = "unsupported_version"
	RunStateTooManyEntries RunStateProblem = "too_many_entries"
	RunStateOversized      RunStateProblem = "oversized"
	RunStateWorldWritable  RunStateProblem = "world_writable"
	RunStateNotRegular     RunStateProblem = "not_regular"
	RunStateUnreadable     RunStateProblem = "unreadable"
)

// load reads the state file. It never fails: a missing file is empty state, and a file that cannot be
// trusted (unreadable, not a regular file, writable by anyone, oversized, corrupt, of another
// version, or holding too many entries) is logged by reason and treated as empty, so the connector
// starts and syncs either way. Entries that are individually invalid are dropped and counted. Nothing
// read from the file is logged. It also removes the temporary files a crashed write left behind.
func (s *runStateStore) load() map[string]runRecord {
	s.sweepTemps()
	records, dropped, problem := s.read()
	switch problem {
	case RunStateOK:
	case RunStateMissing:
		return map[string]runRecord{}
	default:
		return ignoreRunState(string(problem))
	}
	if dropped > 0 {
		slog.Warn("dropped invalid run state entries", "dropped", dropped)
	}
	return records
}

// read is load without its side effects: it neither removes files nor logs, so the operator CLI can
// use it while the daemon runs. It reports why nothing could be used as a problem, and otherwise the
// valid records and how many entries were dropped as invalid. Records is never nil.
func (s *runStateStore) read() (records map[string]runRecord, dropped int, problem RunStateProblem) {
	// O_NOFOLLOW refuses a symlink and O_NONBLOCK keeps a FIFO from blocking the open; every check is
	// then made on the open descriptor, so nothing can be swapped in between a check and the read.
	f, err := os.OpenFile(s.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return map[string]runRecord{}, 0, RunStateMissing
		case errors.Is(err, syscall.ELOOP):
			return map[string]runRecord{}, 0, RunStateNotRegular
		}
		return map[string]runRecord{}, 0, RunStateUnreadable
	}
	defer f.Close()

	info, err := f.Stat()
	switch {
	case err != nil:
		return map[string]runRecord{}, 0, RunStateUnreadable
	case !info.Mode().IsRegular():
		return map[string]runRecord{}, 0, RunStateNotRegular
	// Anyone being able to write the file means its content may not be ours. Group-writable is
	// accepted: a volume mounted with an fsGroup (as Kubernetes does) is, and refusing it would
	// discard the state at every restart. The content is timestamps and a branch name.
	case info.Mode().Perm()&0o002 != 0:
		return map[string]runRecord{}, 0, RunStateWorldWritable
	case info.Size() > maxRunStateBytes:
		return map[string]runRecord{}, 0, RunStateOversized
	}
	// The size can change between Stat and read, so the read is bounded too.
	data, err := io.ReadAll(io.LimitReader(f, maxRunStateBytes+1))
	if err != nil {
		return map[string]runRecord{}, 0, RunStateUnreadable
	}
	if len(data) > maxRunStateBytes {
		return map[string]runRecord{}, 0, RunStateOversized
	}
	if len(data) == 0 {
		return map[string]runRecord{}, 0, RunStateEmpty
	}

	var file struct {
		Version      int                        `json:"version"`
		Repositories map[string]json.RawMessage `json:"repositories"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return map[string]runRecord{}, 0, RunStateCorrupt
	}
	if file.Version != runStateVersion {
		return map[string]runRecord{}, 0, RunStateUnsupported
	}
	if len(file.Repositories) > maxConfigRepos {
		return map[string]runRecord{}, 0, RunStateTooManyEntries
	}
	records = make(map[string]runRecord, len(file.Repositories))
	for id, raw := range file.Repositories {
		var rec runRecord
		if !validRepoID(id) || json.Unmarshal(raw, &rec) != nil || !rec.valid() {
			dropped++
			continue
		}
		records[id] = rec
	}
	return records, dropped, RunStateOK
}

func ignoreRunState(reason string) map[string]runRecord {
	slog.Warn("ignoring run state file", "reason", reason)
	return map[string]runRecord{}
}

// sweepTemps removes the leftovers of a write that crashed before its rename. Only regular files
// carrying this file's own temp prefix are touched: the mirrors root also holds the mirrors.
func (s *runStateStore) sweepTemps() {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), runStateTempPrefix) {
			_ = os.Remove(filepath.Join(s.root, e.Name()))
		}
	}
}

// save writes the records snapshot returns, atomically: a temporary file in the same directory is
// written, synced and renamed over the state file, and the directory is synced, so a crash leaves
// either the old file or the new one, never a partial one. A failure leaves the old file untouched,
// is logged (by step, never with a path) and returned for the caller to ignore: a sync must not
// fail because its bookkeeping could not be saved. A state larger than load would accept is refused
// for the same reason it would be lost: a file that cannot be read back is worse than the old one.
func (s *runStateStore) save(snapshot func() map[string]runRecord) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	records := snapshot()
	data, err := encodeRunState(records)
	if err != nil {
		return writeFailed("encode", err)
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return writeFailed("mkdir", err)
	}
	tmp, err := os.CreateTemp(s.root, runStateTempPrefix+"*")
	if err != nil {
		return writeFailed("create", err)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return writeFailed("chmod", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return writeFailed("write", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return writeFailed("sync", err)
	}
	if err := tmp.Close(); err != nil {
		return writeFailed("close", err)
	}
	if err := s.ops.rename(tmp.Name(), s.path); err != nil {
		return writeFailed("rename", err)
	}
	renamed = true
	if err := s.ops.syncDir(s.root); err != nil {
		return writeFailed("dir_sync", err)
	}
	return nil
}

var errRunStateTooLarge = errors.New("run state exceeds the size the loader accepts")

// encodeRunState renders records as compact JSON without HTML escaping (which would expand a "<" to
// six bytes and let a legitimate state outgrow the loader's bound).
func encodeRunState(records map[string]runRecord) ([]byte, error) {
	if len(records) > maxConfigRepos {
		return nil, errRunStateTooLarge
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		Version      int                  `json:"version"`
		Repositories map[string]runRecord `json:"repositories"`
	}{runStateVersion, records}); err != nil {
		return nil, err
	}
	if buf.Len() > maxRunStateBytes {
		return nil, errRunStateTooLarge
	}
	return buf.Bytes(), nil
}

// writeFailed logs a failed write by step and, when there is one, the operating system's reason
// (which names no path), and returns the error. The error itself is not logged: it can carry the path.
func writeFailed(step string, err error) error {
	attrs := []any{"step", step}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		attrs = append(attrs, "errno", errno.Error())
	}
	slog.Warn("run state write failed", attrs...)
	return fmt.Errorf("run state %s: %w", step, err)
}
