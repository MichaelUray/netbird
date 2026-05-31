package peer

import (
	"testing"

	"github.com/stretchr/testify/assert"

	nbproto "github.com/netbirdio/netbird/client/proto"
)

func TestIsLegacyPeer(t *testing.T) {
	cases := []struct {
		version string
		legacy  bool
	}{
		{"", false},
		{"development", false},
		{"dev-foo-abc", false},
		{"ci-bar", false},
		{"0.68.0-dev-trackc-abc", false},
		{"0.51.2", true},
		{"v0.51.2", true},
		{"a0.51.2", true},
		{"0.51", true},
		{"0.52.0", false},
		{"0.55.0", false},
		{"1.0.0", false},
		{"not-a-version", false},
	}
	for _, c := range cases {
		got := IsLegacyPeer(c.version)
		assert.Equalf(t, c.legacy, got, "IsLegacyPeer(%q)", c.version)
	}
}

func TestDeriveModeReasonCode(t *testing.T) {
	cases := []struct {
		name    string
		state   State
		want    nbproto.ModeReasonCode
		wantTag string
	}{
		{
			name: "no effective mode -> NONE",
			state: State{
				RemoteEffectiveConnectionMode:  "",
				RemoteConfiguredConnectionMode: "p2p-dynamic",
				AgentVersion:                   "0.51.2",
			},
			want: nbproto.ModeReasonCode_MODE_REASON_NONE,
		},
		{
			name: "modes match -> NONE",
			state: State{
				RemoteEffectiveConnectionMode:  "p2p-dynamic",
				RemoteConfiguredConnectionMode: "p2p-dynamic",
				AgentVersion:                   "0.55.0",
			},
			want: nbproto.ModeReasonCode_MODE_REASON_NONE,
		},
		{
			name: "downgrade for legacy peer -> LEGACY_PEER",
			state: State{
				RemoteEffectiveConnectionMode:  "p2p-lazy",
				RemoteConfiguredConnectionMode: "p2p-dynamic",
				AgentVersion:                   "0.51.2",
			},
			want:    nbproto.ModeReasonCode_MODE_REASON_LEGACY_PEER,
			wantTag: "legacy_peer",
		},
		{
			name: "modern peer with mismatch -> SERVER_OVERRIDE",
			state: State{
				RemoteEffectiveConnectionMode:  "p2p-lazy",
				RemoteConfiguredConnectionMode: "p2p-dynamic",
				AgentVersion:                   "0.55.0",
			},
			want:    nbproto.ModeReasonCode_MODE_REASON_SERVER_OVERRIDE,
			wantTag: "server_override",
		},
		{
			name: "modern peer, configured mode empty -> UNKNOWN",
			state: State{
				RemoteEffectiveConnectionMode:  "p2p-lazy",
				RemoteConfiguredConnectionMode: "",
				AgentVersion:                   "0.55.0",
			},
			want:    nbproto.ModeReasonCode_MODE_REASON_UNKNOWN,
			wantTag: "unknown",
		},
		{
			name: "mismatch + agent version unknown -> UNKNOWN",
			state: State{
				RemoteEffectiveConnectionMode:  "p2p-lazy",
				RemoteConfiguredConnectionMode: "p2p-dynamic",
				AgentVersion:                   "",
			},
			want:    nbproto.ModeReasonCode_MODE_REASON_UNKNOWN,
			wantTag: "unknown",
		},
		{
			name: "dev peer with mismatch treated as modern -> SERVER_OVERRIDE",
			state: State{
				RemoteEffectiveConnectionMode:  "p2p-lazy",
				RemoteConfiguredConnectionMode: "p2p-dynamic",
				AgentVersion:                   "0.68.0-dev-trackc-abc",
			},
			want:    nbproto.ModeReasonCode_MODE_REASON_SERVER_OVERRIDE,
			wantTag: "server_override",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeriveModeReasonCode(c.state)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.wantTag, ModeReasonCodeString(got))
		})
	}
}

func TestModeReasonCodeString_NoneIsEmpty(t *testing.T) {
	// Stable contract: NONE always serializes to empty for omitempty.
	assert.Empty(t, ModeReasonCodeString(nbproto.ModeReasonCode_MODE_REASON_NONE))
}
