# Phase-3.7i — p2p-lazy "orphan peer" never-disconnect bug

**Status:** v0.3.1 — IMPLEMENTATION-READY (Codex sign-off 2026-05-24, TDD-Hinweis eingearbeitet)
**Date:** 2026-05-24
**Author:** Michael Uray (MichaelUray)
**Related:** [`2026-05-22-phase37i-lazy-watchdog-spec.md`](2026-05-22-phase37i-lazy-watchdog-spec.md) (deployed), [`2026-05-24-phase37i-lazy-watchdog-impl.md`](2026-05-24-phase37i-lazy-watchdog-impl.md)
**Scope:** `client/internal/lazyconn/inactivity/manager.go` (single file fix), plus unit test
**NOT in scope:** Phase-2 two-timer behaviour change, server protocol change, p2p-dynamic mode

### Changelog
- **v0.3.1 (2026-05-24)** — Codex sign-off + TDD-Hinweis aus Round-2-Review:
  - §9 Commit-Granularität auf strikte TDD-Reihenfolge umgestellt (Test zuerst rot, dann Map-Refactor, dann checkStats-Fix). Alternative "Test + Fix gebündelt" als Note ergänzt.
- **v0.3 (2026-05-24)** — Codex review round 2 polish:
  - Alle relativen Markdown-Links auf `../../../client/...` korrigiert (Spec liegt in `docs/superpowers/plans/`, also 3 Ebenen bis Repo-Root).
  - §2.2: `watchdog.go` → `client/internal/lazyconn/manager/manager.go` (der Watchdog lebt im manager-Package, nicht in eigener Datei).
  - Changelog (v0.2-Einträge): `addedAt` → `firstSeenAt` nachgezogen.
  - §6 Locking-Text neutralisiert: kein Race-Freiheit-Claim mehr; explizit als "folgt bestehender no-sync-Konvention; transient fehlender Eintrag wird defensiv geloggt/übersprungen; race-clean = separater Mutex-Refactor".
