package peer

import (
	"context"
	"net/netip"
	"testing"

	"github.com/netbirdio/netbird/client/internal/peer/guard"
)

// Phase 3.7j Commit 1 of 5 (#5989): unit tests for the intentionally-
// detached marker added on peer.Conn. The marker is consumed by the
// Phase-3.7j guard predicate (Commit 2) so the guard's retry budget is
// not burned by remote/local GO_IDLE or local-inactivity-driven ICE
// detach. These tests cover the marker itself and the six explicit
// clear-points wired in this commit:
//
//	1. AttachICE                           (signal-driven re-attach)
//	2. AttachICEUserInitiated              (local-traffic re-attach)
//	3. AttachICEOnRelayActivity            (relay-activity re-attach)
//	4. ConnMgr.ActivatePeer pre-Open       (covered in conn_mgr tests)
//	5. onNetworkChange                     (SR-watcher reconnect)
//	6. onICEFailed                         (real pion failure)
//
// Set point (DetachICEForPeer marking the flag) is covered indirectly:
// the smoke test verifies the marker round-trips on the Conn itself.
// Full ConnMgr integration is deferred to Commit 3 where DeactivatePeer
// dispatch is restructured.

// newMarkerTestConn returns a minimal *Conn for marker-only assertions.
// We deliberately do NOT call Open() because Open boots a full ICE/relay
// pipeline that is not needed for verifying the atomic.Bool marker and
// its six clear-points.
func newMarkerTestConn(t *testing.T) *Conn {
	t.Helper()
	swWatcher := guard.NewSRWatcher(nil, nil, nil, connConf.ICEConfig)
	sd := ServiceDependencies{
		StatusRecorder:     NewRecorder("https://mgm"),
		SrWatcher:          swWatcher,
		PeerConnDispatcher: testDispatcher,
	}
	cfg := connConf
	cfg.WgConfig = WgConfig{
		RemoteKey:   "remote-peer-key",
		WgInterface: &stubWGIface{},
		AllowedIps:  []netip.Prefix{netip.MustParsePrefix("100.64.0.5/32")},
	}
	conn, err := NewConn(cfg, sd)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	return conn
}

// TestConn_MarkerInitialStateFalse pins the zero-value behaviour of the
// atomic.Bool field: a freshly constructed Conn must report
// IsIntentionallyDetached() == false. Sanity check guards against
// someone "helpfully" changing the field to e.g. a pointer that would
// nil-panic on the first read.
func TestConn_MarkerInitialStateFalse(t *testing.T) {
	conn := newMarkerTestConn(t)
	if conn.IsIntentionallyDetached() {
		t.Fatal("freshly constructed Conn must report IsIntentionallyDetached()==false")
	}
}

// TestConn_MarkIntentionallyDetached_SetsFlag covers the set side of
// the marker contract: after MarkIntentionallyDetached, the getter must
// return true. Without this guarantee the guard predicate cannot
// distinguish intentional detach from a real ICE failure.
func TestConn_MarkIntentionallyDetached_SetsFlag(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.MarkIntentionallyDetached()
	if !conn.IsIntentionallyDetached() {
		t.Fatal("MarkIntentionallyDetached did not set the marker")
	}
	// Idempotency: a second Mark must remain set, not toggle.
	conn.MarkIntentionallyDetached()
	if !conn.IsIntentionallyDetached() {
		t.Fatal("repeated MarkIntentionallyDetached must remain set (idempotent)")
	}
}

// TestConn_ClearIntentionallyDetached_ResetsFlag covers the clear side
// of the marker contract: after Mark+Clear, IsIntentionallyDetached
// must report false. Without this the marker would latch on across an
// entire daemon session and Commit 2's predicate would never re-engage
// the retry budget.
func TestConn_ClearIntentionallyDetached_ResetsFlag(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.MarkIntentionallyDetached()
	conn.ClearIntentionallyDetached()
	if conn.IsIntentionallyDetached() {
		t.Fatal("ClearIntentionallyDetached did not reset the marker")
	}
	// Idempotency: clearing again on a cleared marker is a no-op.
	conn.ClearIntentionallyDetached()
	if conn.IsIntentionallyDetached() {
		t.Fatal("repeated ClearIntentionallyDetached must remain cleared (idempotent)")
	}
}

