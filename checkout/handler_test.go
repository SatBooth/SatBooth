package checkout

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/monero"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/qr"
	"github.com/satbooth/satbooth/rail"
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

// wallet is a minimal lnpay.Wallet: invoices stay pending until settleAll.
type wallet struct {
	mu    sync.Mutex
	seq   int
	invs  map[string]*nwc.Transaction
	ch    chan nwc.Notification
	err   error  // returned by MakeInvoice when set
	block func() // run at the start of MakeInvoice when set (it may block)
	makes atomic.Int64
}

func (w *wallet) Notifications() <-chan nwc.Notification { return w.ch }

func (w *wallet) MakeInvoice(_ context.Context, p nwc.MakeInvoiceParams) (*nwc.Transaction, error) {
	w.makes.Add(1)
	if w.block != nil {
		w.block()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return nil, w.err
	}
	w.seq++
	sum := sha256.Sum256([]byte(fmt.Sprintf("invoice %d", w.seq)))
	h := hex.EncodeToString(sum[:])
	// A valid regtest invoice for the amount of a realistic length (about 300 characters), lowercase, not derived from
	// the payment hash text.
	inv := refundtest.InvoiceLong(approval.Regtest, w.seq, strconv.FormatInt(p.AmountMsat*10, 10)+"p", 290)
	tx := &nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StatePending, Invoice: inv,
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

// counting counts the order lookups of the handler.
type counting struct {
	resolver
	lookups atomic.Int64
}

func (c *counting) ByToken(ctx context.Context, t string) (status.Order, error) {
	c.lookups.Add(1)
	return c.resolver.ByToken(ctx, t)
}

// countLn counts the Pay calls to the Lightning service.
type countLn struct {
	status.Lightning
	pays atomic.Int64
}

func (c *countLn) Pay(ctx context.Context, t string) (store.Invoice, error) {
	c.pays.Add(1)
	return c.Lightning.Pay(ctx, t)
}

type env struct {
	t    testing.TB
	clk  *clock
	st   *store.Store
	svc  *lnpay.Service
	w    *wallet
	cnt  *counting
	ln   *countLn
	oc   *fakeOc
	xm   *fakeXm
	logs *bytes.Buffer
	h    http.Handler
	refs int
}

func newEnv(t *testing.T) *env { return newEnvF(t) }

func newEnvF(t testing.TB) *env {
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
	e := &env{t: t, clk: clk, st: st, svc: svc, w: w, ln: &countLn{Lightning: svc}, oc: &fakeOc{st: st}, xm: &fakeXm{st: st},
		logs: &bytes.Buffer{}}
	e.build(e.oc)
	return e
}

// build makes the handler over a resolver whose on-chain rail is oc (nil: not configured), with the environment's
// Monero rail.
func (e *env) build(oc status.Onchain) { e.buildRails(oc, e.xm) }

// buildRails makes the handler over a resolver whose on-chain rail is oc and Monero rail xm (nil: not configured).
func (e *env) buildRails(oc status.Onchain, xm *fakeXm) {
	res := &status.Resolver{St: e.st, Ln: e.ln, Now: e.clk.Now}
	if oc != nil { // never a typed nil in the interface
		res.Oc = oc
	}
	if xm != nil {
		res.Xm = xm
	}
	e.cnt = &counting{resolver: res}
	e.h = newHandler(e.cnt, e.ln, res.Oc, res.Xm, e.clk.Now, slog.New(slog.NewJSONHandler(e.logs, nil)))
}

func (e *env) order(msat int64, expiresIn time.Duration) store.Order {
	return e.orderBoth(msat, 0, expiresIn)
}

func (e *env) orderBoth(msat, sat int64, expiresIn time.Duration) store.Order {
	e.t.Helper()
	e.refs++
	o, _, err := e.st.CreateOrder(context.Background(), store.Order{AccountID: "acct",
		MerchantRef: fmt.Sprintf("ref-%d", e.refs), AmountMsat: msat, AmountSat: sat, ExpiresAt: e.clk.Now().Add(expiresIn)})
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

type resp struct {
	code int
	body string
	h    http.Header
}

func (e *env) do(method, target string) resp {
	e.t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Body.String(), rec.Header()}
}

// settle pays the order's invoices at the wallet and runs the reconciler until the page says Paid.
func (e *env) settle(token string) {
	e.t.Helper()
	e.w.settleAll()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(e.get("/c/"+token).body, "Paid") {
		if time.Now().After(deadline) {
			e.t.Fatal("order never became paid")
		}
		time.Sleep(time.Millisecond)
	}
}

func qrSVG(bolt11 string) (string, error) {
	b, err := qr.SVG("LIGHTNING:" + strings.ToUpper(bolt11))
	return string(b), err
}

func (e *env) get(target string) resp { return e.do("GET", target) }

var securityHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'none'; img-src 'self'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
	"X-Content-Type-Options":  "nosniff",
	"Referrer-Policy":         "no-referrer",
}

func checkHeaders(t *testing.T, what string, r resp) {
	t.Helper()
	for k, v := range securityHeaders {
		if got := r.h.Get(k); got != v {
			t.Errorf("%s: header %s = %q, want %q", what, k, got, v)
		}
	}
}

// wellFormed parses the page with a strict XML decoder: the template is XHTML-compatible.
func wellFormed(t *testing.T, body string) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(body))
	d.Strict = true
	for {
		if _, err := d.Token(); err != nil {
			if err == io.EOF {
				return
			}
			t.Fatalf("page is not well-formed: %v\n%s", err, body)
		}
	}
}

var forbidden = regexp.MustCompile(`(?i)\sstyle\s*=|\son[a-z]+\s*=|<script|<iframe|<link[^>]*(href="https?:)|src="https?:|@import`)

func checkPlain(t *testing.T, body string) {
	t.Helper()
	wellFormed(t, body)
	if forbidden.MatchString(body) {
		t.Errorf("page has inline style/script/handler or an external reference:\n%s", body)
	}
}

func TestNewOrderShowsPayForm(t *testing.T) {
	e := newEnv(t)
	o := e.order(21_000, time.Hour)
	r := e.get("/c/" + o.Token)
	if r.code != 200 {
		t.Fatalf("code %d", r.code)
	}
	checkHeaders(t, "page", r)
	checkPlain(t, r.body)
	for _, want := range []string{"SatBooth", "21 sats", `<form method="post" action="/c/` + o.Token + `/pay">`, "Pay with Lightning"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q in:\n%s", want, r.body)
		}
	}
	for _, bad := range []string{`http-equiv="refresh"`, "qr.svg", "Paid", "Expired", "Busy"} {
		if strings.Contains(r.body, bad) {
			t.Errorf("unexpected %q", bad)
		}
	}
	if ct := r.h.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content type %q", ct)
	}
}

func TestPayThenOpenInvoice(t *testing.T) {
	e := newEnv(t)
	o := e.order(21_000, time.Hour)
	p := e.do("POST", "/c/"+o.Token+"/pay")
	if p.code != 303 || p.h.Get("Location") != "/c/"+o.Token {
		t.Fatalf("pay: %d %q", p.code, p.h.Get("Location"))
	}
	checkHeaders(t, "pay", p)
	r := e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	invs, _ := e.st.InvoicesForOrder(context.Background(), o.ID)
	if len(invs) != 1 {
		t.Fatalf("%d invoices", len(invs))
	}
	for _, want := range []string{`<meta http-equiv="refresh" content="5; url=/c/` + o.Token + `" />`, `<img class="qr" src="/c/` + o.Token + `/qr.svg"`,
		`readonly="readonly" value="` + invs[0].Bolt11 + `"`, "About 20 min left"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q in:\n%s", want, r.body)
		}
	}
	if strings.Contains(r.body, "Pay with Lightning") {
		t.Error("pay button shown with an open invoice")
	}
	e.clk.Advance(5*time.Minute + time.Second)
	if r := e.get("/c/" + o.Token); !strings.Contains(r.body, "About 15 min left") {
		t.Errorf("minutes: %s", r.body)
	}
}

