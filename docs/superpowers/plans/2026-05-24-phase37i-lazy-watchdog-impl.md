# Phase-3.7i Lazy-Watchdog Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implementiere den 2-Case-Reconcile-Watchdog für die Phase-3.7i Lazy-Connection-State-Machine, der notifyChan-Drops + post-Close-Hang-Stuck-States peer-level heilt.

**Architecture:** 6 Commits auf neuem Branch `pr/g-phase3.7i-lazy-watchdog` mit Base `phase3.7i-runtime-bugfixes-v0.5`. Jeder Commit ist einzeln reviewbar + grün-getestet. Watchdog detektiert ZWEI Stuck-States: (a) `watcherInactivity + relayDrops > 0 + disconnected` und (b) `watcherActivity + !hasListener + disconnected`. Recovery via state-flip + Listener-Arm; KEIN Close-Call (vermeidet conn.mu/managedPeersMu Deadlock).

**Tech Stack:** Go 1.x, NetBird client/, sync/atomic, sirupsen/logrus, testify, peerid.ConnID, go test -race.

**Source-of-Truth Spec:** [2026-05-22-phase37i-lazy-watchdog-spec.md](2026-05-22-phase37i-lazy-watchdog-spec.md) v0.7.2 (commit `88c8a3d6b`).

**Working directory:** `/home/ai-agent/projects/netbird/`

---

## File Structure

| Datei | Aktion | Verantwortung |
|---|---|---|
| `client/internal/peer/conn.go` | Modify | + `TransportSnapshot()` (Stufe 5) ~10 LOC |
| `client/internal/peer/conn_transport_snapshot_test.go` | Create | Stufe-5 Tests |
| `client/internal/lazyconn/activity/manager.go` | Modify | + `HasPeer(connID)` (Stufe 6) ~10 LOC |
| `client/internal/lazyconn/activity/listener_bind_test.go` | Modify | Mock-Race-Fix Pre-Implementation |
| `client/internal/lazyconn/activity/manager_test.go` | Modify | Stufe-6 Tests |
| `client/internal/lazyconn/inactivity/manager.go` | Modify | + `DropCounters` + Drop-Log (Stufe 1) ~30 LOC |
| `client/internal/lazyconn/inactivity/manager_test.go` | Modify | Stufe-1 Tests |
| `client/internal/lazyconn/manager/manager.go` | Modify | Stufe 0 + 3-Refactor + 2-Watchdog + 4-Wiring ~250 LOC |
| `client/internal/lazyconn/manager/recovery_test.go` | Create | Stufe-3 Refactor-Tests |
| `client/internal/lazyconn/manager/watchdog_test.go` | Create | Stufe-2 Watchdog-Tests |
| `client/internal/lazyconn/manager/panic_recovery_test.go` | Create | Stufe-0 Panic-Tests |

**Eines der 6 Commits ist KEIN bestehender Source-File-Touch:**
- Commit 4 (`activity: add HasPeer + fix mockEndpointManager race`) berührt zusätzlich noch `listener_bind_test.go` für den Pre-Implementation Test-Mock-Race-Fix.

---

## Task 0: Branch Setup

**Files:**
- Create: `pr/g-phase3.7i-lazy-watchdog` branch from `phase3.7i-runtime-bugfixes-v0.5`

- [ ] **Step 0.1: Checkout base + create work branch**

```bash
cd /home/ai-agent/projects/netbird
git fetch origin
git checkout phase3.7i-runtime-bugfixes-v0.5
git pull --ff-only origin phase3.7i-runtime-bugfixes-v0.5
git checkout -b pr/g-phase3.7i-lazy-watchdog
```

Expected: `Switched to a new branch 'pr/g-phase3.7i-lazy-watchdog'`

- [ ] **Step 0.2: Sanity-check base compiles + tests pass**

```bash
go build ./client/...
go test -race -timeout 180s ./client/internal/lazyconn/... ./client/internal/peer/ -count=1
```

Expected: all green. If anything fails on base branch, STOP and investigate before adding new code.

---

## Task 1 (Commit 1): peer/conn — TransportSnapshot Accessor (Stufe 5)

**Files:**
- Modify: `client/internal/peer/conn.go` (add method, +~15 LOC)
- Create: `client/internal/peer/conn_transport_snapshot_test.go` (~80 LOC)

**Why first:** TransportSnapshot is a dependency for Stufe-2 Watchdog. Lock-free atomic read.

- [ ] **Step 1.1: Write failing test for both-connected case**

Create `client/internal/peer/conn_transport_snapshot_test.go`:

```go
package peer

import (
	"testing"

	"github.com/netbirdio/netbird/client/internal/peer/worker"
)

func TestTransportSnapshot_BothConnected(t *testing.T) {
	conn := &Conn{
		statusICE:   worker.NewAtomicWorkerStatus(),
		statusRelay: worker.NewAtomicWorkerStatus(),
	}
	conn.statusICE.Set(worker.StatusConnected)
	conn.statusRelay.Set(worker.StatusConnected)

	iceDisc, relayDisc := conn.TransportSnapshot()
	if iceDisc || relayDisc {
		t.Fatalf("expected (false, false), got (%v, %v)", iceDisc, relayDisc)
	}
}
```

- [ ] **Step 1.2: Run test to verify it fails**

Run: `go test ./client/internal/peer/ -run TestTransportSnapshot_BothConnected -count=1`
Expected: FAIL with `undefined: conn.TransportSnapshot` (compile error).

- [ ] **Step 1.3: Implement TransportSnapshot**

Edit `client/internal/peer/conn.go`. Find an appropriate insertion point near other public read-only accessors (e.g. after `IsConnected()` or before `Close()`). Add:

```go
// TransportSnapshot returns the current connectivity state of both
// transports (ICE and Relay) as boolean flags indicating "not connected".
// Intended for external watchdogs and telemetry: pure read, no logging,
// no state mutation, no lock acquisition (atomic loads).
//
// Returns (iceDisconnected, relayDisconnected) where each bool is true if
// that transport is NOT in StatusConnected.
func (conn *Conn) TransportSnapshot() (iceDisconnected, relayDisconnected bool) {
	return conn.statusICE.Get() != worker.StatusConnected,
		conn.statusRelay.Get() != worker.StatusConnected
}
```

Verify `worker` is already imported in `conn.go`. If not, add: `"github.com/netbirdio/netbird/client/internal/peer/worker"`.

- [ ] **Step 1.4: Run test to verify it passes**

Run: `go test ./client/internal/peer/ -run TestTransportSnapshot_BothConnected -count=1`
Expected: PASS.

- [ ] **Step 1.5: Add remaining test cases**

Append to `client/internal/peer/conn_transport_snapshot_test.go`:

