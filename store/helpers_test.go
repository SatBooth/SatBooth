package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0).UTC()

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func freshStore(t *testing.T) (*Store, string, *testClock) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	clk := &testClock{t: t0}
	s, err := Open(context.Background(), path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path, clk
}

// hash returns a valid payment hash made of the one hex digit d.
func hash(d byte) string { return strings.Repeat(string(d), 64) }

func mkOrder(t *testing.T, s *Store, ref string, msat int64, expires time.Time) Order {
	t.Helper()
	o, created, err := s.CreateOrder(context.Background(), Order{AccountID: "acct-1", MerchantRef: ref, AmountMsat: msat, ExpiresAt: expires})
	if err != nil || !created {
		t.Fatalf("CreateOrder(%q): created %v, err %v", ref, created, err)
	}
	return o
}

func mkInvoice(t *testing.T, s *Store, o Order, h byte, expires time.Time) Invoice {
	t.Helper()
	inv := Invoice{OrderID: o.ID, PaymentHash: hash(h), Bolt11: "lnbc1invoice" + string(h), AmountMsat: o.AmountMsat, CreatedAt: t0, ExpiresAt: expires}
	if err := s.AddInvoice(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	return inv
}

// openRawForTest opens path with no pragmas and no connection limit, as an operator's sqlite3
// shell would, to test that the schema protects itself on any connection.
func openRawForTest(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", "file:"+escapePath(path))
}

// agreeAddr fails if ValidAddress and the SQL address check disagree about s.
func (c *sqlChecks) agreeAddr(t testing.TB, s string) {
	t.Helper()
	if got := c.eval(t, checkAddress("?1"), s); got != ValidAddress(s) {
		t.Fatalf("address(%q): Go says %v, SQL says %v", s, ValidAddress(s), got)
	}
}

// agreeMark fails if the Go mark validators and the SQL checks disagree on (kind, height, hash).
func (c *sqlChecks) agreeMark(t testing.TB, kind string, height int64, hash string) {
	t.Helper()
	var ok sql.NullBool
	if err := c.db.QueryRow(`SELECT `+checkMarkShape("?1", "?2", "?3"), kind, height, hash).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if got := ok.Valid && ok.Bool; got != validMarkShape(kind, height, hash) {
		t.Fatalf("mark(%q,%d,%q): Go says %v, SQL says %v", kind, height, hash, validMarkShape(kind, height, hash), got)
	}
}

// mkBothOrder makes an order with a Lightning and an on-chain amount.
func mkBothOrder(t *testing.T, s *Store, ref string, msat, sat int64, expires time.Time) Order {
	t.Helper()
	o, created, err := s.CreateOrder(context.Background(), Order{AccountID: "acct-1", MerchantRef: ref, AmountMsat: msat, AmountSat: sat, ExpiresAt: expires})
	if err != nil || !created {
		t.Fatalf("CreateOrder(%q): created %v, err %v", ref, created, err)
	}
	return o
}

// testQuote returns a valid quote of 21.50 USD with one BTC rate, 76005.65 from two sources, with whole-second times.
func testQuote() Quote {
	return Quote{Price: "21.50", Currency: "USD", MarginBps: 100, QuotedAt: t0.Add(-10 * time.Second),
		Rates: []AssetRate{btcRate()}}
}

// btcRate is testQuote's BTC rate.
func btcRate() AssetRate {
	return AssetRate{Asset: "BTC", Rate: "76005.65", Sources: []QuoteSource{
		{Provider: "kraken", Rate: "76005.65", At: t0.Add(-30 * time.Second)},
		{Provider: "coinbase", Rate: "76010.1", At: t0.Add(-20 * time.Second)},
	}}
}

// xmrRate is an XMR rate of 150.25 from two sources.
func xmrRate() AssetRate {
	return AssetRate{Asset: "XMR", Rate: "150.25", Sources: []QuoteSource{
		{Provider: "kraken", Rate: "150.2", At: t0.Add(-25 * time.Second)},
		{Provider: "bitfinex", Rate: "150.3", At: t0.Add(-15 * time.Second)},
	}}
}

// testQuote2 is testQuote with the XMR rate too, given in the order XMR, BTC (the store returns them sorted by asset).
func testQuote2() Quote {
	q := testQuote()
	q.Rates = []AssetRate{xmrRate(), btcRate()}
	return q
}

// moneroQuote is testQuote with the XMR rate only.
func moneroQuote() Quote {
	q := testQuote()
	q.Rates = []AssetRate{xmrRate()}
	return q
}

// mkQuotedOrder makes a priced order with the rails of msat and sat and the quote q.
func mkQuotedOrder(t *testing.T, s *Store, ref string, msat, sat int64, expires time.Time, q Quote) Order {
	t.Helper()
	return mkQuoted(t, s, Order{AccountID: "acct-1", MerchantRef: ref, AmountMsat: msat, AmountSat: sat, ExpiresAt: expires}, q)
}

// mkQuoted makes the priced order o (any rails) with the quote q.
func mkQuoted(t *testing.T, s *Store, o Order, q Quote) Order {
	t.Helper()
	got, created, err := s.CreateQuotedOrder(context.Background(), o, q)
	if err != nil || !created {
		t.Fatalf("CreateQuotedOrder(%q): created %v, err %v", o.MerchantRef, created, err)
	}
	return got
}

// stored returns q as the store gives it back for the order id: the id filled, the times whole seconds in UTC, the
// rates sorted by asset.
func stored(q Quote, id string) Quote {
	q.OrderID = id
	q.QuotedAt = time.Unix(q.QuotedAt.Unix(), 0).UTC()
	rs := make([]AssetRate, len(q.Rates))
	for i, r := range q.Rates {
		src := make([]QuoteSource, len(r.Sources))
		for j, x := range r.Sources {
			x.At = time.Unix(x.At.Unix(), 0).UTC()
			src[j] = x
		}
		r.Sources = src
		rs[i] = r
	}
	slices.SortFunc(rs, func(a, b AssetRate) int { return strings.Compare(a.Asset, b.Asset) })
	q.Rates = rs
	return q
}

// sameQuote fails unless got equals want field by field, the times with Equal and in UTC.
func sameQuote(t *testing.T, got, want Quote) {
	t.Helper()
	ok := got.OrderID == want.OrderID && got.Price == want.Price && got.Currency == want.Currency &&
		got.MarginBps == want.MarginBps && got.QuotedAt.Equal(want.QuotedAt) && got.QuotedAt.Location() == time.UTC &&
		len(got.Rates) == len(want.Rates)
	for i := 0; ok && i < len(got.Rates); i++ {
		g, w := got.Rates[i], want.Rates[i]
		ok = g.Asset == w.Asset && g.Rate == w.Rate && len(g.Sources) == len(w.Sources)
		for j := 0; ok && j < len(g.Sources); j++ {
			gs, ws := g.Sources[j], w.Sources[j]
			ok = gs.Provider == ws.Provider && gs.Rate == ws.Rate && gs.At.Equal(ws.At) && gs.At.Location() == time.UTC
		}
	}
	if !ok {
		t.Fatalf("quote\n got %+v\nwant %+v", got, want)
	}
}

// b58 is the base58 alphabet of Monero addresses.
const b58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// sub returns a valid regtest subaddress (95 base58 bytes starting 8) that encodes i in its last 6 bytes.
func sub(i int) string {
	tail := make([]byte, 6)
	for k := 5; k >= 0; k-- {
		tail[k] = b58[i%58]
		i /= 58
	}
	return "8" + strings.Repeat("u", 88) + string(tail)
}

// mkMoneroOrder makes a Monero-only order (AmountPiconero set, the other amounts 0).
func mkMoneroOrder(t *testing.T, s *Store, ref string, pico int64, expires time.Time) Order {
	t.Helper()
	o, created, err := s.CreateOrder(context.Background(), Order{AccountID: "acct-1", MerchantRef: ref, AmountPiconero: pico, ExpiresAt: expires})
	if err != nil || !created {
		t.Fatalf("CreateOrder(%q): %v %v", ref, created, err)
	}
	return o
}

// mkSub stores the subaddress sub(idx) at index idx for o.
func mkSub(t *testing.T, s *Store, o Order, idx int64) Subaddress {
	t.Helper()
	a := Subaddress{OrderID: o.ID, Address: sub(int(idx)), Index: idx}
	if err := s.AddSubaddress(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

// agreeSubaddr fails if ValidSubaddress and checkSubaddress disagree about s.
func (c *sqlChecks) agreeSubaddr(t testing.TB, s string) {
	t.Helper()
	if got := c.eval(t, checkSubaddress("?1"), s); got != ValidSubaddress(s) {
		t.Fatalf("subaddress(%q): Go says %v, SQL says %v", s, ValidSubaddress(s), got)
	}
}

// agreeMoneroMark fails if validMoneroMarkShape and checkMoneroMarkShape disagree on (kind, height, confirmations).
func (c *sqlChecks) agreeMoneroMark(t testing.TB, kind string, height, confirmations int64) {
	t.Helper()
	var ok sql.NullBool
	if err := c.db.QueryRow(`SELECT `+checkMoneroMarkShape("?1", "?2", "?3"), kind, height, confirmations).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if got := ok.Valid && ok.Bool; got != validMoneroMarkShape(kind, height, confirmations) {
		t.Fatalf("monero mark(%q,%d,%d): Go says %v, SQL says %v", kind, height, confirmations,
			validMoneroMarkShape(kind, height, confirmations), got)
	}
}

// agreePiconero fails if validPiconero and checkPiconero disagree about n.
func (c *sqlChecks) agreePiconero(t testing.TB, n int64) {
	t.Helper()
	if got := c.eval(t, checkPiconero("?1"), n); got != validPiconero(n) {
		t.Fatalf("piconero %d: Go says %v, SQL says %v", n, validPiconero(n), got)
	}
}
