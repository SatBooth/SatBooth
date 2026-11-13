package rates

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satbooth/satbooth/rates/ratestest"
)

// testClock is the clock both the service and the fake read.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// logBuf captures the service's JSON log lines.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// logLine is one decoded line; every attribute the service logs is typed, so no number decodes into a
// float.
type logLine struct {
	Msg       string   `json:"msg"`
	Level     string   `json:"level"`
	Provider  string   `json:"provider"`
	Class     string   `json:"class"`
	Status    *int     `json:"status"`
	Asset     string   `json:"asset"`
	Currency  string   `json:"currency"`
	Reason    string   `json:"reason"`
	Providers []string `json:"providers"`
	Low       *string  `json:"low"`
	High      *string  `json:"high"`
	raw       string
}

func (l *logBuf) lines(t *testing.T, msg string) []logLine {
	t.Helper()
	var out []logLine
	sc := bufio.NewScanner(strings.NewReader(l.String()))
	for sc.Scan() {
		var ln logLine
		if err := json.Unmarshal(sc.Bytes(), &ln); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		ln.raw = sc.Text()
		if ln.Msg == msg {
			out = append(out, ln)
		}
	}
	return out
}

func forCurrency(ls []logLine, currency string) []logLine {
	var out []logLine
	for _, l := range ls {
		if l.Currency == currency {
			out = append(out, l)
		}
	}
	return out
}

type env struct {
	fake   *ratestest.Server
	clk    *testClock
	log    *logBuf
	client *http.Client
	svc    *Service
}

const customURL = "https://rates.example.invalid/"

// newEnv starts the fake with the four providers quoting USD at 75990 to 76020 (median 76005) and EUR at
// 67000, and a service over it at the policy {2, 200 bps, 300 s}. mod may change the configuration.
func newEnv(t *testing.T, mod func(*Config, *http.Client)) *env {
	t.Helper()
	e := &env{fake: ratestest.New(t), clk: &testClock{t: now2030}, log: &logBuf{}}
	e.fake.SetClock(e.clk.Now)
	e.client = NewClient(e.fake.Transport())
	for p, usd := range map[string]string{"kraken": "76000", "coinbase": "76010", "yadio": "75990", "custom": "76020"} {
		e.fake.Set(p, "USD", usd)
		e.fake.Set(p, "EUR", "67000")
	}
	cfg := Config{
		Providers:  []Provider{Kraken(e.client), Coinbase(e.client), Yadio(e.client), Custom(e.client, customURL)},
		Assets:     []string{"BTC"},
		Currencies: []string{"USD", "EUR"},
		Interval:   50 * time.Millisecond,
		Policy:     Policy{MinSources: 2, MaxSpreadBps: 200, MaxAge: 300 * time.Second},
		MarginBps:  100,
		Now:        e.clk.Now,
		Log:        slog.New(slog.NewJSONHandler(e.log, nil)),
	}
	if mod != nil {
		mod(&cfg, e.client)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e
}

func (e *env) noSentinel(t *testing.T) {
	t.Helper()
	if s := e.log.String(); strings.Contains(s, ratestest.Sentinel) || strings.Contains(s, "example.invalid") {
		t.Fatalf("a provider's body or URL reached the log:\n%s", s)
	}
}

func sourceAt(q Quote, provider string) (time.Time, bool) {
	for _, r := range q.Sources {
		if r.Provider == provider {
			return r.At, true
		}
	}
	return time.Time{}, false
}

func TestServiceFirstFetchThenQuote(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.svc.Quote("BTC", "USD"); !errors.Is(err, ErrTooFew) {
		t.Fatalf("before any fetch: %v, want ErrTooFew", err)
	}
	e.svc.fetch(context.Background())
	q, err := e.svc.Quote("BTC", "USD")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"coinbase", "custom", "kraken", "yadio"}; !equalStrings(names(q.Sources), want) {
		t.Fatalf("Sources %v, want %v", names(q.Sources), want)
	}
	if q.Rate.Cmp(dec(t, "76005")) != 0 || q.Rate.Cmp(Median(q.Sources)) != 0 {
		t.Fatalf("Rate %s, want the median 76005", q.Rate)
	}
	if q.Asset != "BTC" || q.Currency != "USD" || !q.At.Equal(now2030) {
		t.Fatalf("quote %+v", q)
	}
	for _, r := range q.Sources {
		if r.Currency != "USD" || !r.At.Equal(now2030) {
			t.Fatalf("source %+v not on the test clock", r)
		}
	}
	if q, err := e.svc.Quote("BTC", "EUR"); err != nil || q.Rate.Cmp(dec(t, "67000")) != 0 || len(q.Sources) != 4 {
		t.Fatalf("EUR: %+v, %v", q, err)
	}

	pr, err := e.svc.Price("BTC", dec(t, "10"), "USD")
	if err != nil {
		t.Fatal(err)
	}
	want, err := Sats(dec(t, "10"), dec(t, "76005"), 100)
	if err != nil || pr.Amount != want || pr.MarginBps != 100 || pr.Quote.Rate.Cmp(dec(t, "76005")) != 0 || len(pr.Quote.Sources) != 4 {
		t.Fatalf("Price %+v, want %d sats", pr, want)
	}
	if _, err := e.svc.Price("BTC", dec(t, "10"), "COP"); !errors.Is(err, ErrCurrency) {
		t.Fatalf("COP: %v, want ErrCurrency", err)
	}
	if _, err := e.svc.Quote("XMR", "USD"); !errors.Is(err, ErrAsset) {
		t.Fatalf("XMR: %v, want ErrAsset", err)
	}
	if _, err := e.svc.Price("XMR", dec(t, "10"), "USD"); !errors.Is(err, ErrAsset) {
		t.Fatalf("Price XMR: %v, want ErrAsset", err)
	}
	if e.svc.Reachable("XMR", "USD") || !e.svc.Reachable("BTC", "USD") || e.svc.Reachable("BTC", "COP") {
		t.Fatal("Reachable: XMR is not configured, BTC/USD is, COP is not")
	}
	if _, err := e.svc.Price("BTC", Decimal{}, "USD"); !errors.Is(err, ErrZero) {
		t.Fatalf("Price 0: %v, want ErrZero passed through", err)
	}
	cs := e.svc.Currencies()
	if !equalStrings(cs, []string{"USD", "EUR"}) || e.svc.cfg.MarginBps != 100 {
		t.Fatalf("Currencies %v, MarginBps %d", cs, e.svc.cfg.MarginBps)
	}
	cs[0] = "COP"
	if e.svc.Currencies()[0] != "USD" {
		t.Fatal("Currencies is not a copy")
	}
	// A quote's Sources is the caller's: changing it changes no later quote.
	q.Sources[0] = Rate{}
	if q2, _ := e.svc.Quote("BTC", "USD"); q2.Sources[0].Provider != "coinbase" {
		t.Fatal("Quote shares its Sources with the cache")
	}
	// One request per provider per asset per pass; the first pass of a working setup logs nothing.
	for _, p := range []string{"kraken", "coinbase", "yadio", "custom"} {
		if n := e.fake.Hits(p); n != 1 {
			t.Errorf("%s: %d requests, want 1", p, n)
		}
	}
	if s := e.log.String(); s != "" {
		t.Fatalf("a good first pass logged:\n%s", s)
	}
}

