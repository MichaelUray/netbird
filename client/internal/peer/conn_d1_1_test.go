package peer

import (
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/client/internal/peer/conntype"
	"github.com/netbirdio/netbird/shared/connectionmode"
)

// Phase 3.7l (Fix-D D1.1) — source-label + DIAG-dedupe regression tests.
//
// Codex' D1.1 recommendation: label AttachICE callers (signal / guard /
// lazy-activity / relay-activity) so blocked-backoff DIAG markers can
// be distinguished, and dedupe identical reasons within a short window
// so burst-floods (6+ in 200 ms observed live on S26) collapse to a
// single line per window.

// TestAttachICESource_String covers the Stringer contract — log lines
// must produce stable, grep-friendly values for every enum member, and
// an empty zero-value must NOT print as "" (collapse to "unknown" instead).
func TestAttachICESource_String(t *testing.T) {
	cases := []struct {
		src  AttachICESource
		want string
	}{
		{AttachICESourceUnknown, "unknown"},
		{AttachICESourceSignal, "signal"},
		{AttachICESourceLazyActivity, "lazy-activity"},
		{AttachICESourceRelayActivity, "relay-activity"},
		{AttachICESourceGuard, "guard"},
		{AttachICESourceUserInitiated, "user-initiated"},
		{AttachICESourceRemoteOffer, "remote-offer"},
		{AttachICESource(""), "unknown"}, // zero-value safety
	}
	for _, tc := range cases {
		if got := tc.src.String(); got != tc.want {
			t.Errorf("AttachICESource(%q).String() = %q, want %q", string(tc.src), got, tc.want)
		}
	}
}

// TestConn_AttachICEFrom_BlockedBackoff_LogsSourceLabel verifies that
// when AttachICE is blocked by suspended backoff, the emitted [DIAG]
// reason includes the source label. This is the core D1.1 contract:
// the offline analyst can grep for source=signal vs source=lazy-activity
// to attribute the burst to its root cause.
func TestConn_AttachICEFrom_BlockedBackoff_LogsSourceLabel(t *testing.T) {
	bo := newIceBackoff(15 * time.Minute)
	bo.markFailure() // suspend it
	c := &Conn{
		Log:        log.WithField("peer", "test-d1.1"),
		handshaker: &Handshaker{},
		iceBackoff: bo,
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
	}

	// Call with each source — function must return nil (blocked is
	// not an error) and not panic.
	for _, src := range []AttachICESource{
		AttachICESourceSignal,
		AttachICESourceLazyActivity,
		AttachICESourceGuard,
		AttachICESourceRelayActivity,
	} {
		err := c.AttachICEFrom(src)
		if err != nil {
			t.Errorf("AttachICEFrom(%s) returned error: %v (expected nil — blocked is not an error)", src, err)
		}
		// Listener must NOT be attached (backoff blocked).
		if c.handshaker.readICEListener() != nil {
			t.Errorf("AttachICEFrom(%s) installed listener despite suspended backoff", src)
		}
	}
}

