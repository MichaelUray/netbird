package peer

import (
	"testing"

	"github.com/netbirdio/netbird/client/internal/peer/worker"
)

func TestTransportSnapshot_BothConnected(t *testing.T) {
	conn := &Conn{
		statusICE:   worker.NewAtomicStatus(),
		statusRelay: worker.NewAtomicStatus(),
	}
	conn.statusICE.SetConnected()
	conn.statusRelay.SetConnected()

	iceDisc, relayDisc := conn.TransportSnapshot()
	if iceDisc || relayDisc {
		t.Fatalf("expected (false, false), got (%v, %v)", iceDisc, relayDisc)
	}
}

func TestTransportSnapshot_BothDisconnected(t *testing.T) {
	conn := &Conn{
		statusICE:   worker.NewAtomicStatus(),
		statusRelay: worker.NewAtomicStatus(),
	}
	// NewAtomicStatus sets StatusDisconnected as default
	iceDisc, relayDisc := conn.TransportSnapshot()
	if !iceDisc || !relayDisc {
		t.Fatalf("expected (true, true), got (%v, %v)", iceDisc, relayDisc)
	}
}

func TestTransportSnapshot_RelayOnly(t *testing.T) {
	conn := &Conn{
		statusICE:   worker.NewAtomicStatus(),
		statusRelay: worker.NewAtomicStatus(),
	}
	conn.statusICE.SetDisconnected()
	conn.statusRelay.SetConnected()

	iceDisc, relayDisc := conn.TransportSnapshot()
	if !iceDisc || relayDisc {
		t.Fatalf("expected (true, false), got (%v, %v)", iceDisc, relayDisc)
	}
}

func TestTransportSnapshot_RaceSafe(t *testing.T) {
	conn := &Conn{
		statusICE:   worker.NewAtomicStatus(),
		statusRelay: worker.NewAtomicStatus(),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10000; i++ {
			conn.statusICE.SetConnected()
			conn.statusRelay.SetDisconnected()
		}
	}()
	for i := 0; i < 10000; i++ {
		_, _ = conn.TransportSnapshot()
	}
	<-done
}
