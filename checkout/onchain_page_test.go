package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/qr"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/status"
	"github.com/satbooth/satbooth/store"
)

// fakeOc is the on-chain rail: it stores the addresses it makes in the real store, and answers Rail from the store
// plus whatever the test sets in tweak.
type fakeOc struct {
	st    *store.Store
	mu    sync.Mutex
	err   error                       // Address answers this when set
	delay time.Duration               // Address sleeps this long (until its context ends)
	wait  func(context.Context) error // Address calls this first when set
	tweak func(*onchain.Rail)
	calls atomic.Int64
	made  atomic.Int64
}

func (f *fakeOc) Rail(ctx context.Context, o store.Order) (onchain.Rail, error) {
	r := onchain.Rail{State: onchain.RailWaiting, RequiredConfirmations: 1}
	if a, err := f.st.AddressForOrder(ctx, o.ID); err == nil {
		r.Issued, r.Address = true, a.Address
	}
	f.mu.Lock()
	tw := f.tweak
	f.mu.Unlock()
	if tw != nil {
		tw(&r)
	}
	return r, nil
}

func (f *fakeOc) setTweak(fn func(*onchain.Rail)) { f.mu.Lock(); f.tweak = fn; f.mu.Unlock() }

func (f *fakeOc) Address(ctx context.Context, orderID string) (store.Address, error) {
	f.calls.Add(1)
	if f.wait != nil {
		if err := f.wait(ctx); err != nil {
			return store.Address{}, fmt.Errorf("%w: %w", onchain.ErrBusy, err)
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return store.Address{}, fmt.Errorf("%w: %w", onchain.ErrBusy, ctx.Err())
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Address{}, f.err
	}
	if a, err := f.st.AddressForOrder(ctx, orderID); err == nil {
		return a, nil
	}
	idx, ok, err := f.st.MaxAddressIndex(ctx)
	if err != nil {
		return store.Address{}, err
	}
	if ok {
		idx++
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("addr %d", idx)))
	a := store.Address{OrderID: orderID, Address: "bcrt1q" + hex.EncodeToString(sum[:])[:38], Index: idx}
	if err := f.st.AddAddress(ctx, a); err != nil {
		return store.Address{}, err
	}
	f.made.Add(1)
	return a, nil
}

const sat21 = 21_000 // 0.00021 BTC

func (e *env) addrOf(o store.Order) string {
	e.t.Helper()
	a, err := e.st.AddressForOrder(context.Background(), o.ID)
	if err != nil {
		e.t.Fatalf("no address: %v", err)
	}
	return a.Address
}

func (e *env) invoices(o store.Order) []store.Invoice {
	invs, _ := e.st.InvoicesForOrder(context.Background(), o.ID)
	return invs
}

var (
	hrefRe  = regexp.MustCompile(`<a href="([^"]*)">Open in wallet</a>`)
	imgRe   = regexp.MustCompile(`<img class="qr" `)
	valueRe = func(id string) *regexp.Regexp {
		return regexp.MustCompile(`id="` + id + `" type="text" readonly="readonly" value="([^"]*)"`)
	}
)

func href(t *testing.T, body string) (string, bool) {
	t.Helper()
	m := hrefRe.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return html.UnescapeString(m[1]), true
}

func TestOnchainOnlyButtonAndPay(t *testing.T) {
	e := newEnv(t)
	o := e.orderBoth(0, sat21, time.Hour)
	r := e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	for _, want := range []string{"0.00021 BTC", "Pay with bitcoin", `action="/c/` + o.Token + `/pay"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q:\n%s", want, r.body)
		}
	}
	if strings.Contains(r.body, "sats") || strings.Contains(r.body, " or ") {
		t.Errorf("a Lightning amount on an on-chain-only order:\n%s", r.body)
	}
	for i := 0; i < 2; i++ { // twice: one address
		if p := e.do("POST", "/c/"+o.Token+"/pay"); p.code != 303 || p.h.Get("Location") != "/c/"+o.Token {
			t.Fatalf("pay %d: %d %q", i, p.code, p.h.Get("Location"))
		}
	}
	if e.oc.made.Load() != 1 {
		t.Errorf("%d addresses made", e.oc.made.Load())
	}
	if len(e.invoices(o)) != 0 || e.w.makes.Load() != 0 || e.ln.pays.Load() != 0 {
		t.Error("an invoice was made for an on-chain-only order")
	}
	r = e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	addr := e.addrOf(o)
	uri, _ := PaymentURI(addr, sat21, "")
	h, ok := href(t, r.body)
	for _, want := range []string{`http-equiv="refresh"`, `<img class="qr"`, `id="address"`, `value="` + addr + `"`, "0.00021 BTC", "Open in wallet"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q:\n%s", want, r.body)
		}
	}
	if !ok || h != uri {
		t.Errorf("href %q, want %q", h, uri)
	}
	for _, bad := range []string{"Lightning invoice", "New Lightning invoice", "lightning=", "LIGHTNING", "Pay with"} {
		if strings.Contains(r.body, bad) {
			t.Errorf("unexpected %q", bad)
		}
	}
	q := e.get("/c/" + o.Token + "/qr.svg")
	want, _ := qr.SVG(uri)
	if q.code != 200 || q.body != string(want) {
		t.Errorf("qr: %d, not the QR of the URI", q.code)
	}
}

func TestMixedUnifiedURI(t *testing.T) {
	e := newEnv(t)
	o := e.orderBoth(21_000, sat21, time.Hour)
	r := e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	if !strings.Contains(r.body, "21 sats or 0.00021 BTC") || !strings.Contains(r.body, ">Pay<") {
		t.Errorf("new mixed page:\n%s", r.body)
	}
	if p := e.do("POST", "/c/"+o.Token+"/pay"); p.code != 303 || p.h.Get("Location") != "/c/"+o.Token {
		t.Fatalf("pay: %d %q", p.code, p.h.Get("Location"))
	}
	r = e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	addr, inv := e.addrOf(o), e.invoices(o)[0].Bolt11
	uri, err := PaymentURI(addr, sat21, inv)
	if err != nil {
		t.Fatal(err)
	}
	if len(uri) > qr.MaxInput {
		t.Fatalf("test invoice too long: %d", len(uri))
	}
	if h, ok := href(t, r.body); !ok || h != uri {
		t.Errorf("href %q, want the QR text %q", h, uri)
	}
	if m := valueRe("address").FindStringSubmatch(r.body); m == nil || m[1] != addr || addr != strings.ToLower(addr) {
		t.Errorf("address field: %v", m)
	}
	if m := valueRe("invoice").FindStringSubmatch(r.body); m == nil || m[1] != inv {
		t.Errorf("invoice field: %v", m)
	}
	for _, want := range []string{"0.00021 BTC", "About 20 min left", `<img class="qr"`, `http-equiv="refresh"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q", want)
		}
	}
	q := e.get("/c/" + o.Token + "/qr.svg")
	svg, _ := qr.SVG(uri)
	if q.code != 200 || q.body != string(svg) {
		t.Errorf("qr is not the QR of the unified URI (%d)", q.code)
	}
}

