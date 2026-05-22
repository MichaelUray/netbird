---
status: spec v0.2 / pre-implementation review (round 2)
target-branch: see Section 8.1 — Stufe 1 separat upstream-fähig, Stufe 2+3 auf Phase-3.7i-Stack
related-work: pr/mgmt-stream-keepalive, pr/mgmt-stream-watchdog, feat/force-relay-flag, test/plan1+plan2-combined
review-status: 2× Codex pre-review — v0.1 returned with 2 BLOCKERs + 3 SHOULD-FIXes, addressed in v0.2
changelog:
  - v0.1 (2026-05-22 vormittag): Initial spec, Code-Refs verifiziert, Buffer-Größe 1 als kritischer Befund
  - v0.2 (2026-05-22 nachmittag): Codex-Korrekturen integriert:
    * Section 4.2 abgeschwächt: notifyChan-drop ist Robustheits-Mangel, nicht alleinige
      Ursache für 33h-Stuck. Doppelter Absatz entfernt.
    * Stufe-2-Pseudocode komplett ersetzt durch reale APIs + lock-sparsame Strategie
    * RecoverPeerToIdle cross-package-Call durch interne Lazy-Manager-Refaktor ersetzt
    * Panic-Resilience für Manager.Start() als Stufe 0 vorgezogen
    * Branch-Basis differenziert: Stufe 1 upstream-fähig, Stufe 2+3 auf Phase-3.7i-Stack
    * Risiko-Liste + Test-Plan um Codex-Findings erweitert (R6, R7, Tests 6 + 7)
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

Drei Hypothesen mit unterschiedlicher Plausibilität:

1. **Consumer-goroutine tot durch unbehandelten Panic** (Codex-Befund v0.2,
   **am wahrscheinlichsten**): `lazyconn.Manager.Start()` (manager.go:173) hat
   **kein** `defer recover()`. Eine panic in `onPeerActivity` oder
   `onPeerInactivityTimedOut` (z.B. durch nil-deref bei concurrent map-write,
   oder durch Panik in einem davon aufgerufenen `PeerConnIdle`/`MonitorPeerActivity`)
   beendet die ganze Start-Goroutine **dauerhaft**. Danach läuft `inactivity.Manager`
   weiter, schickt Events auf `inactivePeersChan`, aber niemand liest mehr. Der
   Channel füllt sich nach genau 1 Event und dropt dann alle weiteren silent.
   **Das passt perfekt zum 33h-Stuck-Symptom.**

2. **`onPeerInactivityTimedOut` aktiv blockierend**: per Code-Kommentar in
   manager.go:656 ist `PeerConnIdle` „blocking operation, potentially can be
   optimized". Wenn ein einzelner `PeerConnIdle`-Aufruf hängt (z.B. durch
   Wireguard-Tunnel-Removal das im Kernel auf Timeout läuft), blockiert die ganze
   Consumer-Schleife. Sub-Hypothese: solche Blockaden lösen sich unter Android Doze
   nicht mehr von selbst, weil der WG-Userspace-Bind im Doze-State nicht mehr feuert.

3. **Goroutine-Stack-Korruption durch Doze-Resume**: weniger plausibel, aber denkbar
   dass eine vom OS pausierte Goroutine bei Wake-up in einem inkonsistenten Stack-
   State zurückkommt und in einer infinite-loop oder deadlock landet.

Für alle drei Hypothesen ist das **Symptom dasselbe**: notifyChan-Drops sind silent,
der Lazy-Manager bekommt das nicht mit, kein Watchdog erkennt das, der Peer ist
stuck. → Der Fix muss BEIDES adressieren: (a) Consumer-Tod verhindern bzw.
detektieren, und (b) Stuck-State-Reconciliation triggern.

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