```go
func TestTransportSnapshot_BothDisconnected(t *testing.T) {
	conn := &Conn{
		statusICE:   worker.NewAtomicWorkerStatus(),
		statusRelay: worker.NewAtomicWorkerStatus(),
	}
	// Default is StatusDisconnected (iota = 0)
	iceDisc, relayDisc := conn.TransportSnapshot()
	if !iceDisc || !relayDisc {
		t.Fatalf("expected (true, true), got (%v, %v)", iceDisc, relayDisc)
	}
}

func TestTransportSnapshot_RelayOnly(t *testing.T) {
	conn := &Conn{
		statusICE:   worker.NewAtomicWorkerStatus(),
		statusRelay: worker.NewAtomicWorkerStatus(),
	}
	conn.statusICE.Set(worker.StatusDisconnected)
	conn.statusRelay.Set(worker.StatusConnected)

	iceDisc, relayDisc := conn.TransportSnapshot()
	if !iceDisc || relayDisc {
		t.Fatalf("expected (true, false), got (%v, %v)", iceDisc, relayDisc)
	}
}

func TestTransportSnapshot_RaceSafe(t *testing.T) {
	conn := &Conn{
		statusICE:   worker.NewAtomicWorkerStatus(),
		statusRelay: worker.NewAtomicWorkerStatus(),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10000; i++ {
			conn.statusICE.Set(worker.StatusConnected)
			conn.statusRelay.Set(worker.StatusDisconnected)
		}
	}()
	for i := 0; i < 10000; i++ {
		_, _ = conn.TransportSnapshot()
	}
	<-done
}
```

- [ ] **Step 1.6: Verify the constructor signature**

```bash
grep -n "func NewAtomicWorkerStatus" client/internal/peer/worker/state.go
```

If `NewAtomicWorkerStatus` does not exist as a public constructor, find the correct constructor pattern (it might be `&worker.AtomicWorkerStatus{}` direct zero-value, since `atomic.Int32` zero-value is valid). Adjust test setup accordingly.

- [ ] **Step 1.7: Run full Stufe-5 test set with -race**

Run: `go test -race ./client/internal/peer/ -run TestTransportSnapshot -count=1 -timeout 60s`
Expected: PASS, no race reports.

- [ ] **Step 1.8: Commit**

```bash
cd /home/ai-agent/projects/netbird
git add client/internal/peer/conn.go client/internal/peer/conn_transport_snapshot_test.go
GIT_AUTHOR_NAME="Michael Uray" GIT_AUTHOR_EMAIL="25169478+MichaelUray@users.noreply.github.com" \
GIT_COMMITTER_NAME="Michael Uray" GIT_COMMITTER_EMAIL="25169478+MichaelUray@users.noreply.github.com" \
git commit -m "peer/conn: add TransportSnapshot accessor for external watchdogs

Adds a lock-free read-only accessor returning (iceDisconnected,
relayDisconnected) booleans by reading the atomic statusICE/statusRelay
worker states. Pure read, no logging, no state mutation. Intended for
the upcoming lazy-watchdog (Phase 3.7i) to classify peers without
acquiring conn.mu.

See docs/superpowers/plans/2026-05-22-phase37i-lazy-watchdog-spec.md
Section 5.2 Stufe 5 for rationale (replaces fictional GetICEState/
GetRelayState API)."
```

---

## Task 2 (Commit 2): lazyconn/manager — Panic-Recovery (Stufe 0)

**Files:**
- Modify: `client/internal/lazyconn/manager/manager.go` (wrap handler calls in Start)
- Create: `client/internal/lazyconn/manager/panic_recovery_test.go` (~80 LOC)

**Why second:** Pure hardening, smallest change, can land independently. Does NOT fix the stuck-state (Codex round-2 verdict) but prevents future daemon crashes.

- [ ] **Step 2.1: Write failing test for panic in onPeerActivity**

Create `client/internal/lazyconn/manager/panic_recovery_test.go`. Skeleton (adapt to existing test-harness):

```go
package manager

import (
	"context"
	"testing"
	"time"

	"github.com/netbirdio/netbird/client/internal/lazyconn"
	peerid "github.com/netbirdio/netbird/client/internal/peer/id"
)

// TestManagerStart_PanicInOnPeerActivity_DoesNotKillConsumer: inject a
// panic via a test-only hook; assert that Start's select loop continues
// to drain subsequent events.
func TestManagerStart_PanicInOnPeerActivity_DoesNotKillConsumer(t *testing.T) {
	// Use the existing test harness pattern from manager_test.go.
	// The strategy: send a peerConnID that triggers a panic in
	// onPeerActivity (e.g. via a test-injected fault), then send a
	// second peerConnID that the recovered loop should still process.
	t.Skip("TODO: implement once test harness exposes panic-injection hook")
}
```

Note: Read `client/internal/lazyconn/manager/manager_test.go` first to understand the existing test harness (mock peerStore, mock wgIface). Adapt the panic test to that pattern. The simplest panic-injection is to use a peerConnID that doesn't exist in `managedPeersByConnID` and assert no goroutine-leak; but since the existing code already log-warns on missing peers without panic, you may need to inject via a test-only `panicOnNextActivity bool` field.

**Decision**: if panic-injection requires significant test-harness work, scope this commit to: just add the `defer recover()` wrappers + lint/compile + a smoke test that confirms `Start()` still returns on `ctx.Done()`. Document the missing panic-injection-test as a follow-up in the commit message.

- [ ] **Step 2.2: Run test to verify it fails (or is skipped)**

Run: `go test ./client/internal/lazyconn/manager/ -run TestManagerStart_PanicInOnPeerActivity -count=1`
Expected: SKIP (per Step 2.1 placeholder) OR FAIL.

- [ ] **Step 2.3: Wrap handler calls with defer recover() in Start**

Edit `client/internal/lazyconn/manager/manager.go` around line 180-190:

```go
// Start starts the manager and listens for peer activity and inactivity events
func (m *Manager) Start(ctx context.Context) {
	defer m.close()

	if m.inactivityManager != nil {
		go m.inactivityManager.Start(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case peerConnID := <-m.activityManager.OnActivityChan:
			m.safeOnPeerActivity(peerConnID)
		case peerIDs := <-m.inactivityManager.InactivePeersChan():
			m.safeOnPeerInactivityTimedOut(peerIDs)
		}
	}
}

// safeOnPeerActivity wraps onPeerActivity with a panic recovery so the
// consumer goroutine survives bugs in downstream handlers. v0.7 Stufe 0
// hardening — does NOT fix stuck-state symptoms (a Go panic crashes
// the whole process unless recovered here), but prevents future
// regressions.
func (m *Manager) safeOnPeerActivity(peerConnID peerid.ConnID) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("lazyconn: panic in onPeerActivity (peerConnID=%v): %v", peerConnID, r)
		}
	}()
	m.onPeerActivity(peerConnID)
}

// safeOnPeerInactivityTimedOut wraps onPeerInactivityTimedOut with a
// panic recovery (see safeOnPeerActivity).
func (m *Manager) safeOnPeerInactivityTimedOut(peerIDs map[string]struct{}) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("lazyconn: panic in onPeerInactivityTimedOut: %v", r)
		}
	}()
	m.onPeerInactivityTimedOut(peerIDs)
}
```

- [ ] **Step 2.4: Verify build + run package tests**

Run: `go test -race ./client/internal/lazyconn/manager/ -count=1 -timeout 60s`
Expected: all existing tests still PASS.

- [ ] **Step 2.5: Implement actual panic-injection test (if test-harness permits)**

If you can extend the test harness without too much yak-shaving: replace the t.Skip in Step 2.1 with a real test. Use a `t.Cleanup` to restore state. Otherwise: leave as Skip with a TODO and proceed.

- [ ] **Step 2.6: Run full test set**

Run: `go test -race ./client/internal/lazyconn/manager/ -count=1 -timeout 120s`
Expected: PASS.

- [ ] **Step 2.7: Commit**

