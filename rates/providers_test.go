package rates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/rates/ratestest"
)

// krakenSeven are Kraken's BTC fiat quotes, the currencies of the recorded fixtures.
var krakenSeven = []string{"AUD", "CAD", "CHF", "EUR", "GBP", "JPY", "USD"}

// fuzzCurrencies is what the fuzz targets request: the seven, plus one no built-in parser keys on.
var fuzzCurrencies = []string{"AUD", "CAD", "CHF", "EUR", "GBP", "JPY", "USD", "COP"}

// at2030 is the fetch time the tests pass: far from the wall clock, so a stamp from time.Now shows.
var at2030 = time.Date(2030, 1, 2, 3, 4, 5, 6, time.UTC)

// The refusal tables of each parser: every body is refused whole, with the error named. The fuzz targets
// are seeded with all of them (fuzzSeeds).
var (
	krakenRefusals = []struct {
		name string
		body string
		want error
	}{
		{"unknown pair", `{"error":["EQuery:Unknown asset pair"]}`, ErrVendor},
		{"a warning beside a result", `{"error":["WGeneral:Warning"],"result":{"BTC/USD":{"a":["2","1","1.000"],"b":["1","1","1.000"]}}}`, ErrVendor},
		{"a non-string error element", `{"error":[1],"result":{}}`, ErrVendor},
		{"a as a string", `{"error":[],"result":{"BTC/USD":{"a":"2","b":["1"]}}}`, ErrShape},
		{"b missing", `{"error":[],"result":{"BTC/USD":{"a":["2"]}}}`, ErrShape},
		{"a empty", `{"error":[],"result":{"BTC/USD":{"a":[],"b":["1"]}}}`, ErrShape},
		{"a[0] a number", `{"error":[],"result":{"BTC/USD":{"a":[2],"b":["1"]}}}`, ErrShape},
		{"the pair not an object", `{"error":[],"result":{"BTC/USD":["2","1"]}}`, ErrShape},
		{"error not an array", `{"error":"EQuery:Unknown asset pair"}`, ErrShape},
		{"error missing", `{"result":{}}`, ErrShape},
		{"result missing", `{"error":[]}`, ErrShape},
		{"result not an object", `{"error":[],"result":[]}`, ErrShape},
		{"not an object", `[]`, ErrShape},
		{"not JSON", `<html>`, ErrShape},
		{"empty", ``, ErrShape},
		{"two documents", `{"error":[],"result":{}}{}`, ErrShape},
		{"a duplicate pair", `{"error":[],"result":{"BTC/USD":{"a":["2"],"b":["1"]},"BTC/USD":{"a":["4"],"b":["3"]}}}`, ErrShape},
	}
	// coinbaseRefusals are ErrShape.
	coinbaseRefusals = []string{
		`{"data":{"currency":"ETH","rates":{"USD":"1"}}}`,
		`{}`,
		`{"data":{"currency":"BTC"}}`,
		`{"data":{"currency":"BTC","rates":[]}}`,
		`{"data":{"rates":{"USD":"1"}}}`,
		`{"data":{"currency":1,"rates":{"USD":"1"}}}`,
		`{"data":[]}`,
		`{"errors":[{"id":"not_found","message":"Not found"}]}`,
		`[]`,
		`{"data":{"currency":"BTC","rates":{"USD":"1","USD":"2"}}}`,
	}
	// yadioVendorRefusals are ErrVendor, yadioShapeRefusals ErrShape.
	yadioVendorRefusals = []string{
		`{"error":"currency not found"}`,
		`{"error":null}`,
		`{"error":"x","BTC":{"USD":1},"base":"BTC","timestamp":1793902508506}`,
	}
	yadioShapeRefusals = []string{
		`{"BTC":{"USD":1},"base":"EUR","timestamp":1793902508506}`,
		`{"EUR":{"USD":1},"base":"EUR","timestamp":1793902508506}`,
		`{"BTC":{"USD":1},"base":"BTC","timestamp":9223372036854775808}`,
		`{"BTC":{"USD":1},"timestamp":1793902508506}`,
		`{"BTC":{"USD":1},"base":"BTC"}`,
		`{"BTC":{"USD":1},"base":"BTC","timestamp":"1793902508506"}`,
		`{"BTC":{"USD":1},"base":"BTC","timestamp":1793902508506.5}`,
		`{"BTC":{"USD":1},"base":"BTC","timestamp":1.7e12}`,
		`{"BTC":{"USD":1},"base":"BTC","timestamp":-1}`,
		`{"BTC":{"USD":1},"base":"BTC","timestamp":99999999999999999999}`,
		`{"EUR":{"USD":1},"base":"BTC","timestamp":1793902508506}`,
		`{"BTC":[1],"base":"BTC","timestamp":1793902508506}`,
		`{"BTC":{"USD":1},"base":1,"timestamp":1793902508506}`,
		`{"BTC":{"USD":1,"USD":2},"base":"BTC","timestamp":1793902508506}`,
		`[]`,
		`{}`,
	}
	// customRefusals are ErrShape.
	customRefusals = []string{
		`{"asset":"BTC","rates":{"USD":"1"},"timestamp":1793902508,"x":1}`,
		`{"asset":"BTC","rates":{"USD":"1"}}`,
		`{"rates":{"USD":"1"},"timestamp":1793902508}`,
		`{"asset":"BTC","timestamp":1793902508}`,
		`{"asset":"XMR","rates":{"USD":"1"},"timestamp":1793902508}`,
		`{"asset":"btc","rates":{"USD":"1"},"timestamp":1793902508}`,
		`{"asset":1,"rates":{"USD":"1"},"timestamp":1793902508}`,
		`{"Asset":"BTC","rates":{"USD":"1"},"timestamp":1793902508}`,
		`{"asset":"BTC","asset":"BTC","rates":{"USD":"1"},"timestamp":1793902508}`,
		`{"asset":"BTC","rates":{"USD":"1","USD":"2"},"timestamp":1793902508}`,
		`{"asset":"BTC","rates":["USD"],"timestamp":1793902508}`,
		`{"asset":"BTC","rates":{"USD":"1"},"timestamp":"1793902508"}`,
		`{"asset":"BTC","rates":{"USD":"1"},"timestamp":-1}`,
		`{"asset":"BTC","rates":{"USD":"1"},"timestamp":1793902508.0}`,
		`{"asset":"BTC","rates":{"USD":"1"},"timestamp":1.7e9}`,
		`{"asset":"BTC","rates":{"USD":"1"},"timestamp":null}`,
		// nesting 7 deep: two objects and five arrays
		`{"asset":"BTC","rates":{"USD":[[[[["1"]]]]]},"timestamp":1793902508}`,
		`[]`,
		`null`,
		``,
	}
	// bitfinexVendorRefusals are ErrVendor, bitfinexShapeRefusals ErrShape.
	bitfinexVendorRefusals = []string{
		`["error",10020,"symbol: invalid"]`,
		`["error"]`,
		`["error",10020,"symbol: invalid",["tXMRUSD",1,1,2,1,0,0,1,1,1,1]]`,
	}
	// bitfinexSkips are bodies whose one ticker the parser skips for XMR in USD: no rate and no error.
	bitfinexSkips = []string{
		`[["tXMRUSD",150,1,151,1,0,0,150,1,1]]`,                                        // 10 elements
		`[["tXMRUSD"]]`,                                                                // 1 element
		`[["tXMRUSD",151,1,150,1,0,0,150,1,1,1]]`,                                      // bid > ask
		`[["tXMRUSD",1.2e2,1,151,1,0,0,150,1,1,1]]`,                                    // an exponent in the bid
		`[["tXMRUSD",150,1,1.51E2,1,0,0,150,1,1,1]]`,                                   // an exponent in the ask
		`[["tXMRUSD","150",1,151,1,0,0,150,1,1,1]]`,                                    // a string where a number is expected
		`[["tXMRUSD",150,1,null,1,0,0,150,1,1,1]]`,                                     // a null
		`[["tXMRUSD",-150,1,151,1,0,0,150,1,1,1]]`,                                     // a sign
		`[["tXMRUSD",0,1,151,1,0,0,150,1,1,1]]`,                                        // a zero bid
		`[["tXMRUSD",0,1,0,1,0,0,150,1,1,1]]`,                                          // a zero bid and ask
		`[["tXMRUSD",150.1234567890123456789,1,151,1,0,0,150,1,1,1]]`,                  // 19 fraction digits
		`[["tXMRUSD",123456789012345678901,1,1234567890123456789012,1,0,0,150,1,1,1]]`, // 21 integer digits
		`[["tXMREUR",150,1,151,1,0,0,150,1,1,1]]`,                                      // not requested
		`[["txmrusd",150,1,151,1,0,0,150,1,1,1]]`,                                      // another case: not requested
		`[["fUSD",150,1,151,1,0,0,150,1,1,1,0,0,0,0,0]]`,                               // a funding ticker: not requested
		`[[150,150,1,151,1,0,0,150,1,1,1]]`,                                            // a symbol that is not a string
		`[[null,150,1,151,1,0,0,150,1,1,1]]`,                                           // a null symbol
	}
	bitfinexShapeRefusals = []string{
		`{}`,
		`{"error":"x"}`,
		`{"0":["tXMRUSD",1,1,2,1,0,0,1,1,1,1]}`,
		`[1]`,
		`["tXMRUSD",1,1,2,1,0,0,1,1,1,1]`,
		`[["tXMRUSD",1,1,2,1,0,0,1,1,1,1],"x"]`,
		`[["tXMRUSD",1,1,2,1,0,0,1,1,1,1],null]`,
		`[["tXMRUSD",1,1,2,1,0,0,1,1,1,1],{}]`,
		`[["tXMRUSD",1,1,2,1,0,0,1,1,1,1],["tXMRUSD",3,1,4,1,0,0,1,1,1,1]]`, // one symbol twice: read two ways
		`[[]][]`,
		`[[[[[[[1]]]]]]]`, // nesting 7 deep
		`null`,
		`"error"`,
		``,
	}
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func byCurrency(t *testing.T, rs []Rate) map[string]Rate {
	t.Helper()
	m := make(map[string]Rate, len(rs))
	for _, r := range rs {
		if _, dup := m[r.Currency]; dup {
			t.Fatalf("two rates for %s", r.Currency)
		}
		m[r.Currency] = r
	}
	return m
}

// rateProblem checks what every parser promises, whatever the body: an error comes with no rates;
// each rate is positive, carries the provider's name, a requested currency, and no currency twice.
func rateProblem(rs []Rate, err error, provider string, currencies []string) string {
	if err != nil {
		if len(rs) != 0 {
			return "rates returned with an error"
		}
		if !errors.Is(err, ErrShape) && !errors.Is(err, ErrVendor) {
			return "a parser error outside ErrShape and ErrVendor"
		}
		return ""
	}
	seen := map[string]bool{}
	for _, r := range rs {
		if r.Value.IsZero() || r.Value.Units().Sign() <= 0 {
			return "a rate that is not positive"
		}
		if r.Provider != provider {
			return "a rate with another provider's name"
		}
		requested := false
		for _, c := range currencies {
			requested = requested || c == r.Currency
		}
		if !requested {
			return "a rate for a currency not requested"
		}
		if seen[r.Currency] {
			return "two rates for one currency"
		}
		seen[r.Currency] = true
	}
	return ""
}

func mustNoProblem(t *testing.T, rs []Rate, err error, provider string, currencies []string) {
	t.Helper()
	if p := rateProblem(rs, err, provider, currencies); p != "" {
		t.Fatal(p)
	}
}

// midUnits is the expected Kraken rate: floor((ask + bid) / 2) in units.
func midUnits(ask, bid Decimal) *big.Int {
	s := new(big.Int).Add(ask.Units(), bid.Units())
	return s.Quo(s, big.NewInt(2))
}

func wantErr(t *testing.T, rs []Rate, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if len(rs) != 0 {
		t.Fatalf("%d rates with the error", len(rs))
	}
}

func fakeClient(t *testing.T) (*ratestest.Server, *http.Client) {
	t.Helper()
	fake := ratestest.New(t)
	return fake, NewClient(fake.Transport())
}

func TestKrakenFixture(t *testing.T) {
	body := fixture(t, "kraken.json")
	var doc struct {
		Result map[string]struct {
			A []string `json:"a"`
			B []string `json:"b"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	rs, err := parseKraken(body, AssetBTC, krakenSeven, at2030)
	if err != nil {
		t.Fatal(err)
	}
	mustNoProblem(t, rs, err, "kraken", krakenSeven)
	got := byCurrency(t, rs)
	want := 0
	for _, code := range krakenSeven {
		p, ok := doc.Result["BTC/"+code]
		if !ok {
			continue
		}
		ask, bid := dec(t, p.A[0]), dec(t, p.B[0])
		if bid.IsZero() || bid.Cmp(ask) > 0 {
			continue
		}
		want++
		r, ok := got[code]
		if !ok {
			t.Fatalf("no rate for %s", code)
		}
		if r.Value.Units().Cmp(midUnits(ask, bid)) != 0 {
			t.Errorf("%s = %s, want the mid of %s and %s", code, r.Value, ask, bid)
		}
		if !r.At.Equal(at2030) {
			t.Errorf("%s At = %v, want the fetch time", code, r.At)
		}
	}
	if want == 0 || len(rs) != want {
		t.Fatalf("%d rates, want %d (and at least one)", len(rs), want)
	}
	if _, ok := got["USD"]; !ok {
		t.Fatal("the fixture has no USD rate")
	}
}

func TestKrakenErrorBody(t *testing.T) {
	for _, c := range krakenRefusals {
		rs, err := parseKraken([]byte(c.body), AssetBTC, krakenSeven, at2030)
		if !errors.Is(err, c.want) || len(rs) != 0 {
			t.Errorf("%s: %d rates, err %v, want %v", c.name, len(rs), err, c.want)
		}
	}

	rs, err := parseKraken([]byte(`{"error":[],"result":{}}`), AssetBTC, krakenSeven, at2030)
	if err != nil || len(rs) != 0 {
		t.Fatalf("an empty result: %d rates, err %v", len(rs), err)
	}

	// b[0] above a[0] is skipped, a zero bid is skipped, an unparsable price is skipped, a pair not
	// asked for is ignored even when malformed; the others parse.
	body := `{"error":[],"result":{` +
		`"BTC/USD":{"a":["1"],"b":["2"]},` +
		`"BTC/GBP":{"a":["1"],"b":["0"]},` +
		`"BTC/CAD":{"a":["7.6e4"],"b":["1"]},` +
		`"BTC/CHF":{"a":["-1"],"b":["-2"]},` +
		`"BTC/COP":{"a":"junk"},` +
		`"BTC/EUR":{"a":["3","1","1.000"],"b":["1","1","1.000"]},` +
		`"BTC/JPY":{"a":["1.000000000000000001"],"b":["1"]},` +
		`"BTC/AUD":{"a":["5"],"b":["5"]}}}`
	rs, err = parseKraken([]byte(body), AssetBTC, krakenSeven, at2030)
	mustNoProblem(t, rs, err, "kraken", krakenSeven)
	got := byCurrency(t, rs)
	if len(got) != 3 {
		t.Fatalf("rates %v, want EUR, JPY and AUD", got)
	}
	for code, want := range map[string]string{"EUR": "2", "JPY": "1", "AUD": "5"} {
		if got[code].Value.Cmp(dec(t, want)) != 0 {
			t.Errorf("%s = %s, want %s (the floor of the mid)", code, got[code].Value, want)
		}
	}
}

func TestKrakenRequestsOnlyItsFiats(t *testing.T) {
	fake, c := fakeClient(t)
	fake.Set("kraken", "USD", "76000")
	fake.Set("kraken", "EUR", "67000")
	rs, err := Kraken(c).Fetch(context.Background(), AssetBTC, []string{"USD", "COP", "EUR"}, at2030)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("%d rates, want 2", len(rs))
	}
	reqs := fake.Requests("kraken")
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	q, err := url.ParseQuery(reqs[0])
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("pair") != "XBTUSD,XBTEUR" || q.Get("assetVersion") != "1" {
		t.Fatalf("query %q", reqs[0])
	}

	fake.Reset()
	rs, err = Kraken(c).Fetch(context.Background(), AssetBTC, []string{"COP"}, at2030)
	if err != nil || len(rs) != 0 {
		t.Fatalf("COP only: %d rates, err %v", len(rs), err)
	}
	if n := fake.Hits("kraken"); n != 0 {
		t.Fatalf("%d requests for a currency Kraken does not quote", n)
	}
}

// KrakenFiats is the list Kraken asks for: given every valid code, its one request names exactly these
// pairs, in this order. The result is a copy: changing it changes nothing.
func TestKrakenFiatsExported(t *testing.T) {
	var all []string
	for a := 'A'; a <= 'Z'; a++ {
		for b := 'A'; b <= 'Z'; b++ {
			for c := 'A'; c <= 'Z'; c++ {
				if s := string([]rune{a, b, c}); ValidCurrency(s) {
					all = append(all, s)
				}
			}
		}
	}
	fake, c := fakeClient(t)
	if _, err := Kraken(c).Fetch(context.Background(), AssetBTC, all, at2030); err != nil {
		t.Fatal(err)
	}
	reqs := fake.Requests("kraken")
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	q, err := url.ParseQuery(reqs[0])
	if err != nil {
		t.Fatal(err)
	}
	fiats := KrakenFiats()
	want := make([]string, len(fiats))
	for i, f := range fiats {
		want[i] = "XBT" + f
	}
	if q.Get("pair") != strings.Join(want, ",") || !equalStrings(fiats, krakenSeven) {
		t.Fatalf("Kraken asks for %q; KrakenFiats is %v", q.Get("pair"), fiats)
	}
	fiats[0] = "COP"
	if KrakenFiats()[0] != "AUD" {
		t.Fatal("KrakenFiats shares its slice with the provider")
	}
}

// ProviderYadio is Yadio's name, on the provider and on each of its rates.
func TestProviderYadioName(t *testing.T) {
	if ProviderYadio != "yadio" || Yadio(nil).Name() != ProviderYadio {
		t.Fatalf("ProviderYadio %q, Name %q", ProviderYadio, Yadio(nil).Name())
	}
	rs, err := parseYadio([]byte(`{"BTC":{"USD":76022.31},"base":"BTC","timestamp":1793902508506}`), []string{"USD"}, at2030)
	if err != nil || len(rs) != 1 || rs[0].Provider != ProviderYadio {
		t.Fatalf("rates %+v, err %v", rs, err)
	}
}

func TestFetchStampsWithTheGivenTime(t *testing.T) {
	fake, c := fakeClient(t)
	fake.Set("kraken", "USD", "76000")
	fake.Set("coinbase", "USD", "76100")
	for _, p := range []Provider{Kraken(c), Coinbase(c)} {
		rs, err := p.Fetch(context.Background(), AssetBTC, []string{"USD"}, at2030)
		if err != nil || len(rs) != 1 {
			t.Fatalf("%s: %d rates, err %v", p.Name(), len(rs), err)
		}
		if !rs[0].At.Equal(at2030) {
			t.Errorf("%s At = %v, want %v", p.Name(), rs[0].At, at2030)
		}
		if rs[0].Provider != p.Name() || rs[0].Currency != "USD" {
			t.Errorf("%s: rate %+v", p.Name(), rs[0])
		}
	}
}

// TestBuiltinsRefuseOtherAssets: Coinbase and Yadio quote BTC only; Kraken and Bitfinex the assets of their tables.
// An asset outside them is ErrAsset, with no request.
func TestBuiltinsRefuseOtherAssets(t *testing.T) {
	fake, c := fakeClient(t)
	for _, p := range []string{"kraken", "coinbase", "yadio", "bitfinex"} {
		fake.Set(p, "USD", "1")
		fake.SetAsset(p, "XMR", "USD", "1")
	}
	for _, x := range []struct {
		p     Provider
		asset string
	}{{Coinbase(c), "XMR"}, {Yadio(c), "XMR"}, {Kraken(c), "ETH"}, {Bitfinex(c), "ETH"}, {Kraken(c), ""}, {Bitfinex(c), "btc"}} {
		rs, err := x.p.Fetch(context.Background(), x.asset, []string{"USD"}, at2030)
		wantErr(t, rs, err, ErrAsset)
		if n := fake.Hits(x.p.Name()); n != 0 {
			t.Errorf("%s: %d requests for asset %q", x.p.Name(), n, x.asset)
		}
	}
}

func TestProviderNames(t *testing.T) {
	if ProviderBitfinex != "bitfinex" {
		t.Fatalf("ProviderBitfinex %q", ProviderBitfinex)
	}
	c := NewClient(nil)
	for want, p := range map[string]Provider{
		"kraken": Kraken(c), "coinbase": Coinbase(c), "yadio": Yadio(c), "custom": Custom(c, "https://rates.example.invalid/"),
		"bitfinex": Bitfinex(c),
	} {
		if p.Name() != want {
			t.Errorf("Name() = %q, want %q", p.Name(), want)
		}
	}
}

func TestCoinbaseFixture(t *testing.T) {
	body := fixture(t, "coinbase.json")
	var doc struct {
		Data struct {
			Rates map[string]string `json:"rates"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	rs, err := parseCoinbase(body, krakenSeven, at2030)
	mustNoProblem(t, rs, err, "coinbase", krakenSeven)
	got := byCurrency(t, rs)
	want := 0
	for _, code := range krakenSeven {
		s, ok := doc.Data.Rates[code]
		if !ok {
			continue
		}
		d, err := ParseDecimal(s)
		if err != nil || d.IsZero() {
			continue
		}
		want++
		if got[code].Value.Cmp(d) != 0 || !got[code].At.Equal(at2030) {
			t.Errorf("%s = %+v, want %s at the fetch time", code, got[code], d)
		}
	}
	if want == 0 || len(rs) != want {
		t.Fatalf("%d rates, want %d (and at least one)", len(rs), want)
	}
	if _, ok := got["USD"]; !ok {
		t.Fatal("the fixture has no USD rate")
	}
}

func TestCoinbaseMissingCode(t *testing.T) {
	rs, err := parseCoinbase([]byte(`{"data":{"currency":"BTC","rates":{"EUR":"67677"}}}`), []string{"USD", "EUR"}, at2030)
	mustNoProblem(t, rs, err, "coinbase", []string{"USD", "EUR"})
	if len(rs) != 1 || rs[0].Currency != "EUR" {
		t.Fatalf("rates %+v, want EUR only", rs)
	}

	// A non-string, an exponent, a sign, a zero, a null, too many fraction digits: each skipped.
	body := `{"data":{"currency":"BTC","rates":{"USD":75976,"EUR":"6.7e4","GBP":"-1","CAD":"0","CHF":null,` +
		`"AUD":"1.0000000000000000001","JPY":"11979726.3168333333308008"}}}`
	rs, err = parseCoinbase([]byte(body), krakenSeven, at2030)
	mustNoProblem(t, rs, err, "coinbase", krakenSeven)
	if len(rs) != 1 || rs[0].Currency != "JPY" || rs[0].Value.Cmp(dec(t, "11979726.3168333333308008")) != 0 {
		t.Fatalf("rates %+v, want JPY only", rs)
	}

	for _, b := range coinbaseRefusals {
		rs, err := parseCoinbase([]byte(b), krakenSeven, at2030)
		if !errors.Is(err, ErrShape) || len(rs) != 0 {
			t.Errorf("%s: %d rates, err %v, want ErrShape", b, len(rs), err)
		}
	}
}

func TestYadioFixture(t *testing.T) {
	body := fixture(t, "yadio.json")
	var doc struct {
		BTC       map[string]json.Number `json:"BTC"`
		Timestamp json.Number            `json:"timestamp"`
	}
	dc := json.NewDecoder(bytes.NewReader(body))
	dc.UseNumber()
	if err := dc.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	ms, err := doc.Timestamp.Int64()
	if err != nil {
		t.Fatal(err)
	}
	rs, err := parseYadio(body, krakenSeven, at2030)
	mustNoProblem(t, rs, err, "yadio", krakenSeven)
	got := byCurrency(t, rs)
	want := 0
	for _, code := range krakenSeven {
		n, ok := doc.BTC[code]
		if !ok {
			continue
		}
		d, err := ParseDecimal(n.String())
		if err != nil || d.IsZero() {
			continue
		}
		want++
		if got[code].Value.Cmp(d) != 0 {
			t.Errorf("%s = %s, want %s", code, got[code].Value, d)
		}
		if !got[code].At.Equal(time.UnixMilli(ms)) {
			t.Errorf("%s At = %v, want the body's timestamp in ms", code, got[code].At)
		}
	}
	if want == 0 || len(rs) != want {
		t.Fatalf("%d rates, want %d (and at least one)", len(rs), want)
	}
}

func TestYadioErrorBody(t *testing.T) {
	for _, b := range yadioVendorRefusals {
		rs, err := parseYadio([]byte(b), krakenSeven, at2030)
		if !errors.Is(err, ErrVendor) || len(rs) != 0 {
			t.Errorf("%s: %d rates, err %v, want ErrVendor", b, len(rs), err)
		}
	}
	for _, b := range yadioShapeRefusals {
		rs, err := parseYadio([]byte(b), krakenSeven, at2030)
		if !errors.Is(err, ErrShape) || len(rs) != 0 {
			t.Errorf("%s: %d rates, err %v, want ErrShape", b, len(rs), err)
		}
	}
	// Other top-level keys are ignored (the doc example for /exrates/EUR carries a BTC number beside
	// the EUR map).
	rs, err := parseYadio([]byte(`{"EUR":89750.41,"note":{"a":[1]},"BTC":{"USD":76022.31},"base":"BTC","timestamp":1793902508506}`), krakenSeven, at2030)
	if err != nil || len(rs) != 1 {
		t.Fatalf("extra keys: %d rates, err %v", len(rs), err)
	}
}

func TestYadioNumberForms(t *testing.T) {
	body := `{"BTC":{"USD":7.6e4,"EUR":-1,"GBP":0,"JPY":"76022.31","CHF":1E2,"AUD":null,"CAD":76022.31},"base":"BTC","timestamp":1793902508506}`
	rs, err := parseYadio([]byte(body), krakenSeven, at2030)
	mustNoProblem(t, rs, err, "yadio", krakenSeven)
	if len(rs) != 1 || rs[0].Currency != "CAD" || rs[0].Value.Cmp(dec(t, "76022.31")) != 0 {
		t.Fatalf("rates %+v, want CAD 76022.31 only", rs)
	}
	if !rs[0].At.Equal(time.UnixMilli(1793902508506)) {
		t.Fatalf("At = %v", rs[0].At)
	}

	// A timestamp in seconds is read as ms: the rate lands in 1970 and the staleness rule drops it.
	rs, err = parseYadio([]byte(`{"BTC":{"USD":76022.31},"base":"BTC","timestamp":1793902508}`), krakenSeven, at2030)
	if err != nil || len(rs) != 1 {
		t.Fatalf("%d rates, err %v", len(rs), err)
	}
	if rs[0].At.Year() != 1970 {
		t.Fatalf("At = %v, want a time in 1970", rs[0].At)
	}
}

func TestCustomFixture(t *testing.T) {
	rs, err := parseCustom(fixture(t, "custom.json"), AssetBTC, []string{"USD", "EUR", "GBP"})
	mustNoProblem(t, rs, err, "custom", []string{"USD", "EUR", "GBP"})
	got := byCurrency(t, rs)
	if len(got) != 2 || got["USD"].Value.Cmp(dec(t, "76022.31")) != 0 || got["EUR"].Value.Cmp(dec(t, "67979.91")) != 0 {
		t.Fatalf("rates %+v", got)
	}
	for _, r := range rs {
		if !r.At.Equal(time.Unix(1793902508, 0)) {
			t.Errorf("At = %v, want the body's timestamp", r.At)
		}
	}
}

func TestCustomRefusals(t *testing.T) {
	for _, b := range customRefusals {
		rs, err := parseCustom([]byte(b), AssetBTC, []string{"USD"})
		if !errors.Is(err, ErrShape) || len(rs) != 0 {
			t.Errorf("%s: %d rates, err %v, want ErrShape", b, len(rs), err)
		}
	}
	// Invalid UTF-8: the byte 0xFF inside a string.
	bad := []byte(`{"asset":"BTC","rates":{"USD":"1","EUR":"x`)
	bad = append(bad, 0xFF)
	bad = append(bad, []byte(`"},"timestamp":1793902508}`)...)
	if rs, err := parseCustom(bad, AssetBTC, []string{"USD"}); !errors.Is(err, ErrShape) || len(rs) != 0 {
		t.Errorf("invalid UTF-8: %d rates, err %v", len(rs), err)
	}

	// Nesting 6 deep is within the bound: the array value is skipped, USD still parses.
	rs, err := parseCustom([]byte(`{"asset":"BTC","rates":{"EUR":[[[["1"]]]],"USD":"2"},"timestamp":1793902508}`), AssetBTC, []string{"USD", "EUR"})
	if err != nil || len(rs) != 1 || rs[0].Currency != "USD" {
		t.Fatalf("6 deep: %+v, err %v", rs, err)
	}

	// A numeric rate, an exponent, a sign, a zero, a null: skipped; a code not asked for: ignored.
	body := `{"asset":"BTC","rates":{"USD":76022.31,"EUR":"1e3","GBP":"-1","CAD":"0","CHF":null,"COP":"1","JPY":"11979726.5"},"timestamp":1793902508}`
	rs, err = parseCustom([]byte(body), AssetBTC, krakenSeven)
	mustNoProblem(t, rs, err, "custom", krakenSeven)
	if len(rs) != 1 || rs[0].Currency != "JPY" {
		t.Fatalf("rates %+v, want JPY only", rs)
	}

	// Through the client: MaxResponse bytes pass, one more is ErrTooLarge.
	fake, c := fakeClient(t)
	p := Custom(c, "https://rates.example.invalid/rates")
	valid := `{"asset":"BTC","rates":{"USD":"1"},"timestamp":1793902508}`
	fake.Body("custom", []byte(valid+strings.Repeat(" ", MaxResponse-len(valid))))
	if rs, err := p.Fetch(context.Background(), AssetBTC, []string{"USD"}, at2030); err != nil || len(rs) != 1 {
		t.Fatalf("MaxResponse bytes: %d rates, err %v", len(rs), err)
	}
	fake.Body("custom", []byte(valid+strings.Repeat(" ", MaxResponse+1-len(valid))))
	rs, err = p.Fetch(context.Background(), AssetBTC, []string{"USD"}, at2030)
	wantErr(t, rs, err, ErrTooLarge)
}

func TestCustomChecksTheAsset(t *testing.T) {
	fake, c := fakeClient(t)
	p := Custom(c, "https://rates.example.invalid/v1/rates?x=1")
	fake.Body("custom", []byte(`{"asset":"XMR","rates":{"USD":"150.5"},"timestamp":1793902508}`))
	rs, err := p.Fetch(context.Background(), "XMR", []string{"USD"}, at2030)
	if err != nil || len(rs) != 1 || rs[0].Value.Cmp(dec(t, "150.5")) != 0 || !rs[0].At.Equal(time.Unix(1793902508, 0)) {
		t.Fatalf("XMR: %+v, err %v", rs, err)
	}
	if reqs := fake.Requests("custom"); len(reqs) != 1 || reqs[0] != "x=1&asset=XMR" {
		t.Fatalf("requests %q", reqs)
	}
	rs, err = p.Fetch(context.Background(), AssetBTC, []string{"USD"}, at2030)
	wantErr(t, rs, err, ErrShape)

	// The fake's own custom shape, at a configured path.
	fake.Reset()
	fake.Set("custom", "USD", "76050")
	rs, err = Custom(c, "https://rates.example.invalid/v1").Fetch(context.Background(), AssetBTC, []string{"USD"}, at2030)
	if err != nil || len(rs) != 1 || rs[0].Value.Cmp(dec(t, "76050")) != 0 {
		t.Fatalf("a path: %+v, err %v", rs, err)
	}
}

func TestProvidersOverTheFake(t *testing.T) {
	fake, c := fakeClient(t)
	clock := time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC)
	fake.SetClock(func() time.Time { return clock })
	for _, name := range []string{"kraken", "coinbase", "yadio", "custom"} {
		fake.Set(name, "USD", "76000.5")
		fake.Set(name, "EUR", "67000")
	}
	cur := []string{"USD", "EUR", "COP"}
	for _, p := range []Provider{Kraken(c), Coinbase(c), Yadio(c), Custom(c, "https://rates.example.invalid/")} {
		rs, err := p.Fetch(context.Background(), AssetBTC, cur, at2030)
		mustNoProblem(t, rs, err, p.Name(), cur)
		got := byCurrency(t, rs)
		if len(got) != 2 || got["USD"].Value.Cmp(dec(t, "76000.5")) != 0 || got["EUR"].Value.Cmp(dec(t, "67000")) != 0 {
			t.Fatalf("%s: %+v", p.Name(), got)
		}
		wantAt := at2030
		if p.Name() == "yadio" || p.Name() == "custom" {
			wantAt = clock
		}
		if !got["USD"].At.Equal(wantAt) {
			t.Errorf("%s At = %v, want %v", p.Name(), got["USD"].At, wantAt)
		}
	}
	// A vendor error body is ErrVendor; a failing status is ErrStatus with the code.
	fake.VendorError("kraken", true)
	fake.VendorError("yadio", true)
	for _, p := range []Provider{Kraken(c), Yadio(c)} {
		rs, err := p.Fetch(context.Background(), AssetBTC, cur, at2030)
		wantErr(t, rs, err, ErrVendor)
		if strings.Contains(err.Error(), ratestest.Sentinel) {
			t.Errorf("%s: the body reached the error", p.Name())
		}
	}
	fake.Fail("coinbase", 503)
	rs, err := Coinbase(c).Fetch(context.Background(), AssetBTC, cur, at2030)
	wantErr(t, rs, err, ErrStatus)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 503 {
		t.Fatalf("err %v, want a StatusError 503", err)
	}
}

// seedBodies are the bodies every parser target starts from: the recorded fixtures named, a few bodies
// of each shape, and every body of the refusal tables.
func seedBodies(t testing.TB, files ...string) [][]byte {
	t.Helper()
	var out [][]byte
	for _, name := range files {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	for _, s := range []string{
		``, `{}`, `[]`, `null`, `{"error":["EQuery:Unknown asset pair"]}`, `{"error":"currency not found"}`,
		`{"error":[],"result":{"BTC/USD":{"a":["2","1","1.000"],"b":["1","1","1.000"]}}}`,
		`{"error":[],"result":{"BTC/USD":{"a":"2","b":["1"]}}}`,
		`{"data":{"currency":"BTC","rates":{"USD":"75976","EUR":75976,"JPY":"6.7e4"}}}`,
		`{"data":{"currency":"ETH","rates":{"USD":"1"}}}`,
		`{"BTC":{"USD":7.6e4,"EUR":-1,"GBP":0,"JPY":"1","CAD":76022.31},"base":"BTC","timestamp":1793902508506}`,
		`{"asset":"BTC","rates":{"USD":"76022.31","EUR":"67979.91"},"timestamp":1793902508}`,
		`{"asset":"BTC","rates":{"USD":"1"},"timestamp":1793902508,"x":1}`,
		`{"asset":"BTC","rates":{"USD":[[[[["1"]]]]]},"timestamp":1793902508}`,
		"{\"asset\":\"BTC\",\"rates\":{\"USD\":\"\xff\"},\"timestamp\":1}",
		`[["tXMRUSD",150.1,10,150.3,12,-1.2,-0.008,150.2,900,155,148],["tBTCUSD",76000,1,76001,1,0,0,76000,1,1,1]]`,
		`[]`,
	} {
		out = append(out, []byte(s))
	}
	for _, c := range krakenRefusals {
		out = append(out, []byte(c.body))
	}
	for _, table := range [][]string{coinbaseRefusals, yadioVendorRefusals, yadioShapeRefusals, customRefusals,
		bitfinexVendorRefusals, bitfinexShapeRefusals, bitfinexSkips} {
		for _, s := range table {
			out = append(out, []byte(s))
		}
	}
	return out
}

// fuzzSeeds adds seedBodies to a parser's fuzz target.
func fuzzSeeds(f *testing.F, files ...string) {
	for _, b := range seedBodies(f, files...) {
		f.Add(b)
	}
}

// TestFuzzSeedsHoldEveryRefusal: each parser's fuzz target starts from every body of every refusal table,
// so a body a table refuses today is a seed the fuzzer mutates.
func TestFuzzSeedsHoldEveryRefusal(t *testing.T) {
	var refusals []string
	for _, c := range krakenRefusals {
		refusals = append(refusals, c.body)
	}
	refusals = append(refusals, coinbaseRefusals...)
	refusals = append(refusals, yadioVendorRefusals...)
	refusals = append(refusals, yadioShapeRefusals...)
	refusals = append(refusals, customRefusals...)
	refusals = append(refusals, bitfinexVendorRefusals...)
	refusals = append(refusals, bitfinexShapeRefusals...)
	refusals = append(refusals, bitfinexSkips...)
	for _, file := range []string{"kraken.json", "kraken-xmr.json", "coinbase.json", "yadio.json", "custom.json", "bitfinex.json"} {
		seeds := map[string]bool{}
		for _, b := range seedBodies(t, file) {
			seeds[string(b)] = true
		}
		for _, r := range refusals {
			if !seeds[r] {
				t.Errorf("the %s target is not seeded with %q", file, r)
			}
		}
	}
}

func FuzzParseKraken(f *testing.F) {
	fuzzSeeds(f, "kraken.json", "kraken-xmr.json")
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, asset := range []string{AssetBTC, AssetXMR} {
			rs, err := parseKraken(body, asset, fuzzCurrencies, at2030)
			mustNoProblem(t, rs, err, "kraken", fuzzCurrencies)
			for _, r := range rs {
				if !r.At.Equal(at2030) {
					t.Fatal("a Kraken rate not stamped with the fetch time")
				}
			}
		}
	})
}

func FuzzParseCoinbase(f *testing.F) {
	fuzzSeeds(f, "coinbase.json")
	f.Fuzz(func(t *testing.T, body []byte) {
		rs, err := parseCoinbase(body, fuzzCurrencies, at2030)
		mustNoProblem(t, rs, err, "coinbase", fuzzCurrencies)
		for _, r := range rs {
			if !r.At.Equal(at2030) {
				t.Fatal("a Coinbase rate not stamped with the fetch time")
			}
		}
	})
}

func FuzzParseYadio(f *testing.F) {
	fuzzSeeds(f, "yadio.json")
	f.Fuzz(func(t *testing.T, body []byte) {
		rs, err := parseYadio(body, fuzzCurrencies, at2030)
		mustNoProblem(t, rs, err, "yadio", fuzzCurrencies)
	})
}

func FuzzParseCustom(f *testing.F) {
	fuzzSeeds(f, "custom.json")
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, asset := range []string{AssetBTC, "XMR"} {
			rs, err := parseCustom(body, asset, fuzzCurrencies)
			mustNoProblem(t, rs, err, "custom", fuzzCurrencies)
		}
	})
}

