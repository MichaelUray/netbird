package bind

import (
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/netbirdio/netbird/monotime"
)

func TestActivityRecorder_GetLastActivities(t *testing.T) {
	peer := "peer1"
	ar := NewActivityRecorder()
	ar.UpsertAddress("peer1", netip.MustParseAddrPort("192.168.0.5:51820"))
	activities := ar.GetLastActivities()

	p, ok := activities[peer]
	if !ok {
		t.Fatalf("Expected activity for peer %s, but got none", peer)
	}

	if monotime.Since(p) > 5*time.Second {
		t.Fatalf("Expected activity for peer %s to be recent, but got %v", peer, p)
	}
}

// TestActivityRecorder_RecordInboundBurst_FiresOnThirdPacket verifies
// V18.16 Fix 2: an inbound burst of >=3 packets within 30s wakes the
// onActivity callback, mirroring recordOutbound's edge semantics but
// using its own burst gate to prevent legacy-keepalive-driven loops.
func TestActivityRecorder_RecordInboundBurst_FiresOnThirdPacket(t *testing.T) {
	ar := NewActivityRecorder()
	var fired atomic.Int32
	ar.SetOnActivity(func(pubKey string) {
		fired.Add(1)
	})
	ar.UpsertAddress("peer1", netip.MustParseAddrPort("10.0.0.5:51820"))

	// Single packet: should NOT fire.
	ar.recordInboundBurst(netip.MustParseAddrPort("10.0.0.5:51820"))
	if fired.Load() != 0 {
		t.Fatalf("single inbound packet must not fire onActivity, got %d", fired.Load())
	}

	// Second packet: still NOT fire.
	ar.recordInboundBurst(netip.MustParseAddrPort("10.0.0.5:51820"))
	if fired.Load() != 0 {
		t.Fatalf("two inbound packets must not fire onActivity, got %d", fired.Load())
	}

	// Third packet within window: SHOULD fire exactly once.
	ar.recordInboundBurst(netip.MustParseAddrPort("10.0.0.5:51820"))
	if fired.Load() != 1 {
		t.Fatalf("third inbound packet should fire onActivity, fire-count=%d", fired.Load())
	}

	// Fourth+ packets within the same window: must NOT fire again
	// (reset on successful wake mirrors V18.13 semantics).
	ar.recordInboundBurst(netip.MustParseAddrPort("10.0.0.5:51820"))
	ar.recordInboundBurst(netip.MustParseAddrPort("10.0.0.5:51820"))
	if fired.Load() != 1 {
		t.Fatalf("burst should fire exactly once per cycle, got %d", fired.Load())
	}
}

// TestActivityRecorder_RecordInboundBurst_WindowExpiresResetsCount
// verifies the sliding 30s window: packets older than the window
// don't contribute to the burst threshold.
func TestActivityRecorder_RecordInboundBurst_WindowExpiresResetsCount(t *testing.T) {
	ar := NewActivityRecorder()
	var fired atomic.Int32
	ar.SetOnActivity(func(pubKey string) {
		fired.Add(1)
	})
	ar.UpsertAddress("peer1", netip.MustParseAddrPort("10.0.0.5:51820"))

	// Two packets within window.
	ar.recordInboundBurst(netip.MustParseAddrPort("10.0.0.5:51820"))
	ar.recordInboundBurst(netip.MustParseAddrPort("10.0.0.5:51820"))
	if fired.Load() != 0 {
		t.Fatalf("two packets must not fire, got %d", fired.Load())
	}

	// Force-expire the window by rewriting the recorded peer's
	// burst window start far into the past.
	rec := ar.peers["peer1"]
	if rec == nil {
		t.Fatal("peer record missing")
	}
	rec.inboundBurstWindowStart.Store(int64(monotime.Now()) - int64(2*time.Minute))

	// One more packet — should start a fresh window and NOT fire
	// (because the new window count starts at 1).
	ar.recordInboundBurst(netip.MustParseAddrPort("10.0.0.5:51820"))
	if fired.Load() != 0 {
		t.Fatalf("packet after window expiry must not fire (fresh count=1), got %d", fired.Load())
	}
	if c := rec.inboundBurstCount.Load(); c != 1 {
		t.Fatalf("expired window should reset count to 1, got %d", c)
	}
}

// TestActivityRecorder_RecordInboundBurst_UnknownAddressNoOp verifies
// no panic / log spam when a packet arrives from an address we haven't
// registered (e.g. relay peer not yet added to recorder).
func TestActivityRecorder_RecordInboundBurst_UnknownAddressNoOp(t *testing.T) {
	ar := NewActivityRecorder()
	var fired atomic.Int32
	ar.SetOnActivity(func(pubKey string) { fired.Add(1) })

	// No UpsertAddress for this address.
	ar.recordInboundBurst(netip.MustParseAddrPort("99.99.99.99:51820"))
	if fired.Load() != 0 {
		t.Fatal("unknown address must not fire callback")
	}
}
