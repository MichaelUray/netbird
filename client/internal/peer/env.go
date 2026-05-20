package peer

import (
	"os"
	"runtime"
	"strings"
	"sync/atomic"
)

const (
	EnvKeyNBForceRelay       = "NB_FORCE_RELAY"
	EnvKeyNBHomeRelayServers = "NB_HOME_RELAY_SERVERS"
)

// serverForceRelay holds the management-server-pushed force_relay
// PeerConfig field. Engine sets this on each Sync update; readers
// (peer.Conn.Open et al.) OR-combine it with the env-var override.
//
// Step 1 of the connection-mode redesign tracked in #5989 -- a single
// boolean wire field (server-pushed) is intentionally minimal so the
// change can land independently of the broader ConnectionMode enum
// work, which is sequenced as Step 2 of the same ladder.
var serverForceRelay atomic.Bool

// SetServerForceRelay records the management-server-pushed force_relay
// flag. Called by engine.go on PeerConfig updates.
func SetServerForceRelay(b bool) {
	serverForceRelay.Store(b)
}

// ServerForceRelay returns the current management-server-pushed
// force_relay flag (without OR-combining with the env override).
// Useful for surfacing the raw value in status output.
func ServerForceRelay() bool {
	return serverForceRelay.Load()
}

func IsForceRelayed() bool {
	if runtime.GOOS == "js" {
		return true
	}
	if strings.EqualFold(os.Getenv(EnvKeyNBForceRelay), "true") {
		return true
	}
	return serverForceRelay.Load()
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
