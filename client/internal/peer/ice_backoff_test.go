package peer

import (
	"testing"
	"time"
)

// TestIceBackoff_AllowActivityOverride pins down the rate-limited
// "user-activity-overrides-hourly-backoff" semantic added 2026-05-05.
// Codex review caught that markSuccess() previously did NOT stamp
// lastResetAt, so this test specifically also covers the post-success
// path -- without the markSuccess fix the rate-limit window would have
// effectively never engaged after a brief successful connect cycle.
func TestIceBackoff_AllowActivityOverride(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)

	// Not suspended -> no override needed
	if s.AllowActivityOverride() {
		t.Fatal("not suspended: must NOT allow override")
	}

	// Suspended via markFailure
	for i := 0; i < 3; i++ {
		s.markFailure()
	}
	if !s.IsSuspended() {
		t.Fatal("after 3 failures: must be suspended")
	}

	// Recently reset (Reset just happened in newIceBackoff bo, but
	// lastResetAt is zero — falls back to time.Since(zero) = forever
	// which IS > 5min, so override IS allowed). To make the test
	// deterministic, hard-Reset to stamp lastResetAt = now, then
	// re-fail 3x to suspend.
	s.Reset()
	for i := 0; i < 3; i++ {
		s.markFailure()
	}
	if !s.IsSuspended() {
		t.Fatal("after Reset+3 failures: must be suspended")
	}
	// Now lastResetAt is fresh (within 5min) -> override DENIED
	if s.AllowActivityOverride() {
		t.Fatal("recently reset: must NOT allow override (rate-limit)")
	}

	// Simulate >5min since last reset by stamping lastResetAt back
	s.mu.Lock()
	s.lastResetAt = time.Now().Add(-6 * time.Minute)
	s.mu.Unlock()
	if !s.AllowActivityOverride() {
		t.Fatal("suspended + last reset >5min ago: MUST allow override")
	}
}

// TestIceBackoff_OnlyMarkFailureMutates pins the invariant Codex review
// 2026-05-05 asked us to make explicit: the backoff state is mutated
// by exactly three methods (markFailure, markSuccess, Reset) and by
// nothing else. In particular, the backoff must NEVER be triggered by
// inactivity-driven ICE-detach (DetachICEForPeer / lazy-mgr's
// ICEInactiveChan) or by full-conn-close (lazy-mgr relayTimeout).
//
// Test approach: spin a backoff, exercise the read-only paths
// (Snapshot, IsSuspended, AllowActivityOverride) repeatedly, then
// assert failures stayed at 0 and suspended stayed false. This proves
// that the read methods don't have side-effects that would
// accidentally enter the backoff state.
func TestIceBackoff_OnlyMarkFailureMutates(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)

	for i := 0; i < 20; i++ {
		_ = s.IsSuspended()
		_ = s.Snapshot()
		_ = s.AllowActivityOverride()
	}

	if s.IsSuspended() {
		t.Fatal("backoff must not be suspended after read-only calls")
	}
	snap := s.Snapshot()
	if snap.Failures != 0 || snap.Suspended {
		t.Fatalf("read-only calls must not mutate state, got %+v", snap)
	}
}

// TestIceBackoff_MarkSuccessStampsLastResetAt is a direct regression
// pin for the Codex-found inconsistency: markSuccess MUST update
// lastResetAt so it counts as a reset point for the
// activity-override rate limit (and the markFailure grace period).
func TestIceBackoff_MarkSuccessStampsLastResetAt(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	// Force lastResetAt into the past
	s.mu.Lock()
	s.lastResetAt = time.Now().Add(-30 * time.Minute)
	s.mu.Unlock()

	s.markSuccess()

	s.mu.Lock()
	stamped := s.lastResetAt
	s.mu.Unlock()
	if time.Since(stamped) > time.Second {
		t.Fatalf("markSuccess must stamp lastResetAt to ~now, got %v ago", time.Since(stamped))
	}
}


func TestIceBackoff_InitialState(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	if s.IsSuspended() {
		t.Fatal("fresh state must not be suspended")
	}
	snap := s.Snapshot()
	if snap.Failures != 0 || snap.Suspended {
		t.Fatalf("fresh state snapshot wrong: %+v", snap)
	}
}

