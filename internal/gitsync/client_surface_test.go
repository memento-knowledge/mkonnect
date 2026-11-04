package gitsync_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gateway"
	"github.com/memento-knowledge/mkonnect/internal/gitsync"
	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// This file covers what the connector reports about its own configuration: the effective limits, the
// suspended marker, and the origin_check that accompanies every status.

// configWithExtras builds a git_sync_config frame with no repositories, so no sync starts, and the
// given extra top-level members (for example `"schedule":{"interval_seconds":120}`).
func configWithExtras(version string, extras ...string) json.RawMessage {
	s := fmt.Sprintf(`{"type":"git_sync_config","config_version":%s,"repositories":[]`, version)
	for _, e := range extras {
		s += "," + e
	}
	return json.RawMessage(s + "}")
}

func TestEffectiveLimitsDefaultBeforeAnyConfig(t *testing.T) {
	c := newTestClient(t, oneProvider)
	if got := c.EffectiveLimitsForTest(); got != gitsync.DefaultLimits() {
		t.Errorf("effective limits = %+v, want the defaults %+v", got, gitsync.DefaultLimits())
	}
}

func TestAcceptedConfigAppliesScheduleAndLimits(t *testing.T) {
	c := newTestClient(t, oneProvider)
	c.HandleMessage(context.Background(), &stubSender{}, "git_sync_config", configWithExtras("1",
		`"schedule":{"interval_seconds":120}`,
		`"limits":{"max_concurrency":2,"push_batch_refs":500,"pack_threads":3,"pack_window_memory_mb":64,"max_repository_mb":2048,"push_timeout_seconds":600}`))

	want := gitsync.Limits{
		Interval:           2 * time.Minute,
		PushTimeout:        10 * time.Minute,
		MaxConcurrency:     2,
		PushBatchRefs:      500,
		PackThreads:        3,
		PackWindowMemoryMB: 64,
		MaxRepositoryMB:    2048,
	}
	if got := c.EffectiveLimitsForTest(); got != want {
		t.Errorf("effective limits = %+v, want %+v", got, want)
	}
}

// The configuration is a full replace: whatever a later configuration leaves out goes back to its
// default rather than keeping the earlier override.
func TestConfigOmittingLimitsResetsToDefault(t *testing.T) {
	c := newTestClient(t, oneProvider)
	s := &stubSender{}
	c.HandleMessage(context.Background(), s, "git_sync_config", configWithExtras("1",
		`"schedule":{"interval_seconds":120}`, `"limits":{"max_concurrency":2}`))
	c.HandleMessage(context.Background(), s, "git_sync_config", configWithExtras("2"))
	if got := c.EffectiveLimitsForTest(); got != gitsync.DefaultLimits() {
		t.Errorf("after a config without schedule/limits: %+v, want the defaults", got)
	}
}

func TestConfigOmittingOneLimitResetsOnlyThatField(t *testing.T) {
	c := newTestClient(t, oneProvider)
	s := &stubSender{}
	c.HandleMessage(context.Background(), s, "git_sync_config", configWithExtras("1",
		`"limits":{"max_concurrency":2,"push_timeout_seconds":600}`))
	c.HandleMessage(context.Background(), s, "git_sync_config", configWithExtras("2",
		`"limits":{"max_concurrency":3}`))

	want := gitsync.DefaultLimits()
	want.MaxConcurrency = 3
	if got := c.EffectiveLimitsForTest(); got != want {
		t.Errorf("effective limits = %+v, want %+v", got, want)
	}
}