// TestPrettyPrintedBodies: whitespace anywhere JSON allows it changes nothing (an operator's custom
// API may well indent its answer).
func TestPrettyPrintedBodies(t *testing.T) {
	cur := []string{"USD", "EUR"}
	cases := []struct {
		name  string
		parse func([]byte) ([]Rate, error)
		body  string
	}{
		{"kraken", func(b []byte) ([]Rate, error) { return parseKraken(b, AssetBTC, cur, at2030) },
			"\n{ \"error\" : [ ] ,\n  \"result\" : {\r\n\t\"BTC/USD\" : { \"a\" : [ \"3\" , \"1\" ] , \"b\" : [ \"1\" ] },\n\t\"BTC/EUR\" : { \"a\" : [ \"4\" ], \"b\" : [ \"4\" ] } } }\n"},
		{"coinbase", func(b []byte) ([]Rate, error) { return parseCoinbase(b, cur, at2030) },
			"{\n  \"data\": {\n    \"currency\": \"BTC\",\n    \"rates\": {\n      \"USD\": \"2\",\n      \"EUR\": \"4\"\n    }\n  }\n}\n"},
		{"yadio", func(b []byte) ([]Rate, error) { return parseYadio(b, cur, at2030) },
			"{\n  \"BTC\": { \"USD\": 2 , \"EUR\": 4 },\n  \"base\": \"BTC\",\n  \"timestamp\": 1793902508506\n}\n"},
		{"custom", func(b []byte) ([]Rate, error) { return parseCustom(b, AssetBTC, cur) },
			"{\n  \"asset\": \"BTC\",\n  \"rates\": {\n    \"USD\": \"2\",\n    \"EUR\": \"4\"\n  },\n  \"timestamp\": 1793902508\n}\n"},
	}
	for _, c := range cases {
		rs, err := c.parse([]byte(c.body))
		mustNoProblem(t, rs, err, c.name, cur)
		got := byCurrency(t, rs)
		if err != nil || len(got) != 2 || got["USD"].Value.Cmp(dec(t, "2")) != 0 || got["EUR"].Value.Cmp(dec(t, "4")) != 0 {
			t.Errorf("%s: %+v, err %v", c.name, rs, err)
		}
	}
}

