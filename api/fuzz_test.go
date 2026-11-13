package api

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/satbooth/satbooth/rates"
	"github.com/satbooth/satbooth/store"
)

// checkAccepted asserts the invariants of a body parseCreate accepted.
func checkAccepted(t testing.TB, b []byte, req createReq) {
	t.Helper()
	if !utf8.Valid(b) {
		t.Fatalf("accepted invalid UTF-8: %q", b)
	}
	if !validName(req.AccountID) || !validName(req.MerchantRef) {
		t.Fatalf("accepted bad names: %+v", req)
	}
	if req.Price != "" {
		// A priced request: a positive decimal, a valid currency, distinct known rails or none, and no amount.
		d, err := rates.ParseDecimal(req.Price)
		if err != nil || d.IsZero() || !rates.ValidCurrency(req.Currency) || req.AmountMsat != 0 || req.AmountSat != 0 || req.AmountPiconero != 0 {
			t.Fatalf("accepted a bad priced request: %+v", req)
		}
		if req.Rails != nil && (len(req.Rails) < 1 || len(req.Rails) > 3) {
			t.Fatalf("accepted bad rails: %+v", req)
		}
		seen := map[string]bool{}
		for _, r := range req.Rails {
			if r != railLightning && r != railOnchain && r != railMonero || seen[r] {
				t.Fatalf("accepted bad rails: %+v", req)
			}
			seen[r] = true
		}
	} else if req.Currency != "" || req.Rails != nil || req.AmountMsat == 0 && req.AmountSat == 0 && req.AmountPiconero == 0 {
		t.Fatalf("accepted an explicit request with pricing fields or no amount: %+v", req)
	}
	if req.AmountMsat != 0 && (req.AmountMsat < 1000 || req.AmountMsat > maxAmountMsat || req.AmountMsat%1000 != 0) {
		t.Fatalf("accepted bad amount: %+v", req)
	}
	if req.AmountSat != 0 && (req.AmountSat < 1 || req.AmountSat > store.MaxSat) {
		t.Fatalf("accepted bad on-chain amount: %+v", req)
	}
	if req.AmountPiconero != 0 && (req.AmountPiconero < 1 || req.AmountPiconero > store.MaxPiconero) {
		t.Fatalf("accepted bad Monero amount: %+v", req)
	}
	if req.ExpiresIn < MinExpiresIn || req.ExpiresIn > MaxExpiresIn {
		t.Fatalf("accepted bad expiry: %+v", req)
	}
	// The same document, read by encoding/json with any key matching, agrees on every value, and has
	// no key twice: the reading is unambiguous.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("accepted something encoding/json refuses: %q", b)
	}
	for k := range m {
		switch k {
		case "account_id", "merchant_ref", "lightning_msat", "onchain_sat", "monero_piconero", "expires_in", "price", "currency", "rails":
		default:
			t.Fatalf("accepted unknown key %q", k)
		}
	}
	var s struct {
		A string   `json:"account_id"`
		R string   `json:"merchant_ref"`
		M int64    `json:"lightning_msat"`
		S int64    `json:"onchain_sat"`
		X int64    `json:"monero_piconero"`
		P string   `json:"price"`
		C string   `json:"currency"`
		L []string `json:"rails"`
	}
	if err := json.Unmarshal(b, &s); err != nil || s.A != req.AccountID || s.R != req.MerchantRef || s.M != req.AmountMsat || s.S != req.AmountSat || s.X != req.AmountPiconero ||
		s.P != req.Price || s.C != req.Currency || !reflect.DeepEqual(s.L, req.Rails) {
		t.Fatalf("readings differ for %q: %+v vs %+v (%v)", b, s, req, err)
	}
}

