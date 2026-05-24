package inactivity

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/client/internal/lazyconn"
	"github.com/netbirdio/netbird/monotime"
)

const (
	checkInterval = 1 * time.Minute

	// DefaultInactivityThreshold is the relay-tunnel idle-teardown
	// fallback when neither client config nor server-pushed value sets
	// it. Bumped 2026-05-03 from 15 min to 24 h: a 15-min window
	// triggered tear-down for peers that exchange traffic only
	// occasionally (e.g. NAT-keepalive only), forcing a full re-
	// handshake on every wake. 24 h matches the dashboard placeholder
	// and the production value most users actually want.
	DefaultInactivityThreshold = 24 * time.Hour
	MinimumInactivityThreshold = 1 * time.Minute
)

type WgInterface interface {
	LastActivities() map[string]monotime.Time
}

// Manager watches per-peer activity timestamps from the WireGuard
// interface and notifies via channels when peers cross inactivity
// thresholds.
//
// Phase 2 (#5989) introduced TWO independent thresholds per peer:
//   - iceTimeout fires the iceInactiveChan (consumer detaches the ICE
//     worker but keeps the relay-tunnel up).
//   - relayTimeout fires the inactivePeersChan (consumer tears down
//     the whole connection).
//
// Threshold == 0 disables that channel for all peers (the corresponding
// teardown never fires). Phase-1 p2p-lazy is expressed as
// iceTimeout=0 + relayTimeout=X.
//
// Phase-3.7i v0.5: the previous Manager carried a separate
// relayInactiveChan that was aliased to inactivePeersChan; both
// lazyconn.Manager (via InactivePeersChan) and ConnMgr.runDynamic
// InactivityLoop (via RelayInactiveChan) consumed the same buffered
// channel, and only one of them received any given event. Routing
// peers silently stuck in connected-but-stale state were the
// production fallout. The fix is single-ownership: lazyconn.Manager
// is the sole consumer of inactivePeersChan, and the public
// RelayInactiveChan accessor + its field are gone so the alias
// cannot re-form.
type Manager struct {
	iface WgInterface

	// Two-timer thresholds (Phase 2). Both 0 = manager is effectively
	// inert (peers register but no channel ever fires).
	iceTimeout   time.Duration
	relayTimeout time.Duration

	interestedPeers map[string]*lazyconn.PeerConfig

	// firstSeenAt records, per peer, the monotime when AddPeer was
	// called. Used by checkStats as a synthetic last-activity for
	// peers that never produced a WG-stats entry (no endpoint was
	// ever set on the WG interface — typically: ICE permanently
	// stuck in backoff). Without this fallback, such peers would
	// stay in interestedPeers forever, since the !ok branch in
	// checkStats would always skip them and relayTimeout would
	// never fire.
	firstSeenAt map[string]monotime.Time

	iceInactiveChan   chan map[string]struct{}
	inactivePeersChan chan map[string]struct{}

	// inactivityThreshold retained for the Phase-1 NewManager API
	// (mirrors relayTimeout). NewManagerWithTwoTimers no longer
	// relies on it.
	inactivityThreshold time.Duration

	// Counts silent drops on the two notify channels. Drops happen
	// when the consumer (lazyconn-Manager Start loop) is slow or
	// blocked, and indicate that a state-transition event was lost.
	// The lazy reconcile-watchdog reads these via DropCounters().
	notifyDropsRelay atomic.Uint64
	notifyDropsICE   atomic.Uint64
}

// NewManager is the Phase-1 single-timer constructor. Pass a *time.Duration
// to override the default DefaultInactivityThreshold; nil uses the default.
//
// Deprecated: use NewManagerWithTwoTimers. NewManager remains the entry
// point for callers that haven't been migrated; it constructs a manager
// with iceTimeout=0 (= ICE always-on, p2p-lazy semantics).
func NewManager(iface WgInterface, configuredThreshold *time.Duration) *Manager {
	threshold, err := validateInactivityThreshold(configuredThreshold)
	if err != nil {
		threshold = DefaultInactivityThreshold
		log.Warnf("invalid inactivity threshold configured: %v, using default: %v", err, DefaultInactivityThreshold)
	}

	log.Infof("inactivity threshold configured: %v", threshold)
	return newManager(iface, 0, threshold)
}

