package manager

import (
	"context"
	"testing"

	"github.com/netbirdio/netbird/client/internal/lazyconn"
	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/client/internal/peer/worker"
	"github.com/netbirdio/netbird/route"
)

// TestTransitionToActivityWatcherStateOnly_HappyPath: verify state-only
// helper flips expectedWatcher and removes the peer from inactivity
// manager, without any I/O or close.
func TestTransitionToActivityWatcherStateOnly_HappyPath(t *testing.T) {
	h := newTestHarness(t)
	cfg := newTestPeerCfg("peer1")

	h.mgr.managedPeersMu.Lock()
	h.mgr.managedPeers[cfg.PublicKey] = &cfg
	mp := &managedPeer{peerCfg: &cfg, expectedWatcher: watcherInactivity}
	h.mgr.managedPeersByConnID[cfg.PeerConnID] = mp
	h.mgr.transitionToActivityWatcherStateOnly(mp)
	h.mgr.managedPeersMu.Unlock()

	if mp.expectedWatcher != watcherActivity {
		t.Fatalf("expected watcherActivity, got %v", mp.expectedWatcher)
	}
}

// TestPeerStillManaged_Present: peer exists with matching connID → true.
func TestPeerStillManaged_Present(t *testing.T) {
	h := newTestHarness(t)
	cfg := newTestPeerCfg("peer1")
	h.mgr.managedPeersMu.Lock()
	h.mgr.managedPeers[cfg.PublicKey] = &cfg
	h.mgr.managedPeersByConnID[cfg.PeerConnID] = &managedPeer{peerCfg: &cfg, expectedWatcher: watcherInactivity}
	h.mgr.managedPeersMu.Unlock()

	if !h.mgr.peerStillManaged(cfg.PublicKey, cfg.PeerConnID) {
		t.Fatal("expected true")
	}
}

// TestPeerStillManaged_Removed: peer no longer in managedPeers → false.
func TestPeerStillManaged_Removed(t *testing.T) {
	h := newTestHarness(t)
	cfg := newTestPeerCfg("peer1")
	if h.mgr.peerStillManaged(cfg.PublicKey, cfg.PeerConnID) {
		t.Fatal("expected false on empty Manager")
	}
}

// TestPeerStillManaged_ConnIDChanged: peer re-added with different
// ConnID between snapshot and re-validate → false.
func TestPeerStillManaged_ConnIDChanged(t *testing.T) {
	h := newTestHarness(t)
	cfgOld := newTestPeerCfg("peer1")
	h.mgr.managedPeersMu.Lock()
	h.mgr.managedPeers[cfgOld.PublicKey] = &cfgOld
	h.mgr.managedPeersByConnID[cfgOld.PeerConnID] = &managedPeer{peerCfg: &cfgOld, expectedWatcher: watcherInactivity}
	h.mgr.managedPeersMu.Unlock()

	// Now replace with a different cfg (different stub instance → different ConnID)
	cfgNew := newTestPeerCfg("peer1")
	h.mgr.managedPeersMu.Lock()
	delete(h.mgr.managedPeersByConnID, cfgOld.PeerConnID)
	h.mgr.managedPeers[cfgNew.PublicKey] = &cfgNew
	h.mgr.managedPeersByConnID[cfgNew.PeerConnID] = &managedPeer{peerCfg: &cfgNew, expectedWatcher: watcherInactivity}
	h.mgr.managedPeersMu.Unlock()

	if h.mgr.peerStillManaged(cfgOld.PublicKey, cfgOld.PeerConnID) {
		t.Fatal("expected false: ConnID changed since snapshot")
	}
}

