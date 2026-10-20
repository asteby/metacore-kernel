package payments

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func tokens() func() string {
	n := 0
	return func() string { n++; return fmt.Sprintf("tok-%028d", n) }
}

func TestHoldLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	gen := tokens()
	req := HoldRequest{DocumentType: "sales_order", DocumentID: "SO-1", Holder: "kiosk:K1", Channel: ChannelTerminal}

	tok1 := fmt.Sprintf("tok-%028d", 1)
	h, err := Acquire(nil, req, now, gen)
	if err != nil || h.Token != tok1 || !h.Live(now) || !h.ExpiresAt.Equal(now.Add(DefaultHoldTTL)) {
		t.Fatalf("acquire: %+v %v", h, err)
	}
	// The cash desk tries to charge the same ticket while the kiosk's cash
	// recycler is taking bills (7leguas #604).
	other := HoldRequest{DocumentType: "sales_order", DocumentID: "SO-1", Holder: "pos:caja-1"}
	_, err = Acquire(&h, other, now.Add(time.Minute), gen)
	var held *HeldError
	if !errors.As(err, &held) || !errors.Is(err, ErrHeld) || held.Holder != "kiosk:K1" {
		t.Fatalf("second point of sale got the hold: %v", err)
	}
	if err := CheckCharge(&h, "", now.Add(time.Minute)); !errors.Is(err, ErrHeld) {
		t.Fatalf("charge without the token passed: %v", err)
	}
	if err := CheckCharge(&h, tok1, now.Add(time.Minute)); err != nil {
		t.Fatalf("owner charge refused: %v", err)
	}
	// Renewal keeps the token.
	req.Token = tok1
	renewed, err := Acquire(&h, req, now.Add(4*time.Minute), gen)
	if err != nil || renewed.Token != tok1 || !renewed.ExpiresAt.Equal(now.Add(4*time.Minute+DefaultHoldTTL)) {
		t.Fatalf("renew: %+v %v", renewed, err)
	}
	// A kiosk that died leaves an expired hold anyone replaces.
	late := renewed.ExpiresAt.Add(time.Second)
	if err := CheckCharge(&renewed, "", late); err != nil {
		t.Fatalf("expired hold still blocks: %v", err)
	}
	taken, err := Acquire(&renewed, other, late, gen)
	if err != nil || taken.Token == tok1 || taken.Holder != "pos:caja-1" {
		t.Fatalf("takeover of expired hold: %+v %v", taken, err)
	}
	if _, err := Release(&taken, tok1, false, late); !errors.Is(err, ErrHoldNotOwner) {
		t.Fatalf("release by non-owner: %v", err)
	}
	rel, err := Release(&taken, "", true, late)
	if err != nil || rel.Status != HoldReleased {
		t.Fatalf("forced release (document paid): %+v %v", rel, err)
	}
	if _, err := Consume(&taken, "nope", late); !errors.Is(err, ErrHoldNotOwner) {
		t.Fatal("consume by non-owner")
	}
	if c, err := Consume(&taken, taken.Token, late); err != nil || c.Status != HoldConsumed {
		t.Fatalf("consume: %+v %v", c, err)
	}
	// r1/r4: a released or expired copy is not consumed: the overlap with
	// whoever holds the document now is detected.
	if _, err := Consume(rel, rel.Token, late); !errors.Is(err, ErrHoldExpired) {
		t.Fatalf("consume of a released hold: %v", err)
	}
	if _, err := Consume(&taken, taken.Token, taken.ExpiresAt.Add(time.Second)); !errors.Is(err, ErrHoldExpired) {
		t.Fatalf("consume of an expired hold: %v", err)
	}
	if _, err := Acquire(nil, HoldRequest{DocumentType: "sales_order"}, now, gen); err == nil {
		t.Fatal("hold without document accepted")
	}
	if _, err := Acquire(nil, other, now, func() string { return "" }); err == nil {
		t.Fatal("empty token accepted")
	}
	if _, err := Acquire(nil, other, now, func() string { return "short-token" }); err == nil {
		t.Fatal("guessable short token accepted")
	}
	// nil generator = crypto/rand, 256 bits, never repeated.
	a, err := Acquire(nil, other, now, nil)
	b, err2 := Acquire(nil, other, now, nil)
	if err != nil || err2 != nil || len(a.Token) != 64 || a.Token == b.Token {
		t.Fatalf("default tokens: %q %q %v %v", a.Token, b.Token, err, err2)
	}
}

func TestClampHoldTTL(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		0: DefaultHoldTTL, -time.Second: DefaultHoldTTL, time.Second: MinHoldTTL,
		10 * time.Minute: 10 * time.Minute, 3 * time.Hour: MaxHoldTTL,
	} {
		if got := ClampHoldTTL(in); got != want {
			t.Errorf("ClampHoldTTL(%s) = %s, want %s", in, got, want)
		}
	}
}
