//go:build qualify

package qualify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/rates"
	"github.com/satbooth/satbooth/rates/ratestest"
	"github.com/satbooth/satbooth/service"
	"github.com/satbooth/satbooth/store"
)

// The fiat pricing phases (plan 7). Every rate provider is the fake rate server (rates/ratestest) in the test process,
// reached through the service's rate transport, so no live provider is ever called: the fake answers for
// api.kraken.com, api.coinbase.com, api.yadio.io and rates.example.invalid (the custom rate API) and refuses every other
// host. Each phase ends with the safety check, and an unexpected ERROR or WARN line fails it.

const (
	fiatCustomURL  = "https://rates.example.invalid/v1"
	fiatMargin     = "1.5"
	fiatInterval   = 10 * time.Second // fetch_seconds
	fiatMaxAge     = 60 * time.Second // max_age_seconds
	fiatQuoteBound = 30 * time.Second
	fiatRetryAfter = "10" // the API's Retry-After of a rate_unavailable answer
	// fiatTooBig is a price no rail can hold at any rate the fake serves: 20 nines of USD is far above 21 million
	// bitcoin, so with a rate it is refused with the Lightning rail's code, and without one it is rate_unavailable.
	fiatTooBig = "99999999999999999999"

	// The rate service's log lines (rates/service.go): the checks compare them exactly, so a change of wording fails here.
	alFiat          = "ALERT: fiat pricing is unavailable for a currency"
	lnFiatAvailable = "fiat pricing is available for a currency"
	lnFetchFailed   = "rate fetch failed"
)

// fiatRates is the rates block of every fiat phase: the three built-in providers and the custom API, USD and EUR, a
// 1.5 % margin, a fetch every 10 s and rates that count for 60 s; min_sources 2 and max_spread_percent 2 (the
// defaults). Every field is set: a block built in code gets no defaults from service.Run.
func fiatRates() *service.RatesConfig {
	return &service.RatesConfig{Providers: []string{"kraken", "coinbase", "yadio"}, CustomURL: fiatCustomURL,
		Currencies: []string{"USD", "EUR"}, MarginPercent: fiatMargin, MinSources: 2, MaxSpreadPercent: "2",
		MaxAgeSeconds: 60, FetchSeconds: 10}
}

// fiatTable is currency → provider → rate, as decimal text.
type fiatTable map[string]map[string]string

// fiatBase are the fake's rates; fiatUp are the same times 1.1.
var (
	fiatBase = fiatTable{
		"USD": {"kraken": "76000", "coinbase": "76100", "yadio": "75950", "custom": "76050"},
		"EUR": {"kraken": "67000", "coinbase": "67100", "yadio": "66950", "custom": "67050"},
	}
	fiatUp = fiatTable{
		"USD": {"kraken": "83600", "coinbase": "83710", "yadio": "83545", "custom": "83655"},
		"EUR": {"kraken": "73700", "coinbase": "73810", "yadio": "73645", "custom": "73755"},
	}
)

// seedFiat gives every provider of the table its rate in each currency.
func seedFiat(fake *ratestest.Server, tab fiatTable) {
	for cur, byProvider := range tab {
		for p, v := range byProvider {
			fake.Set(p, cur, v)
		}
	}
}

// fiatMedian is the rates package's median of the given providers' rates (all of them when none is named).
func fiatMedian(t *testing.T, byProvider map[string]string, only ...string) rates.Decimal {
	t.Helper()
	var rs []rates.Rate
	for p, v := range byProvider {
		if len(only) > 0 && !slices.Contains(only, p) {
			continue
		}
		d, err := rates.ParseDecimal(v)
		if err != nil {
			t.Fatal(err)
		}
		rs = append(rs, rates.Rate{Provider: p, Value: d})
	}
	return rates.Median(rs)
}

