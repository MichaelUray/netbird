package peer

import (
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/netbirdio/netbird/client/internal/peer/conntype"
	"github.com/netbirdio/netbird/shared/connectionmode"
	signalclient "github.com/netbirdio/netbird/shared/signal/client"
)

// Phase 3.7l (Fix-D D3b) regression tests for AttachICEOnRemoteOffer.
//
// Codex D3b spec (2026-05-29):
//   - Only Body_OFFER triggers this path (caller's responsibility)
//   - Only when mode == ModeP2PDynamic
//   - Only when iceBackoff is suspended (else fall through to standard path)
//   - Only when handshaker.iceListener == nil (no in-flight ICE)
//   - One bypass per minCooldown window
//   - Schedule-preserving (markUserInitiatedRetry, NEVER Reset)
//   - Failure-counter persists across the bypass

// makeRemoteOfferConn builds a minimal Conn wired for D3b tests:
//   - handshaker with a non-ready signaler so SendOffer fails gracefully
//   - workerICE present (so the workerICE-nil gate passes)
//   - iceBackoff configured for suspension
//   - mode p2p-dynamic, currentConnPriority Relay
func makeRemoteOfferConn(t *testing.T) *Conn {
	t.Helper()
	w := &WorkerICE{}
	c := &Conn{
		Log: log.WithField("peer", "test-d3b"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
		opened:              true,
		currentConnPriority: conntype.Relay,
		handshaker: &Handshaker{
			signaler: NewSignaler(&signalclient.MockClient{
				ReadyFunc: func() bool { return false },
			}, wgtypes.Key{}),
		},
		workerICE:  w,
		iceBackoff: newIceBackoff(15 * time.Minute),
	}
	c.everConnected.Store(true)
	return c
}

// TestConn_AttachICEOnRemoteOffer_NotP2PDynamic_NoBypass verifies the
// mode gate: in any non-dynamic mode, the function defers to the
// standard locked attach path WITHOUT consuming a bypass slot.
func TestConn_AttachICEOnRemoteOffer_NotP2PDynamic_NoBypass(t *testing.T) {
	c := makeRemoteOfferConn(t)
	c.config.Mode = connectionmode.ModeP2P // not p2p-dynamic
	c.iceBackoff.markFailure()             // backoff suspended
	bypassedBefore := c.iceBackoff.Snapshot().Suspended

	if err := c.AttachICEOnRemoteOffer(60 * time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Backoff state must NOT have been touched.
	if got := c.iceBackoff.Snapshot().Suspended; got != bypassedBefore {
		t.Errorf("backoff suspended changed from %v to %v in non-dynamic mode (no bypass should happen)",
			bypassedBefore, got)
	}
	if !c.lastRemoteOfferAttach.IsZero() {
		t.Error("lastRemoteOfferAttach was set even in non-dynamic mode")
	}
}

// TestConn_AttachICEOnRemoteOffer_BackoffNotSuspended_NoBypass verifies
// the suspended-only gate: if the backoff is healthy (no failures), the
// function defers without burning the cooldown slot.
func TestConn_AttachICEOnRemoteOffer_BackoffNotSuspended_NoBypass(t *testing.T) {
	c := makeRemoteOfferConn(t)
	// iceBackoff is fresh (not suspended)
	if err := c.AttachICEOnRemoteOffer(60 * time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.lastRemoteOfferAttach.IsZero() {
		t.Error("lastRemoteOfferAttach was set despite no bypass needed")
	}
}

// TestConn_AttachICEOnRemoteOffer_BackoffSuspended_BypassAllowed is the
// main success-path: backoff suspended, no listener attached, no recent
// bypass → bypass MUST fire, lastRemoteOfferAttach MUST advance, backoff
// counter MUST NOT reset.
func TestConn_AttachICEOnRemoteOffer_BackoffSuspended_BypassAllowed(t *testing.T) {
	c := makeRemoteOfferConn(t)
	// Push backoff into suspension with 3 failures so we can verify
	// the schedule-preserving contract.
	c.iceBackoff.markFailure()
	c.iceBackoff.markFailure()
	c.iceBackoff.markFailure()
	failuresBefore := c.iceBackoff.Snapshot().Failures

	if err := c.AttachICEOnRemoteOffer(60 * time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.lastRemoteOfferAttach.IsZero() {
		t.Error("lastRemoteOfferAttach must advance after a successful bypass")
	}
	// Failure counter is preserved (schedule-preserving — NOT Reset()).
	failuresAfter := c.iceBackoff.Snapshot().Failures
	if failuresAfter != failuresBefore {
		t.Errorf("failure counter changed from %d to %d — Reset() leaked in",
			failuresBefore, failuresAfter)
	}
}

// TestConn_AttachICEOnRemoteOffer_CooldownRateLimit verifies the
// per-cooldown gate: two consecutive bypass attempts within the cooldown
// window — the second one MUST be blocked, lastRemoteOfferAttach MUST
// NOT advance.
func TestConn_AttachICEOnRemoteOffer_CooldownRateLimit(t *testing.T) {
	c := makeRemoteOfferConn(t)
	c.iceBackoff.markFailure() // suspend it
	if err := c.AttachICEOnRemoteOffer(60 * time.Second); err != nil {
		t.Fatalf("first call: %v", err)
	}
	firstStamp := c.lastRemoteOfferAttach
	if firstStamp.IsZero() {
		t.Fatal("first call should have set lastRemoteOfferAttach")
	}

	// Re-suspend the backoff (the first bypass consumed the suspend
	// gate). markFailure adds a new suspension window so the gate trips
	// again on the next call.
	c.iceBackoff.markFailure()

	// Second call within cooldown → must be blocked.
	if err := c.AttachICEOnRemoteOffer(60 * time.Second); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !c.lastRemoteOfferAttach.Equal(firstStamp) {
		t.Errorf("lastRemoteOfferAttach advanced within cooldown: was=%s now=%s",
			firstStamp.Format(time.StampMicro),
			c.lastRemoteOfferAttach.Format(time.StampMicro))
	}
}

// TestConn_AttachICEOnRemoteOffer_AfterCooldown_AllowedAgain verifies
// that once the cooldown window elapses, the bypass slot is replenished.
//
// In production the listener gets cleared by the failure path
// (onICEFailed → DetachICE) before the next remote OFFER arrives. We
// simulate that by RemoveICEListener between calls so only the cooldown
// is under test.
func TestConn_AttachICEOnRemoteOffer_AfterCooldown_AllowedAgain(t *testing.T) {
	c := makeRemoteOfferConn(t)
	c.iceBackoff.markFailure()
	if err := c.AttachICEOnRemoteOffer(50 * time.Millisecond); err != nil {
		t.Fatalf("first call: %v", err)
	}
	firstStamp := c.lastRemoteOfferAttach

	// Simulate the failure-path cleanup: detach the listener so the
	// "listener attached" gate doesn't accidentally block the second
	// call. In production this is done by onICEFailed → DetachICE.
	c.handshaker.RemoveICEListener()

	// Wait past the cooldown window, re-suspend, second call must
	// advance the stamp.
	time.Sleep(60 * time.Millisecond)
	c.iceBackoff.markFailure()
	if err := c.AttachICEOnRemoteOffer(50 * time.Millisecond); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !c.lastRemoteOfferAttach.After(firstStamp) {
		t.Errorf("lastRemoteOfferAttach should advance after cooldown elapsed: was=%s now=%s",
			firstStamp.Format(time.StampMicro),
			c.lastRemoteOfferAttach.Format(time.StampMicro))
	}
}

// TestConn_AttachICEOnRemoteOffer_ListenerAttached_Blocks verifies the
// "no in-flight ICE" gate: if a listener is already attached, the
// function must NOT disturb the in-progress session.
func TestConn_AttachICEOnRemoteOffer_ListenerAttached_Blocks(t *testing.T) {
	c := makeRemoteOfferConn(t)
	c.handshaker.AddICEListener(func(o *OfferAnswer) {})
	c.iceBackoff.markFailure()

	if err := c.AttachICEOnRemoteOffer(60 * time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.lastRemoteOfferAttach.IsZero() {
		t.Error("lastRemoteOfferAttach was set despite listener-attached gate failing")
	}
	// Backoff suspended state must be unchanged (no markUserInitiatedRetry).
	if !c.iceBackoff.Snapshot().Suspended {
		t.Error("backoff was unsuspended despite listener-attached gate failing")
	}
}

// TestConn_AttachICEOnRemoteOffer_CooldownClearedOnICESuccess is the
// Codex review-polish 2026-05-29 contract: when ICE actually connects
// after a successful bypass, onICEConnected must clear
// lastRemoteOfferAttach so a subsequent failure within 60 s gets a
// fresh bypass slot instead of being silenced by stale cooldown.
func TestConn_AttachICEOnRemoteOffer_CooldownClearedOnICESuccess(t *testing.T) {
	c := makeRemoteOfferConn(t)
	c.iceBackoff.markFailure()
	if err := c.AttachICEOnRemoteOffer(60 * time.Second); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if c.lastRemoteOfferAttach.IsZero() {
		t.Fatal("precondition: lastRemoteOfferAttach must be set after bypass")
	}

	// Simulate ICE success.
	c.onICEConnected()

	if !c.lastRemoteOfferAttach.IsZero() {
		t.Errorf("lastRemoteOfferAttach not cleared after onICEConnected: still %s",
			c.lastRemoteOfferAttach.Format(time.StampMicro))
	}

	// Confirm a subsequent bypass within the cooldown window now goes
	// through (because the stamp is back to zero).
	c.handshaker.RemoveICEListener()
	c.iceBackoff.markFailure()
	if err := c.AttachICEOnRemoteOffer(60 * time.Second); err != nil {
		t.Fatalf("post-success call: %v", err)
	}
	if c.lastRemoteOfferAttach.IsZero() {
		t.Error("post-success bypass should have set lastRemoteOfferAttach again")
	}
}

// TestConn_AttachICEOnRemoteOffer_NilHandshaker_Errors verifies the
// error-path: no handshaker → caller gets an error so it can defer.
func TestConn_AttachICEOnRemoteOffer_NilHandshaker_Errors(t *testing.T) {
	c := &Conn{
		Log: log.WithField("peer", "test-d3b-nil"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
	}
	if err := c.AttachICEOnRemoteOffer(60 * time.Second); err == nil {
		t.Error("expected error with nil handshaker, got nil")
	}
}
