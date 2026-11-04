package gitsync

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"syscall"
	"time"
)

// The live reload of the local provider configuration (the provider file). A change is noticed by polling a digest, applied only once it has been seen twice, and
// can be forced with SIGHUP. These are constants on purpose: there is no knob and no way to turn the
// reload off, because the only alternative to noticing a revoked credential is restarting.
const (
	// reloadPollInterval is how often the configuration is looked at.
	reloadPollInterval = 5 * time.Second
	// reloadSettle is how long a change must hold before it is applied: it must be seen again this
	// long after it was first seen. It keeps a half-written file, an editor's save sequence and a
	// symlink swap from being applied in an intermediate state, and a file that flaps from being
	// applied at all.
	reloadSettle = time.Second
)

// digest identifies what one read of the configuration saw. It is derived from the bytes of the
// provider file, so it is a function of secrets: it is never logged and never
// leaves a package-private field.
type digest [sha256.Size]byte

// contentDigest is the digest of a configuration whose provider file holds data.
func contentDigest(data []byte) digest { return digestOf("content", data) }

// missingDigest is the digest of a configuration whose provider file does not exist.
func missingDigest() digest { return digestOf("missing", nil) }

// failureDigest is the digest of a configuration that could not be read or loaded, identified by its
// category: every read that fails the same way is the same observation.
func failureDigest(category string) digest { return digestOf("failure", []byte(category)) }

func digestOf(kind string, data []byte) digest {
	h := sha256.New()
	var n [8]byte
	for _, part := range [][]byte{[]byte(kind), data} {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	var d digest
	h.Sum(d[:0])
	return d
}

// bundleKind says what a read of the configuration found.
type bundleKind int

const (
	// bundleOK: the configuration was read and is valid; store holds it.
	bundleOK bundleKind = iota
	// bundleMissing: the provider file does not exist.
	bundleMissing
	// bundleInvalid: the configuration exists but cannot be used; category says why.
	bundleInvalid
)

// configBundle is one read of the configuration. The digest and the parsed store come from the same
// bytes, so what is compared is exactly what would be applied: there is no window between noticing a
// change and loading it in which the files could differ.
type configBundle struct {
	kind     bundleKind
	digest   digest
	store    *Store
	category string // fixed, content-free; set for bundleInvalid
}

// configSource reads the configuration. A confirming read (confirm) is the one whose result may be
// applied: it enforces the file rules exactly as the startup load does, tightening the file's mode
// where it can. The reads that merely look for a change (not confirm) have no side effect: they
// neither change a file's mode nor log about it, so polling is silent and cannot undo an operator's
// deliberate permissions.
type configSource interface {
	read(confirm bool) configBundle
}

// fileSource reads the provider file at path.
type fileSource struct{ path string }

func (f fileSource) read(confirm bool) configBundle {
	var data []byte
	var err error
	var loose os.FileMode
	if confirm {
		data, err = readSecureFile(f.path, "git-sync config", maxConfigFileSize)
	} else {
		data, loose, err = peekFile(f.path, "git-sync config", maxConfigFileSize)
	}
	if err != nil {
		if os.IsNotExist(err) {
			return configBundle{kind: bundleMissing, digest: missingDigest()}
		}
		return configBundle{kind: bundleInvalid, digest: failureDigest(loadUnreadable), category: loadUnreadable}
	}
	// A look at a file whose mode the confirming read would have to tighten is a different
	// observation from the same bytes at an acceptable mode: the mode is part of what it saw, so a
	// change to the mode alone is noticed.
	digest := contentDigest(data)
	if loose != 0 {
		digest = looseModeDigest(loose, data)
	}
	st, err := parseProviders(f.path, data)
	if err != nil {
		c := loadCategory(err)
		return configBundle{kind: bundleInvalid, digest: digest, category: c}
	}
	return configBundle{kind: bundleOK, digest: digest, store: st}
}

// looseModeDigest is the digest a look reports for data at a mode that is not acceptable as-is.
func looseModeDigest(mode os.FileMode, data []byte) digest {
	return digestOf(fmt.Sprintf("loose:%04o", mode.Perm()), data)
}

// peekFile is readSecureFile without its side effects: it applies the same checks through one
// descriptor but never changes the file's mode and never logs. A mode readSecureFile would tighten is
// not an error here: whether it can be tightened is for the confirming read to find out, and a look
// that called it unusable would hide, behind a digest shared by every unreadable file, the fix of
// whatever made it so.
func peekFile(path, what string, maxSize int64) (data []byte, loose os.FileMode, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", what)
	}
	if info.Size() > maxSize {
		return nil, 0, fmt.Errorf("%s is larger than the %d-byte limit", what, maxSize)
	}
	if !readOnlyModeAcceptable(info.Mode()) {
		loose = info.Mode()
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(b)) > maxSize {
		return nil, 0, fmt.Errorf("%s is larger than the %d-byte limit", what, maxSize)
	}
	return b, loose, nil
}

