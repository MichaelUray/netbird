package peer

import (
	"strings"

	"github.com/hashicorp/go-version"
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
// The version-string is run through the same prefix-trim as
// lazyconn/support.go to be tolerant of upstream tagging conventions
// (e.g. "v0.51.2", "a0.51.2").
func isLegacyICECandidateRecv(remoteVersion string) bool {
	if remoteVersion == "" || remoteVersion == "development" {
		return false
	}
	if strings.HasPrefix(remoteVersion, "dev-") ||
		strings.HasPrefix(remoteVersion, "ci-") ||
		strings.Contains(remoteVersion, "-dev-") ||
		strings.Contains(remoteVersion, "-ci-") {
		return false
	}
	if !strings.Contains(remoteVersion, ".") {
		return false
	}
	normalized := remoteVersion
	if len(normalized) > 0 && (normalized[0] == 'v' || normalized[0] == 'a') {
		normalized = normalized[1:]
	}
	parsed, err := version.NewVersion(normalized)
	if err != nil {
		return false
	}
	return parsed.LessThan(legacyCandidateRecvCeiling)
}