// An out-of-range value rejects the whole configuration: the limits, the repositories and the
// version it would have replaced all stay as they were, and the status reports config_error.
func TestOutOfRangeLimitsRejectTheWholeConfig(t *testing.T) {
	for name, extras := range map[string][]string{
		"interval below minimum":      {`"schedule":{"interval_seconds":59}`},
		"interval above maximum":      {`"schedule":{"interval_seconds":86401}`},
		"concurrency zero":            {`"limits":{"max_concurrency":0}`},
		"concurrency above maximum":   {`"limits":{"max_concurrency":33}`},
		"batch refs below minimum":    {`"limits":{"push_batch_refs":99}`},
		"negative pack threads":       {`"limits":{"pack_threads":-1}`},
		"pack window below minimum":   {`"limits":{"pack_window_memory_mb":15}`},
		"negative repository ceiling": {`"limits":{"max_repository_mb":-1}`},
		"push timeout below minimum":  {`"limits":{"push_timeout_seconds":59}`},
		"one bad among good values":   {`"schedule":{"interval_seconds":300}`, `"limits":{"max_concurrency":2,"push_timeout_seconds":59}`},
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, oneProvider)
			s := &stubSender{}
			ctx := context.Background()
			// Unmatched hosts keep the repositories out of the sync engine.
			c.HandleMessage(ctx, s, "git_sync_config", withExtras(configJSON("1", [3]string{"repo-a", "https://other.example.com/a", ""}), `"schedule":{"interval_seconds":120}`))
			before := len(s.messages())
			previous := c.EffectiveLimitsForTest()
			if previous.Interval != 2*time.Minute {
				t.Fatalf("setup: interval = %v, want 2m", previous.Interval)
			}

			bad := withExtras(configJSON("2", [3]string{"repo-b", "https://other.example.com/b", ""}), extras...)
			c.HandleMessage(ctx, s, "git_sync_config", bad)

			if n := len(s.messages()); n != before+1 {
				t.Fatalf("a rejected config sent %d new messages, want one status", n-before)
			}
			wantConfigError(t, lastStatus(t, s.messages()), "2", proto.GitSyncConfigErrLimitsOutOfRange)
			if got := c.EffectiveLimitsForTest(); got != previous {
				t.Errorf("effective limits = %+v after a rejected config, want the previous %+v", got, previous)
			}
			if err := c.OnHeartbeat(ctx, s); err != nil {
				t.Fatalf("OnHeartbeat: %v", err)
			}
			st := lastStatus(t, s.messages())
			if string(st.ConfigVersion) != "1" {
				t.Errorf("config_version = %s, want the previous 1", st.ConfigVersion)
			}
			if _, ok := repoStates(st)["repo-a"]; !ok || len(st.Repositories) != 1 {
				t.Errorf("repositories = %+v, want only the previous repo-a", st.Repositories)
			}
		})
	}
}

// withExtras splices extra top-level members into an already-built config frame.
func withExtras(raw json.RawMessage, extras ...string) json.RawMessage {
	s := string(raw)
	s = s[:len(s)-1]
	for _, e := range extras {
		s += "," + e
	}
	return json.RawMessage(s + "}")
}

// Limits belong to the configuration they arrived with: valid limits in a configuration that is
// rejected for another reason must not take effect alongside the previous repositories.
func TestLimitsOfAConfigRejectedForAnotherReasonDoNotApply(t *testing.T) {
	good := func(id string) string {
		return repoJSON(id, "https://other.example.com/"+id, "", "https://replica.example.com/r/x.git", testPushToken)
	}
	for name, raw := range map[string]json.RawMessage{
		"invalid repo_id": withExtras(configJSONRaw("2", repoJSON("bad id", "https://other.example.com/x", "", "", "")), `"schedule":{"interval_seconds":600}`),
		"duplicate repo_id": withExtras(configJSONRaw("2", good("repo-b"), good("repo-b")),
			`"schedule":{"interval_seconds":600}`, `"limits":{"max_concurrency":8}`),
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, oneProvider)
			s := &stubSender{}
			c.HandleMessage(context.Background(), s, "git_sync_config", withExtras(configWithExtras("1"), `"schedule":{"interval_seconds":120}`))
			previous := c.EffectiveLimitsForTest()

			c.HandleMessage(context.Background(), s, "git_sync_config", raw)
			if got := c.EffectiveLimitsForTest(); got != previous {
				t.Errorf("effective limits = %+v after a rejected config, want the previous %+v", got, previous)
			}
		})
	}
}