func TestProviderOutage(t *testing.T) {
	e := newEnv(t, nil)
	e.fake.Fail("kraken", 500)
	e.fake.Fail("coinbase", 503)
	e.svc.fetch(context.Background())
	q, err := e.svc.Quote("BTC", "USD")
	if err != nil || !equalStrings(names(q.Sources), []string{"custom", "yadio"}) {
		t.Fatalf("after the first fetch: %v, %v", names(q.Sources), err)
	}
	failed := e.log.lines(t, logFetchFailed)
	if len(failed) != 2 {
		t.Fatalf("%d fetch failures logged, want 2", len(failed))
	}
	for _, l := range failed {
		want := map[string]int{"kraken": 500, "coinbase": 503}[l.Provider]
		if want == 0 || l.Class != "status" || l.Status == nil || *l.Status != want {
			t.Fatalf("fetch failure line %s", l.raw)
		}
	}
	if n := len(e.log.lines(t, logUnavailable)) + len(e.log.lines(t, logAvailable)); n != 0 {
		t.Fatalf("the first pass logged %d state lines for an available quote", n)
	}

	// Yadio fails: its old rate still counts until MaxAge.
	e.fake.Fail("yadio", 502)
	e.svc.fetch(context.Background())
	e.clk.Advance(300 * time.Second)
	q, err = e.svc.Quote("BTC", "USD")
	if err != nil {
		t.Fatalf("at MaxAge: %v", err)
	}
	if at, ok := sourceAt(q, "yadio"); !ok || !at.Equal(now2030) {
		t.Fatalf("Yadio's old rate is not in the quote: %v", names(q.Sources))
	}
	e.clk.Advance(time.Nanosecond)
	if _, err := e.svc.Quote("BTC", "USD"); !errors.Is(err, ErrTooFew) {
		t.Fatalf("at MaxAge + 1 ns: %v, want ErrTooFew", err)
	}
	var yadio []logLine
	for _, l := range e.log.lines(t, logFetchFailed) {
		if l.Provider == "yadio" {
			yadio = append(yadio, l)
		}
	}
	if len(yadio) != 1 || yadio[0].Class != "status" || *yadio[0].Status != 502 {
		t.Fatalf("Yadio's failure not logged once: %+v", yadio)
	}

	// The next pass refreshes the custom rate only: too few, one ALERT per currency.
	e.svc.fetch(context.Background())
	if _, err := e.svc.Quote("BTC", "USD"); !errors.Is(err, ErrTooFew) {
		t.Fatalf("one fresh source: %v", err)
	}
	e.svc.fetch(context.Background()) // still too few: no second ALERT
	alerts := e.log.lines(t, logUnavailable)
	if len(alerts) != 2 || len(forCurrency(alerts, "USD")) != 1 || len(forCurrency(alerts, "EUR")) != 1 {
		t.Fatalf("ALERTs %+v, want one per currency", alerts)
	}
	for _, a := range alerts {
		if a.Asset != "BTC" || a.Reason != "too_few" || !equalStrings(a.Providers, []string{"custom"}) || a.Low != nil || a.High != nil || a.Level != "ERROR" {
			t.Fatalf("ALERT %s", a.raw)
		}
	}
	if !strings.HasPrefix(logUnavailable, "ALERT: ") {
		t.Fatalf("logUnavailable %q", logUnavailable)
	}

	// Kraken is back: available again, logged once per currency.
	e.fake.Fail("kraken", 0)
	e.svc.fetch(context.Background())
	e.svc.fetch(context.Background())
	if q, err := e.svc.Quote("BTC", "USD"); err != nil || !equalStrings(names(q.Sources), []string{"custom", "kraken"}) {
		t.Fatalf("Kraken back: %v, %v", names(q.Sources), err)
	}
	back := e.log.lines(t, logAvailable)
	if len(back) != 2 || len(forCurrency(back, "USD")) != 1 {
		t.Fatalf("available lines %+v, want one per currency", back)
	}
	for _, b := range back {
		if b.Asset != "BTC" || !equalStrings(b.Providers, []string{"custom", "kraken"}) || b.Reason != "" {
			t.Fatalf("available line %s", b.raw)
		}
	}
	e.noSentinel(t)
}

