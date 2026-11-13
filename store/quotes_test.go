package store

import (
	"context"
	"errors"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/rates"
)

func TestCreateQuotedOrder(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	for _, rails := range []struct {
		ref       string
		msat, sat int64
	}{{"both", 4000, 900}, {"ln", 4000, 0}, {"chain", 0, 900}} {
		q := testQuote()
		o := mkQuotedOrder(t, s, rails.ref, rails.msat, rails.sat, t0.Add(time.Hour), q)
		if o.AmountMsat != rails.msat || o.AmountSat != rails.sat || o.MerchantRef != rails.ref || !hex32.MatchString(o.ID) {
			t.Fatalf("%s: order %+v", rails.ref, o)
		}
		got, ok, err := s.QuoteForOrder(ctx, o.ID)
		if err != nil || !ok {
			t.Fatalf("%s: QuoteForOrder %v %v", rails.ref, ok, err)
		}
		sameQuote(t, got, stored(q, o.ID))
		if got.Rates[0].Sources[0].Provider != "kraken" || got.Rates[0].Sources[1].Provider != "coinbase" {
			t.Fatalf("%s: sources out of order: %+v", rails.ref, got.Rates)
		}
	}
	// A Monero-only order with its XMR rate, and an order on all three rails with both rates, given XMR first: the
	// rates come back sorted by asset, each with its sources in the order given.
	xo := mkQuoted(t, s, Order{AccountID: "acct-1", MerchantRef: "xmr", AmountPiconero: 7000, ExpiresAt: t0.Add(time.Hour)}, moneroQuote())
	if got, ok, err := s.QuoteForOrder(ctx, xo.ID); err != nil || !ok {
		t.Fatalf("Monero quote: %v %v", ok, err)
	} else {
		sameQuote(t, got, stored(moneroQuote(), xo.ID))
	}
	ao := mkQuoted(t, s, Order{AccountID: "acct-1", MerchantRef: "all", AmountMsat: 4000, AmountSat: 900, AmountPiconero: 7000,
		ExpiresAt: t0.Add(time.Hour)}, testQuote2())
	if got, ok, err := s.QuoteForOrder(ctx, ao.ID); err != nil || !ok {
		t.Fatalf("two-rate quote: %v %v", ok, err)
	} else {
		sameQuote(t, got, stored(testQuote2(), ao.ID))
		if len(got.Rates) != 2 || got.Rates[0].Asset != "BTC" || got.Rates[1].Asset != "XMR" || got.Rates[1].Sources[1].Provider != "bitfinex" {
			t.Fatalf("rates not sorted by asset: %+v", got.Rates)
		}
	}
	if n := countRows(t, s, "order_quote_rates"); n != 6 {
		t.Fatalf("%d rate rows, want 6", n)
	}
	// A nanosecond fraction (a fetch stamp) and a millisecond one (Yadio) are dropped, not refused.
	q := testQuote()
	q.QuotedAt = t0.Add(-10*time.Second + 999_999_999*time.Nanosecond)
	q.Rates[0].Sources[0].At = t0.Add(-30*time.Second + 1)
	q.Rates[0].Sources[1].At = time.UnixMilli(t0.UnixMilli() - 20_000 + 999)
	o := mkQuotedOrder(t, s, "fraction", 4000, 0, t0.Add(time.Hour), q)
	got, ok, err := s.QuoteForOrder(ctx, o.ID)
	if err != nil || !ok {
		t.Fatalf("QuoteForOrder %v %v", ok, err)
	}
	want := testQuote()
	want.OrderID = o.ID
	sameQuote(t, got, want)
	if got.QuotedAt.Nanosecond() != 0 || got.Rates[0].Sources[0].At.Nanosecond() != 0 || got.Rates[0].Sources[1].At.Nanosecond() != 0 {
		t.Fatalf("fractions kept: %+v", got)
	}
	// An explicit order, an unknown id and a malformed id have no quote.
	explicit := mkOrder(t, s, "explicit", 1000, t0.Add(time.Hour))
	for _, id := range []string{explicit.ID, strings.Repeat("0", 32), "nope", "", strings.ToUpper(o.ID)} {
		if got, ok, err := s.QuoteForOrder(ctx, id); err != nil || ok || got.OrderID != "" || got.Rates != nil {
			t.Errorf("QuoteForOrder(%q): %+v %v %v", id, got, ok, err)
		}
	}
	// The accepted boundaries: margins at both bounds, one and eight sources, the longest values.
	long := strings.Repeat("9", 20) + "." + strings.Repeat("9", 18)
	var i int64
	for name, f := range map[string]func(*Quote){
		"margin min": func(q *Quote) { q.MarginBps = rates.MinMarginBps },
		"margin max": func(q *Quote) { q.MarginBps = rates.MaxMarginBps },
		"one source": func(q *Quote) { q.Rates[0].Sources = q.Rates[0].Sources[:1] },
		"eight sources": func(q *Quote) {
			for len(q.Rates[0].Sources) < MaxQuoteSources {
				q.Rates[0].Sources = append(q.Rates[0].Sources, QuoteSource{Provider: "yadio", Rate: "1", At: t0})
			}
		},
		"rate zero (the API refuses it, not the store)": func(q *Quote) { q.Rates[0].Rate = "0"; q.Rates[0].Sources[0].Rate = "0.0" },
		"longest decimals": func(q *Quote) { q.Price, q.Rates[0].Rate, q.Rates[0].Sources[0].Rate = long, long, long },
		"provider 16":      func(q *Quote) { q.Rates[0].Sources[0].Provider = strings.Repeat("z", 16) },
		"provider 1":       func(q *Quote) { q.Rates[0].Sources[0].Provider = "a" },
		"currency AAA":     func(q *Quote) { q.Currency = "AAA" },
		"time 1 s":         func(q *Quote) { q.QuotedAt = time.Unix(1, 0); q.Rates[0].Sources[0].At = time.Unix(1, 5) },
	} {
		i++
		q := testQuote()
		f(&q)
		o, created, err := s.CreateQuotedOrder(ctx, Order{AccountID: "acct-1", MerchantRef: "bound-" + itoa(i), AmountMsat: 1000, ExpiresAt: t0.Add(time.Hour)}, q)
		if err != nil || !created {
			t.Errorf("%s: created %v err %v", name, created, err)
			continue
		}
		if got, ok, err := s.QuoteForOrder(ctx, o.ID); err != nil || !ok {
			t.Errorf("%s: QuoteForOrder %v %v", name, ok, err)
		} else {
			sameQuote(t, got, stored(q, o.ID))
		}
	}
}