func TestSuspendedFollowsTheCriticalUpdateGate(t *testing.T) {
	c := newTestClient(t, oneProvider)
	s := &stubSender{}
	ctx := context.Background()

	if err := c.OnHeartbeat(ctx, s); err != nil {
		t.Fatalf("OnHeartbeat: %v", err)
	}
	if got := lastStatus(t, s.messages()).Suspended; got != "" {
		t.Errorf("suspended = %q with no critical update required, want absent", got)
	}

	c.SetCriticalPatchForTest(true)
	if err := c.OnHeartbeat(ctx, s); err != nil {
		t.Fatalf("OnHeartbeat: %v", err)
	}
	if got := lastStatus(t, s.messages()).Suspended; got != proto.GitSyncSuspendedCriticalUpdate {
		t.Errorf("heartbeat status suspended = %q, want %q", got, proto.GitSyncSuspendedCriticalUpdate)
	}
	c.HandleMessage(ctx, s, "git_sync_config", configWithExtras("1"))
	if got := lastStatus(t, s.messages()).Suspended; got != "critical_update_required" {
		t.Errorf("config status suspended = %q, want critical_update_required", got)
	}

	c.SetCriticalPatchForTest(false)
	if err := c.OnHeartbeat(ctx, s); err != nil {
		t.Fatalf("OnHeartbeat: %v", err)
	}
	if got := lastStatus(t, s.messages()).Suspended; got != "" {
		t.Errorf("suspended = %q after the update requirement cleared, want absent", got)
	}
}

// scriptedProber is a fakeProber whose result, error and blocking can all be changed while it runs.
// A blocked Probe returns ctx.Err() when cancelled, as a real probe's subprocess would.
type scriptedProber struct {
	mu     sync.Mutex
	result gitsync.OriginResult
	err    error
	block  chan struct{}
	calls  atomic.Int32
}

func (p *scriptedProber) Probe(ctx context.Context, _ gitsync.LocalEntry) (gitsync.OriginResult, error) {
	p.calls.Add(1)
	p.mu.Lock()
	block, res, err := p.block, p.result, p.err
	p.mu.Unlock()
	if block != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-block:
		}
	}
	return res, err
}

func (p *scriptedProber) set(res gitsync.OriginResult, err error, block chan struct{}) {
	p.mu.Lock()
	p.result, p.err, p.block = res, err, block
	p.mu.Unlock()
}

// waitProbeDone waits until the probe goroutine has finished, including its status send, so a test
// can assert that nothing further happened.
func waitProbeDone(t *testing.T, c interface{ ProbeInFlightForTest() bool }) {
	t.Helper()
	waitFor(t, func() bool { return !c.ProbeInFlightForTest() })
}

func heartbeat(t *testing.T, c *gitsync.Client, s *stubSender) proto.GitSyncStatusMsg {
	t.Helper()
	if err := c.OnHeartbeat(context.Background(), s); err != nil {
		t.Fatalf("OnHeartbeat: %v", err)
	}
	return lastStatus(t, s.messages())
}

func TestOriginCheckIsReportedBeforeAndAfterConfigAccepted(t *testing.T) {
	prober := &scriptedProber{result: gitsync.OriginAccessDenied}
	c := newTestClientP(t, oneProvider, prober)
	s := &stubSender{}
	ctx := context.Background()
	c.OnConnected(ctx, s)
	waitProbeDone(t, c)

	pre := lastStatus(t, s.messages())
	if pre.OriginCheck == nil || pre.OriginCheck.Result != "access_denied" || pre.OriginCheck.CheckedAt == "" {
		t.Fatalf("origin_check before config = %+v, want access_denied with a checked_at", pre.OriginCheck)
	}

	c.HandleMessage(ctx, s, "git_sync_config", configWithExtras("1"))
	post := lastStatus(t, s.messages())
	if post.OriginCheck == nil || *post.OriginCheck != *pre.OriginCheck {
		t.Errorf("origin_check in the status after config = %+v, want the same %+v", post.OriginCheck, pre.OriginCheck)
	}
	if hb := heartbeat(t, c, s); hb.OriginCheck == nil || *hb.OriginCheck != *pre.OriginCheck {
		t.Errorf("origin_check in a later heartbeat status = %+v, want the same %+v", hb.OriginCheck, pre.OriginCheck)
	}
}

