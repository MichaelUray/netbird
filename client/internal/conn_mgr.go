package internal

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/client/internal/lazyconn"
	"github.com/netbirdio/netbird/client/internal/lazyconn/manager"
	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/client/internal/peerstore"
	"github.com/netbirdio/netbird/route"
	"github.com/netbirdio/netbird/shared/connectionmode"
	mgmProto "github.com/netbirdio/netbird/shared/management/proto"
)

// ConnMgr coordinates both lazy connections (established on-demand) and permanent peer connections.
//
// The connection manager is responsible for:
// - Managing lazy connections via the lazyConnManager
// - Maintaining a list of excluded peers that should always have permanent connections
// - Handling connection establishment based on peer signaling
//
// The implementation is not thread-safe; it is protected by engine.syncMsgMux.
type ConnMgr struct {
	peerStore        *peerstore.Store
	statusRecorder   *peer.Status
	iface            lazyconn.WGIface
	rosenpassEnabled bool

	// Resolved values used to drive lifecycle decisions. Updated when
	// the management server pushes a new PeerConfig.
	mode             connectionmode.Mode
	relayTimeoutSecs uint32
	// Phase 2 (#5989): ICE-only inactivity timeout (seconds). Used in
	// ModeP2PDynamic to teardown the ICE worker without affecting the
	// relay tunnel. 0 = ICE never times out.
	p2pTimeoutSecs uint32
	// Phase 3 (#5989): maximum seconds between P2P retry attempts.
	// 0 means the daemon uses its built-in default.
	p2pRetryMaxSecs uint32

	// Raw inputs kept so we can re-resolve when server-pushed value changes.
	envMode         connectionmode.Mode
	envRelayTimeout uint32
	cfgMode         connectionmode.Mode
	cfgRelayTimeout uint32
	cfgP2pTimeout   uint32
	cfgP2pRetryMax  uint32

	// spMu protects all serverPushed* fields below. Written in
	// UpdatedRemotePeerConfig (NetworkMap goroutine), read by
	// ServerPushed*() accessors (daemon-RPC GetConfig goroutine).
	spMu sync.RWMutex

	// serverPushedMode is the ConnectionMode value that was last received
	// from the management server's PeerConfig (independent of any local
	// env/cfg override). Updated in UpdatedRemotePeerConfig. Used by the
	// Android UI to display "Follow server (currently: <mode>)" in the
	// connection-mode override dropdown so users can see what they would
	// inherit if they leave the override on "Follow server".
	serverPushedMode             connectionmode.Mode
	serverPushedRelayTimeoutSecs uint32
	serverPushedP2pTimeoutSecs   uint32
	serverPushedP2pRetryMaxSecs  uint32

	lazyConnMgr *manager.Manager

	wg            sync.WaitGroup
	lazyCtx       context.Context
	lazyCtxCancel context.CancelFunc
}

func NewConnMgr(engineConfig *EngineConfig, statusRecorder *peer.Status, peerStore *peerstore.Store, iface lazyconn.WGIface) *ConnMgr {
	envMode, envRelayTimeout := peer.ResolveModeFromEnv()

	// First-pass resolution without server input -- updated later when
	// the first NetworkMap arrives via UpdatedRemotePeerConfig.
	mode, relayTimeout, p2pTimeout, p2pRetryMax := resolveConnectionMode(
		envMode, envRelayTimeout,
		engineConfig.ConnectionMode, engineConfig.RelayTimeoutSeconds,
		engineConfig.P2pTimeoutSeconds,
		engineConfig.P2pRetryMaxSeconds,
		nil,
	)

	return &ConnMgr{
		peerStore:        peerStore,
		statusRecorder:   statusRecorder,
		iface:            iface,
		rosenpassEnabled: engineConfig.RosenpassEnabled,
		mode:             mode,
		relayTimeoutSecs: relayTimeout,
		p2pTimeoutSecs:   p2pTimeout,
		p2pRetryMaxSecs:  p2pRetryMax,
		envMode:          envMode,
		envRelayTimeout:  envRelayTimeout,
		cfgMode:          engineConfig.ConnectionMode,
		cfgRelayTimeout:  engineConfig.RelayTimeoutSeconds,
		cfgP2pTimeout:    engineConfig.P2pTimeoutSeconds,
		cfgP2pRetryMax:   engineConfig.P2pRetryMaxSeconds,
	}
}