// fiatSats is rates.Sats of price at rate with the phases' margin.
func fiatSats(t *testing.T, price string, rate rates.Decimal) int64 {
	t.Helper()
	p, err := rates.ParseDecimal(price)
	if err != nil {
		t.Fatal(err)
	}
	m, err := rates.ParsePercent(fiatMargin)
	if err != nil {
		t.Fatal(err)
	}
	n, err := rates.Sats(p, rate, m)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// newFiatEnv is a Lightning environment with fiat pricing, at /state/lightning/<name>, and its fake rate server holding
// the base rates.
func newFiatEnv(t *testing.T, name string) (*lnEnv, *ratestest.Server) {
	t.Helper()
	fake := ratestest.New(t)
	seedFiat(fake, fiatBase)
	e := newEnvAt(t, name, filepath.Join(stateDir(t), "lightning", name), "", nil, fiatRates(), nil)
	e.rateRT = fake.Transport()
	return e, fake
}

// pricedOrder polls a priced POST until it creates an order priced from exactly sources rates. 503 rate_unavailable
// is retried with the same ref. The cache fills one provider at a time, so a POST in the middle of a pass can be
// priced from fewer rates: such an order is kept (logged, returned in extra for the safety check) and the next try
// uses a new ref. Any other answer fails the phase.
func (e *lnEnv) pricedOrder(prefix, price, currency string, sources int, expiresIn int64, bound time.Duration) (o orderJSON, extra []orderJSON) {
	t := e.t
	t.Helper()
	ref := prefix + "-" + randHex(t)[:12]
	poll(t, "a priced order from "+fmt.Sprint(sources)+" rates", bound, 200*time.Millisecond, func() (bool, error) {
		got, status, code := e.createFiatOrder(ref, price, currency, nil, expiresIn)
		switch {
		case status == 503 && code == "rate_unavailable":
			return false, errors.New("503 rate_unavailable")
		case status != 201:
			t.Fatalf("priced order: status %d, code %q, want 201", status, code)
		case got.Pricing == nil:
			t.Fatal("a priced order has no pricing")
		case len(got.Pricing.Rates["BTC"].Sources) != sources:
			n := len(got.Pricing.Rates["BTC"].Sources)
			t.Logf("OBSERVED a priced order was created from %d rates in the middle of a pass (want %d); kept unpaid, retrying with a new ref", n, sources)
			extra = append(extra, got)
			ref = prefix + "-" + randHex(t)[:12]
			return false, fmt.Errorf("priced from %d rates", n)
		}
		o = got
		return true, nil
	})
	if o.State != "new" || o.MerchantRef != ref || !strings.HasPrefix(o.CheckoutURL, "http://"+publicAddr+"/c/") {
		t.Fatalf("the priced order looks wrong: state %q", o.State)
	}
	return o, extra
}

// checkPricing checks an order's pricing: the price text verbatim, the currency, the BTC rate (the only asset of these
// orders), the margin, one source per provider of byProvider (sorted by name, each at its rate on the fake), all times
// RFC 3339, and that each rail's amount is re-derived from the pricing itself (the check an auditor runs).
func checkPricing(t *testing.T, o orderJSON, price, currency, rate string, byProvider map[string]string) {
	t.Helper()
	p := o.Pricing
	if p == nil {
		t.Fatalf("order %s has no pricing", o.ID)
	}
	btc, ok := p.Rates["BTC"]
	if !ok || len(p.Rates) != 1 || o.Monero != nil || o.MoneroPiconero != 0 {
		t.Fatalf("order %s: pricing rates of %d assets (BTC %v), Monero %v %d; want BTC alone", o.ID, len(p.Rates), ok, o.Monero != nil, o.MoneroPiconero)
	}
	if p.Price != price || p.Currency != currency || btc.Rate != rate || p.MarginPercent != fiatMargin {
		t.Fatalf("order %s pricing: price %q currency %q rate %q margin %q, want %q %q %q %q", o.ID, p.Price, p.Currency, btc.Rate, p.MarginPercent, price, currency, rate, fiatMargin)
	}
	if _, err := time.Parse(time.RFC3339, p.QuotedAt); err != nil {
		t.Fatalf("order %s: quoted_at %q", o.ID, p.QuotedAt)
	}
	if len(btc.Sources) != len(byProvider) {
		t.Fatalf("order %s: %d sources, want %d", o.ID, len(btc.Sources), len(byProvider))
	}
	// The audit: the amounts follow from the order's own pricing, Sats of the price at the rate with the margin.
	pd, err1 := rates.ParseDecimal(p.Price)
	r, err2 := rates.ParseDecimal(btc.Rate)
	m, err3 := rates.ParsePercent(p.MarginPercent)
	if err := errors.Join(err1, err2, err3); err != nil {
		t.Fatalf("order %s: the pricing does not parse: %v", o.ID, shownErr(err))
	}
	sats, err := rates.Sats(pd, r, m)
	if err != nil {
		t.Fatalf("order %s: Sats of its pricing: %v", o.ID, shownErr(err))
	}
	if o.Lightning == nil && o.Onchain == nil || o.Lightning != nil && o.Msat != sats*1000 || o.Onchain != nil && o.OnchainSat != sats {
		t.Fatalf("order %s: lightning_msat %d, onchain_sat %d; its pricing gives %d sat", o.ID, o.Msat, o.OnchainSat, sats)
	}
	for i, s := range btc.Sources {
		if i > 0 && btc.Sources[i-1].Provider >= s.Provider {
			t.Fatalf("order %s: sources are not sorted by distinct provider names", o.ID)
		}
		if want, ok := byProvider[s.Provider]; !ok || s.Rate != want {
			t.Fatalf("order %s: source %q at %q, want the fake's %q", o.ID, s.Provider, s.Rate, want)
		}
		if _, err := time.Parse(time.RFC3339, s.At); err != nil {
			t.Fatalf("order %s: source time %q", o.ID, s.At)
		}
	}
}

// hookBody is a delivery's body, exactly its members: pricing (times in Unix seconds) is present for a priced order.
type hookBody struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Seq       int    `json:"seq"`
	OrderID   string `json:"order_id"`
	CreatedAt int64  `json:"created_at"`
	Pricing   *struct {
		Price         string `json:"price"`
		Currency      string `json:"currency"`
		MarginPercent string `json:"margin_percent"`
		QuotedAt      int64  `json:"quoted_at"`
		Rates         map[string]struct {
			Rate    string `json:"rate"`
			Sources []struct {
				Provider string `json:"provider"`
				Rate     string `json:"rate"`
				At       int64  `json:"at"`
			} `json:"sources"`
		} `json:"rates"`
	} `json:"pricing"`
}

