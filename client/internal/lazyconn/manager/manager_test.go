package manager

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/netbirdio/netbird/client/iface/wgaddr"
	"github.com/netbirdio/netbird/client/internal/lazyconn"
	"github.com/netbirdio/netbird/client/internal/lazyconn/inactivity"
	peerid "github.com/netbirdio/netbird/client/internal/peer/id"
	"github.com/netbirdio/netbird/client/internal/peerstore"
	"github.com/netbirdio/netbird/monotime"
)

// mockWGIface satisfies lazyconn.WGIface for unit tests in this package.
// It provides nil-implementations for all interface methods plus a
// controllable LastActivities map that tests can mutate.
type mockWGIface struct {
	mu             sync.Mutex
	lastActivities map[string]monotime.Time
	updateCalls    []updatePeerCall
}

type updatePeerCall struct {
	peerKey    string
	allowedIPs []netip.Prefix
}

func newMockWGIface() *mockWGIface {
	return &mockWGIface{lastActivities: map[string]monotime.Time{}}
}

func (m *mockWGIface) RemovePeer(string) error { return nil }
func (m *mockWGIface) UpdatePeer(peerKey string, allowedIPs []netip.Prefix, _ time.Duration, _ *net.UDPAddr, _ *wgtypes.Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCalls = append(m.updateCalls, updatePeerCall{
		peerKey:    peerKey,
		allowedIPs: append([]netip.Prefix(nil), allowedIPs...),
	})
	return nil
}

// IsUserspaceBind must return FALSE here. Two conflicting consumers:
//   - manager.go: only initializes m.inactivityManager when this is true
//   - activity/manager.go createListener: requires the iface to
//     implement bindProvider when this is true — our mock does not
//
// We keep this false so activity.Manager uses the NewUDPListener kernel
// path (the existing TestManager_MonitorPeerActivity proves the UDP
// path works with MocWGIface{}), and inject the inactivityManager
// manually after NewManager (see newTestHarness below).
func (m *mockWGIface) IsUserspaceBind() bool { return false }
func (m *mockWGIface) Address() wgaddr.Address {
	return wgaddr.Address{
		IP:      netip.MustParseAddr("100.64.0.1"),
		Network: netip.MustParsePrefix("100.64.0.0/16"),
	}
}
func (m *mockWGIface) LastActivities() map[string]monotime.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]monotime.Time, len(m.lastActivities))
	for k, v := range m.lastActivities {
		out[k] = v
	}
	return out
}

func (m *mockWGIface) UpdateCalls() []updatePeerCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]updatePeerCall(nil), m.updateCalls...)
}

// testHarness wires a *Manager with manually-injected two-timer
// inactivity, real activity.Manager (UDP path via false IsUserspaceBind),
// and a real peerstore.Store. Tests populate peers via the helpers
// defined in watchdog_test.go (addStuckInactivityPeer etc.).
//
// Note on "no Close was called" assertions: peerstore.Store is a
// concrete type without an IdleCalled hook. Tests assert this via
// OBSERVABLE STATE post-recovery (Conn still satisfies TransportSnapshot
// the same way, no goroutine leak), not via call-counting.
type testHarness struct {
	t         *testing.T
	ctx       context.Context
	cancel    context.CancelFunc
	wgIface   *mockWGIface
	peerStore *peerstore.Store
	mgr       *Manager
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	wgIface := newMockWGIface()
	peerStore := peerstore.NewConnStore() // verified — store.go:20
	cfg := Config{
		ICEInactivityThreshold:   time.Minute,
		RelayInactivityThreshold: time.Minute,
	}
	mgr := NewManager(cfg, ctx, peerStore, wgIface)

	// With IsUserspaceBind() == false (required so activity.Manager
	// uses the UDP listener path — see mockWGIface.IsUserspaceBind doc),
	// NewManager skips inactivityManager initialization. Inject it
	// manually with non-zero two-timer config so all
	// transitionToActivityWatcherStateOnly + reconcileTick code paths
	// can dereference it safely.
	mgr.inactivityManager = inactivity.NewManagerWithTwoTimers(wgIface, time.Minute, time.Minute)
	if mgr.inactivityManager == nil {
		t.Fatalf("test harness setup error: inactivity.NewManagerWithTwoTimers returned nil")
	}
	h := &testHarness{
		t:         t,
		ctx:       ctx,
		cancel:    cancel,
		wgIface:   wgIface,
		peerStore: peerStore,
		mgr:       mgr,
	}
	t.Cleanup(func() {
		cancel()
		// activity.Manager.MonitorPeerActivity spawns real UDP-listener
		// goroutines per peer. Without Close() they leak across tests
		// (visible as "ReadPackets" goroutines in -race reports). The
		// existing TestManager_MonitorPeerActivity in activity/manager_test.go
		// uses the same pattern (defer mgr.Close()).
		mgr.activityManager.Close()
	})
	return h
}

// newTestPeerCfg builds a minimal valid PeerConfig. PeerConnID is
// derived from a fresh pubKeyStub instance — the resulting ConnID is
// unique per call (the underlying type is unsafe.Pointer). Tests are
// expected to capture the returned cfg in a local variable and use
// cfg.PeerConnID consistently from there. Do NOT call newTestPeerCfg
// twice with the same pubKey and expect identical PeerConnIDs.
func newTestPeerCfg(pubKey string) lazyconn.PeerConfig {
	return lazyconn.PeerConfig{
		PublicKey:  pubKey,
		PeerConnID: peerid.ConnID(&pubKeyStub{pubKey}),
		Log:        log.WithField("peer", pubKey),
	}
}

// pubKeyStub mirrors the activity-package MocPeer pattern: a pointer
// whose address is converted to peerid.ConnID (= unsafe.Pointer per
// peer/id/connid.go). Holding a reference to the cfg keeps the stub
// alive for the duration of the test.
type pubKeyStub struct {
	pubKey string
}