// TestFixturesAgree: the three recorded answers, each read by its own parser, agree within 2 % for
// every one of Kraken's seven fiats ((max − min) × 10000 ≤ 200 × min), so a parser that reads the
// wrong key, the wrong currency or an inverted rate is caught offline. The seven differ from one
// another by far more than 2 %, so a parser that gave every code the same value would fail too.
func TestFixturesAgree(t *testing.T) {
	kr, err := parseKraken(fixture(t, "kraken.json"), AssetBTC, krakenSeven, at2030)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := parseCoinbase(fixture(t, "coinbase.json"), krakenSeven, at2030)
	if err != nil {
		t.Fatal(err)
	}
	yd, err := parseYadio(fixture(t, "yadio.json"), krakenSeven, at2030)
	if err != nil {
		t.Fatal(err)
	}
	maps := []map[string]Rate{byCurrency(t, kr), byCurrency(t, cb), byCurrency(t, yd)}
	for _, code := range krakenSeven {
		var lo, hi Decimal
		for i, m := range maps {
			r, ok := m[code]
			if !ok {
				t.Fatalf("no %s rate from fixture %d", code, i)
			}
			if i == 0 || r.Value.Cmp(lo) < 0 {
				lo = r.Value
			}
			if i == 0 || r.Value.Cmp(hi) > 0 {
				hi = r.Value
			}
		}
		spread := new(big.Int).Sub(hi.Units(), lo.Units())
		spread.Mul(spread, big.NewInt(10000))
		if spread.Cmp(new(big.Int).Mul(lo.Units(), big.NewInt(200))) > 0 {
			t.Errorf("%s: the fixtures differ by more than 2 %% (low %s, high %s)", code, lo, hi)
		}
	}
	if usd, eur := maps[0]["USD"].Value, maps[0]["EUR"].Value; usd.Cmp(eur) == 0 {
		t.Fatal("USD and EUR are the same value")
	}
}

