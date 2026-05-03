# Connection-Lifecycle Hardening — work-in-progress branch notes

This branch (`feat/connection-lifecycle-hardening`) is the home for the
larger Codex-review follow-ups deferred from the
`feat/peer-status-visibility` branch. Items 1 + 2 (reconnect-guard
inactivity-skip; UI ICE-backoff surface) shipped on the prior branch
already.

Reference design doc:
`/opt/infrastructure/docs/superpowers/specs/2026-05-03-netbird-connection-lifecycle-hardening-design.md`

## Item 3 — conn_mgr Mode-Transition test-suite with fakes

Goal: behavioural test coverage for Connection Mode transitions under
all four combinations of (configured mode, effective mode) plus the
edge cases that have bitten us so far.

### Required infrastructure (write FIRST)

A `fakeICE`/`fakeRelay` pair so conn_mgr.go's transitions can be driven
without spinning up real WireGuard, ICE-agent, or signal-client. Today
nothing under `client/internal/` has fakes for these — the existing
unit tests use real components and skip whenever no kernel WG is
present.

  - `peer.fakeWGProxy` — implements wgproxy.Proxy. Records Pause/Work
    calls, exposes a `paused()` accessor.
  - `peer.fakeEndpointUpdater` — records ConfigureWGEndpoint and
    RemoveEndpointAddress calls + their order vs other fake calls.
  - `peer.fakeICEAgent` — drives onICEConnectionIsReady /
    onICEStateDisconnected callbacks on demand.
  - `peer.fakeStatusRecorder` already partially exists; ensure
    Update*State events are observable for assertion (channel-based).

### Test cases

1. eager-static → p2p-dynamic switch with active peer. Expect:
   - ICE retain (no detach immediately)
   - Relay close timer cancelled when ICE re-attaches
   - PeerStateChange pushed once with new effective mode

2. p2p-dynamic → eager-static switch with idle peer (Relay only,
   ICE detached). Expect:
   - ICE re-attach offer fires immediately
   - inactivity-detach timer cancelled
   - No relay close even after >iceTimeout

3. p2p-dynamic offline → live transition. Expect:
   - scheduleRemoteOfflineClose timer present BEFORE; cancelled AFTER
     liveOnline=true arrives within grace.
   - No conn.Close fired.
   - Status snapshot reports Connected throughout.

4. p2p-dynamic live → offline → debounce expiry. Expect:
   - scheduleRemoteOfflineClose timer cancelled if live flips back.
   - After 5s grace expires AND still offline AND re-validated by
     fakeStatusRecorder, conn.Close called once.
   - Subsequent live=true bursts do NOT race with the
     post-Close cleanup (no UpdatePeerState while a Close is in
     flight).

5. ICE-handover order under load. Driven by fakeICEAgent:
   - simulate ICE up while Relay still active, expect strict order
     wgProxy.Work → ConfigureWGEndpoint → RedirectAs → Pause
   - simulate ICE flap (up, down within 1s, up again) — expect no
     RemoveEndpointAddress in the disconnect path.

6. Mass re-key event (RemovePeer + AddPeer same pubkey within 100ms).
   Expect:
   - Old conn's offline-debounce timer cancelled in removePeer (already
     covered by engine_test, but extend with verification that
     PeerStateChangeEvent map drops the pubkey before re-add).
   - New conn rebuilds with fresh State, no stale ICE-backoff
     suspension carried over.

### Expected churn

- `client/internal/peer/conn_fakes_test.go` — new (~300 lines)
- `client/internal/peer/conn_mode_transitions_test.go` — new (~600 lines)
- `client/internal/conn_mgr_mode_transitions_test.go` — new (~400 lines)
- Possibly extend `client/internal/peer/conn.go` to expose 1–2
  test-only hooks (`conn.testForceICEDetach()`) — keep the export
  surface minimal, prefer construction-time injection.

### Risk

The conn.go interfaces are tightly coupled today; introducing fakes
means either:
  a) extracting an interface for `WGProxy`, `EndpointUpdater`,
     `ICEAgent` (clean, bigger refactor, but improves the prod-code
     boundary too), or
  b) a thinner test-package shim that wraps the concrete types
     (faster, but every future signature change has to ripple).

Decision postponed to first task in this branch — pick (a) if the
interface surface stays under ~10 methods total.

## Other in-flight Codex hints (not yet planned)

- Engine startup order: TriggerInitialSnapshot is currently called
  from engine.Start AFTER the first NetworkMap apply via a
  goroutine; verify with a fresh engine that no path can call it
  before networkmap-1 is fully applied.
- conn_state_pusher.flushDelta retain-on-fail is unbounded — if mgmt
  is down for an hour the dirty-set keeps every change. Consider
  bounded coalescing (LRU on lastPushed reads) once we see this in
  the wild.
