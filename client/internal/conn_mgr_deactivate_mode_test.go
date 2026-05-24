package internal

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/netbirdio/netbird/client/iface/wgaddr"
	"github.com/netbirdio/netbird/client/internal/lazyconn"
	"github.com/netbirdio/netbird/client/internal/lazyconn/manager"
	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/client/internal/peer/dispatcher"
	"github.com/netbirdio/netbird/client/internal/peer/guard"
	"github.com/netbirdio/netbird/client/internal/peer/ice"
	"github.com/netbirdio/netbird/client/internal/peerstore"
	"github.com/netbirdio/netbird/monotime"
)

// stubWGIfaceForDeactivateTest is a configurable WGIface that satisfies
// lazyconn.WGIface for tests where we need to instantiate a real
// *manager.Manager and/or exercise the local-activity-gate (Fix C).
//
// Tests must set Userspace explicitly: bool zero value is false, so
// an uninitialized stub reports kernel mode. Dispatch tests that need
// the userspace path set Userspace=true at construction; Fix-C tests
// configure both fields explicitly.
type stubWGIfaceForDeactivateTest struct {
	Userspace  bool
	Activities map[string]monotime.Time
}

func (stubWGIfaceForDeactivateTest) RemovePeer(string) error { return nil }
func (stubWGIfaceForDeactivateTest) UpdatePeer(string, []netip.Prefix, time.Duration, *net.UDPAddr, *wgtypes.Key) error {
	return nil
}
func (s stubWGIfaceForDeactivateTest) IsUserspaceBind() bool { return s.Userspace }
func (stubWGIfaceForDeactivateTest) Address() wgaddr.Address { return wgaddr.Address{} }
func (s stubWGIfaceForDeactivateTest) LastActivities() map[string]monotime.Time {
	if s.Activities == nil {
		return map[string]monotime.Time{}
	}
	return s.Activities
}