func TestBitfinexFixture(t *testing.T) {
	body := fixture(t, "bitfinex.json")
	if !bytes.HasPrefix(body, []byte(`[["t`)) {
		t.Fatalf("not a recorded tickers answer: %.40q", body)
	}
	// The expected mids, read from the body's own numbers, independently of the parser.
	var tickers [][]json.RawMessage
	if err := json.Unmarshal(body, &tickers); err != nil {
		t.Fatal(err)
	}
	want := map[string]*big.Int{}
	for _, tk := range tickers {
		var sym string
		// 12 elements as recorded (2026-11-06): the 11 documented ones and one more, not read.
		if len(tk) < 11 || json.Unmarshal(tk[0], &sym) != nil || len(sym) != 7 || sym[0] != 't' {
			t.Fatalf("a recorded ticker of %d elements: %.40q", len(tk), tk)
		}
		want[sym] = midUnits(dec(t, string(tk[3])), dec(t, string(tk[1])))
	}
	for asset, codes := range bitfinexPairs {
		rs, err := parseBitfinex(body, asset, codes, at2030)
		mustNoProblem(t, rs, err, "bitfinex", codes)
		got := byCurrency(t, rs)
		if len(got) != len(codes) {
			t.Fatalf("%s: %d rates of %v, want every code of the table", asset, len(got), codes)
		}
		for code, r := range got {
			w := want["t"+asset+code]
			if w == nil || r.Value.Units().Cmp(w) != 0 || !r.At.Equal(at2030) {
				t.Errorf("%s/%s = %s at %v, want the mid of its bid and ask at the fetch time", asset, code, r.Value, r.At)
			}
		}
	}
}

