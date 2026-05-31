package peer

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/client/iface"
	"github.com/netbirdio/netbird/client/iface/udpmux"
	"github.com/netbirdio/netbird/client/internal/peer/conntype"
	icemaker "github.com/netbirdio/netbird/client/internal/peer/ice"
	"github.com/netbirdio/netbird/client/internal/portforward"
	"github.com/netbirdio/netbird/client/internal/stdnet"
	"github.com/netbirdio/netbird/route"
)

type ICEConnInfo struct {
	RemoteConn                 net.Conn
	RosenpassPubKey            []byte
	RosenpassAddr              string
	LocalIceCandidateType      string
	RemoteIceCandidateType     string
	RemoteIceCandidateEndpoint string
	LocalIceCandidateEndpoint  string
	Relayed                    bool
	RelayedOnLocal             bool
}

type WorkerICE struct {
	ctx               context.Context
	log               *log.Entry
	config            ConnConfig
	conn              *Conn
	signaler          *Signaler
	iFaceDiscover     stdnet.ExternalIFaceDiscover
	statusRecorder    *Status
	hasRelayOnLocally bool

	agent             *icemaker.ThreadSafeAgent
	agentDialerCancel context.CancelFunc
	agentConnecting   bool      // while it is true, drop all incoming offers
	lastSuccess       time.Time // with this avoid the too frequent ICE agent recreation
	// remoteSessionID represents the peer's session identifier from the latest remote offer.
	remoteSessionID ICESessionID
	// sessionID is used to track the current session ID of the ICE agent
	// increase by one when disconnecting the agent
	// with it the remote peer can discard the already deprecated offer/answer
	// Without it the remote peer may recreate a workable ICE connection
	sessionID            ICESessionID
	remoteSessionChanged bool
	muxAgent             sync.Mutex

	localUfrag string
	localPwd   string

	// we record the last known state of the ICE agent to avoid duplicate on disconnected events
	lastKnownState ice.ConnectionState

	// portForwardAttempted tracks if we've already tried port forwarding this session
	portForwardAttempted bool

	// lastLocalSrflx is the most recently discovered server-reflexive
	// candidate's AddrPort. Recorded by onICECandidate when pion reports
	// a CandidateTypeServerReflexive entry. Read by Conn's diagnostic
	// snapshot to detect the "stuck-srflx" pattern (Codex review
	// 2026-05-30): D3b/D2a recreate the agent on retry but the
	// underlying UDP-Mux stays the same, so the srflx port keeps
	// reappearing across retries unless something at NAT/VPN-level
	// rebinds the socket. Tracking this is Phase 1 of the recovery
	// path — pure observation, no behaviour change.
	//
	// Stored as atomic.Value of netip.AddrPort so cross-goroutine reads
	// from snapshotForDiagnosis don't need muxAgent.
	lastLocalSrflx atomic.Value // netip.AddrPort

	// Track C 2026-05-31: legacy-scoped Candidate-Replay state.
	//
	// All locally-gathered ICE candidates that we have sent (or attempted
	// to send) over the Signaler are also appended to sentCandidates so
	// MaybeReplayCandidates() can re-send them to legacy peers that
	// suffer the v0.51.2 OnRemoteCandidate "agent==nil → drop" bug
	// (see docs/test-reports/2026-05-31-elmira-ice-init-race-android-
	// vs-legacy/ for the full investigation + Codex Plan v4 spec).
	//
	// The replayedThisSession bool is a per-session one-shot gate so
	// repeated triggers (D3b-bypass-spam, OnRemoteOffer + OnRemoteAnswer
	// both firing) only produce a single replay per session.
	// replaySkippedAttempts counts how many additional triggers came in
	// after the gate was consumed — purely diagnostic.
	//
	// Reset in reCreateAgent so a new session starts with a clean slate.
	sentCandidatesMu      sync.Mutex
	sentCandidates        []ice.Candidate
	replayedThisSession   bool
	replaySkippedAttempts int
}

func NewWorkerICE(ctx context.Context, log *log.Entry, config ConnConfig, conn *Conn, signaler *Signaler, ifaceDiscover stdnet.ExternalIFaceDiscover, statusRecorder *Status, hasRelayOnLocally bool) (*WorkerICE, error) {
	sessionID, err := NewICESessionID()
	if err != nil {
		return nil, err
	}

	w := &WorkerICE{
		ctx:               ctx,
		log:               log,
		config:            config,
		conn:              conn,
		signaler:          signaler,
		iFaceDiscover:     ifaceDiscover,
		statusRecorder:    statusRecorder,
		hasRelayOnLocally: hasRelayOnLocally,
		lastKnownState:    ice.ConnectionStateDisconnected,
		sessionID:         sessionID,
	}

	localUfrag, localPwd, err := icemaker.GenerateICECredentials()
	if err != nil {
		return nil, err
	}
	w.localUfrag = localUfrag
	w.localPwd = localPwd
	return w, nil
}

