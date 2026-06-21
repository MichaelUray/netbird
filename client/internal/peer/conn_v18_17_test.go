package peer

import (
	"testing"

	"github.com/netbirdio/netbird/monotime"
)

// TestConn_V18_17_RecordInboundOfferBurst_ThresholdAtThird verifies
// that the new RecordInboundOfferBurst() accessor returns false for
// the 1st OFFER in a 30s window and true on the 2nd (= burst
// threshold reached → caller should release the V14/V15 gate).
//
// V18.18 (2026-06-21): threshold lowered from 3 to 2 after Round-3
// live repro showed sender-guard rate-limits OFFERs to ~2/30s.
func TestConn_V18_17_RecordInboundOfferBurst_ThresholdAtSecond(t *testing.T) {
	conn := newMarkerTestConn(t)

	if r := conn.RecordInboundOfferBurst(); r {
		t.Fatal("first OFFER must not trip threshold")
	}
	if r := conn.RecordInboundOfferBurst(); !r {
		t.Fatal("second OFFER MUST trip threshold (V18.18 release gate)")
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

	// V18.18 threshold=2: a single OFFER leaves count=1 (below threshold,
	// no release). Use the public accessor so behavior stays under test.
	_ = conn.RecordInboundOfferBurst() // count=1
	if c := conn.inboundOfferBurstCount.Load(); c != 1 {
		t.Fatalf("setup: expected count=1, got %d", c)
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
	// V18.18 threshold=2: 1 OFFER leaves count=1 (sub-threshold).
	_ = conn.RecordInboundOfferBurst() // count=1
	if c := conn.inboundOfferBurstCount.Load(); c != 1 {
		t.Fatalf("setup: expected count=1, got %d", c)
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

	// V18.18 threshold=2: 1 OFFER leaves count=1 (sub-threshold).
	_ = conn.RecordInboundOfferBurst() // count=1

	// Simulate the first ever-connected transition.
	conn.ResetOfferBurst() // this is what the V15-clear-point would call

	if c := conn.inboundOfferBurstCount.Load(); c != 0 {
		t.Fatalf("V18.17: ResetOfferBurst must zero counter, got %d", c)
	}
}

// TestConn_V18_17_OfferBurstCountersResetOnRemark verifies Codex R2:
// the V18.17 OFFER-burst counters MUST reset on MarkIntentionallyDetached
// so a Clear → Mark cycle inside the 30s window does not carry stale
// counts forward. Without this reset, 1 OFFER late in cycle N + 1
// OFFER early in cycle N+1 (within 30s of cycle-N start) would trip
// the V18.18 threshold=2 spuriously, bypassing V14 with only 1 real
// cycle-N+1 OFFER. Same class as the V18.16 I-1 bug fixed for
// relayActivityCount.
func TestConn_V18_17_OfferBurstCountersResetOnRemark(t *testing.T) {
	conn := newMarkerTestConn(t)

	// Cycle 1: detach + 1 OFFER (sub-threshold, count=1).
	conn.MarkIntentionallyDetached()
	_ = conn.RecordInboundOfferBurst() // count=1
	if c := conn.inboundOfferBurstCount.Load(); c != 1 {
		t.Fatalf("setup: expected count=1 in cycle 1, got %d", c)
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

	// 1 OFFER in cycle 2: count must be 1 (V18.18: not yet=2, no release).
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

// TestConn_V18_17_V14GateReleasesAfterOfferBurst is a Conn-level test
// that exercises the threshold accessor on a peer that IS lazy-detached.
// (Full integration through ConnMgr is in conn_mgr_v18_17_test.go.)
//
// V18.18 (2026-06-21): threshold lowered from 3 to 2 — 2nd OFFER
// releases (previously 3rd).
func TestConn_V18_17_V14GateReleasesAfterOfferBurst(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.MarkIntentionallyDetached()
	// Simulate everConnected=true (V14 precondition).
	conn.everConnected.Store(true)

	if !conn.IsLazyDetached() {
		t.Fatal("setup: conn must report IsLazyDetached=true")
	}

	// 1st OFFER: V14 gate still blocks (counter < threshold).
	if r := conn.RecordInboundOfferBurst(); r {
		t.Fatal("V14: 1st OFFER must NOT release gate")
	}
	// 2nd OFFER: V18.18 release gate.
	if r := conn.RecordInboundOfferBurst(); !r {
		t.Fatal("V14: 2nd OFFER MUST release gate (V18.18 threshold=2)")
	}
}

// TestConn_V18_17_V15GateCounterSharedWithV14 verifies that the V15
// path (cold-boot, never-connected) shares the SAME counter as V14
// (no double-counting). A peer that hits V15 gate 2 times in 30s
// also gets the OFFER-burst release.
//
// V18.18 (2026-06-21): threshold lowered from 3 to 2.
func TestConn_V18_17_V15GateCounterSharedWithV14(t *testing.T) {
	conn := newMarkerTestConn(t)
	// V15 precondition: !everConnected and lazy mode
	// (newMarkerTestConn already sets the mode correctly)
	if conn.EverConnected() {
		t.Fatal("setup: conn must report EverConnected=false (V15 precondition)")
	}

	if r := conn.RecordInboundOfferBurst(); r {
		t.Fatal("V15: 1st OFFER must NOT release gate")
	}
	if r := conn.RecordInboundOfferBurst(); !r {
		t.Fatal("V15: 2nd OFFER MUST release gate (V18.18, shared counter with V14)")
	}
}

// TestConn_V18_18_Round3RealCadenceRepro pins the production-observed
// Round-3 cadence (2026-06-21, S21→dk20 dead-lock test): sender emits
// only 2 OFFERs in ~26 seconds. With V18.17 threshold=3 this never
// triggered release. With V18.18 threshold=2, the 2nd OFFER releases.
//
// This test is the single most important regression guard against
// "tune threshold back up without thinking" — any future change to
// v18_17OfferBurstMinBurst MUST also update this test.
func TestConn_V18_18_Round3RealCadenceRepro(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.MarkIntentionallyDetached()
	conn.everConnected.Store(true)

	// Round-3 production trace: S21 sent OFFER #1 at T+8.225 s,
	// OFFER #2 at T+28.448 s. Both inside the 30 s window.
	// Threshold must fire on OFFER #2.
	if r := conn.RecordInboundOfferBurst(); r {
		t.Fatal("V18.18 Round-3 repro: OFFER #1 must NOT release")
	}
	// Simulate the ~20 s gap — still within 30 s window.
	// We don't actually sleep; the test verifies counter logic only.
	if r := conn.RecordInboundOfferBurst(); !r {
		t.Fatal("V18.18 Round-3 repro: OFFER #2 MUST release " +
			"(V18.17 threshold=3 was insufficient for sender-guard-rate-limited cadence)")
	}
}
