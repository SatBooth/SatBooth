package lnpay

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/store"
)

func sameCounts(t *testing.T, e *env, when string) {
	t.Helper()
	total, newOpen := e.svc.counts()
	rt, rn, err := e.svc.recount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if total != rt || newOpen != rn {
		t.Fatalf("%s: counts (%d, %d), recount (%d, %d)", when, total, newOpen, rt, rn)
	}
}

// After a long downtime a failing lookup on the first pass closes nothing.
func TestLongDowntimeWithFailingLookupsKeepsTheInvoice(t *testing.T) {
	for name, lookupErr := range map[string]error{
		"no response": fmt.Errorf("%w: relay down", nwc.ErrNoResponse),
		"closed":      nwc.ErrClosed,
		"refused":     &nwc.RelayRefusedError{Reason: "x"},
		"context":     context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			o := e.order("a", 4_000_000)
			inv := e.mustPay(o)
			e.closeFiles()
			e.w.settle(inv.PaymentHash, inv.AmountMsat)
			e.clk.Advance(49 * time.Hour)
			e.w.lookupErr = lookupErr
			e.open()
			e.pass()
			e.pass()
			if closures(t, e, o) != 0 || e.eventCount(store.EventExpired, o.ID) != 0 || e.status(o).State != StateNew {
				t.Fatal("an invoice was closed by failing lookups")
			}
			e.w.lookupErr = nil
			e.pass()
			if e.status(o).State != StatePaid || e.transfers(o) != 1 || e.eventCount(store.EventPaid, o.ID) != 1 ||
				e.eventCount(store.EventExpired, o.ID) != 0 {
				t.Fatal("not paid exactly once")
			}
		})
	}
}

// Past the deadline with only transport errors: not closed, no slot, polled slowly; then the wallet
// itself says NOT_FOUND and the invoice is closed with reason deadline.
func TestStaleInvoice(t *testing.T) {
	e := newEnv(t, func(e *env) { e.limCfg.MaxOpen, e.limCfg.NewShare = 2, 0.5 })
	o := e.order("a", 1_000_000)
	inv := e.mustPay(o)
	e.w.lookupErr = fmt.Errorf("%w: relay down", nwc.ErrNoResponse)
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + store.DeadlineGrace)
	e.pass()
	if closures(t, e, o) != 0 || !contains(e.log.String(), logStale) || e.status(o).State != StateNew {
		t.Fatal("stale invoice closed or not reported")
	}
	if total, _ := e.svc.counts(); total != 0 {
		t.Fatalf("a stale invoice still counts: %d", total)
	}
	sameCounts(t, e, "stale")
	// Its slot is free: another account can use the New share.
	e.mustPay(e.order("b", 1_000_000))
	sameCounts(t, e, "after another pay")
	// Slow polling: not again within 10 minutes.
	n := e.w.lookupCount(inv.PaymentHash)
	e.pass()
	e.clk.Advance(staleEvery - time.Second)
	e.pass()
	if e.w.lookupCount(inv.PaymentHash) != n {
		t.Fatal("polled again before the stale period")
	}
	e.clk.Advance(time.Second)
	e.pass()
	if e.w.lookupCount(inv.PaymentHash) != n+1 {
		t.Fatal("not polled after the stale period")
	}
	// The wallet answers: NOT_FOUND closes it with reason deadline, and the order expires once.
	e.w.lookupErr = &nwc.WalletError{Code: "NOT_FOUND", Message: "x"}
	e.clk.Advance(staleEvery)
	e.pass()
	e.pass()
	cl, _ := e.st.ClosuresForOrder(context.Background(), o.ID)
	if len(cl) != 1 || cl[0].Reason != store.CloseDeadline || e.eventCount(store.EventExpired, o.ID) != 1 {
		t.Fatalf("closures %+v", cl)
	}
	sameCounts(t, e, "after close")
}

