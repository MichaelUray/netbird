# Phase-3.7j — GO_IDLE Mode-Aware Receiver + Guard Retry Hardening

**Status:** DRAFT v0.1 — for Codex review before implementation
**Date:** 2026-05-24
**Author:** Michael Uray (MichaelUray)
**Related:**
- Live-Test-Report: [`docs/test-reports/2026-05-24-codex-review-response-ice-detach-bug.md`](../../../docs/test-reports/2026-05-24-codex-review-response-ice-detach-bug.md) (Codex root-cause analysis)
- Memory: `reference_netbird_p2p_dynamic_go_idle_drop.md`
- Vorgänger-Spec: [`2026-05-24-phase37i-p2p-lazy-orphan-disconnect-spec.md`](2026-05-24-phase37i-p2p-lazy-orphan-disconnect-spec.md) (v0.3.1, implementiert in `pr/h-phase37i-orphan-disconnect@220914567`)

**Scope:**
- `client/internal/conn_mgr.go` (GO_IDLE-Dispatch nach RemoteEffectiveConnectionMode)
- `client/internal/peer/guard/guard.go` + `ice_retry_state.go` (Retry-Accounting)
- `client/internal/peer/conn.go` (Intentional-Detach-Marker)

**NOT in scope:**
- v0.51.2-Client-Side-Änderungen (Legacy-Clients bleiben unverändert)
- Mgmt-Server-Logik (`conversion.go` ist bereits korrekt, `EffectiveConnectionMode` wird gepusht)

---

## 1. Problem statement

### 1.1 Beobachtetes Verhalten

**dk20 ↔ Elmira (v0.51.2 in Bell-Canada)**: P2P-Verbindungen kommen erfolgreich zustande (z. B. via `host/srflx` Pair), bleiben 15-22 min stabil mit erfolgreichen WG-Handshakes, und brechen dann mit dem Log-Eintrag:

```
detaching ICE worker: remote peer signaled GO_IDLE (p2p-dynamic)
ICE disconnected, set Relay to active connection
ICE retries exhausted (3/3), switching to hourly retry
```

Danach steckt die Verbindung in Backoff-Eskalation (`failure #6` = 8 min wait). Während des Backoffs kommen alle 30-60 s `Body_OFFER`-Pakete von Elmira an, werden aber von dk20 ignoriert:

```
remote OFFER (session unknown) without local ICE listener (relay-forced mode or peer in ICE backoff)
```

Phase-3.7i ↔ Phase-3.7i Cross-Site (CTB-Lebring ↔ PVE5) zeigt das gleiche Pattern.

### 1.2 User-Erwartung

P2P-Verbindungen sollen **bestehen bleiben** so lange beide Peers erreichbar sind. Ein remote `GO_IDLE` von einem Legacy-Lazy-Peer soll als full-tunnel-Idle behandelt werden (= lazy semantics), nicht als ICE-only-Detach (= dynamic semantics).

### 1.3 Quantitative Evidenz