func (w *WorkerICE) OnNewOffer(remoteOfferAnswer *OfferAnswer) {
	w.log.Debugf("OnNewOffer for ICE, serial: %s", remoteOfferAnswer.SessionIDString())
	w.muxAgent.Lock()
	defer w.muxAgent.Unlock()

	if w.agent != nil || w.agentConnecting {
		// Phase 3.7c (#5989) re-introduces the Guard-Loop Fix from PR #5805.
		// While the local ICE agent is mid-connection, ignore any incoming
		// offer regardless of sessionID. Both sides' Guards fire fresh
		// offers every ~800ms-30s (driven by their own iceRetryState +
		// srReconnect events). If we tear down on every sessionID-change,
		// the in-flight ICE pair-checks (~5-10s) never complete -- the
		// remote's freshly-recreated agent generates yet another sessionID,
		// loops back, infinite recreate cycle. Empirically observed on
		// badmitterndorf during LTE-carrier instability: 5 different
		// sessionIDs received from the remote in 2min, no P2P convergence.
		if w.agentConnecting {
			w.log.Debugf("agent connecting, skipping new offer (sessionID %s) to let pair-checks finish", remoteOfferAnswer.SessionIDString())
			return
		}
		// backward compatibility with old clients that do not send session ID
		if remoteOfferAnswer.SessionID == nil {
			w.log.Debugf("agent already exists, skipping the offer")
			return
		}
		if w.remoteSessionID == *remoteOfferAnswer.SessionID {
			w.log.Debugf("agent already exists and session ID matches, skipping the offer: %s", remoteOfferAnswer.SessionIDString())
			return
		}
		w.log.Debugf("agent already exists, recreate the connection")
		w.remoteSessionChanged = true
		w.agentDialerCancel()
		if w.agent != nil {
			if err := w.agent.Close(); err != nil {
				w.log.Warnf("failed to close ICE agent: %s", err)
			}
		}

		sessionID, err := NewICESessionID()
		if err != nil {
			w.log.Errorf("failed to create new session ID: %s", err)
		}
		w.sessionID = sessionID
		w.agent = nil
	}

	var preferredCandidateTypes []ice.CandidateType
	if w.hasRelayOnLocally && remoteOfferAnswer.RelaySrvAddress != "" {
		preferredCandidateTypes = icemaker.CandidateTypesP2P()
	} else {
		preferredCandidateTypes = icemaker.CandidateTypes()
	}

	if remoteOfferAnswer.SessionID != nil {
		w.log.Debugf("recreate ICE agent: %s / %s", w.sessionID, *remoteOfferAnswer.SessionID)
	}

	// Phase 3.7l Fix-D Phase 1 — log the underlying UDP-Mux LocalAddr
	// at every agent recreate so offline analysis can confirm Codex'
	// 2026-05-30 hypothesis: the pion Agent is rebuilt on each
	// retry, but the shared UDPMuxSrflx is NOT cycled, so the
	// resulting srflx candidate keeps coming back with the same
	// public port. Two consecutive [DIAG]-recreate lines on the same
	// peer with identical mux_local_addr but a fresh agent instance
	// is the smoking gun. Best-effort: muxLocalAddr returns "n/a" if
	// the mux is not a UniversalUDPMuxDefault or its shared conn is
	// not addressable.
	w.log.Debugf("[DIAG] ice-agent-recreate mux_local_addr=%s prev_local_srflx=%s",
		w.muxLocalAddrStr(), w.lastLocalSrflxOrNone())

	dialerCtx, dialerCancel := context.WithCancel(w.ctx)
	agent, err := w.reCreateAgent(dialerCancel, preferredCandidateTypes)
	if err != nil {
		w.log.Errorf("failed to recreate ICE Agent: %s", err)
		return
	}
	w.agent = agent
	w.agentDialerCancel = dialerCancel
	w.agentConnecting = true
	if remoteOfferAnswer.SessionID != nil {
		w.remoteSessionID = *remoteOfferAnswer.SessionID
	} else {
		w.remoteSessionID = ""
	}

	go w.connect(dialerCtx, agent, remoteOfferAnswer)
}

