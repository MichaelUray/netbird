package peer

import (
	"fmt"
	"time"

	pionice "github.com/pion/ice/v4"
)

// Phase 3.7l (Fix-D D1): per-peer state-snapshot helper for stuck-state
// diagnosis.
//
// The intermittent S26-can-not-reach-Elmira class of bugs is hard to
// catch retrospectively because individual log lines only show one
// dimension at a time: "ICE backoff active", "guard skip offer",
// "Relay is not supported by remote peer" each appear separately, and
// reconstructing the full per-peer state from a noisy logcat dump
// is slow and error-prone.
//
// snapshotForDiagnosis emits ONE structured line capturing every state
// field that's relevant to Codex' stuck-state hypothesis:
//
//	ICE Disconnected/Closed/Failed
//	Relay Disconnected or "relay not supported by remote peer"
//	iceBackoff.IsSuspended() with failure-count and next-retry time
//	remoteEffectiveMode = p2p-lazy / p2p-dynamic
//	everConnected = false (or stale-true post-network-change)
//	intentionallyDetached flag
//	handshaker.iceListener attached vs detached
//	currentConnPriority None/Relay/ICE
//
// The line is emitted at decision points where the peer can silently
// stop progressing — guard skip-offer paths, markFailure call sites,
// "Relay not supported" path, "without local ICE listener" path,
// AttachICEOnRelayActivity early-rejects. With all snapshots grep-able
// for the same peer-key, the offline timeline reconstruction is one
// awk-script away.
//
// Risk: low. No control-flow changes, just additional structured
// log lines at DEBG level. Volume is bounded by existing trigger
// log frequency (already DEBG/TRAC).

// stateSnapshot is the in-memory representation captured by
// snapshotForDiagnosis. Stringer-formatted into one grep-friendly
// key=value line. Caller must hold conn.mu OR accept a benign race
// against concurrent state mutation (fine for diagnostic logging).
type stateSnapshot struct {
	reason         string
	everConnected  bool
	priority       string
	modeLocal      string
	modeRemote     string
	iceState       string
	iceRetrySafe   bool
	iceInProgress  bool
	listener       string
	backoffFail    int
	backoffSuspend bool
	backoffNext    string
	relaySupported string
	intentDetached bool
	openedFlag     bool
	wgIfaceUp      string
}

func (s stateSnapshot) String() string {
	return fmt.Sprintf(
		"reason=%s everConn=%v priority=%s mode_local=%s mode_remote=%s "+
			"ice_state=%s ice_retry_safe=%v ice_in_progress=%v "+
			"listener=%s backoff=[fail=%d,suspended=%v,next=%s] "+
			"remote_relay_supported=%s intent_detached=%v opened=%v "+
			"wg_iface=%s",
		s.reason, s.everConnected, s.priority,
		s.modeLocal, s.modeRemote,
		s.iceState, s.iceRetrySafe, s.iceInProgress,
		s.listener,
		s.backoffFail, s.backoffSuspend, s.backoffNext,
		s.relaySupported, s.intentDetached, s.openedFlag,
		s.wgIfaceUp,
	)
}

// snapshotForDiagnosis captures the current state of this peer connection
// into a stateSnapshot. Each field reads its source with whatever
// thread-safety primitive it already exposes (atomic, mutex on sub-struct)
// — no extra global lock is held. Therefore the snapshot may capture
// a field mid-transition; that is acceptable for diagnostic logging.
//
// The reason argument is the human-readable description of WHY this
// snapshot is being taken (e.g. "guard-skip-bootstrap-offer",
// "markFailure-wg-handshake-timeout", "AttachICEOnRelayActivity-blocked-priority").
func (conn *Conn) snapshotForDiagnosis(reason string) stateSnapshot {
	s := stateSnapshot{
		reason:        reason,
		everConnected: conn.everConnected.Load(),
		priority:      conn.currentConnPriority.String(),
		modeLocal:     conn.config.Mode.String(),
		modeRemote:    conn.remoteEffectiveMode().String(),
		listener:      "n/a",
		iceState:      "no-worker",
		backoffFail:   -1,
		backoffNext:   "n/a",
		relaySupported: "no-worker",
		intentDetached: conn.IsIntentionallyDetached(),
		openedFlag:     conn.opened,
		wgIfaceUp:      "unknown",
	}

	if conn.workerICE != nil {
		// Surface the most informative ICE state. IsConnected and
		// IsRetrySafe lock muxAgent internally.
		switch {
		case conn.workerICE.IsConnected():
			s.iceState = "Connected"
		case conn.workerICE.InProgress():
			s.iceState = "Connecting"
		case conn.workerICE.IsRetrySafe():
			// agent nil OR in Failed/Disconnected/Closed
			s.iceState = "Stale-or-NoAgent"
		default:
			s.iceState = "Other"
		}
		s.iceRetrySafe = conn.workerICE.IsRetrySafe()
		s.iceInProgress = conn.workerICE.InProgress()
		// Best-effort: lastKnownState read (small enum, torn-read-safe)
		switch conn.workerICE.lastKnownState {
		case pionice.ConnectionStateNew:
			s.iceState = "New"
		case pionice.ConnectionStateChecking:
			s.iceState = "Checking"
		case pionice.ConnectionStateConnected:
			s.iceState = "Connected"
		case pionice.ConnectionStateCompleted:
			s.iceState = "Completed"
		case pionice.ConnectionStateFailed:
			s.iceState = "Failed"
		case pionice.ConnectionStateDisconnected:
			s.iceState = "Disconnected"
		case pionice.ConnectionStateClosed:
			s.iceState = "Closed"
		}
	}

	if conn.handshaker != nil {
		if conn.handshaker.readICEListener() != nil {
			s.listener = "attached"
		} else {
			s.listener = "detached"
		}
	}

	if conn.iceBackoff != nil {
		snap := conn.iceBackoff.Snapshot()
		s.backoffFail = snap.Failures
		s.backoffSuspend = snap.Suspended
		if snap.NextRetry.IsZero() {
			s.backoffNext = "none"
		} else {
			s.backoffNext = snap.NextRetry.Format(time.TimeOnly)
		}
	}

	if conn.workerRelay != nil {
		if conn.workerRelay.relaySupportedOnRemotePeer.Load() {
			s.relaySupported = "true"
		} else {
			s.relaySupported = "false"
		}
	}

	if conn.config.WgConfig.WgInterface != nil {
		s.wgIfaceUp = "up"
	}

	return s
}

// logDiagSnapshot is the single emit-point — uniform DEBG-level line
// with the [DIAG] marker so a single grep finds them all across slices.
//
// Pattern for offline analysis:
//
//	logcat | rg '\[DIAG\] peer:' | rg '<peer-key>'
//
// yields the per-peer state timeline ordered by trigger reason.
func (conn *Conn) logDiagSnapshot(reason string) {
	s := conn.snapshotForDiagnosis(reason)
	conn.Log.Debugf("[DIAG] %s", s.String())
}