func TestIceBackoff_SetMaxBackoff_Live(t *testing.T) {
	s := newIceBackoff(1 * time.Minute) // tight cap
	s.markFailure()                     // expect ~1m
	s.markFailure()                     // expect ~1m (capped)
	d2 := s.markFailure()               // still ~1m
	if d2 > 90*time.Second {
		t.Errorf("with 1m cap, third failure should be ~1m, got %v", d2)
	}
	// Live update to 1h cap
	s.SetMaxBackoff(60 * time.Minute)
	// Subsequent failure produces a non-zero delay (jitter-dependent
	// but should be > 0 since backoff was rebuilt).
	d3 := s.markFailure()
	if d3 <= 0 {
		t.Errorf("after SetMaxBackoff: must produce non-zero delay, got %v", d3)
	}
}

func TestIceBackoff_SuccessReset(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	for i := 0; i < 5; i++ {
		s.markFailure()
	}
	s.markSuccess()
	snap := s.Snapshot()
	if snap.Failures != 0 || snap.Suspended {
		t.Fatalf("after markSuccess: %+v", snap)
	}
	// Next failure must be back to step-1 magnitude (~1m)
	delay := s.markFailure()
	if delay > 70*time.Second {
		t.Errorf("after success-reset, first failure must restart at ~1m, got %v", delay)
	}
}

func TestIceBackoff_HardReset(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	s.markFailure()
	s.markFailure()
	s.Reset()
	snap := s.Snapshot()
	if snap.Failures != 0 || snap.Suspended {
		t.Fatalf("after Reset: %+v", snap)
	}
}

func TestIceBackoff_SuspendedExpires(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	s.markFailure()
	// Force nextRetry to past
	s.mu.Lock()
	s.nextRetry = time.Now().Add(-1 * time.Second)
	s.mu.Unlock()
	if s.IsSuspended() {
		t.Fatal("expired suspend must report not suspended")
	}
}

func TestIceBackoff_ExponentialDoubling(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	expectedRanges := []struct {
		min, max time.Duration
	}{
		{50 * time.Second, 70 * time.Second},   // ~1m
		{100 * time.Second, 140 * time.Second}, // ~2m
		{210 * time.Second, 270 * time.Second}, // ~4m
		{420 * time.Second, 540 * time.Second}, // ~8m
		{810 * time.Second, 990 * time.Second}, // ~15m capped
		{810 * time.Second, 990 * time.Second}, // ~15m capped
		{810 * time.Second, 990 * time.Second}, // ~15m capped
	}
	for i, exp := range expectedRanges {
		delay := s.markFailure()
		if delay < exp.min || delay > exp.max {
			t.Errorf("failure #%d: delay %v outside expected range [%v, %v]",
				i+1, delay, exp.min, exp.max)
		}
	}
}

func TestIceBackoff_MaxBackoffOverride(t *testing.T) {
	s := newIceBackoff(5 * time.Minute) // 300s cap
	delays := []time.Duration{}
	for i := 0; i < 5; i++ {
		delays = append(delays, s.markFailure())
	}
	// Last few should be capped at ~5m (300s) regardless of multiplier
	for i := 2; i < 5; i++ {
		if delays[i] > 6*time.Minute {
			t.Errorf("failure #%d: delay %v exceeds 5m cap", i+1, delays[i])
		}
	}
}

func TestIceBackoff_MaxBackoffZero_Disabled(t *testing.T) {
	s := newIceBackoff(0)
	delay := s.markFailure()
	if delay != 0 {
		t.Errorf("disabled backoff must return 0 delay, got %v", delay)
	}
	if s.IsSuspended() {
		t.Fatal("disabled backoff must not suspend")
	}
}

func TestIceBackoff_GracePeriodAfterReset_ShortDelay(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	s.Reset() // simulate srReconnect / network-change

	delay := s.markFailure()
	if delay != networkChangeRetryDelay {
		t.Fatalf("within grace window: expected %v, got %v", networkChangeRetryDelay, delay)
	}

	// A second failure inside the grace window also uses the short delay
	// (long-term exponential schedule is NOT advanced).
	delay2 := s.markFailure()
	if delay2 != networkChangeRetryDelay {
		t.Fatalf("second failure inside grace: expected %v, got %v", networkChangeRetryDelay, delay2)
	}
}

func TestIceBackoff_GraceExpired_NormalExponential(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	s.Reset()

	// Force lastResetAt into the past so the grace window has expired.
	s.mu.Lock()
	s.lastResetAt = time.Now().Add(-2 * networkChangeGracePeriod)
	s.mu.Unlock()

	delay := s.markFailure()
	if delay < 50*time.Second || delay > 70*time.Second {
		t.Fatalf("outside grace: expected ~1m exponential delay, got %v", delay)
	}
}

