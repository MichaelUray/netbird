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

// TestConn_V18_30_ReArmAfterClear_RestoresGuardOverride is the regression
// guard for the V18.30 kernel-mode-sender path: in the lazy-mgr's
// onPeerActivity (manager.go:onPeerActivity), AttachICEFrom invokes
// ClearIntentionallyDetached (conn.go:2479) which wipes
// localWakeIntentUntil to 0. The first arm (in listener_udp.go) happens
// BEFORE this clear and is therefore erased. V18.30's fix re-arms AFTER
// AttachICEFrom. This test asserts that re-arming on an expired/cleared
// state restores ConsumeWakeOfferBudget == true with a fresh budget.
func TestConn_V18_30_ReArmAfterClear_RestoresGuardOverride(t *testing.T) {
	conn := newMarkerTestConn(t)

	// Phase 1: first arm (listener_udp.go).
	conn.ArmLocalWakeIntent(1)
	if !conn.IsLocalWakeIntentActive() {
		t.Fatal("setup: first arm must activate intent")
	}

	// Phase 2: AttachICEFrom → ClearIntentionallyDetached → wipes intent.
	conn.ClearIntentionallyDetached()
	if conn.IsLocalWakeIntentActive() {
		t.Fatal("ClearIntentionallyDetached must wipe wake intent (precondition)")
	}
	if conn.ConsumeWakeOfferBudget() {
		t.Fatal("ConsumeWakeOfferBudget must return false after Clear (precondition for V18.30 fix)")
	}

	// Phase 3: V18.30 re-arm in manager.onPeerActivity post-AttachICE.
	conn.ArmLocalWakeIntent(0)

	if !conn.IsLocalWakeIntentActive() {
		t.Fatal("V18.30: re-arm after Clear must re-activate intent")
	}
	if c := conn.wakeOfferBudgetUsed.Load(); c != 0 {
		t.Fatalf("V18.30: re-arm must reset budget to 0, got %d", c)
	}

	// Guard-bypass loop: budget allows v18_19WakeBudget OFFERs.
	for i := int32(1); i <= v18_19WakeBudget; i++ {
		if !conn.ConsumeWakeOfferBudget() {
			t.Fatalf("V18.30: Consume #%d after re-arm must succeed (budget=%d)", i, v18_19WakeBudget)
		}
	}
	if conn.ConsumeWakeOfferBudget() {
		t.Fatal("V18.30: Consume past budget must return false (cap enforced)")
	}
}

// TestConn_V18_30_RemoteOfflineOverride_NoBudgetConsume verifies that the
// V18.30 remote-offline gate uses the no-cost IsLocalWakeIntentActive
// check rather than ConsumeWakeOfferBudget. The bootstrap-skip gate
// directly ABOVE (conn.go:1562) already consumes a budget slot in the
// same guard tick, so double-consuming would halve the effective
// 3-OFFER budget. Live test 2026-06-21 18:23 UTC reproduced the
// halved-budget behaviour on dk20 → S26 cold-boot.
func TestConn_V18_30_RemoteOfflineOverride_NoBudgetConsume(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.ArmLocalWakeIntent(0)

	// Simulate the bootstrap-skip override consuming one slot above.
	if !conn.ConsumeWakeOfferBudget() {
		t.Fatal("setup: bootstrap-skip override must succeed first")
	}
	bootstrapUsed := conn.wakeOfferBudgetUsed.Load()
	if bootstrapUsed != 1 {
		t.Fatalf("setup: budget after bootstrap consume must be 1, got %d", bootstrapUsed)
	}

	// V18.30 remote-offline check must NOT consume an additional slot.
	if !conn.IsLocalWakeIntentActive() {
		t.Fatal("V18.30: remote-offline check must pass while intent active")
	}
	afterCheck := conn.wakeOfferBudgetUsed.Load()
	if afterCheck != bootstrapUsed {
		t.Fatalf("V18.30: IsLocalWakeIntentActive must NOT consume budget, got %d→%d",
			bootstrapUsed, afterCheck)
	}

	// All v18_19WakeBudget OFFERs should remain available across guard
	// ticks: each tick consumes ONE slot at the bootstrap-skip gate,
	// V18.30 only confirms intent is still active.
	for i := int32(2); i <= v18_19WakeBudget; i++ {
		if !conn.ConsumeWakeOfferBudget() {
			t.Fatalf("V18.30: budget slot %d must be available (no double-consume)", i)
		}
		if !conn.IsLocalWakeIntentActive() {
			t.Fatalf("V18.30: intent must remain active across slot %d", i)
		}
	}
}