- **v0.2 (2026-05-24)** — Codex review round 1 reingearbeitet:
  - Phase-2-Semantik (BLOCKER): Fix-Strategie umgestellt — `firstSeenAt` wird als **synthetisches `lastActive`** verwendet, danach läuft die bestehende Zwei-Timer-Logik unverändert. Damit funktioniert iceTimeout in Phase-2 automatisch korrekt.
  - WG-Stats-Semantik präzisiert: fehlender Eintrag heißt "kein ActivityRecorder-Eintrag", nicht zwingend "nie Bytes". `WGUSPConfigurer.UpdatePeer` legt bei Endpoint-Set bereits einen Eintrag mit `LastActivity=Now` an ([`usp.go:117-124`](../../../client/iface/configurer/usp.go#L117-L124) + [`activity.go:81`](../../../client/iface/bind/activity.go#L81)).
  - Test-Snippets korrigiert: `mockWgInterface` (nicht `mockWGIface`), bestehende `assert`-Konvention statt `require`, kein `time.Sleep` — `firstSeenAt`-Map direkt mit Vergangenheits-Wert seeden.
  - Locking-Text ehrlicher: kein "atomar"-Claim, sondern explizit "folgt der bestehenden no-mutex-Konvention für `interestedPeers`".
  - Branch-Strategie korrigiert: Spec-only auf `spec/phase37i-orphan-disconnect`, Implementierung später auf `pr/h-phase37i-orphan-disconnect` mit Base `pr/g-phase3.7i-lazy-watchdog` (nicht auf den Build-/Deploy-Zweig).
- **v0.1 (2026-05-24)** — initiale Spec.

---

## 1. Problem statement (Anwendersicht)

Auf einem Samsung Galaxy S21 mit dem Phase-3.7i-Lazy-Watchdog-Build (commit `74c46f2eb`) sind nach mehreren Stunden Laufzeit immer noch **deutlich mehr P2P-Verbindungen aktiv**, als die Server-seitige Konfiguration eigentlich erlauben sollte:

| NetBird-Server-Setting | Wert |
|------------------------|------|
| `lazy_connection_enabled` | `false` (Phase-2 global aus) |
| `legacy_lazy_fallback_enabled` | `true` |
| `legacy_lazy_fallback_timeout_seconds` | `300` (= 5 Minuten) |
| `p2p_timeout_seconds` | `180` |
| `relay_timeout_seconds` | `300` |

Der Erwartung des Users nach müssten **alle Legacy-Clients** im Mesh, die im Web-Interface auf p2p-lazy geschaltet sind und keinen Traffic austauschen, sich nach 5 Minuten Inaktivität trennen — egal ob sie jemals einen WireGuard-Handshake hatten oder nicht.

Beobachtet wird:

- **Class A** (p2p-lazy, hatte erfolgreichen WG-Handshake, dann idle): trennt sich korrekt nach 5 min. Logzeile: `connection timed out` (`manager.go:794`). **✓ kein Bug.**
- **Class B** (p2p-lazy, **niemals erfolgreicher WG-Handshake**, ICE permanent im Backoff): bleibt für **Stunden** im `interestedPeers`-Map der `inactivity.Manager`-Instanz, ohne dass jemals `inactivePeersChan` für ihn feuert. **✗ Bug.**
- **Class C** (p2p-lazy, relay-only): aktivitätsabhängig — wird kurz behandelt, ist aber nicht der primäre Fokus dieses Specs.

Konkretes Beispiel aus dem S21-Log (gekürzt):

```
peer 6Enrgb/koy3G8... = 9483C464049E-fwr-lunzamsee
  ICE backoff active (failure #33)
  state: stuck in "p2p connecting"
  inactivity manager log: "peer not found in wg stats"
  hours since added to inactivityManager: ~6h
  never crossed relayTimeout (5min) → never disconnected
```

User-Quote:
> Die Verbindung zur alten Legacy-Client muss auf jeden Fall auch getrennt werden, wenn hier kein Traffic stattfindet und P2P-lazy (legacy) über das Webinterface konfiguriert ist.

---

## 2. Root cause (Code-Pfad)

Datei: [`client/internal/lazyconn/inactivity/manager.go`](../../../client/internal/lazyconn/inactivity/manager.go)
Funktion: `(*Manager).checkStats`
Zeilenbereich: 266–294, bug auf Zeile 273–279.

```go
for peerID, peerCfg := range m.interestedPeers {
    lastActive, ok := lastActivities[peerID]
    if !ok {
        // when peer is in connecting state
        peerCfg.Log.Warnf("peer not found in wg stats")
        continue                                        // ← BUG
    }

    since := monotime.Since(lastActive)

    if m.iceTimeout > 0 && since > m.iceTimeout {
        ...
        iceIdle[peerID] = struct{}{}
    }
    if m.relayTimeout > 0 && since > m.relayTimeout {
        ...
        relayIdle[peerID] = struct{}{}
    }
}
```

### 2.1 Warum das ein Bug ist

`lastActivities` ist die per-peer-Map, die der WG-Interface-Treiber liefert (`m.iface.LastActivities()`). Im userspace-Pfad wird der Eintrag **bereits beim Endpoint-Set** angelegt:

- [`WGUSPConfigurer.UpdatePeer`](../../../client/iface/configurer/usp.go#L94-L126) ruft `c.activityRecorder.UpsertAddress(peerKey, addrPort)` auf, sobald ein `endpoint != nil` reingereicht wird.
- [`ActivityRecorder.UpsertAddress`](../../../client/iface/bind/activity.go#L67-L82) initialisiert dabei `record.LastActivity = monotime.Now()`.

Ein fehlender Eintrag in `LastActivities()` bedeutet also **präzise**: für diesen Peer wurde noch nie `UpdatePeer` mit einem nicht-`nil`-Endpoint aufgerufen. Das passiert in genau diesen Fällen:

1. Peer wurde gerade frisch zu `inactivity.Manager.AddPeer` hinzugefügt (`manager.go:471`/`575`), ICE/Handshake läuft noch, ein Endpoint ist noch nicht ausgehandelt.
2. ICE-Aufbau ist **dauerhaft fehlgeschlagen**, der ICE-Worker steckt im exponentiellen Backoff (`failure #33` im Beispiel) und wird **nie** einen erfolgreichen Endpoint produzieren.

Case 1 ist kurz (Sekunden bis wenige Minuten). Der originale Kommentar `// when peer is in connecting state` und das `continue` sind dafür gedacht.

Case 2 ist der pathologische Pfad: Der Peer steht permanent im `interestedPeers`-Map, das ICE-Backoff-Throttling im ConnMgr versucht und scheitert immer wieder. Da `lastActivities[peerID]` nie befüllt wird, wird `since` nie berechnet, und damit fällt der Peer **niemals** in `iceIdle` oder `relayIdle` — selbst wenn er seit Stunden ohne Traffic im Map sitzt.

`lazyconn.Manager` (= der Consumer von `inactivePeersChan`) bekommt also nie ein Disconnect-Signal für diesen Peer. Der ICE-Worker bleibt registriert, der Backoff läuft endlos weiter, und auf einem Mobilgerät ist das ein nicht-trivialer Battery-Drain und Mesh-State-Müll.

### 2.2 Abgrenzung zum bereits gefixten Watchdog (v0.7.2)

Der gerade deployte Phase-3.7i-Lazy-Watchdog (im `lazyconn.Manager` selbst, [`client/internal/lazyconn/manager/manager.go`](../../../client/internal/lazyconn/manager/manager.go) im Branch `pr/g-phase3.7i-lazy-watchdog`) deckt **zwei andere** stuck-states ab:

- **Case-a**: `inactivePeersChan` voll → `notifyChan` dropt → Consumer kriegt das Event nicht.
- **Case-b**: Consumer hat `Close()` aufgerufen, der WG-Removal hat eine Race, Peer bleibt halb-gehangen.

Beide setzen voraus, dass `checkStats` den Peer **als idle erkannt hat**. Das tut sie hier aber nicht — der Peer kommt nicht mal in `relayIdle`. Damit greift der Watchdog auch nicht (kein Drop-Event, kein Close-Hänger, einfach: nie ein Idle-Event in der gesamten Pipeline).

Dieser Bug ist also **strukturell unabhängig** vom Watchdog. Er liegt eine Ebene tiefer: im idle-detection-Pfad selbst.

---

## 3. Empirical evidence

### 3.1 S21-Logs (deployed binary `74c46f2eb`)

```
[lazyconn] peer 6Enrgb/koy3G8...: ICE backoff active (failure #33)
[lazyconn inactivity] peer 6Enrgb/koy3G8...: peer not found in wg stats
[lazyconn inactivity] peer 6Enrgb/koy3G8...: peer not found in wg stats   (repeated each minute)
... (no "peer relay idle" line ever for 6Enrgb/koy3G8...)
... (no "connection timed out" for 6Enrgb/koy3G8...)
```

Im Gegensatz dazu sieht Class-A einen sauberen Disconnect:

```
[lazyconn inactivity] peer XYZ...: peer relay idle since: 2026-05-24 11:42:13
[lazyconn manager]   peer XYZ...: connection timed out
[lazyconn manager]   peer XYZ...: closing connection
```

### 3.2 Server-Config (via Mgmt-DB / Webinterface)

```
lazy_connection_enabled: false
legacy_lazy_fallback_enabled: true
legacy_lazy_fallback_timeout_seconds: 300
p2p_timeout_seconds: 180
relay_timeout_seconds: 300
```

Der Client läuft also in Phase-1 p2p-lazy-Legacy-Mode. `inactivity.NewManager` wird mit `inactivityThreshold = 5min` aufgerufen (`relayTimeout = 5min`, `iceTimeout = 0`, ICE always-on).

### 3.3 Reproduzierbarkeit

Reproduktion auf einem beliebigen Client mit Phase-1-Lazy-Mode:

1. Sorge dafür, dass mindestens ein Peer im Mesh **nicht direkt erreichbar** ist (NAT, FW, oder einfach offline).
2. Der lokale ICE-Worker probiert P2P, scheitert, geht in Backoff.
3. `inactivity.Manager.AddPeer` wird trotzdem aufgerufen (Peer ist im managed-peers-Map).
4. Da WG nie einen Endpoint für den Peer sieht (ICE liefert keinen), fehlt der Eintrag in `LastActivities()`.
5. Nach 5 Minuten passiert: nichts. Nach 6 Stunden: immer noch nichts.

Auf dem S21 ist das aktuell mit `9483C464049E-fwr-lunzamsee` (LTE-Router in Lunzamsee, schlechte Konnektivität) zuverlässig zu sehen.

---

## 4. Proposed fix

### 4.1 Design-Strategie

Das `continue` für Case 1 (frisch hinzugefügter Peer, Handshake gerade unterwegs) muss in seiner Schutzwirkung erhalten bleiben. Wir dürfen nicht einfach jeden fehlenden Eintrag in `lastActivities` als "idle" werten — sonst killen wir alle Peers in den ersten Sekunden nach `AddPeer`, bevor ICE auch nur eine Chance hatte.

**Kern-Idee (v0.2):** Wir verwenden den `AddPeer`-Zeitpunkt als **synthetisches `lastActive`** für Peers, die noch nie einen WG-Endpoint-Eintrag erzeugt haben. Damit fällt der orphan-peer-Pfad in die **bestehende Zwei-Timer-Logik** rein — keine Sonder-Semantik nötig, Phase-1 und Phase-2 verhalten sich automatisch konsistent:

| Mode | iceTimeout | relayTimeout | Verhalten bei orphan peer |
|------|-----------|--------------|---------------------------|
| Phase-1 lazy (legacy) | 0 | 5 min | nach 5 min → `relayIdle` → full teardown |
| Phase-2 lazy (dynamic) | 3 min | 5 min | nach 3 min → `iceIdle` (Detach-Versuch, no-op falls nichts da), nach 5 min → `relayIdle` → full teardown |
| Beide-Timer-0 (inert) | 0 | 0 | nichts feuert (per Definition) |

Der `iceIdle`-Eintrag für einen orphan peer in Phase-2 ist semantisch korrekt: der ICE-Worker **existiert** ja (siehe `ICE backoff active failure #33`), er hat nur nie einen Endpoint produziert. Ein Detach-Signal an ihn ist legitim, auch wenn er anschließend mangels Endpoint nichts "zurückrollen" muss.

### 4.2 Konkrete Implementierung

**Schritt 1** — internes Tracking-Map in `inactivity.Manager`:

```go
type Manager struct {
    ...
    interestedPeers map[string]*lazyconn.PeerConfig
    firstSeenAt     map[string]monotime.Time   // NEW: per-peer monotonic timestamp when AddPeer was called
    ...
}
```

(Naming `firstSeenAt` — Codex-Vorschlag aus Round 1 übernommen; klarer als `addedAt`.)

**Schritt 2** — `newManager` initialisiert die Map; `AddPeer` / `RemovePeer` pflegen sie:

```go
func newManager(iface WgInterface, iceTimeout, relayTimeout time.Duration) *Manager {
    return &Manager{
        ...
        interestedPeers: make(map[string]*lazyconn.PeerConfig),
        firstSeenAt:     make(map[string]monotime.Time),   // NEW
        ...
    }
}

func (m *Manager) AddPeer(peerCfg *lazyconn.PeerConfig) {
    if m == nil {
        return
    }
    if _, exists := m.interestedPeers[peerCfg.PublicKey]; exists {
        return
    }
    peerCfg.Log.Infof("adding peer to inactivity manager")
    m.interestedPeers[peerCfg.PublicKey] = peerCfg
    m.firstSeenAt[peerCfg.PublicKey] = monotime.Now()   // NEW
}

func (m *Manager) RemovePeer(peer string) {
    if m == nil {
        return
    }
    pi, ok := m.interestedPeers[peer]
    if !ok {
        return
    }
    pi.Log.Debugf("remove peer from inactivity manager")
    delete(m.interestedPeers, peer)
    delete(m.firstSeenAt, peer)                         // NEW
}
```

**Schritt 3** — `checkStats` verwendet `firstSeenAt` als synthetisches `lastActive`, danach läuft die bestehende Logik unverändert:

```go
for peerID, peerCfg := range m.interestedPeers {
    lastActive, ok := lastActivities[peerID]
    if !ok {
        // No ActivityRecorder entry yet (no WG endpoint ever set for this
        // peer — typically: ICE still negotiating, or ICE backoff stuck).
        // Treat firstSeenAt as a synthetic last-activity so the existing
        // two-timer logic below applies uniformly:
        //   - Phase-1 (iceTimeout=0, relayTimeout=5m):
        //       fires relayIdle after 5m → teardown.
        //   - Phase-2 (iceTimeout=3m,  relayTimeout=5m):
        //       fires iceIdle after 3m, relayIdle after 5m.
        seen, seenOK := m.firstSeenAt[peerID]
        if !seenOK {
            // Defensive: should never happen. AddPeer + RemovePeer keep
            // both maps in lockstep under the existing single-goroutine
            // caller convention (see §6). Log + skip if it ever does.
            peerCfg.Log.Warnf("inactivity: peer in interestedPeers without firstSeenAt entry")
            continue
        }
        lastActive = seen
        // Fall through to the shared two-timer logic below.
    }

    since := monotime.Since(lastActive)

    if m.iceTimeout > 0 && since > m.iceTimeout {
        peerCfg.Log.Debugf("peer ICE idle since: %s", checkTime.Add(-since).String())
        iceIdle[peerID] = struct{}{}
    }
    if m.relayTimeout > 0 && since > m.relayTimeout {
        peerCfg.Log.Infof("peer relay idle since: %s", checkTime.Add(-since).String())
        relayIdle[peerID] = struct{}{}
    }
}
```

Das `Warnf("peer not found in wg stats")` aus dem alten Pfad entfällt — die neue Logik führt zum korrekten `peer relay idle since: …`-Log auf der Standardstrecke. Falls noch ein Debug-Hinweis gewünscht ist (z. B. zur Diagnose von "war der teardown auf der orphan-Strecke?"), kann ein einzeiliger `Debugf("treating firstSeenAt as lastActive (no WG endpoint yet)")` direkt vor dem Fall-through stehen.

### 4.3 Alternative Variante (nicht empfohlen)

`AddedAt monotime.Time` direkt in `lazyconn.PeerConfig` mitaufnehmen und vom `lazyconn.Manager`-Code beim Erzeugen setzen. **Nachteil:** API-Bruch im `lazyconn`-Paket, mehr Touch-Points, mehr Test-Updates, und das Feld wäre für andere Konsumenten von `PeerConfig` semantisch unklar. → **Verworfen** zugunsten Variante 4.2.

### 4.4 Nicht-Lösungen (ausgeschlossen)

- ❌ Einfach das `continue` entfernen und alle peers ohne `lastActivities`-Eintrag als idle markieren: tötet frische Peers in den ersten Sekunden.
- ❌ `lastActivities` mit Null-Werten initialisieren beim AddPeer: greift in die WG-Interface-Schicht ein, semantisch falsch (WG hat den Peer wirklich nicht gesehen).
- ❌ Watchdog erweitern: Watchdog operiert auf Drop-Counter + Close-Race-Symptomen, die hier gar nicht entstehen. Falsche Abstraktionsebene.

---

## 5. Phase-2 (Zwei-Timer-Modus) — Verhalten

Mit der v0.2-Strategie aus §4.1 ist hier nichts mehr "special". Der orphan peer fällt durch denselben Code-Pfad wie ein normaler Peer mit `lastActive = firstSeenAt`:

- `iceTimeout > 0 && since > iceTimeout` → `iceIdle[peerID]` befüllt.
- `relayTimeout > 0 && since > relayTimeout` → `relayIdle[peerID]` befüllt.

Der Consumer (`lazyconn.Manager`) verarbeitet `iceIdle` und `relayIdle` in seinen bestehenden Pfaden. Auf einem orphan peer, der nie einen Endpoint hatte, ist `DetachICE` auf der iceIdle-Strecke effektiv ein No-Op (es gibt keinen aktiven ICE-Worker mit Endpoint, der zu detachen wäre — der Backoff-Loop endet beim nächsten Reconcile-Tick durch den Full-Close ohnehin). Das ist semantisch korrekt und passt zur bestehenden Robustheits-Erwartung von `DetachICE`.

---

## 6. Race-Conditions und Konkurrenz-Sicht

`checkStats` läuft in der `inactivity.Manager.Start`-Goroutine. `AddPeer` / `RemovePeer` werden vom `lazyconn.Manager` aus aufgerufen — also von **außerhalb** dieser Goroutine. Der bestehende Code hat bereits **keine gemeinsame Synchronisierung** für `interestedPeers`; das ist eine Annahme über die Aufrufer-Disziplin im `lazyconn`-Paket, nicht eine garantierte Race-Freiheit.

Die neue `firstSeenAt`-Map folgt **derselben Konvention** ohne neuen Mutex: die zwei `delete`-Aufrufe in `RemovePeer` bzw. die zwei `=`-Zuweisungen in `AddPeer` sind nicht atomar zueinander, und auch nicht atomar gegenüber einem konkurrierenden `checkStats`-Lauf. Falls `interestedPeers` einen Peer enthält, dessen `firstSeenAt`-Eintrag (transient) fehlt, **loggt** der `!ok`-Pfad in §4.2 das defensiv und **überspringt** den Peer für diesen Tick — beim nächsten Tick ist die Map dann konsistent.

Eine **race-clean** Lösung wäre ein einzelner `sync.Mutex` auf `interestedPeers` + `firstSeenAt`. Das ist ein **separater Mutex-Refactor**, der nicht in diesen Bugfix-Spec gehört: er würde auch den heutigen, unveränderten Lese-Pfad in `checkStats` betreffen und ist damit kein orphan-disconnect-Thema.

---

## 7. Test plan

### 7.1 Unit tests (neu) — gegen den realen Test-Harness in `manager_test.go`

Datei: `client/internal/lazyconn/inactivity/manager_test.go`

Bestehender Harness (verifiziert: [`manager_test.go:28-34`](../../../client/internal/lazyconn/inactivity/manager_test.go#L28-L34)):

```go
type mockWgInterface struct {
    lastActivities map[string]monotime.Time
}
func (m *mockWgInterface) LastActivities() map[string]monotime.Time { return m.lastActivities }
```

Bestehende Test-Konvention: `assert.*` aus `testify/assert`. **Kein** `require`-Import nötig.

```go
// Orphan peer = peer that never produced a WG-stats entry (no endpoint
// from ICE). Must be marked relayIdle once firstSeenAt is older than
// relayTimeout, even though lastActivities has no key for it.
func TestCheckStats_OrphanPeerMarksRelayIdleAfterTimeout(t *testing.T) {
    iface := &mockWgInterface{lastActivities: map[string]monotime.Time{}}
    relayTO := time.Minute
    mgr := newManager(iface, 0, relayTO)   // Phase-1 lazy

    peerKey := "orphanPeer"
    cfg := &lazyconn.PeerConfig{
        PublicKey: peerKey,
        Log:       log.WithField("peer", peerKey),
    }
    mgr.AddPeer(cfg)

    // Seed firstSeenAt into the past — deterministic, no time.Sleep.
    mgr.firstSeenAt[peerKey] = monotime.Time(int64(monotime.Now()) - int64(2*relayTO))

    iceIdle, relayIdle, err := mgr.checkStats()
    assert.NoError(t, err)
    assert.Empty(t, iceIdle, "iceIdle must stay empty when iceTimeout=0")
    assert.Contains(t, relayIdle, peerKey,
        "orphan peer must be marked relayIdle after relayTimeout")
}

// Fresh orphan peer (within karenzfrist) must not fire any timer.
func TestCheckStats_OrphanPeerSilentBeforeTimeout(t *testing.T) {
    iface := &mockWgInterface{lastActivities: map[string]monotime.Time{}}
    mgr := newManager(iface, 0, 10*time.Minute)

    peerKey := "freshOrphan"
    mgr.AddPeer(&lazyconn.PeerConfig{PublicKey: peerKey, Log: log.WithField("peer", peerKey)})
    // firstSeenAt left at the "now" value newly written by AddPeer.

    iceIdle, relayIdle, err := mgr.checkStats()
    assert.NoError(t, err)
    assert.Empty(t, iceIdle)
    assert.Empty(t, relayIdle, "must not fire within karenzfrist")
}

// Phase-2 mode: orphan peer must hit iceIdle first, then relayIdle.
func TestCheckStats_OrphanPeerTwoTimerPhase2(t *testing.T) {
    iface := &mockWgInterface{lastActivities: map[string]monotime.Time{}}
    iceTO := time.Minute
    relayTO := 5 * time.Minute
    mgr := newManager(iface, iceTO, relayTO)

    peerKey := "orphan2"
    mgr.AddPeer(&lazyconn.PeerConfig{PublicKey: peerKey, Log: log.WithField("peer", peerKey)})

    // 2min into the past → past iceTimeout, but well below relayTimeout.
    mgr.firstSeenAt[peerKey] = monotime.Time(int64(monotime.Now()) - int64(2*time.Minute))
    iceIdle, relayIdle, _ := mgr.checkStats()
    assert.Contains(t, iceIdle, peerKey, "must mark iceIdle after iceTimeout")
    assert.NotContains(t, relayIdle, peerKey, "must not mark relayIdle yet")

    // 6min into the past → past both timers.
    mgr.firstSeenAt[peerKey] = monotime.Time(int64(monotime.Now()) - int64(6*time.Minute))
    iceIdle, relayIdle, _ = mgr.checkStats()
    assert.Contains(t, iceIdle, peerKey)
    assert.Contains(t, relayIdle, peerKey, "must mark relayIdle after relayTimeout")
}

// RemovePeer must clean up the firstSeenAt entry too.
func TestRemovePeer_CleansFirstSeenAt(t *testing.T) {
    iface := &mockWgInterface{lastActivities: map[string]monotime.Time{}}
    mgr := newManager(iface, 0, time.Minute)

    peerKey := "p1"
    mgr.AddPeer(&lazyconn.PeerConfig{PublicKey: peerKey, Log: log.WithField("peer", peerKey)})
    _, present := mgr.firstSeenAt[peerKey]
    assert.True(t, present, "AddPeer must populate firstSeenAt")

    mgr.RemovePeer(peerKey)
    _, present = mgr.firstSeenAt[peerKey]
    assert.False(t, present, "RemovePeer must clean up firstSeenAt")
}
```

Anmerkungen zur Test-Form (Codex-Round-1-Feedback eingearbeitet):
- **Harness-Name korrekt:** `mockWgInterface` (inactivity-Package), nicht `mockWGIface` (das ist im `manager`-Package).
- **Konvention:** `assert.*` ohne `require` — passt zum bestehenden File.
- **Deterministisch:** kein `time.Sleep`, stattdessen `mgr.firstSeenAt[peerKey] = past`. Schneller, robuster, identische Aussage.

### 7.2 Integration-Test (manual, auf S21)

1. Build mit Fix erzeugen, Hash notieren.
2. APK auf S21 deployen.
3. NetBird aktivieren, App im Foreground halten.
4. `adb logcat -v time | grep -E "lazyconn|connection timed out|peer relay idle"` mitschneiden.
5. Erwartung nach max. 7 Minuten Laufzeit (5min relayTimeout + 1min checkInterval-Jitter + 1min Sicherheit):
   - Für jeden `interestedPeers`-Eintrag ohne WG-Stats-Eintrag taucht eine Logzeile `peer relay idle since: …` auf.
   - Direkt danach: `connection timed out` von `lazyconn.Manager`.
   - `netbird status -d`-Ausgabe (via `adb shell`) zeigt deutlich **weniger** persistente p2p-connecting/backoff-Peers als vor dem Fix.
6. Reproduzierbarkeit: nach 30 min wieder Status prüfen, Peer-Count darf nicht "creepen".

### 7.3 Regressions-Risiko

Der Fix verändert das Verhalten **nur** für den Pfad `!ok` (WG-Stats fehlt). Der existierende `since`-Pfad bleibt byte-identisch — er rückt im neuen Code nur ein Indentation-Level höher, weil das `continue` durch Fall-through ersetzt wird. Risiko betrifft also nur Peers, die **kürzer als die Timeouts** im `interestedPeers`-Map sind: diese werden weiter durch `monotime.Since(firstSeenAt) < timeout` geschützt, also kein Regressions-Vektor gegenüber heute.

---

## 8. Beziehung zum Phase-3.7i-Watchdog-Stack

Der bereits deployte Watchdog (`pr/g-phase3.7i-lazy-watchdog`, commit `fc0f98388`, Binary `74c46f2eb`) bleibt **unverändert** und ist **komplementär** zu diesem Fix:

| Stuck-Pfad | Erkennung | Fix |
|------------|-----------|-----|
| Notify-channel-Drop | Watchdog Case-a (drop-counter) | Watchdog (deployed) |
| Post-Close-Hänger | Watchdog Case-b (peerStillManaged) | Watchdog (deployed) |
| **Orphan peer (never handshake)** | **`checkStats` `!ok`-Pfad** | **Diese Spec** |

Damit deckt der Stack alle drei aktuell bekannten Phase-3.7i-Disconnect-Lücken ab.

---

## 9. Branch- und PR-Strategie (für später, nach Codex-Freigabe)

**Korrektur gegenüber v0.1** (Codex-Empfehlung): nicht auf den Build-/Deploy-Zweig committen.

- **Spec-only:** `spec/phase37i-orphan-disconnect` (nur dieses Markdown-File).
- **Implementierung später:** `pr/h-phase37i-orphan-disconnect`.
  - **Base:** `pr/g-phase3.7i-lazy-watchdog` (dort enthält `inactivity/manager.go` bereits den `DropCounters`/Watchdog-Kontext; jeder andere Base produziert unnötige Konflikte).
- **Commit-Granularität (TDD-Reihenfolge, Codex-Hinweis Round 2):**
  1. `inactivity: failing test for orphan-peer disconnect (red)` — Red-Test-Commit: die vier Unit-Tests aus §7.1 zuerst hinzufügen. Sie compilieren noch nicht (`firstSeenAt`-Feld fehlt) bzw. schlagen fehl (`!ok`-Pfad ist `continue`). Erwartet: `go test ./client/internal/lazyconn/inactivity` rot.
  2. `inactivity: track firstSeenAt per peer in Manager` — Feld + Map einführen, `AddPeer`/`RemovePeer` pflegen. Damit compilieren die Tests, einer (`TestRemovePeer_CleansFirstSeenAt`) wird grün. Die orphan-disconnect-Tests bleiben rot, weil `checkStats` noch nicht angepasst.
  3. `inactivity: mark orphan peers idle using firstSeenAt as synthetic last-activity` — der eigentliche Fix in `checkStats`. Damit alle vier Tests grün.

  Alternative kompakter (falls Reviewer-Lesbarkeit vorgeht): Test + Fix im selben Commit pro Test-Case bündeln. Variante hängt vom Reviewer-Stil ab — für Upstream-PR ist die obige Drei-Commit-Form sauber nachvollziehbar.
- **Force-push:** nur mit `--force-with-lease`.
- **Co-Author-Trailer:** keine. Author + committer beide `Michael Uray <25169478+MichaelUray@users.noreply.github.com>` (siehe `feedback_no_co_author_trailer`).
- **Upstream-PR:** **nicht ohne explizite User-Freigabe** (siehe `feedback_pr_requires_explicit_confirmation`). Nur Push auf `MichaelUray/netbird`-Fork.

---

## 10. Offene Fragen für Codex-Review (Round 2)

1. **Locking-Modell**: bleibt bei der "kein Mutex auf interestedPeers"-Konvention (Codex-Round-1 hat das so abgenickt unter "minimaler Fix okay, Formulierung ehrlich") — oder soll separat ein Mutex eingeführt werden?
2. **Karenzfrist-Konfigurierbarkeit**: ist die Karenzfrist für orphan peers implizit `min(iceTimeout, relayTimeout)` (durch Fall-through in die bestehende Logik) ausreichend, oder soll ein eigener `orphanGracePeriod` konfigurierbar werden? Vorschlag: implizit lassen — dann ist genau ein Zeitwert pro Mode relevant.
3. **Log-Verbosität**: `peer not found in wg stats` (Warnf) entfällt im neuen Pfad zugunsten `peer relay idle since: …` (Infof). Ist das ok, oder fehlt ein Debug-Hinweis "orphan-strecke" auf der `!ok`-Linie?
4. **Naming**: `firstSeenAt` übernommen — bestätigt?
5. **Test-Naming**: vier Test-Funktionen alle mit Präfix `TestCheckStats_*` bzw. `TestRemovePeer_*`. Passt zur bestehenden Datei-Konvention (`TestPeerTriggersInactivity`, `TestPeerTriggersActivity`, `TestManager_HasNoRelayInactiveChanAccessor`)?

---

## 11. Akzeptanz-Kriterien

- [ ] Unit-Tests aus §7.1 grün (`go test ./client/internal/lazyconn/inactivity -count=1 -timeout 120s`).
- [ ] `go test -race ./client/internal/lazyconn/inactivity -count=1 -timeout 120s` grün.
- [ ] Auf S21 nach Build-Deploy: kein `interestedPeers`-Peer länger als `relayTimeout + 1min` im Map ohne mindestens einen `peer relay idle since: …`-Log-Event.
- [ ] `netbird status -d` zeigt keine "creep" von persistenten p2p-connecting/backoff-Peers über mehrere Stunden.
- [ ] Class-A-Verhalten (Peer mit Handshake → 5min idle → disconnect) bleibt unverändert.
- [ ] Watchdog-Drop-Counter (`relay_drops`, `ice_drops`) bleiben bei Null während dieses Test-Szenarios — der Pfad geht nicht durch den Watchdog.
- [ ] Kein Lint-/Vet-Fehler, kein neuer Race im `-race`-Build.

---

## 12. Anhang — relevanter Code (Stand 74c46f2eb)

`client/internal/lazyconn/inactivity/manager.go:266-294`:

```go
func (m *Manager) checkStats() (iceIdle, relayIdle map[string]struct{}, err error) {
    lastActivities := m.iface.LastActivities()

    iceIdle = make(map[string]struct{})
    relayIdle = make(map[string]struct{})

    checkTime := time.Now()
    for peerID, peerCfg := range m.interestedPeers {
        lastActive, ok := lastActivities[peerID]
        if !ok {
            // when peer is in connecting state
            peerCfg.Log.Warnf("peer not found in wg stats")
            continue
        }

        since := monotime.Since(lastActive)

        if m.iceTimeout > 0 && since > m.iceTimeout {
            peerCfg.Log.Debugf("peer ICE idle since: %s", checkTime.Add(-since).String())
            iceIdle[peerID] = struct{}{}
        }
        if m.relayTimeout > 0 && since > m.relayTimeout {
            peerCfg.Log.Infof("peer relay idle since: %s", checkTime.Add(-since).String())
            relayIdle[peerID] = struct{}{}
        }
    }

    return iceIdle, relayIdle, nil
}
```

`client/internal/lazyconn/peercfg.go` (zur Klarstellung: kein AddedAt-/FirstSeenAt-Feld vorhanden — die neue Map liegt ausschließlich in `inactivity.Manager`):

```go
type PeerConfig struct {
    PublicKey  string
    AllowedIPs []netip.Prefix
    PeerConnID id.ConnID
    Log        *log.Entry
}
```

`client/iface/configurer/usp.go:117-124` (Beleg für WG-Stats-Semantik aus §2.1):

```go
if endpoint != nil {
    addr, err := netip.ParseAddr(endpoint.IP.String())
    if err != nil {
        return fmt.Errorf("failed to parse endpoint address: %w", err)
    }
    addrPort := netip.AddrPortFrom(addr, uint16(endpoint.Port))
    c.activityRecorder.UpsertAddress(peerKey, addrPort)
}
```

`client/iface/bind/activity.go:67-82` (Beleg: `UpsertAddress` setzt `LastActivity = monotime.Now()`):

```go
func (r *ActivityRecorder) UpsertAddress(publicKey string, address netip.AddrPort) {
    ...
    record.LastActivity.Store(int64(monotime.Now()))
    ...
}
```

---

**ENDE Spec v0.3.1 — IMPLEMENTATION-READY. Codex sign-off 2026-05-24.**
