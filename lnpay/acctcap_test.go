package lnpay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/store"
)

// The per-account cap is checked and counted with the creation under the service mutex: 20
// concurrent presses on 6 orders of one account, the wallet answering slowly, make exactly 2
// invoices; the rest are refused with ErrAccountCap and cost no attempt. A second account gets its
// own 2.
func TestAccountCapAtomic(t *testing.T) {
	e := newEnv(t, func(e *env) { e.limCfg.MaxOpenPerAccount = 2 })
	orders := make([]store.Order, 6)
	for i := range orders {
		orders[i] = e.order("a", 1_000_000)
	}
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	e.w.before = func(ctx context.Context, p nwc.MakeInvoiceParams) (bool, error) {
		select { // slow: both admitted flights hold their reserved slot meanwhile
		case <-release:
		case <-ctx.Done():
		}
		return false, nil
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(func() { open(); wg.Wait() }) // a failure below must not leave a press blocked at the wallet
	var mu sync.Mutex
	ok, capped := 0, 0
	for i := 0; i < 20; i++ {
		o := orders[i%len(orders)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := e.pay(o)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, limits.ErrAccountCap):
				capped++
			default:
				t.Errorf("Pay: %v", err)
			}
		}()
	}
	close(start)
	eventually(t, "two flights at the wallet", func() bool { return e.w.makeCalls.Load() >= 2 })
	// Both slots are reserved while the wallet is still answering: another press is refused now.
	// (Bounded: were it admitted, its flight would wait at the wallet too.)
	pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := e.svc.Pay(pctx, e.order("a", 1_000_000).Token)
	cancel()
	if !errors.Is(err, limits.ErrAccountCap) {
		open()
		t.Fatalf("a press while both flights are at the wallet: %v", err)
	}
	if got := e.svc.countsAcct()["a"]; got != 2 {
		open()
		t.Fatalf("reserved %d, want 2", got)
	}
	open()
	wg.Wait()
	if e.w.makeCalls.Load() != 2 || ok+capped != 20 || capped == 0 || ok == 0 {
		t.Fatalf("make calls %d, ok %d, capped %d", e.w.makeCalls.Load(), ok, capped)
	}
	stored := 0
	for _, o := range orders {
		n := len(e.open_(o))
		if n > 1 {
			t.Fatalf("order %s has %d invoices", o.ID, n)
		}
		stored += n
	}
	if stored != 2 {
		t.Fatalf("%d invoices stored, want 2", stored)
	}
	if n, _, err := e.st.AttemptsSince(context.Background(), limits.DayStart(e.clk.Now())); err != nil || n != 2 {
		t.Fatalf("attempts %d (%v), want 2: a refusal costs no attempt", n, err)
	}
	if got := sameAcct(t, e, "after the wave"); got["a"] != 2 || len(got) != 1 {
		t.Fatalf("byAcct %v", got)
	}
	sameCounts(t, e, "after the wave")
	// The cap is per account: b gets its own 2.
	e.w.before = nil
	e.mustPay(e.order("b", 1_000_000))
	e.mustPay(e.order("b", 1_000_000))
	if _, err := e.pay(e.order("b", 1_000_000)); !errors.Is(err, limits.ErrAccountCap) {
		t.Fatalf("b's third: %v", err)
	}
	if got := sameAcct(t, e, "b"); got["a"] != 2 || got["b"] != 2 {
		t.Fatalf("byAcct %v", got)
	}
	// A press on an order that already has its invoice returns it: no new slot is needed.
	for _, o := range orders {
		if len(e.open_(o)) == 1 {
			if _, err := e.pay(o); err != nil {
				t.Fatalf("press on an order with its invoice: %v", err)
			}
		}
	}
	if e.w.makeCalls.Load() != 4 {
		t.Fatalf("make calls %d, want 4", e.w.makeCalls.Load())
	}
}