func TestOriginCheckRefreshedEveryIntervalAfterConfigAccepted(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	prober := &scriptedProber{result: gitsync.OriginUnreachable}
	c := newTestClientClock(t, oneProvider, prober, clk.now)
	s := &stubSender{}
	ctx := context.Background()
	c.OnConnected(ctx, s)
	waitProbeDone(t, c)
	first := latestOriginCheck(s.messages())
	c.HandleMessage(ctx, s, "git_sync_config", configWithExtras("1"))

	prober.set(gitsync.OriginReachable, nil, nil)

	// The health cadence no longer applies once configured: just past it, nothing is re-probed.
	clk.advance(gateway.DefaultHeartbeatInterval + time.Second)
	if hb := heartbeat(t, c, s); hb.OriginCheck == nil || hb.OriginCheck.Result != "unreachable" {
		t.Errorf("origin_check = %+v, want the earlier unreachable result", hb.OriginCheck)
	}
	// Heartbeats arrive a little unevenly, so the interval is met with a tenth to spare; just short
	// of that, still nothing.
	due := gitsync.OriginCheckIntervalForTest - gitsync.OriginCheckIntervalForTest/10
	clk.advance(due - gateway.DefaultHeartbeatInterval - 2*time.Second)
	heartbeat(t, c, s)
	waitProbeDone(t, c)
	if n := prober.calls.Load(); n != 1 {
		t.Fatalf("probe ran %d times before the interval elapsed, want 1", n)
	}

	// Once due it runs again and the new result replaces the old one.
	clk.advance(2 * time.Second)
	heartbeat(t, c, s)
	waitFor(t, func() bool {
		oc := latestOriginCheck(s.messages())
		return oc != nil && oc.Result == "reachable"
	})
	waitProbeDone(t, c)
	if oc := latestOriginCheck(s.messages()); oc.CheckedAt == first.CheckedAt {
		t.Errorf("checked_at did not advance: %q", oc.CheckedAt)
	}
	if n := prober.calls.Load(); n != 2 {
		t.Errorf("probe ran %d times, want 2", n)
	}
}

func TestOriginCheckAbsentWithoutLocalProvidersAfterConfig(t *testing.T) {
	prober := &scriptedProber{result: gitsync.OriginReachable}
	c := newTestClientP(t, "", prober)
	s := &stubSender{}
	ctx := context.Background()
	c.OnConnected(ctx, s)
	c.HandleMessage(ctx, s, "git_sync_config", configWithExtras("1"))
	heartbeat(t, c, s)
	waitProbeDone(t, c)
	if n := prober.calls.Load(); n != 0 {
		t.Errorf("prober ran %d times with no local providers, want 0", n)
	}
	if oc := latestOriginCheck(s.messages()); oc != nil {
		t.Errorf("origin_check = %+v with no local providers, want absent", oc)
	}
}

// A local fault (for example git is not installed) says nothing about the origin, so it is logged
// and origin_check stays absent, before and after configuration.
func TestOriginCheckAbsentOnLocalFault(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	prober := &scriptedProber{err: errors.New("git not found")}
	c := newTestClientClock(t, oneProvider, prober, clk.now)
	s := &stubSender{}
	ctx := context.Background()
	c.OnConnected(ctx, s)
	waitProbeDone(t, c)
	if prober.calls.Load() != 1 {
		t.Fatalf("probe ran %d times, want 1", prober.calls.Load())
	}
	c.HandleMessage(ctx, s, "git_sync_config", configWithExtras("1"))
	clk.advance(gitsync.OriginCheckIntervalForTest + time.Second)
	heartbeat(t, c, s)
	waitProbeDone(t, c)
	if prober.calls.Load() != 2 {
		t.Fatalf("probe ran %d times, want a second one after configuration", prober.calls.Load())
	}
	if oc := latestOriginCheck(s.messages()); oc != nil {
		t.Errorf("origin_check = %+v after a local fault, want absent", oc)
	}
}