// NewManagerWithTwoTimers is the Phase-2 constructor. Pass 0 for either
// timeout to disable that teardown path. Both 0 leaves the manager
// running but inert (no channel ever fires) -- used by p2p / relay-forced
// modes that don't tear down workers.
func NewManagerWithTwoTimers(iface WgInterface, iceTimeout, relayTimeout time.Duration) *Manager {
	if iceTimeout > 0 {
		log.Infof("ICE inactivity timeout: %v", iceTimeout)
	}
	if relayTimeout > 0 {
		log.Infof("relay inactivity timeout: %v", relayTimeout)
	}
	return newManager(iface, iceTimeout, relayTimeout)
}

func newManager(iface WgInterface, iceTimeout, relayTimeout time.Duration) *Manager {
	return &Manager{
		iface:               iface,
		iceTimeout:          iceTimeout,
		relayTimeout:        relayTimeout,
		interestedPeers:     make(map[string]*lazyconn.PeerConfig),
		firstSeenAt:         make(map[string]monotime.Time),
		iceInactiveChan:     make(chan map[string]struct{}, 1),
		inactivePeersChan:   make(chan map[string]struct{}, 1),
		inactivityThreshold: relayTimeout,
	}
}

// InactivePeersChan is the single source-of-truth for whole-tunnel
// teardown events. lazyconn.Manager is the only consumer; ConnMgr no
// longer subscribes (it used to read the same channel via the now-
// removed RelayInactiveChan accessor, which created an aliasing race
// where one of the two consumers absorbed any given event).
func (m *Manager) InactivePeersChan() chan map[string]struct{} {
	if m == nil {
		// return a nil channel that blocks forever
		return nil
	}

	return m.inactivePeersChan
}

// ICEInactiveChan returns the channel that signals ICE-worker-only
// inactivity per peer (consumer typically calls Conn.DetachICE).
// Always returns a valid channel; if iceTimeout is 0, the channel
// just never fires.
func (m *Manager) ICEInactiveChan() chan map[string]struct{} {
	if m == nil {
		return nil
	}
	return m.iceInactiveChan
}

// DropCounters returns the cumulative count of dropped notifications
// per channel: relayDrops from inactivePeersChan, iceDrops from
// iceInactiveChan. Lock-free atomic load.
func (m *Manager) DropCounters() (relayDrops, iceDrops uint64) {
	return m.notifyDropsRelay.Load(), m.notifyDropsICE.Load()
}

// RecordRelayDropForTest is a test-only helper for the lazyconn/manager
// watchdog tests in another package. Bumps the relay-drop counter
// without going through notifyChan. NOT for production use.
func (m *Manager) RecordRelayDropForTest() {
	m.notifyDropsRelay.Add(1)
}

// RecordICEDropForTest is the ICE counterpart of RecordRelayDropForTest.
// Exported because Go does not support cross-package test-only exports.
func (m *Manager) RecordICEDropForTest() {
	m.notifyDropsICE.Add(1)
}

func (m *Manager) AddPeer(peerCfg *lazyconn.PeerConfig) {
	if m == nil {
		return
	}

	if _, exists := m.interestedPeers[peerCfg.PublicKey]; exists {
		return
	}

	peerCfg.Log.Infof("adding peer to inactivity manager")
	m.interestedPeers[peerCfg.PublicKey] = peerCfg
	m.firstSeenAt[peerCfg.PublicKey] = monotime.Now()
}

func (m *Manager) RemovePeer(peer string) {
	if m == nil {
		return
	}

	pi, ok := m.interestedPeers[peer]
	if !ok {
		return
	}

	pi.Log.Debugf("remove peer from inactivity manager")
	delete(m.interestedPeers, peer)
	delete(m.firstSeenAt, peer)
}