func FuzzCreateOrder(f *testing.F) {
	for _, s := range []string{
		orderBody, `{}`, ``, `null`, `[]`, `{"account_id":"a","merchant_ref":"r","lightning_msat":1000,"expires_in":300}`,
		`{"account_id":"a","account_id":"b","merchant_ref":"r","lightning_msat":1000}`,
		`{"Account_ID":"a","merchant_ref":"r","lightning_msat":1000}`,
		`{"account_id":"a","merchant_ref":"r","lightning_msat":1e3}`,
		`{"account_id":"a","merchant_ref":"r","lightning_msat":9007199254740992}`,
		`{"account_id":"a","merchant_ref":"r","onchain_sat":1000}`,
		`{"account_id":"a","merchant_ref":"r","lightning_msat":1000,"onchain_sat":2100000000000000}`,
		`{"account_id":"a","merchant_ref":"r","onchain_sat":2100000000000001}`,
		`{"account_id":"a","merchant_ref":"r","onchain_sat":0}`,
		`{"account_id":"a","merchant_ref":"r","onchain_sat":null}`,
		`{"account_id":"a","merchant_ref":"r"}`,
		"{\"account_id\":\"a\xff\",\"merchant_ref\":\"r\",\"lightning_msat\":1000}",
		`{"account_id":"a\u0000","merchant_ref":"r","lightning_msat":1000}`,
		fiatBody,
		`{"account_id":"a","merchant_ref":"r","price":"10.00","currency":"EUR","rails":["onchain","lightning"]}`,
		`{"account_id":"a","merchant_ref":"r","price":"\u0031\u0030","currency":"\u0055SD"}`,
		`{"account_id":"a","merchant_ref":"r","price":"0.000000000000000001","currency":"USD","rails":["lightning"]}`,
		`{"account_id":"a","merchant_ref":"r","price":"99999999999999999999.999999999999999999","currency":"USD"}`,
		`{"account_id":"a","merchant_ref":"r","price":"1","currency":"USD","expires_in":300}`,
		`{"account_id":"a","merchant_ref":"r","monero_piconero":1500000000000}`,
		`{"account_id":"a","merchant_ref":"r","monero_piconero":1}`,
		`{"account_id":"a","merchant_ref":"r","monero_piconero":9007199254740991}`,
		`{"account_id":"a","merchant_ref":"r","monero_piconero":9007199254740992}`,
		`{"account_id":"a","merchant_ref":"r","monero_piconero":0}`,
		`{"account_id":"a","merchant_ref":"r","monero_piconero":1.5}`,
		`{"account_id":"a","merchant_ref":"r","monero_piconero":"1"}`,
		`{"account_id":"a","merchant_ref":"r","lightning_msat":1000,"onchain_sat":1,"monero_piconero":1}`,
		`{"account_id":"a","merchant_ref":"r","price":"10.00","currency":"USD","rails":["lightning","onchain","monero"]}`,
		`{"account_id":"a","merchant_ref":"r","price":"10.00","currency":"USD","rails":["monero"]}`,
		`{"account_id":"a","merchant_ref":"r","price":"10.00","currency":"USD","rails":["monero","lightning","monero"]}`,
		`{"account_id":"a","merchant_ref":"r","price":"10.00","currency":"USD","monero_piconero":1}`,
	} {
		f.Add([]byte(s))
	}
	// Every refusal body of the HTTP tests.
	for _, c := range fiatRefusals {
		f.Add([]byte(c.body))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		req, code := parseCreate(b)
		if code != "" {
			switch code {
			case codeInvalidJSON, codeInvalidAccountID, codeInvalidRef, codeInvalidAmount, codeInvalidSat, codeInvalidPiconero, codeMissingAmount,
				codeInvalidExpiresIn, codeInvalidPrice, codeInvalidCurrency, codeInvalidRails, codeMixedPricing:
			default:
				t.Fatalf("unknown code %q", code)
			}
			if !reflect.DeepEqual(req, createReq{}) {
				t.Fatalf("refused but returned %+v", req)
			}
			return
		}
		checkAccepted(t, b, req)
	})
}

