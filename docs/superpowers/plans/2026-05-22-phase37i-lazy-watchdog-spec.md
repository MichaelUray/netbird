---
status: spec v0.7.1 / IMPLEMENTATION-READY (self-reviewed gegen reale netbird-APIs)
target-branch: see Section 8.1 — Stufe 1 separat upstream-fähig auf upstream/main, Stufen 0+2+3+4+5+6 auf phase3.7i-runtime-bugfixes-v0.5
related-work: pr/mgmt-stream-keepalive, pr/mgmt-stream-watchdog, feat/force-relay-flag, phase3.7i-runtime-bugfixes-v0.5
review-status: 7× Codex pre-review + 1× Self-Review — alle BLOCKERs durchgereicht, v0.7.1 ist die Implementierungs-Vorlage. Self-Review hat keine weiteren API-Inkonsistenzen gefunden
changelog:
  - v0.1 (2026-05-22 vormittag): Initial spec, Code-Refs verifiziert, Buffer-Größe 1 als kritischer Befund
  - v0.2 (2026-05-22 nachmittag): Codex round-1 Korrekturen — Stufe-2-Pseudocode mit echten APIs, Lock-Strategie, Panic-Recovery
  - v0.3 (2026-05-23 vormittag): Codex round-2 Korrekturen — Panic-Hypothese verworfen, Stufen 2+3 lock-sparsam, neue Stufe 5 TransportSnapshot, HA-Batch-Korrektur
  - v0.7.1 (2026-05-24 morgen): Codex round-7 BLOCKER + Self-Review:
    * BLOCKER (Codex round-7) — Pseudocode-Compile-Fehler:
      `activityManager.RemovePeer(cfg.PublicKey)` an zwei Stellen kompiliert
      nicht. Reale Signatur ist
      `RemovePeer(log *log.Entry, peerConnID peerid.ConnID)`
      (activity/manager.go:84). Existing-Callsite bei manager.go:494 nutzt
      `m.activityManager.RemovePeer(cfg.Log, cfg.PeerConnID)`. **Fix**:
      vor `managedPeersMu.Unlock()` `connID := cfg.PeerConnID` + 
      `peerLog := cfg.Log` capturen; bei Recheck-Mismatch
      `m.activityManager.RemovePeer(peerLog, connID)` aufrufen.
    * Self-Review (Implementor) — alle weiteren API-Aufrufe der Spec
      gegen reale netbird-Codebasis verifiziert:
      - `activity.Manager.MonitorPeerActivity(peerCfg lazyconn.PeerConfig) error`
        (value-receive) ✓
      - `activity.Manager.HasPeer(connID peerid.ConnID) bool` (neu Stufe 6)
        signature realistisch ✓
      - `inactivity.Manager.RemovePeer(peer string)` (string, NICHT connID!)
        — meine `transitionToActivityWatcherStateOnly` ruft korrekt
        `m.inactivityManager.RemovePeer(mp.peerCfg.PublicKey)` ✓
      - `peerstore.Store.PeerConn(pubKey string) (*peer.Conn, bool)` ✓
      - `peerstore.Store.PeerConnIdle(pubKey string)` ✓
      - `lazyconn.PeerConfig{PublicKey, PeerConnID, Log}` Felder ✓
      - `managedPeer{peerCfg *lazyconn.PeerConfig, expectedWatcher watcherType}` ✓
      - `peerid "github.com/netbirdio/netbird/client/internal/peer/id"`
        Import-Alias in beiden Files ✓
      - `shouldDeferIdleForHA(inactivePeers map[string]struct{}, peerID string) bool` ✓
      Damit keine weiteren API-Inkonsistenzen → echte Implementation-Ready.
    * NICE-FIX (Codex round-7) — Section 8.1 Wording: Überschrift +
      Listenkopf von "5 Commits / Stufen 0+2+3+4+5" auf
      "6 Commits / Stufen 0+2+3+4+5+6" (inkl. HasPeer/Stufe 6)
      vereinheitlicht.
  - v0.7 (2026-05-23 spätabend final): Codex round-6 Korrekturen
    (IMPLEMENTATION-READY):
    * SHOULD-FIX 1 — Pseudocode-Typfehler: `expectedWatcher watcher` → 
      `expectedWatcher watcherType`. Realer Typ ist `watcherType int`
      (manager.go:32), mit Konstanten `watcherActivity watcherType = iota`
      und `watcherInactivity` (manager.go:19-20).
    * SHOULD-FIX 2 — Remove/Exclude-Race nach Unlock: beide Recovery-Pfade
      nutzen nach `managedPeersMu.Unlock()` noch gespeicherte `mp`/`cfg`
      und rufen `armActivityListener()`. Wenn parallel `removePeer()`
      (manager.go:486) läuft, kann ein Listener für einen inzwischen
      entfernten Peer entstehen. **Fix**: post-arm Re-Validate-Pattern.
      Nach `armActivityListener()` kurz revalidieren ob Peer noch managed
      ist (mit selber `PeerConnID`); falls nicht, gerade armierten Listener
      über `activityManager.RemovePeer(connID)` wieder entfernen. Keine
      starre Lock-Hierarchy zwischen `managedPeersMu` und
      `activity.Manager.mu` — recheck/cleanup-Pattern.
    * SHOULD-FIX 3 — Stale Text in R2 + R6: erwähnten noch
      `transitionToActivityWatcherLocked` / `recoverStuckPeer`-Altpfade.
      Auf v0.6-Architektur (`transitionToActivityWatcherStateOnly` +
      `recoverInactivityStuck` + `recoverActivityNoListener`) umgestellt.
    * Codex-Antworten auf offene Fragen v0.6 (alle Q's geschlossen):
      - Q1 (Case-a ConnectionMode-Split): NICHT nötig. `deltaRelay > 0`
        ist der Scope-Gate. Ohne Relay-Full-Sleep-Event keine Case-a-
        Recovery. Case-b ist mode-orthogonal.
      - Q2 (HasPeer-Recheck Lock-Hierarchy): Recheck außerhalb
        `managedPeersMu` ist richtig. Keine starre Hierarchy erzwingen.
      - Q3 (Case-c Hysteresis): separat lassen, gehört zu ICE-Backoff/
        guard-liveness, nicht Lazy-listener-Reconciliation.
      - Q4 (120s Tick-Intervall): hartkodiert OK für ersten PR. Kein
        Account-Setting.
    * Test-Harness-Note (Codex v0.6 Verifikation): existierender Race in
      Test-Mock `mockEndpointManager` (`listener_bind_test.go`) muss
      bereinigt werden bevor `TestActivityManager_HasPeer_RaceSafe`
      grün laufen kann. Als Pre-Implementation-Task in PR aufgenommen.
  - v0.6 (2026-05-23 spätabend): Codex round-5 Korrekturen:
    * BLOCKER — `watcherActivity` ohne Listener wird vom Watchdog NICHT
      geheilt. v0.5 erkannte selbst den kritischen Stuck-Fall (Logs zeigen:
      onPeerInactivityTimedOut flippt state-only auf `watcherActivity`,
      dann hängt PeerConnIdle, Listener nie armiert), aber Phase B sammelte
      nur `expectedWatcher == watcherInactivity` ein (v0.5 Spec line 522),
      und recoverStuckPeer skippte `watcherActivity` explizit. Damit
      widersprach sich der Spec: "Watchdog fängt Close-Hang ab" vs
      "expectedWatcher != watcherInactivity wird geskipped".
      **Fix**: Activity-Listener-Zustand explizit modellieren. Neue
      Stufe 6: `activity.Manager.HasPeer(connID) bool` (read-only unter
      `m.mu`). Watchdog-Snapshot erweitert auf
      `(pubKey, connID, expectedWatcher, hasActivityListener)`. Recovery
      zwei Fälle:
      (a) `watcherInactivity + relayDrops > 0 + disconnected`: state-flip
          + armActivityListener (HA-Defer aktiv).
      (b) `watcherActivity + !hasActivityListener + disconnected`: nur
          armActivityListener — kein state-flip, kein Close, keine
          HA-Defer (Peer ist bereits "soll active", wir reparieren nur
          den fehlenden Listener).
      (c) `watcherActivity + hasActivityListener`: no-op.
    * SHOULD-FIX — Stale Text: R1 erwähnte noch
      `(deltaRelay > 0 || deltaICE > 0)`, Commit-Plan noch
      `state-only/listener/close-best-effort Triple`. Beide bereinigt.
  - v0.5 (2026-05-23 abend): Codex round-4 Korrekturen:
    * BLOCKER — Listener-first + async Close erzeugt anderes hartes Race:
      `closePeerConnBestEffort` hängt in `Conn.Close()` (hält `conn.mu`,
      wartet auf `wgWatcherWg.Wait()`), neu armierter Listener feuert sofort
      → `onPeerActivity()` hält `managedPeersMu` und ruft `PeerConnOpen()`
      → braucht `conn.mu` → deadlock. **Fix**: `closePeerConnBestEffort`
      KOMPLETT aus dem Watchdog-Recovery-Pfad raus. Logs zeigen den
      Stuck-State als `Disconnected, Disconnected` (Close ist also bereits
      gelaufen); der Watchdog muss nur den Listener armieren. Das
      `onPeerInactivityTimedOut`-Refactor verändert sich entsprechend:
      synchrone Close-Sequenz bleibt nach unlock erhalten (existing
      semantics), Watchdog-Recovery ruft Close nicht mehr auf.
    * BLOCKER/SHOULD-FIX — ICE- und Relay-Drops nicht vermischen:
      `iceInactiveChan` führt im realen Code zu `DetachICEForPeer()`
      (`conn_mgr.go:257 runDynamicInactivityLoop`), `inactivePeersChan`
      führt zum Full-Sleep im Lazy-Manager. v0.4 mischte beide in
      `isStuckPeer(deltaRelay || deltaICE)`. **Fix**: Watchdog heilt nur
      Relay-Drops; ICE-Drops bleiben Telemetrie (Stufe 1 Counter) und
      bekommen ggf. später einen eigenen ConnMgr-Watchdog in einer
      Folge-Spec.
    * SHOULD-FIX — Stale Refs: `TestConsumerPanicCount_Accessible` (Test 3)
      entfernt; R4-Wording auf neuen Pair-Helper `state-only + listener-arm`
      umgestellt.
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
    // v0.6 Codex round-5 fix: snapshot ALL peers regardless of expectedWatcher
    // because we need to detect BOTH stuck states:
    //   (a) watcherInactivity + relayDrops > 0 + disconnected (notifyChan drop)
    //   (b) watcherActivity + !hasActivityListener + disconnected (hung Close
    //       in onPeerInactivityTimedOut after state-flip)
    type peerSnap struct {
        pubKey          string
        connID          peerid.ConnID
        expectedWatcher watcherType   // real type per manager.go:32
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

    // PHASE C: per-peer transport-state + listener-state classification, no lock held
    stuckInactivityBatch := make(map[string]struct{})    // case (a) — needs HA-Defer
    stuckActivityNoListener := make(map[string]struct{}) // case (b) — no HA-Defer
    for _, s := range snaps {
        conn, ok := m.peerStore.PeerConn(s.pubKey)
        if !ok { continue }
        iceDisc, relayDisc := conn.TransportSnapshot()  // Stufe 5 API
        if !iceDisc || !relayDisc {
            continue   // healthy or partially connected
        }
        switch s.expectedWatcher {
        case watcherInactivity:
            if isStuckPeer(iceDisc, relayDisc, deltaRelay) {
                stuckInactivityBatch[s.pubKey] = struct{}{}
            }
        case watcherActivity:
            // Stuck-State-Variante (b): peer soll active sein, ist aber
            // disconnected UND hat keinen Activity-Listener registriert.
            // Das ist exakt der Zustand nach hängendem
            // onPeerInactivityTimedOut.PeerConnIdle. Kein drops-Trigger
            // nötig — der fehlende Listener ist das eindeutige Signal.
            if !m.activityManager.HasPeer(s.connID) {   // Stufe 6 API
                stuckActivityNoListener[s.pubKey] = struct{}{}
            }
        }
    }
    total := len(stuckInactivityBatch) + len(stuckActivityNoListener)
    if total == 0 { return }

    log.Warnf("lazyconn watchdog: %d stuck peers (inactivity-stuck=%d relayDrops=%d, activity-no-listener=%d) — ICE-drops=%d telemetry only",
        total, len(stuckInactivityBatch), deltaRelay, len(stuckActivityNoListener), deltaICE)

    // PHASE D: spawn bounded async recovery per peer
    // Each case-a goroutine receives the full case-a batch for correct
    // HA-defer semantics. Case-b skips HA-defer entirely.
    for pubKey := range stuckInactivityBatch {
        m.spawnRecovery(ctx, pubKey, recoveringPeers, recoveringMu, func(pk string) {
            m.recoverInactivityStuck(ctx, pk, stuckInactivityBatch)
        })
    }
    for pubKey := range stuckActivityNoListener {
        m.spawnRecovery(ctx, pubKey, recoveringPeers, recoveringMu, func(pk string) {
            m.recoverActivityNoListener(ctx, pk)
        })
    }
}

// spawnRecovery handles the inflight-dedupe + panic-recovery wrapper for
// any recovery-goroutine. Caller passes the actual recovery function.
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

func isStuckPeer(iceDisc, relayDisc bool, deltaRelay uint64) bool {
    // Watchdog scope (v0.5 Codex round-4 correction): ONLY full-sleep
    // recovery, driven by inactivePeersChan/relay-drops. ICE-drops belong
    // semantically to ConnMgr.runDynamicInactivityLoop (which triggers
    // DetachICEForPeer, not full sleep). Mixing both would muddy the
    // two-timer lifecycle. ICE-recovery is potential future work in a
    // separate ConnMgr-Watchdog spec.
    //
    // Trigger requires BOTH transports disconnected AND relayDrops > 0.
    // Drops alone do not trigger; disconnection alone does not trigger.
    return iceDisc && relayDisc && deltaRelay > 0
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

#### Stufe 3: Recovery-Helper mit Lock-Disziplin + Listener-Only-Recovery (v0.5)
**Datei**: `client/internal/lazyconn/manager/manager.go` (Refaktor + neue Methode)
**Scope**: ~50 LOC Refaktor + ~60 LOC neue Methode

**Codex-Korrekturen v0.5 (BLOCKER round-4)**:
- **Race mit `conn.mu`/`managedPeersMu`**: v0.4 spawnte `closePeerConnBestEffort`
  als fire-and-forget goroutine vor Listener-Arm. Wenn `Conn.Close()` hängt
  (hält `conn.mu` und wartet auf `wgWatcherWg.Wait()` per `conn.go:307-309`),
  feuert der neu armierte Listener sofort → `onPeerActivity()` hält
  `managedPeersMu` und ruft `PeerConnOpen()` (`manager.go:609`) → braucht
  `conn.mu` → deadlock auf `managedPeersMu`.
  **v0.5-Fix**: `closePeerConnBestEffort` KOMPLETT aus dem Watchdog-Recovery-
  Pfad entfernt. Die Stuck-State-Beobachtungen zeigen den Peer als
  `Disconnected, Disconnected` (Logs S21 Section 2.2 line 89). Close ist
  also bereits gelaufen; was fehlt ist nur der Activity-Listener (notifyChan
  hat den `onPeerInactivityTimedOut`-Event verschluckt, danach wurde nie
  `MonitorPeerActivity` gerufen). Daher: Watchdog macht state-flip +
  Listener-Arm, nichts mehr.
- **`onPeerInactivityTimedOut` bleibt mit synchronem Close**: das ist der
  existing happy-path, in dem der Close intentional läuft (Peer wird in
  Sleep gebracht weil Inactivity-Timer abgelaufen). v0.3-Ordering
  wiederhergestellt: nach state-flip+unlock kommt erst `PeerConnIdle`
  (synchron), DANN `armActivityListener`. Wenn `PeerConnIdle` strukturell
  hängt, bleibt der Peer im `watcherActivity`-State-flip ohne Listener —
  exakt die Stuck-State-Symptomatik. Der Watchdog (Stufe 2) deckt genau
  diesen Fall ab und armiert den Listener nachträglich. Damit ist die
  v0.4-Race ausgeschlossen (kein async-Close), und der Stuck-Fall hat
  trotzdem Recovery.

**Codex-Korrekturen v0.3 (übernommen)**:
1. **HA-Batch**: `shouldDeferIdleForHA` mit Single-Peer-Map deferred ad infinitum
   (alle anderen HA-Member werden fälschlich als „aktiv" interpretiert).
   `recoverStuckPeer` muss den vollen `stuckBatch` durchreichen.
2. **Lock-Disziplin**: `PeerConnIdle` ist blocking (chain to `Conn.Close()` mit
   `wgWatcherWg.Wait()`). Darf NICHT unter `managedPeersMu` laufen.

Refaktor splittet die existierende `transitionToActivityWatcher`-Logik in zwei
zentrale Helfer (state-only / listener-arm). Die Close-Operation wird NICHT
in einen separaten Helfer gepackt, sondern bleibt im inactivity-Pfad als
synchroner Aufruf zwischen state-flip und listener-arm. Der Watchdog ruft
Close nicht auf.

```go
// transitionToActivityWatcherStateOnly performs the non-blocking state-machine
// part of the transition (expectedWatcher flip + RemovePeer from inactivity).
// Caller MUST hold m.managedPeersMu. No I/O here.
func (m *Manager) transitionToActivityWatcherStateOnly(mp *managedPeer) {
    mp.peerCfg.Log.Infof("transition to watcherActivity (state-only) from %s", mp.expectedWatcher)
    mp.expectedWatcher = watcherActivity
    m.inactivityManager.RemovePeer(mp.peerCfg.PublicKey)
}

// armActivityListener installs the activity monitor for a peer. Idempotent
// when called against a peer that already has an active monitor (the
// activity manager internally guards against double-arm). No lock required
// (activityManager has its own internal mutex).
func (m *Manager) armActivityListener(mp *managedPeer) {
    if err := m.activityManager.MonitorPeerActivity(*mp.peerCfg); err != nil {
        mp.peerCfg.Log.Errorf("failed to create activity monitor: %v", err)
    }
}

// recoverInactivityStuck handles case (a) — peer is in watcherInactivity but
// notifyChan dropped the inactivity-timed-out event, so the state never
// flipped. State-flip + arm listener (no Close, see v0.5 reasoning).
// HA-defer applies (we are transitioning a peer that was "wanted in sleep").
//
// Runs in its own goroutine (spawned by reconcileTick). Takes the FULL
// stuckBatch for correct HA-defer semantics.
func (m *Manager) recoverInactivityStuck(ctx context.Context, pubKey string, stuckBatch map[string]struct{}) {
    m.managedPeersMu.Lock()
    cfg, ok := m.managedPeers[pubKey]
    if !ok { m.managedPeersMu.Unlock(); return }
    mp, ok := m.managedPeersByConnID[cfg.PeerConnID]
    if !ok { m.managedPeersMu.Unlock(); return }
    // Re-validate — onPeerInactivityTimedOut may have beat us
    if mp.expectedWatcher != watcherInactivity {
        m.managedPeersMu.Unlock()
        return
    }
    if m.shouldDeferIdleForHA(stuckBatch, mp.peerCfg.PublicKey) {
        mp.peerCfg.Log.Infof("watchdog: defer inactivity-stuck recovery (HA peers active, batch=%d)", len(stuckBatch))
        m.managedPeersMu.Unlock()
        return
    }
    // v0.7.1 Codex round-7: capture connID+peerLog BEFORE unlock to match
    // the real activity.Manager.RemovePeer(*log.Entry, peerid.ConnID) signature
    // (activity/manager.go:84). After unlock, cfg/mp may have been mutated
    // by RemovePeer/ExcludePeer racing in parallel.
    connID := cfg.PeerConnID
    peerLog := cfg.Log
    m.transitionToActivityWatcherStateOnly(mp)
    m.managedPeersMu.Unlock()

    // Arm listener (idempotent). No Close — Conn is already disconnected;
    // calling Close would risk the v0.4 conn.mu/managedPeersMu deadlock.
    m.armActivityListener(mp)

    // v0.7 Codex round-6: post-arm Re-Validate. If RemovePeer/ExcludePeer
    // ran in parallel between our snapshot and now, the listener we just
    // armed belongs to a peer that is no longer managed. Cleanup using
    // the connID + peerLog captured before unlock.
    if !m.peerStillManaged(pubKey, connID) {
        m.activityManager.RemovePeer(peerLog, connID)
        return
    }
    peerLog.Infof("watchdog: recovery complete (inactivity-stuck: watcherInactivity -> watcherActivity, listener armed)")
}

// recoverActivityNoListener handles case (b) — peer is in watcherActivity
// but no Activity-Listener is registered. This is the post-Close-hang state:
// onPeerInactivityTimedOut did the state-flip under lock, then hung in
// PeerConnIdle (Conn.Close()) and never reached armActivityListener.
//
// Recovery: arm listener only. No state mutation needed (already
// watcherActivity). No HA-defer needed (peer is already "wanted active"; we
// are not transitioning, just repairing an incomplete transition).
//
// Trigger sees: expectedWatcher == watcherActivity AND
// !activityManager.HasPeer(connID) AND TransportSnapshot returns both
// disconnected. No drops-counter dependency.
func (m *Manager) recoverActivityNoListener(ctx context.Context, pubKey string) {
    m.managedPeersMu.Lock()
    cfg, ok := m.managedPeers[pubKey]
    if !ok { m.managedPeersMu.Unlock(); return }
    mp, ok := m.managedPeersByConnID[cfg.PeerConnID]
    if !ok { m.managedPeersMu.Unlock(); return }
    // Re-validate (state could have shifted since snapshot)
    if mp.expectedWatcher != watcherActivity {
        m.managedPeersMu.Unlock()
        return
    }
    // v0.7.1 Codex round-7: capture connID+peerLog BEFORE unlock; see same
    // rationale as recoverInactivityStuck.
    connID := cfg.PeerConnID
    peerLog := cfg.Log
    m.managedPeersMu.Unlock()

    // Re-check listener under no lock (activity.Manager has its own m.mu).
    // If onPeerInactivityTimedOut finally finished armActivityListener
    // between snapshot and now, HasPeer returns true and we no-op.
    if m.activityManager.HasPeer(connID) {
        return
    }
    m.armActivityListener(mp)

    // v0.7 Codex round-6: post-arm Re-Validate. If RemovePeer/ExcludePeer
    // ran in parallel between snapshot and now, the listener we just
    // armed belongs to a peer that is no longer managed. Cleanup using
    // the captured connID + peerLog.
    if !m.peerStillManaged(pubKey, connID) {
        m.activityManager.RemovePeer(peerLog, connID)
        return
    }
    peerLog.Infof("watchdog: recovery complete (activity-no-listener: listener re-armed for peer in watcherActivity)")
}

// peerStillManaged is the v0.7 Re-Validate helper for both recovery paths.
// After listener-arm outside lock, this verifies the peer is still managed
// with the SAME PeerConnID (defending against RemovePeer / ExcludePeer
// that may have replaced or removed the managedPeer between snapshot and
// arm). Returns true if peer is still managed and connID matches.
func (m *Manager) peerStillManaged(pubKey string, expectedConnID peerid.ConnID) bool {
    m.managedPeersMu.Lock()
    defer m.managedPeersMu.Unlock()
    cfg, ok := m.managedPeers[pubKey]
    if !ok { return false }
    return cfg.PeerConnID == expectedConnID
}
```

**Refaktor von `onPeerInactivityTimedOut`** (line 629) — sequentieller
close→listen pattern (v0.3-Ordering), outside-lock:

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

    // Phase 2: blocking I/O outside lock. Close FIRST (synchronous), then
    // arm listener. This matches v0.3 ordering — the listener can only fire
    // AFTER Close has released conn.mu. The trade-off: if Close hangs
    // forever, the listener is never armed and the peer enters the stuck
    // state. The watchdog (Stufe 2) catches this scenario by detecting the
    // peer's "watcherActivity without listener" condition via TransportSnapshot
    // and re-arms the listener. The hung close goroutine leaks (R12).
    for _, mp := range toTransition {
        m.peerStore.PeerConnIdle(mp.peerCfg.PublicKey)   // can block forever; existing-behavior risk
        m.armActivityListener(mp)
    }
}
```

**Behavior-Change-Warnung**: das ist ein **echter Behavior-Change** an
`onPeerInactivityTimedOut`. Vorher lief `PeerConnIdle` synchron UNTER `managedPeersMu`
(per TODO `potentially can be optimized`); jetzt synchron NACH unlock. Das ist
eine **gewünschte Verbesserung** weil sie den Lock befreit, aber die Close-
Latency-Charakteristik bleibt unverändert. Tests müssen die Ordnung
(close → listener-arm) verifizieren um die v0.4-Race nicht versehentlich
wieder einzuführen.

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

#### Stufe 6: activity.Manager.HasPeer(connID) read-only API (v0.6 NEU)
**Datei**: `client/internal/lazyconn/activity/manager.go` (neue exported Methode)
**Scope**: ~10 LOC

**Codex-Korrektur v0.6 (BLOCKER round-5)**: ohne sichtbaren Listener-Status
kann der Watchdog den `watcherActivity + !hasListener`-Stuck-State nicht
unterscheiden vom gesunden `watcherActivity + hasListener`. Verifiziert in
`activity/manager.go`: `Manager.peers map[peerid.ConnID]listener` geschützt
durch `Manager.mu` (line 30-39). Neue Read-Only-API:

```go
// HasPeer reports whether an activity listener is currently registered
// for the given peer connection ID. Intended for the lazyconn-Manager
// watchdog to distinguish "active and listening" from "active but stuck
// after hung Close".
func (m *Manager) HasPeer(connID peerid.ConnID) bool {
    m.mu.Lock()
    defer m.mu.Unlock()
    _, ok := m.peers[connID]
    return ok
}
```

Tests:
- `TestActivityManager_HasPeer_Empty`: Manager ohne MonitorPeerActivity-Aufruf,
  `HasPeer` für beliebige connID → `false`.
- `TestActivityManager_HasPeer_AfterMonitor`: `MonitorPeerActivity(cfg)`
  aufrufen, dann `HasPeer(cfg.PeerConnID)` → `true`.
- `TestActivityManager_HasPeer_AfterRemove`: `MonitorPeerActivity` +
  `RemovePeer` → `HasPeer` → `false`.
- `TestActivityManager_HasPeer_RaceSafe`: `go test -race` mit konkurrenten
  Monitor/Remove + HasPeer.

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
   im onPeerActivity-Pfad, assert dass Start() weiterläuft.
2. `TestManagerStart_PanicInOnPeerInactivityTimedOut_DoesNotKillConsumer`: analog
   für inactivity-Pfad.
   (v0.5: panic-counter ist nicht exposed — Stufe 0 ist reines Hardening,
   kein Watchdog-Signal. Daher kein Test für externe Counter-API.)

**Für Stufe 1 (notifyChan visibility)**:
4. `TestNotifyChan_FullChannelIncrementsDropCounter`: voller Channel, weitere
   Notification, prüfe `DropCounters()` returnt 1.
5. `TestNotifyChan_DropLogThrottled`: 100 drops → max 3 Log-Lines (1st, 10th, 100th).
6. `TestDropCounters_RelayAndICESeparate`: drops auf inactivePeersChan zählen nur
   relayDrops, drops auf iceInactiveChan zählen nur iceDrops.

**Für Stufe 2 (Reconcile-Watchdog) — v0.6 zwei Recovery-Pfade**:
7a. `TestReconcileWatchdog_DetectsInactivityStuck`: peer in `watcherInactivity`,
    `relayDrops` delta > 0, beide Transports disconnected → `recoverInactivityStuck`
    wird aufgerufen (Case a).
7b. `TestReconcileWatchdog_DetectsActivityNoListener` (Codex v0.6 BLOCKER-Fix):
    peer in `watcherActivity`, `HasPeer(connID) == false`, beide Transports
    disconnected → `recoverActivityNoListener` wird aufgerufen (Case b). Auch
    OHNE `relayDrops` delta.
7c. `TestReconcileWatchdog_ICEDropsOnlyDoesNotTrigger` (Codex v0.5): wenn nur
    `iceDrops` steigen aber `relayDrops` nicht, soll Case-a Watchdog NICHT
    recovern (gehört semantisch zu ConnMgr.runDynamicInactivityLoop).
7d. `TestReconcileWatchdog_ActivityWithListener_NoOp` (v0.6): peer in
    `watcherActivity` + `HasPeer == true` + disconnected → keine Recovery
    (Case c — gesunder zwischen-Zustand vor ICE-handshake).
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

**Für Stufe 3 (Refaktor + Helper) — v0.6 split**:
15. `TestTransitionToActivityWatcherStateOnly_HappyPath`: zentral getestet.
16. `TestRecoverInactivityStuck_HappyPath`: peer in watcherInactivity →
    state-flip + listener armiert. **v0.5: kein Close-Call** — assert dass
    `peerStore.PeerConnIdle` NICHT aufgerufen wurde.
17. `TestRecoverInactivityStuck_AlreadyActivity_NoOp`: peer schon in
    watcherActivity → return early, kein zweiter state-flip.
18. `TestRecoverInactivityStuck_RespectsHA_FullBatch`: HA-defer mit vollem Batch.
19. `TestRecoverInactivityStuck_NoCloseDeadlock`: assert dass die Funktion
    NIEMALS `PeerConnIdle/Close` triggert (counter-Mock).
20. **`TestRecoverActivityNoListener_HappyPath`** (v0.6 NEU): peer in
    watcherActivity, `HasPeer == false` → `armActivityListener` wird genau
    1× aufgerufen; expectedWatcher bleibt watcherActivity (kein state-flip).
21. **`TestRecoverActivityNoListener_ListenerArmedConcurrently_NoOp`**
    (v0.6 NEU): peer in watcherActivity, zwischen Snapshot und Recovery hat
    onPeerInactivityTimedOut endlich `armActivityListener` aufgerufen
    (`HasPeer == true`) → recover macht NICHTS (Re-Check nach Lock-Release).
22. **`TestRecoverActivityNoListener_SkipsHADefer`** (v0.6 NEU): assert dass
    `shouldDeferIdleForHA` für diesen Pfad NICHT aufgerufen wird (Peer ist
    bereits "soll active", kein HA-Failover-Risiko).
23. `TestListenerArmedTriggeredByNextTraffic` (v0.6 End-to-End): state-flip
    done, Listener armiert; simuliere WG-Traffic → `onPeerActivity` feuert
    → peer wird via `PeerConnOpen` geöffnet (das funktioniert weil Conn
    bereits Disconnected war, keine konkurrente Close-Op auf conn.mu).
24. `TestOnPeerInactivityTimedOut_AfterRefactor_HappyPath`: existierender
    Test-Sweep nach Refaktor unverändert grün.
25. `TestOnPeerInactivityTimedOut_AfterRefactor_CloseBeforeListenerArm`
    (v0.5): assert dass der refactored Pfad sequentiell Close → ListenerArm
    macht (v0.3-Ordering wiederhergestellt nach v0.4-Race-Befund).
26. `TestOnPeerInactivityTimedOut_AfterRefactor_IOOutsideLock`: assert dass
    das refactored `onPeerInactivityTimedOut` das blocking I/O nach unlock
    geschoben hat.
27. **`TestRecoverInactivityStuck_RemoveRaceAfterUnlock`** (v0.7 Codex
    round-6 R14): zwischen Snapshot und Unlock parallel `RemovePeer(pubKey)`
    aufrufen; assert dass `peerStillManaged` `false` returnt und
    `activityManager.RemovePeer(pubKey)` cleanup-aufgerufen wurde, sodass
    kein Orphan-Listener übrig bleibt.
28. **`TestRecoverActivityNoListener_RemoveRaceAfterUnlock`** (v0.7 Codex
    round-6 R14): analog für Case-b.
29. **`TestRecoverActivityNoListener_ConnIDChangeAfterUnlock`** (v0.7): peer
    wird parallel via `RemovePeer + AddPeer` neu hinzugefügt mit anderer
    `PeerConnID`. `peerStillManaged` returnt `false` weil ConnID nicht mehr
    matched; cleanup über alte ConnID.

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
- **`TestIntegration_InactivityStuck_WatchdogHeals`** (Case a): erzeuge
  `watcherInactivity`-Stuck-State via Consumer-Goroutine blockieren; prüfe
  dass innerhalb von 2 Watchdog-Ticks der Peer in `watcherActivity` ist und
  HasPeer(connID) == true.
- **`TestIntegration_ActivityNoListener_WatchdogHeals`** (Case b, Codex v0.6
  BLOCKER-Fix): erzeuge den exakten Stuck-Fall — onPeerInactivityTimedOut
  flippt state-only auf `watcherActivity`, hängender PeerConnIdle stoppt
  Listener-Arm. Prüfe dass innerhalb von 2 Watchdog-Ticks `HasPeer(connID)
  == true` und nachträglich injizierter WG-Traffic den Peer via
  `PeerConnOpen` öffnet.
- **`TestIntegration_PanicInConsumer_WatchdogHeals`**: injiziere panic im
  onPeerInactivityTimedOut, prüfe dass nach Stufe-0-Recovery + Watchdog-Tick
  der Peer wieder in einem konsistenten Zustand ist (full-loop verification).

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
| R1 | Watchdog false-positive: gesunder Peer wird gestört | Mittel | Zwei separate Trigger: (a) `watcherInactivity` Pfad — Multi-Faktor `iceDisc && relayDisc && deltaRelay > 0` + `shouldDeferIdleForHA`-Check + Re-Check `expectedWatcher == watcherInactivity` nach Lock-Reacquire; (b) `watcherActivity + !hasActivityListener` Pfad — eindeutiges Listener-fehlt-Signal, kein drops-Trigger nötig, `armActivityListener` ist idempotent. Re-Check `HasPeer(connID)` nach Lock-Release deckt Race ab |
| R2 | Race zwischen `onPeerInactivityTimedOut` und Watchdog-Reconcile (Case-a) | Mittel | Beide nutzen `managedPeersMu`; `recoverInactivityStuck` prüft `expectedWatcher == watcherInactivity` redundant nach Lock-Reacquire; `transitionToActivityWatcherStateOnly` shared zwischen beiden Pfaden. Race mit `recoverActivityNoListener` (Case-b) ausgeschlossen weil Case-a + Case-b zueinander zustands-orthogonal sind. Inflight-Dedupe-Map verhindert dass derselbe pubKey gleichzeitig in beide Cases läuft |
| R3 | Watchdog stört intentionale LazyTimeout-Konfig (z.B. Admin setzt RelayTimeout=24h für long-idle peers) | Niedrig | Trigger basiert auf `dropDelta > 0` (Indikator dass Events verloren gehen) + Disconnected-State, NICHT auf reiner Idle-Zeit. Admin-Config wird respektiert |
| R4 | `PeerConnIdle` chain → `Conn.Close()` → `wgWatcherWg.Wait()` ist hart blocking | **BLOCKER (Codex v0.3 / v0.4 / v0.5)** | **v0.5-Mitigation**: Split in `transitionToActivityWatcherStateOnly` (unter Lock) + `armActivityListener` (außerhalb Lock). `onPeerInactivityTimedOut` ruft `PeerConnIdle` weiterhin synchron außerhalb Lock auf (v0.3-Ordering, close → listen). Der Watchdog (`recoverStuckPeer`) ruft `PeerConnIdle` **gar nicht mehr** auf — vermeidet damit die v0.4-Race komplett (conn.mu/managedPeersMu Deadlock zwischen async-Close und neuem `onPeerActivity → PeerConnOpen`). Stuck-Recovery passiert nur via Listener-Arm, weil der Conn im Stuck-State bereits Disconnected ist. |
| R5 | notifyChan-Drop-Counter wächst monoton → keine echte Heilung | Niedrig | Counter ist Telemetrie, keine Korrektur-Logik; Heilung passiert via Watchdog. Test `TestReconcileWatchdog_DropCounterAloneIsNotTrigger` deckt diese Mitigation ab |
| **R6** | **Watchdog selbst hängt am selben Lock/blockierenden Pfad und heilt dadurch nichts** | **v0.7 obsolet** | v0.5+: weder `recoverInactivityStuck` noch `recoverActivityNoListener` ruft `PeerConnIdle`/`Conn.Close()` auf — kein blocking-Pfad mehr im Watchdog-Code. Watchdog-Tick selbst nutzt Phase-A/B/C/D-Architektur mit strikten Lock-Lebensdauern; `armActivityListener` ist non-blocking (activity.Manager hat keinen blocking-IO-Pfad). Damit kann der Watchdog strukturell nicht mehr am eigenen Recovery-Pfad hängen. R6 historisch erfasst, in v0.7 nicht mehr aktuell |
| **R7** | **Panic im Watchdog-Loop killt sich selbst dauerhaft** (gleicher Bug wie für Start-Loop in Stufe 0) | **Mittel (Codex v0.2)** | v0.3: `runReconcileWatchdog` hat eigenes `defer recover()` mit `go m.runReconcileWatchdog(ctx)`-Self-Restart. Pro-Peer-Recovery-goroutines haben eigenes `defer recover()`. |
| **R8** | **HA-Defer mit Single-Peer-Map deferred ad infinitum** (Codex v0.3 BLOCKER): `shouldDeferIdleForHA` interpretiert „peer nicht im inactivePeers map" als „aktiv". Single-Peer-Map machte alle anderen HA-Member fälschlich aktiv | **BLOCKER (Codex v0.3)** | Watchdog sammelt vollen `stuckBatch` in Phase C/D und übergibt ihn an jede Recovery-goroutine. `shouldDeferIdleForHA` arbeitet wie beim regelmäßigen Pfad mit dem realen Batch der stuck-Peers. Test `TestRecoverStuckPeer_RespectsHA_SinglePeerBatch_RegressionGuard` deckt das ab. |
| **R9** | **`onPeerInactivityTimedOut`-Refaktor verändert blocking-call-Ordnung** (Side-Effect des Stufe-3-Refaktors): `PeerConnIdle` läuft jetzt nach `expectedWatcher=activity`-Flip, nicht davor. Kann subtle Race mit `onPeerActivity` ergeben wenn der activity-Listener bereits feuert bevor `PeerConnIdle` abgeschlossen ist | **Mittel (v0.3 neu)** | Test-Coverage `TestOnPeerInactivityTimedOut_AfterRefactor_*`. Auch: das ist die intendierte Semantik (state-machine zuerst, I/O nachher) und entspricht dem TODO „potentially can be optimized" auf manager.go:659. Race mit activity-Listener ist tolerabel weil der `onPeerActivity`-Pfad selber `expectedWatcher == watcherActivity` als Vorbedingung prüft. |
| **R10** | **~~Listener-armed-before-close leaked goroutines~~** | **v0.5 obsolet** | v0.5 entfernt `closePeerConnBestEffort` aus dem Watchdog-Pfad komplett (Codex v0.4 BLOCKER-Fix). Es gibt keine async-Close-goroutines mehr; `onPeerInactivityTimedOut` ruft Close synchron, das ist existing-behavior. |
| **R11** | **`TransportSnapshot` liest atomare Status-Felder ohne Lock — Race mit ICE-Reconnect Möglichkeit** (v0.4 SHOULD-FIX 1 Side-Effect): Watchdog könnte einen Peer „disconnected" sehen während er gerade in `StatusConnecting`/`StatusReconnecting` ist, und unnötig recovern | **Niedrig (v0.4)** | `isStuckPeer` verlangt BEIDE Transports disconnected UND `relayDrops` delta > 0 — die Wahrscheinlichkeit dass ein Peer GLEICHZEITIG ICE+Relay-Reconnect macht UND notifyChan dropt ist sehr klein. False-positive-Cost ist eine zusätzliche Recovery-Goroutine die nur den Listener nochmal armiert (idempotent), kein State-Corruption. Test `TestTransportSnapshot_RaceSafe` deckt mit `-race` ab. |
| **R12** | **`onPeerInactivityTimedOut` mit hängendem `PeerConnIdle` blockiert Consumer-Goroutine** (v0.5 explicit-known gap): wenn `Conn.Close()` strukturell hängt, blockiert das gesamten Inactivity-Consumer-Loop. Neue Inactivity-Events werden nicht mehr verarbeitet | **Hoch (v0.5 known-limitation)** | **Akzeptiert als out-of-scope**. Der Watchdog mitigiert PEER-LEVEL Stuck-States nachträglich (armt Listener für betroffene Peers). Aber der globale Consumer-Stall fixt der Watchdog nicht. Strukturelle Lösung: separate Folge-Spec für `Conn.Close()`-Hang upstream + `wgWatcherWg.Wait()` cancellable machen. Dieser Spec ist ein "best-effort PEER-recovery", nicht ein "consumer-rescue". |
| **R13** | **`recoverInactivityStuck` ohne Close lässt veralteten Conn-State stehen** (v0.5 trade-off): der alte `*peer.Conn` bleibt im peerStore, mit allen workerICE/workerRelay-State und potentiellen partial-handshake-Daten. Nächster Activity-Event triggert `PeerConnOpen` der den Conn erneut öffnet | **Niedrig (v0.5)** | `Conn.Open()` per `conn.go:213-220` prüft `conn.opened` und ist No-Op wenn schon offen. Im Stuck-State ist der Conn aber Disconnected/Disconnected — `conn.opened` ist false → Open() läuft normal durch. workerICE-Reset passiert via `m.peerStore.PeerConnOpen` + nachfolgend `conn.ResetIceBackoff()` + `AttachICE` (`manager.go:609-621`). Damit ist der Recovery-Pfad identisch zum normalen Activity-Wake. |
| **R14** | **Remove/Exclude-Race nach `managedPeersMu.Unlock()` lässt Listener für entfernten Peer leben** (v0.7 Codex round-6): nach Unlock nutzen beide Recovery-Pfade gespeicherte `mp`/`cfg` für `armActivityListener`. Wenn parallel `removePeer()` (manager.go:486) oder `ExcludePeer` (manager.go:198) lief, kann ein Listener für einen nicht mehr managed-Peer entstehen | **Mittel (v0.7)** | Post-arm Re-Validate-Pattern via Helper `peerStillManaged(pubKey, expectedConnID) bool` (kurzer Lock-Cycle nach Arm). Wenn Peer weg oder ConnID gewechselt: `activityManager.RemovePeer(pubKey)` cleanup. Kein starres Lock-Hierarchy-Erzwingen — recheck/cleanup-Pattern (Codex-Empfehlung). |

### 7.2 Offene Fragen für Codex (v0.7 — alle geschlossen)

Alle v0.6-Fragen wurden durch Codex round-6 beantwortet:

- ✅ **Q1 (Case-a ConnectionMode-Split)**: NICHT nötig. `deltaRelay > 0`
  ist der Scope-Gate für Case-a. Ohne Relay-Full-Sleep-Event keine
  Case-a-Recovery. Case-b ist mode-orthogonal.
- ✅ **Q2 (HasPeer-Recheck Lock-Hierarchy)**: Recheck außerhalb
  `managedPeersMu` ist richtig. Keine starre Hierarchy zwischen
  `managedPeersMu` und `activity.Manager.mu` erzwingen. Recheck/cleanup-
  Pattern (jetzt als `peerStillManaged` in Stufe 3 implementiert).
- ✅ **Q3 (R12 / Conn.Close-Hang Folge-Spec)**: v0.6/v0.7 deckt den
  praktischen Recovery-Pfad ab. Globaler Consumer-Stall bleibt
  out-of-scope; Follow-up-Spec `phase3.7i-conn-close-cancellable` für
  `wgWatcherWg.Wait()` Cancellation als separate Arbeit.
- ✅ **Q4 (Case-c Hysteresis)**: separat lassen. Gehört zu
  ICE-Backoff/guard-liveness, nicht Lazy-listener-Reconciliation. Kein
  Scope für diesen Spec.
- ✅ **Q5 (120s Tick-Intervall)**: hartkodierte 120s OK für ersten PR.
  Kein Account-Setting.

Damit keine offenen Spec-Fragen mehr → **Implementation kann starten**.

### 7.3 Pre-Implementation-Tasks (Codex round-6 Verifikation)

- **Test-Harness-Race in `mockEndpointManager`** (`listener_bind_test.go`):
  Codex round-6 -race-Lauf in `client/internal/lazyconn/activity` zeigt
  bestehende Race-Condition im Test-Mock. Muss bereinigt werden bevor
  `TestActivityManager_HasPeer_RaceSafe` grün laufen kann. In den
  Stufe-6-Implementations-Commit aufgenommen.

## 8. Rollout-Plan

### 8.1 Branch + Commit-Struktur (v0.7 — 6 Commits inkl. HasPeer/Stufe 6)

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

**Stufen 0+2+3+4+5+6 (Panic-Recovery + Watchdog + Refaktor + Wiring + TransportSnapshot + HasPeer) — Phase-3.7i-Stack**:
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
  3. `lazyconn/manager: split state-mutation from blocking I/O, sequential close→listen` (Stufe 3 Refaktor: `transitionToActivityWatcherStateOnly` + `armActivityListener` Pair-Helper; `onPeerInactivityTimedOut` ruft Close synchron außerhalb Lock, danach Listener-Arm — kein async-close)
  4. `lazyconn/activity: add HasPeer(connID) read-only accessor` (Stufe 6 — neue API für Watchdog Listener-State-Detection)
  5. `lazyconn/manager: reconcile watchdog with two-case recovery (inactivity-stuck + activity-no-listener)` (Stufe 2 + 4: Watchdog-goroutine + `recoverInactivityStuck` + `recoverActivityNoListener` + Inflight-Dedupe + Test-Suite)
  6. `lazyconn/inactivity: count + log silent notifyChan drops` (Stufe 1 — kann als letzter Commit auch hier mitlaufen wenn Stufe 1 nicht parallel upstream-merged ist)
- Test-Coverage: ~32 neue Unit-Tests + 3 Integration-Tests (siehe Section 6.1)

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

## Implementation-Ready Status v0.7

**Codex round-6 Verdict (2026-05-23 spätabend)**: „v0.6 ist architektonisch
jetzt tragfähig. Ich sehe keinen verbleibenden BLOCKER im Kernplan." Drei
SHOULD-FIXes vor Implementierung — alle in v0.7 adressiert:

- ✅ **SHOULD-FIX 1 (watcherType-Typo)**: Pseudocode `expectedWatcher watcher`
  → `expectedWatcher watcherType` (manager.go:32). Stufe 2 angepasst.

- ✅ **SHOULD-FIX 2 (Remove/Exclude-Race nach Unlock)**: post-arm
  Re-Validate-Pattern eingeführt. Neuer Helper `peerStillManaged(pubKey,
  expectedConnID) bool` in Stufe 3; beide Recovery-Pfade rufen ihn nach
  `armActivityListener`. Bei Mismatch (Peer entfernt oder ConnID gewechselt):
  `activityManager.RemovePeer(pubKey)` cleanup. R14 neu erfasst.

- ✅ **SHOULD-FIX 3 (Stale Text R2/R6)**: R2 auf v0.6-Architektur
  (`recoverInactivityStuck` + `recoverActivityNoListener` +
  `transitionToActivityWatcherStateOnly`) umgestellt. R6 als v0.7 obsolet
  markiert (keine blocking calls mehr im Watchdog-Code-Pfad).

**Alle offenen Fragen aus 7.2 geschlossen** durch Codex round-6 Antworten:
- ConnectionMode-Split (Q1): nicht nötig
- HasPeer-Recheck Lock-Hierarchy (Q2): recheck/cleanup-Pattern ist richtig
- Case-c Hysteresis (Q3): separat lassen
- 120s Tick-Intervall (Q4): hartkodiert OK
- R12 Follow-up-Spec (Q5): separater Spec, nicht blocking

**Pre-Implementation-Task** (Section 7.3): existierender Race in
`mockEndpointManager` (`listener_bind_test.go`) muss bereinigt werden für
race-clean HasPeer-Tests. In Stufe-6-Commit aufgenommen.

**Nächste Schritte**:
1. Implementation in 6 Commits laut Section 8.1
2. Tests laut Section 6.1 (35 Unit-Tests + 3 Integration-Tests, alle mit
   `go test -race`)
3. Test-Harness-Race in `mockEndpointManager` als ersten Commit der Stufe
   6 bereinigen
4. Hardware-Soak laut Section 6.3 (S21 + dk20 + w11-test1, 72h ohne
   Force-Stop)
5. Post-Soak Codex round-7 Re-Review der konkreten Implementation
6. Bei OK: Upstream-PR an netbirdio/netbird (Stufe 1 separat,
   Stufen 0+2+3+4+5+6 als Phase-3.7i-Stack-PR)

---

## Codex-Review-Anfrage v0.6 (archiviert)

**Status der v0.5-Findings (Codex round-5 vom 2026-05-23 spätabend)**: alle
1 BLOCKER + 1 SHOULD-FIX adressiert:

- ✅ **BLOCKER (watcherActivity ohne Listener wird vom Watchdog nicht
  geheilt)**: Stufe 2 erweitert um zweite Recovery-Case. Stufe 6 neu:
  `activity.Manager.HasPeer(connID) bool` als read-only API. Watchdog-
  Phase-B snapshotet jetzt ALLE Peers (nicht nur watcherInactivity) mit
  `(pubKey, connID, expectedWatcher)`. Phase-C klassifiziert in zwei
  Trigger-Kategorien:
  
  - **Case-a** (existing): `watcherInactivity + iceDisc && relayDisc &&
    deltaRelay > 0` → `recoverInactivityStuck` (state-flip + listener-arm,
    HA-Defer-Check aktiv)
  - **Case-b** (NEU v0.6): `watcherActivity + !HasPeer(connID) + iceDisc
    && relayDisc` → `recoverActivityNoListener` (nur listener-arm, kein
    state-flip, keine HA-Defer; Peer ist bereits "soll active")
  - **Case-c**: `watcherActivity + HasPeer(connID)` → no-op (gesund oder
    transient mid-handshake)
  
  Damit ist der konkrete Stuck-Pfad nach hängendem PeerConnIdle in
  `onPeerInactivityTimedOut` (state-flip done, Close hängt, Listener nie
  armiert) **erstmals erkennbar und heilbar**.

- ✅ **SHOULD-FIX (Stale Text)**: R1 von `(deltaRelay > 0 || deltaICE > 0)`
  auf neue Zwei-Pfad-Beschreibung umgestellt. Commit-Plan: 6 Commits
  statt 5; Commit 3 von `state-only/listener/close-best-effort Triple`
  auf `state-only + listener-arm Pair` aktualisiert. Neuer Commit 4
  für Stufe 6 (`HasPeer` API).

**Verifizierte reale API-Pfade v0.6**:
- `activity.Manager.peers map[peerid.ConnID]listener` unter `Manager.mu`
  (activity/manager.go:30-39) — HasPeer ist sauberer Read-Wrapper.
- `lazyconn.PeerConfig.PeerConnID` (peerid.ConnID, stable across Lazy-Cycles).
- `lazyconn/manager.managedPeer.peerCfg.PeerConnID` als Bridge zwischen
  managedPeer-Schlüssel und activity.Manager-Lookup.

**Neue Fragen v0.6 die ich Codex bitte zu reviewen** (4):

1. **Case-a `isStuckPeer` für p2p-dynamic-ICE-only-Peers**: Case-a verlangt
   `iceDisc && relayDisc`. Bei p2p-dynamic-Peers ohne aktiven Relay-Watcher
   ist `relayDisc` strukturell true. Soll Case-a die ConnectionMode des
   Peers konsultieren? (Case-b hat das Problem nicht.)

2. **Case-b Re-Check-Granularität**: aktuell macht `recoverActivityNoListener`
   einen Re-Check via `m.activityManager.HasPeer(cfg.PeerConnID)` NACH
   Lock-Release. Soll das stattdessen direkt unter `m.managedPeersMu` +
   `activity.Manager.mu` als atomic check-and-arm laufen? Risiko: Lock-
   Hierarchy zwischen den beiden Mutexen.

3. **Case-c Hysteresis**: wenn ein Peer in `watcherActivity + hasListener +
   disconnected` länger als 5 min bleibt, ist das eventuell ein
   ICE-Backoff-Stuck-State. Soll Case-c eine Hysteresis-Auto-Heilung
   bekommen (`ResetIceBackoff + AttachICE`)? Aktuell out-of-scope.

4. **Klassifikation v0.6**: ist die Spec jetzt v0.6-implementierbar? Wenn
   nicht, was sind die verbleibenden BLOCKERs?

Nach Codex-OK v0.6: Implementation in 6 Commits (siehe Section 8.1),
Tests laut Section 6.1 (32 Unit-Tests + 3 Integration-Tests), Hardware-Soak
laut Section 6.3.

---

## Codex-Review-Anfrage v0.5 (archiviert)

**Status der v0.4-Findings (Codex round-4 vom 2026-05-23 abend)**: alle
1 BLOCKER + 1 BLOCKER/SHOULD-FIX + 1 SHOULD-FIX adressiert:

- ✅ **BLOCKER (Listener-first + async Close erzeugt conn.mu/managedPeersMu
  Deadlock)**: `closePeerConnBestEffort` aus Watchdog-Recovery-Pfad
  komplett entfernt. Watchdog macht nur noch state-flip + listener-arm.
  Begründung: empirischer Stuck-State zeigt `Disconnected, Disconnected`
  (Section 2.2 line 89) — Close ist bereits gelaufen; was fehlt ist nur
  der Activity-Listener. Code in Stufe 3 (recoverStuckPeer) hat den
  Close-Call entfernt; onPeerInactivityTimedOut nutzt synchronen Close →
  ListenerArm (v0.3-Ordering wiederhergestellt). Damit kann die v0.4-Race
  strukturell nicht entstehen.

- ✅ **BLOCKER/SHOULD-FIX (ICE- und Relay-Drops nicht vermischen)**:
  `isStuckPeer` signature von `(iceDisc, relayDisc, deltaRelay, deltaICE)`
  zu `(iceDisc, relayDisc, deltaRelay)` reduziert. Trigger nur via
  `relayDrops > 0`. ICE-Drops bleiben Telemetrie (Stufe 1 Counter), kein
  Watchdog-Trigger. Folge-Spec für ConnMgr-Watchdog (DetachICEForPeer-
  Pfad) als Option dokumentiert.

- ✅ **SHOULD-FIX (Stale Refs)**: `TestConsumerPanicCount_Accessible`
  (Test 3 in Stufe 0) entfernt. R4-Wording auf neuen Pair-Helper
  (`state-only + listener-arm`, kein I/O-after-unlock-Triple mehr)
  umgestellt.

**Neue Risiken erfasst**:
- R10 als obsolet markiert (kein async-close mehr).
- R12 NEU: `onPeerInactivityTimedOut`-Consumer-Stall bei hängendem Close
  bleibt out-of-scope für diesen Spec, mitigiert peer-level via Watchdog
  Listener-Arm. Strukturelle Lösung in Folge-Spec.
- R13 NEU: stale Conn-State nach listener-only-recovery wird durch
  Conn.Open() + ResetIceBackoff + AttachICE im normalen
  Activity-Wake-Pfad geheilt — identisch zu fresh-Activity-Trigger.

**Neue Fragen v0.5 die ich Codex bitte zu reviewen** (3 statt 4):

1. **`isStuckPeer` für p2p-dynamic-ICE-only-Peers**: die Heuristik
   verlangt `iceDisc && relayDisc`. Bei p2p-dynamic-Peers ohne aktiven
   Relay-Watcher ist `relayDisc` strukturell true. Soll der Watchdog die
   ConnectionMode konsultieren um falsche Trigger zu vermeiden?

2. **Default-Tick-Intervall 120 s** (Konstante vs. Konfig).

3. **R12 — Follow-up-Spec für `Conn.Close()` Cancellation**: soll dieser
   Watchdog-Spec als-ist mergen (PEER-LEVEL recovery reicht für den
   beobachteten S21-Failure-Mode), oder zuerst die Conn.Close()-Hang-
   Ursache strukturell beheben?

4. **Klassifikation v0.5**: ist die Spec jetzt v0.5-implementierbar? Wenn
   nicht, was sind die verbleibenden BLOCKERs?

Nach Codex-OK v0.5: Implementation in 5 Commits (1 Refaktor + 1 Stufe 0 +
1 Stufe 1 + 1 Stufen 2+3+4 + 1 Stufe 5), Tests laut Section 6.1 (28
Unit-Tests + 2 Integration-Tests), Hardware-Soak laut Section 6.3.

---

## Codex-Review-Anfrage v0.4 (archiviert)

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
