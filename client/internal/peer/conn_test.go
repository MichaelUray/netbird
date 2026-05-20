package peer

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/netbirdio/netbird/client/iface"
	"github.com/netbirdio/netbird/client/iface/configurer"
	"github.com/netbirdio/netbird/client/iface/wgaddr"
	"github.com/netbirdio/netbird/client/iface/wgproxy"
	"github.com/netbirdio/netbird/client/internal/peer/conntype"
	"github.com/netbirdio/netbird/client/internal/peer/dispatcher"
	"github.com/netbirdio/netbird/client/internal/peer/guard"
	"github.com/netbirdio/netbird/client/internal/peer/ice"
	"github.com/netbirdio/netbird/client/internal/peer/worker"
	"github.com/netbirdio/netbird/client/internal/stdnet"
	"github.com/netbirdio/netbird/util"
)

var testDispatcher = dispatcher.NewConnectionDispatcher()

var connConf = ConnConfig{
	Key:         "LLHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
	LocalKey:    "RRHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
	Timeout:     time.Second,
	LocalWgPort: 51820,
	ICEConfig: ice.Config{
		InterfaceBlackList: nil,
	},
}

func TestMain(m *testing.M) {
	_ = util.InitLog("trace", util.LogConsole)
	code := m.Run()
	os.Exit(code)
}

func TestNewConn_interfaceFilter(t *testing.T) {
	ignore := []string{iface.WgInterfaceDefault, "tun0", "zt", "ZeroTier", "utun", "wg", "ts",
		"Tailscale", "tailscale"}

	filter := stdnet.InterfaceFilter(ignore)

	for _, s := range ignore {
		assert.Equal(t, filter(s), false)
	}

}

func TestConn_GetKey(t *testing.T) {
	swWatcher := guard.NewSRWatcher(nil, nil, nil, connConf.ICEConfig)

	sd := ServiceDependencies{
		SrWatcher:          swWatcher,
		PeerConnDispatcher: testDispatcher,
	}
	conn, err := NewConn(connConf, sd)
	if err != nil {
		return
	}

	got := conn.GetKey()

	assert.Equal(t, got, connConf.Key, "they should be equal")
}