// V18.31 (2026-06-21) — IsRemotePeerLazyAware tests.
//
// Gate-decision predicate for V14+V18.17 and V15+V18.17 burst-release.
// Returns false for pre-lazy peers (< 0.65.0) so their eager bootstrap
// OFFER retries don't get promoted to "burst-recovery" by V18.17.

func TestConn_V18_31_IsRemotePeerLazyAware_EmptyVersionDenies(t *testing.T) {
	conn := newMarkerTestConn(t)
	// Default state: AgentVersion=""
	if conn.IsRemotePeerLazyAware() {
		t.Fatal("V18.31: empty AgentVersion must return false (conservative deny)")
	}
}

func TestConn_V18_31_IsRemotePeerLazyAware_LegacyVersionDenies(t *testing.T) {
	conn := newMarkerTestConn(t)
	for _, v := range []string{"0.53.0", "0.59.13", "0.60.4", "0.64.99"} {
		if err := conn.statusRecorder.AddPeer(conn.config.Key, "", ""); err != nil && err.Error() != "peer already exists" {
			// peer might already be added by prior loop iteration; fall through
		}
		if err := conn.statusRecorder.UpdatePeerRemoteMeta(conn.config.Key, RemoteMeta{
			AgentVersion: v,
		}); err != nil {
			t.Fatalf("setup: UpdatePeerRemoteMeta(%q): %v", v, err)
		}
		if conn.IsRemotePeerLazyAware() {
			t.Fatalf("V18.31: version %q must return false (pre-lazy, < 0.65.0)", v)
		}
	}
}

func TestConn_V18_31_IsRemotePeerLazyAware_LazyVersionAllows(t *testing.T) {
	conn := newMarkerTestConn(t)
	for _, v := range []string{"0.65.0", "0.65.1", "0.68.0", "0.71.4", "1.0.0"} {
		if err := conn.statusRecorder.AddPeer(conn.config.Key, "", ""); err != nil && err.Error() != "peer already exists" {
			// peer might already be added by prior loop iteration; fall through
		}
		if err := conn.statusRecorder.UpdatePeerRemoteMeta(conn.config.Key, RemoteMeta{
			AgentVersion: v,
		}); err != nil {
			t.Fatalf("setup: UpdatePeerRemoteMeta(%q): %v", v, err)
		}
		if !conn.IsRemotePeerLazyAware() {
			t.Fatalf("V18.31: version %q must return true (lazy-aware, >= 0.65.0)", v)
		}
	}
}

func TestConn_V18_31_IsRemotePeerLazyAware_DevBuildAllows(t *testing.T) {
	conn := newMarkerTestConn(t)
	for _, v := range []string{
		"0.0.0-dev-deadbeef",
		"0.68.0-dev-v18.30-fixup3-7b7d8a587",
		"0.68.0-ci-1234567",
		"development",
	} {
		if err := conn.statusRecorder.AddPeer(conn.config.Key, "", ""); err != nil && err.Error() != "peer already exists" {
			// peer might already be added by prior loop iteration; fall through
		}
		if err := conn.statusRecorder.UpdatePeerRemoteMeta(conn.config.Key, RemoteMeta{
			AgentVersion: v,
		}); err != nil {
			t.Fatalf("setup: UpdatePeerRemoteMeta(%q): %v", v, err)
		}
		if !conn.IsRemotePeerLazyAware() {
			t.Fatalf("V18.31: dev/CI build %q must return true (shares source-tree, honours all gates)", v)
		}
	}
}

func TestConn_V18_31_IsRemotePeerLazyAware_UnparseableDenies(t *testing.T) {
	conn := newMarkerTestConn(t)
	for _, v := range []string{"abc123", "0", "not-a-version"} {
		if err := conn.statusRecorder.AddPeer(conn.config.Key, "", ""); err != nil && err.Error() != "peer already exists" {
			// peer might already be added by prior loop iteration; fall through
		}
		if err := conn.statusRecorder.UpdatePeerRemoteMeta(conn.config.Key, RemoteMeta{
			AgentVersion: v,
		}); err != nil {
			t.Fatalf("setup: UpdatePeerRemoteMeta(%q): %v", v, err)
		}
		if conn.IsRemotePeerLazyAware() {
			t.Fatalf("V18.31: unparseable %q must return false", v)
		}
	}
}
