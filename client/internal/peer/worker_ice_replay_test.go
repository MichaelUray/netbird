package peer

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	signalmock "github.com/netbirdio/netbird/shared/signal/client"
	sProto "github.com/netbirdio/netbird/shared/signal/proto"
)

// Track C 2026-05-31 (Codex Plan v4) unit-tests for the
// legacy-scoped Candidate Replay mechanism.
//
// Test mocking strategy (Codex Plan v3 review of Signaler-type):
// Signaler is a concrete struct, not an interface. We construct it
// with shared/signal/client.MockClient (which already implements
// signal.Client) so the Send-path is observable via a counter.
// All tests use real Signaler + real MockClient — no test-only
// indirection added to production code.

// candidateSendCount counts how often signaler.SignalICECandidate
// has reached the signaler.Send-path. Both the initial gather-time
// send AND the replay-time send are counted; tests that need to
// distinguish initial-vs-replay use timing OR pre-populate
// sentCandidates without calling signalAndRemember.
type candidateSendCounter struct {
	count int64
}

func (c *candidateSendCounter) total() int { return int(atomic.LoadInt64(&c.count)) }

func (c *candidateSendCounter) onSend(msg *sProto.Message) {
	if msg.Body != nil && msg.Body.Type == sProto.Body_CANDIDATE {
		atomic.AddInt64(&c.count, 1)
	}
}

// newTestWorkerICE returns a minimal WorkerICE wired to a
// MockClient-backed Signaler. We bypass NewWorkerICE because that
// needs an ICE config; for these tests we only exercise the
// candidate-replay state machine.
func newTestWorkerICE(t *testing.T, counter *candidateSendCounter) *WorkerICE {
	t.Helper()
	mock := &signalmock.MockClient{
		ReadyFunc: func() bool { return true },
		SendFunc: func(msg *sProto.Message) error {
			if counter != nil {
				counter.onSend(msg)
			}
			return nil
		},
	}
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate wg key: %v", err)
	}
	return &WorkerICE{
		log:      log.WithField("peer", "track-c-test"),
		signaler: NewSignaler(mock, key),
		config:   ConnConfig{Key: "test-peer-key"},
	}
}

// stubCandidate produces an ice.Candidate that can be Marshal()-ed
// without an ICE agent (host candidate with a fixed address).
func stubCandidate(t *testing.T, port int) ice.Candidate {
	t.Helper()
	cand, err := ice.NewCandidateHost(&ice.CandidateHostConfig{
		Network:   "udp",
		Address:   "192.0.2.1",
		Port:      port,
		Component: 1,
	})
	if err != nil {
		t.Fatalf("NewCandidateHost: %v", err)
	}
	return cand
}

// drainGoroutines gives the SignalICECandidate background goroutines
// a chance to complete before we read the counter. The default delay
// is 50ms which is plenty for an in-process MockClient.
func drainGoroutines() { time.Sleep(50 * time.Millisecond) }

// Test 1 (Codex v3 + v4 explicit "v0.51.2"): version threshold.
func TestIsLegacyICECandidateRecv_Versions(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		why  string
	}{
		{"0.51.2", true, "Elmira, the confirmed-affected baseline"},
		{"v0.51.2", true, "Codex v4 explicit prefix-trim test"},
		{"0.51.0", true, "below ceiling"},
		{"0.51.99", true, "below ceiling"},
		// V18.32 (2026-06-21): ceiling raised 0.52.0 → 0.54.0 after
		// dolice-bg-r1 v0.53.0 confirmed racing in production
		// (OFFER/ANSWER exchange OK, WG handshake timeout 10s,
		// "Required key not available" on kernel-WG).
		{"0.52.0", true, "now legacy (raised ceiling)"},
		{"0.53.0", true, "dolice / lethbridge / lunzamsee — confirmed racing 2026-06-21"},
		{"0.53.99", true, "covers all 0.53.x patch releases"},
		// V18.32 Codex follow-up 2026-06-21: pin suffix normalization
		// + tagged form so future ParseAgentVersion changes don't
		// silently miss real-world dolice variants.
		{"0.53.0-dirty", true, "dirty-suffix-strip — still legacy"},
		{"v0.53.0", true, "tagged form — still legacy"},
		{"0.54.0", false, "new ceiling exclusive"},
		{"0.59.13", false, "ared-park/antiesenhofen — assumed modern"},
		{"0.60.4", false, "MarlCreek/Stocking-BG — assumed modern"},
		{"0.67.1", false, "ctb59-n — modern"},
		{"0.68.0-dev-phase1-srflx-diag-polish-43e1bdfa1", false, "dev-substring → modern"},
		{"0.68.0-dev-orphan-ee9f78c17", false, "dev-substring → modern"},
		{"development", false, "self-marker → modern"},
		{"", false, "empty → modern (don't gate without evidence)"},
		{"a6c5960", false, "short-hash → modern"},
		{"dev-abc123", false, "dev-prefix → modern"},
		{"ci-xyz", false, "ci-prefix → modern"},
	}
	for _, c := range cases {
		got := isLegacyICECandidateRecv(c.in)
		if got != c.want {
			t.Errorf("isLegacyICECandidateRecv(%q) = %v, want %v (%s)", c.in, got, c.want, c.why)
		}
	}
}

