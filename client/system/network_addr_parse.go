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
// Filter rules (Codex Fix-A):
//   - skip loopback (lo)
//   - skip down interfaces
//   - skip tun/wg (netbird's own overlay)
//   - skip empty / unparseable lines
//   - skip IPv6 link-local (fe80::/10) and IPv4 169.254/16
//   - drop CIDR entries with the special "%" zone-id suffix (link-scoped)
//
// MAC address is left blank — Android does not surface it through this
// channel, and the iOS/Desktop paths already tolerate that.
func parseAndroidIFacesNetworkAddresses(raw string) []NetworkAddress {
	if raw == "" {
		return nil
	}
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
			out = append(out, NetworkAddress{
				NetIP: prefix,
			})
		}
	}
	return out
}

// isInterfaceFiltered returns true for interface names that should NOT
// be reported to the management server.
//
//   - tun*    -> NetBird's own overlay
//   - wg*     -> other WireGuard interfaces (loop concern)
//   - lo*     -> loopback
//
// Other prefixes (wlan, rmnet, ccmni, eth, usb, dummy, ...) are kept.
func isInterfaceFiltered(name string) bool {
	if name == "" {
		return true
	}
	prefixes := []string{"tun", "wg", "lo"}
	for _, p := range prefixes {
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
