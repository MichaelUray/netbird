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

// V18.20 (2026-06-21): cold-boot wake-intent on WG handshake-initiation
// (type=1, 148 B). The V18.19 transport-only filter missed never-connected
// peers because wireguard-go sends type=1 BEFORE any type=4 traffic.

// makeV18_20HandshakeInitPacket builds a WG handshake-initiation: 4-byte
// header (type=1 little-endian) + 144-byte body to reach the canonical
// 148-byte size.
func makeV18_20HandshakeInitPacket() []byte {
	buf := make([]byte, 148)
	binary.LittleEndian.PutUint32(buf[:4], 1)
	return buf
}

// TestICEBind_V18_20_ArmIntent_FiresOnHandshakeInit asserts that a WG
// handshake-initiation packet (type=1) routed via the ok-path triggers
// ArmLocalWakeIntent — this is the cold-boot path V18.19 missed.
func TestICEBind_V18_20_ArmIntent_FiresOnHandshakeInit(t *testing.T) {
	b := setupICEBind(t)
	fakeIP := netip.MustParseAddr("127.2.158.151")
	armer := &stubWakeArmer{}
	b.SetEndpoint(fakeIP, v18_19DiscardConn{}, armer)

	pkt := makeV18_20HandshakeInitPacket()
	ep := &Endpoint{AddrPort: netip.AddrPortFrom(fakeIP, 51820)}

	if err := b.Send([][]byte{pkt}, ep); err != nil {
		t.Fatalf("V18.20: Send returned unexpected error: %v", err)
	}

	if got := armer.count.Load(); got != 1 {
		t.Fatalf("V18.20: handshake-init (type=1, 148 B) MUST arm intent exactly once, got count=%d", got)
	}
	if got := armer.lastSize.Load(); got != 148 {
		t.Fatalf("V18.20: arm must be called with payloadSize=148, got %d", got)
	}
}

// TestICEBind_V18_20_ArmIntent_RejectsHandshakeResponse asserts that a WG
// handshake-response (type=2) routed via the ok-path does NOT arm intent.
// Receiver-side responses are not local wake signals.
func TestICEBind_V18_20_ArmIntent_RejectsHandshakeResponse(t *testing.T) {
	b := setupICEBind(t)
	fakeIP := netip.MustParseAddr("127.2.158.151")
	armer := &stubWakeArmer{}
	b.SetEndpoint(fakeIP, v18_19DiscardConn{}, armer)

	pkt := make([]byte, 92)
	binary.LittleEndian.PutUint32(pkt[:4], 2) // type=2 response
	ep := &Endpoint{AddrPort: netip.AddrPortFrom(fakeIP, 51820)}
	_ = b.Send([][]byte{pkt}, ep)

	if got := armer.count.Load(); got != 0 {
		t.Fatalf("V18.20: handshake-response (type=2) must NOT arm intent, got count=%d", got)
	}
}

// TestICEBind_V18_20_isWakeIntentPkg_Boundaries asserts the boundary
// conditions of the wake-intent classifier in isolation.
func TestICEBind_V18_20_isWakeIntentPkg_Boundaries(t *testing.T) {
	mk := func(typ uint32, n int) []byte {
		buf := make([]byte, n)
		if n >= 4 {
			binary.LittleEndian.PutUint32(buf[:4], typ)
		}
		return buf
	}

	cases := []struct {
		name string
		pkt  []byte
		want bool
	}{
		{"type4_transport_33B", mk(4, 33), true},
		{"type4_keepalive_32B", mk(4, 32), false},
		{"type4_short_31B", mk(4, 31), false},
		{"type1_handshake_148B", mk(1, 148), true},
		{"type1_handshake_147B_undersized", mk(1, 147), false},
		{"type2_response_92B", mk(2, 92), false},
		{"type3_cookie_64B", mk(3, 64), false},
		{"too_short_3B", mk(0, 3), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWakeIntentPkg(tc.pkt); got != tc.want {
				t.Fatalf("isWakeIntentPkg(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
