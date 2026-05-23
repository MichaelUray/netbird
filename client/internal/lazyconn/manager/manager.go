package manager

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/client/internal/lazyconn"
	"github.com/netbirdio/netbird/client/internal/lazyconn/activity"
	"github.com/netbirdio/netbird/client/internal/lazyconn/inactivity"
	peerid "github.com/netbirdio/netbird/client/internal/peer/id"
	"github.com/netbirdio/netbird/client/internal/peerstore"
	"github.com/netbirdio/netbird/route"
)

const (
	watcherActivity watcherType = iota
	watcherInactivity
)

// testListenerArmHook is set by tests to inject a hook between the
// armActivityListener call and the peerStillManaged Re-Validate inside
// onPeerInactivityTimedOut + watchdog recovery paths. Production
// codepaths leave this nil — no overhead beyond a nil-check.
//
// Stored in an atomic.Value because watchdog recovery goroutines
// spawned by spawnRecovery may outlive the cancellation of the
// runReconcileWatchdog goroutine; without atomic access, a test
// nilling the hook in t.Cleanup would race the still-running
// recovery goroutine. Production loads collapse to a single
// atomic-pointer load + nil-check.
var testListenerArmHook atomic.Value // of testListenerHook

// testListenerHook wraps the test hook so atomic.Value can store a
// typed nil consistently. callIfSet invokes the hook if a non-nil
// callback has been installed.
type testListenerHook struct {
	fn func(pubKey string)
}

func callTestListenerArmHook(pubKey string) {
	v := testListenerArmHook.Load()
	if v == nil {
		return
	}
	h, ok := v.(testListenerHook)
	if !ok || h.fn == nil {
		return
	}
	h.fn(pubKey)
}

// setTestListenerArmHook is a test-only setter (kept package-private).
// Tests call this via t.Setenv-style helpers; production never invokes
// it. A nil fn clears the hook.
func setTestListenerArmHook(fn func(pubKey string)) {
	testListenerArmHook.Store(testListenerHook{fn: fn})
}

// userInitiatedAttachICECooldown bounds how often a real lazyconn activity
// edge can drive a fresh ICE attempt while ICE backoff is suspended.
// Phase 3.7i (#5989): user-traffic is allowed to bypass the long
// failure-retry-suspend at most once per cooldown so a steady packet
// stream (e.g. a VNC session) does not produce continuous SendOffer
// storms. Tuned to half-a-minute to roughly match the 13-15 s pion ICE
// pair-check budget plus a small grace gap.
const userInitiatedAttachICECooldown = 30 * time.Second

// defaultReconcileInterval is the production tick period for the
// lazyconn reconcile-watchdog (Phase 3.7i Stufe 2 / Task 6). The
// watchdog heals two stuck states (notifyChan-drop and post-Close-hang)
// detected by combining inactivity DropCounters + TransportSnapshot +
// activity.HasPeer. 120 s is a balance between recovery latency (worst
// case ~2 min until a stuck VNC peer self-heals) and CPU overhead.
// Tests use shorter intervals via runReconcileWatchdog(ctx, interval).
const defaultReconcileInterval = 120 * time.Second

type watcherType int

type managedPeer struct {
	peerCfg         *lazyconn.PeerConfig
	expectedWatcher watcherType
}

type Config struct {
	// Phase-1 single-timer field. Deprecated: use ICEInactivityThreshold
	// and RelayInactivityThreshold instead. Kept so existing callers
	// (engine.go) compile during the Phase-2 transition; internally
	// treated as RelayInactivityThreshold when the new fields are zero.
	InactivityThreshold *time.Duration

	// ICEInactivityThreshold is the per-peer ICE-worker idle timeout
	// (Phase 2 / #5989). 0 = ICE always-on (= p2p-lazy semantics, where
	// the whole tunnel goes idle but ICE is never torn down separately).
	ICEInactivityThreshold time.Duration

	// RelayInactivityThreshold is the per-peer relay-worker idle timeout
	// (Phase 2). 0 = relay always-on.
	RelayInactivityThreshold time.Duration
}

// resolvedTimeouts returns the effective (ICE, Relay) timeouts. If only
// the deprecated InactivityThreshold field is set, it maps onto the
// relay timeout for Phase-1 p2p-lazy semantics.
func (c Config) resolvedTimeouts() (iceTimeout, relayTimeout time.Duration) {
	relay := c.RelayInactivityThreshold
	if relay == 0 && c.InactivityThreshold != nil {
		relay = *c.InactivityThreshold
	}
	return c.ICEInactivityThreshold, relay
}

