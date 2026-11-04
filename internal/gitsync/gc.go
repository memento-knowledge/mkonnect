package gitsync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Mirror garbage collection reclaims the disk of mirrors whose repository left the configuration.
//
// Deleting is the one destructive thing this connector does, so the design is deliberately narrow:
//
//   - Only direct children of the mirrors root named exactly like Mirrors.Dir output (64 lowercase hex
//     characters plus ".git") that are real directories are ever candidates. Symlinks, files and any
//     other entry (run-state.json, …) are never followed, modified or removed.
//   - A mirror is an orphan when it is not the mirror of any repository in the CURRENTLY ACCEPTED
//     configuration. It is removed only once it has been an orphan for mirrorGCGrace, measured from when
//     this connector first saw it orphaned. Returning to the configuration clears that mark.
//   - The orphan mark is a small file inside the mirror directory, so it survives a restart and
//     disappears together with the directory. It is written atomically and owner-only; anything
//     unreadable, malformed, oversized or dated in the future counts as "first seen now".
//   - Every filesystem operation goes through an os.Root opened on the mirrors root, which confines path
//     resolution to it: no ".." or symlink in a path can lead outside.
//   - A due mirror is first moved atomically into a trash directory inside the root, and only the moved
//     directory is removed. The move happens while no sync of that repository is queued or running and
//     the repository is still not configured (see manager.claimMirror), so a sync that starts afterwards
//     initialises a fresh directory instead of fetching into one that is being deleted. A removal that
//     fails or is interrupted leaves the trash entry, which the next sweep removes.
//
// The grace period relies on a sane wall clock: a clock that was wrong when a mark was written
// shortens or lengthens it accordingly.
const (
	// mirrorGCGrace is how long a mirror must have been orphaned before it is removed.
	mirrorGCGrace = 7 * 24 * time.Hour

	// orphanMarkerName is the file inside an orphaned mirror that records when it was first seen
	// orphaned (RFC 3339, UTC).
	orphanMarkerName = ".mkonnect-orphaned"
	// markerTmpName is the fixed temporary name a marker is written under before it is renamed into
	// place, so a crash cannot leave a half-written marker and a stray temporary is always replaced.
	markerTmpName = orphanMarkerName + ".tmp"
	// maxMarkerSize bounds how much of a marker is read. An RFC 3339 timestamp with nanoseconds is
	// at most 35 bytes.
	maxMarkerSize = 64

	// gcTrashDirName is where a due mirror is moved before it is removed. It lives inside the mirrors
	// root so the move is an atomic rename on the same filesystem.
	gcTrashDirName = ".mkonnect-gc-trash"
)

// mirrorGCClockFloor is the earliest time the wall clock may read for a sweep to trust it. A host that
// boots with an unset clock (the epoch) would otherwise write marks that look ancient once the clock is
// corrected, expiring the grace period at once. Before the floor nothing is swept, and a mark dated
// before it is rewritten rather than believed, so a wrong clock can only delay removal.
var mirrorGCClockFloor = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

var (
	// mirrorDirRE matches exactly the directory names Mirrors.Dir produces.
	mirrorDirRE = regexp.MustCompile(`^[0-9a-f]{64}\.git$`)
	// tombstoneRE matches exactly the names a mirror gets in the trash directory.
	tombstoneRE = regexp.MustCompile(`^[0-9a-f]{64}\.git\.[0-9a-f]{16}$`)

	// errUnsafeTrash reports a trash path that exists but is not a real directory.
	errUnsafeTrash = errors.New("trash path is not a real directory")
)

// sweepResult counts what one sweep did, for the summary log and for tests.
type sweepResult struct {
	Marked  int // orphans first seen (or with an unusable mark), now marked
	Cleared int // marks removed because the repository is back in the configuration
	Busy    int // orphans left alone because a sync is queued/running or the repository returned
	Removed int // mirrors removed (including leftovers from an earlier sweep)
	Skipped int // candidates or trash entries that were not safe to touch
	Failed  int // operations that failed and will be retried by a later sweep
}