// A paid mixed order (the on-chain rail reads paid) makes no invoice and no address.
func TestPayRefusedWhenOnchainPaid(t *testing.T) {
	e := newEnv(t)
	o := e.orderBoth(21_000, sat21, time.Hour)
	e.oc.setTweak(func(r *onchain.Rail) {
		r.Paid, r.State, r.ConfirmedSat, r.ReceivedSat = true, onchain.RailPaid, sat21, sat21
	})
	p := e.do("POST", "/c/"+o.Token+"/pay")
	if p.code != 303 || p.h.Get("Location") != "/c/"+o.Token {
		t.Fatalf("pay: %d %q", p.code, p.h.Get("Location"))
	}
	if len(e.invoices(o)) != 0 || e.w.makes.Load() != 0 || e.ln.pays.Load() != 0 || e.oc.calls.Load() != 0 {
		t.Errorf("a paid order made something: %d invoices, %d pays, %d address calls", len(e.invoices(o)), e.ln.pays.Load(), e.oc.calls.Load())
	}
	if r := e.get("/c/" + o.Token); !strings.Contains(r.body, ">Paid<") || strings.Contains(r.body, "<form") {
		t.Errorf("paid page:\n%s", r.body)
	}
}

// Seen on-chain and the Lightning invoice expired: no new invoice, by page or by press.
func TestNoNewInvoiceOnceSeen(t *testing.T) {
	e := newEnv(t)
	o := e.orderBoth(21_000, sat21, 3*time.Hour)
	e.do("POST", "/c/"+o.Token+"/pay")
	e.clk.Advance(25 * time.Minute) // the invoice (20 min) is expired, the order is open
	r := e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	if !strings.Contains(r.body, "New Lightning invoice") || strings.Contains(r.body, "Lightning invoice</label>") {
		t.Errorf("an expired invoice on an open order gets a new-invoice button, no field:\n%s", r.body)
	}
	if h, ok := href(t, r.body); !ok || !strings.Contains(h, "?amount=0.00021") || strings.Contains(h, "LIGHTNING=") {
		t.Errorf("link %q", h)
	}
	e.oc.setTweak(func(r *onchain.Rail) {
		r.State, r.ReceivedSat, r.Confirmations, r.RequiredConfirmations = onchain.RailSeen, sat21, 0, 3
	})
	r = e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	if strings.Contains(r.body, "New Lightning invoice") || strings.Contains(r.body, "<form") {
		t.Errorf("a new-invoice button after the payment was seen:\n%s", r.body)
	}
	if !strings.Contains(r.body, "Seen, 0 of 3 confirmations") {
		t.Errorf("no confirmations line:\n%s", r.body)
	}
	makes := e.w.makes.Load()
	p := e.do("POST", "/c/"+o.Token+"/pay")
	if p.code != 303 || e.w.makes.Load() != makes {
		t.Errorf("a press after the payment was seen: %d, %d makes", p.code, e.w.makes.Load()-makes)
	}
}

func TestSeenBelowTheAmount(t *testing.T) {
	e := newEnv(t)
	o := e.orderBoth(0, 1_234_567, time.Hour)
	e.do("POST", "/c/"+o.Token+"/pay")
	e.oc.setTweak(func(r *onchain.Rail) {
		r.State, r.ReceivedSat, r.Confirmations, r.RequiredConfirmations = onchain.RailSeen, 200_000, 2, 3
	})
	r := e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	for _, want := range []string{"Seen, 2 of 3 confirmations", "200,000 sats seen so far", "0.01234567 BTC"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q:\n%s", want, r.body)
		}
	}
	e.oc.setTweak(func(r *onchain.Rail) {
		r.State, r.ReceivedSat, r.Confirmations, r.RequiredConfirmations = onchain.RailSeen, 1_234_567, 1, 3
	})
	if r := e.get("/c/" + o.Token); strings.Contains(r.body, "seen so far") || !strings.Contains(r.body, "Seen, 1 of 3") {
		t.Errorf("full amount seen:\n%s", r.body)
	}
}

