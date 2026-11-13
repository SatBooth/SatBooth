package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func mkOrderFor(t *testing.T, s *Store, account, ref string) Order {
	t.Helper()
	o, created, err := s.CreateOrder(context.Background(), Order{AccountID: account, MerchantRef: ref, AmountMsat: 1000, ExpiresAt: t0.Add(time.Hour)})
	if err != nil || !created {
		t.Fatalf("CreateOrder(%q, %q): created %v, err %v", account, ref, created, err)
	}
	return o
}

func firstPaidOrder(t *testing.T, s *Store, account string) (string, int64) {
	t.Helper()
	var id string
	var at int64
	if err := s.db.QueryRow(`SELECT first_order_id, created_at FROM account_paid WHERE account_id = ?`, account).Scan(&id, &at); err != nil {
		t.Fatal(err)
	}
	return id, at
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAccountPaid(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	o1 := mkOrderFor(t, s, "acct-1", "r1")
	o2 := mkOrderFor(t, s, "acct-1", "r2")
	o3 := mkOrderFor(t, s, "acct-2", "r3")

	if k, err := s.AccountPaid(ctx, "acct-1"); err != nil || k {
		t.Fatalf("before: %v %v", k, err)
	}
	clk.set(t0.Add(time.Minute))
	if wrote, err := s.AddAccountPaid(ctx, o2.ID); err != nil || !wrote {
		t.Fatalf("first: %v %v", wrote, err)
	}
	clk.set(t0.Add(2 * time.Minute))
	// Idempotent by account: a second order of the account, or the same order again, writes nothing.
	for _, o := range []Order{o2, o1} {
		if wrote, err := s.AddAccountPaid(ctx, o.ID); err != nil || wrote {
			t.Fatalf("again with %s: %v %v", o.MerchantRef, wrote, err)
		}
	}
	if id, at := firstPaidOrder(t, s, "acct-1"); id != o2.ID || at != t0.Add(time.Minute).Unix() {
		t.Fatalf("first_order_id %s at %d: the first row must stand", id, at)
	}
	if k, err := s.AccountPaid(ctx, "acct-1"); err != nil || !k {
		t.Fatalf("after: %v %v", k, err)
	}
	if k, err := s.AccountPaid(ctx, "acct-2"); err != nil || k {
		t.Fatalf("another account: %v %v", k, err)
	}
	if wrote, err := s.AddAccountPaid(ctx, o3.ID); err != nil || !wrote {
		t.Fatalf("acct-2: %v %v", wrote, err)
	}
	// Refusals write nothing.
	before := countRows(t, s, "account_paid")
	if _, err := s.AddAccountPaid(ctx, strings.Repeat("0", 32)); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown order: %v", err)
	}
	for _, bad := range []string{"", "x", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.ToUpper(o1.ID), o1.ID[:31] + "g"} {
		if _, err := s.AddAccountPaid(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("order id %q: %v", bad, err)
		}
	}
	for _, bad := range []string{"", "a b", strings.Repeat("a", 65), "acct\x00", "café"} {
		if _, err := s.AccountPaid(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("account id %q: %v", bad, err)
		}
	}
	if n := countRows(t, s, "account_paid"); n != before {
		t.Fatalf("%d rows after the refusals, want %d", n, before)
	}
}

// The fact is durable: a reopened store reads the account as paid.
func TestAccountPaidSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	clk := &testClock{t: t0}
	s, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	o := mkOrderFor(t, s, "acct-1", "r1")
	if _, err := s.AddAccountPaid(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if k, err := s.AccountPaid(ctx, "acct-1"); err != nil || !k {
		t.Fatalf("after reopen: %v %v", k, err)
	}
	if wrote, err := s.AddAccountPaid(ctx, o.ID); err != nil || wrote {
		t.Fatalf("again after reopen: %v %v", wrote, err)
	}
}

func TestAddAccountPaidConcurrent(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	var orders []Order
	for i := 0; i < 4; i++ {
		orders = append(orders, mkOrderFor(t, s, "acct-1", fmt.Sprintf("r%d", i)))
	}
	const n = 32
	var wg sync.WaitGroup
	wrote := make([]bool, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			wrote[i], errs[i] = s.AddAccountPaid(ctx, orders[i%len(orders)].ID)
		}()
	}
	close(start)
	wg.Wait()
	k := 0
	for i := range wrote {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if wrote[i] {
			k++
		}
	}
	if k != 1 || countRows(t, s, "account_paid") != 1 {
		t.Fatalf("%d writes reported, %d rows", k, countRows(t, s, "account_paid"))
	}
}