// OnRemoteCandidate Handles ICE connection Candidate provided by the remote peer.
func (w *WorkerICE) OnRemoteCandidate(candidate ice.Candidate, haRoutes route.HAMap) {
	w.muxAgent.Lock()
	defer w.muxAgent.Unlock()
	w.log.Debugf("OnRemoteCandidate from peer %s -> %s", w.config.Key, candidate.String())
	if w.agent == nil {
		w.log.Warnf("ICE Agent is not initialized yet")
		return
	}

	if candidateViaRoutes(candidate, haRoutes) {
		return
	}

	if err := w.agent.AddRemoteCandidate(candidate); err != nil {
		w.log.Errorf("error while handling remote candidate")
		return
	}

	if shouldAddExtraCandidate(candidate) {
		// sends an extra server reflexive candidate to the remote peer with our related port (usually the wireguard port)
		// this is useful when network has an existing port forwarding rule for the wireguard port and this peer
		extraSrflx, err := extraSrflxCandidate(candidate)
		if err != nil {
			w.log.Errorf("failed creating extra server reflexive candidate %s", err)
			return
		}

		if err := w.agent.AddRemoteCandidate(extraSrflx); err != nil {
			w.log.Errorf("error while handling remote candidate")
			return
		}
	}
}

func (w *WorkerICE) GetLocalUserCredentials() (frag string, pwd string) {
	return w.localUfrag, w.localPwd
}

func (w *WorkerICE) InProgress() bool {
	w.muxAgent.Lock()
	defer w.muxAgent.Unlock()

	return w.agentConnecting
}

// IsConnected returns true when pion's ICE agent reports Connected and
// has not yet transitioned to Disconnected/Failed/Closed. Used by
// Conn.onNetworkChange (Phase 3.7g of #5989) to skip a needless
// workerICE.Close when an srReconnect/network-change event arrives but
// the existing P2P session is still alive end-to-end (typical for a
// brief signal-server outage while peer-to-peer UDP keeps flowing).
// Closing the agent in that case forces a 15-25 s renegotiation cycle
// and a Relay→ICE handover gap that the user would observe as a ping
// dropout, even though no real peer-to-peer connectivity loss occurred.
func (w *WorkerICE) IsConnected() bool {
	w.muxAgent.Lock()
	defer w.muxAgent.Unlock()
	return w.agent != nil && w.lastKnownState == ice.ConnectionStateConnected
}

// IsRetrySafe returns true when the ICE agent is in a state where it is
// safe to tear down the current listener / agent and start a fresh ICE
// negotiation cycle. Concretely: NOT currently mid-connect (agentConnecting)
// AND NOT already Connected. Phase 3.7k+ (Fix B for relay-activity
// stale-listener-gate): used by Conn.AttachICEOnRelayActivity so a
// user-traffic-triggered upgrade can re-attempt ICE when the previous
// agent ended in Failed/Disconnected/Closed without waiting for the
// p2p-dynamic idle-teardown window (~3 min) to clear the stale listener.
//
// State semantics:
//
//	agent == nil              -> retry-safe (no agent at all)
//	agentConnecting == true   -> NOT retry-safe (would race in-flight connect)
//	lastKnownState == Connected -> NOT retry-safe (already P2P, do not disturb)
//	Failed/Disconnected/Closed -> retry-safe (stale, ok to recreate)
//	Checking/New              -> NOT retry-safe (agent making progress)
//
// The Connected vs Checking/New distinction matters: agentConnecting flips
// to false once `connect()` returns even if the state is still in
// transition. Treating Checking/New as retry-safe would create a race
// where the relay-activity path tears down a still-converging agent.
func (w *WorkerICE) IsRetrySafe() bool {
	w.muxAgent.Lock()
	defer w.muxAgent.Unlock()
	if w.agentConnecting {
		return false
	}
	if w.agent == nil {
		return true
	}
	switch w.lastKnownState {
	case ice.ConnectionStateFailed,
		ice.ConnectionStateDisconnected,
		ice.ConnectionStateClosed:
		return true
	default:
		return false
	}
}

func (w *WorkerICE) Close() {
	w.muxAgent.Lock()
	defer w.muxAgent.Unlock()

	if w.agent == nil {
		return
	}

	w.agentDialerCancel()
	if err := w.agent.Close(); err != nil {
		w.log.Warnf("failed to close ICE agent: %s", err)
	}

	w.agent = nil
}

