package internal

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/netbirdio/netbird/client/iface/wgaddr"
	"github.com/netbirdio/netbird/client/internal/lazyconn/manager"
	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/client/internal/peer/dispatcher"
	"github.com/netbirdio/netbird/client/internal/peer/guard"
	"github.com/netbirdio/netbird/client/internal/peer/ice"
	"github.com/netbirdio/netbird/client/internal/peerstore"
	"github.com/netbirdio/netbird/monotime"
)

// stubWGIfaceForDeactivateTest is a no-op WGIface that satisfies
// lazyconn.WGIface for tests where we need to instantiate a real
// *manager.Manager but never actually use it.
type stubWGIfaceForDeactivateTest struct{}

func (stubWGIfaceForDeactivateTest) RemovePeer(string) error { return nil }
func (stubWGIfaceForDeactivateTest) UpdatePeer(string, []netip.Prefix, time.Duration, *net.UDPAddr, *wgtypes.Key) error {
	return nil
}
func (stubWGIfaceForDeactivateTest) IsUserspaceBind() bool   { return true }
func (stubWGIfaceForDeactivateTest) Address() wgaddr.Address { return wgaddr.Address{} }
func (stubWGIfaceForDeactivateTest) LastActivities() map[string]monotime.Time {
	return map[string]monotime.Time{}
}

// newConnMgrWithLazyMgr returns a ConnMgr whose isStartedWithLazyMgr()
// reports true. The underlying lazy manager has empty peer maps; any
// DeactivatePeer call for an unknown peerID is a silent no-op there.
func newConnMgrWithLazyMgr(t *testing.T) *ConnMgr {
	t.Helper()
	store := peerstore.NewConnStore()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	lazyMgr := manager.NewManager(manager.Config{}, ctx, store, stubWGIfaceForDeactivateTest{})

	return &ConnMgr{
		peerStore:     store,
		lazyConnMgr:   lazyMgr,
		lazyCtxCancel: cancel,
	}
}

// newTestConnWithRemoteMode builds a minimal *peer.Conn whose
// RemoteEffectiveMode() returns the requested mode. If remoteMode is
// the empty string, the peer is registered without
// EffectiveConnectionMode so the accessor returns ModeUnspecified
// (NetworkMap-bootstrap-race shape).
func newTestConnWithRemoteMode(t *testing.T, peerKey, remoteMode string) *peer.Conn {
	t.Helper()

	recorder := peer.NewRecorder("https://mgm")
	swWatcher := guard.NewSRWatcher(nil, nil, nil, ice.Config{})

	cfg := peer.ConnConfig{
		Key:      peerKey,
		LocalKey: "RRHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
		WgConfig: peer.WgConfig{
			RemoteKey:  peerKey,
			AllowedIps: []netip.Prefix{netip.MustParsePrefix("100.64.0.5/32")},
		},
	}
	sd := peer.ServiceDependencies{
		StatusRecorder:     recorder,
		SrWatcher:          swWatcher,
		PeerConnDispatcher: dispatcher.NewConnectionDispatcher(),
	}

	conn, err := peer.NewConn(cfg, sd)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}

	if err := recorder.AddPeer(peerKey, "", ""); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	if remoteMode != "" {
		if err := recorder.UpdatePeerRemoteMeta(peerKey, peer.RemoteMeta{
			EffectiveConnectionMode: remoteMode,
		}); err != nil {
			t.Fatalf("UpdatePeerRemoteMeta: %v", err)
		}
	}

	return conn
}

// TestConnMgr_deactivatePeerActionFor verifies the per-remote-mode
// dispatch contract (Phase 3.7j Fix A, spec §3.1).
//
//   - remote p2p-lazy        -> deactivateLazy
//   - remote p2p-dynamic     -> deactivateICE
//   - remote unspecified + LazyMgr   -> deactivateLazy (safer
//     full-close during bootstrap race)
//   - remote unspecified + no LazyMgr -> deactivateNoop (eager mode,
//     with diagnostic log)
//   - remote eager (p2p, relay-forced) -> deactivateNoop
func TestConnMgr_deactivatePeerActionFor(t *testing.T) {
	cases := []struct {
		name        string
		remoteMode  string
		withLazyMgr bool
		want        deactivateAction
	}{
		{"remote p2p-lazy, no LazyMgr", "p2p-lazy", false, deactivateLazy},
		{"remote p2p-lazy, with LazyMgr", "p2p-lazy", true, deactivateLazy},
		{"remote p2p-dynamic", "p2p-dynamic", false, deactivateICE},
		{"remote p2p (eager)", "p2p", false, deactivateNoop},
		{"remote relay-forced (eager)", "relay-forced", false, deactivateNoop},
		{"remote unspecified + LazyMgr -> lazy fallback", "", true, deactivateLazy},
		{"remote unspecified + no LazyMgr -> noop", "", false, deactivateNoop},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mgr *ConnMgr
			if c.withLazyMgr {
				mgr = newConnMgrWithLazyMgr(t)
			} else {
				mgr = &ConnMgr{peerStore: peerstore.NewConnStore()}
			}

			conn := newTestConnWithRemoteMode(t, "peer-"+c.name, c.remoteMode)

			got := mgr.deactivatePeerActionFor(conn)
			if got != c.want {
				t.Errorf("deactivatePeerActionFor(remote=%q, lazyMgr=%v) = %v, want %v",
					c.remoteMode, c.withLazyMgr, got, c.want)
			}
		})
	}
}

