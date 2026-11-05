package gitsync

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"log/slog"
	"time"
)

// The live reload of the local provider configuration (the provider file, and the token files it
// names). A change is noticed by polling a digest, applied only once it has been seen twice, and
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

// digest identifies what one read of the configuration saw: the provider file and every token file it
// names (by their bytes, or by their absence), and whether the provider file's mode was wider than
// accepted. It is derived from credentials, so it is keyed with the per-process random key of Source
// (a plain hash of a short token could be tested against guesses), it is never logged, and it
// never leaves a package-private field.
type digest [sha256.Size]byte

// sourcesDigest is the digest of a read that involved sources.
func sourcesDigest(sources []Source, loose bool) digest {
	mac := hmac.New(sha256.New, digestKey)
	var n [8]byte
	put := func(b []byte) {
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		mac.Write(n[:])
		mac.Write(b)
	}
	for _, s := range sources {
		put([]byte(s.Path))
		if s.Read {
			put([]byte{1})
			put(s.digest[:])
		} else {
			put([]byte{0})
		}
	}
	if loose {
		put([]byte{1})
	} else {
		put([]byte{0})
	}
	var d digest
	mac.Sum(d[:0])
	return d
}

// bundleKind says what a read of the configuration found.
type bundleKind int

const (
	// bundleOK: the configuration was read and is usable; store holds it.
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
	kind   bundleKind
	digest digest
	store  *Store
	// category is the class of the first problem (an IssueClass), set for bundleInvalid: a fixed
	// word, safe to log, taken from nothing in the files.
	category string
	// loose: a look found the provider file's mode wider than accepted, and so did not judge it.
	loose bool
	// dropped counts providers left out of store because their token file does not exist.
	dropped int
}

// bundleOf turns a load into a bundle.
func bundleOf(path string, res loadedConfig) configBundle {
	d := sourcesDigest(res.sources.sorted(), res.loose)
	b := configBundle{digest: d, loose: res.loose}
	switch {
	case res.missing:
		b.kind = bundleMissing
	case len(res.issues) == 0 || len(res.missingToken) > 0:
		// A token file that does not exist is a credential that was taken away: the provider is
		// dropped and the others carry on, rather than keeping the credential in force.
		b.kind = bundleOK
		b.store = &Store{path: path, data: res.entries, sources: res.sources, digest: d}
		b.dropped = len(res.missingToken)
	default:
		b.kind = bundleInvalid
		b.category = string(res.issues[0].Class)
	}
	return b
}

// configSource reads the configuration. A confirming read (confirm) is the one whose result may be
// applied: it enforces the file rules exactly as the startup load does, tightening the provider
// file's mode where it can. The reads that merely look for a change (not confirm) have no side
// effect: they neither change a file's mode nor log about it, so polling is silent and cannot undo
// an operator's deliberate permissions.
type configSource interface {
	read(confirm bool) configBundle
}

// fileSource reads the provider file at path, and the token files it names.
type fileSource struct{ path string }

func (f fileSource) read(confirm bool) configBundle {
	return bundleOf(f.path, loadConfigMode(f.path, confirm))
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

	pending      bool
	pendingSeen  digest // the digest seen first, awaiting its second sighting
	pendingLoose bool   // that sighting found the provider file's mode too open to judge

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
	r.pending, r.pendingSeen, r.pendingLoose = true, b.digest, b.loose
	return true
}

// confirm is the second sighting. It reports whether another settle wait must start.
func (r *reloader) confirm() (rearm bool) {
	r.pending = false
	b := r.src.read(true)
	switch {
	case b.digest == r.applied:
		return false // it went back
	case b.kind == bundleInvalid && b.category == string(IssueFile) && r.pendingLoose && b.digest != r.pendingSeen:
		// The look found a provider file whose mode it could not judge, and the confirming read
		// refused it (it could not tighten the mode). The two disagree about the same bytes, so what
		// was looked at is unusable, and is not looked at again until its content or mode changes.
		// (Any other disagreement means the files moved between the two reads, and is a new first
		// sighting.)
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
		r.pending, r.pendingSeen, r.pendingLoose = true, b.digest, b.loose
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
		if b.dropped > 0 {
			slog.Error("a token file no longer exists: the providers that used it are dropped, which revokes their credentials; the others carry on", "providers", b.dropped)
		}
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