func (w *WorkerICE) reCreateAgent(dialerCancel context.CancelFunc, candidates []ice.CandidateType) (*icemaker.ThreadSafeAgent, error) {
	w.portForwardAttempted = false

	// Track C 2026-05-31: clear the sentCandidates ring AND reset the
	// per-session replay-once gate on each new agent. Replay-Once is
	// per-session, not per-peer-lifetime — a new ICE session must
	// have a fresh chance to replay (Codex Plan v3+v4 review).
	w.sentCandidatesMu.Lock()
	w.sentCandidates = w.sentCandidates[:0]
	w.replayedThisSession = false
	w.replaySkippedAttempts = 0
	w.sentCandidatesMu.Unlock()

	agent, err := icemaker.NewAgent(w.ctx, w.iFaceDiscover, w.config.ICEConfig, candidates, w.localUfrag, w.localPwd)
	if err != nil {
		return nil, fmt.Errorf("create agent: %w", err)
	}

	if err := agent.OnCandidate(w.onICECandidate); err != nil {
		return nil, err
	}

	if err := agent.OnConnectionStateChange(w.onConnectionStateChange(agent, dialerCancel)); err != nil {
		return nil, err
	}

	if err := agent.OnSelectedCandidatePairChange(func(c1, c2 ice.Candidate) {
		w.onICESelectedCandidatePair(agent, c1, c2)
	}); err != nil {
		return nil, err
	}

	return agent, nil
}

func (w *WorkerICE) SessionID() ICESessionID {
	w.muxAgent.Lock()
	defer w.muxAgent.Unlock()

	return w.sessionID
}

// will block until connection succeeded
// but it won't release if ICE Agent went into Disconnected or Failed state,
// so we have to cancel it with the provided context once agent detected a broken connection
func (w *WorkerICE) connect(ctx context.Context, agent *icemaker.ThreadSafeAgent, remoteOfferAnswer *OfferAnswer) {
	w.log.Debugf("gather candidates")
	if err := agent.GatherCandidates(); err != nil {
		w.log.Warnf("failed to gather candidates: %s", err)
		w.closeAgent(agent, w.agentDialerCancel)
		return
	}

	w.log.Debugf("turn agent dial")
	remoteConn, err := w.turnAgentDial(ctx, agent, remoteOfferAnswer)
	if err != nil {
		w.log.Debugf("failed to dial the remote peer: %s", err)
		w.closeAgent(agent, w.agentDialerCancel)
		return
	}
	w.log.Debugf("agent dial succeeded")

	pair, err := agent.GetSelectedCandidatePair()
	if err != nil {
		w.closeAgent(agent, w.agentDialerCancel)
		return
	}
	if pair == nil {
		w.log.Warnf("selected candidate pair is nil, cannot proceed")
		w.closeAgent(agent, w.agentDialerCancel)
		return
	}

	if !isRelayCandidate(pair.Local) {
		// dynamically set remote WireGuard port if other side specified a different one from the default one
		remoteWgPort := iface.DefaultWgPort
		if remoteOfferAnswer.WgListenPort != 0 {
			remoteWgPort = remoteOfferAnswer.WgListenPort
		}

		// To support old version's with direct mode we attempt to punch an additional role with the remote WireGuard port
		go w.punchRemoteWGPort(pair, remoteWgPort)
	}

	ci := ICEConnInfo{
		RemoteConn:                 remoteConn,
		RosenpassPubKey:            remoteOfferAnswer.RosenpassPubKey,
		RosenpassAddr:              remoteOfferAnswer.RosenpassAddr,
		LocalIceCandidateType:      pair.Local.Type().String(),
		RemoteIceCandidateType:     pair.Remote.Type().String(),
		LocalIceCandidateEndpoint:  net.JoinHostPort(pair.Local.Address(), strconv.Itoa(pair.Local.Port())),
		RemoteIceCandidateEndpoint: net.JoinHostPort(pair.Remote.Address(), strconv.Itoa(pair.Remote.Port())),
		Relayed:                    isRelayed(pair),
		RelayedOnLocal:             isRelayCandidate(pair.Local),
	}
	w.log.Debugf("on ICE conn is ready to use")

	w.log.Infof("connection succeeded with offer session: %s", remoteOfferAnswer.SessionIDString())
	w.muxAgent.Lock()
	w.agentConnecting = false
	w.lastSuccess = time.Now()
	w.muxAgent.Unlock()

	// todo: the potential problem is a race between the onConnectionStateChange
	w.conn.onICEConnectionIsReady(selectedPriority(pair), ci)
}

func (w *WorkerICE) closeAgent(agent *icemaker.ThreadSafeAgent, cancel context.CancelFunc) bool {
	cancel()
	if err := agent.Close(); err != nil {
		w.log.Warnf("failed to close ICE agent: %s", err)
	}

	w.muxAgent.Lock()
	defer w.muxAgent.Unlock()

	sessionChanged := w.remoteSessionChanged
	w.remoteSessionChanged = false

	if w.agent == agent {
		// consider to remove from here and move to the OnNewOffer
		sessionID, err := NewICESessionID()
		if err != nil {
			w.log.Errorf("failed to create new session ID: %s", err)
		}
		w.sessionID = sessionID
		w.agent = nil
		w.agentConnecting = false
		w.remoteSessionID = ""
	}
	return sessionChanged
}

