package internal

import (
	mgmProto "github.com/netbirdio/netbird/shared/management/proto"

	"github.com/netbirdio/netbird/client/internal/peer"
)

// buildRemoteMeta projects a mgmt-server RemotePeerConfig into the
// peer.RemoteMeta value used by Status.UpdatePeerRemoteMeta. Extracted
// from the engine.go remotePeers loop so the field-by-field mapping
// can be unit-tested without spinning up the whole Engine.
//
// Codex v3.1 review point P3: the "AgentVersion: rp.GetAgentVersion()"
// line is the most fragile part of the UI Connection-Type-Display
// rollout (one-line change inside a 13-field literal). Extracting the
// builder lets us assert the mapping deterministically.
func buildRemoteMeta(rp *mgmProto.RemotePeerConfig) peer.RemoteMeta {
	return peer.RemoteMeta{
		EffectiveConnectionMode:    rp.GetEffectiveConnectionMode(),
		EffectiveRelayTimeoutSecs:  rp.GetEffectiveRelayTimeoutSecs(),
		EffectiveP2PTimeoutSecs:    rp.GetEffectiveP2PTimeoutSecs(),
		EffectiveP2PRetryMaxSecs:   rp.GetEffectiveP2PRetryMaxSecs(),
		ConfiguredConnectionMode:   rp.GetConfiguredConnectionMode(),
		ConfiguredRelayTimeoutSecs: rp.GetConfiguredRelayTimeoutSecs(),
		ConfiguredP2PTimeoutSecs:   rp.GetConfiguredP2PTimeoutSecs(),
		ConfiguredP2PRetryMaxSecs:  rp.GetConfiguredP2PRetryMaxSecs(),
		Groups:                     rp.GetGroups(),
		LastSeenAtServer:           peer.TimestampOrZero(rp.GetLastSeenAtServer()),
		LiveOnline:                 rp.GetLiveOnline(),
		ServerLivenessKnown:        rp.GetServerLivenessKnown(),
		AgentVersion:               rp.GetAgentVersion(),
	}
}
