package lazyconn

import (
	"strings"

	"github.com/hashicorp/go-version"
)

var (
	minVersion = version.Must(version.NewVersion("0.45.0"))
)

// IsDevOrCIBuild reports whether the given agent-version string carries
// a NetBird dev/ci/development build marker. These builds share our
// source tree, so feature-capability checks generally treat them as
// modern. Public so peer/version_legacy.go can share the predicate
// (code-review dedup 2026-06-04).
func IsDevOrCIBuild(agentVersion string) bool {
	if agentVersion == "development" {
		return true
	}
	// Custom dev/CI builds with explicit prefix or embedded marker:
	//   "dev-089a95a", "ci-abcdef"          (bare prefix form)
	//   "0.0.0-dev-1b923aad9", "0.0.0-ci-…" (semver-padded form used by
	//                                        build-android-lib.sh so
	//                                        version.NewVersion can parse)
	return strings.HasPrefix(agentVersion, "dev-") ||
		strings.HasPrefix(agentVersion, "ci-") ||
		strings.Contains(agentVersion, "-dev-") ||
		strings.Contains(agentVersion, "-ci-")
}

// ParseAgentVersion parses a NetBird agent-version string into a
// comparable *version.Version. Returns (nil, false) when the input is
//   - empty
//   - a dev/ci/development build marker (caller decides semantics)
//   - a short hash without '.' (e.g. "a6c5960", "d47be154")
//   - otherwise unparseable
//
// Leading 'v' or 'a' is stripped; trailing build-suffix '-dirty'/'-dev'/
// '-SNAPSHOT'/... is also stripped so go-version's strict parser accepts it.
//
// Helper introduced 2026-06-04 by the code-review-recommended dedup of
// lazyconn/support.IsSupported and peer/version_legacy.isLegacyICE
// CandidateRecv. Both used to inline identical prefix-strip + dev/ci
// predicate logic.
func ParseAgentVersion(agentVersion string) (*version.Version, bool) {
	if agentVersion == "" || IsDevOrCIBuild(agentVersion) {
		return nil, false
	}
	// filter out versions like this: a6c5960, a7d5c522, d47be154
	if !strings.Contains(agentVersion, ".") {
		return nil, false
	}
	parsed, err := version.NewVersion(normalizeVersion(agentVersion))
	if err != nil {
		return nil, false
	}
	return parsed, true
}

func IsSupported(agentVersion string) bool {
	if IsDevOrCIBuild(agentVersion) {
		return true
	}
	parsed, ok := ParseAgentVersion(agentVersion)
	if !ok {
		return false
	}
	return parsed.GreaterThanOrEqual(minVersion)
}

func normalizeVersion(version string) string {
	// Remove prefixes like 'v' or 'a'
	if len(version) > 0 && (version[0] == 'v' || version[0] == 'a') {
		version = version[1:]
	}

	// Remove any suffixes like '-dirty', '-dev', '-SNAPSHOT', etc.
	parts := strings.Split(version, "-")
	return parts[0]
}