func TestPayTwiceOneInvoice(t *testing.T) {
	e := newEnv(t)
	o := e.order(5_000, time.Hour)
	for i := 0; i < 2; i++ {
		if p := e.do("POST", "/c/"+o.Token+"/pay"); p.code != 303 || strings.Contains(p.h.Get("Location"), "busy") {
			t.Fatalf("pay %d: %d %q", i, p.code, p.h.Get("Location"))
		}
	}
	invs, _ := e.st.InvoicesForOrder(context.Background(), o.ID)
	if len(invs) != 1 || e.w.makes.Load() != 1 {
		t.Fatalf("%d invoices, %d make calls", len(invs), e.w.makes.Load())
	}
}

func TestPaidAndExpired(t *testing.T) {
	e := newEnv(t)
	o := e.order(7_000, time.Hour)
	e.do("POST", "/c/"+o.Token+"/pay")
	e.settle(o.Token)
	r := e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	if !strings.Contains(r.body, "Paid") || strings.Contains(r.body, `http-equiv="refresh"`) || strings.Contains(r.body, "<form") {
		t.Errorf("paid page:\n%s", r.body)
	}
	if q := e.get("/c/" + o.Token + "/qr.svg"); q.code != 404 {
		t.Errorf("qr of a paid order: %d", q.code)
	}

	o2 := e.order(7_000, 10*time.Minute) // Lightning only: lnpay.Status's rule, no on-chain event needed
	e.clk.Advance(11 * time.Minute)
	r = e.get("/c/" + o2.Token)
	checkPlain(t, r.body)
	if r.code != 200 || !strings.Contains(r.body, "Expired") || strings.Contains(r.body, `http-equiv="refresh"`) || strings.Contains(r.body, "<form") {
		t.Errorf("expired page (%d):\n%s", r.code, r.body)
	}
	if p := e.do("POST", "/c/"+o2.Token+"/pay"); p.code != 303 || p.h.Get("Location") != "/c/"+o2.Token {
		t.Errorf("pay on an expired order: %d %q", p.code, p.h.Get("Location"))
	}
}

// An invoice paid after the closing lookup (the wallet still accepts it for a while after its
// expiry): the page read Expired, and then reads Paid.
func TestPaidAfterExpired(t *testing.T) {
	e := newEnv(t)
	o := e.order(7_000, 10*time.Minute)
	e.do("POST", "/c/"+o.Token+"/pay")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor := func(what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(e.get("/c/"+o.Token).body, ">"+what+"<") {
			if time.Now().After(deadline) {
				t.Fatalf("the page never read %s:\n%s", what, e.get("/c/"+o.Token).body)
			}
			time.Sleep(time.Millisecond)
		}
	}
	e.clk.Advance(25 * time.Minute) // past the invoice's expiry plus the grace, and the order's expiry
	waitFor("Expired")
	invs, _ := e.st.InvoicesForOrder(context.Background(), o.ID)
	e.clk.Advance(time.Minute)
	e.w.settleAll()
	e.w.ch <- nwc.Notification{Type: nwc.NotifPaymentReceived, Tx: nwc.Transaction{PaymentHash: invs[0].PaymentHash}}
	waitFor("Paid")
	if r := e.get("/c/" + o.Token); strings.Contains(r.body, "<form") || strings.Contains(r.body, `http-equiv="refresh"`) {
		t.Errorf("paid page:\n%s", r.body)
	}
}

// An order past its expiry whose invoice is expired but not closed yet reads new: no button.
func TestPastExpiryNotClosedChecking(t *testing.T) {
	e := newEnv(t)
	o := e.order(7_000, 10*time.Minute)
	e.do("POST", "/c/"+o.Token+"/pay")
	e.clk.Advance(25 * time.Minute) // the invoice (20 min) and the order are past their expiry
	r := e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	if r.code != 200 || !strings.Contains(r.body, "Checking payment") || !strings.Contains(r.body, `http-equiv="refresh"`) ||
		strings.Contains(r.body, "<form") || strings.Contains(r.body, "Expired") {
		t.Errorf("checking page (%d):\n%s", r.code, r.body)
	}
	if q := e.get("/c/" + o.Token + "/qr.svg"); q.code != 404 {
		t.Errorf("qr of an expired invoice: %d", q.code)
	}
}

func TestBusy(t *testing.T) {
	e := newEnv(t)
	o := e.order(5_000, time.Hour)
	e.w.err = errors.New("wallet exploded") // not in the never-sent list: outcome unknown
	p := e.do("POST", "/c/"+o.Token+"/pay")
	if p.code != 303 || p.h.Get("Location") != "/c/"+o.Token+"?busy=1" {
		t.Fatalf("pay: %d %q", p.code, p.h.Get("Location"))
	}
	r := e.get(p.h.Get("Location"))
	checkPlain(t, r.body)
	if !strings.Contains(r.body, "Busy, try again.") || !strings.Contains(r.body, "Pay with Lightning") {
		t.Errorf("busy page:\n%s", r.body)
	}
	for _, q := range []string{"?busy=2", "?busy=", "?busy=1%00", "?busy=<script>", "?x=Busy"} {
		if r := e.get("/c/" + o.Token + q); strings.Contains(r.body, "Busy") || strings.Contains(r.body, "script") {
			t.Errorf("query %q changed the page", q)
		}
	}
}

// The refresh of a busy page goes to the plain URL, so "Busy, try again" does not come back every 5 s.
func TestBusyRefreshDropsTheQuery(t *testing.T) {
	e := newEnv(t)
	o := e.order(5_000, time.Hour)
	if p := e.do("POST", "/c/"+o.Token+"/pay"); p.code != 303 {
		t.Fatalf("pay: %d", p.code)
	}
	r := e.get("/c/" + o.Token + "?busy=1") // an issued page after a busy press
	if !strings.Contains(r.body, "Busy, try again.") {
		t.Fatalf("not a busy page:\n%s", r.body)
	}
	want := `<meta http-equiv="refresh" content="5; url=/c/` + o.Token + `" />`
	if !strings.Contains(r.body, want) {
		t.Fatalf("missing %q in:\n%s", want, r.body)
	}
	if next := e.get("/c/" + o.Token); strings.Contains(next.body, "Busy") || !strings.Contains(next.body, want) {
		t.Errorf("the refreshed page:\n%s", next.body)
	}
}

