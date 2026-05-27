package peer

import (
	"os"
	"strings"
	"testing"
)

// TestConn_RelayDisconnect_RecoversWhenIntentionallyDetached ensures
// handleRelayDisconnectedLocked invokes the lazy-recovery callback
// when the relay drops while ICE is in intentional-detach state.
//
// Bug pattern (production-reproduced on dk20 -> 80AFCAB57262 on 2026-05-27):
//  1. Peer connected via P2P (ICE)
//  2. Remote signaled GO_IDLE -> ICE detached, intentionallyDetached=true
//  3. Relay subsequently disconnected (network event)
//  4. Peer stuck: Status=Connecting, no listener, no endpoint
//  5. Phase-3.7j guard correctly skips offers (intentionalDetach predicate)
//     but assumes activity edge will re-attach; no such edge fires
//     because lazyconn has no fake-endpoint bind for this peer
//  6. Stuck forever until manual `systemctl restart netbird`
//
// Fix: in handleRelayDisconnectedLocked, when intentionallyDetached &&
// no ICE listener, invoke the onWGTimeoutRecover callback (which routes
// through ConnMgr.RecoverPeerToIdle -> lazyConnMgr.DeactivatePeer).
//
// Static-text check that mirrors the existing
// conn_lazy_keepwgpeer_test.go style: cheap, catches accidental
// reverts, points at the exact landmark when broken.
func TestConn_RelayDisconnect_RecoversWhenIntentionallyDetached(t *testing.T) {
	src, err := os.ReadFile("conn.go")
	if err != nil {
		t.Fatalf("read conn.go: %v", err)
	}
	body := string(src)

	relayBody := extractFunctionBody(t, body, "handleRelayDisconnectedLocked")
	if relayBody == "" {
		t.Fatalf("could not locate handleRelayDisconnectedLocked function body")
	}

	// Recovery landmark: must check IsIntentionallyDetached.
	const guard = "IsIntentionallyDetached()"
	if !strings.Contains(relayBody, guard) {
		t.Errorf("handleRelayDisconnectedLocked missing %q check — peer can become stuck after relay drop in intentional-detach state (no listener, no recovery path)", guard)
	}

	// Recovery landmark: must invoke onWGTimeoutRecover (same callback
	// as WG-handshake-timeout path; routes to ConnMgr.RecoverPeerToIdle).
	const recoverCB = "onWGTimeoutRecover"
	if !strings.Contains(relayBody, recoverCB) {
		t.Errorf("handleRelayDisconnectedLocked missing %q invocation — without this, intentionally-detached peer cannot return to lazy-listening after relay drop", recoverCB)
	}

	// The callback must be invoked in a goroutine (it routes back into
	// ConnMgr -> Conn.Close which needs conn.mu; we hold it here).
	const goCB = "go cb()"
	if !strings.Contains(relayBody, goCB) {
		t.Errorf("handleRelayDisconnectedLocked missing %q — must spawn goroutine so callback doesn't deadlock on conn.mu", goCB)
	}
}
