package rates

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// The helpers of the live tests (live_test.go, build tag live) that need no network, with their offline tests.

// liveFailure reports whether a provider's fetch error fails the live smoke test: an answer the parsers refuse
// (ErrShape, ErrVendor, ErrTooLarge: the vendor's format changed) or an HTTP 4xx other than 429 (SatBooth's request is
// wrong). A transport failure, a 429 or a 5xx is a provider down or limiting, which the smoke test only logs.
func liveFailure(err error) bool {
	var se *StatusError
	switch {
	case errors.Is(err, ErrShape), errors.Is(err, ErrVendor), errors.Is(err, ErrTooLarge):
		return true
	case errors.As(err, &se):
		return se.Code >= 400 && se.Code < 500 && se.Code != 429
	}
	return false
}

// errTrim is a recorded body the trims cannot read: TestLiveRecord then writes nothing.
var errTrim = errors.New("the recorded body is not the documented shape")

// members checks b with CheckJSON and returns the members of its top-level object, each value verbatim.
func members(b []byte) (map[string]json.RawMessage, error) {
	if !CheckJSON(b, maxDepth) {
		return nil, errTrim
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil || m == nil {
		return nil, errTrim
	}
	return m, nil
}

// trimKraken keeps a recorded Kraken Ticker answer to what the parser reads for asset in codes: the error array and the
// pairs krakenKey(asset, code), each pair's value byte for byte (every other key goes). json.Marshal writes the keys in
// order.
func trimKraken(body []byte, asset string, codes []string) ([]byte, error) {
	top, err := members(body)
	if err != nil {
		return nil, err
	}
	result, err := members(top["result"])
	if err != nil || top["error"] == nil {
		return nil, errTrim
	}
	kept := map[string]json.RawMessage{}
	for _, c := range codes {
		if v, ok := result[krakenKey(asset, c)]; ok {
			kept[krakenKey(asset, c)] = v
		}
	}
	r, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{"error": top["error"], "result": r})
}

// trimCoinbase keeps a recorded Coinbase answer to the rates of codes, plus the code with the longest fraction of the
// whole answer (the alphabetically first among equals) when none of codes has a fraction that long: the fixture keeps
// the evidence that MaxFracDigits is enough for what the vendor sends. Every kept rate is byte for byte; the data
// object keeps its currency.
func trimCoinbase(body []byte, codes []string) ([]byte, error) {
	top, err := members(body)
	if err != nil {
		return nil, err
	}
	data, err := members(top["data"])
	if err != nil || data["currency"] == nil {
		return nil, errTrim
	}
	if _, err := members(data["rates"]); err != nil {
		return nil, err
	}
	var rates map[string]string // every rate is a JSON string: a number or a null is refused
	if json.Unmarshal(data["rates"], &rates) != nil {
		return nil, errTrim
	}
	for _, v := range rates {
		if v == "" {
			return nil, errTrim
		}
	}
	frac := func(s string) int {
		if i := strings.IndexByte(s, '.'); i >= 0 {
			return len(s) - i - 1
		}
		return 0
	}
	kept := map[string]string{}
	keptLongest := -1
	for _, c := range codes {
		if v, ok := rates[c]; ok {
			kept[c] = v
			keptLongest = max(keptLongest, frac(v))
		}
	}
	longest, longestCode := -1, ""
	for c, v := range rates {
		if f := frac(v); f > longest || f == longest && c < longestCode {
			longest, longestCode = f, c
		}
	}
	if longest > keptLongest {
		kept[longestCode] = rates[longestCode]
	}
	r, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	d, err := json.Marshal(map[string]json.RawMessage{"currency": data["currency"], "rates": r})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{"data": d})
}

