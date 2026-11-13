package onchain

import (
	"context"
	"errors"
	"time"

	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

// The states of Rail.State (rail's).
const (
	RailWaiting   = rail.Waiting
	RailSeen      = rail.Seen
	RailPaid      = rail.Paid
	RailOverpaid  = rail.Overpaid
	RailLate      = rail.Late
	RailUnderpaid = rail.Underpaid
	RailExpired   = rail.Expired
)

// Rail is the on-chain rail's view of an order, for status, api and checkout.
//
// ReceivedSat is the counted live sum (outputs first seen before the expiry that are not dead), ConfirmedSat the sum of
// the counted outputs' forward transfers without a reversal, LateSat the live outputs first seen at or after the
// expiry. Past the expiry, not paid and nothing late, the final unpaid states follow the order's events, so the API
// never shows one the events lack or one they contradict. With order.underpaid or order.expired (or an order.reverted
// that no order.paid_again answers), it reads underpaid when it has a counted live sum or order.underpaid, else
// expired, whatever the live sum: once such an event exists, live money reads underpaid, not seen, until order.paid
// (or order.paid_again). With none of them, seen when it has a counted live sum, else waiting. So expired and
// underpaid lag the expiry (and the window end) by up to one pass, and an order whose settle keeps failing reads
// waiting or seen for good (logNoFinal at its window end).
// Confirmations is the least confirmation count of the counted live outputs, 0 if one of them is in the mempool or not
// yet marked, and 0 with none. A mined output that is not recorded counts at most the reading of the last completed
// pass (orderViews, boundOut), so it shows 0 until a pass has read its mark (after a start, a failed read, or a pass
// that ended in step 3). Paid is ConfirmedSat >= the amount; Overpaid is Paid with more; PaidAt is the time of
// the transfer that first made the sum reach the amount. Stale is true when no pass has succeeded within 3
// PollIntervals plus the last good pass's duration (so a long healthy pass, many tracked outputs at PollRate, does not
// read stale while the next one runs), before the first good pass (the page shows "checking"), and for an order in the
// stale set of the last pass that reached its end: one of its outputs' calls failed, or its settle failed (pass).
type Rail struct {
	State                                string
	Address                              string
	Issued                               bool
	RequiredConfirmations, Confirmations int
	ReceivedSat, ConfirmedSat, LateSat   int64
	Paid, Overpaid, Stale                bool
	PaidAt                               time.Time
}

// View is the rail's view of the order in rail's shape, field by field (ReceivedSat is Received, and so on).
func (r Rail) View() rail.View {
	return rail.View{State: r.State, Address: r.Address, Issued: r.Issued, RequiredConfirmations: r.RequiredConfirmations,
		Confirmations: r.Confirmations, Received: r.ReceivedSat, Confirmed: r.ConfirmedSat, Late: r.LateSat, Paid: r.Paid,
		Overpaid: r.Overpaid, Stale: r.Stale, PaidAt: r.PaidAt}
}

// View is the on-chain rail as a peer rail sees it (rail.Peer): false, with no error, when the order has no on-chain
// amount; else Rail's view. It does not take the settle mutex: the peer's settle calls it while holding that mutex
// (the rails share it), so the settle is the snapshot, and locking here would wait on the caller.
func (s *Service) View(ctx context.Context, o store.Order) (rail.View, bool, error) {
	if o.AmountSat == 0 {
		return rail.View{}, false, nil
	}
	r, err := s.rail(ctx, o)
	if err != nil {
		return rail.View{}, false, err
	}
	v := r.View()
	// An on-time output that cannot be judged yet makes the peer hold its final event, but only while this rail is
	// fresh: a stale rail (no good pass within its bound, a failed call for the order, or none yet since the start)
	// reports no hold, so the peer treats it as any stale peer (rulings 8-T7d, 8-T7e) and a hold never outlives its pass.
	if !v.Paid && !v.Stale {
		if v.Held, err = s.onTimeBehind(ctx, o); err != nil {
			return rail.View{}, false, err
		}
	}
	return v, true, nil
}

// onTimeBehind reports whether an on-time output of the order was behind in the last completed step 3 (behindNow: the
// node has replaced its payment and bitcoind's wallet has not processed that). Its mark says mempool, so the peer would
// read live money that the node contradicts: it holds its final event as it does for a stale view and does not report the
// order as left without one (rail.PeerFacts.Held), until the wallet has caught up, as this rail holds its own
// (heldInPass). Only the peer's View says so (View.Held): Rail, and so the API, does not read stale for it.
func (s *Service) onTimeBehind(ctx context.Context, o store.Order) (bool, error) {
	if _, held := s.heldNow()[o.ID]; held { // this rail held the order's final event in its last pass
		return true, nil
	}
	behind := s.behindNow()
	if len(behind) == 0 {
		return false, nil
	}
	txos, err := s.st.TXOsForOrder(ctx, o.ID)
	if err != nil {
		return false, err
	}
	for _, x := range txos {
		if _, ok := behind[store.TXOKey{TxID: x.TxID, Vout: x.Vout}]; ok && x.FirstSeen.Before(o.ExpiresAt) {
			return true, nil
		}
	}
	return false, nil
}

// RevertedSeqs is the on-chain rail's half of a peer's step 1 heal (rail.Peer, ruling R2): the seqs its reversals of
// the order's outputs carry, from the store and the ledger only (outputView: no mark, no RPC call); nil when the order
// has no on-chain amount. Like View it does not take the settle mutex: the peer's settle calls it while it holds that
// mutex.
func (s *Service) RevertedSeqs(ctx context.Context, o store.Order) ([]int, error) {
	if o.AmountSat == 0 {
		return nil, nil
	}
	txos, err := s.st.TXOsForOrder(ctx, o.ID)
	if err != nil {
		return nil, err
	}
	views := make([]outView, 0, len(txos))
	for _, x := range txos {
		v, err := s.outputView(ctx, o, x, false)
		if err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return rail.RevertedSeqs(outs(views)), nil
}

var _ rail.Peer = (*Service)(nil)

// Rail derives the rail's view of an order from the store, the ledger, the last cursor and the last good pass. It
// makes no RPC call. An order without an on-chain amount is ErrNoOnchain.
//
// Rail holds the settle mutex while it reads (ruling R1), so status, the API and the page read a settle's ledger
// records and its events together. The records themselves are never half written: settle writes a new output's forward
// and an old one's reversal as one batch (ruling S1), so even a settle that ends early leaves no sum of both.
func (s *Service) Rail(ctx context.Context, o store.Order) (Rail, error) {
	if o.AmountSat == 0 {
		return Rail{}, ErrNoOnchain
	}
	mu := s.settleLock()
	mu.Lock()
	defer mu.Unlock()
	return s.rail(ctx, o)
}

// rail is Rail without the settle mutex: View's path, which a peer's settle calls while it holds that mutex.
func (s *Service) rail(ctx context.Context, o store.Order) (Rail, error) {
	now := s.now()
	r := Rail{RequiredConfirmations: Required(s.cfg.Tiers, o.AmountSat)}
	last := s.lastGoodPass.Load()
	r.Stale = last == 0 || now.Sub(time.Unix(0, last)) > 3*s.cfg.PollInterval+time.Duration(s.lastPassDur.Load())
	s.staleMu.Lock()
	_, stale := s.staleOrders[o.ID]
	s.staleMu.Unlock()
	r.Stale = r.Stale || stale

	a, err := s.st.AddressForOrder(ctx, o.ID)
	switch {
	case err == nil:
		r.Issued, r.Address = true, a.Address
	case !errors.Is(err, store.ErrNotFound):
		return Rail{}, err
	}
	var views []outView
	if r.Issued {
		if views, err = s.orderViews(ctx, o, false); err != nil {
			return Rail{}, err
		}
	}
	cur, _, err := s.st.Cursor(ctx)
	if err != nil {
		return Rail{}, err
	}
	ov := outs(views) // the views are bounded already (orderViews): the count shown is the one the paid decision uses
	r.ConfirmedSat, r.ReceivedSat, r.LateSat = rail.Sums(ov, store.MaxSat)
	if c, ok := rail.Least(ov, cur.Height); ok {
		r.Confirmations = c
	}
	r.Paid = r.ConfirmedSat >= o.AmountSat
	r.Overpaid = r.Paid && r.ConfirmedSat > o.AmountSat
	if r.Paid {
		r.PaidAt = rail.PaidAt(ov, o.AmountSat, store.MaxSat)
	}

	// The state (rail.State). Past the expiry, not paid and nothing late, the two final unpaid states come from the
	// events, read only then.
	r.State, err = rail.State(r.View(), !now.Before(o.ExpiresAt), func() (rail.Events, error) {
		ev, err := s.readEvents(ctx, o.ID)
		if err != nil {
			return rail.Events{}, err
		}
		return ev.ForState(), nil
	})
	if err != nil {
		return Rail{}, err
	}
	return r, nil
}