// Manager manages lazy connections
// It is responsible for:
// - Managing lazy connections activated on-demand
// - Managing inactivity monitors for lazy connections (based on peer disconnection events)
// - Maintaining a list of excluded peers that should always have permanent connections
// - Handling connection establishment based on peer signaling
// - Managing route HA groups and activating all peers in a group when one peer is activated
type Manager struct {
	engineCtx           context.Context
	peerStore           *peerstore.Store
	inactivityThreshold time.Duration

	managedPeers         map[string]*lazyconn.PeerConfig
	managedPeersByConnID map[peerid.ConnID]*managedPeer
	excludes             map[string]lazyconn.PeerConfig
	managedPeersMu       sync.Mutex

	activityManager   *activity.Manager
	inactivityManager *inactivity.Manager

	// Route HA group management
	// If any peer in the same HA group is active, all peers in that group should prevent going idle
	peerToHAGroups map[string][]route.HAUniqueID // peer ID -> HA groups they belong to
	haGroupToPeers map[route.HAUniqueID][]string // HA group -> peer IDs in the group
	routesMu       sync.RWMutex
}

// NewManager creates a new lazy connection manager
// engineCtx is the context for creating peer Connection
func NewManager(config Config, engineCtx context.Context, peerStore *peerstore.Store, wgIface lazyconn.WGIface) *Manager {
	log.Infof("setup lazy connection service")

	m := &Manager{
		engineCtx:            engineCtx,
		peerStore:            peerStore,
		inactivityThreshold:  inactivity.DefaultInactivityThreshold,
		managedPeers:         make(map[string]*lazyconn.PeerConfig),
		managedPeersByConnID: make(map[peerid.ConnID]*managedPeer),
		excludes:             make(map[string]lazyconn.PeerConfig),
		activityManager:      activity.NewManager(wgIface),
		peerToHAGroups:       make(map[string][]route.HAUniqueID),
		haGroupToPeers:       make(map[route.HAUniqueID][]string),
	}

	if wgIface.IsUserspaceBind() {
		iceTO, relayTO := config.resolvedTimeouts()
		if iceTO == 0 && relayTO == 0 {
			// Phase 1 / single-timer fallback when caller hasn't migrated.
			m.inactivityManager = inactivity.NewManager(wgIface, config.InactivityThreshold)
		} else {
			m.inactivityManager = inactivity.NewManagerWithTwoTimers(wgIface, iceTO, relayTO)
		}
	} else {
		log.Warnf("inactivity manager not supported for kernel mode, wait for remote peer to close the connection")
	}

	return m
}

// InactivityManager exposes the underlying inactivity.Manager so the
// engine / conn_mgr can subscribe to ICEInactiveChan / RelayInactiveChan
// in the p2p-dynamic mode lifecycle. Returns nil if the manager runs in
// kernel-bind mode (no inactivity tracking) or if the manager itself is
// nil (defensive).
func (m *Manager) InactivityManager() *inactivity.Manager {
	if m == nil {
		return nil
	}
	return m.inactivityManager
}

// UpdateRouteHAMap updates the HA group mappings for routes
// This should be called when route configuration changes
func (m *Manager) UpdateRouteHAMap(haMap route.HAMap) {
	m.routesMu.Lock()
	defer m.routesMu.Unlock()

	clear(m.peerToHAGroups)
	clear(m.haGroupToPeers)

	for haUniqueID, routes := range haMap {
		var peers []string

		peerSet := make(map[string]bool)
		for _, r := range routes {
			if !peerSet[r.Peer] {
				peerSet[r.Peer] = true
				peers = append(peers, r.Peer)
			}
		}

		if len(peers) <= 1 {
			continue
		}

		m.haGroupToPeers[haUniqueID] = peers

		for _, peerID := range peers {
			m.peerToHAGroups[peerID] = append(m.peerToHAGroups[peerID], haUniqueID)
		}
	}

	log.Debugf("updated route HA mappings: %d HA groups, %d peers with routes", len(m.haGroupToPeers), len(m.peerToHAGroups))
}

// Start starts the manager and listens for peer activity and inactivity events.
// Two code paths depending on whether the inactivity manager is initialized
// (manager.go NewManager only initializes it for userspace-bind WG). The
// kernel-mode path skips the inactivity-channel arm entirely; without this
// guard a kernel-mode caller would nil-deref on
// m.inactivityManager.InactivePeersChan(). v0.7 Stufe 0 hardening (Codex
// round-11): the watchdog (Task 6) also lives only on the userspace path
// because relayDrops + inactivityManager are its inputs.
func (m *Manager) Start(ctx context.Context) {
	defer m.close()

	if m.inactivityManager == nil {
		// Kernel-mode: no inactivity tracking, no watchdog.
		for {
			select {
			case <-ctx.Done():
				return
			case peerConnID := <-m.activityManager.OnActivityChan:
				m.safeOnPeerActivity(peerConnID)
			}
		}
	}

	// Userspace-bind: inactivity tracking active. Task 6 adds the
	// runReconcileWatchdog goroutine into this block.
	go m.inactivityManager.Start(ctx)
	go m.runReconcileWatchdog(ctx, defaultReconcileInterval) // v0.7 Stufe 4 wiring

	for {
		select {
		case <-ctx.Done():
			return
		case peerConnID := <-m.activityManager.OnActivityChan:
			m.safeOnPeerActivity(peerConnID)
		case peerIDs := <-m.inactivityManager.InactivePeersChan():
			m.safeOnPeerInactivityTimedOut(peerIDs)
		}
	}
}

