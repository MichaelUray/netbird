package peer

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/netbirdio/netbird/client/iface/configurer"
	"github.com/netbirdio/netbird/client/iface/wgproxy"
	"github.com/netbirdio/netbird/client/internal/metrics"
	"github.com/netbirdio/netbird/client/internal/peer/conntype"
	"github.com/netbirdio/netbird/client/internal/peer/dispatcher"
	"github.com/netbirdio/netbird/client/internal/peer/guard"
	icemaker "github.com/netbirdio/netbird/client/internal/peer/ice"
	"github.com/netbirdio/netbird/client/internal/peer/id"
	"github.com/netbirdio/netbird/client/internal/peer/worker"
	"github.com/netbirdio/netbird/client/internal/portforward"
	"github.com/netbirdio/netbird/client/internal/stdnet"
	"github.com/netbirdio/netbird/monotime"
	"github.com/netbirdio/netbird/route"
	"github.com/netbirdio/netbird/shared/connectionmode"
	relayClient "github.com/netbirdio/netbird/shared/relay/client"
)

// V18.11: cooldown window after MarkIntentionallyDetached during which
// AttachICEOnRelayActivity bails. Tuned to outlast typical OS-response
// bursts (TCP RST after relay-fallback drops the open connection,
// mDNS broadcast retries, DNS retry storms) without rejecting genuine
// sustained user traffic. 30 s = comfortably longer than the legacy
// peer signal-OFFER interval (~2-3 min) yet short enough that user
// activity after a fresh detach still wakes on the next packet burst.
const v18_11RelayActivityCooldown = 30 * time.Second

// MetricsRecorder is an interface for recording peer connection metrics
type MetricsRecorder interface {
	RecordConnectionStages(
		ctx context.Context,
		remotePubKey string,
		connectionType metrics.ConnectionType,
		isReconnection bool,
		timestamps metrics.ConnectionStageTimestamps,
	)
}

type ServiceDependencies struct {
	StatusRecorder     *Status
	Signaler           *Signaler
	IFaceDiscover      stdnet.ExternalIFaceDiscover
	RelayManager       *relayClient.Manager
	SrWatcher          *guard.SRWatcher
	PeerConnDispatcher *dispatcher.ConnectionDispatcher
	PortForwardManager *portforward.Manager
	MetricsRecorder    MetricsRecorder
}

type WgConfig struct {
	WgListenPort int
	RemoteKey    string
	WgInterface  WGIface
	AllowedIps   []netip.Prefix
	PreSharedKey *wgtypes.Key
}

type RosenpassConfig struct {
	// RosenpassPubKey is this peer's Rosenpass public key
	PubKey []byte
	// RosenpassPubKey is this peer's RosenpassAddr server address (IP:port)
	Addr string

	PermissiveMode bool
}

// ConnConfig is a peer Connection configuration
type ConnConfig struct {
	// Key is a public key of a remote peer
	Key string
	// LocalKey is a public key of a local peer
	LocalKey string

	AgentVersion string

	Timeout time.Duration

	WgConfig WgConfig

	LocalWgPort int

	RosenpassConfig RosenpassConfig

	// ICEConfig ICE protocol configuration
	ICEConfig icemaker.Config

	// Mode is the resolved connection mode for this peer (forwarded
	// from the engine, which got it from the conn_mgr precedence chain).
	// Phase 1 uses it to pick the skip-ICE branch when ModeRelayForced.
	Mode connectionmode.Mode

	// P2pRetryMaxSeconds is the cap for the ICE-failure backoff schedule
	// in p2p-dynamic mode. 0 = use built-in default (DefaultP2PRetryMax).
	// Wire-format sentinel uint32-max (= ^uint32(0)) means "user-explicit
	// disable", which the resolver translates to time.Duration(0) at
	// engine.go before passing it here. Phase 3 of #5989.
	P2pRetryMaxSeconds uint32
}

type Conn struct {
	Log                *log.Entry
	mu                 sync.Mutex
	iceBackoff         *iceBackoffState
	ctx                context.Context
	ctxCancel          context.CancelFunc
	config             ConnConfig
	statusRecorder     *Status
	signaler           *Signaler
	iFaceDiscover      stdnet.ExternalIFaceDiscover
	relayManager       *relayClient.Manager
	srWatcher          *guard.SRWatcher
	portForwardManager *portforward.Manager

	onConnected                               func(remoteWireGuardKey string, remoteRosenpassPubKey []byte, wireGuardIP string, remoteRosenpassAddr string)
	onDisconnected                            func(remotePeer string)
	rosenpassInitializedPresharedKeyValidator func(peerKey string) bool
	// onWGTimeoutRecover, when set, is invoked from onWGDisconnected
	// after the active worker has been closed. The handler should put
	// this peer back into the lazy manager's idle/activity-listening
	// state so the next outbound packet re-triggers the lazy mgr (and
	// re-attaches ICE/relay). Without this hook the peer was stuck in
	// "Connecting" forever after a WireGuard handshake timeout — the
	// lazy mgr kept the peer in its "active" set with no activity
	// listener, so local traffic was silently dropped. Codex follow-up
	// to the 6-host hardware test on c9a47ed90.
	onWGTimeoutRecover func()

	// isActivityListenerArmedFn is the V17.4 predicate consulted by
	// IsLazyDetached(): when the lazy-mgr does NOT have an activity
	// listener armed for this peer, the conn cannot wake via outbound
	// user-traffic, so V14 must let remote OFFERs through. nil means
	// "assume armed" (conservative default = preserve V14 anti-spam).
	isActivityListenerArmedFn func() bool

	statusRelay         *worker.AtomicWorkerStatus
	statusICE           *worker.AtomicWorkerStatus
	currentConnPriority conntype.ConnPriority
	opened              bool // this flag is used to prevent close in case of not opened connection
	// everConnected is set to true the first time configureConnection
	// or relay-only setup transitions this peer into a non-None
	// priority. Codex follow-up: distinguishes the "ICE detached for
	// inactivity" case (skip guard offer to avoid spam) from the
	// "never connected yet" case (must send the bootstrap offer).
	// Without this, the guard's first fire after lazy-mgr activity
	// would incorrectly skip the initial offer because no ICE
	// listener is attached YET.
	everConnected atomic.Bool

	workerICE   *WorkerICE
	workerRelay *WorkerRelay

	wgWatcher       *WGWatcher
	wgWatcherWg     sync.WaitGroup
	wgWatcherCancel context.CancelFunc

	// used to store the remote Rosenpass key for Relayed connection in case of connection update from ice
	rosenpassRemoteKey []byte

	wgProxyICE   wgproxy.Proxy
	wgProxyRelay wgproxy.Proxy
	handshaker   *Handshaker

	guard *guard.Guard
	wg    sync.WaitGroup

	// debug purpose
	dumpState *stateDump

	// diagLastEmit holds the time of the last [DIAG] line emitted for each
	// reason. Used by logDiagSnapshotDedup to suppress burst-duplicates
	// within diagDedupWindow. See conn_state_snapshot.go. Fix-D D1.1.
	diagLastEmit sync.Map

	endpointUpdater *EndpointUpdater

	// Connection stage timestamps for metrics
	metricsRecorder MetricsRecorder
	metricsStages   *MetricsStages

	// lastUserInitiatedAttachICE stamps the most recent successful
	// AttachICEUserInitiated bypass so subsequent user-initiated retries
	// within minCooldown are rate-limited. Protected by conn.mu.
	lastUserInitiatedAttachICE time.Time

	// lastRemoteOfferAttach stamps the most recent successful
	// AttachICEOnRemoteOffer bypass. Used by D3b's one-shot-per-cooldown
	// gate so a flood of incoming OFFERs (legacy guard spam) cannot
	// rapidly drain backoff bypass slots. Protected by conn.mu.
	lastRemoteOfferAttach time.Time

	// intentionallyDetached signals to the guard that the current ICE
	// detached state is the result of a remote/local GO_IDLE or local
	// inactivity timeout — not an ICE pair-check failure. The guard
	// must NOT consume its retry budget while this flag is set
	// (otherwise an intentional idle silently burns the 3-tries window
	// and parks a healthy peer on the hourly schedule).
	//
	// Phase 3.7j (#5989): marker is SET in ConnMgr.DetachICEForPeer and
	// CLEARED on every lifecycle point that legitimately re-engages ICE
	// (AttachICE / AttachICEUserInitiated / AttachICEOnRelayActivity /
	// ConnMgr.ActivatePeer pre-Open / onNetworkChange / onICEFailed).
	intentionallyDetached atomic.Bool

	// V18.11 (2026-06-09): monotonic timestamp of the most recent
	// MarkIntentionallyDetached call. Used by AttachICEOnRelayActivity
	// to enforce a post-detach cooldown window during which transient
	// outbound activity (Android system traffic, mDNS, DNS retries,
	// OS responses to inbound packets) cannot wake P2P. After the
	// cooldown elapses, real user-initiated outbound traffic can wake
	// the peer normally. Production S26 V18.10 trace: 13 re-attach
	// cycles in 30 min with V18.10 gate firing only 3 times, the other
	// 10 cycles came through AttachICEOnRelayActivity → AttachICEFrom
	// (RelayActivity) from spurious outbound packets that bypassed
	// the V18.4 isTransportPkg filter (real type-4 transport with
	// payload > 32 bytes from internal sources).
	intentionallyDetachedAt atomic.Int64

	// Phase 3.7l Fix-D Phase 1: per-peer tracking of "same srflx port
	// across consecutive ICE failures". Mutated under its own mutex
	// from onICEFailed / onICEConnected; snapshot-read from
	// snapshotForDiagnosis. See conn_srflx_state.go for the rationale
	// (Codex 2026-05-30 stuck-srflx investigation).
	srflxState srflxStateSync
}

// MarkIntentionallyDetached records that the current ICE detach is the
// result of an explicit GO_IDLE (remote or local) or a local inactivity
// timeout, rather than a pion ICE failure. The Phase-3.7j guard
// predicate reads this flag via IsIntentionallyDetached to skip the
// retry-budget consumption.
//
// Safe to call concurrently. Idempotent.
func (conn *Conn) MarkIntentionallyDetached() {
	conn.intentionallyDetached.Store(true)
	// V18.11: stamp the detach time so AttachICEOnRelayActivity can
	// enforce a post-detach cooldown window.
	conn.intentionallyDetachedAt.Store(int64(monotime.Now()))
}

// IsIntentionallyDetached returns true while the most recent ICE detach
// is still flagged as intentional. Cleared by AttachICE-family methods,
// ConnMgr.ActivatePeer, onNetworkChange and onICEFailed (see
// intentionallyDetached field comment for the full list).
//
// Safe to call concurrently.
func (conn *Conn) IsIntentionallyDetached() bool {
	return conn.intentionallyDetached.Load()
}

// ClearIntentionallyDetached resets the marker. Called by every
// lifecycle point that legitimately re-engages or invalidates the ICE
// path so the next pion failure is not masked by a stale intentional-
// detach state. Safe to call when the marker was never set.
//
// Safe to call concurrently. Idempotent.
func (conn *Conn) ClearIntentionallyDetached() {
	conn.intentionallyDetached.Store(false)
}

// EverConnected returns true if this Conn has ever completed at least
// one full configureConnection (P2P or relay). Used by the lazy-mode
// inbound-OFFER anti-spam gate (V13) to differentiate a brand-new
// peer-pair (where an inbound OFFER is the legitimate initial-connect
// trigger) from a previously-connected peer that was idle-detached by
// runDynamicInactivityLoop and is now being re-poked by a legacy
// remote (no f433f1b42).
//
// Safe to call concurrently.
func (conn *Conn) EverConnected() bool {
	return conn.everConnected.Load()
}

// IsLazyDetached reports whether this connection is currently in the
// "lazy idle after success" state — i.e. the inactivity manager fired
// DetachICEForPeer (which sets the intentional-detach marker) AND the
// connection had reached at least one successful configureConnection
// before AND the conn is still open (in active lazy-pause).
//
// This composite check is the gating predicate for V13/V14/V14.1/V13.1
// anti-spam gates: when true, an inbound signal / network-change /
// relay-activity must NOT silently re-arm ICE, otherwise legacy peers'
// eager-bootstrap traffic drives a detach/re-attach cycle that defeats
// lazy mode (see report docs/test-reports/2026-06-04-netbird-v16-
// elmira-p2p-resolved).
//
// Fix V17.2 (2026-06-06): added !opened short-circuit. When conn is
// fully-closed (opened=false, e.g. after relayTimeout PeerConnClose),
// the peer is in long-idle state and a remote-initiated OFFER from a
// legacy peer is a legitimate reconnect attempt, NOT bootstrap-spam.
// Without this short-circuit S26 stuck with Marl Creek (legacy v0.60.4)
// because the WG-peer endpoint state had drifted to public-IP, the
// activity-listener couldn't trigger, and V14 blocked every recovery
// path from the remote side — leaving the peer permanently unreachable
// (see report docs/test-reports/2026-06-06-netbird-v17.2-stuck-peer-recovery).
//
// Helper introduced 2026-06-04 by the code-review-recommended dedup of
// the three identical inline checks (V13.1 conn.go:1562, V14
// conn_mgr.go:547, V14.1 conn.go:2206).
//
// V17.4 listener-armed short-circuit removed by V18.3 (2026-06-09):
// after V18.2 reverted the DetachICEForPeer state-sync, the listener
// is never armed in the p2p-dynamic inactivity-timeout sub-state, so
// the V17.4 check always returned false → V14 anti-spam never fired
// on the ICE-detached-Relay-only path. Production S26 observed every
// 4 min legacy-peer-OFFER re-attach cycles for both Lethbridge and a
// 178.183 endpoint despite zero user traffic — exactly the
// lazy-bootstrap-spam pattern V14 was designed to block.
//
// Wake-up paths remain intact:
//   - User outbound traffic over Relay → AttachICEOnRelayActivity
//     (V13.1 gate dropped by V18.1) → SendOffer (we initiate, remote
//     responds with Answer; V14 only gates inbound OFFER) → P2P up.
//   - Remote-initiated wake from a non-legacy peer that respects lazy
//     semantics never sends bootstrap-OFFERs while we are
//     intentionally-detached, so V14 doesn't impact them.
//
// Safe to call concurrently.
func (conn *Conn) IsLazyDetached() bool {
	if !conn.opened {
		return false
	}
	return conn.IsIntentionallyDetached() && conn.everConnected.Load()
}

