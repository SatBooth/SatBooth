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

// gateWallet holds every LookupInvoice until the test releases it.
type gateWallet struct {
	*fakeWallet
	entered chan struct{}
	release chan struct{}
}

func (g gateWallet) LookupInvoice(ctx context.Context, h string) (*nwc.Transaction, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.fakeWallet.LookupInvoice(ctx, h)
}

// The first press is cancelled before its attempt row is written (its flight is held in the
// lookup of the order's expired invoice). The press that joined its flight still gets the new
// invoice; the cancelled press gets its own context error, as outcome unknown.
func TestCancelledFirstPressDoesNotFailTheOthers(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 3_000_000)
	inv1 := e.mustPay(o)
	e.clk.Advance(inv1.ExpiresAt.Sub(e.clk.Now()) + 30*time.Second) // expired, not closed, order still open
	g := gateWallet{fakeWallet: e.w, entered: make(chan struct{}, 1), release: make(chan struct{})}
	e.svc.w = g
	joined := make(chan struct{}, 1)
	e.svc.hook = func(p string) error {
		if p == hookJoined {
			joined <- struct{}{}
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := e.svc.Pay(ctx, o.Token); first <- err }()
	<-g.entered // the flight is in checkExpired: no attempt row yet
	type result struct {
		inv store.Invoice
		err error
	}
	second := make(chan result, 1)
	go func() { inv, err := e.svc.Pay(context.Background(), o.Token); second <- result{inv, err} }()
	<-joined
	cancel()
	firstErr := <-first
	close(g.release)
	r := <-second
	if r.err != nil || r.inv.PaymentHash == "" || r.inv.PaymentHash == inv1.PaymentHash {
		t.Fatalf("second press: %+v %v", r.inv, r.err)
	}
	if !errors.Is(firstErr, ErrUnknown) || !errors.Is(firstErr, context.Canceled) {
		t.Fatalf("cancelled press: %v, want ErrUnknown and context.Canceled", firstErr)
	}
	if e.w.makeCalls.Load() != 2 || len(e.open_(o)) != 2 {
		t.Fatalf("%d make calls, %d invoices", e.w.makeCalls.Load(), len(e.open_(o)))
	}
	if n, _, _ := e.st.AttemptsSince(context.Background(), epoch); n != 2 {
		t.Fatalf("%d attempts", n)
	}
	sameCounts(t, e, "after")
}

// A press whose own context is already done gets an error the checkout shows as busy.
func TestPressWithDoneContext(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.svc.Pay(ctx, o.Token); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if e.w.makeCalls.Load() != 0 {
		t.Fatal("a cancelled press made an invoice")
	}
}

// The account is known from the stored fact. A crash after the transfer, before the fact, leaves
// the account new (the ledger alone no longer makes it known: no scan); the replay writes the fact.
func TestKnownFactWrittenOnReplay(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	o := e.order("acct", 2_000_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.svc.hook = func(p string) error {
		if p == crashAfterTransfer {
			return errors.New("crash")
		}
		return nil
	}
	_ = e.svc.pass(ctx)
	e.restart()
	if e.transfers(o) != 1 {
		t.Fatal("no transfer before the replay")
	}
	if k, err := e.st.AccountPaid(ctx, "acct"); err != nil || k {
		t.Fatalf("fact before the replay: %v %v", k, err)
	}
	if k, err := e.svc.accountKnown(ctx, "acct"); err != nil || k {
		t.Fatalf("known before the replay: %v %v", k, err)
	}
	e.pass()
	if k, err := e.st.AccountPaid(ctx, "acct"); err != nil || !k {
		t.Fatalf("fact after the replay: %v %v", k, err)
	}
	if e.transfers(o) != 1 || e.eventCount(store.EventPaid, o.ID) != 1 {
		t.Fatal("not paid exactly once")
	}
}

// A paid account reads as known after a restart, and its open invoices are in the Known class.
func TestKnownAfterRestart(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(e *env) { e.limCfg.MaxOpen, e.limCfg.NewShare = 10, 0.1 }) // New may hold 1
	makeKnown(t, e, "k")
	e.restart()
	if k, err := e.svc.accountKnown(ctx, "k"); err != nil || !k {
		t.Fatalf("after restart: %v %v", k, err)
	}
	e.mustPay(e.order("n", 1_000_000)) // the one New slot
	e.mustPay(e.order("k", 1_000_000)) // Known: not limited by the New share
	e.mustPay(e.order("k", 1_000_000))
	if total, nw := e.svc.counts(); total != 3 || nw != 1 {
		t.Fatalf("counts (%d, %d), want (3, 1)", total, nw)
	}
	e.restart()
	sameCounts(t, e, "second restart")
	if total, nw := e.svc.counts(); total != 3 || nw != 1 {
		t.Fatalf("after restart: counts (%d, %d), want (3, 1)", total, nw)
	}
}

