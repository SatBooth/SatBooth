package lnpay

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/store"
)

// makeKnown gives the account a paid order, so that its class is Known.
func makeKnown(t *testing.T, e *env, account string) {
	t.Helper()
	o := e.order(account, 1_000_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass()
	if e.status(o).State != StatePaid {
		t.Fatal("setup order not paid")
	}
}

// Many presses on one order.
func TestPayConcurrentOneOrder(t *testing.T) {
	e := newEnv(t)
	o := e.order("acct", 5_000_000)
	release := make(chan struct{})
	e.w.before = func(ctx context.Context, p nwc.MakeInvoiceParams) (bool, error) {
		<-release
		return false, nil
	}
	const n = 50
	started := make(chan struct{}, n)
	res := make(chan store.Invoice, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			started <- struct{}{}
			inv, err := e.pay(o)
			if err != nil {
				errs <- err
				return
			}
			res <- inv
		}()
	}
	for i := 0; i < n; i++ {
		<-started
	}
	close(release)
	var first store.Invoice
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			t.Fatalf("Pay: %v", err)
		case inv := <-res:
			if i == 0 {
				first = inv
			} else if inv.PaymentHash != first.PaymentHash {
				t.Fatalf("two different invoices: %s %s", inv.PaymentHash, first.PaymentHash)
			}
		}
	}
	if c := e.w.makeCalls.Load(); c != 1 {
		t.Fatalf("MakeInvoice called %d times", c)
	}
	if got := e.open_(o); len(got) != 1 {
		t.Fatalf("%d stored invoices", len(got))
	}
	// A press after it is all over returns the same open invoice.
	if inv := e.mustPay(o); inv.PaymentHash != first.PaymentHash || e.w.makeCalls.Load() != 1 {
		t.Fatal("a later press made a new invoice")
	}
}

