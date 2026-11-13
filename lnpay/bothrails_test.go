package lnpay

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
)

// paidHook records the orders the Lightning-paid hook was called with; fail, when set, is its answer.
type paidHook struct {
	mu    sync.Mutex
	calls []store.Order
	fail  error
}

func (h *paidHook) call(_ context.Context, o store.Order) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, o)
	return h.fail
}

func (h *paidHook) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func (h *paidHook) setFail(err error) {
	h.mu.Lock()
	h.fail = err
	h.mu.Unlock()
}

// allFor reports whether every call named the order with its on-chain amount.
func (h *paidHook) allFor(o store.Order) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.calls {
		if c.ID != o.ID || c.AmountSat != o.AmountSat || c.AccountID != o.AccountID {
			return false
		}
	}
	return true
}

func TestLightningPaidHookOnMixedOrder(t *testing.T) {
	e := newEnv(t)
	h := &paidHook{}
	e.svc.SetOnLightningPaid(h.call)
	o := e.orderSat("mixed", 2_000_000, 50_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass()
	if h.count() != 1 || !h.allFor(o) {
		t.Fatalf("hook calls %d, want 1 with the order", h.count())
	}
	e.pass()
	e.pass()
	if h.count() != 1 || e.eventCount(store.EventPaid, o.ID) != 1 {
		t.Fatalf("after more passes: hook calls %d, order.paid %d", h.count(), e.eventCount(store.EventPaid, o.ID))
	}
}

func TestLightningPaidHookNotForLightningOnly(t *testing.T) {
	e := newEnv(t)
	h := &paidHook{}
	e.svc.SetOnLightningPaid(h.call)
	o := e.order("ln-only", 2_000_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass()
	e.pass()
	if h.count() != 0 || e.eventCount(store.EventPaid, o.ID) != 1 {
		t.Fatalf("hook calls %d, order.paid %d", h.count(), e.eventCount(store.EventPaid, o.ID))
	}
}

// A nil hook clears it: the completion runs as without one.
func TestLightningPaidHookCleared(t *testing.T) {
	e := newEnv(t)
	h := &paidHook{}
	e.svc.SetOnLightningPaid(h.call)
	e.svc.SetOnLightningPaid(nil)
	o := e.orderSat("mixed", 2_000_000, 50_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass()
	if h.count() != 0 || e.eventCount(store.EventPaid, o.ID) != 1 {
		t.Fatalf("hook calls %d, order.paid %d", h.count(), e.eventCount(store.EventPaid, o.ID))
	}
}

// The hook's error is completePaid's: the invoice keeps its slot (not released, not done), and the next pass calls
// the hook again and completes.
func TestLightningPaidHookErrorIsRetried(t *testing.T) {
	e := newEnv(t)
	errHook := errors.New("hook failed")
	h := &paidHook{fail: errHook}
	e.svc.SetOnLightningPaid(h.call)
	o := e.orderSat("mixed", 2_000_000, 50_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	if total, _ := e.svc.counts(); total != 1 {
		t.Fatalf("setup: total %d", total)
	}
	if err := e.svc.pass(context.Background()); !errors.Is(err, errHook) {
		t.Fatalf("pass: %v, want the hook's error", err)
	}
	if total, _ := e.svc.counts(); total != 1 || h.count() != 1 {
		t.Fatalf("after the failed hook: total %d, hook calls %d", total, h.count())
	}
	e.svc.mu.Lock()
	oi := e.svc.open[inv.PaymentHash]
	e.svc.mu.Unlock()
	if oi.released || oi.done {
		t.Fatalf("released or done after the hook failed: %+v", oi)
	}
	h.setFail(nil)
	e.pass()
	if total, _ := e.svc.counts(); total != 0 || h.count() != 2 || !h.allFor(o) {
		t.Fatalf("after the retry: total %d, hook calls %d", total, h.count())
	}
	e.pass()
	if h.count() != 2 || e.eventCount(store.EventPaid, o.ID) != 1 || e.transfers(o) != 1 || e.status(o).State != StatePaid {
		t.Fatalf("hook calls %d, order.paid %d, transfers %d", h.count(), e.eventCount(store.EventPaid, o.ID), e.transfers(o))
	}
}

// lnpay replaying its own completion reaches the hook with the order, and only that
// often (onchain's LightningPaid alerts only on an on-chain record, TestLightningPaidWithoutAnOnchainRecordIsSilent).
func TestReplayCallsTheHookWithTheOrder(t *testing.T) {
	errCrash := errors.New("crash")
	crashAt := func(e *env, point string) {
		e.svc.hook = func(p string) error {
			if p == point {
				return errCrash
			}
			return nil
		}
	}
	// A crash between order.paid and the release, a restart (New on the same files) and passes: order.paid once,
	// the hook called with the order (a second call is allowed and harmless).
	t.Run("pass", func(t *testing.T) {
		e := newEnv(t)
		h := &paidHook{}
		e.svc.SetOnLightningPaid(h.call)
		o := e.orderSat("mixed", 2_000_000, 50_000)
		inv := e.mustPay(o)
		e.w.settle(inv.PaymentHash, inv.AmountMsat)
		crashAt(e, crashAfterEvent)
		if err := e.svc.pass(context.Background()); !errors.Is(err, errCrash) {
			t.Fatalf("pass: %v", err)
		}
		if h.count() != 0 || e.eventCount(store.EventPaid, o.ID) != 1 {
			t.Fatalf("before the restart: hook calls %d, order.paid %d", h.count(), e.eventCount(store.EventPaid, o.ID))
		}
		e.restart()
		e.svc.SetOnLightningPaid(h.call)
		e.pass()
		e.pass()
		if n := h.count(); n < 1 || n > 2 || !h.allFor(o) || e.eventCount(store.EventPaid, o.ID) != 1 {
			t.Fatalf("after the replay: hook calls %d, order.paid %d", n, e.eventCount(store.EventPaid, o.ID))
		}
		if e.transfers(o) != 1 || e.status(o).State != StatePaid {
			t.Fatalf("transfers %d, state %s", e.transfers(o), e.status(o).State)
		}
	})
	// A crash after the transfer, before any completion; the invoice is then closed past its expiry, so only the
	// startup completion (healPaid) finds it: it calls the hook once with the order, and a second healPaid adds
	// nothing.
	t.Run("healPaid", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		h := &paidHook{}
		e.svc.SetOnLightningPaid(h.call)
		o := e.orderSat("mixed", 2_000_000, 50_000)
		inv := e.mustPay(o)
		e.w.settle(inv.PaymentHash, inv.AmountMsat)
		crashAt(e, crashAfterTransfer)
		if err := e.svc.pass(ctx); !errors.Is(err, errCrash) {
			t.Fatalf("pass: %v", err)
		}
		if e.transfers(o) != 1 || e.eventCount(store.EventPaid, o.ID) != 0 || h.count() != 0 {
			t.Fatalf("setup: transfers %d, order.paid %d, hook calls %d", e.transfers(o), e.eventCount(store.EventPaid, o.ID), h.count())
		}
		at := inv.ExpiresAt.Add(24 * time.Hour)
		e.clk.Advance(at.Sub(e.clk.Now()))
		if _, err := e.st.CloseInvoice(ctx, inv.PaymentHash, at, store.CloseDeadline); err != nil {
			t.Fatal(err)
		}
		e.clk.Advance(time.Minute)
		e.restart()
		e.svc.SetOnLightningPaid(h.call)
		e.svc.healPaid(ctx)
		if h.count() != 1 || !h.allFor(o) || e.eventCount(store.EventPaid, o.ID) != 1 {
			t.Fatalf("after healPaid: hook calls %d, order.paid %d", h.count(), e.eventCount(store.EventPaid, o.ID))
		}
		e.svc.healPaid(ctx)
		if h.count() != 1 || e.eventCount(store.EventPaid, o.ID) != 1 {
			t.Fatalf("after a second healPaid: hook calls %d, order.paid %d", h.count(), e.eventCount(store.EventPaid, o.ID))
		}
	})
}

// Pay's flight finds the expired invoice settled and completes it; the hook fails after order.paid. The
// payer gets ErrPaid, not the hook's error, and the reconciler's next pass calls the hook again and completes.
func TestPayAnswersPaidWhenTheHookFails(t *testing.T) {
	e := newEnv(t)
	errHook := errors.New("hook broke with detail 7f3a")
	h := &paidHook{fail: errHook}
	e.svc.SetOnLightningPaid(h.call)
	o := e.orderSat("mixed", 2_000_000, 50_000)
	inv := e.mustPay(o)
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + time.Second) // the invoice expired, the order not
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	if _, err := e.pay(o); !errors.Is(err, ErrPaid) || errors.Is(err, errHook) {
		t.Fatalf("Pay: %v, want ErrPaid", err)
	}
	if h.count() != 1 || e.eventCount(store.EventPaid, o.ID) != 1 || e.status(o).State != StatePaid {
		t.Fatalf("hook calls %d, order.paid %d", h.count(), e.eventCount(store.EventPaid, o.ID))
	}
	// The dropped hook error leaves one fixed line with the order id, never the error's text.
	if line := `msg="` + logPaidHookFailed + `" order=` + o.ID; strings.Count(e.log.String(), line) != 1 ||
		strings.Contains(e.log.String(), errHook.Error()) {
		t.Fatalf("want one %q line:\n%s", line, e.log.String())
	}
	e.svc.mu.Lock()
	oi := e.svc.open[inv.PaymentHash]
	e.svc.mu.Unlock()
	if oi.done {
		t.Fatal("done after the hook failed")
	}
	h.setFail(nil)
	e.pass()
	if h.count() != 2 || !h.allFor(o) || e.transfers(o) != 1 || e.eventCount(store.EventPaid, o.ID) != 1 {
		t.Fatalf("after the retry: hook calls %d, transfers %d", h.count(), e.transfers(o))
	}
	if total, _ := e.svc.counts(); total != 0 {
		t.Fatalf("total %d after the completion", total)
	}
}

// settleOrder's heal of order.paid (a Lightning transfer, every invoice closed, no order.paid) tells the
// on-chain rail, as completePaid does: it may be the only completion the order gets, and with the on-chain payment
// settled deep only the hook can give the both-rails ALERT. The hook runs after the account fact and the event; its
// error is returned wrapped as completePaid's. A Lightning-only order's heal does not call it.
func TestSettleOrderHealCallsTheHook(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "hook ok", true: "hook fails"}[fail], func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			h := &paidHook{}
			errHook := errors.New("hook failed")
			if fail {
				h.setFail(errHook)
			}
			e.svc.SetOnLightningPaid(h.call)
			o := e.orderSat("acct", 2_000_000, 50_000)
			ln := e.order("acct-ln", 2_000_000)
			invs := []store.Invoice{e.mustPay(o), e.mustPay(ln)}
			e.clk.Advance(2 * time.Hour)
			for _, inv := range invs {
				if _, err := e.svc.recordPaid(ctx, inv, inv.AmountMsat); err != nil {
					t.Fatal(err)
				}
				if _, err := e.st.CloseInvoice(ctx, inv.PaymentHash, e.clk.Now(), store.CloseLookup); err != nil {
					t.Fatal(err)
				}
			}
			err := e.svc.settleOrder(ctx, o.ID)
			if fail != errors.Is(err, errOnPaid) || fail != errors.Is(err, errHook) {
				t.Fatalf("settleOrder: %v", err)
			}
			if h.count() != 1 || !h.allFor(o) || e.eventCount(store.EventPaid, o.ID) != 1 {
				t.Fatalf("hook calls %d, order.paid %d", h.count(), e.eventCount(store.EventPaid, o.ID))
			}
			if known, err := e.st.AccountPaid(ctx, "acct"); err != nil || !known {
				t.Fatalf("account_paid: %v %v", known, err)
			}
			if err := e.svc.settleOrder(ctx, ln.ID); err != nil {
				t.Fatal(err)
			}
			if h.count() != 1 || e.eventCount(store.EventPaid, ln.ID) != 1 {
				t.Fatalf("the Lightning-only heal: hook calls %d, order.paid %d", h.count(), e.eventCount(store.EventPaid, ln.ID))
			}
		})
	}
}