// resolveConnectionMode applies the spec-section-4.1 precedence chain:
//  1. client env (already resolved by caller via peer.ResolveModeFromEnv)
//  2. client config (from profile, including the FollowServer sentinel)
//  3. server-pushed PeerConfig.ConnectionMode (with UNSPECIFIED ->
//     legacy LazyConnectionEnabled fallback)
//
// Returns the resolved Mode, the resolved relay-timeout in seconds, and
// the resolved p2p-timeout in seconds. 0 for either timeout means the
// caller should use its built-in default.
func resolveConnectionMode(
	envMode connectionmode.Mode,
	envRelayTimeout uint32,
	cfgMode connectionmode.Mode,
	cfgRelayTimeout uint32,
	cfgP2pTimeout uint32,
	cfgP2pRetryMax uint32,
	serverPC *mgmProto.PeerConfig,
) (connectionmode.Mode, uint32, uint32, uint32) {
	mode := envMode
	if mode == connectionmode.ModeUnspecified {
		if cfgMode != connectionmode.ModeUnspecified && cfgMode != connectionmode.ModeFollowServer {
			mode = cfgMode
		}
	}
	if mode == connectionmode.ModeUnspecified {
		if serverPC != nil {
			serverMode := connectionmode.FromProto(serverPC.GetConnectionMode())
			if serverMode != connectionmode.ModeUnspecified {
				mode = serverMode
			} else {
				mode = connectionmode.ResolveLegacyLazyBool(serverPC.GetLazyConnectionEnabled())
			}
		} else {
			mode = connectionmode.ModeP2P // safe default when nothing at all is known
		}
	}

	// Relay-timeout precedence (analog).
	relay := envRelayTimeout
	if relay == 0 {
		relay = cfgRelayTimeout
	}
	if relay == 0 && serverPC != nil {
		relay = serverPC.GetRelayTimeoutSeconds()
	}

	// P2P-timeout precedence: client config wins over server push. No env
	// var in Phase 2; reserved for Phase 3.
	p2p := cfgP2pTimeout
	if p2p == 0 && serverPC != nil {
		p2p = serverPC.GetP2PTimeoutSeconds()
	}

	// P2pRetryMax resolution (analogous to p2p timeout):
	// client-config wins over server-pushed value (0 = not set).
	p2pRetryMax := cfgP2pRetryMax
	if p2pRetryMax == 0 && serverPC != nil {
		p2pRetryMax = serverPC.GetP2PRetryMaxSeconds()
	}

	return mode, relay, p2p, p2pRetryMax
}

// Start initializes the connection manager. The lazy/dynamic connection
// manager is brought up immediately when the resolved Mode is P2PLazy
// or P2PDynamic. Other modes keep the manager dormant; it can still be
// activated later via UpdatedRemotePeerConfig.
func (e *ConnMgr) Start(ctx context.Context) {
	if e.lazyConnMgr != nil {
		log.Errorf("lazy/dynamic connection manager is already started")
		return
	}
	if !modeUsesLazyMgr(e.mode) {
		log.Infof("lazy/dynamic connection manager is disabled (mode=%s)", e.mode)
		return
	}
	if e.rosenpassEnabled {
		log.Warnf("rosenpass enabled, lazy/dynamic connection manager will not be started")
		return
	}
	e.initLazyManager(ctx)
	e.startModeSideEffects()
}

// modeUsesLazyMgr is true for the modes whose lifecycle is driven by the
// lazyconn.Manager (which now hosts the two-timer inactivity manager
// since Phase 2). Eager modes (p2p, relay-forced) do not need it.
func modeUsesLazyMgr(m connectionmode.Mode) bool {
	return m == connectionmode.ModeP2PLazy || m == connectionmode.ModeP2PDynamic
}

// shouldRestartLazyMgr returns true when an incoming mgmt push changes
// any value the inactivity.Manager bakes in at construction time and
// has no setter for. Mode change or relay/p2p timeout change qualify.
//
// p2pRetryMaxSecs is deliberately excluded: it's the per-Conn
// ICE-backoff cap, propagated live to every active *peer.Conn via
// propagateP2pRetryMaxToConns -> SetIceBackoffMax. Restarting the
// inactivity manager on a retry-max push would force
// resetPeersToLazyIdle and kick every tunnel back to idle for no
// functional gain.
func shouldRestartLazyMgr(prevMode, newMode connectionmode.Mode, prevRelay, newRelay, prevP2P, newP2P uint32) bool {
	if prevMode != newMode {
		return true
	}
	if prevRelay != newRelay {
		return true
	}
	if prevP2P != newP2P {
		return true
	}
	return false
}

// startModeSideEffects flips the per-mode goroutines and status flags
// that need to follow a successful initLazyManager. Called by Start()
// and by the management-push transition path.
func (e *ConnMgr) startModeSideEffects() {
	// Both lazy AND dynamic are "lazy" from the status-recorder's
	// perspective (peers are not eagerly opened; they wait for activity).
	// The "Lazy connection: true/false" line in `netbird status` reflects
	// this user-visible distinction, not the internal flavor.
	if e.mode == connectionmode.ModeP2PLazy || e.mode == connectionmode.ModeP2PDynamic {
		e.statusRecorder.UpdateLazyConnection(true)
	}
	if e.mode == connectionmode.ModeP2PDynamic {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.runDynamicInactivityLoop(e.lazyCtx)
		}()
	}
}