```bash
git add client/internal/lazyconn/manager/manager.go client/internal/lazyconn/manager/panic_recovery_test.go
GIT_AUTHOR_NAME="Michael Uray" GIT_AUTHOR_EMAIL="25169478+MichaelUray@users.noreply.github.com" \
GIT_COMMITTER_NAME="Michael Uray" GIT_COMMITTER_EMAIL="25169478+MichaelUray@users.noreply.github.com" \
git commit -m "lazyconn/manager: defer recover() around consumer loop handlers

Wraps onPeerActivity and onPeerInactivityTimedOut in safeOn* helpers
with defer recover() so a panic in any handler logs an error rather
than crashing the whole NetBird daemon.

Pure hardening — does NOT fix the lazy-state stuck symptoms (a panic
crashes the entire Go process unless recovered here, so this prevents
future regressions but cannot resurrect an already-dead daemon).

See docs/superpowers/plans/2026-05-22-phase37i-lazy-watchdog-spec.md
Section 5.2 Stufe 0."
```

---

## Task 3 (Commit 3): lazyconn/manager — Split state-mutation from blocking I/O (Stufe 3 Refactor)

**Files:**
- Modify: `client/internal/lazyconn/manager/manager.go` (~50 LOC refactor of onPeerInactivityTimedOut + 2 new helpers)
- Create: `client/internal/lazyconn/manager/recovery_test.go` (~150 LOC)

**Why third:** Pure refactor with behavior preserved (close still synchronous after state-flip + unlock). Establishes the helpers that Commit 5 (Watchdog) will reuse.

- [ ] **Step 3.1: Identify the current onPeerInactivityTimedOut**

Read `client/internal/lazyconn/manager/manager.go` around line 629 (per spec) to confirm the current implementation. Note the existing `PeerConnIdle` call site that runs UNDER `managedPeersMu` (the TODO `potentially can be optimized` line).

- [ ] **Step 3.2: Write failing test for the new helper**

Create `client/internal/lazyconn/manager/recovery_test.go`:

```go
package manager

import (
	"testing"
)

// TestTransitionToActivityWatcherStateOnly_HappyPath: verify that the
// state-only helper flips expectedWatcher to watcherActivity and removes
// the peer from the inactivity manager, but does NOT call PeerConnIdle.
func TestTransitionToActivityWatcherStateOnly_HappyPath(t *testing.T) {
	t.Skip("TODO: implement once a unit-test harness for managedPeer exists")
}
```

If `manager_test.go` already has a builder/harness for setting up `*Manager` with mocks, use that pattern. Otherwise scope the test to call the helper directly with a hand-rolled `*managedPeer`.

- [ ] **Step 3.3: Implement `transitionToActivityWatcherStateOnly`**

Edit `client/internal/lazyconn/manager/manager.go`. Add (near other private helpers):

```go
// transitionToActivityWatcherStateOnly performs the non-blocking state-machine
// part of the watcherInactivity → watcherActivity transition (expectedWatcher
// flip + RemovePeer from inactivity manager).
// Caller MUST hold m.managedPeersMu. No I/O here.
func (m *Manager) transitionToActivityWatcherStateOnly(mp *managedPeer) {
	mp.peerCfg.Log.Infof("transition to watcherActivity (state-only) from %v", mp.expectedWatcher)
	mp.expectedWatcher = watcherActivity
	m.inactivityManager.RemovePeer(mp.peerCfg.PublicKey)
}
```

- [ ] **Step 3.4: Implement `armActivityListener`**

Add right below:

```go
// armActivityListener installs the activity monitor for a peer. Idempotent
// when called against a peer that already has an active monitor (the
// activity manager internally guards against double-arm). No lock required
// (activityManager has its own internal mutex).
func (m *Manager) armActivityListener(mp *managedPeer) {
	if err := m.activityManager.MonitorPeerActivity(*mp.peerCfg); err != nil {
		mp.peerCfg.Log.Errorf("failed to create activity monitor: %v", err)
	}
}
```

- [ ] **Step 3.5: Refactor `onPeerInactivityTimedOut`**

Replace the existing implementation with:

```go
func (m *Manager) onPeerInactivityTimedOut(peerIDs map[string]struct{}) {
	// Phase 1: short lock — state mutations only
	m.managedPeersMu.Lock()
	toTransition := make([]*managedPeer, 0, len(peerIDs))
	for peerID := range peerIDs {
		peerCfg, ok := m.managedPeers[peerID]
		if !ok {
			continue
		}
		mp, ok := m.managedPeersByConnID[peerCfg.PeerConnID]
		if !ok {
			continue
		}
		if mp.expectedWatcher != watcherInactivity {
			mp.peerCfg.Log.Warnf("ignore inactivity event")
			continue
		}
		if m.shouldDeferIdleForHA(peerIDs, mp.peerCfg.PublicKey) {
			mp.peerCfg.Log.Infof("defer inactivity due to active HA group peers")
			continue
		}
		mp.peerCfg.Log.Infof("connection timed out")
		m.transitionToActivityWatcherStateOnly(mp)
		toTransition = append(toTransition, mp)
	}
	m.managedPeersMu.Unlock()

	// Phase 2: blocking I/O outside lock. Sequential close → listener-arm
	// (v0.3 ordering, restored after v0.4 race finding). If Close hangs
	// here, the peer enters the stuck state (watcherActivity without
	// listener) and the watchdog (Stufe 2, separate commit) will pick it
	// up. The hung close goroutine is then leaked but does not block
	// further recovery.
	for _, mp := range toTransition {
		m.peerStore.PeerConnIdle(mp.peerCfg.PublicKey)
		m.armActivityListener(mp)
	}
}
```

- [ ] **Step 3.6: Verify build + existing tests still pass**

Run: `go test -race ./client/internal/lazyconn/manager/ -count=1 -timeout 120s`
Expected: all existing tests PASS. The behavioural change (lock-released-during-close) MUST NOT regress existing inactivity-flow tests.

- [ ] **Step 3.7: Write IOOutsideLock test**

Append to `recovery_test.go`:

```go
// TestOnPeerInactivityTimedOut_AfterRefactor_IOOutsideLock: instrument
// peerStore.PeerConnIdle with a sleep, assert that managedPeersMu is
// NOT held during that sleep (another goroutine can acquire it).
func TestOnPeerInactivityTimedOut_AfterRefactor_IOOutsideLock(t *testing.T) {
	t.Skip("TODO: needs test-only PeerConnIdle hook; defer to Commit 5 test harness")
}
```

If the existing harness already supports a PeerConnIdle hook (check `peerstore_mock.go` or similar), implement the test fully. Otherwise leave skipped.

- [ ] **Step 3.8: Run full package tests with -race**

Run: `go test -race ./client/internal/lazyconn/manager/ -count=1 -timeout 120s`
Expected: PASS.

- [ ] **Step 3.9: Commit**

