package lnpay

import (
	"context"
	"time"

	"github.com/satbooth/satbooth/store"
)

// State is an order's state (design.md §1).
type State string

const (
	StateNew     State = "new"
	StatePaid    State = "paid"
	StateExpired State = "expired"
)

// Status is an order's derived status.
type Status struct {
	State   State
	Invoice *store.Invoice // the open, unexpired invoice, if any
	PaidAt  time.Time      // the time of the ledger transfer; zero unless paid
}

// Status derives the order's status from the store, the ledger and the clock. An order is paid when
// the ledger holds the transfer of any of its invoices. It is expired only when it is past its
// expiry (the store's boundary: ExpiresAt <= now) and every invoice it ever had is closed, and no
// Pay for it is in flight; until then it reads new, even long after its expiry, because a payment
// may still be recorded.
func (s *Service) Status(ctx context.Context, orderID string) (Status, error) {
	o, err := s.st.OrderByID(ctx, orderID)
	if err != nil {
		return Status{}, err
	}
	// The read order matters, and is the same in settleOrder: the clock, then the flight, then the
	// invoices and their closures, then the ledger. A Pay that finishes after the flight read has
	// stored its invoice before the invoices are read; a payment recorded after the invoices were read
	// is found by the ledger read at the end, which comes after the closures (a paid invoice is closed
	// only after its transfer exists), so a paid order can never read expired.
	now := s.now()
	flying := s.inFlight(o.ID)
	invs, err := s.st.InvoicesForOrder(ctx, o.ID)
	if err != nil {
		return Status{}, err
	}
	closed, err := s.closedSet(ctx, o.ID)
	if err != nil {
		return Status{}, err
	}
	paid, paidAt, err := s.orderPaidAt(ctx, o.ID)
	if err != nil {
		return Status{}, err
	}
	if paid {
		return Status{State: StatePaid, PaidAt: paidAt}, nil
	}
	st := Status{State: StateNew}
	allClosed := true
	for i := range invs {
		if !closed[invs[i].PaymentHash] {
			allClosed = false
			if invs[i].ExpiresAt.After(now) {
				inv := invs[i]
				st.Invoice = &inv
			}
		}
	}
	if !now.Before(o.ExpiresAt) && allClosed && !flying {
		st.State = StateExpired
	}
	return st, nil
}
