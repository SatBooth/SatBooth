package rail

import "github.com/satbooth/satbooth/store"

// OrderEvents is what a rail's settle and view read of an order's events (EventsOf).
type OrderEvents struct {
	Paid      bool         // order.paid
	Final     bool         // order.paid, order.expired or order.underpaid
	Unpaid    bool         // order.expired or order.underpaid
	Underpaid bool         // order.underpaid
	Reverted  int          // the number of order.reverted
	PaidAgain map[int]bool // the seqs of the order.paid_again events
}

// EventsOf folds an order's events. Other types (order.late, and any unknown one) count for nothing.
func EventsOf(l []store.Event) OrderEvents {
	ev := OrderEvents{PaidAgain: map[int]bool{}}
	for _, e := range l {
		switch e.Type {
		case store.EventPaid:
			ev.Paid, ev.Final = true, true
		case store.EventExpired:
			ev.Final, ev.Unpaid = true, true
		case store.EventUnderpaid:
			ev.Final, ev.Unpaid, ev.Underpaid = true, true, true
		case store.EventReverted:
			ev.Reverted++
		case store.EventPaidAgain:
			ev.PaidAgain[e.Seq] = true
		}
	}
	return ev
}

// ReversedNow reports whether the order's last order.reverted has no order.paid_again: the order reads unpaid by its
// events (State), and a paid one no longer reads paid (a rail's settled check). The one statement of that rule.
func (ev OrderEvents) ReversedNow() bool { return ev.Reverted > 0 && !ev.PaidAgain[ev.Reverted] }

// ForState is State's input from the fold.
func (ev OrderEvents) ForState() Events {
	return Events{Unpaid: ev.Unpaid, ReversedNow: ev.ReversedNow(), Underpaid: ev.Underpaid}
}
