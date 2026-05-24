# Phase-3.7j — GO_IDLE Mode-Aware Receiver + Guard Retry Hardening + Local-Activity-Gate

**Status:** DRAFT v0.3 — Codex round-2-review eingearbeitet, **implementation-ready candidate**
**Date:** 2026-05-24
**Author:** Michael Uray (MichaelUray)

### Changelog
- **v0.3 (2026-05-24)** — Codex round-2-review komplett eingearbeitet:
  - **`localActivityGateWindow` dynamisch** aus `p2pTimeoutSecs / 2` mit `[30s, 300s]` Clamp (statt fix 90s).
  - **`ModeUnspecified` Fallback geändert**: nicht mehr "lokal-Mode" sondern `deactivateLazy` (wenn LazyMgr aktiv) sonst `deactivateNoop` + diagnostisches Log. Vermeidet Phase-1/Phase-2-Cross-Talk während NetworkMap-Bootstrap-Race.
  - **Pseudo-Code korrigiert**: `e.iface` statt `e.wgIface` (= echter Feldname in `ConnMgr`).
  - **Userspace-only-Limit von Fix C explizit dokumentiert** in §3.3 Caveat-Box: Kernel-Mode `LastActivities()` returns nil, Gate greift dort nicht — Kernel-Mode-Geräte sollten ohnehin `NB_WG_KERNEL_DISABLED=true` setzen (cross-reference auf `reference_netbird_kernel_mode_pair_selection_bug`).
  - **Implementation-Plan §5 umgestellt auf buildbare Commits**: kein absichtlich roter Tests-only-Commit. Tests werden in dem Code-Commit eingeführt der sie grün macht. PR-Stack ist jederzeit `go build ./...` und `go vet ./...` clean.
- **v0.2 (2026-05-24)** — Codex round 1 review punkte eingearbeitet:
  - **Guard-Predicate-Callback statt direkter Conn-Zugriff:** Guard kennt Conn nicht; neuer `isIntentionalDetach func() bool` Konstruktor-Parameter (Codex Option 2).
  - **Fix C (echter Local-Activity-Gate) hinzugefügt:** für `RemoteEffectiveMode == ModeP2PDynamic` ist der GO_IDLE-Empfang gegated durch lokale Activity-Recorder-Prüfung. Damit decken wir das Phase-3.7i ↔ Phase-3.7i Cross-Site-Problem ab.
  - **Detach-Reason-Enum → Fix D (optional/nice-to-have)** umgekennzeichnet (war fälschlich als "C" benannt in v0.1).
  - **Marker-Lifecycle präzisiert** in §3.2: 6 explizite Clear-Points (AttachICE / AttachICEUserInitiated / AttachICEOnRelayActivity / ActivatePeer / SR-reconnect / **onICEFailed**).
  - **Counter-Trennung Guard-hourly vs `iceBackoff failure #N`** als Begriffsglossar in §2.4 ergänzt — die zwei Counter sind kausal verkettet aber nicht der gleiche Counter.
- **v0.1 (2026-05-24)** — initiale Spec.
**Related:**
- Live-Test-Report: [`docs/test-reports/2026-05-24-codex-review-response-ice-detach-bug.md`](../../../docs/test-reports/2026-05-24-codex-review-response-ice-detach-bug.md) (Codex root-cause analysis)
- Memory: `reference_netbird_p2p_dynamic_go_idle_drop.md`
- Vorgänger-Spec: [`2026-05-24-phase37i-p2p-lazy-orphan-disconnect-spec.md`](2026-05-24-phase37i-p2p-lazy-orphan-disconnect-spec.md) (v0.3.1, implementiert in `pr/h-phase37i-orphan-disconnect@220914567`)

**Scope:**
- `client/internal/conn_mgr.go` (GO_IDLE-Dispatch nach RemoteEffectiveConnectionMode + Local-Activity-Gate)
- `client/internal/peer/guard/guard.go` (neuer `isIntentionalDetach` Predicate-Callback)
- `client/internal/peer/conn.go` (Intentional-Detach-Marker + Clear-Points)
- `client/iface/bind/activity.go` (Reader-Accessor für `LastActivity[peerKey]` von ConnMgr aus)

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