// The Lightning + Monero twin of TestLightningPaidHookOnMixedOrder: completePaid calls the hook for an order with a
// Monero amount and no on-chain one.
func TestLightningPaidHookOnMoneroOrder(t *testing.T) {
	e := newEnv(t)
	h := &paidHook{}
	e.svc.SetOnLightningPaid(h.call)
	o := e.orderPico("mixed", 2_000_000, 1_000_000_000_000)
	inv := e.mustPay(o)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.pass()
	e.pass()
	if h.count() != 1 || !h.allFor(o) || e.eventCount(store.EventPaid, o.ID) != 1 {
		t.Fatalf("hook calls %d, want 1 with the order; order.paid %d", h.count(), e.eventCount(store.EventPaid, o.ID))
	}
}

// The twin of TestSettleOrderHealCallsTheHook: settleOrder's paid heal calls the hook for a Lightning + Monero order.
func TestSettleOrderHealCallsTheHookForMonero(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := &paidHook{}
	e.svc.SetOnLightningPaid(h.call)
	o := e.orderPico("acct", 2_000_000, 1_000_000_000_000)
	inv := e.mustPay(o)
	e.clk.Advance(2 * time.Hour)
	if _, err := e.svc.recordPaid(ctx, inv, inv.AmountMsat); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CloseInvoice(ctx, inv.PaymentHash, e.clk.Now(), store.CloseLookup); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.settleOrder(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if h.count() != 1 || !h.allFor(o) || e.eventCount(store.EventPaid, o.ID) != 1 || e.eventCount(store.EventExpired, o.ID) != 0 {
		t.Fatalf("hook calls %d, order.paid %d", h.count(), e.eventCount(store.EventPaid, o.ID))
	}
}