// TestLiveFailureClasses: the live smoke test fails on an answer the parsers refuse and on a 4xx other than 429 (the
// request itself is wrong: a moved endpoint or a changed query), never on a provider that is down or limiting.
func TestLiveFailureClasses(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{ErrShape, true}, {ErrVendor, true}, {ErrTooLarge, true},
		{&StatusError{Code: 400}, true}, {&StatusError{Code: 401}, true}, {&StatusError{Code: 403}, true},
		{&StatusError{Code: 404}, true}, {&StatusError{Code: 410}, true}, {&StatusError{Code: 499}, true},
		{fmt.Errorf("wrapped: %w", &StatusError{Code: 404}), true},
		{&StatusError{Code: 429}, false}, {&StatusError{Code: 500}, false}, {&StatusError{Code: 503}, false},
		{&StatusError{Code: 399}, false}, {&StatusError{Code: 204}, false},
		{ErrTransport, false}, {ErrStatus, false}, {errors.New("context deadline exceeded"), false}, {nil, false},
	} {
		if got := liveFailure(c.err); got != c.want {
			t.Errorf("liveFailure(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestTrimKraken: the trim keeps the error array and exactly the requested BTC/<code> pairs, each byte for byte; the
// parser reads the same rates before and after; trimming twice changes nothing; a body CheckJSON or the shape refuses
// is refused, never half-trimmed.
func TestTrimKraken(t *testing.T) {
	pair := func(ask, bid string) string {
		return `{"a":["` + ask + `","1","1.000"],"b":["` + bid + `","1","1.000"],"c":["1","0.1"],"o":"7"}`
	}
	body := []byte(`{"error":[],"result":{"BTC/COP":` + pair("9", "8") + `,"BTC/USD":` + pair("76001", "75999") +
		`,"BTC/EUR":` + pair("67001.5", "66999.5") + `,"XXBTZUSD":` + pair("1", "1") + `}}`)
	got, err := trimKraken(body, AssetBTC, []string{"USD", "EUR"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"error":[],"result":{"BTC/EUR":` + pair("67001.5", "66999.5") + `,"BTC/USD":` + pair("76001", "75999") + `}}`
	if string(got) != want {
		t.Fatalf("trimmed\n%s\nwant\n%s", got, want)
	}
	before, err1 := parseKraken(body, AssetBTC, []string{"USD", "EUR"}, at2030)
	after, err2 := parseKraken(got, AssetBTC, []string{"USD", "EUR"}, at2030)
	if err1 != nil || err2 != nil || len(after) != 2 || !sameRates(before, after) {
		t.Fatalf("parsed before %+v (%v), after %+v (%v)", before, err1, after, err2)
	}
	if again, err := trimKraken(got, AssetBTC, []string{"USD", "EUR"}); err != nil || string(again) != string(got) {
		t.Fatalf("trimming twice: %s, %v", again, err)
	}
	for _, bad := range []string{
		``, `[]`, `{"error":[]}`, `{"error":[],"result":[]}`, `{"error":[],"result":{"BTC/USD":1,"BTC/USD":2}}`,
		`{"error":[],"result":{}}{}`, "{\"error\":[],\"result\":{\"BTC/USD\":\"\xff\"}}",
	} {
		if _, err := trimKraken([]byte(bad), AssetBTC, krakenSeven); err == nil {
			t.Errorf("%q: trimmed, want an error", bad)
		}
	}
}

// TestTrimCoinbase: the trim keeps the requested codes and the code with the longest fraction of the whole answer
// (the alphabetically first among equals) when none of the requested codes has a fraction that long, so the fixture
// still shows how many fraction digits the vendor sends; every kept rate is byte for byte.
func TestTrimCoinbase(t *testing.T) {
	body := []byte(`{"data":{"currency":"BTC","rates":{"ZZZ":"1.12345678901234567","AAA":"2.12345678901234567",` +
		`"USD":"76328.31","EUR":"68031.812","COP":"1","00":"3.1"}}}`)
	got, err := trimCoinbase(body, []string{"USD", "EUR"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"data":{"currency":"BTC","rates":{"AAA":"2.12345678901234567","EUR":"68031.812","USD":"76328.31"}}}`; string(got) != want {
		t.Fatalf("trimmed\n%s\nwant\n%s", got, want)
	}
	// A requested code as long as the longest: nothing is added.
	tie := []byte(`{"data":{"currency":"BTC","rates":{"AAA":"2.123","USD":"76328.312","COP":"1"}}}`)
	if got, err := trimCoinbase(tie, []string{"USD"}); err != nil || string(got) != `{"data":{"currency":"BTC","rates":{"USD":"76328.312"}}}` {
		t.Fatalf("a tie: %s, %v", got, err)
	}
	before, err1 := parseCoinbase(body, []string{"USD", "EUR"}, at2030)
	trimmed, _ := trimCoinbase(body, []string{"USD", "EUR"})
	after, err2 := parseCoinbase(trimmed, []string{"USD", "EUR"}, at2030)
	if err1 != nil || err2 != nil || len(after) != 2 || !sameRates(before, after) {
		t.Fatalf("parsed before %+v (%v), after %+v (%v)", before, err1, after, err2)
	}
	if again, err := trimCoinbase(trimmed, []string{"USD", "EUR"}); err != nil || string(again) != string(trimmed) {
		t.Fatalf("trimming twice: %s, %v", again, err)
	}
	for _, bad := range []string{
		``, `[]`, `{"data":[]}`, `{"data":{"currency":"BTC"}}`, `{"data":{"currency":"BTC","rates":{"USD":1}}}`,
		`{"data":{"currency":"BTC","rates":{"USD":"1","USD":"2"}}}`, `{"data":{"currency":"BTC","rates":[]}}`,
		"{\"data\":{\"currency\":\"BTC\",\"rates\":{\"USD\":\"\xff\"}}}",
	} {
		if _, err := trimCoinbase([]byte(bad), krakenSeven); err == nil {
			t.Errorf("%q: trimmed, want an error", bad)
		}
	}
}

// The committed fixtures are already trimmed: TestLiveRecord writes exactly what the trims give.
func TestFixturesAreTrimmed(t *testing.T) {
	for name, trim := range map[string]func([]byte) ([]byte, error){
		"kraken.json":     func(b []byte) ([]byte, error) { return trimKraken(b, AssetBTC, KrakenFiats()) },
		"kraken-xmr.json": func(b []byte) ([]byte, error) { return trimKraken(b, AssetXMR, krakenPairs[AssetXMR]) },
		"coinbase.json":   func(b []byte) ([]byte, error) { return trimCoinbase(b, KrakenFiats()) },
	} {
		body := fixture(t, name)
		got, err := trim(body)
		if err != nil || string(got) != string(body) {
			t.Errorf("%s is not trimmed (%v): run TestLiveRecord, or trim it", name, err)
		}
	}
}

// TestTrimProperties: over seeded random answers, each trim keeps a subset of the rates, every requested code present
// in the answer byte for byte, and the parser's rates for the requested codes; trimming twice changes nothing.
func TestTrimProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(20261106))
	codes := []string{"AUD", "CAD", "CHF", "EUR", "GBP", "JPY", "USD", "COP", "ARS", "00", "1INCH", "ZRX"}
	digits := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('0' + rng.Intn(10))
		}
		return string(b)
	}
	number := func() string {
		s := string(byte('1'+rng.Intn(9))) + digits(rng.Intn(12))
		if rng.Intn(3) > 0 {
			s += "." + digits(rng.Intn(MaxFracDigits+1))
		}
		return s
	}
	for i := 0; i < 2000; i++ {
		var kr, cb []string
		present := map[string]string{}
		for _, c := range codes {
			if rng.Intn(4) == 0 {
				continue
			}
			v := number()
			present[c] = v
			kr = append(kr, fmt.Sprintf(`"BTC/%s":{"a":[%q],"b":[%q]}`, c, v, v))
			cb = append(cb, fmt.Sprintf(`%q:%q`, c, v))
		}
		rng.Shuffle(len(kr), func(a, b int) { kr[a], kr[b] = kr[b], kr[a] })
		rng.Shuffle(len(cb), func(a, b int) { cb[a], cb[b] = cb[b], cb[a] })
		want := krakenSeven[:1+rng.Intn(len(krakenSeven))]
		for _, c := range []struct {
			name  string
			body  string
			trim  func([]byte, []string) ([]byte, error)
			parse func([]byte, []string, time.Time) ([]Rate, error)
		}{
			{"kraken", `{"error":[],"result":{` + strings.Join(kr, ",") + `}}`,
				func(b []byte, c []string) ([]byte, error) { return trimKraken(b, AssetBTC, c) },
				func(b []byte, c []string, at time.Time) ([]Rate, error) { return parseKraken(b, AssetBTC, c, at) }},
			{"coinbase", `{"data":{"currency":"BTC","rates":{` + strings.Join(cb, ",") + `}}}`, trimCoinbase, parseCoinbase},
		} {
			got, err := c.trim([]byte(c.body), want)
			if err != nil {
				t.Fatalf("%s %d: %v", c.name, i, err)
			}
			if again, err := c.trim(got, want); err != nil || string(again) != string(got) {
				t.Fatalf("%s %d: trimming twice changed %s", c.name, i, got)
			}
			for _, code := range want {
				if v, ok := present[code]; ok && !strings.Contains(string(got), `"`+v+`"`) {
					t.Fatalf("%s %d: %s %s lost: %s", c.name, i, code, v, got)
				}
			}
			before, err1 := c.parse([]byte(c.body), want, at2030)
			after, err2 := c.parse(got, want, at2030)
			if err1 != nil || err2 != nil || !sameRates(before, after) {
				t.Fatalf("%s %d: parsed %+v (%v) then %+v (%v)", c.name, i, before, err1, after, err2)
			}
			if len(got) > len(c.body) {
				t.Fatalf("%s %d: the trim grew the body", c.name, i)
			}
		}
	}
}