// safeOnPeerActivity wraps onPeerActivity with a panic recovery so the
// consumer goroutine survives bugs in downstream handlers. v0.7 Stufe 0
// hardening — does NOT fix stuck-state symptoms (a Go panic crashes
// the whole process unless recovered here), but prevents future
// regressions.
func (m *Manager) safeOnPeerActivity(peerConnID peerid.ConnID) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("lazyconn: panic in onPeerActivity (peerConnID=%v): %v", peerConnID, r)
		}
	}()
	m.onPeerActivity(peerConnID)
}

// safeOnPeerInactivityTimedOut wraps onPeerInactivityTimedOut with a
// panic recovery (see safeOnPeerActivity).
func (m *Manager) safeOnPeerInactivityTimedOut(peerIDs map[string]struct{}) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("lazyconn: panic in onPeerInactivityTimedOut: %v", r)
		}
	}()
	m.onPeerInactivityTimedOut(peerIDs)
}

// ExcludePeer marks peers for a permanent connection
// It removes peers from the managed list if they are added to the exclude list
// Adds them back to the managed list and start the inactivity listener if they are removed from the exclude list. In
// this case, we suppose that the connection status is connected or connecting.
// If the peer is not exists yet in the managed list then the responsibility is the upper layer to call the AddPeer function
func (m *Manager) ExcludePeer(peerConfigs []lazyconn.PeerConfig) []string {
	m.managedPeersMu.Lock()
	defer m.managedPeersMu.Unlock()

	added := make([]string, 0)
	excludes := make(map[string]lazyconn.PeerConfig, len(peerConfigs))

	for _, peerCfg := range peerConfigs {
		log.Infof("update excluded lazy connection list with peer: %s", peerCfg.PublicKey)
		excludes[peerCfg.PublicKey] = peerCfg
	}

	// if a peer is newly added to the exclude list, remove from the managed peers list
	for pubKey, peerCfg := range excludes {
		if _, wasExcluded := m.excludes[pubKey]; wasExcluded {
			continue
		}

		added = append(added, pubKey)
		peerCfg.Log.Infof("peer newly added to lazy connection exclude list")
		m.removePeer(pubKey)
	}

	// if a peer has been removed from exclude list then it should be added to the managed peers
	for pubKey, peerCfg := range m.excludes {
		if _, stillExcluded := excludes[pubKey]; stillExcluded {
			continue
		}

		peerCfg.Log.Infof("peer removed from lazy connection exclude list")

		if err := m.addActivePeer(&peerCfg); err != nil {
			log.Errorf("failed to add peer to lazy connection manager: %s", err)
			continue
		}
	}

	m.excludes = excludes
	return added
}

func (m *Manager) AddPeer(peerCfg lazyconn.PeerConfig) (bool, error) {
	m.managedPeersMu.Lock()
	defer m.managedPeersMu.Unlock()

	peerCfg.Log.Debugf("adding peer to lazy connection manager")

	_, exists := m.excludes[peerCfg.PublicKey]
	if exists {
		return true, nil
	}

	if _, ok := m.managedPeers[peerCfg.PublicKey]; ok {
		peerCfg.Log.Warnf("peer already managed")
		return false, nil
	}

	if err := m.activityManager.MonitorPeerActivity(peerCfg); err != nil {
		return false, err
	}

	m.managedPeers[peerCfg.PublicKey] = &peerCfg
	m.managedPeersByConnID[peerCfg.PeerConnID] = &managedPeer{
		peerCfg:         &peerCfg,
		expectedWatcher: watcherActivity,
	}

	// Check if this peer should be activated because its HA group peers are active
	if group, ok := m.shouldActivateNewPeer(peerCfg.PublicKey); ok {
		peerCfg.Log.Debugf("peer belongs to active HA group %s, will activate immediately", group)
		m.activateNewPeerInActiveGroup(peerCfg)
	}

	return false, nil
}

// AddActivePeers adds a list of peers to the lazy connection manager
// suppose these peers was in connected or in connecting states
func (m *Manager) AddActivePeers(peerCfg []lazyconn.PeerConfig) error {
	m.managedPeersMu.Lock()
	defer m.managedPeersMu.Unlock()

	for _, cfg := range peerCfg {
		if _, ok := m.managedPeers[cfg.PublicKey]; ok {
			cfg.Log.Errorf("peer already managed")
			continue
		}

		if err := m.addActivePeer(&cfg); err != nil {
			cfg.Log.Errorf("failed to add peer to lazy connection manager: %v", err)
			return err
		}
	}
	return nil
}

