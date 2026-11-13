// Package ratestest is a fake rate server for tests: one in-memory httptest.Server that answers in the
// shapes of Kraken, Coinbase, Yadio, Bitfinex and SatBooth's custom rate API, and a round tripper that
// re-routes the providers' fixed hosts to it. Every method is safe for concurrent use.
package ratestest

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Sentinel is in every body Fail and VendorError serve, so a test that finds it absent from a log
// proves that no provider's body reached the log.
const Sentinel = "NEVER-IN-A-LOG"

// customHost is the host of the custom rate API in every test.
const customHost = "rates.example.invalid"

// hosts maps each re-routed host to the provider's path segment on the fake.
var hosts = map[string]string{
	"api.kraken.com":       "kraken",
	"api.coinbase.com":     "coinbase",
	"api.yadio.io":         "yadio",
	"api-pub.bitfinex.com": "bitfinex",
	customHost:             "custom",
}

var providers = []string{"kraken", "coinbase", "yadio", "bitfinex", "custom"}

type state struct {
	rates    map[string]map[string]string // asset → currency → decimal text
	at       time.Time                    // zero: serve the clock's time
	fail     int
	vendor   bool
	delay    time.Duration
	body     []byte
	requests []string
}

// Server is the fake. Its zero state serves every provider's shape with no rates.
type Server struct {
	t    testing.TB
	srv  *httptest.Server
	done chan struct{}

	mu    sync.Mutex
	now   func() time.Time
	state map[string]*state
}

// New starts the fake; it closes when the test ends.
func New(t testing.TB) *Server {
	s := &Server{t: t, done: make(chan struct{}), now: time.Now}
	s.reset()
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		close(s.done) // ends any Delay still waiting, so Close does not block on it
		s.srv.Close()
	})
	return s
}

func (s *Server) reset() {
	s.state = make(map[string]*state, len(providers))
	for _, p := range providers {
		s.state[p] = &state{rates: map[string]map[string]string{}}
	}
}

// get returns a provider's state under s.mu, or nil (and an error on the test) for an unknown name.
func (s *Server) get(provider string) *state {
	st, ok := s.state[provider]
	if !ok {
		s.t.Errorf("ratestest: unknown provider %q", provider)
	}
	return st
}

// SetClock sets the clock whose time Yadio (ms) and the custom shape (s) serve as their timestamp,
// unless SetAt pinned the provider. The default is time.Now; Reset keeps the clock.
func (s *Server) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Set gives a provider a BTC rate for a currency: SetAsset(provider, "BTC", currency, rate).
func (s *Server) Set(provider, currency, rate string) { s.SetAsset(provider, "BTC", currency, rate) }

// SetAsset gives a provider a rate of asset for a currency, as unsigned decimal text in JSON's number grammar
// (Kraken and Bitfinex serve it as both bid and ask, Yadio and Bitfinex as a JSON number, the others as a
// string). Coinbase and Yadio serve BTC rates only; Kraken serves the asset its pair names (XBT is BTC),
// Bitfinex the asset its symbol names, and the custom shape the asset of the request's asset query.
func (s *Server) SetAsset(provider, asset, currency, rate string) {
	if !validNumber(rate) {
		s.t.Errorf("ratestest: SetAsset(%q, %q, %q): %q is not an unsigned decimal", provider, asset, currency, rate)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.get(provider); st != nil {
		if st.rates[asset] == nil {
			st.rates[asset] = map[string]string{}
		}
		st.rates[asset][currency] = rate
	}
}

// SetAt pins the timestamp Yadio (in ms) and the custom shape (in s) serve; Kraken, Coinbase and Bitfinex
// have none, so it changes nothing for them. The zero time unpins it.
func (s *Server) SetAt(provider string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.get(provider); st != nil {
		st.at = at
	}
}

// Fail makes a provider answer status with the body {"error":"NEVER-IN-A-LOG"}; 0 clears it. It wins
// over Body and VendorError.
func (s *Server) Fail(provider string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.get(provider); st != nil {
		st.fail = status
	}
}

// VendorError makes Kraken answer 200 with a non-empty error array, Yadio 200 with an error key and
// Bitfinex 200 with its ["error", code, message] array. Coinbase and the custom API have no such answer:
// for them it changes nothing.
func (s *Server) VendorError(provider string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.get(provider); st != nil {
		st.vendor = on
	}
}

// Delay holds a provider's answers for d, or until the request's context ends (a client timeout).
func (s *Server) Delay(provider string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.get(provider); st != nil {
		st.delay = d
	}
}

// Body makes a provider answer 200 with exactly raw; nil clears it.
func (s *Server) Body(provider string, raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.get(provider); st != nil {
		st.body = nil
		if raw != nil {
			st.body = append([]byte{}, raw...)
		}
	}
}

// Hits is the number of requests a provider has had.
func (s *Server) Hits(provider string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.get(provider); st != nil {
		return len(st.requests)
	}
	return 0
}

// Requests returns the raw query of each of a provider's requests, in order.
func (s *Server) Requests(provider string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.get(provider); st != nil {
		return append([]string(nil), st.requests...)
	}
	return nil
}

// Reset clears every provider's rates, timestamps, failures, delays, bodies and requests. The clock
// stays.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reset()
}

