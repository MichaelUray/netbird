package activity

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

// withCapturedHook installs a counting writeAfterCloseHook for the lifetime
// of the test and returns a function to read the call count plus the last
// captured peerLabel. The hook is restored on cleanup.
func withCapturedHook(t *testing.T) (countFn func() int32, lastLabelFn func() string) {
	t.Helper()
	original := writeAfterCloseHook
	var count int32
	var mu sync.Mutex
	var lastLabel string
	writeAfterCloseHook = func(peerLabel string) {
		atomic.AddInt32(&count, 1)
		mu.Lock()
		lastLabel = peerLabel
		mu.Unlock()
	}
	t.Cleanup(func() {
		writeAfterCloseHook = original
	})
	return func() int32 { return atomic.LoadInt32(&count) }, func() string {
		mu.Lock()
		defer mu.Unlock()
		return lastLabel
	}
}

// TestLazyConn_WriteAfterClose_FiresWarnOnce verifies the diagnostic hook
// fires exactly once per lazyConn lifetime no matter how many Writes the
// caller attempts after Close, and that every Write returns io.EOF.
func TestLazyConn_WriteAfterClose_FiresWarnOnce(t *testing.T) {
	countFn, lastLabelFn := withCapturedHook(t)

	c := newLazyConnWithLabel("peerXYZ")
	_ = c.Close()

	for i := 0; i < 5; i++ {
		n, err := c.Write([]byte("ping"))
		if err != io.EOF {
			t.Fatalf("iter %d: expected io.EOF after close, got n=%d err=%v", i, n, err)
		}
		if n != 0 {
			t.Fatalf("iter %d: expected n=0 after close, got %d", i, n)
		}
	}

	if got := countFn(); got != 1 {
		t.Fatalf("write-after-close hook must fire exactly once, fired %d times", got)
	}
	if got := lastLabelFn(); got != "peerXYZ" {
		t.Fatalf("hook must receive the configured peer label, got %q", got)
	}
}

// TestLazyConn_WriteBeforeClose_NoWarn confirms Writes on an open lazyConn
// never trigger the diagnostic hook.
func TestLazyConn_WriteBeforeClose_NoWarn(t *testing.T) {
	countFn, _ := withCapturedHook(t)

	c := newLazyConnWithLabel("peerOpen")
	for i := 0; i < 10; i++ {
		n, err := c.Write([]byte("ping"))
		if err != nil {
			t.Fatalf("iter %d: unexpected error on open conn: %v", i, err)
		}
		if n != 4 {
			t.Fatalf("iter %d: expected n=4, got %d", i, n)
		}
	}
	// Drain the (single-slot) activity channel so a follow-up assertion would
	// not be polluted.
	select {
	case <-c.ActivityChan():
	default:
	}

	if got := countFn(); got != 0 {
		t.Fatalf("hook must not fire while conn is open, fired %d times", got)
	}
}

// TestLazyConn_ConcurrentWriteAndClose drives many writers in parallel with
// Close to make sure the warnOnce gate stays a single fire and no panic
// occurs in the data-race sandwich.
func TestLazyConn_ConcurrentWriteAndClose(t *testing.T) {
	countFn, _ := withCapturedHook(t)

	c := newLazyConnWithLabel("peerRace")

	var wg sync.WaitGroup
	const writers = 100
	const itersPerWriter = 50

	startCh := make(chan struct{})
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			<-startCh
			for j := 0; j < itersPerWriter; j++ {
				// Errors are expected once Close fires; we only care that
				// the call returns without panicking.
				_, _ = c.Write([]byte{1, 2, 3, 4})
			}
		}()
	}

	close(startCh)
	_ = c.Close()
	wg.Wait()

	// After Close + drain, do a guaranteed write-after-close to make sure the
	// hook is reachable from this test (the parallel writes may all have
	// run before Close completed and produced zero fires).
	_, err := c.Write([]byte("trailing"))
	if err != io.EOF {
		t.Fatalf("expected io.EOF on trailing write-after-close, got %v", err)
	}

	if got := countFn(); got != 1 {
		t.Fatalf("hook must fire at most once even under concurrency, fired %d times", got)
	}
}

// TestLazyConn_WriteAfterClose_EmptyLabel ensures the unknown-peer log path
// is exercised when newLazyConn (no label) is used.
func TestLazyConn_WriteAfterClose_EmptyLabel(t *testing.T) {
	countFn, lastLabelFn := withCapturedHook(t)

	c := newLazyConn()
	_ = c.Close()
	_, _ = c.Write([]byte("x"))

	if got := countFn(); got != 1 {
		t.Fatalf("expected exactly 1 fire, got %d", got)
	}
	if got := lastLabelFn(); got != "" {
		t.Fatalf("expected empty label, got %q", got)
	}
}