#### Stufe 0: Panic-Recovery für Manager.Start
**Datei**: `client/internal/lazyconn/manager/manager.go` (line 173 `Start`)
**Scope**: ~20 LOC + 1 atomic counter
**Begründung**: Codex' Befund H4 — `Start()` hat aktuell kein `defer recover()`.
Eine panic in `onPeerActivity` oder `onPeerInactivityTimedOut` killt die Consumer-
Goroutine dauerhaft. Ohne diesen Fix ist Stufe 2 nutzlos, weil der Watchdog selbst
auch über die `managedPeers` map operiert und panics aus demselben Pfad sehen kann.

Konkret (Pseudocode):
```go
// existing real signature:
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
            m.consumerPanicCount.Add(1)
            stack := debug.Stack()
            log.Errorf("lazyconn/manager: panic in onPeerActivity (peer=%v): %v\nstack:\n%s",
                peerConnID, r, stack)
        }
    }()
    m.onPeerActivity(peerConnID)
}

// analog safeOnPeerInactivityTimedOut
```

Plus exposed counter `m.ConsumerPanicCount() uint64` für Watchdog-Detection +
Telemetrie.

Tests:
- `TestManagerStart_PanicInOnPeerActivity_DoesNotKillConsumer`: injiziere eine
  Bedingung die zu panic in `onPeerActivity` führt, assert dass `Start()` weiterläuft
  und folgende Events noch verarbeitet werden.

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

#### Stufe 2: Lock-sparsamer Reconcile-Watchdog
**Datei**: `client/internal/lazyconn/manager/manager.go` (neue Methode am `Manager`)
**Scope**: ~120 LOC + 1 Goroutine + 1 Ticker
**Codex-Korrektur v0.2**: Snapshot **unter** Lock nehmen, Lock freigeben, Recovery
**außerhalb** Lock ausführen. Reale APIs verwenden statt fabrizierter.

Verfügbare reale APIs (verifiziert in v0.2):
- `m.managedPeersByConnID` (`map[ConnID]*managedPeer`, geschützt durch
  `managedPeersMu`) gibt zugriff auf jeden peer's `expectedWatcher` und
  `peerCfg.PublicKey`
- `m.inactivityManager.DropCounters()` (neu in Stufe 1) gibt absolute drop counts
- `m.inactivityManager.ConsumerPanicCount()` (neu in Stufe 0) gibt liveness signal
- `m.peerStore.PeerConn(pubKey) -> *peer.Conn, bool` gibt die Conn auf der man
  ICE/Relay-Status lesen kann (per `conn.GetICEState()` / `conn.GetRelayState()` —
  bitte verifizieren, ggf. müssen wir read-only Accessor exponieren)

Strategie:

```go
func (m *Manager) Start(ctx context.Context) {
    defer m.close()
    if m.inactivityManager != nil {
        go m.inactivityManager.Start(ctx)
    }
    go m.runReconcileWatchdog(ctx)   // ← NEU

    for {
        select {
        case <-ctx.Done(): return
        case peerConnID := <-m.activityManager.OnActivityChan:
            m.safeOnPeerActivity(peerConnID)
        case peerIDs := <-m.inactivityManager.InactivePeersChan():
            m.safeOnPeerInactivityTimedOut(peerIDs)
        }
    }
}

func (m *Manager) runReconcileWatchdog(ctx context.Context) {
    ticker := time.NewTicker(defaultReconcileInterval)  // 120s
    defer ticker.Stop()

    var lastRelayDrops, lastICEDrops uint64
    for {
        select {
        case <-ctx.Done(): return
        case <-ticker.C:
            // Step 1: read drop counters + panic count (no lock needed, atomic)
            relayDrops, iceDrops := m.inactivityManager.DropCounters()
            panicCount := m.ConsumerPanicCount()
            deltaRelay := relayDrops - lastRelayDrops
            deltaICE := iceDrops - lastICEDrops
            lastRelayDrops, lastICEDrops = relayDrops, iceDrops

            // Step 2: under lock, snapshot pubkeys of inactivity-watched peers
            //         that have not seen state transition since last tick
            m.managedPeersMu.Lock()
            stuckCandidates := make([]string, 0)
            for _, mp := range m.managedPeersByConnID {
                if mp.expectedWatcher != watcherInactivity {
                    continue
                }
                stuckCandidates = append(stuckCandidates, mp.peerCfg.PublicKey)
            }
            m.managedPeersMu.Unlock()

            // Step 3: for each candidate, query peerStore+conn outside lock
            //         (cross-package call, may block — that's ok now, no lock held)
            for _, pubKey := range stuckCandidates {
                conn, ok := m.peerStore.PeerConn(pubKey)
                if !ok { continue }

                // Heuristic: peer is stuck if BOTH:
                //   (a) deltaRelay > 0 → we are losing events on the channel
                //   (b) connection is in disconnected state
                //   (c) panicCount > 0 OR conn has been idle for ≥ 2× threshold
                if !isStuckPeer(conn, deltaRelay, panicCount, m.inactivityThreshold) {
                    continue
                }

                log.Warnf("lazy watchdog: detected stuck peer %s "+
                    "(relayDrops=Δ%d, panicCount=%d) — forcing transition to activity watcher",
                    pubKey, deltaRelay, panicCount)

                // Step 4: trigger recovery via existing API (DeactivatePeer is the
                //         closest match, but it expects ConnID and uses different
                //         path. We extract a new internal helper that mirrors
                //         onPeerInactivityTimedOut semantics, callable from outside
                //         the Start-Loop. See Stufe 3.)
                m.recoverStuckPeer(pubKey)
            }
        }
    }
}
```

**Lock-Strategie** (explizit per Codex-Feedback):
- `m.managedPeersMu.Lock()` nur für Snapshot der pubkeys (ms-Bereich)
- Cross-package-Calls (`peerStore.PeerConn`, ggf. Wireguard-Probes) **außerhalb** Lock
- `recoverStuckPeer` (siehe Stufe 3) führt seine eigene Lock-Sequenz lock-sparsam

**Offene Frage Q5 v0.2** (neu, an Codex): Welche reale API zum Lesen von ICE/Relay-
State auf einem `*peer.Conn` ist read-safe ohne weiteren Lock? Aktuell vermute ich
`conn.dumpState`-bezogene Methoden oder den `statusRecorder`. Bitte den richtigen
Pfad empfehlen.

Tests:
- `TestReconcileWatchdog_HoldsLockOnlyForSnapshot`: instrumentiere `peerStore.PeerConn`
  mit Sleep, assert dass `managedPeersMu` während des Sleep **nicht** gehalten ist
  (kann andere goroutine inzwischen `AddPeer` machen).
- `TestReconcileWatchdog_RecoveryActionRunsOutsideLock`: assert dass
  `recoverStuckPeer` während Watchdog-Lauf einen anderen Lazy-Manager-Caller nicht
  blockiert.
- `TestReconcileWatchdog_DropCounterAloneIsNotTrigger`: drop-Counter steigt, aber
  Peer-Conn ist healthy → keine Heilung (Mitigation für R5).
- `TestReconcileWatchdog_RespectsHA`: shouldDeferIdleForHA aus
  onPeerInactivityTimedOut muss auch hier gelten.

#### Stufe 3: Internes Heilungs-Helper statt cross-package-Call
**Datei**: `client/internal/lazyconn/manager/manager.go` (Refaktor + neue Methode)
**Scope**: ~30 LOC Refaktor + ~30 LOC neue Methode

**Codex-Korrektur v0.2**: `RecoverPeerToIdle` in `conn_mgr.go:578` ist ConnMgr-facing.
Ein direkter Call vom `lazyconn/manager` zurück in `conn_mgr` wäre falsche
Abhängigkeitsrichtung (lazyconn ist unter conn_mgr im Dependency-Graph).