### 2.4 Begriffsglossar — Guard-hourly vs. `iceBackoff failure #N`

Diese zwei Counter sind **kausal verkettet** (eines triggert das andere) aber **nicht der gleiche Counter** — wichtig für sauberes Debugging:

| Counter | Lebensort | Zählt was | Konsequenz bei Overflow |
|---------|-----------|-----------|-------------------------|
| **Guard `iceRetryState.retries`** | [`guard/ice_retry_state.go`](../../client/internal/peer/guard/ice_retry_state.go) | Jeder Tick im `ConnStatusPartiallyConnected`-State zählt | nach 3 ticks → `enterHourlyMode()` |
| **`Conn.iceBackoff` failure-counter** | [`peer/conn.go`](../../client/internal/peer/conn.go) `onICEFailed` | Echte Pion-`ConnectionStateFailed`-Events | exponentieller Backoff, Logzeile `failure #N` |

**Kausalkette in der Praxis:** Remote `GO_IDLE` → `DetachICEForPeer` (intentional) → Conn-State `PartiallyConnected` → Guard tickt → `shouldRetry()` inkrementiert (Bug B) → nach 3 Ticks `enterHourlyMode` → hourly callback → Conn versucht echtes ICE-Re-Pair → fails (oder hängt) → `onICEFailed` markiert `iceBackoff` → exponentielle `failure #N`-Eskalation.

Beide Counter müssen unabhängig hardened werden:
- **Bug B Fix** = Guard darf bei intentional detach `shouldRetry` NICHT inkrementieren.
- **iceBackoff** ist bereits korrekt geschützt (siehe `conn.go:1605-1614` Codex-Kommentar) — kein zusätzlicher Fix nötig.

---

## 3. Proposed fix

### 3.1 Fix A — Mode-Aware `DeactivatePeer`

**Idee:** `DeactivatePeer` dispatched nach **`conn.RemoteEffectiveMode()`** primär. Bei `ModeUnspecified` (NetworkMap-Bootstrap-Race) ist die **konservativste sichere Aktion lazy-full-close wenn LazyMgr aktiv, sonst noop** — **NICHT** lokal-dynamic-Detach. Damit ist die Phase-1/Phase-2-Cross-Talk-Lücke auch während des Bootstrap geschlossen.

**Code-Änderung in [`conn_mgr.go`](../../client/internal/conn_mgr.go):**

