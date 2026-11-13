package rail

import (
	"testing"

	"github.com/satbooth/satbooth/store"
)

func evs(types ...string) []store.Event {
	var l []store.Event
	for _, t := range types {
		l = append(l, store.Event{Type: t, Seq: 1})
	}
	return l
}

func again(seqs ...int) []store.Event {
	var l []store.Event
	for _, s := range seqs {
		l = append(l, store.Event{Type: store.EventPaidAgain, Seq: s})
	}
	return l
}

// The fold of an order's events: each type sets its own flags, and only those.
func TestEventsOf(t *testing.T) {
	type flags struct{ paid, final, unpaid, underpaid bool }
	for name, tc := range map[string]struct {
		l        []store.Event
		want     flags
		reverted int
	}{
		"none":                  {nil, flags{}, 0},
		"paid":                  {evs(store.EventPaid), flags{paid: true, final: true}, 0},
		"expired":               {evs(store.EventExpired), flags{final: true, unpaid: true}, 0},
		"underpaid":             {evs(store.EventUnderpaid), flags{final: true, unpaid: true, underpaid: true}, 0},
		"late counts nothing":   {evs(store.EventLate), flags{}, 0},
		"an unknown type":       {evs("order.created", "", "order.PAID"), flags{}, 0},
		"one reverted":          {evs(store.EventReverted), flags{}, 1},
		"three reverted":        {evs(store.EventReverted, store.EventReverted, store.EventReverted), flags{}, 3},
		"paid after expired":    {evs(store.EventExpired, store.EventPaid), flags{paid: true, final: true, unpaid: true}, 0},
		"paid twice":            {evs(store.EventPaid, store.EventPaid), flags{paid: true, final: true}, 0},
		"paid_again only":       {again(1), flags{}, 0},
		"everything, reordered": {evs(store.EventReverted, store.EventUnderpaid, store.EventLate, store.EventPaid), flags{true, true, true, true}, 1},
	} {
		ev := EventsOf(tc.l)
		got := flags{ev.Paid, ev.Final, ev.Unpaid, ev.Underpaid}
		if got != tc.want || ev.Reverted != tc.reverted || ev.PaidAgain == nil {
			t.Errorf("%s: EventsOf = %+v", name, ev)
		}
	}
	ev := EventsOf(append(evs(store.EventReverted, store.EventReverted), again(1, 2, 7)...))
	if len(ev.PaidAgain) != 3 || !ev.PaidAgain[1] || !ev.PaidAgain[2] || !ev.PaidAgain[7] || ev.PaidAgain[3] {
		t.Fatalf("PaidAgain = %v", ev.PaidAgain)
	}
}

// ReversedNow: the last order.reverted (the count) has no order.paid_again of its seq.
func TestReversedNow(t *testing.T) {
	rev := func(n int) []store.Event {
		var l []store.Event
		for range n {
			l = append(l, store.Event{Type: store.EventReverted, Seq: 1})
		}
		return l
	}
	for name, tc := range map[string]struct {
		l    []store.Event
		want bool
	}{
		"none":                          {nil, false},
		"paid_again 0 without reverts":  {again(0), false},
		"one reverted":                  {rev(1), true},
		"one reverted, paid again":      {append(rev(1), again(1)...), false},
		"two reverted, the first again": {append(rev(2), again(1)...), true},
		"two reverted, the last again":  {append(rev(2), again(2)...), false},
		"two reverted, both again":      {append(rev(2), again(1, 2)...), false},
		"paid again for a later seq":    {append(rev(2), again(3)...), true},
		"paid again for seq 0":          {append(rev(1), again(0)...), true},
	} {
		if got := EventsOf(tc.l).ReversedNow(); got != tc.want {
			t.Errorf("%s: ReversedNow = %v", name, got)
		}
	}
	// The zero OrderEvents (no map) reads not reversed and does not panic.
	if (OrderEvents{}).ReversedNow() || !(OrderEvents{Reverted: 1}).ReversedNow() {
		t.Fatal("the zero OrderEvents")
	}
}

// ForState is State's input: Unpaid, ReversedNow and Underpaid, each from its own source.
func TestForState(t *testing.T) {
	for name, tc := range map[string]struct {
		l    []store.Event
		want Events
	}{
		"none":      {nil, Events{}},
		"expired":   {evs(store.EventExpired), Events{Unpaid: true}},
		"underpaid": {evs(store.EventUnderpaid), Events{Unpaid: true, Underpaid: true}},
		"reverted":  {evs(store.EventReverted), Events{ReversedNow: true}},
		"paid":      {evs(store.EventPaid), Events{}},
		"all":       {evs(store.EventUnderpaid, store.EventReverted), Events{Unpaid: true, ReversedNow: true, Underpaid: true}},
	} {
		if got := EventsOf(tc.l).ForState(); got != tc.want {
			t.Errorf("%s: ForState = %+v, want %+v", name, got, tc.want)
		}
	}
}