// NewConn creates a new not opened Conn to the remote peer.
// To establish a connection run Conn.Open
func NewConn(config ConnConfig, services ServiceDependencies) (*Conn, error) {
	if len(config.WgConfig.AllowedIps) == 0 {
		return nil, fmt.Errorf("allowed IPs is empty")
	}

	connLog := log.WithField("peer", config.Key)

	dumpState := newStateDump(config.Key, connLog, services.StatusRecorder)
	var conn = &Conn{
		Log:                connLog,
		config:             config,
		statusRecorder:     services.StatusRecorder,
		signaler:           services.Signaler,
		iFaceDiscover:      services.IFaceDiscover,
		relayManager:       services.RelayManager,
		srWatcher:          services.SrWatcher,
		portForwardManager: services.PortForwardManager,
		statusRelay:        worker.NewAtomicStatus(),
		statusICE:          worker.NewAtomicStatus(),
		dumpState:          dumpState,
		endpointUpdater:    NewEndpointUpdater(connLog, config.WgConfig, isController(config)),
		wgWatcher:          NewWGWatcher(connLog, config.WgConfig.WgInterface, config.Key, dumpState),
		metricsRecorder:    services.MetricsRecorder,
	}

	return conn, nil
}

// Open opens connection to the remote peer
// It will try to establish a connection using ICE and in parallel with relay. The higher priority connection type will
// be used.
func (conn *Conn) Open(engineCtx context.Context) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	if conn.opened {
		return nil
	}

	// Allocate new metrics stages so old goroutines don't corrupt new state
	conn.metricsStages = &MetricsStages{}

	conn.ctx, conn.ctxCancel = context.WithCancel(engineCtx)

	conn.workerRelay = NewWorkerRelay(conn.ctx, conn.Log, isController(conn.config), conn.config, conn, conn.relayManager)

	// Phase 3: initialize per-peer ICE-failure backoff. The cap goes
	// through ResolveP2pRetryCap so the wire-format sentinel
	// (^uint32(0) = user-explicit-disable) and the zero-means-default
	// case stay consistent with ConnMgr.propagateP2pRetryMaxToConns.
	conn.initIceBackoffFromConfig()

	// Mode-driven branching. ModeRelayForced skips ICE entirely; all
	// other modes (P2P, P2PLazy, P2PDynamic) construct workerICE
	// eagerly in Phase 1. Phase 2 will branch P2PDynamic separately
	// to defer the OnNewOffer registration.
	skipICE := conn.config.Mode == connectionmode.ModeRelayForced
	if !skipICE {
		relayIsSupportedLocally := conn.workerRelay.RelayIsSupportedLocally()
		workerICE, err := NewWorkerICE(conn.ctx, conn.Log, conn.config, conn, conn.signaler, conn.iFaceDiscover, conn.statusRecorder, relayIsSupportedLocally)
		if err != nil {
			return err
		}
		conn.workerICE = workerICE
	}

	conn.handshaker = NewHandshaker(conn.Log, conn.config, conn.signaler, conn.workerICE, conn.workerRelay, conn.metricsStages)

	conn.handshaker.AddRelayListener(conn.workerRelay.OnNewOffer)

	// ICE-listener registration depends on mode:
	// - ModeRelayForced: skipICE=true, no workerICE, no listener.
	// - ModeP2P, ModeP2PLazy: workerICE constructed, listener registered eagerly.
	//   P2PLazy's whole-tunnel deferral happens at the conn_mgr level, not here.
	// - ModeP2PDynamic: workerICE constructed eagerly so it's ready, but the
	//   listener registration is deferred. The inactivity manager calls
	//   Conn.AttachICE() once activity is observed on the relay tunnel.
	deferICEListener := conn.config.Mode == connectionmode.ModeP2PDynamic
	if !skipICE && !deferICEListener {
		conn.handshaker.AddICEListener(conn.workerICE.OnNewOffer)
	}

	// Phase 3.7j (#5989) Commit 2 of 5: wire the intentional-detach
	// predicate so the guard can skip its retry budget when ICE was
	// detached gracefully (GO_IDLE / local inactivity timeout). The
	// marker is set/cleared by Conn (Commit 1); the guard reads it
	// via this callback. nil-safe by design.
	conn.guard = guard.NewGuard(conn.Log, conn.isConnectedOnAllWay, conn.IsIntentionallyDetached, conn.config.Timeout, conn.srWatcher)
	// Phase 3.5 (#5989): reset ICE backoff + recreate workerICE on network change.
	// Set before Start() is called so the goroutine sees it without races.
	if !skipICE {
		conn.guard.SetOnNetworkChange(conn.onNetworkChange)
	}

	conn.wg.Add(1)
	go func() {
		defer conn.wg.Done()
		conn.handshaker.Listen(conn.ctx)
	}()
	go conn.dumpState.Start(conn.ctx)

	peerState := State{
		PubKey:           conn.config.Key,
		ConnStatusUpdate: time.Now(),
		ConnStatus:       StatusConnecting,
		Mux:              new(sync.RWMutex),
	}
	if err := conn.statusRecorder.UpdatePeerState(peerState); err != nil {
		conn.Log.Warnf("error while updating the state err: %v", err)
	}

	conn.wg.Add(1)
	go func() {
		defer conn.wg.Done()
		conn.guard.Start(conn.ctx, conn.onGuardEvent)
	}()
	conn.opened = true
	return nil
}

// Close closes this peer Conn issuing a close event to the Conn closeCh.
//
// keepWgPeer controls whether the WireGuard peer entry is removed at
// the iface layer. Pass true on the lazy-suspend path
// (lazy-mgr deactivate, WG-timeout-recover) so that routed-subnet
// AllowedIPs the route-manager appended remain intact -- otherwise the
// peer goes Idle, comes back via the activity listener, and routed
// traffic to the peer's advertised subnets is silently dropped until
// the next mgmt-side reconcile re-attaches them. Pass false on the
// permanent-removal path (engine.removePeer, mode-change tear-down)
// where the peer should disappear from the WG iface entirely.
//
// See docs/bugs/2026-05-04-lazy-wake-on-routed-subnet.md for the full
// mechanism analysis. Regression tests live in
// conn_lazy_keepwgpeer_test.go.
func (conn *Conn) Close(signalToRemote bool, keepWgPeer bool) {
	conn.mu.Lock()
	defer conn.wgWatcherWg.Wait()
	defer conn.mu.Unlock()

	if !conn.opened {
		conn.Log.Debugf("ignore close connection to peer")
		return
	}

	if signalToRemote {
		if err := conn.signaler.SignalIdle(conn.config.Key); err != nil {
			conn.Log.Errorf("failed to signal idle state to peer: %v", err)
		}
	}

	conn.Log.Infof("close peer connection (keepWgPeer=%v)", keepWgPeer)
	conn.ctxCancel()

	if conn.wgWatcherCancel != nil {
		conn.wgWatcherCancel()
	}
	conn.workerRelay.CloseConn()
	if conn.workerICE != nil {
		conn.workerICE.Close()
	}

	if conn.wgProxyRelay != nil {
		err := conn.wgProxyRelay.CloseConn()
		if err != nil {
			conn.Log.Errorf("failed to close wg proxy for relay: %v", err)
		}
		conn.wgProxyRelay = nil
	}

	if conn.wgProxyICE != nil {
		err := conn.wgProxyICE.CloseConn()
		if err != nil {
			conn.Log.Errorf("failed to close wg proxy for ice: %v", err)
		}
		conn.wgProxyICE = nil
	}

	if !keepWgPeer {
		if err := conn.endpointUpdater.RemoveWgPeer(); err != nil {
			conn.Log.Errorf("failed to remove wg endpoint: %v", err)
		}
	} else {
		// Lazy-suspend: keep the WG peer entry so route-manager-applied
		// AllowedIPs (advertised subnets) survive the wake/sleep cycle.
		// The lazy listener that runs next will UpdatePeer in-place to
		// switch the endpoint to its fake 127.2.x.y target -- the
		// AllowedIPs (peer-IP /32 + routed prefixes) stay intact.
		conn.Log.Debugf("keeping WG peer entry across lazy-suspend so routed-subnet AllowedIPs survive")
	}

	if conn.evalStatus() == StatusConnected && conn.onDisconnected != nil {
		conn.onDisconnected(conn.config.WgConfig.RemoteKey)
	}

	conn.setStatusToDisconnected()
	conn.opened = false
	conn.wg.Wait()
	conn.Log.Infof("peer connection closed")
}

// OnRemoteAnswer handles an offer from the remote peer and returns true if the message was accepted, false otherwise
// doesn't block, discards the message if connection wasn't ready
func (conn *Conn) OnRemoteAnswer(answer OfferAnswer) {
	// V15.1 nil-safe: same as OnRemoteOffer — handshaker may be nil
	// when V15 cold-boot-gate suppressed the activation pipeline.
	if conn.handshaker == nil {
		conn.Log.Tracef("OnRemoteAnswer dropped: handshaker not initialized (peer in lazy-idle pre-Open)")
		return
	}
	conn.dumpState.RemoteAnswer()
	conn.Log.Infof("OnRemoteAnswer, priority: %s, status ICE: %s, status relay: %s", conn.currentConnPriority, conn.statusICE, conn.statusRelay)
	conn.handshaker.OnRemoteAnswer(answer)

	// Track C 2026-05-31: legacy peer candidate replay (Codex Plan v4).
	// Goroutine because MaybeReplayCandidates sleeps internally before
	// taking the snapshot. We hook BOTH OnRemoteAnswer and
	// OnRemoteOffer so the replay fires regardless of whether we are
	// the offer-initiator or offer-receiver role.
	if conn.workerICE != nil {
		go conn.workerICE.MaybeReplayCandidates(answer.Version)
	}
}

// OnRemoteCandidate Handles ICE connection Candidate provided by the remote peer.
func (conn *Conn) OnRemoteCandidate(candidate ice.Candidate, haRoutes route.HAMap) {
	conn.dumpState.RemoteCandidate()
	if conn.workerICE != nil {
		conn.workerICE.OnRemoteCandidate(candidate, haRoutes)
	}
}

// SetOnConnected sets a handler function to be triggered by Conn when a new connection to a remote peer established
func (conn *Conn) SetOnConnected(handler func(remoteWireGuardKey string, remoteRosenpassPubKey []byte, wireGuardIP string, remoteRosenpassAddr string)) {
	conn.onConnected = handler
}

// SetOnDisconnected sets a handler function to be triggered by Conn when a connection to a remote disconnected
func (conn *Conn) SetOnDisconnected(handler func(remotePeer string)) {
	conn.onDisconnected = handler
}

// SetOnWGTimeoutRecover wires the lazy-mgr recovery callback. ConnMgr
// installs this so that a WG-handshake-timeout pushes the peer back
// into the activity-listening idle state. See onWGTimeoutRecover docs
// on the Conn struct for the full rationale.
func (conn *Conn) SetOnWGTimeoutRecover(handler func()) {
	conn.onWGTimeoutRecover = handler
}

// SetIsActivityListenerArmedFn wires a predicate used by the V17.4
// stuck-recovery branch of IsLazyDetached(). When the conn is in the
// active lazy-pause state (opened=true + intentionallyDetached=true)
// but the lazy-mgr has NOT armed an activity listener for this peer
// (e.g. iceTimeout sub-state before relayTimeout, or after
// engine.go:2801 remote-offline-close without lazy-mgr transition),
// no outbound traffic edge can wake the peer — the remote OFFER is
// the only recovery path, so V14 must let it through. The predicate
// returns true if the listener is currently armed.
func (conn *Conn) SetIsActivityListenerArmedFn(fn func() bool) {
	conn.isActivityListenerArmedFn = fn
}

// SetRosenpassInitializedPresharedKeyValidator sets a function to check if Rosenpass has taken over
// PSK management for a peer. When this returns true, presharedKey() returns nil
// to prevent UpdatePeer from overwriting the Rosenpass-managed PSK.
func (conn *Conn) SetRosenpassInitializedPresharedKeyValidator(handler func(peerKey string) bool) {
	conn.rosenpassInitializedPresharedKeyValidator = handler
}

func (conn *Conn) OnRemoteOffer(offer OfferAnswer) {
	// V15.1 nil-safe (2026-06-04): when V15 cold-boot-gate in
	// ConnMgr.ActivatePeerForMessage drops the upstream activation,
	// conn.Open() is never called, so conn.handshaker stays nil. The
	// engine still dispatches OnRemoteOffer downstream of
	// ActivatePeerForMessage, which then NPE'd here. The fix is
	// defensive: if no handshaker, the peer has no listening offer
	// pipeline anyway — just drop the offer.
	if conn.handshaker == nil {
		conn.Log.Tracef("OnRemoteOffer dropped: handshaker not initialized (peer in lazy-idle pre-Open)")
		return
	}
	conn.dumpState.RemoteOffer()
	conn.Log.Infof("OnRemoteOffer, on status ICE: %s, status Relay: %s", conn.statusICE, conn.statusRelay)
	conn.handshaker.OnRemoteOffer(offer)

	// Track C 2026-05-31: legacy peer candidate replay also fires when
	// WE are the offer-receiver role. Codex Plan v3+v4 review pointed
	// out that an OnRemoteAnswer-only hook would miss this path.
	// The goroutine + internal sleep gives Handshaker.Listen time to
	// process the offer (→ reCreateAgent → gather()) before the
	// replay snapshot is taken.
	if conn.workerICE != nil {
		go conn.workerICE.MaybeReplayCandidates(offer.Version)
	}
}

