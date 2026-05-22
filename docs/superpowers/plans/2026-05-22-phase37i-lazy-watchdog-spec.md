---
status: spec / pre-implementation review
target-branch: pr/g-phase3.7i-lazy-watchdog (off upstream/main)
related-work: pr/mgmt-stream-keepalive, pr/mgmt-stream-watchdog, feat/force-relay-flag, test/plan1+plan2-combined
review-status: 1× Codex pre-review done (H1/H2/H3 classification accepted, fix-scope refined)
---

# Phase-3.7i Lazy-Connection State-Reconciliation Bug — Spezifikation

## 1. Problem-Statement

In `p2p-dynamic`-Mode (Phase-3.7i) bleibt ein Peer auf der lokalen Client-Seite über
unbegrenzte Zeit in einem inkonsistenten Lazy-Manager-State hängen, in dem er nie mehr
in den Activity-Watcher-Pfad zurückkehrt. Symptomatisch:

- `peer ICE idle since: <timestamp>` + `peer relay idle since: <timestamp>` werden im
  Minutentakt geloggt (per `inactivity/manager.go:243-247`)
- `connectivity guard check, relay state: Disconnected, ice state: Disconnected` läuft
  regelmäßig (per `peer/conn.go:1119`)
- `sending offer with serial: <stable-session-id>` wird wiederholt geschickt
- **Aber**: kein `ICE ConnectionState has changed to Checking`, kein
  `activity detected via LazyConn`, kein State-Transition zurück nach
  `watcherActivity`

Der Bug ist **plattformneutral**. Auf Android sind die Trigger-Bedingungen (Doze,
App-Standby, kurze Goroutine-Pausen, NetworkCallback-Drosselung) häufiger als auf
Linux/Windows, aber der zugrundeliegende Code-Pfad ist OS-unabhängig. Auch
Linux-Hosts nach Suspend/Resume oder mehrtägiger Laufzeit können denselben
Failure-Mode treffen.

Force-Stop der NetBird-App ist der einzige bekannte Workaround. Direkt nach
Service-Restart läuft Activity-Detection → ICE-Negotiation → P2P-Aufbau in unter
1 Sekunde, mit identischem Build und identischen Settings.

## 2. Empirische Belege

### 2.1 Test-Matrix (live verifiziert, 2026-05-22)

| Test-Client | OS | Build | Lazy | Connection-Type zu Elmira (0.51.2) |
|---|---|---|---|---|
| uray-mic-dh | Win11 | 0.68.0-dev-8f71b190d | false | ✅ P2P (User-Report, stabil seit Monaten) |
| dk20 | Linux Debian 13 | 0.68.0-dev-8f71b190d | false | ✅ P2P frisch in <2s, srflx/srflx, 135ms RTT |
| w11-test1 | Win11 | 0.68.0-dev-0ca25fe4c | true | ✅ P2P frisch in <2s, srflx/srflx, 130ms RTT |
| w11-test1 | Win11 | **combined-1428d2831** (Plan-2 oben drauf) | true | ✅ P2P frisch in <2s, host/srflx, 130ms RTT |
| S21 (Daemon-Restart) | Android 15 | combined-1428d2831 | true | ✅ P2P in 660ms (Checking → Connected), srflx/srflx, 135ms RTT |
| **S21 (Daemon-Uptime > 24h)** | **Android 15** | **combined-1428d2831** | **true** | **❌ stuck, kein ICE-Versuch** |

Damit ausgeschlossen:
- NAT-Topologie (uray-mic-dh + S21 hinter selber Public-IP 41.66.90.21)
- OS-Stack-Differenz (dk20 Linux + w11-test1 Win11 + S21 Android schaffen alle P2P
  frisch — alte Daemons nicht zwingend)
- mein Plan-2 / SyncPeerConnections-Race-Fix-Code (w11-test1 mit identischem Build
  schafft P2P frisch)
- Lazy-Setting (w11-test1 lazy=true schafft P2P frisch)
- Elmira (Legacy 0.51.2 + p2p-lazy via Server-Push) — wird von allen frischen
  Clients erfolgreich erreicht

### 2.2 Logs S21 — pre-Force-Stop (Daemon-PID 31923, Uptime >24h)

