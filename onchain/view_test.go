package onchain

import (
	"testing"
	"time"

	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

// Rail.View copies every field, each to its own (distinct values, so a swapped pair shows).
func TestRailView(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	r := Rail{State: RailUnderpaid, Address: "bcrt1qexample", Issued: true, RequiredConfirmations: 3, Confirmations: 2,
		ReceivedSat: 11, ConfirmedSat: 22, LateSat: 33, Paid: true, Overpaid: false, Stale: true, PaidAt: at}
	want := rail.View{State: rail.Underpaid, Address: "bcrt1qexample", Issued: true, RequiredConfirmations: 3,
		Confirmations: 2, Received: 11, Confirmed: 22, Late: 33, Paid: true, Overpaid: false, Stale: true, PaidAt: at}
	if got := r.View(); got != want {
		t.Fatalf("View = %+v, want %+v", got, want)
	}
	r.Paid, r.Overpaid, r.Stale, r.Issued = false, true, false, false
	want.Paid, want.Overpaid, want.Stale, want.Issued = false, true, false, false
	if got := r.View(); got != want {
		t.Fatalf("View = %+v, want %+v", got, want)
	}
}

// Service.View, the peer probe: false and no error for an order with no on-chain amount (where Rail answers
// ErrNoOnchain), else Rail's view.
func TestServiceView(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	if v, ok, err := e.svc.View(bg, store.Order{ID: o.ID, AmountMsat: 1}); ok || err != nil || v != (rail.View{}) {
		t.Fatalf("no on-chain amount: %+v %v %v", v, ok, err)
	}
	e.payOrder(o, 120_000)
	e.node.Mine(1)
	e.passes(3)
	v, ok, err := e.svc.View(bg, o)
	r := e.rail(o)
	if !ok || err != nil || v != r.View() || v.State != rail.Overpaid || v.Received != 120_000 || v.Confirmed != 120_000 ||
		v.PaidAt.IsZero() || v.Confirmations < 1 {
		t.Fatalf("View = %+v %v %v; Rail %+v", v, ok, err, r)
	}
}
