package lnpay

import (
	"context"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/store"
)

// TestPaidMsatSumsEveryPaidInvoice: two paid invoices of one order add up, an unpaid one adds nothing, and another
// order's payment is not counted.
func TestPaidMsatSumsEveryPaidInvoice(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	o := e.order("acct", 5000)
	other := e.order("acct", 5000)
	if got, err := e.svc.PaidMsat(ctx, o.ID); err != nil || got != 0 {
		t.Fatalf("no invoice: %d %v", got, err)
	}
	mk := func(o store.Order, d byte) store.Invoice {
		inv := store.Invoice{OrderID: o.ID, PaymentHash: strings.Repeat(string(d), 64), Bolt11: "lnbcrt" + string(d), AmountMsat: o.AmountMsat,
			CreatedAt: e.clk.Now(), ExpiresAt: e.clk.Now().Add(1800e9)}
		if err := e.st.AddInvoice(ctx, inv); err != nil {
			t.Fatal(err)
		}
		return inv
	}
	paid1, paid2, unpaid, foreign := mk(o, 'a'), mk(o, 'b'), mk(o, 'c'), mk(other, 'd')
	if got, err := e.svc.PaidMsat(ctx, o.ID); err != nil || got != 0 {
		t.Fatalf("nothing paid: %d %v", got, err)
	}
	// The second pays more than the order asked (the wallet settled a larger amount): the ledger amount counts.
	for inv, amount := range map[store.Invoice]int64{paid1: 5000, paid2: 6000, foreign: 5000} {
		if _, err := e.svc.recordPaid(ctx, inv, amount); err != nil {
			t.Fatal(err)
		}
	}
	_ = unpaid
	if got, err := e.svc.PaidMsat(ctx, o.ID); err != nil || got != 11000 {
		t.Fatalf("PaidMsat = %d, %v; want 11000", got, err)
	}
	if got, err := e.svc.PaidMsat(ctx, other.ID); err != nil || got != 5000 {
		t.Fatalf("other order: %d %v", got, err)
	}
	if _, err := e.svc.PaidMsat(ctx, "bad"); err == nil {
		t.Fatal("a bad order id was accepted")
	}
}

// A sum above 2^53 is an error, not a wrapped number.
func TestPaidMsatRefusesAboveTwoToThe53(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	o := e.order("acct", 5000)
	for i, d := range []byte{'a', 'b'} {
		inv := store.Invoice{OrderID: o.ID, PaymentHash: strings.Repeat(string(d), 64), Bolt11: "lnbcrt" + string(d), AmountMsat: o.AmountMsat,
			CreatedAt: e.clk.Now(), ExpiresAt: e.clk.Now().Add(1800e9)}
		if err := e.st.AddInvoice(ctx, inv); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.recordPaid(ctx, inv, 1<<52+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := e.svc.PaidMsat(ctx, o.ID); err == nil {
		t.Fatalf("PaidMsat = %d, nil; want an error (the total is 2^53 + 1)", got)
	}
}

// One transfer above 2^53 is an error too.
func TestPaidMsatRefusesOneTransferAboveTwoToThe53(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	o := e.order("acct", 5000)
	inv := store.Invoice{OrderID: o.ID, PaymentHash: strings.Repeat("a", 64), Bolt11: "lnbcrta", AmountMsat: o.AmountMsat,
		CreatedAt: e.clk.Now(), ExpiresAt: e.clk.Now().Add(1800e9)}
	if err := e.st.AddInvoice(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.recordPaid(ctx, inv, 1<<53+1); err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.PaidMsat(ctx, o.ID); err == nil {
		t.Fatalf("PaidMsat = %d, nil; want an error", got)
	}
}
