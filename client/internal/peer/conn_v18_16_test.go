package peer

import (
	"testing"

	"github.com/netbirdio/netbird/monotime"
	"github.com/netbirdio/netbird/shared/connectionmode"
)

// TestConn_V18_16_BurstWakesEvenInCooldown verifies that 3+ outbound
// activity callbacks within a 30s window wake P2P even when the
// V18.11 30s post-detach cooldown is still active.
//
// Pre-V18.16: AttachICEOnRelayActivity bails on the cooldown check
// BEFORE the burst counter increments, so 3 user packets in 5s never
// wake. Post-V18.16: burst increments first; when count>=3 the
// cooldown is irrelevant for this cycle.
func TestConn_V18_16_BurstWakesEvenInCooldown(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.config.Mode = connectionmode.ModeP2PDynamic
	conn.MarkIntentionallyDetached()
	// stamp detach in the very recent past (within cooldown)
	conn.intentionallyDetachedAt.Store(int64(monotime.Now()))

	// Three back-to-back relay-activity callbacks. Pre-V18.16 they all
	// return false ("blocked-cooldown"). Post-V18.16 the third one
	// proceeds past the burst gate (count>=3 in the 30s window).
	r1 := conn.AttachICEOnRelayActivity()
	r2 := conn.AttachICEOnRelayActivity()
	r3 := conn.AttachICEOnRelayActivity()

	// First two return false because count<3. The THIRD passes the
	// burst gate. Whether it returns true depends on downstream
	// gates (handshaker presence, currentConnPriority, etc.) which
	// newMarkerTestConn does NOT initialise. So we cannot assert
	// `r3 == true` — but we CAN assert the burst counter advanced.
	if r1 || r2 {
		t.Fatalf("first two callbacks must not yet attempt; got r1=%v r2=%v", r1, r2)
	}
	// After the third callback the threshold is reached. The
	// production code resets the counter to 0 in that branch before
	// falling through to the downstream gates. So observing count==0
	// AFTER three calls is the canonical evidence that the burst gate
	// fired. Pre-V18.16 the counter would be 0 (never incremented,
	// cooldown bailed first). Post-V18.16 the counter goes 1→2→3→0
	// (reset on threshold). The discriminator is: did the burst path
	// actually run? We assert via the side-effect on the window-start
	// counter too — it should be 0 after the reset.
	count := conn.relayActivityCount.Load()
	windowStart := conn.relayActivityWindowStart.Load()
	if count != 0 || windowStart != 0 {
		// If we did NOT hit the reset path, count must have reached >=3
		// at some point. Either way the burst path ran.
		// Acceptable post-V18.16 states:
		//   - count==0, windowStart==0 (reset on threshold)
		//   - count>=3 in active window (if test ran before reset)
		// Unacceptable (pre-V18.16): count==0 AND windowStart==0 only
		// because cooldown bailed before the burst code ever ran.
		// To disambiguate, force a 4th callback: post-V18.16 we should
		// start a fresh window with count=1; pre-V18.16 we still bail
		// in cooldown with count==0.
		if count < int32(3) {
			t.Fatalf("V18.16: burst counter should have advanced to >=3 within window before reset; got count=%d windowStart=%d", count, windowStart)
		}
	}
	// Drive one more callback. Post-V18.16: after reset, a fresh
	// window starts with count=1. Pre-V18.16: the cooldown bail keeps
	// count=0 (or count=1 from the fresh-window branch but only
	// because cooldown bails AFTER that branch — no, actually pre-
	// V18.16 cooldown bails FIRST, before the burst code runs at all).
	_ = conn.AttachICEOnRelayActivity()
	finalCount := conn.relayActivityCount.Load()
	if finalCount < int32(1) {
		t.Fatalf("V18.16: after burst-fired reset, next callback should start a fresh window with count>=1; got %d", finalCount)
	}
	// r3 ignored — the test scope is "burst counter advances past 3
	// even within cooldown window". Downstream gates are covered by
	// existing AttachICEOnRelayActivity tests.
	_ = r3
}

// TestConn_V18_16_SingleCallbackInCooldownStillBlocked verifies the
// fallback: a single isolated callback (e.g. an Android system probe)
// arriving within the cooldown is still rejected.
func TestConn_V18_16_SingleCallbackInCooldownStillBlocked(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.config.Mode = connectionmode.ModeP2PDynamic
	conn.MarkIntentionallyDetached()
	conn.intentionallyDetachedAt.Store(int64(monotime.Now()))

	r := conn.AttachICEOnRelayActivity()
	if r {
		t.Fatal("single isolated callback within cooldown must not attempt re-attach")
	}
	// Counter advanced to 1 (fresh window), but burst threshold (3)
	// not yet met. Cooldown still applies as the fallback gate.
	if c := conn.relayActivityCount.Load(); c != 1 {
		t.Fatalf("expected burst count=1 after single callback, got %d", c)
	}
}

// TestConn_V18_16_BurstWindowExpiresResetsCounter verifies that an
// expired 30s window starts a fresh count of 1.
func TestConn_V18_16_BurstWindowExpiresResetsCounter(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.config.Mode = connectionmode.ModeP2PDynamic
	conn.MarkIntentionallyDetached()
	conn.intentionallyDetachedAt.Store(int64(monotime.Now()))

	// First callback starts the window with count=1.
	_ = conn.AttachICEOnRelayActivity()
	if c := conn.relayActivityCount.Load(); c != 1 {
		t.Fatalf("expected count=1 after first callback, got %d", c)
	}

	// Move window start far into the past so the next callback sees
	// it as expired and starts a fresh window.
	expiredStart := int64(monotime.Now()) - int64(2*v18_13BurstWindow)
	conn.relayActivityWindowStart.Store(expiredStart)

	_ = conn.AttachICEOnRelayActivity()
	if c := conn.relayActivityCount.Load(); c != 1 {
		t.Fatalf("expired window should reset count to 1, got %d", c)
	}
	if ws := conn.relayActivityWindowStart.Load(); ws == expiredStart {
		t.Fatal("window start should have been updated on expiry")
	}
}