func TestProviderLies(t *testing.T) {
	e := newEnv(t, nil)
	for _, p := range []string{"kraken", "coinbase", "yadio"} {
		e.fake.Set(p, "USD", "76000")
	}
	e.fake.Set("custom", "USD", "90000")
	e.svc.fetch(context.Background())
	if _, err := e.svc.Quote("BTC", "USD"); !errors.Is(err, ErrDisagree) {
		t.Fatalf("a lying provider: %v, want ErrDisagree", err)
	}
	if _, err := e.svc.Quote("BTC", "EUR"); err != nil {
		t.Fatalf("EUR is unaffected: %v", err)
	}
	alerts := e.log.lines(t, logUnavailable)
	if len(alerts) != 1 {
		t.Fatalf("%d ALERTs, want 1", len(alerts))
	}
	a := alerts[0]
	if a.Currency != "USD" || a.Reason != "disagree" || !equalStrings(a.Providers, []string{"coinbase", "custom", "kraken", "yadio"}) ||
		a.Low == nil || *a.Low != "coinbase" || a.High == nil || *a.High != "custom" {
		t.Fatalf("ALERT %s", a.raw)
	}
	for _, v := range []string{"76000", "90000", "67000"} {
		if strings.Contains(e.log.String(), v) {
			t.Fatalf("a rate is in the log: %s", e.log.String())
		}
	}

	// Exactly 2 % over the lowest: a quote; one unit more: refused.
	e.fake.Set("custom", "USD", "77520")
	e.svc.fetch(context.Background())
	if q, err := e.svc.Quote("BTC", "USD"); err != nil || q.Rate.Cmp(dec(t, "76000")) != 0 {
		t.Fatalf("at the bound: %v", err)
	}
	if l := e.log.lines(t, logAvailable); len(l) != 1 || l[0].Currency != "USD" {
		t.Fatalf("available lines %+v", l)
	}
	e.fake.Set("custom", "USD", "77520.000000000000000001")
	e.svc.fetch(context.Background())
	if _, err := e.svc.Quote("BTC", "USD"); !errors.Is(err, ErrDisagree) {
		t.Fatalf("one unit over: %v, want ErrDisagree", err)
	}
	if n := len(e.log.lines(t, logUnavailable)); n != 2 {
		t.Fatalf("%d ALERTs, want 2", n)
	}
	e.noSentinel(t)
}

func TestReasonChangeLogsAgain(t *testing.T) {
	e := newEnv(t, nil)
	for _, p := range []string{"kraken", "coinbase", "yadio"} {
		e.fake.Fail(p, 500)
	}
	e.svc.fetch(context.Background())
	for _, p := range []string{"kraken", "coinbase", "yadio"} {
		e.fake.Fail(p, 0)
	}
	e.fake.Set("custom", "USD", "90000")
	e.svc.fetch(context.Background())
	if _, err := e.svc.Quote("BTC", "USD"); !errors.Is(err, ErrDisagree) {
		t.Fatalf("%v, want ErrDisagree", err)
	}
	usd := forCurrency(e.log.lines(t, logUnavailable), "USD")
	if len(usd) != 2 || usd[0].Reason != "too_few" || usd[1].Reason != "disagree" {
		t.Fatalf("USD ALERTs %+v, want too_few then disagree", usd)
	}
	if l := forCurrency(e.log.lines(t, logAvailable), "EUR"); len(l) != 1 {
		t.Fatalf("EUR available lines %+v", l)
	}
	// And back from disagree to too_few logs again.
	e.fake.Fail("kraken", 500)
	e.fake.Fail("coinbase", 500)
	e.fake.Fail("yadio", 500)
	e.clk.Advance(301 * time.Second)
	e.svc.fetch(context.Background())
	if usd := forCurrency(e.log.lines(t, logUnavailable), "USD"); len(usd) != 3 || usd[2].Reason != "too_few" {
		t.Fatalf("USD ALERTs %+v", usd)
	}
}

func TestSlowProvider(t *testing.T) {
	e := newEnv(t, func(_ *Config, c *http.Client) { c.Timeout = 200 * time.Millisecond })
	e.svc.fetch(context.Background())
	e.fake.Delay("yadio", 400*time.Millisecond)
	e.clk.Advance(100 * time.Second)
	start := time.Now()
	e.svc.fetch(context.Background())
	if d := time.Since(start); d > 1200*time.Millisecond {
		t.Fatalf("the pass took %v", d)
	}
	q, err := e.svc.Quote("BTC", "USD")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"kraken", "coinbase", "custom"} {
		if at, ok := sourceAt(q, p); !ok || !at.Equal(now2030.Add(100*time.Second)) {
			t.Fatalf("%s's new rate not cached: %v", p, at)
		}
	}
	if at, ok := sourceAt(q, "yadio"); !ok || !at.Equal(now2030) {
		t.Fatalf("Yadio's old rate not kept: %v %v", at, ok)
	}
	if l := e.log.lines(t, logFetchFailed); len(l) != 1 || l[0].Provider != "yadio" || l[0].Class != "transport" || *l[0].Status != 0 {
		t.Fatalf("fetch failures %+v", l)
	}
	e.clk.Advance(200*time.Second + time.Nanosecond)
	q, err = e.svc.Quote("BTC", "USD")
	if err != nil || !equalStrings(names(q.Sources), []string{"coinbase", "custom", "kraken"}) {
		t.Fatalf("Yadio did not age out: %v, %v", names(q.Sources), err)
	}
	e.noSentinel(t)
}

func TestFetchIsBounded(t *testing.T) {
	e := newEnv(t, nil)
	stop := make(chan struct{})
	var calls atomic.Int64
	var wg sync.WaitGroup
	for range 1000 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				e.svc.Quote("BTC", "USD")
				calls.Add(1)
				runtime.Gosched()
			}
		}()
	}
	for range 3 {
		e.svc.fetch(context.Background())
	}
	close(stop)
	wg.Wait()
	for _, p := range []string{"kraken", "coinbase", "yadio", "custom"} {
		if n := e.fake.Hits(p); n != 3 {
			t.Errorf("%s: %d requests, want 3", p, n)
		}
	}
	if calls.Load() < 1000 {
		t.Fatalf("only %d quotes ran", calls.Load())
	}
	if _, err := e.svc.Quote("BTC", "USD"); err != nil {
		t.Fatal(err)
	}
}

