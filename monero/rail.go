package monero

import (
	"context"
	"errors"
	"time"

	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

// Rail derives the Monero rail's view of an order from the store, the ledger, the last cursor and the last good pass;
// it makes no RPC call. Received is the counted live sum (payments first seen before the expiry and not locked beyond
// the normal lock, that are not dead), Confirmed the sum of the counted payments' forward transfers without a reversal,
// Late the live payments that are not counted. Confirmations is the least confirmation count of the counted live
// payments, as wallet-rpc counts them (railTip, from the wallet height the last pass stored, ruling 8-A2). Stale is
// true when no pass has succeeded within 3 PollIntervals plus the last good pass's duration, before the first good
// pass, and for an order in the stale set of the last pass that reached its end. The state is rail.State's. An order
// without a Monero amount is ErrNoMonero.
//
// Rail holds the settle mutex while it reads (ruling R1), so status, the API and the page read a settle's ledger
// records and its events together. The records themselves are never half written: settle writes a new payment's forward
// and an old one's reversal as one batch (ruling S1), so even a settle that ends early leaves no sum of both.
// The Monero pass holds the mutex across its commit and all its settles (ruling S2), so Rail waits for that section.
func (s *Service) Rail(ctx context.Context, o store.Order) (rail.View, error) {
	if o.AmountPiconero == 0 {
		return rail.View{}, ErrNoMonero
	}
	mu := s.settleLock()
	mu.Lock()
	defer mu.Unlock()
	return s.rail(ctx, o)
}

// rail is Rail without the settle mutex: View's path, which a peer's settle calls while it holds that mutex.
func (s *Service) rail(ctx context.Context, o store.Order) (rail.View, error) {
	now := s.now()
	v := rail.View{RequiredConfirmations: rail.Required(s.cfg.Tiers, o.AmountPiconero)}
	last := s.lastGoodPass.Load()
	v.Stale = last == 0 || now.Sub(time.Unix(0, last)) > 3*s.cfg.PollInterval+time.Duration(s.lastPassDur.Load())
	s.staleMu.Lock()
	_, stale := s.staleOrders[o.ID]
	s.staleMu.Unlock()
	v.Stale = v.Stale || stale

	a, err := s.st.SubaddressForOrder(ctx, o.ID)
	switch {
	case err == nil:
		v.Issued, v.Address = true, a.Address
	case !errors.Is(err, store.ErrNotFound):
		return rail.View{}, err
	}
	var views []payView
	if v.Issued {
		if views, err = s.orderViews(ctx, o, false); err != nil {
			return rail.View{}, err
		}
	}
	cur, _, err := s.st.MoneroCursor(ctx)
	if err != nil {
		return rail.View{}, err
	}
	ov := outs(views)
	v.Confirmed, v.Received, v.Late = rail.Sums(ov, store.MaxPiconero)
	if c, ok := rail.Least(ov, railTip(cur.WalletHeight)); ok {
		v.Confirmations = c
	}
	v.Paid = v.Confirmed >= o.AmountPiconero
	v.Overpaid = v.Paid && v.Confirmed > o.AmountPiconero
	if v.Paid {
		v.PaidAt = rail.PaidAt(ov, o.AmountPiconero, store.MaxPiconero)
	}
	v.State, err = rail.State(v, !now.Before(o.ExpiresAt), func() (rail.Events, error) {
		ev, err := s.readEvents(ctx, o.ID)
		if err != nil {
			return rail.Events{}, err
		}
		return ev.ForState(), nil
	})
	if err != nil {
		return rail.View{}, err
	}
	return v, nil
}

// View is the Monero rail as a peer rail sees it (rail.Peer): false, with no error, when the order has no Monero
// amount; else Rail's view. It does not take the settle mutex: the peer's settle calls it while holding that mutex
// (the rails share it), so the settle is the snapshot, and locking here would wait on the caller.
func (s *Service) View(ctx context.Context, o store.Order) (rail.View, bool, error) {
	if o.AmountPiconero == 0 {
		return rail.View{}, false, nil
	}
	v, err := s.rail(ctx, o)
	if err != nil {
		return rail.View{}, false, err
	}
	return v, true, nil
}

// RevertedSeqs is the Monero rail's half of a peer's step 1 heal (rail.Peer, ruling R2): the seqs its reversals of
// the order's payments carry, from the store and the ledger only (paymentViews: no mark, no RPC call); nil when the
// order has no Monero amount. Like View it does not take the settle mutex: the peer's settle calls it while it holds
// that mutex.
func (s *Service) RevertedSeqs(ctx context.Context, o store.Order) ([]int, error) {
	if o.AmountPiconero == 0 {
		return nil, nil
	}
	views, err := s.paymentViews(ctx, o)
	if err != nil {
		return nil, err
	}
	return rail.RevertedSeqs(outs(views)), nil
}

var _ rail.Peer = (*Service)(nil)