// TestXMRFixturesAgree: Kraken's and Bitfinex's XMR/USD, recorded in one run, agree within 2 %, and Kraken's EUR is
// not its USD: a parser that read the wrong key or the wrong asset is caught offline.
func TestXMRFixturesAgree(t *testing.T) {
	kr, err := parseKraken(fixture(t, "kraken-xmr.json"), AssetXMR, []string{"USD", "EUR"}, at2030)
	if err != nil || len(kr) != 2 {
		t.Fatalf("kraken-xmr.json: %d rates, %v", len(kr), err)
	}
	bf, err := parseBitfinex(fixture(t, "bitfinex.json"), AssetXMR, []string{"USD"}, at2030)
	if err != nil || len(bf) != 1 {
		t.Fatalf("bitfinex.json: %d XMR rates, %v", len(bf), err)
	}
	k := byCurrency(t, kr)
	lo, hi := k["USD"].Value, bf[0].Value
	if lo.Cmp(hi) > 0 {
		lo, hi = hi, lo
	}
	spread := new(big.Int).Sub(hi.Units(), lo.Units())
	spread.Mul(spread, big.NewInt(10000))
	if spread.Cmp(new(big.Int).Mul(lo.Units(), big.NewInt(200))) > 0 {
		t.Errorf("XMR/USD: the fixtures differ by more than 2 %% (low %s, high %s)", lo, hi)
	}
	if k["USD"].Value.Cmp(k["EUR"].Value) == 0 {
		t.Fatal("Kraken's XMR in USD and EUR are the same value")
	}
	// Each asset's keys only: the XMR body has no BTC rate, the BTC body no XMR rate.
	if rs, err := parseKraken(fixture(t, "kraken-xmr.json"), AssetBTC, krakenSeven, at2030); err != nil || len(rs) != 0 {
		t.Errorf("BTC from the XMR body: %+v, %v", rs, err)
	}
	if rs, err := parseKraken(fixture(t, "kraken.json"), AssetXMR, krakenSeven, at2030); err != nil || len(rs) != 0 {
		t.Errorf("XMR from the BTC body: %+v, %v", rs, err)
	}
}

