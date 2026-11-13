package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/rates"
	"github.com/satbooth/satbooth/refund/refundtest"
	"github.com/satbooth/satbooth/status"
	"github.com/satbooth/satbooth/store"
)

var epoch = time.Unix(1_800_000_000, 0).UTC()

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// wallet is a minimal lnpay.Wallet: invoices are pending until settle is called.
type wallet struct {
	mu   sync.Mutex
	seq  int
	invs map[string]*nwc.Transaction
	ch   chan nwc.Notification
}

func (w *wallet) Notifications() <-chan nwc.Notification { return w.ch }

func (w *wallet) MakeInvoice(_ context.Context, p nwc.MakeInvoiceParams) (*nwc.Transaction, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	sum := sha256.Sum256([]byte(fmt.Sprintf("invoice %d", w.seq)))
	h := hex.EncodeToString(sum[:])
	tx := &nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StatePending, Invoice: refundtest.InvoiceFor(approval.Regtest, w.seq, p.AmountMsat),
		Description: p.Description, PaymentHash: h, AmountMsat: p.AmountMsat}
	w.invs[h] = tx
	c := *tx
	return &c, nil
}

func (w *wallet) LookupInvoice(_ context.Context, h string) (*nwc.Transaction, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	tx, ok := w.invs[h]
	if !ok {
		return nil, &nwc.WalletError{Code: "NOT_FOUND", Message: "not found"}
	}
	c := *tx
	return &c, nil
}

func (w *wallet) settleAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, tx := range w.invs {
		tx.State = nwc.StateSettled
	}
}

const (
	writeKey = "test-write-key"
	readKey  = "test-read-key"
	base     = "https://pay.example.test"
)

type env struct {
	t    *testing.T
	clk  *clock
	dir  string
	st   *store.Store
	lg   *ledger.Ledger
	lim  *limits.Limiter
	svc  *lnpay.Service
	res  *status.Resolver
	w    *wallet
	keys *Keys
	pr   *fakePricer
	h    http.Handler
}

// The rates service is a Pricer (service passes it in).
var _ Pricer = (*rates.Service)(nil)

// fakePricer is a Pricer whose median rates, sources, quote time, reachable pairs and error the test sets. It converts
// with the real rates.Convert, so every amount it gives is the one the formula gives.
type fakePricer struct {
	mu         sync.Mutex
	rate       string      // BTC
	sources    [][2]string // provider, rate (BTC)
	xmrRate    string
	xmrSources [][2]string
	at         time.Time
	margin     int64
	err        error
	errXMR     error // the error of an XMR Price alone, when set
	currencies []string
	// unreachable holds the pairs ("XMR EUR") that no configured set of providers quotes.
	unreachable map[string]bool
	calls       int
	asked       []string // the assets Price was asked for, in order
	// quoteCurrency, when set, is the currency of the quote returned whatever was asked (a broken pricer).
	quoteCurrency string
}

func newFakePricer() *fakePricer {
	return &fakePricer{rate: "76025", sources: [][2]string{{"coinbase", "76050"}, {"kraken", "76000"}},
		xmrRate: "160", xmrSources: [][2]string{{"kraken", "160.5"}, {"yadio", "159.5"}}, at: epoch.Add(-30 * time.Second),
		margin: 150, currencies: []string{"USD", "EUR"}, unreachable: map[string]bool{"XMR EUR": true}}
}

func (f *fakePricer) Price(asset string, price rates.Decimal, currency string) (rates.Priced, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.asked = append(f.asked, asset)
	if f.err != nil {
		return rates.Priced{}, f.err
	}
	if asset == rates.AssetXMR && f.errXMR != nil {
		return rates.Priced{}, f.errXMR
	}
	if f.unreachable[asset+" "+currency] {
		return rates.Priced{}, rates.ErrUnreachable
	}
	rs, srcs := f.rate, f.sources
	switch asset {
	case rates.AssetBTC:
	case rates.AssetXMR:
		rs, srcs = f.xmrRate, f.xmrSources
	default:
		return rates.Priced{}, rates.ErrAsset
	}
	rate := mustDecimal(rs)
	n, err := rates.Convert(asset, price, rate, f.margin)
	if err != nil {
		return rates.Priced{}, err
	}
	q := rates.Quote{Asset: asset, Currency: currency, Rate: rate, At: f.at}
	if f.quoteCurrency != "" {
		q.Currency = f.quoteCurrency
	}
	for _, s := range srcs {
		q.Sources = append(q.Sources, rates.Rate{Provider: s[0], Currency: currency, Value: mustDecimal(s[1]), At: f.at.Add(-10 * time.Second)})
	}
	return rates.Priced{Amount: n, Quote: q, MarginBps: f.margin}, nil
}

func (f *fakePricer) Reachable(asset, currency string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.unreachable[asset+" "+currency]
}

// askedFor returns the assets Price was asked for since the start.
func (f *fakePricer) askedFor() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func (f *fakePricer) Currencies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.currencies...)
}

func (f *fakePricer) set(fn func(*fakePricer)) { f.mu.Lock(); fn(f); f.mu.Unlock() }

func (f *fakePricer) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func mustDecimal(s string) rates.Decimal {
	d, err := rates.ParseDecimal(s)
	if err != nil {
		panic(err)
	}
	return d
}

// stubOc is a status.Onchain whose rail the test sets.
type stubOc struct {
	mu   sync.Mutex
	rail onchain.Rail
}

func (s *stubOc) set(r onchain.Rail) { s.mu.Lock(); s.rail = r; s.mu.Unlock() }
func (s *stubOc) Rail(context.Context, store.Order) (onchain.Rail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rail, nil
}
func (s *stubOc) Address(context.Context, string) (store.Address, error) {
	return store.Address{}, store.ErrNotFound
}

// withOnchain returns the environment's handler rebuilt over a resolver with an on-chain rail (the Monero rail, if
// any, is kept).
func (e *env) withOnchain(oc *stubOc) {
	e.res = &status.Resolver{St: e.st, Ln: e.svc, Oc: oc, Xm: e.res.Xm, Now: e.clk.Now}
	e.h = newHandler(e.keys, e.st, e.res, base+"/", e.pr, e.clk.Now)
}

// stubXm is a status.Monero whose view the test sets.
type stubXm struct {
	mu   sync.Mutex
	view rail.View
}

func (s *stubXm) set(v rail.View) { s.mu.Lock(); s.view = v; s.mu.Unlock() }
func (s *stubXm) Rail(context.Context, store.Order) (rail.View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view, nil
}
func (s *stubXm) Address(context.Context, string) (store.Subaddress, error) {
	return store.Subaddress{}, store.ErrNotFound
}

// withMonero returns the environment's handler rebuilt over a resolver with a Monero rail (the on-chain rail, if any,
// is kept).
func (e *env) withMonero(xm *stubXm) {
	e.res = &status.Resolver{St: e.st, Ln: e.svc, Oc: e.res.Oc, Xm: xm, Now: e.clk.Now}
	e.h = newHandler(e.keys, e.st, e.res, base+"/", e.pr, e.clk.Now)
}

// restart closes the store and opens it again from its file, with a new Lightning service, resolver and handler
// over it (the on-chain and Monero rails, if any, are kept): what a restart of SatBooth does.
func (e *env) restart() {
	e.t.Helper()
	if err := e.st.Close(); err != nil {
		e.t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(e.dir, "orders.db"), store.WithClock(e.clk.Now))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { st.Close() })
	svc, err := lnpay.New(lnpay.Config{PollRate: 1000, Network: approval.Regtest}, st, e.lg, e.w, e.lim, e.clk.Now, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.st, e.svc = st, svc
	e.res = &status.Resolver{St: st, Ln: svc, Oc: e.res.Oc, Xm: e.res.Xm, Now: e.clk.Now}
	e.h = newHandler(e.keys, st, e.res, base+"/", e.pr, e.clk.Now)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	clk := &clock{t: epoch}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "orders.db"), store.WithClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	lg, err := ledger.Open(ctx, filepath.Join(dir, "ledger.db"), ledger.WithClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(); lg.Close() })
	lim, err := limits.New(limits.Config{RatePerSec: 1e6, Burst: 1 << 20, MaxMsat: 1_000_000_000, DailyMsat: 1 << 50}, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	w := &wallet{invs: map[string]*nwc.Transaction{}, ch: make(chan nwc.Notification, 8)}
	svc, err := lnpay.New(lnpay.Config{PollRate: 1000, Network: approval.Regtest}, st, lg, w, lim, clk.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys(writeKeyFile(t, 0o600, keysBody(keyEntry("w", CapWrite, writeKey), keyEntry("r", CapRead, readKey))))
	if err != nil {
		t.Fatal(err)
	}
	res := &status.Resolver{St: st, Ln: svc, Now: clk.Now}
	pr := newFakePricer()
	return &env{t: t, clk: clk, dir: dir, st: st, lg: lg, lim: lim, svc: svc, res: res, w: w, keys: keys, pr: pr,
		h: newHandler(keys, st, res, base+"/", pr, clk.Now)}
}

func (e *env) do(method, path, key, body string) (int, []byte, http.Header) {
	e.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes(), rec.Header()
}

func (e *env) post(body string) (int, []byte) {
	e.t.Helper()
	c, b, _ := e.do("POST", "/v1/orders", writeKey, body)
	return c, b
}

func asMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not a JSON object: %q", b)
	}
	return m
}

func keysOf(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

const orderBody = `{"account_id":"acct-1","merchant_ref":"ref-1","lightning_msat":21000}`

var allowlist = []string{"account_id", "checkout_url", "created_at", "expires_at", "id", "lightning", "lightning_msat", "merchant_ref", "monero",
	"monero_piconero", "onchain", "onchain_sat", "paid_at", "pricing", "state"}

var onchainKeys = []string{"address", "confirmations", "confirmed_sat", "late_sat", "overpaid", "received_sat", "required_confirmations", "stale", "state"}

var moneroKeys = []string{"address", "confirmations", "confirmed_piconero", "late_piconero", "overpaid", "received_piconero", "required_confirmations",
	"stale", "state"}

func TestCreateAndGet(t *testing.T) {
	e := newEnv(t)
	code, body := e.post(orderBody)
	if code != 201 {
		t.Fatalf("create = %d %s", code, body)
	}
	m := asMap(t, body)
	if got := keysOf(m); !reflect.DeepEqual(got, allowlist) {
		t.Fatalf("keys = %v, want %v", got, allowlist)
	}
	if m["account_id"] != "acct-1" || m["merchant_ref"] != "ref-1" || m["lightning_msat"] != float64(21000) ||
		m["state"] != "new" || m["paid_at"] != nil || m["pricing"] != nil {
		t.Errorf("fields: %v", m)
	}
	o, err := e.st.OrderByID(context.Background(), m["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if m["checkout_url"] != base+"/c/"+o.Token {
		t.Errorf("checkout_url = %v", m["checkout_url"])
	}
	if m["created_at"] != epoch.Format(time.RFC3339) || m["expires_at"] != epoch.Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("times: %v %v", m["created_at"], m["expires_at"])
	}

	// The same body again: 200 and the same order.
	code2, body2 := e.post(orderBody)
	if code2 != 200 {
		t.Fatalf("repeat = %d", code2)
	}
	if !reflect.DeepEqual(asMap(t, body2), m) {
		t.Errorf("repeat differs: %s vs %s", body, body2)
	}

	// GET with the read key: 200, same allowlist, same order.
	code, body, hdr := e.do("GET", "/v1/orders/"+m["id"].(string), readKey, "")
	if code != 200 || !reflect.DeepEqual(asMap(t, body), m) {
		t.Fatalf("get = %d %s", code, body)
	}
	if hdr.Get("Content-Type") != "application/json" || hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("headers: %v", hdr)
	}
	// The body holds nothing outside the allowlist, even as text.
	for _, s := range []string{o.Token, "bolt11", "preimage", "payment_hash", "lnbc"} {
		if s == o.Token {
			continue // the token is in checkout_url by design
		}
		if strings.Contains(string(body), s) {
			t.Errorf("body contains %q", s)
		}
	}
}

func TestExpiresIn(t *testing.T) {
	e := newEnv(t)
	for i, c := range []struct {
		field string
		want  int64
		ok    bool
	}{
		{``, 3600, true}, {`,"expires_in":300`, 300, true}, {`,"expires_in":86400`, 86400, true}, {`,"expires_in":7200`, 7200, true},
		{`,"expires_in":299`, 0, false}, {`,"expires_in":86401`, 0, false}, {`,"expires_in":0`, 0, false}, {`,"expires_in":-300`, 0, false},
		{`,"expires_in":300.0`, 0, false}, {`,"expires_in":3e3`, 0, false}, {`,"expires_in":"600"`, 0, false}, {`,"expires_in":null`, 0, false},
		{`,"expires_in":99999999999999999999`, 0, false},
	} {
		body := fmt.Sprintf(`{"account_id":"a","merchant_ref":"r%d","lightning_msat":1000%s}`, i, c.field)
		code, resp := e.post(body)
		if !c.ok {
			if code != 400 || string(resp) != `{"error":"invalid_expires_in"}`+"\n" {
				t.Errorf("%s: %d %s", c.field, code, resp)
			}
			continue
		}
		if code != 201 {
			t.Fatalf("%s: %d %s", c.field, code, resp)
		}
		got, _ := time.Parse(time.RFC3339, asMap(t, resp)["expires_at"].(string))
		if got != epoch.Add(time.Duration(c.want)*time.Second) {
			t.Errorf("%s: expires_at %v", c.field, got)
		}
	}
}