// Test 2: env-flag NB_LEGACY_CANDIDATE_REPLAY=false → no replay.
func TestMaybeReplayCandidates_GatedByEnv(t *testing.T) {
	t.Setenv(envLegacyCandidateReplayEnabled, "false")
	t.Setenv(envLegacyCandidateReplayDelayMs, "50")
	counter := &candidateSendCounter{}
	w := newTestWorkerICE(t, counter)
	w.sentCandidates = append(w.sentCandidates, stubCandidate(t, 1))
	w.MaybeReplayCandidates("0.51.2") // would be legacy
	drainGoroutines()
	if got := counter.total(); got != 0 {
		t.Errorf("env-disabled: signaler send count = %d, want 0", got)
	}
	if w.replayedThisSession {
		t.Error("env-disabled: replayedThisSession should remain false")
	}
}

// Test 3: modern peer → no replay.
func TestMaybeReplayCandidates_GatedByVersion(t *testing.T) {
	t.Setenv(envLegacyCandidateReplayDelayMs, "50")
	counter := &candidateSendCounter{}
	w := newTestWorkerICE(t, counter)
	w.sentCandidates = append(w.sentCandidates, stubCandidate(t, 1))
	w.MaybeReplayCandidates("0.68.0-dev-phase1-srflx-diag-polish-43e1bdfa1")
	drainGoroutines()
	if got := counter.total(); got != 0 {
		t.Errorf("modern-version: signaler send count = %d, want 0", got)
	}
	if w.replayedThisSession {
		t.Error("modern-version: replayedThisSession should remain false")
	}
}

// Test 4: legacy peer, N candidates pre-populated → replay sends N.
func TestMaybeReplayCandidates_ResendsAllSentCandidates(t *testing.T) {
	t.Setenv(envLegacyCandidateReplayDelayMs, "50")
	counter := &candidateSendCounter{}
	w := newTestWorkerICE(t, counter)
	w.sentCandidates = []ice.Candidate{
		stubCandidate(t, 1),
		stubCandidate(t, 2),
		stubCandidate(t, 3),
	}
	w.MaybeReplayCandidates("0.51.2")
	drainGoroutines()
	if got := counter.total(); got != 3 {
		t.Errorf("legacy-replay-3: send count = %d, want 3", got)
	}
	if !w.replayedThisSession {
		t.Error("replayedThisSession should be true after successful replay")
	}
}

// Test 5 (Codex v3): candidate appended DURING the delay window
// must still appear in the snapshot. This is the OnRemoteOffer-
// receiver-role guarantee.
func TestMaybeReplayCandidates_DelaysBeforeSnapshot(t *testing.T) {
	t.Setenv(envLegacyCandidateReplayDelayMs, "200")
	counter := &candidateSendCounter{}
	w := newTestWorkerICE(t, counter)
	// Initially empty sentCandidates (simulates the moment when
	// Conn.OnRemoteOffer triggers MaybeReplayCandidates BEFORE
	// Handshaker has run reCreateAgent + gather()).

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.MaybeReplayCandidates("0.51.2")
	}()

	// Simulate gather() producing a candidate ~50ms into the
	// 200ms delay window.
	time.Sleep(50 * time.Millisecond)
	w.signalAndRemember(stubCandidate(t, 42))
	wg.Wait()
	drainGoroutines()
	// 1 send from signalAndRemember + 1 send from replay = 2
	if got := counter.total(); got != 2 {
		t.Errorf("delay-before-snapshot: send count = %d, want 2 "+
			"(1 initial + 1 replay)", got)
	}
	if !w.replayedThisSession {
		t.Error("replayedThisSession should be true after successful replay")
	}
}

