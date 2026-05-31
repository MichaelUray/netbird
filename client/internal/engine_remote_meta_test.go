package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/timestamppb"

	mgmProto "github.com/netbirdio/netbird/shared/management/proto"
)

// TestBuildRemoteMeta_IncludesAgentVersion guards the one-line Codex
// v3.1 Phase-2 change: the new AgentVersion field must be projected
// 1:1 from RemotePeerConfig into RemoteMeta.
//
// Without this assertion a future refactor of the literal could silently
// drop the field and the [Legacy] tag would stop appearing in UIs.
func TestBuildRemoteMeta_IncludesAgentVersion(t *testing.T) {
	rp := &mgmProto.RemotePeerConfig{
		WgPubKey:     "test-pub",
		AgentVersion: "0.51.2",
	}
	got := buildRemoteMeta(rp)
	assert.Equal(t, "0.51.2", got.AgentVersion)
}

// TestBuildRemoteMeta_EmptyAgentVersion documents that an empty
// version flows through 1:1. The monotone "do not erase known value"
// rule is the responsibility of UpdatePeerRemoteMeta, not of the
// projection. Keeping buildRemoteMeta pure makes both layers simpler
// to reason about.
func TestBuildRemoteMeta_EmptyAgentVersion(t *testing.T) {
	rp := &mgmProto.RemotePeerConfig{WgPubKey: "test-pub"}
	got := buildRemoteMeta(rp)
	assert.Empty(t, got.AgentVersion)
}

// TestBuildRemoteMeta_AllFields asserts the full RemoteMeta projection
// so adding a new RemotePeerConfig field is forced through a test
// update — defensive against silent drift between proto and meta.
func TestBuildRemoteMeta_AllFields(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	rp := &mgmProto.RemotePeerConfig{
		WgPubKey:                   "test-pub",
		EffectiveConnectionMode:    "p2p-dynamic",
		EffectiveRelayTimeoutSecs:  10,
		EffectiveP2PTimeoutSecs:    20,
		EffectiveP2PRetryMaxSecs:   30,
		ConfiguredConnectionMode:   "p2p-lazy",
		ConfiguredRelayTimeoutSecs: 11,
		ConfiguredP2PTimeoutSecs:   21,
		ConfiguredP2PRetryMaxSecs:  31,
		Groups:                     []string{"dev", "qa"},
		LastSeenAtServer:           timestamppb.New(now),
		LiveOnline:                 true,
		ServerLivenessKnown:        true,
		AgentVersion:               "0.68.0-dev-trackc-abc",
	}
	got := buildRemoteMeta(rp)

	assert.Equal(t, "p2p-dynamic", got.EffectiveConnectionMode)
	assert.Equal(t, uint32(10), got.EffectiveRelayTimeoutSecs)
	assert.Equal(t, uint32(20), got.EffectiveP2PTimeoutSecs)
	assert.Equal(t, uint32(30), got.EffectiveP2PRetryMaxSecs)
	assert.Equal(t, "p2p-lazy", got.ConfiguredConnectionMode)
	assert.Equal(t, uint32(11), got.ConfiguredRelayTimeoutSecs)
	assert.Equal(t, uint32(21), got.ConfiguredP2PTimeoutSecs)
	assert.Equal(t, uint32(31), got.ConfiguredP2PRetryMaxSecs)
	assert.Equal(t, []string{"dev", "qa"}, got.Groups)
	assert.Equal(t, now, got.LastSeenAtServer)
	assert.True(t, got.LiveOnline)
	assert.True(t, got.ServerLivenessKnown)
	assert.Equal(t, "0.68.0-dev-trackc-abc", got.AgentVersion)
}
