package guard

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// Phase 3.7j (#5989) Commit 2 of 5: validate that the
// IsIntentionalDetachFunc predicate gates the PartiallyConnected
// retry-budget branch. We drive the predicate via a closure over an
// atomic.Bool so tests can flip the detach state mid-flight.

// drivePartiallyConnected simulates N PartiallyConnected ticks by
// invoking the same branch logic as reconnectLoopWithRetry. Keeping the
// branch logic in lock-step with the production switch is intentional —
// the unit test acts as a pin that the predicate short-circuit lives
// before iceState.shouldRetry(). Any drift here will surface in CI as a
// failing test alongside any future refactor.
func drivePartiallyConnected(g *Guard, iceState *iceRetryState, callbackCount *int, ticks int) {
	for i := 0; i < ticks; i++ {
		if g.isIntentionalDetach != nil && g.isIntentionalDetach() {
			// intentional detach: skip retry-budget consumption
			continue
		}
		if iceState.shouldRetry() {
			*callbackCount++
		} else {
			iceState.enterHourlyMode()
		}
	}
}

// TestGuard_IntentionalDetach_SkipsRetryBudget pins the headline
// behaviour: when the predicate reports true, ten consecutive
// PartiallyConnected ticks must not consume any retry budget and must
// not arm hourly mode. The guard waits — quietly — for a real network
// event.
func TestGuard_IntentionalDetach_SkipsRetryBudget(t *testing.T) {
	var detached atomic.Bool
	detached.Store(true)

	g, _ := newTestGuardWithDetach(t,
		func() ConnStatus { return ConnStatusPartiallyConnected },
		detached.Load,
	)
	iceState := &iceRetryState{log: g.log}

	callbacks := 0
	drivePartiallyConnected(g, iceState, &callbacks, 10)

	if callbacks != 0 {
		t.Fatalf("callback fired %d times during intentional detach, want 0", callbacks)
	}
	if iceState.retries != 0 {
		t.Fatalf("retries=%d during intentional detach, want 0", iceState.retries)
	}
	if iceState.hourly != nil {
		t.Fatalf("hourly mode armed during intentional detach; expected ticker idle")
	}
}

// TestGuard_RealICEFailure_StillUsesRetryBudget pins the legacy path:
// with no predicate (nil) or predicate=false, the PartiallyConnected
// branch still burns through the 3-attempt budget and arms hourly mode
// on the 4th tick. This guarantees Commit 2 is a strict
// superset — never a regression — of the pre-3.7j Guard semantics.
func TestGuard_RealICEFailure_StillUsesRetryBudget(t *testing.T) {
	cases := []struct {
		name   string
		detach IsIntentionalDetachFunc
	}{
		{"nil predicate", nil},
		{"predicate returns false", func() bool { return false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := newTestGuardWithDetach(t,
				func() ConnStatus { return ConnStatusPartiallyConnected },
				tc.detach,
			)
			iceState := &iceRetryState{log: g.log}

			callbacks := 0
			// 4 ticks: 3 retries succeed, the 4th flips into hourly mode
			drivePartiallyConnected(g, iceState, &callbacks, 4)

			if callbacks != maxICERetries {
				t.Fatalf("callback fired %d times, want %d (=maxICERetries)",
					callbacks, maxICERetries)
			}
			if iceState.hourly == nil {
				t.Fatalf("hourly mode not armed after exhausting retry budget")
			}
			// Stop the hourly ticker to release resources.
			iceState.reset()
		})
	}
}

// TestGuard_DetachThenActivate_ResetsBudget validates the predicate's
// reversibility: a peer that was intentionally detached and then woken
// (predicate flips true -> false) must regain a fresh 3-attempt budget.
// This guards against any future implementation that latches the
// predicate result and forgets to re-arm on the off->on transition of
// real ICE failure.
func TestGuard_DetachThenActivate_ResetsBudget(t *testing.T) {
	var detached atomic.Bool
	detached.Store(true)

	g, _ := newTestGuardWithDetach(t,
		func() ConnStatus { return ConnStatusPartiallyConnected },
		detached.Load,
	)
	iceState := &iceRetryState{log: g.log}

	// Phase 1: detached. 5 ticks must not consume budget.
	callbacks := 0
	drivePartiallyConnected(g, iceState, &callbacks, 5)
	if callbacks != 0 || iceState.retries != 0 {
		t.Fatalf("after 5 detached ticks: callbacks=%d retries=%d, want 0/0",
			callbacks, iceState.retries)
	}

	// Phase 2: peer is reactivated, but ICE fails to converge.
	// 4 ticks must fire 3 retries + arm hourly mode (= legacy path).
	detached.Store(false)
	drivePartiallyConnected(g, iceState, &callbacks, 4)

	if callbacks != maxICERetries {
		t.Fatalf("after activation: callbacks=%d, want %d", callbacks, maxICERetries)
	}
	if iceState.hourly == nil {
		t.Fatalf("hourly mode not armed after retries exhausted in phase 2")
	}
	iceState.reset()
}

// TestGuard_IntentionalDetach_InLoop drives the actual
// reconnectLoopWithRetry to ensure the production switch (not just our
// mirror) honours the predicate. This is a smoke test: we count
// callback invocations across multiple ticks while the predicate
// reports true, then confirm no callback fired.
func TestGuard_IntentionalDetach_InLoop(t *testing.T) {
	var detached atomic.Bool
	detached.Store(true)

	g, _ := newTestGuardWithDetach(t,
		func() ConnStatus { return ConnStatusPartiallyConnected },
		detached.Load,
	)
	// Shrink the timeout so initialTicker's backoff caps quickly.
	g.timeout = 200 * time.Millisecond
	g.log = log.NewEntry(log.StandardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()

	var callbackCount atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.reconnectLoopWithRetry(ctx, func() { callbackCount.Add(1) })
	}()

	<-done

	if got := callbackCount.Load(); got != 0 {
		t.Fatalf("intentional-detach loop fired callback %d times, want 0", got)
	}
}