func TestStaleIsChecking(t *testing.T) {
	e := newEnv(t)
	o := e.orderBoth(0, sat21, time.Hour)
	e.do("POST", "/c/"+o.Token+"/pay")
	e.oc.setTweak(func(r *onchain.Rail) {
		r.State, r.ReceivedSat, r.Confirmations, r.RequiredConfirmations, r.Stale = onchain.RailSeen, sat21, 1, 3, true
	})
	r := e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	if r.code != 200 || !strings.Contains(r.body, "Checking payment…") || strings.Contains(r.body, "Seen,") {
		t.Errorf("stale page (%d):\n%s", r.code, r.body)
	}
	// Stale and "expired": the page does not claim nothing was charged.
	e.clk.Advance(2 * time.Hour)
	e.oc.setTweak(func(r *onchain.Rail) { r.State, r.Stale = onchain.RailExpired, true })
	r = e.get("/c/" + o.Token)
	if r.code != 200 || !strings.Contains(r.body, "Checking payment…") || strings.Contains(r.body, "Nothing was charged") {
		t.Errorf("stale expired page (%d):\n%s", r.code, r.body)
	}
}

// The on-chain rail is not configured: an order with an on-chain amount reads "Checking", never a 500; a press is busy.
func TestRailOffIsChecking(t *testing.T) {
	e := newEnv(t)
	o := e.orderBoth(21_000, sat21, time.Hour)
	l := e.order(5_000, time.Hour)
	e.build(nil)
	r := e.get("/c/" + o.Token)
	checkHeaders(t, "page", r)
	checkPlain(t, r.body)
	if r.code != 200 || !strings.Contains(r.body, "Checking payment…") || strings.Contains(r.body, "<form") ||
		!strings.Contains(r.body, `content="5; url=/c/`+o.Token+`"`) { // the refresh comes back to this order
		t.Errorf("page (%d):\n%s", r.code, r.body)
	}
	if q := e.get("/c/" + o.Token + "/qr.svg"); q.code != 404 {
		t.Errorf("qr: %d", q.code)
	}
	if p := e.do("POST", "/c/"+o.Token+"/pay"); p.code != 303 || p.h.Get("Location") != "/c/"+o.Token+"?busy=1" || e.ln.pays.Load() != 0 {
		t.Errorf("pay: %d %q", p.code, p.h.Get("Location"))
	}
	if r := e.get("/c/" + l.Token); !strings.Contains(r.body, "Pay with Lightning") {
		t.Errorf("a Lightning-only order is affected by the missing rail:\n%s", r.body)
	}
}

// Lightning keeps working while bitcoind is down.
func TestAddressRefusalDoesNotStopLightning(t *testing.T) {
	for _, ae := range []error{onchain.ErrAddress, onchain.ErrIndexBehind, onchain.ErrBusy, onchain.ErrUnavailable,
		fmt.Errorf("wrapped: %w", onchain.ErrUnavailable)} {
		e := newEnv(t)
		e.oc.err = ae
		o := e.orderBoth(21_000, sat21, time.Hour)
		p := e.do("POST", "/c/"+o.Token+"/pay")
		if p.code != 303 || p.h.Get("Location") != "/c/"+o.Token+"?busy=1" {
			t.Errorf("%v: pay %d %q", ae, p.code, p.h.Get("Location"))
		}
		if len(e.invoices(o)) != 1 {
			t.Errorf("%v: %d invoices", ae, len(e.invoices(o)))
		}
		r := e.get(p.h.Get("Location"))
		checkPlain(t, r.body)
		if !strings.Contains(r.body, "Busy, try again.") || !strings.Contains(r.body, `id="invoice"`) || strings.Contains(r.body, `id="address"`) {
			t.Errorf("%v: page:\n%s", ae, r.body)
		}
	}
	// Other answers of Address: paid and expired are plain redirects.
	for _, ae := range []error{onchain.ErrPaid, onchain.ErrExpired} {
		e := newEnv(t)
		e.oc.err = ae
		o := e.orderBoth(21_000, sat21, time.Hour)
		if p := e.do("POST", "/c/"+o.Token+"/pay"); p.code != 303 || p.h.Get("Location") != "/c/"+o.Token {
			t.Errorf("%v: pay %d %q", ae, p.code, p.h.Get("Location"))
		}
	}
	// An unknown error while the invoice was made: logged once, redirect (busy), no 500.
	e := newEnv(t)
	e.oc.err = errors.New("boom")
	o := e.orderBoth(21_000, sat21, time.Hour)
	p := e.do("POST", "/c/"+o.Token+"/pay")
	if p.code != 303 || p.h.Get("Location") != "/c/"+o.Token+"?busy=1" || len(e.invoices(o)) != 1 || strings.Count(e.logs.String(), "unexpected error") != 1 {
		t.Errorf("one step made, one unknown error: %d %q, %d invoices, log %q", p.code, p.h.Get("Location"), len(e.invoices(o)), e.logs.String())
	}
	// The same the other way: the address was made, the invoice failed unknown.
	e = newEnv(t)
	e.ln = &countLn{Lightning: failLn{e.ln.Lightning}}
	e.build(e.oc)
	o = e.orderBoth(21_000, sat21, time.Hour)
	if p := e.do("POST", "/c/"+o.Token+"/pay"); p.code != 303 || e.oc.made.Load() != 1 {
		t.Errorf("address made, invoice unknown error: %d", p.code)
	}
	// Nothing succeeded: 500.
	e = newEnv(t)
	e.oc.err = errors.New("boom")
	o = e.orderBoth(0, sat21, time.Hour)
	if p := e.do("POST", "/c/"+o.Token+"/pay"); p.code != 500 {
		t.Errorf("every step failed: %d", p.code)
	}
	e = newEnv(t)
	e.oc.err = errors.New("boom")
	e.ln = &countLn{Lightning: failLn{e.ln.Lightning}}
	e.build(e.oc)
	o = e.orderBoth(21_000, sat21, time.Hour)
	if p := e.do("POST", "/c/"+o.Token+"/pay"); p.code != 500 {
		t.Errorf("both steps failed: %d", p.code)
	}
}

