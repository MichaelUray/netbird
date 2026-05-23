# Draft PR-Body for Phase-3.7i Lazy-Watchdog (upstream)

**Status:** Draft only. Do NOT create the upstream PR without explicit user OK
(per durable user policy: `feedback_pr_requires_explicit_confirmation`).

This file pre-formats the body so the moment the user approves, the PR can be
opened without further composition.

---

## Title

`client/lazyconn: reconcile watchdog for stuck-state recovery in Phase-3.7i lazy connections`

## Body

### Summary

Phase-3.7i's lazy connection state machine has two distinct stuck states
observable in production after long uptime on memory-pressured devices
(Galaxy S21 was the primary failure site, but the bug pattern is OS-neutral):

- **Case-a** — `watcherInactivity + relayDrops > 0 + both transports disconnected`:
  the inactivity-manager's `inactivePeersChan` (buffer 1) silently dropped
  the inactivity-timeout event before the lazy-manager consumer could process
  it. The peer is permanently stuck in `watcherInactivity` state with no
  activity listener and no scheduled retry. Force-stopping the daemon is
  the only known workaround.

- **Case-b** — `watcherActivity + !activityManager.HasPeer + both transports disconnected`:
  `onPeerInactivityTimedOut` flipped state to `watcherActivity` under
  `managedPeersMu`, released the lock, then hung in `PeerConnIdle` →
  `Conn.Close()` → `wgWatcherWg.Wait()`. The activity listener was never
  armed. The peer is permanently stuck in `watcherActivity` state without
  any wake mechanism.

This PR adds a 120s-tick reconcile watchdog inside the lazy connection
Manager that classifies all managed peers per tick and dispatches recovery
goroutines for the two stuck states. Recovery is **state-flip + listener-arm
only** — the watchdog NEVER calls `PeerConnIdle`/`Conn.Close()`. This avoids
a `conn.mu`/`managedPeersMu` deadlock that an earlier iteration of the
design exhibited: if the watchdog spawned `Conn.Close()` async and the
newly-armed listener fired concurrently, `onPeerActivity` would acquire
`managedPeersMu` and then block on `conn.mu` (still held by the hanging
`Close`), freezing the whole lazy manager.

### Architecture

Phase A → atomic counter read (no lock).
Phase B → short `managedPeersMu` snapshot of all peers (`pubKey`, `connID`, `expectedWatcher`).
Phase C → per-peer classification lock-free via `TransportSnapshot()` + `HasPeer(connID)`.
Phase D → bounded async recovery goroutine per peer, with inflight-dedupe + `defer recover()`.

Two distinct recovery paths:

- `recoverInactivityStuck` (Case-a): re-validate under lock → HA-defer check
  with full stuck-batch → `transitionToActivityWatcherStateOnly` →
  `armActivityListener` → post-arm `peerStillManaged` re-validate +
  `activityManager.RemovePeer(peerLog, connID)` cleanup on race.

- `recoverActivityNoListener` (Case-b): re-validate state + connID under lock
  → re-check `HasPeer` without lock (idempotent) → `armActivityListener` →
  same post-arm re-validate + cleanup.

`onPeerInactivityTimedOut` was also refactored to drop `managedPeersMu` before
the blocking I/O, restoring v0.3 close-then-listen ordering with the same
R14 post-arm re-validate.

A kernel-mode bug-fix is folded into the panic-recovery commit: `Manager.Start`
used to nil-deref on `m.inactivityManager.InactivePeersChan()` when called in
kernel-WG mode (`inactivityManager` is nil there per `manager.go` and
`ConnMgr.Start` invokes the lazy-manager unconditionally). The new two-path
`Start` skips the inactivity arm entirely on kernel-mode.

### Commit sequence

Six commits, each individually reviewable + green-tested:

