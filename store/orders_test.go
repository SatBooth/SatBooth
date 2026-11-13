package store

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestCreateOrderRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	clk.set(t0.Add(500 * time.Millisecond))
	o, created, err := s.CreateOrder(ctx, Order{AccountID: "Acct_1.x-y", MerchantRef: "ref-1", AmountMsat: 21000, ExpiresAt: t0.Add(time.Hour)})
	if err != nil || !created {
		t.Fatalf("created %v err %v", created, err)
	}
	if !hex32.MatchString(o.ID) || !hex32.MatchString(o.Token) || o.ID == o.Token {
		t.Fatalf("id %q token %q", o.ID, o.Token)
	}
	if !o.CreatedAt.Equal(t0) || !o.ExpiresAt.Equal(t0.Add(time.Hour)) || o.CreatedAt.Location() != time.UTC {
		t.Fatalf("times %v %v", o.CreatedAt, o.ExpiresAt)
	}
	byID, err := s.OrderByID(ctx, o.ID)
	if err != nil || byID != o {
		t.Fatalf("OrderByID: %+v, %v", byID, err)
	}
	byTok, err := s.OrderByToken(ctx, o.Token)
	if err != nil || byTok != o {
		t.Fatalf("OrderByToken: %+v, %v", byTok, err)
	}
	if _, err := s.OrderByID(ctx, strings.Repeat("0", 32)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := s.OrderByToken(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad token: %v", err)
	}
	// An order id is not a token and vice versa.
	if _, err := s.OrderByToken(ctx, o.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("id used as token: %v", err)
	}
}

func TestOrdersByAccount(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	a := mkOrder(t, s, "a", 1000, t0.Add(time.Hour))
	clk.set(t0.Add(time.Second))
	b := mkOrder(t, s, "b", 2000, t0.Add(time.Hour))
	other, _, err := s.CreateOrder(ctx, Order{AccountID: "other", MerchantRef: "c", AmountMsat: 1000, ExpiresAt: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.OrdersByAccount(ctx, "acct-1")
	if err != nil || len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("got %+v, %v", got, err)
	}
	got, err = s.OrdersByAccount(ctx, "other")
	if err != nil || len(got) != 1 || got[0] != other {
		t.Fatalf("got %+v, %v", got, err)
	}
	got, err = s.OrdersByAccount(ctx, "nobody")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := s.OrdersByAccount(ctx, "bad id"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad account: %v", err)
	}
}

func TestCreateOrderIdempotentAndConflict(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	first := mkOrder(t, s, "ref", 5000, t0.Add(time.Hour))
	clk.set(t0.Add(time.Minute))
	again, created, err := s.CreateOrder(ctx, Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 5000, ExpiresAt: t0.Add(2 * time.Hour)})
	if err != nil || created || again != first {
		t.Fatalf("repeat: %+v created %v err %v", again, created, err)
	}
	for name, o := range map[string]Order{
		"other amount":  {AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 6000, ExpiresAt: t0.Add(time.Hour)},
		"other account": {AccountID: "acct-2", MerchantRef: "ref", AmountMsat: 5000, ExpiresAt: t0.Add(time.Hour)},
	} {
		got, created, err := s.CreateOrder(ctx, o)
		if !errors.Is(err, ErrConflict) || created || got != (Order{}) {
			t.Errorf("%s: %+v created %v err %v", name, got, created, err)
		}
	}
	// The mode must match too: a priced repeat of this explicit order, and an explicit repeat of a priced one.
	if got, created, err := s.CreateQuotedOrder(ctx, Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 5000, ExpiresAt: t0.Add(time.Hour)}, testQuote()); !errors.Is(err, ErrConflict) || created || got != (Order{}) {
		t.Errorf("priced repeat of an explicit order: %+v created %v err %v", got, created, err)
	}
	priced := mkQuotedOrder(t, s, "priced", 5000, 0, t0.Add(time.Hour), testQuote())
	if got, created, err := s.CreateOrder(ctx, Order{AccountID: "acct-1", MerchantRef: "priced", AmountMsat: priced.AmountMsat, ExpiresAt: t0.Add(time.Hour)}); !errors.Is(err, ErrConflict) || created || got != (Order{}) {
		t.Errorf("explicit repeat of a priced order: %+v created %v err %v", got, created, err)
	}
	if all, _ := s.OrdersByAccount(ctx, "acct-1"); len(all) != 2 {
		t.Fatalf("%d orders", len(all))
	}
}