// A settlement below the amount, past the deadline: not recorded, not closed, stale, one alert
// for the short settlement and the stale line.
func TestShortSettlementPastDeadline(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 2_000_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat-1000)
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + store.DeadlineGrace)
	e.pass()
	e.pass()
	if closures(t, e, o) != 0 || e.transfers(o) != 0 || e.status(o).State != StateNew || e.eventCount(store.EventExpired, o.ID) != 0 {
		t.Fatal("a short settlement past the deadline was recorded or closed")
	}
	logs := e.log.String()
	if strings.Count(logs, logShortSettle) != 1 || strings.Count(logs, logStale) != 1 {
		t.Fatalf("log:\n%s", logs)
	}
	if total, _ := e.svc.counts(); total != 0 {
		t.Fatalf("stale invoice counts: %d", total)
	}
	sameCounts(t, e, "stale")
}

// An order's PaidAt (the API's paid_at) is the ledger transfer's time, to the nanosecond, which is the clock at the
// payment.
func TestPaidAtIsTheTransferTime(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	inv := e.mustPay(o)
	e.clk.Advance(3*time.Minute + 123_456_789*time.Nanosecond)
	at := e.clk.Now()
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass()
	tr, found, err := e.lg.LookupTransfer(context.Background(), PaidTransferID(inv.PaymentHash))
	if err != nil || !found {
		t.Fatalf("transfer: %v %v", found, err)
	}
	s := e.status(o)
	if s.State != StatePaid || !s.PaidAt.Equal(at) || !s.PaidAt.Equal(time.Unix(0, int64(tr.Timestamp))) || s.PaidAt.Nanosecond() != 123_456_789 {
		t.Fatalf("paid_at %v, clock %v, transfer %d", s.PaidAt, at, tr.Timestamp)
	}
	e.clk.Advance(time.Hour)
	if s := e.status(o); !s.PaidAt.Equal(at) {
		t.Fatalf("paid_at moved: %v", s.PaidAt)
	}
}

// A stale invoice holds no slot, also after a restart.
func TestStaleInvoiceNotCountedAfterRestart(t *testing.T) {
	e := newEnv(t, func(e *env) { e.limCfg.MaxOpen, e.limCfg.NewShare = 2, 0.5 })
	o := e.order("a", 1_000_000)
	inv := e.mustPay(o)
	e.w.lookupErr = fmt.Errorf("%w: relay down", nwc.ErrNoResponse)
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + store.DeadlineGrace)
	e.pass()
	if total, _ := e.svc.counts(); total != 0 {
		t.Fatalf("stale invoice counts: %d", total)
	}
	e.restart()
	if total, _ := e.svc.counts(); total != 0 {
		t.Fatalf("after restart the stale invoice counts again: %d", total)
	}
	sameCounts(t, e, "restart")
	e.pass()
	sameCounts(t, e, "pass after restart")
	// Its slot is free: another New account can take the New share's one slot.
	e.w.lookupErr = nil
	e.mustPay(e.order("b", 1_000_000))
	if closures(t, e, o) != 0 {
		t.Fatal("the stale invoice was closed")
	}
}

// An invoice committed although AddInvoice failed, whose read-back failed too, is adopted by the
// reconciler's next snapshot: counted once, tracked, and polled slowly once stale.
func TestUntrackedInvoiceAdopted(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	e.svc.addInvoice = func(ctx context.Context, inv store.Invoice) error {
		if err := e.st.AddInvoice(ctx, inv); err != nil {
			return err
		}
		return errors.New("hiccup")
	}
	e.svc.readBack = func(context.Context, string) ([]store.Invoice, error) { return nil, errors.New("hiccup again") }
	if _, err := e.pay(o); !errors.Is(err, ErrUnknown) {
		t.Fatalf("pay: %v", err)
	}
	invs := e.open_(o)
	if len(invs) != 1 {
		t.Fatalf("%d invoices stored", len(invs))
	}
	e.pass()
	e.svc.mu.Lock()
	_, tracked := e.svc.open[invs[0].PaymentHash]
	e.svc.mu.Unlock()
	if !tracked {
		t.Fatal("the stored invoice is not tracked after a pass")
	}
	if total, _ := e.svc.counts(); total != 1 {
		t.Fatalf("total %d, want 1", total)
	}
	sameCounts(t, e, "adopted")
	// Stale: one alert line, then slow polling.
	e.w.lookupErr = fmt.Errorf("%w: relay down", nwc.ErrNoResponse)
	e.clk.Advance(invs[0].ExpiresAt.Sub(e.clk.Now()) + store.DeadlineGrace)
	e.pass()
	e.pass()
	if n := strings.Count(e.log.String(), logStale); n != 1 {
		t.Fatalf("%d stale lines, want 1", n)
	}
	if total, _ := e.svc.counts(); total != 0 {
		t.Fatalf("stale total %d", total)
	}
	sameCounts(t, e, "stale")
}

