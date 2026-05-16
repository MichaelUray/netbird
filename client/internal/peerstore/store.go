package peerstore

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/exp/maps"

	"github.com/netbirdio/netbird/client/internal/peer"
)

// Store is a thread-safe store for peer connections.
type Store struct {
	peerConns   map[string]*peer.Conn
	peerConnsMu sync.RWMutex
}

func NewConnStore() *Store {
	return &Store{
		peerConns: make(map[string]*peer.Conn),
	}
}

func (s *Store) AddPeerConn(pubKey string, conn *peer.Conn) bool {
	s.peerConnsMu.Lock()
	defer s.peerConnsMu.Unlock()

	_, ok := s.peerConns[pubKey]
	if ok {
		return false
	}

	s.peerConns[pubKey] = conn
	return true
}

func (s *Store) Remove(pubKey string) (*peer.Conn, bool) {
	s.peerConnsMu.Lock()
	defer s.peerConnsMu.Unlock()

	p, ok := s.peerConns[pubKey]
	if !ok {
		return nil, false
	}
	delete(s.peerConns, pubKey)
	return p, true
}

func (s *Store) AllowedIPs(pubKey string) ([]netip.Prefix, bool) {
	s.peerConnsMu.RLock()
	defer s.peerConnsMu.RUnlock()

	p, ok := s.peerConns[pubKey]
	if !ok {
		return nil, false
	}
	return p.WgConfig().AllowedIps, true
}

func (s *Store) AllowedIP(pubKey string) (netip.Addr, bool) {
	s.peerConnsMu.RLock()
	defer s.peerConnsMu.RUnlock()

	p, ok := s.peerConns[pubKey]
	if !ok {
		return netip.Addr{}, false
	}
	return p.AllowedIP(), true
}

func (s *Store) PeerConn(pubKey string) (*peer.Conn, bool) {
	s.peerConnsMu.RLock()
	defer s.peerConnsMu.RUnlock()

	p, ok := s.peerConns[pubKey]
	if !ok {
		return nil, false
	}
	return p, true
}

func (s *Store) PeerConnOpen(ctx context.Context, pubKey string) {
	s.peerConnsMu.RLock()
	defer s.peerConnsMu.RUnlock()

	p, ok := s.peerConns[pubKey]
	if !ok {
		return
	}
	// this can be blocked because of the connect open limiter semaphore
	if err := p.Open(ctx); err != nil {
		p.Log.Errorf("failed to open peer connection: %v", err)
	}

}

// PeerConnOpenUserInitiated is the activity-driven counterpart of
// PeerConnOpen. After Open succeeds it also drives a user-initiated
// AttachICE bypass so a peer parked on the long ICE-failure backoff
// gets a fresh ICE attempt when the user actually generates traffic.
//
// Phase 3.7i (#5989). userInitiatedCooldown rate-limits the bypass to
// at most one fresh attempt per window, regardless of how many activity
// edges fire from the lazyconn manager.
//
// Note (build/production-watchdog 2026-05-16): the lazyconn manager's
// activity-trigger path on the backup base does its own hard
// ResetIceBackoff + AttachICE + NotifyGuardActivity sequence which is
// a superset of this bypass, so this entrypoint is currently kept for
// API completeness / future callers and is not invoked from
// manager.onPeerActivity.
func (s *Store) PeerConnOpenUserInitiated(ctx context.Context, pubKey string, userInitiatedCooldown time.Duration) {
	s.peerConnsMu.RLock()
	defer s.peerConnsMu.RUnlock()

	p, ok := s.peerConns[pubKey]
	if !ok {
		return
	}
	if err := p.Open(ctx); err != nil {
		p.Log.Errorf("failed to open peer connection: %v", err)
		// Even if Open returned an error (idempotent no-op when already
		// opened still returns nil), fall through to the AttachICE path
		// only when Open succeeded. The error path above already logged.
		return
	}
	if err := p.AttachICEUserInitiated(userInitiatedCooldown); err != nil {
		p.Log.Debugf("AttachICEUserInitiated: %v", err)
	}
}

// PeerConnIdle is invoked by the lazy-manager when a peer's idle
// timer expires (relay-inactivity in p2p-lazy / p2p-dynamic). The
// connection is suspended but the WG peer entry stays so any
// route-manager-applied AllowedIPs (advertised subnets) survive the
// wake/sleep cycle. See docs/bugs/2026-05-04-lazy-wake-on-routed-
// subnet.md.
func (s *Store) PeerConnIdle(pubKey string) {
	s.peerConnsMu.RLock()
	defer s.peerConnsMu.RUnlock()

	p, ok := s.peerConns[pubKey]
	if !ok {
		return
	}
	p.Close(true, true)
}

// PeerConnClose is invoked by the lazy-manager when a peer must be
// closed without notifying the remote side (e.g. excluded from lazy on
// re-evaluation). Same lazy-suspend semantics: keep the WG peer entry.
func (s *Store) PeerConnClose(pubKey string) {
	s.peerConnsMu.RLock()
	defer s.peerConnsMu.RUnlock()

	p, ok := s.peerConns[pubKey]
	if !ok {
		return
	}
	p.Close(false, true)
}

func (s *Store) PeersPubKey() []string {
	s.peerConnsMu.RLock()
	defer s.peerConnsMu.RUnlock()

	return maps.Keys(s.peerConns)
}