// TestConn_AttachICEClearsMarker verifies clear-points #1 and #2
// (AttachICE, AttachICEUserInitiated). Both methods are expected to
// clear the marker BEFORE acquiring conn.mu, so an error return from
// the unopened state ("handshaker not initialized") still demonstrates
// the clear semantically -- the marker must be false even though the
// underlying attach failed. This mirrors the production path where the
// clear must happen on the wake-up edge regardless of downstream
// success/failure.
//
// AttachICEOnRelayActivity (clear-point #3) is verified separately
// below because it has additional Mode/state guards.
func TestConn_AttachICEClearsMarker(t *testing.T) {
	t.Run("AttachICE", func(t *testing.T) {
		conn := newMarkerTestConn(t)
		conn.MarkIntentionallyDetached()
		// Expected to error because Open() was not called; the clear is
		// still required to run as it sits above conn.mu.Lock().
		_ = conn.AttachICE()
		if conn.IsIntentionallyDetached() {
			t.Fatal("AttachICE did not clear the marker (clear-point #1)")
		}
	})

	t.Run("AttachICEUserInitiated", func(t *testing.T) {
		conn := newMarkerTestConn(t)
		conn.MarkIntentionallyDetached()
		// minCooldown irrelevant on this path -- iceBackoff is nil so
		// the user-initiated bypass logic falls through to the
		// handshaker-nil error after the clear has fired.
		_ = conn.AttachICEUserInitiated(0)
		if conn.IsIntentionallyDetached() {
			t.Fatal("AttachICEUserInitiated did not clear the marker (clear-point #2)")
		}
	})
}

// TestConn_AttachICEOnRelayActivityClearsMarker verifies clear-point #3.
// The method returns false immediately when the Conn is not in
// ModeP2PDynamic, but the clear sits above that gate so it must still
// fire. Without this clear, a relay-activity-driven wake-up after an
// intentional detach would leave the marker latched and Commit 2's
// guard predicate would silently skip the next legitimate fail.
func TestConn_AttachICEOnRelayActivityClearsMarker(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.MarkIntentionallyDetached()
	// Default ConnConfig is not ModeP2PDynamic, so the method early-
	// returns false. The clear should still have run.
	if attempted := conn.AttachICEOnRelayActivity(); attempted {
		t.Fatal("AttachICEOnRelayActivity unexpectedly attempted attach on non-dynamic mode")
	}
	if conn.IsIntentionallyDetached() {
		t.Fatal("AttachICEOnRelayActivity did not clear the marker (clear-point #3)")
	}
}

// TestConn_OnICEFailedClearsMarker verifies clear-point #6 (non-
// optional per spec). A real pion failure immediately after an
// intentional detach must NOT remain masked: the marker must be
// cleared so the guard's predicate (Commit 2) treats this as a
// legitimate failure and proceeds with backoff escalation.
//
// onICEFailed has an early-return on iceBackoff==nil, so the marker
// clear sits at the very top of the function (above that check).
func TestConn_OnICEFailedClearsMarker(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.MarkIntentionallyDetached()
	// iceBackoff is nil -- the function will early-return after the
	// clear. That's exactly the path we want to verify: even on the
	// shortest possible execution path, the clear must fire.
	conn.onICEFailed()
	if conn.IsIntentionallyDetached() {
		t.Fatal("onICEFailed did not clear the marker (clear-point #6, non-optional)")
	}
}