type failLn struct{ status.Lightning }

func (failLn) Pay(context.Context, string) (store.Invoice, error) {
	return store.Invoice{}, errors.New("boom")
}

func TestWalletRefusalDoesNotStopTheAddress(t *testing.T) {
	e := newEnv(t)
	e.w.err = errors.New("wallet exploded")
	o := e.orderBoth(21_000, sat21, time.Hour)
	p := e.do("POST", "/c/"+o.Token+"/pay")
	if p.code != 303 || p.h.Get("Location") != "/c/"+o.Token+"?busy=1" || e.oc.made.Load() != 1 {
		t.Errorf("pay: %d %q, %d addresses", p.code, p.h.Get("Location"), e.oc.made.Load())
	}
}

func TestLightningOnlyNeverAsksForAnAddress(t *testing.T) {
	e := newEnv(t)
	o := e.order(5_000, time.Hour)
	e.do("POST", "/c/"+o.Token+"/pay")
	if e.oc.calls.Load() != 0 {
		t.Error("Address called for a Lightning-only order")
	}
	r := e.get("/c/" + o.Token)
	for _, bad := range []string{"Bitcoin address", "BTC", "bitcoin:"} {
		if strings.Contains(r.body, bad) {
			t.Errorf("unexpected %q in a Lightning-only page:\n%s", bad, r.body)
		}
	}
	if q := e.get("/c/" + o.Token + "/qr.svg"); q.code != 200 {
		t.Errorf("qr %d", q.code)
	}
}

// --- render-only tests over a stub resolver ---

type stubRes struct {
	so  status.Order
	err error
}

func (s *stubRes) ByToken(_ context.Context, t string) (status.Order, error) {
	if s.err != nil {
		return status.Order{}, s.err
	}
	if t != s.so.Order.Token {
		return status.Order{}, store.ErrNotFound
	}
	return s.so, nil
}

const stubAddr = "bcrt1qtestaddress0000000000000000000000000"

func stubOrder(msat, sat int64, invoice string) status.Order {
	o := status.Order{
		Order: store.Order{ID: strings.Repeat("1", 32), Token: strings.Repeat("2", 32), AmountMsat: msat, AmountSat: sat, ExpiresAt: epoch.Add(time.Hour)},
		State: status.StateNew,
	}
	if msat > 0 {
		s := lnpay.Status{State: lnpay.StateNew}
		if invoice != "" {
			s.Invoice = &store.Invoice{Bolt11: invoice, AmountMsat: msat, ExpiresAt: epoch.Add(20 * time.Minute)}
		}
		o.Lightning = &s
	}
	if sat > 0 {
		o.Onchain = &onchain.Rail{State: onchain.RailWaiting, Issued: true, Address: stubAddr, RequiredConfirmations: 1}
	}
	return o
}

// stubOrder3 is stubOrder with a Monero rail of pico piconero, its subaddress issued.
func stubOrder3(msat, sat, pico int64, invoice string) status.Order {
	o := stubOrder(msat, sat, invoice)
	o.Order.AmountPiconero = pico
	if pico > 0 {
		o.Monero = &rail.View{State: rail.Waiting, Issued: true, Address: testSub, RequiredConfirmations: 10}
	}
	return o
}

func stubHandler(so status.Order, logs io.Writer) http.Handler {
	clk := &clock{t: epoch}
	return newHandler(&stubRes{so: so}, nil, nil, nil, clk.Now, slog.New(slog.NewJSONHandler(logs, nil)))
}

func getStub(h http.Handler, so status.Order, suffix string) resp {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/c/"+so.Order.Token+suffix, nil))
	return resp{rec.Code, rec.Body.String(), rec.Header()}
}

