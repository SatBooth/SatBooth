package onchain

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
	logReorg             = "ALERT: an on-chain payment was reverted by a reorg"
	logLate              = "ALERT: a late on-chain payment arrived after the order's expiry"
	logUnderpaid         = "ALERT: an order was underpaid at expiry"
	logUnderpaidAtWindow = "ALERT: an order was underpaid at the end of its watch window; its payment is not yet confirmed enough and stays tracked"
	logPaidAfterFinal    = rail.LogPaidAfterFinal
)

// outView is what the store and the ledger say about one output of an order: the rail's Out (filled by outputView and
// orderViews, whose methods are the rules both rails share), and the output's key and epochs in the ledger.
type outView struct {
	rail.Out
	x     store.TXO
	epoch uint32 // the recorded forward's epoch
	next  uint32 // the epoch of the next forward, when not recorded
}

// outs are the views' Outs, for the rail's rules over an order.
func outs(views []outView) []rail.Out {
	r := make([]rail.Out, len(views))
	for i, v := range views {
		r[i] = v.Out
	}
	return r
}

func addSat(a, b int64) int64 { return rail.Add(a, b, store.MaxSat) }

// book is the rail's two accounts in ledger 2 (rail.Book): the forward and reversal transfers and their checks.
var book = rail.Book{Ledger: ledgerID, Wallet: OnchainWallet, Merchant: MerchantOnchain}

// outputView walks the output's epochs in the ledger (rail.Book.Walk, with the output's transfer ids): from epoch 0,
// each forward and its reversal, until a forward is missing or one has no reversal. alert logs logDifferent for a
// transfer that does not match.
func (s *Service) outputView(ctx context.Context, o store.Order, x store.TXO, alert bool) (outView, error) {
	ud, err := orderUserData(o.ID)
	if err != nil {
		return outView{}, err
	}
	v := outView{x: x}
	v.Amount, v.Counted = x.AmountSat, x.FirstSeen.Before(o.ExpiresAt)
	ids := func(ep uint32) (ledger.U128, ledger.U128) {
		return PaidTransferID(x.TxID, x.Vout, ep), ReversalTransferID(x.TxID, x.Vout, ep)
	}
	if v.epoch, v.next, err = book.Walk(ctx, s.lg, ids, ud, &v.Out, maxEpochs); err != nil {
		return outView{}, err
	}
	if v.Foreign && alert {
		s.log.Error(logDifferent, "order", o.ID)
	}
	return v, nil
}

// orderViews reads the order's outputs, their current marks and their ledger records, with the confirmation bound
// applied (boundOut): every reader of the views, settle, Rail and LightningPaid, sees the same counts.
func (s *Service) orderViews(ctx context.Context, o store.Order, alert bool) ([]outView, error) {
	kept, _, err := s.views(ctx, o, alert, false)
	return kept, err
}

// views is orderViews; inPass is true only for the pass's own settle. Then an output that was behind in this pass
// (errBehind: the node says its payment was replaced, the wallet has not processed that) and is not recorded is left
// out: it counts zero in every decision of the pass, whatever its mark says, because the node contradicts the mark. A
// recorded one is kept: its record stands (its mark is not dead, so nothing reverses it). The views of Rail and the
// other readers are not changed, and they never read behindInPass (only the pass's goroutine does, here, with inPass).
// all is every output's view, the left-out ones included: the heal step reads it (a reversal's lost event is healed
// whether or not its output is behind).
func (s *Service) views(ctx context.Context, o store.Order, alert, inPass bool) (kept, all []outView, err error) {
	txos, err := s.st.TXOsForOrder(ctx, o.ID)
	if err != nil {
		return nil, nil, err
	}
	keys := make([]store.TXOKey, len(txos))
	for i, x := range txos {
		keys[i] = store.TXOKey{TxID: x.TxID, Vout: x.Vout}
	}
	marks, err := s.st.CurrentMarks(ctx, keys)
	if err != nil {
		return nil, nil, err
	}
	seen := s.seenNow()
	kept, all = make([]outView, 0, len(txos)), make([]outView, 0, len(txos))
	for i, x := range txos {
		v, err := s.outputView(ctx, o, x, alert)
		if err != nil {
			return nil, nil, err
		}
		m, ok := marks[keys[i]]
		v.Marked, v.Kind, v.Height = ok, m.Kind, m.Height
		obs, have := seen[keys[i]]
		boundOut(&v.Out, obs, have, m.BlockHash) // the one place the bound is applied
		all = append(all, v)
		if inPass { // only the pass's goroutine reads behindInPass
			if _, behind := s.behindInPass[keys[i]]; behind && !v.Recorded {
				continue // the one place the behind rule is applied: it counts zero (behindInPass)
			}
		}
		kept = append(kept, v)
	}
	return kept, all, nil
}

