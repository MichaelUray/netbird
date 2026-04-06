//go:build android

package system

import (
	"net"

	"github.com/wlynxg/anet"
)

func getNetInterfaces() ([]net.Interface, error) {
	return anet.Interfaces()
}

func getInterfaceAddrs(iface *net.Interface) ([]net.Addr, error) {
	return anet.InterfaceAddrsByInterface(iface)
}