// A stale invoice whose lookup finally says settled is paid.
func TestStaleInvoiceThenPaid(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	inv := e.mustPay(o)
	e.w.lookupErr = nwc.ErrNoResponse
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + store.DeadlineGrace)
	e.pass()
	e.w.lookupErr = nil
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.clk.Advance(staleEvery)
	e.pass()
	if e.status(o).State != StatePaid || e.transfers(o) != 1 {
		t.Fatal("not paid")
	}
	sameCounts(t, e, "paid")
}

// Run cancelled while a lookup blocks, on an invoice past its deadline: nothing is closed.
func TestRunCancelledMidLookupClosesNothing(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	inv := e.mustPay(o)
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + store.DeadlineGrace + time.Hour)
	e.w.lookupBlock, e.w.lookupEntered = true, make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	<-e.w.lookupEntered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	// The fake answers NOT_FOUND once its context ends, so only the reconciler's context guard keeps
	// it from acting on that (closing would fail on the dead context, but would alert first).
	if closures(t, e, o) != 0 || e.eventCount(store.EventExpired, o.ID) != 0 ||
		contains(e.log.String(), logDeadline) || contains(e.log.String(), logStale) {
		t.Fatal("acted on a lookup that ended with the shutdown")
	}
}

// After an empty round the next one waits: the store is not touched (it is closed here).
func TestIdleRoundIsNotRepeated(t *testing.T) {
	e := newEnv(t)
	e.svc.idle = time.Hour
	var c cursor
	if _, ok, err := e.svc.nextInvoice(context.Background(), &c); ok || err != nil {
		t.Fatalf("empty store: %v %v", ok, err)
	}
	e.closeFiles()
	if _, ok, err := e.svc.nextInvoice(context.Background(), &c); ok || err != nil {
		t.Fatalf("idle round touched the store: %v %v", ok, err)
	}
}

// An indefinite AddInvoice error: the slot follows the truth in the store.
func TestAddInvoiceIndefiniteError(t *testing.T) {
	for _, committed := range []bool{true, false} {
		t.Run(fmt.Sprint("committed ", committed), func(t *testing.T) {
			e := newEnv(t)
			o := e.order("a", 1_000_000)
			e.svc.addInvoice = func(ctx context.Context, inv store.Invoice) error {
				if committed {
					if err := e.st.AddInvoice(ctx, inv); err != nil {
						return err
					}
				}
				return errors.New("hiccup")
			}
			inv, err := e.pay(o)
			stored := len(e.open_(o))
			if committed && (err != nil || stored != 1 || inv.PaymentHash == "") {
				t.Fatalf("committed: %v %d", err, stored)
			}
			if !committed && (!errors.Is(err, ErrUnknown) || stored != 0) {
				t.Fatalf("not committed: %v %d", err, stored)
			}
			sameCounts(t, e, "after the error")
			if total, _ := e.svc.counts(); total != stored {
				t.Fatalf("total %d, stored %d", total, stored)
			}
		})
	}
}