// WgConfig returns the WireGuard config
func (conn *Conn) WgConfig() WgConfig {
	return conn.config.WgConfig
}

// IsConnected returns true if the peer is connected
func (conn *Conn) IsConnected() bool {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	return conn.evalStatus() == StatusConnected
}

// TransportSnapshot returns the current connectivity state of both
// transports (ICE and Relay) as boolean flags indicating "not connected".
// Intended for external watchdogs and telemetry: pure read, no logging,
// no state mutation, no lock acquisition (atomic loads).
//
// Returns (iceDisconnected, relayDisconnected) where each bool is true if
// that transport is NOT in StatusConnected.
func (conn *Conn) TransportSnapshot() (iceDisconnected, relayDisconnected bool) {
	return conn.statusICE.Get() != worker.StatusConnected,
		conn.statusRelay.Get() != worker.StatusConnected
}

// NewConnForTransportTest constructs a minimal *Conn for tests in OTHER
// packages that need a Conn satisfying TransportSnapshot. Production code
// MUST use NewConn — this helper does NOT initialize ICE/relay workers,
// guard, dispatcher, or any of the production fields. It only initializes
// the two atomic status holders + Log so TransportSnapshot reads cleanly.
//
// Exported because Go does not support cross-package test-only exports.
// Callers in lazyconn/manager use this together with peerstore.AddPeerConn
// to build watchdog test fixtures.
func NewConnForTransportTest(log *log.Entry, iceStatus, relayStatus worker.Status) *Conn {
	c := &Conn{
		Log:         log,
		statusICE:   worker.NewAtomicStatus(),
		statusRelay: worker.NewAtomicStatus(),
	}
	if iceStatus == worker.StatusConnected {
		c.statusICE.SetConnected()
	}
	if relayStatus == worker.StatusConnected {
		c.statusRelay.SetConnected()
	}
	return c
}

func (conn *Conn) GetKey() string {
	return conn.config.Key
}

func (conn *Conn) ConnID() id.ConnID {
	return id.ConnID(conn)
}

// configureConnection starts proxying traffic from/to local Wireguard and sets connection status to StatusConnected
func (conn *Conn) onICEConnectionIsReady(priority conntype.ConnPriority, iceConnInfo ICEConnInfo) {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	if conn.ctx.Err() != nil {
		return
	}

	if remoteConnNil(conn.Log, iceConnInfo.RemoteConn) {
		conn.Log.Errorf("remote ICE connection is nil")
		return
	}

	// this never should happen, because Relay is the lower priority and ICE always close the deprecated connection before upgrade
	// todo consider to remove this check
	if conn.currentConnPriority > priority {
		conn.Log.Infof("current connection priority (%s) is higher than the new one (%s), do not upgrade connection", conn.currentConnPriority, priority)
		conn.statusICE.SetConnected()
		conn.updateIceState(iceConnInfo, time.Now())
		return
	}

	conn.Log.Infof("set ICE to active connection")
	conn.dumpState.P2PConnected()

	var (
		ep      *net.UDPAddr
		wgProxy wgproxy.Proxy
		err     error
	)
	if iceConnInfo.RelayedOnLocal {
		conn.dumpState.NewLocalProxy()
		wgProxy, err = conn.newProxy(iceConnInfo.RemoteConn)
		if err != nil {
			conn.Log.Errorf("failed to add turn net.Conn to local proxy: %v", err)
			return
		}
		ep = wgProxy.EndpointAddr()
		conn.wgProxyICE = wgProxy
	} else {
		directEp, err := net.ResolveUDPAddr("udp", iceConnInfo.RemoteConn.RemoteAddr().String())
		if err != nil {
			log.Errorf("failed to resolveUDPaddr")
			conn.handleConfigurationFailure(err, nil)
			return
		}
		ep = directEp
	}

	// Bring the new ICE proxy up FIRST so the destination is ready to
	// receive packets. Then update WG to use it. Only after WG has
	// committed to the new endpoint do we pause the relay -- otherwise
	// there is a 1-2 s window where relay is suspended but WG still
	// points at it, dropping every packet in that window.
	if wgProxy != nil {
		wgProxy.Work()
	}

	conn.Log.Infof("configure WireGuard endpoint to: %s", ep.String())
	updateTime := time.Now()
	conn.enableWgWatcherIfNeeded(updateTime)

	presharedKey := conn.presharedKey(iceConnInfo.RosenpassPubKey)
	if err = conn.endpointUpdater.ConfigureWGEndpoint(ep, presharedKey); err != nil {
		conn.handleConfigurationFailure(err, wgProxy)
		return
	}
	wgConfigWorkaround()

	if conn.wgProxyRelay != nil {
		conn.Log.Debugf("redirect packets from relayed conn to WireGuard")
		conn.wgProxyRelay.RedirectAs(ep)
		// Pause AFTER the redirect is wired up so any in-flight packet
		// from the relay end has a forwarding path while WG converges
		// onto the direct endpoint.
		conn.wgProxyRelay.Pause()
	}

	conn.currentConnPriority = priority
	conn.everConnected.Store(true)
	conn.statusICE.SetConnected()
	conn.updateIceState(iceConnInfo, updateTime)
	conn.doOnConnected(iceConnInfo.RosenpassPubKey, iceConnInfo.RosenpassAddr, updateTime)
}

func (conn *Conn) onICEStateDisconnected(sessionChanged bool) {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	if conn.ctx.Err() != nil {
		return
	}

	conn.Log.Tracef("ICE connection state changed to disconnected")

	if conn.wgProxyICE != nil {
		if err := conn.wgProxyICE.CloseConn(); err != nil {
			conn.Log.Warnf("failed to close deprecated wg proxy conn: %v", err)
		}
	}

	// V18.8 (2026-06-09): in p2p-dynamic mode, treat every ICE-state-
	// disconnect as intentional. The pion-side disconnect (NAT binding
	// expiry, consent freshness failure, idle-peer keep-alive miss)
	// happens silently without runDynamicInactivityLoop's MarkIntention-
	// allyDetached firing — so V14/V18.5 saw marker=false and the guard
	// fired SendOffer ~800 ms after the disconnect, driving the 4-min
	// re-attach cycle even with zero user traffic. Diagnostic build
	// V18.7-diag confirmed every SendOffer originated from onGuardEvent
	// (conn.go:1161) and AttachICEFrom (conn.go:1861) with src=signal,
	// marker=false — neither would have fired with the marker set.
	//
	// In p2p-dynamic mode the design contract is "lazy ICE": when the
	// pair drops we go relay-only until user traffic re-engages us
	// (ICEBind.Send → recordOutbound → AttachICEOnRelayActivity → which
	// clears the marker as clear-point #3). Stamping the marker here
	// makes the lazy semantics work regardless of which side initiated
	// the disconnect. Real pion failures (ConnectionStateFailed) still
	// clear the marker via onICEFailed → clear-point #6, so failure
	// recovery is untouched.
	if conn.config.Mode == connectionmode.ModeP2PDynamic {
		conn.MarkIntentionallyDetached()
	}

	// switch back to relay connection
	if conn.isReadyToUpgrade() {
		conn.Log.Infof("ICE disconnected, set Relay to active connection")
		conn.dumpState.SwitchToRelay()
		if sessionChanged {
			conn.resetEndpoint()
		}

		// todo consider to move after the ConfigureWGEndpoint
		conn.wgProxyRelay.Work()

		presharedKey := conn.presharedKey(conn.rosenpassRemoteKey)
		if err := conn.endpointUpdater.SwitchWGEndpoint(conn.wgProxyRelay.EndpointAddr(), presharedKey); err != nil {
			conn.Log.Errorf("failed to switch to relay conn: %v", err)
		}

		conn.currentConnPriority = conntype.Relay
	} else {
		conn.Log.Infof("ICE disconnected, do not switch to Relay. Reset priority to: %s", conntype.None.String())
		conn.currentConnPriority = conntype.None
		// Intentionally NOT calling RemoveEndpointAddress here: a brief
		// ICE flap (NAT rebind, signal hiccup) is followed within 1-2 s
		// by a fresh ICE-connected callback that re-configures the WG
		// endpoint. Actively removing the endpoint creates a no-endpoint
		// window in which WG drops every packet rather than queuing on
		// a slightly-stale address that the next ConfigureWGEndpoint
		// will replace anyway. If the disconnect is permanent, WG's own
		// keepalive timeout will surface the dead peer.
	}

	changed := conn.statusICE.Get() != worker.StatusDisconnected
	if changed {
		conn.guard.SetICEConnDisconnected()
	}
	conn.statusICE.SetDisconnected()

	conn.disableWgWatcherIfNeeded()

	if conn.currentConnPriority == conntype.None {
		conn.metricsStages.Disconnected()
	}

	peerState := State{
		PubKey:           conn.config.Key,
		ConnStatus:       conn.evalStatus(),
		Relayed:          conn.isRelayed(),
		ConnStatusUpdate: time.Now(),
	}
	if err := conn.statusRecorder.UpdatePeerICEStateToDisconnected(peerState); err != nil {
		conn.Log.Warnf("unable to set peer's state to disconnected ice, got error: %v", err)
	}
}

func (conn *Conn) onRelayConnectionIsReady(rci RelayConnInfo) {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	if conn.ctx.Err() != nil {
		if err := rci.relayedConn.Close(); err != nil {
			conn.Log.Warnf("failed to close unnecessary relayed connection: %v", err)
		}
		return
	}

	conn.dumpState.RelayConnected()
	conn.Log.Debugf("Relay connection has been established, setup the WireGuard")

	wgProxy, err := conn.newProxy(rci.relayedConn)
	if err != nil {
		conn.Log.Errorf("failed to add relayed net.Conn to local proxy: %v", err)
		return
	}
	wgProxy.SetDisconnectListener(conn.onRelayDisconnected)

	conn.dumpState.NewLocalProxy()

	conn.Log.Infof("created new wgProxy for relay connection: %s", wgProxy.EndpointAddr().String())

	if conn.isICEActive() {
		conn.Log.Debugf("do not switch to relay because current priority is: %s", conn.currentConnPriority.String())
		conn.setRelayedProxy(wgProxy)
		conn.statusRelay.SetConnected()
		conn.updateRelayStatus(rci.relayedConn.RemoteAddr().String(), rci.rosenpassPubKey, time.Now())
		return
	}

	controller := isController(conn.config)

	if controller {
		wgProxy.Work()
	}
	updateTime := time.Now()
	conn.enableWgWatcherIfNeeded(updateTime)
	if err := conn.endpointUpdater.ConfigureWGEndpoint(wgProxy.EndpointAddr(), conn.presharedKey(rci.rosenpassPubKey)); err != nil {
		if err := wgProxy.CloseConn(); err != nil {
			conn.Log.Warnf("Failed to close relay connection: %v", err)
		}
		conn.Log.Errorf("Failed to update WireGuard peer configuration: %v", err)
		return
	}
	if !controller {
		wgProxy.Work()
	}

	wgConfigWorkaround()

	conn.rosenpassRemoteKey = rci.rosenpassPubKey
	conn.currentConnPriority = conntype.Relay
	conn.everConnected.Store(true)
	conn.statusRelay.SetConnected()
	conn.setRelayedProxy(wgProxy)
	conn.updateRelayStatus(rci.relayedConn.RemoteAddr().String(), rci.rosenpassPubKey, updateTime)
	conn.Log.Infof("start to communicate with peer via relay")
	conn.doOnConnected(rci.rosenpassPubKey, rci.rosenpassAddr, updateTime)
}

func (conn *Conn) onRelayDisconnected() {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.handleRelayDisconnectedLocked()
}

// handleRelayDisconnectedLocked handles relay disconnection. Caller must hold conn.mu.
func (conn *Conn) handleRelayDisconnectedLocked() {
	if conn.ctx.Err() != nil {
		return
	}

	conn.Log.Debugf("relay connection is disconnected")

	if conn.currentConnPriority == conntype.Relay {
		conn.Log.Debugf("clean up WireGuard config")
		conn.currentConnPriority = conntype.None
		if err := conn.config.WgConfig.WgInterface.RemoveEndpointAddress(conn.config.WgConfig.RemoteKey); err != nil {
			conn.Log.Errorf("failed to remove wg endpoint: %v", err)
		}
	}

	if conn.wgProxyRelay != nil {
		_ = conn.wgProxyRelay.CloseConn()
		conn.wgProxyRelay = nil
	}

	changed := conn.statusRelay.Get() != worker.StatusDisconnected
	if changed {
		conn.guard.SetRelayedConnDisconnected()
	}
	conn.statusRelay.SetDisconnected()

	conn.disableWgWatcherIfNeeded()

	if conn.currentConnPriority == conntype.None {
		conn.metricsStages.Disconnected()
	}

	peerState := State{
		PubKey:           conn.config.Key,
		ConnStatus:       conn.evalStatus(),
		Relayed:          conn.isRelayed(),
		ConnStatusUpdate: time.Now(),
	}
	if err := conn.statusRecorder.UpdatePeerRelayedStateToDisconnected(peerState); err != nil {
		conn.Log.Warnf("unable to save peer's state to Relay disconnected, got error: %v", err)
	}

	// Phase 3.7k stuck-state recovery: when the relay drops while the
	// ICE worker is already in intentional-detach mode (remote/local
	// GO_IDLE), the peer has no active path AND no activity listener
	// armed. The Phase 3.7j guard would correctly skip offers under
	// `intentionallyDetached=true` (assuming an activity edge will
	// re-attach), but no such edge can fire while lazyconn has no
	// fake-endpoint bind for this peer.
	//
	// Symptom (production-reproduced on dk20 against 80AFCAB57262 on
	// 2026-05-27): peer stuck in "Status: Connecting, relay=Disconnected,
	// ice=Disconnected" forever; guard logs "skip offer (ICE detached
	// for inactivity, p2p-dynamic; will re-attach on real traffic)"
	// every ~45s but no real traffic ever traverses the WG layer to
	// trigger lazyconn. Manual `systemctl restart netbird` clears the
	// state.
	//
	// Recovery: invoke the same callback the WG-handshake-timeout path
	// uses (ConnMgr.RecoverPeerToIdle), which pushes the peer back into
	// the lazy manager's activity-listening idle state. The next
	// outbound packet then arms ICE via the lazyconn activity edge.
	//
	// Gated on IsIntentionallyDetached so normal mid-session relay
	// drops (where the guard / ICE-state-disconnect path already
	// handles recovery) remain untouched.
	if conn.IsIntentionallyDetached() && conn.handshaker != nil && conn.handshaker.readICEListener() == nil {
		cb := conn.onWGTimeoutRecover
		if cb != nil {
			conn.Log.Infof("relay disconnect while ICE intentionally-detached: pushing peer back to lazy-idle (activity listener will rearm)")
			go cb()
		}
	}
}