func (m *Manager) RemovePeer(peerID string) {
	m.managedPeersMu.Lock()
	defer m.managedPeersMu.Unlock()

	m.removePeer(peerID)
}

// ActivatePeer activates a peer connection when a signal message is received
// Also activates all peers in the same HA groups as this peer
func (m *Manager) ActivatePeer(peerID string) (found bool) {
	m.managedPeersMu.Lock()
	defer m.managedPeersMu.Unlock()
	cfg, mp := m.getPeerForActivation(peerID)
	if cfg == nil {
		return false
	}

	cfg.Log.Infof("activate peer from inactive state by remote signal message")

	if !m.activateSinglePeer(cfg, mp) {
		return false
	}

	m.activateHAGroupPeers(cfg)
	return true
}

func (m *Manager) DeactivatePeer(peerID peerid.ConnID) {
	m.managedPeersMu.Lock()
	defer m.managedPeersMu.Unlock()

	mp, ok := m.managedPeersByConnID[peerID]
	if !ok {
		return
	}

	if mp.expectedWatcher != watcherInactivity {
		return
	}

	m.peerStore.PeerConnClose(mp.peerCfg.PublicKey)

	mp.peerCfg.Log.Infof("start activity monitor")

	mp.expectedWatcher = watcherActivity

	m.inactivityManager.RemovePeer(mp.peerCfg.PublicKey)

	if err := m.activityManager.MonitorPeerActivity(*mp.peerCfg); err != nil {
		mp.peerCfg.Log.Errorf("failed to create activity monitor: %v", err)
		return
	}
}

// getPeerForActivation checks if a peer can be activated and returns the necessary structs
// Returns nil values if the peer should be skipped
func (m *Manager) getPeerForActivation(peerID string) (*lazyconn.PeerConfig, *managedPeer) {
	cfg, ok := m.managedPeers[peerID]
	if !ok {
		return nil, nil
	}

	mp, ok := m.managedPeersByConnID[cfg.PeerConnID]
	if !ok {
		return nil, nil
	}

	// signal messages coming continuously after success activation, with this avoid the multiple activation
	if mp.expectedWatcher == watcherInactivity {
		return nil, nil
	}

	return cfg, mp
}

// activateSinglePeer activates a single peer
// return true if the peer was activated, false if it was already active
func (m *Manager) activateSinglePeer(cfg *lazyconn.PeerConfig, mp *managedPeer) bool {
	if mp.expectedWatcher == watcherInactivity {
		return false
	}

	mp.expectedWatcher = watcherInactivity
	m.activityManager.RemovePeer(cfg.Log, cfg.PeerConnID)
	m.inactivityManager.AddPeer(cfg)
	return true
}

// activateHAGroupPeers activates all peers in HA groups that the given peer belongs to
func (m *Manager) activateHAGroupPeers(triggeredPeerCfg *lazyconn.PeerConfig) {
	var peersToActivate []string

	m.routesMu.RLock()
	haGroups := m.peerToHAGroups[triggeredPeerCfg.PublicKey]

	if len(haGroups) == 0 {
		m.routesMu.RUnlock()
		triggeredPeerCfg.Log.Debugf("peer is not part of any HA groups")
		return
	}

	for _, haGroup := range haGroups {
		peers := m.haGroupToPeers[haGroup]
		for _, peerID := range peers {
			if peerID != triggeredPeerCfg.PublicKey {
				peersToActivate = append(peersToActivate, peerID)
			}
		}
	}
	m.routesMu.RUnlock()

	activatedCount := 0
	for _, peerID := range peersToActivate {
		cfg, mp := m.getPeerForActivation(peerID)
		if cfg == nil {
			continue
		}

		if m.activateSinglePeer(cfg, mp) {
			activatedCount++
			cfg.Log.Infof("activated peer as part of HA group (triggered by %s)", triggeredPeerCfg.PublicKey)
			m.peerStore.PeerConnOpen(m.engineCtx, cfg.PublicKey)
		}
	}

	if activatedCount > 0 {
		log.Infof("activated %d additional peers in HA groups for peer %s (groups: %v)",
			activatedCount, triggeredPeerCfg.PublicKey, haGroups)
	}
}

// shouldActivateNewPeer checks if a newly added peer should be activated
// because other peers in its HA groups are already active
func (m *Manager) shouldActivateNewPeer(peerID string) (route.HAUniqueID, bool) {
	m.routesMu.RLock()
	defer m.routesMu.RUnlock()

	haGroups := m.peerToHAGroups[peerID]
	if len(haGroups) == 0 {
		return "", false
	}

	for _, haGroup := range haGroups {
		peers := m.haGroupToPeers[haGroup]
		for _, groupPeerID := range peers {
			if groupPeerID == peerID {
				continue
			}

			cfg, ok := m.managedPeers[groupPeerID]
			if !ok {
				continue
			}
			if mp, ok := m.managedPeersByConnID[cfg.PeerConnID]; ok && mp.expectedWatcher == watcherInactivity {
				return haGroup, true
			}
		}
	}
	return "", false
}

