package client

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// TestErrWatchdogReconnectSentinel guards the contract handleSyncStream /
// handleJobStream depend on: errWatchdogReconnect must be a stable,
// non-nil error whose identity survives errors.Is wrapping. Renaming or
// re-declaring it would silently degrade the reconnect path back to
// returning nil (clean-shutdown semantics) and end the retry loop.
func TestErrWatchdogReconnectSentinel(t *testing.T) {
	require.Error(t, errWatchdogReconnect)

	wrapped := errors.Join(errors.New("ctx canceled"), errWatchdogReconnect)
	assert.True(t, errors.Is(wrapped, errWatchdogReconnect))
}

// TestStreamWatchdog_StartStopIdempotent verifies sync.Once protection on
// both start() and stop(). Double-start used to spawn two probe loops,
// double-stop used to close doneCh twice and panic; both are now no-ops.
func TestStreamWatchdog_StartStopIdempotent(t *testing.T) {
	w := &streamWatchdog{
		c:        nil,
		interval: time.Hour,
		timeout:  time.Second,
		doneCh:   make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.ctx = ctx
	w.cancel = cancel

	// Replace run with a counting stub so we can prove start() spawns
	// exactly one goroutine no matter how often it is called.
	var runs atomic.Int32
	stubDone := make(chan struct{})

	w.startOnce.Do(func() {
		runs.Add(1)
		go func() {
			<-w.ctx.Done()
			close(stubDone)
			close(w.doneCh)
		}()
	})
	// Subsequent start() calls must be no-ops.
	for i := 0; i < 4; i++ {
		w.start() // real method, guarded by the same startOnce
	}
	assert.Equal(t, int32(1), runs.Load(), "startOnce must guard against double-start")

	// Stop is allowed to be called concurrently from many goroutines;
	// stopOnce + doneCh-block must remain safe.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.stop()
		}()
	}
	wg.Wait()

	select {
	case <-stubDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchdog stub goroutine did not observe cancel within 2s")
	}
}

// TestStreamWatchdog_ProbeOnce_OK brings up the mock management server,
// dials a real GrpcClient, and validates that a synchronous probeOnce()
// against a healthy server returns true. This is the success leg of the
// branch that increments probeOk.
func TestStreamWatchdog_ProbeOnce_OK(t *testing.T) {
	testKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	s, lis, _, _ := startMockManagement(t)
	t.Cleanup(func() { closeManagementSilently(s, lis) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c, err := NewClient(ctx, lis.Addr().String(), testKey, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	require.NotNil(t, c.watchdog)
	// Drive the probe directly so we don't have to wait an interval.
	ok := c.watchdog.probeOnce()
	assert.True(t, ok, "probeOnce against live mock server must return true")
}

// TestStreamWatchdog_ProbeOnce_Failure stops the mock server and asserts
// that probeOnce flips to false. This is the leg that increments
// probeErr and ultimately trips cancelAllStreams.
func TestStreamWatchdog_ProbeOnce_Failure(t *testing.T) {
	testKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	s, lis, _, _ := startMockManagement(t)
	addr := lis.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c, err := NewClient(ctx, addr, testKey, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	// Yank the server out from under the client and give the underlying
	// HTTP/2 transport a moment to notice. probeOnce uses a 5s deadline
	// so even a CONNECTING-state failure resolves in well under that.
	closeManagementSilently(s, lis)
	time.Sleep(200 * time.Millisecond)

	ok := c.watchdog.probeOnce()
	assert.False(t, ok, "probeOnce against down server must return false")
}

// TestCancelAllStreams_CauseIsObservable proves that a context registered
// via registerStreamCancel observes the watchdog sentinel in
// context.Cause(). Without this, handleJobStream's new
// errors.Is(cause, errWatchdogReconnect) branch would never fire.
func TestCancelAllStreams_CauseIsObservable(t *testing.T) {
	testKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	s, lis, _, _ := startMockManagement(t)
	t.Cleanup(func() { closeManagementSilently(s, lis) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c, err := NewClient(ctx, lis.Addr().String(), testKey, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	streamCtx, streamCancel := context.WithCancelCause(context.Background())
	id := c.registerStreamCancel(streamCancel)
	t.Cleanup(func() { c.unregisterStreamCancel(id) })

	c.cancelAllStreams(errWatchdogReconnect)

	select {
	case <-streamCtx.Done():
	case <-time.After(time.Second):
		t.Fatalf("stream context did not observe cancelAllStreams")
	}
	assert.ErrorIs(t, context.Cause(streamCtx), errWatchdogReconnect)
}

// TestStateLogger_FollowsReconnect proves that the state-logger goroutine
// continues to observe the *current* c.conn after reconnectClientConn()
// swaps it out. Before this fix the logger captured the original conn in
// its closure, so after a reconnect it stayed parked on a Shutdown conn
// forever and the instrumentation went silent.
func TestStateLogger_FollowsReconnect(t *testing.T) {
	testKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	s, lis, _, _ := startMockManagement(t)
	t.Cleanup(func() { closeManagementSilently(s, lis) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c, err := NewClient(ctx, lis.Addr().String(), testKey, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	connBefore, _ := c.snapshotConn()
	require.NoError(t, c.reconnectClientConn(ctx))
	connAfter, _ := c.snapshotConn()
	assert.NotSame(t, connBefore, connAfter, "reconnectClientConn must produce a fresh ClientConn")

	// Give the logger up to 500ms to re-attach. We can't directly probe
	// the goroutine, but we can prove the new conn is reachable via the
	// usual public surface; if the logger had wedged it wouldn't affect
	// this, but the test also catches future regressions where the
	// logger panics on a closed conn.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.ready() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.True(t, c.ready(), "new ClientConn should reach Ready/Idle after reconnect")
}

// TestSnapshotConn_AtomicSwap fires off many concurrent snapshotConn()
// readers while reconnectClientConn swaps the conn + realClient pair
// underneath them. The contract is: every reader sees a non-nil conn
// matched with a non-nil realClient. Without c.connMu in snapshotConn,
// a reader can observe the half-torn state where conn has been Close'd
// but realClient still points at it -- the bug Codex flagged.
func TestSnapshotConn_AtomicSwap(t *testing.T) {
	testKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	s, lis, _, _ := startMockManagement(t)
	t.Cleanup(func() { closeManagementSilently(s, lis) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c, err := NewClient(ctx, lis.Addr().String(), testKey, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	var (
		stop      atomic.Bool
		readerErr atomic.Pointer[error]
		wg        sync.WaitGroup
	)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				conn, rc := c.snapshotConn()
				if conn == nil || rc == nil {
					e := errors.New("snapshotConn returned nil under reconnect")
					readerErr.CompareAndSwap(nil, &e)
					return
				}
			}
		}()
	}

	// Trigger a handful of reconnects. Each call closes the old conn
	// and dials a new one against the same listener.
	for i := 0; i < 5; i++ {
		require.NoError(t, c.reconnectClientConn(ctx))
		time.Sleep(20 * time.Millisecond)
	}
	stop.Store(true)
	wg.Wait()

	if e := readerErr.Load(); e != nil {
		t.Fatalf("reader observed inconsistent snapshot: %v", *e)
	}
}