// checkHookPricing decodes the order's first delivery of typ and checks its pricing against the API's.
func (e *lnEnv) checkHookPricing(o orderJSON, typ string) {
	t := e.t
	t.Helper()
	raw := e.rcv.bodyOf(o.ID, typ)
	var b hookBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		t.Fatalf("the %s body of order %s: unexpected shape: %v", typ, o.ID, shownErr(err))
	}
	ap, hp := o.Pricing, b.Pricing
	if ap == nil {
		t.Fatalf("order %s has no pricing to compare the %s body with", o.ID, typ)
	}
	if hp == nil {
		t.Fatalf("the %s body of priced order %s has no pricing", typ, o.ID)
	}
	q, _ := time.Parse(time.RFC3339, ap.QuotedAt)
	if hp.Price != ap.Price || hp.Currency != ap.Currency || hp.MarginPercent != ap.MarginPercent || hp.QuotedAt != q.Unix() ||
		len(hp.Rates) != len(ap.Rates) {
		t.Fatalf("the %s body's pricing differs from the API's: price %q currency %q, %d rates", typ, hp.Price, hp.Currency, len(hp.Rates))
	}
	for asset, ar := range ap.Rates {
		hr, ok := hp.Rates[asset]
		if !ok || hr.Rate != ar.Rate || len(hr.Sources) != len(ar.Sources) {
			t.Fatalf("the %s body's %s rate differs from the API's: %v, rate %q, %d sources", typ, asset, ok, hr.Rate, len(hr.Sources))
		}
		for i, s := range hr.Sources {
			at, _ := time.Parse(time.RFC3339, ar.Sources[i].At)
			if s.Provider != ar.Sources[i].Provider || s.Rate != ar.Sources[i].Rate || s.At != at.Unix() {
				t.Fatalf("the %s body's %s source %d differs from the API's", typ, asset, i)
			}
		}
	}
	btc := hp.Rates["BTC"]
	t.Logf("OBSERVED fiat %s body of order %s: pricing.rates.BTC.rate %q, price %q %s, %d sources", typ, o.ID, btc.Rate, hp.Price, hp.Currency, len(btc.Sources))
}

// ---- the rate service's log ----

// fiatRec is one record of the service's log, with the attributes of the rate service's lines.
type fiatRec struct {
	Level     string   `json:"level"`
	Msg       string   `json:"msg"`
	Asset     string   `json:"asset"`
	Currency  string   `json:"currency"`
	Reason    string   `json:"reason"`
	Providers []string `json:"providers"`
	Low       string   `json:"low"`
	High      string   `json:"high"`
	Provider  string   `json:"provider"`
	Class     string   `json:"class"`
	Status    int      `json:"status"`
}

func fiatRecords(t *testing.T, logs string) []fiatRec {
	t.Helper()
	var out []fiatRec
	for _, l := range strings.Split(logs, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var r fiatRec
		if err := json.Unmarshal([]byte(l), &r); err != nil || r.Msg == "" {
			t.Fatalf("a log line that is not a JSON record: %s", l)
		}
		out = append(out, r)
	}
	return out
}

// stateLines are the rate service's state lines for one currency, in order: the ALERT (as its reason) and the
// available line (as "available").
func stateLines(recs []fiatRec, currency string) (states []string, lines []fiatRec) {
	for _, r := range recs {
		if r.Currency != currency || r.Asset != "BTC" {
			continue
		}
		switch r.Msg {
		case alFiat:
			states, lines = append(states, r.Reason), append(lines, r)
		case lnFiatAvailable:
			states, lines = append(states, "available"), append(lines, r)
		}
	}
	return states, lines
}