// A presser who gives up does not abandon a counted MakeInvoice.
func TestLeaderCancelledFlightCompletes(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	inFlight := make(chan struct{})
	release := make(chan struct{})
	e.w.before = func(ctx context.Context, p nwc.MakeInvoiceParams) (bool, error) {
		close(inFlight)
		<-release
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { _, err := e.svc.Pay(ctx, o.Token); res <- err }()
	<-inFlight
	cancel()
	if err := <-res; !errors.Is(err, ErrUnknown) {
		t.Fatalf("leader: %v", err)
	}
	close(release)
	eventually(t, "the flight to store the invoice", func() bool { return len(e.open_(o)) == 1 })
	inv := e.mustPay(o)
	if inv.PaymentHash != e.open_(o)[0].PaymentHash || e.w.makeCalls.Load() != 1 {
		t.Fatal("a later press did not get the stored invoice")
	}
	if n, _, _ := e.st.AttemptsSince(context.Background(), epoch); n != 1 {
		t.Fatalf("%d attempts", n)
	}
	sameCounts(t, e, "after")
}

// Before a second invoice is made, an expired but unclosed one is looked up.
func TestPayLooksUpExpiredInvoiceFirst(t *testing.T) {
	setup := func(t *testing.T) (*env, store.Order, store.Invoice) {
		e := newEnv(t)
		o := e.order("a", 3_000_000)
		inv := e.mustPay(o)
		e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + 30*time.Second) // expired, not closed, order still open
		return e, o, inv
	}
	t.Run("paid late", func(t *testing.T) {
		e, o, inv := setup(t)
		e.w.settle(inv.PaymentHash, inv.AmountMsat)
		if _, err := e.pay(o); !errors.Is(err, ErrPaid) {
			t.Fatalf("got %v", err)
		}
		if e.w.makeCalls.Load() != 1 || e.transfers(o) != 1 || e.eventCount(store.EventPaid, o.ID) != 1 || e.status(o).State != StatePaid {
			t.Fatal("not paid exactly once, or a second invoice was made")
		}
		sameCounts(t, e, "paid late")
	})
	t.Run("not settled", func(t *testing.T) {
		e, o, inv := setup(t)
		inv2, err := e.pay(o)
		if err != nil || inv2.PaymentHash == inv.PaymentHash || e.w.makeCalls.Load() != 2 {
			t.Fatalf("second invoice: %v", err)
		}
	})
	t.Run("lookup fails", func(t *testing.T) {
		e, o, _ := setup(t)
		e.w.lookupErr = fmt.Errorf("%w: down", nwc.ErrNoResponse)
		if _, err := e.pay(o); !errors.Is(err, ErrUnknown) {
			t.Fatalf("got %v", err)
		}
		if e.w.makeCalls.Load() != 1 {
			t.Fatal("a second invoice was made without knowing about the first")
		}
		if n, _, _ := e.st.AttemptsSince(context.Background(), epoch); n != 1 {
			t.Fatalf("%d attempts: a refused press must not count", n)
		}
	})
}

// A paid invoice stops holding a slot at once, even before its expiry.
func TestPaidInvoiceHoldsNoSlot(t *testing.T) {
	e := newEnv(t, func(e *env) { e.limCfg.MaxOpen, e.limCfg.NewShare = 3, 0.34 })
	o := e.order("a", 1_000_000)
	inv := e.mustPay(o)
	if total, _ := e.svc.counts(); total != 1 {
		t.Fatal(total)
	}
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass()
	if total, _ := e.svc.counts(); total != 0 {
		t.Fatalf("paid invoice still counts: %d", total)
	}
	sameCounts(t, e, "paid")
	e.restart()
	sameCounts(t, e, "restart")
	if total, _ := e.svc.counts(); total != 0 {
		t.Fatalf("after restart: %d", total)
	}
	// And it is not looked up or written again while it waits for its expiry.
	n := e.w.lookupCount(inv.PaymentHash)
	e.pass()
	e.pass()
	if e.w.lookupCount(inv.PaymentHash) != n {
		t.Fatal("a paid invoice was looked up")
	}
}

type badRowWallet struct{ *fakeWallet }

func (b badRowWallet) LookupInvoice(ctx context.Context, h string) (*nwc.Transaction, error) {
	tx, err := b.fakeWallet.LookupInvoice(ctx, h)
	if tx != nil {
		tx.Type = nwc.TypeOutgoing
	}
	return tx, err
}