// An address and a 4,000-character invoice: no QR (the URI is over qr.MaxInput), the fields and the link stay.
func TestHugeInvoiceKeepsFieldsAndLink(t *testing.T) {
	inv := "lnbcrt" + strings.Repeat("q", 3994)
	so := stubOrder(21_000, sat21, inv)
	var logs strings.Builder
	h := stubHandler(so, &logs)
	r := getStub(h, so, "")
	checkPlain(t, r.body)
	if imgRe.MatchString(r.body) {
		t.Errorf("an <img> for a URI over %d bytes", qr.MaxInput)
	}
	uri, err := PaymentURI(stubAddr, sat21, inv)
	if err != nil || len(uri) <= qr.MaxInput {
		t.Fatalf("test URI: %v, %d bytes", err, len(uri))
	}
	if h, ok := href(t, r.body); !ok || h != uri {
		t.Errorf("link missing or different")
	}
	if m := valueRe("invoice").FindStringSubmatch(r.body); m == nil || m[1] != inv {
		t.Error("invoice field")
	}
	if m := valueRe("address").FindStringSubmatch(r.body); m == nil || m[1] != stubAddr {
		t.Error("address field")
	}
	if q := getStub(h, so, "/qr.svg"); q.code != 404 {
		t.Errorf("qr.svg of a URI that does not fit: %d", q.code)
	}
	if logs.Len() != 0 {
		t.Errorf("a valid invoice logged: %s", logs.String())
	}
	// Lightning-only, with a long invoice: no QR, the field and the link stay.
	so = stubOrder(21_000, 0, "lnbcrt"+strings.Repeat("q", 1300))
	r = getStub(stubHandler(so, io.Discard), so, "")
	if imgRe.MatchString(r.body) || !strings.Contains(r.body, `id="invoice"`) {
		t.Errorf("long Lightning-only page:\n%.400s", r.body)
	}
}

// An invoice PaymentURI refuses (the store allows any printable ASCII): one fixed ALERT, then the address-only URI
// for the QR and the link; the page does not fail.
func TestRefusedInvoiceFallsBackToTheAddressURI(t *testing.T) {
	inv := "lnbcrt1-abc/def?x=y&z=1 \"<script>"
	so := stubOrder(21_000, sat21, inv)
	var logs strings.Builder
	h := stubHandler(so, &logs)
	r := getStub(h, so, "")
	checkPlain(t, r.body)
	uri, _ := PaymentURI(stubAddr, sat21, "")
	if hf, ok := href(t, r.body); !ok || hf != uri {
		t.Errorf("link %q, want the address-only URI %q", hf, uri)
	}
	if !imgRe.MatchString(r.body) {
		t.Error("no QR")
	}
	if q := getStub(h, so, "/qr.svg"); q.code != 200 {
		t.Errorf("qr.svg %d", q.code)
	} else if want, _ := qr.SVG(uri); q.body != string(want) {
		t.Error("the QR is not the address-only URI's")
	}
	if m := valueRe("invoice").FindStringSubmatch(r.body); m == nil || html.UnescapeString(m[1]) != inv {
		t.Errorf("invoice field: %v", m)
	}
	if !strings.Contains(r.body, `id="address"`) || strings.Contains(r.body, "<script>") || strings.Contains(r.body, "ZgotmplZ") {
		t.Errorf("page:\n%s", r.body)
	}
	if n := strings.Count(logs.String(), "ALERT"); n != 1 || strings.Contains(logs.String(), "script") || strings.Contains(logs.String(), "abc") {
		t.Errorf("log: %q", logs.String())
	}
	// Three renders of the same refused invoice: one ALERT in all (Ruling X2). The QR route does not log.
	logs.Reset()
	h = stubHandler(so, &logs)
	for i := 0; i < 3; i++ {
		getStub(h, so, "")
		getStub(h, so, "/qr.svg")
	}
	if n := strings.Count(logs.String(), "ALERT"); n != 1 {
		t.Errorf("%d ALERT lines for three renders: %q", n, logs.String())
	}
	// A Lightning-only order with such an invoice: no QR, no link, both text fields stay, one ALERT.
	so = stubOrder(21_000, 0, inv)
	logs.Reset()
	h = stubHandler(so, &logs)
	r = getStub(h, so, "")
	getStub(h, so, "")
	if imgRe.MatchString(r.body) || strings.Contains(r.body, "<a ") || !strings.Contains(r.body, `id="invoice"`) ||
		strings.Count(logs.String(), "ALERT") != 1 {
		t.Errorf("Lightning-only refused invoice: log %q\n%s", logs.String(), r.body)
	}
	if text, ok := qrText(so); ok || text != "" {
		t.Errorf("qrText %q %v", text, ok)
	}
	if q := getStub(h, so, "/qr.svg"); q.code != 404 {
		t.Errorf("qr.svg %d", q.code)
	}
}

// Ruling W2: a Lightning-only order links "lightning:" and the uppercase invoice, and nothing else.
func TestLightningOnlyLink(t *testing.T) {
	inv := "lnbcrt1q0w8abcXYZ09"
	so := stubOrder(21_000, 0, inv)
	r := getStub(stubHandler(so, io.Discard), so, "")
	checkPlain(t, r.body)
	hf, ok := href(t, r.body)
	if !ok || hf != "lightning:LNBCRT1Q0W8ABCXYZ09" || strings.Contains(r.body, "ZgotmplZ") {
		t.Errorf("link %q %v\n%s", hf, ok, r.body)
	}
	if !regexp.MustCompile(`^lightning:[A-Z0-9]+$`).MatchString(hf) {
		t.Errorf("link %q is not lightning: and [A-Z0-9]", hf)
	}
	if !imgRe.MatchString(r.body) {
		t.Error("no QR")
	}
}