```bash
git add client/internal/lazyconn/manager/manager.go client/internal/lazyconn/manager/recovery_test.go
GIT_AUTHOR_NAME="Michael Uray" GIT_AUTHOR_EMAIL="25169478+MichaelUray@users.noreply.github.com" \
GIT_COMMITTER_NAME="Michael Uray" GIT_COMMITTER_EMAIL="25169478+MichaelUray@users.noreply.github.com" \
git commit -m "lazyconn/manager: split state-mutation from blocking I/O in inactivity-timeout path

Refactors onPeerInactivityTimedOut into a two-phase pattern:
- Phase 1: short managedPeersMu hold for the state-flip + HA-defer
  check + inactivity-manager RemovePeer
- Phase 2: blocking I/O (PeerConnIdle + armActivityListener) sequentially
  AFTER unlock, restoring v0.3 close-then-listen ordering

Extracts two helpers used here and (in a follow-up commit) by the
watchdog: transitionToActivityWatcherStateOnly (under lock) and
armActivityListener (lock-free, idempotent).

Behavioural change: PeerConnIdle no longer runs under managedPeersMu
(the existing TODO 'potentially can be optimized' is now resolved).
If PeerConnIdle hangs structurally, the peer enters the stuck state
(watcherActivity + no listener) which the upcoming watchdog (Stufe 2)
heals.

See docs/superpowers/plans/2026-05-22-phase37i-lazy-watchdog-spec.md
Section 5.2 Stufe 3 (v0.5 + v0.7.1 refinements)."
```

---

## Task 4 (Commit 4): lazyconn/activity — HasPeer + mock-Race-Fix (Stufe 6)

**Files:**
- Modify: `client/internal/lazyconn/activity/listener_bind_test.go` (Pre-Implementation race fix)
- Modify: `client/internal/lazyconn/activity/manager.go` (add `HasPeer`)
- Modify: `client/internal/lazyconn/activity/manager_test.go` (or new file `has_peer_test.go`) — Stufe-6 tests

**Why fourth:** Stufe-2 Watchdog depends on `HasPeer`. Mock-Race-Fix is bundled to keep the `go test -race` activity-package green (Codex round-7 verification finding).

- [ ] **Step 4.1: Reproduce the existing race**

Run: `go test -race ./client/internal/lazyconn/activity/ -count=1 -timeout 60s`
Expected: race report involving `mockEndpointManager` in `listener_bind_test.go` lines 37/41 (concurrent map access on `m.endpoints`).

- [ ] **Step 4.2: Inspect `mockEndpointManager`**

Read `client/internal/lazyconn/activity/listener_bind_test.go`. The mock has `endpoints map[netip.Addr]net.Conn` accessed via `SetEndpoint`/`RemoveEndpoint`/`GetEndpoint` without synchronization. Tests run these concurrently.

- [ ] **Step 4.3: Add a sync.Mutex to mockEndpointManager**

Edit `client/internal/lazyconn/activity/listener_bind_test.go`. Add a mutex field to `mockEndpointManager` and guard all three methods:

```go
type mockEndpointManager struct {
	mu        sync.Mutex
	endpoints map[netip.Addr]net.Conn
}

func (m *mockEndpointManager) SetEndpoint(fakeIP netip.Addr, conn net.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endpoints[fakeIP] = conn
}

func (m *mockEndpointManager) RemoveEndpoint(fakeIP netip.Addr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.endpoints, fakeIP)
}

func (m *mockEndpointManager) GetEndpoint(fakeIP netip.Addr) net.Conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.endpoints[fakeIP]
}
```

Add `"sync"` import if not already present.

- [ ] **Step 4.4: Verify race is gone**

Run: `go test -race ./client/internal/lazyconn/activity/ -count=1 -timeout 60s`
Expected: PASS, no race reports.

- [ ] **Step 4.5: Write failing test for `HasPeer` on empty Manager**

Edit (or create) `client/internal/lazyconn/activity/manager_test.go` (or add new `has_peer_test.go`):

```go
func TestActivityManager_HasPeer_Empty(t *testing.T) {
	mgr := NewManager(newMockWgIface(t)) // adapt to existing test harness
	var dummyConnID peerid.ConnID // zero-value
	if mgr.HasPeer(dummyConnID) {
		t.Fatalf("expected HasPeer == false on empty Manager, got true")
	}
}
```

Note: `newMockWgIface(t)` is a placeholder for whatever test setup the existing `manager_test.go` uses. Adapt.

- [ ] **Step 4.6: Run test to verify it fails**

Run: `go test ./client/internal/lazyconn/activity/ -run TestActivityManager_HasPeer_Empty -count=1`
Expected: FAIL with `undefined: mgr.HasPeer`.

- [ ] **Step 4.7: Implement `HasPeer`**

Edit `client/internal/lazyconn/activity/manager.go`. Add right after the existing `RemovePeer` method:

```go
// HasPeer reports whether an activity listener is currently registered
// for the given peer connection ID. Intended for the lazyconn-Manager
// watchdog (Phase 3.7i) to distinguish "active and listening" from
// "active but stuck after hung Close".
func (m *Manager) HasPeer(connID peerid.ConnID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.peers[connID]
	return ok
}
```

- [ ] **Step 4.8: Run test to verify it passes**

Run: `go test ./client/internal/lazyconn/activity/ -run TestActivityManager_HasPeer_Empty -count=1`
Expected: PASS.

- [ ] **Step 4.9: Add remaining HasPeer tests**

Append (need to inspect existing test patterns to construct PeerConfig with valid PeerConnID + a WgInterface mock that lets MonitorPeerActivity succeed):

```go
func TestActivityManager_HasPeer_AfterMonitor(t *testing.T) {
	mgr := NewManager(newMockWgIface(t))
	cfg := newTestPeerConfig(t) // adapt
	if err := mgr.MonitorPeerActivity(cfg); err != nil {
		t.Fatalf("MonitorPeerActivity: %v", err)
	}
	if !mgr.HasPeer(cfg.PeerConnID) {
		t.Fatalf("expected HasPeer == true after MonitorPeerActivity, got false")
	}
}

func TestActivityManager_HasPeer_AfterRemove(t *testing.T) {
	mgr := NewManager(newMockWgIface(t))
	cfg := newTestPeerConfig(t)
	if err := mgr.MonitorPeerActivity(cfg); err != nil {
		t.Fatalf("MonitorPeerActivity: %v", err)
	}
	mgr.RemovePeer(cfg.Log, cfg.PeerConnID)
	if mgr.HasPeer(cfg.PeerConnID) {
		t.Fatalf("expected HasPeer == false after RemovePeer, got true")
	}
}

func TestActivityManager_HasPeer_RaceSafe(t *testing.T) {
	mgr := NewManager(newMockWgIface(t))
	cfg := newTestPeerConfig(t)
	if err := mgr.MonitorPeerActivity(cfg); err != nil {
		t.Fatalf("MonitorPeerActivity: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			mgr.RemovePeer(cfg.Log, cfg.PeerConnID)
			_ = mgr.MonitorPeerActivity(cfg)
		}
	}()
	for i := 0; i < 1000; i++ {
		_ = mgr.HasPeer(cfg.PeerConnID)
	}
	<-done
}
```

- [ ] **Step 4.10: Run full activity-package tests with -race**

Run: `go test -race ./client/internal/lazyconn/activity/ -count=1 -timeout 120s`
Expected: PASS, no race reports.

- [ ] **Step 4.11: Commit**