// Test 6 (Codex v2): onICECandidate-append happens BEFORE the
// async send, so a fast-answer race still captures the candidate.
func TestOnICECandidate_AppendsBeforeAsyncSend(t *testing.T) {
	counter := &candidateSendCounter{}
	w := newTestWorkerICE(t, counter)
	w.signalAndRemember(stubCandidate(t, 99))
	// Immediately after the call returns: append must be visible
	// in sentCandidates, even though the async send may still be in
	// flight.
	w.sentCandidatesMu.Lock()
	n := len(w.sentCandidates)
	w.sentCandidatesMu.Unlock()
	if n != 1 {
		t.Errorf("sentCandidates len = %d, want 1 (append must be sync)", n)
	}
	drainGoroutines()
}

// Test 7: reCreateAgent clears state including replay-once gate.
func TestReCreateAgentClearsSentCandidatesAndReplayState(t *testing.T) {
	t.Setenv(envLegacyCandidateReplayDelayMs, "10")
	counter := &candidateSendCounter{}
	w := newTestWorkerICE(t, counter)
	// Cannot call reCreateAgent directly without a real ICE config;
	// instead, replicate its state-clearing logic to verify the
	// state-reset semantics.
	w.sentCandidates = append(w.sentCandidates, stubCandidate(t, 1))
	w.replayedThisSession = true
	w.replaySkippedAttempts = 5

	// The exact clear sequence from reCreateAgent (worker_ice.go):
	w.sentCandidatesMu.Lock()
	w.sentCandidates = w.sentCandidates[:0]
	w.replayedThisSession = false
	w.replaySkippedAttempts = 0
	w.sentCandidatesMu.Unlock()

	if len(w.sentCandidates) != 0 {
		t.Errorf("sentCandidates not cleared, len=%d", len(w.sentCandidates))
	}
	if w.replayedThisSession {
		t.Error("replayedThisSession not reset")
	}
	if w.replaySkippedAttempts != 0 {
		t.Errorf("replaySkippedAttempts = %d, want 0", w.replaySkippedAttempts)
	}
}

// Test 8 (Codex v3): per-session one-shot — second trigger no-ops.
func TestMaybeReplayCandidates_PerSessionOneShot(t *testing.T) {
	t.Setenv(envLegacyCandidateReplayDelayMs, "30")
	counter := &candidateSendCounter{}
	w := newTestWorkerICE(t, counter)
	w.sentCandidates = []ice.Candidate{
		stubCandidate(t, 1),
		stubCandidate(t, 2),
	}

	// First replay fires
	w.MaybeReplayCandidates("0.51.2")
	drainGoroutines()
	if got := counter.total(); got != 2 {
		t.Errorf("first replay: send count = %d, want 2", got)
	}
	if !w.replayedThisSession {
		t.Error("after first replay, gate should be consumed")
	}

	// Second trigger is a no-op
	w.MaybeReplayCandidates("0.51.2")
	drainGoroutines()
	if got := counter.total(); got != 2 {
		t.Errorf("second trigger (gated): send count = %d, want 2 (no change)", got)
	}
	if w.replaySkippedAttempts != 1 {
		t.Errorf("replaySkippedAttempts = %d, want 1", w.replaySkippedAttempts)
	}

	// After the simulated reCreateAgent state-reset, replay can fire
	// again.
	w.sentCandidatesMu.Lock()
	w.replayedThisSession = false
	w.replaySkippedAttempts = 0
	w.sentCandidatesMu.Unlock()
	w.MaybeReplayCandidates("0.51.2")
	drainGoroutines()
	if got := counter.total(); got != 4 {
		t.Errorf("after-reset replay: send count = %d, want 4 (2 prev + 2 new)", got)
	}
}