// route names the provider a path on the fake belongs to, or "".
func route(path string) string {
	switch {
	case path == "/kraken/0/public/Ticker":
		return "kraken"
	case path == "/coinbase/v2/exchange-rates":
		return "coinbase"
	case path == "/yadio/exrates/BTC":
		return "yadio"
	case path == "/bitfinex/v2/tickers":
		return "bitfinex"
	case path == "/custom" || strings.HasPrefix(path, "/custom/"):
		return "custom"
	}
	return ""
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	provider := route(r.URL.Path)
	if provider == "" {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	st := s.state[provider]
	st.requests = append(st.requests, r.URL.RawQuery)
	delay, fail, vendor := st.delay, st.fail, st.vendor
	body, hasBody := st.body, st.body != nil // never written in place: Body replaces the slice
	rates := make(map[string]map[string]string, len(st.rates))
	for a, m := range st.rates {
		rates[a] = make(map[string]string, len(m))
		for k, v := range m {
			rates[a][k] = v
		}
	}
	at := st.at
	if at.IsZero() {
		at = s.now()
	}
	s.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case fail != 0:
		w.WriteHeader(fail)
		w.Write([]byte(`{"error":"` + Sentinel + `"}`))
		return
	case hasBody:
		w.Write(body)
		return
	case vendor && provider == "kraken":
		w.Write([]byte(`{"error":["EGeneral:` + Sentinel + `"]}`))
		return
	case vendor && provider == "yadio":
		w.Write([]byte(`{"error":"` + Sentinel + `"}`))
		return
	case vendor && provider == "bitfinex":
		w.Write([]byte(`["error",10020,"` + Sentinel + `"]`))
		return
	}
	btc := rates["BTC"]
	if btc == nil {
		btc = map[string]string{}
	}
	var doc any
	switch provider {
	case "kraken":
		doc = krakenDoc(rates, r.URL.Query().Get("pair"))
	case "bitfinex":
		doc = bitfinexDoc(rates, r.URL.Query().Get("symbols"))
	case "coinbase":
		type data struct {
			Currency string            `json:"currency"`
			Rates    map[string]string `json:"rates"`
		}
		doc = struct {
			Data data `json:"data"`
		}{data{"BTC", btc}}
	case "yadio":
		nums := make(map[string]json.Number, len(btc))
		for k, v := range btc {
			nums[k] = json.Number(v)
		}
		doc = struct {
			BTC       map[string]json.Number `json:"BTC"`
			Base      string                 `json:"base"`
			Timestamp int64                  `json:"timestamp"`
		}{nums, "BTC", at.UnixMilli()}
	case "custom":
		asset := "BTC"
		if q := r.URL.Query(); q.Has("asset") {
			asset = q.Get("asset")
		}
		ar := rates[asset]
		if ar == nil {
			ar = map[string]string{}
		}
		doc = struct {
			Asset     string            `json:"asset"`
			Rates     map[string]string `json:"rates"`
			Timestamp int64             `json:"timestamp"`
		}{asset, ar, at.Unix()}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		s.t.Errorf("ratestest: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write(b)
}

type krakenTicker struct {
	A []string `json:"a"`
	B []string `json:"b"`
	C []string `json:"c"`
}

// krakenAssets maps Kraken's name of an asset in a pair to SatBooth's.
var krakenAssets = map[string]string{"XBT": "BTC", "XMR": "XMR"}

// krakenDoc is Kraken's Ticker answer with assetVersion=1 (keys BTC/<code> and XMR/<code>) for the pairs
// asked (XBT<code> or XMR<code>, comma-separated; every BTC rate set when pair is empty) that have a rate.
func krakenDoc(rates map[string]map[string]string, pair string) any {
	result := map[string]krakenTicker{}
	var pairs [][2]string // asset, code
	if pair == "" {
		for code := range rates["BTC"] {
			pairs = append(pairs, [2]string{"BTC", code})
		}
		sort.Slice(pairs, func(i, j int) bool { return pairs[i][1] < pairs[j][1] })
	} else {
		for _, p := range strings.Split(pair, ",") {
			if len(p) > 3 {
				if asset, ok := krakenAssets[p[:3]]; ok {
					pairs = append(pairs, [2]string{asset, p[3:]})
				}
			}
		}
	}
	for _, ac := range pairs {
		if v, ok := rates[ac[0]][ac[1]]; ok {
			result[ac[0]+"/"+ac[1]] = krakenTicker{A: []string{v, "1", "1.000"}, B: []string{v, "1", "1.000"}, C: []string{v, "0.1"}}
		}
	}
	return struct {
		Error  []string                `json:"error"`
		Result map[string]krakenTicker `json:"result"`
	}{[]string{}, result}
}

// bitfinexDoc is Bitfinex's tickers answer for the symbols asked (t<asset><code>, comma-separated) that have a
// rate, in their order: each an 11-element trading ticker whose bid, ask, last, high and low are the rate, as
// JSON numbers.
func bitfinexDoc(rates map[string]map[string]string, symbols string) any {
	out := []any{}
	for _, sym := range strings.Split(symbols, ",") {
		if len(sym) != 7 || sym[0] != 't' {
			continue
		}
		v, ok := rates[sym[1:4]][sym[4:]]
		if !ok {
			continue
		}
		n, one, zero := json.Number(v), json.Number("1"), json.Number("0")
		out = append(out, []any{sym, n, one, n, one, zero, zero, n, one, n, n})
	}
	return out
}

// validNumber reports whether s is an unsigned decimal in JSON's number grammar: no sign, no exponent,
// no leading zero before another digit, and digits on both sides of a point.
func validNumber(s string) bool {
	intPart, frac, hasPoint := strings.Cut(s, ".")
	if intPart == "" || (len(intPart) > 1 && intPart[0] == '0') || (hasPoint && frac == "") {
		return false
	}
	for _, part := range []string{intPart, frac} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return false
			}
		}
	}
	return true
}

// Transport returns a round tripper that sends requests for api.kraken.com, api.coinbase.com,
// api.yadio.io, api-pub.bitfinex.com and rates.example.invalid to the fake (over plain HTTP, the path prefixed with the
// provider's segment, the query kept) and refuses every other host as a connection refused.
func (s *Server) Transport() http.RoundTripper {
	return reroute{s: s, base: s.srv.Client().Transport}
}

type reroute struct {
	s    *Server
	base http.RoundTripper
}

func (rr reroute) RoundTrip(req *http.Request) (*http.Response, error) {
	seg, ok := hosts[req.URL.Hostname()]
	if !ok {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused (ratestest: host not served)")}
	}
	out := req.Clone(req.Context())
	u := *req.URL
	u.Scheme = "http"
	u.Host = rr.s.srv.Listener.Addr().String()
	u.Path = "/" + seg + req.URL.Path
	if req.URL.RawPath != "" {
		u.RawPath = "/" + seg + req.URL.RawPath
	}
	out.URL = &u
	out.Host = ""
	return rr.base.RoundTrip(out)
}