// RemoteEffectiveMode is the public accessor used by ConnMgr.ActivatePeer
// to gate signal-driven wake-ups against peers the server resolved to
// p2p-lazy. Delegates to remoteEffectiveMode.
func (conn *Conn) RemoteEffectiveMode() connectionmode.Mode {
	return conn.remoteEffectiveMode()
}

// remoteEffectiveMode returns the connection mode the management server
// has resolved per-peer for the REMOTE peer (RemotePeerConfig.
// effective_connection_mode). For peers covered by the server's
// LegacyLazyFallback this is p2p-lazy even when the account-wide mode
// is p2p-dynamic. ModeUnspecified means the status recorder does not
// know the mode yet (early bootstrap before first NetworkMap, or status
// recorder missing).
func (conn *Conn) remoteEffectiveMode() connectionmode.Mode {
	if conn.statusRecorder == nil {
		return connectionmode.ModeUnspecified
	}
	state, err := conn.statusRecorder.GetPeer(conn.config.Key)
	if err != nil {
		return connectionmode.ModeUnspecified
	}
	m, _ := connectionmode.ParseString(state.RemoteEffectiveConnectionMode)
	return m
}

// shouldSkipBootstrapOffer mirrors the gate at the top of onGuardEvent:
// the guard must suppress the bootstrap offer when the remote peer is
// resolved to p2p-lazy OR p2p-dynamic AND this Conn has never connected.
//
// For peers that WERE connected and then lost their relay/ICE path
// (network change, signal/relay reconnect, daemon resume from standby)
// the guard MUST still send recovery offers — the everConnected
// short-circuit below guarantees that — otherwise the tunnel stays cold
// forever (see the 2026-05-17 S26 stuck-peer incident).
//
// Phase 3.7k+ (2026-05-27): p2p-dynamic now also gates the BOOTSTRAP
// offer. Rationale from the user-confirmed semantics:
//
//	p2p          = eager: connect to every peer at startup, stay on
//	p2p-lazy     = strict: never connect until traffic
//	p2p-dynamic  = lazy on bootstrap + active while traffic flows +
//	               teardown on idle  <-- THIS is the change
//
// Previously p2p-dynamic fired a bootstrap offer per peer on the first
// guard tick after startup, which signalled every remote out of its
// lazy-idle state (ConnMgr.ActivatePeer on the receiving side) and
// produced the "all peers go P2P immediately on app connect" burst —
// dozens of ICE establishments with zero user traffic.
//
// With the gate, a freshly-connected p2p-dynamic client establishes a
// peer connection ONLY when there is real traffic, from either side:
//   - local user traffic  -> lazy-bind activity-listener fires ->
//     lazyconn.Manager.onPeerActivity -> AttachICE + SendOffer
//     (independent of this gate; see manager.onPeerActivity)
//   - remote user traffic  -> remote's own activity edge makes it send
//     us an offer -> engine signal-receive -> ConnMgr.ActivatePeer ->
//     Open + AttachICE
//
// No bootstrap deadlock: the gate only suppresses the unsolicited
// periodic guard tick. The two traffic-driven paths above remain fully
// functional, and whichever side first sees traffic breaks the symmetry.
//
// Eager mode (ModeP2P) and relay-forced are unaffected — explicit
// always-on opt-ins.
//
// Extracted into a method (Phase-3.7i v0.5) so the gate logic can be
// behaviourally unit-tested without driving through the full
// Handshaker + Signaler call chain.
func (conn *Conn) shouldSkipBootstrapOffer() bool {
	if conn.everConnected.Load() {
		return false
	}
	// REVERT 2026-06-02: 30s-grace-bypass entfernt — war zu aggressiv,
	// brach lazy-mode für ganze Account-Fleet. User-Report: nach S26-App-
	// Start sofort 13 P2P-Verbindungen aufgebaut (statt erst bei Traffic).
	//
	// Original Phase-37k Verhalten (f433f1b42) wiederhergestellt:
	// p2p-lazy/p2p-dynamic peers bleiben idle BIS local-activity ODER
	// remote-signal echte Activity zeigt. Das war by-design lazy-mode.
	switch conn.remoteEffectiveMode() {
	case connectionmode.ModeP2PLazy, connectionmode.ModeP2PDynamic:
		return true
	default:
		return false
	}
}

func (conn *Conn) onGuardEvent() {
	// V18.5 (2026-06-09): hard skip when the local conn is intentionally
	// detached (= inactivity-timer drove DetachICEForPeer). The guard's
	// purpose is recovery from network/relay/signal disruption; an
	// intentional lazy detach is none of those. Without this skip the
	// guard fires SendOffer within ~800 ms of the ICE-state-disconnect
	// callback (production W11/S26: every 4-min idle → guard wakes peer
	// → re-attach loop). The lazy-mode "wait for user traffic" path
	// re-engages via ICEBind.Send → recordOutbound → AttachICEOnRelay-
	// Activity (V18.4); the guard does not need to fire.
	//
	// Eager modes (p2p, relay-forced) and p2p-dynamic peers that lost
	// connectivity (intentionallyDetached=false) keep the always-on
	// recovery behaviour. Wake paths from user-side outbound traffic
	// clear the marker via AttachICE / AttachICEUserInitiated /
	// AttachICEOnRelayActivity (see clear-point list at conn.go:230).
	if conn.config.Mode == connectionmode.ModeP2PDynamic && conn.IsIntentionallyDetached() {
		conn.Log.Tracef("guard: skip offer (intentionally detached, p2p-dynamic lazy mode)")
		conn.logDiagSnapshot("guard-skip-intentionally-detached")
		return
	}

	// Respect remote peer's resolved connection mode: when the management
	// server has placed the REMOTE peer in p2p-lazy (typical for legacy
	// clients covered by LegacyLazyFallback even though the account-wide
	// mode is p2p-dynamic), it expects strict lazy semantics — i.e. no
	// unsolicited initial offers from us. Skipping here prevents the
	// eager initial P2P establishment to dozens of legacy peers that
	// the user never actually communicates with.
	//
	// CRITICAL gate on everConnected: skip ONLY on the BOOTSTRAP case
	// (peer never connected yet). For peers that WERE connected and
	// then lost their relay/ICE path (network change, signal/relay
	// reconnect, daemon resume from standby), the guard MUST still
	// send recovery OFFERs — otherwise the tunnel stays cold forever
	// because (a) the local activity-listener is inactive for already-
	// opened conns and (b) the legacy remote won't re-initiate on its
	// own either. Discovered 2026-05-17: S26 had 23 idle / 5 offline
	// after overnight standby; relay reconnected at 06:18 but every
	// guard fire was silently skipped here, leaving every legacy peer
	// unable to recover.
	//
	// Note: we still respect remote-initiated OFFERs via the signal-
	// receive path (engine.go -> ConnMgr.ActivatePeer is NOT gated on
	// this), and we still bootstrap when local user traffic triggers
	// the local lazy manager (manager.onPeerActivity -> AttachICE).
	if conn.shouldSkipBootstrapOffer() {
		conn.Log.Tracef("guard: skip offer (remote peer is p2p-lazy/p2p-dynamic AND never connected; wait for remote OFFER or local activity)")
		conn.logDiagSnapshot("guard-skip-bootstrap-offer")
		return
	}

	// Suppress reconnect-offers under p2p-dynamic when the management
	// server reports the remote peer as offline (live_online=false). The
	// guard otherwise spams an offer every 5-30 s for up to relay_timeout
	// minutes after the remote disappeared, and each offer that survives
	// (when the remote reconnects) immediately wakes the lazy manager on
	// the remote side -- defeating the user-visible "idle until traffic"
	// promise of p2p-dynamic. Eager modes (p2p, relay-forced) keep the
	// always-on behaviour because that's what those modes are for.
	if conn.config.Mode == connectionmode.ModeP2PDynamic {
		if state, err := conn.statusRecorder.GetPeer(conn.config.Key); err == nil {
			if state.RemoteServerLivenessKnown && !state.RemoteLiveOnline {
				// REVERT 2026-06-02: 30s-grace-bypass entfernt.
				// Original Verhalten wiederhergestellt: wenn mgmt-server sagt
				// remote offline, KEIN Bootstrap-Offer — lazy-mode-konform.
				conn.Log.Tracef("guard: skip offer (remote peer offline, p2p-dynamic)")
				conn.logDiagSnapshot("guard-skip-remote-offline")
				return
			}
		}
		// Codex hardening audit: also skip when the guard is firing
		// for "PartiallyConnected" (relay up, ICE detached) AND the
		// detach was due to ICE-inactivity (the dynamic inactivity
		// manager called DetachICEForPeer because no payload traffic
		// for iceTimeout). Re-firing offers in that state wastes
		// signal traffic and can wake the remote's lazy manager just
		// to re-attach ICE that we'll detach again on the next idle
		// cycle. The next REAL outbound packet on this peer will go
		// through ConnMgr.ActivatePeer -> conn.AttachICE which DOES
		// respect iceBackoff and is the correct path to re-engage ICE.
		//
		// Detection requires THREE conditions:
		//   1. ICE worker exists but is detached (no listener),
		//   2. no recorded ICE-failure-backoff (else the existing
		//      3-tries-then-hourly retry policy handles it),
		//   3. this Conn has been connected at least ONCE before (the
		//      everConnected flag). Without #3 we'd skip the very
		//      first bootstrap offer for a brand-new peer because
		//      its ICE listener is also nil before initial setup —
		//      regression caught during 6-host hardware test on
		//      4998e5a58.
		if conn.everConnected.Load() &&
			conn.handshaker != nil && conn.handshaker.readICEListener() == nil {
			if state, err := conn.statusRecorder.GetPeer(conn.config.Key); err == nil {
				if !state.IceBackoffSuspended && state.IceBackoffFailures == 0 {
					conn.Log.Tracef("guard: skip offer (ICE detached for inactivity, p2p-dynamic; will re-attach on real traffic)")
					conn.logDiagSnapshot("guard-skip-ice-detached-inactivity")
					return
				}
			}
		}
	}
	conn.dumpState.SendOffer()
	if err := conn.handshaker.SendOffer(); err != nil {
		conn.Log.Errorf("failed to send offer: %v", err)
	}
}

func (conn *Conn) onWGDisconnected() {
	conn.mu.Lock()

	if conn.ctx.Err() != nil {
		conn.mu.Unlock()
		return
	}

	conn.Log.Warnf("WireGuard handshake timeout detected, closing current connection")

	// Close the active connection based on current priority
	switch conn.currentConnPriority {
	case conntype.Relay:
		if conn.workerRelay != nil {
			conn.workerRelay.CloseConn()
		}
		conn.handleRelayDisconnectedLocked()
	case conntype.ICEP2P, conntype.ICETurn:
		if conn.workerICE != nil {
			conn.workerICE.Close()
		}

		// Phase 3.7i (#5989): pion's ICE state-change handler does not
		// always re-program the WG endpoint after a handshake timeout —
		// observed in the field as a peer staying configured with the
		// stale direct public IP (e.g. 64.52.21.35:55699) while WG
		// silently drops every outgoing packet. Explicitly redirect the
		// endpoint to the still-up relay proxy so traffic keeps flowing
		// while ICE renegotiates from scratch.
		//
		// Also marks markFailure on the backoff: a WG-handshake-timeout
		// after pion reported ICE Connected IS a real connection
		// failure that should count toward the long-retry schedule.
		conn.switchEndpointToRelayLocked()

		if conn.iceBackoff != nil {
			delay := conn.iceBackoff.markFailure()
			snap := conn.iceBackoff.Snapshot()
			if delay > 0 {
				conn.Log.Infof("WG-handshake-timeout counted as ICE failure #%d, suspending for %s, next retry at %s",
					snap.Failures,
					delay.Round(time.Second),
					snap.NextRetry.Format("15:04:05"))
			}
			if conn.statusRecorder != nil {
				conn.statusRecorder.UpdatePeerIceBackoff(conn.config.Key, snap)
			}
			conn.logDiagSnapshot("markFailure-wg-handshake-timeout")
		}
	default:
		conn.Log.Debugf("No active connection to close on WG timeout")
	}

	// Capture the callback before releasing the lock; we invoke it in a
	// goroutine because it routes back into ConnMgr -> lazyConnMgr ->
	// peerStore.PeerConnClose -> Conn.Close, which needs conn.mu (we
	// hold it). Spawning a goroutine is fine — onWGDisconnected is itself
	// fired from the WG-watcher goroutine, no caller waits on the result.
	cb := conn.onWGTimeoutRecover
	conn.mu.Unlock()

	if cb != nil {
		go cb()
	}
}

