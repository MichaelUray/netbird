package lazyconn

import (
	"net/netip"

	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/client/iface/bind"
	"github.com/netbirdio/netbird/client/internal/peer/id"
)

type PeerConfig struct {
	PublicKey  string
	AllowedIPs []netip.Prefix
	PeerConnID id.ConnID
	Log        *log.Entry

	// WakeArmer is V18.19 (2026-06-21): when non-nil, the bind layer
	// fires ArmLocalWakeIntent on this on real user payload (>32 B,
	// transport type) destined for the peer. Typically set to the
	// owning *peer.Conn in conn_mgr.go (which satisfies the interface
	// via Phase 1's ArmLocalWakeIntent method). Pass nil to opt-out of
	// sender-side local wake intent (e.g. when lazyconn.Manager is
	// driving a synthetic peer in tests).
	//
	// Interface-typed (bind.WakeIntentArmer) to avoid an import cycle —
	// lazyconn must NOT import client/internal/peer. The peer package
	// already implements ArmLocalWakeIntent in Phase 1 (conn.go:556).
	WakeArmer bind.WakeIntentArmer
}