// A later local fault must not leave the previous result standing: the platform reads origin_check as
// current, and the connector can no longer vouch for it.
func TestOriginCheckClearedWhenALaterProbeHitsALocalFault(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	prober := &scriptedProber{result: gitsync.OriginReachable}
	c := newTestClientClock(t, oneProvider, prober, clk.now)
	s := &stubSender{}
	c.OnConnected(context.Background(), s)
	waitProbeDone(t, c)
	if oc := latestOriginCheck(s.messages()); oc == nil {
		t.Fatal("no origin_check after the first probe")
	}

	prober.set("", errors.New("git not found"), nil)
	clk.advance(gateway.DefaultHeartbeatInterval + time.Second)
	before := len(s.messages())
	heartbeat(t, c, s)
	waitProbeDone(t, c)
	// The heartbeat's own status, then the one the probe pushes so the platform learns the result is
	// gone without waiting for the next tick.
	msgs := s.messages()[before:]
	if len(msgs) != 2 {
		t.Fatalf("%d statuses sent, want the heartbeat's and the probe's", len(msgs))
	}
	if last := msgs[1].(proto.GitSyncStatusMsg); last.OriginCheck != nil {
		t.Errorf("origin_check = %+v after a probe hit a local fault, want absent", last.OriginCheck)
	}
}

// If the clock steps backwards the cadence must not stall until it catches up again.
func TestOriginCheckProbesAgainAfterTheClockStepsBack(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	prober := &scriptedProber{result: gitsync.OriginReachable}
	c := newTestClientClock(t, oneProvider, prober, clk.now)
	s := &stubSender{}
	c.OnConnected(context.Background(), s)
	waitProbeDone(t, c)
	c.HandleMessage(context.Background(), s, "git_sync_config", configWithExtras("1"))

	clk.advance(-2 * time.Hour)
	heartbeat(t, c, s)
	waitFor(t, func() bool { return prober.calls.Load() == 2 })
	waitProbeDone(t, c)
}

// A probe cancelled by its connection closing proves nothing: it neither replaces the cached result
// nor clears it, and it does not use up the cadence, so the next connection probes again at once.
func TestCancelledProbeKeepsTheCachedResultAndIsRetried(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	prober := &scriptedProber{result: gitsync.OriginReachable}
	c := newTestClientClock(t, oneProvider, prober, clk.now)
	s := &stubSender{}

	ctx1, cancel1 := context.WithCancel(context.Background())
	c.OnConnected(ctx1, s)
	waitProbeDone(t, c)
	cached := latestOriginCheck(s.messages())
	c.HandleMessage(ctx1, s, "git_sync_config", configWithExtras("1"))

	// A refresh is under way when the connection drops.
	block := make(chan struct{})
	prober.set(gitsync.OriginUnreachable, nil, block)
	clk.advance(gitsync.OriginCheckIntervalForTest + time.Second)
	if err := c.OnHeartbeat(ctx1, s); err != nil {
		t.Fatalf("OnHeartbeat: %v", err)
	}
	waitFor(t, func() bool { return prober.calls.Load() == 2 })
	cancel1()
	waitProbeDone(t, c)

	// The reconnect serves the old result and, the cancelled attempt not counting, probes again.
	prober.set(gitsync.OriginUnreachable, nil, nil)
	s2 := &stubSender{}
	c.OnConnected(context.Background(), s2)
	if first, ok := s2.messages()[0].(proto.GitSyncStatusMsg); !ok || first.OriginCheck == nil || *first.OriginCheck != *cached {
		t.Errorf("origin_check in the first status of the new connection = %+v, want the cached %+v", first.OriginCheck, cached)
	}
	waitFor(t, func() bool {
		oc := latestOriginCheck(s2.messages())
		return oc != nil && oc.Result == "unreachable"
	})
	waitProbeDone(t, c)
	if n := prober.calls.Load(); n != 3 {
		t.Errorf("probe ran %d times, want 3 (initial, cancelled refresh, retry)", n)
	}
}

