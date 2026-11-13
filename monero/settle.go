package monero

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

// Fixed log lines of settle. Each names the order, nothing else.
const (
	logDifferent         = "ALERT: a payment record exists with different details"
	logBothRails         = "ALERT: an order was paid on both rails"
	logOverpaid          = "ALERT: an order was overpaid; refund the excess"
	logReorg             = "ALERT: a Monero payment was reverted by a reorg or a double spend"
	logLate              = "ALERT: a late Monero payment arrived after the order's expiry"
	logUnderpaid         = "ALERT: an order was underpaid at expiry"
	logUnderpaidAtWindow = "ALERT: an order was underpaid at the end of its watch window; its payment is not yet confirmed enough and stays tracked"
)

// railTip turns a wallet height into the tip rail's rules read. wallet-rpc's height is a count of blocks (get_height
// answers get_blockchain_current_height, src/wallet/wallet_rpc_server.cpp:957-962 at v0.18.5.4), and it counts a
// transfer's confirmations as that height less the transfer's block height (set_confirmations, :163-168). rail counts
// tip - height + 1 (bitcoind's tip is a block's height), so the Monero rail passes the wallet's height less one:
// a payment mined in the wallet's last block has 1 confirmation, as every Monero wallet shows it. The wallet height is
// the one the last pass read (MoneroCursor.WalletHeight, ruling 8-A2), never the cursor's height, which is where the
// next scan starts and stays above a wallet that is catching up. FirstSeenHeight is the wallet's height too (ruling
// R6: lockedBeyondNormal is judged against it).
func railTip(walletHeight int64) int64 { return walletHeight - 1 }

func addPico(a, b int64) int64 { return rail.Add(a, b, store.MaxPiconero) }

// orderViews reads the order's payments, their current marks and their ledger records. alert logs logDifferent for
// a payment whose transfer does not match it.
func (s *Service) orderViews(ctx context.Context, o store.Order, alert bool) ([]payView, error) {
	views, err := s.paymentViews(ctx, o)
	if err != nil || len(views) == 0 {
		return views, err
	}
	keys := make([]store.PaymentKey, len(views))
	for i, v := range views {
		keys[i] = store.PaymentKey{TxID: v.p.TxID, Address: v.p.Address}
	}
	marks, err := s.st.CurrentMoneroMarks(ctx, keys)
	if err != nil {
		return nil, err
	}
	settled, err := s.st.MoneroSettledKeys(ctx, keys)
	if err != nil {
		return nil, err
	}
	for i := range views {
		m, ok := marks[keys[i]]
		views[i].Marked, views[i].Kind, views[i].Height = ok, m.Kind, m.Height
		// Ruling 8-A6: never above wallet-rpc's own count until the payment is settled. A settled payment (its stored
		// fact, ruling S5: not its stored count against today's deepest tier, which a restart may raise) counts
		// WalletHeight − height, so its count keeps growing instead of freezing; track
		// stores at most the depth, and no rule reads a count past it.
		views[i].Bounded = !settled[keys[i]]
		views[i].Observed = m.Confirmations
		if views[i].Foreign && alert {
			s.log.Error(logDifferent, "order", o.ID)
		}
	}
	return views, nil
}

// readEvents reads the order's events and folds them (rail.EventsOf).
func (s *Service) readEvents(ctx context.Context, orderID string) (rail.OrderEvents, error) {
	l, err := s.st.EventsForOrder(ctx, orderID)
	if err != nil {
		return rail.OrderEvents{}, err
	}
	return rail.EventsOf(l), nil
}

// addEvent adds an event and runs the settle:after-event hook when it was added.
func (s *Service) addEvent(ctx context.Context, typ, orderID string, seq int) (bool, error) {
	added, err := s.st.AddEvent(ctx, store.Event{Type: typ, OrderID: orderID, Seq: seq})
	if err != nil {
		return false, err
	}
	if added {
		if err := s.at("settle:after-event"); err != nil {
			return true, err
		}
	}
	return added, nil
}

// paying is the Service as rail.PaidEvents writes through it.
type paying struct{ s *Service }

