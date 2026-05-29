package system

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 3.7l (Fix-A) parser regression tests. Untagged on purpose so
// they run on Linux CI without an Android build environment — the
// parser is plain string handling, no Android dependencies.
//
// Format reminder (Java side, IFaceDiscover.java -> stdnet/discover_mobile.go):
//
//	<name> <idx> <mtu> <up> <bcast> <loop> <p2p> <mcast>|<cidr1> <cidr2> ...

const (
	sampleWLAN = "wlan0 12 1500 true true false false true|192.168.91.154/24 2a04:9546:1c92:9791:e85c:bfd2:155:112e/64 fe80::e85c:bfd2:155:112e%wlan0/64"
	sampleRMnet = "rmnet0 13 1500 true false false false true|10.20.30.40/16 2a04:9546:1234:5678::1/64"
	sampleTun  = "tun0 14 1280 true false false true false|100.87.32.173/16"
	sampleLo   = "lo 1 65536 true false true false false|127.0.0.1/8 ::1/128"
	sampleDown = "rmnet1 15 1500 false false false false true|10.99.0.1/16"
	sampleWG   = "wg0 16 1420 true false false false false|10.0.0.1/24"
)

// TestParseAndroidIFacesNetworkAddresses_WLANOnly: a single up wlan
// interface with one IPv4 + one IPv6 global + one link-local — only
// the two real-global addresses must come back.
func TestParseAndroidIFacesNetworkAddresses_WLANOnly(t *testing.T) {
	addrs := parseAndroidIFacesNetworkAddresses(sampleWLAN)
	require.Len(t, addrs, 2, "expected exactly two addresses (IPv4 + global IPv6), got %v", addrs)

	want := map[string]bool{
		"192.168.91.154/24":                     false,
		"2a04:9546:1c92:9791:e85c:bfd2:155:112e/64": false,
	}
	for _, a := range addrs {
		want[a.NetIP.String()] = true
		assert.Empty(t, a.Mac, "Mac field must be empty for Android entries")
	}
	for s, found := range want {
		assert.True(t, found, "expected address %s missing in result", s)
	}
}

// TestParseAndroidIFacesNetworkAddresses_FiltersTunAndLoAndWG verifies
// that the interface-name filter drops tun*, lo*, wg* — they would
// either be NetBird's own overlay (loop concern) or loopback noise.
func TestParseAndroidIFacesNetworkAddresses_FiltersTunAndLoAndWG(t *testing.T) {
	raw := sampleTun + "\n" + sampleLo + "\n" + sampleWG
	addrs := parseAndroidIFacesNetworkAddresses(raw)
	assert.Empty(t, addrs, "tun/lo/wg interfaces must be filtered, got %v", addrs)
}

// TestParseAndroidIFacesNetworkAddresses_SkipsDownInterface verifies
// that interfaces with the up=false flag are excluded.
func TestParseAndroidIFacesNetworkAddresses_SkipsDownInterface(t *testing.T) {
	addrs := parseAndroidIFacesNetworkAddresses(sampleDown)
	assert.Empty(t, addrs, "down interface must be excluded")
}

// TestParseAndroidIFacesNetworkAddresses_MultiInterface verifies the
// realistic multi-interface case: WiFi + Cellular up, NetBird tun also
// up but filtered, loopback skipped, link-local v6 dropped.
func TestParseAndroidIFacesNetworkAddresses_MultiInterface(t *testing.T) {
	raw := sampleWLAN + "\n" + sampleRMnet + "\n" + sampleTun + "\n" + sampleLo
	addrs := parseAndroidIFacesNetworkAddresses(raw)

	// 2 from wlan (v4 + global v6) + 2 from rmnet (v4 + global v6) = 4
	require.Len(t, addrs, 4)

	want := []string{
		"192.168.91.154/24",
		"2a04:9546:1c92:9791:e85c:bfd2:155:112e/64",
		"10.20.30.40/16",
		"2a04:9546:1234:5678::1/64",
	}
	gotSet := map[string]bool{}
	for _, a := range addrs {
		gotSet[a.NetIP.String()] = true
	}
	for _, w := range want {
		assert.True(t, gotSet[w], "expected %s in result, got=%v", w, gotSet)
	}
}