func (w *WorkerICE) punchRemoteWGPort(pair *ice.CandidatePair, remoteWgPort int) {
	// wait local endpoint configuration
	time.Sleep(time.Second)
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(pair.Remote.Address(), strconv.Itoa(remoteWgPort)))
	if err != nil {
		w.log.Warnf("got an error while resolving the udp address, err: %s", err)
		return
	}

	mux, ok := w.config.ICEConfig.UDPMuxSrflx.(*udpmux.UniversalUDPMuxDefault)
	if !ok {
		w.log.Warn("invalid udp mux conversion")
		return
	}
	_, err = mux.GetSharedConn().WriteTo([]byte{0x6e, 0x62}, addr)
	if err != nil {
		w.log.Warnf("got an error while sending the punch packet, err: %s", err)
	}
}

// onICECandidate is a callback attached to an ICE Agent to receive new local connection candidates
// and then signals them to the remote peer
func (w *WorkerICE) onICECandidate(candidate ice.Candidate) {
	// nil means candidate gathering has been ended
	if candidate == nil {
		return
	}

	// TODO: reported port is incorrect for CandidateTypeHost, makes understanding ICE use via logs confusing as port is ignored
	w.log.Debugf("discovered local candidate %s", candidate.String())
	w.signalAndRemember(candidate)

	if candidate.Type() == ice.CandidateTypeServerReflexive {
		// Phase 3.7l Phase-1 srflx-tracking (Codex 2026-05-30):
		// record the most recent srflx AddrPort so the per-peer
		// diagnostic snapshot can compare it across retries and
		// detect the "stuck-srflx" pattern (same Magenta-NAT port
		// reappearing across N consecutive ICE failures). pion's
		// ice.Candidate.Address() returns the dotted-string form.
		if a, err := netip.ParseAddr(candidate.Address()); err == nil {
			w.lastLocalSrflx.Store(netip.AddrPortFrom(a.Unmap(), uint16(candidate.Port())))
		}
		w.injectPortForwardedCandidate(candidate)
	}
}

// muxLocalAddrStr returns the LocalAddr of the underlying
// UniversalUDPMuxDefault.GetSharedConn() formatted as a string, or
// "n/a" if the mux is missing / not the expected type / its shared
// conn has no addressable local end. Phase-1 diagnostic helper; not
// hot-path. See [DIAG] ice-agent-recreate.
func (w *WorkerICE) muxLocalAddrStr() string {
	mux, ok := w.config.ICEConfig.UDPMuxSrflx.(*udpmux.UniversalUDPMuxDefault)
	if !ok {
		return "n/a"
	}
	conn := mux.GetSharedConn()
	if conn == nil {
		return "n/a"
	}
	la := conn.LocalAddr()
	if la == nil {
		return "n/a"
	}
	return la.String()
}

// lastLocalSrflxOrNone is the string form of LastLocalSrflx with
// "none" instead of a zero AddrPort. Diagnostic-only convenience.
func (w *WorkerICE) lastLocalSrflxOrNone() string {
	ap := w.LastLocalSrflx()
	if !ap.IsValid() {
		return "none"
	}
	return ap.String()
}

// LastLocalSrflx returns the AddrPort of the most recently discovered
// server-reflexive candidate, or the zero value if none has been
// observed yet. Phase 3.7l Phase-1 helper for Conn.snapshotForDiagnosis
// (Codex 2026-05-30): "tracke pro Peer letzter srflx AddrPort". Safe
// to call concurrently with onICECandidate (atomic.Value).
func (w *WorkerICE) LastLocalSrflx() netip.AddrPort {
	v := w.lastLocalSrflx.Load()
	if v == nil {
		return netip.AddrPort{}
	}
	ap, ok := v.(netip.AddrPort)
	if !ok {
		return netip.AddrPort{}
	}
	return ap
}

// injectPortForwardedCandidate signals an additional candidate using the pre-created port mapping.
func (w *WorkerICE) injectPortForwardedCandidate(srflxCandidate ice.Candidate) {
	pfManager := w.conn.portForwardManager
	if pfManager == nil {
		return
	}

	mapping := pfManager.GetMapping()
	if mapping == nil {
		return
	}

	w.muxAgent.Lock()
	if w.portForwardAttempted {
		w.muxAgent.Unlock()
		return
	}
	w.portForwardAttempted = true
	w.muxAgent.Unlock()

	forwardedCandidate, err := w.createForwardedCandidate(srflxCandidate, mapping)
	if err != nil {
		w.log.Warnf("create forwarded candidate: %v", err)
		return
	}

	w.log.Debugf("injecting port-forwarded candidate: %s (mapping: %d -> %d via %s, priority: %d)",
		forwardedCandidate.String(), mapping.InternalPort, mapping.ExternalPort, mapping.NATType, forwardedCandidate.Priority())

	w.signalAndRemember(forwardedCandidate)
}