func TestQRTextCases(t *testing.T) {
	inv := "lnbcrt1abc"
	both, ln, oc := stubOrder(21_000, sat21, inv), stubOrder(21_000, 0, inv), stubOrder(0, sat21, "")
	if got, ok := qrText(both); !ok || got != "bitcoin:"+strings.ToUpper(stubAddr)+"?amount=0.00021&lightning=LNBCRT1ABC" {
		t.Errorf("both: %q", got)
	}
	if got, ok := qrText(oc); !ok || got != "bitcoin:"+strings.ToUpper(stubAddr)+"?amount=0.00021" {
		t.Errorf("address only: %q", got)
	}
	if got, ok := qrText(ln); !ok || got != "LIGHTNING:LNBCRT1ABC" {
		t.Errorf("invoice only: %q", got)
	}
	none := stubOrder(21_000, sat21, "")
	none.Onchain.Issued, none.Onchain.Address = false, ""
	if got, ok := qrText(none); ok || got != "" {
		t.Errorf("nothing issued: %q", got)
	}
	// An address plus an expired invoice (Status.Invoice nil): the address-only URI.
	exp := stubOrder(21_000, sat21, "")
	if got, ok := qrText(exp); !ok || strings.Contains(got, "lightning") {
		t.Errorf("expired invoice: %q", got)
	}
}

func TestEndStates(t *testing.T) {
	type tc struct {
		name  string
		build func(*status.Order)
		want  []string
		not   []string
	}
	for _, c := range []tc{
		{"underpaid", func(o *status.Order) { o.State = status.StateUnderpaid; o.Onchain.ReceivedSat = sat21 - 1 },
			[]string{"Payment received is below the amount. The merchant will contact you."},
			[]string{"<form", "refresh", `<img class="qr"`, "refund", "not yet confirmed"}},
		// The window end: the whole amount was seen, but not confirmed enough in the watch window.
		{"underpaid at the window end", func(o *status.Order) {
			o.State = status.StateUnderpaid
			o.Onchain.State, o.Onchain.ReceivedSat = onchain.RailUnderpaid, sat21
		}, []string{">Underpaid<", "Your payment is not yet confirmed enough. The merchant will contact you."},
			[]string{"<form", "refresh", `<img class="qr"`, "refund", "below the amount"}},
		// A stuck payment that died after order.underpaid: nothing live, the event still says underpaid.
		{"underpaid with nothing live", func(o *status.Order) {
			o.State = status.StateUnderpaid
			o.Onchain.State, o.Onchain.ReceivedSat = onchain.RailUnderpaid, 0
		}, []string{">Underpaid<", "Your payment did not confirm. The merchant will contact you."},
			[]string{"<form", "refresh", `<img class="qr"`, "refund", "below the amount", "not yet confirmed", "Nothing was charged"}},
		{"late", func(o *status.Order) { o.State = status.StateLate },
			[]string{"Your payment arrived after the quote expired. The merchant will contact you."}, []string{"<form", "refresh", `<img class="qr"`, "refund"}},
		{"paid", func(o *status.Order) { o.State = status.StatePaid },
			[]string{">Paid<"}, []string{"<form", "refresh", "refund"}},
		{"paid overpaid", func(o *status.Order) {
			o.State = status.StatePaid
			o.Onchain.Paid, o.Onchain.Overpaid, o.Onchain.ReceivedSat = true, true, sat21+5
		}, []string{">Paid<", "The merchant will refund the extra."}, []string{"<form", "refresh"}},
		{"paid late sat", func(o *status.Order) { o.State = status.StatePaid; o.Onchain.LateSat = 100 },
			[]string{">Paid<", "The merchant will refund the extra."}, nil},
		{"paid on Lightning, then seen on-chain", func(o *status.Order) { o.State = status.StatePaid; o.Onchain.ReceivedSat = 5 },
			[]string{">Paid<", "The merchant will refund the extra."}, nil},
		{"paid on both rails, on-chain exact", func(o *status.Order) {
			o.State = status.StatePaid
			o.Lightning.State = lnpay.StatePaid
			o.Onchain.Paid, o.Onchain.ReceivedSat, o.Onchain.ConfirmedSat = true, sat21, sat21
		}, []string{">Paid<", "The merchant will refund the extra."}, nil},
		{"paid on Lightning only", func(o *status.Order) { o.State = status.StatePaid; o.Lightning.State = lnpay.StatePaid },
			[]string{">Paid<"}, []string{"refund"}},
		{"expired", func(o *status.Order) { o.State = status.StateExpired },
			[]string{">Expired<", "Nothing was charged"}, []string{"<form", "refresh", `<img class="qr"`, "refund"}},
	} {
		so := stubOrder(21_000, sat21, "lnbcrt1abc")
		c.build(&so)
		r := getStub(stubHandler(so, io.Discard), so, "")
		checkPlain(t, r.body)
		for _, w := range c.want {
			if !strings.Contains(r.body, w) {
				t.Errorf("%s: missing %q:\n%s", c.name, w, r.body)
			}
		}
		for _, b := range c.not {
			if strings.Contains(r.body, b) {
				t.Errorf("%s: unexpected %q", c.name, b)
			}
		}
		if q := getStub(stubHandler(so, io.Discard), so, "/qr.svg"); q.code != 404 {
			t.Errorf("%s: qr.svg %d", c.name, q.code)
		}
	}
}