// runDynamicInactivityLoop reads the ICEInactiveChan from the
// inactivity.Manager and detaches the ICE worker per peer.
//
// Phase-3.7i v0.5: relay-idle teardown is owned exclusively by
// lazyconn.Manager.onPeerInactivityTimedOut, which already calls
// peerStore.PeerConnIdle(...) and performs the same "keep WG peer
// entry, close conn" semantics this loop used to do. Having both
// lazyconn.Manager and ConnMgr consume the same buffered relay-idle
// channel created an aliasing race (only one of the two ever saw any
// given event), which manifested in production as routing peers
// silently stuck in connected-but-stale state.
//
// Only meaningful in p2p-dynamic mode; in p2p-lazy iceTimeout is 0
// and ICEInactiveChan never fires.
func (e *ConnMgr) runDynamicInactivityLoop(ctx context.Context) {
	if e.lazyConnMgr == nil {
		return
	}
	im := e.lazyConnMgr.InactivityManager()
	if im == nil {
		return
	}
	log.Infof("p2p-dynamic ICE-inactivity loop started (iceTimeout=%ds)", e.p2pTimeoutSecs)
	defer log.Infof("p2p-dynamic ICE-inactivity loop stopped")
	for {
		select {
		case <-ctx.Done():
			return
		case peers := <-im.ICEInactiveChan():
			for peerKey := range peers {
				if err := e.DetachICEForPeer(peerKey); err != nil {
					log.Warnf("DetachICEForPeer(%s): %v", peerKey, err)
				}
			}
		}
	}
}

// UpdatedRemotePeerConfig is called when the management server pushes a
// new PeerConfig. Re-resolves the effective mode through the precedence
// chain and starts/stops the lazy manager accordingly.
func (e *ConnMgr) UpdatedRemotePeerConfig(ctx context.Context, pc *mgmProto.PeerConfig) error {
	// Capture the raw server-pushed values before resolution so the UI
	// can surface them independently of any local override.
	if pc != nil {
		serverMode := connectionmode.FromProto(pc.GetConnectionMode())
		if serverMode == connectionmode.ModeUnspecified {
			serverMode = connectionmode.ResolveLegacyLazyBool(pc.GetLazyConnectionEnabled())
		}
		e.spMu.Lock()
		e.serverPushedMode = serverMode
		e.serverPushedRelayTimeoutSecs = pc.GetRelayTimeoutSeconds()
		e.serverPushedP2pTimeoutSecs = pc.GetP2PTimeoutSeconds()
		e.serverPushedP2pRetryMaxSecs = pc.GetP2PRetryMaxSeconds()
		e.spMu.Unlock()
	}

	newMode, newRelay, newP2P, newP2pRetry := resolveConnectionMode(
		e.envMode, e.envRelayTimeout, e.cfgMode, e.cfgRelayTimeout,
		e.cfgP2pTimeout, e.cfgP2pRetryMax, pc,
	)

	if newMode == e.mode && newRelay == e.relayTimeoutSecs &&
		newP2P == e.p2pTimeoutSecs && newP2pRetry == e.p2pRetryMaxSecs {
		return nil
	}
	prev := e.mode
	prevRelay := e.relayTimeoutSecs
	prevP2P := e.p2pTimeoutSecs
	e.mode = newMode
	e.relayTimeoutSecs = newRelay
	e.p2pTimeoutSecs = newP2P
	e.p2pRetryMaxSecs = newP2pRetry
	e.propagateP2pRetryMaxToConns()

	wasManaged := modeUsesLazyMgr(prev)
	isManaged := modeUsesLazyMgr(newMode)
	modeChanged := prev != newMode

	if modeChanged && wasManaged && !isManaged {
		log.Infof("lazy/dynamic connection manager disabled by management push (mode=%s)", newMode)
		e.closeManager(ctx)
		e.statusRecorder.UpdateLazyConnection(false)
		return nil
	}

	if wasManaged && isManaged && shouldRestartLazyMgr(prev, newMode, prevRelay, newRelay, prevP2P, newP2P) {
		log.Infof("lazy/dynamic manager restart: mode %s->%s relay=%d p2p=%d",
			prev, newMode, newRelay, newP2P)
		e.closeManager(ctx)
		e.statusRecorder.UpdateLazyConnection(false)
	}

	if isManaged && e.lazyConnMgr == nil {
		if e.rosenpassEnabled {
			log.Warnf("rosenpass enabled, ignoring lazy/dynamic mode push")
			return nil
		}
		log.Infof("lazy/dynamic connection manager enabled by management push (mode=%s)", newMode)
		e.initLazyManager(ctx)
		e.startModeSideEffects()
		// Phase 3.7i: when management activates lazy/dynamic mode at
		// runtime we must reset all existing peer connections through
		// the lazy/idle entry. The previous AddActivePeers path kept
		// every already-open WireGuard tunnel running and only started
		// the inactivity timers from "now" -- callers expected the new
		// mode to apply immediately ("Idle until traffic"), not "stay
		// open until 3 hours from now". Brief packet loss (~1-2 s per
		// peer while the tunnel is rebuilt) is acceptable; mode changes
		// are rare and almost always intentional.
		return e.resetPeersToLazyIdle(ctx)
	}
	return nil
}

