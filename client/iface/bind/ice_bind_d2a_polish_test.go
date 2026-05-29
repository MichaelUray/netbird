package bind

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	wgConn "golang.zx2c4.com/wireguard/conn"
)

// Phase 3.7l (Fix-D D2a) polish — Codex 2026-05-29 review followups.
//
// The initial D2a patch checked only bufs[0] for isTransportPkg(); the
// polish patch scans the full batch so a mixed {handshake, transport}
// send still records activity. This file pins that behaviour.

// TestICEBind_Send_MixedBatch_HandshakeFirst_TransportLater verifies
// the Codex polish concern: WireGuard can batch a handshake frame
// followed by transport payload in the SAME Send() call (UDP_SEGMENT
// GSO, roam events, etc.). Pre-polish code only looked at bufs[0] and
// would miss the activity edge. Polish must scan the whole batch.
func TestICEBind_Send_MixedBatch_HandshakeFirst_TransportLater(t *testing.T) {
	fx := newD2AFixture(t)

	bufs := [][]byte{
		makeWGHandshakePacket(),  // type=1 first
		makeWGTransportPacket(),  // type=4 second
	}
	err := fx.bind.Send(bufs, wgConn.Endpoint(fx.endpoint))
	require.NoError(t, err)

	select {
	case pk := <-fx.activityPeerCh:
		assert.Equal(t, "test-d2a-peer-pubkey", pk, "mixed batch must still surface activity for the transport frame")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("activity callback did not fire on mixed batch (Codex polish regression)")
	}
}

// TestICEBind_Send_MixedBatch_TransportFirst_HandshakeLater is the
// symmetric case: transport first, then handshake. Activity must
// still fire (and only once, the loop breaks after first transport
// match).
func TestICEBind_Send_MixedBatch_TransportFirst_HandshakeLater(t *testing.T) {
	fx := newD2AFixture(t)
	bufs := [][]byte{
		makeWGTransportPacket(),
		makeWGHandshakePacket(),
	}
	err := fx.bind.Send(bufs, wgConn.Endpoint(fx.endpoint))
	require.NoError(t, err)

	select {
	case <-fx.activityPeerCh:
		// Expected.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("activity callback did not fire on transport-first batch")
	}
}

// TestICEBind_Send_AllHandshakeBatch_NoActivity is the negative case:
// a pure handshake/keepalive batch must not produce an activity edge.
func TestICEBind_Send_AllHandshakeBatch_NoActivity(t *testing.T) {
	fx := newD2AFixture(t)
	bufs := [][]byte{
		makeWGHandshakePacket(),
		makeWGHandshakePacket(),
	}
	err := fx.bind.Send(bufs, wgConn.Endpoint(fx.endpoint))
	require.NoError(t, err)

	select {
	case pk := <-fx.activityPeerCh:
		t.Fatalf("activity fired for all-handshake batch (peer=%s); expected silence", pk)
	case <-time.After(150 * time.Millisecond):
		// Expected.
	}
}

// TestICEBind_Send_MixedBatch_OnlyOneActivityFire verifies that even
// when MULTIPLE transport packets are in the batch, exactly ONE
// activity callback fires (the polish loop breaks on first match).
// This guards against an accidental N-fire-per-batch regression that
// would defeat the saveFrequency rate-limit.
func TestICEBind_Send_MixedBatch_OnlyOneActivityFire(t *testing.T) {
	fx := newD2AFixture(t)
	// Burst of transport packets in one Send. The loop must break on
	// first transport hit, so a single call to recorder.record is made.
	bufs := [][]byte{
		makeWGTransportPacket(),
		makeWGTransportPacket(),
		makeWGTransportPacket(),
		makeWGTransportPacket(),
	}
	err := fx.bind.Send(bufs, wgConn.Endpoint(fx.endpoint))
	require.NoError(t, err)

	// We expect exactly ONE channel send (the recorder's CompareAndSwap
	// rate-limit also guards multi-fires, but the loop break is the
	// real test contract here).
	fires := 0
	deadline := time.After(200 * time.Millisecond)
collect:
	for {
		select {
		case <-fx.activityPeerCh:
			fires++
		case <-deadline:
			break collect
		}
	}
	assert.Equal(t, 1, fires, "exactly one activity fire per Send call expected, got %d", fires)
}

// TestICEBind_Send_NonStdEndpoint_NoPanic guards against the type-
// assertion in the D2a path: if WG passes a custom Endpoint impl
// (not *StdNetEndpoint), the assertion fails and the path must
// short-circuit without panic.
func TestICEBind_Send_NonStdEndpoint_NoPanic(t *testing.T) {
	fx := newD2AFixture(t)
	// We can't pass a non-StdNetEndpoint to b.endpoints[ep.DstIP()]
	// directly (Send() looks up by IP, then writes to the registered
	// conn). But we CAN exercise the assertion path by constructing a
	// fake non-StdNetEndpoint that resolves to the same fake-IP. Use
	// a custom type implementing wgConn.Endpoint.
	customEP := &nonStdTestEndpoint{addr: fx.relayAddrPort.Addr()}
	bufs := [][]byte{makeWGTransportPacket()}
	// Must not panic. Activity must NOT fire (non-StdNetEndpoint cannot
	// surface AddrPort via the type assertion).
	err := fx.bind.Send(bufs, wgConn.Endpoint(customEP))
	require.NoError(t, err)

	select {
	case pk := <-fx.activityPeerCh:
		t.Fatalf("activity fired for non-StdNetEndpoint (peer=%s); expected silence", pk)
	case <-time.After(100 * time.Millisecond):
		// Expected.
	}
}

// nonStdTestEndpoint is a wgConn.Endpoint impl that is NOT
// *StdNetEndpoint. Used to exercise the type-assertion branch.
type nonStdTestEndpoint struct {
	addr netip.Addr
}

func (e *nonStdTestEndpoint) ClearSrc()                  {}
func (e *nonStdTestEndpoint) SrcToString() string        { return "" }
func (e *nonStdTestEndpoint) DstToString() string        { return e.addr.String() }
func (e *nonStdTestEndpoint) DstToBytes() []byte         { return nil }
func (e *nonStdTestEndpoint) DstIP() netip.Addr          { return e.addr }
func (e *nonStdTestEndpoint) SrcIP() netip.Addr          { return netip.Addr{} }
