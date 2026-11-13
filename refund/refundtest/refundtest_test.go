package refundtest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/lninvoice"
	"github.com/satbooth/satbooth/nwc"
)

func TestInvoiceIsValidAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range []approval.Network{approval.Mainnet, approval.Testnet, approval.Signet, approval.Regtest} {
		for i := 0; i < 200; i++ {
			s := Invoice(n, i)
			if err := lninvoice.Check(s, n); err != nil {
				t.Fatalf("net %d n %d: %v", n, i, err)
			}
			if seen[s] {
				t.Fatalf("duplicate invoice %d", i)
			}
			seen[s] = true
		}
	}
}

func TestScripts(t *testing.T) {
	ctx := context.Background()
	w := New()
	if _, err := w.LookupInvoiceByString(ctx, "x"); err == nil {
		t.Fatal("unscripted lookup must be NOT_FOUND")
	}
	boom := errors.New("boom")
	w.Lookup("x", Err(boom), Tx(nwc.Transaction{State: nwc.StatePending}), Tx(nwc.Transaction{State: nwc.StateSettled}))
	if _, err := w.LookupInvoiceByString(ctx, "x"); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	for _, want := range []string{"pending", "settled", "settled"} {
		tx, err := w.LookupInvoiceByString(ctx, "x")
		if err != nil || tx.State != want {
			t.Fatalf("got %v, %v; want %s", tx, err, want)
		}
	}
	w.FailOnce(CallPay, boom)
	if _, err := w.PayInvoiceUntil(ctx, "x", 1000, time.Time{}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if r, err := w.PayInvoiceUntil(ctx, "x", 1000, time.Time{}); err != nil || r.Preimage == "" {
		t.Fatal(r, err)
	}
	if w.PayCount("x") != 2 || len(w.Pays()) != 2 || w.LookupCount("x") != 5 {
		t.Fatalf("counts: %d %d %d", w.PayCount("x"), len(w.Pays()), w.LookupCount("x"))
	}
	if b, err := w.GetBudget(ctx); err != nil || b.Set {
		t.Fatal(b, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	// An attempt counts whatever its context: a second pay_invoice with a cancelled context is still a second attempt.
	_, err := w.PayInvoiceUntil(cctx, "y", 1000, time.Time{})
	if !errors.Is(err, context.Canceled) || w.PayCount("y") != 1 || len(w.Pays()) != 3 {
		t.Fatalf("ended context: %v, count %d", err, w.PayCount("y"))
	}
	if _, err := w.LookupInvoiceByString(cctx, "x"); !errors.Is(err, context.Canceled) || w.LookupCount("x") != 6 {
		t.Fatalf("ended context lookup: %v, count %d", err, w.LookupCount("x"))
	}
	// A queued failure stays queued past a call whose context had ended.
	w.FailOnce(CallLookup, boom)
	if _, err := w.LookupInvoiceByString(cctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := w.LookupInvoiceByString(ctx, "z"); !errors.Is(err, boom) {
		t.Fatalf("the failure goes to the next call, of any invoice: %v", err)
	}
}

func TestZeroAnswerRefused(t *testing.T) {
	for name, a := range map[string]Answer{"neither": {}, "both": {Tx: &nwc.Transaction{}, Err: errors.New("x")}} {
		for _, script := range []func(*Wallet){func(w *Wallet) { w.Lookup("x", a) }, func(w *Wallet) { w.AfterPay("x", a) }} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("%s: scripted without a panic", name)
					}
				}()
				script(New())
			}()
		}
	}
}

func TestAfterPay(t *testing.T) {
	ctx := context.Background()
	w := New()
	w.AfterPay("x", Tx(nwc.Transaction{State: nwc.StatePending}), Tx(nwc.Transaction{State: nwc.StateSettled}))
	for i := 0; i < 3; i++ { // before a payment: the Lookup script (none: NOT_FOUND), however often
		var we *nwc.WalletError
		if _, err := w.LookupInvoiceByString(ctx, "x"); !errors.As(err, &we) || we.Code != "NOT_FOUND" {
			t.Fatalf("before a payment: %v", err)
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	w.PayInvoiceUntil(cctx, "x", 1000, time.Time{}) // never reached the wallet
	if _, err := w.LookupInvoiceByString(ctx, "x"); err == nil {
		t.Fatal("a pay with an ended context switched the script")
	}
	w.PayInvoiceUntil(ctx, "x", 1000, time.Time{})
	for _, want := range []string{"pending", "settled", "settled"} {
		if tx, err := w.LookupInvoiceByString(ctx, "x"); err != nil || tx.State != want {
			t.Fatalf("after a payment: %v %v, want %s", tx, err, want)
		}
	}
}

func TestPaysInFlight(t *testing.T) {
	w := New()
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	w.BeforePay(func(context.Context, string) { entered <- struct{}{}; <-release })
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.PayInvoiceUntil(context.Background(), "z", 1000, time.Time{}) }()
	}
	<-entered
	<-entered
	close(release)
	wg.Wait()
	if w.MaxPaysInFlight() != 2 {
		t.Fatal(w.MaxPaysInFlight())
	}
}

func TestConcurrentCounts(t *testing.T) {
	w := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.PayInvoiceUntil(context.Background(), "z", 1000, time.Time{}) }()
	}
	wg.Wait()
	if w.PayCount("z") != 50 {
		t.Fatal(w.PayCount("z"))
	}
}