// TestOnPeerInactivityTimedOut_RemoveRaceAfterUnlock (v0.7 R14): when
// RemovePeer races between armActivityListener and peerStillManaged,
// the orphan listener must be cleaned up via activityManager.RemovePeer.
// This test verifies the R14 fix shipped in Task 3.
func TestOnPeerInactivityTimedOut_RemoveRaceAfterUnlock(t *testing.T) {
	h := newTestHarness(t)
	cfg := newTestPeerCfg("peerR14")

	// Wire up peer in watcherInactivity
	h.mgr.managedPeersMu.Lock()
	h.mgr.managedPeers[cfg.PublicKey] = &cfg
	h.mgr.managedPeersByConnID[cfg.PeerConnID] = &managedPeer{
		peerCfg:         &cfg,
		expectedWatcher: watcherInactivity,
	}
	h.mgr.managedPeersMu.Unlock()

	// Install hook: between arm and re-validate, simulate concurrent removal.
	setTestListenerArmHook(func(pubKey string) {
		if pubKey != cfg.PublicKey {
			return
		}
		h.mgr.managedPeersMu.Lock()
		delete(h.mgr.managedPeers, cfg.PublicKey)
		delete(h.mgr.managedPeersByConnID, cfg.PeerConnID)
		h.mgr.managedPeersMu.Unlock()
	})
	t.Cleanup(func() { setTestListenerArmHook(nil) })

	h.mgr.onPeerInactivityTimedOut(map[string]struct{}{cfg.PublicKey: {}})

	if h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
		t.Fatal("R14 regression: listener not cleaned up after race-removed peer")
	}
}

// ---------------------------------------------------------------------
// Step 6.12: recovery-function tests for the lazy-watchdog
// ---------------------------------------------------------------------

// wireInactivityPeer is a small helper that installs a peer in
// watcherInactivity state with both transports Disconnected.
func wireInactivityPeer(t *testing.T, h *testHarness, pubKey string) (cfg lazyconn.PeerConfig) {
	t.Helper()
	cfg = newTestPeerCfg(pubKey)
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

// wireActivityPeer installs a peer in watcherActivity state without
// arming a listener (Case-b stuck state).
func wireActivityPeer(t *testing.T, h *testHarness, pubKey string) (cfg lazyconn.PeerConfig) {
	t.Helper()
	cfg = newTestPeerCfg(pubKey)
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
	return cfg
}

func TestRecoverInactivityStuck_HappyPath(t *testing.T) {
	h := newTestHarness(t)
	cfg := wireInactivityPeer(t, h, "peerR-a")
	connBefore, _ := h.peerStore.PeerConn(cfg.PublicKey)

	h.mgr.recoverInactivityStuck(h.ctx, cfg.PublicKey, map[string]struct{}{cfg.PublicKey: {}})

	h.mgr.managedPeersMu.Lock()
	mp := h.mgr.managedPeersByConnID[cfg.PeerConnID]
	got := mp.expectedWatcher
	h.mgr.managedPeersMu.Unlock()
	if got != watcherActivity {
		t.Fatalf("expected watcherActivity, got %v", got)
	}
	if !h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
		t.Fatal("expected listener armed after recovery")
	}
	// Conn instance must remain the same — proves no Conn-replacement.
	connAfter, _ := h.peerStore.PeerConn(cfg.PublicKey)
	if connBefore != connAfter {
		t.Fatal("peerStore.PeerConn instance changed — should remain identical")
	}
}

func TestRecoverInactivityStuck_AlreadyActivity_NoOp(t *testing.T) {
	h := newTestHarness(t)
	cfg := wireActivityPeer(t, h, "peerR-already")

	h.mgr.recoverInactivityStuck(h.ctx, cfg.PublicKey, map[string]struct{}{cfg.PublicKey: {}})

	// Listener must NOT have been armed; state must remain watcherActivity.
	h.mgr.managedPeersMu.Lock()
	mp := h.mgr.managedPeersByConnID[cfg.PeerConnID]
	got := mp.expectedWatcher
	h.mgr.managedPeersMu.Unlock()
	if got != watcherActivity {
		t.Fatalf("expected watcherActivity unchanged, got %v", got)
	}
	if h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
		t.Fatal("recoverInactivityStuck must NOT arm listener when peer already watcherActivity")
	}
}

