package lnpay

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/store"
)

// HealWindow bounds the startup scan for closed invoices that are paid in the ledger but whose order
// has no order.paid: a crash older than that is the threat model's residual.
const HealWindow = 7 * 24 * time.Hour

// cursor is the reconciler's position in its round robin over the open invoices.
type cursor struct {
	snap      []store.Invoice
	idx       int
	worked    bool      // something was looked up in this round
	idleUntil time.Time // real time: no new round before this, after a round with nothing to look up
}

// Run runs the reconciler until ctx ends (it then returns ctx.Err()). It looks up every open
// invoice in turn, PollRate lookups a second in total, so its load does not depend on traffic. A
// notification for one of our invoices moves that invoice to the front of the queue; it is not
// believed, the lookup is. The first step runs at once. After a round in which nothing was looked
// up (no open invoices, or every entry skipped because it is fully recorded or stale and polled
// recently), the next round and its expiry sweep wait at least 10 s, so an idle service does not
// query the store on every tick; notifications still jump the queue.
func (s *Service) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer s.running.Store(false)
	lctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.listen(lctx) }()
	defer func() { cancel(); wg.Wait() }()

	s.healPaid(ctx)
	tick := time.NewTicker(time.Duration(float64(time.Second) / s.cfg.PollRate))
	defer tick.Stop()
	var c cursor
	for {
		if c.idx >= len(c.snap) && !time.Now().Before(c.idleUntil) { // a new round: expire the orders that are due
			if err := s.sweep(ctx); err != nil && ctx.Err() == nil {
				s.log.Error(logStepFailed)
			}
		}
		inv, ok, err := s.nextInvoice(ctx, &c)
		if err == nil && ok {
			var looked bool
			looked, err = s.reconcileInv(ctx, inv)
			s.afterStep(&c, looked)
		}
		if err != nil && ctx.Err() == nil {
			s.log.Error(logStepFailed)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// listen turns notifications into front-of-queue hints.
func (s *Service) listen(ctx context.Context) {
	ch := s.w.Notifications()
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-ch:
			if !ok {
				return
			}
			if n.Type == nwc.NotifPaymentReceived {
				s.prioritise(n.Tx.PaymentHash)
			}
		}
	}
}

// prioritise moves one of our open or watched invoices to the front. Anything else is ignored.
func (s *Service) prioritise(hash string) {
	hash = strings.ToLower(hash)
	if !validHash(hash) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Contains(s.front, hash) {
		return
	}
	if o, ours := s.open[hash]; ours {
		s.front = append(s.front, hash)
		if o.stale { // a notification is looked up at once, not at the slow poll
			o.polled = time.Time{}
			s.open[hash] = o
		}
	} else if w, ours := s.watch[hash]; ours {
		s.front = append(s.front, hash)
		w.polled = time.Time{}
		s.watch[hash] = w
	}
}

// nextInvoice returns the next invoice to look up: first those a notification moved to the front,
// then the round robin over a snapshot of the open invoices, refreshed when it is used up (and not
// before idleUntil, if the last snapshot was empty).
func (s *Service) nextInvoice(ctx context.Context, c *cursor) (store.Invoice, bool, error) {
	for {
		s.mu.Lock()
		if len(s.front) == 0 {
			s.mu.Unlock()
			break
		}
		h := s.front[0]
		s.front = s.front[1:]
		o, ok := s.open[h]
		w, watched := s.watch[h]
		s.mu.Unlock()
		if ok {
			return o.inv, true, nil
		}
		if watched {
			return w.inv, true, nil
		}
	}
	if c.idx >= len(c.snap) {
		if time.Now().Before(c.idleUntil) {
			return store.Invoice{}, false, nil
		}
		snap, err := s.snapshot(ctx)
		if err != nil {
			return store.Invoice{}, false, err
		}
		c.snap, c.idx, c.worked = snap, 0, false
		if len(snap) == 0 {
			c.idleUntil = time.Now().Add(s.idle)
		}
	}
	if c.idx >= len(c.snap) {
		return store.Invoice{}, false, nil
	}
	inv := c.snap[c.idx]
	c.idx++
	return inv, true, nil
}

