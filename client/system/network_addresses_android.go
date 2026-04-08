//go:build android

package system

import (
	"context"
	"fmt"
	"net"
	"strings"

	log "github.com/sirupsen/logrus"
)

// androidIfaceAddrsKey stores per-call interface→addresses pairs that were
// parsed from the externalDiscover output, so getInterfaceAddrs() can return
// them without reparsing or doing a second IFaces() call.
type androidIfaceAddrsKey struct{}

// getNetInterfaces returns the list of system network interfaces.
//
// On Android the standard library net.Interfaces() relies on a NETLINK_ROUTE
// socket which SELinux blocks since Android 11; it returns an empty list
// without error. To get a usable list we ask the host application (the
// android-client) for the interface description via the
// stdnet.ExternalIFaceDiscover hook that is already used elsewhere
// (mobile_dependencies / discover_mobile.go). The discoverer is passed in
// through the context as IFaceDiscoverCtxKey.
//
// As a fallback, when no discoverer is available (e.g. unit tests), we
// return whatever net.Interfaces() gives us — which may be empty but never
// crashes the caller.
func getNetInterfaces(ctx context.Context) ([]net.Interface, error) {
	discover, ok := ctx.Value(IFaceDiscoverCtxKey).(IFaceDiscoverFunc)
	if !ok || discover == nil {
		return net.Interfaces()
	}

	raw, err := discover()
	if err != nil {
		log.Warnf("network_addresses_android: external iFace discover failed: %v", err)
		return net.Interfaces()
	}

	ifaces, addrMap := parseExternalIfaces(raw)

	// Stash the parsed addresses so getInterfaceAddrs() can read them back
	// without doing another IFaces() round-trip.
	if amap := addressMapFromContext(ctx); amap != nil {
		for name, addrs := range addrMap {
			amap[name] = addrs
		}
	}
	return ifaces, nil
}

// getInterfaceAddrs returns the addresses of a single interface. It first
// looks for a parsed result stashed in the context by getNetInterfaces, and
// only falls back to iface.Addrs() (which is also broken on Android 11+)
// if no parsed result is available.
func getInterfaceAddrs(ctx context.Context, iface *net.Interface) ([]net.Addr, error) {
	if amap := addressMapFromContext(ctx); amap != nil {
		if addrs, ok := amap[iface.Name]; ok {
			return addrs, nil
		}
	}
	return iface.Addrs()
}

// WithIFaceDiscover returns a context that carries an IFaceDiscoverFunc and a
// fresh per-call address cache. The caller (engine) should always wrap the
// context handed to system.GetInfo / system.GetInfoWithChecks on Android.
func WithIFaceDiscover(parent context.Context, discover IFaceDiscoverFunc) context.Context {
	if discover == nil {
		return parent
	}
	ctx := context.WithValue(parent, IFaceDiscoverCtxKey, discover)
	return context.WithValue(ctx, androidIfaceAddrsKey{}, map[string][]net.Addr{})
}

func addressMapFromContext(ctx context.Context) map[string][]net.Addr {
	v := ctx.Value(androidIfaceAddrsKey{})
	if m, ok := v.(map[string][]net.Addr); ok {
		return m
	}
	return nil
}

// parseExternalIfaces parses the newline-separated interface description
// emitted by stdnet.ExternalIFaceDiscover.IFaces(). Each line has the format:
//
//	name index mtu up broadcast loopback pointToPoint multicast|addr1 addr2 ...
//
// We deliberately do NOT skip interfaces without addresses here: posture
// checks need every administrative interface (e.g. a freshly-up wlan0 with
// an IPv4 lease still pending), and a separate pass in networkAddresses()
// will drop entries that have no usable IP.
func parseExternalIfaces(raw string) ([]net.Interface, map[string][]net.Addr) {
	var ifaces []net.Interface
	addrMap := make(map[string][]net.Addr)

	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 2 {
			log.Warnf("network_addresses_android: cannot split iface line %q", line)
			continue
		}

		var name string
		var index, mtu int
		var up, broadcast, loopback, pointToPoint, multicast bool
		_, err := fmt.Sscanf(fields[0], "%s %d %d %t %t %t %t %t",
			&name, &index, &mtu, &up, &broadcast, &loopback, &pointToPoint, &multicast)
		if err != nil {
			log.Warnf("network_addresses_android: cannot parse iface header %q: %v", fields[0], err)
			continue
		}

		ni := net.Interface{
			Name:  name,
			Index: index,
			MTU:   mtu,
		}
		if up {
			ni.Flags |= net.FlagUp
		}
		if broadcast {
			ni.Flags |= net.FlagBroadcast
		}
		if loopback {
			ni.Flags |= net.FlagLoopback
		}
		if pointToPoint {
			ni.Flags |= net.FlagPointToPoint
		}
		if multicast {
			ni.Flags |= net.FlagMulticast
		}
		ifaces = append(ifaces, ni)

		var addrs []net.Addr
		for _, addr := range strings.Split(strings.Trim(fields[1], " \n"), " ") {
			if addr == "" || strings.Contains(addr, "%") {
				continue
			}
			ip, ipNet, err := net.ParseCIDR(addr)
			if err != nil {
				log.Warnf("network_addresses_android: cannot parse addr %q: %v", addr, err)
				continue
			}
			ipNet.IP = ip
			addrs = append(addrs, ipNet)
		}
		addrMap[name] = addrs
	}
	return ifaces, addrMap
}