// Two orders racing at the cap, including while the first MakeInvoice is in flight.
func TestPayRaceAtCap(t *testing.T) {
	e := newEnv(t, func(e *env) { e.limCfg.MaxOpen, e.limCfg.NewShare = 2, 0.5 }) // the total is 2; Known may use the New slot
	makeKnown(t, e, "k")                                                          // its paid invoice holds no slot
	o1, o2, o3 := e.order("k", 1_000_000), e.order("k", 1_000_000), e.order("k", 1_000_000)
	e.mustPay(o1) // 1 open; the total cap is 2
	inFlight := make(chan struct{})
	release := make(chan struct{})
	e.w.before = func(ctx context.Context, p nwc.MakeInvoiceParams) (bool, error) {
		close(inFlight)
		<-release
		return false, nil
	}
	first := make(chan error, 1)
	go func() { _, err := e.pay(o2); first <- err }()
	<-inFlight
	e.w.before = nil
	_, err := e.pay(o3)
	if !errors.Is(err, limits.ErrOpenCap) {
		t.Fatalf("second order: %v, want ErrOpenCap while the first is in flight", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first order: %v", err)
	}
	if _, err := e.pay(o3); !errors.Is(err, limits.ErrOpenCap) {
		t.Fatalf("third: %v", err)
	}
}

func TestPayRaceManyOrders(t *testing.T) {
	e := newEnv(t, func(e *env) { e.limCfg.MaxOpen, e.limCfg.NewShare = 20, 0.5 }) // New may hold 10
	orders := make([]store.Order, 40)
	for i := range orders {
		orders[i] = e.order(fmt.Sprintf("a%d", i), 1_000_000)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, share := 0, 0
	for _, o := range orders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.pay(o)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, limits.ErrClassShare):
				share++
			default:
				t.Errorf("Pay: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 10 || share != 30 {
		t.Fatalf("ok %d share-refused %d, want 10 and 30", ok, share)
	}
}

// A MakeInvoice that blocks does not delay Pay on another order.
func TestPayBlockedDoesNotDelayOthers(t *testing.T) {
	e := newEnv(t)
	a, b := e.order("a", 1_000_000), e.order("b", 1_000_000)
	inFlight := make(chan struct{})
	release := make(chan struct{})
	e.w.before = func(ctx context.Context, p nwc.MakeInvoiceParams) (bool, error) {
		if strings.HasSuffix(p.Description, a.ID) {
			close(inFlight)
			<-release
		}
		return false, nil
	}
	done := make(chan error, 1)
	go func() { _, err := e.pay(a); done <- err }()
	<-inFlight
	if _, err := e.pay(b); err != nil {
		t.Fatalf("Pay(b) while a is blocked: %v", err)
	}
	if _, err := e.svc.Status(context.Background(), b.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPayRefusals(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Pay(context.Background(), strings.Repeat("0", 32)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown token: %v", err)
	}
	if _, err := e.svc.Pay(context.Background(), "../x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bad token: %v", err)
	}
	// Whole sats only, within bounds.
	for _, msat := range []int64{1_500, 1_000_000_000 + 1000} {
		o := e.order("x", msat)
		if _, err := e.pay(o); !errors.Is(err, limits.ErrAmount) {
			t.Fatalf("amount %d: %v", msat, err)
		}
	}
	if e.w.makeCalls.Load() != 0 {
		t.Fatal("a refused amount reached the wallet")
	}
	if err := e.svc.ValidateAmount(1_000); err != nil {
		t.Fatal(err)
	}
	for _, msat := range []int64{0, -1000, 999, 1_001, 1_000_000_000 + 1000} {
		if err := e.svc.ValidateAmount(msat); !errors.Is(err, limits.ErrAmount) {
			t.Fatalf("ValidateAmount(%d) = %v", msat, err)
		}
	}
	// Paid and expired orders.
	paid := e.order("p", 1_000_000)
	inv := e.mustPay(paid)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass()
	if _, err := e.pay(paid); !errors.Is(err, ErrPaid) {
		t.Fatalf("paid order: %v", err)
	}
	gone := e.order("g", 1_000_000)
	e.clk.Advance(2 * time.Hour)
	if _, err := e.pay(gone); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired order: %v", err)
	}
}

func TestLimitRefusalsAreSentinels(t *testing.T) {
	t.Run("daily count", func(t *testing.T) {
		e := newEnv(t, func(e *env) { e.limCfg.DailyCount = 2 })
		for i := 0; i < 2; i++ {
			e.mustPay(e.order(fmt.Sprintf("a%d", i), 1_000_000))
		}
		before := e.w.makeCalls.Load()
		if _, err := e.pay(e.order("z", 1_000_000)); !errors.Is(err, limits.ErrDailyCount) {
			t.Fatalf("got %v", err)
		}
		if e.w.makeCalls.Load() != before {
			t.Fatal("refused request reached the wallet")
		}
		n, _, _ := e.st.AttemptsSince(context.Background(), limits.DayStart(e.clk.Now()))
		if n != 2 {
			t.Fatalf("a refused request consumed budget: %d attempts", n)
		}
		// Next UTC day: the budget is back (loaded from the store, not remembered).
		e.clk.Advance(24 * time.Hour)
		o := e.order("y", 1_000_000)
		if _, err := e.pay(o); err != nil {
			t.Fatalf("next day: %v", err)
		}
	})
	t.Run("daily amount", func(t *testing.T) {
		e := newEnv(t, func(e *env) { e.limCfg.DailyMsat = 2_500_000 })
		e.mustPay(e.order("a", 1_000_000))
		e.mustPay(e.order("b", 1_000_000))
		if _, err := e.pay(e.order("c", 1_000_000)); !errors.Is(err, limits.ErrDailyAmount) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("rate", func(t *testing.T) {
		e := newEnv(t, func(e *env) { e.limCfg.RatePerSec, e.limCfg.Burst = 1, 1 })
		e.mustPay(e.order("a", 1_000_000))
		if _, err := e.pay(e.order("b", 1_000_000)); !errors.Is(err, limits.ErrRate) {
			t.Fatalf("got %v", err)
		}
		e.clk.Advance(time.Second)
		e.mustPay(e.order("c", 1_000_000))
	})
	t.Run("new class share", func(t *testing.T) {
		e := newEnv(t, func(e *env) { e.limCfg.MaxOpen, e.limCfg.NewShare = 10, 0.2 })
		e.mustPay(e.order("a", 1_000_000))
		e.mustPay(e.order("b", 1_000_000))
		if _, err := e.pay(e.order("c", 1_000_000)); !errors.Is(err, limits.ErrClassShare) {
			t.Fatalf("got %v", err)
		}
	})
}

// Timeouts and outcomes.
func TestPayOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		lose    bool
		want    error
		counted bool
	}{
		{"no response", fmt.Errorf("%w: timeout", nwc.ErrNoResponse), false, ErrUnknown, true},
		{"no response wrapping a context error", fmt.Errorf("%w: %w", nwc.ErrNoResponse, context.DeadlineExceeded), false, ErrUnknown, true},
		{"relay refused", &nwc.RelayRefusedError{Reason: "blocked"}, false, ErrNotCreated, true},
		{"context error, request never registered", context.Canceled, false, ErrNotCreated, true},
		{"deadline exceeded, plain", context.DeadlineExceeded, false, ErrNotCreated, true},
		{"client closed", nwc.ErrClosed, false, ErrUnknown, true},
		{"wallet error", &nwc.WalletError{Code: "INTERNAL", Message: "x"}, false, ErrUnknown, true},
		{"other error", errors.New("boom"), false, ErrUnknown, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			o := e.order("a", 1_000_000)
			e.w.before = func(context.Context, nwc.MakeInvoiceParams) (bool, error) { return false, c.err }
			_, err := e.pay(o)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if errors.Is(err, ErrUnknown) == errors.Is(err, ErrNotCreated) {
				t.Fatal("must be exactly one of the two sentinels")
			}
			if len(e.open_(o)) != 0 {
				t.Fatal("an invoice was stored")
			}
			n, _, _ := e.st.AttemptsSince(context.Background(), epoch)
			if n != 1 {
				t.Fatalf("%d attempts, want 1: a call to the hub counts even when it failed", n)
			}
			// The caller may press again, and the next press makes a new invoice.
			e.w.before = nil
			inv, err := e.pay(o)
			if err != nil || len(e.open_(o)) != 1 || inv.OrderID != o.ID {
				t.Fatalf("second press: %v", err)
			}
		})
	}
}

