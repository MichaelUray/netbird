package peer

import (
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// SentinelP2pRetryDisabled is the wire-format value (== ^uint32(0))
// that means "user-explicit disable" for the ICE-failure backoff cap.
// Translated to time.Duration(0) at the daemon boundary, which is the
// existing "backoff disabled" semantic checked by markFailure /
// markUserInitiatedRetry.
const SentinelP2pRetryDisabled uint32 = ^uint32(0)

// ResolveP2pRetryCap maps a wire-format uint32 to the time.Duration
// that newIceBackoff / SetMaxBackoff expect. Single source of truth
// for the sentinel + zero-means-default semantics shared by
// (*Conn).initIceBackoffFromConfig and
// ConnMgr.propagateP2pRetryMaxToConns. Phase-3.7i v0.5 fix.
func ResolveP2pRetryCap(seconds uint32) time.Duration {
	switch seconds {
	case SentinelP2pRetryDisabled:
		return 0
	case 0:
		return DefaultP2PRetryMax
	default:
		return time.Duration(seconds) * time.Second
	}
}

const (
	// DefaultP2PRetryMax is the built-in fallback when the management
	// server has not pushed a p2p_retry_max_seconds value (Proto wire
	// value 0 = "not set"). Phase 3 of #5989.
	DefaultP2PRetryMax = 15 * time.Minute

	iceBackoffInitialInterval     = 1 * time.Minute
	iceBackoffMultiplier          = 2.0
	iceBackoffRandomizationFactor = 0.1

	// networkChangeGracePeriod is the window after Reset() (signal/relay
	// reconnect, network-change event) during which markFailure caps the
	// suspend delay at networkChangeRetryDelay. Phase 3.7f of #5989.
	//
	// Rationale: the first ICE pair-check after a network change often
	// fails on stale NAT mappings, even when subsequent attempts succeed.
	// Falling back to the normal 1-minute initial backoff after that
	// single failure leaves the peer on relay for far longer than the
	// underlying connectivity actually warrants. A short fixed delay
	// inside the grace window lets follow-up attempts run while the new
	// LTE/Wi-Fi mapping is still fresh; outside the window the normal
	// exponential schedule applies as before.
	//
	// Phase 3.7h widened the window from 30 s to 60 s and reduced the
	// retry delay from 5 s to 2 s after observing real-world LTE-bounce
	// behaviour: cold NAT mappings often need 3-4 ICE attempts to prime,
	// and the previous 30 s window only fit ~2 attempts (each pair-check
	// is ~12-15 s) before the schedule jumped to a 1-minute exponential
	// suspend. The wider window plus shorter delay typically fits ~4-5
	// attempts and recovers within ~50 s for peers behind a single NAT
	// instead of 2-3 minutes.
	networkChangeGracePeriod = 60 * time.Second
	networkChangeRetryDelay  = 2 * time.Second
)

// iceBackoffState tracks per-peer ICE-failure backoff in p2p-dynamic
// mode. Phase 3 of #5989.
type iceBackoffState struct {
	mu          sync.Mutex
	bo          *backoff.ExponentialBackOff
	failures    int
	nextRetry   time.Time
	suspended   bool
	maxBackoff  time.Duration
	lastResetAt time.Time

	// V16.1 (2026-06-04): post-success-drop has its own counter, kept
	// separate from `failures` so the milder schedule below does not
	// feed the long-term exponential `bo` state. Reset by markSuccess
	// and Reset, like `failures`.
	postSuccessFailures int
}

// BackoffSnapshot is a read-only view used by the status output.
type BackoffSnapshot struct {
	Failures  int
	NextRetry time.Time
	Suspended bool
}

func newIceBackoff(maxBackoff time.Duration) *iceBackoffState {
	return &iceBackoffState{
		bo:         buildBackoff(maxBackoff),
		maxBackoff: maxBackoff,
	}
}

func buildBackoff(maxBackoff time.Duration) *backoff.ExponentialBackOff {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = iceBackoffInitialInterval
	bo.Multiplier = iceBackoffMultiplier
	bo.RandomizationFactor = iceBackoffRandomizationFactor
	bo.MaxInterval = maxBackoff
	bo.MaxElapsedTime = 0
	bo.Reset()
	return bo
}

func (s *iceBackoffState) IsSuspended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.suspended {
		return false
	}
	if time.Now().After(s.nextRetry) {
		return false
	}
	return true
}

