package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInvoiceRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	o := mkOrder(t, s, "r", 3000, t0.Add(time.Hour))
	i1 := mkInvoice(t, s, o, 'a', t0.Add(20*time.Minute))
	i2 := Invoice{OrderID: o.ID, PaymentHash: hash('b'), Bolt11: "lnbc2", AmountMsat: 3000, CreatedAt: t0.Add(time.Minute), ExpiresAt: t0.Add(21 * time.Minute)}
	if err := s.AddInvoice(ctx, i2); err != nil {
		t.Fatal(err)
	}
	got, err := s.InvoicesForOrder(ctx, o.ID)
	if err != nil || len(got) != 2 || got[0] != i1 || got[1] != i2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got, err := s.InvoicesForOrder(ctx, strings.Repeat("0", 32)); err != nil || len(got) != 0 {
		t.Fatalf("unknown order: %+v, %v", got, err)
	}
	open, err := s.OpenInvoices(ctx)
	if err != nil || len(open) != 2 {
		t.Fatalf("open: %+v, %v", open, err)
	}
}

func TestAddInvoiceRefusals(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	o := mkOrder(t, s, "r", 3000, t0.Add(time.Hour))
	good := mkInvoice(t, s, o, 'a', t0.Add(20*time.Minute))

	if err := s.AddInvoice(ctx, good); !errors.Is(err, ErrDuplicate) {
		t.Errorf("same invoice again: %v", err)
	}
	dup := good
	dup.Bolt11 = "other"
	if err := s.AddInvoice(ctx, dup); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate payment hash: %v", err)
	}
	unknown := good
	unknown.PaymentHash = hash('c')
	unknown.OrderID = strings.Repeat("e", 32)
	if err := s.AddInvoice(ctx, unknown); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown order: %v", err)
	}
	mod := func(f func(*Invoice)) Invoice { i := good; i.PaymentHash = hash('d'); f(&i); return i }
	for name, inv := range map[string]Invoice{
		"short hash":       mod(func(i *Invoice) { i.PaymentHash = strings.Repeat("d", 63) }),
		"long hash":        mod(func(i *Invoice) { i.PaymentHash = strings.Repeat("d", 65) }),
		"upper hash":       mod(func(i *Invoice) { i.PaymentHash = strings.Repeat("D", 64) }),
		"non-hex hash":     mod(func(i *Invoice) { i.PaymentHash = strings.Repeat("g", 64) }),
		"bad order id":     mod(func(i *Invoice) { i.OrderID = "x" }),
		"empty bolt11":     mod(func(i *Invoice) { i.Bolt11 = "" }),
		"bolt11 space":     mod(func(i *Invoice) { i.Bolt11 = "ln bc" }),
		"bolt11 NUL":       mod(func(i *Invoice) { i.Bolt11 = "ln\x00bc" }),
		"bolt11 too big":   mod(func(i *Invoice) { i.Bolt11 = strings.Repeat("l", 4097) }),
		"amount 0":         mod(func(i *Invoice) { i.AmountMsat = 0 }),
		"amount big":       mod(func(i *Invoice) { i.AmountMsat = 1<<53 + 1 }),
		"expiry not after": mod(func(i *Invoice) { i.ExpiresAt = i.CreatedAt }),
		"expiry zero":      mod(func(i *Invoice) { i.ExpiresAt = time.Time{} }),
	} {
		if err := s.AddInvoice(ctx, inv); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An amount inside the store's range that differs from the order's, smaller or larger, is refused
	// in Go (the trigger is the second line), and nothing is written.
	for name, amount := range map[string]int64{"smaller": o.AmountMsat - 1000, "larger": o.AmountMsat + 1000} {
		inv := mod(func(i *Invoice) { i.AmountMsat = amount })
		if err := s.AddInvoice(ctx, inv); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "order amount") {
			t.Errorf("amount %s than the order's: %v", name, err)
		}
	}
	if all, err := s.InvoicesForOrder(ctx, o.ID); err != nil || len(all) != 1 {
		t.Fatalf("after the refusals: %d invoices, %v", len(all), err)
	}
	// A zero CreatedAt is stamped from the clock.
	clk.set(t0.Add(5 * time.Second))
	z := mod(func(i *Invoice) { i.CreatedAt = time.Time{}; i.ExpiresAt = t0.Add(time.Hour) })
	if err := s.AddInvoice(ctx, z); err != nil {
		t.Fatal(err)
	}
	all, _ := s.InvoicesForOrder(ctx, o.ID)
	if len(all) != 2 || !all[1].CreatedAt.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("%+v", all)
	}
}