1. `peer/conn: add TransportSnapshot accessor for external watchdogs`
2. `lazyconn/manager: defer recover() in consumer loop + Start() two-path split`
3. `lazyconn/manager: refactor inactivity-timeout I/O outside lock + R14 race protection`
4. `lazyconn/activity: add HasPeer(connID) accessor + fix mockEndpointManager race`
5. `lazyconn/inactivity: count + log silent notifyChan drops`
6. `lazyconn/manager: reconcile watchdog with two-case recovery`

(Plus one test-rename touch-up after a post-implementation code review
identified that `TestReconcileWatchdog_PanicSelfRestart` was misnamed —
it actually verifies `spawnRecovery`'s panic containment, not
`runReconcileWatchdog`'s self-restart. Now `_RecoveryPanicContained`.)

### Testability trade-offs (please review)

Three `*ForTest` symbols are exported in production packages to enable
cross-package unit tests. Go does not support cross-package test-only
exports, and the alternative — making `peerstore.Store` mockable via an
interface — would be significantly more invasive. The chosen approach
stays under `client/internal/` (not public-API), but the exports are
intentional and flagged here so maintainers can choose to keep, rename,
or replace them with an interface refactor:

- `peer.NewConnForTransportTest(log, ice, relay) *Conn` — minimal `*Conn`
  constructor that initializes only `Log + statusICE + statusRelay`. Used by
  the lazy-manager watchdog tests to inject controllable transport states.

- `inactivity.Manager.RecordRelayDropForTest()` /
  `inactivity.Manager.RecordICEDropForTest()` — atomic counter bumpers used
  by watchdog tests to simulate dropped-notify events without filling the
  channels.

- `var testListenerArmHook atomic.Value` (in `lazyconn/manager`) — package-
  level test-only injection point between `armActivityListener` and the
  post-arm `peerStillManaged` re-validate. Tests use it to simulate
  concurrent `RemovePeer` and verify the cleanup path.

Each is documented as test-only with a clear doc-comment. If maintainers
prefer the interface-refactor route instead, the watchdog implementation
can pivot there without architectural changes.

### Test coverage

- 5 unit + race tests for `TransportSnapshot`
- 4 unit + race tests for `activity.HasPeer`
- 4 unit + race tests for `inactivity.DropCounters` + drop-log throttling
- 11 unit tests for `recoverInactivityStuck` / `recoverActivityNoListener` /
  `peerStillManaged` (includes R14 race variants via test hook)
- 9 reconcile-watchdog tests + 3 integration tests
- All green under `go test -race ./client/internal/lazyconn/... ./client/internal/peer/`

### Hardware soak

Built as `0.0.0-dev-40e5fb8b5`, deployed on Galaxy S21 (kernel-bind via
WireGuard-userspace fallback) for a 30-minute interactive verification
session before the PR was opened:

- 27/32 peers connected, P2P reached 60% ratio after activity (16/27 direct).
- P2P path to upstream Elmira node verified: srflx-srflx pair via
  41.66.90.21:29893 ↔ 72.137.199.238:34887, ~136ms RTT.
- 13 simultaneous inactivity-timeouts at the 10-minute mark processed
  cleanly via the refactored `onPeerInactivityTimedOut`. Zero stuck peers,
  zero notifyChan drops, zero daemon panics.
- Connectivity remained stable (5/5 ping, 135–149ms) after the stress event.

Longer soak (24h+) was running at the time of PR submission to capture an
actual watchdog firing event in the wild.

### Related design docs

`docs/superpowers/plans/2026-05-22-phase37i-lazy-watchdog-spec.md`
(v0.7.2) captures the full architectural rationale: two-case classification,
why the watchdog deliberately avoids `Conn.Close()`, R14 race protection
(`peerStillManaged` post-arm re-validate), HA-defer batch semantics, and
the kernel-mode `Start()` two-path split.

The plan file
`docs/superpowers/plans/2026-05-24-phase37i-lazy-watchdog-impl.md` (v4.2)
captures the 6-commit implementation sequence with API references and
concrete TDD test code, including the bootstrap of the previously
nonexistent `lazyconn/manager` test harness.