```bash
git add client/internal/lazyconn/activity/manager.go \
        client/internal/lazyconn/activity/listener_bind_test.go \
        client/internal/lazyconn/activity/manager_test.go
GIT_AUTHOR_NAME="Michael Uray" GIT_AUTHOR_EMAIL="25169478+MichaelUray@users.noreply.github.com" \
GIT_COMMITTER_NAME="Michael Uray" GIT_COMMITTER_EMAIL="25169478+MichaelUray@users.noreply.github.com" \
git commit -m "lazyconn/activity: add HasPeer(connID) accessor + fix mockEndpointManager race

Adds HasPeer(connID peerid.ConnID) bool — a read-only accessor under
the existing Manager.mu. Intended for the upcoming lazy-watchdog to
distinguish 'active and listening' from 'active but stuck after hung
Close' (Case-b of the two-case recovery).

Also fixes a pre-existing data race in mockEndpointManager
(listener_bind_test.go) where concurrent SetEndpoint/RemoveEndpoint/
GetEndpoint accessed the endpoints map without synchronization.
Required so the race-clean HasPeer tests can run.

See docs/superpowers/plans/2026-05-22-phase37i-lazy-watchdog-spec.md
Section 5.2 Stufe 6 + Section 7.3."
```

---

## Task 5 (Commit 5): lazyconn/inactivity — Drop counters + log (Stufe 1)

**Files:**
- Modify: `client/internal/lazyconn/inactivity/manager.go` (atomic counters + notifyChan instrumentation + DropCounters)
- Modify: `client/internal/lazyconn/inactivity/manager_test.go` (Stufe-1 tests)

**Why fifth:** `DropCounters` is consumed by Stufe-2 Watchdog. Could in principle be a standalone upstream PR (per spec Section 8.1 alternative), but bundled here for simplicity.

- [ ] **Step 5.1: Write failing test for DropCounters API**

Append to `client/internal/lazyconn/inactivity/manager_test.go`:

```go
func TestDropCounters_InitialZero(t *testing.T) {
	m := NewManagerWithTwoTimers(newMockWgIfaceInactivity(t), time.Second, time.Second)
	relay, ice := m.DropCounters()
	if relay != 0 || ice != 0 {
		t.Fatalf("expected (0,0) initial, got (%d,%d)", relay, ice)
	}
}
```

- [ ] **Step 5.2: Run, expect FAIL**

Run: `go test ./client/internal/lazyconn/inactivity/ -run TestDropCounters_InitialZero -count=1`
Expected: FAIL with `undefined: m.DropCounters`.

- [ ] **Step 5.3: Add atomic counters + DropCounters to Manager struct**

Edit `client/internal/lazyconn/inactivity/manager.go`. Modify the `Manager` struct (line 56) to add:

```go
type Manager struct {
	iface WgInterface

	iceTimeout   time.Duration
	relayTimeout time.Duration

	interestedPeers map[string]*lazyconn.PeerConfig

	iceInactiveChan   chan map[string]struct{}
	inactivePeersChan chan map[string]struct{}

	inactivityThreshold time.Duration

	// v0.7 Stufe 1: counts silent drops on the two notify channels.
	// Drops happen when the consumer (lazyconn-Manager Start loop) is
	// slow or blocked, and indicate that a state-transition event was
	// lost. The lazy-watchdog reads these via DropCounters().
	notifyDropsRelay atomic.Uint64
	notifyDropsICE   atomic.Uint64
}
```

Add `"sync/atomic"` import.

Add the method (near other exported methods like `InactivePeersChan`):

```go
// DropCounters returns the cumulative count of dropped notifications
// per channel: relayDrops from inactivePeersChan, iceDrops from
// iceInactiveChan. Lock-free atomic load.
func (m *Manager) DropCounters() (relayDrops, iceDrops uint64) {
	return m.notifyDropsRelay.Load(), m.notifyDropsICE.Load()
}
```

- [ ] **Step 5.4: Run, expect PASS**

Run: `go test ./client/internal/lazyconn/inactivity/ -run TestDropCounters_InitialZero -count=1`
Expected: PASS.

- [ ] **Step 5.5: Modify notifyChan to count + log drops**

Replace the current `notifyChan` (line 202 area):

```go
func (m *Manager) notifyChan(ctx context.Context, ch chan map[string]struct{}, peers map[string]struct{}) {
	select {
	case ch <- peers:
	case <-ctx.Done():
		return
	default:
		var n uint64
		switch ch {
		case m.inactivePeersChan:
			n = m.notifyDropsRelay.Add(1)
		case m.iceInactiveChan:
			n = m.notifyDropsICE.Add(1)
		}
		// Throttle: log on 1st, 10th, 100th, then every 100 drops
		if n == 1 || n == 10 || (n >= 100 && n%100 == 0) {
			log.Warnf("inactivity: notify channel full, dropped %d-th event (peers in batch=%d). "+
				"Consumer may be slow or stuck — see lazyconn/manager.go state.",
				n, len(peers))
		}
		return
	}
}
```

- [ ] **Step 5.6: Add full-channel + throttled-log tests**

Append:

```go
func TestNotifyChan_FullChannelIncrementsDropCounter(t *testing.T) {
	m := NewManagerWithTwoTimers(newMockWgIfaceInactivity(t), time.Second, time.Second)
	// Fill the channel
	m.inactivePeersChan <- map[string]struct{}{"pre-fill": {}}
	// Now trigger a drop
	m.notifyChan(context.Background(), m.inactivePeersChan, map[string]struct{}{"dropped": {}})
	relay, _ := m.DropCounters()
	if relay != 1 {
		t.Fatalf("expected relayDrops=1, got %d", relay)
	}
}

func TestDropCounters_RelayAndICESeparate(t *testing.T) {
	m := NewManagerWithTwoTimers(newMockWgIfaceInactivity(t), time.Second, time.Second)
	// Fill both channels
	m.inactivePeersChan <- map[string]struct{}{"r": {}}
	m.iceInactiveChan <- map[string]struct{}{"i": {}}
	m.notifyChan(context.Background(), m.inactivePeersChan, map[string]struct{}{"r2": {}})
	m.notifyChan(context.Background(), m.iceInactiveChan, map[string]struct{}{"i2": {}})
	m.notifyChan(context.Background(), m.iceInactiveChan, map[string]struct{}{"i3": {}})
	relay, ice := m.DropCounters()
	if relay != 1 || ice != 2 {
		t.Fatalf("expected (1,2), got (%d,%d)", relay, ice)
	}
}
```

- [ ] **Step 5.7: Run full package tests with -race**

Run: `go test -race ./client/internal/lazyconn/inactivity/ -count=1 -timeout 60s`
Expected: PASS.

- [ ] **Step 5.8: Commit**

```bash
git add client/internal/lazyconn/inactivity/manager.go client/internal/lazyconn/inactivity/manager_test.go
GIT_AUTHOR_NAME="Michael Uray" GIT_AUTHOR_EMAIL="25169478+MichaelUray@users.noreply.github.com" \
GIT_COMMITTER_NAME="Michael Uray" GIT_COMMITTER_EMAIL="25169478+MichaelUray@users.noreply.github.com" \
git commit -m "lazyconn/inactivity: count + log silent notifyChan drops

Adds two atomic counters (notifyDropsRelay, notifyDropsICE) to
inactivity.Manager that increment whenever notifyChan() falls into the
default arm (channel full → silent drop). Logs are throttled at 1st,
10th, 100th, then every 100 drops to avoid log-storms.

Exposes DropCounters() (relayDrops, iceDrops uint64) for the upcoming
lazy-watchdog to detect stuck consumer goroutines.

See docs/superpowers/plans/2026-05-22-phase37i-lazy-watchdog-spec.md
Section 5.2 Stufe 1."
```

---

## Task 6 (Commit 6): lazyconn/manager — Reconcile-Watchdog two-case recovery (Stufen 2 + 4)

