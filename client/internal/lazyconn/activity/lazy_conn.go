package activity

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// writeAfterCloseHook is invoked once per lazyConn the first time Write is
// called after Close. Tests override this to count fires without depending
// on the logrus hook machinery. Default fires a Warnf via the package logger.
//
// Phase 3.7i (#5989): exposes a deterministic diagnostic for the race
// between BindListener.ReadPackets closing the lazyConn after activity is
// observed and a subsequent ICEBind.Send hitting Write on the now-closed
// conn. Without the hook the race silently returns io.EOF and we cannot
// tell whether a stuck Relay peer was caused by lost activity edges.
var writeAfterCloseHook = func(peerLabel string) {
	if peerLabel == "" {
		log.Warn("lazyConn: Write to closed lazyConn (peer=unknown); possible race between close+reattach. Subsequent dropped writes will be silent.")
	} else {
		log.Warnf("lazyConn: Write to closed lazyConn (peer=%s); possible race between close+reattach. Subsequent dropped writes will be silent.", peerLabel)
	}
}

// lazyConn detects activity when WireGuard attempts to send packets.
// It does not deliver packets, only signals that activity occurred.
type lazyConn struct {
	activityCh chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc

	// peerLabel is an optional human-readable identifier (typically the
	// remote peer's WireGuard public key) used solely for diagnostics.
	// Empty when the lazyConn is constructed by tests or other callers
	// that do not bind to a specific peer.
	peerLabel string

	// warnOnce ensures the write-after-close warning fires exactly once
	// per lazyConn instance, even under concurrent Write calls.
	warnOnce sync.Once
}

// newLazyConn creates a new lazyConn for activity detection.
func newLazyConn() *lazyConn {
	return newLazyConnWithLabel("")
}

// newLazyConnWithLabel creates a new lazyConn tagged with peerLabel for
// diagnostic logging. The label is only consulted when Write is called
// after Close.
func newLazyConnWithLabel(peerLabel string) *lazyConn {
	ctx, cancel := context.WithCancel(context.Background())
	return &lazyConn{
		activityCh: make(chan struct{}, 1),
		ctx:        ctx,
		cancel:     cancel,
		peerLabel:  peerLabel,
	}
}

// Read blocks until the connection is closed.
func (c *lazyConn) Read(_ []byte) (n int, err error) {
	<-c.ctx.Done()
	return 0, io.EOF
}

// Write signals activity detection when ICEBind routes packets to this endpoint.
//
// Behaviour contract:
//   - If the conn is still open, the activity channel is signalled (non-blocking
//     coalesce) and len(b) is returned.
//   - If the conn has been closed, io.EOF is returned. The FIRST write-after-
//     close additionally fires writeAfterCloseHook so the diagnostic appears
//     in logs exactly once per conn lifetime; subsequent drops are silent so
//     a steady packet rate does not flood logs.
func (c *lazyConn) Write(b []byte) (n int, err error) {
	if c.ctx.Err() != nil {
		c.warnOnce.Do(func() {
			writeAfterCloseHook(c.peerLabel)
		})
		return 0, io.EOF
	}

	select {
	case c.activityCh <- struct{}{}:
	default:
	}

	return len(b), nil
}

// ActivityChan returns the channel that signals when activity is detected.
func (c *lazyConn) ActivityChan() <-chan struct{} {
	return c.activityCh
}

// Close closes the connection.
func (c *lazyConn) Close() error {
	c.cancel()
	return nil
}

// LocalAddr returns the local address.
func (c *lazyConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IP{127, 0, 0, 1}, Port: lazyBindPort}
}

// RemoteAddr returns the remote address.
func (c *lazyConn) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.IP{127, 0, 0, 1}, Port: lazyBindPort}
}

// SetDeadline sets the read and write deadlines.
func (c *lazyConn) SetDeadline(_ time.Time) error {
	return nil
}

// SetReadDeadline sets the deadline for future Read calls.
func (c *lazyConn) SetReadDeadline(_ time.Time) error {
	return nil
}

// SetWriteDeadline sets the deadline for future Write calls.
func (c *lazyConn) SetWriteDeadline(_ time.Time) error {
	return nil
}
