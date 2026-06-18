# V18.15 Routing Peer Bootstrap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ensure routed-subnet traffic can wake brand-new lazy routing peers before their first successful P2P connection, without weakening V14/V15 signal gating.

**Architecture:** Keep the fix in the lazy-route bootstrap path. The management-sync route map feeds lazy-manager peer-to-route-prefix state, lazy peer configs include `[peer /32] + routed prefixes`, route-manager receives one synthetic idle state after a newly added lazy peer is registered, and activity listeners preserve WG peer entries on real activity.

**Tech Stack:** Go, NetBird client internals, existing route-manager/lazyconn tests, Android userspace WireGuard constraints.

---

### Task 1: Refcounter Idempotence For Already-Pinned AllowedIPs

**Files:**
- Modify: `client/internal/routemanager/static/route.go`
- Modify: `client/internal/routemanager/dynamic/route.go`
- Modify: `client/internal/routemanager/dnsinterceptor/handler.go`
- Test: `client/internal/routemanager/static/route_test.go`

- [ ] **Step 1: Write failing static-route test**

Add a test that calls `AddAllowedIPs("peerA")` twice for the same prefix and asserts the underlying add callback is invoked once.

- [ ] **Step 2: Run test and verify RED**

Run: `go test ./client/internal/routemanager/static -run TestRoute_AddAllowedIPs_IdempotentOnExistingPeer -count=1`

Expected: FAIL because the refcount increments twice or the test file/function does not exist.

- [ ] **Step 3: Implement idempotent AddAllowedIPs guards**

For each route handler, check the refcounter before incrementing. If a prefix is already stored with `Out == peerKey`, return nil for that prefix instead of incrementing.

- [ ] **Step 4: Run focused tests**

Run: `go test ./client/internal/routemanager/static ./client/internal/routemanager/dynamic ./client/internal/routemanager/dnsinterceptor -count=1`

Expected: PASS.

### Task 2: Lazy Route Prefix Map And Stable PeerConfig AllowedIPs

**Files:**
- Modify: `client/internal/lazyconn/manager/manager.go`
- Test: `client/internal/lazyconn/manager/route_prefix_test.go`

- [ ] **Step 1: Write failing tests**

Add tests for:
- `UpdateRouteHAMap` stores routed prefixes from management `route.HAMap` for single-router and HA routes.
- `AddPeer` enriches `PeerConfig.AllowedIPs` with `[NetBird /32] + routed prefixes`.
- Prefixes are deduplicated and sorted deterministically.

- [ ] **Step 2: Run tests and verify RED**

Run: `go test ./client/internal/lazyconn/manager -run 'TestManager_RoutePrefixes|TestManager_AddPeer_RoutingPeerAllowedIPs' -count=1`

Expected: FAIL because the lazy manager currently stores HA groups only and does not enrich allowed IPs.

- [ ] **Step 3: Implement route-prefix tracking**

Add `peerToRoutePrefixes map[string][]netip.Prefix` to `Manager`, populate it from `UpdateRouteHAMap` using management-pushed route prefixes, and enrich stored lazy peer configs with a sorted set union.

- [ ] **Step 4: Run focused tests**

Run: `go test ./client/internal/lazyconn/manager -run 'TestManager_RoutePrefixes|TestManager_AddPeer_RoutingPeerAllowedIPs' -count=1`

Expected: PASS.

### Task 3: Idle Router-State Dispatch For New Lazy Peers

**Files:**
- Modify: `client/internal/peer/status.go`
- Modify: `client/internal/conn_mgr.go`
- Test: `client/internal/peer/status_test.go`
- Test: `client/internal/conn_mgr_test.go`

- [ ] **Step 1: Write failing status idempotence test**

Add a test proving an initial synthetic `StatusIdle` update after `AddPeer` dispatches router subscribers exactly once, while a repeated identical idle update does not dispatch again.

- [ ] **Step 2: Run test and verify RED**

Run: `go test ./client/internal/peer -run TestStatus_UpdatePeerState_InitialIdleDispatchesOnce -count=1`

Expected: FAIL because repeated idle updates currently dispatch.

- [ ] **Step 3: Implement idempotent idle notification**

Treat a peer with `ConnStatus == StatusIdle` and zero `ConnStatusUpdate` as not-yet-dispatched. Dispatch idle only when entering Idle or when the timestamp is zero, then store the timestamp.