// A reconnect must not re-run a probe whose result is still within its cadence, and serves the
// cached result straight away. (A connection that drops while a probe is still running does retry,
// see TestCancelledProbeKeepsTheCachedResultAndIsRetried.)
func TestReconnectWithinTheIntervalDoesNotReprobe(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	prober := &scriptedProber{result: gitsync.OriginReachable}
	c := newTestClientClock(t, oneProvider, prober, clk.now)
	s := &stubSender{}
	ctx1, cancel1 := context.WithCancel(context.Background())
	c.OnConnected(ctx1, s)
	waitProbeDone(t, c)
	c.HandleMessage(ctx1, s, "git_sync_config", configWithExtras("1"))
	cancel1()

	clk.advance(time.Minute)
	s2 := &stubSender{}
	c.OnConnected(context.Background(), s2)
	waitProbeDone(t, c)
	if n := prober.calls.Load(); n != 1 {
		t.Errorf("probe ran %d times across a reconnect, want 1", n)
	}
	if first := lastStatus(t, s2.messages()); first.OriginCheck == nil {
		t.Error("the first status on the new connection should carry the cached origin_check")
	}
}

// Probes never pile up behind a slow origin: heartbeats that arrive while one is running start none.
func TestNoProbePileUpBehindASlowOrigin(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	block := make(chan struct{})
	prober := &scriptedProber{result: gitsync.OriginReachable, block: block}
	c := newTestClientClock(t, oneProvider, prober, clk.now)
	s := &stubSender{}
	c.OnConnected(context.Background(), s)
	waitFor(t, func() bool { return prober.calls.Load() == 1 })

	for range 10 {
		clk.advance(gitsync.OriginCheckIntervalForTest + time.Second)
		heartbeat(t, c, s)
	}
	if n := prober.calls.Load(); n != 1 {
		t.Fatalf("probe ran %d times while one was in flight, want 1", n)
	}

	close(block)
	waitProbeDone(t, c)
	clk.advance(gitsync.OriginCheckIntervalForTest + time.Second)
	heartbeat(t, c, s)
	waitFor(t, func() bool { return prober.calls.Load() == 2 })
	waitProbeDone(t, c)
}

// A probe that is still running when a newer connection takes over belongs to a connection that is
// no longer current. Whatever it finds, it must not replace or clear the cached result the new
// connection serves, nor push a status, and its cadence slot is given back so the new connection
// probes again.
func TestProbeOfASupersededConnectionNeitherStoresNorClears(t *testing.T) {
	for name, finish := range map[string]struct {
		res gitsync.OriginResult
		err error
	}{
		"result":      {res: gitsync.OriginAccessDenied},
		"local fault": {err: errors.New("git not found")},
	} {
		t.Run(name, func(t *testing.T) {
			clk := &testClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
			prober := &scriptedProber{result: gitsync.OriginReachable}
			c := newTestClientClock(t, oneProvider, prober, clk.now)
			s1 := &stubSender{}
			ctx1, cancel1 := context.WithCancel(context.Background())
			defer cancel1()
			c.OnConnected(ctx1, s1)
			waitProbeDone(t, c)
			cached := latestOriginCheck(s1.messages())
			c.HandleMessage(ctx1, s1, "git_sync_config", configWithExtras("1"))

			// A refresh is running on the first connection when the second one takes over.
			block := make(chan struct{})
			prober.set(finish.res, finish.err, block)
			clk.advance(gitsync.OriginCheckIntervalForTest + time.Second)
			heartbeat(t, c, s1)
			waitFor(t, func() bool { return prober.calls.Load() == 2 })
			s2 := &stubSender{}
			c.OnConnected(context.Background(), s2)
			sent1, sent2 := len(s1.messages()), len(s2.messages())

			close(block)
			waitProbeDone(t, c)

			if hb := heartbeat(t, c, s2); hb.OriginCheck == nil || *hb.OriginCheck != *cached {
				t.Errorf("origin_check on the current connection = %+v, want the cached %+v", hb.OriginCheck, cached)
			}
			if n := len(s1.messages()) - sent1; n != 0 {
				t.Errorf("the superseded connection was sent %d statuses, want none", n)
			}
			if n := len(s2.messages()) - sent2; n != 1 {
				t.Errorf("the current connection was sent %d statuses, want only the heartbeat's", n)
			}

			// The abandoned attempt did not use up the cadence slot.
			prober.set(gitsync.OriginUnreachable, nil, nil)
			heartbeat(t, c, s2)
			waitFor(t, func() bool { return prober.calls.Load() == 3 })
			waitProbeDone(t, c)
		})
	}
}
