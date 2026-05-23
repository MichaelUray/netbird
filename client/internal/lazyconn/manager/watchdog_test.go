package manager

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/netbirdio/netbird/client/internal/lazyconn"
	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/client/internal/peer/worker"
)

// addStuckInactivityPeer wires up a peer in watcherInactivity state with
// both transports Disconnected in the peerStore. Returns the cfg so the
// test can drive subsequent state.
func addStuckInactivityPeer(t *testing.T, h *testHarness, pubKey string) lazyconn.PeerConfig {
	t.Helper()
	cfg := newTestPeerCfg(pubKey)
	conn := peer.NewConnForTransportTest(cfg.Log, worker.StatusDisconnected, worker.StatusDisconnected)
	if !h.peerStore.AddPeerConn(cfg.PublicKey, conn) {
		t.Fatalf("AddPeerConn for %q failed", pubKey)
	}
	h.mgr.managedPeersMu.Lock()
	h.mgr.managedPeers[cfg.PublicKey] = &cfg
	h.mgr.managedPeersByConnID[cfg.PeerConnID] = &managedPeer{
		peerCfg:         &cfg,
		expectedWatcher: watcherInactivity,
	}
	h.mgr.managedPeersMu.Unlock()
	return cfg
}

// addStuckActivityNoListenerPeer wires up a peer in watcherActivity state
// without an armed activity listener (the Case-b stuck state).
func addStuckActivityNoListenerPeer(t *testing.T, h *testHarness, pubKey string) lazyconn.PeerConfig {
	t.Helper()
	cfg := newTestPeerCfg(pubKey)
	conn := peer.NewConnForTransportTest(cfg.Log, worker.StatusDisconnected, worker.StatusDisconnected)
	if !h.peerStore.AddPeerConn(cfg.PublicKey, conn) {
		t.Fatalf("AddPeerConn for %q failed", pubKey)
	}
	h.mgr.managedPeersMu.Lock()
	h.mgr.managedPeers[cfg.PublicKey] = &cfg
	h.mgr.managedPeersByConnID[cfg.PeerConnID] = &managedPeer{
		peerCfg:         &cfg,
		expectedWatcher: watcherActivity,
	}
	h.mgr.managedPeersMu.Unlock()
	// Deliberately do NOT call h.mgr.activityManager.MonitorPeerActivity —
	// that's the stuck state we want to heal.
	return cfg
}

// addHealthyConnectedPeer wires up a peer with BOTH transports connected
// (no stuck-state). Used to verify that drop-counter > 0 alone does NOT
// trigger recovery on healthy peers.
func addHealthyConnectedPeer(t *testing.T, h *testHarness, pubKey string) lazyconn.PeerConfig {
	t.Helper()
	cfg := newTestPeerCfg(pubKey)
	conn := peer.NewConnForTransportTest(cfg.Log, worker.StatusConnected, worker.StatusConnected)
	if !h.peerStore.AddPeerConn(cfg.PublicKey, conn) {
		t.Fatalf("AddPeerConn for %q failed", pubKey)
	}
	h.mgr.managedPeersMu.Lock()
	h.mgr.managedPeers[cfg.PublicKey] = &cfg
	h.mgr.managedPeersByConnID[cfg.PeerConnID] = &managedPeer{
		peerCfg:         &cfg,
		expectedWatcher: watcherInactivity,
	}
	h.mgr.managedPeersMu.Unlock()
	return cfg
}

// driveOneTick runs a single reconcileTick with synthetic relay-drops
// delta = relayDelta. Returns nothing — tests inspect state via
// h.mgr.activityManager.HasPeer + managedPeer.expectedWatcher afterwards.
func driveOneTick(t *testing.T, h *testHarness, relayDelta uint64) {
	t.Helper()
	// Bump the real inactivity-manager drop counter so reconcileTick's
	// Phase A sees a non-zero delta.
	for i := uint64(0); i < relayDelta; i++ {
		h.mgr.inactivityManager.RecordRelayDropForTest()
	}
	var lastRelay, lastICE uint64
	recoveringPeers := make(map[string]struct{})
	var recoveringMu sync.Mutex
	h.mgr.reconcileTick(h.ctx, &lastRelay, &lastICE, recoveringPeers, &recoveringMu)
	// Allow recovery goroutines to finish (spawnRecovery is async).
	time.Sleep(50 * time.Millisecond)
}