func TestFetchKeepsOldOnShapeError(t *testing.T) {
	e := newEnv(t, nil)
	e.svc.fetch(context.Background())
	e.fake.Body("coinbase", []byte("{}"))
	e.clk.Advance(100 * time.Second)
	e.svc.fetch(context.Background())
	q, err := e.svc.Quote("BTC", "USD")
	if err != nil {
		t.Fatal(err)
	}
	if at, ok := sourceAt(q, "coinbase"); !ok || !at.Equal(now2030) {
		t.Fatalf("Coinbase's old rate not kept: %v", names(q.Sources))
	}
	if l := e.log.lines(t, logFetchFailed); len(l) != 1 || l[0].Provider != "coinbase" || l[0].Class != "shape" || *l[0].Status != 0 {
		t.Fatalf("fetch failures %+v", l)
	}
	e.clk.Advance(200*time.Second + time.Nanosecond)
	q, err = e.svc.Quote("BTC", "USD")
	if err != nil || !equalStrings(names(q.Sources), []string{"custom", "kraken", "yadio"}) {
		t.Fatalf("Coinbase did not age out: %v, %v", names(q.Sources), err)
	}
}

// TestFetchErrorClasses: each class of a failed fetch is logged with its name and the status.
func TestFetchErrorClasses(t *testing.T) {
	e := newEnv(t, nil)
	e.fake.VendorError("kraken", true)
	e.fake.Body("coinbase", bytes.Repeat([]byte(" "), MaxResponse+1))
	e.fake.Fail("yadio", 429)
	e.svc.fetch(context.Background())
	got := map[string]string{}
	for _, l := range e.log.lines(t, logFetchFailed) {
		got[l.Provider] = l.Class
		if l.Status == nil || (l.Provider == "yadio") != (*l.Status == 429) || (l.Provider != "yadio" && *l.Status != 0) {
			t.Fatalf("line %s", l.raw)
		}
	}
	want := map[string]string{"kraken": "vendor", "coinbase": "too_large", "yadio": "status"}
	if len(got) != len(want) {
		t.Fatalf("classes %v, want %v", got, want)
	}
	for p, c := range want {
		if got[p] != c {
			t.Fatalf("classes %v, want %v", got, want)
		}
	}
	e.noSentinel(t)
}

// TestReplaceDropsAMissingCurrency: a provider's result replaces its entry; a currency it stops quoting
// is gone at once, not kept until it ages out.
func TestReplaceDropsAMissingCurrency(t *testing.T) {
	e := newEnv(t, nil)
	e.svc.fetch(context.Background())
	e.fake.Reset()
	for _, p := range []string{"kraken", "coinbase", "yadio", "custom"} {
		e.fake.Set(p, "USD", "76000")
	}
	e.svc.fetch(context.Background())
	if _, err := e.svc.Quote("BTC", "EUR"); !errors.Is(err, ErrTooFew) {
		t.Fatalf("EUR kept after every provider dropped it: %v", err)
	}
}

// overlap records, per pass (keyed by the at a pass passes), the fetches in flight, and whether one
// began while a fetch of another pass was in flight.
type overlap struct {
	mu       sync.Mutex
	inFlight map[int64]int
	bad      bool
}

type watched struct {
	Provider
	o *overlap
}

func (w watched) Fetch(ctx context.Context, asset string, currencies []string, at time.Time) ([]Rate, error) {
	key := at.UnixNano()
	w.o.mu.Lock()
	for k, n := range w.o.inFlight {
		if k != key && n > 0 {
			w.o.bad = true
		}
	}
	w.o.inFlight[key]++
	w.o.mu.Unlock()
	defer func() {
		w.o.mu.Lock()
		w.o.inFlight[key]--
		w.o.mu.Unlock()
	}()
	return w.Provider.Fetch(ctx, asset, currencies, at)
}

