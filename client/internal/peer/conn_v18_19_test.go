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