| Timestamp | Event | Quelle |
|-----------|-------|--------|
| 17:24:10 | dk20 ICE up, configure WG endpoint to `72.137.199.238:34887` | trace-log |
| 17:24:39 | first WG handshake (0.37 s) | trace-log |
| 17:34:09 | Dump stat: Status Connected, WGCheckSuccess=52, P2PConnected=2 | trace-log |
| 17:39:34 | `remote peer signaled GO_IDLE (p2p-dynamic)` | [`conn_mgr.go:544`](../../client/internal/conn_mgr.go) |
| 17:39:46 | `ICE retries exhausted (3/3), switching to hourly retry` | [`ice_retry_state.go:52`](../../client/internal/peer/guard/ice_retry_state.go#L52) |

15 min 24 s stabile P2P → match Elmira's `DefaultInactivityThreshold = 15 * time.Minute` aus v0.51.2-Quellcode, NICHT unser `p2p_timeout_seconds=180` oder `legacy_lazy_fallback_timeout_seconds=300`.

---

## 2. Root cause (Codex-bestätigt)

**Zwei separate Bugs, beide in Phase-3.7i deployt:**

### 2.1 Bug A — Mode-Cross-Talk in `ConnMgr.DeactivatePeer`

[`client/internal/conn_mgr.go:532-565`](../../client/internal/conn_mgr.go#L532-L565):

```go
func (e *ConnMgr) deactivatePeerAction() deactivateAction {
    switch e.mode {                                  // ← LOKALER Mode
    case connectionmode.ModeP2PLazy:
        return deactivateLazy
    case connectionmode.ModeP2PDynamic:
        return deactivateICE                          // ← falsch für remote-lazy
    default:
        return deactivateNoop
    }
}

func (e *ConnMgr) DeactivatePeer(conn *peer.Conn) {
    switch e.deactivatePeerAction() {                 // ← dispatched nach lokal
    case deactivateLazy:
        e.lazyConnMgr.DeactivatePeer(conn.ConnID())
    case deactivateICE:
        e.DetachICEForPeer(conn.GetKey())             // ← Relay bleibt, ICE weg
    ...
```

**Problem:** Der Dispatch ignoriert `conn.RemoteEffectiveMode()`. Wenn Elmira (remote-effective: `p2p-lazy`) ein `Body_GO_IDLE` sendet, sollte das als **Phase-1-Lazy-Idle** behandelt werden (= full close, peer fällt zurück zu Activity-Listener-Idle). Stattdessen interpretiert dk20 (lokal `p2p-dynamic`) als **Phase-2-Dynamic-ICE-Detach** → ICE ist weg, Relay bleibt, Verbindung in "Relayed"-Zustand.

### 2.2 Bug B — Guard-Retry-Counter verbraucht Budget bei intentional Detach

[`client/internal/peer/guard/guard.go:142-150`](../../client/internal/peer/guard/guard.go#L142-L150):

```go
case ConnStatusPartiallyConnected:
    if iceState.shouldRetry() {         // ← inkrementiert retries
        callback()                       // ← onGuardEvent: könnte Offer skippen
    } else {
        iceState.enterHourlyMode()       // ← nach 3 Ticks: hourly mode
        ...
```

[`client/internal/peer/guard/ice_retry_state.go:35-48`](../../client/internal/peer/guard/ice_retry_state.go#L35-L48):

```go
func (s *iceRetryState) shouldRetry() bool {
    if s.hourly != nil {
        return true
    }
    s.retries++                          // ← inkrementiert IMMER
    if s.retries <= maxICERetries {      // maxICERetries = 3
        return true
    }
    return false
}
```

**Problem:** Nach intentional `DetachICEForPeer` ist der Conn-State `PartiallyConnected` (Relay up, ICE down). Der Guard-Loop ruft `shouldRetry()`, verbraucht Budget. `onGuardEvent()` kann zwar den Offer skippen (z. B. weil `RemoteEffectiveMode == p2p-lazy` oder weil ICE intentional detached ist), aber der Counter wurde bereits inkrementiert. Nach 3 Ticks → `enterHourlyMode()` → Backoff-Eskalation.

[`client/internal/peer/conn.go:1605-1614`](../../client/internal/peer/conn.go#L1605-L1614) zeigt, dass `iceBackoff` schon korrekt unverändert bleibt bei intentional Detach. **Aber der separate `iceRetryState` im Guard hat diesen Schutz nicht.**

### 2.3 Hintergrund — v0.51.2 sendet selbst

Codex hat verifiziert (gegen Source-Tag `v0.51.2`):
- `signal/proto/signalexchange.proto: GO_IDLE = 5` ist in v0.51.2 vorhanden
- `Signaler.SignalIdle()` existiert
- v0.51.2 sendet `Body_GO_IDLE` aus `Conn.Close(signalToRemote=true)` wenn lokaler lazy manager den Peer als inaktiv markiert
- v0.51.2 nutzt `DefaultInactivityThreshold = 15 * time.Minute` (lokaler Default)
- v0.51.2-PeerConfig hat **kein** Feld für Server-pushed Timeout — unser `legacy_lazy_fallback_timeout_seconds=300` erreicht sie nicht

→ Elmira sendet GO_IDLE legitim aus ihrer eigenen Logik. Der Bug ist **ausschließlich auf der Empfänger-Seite** (Phase-3.7i `dk20`).

---

## 3. Proposed fix

### 3.1 Fix A — Mode-Aware `DeactivatePeer`

**Idee:** `DeactivatePeer` dispatched nach **`conn.RemoteEffectiveMode()`** primär; der lokale Mode ist nur Fallback bei `ModeUnspecified` (Bootstrap-Race).

**Code-Änderung in [`conn_mgr.go`](../../client/internal/conn_mgr.go):**

```go
// deactivatePeerActionFor returns the per-peer deactivation rule based
// on the REMOTE peer's effective connection mode (server-resolved).
// Local mode is only used as fallback when the remote effective mode
// is unknown (status recorder bootstrap race before first NetworkMap).
func (e *ConnMgr) deactivatePeerActionFor(conn *peer.Conn) deactivateAction {
    remote := conn.RemoteEffectiveMode()
    switch remote {
    case connectionmode.ModeP2PLazy:
        return deactivateLazy
    case connectionmode.ModeP2PDynamic:
        return deactivateICE
    case connectionmode.ModeUnspecified:
        // Status recorder not yet populated; fall back to local mode.
        return e.deactivatePeerAction()
    default:
        return deactivateNoop
    }
}

func (e *ConnMgr) DeactivatePeer(conn *peer.Conn) {
    switch e.deactivatePeerActionFor(conn) {
    case deactivateLazy:
        if !e.isStartedWithLazyMgr() {
            // Local manager is eager/dynamic but the remote peer wants
            // full close. Fall through to ICE detach so we at least
            // free the ICE pair; relay stays up.
            conn.Log.Infof("remote peer signaled GO_IDLE (lazy semantics) but local mgr not lazy; falling back to ICE detach")
            if err := e.DetachICEForPeer(conn.GetKey()); err != nil {
                conn.Log.Warnf("DetachICEForPeer failed: %v", err)
            }
            return
        }
        conn.Log.Infof("closing peer connection: remote peer initiated inactive, idle lazy state and sent GOAWAY (mode-aware)")
        e.lazyConnMgr.DeactivatePeer(conn.ConnID())
    case deactivateICE:
        conn.Log.Infof("detaching ICE worker: remote peer signaled GO_IDLE (p2p-dynamic, mode-aware)")
        if err := e.DetachICEForPeer(conn.GetKey()); err != nil {
            conn.Log.Warnf("DetachICEForPeer failed: %v", err)
        }
    case deactivateNoop:
        return
    }
}
```

**Backward-compat:** `deactivatePeerAction()` (das alte) bleibt unverändert für andere Aufrufer (gibt's? Eigentlich keine, aber sicher ist sicher).

### 3.2 Fix B — Intentional-Detach-Marker für Guard

**Idee:** `Conn` markiert sich nach `DetachICEForPeer` als `intentionallyDetached`. Der Guard-Loop schaut diesen Marker an im `ConnStatusPartiallyConnected`-Pfad und skipped `shouldRetry()`-Increment.

**Code-Änderung in [`client/internal/peer/conn.go`](../../client/internal/peer/conn.go):**

```go
type Conn struct {
    ...
    // intentionallyDetached signals to the guard that the current ICE
    // detached state is the result of a remote/local GO_IDLE or
    // local inactivity timeout — not a pair-check failure. The guard
    // must NOT consume its retry budget while this flag is set.
    //
    // Cleared on next signal-driven activation (ActivatePeer) or
    // on local network change (peerActivity event from guard).
    intentionallyDetached atomic.Bool
}

func (conn *Conn) MarkIntentionallyDetached() {
    conn.intentionallyDetached.Store(true)
}

func (conn *Conn) IsIntentionallyDetached() bool {
    return conn.intentionallyDetached.Load()
}

func (conn *Conn) ClearIntentionallyDetached() {
    conn.intentionallyDetached.Store(false)
}
```

**Aufrufpunkt:** [`conn_mgr.go:DetachICEForPeer`](../../client/internal/conn_mgr.go) ruft `conn.MarkIntentionallyDetached()` direkt vor dem detach.

**Code-Änderung in [`guard.go:140-150`](../../client/internal/peer/guard/guard.go#L140-L150):**

```go
case ConnStatusPartiallyConnected:
    if g.conn.IsIntentionallyDetached() {
        // ICE was detached intentionally (GO_IDLE or local timeout).
        // Do NOT burn the retry budget — wait for a real network event
        // (peerActivity, srReconnected, or remote signal) to reset.
        g.log.Debugf("guard: intentional detach active; skipping retry tick")
    } else if iceState.shouldRetry() {
        callback()
    } else {
        iceState.enterHourlyMode()
        ticker.Stop()
        tickerChannel = iceState.hourlyC()
    }
```

**Marker-Clear:**
- In `ConnMgr.ActivatePeer` (signal-driven wake-up): `conn.ClearIntentionallyDetached()` vor `conn.Open(ctx)`.
- In Guard `peerActivity`-handler: indirekt via Conn-State-Reset.

### 3.3 Fix C — Detach-Reason im Log

**Idee:** Bei jedem `DetachICEForPeer`-Aufruf wird ein Reason mitgegeben:
- `remote-lazy-go-idle` (Mode-aware: Remote ist p2p-lazy)
- `remote-dynamic-go-idle` (Remote ist p2p-dynamic)
- `local-ice-timeout` (lokaler Phase-2-Inactivity-Manager)
- `failure` (ICE pair-check broken — von `onICEFailed`)
- `network-reset` (manueller reset bei SR-watcher reconnect)

```go
type DetachReason string

const (
    DetachReasonRemoteLazyGoIdle    DetachReason = "remote-lazy-go-idle"
    DetachReasonRemoteDynamicGoIdle DetachReason = "remote-dynamic-go-idle"
    DetachReasonLocalIceTimeout     DetachReason = "local-ice-timeout"
    DetachReasonFailure             DetachReason = "failure"
    DetachReasonNetworkReset        DetachReason = "network-reset"
)

func (e *ConnMgr) DetachICEForPeerWithReason(peerKey string, reason DetachReason) error {
    // ... existing logic
    conn.Log.Infof("ICE detach (reason=%s)", reason)
    conn.MarkIntentionallyDetached()
    return ...
}
```

Vorteile: bessere Diagnostik, einfachere Regression-Tests.

---

## 4. Test plan

### 4.1 Unit-Tests (neu)

**Datei: `client/internal/conn_mgr_test.go`** (oder neuer `_deactivate_mode_test.go`):

```go
// Bug A: GO_IDLE von remote-lazy-peer → full lazy close, NICHT ICE-only
func TestDeactivatePeer_RemoteLazy_ClosesFully(t *testing.T) {
    // Setup: ConnMgr in p2p-dynamic mode, lazyMgr available
    // Conn mit RemoteEffectiveMode = ModeP2PLazy
    // Trigger DeactivatePeer
    // Assert: lazyConnMgr.DeactivatePeer wurde aufgerufen (nicht DetachICE)
}

func TestDeactivatePeer_RemoteLazy_NoLazyMgr_FallsBackToDetach(t *testing.T) {
    // ConnMgr ohne lazyMgr (eager mode), Remote-lazy peer sendet GO_IDLE
    // Assert: DetachICEForPeer aufgerufen (graceful fallback), nicht silent noop
}

func TestDeactivatePeer_RemoteDynamic_DetachesICEOnly(t *testing.T) {
    // Existing behavior: ConnMgr p2p-dynamic, Remote-dynamic peer GO_IDLE
    // Assert: DetachICEForPeer aufgerufen
}

func TestDeactivatePeer_RemoteUnspecified_FallsBackToLocalMode(t *testing.T) {
    // Status recorder hat RemoteEffectiveMode noch nicht gesetzt
    // Assert: dispatched nach local mode (existing behavior)
}
```

**Datei: `client/internal/peer/guard/guard_test.go`** (extended):

```go
// Bug B: intentional detach darf retry budget nicht verbrauchen
func TestGuard_IntentionalDetach_SkipsRetryBudget(t *testing.T) {
    // Mock Conn mit IsIntentionallyDetached() = true
    // 10× tick im ConnStatusPartiallyConnected state
    // Assert: iceState.retries bleibt 0
    // Assert: kein enterHourlyMode call
}

func TestGuard_RealICEFailure_StillUsesRetryBudget(t *testing.T) {
    // Mock Conn mit IsIntentionallyDetached() = false
    // 5× tick im PartiallyConnected
    // Assert: nach 4. tick → hourlyMode (Backward-compat)
}

func TestGuard_DetachThenActivate_ResetsBudget(t *testing.T) {
    // 1× intentional detach → markers set
    // 1× ActivatePeer (signal-driven) → markers cleared
    // 1× simulated real failure → shouldRetry erlaubt 3 retries again
}
```

### 4.2 Integration-Test (manual, auf S21 oder dk20)

1. Trace-Log auf testendem Client (`--log-level trace`)
2. P2P-Verbindung zu Legacy-Peer (z. B. Elmira) etablieren
3. 16+ min warten bis Elmira's 15-min-Threshold zuschlägt
4. **Erwartung mit Fix:**
   - Log: `closing peer connection: remote peer initiated inactive, idle lazy state and sent GOAWAY (mode-aware)`
   - **Nicht:** `detaching ICE worker: remote peer signaled GO_IDLE (p2p-dynamic)`
   - Peer geht in lazy-idle (activity-listener wartet)
   - Bei lokalem Traffic → wieder aktiv via Activity-Trigger
5. **Backoff-Verhalten:** kein `ICE retries exhausted, switching to hourly retry` mehr für legitimen Idle-Disconnect

### 4.3 Regressions-Risiko

Risiken:
- Mode-Aware-Dispatch: wenn `RemoteEffectiveMode` durch Bug nicht gesetzt → fällt auf lokal zurück (Default-Fall) → kein Regressions-Risiko
- Intentional-Detach-Marker: wenn Marker stuck (nicht gecleared) → Guard schickt keine Re-Connect-Offers → könnte zu cold-tunnel führen. Mitigation: Marker wird auf JEDEN ActivatePeer/peerActivity/SR-reconnect gecleared.

**Vor PR-Erstellung:**
- `go test ./client/internal/conn_mgr_test.go -run TestDeactivatePeer -count=1`
- `go test ./client/internal/peer/guard/... -count=1 -race`
- `go test ./client/internal/lazyconn/... -count=1` (regression)

---

## 5. Implementation plan (TDD-Reihenfolge)

| Commit | Inhalt | Tests rot/grün nach Commit |
|--------|--------|------------------------------|
| 1 | Failing tests aus §4.1 (alle 7 Tests) | compile-fail bzw. fail (kein RemoteEffectiveMode-aware dispatch, kein IntentionallyDetached marker) |
| 2 | `Conn.MarkIntentionallyDetached/Clear/Is` Marker einführen + `DetachICEForPeer` ruft `MarkIntentionallyDetached()` | Guard-Tests grün, ConnMgr-Tests noch rot |
| 3 | `ConnMgr.deactivatePeerActionFor` + `DeactivatePeer` umstellen auf `RemoteEffectiveMode` | alle Tests grün |
| 4 (optional) | Detach-Reason-Enum + Logging | nur Doku-Fix |

---

## 6. Branch- und PR-Strategie (für später, nach Codex-Freigabe)

**Korrektur gegenüber orphan-disconnect-Spec:** GO_IDLE-Mode-Aware ist ein **Phase-3.7j-Increment**, keine Korrektur zu Phase-3.7i.

- **Spec-only:** `spec/phase37j-go-idle-mode-aware` (dieses Markdown-File)
- **Implementierung später:** `pr/i-phase37j-go-idle-mode-aware`
  - **Base:** `pr/h-phase37i-orphan-disconnect` (dort ist `RemoteEffectiveConnectionMode`-Infrastruktur bereits enthalten)
- **Force-push:** nur mit `--force-with-lease`
- **Co-Author-Trailer:** keine. Author + committer `Michael Uray <25169478+MichaelUray@users.noreply.github.com>`
- **Upstream-PR:** **nicht ohne explizite User-Freigabe**

---

## 7. Offene Fragen für Codex-Review (Round 1)

1. **Marker-Lifecycle**: `intentionallyDetached` Marker — welche Events sollten ihn EXPLIZIT clearen?
   - Vorschlag: `ActivatePeer`, `peerActivity` (im Guard), `srReconnected`, `network-change`
   - Alternative: nur 1 zentraler Clear-Pfad in `Conn.OpenICEWorker()` oder ähnlich

2. **Fallback-Verhalten** bei `RemoteEffectiveMode == ModeUnspecified`:
   - Aktueller Vorschlag: fällt auf lokal-Mode zurück (= current behavior)
   - Codex-Hinweis war: "in onGuardEvent skip OFFER for RemoteEffectiveMode=p2p-lazy". Sollte für `Unspecified` ein Skip-Modus existieren?

3. **Eager-Mode-Behandlung** (`p2p`, `relay-forced`): aktuell `deactivateNoop`. Soll Eager-Mode überhaupt jemals `GO_IDLE` empfangen (=Server-Push falsch konfiguriert)? Oder ist `noop` korrekt?

4. **Detach-Reason-Enum**: ist §3.3 ein nice-to-have oder zwingend für die Spec? Codex hat es vorgeschlagen ("Track why ICE is detached"). Einbauen oder separates Spec?

5. **Phase-2 Cross-Site (PVE5 ↔ CTB-Lebring) als gleiches Bug?**
   - Codex hat es als gleiches Pattern beschrieben
   - Mit `RemoteEffectiveConnectionMode = p2p-dynamic` (beide Phase-3.7i) würde Fix A nicht greifen (geht in den `deactivateICE`-Zweig)
   - **Brauchen wir zusätzlich Fix B (Guard-Retry-Härtung) für Cross-Dynamic Same-Mode**? Codex' Empfehlung war "C: For RemoteEffectiveConnectionMode == p2p-dynamic, accept remote GO_IDLE only if local transport activity also looks idle". Soll das in dieser Spec rein?

6. **Konsistenz mit Phase-3.7i orphan-disconnect-Fix**: dort haben wir `firstSeenAt` für Orphan-Peers eingeführt. Der GO_IDLE-Empfang triggert `lazyConnMgr.DeactivatePeer` (für `deactivateLazy`-Pfad). Ist das korrekt orchestriert oder gibt's eine Race?

---

## 8. Akzeptanz-Kriterien

- [ ] Unit-Tests aus §4.1 grün (`go test ./client/internal/...`)
- [ ] `go test -race ./client/internal/peer/guard/... -count=1` grün
- [ ] Integration-Test (S21 oder dk20) zeigt: nach 16 min Idle + Elmira-GO_IDLE → kein `ICE retries exhausted, switching to hourly retry` mehr
- [ ] Log-Verifikation: Mode-Aware-Pfad sichtbar als `(mode-aware)` oder via Detach-Reason
- [ ] Kein neuer Lint-/Vet-Fehler
- [ ] Kein neuer Race im `-race`-Build
- [ ] Vorhandene `lazyconn/...`-Tests bleiben grün (kein Regression zur Phase-3.7i orphan-disconnect-Spec)

---

## 9. Anhang — relevanter Code (Stand `pr/h@220914567` / `build/orphan-disconnect-on-combined@ee9f78c17`)

### 9.1 `client/internal/conn_mgr.go:532-565` (current)

```go
type deactivateAction int

const (
    deactivateNoop deactivateAction = iota
    deactivateLazy
    deactivateICE
)

func (e *ConnMgr) deactivatePeerAction() deactivateAction {
    switch e.mode {
    case connectionmode.ModeP2PLazy:
        return deactivateLazy
    case connectionmode.ModeP2PDynamic:
        return deactivateICE
    default:
        return deactivateNoop
    }
}

func (e *ConnMgr) DeactivatePeer(conn *peer.Conn) {
    switch e.deactivatePeerAction() {
    case deactivateLazy:
        ...
    case deactivateICE:
        e.DetachICEForPeer(conn.GetKey())
    case deactivateNoop:
        return
    }
}
```

### 9.2 `client/internal/peer/guard/ice_retry_state.go:35-48` (current)

```go
func (s *iceRetryState) shouldRetry() bool {
    if s.hourly != nil {
        return true
    }
    s.retries++
    if s.retries <= maxICERetries {
        return true
    }
    return false
}
```

### 9.3 `client/internal/peer/conn.go:752-758` (RemoteEffectiveMode accessor — schon vorhanden)

```go
func (conn *Conn) RemoteEffectiveMode() connectionmode.Mode {
    return conn.remoteEffectiveMode()
}
```

→ kann sofort genutzt werden, kein Refactor nötig.

---

**ENDE Spec v0.1 — Bitte zur Review an Codex.**
