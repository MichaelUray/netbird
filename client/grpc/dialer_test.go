package grpc

import (
	"testing"
	"time"
)

// DefaultKeepaliveClientParameters must enable PermitWithoutStream so
// HTTP/2 PINGs run during streamless idle phases (between withMgmtStream
// backoff retries; on a freshly-created ClientConn before the first
// stream opens). Without that flag the conn cannot detect a dead TCP
// path until a stream is actually opened -- which can itself be the
// operation that hangs.
func TestDefaultKeepaliveClientParameters(t *testing.T) {
	params := DefaultKeepaliveClientParameters()

	if !params.PermitWithoutStream {
		t.Fatal("PermitWithoutStream must be true so HTTP/2 PINGs run in streamless idle phases")
	}
	if params.Time < 10*time.Second || params.Time > 60*time.Second {
		t.Fatalf("Time = %v, want a value in [10s, 60s]", params.Time)
	}
	if params.Timeout < 5*time.Second || params.Timeout > 20*time.Second {
		t.Fatalf("Timeout = %v, want a value in [5s, 20s]", params.Timeout)
	}
}
