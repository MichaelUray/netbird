package peer

import (
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestHandshaker_AddRemoveICEListener(t *testing.T) {
	h := &Handshaker{}
	listener := func(o *OfferAnswer) {}

	h.AddICEListener(listener)
	if h.iceListener == nil {
		t.Fatal("iceListener should be set after AddICEListener")
	}

	h.RemoveICEListener()
	if h.iceListener != nil {
		t.Fatal("iceListener should be nil after RemoveICEListener")
	}

	// Idempotency: removing again is a no-op.
	h.RemoveICEListener()
	if h.iceListener != nil {
		t.Fatal("RemoveICEListener should be idempotent")
	}

	// Re-add works.
	h.AddICEListener(listener)
	if h.iceListener == nil {
		t.Fatal("re-adding the listener should work")
	}
}

func TestHandshaker_readICEListener(t *testing.T) {
	h := &Handshaker{}
	if got := h.readICEListener(); got != nil {
		t.Fatal("readICEListener on empty Handshaker should return nil")
	}

	listener := func(o *OfferAnswer) {}
	h.AddICEListener(listener)
	if got := h.readICEListener(); got == nil {
		t.Fatal("readICEListener after AddICEListener should return non-nil")
	}

	h.RemoveICEListener()
	if got := h.readICEListener(); got != nil {
		t.Fatal("readICEListener after RemoveICEListener should return nil")
	}
}

func TestHandshaker_OnRemoteOffer_BufferedChannelAcceptsBurst(t *testing.T) {
	// V18.16 (2026-06-20): remoteOffersCh and remoteAnswerCh are buffered
	// to size 4 so that early OFFERs/ANSWERs arriving between
	// NewHandshaker and the first iteration of Listen()'s select loop
	// are not dropped with "skipping remote offer message because
	// receiver not ready". This test verifies the buffer accepts a
	// 3-offer burst WITHOUT a parked receiver.
	h := &Handshaker{
		log:            log.WithField("test", "v18.16"),
		remoteOffersCh: make(chan OfferAnswer, 4),
		remoteAnswerCh: make(chan OfferAnswer, 4),
	}
	for i := 0; i < 3; i++ {
		h.OnRemoteOffer(OfferAnswer{})
	}
	// We expect 3 items queued in remoteOffersCh and no panic.
	if got := len(h.remoteOffersCh); got != 3 {
		t.Fatalf("buffered channel should hold 3 queued offers, got %d", got)
	}
}

func TestHandshaker_OnRemoteAnswer_BufferedChannelAcceptsBurst(t *testing.T) {
	h := &Handshaker{
		log:            log.WithField("test", "v18.16"),
		remoteOffersCh: make(chan OfferAnswer, 4),
		remoteAnswerCh: make(chan OfferAnswer, 4),
	}
	for i := 0; i < 3; i++ {
		h.OnRemoteAnswer(OfferAnswer{})
	}
	if got := len(h.remoteAnswerCh); got != 3 {
		t.Fatalf("buffered channel should hold 3 queued answers, got %d", got)
	}
}
