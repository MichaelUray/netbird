---
status: spec v0.4 / pre-implementation review (round 4)
target-branch: see Section 8.1 — Stufe 1 separat upstream-fähig auf upstream/main, Stufen 0+2+3+4+5 auf phase3.7i-runtime-bugfixes-v0.5
related-work: pr/mgmt-stream-keepalive, pr/mgmt-stream-watchdog, feat/force-relay-flag, phase3.7i-runtime-bugfixes-v0.5
review-status: 4× Codex pre-review — v0.3 returned with 1 BLOCKER + 2 SHOULD-FIXes, addressed in v0.4
changelog:
  - v0.1 (2026-05-22 vormittag): Initial spec, Code-Refs verifiziert, Buffer-Größe 1 als kritischer Befund
  - v0.2 (2026-05-22 nachmittag): Codex round-1 Korrekturen — Stufe-2-Pseudocode mit echten APIs, Lock-Strategie, Panic-Recovery
  - v0.3 (2026-05-23 vormittag): Codex round-2 Korrekturen — Panic-Hypothese verworfen, Stufen 2+3 lock-sparsam, neue Stufe 5 TransportSnapshot, HA-Batch-Korrektur
  - v0.4 (2026-05-23 nachmittag): Codex round-3 Korrekturen:
    * BLOCKER — Recovery-Reihenfolge in `transitionToActivityWatcherIOAfterUnlock`
      ruft `PeerConnIdle()` VOR `MonitorPeerActivity()`. Wenn `PeerConnIdle`
      blockiert (die vermutete Stuck-Ursache!), wird der Activity-Listener nie
      armiert und der Peer landet in `watcherActivity` ohne Listener. Fix:
      `MonitorPeerActivity` zuerst armieren, `PeerConnIdle` danach als async
      best-effort goroutine. Damit kann nächster Traffic auch bei hängendem
      Close den Wake signalisieren.
    * SHOULD-FIX 1 — `Conn.TransportSnapshot()` v0.3-Stub nutzte fiktive Felder
      `iceConnected`/`relayConnected`/`conn.mu`. Reale Felder sind
      `conn.statusICE *worker.AtomicWorkerStatus` und
      `conn.statusRelay *worker.AtomicWorkerStatus` mit atomarer `.Get()`-API.
      Kein Lock nötig (atomic loads). Section 5.2 Stufe 5 + Section 7.2 Q5
      angepasst.
    * SHOULD-FIX 2 — v0.2-Reste entfernt: `panicCount > 0` als
      Watchdog-Trigger aus R1, Tests + offenen Fragen; Single-Peer-Map-Frage
      (alte Q5 in v0.3) wurde durch v0.3 HA-Batch-Fix obsolet, gestrichen.
    * Branch-Basis (v0.3 Q4 offen): `phase3.7i-runtime-bugfixes-v0.5` als
      Base für Stufen 0/2/3/4/5 explizit gewählt (Codex-Empfehlung — enthält
      NewManagerWithTwoTimers, expectedWatcher, HA-Defer, Activity-AttachICE).
    * 1 neuer Unit-Test:
      `TestRecoverStuckPeer_PeerConnIdleHangs_ListenerStillArmed`.
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

### 4.2 Konkrete Drop-Stelle (Robustheits-Mangel, nicht alleinige Ursache)

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

**Verifizierter Code-Befund**: Beide Notify-Channels haben Buffer 1:

```go
// inactivity/manager.go (Constructor):
iceInactiveChan:   make(chan map[string]struct{}, 1),
inactivePeersChan: make(chan map[string]struct{}, 1),
```

**Was Buffer 1 + default-drop alleine erklärt — und was nicht** (Codex-Korrektur v0.2):