// Every refusal happens before anything is written.
func TestCreateQuotedOrderRefusals(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	order := Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 1000, AmountSat: 10, ExpiresAt: t0.Add(time.Hour)}
	type tc struct {
		o Order
		q func(*Quote)
	}
	nine := func(q *Quote) {
		for len(q.Rates[0].Sources) < MaxQuoteSources+1 {
			q.Rates[0].Sources = append(q.Rates[0].Sources, QuoteSource{Provider: "yadio", Rate: "1", At: t0})
		}
	}
	lnXMR := Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 1000, AmountPiconero: 5, ExpiresAt: t0.Add(time.Hour)}
	xmrOnly := Order{AccountID: "acct-1", MerchantRef: "ref", AmountPiconero: 5, ExpiresAt: t0.Add(time.Hour)}
	cases := map[string]tc{
		"price 12.":          {order, func(q *Quote) { q.Price = "12." }},
		"price 40 bytes":     {order, func(q *Quote) { q.Price = strings.Repeat("1", 21) + "." + strings.Repeat("1", 18) }},
		"price empty":        {order, func(q *Quote) { q.Price = "" }},
		"price negative":     {order, func(q *Quote) { q.Price = "-1" }},
		"price exponent":     {order, func(q *Quote) { q.Price = "1e3" }},
		"price NUL":          {order, func(q *Quote) { q.Price = "1" + string([]byte{0x00}) }},
		"currency usd":       {order, func(q *Quote) { q.Currency = "usd" }},
		"currency BTC":       {order, func(q *Quote) { q.Currency = "BTC" }},
		"currency XMR":       {order, func(q *Quote) { q.Currency = "XMR" }},
		"currency EURO":      {order, func(q *Quote) { q.Currency = "EURO" }},
		"currency empty":     {order, func(q *Quote) { q.Currency = "" }},
		"rate .5":            {order, func(q *Quote) { q.Rates[0].Rate = ".5" }},
		"rate 19 fraction":   {order, func(q *Quote) { q.Rates[0].Rate = "1." + strings.Repeat("1", 19) }},
		"xmr rate 1.":        {lnXMR, func(q *Quote) { q.Rates = []AssetRate{btcRate(), xmrRate()}; q.Rates[1].Rate = "1." }},
		"margin -5001":       {order, func(q *Quote) { q.MarginBps = -5001 }},
		"margin 5001":        {order, func(q *Quote) { q.MarginBps = 5001 }},
		"no sources":         {order, func(q *Quote) { q.Rates[0].Sources = nil }},
		"nine sources":       {order, nine},
		"xmr no sources":     {lnXMR, func(q *Quote) { q.Rates = []AssetRate{btcRate(), xmrRate()}; q.Rates[1].Sources = nil }},
		"provider Kraken":    {order, func(q *Quote) { q.Rates[0].Sources[0].Provider = "Kraken" }},
		"provider empty":     {order, func(q *Quote) { q.Rates[0].Sources[1].Provider = "" }},
		"provider 17":        {order, func(q *Quote) { q.Rates[0].Sources[0].Provider = strings.Repeat("a", 17) }},
		"provider digit":     {order, func(q *Quote) { q.Rates[0].Sources[0].Provider = "k2" }},
		"source rate 1.":     {order, func(q *Quote) { q.Rates[0].Sources[1].Rate = "1." }},
		"source at zero":     {order, func(q *Quote) { q.Rates[0].Sources[0].At = time.Time{} }},
		"source at epoch":    {order, func(q *Quote) { q.Rates[0].Sources[0].At = time.Unix(0, 999_999_999) }},
		"source before 1970": {order, func(q *Quote) { q.Rates[0].Sources[0].At = time.Unix(-5, 0) }},
		"no rates":           {order, func(q *Quote) { q.Rates = nil }},
		"three rates":        {lnXMR, func(q *Quote) { q.Rates = []AssetRate{btcRate(), xmrRate(), btcRate()} }},
		"BTC twice":          {order, func(q *Quote) { q.Rates = []AssetRate{btcRate(), btcRate()} }},
		"XMR twice":          {xmrOnly, func(q *Quote) { q.Rates = []AssetRate{xmrRate(), xmrRate()} }},
		"asset ETH":          {order, func(q *Quote) { q.Rates[0].Asset = "ETH" }},
		"asset btc":          {order, func(q *Quote) { q.Rates[0].Asset = "btc" }},
		"asset empty":        {order, func(q *Quote) { q.Rates[0].Asset = "" }},
		"ETH beside BTC": {order, func(q *Quote) {
			q.Rates = append(q.Rates, AssetRate{Asset: "ETH", Rate: "1", Sources: btcRate().Sources})
		}},
		// The rates must be exactly the order's rails, both ways.
		"XMR rate, no Monero":      {order, func(q *Quote) { q.Rates = []AssetRate{btcRate(), xmrRate()} }},
		"only XMR, BTC rails":      {order, func(q *Quote) { q.Rates = []AssetRate{xmrRate()} }},
		"no XMR rate, Monero rail": {lnXMR, func(*Quote) {}},
		"only XMR, Lightning rail": {lnXMR, func(q *Quote) { q.Rates = []AssetRate{xmrRate()} }},
		"BTC rate, Monero only":    {xmrOnly, func(*Quote) {}},
		"both rates, Monero only":  {xmrOnly, func(q *Quote) { q.Rates = []AssetRate{btcRate(), xmrRate()} }},
		"order: piconero 2^53":     {Order{AccountID: "a", MerchantRef: "ref", AmountPiconero: 1 << 53, ExpiresAt: t0.Add(time.Hour)}, func(q *Quote) { q.Rates = []AssetRate{xmrRate()} }},
		"quoted at zero":           {order, func(q *Quote) { q.QuotedAt = time.Time{} }},
		"quoted at epoch":          {order, func(q *Quote) { q.QuotedAt = time.Unix(0, 0) }},
		"order id given":           {order, func(q *Quote) { q.OrderID = strings.Repeat("a", 32) }},
		"order: bad account":       {Order{AccountID: "a b", MerchantRef: "ref", AmountMsat: 1000, ExpiresAt: t0.Add(time.Hour)}, func(*Quote) {}},
		"order: no amount":         {Order{AccountID: "a", MerchantRef: "ref", ExpiresAt: t0.Add(time.Hour)}, func(*Quote) {}},
		"order: id given":          {Order{ID: strings.Repeat("a", 32), AccountID: "a", MerchantRef: "ref", AmountMsat: 1, ExpiresAt: t0.Add(time.Hour)}, func(*Quote) {}},
		"order: expiry passed":     {Order{AccountID: "a", MerchantRef: "ref", AmountMsat: 1, ExpiresAt: t0}, func(*Quote) {}},
	}
	for name, c := range cases {
		q := testQuote()
		c.q(&q)
		got, created, err := s.CreateQuotedOrder(ctx, c.o, q)
		if !errors.Is(err, ErrInvalid) || created || got != (Order{}) {
			t.Errorf("%s: %+v created %v err %v", name, got, created, err)
		}
		if n, m, r := countRows(t, s, "orders"), countRows(t, s, "order_quotes"), countRows(t, s, "order_quote_rates"); n != 0 || m != 0 || r != 0 {
			t.Fatalf("%s: %d orders, %d quotes and %d rates written", name, n, m, r)
		}
	}
}

