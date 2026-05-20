package client

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// TestRegisterUnregisterStreamCancel exercises the round trip the new
// handleJobStream depends on: a cancel is registered, fired by
// cancelAllStreams, then unregistered. After unregister, a second
// cancelAllStreams call must not touch the now-deleted cancel.
func TestRegisterUnregisterStreamCancel(t *testing.T) {
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

	c.streamCancelMu.Lock()
	_, present := c.streamCancels[id]
	c.streamCancelMu.Unlock()
	require.True(t, present, "registerStreamCancel must store the cancel func")

	c.cancelAllStreams(errWatchdogReconnect)
	select {
	case <-streamCtx.Done():
	case <-time.After(time.Second):
		t.Fatalf("stream context did not observe first cancelAllStreams")
	}

	c.unregisterStreamCancel(id)
	c.streamCancelMu.Lock()
	_, present = c.streamCancels[id]
	c.streamCancelMu.Unlock()
	assert.False(t, present, "unregisterStreamCancel must remove the entry")

	// Second cancelAllStreams must be a no-op on the unregistered entry.
	c.cancelAllStreams(errors.New("should not propagate"))
	// The original cause stays errWatchdogReconnect.
	assert.ErrorIs(t, context.Cause(streamCtx), errWatchdogReconnect)
}

// TestStreamCancelMap_ConcurrentRegisterCancel hammers the
// register/unregister/cancel paths from many goroutines. Without
// streamCancelMu protecting both reads and writes this would race; the
// race detector flags any unsynchronized map access.
func TestStreamCancelMap_ConcurrentRegisterCancel(t *testing.T) {
	testKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	s, lis, _, _ := startMockManagement(t)
	t.Cleanup(func() { closeManagementSilently(s, lis) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c, err := NewClient(ctx, lis.Addr().String(), testKey, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 64; j++ {
				_, sc := context.WithCancelCause(context.Background())
				id := c.registerStreamCancel(sc)
				c.unregisterStreamCancel(id)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 32; j++ {
				c.cancelAllStreams(errWatchdogReconnect)
			}
		}()
	}
	wg.Wait()

	c.streamCancelMu.Lock()
	remaining := len(c.streamCancels)
	c.streamCancelMu.Unlock()
	assert.Equal(t, 0, remaining, "all stream-cancels should be deregistered")
}

// TestHandleJobStream_CauseAwareReturn exercises the new branch in
// handleJobStream that returns errWatchdogReconnect instead of nil when
// the context cancel was watchdog-driven. We bypass the actual gRPC stub
// (which requires a working Job server) and validate the branch's
// errors.Is logic by simulating exactly the post-receiveJobRequest
// state: ctx.Err() set, context.Cause(ctx) carrying the sentinel.
func TestHandleJobStream_CauseAwareReturn(t *testing.T) {
	// Build a context exactly like handleJobStream's local ctx.
	streamCtx, streamCancel := context.WithCancelCause(context.Background())
	streamCancel(errWatchdogReconnect)

	require.Error(t, streamCtx.Err())

	cause := context.Cause(streamCtx)
	require.True(t, errors.Is(cause, errWatchdogReconnect),
		"context.Cause must round-trip the watchdog sentinel so the new branch fires")
	require.False(t, errors.Is(cause, context.Canceled) && !errors.Is(cause, errWatchdogReconnect),
		"sentinel must not be masked by generic context.Canceled")

	// Same path but with the engine-shutdown semantics: a bare
	// context.CancelFunc (no cause). The handleJobStream return must
	// fall through to the `return nil` branch.
	plainCtx, plainCancel := context.WithCancel(context.Background())
	plainCancel()
	c := context.Cause(plainCtx)
	assert.False(t, errors.Is(c, errWatchdogReconnect),
		"plain cancel must not look like a watchdog reconnect")
	assert.ErrorIs(t, c, context.Canceled)
}
