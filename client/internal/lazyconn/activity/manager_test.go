package activity

import (
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/netbirdio/netbird/client/iface/wgaddr"
	"github.com/netbirdio/netbird/client/internal/lazyconn"
	peerid "github.com/netbirdio/netbird/client/internal/peer/id"
)

type MocPeer struct {
	PeerID string
}

func (m *MocPeer) ConnID() peerid.ConnID {
	return peerid.ConnID(m)
}

type MocWGIface struct {
}

func (m MocWGIface) RemovePeer(string) error {
	return nil
}

func (m MocWGIface) UpdatePeer(string, []netip.Prefix, time.Duration, *net.UDPAddr, *wgtypes.Key) error {
	return nil
}

func (m MocWGIface) IsUserspaceBind() bool {
	return false
}

func (m MocWGIface) Address() wgaddr.Address {
	return wgaddr.Address{
		IP:      netip.MustParseAddr("100.64.0.1"),
		Network: netip.MustParsePrefix("100.64.0.0/16"),
	}
}

// GetPeerListener is a test helper to access listeners
func (m *Manager) GetPeerListener(peerConnID peerid.ConnID) (listener, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	l, exists := m.peers[peerConnID]
	return l, exists
}

func TestManager_MonitorPeerActivity(t *testing.T) {
	mocWgInterface := &MocWGIface{}

	peer1 := &MocPeer{
		PeerID: "examplePublicKey1",
	}
	mgr := NewManager(mocWgInterface)
	defer mgr.Close()
	peerCfg1 := lazyconn.PeerConfig{
		PublicKey:  peer1.PeerID,
		PeerConnID: peer1.ConnID(),
		Log:        log.WithField("peer", "examplePublicKey1"),
	}

	if err := mgr.MonitorPeerActivity(peerCfg1); err != nil {
		t.Fatalf("failed to monitor peer activity: %v", err)
	}

	listener, exists := mgr.GetPeerListener(peerCfg1.PeerConnID)
	if !exists {
		t.Fatalf("peer listener not found")
	}

	// Get the UDP listener's address for triggering
	udpListener, ok := listener.(*UDPListener)
	if !ok {
		t.Fatalf("expected UDPListener")
	}
	if err := trigger(udpListener.conn.LocalAddr().String()); err != nil {
		t.Fatalf("failed to trigger activity: %v", err)
	}

	select {
	case peerConnID := <-mgr.OnActivityChan:
		if peerConnID != peerCfg1.PeerConnID {
			t.Fatalf("unexpected peerConnID: %v", peerConnID)
		}
	case <-time.After(1 * time.Second):
	}
}

func TestManager_RemovePeerActivity(t *testing.T) {
	mocWgInterface := &MocWGIface{}

	peer1 := &MocPeer{
		PeerID: "examplePublicKey1",
	}
	mgr := NewManager(mocWgInterface)
	defer mgr.Close()

	peerCfg1 := lazyconn.PeerConfig{
		PublicKey:  peer1.PeerID,
		PeerConnID: peer1.ConnID(),
		Log:        log.WithField("peer", "examplePublicKey1"),
	}

	if err := mgr.MonitorPeerActivity(peerCfg1); err != nil {
		t.Fatalf("failed to monitor peer activity: %v", err)
	}

	listener, _ := mgr.GetPeerListener(peerCfg1.PeerConnID)
	udpListener, _ := listener.(*UDPListener)
	addr := udpListener.conn.LocalAddr().String()

	mgr.RemovePeer(peerCfg1.Log, peerCfg1.PeerConnID)

	if err := trigger(addr); err != nil {
		t.Fatalf("failed to trigger activity: %v", err)
	}

	select {
	case <-mgr.OnActivityChan:
		t.Fatal("should not have active activity")
	case <-time.After(1 * time.Second):
	}
}