func TestConn_OnRemoteOffer(t *testing.T) {
	swWatcher := guard.NewSRWatcher(nil, nil, nil, connConf.ICEConfig)
	sd := ServiceDependencies{
		StatusRecorder:     NewRecorder("https://mgm"),
		SrWatcher:          swWatcher,
		PeerConnDispatcher: testDispatcher,
	}
	conn, err := NewConn(connConf, sd)
	if err != nil {
		return
	}

	onNewOfferChan := make(chan struct{})

	conn.handshaker.AddRelayListener(func(remoteOfferAnswer *OfferAnswer) {
		onNewOfferChan <- struct{}{}
	})

	conn.OnRemoteOffer(OfferAnswer{
		IceCredentials: IceCredentials{
			UFrag: "test",
			Pwd:   "test",
		},
		WgListenPort: 0,
		Version:      "",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	select {
	case <-onNewOfferChan:
		// success
	case <-ctx.Done():
		t.Error("expected to receive a new offer notification, but timed out")
	}
}

func TestConn_OnRemoteAnswer(t *testing.T) {
	swWatcher := guard.NewSRWatcher(nil, nil, nil, connConf.ICEConfig)
	sd := ServiceDependencies{
		StatusRecorder:     NewRecorder("https://mgm"),
		SrWatcher:          swWatcher,
		PeerConnDispatcher: testDispatcher,
	}
	conn, err := NewConn(connConf, sd)
	if err != nil {
		return
	}

	onNewOfferChan := make(chan struct{})

	conn.handshaker.AddRelayListener(func(remoteOfferAnswer *OfferAnswer) {
		onNewOfferChan <- struct{}{}
	})

	conn.OnRemoteAnswer(OfferAnswer{
		IceCredentials: IceCredentials{
			UFrag: "test",
			Pwd:   "test",
		},
		WgListenPort: 0,
		Version:      "",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	select {
	case <-onNewOfferChan:
		// success
	case <-ctx.Done():
		t.Error("expected to receive a new offer notification, but timed out")
	}
}

func TestConn_presharedKey(t *testing.T) {
	conn1 := Conn{
		config: ConnConfig{
			Key:             "LLHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
			LocalKey:        "RRHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
			RosenpassConfig: RosenpassConfig{},
		},
	}
	conn2 := Conn{
		config: ConnConfig{
			Key:             "RRHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
			LocalKey:        "LLHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
			RosenpassConfig: RosenpassConfig{},
		},
	}

	tests := []struct {
		conn1Permissive         bool
		conn1RosenpassEnabled   bool
		conn2Permissive         bool
		conn2RosenpassEnabled   bool
		conn1ExpectedInitialKey bool
		conn2ExpectedInitialKey bool
	}{
		{
			conn1Permissive:         false,
			conn1RosenpassEnabled:   false,
			conn2Permissive:         false,
			conn2RosenpassEnabled:   false,
			conn1ExpectedInitialKey: false,
			conn2ExpectedInitialKey: false,
		},
		{
			conn1Permissive:         false,
			conn1RosenpassEnabled:   true,
			conn2Permissive:         false,
			conn2RosenpassEnabled:   true,
			conn1ExpectedInitialKey: true,
			conn2ExpectedInitialKey: true,
		},
		{
			conn1Permissive:         false,
			conn1RosenpassEnabled:   true,
			conn2Permissive:         false,
			conn2RosenpassEnabled:   false,
			conn1ExpectedInitialKey: true,
			conn2ExpectedInitialKey: false,
		},
		{
			conn1Permissive:         false,
			conn1RosenpassEnabled:   false,
			conn2Permissive:         false,
			conn2RosenpassEnabled:   true,
			conn1ExpectedInitialKey: false,
			conn2ExpectedInitialKey: true,
		},
		{
			conn1Permissive:         true,
			conn1RosenpassEnabled:   true,
			conn2Permissive:         false,
			conn2RosenpassEnabled:   false,
			conn1ExpectedInitialKey: false,
			conn2ExpectedInitialKey: false,
		},
		{
			conn1Permissive:         false,
			conn1RosenpassEnabled:   false,
			conn2Permissive:         true,
			conn2RosenpassEnabled:   true,
			conn1ExpectedInitialKey: false,
			conn2ExpectedInitialKey: false,
		},
		{
			conn1Permissive:         true,
			conn1RosenpassEnabled:   true,
			conn2Permissive:         true,
			conn2RosenpassEnabled:   true,
			conn1ExpectedInitialKey: true,
			conn2ExpectedInitialKey: true,
		},
		{
			conn1Permissive:         false,
			conn1RosenpassEnabled:   false,
			conn2Permissive:         false,
			conn2RosenpassEnabled:   true,
			conn1ExpectedInitialKey: false,
			conn2ExpectedInitialKey: true,
		},
		{
			conn1Permissive:         false,
			conn1RosenpassEnabled:   true,
			conn2Permissive:         true,
			conn2RosenpassEnabled:   true,
			conn1ExpectedInitialKey: true,
			conn2ExpectedInitialKey: true,
		},
	}

	conn1.config.RosenpassConfig.PermissiveMode = true
	for i, test := range tests {
		tcase := i + 1
		t.Run(fmt.Sprintf("Rosenpass test case %d", tcase), func(t *testing.T) {
			conn1.config.RosenpassConfig = RosenpassConfig{}
			conn2.config.RosenpassConfig = RosenpassConfig{}

			if test.conn1RosenpassEnabled {
				conn1.config.RosenpassConfig.PubKey = []byte("dummykey")
			}
			conn1.config.RosenpassConfig.PermissiveMode = test.conn1Permissive

			if test.conn2RosenpassEnabled {
				conn2.config.RosenpassConfig.PubKey = []byte("dummykey")
			}
			conn2.config.RosenpassConfig.PermissiveMode = test.conn2Permissive

			conn1PresharedKey := conn1.presharedKey(conn2.config.RosenpassConfig.PubKey)
			conn2PresharedKey := conn2.presharedKey(conn1.config.RosenpassConfig.PubKey)

			if test.conn1ExpectedInitialKey {
				if conn1PresharedKey == nil {
					t.Errorf("Case %d: Expected conn1 to have a non-nil key, but got nil", tcase)
				}
			} else {
				if conn1PresharedKey != nil {
					t.Errorf("Case %d: Expected conn1 to have a nil key, but got %v", tcase, conn1PresharedKey)
				}
			}

			// Assert conn2's key expectation
			if test.conn2ExpectedInitialKey {
				if conn2PresharedKey == nil {
					t.Errorf("Case %d: Expected conn2 to have a non-nil key, but got nil", tcase)
				}
			} else {
				if conn2PresharedKey != nil {
					t.Errorf("Case %d: Expected conn2 to have a nil key, but got %v", tcase, conn2PresharedKey)
				}
			}
		})
	}
}

// TestConn_AttachICE_NilHandshaker verifies AttachICE errors when called
// before Open() has wired up the handshaker.
func TestConn_AttachICE_NilHandshaker(t *testing.T) {
	c := &Conn{Log: log.WithField("peer", "test")}
	if err := c.AttachICE(); err == nil {
		t.Fatal("AttachICE on Conn with nil handshaker should return error")
	}
}

// TestConn_AttachICE_NilWorkerICE verifies AttachICE errors when the conn
// is in relay-forced mode (workerICE was never created).
func TestConn_AttachICE_NilWorkerICE(t *testing.T) {
	c := &Conn{
		Log:        log.WithField("peer", "test"),
		handshaker: &Handshaker{},
	}
	if err := c.AttachICE(); err == nil {
		t.Fatal("AttachICE with nil workerICE should return error (relay-forced mode)")
	}
}

// TestConn_DetachICE_NoHandshaker is a no-op idempotency check: calling
// DetachICE before Open() must not panic and must not error.
func TestConn_DetachICE_NoHandshaker(t *testing.T) {
	c := &Conn{Log: log.WithField("peer", "test")}
	if err := c.DetachICE(); err != nil {
		t.Fatalf("DetachICE with nil handshaker should be no-op, got error: %v", err)
	}
}

// TestConn_DetachICE_ClearsListener verifies DetachICE removes the ICE
// listener from the handshaker. workerICE is left nil so Close() is skipped.
func TestConn_DetachICE_ClearsListener(t *testing.T) {
	h := &Handshaker{}
	h.AddICEListener(func(o *OfferAnswer) {})
	c := &Conn{
		Log:        log.WithField("peer", "test"),
		handshaker: h,
	}

	if h.readICEListener() == nil {
		t.Fatal("precondition: handshaker should have a listener")
	}

	if err := c.DetachICE(); err != nil {
		t.Fatalf("DetachICE returned error: %v", err)
	}

	if h.readICEListener() != nil {
		t.Fatal("DetachICE should clear the ICE listener")
	}

	// Idempotent: second call is a no-op.
	if err := c.DetachICE(); err != nil {
		t.Fatalf("DetachICE second call should be no-op, got: %v", err)
	}
}

func TestConn_AttachICE_NoOpWhenSuspended(t *testing.T) {
	c := &Conn{
		Log:        log.WithField("peer", "test"),
		handshaker: &Handshaker{},
		iceBackoff: newIceBackoff(15 * time.Minute),
	}
	c.iceBackoff.markFailure() // suspend it

	// AttachICE should return nil but not actually attach
	err := c.AttachICE()
	if err != nil {
		t.Fatalf("expected nil error during backoff, got %v", err)
	}
	if c.handshaker.readICEListener() != nil {
		t.Fatal("AttachICE during backoff must NOT register a listener")
	}
}

func TestConn_AttachICE_AfterBackoffExpiry(t *testing.T) {
	c := &Conn{
		Log:        log.WithField("peer", "test"),
		handshaker: &Handshaker{},
		iceBackoff: newIceBackoff(15 * time.Minute),
	}
	c.iceBackoff.markFailure()
	// Force nextRetry into the past
	c.iceBackoff.mu.Lock()
	c.iceBackoff.nextRetry = time.Now().Add(-1 * time.Second)
	c.iceBackoff.mu.Unlock()

	// Without workerICE, AttachICE returns the "nil workerICE" error
	// -- but we only care that the backoff gate is NOT engaged anymore.
	err := c.AttachICE()
	if err == nil {
		t.Fatal("expected the relay-forced error path (nil workerICE)")
	}
	// The error should be about workerICE, not "suspended":
	if errMsg := err.Error(); !strings.Contains(errMsg, "workerICE") {
		t.Fatalf("after backoff expiry, error should be about workerICE not suspend; got %q", errMsg)
	}
}

// TestConn_AttachICE_NoOpWhenSuspended_RegressionForBackoffSchedule confirms
// that the plain AttachICE entry point continues to honour the suspend gate
// without any bypass — this is the regression-fence for Fix #3 so the long
// failure-retry schedule still parks non-user paths on relay.
func TestConn_AttachICE_NoOpWhenSuspended_RegressionForBackoffSchedule(t *testing.T) {
	c := &Conn{
		Log:        log.WithField("peer", "test"),
		handshaker: &Handshaker{},
		iceBackoff: newIceBackoff(15 * time.Minute),
	}
	// Three failures => firmly inside the exponential suspend window.
	c.iceBackoff.markFailure()
	c.iceBackoff.markFailure()
	c.iceBackoff.markFailure()
	if !c.iceBackoff.IsSuspended() {
		t.Fatal("precondition: must be suspended after 3 failures")
	}

	if err := c.AttachICE(); err != nil {
		t.Fatalf("AttachICE during suspend must return nil, got %v", err)
	}
	if c.handshaker.readICEListener() != nil {
		t.Fatal("AttachICE during suspend MUST NOT attach a listener")
	}
	if c.iceBackoff.IsSuspended() != true {
		t.Fatal("AttachICE must not clear the suspend gate")
	}
}

// TestConn_AttachICEUserInitiated_NilHandshaker verifies error-path symmetry
// with AttachICE.
func TestConn_AttachICEUserInitiated_NilHandshaker(t *testing.T) {
	c := &Conn{Log: log.WithField("peer", "test")}
	if err := c.AttachICEUserInitiated(30 * time.Second); err == nil {
		t.Fatal("AttachICEUserInitiated with nil handshaker should error")
	}
}

// TestConn_AttachICEUserInitiated_NilWorkerICE: relay-forced mode must still
// reject the user-initiated path with a clear error.
func TestConn_AttachICEUserInitiated_NilWorkerICE(t *testing.T) {
	c := &Conn{
		Log:        log.WithField("peer", "test"),
		handshaker: &Handshaker{},
	}
	if err := c.AttachICEUserInitiated(30 * time.Second); err == nil {
		t.Fatal("AttachICEUserInitiated with nil workerICE should error (relay-forced)")
	}
}

// TestConn_AttachICEUserInitiated_FirstBypassClearsSuspend simulates the
// real Phase-3.7i scenario: backoff suspended (failure #3, hourly retry),
// user-initiated activity attempts a bypass. Because workerICE is nil
// here we cannot drive the full listener-attach path, but we can verify
// the backoff gate flipped and lastUserInitiatedAttachICE was stamped.
func TestConn_AttachICEUserInitiated_FirstBypassClearsSuspend(t *testing.T) {
	c := &Conn{
		Log:        log.WithField("peer", "test"),
		handshaker: &Handshaker{},
		iceBackoff: newIceBackoff(60 * time.Minute), // generous cap so we stay suspended
	}
	for i := 0; i < 3; i++ {
		c.iceBackoff.markFailure()
	}
	if !c.iceBackoff.IsSuspended() {
		t.Fatal("precondition: suspended after 3 failures")
	}
	priorFailures := c.iceBackoff.Snapshot().Failures

	// nil workerICE => the post-bypass attach path returns an error,
	// which is fine: we only assert the bypass *side-effects*.
	_ = c.AttachICEUserInitiated(30 * time.Second)

	if c.iceBackoff.IsSuspended() {
		t.Fatal("after user-initiated bypass, backoff must not be suspended")
	}
	if got := c.iceBackoff.Snapshot().Failures; got != priorFailures {
		t.Fatalf("user-initiated bypass MUST NOT reset failures counter, got %d want %d", got, priorFailures)
	}
	if c.lastUserInitiatedAttachICE.IsZero() {
		t.Fatal("lastUserInitiatedAttachICE must be stamped after a real bypass")
	}
}

// TestConn_AttachICEUserInitiated_CooldownBlocksSecondCall: a second call
// within the cooldown window must be a no-op.
func TestConn_AttachICEUserInitiated_CooldownBlocksSecondCall(t *testing.T) {
	c := &Conn{
		Log:        log.WithField("peer", "test"),
		handshaker: &Handshaker{},
		iceBackoff: newIceBackoff(60 * time.Minute),
	}
	for i := 0; i < 3; i++ {
		c.iceBackoff.markFailure()
	}
	_ = c.AttachICEUserInitiated(30 * time.Second)
	first := c.lastUserInitiatedAttachICE
	if first.IsZero() {
		t.Fatal("first call must stamp lastUserInitiatedAttachICE")
	}
	// Re-suspend so the cooldown path is exercised (not the not-suspended
	// short-circuit). The real flow does this via a fresh markFailure().
	c.iceBackoff.markFailure()
	if !c.iceBackoff.IsSuspended() {
		t.Fatal("precondition: re-suspended after extra failure")
	}

	// Second call inside cooldown — must NOT touch the suspend gate.
	_ = c.AttachICEUserInitiated(30 * time.Second)

	if !c.iceBackoff.IsSuspended() {
		t.Fatal("second call inside cooldown must leave backoff suspended")
	}
	if !c.lastUserInitiatedAttachICE.Equal(first) {
		t.Fatal("second call inside cooldown must NOT re-stamp lastUserInitiatedAttachICE")
	}
}

// TestConn_AttachICEUserInitiated_CooldownExpiredAllowsBypass: after the
// cooldown lapses a fresh bypass goes through.
func TestConn_AttachICEUserInitiated_CooldownExpiredAllowsBypass(t *testing.T) {
	c := &Conn{
		Log:        log.WithField("peer", "test"),
		handshaker: &Handshaker{},
		iceBackoff: newIceBackoff(60 * time.Minute),
	}
	for i := 0; i < 3; i++ {
		c.iceBackoff.markFailure()
	}
	_ = c.AttachICEUserInitiated(30 * time.Second)
	// Synthetically age the stamp past the cooldown.
	c.mu.Lock()
	c.lastUserInitiatedAttachICE = time.Now().Add(-45 * time.Second)
	c.mu.Unlock()

	// Re-suspend so the gate is active again.
	c.iceBackoff.markFailure()
	if !c.iceBackoff.IsSuspended() {
		t.Fatal("precondition: re-suspended")
	}
	priorFailures := c.iceBackoff.Snapshot().Failures

	_ = c.AttachICEUserInitiated(30 * time.Second)

	if c.iceBackoff.IsSuspended() {
		t.Fatal("after cooldown expired, user-initiated bypass must run again")
	}
	if got := c.iceBackoff.Snapshot().Failures; got != priorFailures {
		t.Fatalf("failures counter must be preserved across bypasses, got %d want %d", got, priorFailures)
	}
}

// TestConn_AttachICEUserInitiated_AfterFailure_FailuresCounterIncrements:
// once the bypass leads to a fresh attempt that ALSO fails, the failure
// counter must keep climbing — not get rolled back by the bypass.
func TestConn_AttachICEUserInitiated_AfterFailure_FailuresCounterIncrements(t *testing.T) {
	c := &Conn{
		Log:        log.WithField("peer", "test"),
		handshaker: &Handshaker{},
		iceBackoff: newIceBackoff(60 * time.Minute),
	}
	for i := 0; i < 3; i++ {
		c.iceBackoff.markFailure()
	}
	preBypass := c.iceBackoff.Snapshot().Failures

	_ = c.AttachICEUserInitiated(30 * time.Second)
	// pion would normally drive onICEFailed; emulate it directly.
	c.onICEFailed()

	if got := c.iceBackoff.Snapshot().Failures; got != preBypass+1 {
		t.Fatalf("post-bypass failure must increment counter: got %d want %d", got, preBypass+1)
	}
	if !c.iceBackoff.IsSuspended() {
		t.Fatal("post-bypass failure must re-suspend the backoff")
	}
}

func TestConn_OnICEFailed_MarksBackoffFailure(t *testing.T) {
	c := &Conn{
		Log:        log.WithField("peer", "test"),
		iceBackoff: newIceBackoff(15 * time.Minute),
	}
	if c.iceBackoff.IsSuspended() {
		t.Fatal("precondition: not suspended")
	}
	c.onICEFailed()
	if !c.iceBackoff.IsSuspended() {
		t.Fatal("after onICEFailed, must be suspended")
	}
	if c.iceBackoff.Snapshot().Failures != 1 {
		t.Fatalf("failures must be 1, got %d", c.iceBackoff.Snapshot().Failures)
	}
}

func TestConn_OnICEConnected_ResetsBackoff(t *testing.T) {
	c := &Conn{
		Log:        log.WithField("peer", "test"),
		iceBackoff: newIceBackoff(15 * time.Minute),
	}
	c.iceBackoff.markFailure()
	c.iceBackoff.markFailure()
	c.onICEConnected()
	snap := c.iceBackoff.Snapshot()
	if snap.Failures != 0 || snap.Suspended {
		t.Fatalf("after onICEConnected: %+v", snap)
	}
}

func TestConn_presharedKey_RosenpassManaged(t *testing.T) {
	conn := Conn{
		config: ConnConfig{
			Key:             "LLHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
			LocalKey:        "RRHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
			RosenpassConfig: RosenpassConfig{PubKey: []byte("dummykey")},
		},
	}

	// When Rosenpass has already initialized the PSK for this peer,
	// presharedKey must return nil to avoid UpdatePeer overwriting it.
	conn.rosenpassInitializedPresharedKeyValidator = func(peerKey string) bool { return true }
	if k := conn.presharedKey([]byte("remote")); k != nil {
		t.Fatalf("expected nil presharedKey when Rosenpass manages PSK, got %v", k)
	}

	// When Rosenpass hasn't taken over yet, presharedKey should provide
	// a non-nil initial key (deterministic or from NetBird PSK).
	conn.rosenpassInitializedPresharedKeyValidator = func(peerKey string) bool { return false }
	if k := conn.presharedKey([]byte("remote")); k == nil {
		t.Fatalf("expected non-nil presharedKey before Rosenpass manages PSK")
	}
}

// --- Fix #2 (Phase 3.7i): WG-handshake-watchdog endpoint fallback ----------

// stubWGIface is the minimum WGIface implementation needed by the
// endpoint-fallback tests. It only records UpdatePeer calls.
type stubWGIface struct {
	updatePeerCalls atomic.Int32
	lastEndpoint    atomic.Pointer[net.UDPAddr]
	updatePeerErr   error
}

func (s *stubWGIface) UpdatePeer(_ string, _ []netip.Prefix, _ time.Duration, endpoint *net.UDPAddr, _ *wgtypes.Key) error {
	s.updatePeerCalls.Add(1)
	if endpoint != nil {
		ep := *endpoint
		s.lastEndpoint.Store(&ep)
	}
	return s.updatePeerErr
}
func (s *stubWGIface) RemovePeer(_ string) error                            { return nil }
func (s *stubWGIface) GetStats() (map[string]configurer.WGStats, error)     { return nil, nil }
func (s *stubWGIface) GetProxy() wgproxy.Proxy                              { return nil }
func (s *stubWGIface) Address() wgaddr.Address                              { return wgaddr.Address{} }
func (s *stubWGIface) RemoveEndpointAddress(_ string) error                 { return nil }

// stubRelayProxy implements wgproxy.Proxy for tests. It records Work/Pause/
// CloseConn calls and reports a fixed EndpointAddr.
type stubRelayProxy struct {
	endpoint      *net.UDPAddr
	connected     atomic.Bool
	workCalls     atomic.Int32
	pauseCalls    atomic.Int32
	closeCalls    atomic.Int32
	redirectCalls atomic.Int32
}

func newStubRelayProxy(ep *net.UDPAddr, connected bool) *stubRelayProxy {
	p := &stubRelayProxy{endpoint: ep}
	p.connected.Store(connected)
	return p
}

func (p *stubRelayProxy) AddTurnConn(_ context.Context, _ *net.UDPAddr, _ net.Conn) error {
	return nil
}
func (p *stubRelayProxy) EndpointAddr() *net.UDPAddr     { return p.endpoint }
func (p *stubRelayProxy) Work()                          { p.workCalls.Add(1) }
func (p *stubRelayProxy) Pause()                         { p.pauseCalls.Add(1) }
func (p *stubRelayProxy) RedirectAs(_ *net.UDPAddr)      { p.redirectCalls.Add(1) }
func (p *stubRelayProxy) CloseConn() error               { p.closeCalls.Add(1); return nil }
func (p *stubRelayProxy) SetDisconnectListener(_ func()) {}

// newOnWGDisconnectedTestConn builds a Conn primed for the onWGDisconnected
// fallback tests:
//   - currentConnPriority=ICEP2P so the ICE branch runs
//   - non-nil workerICE is intentionally omitted (the production guard makes
//     workerICE.Close() a no-op when nil; the load-bearing behaviour is the
//     subsequent endpoint switch and backoff bookkeeping)
//   - working endpointUpdater backed by stubWGIface
func newOnWGDisconnectedTestConn(t *testing.T, withRelayProxy bool) (*Conn, *stubWGIface, *stubRelayProxy) {
	t.Helper()
	iface := &stubWGIface{}
	wgConfig := WgConfig{
		RemoteKey:    "remote-peer-key",
		WgInterface:  iface,
		AllowedIps:   []netip.Prefix{netip.MustParsePrefix("100.64.0.5/32")},
		PreSharedKey: nil,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := &Conn{
		Log:                 log.WithField("peer", "wg-disc-test"),
		ctx:                 ctx,
		ctxCancel:           cancel,
		config:              ConnConfig{Key: "remote-peer-key", LocalKey: "local-key", WgConfig: wgConfig},
		statusRelay:         worker.NewAtomicStatus(),
		statusICE:           worker.NewAtomicStatus(),
		currentConnPriority: conntype.ICEP2P,
		iceBackoff:          newIceBackoff(15 * time.Minute),
		endpointUpdater:     NewEndpointUpdater(log.WithField("peer", "wg-disc-test"), wgConfig, true),
	}

	var proxy *stubRelayProxy
	if withRelayProxy {
		proxy = newStubRelayProxy(&net.UDPAddr{IP: net.ParseIP("127.1.0.5"), Port: 51820}, true)
		c.wgProxyRelay = proxy
	}
	return c, iface, proxy
}

// TestConn_OnWGDisconnected_ICEMode_FallbackToRelay verifies the core Fix #2
// invariant: when the WG watchdog fires while currentConnPriority is ICE
// and a relay proxy is up, the endpoint is explicitly redirected back to
// the relay proxy and the failure is counted toward the ICE backoff.
func TestConn_OnWGDisconnected_ICEMode_FallbackToRelay(t *testing.T) {
	c, iface, proxy := newOnWGDisconnectedTestConn(t, true)

	c.onWGDisconnected()

	if got := iface.updatePeerCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 UpdatePeer call, got %d", got)
	}
	ep := iface.lastEndpoint.Load()
	if ep == nil {
		t.Fatal("UpdatePeer must be called with a non-nil endpoint")
	}
	if !ep.IP.Equal(proxy.endpoint.IP) || ep.Port != proxy.endpoint.Port {
		t.Fatalf("UpdatePeer called with wrong endpoint: got %v want %v", ep, proxy.endpoint)
	}
	if got := proxy.workCalls.Load(); got != 1 {
		t.Fatalf("relay proxy.Work() must be called exactly once, got %d", got)
	}
	if c.currentConnPriority != conntype.Relay {
		t.Fatalf("currentConnPriority must switch to Relay, got %v", c.currentConnPriority)
	}
	snap := c.iceBackoff.Snapshot()
	if snap.Failures != 1 {
		t.Fatalf("ICE backoff must record exactly 1 failure, got %d", snap.Failures)
	}
	if !snap.Suspended {
		t.Fatal("ICE backoff must be suspended after the failure")
	}
}

// TestConn_OnWGDisconnected_ICEMode_NoRelayProxy: when no relay proxy is up
// (e.g. relay-forced disabled or relay torn down), the fallback must be a
// no-op for the endpoint switch — recovery is left to pion / Guard — but
// the failure should still be counted.
func TestConn_OnWGDisconnected_ICEMode_NoRelayProxy(t *testing.T) {
	c, iface, _ := newOnWGDisconnectedTestConn(t, false)

	c.onWGDisconnected()

	if got := iface.updatePeerCalls.Load(); got != 0 {
		t.Fatalf("without a relay proxy, no UpdatePeer call expected, got %d", got)
	}
	if c.currentConnPriority != conntype.ICEP2P {
		// Priority is preserved because no successful relay swap occurred.
		t.Fatalf("currentConnPriority must stay ICEP2P when no relay is up, got %v", c.currentConnPriority)
	}
	if c.iceBackoff.Snapshot().Failures != 1 {
		t.Fatal("ICE backoff must still record the failure even without a relay")
	}
}

// TestConn_OnWGDisconnected_Idempotent verifies that the load-bearing
// helper switchEndpointToRelayLocked is safe to call twice in a row even
// after currentConnPriority has been moved to Relay by the first call.
// Calling onWGDisconnected() twice would re-enter the Relay branch which
// touches subsystems (guard, statusRecorder, metricsStages, workerRelay)
// that are not wired up in this minimal test fixture — covered by the
// dedicated relay-disconnect tests elsewhere. The Phase-3.7i fallback
// itself is fully covered by the switchEndpointToRelayLocked test below.
func TestConn_OnWGDisconnected_Idempotent(t *testing.T) {
	c, iface, proxy := newOnWGDisconnectedTestConn(t, true)

	c.onWGDisconnected()
	firstCalls := iface.updatePeerCalls.Load()
	firstWork := proxy.workCalls.Load()

	// Second invocation of just the fallback helper: currentConnPriority
	// is now Relay so the guard inside switchEndpointToRelayLocked must
	// short-circuit, leaving UpdatePeer and proxy.Work() untouched.
	c.mu.Lock()
	c.switchEndpointToRelayLocked()
	c.mu.Unlock()

	if got := iface.updatePeerCalls.Load(); got != firstCalls {
		t.Fatalf("second fallback call must not re-issue UpdatePeer: got %d want %d", got, firstCalls)
	}
	if got := proxy.workCalls.Load(); got != firstWork {
		t.Fatalf("second fallback call must not re-issue proxy.Work: got %d want %d", got, firstWork)
	}
}

// TestConn_OnWGDisconnected_AfterCtxCancel_NoOp confirms that a stale
// watchdog callback fired after the Conn was cancelled does not touch
// anything.
func TestConn_OnWGDisconnected_AfterCtxCancel_NoOp(t *testing.T) {
	c, iface, _ := newOnWGDisconnectedTestConn(t, true)
	c.ctxCancel()

	c.onWGDisconnected()

	if got := iface.updatePeerCalls.Load(); got != 0 {
		t.Fatalf("post-cancel onWGDisconnected must be a no-op, got %d UpdatePeer calls", got)
	}
	if got := c.iceBackoff.Snapshot().Failures; got != 0 {
		t.Fatalf("post-cancel onWGDisconnected must not bump failure counter, got %d", got)
	}
}

// TestConn_SwitchEndpointToRelayLocked_NoOpWhenAlreadyOnRelay: defensive
// guard against duplicate switches when a concurrent onICEStateDisconnected
// already restored the relay priority.
func TestConn_SwitchEndpointToRelayLocked_NoOpWhenAlreadyOnRelay(t *testing.T) {
	c, iface, proxy := newOnWGDisconnectedTestConn(t, true)
	c.currentConnPriority = conntype.Relay

	c.mu.Lock()
	c.switchEndpointToRelayLocked()
	c.mu.Unlock()

	if got := iface.updatePeerCalls.Load(); got != 0 {
		t.Fatalf("already-on-relay path must NOT call UpdatePeer, got %d", got)
	}
	if got := proxy.workCalls.Load(); got != 0 {
		t.Fatalf("already-on-relay path must NOT call proxy.Work, got %d", got)
	}
}

// initIceBackoffFromConfig must read conn.config.P2pRetryMaxSeconds
// through ResolveP2pRetryCap. Regression for the v0.2/v0.3 plan gap
// where the test only checked the helper's arithmetic, not the call
// site -- which is where the original bug lived (Conn.Open multiplying
// the raw uint32 by time.Second and producing a ~136-year cap on
// the sentinel input).
func TestConn_InitIceBackoffFromConfig_SentinelDisables(t *testing.T) {
	swWatcher := guard.NewSRWatcher(nil, nil, nil, connConf.ICEConfig)
	sd := ServiceDependencies{
		StatusRecorder:     NewRecorder("https://mgm"),
		SrWatcher:          swWatcher,
		PeerConnDispatcher: testDispatcher,
	}
	cfg := connConf
	cfg.P2pRetryMaxSeconds = SentinelP2pRetryDisabled
	cfg.WgConfig = WgConfig{
		RemoteKey:   "remote-peer-key",
		WgInterface: &stubWGIface{},
		AllowedIps:  []netip.Prefix{netip.MustParsePrefix("100.64.0.5/32")},
	}

	conn, err := NewConn(cfg, sd)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}

	conn.initIceBackoffFromConfig()
	if conn.iceBackoff == nil {
		t.Fatal("iceBackoff not initialized")
	}
	if got := conn.iceBackoff.maxBackoff; got != 0 {
		t.Fatalf("maxBackoff = %v, want 0 (disabled)", got)
	}
	if delay := conn.iceBackoff.markFailure(); delay != 0 {
		t.Fatalf("markFailure on disabled backoff returned %v, want 0", delay)
	}
}

// Normal non-zero, non-sentinel value: the cap must equal
// time.Duration(seconds)*time.Second.
func TestConn_InitIceBackoffFromConfig_NormalValue(t *testing.T) {
	swWatcher := guard.NewSRWatcher(nil, nil, nil, connConf.ICEConfig)
	sd := ServiceDependencies{
		StatusRecorder:     NewRecorder("https://mgm"),
		SrWatcher:          swWatcher,
		PeerConnDispatcher: testDispatcher,
	}
	cfg := connConf
	cfg.P2pRetryMaxSeconds = 600
	cfg.WgConfig = WgConfig{
		RemoteKey:   "remote-peer-key",
		WgInterface: &stubWGIface{},
		AllowedIps:  []netip.Prefix{netip.MustParsePrefix("100.64.0.5/32")},
	}

	conn, err := NewConn(cfg, sd)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}

	conn.initIceBackoffFromConfig()
	if got := conn.iceBackoff.maxBackoff; got != 10*time.Minute {
		t.Fatalf("maxBackoff = %v, want 10m", got)
	}
}

// shouldSkipBootstrapOffer is the extracted gate at the top of
// onGuardEvent (Phase-3.7i v0.5). It must:
//  1. SKIP only when remote is p2p-lazy AND this Conn has never connected.
//  2. NOT skip when the Conn has ever connected (recovery offers must
//     flow -- regression for the 2026-05-17 S26 stuck-peer incident).
//  3. NOT skip when remote is anything other than p2p-lazy.
func TestConn_ShouldSkipBootstrapOffer(t *testing.T) {
	swWatcher := guard.NewSRWatcher(nil, nil, nil, connConf.ICEConfig)

	type tc struct {
		name           string
		everConnected  bool
		remoteMode     string // RemoteEffectiveConnectionMode in status state
		registerPeer   bool   // whether to put the peer in the status recorder
		wantSkip       bool
	}
	cases := []tc{
		{"fresh peer, remote unknown -> bootstrap fires", false, "", false, false},
		{"fresh peer, remote p2p-lazy -> SKIP", false, "p2p-lazy", true, true},
		{"recovered peer, remote p2p-lazy -> bootstrap fires (recovery)", true, "p2p-lazy", true, false},
		{"fresh peer, remote p2p-dynamic -> bootstrap fires", false, "p2p-dynamic", true, false},
		{"fresh peer, remote p2p -> bootstrap fires", false, "p2p", true, false},
		{"fresh peer, remote relay-forced -> bootstrap fires", false, "relay-forced", true, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recorder := NewRecorder("https://mgm")
			cfg := connConf
			cfg.Key = "remote-" + c.name
			cfg.WgConfig = WgConfig{
				RemoteKey:   cfg.Key,
				WgInterface: &stubWGIface{},
				AllowedIps:  []netip.Prefix{netip.MustParsePrefix("100.64.0.5/32")},
			}
			sd := ServiceDependencies{
				StatusRecorder:     recorder,
				SrWatcher:          swWatcher,
				PeerConnDispatcher: testDispatcher,
			}
			conn, err := NewConn(cfg, sd)
			if err != nil {
				t.Fatalf("NewConn: %v", err)
			}

			if c.registerPeer {
				if err := recorder.AddPeer(cfg.Key, "", ""); err != nil {
					t.Fatalf("AddPeer: %v", err)
				}
				if err := recorder.UpdatePeerRemoteMeta(cfg.Key, RemoteMeta{
					EffectiveConnectionMode: c.remoteMode,
				}); err != nil {
					t.Fatalf("UpdatePeerRemoteMeta: %v", err)
				}
			}
			if c.everConnected {
				conn.everConnected.Store(true)
			}

			got := conn.shouldSkipBootstrapOffer()
			if got != c.wantSkip {
				t.Errorf("shouldSkipBootstrapOffer = %v, want %v", got, c.wantSkip)
			}
		})
	}
}
