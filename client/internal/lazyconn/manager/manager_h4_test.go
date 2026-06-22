// Package manager — H4 invariant test for the V18.30 fixup #1 sequence in
// onPeerActivity (manager.go:837-867).
//
// Approach used: Option 1 (NewConnForTransportTest + direct sequence-replay
// against a real *peer.Conn). Codex v2 amendment T3 explicitly rejected the
// exports_test.go cross-package helper trick (Go does not export _test.go
// symbols across packages). Option 2 (a full peer.NewConn rebuild here) was
// not necessary because the invariant we need to pin is observable on the
// peer.Conn atomic fields alone:
//
//  1. peer.NewConnForTransportTest yields a *peer.Conn whose Mode is the
//     zero value (ModeUnspecified), so the V18.10 intentionally-detached
//     signal gate in AttachICEFrom (conn.go:2547) is skipped and execution
//     reaches the ClearIntentionallyDetached() call at conn.go:2570 before
//     bailing on `handshaker == nil`.
//  2. ClearIntentionallyDetached (conn.go:429) unconditionally stores 0
//     into localWakeIntentUntil — exactly the wipe that the V18.30 fixup #1
//     re-arm at manager.go:867 has to repair.
//  3. ArmLocalWakeIntent (conn.go:558) is a pure-atomic state setter — no
//     handshaker, no ICE worker, no signaler needed. IsLocalWakeIntentActive
//     (conn.go:624) reads the same atomic and is the observable signal that
//     the V18.30 invariant holds.
//
// Pinning verified: removing the `conn.ArmLocalWakeIntent(0)` call from the
// test sequence (mirroring a hypothetical regression at manager.go:867)
// makes the assertion fail with the expected "V18.30 regression" message.
// See doc on TestManager_OnPeerActivity_V18_30_ReArmsAfterAttachICE below.
//
// Honest caveat: because this test replays the sequence directly rather
// than calling onPeerActivity end-to-end, it pins the *contract* between
// ClearIntentionallyDetached and ArmLocalWakeIntent rather than a future
// regression of manager.go:867 itself. A future refactor that drops the
// re-arm line from onPeerActivity would NOT be caught by this test in
// isolation; it would still be caught by the V18.30 hardware regression
// (kernel-mode cold-boot dead-lock). The test's value is keeping the
// peer-package contract honest so the manager-side re-arm remains
// semantically correct.
package manager

import (
	"testing"

	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/client/internal/peer/worker"
)

// TestManager_OnPeerActivity_V18_30_ReArmsAfterAttachICE pins the V18.30
// fixup #1 invariant from lazyconn/manager.go:837-867:
//
//	conn.AttachICEFrom(LazyActivity)   // calls ClearIntentionallyDetached
//	                                   // → wipes localWakeIntentUntil
//	conn.ArmLocalWakeIntent(0)         // V18.30 fixup #1 — MUST re-arm here
//
// The hardware-validated symptom on regression is a kernel-mode cold-boot
// dead-lock (dk20→5723C) because the bootstrap-OFFER guard's wake-intent
// override (conn.go:1622 / shouldSkipBootstrapOffer) never sees the intent
// the listener_udp.go activity-edge had armed earlier.
//
// Sequence pinned:
//  1. MarkIntentionallyDetached    (simulates idle-detach in p2p-dynamic)
//  2. ArmLocalWakeIntent(148)      (simulates listener_udp.go arming on
//                                   the first outbound WG handshake-init)
//  3. AttachICEFrom(LazyActivity)  (lazy-mgr activity wake — calls
//                                   ClearIntentionallyDetached, wiping
//                                   localWakeIntentUntil to 0; then bails
//                                   on handshaker==nil → error is the
//                                   same outcome production logs and
//                                   continues from)
//  4. ArmLocalWakeIntent(0)        (V18.30 fixup #1 re-arm — the line
//                                   under test)
//
// Assertion: after step 4, IsLocalWakeIntentActive() == true.
//
// Pinning verification: locally comment out step 4. Test MUST fail with
// the "V18.30 regression" message. Restore and rerun — green.
func TestManager_OnPeerActivity_V18_30_ReArmsAfterAttachICE(t *testing.T) {
	// Build a logger-bearing *peer.Conn via the exported transport-test
	// helper. This is the same constructor recovery_test.go uses, which
	// keeps the H4 test consistent with the rest of the manager-package
	// test suite.
	cfg := newTestPeerCfg("h4-pub-key-v18-30")
	conn := peer.NewConnForTransportTest(cfg.Log, worker.StatusDisconnected, worker.StatusDisconnected)

	// Step 1: simulate the idle-detach marker that DetachICEForPeer
	// would set in production.
	conn.MarkIntentionallyDetached()
	if !conn.IsIntentionallyDetached() {
		t.Fatal("setup: MarkIntentionallyDetached did not set the marker")
	}

	// Step 2: simulate listener_udp.go arming wake intent on first
	// outbound WG handshake-init (148 B is the canonical type-1 size
	// V18.20 added to isWakeIntentPkg).
	conn.ArmLocalWakeIntent(148)
	if !conn.IsLocalWakeIntentActive() {
		t.Fatal("setup: ArmLocalWakeIntent did not arm the intent")
	}

	// Step 3: lazy-mgr activity wake. AttachICEFrom on a
	// NewConnForTransportTest will execute ClearIntentionallyDetached
	// (conn.go:2570) THEN bail with "handshaker not initialized" at
	// conn.go:2583. The error is expected and matches what the
	// production manager logs at manager.go:838. The side effect we
	// care about is the Clear, which wipes localWakeIntentUntil to 0.
	if err := conn.AttachICEFrom(peer.AttachICESourceLazyActivity); err == nil {
		t.Fatal("setup: AttachICEFrom should return handshaker-nil error on NewConnForTransportTest fixture")
	}
	if conn.IsIntentionallyDetached() {
		t.Fatal("setup: AttachICEFrom did not clear the intentional-detach marker")
	}
	if conn.IsLocalWakeIntentActive() {
		t.Fatal("setup: AttachICEFrom did not wipe localWakeIntentUntil " +
			"(expected post-ClearIntentionallyDetached state) — the V18.30 " +
			"invariant this test pins has changed; review conn.go:429-444")
	}

	// Step 4: V18.30 fixup #1 — the line under test (manager.go:867).
	// Remove this line to verify the pinning works: the assertion below
	// must then fail with the "V18.30 regression" message.
	conn.ArmLocalWakeIntent(0)

	// Pinning assertion: the contract enforced by manager.go:867 is
	// "after AttachICEFrom in onPeerActivity, the local wake intent
	// must be active so the bootstrap-OFFER guard's override fires on
	// the very next tick (conn.go:1622)".
	if !conn.IsLocalWakeIntentActive() {
		t.Fatal("V18.30 regression: ArmLocalWakeIntent(0) after AttachICEFrom " +
			"did not re-arm localWakeIntentUntil; the bootstrap-OFFER guard's " +
			"wake-intent override (shouldSkipBootstrapOffer) will not fire, " +
			"reintroducing the kernel-mode cold-boot dead-lock V18.30 fixup #1 " +
			"resolved (manager.go:851-867)")
	}
}