Stattdessen: in `manager.go` extrahiere die State-Transition-Logik aus
`onPeerInactivityTimedOut` (line 633ff: `PeerConnIdle` + `expectedWatcher=activity`
+ `RemovePeer` + `MonitorPeerActivity`) in eine private Methode
`transitionToActivityWatcherLocked(mp *managedPeer)`. Beide Caller (existierender
`onPeerInactivityTimedOut` und neuer `recoverStuckPeer`) nutzen sie.

```go
// transitionToActivityWatcherLocked moves a managed peer from
// inactivity-watcher to activity-watcher. Caller MUST hold m.managedPeersMu.
// Mirrors the body of onPeerInactivityTimedOut (manager.go:653-672) so both
// the normal idle-timeout path and the watchdog stuck-recovery path execute
// the same state machine transition.
func (m *Manager) transitionToActivityWatcherLocked(mp *managedPeer) {
    mp.peerCfg.Log.Infof("transition to watcherActivity (from %s)", mp.expectedWatcher)
    m.peerStore.PeerConnIdle(mp.peerCfg.PublicKey)
    mp.expectedWatcher = watcherActivity
    m.inactivityManager.RemovePeer(mp.peerCfg.PublicKey)
    if err := m.activityManager.MonitorPeerActivity(*mp.peerCfg); err != nil {
        mp.peerCfg.Log.Errorf("failed to create activity monitor: %v", err)
    }
}

// recoverStuckPeer is the watchdog's recovery entry point. Looks up the
// managed peer by pubkey, validates it is still in watcherInactivity, then
// runs the same transition as onPeerInactivityTimedOut.
//
// Lock semantics: acquires m.managedPeersMu briefly. The blocking
// PeerConnIdle inside transitionToActivityWatcherLocked is still under
// lock. Codex-Feedback v0.2: this is acceptable IF watchdog tick interval
// (120s) >> longest expected PeerConnIdle latency. If profiling shows
// long PeerConnIdle calls, follow-up patch to move that out of lock.
func (m *Manager) recoverStuckPeer(pubKey string) {
    m.managedPeersMu.Lock()
    defer m.managedPeersMu.Unlock()

    // Re-resolve: pubKey → ConnID → managedPeer (state may have changed)
    cfg, ok := m.managedPeers[pubKey]
    if !ok { return }
    mp, ok := m.managedPeersByConnID[cfg.PeerConnID]
    if !ok { return }

    if mp.expectedWatcher != watcherInactivity {
        // Already in activity watcher — likely real onPeerInactivityTimedOut
        // beat us to it. Idempotent no-op.
        return
    }

    // shouldDeferIdleForHA needs map (peerIDs map[string]struct{}) — wrap
    // single-peer call: we treat solo recovery as "only this peer is timing
    // out right now" which is identical to onPeerInactivityTimedOut for a
    // single-element batch.
    if m.shouldDeferIdleForHA(map[string]struct{}{pubKey: {}}, mp.peerCfg.PublicKey) {
        mp.peerCfg.Log.Infof("watchdog: defer recovery due to active HA group peers")
        return
    }

    m.transitionToActivityWatcherLocked(mp)
}
```

Refaktor von `onPeerInactivityTimedOut`:
```go
func (m *Manager) onPeerInactivityTimedOut(peerIDs map[string]struct{}) {
    m.managedPeersMu.Lock()
    defer m.managedPeersMu.Unlock()
    for peerID := range peerIDs {
        // ... existing lookup logic ...
        if mp.expectedWatcher != watcherInactivity { continue }
        if m.shouldDeferIdleForHA(peerIDs, mp.peerCfg.PublicKey) { continue }
        mp.peerCfg.Log.Infof("connection timed out")
        m.transitionToActivityWatcherLocked(mp)   // ← replaces 4 inline statements
    }
}
```

Damit haben beide Pfade dieselbe State-Transition-Implementierung. Refaktor ist
test-coverage-erhaltend (alle bestehenden Tests zu `onPeerInactivityTimedOut`
greifen weiter, plus neue Tests für `recoverStuckPeer`).