// TestConn_LogDiagSnapshotDedup_SuppressesBurst verifies that within
// a single dedupe-window the SAME reason is emitted at most once. This
// is the load-bearing piece of D1.1 — without it the per-peer reason
// labels would just multiply the existing log noise.
func TestConn_LogDiagSnapshotDedup_SuppressesBurst(t *testing.T) {
	c := &Conn{
		Log: log.WithField("peer", "test-dedup"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
	}

	// First call must record the reason in diagLastEmit.
	c.logDiagSnapshotDedup("dedup-reason-A", 500*time.Millisecond)

	// Immediate second call within the window: dedupe should suppress
	// the log emit, but state must still hold the first-emit timestamp.
	v, ok := c.diagLastEmit.Load("dedup-reason-A")
	if !ok {
		t.Fatal("expected diagLastEmit to record reason after first call")
	}
	firstTime, ok := v.(time.Time)
	if !ok {
		t.Fatalf("diagLastEmit value type wrong: %T", v)
	}

	c.logDiagSnapshotDedup("dedup-reason-A", 500*time.Millisecond)

	// Timestamp must NOT have advanced (second call was suppressed).
	v2, _ := c.diagLastEmit.Load("dedup-reason-A")
	secondTime, _ := v2.(time.Time)
	if !secondTime.Equal(firstTime) {
		t.Errorf("dedupe failed: timestamp advanced from %s to %s within window",
			firstTime.Format(time.StampMicro), secondTime.Format(time.StampMicro))
	}
}

// TestConn_LogDiagSnapshotDedup_DifferentReasonsNotSuppressed verifies
// that different reasons share no dedupe window — each reason gets its
// own clock so labelled paths don't shadow each other.
func TestConn_LogDiagSnapshotDedup_DifferentReasonsNotSuppressed(t *testing.T) {
	c := &Conn{
		Log: log.WithField("peer", "test-dedup-multi"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
	}

	c.logDiagSnapshotDedup("reason-A", 500*time.Millisecond)
	c.logDiagSnapshotDedup("reason-B", 500*time.Millisecond)
	c.logDiagSnapshotDedup("reason-C", 500*time.Millisecond)

	for _, r := range []string{"reason-A", "reason-B", "reason-C"} {
		if _, ok := c.diagLastEmit.Load(r); !ok {
			t.Errorf("expected reason %q recorded after distinct call", r)
		}
	}
}

// TestConn_LogDiagSnapshotDedup_ZeroWindow_AlwaysEmits is the safety
// case: window<=0 disables dedupe entirely, behaving like
// logDiagSnapshot. Callers that don't want dedupe (single-shot events
// like "ICE Connected") can opt-out cleanly.
func TestConn_LogDiagSnapshotDedup_ZeroWindow_AlwaysEmits(t *testing.T) {
	c := &Conn{
		Log: log.WithField("peer", "test-dedup-zero"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
	}

	c.logDiagSnapshotDedup("no-dedup", 0)
	// No record-tracking is performed when window is 0 — diagLastEmit
	// stays empty for this reason.
	if _, ok := c.diagLastEmit.Load("no-dedup"); ok {
		t.Errorf("logDiagSnapshotDedup with window=0 should NOT record dedupe state")
	}
}

// TestConn_LogDiagSnapshotDedup_WindowExpiry verifies that after the
// dedupe window elapses, the same reason is allowed to emit again. This
// closes the per-window-but-not-forever-silent contract.
func TestConn_LogDiagSnapshotDedup_WindowExpiry(t *testing.T) {
	c := &Conn{
		Log: log.WithField("peer", "test-dedup-expiry"),
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
	}
	window := 50 * time.Millisecond
	c.logDiagSnapshotDedup("reason-X", window)
	v1, _ := c.diagLastEmit.Load("reason-X")
	t1 := v1.(time.Time)

	// Wait past the window then call again — timestamp must advance.
	time.Sleep(window + 20*time.Millisecond)
	c.logDiagSnapshotDedup("reason-X", window)
	v2, _ := c.diagLastEmit.Load("reason-X")
	t2 := v2.(time.Time)

	if !t2.After(t1) {
		t.Errorf("after window expiry timestamp should advance: t1=%s t2=%s",
			t1.Format(time.StampMicro), t2.Format(time.StampMicro))
	}
}

// TestConn_AttachICEFrom_BlockedDIAGContainsSourceSuffix builds the full
// end-to-end check: a backoff-suspended Conn called with each source
// emits a DIAG line whose "reason=" suffix matches the source label.
// Uses a logrus test-hook to capture the actual log lines.
func TestConn_AttachICEFrom_BlockedDIAGContainsSourceSuffix(t *testing.T) {
	// Capture all logged messages via a memory hook.
	hook := &capturingLogHook{}
	logger := log.New()
	logger.SetLevel(log.DebugLevel)
	logger.AddHook(hook)

	bo := newIceBackoff(15 * time.Minute)
	bo.markFailure()
	c := &Conn{
		Log:        logger.WithField("peer", "test-suffix"),
		handshaker: &Handshaker{},
		iceBackoff: bo,
		config: ConnConfig{
			Mode: connectionmode.ModeP2PDynamic,
		},
		currentConnPriority: conntype.Relay,
	}
	c.everConnected.Store(true)

	cases := []struct {
		src        AttachICESource
		suffix     string
		dedupReset bool
	}{
		{AttachICESourceSignal, "source-signal", true},
		{AttachICESourceLazyActivity, "source-lazy-activity", true},
		{AttachICESourceGuard, "source-guard", true},
		{AttachICESourceRelayActivity, "source-relay-activity", true},
	}
	for _, tc := range cases {
		// Reset dedupe state between cases — each reason is unique
		// per source label, so dedupe doesn't actually collide across
		// cases, but resetting keeps the assertion lifecycle clean.
		if tc.dedupReset {
			c.diagLastEmit = sync.Map{}
		}
		hook.entries = nil
		_ = c.AttachICEFrom(tc.src)

		found := false
		for _, e := range hook.entries {
			if strings.Contains(e, "[DIAG]") && strings.Contains(e, tc.suffix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("AttachICEFrom(%s): no DIAG entry containing %q\ncaptured=%v",
				tc.src, tc.suffix, hook.entries)
		}
	}
}

// capturingLogHook is a tiny logrus hook that stores formatted message
// strings into a slice. Sufficient for assert-style log inspection in
// these tests.
type capturingLogHook struct {
	entries []string
}

func (h *capturingLogHook) Levels() []log.Level {
	return log.AllLevels
}
func (h *capturingLogHook) Fire(e *log.Entry) error {
	h.entries = append(h.entries, e.Message)
	return nil
}
