package gitsync

import (
	"fmt"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// defaultSyncInterval is how often each repository is re-synced when the platform does not say.
const defaultSyncInterval = 15 * time.Minute

// Limits is the effective schedule and resource bounds for the mirror syncs: the built-in defaults
// with any platform-supplied overrides applied. It is a plain value with no behaviour of its own, so
// the code that enforces each bound reads it without depending on the wire types.
type Limits struct {
	// Interval is the delay between periodic re-syncs of each repository.
	Interval time.Duration
	// PushTimeout bounds a single push.
	PushTimeout time.Duration
	// MaxConcurrency is how many repositories sync at once.
	MaxConcurrency int
	// PushBatchRefs is the maximum number of refs sent per push; 0 means unbatched.
	PushBatchRefs int
	// PackThreads is the thread count handed to git when packing; 0 means git's default.
	PackThreads int
	// PackWindowMemoryMB bounds the per-thread delta-window memory git uses when packing, in MiB; 0
	// means git's default.
	PackWindowMemoryMB int
	// MaxRepositoryMB is the on-disk ceiling for one mirrored repository, in MiB; 0 means no ceiling.
	MaxRepositoryMB int
}

// DefaultLimits returns the limits in force when the platform supplies none. Concurrency and the
// push timeout are the values the connector has always used.
func DefaultLimits() Limits {
	return Limits{
		Interval:       defaultSyncInterval,
		PushTimeout:    defaultPushTimeout,
		MaxConcurrency: defaultSyncConcurrency,
	}
}

// LimitsError reports a platform-supplied schedule or limits value outside its accepted range. It
// carries the proto.GitSyncConfigError reason the status report should use, and names the offending
// field in its text. It deliberately holds no value taken from the wire, so it is safe to log.
type LimitsError struct {
	// Reason is the config_error reason: always proto.GitSyncConfigErrLimitsOutOfRange.
	Reason string
	// Field is the wire name of the offending field, taken from this package's own bounds table.
	Field string
}

func (e *LimitsError) Error() string {
	return fmt.Sprintf("%s: %s is out of range", e.Reason, e.Field)
}

// bound is the accepted range of one integer field. When zeroOK is set, 0 is also accepted (and
// means "unbounded" or "use git's default" for that field) in addition to min through max.
type bound struct {
	field    string
	min, max int
	zeroOK   bool
}

func (b bound) check(v int) error {
	if (b.zeroOK && v == 0) || (v >= b.min && v <= b.max) {
		return nil
	}
	return &LimitsError{Reason: proto.GitSyncConfigErrLimitsOutOfRange, Field: b.field}
}

// The accepted ranges, in the units the wire carries. They are the connector's own sanity bounds,
// wide enough to never constrain a sensible deployment while keeping a typo (a timeout in
// milliseconds, a negative count) from reaching git.
var (
	boundInterval       = bound{field: "interval_seconds", min: 60, max: 86400}
	boundMaxConcurrency = bound{field: "max_concurrency", min: 1, max: 32}
	boundPushBatchRefs  = bound{field: "push_batch_refs", min: 100, max: 100000, zeroOK: true}
	boundPackThreads    = bound{field: "pack_threads", min: 0, max: 64}
	boundPackWindowMB   = bound{field: "pack_window_memory_mb", min: 16, max: 65536, zeroOK: true}
	boundMaxRepoMB      = bound{field: "max_repository_mb", min: 1, max: 10485760, zeroOK: true}
	boundPushTimeout    = bound{field: "push_timeout_seconds", min: 60, max: 86400}
)

// ResolveLimits returns the effective limits: DefaultLimits with each non-nil field of s and l
// applied. Nil arguments and nil fields keep the default. Every supplied value is validated and any
// out-of-range value rejects the whole result with a *LimitsError, so a configuration is never
// partially applied; the first offending field in a fixed order is the one reported.
func ResolveLimits(s *proto.GitSyncSchedule, l *proto.GitSyncLimits) (Limits, error) {
	out := DefaultLimits()
	if s != nil && s.IntervalSeconds != nil {
		if err := boundInterval.check(*s.IntervalSeconds); err != nil {
			return Limits{}, err
		}
		out.Interval = time.Duration(*s.IntervalSeconds) * time.Second
	}
	if l == nil {
		return out, nil
	}
	for _, f := range []struct {
		b   bound
		in  *int
		set func(int)
	}{
		{boundMaxConcurrency, l.MaxConcurrency, func(v int) { out.MaxConcurrency = v }},
		{boundPushBatchRefs, l.PushBatchRefs, func(v int) { out.PushBatchRefs = v }},
		{boundPackThreads, l.PackThreads, func(v int) { out.PackThreads = v }},
		{boundPackWindowMB, l.PackWindowMemoryMB, func(v int) { out.PackWindowMemoryMB = v }},
		{boundMaxRepoMB, l.MaxRepositoryMB, func(v int) { out.MaxRepositoryMB = v }},
		{boundPushTimeout, l.PushTimeoutSeconds, func(v int) { out.PushTimeout = time.Duration(v) * time.Second }},
	} {
		if f.in == nil {
			continue
		}
		if err := f.b.check(*f.in); err != nil {
			return Limits{}, err
		}
		f.set(*f.in)
	}
	return out, nil
}