// An account at its cap gets a slot back when an invoice is paid, closed by the lookup
// rule, or goes stale past its deadline; a restart derives the same per-account counts.
func TestAccountCapReleases(t *testing.T) {
	e := newEnv(t, func(e *env) { e.limCfg.MaxOpenPerAccount = 2 })
	atCap := func(when string) {
		t.Helper()
		if _, err := e.pay(e.order("a", 1_000_000)); !errors.Is(err, limits.ErrAccountCap) {
			t.Fatalf("%s: %v, want ErrAccountCap", when, err)
		}
		if got := sameAcct(t, e, when); got["a"] != 2 {
			t.Fatalf("%s: byAcct %v", when, got)
		}
		sameCounts(t, e, when)
	}
	o1 := e.order("a", 1_000_000)
	i1 := e.mustPay(o1) // expires t0 + 20 min
	e.clk.Advance(10 * time.Minute)
	o2, o3, o4 := e.order("a", 1_000_000), e.order("a", 1_000_000), e.order("a", 1_000_000) // open until t0 + 70 min
	i2 := e.mustPay(o2)                                                                     // expires t0 + 30 min
	atCap("two open")
	// o3 itself is refused at the cap; it is pressed again after the release below.
	if _, err := e.pay(o3); !errors.Is(err, limits.ErrAccountCap) || len(e.open_(o3)) != 0 {
		t.Fatalf("o3 at the cap: %v", err)
	}

	// Paid: the settlement frees its slot.
	e.w.settle(i2.PaymentHash, i2.AmountMsat)
	e.pass()
	if e.status(o2).State != StatePaid {
		t.Fatal("o2 not paid")
	}
	if got := sameAcct(t, e, "paid"); got["a"] != 1 {
		t.Fatalf("after the payment: byAcct %v", got)
	}
	i3 := e.mustPay(o3) // the refused order, pressed again after the release: it gets its invoice; expires t0 + 30 min
	atCap("after the payment")

	// Closed by the lookup rule: i1 is past its expiry and the grace; i3 is not.
	e.clk.Advance(i1.ExpiresAt.Add(DefaultGrace + time.Minute).Sub(e.clk.Now()))
	e.pass()
	if closures(t, e, o1) != 1 || closures(t, e, o3) != 0 {
		t.Fatalf("closures o1 %d o3 %d", closures(t, e, o1), closures(t, e, o3))
	}
	if got := sameAcct(t, e, "closed"); got["a"] != 1 {
		t.Fatalf("after the close: byAcct %v", got)
	}
	i4 := e.mustPay(o4) // expires 20 min from now, after i3
	atCap("after the close")

	// Stale: the lookups fail, and i3 reaches its deadline (ExpiresAt + 24 h) before i4 does.
	e.w.lookupErr = fmt.Errorf("%w: relay down", nwc.ErrNoResponse)
	e.clk.Advance(i3.ExpiresAt.Add(store.DeadlineGrace).Sub(e.clk.Now()))
	e.pass()
	if !contains(e.log.String(), logStale) || closures(t, e, o3) != 0 {
		t.Fatalf("i3 not stale, or closed:\n%s", e.log.String())
	}
	if got := sameAcct(t, e, "stale"); got["a"] != 1 {
		t.Fatalf("after the stale deadline: byAcct %v", got)
	}
	if !e.clk.Now().Before(i4.ExpiresAt.Add(store.DeadlineGrace)) {
		t.Fatal("setup: i4 is past its deadline too")
	}
	e.mustPay(e.order("a", 1_000_000)) // the earlier orders have expired: a new one
	atCap("after the stale deadline")
	// Another account is not held by a's cap.
	e.mustPay(e.order("b", 1_000_000))

	// A restart derives the same per-account counts from the store, and a is still at its cap.
	before := e.svc.countsAcct()
	e.restart()
	if after := sameAcct(t, e, "restart"); after["a"] != before["a"] || after["b"] != before["b"] || len(after) != len(before) {
		t.Fatalf("restart: byAcct %v, before %v", after, before)
	}
	atCap("after the restart")
}