// readEvents reads the order's events and folds them (rail.EventsOf).
func (s *Service) readEvents(ctx context.Context, orderID string) (rail.OrderEvents, error) {
	l, err := s.eventsFor(ctx, orderID)
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

// paidEvents are rail.PaidEvents with the paid-again ALERT of the rail whose money makes the order paid: confirmed is
// this rail's sum, peerPaid the peer's paid.
func (s *Service) paidEvents(ctx context.Context, o store.Order, ev *rail.OrderEvents, confirmed int64, peerPaid bool) error {
	return rail.PaidEvents(ctx, paying{s}, o, ev, rail.PaidAgainLine(confirmed >= o.AmountSat, peerPaid, rail.LogPaidAgainOnchain, rail.LogPaidAgainMonero))
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
			return nil, false, errors.New("onchain: a reversal exists with other details")
		case strings.HasPrefix(string(r), "exists_with_different_"):
			s.log.Error(logDifferent, "order", o.ID)
			left[batch[j].ID] = true
			return nil, true, nil
		default:
			return nil, false, fmt.Errorf("onchain: the ledger refused a transfer: %s", r)
		}
	}
	return results, false, nil
}

// peerState reads the peer rail (the Monero rail) for an order: rail.PeerFactsOf its view at the order's amount
// there. No peer reads nothing.
func (s *Service) peerState(ctx context.Context, o store.Order) (rail.PeerFacts, error) {
	p := s.peerRail()
	if p == nil {
		return rail.PeerFacts{}, nil
	}
	v, ok, err := p.View(ctx, o)
	if err != nil {
		return rail.PeerFacts{}, fmt.Errorf("onchain: reading the Monero rail: %w", err)
	}
	return rail.PeerFactsOf(v, ok, o.AmountPiconero), nil
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
		return nil, fmt.Errorf("onchain: reading the Monero rail's reversals: %w", err)
	}
	return seqs, nil
}

