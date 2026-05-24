package inactivity

import (
	"context"
	"reflect"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"

	"github.com/netbirdio/netbird/client/internal/lazyconn"
	"github.com/netbirdio/netbird/monotime"
)

// Manager must not expose RelayInactiveChan(): the Phase-1 alias
// between relayInactiveChan and inactivePeersChan caused a race where
// lazyconn.Manager and ConnMgr both consumed the same buffered chan
// and only one received any given event. The fix is to remove the
// accessor entirely so the alias cannot be re-introduced.
func TestManager_HasNoRelayInactiveChanAccessor(t *testing.T) {
	m := NewManagerWithTwoTimers(&mockWgInterface{}, 0, time.Minute)
	if _, exists := reflect.TypeOf(m).MethodByName("RelayInactiveChan"); exists {
		t.Fatal("Manager.RelayInactiveChan must be removed (alias race regression risk)")
	}
}

type mockWgInterface struct {
	lastActivities map[string]monotime.Time
}

func (m *mockWgInterface) LastActivities() map[string]monotime.Time {
	return m.lastActivities
}

func TestPeerTriggersInactivity(t *testing.T) {
	peerID := "peer1"

	// Past activity must exceed DefaultInactivityThreshold (24 h after
	// the Phase-3.7i tuning) — pick 25 h for safety margin.
	wgMock := &mockWgInterface{
		lastActivities: map[string]monotime.Time{
			peerID: monotime.Time(int64(monotime.Now()) - int64(25*time.Hour)),
		},
	}

	fakeTick := make(chan time.Time, 1)
	newTicker = func(d time.Duration) Ticker {
		return &fakeTickerMock{CChan: fakeTick}
	}

	peerLog := log.WithField("peer", peerID)
	peerCfg := &lazyconn.PeerConfig{
		PublicKey: peerID,
		Log:       peerLog,
	}

	manager := NewManager(wgMock, nil)
	manager.AddPeer(peerCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the manager in a goroutine
	go manager.Start(ctx)

	// Send a tick to simulate time passage
	fakeTick <- time.Now()

	// Check if peer appears on inactivePeersChan
	select {
	case inactivePeers := <-manager.inactivePeersChan:
		assert.Contains(t, inactivePeers, peerID, "expected peer to be marked inactive")
	case <-time.After(1 * time.Second):
		t.Fatal("expected inactivity event, but none received")
	}
}

func TestPeerTriggersActivity(t *testing.T) {
	peerID := "peer1"

	wgMock := &mockWgInterface{
		lastActivities: map[string]monotime.Time{
			peerID: monotime.Time(int64(monotime.Now()) - int64(5*time.Minute)),
		},
	}

	fakeTick := make(chan time.Time, 1)
	newTicker = func(d time.Duration) Ticker {
		return &fakeTickerMock{CChan: fakeTick}
	}

	peerLog := log.WithField("peer", peerID)
	peerCfg := &lazyconn.PeerConfig{
		PublicKey: peerID,
		Log:       peerLog,
	}

	manager := NewManager(wgMock, nil)
	manager.AddPeer(peerCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the manager in a goroutine
	go manager.Start(ctx)

	// Send a tick to simulate time passage
	fakeTick <- time.Now()

	// Check if peer appears on inactivePeersChan
	select {
	case <-manager.inactivePeersChan:
		t.Fatal("expected inactive peer to be marked inactive")
	case <-time.After(1 * time.Second):
		// No inactivity event should be received
	}
}

// fakeTickerMock implements Ticker interface for testing
type fakeTickerMock struct {
	CChan chan time.Time
}

func (f *fakeTickerMock) C() <-chan time.Time {
	return f.CChan
}

func (f *fakeTickerMock) Stop() {}

// --- Phase 2 (#5989) two-timer tests ---

// makePeerCfg is a test helper for building a minimal PeerConfig with logger.
func makePeerCfg(peerID string) *lazyconn.PeerConfig {
	return &lazyconn.PeerConfig{
		PublicKey: peerID,
		Log:       log.WithField("peer", peerID),
	}
}

// pastActivity returns a monotime.Time corresponding to (now - d).
func pastActivity(d time.Duration) monotime.Time {
	return monotime.Time(int64(monotime.Now()) - int64(d))
}

func TestTwoTimers_OnlyICEFires(t *testing.T) {
	peerID := "peer1"

	// Peer idle for 6 minutes: above iceTimeout (5m), below relayTimeout (24h).
	wgMock := &mockWgInterface{
		lastActivities: map[string]monotime.Time{
			peerID: pastActivity(6 * time.Minute),
		},
	}

	fakeTick := make(chan time.Time, 1)
	newTicker = func(d time.Duration) Ticker {
		return &fakeTickerMock{CChan: fakeTick}
	}

	manager := NewManagerWithTwoTimers(wgMock, 5*time.Minute, 24*time.Hour)
	manager.AddPeer(makePeerCfg(peerID))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go manager.Start(ctx)

	fakeTick <- time.Now()

	select {
	case peers := <-manager.ICEInactiveChan():
		assert.Contains(t, peers, peerID, "expected peerID on ICE channel")
	case <-time.After(1 * time.Second):
		t.Fatal("expected ICE-inactive event, none received")
	}

	// Relay channel must NOT fire.
	select {
	case <-manager.InactivePeersChan():
		t.Fatal("Relay channel should not fire when only iceTimeout exceeded")
	case <-time.After(200 * time.Millisecond):
		// expected
	}
}

func TestTwoTimers_BothFire(t *testing.T) {
	peerID := "peer1"

	// Peer idle for 25h: above both iceTimeout (5m) and relayTimeout (24h).
	wgMock := &mockWgInterface{
		lastActivities: map[string]monotime.Time{
			peerID: pastActivity(25 * time.Hour),
		},
	}

	fakeTick := make(chan time.Time, 1)
	newTicker = func(d time.Duration) Ticker {
		return &fakeTickerMock{CChan: fakeTick}
	}

	manager := NewManagerWithTwoTimers(wgMock, 5*time.Minute, 24*time.Hour)
	manager.AddPeer(makePeerCfg(peerID))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go manager.Start(ctx)

	fakeTick <- time.Now()

	gotICE := false
	gotRelay := false
	deadline := time.After(1 * time.Second)
	for !gotICE || !gotRelay {
		select {
		case peers := <-manager.ICEInactiveChan():
			if _, ok := peers[peerID]; ok {
				gotICE = true
			}
		case peers := <-manager.InactivePeersChan():
			if _, ok := peers[peerID]; ok {
				gotRelay = true
			}
		case <-deadline:
			t.Fatalf("timeout waiting for both channels (gotICE=%v, gotRelay=%v)", gotICE, gotRelay)
		}
	}
}

func TestTwoTimers_ICEDisabled(t *testing.T) {
	peerID := "peer1"

	// iceTimeout=0 (disabled) + relayTimeout=10m, peer idle 11m -> only relay fires.
	wgMock := &mockWgInterface{
		lastActivities: map[string]monotime.Time{
			peerID: pastActivity(11 * time.Minute),
		},
	}

	fakeTick := make(chan time.Time, 1)
	newTicker = func(d time.Duration) Ticker {
		return &fakeTickerMock{CChan: fakeTick}
	}

	manager := NewManagerWithTwoTimers(wgMock, 0, 10*time.Minute)
	manager.AddPeer(makePeerCfg(peerID))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go manager.Start(ctx)

	fakeTick <- time.Now()

	select {
	case peers := <-manager.InactivePeersChan():
		assert.Contains(t, peers, peerID)
	case <-time.After(1 * time.Second):
		t.Fatal("relay channel should fire when relayTimeout exceeded")
	}

	// ICE channel must never fire because iceTimeout=0.
	select {
	case <-manager.ICEInactiveChan():
		t.Fatal("ICE channel should NEVER fire when iceTimeout=0")
	case <-time.After(200 * time.Millisecond):
		// expected
	}
}

func TestTwoTimers_RelayDisabled(t *testing.T) {
	peerID := "peer1"

	// iceTimeout=5m + relayTimeout=0, peer idle 6m -> only ICE fires.
	wgMock := &mockWgInterface{
		lastActivities: map[string]monotime.Time{
			peerID: pastActivity(6 * time.Minute),
		},
	}

	fakeTick := make(chan time.Time, 1)
	newTicker = func(d time.Duration) Ticker {
		return &fakeTickerMock{CChan: fakeTick}
	}

	manager := NewManagerWithTwoTimers(wgMock, 5*time.Minute, 0)
	manager.AddPeer(makePeerCfg(peerID))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go manager.Start(ctx)

	fakeTick <- time.Now()

	select {
	case peers := <-manager.ICEInactiveChan():
		assert.Contains(t, peers, peerID)
	case <-time.After(1 * time.Second):
		t.Fatal("ICE channel should fire when iceTimeout exceeded")
	}

	// Relay channel must never fire because relayTimeout=0.
	select {
	case <-manager.InactivePeersChan():
		t.Fatal("Relay channel should NEVER fire when relayTimeout=0")
	case <-time.After(200 * time.Millisecond):
		// expected
	}
}

func TestTwoTimers_BothDisabled(t *testing.T) {
	peerID := "peer1"

	wgMock := &mockWgInterface{
		lastActivities: map[string]monotime.Time{
			peerID: pastActivity(99 * time.Hour),
		},
	}

	fakeTick := make(chan time.Time, 1)
	newTicker = func(d time.Duration) Ticker {
		return &fakeTickerMock{CChan: fakeTick}
	}

	manager := NewManagerWithTwoTimers(wgMock, 0, 0)
	manager.AddPeer(makePeerCfg(peerID))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go manager.Start(ctx)

	fakeTick <- time.Now()

	// Neither channel should fire.
	select {
	case <-manager.ICEInactiveChan():
		t.Fatal("ICE channel must not fire when both disabled")
	case <-manager.InactivePeersChan():
		t.Fatal("Relay channel must not fire when both disabled")
	case <-time.After(300 * time.Millisecond):
		// expected
	}
}

// TestPhase1_LazyEquivalence verifies that the legacy NewManager constructor
// behaves identically to the Phase-1 single-timer code: peers cross the
// (single) inactivityThreshold and appear on InactivePeersChan, ICE
// channel never fires.
func TestPhase1_LazyEquivalence(t *testing.T) {
	peerID := "peer1"

	// DefaultInactivityThreshold is 24 h (Phase-3.7i tuning); use 25 h
	// of past activity so the test is robust to that constant changing
	// in either direction.
	wgMock := &mockWgInterface{
		lastActivities: map[string]monotime.Time{
			peerID: pastActivity(25 * time.Hour),
		},
	}

	fakeTick := make(chan time.Time, 1)
	newTicker = func(d time.Duration) Ticker {
		return &fakeTickerMock{CChan: fakeTick}
	}

	// Phase-1 entry point with default threshold.
	manager := NewManager(wgMock, nil)
	manager.AddPeer(makePeerCfg(peerID))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go manager.Start(ctx)

	fakeTick <- time.Now()

	// InactivePeersChan (Phase-1 alias of RelayInactiveChan) must fire.
	select {
	case peers := <-manager.InactivePeersChan():
		assert.Contains(t, peers, peerID)
	case <-time.After(1 * time.Second):
		t.Fatal("Phase-1 InactivePeersChan must fire (= RelayInactiveChan in Phase 2)")
	}

	// ICE channel must NEVER fire from Phase-1 entry point (iceTimeout=0).
	select {
	case <-manager.ICEInactiveChan():
		t.Fatal("ICE channel must not fire in Phase-1 NewManager mode")
	case <-time.After(200 * time.Millisecond):
		// expected
	}
}

func TestDropCounters_InitialZero(t *testing.T) {
	m := NewManagerWithTwoTimers(
		&mockWgInterface{lastActivities: map[string]monotime.Time{}},
		time.Second, time.Second)
	relay, ice := m.DropCounters()
	if relay != 0 || ice != 0 {
		t.Fatalf("expected (0,0) initial, got (%d,%d)", relay, ice)
	}
}

func TestNotifyChan_FullChannelIncrementsDropCounter(t *testing.T) {
	m := NewManagerWithTwoTimers(
		&mockWgInterface{lastActivities: map[string]monotime.Time{}},
		time.Second, time.Second)
	// Fill the channel (capacity is 1)
	m.inactivePeersChan <- map[string]struct{}{"pre-fill": {}}
	// Now trigger a drop
	m.notifyChan(context.Background(), m.inactivePeersChan, map[string]struct{}{"dropped": {}})
	relay, _ := m.DropCounters()
	if relay != 1 {
		t.Fatalf("expected relayDrops=1, got %d", relay)
	}
}

func TestDropCounters_RelayAndICESeparate(t *testing.T) {
	m := NewManagerWithTwoTimers(
		&mockWgInterface{lastActivities: map[string]monotime.Time{}},
		time.Second, time.Second)
	// Fill both channels
	m.inactivePeersChan <- map[string]struct{}{"r": {}}
	m.iceInactiveChan <- map[string]struct{}{"i": {}}
	m.notifyChan(context.Background(), m.inactivePeersChan, map[string]struct{}{"r2": {}})
	m.notifyChan(context.Background(), m.iceInactiveChan, map[string]struct{}{"i2": {}})
	m.notifyChan(context.Background(), m.iceInactiveChan, map[string]struct{}{"i3": {}})
	relay, ice := m.DropCounters()
	if relay != 1 || ice != 2 {
		t.Fatalf("expected (1,2), got (%d,%d)", relay, ice)
	}
}

// --- firstSeenAt tracking (preparing for orphan-peer disconnect) ---

// AddPeer must populate firstSeenAt and RemovePeer must clean it up,
// otherwise the map grows unbounded as peers churn through the lazy
// state machine.
func TestRemovePeer_CleansFirstSeenAt(t *testing.T) {
	iface := &mockWgInterface{lastActivities: map[string]monotime.Time{}}
	mgr := newManager(iface, 0, time.Minute)

	peerKey := "p1"
	mgr.AddPeer(&lazyconn.PeerConfig{PublicKey: peerKey, Log: log.WithField("peer", peerKey)})
	_, present := mgr.firstSeenAt[peerKey]
	assert.True(t, present, "AddPeer must populate firstSeenAt")

	mgr.RemovePeer(peerKey)
	_, present = mgr.firstSeenAt[peerKey]
	assert.False(t, present, "RemovePeer must clean up firstSeenAt")
}

// A fresh peer with no WG-stats entry (orphan) must not fire any timer
// within its karenzfrist. This holds both with the old continue-skip
// behavior and with the upcoming firstSeenAt-based fallback, so the
// test belongs to the refactor commit and stays green afterwards.
func TestCheckStats_OrphanPeerSilentBeforeTimeout(t *testing.T) {
	iface := &mockWgInterface{lastActivities: map[string]monotime.Time{}}
	mgr := newManager(iface, 0, 10*time.Minute)

	peerKey := "freshOrphan"
	mgr.AddPeer(&lazyconn.PeerConfig{PublicKey: peerKey, Log: log.WithField("peer", peerKey)})

	iceIdle, relayIdle, err := mgr.checkStats()
	assert.NoError(t, err)
	assert.Empty(t, iceIdle)
	assert.Empty(t, relayIdle, "must not fire within karenzfrist")
}