// sweepParams carries everything a sweep depends on, so the core has no hidden inputs: it is given the
// time, the configured set, and the hooks for the parts that need coordination or are destructive.
type sweepParams struct {
	// Root is the mirrors root. It must be an absolute path other than a filesystem root; it may
	// itself be a symlink.
	Root string
	// Keep is the set of mirror directory names (as Mirrors.Dir produces) of every repository in the
	// accepted configuration. The caller must never run a sweep with an empty set.
	Keep  map[string]struct{}
	Now   time.Time
	Grace time.Duration
	// Busy reports whether a sync of the mirror is queued or running. Such a mirror is neither marked
	// nor removed.
	Busy func(name string) bool
	// LastConfigured reports when the mirror was last seen in an accepted configuration. A mark older
	// than that is stale: the repository came back after it was written. May be nil.
	LastConfigured func(name string) (time.Time, bool)
	// Claim runs tombstone, which moves the mirror aside, atomically with respect to a sync starting
	// for it, and reports whether it ran. It must decline (false, nil) when a sync is queued or running
	// or the repository is configured again.
	Claim func(name string, tombstone func() error) (claimed bool, err error)
	// Remove removes name (a path relative to the root) and everything below it.
	Remove func(r *os.Root, name string) error
	Log    *slog.Logger
}

// sweepMirrors runs one garbage-collection pass over the mirrors root. It never returns an error: a
// problem with one candidate is logged and counted, and retried by a later sweep.
func sweepMirrors(ctx context.Context, p sweepParams) sweepResult {
	var res sweepResult
	if p.Log == nil {
		p.Log = slog.Default()
	}
	if p.Now.Before(mirrorGCClockFloor) {
		p.Log.Warn("mirror cleanup skipped: the system clock reads an implausibly early time")
		return res
	}
	clean := filepath.Clean(p.Root)
	if !filepath.IsAbs(clean) || filepath.Dir(clean) == clean {
		p.Log.Warn("mirror cleanup skipped: the mirrors root is not an absolute path below a filesystem root")
		return res
	}
	root, err := os.OpenRoot(clean)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			p.Log.Warn("mirror cleanup skipped: cannot open the mirrors root", "reason", errReason(err))
		}
		return res
	}
	defer root.Close()

	s := &sweeper{p: p, root: root, res: &res}
	s.recoverTombstones(ctx)
	for _, name := range s.mirrorCandidates() {
		if ctx.Err() != nil {
			break
		}
		s.sweepOne(name)
	}
	return res
}

type sweeper struct {
	p    sweepParams
	root *os.Root
	res  *sweepResult
}