// waitStates polls the log until both currencies' state lines read exactly usd and eur. A line beyond them fails
// at once: a state is logged only on a change.
func waitStates(t *testing.T, s *svcRun, what string, bound time.Duration, usd, eur []string) []fiatRec {
	t.Helper()
	var recs []fiatRec
	poll(t, what, bound, 250*time.Millisecond, func() (bool, error) {
		recs = fiatRecords(t, s.logs.String())
		gu, _ := stateLines(recs, "USD")
		ge, _ := stateLines(recs, "EUR")
		if len(gu) > len(usd) || len(ge) > len(eur) || !slices.Equal(gu, usd[:len(gu)]) || !slices.Equal(ge, eur[:len(ge)]) {
			t.Fatalf("state lines USD %v, EUR %v; want USD %v, EUR %v", gu, ge, usd, eur)
		}
		if slices.Equal(gu, usd) && slices.Equal(ge, eur) {
			return true, nil
		}
		return false, fmt.Errorf("state lines USD %v, EUR %v", gu, ge)
	})
	return recs
}

// checkFiatLog checks a whole log: every line a JSON record; at ERROR only the fiat ALERT; at WARN only rate fetch
// failed, each naming a provider of failing with the class status and exactly the HTTP status the fake fails it with;
// no startup failure; and no byte of a provider's body (ratestest.Sentinel is in every failing answer of the fake).
// failed counts the WARN lines per provider: one per run of failures (the passes after the first log at DEBUG, which
// the service's INFO log leaves out).
func checkFiatLog(t *testing.T, logs string, failing map[string]int) (failed map[string]int) {
	t.Helper()
	if strings.Contains(logs, ratestest.Sentinel) {
		t.Errorf("a provider's body reached the service's log")
	}
	failed = map[string]int{}
	for _, r := range fiatRecords(t, logs) {
		switch {
		case r.Level == "ERROR" && r.Msg == alFiat:
		case r.Level == "WARN" && r.Msg == alWalletBehind:
			t.Logf("OBSERVED the WARN %q was logged (tolerated)", alWalletBehind)
		case r.Level == "WARN" && r.Msg == lnFetchFailed:
			if want, ok := failing[r.Provider]; !ok || r.Class != "status" || r.Status != want {
				t.Errorf("rate fetch failed: provider %q class %q status %d, want the class status and the fake's %d", r.Provider, r.Class, r.Status, want)
			}
			failed[r.Provider]++
		case r.Level == "ERROR" || r.Level == "WARN" || strings.Contains(r.Msg, "ALERT") || strings.Contains(r.Msg, "startup failed") || strings.Contains(r.Msg, "unexpectedly"):
			t.Errorf("unexpected log line: %s %s", r.Level, r.Msg)
		}
	}
	return failed
}

// ---- the phases ----

