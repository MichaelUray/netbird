package peer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findPeerInFullStatus is a tiny helper so the AgentVersion tests stay
// readable; offline peers are merged into FullStatus.Peers along with
// online ones, so we can use a single lookup for both code paths.
func findPeerInFullStatus(d *Status, pubKey string) (State, bool) {
	for _, st := range d.GetFullStatus().Peers {
		if st.PubKey == pubKey {
			return st, true
		}
	}
	return State{}, false
}

// TestUpdatePeerRemoteMeta_AgentVersion_Online covers the Codex v3.1 P1
// monotone-update rule for online peers: empty values never erase a
// previously-known version.
func TestUpdatePeerRemoteMeta_AgentVersion_Online(t *testing.T) {
	d := NewRecorder("https://mgmt.example.test")
	const pubKey = "test-agent-version-online"
	require.NoError(t, d.AddPeer(pubKey, "peer.example.test", "100.64.0.10"))

	// First sync: agent version "0.51.2" sets the field.
	require.NoError(t, d.UpdatePeerRemoteMeta(pubKey, RemoteMeta{AgentVersion: "0.51.2"}))
	st, err := d.GetPeer(pubKey)
	require.NoError(t, err)
	assert.Equal(t, "0.51.2", st.AgentVersion)

	// Second sync with empty AgentVersion — monotone guard MUST keep
	// the previously-known value.
	require.NoError(t, d.UpdatePeerRemoteMeta(pubKey, RemoteMeta{AgentVersion: ""}))
	st, err = d.GetPeer(pubKey)
	require.NoError(t, err)
	assert.Equal(t, "0.51.2", st.AgentVersion, "empty AgentVersion must NOT overwrite a known value")

	// Third sync with a new non-empty value — must overwrite.
	require.NoError(t, d.UpdatePeerRemoteMeta(pubKey, RemoteMeta{AgentVersion: "0.55.0"}))
	st, err = d.GetPeer(pubKey)
	require.NoError(t, err)
	assert.Equal(t, "0.55.0", st.AgentVersion)
}

// TestUpdatePeerRemoteMeta_AgentVersion_Offline mirrors the online
// test but uses an offline peer (peer is in d.offlinePeers, not
// d.peers). Both code paths must apply the monotone guard.
func TestUpdatePeerRemoteMeta_AgentVersion_Offline(t *testing.T) {
	d := NewRecorder("https://mgmt.example.test")
	const pubKey = "test-agent-version-offline"
	d.ReplaceOfflinePeers([]State{
		{
			PubKey:       pubKey,
			FQDN:         "offline-peer.example.test",
			IP:           "100.64.0.20",
			ConnStatus:   StatusIdle,
			AgentVersion: "0.51.0",
		},
	})

	// Empty sync must NOT erase the known version on offlinePeers either.
	require.NoError(t, d.UpdatePeerRemoteMeta(pubKey, RemoteMeta{AgentVersion: ""}))
	st, found := findPeerInFullStatus(d, pubKey)
	require.True(t, found)
	assert.Equal(t, "0.51.0", st.AgentVersion, "offline-peer: empty must keep known value")

	// New version flows through.
	require.NoError(t, d.UpdatePeerRemoteMeta(pubKey, RemoteMeta{AgentVersion: "0.52.0"}))
	st, found = findPeerInFullStatus(d, pubKey)
	require.True(t, found)
	assert.Equal(t, "0.52.0", st.AgentVersion)
}