func TestBitfinexRefusals(t *testing.T) {
	usd := []string{"USD"}
	for _, b := range bitfinexVendorRefusals {
		rs, err := parseBitfinex([]byte(b), AssetXMR, usd, at2030)
		if !errors.Is(err, ErrVendor) || len(rs) != 0 {
			t.Errorf("%s: %d rates, %v; want ErrVendor", b, len(rs), err)
		}
	}
	for _, b := range bitfinexShapeRefusals {
		rs, err := parseBitfinex([]byte(b), AssetXMR, usd, at2030)
		if !errors.Is(err, ErrShape) || len(rs) != 0 {
			t.Errorf("%s: %d rates, %v; want ErrShape", b, len(rs), err)
		}
	}
	for _, b := range bitfinexSkips {
		rs, err := parseBitfinex([]byte(b), AssetXMR, usd, at2030)
		if err != nil || len(rs) != 0 {
			t.Errorf("%s: %+v, %v; want the ticker skipped", b, rs, err)
		}
	}
	// Invalid UTF-8 in a symbol: the whole body is refused.
	if rs, err := parseBitfinex([]byte("[[\"tXMRUSD\xff\",150,1,151,1,0,0,150,1,1,1]]"), AssetXMR, usd, at2030); !errors.Is(err, ErrShape) || len(rs) != 0 {
		t.Errorf("invalid UTF-8: %d rates, %v", len(rs), err)
	}
	// The accepted forms: the floor of the mid; bid equal to ask; more than 11 elements; a skipped ticker beside a good
	// one; whitespace; the second requested code beside the first.
	for _, c := range []struct {
		body string
		want map[string]string
	}{
		{`[["tXMRUSD",150,1,151,1,0,0,150,1,1,1]]`, map[string]string{"USD": "150.5"}},
		{`[["tXMRUSD",0.000000000000000001,1,0.000000000000000002,1,0,0,1,1,1,1]]`, map[string]string{"USD": "0.000000000000000001"}},
		{`[["tXMRUSD",150.25,1,150.25,1,0,0,150,1,1,1]]`, map[string]string{"USD": "150.25"}},
		{`[["tXMRUSD",150,1,152,1,0,0,150,1,1,1,"extra",[1]]]`, map[string]string{"USD": "151"}},
		{`[["tXMRUSD",151,1,150,1,0,0,150,1,1,1],["tXMREUR",130,1,131,1,0,0,1,1,1,1]]`, map[string]string{"EUR": "130.5"}},
		{" [ [ \"tXMREUR\" , 130 , 1 , 130 , 1 , 0 , 0 , 1 , 1 , 1 , 1 ] ,\n[\"tXMRUSD\",150,1,150,1,0,0,1,1,1,1] ] ", map[string]string{"USD": "150", "EUR": "130"}},
	} {
		rs, err := parseBitfinex([]byte(c.body), AssetXMR, []string{"USD", "EUR"}, at2030)
		mustNoProblem(t, rs, err, "bitfinex", []string{"USD", "EUR"})
		got := byCurrency(t, rs)
		if len(got) != len(c.want) {
			t.Fatalf("%s: %+v, want %v", c.body, rs, c.want)
		}
		for code, w := range c.want {
			if got[code].Value.Cmp(dec(t, w)) != 0 || !got[code].At.Equal(at2030) {
				t.Errorf("%s: %s = %s, want %s", c.body, code, got[code].Value, w)
			}
		}
	}
	// A BTC ticker is not an XMR rate, nor the other way round.
	body := []byte(`[["tBTCUSD",76000,1,76001,1,0,0,1,1,1,1],["tXMRUSD",150,1,151,1,0,0,1,1,1,1]]`)
	for asset, want := range map[string]string{AssetBTC: "76000.5", AssetXMR: "150.5"} {
		rs, err := parseBitfinex(body, asset, []string{"USD"}, at2030)
		if err != nil || len(rs) != 1 || rs[0].Value.Cmp(dec(t, want)) != 0 {
			t.Errorf("%s: %+v, %v; want %s", asset, rs, err, want)
		}
	}
}