// TestFiatPricing: a priced order converts at the median of the four rates with the margin, is paid over Lightning,
// and its webhook carries the pricing. An explicit order still works. The rates move by 10 % and the service restarts
// on the same data: the paid order keeps its rate and amount, new orders price at the new rates in USD and EUR, and
// the on-chain rail, absent here, is refused.
func TestFiatPricing(t *testing.T) {
	e, fake := newFiatEnv(t, "fiat")
	began := time.Now()
	s := e.start()
	o, extra := e.pricedOrder("fiat-1", "10.00", "USD", 4, 3600, fiatQuoteBound)
	t.Logf("MEASURED fiat=quote start_to_first_quote_seconds=%.2f (from the start of the service; the first fetch runs at once)", time.Since(began).Seconds())
	rate := fiatMedian(t, fiatBase["USD"])
	if rate.String() != "76025" || fiatSats(t, "10.00", rate)*1000 != 13_351_000 {
		t.Fatalf("the expected values disagree with the test's own figures: median %s, %d sat", rate, fiatSats(t, "10.00", rate))
	}
	if o.Msat != 13_351_000 || o.OnchainSat != 0 || o.Onchain != nil || o.Lightning == nil {
		t.Fatalf("the priced order: lightning_msat %d, onchain_sat %d, want 13351000 and 0", o.Msat, o.OnchainSat)
	}
	checkPricing(t, o, "10.00", "USD", "76025", fiatBase["USD"])
	tok := tokenOf(o)
	if code, page := e.page(tok); code != 200 || !strings.Contains(page, "10.00 USD") || !strings.Contains(page, `href="https://yadio.io"`) {
		t.Fatalf("the priced order's page: status %d, price shown %v, Yadio credit %v", code, strings.Contains(page, "10.00 USD"), strings.Contains(page, "yadio.io"))
	}
	invoice := e.pressPay(tok)
	paidAt := time.Now()
	e.customerPays(invoice)
	d := e.waitOrder(o.ID, "paid", paidAt, paidBound)
	t.Logf("MEASURED fiat=pay pay_to_order_paid_seconds=%.2f", d.Seconds())
	e.waitHook(o.ID, store.EventPaid, hookBound)
	e.checkHookPricing(o, store.EventPaid)

	explicit := e.createOrder("fiat-explicit-"+randHex(t)[:12], orderMsat, 3600)
	if explicit.Pricing != nil || explicit.Msat != orderMsat {
		t.Fatalf("the explicit order: pricing %v, lightning_msat %d", explicit.Pricing != nil, explicit.Msat)
	}
	before := e.order(o.ID)
	e.settleQuiet()
	s.stop()
	noWarnings(t, s.logs.String())

	// Every rate times 1.1, set while the service is down (set while it runs, a pass could read half the new rates,
	// see them disagree and log an ALERT), then a new service on the same data.
	seedFiat(fake, fiatUp)
	s = e.start()
	after := e.order(o.ID)
	if after.Pricing == nil || before.Pricing == nil {
		t.Fatalf("the paid order has no pricing: before the restart %v, after %v", before.Pricing != nil, after.Pricing != nil)
	}
	if after.State != "paid" || after.Msat != before.Msat || after.OnchainSat != 0 || !reflect.DeepEqual(after.Pricing, before.Pricing) || after.Pricing.Rates["BTC"].Rate != "76025" {
		t.Fatalf("after the restart the paid order reads state %s, lightning_msat %d, rate %q; want paid, %d, the same pricing at 76025", after.State, after.Msat, after.Pricing.Rates["BTC"].Rate, before.Msat)
	}
	if _, page := e.page(tok); !strings.Contains(page, "10.00 USD") {
		t.Fatal("after the restart the page no longer shows the price")
	}
	o2, extra2 := e.pricedOrder("fiat-2", "10.00", "USD", 4, 3600, fiatQuoteBound)
	up := fiatMedian(t, fiatUp["USD"])
	if up.String() != "83627.5" {
		t.Fatalf("the expected median after the move is %s, the test expects 83627.5", up)
	}
	checkPricing(t, o2, "10.00", "USD", "83627.5", fiatUp["USD"])
	if o2.Msat != fiatSats(t, "10.00", up)*1000 {
		t.Fatalf("the order at the moved rate: lightning_msat %d, want %d", o2.Msat, fiatSats(t, "10.00", up)*1000)
	}
	// With the new rates live (o2 was priced from them), the paid order still reads its own rate and amount.
	if again := e.order(o.ID); again.State != "paid" || again.Msat != before.Msat || !reflect.DeepEqual(again.Pricing, before.Pricing) {
		t.Fatalf("with the new rates live the paid order reads state %s, lightning_msat %d; want paid, %d, the same pricing", again.State, again.Msat, before.Msat)
	}
	eur := fiatMedian(t, fiatUp["EUR"])
	o3, extra3 := e.pricedOrder("fiat-eur", "10.00", "EUR", 4, 3600, fiatQuoteBound)
	checkPricing(t, o3, "10.00", "EUR", eur.String(), fiatUp["EUR"])
	if o3.Msat != fiatSats(t, "10.00", eur)*1000 {
		t.Fatalf("the EUR order: lightning_msat %d, want %d", o3.Msat, fiatSats(t, "10.00", eur)*1000)
	}
	t.Logf("OBSERVED fiat restart: the paid order kept rate 76025 and %d msat; new orders priced at %s USD (%d msat) and %s EUR (%d msat)", after.Msat, up, o2.Msat, eur, o3.Msat)
	if _, status, code := e.createFiatOrder("fiat-oc-"+randHex(t)[:12], "10.00", "USD", []string{"onchain"}, 3600); status != 400 || code != "onchain_disabled" {
		t.Fatalf(`"rails":["onchain"] without the on-chain rail: status %d, code %q, want 400 onchain_disabled`, status, code)
	}
	e.settleQuiet()
	s.stop()
	noWarnings(t, s.logs.String())

	wants := []want{{orderID: o.ID, msat: o.Msat, paid: true}, {orderID: explicit.ID, msat: orderMsat}, {orderID: o2.ID, msat: o2.Msat}, {orderID: o3.ID, msat: o3.Msat}}
	for _, x := range slices.Concat(extra, extra2, extra3) {
		wants = append(wants, want{orderID: x.ID, msat: x.Msat})
	}
	e.safety(wants)
}