// A press whose request context is done (the browser aborted it, or a deadline passed) is busy, not a
// 500, whether it ends before the order is read or while the press waits on the flight.
func TestCancelledPressIsBusy(t *testing.T) {
	e := newEnv(t)
	o := e.order(5_000, time.Hour)
	for name, mk := range map[string]func() (context.Context, context.CancelFunc){
		"cancelled": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		},
		"deadline": func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		},
	} {
		ctx, cancel := mk()
		req := httptest.NewRequest("POST", "/c/"+o.Token+"/pay", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		cancel()
		if rec.Code != 303 || rec.Header().Get("Location") != "/c/"+o.Token+"?busy=1" {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Header().Get("Location"))
		}
	}
	// While it waits on a flight held by the wallet.
	entered, release := make(chan struct{}), make(chan struct{})
	e.w.block = func() { close(entered); <-release }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, httptest.NewRequest("POST", "/c/"+o.Token+"/pay", nil).WithContext(ctx))
		done <- rec
	}()
	<-entered
	cancel()
	rec := <-done
	close(release)
	if rec.Code != 303 || rec.Header().Get("Location") != "/c/"+o.Token+"?busy=1" {
		t.Errorf("waiting: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// An account at its cap of open invoices (5 by default): the next order's press is busy, and its page
// says "Busy, try again." with the pay button.
func TestBusyAtAccountCap(t *testing.T) {
	e := newEnv(t) // every order is the account "acct"
	for i := 0; i < limits.DefaultMaxOpenPerAccount; i++ {
		o := e.order(5_000, time.Hour)
		if p := e.do("POST", "/c/"+o.Token+"/pay"); p.code != 303 || strings.Contains(p.h.Get("Location"), "busy") {
			t.Fatalf("press %d: %d %q", i, p.code, p.h.Get("Location"))
		}
	}
	o := e.order(5_000, time.Hour)
	if _, err := e.svc.Pay(context.Background(), o.Token); !errors.Is(err, limits.ErrAccountCap) {
		t.Fatalf("Pay at the cap: %v, want ErrAccountCap", err)
	}
	p := e.do("POST", "/c/"+o.Token+"/pay")
	if p.code != 303 || p.h.Get("Location") != "/c/"+o.Token+"?busy=1" {
		t.Fatalf("pay at the cap: %d %q", p.code, p.h.Get("Location"))
	}
	r := e.get(p.h.Get("Location"))
	if !strings.Contains(r.body, "Busy, try again.") || !strings.Contains(r.body, "Pay with Lightning") {
		t.Errorf("busy page:\n%s", r.body)
	}
}

func TestBusyOnLimitRefusal(t *testing.T) {
	for _, err := range []error{limits.ErrRate, limits.ErrOpenCap, limits.ErrClassShare, limits.ErrDailyCount, limits.ErrDailyAmount,
		limits.ErrAmount, limits.ErrAccountCap, fmt.Errorf("wrapped: %w", limits.ErrAccountCap), lnpay.ErrNotCreated, lnpay.ErrUnknown, fmt.Errorf("wrapped: %w", limits.ErrRate),
		context.Canceled, context.DeadlineExceeded, fmt.Errorf("%w: %w", lnpay.ErrUnknown, context.Canceled)} {
		if !busy(err) {
			t.Errorf("%v is not busy", err)
		}
	}
	for _, err := range []error{errors.New("x"), lnpay.ErrPaid, store.ErrNotFound, nil} {
		if busy(err) {
			t.Errorf("%v is busy", err)
		}
	}
}

func TestQR(t *testing.T) {
	e := newEnv(t)
	o := e.order(21_000, time.Hour)
	if q := e.get("/c/" + o.Token + "/qr.svg"); q.code != 404 {
		t.Fatalf("qr without an invoice: %d", q.code)
	}
	e.do("POST", "/c/"+o.Token+"/pay")
	q := e.get("/c/" + o.Token + "/qr.svg")
	if q.code != 200 {
		t.Fatalf("qr: %d", q.code)
	}
	checkHeaders(t, "qr", q)
	// no-store, like the page: the URL is the same for every invoice of the order, so a cached image
	// could show an expired invoice after a new one was made.
	if q.h.Get("Content-Type") != "image/svg+xml" || q.h.Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", q.h)
	}
	wellFormed(t, q.body)
	if !strings.HasPrefix(q.body, "<svg ") || strings.Contains(q.body, "<script") {
		t.Errorf("svg: %.100s", q.body)
	}
	invs, _ := e.st.InvoicesForOrder(context.Background(), o.ID)
	want, err := qrSVG(invs[0].Bolt11)
	if err != nil || q.body != want {
		t.Errorf("qr is not the QR of LIGHTNING:+upper(bolt11): %v", err)
	}
	// The page says nothing about the QR of another order's invoice.
	o2 := e.order(21_000, time.Hour)
	if e.get("/c/"+o2.Token+"/qr.svg").code != 404 {
		t.Error("qr of an order without an invoice")
	}
}

func TestStatic(t *testing.T) {
	e := newEnv(t)
	r := e.get("/static/style.css")
	if r.code != 200 || r.h.Get("Content-Type") != "text/css; charset=utf-8" || r.body != string(styleCSS) {
		t.Fatalf("css: %d %q", r.code, r.h.Get("Content-Type"))
	}
	checkHeaders(t, "css", r)
	if strings.Contains(r.body, "url(") || strings.Contains(r.body, "@import") || strings.Contains(r.body, "@font-face") {
		t.Error("stylesheet loads something")
	}
	for _, p := range []string{"/static/", "/static/other.css", "/static/style.css/x", "/static/../c/x"} {
		if c := e.get(p).code; c != 404 {
			t.Errorf("%s: %d", p, c)
		}
	}
}

func TestRefreshOnlyWhileOpen(t *testing.T) {
	e := newEnv(t)
	o := e.order(5_000, time.Hour)
	has := func() bool { return strings.Contains(e.get("/c/"+o.Token).body, `http-equiv="refresh"`) }
	if has() {
		t.Fatal("refresh on a new order")
	}
	e.do("POST", "/c/"+o.Token+"/pay")
	if !has() {
		t.Fatal("no refresh with an open invoice")
	}
	e.settle(o.Token)
	if has() {
		t.Fatal("refresh on a paid order")
	}
}

// Every bad path gives the same 404 (body and headers), and a malformed token does no lookup.
func TestCraftedPaths(t *testing.T) {
	e := newEnv(t)
	o := e.order(5_000, time.Hour)
	e.do("POST", "/c/"+o.Token+"/pay")
	other := strings.Repeat("0", 32)
	long := strings.Repeat("a", 5000)
	malformed := []string{
		"/c/", "/c", "/c/x", "/c/" + o.Token[:31], "/c/" + o.Token + "0", "/c/" + strings.ToUpper(o.Token), "/c/" + long,
		"/c/" + strings.Repeat("a", 33), "/c/%2e%2e", "/c/..", "/c/../" + o.Token, "/c/" + o.Token + "/..", "/c/./" + o.Token,
		"/c/%2F" + o.Token, "/c/" + o.Token[:16] + "%2F" + o.Token[16:], "/c/" + o.Token + "%2F", "/c//" + o.Token,
		"/c/" + o.Token + "/", "/c/%20" + o.Token[1:], "/c/" + o.Token[:31] + "%00", "/c/" + o.Token[:31] + "%e2%82%ac",
		"/c/" + o.Token + "/qr.svg/x", "/c/" + o.Token + "/qr", "/", "/x", "/c/" + o.Token + "/PAY", "/%2e%2e/c/" + o.Token,
		"/c/%2e%2e%2f" + o.Token, "/C/" + o.Token, "/static", "/static//style.css",
	}
	wantCode := 404
	var wantBody string
	var wantHdr http.Header
	check := func(what string, r resp) {
		t.Helper()
		if r.code != wantCode {
			t.Errorf("%s: code %d", what, r.code)
		}
		checkHeaders(t, what, r)
		if wantHdr == nil {
			wantBody, wantHdr = r.body, r.h
			return
		}
		if r.body != wantBody {
			t.Errorf("%s: body differs: %q", what, r.body)
		}
		if fmt.Sprint(r.h) != fmt.Sprint(wantHdr) {
			t.Errorf("%s: headers differ: %v vs %v", what, r.h, wantHdr)
		}
	}
	// The unknown token comes first: its response is the reference.
	for _, suffix := range []string{"", "/qr.svg"} {
		check("unknown"+suffix, e.get("/c/"+other+suffix))
	}
	if r := e.do("POST", "/c/"+other+"/pay"); r.code != 404 || r.body != wantBody || fmt.Sprint(r.h) != fmt.Sprint(wantHdr) {
		t.Errorf("pay with an unknown token: %d %q %v", r.code, r.body, r.h)
	}
	before := e.cnt.lookups.Load()
	for _, p := range malformed {
		for _, m := range []string{"GET", "POST"} {
			u, err := url.ParseRequestURI(p)
			if err != nil {
				continue
			}
			req := httptest.NewRequest(m, "/", nil)
			req.URL = u
			req.RequestURI = p
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			check(m+" "+p, resp{rec.Code, rec.Body.String(), rec.Header()})
		}
	}
	// The unknown tokens above are lookups; the malformed ones are not.
	if got := e.cnt.lookups.Load() - before; got != 0 {
		t.Errorf("%d store lookups for malformed paths", got)
	}
	if got := e.ln.pays.Load(); got != 1 { // the setup pay; the unknown token's press stops at the order lookup
		t.Errorf("pays: %d, want 1", got)
	}
}

func TestMethodsAndHeadersEverywhere(t *testing.T) {
	e := newEnv(t)
	o := e.order(5_000, time.Hour)
	cases := []struct{ m, p string }{
		{"GET", "/c/" + o.Token + "/pay"}, {"POST", "/c/" + o.Token}, {"PUT", "/c/" + o.Token}, {"DELETE", "/c/" + o.Token},
		{"POST", "/c/" + o.Token + "/qr.svg"}, {"POST", "/static/style.css"}, {"OPTIONS", "/c/" + o.Token}, {"PATCH", "/"},
	}
	for _, c := range cases {
		r := e.do(c.m, c.p)
		checkHeaders(t, c.m+" "+c.p, r)
		if r.code != 404 { // the catch-all pattern matches every method, so the mux never answers 405
			t.Errorf("%s %s: %d", c.m, c.p, r.code)
		}
		if r.code == 303 || r.h.Get("Location") != "" {
			t.Errorf("%s %s: redirect", c.m, c.p)
		}
	}
	if e.ln.pays.Load() != 0 {
		t.Error("a non-POST reached Pay")
	}
}

func TestNoSecretsOnPage(t *testing.T) {
	e := newEnv(t)
	o := e.order(5_000, time.Hour)
	e.do("POST", "/c/"+o.Token+"/pay")
	r := e.get("/c/" + o.Token)
	invs, _ := e.st.InvoicesForOrder(context.Background(), o.ID)
	for _, secret := range []string{o.ID, o.AccountID, o.MerchantRef, invs[0].PaymentHash, invs[0].OrderID} {
		if strings.Contains(r.body, secret) {
			t.Errorf("page contains %q", secret)
		}
	}
}

func TestNoPayBodyNeeded(t *testing.T) {
	e := newEnv(t)
	o := e.order(5_000, time.Hour)
	req := httptest.NewRequest("POST", "/c/"+o.Token+"/pay", bytes.NewReader(bytes.Repeat([]byte("a"), 1<<20)))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 303 {
		t.Errorf("code %d", rec.Code)
	}
}

// quoted creates a priced order of 12.50 EUR (18,120 sats on Lightning) whose quote has the given sources.
func (e *env) quoted(expiresIn time.Duration, providers ...string) store.Order {
	e.t.Helper()
	e.refs++
	r := store.AssetRate{Asset: "BTC", Rate: "70000"}
	for _, p := range providers {
		r.Sources = append(r.Sources, store.QuoteSource{Provider: p, Rate: "70000", At: e.clk.Now()})
	}
	q := store.Quote{Price: "12.50", Currency: "EUR", MarginBps: 150, QuotedAt: e.clk.Now(), Rates: []store.AssetRate{r}}
	o, _, err := e.st.CreateQuotedOrder(context.Background(), store.Order{AccountID: "acct", MerchantRef: fmt.Sprintf("ref-%d", e.refs),
		AmountMsat: 18_120_000, ExpiresAt: e.clk.Now().Add(expiresIn)}, q)
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

// priceBeforeSats asserts the fiat price line right before the amounts line.
func priceBeforeSats(t *testing.T, what, body string) {
	t.Helper()
	checkPlain(t, body)
	const line = `<p class="amount">12.50 EUR</p>` + "\n" + `<p class="amount">18,120 sats</p>`
	if !strings.Contains(body, line) {
		t.Errorf("%s: no price line before the amounts:\n%s", what, body)
	}
}

// A priced order's page says what was priced, on every state; an explicit order's page has no currency line.
func TestFiatPriceShown(t *testing.T) {
	e := newEnv(t)
	o := e.quoted(time.Hour, "kraken", "coinbase")
	priceBeforeSats(t, "new", e.get("/c/"+o.Token).body)
	e.do("POST", "/c/"+o.Token+"/pay")
	r := e.get("/c/" + o.Token)
	if !strings.Contains(r.body, "lnbcrt") {
		t.Fatalf("no open invoice:\n%s", r.body)
	}
	priceBeforeSats(t, "open", r.body)
	e.settle(o.Token)
	r = e.get("/c/" + o.Token)
	if !strings.Contains(r.body, ">Paid<") {
		t.Fatalf("not paid:\n%s", r.body)
	}
	priceBeforeSats(t, "paid", r.body)

	o2 := e.quoted(10*time.Minute, "kraken", "coinbase")
	e.clk.Advance(11 * time.Minute)
	r = e.get("/c/" + o2.Token)
	if !strings.Contains(r.body, ">Expired<") {
		t.Fatalf("not expired:\n%s", r.body)
	}
	priceBeforeSats(t, "expired", r.body)

	ex := e.order(18_120_000, time.Hour)
	r = e.get("/c/" + ex.Token)
	checkPlain(t, r.body)
	if strings.Count(r.body, `class="amount"`) != 1 || strings.Contains(r.body, "EUR") || strings.Contains(r.body, "yadio") {
		t.Errorf("explicit order's page:\n%s", r.body)
	}
}

// Yadio asks to be cited with a link where its data is shown (threat model §10 threat 7):
// the page links it only when Yadio was one of the order's quote sources.
func TestYadioCredit(t *testing.T) {
	e := newEnv(t)
	const credit = `<p class="note">Rates: <a href="https://yadio.io">yadio.io</a></p>`
	for _, c := range []struct {
		providers []string
		want      bool
	}{
		{[]string{"coinbase", "kraken", "yadio"}, true}, {[]string{"yadio", "kraken"}, true},
		{[]string{"coinbase", "kraken"}, false}, {[]string{"custom", "kraken"}, false}, {[]string{"yadi", "kraken"}, false},
	} {
		o := e.quoted(time.Hour, c.providers...)
		r := e.get("/c/" + o.Token)
		checkPlain(t, r.body)
		if strings.Contains(r.body, credit) != c.want || !c.want && strings.Contains(r.body, "yadio") {
			t.Errorf("%v: credit %v, want %v:\n%s", c.providers, strings.Contains(r.body, credit), c.want, r.body)
		}
		if strings.Count(r.body, "yadio.io") > 2 {
			t.Errorf("%v: more than one credit", c.providers)
		}
	}
	// A Yadio source of the XMR rate alone credits it too.
	e.refs++
	q := store.Quote{Price: "12.50", Currency: "EUR", MarginBps: 150, QuotedAt: e.clk.Now(), Rates: []store.AssetRate{
		{Asset: "BTC", Rate: "70000", Sources: []store.QuoteSource{{Provider: "kraken", Rate: "70000", At: e.clk.Now()}}},
		{Asset: "XMR", Rate: "150", Sources: []store.QuoteSource{{Provider: "yadio", Rate: "150", At: e.clk.Now()}}}}}
	xo, _, err := e.st.CreateQuotedOrder(context.Background(), store.Order{AccountID: "acct", MerchantRef: fmt.Sprintf("ref-%d", e.refs),
		AmountMsat: 18_120_000, AmountPiconero: 84_000_000_000, ExpiresAt: e.clk.Now().Add(time.Hour)}, q)
	if err != nil {
		t.Fatal(err)
	}
	if r := e.get("/c/" + xo.Token); !strings.Contains(r.body, credit) || strings.Count(r.body, "yadio.io") > 2 {
		t.Errorf("a Yadio XMR rate without the credit:\n%s", r.body)
	}
	// On a paid page too.
	o := e.quoted(time.Hour, "kraken", "yadio")
	e.do("POST", "/c/"+o.Token+"/pay")
	e.settle(o.Token)
	if r := e.get("/c/" + o.Token); !strings.Contains(r.body, credit) {
		t.Errorf("paid page without the credit:\n%s", r.body)
	}
}

// --- the Monero rail (plan 8) ---

// fakeXm is the Monero rail: it stores the subaddresses it makes in the real store, and answers Rail from the store
// plus whatever the test sets in tweak.
type fakeXm struct {
	st    *store.Store
	mu    sync.Mutex
	err   error // Address answers this when set
	tweak func(*rail.View)
	calls atomic.Int64
	made  atomic.Int64
}

func (f *fakeXm) Rail(ctx context.Context, o store.Order) (rail.View, error) {
	v := rail.View{State: rail.Waiting, RequiredConfirmations: 10}
	if a, err := f.st.SubaddressForOrder(ctx, o.ID); err == nil {
		v.Issued, v.Address = true, a.Address
	}
	f.mu.Lock()
	tw := f.tweak
	f.mu.Unlock()
	if tw != nil {
		tw(&v)
	}
	return v, nil
}

func (f *fakeXm) setTweak(fn func(*rail.View)) { f.mu.Lock(); f.tweak = fn; f.mu.Unlock() }

func (f *fakeXm) Address(ctx context.Context, orderID string) (store.Subaddress, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Subaddress{}, f.err
	}
	if a, err := f.st.SubaddressForOrder(ctx, orderID); err == nil {
		return a, nil
	}
	idx, _, err := f.st.MaxSubaddressIndex(ctx)
	if err != nil {
		return store.Subaddress{}, err
	}
	idx++
	a := store.Subaddress{OrderID: orderID, Address: subaddressOf(idx), Index: idx}
	if err := f.st.AddSubaddress(ctx, a); err != nil {
		return store.Subaddress{}, err
	}
	f.made.Add(1)
	return a, nil
}

// subaddressOf is a mainnet-shaped subaddress for index i: "8" and 94 base58 characters drawn from a hash of i.
func subaddressOf(i int64) string {
	b := []byte{'8'}
	for n := 0; len(b) < 95; n++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("subaddress %d %d", i, n)))
		for _, c := range sum {
			if len(b) < 95 {
				b = append(b, moneroBase58[int(c)%len(moneroBase58)])
			}
		}
	}
	return string(b)
}