// TestParseAndroidIFacesNetworkAddresses_LinkLocalDropped verifies the
// 169.254/16 (IPv4) + fe80::/10 (IPv6) link-local filter.
func TestParseAndroidIFacesNetworkAddresses_LinkLocalDropped(t *testing.T) {
	raw := "wlan0 12 1500 true true false false true|169.254.1.2/16 fe80::1234/64 192.168.91.154/24"
	addrs := parseAndroidIFacesNetworkAddresses(raw)

	require.Len(t, addrs, 1, "only the non-link-local address should survive, got %v", addrs)
	assert.Equal(t, "192.168.91.154/24", addrs[0].NetIP.String())
}

// TestParseAndroidIFacesNetworkAddresses_MalformedLinesSkipped guards
// against the empty / pipe-less / broken-header lines that random
// Android Java callsites have produced historically — the parser must
// stay graceful and only yield well-formed entries.
func TestParseAndroidIFacesNetworkAddresses_MalformedLinesSkipped(t *testing.T) {
	raw := "\n\n" +
		"wlan0 12 1500 true true false false true|192.168.91.154/24\n" +
		"this-has-no-pipe\n" +
		"broken-header|10.0.0.1/8\n" +
		"   \n" +
		sampleRMnet
	addrs := parseAndroidIFacesNetworkAddresses(raw)
	// wlan (1 addr) + rmnet (2 addr) = 3
	require.Len(t, addrs, 3, "broken lines should be skipped, valid lines kept; got %v", addrs)
}

// TestParseAndroidIFacesNetworkAddresses_EmptyInput safety case.
func TestParseAndroidIFacesNetworkAddresses_EmptyInput(t *testing.T) {
	assert.Nil(t, parseAndroidIFacesNetworkAddresses(""))
	assert.Nil(t, parseAndroidIFacesNetworkAddresses("\n\n  \n"))
}

// TestIsInterfaceFiltered covers each branch of the name-based filter.
func TestIsInterfaceFiltered(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"tun0", true},
		{"tun1", true},
		{"wg0", true},
		{"wg-clouds", true},
		{"lo", true},
		{"lo0", true},
		{"wlan0", false},
		{"wlan1", false},
		{"rmnet0", false},
		{"rmnet_data0", false},
		{"eth0", false},
		{"ccmni0", false},
		{"", true}, // empty name is filtered
	}
	for _, tc := range cases {
		got := isInterfaceFiltered(tc.name)
		assert.Equal(t, tc.want, got, "isInterfaceFiltered(%q)", tc.name)
	}
}

// TestIsLinkLocal covers v4/v6 link-local and a few normal addresses.
func TestIsLinkLocal(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"169.254.1.1", true},
		{"fe80::1", true},
		{"192.168.91.1", false},
		{"10.20.30.40", false},
		{"2a04:9546::1", false},
		{"::1", false}, // loopback (separate filter), not link-local
	}
	for _, tc := range cases {
		a, err := netip.ParseAddr(tc.addr)
		require.NoError(t, err, "parse %s", tc.addr)
		got := isLinkLocal(a)
		assert.Equal(t, tc.want, got, "isLinkLocal(%s)", tc.addr)
	}
}

// TestAndroidNetworkAddressProvider_ContextRoundtrip verifies the ctx
// helper round-trip: With… stores the provider, From… retrieves it,
// nil-context and absent-key cases return ok=false.
func TestAndroidNetworkAddressProvider_ContextRoundtrip(t *testing.T) {
	p := &fakeAndroidProvider{iface: sampleWLAN}

	// Round-trip
	ctx := WithAndroidNetworkAddressProvider(t.Context(), p)
	got, ok := androidNetworkAddressProviderFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, p, got)

	// Absent
	_, ok = androidNetworkAddressProviderFromContext(t.Context())
	assert.False(t, ok)

	// Nil ctx
	_, ok = androidNetworkAddressProviderFromContext(nil)
	assert.False(t, ok)

	// nil provider must not panic and must NOT install a nil value
	ctx = WithAndroidNetworkAddressProvider(t.Context(), nil)
	_, ok = androidNetworkAddressProviderFromContext(ctx)
	assert.False(t, ok, "WithAndroidNetworkAddressProvider(nil) must skip installation")
}

// fakeAndroidProvider is a tiny stand-in for the gomobile-bridged real
// IFaceDiscover. Stores the canned `iface` string and an optional err.
type fakeAndroidProvider struct {
	iface string
	err   error
}

func (f *fakeAndroidProvider) IFaces() (string, error) {
	return f.iface, f.err
}
