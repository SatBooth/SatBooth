package rail

import (
	"context"

	"github.com/satbooth/satbooth/store"
)

// LogPaidAfterFinal is the ALERT of an order.paid added after order.expired or order.underpaid, the same on every
// rail. It names the order, nothing else.
const LogPaidAfterFinal = "ALERT: an order was paid after its order.expired or order.underpaid event; the payment is recorded and the order reads paid"

// Paying is what PaidEvents writes through, in a rail: the store's account fact, lnpay's in-memory set of known
// accounts, the store's events (with the rail's settle:after-event hook, run when an event is added), the rail's test
// hook, and its log.
type Paying interface {
	AddAccountPaid(ctx context.Context, orderID string) (bool, error)
	MarkKnown(account string)
	AddEvent(ctx context.Context, typ, orderID string, seq int) (bool, error)
	At(point string) error
	Alert(line, orderID string)
}

// PaidEvents are the two level-triggered rules of a paid order: order.paid when absent, and order.paid_again for the
// latest order.reverted when absent (Ruling C: the id is EventID("order.paid_again", orderID, n)), with the account
// fact before order.paid (Ruling T2). order.paid may follow order.expired or order.underpaid (Ruling S: a counted
// payment evicted before the expiry and mined after it); the money arrived, so it is recorded and the order reads
// paid, with LogPaidAfterFinal. paidAgain is the rail's ALERT for an added order.paid_again. ev is updated as the
// events are added.
func PaidEvents(ctx context.Context, p Paying, o store.Order, ev *OrderEvents, paidAgain string) error {
	if !ev.Paid {
		// Ruling T2: a paid order makes its account known, whatever the rail. The fact first, then lnpay's in-memory
		// set, then the event, as lnpay's completePaid does; a crash in between is completed by the next pass.
		if _, err := p.AddAccountPaid(ctx, o.ID); err != nil {
			return err
		}
		if err := p.At("settle:after-account"); err != nil {
			return err
		}
		p.MarkKnown(o.AccountID)
		added, err := p.AddEvent(ctx, store.EventPaid, o.ID, 1)
		if added && ev.Unpaid {
			p.Alert(LogPaidAfterFinal, o.ID)
		}
		if err != nil {
			return err
		}
		ev.Paid, ev.Final = true, true
	}
	if n := ev.Reverted; n >= 1 && !ev.PaidAgain[n] {
		added, err := p.AddEvent(ctx, store.EventPaidAgain, o.ID, n)
		if added {
			p.Alert(paidAgain, o.ID)
		}
		if err != nil {
			return err
		}
		ev.PaidAgain[n] = true
	}
	return nil
}

// The paid-again ALERTs, one per rail whose money can make a reverted order paid again. Each names the order, nothing
// else.
const (
	LogPaidAgainOnchain   = "ALERT: a reverted on-chain payment confirmed again"
	LogPaidAgainMonero    = "ALERT: a reverted Monero payment confirmed again"
	LogPaidAgainLightning = "ALERT: an order reverted by a reorg or a double spend was paid again on Lightning"
)

// PaidAgainLine is the paid-again ALERT for the rail whose money makes the order paid: the calling rail's own line
// when its confirmed sum reaches the amount (ownPaid), else the peer's when the peer reads paid, else Lightning's.
func PaidAgainLine(ownPaid, peerPaid bool, own, peer string) string {
	switch {
	case ownPaid:
		return own
	case peerPaid:
		return peer
	}
	return LogPaidAgainLightning
}
