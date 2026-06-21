package peer

import (
	"github.com/hashicorp/go-version"

	"github.com/netbirdio/netbird/client/internal/lazyconn"
)

// legacyCandidateRecvCeiling is the NetBird version ceiling below
// which we treat the remote peer as suffering the OnRemoteCandidate
// "agent==nil → drop" race in worker_ice.go (Elmira v0.51.2 has
// 413× WARNs on AWOam in 1 day; full investigation in
// docs/test-reports/2026-05-31-elmira-ice-init-race-android-vs-
// legacy/).
//
// Initially set to "0.52.0" to cover only confirmed-affected
// versions:
//
//   - Elmira v0.51.2 (heute 413× WARNs on AWOam)
//
// 2026-06-21 V18.32 raised to "0.54.0" after live hardware
// confirmation that v0.53.0 BG-routers race the same way:
//
//   - User-reported S26 → 10.1.233.51 (Dolice LAN) unreachable
//   - dk20 → 10.1.233.51 (also via dolice-bg-r1) unreachable
//   - dk20 client.log shows OFFER/ANSWER exchange succeeds with
//     dolice-bg-r1 v0.53.0 + "WireGuard handshake timed out"
//     within 10 s, then "Required key not available" on kernel-WG
//   - Same symptom pattern as Elmira v0.51.2: signalling looks
//     healthy, ICE candidates exchange, but the post-establishment
//     handshake never completes because remote candidate frames
//     arrived before remote's pion agent was bootstrapped
//
// 0.54.0 (exclusive) covers 0.51.x / 0.52.x / 0.53.x — exactly the
// set of legacy versions our fleet still has in production
// (BG-routers we cannot easily firmware-update remotely). Plain
// 0.54.0 and above are not (yet) known to race.
//
// We do NOT preemptively widen further (e.g. 0.60.0) since the
// upstream commit fixing the bug at a specific version is not
// known — our own branch still has the same agent-nil-return
// pattern, so we cannot assume any modern version is automatically
// immune.
var legacyCandidateRecvCeiling = version.Must(version.NewVersion("0.54.0"))

// isLegacyICECandidateRecv returns true if the remote peer's
// NetBird version is in the range known to drop ICE candidates
// that arrive before the pion agent is initialized.
//
// Dev/CI builds (with prefix "dev-"/"ci-" or substring "-dev-"/
// "-ci-") are treated as modern (they share our source tree).
// "development" and unparseable short-hash forms are also modern.
//
// Uses lazyconn.ParseAgentVersion for the prefix-trim + dev/ci predicate
// so the legacy-detection and the lazy-support-check stay in sync
// (code-review dedup 2026-06-04).
func isLegacyICECandidateRecv(remoteVersion string) bool {
	parsed, ok := lazyconn.ParseAgentVersion(remoteVersion)
	if !ok {
		return false
	}
	return parsed.LessThan(legacyCandidateRecvCeiling)
}