- [ ] **Step 4: Wire lazy AddPeerConn synthetic idle update**

After `lazyConnMgr.AddPeer(lazyPeerCfg)` succeeds for a newly managed lazy peer, call `conn.StatusRecorder` equivalent via a small `peer.Conn` method or a `ConnMgr` helper so `UpdatePeerState(State{PubKey, ConnStatus: StatusIdle, ConnStatusUpdate: time.Now()})` flows through `Status.UpdatePeerState`.

- [ ] **Step 5: Run focused tests**

Run: `go test ./client/internal/peer ./client/internal -run 'TestStatus_UpdatePeerState_InitialIdleDispatchesOnce|TestConnMgr' -count=1`

Expected: PASS.

### Task 4: Bind Activity Preserves WG Peer Entry

**Files:**
- Modify: `client/internal/lazyconn/activity/listener_bind.go`
- Test: `client/internal/lazyconn/activity/listener_bind_test.go`

- [ ] **Step 1: Write failing activity-preservation test**

Add `TestBindListener_ActivityFire_PreservesWgPeerEntry`: on real activity, `RemovePeer` must not be called, and the fake bind endpoint behavior must avoid deleting routed-subnet AllowedIPs from WG.

- [ ] **Step 2: Run test and verify RED**

Run: `go test ./client/internal/lazyconn/activity -run TestBindListener_ActivityFire_PreservesWgPeerEntry -count=1`

Expected: FAIL because `ReadPackets` currently calls `RemovePeer` after activity.

- [ ] **Step 3: Implement activity cleanup split**

On `readActivity`, do not call `wgIface.RemovePeer`. Preserve the WG peer entry and leave the fake-bind side in a drop-and-wait state until `Open`/`AttachICEFrom(LazyActivity)` overwrites the endpoint. On close/cancel, keep the existing destructive cleanup.

- [ ] **Step 4: Run focused tests**

Run: `go test ./client/internal/lazyconn/activity -run 'TestBindListener|TestManager_BindMode' -count=1`

Expected: PASS after updating tests whose old expectation required endpoint removal after activity.

### Task 5: Route Manager Brand-New Routing Peer Integration

**Files:**
- Test: `client/internal/routemanager/client/client_test.go`
- Test: `client/internal/lazyconn/manager/route_prefix_test.go`

- [ ] **Step 1: Write failing route-manager bootstrap test**

Add `TestRouteManager_BrandNewRoutingPeer_AddAllowedIPsBeforeFirstConnect`: with a route update first, a subscribed peer initially missing/idle, and no `StatusConnected`, the synthetic Idle dispatch causes `AddAllowedIPs` before first connect.

- [ ] **Step 2: Run test and verify RED or confirm covered by Tasks 2-3**

Run: `go test ./client/internal/routemanager/client ./client/internal/lazyconn/manager -run 'BrandNewRoutingPeer|RoutingPeerAllowedIPs' -count=1`

Expected: FAIL before Tasks 2-3 implementation; PASS after integration.

- [ ] **Step 3: Adjust only if integration gap remains**

If the test exposes an ordering gap, fix the smallest code path that keeps V14/V15 untouched.

### Task 6: Verification, Memory, And Build Preparation

**Files:**
- Modify: `/home/ai-agent/.claude/projects/-opt-infrastructure/memory/reference_netbird_v18_series_complete.md`

- [ ] **Step 1: Run focused package tests**

Run: `go test ./client/internal/routemanager/... ./client/internal/lazyconn/... ./client/internal/peer ./client/internal -count=1`

- [ ] **Step 2: Run race tests for touched lazy packages**

Run: `go test -race ./client/internal/lazyconn/... -count=1 -timeout 180s`

- [ ] **Step 3: Build target smoke commands**

Run linux/windows/android build commands after syncing Android source:

```bash
cd /home/ai-agent/projects/netbird-android/netbird
git pull --ff-only fork fix/phase1-srflx-diag-tracking
```

Use tag `0.68.0-dev-v18.15-routing-peer-bootstrap-<short-hash>`.

- [ ] **Step 4: Update memory**

Append V18.15 to the iteration table plus Root Cause, Verified Behavior, and Lessons Learned sections in `reference_netbird_v18_series_complete.md`.