func TestManager_MultiPeerActivity(t *testing.T) {
	mocWgInterface := &MocWGIface{}

	peer1 := &MocPeer{
		PeerID: "examplePublicKey1",
	}
	mgr := NewManager(mocWgInterface)
	defer mgr.Close()

	peerCfg1 := lazyconn.PeerConfig{
		PublicKey:  peer1.PeerID,
		PeerConnID: peer1.ConnID(),
		Log:        log.WithField("peer", "examplePublicKey1"),
	}

	peer2 := &MocPeer{}
	peerCfg2 := lazyconn.PeerConfig{
		PublicKey:  peer2.PeerID,
		PeerConnID: peer2.ConnID(),
		Log:        log.WithField("peer", "examplePublicKey2"),
	}

	if err := mgr.MonitorPeerActivity(peerCfg1); err != nil {
		t.Fatalf("failed to monitor peer activity: %v", err)
	}

	if err := mgr.MonitorPeerActivity(peerCfg2); err != nil {
		t.Fatalf("failed to monitor peer activity: %v", err)
	}

	listener, exists := mgr.GetPeerListener(peerCfg1.PeerConnID)
	if !exists {
		t.Fatalf("peer listener for peer1 not found")
	}

	udpListener1, _ := listener.(*UDPListener)
	if err := trigger(udpListener1.conn.LocalAddr().String()); err != nil {
		t.Fatalf("failed to trigger activity: %v", err)
	}

	listener, exists = mgr.GetPeerListener(peerCfg2.PeerConnID)
	if !exists {
		t.Fatalf("peer listener for peer2 not found")
	}

	udpListener2, _ := listener.(*UDPListener)
	if err := trigger(udpListener2.conn.LocalAddr().String()); err != nil {
		t.Fatalf("failed to trigger activity: %v", err)
	}

	for i := 0; i < 2; i++ {
		select {
		case <-mgr.OnActivityChan:
		case <-time.After(1 * time.Second):
			t.Fatal("timed out waiting for activity")
		}
	}
}

func TestActivityManager_HasPeer_Empty(t *testing.T) {
	mgr := NewManager(&MocWGIface{})
	defer mgr.Close()
	dummy := &MocPeer{PeerID: "nonexistent"}
	if mgr.HasPeer(dummy.ConnID()) {
		t.Fatal("expected HasPeer == false on empty Manager")
	}
}

func TestActivityManager_HasPeer_AfterMonitor(t *testing.T) {
	mgr := NewManager(&MocWGIface{})
	defer mgr.Close()
	peer := &MocPeer{PeerID: "peerA"}
	cfg := lazyconn.PeerConfig{
		PublicKey:  peer.PeerID,
		PeerConnID: peer.ConnID(),
		Log:        log.WithField("peer", peer.PeerID),
	}
	if err := mgr.MonitorPeerActivity(cfg); err != nil {
		t.Fatalf("MonitorPeerActivity: %v", err)
	}
	if !mgr.HasPeer(cfg.PeerConnID) {
		t.Fatal("expected HasPeer == true after MonitorPeerActivity")
	}
}

func TestActivityManager_HasPeer_AfterRemove(t *testing.T) {
	mgr := NewManager(&MocWGIface{})
	defer mgr.Close()
	peer := &MocPeer{PeerID: "peerB"}
	cfg := lazyconn.PeerConfig{
		PublicKey:  peer.PeerID,
		PeerConnID: peer.ConnID(),
		Log:        log.WithField("peer", peer.PeerID),
	}
	if err := mgr.MonitorPeerActivity(cfg); err != nil {
		t.Fatalf("MonitorPeerActivity: %v", err)
	}
	mgr.RemovePeer(cfg.Log, cfg.PeerConnID)
	if mgr.HasPeer(cfg.PeerConnID) {
		t.Fatal("expected HasPeer == false after RemovePeer")
	}
}

func TestActivityManager_HasPeer_RaceSafe(t *testing.T) {
	mgr := NewManager(&MocWGIface{})
	defer mgr.Close()
	peer := &MocPeer{PeerID: "peerC"}
	cfg := lazyconn.PeerConfig{
		PublicKey:  peer.PeerID,
		PeerConnID: peer.ConnID(),
		Log:        log.WithField("peer", peer.PeerID),
	}
	if err := mgr.MonitorPeerActivity(cfg); err != nil {
		t.Fatalf("MonitorPeerActivity: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			mgr.RemovePeer(cfg.Log, cfg.PeerConnID)
			_ = mgr.MonitorPeerActivity(cfg)
		}
	}()
	for i := 0; i < 1000; i++ {
		_ = mgr.HasPeer(cfg.PeerConnID)
	}
	<-done
}

