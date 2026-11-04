package gitsync

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/proto"
)

func intp(v int) *int { return &v }

func TestDefaultLimitsMatchExistingConstants(t *testing.T) {
	d := DefaultLimits()
	if d.MaxConcurrency != defaultSyncConcurrency {
		t.Errorf("MaxConcurrency = %d, want defaultSyncConcurrency %d", d.MaxConcurrency, defaultSyncConcurrency)
	}
	if d.PushTimeout != defaultPushTimeout {
		t.Errorf("PushTimeout = %v, want defaultPushTimeout %v", d.PushTimeout, defaultPushTimeout)
	}
	want := Limits{
		Interval:       15 * time.Minute,
		PushTimeout:    30 * time.Minute,
		MaxConcurrency: 4,
	}
	if d != want {
		t.Errorf("DefaultLimits() = %+v, want %+v", d, want)
	}
}

func TestResolveLimitsNilInputsGiveDefaults(t *testing.T) {
	for name, tc := range map[string]struct {
		s *proto.GitSyncSchedule
		l *proto.GitSyncLimits
	}{
		"both nil":          {},
		"empty schedule":    {s: &proto.GitSyncSchedule{}},
		"empty limits":      {l: &proto.GitSyncLimits{}},
		"both empty":        {s: &proto.GitSyncSchedule{}, l: &proto.GitSyncLimits{}},
		"nil interval only": {s: &proto.GitSyncSchedule{IntervalSeconds: nil}},
	} {
		got, err := ResolveLimits(tc.s, tc.l)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
		if got != DefaultLimits() {
			t.Errorf("%s: got %+v, want defaults %+v", name, got, DefaultLimits())
		}
	}
}