// mirrorCandidates lists the root's entries whose names have the mirror format, sorted. The type of an
// entry is checked separately, by Lstat, in sweepOne.
func (s *sweeper) mirrorCandidates() []string {
	var out []string
	for _, n := range s.listNames(".") {
		if mirrorDirRE.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}

func (s *sweeper) listNames(dir string) []string {
	f, err := s.root.Open(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.p.Log.Warn("mirror cleanup: cannot list a directory", "reason", errReason(err))
		}
		return nil
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		s.p.Log.Warn("mirror cleanup: cannot list a directory", "reason", errReason(err))
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func (s *sweeper) sweepOne(name string) {
	fi, err := s.root.Lstat(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.res.Failed++
			s.p.Log.Warn("mirror cleanup: cannot inspect a mirror", "mirror", name, "reason", errReason(err))
		}
		return
	}
	if fi.Mode().Type() != fs.ModeDir {
		s.res.Skipped++
		s.p.Log.Warn("mirror cleanup: skipping an entry named like a mirror that is not a real directory", "mirror", name, "kind", kindOf(fi.Mode()))
		return
	}
	if _, configured := s.p.Keep[name]; configured {
		s.clearMarker(name)
		return
	}
	if s.p.Busy(name) {
		s.res.Busy++
		s.p.Log.Debug("mirror cleanup: orphan has a sync queued or running; leaving it", "mirror", name)
		return
	}
	since, ok := s.readMarker(name)
	if !ok {
		s.writeMarker(name)
		return
	}
	if s.p.LastConfigured != nil {
		if last, seen := s.p.LastConfigured(name); seen && since.Before(last) {
			// The repository was configured again after this mark was written, so the mark describes an
			// earlier absence; the grace period starts over.
			s.writeMarker(name)
			return
		}
	}
	if age := s.p.Now.Sub(since); age >= s.p.Grace {
		s.removeDue(name, age)
	}
}

// removeDue moves a mirror that has outlived its grace period into the trash directory and removes it.
func (s *sweeper) removeDue(name string, age time.Duration) {
	var moved string
	claimed, err := s.p.Claim(name, func() error {
		var err error
		moved, err = s.moveToTrash(name)
		return err
	})
	if err != nil {
		s.res.Failed++
		s.p.Log.Warn("mirror cleanup: cannot set an expired mirror aside; will retry", "mirror", name, "reason", errReason(err))
		return
	}
	if !claimed {
		s.res.Busy++
		s.p.Log.Debug("mirror cleanup: mirror not claimed (in use, configured again, or cleanup paused); leaving it", "mirror", name)
		return
	}
	if err := s.p.Remove(s.root, moved); err != nil {
		s.res.Failed++
		s.p.Log.Warn("mirror cleanup: mirror set aside but not yet removed; will retry", "mirror", name, "reason", errReason(err))
		return
	}
	s.res.Removed++
	s.p.Log.Info("removed orphaned mirror", "mirror", name, "age", age.Round(time.Second))
}

// moveToTrash renames a mirror into the trash directory under a unique name and returns its new
// root-relative path.
func (s *sweeper) moveToTrash(name string) (string, error) {
	if err := s.ensureTrash(); err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	dest := filepath.Join(gcTrashDirName, name+"."+hex.EncodeToString(b[:]))
	if err := s.root.Rename(name, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// ensureTrash creates the trash directory (owner-only) if needed and requires it to be a real directory.
func (s *sweeper) ensureTrash() error {
	fi, err := s.root.Lstat(gcTrashDirName)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s.root.Mkdir(gcTrashDirName, 0o700)
	case err != nil:
		return err
	case fi.Mode().Type() != fs.ModeDir:
		return errUnsafeTrash
	}
	return nil
}

// recoverTombstones removes mirrors an earlier sweep set aside but could not finish removing (the
// removal failed, or the process stopped). Only real directories with a well-formed tombstone name are
// removed.
func (s *sweeper) recoverTombstones(ctx context.Context) {
	fi, err := s.root.Lstat(gcTrashDirName)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.res.Failed++
			s.p.Log.Warn("mirror cleanup: cannot inspect the trash directory", "reason", errReason(err))
		}
		return
	}
	if fi.Mode().Type() != fs.ModeDir {
		s.res.Skipped++
		s.p.Log.Warn("mirror cleanup: skipping the trash path: not a real directory", "kind", kindOf(fi.Mode()))
		return
	}
	for _, n := range s.listNames(gcTrashDirName) {
		if ctx.Err() != nil {
			return
		}
		path := filepath.Join(gcTrashDirName, n)
		if !tombstoneRE.MatchString(n) {
			s.res.Skipped++
			s.p.Log.Warn("mirror cleanup: skipping an unexpected entry in the trash directory")
			continue
		}
		fi, err := s.root.Lstat(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				s.res.Failed++
				s.p.Log.Warn("mirror cleanup: cannot inspect a set-aside mirror; will retry", "mirror", n, "reason", errReason(err))
			}
			continue
		}
		if fi.Mode().Type() != fs.ModeDir {
			s.res.Skipped++
			s.p.Log.Warn("mirror cleanup: skipping a trash entry that is not a real directory", "mirror", n, "kind", kindOf(fi.Mode()))
			continue
		}
		if err := s.p.Remove(s.root, path); err != nil {
			s.res.Failed++
			s.p.Log.Warn("mirror cleanup: cannot remove a set-aside mirror; will retry", "mirror", n, "reason", errReason(err))
			continue
		}
		s.res.Removed++
		s.p.Log.Info("removed a set-aside orphaned mirror left by an earlier sweep", "mirror", n)
	}
}