// snapshot reads the open invoices for a new round and adopts those the service does not track,
// then adds the watched closed invoices that are due (their slow poll has come round, or their
// deadline has passed), most overdue first, at most max(open invoices, watchFloor) of them. A watched
// invoice that is not due costs a round nothing. The due ones share the lookup rate with the open
// invoices, under that cap; the due ones left out are the most overdue of the next round. Past about
// 600 × PollRate watched invoices (3,000 at the default 5/s) every round carries the cap, and a
// watched invoice is looked up less often than every 10 minutes.
func (s *Service) snapshot(ctx context.Context) ([]store.Invoice, error) {
	invs, err := s.st.OpenInvoices(ctx)
	if err != nil {
		return nil, err
	}
	for _, inv := range invs {
		if err := s.adopt(ctx, inv); err != nil {
			return nil, err
		}
	}
	now := s.now()
	s.mu.Lock()
	var due []watchInv
	for _, w := range s.watch {
		if now.Sub(w.polled) >= staleEvery || !now.Before(s.deadline(w.inv)) {
			due = append(due, w)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(due, func(a, b watchInv) int {
		if c := a.polled.Compare(b.polled); c != 0 {
			return c
		}
		if c := a.inv.ExpiresAt.Compare(b.inv.ExpiresAt); c != 0 {
			return c
		}
		return strings.Compare(a.inv.PaymentHash, b.inv.PaymentHash)
	})
	if limit := max(len(invs), watchFloor); len(due) > limit {
		due = due[:limit]
	}
	for _, w := range due {
		invs = append(invs, w.inv)
	}
	return invs, nil
}

// afterStep ends a round that did no lookup (every entry skipped) with the idle wait.
func (s *Service) afterStep(c *cursor, looked bool) {
	c.worked = c.worked || looked
	if c.idx >= len(c.snap) && !c.worked {
		c.idleUntil = time.Now().Add(s.idle)
	}
}

// pass runs one full round synchronously, without the rate limit: every open invoice once, then the
// expiry sweep. Tests drive the reconciler with it.
func (s *Service) pass(ctx context.Context) error {
	invs, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	for _, inv := range invs {
		if err := s.reconcile(ctx, inv); err != nil {
			return err
		}
	}
	return s.sweep(ctx)
}

// errOnPaid wraps an error of the OnLightningPaid hook. completePaid has added order.paid by then, so Pay answers
// ErrPaid for it; the reconciler returns it, and its next lookup completes the payment again, hook included.
var errOnPaid = errors.New("lnpay: the Lightning-paid hook failed")

// notFound: the wallet itself answered NOT_FOUND. It is the only wallet answer that is evidence the
// invoice does not exist, so the only one that closes an invoice at the deadline. SatBooth's
// connections are not isolated apps, so the hub's NOT_FOUND for an isolated app's re-paired invoices
// (transactions_service.go:612-628) does not apply. Every other answer is no evidence that the
// invoice is unpaid and is treated like a transport failure: INTERNAL (the hub's answer to a plain
// database error, map_nip47_error.go), RESTRICTED or EXPIRED (a narrowed or expired connection,
// permissions.go), a malformed message, a row for another hash or type.
func notFound(err error) bool {
	var we *nwc.WalletError
	return errors.As(err, &we) && we.Code == "NOT_FOUND"
}

// reconcile looks one open invoice up and acts on the answer.
//
// An invoice is closed only on evidence: a non-settled row from a lookup made after ExpiresAt plus
// the grace period (CloseLookup), or, 24 h after max(ExpiresAt, the service's start), a wallet answer
// of NOT_FOUND (CloseDeadline). Nothing else closes it, however long it lasts: a transport failure,
// any other wallet error, a malformed answer or a row for another hash or type is no evidence that it
// is unpaid. Past the deadline such an invoice stops counting against MaxOpen, is polled every 10
// minutes (at once if a notification names it) with an alert line, and stays open, so its order
// keeps reading "new".
func (s *Service) reconcile(ctx context.Context, inv store.Invoice) error {
	_, err := s.reconcileInv(ctx, inv)
	return err
}

// reconcileInv is reconcile, also reporting whether it did any work (false: the invoice was skipped).
func (s *Service) reconcileInv(ctx context.Context, inv store.Invoice) (bool, error) {
	start := s.now()
	s.mu.Lock()
	o, tracked := s.open[inv.PaymentHash]
	w, watched := s.watch[inv.PaymentHash]
	s.mu.Unlock()
	if watched {
		return s.reconcileWatched(ctx, w)
	}
	if !tracked {
		// Neither open in memory nor watched: it may have been closed since the snapshot was taken
		// (for example unwatched at its deadline through a notification). A closed invoice is never
		// looked up as an open one.
		closed, err := s.closedSet(ctx, inv.OrderID)
		if err != nil {
			return true, err
		}
		if closed[inv.PaymentHash] {
			return false, nil
		}
	}
	if tracked && (o.done && start.Before(inv.ExpiresAt) || o.stale && start.Sub(o.polled) < staleEvery) {
		return false, nil // fully recorded and waiting for its expiry, or a stale invoice polled recently
	}
	// A payment already in the ledger needs no lookup: complete what a crash may have left undone.
	if _, found, err := s.paidTransfer(ctx, inv.PaymentHash); err != nil {
		return true, err
	} else if found {
		return true, s.completePaid(ctx, inv, start)
	}
	lctx, cancel := context.WithTimeout(ctx, lookupBound)
	tx, err := s.w.LookupInvoice(lctx, inv.PaymentHash)
	cancel()
	if err := s.crash(crashAfterLookup); err != nil {
		return true, err
	}
	evidence := false // the wallet said NOT_FOUND
	switch {
	case err != nil:
		if ctx.Err() != nil {
			return true, ctx.Err() // shutting down: say nothing about the invoice
		}
		evidence = notFound(err)
		s.log.Info(logLookupFailed)
	case strings.ToLower(tx.PaymentHash) != inv.PaymentHash || tx.Type != nwc.TypeIncoming:
		s.log.Error(logBadLookup)
	case tx.State == nwc.StateSettled:
		if tx.AmountMsat >= inv.AmountMsat {
			return true, s.recordAndComplete(ctx, inv, tx.AmountMsat, start)
		}
		// Never seen from the hub 921003b, whose settled row keeps the invoice's amount
		// (transactions_service.go markTransactionSettled updates state, preimage and fees only), so
		// this guards another wallet: not recorded, reported once, and not closed as "unpaid" by
		// the lookup rule. It leaves the invoice open (stale, past the deadline).
		s.alertShort(inv)
	default:
		// Not settled. A lookup made after the invoice's expiry plus the grace period closes it.
		if !start.Before(inv.ExpiresAt.Add(s.cfg.Grace)) {
			return true, s.closeInvoice(ctx, inv, start, store.CloseLookup)
		}
		return true, nil
	}
	if start.Before(s.deadline(inv)) {
		// Before the stale deadline, which a restart moved to 24 h after the start: no stale line and
		// no slow polling yet, but past its own deadline the invoice holds no slot (classify).
		if !start.Before(inv.ExpiresAt.Add(store.DeadlineGrace)) {
			s.release(inv.PaymentHash, false, false, start)
		}
		return true, nil
	}
	if !evidence {
		s.release(inv.PaymentHash, true, false, start)
		s.log.Error(logStale, "order", inv.OrderID)
		return true, nil
	}
	s.log.Error(logDeadline)
	return true, s.closeInvoice(ctx, inv, start, store.CloseDeadline)
}

// reconcileWatched looks up a closed invoice that is watched for a late settlement (design.md, "Late settlement"): the
// wallet can accept a payment for a while after an invoice's expiry (LDK: up to about 2 h, inbound_payment.rs), after
// the closing lookup said not settled and the order read expired. Every staleEvery (at once on a notification) until
// its deadline, then never again, without a line (a payment the ledger already holds is completed first). A settlement
// for the full amount is recorded through the normal paid path (one transfer, order.paid, the account fact) with an
// alert line, and the order reads paid; a short one or a bad row alerts as for an open invoice; an error changes
// nothing (it is closed already). A watched invoice holds no MaxOpen slot and does not stop a new invoice for its
// order.
func (s *Service) reconcileWatched(ctx context.Context, w watchInv) (bool, error) {
	inv := w.inv
	start := s.now()
	if !start.Before(s.deadline(inv)) {
		// The drop reads the ledger first: a late payment whose transfer was written but whose
		// completion failed (a returned error) is completed now rather than dropped without its
		// order.paid and account fact. Kept watched until that succeeds.
		if _, found, err := s.paidTransfer(ctx, inv.PaymentHash); err != nil {
			return true, err
		} else if found {
			if err := s.completePaid(ctx, inv, start); err != nil {
				return true, err
			}
		}
		s.unwatch(inv.PaymentHash)
		return false, nil
	}
	if start.Sub(w.polled) < staleEvery {
		return false, nil
	}
	// Recorded already (a replay, or a crash before the event): complete it, stop watching.
	if _, found, err := s.paidTransfer(ctx, inv.PaymentHash); err != nil {
		return true, err
	} else if found {
		if err := s.completePaid(ctx, inv, start); err != nil {
			return true, err
		}
		s.unwatch(inv.PaymentHash)
		return true, nil
	}
	lctx, cancel := context.WithTimeout(ctx, lookupBound)
	tx, err := s.w.LookupInvoice(lctx, inv.PaymentHash)
	cancel()
	if err := s.crash(crashAfterLookup); err != nil {
		return true, err
	}
	s.mu.Lock()
	if cur, ok := s.watch[inv.PaymentHash]; ok {
		cur.polled = start
		s.watch[inv.PaymentHash] = cur
	}
	s.mu.Unlock()
	switch {
	case err != nil:
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		s.log.Info(logLookupFailed)
	case strings.ToLower(tx.PaymentHash) != inv.PaymentHash || tx.Type != nwc.TypeIncoming:
		s.log.Error(logBadLookup)
	case tx.State == nwc.StateSettled && tx.AmountMsat >= inv.AmountMsat:
		s.log.Error(logLatePaid, "order", inv.OrderID)
		if err := s.recordAndComplete(ctx, inv, tx.AmountMsat, start); err != nil {
			return true, err
		}
		s.unwatch(inv.PaymentHash)
	case tx.State == nwc.StateSettled:
		s.alertShort(inv)
	}
	return true, nil
}

func (s *Service) watchClosed(inv store.Invoice, polled time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watch[inv.PaymentHash] = watchInv{inv: inv, polled: polled}
}

func (s *Service) unwatch(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.watch, hash)
	delete(s.alerted, hash)
}

func (s *Service) alertShort(inv store.Invoice) {
	s.mu.Lock()
	first := !s.alerted[inv.PaymentHash]
	s.alerted[inv.PaymentHash] = true
	s.mu.Unlock()
	if first {
		s.log.Error(logShortSettle, "order", inv.OrderID)
	}
}

func (s *Service) recordAndComplete(ctx context.Context, inv store.Invoice, amountMsat int64, now time.Time) error {
	created, err := s.recordPaid(ctx, inv, amountMsat)
	if err != nil {
		return err
	}
	if created {
		if err := s.warnIfAlreadyPaid(ctx, inv); err != nil {
			return err
		}
	}
	if err := s.crash(crashAfterTransfer); err != nil {
		return err
	}
	return s.completePaid(ctx, inv, now)
}

// warnIfAlreadyPaid reports a second payment for an order: the money arrived, so it is recorded,
// but the order is paid once and the event exists once.
func (s *Service) warnIfAlreadyPaid(ctx context.Context, inv store.Invoice) error {
	invs, err := s.st.InvoicesForOrder(ctx, inv.OrderID)
	if err != nil {
		return err
	}
	for _, other := range invs {
		if other.PaymentHash == inv.PaymentHash {
			continue
		}
		if _, found, err := s.paidTransfer(ctx, other.PaymentHash); err != nil {
			return err
		} else if found {
			s.log.Error(logDoublePaid, "order", inv.OrderID)
			return nil
		}
	}
	return nil
}

// healPaid is the startup completion: for each invoice closed within HealWindow whose
// order has no order.paid, if the ledger holds its paid transfer, completePaid runs (the account_paid
// fact, then the event). One pass; completePaid is idempotent, so a second start adds nothing. A
// failure is logged with a fixed line and does not stop Run.
func (s *Service) healPaid(ctx context.Context) {
	now := s.now()
	invs, err := s.st.ClosedInvoicesWithoutPaid(ctx, now.Add(-HealWindow))
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error(logHealFailed)
		}
		return
	}
	for _, inv := range invs {
		if ctx.Err() != nil {
			return
		}
		_, found, err := s.paidTransfer(ctx, inv.PaymentHash)
		if err != nil {
			// The ledger is failing: the next lookups would fail the same way. Log once and stop.
			if ctx.Err() == nil {
				s.log.Error(logHealFailed, "order", inv.OrderID)
			}
			return
		}
		if !found {
			continue
		}
		if err := s.completePaid(ctx, inv, now); err != nil && ctx.Err() == nil {
			s.log.Error(logHealFailed, "order", inv.OrderID)
		}
	}
}

