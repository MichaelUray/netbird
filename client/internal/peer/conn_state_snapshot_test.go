package peer

import (
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pionice "github.com/pion/ice/v4"
	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/client/internal/peer/conntype"
	"github.com/netbirdio/netbird/shared/connectionmode"
)

// Phase 3.7l (Fix-D D1) regression tests for snapshotForDiagnosis.
//
// The diagnostic snapshot must:
//  1. Capture all stuck-state fields without crashing on nil sub-structs
//  2. Produce a one-line, key=value-formatted string for grep
//  3. Reflect the most informative ICE state when multiple state-getters
//     would yield different verdicts (Connected wins over RetrySafe etc.)
//  4. Include the reason argument for log-grep correlation

// TestConn_SnapshotForDiagnosis_NilSafe verifies that snapshotForDiagnosis
// does not panic when every optional sub-struct is nil — the diagnostic
// log is meant to be safe to invoke at ANY point in the conn lifecycle.
func TestConn_SnapshotForDiagnosis_NilSafe(t *testing.T) {
	c := &Conn{
		Log: log.WithField("peer", "test"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
		currentConnPriority: conntype.None,
	}
	// All optional sub-structs intentionally nil.
	s := c.snapshotForDiagnosis("test-nil-safe")
	str := s.String()

	for _, must := range []string{
		"reason=test-nil-safe",
		"everConn=false",
		"priority=None",
		"mode_local=p2p-dynamic",
		"ice_state=no-worker",
		"listener=n/a",
		"backoff=[fail=-1",
		"remote_relay_supported=no-worker",
	} {
		if !strings.Contains(str, must) {
			t.Errorf("snapshot string missing expected token %q\ngot: %s", must, str)
		}
	}
}

// TestConn_SnapshotForDiagnosis_EverConnected verifies the everConnected
// atomic flag is reflected correctly.
func TestConn_SnapshotForDiagnosis_EverConnected(t *testing.T) {
	c := &Conn{
		Log: log.WithField("peer", "test"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
	}
	c.everConnected.Store(true)
	s := c.snapshotForDiagnosis("test")
	if !strings.Contains(s.String(), "everConn=true") {
		t.Errorf("expected everConn=true in snapshot, got: %s", s.String())
	}
}

// TestConn_SnapshotForDiagnosis_BackoffPopulated verifies that an
// iceBackoff with markFailure history is captured fully (failures, suspended,
// next-retry).
func TestConn_SnapshotForDiagnosis_BackoffPopulated(t *testing.T) {
	c := &Conn{
		Log: log.WithField("peer", "test"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
		iceBackoff: newIceBackoff(15 * time.Minute),
	}
	// Three failures -> suspended with positive nextRetry
	c.iceBackoff.markFailure()
	c.iceBackoff.markFailure()
	c.iceBackoff.markFailure()
	s := c.snapshotForDiagnosis("test-backoff")
	str := s.String()
	if !strings.Contains(str, "fail=3") {
		t.Errorf("expected fail=3 in snapshot, got: %s", str)
	}
	if !strings.Contains(str, "suspended=true") {
		t.Errorf("expected suspended=true, got: %s", str)
	}
}

// TestConn_SnapshotForDiagnosis_ICEStateConnected verifies that when the
// workerICE's lastKnownState is Connected, the snapshot reflects "Connected"
// (overriding the IsRetrySafe-derived classification).
func TestConn_SnapshotForDiagnosis_ICEStateConnected(t *testing.T) {
	w := &WorkerICE{
		agent:          sentinelAgentForTest(),
		lastKnownState: pionice.ConnectionStateConnected,
	}
	c := &Conn{
		Log: log.WithField("peer", "test"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
		workerICE: w,
	}
	s := c.snapshotForDiagnosis("test-ice-connected")
	if !strings.Contains(s.String(), "ice_state=Connected") {
		t.Errorf("expected ice_state=Connected, got: %s", s.String())
	}
}

// TestConn_SnapshotForDiagnosis_ICEStateFailed verifies the Failed state
// is surfaced and the retry-safe flag agrees.
func TestConn_SnapshotForDiagnosis_ICEStateFailed(t *testing.T) {
	w := &WorkerICE{
		agent:          sentinelAgentForTest(),
		lastKnownState: pionice.ConnectionStateFailed,
	}
	c := &Conn{
		Log: log.WithField("peer", "test"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
		workerICE: w,
	}
	s := c.snapshotForDiagnosis("test-ice-failed")
	str := s.String()
	if !strings.Contains(str, "ice_state=Failed") {
		t.Errorf("expected ice_state=Failed, got: %s", str)
	}
	if !strings.Contains(str, "ice_retry_safe=true") {
		t.Errorf("expected ice_retry_safe=true for Failed state, got: %s", str)
	}
}

// TestConn_SnapshotForDiagnosis_RelayUnsupported verifies the
// remote_relay_supported field reflects WorkerRelay.relaySupportedOnRemotePeer
// when explicitly set to false (the Fix-D stuck-state condition).
func TestConn_SnapshotForDiagnosis_RelayUnsupported(t *testing.T) {
	wr := &WorkerRelay{}
	wr.relaySupportedOnRemotePeer.Store(false)
	c := &Conn{
		Log: log.WithField("peer", "test"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
		workerRelay: wr,
	}
	s := c.snapshotForDiagnosis("test-relay-unsupported")
	if !strings.Contains(s.String(), "remote_relay_supported=false") {
		t.Errorf("expected remote_relay_supported=false, got: %s", s.String())
	}
}

// TestConn_SnapshotForDiagnosis_IntentDetached verifies the
// intentionallyDetached atomic flag round-trips correctly.
func TestConn_SnapshotForDiagnosis_IntentDetached(t *testing.T) {
	c := &Conn{
		Log: log.WithField("peer", "test"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
	}
	// Test default false
	s := c.snapshotForDiagnosis("test-intent-detached-false")
	if !strings.Contains(s.String(), "intent_detached=false") {
		t.Errorf("expected intent_detached=false default, got: %s", s.String())
	}
	// Set true via the public API (which is what conn.go uses)
	c.MarkIntentionallyDetached()
	s = c.snapshotForDiagnosis("test-intent-detached-true")
	if !strings.Contains(s.String(), "intent_detached=true") {
		t.Errorf("expected intent_detached=true after Set, got: %s", s.String())
	}
}

// TestConn_LogDiagSnapshot_EmitsDebugMarker is a smoke test confirming
// that logDiagSnapshot is callable and produces a log entry matching the
// [DIAG] marker. With a buffered logger this could be more precise; for
// now it just guards that calling does not panic.
func TestConn_LogDiagSnapshot_EmitsDebugMarker(t *testing.T) {
	c := &Conn{
		Log: log.WithField("peer", "test"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
	}
	// Calling logDiagSnapshot must not panic and must complete.
	c.logDiagSnapshot("smoke-test")
}

// TestConn_SnapshotForDiagnosis_StuckStateSignature is the most important
// test: it reproduces the Fix-D stuck-state condition Codex hypothesised
// (ICE Failed + listener attached + backoff suspended + remote lazy +
// everConnected=false) and verifies the snapshot string contains every
// field needed to identify the condition from a log dump.
//
// If a real S26 stuck-state event log later contains a [DIAG] line, this
// is the field-set used to confirm or refute the hypothesis.
func TestConn_SnapshotForDiagnosis_StuckStateSignature(t *testing.T) {
	w := &WorkerICE{
		agent:          sentinelAgentForTest(),
		lastKnownState: pionice.ConnectionStateFailed,
	}
	wr := &WorkerRelay{}
	wr.relaySupportedOnRemotePeer.Store(false)

	// Mark backoff to populate suspended state.
	bo := newIceBackoff(15 * time.Minute)
	bo.markFailure()
	bo.markFailure()
	bo.markFailure()

	// Listener attached.
	h := &Handshaker{}
	h.AddICEListener(func(o *OfferAnswer) {})

	c := &Conn{
		Log: log.WithField("peer", "test"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
		opened:              true,
		currentConnPriority: conntype.Relay,
		workerICE:           w,
		workerRelay:         wr,
		iceBackoff:          bo,
		handshaker:          h,
	}
	// everConnected stays false (the bootstrap-never-connected case).
	_ = atomic.LoadInt32(new(int32)) // suppress unused atomic import linter

	s := c.snapshotForDiagnosis("stuck-state-repro")
	str := s.String()
	for _, must := range []string{
		"reason=stuck-state-repro",
		"everConn=false",
		"priority=PriorityRelay",
		"mode_local=p2p-dynamic",
		"ice_state=Failed",
		"listener=attached",
		"suspended=true",
		"remote_relay_supported=false",
	} {
		if !strings.Contains(str, must) {
			t.Errorf("stuck-state signature missing %q\ngot: %s", must, str)
		}
	}
}

// TestConn_SnapshotForDiagnosis_SrflxFieldsFormatted verifies that the
// three Phase-1 stuck-srflx fields land in the one-line DIAG string in
// the expected key=value form. Codex 2026-05-30 polish — without this
// test the wiring between Conn.srflxState and stateSnapshot.String()
// is only covered indirectly by hardware logs.
func TestConn_SnapshotForDiagnosis_SrflxFieldsFormatted(t *testing.T) {
	c := &Conn{
		Log: log.WithField("peer", "test"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
		currentConnPriority: conntype.None,
	}
	// Seed the srflx state with a known stuck pattern: real public
	// port, three same-port failures, lastChanged stamped at a fixed
	// instant we can grep for in the formatted output.
	ap := netip.MustParseAddrPort("41.66.90.21:32038")
	stamp := time.Date(2026, 5, 30, 11, 56, 40, 0, time.UTC)
	c.srflxState.observeFailure(ap, stamp)
	c.srflxState.observeFailure(ap, stamp.Add(15*time.Second))
	c.srflxState.observeFailure(ap, stamp.Add(30*time.Second))

	str := c.snapshotForDiagnosis("srflx-format-test").String()

	for _, must := range []string{
		"srflx_local=41.66.90.21:32038",
		"srflx_same_failures=3",
		// Format uses time.TimeOnly = "15:04:05"; stamp is 11:56:40 UTC.
		"srflx_last_changed=11:56:40",
		// srflx_stable_for_seconds is derived from now - lastChanged.
		// Since `stamp` is way in the past (2026-05-30), the diff is
		// a positive integer. Just assert the key is present.
		"srflx_stable_for_seconds=",
	} {
		if !strings.Contains(str, must) {
			t.Errorf("srflx fields missing %q\ngot: %s", must, str)
		}
	}
}

// TestConn_SnapshotForDiagnosis_SrflxNoneAndNeverWhenUntouched covers
// the documented zero-state of stateSnapshot before any
// observeFailure/observeSuccess call: the DIAG line must show
// srflx_local=none and srflx_last_changed=never so offline tooling
// can distinguish "never observed" from "stuck on a real port".
func TestConn_SnapshotForDiagnosis_SrflxNoneAndNeverWhenUntouched(t *testing.T) {
	c := &Conn{
		Log: log.WithField("peer", "test"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
		currentConnPriority: conntype.None,
	}
	str := c.snapshotForDiagnosis("srflx-zero-state").String()

	for _, must := range []string{
		"srflx_local=none",
		"srflx_same_failures=0",
		"srflx_last_changed=never",
		// Codex follow-up: untouched state surfaces -1 sentinel so
		// offline tooling can distinguish "no observation possible"
		// from "0 seconds stable".
		"srflx_stable_for_seconds=-1",
	} {
		if !strings.Contains(str, must) {
			t.Errorf("zero-srflx-state token missing %q\ngot: %s", must, str)
		}
	}
}

// TestConn_OnICEFailedIncrementsSrflxCounter verifies the wiring
// between Conn.onICEFailed and Conn.srflxState. Codex 2026-05-30 Q5:
// without this test the hook-up is only verified by source inspection
// and end-to-end hardware capture, not at the unit level.
//
// The test seeds a known LastLocalSrflx via a hand-constructed
// WorkerICE (we intentionally do NOT call Open() — that path requires
// the full ICE/relay pipeline and is unrelated to the assertion).
// onICEFailed then runs through its early-return-on-nil-backoff guard
// via a real iceBackoff, hits the srflxState.observeFailure call, and
// the counter must advance from 0 to 1 with lastSrflx == the seeded
// value.
func TestConn_OnICEFailedIncrementsSrflxCounter(t *testing.T) {
	conn := newMarkerTestConn(t)
	// onICEFailed early-returns when iceBackoff == nil. Initialise it
	// so the srflxState hook actually runs.
	conn.iceBackoff = newIceBackoff(30 * time.Second)

	// Seed a srflx value in a minimal WorkerICE. The Conn.workerICE !=
	// nil branch is the path under test.
	ap := netip.MustParseAddrPort("203.0.113.7:17692")
	conn.workerICE = &WorkerICE{}
	conn.workerICE.lastLocalSrflx.Store(ap)

	if got := conn.srflxState.snapshot().samePortFailures; got != 0 {
		t.Fatalf("pre-condition: samePortFailures=%d, want 0", got)
	}

	conn.onICEFailed()

	snap := conn.srflxState.snapshot()
	if snap.samePortFailures != 1 {
		t.Errorf("samePortFailures after onICEFailed = %d, want 1",
			snap.samePortFailures)
	}
	if snap.lastSrflx != ap {
		t.Errorf("lastSrflx after onICEFailed = %v, want %v",
			snap.lastSrflx, ap)
	}
}

// TestConn_OnICEConnectedResetsSrflxCounter is the matching hook-
// wiring assertion for the success path. Codex 2026-05-30 Q5 polish.
func TestConn_OnICEConnectedResetsSrflxCounter(t *testing.T) {
	conn := newMarkerTestConn(t)
	conn.iceBackoff = newIceBackoff(30 * time.Second)

	ap := netip.MustParseAddrPort("203.0.113.7:17692")
	conn.workerICE = &WorkerICE{}
	conn.workerICE.lastLocalSrflx.Store(ap)

	// Pre-seed a non-zero failure streak directly so we can observe the
	// reset action.
	conn.srflxState.observeFailure(ap, time.Now())
	conn.srflxState.observeFailure(ap, time.Now())
	if got := conn.srflxState.snapshot().samePortFailures; got != 2 {
		t.Fatalf("pre-condition: samePortFailures=%d, want 2", got)
	}

	conn.onICEConnected()

	snap := conn.srflxState.snapshot()
	if snap.samePortFailures != 0 {
		t.Errorf("samePortFailures after onICEConnected = %d, want 0",
			snap.samePortFailures)
	}
	if snap.lastSrflx != ap {
		t.Errorf("lastSrflx after onICEConnected = %v, want %v "+
			"(success path records the current AddrPort)",
			snap.lastSrflx, ap)
	}
}