```
05-22 18:57:23.439 [DEBG] lazyconn/inactivity/manager.go:243
   peer ICE idle since: 2026-05-21 09:15:32.420694923 +0000 UTC
05-22 18:57:23.439 [INFO] lazyconn/inactivity/manager.go:247
   peer relay idle since: 2026-05-21 09:15:32.420694923 +0000 UTC
05-22 18:57:28.864 [TRAC] peer/conn.go:1119
   connectivity guard check, relay state: Disconnected, ice state: Disconnected
05-22 18:57:28.864 [INFO] peer/handshaker.go:225
   sending offer with serial: 5e86fe4200          ← stable serial (workerICE SessionID)
05-22 18:58:12.091 [TRAC] peer/conn.go:1119
   connectivity guard check, relay state: Disconnected, ice state: Disconnected
05-22 18:58:12.091 [INFO] peer/handshaker.go:225
   sending offer with serial: 5e86fe4200          ← same serial — same WorkerICE
05-22 18:58:23.439 [DEBG] lazyconn/inactivity/manager.go:243
   peer ICE idle since: 2026-05-21 09:15:32...    ← idle for 33+ hours
[KEIN "ICE ConnectionState has changed to Checking"]
[KEIN "activity detected via LazyConn"]
[KEIN "onPeerInactivityTimedOut"-relevanter Log]
```

### 2.3 Logs S21 — post-Force-Stop (Daemon-PID 32510, frisch)

```
19:02:33.441 [INFO] lazyconn/activity/listener_bind.go:103
   activity detected via LazyConn
19:02:33.443 [INFO] lazyconn/manager/manager.go:582
   detected peer activity
19:02:33.443 [DEBG] peer/conn.go:1541
   ICE listener attached (locked path)
19:02:33.443 [INFO] peer/handshaker.go:225
   sending offer with serial: 6e34ca8bc1          ← new serial (new WorkerICE)
19:02:33.601 [DEBG] peer/worker_ice.go:547
   ICE ConnectionState has changed to Checking
19:02:34.260 [DEBG] peer/worker_ice.go:547
   ICE ConnectionState has changed to Connected   ← 660ms total
19:02:34.260 [DEBG] peer/worker_ice.go:536
   successful ICE path: udp4 srflx 41.66.90.21:32695 <-> 72.137.199.238:34887  rtt=135ms
```

## 3. Verworfene Hypothesen (mit Begründung)

### H1 (refined): „Android-Power-Management / Doze killt Activity-Listener"

**Ursprünglich**: ich vermutete Doze-Mode unterbreche den UDP-Read-Loop in
`lazyconn/activity/listener_bind.go` und beim Wake-up reagiere er nicht mehr.

**Codex-Korrektur (akzeptiert)**: Das ist plausibel als Trigger, aber zu
plattformspezifisch formuliert. Der eigentliche Code-Bug ist die fehlende
Selbstheilung der Lazy-State-Machine. **Wenn Doze nur Trigger wäre, würde das
Vordergrund-Bringen der App das Problem lösen — tut es aber nicht.** Beweis:
User-Test, `Disconnect`-Button in der App reagiert auch nicht, bleibt in
„Disconnecting"-State stehen. Foreground-Aktivität reaktiviert das App-UI,
aber der VPN-Foreground-Service mit seinem inkonsistenten Goroutine-State
läuft unverändert weiter.

→ Bug ist plattformneutral. Doze macht ihn auf Android wahrscheinlicher, aber
Linux-Suspend/Resume oder einfach lange Laufzeit kann ihn auch erzeugen.

### H2 (teilweise korrekt): „connectivity guard schickt Offers ohne ICE-Re-Init"

`peer/conn.go:1119` → `peer/conn.go:831` ruft `SendOffer()` auf, aber re-initialisiert
ICE nicht. Das ist für normale Recovery-Pfade korrekt (Guard ist Signal-Retry,
nicht Lazy-State-Reconciler), aber im stuck-state führt es zu Endlos-Offer-Loop
ohne Effekt.

→ Guard im stuck-state ist Symptom, nicht Ursache. Ein Fix DORT würde Architektur
korrumpieren (Guard und Lazy-Manager hätten überlappende Verantwortung). Fix
gehört in Lazy-Manager.