func TestPassesNeverOverlap(t *testing.T) {
	o := &overlap{inFlight: map[int64]int{}}
	e := newEnv(t, func(cfg *Config, _ *http.Client) {
		cfg.Interval = 10 * time.Millisecond
		cfg.Now = time.Now // a new at for every pass
		for i, p := range cfg.Providers {
			cfg.Providers[i] = watched{p, o}
		}
	})
	e.fake.SetClock(time.Now)
	const delay, run = 50 * time.Millisecond, 200 * time.Millisecond
	for _, p := range []string{"kraken", "coinbase", "yadio", "custom"} {
		e.fake.Delay(p, delay)
	}
	ctx, cancel := context.WithTimeout(context.Background(), run)
	defer cancel()
	if err := e.svc.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}
	for _, p := range []string{"kraken", "coinbase", "yadio", "custom"} {
		if n := e.fake.Hits(p); n < 2 || n > int(run/delay)+1 {
			t.Errorf("%s: %d requests in %v, want 2 to %d", p, n, run, int(run/delay)+1)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.bad {
		t.Fatal("a fetch began while one of another pass was in flight")
	}
	// A pass cut short by the end of Run logs no failure.
	if l := e.log.lines(t, logFetchFailed); len(l) != 0 {
		t.Fatalf("cancelled fetches logged: %+v", l)
	}
}

func TestRunStopsOnContext(t *testing.T) {
	e := newEnv(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for e.fake.Hits("custom") < 2 { // the pass at once, then one after Interval
		if time.Now().After(deadline) {
			t.Fatal("Run made fewer than 2 passes in 5 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := e.svc.Quote("BTC", "USD"); err != nil {
		t.Fatalf("Run did not fill the cache: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	n := e.fake.Hits("custom")
	time.Sleep(150 * time.Millisecond)
	if e.fake.Hits("custom") != n {
		t.Fatal("a fetch ran after Run returned")
	}
}

func TestNewRefusals(t *testing.T) {
	good := func() Config {
		return Config{
			Providers:  []Provider{Kraken(NewClient(nil))},
			Assets:     []string{"BTC"},
			Currencies: []string{"USD"},
			Interval:   10 * time.Second,
			Policy:     Policy{MinSources: 1, MaxSpreadBps: 1, MaxAge: 20 * time.Second},
			MarginBps:  0,
			Now:        time.Now,
		}
	}
	if s, err := New(good()); err != nil || s.log == nil {
		t.Fatalf("a good config: %v (a nil Log must default)", err)
	}
	for name, mod := range map[string]func(*Config){
		"no providers":         func(c *Config) { c.Providers = nil },
		"no assets":            func(c *Config) { c.Assets = []string{} },
		"no currencies":        func(c *Config) { c.Currencies = nil },
		"zero interval":        func(c *Config) { c.Interval = 0 },
		"negative interval":    func(c *Config) { c.Interval = -time.Second },
		"interval > max age/2": func(c *Config) { c.Interval = 10*time.Second + 1 },
		"min sources 0":        func(c *Config) { c.Policy.MinSources = 0 },
		"max spread 0":         func(c *Config) { c.Policy.MaxSpreadBps = 0 },
		"max age under 1 s":    func(c *Config) { c.Policy.MaxAge = time.Second - 1; c.Interval = time.Millisecond },
		"margin under -50 %":   func(c *Config) { c.MarginBps = MinMarginBps - 1 },
		"margin over +50 %":    func(c *Config) { c.MarginBps = MaxMarginBps + 1 },
		"nil clock":            func(c *Config) { c.Now = nil },
	} {
		c := good()
		mod(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The bounds themselves are accepted.
	for name, mod := range map[string]func(*Config){
		"interval = max age/2": func(c *Config) { c.Interval = 10 * time.Second },
		"max age 1 s":          func(c *Config) { c.Policy.MaxAge = time.Second; c.Interval = 500 * time.Millisecond },
		"margin -50 %":         func(c *Config) { c.MarginBps = MinMarginBps },
		"margin +50 %":         func(c *Config) { c.MarginBps = MaxMarginBps },
	} {
		c := good()
		mod(&c)
		if _, err := New(c); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Two providers with one name.
	c0 := good()
	c0.Providers = append(c0.Providers, Coinbase(NewClient(nil)), Kraken(NewClient(nil)))
	if _, err := New(c0); err == nil || strings.Contains(err.Error(), "kraken") {
		t.Fatalf("two providers named kraken: %v", err)
	}
	// New copies the lists.
	c := good()
	s, _ := New(c)
	c.Currencies[0] = "EUR"
	if s.Currencies()[0] != "USD" {
		t.Fatal("New kept the caller's slice")
	}
}

// TestFastEntryNotHeldBack: a provider's answer reaches the cache as soon as it arrives; a stalled
// provider does not hold back the others' fresh rates until the pass ends.
func TestFastEntryNotHeldBack(t *testing.T) {
	e := newEnv(t, nil)
	e.svc.fetch(context.Background())
	e.fake.Delay("yadio", 2*time.Second)
	e.clk.Advance(100 * time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.svc.fetch(context.Background())
	}()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		q, err := e.svc.Quote("BTC", "USD")
		if err != nil {
			t.Fatal(err)
		}
		at, _ := sourceAt(q, "kraken")
		if at.Equal(now2030.Add(100 * time.Second)) {
			break
		}
		select {
		case <-done:
			t.Fatal("Kraken's new rate reached the cache only when the pass ended")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Kraken's new rate not cached while Yadio stalls")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("the pass ended before Yadio's delay: the test proves nothing")
	default:
	}
	<-done
}

// blocker is a provider that ignores every bound but its context.
type blocker struct{ name string }

func (b blocker) Name() string { return b.name }
func (b blocker) Fetch(ctx context.Context, _ string, _ []string, _ time.Time) ([]Rate, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestFetchFallbackBound: a provider that never returns on its own is cut off by the service's own
// context bound, and logged as a transport failure.
func TestFetchFallbackBound(t *testing.T) {
	old := fetchBound
	fetchBound = 100 * time.Millisecond
	t.Cleanup(func() { fetchBound = old })
	e := newEnv(t, func(cfg *Config, _ *http.Client) { cfg.Providers = append(cfg.Providers, blocker{"stuck"}) })
	start := time.Now()
	e.svc.fetch(context.Background())
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the pass took %v", d)
	}
	if l := e.log.lines(t, logFetchFailed); len(l) != 1 || l[0].Provider != "stuck" || l[0].Class != "transport" {
		t.Fatalf("fetch failures %+v", l)
	}
	if old != FetchTimeout+time.Second {
		t.Fatalf("fetchBound = %v, want FetchTimeout + 1 s", old)
	}
}

// TestFetchFailureLoggedOncePerRun: a provider's failure is logged at Warn on the first pass of a run of failures and
// at Debug on the passes after it; once the provider has answered again, its next failure is at Warn again. Each
// provider has its own run.
func TestFetchFailureLoggedOncePerRun(t *testing.T) {
	e := newEnv(t, nil)
	e.svc.log = slog.New(slog.NewJSONHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	levels := func() map[string][]string {
		out := map[string][]string{}
		for _, l := range e.log.lines(t, logFetchFailed) {
			if l.Class != "status" || l.Status == nil || *l.Status != 500 {
				t.Fatalf("line %s", l.raw)
			}
			out[l.Provider] = append(out[l.Provider], l.Level)
		}
		return out
	}
	e.fake.Fail("kraken", 500)
	for i := 0; i < 3; i++ {
		e.svc.fetch(context.Background())
	}
	e.fake.Fail("coinbase", 500)
	e.svc.fetch(context.Background())
	e.svc.fetch(context.Background())
	got := levels()
	if want := []string{"WARN", "DEBUG", "DEBUG", "DEBUG", "DEBUG"}; !equalStrings(got["kraken"], want) {
		t.Fatalf("kraken: %v, want %v", got["kraken"], want)
	}
	if want := []string{"WARN", "DEBUG"}; !equalStrings(got["coinbase"], want) {
		t.Fatalf("coinbase: %v, want %v", got["coinbase"], want)
	}
	// Kraken recovers for one pass, then fails again: Warn once more. Coinbase's run goes on.
	e.fake.Fail("kraken", 0)
	e.svc.fetch(context.Background())
	e.fake.Fail("kraken", 500)
	e.svc.fetch(context.Background())
	e.svc.fetch(context.Background())
	got = levels()
	if want := []string{"WARN", "DEBUG", "DEBUG", "DEBUG", "DEBUG", "WARN", "DEBUG"}; !equalStrings(got["kraken"], want) {
		t.Fatalf("kraken after recovering: %v, want %v", got["kraken"], want)
	}
	if want := []string{"WARN", "DEBUG", "DEBUG", "DEBUG", "DEBUG"}; !equalStrings(got["coinbase"], want) {
		t.Fatalf("coinbase: %v, want %v", got["coinbase"], want)
	}
	// A pass cut short by its context logs nothing and leaves the runs as they were.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.svc.fetch(ctx)
	if n := len(e.log.lines(t, logFetchFailed)); n != 12 {
		t.Fatalf("%d lines after a cancelled pass, want 12", n)
	}
	e.noSentinel(t)
}

// TestServiceTwoAssets: with BTC and XMR configured, each asset is fetched and quoted on its own, from the providers
// that quote it; Coinbase's and Yadio's ErrAsset for XMR is no failure; the price of XMR is in piconero.
func TestServiceTwoAssets(t *testing.T) {
	e := newEnv(t, func(cfg *Config, c *http.Client) {
		cfg.Assets = []string{AssetBTC, AssetXMR}
		cfg.Providers = []Provider{Kraken(c), Coinbase(c), Yadio(c), Bitfinex(c), Custom(c, customURL)}
	})
	for p, usd := range map[string]string{"kraken": "150.2", "bitfinex": "150.4", "custom": "150"} {
		e.fake.SetAsset(p, "XMR", "USD", usd)
	}
	e.fake.SetAsset("kraken", "XMR", "EUR", "130")
	e.fake.SetAsset("custom", "XMR", "EUR", "130.4")
	for _, pair := range [][2]string{{"XMR", "USD"}, {"XMR", "EUR"}, {"BTC", "USD"}, {"BTC", "EUR"}} {
		if !e.svc.Reachable(pair[0], pair[1]) {
			t.Fatalf("%s/%s not reachable", pair[0], pair[1])
		}
	}
	e.svc.fetch(context.Background())
	q, err := e.svc.Quote("XMR", "USD")
	if err != nil || !equalStrings(names(q.Sources), []string{"bitfinex", "custom", "kraken"}) || q.Rate.Cmp(dec(t, "150.2")) != 0 || q.Asset != "XMR" {
		t.Fatalf("XMR/USD: %+v, %v", q, err)
	}
	q, err = e.svc.Quote("XMR", "EUR")
	if err != nil || !equalStrings(names(q.Sources), []string{"custom", "kraken"}) || q.Rate.Cmp(dec(t, "130.2")) != 0 {
		t.Fatalf("XMR/EUR: %+v, %v", q, err)
	}
	q, err = e.svc.Quote("BTC", "USD")
	if err != nil || !equalStrings(names(q.Sources), []string{"coinbase", "custom", "kraken", "yadio"}) || q.Rate.Cmp(dec(t, "76005")) != 0 {
		t.Fatalf("BTC/USD: %+v, %v", q, err)
	}
	if q, err := e.svc.Quote("BTC", "EUR"); err != nil || len(q.Sources) != 4 || q.Rate.Cmp(dec(t, "67000")) != 0 {
		t.Fatalf("BTC/EUR: %+v, %v", q, err)
	}
	pr, err := e.svc.Price("XMR", dec(t, "100"), "USD")
	want, err2 := Piconero(dec(t, "100"), dec(t, "150.2"), 100)
	if err != nil || err2 != nil || pr.Amount != want || want != 672_436_750_999 || pr.Quote.Asset != "XMR" || pr.MarginBps != 100 {
		t.Fatalf("Price XMR: %+v, %v; want %d piconero", pr, err, want)
	}
	pr, err = e.svc.Price("BTC", dec(t, "10"), "USD")
	if want, _ := Sats(dec(t, "10"), dec(t, "76005"), 100); err != nil || pr.Amount != want || pr.Quote.Asset != "BTC" {
		t.Fatalf("Price BTC: %+v, %v", pr, err)
	}
	// Coinbase and Yadio were asked for XMR and said ErrAsset, with no request; that is no failure and logs nothing.
	for p, n := range map[string]int{"kraken": 2, "bitfinex": 2, "custom": 2, "coinbase": 1, "yadio": 1} {
		if got := e.fake.Hits(p); got != n {
			t.Errorf("%s: %d requests, want %d", p, got, n)
		}
	}
	if s := e.log.String(); s != "" {
		t.Fatalf("a good first pass logged:\n%s", s)
	}
	// Bitfinex was asked for each asset in the currencies of its table only (it had no BTC rate to give).
	if reqs := e.fake.Requests("bitfinex"); len(reqs) != 2 || reqs[0] != "symbols=tBTCUSD,tBTCEUR" || reqs[1] != "symbols=tXMRUSD" {
		t.Fatalf("bitfinex requests %q", reqs)
	}
}

// TestUnreachablePairIsSilent: a pair that the configured providers can never quote to MinSources is
// never an ALERT and quotes ErrUnreachable; a reachable pair in an outage still alerts.
func TestUnreachablePairIsSilent(t *testing.T) {
	setup := func(t *testing.T) *env {
		e := newEnv(t, func(cfg *Config, c *http.Client) {
			cfg.Assets = []string{AssetBTC, AssetXMR}
			cfg.Providers = []Provider{Kraken(c), Bitfinex(c)}
		})
		e.fake.Set("bitfinex", "USD", "76010")
		e.fake.Set("bitfinex", "EUR", "67000")
		e.fake.SetAsset("kraken", "XMR", "USD", "150.2")
		e.fake.SetAsset("kraken", "XMR", "EUR", "130")
		e.fake.SetAsset("bitfinex", "XMR", "USD", "150.4")
		return e
	}
	e := setup(t)
	for pair, want := range map[[2]string]bool{
		{"XMR", "EUR"}: false, {"XMR", "USD"}: true, {"BTC", "EUR"}: true, {"BTC", "USD"}: true,
		{"XMR", "GBP"}: false, {"ETH", "USD"}: false, {"BTC", "GBP"}: false,
	} {
		if got := e.svc.Reachable(pair[0], pair[1]); got != want {
			t.Errorf("Reachable(%s, %s) = %v, want %v", pair[0], pair[1], got, want)
		}
	}
	for range 3 {
		e.svc.fetch(context.Background())
		e.clk.Advance(10 * time.Second)
	}
	for _, msg := range []string{logUnavailable, logAvailable} {
		for _, l := range e.log.lines(t, msg) {
			if l.Asset == "XMR" && l.Currency == "EUR" {
				t.Fatalf("a line for the unreachable pair: %s", l.raw)
			}
		}
	}
	if s := e.log.String(); s != "" {
		t.Fatalf("a working setup logged:\n%s", s)
	}
	// Not fetched: Kraken's XMR request names USD only, and the cache holds no XMR/EUR rate.
	for _, q := range e.fake.Requests("kraken") {
		if strings.Contains(q, "XMREUR") || strings.Contains(q, "XMR") && !strings.HasSuffix(q, "pair=XMRUSD") {
			t.Fatalf("kraken asked %q", q)
		}
	}
	if rs := e.svc.rates("XMR", "EUR"); len(rs) != 0 {
		t.Fatalf("the cache holds the unreachable pair: %+v", rs)
	}
	if _, err := e.svc.Quote("XMR", "EUR"); !errors.Is(err, ErrUnreachable) || errors.Is(err, ErrTooFew) {
		t.Fatalf("Quote XMR/EUR: %v, want ErrUnreachable", err)
	}
	if _, err := e.svc.Price("XMR", dec(t, "10"), "EUR"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Price XMR/EUR: %v, want ErrUnreachable", err)
	}
	if ErrUnreachable.Error() != "rates: no configured set of providers quotes this pair" {
		t.Fatalf("ErrUnreachable %q", ErrUnreachable)
	}
	if q, err := e.svc.Quote("XMR", "USD"); err != nil || !equalStrings(names(q.Sources), []string{"bitfinex", "kraken"}) {
		t.Fatalf("XMR/USD: %+v, %v", q, err)
	}
	if _, err := e.svc.Quote("BTC", "EUR"); err != nil {
		t.Fatalf("BTC/EUR, the same currency for BTC: %v", err)
	}
	// An unconfigured asset or currency keeps its own error.
	if _, err := e.svc.Quote("XMR", "GBP"); !errors.Is(err, ErrCurrency) {
		t.Fatalf("XMR/GBP: %v, want ErrCurrency", err)
	}
	if _, err := e.svc.Quote("ETH", "USD"); !errors.Is(err, ErrAsset) {
		t.Fatalf("ETH/USD: %v, want ErrAsset", err)
	}

	// Bitfinex fails from the start: XMR/USD is reachable and too few, and alerts; XMR/EUR still says nothing.
	e = setup(t)
	e.fake.Fail("bitfinex", 500)
	for range 3 {
		e.svc.fetch(context.Background())
	}
	if _, err := e.svc.Quote("XMR", "USD"); !errors.Is(err, ErrTooFew) {
		t.Fatalf("XMR/USD in an outage: %v, want ErrTooFew", err)
	}
	if _, err := e.svc.Quote("XMR", "EUR"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("XMR/EUR in an outage: %v, want ErrUnreachable", err)
	}
	alerts := e.log.lines(t, logUnavailable)
	var xmr []logLine
	for _, a := range alerts {
		if a.Asset == "XMR" {
			xmr = append(xmr, a)
		}
	}
	if len(xmr) != 1 || xmr[0].Currency != "USD" || xmr[0].Reason != "too_few" || !equalStrings(xmr[0].Providers, []string{"kraken"}) {
		t.Fatalf("XMR ALERTs %+v, want one for USD", xmr)
	}
	if len(alerts) != 3 { // BTC/USD, BTC/EUR and XMR/USD, once each
		t.Fatalf("%d ALERTs, want 3: %+v", len(alerts), alerts)
	}
	e.noSentinel(t)
}

// TestReachableNeedsMinSources: a pair is reachable when at least MinSources configured providers quote it, the custom
// API counting for every pair and a provider of an unknown name for none.
func TestReachableNeedsMinSources(t *testing.T) {
	c := NewClient(nil)
	for _, x := range []struct {
		providers []Provider
		min       int
		asset     string
		currency  string
		want      bool
	}{
		{[]Provider{Kraken(c), Custom(c, customURL)}, 2, AssetXMR, "EUR", true},
		{[]Provider{Kraken(c), Custom(c, customURL)}, 2, AssetXMR, "CHF", false},
		{[]Provider{Kraken(c), Custom(c, customURL)}, 3, AssetXMR, "USD", false},
		{[]Provider{Kraken(c), Bitfinex(c), Custom(c, customURL)}, 3, AssetXMR, "USD", true},
		{[]Provider{Coinbase(c), Yadio(c)}, 2, AssetXMR, "USD", false},
		{[]Provider{Coinbase(c), Yadio(c)}, 2, AssetBTC, "CHF", true},
		{[]Provider{Kraken(c)}, 1, AssetXMR, "USD", true},
		{[]Provider{Kraken(c)}, 1, AssetXMR, "CHF", false},
		{[]Provider{Custom(c, customURL)}, 1, AssetXMR, "CHF", true},
		{[]Provider{blocker{"stuck"}, Kraken(c)}, 2, AssetBTC, "USD", false},
	} {
		s, err := New(Config{
			Providers: x.providers, Assets: []string{AssetBTC, AssetXMR}, Currencies: []string{"USD", "EUR", "CHF"},
			Interval: time.Second, Policy: Policy{MinSources: x.min, MaxSpreadBps: 200, MaxAge: 300 * time.Second}, Now: time.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Reachable(x.asset, x.currency); got != x.want {
			var ns []string
			for _, p := range x.providers {
				ns = append(ns, p.Name())
			}
			t.Errorf("%v, MinSources %d: Reachable(%s, %s) = %v, want %v", ns, x.min, x.asset, x.currency, got, x.want)
		}
	}
}

// TestUnreachableAssetNotRequested: an asset none of whose configured currencies is reachable is asked
// of no provider, and a provider's answer for an unreachable pair is not cached.
func TestUnreachableAssetNotRequested(t *testing.T) {
	// Kraken, Coinbase and Yadio with USD: XMR/USD has Kraken alone, below MinSources 2.
	e := newEnv(t, func(cfg *Config, c *http.Client) {
		cfg.Assets = []string{AssetBTC, AssetXMR}
		cfg.Currencies = []string{"USD"}
		cfg.Providers = []Provider{Kraken(c), Coinbase(c), Yadio(c)}
	})
	e.fake.SetAsset("kraken", "XMR", "USD", "150")
	for range 3 {
		e.svc.fetch(context.Background())
	}
	for _, q := range e.fake.Requests("kraken") {
		if q != "assetVersion=1&pair=XBTUSD" {
			t.Fatalf("kraken asked %q", q)
		}
	}
	if n := e.fake.Hits("kraken"); n != 3 {
		t.Fatalf("kraken: %d requests, want 3 (BTC only)", n)
	}
	if rs := e.svc.rates("XMR", "USD"); len(rs) != 0 {
		t.Fatalf("XMR cached: %+v", rs)
	}

	// Coinbase, Yadio and the custom API with USD and EUR: only the custom API quotes XMR, so it is asked for BTC only.
	e = newEnv(t, func(cfg *Config, c *http.Client) {
		cfg.Assets = []string{AssetBTC, AssetXMR}
		cfg.Providers = []Provider{Coinbase(c), Yadio(c), Custom(c, customURL)}
	})
	e.fake.SetAsset("custom", "XMR", "USD", "150")
	e.svc.fetch(context.Background())
	e.svc.fetch(context.Background())
	if reqs := e.fake.Requests("custom"); len(reqs) != 2 || reqs[0] != "asset=BTC" || reqs[1] != "asset=BTC" {
		t.Fatalf("custom asked %q", reqs)
	}
	if s := e.log.String(); s != "" {
		t.Fatalf("logged:\n%s", s)
	}

	// Kraken and the custom API with USD and CHF, MinSources 2: XMR/USD is reachable, XMR/CHF is not (Kraken has no
	// XMR/CHF). The custom API answers XMR in both; only USD is kept.
	e = newEnv(t, func(cfg *Config, c *http.Client) {
		cfg.Assets = []string{AssetBTC, AssetXMR}
		cfg.Currencies = []string{"USD", "CHF"}
		cfg.Providers = []Provider{Kraken(c), Custom(c, customURL)}
	})
	e.fake.SetAsset("kraken", "XMR", "USD", "150")
	e.fake.SetAsset("custom", "XMR", "USD", "150.2")
	e.fake.SetAsset("custom", "XMR", "CHF", "130")
	e.svc.fetch(context.Background())
	if rs := e.svc.rates("XMR", "CHF"); len(rs) != 0 {
		t.Fatalf("XMR/CHF cached: %+v", rs)
	}
	if q, err := e.svc.Quote("XMR", "USD"); err != nil || len(q.Sources) != 2 {
		t.Fatalf("XMR/USD: %+v, %v", q, err)
	}
	if _, err := e.svc.Quote("XMR", "CHF"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("XMR/CHF: %v", err)
	}
}

// TestFetchFailureRunsPerAsset: a provider's run of failures is kept per asset, and each line names the asset. An
// operator's BTC-only custom API answers BTC to an XMR request (ErrShape, every pass); a later BTC outage of the same
// API still gets its Warn.
func TestFetchFailureRunsPerAsset(t *testing.T) {
	e := newEnv(t, func(cfg *Config, c *http.Client) {
		cfg.Assets = []string{AssetBTC, AssetXMR}
	})
	e.svc.log = slog.New(slog.NewJSONHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.fake.SetAsset("kraken", "XMR", "USD", "150")
	e.fake.SetAsset("kraken", "XMR", "EUR", "130")
	e.fake.Body("custom", []byte(fmt.Sprintf(`{"asset":"BTC","rates":{"USD":"76020","EUR":"67000"},"timestamp":%d}`, now2030.Unix())))
	type key struct{ provider, asset string }
	levels := func() map[key][]string {
		out := map[key][]string{}
		for _, l := range e.log.lines(t, logFetchFailed) {
			if l.Asset == "" {
				t.Fatalf("a failure line without its asset: %s", l.raw)
			}
			out[key{l.Provider, l.Asset}] = append(out[key{l.Provider, l.Asset}], l.Level+" "+l.Class)
		}
		return out
	}
	for range 3 {
		e.svc.fetch(context.Background())
	}
	got := levels()
	if want := []string{"WARN shape", "DEBUG shape", "DEBUG shape"}; len(got) != 1 || !equalStrings(got[key{"custom", "XMR"}], want) {
		t.Fatalf("XMR's failures %v, want %v", got, want)
	}
	if _, err := e.svc.Quote("BTC", "USD"); err != nil {
		t.Fatalf("BTC/USD: %v", err)
	}
	// The custom API goes down: its BTC fetch fails too, and that run starts with a Warn of its own.
	e.fake.Fail("custom", 500)
	e.svc.fetch(context.Background())
	e.svc.fetch(context.Background())
	got = levels()
	if want := []string{"WARN status", "DEBUG status"}; !equalStrings(got[key{"custom", "BTC"}], want) {
		t.Fatalf("BTC's failures %v, want %v", got[key{"custom", "BTC"}], want)
	}
	if want := []string{"WARN shape", "DEBUG shape", "DEBUG shape", "DEBUG status", "DEBUG status"}; !equalStrings(got[key{"custom", "XMR"}], want) {
		t.Fatalf("XMR's failures %v, want %v", got[key{"custom", "XMR"}], want)
	}
	e.noSentinel(t)
}
