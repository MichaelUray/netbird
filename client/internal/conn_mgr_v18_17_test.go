package internal

import (
	"context"
	"net/netip"
	"testing"

	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/client/internal/peer/dispatcher"
	"github.com/netbirdio/netbird/client/internal/peer/guard"
	"github.com/netbirdio/netbird/client/internal/peer/ice"
	"github.com/netbirdio/netbird/shared/connectionmode"
	sProto "github.com/netbirdio/netbird/shared/signal/proto"
)

const peerKeyAlphaV18_17 = "0OEicbABQa7BaiavfsxggK/+H8GBNpZD5q4WdCpgcGI="

// newV18_17TestConn builds a *peer.Conn with conn.config.Mode set to
// ModeP2PDynamic so the downstream V18.10 AttachICEOnRemoteOffer gate
// is exercised in the V14 path. The base helper newTestConnWithRemoteMode
// leaves Mode unset (ModeUnspecified) which short-circuits the V18.10
// gate — not the path the burst-release tests need to verify.
func newV18_17TestConn(t *testing.T, peerKey string) *peer.Conn {
	t.Helper()
	recorder := peer.NewRecorder("https://mgm")
	swWatcher := guard.NewSRWatcher(nil, nil, nil, ice.Config{})

	cfg := peer.ConnConfig{
		Key:      peerKey,
		LocalKey: "RRHf3Ma6z6mdLbriAJbqhX7+nM/B71lgw2+91q3LfhU=",
		WgConfig: peer.WgConfig{
			RemoteKey:  peerKey,
			AllowedIps: []netip.Prefix{netip.MustParsePrefix("100.64.0.5/32")},
		},
		Mode: connectionmode.ModeP2PDynamic,
	}
	sd := peer.ServiceDependencies{
		StatusRecorder:     recorder,
		SrWatcher:          swWatcher,
		PeerConnDispatcher: dispatcher.NewConnectionDispatcher(),
	}
	conn, err := peer.NewConn(cfg, sd)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	if err := recorder.AddPeer(peerKey, "", ""); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	return conn
}

// newV18_17TestHarness builds a ConnMgr+Conn pair. The Conn is left in
// the "lazy-detached + everConnected" V14 state by default; callers
// flip everConnected=false to exercise V15.
func newV18_17TestHarness(t *testing.T, mode connectionmode.Mode) (*ConnMgr, *peer.Conn) {
	t.Helper()

	cm := newConnMgrWithLazyMgr(t)
	cm.mode = mode

	conn := newV18_17TestConn(t, peerKeyAlphaV18_17)
	conn.MarkIntentionallyDetached()
	conn.SetEverConnectedForTest(true)
	if added := cm.peerStore.AddPeerConn(peerKeyAlphaV18_17, conn); !added {
		t.Fatalf("peer already exists in store unexpectedly")
	}
	return cm, conn
}

// TestConnMgr_V18_17_V14_3rdOfferReachesAttachAndConsumesFlag verifies
// the CORRECT invariant (Codex Round-3 audit 2026-06-21):
//  1. First 2 OFFERs are V14-blocked → flag NOT armed, lazy activation
//     count unchanged.
//  2. 3rd OFFER releases V14 → fall-through proceeds to
//     lazyConnMgr.ActivatePeer (count++) AND ClearIntentionallyDetached
//     OR V18.10 ConsumeBurstReleasePending — either way the flag is
//     FALSE at end (not armed-stuck).
//
// This is the test design that the previous plan version got wrong:
// asserting flag=TRUE after the call would prove only the half-wired
// state (V14 release armed but V18.10/Clear did not consume).
func TestConnMgr_V18_17_V14_3rdOfferReachesAttachAndConsumesFlag(t *testing.T) {
	cm, conn := newV18_17TestHarness(t, connectionmode.ModeP2PDynamic)
	ctx := context.Background()

	preCount := cm.lazyConnMgr.ActivationCountForKey(peerKeyAlphaV18_17)

	// 1st OFFER: V14 blocks (V18.18 threshold=2, count=1 < 2).
	cm.ActivatePeerForMessage(ctx, conn, sProto.Body_OFFER)
	if conn.IsBurstReleasePendingLoad() {
		t.Fatal("V14: 1st OFFER must NOT arm release-pending")
	}
	if got := cm.lazyConnMgr.ActivationCountForKey(peerKeyAlphaV18_17); got != preCount {
		t.Fatalf("V14: 1st OFFER must NOT reach lazyConnMgr.ActivatePeer (count %d, want %d)", got, preCount)
	}

	// 2nd OFFER: V14+V18.17 releases (V18.18 threshold=2). Fall-through
	// reaches ActivatePeer AND consumes the flag (Clear or V18.10).
	cm.ActivatePeerForMessage(ctx, conn, sProto.Body_OFFER)
	if got := cm.lazyConnMgr.ActivationCountForKey(peerKeyAlphaV18_17); got != preCount+1 {
		t.Fatalf("V14+V18.17: 2nd OFFER MUST reach lazyConnMgr.ActivatePeer (count %d, want %d)", got, preCount+1)
	}
	if conn.IsBurstReleasePendingLoad() {
		t.Fatal("V14+V18.17: 2nd OFFER must NOT leave flag armed — either Clear or V18.10 must have consumed it in the same call")
	}
}

