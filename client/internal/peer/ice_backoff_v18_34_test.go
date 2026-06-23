package peer

import (
	"testing"
	"time"
)

// TestIceBackoff_V18_34_IdleResetAfter15Min — V18.34 Phase 1 invariant:
// after idleResetThreshold without a fresh failure, the failure counter
// and exponential backoff state reset on the next markFailure, so a new
// failure cycle starts from a clean slate.
func TestIceBackoff_V18_34_IdleResetAfter15Min(t *testing.T) {
	s := newIceBackoff(60 * time.Second)

	// Simulate 5 consecutive failures (ramps exponential)
	for i := 0; i < 5; i++ {
		s.markFailure()
	}
	if s.failures != 5 {
		t.Fatalf("setup: failures=%d, want 5", s.failures)
	}

	// Rewind lastFailureAt to 16 min ago (just past idleResetThreshold)
	s.mu.Lock()
	s.lastFailureAt = time.Now().Add(-16 * time.Minute)
	s.mu.Unlock()

	// Next failure should idle-reset before incrementing
	s.markFailure()
	if s.failures != 1 {
		t.Fatalf("V18.34 idle-reset: failures=%d, want 1 (counter reset before increment)", s.failures)
	}
}

// TestIceBackoff_V18_34_NoResetWithin15Min — no reset if last failure
// was less than idleResetThreshold ago.
func TestIceBackoff_V18_34_NoResetWithin15Min(t *testing.T) {
	s := newIceBackoff(60 * time.Second)

	for i := 0; i < 5; i++ {
		s.markFailure()
	}

	// Rewind to 14 min ago (still within threshold)
	s.mu.Lock()
	s.lastFailureAt = time.Now().Add(-14 * time.Minute)
	s.mu.Unlock()

	s.markFailure()
	if s.failures != 6 {
		t.Fatalf("within-threshold: failures=%d, want 6 (no reset)", s.failures)
	}
}

// TestIceBackoff_V18_34_FirstFailureNoReset — lastFailureAt=zero must
// NOT trigger a reset (would be no-op but verify the guard).
func TestIceBackoff_V18_34_FirstFailureNoReset(t *testing.T) {
	s := newIceBackoff(60 * time.Second)

	if !s.lastFailureAt.IsZero() {
		t.Fatalf("setup: lastFailureAt not zero on fresh state")
	}
	s.markFailure()
	if s.failures != 1 {
		t.Fatalf("first failure: failures=%d, want 1", s.failures)
	}
	if s.lastFailureAt.IsZero() {
		t.Fatalf("after first failure, lastFailureAt must be set")
	}
}

// TestIceBackoff_V18_34_markSuccessClearsLastFailureAt — markSuccess
// must zero lastFailureAt so the next failure cycle starts fresh.
func TestIceBackoff_V18_34_markSuccessClearsLastFailureAt(t *testing.T) {
	s := newIceBackoff(60 * time.Second)

	s.markFailure()
	if s.lastFailureAt.IsZero() {
		t.Fatalf("setup: lastFailureAt zero after failure")
	}

	s.markSuccess()
	if !s.lastFailureAt.IsZero() {
		t.Fatalf("markSuccess did not clear lastFailureAt")
	}
}

// TestIceBackoff_V18_34_ResetClearsLastFailureAt — Reset() must zero
// lastFailureAt (network-change / srReconnect path).
func TestIceBackoff_V18_34_ResetClearsLastFailureAt(t *testing.T) {
	s := newIceBackoff(60 * time.Second)

	s.markFailure()
	s.Reset()
	if !s.lastFailureAt.IsZero() {
		t.Fatalf("Reset did not clear lastFailureAt")
	}
}

// TestIceBackoff_V18_34_PostSuccessDrop_AlsoResets — the
// markFailurePostSuccessDrop path must also honour the idle-reset.
func TestIceBackoff_V18_34_PostSuccessDrop_AlsoResets(t *testing.T) {
	s := newIceBackoff(60 * time.Second)

	for i := 0; i < 4; i++ {
		s.markFailurePostSuccessDrop()
	}
	if s.postSuccessFailures != 4 {
		t.Fatalf("setup: postSuccessFailures=%d, want 4", s.postSuccessFailures)
	}

	s.mu.Lock()
	s.lastFailureAt = time.Now().Add(-20 * time.Minute)
	s.mu.Unlock()

	s.markFailurePostSuccessDrop()
	if s.postSuccessFailures != 1 {
		t.Fatalf("V18.34 idle-reset (postSuccessDrop): postSuccessFailures=%d, want 1", s.postSuccessFailures)
	}
}

// TestIceBackoff_V18_34_AfterIdleResetUsesNetworkChangeGrace —
// Codex v2 amendment: after maybeIdleReset bumps lastResetAt, the next
// markFailure must honour the network-change grace period and use
// networkChangeRetryDelay (=2s) instead of the long exponential.
// This is the intended semantic: idle-reset is a "fresh context" event,
// equivalent to a network change.
func TestIceBackoff_V18_34_AfterIdleResetUsesNetworkChangeGrace(t *testing.T) {
	s := newIceBackoff(60 * time.Second)

	// Drive the exponential up so a NEW failure WITHOUT grace would
	// produce a delay much larger than networkChangeRetryDelay.
	for i := 0; i < 5; i++ {
		s.markFailure()
	}

	// Move lastFailureAt far enough back that idle-reset triggers,
	// AND lastResetAt far back so grace period is currently expired.
	s.mu.Lock()
	s.lastFailureAt = time.Now().Add(-20 * time.Minute)
	s.lastResetAt = time.Now().Add(-1 * time.Hour) // grace expired pre-reset
	s.mu.Unlock()

	// Next markFailure: maybeIdleReset should fire, bump lastResetAt
	// to now, and the delay computation should pick the grace branch.
	delay := s.markFailure()

	if delay != networkChangeRetryDelay {
		t.Fatalf("after idle-reset, next markFailure delay=%v, want %v (network-change grace)",
			delay, networkChangeRetryDelay)
	}
	if s.failures != 1 {
		t.Fatalf("after idle-reset, failures=%d, want 1", s.failures)
	}
}
