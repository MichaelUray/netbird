package inactivity

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/client/internal/lazyconn"
	"github.com/netbirdio/netbird/monotime"
)

// raceMockWgInterface is a goroutine-safe mock that the race tests
// can hand to the Manager. The real iface implementation has its own
// internal synchronization; the bare-map mockWgInterface in
// manager_test.go is fine for serial tests but would itself race in
// parallel ones. We use a tiny RWMutex here purely so the only
// race the test surfaces is the one in inactivity.Manager.
type raceMockWgInterface struct {
	mu             sync.RWMutex
	lastActivities map[string]monotime.Time
}

func newRaceMockWgInterface() *raceMockWgInterface {
	return &raceMockWgInterface{lastActivities: make(map[string]monotime.Time)}
}

func (m *raceMockWgInterface) LastActivities() map[string]monotime.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]monotime.Time, len(m.lastActivities))
	for k, v := range m.lastActivities {
		out[k] = v
	}
	return out
}

func makeRacePeer(id string) *lazyconn.PeerConfig {
	return &lazyconn.PeerConfig{
		PublicKey: id,
		Log:       log.WithField("peer", id),
	}
}

// TestManager_AddRemoveCheckStatsRace_FakeTicker drives checkStats via
// the production Manager.Start path (using the fakeTickerMock override
// that already exists in manager_test.go) while four worker goroutines
// churn AddPeer/RemovePeer. With -race this must reliably surface the
// concurrent map access on Manager.interestedPeers/firstSeenAt before
// the mutex fix; after the fix it must run clean.
func TestManager_AddRemoveCheckStatsRace_FakeTicker(t *testing.T) {
	wgMock := newRaceMockWgInterface()

	fakeTick := make(chan time.Time, 1)
	prevTicker := newTicker
	newTicker = func(d time.Duration) Ticker {
		return &fakeTickerMock{CChan: fakeTick}
	}
	t.Cleanup(func() { newTicker = prevTicker })

	// relayTimeout=1ns guarantees that any peer present in
	// interestedPeers when checkStats runs will be classified as
	// relay-idle, so the production path exercises the read side.
	manager := NewManagerWithTwoTimers(wgMock, 0, time.Nanosecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go manager.Start(ctx)

	// Drain channels so notifyChan can deliver without blocking and
	// without losing ticks; we don't assert on contents here, the
	// race detector is the assertion.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-manager.InactivePeersChan():
			case <-manager.ICEInactiveChan():
			}
		}
	}()

	// Brief startup window so Start's goroutine is parked on the
	// ticker channel before we begin pumping.
	time.Sleep(10 * time.Millisecond)

	const workers = 4
	const iters = 500
	var wg sync.WaitGroup
	wg.Add(workers + 1)

	// Ticker pumper.
	go func() {
		defer wg.Done()
		for i := 0; i < iters*workers; i++ {
			select {
			case fakeTick <- time.Now():
			case <-ctx.Done():
				return
			}
		}
	}()

	// Add/Remove churn.
	for w := 0; w < workers; w++ {
		w := w
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				peerID := fmt.Sprintf("w%d-p%d", w, i%8)
				manager.AddPeer(makeRacePeer(peerID))
				manager.RemovePeer(peerID)
			}
		}()
	}

	wg.Wait()
	cancel()
	<-drainDone
}

// TestManager_AddRemoveCheckStatsRace_Direct hits the same race
// surgically without depending on Manager.Start at all: one goroutine
// hammers checkStats() while four others churn Add/Remove. This
// minimizes timing flakiness and gives the race detector the densest
// possible interleaving.
func TestManager_AddRemoveCheckStatsRace_Direct(t *testing.T) {
	wgMock := newRaceMockWgInterface()
	manager := NewManagerWithTwoTimers(wgMock, 0, time.Nanosecond)

	const workers = 4
	const iters = 1000
	var wg sync.WaitGroup
	wg.Add(workers + 1)

	// checkStats hammerer.
	go func() {
		defer wg.Done()
		for i := 0; i < iters*workers; i++ {
			if _, _, err := manager.checkStats(); err != nil {
				t.Errorf("checkStats returned error: %v", err)
				return
			}
		}
	}()

	for w := 0; w < workers; w++ {
		w := w
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				peerID := fmt.Sprintf("w%d-p%d", w, i%8)
				manager.AddPeer(makeRacePeer(peerID))
				manager.RemovePeer(peerID)
			}
		}()
	}

	wg.Wait()
}