Tests:
- `TestRecoverStuckPeer_HappyPath`: peer in watcherInactivity, recoverStuckPeer →
  watcherActivity.
- `TestRecoverStuckPeer_AlreadyActivity_NoOp`: peer schon in watcherActivity,
  recoverStuckPeer ist idempotent.
- `TestRecoverStuckPeer_RespectsHA`: HA-defer logik greift.
- `TestOnPeerInactivityTimedOut_StillWorks`: existierender Pfad nach Refaktor.

#### Stufe 4: Wiring (formerly Stufe 3 in v0.1)
**Datei**: `client/internal/lazyconn/manager/manager.go` (Start-Methode)
**Scope**: 1 LOC

Watchdog-Goroutine wird in `Start()` gestartet (siehe Stufe 2 Pseudocode oben:
`go m.runReconcileWatchdog(ctx)`). Lebenszyklus identisch zum Start-Loop. Kein
separater Knopf, kein Config-Toggle — der Watchdog ist Teil des Lazy-Managers
und entweder beide laufen oder keiner.

→ **Anti-Codex-Empfehlung umgesetzt**: kein cross-package-Call von `conn_mgr.go`
in den Lazy-Manager.

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
7. `TestReconcileWatchdog_DetectsStuckPeer`: drop-delta > 0 + Peer disconnected +
   panicCount > 0 → recoverStuckPeer wird aufgerufen.
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
15. `TestTransitionToActivityWatcherLocked_HappyPath`: zentral getestet.
16. `TestRecoverStuckPeer_HappyPath`: peer in watcherInactivity →
    recoverStuckPeer → watcherActivity.
17. `TestRecoverStuckPeer_AlreadyActivity_NoOp`: peer schon in watcherActivity,
    Idempotenz.
18. `TestRecoverStuckPeer_RespectsHA`: HA-defer in Recovery-Pfad.
19. `TestOnPeerInactivityTimedOut_AfterRefactor`: existierender Test-Sweep auf
    `onPeerInactivityTimedOut` nach Refaktor unverändert grün.

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
| R1 | Watchdog false-positive: gesunder Peer wird gestört | Mittel | Multi-Faktor-Heuristik in `isStuckPeer` (drop-delta > 0 UND conn-disconnected UND panicCount/threshold) + `shouldDeferIdleForHA`-Check + Re-Check `expectedWatcher == watcherInactivity` nach Lock-Reacquire |
| R2 | Race zwischen `onPeerInactivityTimedOut` und Watchdog-Reconcile | Mittel | Beide nutzen `managedPeersMu`; Reconcile prüft `expectedWatcher == watcherInactivity` redundant nach Lock-Reacquire; `transitionToActivityWatcherLocked` shared zwischen beiden Pfaden |
| R3 | Watchdog stört intentionale LazyTimeout-Konfig (z.B. Admin setzt RelayTimeout=24h für long-idle peers) | Niedrig | Trigger basiert auf `dropDelta > 0` (Indikator dass Events verloren gehen) + Disconnected-State, NICHT auf reiner Idle-Zeit. Admin-Config wird respektiert |
| R4 | `PeerConnIdle` ist „blocking operation" (per Code-Kommentar) — Watchdog hält Lock während dieses Calls | **Hoch (Codex v0.2)** | **v0.2-Mitigation**: Lock nur für Snapshot der pubkeys gehalten. Cross-package-Calls (`peerStore.PeerConn`, ggf. Probe-Reads) **außerhalb** Lock. `recoverStuckPeer` macht eigenes Lock-Cycle: Lock → re-validate → `transitionToActivityWatcherLocked` → Unlock. Follow-up Patch wenn `PeerConnIdle`-Latenz Probleme macht. |
| R5 | notifyChan-Drop-Counter wächst monoton → keine echte Heilung | Niedrig | Counter ist Telemetrie, keine Korrektur-Logik; Heilung passiert via Watchdog. Test `TestReconcileWatchdog_DropCounterAloneIsNotTrigger` deckt diese Mitigation ab |
| **R6** | **Watchdog selbst hängt am selben Lock/blockierenden Pfad und heilt dadurch nichts** | **Hoch (Codex v0.2)** | Lock-sparsame Snapshot/Action-Trennung (siehe Stufe 2). Wenn `recoverStuckPeer` durch `PeerConnIdle`-Blockade dauerhaft hängt, würde der Watchdog-Loop dort stehen bleiben — **das wäre Total-Failure**. Mitigation Stufe 2: Watchdog-Recovery in eigener short-lived goroutine spawnen, sodass nächster Tick weitermacht auch bei Blockade |
| **R7** | **Panic im Watchdog-Loop killt sich selbst dauerhaft** (gleicher Bug wie für Start-Loop in Stufe 0) | **Mittel (Codex v0.2)** | `runReconcileWatchdog` braucht eigenes `defer recover()` + Re-Start-Loop, ODER: Panic-Wrapper um den inneren Tick-Handler analog zu Stufe 0 `safeOnPeerActivity` |

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