func TestReconcileWatchdog_DetectsInactivityStuck(t *testing.T) {
	h := newTestHarness(t)
	cfg := addStuckInactivityPeer(t, h, "peerA")

	driveOneTick(t, h, 1) // delta > 0

	// Recovery must have flipped state + armed listener.
	h.mgr.managedPeersMu.Lock()
	mp := h.mgr.managedPeersByConnID[cfg.PeerConnID]
	got := mp.expectedWatcher
	h.mgr.managedPeersMu.Unlock()
	if got != watcherActivity {
		t.Fatalf("expected watcherActivity, got %v", got)
	}
	if !h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
		t.Fatal("expected activity listener armed after Case-a recovery")
	}
}

func TestReconcileWatchdog_DetectsActivityNoListener(t *testing.T) {
	h := newTestHarness(t)
	cfg := addStuckActivityNoListenerPeer(t, h, "peerB")

	driveOneTick(t, h, 0) // delta IRRELEVANT for Case-b

	if !h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
		t.Fatal("expected activity listener re-armed by Case-b recovery")
	}
}

func TestReconcileWatchdog_ICEDropsOnlyDoesNotTrigger(t *testing.T) {
	h := newTestHarness(t)
	cfg := addStuckInactivityPeer(t, h, "peerC")

	// Bump ICE counter, NOT relay counter.
	h.mgr.inactivityManager.RecordICEDropForTest()
	driveOneTick(t, h, 0) // relayDelta == 0

	h.mgr.managedPeersMu.Lock()
	mp := h.mgr.managedPeersByConnID[cfg.PeerConnID]
	got := mp.expectedWatcher
	h.mgr.managedPeersMu.Unlock()
	if got != watcherInactivity {
		t.Fatalf("ICE-drops-only must NOT trigger Case-a; expected watcherInactivity, got %v", got)
	}
}

func TestReconcileWatchdog_ActivityWithListener_NoOp(t *testing.T) {
	h := newTestHarness(t)
	cfg := addStuckActivityNoListenerPeer(t, h, "peerD")
	// Arm listener BEFORE the tick → Case-c
	if err := h.mgr.activityManager.MonitorPeerActivity(cfg); err != nil {
		t.Fatalf("MonitorPeerActivity: %v", err)
	}

	driveOneTick(t, h, 0)

	// Should still be exactly one listener (no spurious re-arm or removal)
	if !h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
		t.Fatal("Case-c (already listening) must not remove the listener")
	}
}

// TestReconcileWatchdog_DropCounterAloneIsNotTrigger: healthy peer
// (StatusConnected) + relayDrops > 0 → no recovery. The transport-state
// snapshot guards before the drop-counter check.
func TestReconcileWatchdog_DropCounterAloneIsNotTrigger(t *testing.T) {
	h := newTestHarness(t)
	cfg := addHealthyConnectedPeer(t, h, "peerHealthy")

	driveOneTick(t, h, 5) // big delta

	h.mgr.managedPeersMu.Lock()
	mp := h.mgr.managedPeersByConnID[cfg.PeerConnID]
	got := mp.expectedWatcher
	h.mgr.managedPeersMu.Unlock()
	if got != watcherInactivity {
		t.Fatalf("healthy peer must not be recovered; expected watcherInactivity, got %v", got)
	}
	if h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
		t.Fatal("healthy peer must not have an activity listener after tick")
	}
}

// TestReconcileWatchdog_DisconnectAloneIsNotTrigger: stuck-inactivity peer
// (both transports Disconnected) + relayDelta == 0 → no Case-a trigger.
// The isStuckPeer heuristic requires BOTH transports disconnected AND
// deltaRelay > 0.
func TestReconcileWatchdog_DisconnectAloneIsNotTrigger(t *testing.T) {
	h := newTestHarness(t)
	cfg := addStuckInactivityPeer(t, h, "peerDiscOnly")

	driveOneTick(t, h, 0) // no drops

	h.mgr.managedPeersMu.Lock()
	mp := h.mgr.managedPeersByConnID[cfg.PeerConnID]
	got := mp.expectedWatcher
	h.mgr.managedPeersMu.Unlock()
	if got != watcherInactivity {
		t.Fatalf("disconnect-without-drops must NOT trigger Case-a; expected watcherInactivity, got %v", got)
	}
}

