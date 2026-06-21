package activity

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/client/internal/lazyconn"
)

// UDPListener uses UDP sockets for activity detection in kernel mode.
type UDPListener struct {
	wgIface  WgInterface
	peerCfg  lazyconn.PeerConfig
	conn     *net.UDPConn
	endpoint *net.UDPAddr
	done     sync.Mutex

	isClosed atomic.Bool
}

// NewUDPListener creates a listener that detects activity via UDP socket reads.
func NewUDPListener(wgIface WgInterface, cfg lazyconn.PeerConfig) (*UDPListener, error) {
	d := &UDPListener{
		wgIface: wgIface,
		peerCfg: cfg,
	}

	conn, err := d.newConn()
	if err != nil {
		return nil, fmt.Errorf("create UDP connection: %v", err)
	}
	d.conn = conn
	d.endpoint = conn.LocalAddr().(*net.UDPAddr)

	if err := d.createEndpoint(); err != nil {
		return nil, err
	}

	d.done.Lock()
	cfg.Log.Infof("created activity listener: %s", d.conn.LocalAddr().(*net.UDPAddr).String())
	return d, nil
}

// ReadPackets blocks reading from the UDP socket until activity is detected or the listener is closed.
// The returned readResult tells the activity-manager whether it was a real
// activity edge (readActivity) or a close-triggered exit (readClosed).
func (d *UDPListener) ReadPackets() readResult {
	result := readClosed

	for {
		n, remoteAddr, err := d.conn.ReadFromUDP(make([]byte, 1))
		if err != nil {
			if d.isClosed.Load() {
				d.peerCfg.Log.Infof("exit from activity listener")
			} else {
				d.peerCfg.Log.Errorf("failed to read from activity listener: %s", err)
			}
			break
		}

		if n < 1 {
			d.peerCfg.Log.Warnf("received %d bytes from %s, too short", n, remoteAddr)
			continue
		}
		d.peerCfg.Log.Infof("activity detected")
		// V18.30 (2026-06-21): kernel-mode counterpart to V18.19's ICEBind.Send
		// hook. wireguard.ko routes outbound user payload (handshake-init or
		// transport) to this UDP socket — its activity-edge IS the sender-side
		// wake signal we need. Arm the local wake intent so the
		// shouldSkipBootstrapOffer guard opens for OFFER #2..N and the
		// receiver-side V18.18 burst threshold is satisfied.
		//
		// Nil-safety mirrors listener_bind.go: WakeArmer may be nil in tests
		// that construct lazyconn.PeerConfig directly without a peer.Conn.
		if d.peerCfg.WakeArmer != nil {
			d.peerCfg.WakeArmer.ArmLocalWakeIntent(n)
		}
		result = readActivity
		break
	}

	d.peerCfg.Log.Debugf("removing lazy endpoint: %s", d.endpoint.String())
	if err := d.wgIface.RemovePeer(d.peerCfg.PublicKey); err != nil {
		d.peerCfg.Log.Errorf("failed to remove endpoint: %s", err)
	}

	// Ignore close error as it may return "use of closed network connection" if already closed.
	_ = d.conn.Close()
	d.done.Unlock()
	return result
}

// Close stops the listener and cleans up resources.
func (d *UDPListener) Close() {
	d.peerCfg.Log.Infof("closing activity listener: %s", d.conn.LocalAddr().String())
	d.isClosed.Store(true)

	if err := d.conn.Close(); err != nil {
		d.peerCfg.Log.Errorf("failed to close UDP listener: %s", err)
	}
	d.done.Lock()
}

func (d *UDPListener) createEndpoint() error {
	d.peerCfg.Log.Debugf("creating lazy endpoint: %s", d.endpoint.String())
	return d.wgIface.UpdatePeer(d.peerCfg.PublicKey, d.peerCfg.AllowedIPs, 0, d.endpoint, nil)
}

func (d *UDPListener) newConn() (*net.UDPConn, error) {
	addr := &net.UDPAddr{
		Port: 0,
		IP:   listenIP,
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Errorf("failed to create activity listener on %s: %s", addr, err)
		return nil, err
	}

	return conn, nil
}
