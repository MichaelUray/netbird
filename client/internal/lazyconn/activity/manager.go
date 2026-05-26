package activity

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/netbirdio/netbird/client/iface/wgaddr"
	"github.com/netbirdio/netbird/client/internal/lazyconn"
	peerid "github.com/netbirdio/netbird/client/internal/peer/id"
)

// readResult tells the activity-manager why a listener's ReadPackets
// returned. Without this distinction the manager cannot tell a real
// activity edge apart from a close/cancel-driven return, which on
// Android led to silently dropped activity events under network-change
// pressure (Phase 3.7j-android-lazyconn-fix: production-reproduced on
// Samsung Galaxy S21 + S24+).
type readResult int

const (
	// readClosed indicates the listener exited because Close was called
	// (or its context was cancelled). No activity occurred; the
	// activity-manager MUST NOT publish a notification.
	readClosed readResult = iota

	// readActivity indicates the listener observed real transport
	// activity. The activity-manager MUST publish a notification on
	// OnActivityChan, even if the per-peer map-entry has already been
	// removed by a concurrent RemovePeer call.
	readActivity
)

// listener defines the contract for activity detection listeners.
type listener interface {
	// ReadPackets blocks until either real transport activity is
	// observed (returns readActivity) or the listener is closed /
	// cancelled (returns readClosed). The return value MUST reflect
	// the actual wake reason so that the activity-manager can
	// distinguish activity from teardown.
	ReadPackets() readResult
	Close()
}

type WgInterface interface {
	RemovePeer(peerKey string) error
	UpdatePeer(peerKey string, allowedIps []netip.Prefix, keepAlive time.Duration, endpoint *net.UDPAddr, preSharedKey *wgtypes.Key) error
	IsUserspaceBind() bool
	Address() wgaddr.Address
}

type Manager struct {
	OnActivityChan chan peerid.ConnID

	wgIface WgInterface

	peers map[peerid.ConnID]listener
	done  chan struct{}

	mu sync.Mutex
}

func NewManager(wgIface WgInterface) *Manager {
	m := &Manager{
		OnActivityChan: make(chan peerid.ConnID, 1),
		wgIface:        wgIface,
		peers:          make(map[peerid.ConnID]listener),
		done:           make(chan struct{}),
	}
	return m
}

func (m *Manager) MonitorPeerActivity(peerCfg lazyconn.PeerConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.peers[peerCfg.PeerConnID]; ok {
		log.Warnf("activity listener already exists for: %s", peerCfg.PublicKey)
		return nil
	}

	listener, err := m.createListener(peerCfg)
	if err != nil {
		return err
	}

	m.peers[peerCfg.PeerConnID] = listener
	go m.waitForTraffic(listener, peerCfg.PeerConnID)
	return nil
}

func (m *Manager) createListener(peerCfg lazyconn.PeerConfig) (listener, error) {
	if !m.wgIface.IsUserspaceBind() {
		return NewUDPListener(m.wgIface, peerCfg)
	}

	provider, ok := m.wgIface.(bindProvider)
	if !ok {
		return nil, errors.New("interface claims userspace bind but doesn't implement bindProvider")
	}

	return NewBindListener(m.wgIface, provider.GetBind(), peerCfg)
}

func (m *Manager) RemovePeer(log *log.Entry, peerConnID peerid.ConnID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	listener, ok := m.peers[peerConnID]
	if !ok {
		return
	}
	log.Debugf("removing activity listener")
	delete(m.peers, peerConnID)
	listener.Close()
}

// HasPeer reports whether an activity listener is currently registered
// for the given peer connection ID. Intended for the lazyconn-Manager
// watchdog (Phase 3.7i) to distinguish "active and listening" from
// "active but stuck after hung Close".
func (m *Manager) HasPeer(connID peerid.ConnID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.peers[connID]
	return ok
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	close(m.done)
	for peerID, listener := range m.peers {
		delete(m.peers, peerID)
		listener.Close()
	}
}

func (m *Manager) waitForTraffic(l listener, peerConnID peerid.ConnID) {
	result := l.ReadPackets()
	if result != readActivity {
		// Listener exited via Close/cancel. No real activity to report.
		// The peer map-entry, if any, is owned by whoever triggered the
		// close (Manager.Close, Manager.RemovePeer); they handled the
		// delete. Returning here without notify preserves the "close
		// does not synthesise activity" invariant.
		return
	}

	// Real activity was observed. The notification edge MUST NOT be
	// dropped just because the per-peer map-entry was concurrently
	// removed (Phase 3.7j-android-lazyconn-fix). We still delete the
	// entry idempotently in case we own it, so subsequent listener
	// teardown is a no-op against m.peers.
	m.mu.Lock()
	_, wasRegistered := m.peers[peerConnID]
	delete(m.peers, peerConnID)
	m.mu.Unlock()

	if !wasRegistered {
		// Debug only: a concurrent removal beat us to the map entry but
		// real activity arrived nonetheless. Kept at debug-level so
		// production logs are not noisy; intended for Android
		// validation builds and post-mortem analysis of the race.
		log.Debugf("activity observed after listener-map entry was already removed for %v", peerConnID)
	}

	m.notify(peerConnID)
}

func (m *Manager) notify(peerConnID peerid.ConnID) {
	// Debug-level diagnostic: the production-reproduced bug could
	// theoretically also manifest as a blocked notify (OnActivityChan
	// is buffer-1). before/after notify logs let the Android validation
	// build distinguish blocked-delivery from the lost-edge race.
	log.Debugf("waitForTraffic: notify enter peerConnID=%v chanLen=%d", peerConnID, len(m.OnActivityChan))
	select {
	case <-m.done:
	case m.OnActivityChan <- peerConnID:
	}
	log.Debugf("waitForTraffic: notify return peerConnID=%v", peerConnID)
}