func (p paying) AddAccountPaid(ctx context.Context, orderID string) (bool, error) {
	return p.s.st.AddAccountPaid(ctx, orderID)
}
func (p paying) MarkKnown(account string) { p.s.ln.MarkKnown(account) }
func (p paying) AddEvent(ctx context.Context, typ, orderID string, seq int) (bool, error) {
	return p.s.addEvent(ctx, typ, orderID, seq)
}
func (p paying) At(point string) error      { return p.s.at(point) }
func (p paying) Alert(line, orderID string) { p.s.log.Error(line, "order", orderID) }

// paidEvents are rail.PaidEvents (so a customer who pays in Monero becomes known for the Lightning limits, as on the
// other rails), with the paid-again ALERT of the rail whose money makes the order paid: confirmed is this rail's sum.
func (s *Service) paidEvents(ctx context.Context, o store.Order, ev *rail.OrderEvents, confirmed int64, peerPaid bool) error {
	return rail.PaidEvents(ctx, paying{s}, o, ev, rail.PaidAgainLine(confirmed >= o.AmountPiconero, peerPaid, rail.LogPaidAgainMonero, rail.LogPaidAgainOnchain))
}

// createTransfers writes a settle's transfers as one batch (ledger.CreateTransfers, ruling S1) and returns each one's
// result: TransferCreated or TransferExists, the batch applied. The batch is its forwards, then its reversals from
// index nFwd. When a forward exists with other details nothing is applied: it logs logDifferent, adds the item's id to
// left and reports retry, so the caller plans again without it. When a reversal does, nothing is applied either: it
// logs logDifferent and returns an error (ruling TU1), never a retry without it, since the plan around it (its
// replacement's forward, the seq it carries) assumed it written. Any other refusal is an error. An empty batch writes
// nothing.
func (s *Service) createTransfers(ctx context.Context, o store.Order, batch []ledger.Transfer, nFwd int, left map[ledger.U128]bool) (
	results []ledger.CreateTransferResult, retry bool, err error) {
	if len(batch) == 0 {
		return nil, false, nil
	}
	results, err = s.lg.CreateTransfers(ctx, batch)
	if err != nil {
		return nil, false, err
	}
	for j, r := range results {
		switch {
		case r == ledger.TransferCreated, r == ledger.TransferExists, r == ledger.TransferLinkedEventFailed:
		case strings.HasPrefix(string(r), "exists_with_different_") && j >= nFwd:
			s.log.Error(logDifferent, "order", o.ID)
			return nil, false, errors.New("monero: a reversal exists with other details")
		case strings.HasPrefix(string(r), "exists_with_different_"):
			s.log.Error(logDifferent, "order", o.ID)
			left[batch[j].ID] = true
			return nil, true, nil
		default:
			return nil, false, fmt.Errorf("monero: the ledger refused a transfer: %s", r)
		}
	}
	return results, false, nil
}

// peerState reads the peer rail (the on-chain rail) for an order: rail.PeerFactsOf its view at the order's amount
// there. No peer reads nothing.
func (s *Service) peerState(ctx context.Context, o store.Order) (rail.PeerFacts, error) {
	p := s.peerRail()
	if p == nil {
		return rail.PeerFacts{}, nil
	}
	v, ok, err := p.View(ctx, o)
	if err != nil {
		return rail.PeerFacts{}, fmt.Errorf("monero: reading the on-chain rail: %w", err)
	}
	return rail.PeerFactsOf(v, ok, o.AmountSat), nil
}

// peerSeqs reads the seqs the peer rail's reversals of the order carry (rail.Peer.RevertedSeqs, ruling R2): the
// store and the ledger only, no RPC call, and not the settle mutex, which settle holds. No peer reads nothing.
func (s *Service) peerSeqs(ctx context.Context, o store.Order) ([]int, error) {
	p := s.peerRail()
	if p == nil {
		return nil, nil
	}
	seqs, err := p.RevertedSeqs(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("monero: reading the on-chain rail's reversals: %w", err)
	}
	return seqs, nil
}