// newConnMgrWithLazyMgr returns a ConnMgr whose isStartedWithLazyMgr()
// reports true. The underlying lazy manager has empty peer maps; any
// DeactivatePeer call for an unknown peerID is a silent no-op there.
func newConnMgrWithLazyMgr(t *testing.T) *ConnMgr {
	t.Helper()
	store := peerstore.NewConnStore()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	lazyMgr := manager.NewManager(manager.Config{}, ctx, store, stubWGIfaceForDeactivateTest{Userspace: true})

	return &ConnMgr{
		peerStore:     store,
		iface:         stubWGIfaceForDeactivateTest{Userspace: true},
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

// --- Phase-3.7j Fix C: Local-Activity-Gate tests ---

// newConnMgrForGateTest returns a ConnMgr wired with the given WGIface
// stub and p2pTimeoutSecs. No LazyMgr is started; the deactivateICE
// branch does not consult it.
func newConnMgrForGateTest(iface lazyconn.WGIface, p2pTimeoutSecs uint32) *ConnMgr {
	return &ConnMgr{
		peerStore:      peerstore.NewConnStore(),
		iface:          iface,
		p2pTimeoutSecs: p2pTimeoutSecs,
	}
}

// TestDeactivatePeer_LocalActivityGate_SkipsRecent: userspace bind +
// recent local activity (within gate window) -> detach is skipped.
// IsIntentionallyDetached() must remain false because DetachICEForPeer
// was not invoked. Verifies the headline guarantee of Fix C: the
// remote's stale-idle view does not tear down a tunnel we're still
// actively using.
func TestDeactivatePeer_LocalActivityGate_SkipsRecent(t *testing.T) {
	peerKey := "peer-gate-recent"
	iface := stubWGIfaceForDeactivateTest{
		Userspace:  true,
		Activities: map[string]monotime.Time{peerKey: monotime.Now()},
	}
	mgr := newConnMgrForGateTest(iface, 180) // gate = 90s
	conn := newTestConnWithRemoteMode(t, peerKey, "p2p-dynamic")
	mgr.peerStore.AddPeerConn(conn.GetKey(), conn)

	mgr.DeactivatePeer(conn)

	if conn.IsIntentionallyDetached() {
		t.Fatalf("IsIntentionallyDetached() = true; gate should have skipped detach for recent local activity")
	}
}

// TestDeactivatePeer_LocalActivityGate_ProceedsIfStale: userspace bind
// + last activity older than the gate window -> detach proceeds and
// IsIntentionallyDetached() flips to true. The "stale" timestamp is
// computed as gate+1m to be robust against scheduling jitter.
func TestDeactivatePeer_LocalActivityGate_ProceedsIfStale(t *testing.T) {
	peerKey := "peer-gate-stale"
	// p2pTimeoutSecs=180 -> gate=90s. Backdate by 3 minutes.
	stale := monotime.Time(int64(monotime.Now()) - int64(3*time.Minute))
	iface := stubWGIfaceForDeactivateTest{
		Userspace:  true,
		Activities: map[string]monotime.Time{peerKey: stale},
	}
	mgr := newConnMgrForGateTest(iface, 180)
	conn := newTestConnWithRemoteMode(t, peerKey, "p2p-dynamic")
	mgr.peerStore.AddPeerConn(conn.GetKey(), conn)

	mgr.DeactivatePeer(conn)

	if !conn.IsIntentionallyDetached() {
		t.Fatalf("IsIntentionallyDetached() = false; detach should have proceeded for stale local activity")
	}
}

// TestDeactivatePeer_LocalActivityGate_KernelMode_NoGate: kernel mode
// (IsUserspaceBind == false) -> gate is skipped entirely (the kernel
// configurer's LastActivities() returns nil so the gate has no signal
// to act on). Even with a "recent" timestamp in the test stub, the
// detach must proceed. Validates the caveat documented at the gate.
func TestDeactivatePeer_LocalActivityGate_KernelMode_NoGate(t *testing.T) {
	peerKey := "peer-gate-kernel"
	iface := stubWGIfaceForDeactivateTest{
		Userspace:  false, // kernel mode
		Activities: map[string]monotime.Time{peerKey: monotime.Now()},
	}
	mgr := newConnMgrForGateTest(iface, 180)
	conn := newTestConnWithRemoteMode(t, peerKey, "p2p-dynamic")
	mgr.peerStore.AddPeerConn(conn.GetKey(), conn)

	mgr.DeactivatePeer(conn)

	if !conn.IsIntentionallyDetached() {
		t.Fatalf("IsIntentionallyDetached() = false in kernel mode; gate should not fire and detach should proceed")
	}
}

// TestDeactivatePeer_LocalActivityGate_NoActivityRecord: userspace
// bind, but the ActivityRecorder has no entry for this peer (e.g. the
// dynamic tunnel is up via relay only and no payload has crossed it
// since the bind was reset). Detach proceeds.
func TestDeactivatePeer_LocalActivityGate_NoActivityRecord(t *testing.T) {
	peerKey := "peer-gate-no-record"
	iface := stubWGIfaceForDeactivateTest{
		Userspace:  true,
		Activities: map[string]monotime.Time{}, // empty
	}
	mgr := newConnMgrForGateTest(iface, 180)
	conn := newTestConnWithRemoteMode(t, peerKey, "p2p-dynamic")
	mgr.peerStore.AddPeerConn(conn.GetKey(), conn)

	mgr.DeactivatePeer(conn)

	if !conn.IsIntentionallyDetached() {
		t.Fatalf("IsIntentionallyDetached() = false with no activity record; detach should have proceeded")
	}
}

// TestConnMgr_localActivityGateWindow: clamp-behaviour table for the
// helper that derives the gate window from p2pTimeoutSecs/2.
func TestConnMgr_localActivityGateWindow(t *testing.T) {
	cases := []struct {
		name           string
		p2pTimeoutSecs uint32
		want           time.Duration
	}{
		{"zero -> Min", 0, localActivityGateMinWindow},
		{"60s -> Min (30s)", 60, localActivityGateMinWindow},
		{"180s -> 90s", 180, 90 * time.Second},
		{"600s -> 5m (=Max)", 600, localActivityGateMaxWindow},
		{"86400s -> Max (clamp from 12h)", 86400, localActivityGateMaxWindow},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mgr := &ConnMgr{p2pTimeoutSecs: c.p2pTimeoutSecs}
			got := mgr.localActivityGateWindow()
			if got != c.want {
				t.Errorf("localActivityGateWindow(p2pTimeoutSecs=%d) = %v, want %v",
					c.p2pTimeoutSecs, got, c.want)
			}
		})
	}
}
