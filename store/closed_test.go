package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestInvoicesClosedByLookup(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	o := mkOrder(t, s, "r", 3000, t0.Add(time.Hour))
	exp := t0.Add(20 * time.Minute)
	a := mkInvoice(t, s, o, 'a', exp)                  // closed by lookup
	b := mkInvoice(t, s, o, 'b', exp.Add(time.Minute)) // closed by lookup, later expiry
	mkInvoice(t, s, o, 'c', exp)                       // closed at the deadline
	mkInvoice(t, s, o, 'd', exp)                       // open
	clk.set(exp.Add(DeadlineGrace + time.Hour))
	for h, r := range map[byte]string{'a': CloseLookup, 'b': CloseLookup, 'c': CloseDeadline} {
		if _, err := s.CloseInvoice(ctx, hash(h), exp.Add(DeadlineGrace+time.Minute), r); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		after time.Time
		want  []Invoice
	}{
		{time.Time{}, []Invoice{a, b}},
		{exp.Add(-time.Second), []Invoice{a, b}},
		{exp, []Invoice{b}}, // strictly after
		{exp.Add(time.Minute - time.Second), []Invoice{b}},
		{exp.Add(time.Minute), nil},
	} {
		got, err := s.InvoicesClosedByLookup(ctx, c.after)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("after %v: %+v %v, want %+v", c.after, got, err, c.want)
		}
	}
	// A sub-second threshold counts as the whole second it is in, as every stored time: one
	// nanosecond before exp is in the second before it, so a (expiring at exp) is after it.
	if got, _ := s.InvoicesClosedByLookup(ctx, exp.Add(-time.Nanosecond)); len(got) != 2 {
		t.Errorf("sub-second threshold: %+v", got)
	}
}

// A seeded property test: under random invoices and closes, InvoicesClosedByLookup equals the
// model (closed with reason lookup, expiry strictly after the threshold, oldest expiry first, then
// by hash), at thresholds around every expiry, and after a reopen.
func TestInvoicesClosedByLookupProperty(t *testing.T) {
	ctx := context.Background()
	for seed := int64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			path := filepath.Join(t.TempDir(), "store.db")
			clk := &testClock{t: t0}
			s, err := Open(ctx, path, WithClock(clk.now))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			clk.set(t0.Add(100 * 24 * time.Hour)) // every close below is in the past of the clock
			var orders []Order
			for i := 0; i < 5; i++ {
				o, _, err := s.CreateOrder(ctx, Order{AccountID: "a", MerchantRef: fmt.Sprintf("r%d", i), AmountMsat: 1000, ExpiresAt: clk.now().Add(time.Hour)})
				if err != nil {
					t.Fatal(err)
				}
				orders = append(orders, o)
			}
			reason := map[string]string{}
			var invs []Invoice
			for i := 0; i < 120; i++ {
				h := fmt.Sprintf("%064x", rng.Uint64())
				exp := t0.Add(time.Duration(rng.Intn(20*24*3600)) * time.Second)
				inv := Invoice{OrderID: orders[rng.Intn(len(orders))].ID, PaymentHash: h, Bolt11: "ln" + h[:8], AmountMsat: 1000,
					CreatedAt: exp.Add(-20 * time.Minute), ExpiresAt: exp}
				if err := s.AddInvoice(ctx, inv); err != nil {
					t.Fatal(err)
				}
				invs = append(invs, inv)
				switch rng.Intn(3) {
				case 0:
					reason[h] = CloseLookup
				case 1:
					reason[h] = CloseDeadline
				}
				if r, ok := reason[h]; ok {
					if _, err := s.CloseInvoice(ctx, h, exp.Add(DeadlineGrace+time.Duration(rng.Intn(3600))*time.Second), r); err != nil {
						t.Fatal(err)
					}
				}
			}
			model := func(after time.Time) []Invoice {
				var out []Invoice
				for _, inv := range invs {
					if reason[inv.PaymentHash] == CloseLookup && inv.ExpiresAt.Unix() > after.Unix() {
						out = append(out, inv)
					}
				}
				slices.SortFunc(out, func(x, y Invoice) int {
					if c := x.ExpiresAt.Compare(y.ExpiresAt); c != 0 {
						return c
					}
					if x.PaymentHash < y.PaymentHash {
						return -1
					}
					return 1
				})
				return out
			}
			check := func(when string) {
				t.Helper()
				thresholds := []time.Time{{}, t0.Add(-time.Hour), t0.Add(40 * 24 * time.Hour)}
				for _, inv := range invs {
					thresholds = append(thresholds, inv.ExpiresAt.Add(-time.Second), inv.ExpiresAt, inv.ExpiresAt.Add(time.Second))
				}
				for _, th := range thresholds {
					got, err := s.InvoicesClosedByLookup(ctx, th)
					if err != nil || !slices.Equal(got, model(th)) {
						t.Fatalf("%s, after %v: %d invoices (%v), want %d", when, th, len(got), err, len(model(th)))
					}
				}
			}
			check("fresh")
			s.Close()
			if s, err = Open(ctx, path, WithClock(clk.now)); err != nil {
				t.Fatal(err)
			}
			check("reopened")
		})
	}
}

func FuzzInvoicesClosedByLookup(f *testing.F) {
	for _, n := range []int64{0, -1, 1, t0.Unix(), t0.Unix() + 1199, t0.Unix() + 1200, t0.Unix() + 1201, 1 << 62, -1 << 62} {
		f.Add(n)
	}
	ctx := context.Background()
	clk := &testClock{t: t0}
	s, err := Open(ctx, filepath.Join(f.TempDir(), "store.db"), WithClock(clk.now))
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { s.Close() })
	o, _, err := s.CreateOrder(ctx, Order{AccountID: "a", MerchantRef: "r", AmountMsat: 1000, ExpiresAt: t0.Add(time.Hour)})
	if err != nil {
		f.Fatal(err)
	}
	exp := t0.Add(20 * time.Minute)
	inv := Invoice{OrderID: o.ID, PaymentHash: hash('a'), Bolt11: "lnbc", AmountMsat: 1000, CreatedAt: t0, ExpiresAt: exp}
	if err := s.AddInvoice(ctx, inv); err != nil {
		f.Fatal(err)
	}
	clk.set(exp)
	if _, err := s.CloseInvoice(ctx, inv.PaymentHash, exp, CloseLookup); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, sec int64) {
		got, err := s.InvoicesClosedByLookup(ctx, time.Unix(sec, 0))
		if err != nil {
			t.Fatal(err)
		}
		if want := sec < exp.Unix(); want != (len(got) == 1) || len(got) > 1 || (len(got) == 1 && got[0] != inv) {
			t.Fatalf("after %d: %+v", sec, got)
		}
	})
}

// The query reads, never writes: a done context is its error.
func TestInvoicesClosedByLookupContext(t *testing.T) {
	s, _, _ := freshStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.InvoicesClosedByLookup(ctx, t0); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