// UpdatedRemoteFeatureFlag is the legacy entry point that only knows the
// boolean LazyConnectionEnabled field. Kept as a thin shim that builds a
// synthetic PeerConfig and delegates to UpdatedRemotePeerConfig.
//
// Deprecated: callers should switch to UpdatedRemotePeerConfig and pass
// the real PeerConfig so the new ConnectionMode + timeouts propagate.
func (e *ConnMgr) UpdatedRemoteFeatureFlag(ctx context.Context, enabled bool) error {
	return e.UpdatedRemotePeerConfig(ctx, &mgmProto.PeerConfig{LazyConnectionEnabled: enabled})
}

// UpdateRouteHAMap updates the route HA mappings in the lazy connection manager
func (e *ConnMgr) UpdateRouteHAMap(haMap route.HAMap) {
	if !e.isStartedWithLazyMgr() {
		log.Debugf("lazy connection manager is not started, skipping UpdateRouteHAMap")
		return
	}

	e.lazyConnMgr.UpdateRouteHAMap(haMap)
}

// SetExcludeList sets the list of peer IDs that should always have permanent connections.
func (e *ConnMgr) SetExcludeList(ctx context.Context, peerIDs map[string]bool) {
	if e.lazyConnMgr == nil {
		return
	}

	excludedPeers := make([]lazyconn.PeerConfig, 0, len(peerIDs))

	for peerID := range peerIDs {
		var peerConn *peer.Conn
		var exists bool
		if peerConn, exists = e.peerStore.PeerConn(peerID); !exists {
			log.Warnf("failed to find peer conn for peerID: %s", peerID)
			continue
		}

		lazyPeerCfg := lazyconn.PeerConfig{
			PublicKey:  peerID,
			AllowedIPs: peerConn.WgConfig().AllowedIps,
			PeerConnID: peerConn.ConnID(),
			Log:        peerConn.Log,
		}
		excludedPeers = append(excludedPeers, lazyPeerCfg)
	}

	added := e.lazyConnMgr.ExcludePeer(excludedPeers)
	for _, peerID := range added {
		var peerConn *peer.Conn
		var exists bool
		if peerConn, exists = e.peerStore.PeerConn(peerID); !exists {
			// if the peer not exist in the store, it means that the engine will call the AddPeerConn in next step
			continue
		}

		peerConn.Log.Infof("peer has been added to lazy connection exclude list, opening permanent connection")
		if err := peerConn.Open(ctx); err != nil {
			peerConn.Log.Errorf("failed to open connection: %v", err)
		}
	}
}

func (e *ConnMgr) AddPeerConn(ctx context.Context, peerKey string, conn *peer.Conn) (exists bool) {
	if success := e.peerStore.AddPeerConn(peerKey, conn); !success {
		return true
	}

	// Wire WG-timeout recovery so the peer is pushed back to lazy-idle
	// (activity listener restarted) when WireGuard handshakes time out.
	// Closes over peerKey so the callback is independent of conn state.
	conn.SetOnWGTimeoutRecover(func() { e.RecoverPeerToIdle(peerKey) })

	if !e.isStartedWithLazyMgr() {
		if err := conn.Open(ctx); err != nil {
			conn.Log.Errorf("failed to open connection: %v", err)
		}
		return
	}

	if !lazyconn.IsSupported(conn.AgentVersionString()) {
		conn.Log.Warnf("peer does not support lazy connection (%s), open permanent connection", conn.AgentVersionString())
		if err := conn.Open(ctx); err != nil {
			conn.Log.Errorf("failed to open connection: %v", err)
		}
		return
	}

	lazyPeerCfg := lazyconn.PeerConfig{
		PublicKey:  peerKey,
		AllowedIPs: conn.WgConfig().AllowedIps,
		PeerConnID: conn.ConnID(),
		Log:        conn.Log,
	}
	excluded, err := e.lazyConnMgr.AddPeer(lazyPeerCfg)
	if err != nil {
		conn.Log.Errorf("failed to add peer to lazyconn manager: %v", err)
		if err := conn.Open(ctx); err != nil {
			conn.Log.Errorf("failed to open connection: %v", err)
		}
		return
	}

	if excluded {
		conn.Log.Infof("peer is on lazy conn manager exclude list, opening connection")
		if err := conn.Open(ctx); err != nil {
			conn.Log.Errorf("failed to open connection: %v", err)
		}
		return
	}

	conn.Log.Infof("peer added to lazy conn manager")
	return
}

