package bind

import (
	"encoding/binary"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// stubWakeArmer is a minimal WakeIntentArmer for tests — counts
// invocations and records the last payloadSize.
type stubWakeArmer struct {
	count    atomic.Int32
	lastSize atomic.Int32
}

func (s *stubWakeArmer) ArmLocalWakeIntent(payloadSize int) {
	s.count.Add(1)
	s.lastSize.Store(int32(payloadSize))
}

// makeV18_19TransportPacket builds a minimal WG transport packet: 4-byte
// header (type=4 little-endian) + payload to reach the requested total
// size. Used to exercise the V18.6 size>32 + transport-type filter.
func makeV18_19TransportPacket(totalSize int) []byte {
	if totalSize < 4 {
		totalSize = 4
	}
	buf := make([]byte, totalSize)
	// type=4 (transport) little-endian uint32
	binary.LittleEndian.PutUint32(buf[:4], 4)
	return buf
}

// v18_19DiscardConn is a net.Conn that discards all writes — sufficient
// for SetEndpoint dispatch in tests where the relay write target is
// irrelevant; we only assert on ArmLocalWakeIntent side-effects.
type v18_19DiscardConn struct{}

func (v18_19DiscardConn) Read(b []byte) (int, error)          { return 0, nil }
func (v18_19DiscardConn) Write(b []byte) (int, error)         { return len(b), nil }
func (v18_19DiscardConn) Close() error                        { return nil }
func (v18_19DiscardConn) LocalAddr() net.Addr                 { return &net.UDPAddr{} }
func (v18_19DiscardConn) RemoteAddr() net.Addr                { return &net.UDPAddr{} }
func (v18_19DiscardConn) SetDeadline(_ time.Time) error       { return nil }
func (v18_19DiscardConn) SetReadDeadline(_ time.Time) error   { return nil }
func (v18_19DiscardConn) SetWriteDeadline(_ time.Time) error  { return nil }

// TestICEBind_V18_19_ArmIntent_FiresOnRealPayload verifies that a
// transport packet > 32 B routed via the ok-path triggers
// ArmLocalWakeIntent exactly once per Send batch.
func TestICEBind_V18_19_ArmIntent_FiresOnRealPayload(t *testing.T) {
	b := setupICEBind(t)
	fakeIP := netip.MustParseAddr("127.2.158.151")
	armer := &stubWakeArmer{}
	b.SetEndpoint(fakeIP, v18_19DiscardConn{}, armer)

	pkt := makeV18_19TransportPacket(148) // > 32 B, type 4
	ep := &Endpoint{AddrPort: netip.AddrPortFrom(fakeIP, 51820)}

	if err := b.Send([][]byte{pkt}, ep); err != nil {
		t.Fatalf("V18.19: Send returned unexpected error: %v", err)
	}

	if got := armer.count.Load(); got != 1 {
		t.Fatalf("V18.19: real payload (148 B) MUST arm intent exactly once, got count=%d", got)
	}
	if got := armer.lastSize.Load(); got != 148 {
		t.Fatalf("V18.19: arm must be called with payloadSize=148, got %d", got)
	}
}

// TestICEBind_V18_19_ArmIntent_DoesNotFireOnKeepalive verifies the
// V18.6 size filter: a 32-byte WG keepalive must NOT arm intent
// (this is the test G2 negative case from the plan).
func TestICEBind_V18_19_ArmIntent_DoesNotFireOnKeepalive(t *testing.T) {
	b := setupICEBind(t)
	fakeIP := netip.MustParseAddr("127.2.158.151")
	armer := &stubWakeArmer{}
	b.SetEndpoint(fakeIP, v18_19DiscardConn{}, armer)

	keepalive := makeV18_19TransportPacket(32) // exactly 32 B = WG keepalive
	ep := &Endpoint{AddrPort: netip.AddrPortFrom(fakeIP, 51820)}
	_ = b.Send([][]byte{keepalive}, ep)

	if got := armer.count.Load(); got != 0 {
		t.Fatalf("V18.19: WG keepalive (32 B) must NOT arm intent (V18.6 filter), got count=%d", got)
	}
}

// TestICEBind_V18_19_NoArmer_NoCrash verifies that SetEndpoint with
// nil armer (pre-V18.19 caller) does not crash Send.
func TestICEBind_V18_19_NoArmer_NoCrash(t *testing.T) {
	b := setupICEBind(t)
	fakeIP := netip.MustParseAddr("127.2.158.151")
	b.SetEndpoint(fakeIP, v18_19DiscardConn{}, nil)

	pkt := makeV18_19TransportPacket(148)
	ep := &Endpoint{AddrPort: netip.AddrPortFrom(fakeIP, 51820)}
	if err := b.Send([][]byte{pkt}, ep); err != nil {
		t.Fatalf("V18.19: Send with nil armer returned unexpected error: %v", err)
	}
	// Test passes if no panic.
}

// TestICEBind_V18_19_RemoveEndpoint_CleansArmer verifies that
// RemoveEndpoint clears the wake-armer map entry too.
func TestICEBind_V18_19_RemoveEndpoint_CleansArmer(t *testing.T) {
	b := setupICEBind(t)
	fakeIP := netip.MustParseAddr("127.2.158.151")
	armer := &stubWakeArmer{}
	b.SetEndpoint(fakeIP, v18_19DiscardConn{}, armer)
	b.RemoveEndpoint(fakeIP)

	// Send must fall through to StdNetBind (not crash, even though no entry).
	pkt := makeV18_19TransportPacket(148)
	ep := &Endpoint{AddrPort: netip.AddrPortFrom(fakeIP, 51820)}
	// StdNetBind.Send will error because we don't have an open UDP socket;
	// that's irrelevant — we only assert the armer did NOT fire and the
	// map entry is gone.
	_ = b.Send([][]byte{pkt}, ep)

	if got := armer.count.Load(); got != 0 {
		t.Fatalf("V18.19: armer must NOT fire after RemoveEndpoint, got count=%d", got)
	}
	b.endpointsMu.Lock()
	_, present := b.endpointsWakeArmer[fakeIP]
	b.endpointsMu.Unlock()
	if present {
		t.Fatal("V18.19: RemoveEndpoint must delete from endpointsWakeArmer map")
	}
}
