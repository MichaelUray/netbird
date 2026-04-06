//go:build !android

package system

import "net"

func getNetInterfaces() ([]net.Interface, error) {
	return net.Interfaces()
}

func getInterfaceAddrs(iface *net.Interface) ([]net.Addr, error) {
	return iface.Addrs()
}