// activateNewPeerInActiveGroup activates a newly added peer that should be active due to HA group
func (m *Manager) activateNewPeerInActiveGroup(peerCfg lazyconn.PeerConfig) {
	mp, ok := m.managedPeersByConnID[peerCfg.PeerConnID]
	if !ok {
		return
	}

	if !m.activateSinglePeer(&peerCfg, mp) {
		return
	}

	peerCfg.Log.Infof("activated newly added peer due to active HA group peers")
	m.peerStore.PeerConnOpen(m.engineCtx, peerCfg.PublicKey)
}

func (m *Manager) addActivePeer(peerCfg *lazyconn.PeerConfig) error {
	if _, ok := m.managedPeers[peerCfg.PublicKey]; ok {
		peerCfg.Log.Warnf("peer already managed")
		return nil
	}

	m.managedPeers[peerCfg.PublicKey] = peerCfg
	m.managedPeersByConnID[peerCfg.PeerConnID] = &managedPeer{
		peerCfg:         peerCfg,
		expectedWatcher: watcherInactivity,
	}

	m.inactivityManager.AddPeer(peerCfg)
	return nil
}

func (m *Manager) removePeer(peerID string) {
	cfg, ok := m.managedPeers[peerID]
	if !ok {
		return
	}

	cfg.Log.Infof("removing lazy peer")

	m.inactivityManager.RemovePeer(cfg.PublicKey)
	m.activityManager.RemovePeer(cfg.Log, cfg.PeerConnID)
	delete(m.managedPeers, peerID)
	delete(m.managedPeersByConnID, cfg.PeerConnID)
}

func (m *Manager) close() {
	m.managedPeersMu.Lock()
	defer m.managedPeersMu.Unlock()

	m.activityManager.Close()

	m.managedPeers = make(map[string]*lazyconn.PeerConfig)
	m.managedPeersByConnID = make(map[peerid.ConnID]*managedPeer)

	// Clear route mappings
	m.routesMu.Lock()
	m.peerToHAGroups = make(map[string][]route.HAUniqueID)
	m.haGroupToPeers = make(map[route.HAUniqueID][]string)
	m.routesMu.Unlock()

	log.Infof("lazy connection manager closed")
}

// shouldDeferIdleForHA checks if peer should stay connected due to HA group requirements
func (m *Manager) shouldDeferIdleForHA(inactivePeers map[string]struct{}, peerID string) bool {
	m.routesMu.RLock()
	defer m.routesMu.RUnlock()

	haGroups := m.peerToHAGroups[peerID]
	if len(haGroups) == 0 {
		return false
	}

	for _, haGroup := range haGroups {
		if active := m.checkHaGroupActivity(haGroup, peerID, inactivePeers); active {
			return true
		}
	}

	return false
}

func (m *Manager) checkHaGroupActivity(haGroup route.HAUniqueID, peerID string, inactivePeers map[string]struct{}) bool {
	groupPeers := m.haGroupToPeers[haGroup]
	for _, groupPeerID := range groupPeers {

		if groupPeerID == peerID {
			continue
		}

		cfg, ok := m.managedPeers[groupPeerID]
		if !ok {
			continue
		}

		groupMp, ok := m.managedPeersByConnID[cfg.PeerConnID]
		if !ok {
			continue
		}

		if groupMp.expectedWatcher != watcherInactivity {
			continue
		}

		// If any peer in the group is active, do defer idle
		if _, isInactive := inactivePeers[groupPeerID]; !isInactive {
			return true
		}
	}
	return false
}