// The response was lost but the hub made the invoice: the customer never saw it, a new press makes
// another, and nothing is paid twice or lost.
func TestPayLostResponse(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	lose := true
	e.w.before = func(context.Context, nwc.MakeInvoiceParams) (bool, error) { return lose, nil }
	if _, err := e.pay(o); !errors.Is(err, ErrUnknown) {
		t.Fatalf("got %v", err)
	}
	lose = false
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass()
	if e.status(o).State != StatePaid || e.transfers(o) != 1 {
		t.Fatal("not paid exactly once")
	}
}

func TestFiveTimeoutsCountFiveAttempts(t *testing.T) {
	e := newEnv(t, func(e *env) { e.limCfg.DailyCount = 5 })
	e.w.before = func(context.Context, nwc.MakeInvoiceParams) (bool, error) { return false, nwc.ErrNoResponse }
	o := e.order("a", 1_000_000)
	for i := 0; i < 5; i++ {
		if _, err := e.pay(o); !errors.Is(err, ErrUnknown) {
			t.Fatalf("press %d: %v", i, err)
		}
	}
	n, msat, _ := e.st.AttemptsSince(context.Background(), limits.DayStart(e.clk.Now()))
	if n != 5 || msat != 5_000_000 {
		t.Fatalf("attempts %d msat %d", n, msat)
	}
	if _, err := e.pay(o); !errors.Is(err, limits.ErrDailyCount) {
		t.Fatalf("sixth: %v", err)
	}
	// The count survives a restart.
	e.restart()
	if _, err := e.pay(o); !errors.Is(err, limits.ErrDailyCount) {
		t.Fatalf("after restart: %v", err)
	}
}

func TestDescriptionIsOnlyTheOrderID(t *testing.T) {
	e := newEnv(t)
	o := e.order("account-secret-name", 1_000_000)
	e.mustPay(o)
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	if len(e.w.descs) != 1 || e.w.descs[0] != "SatBooth order "+o.ID {
		t.Fatalf("descriptions %q", e.w.descs)
	}
	if contains(e.w.descs[0], "account-secret-name") || contains(e.w.descs[0], "ref-") {
		t.Fatal("description leaks a merchant value")
	}
}

func TestBadWalletResponses(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	for name, mut := range map[string]func(*nwc.Transaction){
		"amount differs": func(tx *nwc.Transaction) { tx.AmountMsat++ },
		"short hash":     func(tx *nwc.Transaction) { tx.PaymentHash = tx.PaymentHash[:60] },
		"non-hex hash":   func(tx *nwc.Transaction) { tx.PaymentHash = strings.Repeat("zz", 32) },
		"bad bolt11":     func(tx *nwc.Transaction) { tx.Invoice = "has space" },
		"empty bolt11":   func(tx *nwc.Transaction) { tx.Invoice = "" },
	} {
		t.Run(name, func(t *testing.T) {
			w := &mutatingWallet{fakeWallet: e.w, mut: mut}
			e.svc.w = w
			if _, err := e.pay(o); !errors.Is(err, ErrUnknown) {
				t.Fatalf("got %v", err)
			}
			if len(e.open_(o)) != 0 {
				t.Fatal("stored")
			}
		})
	}
	e.svc.w = e.w
	// An uppercase hash is lowercased before any store call.
	e.svc.w = &mutatingWallet{fakeWallet: e.w, mut: func(tx *nwc.Transaction) { tx.PaymentHash = strings.ToUpper(tx.PaymentHash) }}
	inv, err := e.pay(o)
	if err != nil || inv.PaymentHash != strings.ToLower(inv.PaymentHash) {
		t.Fatalf("uppercase hash: %v %q", err, inv.PaymentHash)
	}
}

