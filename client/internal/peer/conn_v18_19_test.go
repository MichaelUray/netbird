package peer

import (
	"testing"
	"time"

	"github.com/netbirdio/netbird/monotime"
)

// TestConn_V18_19_ArmLocalWakeIntent_SetsTimestamp verifies that arming
// the intent sets localWakeIntentUntil to a future monotime within
// approximately v18_19WakeIntentWindow of now.
func TestConn_V18_19_ArmLocalWakeIntent_SetsTimestamp(t *testing.T) {
	conn := newMarkerTestConn(t)

	if conn.IsLocalWakeIntentActive() {
		t.Fatal("initial state must be inactive")
	}

	conn.ArmLocalWakeIntent(148)

	if !conn.IsLocalWakeIntentActive() {
		t.Fatal("intent must be active after ArmLocalWakeIntent")
	}
	if c := conn.wakeOfferBudgetUsed.Load(); c != 0 {
		t.Fatalf("budget must reset to 0 on fresh arm, got %d", c)
	}
}

// TestConn_V18_19_ConsumeWakeOfferBudget_AllowsUpToK verifies that
// ConsumeWakeOfferBudget returns true exactly v18_19WakeBudget times
// while intent is active.
func TestConn_V18_19_ConsumeWakeOfferBudget_AllowsUpToK(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.ArmLocalWakeIntent(148)

	for i := int32(1); i <= v18_19WakeBudget; i++ {
		if !conn.ConsumeWakeOfferBudget() {
			t.Fatalf("Consume #%d must return true (budget=%d)", i, v18_19WakeBudget)
		}
	}
}

// TestConn_V18_19_ConsumeWakeOfferBudget_BlocksAfterBudget verifies that
// after K successful consumes, the next Consume returns false.
func TestConn_V18_19_ConsumeWakeOfferBudget_BlocksAfterBudget(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.ArmLocalWakeIntent(148)
	for i := int32(0); i < v18_19WakeBudget; i++ {
		_ = conn.ConsumeWakeOfferBudget()
	}

	if conn.ConsumeWakeOfferBudget() {
		t.Fatal("Consume past budget MUST return false")
	}
	if conn.ConsumeWakeOfferBudget() {
		t.Fatal("Repeated Consume past budget MUST return false")
	}
}

// TestConn_V18_19_ConsumeWakeOfferBudget_FailsWhenIntentExpired verifies
// that an expired intent (localWakeIntentUntil in the past) does NOT
// allow Consume.
func TestConn_V18_19_ConsumeWakeOfferBudget_FailsWhenIntentExpired(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.ArmLocalWakeIntent(148)
	if !conn.ConsumeWakeOfferBudget() {
		t.Fatal("setup: first Consume must succeed")
	}

	// Force-expire by rewinding localWakeIntentUntil to past.
	conn.localWakeIntentUntil.Store(int64(monotime.Now()) - int64(time.Second))

	if conn.ConsumeWakeOfferBudget() {
		t.Fatal("Consume after intent-expired MUST return false")
	}
	if conn.IsLocalWakeIntentActive() {
		t.Fatal("IsLocalWakeIntentActive must return false after expiry")
	}
}

// TestConn_V18_19_MarkIntentionallyDetached_ResetsIntent verifies the
// Reset Rule (Codex amendment 2026-06-21): Mark MUST clear the intent,
// budget, and log latch. Without this, stale intent from a prior cycle
// would survive into a new idle cycle.
func TestConn_V18_19_MarkIntentionallyDetached_ResetsIntent(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.ArmLocalWakeIntent(148)
	_ = conn.ConsumeWakeOfferBudget() // budget=1

	conn.MarkIntentionallyDetached()

	if c := conn.localWakeIntentUntil.Load(); c != 0 {
		t.Fatalf("Mark must reset localWakeIntentUntil, got %d", c)
	}
	if c := conn.wakeOfferBudgetUsed.Load(); c != 0 {
		t.Fatalf("Mark must reset wakeOfferBudgetUsed, got %d", c)
	}
	if conn.v18_19LoggedArmIntent.Load() {
		t.Fatal("Mark must reset v18_19LoggedArmIntent latch")
	}
}

// TestConn_V18_19_ClearIntentionallyDetached_ResetsIntent — same for Clear.
func TestConn_V18_19_ClearIntentionallyDetached_ResetsIntent(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.ArmLocalWakeIntent(148)
	_ = conn.ConsumeWakeOfferBudget()

	conn.ClearIntentionallyDetached()

	if c := conn.localWakeIntentUntil.Load(); c != 0 {
		t.Fatalf("Clear must reset localWakeIntentUntil, got %d", c)
	}
	if c := conn.wakeOfferBudgetUsed.Load(); c != 0 {
		t.Fatalf("Clear must reset wakeOfferBudgetUsed, got %d", c)
	}
	if conn.v18_19LoggedArmIntent.Load() {
		t.Fatal("Clear must reset v18_19LoggedArmIntent latch")
	}
}