**Files:**
- Modify: `client/internal/lazyconn/manager/manager.go` (~200 LOC: watchdog goroutine + two recovery functions + helpers + Start-wiring)
- Create: `client/internal/lazyconn/manager/watchdog_test.go` (~400 LOC for full test suite)
- Modify: `client/internal/lazyconn/manager/recovery_test.go` (add Stufe-3 recovery tests + R14 race tests)

**Why last:** Consumes APIs from Commits 1 (`TransportSnapshot`), 4 (`HasPeer`), and 5 (`DropCounters`), plus helpers from Commit 3. Most lines, most tests — keeps the reviewer focus on a single architectural addition.

### Sub-tasks: implementation

- [ ] **Step 6.1: Add the watchdog constants + helper functions to manager.go**

Edit `client/internal/lazyconn/manager/manager.go`. Add at the top of the file (near other constants):

```go
const defaultReconcileInterval = 120 * time.Second
```

- [ ] **Step 6.2: Implement `isStuckPeer`**

Add (private helper, near other helpers):

```go
// isStuckPeer is the Case-a heuristic: peer is watcherInactivity but the
// notifyChan dropped events AND both transports are disconnected.
// v0.5 Codex round-4: ONLY relayDrops trigger this — iceDrops semantically
// belong to ConnMgr.runDynamicInactivityLoop (DetachICEForPeer), not full
// sleep.
func isStuckPeer(iceDisc, relayDisc bool, deltaRelay uint64) bool {
	return iceDisc && relayDisc && deltaRelay > 0
}
```

- [ ] **Step 6.3: Implement `peerStillManaged` (re-validate helper, v0.7 R14)**

Add:

```go
// peerStillManaged is the v0.7 post-arm Re-Validate helper. After
// armActivityListener is called outside lock, this re-acquires
// managedPeersMu briefly to verify the peer is still managed with the
// SAME PeerConnID (defending against RemovePeer/ExcludePeer racing).
func (m *Manager) peerStillManaged(pubKey string, expectedConnID peerid.ConnID) bool {
	m.managedPeersMu.Lock()
	defer m.managedPeersMu.Unlock()
	cfg, ok := m.managedPeers[pubKey]
	if !ok {
		return false
	}
	return cfg.PeerConnID == expectedConnID
}
```

- [ ] **Step 6.4: Implement `recoverInactivityStuck` (Case-a)**

Add:

```go
// recoverInactivityStuck handles Case-a: peer in watcherInactivity, but
// notifyChan dropped the inactivity-timed-out event so the state never
// flipped. State-flip + arm listener; NO Close (Conn is already
// disconnected). HA-defer applies. v0.7.1: capture connID+peerLog BEFORE
// unlock to use the real activity.Manager.RemovePeer signature.
func (m *Manager) recoverInactivityStuck(ctx context.Context, pubKey string, stuckBatch map[string]struct{}) {
	m.managedPeersMu.Lock()
	cfg, ok := m.managedPeers[pubKey]
	if !ok {
		m.managedPeersMu.Unlock()
		return
	}
	mp, ok := m.managedPeersByConnID[cfg.PeerConnID]
	if !ok {
		m.managedPeersMu.Unlock()
		return
	}
	if mp.expectedWatcher != watcherInactivity {
		m.managedPeersMu.Unlock()
		return
	}
	if m.shouldDeferIdleForHA(stuckBatch, mp.peerCfg.PublicKey) {
		mp.peerCfg.Log.Infof("watchdog: defer inactivity-stuck recovery (HA peers active, batch=%d)", len(stuckBatch))
		m.managedPeersMu.Unlock()
		return
	}
	connID := cfg.PeerConnID
	peerLog := cfg.Log
	m.transitionToActivityWatcherStateOnly(mp)
	m.managedPeersMu.Unlock()

	m.armActivityListener(mp)

	if !m.peerStillManaged(pubKey, connID) {
		m.activityManager.RemovePeer(peerLog, connID)
		return
	}
	peerLog.Infof("watchdog: recovery complete (inactivity-stuck: watcherInactivity -> watcherActivity, listener armed)")
}
```

- [ ] **Step 6.5: Implement `recoverActivityNoListener` (Case-b)**

Add:

```go
// recoverActivityNoListener handles Case-b: peer in watcherActivity but no
// Activity-Listener registered. This is the post-Close-hang state.
// Recovery: arm listener only. No state mutation needed (already
// watcherActivity). No HA-defer (peer is already "wanted active").
func (m *Manager) recoverActivityNoListener(ctx context.Context, pubKey string) {
	m.managedPeersMu.Lock()
	cfg, ok := m.managedPeers[pubKey]
	if !ok {
		m.managedPeersMu.Unlock()
		return
	}
	mp, ok := m.managedPeersByConnID[cfg.PeerConnID]
	if !ok {
		m.managedPeersMu.Unlock()
		return
	}
	if mp.expectedWatcher != watcherActivity {
		m.managedPeersMu.Unlock()
		return
	}
	connID := cfg.PeerConnID
	peerLog := cfg.Log
	m.managedPeersMu.Unlock()

	if m.activityManager.HasPeer(connID) {
		return
	}
	m.armActivityListener(mp)

	if !m.peerStillManaged(pubKey, connID) {
		m.activityManager.RemovePeer(peerLog, connID)
		return
	}
	peerLog.Infof("watchdog: recovery complete (activity-no-listener: listener re-armed for peer in watcherActivity)")
}
```

- [ ] **Step 6.6: Implement `spawnRecovery` (inflight-dedupe + panic-recovery)**

Add:

```go
// spawnRecovery wraps a recovery function in an inflight-dedupe lock cycle
// + panic-recovery. Caller passes the actual recovery work as fn.
func (m *Manager) spawnRecovery(ctx context.Context, pubKey string,
	recoveringPeers map[string]struct{}, recoveringMu *sync.Mutex,
	fn func(string)) {
	recoveringMu.Lock()
	if _, inflight := recoveringPeers[pubKey]; inflight {
		recoveringMu.Unlock()
		return
	}
	recoveringPeers[pubKey] = struct{}{}
	recoveringMu.Unlock()
	go func() {
		defer func() {
			recoveringMu.Lock()
			delete(recoveringPeers, pubKey)
			recoveringMu.Unlock()
			if r := recover(); r != nil {
				log.Errorf("lazyconn watchdog: recovery panic for %s: %v", pubKey, r)
			}
		}()
		fn(pubKey)
	}()
}
```

- [ ] **Step 6.7: Implement `reconcileTick` (Phase A/B/C/D)**

Add:

```go
func (m *Manager) reconcileTick(ctx context.Context, lastRelayDrops, lastICEDrops *uint64,
	recoveringPeers map[string]struct{}, recoveringMu *sync.Mutex) {

	// PHASE A: atomic counter read (no lock)
	relayDrops, iceDrops := m.inactivityManager.DropCounters()
	deltaRelay := relayDrops - *lastRelayDrops
	deltaICE := iceDrops - *lastICEDrops
	*lastRelayDrops, *lastICEDrops = relayDrops, iceDrops

	// PHASE B: snapshot ALL peers (state + connID + expectedWatcher)
	type peerSnap struct {
		pubKey          string
		connID          peerid.ConnID
		expectedWatcher watcherType
	}
	m.managedPeersMu.Lock()
	snaps := make([]peerSnap, 0, len(m.managedPeersByConnID))
	for connID, mp := range m.managedPeersByConnID {
		snaps = append(snaps, peerSnap{
			pubKey:          mp.peerCfg.PublicKey,
			connID:          connID,
			expectedWatcher: mp.expectedWatcher,
		})
	}
	m.managedPeersMu.Unlock()

	// PHASE C: per-peer transport-state + listener-state classification
	stuckInactivityBatch := make(map[string]struct{})
	stuckActivityNoListener := make(map[string]struct{})
	for _, s := range snaps {
		conn, ok := m.peerStore.PeerConn(s.pubKey)
		if !ok {
			continue
		}
		iceDisc, relayDisc := conn.TransportSnapshot()
		if !iceDisc || !relayDisc {
			continue
		}
		switch s.expectedWatcher {
		case watcherInactivity:
			if isStuckPeer(iceDisc, relayDisc, deltaRelay) {
				stuckInactivityBatch[s.pubKey] = struct{}{}
			}
		case watcherActivity:
			if !m.activityManager.HasPeer(s.connID) {
				stuckActivityNoListener[s.pubKey] = struct{}{}
			}
		}
	}
	total := len(stuckInactivityBatch) + len(stuckActivityNoListener)
	if total == 0 {
		return
	}

	log.Warnf("lazyconn watchdog: %d stuck peers (inactivity-stuck=%d relayDrops=%d, activity-no-listener=%d) — ICE-drops=%d telemetry only",
		total, len(stuckInactivityBatch), deltaRelay, len(stuckActivityNoListener), deltaICE)

	// PHASE D: spawn bounded async recovery per peer
	for pubKey := range stuckInactivityBatch {
		batch := stuckInactivityBatch
		m.spawnRecovery(ctx, pubKey, recoveringPeers, recoveringMu, func(pk string) {
			m.recoverInactivityStuck(ctx, pk, batch)
		})
	}
	for pubKey := range stuckActivityNoListener {
		m.spawnRecovery(ctx, pubKey, recoveringPeers, recoveringMu, func(pk string) {
			m.recoverActivityNoListener(ctx, pk)
		})
	}
}
```

- [ ] **Step 6.8: Implement `runReconcileWatchdog` (the long-lived loop)**

Add:

```go
func (m *Manager) runReconcileWatchdog(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("lazyconn watchdog: panic, restart loop: %v", r)
			go m.runReconcileWatchdog(ctx)
		}
	}()

	ticker := time.NewTicker(defaultReconcileInterval)
	defer ticker.Stop()

	var lastRelayDrops, lastICEDrops uint64
	var recoveringMu sync.Mutex
	recoveringPeers := make(map[string]struct{})

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcileTick(ctx, &lastRelayDrops, &lastICEDrops, recoveringPeers, &recoveringMu)
		}
	}
}
```

- [ ] **Step 6.9: Wire watchdog into `Start()` (Stufe 4)**

Edit `Start()` at line 173 to add ONE line right before the `for` loop:

```go
func (m *Manager) Start(ctx context.Context) {
	defer m.close()

	if m.inactivityManager != nil {
		go m.inactivityManager.Start(ctx)
	}

	go m.runReconcileWatchdog(ctx)  // v0.7 Stufe 4 wiring

	for {
		select {
		case <-ctx.Done():
			return
		// ... existing cases
```

- [ ] **Step 6.10: Verify build + existing tests still pass**

Run: `go build ./client/...`
Run: `go test -race ./client/internal/lazyconn/manager/ -count=1 -timeout 180s`
Expected: PASS. The watchdog is wired but with no test triggers yet — should be inert in existing tests.

### Sub-tasks: unit tests

- [ ] **Step 6.11: Write watchdog Phase-B/C/D tests**

Create `client/internal/lazyconn/manager/watchdog_test.go`. Per Spec Section 6.1, write the following tests one at a time, each TDD-style (write → fail → no further code change → pass since impl is done; the goal is COVERAGE assertion):

- `TestReconcileWatchdog_DetectsInactivityStuck` (Case-a)
- `TestReconcileWatchdog_DetectsActivityNoListener` (Case-b)
- `TestReconcileWatchdog_ICEDropsOnlyDoesNotTrigger`
- `TestReconcileWatchdog_ActivityWithListener_NoOp` (Case-c)
- `TestReconcileWatchdog_DropCounterAloneIsNotTrigger`
- `TestReconcileWatchdog_DisconnectAloneIsNotTrigger`
- `TestReconcileWatchdog_InflightDedupePreventsDoubleSpawn`
- `TestReconcileWatchdog_PanicSelfRestart`
- `TestReconcileWatchdog_StartStop`

Each test needs a Manager constructed with a controllable `inactivity.Manager` (so you can inc DropCounters at will), a fake peerStore returning `*peer.Conn` with controllable TransportSnapshot, and a fake activity.Manager with controllable HasPeer. **Spend time building the harness FIRST** (helper `newTestWatchdogManager(t)`), then knock out the tests rapidly.

Skeleton harness:

```go
type testWatchdogHarness struct {
	mgr         *Manager
	activityMgr *activity.Manager
	inactivity  *inactivity.Manager
	peerStore   *peerstore.Store
}

func newTestWatchdogHarness(t *testing.T) *testWatchdogHarness {
	// 1. Build a mock WGInterface that satisfies both lazyconn.WGIface
	//    and activity.WgInterface.
	// 2. Construct activity.Manager via NewManager(mockWgIface).
	// 3. Construct inactivity.Manager via NewManagerWithTwoTimers(...).
	// 4. Construct peerstore.Store via peerstore.NewStore(t.Context()).
	// 5. Construct *Manager via NewManager(config, ctx, peerStore, mockWgIface).
	// 6. Replace mgr.activityManager / mgr.inactivityManager with our
	//    instances (may need a test-only setter or initialization order
	//    care).
}
```

Each test then drives state changes and calls `mgr.reconcileTick(...)` directly with controlled `lastRelayDrops` and a fresh `recoveringPeers` map; asserts the resulting state mutations (HasPeer status, expectedWatcher field).

- [ ] **Step 6.12: Write recovery-function tests in recovery_test.go**

Append to `client/internal/lazyconn/manager/recovery_test.go`:

- `TestRecoverInactivityStuck_HappyPath` — also asserts `peerStore.PeerConnIdle` was NOT called (via counter-mock)
- `TestRecoverInactivityStuck_AlreadyActivity_NoOp`
- `TestRecoverInactivityStuck_RespectsHA_FullBatch`
- `TestRecoverInactivityStuck_NoCloseDeadlock` — explicit assertion via PeerConnIdle counter
- `TestRecoverActivityNoListener_HappyPath`
- `TestRecoverActivityNoListener_ListenerArmedConcurrently_NoOp`
- `TestRecoverActivityNoListener_SkipsHADefer`
- `TestRecoverInactivityStuck_RemoveRaceAfterUnlock` (v0.7 R14)
- `TestRecoverActivityNoListener_RemoveRaceAfterUnlock` (v0.7 R14)
- `TestRecoverActivityNoListener_ConnIDChangeAfterUnlock` (v0.7 R14)

For R14 race tests: use a `sync.WaitGroup` + a test-only hook between the function's `m.managedPeersMu.Unlock()` and `m.armActivityListener(mp)` that lets the test thread call `mgr.RemovePeer(pubKey)`. Then assert that the cleanup (`activityManager.HasPeer == false`) ran.