type mutatingWallet struct {
	*fakeWallet
	mut func(*nwc.Transaction)
}

func (m *mutatingWallet) MakeInvoice(ctx context.Context, p nwc.MakeInvoiceParams) (*nwc.Transaction, error) {
	tx, err := m.fakeWallet.MakeInvoice(ctx, p)
	if tx != nil {
		m.mut(tx)
	}
	return tx, err
}

// The status derived from the store, the ledger and the clock.
func TestStatus(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	if s := e.status(o); s.State != StateNew || s.Invoice != nil {
		t.Fatalf("fresh: %+v", s)
	}
	inv := e.mustPay(o)
	if s := e.status(o); s.State != StateNew || s.Invoice == nil || s.Invoice.PaymentHash != inv.PaymentHash {
		t.Fatalf("with invoice: %+v", s)
	}
	// The invoice expires (store boundary: expires_at <= now is past).
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()))
	if s := e.status(o); s.Invoice != nil || s.State != StateNew {
		t.Fatalf("at the invoice expiry: %+v", s)
	}
	// Past the order's expiry but the invoice is still open (no lookup yet): never expired.
	e.clk.Advance(time.Hour)
	if s := e.status(o); s.State != StateNew {
		t.Fatalf("expired with an open invoice: %+v", s)
	}
	if _, err := e.svc.Status(context.Background(), strings.Repeat("1", 32)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	e.pass() // too soon after the invoice's expiry? Grace has passed (1 h later): closes it, order expires.
	if s := e.status(o); s.State != StateExpired {
		t.Fatalf("after close: %+v", s)
	}
	if e.eventCount(store.EventExpired, o.ID) != 1 {
		t.Fatal("no order.expired event")
	}
}

func TestOrderWithoutInvoiceExpires(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	e.clk.Advance(time.Hour)
	if s := e.status(o); s.State != StateExpired {
		t.Fatalf("%+v", s)
	}
	e.pass()
	e.pass()
	if e.eventCount(store.EventExpired, o.ID) != 1 {
		t.Fatal("expired event missing or duplicated")
	}
}

// The close needs a lookup after ExpiresAt + Grace.
func TestGraceBeforeClose(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	inv := e.mustPay(o)
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now())) // at ExpiresAt exactly
	e.pass()
	if got := closures(t, e, o); got != 0 {
		t.Fatal("closed before the grace period")
	}
	e.clk.Advance(DefaultGrace - time.Second)
	e.pass()
	if got := closures(t, e, o); got != 0 {
		t.Fatal("closed before the grace period ended")
	}
	e.clk.Advance(time.Second)
	e.pass()
	cl, _ := e.st.ClosuresForOrder(context.Background(), o.ID)
	if len(cl) != 1 || cl[0].Reason != store.CloseLookup {
		t.Fatalf("closures %+v", cl)
	}
}

func closures(t *testing.T, e *env, o store.Order) int {
	cl, err := e.st.ClosuresForOrder(context.Background(), o.ID)
	if err != nil {
		t.Fatal(err)
	}
	return len(cl)
}

// A late payment inside the grace period is still recorded; an order past its expiry is paid.
func TestLatePaymentInGrace(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	inv := e.mustPay(o)
	e.clk.Advance(time.Hour + time.Minute) // order and invoice both past; grace is 2 min from the invoice expiry
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass()
	if s := e.status(o); s.State != StatePaid || s.PaidAt.IsZero() {
		t.Fatalf("%+v", s)
	}
	if e.eventCount(store.EventExpired, o.ID) != 0 || e.eventCount(store.EventPaid, o.ID) != 1 {
		t.Fatal("events")
	}
}