func TestAmountsLine(t *testing.T) {
	for _, c := range []struct {
		msat, sat int64
		want      string
	}{
		{21_000, 0, "21 sats"}, {1_000_000_000, 0, "1,000,000 sats"}, {0, 21_000, "0.00021 BTC"}, {123_456_000, 0, "123,456 sats"},
		{21_000_000, 21_000, "21,000 sats or 0.00021 BTC"}, {1_000, 1, "1 sat or 0.00000001 BTC"},
		{0, 2_100_000_000_000_000, "21000000 BTC"}, {999_000, 0, "999 sats"}, {1_000_000, 0, "1,000 sats"},
	} {
		if got := amounts(store.Order{AmountMsat: c.msat, AmountSat: c.sat}); got != c.want {
			t.Errorf("(%d, %d): %q, want %q", c.msat, c.sat, got, c.want)
		}
	}
}

// thousands against a second statement of the rule, over every digit count.
func TestThousandsProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	ref := func(n int64) string {
		d := fmt.Sprint(n)
		var out []byte
		for i := range d {
			if i > 0 && (len(d)-i)%3 == 0 {
				out = append(out, ',')
			}
			out = append(out, d[i])
		}
		return string(out)
	}
	for i := 0; i < 5000; i++ {
		n := rng.Int63n(1 << uint(1+rng.Intn(61)))
		if got := thousands(n); got != ref(n) || strings.ReplaceAll(got, ",", "") != fmt.Sprint(n) {
			t.Fatalf("thousands(%d) = %q, want %q", n, got, ref(n))
		}
	}
	for _, n := range []int64{0, 1, 999, 1000, 1001, 999999, 1000000} {
		if got := thousands(n); got != ref(n) {
			t.Errorf("thousands(%d) = %q", n, got)
		}
	}
}

// --- the press under the server's write timeout ---

// barrier makes each step wait until both have started, so the test passes only if they run at the same time.
type barrier struct {
	n  atomic.Int32
	ch chan struct{}
}

func newBarrier() *barrier { return &barrier{ch: make(chan struct{})} }

