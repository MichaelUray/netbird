package peer

import (
	"net/netip"
	"sync"
	"testing"
	"time"
)

// Phase 3.7l Phase-1 unit tests. The state machine has three branches
// (first-failure-or-srflx-change / same-srflx-failure / success-reset);
// each covered, plus a concurrency smoke-test for srflxStateSync to
// prove the mutex isolates state under racy access (the production
// callers are onICEFailed and snapshotForDiagnosis from different
// goroutines).

func mustAP(t *testing.T, s string) netip.AddrPort {
	t.Helper()
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ap
}

func TestSrflxFailureState_FirstFailureSeedsCounter(t *testing.T) {
	var s srflxFailureState
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	s.observeICEFailure(mustAP(t, "203.0.113.7:17692"), now)

	if got, want := s.samePortFailures, 1; got != want {
		t.Fatalf("samePortFailures = %d, want %d", got, want)
	}
	if got, want := s.lastSrflx.String(), "203.0.113.7:17692"; got != want {
		t.Fatalf("lastSrflx = %q, want %q", got, want)
	}
	if s.lastChanged.IsZero() {
		t.Fatal("lastChanged must be set on first failure")
	}
}

func TestSrflxFailureState_SameSrflxIncrements(t *testing.T) {
	var s srflxFailureState
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	ap := mustAP(t, "203.0.113.7:17692")

	s.observeICEFailure(ap, now)
	firstChanged := s.lastChanged

	s.observeICEFailure(ap, now.Add(30*time.Second))
	s.observeICEFailure(ap, now.Add(60*time.Second))

	if got, want := s.samePortFailures, 3; got != want {
		t.Fatalf("samePortFailures = %d, want %d", got, want)
	}
	if !s.lastChanged.Equal(firstChanged) {
		t.Fatalf("lastChanged must NOT advance while srflx is unchanged; "+
			"got=%v want=%v", s.lastChanged, firstChanged)
	}
}

func TestSrflxFailureState_ChangedSrflxResetsCounter(t *testing.T) {
	var s srflxFailureState
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	apOld := mustAP(t, "203.0.113.7:17692")
	apNew := mustAP(t, "203.0.113.7:8288")

	s.observeICEFailure(apOld, now)
	s.observeICEFailure(apOld, now.Add(30*time.Second))
	s.observeICEFailure(apNew, now.Add(60*time.Second))

	if got, want := s.samePortFailures, 1; got != want {
		t.Fatalf("samePortFailures = %d, want %d (reset on srflx change)", got, want)
	}
	if got, want := s.lastSrflx, apNew; got != want {
		t.Fatalf("lastSrflx = %v, want %v", got, want)
	}
	if !s.lastChanged.Equal(now.Add(60 * time.Second)) {
		t.Fatalf("lastChanged must advance on srflx change; got=%v", s.lastChanged)
	}
}

func TestSrflxFailureState_SuccessResets(t *testing.T) {
	var s srflxFailureState
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	ap := mustAP(t, "203.0.113.7:17692")

	s.observeICEFailure(ap, now)
	s.observeICEFailure(ap, now.Add(time.Second))
	s.observeICESuccess(ap, now.Add(2*time.Second))

	if got, want := s.samePortFailures, 0; got != want {
		t.Fatalf("samePortFailures after success = %d, want %d", got, want)
	}
	if got, want := s.lastSrflx, ap; got != want {
		t.Fatalf("lastSrflx = %v, want %v", got, want)
	}
}

// TestSrflxFailureState_ZeroAddrPortBehaviour locks in the documented
// "two zero AddrPorts compare equal, so consecutive failures with no
// observed srflx still register as a same-port streak" semantics.
// That pattern itself is diagnostic: pion never surfaced a public
// mapping in either attempt.
func TestSrflxFailureState_ZeroAddrPortBehaviour(t *testing.T) {
	var s srflxFailureState
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)

	s.observeICEFailure(netip.AddrPort{}, now)
	s.observeICEFailure(netip.AddrPort{}, now.Add(30*time.Second))

	if got, want := s.samePortFailures, 2; got != want {
		t.Fatalf("samePortFailures with zero AddrPort = %d, want %d", got, want)
	}
	if s.lastSrflx.IsValid() {
		t.Fatalf("lastSrflx should remain zero (invalid AddrPort); got %v", s.lastSrflx)
	}
}

// TestSrflxStateSync_ConcurrentAccess is a smoke test for the
// per-Conn lock. 100 goroutines hammer failure+snapshot in parallel
// against the same srflx; the final counter must equal the number of
// failures the test issued. Without the mutex this test races (and
// Go's race detector flags it under `go test -race`).
func TestSrflxStateSync_ConcurrentAccess(t *testing.T) {
	var sync srflxStateSync
	ap := mustAP(t, "203.0.113.7:17692")
	now := time.Now()

	const goroutines = 100
	const perRoutine = 10
	var wg parallelHelper
	wg.runFn = func() {
		for i := 0; i < perRoutine; i++ {
			sync.observeFailure(ap, now)
			_ = sync.snapshot()
		}
	}
	wg.spawn(goroutines)

	snap := sync.snapshot()
	if got, want := snap.samePortFailures, goroutines*perRoutine; got != want {
		t.Fatalf("samePortFailures = %d, want %d", got, want)
	}
}

// parallelHelper is a tiny zero-dep WaitGroup wrapper kept inline to
// avoid pulling in golang.org/x/sync. Not exported.
type parallelHelper struct {
	runFn func()
}

func (p *parallelHelper) spawn(n int) {
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			p.runFn()
		}()
	}
	wg.Wait()
}
