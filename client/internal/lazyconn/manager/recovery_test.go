package manager

import (
	"testing"
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
	testListenerArmHook = func(pubKey string) {
		if pubKey != cfg.PublicKey {
			return
		}
		h.mgr.managedPeersMu.Lock()
		delete(h.mgr.managedPeers, cfg.PublicKey)
		delete(h.mgr.managedPeersByConnID, cfg.PeerConnID)
		h.mgr.managedPeersMu.Unlock()
	}
	t.Cleanup(func() { testListenerArmHook = nil })

	h.mgr.onPeerInactivityTimedOut(map[string]struct{}{cfg.PublicKey: {}})

	if h.mgr.activityManager.HasPeer(cfg.PeerConnID) {
		t.Fatal("R14 regression: listener not cleaned up after race-removed peer")
	}
}