func (b *barrier) arrive(ctx context.Context) error {
	if b.n.Add(1) == 2 {
		close(b.ch)
	}
	select {
	case <-b.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type slowLn struct {
	status.Lightning
	wait func(context.Context) error
}

func (s slowLn) Pay(ctx context.Context, token string) (store.Invoice, error) {
	if err := s.wait(ctx); err != nil {
		return store.Invoice{}, fmt.Errorf("%w: %w", lnpay.ErrUnknown, err)
	}
	return s.Lightning.Pay(ctx, token)
}

// The address and the invoice run at the same time: each waits for the other to start.
func TestPayRunsBothStepsConcurrently(t *testing.T) {
	e := newEnv(t)
	b := newBarrier()
	e.oc.wait = b.arrive
	e.ln = &countLn{Lightning: slowLn{e.ln.Lightning, b.arrive}}
	e.build(e.oc)
	e.h.(*handler).payBudget = 10 * time.Second // a sequential run would sit at the barrier until this ends
	o := e.orderBoth(21_000, sat21, time.Hour)
	p := e.do("POST", "/c/"+o.Token+"/pay")
	if p.code != 303 || p.h.Get("Location") != "/c/"+o.Token {
		t.Fatalf("pay: %d %q", p.code, p.h.Get("Location"))
	}
	if len(e.invoices(o)) != 1 || e.oc.made.Load() != 1 {
		t.Error("an invoice or an address is missing")
	}
}

// Hung steps end at the press budget, which is under the 15 s WriteTimeout, and the answer is busy.
func TestPayBudgetBoundsTheStepsAndIsUnderWriteTimeout(t *testing.T) {
	if defaultPayBudget+time.Second > 15*time.Second || defaultPayBudget < onchain.PressWait {
		t.Fatalf("budget %v: must hold the 10 s address wait and stay under the 15 s WriteTimeout", defaultPayBudget)
	}
	e := newEnv(t)
	e.oc.delay = time.Hour
	e.ln = &countLn{Lightning: slowLn{e.ln.Lightning, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}}
	e.build(e.oc)
	e.h.(*handler).payBudget = 200 * time.Millisecond
	o := e.orderBoth(21_000, sat21, time.Hour)
	start := time.Now()
	p := e.do("POST", "/c/"+o.Token+"/pay")
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %v", took)
	}
	if p.code != 303 || p.h.Get("Location") != "/c/"+o.Token+"?busy=1" {
		t.Errorf("pay: %d %q", p.code, p.h.Get("Location"))
	}
}

func TestAllRoutesCarryHeadersForOnchainOrders(t *testing.T) {
	e := newEnv(t)
	o := e.orderBoth(21_000, sat21, time.Hour)
	for _, c := range []struct{ m, p string }{
		{"GET", "/c/" + o.Token}, {"POST", "/c/" + o.Token + "/pay"}, {"GET", "/c/" + o.Token + "/qr.svg"}, {"GET", "/static/style.css"},
		{"GET", "/c/" + o.Token}, {"GET", "/c/" + o.Token + "/qr.svg"}, {"GET", "/nope"},
	} {
		checkHeaders(t, c.m+" "+c.p, e.do(c.m, c.p))
	}
}

// Past the quote's expiry the address is gone from the page and the QR; an open invoice stays, alone.
func TestExpiredQuoteDropsTheAddress(t *testing.T) {
	inv := "lnbcrt1q0w8abc"
	so := stubOrder(21_000, sat21, inv)
	so.Order.ExpiresAt = epoch.Add(-time.Minute) // the invoice (until +20 min) is still open
	h := stubHandler(so, io.Discard)
	r := getStub(h, so, "")
	checkPlain(t, r.body)
	if strings.Contains(r.body, "Bitcoin address") || strings.Contains(r.body, "bitcoin:") || strings.Contains(r.body, stubAddr) ||
		!strings.Contains(r.body, `id="invoice"`) {
		t.Errorf("page:\n%s", r.body)
	}
	if hf, ok := href(t, r.body); !ok || hf != "lightning:LNBCRT1Q0W8ABC" {
		t.Errorf("link %q", hf)
	}
	q := getStub(h, so, "/qr.svg")
	want, _ := qr.SVG("LIGHTNING:LNBCRT1Q0W8ABC")
	if q.code != 200 || q.body != string(want) {
		t.Errorf("qr.svg %d: not the invoice-only QR", q.code)
	}
	// Past expiry, no invoice: nothing to show and no QR.
	so = stubOrder(21_000, sat21, "")
	so.Order.ExpiresAt = epoch.Add(-time.Minute)
	if q := getStub(stubHandler(so, io.Discard), so, "/qr.svg"); q.code != 404 {
		t.Errorf("qr.svg of an expired quote without an invoice: %d", q.code)
	}
}

// Ruling Y2: once on-chain sats were seen, Lightning is no longer offered and the URI asks for what is missing.
func TestSeenStopsLightning(t *testing.T) {
	inv := "lnbcrt1q0w8abc"
	for _, c := range []struct {
		name     string
		received int64
		uri      string // "" = no QR and no link
		note     string
	}{
		{"seen 0 of N", sat21, "", ""},
		{"partial", 5_000, "bitcoin:" + strings.ToUpper(stubAddr) + "?amount=0.00016", "Send exactly 0.00016 BTC."},
		{"over", sat21 + 1, "", ""},
	} {
		so := stubOrder(21_000, sat21, inv)
		so.Onchain.State, so.Onchain.ReceivedSat, so.Onchain.Confirmations, so.Onchain.RequiredConfirmations = onchain.RailSeen, c.received, 0, 3
		h := stubHandler(so, io.Discard)
		r := getStub(h, so, "")
		checkPlain(t, r.body)
		for _, bad := range []string{`id="invoice"`, "Lightning invoice", "min left", "Waiting for payment", "lightning=", "New Lightning invoice", "<form"} {
			if strings.Contains(r.body, bad) {
				t.Errorf("%s: unexpected %q:\n%s", c.name, bad, r.body)
			}
		}
		for _, want := range []string{"Payment seen", "Seen, 0 of 3 confirmations", `id="address"`} {
			if !strings.Contains(r.body, want) {
				t.Errorf("%s: missing %q:\n%s", c.name, want, r.body)
			}
		}
		hf, ok := href(t, r.body)
		q := getStub(h, so, "/qr.svg")
		if c.uri == "" {
			if ok || imgRe.MatchString(r.body) || q.code != 404 || strings.Contains(r.body, "Send exactly") {
				t.Errorf("%s: a QR, link or amount with nothing missing:\n%s", c.name, r.body)
			}
			continue
		}
		if !ok || hf != c.uri || !strings.Contains(r.body, c.note) || !imgRe.MatchString(r.body) {
			t.Errorf("%s: link %q (%v), want %q, note %q:\n%s", c.name, hf, ok, c.uri, c.note, r.body)
		}
		if want, _ := qr.SVG(c.uri); q.code != 200 || q.body != string(want) {
			t.Errorf("%s: qr.svg is not the QR of the remaining-amount URI", c.name)
		}
		if !strings.Contains(r.body, "5,000 sats seen so far") {
			t.Errorf("%s: no sats seen", c.name)
		}
	}
}

// A 0 tier: a seen, unpaid order (here a part of the amount) reads "Seen", with no counts, on both page branches
// that show the line: while the address is shown, and past the expiry with nothing open.
func TestSeenOnAZeroTierHasNoCounts(t *testing.T) {
	for _, c := range []struct {
		name    string
		expired bool
	}{{"open", false}, {"past the expiry", true}} {
		so := stubOrder(0, sat21, "")
		so.Onchain.State, so.Onchain.ReceivedSat, so.Onchain.Confirmations, so.Onchain.RequiredConfirmations = onchain.RailSeen, 5_000, 0, 0
		if c.expired {
			so.Order.ExpiresAt = epoch.Add(-time.Minute)
		}
		r := getStub(stubHandler(so, io.Discard), so, "")
		checkPlain(t, r.body)
		if !strings.Contains(r.body, `<p class="state open">Seen</p>`) || strings.Contains(r.body, "confirmations") || !strings.Contains(r.body, "5,000 sats seen so far") {
			t.Errorf("%s:\n%s", c.name, r.body)
		}
		if c.expired == strings.Contains(r.body, `id="address"`) {
			t.Errorf("%s: not the branch this case is for:\n%s", c.name, r.body)
		}
		// A tier above 0 keeps the counts on the same branch.
		so.Onchain.RequiredConfirmations = 1
		if r := getStub(stubHandler(so, io.Discard), so, ""); !strings.Contains(r.body, "Seen, 0 of 1 confirmations") {
			t.Errorf("%s, tier 1:\n%s", c.name, r.body)
		}
	}
}