func (e *ConnMgr) RemovePeerConn(peerKey string) {
	conn, ok := e.peerStore.Remove(peerKey)
	if !ok {
		return
	}
	// Permanent removal: drop the WG peer entry too. If we kept it the
	// stale entry would linger in WG until the next full reconcile.
	defer conn.Close(false, false)

	if !e.isStartedWithLazyMgr() {
		return
	}

	e.lazyConnMgr.RemovePeer(peerKey)
	conn.Log.Infof("removed peer from lazy conn manager")
}

func (e *ConnMgr) ActivatePeer(ctx context.Context, conn *peer.Conn) {
	if !e.isStartedWithLazyMgr() {
		return
	}

	if found := e.lazyConnMgr.ActivatePeer(conn.GetKey()); found {
		// Phase 3.7j: clear the intentional-detach marker (clear-point #4).
		// Signal-driven wake-up after a remote OFFER means a peer that
		// was intentionally idle is being explicitly re-engaged; the
		// upcoming Open() + AttachICE cycle must see a clean marker so
		// the guard's retry budget is not skipped on any subsequent
		// real pair-check failure.
		conn.ClearIntentionallyDetached()
		if err := conn.Open(ctx); err != nil {
			conn.Log.Errorf("failed to open connection: %v", err)
		}
	}

	// p2p-dynamic: re-attach ICE on EVERY signal trigger, not only on
	// the lazy-manager's first activity edge. The runDynamicInactivityLoop
	// path (DetachICEForPeer when iceTimeout fires) leaves the peer in an
	// "inactivity-with-ICE-detached" sub-state that the lazy manager does
	// not represent. Without this re-arm, subsequent remote OFFERs would
	// reach handshaker.Listen() with iceListener==nil and be silently
	// dropped, leaving the peer stuck on relay even though both sides
	// are signaling normally. AttachICE is idempotent (no-op if listener
	// already attached) and honors iceBackoff.IsSuspended() so the
	// failure-backoff is not bypassed.
	if e.mode == connectionmode.ModeP2PDynamic {
		if err := conn.AttachICE(); err != nil {
			conn.Log.Warnf("AttachICE on signal activity: %v", err)
		}
	}
}

// deactivateAction selects what DeactivatePeer should do when the remote
// peer signals GO_IDLE. The dispatch is a pure function of the locally
// resolved connection mode.
type deactivateAction int

const (
	deactivateNoop deactivateAction = iota
	deactivateLazy
	deactivateICE
)

// deactivatePeerAction returns the per-LOCAL-mode deactivation rule.
// Kept for backward-compat with the v0.1 dispatch contract; the live
// DeactivatePeer path uses deactivatePeerActionFor instead (Phase 3.7j
// Fix A, #5989) which dispatches by the REMOTE peer's effective mode.
//
// Eager modes (p2p, relay-forced, unspecified) ignore GO_IDLE because
// they are meant to keep tunnels always-on. p2p-lazy delegates to the
// lazy connection manager so the whole tunnel is torn down.
// p2p-dynamic detaches only the ICE worker so the relay tunnel stays
// up.
func (e *ConnMgr) deactivatePeerAction() deactivateAction {
	switch e.mode {
	case connectionmode.ModeP2PLazy:
		return deactivateLazy
	case connectionmode.ModeP2PDynamic:
		return deactivateICE
	default:
		return deactivateNoop
	}
}

// deactivatePeerActionFor returns the per-peer deactivation rule based
// on the REMOTE peer's effective connection mode (server-resolved via
// RemotePeerConfig.effective_connection_mode). This is the Phase 3.7j
// Fix A replacement for the local-mode-driven deactivatePeerAction:
// the old dispatch produced Phase-1/Phase-2 cross-talk when a remote
// lazy peer signaled GO_IDLE to a local p2p-dynamic instance (or vice
// versa). The local end has no way of knowing the remote's lifecycle
// expectations without consulting RemoteEffectiveMode.
//
// ModeUnspecified Fallback (Codex round-2 v0.3): during the
// NetworkMap-bootstrap race RemoteEffectiveMode is briefly unknown.
// In that window the conservative-safe action is lazy-full-close when
// a LazyMgr is active (mirrors what p2p-lazy would do), otherwise a
// diagnostic noop. The previous "fall back to local mode" plan was
// rejected because it reintroduced the exact cross-talk Fix A is
// closing -- see spec §3.1.
func (e *ConnMgr) deactivatePeerActionFor(conn *peer.Conn) deactivateAction {
	remote := conn.RemoteEffectiveMode()
	switch remote {
	case connectionmode.ModeP2PLazy:
		return deactivateLazy
	case connectionmode.ModeP2PDynamic:
		return deactivateICE
	case connectionmode.ModeUnspecified:
		// Bootstrap race: prefer the safer lazy-full-close if we have
		// a LazyMgr; otherwise emit a diagnostic and noop.
		if e.isStartedWithLazyMgr() {
			conn.Log.Debugf("GO_IDLE during RemoteEffectiveMode bootstrap race; using lazy-full-close fallback")
			return deactivateLazy
		}
		conn.Log.Debugf("GO_IDLE during RemoteEffectiveMode bootstrap race + no LazyMgr; treating as noop")
		return deactivateNoop
	default:
		// Eager remote modes (p2p, relay-forced) keep the tunnel
		// always-on and never expect a GO_IDLE-driven teardown.
		return deactivateNoop
	}
}