func trigger(addr string) error {
	// Create a connection to the destination UDP address and port
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Write the bytes to the UDP connection
	_, err = conn.Write([]byte{0x01, 0x02, 0x03, 0x04, 0x05})
	if err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Android LazyConn race-bug regression tests
//
// Background: production-reproduced bug on Samsung S21/S24+ where activity
// listeners enter a stale state after long uptime + repeated network capability
// changes. Symptom: BindListener logs "activity detected via LazyConn" plus
// "removing lazy endpoint" plus "Removing peer from interface tun0", but the
// expected follow-up "detected peer activity" never arrives. Manager never
// publishes on OnActivityChan; peer is silently stuck until app force-restart.
//
// Root cause hypothesis: waitForTraffic checks m.peers after ReadPackets
// returns. If a concurrent Manager.RemovePeer call deletes the listener
// between the ReadPackets exit and the lock acquisition in waitForTraffic,
// the activity edge is dropped silently and the lazyconn-manager upper layer
// is never notified.
// ---------------------------------------------------------------------------

// fakeListener is a deterministic test double for the activity listener.
// It lets the test choose when ReadPackets returns and whether it returns
// because of "activity" or because of "close/cancel".
type fakeListener struct {
	// activityCh: closing this channel simulates a real activity edge that
	// would normally unblock the production listener's read loop.
	activityCh chan struct{}
	// closeCh: closing this channel simulates the listener being closed by
	// the manager (Manager.Close, Manager.RemovePeer) or by a context
	// cancellation. Production code returns from ReadPackets via this path
	// without notifying the upper manager layer.
	closeCh chan struct{}
	closed  sync.Mutex
}

func newFakeListener() *fakeListener {
	return &fakeListener{
		activityCh: make(chan struct{}),
		closeCh:    make(chan struct{}),
	}
}

func (f *fakeListener) ReadPackets() {
	select {
	case <-f.activityCh:
	case <-f.closeCh:
	}
}

func (f *fakeListener) Close() {
	f.closed.Lock()
	defer f.closed.Unlock()
	select {
	case <-f.closeCh:
		// already closed; idempotent
	default:
		close(f.closeCh)
	}
}

// fireActivity unblocks ReadPackets via the activity path.
func (f *fakeListener) fireActivity() {
	close(f.activityCh)
}

// TestWaitForTraffic_NotifiesActivityEvenIfPeerRemovedAfterRead reproduces the
// Android stale-peer bug: a concurrent Manager.RemovePeer (or any other code
// path that deletes the peer from m.peers) racing with a real activity edge
// must NOT silently drop the activity event. Without the fix the bug appears
// as a timeout on OnActivityChan: the listener observed activity but the
// manager loop above never learns about it.
func TestWaitForTraffic_NotifiesActivityEvenIfPeerRemovedAfterRead(t *testing.T) {
	mgr := NewManager(&MocWGIface{})
	defer mgr.Close()

	peer := &MocPeer{PeerID: "race-victim"}
	peerConnID := peer.ConnID()
	fl := newFakeListener()

	// Manually inject the fake listener into the manager's internal map.
	// MonitorPeerActivity would create a real UDP/Bind listener; we want to
	// observe the precise behaviour of waitForTraffic without depending on
	// the listener implementation under test.
	mgr.mu.Lock()
	mgr.peers[peerConnID] = fl
	mgr.mu.Unlock()

	go mgr.waitForTraffic(fl, peerConnID)

	// Simulate the racing RemovePeer that removes the entry from m.peers
	// while the listener is still blocked in ReadPackets. The production
	// trigger we have evidence for is engine.updateNetworkMap -> removePeers
	// -> connMgr.RemovePeerConn -> lazyConnMgr.RemovePeer.
	mgr.mu.Lock()
	delete(mgr.peers, peerConnID)
	mgr.mu.Unlock()

	// Now the real activity edge arrives. With the bug, waitForTraffic sees
	// the peer is not in m.peers and exits without notify. With the fix,
	// the activity edge MUST still produce a notify because the listener
	// observed real packet activity, regardless of map-membership.
	fl.fireActivity()

	select {
	case got := <-mgr.OnActivityChan:
		if got != peerConnID {
			t.Fatalf("expected notify for %v, got %v", peerConnID, got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected activity notification but got none -- " +
			"this is the production-reproduced Android LazyConn lost-edge bug")
	}
}

// TestWaitForTraffic_DoesNotNotifyOnClose pins the inverse invariant:
// when ReadPackets returns because the listener was closed (Manager.Close
// or Manager.RemovePeer or context cancellation), no synthetic activity
// notification must be published. This test passes against both the current
// (broken-for-the-other-case) implementation and the proposed fix, but is
// kept as a regression pin so a future patch cannot accidentally turn close
// into a synthetic activity event.
func TestWaitForTraffic_DoesNotNotifyOnClose(t *testing.T) {
	mgr := NewManager(&MocWGIface{})
	defer mgr.Close()

	peer := &MocPeer{PeerID: "closed-listener"}
	peerConnID := peer.ConnID()
	fl := newFakeListener()

	mgr.mu.Lock()
	mgr.peers[peerConnID] = fl
	mgr.mu.Unlock()

	go mgr.waitForTraffic(fl, peerConnID)

	// Close the listener without firing activity. Simulates the production
	// Manager.RemovePeer path (delete from m.peers, then listener.Close)
	// or a context cancellation path. In either case there is no real
	// activity to report.
	mgr.mu.Lock()
	delete(mgr.peers, peerConnID)
	mgr.mu.Unlock()
	fl.Close()

	select {
	case got := <-mgr.OnActivityChan:
		t.Fatalf("unexpected activity notification after listener close: %v", got)
	case <-time.After(150 * time.Millisecond):
		// healthy: no notification
	}
}