// switchEndpointToRelayLocked redirects the WG endpoint back to the
// already-running relay proxy when an ICE-priority connection has just
// been torn down by the WG-handshake watchdog. Caller MUST hold conn.mu.
//
// No-op when no relay proxy is up — that path leaves recovery to the
// pion ICE state-change handler / Guard reconnect-loop.
//
// Mirrors the relay-fallback half of onICEStateDisconnected (where
// isReadyToUpgrade() is true) but without the sessionChanged / status
// bookkeeping that path needs for an ICE-graceful close.
func (conn *Conn) switchEndpointToRelayLocked() {
	if conn.wgProxyRelay == nil {
		conn.Log.Debugf("WG-handshake-timeout on ICE: no relay proxy up, leaving recovery to pion")
		return
	}
	if conn.currentConnPriority == conntype.Relay {
		// Already on relay (e.g. a concurrent onICEStateDisconnected
		// already swapped). Nothing to do.
		return
	}

	conn.Log.Infof("WG-handshake-timeout on ICE - explicit fallback to relay endpoint %s", conn.wgProxyRelay.EndpointAddr().String())
	conn.wgProxyRelay.Work()

	presharedKey := conn.presharedKey(conn.rosenpassRemoteKey)
	if err := conn.endpointUpdater.SwitchWGEndpoint(conn.wgProxyRelay.EndpointAddr(), presharedKey); err != nil {
		conn.Log.Errorf("WG-handshake-timeout fallback: SwitchWGEndpoint to relay failed: %v", err)
		return
	}

	conn.currentConnPriority = conntype.Relay
}

func (conn *Conn) updateRelayStatus(relayServerAddr string, rosenpassPubKey []byte, updateTime time.Time) {
	peerState := State{
		PubKey:             conn.config.Key,
		ConnStatusUpdate:   updateTime,
		ConnStatus:         conn.evalStatus(),
		Relayed:            conn.isRelayed(),
		RelayServerAddress: relayServerAddr,
		RosenpassEnabled:   isRosenpassEnabled(rosenpassPubKey),
	}

	err := conn.statusRecorder.UpdatePeerRelayedState(peerState)
	if err != nil {
		conn.Log.Warnf("unable to save peer's Relay state, got error: %v", err)
	}
}

func (conn *Conn) updateIceState(iceConnInfo ICEConnInfo, updateTime time.Time) {
	peerState := State{
		PubKey:                     conn.config.Key,
		ConnStatusUpdate:           updateTime,
		ConnStatus:                 conn.evalStatus(),
		Relayed:                    iceConnInfo.Relayed,
		LocalIceCandidateType:      iceConnInfo.LocalIceCandidateType,
		RemoteIceCandidateType:     iceConnInfo.RemoteIceCandidateType,
		LocalIceCandidateEndpoint:  iceConnInfo.LocalIceCandidateEndpoint,
		RemoteIceCandidateEndpoint: iceConnInfo.RemoteIceCandidateEndpoint,
		RosenpassEnabled:           isRosenpassEnabled(iceConnInfo.RosenpassPubKey),
	}

	err := conn.statusRecorder.UpdatePeerICEState(peerState)
	if err != nil {
		conn.Log.Warnf("unable to save peer's ICE state, got error: %v", err)
	}
}

func (conn *Conn) setStatusToDisconnected() {
	conn.statusRelay.SetDisconnected()
	conn.statusICE.SetDisconnected()
	conn.currentConnPriority = conntype.None

	peerState := State{
		PubKey:           conn.config.Key,
		ConnStatus:       StatusIdle,
		ConnStatusUpdate: time.Now(),
		Mux:              new(sync.RWMutex),
	}
	err := conn.statusRecorder.UpdatePeerState(peerState)
	if err != nil {
		// pretty common error because by that time Engine can already remove the peer and status won't be available.
		// todo rethink status updates
		conn.Log.Debugf("error while updating peer's state, err: %v", err)
	}
	if err := conn.statusRecorder.UpdateWireGuardPeerState(conn.config.Key, configurer.WGStats{}); err != nil {
		conn.Log.Debugf("failed to reset wireguard stats for peer: %s", err)
	}
}

func (conn *Conn) doOnConnected(remoteRosenpassPubKey []byte, remoteRosenpassAddr string, updateTime time.Time) {
	if runtime.GOOS == "ios" {
		runtime.GC()
	}

	conn.metricsStages.RecordConnectionReady(updateTime)

	if conn.onConnected != nil {
		conn.onConnected(conn.config.Key, remoteRosenpassPubKey, conn.config.WgConfig.AllowedIps[0].Addr().String(), remoteRosenpassAddr)
	}
}

func (conn *Conn) isRelayed() bool {
	switch conn.currentConnPriority {
	case conntype.Relay, conntype.ICETurn:
		return true
	default:
		return false
	}
}

func (conn *Conn) evalStatus() ConnStatus {
	if conn.statusRelay.Get() == worker.StatusConnected || conn.statusICE.Get() == worker.StatusConnected {
		return StatusConnected
	}

	return StatusConnecting
}

// isConnectedOnAllWay evaluates the overall connection status based on ICE and Relay transports.
//
// The result is a tri-state:
//   - ConnStatusConnected:          all available transports are up
//   - ConnStatusPartiallyConnected: relay is up but ICE is still pending/reconnecting
//   - ConnStatusDisconnected:       no working transport
func (conn *Conn) isConnectedOnAllWay() (status guard.ConnStatus) {
	defer func() {
		if status == guard.ConnStatusDisconnected {
			conn.logTraceConnState()
		}
	}()

	iceWorkerCreated := conn.workerICE != nil

	var iceInProgress bool
	if iceWorkerCreated {
		iceInProgress = conn.workerICE.InProgress()
	}

	return evalConnStatus(connStatusInputs{
		forceRelay:          conn.config.Mode == connectionmode.ModeRelayForced,
		peerUsesRelay:       conn.workerRelay.IsRelayConnectionSupportedWithPeer(),
		relayConnected:      conn.statusRelay.Get() == worker.StatusConnected,
		remoteSupportsICE:   conn.handshaker.RemoteICESupported(),
		iceWorkerCreated:    iceWorkerCreated,
		iceStatusConnecting: conn.statusICE.Get() != worker.StatusDisconnected,
		iceInProgress:       iceInProgress,
	})
}

func (conn *Conn) enableWgWatcherIfNeeded(enabledTime time.Time) {
	if !conn.wgWatcher.IsEnabled() {
		wgWatcherCtx, wgWatcherCancel := context.WithCancel(conn.ctx)
		conn.wgWatcherCancel = wgWatcherCancel
		conn.wgWatcherWg.Add(1)
		go func() {
			defer conn.wgWatcherWg.Done()
			conn.wgWatcher.EnableWgWatcher(wgWatcherCtx, enabledTime, conn.onWGDisconnected, conn.onWGHandshakeSuccess)
		}()
	}
}

func (conn *Conn) disableWgWatcherIfNeeded() {
	if conn.currentConnPriority == conntype.None && conn.wgWatcherCancel != nil {
		conn.wgWatcherCancel()
		conn.wgWatcherCancel = nil
	}
}

func (conn *Conn) newProxy(remoteConn net.Conn) (wgproxy.Proxy, error) {
	conn.Log.Debugf("setup proxied WireGuard connection")
	udpAddr := &net.UDPAddr{
		IP:   conn.config.WgConfig.AllowedIps[0].Addr().AsSlice(),
		Port: conn.config.WgConfig.WgListenPort,
	}

	wgProxy := conn.config.WgConfig.WgInterface.GetProxy()
	if err := wgProxy.AddTurnConn(conn.ctx, udpAddr, remoteConn); err != nil {
		conn.Log.Errorf("failed to add turn net.Conn to local proxy: %v", err)
		return nil, err
	}
	return wgProxy, nil
}

func (conn *Conn) resetEndpoint() {
	if !isController(conn.config) {
		return
	}
	conn.Log.Infof("reset wg endpoint")
	conn.wgWatcher.Reset()
	if err := conn.endpointUpdater.RemoveEndpointAddress(); err != nil {
		conn.Log.Warnf("failed to remove endpoint address before update: %v", err)
	}
}

func (conn *Conn) isReadyToUpgrade() bool {
	return conn.wgProxyRelay != nil && conn.currentConnPriority != conntype.Relay
}

func (conn *Conn) isICEActive() bool {
	return (conn.currentConnPriority == conntype.ICEP2P || conn.currentConnPriority == conntype.ICETurn) && conn.statusICE.Get() == worker.StatusConnected
}

func (conn *Conn) handleConfigurationFailure(err error, wgProxy wgproxy.Proxy) {
	conn.Log.Warnf("Failed to update wg peer configuration: %v", err)
	if wgProxy != nil {
		if ierr := wgProxy.CloseConn(); ierr != nil {
			conn.Log.Warnf("Failed to close wg proxy: %v", ierr)
		}
	}
	if conn.wgProxyRelay != nil {
		conn.wgProxyRelay.Work()
	}
}

func (conn *Conn) logTraceConnState() {
	if conn.workerRelay.IsRelayConnectionSupportedWithPeer() {
		conn.Log.Tracef("connectivity guard check, relay state: %s, ice state: %s", conn.statusRelay, conn.statusICE)
	} else {
		conn.Log.Tracef("connectivity guard check, ice state: %s", conn.statusICE)
	}
}

func (conn *Conn) setRelayedProxy(proxy wgproxy.Proxy) {
	if conn.wgProxyRelay != nil {
		if err := conn.wgProxyRelay.CloseConn(); err != nil {
			conn.Log.Warnf("failed to close deprecated wg proxy conn: %v", err)
		}
	}
	conn.wgProxyRelay = proxy
}

// onWGHandshakeSuccess is called when the first WireGuard handshake is detected
func (conn *Conn) onWGHandshakeSuccess(when time.Time) {
	conn.metricsStages.RecordWGHandshakeSuccess(when)
	conn.recordConnectionMetrics()
}

// recordConnectionMetrics records connection stage timestamps as metrics
func (conn *Conn) recordConnectionMetrics() {
	if conn.metricsRecorder == nil {
		return
	}

	// Determine connection type based on current priority
	conn.mu.Lock()
	priority := conn.currentConnPriority
	conn.mu.Unlock()

	var connType metrics.ConnectionType
	switch priority {
	case conntype.Relay:
		connType = metrics.ConnectionTypeRelay
	default:
		connType = metrics.ConnectionTypeICE
	}

	// Record metrics with timestamps - duration calculation happens in metrics package
	conn.metricsRecorder.RecordConnectionStages(
		context.Background(),
		conn.config.Key,
		connType,
		conn.metricsStages.IsReconnection(),
		conn.metricsStages.GetTimestamps(),
	)
}

// AllowedIP returns the allowed IP of the remote peer
func (conn *Conn) AllowedIP() netip.Addr {
	return conn.config.WgConfig.AllowedIps[0].Addr()
}

func (conn *Conn) AgentVersionString() string {
	return conn.config.AgentVersion
}

func (conn *Conn) presharedKey(remoteRosenpassKey []byte) *wgtypes.Key {
	if conn.config.RosenpassConfig.PubKey == nil {
		return conn.config.WgConfig.PreSharedKey
	}

	if remoteRosenpassKey == nil && conn.config.RosenpassConfig.PermissiveMode {
		return conn.config.WgConfig.PreSharedKey
	}

	// If Rosenpass has already set a PSK for this peer, return nil to prevent
	// UpdatePeer from overwriting the Rosenpass-managed key.
	if conn.rosenpassInitializedPresharedKeyValidator != nil && conn.rosenpassInitializedPresharedKeyValidator(conn.config.Key) {
		return nil
	}

	// Use NetBird PSK as the seed for Rosenpass. This same PSK is passed to
	// Rosenpass as PeerConfig.PresharedKey, ensuring the derived post-quantum
	// key is cryptographically bound to the original secret.
	if conn.config.WgConfig.PreSharedKey != nil {
		return conn.config.WgConfig.PreSharedKey
	}

	// Fallback to deterministic key if no NetBird PSK is configured
	determKey, err := conn.rosenpassDetermKey()
	if err != nil {
		conn.Log.Errorf("failed to generate Rosenpass initial key: %v", err)
		return nil
	}

	return determKey
}

// todo: move this logic into Rosenpass package
func (conn *Conn) rosenpassDetermKey() (*wgtypes.Key, error) {
	lk := []byte(conn.config.LocalKey)
	rk := []byte(conn.config.Key) // remote key
	var keyInput []byte
	if string(lk) > string(rk) {
		//nolint:gocritic
		keyInput = append(lk[:16], rk[:16]...)
	} else {
		//nolint:gocritic
		keyInput = append(rk[:16], lk[:16]...)
	}

	key, err := wgtypes.NewKey(keyInput)
	if err != nil {
		return nil, err
	}
	return &key, nil
}

func isController(config ConnConfig) bool {
	return config.LocalKey > config.Key
}

func isRosenpassEnabled(remoteRosenpassPubKey []byte) bool {
	return remoteRosenpassPubKey != nil
}

func evalConnStatus(in connStatusInputs) guard.ConnStatus {
	// "Relay up and needed" — the peer uses relay and the transport is connected.
	relayUsedAndUp := in.peerUsesRelay && in.relayConnected

	// Force-relay mode: ICE never runs. Relay is the only transport and must be up.
	if in.forceRelay {
		return boolToConnStatus(relayUsedAndUp)
	}

	// Remote peer doesn't support ICE, or we haven't created the worker yet:
	// relay is the only possible transport.
	if !in.remoteSupportsICE || !in.iceWorkerCreated {
		return boolToConnStatus(relayUsedAndUp)
	}

	// ICE counts as "up" when the status is anything other than Disconnected, OR
	// when a negotiation is currently in progress (so we don't spam offers while one is in flight).
	iceUp := in.iceStatusConnecting || in.iceInProgress

	// Relay side is acceptable if the peer doesn't rely on relay, or relay is connected.
	relayOK := !in.peerUsesRelay || in.relayConnected

	switch {
	case iceUp && relayOK:
		return guard.ConnStatusConnected
	case relayUsedAndUp:
		// Relay is up but ICE is down — partially connected.
		return guard.ConnStatusPartiallyConnected
	default:
		return guard.ConnStatusDisconnected
	}
}

