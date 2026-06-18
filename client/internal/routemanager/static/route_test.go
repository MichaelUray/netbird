package static

import (
	"net/netip"
	"testing"

	"github.com/netbirdio/netbird/client/internal/routemanager/common"
	"github.com/netbirdio/netbird/client/internal/routemanager/refcounter"
	"github.com/netbirdio/netbird/route"
)

func TestRoute_AddAllowedIPs_IdempotentOnExistingPeer(t *testing.T) {
	prefix := netip.MustParsePrefix("10.1.233.0/24")
	adds := 0
	allowedIPs := refcounter.New(
		func(key netip.Prefix, peerKey string) (string, error) {
			adds++
			if key != prefix {
				t.Fatalf("unexpected prefix: got %s want %s", key, prefix)
			}
			if peerKey != "peerA" {
				t.Fatalf("unexpected peer: got %s want peerA", peerKey)
			}
			return peerKey, nil
		},
		func(netip.Prefix, string) error {
			return nil
		},
	)

	rt := NewRoute(common.HandlerParams{
		Route: &route.Route{
			Network: prefix,
		},
		AllowedIPsRefCounter: allowedIPs,
	})

	if err := rt.AddAllowedIPs("peerA"); err != nil {
		t.Fatalf("first AddAllowedIPs: %v", err)
	}
	if err := rt.AddAllowedIPs("peerA"); err != nil {
		t.Fatalf("second AddAllowedIPs: %v", err)
	}

	if adds != 1 {
		t.Fatalf("underlying add called %d times, want 1", adds)
	}
	ref, ok := allowedIPs.Get(prefix)
	if !ok {
		t.Fatal("expected prefix to remain pinned")
	}
	if ref.Count != 1 {
		t.Fatalf("ref count = %d, want 1", ref.Count)
	}
}
