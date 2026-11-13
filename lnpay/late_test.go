package lnpay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/store"
)

// closedExpired makes an order and its invoice, then lets both expire: one pass closes the invoice
// by lookup and adds order.expired.
func closedExpired(t *testing.T, e *env) (store.Order, store.Invoice) {
	t.Helper()
	o := e.order("late", 2_000_000)
	inv := e.mustPay(o)
	e.clk.Advance(o.ExpiresAt.Sub(e.clk.Now()) + 5*time.Minute)
	e.pass()
	cl, _ := e.st.ClosuresForOrder(context.Background(), o.ID)
	if len(cl) != 1 || cl[0].Reason != store.CloseLookup || e.eventCount(store.EventExpired, o.ID) != 1 || e.status(o).State != StateExpired {
		t.Fatalf("setup: closures %+v, state %s", cl, e.status(o).State)
	}
	return o, inv
}

// paidLate checks that the order is paid exactly once, after its order.expired, with the alert.
func paidLate(t *testing.T, e *env, o store.Order) {
	t.Helper()
	ctx := context.Background()
	if s := e.status(o); s.State != StatePaid {
		t.Fatalf("state %s", s.State)
	}
	if e.transfers(o) != 1 || e.merchantCredits() != uint64(o.AmountMsat) {
		t.Fatalf("transfers %d, credits %d", e.transfers(o), e.merchantCredits())
	}
	evs, err := e.st.UndeliveredEvents(ctx, store.MainEndpoint, epoch.Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	var last time.Time
	for _, ev := range evs { // oldest first
		if ev.OrderID == o.ID {
			types = append(types, ev.Type)
			if !ev.CreatedAt.After(last) {
				t.Fatalf("events not strictly in time order: %+v", evs)
			}
			last = ev.CreatedAt
		}
	}
	if fmt.Sprint(types) != fmt.Sprint([]string{store.EventExpired, store.EventPaid}) {
		t.Fatalf("events %v, want order.expired then order.paid", types)
	}
	if n := strings.Count(e.log.String(), logLatePaid); n != 1 {
		t.Fatalf("%d late-payment alerts, want 1:\n%s", n, e.log.String())
	}
	if k, err := e.st.AccountPaid(ctx, o.AccountID); err != nil || !k {
		t.Fatalf("account fact: %v %v", k, err)
	}
}

// A payment that settles after the closing lookup is found by the slow poll and recorded once;
// further polls, passes and notifications change nothing.
func TestLateSettleAfterClose(t *testing.T) {
	e := newEnv(t)
	o, inv := closedExpired(t, e)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass() // within the slow-poll period of the closing lookup: not looked up yet
	if e.status(o).State != StateExpired {
		t.Fatal("looked up before the slow-poll period")
	}
	e.clk.Advance(staleEvery)
	e.pass()
	paidLate(t, e, o)
	// Replays: more passes, a notification, a restart.
	n := e.w.lookupCount(inv.PaymentHash)
	for i := 0; i < 3; i++ {
		e.clk.Advance(staleEvery)
		e.pass()
		e.svc.prioritise(inv.PaymentHash)
	}
	e.restart()
	e.pass()
	e.clk.Advance(staleEvery)
	e.pass()
	paidLate(t, e, o)
	if e.w.lookupCount(inv.PaymentHash) != n {
		t.Fatal("a recorded late payment was looked up again")
	}
}

// A notification for the closed invoice is looked up at once, and racing it with a poll records
// the payment once.
func TestLateSettleNotificationAndPoll(t *testing.T) {
	e := newEnv(t)
	o, inv := closedExpired(t, e)
	e.clk.Advance(time.Minute) // a payment a minute after the close (events are ordered by time)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.svc.prioritise(inv.PaymentHash)
	var c cursor
	got, ok, err := e.svc.nextInvoice(context.Background(), &c)
	if err != nil || !ok || got.PaymentHash != inv.PaymentHash {
		t.Fatalf("front: %v %v %s", ok, err, got.PaymentHash)
	}
	if looked, err := e.svc.reconcileInv(context.Background(), got); err != nil || !looked {
		t.Fatalf("looked %v, err %v", looked, err)
	}
	e.clk.Advance(staleEvery)
	e.pass()
	paidLate(t, e, o)
}

// A restart between the close and the late settlement re-derives the watch from the store; the
// first round after the start looks the invoice up.
func TestLateSettleAfterRestart(t *testing.T) {
	e := newEnv(t)
	o, inv := closedExpired(t, e)
	e.clk.Advance(time.Minute)
	e.restart()
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass()
	paidLate(t, e, o)
}

// A crash after the late payment's transfer, before its event: the restart completes it (the
// order already has order.expired, so only the watch can heal it).
func TestLateSettleCrashAfterTransfer(t *testing.T) {
	e := newEnv(t)
	o, inv := closedExpired(t, e)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.clk.Advance(staleEvery)
	e.svc.hook = func(p string) error {
		if p == crashAfterTransfer {
			return errors.New("crash")
		}
		return nil
	}
	if err := e.svc.pass(context.Background()); err == nil {
		t.Fatal("no crash")
	}
	if e.transfers(o) != 1 || e.eventCount(store.EventPaid, o.ID) != 0 {
		t.Fatal("setup: want the transfer without the event")
	}
	e.restart()
	e.pass()
	e.pass()
	paidLate(t, e, o)
}

// Never settled: polled every 10 minutes until the deadline, then never again, with no line of
// its own; a settlement after the deadline is not recorded.
func TestWatchStopsAtDeadline(t *testing.T) {
	e := newEnv(t)
	o, inv := closedExpired(t, e)
	e.w.lookupErr = &nwc.WalletError{Code: "NOT_FOUND", Message: "x"} // not evidence of anything here
	deadline := inv.ExpiresAt.Add(store.DeadlineGrace)
	e.clk.Advance(deadline.Add(-time.Second).Sub(e.clk.Now()))
	before := e.w.lookupCount(inv.PaymentHash)
	e.pass()
	if e.w.lookupCount(inv.PaymentHash) != before+1 {
		t.Fatal("not looked up before the deadline")
	}
	e.w.lookupErr = nil
	logs := e.log.String()
	n := e.w.lookupCount(inv.PaymentHash)
	e.clk.Advance(time.Second)
	for i := 0; i < 3; i++ {
		e.pass()
		e.clk.Advance(staleEvery)
	}
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass()
	e.restart()
	e.pass()
	if e.w.lookupCount(inv.PaymentHash) != n || e.log.String() != logs {
		t.Fatalf("looked up or logged after the deadline:\n%s", strings.TrimPrefix(e.log.String(), logs))
	}
	if e.status(o).State != StateExpired || e.transfers(o) != 0 || closures(t, e, o) != 1 {
		t.Fatal("changed after the deadline")
	}
	if contains(logs, logDeadline) || contains(logs, logStale) {
		t.Fatal("a closed invoice's error answer was treated like an open one's")
	}
}

// A short settlement, or a row for another hash, on a closed invoice is an alert and not paid.
func TestLateShortSettleAndBadRow(t *testing.T) {
	t.Run("short", func(t *testing.T) {
		e := newEnv(t)
		o, inv := closedExpired(t, e)
		e.w.settle(inv.PaymentHash, inv.AmountMsat-1000)
		for i := 0; i < 3; i++ {
			e.clk.Advance(staleEvery)
			e.pass()
		}
		if e.status(o).State != StateExpired || e.transfers(o) != 0 || strings.Count(e.log.String(), logShortSettle) != 1 {
			t.Fatalf("state %s, transfers %d, log:\n%s", e.status(o).State, e.transfers(o), e.log.String())
		}
	})
	t.Run("bad row", func(t *testing.T) {
		e := newEnv(t)
		o, inv := closedExpired(t, e)
		e.w.settle(inv.PaymentHash, inv.AmountMsat)
		e.svc.w = badRowWallet{e.w}
		e.clk.Advance(staleEvery)
		e.pass()
		if e.status(o).State != StateExpired || e.transfers(o) != 0 || !contains(e.log.String(), logBadLookup) {
			t.Fatalf("state %s, log:\n%s", e.status(o).State, e.log.String())
		}
	})
}

// A watched closed invoice holds no MaxOpen slot and does not stop a new invoice for its order.
func TestWatchedInvoiceHoldsNoSlot(t *testing.T) {
	e := newEnv(t, func(e *env) { e.limCfg.MaxOpen, e.limCfg.NewShare = 2, 0.5 }) // New may hold 1
	o := e.order("a", 1_000_000)
	inv := e.mustPay(o)
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + 5*time.Minute) // invoice closed, order still open
	e.pass()
	if closures(t, e, o) != 1 {
		t.Fatal("not closed")
	}
	if total, _ := e.svc.counts(); total != 0 {
		t.Fatalf("watched invoice counts: %d", total)
	}
	inv2 := e.mustPay(o) // the one New slot
	if inv2.PaymentHash == inv.PaymentHash || e.w.makeCalls.Load() != 2 {
		t.Fatal("no new invoice while the first is watched")
	}
	if _, err := e.pay(e.order("b", 1_000_000)); !errors.Is(err, limits.ErrClassShare) && !errors.Is(err, limits.ErrOpenCap) {
		t.Fatalf("the New slot should be taken by the new invoice only: %v", err)
	}
	sameCounts(t, e, "watched")
	e.restart()
	sameCounts(t, e, "restart")
	// Both are paid: both recorded, the order is paid once, and the second payment alerts.
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.w.settle(inv2.PaymentHash, inv2.AmountMsat)
	e.pass()
	if e.transfers(o) != 2 || e.eventCount(store.EventPaid, o.ID) != 1 || !contains(e.log.String(), logDoublePaid) {
		t.Fatalf("transfers %d, log:\n%s", e.transfers(o), e.log.String())
	}
}