### 8.1 Branch + Commit-Struktur (v0.2 differenziert)

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

**Stufen 0+2+3+4 (Reconcile-Watchdog + Panic-Recovery + Refaktor + Wiring) — Phase-3.7i-Stack**:
- Base: muss auf einem Branch sitzen der den vollen Phase-3.7i-Lazy-Manager-
  Code-Stand hat (Zwei-Timer-Logik, expectedWatcher state machine, HA-defer-Logik)
- **Frage an Codex Q8 (neu)**: ist das `pr/c-phase3.7i-routing-prefs`,
  `pr/d-phase3.7i-network-controller`, `pr/e-phase3.7i-engine-glue`, oder ein
  späterer Stack-Branch der den vollen Lazy-Manager-Code hat? Memory
  `reference_netbird_open_fork_branches.md` listet pr/c/d/e als deferred wegen
  Proto-Tag-Konflikten — sind die der richtige Base? Oder
  `phase3.7i-runtime-bugfixes-v0.5`?
- Branch: `pr/g-phase3.7i-lazy-watchdog` (auf gewähltem Phase-3.7i-Base)
- Commits (4 separate commits für reviewbarkeit):
  1. `lazyconn/manager: recover() guard around Start consumer loop` (Stufe 0)
  2. `lazyconn/manager: extract transitionToActivityWatcherLocked helper` (Stufe 3 Refaktor — pure code-move, kein behavior-change)
  3. `lazyconn/manager: reconcile watchdog for stuck inactivity-state` (Stufe 2 + 4: Watchdog-goroutine + Recovery-Path + Test-Suite)
- Test-Coverage: 8 neue Unit-Tests (siehe Section 6.1)

Author + Committer für alle Commits:
`Michael Uray <25169478+MichaelUray@users.noreply.github.com>`. Keine
Co-Authored-By-Trailer.

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

## Codex-Review-Anfrage v0.2

**Status der v0.1-Findings**: Alle 2 BLOCKERs + 3 SHOULD-FIXes adressiert:
- ✅ BLOCKER 1 (Section 4.2 overclaimed + Duplikat): Section 4.2 abgeschwächt + Duplikat entfernt
- ✅ BLOCKER 2 (Stufe-2-Pseudocode nicht implementierbar + Lock-Problem): kompletter Rewrite Stufe 2, Lock-Strategie explizit, reale APIs
- ✅ SHOULD-FIX RecoverPeerToIdle cross-package: durch internen `transitionToActivityWatcherLocked`-Refaktor (Stufe 3) ersetzt
- ✅ SHOULD-FIX Panic-Recovery: neue Stufe 0 vor allem anderen
- ✅ SHOULD-FIX Branch-Basis: Stufe 1 separater Upstream-PR (`pr/h-lazyconn-notify-drop-visibility`), Stufen 0+2+3+4 auf Phase-3.7i-Stack-Base