// A seeded property test: valid documents mutated across every boundary (sizes, odd characters,
// duplicates, case variants, truncations). Whatever parseCreate accepts satisfies the invariants,
// and valid documents are accepted.
func TestParseCreateProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	names := []string{"a", "A-z_0.9", strings.Repeat("n", 64), strings.Repeat("n", 65), "", "a b", "é", "a\x00", "a/b", "..", "-"}
	amounts := []string{"1000", "999", "1001", "0", "-1000", "9007199254740000", "9007199254740992", "9007199254741000", "1e3", "1000.0", `"1000"`, "null", "01000", "1000000000000000000000"}
	sats := []string{"", "", `,"onchain_sat":1`, `,"onchain_sat":2100000000000000`, `,"onchain_sat":2100000000000001`, `,"onchain_sat":0`,
		`,"onchain_sat":-1`, `,"onchain_sat":1e3`, `,"onchain_sat":1.0`, `,"onchain_sat":"1"`, `,"onchain_sat":null`, `,"onchain_sat":01`,
		`,"onchain_sat":9223372036854775807`, `,"onchain_sat":9223372036854775808`}
	picos := []string{"", "", "", `,"monero_piconero":1`, `,"monero_piconero":9007199254740991`, `,"monero_piconero":9007199254740992`,
		`,"monero_piconero":0`, `,"monero_piconero":-1`, `,"monero_piconero":1e3`, `,"monero_piconero":1.0`, `,"monero_piconero":"1"`,
		`,"monero_piconero":null`, `,"monero_piconero":01`, `,"monero_piconero":9223372036854775807`, `,"monero_piconero":9223372036854775808`}
	expiries := []string{"", `,"expires_in":300`, `,"expires_in":299`, `,"expires_in":86400`, `,"expires_in":86401`, `,"expires_in":null`, `,"expires_in":1e3`}
	valid, pricedValid := 0, 0
	for i := 0; i < 20000; i++ {
		a, r, m, e := names[rng.Intn(len(names))], names[rng.Intn(len(names))], amounts[rng.Intn(len(amounts))], expiries[rng.Intn(len(expiries))]
		sat := sats[rng.Intn(len(sats))] + picos[rng.Intn(len(picos))]
		lightning := `,"lightning_msat":` + m
		if rng.Intn(4) == 0 {
			lightning = "" // on-chain only, or no amount at all
		}
		if rng.Intn(3) == 0 { // a priced body, with or without the explicit members
			lightning = pricedMembers(rng)
			if rng.Intn(4) != 0 {
				sat = ""
			}
		}
		doc := fmt.Sprintf(`{"account_id":%s,"merchant_ref":%s%s%s%s}`, quote(a), quote(r), lightning, sat, e)
		b := []byte(doc)
		switch rng.Intn(6) {
		case 0: // truncate
			b = b[:rng.Intn(len(b)+1)]
		case 1: // duplicate a key
			b = []byte(strings.Replace(doc, `{`, `{"account_id":"x",`, 1))
		case 2: // case variant
			b = []byte(strings.Replace(doc, "merchant_ref", "Merchant_Ref", 1))
		case 3: // flip one byte
			if len(b) > 0 {
				b[rng.Intn(len(b))] = byte(rng.Intn(256))
			}
		case 4: // append
			b = append(b, []byte(" {}")...)
		}
		req, code := parseCreate(b)
		if code == "" {
			checkAccepted(t, b, req)
			valid++
			if req.Price != "" {
				pricedValid++
			}
		}
	}
	if valid == 0 {
		t.Fatal("the generator never produced an accepted document")
	}
	if _, code := parseCreate([]byte(orderBody)); code != "" {
		t.Fatalf("valid body refused: %s", code)
	}
	if req, code := parseCreate([]byte(`{"account_id":"a","merchant_ref":"r","onchain_sat":5}`)); code != "" || req.AmountSat != 5 || req.AmountMsat != 0 {
		t.Fatalf("on-chain only body: %+v %s", req, code)
	}
	if req, code := parseCreate([]byte(`{"account_id":"a","merchant_ref":"r","monero_piconero":5}`)); code != "" || req.AmountPiconero != 5 ||
		req.AmountMsat != 0 || req.AmountSat != 0 {
		t.Fatalf("Monero only body: %+v %s", req, code)
	}
	if pricedValid == 0 {
		t.Fatal("the generator never produced an accepted priced document")
	}
}