// completePaid is everything after the transfer exists: the account becomes Known (its account_paid
// fact, then the counts), the order.paid
// event is added (idempotent; skipped once done), the watched rails are told for an order with an
// on-chain or Monero amount (OnLightningPaid), the invoice stops holding a slot, and once it is past its expiry it is closed,
// so it is not looked up for ever. The hook runs at most once per completion: a hook error leaves the
// invoice not done, so the next lookup (the paid transfer exists) completes it again, hook included.
// settleOrder's heal path calls the hook too.
func (s *Service) completePaid(ctx context.Context, inv store.Invoice, now time.Time) error {
	s.mu.Lock()
	oi, tracked := s.open[inv.PaymentHash]
	s.mu.Unlock()
	if !tracked || !oi.done {
		o, err := s.st.OrderByID(ctx, inv.OrderID)
		if err != nil {
			return err
		}
		// The fact first, so the account is known in memory only once it is known in the store.
		if _, err := s.st.AddAccountPaid(ctx, inv.OrderID); err != nil {
			return err
		}
		s.markKnown(o.AccountID)
		if _, err := s.st.AddEvent(ctx, store.Event{Type: store.EventPaid, OrderID: inv.OrderID}); err != nil {
			return err
		}
		if err := s.crash(crashAfterEvent); err != nil {
			return err
		}
		if watched(o) && s.onPaid != nil {
			if err := s.onPaid(ctx, o); err != nil {
				return fmt.Errorf("%w: %w", errOnPaid, err)
			}
		}
		s.release(inv.PaymentHash, false, true, now)
	}
	if now.Before(inv.ExpiresAt) {
		return nil
	}
	if _, err := s.st.CloseInvoice(ctx, inv.PaymentHash, now, store.CloseLookup); err != nil {
		return err
	}
	s.detach(inv.PaymentHash)
	return nil
}