// TestFiatOutage: Kraken and Coinbase fail from the start (they never had a rate to keep), so orders price from Yadio
// and the custom API. Then Yadio fails too: its last rate counts until it ages out, then a priced POST is 503
// rate_unavailable with Retry-After while a repeat of an existing priced order is still 200 with that order and an
// explicit order still works, and the ALERT says too_few. The custom API
// alone moving away logs nothing (too_few stays too_few). When the failures clear, the four rates disagree: the ALERT
// again, now disagree. The custom API back in line: 201 and the available line. Then only stale rates are left: 503.
// No provider's body reaches the log.
func TestFiatOutage(t *testing.T) {
	e, fake := newFiatEnv(t, "fiat-outage")
	fake.Reset()
	seedFiat(fake, fiatBase)
	fake.Fail("kraken", 500)
	fake.Fail("coinbase", 503)
	s := e.start()

	first, extra := e.pricedOrder("outage-1", "10.00", "USD", 2, 3600, fiatQuoteBound)
	r2 := fiatMedian(t, fiatBase["USD"], "yadio", "custom")
	checkPricing(t, first, "10.00", "USD", r2.String(), map[string]string{"custom": fiatBase["USD"]["custom"], "yadio": fiatBase["USD"]["yadio"]})
	t.Logf("OBSERVED fiat=outage with kraken and coinbase failing: priced from custom and yadio at %s", r2)
	// The repeat of the first order's POST is 200 (the same order) whether or not a rate is available, and the same
	// reference with another price text ("10.000") is 409 conflict whether or not a rate is available: both are looked
	// up before any conversion. The probe asks for a price no rail can hold under a reference of its own: it is
	// converted, so it is 400 invalid_lightning_msat while a rate is available and 503 rate_unavailable while none
	// is. None of the three creates an order.
	repeat := func(ref string) (int, string, string) {
		return e.apiErr("POST", "/v1/orders", e.writeKey, map[string]any{"account_id": acctID, "merchant_ref": ref,
			"price": "10.00", "currency": "USD", "expires_in": 3600})
	}
	conflict := func(ref string) (int, string, string) {
		return e.apiErr("POST", "/v1/orders", e.writeKey, map[string]any{"account_id": acctID, "merchant_ref": ref,
			"price": "10.000", "currency": "USD", "expires_in": 3600})
	}
	probeRef := "outage-probe-" + randHex(t)[:12]
	probe := func() (int, string, string) {
		return e.apiErr("POST", "/v1/orders", e.writeKey, map[string]any{"account_id": acctID, "merchant_ref": probeRef,
			"price": fiatTooBig, "currency": "USD", "expires_in": 3600})
	}
	if status, code, _ := repeat(first.MerchantRef); status != 200 {
		t.Fatalf("the repeat of the priced POST: status %d, code %q, want 200", status, code)
	}
	if status, code, _ := conflict(first.MerchantRef); status != 409 || code != "conflict" {
		t.Fatalf("another price text under the first reference with a rate available: status %d, code %q, want 409 conflict", status, code)
	}
	if status, code, _ := probe(); status != 400 || code != "invalid_lightning_msat" {
		t.Fatalf("the probe with a rate available: status %d, code %q, want 400 invalid_lightning_msat", status, code)
	}

	// Yadio fails right after one of its good answers, so its last rate is pinned to that moment and ages out
	// max_age_seconds later.
	h := fake.Hits("yadio")
	poll(t, "a Yadio fetch", 2*fiatInterval+5*time.Second, 20*time.Millisecond, func() (bool, error) {
		return fake.Hits("yadio") > h, errors.New("no fetch yet")
	})
	fake.Fail("yadio", 502)
	lastGood := time.Now()
	var retry string
	poll(t, "the priced POST turns 503", fiatMaxAge+fiatInterval, 250*time.Millisecond, func() (bool, error) {
		status, code, ra := probe()
		if status == 400 && code == "invalid_lightning_msat" {
			return false, errors.New("400: the old Yadio rate still counts")
		}
		if status != 503 || code != "rate_unavailable" {
			t.Fatalf("the priced POST in the outage: status %d, code %q, want 503 rate_unavailable", status, code)
		}
		retry = ra
		return true, nil
	})
	gap := time.Since(lastGood)
	t.Logf("MEASURED fiat=outage last_fetch_to_unavailable_seconds=%.2f (max_age_seconds 60, fetch_seconds 10; from Yadio's last good answer to the first 503, POSTs every 0.25 s)", gap.Seconds())
	if status, code, _ := repeat(first.MerchantRef); status != 200 {
		t.Errorf("the repeat of the priced POST with no rate: status %d, code %q, want 200 (the existing order)", status, code)
	}
	if status, code, ra := conflict(first.MerchantRef); status != 409 || code != "conflict" || ra != "" {
		t.Errorf("another price text under the first reference with no rate: status %d, code %q, Retry-After %q, want 409 conflict", status, code, ra)
	}
	if gap < fiatMaxAge-5*time.Second {
		t.Errorf("the priced POST turned 503 after %.1f s: Yadio's last rate should count until it is 60 s old", gap.Seconds())
	}
	if retry != fiatRetryAfter {
		t.Errorf("rate_unavailable with Retry-After %q, want %q", retry, fiatRetryAfter)
	}
	during := e.createOrder("outage-explicit-"+randHex(t)[:12], orderMsat, 3600)
	if during.Pricing != nil {
		t.Fatal("an explicit order made in the outage has pricing")
	}
	recs := waitStates(t, s, "the too_few ALERT", 2*fiatInterval+5*time.Second, []string{"too_few"}, []string{"too_few"})
	for _, cur := range []string{"USD", "EUR"} {
		if _, l := stateLines(recs, cur); !slices.Equal(l[0].Providers, []string{"custom"}) || l[0].Level != "ERROR" {
			t.Errorf("the %s too_few ALERT: level %s, providers %v, want ERROR and [custom]", cur, l[0].Level, l[0].Providers)
		}
	}
	fiatFailing := map[string]int{"kraken": 500, "coinbase": 503, "yadio": 502}
	failed := checkFiatLog(t, s.logs.String(), fiatFailing)
	if !maps.Equal(failed, map[string]int{"kraken": 1, "coinbase": 1, "yadio": 1}) {
		t.Errorf("rate fetch failed WARN lines per provider: %v, want one each for kraken, coinbase and yadio (one run each)", failed)
	}
	t.Logf("OBSERVED fiat=outage rate fetch failed lines so far: %v; the provider's body is absent from the log", failed)

	// The custom API alone moves far away while the other three fail: one rate is still too few, nothing is logged.
	// Two more custom fetches: passes never overlap, so the pass that read the new rate has ended.
	fake.Set("custom", "USD", "90000")
	h = fake.Hits("custom")
	poll(t, "two passes after the move", 3*fiatInterval, 50*time.Millisecond, func() (bool, error) {
		return fake.Hits("custom") >= h+2, errors.New("not yet")
	})
	waitStates(t, s, "no new state line", time.Second, []string{"too_few"}, []string{"too_few"})

	// The failures clear: four fresh rates, the custom one 18 % above the rest.
	for _, p := range []string{"kraken", "coinbase", "yadio"} {
		fake.Fail(p, 0)
	}
	recs = waitStates(t, s, "the disagree ALERT", 3*fiatInterval, []string{"too_few", "disagree"}, []string{"too_few", "available"})
	_, l := stateLines(recs, "USD")
	if d := l[1]; d.Level != "ERROR" || !slices.Equal(d.Providers, []string{"coinbase", "custom", "kraken", "yadio"}) || d.Low != "yadio" || d.High != "custom" {
		t.Errorf("the disagree ALERT: level %s, providers %v, low %q, high %q; want ERROR, the four, yadio and custom", d.Level, d.Providers, d.Low, d.High)
	}
	if status, code, ra := probe(); status != 503 || code != "rate_unavailable" || ra != fiatRetryAfter {
		t.Errorf("the priced POST while the rates disagree: status %d, code %q, Retry-After %q", status, code, ra)
	}

	// The custom API back in line: a new priced order is 201 from the four, and USD logs available once.
	fake.Set("custom", "USD", fiatBase["USD"]["custom"])
	second, extra2 := e.pricedOrder("outage-2", "10.00", "USD", 4, 3600, 3*fiatInterval)
	checkPricing(t, second, "10.00", "USD", fiatMedian(t, fiatBase["USD"]).String(), fiatBase["USD"])
	recs = waitStates(t, s, "the available line", 2*fiatInterval, []string{"too_few", "disagree", "available"}, []string{"too_few", "available"})
	if _, l := stateLines(recs, "USD"); l[2].Level != "INFO" || len(l[2].Providers) != 4 {
		t.Errorf("the available line: level %s, providers %v", l[2].Level, l[2].Providers)
	}

	// Staleness: Yadio and the custom API serve rates 10 minutes old, Kraken and Coinbase fail again. Their cached
	// rates count until they are 60 s old, then nothing fresh is left.
	old := time.Now().Add(-10 * time.Minute)
	fake.SetAt("yadio", old)
	fake.SetAt("custom", old)
	fake.Fail("kraken", 500)
	fake.Fail("coinbase", 503)
	staleFrom := time.Now()
	poll(t, "the priced POST turns 503 on stale rates", fiatMaxAge+2*fiatInterval, 250*time.Millisecond, func() (bool, error) {
		status, code, ra := probe()
		if status == 400 && code == "invalid_lightning_msat" {
			return false, errors.New("400: a fresh rate is still cached")
		}
		if status != 503 || code != "rate_unavailable" || ra != fiatRetryAfter {
			t.Fatalf("the priced POST on stale rates: status %d, code %q, Retry-After %q", status, code, ra)
		}
		return true, nil
	})
	t.Logf("MEASURED fiat=stale stale_to_unavailable_seconds=%.2f (from serving 10-minute-old rates and failing the other two to the first 503)", time.Since(staleFrom).Seconds())
	if status, code, _ := repeat(second.MerchantRef); status != 200 {
		t.Errorf("the repeat of the second priced POST on stale rates: status %d, code %q, want 200", status, code)
	}
	recs = waitStates(t, s, "the second too_few ALERT", 2*fiatInterval+5*time.Second,
		[]string{"too_few", "disagree", "available", "too_few"}, []string{"too_few", "available", "too_few"})
	if _, l := stateLines(recs, "USD"); len(l[3].Providers) != 0 {
		t.Errorf("the stale too_few ALERT names providers %v, want none", l[3].Providers)
	}

	s.stopQuiet()
	logs := s.logs.String()
	failed = checkFiatLog(t, logs, fiatFailing)
	// Kraken and Coinbase failed in two runs (from the start, and again in the staleness step), Yadio in one.
	if !maps.Equal(failed, map[string]int{"kraken": 2, "coinbase": 2, "yadio": 1}) {
		t.Errorf("rate fetch failed WARN lines per provider: %v, want kraken 2, coinbase 2, yadio 1", failed)
	}
	recs = fiatRecords(t, logs)
	gu, _ := stateLines(recs, "USD")
	ge, _ := stateLines(recs, "EUR")
	t.Logf("OBSERVED fiat=outage state lines USD %v, EUR %v; rate fetch failed lines %v; %d log lines", gu, ge, failed, len(recs))
	if !slices.Equal(gu, []string{"too_few", "disagree", "available", "too_few"}) || !slices.Equal(ge, []string{"too_few", "available", "too_few"}) {
		t.Errorf("state lines USD %v, EUR %v", gu, ge)
	}

	wants := []want{{orderID: first.ID, msat: first.Msat}, {orderID: during.ID, msat: orderMsat}, {orderID: second.ID, msat: second.Msat}}
	for _, x := range slices.Concat(extra, extra2) {
		wants = append(wants, want{orderID: x.ID, msat: x.Msat})
	}
	e.safety(wants)
}