// DeactivatePeer is invoked when the remote peer signals GO_IDLE. The
// behavior is dispatched by the REMOTE peer's effective connection
// mode (see deactivatePeerActionFor). Phase 3.7j Fix A for the
// mode-cross-talk in #5989: the previous dispatch used the LOCAL
// mode, so a v0.51.2 legacy peer that resolves to p2p-lazy server-side
// caused a local p2p-dynamic instance to detach the ICE worker only --
// the relay tunnel stayed up forever, ICE re-attach went into
// exponential backoff, and the next remote OFFER was silently dropped.
//
// deactivateLazy + no LazyMgr is a graceful fallback to ICE detach
// rather than a silent return: at least the ICE pair is freed and the
// relay tunnel stays up; without this branch the eager local end would
// hold a stale ICE pair forever until its own activity timer fired.
func (e *ConnMgr) DeactivatePeer(conn *peer.Conn) {
	switch e.deactivatePeerActionFor(conn) {
	case deactivateLazy:
		if !e.isStartedWithLazyMgr() {
			// Local manager is eager/dynamic but the remote peer wants
			// a full close. Fall through to ICE detach so we at least
			// free the ICE pair; the relay tunnel stays up.
			conn.Log.Infof("remote peer signaled GO_IDLE (lazy semantics) but local mgr not lazy; falling back to ICE detach")
			if err := e.DetachICEForPeer(conn.GetKey()); err != nil {
				conn.Log.Warnf("DetachICEForPeer failed: %v", err)
			}
			return
		}
		conn.Log.Infof("closing peer connection: remote peer initiated inactive, idle lazy state and sent GOAWAY (mode-aware)")
		e.lazyConnMgr.DeactivatePeer(conn.ConnID())
	case deactivateICE:
		conn.Log.Infof("detaching ICE worker: remote peer signaled GO_IDLE (p2p-dynamic, mode-aware)")
		if err := e.DetachICEForPeer(conn.GetKey()); err != nil {
			conn.Log.Warnf("DetachICEForPeer failed: %v", err)
		}
	case deactivateNoop:
		// Eager remote modes keep the tunnel up unconditionally.
		return
	}
}

// RecoverPeerToIdle pushes a peer back into the lazy manager's
// activity-listening idle state after the local WireGuard handshake
// has timed out. Without this, the peer stays stuck in "Connecting"
// forever (lazy mgr keeps it in active set with no activity listener,
// so subsequent local traffic is silently dropped). Codex follow-up.
//
// Safe to call when the lazy mgr is disabled or the peer is unknown:
// both cases short-circuit silently. The lazy mgr's DeactivatePeer
// also ignores peers that are not in the active state, so duplicate
// invocations (e.g. WG timeout twice) are no-ops.
func (e *ConnMgr) RecoverPeerToIdle(peerKey string) {
	if !e.isStartedWithLazyMgr() {
		return
	}
	conn, ok := e.peerStore.PeerConn(peerKey)
	if !ok {
		return
	}
	conn.Log.Infof("WG timeout recovery: pushing peer back to lazy-idle (activity listener will rearm)")
	e.lazyConnMgr.DeactivatePeer(conn.ConnID())
}

// DetachICEForPeer looks up the Conn for peerKey and tears down its
// ICE worker without touching the relay tunnel. Used by:
//   - DeactivatePeer when the remote peer sends GO_IDLE (p2p-dynamic)
//   - the inactivity manager when the iceTimeout elapses (wired in
//     engine.go runDynamicInactivityLoop)
//
// Missing peers are not an error; they may have been removed concurrently.
func (e *ConnMgr) DetachICEForPeer(peerKey string) error {
	conn, ok := e.peerStore.PeerConn(peerKey)
	if !ok {
		return nil
	}
	// Phase 3.7j (#5989): mark the upcoming detach as intentional BEFORE
	// invoking DetachICE so the guard's predicate (Commit 2) treats the
	// resulting disconnected state as expected and does not consume its
	// retry budget. Both DetachICEForPeer call sites are intentional
	// teardowns: (1) remote GO_IDLE via DeactivatePeer, (2) local
	// inactivity timeout via runDynamicInactivityLoop. The marker is
	// cleared by every Attach-family method, ConnMgr.ActivatePeer,
	// onNetworkChange and onICEFailed (see Conn.intentionallyDetached
	// for the full list of clear-points).
	conn.MarkIntentionallyDetached()
	return conn.DetachICE()
}