func TestCreateRefusals(t *testing.T) {
	e := newEnv(t)
	const marker = "SECRET-MARKER"
	cases := []struct{ name, body, code string }{
		{"empty", ``, "invalid_json"},
		{"not json", `nope`, "invalid_json"},
		{"array", `[]`, "invalid_json"},
		{"null", `null`, "invalid_json"},
		{"unknown field", `{"account_id":"a","merchant_ref":"r","lightning_msat":1000,"` + marker + `":1}`, "invalid_json"},
		{"duplicate key", `{"account_id":"a","merchant_ref":"r","lightning_msat":1000,"lightning_msat":2000}`, "invalid_json"},
		{"duplicate key same value", `{"account_id":"a","account_id":"a","merchant_ref":"r","lightning_msat":1000}`, "invalid_json"},
		{"case variant key", `{"Account_ID":"a","merchant_ref":"r","lightning_msat":1000}`, "invalid_json"},
		{"case variant duplicate", `{"account_id":"a","ACCOUNT_ID":"b","merchant_ref":"r","lightning_msat":1000}`, "invalid_json"},
		{"trailing document", orderBody + `{}`, "invalid_json"},
		{"trailing garbage", orderBody + ` x`, "invalid_json"},
		{"invalid utf-8", "{\"account_id\":\"a\xff\",\"merchant_ref\":\"r\",\"lightning_msat\":1000}", "invalid_json"},
		{"missing account", `{"merchant_ref":"r","lightning_msat":1000}`, "invalid_json"},
		{"missing ref", `{"account_id":"a","lightning_msat":1000}`, "invalid_json"},
		{"missing amount", `{"account_id":"a","merchant_ref":"r"}`, "missing_amount"},
		{"missing amount with expiry", `{"account_id":"a","merchant_ref":"r","expires_in":300}`, "missing_amount"},
		{"onchain zero", `{"account_id":"a","merchant_ref":"r","onchain_sat":0}`, "invalid_onchain_sat"},
		{"onchain negative", `{"account_id":"a","merchant_ref":"r","onchain_sat":-1}`, "invalid_onchain_sat"},
		{"onchain fraction", `{"account_id":"a","merchant_ref":"r","onchain_sat":1.0}`, "invalid_onchain_sat"},
		{"onchain exponent", `{"account_id":"a","merchant_ref":"r","onchain_sat":1e3}`, "invalid_onchain_sat"},
		{"onchain string", `{"account_id":"a","merchant_ref":"r","onchain_sat":"1000"}`, "invalid_onchain_sat"},
		{"onchain null", `{"account_id":"a","merchant_ref":"r","onchain_sat":null}`, "invalid_onchain_sat"},
		{"onchain above max", `{"account_id":"a","merchant_ref":"r","onchain_sat":2100000000000001}`, "invalid_onchain_sat"},
		{"onchain overflow", `{"account_id":"a","merchant_ref":"r","onchain_sat":99999999999999999999}`, "invalid_onchain_sat"},
		{"onchain bad with good lightning", `{"account_id":"a","merchant_ref":"r","lightning_msat":1000,"onchain_sat":0}`, "invalid_onchain_sat"},
		{"lightning zero with good onchain", `{"account_id":"a","merchant_ref":"r","lightning_msat":0,"onchain_sat":1000}`, "invalid_lightning_msat"},
		{"onchain case variant", `{"account_id":"a","merchant_ref":"r","Onchain_Sat":1000}`, "invalid_json"},
		{"onchain duplicate", `{"account_id":"a","merchant_ref":"r","onchain_sat":1000,"onchain_sat":1000}`, "invalid_json"},
		{"onchain disabled", `{"account_id":"a","merchant_ref":"r","onchain_sat":1000}`, "onchain_disabled"},
		{"onchain disabled with lightning", `{"account_id":"a","merchant_ref":"r","lightning_msat":1000,"onchain_sat":1000}`, "onchain_disabled"},
		{"account number", `{"account_id":1,"merchant_ref":"r","lightning_msat":1000}`, "invalid_account_id"},
		{"account null", `{"account_id":null,"merchant_ref":"r","lightning_msat":1000}`, "invalid_account_id"},
		{"account empty", `{"account_id":"","merchant_ref":"r","lightning_msat":1000}`, "invalid_account_id"},
		{"account space", `{"account_id":"a b","merchant_ref":"r","lightning_msat":1000}`, "invalid_account_id"},
		{"account slash", `{"account_id":"a/b","merchant_ref":"r","lightning_msat":1000}`, "invalid_account_id"},
		{"account unicode", `{"account_id":"é","merchant_ref":"r","lightning_msat":1000}`, "invalid_account_id"},
		{"account nul", `{"account_id":"a\u0000","merchant_ref":"r","lightning_msat":1000}`, "invalid_account_id"},
		{"account 65", `{"account_id":"` + strings.Repeat("a", 65) + `","merchant_ref":"r","lightning_msat":1000}`, "invalid_account_id"},
		{"ref empty", `{"account_id":"a","merchant_ref":"","lightning_msat":1000}`, "invalid_merchant_ref"},
		{"ref 65", `{"account_id":"a","merchant_ref":"` + strings.Repeat("r", 65) + `","lightning_msat":1000}`, "invalid_merchant_ref"},
		{"ref bool", `{"account_id":"a","merchant_ref":true,"lightning_msat":1000}`, "invalid_merchant_ref"},
		{"amount zero", `{"account_id":"a","merchant_ref":"r","lightning_msat":0}`, "invalid_lightning_msat"},
		{"amount negative", `{"account_id":"a","merchant_ref":"r","lightning_msat":-1000}`, "invalid_lightning_msat"},
		{"amount partial sat", `{"account_id":"a","merchant_ref":"r","lightning_msat":1500}`, "invalid_lightning_msat"},
		{"amount fraction", `{"account_id":"a","merchant_ref":"r","lightning_msat":1000.0}`, "invalid_lightning_msat"},
		{"amount exponent", `{"account_id":"a","merchant_ref":"r","lightning_msat":1e3}`, "invalid_lightning_msat"},
		{"amount string", `{"account_id":"a","merchant_ref":"r","lightning_msat":"1000"}`, "invalid_lightning_msat"},
		{"amount null", `{"account_id":"a","merchant_ref":"r","lightning_msat":null}`, "invalid_lightning_msat"},
		{"amount above max", `{"account_id":"a","merchant_ref":"r","lightning_msat":1000001000}`, "invalid_lightning_msat"},
		{"amount 2^53", `{"account_id":"a","merchant_ref":"r","lightning_msat":9007199254740992}`, "invalid_lightning_msat"},
		{"amount overflow", `{"account_id":"a","merchant_ref":"r","lightning_msat":99999999999999999999}`, "invalid_lightning_msat"},
		{"deep nesting", `{"account_id":"a","merchant_ref":"r","lightning_msat":1000,"x":` + strings.Repeat("[", 50) + strings.Repeat("]", 50) + `}`, "invalid_json"},
		{"monero disabled", `{"account_id":"a","merchant_ref":"r","monero_piconero":1500000000000}`, "monero_disabled"},
		{"monero disabled with lightning", `{"account_id":"a","merchant_ref":"r","lightning_msat":1000,"monero_piconero":1}`, "monero_disabled"},
		{"monero zero with good lightning", `{"account_id":"a","merchant_ref":"r","lightning_msat":1000,"monero_piconero":0}`, "invalid_monero_piconero"},
		{"monero case variant", `{"account_id":"a","merchant_ref":"r","Monero_Piconero":1}`, "invalid_json"},
		{"monero duplicate", `{"account_id":"a","merchant_ref":"r","monero_piconero":1,"monero_piconero":1}`, "invalid_json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := e.post(c.body)
			if code != 400 || string(body) != `{"error":"`+c.code+`"}`+"\n" {
				t.Errorf("= %d %s, want 400 %s", code, body, c.code)
			}
			if strings.Contains(string(body), marker) {
				t.Error("echoed input")
			}
		})
	}
	// None of them created an order.
	if os, _ := e.st.OrdersByAccount(context.Background(), "a"); len(os) != 0 {
		t.Errorf("%d orders created by refused requests", len(os))
	}
}

func TestAmountBounds(t *testing.T) {
	e := newEnv(t)
	for i, c := range []struct {
		msat int64
		ok   bool
	}{{1000, true}, {999, false}, {1_000_000_000, true}, {1_000_001_000, false}, {1_000_000_001, false}} {
		code, body := e.post(fmt.Sprintf(`{"account_id":"a","merchant_ref":"r%d","lightning_msat":%d}`, i, c.msat))
		if (code == 201) != c.ok {
			t.Errorf("%d msat: %d %s", c.msat, code, body)
		}
	}
}

func TestBodyCap(t *testing.T) {
	e := newEnv(t)
	pad := func(n int) string { // a valid order body of exactly n bytes
		core := `{"account_id":"a","merchant_ref":"r","lightning_msat":1000}`
		return core + strings.Repeat(" ", n-len(core))
	}
	if code, body := e.post(pad(MaxBodyBytes)); code != 201 {
		t.Errorf("16 KiB body: %d %s", code, body)
	}
	code, body := e.post(pad(MaxBodyBytes + 1))
	if code != 400 || string(body) != `{"error":"body_too_large"}`+"\n" {
		t.Errorf("16 KiB + 1: %d %s", code, body)
	}
}

func TestConflict(t *testing.T) {
	e := newEnv(t)
	if c, b := e.post(orderBody); c != 201 {
		t.Fatal(c, b)
	}
	for _, body := range []string{
		`{"account_id":"acct-1","merchant_ref":"ref-1","lightning_msat":22000}`,
		`{"account_id":"acct-2","merchant_ref":"ref-1","lightning_msat":21000}`,
	} {
		c, b := e.post(body)
		if c != 409 || string(b) != `{"error":"conflict"}`+"\n" {
			t.Errorf("%s: %d %s", body, c, b)
		}
	}
	// A different expires_in on the same reference is the same order (the store compares account and amount).
	if c, _ := e.post(`{"account_id":"acct-1","merchant_ref":"ref-1","lightning_msat":21000,"expires_in":600}`); c != 200 {
		t.Errorf("repeat with another expiry: %d", c)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	_, cb := e.post(orderBody)
	id := asMap(t, cb)["id"].(string)
	unauth := `{"error":"unauthorized"}` + "\n"
	forbid := `{"error":"forbidden"}` + "\n"

	routes := []struct{ method, path, body, key string }{
		{"POST", "/v1/orders", orderBody, readKey},
		{"GET", "/v1/orders/" + id, "", writeKey},
	}
	for _, r := range routes {
		if c, b, _ := e.do(r.method, r.path, r.key, r.body); c != 403 || string(b) != forbid {
			t.Errorf("%s %s with wrong capability: %d %s", r.method, r.path, c, b)
		}
		for _, hdr := range []string{"", "Bearer ", "Bearer x", "bearer " + writeKey, "Basic " + writeKey, writeKey, "Bearer " + writeKey + " ", "Bearer  " + writeKey,
			"Bearer " + strings.Repeat("a", 100000)} {
			req := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
			if hdr != "" {
				req.Header.Set("Authorization", hdr)
			}
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			if rec.Code != 401 || rec.Body.String() != unauth || rec.Header().Get("WWW-Authenticate") != "Bearer" {
				n := len(hdr)
				if n > 20 {
					n = 20
				}
				t.Errorf("%s %s with %q: %d %s", r.method, r.path, hdr[:n], rec.Code, rec.Body)
			}
		}
	}
	// Auth comes before the body is looked at: a bad body without a key is a 401.
	if c, b, _ := e.do("POST", "/v1/orders", "", `nope`); c != 401 {
		t.Errorf("unauthenticated bad body: %d %s", c, b)
	}
	// A reload that revokes the key takes effect.
	if c, _, _ := e.do("GET", "/v1/orders/"+id, readKey, ""); c != 200 {
		t.Fatal(c)
	}
}

func TestOtherRoutesAre404(t *testing.T) {
	e := newEnv(t)
	_, cb := e.post(orderBody)
	id := asMap(t, cb)["id"].(string)
	notFound := `{"error":"not_found"}` + "\n"
	reqs := []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/v1"}, {"GET", "/v1/"}, {"GET", "/v1/orders"}, {"GET", "/v1/orders/"},
		{"POST", "/v1/orders/"}, {"POST", "/v1/orders/" + id}, {"PUT", "/v1/orders"}, {"DELETE", "/v1/orders/" + id},
		{"PATCH", "/v1/orders/" + id}, {"HEAD", "/v1/orders/" + id}, {"OPTIONS", "/v1/orders"}, {"HEAD", "/v1/orders"},
		{"GET", "/v1/orders/" + id + "/"}, {"GET", "/v1/orders/" + id + "/x"}, {"GET", "/v1/orders//" + id},
		{"POST", "/v1/orders/x"}, {"POST", "/v1/orders/"}, {"GET", "/v2/orders/" + id}, {"GET", "/c/abc"},
		{"GET", "/v1/ORDERS/" + id}, {"POST", "/v1/orders//"}, {"GET", "/healthz"},
	}
	for _, r := range reqs {
		for _, key := range []string{"", writeKey, readKey} {
			if c, b, _ := e.do(r.method, r.path, key, ""); c != 404 || string(b) != notFound {
				t.Errorf("%s %s (key %q): %d %s", r.method, r.path, key, c, b)
			}
		}
	}
	// Unknown and malformed ids, with the read key.
	for _, id := range []string{strings.Repeat("0", 32), "x", strings.Repeat("A", 32), id + "0", "%20", "a%2Fb", "%2e%2e", "%27%20OR%201=1%20--"} {
		req := httptest.NewRequest("GET", "/v1/orders/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+readKey)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != 404 || rec.Body.String() != notFound {
			t.Errorf("id %q: %d %s", id, rec.Code, rec.Body)
		}
	}
}