// A lookup that keeps failing closes with reason deadline at ExpiresAt + 24 h.
func TestDeadlineClose(t *testing.T) {
	e := newEnv(t, func(e *env) { e.limCfg.MaxOpen, e.limCfg.NewShare = 2, 0.5 })
	o := e.order("a", 1_000_000)
	inv := e.mustPay(o)
	e.w.lookupErr = &nwc.WalletError{Code: "NOT_FOUND", Message: "nope"}
	if _, err := e.pay(e.order("b", 1_000_000)); !errors.Is(err, limits.ErrOpenCap) && !errors.Is(err, limits.ErrClassShare) {
		t.Fatalf("cap not reached: %v", err)
	}
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + 24*time.Hour - time.Second)
	e.pass()
	if closures(t, e, o) != 0 || contains(e.log.String(), logDeadline) {
		t.Fatal("closed before the deadline")
	}
	e.clk.Advance(time.Second)
	e.pass()
	cl, _ := e.st.ClosuresForOrder(context.Background(), o.ID)
	if len(cl) != 1 || cl[0].Reason != store.CloseDeadline {
		t.Fatalf("closures %+v", cl)
	}
	if !contains(e.log.String(), logDeadline) {
		t.Fatal("no alert line")
	}
	if s := e.status(o); s.State != StateExpired {
		t.Fatalf("%+v", s)
	}
	total, _ := e.svc.counts()
	if total != 0 {
		t.Fatalf("closed invoice still counts: %d", total)
	}
	// The slot is free again.
	e.w.lookupErr = nil
	e.clk.Advance(0)
	if _, err := e.pay(e.order("c", 1_000_000)); err != nil {
		t.Fatalf("after the close: %v", err)
	}
}

func TestLookupErrorLogIsFixed(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	e.mustPay(o)
	e.w.lookupErr = errors.New("secret-looking-detail xyz")
	e.pass()
	if contains(e.log.String(), "secret-looking-detail") || !contains(e.log.String(), logLookupFailed) {
		t.Fatalf("log: %s", e.log.String())
	}
}

func TestShortSettlementNotPaid(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 2_000_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat-1000)
	e.pass()
	e.pass()
	if e.transfers(o) != 0 || e.status(o).State == StatePaid {
		t.Fatal("a short payment was recorded")
	}
	if n := strings.Count(e.log.String(), logShortSettle); n != 1 {
		t.Fatalf("alert logged %d times, want once per invoice", n)
	}
	// Exactly the amount, and more, are paid; more records what was settled.
	o2 := e.order("b", 2_000_000)
	inv2 := e.mustPay(o2)
	e.w.settle(inv2.PaymentHash, inv2.AmountMsat+5000)
	e.pass()
	if e.merchantCredits() != 2_005_000 {
		t.Fatalf("credits %d", e.merchantCredits())
	}
}

// Found twice.
func TestPaidExactlyOnce(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 3_000_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	for i := 0; i < 10; i++ {
		e.w.notify(inv.PaymentHash)
	}
	eventually(t, "the order to be paid", func() bool { return e.eventCount(store.EventPaid, o.ID) == 1 })
	eventually(t, "the notifications to be consumed", func() bool { return len(e.w.notifs) == 0 })
	e.pass()
	for i := 0; i < 10; i++ {
		e.w.notify(inv.PaymentHash)
	}
	eventually(t, "the replay to be consumed", func() bool { return len(e.w.notifs) == 0 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	if e.transfers(o) != 1 || e.merchantCredits() != 3_000_000 {
		t.Fatalf("transfers %d credits %d", e.transfers(o), e.merchantCredits())
	}
	if e.eventCount(store.EventPaid, o.ID) != 1 || e.eventCount(store.EventExpired, o.ID) != 0 {
		t.Fatal("events")
	}
}

func TestPaymentRecordedConcurrentlyOnce(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 3_000_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = e.svc.pass(context.Background()) }()
	}
	wg.Wait()
	if e.transfers(o) != 1 || e.merchantCredits() != 3_000_000 || e.eventCount(store.EventPaid, o.ID) != 1 {
		t.Fatal("not exactly once")
	}
}

// A notification for a hash that is not ours, or is malformed, is ignored.
func TestForeignNotificationsIgnored(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	e.mustPay(o)
	for _, h := range []string{"", "xyz", strings.Repeat("ab", 32), strings.Repeat("AB", 32)} {
		e.svc.prioritise(h)
	}
	e.svc.mu.Lock()
	n := len(e.svc.front)
	e.svc.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d foreign hashes queued", n)
	}
}