func (s *Service) closeInvoice(ctx context.Context, inv store.Invoice, at time.Time, reason string) error {
	if _, err := s.st.CloseInvoice(ctx, inv.PaymentHash, at, reason); err != nil {
		return err
	}
	s.detach(inv.PaymentHash)
	if reason == store.CloseLookup { // the closing lookup was at at: the next one is due staleEvery later
		s.watchClosed(inv, at)
	}
	if err := s.crash(crashAfterClose); err != nil {
		return err
	}
	return s.settleOrder(ctx, inv.OrderID)
}

// sweep adds order.expired (or, if the ledger shows a payment, order.paid) for the orders that are
// past their expiry with every invoice closed. A failing order is logged and the others go on.
func (s *Service) sweep(ctx context.Context) error {
	orders, err := s.st.OrdersToExpire(ctx, s.now())
	if err != nil {
		return err
	}
	for _, o := range orders {
		if err := s.settleOrder(ctx, o.ID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Error(logStepFailed)
		}
	}
	return nil
}

// watched reports whether the order has a watched rail (on-chain or Monero): that rail owns its final
// event, and the Lightning-paid hook is called for it.
func watched(o store.Order) bool { return o.AmountSat > 0 || o.AmountPiconero > 0 }

// settleOrder adds the order's final event if it is due: order.expired (not for an order with a
// watched rail, on-chain or Monero: that rail adds it) when it is past its expiry
// and every invoice is closed, unless a payment is in the ledger (then order.paid, healing a crash
// between the transfer and the event, and the Lightning-paid hook for a mixed order, whose error is
// returned as completePaid's is). A Pay in flight for the order holds it back. The read order is
// Status's: the clock, the flight, the invoices and closures, then the ledger.
func (s *Service) settleOrder(ctx context.Context, orderID string) error {
	now := s.now()
	if s.inFlight(orderID) {
		return nil
	}
	o, err := s.st.OrderByID(ctx, orderID)
	if err != nil {
		return err
	}
	if now.Before(o.ExpiresAt) {
		return nil
	}
	invs, err := s.st.InvoicesForOrder(ctx, orderID)
	if err != nil {
		return err
	}
	closed, err := s.closedSet(ctx, orderID)
	if err != nil {
		return err
	}
	for _, inv := range invs {
		if !closed[inv.PaymentHash] {
			return nil
		}
	}
	paid, _, err := s.orderPaidAt(ctx, orderID)
	if err != nil {
		return err
	}
	if paid {
		// The same order as completePaid: the account_paid fact, then the event.
		if _, err := s.st.AddAccountPaid(ctx, orderID); err != nil {
			return err
		}
		s.markKnown(o.AccountID)
		if _, err := s.st.AddEvent(ctx, store.Event{Type: store.EventPaid, OrderID: orderID}); err != nil {
			return err
		}
		// The watched rail is told too, as completePaid tells it: this heal may be the only
		// completion the order gets, and with its watched payment settled deep only this call can give the
		// both-rails ALERT. The rail logs it once per process, so a call after completePaid's adds nothing.
		if watched(o) && s.onPaid != nil {
			if err := s.onPaid(ctx, o); err != nil {
				return fmt.Errorf("%w: %w", errOnPaid, err)
			}
		}
		return nil
	}
	if watched(o) {
		return nil // the watched rails (on-chain, Monero) own the final events of an order with one
	}
	_, err = s.st.AddEvent(ctx, store.Event{Type: store.EventExpired, OrderID: orderID})
	return err
}
