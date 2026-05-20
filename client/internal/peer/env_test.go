package peer

import (
	"os"
	"testing"
)

// IsForceRelayed must OR-combine the env-var override with the
// server-pushed force_relay flag. The boolean is set by the
// engine on every Sync update via SetServerForceRelay.
//
// Step 1 of the connection-mode redesign tracked in #5989. The
// env-var override remains so existing deployments are unchanged.
func TestIsForceRelayed_EnvOrServer(t *testing.T) {
	prevEnv := os.Getenv(EnvKeyNBForceRelay)
	prevServer := ServerForceRelay()
	t.Cleanup(func() {
		_ = os.Setenv(EnvKeyNBForceRelay, prevEnv)
		SetServerForceRelay(prevServer)
	})

	cases := []struct {
		name   string
		env    string
		server bool
		want   bool
	}{
		{"neither set", "", false, false},
		{"env=true", "true", false, true},
		{"server=true only", "", true, true},
		{"both set", "true", true, true},
		{"env=false", "false", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_ = os.Setenv(EnvKeyNBForceRelay, c.env)
			SetServerForceRelay(c.server)
			if got := IsForceRelayed(); got != c.want {
				t.Errorf("IsForceRelayed() = %v, want %v (env=%q server=%v)", got, c.want, c.env, c.server)
			}
		})
	}
}