// settle brings one order's ledger records and events up to date with its outputs' marks. It reads only the store, the
// ledger, lnpay's status and the peer's view, so a crash anywhere is completed by the next call. It runs on the pass's
// goroutine only. The peer (the Monero rail, when set) counts as Lightning does: paid there makes the order paid, its
// money joins the both-rails rule, and while it is being paid it holds the final event.
//
// The whole call holds the settle mutex (ruling 8-A1), which the two watched rails share: the peer's paid state, read
// below, changes only in the peer's own settle, so it cannot change between that read and this call's events (a false
// order.paid_again, or two reversals each stamped 0 and no order.reverted). lnpay takes the same mutex to write its
// paid transfer, which is what its Status reads as paid: a Lightning payment is recorded either before a settle reads
// it or after that settle's reversals and events, never between. Lock order: the settle mutex is taken first, here, in
// Rail (ruling R1: a status read waits for the settle's end) and in lnpay's record of a payment, which holds no other
// lock when it takes it, and which no holder of the settle mutex calls (lnpay's hook into the rails, LightningPaid,
// takes no settle mutex); under it settle takes only leaf locks, none of which is held while the settle mutex is
// taken, and none of which is held while another lock is taken
// except bothMu, which alertBoth holds across its log line (the log handler's own lock is a leaf too): seenMu (held
// only to copy the observations' map pointer, in seenNow, called by orderViews here and in the peer's View of this
// rail), mu (peerRail), bothMu, the peer's mu and staleMu in its View, and lnpay's mu in Status and
// MarkKnown. Rail takes the settle mutex, then staleMu.
// settle makes no RPC call. The peer is read through View and RevertedSeqs, which do not take the settle mutex (this
// call holds it) and make no RPC call. work, LightningPaid and confirmedSum take no settle mutex: they take seenMu
// alone (through orderViews), as the publish in trackAll does.
func (s *Service) settle(ctx context.Context, o store.Order) error {
	mu := s.settleLock()
	mu.Lock()
	defer mu.Unlock()

	// 1. Read everything, then heal: every reversal that carries a seq has its order.reverted, so the count of those
	// events is complete before k is derived. The reversals are this rail's and the peer's (ruling R2): a reversal and
	// its event are written to two databases, so a crash or a store error between them on either rail is healed by
	// the next settle of the order on either rail, under this mutex, from the ledgers alone.
	now := s.now()
	ud, err := orderUserData(o.ID)
	if err != nil {
		return err
	}
	views, allViews, err := s.views(ctx, o, true, true)
	if err != nil {
		return err
	}
	cur, _, err := s.st.Cursor(ctx)
	if err != nil {
		return err
	}
	// tip is the node's tip at the pass's step 1. The chain may have moved since (a reorg can lower the tip), so the
	// views' confirmations are at most the count of the gettransaction answer that gave their mark, and a mined output
	// that is not recorded and has no such answer counts zero: orderViews applied that (boundOut).
	tip := cur.Height
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
	for _, k := range rail.HealSeqs(rail.RevertedSeqs(outs(allViews)), peerSeqs) {
		if _, err := s.addEvent(ctx, store.EventReverted, o.ID, k); err != nil {
			return err
		}
	}
	ev, err := s.readEvents(ctx, o.ID)
	if err != nil {
		return err
	}

	// 2. Paid before this call, on the combined state (Ruling K). The paid rules are level-triggered: a crash between
	// a record and its event is completed here, before a reversal could follow it.
	confirmed, _, _ := rail.Sums(outs(views), store.MaxSat)
	before := rail.IsPaid(otherPaid, confirmed, o.AmountSat)
	if before {
		if err := s.paidEvents(ctx, o, &ev, confirmed, peer.Paid); err != nil {
			return err
		}
	}

	// 3 and 4 are planned in memory and written as one batch (ruling S1: ledger.CreateTransfers, one transaction), so
	// no settle that ends early (an error, a cancellation, a crash) leaves a new forward without the reversal planned
	// beside it: Rail and the next settle never sum a replacement and the output it replaces. The views are updated as
	// each transfer is planned, as before. A forward that exists with other details is logged (logDifferent) and left
	// out, and the rest is planned again from step 1's views and retried as a batch; a reversal that does fails the
	// settle with nothing written (ruling TU1: the plan around it assumed it); any other refusal is an error.
	//
	// 3. Record each live output at its need (Recordable): mined at its tier, or in the mempool on a 0 tier. The
	// overpaid check counts only the records step 4 keeps (rail.Keep): a 0-conf fee bump that keeps the output records
	// the new output in the same call that reverses the old one, and that is no overpayment.
	//
	// 4. Reverse each recorded output that reverts: dead, or no longer mined when it needs a block. k is the next
	// order.reverted seq when these reversals turn a paid order unpaid, else 0 (rail.ReversalSeq): a reversal that
	// leaves the order paid gives no event. Only the reversal that takes the running sum below the amount carries k,
	// the others 0 (rail.Stamps). The batch is written whole or not at all, so the ledger never holds a part of them.
	//
	// The hold of the final event after a dead mark extends to reversals. On a 0 tier a fee bump that lands between the
	// pass's listsinceblock and the original's gettransaction kills the original before its replacement can be listed;
	// reversing the record now would turn the order unpaid, with a false order.reverted and its reorg ALERT, and
	// order.paid_again next pass. So when these reversals would turn the paid order unpaid, the 0-tier records whose
	// outputs this pass marked dead after its scan (diedNow) are held: not reversed in this pass. The next pass reverses
	// them, after its scan has listed and step 3 has recorded any replacement that inherits, so the rule below handles it
	// as a same-pass bump (no event); a real double spend or eviction is reversed then, one pass late. While held, the
	// record stands and the order reads paid by its ledger and its events alike. A reversal that leaves the order paid (a
	// replacement recorded in this call: the same-pass bump) is not held. A record that needs a block is never held: its
	// replacement could not be recorded from the mempool either. The hold is in memory and lost with the process, which is
	// safe: the restarted pass's listsinceblock comes after the dead mark, so its step 3 records any replacement before
	// this step reverses the original. rail.PlanReversals applies the rule; which records are held (diedNow: an RBF bump
	// between listsinceblock and gettransaction) is this rail's own.
	need := int64(Required(s.cfg.Tiers, o.AmountSat))
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
		keep := rail.Keep(outs(views), need, store.MaxSat)
		for i := range views {
			v := &views[i]
			if v.Foreign || v.Recorded || !v.Recordable(need, tip) {
				continue
			}
			t := book.Forward(PaidTransferID(v.x.TxID, v.x.Vout, v.next), ud, v.x.AmountSat, v.Counted)
			if left[t.ID] {
				continue
			}
			batch = append(batch, t)
			overpays = append(overpays, rail.Overpays(v.Out, keep, o.AmountSat, store.MaxSat))
			v.Recorded, v.epoch, v.Code, v.Timestamp = true, v.next, t.Code, t.Timestamp
			if v.Counted {
				keep = addSat(keep, v.x.AmountSat)
			}
		}
		nFwd, afterForwards = len(batch), outs(views)
		plan := rail.PlanReversals(outs(views), rail.PlanInput{TierNeed: need, Amount: o.AmountSat, Limit: store.MaxSat,
			Before: before, LightningPaid: otherPaid, Reverted: ev.Reverted, Held: func(i int) bool { return s.diedNow(views[i]) }})
		k = plan.K
		for j, i := range plan.Idx {
			v := &views[i]
			batch = append(batch, book.Reversal(ReversalTransferID(v.x.TxID, v.x.Vout, v.epoch), ud, v.x.AmountSat, plan.Stamps[j]))
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
	// The both-rails ALERT (rail.BothRecorded, the rule LightningPaid uses too), deduplicated per process by alertBoth.
	// Level-triggered: while the order is settled, every call alerts if Lightning reads paid and an on-chain record
	// exists, whichever came first, so a call that failed or crashed before its alert is healed by the next one.
	// lnpay's completion (LightningPaid) alerts when Lightning pays second, also once the output is settled deep and
	// no longer read here. Each rail writes its record, then reads the other's, so at least one sees both: the peer
	// may have recorded its payment after step 1's read (and so may Lightning when it is not wired to the settle lock, as
	// in tests, which then runs LightningPaid before this transfer exists), so a transfer created here is followed by
	// one more Status read. It covers a record created and settled in the same
	// call (no later call reads it). The read is local: steps 4 and 5 keep step 1's view, as step 2 used it.
	// The peer joins the rule as Lightning does, with its money too (6.1-W: money on two rails needs a person), and is
	// re-read after a creation as Lightning is.
	// The records read are those standing after step 3's forwards (afterForwards), before step 4's reversals, as when
	// the two steps were written one transfer at a time.
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
	confirmed, _, _ = rail.Sums(outs(views), store.MaxSat)

	// 5. The events.
	if k > 0 {
		if _, err := s.addEvent(ctx, store.EventReverted, o.ID, k); err != nil {
			return err
		}
		ev.Reverted = k
	}
	_, countedLive, _ := rail.Sums(outs(views), store.MaxSat)
	if rail.IsPaid(otherPaid, confirmed, o.AmountSat) {
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
	// The final event (rail.Final), past the expiry and once Lightning is done: below the amount, order.expired
	// (nothing live) or order.underpaid; at the window end, an on-time payment that is live but not yet confirmed
	// enough to be recorded is order.underpaid too. Its output stays tracked (store.WatchedTXOs), so a later
	// confirmation still records it (Ruling S: order.paid after the final event).
	// No final event while an on-time output of the order was marked dead in this pass, after its listsinceblock: its
	// replacement may be listed only by the next pass (a fee bump after the expiry inherits the first-seen, plan 6.1),
	// and the order is being paid then. The next pass writes the event if nothing replaced the payment.
	typ, window, due := rail.Final(rail.FinalInput{Now: now, ExpiresAt: o.ExpiresAt, CountedLive: countedLive,
		Amount: o.AmountSat, HasFinal: ev.Final, LightningDone: LightningDone(o, ln), PeerPending: peer.Pending,
		PeerReceived: peer.Received, PeerStale: peer.Stale})
	if _, behind := s.heldInPass[o.ID]; due && (behind || s.diedAfterScan(views)) {
		// behind: step 3 left an output of the order unjudged because the wallet lags (it counts zero in these views)
		s.heldInPass[o.ID] = struct{}{} // leftWithoutFinal waits one pass for it
	} else if due {
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

// diedAfterScan reports whether an on-time output among views was marked dead by the current pass's step 3, after
// the pass's listsinceblock (Service.diedInPass).
func (s *Service) diedAfterScan(views []outView) bool {
	return slices.ContainsFunc(views, s.diedNow)
}

// diedNow is diedAfterScan for one output: it is on time and the current pass's step 3 marked it dead.
func (s *Service) diedNow(v outView) bool {
	_, ok := s.diedInPass[store.TXOKey{TxID: v.x.TxID, Vout: v.x.Vout}]
	return ok && v.Counted
}

// LightningDone reports whether an order's Lightning side can no longer pay it (rail.LightningDone).
func LightningDone(o store.Order, ln lnpay.Status) bool { return rail.LightningDone(o, ln) }

// LightningPaid is lnpay's OnLightningPaid hook (service.Run sets it): lnpay completed a Lightning payment of the mixed
// order o. If the order has an on-chain record (rail.BothRecorded: a payment of any amount, counted or late), the
// both-rails ALERT is logged, once per order per process; an order with none (nothing paid, or an output not yet
// confirmed enough to be recorded) logs nothing. It covers an on-chain output settled deep, which the watcher no
// longer reads. lnpay calls it from the reconciler, the notification listener and Pay's flights, concurrently with the
// pass; an error is returned to lnpay, which completes the payment again on its next lookup.
func (s *Service) LightningPaid(ctx context.Context, o store.Order) error {
	views, err := s.orderViews(ctx, o, false)
	if err != nil {
		return err
	}
	if rail.BothRecorded(outs(views)) {
		s.alertBoth(o.ID)
	}
	return nil
}

// maxBothAlerted bounds bothAlerted; when full it is cleared (the worst case is one more ALERT per order).
const maxBothAlerted = 4096

// alertBoth logs logBothRails for the order once per process. The pass (settle) and lnpay's hook (LightningPaid) call
// it from different goroutines; bothMu makes the check and the mark one step.
func (s *Service) alertBoth(orderID string) {
	s.bothMu.Lock()
	defer s.bothMu.Unlock()
	if _, seen := s.bothAlerted[orderID]; seen {
		return
	}
	if len(s.bothAlerted) >= maxBothAlerted {
		clear(s.bothAlerted)
	}
	s.bothAlerted[orderID] = struct{}{}
	s.log.Error(logBothRails, "order", orderID)
}