// A quote row and its rate rows cannot be changed, deleted or replaced.
func TestQuoteImmutable(t *testing.T) {
	s, path, _ := freshStore(t)
	o := mkQuotedOrder(t, s, "ref", 4000, 0, t0.Add(time.Hour), testQuote())
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for name, q := range map[string]string{
		"update price":           `UPDATE order_quotes SET price = '1'`,
		"update margin":          `UPDATE order_quotes SET margin_bps = margin_bps`,
		"delete":                 `DELETE FROM order_quotes`,
		"insert or replace":      `INSERT OR REPLACE INTO order_quotes SELECT order_id, '1', currency, margin_bps, quoted_at FROM order_quotes`,
		"replace":                `REPLACE INTO order_quotes SELECT * FROM order_quotes`,
		"upsert":                 `INSERT INTO order_quotes SELECT * FROM order_quotes WHERE 1 ON CONFLICT DO UPDATE SET price = '1'`,
		"insert or ignore":       `INSERT OR IGNORE INTO order_quotes SELECT order_id, '1', currency, margin_bps, quoted_at FROM order_quotes`,
		"rate update":            `UPDATE order_quote_rates SET rate = '1'`,
		"rate update sources":    `UPDATE order_quote_rates SET sources = sources`,
		"rate delete":            `DELETE FROM order_quote_rates`,
		"rate insert or replace": `INSERT OR REPLACE INTO order_quote_rates SELECT order_id, asset, '1', sources FROM order_quote_rates`,
		"rate replace":           `REPLACE INTO order_quote_rates SELECT * FROM order_quote_rates`,
		"rate upsert":            `INSERT INTO order_quote_rates SELECT * FROM order_quote_rates WHERE 1 ON CONFLICT DO UPDATE SET rate = '1'`,
		"rate insert or ignore":  `INSERT OR IGNORE INTO order_quote_rates SELECT order_id, asset, '1', sources FROM order_quote_rates`,
	} {
		if _, err := s.db.Exec(q); err == nil {
			t.Errorf("store connection: %s succeeded", name)
		}
		if _, err := raw.Exec(q); err == nil {
			t.Errorf("raw connection: %s succeeded", name)
		}
	}
	got, ok, err := s.QuoteForOrder(context.Background(), o.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	sameQuote(t, got, stored(testQuote(), o.ID))
	if countRows(t, s, "order_quotes") != 1 || countRows(t, s, "order_quote_rates") != 1 {
		t.Fatal("the quote row count changed")
	}
}

// An insert for an unknown order fails on the trigger, on the store's connection and on a raw one without
// foreign keys.
func TestQuoteNeedsItsOrder(t *testing.T) {
	s, path, _ := freshStore(t)
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ins := `INSERT INTO order_quotes VALUES ('` + strings.Repeat("d", 32) + `', '1', 'USD', 0, 5)`
	if _, err := s.db.Exec(ins); err == nil || !strings.Contains(err.Error(), "unknown order") {
		t.Errorf("store connection: %v", err)
	}
	if _, err := raw.Exec(ins); err == nil || !strings.Contains(err.Error(), "unknown order") {
		t.Errorf("raw connection: %v", err)
	}
	// A rate row needs its quote, even when its order exists.
	o := mkOrder(t, s, "explicit", 1000, t0.Add(time.Hour))
	for _, id := range []string{strings.Repeat("d", 32), o.ID} {
		rate := `INSERT INTO order_quote_rates VALUES ('` + id + `', 'BTC', '1', '[{"provider":"kraken","rate":"1","at":5}]')`
		if _, err := s.db.Exec(rate); err == nil || !strings.Contains(err.Error(), "unknown quote") {
			t.Errorf("store connection, rate: %v", err)
		}
		if _, err := raw.Exec(rate); err == nil || !strings.Contains(err.Error(), "unknown quote") {
			t.Errorf("raw connection, rate: %v", err)
		}
	}
	if countRows(t, s, "order_quotes") != 0 || countRows(t, s, "order_quote_rates") != 0 {
		t.Fatal("a quote without an order, or a rate without a quote, was stored")
	}
}

// A repeat with the same reference after the rate moved returns the first order and its locked
// rate. A different price, currency or rail set, or a switch between explicit and priced, is a conflict.
func TestQuotedRepeatKeepsTheRate(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	first := mkQuotedOrder(t, s, "ref", 4000, 900, t0.Add(time.Hour), testQuote())
	clk.set(t0.Add(time.Minute))
	moved := testQuote()
	moved.MarginBps = -200
	moved.Rates = []AssetRate{{Asset: "BTC", Rate: "80000", Sources: []QuoteSource{{Provider: "yadio", Rate: "80000", At: t0.Add(50 * time.Second)}}}}
	moved.QuotedAt = t0.Add(55 * time.Second)
	again, created, err := s.CreateQuotedOrder(ctx, Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 3800, AmountSat: 860, ExpiresAt: t0.Add(2 * time.Hour)}, moved)
	if err != nil || created || again != first {
		t.Fatalf("repeat: %+v created %v err %v", again, created, err)
	}
	got, ok, err := s.QuoteForOrder(ctx, first.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	sameQuote(t, got, stored(testQuote(), first.ID))
	// The price is compared byte for byte: "21.5" is the same value as "21.50" but another text.
	for name, c := range map[string]struct {
		o Order
		q func(*Quote)
	}{
		"other price":      {Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 4000, AmountSat: 900, ExpiresAt: t0.Add(time.Hour)}, func(q *Quote) { q.Price = "22" }},
		"same value, text": {Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 4000, AmountSat: 900, ExpiresAt: t0.Add(time.Hour)}, func(q *Quote) { q.Price = "21.5" }},
		"other currency":   {Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 4000, AmountSat: 900, ExpiresAt: t0.Add(time.Hour)}, func(q *Quote) { q.Currency = "EUR" }},
		"lightning only":   {Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 4000, ExpiresAt: t0.Add(time.Hour)}, func(*Quote) {}},
		"on-chain only":    {Order{AccountID: "acct-1", MerchantRef: "ref", AmountSat: 900, ExpiresAt: t0.Add(time.Hour)}, func(*Quote) {}},
		"other account":    {Order{AccountID: "acct-2", MerchantRef: "ref", AmountMsat: 4000, AmountSat: 900, ExpiresAt: t0.Add(time.Hour)}, func(*Quote) {}},
		"Monero added":     {Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 4000, AmountSat: 900, AmountPiconero: 5, ExpiresAt: t0.Add(time.Hour)}, func(q *Quote) { *q = testQuote2() }},
	} {
		q := testQuote()
		c.q(&q)
		if got, created, err := s.CreateQuotedOrder(ctx, c.o, q); !errors.Is(err, ErrConflict) || created || got != (Order{}) {
			t.Errorf("%s: %+v created %v err %v", name, got, created, err)
		}
	}
	// An explicit repeat of a priced order, even with its exact amounts.
	if got, created, err := s.CreateOrder(ctx, Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 4000, AmountSat: 900, ExpiresAt: t0.Add(time.Hour)}); !errors.Is(err, ErrConflict) || created || got != (Order{}) {
		t.Errorf("explicit repeat of a priced order: %+v created %v err %v", got, created, err)
	}
	// A priced repeat of an explicit order.
	mkOrder(t, s, "plain", 4000, t0.Add(time.Hour))
	if got, created, err := s.CreateQuotedOrder(ctx, Order{AccountID: "acct-1", MerchantRef: "plain", AmountMsat: 4000, ExpiresAt: t0.Add(time.Hour)}, testQuote()); !errors.Is(err, ErrConflict) || created || got != (Order{}) {
		t.Errorf("priced repeat of an explicit order: %+v created %v err %v", got, created, err)
	}
	if n, m := countRows(t, s, "orders"), countRows(t, s, "order_quotes"); n != 2 || m != 1 {
		t.Fatalf("%d orders, %d quotes", n, m)
	}
	got, _, _ = s.QuoteForOrder(ctx, first.ID)
	sameQuote(t, got, stored(testQuote(), first.ID))
}

