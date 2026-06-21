package activity

import (
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/netbirdio/netbird/client/internal/lazyconn"
)

// udpStubWakeArmer is a minimal bind.WakeIntentArmer for UDPListener tests —
// counts invocations and records the last payloadSize observed.
type udpStubWakeArmer struct {
	count    atomic.Int32
	lastSize atomic.Int32
}

func (s *udpStubWakeArmer) ArmLocalWakeIntent(payloadSize int) {
	s.count.Add(1)
	s.lastSize.Store(int32(payloadSize))
}

func TestUDPListener_Creation(t *testing.T) {
	mockIface := &MocWGIface{}

	peer := &MocPeer{PeerID: "testPeer1"}
	cfg := lazyconn.PeerConfig{
		PublicKey:  peer.PeerID,
		PeerConnID: peer.ConnID(),
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("100.64.0.2/32")},
		Log:        log.WithField("peer", "testPeer1"),
	}

	listener, err := NewUDPListener(mockIface, cfg)
	require.NoError(t, err)
	require.NotNil(t, listener.conn)
	require.NotNil(t, listener.endpoint)

	readPacketsDone := make(chan struct{})
	go func() {
		listener.ReadPackets()
		close(readPacketsDone)
	}()

	listener.Close()

	select {
	case <-readPacketsDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ReadPackets to exit after Close")
	}
}

func TestUDPListener_ActivityDetection(t *testing.T) {
	mockIface := &MocWGIface{}

	peer := &MocPeer{PeerID: "testPeer1"}
	cfg := lazyconn.PeerConfig{
		PublicKey:  peer.PeerID,
		PeerConnID: peer.ConnID(),
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("100.64.0.2/32")},
		Log:        log.WithField("peer", "testPeer1"),
	}

	listener, err := NewUDPListener(mockIface, cfg)
	require.NoError(t, err)

	activityDetected := make(chan struct{})
	go func() {
		listener.ReadPackets()
		close(activityDetected)
	}()

	conn, err := net.Dial("udp", listener.conn.LocalAddr().String())
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte{0x01, 0x02, 0x03})
	require.NoError(t, err)

	select {
	case <-activityDetected:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for activity detection")
	}
}

// TestUDPListener_V18_30_ArmsWakeIntentOnActivity asserts that when activity
// is detected on the UDP socket (kernel-mode-WG forwards an outbound packet
// to the lazy-bind UDP endpoint), the registered WakeArmer's ArmLocalWakeIntent
// fires exactly once with the byte-count of the activity payload.
//
// This is the kernel-mode counterpart to V18.19's ICEBind.Send hook —
// userspace-bind clients arm via the bind layer's per-packet hook, kernel-mode
// clients arm via the UDPListener's activity-edge.
func TestUDPListener_V18_30_ArmsWakeIntentOnActivity(t *testing.T) {
	mockIface := &MocWGIface{}

	peer := &MocPeer{PeerID: "testPeer1"}
	armer := &udpStubWakeArmer{}
	cfg := lazyconn.PeerConfig{
		PublicKey:  peer.PeerID,
		PeerConnID: peer.ConnID(),
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("100.64.0.2/32")},
		Log:        log.WithField("peer", "testPeer1"),
		WakeArmer:  armer,
	}

	listener, err := NewUDPListener(mockIface, cfg)
	require.NoError(t, err)

	done := make(chan readResult, 1)
	go func() {
		done <- listener.ReadPackets()
	}()

	conn, err := net.Dial("udp", listener.conn.LocalAddr().String())
	require.NoError(t, err)
	defer conn.Close()

	payload := []byte{0x04, 0x00, 0x00, 0x00, 0xAA, 0xBB, 0xCC, 0xDD}
	_, err = conn.Write(payload)
	require.NoError(t, err)

	select {
	case res := <-done:
		require.Equal(t, readActivity, res, "ReadPackets must return readActivity")
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ReadPackets to return")
	}

	assert.Equal(t, int32(1), armer.count.Load(),
		"V18.30: ArmLocalWakeIntent MUST be called exactly once on activity edge")
	assert.Greater(t, armer.lastSize.Load(), int32(0),
		"V18.30: ArmLocalWakeIntent payloadSize must be > 0")
}

// TestUDPListener_V18_30_NilArmer_NoCrash asserts that a UDPListener whose
// PeerConfig.WakeArmer is nil (pre-V18.30 callers / tests) does NOT crash
// when activity is detected — mirrors listener_bind.go's nil-safe behavior.
func TestUDPListener_V18_30_NilArmer_NoCrash(t *testing.T) {
	mockIface := &MocWGIface{}

	peer := &MocPeer{PeerID: "testPeer1"}
	cfg := lazyconn.PeerConfig{
		PublicKey:  peer.PeerID,
		PeerConnID: peer.ConnID(),
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("100.64.0.2/32")},
		Log:        log.WithField("peer", "testPeer1"),
		// WakeArmer intentionally nil
	}

	listener, err := NewUDPListener(mockIface, cfg)
	require.NoError(t, err)

	done := make(chan readResult, 1)
	go func() {
		done <- listener.ReadPackets()
	}()

	conn, err := net.Dial("udp", listener.conn.LocalAddr().String())
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte{0x01, 0x02, 0x03})
	require.NoError(t, err)

	select {
	case res := <-done:
		require.Equal(t, readActivity, res, "ReadPackets must return readActivity even with nil armer")
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ReadPackets to return (nil-armer path)")
	}
	// Pass if no panic.
}

func TestUDPListener_Close(t *testing.T) {
	mockIface := &MocWGIface{}

	peer := &MocPeer{PeerID: "testPeer1"}
	cfg := lazyconn.PeerConfig{
		PublicKey:  peer.PeerID,
		PeerConnID: peer.ConnID(),
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("100.64.0.2/32")},
		Log:        log.WithField("peer", "testPeer1"),
	}

	listener, err := NewUDPListener(mockIface, cfg)
	require.NoError(t, err)

	readPacketsDone := make(chan struct{})
	go func() {
		listener.ReadPackets()
		close(readPacketsDone)
	}()

	listener.Close()

	select {
	case <-readPacketsDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ReadPackets to exit after Close")
	}

	assert.True(t, listener.isClosed.Load(), "Listener should be marked as closed")
}
