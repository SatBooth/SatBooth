package rail

import (
	"context"
	"time"

	"github.com/satbooth/satbooth/store"
)

// The states of View.State.
const (
	Waiting   = "waiting"
	Seen      = "seen"
	Paid      = "paid"
	Overpaid  = "overpaid"
	Late      = "late"
	Underpaid = "underpaid"
	Expired   = "expired"
)

// View is a rail's view of an order, for status, api and checkout, in the rail's base unit. Received is the counted
// live sum, Confirmed the sum of the counted outputs' forward transfers without a reversal, Late the live outputs first
// seen at or after the expiry. Confirmations is the least confirmation count of the counted live outputs (Least), 0 if
// one of them is in the mempool or not yet marked, and 0 with none. Paid is Confirmed >= the amount; Overpaid is Paid
// with more; PaidAt is the time of the transfer that first made the sum reach the amount (PaidAt).
type View struct {
	State                                string
	Address                              string
	Issued                               bool
	RequiredConfirmations, Confirmations int
	Received, Confirmed, Late            int64 // in the rail's base unit
	Paid, Overpaid, Stale                bool
	// Held is set in the view a rail gives its peer only: the rail holds the order's final event for an output it cannot
	// judge yet (the on-chain rail's behind output, while bitcoind's wallet lags). Not part of status, api or checkout.
	Held   bool
	PaidAt time.Time
}

// Events is what State needs of an order's events: Unpaid (order.expired or order.underpaid exists), ReversedNow (the
// last order.reverted has no order.paid_again) and Underpaid (order.underpaid exists).
type Events struct{ Unpaid, ReversedNow, Underpaid bool }

// State is the state of a view. Past the expiry, not paid and nothing late, the final unpaid states follow the order's
// events, so the API never shows one the events lack or one they contradict: events is called then, and only then.
// With order.underpaid or order.expired (or an order.reverted that no order.paid_again answers), it reads underpaid
// when it has a counted live sum or order.underpaid, else expired, whatever the live sum: once such an event exists,
// live money reads underpaid, not seen, until order.paid (or order.paid_again). With none of them, seen when it has a
// counted live sum, else waiting.
func State(v View, expired bool, events func() (Events, error)) (string, error) {
	switch {
	case v.Overpaid:
		return Overpaid, nil
	case v.Paid:
		return Paid, nil
	case v.Late > 0:
		return Late, nil
	case !expired && v.Received > 0:
		return Seen, nil
	case !expired:
		return Waiting, nil
	}
	// The two final unpaid states come from the events: settle writes them a pass late, holds them while an on-time
	// output died after the pass's scan, and may keep failing; until then the order reads seen (money
	// live: past the expiry and before the window end, settle waits for the whole amount to confirm) or waiting
	// (nothing live). An order.reverted that no order.paid_again answers backs them too: after order.paid settle writes
	// no order.expired or order.underpaid. The events are read first: money live again after such an event (a payment
	// evicted, then rebroadcast; or reorged back into the mempool) reads underpaid, never seen, which the event would
	// contradict.
	ev, err := events()
	if err != nil {
		return "", err
	}
	unpaid := ev.Unpaid || ev.ReversedNow
	switch {
	case !unpaid && v.Received > 0:
		return Seen, nil
	case !unpaid:
		return Waiting, nil
	case ev.Underpaid || v.Received > 0:
		return Underpaid, nil
	}
	return Expired, nil
}

// Peer is a rail as another rail of the same order sees it: its view of the order, and true, when the order has an
// amount on it; a zero View and false, with no error, when it has none. On an error the View and the bool are zero
// and mean nothing: a caller checks the error first, since a failed read is not "no amount on that rail".
//
// RevertedSeqs is the peer's half of step 1's heal (ruling R2): the seqs its reversals of the order's payments carry
// (RevertedSeqs over its outputs), read from the store and the ledger only, with no RPC call; nil when the order has
// no amount on the peer. A rail's settle adds the missing order.reverted for these as for its own, so a reversal
// whose event a crash or a store error lost on one rail is healed before the other rail derives a seq. Neither method
// takes the settle mutex: a settle calls them while it holds it.
type Peer interface {
	View(ctx context.Context, o store.Order) (View, bool, error)
	RevertedSeqs(ctx context.Context, o store.Order) ([]int, error)
}

// PeerFacts is what a rail's settle reads of a peer rail's view of an order: Paid (the peer reads paid; the order is
// paid, as by Lightning), Money (a counted or late live sum on the peer: money on two rails needs a person, 6.1-W),
// Pending (the peer's counted live sum reaches its amount and it is not yet paid: it holds the final event, Final's
// PeerPending), Received (a counted live sum above 0: the final event is order.underpaid, Final's PeerReceived) and
// Stale (the peer's view is stale and not paid: the peer may hold money it cannot see, so it holds the final event
// as a pending peer does, as long as it stays stale; Final's PeerStale, rulings 8-T7d and 8-T7e).
// Held is the peer's Held while it is not paid: the peer holds the final event for an output it cannot judge yet. It
// implies Stale (the peer holds the event as a stale peer does) and adds that the rail's left-the-window check carries
// the order instead of reporting it (the hold lasts a pass or two, not until a person looks).
type PeerFacts struct{ Paid, Money, Pending, Received, Stale, Held bool }

// PeerFactsOf derives the facts from a peer's View answer (ok false: the order has no amount on the peer, so nothing)
// and the order's amount on the peer, in the peer's base unit.
func PeerFactsOf(v View, ok bool, amount int64) PeerFacts {
	if !ok {
		return PeerFacts{}
	}
	return PeerFacts{Paid: v.Paid, Money: v.Received > 0 || v.Late > 0, Pending: v.Received >= amount && !v.Paid,
		Received: v.Received > 0, Stale: (v.Stale || v.Held) && !v.Paid, Held: v.Held && !v.Paid}
}

// MoneyElsewhere is the other half of the both-rails rule: Lightning reads paid, or the peer reads paid or holds money
// (counted or late). Money on two rails needs a person (6.1-W), not only an order paid on two.
func MoneyElsewhere(lightningPaid bool, peer PeerFacts) bool {
	return lightningPaid || peer.Paid || peer.Money
}

// BothRails is the both-rails rule, the one definition both settles use: a record on this rail (BothRecorded) beside
// money elsewhere (MoneyElsewhere).
func BothRails(outs []Out, lightningPaid bool, peer PeerFacts) bool {
	return BothRecorded(outs) && MoneyElsewhere(lightningPaid, peer)
}