// 32 goroutines price the same reference at different rates: exactly one order and one quote are stored, and every
// caller gets that order back.
func TestCreateQuotedOrderConcurrent(t *testing.T) {
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
			q := testQuote()
			q.Rates[0].Rate = "7600" + itoa(int64(i))
			res[i], created[i], errs[i] = s.CreateQuotedOrder(ctx, Order{AccountID: "acct", MerchantRef: "same", AmountMsat: 7000 + int64(i), ExpiresAt: t0.Add(time.Hour)}, q)
		}()
	}
	close(start)
	wg.Wait()
	nc, winner := 0, -1
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if created[i] {
			nc, winner = nc+1, i
		}
		if res[i] != res[0] {
			t.Fatalf("goroutine %d got a different order", i)
		}
	}
	if nc != 1 {
		t.Fatalf("%d goroutines created the order", nc)
	}
	if countRows(t, s, "orders") != 1 || countRows(t, s, "order_quotes") != 1 || countRows(t, s, "order_quote_rates") != 1 {
		t.Fatal("more than one order, quote or rate")
	}
	got, _, err := s.QuoteForOrder(ctx, res[0].ID)
	if err != nil || got.Rates[0].Rate != "7600"+itoa(int64(winner)) || res[0].AmountMsat != 7000+int64(winner) {
		t.Fatalf("the quote %+v is not the winner's (%d): %v", got, winner, err)
	}
}