func TestCreateOrderRefusals(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	ok := Order{AccountID: "acct", MerchantRef: "ref", AmountMsat: 1000, ExpiresAt: t0.Add(time.Hour)}
	mod := func(f func(*Order)) Order { o := ok; f(&o); return o }
	cases := map[string]Order{
		"empty account":     mod(func(o *Order) { o.AccountID = "" }),
		"account bad char":  mod(func(o *Order) { o.AccountID = "ac ct" }),
		"account slash":     mod(func(o *Order) { o.AccountID = "a/b" }),
		"account non-ascii": mod(func(o *Order) { o.AccountID = "café" }),
		"account 65":        mod(func(o *Order) { o.AccountID = strings.Repeat("a", 65) }),
		"account NUL":       mod(func(o *Order) { o.AccountID = "a\x00b" }),
		"account newline":   mod(func(o *Order) { o.AccountID = "a\n" }),
		"empty ref":         mod(func(o *Order) { o.MerchantRef = "" }),
		"ref bad char":      mod(func(o *Order) { o.MerchantRef = "r:1" }),
		"ref 65":            mod(func(o *Order) { o.MerchantRef = strings.Repeat("r", 65) }),
		"amount 0":          mod(func(o *Order) { o.AmountMsat = 0 }),
		"amount negative":   mod(func(o *Order) { o.AmountMsat = -1 }),
		"amount above 2^53": mod(func(o *Order) { o.AmountMsat = 1<<53 + 1 }),
		"expiry zero":       mod(func(o *Order) { o.ExpiresAt = time.Time{} }),
		"expiry not after":  mod(func(o *Order) { o.ExpiresAt = t0 }),
		"caller-set id":     mod(func(o *Order) { o.ID = strings.Repeat("a", 32) }),
		"caller-set token":  mod(func(o *Order) { o.Token = strings.Repeat("a", 32) }),
	}
	for name, o := range cases {
		if _, created, err := s.CreateOrder(ctx, o); !errors.Is(err, ErrInvalid) || created {
			t.Errorf("%s: created %v err %v", name, created, err)
		}
	}
	// The boundaries that are allowed.
	for _, o := range []Order{
		mod(func(o *Order) {
			o.AccountID = strings.Repeat("a", 64)
			o.MerchantRef = strings.Repeat("Z", 64)
			o.AmountMsat = 1 << 53
		}),
		mod(func(o *Order) { o.MerchantRef = "-._09azAZ"; o.AmountMsat = 1 }),
	} {
		if _, created, err := s.CreateOrder(ctx, o); err != nil || !created {
			t.Errorf("%+v: created %v err %v", o, created, err)
		}
	}
}

// 32 goroutines create the same merchant reference: exactly one creates it, the rest get it back.
func TestCreateOrderConcurrent(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	const n = 32
	var wg sync.WaitGroup
	res := make([]Order, n)
	created := make([]bool, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res[i], created[i], errs[i] = s.CreateOrder(ctx, Order{AccountID: "acct", MerchantRef: "same", AmountMsat: 7000, ExpiresAt: t0.Add(time.Hour)})
		}()
	}
	close(start)
	wg.Wait()
	nc := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if created[i] {
			nc++
		}
		if res[i] != res[0] {
			t.Fatalf("goroutine %d got a different order", i)
		}
	}
	if nc != 1 {
		t.Fatalf("%d goroutines created the order", nc)
	}
	if all, _ := s.OrdersByAccount(ctx, "acct"); len(all) != 1 {
		t.Fatalf("%d orders", len(all))
	}
}