Test-only hook example: add a package-level var `var testListenerArmHook func()` and call it at the right point if non-nil (`if testListenerArmHook != nil { testListenerArmHook() }`). Restore via `t.Cleanup`.

### Sub-tasks: integration tests

- [ ] **Step 6.13: Write integration tests**

Append to `watchdog_test.go`:

- `TestIntegration_InactivityStuck_WatchdogHeals` (Case a, full Manager wired + DropCounters drip)
- `TestIntegration_ActivityNoListener_WatchdogHeals` (Case b — start a peer, simulate hung-Close after state-flip, watchdog re-arms)
- `TestIntegration_PanicInConsumer_WatchdogHeals` (Stufe 0 + Stufe 2 cooperation)

These should use a controllable tick interval (override `defaultReconcileInterval` via a package-level var, or pass it as a Manager-config field) for fast test execution (e.g. 50ms instead of 120s).

- [ ] **Step 6.14: Make the tick interval configurable for tests**

Refactor `defaultReconcileInterval` from a `const` to a `var` so tests can monkey-patch it, OR (cleaner) make `runReconcileWatchdog` take the interval as a parameter:

```go
func (m *Manager) runReconcileWatchdog(ctx context.Context, interval time.Duration) {
	// ...
	ticker := time.NewTicker(interval)
	// ...
}
```

And `Start()` calls `go m.runReconcileWatchdog(ctx, defaultReconcileInterval)`.

Integration tests then call `mgr.runReconcileWatchdog(ctx, 50*time.Millisecond)` directly.

- [ ] **Step 6.15: Run full Stufe-2 test set with -race**

Run: `go test -race ./client/internal/lazyconn/manager/ -count=1 -timeout 300s`
Expected: PASS. If any race is reported, FIX it before commit.

### Sub-tasks: final integration

- [ ] **Step 6.16: Run full client/ test suite with -race**

Run: `go test -race ./client/internal/lazyconn/... ./client/internal/peer/ -count=1 -timeout 300s`
Expected: PASS across all touched packages.

- [ ] **Step 6.17: Commit**

```bash
git add client/internal/lazyconn/manager/manager.go \
        client/internal/lazyconn/manager/watchdog_test.go \
        client/internal/lazyconn/manager/recovery_test.go
GIT_AUTHOR_NAME="Michael Uray" GIT_AUTHOR_EMAIL="25169478+MichaelUray@users.noreply.github.com" \
GIT_COMMITTER_NAME="Michael Uray" GIT_COMMITTER_EMAIL="25169478+MichaelUray@users.noreply.github.com" \
git commit -m "lazyconn/manager: reconcile watchdog with two-case recovery

Adds a 120s-tick reconcile watchdog that heals two distinct stuck
states observed in Phase 3.7i lazy connection management:

Case-a: peer in watcherInactivity, both transports disconnected, and
relayDrops > 0 on the inactivity-manager notifyChan. Indicates the
inactivity-timed-out event was silently dropped before reaching
onPeerInactivityTimedOut. Recovery: state-flip to watcherActivity +
arm activity listener (no Close, conn is already disconnected).

Case-b: peer in watcherActivity but no activity listener registered.
This is the post-Close-hang state — onPeerInactivityTimedOut flipped
state, then PeerConnIdle hung structurally, so armActivityListener
never ran. Recovery: arm listener only.

Architecture: Phase A (atomic counter read, no lock) → Phase B (short
managedPeersMu snapshot of all peers) → Phase C (per-peer classification
lock-free via TransportSnapshot + HasPeer) → Phase D (bounded async
recovery goroutine per peer with inflight-dedupe + panic-recovery).

The watchdog NEVER calls PeerConnIdle/Conn.Close — avoiding the
conn.mu/managedPeersMu deadlock between async-close and a
concurrent onPeerActivity → PeerConnOpen path (the v0.4 BLOCKER).

Post-arm Re-Validate via peerStillManaged(pubKey, expectedConnID)
cleans up orphan listeners if RemovePeer/ExcludePeer raced (v0.7 R14).

Wired into Manager.Start() as a single goroutine line. Tick interval
is parameterized so tests can run fast.

See docs/superpowers/plans/2026-05-22-phase37i-lazy-watchdog-spec.md
Section 5.2 Stufen 2+4."
```

---

## Task 7: Final verification + push

- [ ] **Step 7.1: Run full client/ test suite from scratch**

```bash
go clean -testcache
go test -race -timeout 600s ./client/...
```
Expected: PASS. If any test fails, investigate before pushing.

- [ ] **Step 7.2: Sanity check the 6 commits**

```bash
git log --oneline phase3.7i-runtime-bugfixes-v0.5..pr/g-phase3.7i-lazy-watchdog
```
Expected: exactly 6 commits in the order listed in Section 8.1 of the spec.

- [ ] **Step 7.3: Verify Author/Committer on all commits**

```bash
git log --format='%H %an <%ae> | %cn <%ce> | %s' phase3.7i-runtime-bugfixes-v0.5..pr/g-phase3.7i-lazy-watchdog
```
Expected: every commit has Author + Committer `Michael Uray <25169478+MichaelUray@users.noreply.github.com>`. No `Co-Authored-By: Claude/Codex` trailers.

- [ ] **Step 7.4: Push to fork**

```bash
git push -u origin pr/g-phase3.7i-lazy-watchdog
```
Note: NOT pushing to netbirdio/netbird — only to MichaelUray/netbird fork per durable user policy.

- [ ] **Step 7.5: Hardware-Soak deploy (per Spec Section 6.3, before any upstream PR)**

Build + deploy `pr/g-phase3.7i-lazy-watchdog` on `phase3.7i-runtime-bugfixes-v0.5` to:
- S21 (Android, primary failure site)
- dk20 (Linux secondary)
- w11-test1 (Win11 control)

Monitor 72h for:
- `notifyChan ... full, dropped event` warn-logs (Stufe 1 telemetry)
- `lazyconn watchdog: N stuck peers (...) — spawning recovery` warn-logs
- `watchdog: recovery complete` info-logs
- VNC-Connect to Elmira works without Force-Stop after 24h/48h/72h
- No spurious recoveries on healthy peers
- CPU overhead < 0.1%

If any issue: document, debug, fix, re-soak. Do NOT submit upstream-PR before clean soak.

- [ ] **Step 7.6: After clean Soak — Codex round-N final review of the actual code (not the spec)**

Submit the 6-commit diff to Codex for a fresh code-review pass against the real PR diff (not the pseudocode). This is the "post-soak, pre-upstream" gate.

- [ ] **Step 7.7: User approval → upstream PR**

ONLY after explicit user OK: prepare upstream PR against netbirdio/netbird per durable user policy. Use `maintainer_can_modify=true`. PR body should reference this plan + the spec.

---

## Done criteria

- [ ] All 6 commits exist on `pr/g-phase3.7i-lazy-watchdog` with correct Author/Committer
- [ ] `go test -race -timeout 600s ./client/...` passes
- [ ] 72h soak on S21 + dk20 + w11-test1 with no regressions
- [ ] At least one Case-a OR Case-b recovery log line observed in the wild OR explicit user confirmation that the watchdog adds no overhead (false-negatives are acceptable; false-positives are not)
- [ ] VNC-to-Elmira stays functional 72h without daemon-restart
