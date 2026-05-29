package peer

import (
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