// createForwardedCandidate creates a new server reflexive candidate with the forwarded port.
// It uses the NAT gateway's external IP with the forwarded port.
func (w *WorkerICE) createForwardedCandidate(srflxCandidate ice.Candidate, mapping *portforward.Mapping) (ice.Candidate, error) {
	var externalIP string
	if mapping.ExternalIP != nil && !mapping.ExternalIP.IsUnspecified() {
		externalIP = mapping.ExternalIP.String()
	} else {
		// Fallback to STUN-discovered address if NAT didn't provide external IP
		externalIP = srflxCandidate.Address()
	}

	// Per RFC 8445, the related address for srflx is the base (host candidate address).
	// If the original srflx has unspecified related address, use its own address as base.
	relAddr := srflxCandidate.RelatedAddress().Address
	if relAddr == "" || relAddr == "0.0.0.0" || relAddr == "::" {
		relAddr = srflxCandidate.Address()
	}

	// Arbitrary +1000 boost on top of RFC 8445 priority to favor port-forwarded candidates
	// over regular srflx during ICE connectivity checks.
	priority := srflxCandidate.Priority() + 1000

	candidate, err := ice.NewCandidateServerReflexive(&ice.CandidateServerReflexiveConfig{
		Network:   srflxCandidate.NetworkType().String(),
		Address:   externalIP,
		Port:      int(mapping.ExternalPort),
		Component: srflxCandidate.Component(),
		Priority:  priority,
		RelAddr:   relAddr,
		RelPort:   int(mapping.InternalPort),
	})
	if err != nil {
		return nil, fmt.Errorf("create candidate: %w", err)
	}

	for _, e := range srflxCandidate.Extensions() {
		if e.Key == ice.ExtensionKeyCandidateID {
			e.Value = srflxCandidate.ID()
		}
		if err := candidate.AddExtension(e); err != nil {
			return nil, fmt.Errorf("add extension: %w", err)
		}
	}

	return candidate, nil
}

func (w *WorkerICE) onICESelectedCandidatePair(agent *icemaker.ThreadSafeAgent, c1, c2 ice.Candidate) {
	w.log.Debugf("selected candidate pair [local <-> remote] -> [%s <-> %s], peer %s", c1.String(), c2.String(),
		w.config.Key)

	pairStat, ok := agent.GetSelectedCandidatePairStats()
	if !ok {
		w.log.Warnf("failed to get selected candidate pair stats")
		return
	}

	duration := time.Duration(pairStat.CurrentRoundTripTime * float64(time.Second))
	if err := w.statusRecorder.UpdateLatency(w.config.Key, duration); err != nil {
		w.log.Debugf("failed to update latency for peer: %s", err)
		return
	}
}

func (w *WorkerICE) logSuccessfulPaths(agent *icemaker.ThreadSafeAgent) {
	sessionID := w.SessionID()
	stats := agent.GetCandidatePairsStats()
	localCandidates, _ := agent.GetLocalCandidates()
	remoteCandidates, _ := agent.GetRemoteCandidates()

	localMap := make(map[string]ice.Candidate)
	for _, c := range localCandidates {
		localMap[c.ID()] = c
	}
	remoteMap := make(map[string]ice.Candidate)
	for _, c := range remoteCandidates {
		remoteMap[c.ID()] = c
	}

	for _, stat := range stats {
		if stat.State == ice.CandidatePairStateSucceeded {
			local, lok := localMap[stat.LocalCandidateID]
			remote, rok := remoteMap[stat.RemoteCandidateID]
			if !lok || !rok {
				continue
			}
			w.log.Debugf("successful ICE path %s: [%s %s %s:%d] <-> [%s %s %s:%d] rtt=%.3fms",
				sessionID,
				local.NetworkType(), local.Type(), local.Address(), local.Port(),
				remote.NetworkType(), remote.Type(), remote.Address(), remote.Port(),
				stat.CurrentRoundTripTime*1000)
		}
	}
}

