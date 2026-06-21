package peer

import (
	"testing"
	"time"

	"github.com/netbirdio/netbird/monotime"
)

// TestConn_V18_17_RecordInboundOfferBurst_ThresholdAtThird verifies
// that the new RecordInboundOfferBurst() accessor returns false for
// the first 2 OFFERs in a 30s window and true on the 3rd (= burst
// threshold reached → caller should release the V14/V15 gate).
func TestConn_V18_17_RecordInboundOfferBurst_ThresholdAtThird(t *testing.T) {
	conn := newMarkerTestConn(t)

	if r := conn.RecordInboundOfferBurst(); r {
		t.Fatal("first OFFER must not trip threshold")
	}
	if r := conn.RecordInboundOfferBurst(); r {
		t.Fatal("second OFFER must not trip threshold")
	}
	if r := conn.RecordInboundOfferBurst(); !r {
		t.Fatal("third OFFER MUST trip threshold (release gate)")
	}
	// After threshold, counter resets to 0 — next OFFER starts a
	// fresh accumulation (mirrors V18.13 reset-on-fire semantics).
	if c := conn.inboundOfferBurstCount.Load(); c != 0 {
		t.Fatalf("expected counter reset to 0 after threshold, got %d", c)
	}
}

// TestConn_V18_17_RecordInboundOfferBurst_WindowExpiresResets verifies
// that the 30s sliding window restarts the counter at 1 on the first
// OFFER after window expiry.
func TestConn_V18_17_RecordInboundOfferBurst_WindowExpiresResets(t *testing.T) {
	conn := newMarkerTestConn(t)

	_ = conn.RecordInboundOfferBurst() // count=1
	_ = conn.RecordInboundOfferBurst() // count=2
	if c := conn.inboundOfferBurstCount.Load(); c != 2 {
		t.Fatalf("setup: expected count=2, got %d", c)
	}

	// Force-expire the window by rewinding windowStart.
	conn.inboundOfferBurstWindowStart.Store(
		int64(monotime.Now()) - int64(2*v18_17OfferBurstWindow))

	if r := conn.RecordInboundOfferBurst(); r {
		t.Fatal("first OFFER after window expiry must not trip threshold")
	}
	if c := conn.inboundOfferBurstCount.Load(); c != 1 {
		t.Fatalf("expected count=1 (fresh window) after expiry, got %d", c)
	}
}

// TestConn_V18_17_ClearIntentionallyDetached_ResetsOfferBurst verifies
// that a successful activation (Clear) resets the burst counter so
// the NEXT detach cycle starts fresh.
func TestConn_V18_17_ClearIntentionallyDetached_ResetsOfferBurst(t *testing.T) {
	conn := newMarkerTestConn(t)

	conn.MarkIntentionallyDetached()
	_ = conn.RecordInboundOfferBurst() // count=1
	_ = conn.RecordInboundOfferBurst() // count=2
	if c := conn.inboundOfferBurstCount.Load(); c != 2 {
		t.Fatalf("setup: expected count=2, got %d", c)
	}

	conn.ClearIntentionallyDetached()

	if c := conn.inboundOfferBurstCount.Load(); c != 0 {
		t.Fatalf("V18.17: ClearIntentionallyDetached must reset OFFER-burst counter, got %d", c)
	}
	if ws := conn.inboundOfferBurstWindowStart.Load(); ws != 0 {
		t.Fatalf("V18.17: ClearIntentionallyDetached must reset OFFER-burst windowStart, got %d", ws)
	}
}

// TestConn_V18_17_ResetOfferBurstOnFirstEverConnected verifies that
// the V15 cold-boot path (peer reaches everConnected for the first
// time) resets the OFFER-burst counter so subsequent cycles re-arm
// from scratch.
func TestConn_V18_17_ResetOfferBurstOnFirstEverConnected(t *testing.T) {
	conn := newMarkerTestConn(t)

	_ = conn.RecordInboundOfferBurst() // count=1
	_ = conn.RecordInboundOfferBurst() // count=2

	// Simulate the first ever-connected transition.
	conn.ResetOfferBurst() // this is what the V15-clear-point would call

	if c := conn.inboundOfferBurstCount.Load(); c != 0 {
		t.Fatalf("V18.17: ResetOfferBurst must zero counter, got %d", c)
	}
}

// TestConn_V18_17_OfferBurstCountersResetOnRemark verifies Codex R2:
// the V18.17 OFFER-burst counters MUST reset on MarkIntentionallyDetached
// so a Clear → Mark cycle inside the 30s window does not carry stale
// counts forward. Without this reset, 2 OFFERs late in cycle N + 1
// OFFER early in cycle N+1 (within 30s of cycle-N start) would trip
// the threshold spuriously, bypassing V14 with only 1 real cycle-N+1
// OFFER. Same class as the V18.16 I-1 bug fixed for relayActivityCount.
func TestConn_V18_17_OfferBurstCountersResetOnRemark(t *testing.T) {
	conn := newMarkerTestConn(t)

	// Cycle 1: detach + 2 OFFERs.
	conn.MarkIntentionallyDetached()
	_ = conn.RecordInboundOfferBurst() // count=1
	_ = conn.RecordInboundOfferBurst() // count=2
	if c := conn.inboundOfferBurstCount.Load(); c != 2 {
		t.Fatalf("setup: expected count=2 in cycle 1, got %d", c)
	}

	// Clear + Mark cycle 2 within 30s window.
	conn.ClearIntentionallyDetached()
	conn.MarkIntentionallyDetached()

	if c := conn.inboundOfferBurstCount.Load(); c != 0 {
		t.Fatalf("V18.17 R2: counter must reset on Mark, got %d (stale from cycle 1)", c)
	}
	if ws := conn.inboundOfferBurstWindowStart.Load(); ws != 0 {
		t.Fatalf("V18.17 R2: windowStart must reset on Mark, got %d (stale from cycle 1)", ws)
	}

	// 1 OFFER in cycle 2: count must be 1, not 3.
	_ = conn.RecordInboundOfferBurst()
	if c := conn.inboundOfferBurstCount.Load(); c != 1 {
		t.Fatalf("V18.17 R2: after Mark-reset, first OFFER should set count=1, got %d", c)
	}
}

// TestConn_V18_17_BurstReleasePending_OneShot verifies the V18.10
// bypass flag (Codex R1): set on burst release, consumed exactly once
// by CompareAndSwap, returns false on subsequent calls.
func TestConn_V18_17_BurstReleasePending_OneShot(t *testing.T) {
	conn := newMarkerTestConn(t)

	// Initially: not pending.
	if conn.ConsumeBurstReleasePending() {
		t.Fatal("initial state must be not-pending")
	}

	// Set pending → first Consume returns true, clears flag.
	conn.SetBurstReleasePending()
	if !conn.ConsumeBurstReleasePending() {
		t.Fatal("Consume must return true when pending")
	}
	// Second Consume must return false (one-shot).
	if conn.ConsumeBurstReleasePending() {
		t.Fatal("Consume must return false on second call (flag was consumed)")
	}
}

var _ = time.Second // keep import alive even if unused in later edits
