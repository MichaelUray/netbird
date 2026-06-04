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
// Hardware-test pending against Lethbridge v0.53.0, dolice-bg-r1
// v0.53.0, LunzAmSee-nr-r1 v0.53.0. If those also race, raise the
// ceiling to "0.54.0" or "0.55.0". We do NOT preemptively widen to
// "0.60.0" since the upstream commit fixing the bug at a specific
// version is not known — our own branch still has the same
// agent-nil-return pattern, so we cannot assume any modern version
// is automatically immune.
var legacyCandidateRecvCeiling = version.Must(version.NewVersion("0.52.0"))

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