// The quote's INSERT, or its second rate's, fails after the order's: the transaction rolls back everything, and the
// writer is not wedged.
func TestQuotedOrderRollsBack(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	for _, trig := range []string{
		`CREATE TRIGGER t_fail BEFORE INSERT ON order_quotes BEGIN SELECT RAISE(ABORT, 'test'); END`,
		`CREATE TRIGGER t_fail BEFORE INSERT ON order_quote_rates WHEN NEW.asset = 'XMR' BEGIN SELECT RAISE(ABORT, 'test'); END`,
	} {
		if _, err := s.db.Exec(trig); err != nil {
			t.Fatal(err)
		}
		o := Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 4000, AmountPiconero: 5, ExpiresAt: t0.Add(time.Hour)}
		if got, created, err := s.CreateQuotedOrder(ctx, o, testQuote2()); err == nil || created || got != (Order{}) {
			t.Fatalf("with the failing trigger: %+v created %v err %v", got, created, err)
		}
		if n, m, r := countRows(t, s, "orders"), countRows(t, s, "order_quotes"), countRows(t, s, "order_quote_rates"); n != 0 || m != 0 || r != 0 {
			t.Fatalf("%d orders, %d quotes and %d rates after the rollback", n, m, r)
		}
		if _, err := s.db.Exec(`DROP TRIGGER t_fail`); err != nil {
			t.Fatal(err)
		}
	}
	got := mkQuoted(t, s, Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 4000, AmountPiconero: 5, ExpiresAt: t0.Add(time.Hour)}, testQuote2())
	if q, ok, err := s.QuoteForOrder(ctx, got.ID); err != nil || !ok {
		t.Fatal(ok, err)
	} else {
		sameQuote(t, q, stored(testQuote2(), got.ID))
	}
}

// The longest quote the Go validators accept fits the SQL length bound of sources with room to spare: the bound
// is a guard for raw inserts, never reached through CreateQuotedOrder.
func TestLongestQuoteFits(t *testing.T) {
	long := strings.Repeat("9", 20) + "." + strings.Repeat("9", 18)
	if len(long) != 39 || !validDecimal(long) {
		t.Fatal("not the longest decimal")
	}
	// The latest time whose Unix seconds have 19 digits and survive a round trip through time.Time.
	far := time.Unix(9_000_000_000_000_000_000, 999_999_999)
	if far.Unix() != 9_000_000_000_000_000_000 {
		t.Fatalf("far time does not round-trip: %d", far.Unix())
	}
	q := Quote{Price: long, Currency: "ZZZ", MarginBps: rates.MinMarginBps, QuotedAt: far}
	for _, asset := range []string{"BTC", "XMR"} {
		r := AssetRate{Asset: asset, Rate: long}
		for i := 0; i < MaxQuoteSources; i++ {
			r.Sources = append(r.Sources, QuoteSource{Provider: strings.Repeat("z", 16), Rate: long, At: far})
		}
		q.Rates = append(q.Rates, r)
	}
	if err := validQuote(q); err != nil {
		t.Fatal(err)
	}
	b, err := marshalSources(q.Rates[0].Sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) >= 4096 {
		t.Fatalf("the longest sources are %d bytes", len(b))
	}
	t.Logf("the longest sources: %d bytes", len(b))
	s, _, _ := freshStore(t)
	o := mkQuoted(t, s, Order{AccountID: "acct-1", MerchantRef: "long", AmountMsat: 1, AmountPiconero: MaxPiconero, ExpiresAt: t0.Add(time.Hour)}, q)
	got, ok, err := s.QuoteForOrder(context.Background(), o.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	sameQuote(t, got, stored(q, o.ID))
}

// The restart with an order already priced: the quote comes back byte for byte.
func TestQuoteSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	clk := &testClock{t: t0}
	s, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	q := testQuote()
	q.QuotedAt = q.QuotedAt.Add(123 * time.Millisecond)
	q.Rates[0].Sources[1].At = q.Rates[0].Sources[1].At.Add(7)
	o := mkQuotedOrder(t, s, "ref", 4000, 900, t0.Add(time.Hour), q)
	before, _, err := s.QuoteForOrder(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	after, ok, err := s2.QuoteForOrder(ctx, o.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	sameQuote(t, after, before)
	sameQuote(t, after, stored(q, o.ID))
	// The repeat after the restart, at another rate, still returns the order and the locked rate.
	moved := testQuote()
	moved.Rates[0].Rate = "1"
	again, created, err := s2.CreateQuotedOrder(ctx, Order{AccountID: "acct-1", MerchantRef: "ref", AmountMsat: 1, AmountSat: 1, ExpiresAt: t0.Add(time.Hour)}, moved)
	if err != nil || created || again != o {
		t.Fatalf("repeat after restart: %+v %v %v", again, created, err)
	}
	if after, _, _ := s2.QuoteForOrder(ctx, o.ID); after.Rates[0].Rate != q.Rates[0].Rate {
		t.Fatalf("rate %q after the repeat", after.Rates[0].Rate)
	}
}

// decimalBytes are the byte classes of rates' decimal vectors and refusals: digits, the point, the bytes either
// side of the digits, signs, exponent letters, spaces, NUL, DEL, and the first bytes of multi-byte look-alikes.
var decimalBytes = []byte{'0', '1', '5', '9', '.', '.', '.', 0x2f, 0x3a, '-', '+', 'e', 'E', ',', '_', 'x', ' ', '\n', '\t', 0x00, 0x7f, 0x80, 0xc2, 0xa0, 0xef, 0xbc, 0x91, 0xd9, 0xff}

// drawDecimal returns a string near the decimal grammar: mostly digits with zero, one or two points placed at the
// boundary positions, with sometimes one byte from another class.
func drawDecimal(rng *rand.Rand) string {
	digits := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('0' + rng.Intn(10))
		}
		return string(b)
	}
	lens := []int{0, 1, 2, 17, 18, 19, 20, 21, 22}
	var s string
	switch rng.Intn(4) {
	case 0: // integer part only
		s = digits(lens[rng.Intn(len(lens))])
	case 1, 2: // integer part, point, fraction part
		s = digits(lens[rng.Intn(len(lens))]) + "." + digits(lens[rng.Intn(len(lens))])
	default: // any bytes of the classes, up to 42 long
		b := make([]byte, rng.Intn(43))
		for i := range b {
			b[i] = decimalBytes[rng.Intn(len(decimalBytes))]
		}
		s = string(b)
	}
	if len(s) > 0 && rng.Intn(4) == 0 {
		b := []byte(s)
		b[rng.Intn(len(b))] = decimalBytes[rng.Intn(len(decimalBytes))]
		s = string(b)
	}
	return s
}