// TestDeactivatePeer_RemoteLazy_ClosesFully: LazyMgr active + remote
// p2p-lazy -> lazy-full-close path. Asserts DetachICEForPeer was NOT
// run (intentionallyDetached marker stays false) and the dispatched
// action is deactivateLazy (so the lazyConnMgr.DeactivatePeer branch
// is taken).
func TestDeactivatePeer_RemoteLazy_ClosesFully(t *testing.T) {
	mgr := newConnMgrWithLazyMgr(t)
	conn := newTestConnWithRemoteMode(t, "peer-remote-lazy", "p2p-lazy")
	mgr.peerStore.AddPeerConn(conn.GetKey(), conn)

	if got := mgr.deactivatePeerActionFor(conn); got != deactivateLazy {
		t.Fatalf("action = %v, want deactivateLazy", got)
	}

	mgr.DeactivatePeer(conn)

	// The lazy branch closes the peer via the lazy manager. The peer
	// is unknown to the dummy lazy mgr so the call is a no-op there;
	// the key assertion is that the ICE-detach branch did NOT run.
	if conn.IsIntentionallyDetached() {
		t.Fatalf("IsIntentionallyDetached() = true after lazy-full-close dispatch; want false (DetachICEForPeer should not have run)")
	}
}

// TestDeactivatePeer_RemoteLazy_NoLazyMgr_FallsBackToDetach: remote
// peer wants full close but local mgr is eager (no LazyMgr).
// DeactivatePeer must fall through to ICE detach rather than silently
// no-op (spec §3.1 fallback).
func TestDeactivatePeer_RemoteLazy_NoLazyMgr_FallsBackToDetach(t *testing.T) {
	mgr := &ConnMgr{peerStore: peerstore.NewConnStore()}
	conn := newTestConnWithRemoteMode(t, "peer-remote-lazy-no-mgr", "p2p-lazy")
	mgr.peerStore.AddPeerConn(conn.GetKey(), conn)

	if mgr.isStartedWithLazyMgr() {
		t.Fatalf("isStartedWithLazyMgr() = true, want false (test setup mistake)")
	}

	mgr.DeactivatePeer(conn)

	if !conn.IsIntentionallyDetached() {
		t.Fatalf("IsIntentionallyDetached() = false after no-LazyMgr fallback; expected DetachICEForPeer to have run")
	}
}

// TestDeactivatePeer_RemoteDynamic_DetachesICEOnly: existing behaviour
// preserved -- remote p2p-dynamic GO_IDLE -> ICE detach.
func TestDeactivatePeer_RemoteDynamic_DetachesICEOnly(t *testing.T) {
	mgr := &ConnMgr{peerStore: peerstore.NewConnStore()}
	conn := newTestConnWithRemoteMode(t, "peer-remote-dynamic", "p2p-dynamic")
	mgr.peerStore.AddPeerConn(conn.GetKey(), conn)

	mgr.DeactivatePeer(conn)

	if !conn.IsIntentionallyDetached() {
		t.Fatalf("IsIntentionallyDetached() = false after remote-dynamic GO_IDLE; expected DetachICEForPeer to have run")
	}
}

// TestDeactivatePeer_RemoteUnspecified_LazyMgrFallsBackToLazy:
// bootstrap-race window. RemoteEffectiveMode unknown + LazyMgr active
// -> safer lazy-full-close path, NOT DetachICEForPeer.
func TestDeactivatePeer_RemoteUnspecified_LazyMgrFallsBackToLazy(t *testing.T) {
	mgr := newConnMgrWithLazyMgr(t)
	conn := newTestConnWithRemoteMode(t, "peer-remote-unspec-lazymgr", "")
	mgr.peerStore.AddPeerConn(conn.GetKey(), conn)

	if got := mgr.deactivatePeerActionFor(conn); got != deactivateLazy {
		t.Fatalf("action = %v, want deactivateLazy (bootstrap race + LazyMgr)", got)
	}

	mgr.DeactivatePeer(conn)

	if conn.IsIntentionallyDetached() {
		t.Fatalf("IsIntentionallyDetached() = true after lazy-fallback dispatch; want false (DetachICEForPeer should not have run)")
	}
}

// TestDeactivatePeer_RemoteUnspecified_NoLazyMgrNoops: bootstrap-race
// window without a LazyMgr -> diagnostic noop. Neither DetachICEForPeer
// nor lazyConnMgr.DeactivatePeer is called.
func TestDeactivatePeer_RemoteUnspecified_NoLazyMgrNoops(t *testing.T) {
	mgr := &ConnMgr{peerStore: peerstore.NewConnStore()}
	conn := newTestConnWithRemoteMode(t, "peer-remote-unspec-eager", "")
	mgr.peerStore.AddPeerConn(conn.GetKey(), conn)

	if got := mgr.deactivatePeerActionFor(conn); got != deactivateNoop {
		t.Fatalf("action = %v, want deactivateNoop (bootstrap race + no LazyMgr)", got)
	}

	mgr.DeactivatePeer(conn)

	if conn.IsIntentionallyDetached() {
		t.Fatalf("IsIntentionallyDetached() = true after noop dispatch; want false (no detach should have run)")
	}
}