func (e *ConnMgr) Close() {
	if !e.isStartedWithLazyMgr() {
		return
	}

	e.lazyCtxCancel()
	e.wg.Wait()
	e.lazyConnMgr = nil
}

func (e *ConnMgr) initLazyManager(engineCtx context.Context) {
	cfg := manager.Config{
		InactivityThreshold: inactivityThresholdEnv(),
	}
	if e.relayTimeoutSecs > 0 {
		cfg.RelayInactivityThreshold = time.Duration(e.relayTimeoutSecs) * time.Second
	}
	if e.mode == connectionmode.ModeP2PDynamic && e.p2pTimeoutSecs > 0 {
		cfg.ICEInactivityThreshold = time.Duration(e.p2pTimeoutSecs) * time.Second
	}
	e.lazyConnMgr = manager.NewManager(cfg, engineCtx, e.peerStore, e.iface)

	e.lazyCtx, e.lazyCtxCancel = context.WithCancel(engineCtx)

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.lazyConnMgr.Start(e.lazyCtx)
	}()
}

// propagateP2pRetryMaxToConns iterates all active Conn instances and
// updates their iceBackoff.SetMaxBackoff via the canonical wire-format
// translation in peer.ResolveP2pRetryCap. Single source of truth shared
// with (*peer.Conn).initIceBackoffFromConfig. Phase 3 of #5989.
func (e *ConnMgr) propagateP2pRetryMaxToConns() {
	d := peer.ResolveP2pRetryCap(e.p2pRetryMaxSecs)
	for _, peerKey := range e.peerStore.PeersPubKey() {
		if conn, ok := e.peerStore.PeerConn(peerKey); ok {
			conn.SetIceBackoffMax(d)
		}
	}
}

func (e *ConnMgr) addPeersToLazyConnManager() error {
	peers := e.peerStore.PeersPubKey()
	lazyPeerCfgs := make([]lazyconn.PeerConfig, 0, len(peers))
	for _, peerID := range peers {
		var peerConn *peer.Conn
		var exists bool
		if peerConn, exists = e.peerStore.PeerConn(peerID); !exists {
			log.Warnf("failed to find peer conn for peerID: %s", peerID)
			continue
		}

		lazyPeerCfg := lazyconn.PeerConfig{
			PublicKey:  peerID,
			AllowedIPs: peerConn.WgConfig().AllowedIps,
			PeerConnID: peerConn.ConnID(),
			Log:        peerConn.Log,
		}
		lazyPeerCfgs = append(lazyPeerCfgs, lazyPeerCfg)
	}

	return e.lazyConnMgr.AddActivePeers(lazyPeerCfgs)
}

// resetPeersToLazyIdle closes every currently-open peer connection and
// re-registers it via the standard AddPeer (idle) entry of the lazy
// manager. Used when management activates lazy/dynamic mode at runtime:
// without this, AddActivePeers would keep all existing tunnels running
// until their inactivity timers fired, contradicting the user-visible
// promise of lazy/dynamic ("idle until traffic").
//
// Peers with daemon versions that don't support lazy connection, peers
// on the exclude list, and any AddPeer error fall back to eager Open()
// to preserve current behaviour for those edge cases. Net effect for
// the common case: every supported peer flips from Connected -> Idle
// and waits for the next outbound payload packet.
func (e *ConnMgr) resetPeersToLazyIdle(ctx context.Context) error {
	for _, peerID := range e.peerStore.PeersPubKey() {
		peerConn, ok := e.peerStore.PeerConn(peerID)
		if !ok {
			log.Warnf("failed to find peer conn for peerID: %s", peerID)
			continue
		}

		// Tear the tunnel down. signalToRemote=true so the remote peer
		// also drops its half (otherwise it would keep the tunnel half-
		// open until its own ICE backoff fired). keepWgPeer=false: this
		// is a mode-change full reopen, not a lazy-suspend; the peer
		// will be re-Opened (or re-AddPeerConn'd) right below with a
		// fresh AllowedIP set from the new mode's PeerConfig.
		peerConn.Close(true, false)

		if !lazyconn.IsSupported(peerConn.AgentVersionString()) {
			peerConn.Log.Warnf("peer does not support lazy connection (%s), opening permanent connection after mode reset", peerConn.AgentVersionString())
			if err := peerConn.Open(ctx); err != nil {
				peerConn.Log.Errorf("failed to re-open connection after mode reset: %v", err)
			}
			continue
		}

		lazyPeerCfg := lazyconn.PeerConfig{
			PublicKey:  peerID,
			AllowedIPs: peerConn.WgConfig().AllowedIps,
			PeerConnID: peerConn.ConnID(),
			Log:        peerConn.Log,
		}
		excluded, err := e.lazyConnMgr.AddPeer(lazyPeerCfg)
		if err != nil {
			peerConn.Log.Errorf("failed to add peer to lazy conn manager during mode reset: %v", err)
			if err := peerConn.Open(ctx); err != nil {
				peerConn.Log.Errorf("failed to re-open connection after AddPeer error: %v", err)
			}
			continue
		}
		if excluded {
			peerConn.Log.Infof("peer is on lazy conn manager exclude list, opening connection after mode reset")
			if err := peerConn.Open(ctx); err != nil {
				peerConn.Log.Errorf("failed to re-open excluded peer after mode reset: %v", err)
			}
			continue
		}
		peerConn.Log.Infof("peer reset to idle by lazy/dynamic mode change")
	}
	return nil
}

