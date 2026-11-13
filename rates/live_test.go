//go:build live

// The live tests call the real providers, once each per run. The smoke test:
// go test -tags live -run '^TestLiveSmoke$' -v ./rates/
//
// TestLiveRecord rewrites rates/testdata, so it runs only when asked by name and by the environment:
// SATBOOTH_RECORD_FIXTURES=1 go test -tags live -run '^TestLiveRecord$' -v ./rates/

package rates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// metaHeaders are the response headers TestLiveRecord keeps, besides any X-RateLimit-*.
var metaHeaders = []string{"Content-Type", "Cache-Control", "Date", "Age", "RateLimit", "RateLimit-Policy"}

type recording struct {
	name   string
	body   []byte
	status int
}

// TestLiveRecord fetches each built-in provider's answer once (Kraken's twice: BTC and XMR), writes the body (Kraken's
// and Coinbase's trimmed to what the parser tests read, Yadio's and Bitfinex's whole) and its metadata to testdata, and
// then checks what the vendors' docs do not say. Every file is written before any check runs, so a failed check never
// calls for a second request.
func TestLiveRecord(t *testing.T) {
	if os.Getenv("SATBOOTH_RECORD_FIXTURES") != "1" {
		t.Skip("rewrites rates/testdata: set SATBOOTH_RECORD_FIXTURES=1 to record")
	}
	c := NewClient(nil)
	keep := func(b []byte) ([]byte, error) { return b, nil }
	btc := KrakenFiats()
	urls := []struct {
		name, url string
		trim      func([]byte) ([]byte, error)
	}{
		{"kraken", krakenTickerURL(AssetBTC, btc), func(b []byte) ([]byte, error) { return trimKraken(b, AssetBTC, btc) }},
		{"kraken-xmr", krakenTickerURL(AssetXMR, []string{"USD", "EUR"}),
			func(b []byte) ([]byte, error) { return trimKraken(b, AssetXMR, []string{"USD", "EUR"}) }},
		{"coinbase", coinbaseURL, func(b []byte) ([]byte, error) { return trimCoinbase(b, btc) }},
		{"yadio", yadioURL, keep},
		{"bitfinex", bitfinexBase + "?symbols=" + strings.Join(bitfinexLiveSymbols, ","), keep},
	}
	got := map[string]recording{}
	for _, u := range urls {
		rec, err := record(c, u.name, u.url, u.trim)
		if err != nil {
			t.Errorf("%s: %v", u.name, err)
			continue
		}
		got[u.name] = rec
	}
	if t.Failed() {
		t.FailNow()
	}
	now := time.Now()
	usd := map[string]Decimal{}

	// (1) Kraken's result keys with assetVersion=1 are exactly BTC/<code> for all seven.
	var kr struct {
		Error  []string                   `json:"error"`
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(got["kraken"].body, &kr); err != nil {
		t.Fatalf("kraken: %v", err)
	}
	var keys []string
	for k := range kr.Result {
		keys = append(keys, k)
	}
	t.Logf("kraken: status %d, error %q, result keys %q", got["kraken"].status, kr.Error, keys)
	for _, code := range btc {
		if _, ok := kr.Result["BTC/"+code]; !ok {
			t.Errorf("kraken: no key BTC/%s: assetVersion=1 does not rename every key, use the fixed table", code)
		}
	}
	if len(kr.Result) != len(btc) { // after the trim: a key missing above
		t.Errorf("kraken: %d result keys, want %d", len(kr.Result), len(btc))
	}
	rs, err := parseKraken(got["kraken"].body, AssetBTC, btc, now)
	if err != nil || len(rs) != len(btc) {
		t.Errorf("kraken: %d rates, err %v", len(rs), err)
	}
	for _, r := range rs {
		t.Logf("kraken %s %s", r.Currency, r.Value)
		if r.Currency == "USD" {
			usd["kraken"] = r.Value
		}
	}

	// (4) The longest fraction among Coinbase's rates kept: the seven, plus the code with the longest fraction of the
	// whole answer when it is longer (trimCoinbase).
	var cb struct {
		Data struct {
			Rates map[string]string `json:"rates"`
		} `json:"data"`
	}
	if err := json.Unmarshal(got["coinbase"].body, &cb); err != nil {
		t.Fatalf("coinbase: %v", err)
	}
	longest, longestCode := 0, ""
	for code, s := range cb.Data.Rates {
		if i := strings.IndexByte(s, '.'); i >= 0 && len(s)-i-1 > longest {
			longest, longestCode = len(s)-i-1, code
		}
	}
	t.Logf("coinbase: status %d, %d rate keys, longest fraction %d digits (%s; ParseDecimal refuses more than %d)",
		got["coinbase"].status, len(cb.Data.Rates), longest, longestCode, MaxFracDigits)
	rs, err = parseCoinbase(got["coinbase"].body, btc, now)
	if err != nil || len(rs) != len(btc) {
		t.Errorf("coinbase: %d rates, err %v", len(rs), err)
	}
	for _, r := range rs {
		t.Logf("coinbase %s %s", r.Currency, r.Value)
		if r.Currency == "USD" {
			usd["coinbase"] = r.Value
		}
	}

	// (3) Yadio's timestamp is in ms and within 10 minutes of now; the lag is logged.
	var yd struct {
		Timestamp json.Number `json:"timestamp"`
	}
	dc := json.NewDecoder(bytes.NewReader(got["yadio"].body))
	dc.UseNumber()
	if err := dc.Decode(&yd); err != nil {
		t.Fatalf("yadio: %v", err)
	}
	ms, err := yd.Timestamp.Int64()
	if err != nil {
		t.Fatalf("yadio timestamp: %v", err)
	}
	lag := now.Sub(time.UnixMilli(ms))
	t.Logf("yadio: status %d, timestamp %d, lag %v (the default max_age_seconds is 300)", got["yadio"].status, ms, lag.Round(time.Second))
	if len(yd.Timestamp.String()) != 13 || lag < -time.Minute || lag > 10*time.Minute {
		t.Errorf("yadio: the timestamp is not ms within 10 minutes of now")
	}
	rs, err = parseYadio(got["yadio"].body, btc, now)
	if err != nil || len(rs) != len(btc) {
		t.Errorf("yadio: %d rates, err %v", len(rs), err)
	}
	for _, r := range rs {
		t.Logf("yadio %s %s", r.Currency, r.Value)
		if r.Currency == "USD" {
			usd["yadio"] = r.Value
		}
	}

	// (2) The three USD rates agree within 2 %: (max − min) × 10000 ≤ 200 × min.
	if len(usd) != 3 {
		t.Fatalf("USD from %d providers, want 3", len(usd))
	}
	lo, hi := usd["kraken"], usd["kraken"]
	for _, v := range usd {
		if v.Cmp(lo) < 0 {
			lo = v
		}
		if v.Cmp(hi) > 0 {
			hi = v
		}
	}
	spread := new(big.Int).Sub(hi.Units(), lo.Units())
	spread.Mul(spread, big.NewInt(10000))
	bps := new(big.Int).Quo(spread, lo.Units())
	t.Logf("USD: low %s, high %s, spread %s bps", lo, hi, bps)
	if spread.Cmp(new(big.Int).Mul(lo.Units(), big.NewInt(200))) > 0 {
		t.Errorf("the USD rates differ by more than 2%%")
	}

	// (5) Kraken's XMR keys with assetVersion=1 are XMR/USD and XMR/EUR (else the fallback keys XXMRZUSD and XXMRZEUR).
	var kx struct {
		Error  []string                   `json:"error"`
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(got["kraken-xmr"].body, &kx); err != nil {
		t.Fatalf("kraken-xmr: %v", err)
	}
	keys = keys[:0]
	for k := range kx.Result {
		keys = append(keys, k)
	}
	t.Logf("kraken-xmr: status %d, error %q, result keys %q", got["kraken-xmr"].status, kx.Error, keys)
	for _, code := range []string{"USD", "EUR"} {
		if _, ok := kx.Result[krakenKey(AssetXMR, code)]; !ok {
			t.Errorf("kraken-xmr: no key %s: use the fallback keys XXMRZUSD and XXMRZEUR", krakenKey(AssetXMR, code))
		}
	}
	xmr := map[string]Decimal{}
	rs, err = parseKraken(got["kraken-xmr"].body, AssetXMR, []string{"USD", "EUR"}, now)
	if err != nil || len(rs) != 2 {
		t.Errorf("kraken-xmr: %d rates, err %v", len(rs), err)
	}
	for _, r := range rs {
		t.Logf("kraken XMR %s %s", r.Currency, r.Value)
		if r.Currency == "USD" {
			xmr["kraken"] = r.Value
		}
	}

	// (6) Bitfinex answers an array of at least 11 elements for each symbol of bitfinexPairs (12 on 2026-11-06); which
	// of the symbols asked it quotes (it leaves out one it does not: tBTCJPY on 2026-11-06).
	var bf [][]json.RawMessage
	if err := json.Unmarshal(got["bitfinex"].body, &bf); err != nil {
		t.Fatalf("bitfinex: %v", err)
	}
	present := map[string]bool{}
	for _, tk := range bf {
		var sym string
		if len(tk) > 0 {
			json.Unmarshal(tk[0], &sym)
		}
		t.Logf("bitfinex: %s, %d elements", sym, len(tk))
		if len(tk) < 11 {
			t.Errorf("bitfinex: %s has %d elements, want at least 11", sym, len(tk))
		}
		present[sym] = true
	}
	for _, sym := range bitfinexLiveSymbols {
		if !present[sym] {
			t.Logf("bitfinex: no ticker %s", sym)
		}
	}
	for asset, codes := range bitfinexPairs {
		for _, code := range codes {
			if !present[bitfinexSymbol(asset, code)] {
				t.Errorf("bitfinex: no ticker %s: drop it from bitfinexPairs", bitfinexSymbol(asset, code))
			}
		}
	}
	for asset, codes := range bitfinexPairs {
		rs, err = parseBitfinex(got["bitfinex"].body, asset, codes, now)
		if err != nil || len(rs) != len(codes) {
			t.Errorf("bitfinex %s: %d rates of %v, err %v", asset, len(rs), codes, err)
		}
		for _, r := range rs {
			t.Logf("bitfinex %s %s %s", asset, r.Currency, r.Value)
			if asset == AssetXMR && r.Currency == "USD" {
				xmr["bitfinex"] = r.Value
			}
		}
	}

	// (7) Kraken's and Bitfinex's XMR/USD agree within 2 %.
	if len(xmr) != 2 {
		t.Fatalf("XMR/USD from %d providers, want 2", len(xmr))
	}
	lo, hi = xmr["kraken"], xmr["bitfinex"]
	if lo.Cmp(hi) > 0 {
		lo, hi = hi, lo
	}
	spread = new(big.Int).Sub(hi.Units(), lo.Units())
	spread.Mul(spread, big.NewInt(10000))
	t.Logf("XMR/USD: low %s, high %s, spread %s bps", lo, hi, new(big.Int).Quo(spread, lo.Units()))
	if spread.Cmp(new(big.Int).Mul(lo.Units(), big.NewInt(200))) > 0 {
		t.Errorf("the XMR/USD rates differ by more than 2%%")
	}
}

// bitfinexLiveSymbols are the tickers TestLiveRecord asks Bitfinex for: XMR/USD and the BTC pairs its table may hold
// (tBTCJPY was absent on 2026-11-06, so bitfinexPairs leaves it out).
var bitfinexLiveSymbols = []string{"tXMRUSD", "tBTCUSD", "tBTCEUR", "tBTCGBP", "tBTCJPY"}

// record makes one GET and writes testdata/<name>.json, the body as trim keeps it (trimKraken and trimCoinbase keep
// only what the parser tests read; a body they refuse is not written), and testdata/<name>.meta.json.
func record(c *http.Client, name, url string, trim func([]byte) ([]byte, error)) (recording, error) {
	ctx, cancel := context.WithTimeout(context.Background(), FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return recording{}, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return recording{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil {
		return recording{}, err
	}
	if resp.StatusCode == http.StatusOK {
		if body, err = trim(body); err != nil {
			return recording{}, err
		}
	}
	headers := map[string]string{}
	for k, v := range resp.Header {
		keep := strings.HasPrefix(k, "X-Ratelimit-")
		for _, h := range metaHeaders {
			keep = keep || k == http.CanonicalHeaderKey(h)
		}
		if keep {
			headers[k] = strings.Join(v, ", ")
		}
	}
	meta, err := json.MarshalIndent(struct {
		RecordedAt string            `json:"recorded_at"`
		Status     int               `json:"status"`
		Headers    map[string]string `json:"headers"`
	}{time.Now().UTC().Format(time.RFC3339), resp.StatusCode, headers}, "", "  ")
	if err != nil {
		return recording{}, err
	}
	if err := os.WriteFile(filepath.Join("testdata", name+".json"), body, 0o644); err != nil {
		return recording{}, err
	}
	if err := os.WriteFile(filepath.Join("testdata", name+".meta.json"), append(meta, '\n'), 0o644); err != nil {
		return recording{}, err
	}
	return recording{name: name, body: body, status: resp.StatusCode}, nil
}

// TestLiveSmoke calls each built-in provider's Fetch once, over the production client, for BTC in USD and EUR (and Kraken
// and Bitfinex once more, for XMR in USD), then runs
// Aggregate with the service's default policy over the rates it got. It logs each rate and its time, the median, the
// spread and whether an order would be priced. It fails on an answer the parsers refuse (ErrShape, ErrVendor,
// ErrTooLarge: the vendor's format changed) and on an HTTP 4xx other than 429 (the request is wrong), never on a
// provider that is down or limiting (ErrTransport, a 429 or a 5xx) or on too few or disagreeing rates (ErrTooFew,
// ErrDisagree). See liveFailure.
func TestLiveSmoke(t *testing.T) {
	c := NewClient(nil)
	currencies := []string{"USD", "EUR"}
	now := time.Now()
	var got []Rate
	for _, p := range []Provider{Kraken(c), Coinbase(c), Yadio(c), Bitfinex(c)} {
		ctx, cancel := context.WithTimeout(context.Background(), FetchTimeout+time.Second)
		rs, err := p.Fetch(ctx, AssetBTC, currencies, now)
		cancel()
		switch {
		case liveFailure(err):
			t.Errorf("%s: %v: the parser refuses the vendor's answer, or the provider refuses SatBooth's request", p.Name(), err)
			continue
		case err != nil:
			t.Logf("%s: unavailable (%v); a provider down is not a failure here", p.Name(), err)
			continue
		}
		for _, r := range rs {
			r.Provider = p.Name()
			t.Logf("%s %s %s at %s", r.Provider, r.Currency, r.Value, r.At.UTC().Format(time.RFC3339))
			got = append(got, r)
		}
	}
	// XMR in USD: Kraken's XMR/USD key and Bitfinex's tXMRUSD symbol, the two sources the default rules need.
	var xmr []Rate
	for _, p := range []Provider{Kraken(c), Bitfinex(c)} {
		ctx, cancel := context.WithTimeout(context.Background(), FetchTimeout+time.Second)
		rs, err := p.Fetch(ctx, AssetXMR, []string{"USD"}, now)
		cancel()
		switch {
		case liveFailure(err):
			t.Errorf("%s XMR: %v: the parser refuses the vendor's answer, or the provider refuses SatBooth's request", p.Name(), err)
			continue
		case err != nil:
			t.Logf("%s XMR: unavailable (%v); a provider down is not a failure here", p.Name(), err)
			continue
		case len(rs) != 1:
			t.Errorf("%s XMR: %d USD rates, want 1: the key or symbol is no longer XMR/USD", p.Name(), len(rs))
			continue
		}
		t.Logf("%s XMR USD %s", p.Name(), rs[0].Value)
		xmr = append(xmr, rs[0])
	}
	if q, err := Aggregate(AssetXMR, "USD", xmr, time.Now(), Policy{MinSources: 2, MaxSpreadBps: 200, MaxAge: 300 * time.Second}); err != nil {
		t.Logf("XMR/USD: the default rules would NOT price an order: %v", err)
	} else {
		t.Logf("XMR/USD: median %s from %d sources; the default rules would price an order", q.Rate, len(q.Sources))
	}
	// The service's defaults: min_sources 2, max_spread_percent 2, max_age_seconds 300.
	policy := Policy{MinSources: 2, MaxSpreadBps: 200, MaxAge: 300 * time.Second}
	for _, cur := range currencies {
		var rs []Rate
		for _, r := range got {
			if r.Currency == cur {
				rs = append(rs, r)
			}
		}
		if len(rs) >= 2 {
			lo, hi := rs[0].Value, rs[0].Value
			for _, r := range rs[1:] {
				if r.Value.Cmp(lo) < 0 {
					lo = r.Value
				}
				if r.Value.Cmp(hi) > 0 {
					hi = r.Value
				}
			}
			diff := new(big.Int).Sub(hi.Units(), lo.Units())
			diff.Mul(diff, big.NewInt(10000))
			t.Logf("%s: %d rates, low %s, high %s, spread %s bps (the default bound is 200)", cur, len(rs), lo, hi, diff.Quo(diff, lo.Units()))
		}
		q, err := Aggregate(AssetBTC, cur, rs, time.Now(), policy)
		switch {
		case errors.Is(err, ErrTooFew), errors.Is(err, ErrDisagree):
			t.Logf("%s: the default rules would NOT price an order: %v", cur, err)
		case err != nil:
			t.Errorf("%s: Aggregate: %v", cur, err)
		default:
			t.Logf("%s: median %s from %d sources; the default rules would price an order", cur, q.Rate, len(q.Sources))
		}
	}
}