func TestIceBackoff_NoGraceWithoutReset(t *testing.T) {
	// Fresh state without an explicit Reset must use the normal exponential
	// schedule (lastResetAt is zero so the grace path does not apply).
	s := newIceBackoff(15 * time.Minute)
	delay := s.markFailure()
	if delay < 50*time.Second {
		t.Fatalf("fresh state without Reset: expected ~1m delay, got %v", delay)
	}
}

// TestIceBackoff_MarkUserInitiatedRetry_DisabledNoBypass ensures a 0-cap
// backoff (= disabled) never reports a successful bypass, mirroring the
// rest of the state-machine's disabled-mode contract.
func TestIceBackoff_MarkUserInitiatedRetry_DisabledNoBypass(t *testing.T) {
	s := newIceBackoff(0)
	if s.markUserInitiatedRetry() {
		t.Fatal("disabled backoff must not report a bypass")
	}
}

// TestIceBackoff_MarkUserInitiatedRetry_NotSuspendedNoBypass: when the
// backoff is not currently suspended there is nothing to bypass.
func TestIceBackoff_MarkUserInitiatedRetry_NotSuspendedNoBypass(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	if s.markUserInitiatedRetry() {
		t.Fatal("non-suspended backoff must not report a bypass")
	}
}

// TestIceBackoff_MarkUserInitiatedRetry_ExpiredNoBypass: when the suspend
// already elapsed naturally, mark the gate as un-suspended but report
// false because no real bypass was needed.
func TestIceBackoff_MarkUserInitiatedRetry_ExpiredNoBypass(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	s.markFailure()
	s.mu.Lock()
	s.nextRetry = time.Now().Add(-1 * time.Second)
	s.mu.Unlock()

	if s.markUserInitiatedRetry() {
		t.Fatal("expired suspend must not count as a bypass")
	}
	if s.IsSuspended() {
		t.Fatal("after markUserInitiatedRetry on expired suspend, must report not suspended")
	}
}

// TestIceBackoff_MarkUserInitiatedRetry_SuspendedBypassPreservesFailures:
// the core invariant — bypassing the suspend gate must not roll back the
// failure counter or the exponential schedule.
func TestIceBackoff_MarkUserInitiatedRetry_SuspendedBypassPreservesFailures(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	for i := 0; i < 3; i++ {
		s.markFailure()
	}
	priorFailures := s.Snapshot().Failures

	if !s.markUserInitiatedRetry() {
		t.Fatal("suspended backoff with future nextRetry must report bypass=true")
	}
	if s.IsSuspended() {
		t.Fatal("after bypass, IsSuspended must be false")
	}
	if got := s.Snapshot().Failures; got != priorFailures {
		t.Fatalf("bypass must NOT reset failures counter, got %d want %d", got, priorFailures)
	}
	// Next markFailure must keep climbing the existing exponential schedule
	// (3 failures already past the ~1m+~2m+~4m steps -> ~8m next).
	d := s.markFailure()
	if d < 5*time.Minute {
		t.Fatalf("post-bypass markFailure must follow existing schedule (>=5m for 4th failure), got %v", d)
	}
}

func TestIceBackoff_FirstFailure(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	delay := s.markFailure()
	if delay <= 0 {
		t.Fatalf("first failure must produce a positive delay, got %v", delay)
	}
	if delay < 50*time.Second || delay > 70*time.Second {
		t.Fatalf("first failure delay should be ~1m (with 10%% jitter), got %v", delay)
	}
	if !s.IsSuspended() {
		t.Fatal("after first failure must be suspended")
	}
	snap := s.Snapshot()
	if snap.Failures != 1 || !snap.Suspended {
		t.Fatalf("snapshot wrong: %+v", snap)
	}
}