func TestResolveLimitsAppliesEveryField(t *testing.T) {
	got, err := ResolveLimits(
		&proto.GitSyncSchedule{IntervalSeconds: intp(300)},
		&proto.GitSyncLimits{
			MaxConcurrency:     intp(2),
			PushBatchRefs:      intp(5000),
			PackThreads:        intp(8),
			PackWindowMemoryMB: intp(256),
			MaxRepositoryMB:    intp(20480),
			PushTimeoutSeconds: intp(600),
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Limits{
		Interval:           5 * time.Minute,
		PushTimeout:        10 * time.Minute,
		MaxConcurrency:     2,
		PushBatchRefs:      5000,
		PackThreads:        8,
		PackWindowMemoryMB: 256,
		MaxRepositoryMB:    20480,
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestResolveLimitsPartialKeepsOtherDefaults pins that supplying one field leaves the rest at their
// defaults, and that explicit zeros (valid for several fields) are accepted rather than rejected. The
// absent-versus-zero distinction itself is pinned at the wire level in the proto tests, because every
// zero-allowed field also defaults to 0.
func TestResolveLimitsPartialKeepsOtherDefaults(t *testing.T) {
	got, err := ResolveLimits(nil, &proto.GitSyncLimits{MaxConcurrency: intp(8)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := DefaultLimits()
	want.MaxConcurrency = 8
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	got, err = ResolveLimits(nil, &proto.GitSyncLimits{PackThreads: intp(0), MaxRepositoryMB: intp(0)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != DefaultLimits() {
		t.Errorf("explicit zeros: got %+v, want defaults %+v", got, DefaultLimits())
	}
}

// TestResolveLimitsBounds exercises both edges of every accepted range, one value either side, and
// each field's zero case.
func TestResolveLimitsBounds(t *testing.T) {
	fields := []struct {
		name   string
		set    func(*proto.GitSyncSchedule, *proto.GitSyncLimits, *int)
		accept []int
		reject []int
	}{
		{
			name:   "interval_seconds",
			set:    func(s *proto.GitSyncSchedule, _ *proto.GitSyncLimits, v *int) { s.IntervalSeconds = v },
			accept: []int{60, 61, 86399, 86400},
			reject: []int{-1, 0, 59, 86401},
		},
		{
			name:   "max_concurrency",
			set:    func(_ *proto.GitSyncSchedule, l *proto.GitSyncLimits, v *int) { l.MaxConcurrency = v },
			accept: []int{1, 2, 31, 32},
			reject: []int{-1, 0, 33},
		},
		{
			name:   "push_batch_refs",
			set:    func(_ *proto.GitSyncSchedule, l *proto.GitSyncLimits, v *int) { l.PushBatchRefs = v },
			accept: []int{0, 100, 101, 99999, 100000},
			reject: []int{-1, 1, 99, 100001},
		},
		{
			name:   "pack_threads",
			set:    func(_ *proto.GitSyncSchedule, l *proto.GitSyncLimits, v *int) { l.PackThreads = v },
			accept: []int{0, 1, 63, 64},
			reject: []int{-1, 65},
		},
		{
			name:   "pack_window_memory_mb",
			set:    func(_ *proto.GitSyncSchedule, l *proto.GitSyncLimits, v *int) { l.PackWindowMemoryMB = v },
			accept: []int{0, 16, 17, 65535, 65536},
			reject: []int{-1, 1, 15, 65537},
		},
		{
			name:   "max_repository_mb",
			set:    func(_ *proto.GitSyncSchedule, l *proto.GitSyncLimits, v *int) { l.MaxRepositoryMB = v },
			accept: []int{0, 1, 2, 10485759, 10485760},
			reject: []int{-1, 10485761},
		},
		{
			name:   "push_timeout_seconds",
			set:    func(_ *proto.GitSyncSchedule, l *proto.GitSyncLimits, v *int) { l.PushTimeoutSeconds = v },
			accept: []int{60, 61, 86399, 86400},
			reject: []int{-1, 0, 59, 86401},
		},
	}
	for _, f := range fields {
		for _, v := range f.accept {
			s, l := &proto.GitSyncSchedule{}, &proto.GitSyncLimits{}
			f.set(s, l, intp(v))
			if _, err := ResolveLimits(s, l); err != nil {
				t.Errorf("%s=%d: unexpected error: %v", f.name, v, err)
			}
		}
		for _, v := range f.reject {
			s, l := &proto.GitSyncSchedule{}, &proto.GitSyncLimits{}
			f.set(s, l, intp(v))
			got, err := ResolveLimits(s, l)
			if err == nil {
				t.Errorf("%s=%d: expected an out-of-range error, got %+v", f.name, v, got)
				continue
			}
			if got != (Limits{}) {
				t.Errorf("%s=%d: result must be the zero value on error, got %+v", f.name, v, got)
			}
			var le *LimitsError
			if !errors.As(err, &le) {
				t.Errorf("%s=%d: error %T is not a *LimitsError", f.name, v, err)
				continue
			}
			if le.Reason != proto.GitSyncConfigErrLimitsOutOfRange || le.Field != f.name {
				t.Errorf("%s=%d: error = %+v, want reason limits_out_of_range naming %s", f.name, v, le, f.name)
			}
		}
	}
}

// TestResolveLimitsRejectsWholeResult pins that one bad field discards the valid ones beside it:
// nothing is partially applied.
func TestResolveLimitsRejectsWholeResult(t *testing.T) {
	got, err := ResolveLimits(
		&proto.GitSyncSchedule{IntervalSeconds: intp(300)},
		&proto.GitSyncLimits{MaxConcurrency: intp(2), PushTimeoutSeconds: intp(5)},
	)
	if err == nil {
		t.Fatalf("expected an error, got %+v", got)
	}
	if got != (Limits{}) {
		t.Errorf("result = %+v, want the zero value", got)
	}
}

// TestResolveLimitsErrorNamesFieldNotValue pins that the error text names the offending field and
// never echoes the supplied value, so it is safe to log.
func TestResolveLimitsErrorNamesFieldNotValue(t *testing.T) {
	_, err := ResolveLimits(nil, &proto.GitSyncLimits{MaxRepositoryMB: intp(987654321)})
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "max_repository_mb") || !strings.Contains(msg, "limits_out_of_range") {
		t.Errorf("error text %q should name the reason and field", msg)
	}
	if strings.Contains(msg, "987654321") {
		t.Errorf("error text %q echoes the wire value", msg)
	}
}

// TestResolveLimitsReportsFirstBadFieldInFixedOrder keeps the reported field deterministic when
// several are out of range: the schedule first, then limits in declaration order.
func TestResolveLimitsReportsFirstBadFieldInFixedOrder(t *testing.T) {
	_, err := ResolveLimits(
		&proto.GitSyncSchedule{IntervalSeconds: intp(1)},
		&proto.GitSyncLimits{MaxConcurrency: intp(0), PushTimeoutSeconds: intp(1)},
	)
	var le *LimitsError
	if !errors.As(err, &le) || le.Field != "interval_seconds" {
		t.Errorf("error = %v, want interval_seconds first", err)
	}
	_, err = ResolveLimits(nil, &proto.GitSyncLimits{MaxConcurrency: intp(0), PushTimeoutSeconds: intp(1)})
	if !errors.As(err, &le) || le.Field != "max_concurrency" {
		t.Errorf("error = %v, want max_concurrency first", err)
	}
}