func (w *WorkerICE) onConnectionStateChange(agent *icemaker.ThreadSafeAgent, dialerCancel context.CancelFunc) func(ice.ConnectionState) {
	return func(state ice.ConnectionState) {
		w.log.Debugf("ICE ConnectionState has changed to %s", state.String())
		switch state {
		case ice.ConnectionStateConnected:
			w.lastKnownState = ice.ConnectionStateConnected
			w.logSuccessfulPaths(agent)
			// Phase 3 of #5989: reset backoff on ICE success.
			w.conn.onICEConnected()
			return
		case ice.ConnectionStateFailed, ice.ConnectionStateDisconnected, ice.ConnectionStateClosed:
			// ice.ConnectionStateClosed happens when we recreate the agent. For the P2P to TURN switch important to
			// notify the conn.onICEStateDisconnected changes to update the current used priority

			sessionChanged := w.closeAgent(agent, dialerCancel)

			if w.lastKnownState == ice.ConnectionStateConnected {
				w.lastKnownState = ice.ConnectionStateDisconnected
				w.conn.onICEStateDisconnected(sessionChanged)
			}

			// Phase 3 of #5989: record failure in backoff only for true
			// ICE failure (not for the synthetic Closed event that occurs
			// when we recreate the agent on reconnect).
			if state == ice.ConnectionStateFailed {
				w.conn.onICEFailed()
			}
		default:
			return
		}
	}
}

func (w *WorkerICE) turnAgentDial(ctx context.Context, agent *icemaker.ThreadSafeAgent, remoteOfferAnswer *OfferAnswer) (*ice.Conn, error) {
	if isController(w.config) {
		return agent.Dial(ctx, remoteOfferAnswer.IceCredentials.UFrag, remoteOfferAnswer.IceCredentials.Pwd)
	} else {
		return agent.Accept(ctx, remoteOfferAnswer.IceCredentials.UFrag, remoteOfferAnswer.IceCredentials.Pwd)
	}
}

func shouldAddExtraCandidate(candidate ice.Candidate) bool {
	if candidate.Type() != ice.CandidateTypeServerReflexive {
		return false
	}

	if candidate.Port() == candidate.RelatedAddress().Port {
		return false
	}

	// in the older version when we didn't set candidate ID extension the remote peer sent the extra candidates
	// in newer version we generate locally the extra candidate
	if _, ok := candidate.GetExtension(ice.ExtensionKeyCandidateID); !ok {
		return false
	}
	return true
}

func extraSrflxCandidate(candidate ice.Candidate) (*ice.CandidateServerReflexive, error) {
	relatedAdd := candidate.RelatedAddress()
	ec, err := ice.NewCandidateServerReflexive(&ice.CandidateServerReflexiveConfig{
		Network:   candidate.NetworkType().String(),
		Address:   candidate.Address(),
		Port:      relatedAdd.Port,
		Component: candidate.Component(),
		RelAddr:   relatedAdd.Address,
		RelPort:   relatedAdd.Port,
	})
	if err != nil {
		return nil, err
	}

	for _, e := range candidate.Extensions() {
		// overwrite the original candidate ID with the new one to avoid candidate duplication
		if e.Key == ice.ExtensionKeyCandidateID {
			e.Value = candidate.ID()
		}
		if err := ec.AddExtension(e); err != nil {
			return nil, err
		}
	}

	return ec, nil
}

func candidateViaRoutes(candidate ice.Candidate, clientRoutes route.HAMap) bool {
	addr, err := netip.ParseAddr(candidate.Address())
	if err != nil {
		log.Errorf("Failed to parse IP address %s: %v", candidate.Address(), err)
		return false
	}

	var routePrefixes []netip.Prefix
	for _, routes := range clientRoutes {
		if len(routes) > 0 && routes[0] != nil {
			routePrefixes = append(routePrefixes, routes[0].Network)
		}
	}

	for _, prefix := range routePrefixes {
		// default route is handled by route exclusion / ip rules
		if prefix.Bits() == 0 {
			continue
		}

		if prefix.Contains(addr) {
			log.Debugf("Ignoring candidate [%s], its address is part of routed network %s", candidate.String(), prefix)
			return true
		}
	}
	return false
}

func isRelayCandidate(candidate ice.Candidate) bool {
	return candidate.Type() == ice.CandidateTypeRelay
}

func isRelayed(pair *ice.CandidatePair) bool {
	if pair.Local.Type() == ice.CandidateTypeRelay || pair.Remote.Type() == ice.CandidateTypeRelay {
		return true
	}
	return false
}

func selectedPriority(pair *ice.CandidatePair) conntype.ConnPriority {
	if isRelayed(pair) {
		return conntype.ICETurn
	} else {
		return conntype.ICEP2P
	}
}

