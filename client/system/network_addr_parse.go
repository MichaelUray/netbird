package system

import (
	"fmt"
	"net/netip"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Phase 3.7l (Fix-A) — parser for the Android IFaceDiscover output.
// Kept in an OS-agnostic file so the Linux CI can unit-test it without
// an Android build environment. Codex recommendation 2026-05-29.

// maxAndroidReportedAddresses is the conservative cap on how many
// distinct NetworkAddress entries Fix-A reports to the management
// server. Codex review-polish 2026-05-29: Android can rotate IPv6
// privacy/temporary addresses every few hours, so an uncapped report
// could grow unboundedly over a long-running daemon session. 32 is
// well above the realistic count (typical: 6-12 across WiFi + cellular
// + virtual interfaces) and far below any practical concern.
const maxAndroidReportedAddresses = 32

// parseAndroidIFacesNetworkAddresses parses the newline-separated,
// pipe-delimited interface description produced by
// io.netbird.client.tool.IFaceDiscover.iFaces() (the Java side) and
// returns the subset of addresses we want the management server's
// posture-check to evaluate.
//
// Format (matches client/internal/stdnet/discover_mobile.go parser):
//
//	<name> <idx> <mtu> <up> <bcast> <loop> <p2p> <mcast>|<cidr1> <cidr2> ...
//
// Filter rules (Codex Fix-A + 2026-05-29 review-polish):
//   - skip loopback (lo)
//   - skip down interfaces
//   - skip overlay interfaces (tun, wg, utun, ipsec, zt, tailscale,
//     nordlynx) — these all carry encapsulated traffic and should not
//     contribute to posture-check evaluation
//   - skip empty / unparseable lines
//   - skip IPv6 link-local (fe80::/10) and IPv4 169.254/16
//   - drop CIDR entries with the special "%" zone-id suffix (link-scoped)
//   - de-duplicate identical prefixes within a single call
//   - cap output at maxAndroidReportedAddresses to bound memory growth
//
// MAC address is left blank — Android does not surface it through this
// channel, and the iOS/Desktop paths already tolerate that.
func parseAndroidIFacesNetworkAddresses(raw string) []NetworkAddress {
	if raw == "" {
		return nil
	}
	// seen tracks already-emitted prefixes to dedupe across interfaces.
	// Android often reports the same global IPv6 on multiple aliases
	// (e.g. rmnet0 + rmnet_data0); the management server only needs
	// to see each unique prefix once.
	seen := make(map[string]struct{})
	var out []NetworkAddress
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 2)
		if len(fields) != 2 {
			log.Tracef("parseAndroidIFacesNetworkAddresses: skipping malformed line %q", line)
			continue
		}

		var name string
		var idx, mtu int
		var up, broadcast, loopback, pointToPoint, multicast bool
		if _, err := fmt.Sscanf(fields[0],
			"%s %d %d %t %t %t %t %t",
			&name, &idx, &mtu, &up, &broadcast, &loopback, &pointToPoint, &multicast); err != nil {
			log.Tracef("parseAndroidIFacesNetworkAddresses: header parse failed %q: %v", fields[0], err)
			continue
		}

		if !up || loopback || isInterfaceFiltered(name) {
			continue
		}

		addrs := strings.TrimSpace(fields[1])
		if addrs == "" {
			continue
		}
		for _, raw := range strings.Fields(addrs) {
			if strings.Contains(raw, "%") {
				continue
			}
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				log.Tracef("parseAndroidIFacesNetworkAddresses: skip %q: %v", raw, err)
				continue
			}
			if isLinkLocal(prefix.Addr()) {
				continue
			}
			key := prefix.String()
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, NetworkAddress{
				NetIP: prefix,
			})
			// Cap defence: stop adding entries once we hit the cap.
			// We DON'T early-return because the outer loop's bookkeeping
			// (dedupe map, malformed-line skip log) stays useful for
			// diagnostic purposes; just stop appending.
			if len(out) >= maxAndroidReportedAddresses {
				log.Warnf("parseAndroidIFacesNetworkAddresses: capped at %d entries (more available; check IPv6 privacy-addr churn)",
					maxAndroidReportedAddresses)
				return out
			}
		}
	}
	return out
}

// overlayInterfacePrefixes lists name prefixes that identify
// encapsulated / overlay interfaces. Anything matching is excluded from
// the posture-check report because the addresses on these devices are
// VPN-internal and would either pollute the report or create
// posture-check loops (NetBird's own tun, other WG/Tailscale/ZeroTier
// overlays, etc.).
//
// Codex review-polish 2026-05-29: extended beyond {tun, wg, lo} to also
// cover utun (macOS-style), ipsec (Android StrongSwan/IPsec), zt
// (ZeroTier), tailscale, nordlynx (NordVPN WG).
var overlayInterfacePrefixes = []string{
	"tun",
	"wg",
	"lo",
	"utun",
	"ipsec",
	"zt",
	"tailscale",
	"nordlynx",
}

// isInterfaceFiltered returns true for interface names that should NOT
// be reported to the management server. See overlayInterfacePrefixes for
// the rationale of each prefix.
func isInterfaceFiltered(name string) bool {
	if name == "" {
		return true
	}
	for _, p := range overlayInterfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// isLinkLocal returns true for IPv4 169.254/16 and IPv6 fe80::/10
// addresses. The posture check has no use for them and they vary
// across reboots, polluting the meta_network_addresses field.
func isLinkLocal(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	if addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return true
	}
	return false
}
