package bind

import (
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	wgConn "golang.zx2c4.com/wireguard/conn"
)

// Phase 3.7l (Fix-D D2a) regression tests for ICEBind.Send relay-path
// activity recording.
//
// Before D2a, the activity-recorder was triggered ONLY by inbound traffic
// (Recv path). When a peer is stuck on Relay and gets no replies, no
// inbound traffic ⇒ AttachICEOnRelayActivity never fires ⇒ no ICE-retry
// triggered by user pings. D2a closes that loop on the Send side.

// makeWGTransportPacket builds a minimal byte buffer that passes
// isTransportPkg: type=4 (uint32 little-endian) + at least 33 bytes total.
func makeWGTransportPacket() []byte {
	buf := make([]byte, 64)
	binary.LittleEndian.PutUint32(buf[:4], 4)
	return buf
}

// makeWGHandshakePacket builds a non-transport WG handshake packet
// (type=1, init handshake). isTransportPkg should return false for it.
func makeWGHandshakePacket() []byte {
	buf := make([]byte, 64)
	binary.LittleEndian.PutUint32(buf[:4], 1)
	return buf
}

// fakeRelayConn implements net.Conn just enough for ICEBind.Send to call
// Write on it without touching real networking. Writes are counted so we
// can assert that they really happened.
type fakeRelayConn struct {
	writes atomic.Int32
}

func (c *fakeRelayConn) Read(b []byte) (int, error)         { return 0, nil }
func (c *fakeRelayConn) Write(b []byte) (int, error)        { c.writes.Add(1); return len(b), nil }
func (c *fakeRelayConn) Close() error                       { return nil }
func (c *fakeRelayConn) LocalAddr() net.Addr                { return &net.UDPAddr{} }
func (c *fakeRelayConn) RemoteAddr() net.Addr               { return &net.UDPAddr{} }
func (c *fakeRelayConn) SetDeadline(t time.Time) error      { return nil }
func (c *fakeRelayConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *fakeRelayConn) SetWriteDeadline(t time.Time) error { return nil }

// d2aFixture wires up an ICEBind with a registered relay-endpoint and an
// activity callback we can observe.
type d2aFixture struct {
	bind           *ICEBind
	relayAddrPort  netip.AddrPort
	endpoint       *Endpoint
	relayConn      *fakeRelayConn
	activityFires  atomic.Int32
	activityPeerCh chan string
}

func newD2AFixture(t *testing.T) *d2aFixture {
	t.Helper()
	b := setupICEBind(t)

	// Wire an activity callback so we can observe ActivityRecorder.record
	// outcomes. We also feed a known peer-key mapping for the fake-IP.
	const peerKey = "test-d2a-peer-pubkey"
	fakeAddr := netip.AddrPortFrom(netip.MustParseAddr("127.1.0.42"), 51820)
	b.ActivityRecorder().UpsertAddress(peerKey, fakeAddr)

	// UpsertAddress stamps LastActivity=now, so the very next record()
	// call would be silenced by the 5-second saveFrequency rate-limit.
	// Reset it to zero so tests fire on the first transport packet.
	b.ActivityRecorder().mu.Lock()
	if rec, ok := b.ActivityRecorder().addrToPeer[fakeAddr]; ok {
		rec.LastActivity.Store(0)
	}
	b.ActivityRecorder().mu.Unlock()

	ch := make(chan string, 16)
	b.ActivityRecorder().SetOnActivity(func(pk string) {
		ch <- pk
	})

	// Register a fake relay-conn at the same fakeIP that ICEBind.Send
	// will route over.
	relayConn := &fakeRelayConn{}
	b.endpointsMu.Lock()
	if b.endpoints == nil {
		b.endpoints = make(map[netip.Addr]net.Conn)
	}
	b.endpoints[fakeAddr.Addr()] = relayConn
	b.endpointsMu.Unlock()

	ep := &Endpoint{AddrPort: fakeAddr}

	return &d2aFixture{
		bind:           b,
		relayAddrPort:  fakeAddr,
		endpoint:       ep,
		relayConn:      relayConn,
		activityPeerCh: ch,
	}
}