### H3 (refuted): „Gleiche Offer-Serial ist Bug"

`peer/handshaker.go:224` baut die Serial aus `workerICE.SessionID()`, die per
`worker_ice.go:71` einmalig bei `NewWorkerICE()` erzeugt wird. Eine neue Serial
entsteht erst bei `closeAgent()` oder neuem Worker — Force-Stop erzwingt neuen
Worker → neue Serial. Wiederholungen mit gleicher Serial sind erwartetes
Retry-Verhalten innerhalb derselben WorkerICE-Lifetime, nicht Bug.

## 4. Root-Cause-Analyse

### 4.1 Beweismaterial-Synthese

Im stuck-state läuft genau dies regelmäßig (alle ~30-60 s):

```
inactivity/manager.go:243 — "peer ICE idle since: <T>"
inactivity/manager.go:247 — "peer relay idle since: <T>"
peer/conn.go:1119         — "connectivity guard check, relay=Disc, ice=Disc"
peer/handshaker.go:225    — "sending offer with serial: <stable>"
```

Aber NIE:
```
lazyconn/activity/listener_bind.go:103 — "activity detected via LazyConn"
lazyconn/manager/manager.go:582        — "detected peer activity"
lazyconn/manager/manager.go:653        — "connection timed out"
lazyconn/manager/manager.go:664        — "start activity monitor"
```

Der erwartete Pfad nach `relayIdle > relayTimeoutSeconds` ist:

`inactivity/manager.go:checkStats` → `notifyChan(inactivePeersChan)` →
`manager/manager.go:186 select case peerIDs := <-InactivePeersChan()` →
`onPeerInactivityTimedOut(peerIDs)` (manager.go:629) →
`PeerConnIdle` (manager.go:656) + `expectedWatcher = watcherActivity` (manager.go:658) +
`RemovePeer` + `MonitorPeerActivity` (manager.go:664).

Im stuck-state passiert nichts davon. Der Peer verbleibt im
`expectedWatcher == watcherInactivity` State und der Activity-Watcher wird nie
neu gestartet.

### 4.2 Konkrete Drop-Stelle (Codex-Befund verifiziert)

`client/internal/lazyconn/inactivity/manager.go:202-209`:

```go
func (m *Manager) notifyChan(ctx context.Context, ch chan map[string]struct{}, peers map[string]struct{}) {
    select {
    case ch <- peers:
    case <-ctx.Done():
        return
    default:
        return                       // ← SILENT DROP
    }
}
```

**Kritisch (verifiziert im Code)**: Beide Notify-Channels sind mit Buffer 1 gebaut:

```go
// inactivity/manager.go (Constructor):
iceInactiveChan:   make(chan map[string]struct{}, 1),
inactivePeersChan: make(chan map[string]struct{}, 1),
```