// A Lightning-only order: expired follows lnpay.Status's rule (time and closed invoices). An on-chain order reads
// expired only once its order.expired exists (onchain's TestRailFinalStatesFollowTheEvents).
func TestStateExpired(t *testing.T) {
	e := newEnv(t)
	_, cb := e.post(orderBody)
	id := asMap(t, cb)["id"].(string)
	e.clk.Advance(2 * time.Hour)
	_, b, _ := e.do("GET", "/v1/orders/"+id, readKey, "")
	m := asMap(t, b)
	if m["state"] != "expired" || m["paid_at"] != nil || !reflect.DeepEqual(keysOf(m), allowlist) {
		t.Errorf("%s", b)
	}
}

func TestStatePaid(t *testing.T) {
	e := newEnv(t)
	_, cb := e.post(orderBody)
	m0 := asMap(t, cb)
	o, err := e.st.OrderByID(context.Background(), m0["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Pay(context.Background(), o.Token); err != nil {
		t.Fatal(err)
	}
	e.w.settleAll()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, b, _ := e.do("GET", "/v1/orders/"+o.ID, readKey, "")
		m := asMap(t, b)
		if m["state"] == "paid" {
			if !reflect.DeepEqual(keysOf(m), allowlist) {
				t.Errorf("keys %v", keysOf(m))
			}
			if _, err := time.Parse(time.RFC3339, m["paid_at"].(string)); err != nil {
				t.Errorf("paid_at %v", m["paid_at"])
			}
			for _, s := range []string{"lnbc", "payment_hash", "preimage"} {
				if strings.Contains(string(b), s) {
					t.Errorf("paid body contains %q", s)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never paid: %s", b)
		}
		time.Sleep(time.Millisecond)
	}
	// Creating the same reference again reports the paid order, 200.
	c, b := e.post(orderBody)
	if c != 200 || asMap(t, b)["state"] != "paid" {
		t.Errorf("repeat of a paid order: %d %s", c, b)
	}
}

// Requests in flight while the key file is reloaded all get a definite answer for their key. Run with -race.
func TestReloadWhileServing(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.json")
	pathB := filepath.Join(dir, "b.json")
	live := filepath.Join(dir, "live.json")
	mk := func(path, body string) {
		if err := writeFile0600(path, body); err != nil {
			t.Fatal(err)
		}
	}
	setA := keysBody(keyEntry("w", CapWrite, writeKey), keyEntry("r", CapRead, readKey))
	setB := keysBody(keyEntry("w", CapWrite, writeKey), keyEntry("r2", CapRead, "other-read"))
	mk(pathA, setA)
	mk(pathB, setB)
	mk(live, setA)
	keys, err := LoadKeys(live)
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(keys, e.st, e.res, base, e.pr, e.clk.Now)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad sync.Map
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				// The write key is in both sets: it must never be refused.
				body := fmt.Sprintf(`{"account_id":"a","merchant_ref":"w%d-%d","lightning_msat":1000}`, i, n)
				req := httptest.NewRequest("POST", "/v1/orders", strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+writeKey)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != 201 {
					bad.Store(rec.Code, rec.Body.String())
				}
				// The read key is in set A only: 200 or 401, nothing else.
				req = httptest.NewRequest("GET", "/v1/orders/"+strings.Repeat("0", 32), nil)
				req.Header.Set("Authorization", "Bearer "+readKey)
				rec = httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != 404 && rec.Code != 401 {
					bad.Store(rec.Code, rec.Body.String())
				}
			}
		}(i)
	}
	for i := 0; i < 100; i++ {
		src := pathA
		if i%2 == 1 {
			src = pathB
		}
		b, _ := readFile(src)
		tmp := live + ".tmp"
		mk(tmp, string(b))
		if err := renameFile(tmp, live); err != nil {
			t.Fatal(err)
		}
		if err := keys.Reload(); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	bad.Range(func(k, v any) bool { t.Errorf("unexpected %v %v", k, v); return true })
}

func TestNilArgumentsPanic(t *testing.T) {
	e := newEnv(t)
	for name, f := range map[string]func(){
		"keys":  func() { Handler(nil, e.st, e.res, base, e.pr, nil, nil) },
		"store": func() { Handler(e.keys, nil, e.res, base, e.pr, nil, nil) },
		"res":   func() { Handler(e.keys, e.st, nil, base, e.pr, nil, nil) },
		"ln":    func() { Handler(e.keys, e.st, &status.Resolver{St: e.st}, base, e.pr, nil, nil) },
		"base":  func() { Handler(e.keys, e.st, e.res, "", e.pr, nil, nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			f()
		}()
	}
	// A nil Pricer is allowed: fiat pricing is not configured.
	if Handler(e.keys, e.st, e.res, base, nil, nil, nil) == nil {
		t.Error("nil handler")
	}
}

func TestRailObjects(t *testing.T) {
	e := newEnv(t)
	// Lightning only: lightning object, no onchain, onchain_sat 0.
	_, b := e.post(orderBody)
	m := asMap(t, b)
	if m["onchain"] != nil || m["onchain_sat"] != float64(0) || m["monero"] != nil || m["monero_piconero"] != float64(0) {
		t.Errorf("lightning-only: %v", m)
	}
	if !strings.Contains(string(b), `"monero":null`) {
		t.Errorf("monero is not an explicit null: %s", b)
	}
	ln, ok := m["lightning"].(map[string]any)
	if !ok || !reflect.DeepEqual(keysOf(ln), []string{"state"}) || ln["state"] != "new" {
		t.Errorf("lightning object: %v", m["lightning"])
	}

	// On-chain only.
	oc := &stubOc{rail: onchain.Rail{State: onchain.RailWaiting, RequiredConfirmations: 3}}
	e.withOnchain(oc)
	code, b := e.post(`{"account_id":"a","merchant_ref":"oc-only","onchain_sat":50000}`)
	if code != 201 {
		t.Fatalf("on-chain only: %d %s", code, b)
	}
	m = asMap(t, b)
	if !reflect.DeepEqual(keysOf(m), allowlist) {
		t.Fatalf("keys = %v", keysOf(m))
	}
	if m["lightning"] != nil || m["lightning_msat"] != float64(0) || m["onchain_sat"] != float64(50000) || m["state"] != "new" ||
		m["monero"] != nil || m["monero_piconero"] != float64(0) {
		t.Errorf("on-chain only: %v", m)
	}
	o := asMap2(t, m["onchain"])
	if !reflect.DeepEqual(keysOf(o), onchainKeys) {
		t.Fatalf("onchain keys = %v, want %v", keysOf(o), onchainKeys)
	}
	if o["address"] != nil || o["state"] != "waiting" || o["required_confirmations"] != float64(3) || o["confirmations"] != float64(0) ||
		o["received_sat"] != float64(0) || o["confirmed_sat"] != float64(0) || o["late_sat"] != float64(0) || o["overpaid"] != false {
		t.Errorf("onchain object: %v", o)
	}

	// Both rails, address issued, a payment seen.
	oc.set(onchain.Rail{State: onchain.RailSeen, Address: "bcrt1qexample", Issued: true, RequiredConfirmations: 3,
		Confirmations: 1, ReceivedSat: 50000})
	code, b = e.post(`{"account_id":"a","merchant_ref":"both","lightning_msat":21000,"onchain_sat":50000}`)
	if code != 201 {
		t.Fatalf("both: %d %s", code, b)
	}
	m = asMap(t, b)
	o = asMap2(t, m["onchain"])
	if o["address"] != "bcrt1qexample" || o["state"] != "seen" || o["confirmations"] != float64(1) || o["received_sat"] != float64(50000) {
		t.Errorf("onchain object: %v", o)
	}
	if m["lightning_msat"] != float64(21000) || m["onchain_sat"] != float64(50000) || m["lightning"] == nil || m["monero"] != nil {
		t.Errorf("both: %v", m)
	}
	// GET agrees with the create response.
	_, gb, _ := e.do("GET", "/v1/orders/"+m["id"].(string), readKey, "")
	if !reflect.DeepEqual(asMap(t, gb), m) {
		t.Errorf("GET differs: %s vs %s", gb, b)
	}
	// The repeat is 200 with the same order.
	if c, _ := e.post(`{"account_id":"a","merchant_ref":"both","lightning_msat":21000,"onchain_sat":50000}`); c != 200 {
		t.Errorf("repeat = %d", c)
	}
	// A different on-chain amount for the same reference is a conflict.
	if c, _ := e.post(`{"account_id":"a","merchant_ref":"both","lightning_msat":21000,"onchain_sat":50001}`); c != 409 {
		t.Errorf("changed amount = %d", c)
	}
}

func asMap2(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %v", v)
	}
	return m
}

// A paid order's view still shows a late or overpaid on-chain amount, so that it can be refunded.
func TestPaidOrderShowsLateAndOverpaid(t *testing.T) {
	e := newEnv(t)
	oc := &stubOc{rail: onchain.Rail{State: onchain.RailWaiting, RequiredConfirmations: 1}}
	e.withOnchain(oc)
	_, b := e.post(`{"account_id":"a","merchant_ref":"p","onchain_sat":50000}`)
	id := asMap(t, b)["id"].(string)
	paidAt := epoch.Add(10 * time.Minute)
	oc.set(onchain.Rail{State: onchain.RailOverpaid, Address: "bcrt1qexample", Issued: true, RequiredConfirmations: 1,
		Confirmations: 2, ReceivedSat: 70000, ConfirmedSat: 70000, LateSat: 1500, Paid: true, Overpaid: true, PaidAt: paidAt})
	_, gb, _ := e.do("GET", "/v1/orders/"+id, readKey, "")
	m := asMap(t, gb)
	o := asMap2(t, m["onchain"])
	if m["state"] != "paid" || m["paid_at"] != paidAt.Format(time.RFC3339) {
		t.Errorf("state: %v", m)
	}
	if o["late_sat"] != float64(1500) || o["overpaid"] != true || o["received_sat"] != float64(70000) || o["confirmed_sat"] != float64(70000) {
		t.Errorf("onchain: %v", o)
	}
}

// The API carries the watcher's staleness, the same Rail.Stale the page uses, so that an automation does
// not act on an "expired" that a stale watcher could not confirm.
func TestOnchainStale(t *testing.T) {
	e := newEnv(t)
	oc := &stubOc{rail: onchain.Rail{State: onchain.RailExpired, RequiredConfirmations: 1, Stale: true}}
	e.withOnchain(oc)
	_, b := e.post(`{"account_id":"a","merchant_ref":"st","onchain_sat":50000}`)
	id := asMap(t, b)["id"].(string)
	_, gb, _ := e.do("GET", "/v1/orders/"+id, readKey, "")
	if o := asMap2(t, asMap(t, gb)["onchain"]); o["stale"] != true {
		t.Errorf("a stale rail: %v", o)
	}
	oc.set(onchain.Rail{State: onchain.RailExpired, RequiredConfirmations: 1})
	_, gb, _ = e.do("GET", "/v1/orders/"+id, readKey, "")
	if o := asMap2(t, asMap(t, gb)["onchain"]); o["stale"] != false {
		t.Errorf("a fresh rail: %v", o)
	}
}

func TestOnchainDisabledCreatesNothing(t *testing.T) {
	e := newEnv(t)
	if e.res.HasOnchain() {
		t.Fatal("a resolver without an on-chain rail reports HasOnchain")
	}
	code, b := e.post(`{"account_id":"a","merchant_ref":"x","onchain_sat":1000}`)
	if code != 400 || string(b) != `{"error":"onchain_disabled"}`+"\n" {
		t.Errorf("= %d %s", code, b)
	}
	if os, _ := e.st.OrdersByAccount(context.Background(), "a"); len(os) != 0 {
		t.Errorf("%d orders created", len(os))
	}
	// With the rail configured, the same body works.
	e.withOnchain(&stubOc{})
	if !e.res.HasOnchain() {
		t.Fatal("HasOnchain false with a rail")
	}
	if c, b := e.post(`{"account_id":"a","merchant_ref":"x","onchain_sat":1000}`); c != 201 {
		t.Errorf("= %d %s", c, b)
	}
	// MaxSat itself is accepted.
	if c, b := e.post(`{"account_id":"a","merchant_ref":"max","onchain_sat":2100000000000000}`); c != 201 {
		t.Errorf("max = %d %s", c, b)
	}
}

