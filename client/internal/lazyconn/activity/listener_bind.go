package activity

import (
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/netbirdio/netbird/client/iface/device"
	"github.com/netbirdio/netbird/client/internal/lazyconn"
)

type bindProvider interface {
	GetBind() device.EndpointManager
}

const (
	// lazyBindPort is an obscure port used for lazy peer endpoints to avoid confusion with real peers.
	// The actual routing is done via fakeIP in ICEBind, not by this port.
	lazyBindPort = 17473
)

// BindListener uses lazyConn with bind implementations for direct data passing in userspace bind mode.
type BindListener struct {
	wgIface WgInterface
	peerCfg lazyconn.PeerConfig
	done    sync.WaitGroup

	lazyConn *lazyConn
	bind     device.EndpointManager
	fakeIP   netip.Addr
}

// NewBindListener creates a listener that passes data directly through bind using LazyConn.
// It automatically derives a unique fake IP from the peer's NetBird IP in the 127.2.x.x range.
func NewBindListener(wgIface WgInterface, bind device.EndpointManager, cfg lazyconn.PeerConfig) (*BindListener, error) {
	fakeIP, err := deriveFakeIP(wgIface, cfg.AllowedIPs)
	if err != nil {
		return nil, fmt.Errorf("derive fake IP: %w", err)
	}

	d := &BindListener{
		wgIface: wgIface,
		peerCfg: cfg,
		bind:    bind,
		fakeIP:  fakeIP,
	}

	if err := d.setupLazyConn(); err != nil {
		return nil, fmt.Errorf("setup lazy connection: %v", err)
	}

	d.done.Add(1)
	return d, nil
}

// deriveFakeIP creates a deterministic fake IP for bind mode based on peer's NetBird IP.
// Maps peer IP 100.64.x.y to fake IP 127.2.x.y (similar to relay proxy using 127.1.x.y).
// It finds the peer's actual NetBird IP by checking which allowedIP is in the same subnet as our WG interface.
func deriveFakeIP(wgIface WgInterface, allowedIPs []netip.Prefix) (netip.Addr, error) {
	if len(allowedIPs) == 0 {
		return netip.Addr{}, fmt.Errorf("no allowed IPs for peer")
	}

	ourNetwork := wgIface.Address().Network

	var peerIP netip.Addr
	for _, allowedIP := range allowedIPs {
		ip := allowedIP.Addr()
		if !ip.Is4() {
			continue
		}
		if ourNetwork.Contains(ip) {
			peerIP = ip
			break
		}
	}

	if !peerIP.IsValid() {
		return netip.Addr{}, fmt.Errorf("no peer NetBird IP found in allowed IPs")
	}

	octets := peerIP.As4()
	fakeIP := netip.AddrFrom4([4]byte{127, 2, octets[2], octets[3]})
	return fakeIP, nil
}

func (d *BindListener) setupLazyConn() error {
	d.lazyConn = newLazyConnWithLabel(d.peerCfg.PublicKey)
	// V18.19 (2026-06-21): nil armer for now — the lazyconn package
	// cannot import the peer package, so wiring the peer.Conn through
	// here requires either a callback hook plumbed via lazyconn.PeerConfig
	// or a setter API on BindListener invoked from conn_mgr.go after
	// the listener is created. Phase 2 keeps the bind-layer plumbing
	// in place; Phase 3 will wire the actual armer end-to-end.
	d.bind.SetEndpoint(d.fakeIP, d.lazyConn, nil)

	endpoint := &net.UDPAddr{
		IP:   d.fakeIP.AsSlice(),
		Port: lazyBindPort,
	}
	return d.wgIface.UpdatePeer(d.peerCfg.PublicKey, d.peerCfg.AllowedIPs, 0, endpoint, nil)
}

// refreshEndpoint re-binds the fake-IP on the ICEBind and re-installs it
// as the WG-peer endpoint. Used by activity.Manager.RefreshLazyEndpoint
// to recover from an external path that overrode the WG-peer endpoint
// (relay-proxy switch, then relay drop) and left the lazy-bind listener
// orphaned.
//
// Reuses the existing lazyConn (no Close/recreate) so the running
// ReadPackets goroutine continues to observe activity edges. Safe to
// call concurrently with ReadPackets — bind.SetEndpoint and
// wgIface.UpdatePeer are themselves idempotent.
func (d *BindListener) refreshEndpoint() error {
	// V18.19 (2026-06-21): nil armer for now — the lazyconn package
	// cannot import the peer package, so wiring the peer.Conn through
	// here requires either a callback hook plumbed via lazyconn.PeerConfig
	// or a setter API on BindListener invoked from conn_mgr.go after
	// the listener is created. Phase 2 keeps the bind-layer plumbing
	// in place; Phase 3 will wire the actual armer end-to-end.
	d.bind.SetEndpoint(d.fakeIP, d.lazyConn, nil)

	endpoint := &net.UDPAddr{
		IP:   d.fakeIP.AsSlice(),
		Port: lazyBindPort,
	}
	return d.wgIface.UpdatePeer(d.peerCfg.PublicKey, d.peerCfg.AllowedIPs, 0, endpoint, nil)
}

// ReadPackets blocks until activity is detected on the LazyConn or the listener is closed.
// The returned readResult tells the activity-manager whether it was a real
// activity edge (readActivity) or a close/cancel-triggered exit (readClosed).
func (d *BindListener) ReadPackets() readResult {
	result := readClosed

	select {
	case <-d.lazyConn.ActivityChan():
		d.peerCfg.Log.Infof("activity detected via LazyConn")
		result = readActivity
	case <-d.lazyConn.ctx.Done():
		d.peerCfg.Log.Infof("exit from activity listener")
	}

	if result == readActivity {
		d.peerCfg.Log.Debugf("preserving lazy WG peer entry for peer %s after activity", d.peerCfg.PublicKey)
		d.done.Done()
		return result
	}

	d.peerCfg.Log.Debugf("removing lazy endpoint for peer %s", d.peerCfg.PublicKey)
	if err := d.wgIface.RemovePeer(d.peerCfg.PublicKey); err != nil {
		d.peerCfg.Log.Errorf("failed to remove endpoint: %s", err)
	}
	_ = d.lazyConn.Close()
	d.bind.RemoveEndpoint(d.fakeIP)
	d.done.Done()
	return result
}

// Close stops the listener and cleans up resources.
func (d *BindListener) Close() {
	d.peerCfg.Log.Infof("closing activity listener (LazyConn)")

	if err := d.lazyConn.Close(); err != nil {
		d.peerCfg.Log.Errorf("failed to close LazyConn: %s", err)
	}

	d.done.Wait()
}