// reloader notices changes to the provider configuration and applies them. One goroutine owns all of
// its state.
//
// A change is a digest that differs from the one in force. It is applied only when a second read,
// settle later, shows the same digest; any other digest at that point starts the wait over, and the
// digest of what is already in force cancels it, so a file that flaps, or is rewritten in several
// steps, is never applied half-way. A configuration that cannot be used is never applied: the last
// good one stays in force, however long that is, and is reported once per distinct unusable content.
// A provider file that no longer exists is the one change that fails closed: after the settle the
// configuration is empty, every credential is revoked, and the log says so loudly.
type reloader struct {
	src    configSource
	apply  func(*Store)
	poll   time.Duration
	settle time.Duration
	hup    chan struct{}

	applied digest // what is in force

	pending     bool
	pendingSeen digest // the digest seen first, awaiting its second sighting

	lastBad     digest // the unusable content already reported
	haveLastBad bool
	// badFrom is the content a look found that the confirming read then refused, so that it is not
	// looked at, and confirmed, again every poll.
	badFrom     digest
	haveBadFrom bool
}

func newReloader(src configSource, applied digest, apply func(*Store)) *reloader {
	return &reloader{
		src: src, apply: apply, applied: applied,
		poll: reloadPollInterval, settle: reloadSettle,
		hup: make(chan struct{}, 1),
	}
}

// requestReload asks for an immediate reload (SIGHUP). Requests that arrive while one is pending
// coalesce.
func (r *reloader) requestReload() {
	select {
	case r.hup <- struct{}{}:
	default:
	}
}

// run polls until ctx ends.
func (r *reloader) run(ctx context.Context) {
	tick := time.NewTicker(r.poll)
	defer tick.Stop()
	var settleTimer *time.Timer
	var settleC <-chan time.Time
	arm := func() {
		if settleTimer == nil {
			settleTimer = time.NewTimer(r.settle)
		} else {
			if !settleTimer.Stop() {
				select {
				case <-settleTimer.C:
				default:
				}
			}
			settleTimer.Reset(r.settle)
		}
		settleC = settleTimer.C
	}
	disarm := func() {
		if settleTimer != nil && !settleTimer.Stop() {
			select {
			case <-settleTimer.C:
			default:
			}
		}
		settleC = nil
	}
	defer disarm()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if r.pending {
				continue // the settle read is already due
			}
			if r.sight(r.src.read(false)) {
				arm()
			}
		case <-settleC:
			settleC = nil
			if r.confirm() {
				arm() // the change moved on; wait for the new one to hold
			}
		case <-r.hup:
			disarm()
			r.pending = false
			if r.forced() {
				arm()
			}
		}
	}
}

// sight records the first sighting of b and reports whether a settle wait must start. A digest that
// is in force, or is the already-reported unusable one, starts nothing.
func (r *reloader) sight(b configBundle) bool {
	if b.digest == r.applied {
		r.haveLastBad, r.haveBadFrom = false, false
		return false
	}
	if r.haveLastBad && b.digest == r.lastBad {
		return false
	}
	if r.haveBadFrom && b.digest == r.badFrom {
		return false
	}
	r.pending, r.pendingSeen = true, b.digest
	return true
}

// confirm is the second sighting. It reports whether another settle wait must start.
func (r *reloader) confirm() (rearm bool) {
	r.pending = false
	b := r.src.read(true)
	switch {
	case b.digest == r.applied:
		return false // it went back
	case b.kind == bundleInvalid && b.category == loadUnreadable && b.digest != r.pendingSeen:
		// The look found content that the confirming read then refused (it could not tighten the
		// file's mode, or the file went bad): unusable, and the content that was looked at is not
		// looked at again until it changes.
		r.badFrom, r.haveBadFrom = r.pendingSeen, true
		r.decide(b, false)
		return false
	case b.digest != r.pendingSeen:
		// What the first sighting saw is not what is there now: count this as a new first sighting.
		return r.sight(b)
	}
	r.decide(b, false)
	return false
}

// forced handles a SIGHUP: the configuration is read and applied at once, without the settle. The one
// exception is a provider file that does not exist, which still waits: an editor that saves by
// deleting and recreating the file would otherwise revoke every credential for the instant between.
func (r *reloader) forced() (rearm bool) {
	b := r.src.read(true)
	if b.kind == bundleMissing && b.digest != r.applied {
		slog.Warn("reload requested but the git-sync config file is missing; it will be treated as empty if it is still missing shortly")
		r.pending, r.pendingSeen = true, b.digest
		return true
	}
	if b.digest == r.applied {
		slog.Info("reload requested: the git-sync config is unchanged")
		return false
	}
	r.decide(b, true)
	return false
}

// decide acts on a configuration that has held. explicit says an operator asked (SIGHUP), so an
// unusable file is reported every time.
func (r *reloader) decide(b configBundle, explicit bool) {
	switch b.kind {
	case bundleOK:
		r.applied, r.haveLastBad, r.haveBadFrom = b.digest, false, false
		r.apply(b.store)
	case bundleMissing:
		slog.Error("the git-sync config file no longer exists: treating the provider configuration as empty, which revokes every provider credential and stops all syncing until the file is restored")
		r.applied, r.haveLastBad, r.haveBadFrom = b.digest, false, false
		r.apply(nil)
	default:
		if explicit || !r.haveLastBad || r.lastBad != b.digest {
			slog.Warn("the git-sync config cannot be used; keeping the last good configuration in force", "reason", b.category)
		}
		r.lastBad, r.haveLastBad = b.digest, true
	}
}