// markFailure increments the failure counter and computes the next retry
// time. Returns the delay so callers can log it. If maxBackoff is 0
// (= disabled), returns 0 and does not modify state.
//
// Phase 3.7f of #5989: while we are still inside networkChangeGracePeriod
// after the most recent Reset() (typically a srReconnect / network-change
// event), the suspend delay is capped at networkChangeRetryDelay and the
// long-term exponential schedule is NOT advanced. Once the grace window
// elapses, normal exponential backoff applies. This lets the second ICE
// pair-check run while a fresh LTE/Wi-Fi NAT mapping is still warm,
// without flooding signaling for chronically broken peers.
func (s *iceBackoffState) markFailure() time.Duration {
	return s.markFailureWithSeverity(false)
}

// markFailurePostSuccessDrop is the milder variant for failures that occur
// AFTER an ICE session was already established. Halifax-CGNAT and similar
// stateful NAT setups routinely trip post-success-drops via NAT-stale
// without the underlying P2P path being structurally broken — w11-test1
// 2026-06-04 had failure #1 → 2 s suspend → next OFFER → success in 0.4 s.
// Without this milder path, those transient drops feed the same exponential
// curve as first-attempt-cannot-pair, so a peer that fluctuates ends up
// in 2 min+ suspends after only 3-4 cycles and looks "permanently broken"
// to the user (S26 case 2026-06-04).
//
// V16.1 schedule (ramped, independent of `bo`):
//
//	post-fail #1: 2 s
//	post-fail #2: 5 s
//	post-fail #3: 10 s
//	post-fail #4+: 30 s (postSuccessDropMaxSuspend)
//
// The first-attempt / re-attach paths keep the original exponential
// behaviour because they signal a real "cannot establish" condition.
// `s.bo` is NOT touched here — so a peer that flutters via
// post-success-drops never poisons the schedule for a later genuine
// first-attempt failure (V16 bug found by code-review 2026-06-04).
func (s *iceBackoffState) markFailurePostSuccessDrop() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxBackoff == 0 {
		return 0
	}
	s.failures++
	s.postSuccessFailures++

	// Network-change grace still wins (same logic as markFailure).
	var delay time.Duration
	if !s.lastResetAt.IsZero() && time.Since(s.lastResetAt) < networkChangeGracePeriod {
		delay = networkChangeRetryDelay
	} else {
		delay = postSuccessDropDelayFor(s.postSuccessFailures)
	}

	s.nextRetry = time.Now().Add(delay)
	s.suspended = true
	return delay
}

const postSuccessDropMaxSuspend = 30 * time.Second

// postSuccessDropDelayFor returns the ramped post-success-drop delay for
// the n-th consecutive post-success failure (1-based). Public package-local
// so unit tests can pin the schedule.
func postSuccessDropDelayFor(n int) time.Duration {
	switch n {
	case 0, 1:
		return 2 * time.Second
	case 2:
		return 5 * time.Second
	case 3:
		return 10 * time.Second
	default:
		return postSuccessDropMaxSuspend
	}
}

func (s *iceBackoffState) markFailureWithSeverity(postSuccessDrop bool) time.Duration {
	if postSuccessDrop {
		return s.markFailurePostSuccessDrop()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxBackoff == 0 {
		return 0
	}
	s.failures++

	var delay time.Duration
	if !s.lastResetAt.IsZero() && time.Since(s.lastResetAt) < networkChangeGracePeriod {
		delay = networkChangeRetryDelay
	} else {
		delay = s.bo.NextBackOff()
	}

	s.nextRetry = time.Now().Add(delay)
	s.suspended = true
	return delay
}

func (s *iceBackoffState) Snapshot() BackoffSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return BackoffSnapshot{
		Failures:  s.failures,
		NextRetry: s.nextRetry,
		Suspended: s.suspended && time.Now().Before(s.nextRetry),
	}
}