func TestAddInvoiceConcurrentSameHash(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	o := mkOrder(t, s, "r", 3000, t0.Add(time.Hour))
	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = s.AddInvoice(ctx, Invoice{OrderID: o.ID, PaymentHash: hash('a'), Bolt11: "lnbc", AmountMsat: 3000, CreatedAt: t0, ExpiresAt: t0.Add(time.Minute)})
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrDuplicate):
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d adds succeeded", ok)
	}
}

func TestCloseInvoice(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	o := mkOrder(t, s, "r", 3000, t0.Add(time.Hour))
	exp := t0.Add(20 * time.Minute)
	mkInvoice(t, s, o, 'a', exp)
	mkInvoice(t, s, o, 'b', exp)
	clk.set(exp.Add(24 * time.Hour))

	if _, err := s.CloseInvoice(ctx, hash('f'), exp, "lookup"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown hash: %v", err)
	}
	if _, err := s.CloseInvoice(ctx, "short", exp, "lookup"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad hash: %v", err)
	}
	for _, reason := range []string{"", "paid", "Lookup", "lookup "} {
		if _, err := s.CloseInvoice(ctx, hash('a'), exp, reason); !errors.Is(err, ErrInvalid) {
			t.Errorf("reason %q: %v", reason, err)
		}
	}
	// A close before the invoice expired would let the order expire with a live invoice.
	if _, err := s.CloseInvoice(ctx, hash('a'), exp.Add(-time.Second), "lookup"); !errors.Is(err, ErrInvalid) {
		t.Errorf("lookup close before expiry: %v", err)
	}
	if _, err := s.CloseInvoice(ctx, hash('a'), exp.Add(24*time.Hour-time.Second), "deadline"); !errors.Is(err, ErrInvalid) {
		t.Errorf("deadline close before expiry+24h: %v", err)
	}
	if open, _ := s.OpenInvoices(ctx); len(open) != 2 {
		t.Fatalf("%d open after refusals", len(open))
	}

	// A close in the future of the store's clock is refused, so a caller cannot close early by
	// passing the time of the deadline.
	clk.set(exp.Add(time.Minute))
	if _, err := s.CloseInvoice(ctx, hash('a'), exp.Add(24*time.Hour), "deadline"); !errors.Is(err, ErrInvalid) {
		t.Errorf("deadline close in the future: %v", err)
	}
	if _, err := s.CloseInvoice(ctx, hash('a'), exp.Add(2*time.Minute), "lookup"); !errors.Is(err, ErrInvalid) {
		t.Errorf("lookup close in the future: %v", err)
	}
	if open, _ := s.OpenInvoices(ctx); len(open) != 2 {
		t.Fatalf("%d open after future closes", len(open))
	}
	closed, err := s.CloseInvoice(ctx, hash('a'), exp, "lookup")
	if err != nil || !closed {
		t.Fatalf("close: %v %v", closed, err)
	}
	clk.set(exp.Add(24 * time.Hour))
	// Idempotent by hash, whatever the second reason: the first row stands.
	closed, err = s.CloseInvoice(ctx, hash('a'), exp.Add(24*time.Hour), "deadline")
	if err != nil || closed {
		t.Fatalf("second close: %v %v", closed, err)
	}
	open, _ := s.OpenInvoices(ctx)
	if len(open) != 1 || open[0].PaymentHash != hash('b') {
		t.Fatalf("open: %+v", open)
	}
	if ok, err := s.CloseInvoice(ctx, hash('b'), exp.Add(24*time.Hour), "deadline"); err != nil || !ok {
		t.Fatalf("deadline close: %v %v", ok, err)
	}
	if open, _ := s.OpenInvoices(ctx); len(open) != 0 {
		t.Fatalf("open: %+v", open)
	}
	cl, err := s.ClosuresForOrder(ctx, o.ID)
	if err != nil || len(cl) != 2 {
		t.Fatalf("closures: %+v %v", cl, err)
	}
	want := map[string]Closure{
		hash('a'): {PaymentHash: hash('a'), At: exp, Reason: "lookup"},
		hash('b'): {PaymentHash: hash('b'), At: exp.Add(24 * time.Hour), Reason: "deadline"},
	}
	for _, c := range cl {
		if c != want[c.PaymentHash] {
			t.Errorf("closure %+v", c)
		}
	}
}

