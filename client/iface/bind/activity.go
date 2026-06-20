package bind

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/monotime"
)

const (
	saveFrequency = int64(5 * time.Second)
)

const (
	// V18.16 (2026-06-20): inbound-burst gate. recordInboundBurst
	// fires the onActivity callback only on the K-th packet within
	// a sliding W-second window. Tuned to distinguish real user
	// VNC/SSH/HTTP traffic from legacy peer keep-alives + single
	// Android system probes that survive size+type filters.
	// Window matches V18.13's outbound burst gate so both directions
	// share the same UX tuning. Threshold=3 = at least 2 packets of
	// wire-time activity beyond the first one (which could be an
	// artifact). Production rationale documented in V18.16 commit.
	v18_16InboundBurstWindow   = 30 * time.Second
	v18_16InboundBurstMinBurst = int32(3)
)

type PeerRecord struct {
	PublicKey    string
	Address      netip.AddrPort
	LastActivity atomic.Int64 // UnixNano timestamp

	// V18.16 (2026-06-20): inbound-burst counters. Drive the
	// recordInboundBurst edge. Per-peer to avoid global contention.
	inboundBurstCount       atomic.Int32
	inboundBurstWindowStart atomic.Int64 // monotime ns of window start
}

type ActivityRecorder struct {
	mu         sync.RWMutex
	peers      map[string]*PeerRecord         // publicKey to PeerRecord map
	addrToPeer map[netip.AddrPort]*PeerRecord // address to PeerRecord map
	// onActivity, if set, is invoked once per saveFrequency-window per
	// peer when transport activity is observed. Used by the engine's
	// connMgr to fast-path ICE re-attach for peers that fell back to
	// relay-only on iceTimeout (Codex review 2026-05-05). Rate-limited
	// piggybacks the existing CAS to avoid a hot-path allocation.
	onActivity func(pubKey string)
}

func NewActivityRecorder() *ActivityRecorder {
	return &ActivityRecorder{
		peers:      make(map[string]*PeerRecord),
		addrToPeer: make(map[netip.AddrPort]*PeerRecord),
	}
}

// SetOnActivity registers a callback invoked at most once per
// saveFrequency (5s) per peer when transport activity is recorded.
// Pass nil to clear. Safe to call before the recorder starts seeing
// traffic.
func (r *ActivityRecorder) SetOnActivity(cb func(pubKey string)) {
	r.mu.Lock()
	r.onActivity = cb
	r.mu.Unlock()
}

// GetLastActivities returns a snapshot of peer last activity
func (r *ActivityRecorder) GetLastActivities() map[string]monotime.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()

	activities := make(map[string]monotime.Time, len(r.peers))
	for key, record := range r.peers {
		monoTime := record.LastActivity.Load()
		activities[key] = monotime.Time(monoTime)
	}
	return activities
}

// UpsertAddress adds or updates the address for a publicKey
func (r *ActivityRecorder) UpsertAddress(publicKey string, address netip.AddrPort) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var record *PeerRecord
	record, exists := r.peers[publicKey]
	if exists {
		delete(r.addrToPeer, record.Address)
		record.Address = address
	} else {
		record = &PeerRecord{
			PublicKey: publicKey,
			Address:   address,
		}
		record.LastActivity.Store(int64(monotime.Now()))
		r.peers[publicKey] = record
	}

	r.addrToPeer[address] = record
}

func (r *ActivityRecorder) Remove(publicKey string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if record, exists := r.peers[publicKey]; exists {
		delete(r.addrToPeer, record.Address)
		delete(r.peers, publicKey)
	}
}

// record updates LastActivity for the given address using atomic store.
//
// V18.4 (2026-06-09): no longer fires the onActivity callback. Receive-
// path packets (which were the original trigger source) include legacy
// peer keep-alives that should NOT wake intentionally-detached peers.
// Callers that want the wake-up edge must use recordOutbound instead;
// the bidir Last-Activity timestamp is still maintained here so
// LastActivities()-based heuristics (Phase-3.7j Fix-C local activity
// gate, etc.) keep working unchanged.
func (r *ActivityRecorder) record(address netip.AddrPort) {
	r.mu.RLock()
	record, ok := r.addrToPeer[address]
	r.mu.RUnlock()
	if !ok {
		log.Warnf("could not find record for address %s", address)
		return
	}

	now := int64(monotime.Now())
	last := record.LastActivity.Load()
	if now-last < saveFrequency {
		return
	}
	record.LastActivity.CompareAndSwap(last, now)
}