// settle brings one order's ledger records and events up to date with its payments' marks: onchain's settle over
// rail's functions, without the Bitcoin-only parts (inheritance, the hold of a record that died after the scan, the
// mempool-loaded gate). It reads only the store, the ledger, lnpay's status and the peer's view, so a crash anywhere
// is completed by the next call. It runs on the pass's goroutine only. unknown holds the payments whose transaction
// the wallet, catching up, did not know in this pass (errNotKnown): their stored marks are older than this pass, so
// none of them is newly recorded here (ruling RU); a recorded one is kept or reversed on its marks as before.
//
// The whole call holds the settle mutex (ruling 8-A1), which the two watched rails share: the peer's paid state, read
// below, changes only in the peer's own settle, so it cannot change between that read and this call's events (a false
// order.paid_again, or two reversals each stamped 0 and no order.reverted). lnpay takes the same mutex to write its
// paid transfer, which is what its Status reads as paid: a Lightning payment is recorded either before a settle reads
// it or after that settle's reversals and events, never between. Lock order: the settle mutex is taken first, here, in
// Rail (ruling R1: a status read waits for the settle's end) and in lnpay's record of a payment, which holds no other
// lock when it takes it, and which no holder of the settle mutex calls (lnpay's hook into the rails, LightningPaid,
// takes no settle mutex); under it settle takes only leaf locks (mu, bothMu, the peer's mu and staleMu in its View,
// lnpay's mu in Status and MarkKnown), none of which is held while the settle mutex is taken, and it makes no RPC call. The peer is read through View and RevertedSeqs, which do not take
// the settle mutex (this call holds it) and make no RPC call.
func (s *Service) settle(ctx context.Context, o store.Order, unknown map[store.PaymentKey]bool) error {
	mu := s.settleLock()
	mu.Lock()
	defer mu.Unlock()
	return s.settleLocked(ctx, o, unknown)
}