**Neue Fragen v0.2 die ich Codex bitte zu reviewen**:

1. **Reale API für ICE/Relay-State auf `*peer.Conn`** (Q5 neu, Section 5.2 Stufe 2):
   Aktuell vermute ich `conn.dumpState`-bezogene Methoden oder einen
   `statusRecorder`-Pfad. Welche read-safe API zum Lesen von ICE/Relay-State auf
   einem `*peer.Conn` ist die richtige? Falls keine existiert: ist es OK einen
   neuen read-only Accessor `conn.IsDisconnected() bool` zu exponieren?

2. **`isStuckPeer`-Heuristik** (Stufe 2): die Multi-Faktor-Heuristik
   (drop-delta > 0 UND conn-disconnected UND (panicCount > 0 OR idle ≥ 2×threshold))
   — ist das die richtige Kombination? Soll `panicCount > 0` ein hard-Trigger sein
   (auch wenn Conn nicht disconnected) oder nur additiv?

3. **Watchdog-Recovery in eigener goroutine** (R6 Mitigation): aktueller Vorschlag
   ist `recoverStuckPeer` synchron im Tick-Handler laufen zu lassen. Wenn
   `PeerConnIdle` hängt, blockiert das den ganzen Tick-Loop. Alternative: pro
   stuck-Peer eine `go m.recoverStuckPeer(pubKey)` spawnen. Risiko: race conditions
   wenn mehrere Watchdog-Ticks vor erstem Recovery-Result feuern. Welche Strategie
   bevorzugt?

4. **Phase-3.7i-Stack-Base** (Q8 neu, Section 8.1): pr/c-phase3.7i-routing-prefs,
   pr/d-phase3.7i-network-controller, pr/e-phase3.7i-engine-glue,
   phase3.7i-runtime-bugfixes-v0.5 — welcher ist der richtige Base für Stufen
   0+2+3+4? Aktuell habe ich keinen klaren Pfad und der Code in
   test/plan1+plan2-combined ist die einzige Stelle wo der volle Phase-3.7i-Stack
   deployed läuft.

5. **`shouldDeferIdleForHA` mit single-Peer map**: in `recoverStuckPeer` (Stufe 3)
   übergebe ich der HA-Check-Funktion eine `map[string]struct{}{pubKey: {}}`.
   Das spiegelt das Verhalten von `onPeerInactivityTimedOut` für einen Single-Peer-
   Batch. Ist semantisch korrekt? Oder erwartet `shouldDeferIdleForHA` den ganzen
   Batch der gerade idle ist (für HA-Failover-Awareness über alle peers)?

6. **`PeerConnIdle`-Latency-Profiling**: bevor wir den Watchdog deployen, sollte
   ich `PeerConnIdle`-Latenz messen unter Last (z.B. mit 30 peers gleichzeitig)?
   Falls > Watchdog-Tick-Intervall, dann muss Stufe 2 die Recovery in eigener
   goroutine spawnen (siehe Q3). Wenn < 1s, dann reicht der synchrone Pfad.

7. **Test-Strategie für Consumer-Blockade**: gibt es im NetBird-Test-Framework
   eine etablierte Methode den Consumer im Lazy-Manager-Select-Loop zu blockieren
   (für Integration-Tests)? Alternativ: ein Test-Hook der den onPeerActivity-
   Handler durch einen blockierenden Mock ersetzt.

8. **Klassifikation v0.2**: ist die Spec jetzt v0.2-implementierbar? Wenn nicht,
   was sind die verbleibenden BLOCKERs?

Nach Codex-OK v0.2: Implementation in 4 Commits laut Section 8.1, Tests laut
Section 6.1 (19 Unit-Tests + 2 Integration-Tests), Hardware-Soak laut Section 6.3.