// recordOutbound updates LastActivity AND fires the onActivity callback.
// Called from the WG send path so the relay-activity → P2P upgrade only
// triggers on locally-initiated outbound traffic. Without this split,
// legacy (pre-0.68) peers' keep-alives arriving via relay would
// repeatedly wake an intentionally-detached peer (production W11 + S26
// 2026-06-09: 4-min detach / re-attach cycle for every paired legacy
// peer, even with zero user traffic).
func (r *ActivityRecorder) recordOutbound(address netip.AddrPort) {
	r.mu.RLock()
	record, ok := r.addrToPeer[address]
	cb := r.onActivity
	r.mu.RUnlock()
	if !ok {
		log.Warnf("could not find record for address %s", address)
		return
	}

	now := int64(monotime.Now())
	last := record.LastActivity.Load()
	if now-last < saveFrequency {
		return
	}

	if record.LastActivity.CompareAndSwap(last, now) && cb != nil {
		// Fire only on the actual save edge (CAS success). Prevents
		// duplicate events when many goroutines race on the same packet
		// burst. Callback runs synchronously on the WG send goroutine —
		// handler MUST be cheap or self-defer to its own goroutine.
		cb(record.PublicKey)
	}
}

// recordInboundBurst is V18.16 Fix 2: the inbound mirror of
// recordOutbound. Fires onActivity ONLY on the K-th transport packet
// (>32 B) arriving from this peer within a sliding W-second window.
//
// Why not just reuse recordOutbound's semantics on inbound?
// recordOutbound fires on every saveFrequency edge (1/peer/5s). Legacy
// pre-0.68 peers send WG keep-alives over Relay every ~25 s that pass
// the size+type filter; without a burst gate, EVERY keep-alive would
// wake an intentionally-detached peer. The original V18.4 split moved
// the recv path away from firing callbacks specifically to avoid this.
//
// recordInboundBurst re-introduces an inbound wake edge but gated:
//   - 3 packets within 30 s of wall clock from the SAME peer
//   - resets counters on successful wake (next detach cycle starts fresh)
//
// A legacy keep-alive cadence of ~25 s sees count=1, window expires
// before the 2nd keep-alive arrives, count resets to 1 forever — never
// reaches 3. A sustained user inbound flow (VNC frame updates, SSH
// stream, HTTP response) easily exceeds the threshold.
//
// Caller is the relay-recv path in ice_bind.go:receiveRelayed. Only
// transport packets (type 4 + size>32 B) are eligible — the caller
// applies that filter exactly as the send path does.
func (r *ActivityRecorder) recordInboundBurst(address netip.AddrPort) {
	r.mu.RLock()
	record, ok := r.addrToPeer[address]
	cb := r.onActivity
	r.mu.RUnlock()
	if !ok {
		// Unknown address — peer not registered, no-op. Do NOT log:
		// receive-path log spam was a real prod issue in V18.4.
		return
	}

	now := int64(monotime.Now())
	windowStart := record.inboundBurstWindowStart.Load()
	if windowStart == 0 || monotime.Since(monotime.Time(windowStart)) > v18_16InboundBurstWindow {
		// Window expired (or never started) — start fresh.
		record.inboundBurstWindowStart.Store(now)
		record.inboundBurstCount.Store(1)
		return
	}
	count := record.inboundBurstCount.Add(1)
	if count < v18_16InboundBurstMinBurst {
		return
	}
	// Threshold reached. Reset for the next detach cycle and fire.
	record.inboundBurstCount.Store(0)
	record.inboundBurstWindowStart.Store(0)
	if cb != nil {
		cb(record.PublicKey)
	}
}
