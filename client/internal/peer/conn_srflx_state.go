package peer

import (
	"net/netip"
	"sync"
	"time"
)

// Phase 3.7l Phase-1 — per-peer "stuck-srflx" tracking.
//
// Codex' 2026-05-30 review of the Elmira ICE-failure investigation
// established that D2a / D3b / AttachICE* recreate the pion Agent on
// retry but DO NOT cycle the underlying UDP-Mux. Consequence: every
// retry re-discovers the SAME server-reflexive (srflx) AddrPort from
// the STUN server, so a stuck Magenta-NAT mapping keeps producing the
// same pair on every attempt. The only thing that actually unsticks
// it observed in the wild was an external NAT-mapping refresh (WiFi
// roam, VPNService rebind, NAT TTL expiry) that handed the device a
// fresh public port.
//
// Phase 1 is observation-only: track the last srflx AddrPort per peer
// and how many consecutive ICE failures occurred without it changing.
// The data flows into the [DIAG] snapshot line so offline analysis can
// confirm the pattern. NO recovery action — that comes in Phase 3
// after we have hardware evidence of the "same srflx, N failures,
// relay alive" condition firing in real captures.

// srflxFailureState is the per-Conn snapshot of the stuck-srflx
// observation. All access goes through srflxStateSync (below), which
// holds its OWN sync.Mutex — NOT conn.mu — so the failure-/success-
// hooks can update the counter without re-entrancy against the
// existing Conn locking. Direct field access on srflxFailureState is
// only safe in unit tests / the wrapper methods.
type srflxFailureState struct {
	lastSrflx        netip.AddrPort
	samePortFailures int
	lastChanged      time.Time
}

// observeICEFailure records one ICE failure and updates the
// same-port-failure counter against the current local srflx. Returns
// the updated state for caller convenience (Diag-emit usually reads it
// straight back).
//
// Behaviour:
//   - first failure (no prior state, or different srflx)         → reset to 1
//   - subsequent failure with identical srflx (incl. zero-zero)  → ++
//   - srflx changed since last call                              → reset to 1
//
// "Identical zero AddrPort" (which can happen if pion has not yet
// surfaced a srflx candidate by the time the agent fails) is treated
// as "same as last zero" — that pattern itself is diagnostically
// interesting (the recovery never observed any public port at all).
func (s *srflxFailureState) observeICEFailure(currentSrflx netip.AddrPort, now time.Time) {
	if currentSrflx == s.lastSrflx {
		s.samePortFailures++
		// Codex review 2026-05-30 polish: on the very first observation
		// (lastChanged still zero), stamp `now` even though we took the
		// same-srflx branch. Without this the DIAG line shows
		// `srflx_same_failures=1 srflx_last_changed=never`, which looks
		// like a missing timestamp rather than the documented "first
		// failure with zero AddrPort starts a same-streak" semantics.
		if s.lastChanged.IsZero() {
			s.lastChanged = now
		}
		return
	}
	s.lastSrflx = currentSrflx
	s.samePortFailures = 1
	s.lastChanged = now
}

// observeICESuccess resets the counter on every successful ICE Connected
// transition. A fresh stuck-streak only counts after the next failure
// cycle starts.
func (s *srflxFailureState) observeICESuccess(currentSrflx netip.AddrPort, now time.Time) {
	s.lastSrflx = currentSrflx
	s.samePortFailures = 0
	s.lastChanged = now
}

// srflxStateSync is the tiny mutex helper that wraps any access to a
// Conn's srflxFailureState. We use a per-Conn sync.Mutex instead of
// conn.mu because the failure-/success-hooks (onICEFailed,
// onICEConnected) intentionally do NOT hold conn.mu over the full
// pion-state-update path; piggybacking on conn.mu would force
// re-entrancy. A dedicated mutex keeps the observation lock-window
// tiny and unconfused.
type srflxStateSync struct {
	mu    sync.Mutex
	state srflxFailureState
}

// observeFailure is a tiny locked wrapper around srflxFailureState.observeICEFailure.
func (s *srflxStateSync) observeFailure(srflx netip.AddrPort, now time.Time) srflxFailureState {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.observeICEFailure(srflx, now)
	return s.state
}

// observeSuccess is the matching wrapper for observeICESuccess.
func (s *srflxStateSync) observeSuccess(srflx netip.AddrPort, now time.Time) srflxFailureState {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.observeICESuccess(srflx, now)
	return s.state
}

// snapshot returns a copy under the lock for diagnostic emit. Cheap.
func (s *srflxStateSync) snapshot() srflxFailureState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}