Mit Buffer 1 reicht es schon, dass der Consumer in `manager/manager.go:186`
**einen einzigen Tick** im case-arm hängt (z.B. weil er gerade in `onPeerInactivityTimedOut`
ist und der `PeerConnIdle`-Aufruf auf line 656 laut Code-Kommentar
„blocking operation, potentially can be optimized" ist), dann sind alle weiteren
Notifications binnen Sekunden silent verworfen — bis der Consumer wieder leert.

Wenn der Consumer durch eine vorige panic gestorben ist oder dauerhaft blockiert,
sind die Drops permanent. Aus außen sieht das aus wie:
- `inactivity/manager.go:checkStats` läuft korrekt
- Logged „peer relay idle since: T" jeden Tick
- Schickt aber nichts mehr durch
- Lazy-Manager kriegt nichts mit → kein State-Heal

Aus außen sieht das aus wie:
- `inactivity/manager.go:checkStats` läuft korrekt
- Logged „peer relay idle since: T" jeden Tick
- Schickt aber nichts mehr durch
- Lazy-Manager kriegt nichts mit → kein State-Heal

### 4.3 Warum bleibt der Consumer hängen?

Mehrere Möglichkeiten, die Codex nennt als realistisch:

1. **Consumer ist tot durch frühere panic-Resilience**: das Lazy-Manager-Select hat
   `recover()`-Pfade die einzelne goroutine-panics auffangen, aber wenn die
   Select-Loop selbst durch eine panic in einem case beendet wurde, ist der Consumer
   weg.
2. **`onPeerInactivityTimedOut` blockierte**: per Code-Kommentar in `manager.go:656`
   ist `PeerConnIdle` „blocking operation, potentially can be optimized". Wenn ein
   einzelner `PeerConnIdle`-Aufruf hängt (z.B. durch Wireguard-Tunnel-Removal das im
   Kernel auf Timeout läuft), blockiert die ganze Consumer-Schleife.
3. **Goroutine-Pause unter Doze**: Android pausiert Goroutines aus einem
   Foreground-Service kürzer, aber wenn `select` mit `inactivePeersChan` aufwacht und
   die Konsumlogik gerade an einer Stelle steht wo sie Network-Operations macht,
   können diese unter Doze fehlschlagen oder hängen. Beim Wake-up ist der State
   inkonsistent.

In allen drei Fällen ist das **Symptom dasselbe**: notifyChan-Drops sind silent, der
Lazy-Manager bekommt das nicht mit, kein Watchdog erkennt das, der Peer ist stuck.

### 4.4 Existierende Recovery-Pfade die nicht greifen

- **`conn_mgr.go:568 RecoverPeerToIdle`**: existiert für WG-Handshake-Timeout-Recovery.
  Geht durch `lazyConnMgr.DeactivatePeer(connID)` (lazyconn/manager/manager.go:321),
  was funktional fast identisch ist zu `onPeerInactivityTimedOut`. Aber dieser Pfad
  wird nur von Engine-Side-Watchdogs aufgerufen (WG-Handshake-Timeout), nicht
  ausgelöst durch wiederholte idle-Detektionen.
- **`peer/guard/guard.go:141 SendOffer`-Loop**: der Connectivity-Guard schickt fleißig
  Offers aber re-initialisiert ICE nicht.
- **Mein Plan-2 stream-watchdog**: arbeitet auf der Mgmt-Stream-Ebene
  (`grpc.go:streamWatchdog`), erkennt mgmt-stream-Outages, nicht Lazy-State-Stalls.

→ Es gibt aktuell keinen Code-Pfad der „wiederholte Lazy-Idle ohne
State-Transition" detektiert und reagiert.

## 5. Architektur des Fixes

### 5.1 Design-Prinzipien (Codex-Empfehlung)

1. **Sichtbarkeit zuerst**: silent drops müssen mindestens loggable sein, idealerweise
   counter-instrumentiert. Allein das Logging macht zukünftige Diagnose 10× einfacher.
2. **State-Heilung im Lazy-Manager**, nicht im Guard. Guard bleibt Signal-Retry.
3. **Bestehende Recovery-Funktionen wiederverwenden** (`RecoverPeerToIdle` /
   `DeactivatePeer` / `onPeerInactivityTimedOut`-Pfad) statt neue ICE-Re-Init-Logik
   im Guard einbauen.
4. **Keine Serial-Regeneration im Handshaker** — würde Offer-Storms und
   Session-Churn erzeugen, gegen Phase-3.7i-Effizienz-Design.
5. **Plattformneutral**: keine Android-spezifischen `protect()`-Calls oder
   Doze-Detection. Der Fix muss auch auf Linux nach Suspend/Resume helfen.

### 5.2 Drei-Stufen-Patch

#### Stufe 1: notifyChan-Drops sichtbar machen
**Datei**: `client/internal/lazyconn/inactivity/manager.go` (line 202)
**Scope**: ~15 LOC + 1 atomic counter pro Manager-Instanz

```go
type Manager struct {
    // ... existing fields ...
    notifyDropsRelay atomic.Uint64
    notifyDropsICE   atomic.Uint64
}

func (m *Manager) notifyChan(ctx context.Context, ch chan map[string]struct{}, peers map[string]struct{}, kind string) {
    select {
    case ch <- peers:
    case <-ctx.Done():
        return
    default:
        var n uint64
        switch kind {
        case "relay":
            n = m.notifyDropsRelay.Add(1)
        case "ice":
            n = m.notifyDropsICE.Add(1)
        }
        // Throttle log: every 10 drops AND every minute boundary
        if n % 10 == 1 {
            log.Warnf("inactivity/manager: notifyChan %q full, dropped event (total drops=%d for this kind, %d peers in batch). " +
                      "Consumer (lazyconn/manager) may be blocked. Lazy-state reconciliation will rely on the new watchdog.",
                      kind, n, len(peers))
        }
        return
    }
}
```

Tests:
- Bestehender Test in `inactivity/manager_test.go` muss `kind`-Argument erhalten.
- Neuer Test `TestNotifyChan_FullChannelLogsAndCounts` der einen vollen Channel
  simuliert und prüft dass `notifyDropsRelay`-Counter inkrementiert + Warn-Log
  emittiert wird.

#### Stufe 2: Reconcile-Watchdog im Lazy-Manager
**Datei**: `client/internal/lazyconn/manager/manager.go` (neue Methode am `Manager`)
**Scope**: ~80 LOC + 1 Goroutine + 1 Ticker

Eine separate Watchdog-Goroutine im Lazy-Manager, die periodisch (alle 60 s) prüft:

```go
type managedPeer struct {
    // existing fields
    lastIdleSeen        time.Time   // updated when peer is in relay-idle state
    consecutiveIdleObs  int         // counts consecutive idle observations without state transition
}

func (m *Manager) reconcileWatchdog(ctx context.Context) {
    ticker := time.NewTicker(m.reconcileInterval)  // default 60s
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            m.checkStuckPeers()
        }
    }
}

func (m *Manager) checkStuckPeers() {
    m.managedPeersMu.Lock()
    defer m.managedPeersMu.Unlock()
    
    now := time.Now()
    relayTO := m.cfg.RelayTimeoutSeconds  // from PeerConfig per Phase-3.7i
    
    for peerID, mp := range m.managedPeersByConnID {
        if mp.expectedWatcher != watcherInactivity {
            continue  // not our case
        }
        
        // Snapshot peer state via inactivityManager + status recorder
        relayIdleFor := m.inactivityManager.RelayIdleSince(mp.peerCfg.PublicKey)
        iceState, relayState := m.peerStore.PeerConnState(mp.peerCfg.PublicKey)
        
        // STUCK CRITERIA:
        // (a) inactivity says "relay idle longer than 2× relayTimeout"
        // (b) actual conn state is relay=Disc AND ice=Disc
        // (c) expectedWatcher claims we should be watched by inactivity
        //   → these three together mean: inactivity-event got lost, peer needs heal
        if relayIdleFor > 2 * relayTO && relayState == Disconnected && iceState == Disconnected {
            mp.consecutiveIdleObs++
            mp.peerCfg.Log.Warnf("lazy-state stuck: relay idle %s > 2× timeout %s, watcher=%s, " +
                "consecutive obs=%d — forcing reconciliation",
                relayIdleFor, relayTO, mp.expectedWatcher, mp.consecutiveIdleObs)
            
            if mp.consecutiveIdleObs >= 2 {
                // Force-reconcile: same path as onPeerInactivityTimedOut
                m.forceReconcileToActivityWatcher(mp)
                mp.consecutiveIdleObs = 0  // reset after action
            }
        } else {
            mp.consecutiveIdleObs = 0  // reset if state is sane
        }
    }
}

func (m *Manager) forceReconcileToActivityWatcher(mp *managedPeer) {
    // Mirror of onPeerInactivityTimedOut body, lock-free (caller holds managedPeersMu)
    mp.peerCfg.Log.Infof("watchdog: forcing transition watcherInactivity → watcherActivity (stuck-state recovery)")
    
    m.peerStore.PeerConnIdle(mp.peerCfg.PublicKey)
    mp.expectedWatcher = watcherActivity
    m.inactivityManager.RemovePeer(mp.peerCfg.PublicKey)
    
    if err := m.activityManager.MonitorPeerActivity(*mp.peerCfg); err != nil {
        mp.peerCfg.Log.Errorf("watchdog: failed to create activity monitor: %v", err)
    }
}
```

Tests:
- `TestReconcileWatchdog_DetectsStuckPeer`: zwei `checkStuckPeers()`-Calls mit stuck
  state, assert dass nach dem zweiten der Peer in `watcherActivity` ist.
- `TestReconcileWatchdog_IgnoresHealthyPeer`: peer mit recent state-transition wird
  NICHT geheilt.
- `TestReconcileWatchdog_RespectsHA`: shouldDeferIdleForHA-Logik aus
  `onPeerInactivityTimedOut` muss auch im Watchdog gelten (sonst Race mit HA-Failover).

#### Stufe 3: Aktivierung des Watchdogs
**Datei**: `client/internal/conn_mgr.go` (initLazyManager-Pfad)
**Scope**: 2 LOC

Wenn `e.lazyConnMgr` erzeugt wird, zusätzlich:

```go
go e.lazyConnMgr.RunReconcileWatchdog(e.ctx)
```

Lebenszyklus identisch zum Lazy-Manager — der Watchdog stoppt automatisch wenn
`e.ctx` cancelled wird oder der Lazy-Manager beendet wird.

### 5.3 Was NICHT geändert wird (explizit)

- `peer/handshaker.go:224` (Serial-Generation) — bleibt unverändert.
- `peer/conn.go:831` (Guard SendOffer) — bleibt unverändert.
- `peer/worker_ice.go` (Reset-Logik) — bleibt unverändert.
- Keine Android-spezifischen Code-Pfade.
- Keine Plan-2-mgmt-stream-watchdog-Erweiterungen.

## 6. Test-Plan

### 6.1 Unit-Tests (in derselben PR)
1. `TestNotifyChan_FullChannelLogsAndCounts` (Stufe 1)
2. `TestReconcileWatchdog_DetectsStuckPeer` (Stufe 2)
3. `TestReconcileWatchdog_IgnoresHealthyPeer` (Stufe 2)
4. `TestReconcileWatchdog_RespectsHA` (Stufe 2)
5. `TestReconcileWatchdog_StartStop` (Stufe 3) — Watchdog-Goroutine lifecycle

Alle laufen mit `go test -race`. Bestehende Tests in
`client/internal/lazyconn/inactivity/manager_test.go` müssen `kind`-Parameter-Änderung
ohne Regression überleben.

### 6.2 Integration-Tests (in derselben PR)
- E2E-Test der einen stuck-state künstlich erzeugt (Consumer blockieren via
  injizierte Channel-Lese-Pause) und prüft dass innerhalb von 2 Watchdog-Ticks
  der Peer in `watcherActivity` ist.

### 6.3 Hardware-Soak (vor merge)
Build deploy auf:
- 1× S21 (Android, primärer Failure-Site)
- 1× dk20 (Linux, sekundärer)
- 1× w11-test1 (Win11, Control)

Soak-Dauer: 72 h ohne Force-Stop. Erfolgs-Kriterien:
- Mindestens 1× stuck-state durch Watchdog erkannt + geheilt (oder kein stuck-state
  überhaupt eingetreten — beides ist OK)
- Keine spurious Recoveries auf gesunden Peers (false positives)
- Keine Performance-Regression (Watchdog-Overhead < 0.1% CPU)
- VNC-Verbindung zu Elmira testbar nach 24h, 48h, 72h ohne manuelle Recovery

## 7. Risiken + offene Fragen

### 7.1 Risiken

| # | Risiko | Severity | Mitigation |
|---|---|---|---|
| R1 | Watchdog false-positive: gesunder Peer wird gestört | Mittel | `consecutiveIdleObs >= 2` Schwelle + `shouldDeferIdleForHA`-Check |
| R2 | Race zwischen `onPeerInactivityTimedOut` und Watchdog-Reconcile | Mittel | Beide nutzen `managedPeersMu`; Reconcile prüft `expectedWatcher == watcherInactivity` redundant |
| R3 | Watchdog stört intentionalen LazyTimeout-Konfig (z.B. Admin setzt RelayTimeout=24h für long-idle peers) | Niedrig | Schwelle ist `2× relayTimeout`, also skaliert mit Admin-Config |
| R4 | `PeerConnIdle` ist „blocking operation" (per Code-Kommentar) — Watchdog hält Lock zu lang | Mittel | Vorher `forceReconcileToActivityWatcher` außerhalb des Lock-Scope spawn-en, oder PeerConnIdle in eigener Goroutine starten und Lock freigeben |
| R5 | notifyChan-Drop-Counter wächst monoton → keine echte Heilung | Niedrig | Counter ist Telemetrie, keine Korrektur-Logik; Heilung passiert via Watchdog |

### 7.2 Offene Fragen für Codex

1. **~~Channel-Buffer-Größe~~** (selbst verifiziert): `iceInactiveChan` und
   `inactivePeersChan` haben beide Buffer 1. Mit Buffer 1 ist Drop fast garantiert
   bei jeder Consumer-Pause. Damit ist die silent-drop-These nicht mehr ein
   Verdacht sondern strukturell zwingend. **Frage an Codex**: ist Buffer 1
   absichtlich (z.B. um Backpressure auf den Producer zu üben), oder
   konservative Default-Wahl? Falls absichtlich: bitte erklären, sonst sollte
   Stufe 1 auch die Buffer-Erhöhung erwägen (z.B. auf len(managedPeers) oder
   ein Vielfaches). Aktuell Spec geht NICHT mit Buffer-Erhöhung, weil das
   Symptom (stuck Consumer) auch mit größerem Buffer irgendwann auftreten würde.

2. **`PeerConnIdle` blocking behavior**: ist das echt blocking (Wireguard-Kernel-Call)
   oder nur „blocking unter Last"? Wenn echt blocking, dann sollte
   `forceReconcileToActivityWatcher` async laufen.

3. **Consumer-Tod durch panic**: gibt es im Lazy-Manager-Code einen Pfad wo das
   `select` in `manager.go:186` durch eine Panic in einem case komplett beendet
   wird? Wenn ja, ist mein Watchdog-Fix unzureichend — wir bräuchten ZUSÄTZLICH
   einen Goroutine-Liveness-Detector. Bitte das Panic-Handling im Select-Body
   verifizieren.

4. **`RecoverPeerToIdle` vs. `forceReconcileToActivityWatcher`**: kann ich
   `conn_mgr.go:568 RecoverPeerToIdle` direkt aufrufen statt eine eigene Funktion
   zu schreiben? Codex' frühere Empfehlung war diesen Pfad wiederzuverwenden. Ich
   habe die Funktion ausgemodelliert — wenn semantisch identisch, einfacher.
   Spec geht aktuell mit einer dedizierten `forceReconcileToActivityWatcher`-Methode
   weil die Mutex-Hierarchie sauberer ist (caller hält bereits den Lock).

5. **Default-Schwelle „2× relayTimeout"**: ist das die richtige Konstante? Soll
   das konfigurierbar sein (via Account-Setting `legacy_lazy_fallback_timeout_seconds`
   oder dediziertem Knopf), oder als Hartkodierung okay?

6. **HA-Defer-Logik**: `onPeerInactivityTimedOut` hat eine `shouldDeferIdleForHA`-Logik
   bei manager.go:648. Soll der Watchdog dieselbe Logik anwenden, oder ist
   stuck-state ein „override" der HA-Defer-Logik?

## 8. Rollout-Plan

### 8.1 Branch + Commit-Struktur
Neuer Branch `pr/g-phase3.7i-lazy-watchdog` auf `upstream/main` (NICHT auf
mgmt-stream-watchdog oder force-relay-flag stacken).

Commits (3 separate commits für reviewbarkeit):
1. `lazyconn/inactivity: log + count silent notifyChan drops`
2. `lazyconn/manager: reconcile-watchdog for stuck inactivity-state` (incl. tests)
3. `conn_mgr: start lazyconn reconcile-watchdog with lazy manager`

Author + Committer: `Michael Uray <25169478+MichaelUray@users.noreply.github.com>`.
Keine Co-Authored-By-Trailer.

### 8.2 Deployment-Stufen
1. **Stage 1 (canary)**: Combined-Branch (test/plan1+plan2-combined inkl. dieser
   Patches) auf S21 deployen. Monitor 72h auf:
   - Auftreten von „notifyChan ... full, dropped event" Warn-Logs
   - Auftreten von „lazy-state stuck ... forcing reconciliation" Warn-Logs
   - VNC-Connect-Erfolgsrate zu Elmira ohne Force-Stop
2. **Stage 2**: Auf alle 5 BM-Router + r1-pve5 ausrollen. Monitor 7 Tage.
3. **Stage 3**: Upstream-PR an netbirdio/netbird (sobald separat Codex re-review +
   User-OK vorliegen).

### 8.3 Rollback-Plan
Patch ist additiv (neue Goroutine + 1 zusätzlicher counter). Rollback = revert der
3 Commits, kein Schema/State-Change nötig. Bestehende Lazy-Manager-Logik bleibt
unverändert; der Watchdog läuft nur ZUSÄTZLICH.

## 9. Beziehung zu anderen offenen Branches

| Branch | Status | Konflikt mit dieser Spec? |
|---|---|---|
| `pr/mgmt-stream-keepalive` (PR-A) | ready, behind=0 | Nein |
| `pr/mgmt-stream-watchdog` (PR-B + Codex BLOCKER-fixes) | ready, behind=0 | Nein |
| `feat/force-relay-flag` (Step-1 force_relay proto field) | ready, server-side TODO | Nein |
| `test/plan1+plan2-combined` | deployt auf 6 Hosts | dieser Branch oben drauf reicht es nach |

Dieser Patch ist semantisch unabhängig von den drei anderen.

## 10. Verifikations-Material (für Codex)

- Spec liegt in `docs/superpowers/plans/2026-05-22-phase37i-lazy-watchdog-spec.md`
- Live-Diagnostic-Logs S21 pre/post: `/tmp/s21_diag_result.txt` +
  `/tmp/s21_elmira_v2.log` auf der Build-Maschine
- Mgmt-API live-zugänglich (Token in `.env.local`)
- API-IDs:
  - Elmira: `d22dq33kp1rs73dg79b0` (NetBird 0.51.2 Legacy, IP 100.87.72.3)
  - S21 (o1sxeea): `d79qj4m26gt000f8hcp0` (NetBird combined-1428d2831, IP 100.87.223.194)
  - dk20: NetBird IP 100.87.158.151 (0.68.0-dev-8f71b190d)
  - w11-test1: NetBird IP 100.87.118.182 (0.68.0-dev-0ca25fe4c)
- Code-Refs sind alle gegen `test/plan1+plan2-combined` Tip `1428d2831`

## Codex-Review-Anfrage

Bitte review:
1. **Root-Cause-Analyse**: stimmt meine Synthese der pre-state-Logs (Section 4.1-4.3)
   überein mit Deiner früheren Klassifikation? Habe ich H1/H2/H3 korrekt gewürdigt?
2. **Patch-Scope**: sind die drei Stufen ausreichend und nicht overengineered?
   Sollte etwas weggelassen oder hinzugefügt werden?
3. **Risiko-Liste (Section 7.1)**: fehlt ein kritisches Risiko? Sind die Mitigations
   ausreichend?
4. **Offene Fragen (Section 7.2)**: insbesondere Q3 (Consumer-Tod durch panic) — bitte
   den Lazy-Manager-Select-Body in `manager/manager.go:186` checken auf
   panic-Resilience-Pfade.
5. **`RecoverPeerToIdle`-Wiederverwendung (Q4)**: sollte ich die dedizierte
   Funktion abschaffen und stattdessen `conn_mgr.RecoverPeerToIdle` aufrufen
   (cross-package call vom Lazy-Manager aus)?
6. **Test-Plan-Abdeckung (Section 6)**: deckt das den stuck-state robust ab?
   Insbesondere: gibt es einen sauberen Weg den Consumer in `manager.go:186`
   im Test künstlich zu blockieren?
7. **Branch-Strategie**: `pr/g-phase3.7i-lazy-watchdog` separat, nicht stacken — okay?
8. **Klassifikation**: BLOCKER-für-Phase-3.7i-Upstreaming wie Codex' Vorbefund?
   Oder SHOULD-FIX?

Nach Codex-OK: Implementation in 3 Commits laut Section 8.1, Tests laut Section 6.1,
Hardware-Soak laut Section 6.3.