func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}

	ticker := newTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			iceIdle, relayIdle, err := m.checkStats()
			if err != nil {
				log.Errorf("error checking stats: %v", err)
				return
			}

			if len(iceIdle) > 0 {
				m.notifyChan(ctx, m.iceInactiveChan, iceIdle)
			}
			if len(relayIdle) > 0 {
				// Single owner (lazyconn.Manager). The Phase-1
				// RelayInactiveChan accessor is intentionally
				// removed; see TestManager_HasNoRelayInactiveChanAccessor.
				m.notifyChan(ctx, m.inactivePeersChan, relayIdle)
			}
		}
	}
}

func (m *Manager) notifyChan(ctx context.Context, ch chan map[string]struct{}, peers map[string]struct{}) {
	select {
	case ch <- peers:
	case <-ctx.Done():
		return
	default:
		var n uint64
		switch ch {
		case m.inactivePeersChan:
			n = m.notifyDropsRelay.Add(1)
		case m.iceInactiveChan:
			n = m.notifyDropsICE.Add(1)
		}
		// Throttle: log on 1st, 10th, 100th, then every 100 drops
		if n == 1 || n == 10 || (n >= 100 && n%100 == 0) {
			log.Warnf("inactivity: notify channel full, dropped %d-th event (peers in batch=%d). "+
				"Consumer may be slow or stuck — see lazyconn/manager.go state.",
				n, len(peers))
		}
		return
	}
}

// checkStats walks the per-peer activity-since values and groups peers
// into two sets:
//   - iceIdle: peers idle longer than iceTimeout (only populated when
//     iceTimeout > 0; otherwise this set is always empty)
//   - relayIdle: peers idle longer than relayTimeout (only populated
//     when relayTimeout > 0)
//
// Both sets are returned independently so consumers can act on each
// without coupling. A peer that has crossed both thresholds appears in
// both sets and the consumer is expected to handle them in order
// (first DetachICE on the iceIdle set, then full Close on the relayIdle
// set; the order is fine because Close on a peer where ICE is already
// detached is still correct).
func (m *Manager) checkStats() (iceIdle, relayIdle map[string]struct{}, err error) {
	lastActivities := m.iface.LastActivities()

	iceIdle = make(map[string]struct{})
	relayIdle = make(map[string]struct{})

	checkTime := time.Now()
	for peerID, peerCfg := range m.interestedPeers {
		lastActive, ok := lastActivities[peerID]
		if !ok {
			// No ActivityRecorder entry yet — no WG endpoint was
			// ever set for this peer. Normal short-lived state
			// while ICE negotiates, but with permanently failing
			// ICE (backoff stuck) the peer would stay in
			// interestedPeers forever without ever crossing
			// relayTimeout. Treat firstSeenAt as a synthetic
			// last-activity so the existing two-timer logic below
			// applies uniformly:
			//   Phase-1 (iceTimeout=0): fires relayIdle after relayTimeout.
			//   Phase-2 (iceTimeout>0): fires iceIdle, then relayIdle.
			seen, seenOK := m.firstSeenAt[peerID]
			if !seenOK {
				// Defensive: matches the existing no-sync
				// convention shared with interestedPeers. Skip
				// defensively on transient mismatch — the next
				// checkStats tick will see a consistent state.
				peerCfg.Log.Warnf("inactivity: peer in interestedPeers without firstSeenAt entry")
				continue
			}
			lastActive = seen
			// Fall through to the shared two-timer logic below.
		}

		since := monotime.Since(lastActive)

		if m.iceTimeout > 0 && since > m.iceTimeout {
			peerCfg.Log.Debugf("peer ICE idle since: %s", checkTime.Add(-since).String())
			iceIdle[peerID] = struct{}{}
		}
		if m.relayTimeout > 0 && since > m.relayTimeout {
			peerCfg.Log.Infof("peer relay idle since: %s", checkTime.Add(-since).String())
			relayIdle[peerID] = struct{}{}
		}
	}

	return iceIdle, relayIdle, nil
}

func validateInactivityThreshold(configuredThreshold *time.Duration) (time.Duration, error) {
	if configuredThreshold == nil {
		return DefaultInactivityThreshold, nil
	}
	if *configuredThreshold < MinimumInactivityThreshold {
		return 0, fmt.Errorf("configured inactivity threshold %v is too low, using %v", *configuredThreshold, MinimumInactivityThreshold)
	}
	return *configuredThreshold, nil
}
