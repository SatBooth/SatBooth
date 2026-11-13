package ratestest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func fetch(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

const (
	krakenURL   = "https://api.kraken.com/0/public/Ticker?assetVersion=1&pair=XBTUSD,XBTEUR"
	coinbaseURL = "https://api.coinbase.com/v2/exchange-rates?currency=BTC"
	yadioURL    = "https://api.yadio.io/exrates/BTC"
	customURL   = "https://rates.example.invalid/v1/rates?key=1"
)

func TestShapes(t *testing.T) {
	s := New(t)
	c := &http.Client{Transport: s.Transport()}
	now := time.Date(2031, 1, 2, 3, 4, 5, 600_000_000, time.UTC)
	s.SetClock(func() time.Time { return now })
	for _, p := range []string{"kraken", "coinbase", "yadio", "custom"} {
		s.Set(p, "USD", "76000.5")
		s.Set(p, "EUR", "67000")
	}

	code, body := fetch(t, c, krakenURL)
	want := `{"error":[],"result":{"BTC/EUR":{"a":["67000","1","1.000"],"b":["67000","1","1.000"],"c":["67000","0.1"]},` +
		`"BTC/USD":{"a":["76000.5","1","1.000"],"b":["76000.5","1","1.000"],"c":["76000.5","0.1"]}}}`
	if code != 200 || body != want {
		t.Fatalf("kraken %d %s", code, body)
	}
	// Only the pairs asked for.
	if _, body := fetch(t, c, "https://api.kraken.com/0/public/Ticker?assetVersion=1&pair=XBTUSD,XBTJPY"); strings.Contains(body, "EUR") || !strings.Contains(body, "BTC/USD") {
		t.Fatalf("kraken pair filter: %s", body)
	}
	code, body = fetch(t, c, coinbaseURL)
	if code != 200 || body != `{"data":{"currency":"BTC","rates":{"EUR":"67000","USD":"76000.5"}}}` {
		t.Fatalf("coinbase %d %s", code, body)
	}
	code, body = fetch(t, c, yadioURL)
	if code != 200 || body != `{"BTC":{"EUR":67000,"USD":76000.5},"base":"BTC","timestamp":1925089445600}` {
		t.Fatalf("yadio %d %s", code, body)
	}
	if now.UnixMilli() != 1925089445600 {
		t.Fatal("the test's clock arithmetic is off")
	}
	code, body = fetch(t, c, customURL)
	if code != 200 || body != `{"asset":"BTC","rates":{"EUR":"67000","USD":"76000.5"},"timestamp":1925089445}` {
		t.Fatalf("custom %d %s", code, body)
	}
	// The custom shape at the bare host and under any path.
	for _, u := range []string{"https://rates.example.invalid", "https://rates.example.invalid/", "https://rates.example.invalid/a/b"} {
		if code, _ := fetch(t, c, u); code != 200 {
			t.Fatalf("%s: %d", u, code)
		}
	}
	// Other paths of a vendor host are 404 and count no hit.
	before := s.Hits("yadio")
	if code, _ := fetch(t, c, "https://api.yadio.io/exrates/EUR"); code != 404 {
		t.Fatalf("an unknown path: %d", code)
	}
	if s.Hits("yadio") != before {
		t.Fatal("a 404 counted as a hit")
	}
}

func TestSetAt(t *testing.T) {
	s := New(t)
	c := &http.Client{Transport: s.Transport()}
	s.Set("yadio", "USD", "1")
	s.Set("custom", "USD", "1")
	at := time.Unix(1793902508, 506_000_000)
	s.SetAt("yadio", at)
	s.SetAt("custom", at)
	s.SetAt("kraken", at) // ignored
	if _, b := fetch(t, c, yadioURL); !strings.Contains(b, `"timestamp":1793902508506}`) {
		t.Fatalf("yadio %s", b)
	}
	if _, b := fetch(t, c, customURL); !strings.Contains(b, `"timestamp":1793902508}`) {
		t.Fatalf("custom %s", b)
	}
	// The default clock is the wall clock.
	s.Reset()
	s.Set("yadio", "USD", "1")
	before := time.Now().UnixMilli()
	_, b := fetch(t, c, yadioURL)
	var doc struct {
		Timestamp int64 `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(b), &doc); err != nil || doc.Timestamp < before || doc.Timestamp > time.Now().UnixMilli() {
		t.Fatalf("yadio's default timestamp %d (%v), want the wall clock", doc.Timestamp, err)
	}
}

func TestFailVendorErrorBody(t *testing.T) {
	s := New(t)
	c := &http.Client{Transport: s.Transport()}
	s.Set("coinbase", "USD", "1")
	s.Fail("coinbase", 503)
	code, body := fetch(t, c, coinbaseURL)
	if code != 503 || body != `{"error":"NEVER-IN-A-LOG"}` || Sentinel != "NEVER-IN-A-LOG" {
		t.Fatalf("Fail: %d %s", code, body)
	}
	s.Fail("coinbase", 0)
	if code, _ := fetch(t, c, coinbaseURL); code != 200 {
		t.Fatalf("Fail cleared: %d", code)
	}

	s.VendorError("kraken", true)
	s.VendorError("yadio", true)
	if code, body := fetch(t, c, krakenURL); code != 200 || body != `{"error":["EGeneral:NEVER-IN-A-LOG"]}` {
		t.Fatalf("kraken vendor error: %d %s", code, body)
	}
	if code, body := fetch(t, c, yadioURL); code != 200 || body != `{"error":"NEVER-IN-A-LOG"}` {
		t.Fatalf("yadio vendor error: %d %s", code, body)
	}
	s.VendorError("kraken", false)
	if _, body := fetch(t, c, krakenURL); body != `{"error":[],"result":{}}` {
		t.Fatalf("kraken vendor error cleared: %s", body)
	}

	s.Body("custom", []byte("exact \x00 bytes"))
	if code, body := fetch(t, c, customURL); code != 200 || body != "exact \x00 bytes" {
		t.Fatalf("Body: %d %q", code, body)
	}
	s.Body("custom", []byte{}) // an empty body is a body, not a clear
	if code, body := fetch(t, c, customURL); code != 200 || body != "" {
		t.Fatalf("an empty Body: %d %q", code, body)
	}
	s.Fail("custom", 500) // Fail wins over Body
	if code, _ := fetch(t, c, customURL); code != 500 {
		t.Fatalf("Fail over Body: %d", code)
	}
	s.Fail("custom", 0)
	s.Body("custom", nil) // clears
	if _, body := fetch(t, c, customURL); !strings.HasPrefix(body, `{"asset":"BTC"`) {
		t.Fatalf("Body cleared: %s", body)
	}
}

func TestHitsRequestsReset(t *testing.T) {
	s := New(t)
	c := &http.Client{Transport: s.Transport()}
	fetch(t, c, krakenURL)
	fetch(t, c, "https://api.kraken.com/0/public/Ticker?pair=XBTCHF")
	fetch(t, c, coinbaseURL)
	if s.Hits("kraken") != 2 || s.Hits("coinbase") != 1 || s.Hits("yadio") != 0 {
		t.Fatalf("hits %d %d %d", s.Hits("kraken"), s.Hits("coinbase"), s.Hits("yadio"))
	}
	reqs := s.Requests("kraken")
	if len(reqs) != 2 || reqs[0] != "assetVersion=1&pair=XBTUSD,XBTEUR" || reqs[1] != "pair=XBTCHF" {
		t.Fatalf("requests %q", reqs)
	}
	reqs[0] = "changed"
	if s.Requests("kraken")[0] == "changed" {
		t.Fatal("Requests returns the fake's own slice")
	}
	s.Set("kraken", "USD", "1")
	s.Fail("coinbase", 500)
	s.Delay("yadio", time.Hour)
	s.Reset()
	if s.Hits("kraken") != 0 || len(s.Requests("kraken")) != 0 {
		t.Fatal("Reset kept the hits")
	}
	if _, body := fetch(t, c, krakenURL); body != `{"error":[],"result":{}}` {
		t.Fatalf("Reset kept a rate: %s", body)
	}
	if code, _ := fetch(t, c, coinbaseURL); code != 200 {
		t.Fatal("Reset kept a failure")
	}
	start := time.Now()
	fetch(t, c, yadioURL)
	if time.Since(start) > 5*time.Second {
		t.Fatal("Reset kept a delay")
	}
}

func TestDelayHonoursTheContext(t *testing.T) {
	s := New(t)
	s.Delay("yadio", 200*time.Millisecond)
	c := &http.Client{Transport: s.Transport()}
	start := time.Now()
	if code, _ := fetch(t, c, yadioURL); code != 200 || time.Since(start) < 200*time.Millisecond {
		t.Fatalf("Delay: %d after %v", code, time.Since(start))
	}

	s.Delay("yadio", time.Hour)
	c.Timeout = 100 * time.Millisecond
	start = time.Now()
	if _, err := c.Get(yadioURL); err == nil {
		t.Fatal("no timeout")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the delay outlived the client's timeout: %v", d)
	}
	// The server's Close (at cleanup) does not wait for an hour either: the handler saw the cancel.
}

func TestTransportRefusesUnknownHosts(t *testing.T) {
	s := New(t)
	c := &http.Client{Transport: s.Transport()}
	for _, u := range []string{"https://example.com/", "https://api.kraken.com.example.invalid/", "http://127.0.0.1:1/"} {
		_, err := c.Get(u)
		var op *net.OpError
		if err == nil || !errors.As(err, &op) || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("%s: %v", u, err)
		}
	}
	if s.Hits("kraken")+s.Hits("coinbase")+s.Hits("yadio")+s.Hits("custom") != 0 {
		t.Fatal("a refused host reached the fake")
	}
	// A port on a known host is still that host.
	if code, _ := fetch(t, c, "https://api.yadio.io:443/exrates/BTC"); code != 200 {
		t.Fatalf("with a port: %d", code)
	}
}

func TestTransportDoesNotChangeTheRequest(t *testing.T) {
	s := New(t)
	rt := s.Transport()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, customURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	host := req.Host
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if req.URL.String() != customURL || req.Host != host {
		t.Fatalf("the request was changed: %s %q", req.URL, req.Host)
	}
}

func TestConcurrentUse(t *testing.T) {
	s := New(t)
	c := &http.Client{Transport: s.Transport()}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				s.Set("coinbase", "USD", "1")
				s.SetAt("yadio", time.Now())
				s.Fail("kraken", j%2*500)
				s.VendorError("yadio", j%2 == 0)
				s.Body("custom", []byte("{}"))
				s.Delay("coinbase", 0)
				_ = s.Hits("coinbase")
				_ = s.Requests("coinbase")
				for _, u := range []string{krakenURL, coinbaseURL, yadioURL, customURL} {
					resp, err := c.Get(u)
					if err != nil {
						t.Error(err)
						return
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}()
	}
	wg.Wait()
	if s.Hits("coinbase") != 160 {
		t.Fatalf("hits %d, want 160", s.Hits("coinbase"))
	}
}

// TestSetRefusesNonDecimals uses a recorder in place of t, so the refusal shows without failing.
func TestSetRefusesNonDecimals(t *testing.T) {
	for _, bad := range []string{"", "1e3", "-1", "1.", ".5", "01", "1,5", " 1", "NaN"} {
		r := &recorder{TB: t}
		s := New(r)
		s.Set("yadio", "USD", bad)
		if !r.failed {
			t.Errorf("Set accepted %q", bad)
		}
	}
	r := &recorder{TB: t}
	s := New(r)
	s.Set("yadio", "USD", "0")
	s.Set("yadio", "USD", "0.5")
	s.Set("yadio", "USD", "76000.25")
	s.Set("nobody", "USD", "1")
	if !r.failed || r.count != 1 {
		t.Errorf("an unknown provider: failed %v after %d errors, want exactly one", r.failed, r.count)
	}
}

type recorder struct {
	testing.TB
	mu     sync.Mutex
	failed bool
	count  int
}

func (r *recorder) Errorf(string, ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = true
	r.count++
}

// TestAssetShapes: Bitfinex's tickers, Kraken's XMR pairs and the custom API's asset query serve the rates of the asset
// asked, and only those; Set is SetAsset for BTC.
func TestAssetShapes(t *testing.T) {
	s := New(t)
	c := &http.Client{Transport: s.Transport()}
	now := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	for _, p := range []string{"kraken", "bitfinex", "custom"} {
		s.Set(p, "USD", "76000.5")
		s.SetAsset(p, "XMR", "USD", "150.25")
		s.SetAsset(p, "XMR", "EUR", "130")
	}

	code, body := fetch(t, c, "https://api-pub.bitfinex.com/v2/tickers?symbols=tXMRUSD,tBTCUSD,tXMREUR,tBTCEUR,tXMRGBP")
	want := `[["tXMRUSD",150.25,1,150.25,1,0,0,150.25,1,150.25,150.25],["tBTCUSD",76000.5,1,76000.5,1,0,0,76000.5,1,76000.5,76000.5],` +
		`["tXMREUR",130,1,130,1,0,0,130,1,130,130]]`
	if code != 200 || body != want {
		t.Fatalf("bitfinex %d %s", code, body)
	}
	if _, body := fetch(t, c, "https://api-pub.bitfinex.com/v2/tickers?symbols="); body != `[]` {
		t.Fatalf("bitfinex with no symbol: %s", body)
	}
	if reqs := s.Requests("bitfinex"); len(reqs) != 2 || reqs[0] != "symbols=tXMRUSD,tBTCUSD,tXMREUR,tBTCEUR,tXMRGBP" {
		t.Fatalf("bitfinex requests %q", reqs)
	}

	code, body = fetch(t, c, "https://api.kraken.com/0/public/Ticker?assetVersion=1&pair=XMRUSD,XMREUR,XBTUSD,XMRGBP")
	want = `{"error":[],"result":{"BTC/USD":{"a":["76000.5","1","1.000"],"b":["76000.5","1","1.000"],"c":["76000.5","0.1"]},` +
		`"XMR/EUR":{"a":["130","1","1.000"],"b":["130","1","1.000"],"c":["130","0.1"]},` +
		`"XMR/USD":{"a":["150.25","1","1.000"],"b":["150.25","1","1.000"],"c":["150.25","0.1"]}}}`
	if code != 200 || body != want {
		t.Fatalf("kraken %d %s", code, body)
	}

	for u, want := range map[string]string{
		"https://rates.example.invalid/v1?asset=XMR":     `{"asset":"XMR","rates":{"EUR":"130","USD":"150.25"},"timestamp":1925089445}`,
		"https://rates.example.invalid/v1?k=1&asset=XMR": `{"asset":"XMR","rates":{"EUR":"130","USD":"150.25"},"timestamp":1925089445}`,
		"https://rates.example.invalid/v1?asset=BTC":     `{"asset":"BTC","rates":{"USD":"76000.5"},"timestamp":1925089445}`,
		"https://rates.example.invalid/v1":               `{"asset":"BTC","rates":{"USD":"76000.5"},"timestamp":1925089445}`,
		"https://rates.example.invalid/v1?asset=ETH":     `{"asset":"ETH","rates":{},"timestamp":1925089445}`,
	} {
		if code, body := fetch(t, c, u); code != 200 || body != want {
			t.Fatalf("custom %s: %d %s", u, code, body)
		}
	}
	// Coinbase and Yadio serve BTC only.
	s.SetAsset("coinbase", "XMR", "USD", "1")
	if _, body := fetch(t, c, coinbaseURL); body != `{"data":{"currency":"BTC","rates":{}}}` {
		t.Fatalf("coinbase %s", body)
	}

	s.VendorError("bitfinex", true)
	if code, body := fetch(t, c, "https://api-pub.bitfinex.com/v2/tickers?symbols=tXMRUSD"); code != 200 || body != `["error",10020,"NEVER-IN-A-LOG"]` {
		t.Fatalf("bitfinex vendor error: %d %s", code, body)
	}
	r := &recorder{TB: t}
	New(r).SetAsset("bitfinex", "XMR", "USD", "1e2")
	if !r.failed {
		t.Fatal("SetAsset accepted 1e2")
	}
}