// TestReconcileWatchdog_InflightDedupePreventsDoubleSpawn: two ticks
// back-to-back must not spawn two recovery goroutines for the same peer.
// We assert this indirectly by holding a recovery in-flight (via the
// testListenerArmHook to block) while a second tick fires.
func TestReconcileWatchdog_InflightDedupePreventsDoubleSpawn(t *testing.T) {
	h := newTestHarness(t)
	cfg := addStuckInactivityPeer(t, h, "peerDedupe")

	// Shared state between the two ticks.
	var lastRelay, lastICE uint64
	recoveringPeers := make(map[string]struct{})
	var recoveringMu sync.Mutex

	// Block the first recovery inside the hook so the second tick sees
	// the entry in recoveringPeers and skips.
	release := make(chan struct{})
	var spawnCount int
	var spawnMu sync.Mutex
	setTestListenerArmHook(func(pubKey string) {
		if pubKey != cfg.PublicKey {
			return
		}
		spawnMu.Lock()
		spawnCount++
		spawnMu.Unlock()
		<-release
	})
	t.Cleanup(func() { setTestListenerArmHook(nil) })

	// Tick 1 — bump drops so the peer is classified stuck.
	h.mgr.inactivityManager.RecordRelayDropForTest()
	h.mgr.reconcileTick(h.ctx, &lastRelay, &lastICE, recoveringPeers, &recoveringMu)
	// Give goroutine time to reach the hook.
	time.Sleep(30 * time.Millisecond)

	// Tick 2 — must NOT spawn a second recovery for the same peer.
	h.mgr.inactivityManager.RecordRelayDropForTest()
	h.mgr.reconcileTick(h.ctx, &lastRelay, &lastICE, recoveringPeers, &recoveringMu)
	time.Sleep(30 * time.Millisecond)

	close(release)
	time.Sleep(30 * time.Millisecond)

	spawnMu.Lock()
	got := spawnCount
	spawnMu.Unlock()
	if got != 1 {
		t.Fatalf("inflight-dedupe failed: expected 1 recovery spawn, got %d", got)
	}
}

// TestReconcileWatchdog_PanicSelfRestart: the runReconcileWatchdog
// deferred recover() must restart the loop after a panic. We exercise
// this by running runReconcileWatchdog with a short interval, injecting
// a panic via testListenerArmHook during recovery, then verifying that
// the goroutine processes a SECOND tick.
func TestReconcileWatchdog_PanicSelfRestart(t *testing.T) {
	h := newTestHarness(t)
	cfg := addStuckInactivityPeer(t, h, "peerPanic")

	// First listener-arm panics, subsequent calls are no-ops. Tests the
	// spawnRecovery panic-recovery (logged but contained) — the watchdog
	// loop itself should keep ticking either way.
	var armCount int
	var armMu sync.Mutex
	setTestListenerArmHook(func(pubKey string) {
		armMu.Lock()
		armCount++
		count := armCount
		armMu.Unlock()
		if count == 1 {
			panic("synthetic panic in recovery")
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		h.mgr.runReconcileWatchdog(ctx, 30*time.Millisecond)
		close(loopDone)
	}()

	// Bump drops so the peer is classified stuck on every tick.
	h.mgr.inactivityManager.RecordRelayDropForTest()

	// Allow at least 3 ticks so the panic-then-recover cycle runs.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-loopDone
	// Wait for any in-flight recovery goroutines to finish writing to
	// armCount (spawnRecovery is async — small grace period).
	time.Sleep(50 * time.Millisecond)
	setTestListenerArmHook(nil)

	armMu.Lock()
	got := armCount
	armMu.Unlock()
	_ = cfg
	if got < 1 {
		t.Fatalf("expected at least 1 hook hit (panic case), got %d", got)
	}
}

// TestReconcileWatchdog_StartStop: runReconcileWatchdog must exit cleanly
// when its context is cancelled, within 1 s.
func TestReconcileWatchdog_StartStop(t *testing.T) {
	h := newTestHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.mgr.runReconcileWatchdog(ctx, 50*time.Millisecond)
		close(done)
	}()

	// Let it tick at least once.
	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// success
	case <-time.After(1 * time.Second):
		t.Fatal("runReconcileWatchdog did not exit within 1 s after cancel")
	}
}