// A notification moves its invoice to the front of the queue; nothing else does.
func TestNotificationMovesToFront(t *testing.T) {
	e := newEnv(t)
	var hashes []string
	for i := 0; i < 3; i++ {
		hashes = append(hashes, e.mustPay(e.order(fmt.Sprintf("a%d", i), 1_000_000)).PaymentHash)
		e.clk.Advance(time.Second) // distinct creation times: the snapshot order is oldest first
	}
	var c cursor
	got := func() string {
		inv, ok, err := e.svc.nextInvoice(context.Background(), &c)
		if err != nil || !ok {
			t.Fatalf("next: %v %v", ok, err)
		}
		return inv.PaymentHash
	}
	e.svc.prioritise(strings.ToUpper(hashes[2])) // the hub's case does not matter
	e.svc.prioritise(hashes[2])
	if g := got(); g != hashes[2] {
		t.Fatalf("front = %s", g)
	}
	for i := 0; i < 3; i++ {
		if g := got(); g != hashes[i] {
			t.Fatalf("round robin %d = %s", i, g)
		}
	}
}

// The payment settles while the service is down, for longer than every deadline.
func TestPaymentWhileDown(t *testing.T) {
	for _, downtime := range []time.Duration{3 * time.Hour, 49 * time.Hour, 24 * 400 * time.Hour} {
		t.Run(downtime.String(), func(t *testing.T) {
			e := newEnv(t)
			o := e.order("a", 4_000_000)
			inv := e.mustPay(o)
			// "Down": no Run, the files are closed. The payment settles at the hub meanwhile.
			e.closeFiles()
			e.w.settle(inv.PaymentHash, inv.AmountMsat)
			e.clk.Advance(downtime)
			e.open()
			// Status is never expired while the payment could still be recorded.
			if s := e.status(o); s.State != StateNew {
				t.Fatalf("status before the first pass: %+v", s)
			}
			e.pass()
			if s := e.status(o); s.State != StatePaid {
				t.Fatalf("status after the first pass: %+v", s)
			}
			if e.transfers(o) != 1 || e.eventCount(store.EventPaid, o.ID) != 1 || e.eventCount(store.EventExpired, o.ID) != 0 {
				t.Fatal("not recorded exactly once")
			}
		})
	}
}

// While the relay is down the lookups fail: the order stays new for any length of time short of the
// deadline, and the payment is found when the lookups work again.
func TestPaymentWhileRelayDown(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 4_000_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.w.lookupErr = fmt.Errorf("%w: relay down", nwc.ErrNoResponse)
	e.clk.Advance(5 * time.Hour)
	e.pass()
	if s := e.status(o); s.State != StateNew {
		t.Fatalf("%+v", s)
	}
	e.w.lookupErr = nil
	e.pass()
	if s := e.status(o); s.State != StatePaid {
		t.Fatalf("%+v", s)
	}
}

// Restart at every step: no payment lost, nothing paid twice.
func TestCrashAtEveryStep(t *testing.T) {
	errCrash := errors.New("crash")
	// Crash points while recording a payment.
	for _, point := range []string{crashAfterLookup, crashAfterTransfer, crashAfterEvent} {
		t.Run(point, func(t *testing.T) {
			e := newEnv(t)
			o := e.order("a", 4_000_000)
			inv := e.mustPay(o)
			e.w.settle(inv.PaymentHash, inv.AmountMsat)
			e.svc.hook = func(p string) error {
				if p == point {
					return errCrash
				}
				return nil
			}
			if err := e.svc.pass(context.Background()); !errors.Is(err, errCrash) {
				t.Fatalf("pass: %v", err)
			}
			e.restart()
			e.pass()
			e.pass()
			if e.transfers(o) != 1 || e.merchantCredits() != 4_000_000 ||
				e.eventCount(store.EventPaid, o.ID) != 1 || e.status(o).State != StatePaid {
				t.Fatalf("transfers %d credits %d", e.transfers(o), e.merchantCredits())
			}
		})
	}
	// Crash points while making an invoice.
	for _, point := range []string{crashAfterAttempt, crashAfterMake} {
		t.Run(point, func(t *testing.T) {
			e := newEnv(t)
			o := e.order("a", 4_000_000)
			e.svc.hook = func(p string) error {
				if p == point {
					return errCrash
				}
				return nil
			}
			if _, err := e.pay(o); !errors.Is(err, errCrash) {
				t.Fatalf("pay: %v", err)
			}
			e.restart()
			n, _, _ := e.st.AttemptsSince(context.Background(), epoch)
			if n != 1 {
				t.Fatalf("attempt not durable: %d", n)
			}
			inv := e.mustPay(o)
			e.w.settle(inv.PaymentHash, inv.AmountMsat)
			e.pass()
			if e.transfers(o) != 1 || e.status(o).State != StatePaid {
				t.Fatal("not paid")
			}
		})
	}
	// Crash while closing: after the close, before the expired event.
	t.Run(crashAfterClose, func(t *testing.T) {
		e := newEnv(t)
		o := e.order("a", 4_000_000)
		e.mustPay(o)
		e.clk.Advance(3 * time.Hour)
		e.svc.hook = func(p string) error {
			if p == crashAfterClose {
				return errCrash
			}
			return nil
		}
		if err := e.svc.pass(context.Background()); !errors.Is(err, errCrash) {
			t.Fatalf("pass: %v", err)
		}
		e.restart()
		e.pass()
		e.pass()
		if e.eventCount(store.EventExpired, o.ID) != 1 || e.status(o).State != StateExpired || e.transfers(o) != 0 {
			t.Fatal("not expired exactly once")
		}
	})
}