// TestICEBind_Send_RelayTransportPkt_TriggersActivity verifies the core
// D2a contract: a WG transport packet sent over the relay-endpoint path
// records activity AND the registered onActivity callback fires.
func TestICEBind_Send_RelayTransportPkt_TriggersActivity(t *testing.T) {
	fx := newD2AFixture(t)

	bufs := [][]byte{makeWGTransportPacket()}
	err := fx.bind.Send(bufs, wgConn.Endpoint(fx.endpoint))
	require.NoError(t, err)

	// Activity callback must have fired exactly once for the relay-peer.
	select {
	case pk := <-fx.activityPeerCh:
		assert.Equal(t, "test-d2a-peer-pubkey", pk, "activity must surface the relay-peer's pubkey")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("activity callback did not fire within 500ms")
	}

	// The fake relay-conn must still have been written to (the original
	// Send semantics).
	assert.Equal(t, int32(1), fx.relayConn.writes.Load(), "fake relay-conn must have received exactly one Write")
}

// TestICEBind_Send_RelayHandshakePkt_NoActivity verifies a WG handshake
// (non-transport) packet does NOT trigger activity — we don't want to
// count handshakes / keepalives as user-activity for the ICE-retry
// heuristic.
func TestICEBind_Send_RelayHandshakePkt_NoActivity(t *testing.T) {
	fx := newD2AFixture(t)

	bufs := [][]byte{makeWGHandshakePacket()}
	err := fx.bind.Send(bufs, wgConn.Endpoint(fx.endpoint))
	require.NoError(t, err)

	// Still must have written to the relay-conn (handshakes go through).
	assert.Equal(t, int32(1), fx.relayConn.writes.Load(), "handshake must still be sent")

	// But the activity callback must NOT fire.
	select {
	case pk := <-fx.activityPeerCh:
		t.Fatalf("activity callback fired for handshake packet (peerKey=%s); expected silence", pk)
	case <-time.After(150 * time.Millisecond):
		// Expected: no activity event.
	}
}

// TestICEBind_Send_DirectPath_NoActivityFromOurPath verifies the D2a
// instrumentation only fires on the relay-endpoint path. Direct ICE-pair
// sends (no relay endpoint registered) go through StdNetBind.Send and do
// NOT trip the d2a recorder. (Note: the receive-side recorder may still
// emit activity for inbound packets — out of scope for this test.)
func TestICEBind_Send_DirectPath_NoActivityFromOurPath(t *testing.T) {
	fx := newD2AFixture(t)

	// Build an endpoint that's NOT registered in b.endpoints.
	otherEP := &Endpoint{AddrPort: netip.AddrPortFrom(netip.MustParseAddr("10.0.0.1"), 51820)}
	bufs := [][]byte{makeWGTransportPacket()}

	// StdNetBind.Send will fail (no socket bound for direct UDP), that
	// is fine — we just need to confirm the d2a code path did NOT
	// activate the recorder. The recorder lookup table only knows about
	// the relay-fakeIP, so even if StdNetBind happened to succeed it
	// wouldn't fire OnActivity for our test peer.
	_ = fx.bind.Send(bufs, wgConn.Endpoint(otherEP))

	select {
	case pk := <-fx.activityPeerCh:
		t.Fatalf("activity fired on direct (non-relay) send path for peer %s — D2a leaked", pk)
	case <-time.After(100 * time.Millisecond):
		// Expected.
	}
}

// TestICEBind_Send_RelayTransportPkt_RateLimitedBySaveFrequency verifies
// that recorded activity is gated by ActivityRecorder.saveFrequency — a
// burst of sends fires onActivity AT MOST once until the window elapses.
// This is the existing receive-side semantic and D2a must inherit it
// (otherwise per-packet bursts would spam AttachICEOnRelayActivity).
func TestICEBind_Send_RelayTransportPkt_RateLimitedBySaveFrequency(t *testing.T) {
	fx := newD2AFixture(t)

	const burst = 20
	wg := sync.WaitGroup{}
	wg.Add(burst)
	for i := 0; i < burst; i++ {
		go func() {
			defer wg.Done()
			bufs := [][]byte{makeWGTransportPacket()}
			_ = fx.bind.Send(bufs, wgConn.Endpoint(fx.endpoint))
		}()
	}
	wg.Wait()

	// Drain whatever fired within a short window.
	deadline := time.After(250 * time.Millisecond)
	fires := 0
collect:
	for {
		select {
		case <-fx.activityPeerCh:
			fires++
		case <-deadline:
			break collect
		}
	}
	// Inside one saveFrequency window the recorder's CompareAndSwap
	// guards reduce a burst to ~1 fire. We tolerate >0 and require that
	// fires ≤ burst (sanity), but the strong assertion is that the
	// rate-limit really clamps the spam.
	assert.Greater(t, fires, 0, "at least one activity event expected")
	assert.LessOrEqual(t, fires, 2,
		"burst of %d transport packets must be rate-limited to ≤2 onActivity callbacks (saw %d)",
		burst, fires)
}