// signalAndRemember sends an ICE candidate to the remote peer via the
// Signaler AND appends it to sentCandidates so a later
// MaybeReplayCandidates call (for legacy peers running NetBird
// v0.51.2 with the OnRemoteCandidate "agent==nil → drop" race) can
// re-send it.
//
// Track C 2026-05-31 (Codex Plan v3+v4 review). All paths that
// previously called w.signaler.SignalICECandidate directly
// (onICECandidate and injectPortForwardedCandidate) must go through
// this helper so the replay buffer is complete — otherwise
// port-forwarded candidates would be missing from the replay set.
//
// Append happens BEFORE the async send: if the send fails, the
// replay is our second chance to deliver this candidate. Gating
// the append on send-success would lose us replays in the
// fast-answer race.
func (w *WorkerICE) signalAndRemember(candidate ice.Candidate) {
	w.sentCandidatesMu.Lock()
	w.sentCandidates = append(w.sentCandidates, candidate)
	w.sentCandidatesMu.Unlock()

	go func() {
		if err := w.signaler.SignalICECandidate(candidate, w.config.Key); err != nil {
			w.log.Errorf("failed signaling candidate to the remote peer %s %s", w.config.Key, err)
		}
	}()
}

// MaybeReplayCandidates re-sends previously gathered ICE candidates
// to the remote peer if the peer runs a NetBird version known to
// drop early-arriving candidates (the v0.51.2 ICE-Init-Race in
// worker_ice.go OnRemoteCandidate, see
// docs/test-reports/2026-05-31-elmira-ice-init-race-android-vs-
// legacy/ for the full investigation).
//
// CRITICAL ORDERING (Codex Plan v3+v4 review):
//
//  1. Sleep FIRST. Both the snapshot AND the replayedThisSession
//     gate must be read AFTER the delay, because the
//     OnRemoteOffer-receiver-role path triggers a parallel
//     Handshaker.Listen → WorkerICE.OnNewOffer → reCreateAgent
//     that runs during the sleep. reCreateAgent clears BOTH
//     sentCandidates AND replayedThisSession. If we read either
//     pre-sleep, we'd see the previous session's state and either
//     get an empty replay (snapshot) or a false "already replayed"
//     (gate) and bail incorrectly.
//
//  2. Gate-AFTER-snapshot-validity. We only consume the
//     "replayedThisSession=true" gate WHEN we actually have
//     candidates to send. If the snapshot is empty (gather still
//     slow, race on first OnRemoteOffer), we return with the gate
//     untouched, so a later trigger in the same session can try
//     again (Codex Plan v4: no-consume-on-empty).
//
//  3. The send goroutines fire AFTER we release the mutex so the
//     caller's path is not blocked.
//
// The replay itself is idempotent on the receiver side (pion's
// agent.AddRemoteCandidate dropping duplicates), so it is safe
// even if a modern peer somehow ends up gated as legacy.
func (w *WorkerICE) MaybeReplayCandidates(remoteVersion string) {
	if !legacyCandidateReplayEnabled() {
		return
	}
	if !isLegacyICECandidateRecv(remoteVersion) {
		return
	}

	// Step 1: sleep FIRST, give reCreateAgent + gather() time to run
	// if this trigger came via the OnRemoteOffer-receiver-role path.
	time.Sleep(legacyCandidateReplayDelay())

	// Step 2: gate-check AND snapshot under same lock, AFTER the sleep.
	w.sentCandidatesMu.Lock()

	if w.replayedThisSession {
		w.replaySkippedAttempts++
		skipped := w.replaySkippedAttempts
		w.sentCandidatesMu.Unlock()
		w.log.Debugf("[Track-C] replay skipped (already replayed this session, total skipped=%d) for peer %s",
			skipped, w.config.Key)
		return
	}

	snapshot := append([]ice.Candidate(nil), w.sentCandidates...)
	if len(snapshot) == 0 {
		// Empty snapshot — gather() has not produced candidates yet
		// even after the sleep. Do NOT consume the
		// replayedThisSession gate, so a later trigger in this same
		// session still has a chance to replay.
		w.sentCandidatesMu.Unlock()
		w.log.Debugf("[Track-C] no candidates to replay for peer %s (remote version %s) — gather race?",
			w.config.Key, remoteVersion)
		return
	}

	// Only set the gate when we actually have something to send.
	w.replayedThisSession = true
	w.sentCandidatesMu.Unlock()

	w.log.Debugf("[Track-C] replaying %d gathered candidates to legacy peer %s (remote version %s)",
		len(snapshot), w.config.Key, remoteVersion)

	for _, c := range snapshot {
		c := c
		go func() {
			if err := w.signaler.SignalICECandidate(c, w.config.Key); err != nil {
				w.log.Debugf("[Track-C] failed replaying candidate to %s: %s", w.config.Key, err)
			}
		}()
	}
}