// A payment after a restart for an order whose event is missing but whose transfer exists is
// completed without a second transfer, also when the invoice is already closed.
func TestEventHealedForClosedPaidInvoice(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 4_000_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.svc.hook = func(p string) error {
		if p == crashAfterTransfer {
			return errors.New("crash")
		}
		return nil
	}
	_ = e.svc.pass(context.Background())
	e.restart()
	e.clk.Advance(3 * time.Hour)
	e.pass()
	e.pass()
	if e.eventCount(store.EventPaid, o.ID) != 1 || e.eventCount(store.EventExpired, o.ID) != 0 || e.transfers(o) != 1 {
		t.Fatal("not healed")
	}
}

// Known class: an account with a paid order is Known and no longer holds a New slot.
func TestKnownClass(t *testing.T) {
	e := newEnv(t, func(e *env) { e.limCfg.MaxOpen, e.limCfg.NewShare = 10, 0.2 }) // New may hold 2
	n1 := e.order("n1", 1_000_000)
	inv1 := e.mustPay(n1)
	e.mustPay(e.order("n2", 1_000_000))
	if _, err := e.pay(e.order("n3", 1_000_000)); !errors.Is(err, limits.ErrClassShare) {
		t.Fatalf("a third new account: %v", err)
	}
	e.w.settle(inv1.PaymentHash, inv1.AmountMsat)
	e.pass()
	if _, nw := e.svc.counts(); nw != 1 {
		t.Fatalf("new-class count %d, want 1 after n1 became Known", nw)
	}
	e.mustPay(e.order("n3", 1_000_000)) // the freed New slot
	if _, err := e.pay(e.order("n4", 1_000_000)); !errors.Is(err, limits.ErrClassShare) {
		t.Fatalf("n4: %v", err)
	}
	e.mustPay(e.order("n1", 1_000_000)) // Known: not limited by the New share
	e.restart()                         // the class is derived from the ledger, not remembered
	e.mustPay(e.order("n1", 1_000_000))
}

// The in-memory counts equal a recount from the store after a randomized sequence.
func TestCountsMatchRecount(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			e := newEnv(t, func(e *env) { e.limCfg.MaxOpen, e.limCfg.NewShare = 12, 0.5 })
			var orders []store.Order
			check := func(step string) {
				t.Helper()
				total, newOpen := e.svc.counts()
				rt, rn, err := e.svc.recount(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if total != rt || newOpen != rn {
					t.Fatalf("%s: counts (%d, %d), recount (%d, %d)", step, total, newOpen, rt, rn)
				}
				sameAcct(t, e, step) // the per-account cap reads byAcct
			}
			for i := 0; i < 60; i++ {
				switch rng.Intn(6) {
				case 0, 1:
					orders = append(orders, e.order(fmt.Sprintf("acct%d", rng.Intn(4)), 1_000_000))
				case 2, 3:
					if len(orders) > 0 {
						o := orders[rng.Intn(len(orders))]
						e.w.before = func(context.Context, nwc.MakeInvoiceParams) (bool, error) {
							if rng.Intn(5) == 0 {
								return false, nwc.ErrNoResponse
							}
							return false, nil
						}
						mode := rng.Intn(8) // a store hiccup on AddInvoice: committed, or not
						e.svc.addInvoice = func(ctx context.Context, inv store.Invoice) error {
							switch mode {
							case 0:
								_ = e.st.AddInvoice(ctx, inv)
								return errors.New("hiccup")
							case 1:
								return errors.New("hiccup")
							}
							return e.st.AddInvoice(ctx, inv)
						}
						_, _ = e.pay(o)
					}
				case 4:
					if len(orders) > 0 {
						o := orders[rng.Intn(len(orders))]
						for _, inv := range e.open_(o) {
							if rng.Intn(2) == 0 {
								e.w.settle(inv.PaymentHash, inv.AmountMsat)
							}
						}
					}
				case 5:
					e.clk.Advance(time.Duration(rng.Intn(50)) * time.Minute)
					e.pass()
				}
				check(fmt.Sprintf("step %d", i))
			}
			e.restart()
			check("after restart")
		})
	}
}