// settleLocked is settle with the settle mutex already held by the caller: the pass holds it once across its
// generation check, its commit and all its settles (ruling S2). The on-chain rail's settle still takes it per call.
func (s *Service) settleLocked(ctx context.Context, o store.Order, unknown map[store.PaymentKey]bool) error {

	// 1. Read everything, then heal: every reversal that carries a seq has its order.reverted, so the count of those
	// events is complete before k is derived. The reversals are this rail's and the peer's (ruling R2): a reversal and
	// its event are written to two databases, so a crash or a store error between them on either rail is healed by
	// the next settle of the order on either rail, under this mutex, from the ledgers alone.
	now := s.now()
	ud, err := orderUserData(o.ID)
	if err != nil {
		return err
	}
	views, err := s.orderViews(ctx, o, true)
	if err != nil {
		return err
	}
	cur, _, err := s.st.MoneroCursor(ctx)
	if err != nil {
		return err
	}
	tip := railTip(cur.WalletHeight) // the wallet's height this pass read (ruling 8-A2), stored with the cursor
	ln := lnpay.Status{}
	if o.AmountMsat > 0 {
		if ln, err = s.ln.Status(ctx, o.ID); err != nil {
			return err
		}
	}
	if err := s.at("settle:after-status"); err != nil {
		return err
	}
	lnPaid := ln.State == lnpay.StatePaid
	peer, err := s.peerState(ctx, o)
	if err != nil {
		return err
	}
	otherPaid := lnPaid || peer.Paid
	peerSeqs, err := s.peerSeqs(ctx, o)
	if err != nil {
		return err
	}
	for _, k := range rail.HealSeqs(rail.RevertedSeqs(outs(views)), peerSeqs) {
		if _, err := s.addEvent(ctx, store.EventReverted, o.ID, k); err != nil {
			return err
		}
	}
	ev, err := s.readEvents(ctx, o.ID)
	if err != nil {
		return err
	}

	// 2. Paid before this call, on the combined state (Lightning, the peer and this rail).
	confirmed, _, _ := rail.Sums(outs(views), store.MaxPiconero)
	before := rail.IsPaid(otherPaid, confirmed, o.AmountPiconero)
	if before {
		if err := s.paidEvents(ctx, o, &ev, confirmed, peer.Paid); err != nil {
			return err
		}
	}

	// 3 and 4 are planned in memory and written as one batch (ruling S1: ledger.CreateTransfers, one transaction), so
	// no settle that ends early (an error, a cancellation, a crash) leaves a new forward without the reversal planned
	// beside it: Rail and the next settle never sum a replacement and the payment it replaces. The views are updated as
	// each transfer is planned, as before. A forward that exists with other details is logged (logDifferent) and left
	// out, and the rest is planned again from step 1's views and retried as a batch; a reversal that does fails the
	// settle with nothing written (ruling TU1: the plan around it assumed it); any other refusal is an error.
	//
	// 3. Record each live payment at its need (Recordable): mined at its tier, or in the pool on a 0 tier; a late or
	// locked one only once mined, with CodeLate. A payment the wallet did not know in this pass is not newly recorded
	// from its older mark (ruling RU): it is recorded on a later pass, once the wallet answers for it again.
	//
	// 4. Reverse each recorded payment that reverts: dead, or no longer mined when it needs a block. k is the next
	// order.reverted seq when these reversals turn a paid order unpaid, else 0; only the reversal that takes the
	// running sum below the amount carries it (rail.PlanReversals, rail.Stamps).
	need := int64(rail.Required(s.cfg.Tiers, o.AmountPiconero))
	start := slices.Clone(views)
	left := map[ledger.U128]bool{} // the transfers that exist with other details
	var batch []ledger.Transfer
	var overpays []bool // per forward of the batch: it overpays the order, judged when it was planned
	var afterForwards []rail.Out
	var results []ledger.CreateTransferResult
	var reversed []int // the views of the batch's reversals
	nFwd, k := 0, 0
	for {
		views = slices.Clone(start)
		batch, overpays, reversed = batch[:0], overpays[:0], reversed[:0]
		keep := rail.Keep(outs(views), need, store.MaxPiconero)
		for i := range views {
			v := &views[i]
			if v.Foreign || v.Recorded || !v.Recordable(need, tip) || unknown[store.PaymentKey{TxID: v.p.TxID, Address: v.p.Address}] {
				continue
			}
			t := book.Forward(PaidTransferID(v.p.TxID, v.index, v.next), ud, v.p.AmountPiconero, v.Counted)
			if left[t.ID] {
				continue
			}
			batch = append(batch, t)
			overpays = append(overpays, rail.Overpays(v.Out, keep, o.AmountPiconero, store.MaxPiconero))
			v.Recorded, v.epoch, v.Code, v.Timestamp = true, v.next, t.Code, t.Timestamp
			if v.Counted {
				keep = addPico(keep, v.p.AmountPiconero)
			}
		}
		nFwd, afterForwards = len(batch), outs(views)
		plan := rail.PlanReversals(outs(views), rail.PlanInput{TierNeed: need, Amount: o.AmountPiconero, Limit: store.MaxPiconero,
			Before: before, LightningPaid: otherPaid, Reverted: ev.Reverted})
		k = plan.K
		for j, i := range plan.Idx {
			v := &views[i]
			batch = append(batch, book.Reversal(ReversalTransferID(v.p.TxID, v.index, v.epoch), ud, v.p.AmountPiconero, plan.Stamps[j]))
			v.Recorded, v.next = false, v.epoch+1
			reversed = append(reversed, i)
		}
		var retry bool
		if results, retry, err = s.createTransfers(ctx, o, batch, nFwd, left); err != nil {
			return err
		}
		if !retry {
			break
		}
	}
	// The batch is written: each reversal in it now exists, so its view reads reversed (ruling T3), and step 5's
	// historical checks (rail.LateRecorded) count a late forward this call reversed, as the next call would from the
	// ledger. Without it a recovery that reverses a late forward whose order.late a crash left unwritten clears the
	// pending row without the event, and past the window nothing settles the order again.
	for _, i := range reversed {
		views[i].Reversed = true
	}
	if len(batch) > 0 {
		if err := s.at("settle:after-transfers"); err != nil {
			return err
		}
	}
	createdAny := false
	for j := 0; j < nFwd; j++ {
		if results[j] == ledger.TransferCreated {
			createdAny = true
			if overpays[j] {
				s.log.Error(logOverpaid, "order", o.ID)
			}
		}
	}
	// The both-rails rule, one statement: a Monero record (rail.BothRecorded) beside money on another rail needs a
	// person (6.1-W): Lightning paid, or the peer paid or holding money, counted or late. Deduplicated per process by
	// alertBoth, level-triggered while the payment is read here, and also given by LightningPaid when Lightning pays
	// second. Each rail writes its record, then reads the other's, so at least one sees both: a transfer created here
	// is followed by a re-read of Lightning and of the peer. The records are those standing after step 3's forwards
	// (afterForwards), before step 4's reversals, as when the two steps were written one transfer at a time.
	if rail.BothRails(afterForwards, lnPaid, peer) {
		s.alertBoth(o.ID)
	}
	if createdAny && !rail.MoneyElsewhere(lnPaid, peer) {
		lnAgain := false
		if o.AmountMsat > 0 {
			again, err := s.ln.Status(ctx, o.ID)
			if err != nil {
				return err
			}
			lnAgain = again.State == lnpay.StatePaid
		}
		peerAgain, err := s.peerState(ctx, o)
		if err != nil {
			return err
		}
		if rail.BothRails(afterForwards, lnAgain, peerAgain) {
			s.alertBoth(o.ID)
		}
	}
	for j := nFwd; j < len(batch); j++ {
		if results[j] == ledger.TransferCreated {
			s.log.Error(logReorg, "order", o.ID)
		}
	}
	// The confirmed sum after the reversals is summed again over the records that stand, never the capped sum less
	// the reversals (ruling 8-A5).
	confirmed, _, _ = rail.Sums(outs(views), store.MaxPiconero)

	// 5. The events, in the on-chain order.
	if k > 0 {
		if _, err := s.addEvent(ctx, store.EventReverted, o.ID, k); err != nil {
			return err
		}
		ev.Reverted = k
	}
	_, countedLive, _ := rail.Sums(outs(views), store.MaxPiconero)
	if rail.IsPaid(otherPaid, confirmed, o.AmountPiconero) {
		if err := s.paidEvents(ctx, o, &ev, confirmed, peer.Paid); err != nil {
			return err
		}
	}
	if rail.LateRecorded(outs(views)) {
		added, err := s.addEvent(ctx, store.EventLate, o.ID, 1)
		if added {
			s.log.Error(logLate, "order", o.ID)
		}
		if err != nil {
			return err
		}
	}
	// The final event (rail.Final), past the expiry, once Lightning is done and no peer is still being paid. An
	// AddEvent answering false (the peer stored the same event first) is no error.
	typ, window, due := rail.Final(rail.FinalInput{Now: now, ExpiresAt: o.ExpiresAt, CountedLive: countedLive,
		Amount: o.AmountPiconero, HasFinal: ev.Final, LightningDone: rail.LightningDone(o, ln), PeerPending: peer.Pending,
		PeerReceived: peer.Received, PeerStale: peer.Stale})
	if due {
		line := logUnderpaid
		if window {
			line = logUnderpaidAtWindow
		}
		added, err := s.addEvent(ctx, typ, o.ID, 1)
		if added && typ == store.EventUnderpaid {
			s.log.Error(line, "order", o.ID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// LightningPaid is lnpay's OnLightningPaid hook for an order with a Monero amount: lnpay completed a Lightning
// payment of o. If the order has a Monero record (rail.BothRecorded: a payment of any amount, counted or late), the
// both-rails ALERT is logged, once per order per process. It covers a Monero payment settled deep, which the watcher
// no longer reads. lnpay calls it concurrently with the pass; an error is returned to lnpay, which completes the
// payment again on its next lookup.
func (s *Service) LightningPaid(ctx context.Context, o store.Order) error {
	if o.AmountPiconero == 0 {
		return nil
	}
	views, err := s.orderViews(ctx, o, false)
	if err != nil {
		return err
	}
	if rail.BothRecorded(outs(views)) {
		s.alertBoth(o.ID)
	}
	return nil
}

// alertBoth logs logBothRails for the order once per process. The pass (settle) and lnpay's hook (LightningPaid) call
// it from different goroutines; bothMu makes the check and the mark one step.
func (s *Service) alertBoth(orderID string) {
	s.bothMu.Lock()
	defer s.bothMu.Unlock()
	if _, seen := s.bothAlerted[orderID]; seen {
		return
	}
	if len(s.bothAlerted) >= maxAlerted {
		clear(s.bothAlerted)
	}
	s.bothAlerted[orderID] = struct{}{}
	s.log.Error(logBothRails, "order", orderID)
}