// The members of a priced body, drawn across every boundary of each rule.
func pricedMembers(rng *rand.Rand) string {
	prices := []string{`"10.00"`, `"1"`, `"0.000000000000000001"`, `"99999999999999999999"`, `"99999999999999999999.999999999999999999"`,
		`"100000000000000000000"`, `"1.0000000000000000001"`, `"0"`, `"0.0"`, `"00.10"`, `""`, `"1e3"`, `"-1"`, `"+1"`, `" 1"`, `"1 "`, `".5"`, `"5."`,
		`"1..2"`, `"1,5"`, `"\u0031"`, `"١"`, `10`, `10.5`, `null`, `true`, `["1"]`, `{"v":"1"}`}
	currencies := []string{`"USD"`, `"EUR"`, `"GBP"`, `"usd"`, `"USDT"`, `"US"`, `"BTC"`, `"U\u0053D"`, `"ÜSD"`, `""`, `null`, `840`, `["USD"]`}
	rails := []string{`["lightning"]`, `["onchain"]`, `["lightning","onchain"]`, `["onchain","lightning"]`, `[]`, `["lightning","lightning"]`,
		`["monero"]`, `["Lightning"]`, `["lightning","onchain","lightning"]`, `[""]`, `[1]`, `[null]`, `"lightning"`, `null`, `{}`, `[["lightning"]]`,
		`["lightning","onchain","monero"]`, `["monero","lightning"]`, `["monero","lightning","monero"]`, `["onchain","monero","onchain"]`,
		`["lightning","onchain","monero","lightning"]`, `["Monero"]`, `["monero","monero"]`}
	var b strings.Builder
	if rng.Intn(8) != 0 {
		b.WriteString(`,"price":` + prices[rng.Intn(len(prices))])
	}
	if rng.Intn(8) != 0 {
		b.WriteString(`,"currency":` + currencies[rng.Intn(len(currencies))])
	}
	if rng.Intn(2) == 0 {
		b.WriteString(`,"rails":` + rails[rng.Intn(len(rails))])
	}
	if rng.Intn(6) == 0 {
		b.WriteString(`,"lightning_msat":1000`)
	}
	if rng.Intn(8) == 0 {
		b.WriteString(`,"monero_piconero":1000`)
	}
	return b.String()
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

// The key file parser: random mutations never panic, and anything accepted has unique ids, hashes of
// the right shape and a known capability.
func TestParseKeysProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	base := keysBody(keyEntry("a", CapWrite, "k1"), keyEntry("b", CapRead, "k2"))
	for i := 0; i < 20000; i++ {
		b := []byte(base)
		for n := rng.Intn(4); n >= 0; n-- {
			switch rng.Intn(3) {
			case 0:
				b[rng.Intn(len(b))] = byte(rng.Intn(256))
			case 1:
				b = b[:rng.Intn(len(b)+1)]
			case 2:
				b = append(b, byte(rng.Intn(256)))
			}
			if len(b) == 0 {
				b = []byte(base)
			}
		}
		es, err := parseKeys(b)
		if err != nil {
			continue
		}
		if len(es) < 1 || len(es) > maxKeys {
			t.Fatalf("accepted %d keys", len(es))
		}
		seen := map[[32]byte]bool{}
		for _, e := range es {
			if e.bits != bitWrite && e.bits != bitRead || seen[e.hash] {
				t.Fatalf("bad entry accepted from %q", b)
			}
			seen[e.hash] = true
		}
	}
}
