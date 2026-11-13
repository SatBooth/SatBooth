package lnpay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
)

func (e *env) orderSat(account string, msat, sat int64) store.Order {
	e.t.Helper()
	e.refs++
	o, _, err := e.st.CreateOrder(context.Background(), store.Order{AccountID: account,
		MerchantRef: fmt.Sprintf("ref-%d", e.refs), AmountMsat: msat, AmountSat: sat, ExpiresAt: e.clk.Now().Add(time.Hour)})
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

func TestPayOnChainOnlyOrder(t *testing.T) {
	// One token in the bucket, refilled so slowly that a taken token would show: the Lightning Pay
	// after the refused one must still get it.
	e := newEnv(t, func(e *env) {
		e.limCfg.RatePerSec, e.limCfg.Burst = 1e-9, 1
	})
	o := e.orderSat("acct", 0, 50_000)
	if _, err := e.pay(o); !errors.Is(err, ErrNoLightning) {
		t.Fatalf("err %v, want ErrNoLightning", err)
	}
	if n, _, _ := e.st.AttemptsSince(context.Background(), epoch.Add(-time.Hour)); n != 0 {
		t.Fatalf("%d attempt rows", n)
	}
	if e.w.makeCalls.Load() != 0 {
		t.Fatal("wallet was called")
	}
	if len(e.open_(o)) != 0 {
		t.Fatal("invoice stored")
	}
	if _, err := e.pay(e.order("acct2", 2_000_000)); err != nil {
		t.Fatalf("the refused Pay took the limit token: %v", err)
	}
}

func TestStatusOnChainOnlyOrder(t *testing.T) {
	e := newEnv(t)
	o := e.orderSat("acct", 0, 50_000)
	if s := e.status(o); s.State != StateNew || s.Invoice != nil {
		t.Fatalf("before expiry: %+v", s)
	}
	e.clk.Advance(time.Hour)
	if s := e.status(o); s.State != StateExpired {
		t.Fatalf("after expiry: %+v", s)
	}
}

func TestSweepLeavesOnChainOrdersAlone(t *testing.T) {
	e := newEnv(t)
	// Mixed order, invoice closed past expiry: no order.expired from the sweep.
	mixed := e.orderSat("acct", 2_000_000, 50_000)
	e.mustPay(mixed)
	e.clk.Advance(2 * time.Hour)
	e.pass()
	if err := e.svc.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := e.eventCount(store.EventExpired, mixed.ID); n != 0 {
		t.Fatalf("mixed order got %d order.expired", n)
	}
	// A Lightning-only order still expires.
	ln := e.order("acct2", 2_000_000)
	e.mustPay(ln)
	e.clk.Advance(2 * time.Hour)
	e.pass()
	if n := e.eventCount(store.EventExpired, ln.ID); n != 1 {
		t.Fatalf("lightning-only order got %d order.expired", n)
	}
}

func TestSettleOrderKeepsPaidHealForMixed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o := e.orderSat("acct", 2_000_000, 50_000)
	inv := e.mustPay(o)
	if _, err := e.svc.recordPaid(ctx, inv, o.AmountMsat); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(2 * time.Hour)
	if _, err := e.st.CloseInvoice(ctx, inv.PaymentHash, e.clk.Now(), store.CloseLookup); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.settleOrder(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if e.eventCount(store.EventPaid, o.ID) != 1 || e.eventCount(store.EventExpired, o.ID) != 0 {
		t.Fatal("heal lost")
	}
}

// strandedPaid makes an order with an invoice that has a ledger transfer and no order.paid, closed
// as a deadline closure 24 h after the invoice's expiry; closedAgo is how long before the restart
// the clock then advances. It restarts the service on the files and returns the order and invoice.
func strandedPaid(t *testing.T, e *env, closedAgo time.Duration) (store.Order, store.Invoice) {
	return stranded(t, e, e.order("stranded", 2_000_000), true, closedAgo)
}

// stranded is strandedPaid for a given order; withTransfer false leaves the ledger without the
// payment, so the closure is a genuine unpaid one.
func stranded(t *testing.T, e *env, o store.Order, withTransfer bool, closedAgo time.Duration) (store.Order, store.Invoice) {
	t.Helper()
	ctx := context.Background()
	inv := e.mustPay(o)
	if withTransfer {
		if _, err := e.svc.recordPaid(ctx, inv, o.AmountMsat); err != nil {
			t.Fatal(err)
		}
	}
	at := inv.ExpiresAt.Add(24 * time.Hour)
	e.clk.Advance(at.Sub(e.clk.Now()))
	if _, err := e.st.CloseInvoice(ctx, inv.PaymentHash, at, store.CloseDeadline); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(closedAgo)
	e.restart()
	return o, inv
}

// runUntil runs the service until cond holds or fails.
func runUntil(t *testing.T, e *env, what string, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	eventually(t, what, cond)
	cancel()
	<-done
}

func TestHealPaidAtStartup(t *testing.T) {
	e := newEnv(t)
	o, _ := strandedPaid(t, e, time.Minute)
	if e.eventCount(store.EventPaid, o.ID) != 0 {
		t.Fatal("setup: event exists")
	}
	runUntil(t, e, "order.paid", func() bool { return e.eventCount(store.EventPaid, o.ID) == 1 })
	known, err := e.st.AccountPaid(context.Background(), "stranded")
	if err != nil || !known {
		t.Fatalf("account_paid: %v %v", known, err)
	}
	// Replay: after the heal nothing is left to heal, and a second start adds nothing.
	left, err := e.st.ClosedInvoicesWithoutPaid(context.Background(), epoch.Add(-HealWindow))
	if err != nil || len(left) != 0 {
		t.Fatalf("still without order.paid: %v, %v", left, err)
	}
	e.restart()
	e.svc.healPaid(context.Background())
	if n := e.eventCount(store.EventPaid, o.ID); n != 1 {
		t.Fatalf("%d order.paid after replay", n)
	}
}

func TestHealIgnoresClosureWithoutTransfer(t *testing.T) {
	e := newEnv(t)
	o, _ := stranded(t, e, e.order("unpaid", 2_000_000), false, time.Minute)
	e.svc.healPaid(context.Background())
	if n := e.eventCount(store.EventPaid, o.ID); n != 0 {
		t.Fatalf("%d order.paid for an unpaid closure", n)
	}
	if known, err := e.st.AccountPaid(context.Background(), "unpaid"); err != nil || known {
		t.Fatalf("account_paid: %v %v", known, err)
	}
}

func TestHealMixedOrderThroughRun(t *testing.T) {
	e := newEnv(t)
	o, _ := stranded(t, e, e.orderSat("mixed", 2_000_000, 50_000), true, time.Minute)
	runUntil(t, e, "order.paid", func() bool { return e.eventCount(store.EventPaid, o.ID) == 1 })
	if known, err := e.st.AccountPaid(context.Background(), "mixed"); err != nil || !known {
		t.Fatalf("account_paid: %v %v", known, err)
	}
	if n := e.eventCount(store.EventExpired, o.ID); n != 0 {
		t.Fatalf("%d order.expired", n)
	}
}

// healPaid alone completes a stranded mixed order (a Lightning transfer, its invoice closed, no order.paid, and an
// on-chain amount): no other heal path runs here, unlike TestHealMixedOrderThroughRun.
func TestHealPaidAloneOnMixedOrder(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o, _ := stranded(t, e, e.orderSat("mixed-alone", 2_000_000, 50_000), true, time.Minute)
	if e.eventCount(store.EventPaid, o.ID) != 0 {
		t.Fatal("setup: event exists")
	}
	if known, err := e.st.AccountPaid(ctx, "mixed-alone"); err != nil || known {
		t.Fatalf("setup: account_paid %v %v", known, err)
	}
	e.svc.healPaid(ctx)
	if n := e.eventCount(store.EventPaid, o.ID); n != 1 {
		t.Fatalf("%d order.paid after healPaid", n)
	}
	if known, err := e.st.AccountPaid(ctx, "mixed-alone"); err != nil || !known {
		t.Fatalf("account_paid: %v %v", known, err)
	}
	e.svc.healPaid(ctx) // replay adds nothing
	if n := e.eventCount(store.EventPaid, o.ID); n != 1 {
		t.Fatalf("%d order.paid after replay", n)
	}
}

func TestSweepPaidHealWritesAccountFact(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o := e.order("swept", 2_000_000)
	inv := e.mustPay(o)
	if _, err := e.svc.recordPaid(ctx, inv, o.AmountMsat); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(2 * time.Hour)
	if _, err := e.st.CloseInvoice(ctx, inv.PaymentHash, e.clk.Now(), store.CloseLookup); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if e.eventCount(store.EventPaid, o.ID) != 1 {
		t.Fatal("no order.paid")
	}
	if known, err := e.st.AccountPaid(ctx, "swept"); err != nil || !known {
		t.Fatalf("account_paid: %v %v", known, err)
	}
	if known, _ := e.svc.accountKnown(ctx, "swept"); !known {
		t.Fatal("not known in memory")
	}
}

func TestHealStoreErrorLogs(t *testing.T) {
	e := newEnv(t)
	e.st.Close()
	e.svc.healPaid(context.Background())
	if n := strings.Count(e.log.String(), logHealFailed); n != 1 {
		t.Fatalf("%d lines, want 1:\n%s", n, e.log.String())
	}
}

func TestHealLedgerErrorLogsOnce(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		stranded(t, e, e.order(fmt.Sprintf("a%d", i), 2_000_000), true, time.Minute)
	}
	e.lg.Close()
	e.svc.healPaid(context.Background())
	if n := strings.Count(e.log.String(), logHealFailed); n != 1 {
		t.Fatalf("%d lines, want 1:\n%s", n, e.log.String())
	}
	if !contains(e.log.String(), "order=") {
		t.Fatal("no order attribute")
	}
}