// readMarker returns when the mirror was first seen orphaned. A missing marker, or one that is not a
// regular file, is oversized, does not parse, or is dated in the future or before the clock floor,
// reports false.
func (s *sweeper) readMarker(name string) (time.Time, bool) {
	path := filepath.Join(name, orphanMarkerName)
	fi, err := s.root.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxMarkerSize {
		return time.Time{}, false
	}
	f, err := s.root.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxMarkerSize+1))
	if err != nil || len(b) > maxMarkerSize {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	if err != nil || t.After(s.p.Now) || t.Before(mirrorGCClockFloor) {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// writeMarker records the current time as the moment the mirror was first seen orphaned. The marker is
// written to a fixed temporary name (exclusively created, owner-only) and renamed into place, so it is
// atomic, and a symlink planted at either name is replaced, never followed. If something that cannot be
// removed squats on a name, the mirror is left unmarked, and therefore never removed.
func (s *sweeper) writeMarker(name string) {
	marker := filepath.Join(name, orphanMarkerName)
	tmp := filepath.Join(name, markerTmpName)
	fail := func(err error) {
		s.res.Failed++
		s.p.Log.Warn("mirror cleanup: cannot mark an orphaned mirror; it will not be removed", "mirror", name, "reason", errReason(err))
	}

	if _, err := s.root.Lstat(tmp); err == nil {
		if err := s.root.Remove(tmp); err != nil {
			fail(err)
			return
		}
	}
	if fi, err := s.root.Lstat(marker); err == nil && !fi.Mode().IsRegular() {
		if err := s.root.Remove(marker); err != nil {
			fail(err)
			return
		}
	}
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fail(err)
		return
	}
	_, err = f.WriteString(s.p.Now.UTC().Format(time.RFC3339Nano) + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = s.root.Rename(tmp, marker)
	}
	if err != nil {
		_ = s.root.Remove(tmp)
		fail(err)
		return
	}
	s.res.Marked++
	s.p.Log.Warn("mirror is no longer in the configuration; it will be removed after the grace period", "mirror", name, "grace", s.p.Grace)
}

// clearMarker removes the orphan mark (and any stray temporary) from a mirror that is configured.
func (s *sweeper) clearMarker(name string) {
	if _, err := s.root.Lstat(filepath.Join(name, markerTmpName)); err == nil {
		_ = s.root.Remove(filepath.Join(name, markerTmpName))
	}
	marker := filepath.Join(name, orphanMarkerName)
	if _, err := s.root.Lstat(marker); err != nil {
		return
	}
	if err := s.root.Remove(marker); err != nil {
		s.p.Log.Warn("mirror cleanup: cannot clear the orphan mark of a configured mirror", "mirror", name, "reason", errReason(err))
		return
	}
	s.res.Cleared++
	s.p.Log.Info("mirror is back in the configuration; orphan mark cleared", "mirror", name)
}

// kindOf names a non-directory file type for the log.
func kindOf(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "symlink"
	case m.IsRegular():
		return "file"
	default:
		return "other"
	}
}

// errReason reduces an error to a fixed phrase. Errors from the filesystem embed full paths, which this
// code must not log, so only a classification or the bare errno text is ever written.
func errReason(err error) string {
	var errno syscall.Errno
	switch {
	case errors.Is(err, errUnsafeTrash):
		return errUnsafeTrash.Error()
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, fs.ErrNotExist):
		return "not found"
	case errors.As(err, &errno):
		return errno.Error()
	default:
		return "unspecified error"
	}
}

// --- wiring into the connector ---