// A snapshot taken while a Pay for the order is in flight does not adopt its new invoice: the
// flight tracks it, and it is counted once.
func TestNoAdoptionDuringFlight(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	stored := make(chan struct{})
	release := make(chan struct{})
	e.svc.addInvoice = func(ctx context.Context, inv store.Invoice) error {
		err := e.st.AddInvoice(ctx, inv)
		close(stored)
		<-release
		return err
	}
	done := make(chan error, 1)
	go func() { _, err := e.pay(o); done <- err }()
	<-stored
	e.pass() // sees the stored invoice, not yet in s.open
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if total, _ := e.svc.counts(); total != 1 {
		t.Fatalf("total %d, want 1", total)
	}
	sameCounts(t, e, "after")
}

// An order whose amount the current limits refuse (created under older bounds) is refused with
// ErrAmount, which the checkout shows as busy, and one fixed alert line tells the operator.
func TestAmountRefusalIsLogged(t *testing.T) {
	e := newEnv(t)
	for _, msat := range []int64{2_000_000_000, 1_000_500} { // above MaxMsat; not whole sats
		o := e.order("a", msat)
		if _, err := e.pay(o); !errors.Is(err, limits.ErrAmount) {
			t.Fatalf("%d: %v", msat, err)
		}
	}
	if n := strings.Count(e.log.String(), logAmountRefused); n != 2 {
		t.Fatalf("%d lines, want 2:\n%s", n, e.log.String())
	}
	if e.w.makeCalls.Load() != 0 {
		t.Fatal("an invoice was made")
	}
}

// An invoice that expired before a restart, with failing lookups, stops counting at its own
// deadline (ExpiresAt + 24 h), as the recount says, although the restart moved its stale deadline to
// 24 h after the start.
func TestOwnDeadlineReleasesAfterRestart(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	inv := e.mustPay(o)
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + time.Hour) // expired, never looked up since
	e.restart()                                               // the stale deadline is now this start + 24 h
	e.w.lookupErr = fmt.Errorf("%w: relay down", nwc.ErrNoResponse)
	e.pass()
	if total, _ := e.svc.counts(); total != 1 {
		t.Fatalf("before its own deadline: total %d", total)
	}
	e.clk.Advance(inv.ExpiresAt.Add(store.DeadlineGrace).Sub(e.clk.Now()))
	e.pass()
	if total, _ := e.svc.counts(); total != 0 {
		t.Fatalf("past its own deadline: total %d", total)
	}
	sameCounts(t, e, "past its own deadline")
	if closures(t, e, o) != 0 || contains(e.log.String(), logStale) {
		t.Fatal("closed, or stale before the start + 24 h")
	}
}

// MarkKnown (Ruling T2): an account paid on another rail (the on-chain watcher writes the account_paid fact,
// then calls MarkKnown) leaves the New share at once, as one paid on Lightning does; the counts agree with a recount.
func TestMarkKnownFromAnotherRail(t *testing.T) {
	e := newEnv(t, func(e *env) { e.limCfg.MaxOpen, e.limCfg.NewShare = 10, 0.2 }) // New may hold 2
	e.mustPay(e.order("n1", 1_000_000))
	e.mustPay(e.order("n2", 1_000_000))
	if _, err := e.pay(e.order("n3", 1_000_000)); !errors.Is(err, limits.ErrClassShare) {
		t.Fatalf("a third new account: %v", err)
	}
	oc, _, err := e.st.CreateOrder(context.Background(), store.Order{AccountID: "n1", MerchantRef: "onchain-1", AmountSat: 5_000,
		ExpiresAt: e.clk.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.AddAccountPaid(context.Background(), oc.ID); err != nil { // the fact first
		t.Fatal(err)
	}
	e.svc.MarkKnown("n1")
	e.svc.MarkKnown("n1") // twice is once
	if _, nw := e.svc.counts(); nw != 1 {
		t.Fatalf("new-class count %d, want 1 after n1 became known on-chain", nw)
	}
	sameCounts(t, e, "after MarkKnown")
	e.mustPay(e.order("n3", 1_000_000)) // the freed New slot
}

// The Monero twin of TestMarkKnownFromAnotherRail: an account paid in Monero (the Monero watcher writes the fact, then
// calls MarkKnown) leaves the New share at once.
func TestMarkKnownFromMonero(t *testing.T) {
	e := newEnv(t, func(e *env) { e.limCfg.MaxOpen, e.limCfg.NewShare = 10, 0.2 }) // New may hold 2
	e.mustPay(e.order("n1", 1_000_000))
	e.mustPay(e.order("n2", 1_000_000))
	if _, err := e.pay(e.order("n3", 1_000_000)); !errors.Is(err, limits.ErrClassShare) {
		t.Fatalf("a third new account: %v", err)
	}
	ox, _, err := e.st.CreateOrder(context.Background(), store.Order{AccountID: "n1", MerchantRef: "monero-1",
		AmountPiconero: 1_000_000_000_000, ExpiresAt: e.clk.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.AddAccountPaid(context.Background(), ox.ID); err != nil {
		t.Fatal(err)
	}
	e.svc.MarkKnown("n1")
	if _, nw := e.svc.counts(); nw != 1 {
		t.Fatalf("new-class count %d, want 1 after n1 became known in Monero", nw)
	}
	sameCounts(t, e, "after MarkKnown")
	e.mustPay(e.order("n3", 1_000_000))
}
