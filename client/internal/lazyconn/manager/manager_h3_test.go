package manager

import (
	"net/netip"
	"testing"

	"github.com/netbirdio/netbird/client/internal/lazyconn/activity"
)

// TestManager_RemovePeer_ClearsRoutePrefixes ensures the per-peer entry
// in peerToRoutePrefixes is removed when a peer is removed. Without the
// H3 fix, the entry persists until the next UpdateRouteHAMap rebuild,
// producing a bounded but real map-leak across peer-churn cycles.
func TestManager_RemovePeer_ClearsRoutePrefixes(t *testing.T) {
	h := newTestHarness(t)
	cfg := newTestPeerCfg("peer-h3-remove")

	h.mgr.managedPeersMu.Lock()
	h.mgr.managedPeers[cfg.PublicKey] = &cfg
	h.mgr.managedPeersByConnID[cfg.PeerConnID] = &managedPeer{
		peerCfg:         &cfg,
		expectedWatcher: watcherInactivity,
	}
	h.mgr.managedPeersMu.Unlock()

	h.mgr.routesMu.Lock()
	h.mgr.peerToRoutePrefixes[cfg.PublicKey] = []netip.Prefix{
		netip.MustParsePrefix("10.20.30.0/24"),
	}
	h.mgr.routesMu.Unlock()

	h.mgr.RemovePeer(cfg.PublicKey)

	h.mgr.routesMu.RLock()
	_, exists := h.mgr.peerToRoutePrefixes[cfg.PublicKey]
	h.mgr.routesMu.RUnlock()

	if exists {
		t.Fatalf("peerToRoutePrefixes still contains %q after RemovePeer", cfg.PublicKey)
	}
}

// TestManager_Close_ClearsRoutePrefixes drives the close() path and
// asserts the peerToRoutePrefixes map is reset.
//
// activity.Manager.Close() is NOT idempotent — close(m.done) panics
// on a second call. newTestHarness.t.Cleanup also calls
// mgr.activityManager.Close(), so this test re-assigns
// h.mgr.activityManager to a fresh instance after invoking close()
// so the harness Cleanup runs against a still-open manager.
func TestManager_Close_ClearsRoutePrefixes(t *testing.T) {
	h := newTestHarness(t)

	h.mgr.routesMu.Lock()
	h.mgr.peerToRoutePrefixes["a"] = []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")}
	h.mgr.peerToRoutePrefixes["b"] = []netip.Prefix{netip.MustParsePrefix("10.2.0.0/16")}
	h.mgr.routesMu.Unlock()

	h.mgr.close()

	// Re-init activityManager so the harness Cleanup's
	// mgr.activityManager.Close() does not double-close the same
	// channel (activity/manager.go:171 `close(m.done)` would panic).
	h.mgr.activityManager = activity.NewManager(h.wgIface)

	h.mgr.routesMu.RLock()
	got := len(h.mgr.peerToRoutePrefixes)
	h.mgr.routesMu.RUnlock()

	if got != 0 {
		t.Fatalf("peerToRoutePrefixes len=%d after close(); want 0", got)
	}
}
