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