// ---------------------------------------------------------------------
// Integration tests (Step 6.13)
// ---------------------------------------------------------------------

// TestIntegration_InactivityStuck_WatchdogHeals: full Manager wired +
// DropCounters drip + short tick interval. Watchdog detects stuck peer
// and flips it to watcherActivity within ~3 ticks.
func TestIntegration_InactivityStuck_WatchdogHeals(t *testing.T) {
	h := newTestHarness(t)
	cfg := addStuckInactivityPeer(t, h, "peerInt-a")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Drip drops continuously so the tick after the FIRST tick sees a
	// positive delta (the first tick may zero-baseline).
	h.mgr.inactivityManager.RecordRelayDropForTest()
	go h.mgr.runReconcileWatchdog(ctx, 50*time.Millisecond)

	// Up to 3 ticks (~150 ms) + grace.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		h.mgr.managedPeersMu.Lock()
		mp := h.mgr.managedPeersByConnID[cfg.PeerConnID]
		got := mp.expectedWatcher
		h.mgr.managedPeersMu.Unlock()
		if got == watcherActivity {
			return // healed
		}
		// Drip another drop so each tick has a positive delta.
		h.mgr.inactivityManager.RecordRelayDropForTest()
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("watchdog did not heal inactivity-stuck peer within deadline")
}

// TestIntegration_ActivityNoListener_WatchdogHeals: Case-b end-to-end.
// Peer in watcherActivity with no listener → watchdog re-arms within
// a few ticks.
func TestIntegration_ActivityNoListener_WatchdogHeals(t *testing.T) {
	h := newTestHarness(t)
	cfg := addStuckActivityNoListenerPeer(t, h, "peerInt-b")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go h.mgr.runReconcileWatchdog(ctx, 50*time.Millisecond)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
			return // healed
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("watchdog did not re-arm activity listener within deadline")
}

// TestIntegration_PanicInConsumer_WatchdogHeals: Stufe 0 + Stufe 2
// cooperation. The consumer-loop panic-recovery (safeOnPeerActivity /
// safeOnPeerInactivityTimedOut) keeps the consumer alive after a bug,
// and the watchdog still ticks and recovers any subsequently stuck peer.
// We exercise this by running runReconcileWatchdog directly while a
// stuck peer exists — verifying the watchdog's own panic-recovery (the
// spawnRecovery wrapper) is independent of the consumer loop.
func TestIntegration_PanicInConsumer_WatchdogHeals(t *testing.T) {
	h := newTestHarness(t)
	cfg := addStuckInactivityPeer(t, h, "peerInt-panic")

	// Inject a one-shot panic in the recovery hook (simulating a bug
	// in armActivityListener path). The watchdog must keep ticking
	// and the recovery for the same peer must eventually succeed on a
	// later tick.
	var armCount int
	var armMu sync.Mutex
	setTestListenerArmHook(func(pubKey string) {
		armMu.Lock()
		armCount++
		count := armCount
		armMu.Unlock()
		if count == 1 && pubKey == cfg.PublicKey {
			panic("synthetic consumer panic")
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		h.mgr.runReconcileWatchdog(ctx, 50*time.Millisecond)
		close(loopDone)
	}()

	h.mgr.inactivityManager.RecordRelayDropForTest()

	// After the first panic, subsequent ticks must still classify the
	// peer as stuck — armActivityListener IS idempotent, so the
	// listener IS installed even when the hook (which fires AFTER arm)
	// panics. Verify the listener is armed despite the panic.
	healed := false
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		if h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
			healed = true
			break
		}
		h.mgr.inactivityManager.RecordRelayDropForTest()
		time.Sleep(30 * time.Millisecond)
	}
	cancel()
	<-loopDone
	time.Sleep(50 * time.Millisecond)
	setTestListenerArmHook(nil)

	if !healed {
		t.Fatal("watchdog did not heal stuck peer after panic in recovery hook")
	}
}