// checkDecimal, the SQL mirror, and rates.ParseDecimal, which the Go side calls, accept the same strings.
func TestDecimalGrammarAgrees(t *testing.T) {
	rng := rand.New(rand.NewSource(20261106))
	c := newSQLChecks(t)
	check := func(s string) {
		if _, err := rates.ParseDecimal(s); c.eval(t, checkDecimal("?1"), s) != (err == nil) {
			t.Fatalf("% x: SQL says %v, rates says %v", s, err != nil, err)
		}
	}
	for _, s := range []string{"", ".", "1.", ".5", "1.2.3", "0", "00", "0.0", "1.5", "-1", "+1", "1e5", " 1", "1 ",
		strings.Repeat("1", 20), strings.Repeat("1", 21), "1." + strings.Repeat("1", 18), "1." + strings.Repeat("1", 19),
		strings.Repeat("1", 20) + "." + strings.Repeat("1", 18), strings.Repeat("1", 40), "1" + string([]byte{0x00})} {
		check(s)
	}
	accepted := 0
	for i := 0; i < 20000; i++ {
		s := drawDecimal(rng)
		check(s)
		if validDecimal(s) {
			accepted++
		}
	}
	// The draws cross the boundary both ways.
	if accepted < 2000 || accepted > 18000 {
		t.Fatalf("%d of 20000 draws accepted", accepted)
	}
}

// currencyBytes are the byte classes around the currency rule: upper and lower case letters and the bytes either
// side of each range, digits, NUL, DEL and non-ASCII.
var currencyBytes = []byte{'A', 'B', 'C', 'T', 'U', 'Z', 'a', 'z', '@', '[', '`', '{', '0', '9', ' ', 0x00, 0x7f, 0x80, 0xc3, 0xff}

// checkCurrency, the SQL mirror, and rates.ValidCurrency, which the Go side calls, accept the same strings.
func TestCurrencyRuleAgrees(t *testing.T) {
	rng := rand.New(rand.NewSource(20261107))
	c := newSQLChecks(t)
	check := func(s string) {
		if got := c.eval(t, checkCurrency("?1"), s); got != rates.ValidCurrency(s) {
			t.Fatalf("% x: SQL says %v, rates says %v", s, got, rates.ValidCurrency(s))
		}
	}
	for _, s := range []string{"", "U", "US", "USD", "USDT", "BTC", "BTc", "btc", "XBT", "XMR", "XMr", "xmr", "XMS", "EUR", "usd"} {
		check(s)
	}
	// Every byte in every position of a 3-byte code, and next to "BTC" and "XMR".
	for _, base := range []string{"USD", "BTC", "XMR"} {
		for b := 0; b < 256; b++ {
			for pos := 0; pos < 3; pos++ {
				v := []byte(base)
				v[pos] = byte(b)
				check(string(v))
			}
		}
	}
	accepted := 0
	for i := 0; i < 20000; i++ {
		n := 3
		if rng.Intn(5) == 0 {
			n = rng.Intn(6)
		}
		v := make([]byte, n)
		for j := range v {
			v[j] = currencyBytes[rng.Intn(len(currencyBytes))]
		}
		check(string(v))
		if rates.ValidCurrency(string(v)) {
			accepted++
		}
	}
	if accepted < 200 { // about 2 %: three of 20 classes are 6 upper-case letters
		t.Fatalf("%d of 20000 draws accepted", accepted)
	}
}