// TestRecoverInactivityStuck_RespectsHA_FullBatch: when a peer's HA-group
// contains an active sibling, recoverInactivityStuck must defer (NOT
// flip state). We install two peers in the same HA group: peerA is
// stuck (watcherInactivity), peerB is active (watcherInactivity but
// NOT in the inactive batch → counts as "still active" by
// shouldDeferIdleForHA semantics).
func TestRecoverInactivityStuck_RespectsHA_FullBatch(t *testing.T) {
	h := newTestHarness(t)
	cfgStuck := wireInactivityPeer(t, h, "peerHA-stuck")
	cfgActive := wireInactivityPeer(t, h, "peerHA-active")

	// Wire both peers into the same HA group.
	const haID route.HAUniqueID = "hagrp-1"
	h.mgr.routesMu.Lock()
	h.mgr.haGroupToPeers[haID] = []string{cfgStuck.PublicKey, cfgActive.PublicKey}
	h.mgr.peerToHAGroups[cfgStuck.PublicKey] = []route.HAUniqueID{haID}
	h.mgr.peerToHAGroups[cfgActive.PublicKey] = []route.HAUniqueID{haID}
	h.mgr.routesMu.Unlock()

	// Batch contains only the stuck peer → sibling is considered active.
	h.mgr.recoverInactivityStuck(h.ctx, cfgStuck.PublicKey, map[string]struct{}{cfgStuck.PublicKey: {}})

	h.mgr.managedPeersMu.Lock()
	mp := h.mgr.managedPeersByConnID[cfgStuck.PeerConnID]
	got := mp.expectedWatcher
	h.mgr.managedPeersMu.Unlock()
	if got != watcherInactivity {
		t.Fatalf("HA-defer failed: expected watcherInactivity (defer), got %v", got)
	}
	if h.mgr.activityManager.HasPeer(cfgStuck.PeerConnID) {
		t.Fatal("HA-defer: must NOT arm listener when HA sibling is active")
	}
}

func TestRecoverActivityNoListener_HappyPath(t *testing.T) {
	h := newTestHarness(t)
	cfg := wireActivityPeer(t, h, "peerR-b")

	h.mgr.recoverActivityNoListener(h.ctx, cfg.PublicKey)

	if !h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
		t.Fatal("expected listener armed by recoverActivityNoListener")
	}
}

// TestRecoverActivityNoListener_ListenerArmedConcurrently_NoOp: if the
// listener is already armed before the call, recoverActivityNoListener
// must short-circuit and NOT call armActivityListener again.
func TestRecoverActivityNoListener_ListenerArmedConcurrently_NoOp(t *testing.T) {
	h := newTestHarness(t)
	cfg := wireActivityPeer(t, h, "peerR-bnoop")
	if err := h.mgr.activityManager.MonitorPeerActivity(cfg); err != nil {
		t.Fatalf("MonitorPeerActivity: %v", err)
	}

	// Track arm attempts via the hook (must NOT be invoked).
	var hits int
	setTestListenerArmHook(func(pubKey string) {
		if pubKey == cfg.PublicKey {
			hits++
		}
	})
	t.Cleanup(func() { setTestListenerArmHook(nil) })

	h.mgr.recoverActivityNoListener(h.ctx, cfg.PublicKey)

	if hits != 0 {
		t.Fatalf("expected hook NOT to fire when listener already armed, got %d hits", hits)
	}
	if !h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
		t.Fatal("listener disappeared unexpectedly")
	}
}

// TestRecoverActivityNoListener_SkipsHADefer: HA-defer applies to
// inactivity-stuck recovery only (peer "wanted inactive"). Case-b is
// "peer already wanted active" — no HA-defer. We install two peers in
// the same HA group; the watchdog must still re-arm the listener for
// the activity-stuck peer.
func TestRecoverActivityNoListener_SkipsHADefer(t *testing.T) {
	h := newTestHarness(t)
	cfgStuck := wireActivityPeer(t, h, "peerHA-b-stuck")
	cfgOther := wireInactivityPeer(t, h, "peerHA-b-other")

	const haID route.HAUniqueID = "hagrp-2"
	h.mgr.routesMu.Lock()
	h.mgr.haGroupToPeers[haID] = []string{cfgStuck.PublicKey, cfgOther.PublicKey}
	h.mgr.peerToHAGroups[cfgStuck.PublicKey] = []route.HAUniqueID{haID}
	h.mgr.peerToHAGroups[cfgOther.PublicKey] = []route.HAUniqueID{haID}
	h.mgr.routesMu.Unlock()

	h.mgr.recoverActivityNoListener(h.ctx, cfgStuck.PublicKey)

	if !h.mgr.activityManager.HasPeer(cfgStuck.PeerConnID) {
		t.Fatal("Case-b recovery must NOT honor HA-defer; listener must be armed")
	}
}