// TestConn_OnNetworkChangeClearsMarker verifies clear-point #5 (SR-
// watcher reconnect wired via the existing onNetworkChange callback).
// Network events invalidate any previous "intentionally idle"
// reasoning -- the path may be entirely different now -- so the marker
// must be cleared before the upcoming guard-driven ICE cycle starts.
//
// onNetworkChange normally runs on a fully Open()ed Conn: it acquires
// conn.mu and reads conn.ctx, then touches conn.iceBackoff and
// conn.workerICE (both nil-safe). To avoid booting the full ICE/relay
// pipeline we manually wire just the minimum scaffolding the function
// needs: conn.ctx must be non-nil so conn.ctx.Err() doesn't panic.
// Every other field path is nil-safe (handshaker == nil and
// workerICE == nil short-circuit the rest of the body, so the function
// runs cleanly past the marker clear without needing Open()).
//
// This deliberately exercises the production code path -- the
// clear-point #5 wiring is verified end-to-end at the unit level, not
// only by source inspection.
func TestConn_OnNetworkChangeClearsMarker(t *testing.T) {
	conn := newMarkerTestConn(t)

	// Minimum scaffolding for onNetworkChange: a live context so
	// conn.ctx.Err() returns nil and the function proceeds past the
	// guard at the top of the locked section. Every other field path
	// inside onNetworkChange is nil-safe on a not-yet-Open()ed Conn:
	//   - iceBackoff:        nil -> skipped
	//   - workerICE:         nil -> skipped (relay-forced-style branch)
	//   - attachICEListener: handshaker==nil -> early-return false
	conn.ctx, conn.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(conn.ctxCancel)

	conn.MarkIntentionallyDetached()
	conn.onNetworkChange()
	if conn.IsIntentionallyDetached() {
		t.Fatal("onNetworkChange did not clear the marker (clear-point #5)")
	}
}

// TestConn_IsLazyDetached_OpenedFalseShortCircuit verifies the V17.2 fix:
// when conn.opened is false (peer fully closed by relayTimeout / engine
// shutdown / explicit close), IsLazyDetached must return false even if
// the intentionally-detached marker is set and everConnected is true.
//
// Rationale: a fully-closed peer is in long-idle state. A remote-
// initiated OFFER from a legacy peer is a legitimate reconnect attempt,
// not bootstrap-spam — the anti-spam gate must let it through so the
// peer can come back online. Without this short-circuit S26 stuck with
// Marl Creek (2026-06-06): WG endpoint had drifted to public IP, the
// activity-listener never triggered, and V14 blocked every recovery
// path — leaving the peer permanently unreachable.
func TestConn_IsLazyDetached_OpenedFalseShortCircuit(t *testing.T) {
	conn := newMarkerTestConn(t)

	// Simulate the "post-success, lazy-detached, then fully closed" state.
	conn.everConnected.Store(true)
	conn.MarkIntentionallyDetached()
	conn.opened = false // simulates Close() result

	if conn.IsLazyDetached() {
		t.Fatal("IsLazyDetached must return false when opened=false (V17.2 short-circuit), even with everConnected+intentionallyDetached set")
	}
}

// TestConn_IsLazyDetached_OpenedTrueStillGated verifies that the V17.2
// short-circuit does NOT regress the original V13/V14/V14.1 gate.
// When conn.opened is true (active lazy-pause), IsLazyDetached must
// continue to return true so anti-spam gating still applies.
func TestConn_IsLazyDetached_OpenedTrueStillGated(t *testing.T) {
	conn := newMarkerTestConn(t)

	conn.everConnected.Store(true)
	conn.MarkIntentionallyDetached()
	conn.opened = true // simulates active lazy-pause (ICE detached, conn still open)

	if !conn.IsLazyDetached() {
		t.Fatal("IsLazyDetached must remain true when opened=true + everConnected + intentionallyDetached (V14 gate preserved)")
	}
}

// TestConn_IsLazyDetached_FreshConnNotDetached verifies a brand-new conn
// (never connected, never marked) does not report lazy-detached. Sanity
// check for the V17.2 change.
func TestConn_IsLazyDetached_FreshConnNotDetached(t *testing.T) {
	conn := newMarkerTestConn(t)

	if conn.IsLazyDetached() {
		t.Fatal("IsLazyDetached must return false on a fresh conn (no everConnected, no marker)")
	}
}
