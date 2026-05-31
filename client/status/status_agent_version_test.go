package status

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/netbirdio/netbird/client/proto"
)

// TestMapPeers_PrefersConnectionTypeExtended verifies Codex v3.1 P2:
// mapPeers must populate ConnType from ConnectionTypeExtended when
// available, so transient "Relayed (negotiating P2P)" labels reach
// the CLI too.
func TestMapPeers_PrefersConnectionTypeExtended(t *testing.T) {
	peers := []*proto.PeerState{
		{
			PubKey:                 "p1",
			IP:                     "100.64.0.1",
			Fqdn:                   "p1.test",
			ConnStatus:             "Connected",
			Relayed:                true, // would yield "Relayed" under fallback
			ConnectionTypeExtended: "Relayed (negotiating P2P)",
		},
	}
	out := mapPeers(peers, "", nil, nil, nil, "")
	if assert.Len(t, out.Details, 1) {
		assert.Equal(t, "Relayed (negotiating P2P)", out.Details[0].ConnType,
			"ConnType must prefer ConnectionTypeExtended")
		assert.Equal(t, "Relayed (negotiating P2P)", out.Details[0].ConnectionTypeExtended)
	}
}

// TestMapPeers_FallbackWhenNoExtended ensures the old (relayed bool)
// heuristic still applies when the daemon pre-dates Phase 3.7i and
// doesn't populate ConnectionTypeExtended.
func TestMapPeers_FallbackWhenNoExtended(t *testing.T) {
	peers := []*proto.PeerState{
		{PubKey: "p1", IP: "100.64.0.1", Fqdn: "p1.test", ConnStatus: "Connected", Relayed: false},
		{PubKey: "p2", IP: "100.64.0.2", Fqdn: "p2.test", ConnStatus: "Connected", Relayed: true},
	}
	out := mapPeers(peers, "", nil, nil, nil, "")
	connTypeByPubKey := map[string]string{}
	for _, d := range out.Details {
		connTypeByPubKey[d.PubKey] = d.ConnType
	}
	assert.Equal(t, "P2P", connTypeByPubKey["p1"])
	assert.Equal(t, "Relayed", connTypeByPubKey["p2"])
}

// TestMapPeers_AgentVersionFlowsThrough covers Phases 4+9: the new
// AgentVersion / IsLegacyPeer / EffectiveConnectionMode /
// ConfiguredConnectionMode / ModeReason fields are populated.
func TestMapPeers_AgentVersionFlowsThrough(t *testing.T) {
	peers := []*proto.PeerState{
		{
			PubKey:                   "legacy",
			IP:                       "100.64.0.10",
			Fqdn:                     "legacy.test",
			ConnStatus:               "Connected",
			ConnectionTypeExtended:   "Relayed",
			AgentVersion:             "0.51.2",
			EffectiveConnectionMode:  "p2p-lazy",
			ConfiguredConnectionMode: "p2p-dynamic",
			ModeReasonCode:           proto.ModeReasonCode_MODE_REASON_LEGACY_PEER,
		},
		{
			PubKey:                  "modern",
			IP:                      "100.64.0.20",
			Fqdn:                    "modern.test",
			ConnStatus:              "Connected",
			ConnectionTypeExtended:  "P2P",
			AgentVersion:            "0.68.0-dev-trackc-abc",
			EffectiveConnectionMode: "p2p-dynamic",
		},
	}
	out := mapPeers(peers, "", nil, nil, nil, "")
	byPubKey := map[string]PeerStateDetailOutput{}
	for _, d := range out.Details {
		byPubKey[d.PubKey] = d
	}

	// Legacy peer: all fields populated, ModeReason "legacy_peer".
	legacy := byPubKey["legacy"]
	assert.Equal(t, "0.51.2", legacy.AgentVersion)
	assert.True(t, legacy.IsLegacyPeer, "0.51.2 must be classified as legacy")
	assert.Equal(t, "p2p-lazy", legacy.EffectiveConnectionMode)
	assert.Equal(t, "p2p-dynamic", legacy.ConfiguredConnectionMode)
	assert.Equal(t, "legacy_peer", legacy.ModeReason)

	// Modern dev peer: AgentVersion present, NOT legacy.
	modern := byPubKey["modern"]
	assert.Equal(t, "0.68.0-dev-trackc-abc", modern.AgentVersion)
	assert.False(t, modern.IsLegacyPeer, "dev-prefixed versions must not be legacy")
	assert.Empty(t, modern.ModeReason, "matching modes -> ModeReason empty (omitempty)")
}

// TestParsePeers_AgentVersionAndDowngradeReason verifies Phase 9 — the
// text output of parsePeers includes the new lines when the daemon
// provides the data, and omits them cleanly when it doesn't.
func TestParsePeers_AgentVersionAndDowngradeReason(t *testing.T) {
	peers := PeersStateOutput{
		Details: []PeerStateDetailOutput{
			{
				FQDN:                     "legacy.test",
				IP:                       "100.64.0.10",
				PubKey:                   "k1",
				Status:                   "Connected",
				ConnType:                 "Relayed",
				AgentVersion:             "0.51.2",
				IsLegacyPeer:             true,
				EffectiveConnectionMode:  "p2p-lazy",
				ConfiguredConnectionMode: "p2p-dynamic",
				ModeReason:               "legacy_peer",
			},
			{
				FQDN:     "minimal.test",
				IP:       "100.64.0.99",
				PubKey:   "k2",
				Status:   "Idle",
				ConnType: "-",
				// no AgentVersion / mode info — old-daemon shape
			},
		},
	}
	out := parsePeers(peers, false, false)

	// Legacy peer: all new lines visible with the expected wording.
	assert.Contains(t, out, "Agent version: 0.51.2  [Legacy]")
	assert.Contains(t, out, "Effective mode: p2p-lazy")
	assert.Contains(t, out, "Configured mode: p2p-dynamic — downgraded because remote peer is on a legacy version")

	// Minimal peer: none of the new lines must appear for it. Easiest
	// check: the text after the second FQDN doesn't contain them.
	idx := strings.Index(out, "minimal.test")
	if assert.GreaterOrEqual(t, idx, 0, "minimal peer must be in the output") {
		tail := out[idx:]
		assert.NotContains(t, tail, "Agent version:")
		assert.NotContains(t, tail, "Effective mode:")
		assert.NotContains(t, tail, "Configured mode:")
	}
}