// TestRecoverInactivityStuck_RemoveRaceAfterUnlock (v0.7 R14): if
// RemovePeer races between armActivityListener and the post-arm
// peerStillManaged check, the orphan listener must be cleaned up via
// activityManager.RemovePeer.
func TestRecoverInactivityStuck_RemoveRaceAfterUnlock(t *testing.T) {
	h := newTestHarness(t)
	cfg := wireInactivityPeer(t, h, "peerR14-a")

	setTestListenerArmHook(func(pubKey string) {
		if pubKey != cfg.PublicKey {
			return
		}
		h.mgr.managedPeersMu.Lock()
		delete(h.mgr.managedPeers, cfg.PublicKey)
		delete(h.mgr.managedPeersByConnID, cfg.PeerConnID)
		h.mgr.managedPeersMu.Unlock()
	})
	t.Cleanup(func() { setTestListenerArmHook(nil) })

	h.mgr.recoverInactivityStuck(h.ctx, cfg.PublicKey, map[string]struct{}{cfg.PublicKey: {}})

	if h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
		t.Fatal("R14 regression (Case-a): orphan listener not cleaned up after race-removed peer")
	}
}

// TestRecoverActivityNoListener_RemoveRaceAfterUnlock (v0.7 R14):
// same race window in the Case-b path.
func TestRecoverActivityNoListener_RemoveRaceAfterUnlock(t *testing.T) {
	h := newTestHarness(t)
	cfg := wireActivityPeer(t, h, "peerR14-b")

	setTestListenerArmHook(func(pubKey string) {
		if pubKey != cfg.PublicKey {
			return
		}
		h.mgr.managedPeersMu.Lock()
		delete(h.mgr.managedPeers, cfg.PublicKey)
		delete(h.mgr.managedPeersByConnID, cfg.PeerConnID)
		h.mgr.managedPeersMu.Unlock()
	})
	t.Cleanup(func() { setTestListenerArmHook(nil) })

	h.mgr.recoverActivityNoListener(h.ctx, cfg.PublicKey)

	if h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
		t.Fatal("R14 regression (Case-b): orphan listener not cleaned up after race-removed peer")
	}
}

// TestRecoverActivityNoListener_ConnIDChangeAfterUnlock (v0.7 R14):
// peer is re-added with a fresh ConnID between Unlock and the post-arm
// Re-Validate. peerStillManaged must report false and the orphan
// listener for the OLD ConnID must be removed.
func TestRecoverActivityNoListener_ConnIDChangeAfterUnlock(t *testing.T) {
	h := newTestHarness(t)
	cfgOld := wireActivityPeer(t, h, "peerR14-c")

	setTestListenerArmHook(func(pubKey string) {
		if pubKey != cfgOld.PublicKey {
			return
		}
		// Simulate the peer being removed and re-added with a fresh
		// ConnID between Unlock and the post-arm Re-Validate.
		h.mgr.managedPeersMu.Lock()
		delete(h.mgr.managedPeers, cfgOld.PublicKey)
		delete(h.mgr.managedPeersByConnID, cfgOld.PeerConnID)
		cfgNew := newTestPeerCfg("peerR14-c")
		h.mgr.managedPeers[cfgNew.PublicKey] = &cfgNew
		h.mgr.managedPeersByConnID[cfgNew.PeerConnID] = &managedPeer{
			peerCfg:         &cfgNew,
			expectedWatcher: watcherActivity,
		}
		h.mgr.managedPeersMu.Unlock()
	})
	t.Cleanup(func() { setTestListenerArmHook(nil) })

	h.mgr.recoverActivityNoListener(h.ctx, cfgOld.PublicKey)

	if h.mgr.activityManager.HasPeer(cfgOld.PeerConnID) {
		t.Fatal("R14 regression (ConnID-change): orphan listener for OLD ConnID not cleaned up")
	}
}

// Silence unused-import warnings if a future refactor drops "context".
var _ = context.Background