// validProvider: 1 to 16 bytes of [a-z], every byte in every position at each length boundary.
func TestValidProvider(t *testing.T) {
	for _, n := range []int{0, 1, 2, 15, 16, 17, 64} {
		base := strings.Repeat("k", n)
		if got, want := validProvider(base), n >= 1 && n <= 16; got != want {
			t.Fatalf("length %d: %v", n, got)
		}
		for b := 0; b < 256 && n > 0; b++ {
			for _, pos := range []int{0, n / 2, n - 1} {
				v := []byte(base)
				v[pos] = byte(b)
				want := n <= 16 && 'a' <= b && b <= 'z'
				if got := validProvider(string(v)); got != want {
					t.Fatalf("% x: %v, want %v", v, got, want)
				}
			}
		}
	}
	for _, s := range []string{"kraken", "coinbase", "yadio", "custom"} {
		if !validProvider(s) {
			t.Errorf("%q refused", s)
		}
	}
}

// A quote row written past the store that the SQL checks allow but the Go validators refuse (sources that are not
// source objects, a provider in capitals) reads back as an error, never as a quote.
func TestQuoteForOrderRefusesAMalformedRow(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	good := `[{"provider":"kraken","rate":"1","at":5}]`
	for i, rows := range [][][2]string{
		{{"BTC", `["kraken"]`}}, {{"BTC", `[{"provider":"Kraken","rate":"1","at":5}]`}}, {{"BTC", `[{"provider":"kraken","rate":"1","at":0}]`}},
		{{"BTC", `[{"provider":"kraken","rate":"1","at":1.5}]`}}, {{"BTC", `[{"provider":"kraken","rate":"1.","at":5}]`}},
		{{"BTC", good}, {"XMR", `["kraken"]`}}, // one good rate beside a malformed one
		{},                                     // a quote without a rate
	} {
		o := mkOrder(t, s, "raw-"+itoa(int64(i)), 1000, t0.Add(time.Hour))
		if _, err := s.db.Exec(`INSERT INTO order_quotes VALUES (?, '1', 'USD', 0, 5)`, o.ID); err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if _, err := s.db.Exec(`INSERT INTO order_quote_rates VALUES (?, ?, '1', ?)`, o.ID, r[0], r[1]); err != nil {
				t.Fatalf("%v: %v", rows, err)
			}
		}
		if got, ok, err := s.QuoteForOrder(ctx, o.ID); !errors.Is(err, ErrStoredQuote) || ok || got.OrderID != "" || got.Rates != nil {
			t.Errorf("%v: %+v %v %v", rows, got, ok, err)
		}
	}
}