// At the deadline only NOT_FOUND is evidence. Everything else leaves the invoice open and stale.
func TestDeadlineClosesOnlyOnNotFound(t *testing.T) {
	cases := map[string]error{
		"INTERNAL":   &nwc.WalletError{Code: "INTERNAL", Message: "x"},
		"RESTRICTED": &nwc.WalletError{Code: "RESTRICTED", Message: "x"},
		"EXPIRED":    &nwc.WalletError{Code: "EXPIRED", Message: "x"},
		"malformed":  fmt.Errorf("%w: bad", nwc.ErrMalformed),
		"bad row":    nil,
	}
	for name, lookupErr := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			o := e.order("a", 1_000_000)
			inv := e.mustPay(o)
			e.w.lookupErr = lookupErr
			if lookupErr == nil {
				e.svc.w = badRowWallet{e.w}
			}
			e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + store.DeadlineGrace)
			e.pass()
			if closures(t, e, o) != 0 || !contains(e.log.String(), logStale) || e.status(o).State != StateNew {
				t.Fatal("closed, or not reported as stale")
			}
			if total, _ := e.svc.counts(); total != 0 {
				t.Fatalf("stale invoice counts: %d", total)
			}
			sameCounts(t, e, "stale")
			// Only NOT_FOUND closes it.
			e.svc.w = e.w
			e.w.lookupErr = &nwc.WalletError{Code: "NOT_FOUND", Message: "x"}
			e.clk.Advance(staleEvery)
			e.pass()
			e.pass()
			cl, _ := e.st.ClosuresForOrder(context.Background(), o.ID)
			if len(cl) != 1 || cl[0].Reason != store.CloseDeadline || e.eventCount(store.EventExpired, o.ID) != 1 {
				t.Fatalf("closures %+v", cl)
			}
		})
	}
}

// The deadline is measured from the service's start when that is later: after 49 h of downtime a
// NOT_FOUND on the first pass closes nothing; 24 h after the start it does.
func TestDeadlineIsMeasuredFromStart(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	e.mustPay(o)
	e.closeFiles()
	e.clk.Advance(49 * time.Hour)
	e.open()
	e.w.lookupErr = &nwc.WalletError{Code: "NOT_FOUND", Message: "x"}
	e.pass()
	e.clk.Advance(store.DeadlineGrace - time.Second)
	e.pass()
	if closures(t, e, o) != 0 {
		t.Fatal("closed before start + 24 h")
	}
	e.clk.Advance(time.Second)
	e.pass()
	cl, _ := e.st.ClosuresForOrder(context.Background(), o.ID)
	if len(cl) != 1 || cl[0].Reason != store.CloseDeadline {
		t.Fatalf("closures %+v", cl)
	}
}

// A notification for a stale invoice is looked up at once, not at the slow poll.
func TestNotificationOvertakesSlowPoll(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	inv := e.mustPay(o)
	e.w.lookupErr = nwc.ErrNoResponse
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + store.DeadlineGrace)
	e.pass() // stale
	e.w.lookupErr = nil
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.svc.prioritise(inv.PaymentHash)
	var c cursor
	got, ok, err := e.svc.nextInvoice(context.Background(), &c)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if looked, err := e.svc.reconcileInv(context.Background(), got); err != nil || !looked {
		t.Fatalf("looked %v err %v", looked, err)
	}
	if e.status(o).State != StatePaid {
		t.Fatal("not looked up at once")
	}
}

// A round in which every entry is skipped (fully recorded, waiting for its expiry) idles.
func TestRoundOfSkippedEntriesIdles(t *testing.T) {
	e := newEnv(t)
	e.svc.idle = time.Hour
	o := e.order("a", 1_000_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass() // recorded; now only waits for its expiry
	var c cursor
	step := func() (bool, bool) {
		got, ok, err := e.svc.nextInvoice(context.Background(), &c)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return false, false
		}
		looked, err := e.svc.reconcileInv(context.Background(), got)
		if err != nil {
			t.Fatal(err)
		}
		e.svc.afterStep(&c, looked)
		return looked, true
	}
	if looked, ran := step(); !ran || looked {
		t.Fatalf("ran %v looked %v", ran, looked)
	}
	e.closeFiles()
	if _, ran := step(); ran {
		t.Fatal("the next round started inside the idle period")
	}
}
