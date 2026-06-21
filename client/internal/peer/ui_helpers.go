package peer

import (
	nbproto "github.com/netbirdio/netbird/client/proto"
)

// IsLegacyPeer returns true if the given NetBird agent version is in
// the range known to predate compatibility fixes (Track-C ICE-init-
// race in v0.51.x, modern mode-negotiation). Mirrors the ceiling used
// by isLegacyICECandidateRecv so both decisions (server-side replay
// and UI [Legacy] tag) stay in sync.
//
// Empty/dev/ci versions return false (assumed modern). Treat empty as
// "we don't know" rather than "modern" only at the UI layer if you
// want to hide the tag entirely for unknown peers.
//
// Used by the daemon proto-mapper to populate
// proto.PeerState.mode_reason_code, and by Fyne/CLI UIs as the
// canonical "is this peer legacy" check. Android+iOS receive the
// boolean derived from this via proto and don't need to re-implement
// the parser.
func IsLegacyPeer(agentVersion string) bool {
	return isLegacyICECandidateRecv(agentVersion)
}

// DeriveModeReasonCode returns the daemon-derived reason for a
// EffectiveConnectionMode / ConfiguredConnectionMode mismatch, using
// the proto enum directly (Codex v3 review: do not invent a parallel
// internal type — UIs receive the proto enum and translate per
// surface).
//
// Priority:
//  1. No effective mode known yet   -> NONE
//  2. Modes match                   -> NONE
//  3. Peer is legacy (<0.54 per
//     V18.32; see version_legacy.go)  -> LEGACY_PEER
//  4. Peer is modern, configured
//     mode known                    -> SERVER_OVERRIDE
//  5. Everything else (e.g. agent
//     version unknown)              -> UNKNOWN
//
// Callers should always check NONE first and skip rendering when
// returned -- there is nothing for the user to act on.
func DeriveModeReasonCode(s State) nbproto.ModeReasonCode {
	if s.RemoteEffectiveConnectionMode == "" {
		return nbproto.ModeReasonCode_MODE_REASON_NONE
	}
	if s.RemoteEffectiveConnectionMode == s.RemoteConfiguredConnectionMode {
		return nbproto.ModeReasonCode_MODE_REASON_NONE
	}
	if IsLegacyPeer(s.AgentVersion) {
		return nbproto.ModeReasonCode_MODE_REASON_LEGACY_PEER
	}
	if s.AgentVersion != "" && s.RemoteConfiguredConnectionMode != "" {
		return nbproto.ModeReasonCode_MODE_REASON_SERVER_OVERRIDE
	}
	return nbproto.ModeReasonCode_MODE_REASON_UNKNOWN
}

// ModeReasonCodeString returns a stable kebab/snake string for use in
// JSON/YAML outputs (CLI, scripts). Returns empty string for NONE so
// callers can omit it from output entirely.
//
// Codex v3.1 review point P4: stable strings beat raw enum ints for
// human/script consumers.
func ModeReasonCodeString(code nbproto.ModeReasonCode) string {
	switch code {
	case nbproto.ModeReasonCode_MODE_REASON_LEGACY_PEER:
		return "legacy_peer"
	case nbproto.ModeReasonCode_MODE_REASON_SERVER_OVERRIDE:
		return "server_override"
	case nbproto.ModeReasonCode_MODE_REASON_UNKNOWN:
		return "unknown"
	}
	return ""
}