func TestConfigValidation(t *testing.T) {
	e := newEnv(t)
	lim, _ := limits.New(e.limCfg, e.clk.Now)
	for name, cfg := range map[string]Config{
		"expiry too short": {InvoiceExpiry: 14 * time.Minute},
		"expiry too long":  {InvoiceExpiry: 31 * time.Minute},
		"negative rate":    {PollRate: -1},
		"negative grace":   {Grace: -time.Second},
	} {
		if _, err := New(cfg, e.st, e.lg, e.w, lim, e.clk.Now, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := New(Config{}, e.st, e.lg, e.w, lim, e.clk.Now, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{}, nil, e.lg, e.w, lim, e.clk.Now, nil); err == nil {
		t.Error("nil store accepted")
	}
}

func TestPaidTransferIDIsStableAndDistinct(t *testing.T) {
	a := PaidTransferID(strings.Repeat("ab", 32))
	b := PaidTransferID(strings.Repeat("ac", 32))
	if a == b || a.IsZero() || a.IsMax() || PaidTransferID(strings.Repeat("ab", 32)) != a {
		t.Fatal("derivation")
	}
	// Pinned vector: the first 16 bytes of SHA-256("satbooth/ln-paid/v1" || the 32 raw hash bytes).
	if got := a.String(); got != pinnedID {
		t.Fatalf("id %s", got)
	}
}

func TestRunTwiceRefused(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	eventually(t, "Run to start", func() bool { return e.svc.running.Load() })
	if err := e.svc.Run(context.Background()); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Run: %v", err)
	}
	cancel()
	<-done
}

const pinnedID = "122667547909701342706400077350042540450"

// An order never reads expired, and no expiry event is added, while a Pay for it is in flight: the
// invoice it is about to store could still be paid.
func TestExpiryHeldBackWhilePayInFlight(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	inFlight := make(chan struct{})
	release := make(chan struct{})
	e.w.before = func(context.Context, nwc.MakeInvoiceParams) (bool, error) {
		close(inFlight)
		<-release
		return false, nil
	}
	done := make(chan error, 1)
	go func() { _, err := e.pay(o); done <- err }()
	<-inFlight
	e.clk.Advance(2 * time.Hour) // past the order's expiry while MakeInvoice is out
	if s := e.status(o); s.State != StateNew {
		t.Fatalf("status during the flight: %+v", s)
	}
	e.pass()
	if e.eventCount(store.EventExpired, o.ID) != 0 {
		t.Fatal("expired event added during the flight")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s := e.status(o); s.State != StateNew || s.Invoice == nil {
		t.Fatalf("status after: %+v", s)
	}
	e.pass()
	if e.eventCount(store.EventExpired, o.ID) != 0 {
		t.Fatal("expired with an open invoice")
	}
}

func TestPaidTransferIDProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		raw := make([]byte, 32)
		rng.Read(raw)
		h := fmt.Sprintf("%x", raw)
		id := PaidTransferID(h)
		if id.IsZero() || id.IsMax() || PaidTransferID(strings.ToUpper(h)) != id || seen[id.String()] {
			t.Fatalf("hash %s: id %s", h, id)
		}
		seen[id.String()] = true
		for _, bad := range []string{h[:63], h + "0", "", h[:62] + "zz", " " + h[1:], h[:32] + "é" + h[34:]} {
			if !PaidTransferID(bad).IsZero() {
				t.Fatalf("bad hash %q accepted", bad)
			}
		}
	}
}

func FuzzPaidTransferID(f *testing.F) {
	f.Add(strings.Repeat("ab", 32))
	f.Add("")
	f.Fuzz(func(t *testing.T, h string) {
		id := PaidTransferID(h)
		if validHash(strings.ToLower(h)) && len(h) == 64 {
			if id.IsZero() || PaidTransferID(strings.ToLower(h)) != id {
				t.Fatalf("valid hash %q gave %v", h, id)
			}
		}
		if len(h) != 64 && !id.IsZero() {
			t.Fatalf("bad length accepted: %q", h)
		}
	})
}