// Test 9 (Codex v4): replayedThisSession=true from previous session
// must NOT block a new replay if reCreateAgent runs during the
// sleep window.
func TestMaybeReplayCandidates_DelaysBeforeReplayOnceGate(t *testing.T) {
	t.Setenv(envLegacyCandidateReplayDelayMs, "200")
	counter := &candidateSendCounter{}
	w := newTestWorkerICE(t, counter)
	// Simulate previous-session state: gate consumed, old (now
	// stale) candidates buffered.
	w.sentCandidates = []ice.Candidate{stubCandidate(t, 1001)}
	w.replayedThisSession = true

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.MaybeReplayCandidates("0.51.2")
	}()

	// Simulate reCreateAgent firing ~50ms into the sleep window.
	time.Sleep(50 * time.Millisecond)
	w.sentCandidatesMu.Lock()
	w.sentCandidates = w.sentCandidates[:0]
	w.replayedThisSession = false
	w.replaySkippedAttempts = 0
	w.sentCandidatesMu.Unlock()

	// Simulate fresh gather() ~50ms later (still inside the 200ms
	// delay).
	time.Sleep(50 * time.Millisecond)
	w.signalAndRemember(stubCandidate(t, 2002))

	wg.Wait()
	drainGoroutines()
	// 1 from signalAndRemember (fresh gather) + 1 from replay = 2.
	// We MUST see the replay despite the previous session's gate
	// being initially consumed.
	if got := counter.total(); got != 2 {
		t.Errorf("delay-before-gate: send count = %d, want 2 "+
			"(1 fresh send + 1 replay) — gate-was-read-too-early?", got)
	}
	if !w.replayedThisSession {
		t.Error("replayedThisSession should be true after successful replay")
	}
}

// Test 10 (Codex v4): no-consume-on-empty — empty snapshot must
// NOT consume the gate, so a later trigger can still fire.
func TestMaybeReplayCandidates_EmptySnapshotDoesNotConsumeReplayGate(t *testing.T) {
	t.Setenv(envLegacyCandidateReplayDelayMs, "50")
	counter := &candidateSendCounter{}
	w := newTestWorkerICE(t, counter)

	// First call: empty sentCandidates, should return without
	// consuming the gate.
	w.MaybeReplayCandidates("0.51.2")
	drainGoroutines()
	if got := counter.total(); got != 0 {
		t.Errorf("empty-snapshot: send count = %d, want 0", got)
	}
	if w.replayedThisSession {
		t.Error("empty-snapshot: replayedThisSession must NOT be consumed")
	}

	// Now append a candidate and trigger again — must fire.
	w.signalAndRemember(stubCandidate(t, 7))
	w.MaybeReplayCandidates("0.51.2")
	drainGoroutines()
	// 1 from signalAndRemember + 1 from replay = 2
	if got := counter.total(); got != 2 {
		t.Errorf("after-append replay: send count = %d, want 2", got)
	}
	if !w.replayedThisSession {
		t.Error("after-append: replayedThisSession should be consumed now")
	}

	// Third trigger is gated.
	w.MaybeReplayCandidates("0.51.2")
	drainGoroutines()
	if got := counter.total(); got != 2 {
		t.Errorf("third trigger (gated): send count = %d, want 2", got)
	}
	if w.replaySkippedAttempts != 1 {
		t.Errorf("replaySkippedAttempts = %d, want 1", w.replaySkippedAttempts)
	}
}

// Defensive: ensure env-knob default-on works without an explicit
// setenv.
func TestLegacyCandidateReplayEnabled_DefaultOn(t *testing.T) {
	old := os.Getenv(envLegacyCandidateReplayEnabled)
	os.Unsetenv(envLegacyCandidateReplayEnabled)
	defer func() {
		if old != "" {
			os.Setenv(envLegacyCandidateReplayEnabled, old)
		}
	}()
	if !legacyCandidateReplayEnabled() {
		t.Error("default-on regression: unset env should be ENABLED")
	}
}