// PricedRepeat answers a priced request's reference in plain reads: the existing order when it is a repeat (the same
// account, price text, currency and rail set), ErrConflict for any other order under that reference, ErrNotFound
// when there is none.
func TestPricedRepeat(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	o := mkQuotedOrder(t, s, "p1", 4000, 0, t0.Add(time.Hour), testQuote())
	mkOrder(t, s, "ex", 4000, t0.Add(time.Hour))
	same := PricedRequest{AccountID: "acct-1", MerchantRef: "p1", Price: "21.50", Currency: "USD", Lightning: true}
	if got, err := s.PricedRepeat(ctx, same); err != nil || got != o {
		t.Fatalf("the repeat: %+v %v, want %+v", got, err, o)
	}
	for name, mut := range map[string]func(*PricedRequest){
		"another account":       func(r *PricedRequest) { r.AccountID = "acct-2" },
		"the same value, text":  func(r *PricedRequest) { r.Price = "21.5" },
		"another price":         func(r *PricedRequest) { r.Price = "22" },
		"another currency":      func(r *PricedRequest) { r.Currency = "EUR" },
		"both rails":            func(r *PricedRequest) { r.Onchain = true },
		"on-chain only":         func(r *PricedRequest) { r.Lightning, r.Onchain = false, true },
		"Monero added":          func(r *PricedRequest) { r.Monero = true },
		"Monero only":           func(r *PricedRequest) { r.Lightning, r.Monero = false, true },
		"an explicit order":     func(r *PricedRequest) { r.MerchantRef = "ex" },
		"an explicit, its rail": func(r *PricedRequest) { r.MerchantRef, r.Price = "ex", "4" },
	} {
		r := same
		mut(&r)
		if got, err := s.PricedRepeat(ctx, r); !errors.Is(err, ErrConflict) || got != (Order{}) {
			t.Errorf("%s: %+v %v, want ErrConflict", name, got, err)
		}
	}
	// The reference is matched exactly: no case folding, no prefix; a malformed one is simply not found.
	for _, ref := range []string{"p2", "P1", "p", "p1 ", "p10", "p1\x00", "p'1", "", "a b", strings.Repeat("r", 65), o.ID, o.Token} {
		r := same
		r.MerchantRef = ref
		if got, err := s.PricedRepeat(ctx, r); !errors.Is(err, ErrNotFound) || got != (Order{}) {
			t.Errorf("ref %q: %+v %v, want ErrNotFound", ref, got, err)
		}
	}
	// Another account's order is a conflict before its quote is read: a malformed quote there is not this caller's
	// error. The owner gets the read error, as from QuoteForOrder.
	bad := mkOrder(t, s, "bad", 4000, t0.Add(time.Hour))
	if _, err := s.db.Exec(`INSERT INTO order_quotes VALUES (?, '21.50', 'USD', 0, 5)`, bad.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO order_quote_rates VALUES (?, 'BTC', '1', '["kraken"]')`, bad.ID); err != nil {
		t.Fatal(err)
	}
	r := same
	r.MerchantRef, r.AccountID = "bad", "acct-2"
	if _, err := s.PricedRepeat(ctx, r); !errors.Is(err, ErrConflict) {
		t.Errorf("another account's order with a malformed quote: %v, want ErrConflict", err)
	}
	r.AccountID = "acct-1"
	if _, err := s.PricedRepeat(ctx, r); !errors.Is(err, ErrStoredQuote) {
		t.Errorf("the owner's order with a malformed quote: %v, want the stored-quote error", err)
	}
	if n := countRows(t, s, "orders"); n != 3 {
		t.Errorf("%d orders, want 3: PricedRepeat wrote", n)
	}
	// A Monero order: the repeat needs Monero set, and Monero flipped off is a conflict.
	x := mkQuoted(t, s, Order{AccountID: "acct-1", MerchantRef: "x1", AmountMsat: 4000, AmountPiconero: 9, ExpiresAt: t0.Add(time.Hour)}, testQuote2())
	xr := PricedRequest{AccountID: "acct-1", MerchantRef: "x1", Price: "21.50", Currency: "USD", Lightning: true, Monero: true}
	if got, err := s.PricedRepeat(ctx, xr); err != nil || got != x {
		t.Fatalf("the Monero repeat: %+v %v", got, err)
	}
	xr.Monero = false
	if _, err := s.PricedRepeat(ctx, xr); !errors.Is(err, ErrConflict) {
		t.Fatalf("Monero flipped off: %v", err)
	}
}

// PricedRepeat and CreateQuotedOrder apply one rule: over seeded requests that cross every part of it (account, price
// text, currency, rail set, explicit orders under a reference), a repeat is CreateQuotedOrder's existing order, a
// conflict its ErrConflict, and not found its new order.
func TestPricedRepeatAgreesWithCreate(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	rng := rand.New(rand.NewSource(20261108))
	accounts := []string{"acct-1", "acct-2"}
	prices := []string{"10.00", "10.0", "10", "7"}
	currencies := []string{"USD", "EUR"}
	rails := [][3]bool{{true, false, false}, {false, true, false}, {true, true, false}, {false, false, true}, {true, false, true}, {true, true, true}}
	var past []PricedRequest
	var repeats, conflicts, news int
	for i := 0; i < 3000; i++ {
		acct, ref := accounts[rng.Intn(len(accounts))], "r"+itoa(int64(rng.Intn(40)))
		if rng.Intn(10) == 0 { // an explicit order under the reference, when it is free or the same explicit order
			_, _, err := s.CreateOrder(ctx, Order{AccountID: acct, MerchantRef: ref, AmountMsat: 1000, ExpiresAt: t0.Add(time.Hour)})
			if err != nil && !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
			continue
		}
		rl := rails[rng.Intn(len(rails))]
		r := PricedRequest{AccountID: acct, MerchantRef: ref, Price: prices[rng.Intn(len(prices))],
			Currency: currencies[rng.Intn(len(currencies))], Lightning: rl[0], Onchain: rl[1], Monero: rl[2]}
		// A third of the draws replay an earlier request, a third replay one with one part changed.
		if len(past) > 0 && rng.Intn(3) > 0 {
			r = past[rng.Intn(len(past))]
			if rng.Intn(2) == 0 {
				switch rng.Intn(5) {
				case 0:
					r.AccountID = accounts[rng.Intn(len(accounts))]
				case 1:
					r.Price = prices[rng.Intn(len(prices))]
				case 2:
					r.Currency = currencies[rng.Intn(len(currencies))]
				case 3:
					r.Lightning, r.Onchain, r.Monero = rl[0], rl[1], rl[2]
				case 4:
					r.MerchantRef = ref
				}
			}
		}
		past = append(past, r)
		before, perr := s.PricedRepeat(ctx, r)
		// The amounts follow the rate, so they differ from call to call; only the rail set matters.
		o := Order{AccountID: r.AccountID, MerchantRef: r.MerchantRef, ExpiresAt: t0.Add(time.Hour)}
		if r.Lightning {
			o.AmountMsat = 1000 * (1 + rng.Int63n(1000))
		}
		if r.Onchain {
			o.AmountSat = 1 + rng.Int63n(1000)
		}
		if r.Monero {
			o.AmountPiconero = 1 + rng.Int63n(1_000_000)
		}
		q := testQuote()
		q.Price, q.Currency = r.Price, r.Currency
		q.Rates = nil // the rates of the request's rails
		if r.Lightning || r.Onchain {
			q.Rates = append(q.Rates, btcRate())
		}
		if r.Monero {
			q.Rates = append(q.Rates, xmrRate())
		}
		got, created, cerr := s.CreateQuotedOrder(ctx, o, q)
		switch {
		case perr == nil:
			repeats++
			if cerr != nil || created || got != before {
				t.Fatalf("%+v: PricedRepeat gave %+v, CreateQuotedOrder %+v created %v err %v", r, before, got, created, cerr)
			}
		case errors.Is(perr, ErrConflict):
			conflicts++
			if !errors.Is(cerr, ErrConflict) {
				t.Fatalf("%+v: PricedRepeat gave ErrConflict, CreateQuotedOrder created %v err %v", r, created, cerr)
			}
		case errors.Is(perr, ErrNotFound):
			news++
			if cerr != nil || !created {
				t.Fatalf("%+v: PricedRepeat gave ErrNotFound, CreateQuotedOrder created %v err %v", r, created, cerr)
			}
		default:
			t.Fatalf("%+v: %v", r, perr)
		}
	}
	if repeats < 150 || conflicts < 500 || news < 20 {
		t.Fatalf("draws: %d repeats, %d conflicts, %d new", repeats, conflicts, news)
	}
}