// markSuccess clears the failure counter and resets the internal backoff
// to its initial interval. Called when pion reports ConnectionStateConnected.
//
// Also stamps lastResetAt: a successful ICE connect is semantically the
// strongest "the path works" signal we have, so the post-network-change
// grace period (markFailure) and the activity-override rate limit
// (AllowActivityOverride) both honour it as a fresh reset point. Codex
// review 2026-05-05 caught the previous miss: without this stamp,
// Reset() and markSuccess() were inconsistent and AllowActivityOverride
// would have allowed an override immediately after a fresh successful
// connect, defeating its rate-limit intent.
func (s *iceBackoffState) markSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = 0
	s.postSuccessFailures = 0
	s.suspended = false
	s.bo.Reset()
	s.lastResetAt = time.Now()
}

// Reset is the hard reset triggered by interface-change or mode-push.
// In addition to clearing the failure counter and exponential schedule,
// it stamps lastResetAt so that markFailure can apply the
// post-network-change grace period (Phase 3.7f).
func (s *iceBackoffState) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = 0
	s.postSuccessFailures = 0
	s.suspended = false
	s.bo.Reset()
	s.lastResetAt = time.Now()
}

// activityOverrideMinInterval bounds how often relay-state activity
// can override an active ICE-failure backoff. The backoff exists to
// protect signal-server load against truly broken paths; user activity
// however is the strongest "I want this peer back" signal we have, so
// we allow ONE override per this window per peer. 5 min lines up with
// the relayTimeout default -- after one override window the conn would
// have cycled to Idle anyway, freeing the backoff via the C->A wake
// path which already does ResetIceBackoff.
const activityOverrideMinInterval = 5 * time.Minute

// AllowActivityOverride returns true if a relay-state activity event
// is permitted to bypass an active backoff suspension. The caller is
// expected to call Reset() afterwards if true is returned. Guards
// against signal-storm by enforcing activityOverrideMinInterval since
// the last (success-, network-change-, or override-driven) reset.
//
// Phase 3.7i (#5989), Codex review 2026-05-05 point 5: "Optional
// maximal ein sehr bewusstes 'user activity retry override' mit harter
// Rate-Limitierung". This is that override, gated to once per 5min.
func (s *iceBackoffState) AllowActivityOverride() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.suspended {
		return false // not in backoff, nothing to override
	}
	if time.Since(s.lastResetAt) < activityOverrideMinInterval {
		return false // too soon since last reset, respect rate limit
	}
	return true
}

// markUserInitiatedRetry temporarily lifts the suspended gate so a single
// AttachICEUserInitiated call can drive a fresh ICE attempt while the
// exponential backoff is otherwise in force.
//
// Semantics (Phase 3.7i):
//   - Returns false if the backoff is disabled (maxBackoff==0) or currently
//     not suspended — callers should fall through to the normal AttachICE
//     path in that case, no bypass was needed.
//   - Returns true and clears s.suspended when a bypass actually happens.
//     Failures counter and the underlying exponential schedule are NOT
//     reset: the next markFailure picks up where the previous one left off.
//     This is the key difference vs. Reset()/markSuccess(): we want a
//     single targeted retry without losing the long-term backoff state
//     for chronically broken peers.
//
// Merge note (build/production-v2): coexists with AllowActivityOverride
// above. AllowActivityOverride is the relay-state activity path (5min
// gate, caller expected to call Reset() to fully clear). markUserInitiated
// Retry is the user-traffic / AttachICEUserInitiated path (per-call
// cooldown on conn.go, schedule preserved).
func (s *iceBackoffState) markUserInitiatedRetry() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxBackoff == 0 {
		return false
	}
	if !s.suspended {
		return false
	}
	if time.Now().After(s.nextRetry) {
		// Naturally expired; not a true bypass but caller can proceed.
		s.suspended = false
		return false
	}
	s.suspended = false
	return true
}

// SetMaxBackoff updates the cap. Called from ConnMgr.UpdatedRemotePeerConfig
// when the server pushes a new value. Rebuilds the internal backoff with
// the new schedule but preserves the failure counter.
func (s *iceBackoffState) SetMaxBackoff(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d == s.maxBackoff {
		return
	}
	s.maxBackoff = d
	s.bo = buildBackoff(d)
}