func (m *Manager) onPeerActivity(peerConnID peerid.ConnID) {
	m.managedPeersMu.Lock()
	defer m.managedPeersMu.Unlock()

	mp, ok := m.managedPeersByConnID[peerConnID]
	if !ok {
		log.Errorf("peer not found by conn id: %v", peerConnID)
		return
	}

	if mp.expectedWatcher != watcherActivity {
		mp.peerCfg.Log.Warnf("ignore activity event")
		return
	}

	mp.peerCfg.Log.Infof("detected peer activity")

	if !m.activateSinglePeer(mp.peerCfg, mp) {
		return
	}

	m.activateHAGroupPeers(mp.peerCfg)

	// Phase 3.7i (#5989): the signal-trigger and activity-trigger paths
	// must be symmetric. Signal-trigger goes through
	// ConnMgr.ActivatePeer which calls conn.AttachICE for p2p-dynamic.
	// Activity-trigger here previously went through PeerConnOpen only
	// — Open() recreates workerICE but does NOT register the ICE
	// listener on the handshaker (deferICEListener=true for
	// p2p-dynamic). Without AttachICE the guard's onGuardEvent then
	// sees readICEListener()==nil + everConnected==true and skips
	// every offer with "will re-attach on real traffic" — but the
	// only re-attach path is here, so we'd loop forever.
	//
	// Merge note (build/production-v2): the 0ca25fe4c base does a hard
	// ResetIceBackoff + AttachICE + NotifyGuardActivity on every activity
	// wake (strong "user wants this peer back" semantics). Fix #3 added a
	// rate-limited AttachICEUserInitiated path via PeerConnOpenUserInitiated.
	// We keep the stronger reset path here (tested in production APK), but
	// the PeerConnOpenUserInitiated / AttachICEUserInitiated APIs are kept
	// in the codebase as additional, more explicit hooks for callers that
	// want the bypass-without-reset semantics.
	m.peerStore.PeerConnOpen(m.engineCtx, mp.peerCfg.PublicKey)

	if conn, ok := m.peerStore.PeerConn(mp.peerCfg.PublicKey); ok {
		conn.ResetIceBackoff()
		if err := conn.AttachICE(); err != nil {
			mp.peerCfg.Log.Warnf("AttachICE on activity wake: %v", err)
		}
		// Phase 3.7i (#5989), Codex review 2026-05-05: also reset the
		// guard's per-cycle ICE retry budget. After C->A Idle wake the
		// Conn (and its guard) is freshly created, but the 3-retries-
		// then-hourly counter is shared across the whole reconnect
		// cycle. For peers with non-LAN candidates a single fresh
		// pair-check cycle often needs all 3 tries (cold srflx
		// mappings), and without an activity-driven reset the next
		// real user packet would already find the guard in hourly
		// mode -- defeating p2p-dynamic's "fast P2P recovery" promise.
		conn.NotifyGuardActivity()
	}
}

// transitionToActivityWatcherStateOnly performs the non-blocking
// state-machine part of the watcherInactivity → watcherActivity
// transition (expectedWatcher flip + RemovePeer from inactivity manager).
// Caller MUST hold m.managedPeersMu. No I/O here.
func (m *Manager) transitionToActivityWatcherStateOnly(mp *managedPeer) {
	mp.peerCfg.Log.Infof("transition to watcherActivity (state-only) from %v", mp.expectedWatcher)
	mp.expectedWatcher = watcherActivity
	m.inactivityManager.RemovePeer(mp.peerCfg.PublicKey)
}

// armActivityListener installs the activity monitor for a peer.
// Idempotent — activity.Manager.MonitorPeerActivity logs a warning and
// returns nil when called for an already-monitored connID. No lock
// required (activityManager has its own internal mutex).
func (m *Manager) armActivityListener(mp *managedPeer) {
	if err := m.activityManager.MonitorPeerActivity(*mp.peerCfg); err != nil {
		mp.peerCfg.Log.Errorf("failed to create activity monitor: %v", err)
	}
}

// peerStillManaged is the post-arm Re-Validate helper for any code path
// that releases managedPeersMu before calling armActivityListener.
// Re-acquires the lock briefly to verify the peer is still managed with
// the SAME PeerConnID, defending against RemovePeer/ExcludePeer racing
// (R14). Returns true if peer is still managed and connID matches.
//
// Used by:
//   - onPeerInactivityTimedOut (this commit)
//   - recoverInactivityStuck + recoverActivityNoListener (Watchdog, Task 6)
func (m *Manager) peerStillManaged(pubKey string, expectedConnID peerid.ConnID) bool {
	m.managedPeersMu.Lock()
	defer m.managedPeersMu.Unlock()
	cfg, ok := m.managedPeers[pubKey]
	if !ok {
		return false
	}
	return cfg.PeerConnID == expectedConnID
}

func (m *Manager) onPeerInactivityTimedOut(peerIDs map[string]struct{}) {
	// Phase 1: short lock — state mutations + capture of connID+log
	// BEFORE unlock for safe use of activityManager.RemovePeer on
	// cleanup (signature is RemovePeer(*log.Entry, peerid.ConnID),
	// see activity/manager.go:84).
	type pending struct {
		mp      *managedPeer
		connID  peerid.ConnID
		peerLog *log.Entry
		pubKey  string
	}

	m.managedPeersMu.Lock()
	toTransition := make([]pending, 0, len(peerIDs))
	for peerID := range peerIDs {
		peerCfg, ok := m.managedPeers[peerID]
		if !ok {
			log.Errorf("peer not found by peerId: %v", peerID)
			continue
		}
		mp, ok := m.managedPeersByConnID[peerCfg.PeerConnID]
		if !ok {
			log.Errorf("peer not found by conn id: %v", peerCfg.PeerConnID)
			continue
		}
		if mp.expectedWatcher != watcherInactivity {
			mp.peerCfg.Log.Warnf("ignore inactivity event")
			continue
		}
		if m.shouldDeferIdleForHA(peerIDs, mp.peerCfg.PublicKey) {
			mp.peerCfg.Log.Infof("defer inactivity due to active HA group peers")
			continue
		}
		mp.peerCfg.Log.Infof("connection timed out")
		m.transitionToActivityWatcherStateOnly(mp)
		toTransition = append(toTransition, pending{
			mp:      mp,
			connID:  peerCfg.PeerConnID,
			peerLog: peerCfg.Log,
			pubKey:  peerCfg.PublicKey,
		})
	}
	m.managedPeersMu.Unlock()

	// Phase 2: blocking I/O outside lock. Sequential close → listener-arm
	// (v0.3 ordering restored after v0.4 race finding). Then a post-arm
	// Re-Validate (v0.7.1 R14): if RemovePeer/ExcludePeer raced between
	// our state-flip and listener-arm, the listener we just installed
	// belongs to a no-longer-managed peer. Remove it.
	for _, p := range toTransition {
		m.peerStore.PeerConnIdle(p.pubKey)
		m.armActivityListener(p.mp)
		callTestListenerArmHook(p.pubKey)
		if !m.peerStillManaged(p.pubKey, p.connID) {
			m.activityManager.RemovePeer(p.peerLog, p.connID)
		}
	}
}