func TestBitfinexRequestsOnlyItsPairs(t *testing.T) {
	fake, c := fakeClient(t)
	fake.SetAsset("bitfinex", "XMR", "USD", "150.5")
	fake.SetAsset("bitfinex", "XMR", "EUR", "130")
	fake.Set("bitfinex", "USD", "76000")
	fake.Set("bitfinex", "EUR", "67000")
	rs, err := Bitfinex(c).Fetch(context.Background(), AssetXMR, []string{"USD", "EUR"}, at2030)
	if err != nil || len(rs) != 1 || rs[0].Currency != "USD" || rs[0].Value.Cmp(dec(t, "150.5")) != 0 || rs[0].Provider != "bitfinex" || !rs[0].At.Equal(at2030) {
		t.Fatalf("XMR: %+v, %v", rs, err)
	}
	if reqs := fake.Requests("bitfinex"); len(reqs) != 1 || reqs[0] != "symbols=tXMRUSD" {
		t.Fatalf("requests %q", reqs)
	}
	rs, err = Bitfinex(c).Fetch(context.Background(), AssetBTC, []string{"EUR", "COP", "USD", "EUR"}, at2030)
	if got := byCurrency(t, rs); err != nil || len(got) != 2 || got["USD"].Value.Cmp(dec(t, "76000")) != 0 || got["EUR"].Value.Cmp(dec(t, "67000")) != 0 {
		t.Fatalf("BTC: %+v, %v", rs, err)
	}
	if reqs := fake.Requests("bitfinex"); len(reqs) != 2 || reqs[1] != "symbols=tBTCEUR,tBTCUSD" {
		t.Fatalf("requests %q", reqs)
	}
	for _, x := range []struct {
		asset string
		cur   []string
	}{{AssetXMR, []string{"CHF"}}, {AssetXMR, []string{"EUR", "GBP"}}, {AssetBTC, []string{"CHF", "COP"}}, {AssetBTC, nil}} {
		rs, err := Bitfinex(c).Fetch(context.Background(), x.asset, x.cur, at2030)
		if err != nil || len(rs) != 0 {
			t.Fatalf("%s %v: %+v, %v", x.asset, x.cur, rs, err)
		}
	}
	if n := fake.Hits("bitfinex"); n != 2 {
		t.Fatalf("%d requests, want 2: a currency outside the table made one", n)
	}
	// The vendor's error answer.
	fake.VendorError("bitfinex", true)
	rs, err = Bitfinex(c).Fetch(context.Background(), AssetXMR, []string{"USD"}, at2030)
	wantErr(t, rs, err, ErrVendor)
	if strings.Contains(err.Error(), ratestest.Sentinel) {
		t.Fatal("the body reached the error")
	}
}

func FuzzParseBitfinex(f *testing.F) {
	fuzzSeeds(f, "bitfinex.json")
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, asset := range []string{AssetBTC, AssetXMR, "ETH"} {
			rs, err := parseBitfinex(body, asset, fuzzCurrencies, at2030)
			mustNoProblem(t, rs, err, "bitfinex", fuzzCurrencies)
			for _, r := range rs {
				if !r.At.Equal(at2030) {
					t.Fatal("a Bitfinex rate not stamped with the fetch time")
				}
			}
		}
	})
}