func boolToConnStatus(connected bool) guard.ConnStatus {
	if connected {
		return guard.ConnStatusConnected
	}
	return guard.ConnStatusDisconnected
}

// AttachICEOnRelayActivity is the relay-state fast-path triggered by
// ActivityRecorder when transport activity (>32-byte type-4 WG packet)
// is observed for a peer that's currently sitting in Relayed state
// (ICE worker detached on iceTimeout). Encapsulates Codex review-point-
// 4 gating so the engine doesn't have to peek into Conn internals:
//
//  1. mode must be p2p-dynamic (other modes have no detached state)
//  2. conn must be open (not yet closed by relay-timeout)
//  3. currentConnPriority must be Relay (we're using the relay tunnel)
//  4. handshaker.iceListener must be nil (ICE actually detached)
//  5. iceBackoff: by default skipped while suspended, BUT a rate-
//     limited override applies (iceBackoff.AllowActivityOverride —
//     one bypass per activityOverrideMinInterval=5min per peer).
//     Codex review 2026-05-05 point 5: real user activity is the
//     strongest "I want this peer back" signal, so a single override
//     per 5min trades a bounded extra offer/answer pair for unsticking
//     legitimately working peers that hit a transient ICE drop.
//  6. everConnected must be true (we had P2P at least once -- avoids
//     pointless retries for peers we never reached P2P with)
//
// Returns true when AttachICE was actually called (caller can rate-
// limit further). The lazy-mgr.onPeerActivity path uses
// ResetIceBackoff (unconditional reset) because there the trigger is
// "user wants the peer back after full Idle" — that signal is even
// stronger than relay-state activity, so the stronger reset is OK.
//
// Phase 3.7i (#5989), Codex review 2026-05-05.
func (conn *Conn) AttachICEOnRelayActivity() (attempted bool) {
	// V18.11 (2026-06-09): post-detach cooldown. Bail when the most
	// recent MarkIntentionallyDetached call was within the cooldown
	// window. Spurious outbound traffic that fires in the first ~30 s
	// after a pion-side disconnect (Android system mDNS, DNS retry
	// bursts, OS-generated TCP RSTs to inbound legacy keep-alives,
	// internal NetBird route-manager pings, etc.) cannot wake P2P in
	// this window. Sustained user activity outlasting 30 s — or any
	// activity after the cooldown elapses — still wakes the peer
	// normally. Production S26 V18.10 trace: 13 re-attach cycles in
	// 30 min with V18.10 gate (signal-channel) firing only 3 times;
	// the other 10 came through this path from non-user outbound.
	if conn.config.Mode == connectionmode.ModeP2PDynamic &&
		conn.IsIntentionallyDetached() {
		detachedAtNs := conn.intentionallyDetachedAt.Load()
		if detachedAtNs > 0 {
			since := monotime.Since(monotime.Time(detachedAtNs))
			if since < v18_11RelayActivityCooldown {
				conn.Log.Tracef("V18.11 cooldown: skipping AttachICEOnRelayActivity (detached %v ago, cooldown %v)",
					since, v18_11RelayActivityCooldown)
				conn.logDiagSnapshot("AttachICEOnRelayActivity-blocked-cooldown")
				return false
			}
		}
	}

	// V13.1 RETIRED by V18.1 (2026-06-06): the original V13.1 gate
	// (block relay-activity ICE-recovery when IsLazyDetached()) was a
	// no-op from 2026-06-03 to V18 because the listener-armed predicate
	// was never wired to a real source — IsLazyDetached returned false
	// in the inactivity-timeout sub-state, so V13.1 never fired. V18
	// (HandleICEInactivityTransition state-sync) closed that gap → V13.1
	// suddenly became active → blocked every legitimate user-traffic-
	// driven Relay-to-P2P upgrade (production W11 → Marl Creek: after
	// 4-min idle, Relay-only, but user traffic over Relay would no
	// longer re-attach ICE).
	//
	// Decision: drop V13.1 entirely. V14 (conn_mgr.go ActivatePeer-
	// ForMessage) and V15 (cold-boot OFFER) gate signal-channel spam
	// from legacy peers; that is the primary defence. Transport-level
	// relay-data is rarer than signal-OFFER spam, and conflating it
	// with "intentional spam" forbids the legitimate "user is
	// transferring data" path that the user spec explicitly requires
	// ("wenn Datentransfer stattfindet, soll P2P wiederhergestellt
	// werden"). If legacy keep-alives later prove to wake P2P too
	// aggressively, the right gate is a LastActivities-based check
	// for local outbound traffic within a window (Phase-3.7j Fix-C
	// pattern, see conn_mgr.go DeactivatePeer), NOT a coarse
	// IsLazyDetached return.
	// V18.9 (2026-06-09): moved ClearIntentionallyDetached from before
	// the priority/mode checks to AFTER. The earlier unconditional clear
	// at function entry created a race window where every relay-activity
	// edge — including the bursts that fire during the ICE-disconnect →
	// relay-fallback transition (when currentConnPriority is still ICE
	// or None), or from spurious outbound traffic that the V18.4 filter
	// can't classify as user-initiated (e.g. OS-generated responses to
	// inbound packets) — cleared the marker WITHOUT actually re-engaging
	// ICE. Combined with V18.8 (mark on every pion-disconnect), this
	// silently undid V18.8's gate within seconds and left the cycle
	// intact (W11 production V18.8 test: cycle reduced but not gone,
	// 8-14 s detach→re-attach intervals interleaved with longer ones).
	//
	// Phase 3.7j: clear the intentional-detach marker — Relay activity
	// is an unambiguous signal that the local stack is re-engaging ICE.
	// Cleared only after we confirm the call will proceed to re-engage.
	conn.mu.Lock()
	if conn.config.Mode != connectionmode.ModeP2PDynamic {
		conn.logDiagSnapshot("AttachICEOnRelayActivity-blocked-mode-not-p2p-dynamic")
		conn.mu.Unlock()
		return false
	}
	if !conn.opened {
		conn.logDiagSnapshot("AttachICEOnRelayActivity-blocked-not-opened")
		conn.mu.Unlock()
		return false
	}
	if conn.currentConnPriority != conntype.Relay {
		conn.logDiagSnapshot("AttachICEOnRelayActivity-blocked-priority-not-relay")
		conn.mu.Unlock()
		return false
	}
	if conn.handshaker == nil {
		conn.logDiagSnapshot("AttachICEOnRelayActivity-blocked-no-handshaker")
		conn.mu.Unlock()
		return false
	}
	// Phase 3.7k+ (Fix B for stale-listener-gate): when the previous ICE
	// agent ended in Failed/Disconnected/Closed without the listener being
	// detached, the old `handshaker.readICEListener() != nil` blanket-reject
	// left the peer on Relay until the p2p-dynamic idle-teardown (~3 min)
	// finally cleared the listener. With WorkerICE.IsRetrySafe() we can
	// distinguish "ICE actively working" from "ICE stale-attached" and
	// release the stale state immediately on relay-activity so the user
	// gets a P2P upgrade attempt without the 3-minute wait.
	//
	// Healthy / mid-connect ICE is still NOT disturbed:
	//   - agentConnecting=true       -> would race in-flight connect
	//   - lastKnownState=Connected   -> already P2P, no need to retry
	//   - lastKnownState=Checking/New -> agent making progress
	staleListener := false
	if listener := conn.handshaker.readICEListener(); listener != nil {
		if conn.workerICE == nil || !conn.workerICE.IsRetrySafe() {
			conn.logDiagSnapshot("AttachICEOnRelayActivity-blocked-listener-not-retry-safe")
			conn.mu.Unlock()
			return false
		}
		staleListener = true
	}
	if conn.iceBackoff != nil && conn.iceBackoff.IsSuspended() {
		// Phase 3.7i (#5989), Codex review point 5 follow-up: activity-
		// driven override of an active failure backoff. Rate-limited
		// inside iceBackoff.AllowActivityOverride to one override per
		// 5min per peer, so we never spam the signal server. Without
		// this, a transient ICE drop on a flaky link (e.g. LTE NAT
		// mapping recovery > 12s while the Guard's 3-fast-retries
		// timer fires) leaves the peer permanently relay-only for an
		// hour even when the user actively pings.
		if conn.iceBackoff.AllowActivityOverride() {
			conn.iceBackoff.Reset()
			if conn.statusRecorder != nil {
				conn.statusRecorder.UpdatePeerIceBackoff(conn.config.Key, conn.iceBackoff.Snapshot())
			}
			conn.Log.Infof("ICE backoff override on relay-activity (1x per %s rate limit)", "5min")
			conn.logDiagSnapshot("AttachICEOnRelayActivity-backoff-override-allowed")
		} else {
			conn.logDiagSnapshot("AttachICEOnRelayActivity-blocked-backoff-override-cooldown")
			conn.mu.Unlock()
			return false
		}
	}
	if !conn.everConnected.Load() {
		conn.logDiagSnapshot("AttachICEOnRelayActivity-blocked-never-connected")
		conn.mu.Unlock()
		return false
	}
	// All gates passed; release the lock before calling AttachICE
	// because AttachICE re-acquires it.
	conn.mu.Unlock()
	// Fix B: if a stale listener was attached (ICE in Failed/Disconnected/
	// Closed state), clear it first so AttachICE -> attachICEListenerLocked
	// will install a fresh listener and trigger SendOffer. DetachICE is
	// idempotent if there is nothing to clear, so the call is safe even if
	// the state changed between the check and here.
	if staleListener {
		if err := conn.DetachICE(); err != nil {
			conn.Log.Warnf("DetachICE on stale-listener relay-activity retry: %v", err)
			return false
		}
		conn.Log.Debugf("relay-activity: cleared stale ICE listener before re-attach")
	}
	// Fix-D D1.1: source-labeled — this is the relay-activity recovery
	// path (D2a-fed via ICEBind.Send activity recorder).
	if err := conn.AttachICEFrom(AttachICESourceRelayActivity); err != nil {
		conn.Log.Warnf("AttachICE on relay-activity: %v", err)
		return false
	}
	// Phase 3.7i (#5989), Codex review 2026-05-05: also reset the
	// guard's per-cycle ICE retry budget so the new pair-check cycle
	// is not immediately throttled into hourly mode by 3 stale
	// failures. The iceBackoff override above only handles the
	// failure-suspension side; the guard runs a parallel 3-tries-then-
	// hourly counter that is independent of iceBackoff.
	if conn.guard != nil {
		conn.guard.NotifyPeerActivity()
	}
	conn.Log.Debugf("ICE re-attached on relay-activity (relay -> P2P upgrade attempt)")
	conn.logDiagSnapshot("AttachICEOnRelayActivity-success-ice-reattached")
	return true
}

// NotifyGuardActivity forwards a peer-activity event to the underlying
// guard so it resets its per-cycle ICE retry budget and ticker. Safe
// to call even when the guard hasn't been created yet (returns
// silently). Phase 3.7i (#5989), Codex review 2026-05-05.
func (conn *Conn) NotifyGuardActivity() {
	conn.mu.Lock()
	g := conn.guard
	conn.mu.Unlock()
	if g != nil {
		g.NotifyPeerActivity()
	}
}

// ResetIceBackoff hard-resets the per-peer ICE-failure backoff state
// (failure counter back to 0, suspended -> false, exponential schedule
// back to its initial interval, lastResetAt stamped). Intended for the
// lazy-mgr activity-trigger path: a transient ICE failure (e.g.
// concurrent wake-up race) otherwise enters "3 retries exhausted ->
// hourly retry" mode (guard/ice_retry_state.go:52) and the next
// legitimate activity sees AttachICE early-return on
// iceBackoff.IsSuspended() -> peer permanently stuck on relay. Called
// from lazyconn manager.onPeerActivity before AttachICE so real user
// traffic always gets a fresh ICE attempt. The signal-trigger path
// does NOT reset (it deliberately respects the failure backoff).
func (conn *Conn) ResetIceBackoff() {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.iceBackoff == nil {
		return
	}
	conn.iceBackoff.Reset()
	if conn.statusRecorder != nil {
		// Codex review 2026-05-05 follow-up: keep status output (CLI
		// `netbird status -d`, daemon RPC) in sync with the cleared
		// backoff state so it doesn't continue to advertise a stale
		// "suspended" / "Failures=N" snapshot after the reset.
		conn.statusRecorder.UpdatePeerIceBackoff(conn.config.Key, conn.iceBackoff.Snapshot())
	}
}

// AttachICE registers the ICE-offer listener on the handshaker after the
// activity-detector observes traffic on the relay tunnel. Idempotent: if
// the listener is already attached, it is a no-op. Triggers a fresh offer
// so the remote side learns we are now ICE-capable.
//
// Used by p2p-dynamic mode: workerICE is created in Open() but the
// handshaker dispatch is deferred until traffic activity is seen.
//
// Backward-compatible wrapper around AttachICEFrom. New call sites
// SHOULD use AttachICEFrom with an explicit source label so the
// blocked-backoff DIAG marker can distinguish guard-driven, signal-
// driven, lazy-activity and relay-activity retry storms.
func (conn *Conn) AttachICE() error {
	return conn.AttachICEFrom(AttachICESourceUnknown)
}