const (
	// mirrorGCInterval is how often the connector re-checks for expired orphans on its own, in
	// addition to the check after each accepted configuration. Removal is a once-a-week event, so this
	// only bounds how long past its deadline an orphan can linger.
	mirrorGCInterval = 6 * time.Hour

	// mirrorGCEnvVar disables mirror cleanup when set to "off": an operator's kill switch for a
	// destructive background task.
	mirrorGCEnvVar = "GIT_SYNC_MIRROR_GC"
)

func mirrorGCDisabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(mirrorGCEnvVar)), "off")
}

// mirrorGC is the connector's mirror-cleanup state.
type mirrorGC struct {
	// wake asks the loop for a sweep; it holds at most one pending request.
	wake chan struct{}
	// rejected records that the most recent git_sync_config was rejected. The previous configuration
	// stays in force, but a platform that is sending invalid configuration is not one to delete data on.
	rejected atomic.Bool
	interval time.Duration
	// remove removes a path inside the mirrors root; tests replace it. Nil means (*os.Root).RemoveAll.
	remove func(r *os.Root, name string) error

	mu   sync.Mutex
	done chan struct{} // closed when the loop started by startMirrorGC has exited
	// configured is when each mirror was last seen in an accepted configuration. It is kept even while
	// sweeps are paused, so an orphan mark written before a repository came back is recognised as stale
	// however the sweeps and configurations interleave.
	configured map[string]time.Time
}

func newMirrorGC() *mirrorGC {
	return &mirrorGC{wake: make(chan struct{}, 1), interval: mirrorGCInterval, configured: map[string]time.Time{}}
}

// noteConfigured records that every mirror in keep is configured as of at.
func (g *mirrorGC) noteConfigured(keep map[string]struct{}, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for name := range keep {
		g.configured[name] = at
	}
}

func (g *mirrorGC) lastConfigured(name string) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.configured[name]
	return t, ok
}

// configApplied records the outcome of a git_sync_config and, when it was accepted, asks for a sweep.
func (g *mirrorGC) configApplied(accepted bool) {
	g.rejected.Store(!accepted)
	if !accepted {
		return
	}
	select {
	case g.wake <- struct{}{}:
	default: // a sweep is already requested
	}
}

func (g *mirrorGC) currentDone() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.done
}