// isStuckPeer is the Case-a heuristic: peer is watcherInactivity but the
// notifyChan dropped events AND both transports are disconnected.
// v0.5 Codex round-4: ONLY relayDrops trigger this — iceDrops semantically
// belong to ConnMgr.runDynamicInactivityLoop (DetachICEForPeer), not full
// sleep.
func isStuckPeer(iceDisc, relayDisc bool, deltaRelay uint64) bool {
	return iceDisc && relayDisc && deltaRelay > 0
}

// recoverInactivityStuck handles Case-a: peer in watcherInactivity, but
// notifyChan dropped the inactivity-timed-out event so the state never
// flipped. State-flip + arm listener; NO Close (Conn is already
// disconnected). HA-defer applies. v0.7.1: capture connID+peerLog BEFORE
// unlock to use the real activity.Manager.RemovePeer signature.
func (m *Manager) recoverInactivityStuck(ctx context.Context, pubKey string, stuckBatch map[string]struct{}) {
	m.managedPeersMu.Lock()
	cfg, ok := m.managedPeers[pubKey]
	if !ok {
		m.managedPeersMu.Unlock()
		return
	}
	mp, ok := m.managedPeersByConnID[cfg.PeerConnID]
	if !ok {
		m.managedPeersMu.Unlock()
		return
	}
	if mp.expectedWatcher != watcherInactivity {
		m.managedPeersMu.Unlock()
		return
	}
	if m.shouldDeferIdleForHA(stuckBatch, mp.peerCfg.PublicKey) {
		mp.peerCfg.Log.Infof("watchdog: defer inactivity-stuck recovery (HA peers active, batch=%d)", len(stuckBatch))
		m.managedPeersMu.Unlock()
		return
	}
	connID := cfg.PeerConnID
	peerLog := cfg.Log
	m.transitionToActivityWatcherStateOnly(mp)
	m.managedPeersMu.Unlock()

	m.armActivityListener(mp)
	callTestListenerArmHook(pubKey)

	if !m.peerStillManaged(pubKey, connID) {
		m.activityManager.RemovePeer(peerLog, connID)
		return
	}
	peerLog.Infof("watchdog: recovery complete (inactivity-stuck: watcherInactivity -> watcherActivity, listener armed)")
}

// recoverActivityNoListener handles Case-b: peer in watcherActivity but no
// Activity-Listener registered. This is the post-Close-hang state.
// Recovery: arm listener only. No state mutation needed (already
// watcherActivity). No HA-defer (peer is already "wanted active").
func (m *Manager) recoverActivityNoListener(ctx context.Context, pubKey string) {
	m.managedPeersMu.Lock()
	cfg, ok := m.managedPeers[pubKey]
	if !ok {
		m.managedPeersMu.Unlock()
		return
	}
	mp, ok := m.managedPeersByConnID[cfg.PeerConnID]
	if !ok {
		m.managedPeersMu.Unlock()
		return
	}
	if mp.expectedWatcher != watcherActivity {
		m.managedPeersMu.Unlock()
		return
	}
	connID := cfg.PeerConnID
	peerLog := cfg.Log
	m.managedPeersMu.Unlock()

	if m.activityManager.HasPeer(connID) {
		return
	}
	m.armActivityListener(mp)
	callTestListenerArmHook(pubKey)

	if !m.peerStillManaged(pubKey, connID) {
		m.activityManager.RemovePeer(peerLog, connID)
		return
	}
	peerLog.Infof("watchdog: recovery complete (activity-no-listener: listener re-armed for peer in watcherActivity)")
}