// AttachICEFrom is the source-labeled variant of AttachICE. The source
// is recorded in the blocked-backoff DIAG marker for offline analysis
// (Fix-D D1.1, Codex 2026-05-29).
func (conn *Conn) AttachICEFrom(src AttachICESource) error {
	// V18.10 (2026-06-09): signal-driven re-attach must NOT proceed when
	// the conn is intentionally detached in p2p-dynamic mode. V14 gate
	// in ActivatePeerForMessage upstream is supposed to prevent this,
	// but production traces (W11 V18.8-V18.9) show signal-driven
	// AttachICEFrom (src=Signal) still firing during the relay-fallback
	// transition — the marker that V18.8 stamped on onICEStateDisconnected
	// gets cleared by an earlier clear-point before V14 sees it. By
	// bailing here for src=Signal we keep the lazy gate strict even if
	// V14's check raced the marker. User-driven wakes (RelayActivity,
	// LazyActivity) are NOT gated — they are the legitimate path to
	// re-engage P2P on real outbound traffic.
	if conn.config.Mode == connectionmode.ModeP2PDynamic &&
		src == AttachICESourceSignal &&
		conn.IsIntentionallyDetached() {
		conn.Log.Tracef("V18.10 gate: skipping signal-driven AttachICE while intentionally detached (p2p-dynamic lazy)")
		conn.logDiagSnapshot("AttachICEFrom-blocked-signal-intentionally-detached")
		return nil
	}
	// Phase 3.7j: clear the intentional-detach marker here (clear-point #1).
	// Signal-driven ICE re-attach is the explicit counterpart to an
	// intentional detach; once we re-attach the marker must not linger
	// across the next pion lifecycle.
	conn.ClearIntentionallyDetached()
	conn.mu.Lock()
	defer conn.mu.Unlock()

	if conn.iceBackoff != nil && conn.iceBackoff.IsSuspended() {
		snap := conn.iceBackoff.Snapshot()
		conn.Log.Debugf("ICE backoff active (failure #%d, retry at %s), staying on relay",
			snap.Failures,
			snap.NextRetry.Format("15:04:05"))
		conn.logDiagSnapshotDedup("AttachICE-blocked-backoff-suspended-source-"+src.String(), diagDedupWindow)
		return nil
	}
	if conn.handshaker == nil {
		return fmt.Errorf("AttachICE: handshaker not initialized (Open not called)")
	}
	if conn.workerICE == nil {
		return fmt.Errorf("AttachICE: workerICE is nil (relay-forced mode)")
	}

	if !conn.attachICEListenerLocked() {
		return nil
	}

	if err := conn.handshaker.SendOffer(); err != nil {
		conn.Log.Warnf("AttachICE: SendOffer failed: %v", err)
	}
	return nil
}

// AttachICEUserInitiated is the activity-driven variant of AttachICE used
// by the lazyconn manager when an actual local Write hits the lazy fake-IP
// endpoint (= the user is generating traffic).
//
// Behaviour vs AttachICE:
//   - Backoff not suspended → identical to AttachICE.
//   - Backoff suspended AND last user-initiated bypass < minCooldown ago →
//     no-op (rate-limit) so repeated re-Writes do not spam fresh ICE
//     attempts inside a single failure window.
//   - Backoff suspended AND outside cooldown → bypass the gate ONCE via
//     iceBackoff.markUserInitiatedRetry, log the failure-count, and run
//     the normal attach/SendOffer path. failures counter and exponential
//     schedule are preserved so a still-broken peer eventually falls back
//     onto the long suspend.
//
// Phase 3.7i (#5989): the original hourly-retry behaviour parks a peer on
// relay for ~1 h even while the user is actively generating traffic. This
// path lets user activity drive at most one fresh ICE attempt per cooldown
// without short-circuiting the failure schedule entirely.
func (conn *Conn) AttachICEUserInitiated(minCooldown time.Duration) error {
	// Phase 3.7j: clear the intentional-detach marker here (clear-point #2).
	// Local user-traffic-driven wake-up is the activity counterpart to
	// AttachICE; clearing here keeps the marker semantics symmetric
	// across all three Attach-paths.
	conn.ClearIntentionallyDetached()
	conn.mu.Lock()
	defer conn.mu.Unlock()

	// Run the user-initiated bypass/cooldown logic BEFORE the nil-checks so
	// that a hard-relay-forced caller path (workerICE==nil, handshaker==nil)
	// does not accidentally short-circuit the gate accounting. The gate is
	// the load-bearing piece of Fix #3; the nil-checks are guard-rails that
	// only matter once we are about to actually attach.
	bypassed := false
	if conn.iceBackoff != nil && conn.iceBackoff.IsSuspended() {
		now := time.Now()
		if !conn.lastUserInitiatedAttachICE.IsZero() && now.Sub(conn.lastUserInitiatedAttachICE) < minCooldown {
			snap := conn.iceBackoff.Snapshot()
			conn.Log.Debugf("user-initiated AttachICE rate-limited (last attempt %s ago, cooldown %s, backoff failure #%d)",
				now.Sub(conn.lastUserInitiatedAttachICE).Round(time.Second),
				minCooldown,
				snap.Failures)
			// Codex review-polish 2026-05-29: emit a source-labeled
			// DIAG marker on the user-initiated cooldown-block so the
			// AttachICESourceUserInitiated enum value is observable in
			// offline analysis (previously this path had no [DIAG]).
			conn.logDiagSnapshotDedup("AttachICEUserInitiated-blocked-cooldown-source-"+AttachICESourceUserInitiated.String(), diagDedupWindow)
			return nil
		}
		// Outside cooldown: take a single bypass slot.
		if conn.iceBackoff.markUserInitiatedRetry() {
			snap := conn.iceBackoff.Snapshot()
			conn.Log.Infof("ICE backoff active (failure #%d), but user-initiated activity - attempting fresh ICE",
				snap.Failures)
			conn.lastUserInitiatedAttachICE = now
			bypassed = true
		}
	}

	if conn.handshaker == nil {
		if bypassed {
			conn.Log.Debugf("AttachICEUserInitiated: bypass succeeded but handshaker not initialized; deferring to next signal-driven attempt")
		}
		return fmt.Errorf("AttachICEUserInitiated: handshaker not initialized (Open not called)")
	}
	if conn.workerICE == nil {
		if bypassed {
			conn.Log.Debugf("AttachICEUserInitiated: bypass succeeded but workerICE is nil; deferring to next signal-driven attempt")
		}
		return fmt.Errorf("AttachICEUserInitiated: workerICE is nil (relay-forced mode)")
	}

	if !conn.attachICEListenerLocked() {
		return nil
	}

	if err := conn.handshaker.SendOffer(); err != nil {
		conn.Log.Warnf("AttachICEUserInitiated: SendOffer failed: %v", err)
	}
	return nil
}

// AttachICEOnRemoteOffer is the Fix-D D3b path: when a remote OFFER
// arrives via signal and the local backoff is currently suspended, allow
// ONE schedule-preserving ICE retry per minCooldown window. The intuition:
// a remote-side OFFER is itself a "the other side wants to talk to you"
// signal that's at least as strong as a local user-traffic edge, and the
// chronic ICE failures that drove the backoff into suspension may have
// nothing to do with that remote's current network state — give the
// recovery one shot.
//
// Risk-control vs. the more permissive AttachICEUserInitiated:
//   - mode MUST be p2p-dynamic (D2a/D2b paths are scoped here too)
//   - handshaker.iceListener MUST be nil (no in-flight ICE to disturb)
//   - rate-limited by minCooldown per Conn so an offer-storm from a
//     buggy/legacy remote does not drain the backoff bypass slot every
//     few seconds
//   - schedule-preserving: uses markUserInitiatedRetry, never Reset(),
//     so the long-term exponential schedule keeps growing for genuinely
//     unreachable peers
//
// Codex D3b recommendation (2026-05-29).
func (conn *Conn) AttachICEOnRemoteOffer(minCooldown time.Duration) error {
	// V18.10 (2026-06-09): symmetric with AttachICEFrom gate. Remote
	// OFFER is a signal-channel trigger — in p2p-dynamic lazy mode the
	// remote should not be able to wake us by re-OFFERing while we are
	// intentionally detached. User-driven wakes (RelayActivity / Lazy-
	// Activity) clear the marker through the standard clear-points and
	// then a subsequent legitimate signal cycle resumes naturally.
	if conn.config.Mode == connectionmode.ModeP2PDynamic &&
		conn.IsIntentionallyDetached() {
		conn.Log.Tracef("V18.10 gate: skipping AttachICEOnRemoteOffer while intentionally detached (p2p-dynamic lazy)")
		conn.logDiagSnapshot("AttachICEOnRemoteOffer-blocked-intentionally-detached")
		return nil
	}
	// Symmetric with the other Attach-paths: clear the intentional-detach
	// marker since a remote OFFER is an explicit re-engage signal.
	conn.ClearIntentionallyDetached()
	conn.mu.Lock()
	defer conn.mu.Unlock()

	if conn.config.Mode != connectionmode.ModeP2PDynamic {
		// Not our mode, no bypass intended. Defer to the standard
		// signal-driven AttachICE path.
		return conn.attachICEFromLocked(AttachICESourceRemoteOffer)
	}
	if conn.handshaker == nil {
		return fmt.Errorf("AttachICEOnRemoteOffer: handshaker not initialized (Open not called)")
	}
	if conn.workerICE == nil {
		// Relay-forced mode: nothing to attach.
		return fmt.Errorf("AttachICEOnRemoteOffer: workerICE is nil (relay-forced mode)")
	}

	// Gate 1: don't disturb in-flight ICE.
	if conn.handshaker.readICEListener() != nil {
		conn.logDiagSnapshotDedup("AttachICEOnRemoteOffer-blocked-listener-attached", diagDedupWindow)
		return nil
	}

	// Gate 2: only useful when backoff is currently suspended. If it
	// isn't, the standard signal-driven AttachICE path can run without
	// any bypass.
	if conn.iceBackoff == nil || !conn.iceBackoff.IsSuspended() {
		return conn.attachICEFromLocked(AttachICESourceRemoteOffer)
	}

	// Gate 3: rate-limit one bypass per minCooldown window.
	now := time.Now()
	if !conn.lastRemoteOfferAttach.IsZero() && now.Sub(conn.lastRemoteOfferAttach) < minCooldown {
		conn.logDiagSnapshotDedup("AttachICEOnRemoteOffer-blocked-cooldown", diagDedupWindow)
		return nil
	}

	// Take the bypass: schedule-preserving (NOT Reset). markUserInitiated-
	// Retry returns false if the backoff is no longer actually suspended
	// (e.g. expired naturally between IsSuspended()==true above and now);
	// in that case fall through to the normal attach path without
	// consuming the cooldown slot.
	if !conn.iceBackoff.markUserInitiatedRetry() {
		return conn.attachICEFromLocked(AttachICESourceRemoteOffer)
	}
	conn.lastRemoteOfferAttach = now
	conn.Log.Infof("ICE backoff active but remote OFFER triggers user-initiated bypass (schedule preserved)")
	conn.logDiagSnapshot("AttachICEOnRemoteOffer-backoff-bypass-allowed")

	if !conn.attachICEListenerLocked() {
		return nil
	}
	if err := conn.handshaker.SendOffer(); err != nil {
		conn.Log.Warnf("AttachICEOnRemoteOffer: SendOffer failed: %v", err)
	}
	return nil
}

// attachICEFromLocked is the locked-state core used by AttachICEOnRemoteOffer
// to fall through to the normal attach path while the caller already holds
// conn.mu. It mirrors the body of AttachICEFrom MINUS the
// ClearIntentionallyDetached / mu.Lock prelude (already done by the caller).
//
// Returns nil on success (including the no-op cases: listener already
// attached, backoff suspended without bypass).
func (conn *Conn) attachICEFromLocked(src AttachICESource) error {
	if conn.iceBackoff != nil && conn.iceBackoff.IsSuspended() {
		snap := conn.iceBackoff.Snapshot()
		conn.Log.Debugf("ICE backoff active (failure #%d, retry at %s), staying on relay",
			snap.Failures,
			snap.NextRetry.Format("15:04:05"))
		conn.logDiagSnapshotDedup("AttachICE-blocked-backoff-suspended-source-"+src.String(), diagDedupWindow)
		return nil
	}
	if conn.handshaker == nil {
		return fmt.Errorf("AttachICEFromLocked: handshaker not initialized")
	}
	if conn.workerICE == nil {
		return fmt.Errorf("AttachICEFromLocked: workerICE is nil")
	}
	if !conn.attachICEListenerLocked() {
		return nil
	}
	if err := conn.handshaker.SendOffer(); err != nil {
		conn.Log.Warnf("attachICEFromLocked: SendOffer failed: %v", err)
	}
	return nil
}

// attachICEListenerLocked attaches the ICE listener to the handshaker if it
// is not already attached. Returns true when a new attachment was made,
// false when the call was a no-op (already attached, ICE backoff suspended,
// handshaker not initialised, or workerICE not present).
//
// Caller MUST hold conn.mu. Used by:
//   - AttachICE (signal-trigger path), which then issues SendOffer.
//   - onNetworkChange (Phase 3.7e, #5989), which deliberately does NOT call
//     SendOffer because the Guard reconnect-loop handles that.
//
// Honours iceBackoff.IsSuspended() so the failure-backoff is not bypassed.
func (conn *Conn) attachICEListenerLocked() bool {
	if conn.iceBackoff != nil && conn.iceBackoff.IsSuspended() {
		snap := conn.iceBackoff.Snapshot()
		conn.Log.Debugf("ICE backoff active (failure #%d, retry at %s), staying on relay",
			snap.Failures,
			snap.NextRetry.Format("15:04:05"))
		return false
	}
	if conn.handshaker == nil || conn.workerICE == nil {
		return false
	}
	if conn.handshaker.readICEListener() != nil {
		return false
	}

	conn.handshaker.AddICEListener(conn.workerICE.OnNewOffer)
	conn.Log.Debugf("ICE listener attached (locked path)")
	return true
}