func (e *ConnMgr) closeManager(ctx context.Context) {
	if e.lazyConnMgr == nil {
		return
	}

	e.lazyCtxCancel()
	e.wg.Wait()
	e.lazyConnMgr = nil

	for _, peerID := range e.peerStore.PeersPubKey() {
		e.peerStore.PeerConnOpen(ctx, peerID)
	}
}

func (e *ConnMgr) isStartedWithLazyMgr() bool {
	return e.lazyConnMgr != nil && e.lazyCtxCancel != nil
}

// Mode returns the currently resolved connection mode. Used by the engine
// when constructing per-peer connections (Phase 1 forwards it into
// peer.ConnConfig in a follow-up commit).
func (e *ConnMgr) Mode() connectionmode.Mode {
	return e.mode
}

// RelayTimeout returns the resolved relay-worker idle timeout in seconds.
func (e *ConnMgr) RelayTimeout() uint32 {
	return e.relayTimeoutSecs
}

// P2pRetryMax returns the resolved cap in seconds for the ICE-failure
// backoff schedule. Wire-format sentinel uint32-max means "user-explicit
// disable"; callers must translate that to 0. Phase 3 of #5989.
func (e *ConnMgr) P2pRetryMax() uint32 {
	return e.p2pRetryMaxSecs
}

// P2pTimeout returns the resolved ICE-only inactivity timeout in
// seconds. Phase 2 of #5989. 0 = ICE never times out (for non-dynamic
// modes). Phase 3.7i adds this accessor so the engine can include it
// in PeerSystemMeta.
func (e *ConnMgr) P2pTimeout() uint32 {
	return e.p2pTimeoutSecs
}

// ServerPushedMode returns the connection mode the management server
// most recently pushed via PeerConfig (independent of any local env
// or config override). Returns ModeUnspecified if no PeerConfig has
// been received yet. Used by the Android UI to display "Follow server
// (currently: <mode>)" in the override dropdown.
func (e *ConnMgr) ServerPushedMode() connectionmode.Mode {
	e.spMu.RLock()
	defer e.spMu.RUnlock()
	return e.serverPushedMode
}

// ServerPushedRelayTimeoutSecs returns the relay-worker idle-timeout
// (seconds) most recently pushed by the management server, or 0 if no
// PeerConfig has been received. Used by the Android UI as a hint in
// the override field.
func (e *ConnMgr) ServerPushedRelayTimeoutSecs() uint32 {
	e.spMu.RLock()
	defer e.spMu.RUnlock()
	return e.serverPushedRelayTimeoutSecs
}

// ServerPushedP2pTimeoutSecs returns the ICE-only inactivity timeout
// (seconds) most recently pushed by the management server. Only
// meaningful in p2p-dynamic mode.
func (e *ConnMgr) ServerPushedP2pTimeoutSecs() uint32 {
	e.spMu.RLock()
	defer e.spMu.RUnlock()
	return e.serverPushedP2pTimeoutSecs
}

// ServerPushedP2pRetryMaxSecs returns the ICE-failure backoff cap
// (seconds) most recently pushed by the management server. When the
// server has not pushed a value (Phase 1 management servers do not
// know about this field yet) the built-in DefaultP2PRetryMax is
// returned so the Android UI hint shows what value the daemon is
// actually using as fallback.
func (e *ConnMgr) ServerPushedP2pRetryMaxSecs() uint32 {
	e.spMu.RLock()
	v := e.serverPushedP2pRetryMaxSecs
	e.spMu.RUnlock()
	if v > 0 {
		return v
	}
	return uint32(peer.DefaultP2PRetryMax / time.Second)
}

func inactivityThresholdEnv() *time.Duration {
	envValue := os.Getenv(lazyconn.EnvInactivityThreshold)
	if envValue == "" {
		return nil
	}

	parsedMinutes, err := strconv.Atoi(envValue)
	if err != nil || parsedMinutes <= 0 {
		return nil
	}

	d := time.Duration(parsedMinutes) * time.Minute
	return &d
}