// register adds a priced order to the on-chain registry with the events it must have.
func (e *ocEnv) register(o orderJSON, tag string, ev map[string]rng) *ocOrder {
	e.t.Helper()
	exp, err := time.Parse(time.RFC3339, o.ExpiresAt)
	if err != nil || o.Onchain == nil {
		e.t.Fatalf("order %s: expires_at %q, onchain %v", o.ID, o.ExpiresAt, o.Onchain != nil)
	}
	if ev == nil {
		ev = map[string]rng{}
	}
	ro := &ocOrder{ID: o.ID, Token: tokenOf(o), Tag: tag, Sat: o.OnchainSat, Required: o.Onchain.RequiredConfirmations, Expiry: exp.Unix(), Ev: ev}
	e.reg.Orders = append(e.reg.Orders, ro)
	e.saveReg()
	return ro
}

// TestOnchainFiat: in the shared on-chain environment with the rates block added for this phase, a priced order with
// the configured rails gets both amounts from one conversion, is paid on-chain, and its webhook carries the pricing.
func TestOnchainFiat(t *testing.T) {
	fake := ratestest.New(t)
	seedFiat(fake, fiatBase)
	e := newOcEnvAt(t, ocName, ocRoot(t), true, fiatRates())
	e.rateRT = fake.Transport()
	s := e.start()
	o, extra := e.pricedOrder("oc-fiat", "25.00", "USD", 4, 86400, fiatQuoteBound)
	for _, x := range extra {
		e.register(x, "fiat-extra", nil)
	}
	ro := e.register(o, "fiat", map[string]rng{store.EventPaid: {1, 1}})
	sat := fiatSats(t, "25.00", fiatMedian(t, fiatBase["USD"]))
	if o.OnchainSat != sat || o.Msat != sat*1000 || o.Lightning == nil || o.Onchain.State != "waiting" || o.Onchain.RequiredConfirmations != 1 {
		t.Fatalf("the priced order: onchain_sat %d, lightning_msat %d, %d confirmations; want %d, %d, 1", o.OnchainSat, o.Msat, o.Onchain.RequiredConfirmations, sat, sat*1000)
	}
	checkPricing(t, o, "25.00", "USD", "76025", fiatBase["USD"])
	if code, page := e.page(ro.Token); code != 200 || !strings.Contains(page, "25.00 USD") {
		t.Fatalf("the priced order's page: status %d, price shown %v", code, strings.Contains(page, "25.00 USD"))
	}
	addr, _ := e.press(ro, true)
	_, sent := e.pay(addr, sat)
	e.mine(1)
	d, got := e.waitOC(ro.ID, "paid", sent, ocBound, func(o orderJSON) bool {
		return o.State == "paid" && o.Onchain != nil && o.Onchain.State == "paid" && o.Onchain.ConfirmedSat == sat
	})
	t.Logf("MEASURED onchain=fiat send_to_paid_seconds=%.2f (%d sat for 25.00 USD at 76025 with a 1.5 %% margin; 1 block)", d.Seconds(), sat)
	if !reflect.DeepEqual(got.Pricing, o.Pricing) {
		t.Fatal("the paid order's pricing differs from the one it was created with")
	}
	e.waitHook(ro.ID, store.EventPaid, hookBound)
	e.checkHookPricing(o, store.EventPaid)
	time.Sleep(ocQuiet)
	e.finish(s, nil)
}
