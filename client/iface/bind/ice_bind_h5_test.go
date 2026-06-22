package bind

import (
	"net/netip"
	"testing"
)

// H5 (2026-06-22) — kind-aware nil-check for WakeIntentArmer in
// SetEndpoint.
//
// Background: WakeIntentArmer is an interface. A typed-nil pointer
// wrapped in an interface (e.g. `var c *peer.Conn; bind.SetEndpoint(ip,
// conn, c)`) is NOT == nil — the interface header carries the type
// descriptor. With a plain `armer != nil` check, that typed-nil slips
// past SetEndpoint, lands in endpointsWakeArmer, and the Send path
// later does `wakeArmer.ArmLocalWakeIntent(...)` on a nil receiver.
// The production peer.Conn implementation reads atomic fields off the
// receiver — so this panics.
//
// Fix: isNilArmer covers all reflect kinds that can hold a typed-nil
// (Chan, Func, Interface, Map, Ptr, Slice). Codex review v2 amendment:
// every nil-able kind must be covered by REAL test cases (not just a
// comment placeholder). Each kind below is a defined type with a
// method receiver implementing WakeIntentArmer so the typed-nil really
// flows through the interface.

// nilPtrArmer: kind=Ptr. Method on *nilPtrArmer; nil pointer wrapped
// in interface is a typed-nil. We panic in the method to make it
// obvious if the dispatch were ever reached.
type nilPtrArmer struct{ called bool }

func (a *nilPtrArmer) ArmLocalWakeIntent(_ int) {
	if a == nil {
		panic("nilPtrArmer.ArmLocalWakeIntent reached with nil receiver — isNilArmer failed for Ptr")
	}
	a.called = true
}

// nilFuncArmer: kind=Func. Defined-type function with a method.
type nilFuncArmer func()

func (nilFuncArmer) ArmLocalWakeIntent(_ int) {}

// nilMapArmer: kind=Map.
type nilMapArmer map[string]int

func (nilMapArmer) ArmLocalWakeIntent(_ int) {}

// nilSliceArmer: kind=Slice.
type nilSliceArmer []int

func (nilSliceArmer) ArmLocalWakeIntent(_ int) {}

// nilChanArmer: kind=Chan.
type nilChanArmer chan int

func (nilChanArmer) ArmLocalWakeIntent(_ int) {}

// Note on reflect.Interface kind: reflect.ValueOf on a value that is
// already typed `WakeIntentArmer` returns the dynamic Kind of the
// concrete stored value, not Kind==Interface. Reaching Kind==Interface
// would require nested interface fields (e.g. a struct holding an
// interface field, inspected via Field()). Since WakeIntentArmer never
// gets wrapped that way at the SetEndpoint call site, the Interface
// branch in isNilArmer is defensive — included for completeness and
// future-proofing if the interface signature ever changes. The 5
// reachable kinds (Ptr, Func, Map, Slice, Chan) are all covered below
// alongside the untyped-nil case.

// TestIsNilArmer_AllNilKinds: every reachable nil-able kind must be
// detected as nil. Codex v2 amendment: explicit table over all cases
// via defined types with method receivers rather than a Ptr-only test
// with a comment placeholder.
func TestIsNilArmer_AllNilKinds(t *testing.T) {
	// Untyped nil — the trivial case isNilArmer must also handle.
	var untyped WakeIntentArmer

	// Each typed-nil below: declare the zero value, then assign to a
	// WakeIntentArmer variable so we get a true typed-nil interface.
	var ptrNil *nilPtrArmer
	var funcNil nilFuncArmer
	var mapNil nilMapArmer
	var sliceNil nilSliceArmer
	var chanNil nilChanArmer

	cases := []struct {
		name string
		val  WakeIntentArmer
	}{
		{"untyped-nil", untyped},
		{"typed-nil Ptr", ptrNil},
		{"typed-nil Func", funcNil},
		{"typed-nil Map", mapNil},
		{"typed-nil Slice", sliceNil},
		{"typed-nil Chan", chanNil},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if !isNilArmer(tc.val) {
				t.Fatalf("H5: isNilArmer(%s) = false, want true — typed-nil must be rejected", tc.name)
			}
		})
	}
}

// TestIsNilArmer_AcceptsConcrete: positive control. A real non-nil
// armer must NOT be treated as nil.
func TestIsNilArmer_AcceptsConcrete(t *testing.T) {
	concrete := &nilPtrArmer{}
	if isNilArmer(concrete) {
		t.Fatalf("H5: isNilArmer(concrete *nilPtrArmer) = true, want false — real armer must pass through")
	}
}

// TestICEBind_SetEndpoint_RejectsTypedNilPointer: end-to-end. A typed-
// nil pointer passed to SetEndpoint must NOT land in
// endpointsWakeArmer. Reproduces the original H5 prod-panic shape.
func TestICEBind_SetEndpoint_RejectsTypedNilPointer(t *testing.T) {
	b := setupICEBind(t)
	fakeIP := netip.MustParseAddr("127.2.42.7")

	var typedNil *nilPtrArmer // typed-nil pointer
	b.SetEndpoint(fakeIP, v18_19DiscardConn{}, typedNil)

	b.endpointsMu.Lock()
	_, present := b.endpointsWakeArmer[fakeIP]
	b.endpointsMu.Unlock()

	if present {
		t.Fatalf("H5: typed-nil *nilPtrArmer MUST NOT be stored in endpointsWakeArmer — Send would panic")
	}
}

// TestICEBind_SetEndpoint_AcceptsConcreteArmer: positive end-to-end.
// A real concrete armer must be registered so V18.19 wake-intent still
// works.
func TestICEBind_SetEndpoint_AcceptsConcreteArmer(t *testing.T) {
	b := setupICEBind(t)
	fakeIP := netip.MustParseAddr("127.2.42.8")

	armer := &stubWakeArmer{}
	b.SetEndpoint(fakeIP, v18_19DiscardConn{}, armer)

	b.endpointsMu.Lock()
	got, present := b.endpointsWakeArmer[fakeIP]
	b.endpointsMu.Unlock()

	if !present {
		t.Fatalf("H5: concrete armer MUST be registered in endpointsWakeArmer — Send arm path broken")
	}
	if got != armer {
		t.Fatalf("H5: endpointsWakeArmer[fakeIP] = %v, want %v", got, armer)
	}
}