// spawnRecovery wraps a recovery function in an inflight-dedupe lock cycle
// + panic-recovery. Caller passes the actual recovery work as fn.
func (m *Manager) spawnRecovery(ctx context.Context, pubKey string,
	recoveringPeers map[string]struct{}, recoveringMu *sync.Mutex,
	fn func(string)) {
	recoveringMu.Lock()
	if _, inflight := recoveringPeers[pubKey]; inflight {
		recoveringMu.Unlock()
		return
	}
	recoveringPeers[pubKey] = struct{}{}
	recoveringMu.Unlock()
	go func() {
		defer func() {
			recoveringMu.Lock()
			delete(recoveringPeers, pubKey)
			recoveringMu.Unlock()
			if r := recover(); r != nil {
				log.Errorf("lazyconn watchdog: recovery panic for %s: %v", pubKey, r)
			}
		}()
		fn(pubKey)
	}()
}

// reconcileTick performs a single watchdog pass:
//   - PHASE A: lock-free atomic read of inactivity DropCounters.
//   - PHASE B: short managedPeersMu snapshot of all peers.
//   - PHASE C: per-peer classification via TransportSnapshot + HasPeer.
//   - PHASE D: bounded async recovery goroutine per stuck peer.
//
// The watchdog NEVER calls PeerConnIdle/Conn.Close — that's the v0.4
// BLOCKER deadlock path between conn.mu and managedPeersMu.
func (m *Manager) reconcileTick(ctx context.Context, lastRelayDrops, lastICEDrops *uint64,
	recoveringPeers map[string]struct{}, recoveringMu *sync.Mutex) {

	// PHASE A: atomic counter read (no lock)
	relayDrops, iceDrops := m.inactivityManager.DropCounters()
	deltaRelay := relayDrops - *lastRelayDrops
	deltaICE := iceDrops - *lastICEDrops
	*lastRelayDrops, *lastICEDrops = relayDrops, iceDrops

	// PHASE B: snapshot ALL peers (state + connID + expectedWatcher)
	type peerSnap struct {
		pubKey          string
		connID          peerid.ConnID
		expectedWatcher watcherType
	}
	m.managedPeersMu.Lock()
	snaps := make([]peerSnap, 0, len(m.managedPeersByConnID))
	for connID, mp := range m.managedPeersByConnID {
		snaps = append(snaps, peerSnap{
			pubKey:          mp.peerCfg.PublicKey,
			connID:          connID,
			expectedWatcher: mp.expectedWatcher,
		})
	}
	m.managedPeersMu.Unlock()

	// PHASE C: per-peer transport-state + listener-state classification
	stuckInactivityBatch := make(map[string]struct{})
	stuckActivityNoListener := make(map[string]struct{})
	for _, s := range snaps {
		conn, ok := m.peerStore.PeerConn(s.pubKey)
		if !ok {
			continue
		}
		iceDisc, relayDisc := conn.TransportSnapshot()
		if !iceDisc || !relayDisc {
			continue
		}
		switch s.expectedWatcher {
		case watcherInactivity:
			if isStuckPeer(iceDisc, relayDisc, deltaRelay) {
				stuckInactivityBatch[s.pubKey] = struct{}{}
			}
		case watcherActivity:
			if !m.activityManager.HasPeer(s.connID) {
				stuckActivityNoListener[s.pubKey] = struct{}{}
			}
		}
	}
	total := len(stuckInactivityBatch) + len(stuckActivityNoListener)
	if total == 0 {
		return
	}

	log.Warnf("lazyconn watchdog: %d stuck peers (inactivity-stuck=%d relayDrops=%d, activity-no-listener=%d) — ICE-drops=%d telemetry only",
		total, len(stuckInactivityBatch), deltaRelay, len(stuckActivityNoListener), deltaICE)

	// PHASE D: spawn bounded async recovery per peer
	for pubKey := range stuckInactivityBatch {
		batch := stuckInactivityBatch
		m.spawnRecovery(ctx, pubKey, recoveringPeers, recoveringMu, func(pk string) {
			m.recoverInactivityStuck(ctx, pk, batch)
		})
	}
	for pubKey := range stuckActivityNoListener {
		m.spawnRecovery(ctx, pubKey, recoveringPeers, recoveringMu, func(pk string) {
			m.recoverActivityNoListener(ctx, pk)
		})
	}
}

// runReconcileWatchdog is the long-lived watchdog goroutine. Self-
// restarts on panic so a downstream bug cannot silently take the
// watchdog offline. interval is parameterized so integration tests
// can drive it faster than the 120 s production default.
func (m *Manager) runReconcileWatchdog(ctx context.Context, interval time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("lazyconn watchdog: panic, restart loop: %v", r)
			go m.runReconcileWatchdog(ctx, interval)
		}
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastRelayDrops, lastICEDrops uint64
	var recoveringMu sync.Mutex
	recoveringPeers := make(map[string]struct{})

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcileTick(ctx, &lastRelayDrops, &lastICEDrops, recoveringPeers, &recoveringMu)
		}
	}
}