func TestHealWindow(t *testing.T) {
	for _, tc := range []struct {
		ago  time.Duration
		scan bool
	}{{HealWindow - time.Second, true}, {HealWindow, true}, {HealWindow + time.Second, false}} {
		t.Run(tc.ago.String(), func(t *testing.T) {
			e := newEnv(t)
			o, _ := strandedPaid(t, e, 0)
			// Move the clock so the closure is exactly tc.ago old. healPaid is called alone: the
			// expiry sweep has its own heal of a paid order and would hide the window.
			cl, err := e.st.ClosuresForOrder(context.Background(), o.ID)
			if err != nil || len(cl) != 1 {
				t.Fatalf("closures %v, %v", cl, err)
			}
			e.clk.Advance(tc.ago - e.clk.Now().Sub(cl[0].At))
			e.svc.healPaid(context.Background())
			want := 0
			if tc.scan {
				want = 1
			}
			if n := e.eventCount(store.EventPaid, o.ID); n != want {
				t.Fatalf("closure %v old: %d order.paid, want %d", tc.ago, n, want)
			}
		})
	}
}

func TestHealLedgerErrorLogsAndRoundRuns(t *testing.T) {
	e := newEnv(t)
	strandedPaid(t, e, time.Minute)
	live := e.order("live", 2_000_000)
	e.mustPay(live)
	e.lg.Close() // every ledger lookup fails from here
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	eventually(t, "the fixed line", func() bool { return contains(e.log.String(), logHealFailed) })
	// The round runs: a reconciler step is attempted (and fails, the ledger being closed).
	eventually(t, "a reconciler step", func() bool { return contains(e.log.String(), logStepFailed) })
	select {
	case err := <-done:
		t.Fatalf("Run returned: %v", err)
	default:
	}
	cancel()
	<-done
}

// orderPico makes an order with Lightning and Monero amounts and no on-chain amount.
func (e *env) orderPico(account string, msat, pico int64) store.Order {
	e.t.Helper()
	e.refs++
	o, _, err := e.st.CreateOrder(context.Background(), store.Order{AccountID: account,
		MerchantRef: fmt.Sprintf("ref-%d", e.refs), AmountMsat: msat, AmountPiconero: pico, ExpiresAt: e.clk.Now().Add(time.Hour)})
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

// The Lightning + Monero twin of TestSweepLeavesOnChainOrdersAlone: the watched rail owns the final event, so the
// sweep adds no order.expired to an order with a Monero amount, past its expiry with its invoice closed.
func TestSweepLeavesMoneroOrdersAlone(t *testing.T) {
	e := newEnv(t)
	xmr := e.orderPico("acct", 2_000_000, 1_000_000_000_000)
	e.mustPay(xmr)
	e.clk.Advance(2 * time.Hour)
	e.pass()
	if err := e.svc.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := e.eventCount(store.EventExpired, xmr.ID); n != 0 {
		t.Fatalf("a Lightning + Monero order got %d order.expired", n)
	}
}