const pico15 = 1_500_000_000_000 // 1.5 XMR

func (e *env) orderAll(msat, sat, pico int64, expiresIn time.Duration) store.Order {
	e.t.Helper()
	e.refs++
	o, _, err := e.st.CreateOrder(context.Background(), store.Order{AccountID: "acct", MerchantRef: fmt.Sprintf("ref-%d", e.refs),
		AmountMsat: msat, AmountSat: sat, AmountPiconero: pico, ExpiresAt: e.clk.Now().Add(expiresIn)})
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

func (e *env) subOf(o store.Order) string {
	e.t.Helper()
	a, err := e.st.SubaddressForOrder(context.Background(), o.ID)
	if err != nil {
		e.t.Fatalf("no subaddress: %v", err)
	}
	return a.Address
}

// press posts the pay form with the given body (form-encoded); "" posts no body.
func (e *env) press(token, body string) resp {
	e.t.Helper()
	req := httptest.NewRequest("POST", "/c/"+token+"/pay", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Body.String(), rec.Header()}
}

var moneroHrefRe = regexp.MustCompile(`<a href="(monero:[^"]*)">Open in wallet</a>`)

// moneroForm and bitcoinForm are the picker's two forms.
func moneroForm(token string) string {
	return `<form method="post" action="/c/` + token + `/pay">` + "\n" + `<input type="hidden" name="rail" value="monero" />` + "\n" +
		`<button type="submit">Pay with Monero</button>`
}

func bitcoinForm(token string) string {
	return `<form method="post" action="/c/` + token + `/pay">` + "\n" + `<input type="hidden" name="rail" value="bitcoin" />` + "\n" +
		`<button type="submit">Pay with bitcoin</button>`
}

// checkMonero asserts the Monero block of a page: the subaddress field, the amount note, the link and the QR, and that
// /qr.svg?rail=monero is the QR of the link.
func checkMonero(t *testing.T, e *env, o store.Order, body, xmr string) {
	t.Helper()
	checkPlain(t, body)
	addr := e.subOf(o)
	uri, err := MoneroURI(addr, mustPico(t, xmr))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<label for="monero">Monero address</label>`, `id="monero" type="text" readonly="readonly" value="` + addr + `"`,
		"Send exactly " + xmr + " XMR.", `<img class="qr" src="/c/` + o.Token + `/qr.svg?rail=monero"`, `http-equiv="refresh"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
	if m := moneroHrefRe.FindStringSubmatch(body); m == nil || m[1] != uri {
		t.Errorf("Monero link %v, want %q", m, uri)
	}
	q := e.get("/c/" + o.Token + "/qr.svg?rail=monero")
	if want, _ := qr.SVG(uri); q.code != 200 || q.body != string(want) {
		t.Errorf("qr.svg?rail=monero %d: not the QR of the Monero URI", q.code)
	}
}

func mustPico(t *testing.T, xmr string) int64 {
	t.Helper()
	n, ok := parseXMR(xmr)
	if !ok {
		t.Fatalf("bad XMR %q", xmr)
	}
	return n
}

// The rail picker: a mixed order offers bitcoin and Monero, each press makes only its own rail's address or invoice,
// and once money is seen on one family the other is no longer offered.
func TestRailPicker(t *testing.T) {
	e := newEnv(t)
	o := e.orderAll(21_000_000, sat21, pico15, time.Hour)
	r := e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	for _, want := range []string{"21,000 sats or 0.00021 BTC or 1.5 XMR", "Ready to pay", bitcoinForm(o.Token), moneroForm(o.Token)} {
		if !strings.Contains(r.body, want) {
			t.Errorf("new mixed page: missing %q:\n%s", want, r.body)
		}
	}
	if strings.Count(r.body, "<form") != 2 || strings.Contains(r.body, `id="monero"`) {
		t.Errorf("new mixed page:\n%s", r.body)
	}

	// rail=monero: only the Monero address, and the bitcoin button stays.
	p := e.press(o.Token, "rail=monero")
	if p.code != 303 || p.h.Get("Location") != "/c/"+o.Token {
		t.Fatalf("monero press: %d %q", p.code, p.h.Get("Location"))
	}
	if e.ln.pays.Load() != 0 || e.w.makes.Load() != 0 || e.oc.calls.Load() != 0 || e.xm.calls.Load() != 1 || e.xm.made.Load() != 1 {
		t.Errorf("monero press: %d pays, %d address calls, %d Monero calls", e.ln.pays.Load(), e.oc.calls.Load(), e.xm.calls.Load())
	}
	r = e.get("/c/" + o.Token)
	checkMonero(t, e, o, r.body, "1.5")
	if !strings.Contains(r.body, bitcoinForm(o.Token)) || strings.Contains(r.body, "Pay with Monero") || strings.Count(r.body, "<form") != 1 ||
		strings.Contains(r.body, `id="address"`) || strings.Contains(r.body, `id="invoice"`) || !strings.Contains(r.body, "Waiting for payment") {
		t.Errorf("after the Monero press:\n%s", r.body)
	}
	if q := e.get("/c/" + o.Token + "/qr.svg"); q.code != 404 {
		t.Errorf("the bitcoin QR with nothing bitcoin issued: %d", q.code)
	}
	// A second Monero press reuses the subaddress.
	e.press(o.Token, "rail=monero")
	if e.xm.made.Load() != 1 {
		t.Errorf("%d subaddresses made", e.xm.made.Load())
	}

	// rail=bitcoin: today's press, and the Monero stub's Address is not called.
	calls := e.xm.calls.Load()
	if p := e.press(o.Token, "rail=bitcoin"); p.code != 303 || p.h.Get("Location") != "/c/"+o.Token {
		t.Fatalf("bitcoin press: %d %q", p.code, p.h.Get("Location"))
	}
	if e.xm.calls.Load() != calls || e.oc.made.Load() != 1 || len(e.invoices(o)) != 1 {
		t.Errorf("bitcoin press: %d Monero calls, %d addresses, %d invoices", e.xm.calls.Load()-calls, e.oc.made.Load(), len(e.invoices(o)))
	}
	r = e.get("/c/" + o.Token)
	checkMonero(t, e, o, r.body, "1.5")
	addr := e.addrOf(o)
	uri, _ := PaymentURI(addr, sat21, e.invoices(o)[0].Bolt11)
	if h, ok := href(t, r.body); !ok || h != uri || !strings.Contains(r.body, `id="invoice"`) || strings.Contains(r.body, "<form") {
		t.Errorf("both rails issued: link %q\n%s", h, r.body)
	}

	// Monero money seen: the bitcoin address, invoice and button are gone; the Monero URI asks for what is missing.
	e.xm.setTweak(func(v *rail.View) { v.State, v.Received, v.Confirmations = rail.Seen, 500_000_000_000, 0 })
	r = e.get("/c/" + o.Token)
	checkMonero(t, e, o, r.body, "1")
	for _, bad := range []string{`id="address"`, `id="invoice"`, "bitcoin:", "<form", "Pay with bitcoin", "lightning"} {
		if strings.Contains(r.body, bad) {
			t.Errorf("Monero seen: unexpected %q:\n%s", bad, r.body)
		}
	}
	if !strings.Contains(r.body, "Payment seen") || !strings.Contains(r.body, "Seen, 0 of 10 confirmations") || !strings.Contains(r.body, "0.5 XMR seen so far.") {
		t.Errorf("Monero seen:\n%s", r.body)
	}
	if q := e.get("/c/" + o.Token + "/qr.svg"); q.code != 404 {
		t.Errorf("the bitcoin QR once Monero was seen: %d", q.code)
	}
	// A bitcoin press now makes nothing, and asks nothing of Monero.
	pays, calls := e.w.makes.Load(), e.xm.calls.Load()
	e.clk.Advance(25 * time.Minute) // the invoice is expired: a press would make a new one
	if p := e.press(o.Token, "rail=bitcoin"); p.code != 303 || e.w.makes.Load() != pays || e.xm.calls.Load() != calls {
		t.Errorf("a bitcoin press once Monero was seen: %d, %d makes", p.code, e.w.makes.Load()-pays)
	}

	// On-chain money seen (and no Monero money): the Monero block and button are gone.
	e.xm.setTweak(nil)
	e.oc.setTweak(func(r *onchain.Rail) { r.State, r.ReceivedSat = onchain.RailSeen, 5_000 })
	r = e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	for _, bad := range []string{`id="monero"`, "monero:", "Pay with Monero", "rail=monero", "Send exactly 1.5 XMR"} {
		if strings.Contains(r.body, bad) {
			t.Errorf("on-chain seen: unexpected %q:\n%s", bad, r.body)
		}
	}
	if !strings.Contains(r.body, `id="address"`) || !strings.Contains(r.body, "Send exactly 0.00016 BTC.") {
		t.Errorf("on-chain seen:\n%s", r.body)
	}
	if q := e.get("/c/" + o.Token + "/qr.svg?rail=monero"); q.code != 404 {
		t.Errorf("the Monero QR once on-chain money was seen: %d", q.code)
	}
	calls = e.xm.calls.Load()
	if p := e.press(o.Token, "rail=monero"); p.code != 303 || e.xm.calls.Load() != calls {
		t.Errorf("a Monero press once on-chain money was seen: %d, %d calls", p.code, e.xm.calls.Load()-calls)
	}
}

// A press names a rail the order has, once, or none: anything else is a 400 with a fixed body that makes nothing.
func TestRailPickerRefusals(t *testing.T) {
	e := newEnv(t)
	mixed := e.orderAll(21_000, sat21, pico15, time.Hour)
	btc := e.orderBoth(21_000, sat21, time.Hour)
	xmOnly := e.orderAll(0, 0, pico15, time.Hour)
	for _, c := range []struct {
		name  string
		token string
		body  string
	}{
		{"ethereum", mixed.Token, "rail=ethereum"}, {"empty value", mixed.Token, "rail="}, {"case", mixed.Token, "rail=Monero"},
		{"twice", mixed.Token, "rail=monero&rail=bitcoin"}, {"twice the same", mixed.Token, "rail=monero&rail=monero"},
		{"a script", mixed.Token, "rail=%3Cscript%3E"}, {"monero on a bitcoin order", btc.Token, "rail=monero"},
		{"bitcoin on a Monero order", xmOnly.Token, "rail=bitcoin"}, {"lightning", mixed.Token, "rail=lightning"},
	} {
		p := e.press(c.token, c.body)
		checkHeaders(t, c.name, p)
		if p.code != 400 || p.body != badRequestBody || p.h.Get("Location") != "" || strings.Contains(p.body, "script") {
			t.Errorf("%s: %d %q", c.name, p.code, p.body)
		}
	}
	if e.xm.calls.Load() != 0 || e.oc.calls.Load() != 0 || e.ln.pays.Load() != 0 {
		t.Errorf("a refused press made something: %d, %d, %d", e.xm.calls.Load(), e.oc.calls.Load(), e.ln.pays.Load())
	}
	// A malformed token is still the 404, whatever the rail.
	if p := e.press(strings.Repeat("Z", 32), "rail=ethereum"); p.code != 404 {
		t.Errorf("a malformed token: %d", p.code)
	}
	// The press without a body, or with an ignored field, is bitcoin on an order with a bitcoin rail.
	if p := e.press(mixed.Token, "other=1"); p.code != 303 || e.xm.calls.Load() != 0 || e.oc.calls.Load() != 1 {
		t.Errorf("no rail on a mixed order: %d, %d Monero calls", p.code, e.xm.calls.Load())
	}
}

// A Monero-only order: one button "Pay with Monero", and a press without a rail is Monero.
func TestMoneroOnlyOrder(t *testing.T) {
	e := newEnv(t)
	o := e.orderAll(0, 0, pico15, time.Hour)
	r := e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	if !strings.Contains(r.body, moneroForm(o.Token)) || strings.Count(r.body, "<form") != 1 || strings.Contains(r.body, "bitcoin") ||
		!strings.Contains(r.body, `<p class="amount">1.5 XMR</p>`) || strings.Contains(r.body, "sats") || strings.Contains(r.body, "BTC") {
		t.Errorf("new Monero-only page:\n%s", r.body)
	}
	if p := e.do("POST", "/c/"+o.Token+"/pay"); p.code != 303 || p.h.Get("Location") != "/c/"+o.Token || e.xm.made.Load() != 1 {
		t.Fatalf("press: %d %q, %d made", p.code, p.h.Get("Location"), e.xm.made.Load())
	}
	r = e.get("/c/" + o.Token)
	checkMonero(t, e, o, r.body, "1.5")
	if strings.Contains(r.body, "<form") || strings.Contains(r.body, "bitcoin") || strings.Contains(r.body, `id="address"`) {
		t.Errorf("issued Monero-only page:\n%s", r.body)
	}
	// Past the expiry the subaddress is no longer offered.
	e.clk.Advance(2 * time.Hour)
	r = e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	if strings.Contains(r.body, `id="monero"`) || strings.Contains(r.body, "monero:") || !strings.Contains(r.body, "Checking payment…") {
		t.Errorf("past the expiry:\n%s", r.body)
	}
	if q := e.get("/c/" + o.Token + "/qr.svg?rail=monero"); q.code != 404 {
		t.Errorf("qr past the expiry: %d", q.code)
	}
	if q := e.get("/c/" + o.Token + "/qr.svg?rail=ethereum"); q.code != 404 {
		t.Errorf("qr of an unknown rail: %d", q.code)
	}
}

// The lines of a Monero payment that was seen: the confirmations with the tier, the XMR seen below the amount,
// "Checking payment…" while the view is stale, and no counts on a 0 tier.
func TestMoneroSeen(t *testing.T) {
	e := newEnv(t)
	o := e.orderAll(0, 0, pico15, time.Hour)
	e.press(o.Token, "rail=monero")
	e.xm.setTweak(func(v *rail.View) { v.State, v.Received, v.Confirmations = rail.Seen, 500_000_000_000, 3 })
	r := e.get("/c/" + o.Token)
	checkMonero(t, e, o, r.body, "1")
	for _, want := range []string{"Payment seen", "Seen, 3 of 10 confirmations", "0.5 XMR seen so far."} {
		if !strings.Contains(r.body, want) {
			t.Errorf("missing %q:\n%s", want, r.body)
		}
	}
	// The whole amount seen: no amount left, so no URI and no QR; the field stays.
	e.xm.setTweak(func(v *rail.View) { v.State, v.Received, v.Confirmations = rail.Seen, pico15, 1 })
	r = e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	if strings.Contains(r.body, "seen so far") || strings.Contains(r.body, "monero:") || imgRe.MatchString(r.body) ||
		strings.Contains(r.body, "Send exactly") || !strings.Contains(r.body, `id="monero"`) || !strings.Contains(r.body, "Seen, 1 of 10") {
		t.Errorf("the whole amount seen:\n%s", r.body)
	}
	if q := e.get("/c/" + o.Token + "/qr.svg?rail=monero"); q.code != 404 {
		t.Errorf("qr with nothing missing: %d", q.code)
	}
	// Stale: "Checking payment…", not the counts.
	e.xm.setTweak(func(v *rail.View) {
		v.State, v.Received, v.Confirmations, v.Stale = rail.Seen, 500_000_000_000, 3, true
	})
	r = e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	if !strings.Contains(r.body, "Checking payment…") || strings.Contains(r.body, "Seen,") {
		t.Errorf("stale:\n%s", r.body)
	}
	// A 0 tier: "Seen" with no counts, open and past the expiry.
	e.xm.setTweak(func(v *rail.View) { v.State, v.Received, v.RequiredConfirmations = rail.Seen, 500_000_000_000, 0 })
	for _, expired := range []bool{false, true} {
		if expired {
			e.clk.Advance(2 * time.Hour)
		}
		r = e.get("/c/" + o.Token)
		checkPlain(t, r.body)
		if !strings.Contains(r.body, `<p class="state open">Seen</p>`) || strings.Contains(r.body, "confirmations") ||
			!strings.Contains(r.body, "0.5 XMR seen so far.") || expired == strings.Contains(r.body, `id="monero"`) {
			t.Errorf("0 tier, expired %v:\n%s", expired, r.body)
		}
	}
	// Stale and expired: the page does not claim nothing was charged.
	e.xm.setTweak(func(v *rail.View) { v.State, v.Stale = rail.Expired, true })
	r = e.get("/c/" + o.Token)
	if !strings.Contains(r.body, "Checking payment…") || strings.Contains(r.body, "Nothing was charged") {
		t.Errorf("stale expired:\n%s", r.body)
	}
}

// The Monero press's errors mirror the on-chain press's (ruling 8-T8a): busy, wallet-rpc unavailable (or not yet
// checked), the wallet behind and a failed subaddress check are ?busy=1; expired and paid are the page; no Monero
// amount is a 400, an unknown order a 404, anything else a 500. A stale Monero view never hides "Pay with Monero", and
// with no subaddress issued it shows no "Checking payment…".
func TestMoneroPressErrors(t *testing.T) {
	for _, c := range []struct {
		err   error
		stale bool
		code  int
		loc   string
	}{
		{monero.ErrBusy, false, 303, "?busy=1"}, {fmt.Errorf("%w: %w", monero.ErrBusy, context.DeadlineExceeded), false, 303, "?busy=1"},
		{context.Canceled, false, 303, "?busy=1"},
		{monero.ErrExpired, false, 303, ""}, {monero.ErrPaid, false, 303, ""},
		{monero.ErrUnavailable, true, 303, "?busy=1"}, {fmt.Errorf("%w: the startup check has not passed", monero.ErrUnavailable), true, 303, "?busy=1"},
		{monero.ErrIndexBehind, false, 303, "?busy=1"}, {fmt.Errorf("%w: %w", monero.ErrSubaddress, monero.ErrIndexBehind), false, 303, "?busy=1"},
		{fmt.Errorf("%w: step 3", monero.ErrSubaddress), false, 303, "?busy=1"},
		{monero.ErrNoMonero, false, 400, ""}, {store.ErrNotFound, false, 404, ""}, {errors.New("boom"), false, 500, ""},
	} {
		e := newEnv(t)
		e.xm.err = c.err
		if c.stale {
			e.xm.setTweak(func(v *rail.View) { v.Stale = true })
		}
		o := e.orderAll(21_000, 0, pico15, time.Hour)
		p := e.press(o.Token, "rail=monero")
		if p.code != c.code || c.code == 303 && p.h.Get("Location") != "/c/"+o.Token+c.loc {
			t.Errorf("%v: %d %q", c.err, p.code, p.h.Get("Location"))
		}
		if e.ln.pays.Load() != 0 || e.oc.calls.Load() != 0 {
			t.Errorf("%v: the Monero press touched a bitcoin rail", c.err)
		}
		if c.code != 303 {
			continue
		}
		r := e.get(p.h.Get("Location"))
		checkPlain(t, r.body)
		if (c.loc != "") != strings.Contains(r.body, "Busy, try again.") || strings.Contains(r.body, "Checking payment…") {
			t.Errorf("%v: page:\n%s", c.err, r.body)
		}
		if !strings.Contains(r.body, moneroForm(o.Token)) || !strings.Contains(r.body, bitcoinForm(o.Token)) {
			t.Errorf("%v: the picker lost a button:\n%s", c.err, r.body)
		}
	}
}

// A stale Monero view never hides "Pay with Monero" (ruling 8-T8a), and "Checking payment…" shows only for a rail with
// an issued address or money seen (staleChecking), on both rails.
func TestStaleViewChecksOnlyAUsedRail(t *testing.T) {
	e := newEnv(t)
	// A Monero-only order, stale, nothing issued: the button, no "Checking payment…", no refresh.
	x := e.orderAll(0, 0, pico15, time.Hour)
	e.xm.setTweak(func(v *rail.View) { v.Stale = true })
	r := e.get("/c/" + x.Token)
	checkPlain(t, r.body)
	if !strings.Contains(r.body, moneroForm(x.Token)) || strings.Contains(r.body, "Checking payment…") || strings.Contains(r.body, `http-equiv="refresh"`) {
		t.Errorf("stale Monero-only page:\n%s", r.body)
	}
	// Issued: now "Checking payment…".
	e.press(x.Token, "rail=monero")
	if r = e.get("/c/" + x.Token); !strings.Contains(r.body, "Checking payment…") {
		t.Errorf("stale Monero-only page with a subaddress:\n%s", r.body)
	}

	// A mixed order paid to with bitcoin while the Monero view is stale and has no subaddress: no "Checking payment…"
	// about Monero, and the Monero button stays.
	m := e.orderAll(21_000, sat21, pico15, time.Hour)
	e.press(m.Token, "rail=bitcoin")
	r = e.get("/c/" + m.Token)
	checkPlain(t, r.body)
	if strings.Contains(r.body, "Checking payment…") || !strings.Contains(r.body, moneroForm(m.Token)) || !strings.Contains(r.body, `id="address"`) {
		t.Errorf("bitcoin issued, Monero stale and unused:\n%s", r.body)
	}

	// On-chain, the same rule: a stale view with only the invoice issued (the address failed) says nothing; with the
	// address, "Checking payment…".
	e.xm.setTweak(nil)
	e.oc.setTweak(func(r *onchain.Rail) { r.Stale = true })
	e.oc.err = onchain.ErrUnavailable
	b := e.orderBoth(21_000, sat21, time.Hour)
	e.press(b.Token, "")
	r = e.get("/c/" + b.Token)
	checkPlain(t, r.body)
	if strings.Contains(r.body, "Checking payment…") || !strings.Contains(r.body, `id="invoice"`) || strings.Contains(r.body, `id="address"`) {
		t.Errorf("on-chain stale and unused:\n%s", r.body)
	}
	e.oc.err = nil
	e.press(b.Token, "")
	if r = e.get("/c/" + b.Token); !strings.Contains(r.body, "Checking payment…") || !strings.Contains(r.body, `id="address"`) {
		t.Errorf("on-chain stale with an address:\n%s", r.body)
	}

	// Expired, the stale rail unused: the page says "Nothing was charged"; used, it does not.
	for _, c := range []struct {
		name string
		so   status.Order
	}{
		{"on-chain", func() status.Order {
			so := stubOrder(21_000, sat21, "")
			so.Onchain.Issued, so.Onchain.Address, so.Onchain.Stale = false, "", true
			return so
		}()},
		{"Monero", func() status.Order {
			so := stubOrder3(21_000, 0, pico15, "")
			so.Monero.Issued, so.Monero.Address, so.Monero.Stale = false, "", true
			return so
		}()},
	} {
		so := c.so
		so.State = status.StateExpired
		r := getStub(stubHandler(so, io.Discard), so, "")
		if !strings.Contains(r.body, "Nothing was charged") || strings.Contains(r.body, "Checking payment…") {
			t.Errorf("%s: expired, stale and unused:\n%s", c.name, r.body)
		}
		if so.Onchain != nil {
			so.Onchain.Issued = true
		} else {
			so.Monero.Issued = true
		}
		r = getStub(stubHandler(so, io.Discard), so, "")
		if strings.Contains(r.body, "Nothing was charged") || !strings.Contains(r.body, "Checking payment…") {
			t.Errorf("%s: expired, stale and used:\n%s", c.name, r.body)
		}
	}
}

// The press on a known order with a body it cannot read as a form is a 400; an unknown well-formed token is the
// 404 whatever the body.
func TestPressBodyRefusals(t *testing.T) {
	e := newEnv(t)
	o := e.orderAll(21_000, sat21, pico15, time.Hour)
	unknown := strings.Repeat("ab", 16)
	big := "rail=monero&pad=" + strings.Repeat("a", 2<<10)
	for _, c := range []struct {
		name, token, body string
		code              int
	}{
		{"a form over the limit", o.Token, big, 400}, {"a malformed escape", o.Token, "rail=%zz", 400},
		{"unknown token, bad rail", unknown, "rail=ethereum", 404}, {"unknown token, a form over the limit", unknown, big, 404},
		{"unknown token, monero", unknown, "rail=monero", 404},
	} {
		p := e.press(c.token, c.body)
		checkHeaders(t, c.name, p)
		if p.code != c.code || c.code == 400 && p.body != badRequestBody || c.code == 404 && p.body != notFoundBody {
			t.Errorf("%s: %d %q", c.name, p.code, p.body)
		}
	}
	if e.xm.calls.Load() != 0 || e.oc.calls.Load() != 0 || e.ln.pays.Load() != 0 {
		t.Errorf("a refused press made something: %d, %d, %d", e.xm.calls.Load(), e.oc.calls.Load(), e.ln.pays.Load())
	}
	// The QR route: no rail is the bitcoin QR, exactly rail=monero the Monero one, any other rail value a 404.
	e.press(o.Token, "rail=monero")
	if q := e.get("/c/" + o.Token + "/qr.svg?rail=monero"); q.code != 200 {
		t.Errorf("qr.svg?rail=monero: %d", q.code)
	}
	for _, q := range []string{"?rail=", "?rail=bitcoin", "?rail=Monero", "?rail=monero&rail=monero", "?rail"} {
		if r := e.get("/c/" + o.Token + "/qr.svg" + q); r.code != 404 || r.body != notFoundBody {
			t.Errorf("qr.svg%s: %d", q, r.code)
		}
	}
}

// The Monero rail is not configured: an order with a Monero amount reads "Checking", never a 500; a press is busy;
// Lightning-only orders are unaffected.
func TestMoneroRailOffIsChecking(t *testing.T) {
	e := newEnv(t)
	o := e.orderAll(21_000, 0, pico15, time.Hour)
	l := e.order(5_000, time.Hour)
	e.buildRails(e.oc, nil)
	r := e.get("/c/" + o.Token)
	checkPlain(t, r.body)
	if r.code != 200 || !strings.Contains(r.body, "Checking payment…") || strings.Contains(r.body, "<form") {
		t.Errorf("page (%d):\n%s", r.code, r.body)
	}
	if q := e.get("/c/" + o.Token + "/qr.svg?rail=monero"); q.code != 404 {
		t.Errorf("qr: %d", q.code)
	}
	if p := e.press(o.Token, "rail=monero"); p.code != 303 || p.h.Get("Location") != "/c/"+o.Token+"?busy=1" || e.ln.pays.Load() != 0 {
		t.Errorf("pay: %d %q", p.code, p.h.Get("Location"))
	}
	if r := e.get("/c/" + l.Token); !strings.Contains(r.body, "Pay with Lightning") {
		t.Errorf("a Lightning-only order is affected:\n%s", r.body)
	}
}

// The end states of an order with a Monero rail: the refund note for an overpaid or late Monero view, or for money on
// Monero and another rail; the underpaid texts from the Monero view.
func TestMoneroEndStates(t *testing.T) {
	for _, c := range []struct {
		name   string
		build  func(*status.Order)
		refund bool
		want   string
	}{
		{"paid on Monero", func(o *status.Order) { o.State, o.Monero.Paid, o.Monero.Received = status.StatePaid, true, pico15 }, false, ">Paid<"},
		{"overpaid on Monero", func(o *status.Order) {
			o.State, o.Monero.Paid, o.Monero.Overpaid, o.Monero.Received = status.StatePaid, true, true, pico15+1
		}, true, ">Paid<"},
		{"Monero late", func(o *status.Order) { o.State, o.Monero.Paid, o.Monero.Late = status.StatePaid, true, 5 }, true, ">Paid<"},
		{"paid on Lightning, Monero seen", func(o *status.Order) {
			o.State, o.Lightning.State, o.Monero.Received = status.StatePaid, lnpay.StatePaid, 5
		}, true, ">Paid<"},
		{"paid on Monero, on-chain seen", func(o *status.Order) {
			o.State, o.Monero.Paid, o.Monero.Received, o.Onchain.ReceivedSat = status.StatePaid, true, pico15, 5
		}, true, ">Paid<"},
		{"paid on both Monero and on-chain", func(o *status.Order) {
			o.State, o.Monero.Paid, o.Monero.Received = status.StatePaid, true, pico15
			o.Onchain.Paid, o.Onchain.ReceivedSat = true, sat21
		}, true, ">Paid<"},
		{"underpaid on Monero", func(o *status.Order) {
			o.State, o.Monero.State, o.Monero.Received = status.StateUnderpaid, rail.Underpaid, 5
		}, false, "Payment received is below the amount."},
		{"underpaid on Monero, the whole amount", func(o *status.Order) {
			o.State, o.Monero.State, o.Monero.Received = status.StateUnderpaid, rail.Underpaid, pico15
		}, false, "Your payment is not yet confirmed enough."},
		{"underpaid on Monero, nothing live", func(o *status.Order) {
			o.State, o.Monero.State, o.Monero.Received = status.StateUnderpaid, rail.Underpaid, 0
		}, false, "Your payment did not confirm."},
	} {
		so := stubOrder3(21_000, sat21, pico15, "lnbcrt1abc")
		c.build(&so)
		r := getStub(stubHandler(so, io.Discard), so, "")
		checkPlain(t, r.body)
		if !strings.Contains(r.body, c.want) || strings.Contains(r.body, "refund the extra") != c.refund || strings.Contains(r.body, "<form") {
			t.Errorf("%s: refund %v:\n%s", c.name, c.refund, r.body)
		}
		if q := getStub(stubHandler(so, io.Discard), so, "/qr.svg?rail=monero"); q.code != 404 {
			t.Errorf("%s: qr.svg?rail=monero %d", c.name, q.code)
		}
	}
}

func TestAmountsLineMonero(t *testing.T) {
	for _, c := range []struct {
		msat, sat, pico int64
		want            string
	}{
		{21_000_000, 21_000, pico15, "21,000 sats or 0.00021 BTC or 1.5 XMR"}, {0, 0, 1, "0.000000000001 XMR"},
		{1_000, 0, store.MaxPiconero, "1 sat or 9007.199254740991 XMR"}, {0, 21_000, 1_000_000_000_000, "0.00021 BTC or 1 XMR"},
	} {
		if got := amounts(store.Order{AmountMsat: c.msat, AmountSat: c.sat, AmountPiconero: c.pico}); got != c.want {
			t.Errorf("(%d, %d, %d): %q, want %q", c.msat, c.sat, c.pico, got, c.want)
		}
	}
}

func TestLogoRoute(t *testing.T) {
	e := newEnv(t)
	r := e.get("/static/logo.svg")
	if r.code != 200 || r.h.Get("Content-Type") != "image/svg+xml" || r.body != string(logoSVG) {
		t.Fatalf("logo: %d %q", r.code, r.h.Get("Content-Type"))
	}
	checkHeaders(t, "logo", r)
	if got, want := r.h.Get("Cache-Control"), e.get("/static/style.css").h.Get("Cache-Control"); got != want {
		t.Errorf("logo Cache-Control %q, style.css %q", got, want)
	}
	if e.get("/static/logo.svg/x").code != 404 || e.do("POST", "/static/logo.svg").code == 200 {
		t.Error("logo route is not a plain GET")
	}
}

func TestLogoIsInert(t *testing.T) {
	s := strings.ToLower(string(logoSVG))
	if len(s) == 0 {
		t.Fatal("empty logo")
	}
	for _, bad := range []string{"<script", "href=", "xlink:", "style=", "<style", "url(", "@import", "<image", "<foreignobject", "javascript:"} {
		if strings.Contains(s, bad) {
			t.Errorf("logo contains %q", bad)
		}
	}
	if regexp.MustCompile(`\son[a-z]+\s*=`).MatchString(s) {
		t.Error("logo has an event-handler attribute")
	}
}

func TestPageHasLogo(t *testing.T) {
	e := newEnv(t)
	o := e.order(5_000, time.Hour)
	b := e.get("/c/" + o.Token).body
	if !strings.Contains(b, `<link rel="icon" href="/static/logo.svg" type="image/svg+xml" />`) || !strings.Contains(b, `src="/static/logo.svg"`) {
		t.Error("page lacks the icon link or the header mark")
	}
}