- Eine **kurze** Consumer-Pause (z.B. weil der case gerade in `onPeerInactivityTimedOut`
  arbeitet und `PeerConnIdle` läuft, das laut Code-Kommentar auf manager.go:656
  „blocking operation, potentially can be optimized" ist) droppt 1 Event. Beim
  **nächsten Tick** wird wieder gesendet, sobald der Channel leer ist. → Diese
  Drops sind **Robustheits-Problem** (verlorene Telemetrie + verlorene
  State-Transition-Trigger), aber führen nicht alleine zu 33h-Stuck.
- Damit der **Stuck-State persistent** ist, muss zusätzlich gelten: der Consumer
  ist **dauerhaft tot oder blockiert**, oder die `expectedWatcher`-State-Machine
  ist in eine Konfiguration geraten in der zukünftige Idle-Events strukturell
  nicht mehr Action triggern (siehe 4.3).

→ Section 4.2 alleine **rechtfertigt nicht** die ganze Fix-Architektur. notifyChan-
Drop-Visibility ist Stufe 1 (sinnvoll als eigenständiger Fix), aber Stufe 2 muss
die persistente Stuck-Ursache angreifen.

Symptom-Sicht von außen:
- `inactivity/manager.go:checkStats` läuft korrekt, logged „peer relay idle since: T"
- Aber bei stuck-Consumer landet nichts mehr in der State-Transition
- Lazy-Manager kriegt nichts mit → kein State-Heal über Stunden bis Tage

Aus außen sieht das aus wie:
- `inactivity/manager.go:checkStats` läuft korrekt
- Logged „peer relay idle since: T" jeden Tick
- Schickt aber nichts mehr durch
- Lazy-Manager kriegt nichts mit → kein State-Heal

### 4.3 Warum bleibt der Consumer hängen?

**Verworfene Hypothese (v0.3 Codex round-2)**: „Consumer-goroutine tot durch
unbehandelten Panic" ist technisch falsch. Go's runtime crashed bei einem
unrecovered Panic in jeder Goroutine den **gesamten Prozess** (per `runtime.gopanic`).
`lazyconn.Manager.Start()` hat zwar kein `defer recover()`, aber wenn dort ein
Panic auftreten würde, wäre der NetBird-Daemon weg — nicht stundenlang stale am
Laufen. Die User-Beobachtung „NetBird-App zeigt Mgmt+Signal connected, idle-logs
laufen seit 24h" widerlegt die Panic-Hypothese empirisch.

→ Panic-Recovery bleibt als **reines Hardening** in Stufe 0 (verhindert zukünftige
Daemon-Crashes bei neuen Bugs), aber NICHT als Erklärung für den beobachteten
Stuck-State.

**Verbleibende plausible Hypothesen**:

1. **`onPeerInactivityTimedOut` aktiv blockierend** (am wahrscheinlichsten v0.3):
   Verifiziert per Code-Pfad: `manager.go:656 PeerConnIdle` ruft
   `store.go:137 p.Close(true, true)` auf, das wiederum `peer/conn.go:311 Close()`
   ist mit `defer conn.wgWatcherWg.Wait()`. Wenn die wg-watcher-goroutine
   ihrerseits in einem Network-Read hängt (z.B. UDP-Read im Userspace-WG auf
   Android unter Doze, oder Wireguard-Kernel-Tunnel-Removal-Timeout), wartet
   `Close()` indefinitely. **Die ganze Consumer-Schleife steht still**, neue
   Idle-Events auf `inactivePeersChan` werden gedroppt (Buffer 1), kein Recovery.
   Symptom = exakt was wir auf S21 gesehen haben: idle-logs laufen, aber
   `ICE Checking` wird nie aufgerufen.

2. **State-Machine-Falle ohne externe Blockade**: theoretisch denkbar dass eine
   Sequenz von Events `expectedWatcher` in einem Zustand belässt aus dem keine
   weiteren Transitions möglich sind. Aktuell habe ich kein konkretes Szenario
   identifizieren können wo das passiert ohne externe Blockade — eher Theorie.

3. **Goroutine-Schedule-Pause unter Doze**: die ganze Consumer-Goroutine könnte
   vom Android-Scheduler unter Doze pausiert und nie wieder wachgeküsst werden.
   Bei manchen `select`-Implementierungen kann das passieren wenn die Channels
   selbst nicht wachgeküsst werden. Weniger plausibel weil andere Goroutines
   (inactivity.Manager.checkStats) weiterlaufen — sie würden gleich pausiert
   sein.

**Welche Hypothese auch zutrifft**: das Symptom ist identisch — keine Events kommen
durch, kein State-Heal. → Der Watchdog-Fix in Stufe 2 muss **außerhalb der
Consumer-Goroutine** laufen und einen Recovery-Pfad triggern der **nicht durch
dieselbe Blockade getroffen ist**. Konkret bedeutet das: Recovery-Aktionen
(`PeerConnIdle`, `MonitorPeerActivity`) **nicht im Watchdog-Tick synchron** ausführen,
sondern in separater (bounded) Goroutine pro Peer.

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

### 5.2 Vier-Stufen-Patch (v0.2: neue Stufe 0 für Consumer-Liveness)

#### Stufe 0: Panic-Recovery für Manager.Start (reines Hardening, v0.3)
**Datei**: `client/internal/lazyconn/manager/manager.go` (line 173 `Start`)
**Scope**: ~20 LOC
**Begründung v0.3**: **NICHT** als Stuck-Fix (Codex round-2 BLOCKER 1: Goroutine-Panic
crashed Prozess komplett, kann den 33h-Stuck nicht verursacht haben). Sondern als
**reines Defensive-Hardening**: das aktuelle `Manager.Start()` ohne `defer recover()`
ist eine latente Crash-Quelle. Eine zukünftige Bug-Einführung (nil-deref bei
concurrent map-write, race condition in `MonitorPeerActivity`, etc.) würde aktuell
den gesamten NetBird-Daemon mitreißen. Stufe 0 kapselt das ab so dass solche Bugs
geloggt + recovered werden statt den Daemon zu killen.

Konkret (Pseudocode, real-API):
```go
// current real signature (manager.go:173):
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
            m.safeOnPeerActivity(peerConnID)            // ← wraps panic
        case peerIDs := <-m.inactivityManager.InactivePeersChan():
            m.safeOnPeerInactivityTimedOut(peerIDs)      // ← wraps panic
        }
    }
}

func (m *Manager) safeOnPeerActivity(peerConnID peerid.ConnID) {
    defer func() {
        if r := recover(); r != nil {
            stack := debug.Stack()
            log.Errorf("lazyconn/manager: panic in onPeerActivity (peer=%v): %v\nstack:\n%s",
                peerConnID, r, stack)
            // Continue with next event — panic was within one handler call,
            // not a state-corruption that warrants killing the loop
        }
    }()
    m.onPeerActivity(peerConnID)
}

// analog safeOnPeerInactivityTimedOut(peerIDs map[string]struct{})
```

**Wichtig (v0.3 Korrektur)**: Kein atomic counter für Watchdog-Signal mehr — der
Watchdog kann den Stuck-State nicht aus Panic-Count ableiten weil bei aktuellem
Verhalten der Daemon bei Panic crashed (Counter würde nicht sinnvoll überleben).

Tests:
- `TestManagerStart_PanicInOnPeerActivity_DoesNotKillConsumer`: injiziere eine
  Bedingung die zu panic in `onPeerActivity` führt (z.B. nil-deref via test-hook),
  assert dass `Start()` weiterläuft und folgende Events noch verarbeitet werden.
- `TestManagerStart_PanicInOnPeerInactivity_DoesNotKillConsumer`: analog für
  inactivity-Pfad.

#### Stufe 1: notifyChan-Drops sichtbar machen
**Datei**: `client/internal/lazyconn/inactivity/manager.go` (line 202)
**Scope**: ~15 LOC + 2 atomic counter pro Manager-Instanz

Die `kind`-Information ist über das Channel-Reference identifizierbar — vereinfacht
die API:

```go
type Manager struct {
    // ... existing fields ...
    notifyDropsRelay atomic.Uint64
    notifyDropsICE   atomic.Uint64
}

func (m *Manager) notifyChan(ctx context.Context, ch chan map[string]struct{}, peers map[string]struct{}) {
    select {
    case ch <- peers:
    case <-ctx.Done():
        return
    default:
        var n uint64
        // identify which counter via channel pointer
        switch ch {
        case m.inactivePeersChan:
            n = m.notifyDropsRelay.Add(1)
        case m.iceInactiveChan:
            n = m.notifyDropsICE.Add(1)
        }
        // Throttle: log on 1st, 10th, 100th, then every 100 drops
        if n == 1 || n == 10 || (n >= 100 && n%100 == 0) {
            log.Warnf("inactivity: notify channel full, dropped %d-th event (peers in batch=%d). " +
                      "Consumer may be slow or stuck — see lazyconn/manager.go state.",
                      n, len(peers))
        }
        return
    }
}

// NEW exported method for watchdog inspection (no PII leaked):
func (m *Manager) DropCounters() (relayDrops, iceDrops uint64) {
    return m.notifyDropsRelay.Load(), m.notifyDropsICE.Load()
}
```

**Wichtig**: Stufe 1 ist **upstream-fähig als eigenständiger PR** auf `upstream/main`,
weil sie keine Phase-3.7i-spezifischen Code-Pfade berührt. Nur Robustheits-Patch
am bestehenden `notifyChan`.

Tests:
- Bestehende Tests in `inactivity/manager_test.go` bleiben unverändert (API-
  Signatur unverändert).
- Neuer Test `TestNotifyChan_FullChannelIncrementsDropCounter`: gefüllter Channel,
  weitere Notification, prüfe `DropCounters()` und Warn-Log via test-hook auf
  logger.

#### Stufe 2: Reconcile-Watchdog mit Phasen-strikter Lock-Trennung (v0.3 streng neu)
**Datei**: `client/internal/lazyconn/manager/manager.go` (neue Methode am `Manager`)
**Scope**: ~180 LOC + 1 Goroutine + 1 Ticker
**Codex-Korrektur v0.3 (BLOCKER 2)**: Vier-Phasen-Architektur mit klar getrennten
Lock-Lebensdauern. Blocking calls (`PeerConnIdle` chain die zu `Conn.Close()` und
dann `wgWatcherWg.Wait()` führt) laufen NIE unter `managedPeersMu`. Recovery wird
in bounded async goroutines pro Peer ausgeführt, sodass ein blockierender Peer
weder den Watchdog-Tick noch andere Peer-Recoveries blockiert.

Verfügbare reale APIs (verifiziert v0.3):
- `m.managedPeersByConnID` (`map[ConnID]*managedPeer`, geschützt durch
  `managedPeersMu`)
- `m.inactivityManager.DropCounters()` (neu in Stufe 1)
- `m.peerStore.PeerConn(pubKey) -> *peer.Conn, bool`
- **`conn.TransportSnapshot()`** (neu in Stufe 5) — read-only, no logging,
  replaces nicht-existente `GetICEState`/`GetRelayState`

Panic-Count aus Stufe 0 wird NICHT als Watchdog-Signal verwendet (v0.3
Korrektur: Panic crashed den Prozess, Counter würde nicht sinnvoll überleben).

**Vier-Phasen-Strategie**:

```go
func (m *Manager) runReconcileWatchdog(ctx context.Context) {
    defer func() {
        if r := recover(); r != nil {
            log.Errorf("lazyconn watchdog: panic, restart loop: %v", r)
            go m.runReconcileWatchdog(ctx)   // self-restart
        }
    }()

    ticker := time.NewTicker(defaultReconcileInterval)  // 120s
    defer ticker.Stop()

    var lastRelayDrops, lastICEDrops uint64
    // recoveringPeers prevents double-spawning recovery for same peer.
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

func (m *Manager) reconcileTick(ctx context.Context, lastRelayDrops, lastICEDrops *uint64,
    recoveringPeers map[string]struct{}, recoveringMu *sync.Mutex) {

    // PHASE A: atomic counter read (no lock)
    relayDrops, iceDrops := m.inactivityManager.DropCounters()
    deltaRelay := relayDrops - *lastRelayDrops
    deltaICE := iceDrops - *lastICEDrops
    *lastRelayDrops, *lastICEDrops = relayDrops, iceDrops

    // PHASE B: short snapshot under managedPeersMu (no blocking calls here!)
    m.managedPeersMu.Lock()
    candidates := make([]string, 0)
    for _, mp := range m.managedPeersByConnID {
        if mp.expectedWatcher != watcherInactivity { continue }
        candidates = append(candidates, mp.peerCfg.PublicKey)
    }
    m.managedPeersMu.Unlock()

    // PHASE C: per-candidate transport-state read, no lock held
    stuckBatch := make(map[string]struct{})
    for _, pubKey := range candidates {
        conn, ok := m.peerStore.PeerConn(pubKey)
        if !ok { continue }
        iceDisc, relayDisc := conn.TransportSnapshot()  // Stufe 5 API
        if isStuckPeer(iceDisc, relayDisc, deltaRelay, deltaICE) {
            stuckBatch[pubKey] = struct{}{}
        }
    }
    if len(stuckBatch) == 0 { return }

    log.Warnf("lazyconn watchdog: %d stuck peers (deltaRelayDrops=%d, deltaICEDrops=%d) — spawning recovery",
        len(stuckBatch), deltaRelay, deltaICE)

    // PHASE D: spawn bounded async recovery per peer
    // The full stuckBatch is passed to each recovery goroutine so the
    // HA-defer check in recoverStuckPeer sees the correct batch state.
    for pubKey := range stuckBatch {
        recoveringMu.Lock()
        if _, inflight := recoveringPeers[pubKey]; inflight {
            recoveringMu.Unlock()
            continue   // skip — recovery already running
        }
        recoveringPeers[pubKey] = struct{}{}
        recoveringMu.Unlock()

        go func(pk string, batch map[string]struct{}) {
            defer func() {
                recoveringMu.Lock()
                delete(recoveringPeers, pk)
                recoveringMu.Unlock()
                if r := recover(); r != nil {
                    log.Errorf("lazyconn watchdog: recovery panic for %s: %v", pk, r)
                }
            }()
            m.recoverStuckPeer(ctx, pk, batch)
        }(pubKey, stuckBatch)
    }
}

func isStuckPeer(iceDisc, relayDisc bool, deltaRelay, deltaICE uint64) bool {
    // Both transports disconnected AND notifyChan is actively dropping events.
    // Drops alone do not trigger; disconnection alone does not trigger.
    return iceDisc && relayDisc && (deltaRelay > 0 || deltaICE > 0)
}
```

**Lock-Garantien (Codex v0.3)**:
- `managedPeersMu` wird nur in Phase B gehalten (reine Map-Iteration, kein I/O).
- Phase C läuft ohne Lock; `TransportSnapshot` nimmt seinen eigenen short-lived
  Conn-internen Mutex.
- Phase D spawnt goroutines die ihre eigenen Locks managen.
- **Watchdog-Tick blockiert NIE auf** `PeerConnIdle`/`Close()`/`wgWatcherWg.Wait()`.

Tests:
- `TestWatchdog_HoldsLockOnlyForSnapshot`: instrumentiere `TransportSnapshot` mit
  Sleep, assert dass `managedPeersMu` während Sleep nicht gehalten.
- `TestWatchdog_RecoveryRunsInOwnGoroutine`: blockierender `PeerConnIdle` darf
  nächsten Tick nicht verzögern.
- `TestWatchdog_DropCounterAloneIsNotTrigger`: drops > 0, aber Conn ist healthy
  (iceDisc=false) → keine Heilung.
- `TestWatchdog_DisconnectAloneIsNotTrigger`: Conn disconnected, aber keine drops
  → keine Heilung (regelmäßiger Pfad soll greifen).
- `TestWatchdog_BatchedHACheckUsedNotSinglePeer`: assert dass `recoverStuckPeer`
  mit dem vollen `stuckBatch` aufgerufen wird, nicht mit Single-Peer-Map.
- `TestWatchdog_InflightDedupePreventsDoubleSpawn`: zwei Ticks mit demselben
  stuck peer in flight → nur 1 Recovery-Goroutine.
- `TestWatchdog_PanicSelfRestart`: injiziere panic in reconcileTick, assert dass
  ein neuer runReconcileWatchdog-Loop läuft.

#### Stufe 3: Recovery-Helper mit Lock-Disziplin + Listener-First-Reihenfolge (v0.4)
**Datei**: `client/internal/lazyconn/manager/manager.go` (Refaktor + neue Methode)
**Scope**: ~50 LOC Refaktor + ~70 LOC neue Methode

**Codex-Korrekturen v0.4 (BLOCKER round-3)**:
- **Recovery-Reihenfolge**: v0.3 rief im `IOAfterUnlock`-Pfad zuerst
  `PeerConnIdle` (blocking) und erst dann `MonitorPeerActivity`. Wenn
  `PeerConnIdle` blockiert (die vermutete Stuck-Ursache!), wird der
  Activity-Listener nie armiert. Der Peer landet in `watcherActivity`-State
  ohne Listener, und `recoveringPeers` blockiert künftige Recovery-Versuche.
  **v0.4-Fix**: Listener ZUERST armieren, Close DANACH als async best-effort
  goroutine. Damit kann der nächste WG-Wake auch bei hängendem Close
  funktionieren.

**Codex-Korrekturen v0.3 (übernommen)**:
1. **HA-Batch**: `shouldDeferIdleForHA` mit Single-Peer-Map deferred ad infinitum
   (alle anderen HA-Member werden fälschlich als „aktiv" interpretiert).
   `recoverStuckPeer` muss den vollen `stuckBatch` durchreichen.
2. **Lock-Disziplin**: `PeerConnIdle` ist blocking (chain to `Conn.Close()` mit
   `wgWatcherWg.Wait()`). Darf NICHT unter `managedPeersMu` laufen.

Refaktor splittet die existierende `transitionToActivityWatcher`-Logik in drei
Hälften (state-only / listener / close-best-effort):

```go
// transitionToActivityWatcherStateOnly performs the non-blocking state-machine
// part of the transition (expectedWatcher flip + RemovePeer from inactivity).
// Caller MUST hold m.managedPeersMu. No I/O here.
func (m *Manager) transitionToActivityWatcherStateOnly(mp *managedPeer) {
    mp.peerCfg.Log.Infof("transition to watcherActivity (state-only) from %s", mp.expectedWatcher)
    mp.expectedWatcher = watcherActivity
    m.inactivityManager.RemovePeer(mp.peerCfg.PublicKey)
}

// armActivityListener installs the activity monitor for a peer. This must run
// BEFORE the blocking close (PeerConnIdle), so that even if the close hangs,
// the next inbound WG traffic can still wake the peer back into the activity
// watcher path. No lock required (activityManager has its own internal mutex).
func (m *Manager) armActivityListener(mp *managedPeer) {
    if err := m.activityManager.MonitorPeerActivity(*mp.peerCfg); err != nil {
        mp.peerCfg.Log.Errorf("failed to create activity monitor: %v", err)
    }
}

// closePeerConnBestEffort runs the (potentially blocking) PeerConnIdle close
// in its own goroutine. The watchdog's recovery completion is independent of
// the close finishing — we only need the activity listener armed. If the close
// hangs forever (the stuck-state symptom we are healing!), that goroutine
// leaks but the peer is now fully reachable via the new listener.
// No lock held by caller.
func (m *Manager) closePeerConnBestEffort(mp *managedPeer) {
    go func() {
        defer func() {
            if r := recover(); r != nil {
                mp.peerCfg.Log.Errorf("PeerConnIdle panic: %v", r)
            }
        }()
        m.peerStore.PeerConnIdle(mp.peerCfg.PublicKey)
    }()
}

// recoverStuckPeer is the watchdog's recovery entry point. Runs in its own
// goroutine (spawned by reconcileTick). Takes the FULL stuckBatch for
// correct HA-defer semantics. Listener-first ordering: state-flip + listener
// arm under/right after lock release, close fires-and-forgets in its own
// goroutine.
func (m *Manager) recoverStuckPeer(ctx context.Context, pubKey string, stuckBatch map[string]struct{}) {
    // Short lock for re-validation + state mutation
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
    // Re-validate — onPeerInactivityTimedOut may have beat us
    if mp.expectedWatcher != watcherInactivity {
        m.managedPeersMu.Unlock()
        return
    }
    // HA-defer with full batch (v0.3 critical correction; v0.2 used single-peer
    // map which made all other HA peers look "active" and deferred forever)
    if m.shouldDeferIdleForHA(stuckBatch, mp.peerCfg.PublicKey) {
        mp.peerCfg.Log.Infof("watchdog: defer recovery (HA peers active, batch=%d)", len(stuckBatch))
        m.managedPeersMu.Unlock()
        return
    }
    m.transitionToActivityWatcherStateOnly(mp)
    m.managedPeersMu.Unlock()
    // Lock released. mp pointer is safe: managedPeer struct is only removed
    // via RemovePeer/ExcludePeer paths that both lock managedPeersMu.

    // v0.4 listener-first ordering — arm BEFORE the (potentially blocking)
    // close. If we did this in the opposite order and PeerConnIdle hung
    // forever (the very symptom we are healing!), the peer would land in
    // watcherActivity without a listener and would never wake up again.
    m.armActivityListener(mp)
    m.closePeerConnBestEffort(mp)
    mp.peerCfg.Log.Infof("watchdog: recovery complete (watcherInactivity -> watcherActivity, listener armed, close best-effort)")
}
```

**Refaktor von `onPeerInactivityTimedOut`** (line 629) — Three-phase pattern,
listener-first identical to the watchdog recovery path:

```go
func (m *Manager) onPeerInactivityTimedOut(peerIDs map[string]struct{}) {
    // Phase 1: short lock — state mutations only
    m.managedPeersMu.Lock()
    toTransition := make([]*managedPeer, 0, len(peerIDs))
    for peerID := range peerIDs {
        peerCfg, ok := m.managedPeers[peerID]
        if !ok { continue }
        mp, ok := m.managedPeersByConnID[peerCfg.PeerConnID]
        if !ok { continue }
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

    // Phase 2: arm activity listener first — survives a hanging close.
    for _, mp := range toTransition {
        m.armActivityListener(mp)
    }
    // Phase 3: close existing conn best-effort. Each peer's close runs in its
    // own goroutine via closePeerConnBestEffort; the outer call returns once
    // all goroutines are spawned.
    for _, mp := range toTransition {
        m.closePeerConnBestEffort(mp)
    }
}
```

**Behavior-Change-Warnung**: das ist ein **echter Behavior-Change** an
`onPeerInactivityTimedOut`. Vorher lief `PeerConnIdle` synchron unter Lock
(per TODO `potentially can be optimized`); jetzt async außerhalb Lock und nach
Listener-Arm. Das ist eine **gewünschte Verbesserung**, aber muss in Tests sehr
genau abgedeckt werden um Regressionen zu verhindern. Insbesondere: ein
gleichzeitig feuernder Activity-Event (durch den neu armierten Listener) der
auf eine teilweise-geclosete `*peer.Conn` zugreift muss von der existierenden
`onPeerActivity`-Validierung (Re-Check via `expectedWatcher == watcherActivity`
und Conn-Lookup in peerStore) abgefangen werden.

Tests:
- `TestRecoverStuckPeer_HappyPath`: State-Mutation läuft unter Lock, dann
  Listener armiert, dann Close-Goroutine gespawnt.
- `TestRecoverStuckPeer_AlreadyActivity_NoOp`: peer schon in watcherActivity,
  Idempotenz.
- `TestRecoverStuckPeer_RespectsHA_FullBatch`: HA-defer mit Batch.
- `TestRecoverStuckPeer_RespectsHA_SinglePeerBatch_RegressionGuard`: explicit
  Test für den v0.2-Bug — wenn andere HA-Member aktiv sind und der stuckBatch
  nur diesen einen Peer enthält, soll RICHTIG deferred werden. Wenn der
  Watchdog jedoch korrekt alle stuck-Peers im batch sammelt, soll NICHT
  deferred werden.
- `TestRecoverStuckPeer_PeerConnIdleOutsideLock`: instrument PeerConnIdle mit
  Sleep + assert dass `managedPeersMu` während Sleep nicht gehalten.
- **`TestRecoverStuckPeer_PeerConnIdleHangs_ListenerStillArmed`** (Codex v0.4
  BLOCKER): instrument `peerStore.PeerConnIdle` so dass es niemals returned;
  assert dass (a) `activityManager.MonitorPeerActivity` aufgerufen wurde,
  (b) `mp.expectedWatcher == watcherActivity`, (c) `recoverStuckPeer` returned
  obwohl close noch hängt, (d) ein nachträglicher Activity-Event den Peer
  erfolgreich wakeable macht.
- `TestOnPeerInactivityTimedOut_AfterRefactor_HappyPath`: existierende
  Pfad-Semantik bleibt erhalten.
- `TestOnPeerInactivityTimedOut_AfterRefactor_ListenerArmedBeforeClose`:
  assert dass der refactored Pfad denselben listener-first-Ordering hat wie
  recoverStuckPeer.
- `TestOnPeerInactivityTimedOut_AfterRefactor_IOOutsideLock`: assert dass das
  refactored `onPeerInactivityTimedOut` das blocking I/O nach unlock geschoben hat.

#### Stufe 4: Wiring
**Datei**: `client/internal/lazyconn/manager/manager.go` (Start-Methode line 173)
**Scope**: 1 LOC

Watchdog-Goroutine wird in `Start()` gestartet:
```go
go m.runReconcileWatchdog(ctx)
```
Lebenszyklus identisch zum Start-Loop. Kein separater Knopf, kein Config-Toggle.

#### Stufe 5: Read-only Transport-Snapshot API auf peer.Conn (v0.4 reale Felder)
**Datei**: `client/internal/peer/conn.go` (neue exported Methode)
**Scope**: ~20 LOC

**Codex-Korrektur v0.4 (SHOULD-FIX 1)**: der v0.3-Stub nutzte fiktive Felder
`iceConnected`/`relayConnected` und einen Lock `conn.mu`. Reale Implementation
in `peer/conn.go` (line 125-140 verifiziert v0.4):
- `statusRelay *worker.AtomicWorkerStatus`
- `statusICE   *worker.AtomicWorkerStatus`
- `AtomicWorkerStatus.Get() Status` (worker/state.go:40) lädt atomar via
  `Status(acs.status.Load())`, kein Lock nötig
- Enum: `StatusDisconnected Status = iota`, `StatusConnected`, … (worker/state.go:10)

**v0.3-Befund (übernommen)**: `conn.GetICEState()` / `conn.GetRelayState()`
existieren nicht. `isConnectedOnAllWay()` (conn.go:1027) ist private und ruft
`logTraceConnState()` als Side-Effect bei disconnected (Log-Storm wenn Watchdog
das alle 120s aufruft, mal 32 peers). Watchdog braucht eine saubere read-only
API ohne Side-Effects.

Neue exported API:
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

**Lock-Garantien**: keine. `AtomicWorkerStatus.status` ist ein
`atomic.Int32` (siehe worker/state.go:7-15) und wird ausschließlich via
`Set()`/`Get()` atomic-load/store gelesen/geschrieben. Concurrent
`OnICEConnected`/`OnRelayDisconnected`-Handler greifen über dieselben
atomar-typed Methoden zu.

Tests:
- `TestTransportSnapshot_BothConnected`: setze beide AtomicWorkerStatus auf
  `StatusConnected`, assert `(false, false)`.
- `TestTransportSnapshot_BothDisconnected`: beide auf `StatusDisconnected`,
  assert `(true, true)`.
- `TestTransportSnapshot_RelayOnly`: ICE=Disconnected, Relay=Connected →
  `(true, false)`.
- `TestTransportSnapshot_NoLogging`: via test-hook auf logger assert dass keine
  log-Aufrufe stattfinden.
- `TestTransportSnapshot_RaceSafe`: `go test -race` mit konkurrenten
  `statusICE.Set()` / `statusRelay.Set()` Mutations.

### 5.3 Was NICHT geändert wird (explizit)

- `peer/handshaker.go:224` (Serial-Generation) — bleibt unverändert.
- `peer/conn.go:831` (Guard SendOffer) — bleibt unverändert.
- `peer/worker_ice.go` (Reset-Logik) — bleibt unverändert.
- Keine Android-spezifischen Code-Pfade.
- Keine Plan-2-mgmt-stream-watchdog-Erweiterungen.

## 6. Test-Plan

### 6.1 Unit-Tests (in derselben PR — v0.2 erweitert)

**Für Stufe 0 (Panic-Recovery)**:
1. `TestManagerStart_PanicInOnPeerActivity_DoesNotKillConsumer`: injiziere panic
   im onPeerActivity-Pfad, assert dass Start() weiterläuft + Counter increment.
2. `TestManagerStart_PanicInOnPeerInactivityTimedOut_DoesNotKillConsumer`: analog
   für inactivity-Pfad.
3. `TestConsumerPanicCount_Accessible`: counter via öffentliche API lesbar.

**Für Stufe 1 (notifyChan visibility)**:
4. `TestNotifyChan_FullChannelIncrementsDropCounter`: voller Channel, weitere
   Notification, prüfe `DropCounters()` returnt 1.
5. `TestNotifyChan_DropLogThrottled`: 100 drops → max 3 Log-Lines (1st, 10th, 100th).
6. `TestDropCounters_RelayAndICESeparate`: drops auf inactivePeersChan zählen nur
   relayDrops, drops auf iceInactiveChan zählen nur iceDrops.

**Für Stufe 2 (Reconcile-Watchdog)**:
7. `TestReconcileWatchdog_DetectsStuckPeer`: drop-delta > 0 + Peer beide-
   Transports disconnected (per TransportSnapshot) → recoverStuckPeer wird
   aufgerufen (per v0.4 ist `panicCount` NICHT mehr Watchdog-Signal — Panic
   crasht den Prozess und überlebt keinen Stuck-State).
8. `TestReconcileWatchdog_IgnoresHealthyPeer`: peer mit recent state-transition
   wird NICHT geheilt.
9. **`TestReconcileWatchdog_DropCounterAloneIsNotTrigger`** (Codex R5 v0.2):
   drop-Counter steigt, aber Peer-Conn ist healthy → keine Heilung. Beweis dass
   Drop-Counter NUR Sichtbarkeit ist, nicht Recovery-Trigger.
10. **`TestReconcileWatchdog_HoldsLockOnlyForSnapshot`** (Codex R4/R6 v0.2):
    instrumentiere `peerStore.PeerConn` mit Sleep, assert dass
    `managedPeersMu` während des Sleep **nicht** gehalten ist (kann andere
    goroutine inzwischen `AddPeer` machen).
11. **`TestReconcileWatchdog_RecoveryActionRunsOutsideTickLoop`** (Codex R6 v0.2):
    assert dass blocking `PeerConnIdle` während Recovery den nächsten Watchdog-
    Tick nicht blockiert.
12. **`TestReconcileWatchdog_PanicSelfHealing`** (Codex R7 v0.2): injiziere panic
    in watchdog tick handler, assert dass Watchdog-Loop weiterläuft + nächster
    Tick noch funktioniert.
13. `TestReconcileWatchdog_RespectsHA`: shouldDeferIdleForHA-Logik auch im
    Watchdog-Pfad aktiv.
14. `TestReconcileWatchdog_StartStop`: Watchdog-Goroutine startet mit
    `Start(ctx)` und stoppt bei `ctx.Done()`.

**Für Stufe 3 (Refaktor + Helper)**:
15. `TestTransitionToActivityWatcherStateOnly_HappyPath`: zentral getestet.
16. `TestRecoverStuckPeer_HappyPath`: peer in watcherInactivity →
    recoverStuckPeer → watcherActivity + Listener armiert + Close-Goroutine
    gespawnt.
17. `TestRecoverStuckPeer_AlreadyActivity_NoOp`: peer schon in watcherActivity,
    Idempotenz.
18. `TestRecoverStuckPeer_RespectsHA_FullBatch`: HA-defer im Recovery-Pfad mit
    vollem Batch.
19. **`TestRecoverStuckPeer_PeerConnIdleHangs_ListenerStillArmed`** (Codex v0.4
    BLOCKER-Fix): instrument `peerStore.PeerConnIdle` so dass es niemals
    returned; assert (a) `armActivityListener` wurde aufgerufen,
    (b) `mp.expectedWatcher == watcherActivity`, (c) `recoverStuckPeer`
    selbst returned trotz hängendem Close, (d) ein nachträglicher
    Activity-Event über den neu armierten Listener wakeable.
20. `TestOnPeerInactivityTimedOut_AfterRefactor_HappyPath`: existierender
    Test-Sweep nach Refaktor unverändert grün.
21. `TestOnPeerInactivityTimedOut_AfterRefactor_ListenerArmedBeforeClose`:
    assert dass der refactored Pfad denselben listener-first-Ordering hat
    wie `recoverStuckPeer`.

**Für Stufe 5 (TransportSnapshot)**:
22. `TestTransportSnapshot_BothConnected`: `statusICE=StatusConnected`,
    `statusRelay=StatusConnected` → `(false, false)`.
23. `TestTransportSnapshot_BothDisconnected`: beide auf `StatusDisconnected` →
    `(true, true)`.
24. `TestTransportSnapshot_RelayOnly`: ICE disconnected, Relay connected →
    `(true, false)`.
25. `TestTransportSnapshot_NoLogging`: assert keine log-Aufrufe.
26. `TestTransportSnapshot_RaceSafe`: `go test -race` mit konkurrenten
    `statusICE.Set()`/`statusRelay.Set()`.

Alle laufen mit `go test -race`. Bestehende Tests in
`client/internal/lazyconn/inactivity/manager_test.go` und
`client/internal/lazyconn/manager/manager_test.go` müssen ohne Regression
überleben.

### 6.2 Integration-Tests (in derselben PR)
- **`TestIntegration_StuckConsumerRecovery`**: E2E-Test der einen stuck-state
  künstlich erzeugt (Consumer-Goroutine via `sync.WaitGroup` blockieren) und
  prüft dass innerhalb von 2 Watchdog-Ticks der Peer in `watcherActivity` ist.
- **`TestIntegration_PanicInConsumer_WatchdogHeals`**: injiziere panic im
  onPeerInactivityTimedOut, prüfe dass nach Stufe-0-Recovery + Watchdog-Tick
  der Peer in watcherActivity ist (full-loop verification).

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
| R1 | Watchdog false-positive: gesunder Peer wird gestört | Mittel | Multi-Faktor-Heuristik in `isStuckPeer` (`iceDisc && relayDisc && (deltaRelay > 0 \|\| deltaICE > 0)` — beide Transports disconnected UND notifyChan dropt aktiv) + `shouldDeferIdleForHA`-Check + Re-Check `expectedWatcher == watcherInactivity` nach Lock-Reacquire |
| R2 | Race zwischen `onPeerInactivityTimedOut` und Watchdog-Reconcile | Mittel | Beide nutzen `managedPeersMu`; Reconcile prüft `expectedWatcher == watcherInactivity` redundant nach Lock-Reacquire; `transitionToActivityWatcherLocked` shared zwischen beiden Pfaden |
| R3 | Watchdog stört intentionale LazyTimeout-Konfig (z.B. Admin setzt RelayTimeout=24h für long-idle peers) | Niedrig | Trigger basiert auf `dropDelta > 0` (Indikator dass Events verloren gehen) + Disconnected-State, NICHT auf reiner Idle-Zeit. Admin-Config wird respektiert |
| R4 | `PeerConnIdle` chain → `Conn.Close()` → `wgWatcherWg.Wait()` ist hart blocking; lief in v0.2-Stufe 3 unter `managedPeersMu` | **BLOCKER (Codex v0.3)** | **v0.3-Mitigation**: Split in `transitionToActivityWatcherStateOnly` (unter Lock) + `transitionToActivityWatcherIOAfterUnlock` (außerhalb Lock). Beide Recovery-Pfade (`onPeerInactivityTimedOut` + `recoverStuckPeer`) folgen diesem Pattern. Watchdog spawnt zusätzlich bounded async goroutines pro Peer → ein blockierender Peer blockiert weder Watchdog-Tick noch andere Recoveries. |
| R5 | notifyChan-Drop-Counter wächst monoton → keine echte Heilung | Niedrig | Counter ist Telemetrie, keine Korrektur-Logik; Heilung passiert via Watchdog. Test `TestReconcileWatchdog_DropCounterAloneIsNotTrigger` deckt diese Mitigation ab |
| **R6** | **Watchdog selbst hängt am selben Lock/blockierenden Pfad und heilt dadurch nichts** | **Hoch (Codex v0.2)** | Lock-sparsame Snapshot/Action-Trennung (siehe Stufe 2). Wenn `recoverStuckPeer` durch `PeerConnIdle`-Blockade dauerhaft hängt, würde der Watchdog-Loop dort stehen bleiben — **das wäre Total-Failure**. Mitigation Stufe 2: Watchdog-Recovery in eigener short-lived goroutine spawnen, sodass nächster Tick weitermacht auch bei Blockade |
| **R7** | **Panic im Watchdog-Loop killt sich selbst dauerhaft** (gleicher Bug wie für Start-Loop in Stufe 0) | **Mittel (Codex v0.2)** | v0.3: `runReconcileWatchdog` hat eigenes `defer recover()` mit `go m.runReconcileWatchdog(ctx)`-Self-Restart. Pro-Peer-Recovery-goroutines haben eigenes `defer recover()`. |
| **R8** | **HA-Defer mit Single-Peer-Map deferred ad infinitum** (Codex v0.3 BLOCKER): `shouldDeferIdleForHA` interpretiert „peer nicht im inactivePeers map" als „aktiv". Single-Peer-Map machte alle anderen HA-Member fälschlich aktiv | **BLOCKER (Codex v0.3)** | Watchdog sammelt vollen `stuckBatch` in Phase C/D und übergibt ihn an jede Recovery-goroutine. `shouldDeferIdleForHA` arbeitet wie beim regelmäßigen Pfad mit dem realen Batch der stuck-Peers. Test `TestRecoverStuckPeer_RespectsHA_SinglePeerBatch_RegressionGuard` deckt das ab. |
| **R9** | **`onPeerInactivityTimedOut`-Refaktor verändert blocking-call-Ordnung** (Side-Effect des Stufe-3-Refaktors): `PeerConnIdle` läuft jetzt nach `expectedWatcher=activity`-Flip, nicht davor. Kann subtle Race mit `onPeerActivity` ergeben wenn der activity-Listener bereits feuert bevor `PeerConnIdle` abgeschlossen ist | **Mittel (v0.3 neu)** | Test-Coverage `TestOnPeerInactivityTimedOut_AfterRefactor_*`. Auch: das ist die intendierte Semantik (state-machine zuerst, I/O nachher) und entspricht dem TODO „potentially can be optimized" auf manager.go:659. Race mit activity-Listener ist tolerabel weil der `onPeerActivity`-Pfad selber `expectedWatcher == watcherActivity` als Vorbedingung prüft. |
| **R10** | **Listener-armed-before-close lässt einen blockierenden `PeerConnIdle` als langlebige Leak-Goroutine zurück** (v0.4 BLOCKER-Fix Side-Effect): `closePeerConnBestEffort` spawnt eine goroutine die nie returned wenn `Conn.Close()` ewig hängt. Bei 32 stuck-Peers ergeben sich potentiell 32 leaked goroutines pro stuck-Cycle | **Mittel (Codex v0.4)** | Akzeptiert als Trade-Off — Alternative ist Peer ohne Listener. Mitigation Telemetrie: pro `closePeerConnBestEffort` ein atomic `pendingCloses`-Counter inc/dec, vom Watchdog im 120s-Tick mit-geloggt; Schwellwert (>10 pending closes über 10min) erzeugt Warn-Log und nimmt das in zukünftiges Debugging mit. **KEIN automatischer Kill der hängenden goroutine** — der Bug muss in Conn.Close()/wgWatcherWg.Wait() upstream gefixt werden. |
| **R11** | **`TransportSnapshot` liest atomare Status-Felder ohne Lock — Race mit ICE-Reconnect Möglichkeit** (v0.4 SHOULD-FIX 1 Side-Effect): Watchdog könnte einen Peer „disconnected" sehen während er gerade in `StatusConnecting`/`StatusReconnecting` ist, und unnötig recovern | **Niedrig (v0.4)** | `isStuckPeer` verlangt BEIDE Transports disconnected UND drop-delta > 0 — die Wahrscheinlichkeit dass ein Peer GLEICHZEITIG ICE+Relay-Reconnect macht UND notifyChan dropt ist sehr klein. False-positive-Cost ist eine zusätzliche Recovery-Goroutine, kein State-Corruption. Test `TestTransportSnapshot_RaceSafe` deckt mit `-race` ab. |

### 7.2 Offene Fragen für Codex (v0.4 — auf 4 verbleibende reduziert)

Folgende Fragen aus v0.3 sind durch v0.4-Änderungen **geschlossen**:
- ~~Q4 v0.3 (`RecoverPeerToIdle` vs. eigene Methode)~~ — entschieden: eigene
  `recoverStuckPeer` + Helper-Triple in derselben Datei (cross-package-Calls
  von `lazyconn/manager` nach `conn_mgr` würden Importzyklus erzeugen).
- ~~Q5 v0.3 (single-peer-map für HA-Defer)~~ — durch v0.3 HA-Batch-Fix obsolet.
- ~~Q5 v0.4 alt (Lock für TransportSnapshot)~~ — verifiziert: reale Felder sind
  `statusICE/statusRelay *worker.AtomicWorkerStatus`, atomic, kein Lock nötig.
- ~~Branch-Basis Q4 v0.3~~ — v0.4: `phase3.7i-runtime-bugfixes-v0.5` (siehe 8.1).

Verbleibend für Codex round-4:

1. **`isStuckPeer`-Symmetrie**: aktuell verlangt `iceDisc && relayDisc &&
   (deltaRelay > 0 || deltaICE > 0)`. Phase-3.7i p2p-dynamic kann Lazy-
   Activation NUR ICE machen (kein Relay-Watcher), Relay bleibt dann
   structurally „disconnected". Wenn Watchdog dieselbe Logik auf p2p-dynamic-
   only-Peers anwendet, schlägt das fortwährend an. Soll der Watchdog die
   ConnectionMode des Peers konsultieren und je nach Mode unterschiedlich
   triggern?

2. **Default-Tick-Intervall 120 s**: ist das die richtige Konstante? Soll
   das konfigurierbar sein (via Account-Setting `lazy_watchdog_interval_seconds`
   oder dediziertem Knopf), oder als Hartkodierung okay?

3. **Listener-armed-Leak-Mitigation (R10)**: aktuell akzeptiere ich, dass
   `closePeerConnBestEffort` bei hängendem `Conn.Close()` eine leaked
   goroutine hinterlässt, weil die Alternative (Peer ohne Listener) schlimmer
   ist. Soll der Watchdog zusätzlich pro-Peer `pendingCloses`-Counter führen
   und nach Schwellwert (z.B. >10 über 10 min) per Telemetrie warnen?
   Oder soll ich gleich versuchen den Conn.Close()-Hang upstream zu beheben
   (parallele Spec)?

4. **Test-Strategie für blocking-call-outside-lock**: gibt es eine etablierte
   Methode in NetBird-Tests einen `PeerConnIdle`-Call programmatisch zu
   verzögern (für `TestRecoverStuckPeer_PeerConnIdleOutsideLock` und
   `TestRecoverStuckPeer_PeerConnIdleHangs_ListenerStillArmed`)? Aktuell
   denke ich an einen test-only `Conn.idleHookForTest func()` der vor dem
   `Close` aufgerufen wird.

## 8. Rollout-Plan

### 8.1 Branch + Commit-Struktur (v0.4 — Base fixiert)

**Codex-Befund v0.2**: `upstream/main` hat den alten Lazy-Manager und 1-slot
`inactivePeersChan`, aber **nicht** die komplette Phase-3.7i-Zwei-Timer- /
ICEInactive-Logik. Daher zwei verschiedene Branch-Strategien:

**Stufe 1 (notifyChan visibility) — eigenständiger Upstream-PR**:
- Base: `upstream/main` (sauber, kein Phase-3.7i-Code nötig)
- Branch: `pr/h-lazyconn-notify-drop-visibility`
- Rationale: Robustheits-Patch am bestehenden `notifyChan`-Code, der schon in
  upstream/main vorhanden ist. Ohne Bezug zu Zwei-Timer-Logik.
- Commits:
  1. `lazyconn/inactivity: count + log silent notifyChan drops`
- Test-Coverage: 1 neuer Unit-Test
- Upstream-PR-Strategie: separat einreichen als NICHT-Phase-3.7i-blockierender Fix

**Stufen 0+2+3+4+5 (Panic-Recovery + Watchdog + Refaktor + Wiring + TransportSnapshot) — Phase-3.7i-Stack**:
- **Base v0.4 (Codex-Empfehlung)**: `phase3.7i-runtime-bugfixes-v0.5`
  - enthält den relevanten Lazy-Manager-Stand: `NewManagerWithTwoTimers`,
    `expectedWatcher`, HA-Defer-Logik, Activity-AttachICE
  - sauberer als `test/plan1+plan2-combined` (das ist Deployment-Branch mit
    Plan-2 mgmt-stream-watchdog/keepalive oben drauf — semantisch unverwandt)
  - sauberer als `pr/c/d/e` (das sind upstream-PR-Branches für andere
    Phase-3.7i-Subsysteme und enthalten den Lazy-Manager-Code nicht voll)
- Branch: `pr/g-phase3.7i-lazy-watchdog` (auf `phase3.7i-runtime-bugfixes-v0.5`)
- Commits (5 separate commits, jeder einzeln reviewbar):
  1. `peer/conn: add TransportSnapshot accessor for external watchdogs` (Stufe 5 — lock-free atomic-load über `statusICE`/`statusRelay`)
  2. `lazyconn/manager: defer recover() around consumer loop handlers` (Stufe 0 — pure Hardening, nicht Stuck-Fix)
  3. `lazyconn/manager: split state-mutation from blocking I/O, listener-first ordering` (Stufe 3 Refaktor: state-only/listener/close-best-effort Triple; `onPeerInactivityTimedOut` folgt demselben Pattern)
  4. `lazyconn/manager: reconcile watchdog with phased lock-strategy + async recovery` (Stufe 2 + 4: Watchdog-goroutine + recoverStuckPeer + Inflight-Dedupe + Test-Suite)
  5. `lazyconn/inactivity: count + log silent notifyChan drops` (Stufe 1 — kann als letzter Commit auch hier mitlaufen wenn Stufe 1 nicht parallel upstream-merged ist)
- Test-Coverage: ~26 neue Unit-Tests (siehe Section 6.1)

Author + Committer für alle Commits:
`Michael Uray <25169478+MichaelUray@users.noreply.github.com>`. Keine
Co-Authored-By-Trailer.

### 8.2 Deployment-Stufen
1. **Stage 1 (canary)**: Combined-Branch (`pr/g-phase3.7i-lazy-watchdog` auf
   `phase3.7i-runtime-bugfixes-v0.5` + Plan-1/Plan-2-Stack) auf S21 deployen.
   Monitor 72h auf:
   - Auftreten von „notifyChan ... full, dropped event" Warn-Logs
   - Auftreten von „lazyconn watchdog: N stuck peers ... spawning recovery"
     Warn-Logs
   - Auftreten von „watchdog: recovery complete" Info-Logs
   - VNC-Connect-Erfolgsrate zu Elmira ohne Force-Stop
   - `pendingCloses`-Counter (siehe R10) — sollte ≤ 1 bei normalem Betrieb
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

## Codex-Review-Anfrage v0.4

**Status der v0.3-Findings (Codex round-3 vom 2026-05-23 nachmittag)**: alle
1 BLOCKER + 2 SHOULD-FIXes adressiert:

- ✅ **BLOCKER (Recovery-Reihenfolge lässt Peer ohne Listener wenn `PeerConnIdle`
  hängt)**: Stufe 3 komplett neu geschnitten. Statt
  `transitionToActivityWatcherIOAfterUnlock(close → listen)` jetzt
  Triple-Helper:
  1. `transitionToActivityWatcherStateOnly` (unter Lock, state-flip)
  2. `armActivityListener` (lock-free, Listener ZUERST armieren)
  3. `closePeerConnBestEffort` (fire-and-forget goroutine, Close darf ewig
     hängen ohne den Peer zu blockieren)
  
  Damit kann der nächste WG-Wake-Event auch dann den Peer in den
  Activity-Pfad zurückbringen, wenn `Conn.Close()` strukturell hängt.
  Neuer Test `TestRecoverStuckPeer_PeerConnIdleHangs_ListenerStillArmed`
  verifiziert genau diesen Pfad. `onPeerInactivityTimedOut` folgt dem
  gleichen Triple-Pattern für Symmetrie.

- ✅ **SHOULD-FIX 1 (TransportSnapshot fiktive Felder)**: Stufe 5 nutzt jetzt
  die realen Felder `conn.statusICE *worker.AtomicWorkerStatus` und
  `conn.statusRelay *worker.AtomicWorkerStatus` (verifiziert in
  `peer/conn.go:125-140`). `AtomicWorkerStatus.Get() Status` lädt atomar
  ohne Lock. `worker.StatusConnected`/`StatusDisconnected` als Enums
  vorhanden (`worker/state.go:10-15`). Kein `conn.mu` mehr nötig.

- ✅ **SHOULD-FIX 2 (v0.2-Reste)**: `panicCount > 0` als Watchdog-Trigger aus
  R1 und Test 7 entfernt (Panic crashed den Prozess, Counter überlebt
  keinen Stuck-State). Single-Peer-Map-Frage aus 7.2 gestrichen (durch v0.3
  HA-Batch-Fix obsolet).

- ✅ **Branch-Basis (Codex round-3 Q4 v0.3)**: explizit `phase3.7i-runtime-bugfixes-v0.5`
  als Base für Stufen 0/2/3/4/5. Lokal verifiziert dass der Branch existiert
  und enthält `NewManagerWithTwoTimers`, `expectedWatcher`-state-machine,
  HA-Defer-Logik, Activity-AttachICE.

**Neue Fragen v0.4 die ich Codex bitte zu reviewen** (4 statt 8):

1. **Listener-armed-vor-Close-Reihenfolge** (Stufe 3, v0.4 BLOCKER-Fix):
   Bin ich richtig dass das Pattern `armActivityListener()` → `closePeerConnBestEffort()`
   keine subtle Race ergibt? Speziell: kann der neu armierte Listener feuern
   während die zugehörige `*peer.Conn` noch nicht-vollständig-geclosed ist?
   Die `onPeerActivity`-Validierung prüft `expectedWatcher == watcherActivity`
   + Conn-Lookup im peerStore — sollte das tolerant decken, aber bitte das
   Race-Modell sanity-checken.

2. **`isStuckPeer` für p2p-dynamic-ICE-only-Peers** (Section 7.2 Q1): die
   Heuristik verlangt `iceDisc && relayDisc`. Phase-3.7i p2p-dynamic-Peers
   ohne Relay-Watcher haben strukturell `relayDisc == true`. Soll der
   Watchdog die ConnectionMode des Peers konsultieren? Konkret: ist es
   sicherer den Watchdog auf p2p-dynamic-Peers nur bei `iceDisc && deltaICE > 0`
   triggern zu lassen?

3. **R10 — Leaked goroutines bei hängendem `Conn.Close()`**: aktuell
   akzeptiere ich potentiell 32 leaked goroutines pro stuck-Cycle bei 32
   Peers wenn `wgWatcherWg.Wait()` strukturell hängt. Alternative: zusätzlich
   eine parallele Spec für den `Conn.Close()`-Hang upstream beheben. Welcher
   Pfad ist sauberer — beides parallel, oder erst Watchdog deployen +
   Telemetrie sammeln?

4. **Klassifikation v0.4**: ist die Spec jetzt v0.4-implementierbar? Wenn
   nicht, was sind die verbleibenden BLOCKERs?

Nach Codex-OK v0.4: Implementation in 5 Commits (1 Refaktor + 1 Stufe 0 + 1
Stufe 1 + 1 Stufen 2+3+4 + 1 Stufe 5), Tests laut Section 6.1 (26 Unit-Tests
+ 2 Integration-Tests), Hardware-Soak laut Section 6.3.