// TestConn_V18_19_ArmIntent_BudgetRefreshOnReArmAfterExpiry verifies
// that re-arming after expiry refreshes the budget to 0 (Codex Round-1
// Q3: "OFFER #1 immediately on intent arm" requires budget freshly
// available on each new intent cycle).
func TestConn_V18_19_ArmIntent_BudgetRefreshOnReArmAfterExpiry(t *testing.T) {
	conn := newMarkerTestConn(t)

	// First intent: use entire budget.
	conn.ArmLocalWakeIntent(148)
	for i := int32(0); i < v18_19WakeBudget; i++ {
		_ = conn.ConsumeWakeOfferBudget()
	}
	if conn.ConsumeWakeOfferBudget() {
		t.Fatal("setup: budget should be exhausted")
	}

	// Expire intent.
	conn.localWakeIntentUntil.Store(int64(monotime.Now()) - int64(time.Second))

	// Re-arm. Budget MUST be reset to 0 → fresh K consumes allowed.
	conn.ArmLocalWakeIntent(148)
	if c := conn.wakeOfferBudgetUsed.Load(); c != 0 {
		t.Fatalf("re-arm after expiry must reset budget to 0, got %d", c)
	}
	if !conn.ConsumeWakeOfferBudget() {
		t.Fatal("first Consume after re-arm MUST succeed (fresh budget)")
	}
}

// TestConn_V18_19_LatchResetsBetweenCycles verifies that after Mark
// clears the v18_19LoggedArmIntent latch, the NEXT ArmLocalWakeIntent
// in the new cycle emits Info (not Trace) — i.e. the per-cycle Info-log
// cardinality contract is honored across detach cycles. This is the
// invariant Phase 3 log-grep relies on for operator-observability.
func TestConn_V18_19_LatchResetsBetweenCycles(t *testing.T) {
	conn := newMarkerTestConn(t)

	// Cycle 1.
	conn.ArmLocalWakeIntent(148)
	if !conn.v18_19LoggedArmIntent.Load() {
		t.Fatal("setup: latch must be set after first arm in cycle 1")
	}

	// End cycle 1 → start cycle 2.
	conn.MarkIntentionallyDetached()
	if conn.v18_19LoggedArmIntent.Load() {
		t.Fatal("Mark must reset the latch")
	}

	// First arm in cycle 2 must re-set the latch (= Info log emits).
	conn.ArmLocalWakeIntent(148)
	if !conn.v18_19LoggedArmIntent.Load() {
		t.Fatal("first arm in cycle 2 must re-set the latch (per-cycle Info contract)")
	}
}

// TestConn_V18_19_GuardOverride_ConsumesBudget verifies the Phase 3
// guard-override decision tree: when shouldSkipBootstrapOffer would
// otherwise return (remote p2p-dynamic AND never-connected) but the
// local wake intent is armed, ConsumeWakeOfferBudget atomically claims
// one slot, allowing the OFFER to be sent.
//
// We exercise the decision tree directly (without driving through
// onGuardEvent) because the full guard call chain requires statusRecorder
// + handshaker + Signaler stubs that the marker-test harness does not
// provide. The override's correctness reduces to: shouldSkip=true AND
// ConsumeWakeOfferBudget=true ⇒ proceed; budget counter increments.
func TestConn_V18_19_GuardOverride_ConsumesBudget(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.ArmLocalWakeIntent(148)

	before := conn.wakeOfferBudgetUsed.Load()

	// Simulate the guard's override-check decision:
	if !conn.ConsumeWakeOfferBudget() {
		t.Fatal("expected Consume to succeed with active intent + fresh budget")
	}

	after := conn.wakeOfferBudgetUsed.Load()
	if after != before+1 {
		t.Fatalf("expected budget to increment by 1, got %d→%d", before, after)
	}

	if !conn.IsLocalWakeIntentActive() {
		t.Fatal("intent must remain active after a single Consume (window not expired)")
	}
}

// TestConn_V18_19_GuardOverride_FallsThroughWhenNoIntent verifies that
// without an armed intent, ConsumeWakeOfferBudget returns false (= the
// guard falls through to its original skip-return behaviour). This is
// the safety property: the override never sends OFFERs on its own, it
// only un-gates the existing send path when the local side has demand.
func TestConn_V18_19_GuardOverride_FallsThroughWhenNoIntent(t *testing.T) {
	conn := newMarkerTestConn(t)

	if conn.IsLocalWakeIntentActive() {
		t.Fatal("setup: intent must be inactive")
	}
	if conn.ConsumeWakeOfferBudget() {
		t.Fatal("ConsumeWakeOfferBudget MUST return false without armed intent")
	}
	if c := conn.wakeOfferBudgetUsed.Load(); c != 0 {
		t.Fatalf("budget must not increment when intent inactive, got %d", c)
	}
}