```go
// deactivatePeerActionFor returns the per-peer deactivation rule based
// on the REMOTE peer's effective connection mode (server-resolved).
//
// ModeUnspecified Fallback (Codex round 2): während des NetworkMap-
// Bootstrap-Race ist RemoteEffectiveMode noch unbekannt. In dieser
// kurzen Lücke ist die konservativ-sichere Aktion lazy-full-close
// (wenn LazyMgr aktiv) — NICHT lokal-dynamic-Detach. Damit
// vermeiden wir das in §2.1 dokumentierte Cross-Talk-Symptom auch
// während des Bootstrap. Wenn LazyMgr nicht aktiv ist (eager modes)
// ist die korrekte Aktion noop mit Diagnose-Log.
func (e *ConnMgr) deactivatePeerActionFor(conn *peer.Conn) deactivateAction {
    remote := conn.RemoteEffectiveMode()
    switch remote {
    case connectionmode.ModeP2PLazy:
        return deactivateLazy
    case connectionmode.ModeP2PDynamic:
        return deactivateICE
    case connectionmode.ModeUnspecified:
        // Bootstrap race: prefer the safer lazy-full-close if we have a
        // LazyMgr; otherwise emit a diagnostic and noop.
        if e.isStartedWithLazyMgr() {
            conn.Log.Debugf("GO_IDLE during RemoteEffectiveMode bootstrap race; using lazy-full-close fallback")
            return deactivateLazy
        }
        conn.Log.Debugf("GO_IDLE during RemoteEffectiveMode bootstrap race + no LazyMgr; treating as noop")
        return deactivateNoop
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

### 3.2 Fix B — Intentional-Detach-Marker + Predicate-Callback im Guard

**Idee:** `Conn` markiert sich nach `DetachICEForPeer` als `intentionallyDetached`. Der **Guard erhält einen neuen `isIntentionalDetach func() bool` Predicate-Callback** als Konstruktor-Parameter — Guard kennt `Conn` nicht direkt (siehe v0.1 Codex Review).

**Code-Änderung in [`client/internal/peer/conn.go`](../../client/internal/peer/conn.go):**

```go
type Conn struct {
    ...
    // intentionallyDetached signals to the guard that the current ICE
    // detached state is the result of a remote/local GO_IDLE or
    // local inactivity timeout — not a pair-check failure. The guard
    // must NOT consume its retry budget while this flag is set.
    //
    // Cleared on the 6 explicit lifecycle points listed below
    // (§3.2 Marker-Lifecycle).
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

**Code-Änderung in [`guard.go`](../../client/internal/peer/guard/guard.go):**

```go
// IsIntentionalDetachFunc reports whether the peer's current ICE-detached
// state is the result of a graceful idle signal (GO_IDLE or local
// inactivity timeout). The guard uses this predicate inside the
// PartiallyConnected branch to skip the retry-budget increment when
// the detach was intentional.
//
// Returning false (= default for new code paths) preserves the
// existing retry semantics for actual ICE pair-check failures.
type IsIntentionalDetachFunc func() bool

type Guard struct {
    log                     *log.Entry
    isConnectedOnAllWay     connStatusFunc
    isIntentionalDetach     IsIntentionalDetachFunc   // NEW: nil-safe
    timeout                 time.Duration
    ...
}

func NewGuard(
    log *log.Entry,
    isConnectedFn connStatusFunc,
    isIntentionalDetachFn IsIntentionalDetachFunc,   // NEW param
    timeout time.Duration,
    srWatcher *SRWatcher,
) *Guard {
    return &Guard{
        log:                 log,
        isConnectedOnAllWay: isConnectedFn,
        isIntentionalDetach: isIntentionalDetachFn,
        ...
    }
}

// In the guard loop ConnStatusPartiallyConnected branch:
case ConnStatusPartiallyConnected:
    if g.isIntentionalDetach != nil && g.isIntentionalDetach() {
        // ICE was detached intentionally (GO_IDLE or local timeout).
        // Do NOT burn the retry budget — wait for a real network event.
        g.log.Debugf("guard: intentional detach active; skipping retry tick")
        // NOTE: tickerChannel stays as-is (no reset, no enterHourlyMode);
        // next periodic tick will re-check.
    } else if iceState.shouldRetry() {
        callback()
    } else {
        iceState.enterHourlyMode()
        ticker.Stop()
        tickerChannel = iceState.hourlyC()
    }
```

**Marker-Lifecycle — 6 explizite Clear-Points** (v0.2 Korrektur):

| # | Wann clear | Code-Pfad | Begründung |
|---|------------|-----------|------------|
| 1 | Vor jedem `conn.AttachICE()` | [`conn.go:1450`](../../client/internal/peer/conn.go#L1450) | User-/Signal-getriebener ICE-Aufbau ist Gegenstück zum intentional detach |
| 2 | Vor jedem `conn.AttachICEUserInitiated()` | [`conn.go:1497`](../../client/internal/peer/conn.go#L1497) | Lokaler-Traffic-getriebener wake-up |
| 3 | Vor jedem `conn.AttachICEOnRelayActivity()` | [`conn.go:1341`](../../client/internal/peer/conn.go#L1341) | Relay-Activity-getriebener re-attach |
| 4 | In `ConnMgr.ActivatePeer` direkt vor `conn.Open(ctx)` | [`conn_mgr.go`](../../client/internal/conn_mgr.go) | Signal-driven wake-up nach Remote-OFFER |
| 5 | Beim SR-watcher reconnect (`srReconnected` event) | [`guard.go:175+`](../../client/internal/peer/guard/guard.go#L175) | Netzwerk-Change → frischer ICE-Cycle, Marker veraltet |
| 6 | **In `Conn.onICEFailed` direkt nach `markFailure`** | [`conn.go:1615`](../../client/internal/peer/conn.go#L1615) | echte Pion-Failure muss als Failure sichtbar bleiben — darf NICHT durch einen stale intentional-Marker maskiert werden |

**Wichtig** (Codex round-1 hervorgehoben): Punkt 6 ist nicht-optional. Ohne ihn könnte eine echte ICE-Failure direkt nach einem intentional Detach durch den Marker maskiert werden, und Guard würde nie auf legitime Failure-Eskalation umschalten.

### 3.3 Fix C — Local-Activity-Gate für remote-dynamic GO_IDLE

**Idee** (Codex round-1 echte Fix C): wenn `RemoteEffectiveMode == ModeP2PDynamic` und ein Remote sendet `GO_IDLE`, prüfe **vor dem ICE-Detach** ob der lokale `ActivityRecorder` für diesen Peer Aktivität in den letzten N Sekunden gesehen hat. Falls ja: skip detach (lokale Sicht weiß: Tunnel wird tatsächlich gerade benutzt).

**Motivation:** Im Phase-3.7i ↔ Phase-3.7i Cross-Site-Pfad (z.B. CTB-Lebring ↔ PVE5) sendet eine Seite GO_IDLE basierend auf ihrer lokalen Activity-Sicht. Wenn aber die ANDERE Seite gerade aktiv ist (z.B. der Cross-Site VNC-Traffic läuft real), ist es falsch, eine aktive P2P-Strecke zu detachen — die lokale Activity-Sicht wissens besser.

**Code-Änderung in [`conn_mgr.go:DeactivatePeer`](../../client/internal/conn_mgr.go):**

```go
const (
    // localActivityGateMinWindow ist die Untergrenze für die Toleranz.
    // Verhindert dass bei sehr kurzem p2p_timeout_seconds (z. B. Tests) das
    // Gate praktisch sofort offen ist und Detach-Storms verursacht.
    localActivityGateMinWindow = 30 * time.Second
    // localActivityGateMaxWindow ist die Obergrenze. Verhindert dass bei
    // sehr großem p2p_timeout_seconds (z. B. 24h-Tweak) das Gate über
    // sinnvolle WG-NAT-Mapping-Zeiten hinauswächst.
    localActivityGateMaxWindow = 5 * time.Minute
)

// localActivityGateWindow leitet die Toleranz dynamisch aus dem aktuellen
// p2pTimeoutSecs ab und clampt in [Min, Max]. p2pTimeoutSecs/2 als
// Default-Halbschritt: wenn die andere Seite nach iceTimeout idle signal
// sendet, hat die lokale Seite die ersten p2pTimeoutSecs/2 Sekunden noch
// als "kürzlich aktiv" — ohne das Gate zu lang aufzudehnen.
func (e *ConnMgr) localActivityGateWindow() time.Duration {
    if e.p2pTimeoutSecs == 0 {
        return localActivityGateMinWindow
    }
    w := time.Duration(e.p2pTimeoutSecs/2) * time.Second
    if w < localActivityGateMinWindow {
        return localActivityGateMinWindow
    }
    if w > localActivityGateMaxWindow {
        return localActivityGateMaxWindow
    }
    return w
}

func (e *ConnMgr) DeactivatePeer(conn *peer.Conn) {
    switch e.deactivatePeerActionFor(conn) {
    case deactivateLazy:
        // ... existing lazy-close path (Fix A) ...
    case deactivateICE:
        // Fix C: Local-Activity-Gate für remote-dynamic GO_IDLE.
        // Wenn lokaler ActivityRecorder kürzlich Activity gesehen hat,
        // ist die P2P-Strecke wirklich aktiv und ein remote-stale-idle
        // Signal soll sie nicht detachen.
        //
        // CAVEAT: LastActivities() ist nur im Userspace-Mode befüllt
        // (Kernel-Mode kernel_unix.go:330 returns nil). Im Kernel-Mode
        // greift dieser Gate nicht — siehe §3.3 Caveat-Box unten.
        if e.iface != nil && e.iface.IsUserspaceBind() {
            activities := e.iface.LastActivities()
            if last, ok := activities[conn.GetKey()]; ok {
                gate := e.localActivityGateWindow()
                if monotime.Since(last) < gate {
                    conn.Log.Infof("ICE detach skipped: remote GO_IDLE but local activity %v ago (within %v gate)",
                        monotime.Since(last), gate)
                    return
                }
            }
        }
        // Kein Activity-Eintrag ODER zu alt ODER Kernel-Mode → Detach durchführen
        conn.Log.Infof("detaching ICE worker: remote peer signaled GO_IDLE (p2p-dynamic, mode-aware)")
        if err := e.DetachICEForPeer(conn.GetKey()); err != nil { ... }
    case deactivateNoop:
        return
    }
}
```

> **Caveat — Userspace-only Gate** (Codex round-2 hervorgehoben):
>
> `WGIface.LastActivities()` ist ausschließlich im **Userspace-Mode** befüllt. [`client/iface/configurer/kernel_unix.go:330-331`](../../client/iface/configurer/kernel_unix.go#L330-L331) returns `nil` im Kernel-Mode. Damit greift Fix C dort **nicht** und der `GO_IDLE`-Empfang führt direkt zum ICE-Detach (= alte v0.1-Semantik).
>
> Konsequenz: Kernel-Mode-Geräte profitieren nur von Fix A + B, nicht von Fix C. Sie sollten ohnehin `NB_WG_KERNEL_DISABLED=true` setzen — siehe Memory [`reference_netbird_kernel_mode_pair_selection_bug.md`](../../../../home/ai-agent/.claude/projects/-opt-infrastructure/memory/reference_netbird_kernel_mode_pair_selection_bug.md). Für die strukturelle Lösung im Kernel-Mode wäre ein separater Spec-Eintrag nötig (z. B. Phase-3.7k: Kernel-Mode-LastActivity-Wrapper über `wgctrl`-Statistiken).

### 3.4 Fix D (optional) — Detach-Reason im Log

**Idee:** Bei jedem `DetachICEForPeer`-Aufruf wird ein Reason mitgegeben:
- `remote-lazy-go-idle` (Mode-aware: Remote ist p2p-lazy)
- `remote-dynamic-go-idle` (Remote ist p2p-dynamic, Fix C-Gate gepasst)
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

**Optional**: kann in dieser Spec mitkommen oder als follow-up. Bringt Diagnose-Verbesserung, keine funktionale Änderung.

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

func TestDeactivatePeer_RemoteUnspecified_LazyMgrFallsBackToLazy(t *testing.T) {
    // Status recorder hat RemoteEffectiveMode noch nicht gesetzt;
    // ConnMgr läuft mit aktivem LazyMgr (p2p-lazy oder p2p-dynamic + Lazy).
    // Assert: lazyConnMgr.DeactivatePeer aufgerufen (safer full-close),
    //         NICHT DetachICEForPeer.
}

func TestDeactivatePeer_RemoteUnspecified_NoLazyMgrNoops(t *testing.T) {
    // ConnMgr ohne LazyMgr (eager modes), RemoteEffectiveMode unspezifiziert.
    // Assert: weder DetachICEForPeer noch lazyConnMgr.DeactivatePeer
    //         aufgerufen; nur Diagnose-Log.
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

## 5. Implementation plan (buildbare Commits)

**Codex round-2 Korrektur:** kein absichtlich roter Tests-only-Commit im publizierten PR-Stack. Tests werden in dem Code-Commit eingeführt, der sie grün macht. PR-Stack ist jederzeit `go build ./...` + `go vet ./...` + `go test ./...` clean.

| Commit | Inhalt | Build-State nach Commit | Test-State |
|--------|--------|--------------------------|------------|
| 1 | `Conn.{Mark,Clear,Is}IntentionallyDetached` + 6 Clear-Points + `DetachICEForPeer` Hook + `onICEFailed`-Clear **mit zugehörigen Marker-Lifecycle-Tests** | green | green (alle Marker-Tests grün; vorhandene Tests unverändert) |
| 2 | `Guard.IsIntentionalDetachFunc` predicate-Callback + Konstruktor-Param + PartiallyConnected-Skip **mit Guard-Skip-Tests** | green | green (Guard-Tests grün; alle Aufrufer von `NewGuard` passen sich an) |
| 3 | `ConnMgr.deactivatePeerActionFor` + `DeactivatePeer` umstellen auf `RemoteEffectiveMode` (Fix A) **mit Mode-Dispatch-Tests** | green | green (Fix-A Tests grün; ModeUnspecified-Bootstrap-Race-Test inkludiert) |
| 4 | `ConnMgr.DeactivatePeer` Local-Activity-Gate (Fix C) + `localActivityGateWindow()` Helper **mit Activity-Gate-Tests** | green | green (Fix-C Tests grün; Userspace + Kernel-Mode-Fallback-Test) |
| 5 (optional) | Detach-Reason-Enum + Logging (Fix D) **mit Reason-Tests** | green | green (Diag-Tests grün, kein funktionaler Wechsel) |

**Build-Reihenfolge-Constraint:**
- Commit 1 (Conn-Marker) muss vor Commit 2 (Guard-Predicate) — sonst hat der Predicate kein Implement.
- Commit 2 muss vor Commit 3+4 — sonst können Fix-A/C-Tests den Guard-Verifikations-Pfad nicht aufrufen.
- Commit 5 ist orthogonal und kann am Ende, vorne, oder weggelassen werden.

**Rationale für buildbare Commits:** ein PR der mit `git bisect` durchlaufen werden kann braucht jeden Commit clean. Außerdem erlaubt es Reviewern den Stack inkrementell zu reviewen + kontextuell mergen, falls ein PR-Commit unabhängig ist.

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

## 7. Designfragen — Status

**Aus Round 1 final geklärt (in v0.2 eingearbeitet):**
- Guard-Interface: predicate-Callback (Codex Option 2) statt direkter Conn-Zugriff ✓
- Fix C als echter Local-Activity-Gate eingeführt (war v0.1 falsch als "Detach-Reason" benannt) ✓
- Marker-Lifecycle: 6 explizite Clear-Points inkl. `onICEFailed` ✓
- Counter-Trennung Guard-hourly vs `iceBackoff failure #N` als Begriffsglossar §2.4 ✓

**Aus Round 2 final geklärt (in v0.3 eingearbeitet):**
- `localActivityGateWindow` ist dynamisch aus `p2pTimeoutSecs / 2` abgeleitet und auf `[30s, 5min]` geclampt (§3.3) ✓
- `ModeUnspecified`-Fallback geht auf `deactivateLazy` (wenn LazyMgr aktiv) sonst `deactivateNoop` mit Diagnose-Log — nicht mehr lokal-dynamic-Detach (§3.1) ✓
- Pseudo-Code auf `e.iface` korrigiert (`ConnMgr` hat `e.iface`, nicht `e.wgIface`) ✓
- Userspace-only-Limit von Fix C explizit als Caveat-Box dokumentiert; Kernel-Mode greift Gate nicht (§3.3) ✓
- Implementation-Plan §5 auf buildbare Commits umgestellt (kein roter Tests-only-Commit, Tests im jeweiligen Code-Commit grün) ✓

**Keine offenen Designfragen vor Implementierung.**

Eager-Mode (`p2p`, `relay-forced`)-Behandlung bleibt bewusst bei `deactivateNoop` (über `default`-Branch in `deactivatePeerActionFor`) — Eager-Modes sollen kein `GO_IDLE` empfangen; falls doch, ist es ein Server-Config-Mismatch der per `noop` ignoriert wird (kein Detach, kein Hourly-Mode). Detach-Reason-Enum (Fix D) bleibt optional in dieser Spec und kann als letzter Commit weggelassen werden.

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

**ENDE Spec v0.3 — implementation-ready candidate.**