// DetachICE removes the ICE-offer listener and tears down the ICE worker.
// Idempotent: if no listener is attached, it is a no-op. Used by
// p2p-dynamic mode when the inactivity manager fires the iceTimeout but
// the relay tunnel should stay up.
func (conn *Conn) DetachICE() error {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	if conn.handshaker == nil {
		return nil
	}
	if conn.handshaker.readICEListener() == nil {
		return nil
	}

	conn.handshaker.RemoveICEListener()
	if conn.workerICE != nil {
		conn.workerICE.Close()
	}
	conn.Log.Debugf("ICE listener detached (p2p-dynamic teardown)")
	return nil
}

// onICEFailed is invoked when pion's ICE agent reports
// ConnectionStateFailed. Increments the backoff counter and tears
// down the ICE worker. Phase 3 of #5989.
//
// Backoff sources are intentionally narrow (Codex review 2026-05-05):
// only Pion's ConnectionStateFailed counts as a "failure" worth
// pushing the exponential schedule forward. Inactivity-driven detach
// (DetachICEForPeer via ICEInactiveChan) and full-conn close (lazy-mgr
// relayTimeout) bypass markFailure entirely. So the backoff exclusively
// measures "ICE pair-checks broke after a real attempt", never
// "no traffic flowed for a while".
func (conn *Conn) onICEFailed() {
	// Phase 3.7j: clear the intentional-detach marker here (clear-point #6,
	// non-optional). A real pion failure immediately after an intentional
	// detach must remain visible to the guard; otherwise the marker would
	// continue to suppress failure-handling and the peer would never
	// escalate to the legitimate fail+backoff path. Done unconditionally
	// at the top so even the "iceBackoff == nil" early-return path still
	// surfaces the real failure to the guard predicate.
	conn.ClearIntentionallyDetached()
	if conn.iceBackoff == nil {
		return
	}
	// Distinguish failure types in the log so future debugging can
	// tell apart "first-attempt couldn't pair" from "established P2P
	// silently dropped" from "re-attach after detach failed". The
	// classification is best-effort -- pion only tells us "Failed";
	// we infer from local state.
	failType := "first-attempt"
	isPostSuccessDrop := false
	switch {
	case conn.everConnected.Load():
		failType = "post-success-drop"
		isPostSuccessDrop = true
	case conn.handshaker != nil && conn.handshaker.readICEListener() != nil:
		failType = "re-attach"
	}

	// V16 (2026-06-04): post-success-drops on stateful NAT (Halifax-CGNAT)
	// must not feed the exponential curve — see ice_backoff.go for the
	// rationale. first-attempt and re-attach keep the original schedule.
	var delay time.Duration
	if isPostSuccessDrop {
		delay = conn.iceBackoff.markFailurePostSuccessDrop()
	} else {
		delay = conn.iceBackoff.markFailure()
	}
	snap := conn.iceBackoff.Snapshot()
	if delay > 0 {
		conn.Log.Infof("ICE failure #%d (%s), suspending for %s, next retry at %s",
			snap.Failures,
			failType,
			delay.Round(time.Second),
			snap.NextRetry.Format("15:04:05"))
	}
	if conn.statusRecorder != nil {
		conn.statusRecorder.UpdatePeerIceBackoff(conn.config.Key, snap)
	}

	// Phase 3.7l Fix-D Phase 1: record this ICE failure against the
	// current local srflx so snapshotForDiagnosis can surface "N
	// consecutive failures with identical srflx port" in the DIAG line.
	// Pure observation — no recovery action in Phase 1. workerICE may
	// be nil in relay-forced mode; LastLocalSrflx is then the zero
	// AddrPort, which the same-srflx logic treats as "still no public
	// port observed" (itself a diagnostic signal).
	if conn.workerICE != nil {
		conn.srflxState.observeFailure(conn.workerICE.LastLocalSrflx(), time.Now())
	}

	conn.logDiagSnapshot("markFailure-on-ice-failed-" + failType)
	// Tear down ICE. Idempotent. Conn stays on relay.
	if err := conn.DetachICE(); err != nil {
		conn.Log.Warnf("DetachICE after onICEFailed: %v", err)
	}
}

// onICEConnected is invoked when pion's ICE agent reports
// ConnectionStateConnected. Resets the backoff. Phase 3 of #5989.
func (conn *Conn) onICEConnected() {
	if conn.iceBackoff == nil {
		return
	}
	if conn.iceBackoff.Snapshot().Failures > 0 {
		conn.Log.Infof("ICE success, resetting backoff (was %d failures)",
			conn.iceBackoff.Snapshot().Failures)
	}
	conn.iceBackoff.markSuccess()
	if conn.statusRecorder != nil {
		conn.statusRecorder.UpdatePeerIceBackoff(conn.config.Key, conn.iceBackoff.Snapshot())
	}

	// Phase 3.7l Fix-D Phase 1: reset the stuck-srflx counter on every
	// real ICE-Connected transition. A subsequent failure starts a
	// fresh streak against the new local srflx.
	if conn.workerICE != nil {
		conn.srflxState.observeSuccess(conn.workerICE.LastLocalSrflx(), time.Now())
	}
	// Codex review-polish 2026-05-29: clear the D3b remote-offer
	// cooldown stamp on ICE-success. The cooldown exists to rate-limit
	// bypass spam during chronic-failure windows; once ICE actually
	// connects, the previous bypass-burn is done and a future ICE-drop
	// (e.g. network change a minute later) should be allowed to bypass
	// immediately rather than waiting out a stale 60 s window.
	//
	// Protected by conn.mu because lastRemoteOfferAttach is mutated
	// under the same lock from AttachICEOnRemoteOffer. Use a short
	// critical section to avoid contention with the WG transport hot
	// path that may be unwinding the previous Relay session.
	conn.mu.Lock()
	conn.lastRemoteOfferAttach = time.Time{}
	conn.mu.Unlock()
}

// initIceBackoffFromConfig (re-)initializes conn.iceBackoff from
// conn.config.P2pRetryMaxSeconds via the canonical wire-format
// translation in ResolveP2pRetryCap. Called from Open() on a fresh
// peer; extracted so the call site can be unit-tested in isolation.
//
// Callers must hold conn.mu (Open holds it implicitly via its
// caller).
func (conn *Conn) initIceBackoffFromConfig() {
	cap := ResolveP2pRetryCap(conn.config.P2pRetryMaxSeconds)
	if conn.iceBackoff == nil {
		conn.iceBackoff = newIceBackoff(cap)
	} else {
		conn.iceBackoff.SetMaxBackoff(cap)
	}
}

// SetIceBackoffMax updates the per-peer backoff cap. Called by ConnMgr
// when the server pushes a new p2p_retry_max_seconds value. If the
// iceBackoff is not yet initialized (Conn not opened yet), the value
// is stored in config so Open() picks it up. Phase 3 of #5989.
func (conn *Conn) SetIceBackoffMax(d time.Duration) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.config.P2pRetryMaxSeconds = uint32(d / time.Second)
	if conn.iceBackoff != nil {
		conn.iceBackoff.SetMaxBackoff(d)
	}
}

// IceBackoffSnapshot exposes the read-only backoff state for the
// status output (Task E1). Returns zero-value snapshot if no backoff
// is active. Phase 3 of #5989.
func (conn *Conn) IceBackoffSnapshot() BackoffSnapshot {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.iceBackoff == nil {
		return BackoffSnapshot{}
	}
	return conn.iceBackoff.Snapshot()
}

// onNetworkChange is invoked by Guard when the signal/relay layer
// reconnects after a network change (LTE-modem replug, WiFi roaming, etc.).
// Phase 3.5 of #5989.
//
// Resets the per-peer ICE-failure backoff (because the NAT topology may
// have changed -- previous failures do not predict future ones) AND
// recreates the workerICE wrapper so the next AttachICE/offer has a
// fresh pion-agent rather than one closed by a previous DetachICE call.
//
// Called from Guard's goroutine; acquires conn.mu, so it must not be
// invoked from a path that already holds conn.mu.
func (conn *Conn) onNetworkChange() {
	// V14.1 (2026-06-03): gate the SR-watcher / network-change re-attach for
	// peers that the lazy manager just put to sleep. onNetworkChange fires
	// on EVERY srReconnect — including spurious Signal-gRPC-stream timeouts
	// (TCP keepalive, server-side stream rotation), which are not real
	// network events. The default behaviour re-attaches ICE for every peer
	// in batch, which after a 3-min p2p-dynamic teardown reads the user as
	// "P2P is back up" without the user ever sending traffic — defeating
	// lazy mode. Observed on S21 with 11 peers: a 30-s grpc stream blip
	// re-armed all 11 ICE listeners simultaneously, every 3 min, in a
	// stable cycle.
	//
	// We skip ONLY when the conn is currently intentionally-detached AND
	// has ever-connected. That combination means the lazy manager
	// deliberately left ICE down because no real WG traffic crossed the
	// peer for p2pTimeoutSecs. A real network event (LTE replug, WiFi
	// roam) on a peer with active traffic would have cleared the marker
	// earlier (AttachICEFrom on previous activity), so this gate fires
	// only on the legacy-spam-recovery path, not on legitimate roam
	// recovery for actively-used peers.
	//
	// User-outbound traffic still wakes the peer via lazyconn/manager
	// .onPeerActivity (separate path, not affected by this gate).
	if conn.IsLazyDetached() {
		conn.Log.Tracef("V14.1 gate: skipping onNetworkChange re-attach (intentionally-detached + everConnected, lazy-mode preserved)")
		return
	}
	// Phase 3.7j: clear the intentional-detach marker here (clear-point #5,
	// SR-watcher reconnect). A network event (LTE replug, WiFi roam)
	// invalidates any previous "intentionally idle" reasoning -- the
	// path may be entirely different now -- and the Guard about to
	// drive a fresh ICE cycle must see a clean slate. Done before
	// acquiring conn.mu because ClearIntentionallyDetached is atomic
	// and lock-free; keeps ordering trivial.
	conn.ClearIntentionallyDetached()
	conn.mu.Lock()
	defer conn.mu.Unlock()

	if conn.ctx.Err() != nil {
		return
	}

	if conn.iceBackoff != nil {
		snap := conn.iceBackoff.Snapshot()
		if snap.Failures > 0 {
			conn.Log.Infof("network change detected, resetting ICE backoff (was %d failures)",
				snap.Failures)
		}
		conn.iceBackoff.Reset()
		if conn.statusRecorder != nil {
			conn.statusRecorder.UpdatePeerIceBackoff(conn.config.Key, conn.iceBackoff.Snapshot())
		}
	}

	// We deliberately do NOT replace the workerICE wrapper here. Replacing
	// it leaks underlying socket/iface bindings between the old and new
	// instance, which empirically causes ICE to fail with a 13s pair-check
	// timeout instead of converging in <1s like a fresh daemon-start does.
	//
	// We also deliberately do NOT call handshaker.SendOffer() here even
	// though that was an earlier attempt. The Guard's reconnect-loop
	// already issues sendOffer via its newReconnectTicker (800ms initial,
	// up to ~4 retries in the first ~6s) right after the same srReconnect
	// event that fires this callback. Adding our own SendOffer just creates
	// a sending-offer storm: 5 offers per peer in 6 seconds, which on the
	// remote side triggers repeated tear-down + reCreateAgent cycles in
	// quick succession (each new sessionID forces it). That prevents ICE
	// from ever completing its pair-checks.
	//
	// All we do here: close the current pion agent (sets w.agent = nil).
	// The Guard's natural reconnect-loop then drives the next sendOffer,
	// the remote responds with a fresh offer, and our existing OnNewOffer
	// path (still attached to the unchanged workerICE wrapper) goes
	// through the well-tested "agent==nil + new offer -> reCreateAgent"
	// branch in worker_ice.go.
	//
	// Phase 3.7g (#5989): only tear down the workerICE agent when ICE is
	// actually broken. If pion's lastKnownState is still Connected the
	// peer-to-peer UDP path is alive end-to-end (typical for a brief
	// signal-server outage where WG keepalives between peers continued
	// to flow); closing the agent here would force a 15-25 s ICE
	// renegotiation cycle plus a Relay→ICE handover gap that the user
	// would observe as a ping dropout for no good reason.
	//
	// If ICE actually went Disconnected/Failed during the network event,
	// pion has already cleared w.agent via onConnectionStateChange and
	// the Close call below is a no-op anyway. Either way, a fresh remote
	// OFFER will recreate the agent through the existing OnNewOffer path.
	//
	// In ModeRelayForced workerICE is nil; nothing to close.
	if conn.workerICE != nil && !conn.workerICE.IsConnected() {
		conn.workerICE.Close()
	} else if conn.workerICE != nil {
		conn.Log.Debugf("network change: skipping workerICE.Close (ICE still Connected, soft-fallback)")
	}

	// Phase 3.7e (#5989): force the ICE listener back on after a network
	// change. Empirically, after an LTE-modem replug the iceListener can
	// end up detached for some peers (paths via onICEFailed → DetachICE
	// after a Failed transition that we did not log because of timing,
	// or via concurrent state changes during the bounce). Re-attaching
	// on every signal in ConnMgr.ActivatePeer (Phase 3.7d) is necessary
	// but not sufficient: by the time the next signal arrives, several
	// remote OFFERs and the Guard's first sendOffer may already have
	// been silently dropped at handshaker.Listen() because no listener
	// was present. Re-attaching here closes that window deterministically.
	//
	// We do NOT call SendOffer from this path. The Guard's natural
	// reconnect-ticker (newReconnectTicker, 800 ms initial) issues the
	// next offer right after the same srReconnect event that drove this
	// callback; sending an extra one creates the offer-storm that
	// Phase 3.7b removed.
	conn.attachICEListenerLocked()

	conn.Log.Debugf("ICE state reset on network change (agent closed; listener re-armed; Guard will resend offer)")
}
