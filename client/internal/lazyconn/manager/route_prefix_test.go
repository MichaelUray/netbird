package manager

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/netbirdio/netbird/route"
)

func TestManager_UpdateRouteHAMap_StoresRoutePrefixesForAllRoutingPeers(t *testing.T) {
	h := newTestHarness(t)

	dolice := "dolice-peer"
	marl := "marl-peer"
	h.mgr.UpdateRouteHAMap(route.HAMap{
		"single": []*route.Route{
			{
				ID:          "dolice-resource:route",
				Network:     netip.MustParsePrefix("10.1.233.0/24"),
				NetworkType: route.IPv4Network,
				Peer:        dolice,
			},
		},
		"ha": []*route.Route{
			{
				ID:          "marl-resource:route-a",
				Network:     netip.MustParsePrefix("10.1.41.0/24"),
				NetworkType: route.IPv4Network,
				Peer:        marl,
			},
			{
				ID:          "marl-resource:route-b",
				Network:     netip.MustParsePrefix("10.1.42.0/24"),
				NetworkType: route.IPv4Network,
				Peer:        dolice,
			},
		},
	})

	assertPrefixesEqual(t, h.mgr.peerToRoutePrefixes[dolice], []netip.Prefix{
		netip.MustParsePrefix("10.1.42.0/24"),
		netip.MustParsePrefix("10.1.233.0/24"),
	})
	assertPrefixesEqual(t, h.mgr.peerToRoutePrefixes[marl], []netip.Prefix{
		netip.MustParsePrefix("10.1.41.0/24"),
	})
}

func TestManager_AddPeer_RoutingPeerAllowedIPsIncludeSubnets(t *testing.T) {
	h := newTestHarness(t)
	pubKey := "routing-peer"
	peerIP := netip.MustParsePrefix("100.87.22.246/32")
	subnet := netip.MustParsePrefix("10.1.233.0/24")

	h.mgr.UpdateRouteHAMap(route.HAMap{
		"dolice": []*route.Route{
			{
				ID:          "dolice-resource:route",
				Network:     subnet,
				NetworkType: route.IPv4Network,
				Peer:        pubKey,
			},
			{
				ID:          "duplicate-peer-ip:route",
				Network:     peerIP,
				NetworkType: route.IPv4Network,
				Peer:        pubKey,
			},
		},
	})

	cfg := newTestPeerCfg(pubKey)
	cfg.AllowedIPs = []netip.Prefix{peerIP}
	excluded, err := h.mgr.AddPeer(cfg)
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	if excluded {
		t.Fatal("peer should not be excluded")
	}

	stored := h.mgr.managedPeers[pubKey]
	if stored == nil {
		t.Fatal("peer was not stored")
	}
	assertPrefixesEqual(t, stored.AllowedIPs, []netip.Prefix{subnet, peerIP})

	calls := h.wgIface.UpdateCalls()
	if len(calls) == 0 {
		t.Fatal("expected lazy activity endpoint UpdatePeer call")
	}
	assertPrefixesEqual(t, calls[len(calls)-1].allowedIPs, []netip.Prefix{subnet, peerIP})
}

func assertPrefixesEqual(t *testing.T, got, want []netip.Prefix) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("prefixes:\n got: %v\nwant: %v", got, want)
	}
}