// TestConnMgr_V18_17_V15_3rdOfferReachesAttachAndConsumesFlag — same
// invariant for V15 cold-boot path. V18.18: 2nd OFFER triggers (not 3rd).
func TestConnMgr_V18_17_V15_3rdOfferReachesAttachAndConsumesFlag(t *testing.T) {
	cm, conn := newV18_17TestHarness(t, connectionmode.ModeP2PDynamic)
	conn.SetEverConnectedForTest(false)
	conn.ClearIntentionallyDetached() // V15 precondition: !IsLazyDetached AND !EverConnected
	ctx := context.Background()

	preCount := cm.lazyConnMgr.ActivationCountForKey(peerKeyAlphaV18_17)

	// 1st OFFER: V15 blocks (count=1 < 2).
	cm.ActivatePeerForMessage(ctx, conn, sProto.Body_OFFER)
	if conn.IsBurstReleasePendingLoad() {
		t.Fatal("V15: 1st OFFER must NOT arm release-pending")
	}
	if got := cm.lazyConnMgr.ActivationCountForKey(peerKeyAlphaV18_17); got != preCount {
		t.Fatalf("V15: 1st OFFER must NOT reach lazyConnMgr.ActivatePeer (count %d, want %d)", got, preCount)
	}

	// 2nd OFFER: V15+V18.17 releases.
	cm.ActivatePeerForMessage(ctx, conn, sProto.Body_OFFER)
	if got := cm.lazyConnMgr.ActivationCountForKey(peerKeyAlphaV18_17); got != preCount+1 {
		t.Fatalf("V15+V18.17: 2nd OFFER MUST reach lazyConnMgr.ActivatePeer (count %d, want %d)", got, preCount+1)
	}
	if conn.IsBurstReleasePendingLoad() {
		t.Fatal("V15+V18.17: 2nd OFFER must NOT leave flag armed")
	}
}

// TestConnMgr_V18_17_NonOfferMsgsStillBlockedByV14 — non-OFFER msgs
// neither arm the flag nor reach the attach path.
func TestConnMgr_V18_17_NonOfferMsgsStillBlockedByV14(t *testing.T) {
	cm, conn := newV18_17TestHarness(t, connectionmode.ModeP2PDynamic)
	ctx := context.Background()

	preCount := cm.lazyConnMgr.ActivationCountForKey(peerKeyAlphaV18_17)
	for i := 0; i < 5; i++ {
		cm.ActivatePeerForMessage(ctx, conn, sProto.Body_CANDIDATE)
		cm.ActivatePeerForMessage(ctx, conn, sProto.Body_ANSWER)
		cm.ActivatePeerForMessage(ctx, conn, sProto.Body_MODE)
	}
	if conn.IsBurstReleasePendingLoad() {
		t.Fatal("V14: non-OFFER msgs must NOT arm release-pending")
	}
	if got := cm.lazyConnMgr.ActivationCountForKey(peerKeyAlphaV18_17); got != preCount {
		t.Fatalf("V14: non-OFFER msgs must NOT reach lazyConnMgr.ActivatePeer (count %d, want %d)", got, preCount)
	}
}

// TestConnMgr_V18_17_OneShotConsume — calling V18.10 path twice with
// only ONE pre-armed flag: first attach gets the bypass, second does not.
func TestConnMgr_V18_17_OneShotConsume(t *testing.T) {
	_, conn := newV18_17TestHarness(t, connectionmode.ModeP2PDynamic)

	conn.SetBurstReleasePending()
	if !conn.ConsumeBurstReleasePending() {
		t.Fatal("first Consume must return true after SetBurstReleasePending")
	}
	if conn.ConsumeBurstReleasePending() {
		t.Fatal("second Consume must return false — flag is one-shot")
	}
}
