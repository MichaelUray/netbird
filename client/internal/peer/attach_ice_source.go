package peer

// Phase 3.7l (Fix-D D1.1) — source-label for AttachICE call paths.
//
// Live-D1 data on S26/S21 showed that a stuck-state peer can produce 6+
// AttachICE-blocked-backoff-suspended DIAG entries within 200 ms,
// without any way to distinguish which recovery path drove the burst:
//
//   guard tick                  -> conn.AttachICE()
//   ConnMgr.ActivatePeer        -> conn.AttachICE()  (signal-driven)
//   lazyconn manager wake       -> conn.AttachICE()
//   AttachICEOnRelayActivity    -> conn.AttachICE()
//   AttachICEUserInitiated      -> conn.AttachICE()
//
// Each path has a different fix surface. Without a source label the
// log only says "blocked" — not "blocked because the signal-driven path
// fired but backoff is still warm". Codex' D1.1 recommendation (Plan
// only, 2026-05-29): label the source, then dedupe DIAG bursts per
// reason on a short (~1 s) window so the offline timeline stays
// readable.
//
// The source enum is a plain string so log lines remain grep-friendly:
//
//   [DIAG] reason=AttachICE-blocked-backoff-suspended source=signal
//
// Unknown sources fall back to AttachICESourceUnknown — used by legacy
// tests that call AttachICE() directly without picking a label, and by
// any future call site the author forgot to annotate.
type AttachICESource string

const (
	// AttachICESourceUnknown is the safe default for any caller that
	// has not been annotated. Should not appear on production paths.
	AttachICESourceUnknown AttachICESource = "unknown"

	// AttachICESourceSignal labels the engine.go / conn_mgr.go path
	// where a remote OFFER arrives via signal and ConnMgr.ActivatePeer
	// is invoked. Most common cause of guard-burst storms.
	AttachICESourceSignal AttachICESource = "signal"

	// AttachICESourceLazyActivity labels the local-write-on-fake-IP
	// recovery path in lazyconn/manager.go (peer transitioned out of
	// idle because a local app sent traffic to its NetBird IP).
	AttachICESourceLazyActivity AttachICESource = "lazy-activity"

	// AttachICESourceRelayActivity labels the relay-tunnel-payload
	// recovery path (D2a addition — receive- AND send-side after the
	// fix). Fires when actual peer traffic flows over the relay tunnel
	// while ICE is detached.
	AttachICESourceRelayActivity AttachICESource = "relay-activity"

	// AttachICESourceGuard labels the periodic Guard tick — every
	// ~30 s the guard re-evaluates whether an offer is due.
	AttachICESourceGuard AttachICESource = "guard"

	// AttachICESourceUserInitiated labels AttachICEUserInitiated, the
	// lazyconn manager's bypass path used when the user generated traffic
	// and the backoff would otherwise block the retry.
	AttachICESourceUserInitiated AttachICESource = "user-initiated"

	// AttachICESourceRemoteOffer labels the new D3b path that lets a
	// remote-side OFFER serve as a one-shot user-activity bypass when
	// the local backoff is suspended and there is no other transport.
	AttachICESourceRemoteOffer AttachICESource = "remote-offer"
)

// String returns the enum value as a plain string suitable for log lines.
// Implements fmt.Stringer so source can be passed directly to %s formatters.
func (s AttachICESource) String() string {
	if s == "" {
		return string(AttachICESourceUnknown)
	}
	return string(s)
}
