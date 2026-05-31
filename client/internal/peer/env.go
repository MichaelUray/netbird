package peer

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/shared/connectionmode"
)

const (
	EnvKeyNBConnectionMode   = "NB_CONNECTION_MODE"
	EnvKeyNBForceRelay       = "NB_FORCE_RELAY"
	EnvKeyNBHomeRelayServers = "NB_HOME_RELAY_SERVERS"

	envEnableLazyConn      = "NB_ENABLE_EXPERIMENTAL_LAZY_CONN"
	envInactivityThreshold = "NB_LAZY_CONN_INACTIVITY_THRESHOLD"

	// Track C 2026-05-31: legacy candidate replay knobs.
	envLegacyCandidateReplayEnabled = "NB_LEGACY_CANDIDATE_REPLAY"
	envLegacyCandidateReplayDelayMs = "NB_LEGACY_CANDIDATE_REPLAY_DELAY_MS"
)

// legacyCandidateReplayEnabled reports whether the Track-C
// legacy-scoped ICE-candidate-replay path is active. Default
// ENABLED. Set NB_LEGACY_CANDIDATE_REPLAY=false (or "0") to disable.
//
// Codex Plan v3 review: On Android, env vars are sourced via the
// EnvVarPackager which currently does not propagate user-set shell
// env. So this knob is effectively only usable on Linux/Windows
// builds OR if we later extend EnvVarPackager. The Android rollback
// path is APK-revert to the previous build without Track-C.
func legacyCandidateReplayEnabled() bool {
	v := os.Getenv(envLegacyCandidateReplayEnabled)
	if v == "" {
		return true
	}
	return strings.EqualFold(v, "true") || v == "1"
}

// legacyCandidateReplayDelay returns the duration MaybeReplayCandidates
// sleeps before reading the gate + snapshot. Default 300ms; clamped
// to [0, 5000ms]. Override via NB_LEGACY_CANDIDATE_REPLAY_DELAY_MS.
//
// The delay needs to be long enough that, on the OnRemoteOffer-
// receiver-role path, Handshaker.Listen has time to process the
// offer (→ WorkerICE.OnNewOffer → reCreateAgent → gather()) so the
// replay snapshot captures the freshly-gathered candidates. 300ms
// is empirical-safe vs an observed Elmira race window of 100-200ms.
func legacyCandidateReplayDelay() time.Duration {
	v := os.Getenv(envLegacyCandidateReplayDelayMs)
	if v == "" {
		return 300 * time.Millisecond
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 5000 {
		return 300 * time.Millisecond
	}
	return time.Duration(n) * time.Millisecond
}

var deprecationOnce sync.Map // env-var name -> *sync.Once

// IsForceRelayed reports whether legacy NB_FORCE_RELAY is set, plus the
// runtime-special-case js (always relayed because of browser limitations).
//
// Deprecated: prefer ResolveModeFromEnv. Kept for callers that haven't
// migrated yet (Phase 1 backwards compat).
func IsForceRelayed() bool {
	if runtime.GOOS == "js" {
		return true
	}
	return strings.EqualFold(os.Getenv(EnvKeyNBForceRelay), "true")
}

// ResolveModeFromEnv reads all three legacy env vars plus the new
// NB_CONNECTION_MODE, applies the documented precedence and returns
// the resolved Mode and relay-timeout (in seconds, 0 if unset).
//
// Precedence:
//  1. NB_CONNECTION_MODE if parseable -> wins
//  2. NB_FORCE_RELAY=true             -> ModeRelayForced (most-restrictive)
//  3. NB_ENABLE_EXPERIMENTAL_LAZY_CONN=true -> ModeP2PLazy
//  4. otherwise                       -> ModeUnspecified (caller falls through)
//
// NB_LAZY_CONN_INACTIVITY_THRESHOLD is parsed independently as the
// relay-timeout (alias) and emits a deprecation-warning if used.
func ResolveModeFromEnv() (connectionmode.Mode, uint32) {
	mode := connectionmode.ModeUnspecified

	if raw := os.Getenv(EnvKeyNBConnectionMode); raw != "" {
		parsed, err := connectionmode.ParseString(raw)
		if err != nil {
			log.Warnf("ignoring %s=%q: %v", EnvKeyNBConnectionMode, raw, err)
		} else if parsed != connectionmode.ModeUnspecified {
			mode = parsed
		}
	}

	if mode == connectionmode.ModeUnspecified {
		if strings.EqualFold(os.Getenv(EnvKeyNBForceRelay), "true") {
			warnDeprecated(EnvKeyNBForceRelay, EnvKeyNBConnectionMode+"=relay-forced")
			mode = connectionmode.ModeRelayForced
		} else if isLazyEnvTrue() {
			warnDeprecated(envEnableLazyConn, EnvKeyNBConnectionMode+"=p2p-lazy")
			mode = connectionmode.ModeP2PLazy
		}
	}

	timeoutSecs := uint32(0)
	if raw := os.Getenv(envInactivityThreshold); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			timeoutSecs = uint32(d.Seconds())
			warnDeprecated(envInactivityThreshold, "the relay_timeout setting on the management server")
		} else {
			log.Warnf("ignoring %s=%q: %v", envInactivityThreshold, raw, err)
		}
	}

	return mode, timeoutSecs
}

func isLazyEnvTrue() bool {
	v, err := strconv.ParseBool(os.Getenv(envEnableLazyConn))
	return err == nil && v
}

func warnDeprecated(envName, replacement string) {
	once, _ := deprecationOnce.LoadOrStore(envName, &sync.Once{})
	once.(*sync.Once).Do(func() {
		log.Warnf("env var %s is deprecated; use %s instead. The legacy var still works in this release but may be removed in a future major version.", envName, replacement)
	})
}

// OverrideRelayURLs returns the relay server URL list set in
// NB_HOME_RELAY_SERVERS (comma-separated) and a boolean indicating whether
// the override is active. When the env var is unset, the boolean is false
// and the caller should keep the list received from the management server.
// Intended for lab/debug scenarios where a peer must pin to a specific home
// relay regardless of what management offers.
func OverrideRelayURLs() ([]string, bool) {
	raw := os.Getenv(EnvKeyNBHomeRelayServers)
	if raw == "" {
		return nil, false
	}
	parts := strings.Split(raw, ",")
	urls := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			urls = append(urls, p)
		}
	}
	if len(urls) == 0 {
		return nil, false
	}
	return urls, true
}