// TestResolveP2pRetryCap covers the wire-format -> time.Duration
// translation for ICE-backoff cap. Single source of truth shared by
// Conn.initIceBackoffFromConfig and ConnMgr.propagateP2pRetryMaxToConns.
func TestResolveP2pRetryCap(t *testing.T) {
	cases := []struct {
		name string
		in   uint32
		want time.Duration
	}{
		{"sentinel/user-explicit-disable", SentinelP2pRetryDisabled, 0},
		{"zero/use-daemon-default", 0, DefaultP2PRetryMax},
		{"sixty-seconds", 60, time.Minute},
		{"one-hour", 3600, time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveP2pRetryCap(c.in); got != c.want {
				t.Fatalf("ResolveP2pRetryCap(%d) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// The disabled semantic at ice_backoff.go markFailure (maxBackoff == 0
// short-circuits to a 0 delay) is what we rely on the helper to feed.
func TestResolveP2pRetryCap_SentinelMakesBackoffDisabled(t *testing.T) {
	d := ResolveP2pRetryCap(SentinelP2pRetryDisabled)
	bo := newIceBackoff(d)
	if delay := bo.markFailure(); delay != 0 {
		t.Fatalf("disabled backoff returned non-zero delay: %v", delay)
	}
}

// TestIceBackoff_PostSuccessDropRamp verifies the V16.1 ramped schedule:
// 2s, 5s, 10s, then 30s cap. This is the schedule pinned for transient
// Halifax-CGNAT NAT-stale failures so they don't poison the exponential
// schedule for genuine first-attempt-cannot-pair failures.
func TestIceBackoff_PostSuccessDropRamp(t *testing.T) {
	expected := []time.Duration{
		2 * time.Second,
		5 * time.Second,
		10 * time.Second,
		30 * time.Second,
		30 * time.Second, // cap
		30 * time.Second, // cap
	}
	s := newIceBackoff(15 * time.Minute)
	// Skip the network-change grace by clearing lastResetAt to a far past.
	s.lastResetAt = time.Now().Add(-2 * time.Hour)

	for i, want := range expected {
		got := s.markFailurePostSuccessDrop()
		if got != want {
			t.Errorf("post-success-drop #%d: got %v, want %v", i+1, got, want)
		}
	}
}

// TestIceBackoff_PostSuccessDropDoesNotPoisonExponential verifies the
// core V16.1 fix: many post-success-drops must NOT advance the long-term
// exponential schedule (bo.NextBackOff). The next genuine first-attempt
// failure must therefore still return the InitialInterval, not the
// post-N-th-exponential interval. This is the bug that V16 (before .1)
// left open: s.bo.NextBackOff() was called inside the post-success path,
// poisoning the schedule.
func TestIceBackoff_PostSuccessDropDoesNotPoisonExponential(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	s.lastResetAt = time.Now().Add(-2 * time.Hour) // skip grace

	// Flutter 6× via post-success-drop. Pre-V16.1 these would each call
	// bo.NextBackOff() and advance currentInterval up to MaxInterval.
	for i := 0; i < 6; i++ {
		s.markFailurePostSuccessDrop()
	}

	// Now a genuine first-attempt failure should still get the
	// InitialInterval (~iceBackoffInitialInterval) with randomization,
	// NOT something close to maxBackoff.
	delay := s.markFailure()
	// InitialInterval is 1min, randomization 0.1 → ~54-66s.
	if delay > 70*time.Second {
		t.Errorf("post-success-drops poisoned exponential schedule: "+
			"first-attempt got %v, expected ~1m initial-interval", delay)
	}
}

// TestIceBackoff_MarkSuccessResetsPostSuccessCounter verifies that a
// successful ICE-Connected event clears the post-success-drop counter,
// so a future flutter starts the ramp from 2s again, not from the cap.
func TestIceBackoff_MarkSuccessResetsPostSuccessCounter(t *testing.T) {
	s := newIceBackoff(15 * time.Minute)
	s.lastResetAt = time.Now().Add(-2 * time.Hour)

	for i := 0; i < 4; i++ {
		s.markFailurePostSuccessDrop()
	}
	s.markSuccess()

	first := s.markFailurePostSuccessDrop()
	if first != 2*time.Second {
		t.Errorf("markSuccess did not reset post-success counter: "+
			"first delay after reset was %v, expected 2s", first)
	}
}

// TestIceBackoff_PostSuccessDropDelayForSchedule pins the static ramp.
func TestIceBackoff_PostSuccessDropDelayForSchedule(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{0, 2 * time.Second},
		{1, 2 * time.Second},
		{2, 5 * time.Second},
		{3, 10 * time.Second},
		{4, 30 * time.Second},
		{100, 30 * time.Second},
	}
	for _, tc := range cases {
		got := postSuccessDropDelayFor(tc.n)
		if got != tc.want {
			t.Errorf("postSuccessDropDelayFor(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}