func TestKrakenXMR(t *testing.T) {
	fake, c := fakeClient(t)
	fake.SetAsset("kraken", "XMR", "USD", "150.5")
	fake.SetAsset("kraken", "XMR", "EUR", "130")
	fake.Set("kraken", "USD", "76000")
	rs, err := Kraken(c).Fetch(context.Background(), AssetXMR, []string{"USD", "EUR", "GBP"}, at2030)
	got := byCurrency(t, rs)
	if err != nil || len(got) != 2 || got["USD"].Value.Cmp(dec(t, "150.5")) != 0 || got["EUR"].Value.Cmp(dec(t, "130")) != 0 {
		t.Fatalf("XMR: %+v, %v", rs, err)
	}
	reqs := fake.Requests("kraken")
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	q, err := url.ParseQuery(reqs[0])
	if err != nil || q.Get("pair") != "XMRUSD,XMREUR" || q.Get("assetVersion") != "1" {
		t.Fatalf("query %q", reqs[0])
	}
	// The recorded answer: each rate the floor of the mid of its a[0] and b[0].
	body := fixture(t, "kraken-xmr.json")
	var raw struct {
		Result map[string]struct{ A, B []string }
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	rs, err = parseKraken(body, AssetXMR, []string{"USD", "EUR"}, at2030)
	mustNoProblem(t, rs, err, "kraken", []string{"USD", "EUR"})
	if len(rs) != 2 {
		t.Fatalf("the fixture: %d rates", len(rs))
	}
	for _, r := range rs {
		p := raw.Result[krakenKey(AssetXMR, r.Currency)]
		if len(p.A) == 0 || len(p.B) == 0 || r.Value.Units().Cmp(midUnits(dec(t, p.A[0]), dec(t, p.B[0]))) != 0 {
			t.Errorf("%s = %s, not the mid of the fixture's pair", r.Currency, r.Value)
		}
	}
	// An asset outside the table, or no code of the asset's list: no request.
	rs, err = Kraken(c).Fetch(context.Background(), "ETH", []string{"USD"}, at2030)
	wantErr(t, rs, err, ErrAsset)
	if rs, err := Kraken(c).Fetch(context.Background(), AssetXMR, []string{"GBP", "AUD"}, at2030); err != nil || len(rs) != 0 {
		t.Fatalf("XMR in GBP and AUD: %+v, %v", rs, err)
	}
	if n := fake.Hits("kraken"); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

// TestCustomAssetQuery: the custom API is asked for the asset in an asset query parameter, appended to the
// configured URL's own query when it has one.
func TestCustomAssetQuery(t *testing.T) {
	fake, c := fakeClient(t)
	fake.Set("custom", "USD", "76000")
	fake.SetAsset("custom", "XMR", "USD", "150.5")
	for u, want := range map[string]string{
		"https://rates.example.invalid":               "asset=",
		"https://rates.example.invalid/v1/rates":      "asset=",
		"https://rates.example.invalid/v1/rates?x=1":  "x=1&asset=",
		"https://rates.example.invalid/v1/rates?":     "asset=",
		"https://rates.example.invalid/v1/rates?x=1&": "x=1&asset=",
		"https://rates.example.invalid/?k=a%26b&y=2":  "k=a%26b&y=2&asset=",
	} {
		fake.Reset()
		fake.Set("custom", "USD", "76000")
		fake.SetAsset("custom", "XMR", "USD", "150.5")
		for asset, rate := range map[string]string{AssetXMR: "150.5", AssetBTC: "76000"} {
			rs, err := Custom(c, u).Fetch(context.Background(), asset, []string{"USD"}, at2030)
			if err != nil || len(rs) != 1 || rs[0].Value.Cmp(dec(t, rate)) != 0 {
				t.Fatalf("%s %s: %+v, %v", u, asset, rs, err)
			}
			reqs := fake.Requests("custom")
			if got := reqs[len(reqs)-1]; got != want+asset {
				t.Fatalf("%s %s: query %q, want %q", u, asset, got, want+asset)
			}
		}
	}
}

// TestQuotes: the reachability table, at every cell; and for Kraken and Bitfinex, Quotes is exactly what Fetch
// requests given every valid code, so the two never drift apart.
func TestQuotes(t *testing.T) {
	for _, c := range []struct {
		provider, asset, currency string
		want                      bool
	}{
		{"kraken", "BTC", "USD", true}, {"kraken", "BTC", "AUD", true}, {"kraken", "BTC", "COP", false},
		{"kraken", "XMR", "USD", true}, {"kraken", "XMR", "EUR", true}, {"kraken", "XMR", "GBP", false},
		{"kraken", "ETH", "USD", false}, {"kraken", "", "USD", false},
		{"bitfinex", "XMR", "USD", true}, {"bitfinex", "XMR", "EUR", false}, {"bitfinex", "BTC", "USD", true},
		{"bitfinex", "BTC", "CHF", false}, {"bitfinex", "BTC", "JPY", false}, {"bitfinex", "BTC", "GBP", true},
		{"bitfinex", "ETH", "USD", false},
		{"coinbase", "BTC", "COP", true}, {"coinbase", "BTC", "ZZZ", true}, {"coinbase", "XMR", "USD", false},
		{"yadio", "BTC", "ARS", true}, {"yadio", "XMR", "USD", false}, {"yadio", "ETH", "USD", false},
		{"custom", "XMR", "ZZZ", true}, {"custom", "BTC", "USD", true}, {"custom", "ETH", "USD", true},
		{"unknown", "BTC", "USD", false}, {"", "BTC", "USD", false}, {"Kraken", "BTC", "USD", false},
		{"kraken ", "BTC", "USD", false},
	} {
		if got := Quotes(c.provider, c.asset, c.currency); got != c.want {
			t.Errorf("Quotes(%q, %q, %q) = %v, want %v", c.provider, c.asset, c.currency, got, c.want)
		}
	}
	var all []string
	for a := 'A'; a <= 'Z'; a++ {
		for b := 'A'; b <= 'Z'; b++ {
			for c := 'A'; c <= 'Z'; c++ {
				if s := string([]rune{a, b, c}); ValidCurrency(s) {
					all = append(all, s)
				}
			}
		}
	}
	fake, c := fakeClient(t)
	for _, p := range []Provider{Kraken(c), Bitfinex(c)} {
		for _, asset := range []string{AssetBTC, AssetXMR} {
			if _, err := p.Fetch(context.Background(), asset, all, at2030); err != nil {
				t.Fatal(err)
			}
			reqs := fake.Requests(p.Name())
			q, err := url.ParseQuery(reqs[len(reqs)-1])
			if err != nil {
				t.Fatal(err)
			}
			asked := q.Get("pair") + q.Get("symbols")
			var want []string
			for _, code := range all {
				if Quotes(p.Name(), asset, code) {
					if p.Name() == "kraken" {
						want = append(want, map[string]string{AssetBTC: "XBT", AssetXMR: "XMR"}[asset]+code)
					} else {
						want = append(want, "t"+asset+code)
					}
				}
			}
			if len(want) == 0 || asked != strings.Join(want, ",") {
				t.Errorf("%s %s asks for %q; Quotes says %q", p.Name(), asset, asked, want)
			}
		}
	}
}

// BitfinexFiats is the BTC codes Bitfinex is asked for, the ones Quotes counts for it, as a copy; the provider name
// constants are the providers' names.
func TestBitfinexFiatsAndNames(t *testing.T) {
	fiats := BitfinexFiats()
	if !equalStrings(fiats, []string{"USD", "EUR", "GBP"}) {
		t.Fatalf("BitfinexFiats is %v", fiats)
	}
	for _, c := range fiats {
		if !Quotes(ProviderBitfinex, AssetBTC, c) {
			t.Fatalf("Quotes does not count bitfinex for BTC in %s", c)
		}
	}
	if Quotes(ProviderBitfinex, AssetBTC, "JPY") {
		t.Fatal("Quotes counts bitfinex for BTC in JPY")
	}
	fiats[0] = "COP"
	if BitfinexFiats()[0] != "USD" {
		t.Fatal("BitfinexFiats shares its slice with the provider")
	}
	c := NewClient(nil)
	if Kraken(c).Name() != ProviderKraken || Coinbase(c).Name() != ProviderCoinbase || Yadio(c).Name() != ProviderYadio ||
		Bitfinex(c).Name() != ProviderBitfinex || ProviderKraken != "kraken" || ProviderCoinbase != "coinbase" {
		t.Fatal("a provider's name is not its constant")
	}
}