// accountIDs returns account ids around every boundary of the name rule: lengths 0, 1, 63, 64 and
// 65, every allowed punctuation mark, and characters just outside the allowed set.
func accountIDs(rng *rand.Rand) []string {
	out := []string{"", "a", "A", "0", ".", "_", "-", strings.Repeat("z", 63), strings.Repeat("z", 64), strings.Repeat("z", 65),
		"a b", "a/b", "a:b", "a@b", "a`b", "a[b", "a{b", "a\x00", "a\x7f", "a\x80", "café", "ａ"}
	alphabet := []byte("abAZ09._-/ :@`[{\x00\x7f\x80")
	for i := 0; i < 200; i++ {
		n := rng.Intn(67)
		if rng.Intn(4) == 0 {
			n = 63 + rng.Intn(3)
		}
		b := make([]byte, n)
		for j := range b {
			if rng.Intn(8) == 0 {
				b[j] = alphabet[rng.Intn(len(alphabet))]
			} else {
				b[j] = "abcxyzABCXYZ0189._-"[rng.Intn(19)]
			}
		}
		out = append(out, string(b))
	}
	return out
}

// A seeded property test: under random orders and marks, AccountPaid equals a model (an account is
// paid once any of its orders was marked), AddAccountPaid reports a write exactly once per account,
// the first mark's order stands, invalid ids are ErrInvalid exactly when the Go validators say so,
// and all of it survives a reopen.
func TestAccountPaidProperty(t *testing.T) {
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
			ids := accountIDs(rng)
			var valid []string
			for _, a := range ids {
				if validName(a) {
					valid = append(valid, a)
				}
			}
			var orders []Order
			first := map[string]string{} // account -> first marked order
			for i := 0; i < 300; i++ {
				clk.set(t0.Add(time.Duration(i) * time.Second))
				switch r := rng.Intn(4); {
				case r == 0 || len(orders) == 0:
					o, _, err := s.CreateOrder(ctx, Order{AccountID: valid[rng.Intn(len(valid))], MerchantRef: fmt.Sprintf("r%d", i),
						AmountMsat: 1000, ExpiresAt: t0.Add(10 * time.Hour)})
					if err != nil {
						t.Fatal(err)
					}
					orders = append(orders, o)
				case r == 1:
					o := orders[rng.Intn(len(orders))]
					wrote, err := s.AddAccountPaid(ctx, o.ID)
					if err != nil {
						t.Fatal(err)
					}
					_, had := first[o.AccountID]
					if wrote == had {
						t.Fatalf("mark %s of %q: wrote %v, already paid %v", o.ID, o.AccountID, wrote, had)
					}
					if !had {
						first[o.AccountID] = o.ID
					}
				default:
					a := ids[rng.Intn(len(ids))]
					k, err := s.AccountPaid(ctx, a)
					if !validName(a) {
						if !errors.Is(err, ErrInvalid) {
							t.Fatalf("AccountPaid(%q): %v, want ErrInvalid", a, err)
						}
						continue
					}
					if _, want := first[a]; err != nil || k != want {
						t.Fatalf("AccountPaid(%q) = %v, %v; want %v", a, k, err, want)
					}
				}
			}
			s.Close()
			if s, err = Open(ctx, path, WithClock(clk.now)); err != nil {
				t.Fatal(err)
			}
			for _, a := range valid {
				_, want := first[a]
				if k, err := s.AccountPaid(ctx, a); err != nil || k != want {
					t.Fatalf("after reopen, AccountPaid(%q) = %v, %v; want %v", a, k, err, want)
				}
				if want {
					if id, _ := firstPaidOrder(t, s, a); id != first[a] {
						t.Fatalf("%q: first order %s, want %s", a, id, first[a])
					}
				}
			}
			if n := countRows(t, s, "account_paid"); n != len(first) {
				t.Fatalf("%d rows, want %d", n, len(first))
			}
		})
	}
}

func FuzzAccountPaid(f *testing.F) {
	for _, s := range []string{"", "a", "acct-1", strings.Repeat("a", 32), strings.Repeat("a", 64), strings.Repeat("a", 65),
		"a b", "a\x00", "\xff", "café", strings.Repeat("0", 32), strings.Repeat("A", 32)} {
		f.Add(s)
	}
	s, err := Open(context.Background(), filepath.Join(f.TempDir(), "store.db"), WithClock(func() time.Time { return t0 }))
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { s.Close() })
	f.Fuzz(func(t *testing.T, v string) {
		ctx := context.Background()
		k, err := s.AccountPaid(ctx, v)
		switch {
		case !validName(v):
			if !errors.Is(err, ErrInvalid) || k {
				t.Fatalf("AccountPaid(%q) = %v, %v; want ErrInvalid", v, k, err)
			}
		case err != nil || k:
			t.Fatalf("AccountPaid(%q) = %v, %v on a store with no orders", v, k, err)
		}
		wrote, err := s.AddAccountPaid(ctx, v)
		switch {
		case wrote:
			t.Fatalf("AddAccountPaid(%q) wrote on a store with no orders", v)
		case !validLowerHex(v, 32):
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("AddAccountPaid(%q): %v, want ErrInvalid", v, err)
			}
		case !errors.Is(err, ErrNotFound):
			t.Fatalf("AddAccountPaid(%q): %v, want ErrNotFound", v, err)
		}
	})
}
