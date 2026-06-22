package peer

import "testing"

// TestConn_IsRemotePeerLazyAware_DevShortcut documents AND locks the
// H2 trust-shortcut in Conn.IsRemotePeerLazyAware (see conn.go:687).
// The shortcut delegates to lazyconn.IsDevOrCIBuild on the unauthen-
// ticated remote AgentVersion string, which means a peer (or a
// compromised mgmt-server feed) advertising "dev-x" / "ci-x" /
// "0.0.0-dev-x" / "9.9.9-dev-x" trips the LazyAware-true branch even
// when its real version is well below v18_31LazyAwareCeiling (0.65.0).
//
// Codex 2026-06-22 v2 review classified this as soft-gate (anti-spam
// burst-release framing, not access control) for the controlled
// MichaelUray fleet, so the production behaviour stays. This test
// fixes that behaviour as the SPEC: every entry below is a contract
// the V18.x sprint commits to.
//
// PAIRING (intentional): the dev/CI shortcut in IsRemotePeerLazyAware
// and the adversarial cases in this test MUST be removed together in
// any future refactor that replaces the version-string trust path
// with an explicit capability-bit on the signal-protocol wire
// (upstream-PR future work — see THREAT-MODEL CAVEAT block on
// IsRemotePeerLazyAware). Until then this test prevents silent drift.
//
// Test path is the PRODUCTION path: each sub-case constructs a fresh
// Conn via newMarkerTestConn (so the StatusRecorder is fresh — no
// monotone-empty cross-pollution between cases), seeds the peer via
// StatusRecorder.AddPeer, optionally publishes AgentVersion via
// StatusRecorder.UpdatePeerRemoteMeta, then asserts on
// Conn.IsRemotePeerLazyAware(). Any future refactor of the lookup
// chain (GetPeer → AgentVersion → IsDevOrCIBuild → ParseAgentVersion)
// is therefore observable by this test (Codex review v2 amendment T5).
func TestConn_IsRemotePeerLazyAware_DevShortcut(t *testing.T) {
	cases := []struct {
		name          string
		agentVersion  string
		wantLazyAware bool
	}{
		// Happy-path: empty / modern / boundary
		{"empty drops (no sync yet)", "", false},
		{"explicit 'development' allowed", "development", true},
		{"bare dev-prefix allowed", "dev-1b923aad9", true},
		{"bare ci-prefix allowed", "ci-abcdef", true},
		{"semver-padded 0.0.0-dev allowed", "0.0.0-dev-1b923aad9", true},
		{"modern stable 0.68.0 allowed", "0.68.0", true},
		{"exactly at ceiling 0.65.0 allowed", "0.65.0", true},
		{"just below ceiling 0.64.999 denied", "0.64.999", false},
		{"0.54.0 legacy denied", "0.54.0", false},

		// Adversarial: known dev/CI-shortcut bypass — locked as
		// CURRENT behaviour, paired with the shortcut for removal.
		{"ADVERSARIAL 9.9.9-dev-x allowed (gap)", "9.9.9-dev-x", true},
		{"ADVERSARIAL 9.9.9-ci-x allowed (gap)", "9.9.9-ci-x", true},

		// Garbage / unparseable
		{"garbage drops", "not-a-version", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := newMarkerTestConn(t)
			// conn.config.Key is what IsRemotePeerLazyAware looks up
			// (see conn.go:688 — statusRecorder.GetPeer(conn.config.Key)).
			remoteKey := conn.config.Key

			if err := conn.statusRecorder.AddPeer(remoteKey, "fqdn", "ip"); err != nil {
				t.Fatalf("AddPeer: %v", err)
			}

			if tc.agentVersion != "" {
				if err := conn.statusRecorder.UpdatePeerRemoteMeta(remoteKey, RemoteMeta{
					AgentVersion: tc.agentVersion,
				}); err != nil {
					t.Fatalf("UpdatePeerRemoteMeta: %v", err)
				}
			}

			got := conn.IsRemotePeerLazyAware()
			if got != tc.wantLazyAware {
				t.Fatalf("IsRemotePeerLazyAware() for agentVersion=%q = %v, want %v",
					tc.agentVersion, got, tc.wantLazyAware)
			}
		})
	}
}