func TestCloseInvoiceConcurrent(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	o := mkOrder(t, s, "r", 3000, t0.Add(time.Hour))
	exp := t0.Add(time.Minute)
	mkInvoice(t, s, o, 'a', exp)
	clk.set(exp)
	const n = 32
	var wg sync.WaitGroup
	res := make([]bool, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := s.CloseInvoice(ctx, hash('a'), exp, "lookup")
			if err != nil {
				t.Error(err)
			}
			res[i] = ok
		}()
	}
	close(start)
	wg.Wait()
	c := 0
	for _, r := range res {
		if r {
			c++
		}
	}
	if c != 1 {
		t.Fatalf("%d closes won", c)
	}
}

func TestInvoiceAttempts(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	a := mkOrder(t, s, "a", 1000, t0.Add(time.Hour))
	b := mkOrder(t, s, "b", 2500, t0.Add(time.Hour))
	if n, m, err := s.AttemptsSince(ctx, t0); err != nil || n != 0 || m != 0 {
		t.Fatalf("empty: %d %d %v", n, m, err)
	}
	for _, at := range []struct {
		o Order
		t time.Time
	}{{a, t0}, {a, t0.Add(time.Hour)}, {b, t0.Add(time.Hour)}, {b, t0.Add(2 * time.Hour)}} {
		if err := s.AddInvoiceAttempt(ctx, at.o.ID, at.t); err != nil {
			t.Fatal(err)
		}
	}
	// Two attempts at the same instant for the same order are two attempts.
	if err := s.AddInvoiceAttempt(ctx, b.ID, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		since time.Time
		n     int
		msat  int64
	}{
		{t0.Add(-time.Hour), 5, 1000 + 1000 + 2500*3},
		{t0, 5, 9500},
		{t0.Add(time.Second), 4, 8500},
		{t0.Add(time.Hour), 4, 8500},
		{t0.Add(time.Hour + time.Second), 2, 5000},
		{t0.Add(3 * time.Hour), 0, 0},
	} {
		n, m, err := s.AttemptsSince(ctx, c.since)
		if err != nil || n != c.n || m != c.msat {
			t.Errorf("since %v: %d %d %v, want %d %d", c.since.Sub(t0), n, m, err, c.n, c.msat)
		}
	}
	if err := s.AddInvoiceAttempt(ctx, strings.Repeat("0", 32), t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown order: %v", err)
	}
	if err := s.AddInvoiceAttempt(ctx, "x", t0); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad order id: %v", err)
	}
	if err := s.AddInvoiceAttempt(ctx, a.ID, time.Time{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("zero time: %v", err)
	}
}

func TestNewestInvoice(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	if b, ok, err := s.NewestInvoice(ctx); err != nil || ok || b != "" {
		t.Fatalf("empty store: %q %v %v", b, ok, err)
	}
	o := mkOrder(t, s, "r", 3000, t0.Add(time.Hour))
	add := func(h byte, text string, created time.Time) {
		t.Helper()
		inv := Invoice{OrderID: o.ID, PaymentHash: hash(h), Bolt11: text, AmountMsat: 3000, CreatedAt: created, ExpiresAt: created.Add(time.Hour)}
		if err := s.AddInvoice(ctx, inv); err != nil {
			t.Fatal(err)
		}
	}
	// The newest by creation time wins whatever the insertion order or the hash.
	add('c', "lnbc-mid", t0.Add(2*time.Minute))
	add('f', "lnbc-old", t0.Add(time.Minute))
	if b, ok, err := s.NewestInvoice(ctx); err != nil || !ok || b != "lnbc-mid" {
		t.Fatalf("got %q %v %v", b, ok, err)
	}
	// A tie in creation time goes to the greater payment hash.
	add('d', "lnbc-tie-high", t0.Add(2*time.Minute))
	add('a', "lnbc-tie-low", t0.Add(2*time.Minute))
	if b, ok, err := s.NewestInvoice(ctx); err != nil || !ok || b != "lnbc-tie-high" {
		t.Fatalf("tie: got %q %v %v", b, ok, err)
	}
	add('0', "lnbc-new", t0.Add(3*time.Minute))
	if b, ok, err := s.NewestInvoice(ctx); err != nil || !ok || b != "lnbc-new" {
		t.Fatalf("newest: got %q %v %v", b, ok, err)
	}
}

// NewestInvoice reads one row through the index of migration 11 and sorts nothing.
func TestNewestInvoiceUsesItsIndex(t *testing.T) {
	s, _, _ := freshStore(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN ` + newestInvoiceSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if p := strings.Join(plan, "; "); !strings.Contains(p, "invoices_created") || strings.Contains(p, "TEMP B-TREE") {
		t.Fatalf("plan: %s", p)
	}
}