// wait blocks until the loop has exited or ctx ends. It returns at once if no loop was started.
func (g *mirrorGC) wait(ctx context.Context) {
	done := g.currentDone()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// dirName returns the directory name Mirrors.Dir gives repoID: the name sweepMirrors matches against.
func (m Mirrors) dirName(repoID string) string { return filepath.Base(m.Dir(repoID)) }

// noteConfigApplied is called after every git_sync_config, accepted or not, on the gateway read loop.
// It records which mirrors the new configuration keeps before it wakes the cleanup loop.
func (c *Client) noteConfigApplied(accepted bool) {
	if accepted {
		if keep, ok := c.acceptedMirrorKeep(); ok {
			c.gc.noteConfigured(keep, c.now())
		}
	}
	c.gc.configApplied(accepted)
}

// acceptedMirrorKeep returns the mirror directory names of every repository in the accepted
// configuration (matched or not: any configured repository keeps its mirror). It reports false when no
// configuration has been accepted in this process or the accepted one has no repositories: the
// in-memory set is empty after a restart until the platform re-sends it, and an empty set must never be
// read as "everything is an orphan".
func (c *Client) acceptedMirrorKeep() (map[string]struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.configAccepted || len(c.repos) == 0 {
		return nil, false
	}
	mirrors := c.mgr.deps.mirrors
	keep := make(map[string]struct{}, len(c.repos))
	for id := range c.repos {
		keep[mirrors.dirName(id)] = struct{}{}
	}
	return keep, true
}

// mirrorGCAllowed reports whether a sweep may run now: cleanup is not disabled, the latest
// configuration was accepted, and no critical security update is pending.
func (c *Client) mirrorGCAllowed() bool {
	return !mirrorGCDisabled() && !c.gc.rejected.Load() && !c.criticalPatchRequired.Load()
}

// mirrorOrphaned reports, from the live configuration, whether the mirror is still an orphan.
func (c *Client) mirrorOrphaned(name string) bool {
	if !c.mirrorGCAllowed() {
		return false
	}
	keep, ok := c.acceptedMirrorKeep()
	if !ok {
		return false
	}
	_, configured := keep[name]
	return !configured
}

// claimMirror runs tombstone for an orphaned mirror while no sync of it can start; see
// manager.claimMirror.
func (c *Client) claimMirror(name string, tombstone func() error) (bool, error) {
	return c.mgr.claimMirror(name, func() bool { return c.mirrorOrphaned(name) }, tombstone)
}

// collectMirrors runs one cleanup sweep if it is allowed and a configuration with repositories has
// been accepted. Anything else is a no-op, never a sweep against an empty set.
func (c *Client) collectMirrors(ctx context.Context) {
	if !c.mirrorGCAllowed() {
		return
	}
	keep, ok := c.acceptedMirrorKeep()
	if !ok {
		return
	}
	remove := c.gc.remove
	if remove == nil {
		remove = (*os.Root).RemoveAll
	}
	res := sweepMirrors(ctx, sweepParams{
		Root:           c.mgr.deps.mirrors.Root,
		Keep:           keep,
		Now:            c.now(),
		Grace:          mirrorGCGrace,
		Busy:           c.mgr.mirrorBusy,
		LastConfigured: c.gc.lastConfigured,
		Claim:          c.claimMirror,
		Remove:         remove,
		Log:            slog.Default(),
	})
	if res != (sweepResult{}) {
		slog.Info("mirror cleanup finished", "marked", res.Marked, "cleared", res.Cleared, "busy", res.Busy,
			"removed", res.Removed, "skipped", res.Skipped, "failed", res.Failed)
	}
}

// startMirrorGC starts the cleanup loop: a sweep after each accepted configuration (the wake signal)
// and one every mirrorGCInterval. It runs until ctx ends; Wait confirms it has stopped. It starts at
// most one loop per Client, so it is safe to call from Run more than once. Sweeps run here, never on the gateway read loop.
func (c *Client) startMirrorGC(ctx context.Context) {
	if mirrorGCDisabled() {
		slog.Info("mirror cleanup is disabled", "env", mirrorGCEnvVar)
	}
	done := make(chan struct{})
	c.gc.mu.Lock()
	if c.gc.done != nil { // already running: a second loop would sweep the same root concurrently
		c.gc.mu.Unlock()
		return
	}
	c.gc.done = done
	c.gc.mu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(c.gc.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.gc.wake:
			case <-ticker.C:
			}
			c.collectMirrors(ctx)
		}
	}()
}

// mirrorBusy reports whether a sync of the mirror directory called name is queued or running.
func (m *manager) mirrorBusy(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mirrorInflightLocked(name)
}

// mirrorInflightLocked is mirrorBusy with m.mu held. A repository removed from the configuration while
// its sync runs stays in flight until the sync ends.
func (m *manager) mirrorInflightLocked(name string) bool {
	for id := range m.inflight {
		if m.deps.mirrors.dirName(id) == name {
			return true
		}
	}
	return false
}

// claimMirror runs tombstone (which moves the mirror aside) provided no sync of the mirror is queued or
// running and orphaned still holds. It does both checks and the move with m.mu held, and enqueue
// registers a sync under m.mu, so a sync cannot start between the check and the move: one that starts
// afterwards finds no directory and initialises a fresh one. It reports whether tombstone ran.
//
// Lock order: m.mu, then the Client's mu (inside orphaned). Nothing takes them the other way round: the
// manager calls its hooks without holding m.mu.
func (m *manager) claimMirror(name string, orphaned func() bool, tombstone func() error) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mirrorInflightLocked(name) || !orphaned() {
		return false, nil
	}
	return true, tombstone()
}