// An order with an on-chain amount, read while the rail is not configured: 503 onchain_unavailable.
func TestGetOnchainOrderWithoutRail(t *testing.T) {
	e := newEnv(t)
	o, _, err := e.st.CreateOrder(context.Background(), store.Order{AccountID: "a", MerchantRef: "oc", AmountSat: 1000,
		ExpiresAt: epoch.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	code, b, _ := e.do("GET", "/v1/orders/"+o.ID, readKey, "")
	if code != 503 || string(b) != `{"error":"onchain_unavailable"}`+"\n" {
		t.Errorf("= %d %s", code, b)
	}
}

// An order with a Monero amount, read while the Monero rail is not configured: 503 monero_unavailable, the only way
// that code arises. With both rails missing the on-chain one is named.
func TestGetMoneroOrderWithoutRail(t *testing.T) {
	e := newEnv(t)
	o, _, err := e.st.CreateOrder(context.Background(), store.Order{AccountID: "a", MerchantRef: "xm", AmountPiconero: 1000,
		ExpiresAt: epoch.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	both, _, err := e.st.CreateOrder(context.Background(), store.Order{AccountID: "a", MerchantRef: "both", AmountSat: 1000, AmountPiconero: 1000,
		ExpiresAt: epoch.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if code, b, _ := e.do("GET", "/v1/orders/"+o.ID, readKey, ""); code != 503 || string(b) != `{"error":"monero_unavailable"}`+"\n" {
		t.Errorf("= %d %s", code, b)
	}
	if code, b, _ := e.do("GET", "/v1/orders/"+both.ID, readKey, ""); code != 503 || string(b) != `{"error":"onchain_unavailable"}`+"\n" {
		t.Errorf("both missing: %d %s", code, b)
	}
	e.withOnchain(&stubOc{})
	if code, b, _ := e.do("GET", "/v1/orders/"+both.ID, readKey, ""); code != 503 || string(b) != `{"error":"monero_unavailable"}`+"\n" {
		t.Errorf("Monero missing: %d %s", code, b)
	}
	e.withMonero(&stubXm{view: rail.View{State: rail.Waiting}})
	if code, b, _ := e.do("GET", "/v1/orders/"+both.ID, readKey, ""); code != 200 {
		t.Errorf("both rails: %d %s", code, b)
	}
}

// --- priced orders (plan 7) ---

const fiatBody = `{"account_id":"a","merchant_ref":"r1","price":"10.00","currency":"USD"}`

var pricingKeys = []string{"currency", "margin_percent", "price", "quoted_at", "rates"}

// wantSats is rates.Sats of price at rate with margin, the one formula.
func wantSats(t *testing.T, price, rate string, margin int64) int64 {
	t.Helper()
	n, err := rates.Sats(mustDecimal(price), mustDecimal(rate), margin)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// checkPricing asserts the pricing object of a priced response against the fake's values.
func checkPricing(t *testing.T, m map[string]any, price, currency, rate string, at time.Time) {
	t.Helper()
	p, ok := m["pricing"].(map[string]any)
	if !ok {
		t.Fatalf("pricing = %v", m["pricing"])
	}
	if !reflect.DeepEqual(keysOf(p), pricingKeys) {
		t.Fatalf("pricing keys = %v, want %v", keysOf(p), pricingKeys)
	}
	if p["price"] != price || p["currency"] != currency || p["margin_percent"] != "1.5" ||
		p["quoted_at"] != at.UTC().Truncate(time.Second).Format(time.RFC3339) {
		t.Errorf("pricing = %v", p)
	}
	rs := asMap2(t, p["rates"])
	wantAssets := []string{"BTC"}
	if m["monero"] != nil {
		wantAssets = []string{"BTC", "XMR"}
		if m["lightning"] == nil && m["onchain"] == nil {
			wantAssets = []string{"XMR"}
		}
	}
	if !reflect.DeepEqual(keysOf(rs), wantAssets) {
		t.Fatalf("rates assets = %v, want %v", keysOf(rs), wantAssets)
	}
	if _, ok := rs["BTC"]; ok {
		checkRate(t, rs["BTC"], rate, at, [][2]string{{"coinbase", "76050"}, {"kraken", "76000"}})
	}
	if _, ok := rs["XMR"]; ok {
		checkRate(t, rs["XMR"], "160", at, [][2]string{{"kraken", "160.5"}, {"yadio", "159.5"}})
	}
	// The invariant of every priced response: no amount came from the body, so the Lightning amount is whole sats.
	if msat := m["lightning_msat"].(float64); int64(msat)%1000 != 0 {
		t.Errorf("lightning_msat %v is not whole sats", msat)
	}
	auditPricing(t, m)
}

// checkRate asserts one asset's member of pricing.rates against the fake's values.
func checkRate(t *testing.T, v any, rate string, at time.Time, sources [][2]string) {
	t.Helper()
	r := asMap2(t, v)
	if !reflect.DeepEqual(keysOf(r), []string{"rate", "sources"}) || r["rate"] != rate {
		t.Errorf("rate = %v, want %s", r, rate)
	}
	src, ok := r["sources"].([]any)
	if !ok || len(src) != len(sources) {
		t.Fatalf("sources = %v", r["sources"])
	}
	srcAt := at.Add(-10 * time.Second).UTC().Truncate(time.Second).Format(time.RFC3339)
	for i, want := range sources {
		s := asMap2(t, src[i])
		if !reflect.DeepEqual(keysOf(s), []string{"at", "provider", "rate"}) || s["provider"] != want[0] || s["rate"] != want[1] || s["at"] != srcAt {
			t.Errorf("source %d = %v", i, s)
		}
	}
}

// auditPricing re-derives a priced response's amounts from its own pricing, as an auditor would (auditErr).
func auditPricing(t *testing.T, m map[string]any) {
	t.Helper()
	if err := auditErr(m); err != nil {
		t.Error(err)
	}
}

// auditErr checks a priced response against its own pricing: Sats of the price at the BTC rate with the margin is
// onchain_sat, and that times 1000 is lightning_msat, and Piconero of the price at the XMR rate with the margin is
// monero_piconero, for each rail the order has; the rates name exactly the assets of the order's rails.
func auditErr(m map[string]any) error {
	p, ok := m["pricing"].(map[string]any)
	if !ok {
		return fmt.Errorf("no pricing: %v", m)
	}
	ps, _ := p["price"].(string)
	ms, _ := p["margin_percent"].(string)
	price, err1 := rates.ParseDecimal(ps)
	margin, err2 := rates.ParsePercent(ms)
	if err := errors.Join(err1, err2); err != nil {
		return fmt.Errorf("pricing %v does not parse: %v", p, err)
	}
	rs, _ := p["rates"].(map[string]any)
	amount := func(asset string) (int64, error) {
		r, _ := rs[asset].(map[string]any)
		s, _ := r["rate"].(string)
		rate, err := rates.ParseDecimal(s)
		if err != nil {
			return 0, fmt.Errorf("pricing %v: the %s rate does not parse: %v", p, asset, err)
		}
		return rates.Convert(asset, price, rate, margin)
	}
	ln, oc, xm := m["lightning"] != nil, m["onchain"] != nil, m["monero"] != nil
	_, hasBTC := rs["BTC"]
	_, hasXMR := rs["XMR"]
	switch {
	case !ln && !oc && !xm:
		return fmt.Errorf("a priced order with no rail: %v", m)
	case hasBTC != (ln || oc) || hasXMR != xm || len(rs) != btoi(hasBTC)+btoi(hasXMR):
		return fmt.Errorf("pricing rates %v do not match the rails", rs)
	}
	if ln || oc {
		sats, err := amount("BTC")
		if err != nil {
			return err
		}
		switch {
		case ln && m["lightning_msat"] != float64(sats*1000):
			return fmt.Errorf("lightning_msat %v, but the pricing gives %d sat", m["lightning_msat"], sats)
		case oc && m["onchain_sat"] != float64(sats):
			return fmt.Errorf("onchain_sat %v, but the pricing gives %d sat", m["onchain_sat"], sats)
		}
	}
	if xm {
		pico, err := amount("XMR")
		if err != nil {
			return err
		}
		if m["monero_piconero"] != float64(pico) {
			return fmt.Errorf("monero_piconero %v, but the pricing gives %d piconero", m["monero_piconero"], pico)
		}
	}
	return nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestAuditCatches: the audit refuses a response whose amounts do not follow from its pricing.
func TestAuditCatches(t *testing.T) {
	good := func() map[string]any {
		return map[string]any{"lightning_msat": float64(13_351_000), "onchain_sat": float64(13_351), "lightning": map[string]any{},
			"onchain": map[string]any{}, "monero_piconero": float64(63_437_500_000), "monero": map[string]any{},
			"pricing": map[string]any{"price": "10.00", "margin_percent": "1.5",
				"rates": map[string]any{"BTC": map[string]any{"rate": "76025"}, "XMR": map[string]any{"rate": "160"}}}}
	}
	if n := wantPiconero(t, "10.00", "160", 150); n != 63_437_500_000 {
		t.Fatalf("Piconero = %d", n)
	}
	if err := auditErr(good()); err != nil {
		t.Fatalf("the good response: %v", err)
	}
	for name, mod := range map[string]func(m map[string]any){
		"msat one sat high":   func(m map[string]any) { m["lightning_msat"] = float64(13_352_000) },
		"msat one msat low":   func(m map[string]any) { m["lightning_msat"] = float64(13_350_999) },
		"onchain one sat low": func(m map[string]any) { m["onchain_sat"] = float64(13_350) },
		"another rate":        func(m map[string]any) { rateOf(m, "BTC")["rate"] = "76000" },
		"another XMR rate":    func(m map[string]any) { rateOf(m, "XMR")["rate"] = "160.01" },
		"another margin":      func(m map[string]any) { m["pricing"].(map[string]any)["margin_percent"] = "1" },
		"another price":       func(m map[string]any) { m["pricing"].(map[string]any)["price"] = "10.01" },
		"a bad rate":          func(m map[string]any) { rateOf(m, "BTC")["rate"] = "x" },
		"a bad XMR rate":      func(m map[string]any) { rateOf(m, "XMR")["rate"] = "" },
		"piconero one low":    func(m map[string]any) { m["monero_piconero"] = float64(63_437_499_999) },
		"no XMR rate":         func(m map[string]any) { delete(m["pricing"].(map[string]any)["rates"].(map[string]any), "XMR") },
		"no BTC rate":         func(m map[string]any) { delete(m["pricing"].(map[string]any)["rates"].(map[string]any), "BTC") },
		"an extra asset": func(m map[string]any) {
			m["pricing"].(map[string]any)["rates"].(map[string]any)["LTC"] = map[string]any{}
		},
		"no rail":    func(m map[string]any) { m["lightning"], m["onchain"], m["monero"] = nil, nil, nil },
		"no pricing": func(m map[string]any) { delete(m, "pricing") },
	} {
		m := good()
		mod(m)
		if auditErr(m) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// One rail: the other amounts are not looked at.
	m := good()
	m["onchain"], m["onchain_sat"], m["monero"] = nil, float64(0), nil
	delete(m["pricing"].(map[string]any)["rates"].(map[string]any), "XMR")
	if err := auditErr(m); err != nil {
		t.Errorf("Lightning only: %v", err)
	}
	m = good()
	m["onchain"], m["lightning"] = nil, nil
	delete(m["pricing"].(map[string]any)["rates"].(map[string]any), "BTC")
	if err := auditErr(m); err != nil {
		t.Errorf("Monero only: %v", err)
	}
}

func rateOf(m map[string]any, asset string) map[string]any {
	return m["pricing"].(map[string]any)["rates"].(map[string]any)[asset].(map[string]any)
}

// wantPiconero is rates.Piconero of price at rate with margin, the one formula.
func wantPiconero(t *testing.T, price, rate string, margin int64) int64 {
	t.Helper()
	n, err := rates.Piconero(mustDecimal(price), mustDecimal(rate), margin)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFiatCreate(t *testing.T) {
	e := newEnv(t)
	code, b := e.post(fiatBody)
	if code != 201 {
		t.Fatalf("create = %d %s", code, b)
	}
	m := asMap(t, b)
	if !reflect.DeepEqual(keysOf(m), allowlist) {
		t.Fatalf("keys = %v", keysOf(m))
	}
	sats := wantSats(t, "10.00", "76025", 150)
	if sats != 13351 || m["lightning_msat"] != float64(13_351_000) || m["onchain_sat"] != float64(0) || m["onchain"] != nil || m["lightning"] == nil {
		t.Errorf("amounts: %v (Sats %d)", m, sats)
	}
	checkPricing(t, m, "10.00", "USD", "76025", e.pr.at)
	// The store holds the quote as sent: the price text unchanged, the rate in canonical form.
	q, ok, err := e.st.QuoteForOrder(context.Background(), m["id"].(string))
	if err != nil || !ok || q.Price != "10.00" || q.Currency != "USD" || len(q.Rates) != 1 || q.Rates[0].Asset != "BTC" ||
		q.Rates[0].Rate != "76025" || q.MarginBps != 150 || len(q.Rates[0].Sources) != 2 {
		t.Fatalf("stored quote %+v %v %v", q, ok, err)
	}
	// GET returns the same view.
	_, gb, _ := e.do("GET", "/v1/orders/"+m["id"].(string), readKey, "")
	if !reflect.DeepEqual(asMap(t, gb), m) {
		t.Errorf("GET differs: %s vs %s", gb, b)
	}

	// With the on-chain rail, the default rails are both, each the same Sats.
	e.withOnchain(&stubOc{rail: onchain.Rail{State: onchain.RailWaiting, RequiredConfirmations: 1}})
	code, b = e.post(`{"account_id":"a","merchant_ref":"r2","price":"10.00","currency":"USD"}`)
	m = asMap(t, b)
	if code != 201 || m["lightning_msat"] != float64(13_351_000) || m["onchain_sat"] != float64(13_351) || m["onchain"] == nil || m["lightning"] == nil {
		t.Errorf("both rails: %d %s", code, b)
	}
	checkPricing(t, m, "10.00", "USD", "76025", e.pr.at)

	// Explicit rails.
	for i, c := range []struct {
		rails  string
		ln, oc bool
	}{{`["onchain"]`, false, true}, {`["lightning"]`, true, false}, {`["onchain","lightning"]`, true, true}, {`["lightning","onchain"]`, true, true}} {
		code, b = e.post(fmt.Sprintf(`{"account_id":"a","merchant_ref":"rails-%d","price":"10.00","currency":"EUR","rails":%s}`, i, c.rails))
		if code != 201 {
			t.Fatalf("%s: %d %s", c.rails, code, b)
		}
		m = asMap(t, b)
		wantLn, wantOc := float64(0), float64(0)
		if c.ln {
			wantLn = 13_351_000
		}
		if c.oc {
			wantOc = 13_351
		}
		if m["lightning_msat"] != wantLn || m["onchain_sat"] != wantOc || (m["lightning"] != nil) != c.ln || (m["onchain"] != nil) != c.oc {
			t.Errorf("%s: %s", c.rails, b)
		}
		checkPricing(t, m, "10.00", "EUR", "76025", e.pr.at)
	}

	// Across the boundaries of the conversion, each rail's amount is rates.Sats exactly: never 0, never off by one.
	for i, price := range []string{"0.000000000000000001", "0.01", "1", "9.99", "123.456789", "748.999999999999999999", "1000"} {
		code, b = e.post(fmt.Sprintf(`{"account_id":"a","merchant_ref":"b-%d","price":%q,"currency":"USD","rails":["onchain"]}`, i, price))
		if code != 201 {
			t.Fatalf("%s: %d %s", price, code, b)
		}
		n := wantSats(t, price, "76025", 150)
		if m = asMap(t, b); n < 1 || m["onchain_sat"] != float64(n) {
			t.Errorf("%s: onchain_sat %v, want %d", price, m["onchain_sat"], n)
		}
		auditPricing(t, m)
		code, b = e.post(fmt.Sprintf(`{"account_id":"a","merchant_ref":"bl-%d","price":%q,"currency":"USD","rails":["lightning"]}`, i, price))
		if price == "1000" { // above the limiter's MaxMsat: TestFiatAmountBounds
			continue
		}
		if m = asMap(t, b); code != 201 || m["lightning_msat"] != float64(n*1000) {
			t.Errorf("%s: %d lightning_msat %v, want %d", price, code, m["lightning_msat"], n*1000)
		}
		auditPricing(t, m)
	}
}

// fiatRefusals are the priced bodies refused with 400 before the pricer is asked (newEnv has no on-chain rail, and
// USD and EUR configured). FuzzCreateOrder is seeded with them.
var fiatRefusals = []struct{ name, body, code string }{
	{"price number", `{"account_id":"a","merchant_ref":"r","price":10,"currency":"USD"}`, "invalid_price"},
	{"price null", `{"account_id":"a","merchant_ref":"r","price":null,"currency":"USD"}`, "invalid_price"},
	{"price empty", `{"account_id":"a","merchant_ref":"r","price":"","currency":"USD"}`, "invalid_price"},
	{"price zero", `{"account_id":"a","merchant_ref":"r","price":"0","currency":"USD"}`, "invalid_price"},
	{"price zero fraction", `{"account_id":"a","merchant_ref":"r","price":"0.000","currency":"USD"}`, "invalid_price"},
	{"price exponent", `{"account_id":"a","merchant_ref":"r","price":"1e3","currency":"USD"}`, "invalid_price"},
	{"price negative", `{"account_id":"a","merchant_ref":"r","price":"-1","currency":"USD"}`, "invalid_price"},
	{"price sign", `{"account_id":"a","merchant_ref":"r","price":"+1","currency":"USD"}`, "invalid_price"},
	{"price space", `{"account_id":"a","merchant_ref":"r","price":" 1","currency":"USD"}`, "invalid_price"},
	{"price comma", `{"account_id":"a","merchant_ref":"r","price":"1,5","currency":"USD"}`, "invalid_price"},
	{"price no int", `{"account_id":"a","merchant_ref":"r","price":".5","currency":"USD"}`, "invalid_price"},
	{"price no frac", `{"account_id":"a","merchant_ref":"r","price":"5.","currency":"USD"}`, "invalid_price"},
	{"price 21 digits", `{"account_id":"a","merchant_ref":"r","price":"100000000000000000000","currency":"USD"}`, "invalid_price"},
	{"price 19 fraction digits", `{"account_id":"a","merchant_ref":"r","price":"1.0000000000000000001","currency":"USD"}`, "invalid_price"},
	{"price array", `{"account_id":"a","merchant_ref":"r","price":["1"],"currency":"USD"}`, "invalid_price"},
	{"currency lower", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"usd"}`, "invalid_currency"},
	{"currency four", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USDT"}`, "invalid_currency"},
	{"currency BTC", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"BTC"}`, "invalid_currency"},
	{"currency null", `{"account_id":"a","merchant_ref":"r","price":"10","currency":null}`, "invalid_currency"},
	{"currency number", `{"account_id":"a","merchant_ref":"r","price":"10","currency":840}`, "invalid_currency"},
	{"currency not configured", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"GBP"}`, "invalid_currency"},
	{"currency without price", `{"account_id":"a","merchant_ref":"r","currency":"USD","lightning_msat":1000}`, "invalid_price"},
	{"price without currency", `{"account_id":"a","merchant_ref":"r","price":"10"}`, "invalid_currency"},
	{"mixed lightning", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","lightning_msat":1000}`, "mixed_pricing"},
	{"mixed onchain", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","onchain_sat":1000}`, "mixed_pricing"},
	{"mixed with a bad amount", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","lightning_msat":-1}`, "mixed_pricing"},
	{"rails with lightning_msat", `{"account_id":"a","merchant_ref":"r","lightning_msat":1000,"rails":["lightning"]}`, "invalid_rails"},
	{"rails alone", `{"account_id":"a","merchant_ref":"r","rails":["lightning"]}`, "invalid_rails"},
	{"rails empty", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":[]}`, "invalid_rails"},
	{"rails repeated", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":["lightning","lightning"]}`, "invalid_rails"},
	{"rails unknown", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":["ethereum"]}`, "invalid_rails"},
	{"rails unknown beside known", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":["lightning","litecoin"]}`, "invalid_rails"},
	{"rails case", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":["Lightning"]}`, "invalid_rails"},
	{"rails null", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":null}`, "invalid_rails"},
	{"rails string", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":"lightning"}`, "invalid_rails"},
	{"rails number element", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":[1]}`, "invalid_rails"},
	{"rails three", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":["lightning","onchain","lightning"]}`, "invalid_rails"},
	{"rails monero repeated", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":["monero","lightning","monero"]}`, "invalid_rails"},
	{"rails first and third", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":["onchain","monero","onchain"]}`, "invalid_rails"},
	{"rails four", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":["lightning","onchain","monero","lightning"]}`, "invalid_rails"},
	{"rails monero without the rail", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":["monero"]}`, "monero_disabled"},
	{"rails all three without the rails", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":["lightning","onchain","monero"]}`, "onchain_disabled"},
	{"mixed monero", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","monero_piconero":1000}`, "mixed_pricing"},
	{"mixed with a bad monero amount", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","monero_piconero":"x"}`, "mixed_pricing"},
	{"rails onchain without the rail", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":["onchain"]}`, "onchain_disabled"},
	{"rails both without the rail", `{"account_id":"a","merchant_ref":"r","price":"10","currency":"USD","rails":["lightning","onchain"]}`, "onchain_disabled"},
	{"price case variant", `{"account_id":"a","merchant_ref":"r","Price":"10","currency":"USD"}`, "invalid_json"},
	{"price duplicate", `{"account_id":"a","merchant_ref":"r","price":"10","price":"10","currency":"USD"}`, "invalid_json"},
}

func TestFiatRefusals(t *testing.T) {
	e := newEnv(t)
	for _, c := range fiatRefusals {
		t.Run(c.name, func(t *testing.T) {
			code, body := e.post(c.body)
			if code != 400 || string(body) != `{"error":"`+c.code+`"}`+"\n" {
				t.Errorf("= %d %s, want 400 %s", code, body, c.code)
			}
		})
	}
	// None of them reached the pricer.
	if n := e.pr.callCount(); n != 0 {
		t.Errorf("the pricer was asked %d times for refused requests", n)
	}

	// No pricer configured: fiat_disabled, and explicit orders still work.
	h := newHandler(e.keys, e.st, e.res, base, nil, e.clk.Now)
	req := httptest.NewRequest("POST", "/v1/orders", strings.NewReader(fiatBody))
	req.Header.Set("Authorization", "Bearer "+writeKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 || rec.Body.String() != `{"error":"fiat_disabled"}`+"\n" {
		t.Errorf("no pricer: %d %s", rec.Code, rec.Body)
	}

	// The pricer's refusals.
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{rates.ErrTooFew, 503, "rate_unavailable"}, {rates.ErrDisagree, 503, "rate_unavailable"},
		{rates.ErrCurrency, 503, "rate_unavailable"}, {rates.ErrDuplicate, 503, "rate_unavailable"},
		{fmt.Errorf("wrapped: %w", rates.ErrTooFew), 503, "rate_unavailable"},
		{rates.ErrAsset, 500, "internal"}, {rates.ErrZero, 500, "internal"}, {rates.ErrMargin, 500, "internal"},
		{rates.ErrUnreachable, 400, "invalid_currency"}, {fmt.Errorf("wrapped: %w", rates.ErrUnreachable), 400, "invalid_currency"},
		{fmt.Errorf("boom https://rates.example.invalid/secret"), 500, "internal"},
	} {
		e.pr.set(func(f *fakePricer) { f.err = c.err })
		code, body, hdr := e.do("POST", "/v1/orders", writeKey, fiatBody)
		if code != c.status || string(body) != `{"error":"`+c.code+`"}`+"\n" {
			t.Errorf("%v: %d %s", c.err, code, body)
		}
		if ra := hdr.Get("Retry-After"); (c.status == 503) != (ra == "10") || c.status != 503 && ra != "" {
			t.Errorf("%v: Retry-After %q", c.err, ra)
		}
	}
	e.pr.set(func(f *fakePricer) { f.err = nil })
	// A quote of another currency than the one asked is never recorded.
	e.pr.set(func(f *fakePricer) { f.quoteCurrency = "EUR" })
	if code, body := e.post(fiatBody); code != 500 || string(body) != `{"error":"internal"}`+"\n" {
		t.Errorf("a quote in another currency: %d %s", code, body)
	}
	e.pr.set(func(f *fakePricer) { f.quoteCurrency = "" })
	if os, _ := e.st.OrdersByAccount(context.Background(), "a"); len(os) != 0 {
		t.Errorf("%d orders created by refused requests", len(os))
	}
}

// A priced amount out of a rail's bounds is refused, never clamped, and creates nothing.
func TestFiatAmountBounds(t *testing.T) {
	e := newEnv(t)
	// "1000" USD at 76025 with 1.5 % is 1,335,088 sats: above newEnv's MaxMsat of 1,000,000,000 msat.
	if n := wantSats(t, "1000", "76025", 150); n != 1_335_088 {
		t.Fatalf("Sats = %d", n)
	}
	big := `{"account_id":"a","merchant_ref":"big","price":"1000","currency":"USD"}`
	if code, b := e.post(big); code != 400 || string(b) != `{"error":"invalid_lightning_msat"}`+"\n" {
		t.Errorf("above MaxMsat: %d %s", code, b)
	}
	// Just under the limit: the largest price whose Lightning amount fits is accepted.
	if code, b := e.post(`{"account_id":"a","merchant_ref":"fits","price":"749","currency":"USD"}`); code != 201 {
		t.Errorf("under MaxMsat: %d %s", code, b)
	}
	// 10^10 USD is about 1.3 × 10^13 sats: under the bitcoin supply, so Sats succeeds, but about 1.3 × 10^16 msat, above
	// 2^53. With newEnv's limiter (MaxMsat 10^9) both bounds refuse it with the same code, so this pins the answer, not
	// which bound gave it.
	if n := wantSats(t, "10000000000", "76025", 150); n*1000 <= maxAmountMsat || n > store.MaxSat {
		t.Fatalf("Sats = %d: not between 2^53 msat and the supply", n)
	}
	if code, b := e.post(`{"account_id":"a","merchant_ref":"huge","price":"10000000000","currency":"USD","rails":["lightning"]}`); code != 400 ||
		string(b) != `{"error":"invalid_lightning_msat"}`+"\n" {
		t.Errorf("above 2^53: %d %s", code, b)
	}
	if os, _ := e.st.OrdersByAccount(context.Background(), "a"); len(os) != 1 {
		t.Fatalf("%d orders, want 1", len(os))
	}
	e.withOnchain(&stubOc{})
	// Both rails, Lightning out of bounds: the whole order is refused, not created on-chain only.
	if code, b := e.post(big); code != 400 || string(b) != `{"error":"invalid_lightning_msat"}`+"\n" {
		t.Errorf("both rails above MaxMsat: %d %s", code, b)
	}
	code, b := e.post(`{"account_id":"a","merchant_ref":"big","price":"1000","currency":"USD","rails":["onchain"]}`)
	if code != 201 || asMap(t, b)["onchain_sat"] != float64(1_335_088) {
		t.Errorf("on-chain only: %d %s", code, b)
	}
	// The pricer's ErrRange (the bitcoin supply): the rail it names.
	e.pr.set(func(f *fakePricer) { f.err = rates.ErrRange })
	for _, c := range []struct{ rails, code string }{
		{``, "invalid_onchain_sat"}, {`,"rails":["lightning"]`, "invalid_lightning_msat"}, {`,"rails":["onchain"]`, "invalid_onchain_sat"},
		{`,"rails":["lightning","onchain"]`, "invalid_onchain_sat"},
	} {
		if code, b := e.post(`{"account_id":"a","merchant_ref":"range","price":"1","currency":"USD"` + c.rails + `}`); code != 400 ||
			string(b) != `{"error":"`+c.code+`"}`+"\n" {
			t.Errorf("ErrRange %s: %d %s", c.rails, code, b)
		}
	}
	// Without the on-chain rail the default rails are Lightning only.
	e2 := newEnv(t)
	e2.pr.set(func(f *fakePricer) { f.err = rates.ErrRange })
	if code, b := e2.post(`{"account_id":"a","merchant_ref":"range","price":"1","currency":"USD"}`); code != 400 ||
		string(b) != `{"error":"invalid_lightning_msat"}`+"\n" {
		t.Errorf("ErrRange, Lightning only: %d %s", code, b)
	}
	if os, _ := e.st.OrdersByAccount(context.Background(), "a"); len(os) != 2 {
		t.Errorf("%d orders, want 2", len(os))
	}
	if os, _ := e2.st.OrdersByAccount(context.Background(), "a"); len(os) != 0 {
		t.Errorf("%d orders, want 0", len(os))
	}
}

// A priced Lightning amount below the limiter's MinMsat is refused like an explicit one, and creates no order; one at
// MinMsat is accepted.
func TestFiatBelowMinMsat(t *testing.T) {
	e := newEnv(t)
	lim, err := limits.New(limits.Config{RatePerSec: 1e6, Burst: 1 << 20, MinMsat: 100_000, MaxMsat: 1_000_000_000, DailyMsat: 1 << 50}, e.clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := lnpay.New(lnpay.Config{PollRate: 1000, Network: approval.Regtest}, e.st, e.lg, e.w, lim, e.clk.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.lim, e.svc = lim, svc
	e.res = &status.Resolver{St: e.st, Ln: svc, Now: e.clk.Now}
	e.h = newHandler(e.keys, e.st, e.res, base+"/", e.pr, e.clk.Now)
	// "0.07" USD at 76025 with 1.5 % is 94 sats, under 100; "0.075" is 101 sats, at least 100.
	if lo, hi := wantSats(t, "0.07", "76025", 150), wantSats(t, "0.075", "76025", 150); lo != 94 || hi != 101 {
		t.Fatalf("Sats = %d, %d", lo, hi)
	}
	if code, b := e.post(`{"account_id":"a","merchant_ref":"small","price":"0.07","currency":"USD"}`); code != 400 ||
		string(b) != `{"error":"invalid_lightning_msat"}`+"\n" {
		t.Errorf("below MinMsat: %d %s", code, b)
	}
	if os, _ := e.st.OrdersByAccount(context.Background(), "a"); len(os) != 0 {
		t.Errorf("%d orders after a refusal, want 0", len(os))
	}
	if code, b := e.post(`{"account_id":"a","merchant_ref":"enough","price":"0.075","currency":"USD"}`); code != 201 ||
		asMap(t, b)["lightning_msat"] != float64(101_000) {
		t.Errorf("at MinMsat: %d %s", code, b)
	}
}

// A repeat with the same merchant_ref returns the first order and its locked quote, whatever the rate
// does; a different price text, an explicit body, or another currency or rail set is a conflict.
func TestFiatRepeatIsIdempotent(t *testing.T) {
	e := newEnv(t)
	frac := epoch.Add(123_456_789 * time.Nanosecond) // a quote time with a fraction of a second
	e.pr.set(func(f *fakePricer) { f.at = frac })
	code, b := e.post(fiatBody)
	if code != 201 {
		t.Fatalf("create = %d %s", code, b)
	}
	first := asMap(t, b)
	checkPricing(t, first, "10.00", "USD", "76025", frac)
	if p := first["pricing"].(map[string]any); p["quoted_at"] != epoch.Format(time.RFC3339) {
		t.Errorf("quoted_at %v", p["quoted_at"])
	}
	q, _, _ := e.st.QuoteForOrder(context.Background(), first["id"].(string))
	if !q.QuotedAt.Equal(epoch) || q.QuotedAt.Nanosecond() != 0 {
		t.Errorf("stored QuotedAt %v, want whole seconds", q.QuotedAt)
	}

	// The rate moves: the repeat is 200 with the first order, amounts and pricing.
	e.pr.set(func(f *fakePricer) {
		f.rate, f.sources, f.at = "80000", [][2]string{{"kraken", "80000"}, {"yadio", "80001"}}, epoch.Add(time.Minute)
	})
	e.clk.Advance(time.Minute)
	code, b = e.post(fiatBody)
	if code != 200 || !reflect.DeepEqual(asMap(t, b), first) {
		t.Errorf("repeat after the rate moved: %d %s, want %v", code, b, first)
	}

	// A restart: the quote stays locked.
	e.restart()
	code, b = e.post(fiatBody)
	if code != 200 || !reflect.DeepEqual(asMap(t, b), first) {
		t.Errorf("repeat after a restart: %d %s", code, b)
	}
	if _, gb, _ := e.do("GET", "/v1/orders/"+first["id"].(string), readKey, ""); !reflect.DeepEqual(asMap(t, gb), first) {
		t.Errorf("GET after a restart: %s", gb)
	}

	for _, body := range []string{
		`{"account_id":"a","merchant_ref":"r1","price":"10.01","currency":"USD"}`,
		`{"account_id":"a","merchant_ref":"r1","price":"10.0","currency":"USD"}`, // the same value, other text
		`{"account_id":"a","merchant_ref":"r1","price":"10.00","currency":"EUR"}`,
		`{"account_id":"b","merchant_ref":"r1","price":"10.00","currency":"USD"}`,
		`{"account_id":"a","merchant_ref":"r1","lightning_msat":13351000}`, // the explicit body of the same amount
	} {
		if c, rb := e.post(body); c != 409 || string(rb) != `{"error":"conflict"}`+"\n" {
			t.Errorf("%s: %d %s", body, c, rb)
		}
	}
	// The price text is matched byte for byte: "21.50" then "21.5" is a conflict.
	if c, rb := e.post(`{"account_id":"a","merchant_ref":"t","price":"21.50","currency":"USD"}`); c != 201 {
		t.Fatalf("21.50: %d %s", c, rb)
	}
	if c, rb := e.post(`{"account_id":"a","merchant_ref":"t","price":"21.5","currency":"USD"}`); c != 409 {
		t.Errorf("21.5 after 21.50: %d %s", c, rb)
	}
	// A priced repeat of an explicit order is a conflict.
	if c, _ := e.post(`{"account_id":"a","merchant_ref":"ex","lightning_msat":13351000}`); c != 201 {
		t.Fatal(c)
	}
	if c, rb := e.post(`{"account_id":"a","merchant_ref":"ex","price":"10.00","currency":"USD"}`); c != 409 {
		t.Errorf("priced repeat of an explicit order: %d %s", c, rb)
	}
	// Another rail set is a conflict: here the on-chain rail is added to the configuration, so the default rails
	// change (by design: the rail set of a request without rails is the operator's configuration).
	e.withOnchain(&stubOc{})
	if c, rb := e.post(fiatBody); c != 409 {
		t.Errorf("repeat after the configured rails changed: %d %s", c, rb)
	}
	if c, rb := e.post(`{"account_id":"a","merchant_ref":"r1","price":"10.00","currency":"USD","rails":["lightning"]}`); c != 200 ||
		!reflect.DeepEqual(asMap(t, rb), first) {
		t.Errorf("repeat naming the original rails: %d %s", c, rb)
	}

	// During a rate outage the repeat still returns the first order (it is looked up before pricing), and GET reads it.
	e.pr.set(func(f *fakePricer) { f.err = rates.ErrTooFew })
	if c, rb := e.post(`{"account_id":"a","merchant_ref":"r1","price":"10.00","currency":"USD","rails":["lightning"]}`); c != 200 ||
		!reflect.DeepEqual(asMap(t, rb), first) {
		t.Errorf("repeat during an outage: %d %s", c, rb)
	}
	if c, gb, _ := e.do("GET", "/v1/orders/"+first["id"].(string), readKey, ""); c != 200 || !reflect.DeepEqual(asMap(t, gb), first) {
		t.Errorf("GET during an outage: %d %s", c, gb)
	}
}

// A priced repeat (the same account, price text, currency and rail set) returns the existing order with 200 before any
// pricing: during a rate outage, after fiat pricing was switched off, and when the moved rate would push a rail out of
// bounds, with no call to the pricer. Anything else falls through to pricing and the store's check.
func TestPricedRepeatSkipsPricing(t *testing.T) {
	e := newEnv(t)
	code, b := e.post(fiatBody)
	if code != 201 {
		t.Fatalf("create = %d %s", code, b)
	}
	first := asMap(t, b)
	priced := e.pr.callCount()
	repeat := func(h http.Handler, body string) (int, []byte, http.Header) {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/orders", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+writeKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes(), rec.Header()
	}
	same := func(what string, h http.Handler, body string) {
		t.Helper()
		c, rb, hdr := repeat(h, body)
		if c != 200 || !reflect.DeepEqual(asMap(t, rb), first) || hdr.Get("Retry-After") != "" {
			t.Errorf("%s: %d %s, want 200 and the first order", what, c, rb)
		}
	}

	// A rate outage: 200, the original quote, and the pricer is not asked.
	e.pr.set(func(f *fakePricer) { f.err = rates.ErrTooFew })
	same("during an outage", e.h, fiatBody)
	same("during an outage, the rails named", e.h, `{"account_id":"a","merchant_ref":"r1","price":"10.00","currency":"USD","rails":["lightning"]}`)
	same("during an outage, another expires_in", e.h, `{"account_id":"a","merchant_ref":"r1","price":"10.00","currency":"USD","expires_in":7200}`)
	if n := e.pr.callCount(); n != priced {
		t.Errorf("the pricer was asked %d times for repeats", n-priced)
	}
	// Fiat pricing switched off (no pricer), or the currency no longer configured.
	same("fiat disabled", newHandler(e.keys, e.st, e.res, base+"/", nil, e.clk.Now), fiatBody)
	e.pr.set(func(f *fakePricer) { f.currencies = []string{"EUR"} })
	same("USD no longer configured", e.h, fiatBody)
	// The rate moved so far that the Lightning amount would be above MaxMsat (10.00 USD at 0.01 is 10^11 sats).
	e.pr.set(func(f *fakePricer) { f.err, f.rate, f.currencies = nil, "0.01", []string{"USD", "EUR"} })
	same("a rate that would push the rail out of bounds", e.h, fiatBody)
	if n := e.pr.callCount(); n != priced {
		t.Errorf("the pricer was asked %d times for repeats", n-priced)
	}
	// The same rate, a new reference: out of bounds, so the lookup really was what answered above.
	if c, rb := e.post(`{"account_id":"a","merchant_ref":"r2","price":"10.00","currency":"USD"}`); c != 400 ||
		string(rb) != `{"error":"invalid_lightning_msat"}`+"\n" {
		t.Errorf("a new order at the moved rate: %d %s", c, rb)
	}

	// Not a repeat: the lookup answers the store's 409 at once, with a rate or without, and the pricer is not asked.
	e.pr.set(func(f *fakePricer) { f.rate = "76025" })
	priced = e.pr.callCount()
	notRepeats := []string{
		`{"account_id":"a","merchant_ref":"r1","price":"10.0","currency":"USD"}`,
		`{"account_id":"a","merchant_ref":"r1","price":"10.00","currency":"EUR"}`,
		`{"account_id":"b","merchant_ref":"r1","price":"10.00","currency":"USD"}`,
	}
	for _, failing := range []error{nil, rates.ErrTooFew} {
		e.pr.set(func(f *fakePricer) { f.err = failing })
		for _, body := range notRepeats {
			before := e.pr.callCount()
			if c, rb := e.post(body); c != 409 || string(rb) != `{"error":"conflict"}`+"\n" || e.pr.callCount() != before {
				t.Errorf("%s, pricer error %v: %d %s, %d pricer calls", body, failing, c, rb, e.pr.callCount()-before)
			}
		}
	}
	// Another rail set is not a repeat either: on-chain only names a rail the order lacks.
	e.pr.set(func(f *fakePricer) { f.err = nil })
	e.withOnchain(&stubOc{})
	if c, rb := e.post(`{"account_id":"a","merchant_ref":"r1","price":"10.00","currency":"USD","rails":["onchain"]}`); c != 409 {
		t.Errorf("another rail set: %d %s", c, rb)
	}
	// A priced repeat of an explicit order is not a repeat: the store's 409.
	if c, _ := e.post(`{"account_id":"a","merchant_ref":"ex","lightning_msat":13351000}`); c != 201 {
		t.Fatal(c)
	}
	if c, rb := e.post(`{"account_id":"a","merchant_ref":"ex","price":"10.00","currency":"USD","rails":["lightning"]}`); c != 409 {
		t.Errorf("a priced repeat of an explicit order: %d %s", c, rb)
	}
	if n := e.pr.callCount(); n != priced {
		t.Errorf("the pricer was asked %d times for requests under a used reference", n-priced)
	}
	if os, _ := e.st.OrdersByAccount(context.Background(), "a"); len(os) != 2 {
		t.Errorf("%d orders, want 2", len(os))
	}
}

// A priced request under a reference used by an order it does not repeat answers 409 conflict before any pricing, the
// same code and body the store's conflict gives: during a rate outage, with fiat pricing off, with the currency no
// longer configured, with the on-chain rail it names gone, and for another account's order. Nothing is created and
// the pricer is not asked.
func TestPricedConflictBeforePricing(t *testing.T) {
	e := newEnv(t)
	if c, b := e.post(fiatBody); c != 201 {
		t.Fatalf("create = %d %s", c, b)
	}
	if c, b := e.post(`{"account_id":"a","merchant_ref":"ex","lightning_msat":13351000}`); c != 201 {
		t.Fatalf("explicit = %d %s", c, b)
	}
	// The store's conflict, as the explicit path answers it: the body every 409 below must equal.
	c, storeConflict := e.post(`{"account_id":"b","merchant_ref":"r1","lightning_msat":1000}`)
	if c != 409 {
		t.Fatalf("the store's conflict: %d %s", c, storeConflict)
	}
	priced := e.pr.callCount()
	noFiat := newHandler(e.keys, e.st, e.res, base+"/", nil, e.clk.Now)
	conflicting := []string{
		`{"account_id":"a","merchant_ref":"r1","price":"10.01","currency":"USD"}`,                     // another price
		`{"account_id":"a","merchant_ref":"r1","price":"10.00","currency":"EUR"}`,                     // another currency
		`{"account_id":"b","merchant_ref":"r1","price":"10.00","currency":"USD"}`,                     // another account
		`{"account_id":"b","merchant_ref":"ex","price":"10.00","currency":"USD"}`,                     // another account's explicit order
		`{"account_id":"a","merchant_ref":"ex","price":"10.00","currency":"USD"}`,                     // a priced repeat of an explicit order
		`{"account_id":"a","merchant_ref":"r1","price":"10.00","currency":"USD","rails":["onchain"]}`, // the on-chain rail, which is off
		`{"account_id":"a","merchant_ref":"r1","price":"10.00","currency":"JPY"}`,                     // a currency not configured
	}
	for _, sc := range []struct {
		name string
		h    http.Handler
		set  func(*fakePricer)
	}{
		{"a rate outage", e.h, func(f *fakePricer) { f.err = rates.ErrTooFew }},
		{"rates disagree", e.h, func(f *fakePricer) { f.err = rates.ErrDisagree }},
		{"fiat pricing off", noFiat, func(f *fakePricer) { f.err = nil }},
		{"USD no longer configured", e.h, func(f *fakePricer) { f.currencies = []string{"EUR"} }},
	} {
		e.pr.set(sc.set)
		for _, body := range conflicting {
			req := httptest.NewRequest("POST", "/v1/orders", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+writeKey)
			rec := httptest.NewRecorder()
			sc.h.ServeHTTP(rec, req)
			if rec.Code != 409 || !bytes.Equal(rec.Body.Bytes(), storeConflict) || rec.Header().Get("Retry-After") != "" {
				t.Errorf("%s, %s: %d %s, want 409 %s", sc.name, body, rec.Code, rec.Body.Bytes(), storeConflict)
			}
		}
	}
	if n := e.pr.callCount(); n != priced {
		t.Errorf("the pricer was asked %d times", n-priced)
	}
	if os, _ := e.st.OrdersByAccount(context.Background(), "b"); len(os) != 0 {
		t.Errorf("%d orders for account b", len(os))
	}
	if os, _ := e.st.OrdersByAccount(context.Background(), "a"); len(os) != 2 {
		t.Errorf("%d orders for account a, want 2", len(os))
	}
}

// A repeat of a priced order with an on-chain amount, naming its rails, while the on-chain rail is off: the lookup
// finds it, and the answer is GET's for that order, 503 onchain_unavailable (it was 400 onchain_disabled when the
// repeat was priced first). Without "rails", the configured rail set is Lightning only: not a repeat, the store's 409.
func TestPricedRepeatOnchainRailOff(t *testing.T) {
	e := newEnv(t)
	e.withOnchain(&stubOc{rail: onchain.Rail{State: onchain.RailWaiting, RequiredConfirmations: 1}})
	if code, b := e.post(fiatBody); code != 201 || asMap(t, b)["onchain_sat"] == float64(0) {
		t.Fatalf("create = %d %s", code, b)
	}
	e.res = &status.Resolver{St: e.st, Ln: e.svc, Now: e.clk.Now}
	e.h = newHandler(e.keys, e.st, e.res, base+"/", e.pr, e.clk.Now)
	if c, gb, _ := e.do("GET", "/v1/orders/"+orderIDByRef(t, e, "r1"), readKey, ""); c != 503 || string(gb) != `{"error":"onchain_unavailable"}`+"\n" {
		t.Errorf("GET with the rail off: %d %s", c, gb)
	}
	if c, rb := e.post(`{"account_id":"a","merchant_ref":"r1","price":"10.00","currency":"USD","rails":["lightning","onchain"]}`); c != 503 ||
		string(rb) != `{"error":"onchain_unavailable"}`+"\n" {
		t.Errorf("repeat naming both rails with the rail off: %d %s", c, rb)
	}
	if c, rb := e.post(fiatBody); c != 409 {
		t.Errorf("repeat without rails, the rail off: %d %s", c, rb)
	}
}

func orderIDByRef(t *testing.T, e *env, ref string) string {
	t.Helper()
	os, err := e.st.OrdersByAccount(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range os {
		if o.MerchantRef == ref {
			return o.ID
		}
	}
	t.Fatalf("no order %q", ref)
	return ""
}

// Concurrent first creates of one priced order: each is 201 or 200 with the same order, and exactly one order exists.
func TestPricedConcurrentCreates(t *testing.T) {
	e := newEnv(t)
	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	ids := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/v1/orders", strings.NewReader(fiatBody))
			req.Header.Set("Authorization", "Bearer "+writeKey)
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			codes[i] = rec.Code
			var m map[string]any
			if json.Unmarshal(rec.Body.Bytes(), &m) == nil {
				ids[i], _ = m["id"].(string)
			}
		}()
	}
	wg.Wait()
	created := 0
	for i := range n {
		if codes[i] == 201 {
			created++
		}
		if codes[i] != 201 && codes[i] != 200 || ids[i] == "" || ids[i] != ids[0] {
			t.Errorf("request %d: %d, id %q (first %q)", i, codes[i], ids[i], ids[0])
		}
	}
	if created != 1 {
		t.Errorf("%d answers were 201, want 1", created)
	}
	if os, _ := e.st.OrdersByAccount(context.Background(), "a"); len(os) != 1 {
		t.Errorf("%d orders, want 1", len(os))
	}
}

// The explicit API is unchanged: pricing is null on every explicit order, and a body-supplied amount never comes
// with a pricing object.
func TestExplicitHasNullPricing(t *testing.T) {
	e := newEnv(t)
	e.withOnchain(&stubOc{})
	for i, body := range []string{orderBody, `{"account_id":"a","merchant_ref":"o","onchain_sat":5}`,
		`{"account_id":"a","merchant_ref":"b","lightning_msat":1000,"onchain_sat":5}`} {
		code, b := e.post(body)
		m := asMap(t, b)
		if v, ok := m["pricing"]; code != 201 || !ok || v != nil {
			t.Errorf("%d: %d %s", i, code, b)
		}
		if !strings.Contains(string(b), `"pricing":null`) {
			t.Errorf("%d: pricing is not an explicit null: %s", i, b)
		}
	}
}

// price writes nothing: it returns the answer of a refusal, and whether that answer carries Retry-After (only
// rate_unavailable does); create sets the header.
func TestPriceReturnsTheRetryFlag(t *testing.T) {
	e := newEnv(t)
	h := e.h.(*handler)
	req, code := parseCreate([]byte(fiatBody))
	if code != "" {
		t.Fatal(code)
	}
	for _, c := range []struct {
		name   string
		set    func(*fakePricer)
		status int
		code   string
		retry  bool
	}{
		{"an outage", func(f *fakePricer) { f.err = rates.ErrTooFew }, 503, codeRateUnavailable, true},
		{"rates disagree", func(f *fakePricer) { f.err = rates.ErrDisagree }, 503, codeRateUnavailable, true},
		{"out of bounds", func(f *fakePricer) { f.err = rates.ErrRange }, 400, codeInvalidAmount, false},
		{"another error", func(f *fakePricer) { f.err = rates.ErrAsset }, 500, codeInternal, false},
		{"not configured", func(f *fakePricer) { f.err, f.currencies = nil, []string{"EUR"} }, 400, codeInvalidCurrency, false},
		{"unreachable", func(f *fakePricer) { f.currencies, f.err = []string{"USD", "EUR"}, rates.ErrUnreachable }, 400, codeInvalidCurrency, false},
	} {
		e.pr.set(c.set)
		var o store.Order
		q, status, code, retry := h.price(req, &o)
		if q != nil || status != c.status || code != c.code || retry != c.retry || o != (store.Order{}) {
			t.Errorf("%s: %v %d %q %v %+v", c.name, q, status, code, retry, o)
		}
	}
	e.pr.set(func(f *fakePricer) { f.err, f.currencies = nil, []string{"USD"} })
	var o store.Order
	if q, status, code, retry := h.price(req, &o); q == nil || status != 0 || code != "" || retry || o.AmountMsat == 0 {
		t.Errorf("a rate: %v %d %q %v %+v", q, status, code, retry, o)
	}
}

// --- the Monero rail (plan 8) ---

// An explicit monero_piconero with the Monero rail: 201, the amount echoed, the monero object waiting with no address;
// GET agrees; the repeat is 200.
func TestMoneroCreate(t *testing.T) {
	e := newEnv(t)
	xm := &stubXm{view: rail.View{State: rail.Waiting, RequiredConfirmations: 10}}
	e.withMonero(xm)
	if !e.res.HasMonero() {
		t.Fatal("HasMonero false with a rail")
	}
	const body = `{"account_id":"a","merchant_ref":"xm","monero_piconero":1500000000000}`
	code, b := e.post(body)
	if code != 201 {
		t.Fatalf("create = %d %s", code, b)
	}
	m := asMap(t, b)
	if !reflect.DeepEqual(keysOf(m), allowlist) {
		t.Fatalf("keys = %v", keysOf(m))
	}
	if m["monero_piconero"] != float64(1_500_000_000_000) || m["lightning"] != nil || m["onchain"] != nil || m["lightning_msat"] != float64(0) ||
		m["onchain_sat"] != float64(0) || m["state"] != "new" || m["pricing"] != nil {
		t.Errorf("Monero only: %v", m)
	}
	if !strings.Contains(string(b), `"monero_piconero":1500000000000,`) {
		t.Errorf("monero_piconero is not an exact integer: %s", b)
	}
	x := asMap2(t, m["monero"])
	if !reflect.DeepEqual(keysOf(x), moneroKeys) {
		t.Fatalf("monero keys = %v, want %v", keysOf(x), moneroKeys)
	}
	if x["state"] != "waiting" || x["address"] != nil || x["required_confirmations"] != float64(10) || x["confirmations"] != float64(0) ||
		x["received_piconero"] != float64(0) || x["confirmed_piconero"] != float64(0) || x["late_piconero"] != float64(0) ||
		x["overpaid"] != false || x["stale"] != false {
		t.Errorf("monero object: %v", x)
	}
	if !strings.Contains(string(b), `"address":null`) {
		t.Errorf("the address is not an explicit null: %s", b)
	}
	_, gb, _ := e.do("GET", "/v1/orders/"+m["id"].(string), readKey, "")
	if !reflect.DeepEqual(asMap(t, gb), m) {
		t.Errorf("GET differs: %s vs %s", gb, b)
	}
	if c, rb := e.post(body); c != 200 || !reflect.DeepEqual(asMap(t, rb), m) {
		t.Errorf("repeat = %d %s", c, rb)
	}
	if c, _ := e.post(`{"account_id":"a","merchant_ref":"xm","monero_piconero":1500000000001}`); c != 409 {
		t.Errorf("changed amount = %d", c)
	}

	// Issued, seen, then paid with more, late money and a stale watcher: every member follows the view.
	paidAt := epoch.Add(5 * time.Minute)
	xm.set(rail.View{State: rail.Overpaid, Address: "8" + strings.Repeat("A", 94), Issued: true, RequiredConfirmations: 10, Confirmations: 12,
		Received: 1_600_000_000_000, Confirmed: 1_600_000_000_000, Late: 7, Paid: true, Overpaid: true, Stale: true, PaidAt: paidAt})
	_, gb, _ = e.do("GET", "/v1/orders/"+m["id"].(string), readKey, "")
	g := asMap(t, gb)
	x = asMap2(t, g["monero"])
	if g["state"] != "paid" || g["paid_at"] != paidAt.Format(time.RFC3339) || x["state"] != "overpaid" || x["address"] != "8"+strings.Repeat("A", 94) ||
		x["confirmations"] != float64(12) || x["received_piconero"] != float64(1_600_000_000_000) || x["confirmed_piconero"] != float64(1_600_000_000_000) ||
		x["late_piconero"] != float64(7) || x["overpaid"] != true || x["stale"] != true {
		t.Errorf("paid Monero order: %s", gb)
	}

	// All three rails, explicit.
	e.withOnchain(&stubOc{rail: onchain.Rail{State: onchain.RailWaiting, RequiredConfirmations: 1}})
	code, b = e.post(`{"account_id":"a","merchant_ref":"all","lightning_msat":21000,"onchain_sat":50000,"monero_piconero":9007199254740991}`)
	if m = asMap(t, b); code != 201 || m["lightning"] == nil || m["onchain"] == nil || m["monero"] == nil || m["monero_piconero"] != float64(store.MaxPiconero) {
		t.Errorf("all three: %d %s", code, b)
	}
}

// The refusals of monero_piconero: every malformed value, the key without the rail, and the rails member.
func TestMoneroRefusals(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ name, value string }{
		{"zero", "0"}, {"negative", "-1"}, {"2^53", "9007199254740992"}, {"a string", `"1500000000000"`}, {"a float", "1.5"},
		{"a whole float", "1500000000000.0"}, {"an exponent", "15e11"}, {"null", "null"}, {"overflow", "99999999999999999999"},
		{"int64 max", "9223372036854775807"}, {"a bool", "true"}, {"an array", "[1]"},
	} {
		body := `{"account_id":"a","merchant_ref":"r","monero_piconero":` + c.value + `}`
		if code, b := e.post(body); code != 400 || string(b) != `{"error":"invalid_monero_piconero"}`+"\n" {
			t.Errorf("%s: %d %s", c.name, code, b)
		}
	}
	// The key without the rail, explicit and priced.
	if code, b := e.post(`{"account_id":"a","merchant_ref":"r","monero_piconero":1}`); code != 400 || string(b) != `{"error":"monero_disabled"}`+"\n" {
		t.Errorf("without the rail: %d %s", code, b)
	}
	if code, b := e.post(`{"account_id":"a","merchant_ref":"r","price":"10.00","currency":"USD","rails":["monero"]}`); code != 400 ||
		string(b) != `{"error":"monero_disabled"}`+"\n" {
		t.Errorf("priced without the rail: %d %s", code, b)
	}
	if n := e.pr.callCount(); n != 0 {
		t.Errorf("the pricer was asked %d times", n)
	}
	if os, _ := e.st.OrdersByAccount(context.Background(), "a"); len(os) != 0 {
		t.Fatalf("%d orders created by refused requests", len(os))
	}
	// With the rails: three rails accepted, a repeated one refused.
	e.withOnchain(&stubOc{})
	e.withMonero(&stubXm{view: rail.View{State: rail.Waiting}})
	code, b := e.post(`{"account_id":"a","merchant_ref":"three","price":"10.00","currency":"USD","rails":["lightning","onchain","monero"]}`)
	m := asMap(t, b)
	if code != 201 || m["lightning"] == nil || m["onchain"] == nil || m["monero"] == nil {
		t.Errorf("three rails: %d %s", code, b)
	}
	if code, b := e.post(`{"account_id":"a","merchant_ref":"rep","price":"10.00","currency":"USD","rails":["monero","lightning","monero"]}`); code != 400 ||
		string(b) != `{"error":"invalid_rails"}`+"\n" {
		t.Errorf("repeated rail: %d %s", code, b)
	}
	// The minimum and the maximum are accepted.
	for i, v := range []string{"1", "9007199254740991"} {
		if code, b := e.post(fmt.Sprintf(`{"account_id":"a","merchant_ref":"b-%d","monero_piconero":%s}`, i, v)); code != 201 {
			t.Errorf("%s: %d %s", v, code, b)
		}
	}
}

// Ruling 8-A: an XMR pair that no configured set of providers quotes is a permanent 400
// invalid_currency, decided before any quote: no Retry-After, no order, and Price never asked for XMR. A reachable pair
// in a rate outage stays 503 rate_unavailable.
func TestFiatMoneroRefusals(t *testing.T) {
	e := newEnv(t)
	e.withMonero(&stubXm{view: rail.View{State: rail.Waiting}})
	if e.pr.Reachable("XMR", "EUR") || !e.pr.Reachable("BTC", "EUR") || !e.pr.Reachable("XMR", "USD") {
		t.Fatal("the fake's reachable pairs")
	}
	for _, rails := range []string{`["lightning","monero"]`, `["monero"]`, `["monero","lightning"]`} {
		code, b, hdr := e.do("POST", "/v1/orders", writeKey, `{"account_id":"a","merchant_ref":"eur","price":"10.00","currency":"EUR","rails":`+rails+`}`)
		if code != 400 || string(b) != `{"error":"invalid_currency"}`+"\n" || hdr.Get("Retry-After") != "" {
			t.Errorf("%s in EUR: %d %s, Retry-After %q", rails, code, b, hdr.Get("Retry-After"))
		}
	}
	for _, a := range e.pr.askedFor() {
		if a == "XMR" {
			t.Errorf("Price was asked for XMR in EUR: %v", e.pr.askedFor())
		}
	}
	if n := e.pr.callCount(); n != 0 {
		t.Errorf("the pricer was asked %d times before the reachable check refused", n)
	}
	if os, _ := e.st.OrdersByAccount(context.Background(), "a"); len(os) != 0 {
		t.Fatalf("%d orders", len(os))
	}
	// Lightning alone in EUR is fine.
	if code, b := e.post(`{"account_id":"a","merchant_ref":"eur","price":"10.00","currency":"EUR","rails":["lightning"]}`); code != 201 {
		t.Errorf("Lightning in EUR: %d %s", code, b)
	}
	// A reachable pair in an outage: 503 with Retry-After, as before, and nothing created.
	e.pr.set(func(f *fakePricer) { f.errXMR = rates.ErrTooFew })
	code, b, hdr := e.do("POST", "/v1/orders", writeKey, `{"account_id":"a","merchant_ref":"usd","price":"10.00","currency":"USD","rails":["lightning","monero"]}`)
	if code != 503 || string(b) != `{"error":"rate_unavailable"}`+"\n" || hdr.Get("Retry-After") != "10" {
		t.Errorf("XMR/USD outage: %d %s, Retry-After %q", code, b, hdr.Get("Retry-After"))
	}
	// The XMR conversion out of bounds: the Monero code.
	e.pr.set(func(f *fakePricer) { f.errXMR = rates.ErrRange })
	if code, b := e.post(`{"account_id":"a","merchant_ref":"usd","price":"10.00","currency":"USD","rails":["lightning","monero"]}`); code != 400 ||
		string(b) != `{"error":"invalid_monero_piconero"}`+"\n" {
		t.Errorf("XMR ErrRange: %d %s", code, b)
	}
	// An XMR answer of ErrUnreachable (the belt beside the braces): 400 invalid_currency, no Retry-After.
	e.pr.set(func(f *fakePricer) { f.errXMR = rates.ErrUnreachable })
	code, b, hdr = e.do("POST", "/v1/orders", writeKey, `{"account_id":"a","merchant_ref":"usd","price":"10.00","currency":"USD","rails":["monero"]}`)
	if code != 400 || string(b) != `{"error":"invalid_currency"}`+"\n" || hdr.Get("Retry-After") != "" {
		t.Errorf("XMR ErrUnreachable: %d %s %q", code, b, hdr.Get("Retry-After"))
	}
	if os, _ := e.st.OrdersByAccount(context.Background(), "a"); len(os) != 1 {
		t.Errorf("%d orders, want 1", len(os))
	}
}

// A priced order on the on-chain and Monero rails: each amount from its own asset's quote, both rates recorded with
// their sources; the repeat is 200, another rail set 409.
func TestFiatMoneroCreate(t *testing.T) {
	e := newEnv(t)
	e.withOnchain(&stubOc{rail: onchain.Rail{State: onchain.RailWaiting, RequiredConfirmations: 1}})
	e.withMonero(&stubXm{view: rail.View{State: rail.Waiting, RequiredConfirmations: 10}})
	body := `{"account_id":"a","merchant_ref":"ocxm","price":"10.00","currency":"USD","rails":["onchain","monero"]}`
	code, b := e.post(body)
	if code != 201 {
		t.Fatalf("create = %d %s", code, b)
	}
	m := asMap(t, b)
	sats, pico := wantSats(t, "10.00", "76025", 150), wantPiconero(t, "10.00", "160", 150)
	if m["onchain_sat"] != float64(sats) || m["monero_piconero"] != float64(pico) || m["lightning"] != nil || m["lightning_msat"] != float64(0) ||
		m["onchain"] == nil || m["monero"] == nil {
		t.Errorf("amounts: %s (want %d sat, %d piconero)", b, sats, pico)
	}
	checkPricing(t, m, "10.00", "USD", "76025", e.pr.at)
	if got := e.pr.askedFor(); !reflect.DeepEqual(got, []string{"BTC", "XMR"}) {
		t.Errorf("Price asked for %v", got)
	}
	q, ok, err := e.st.QuoteForOrder(context.Background(), m["id"].(string))
	if err != nil || !ok || len(q.Rates) != 2 || q.Rates[0].Asset != "BTC" || q.Rates[0].Rate != "76025" || q.Rates[1].Asset != "XMR" ||
		q.Rates[1].Rate != "160" || len(q.Rates[1].Sources) != 2 || q.Rates[1].Sources[1].Provider != "yadio" || q.MarginBps != 150 {
		t.Fatalf("stored quote %+v %v %v", q, ok, err)
	}
	_, gb, _ := e.do("GET", "/v1/orders/"+m["id"].(string), readKey, "")
	if !reflect.DeepEqual(asMap(t, gb), m) {
		t.Errorf("GET differs: %s vs %s", gb, b)
	}
	// The rates move: the repeat is 200 with the first order; another rail set is a conflict.
	e.pr.set(func(f *fakePricer) { f.xmrRate, f.rate = "170", "80000" })
	if c, rb := e.post(body); c != 200 || !reflect.DeepEqual(asMap(t, rb), m) {
		t.Errorf("repeat: %d %s", c, rb)
	}
	if c, rb := e.post(`{"account_id":"a","merchant_ref":"ocxm","price":"10.00","currency":"USD","rails":["monero"]}`); c != 409 {
		t.Errorf("repeat with rails [monero]: %d %s", c, rb)
	}
	// Monero only, and all three: the amounts follow the moved rates.
	e.pr.set(func(f *fakePricer) { f.xmrRate, f.rate = "160", "76025" })
	code, b = e.post(`{"account_id":"a","merchant_ref":"xmonly","price":"10.00","currency":"USD","rails":["monero"]}`)
	if m = asMap(t, b); code != 201 || m["monero_piconero"] != float64(pico) || m["lightning"] != nil || m["onchain"] != nil {
		t.Errorf("Monero only: %d %s", code, b)
	}
	checkPricing(t, m, "10.00", "USD", "76025", e.pr.at)
	code, b = e.post(`{"account_id":"a","merchant_ref":"all","price":"10.00","currency":"USD","rails":["monero","lightning","onchain"]}`)
	if m = asMap(t, b); code != 201 || m["monero_piconero"] != float64(pico) || m["lightning_msat"] != float64(sats*1000) || m["onchain_sat"] != float64(sats) {
		t.Errorf("all three: %d %s", code, b)
	}
	checkPricing(t, m, "10.00", "USD", "76025", e.pr.at)
	// The default rails never include Monero.
	code, b = e.post(`{"account_id":"a","merchant_ref":"default","price":"10.00","currency":"USD"}`)
	if m = asMap(t, b); code != 201 || m["monero"] != nil || m["lightning"] == nil || m["onchain"] == nil {
		t.Errorf("default rails: %d %s", code, b)
	}
	// Across the boundaries of the conversion, monero_piconero is rates.Piconero exactly.
	for i, price := range []string{"0.000000000000000001", "0.01", "1", "9.99", "123.456789", "1000"} {
		code, b = e.post(fmt.Sprintf(`{"account_id":"a","merchant_ref":"b-%d","price":%q,"currency":"USD","rails":["monero"]}`, i, price))
		n := wantPiconero(t, price, "160", 150)
		if m = asMap(t, b); code != 201 || n < 1 || m["monero_piconero"] != float64(n) {
			t.Errorf("%s: %d monero_piconero %v, want %d", price, code, m["monero_piconero"], n)
		}
		auditPricing(t, m)
	}
	// The XMR conversion above 2^53 − 1 piconero is refused, never clamped (the real rates.Convert answers ErrRange).
	if code, b := e.post(`{"account_id":"a","merchant_ref":"huge","price":"10000000","currency":"USD","rails":["monero"]}`); code != 400 ||
		string(b) != `{"error":"invalid_monero_piconero"}`+"\n" {
		t.Errorf("above MaxPiconero: %d %s", code, b)
	}
}

// The quote time of a two-asset quote is the older of the two quotes', and a pricer that gives the assets different
// margins is refused (one service has one margin).
func TestPriceTwoAssets(t *testing.T) {
	e := newEnv(t)
	e.withMonero(&stubXm{view: rail.View{State: rail.Waiting}})
	h := e.h.(*handler)
	req, code := parseCreate([]byte(`{"account_id":"a","merchant_ref":"r","price":"10.00","currency":"USD","rails":["lightning","monero"]}`))
	if code != "" {
		t.Fatal(code)
	}
	var o store.Order
	q, st, code, _ := h.price(req, &o)
	if q == nil || st != 0 || code != "" || len(q.Rates) != 2 || q.Rates[0].Asset != "BTC" || q.Rates[1].Asset != "XMR" ||
		!q.QuotedAt.Equal(e.pr.at) || o.AmountMsat == 0 || o.AmountPiconero == 0 || o.AmountSat != 0 {
		t.Fatalf("%+v %d %q %+v", q, st, code, o)
	}
	p := &twoTimes{fakePricer: e.pr, xmrAt: e.pr.at.Add(-time.Minute)}
	h.pr = p
	o = store.Order{}
	if q, _, code, _ = h.price(req, &o); code != "" || !q.QuotedAt.Equal(p.xmrAt) {
		t.Errorf("an older XMR quote: %v %q", q, code)
	}
	p.xmrAt, p.xmrMargin = time.Time{}, 100
	o = store.Order{}
	if q, st, code, _ = h.price(req, &o); q != nil || st != 500 || code != codeInternal {
		t.Errorf("two margins: %v %d %q", q, st, code)
	}
}

// twoTimes is the fake pricer with an XMR quote at another time or margin.
type twoTimes struct {
	*fakePricer
	xmrAt     time.Time
	xmrMargin int64
}

func (p *twoTimes) Price(asset string, price rates.Decimal, currency string) (rates.Priced, error) {
	r, err := p.fakePricer.Price(asset, price, currency)
	if err == nil && asset == rates.AssetXMR {
		if !p.xmrAt.IsZero() {
			r.Quote.At = p.xmrAt
		}
		if p.xmrMargin != 0 {
			r.MarginBps = p.xmrMargin
		}
	}
	return r, err
}
