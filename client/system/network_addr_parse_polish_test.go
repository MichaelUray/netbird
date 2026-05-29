package system

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 3.7l (Fix-A) polish — Codex 2026-05-29 review followups.
//
// Pins the post-review behaviour:
//   - Duplicate prefixes across interfaces are de-duped
//   - Extended overlay-interface filter (utun, ipsec, zt, tailscale, nordlynx)
//   - maxAndroidReportedAddresses cap stops unbounded growth on IPv6
//     privacy-temp churn

// TestParseAndroidIFacesNetworkAddresses_DuplicatesDeduped verifies the
// dedupe contract: identical CIDRs surfacing on multiple interfaces
// (common Android pattern with rmnet0 + rmnet_data0 aliases) yield
// exactly one NetworkAddress.
func TestParseAndroidIFacesNetworkAddresses_DuplicatesDeduped(t *testing.T) {
	raw := "wlan0 12 1500 true true false false true|192.168.91.154/24 2a04:9546:1c92:9791::1/64\n" +
		"rmnet0 13 1500 true false false false true|2a04:9546:1c92:9791::1/64\n" + // SAME global v6 → dup
		"rmnet_data0 14 1500 true false false false true|192.168.91.154/24 2a04:9546:1c92:9791::1/64" // both already seen
	addrs := parseAndroidIFacesNetworkAddresses(raw)
	require.Len(t, addrs, 2, "duplicate prefixes across interfaces should collapse to one each, got %v", addrs)

	want := map[string]bool{
		"192.168.91.154/24":          false,
		"2a04:9546:1c92:9791::1/64":  false,
	}
	for _, a := range addrs {
		want[a.NetIP.String()] = true
	}
	for s, found := range want {
		assert.True(t, found, "expected %s to survive dedupe, got=%v", s, addrs)
	}
}

// TestParseAndroidIFacesNetworkAddresses_CapEnforced verifies the
// maxAndroidReportedAddresses cap. Generate >32 unique IPv6 addresses
// on a single interface and ensure the output is exactly the cap.
func TestParseAndroidIFacesNetworkAddresses_CapEnforced(t *testing.T) {
	// Build a single wlan0 line with 40 unique global IPv6 addresses.
	var b strings.Builder
	b.WriteString("wlan0 12 1500 true true false false true|")
	for i := 0; i < 40; i++ {
		// 2001:db8::N/128 — RFC 3849 documentation prefix, guaranteed unique
		b.WriteString("2001:db8::")
		b.WriteString(itoaHex(i))
		b.WriteString("/128 ")
	}
	addrs := parseAndroidIFacesNetworkAddresses(b.String())
	assert.LessOrEqual(t, len(addrs), maxAndroidReportedAddresses,
		"output exceeded maxAndroidReportedAddresses=%d (got %d)",
		maxAndroidReportedAddresses, len(addrs))
	assert.Equal(t, maxAndroidReportedAddresses, len(addrs),
		"cap should fill to exactly maxAndroidReportedAddresses (deterministic), got %d",
		len(addrs))
}

func itoaHex(n int) string {
	const hex = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = hex[n&0xf]
		n >>= 4
	}
	return string(buf[i:])
}

// TestIsInterfaceFiltered_ExtendedOverlayPrefixes verifies the polish
// extension to cover non-NetBird overlay implementations.
func TestIsInterfaceFiltered_ExtendedOverlayPrefixes(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Pre-polish: already filtered
		{"tun0", true},
		{"wg0", true},
		{"lo", true},
		// Post-polish: newly filtered
		{"utun0", true},          // macOS-style userspace tun
		{"utun5", true},
		{"ipsec0", true},         // StrongSwan / Android IPsec
		{"ipsec_ike", true},
		{"zt0", true},            // ZeroTier
		{"zt-xxx", true},
		{"tailscale0", true},     // Tailscale
		{"nordlynx", true},       // NordVPN WireGuard
		{"nordlynx0", true},
		// Real interfaces must still pass
		{"wlan0", false},
		{"rmnet0", false},
		{"rmnet_data0", false},
		{"eth0", false},
		{"ccmni0", false},
		{"dummy0", false}, // dummy interface — could be argued either way, not in filter
	}
	for _, tc := range cases {
		got := isInterfaceFiltered(tc.name)
		assert.Equal(t, tc.want, got,
			"isInterfaceFiltered(%q) = %v, want %v", tc.name, got, tc.want)
	}
}
