package checkout

import (
	"errors"
	"math/rand"
	"net/url"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/store"
)

// testMain and testRegtest are the P2WPKH address of BIP-173's example under the bc and bcrt prefixes; testInvoice is
// an invoice prefix in the shape of BOLT 11's example invoices (timestamp, payment hash and description, no payment
// secret), cut before the signature.
const (
	testRegtest = "bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080"
	testMain    = "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"
	testInvoice = "lnbc1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaq"
)

func TestPaymentURIExact(t *testing.T) {
	got, err := PaymentURI(testRegtest, 1, "")
	want := "bitcoin:" + strings.ToUpper(testRegtest) + "?amount=0.00000001"
	if err != nil || got != want {
		t.Errorf("on-chain only: got %q, %v; want %q", got, err, want)
	}
	got, err = PaymentURI(testMain, 150000000, testInvoice)
	want = "bitcoin:" + strings.ToUpper(testMain) + "?amount=1.5&lightning=" + strings.ToUpper(testInvoice)
	if err != nil || got != want {
		t.Errorf("unified: got %q, %v; want %q", got, err, want)
	}
}

func TestPaymentURIRefusals(t *testing.T) {
	long := "ln" + strings.Repeat("a", 4095)
	cases := []struct {
		name    string
		addr    string
		sat     int64
		bolt11  string
		wantErr bool
	}{
		{"ok", testMain, 1, "", false},
		{"max amount", testMain, store.MaxSat, "", false},
		{"bad address empty", "", 1, "", true},
		{"bad address upper", strings.ToUpper(testMain), 1, "", true},
		{"bad address prefix", "xx1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", 1, "", true},
		{"amount zero", testMain, 0, "", true},
		{"amount negative", testMain, -1, "", true},
		{"amount over max", testMain, store.MaxSat + 1, "", true},
		{"invoice 4096", testMain, 1, "ln" + strings.Repeat("a", 4094), false},
		{"invoice 4097", testMain, 1, long, true},
		{"invoice no ln prefix", testMain, 1, "abc123", true},
		{"invoice one char", testMain, 1, "l", true},
		{"invoice LN upper ok", testMain, 1, "LNBC1x", false},
		{"invoice Ln mixed ok", testMain, 1, "Lnbc1x", false},
		{"invoice ampersand", testMain, 1, "lnbc1&x=y", true},
		{"invoice space", testMain, 1, "lnbc1 x", true},
		{"invoice percent", testMain, 1, "lnbc1%41", true},
		{"invoice hash", testMain, 1, "lnbc1#x", true},
		{"invoice newline", testMain, 1, "lnbc1\nx", true},
		{"invoice non-ascii", testMain, 1, "lnbc1é", true},
		{"invoice NUL", testMain, 1, "lnbc1\x00", true},
		{"invoice quote", testMain, 1, "lnbc1\"x", true},
	}
	for _, c := range cases {
		got, err := PaymentURI(c.addr, c.sat, c.bolt11)
		if c.wantErr {
			if !errors.Is(err, ErrURI) || got != "" {
				t.Errorf("%s: got %q, %v; want ErrURI", c.name, got, err)
			}
		} else if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
	}
	if _, err := PaymentURI(testMain, 1, "lnbc1&secret"); err != nil && strings.Contains(err.Error(), "secret") {
		t.Error("error text echoes input")
	}
}

func randAlnum(rng *rand.Rand, alphabet string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(b)
}

func TestPaymentURIProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	const lower = "abcdefghijklmnopqrstuvwxyz0123456789"
	const mixed = lower + "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	prefixes := []string{"bc1", "tb1", "bcrt1"}
	for i := 0; i < 2000; i++ {
		p := prefixes[rng.Intn(3)]
		addr := p + randAlnum(rng, lower, 14-len(p)+rng.Intn(90-14+1))
		if len(addr) > 90 {
			addr = addr[:90]
		}
		var sat int64
		switch rng.Intn(4) {
		case 0:
			sat = 1 + rng.Int63n(10)
		case 1:
			sat = store.MaxSat - rng.Int63n(10)
		default:
			sat = 1 + rng.Int63n(store.MaxSat)
		}
		bolt := ""
		if rng.Intn(2) == 0 {
			bolt = []string{"ln", "LN", "lN", "Ln"}[rng.Intn(4)] + randAlnum(rng, mixed, rng.Intn(4095))
		}
		s, err := PaymentURI(addr, sat, bolt)
		if err != nil {
			t.Fatalf("valid input refused: %q %d: %v", addr, sat, err)
		}
		u, err := url.Parse(s)
		if err != nil {
			t.Fatalf("net/url: %v", err)
		}
		if u.Scheme != "bitcoin" || u.Opaque != strings.ToUpper(addr) {
			t.Fatalf("scheme %q opaque %q", u.Scheme, u.Opaque)
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			t.Fatal(err)
		}
		wantKeys := 1
		if bolt != "" {
			wantKeys = 2
			if q.Get("lightning") != strings.ToUpper(bolt) {
				t.Fatalf("lightning mismatch")
			}
		}
		assertHrefSafe(t, s)
		if len(q) != wantKeys || len(q["amount"]) != 1 {
			t.Fatalf("keys %v", q)
		}
		got, err := btcrpc.ParseBTC(q.Get("amount"))
		if err != nil || got != sat {
			t.Fatalf("amount %q -> %d, %v; want %d", q.Get("amount"), got, err, sat)
		}
	}
}

// refValidInvoice is a second, slower statement of the invoice rule.
func refValidInvoice(s string) bool {
	if len(s) < 1 || len(s) > 4096 || !strings.HasPrefix(strings.ToLower(s), "ln") {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !ok {
			return false
		}
	}
	return true
}

func TestInvoiceRuleProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	odd := []string{"&", "=", "?", "#", "%", " ", "\x00", "\n", "é", "\xff", "-", "+", "/", ":", "\"", "<", "'"}
	for i := 0; i < 5000; i++ {
		n := rng.Intn(20)
		if rng.Intn(10) == 0 {
			n = 4095 + rng.Intn(3)
		}
		var b strings.Builder
		for j := 0; j < n; j++ {
			if rng.Intn(15) == 0 {
				b.WriteString(odd[rng.Intn(len(odd))])
			} else {
				b.WriteByte("abcXYZ019"[rng.Intn(9)])
			}
		}
		s := b.String()
		if rng.Intn(2) == 0 && s != "" {
			s = "ln" + s
		}
		_, err := PaymentURI(testMain, 1, s)
		if (err == nil) != (s == "" || refValidInvoice(s)) {
			t.Fatalf("invoice %q: err %v, ref %v", s, err, refValidInvoice(s))
		}
	}
}

// assertHrefSafe pins the invariant the checkout page relies on when it wraps the URI in template.URL: the
// prefix is "bitcoin:" and, once the two literal lowercase keys are removed, every other byte is in
// [A-Z0-9.:?=&].
func assertHrefSafe(t testing.TB, s string) {
	t.Helper()
	rest, ok := strings.CutPrefix(s, "bitcoin:")
	if !ok {
		t.Fatalf("missing bitcoin: prefix in %q", s)
	}
	rest = strings.Replace(rest, "?amount=", "?=", 1)
	rest = strings.Replace(rest, "&lightning=", "&=", 1)
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == ':' || c == '?' || c == '=' || c == '&') {
			t.Fatalf("byte %q outside [A-Z0-9.:?=&] in %q", c, s)
		}
	}
}

// --- the Monero URI (plan 8) ---

// moneroBase58 is Monero's base58 alphabet (no 0, O, I or l).
const moneroBase58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// testSub is a subaddress of the mainnet shape: "8" and 94 base58 characters.
var testSub = "8" + strings.Repeat("Bz9", 31) + "x"

func TestMoneroURI(t *testing.T) {
	if len(testSub) != 95 {
		t.Fatalf("test subaddress of %d bytes", len(testSub))
	}
	got, err := MoneroURI(testSub, 1_500_000_000_000)
	if want := "monero:" + testSub + "?tx_amount=1.5"; err != nil || got != want {
		t.Errorf("got %q, %v; want %q", got, err, want)
	}
	got, err = MoneroURI(testSub, store.MaxPiconero)
	if want := "monero:" + testSub + "?tx_amount=9007.199254740991"; err != nil || got != want {
		t.Errorf("2^53 − 1: got %q, %v; want %q", got, err, want)
	}
	got, err = MoneroURI(testSub, 1)
	if want := "monero:" + testSub + "?tx_amount=0.000000000001"; err != nil || got != want {
		t.Errorf("1 piconero: got %q, %v; want %q", got, err, want)
	}
	for _, c := range []struct {
		name string
		addr string
		pico int64
	}{
		{"zero", testSub, 0}, {"negative", testSub, -1}, {"2^53", testSub, store.MaxPiconero + 1}, {"int64 max", testSub, 1<<63 - 1},
		{"a primary address", "4" + testSub[1:], 1}, {"a testnet primary 9", "9" + testSub[1:], 1}, {"a testnet primary A", "A" + testSub[1:], 1},
		{"a stagenet primary", "5" + testSub[1:], 1}, {"94 bytes", testSub[:94], 1}, {"96 bytes", testSub + "1", 1}, {"empty", "", 1},
		{"a zero", testSub[:94] + "0", 1}, {"an O", testSub[:94] + "O", 1}, {"an I", testSub[:94] + "I", 1}, {"an l", testSub[:94] + "l", 1},
		{"a quote", testSub[:94] + `"`, 1}, {"a question mark", testSub[:94] + "?", 1}, {"an ampersand", testSub[:94] + "&", 1},
		{"a lt", "8<" + testSub[2:], 1}, {"a space", testSub[:94] + " ", 1}, {"a non-ASCII byte", testSub[:93] + "é", 1},
	} {
		if got, err := MoneroURI(c.addr, c.pico); !errors.Is(err, ErrURI) || got != "" {
			t.Errorf("%s: got %q, %v; want ErrURI", c.name, got, err)
		}
	}
	// Every network's subaddress is accepted.
	for _, first := range []string{"8", "B", "7"} {
		if _, err := MoneroURI(first+testSub[1:], 1); err != nil {
			t.Errorf("a subaddress starting %s: %v", first, err)
		}
	}
	if _, err := MoneroURI("4"+testSub[1:]+"SECRET", 1); err != nil && strings.Contains(err.Error(), "SECRET") {
		t.Error("error text echoes input")
	}
}

// parseXMR reads a tx_amount back to piconero: digits, and optionally a point and 1 to 12 digits with no trailing zero.
func parseXMR(s string) (int64, bool) {
	whole, frac, dot := strings.Cut(s, ".")
	if whole == "" || len(whole) > 1 && whole[0] == '0' || dot && (frac == "" || len(frac) > 12 || strings.HasSuffix(frac, "0")) {
		return 0, false
	}
	var n int64
	for _, c := range whole + frac + strings.Repeat("0", 12-len(frac)) {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
		if n > store.MaxPiconero*10 {
			return 0, false
		}
	}
	return n, true
}

// refValidSub is a second, slower statement of the subaddress rule.
func refValidSub(s string) bool {
	if len(s) != 95 || !strings.ContainsRune("8B7", rune(s[0])) {
		return false
	}
	return strings.Trim(s, moneroBase58) == ""
}

// assertMoneroHrefSafe pins what template.URL relies on for the Monero URI: "monero:", 95 base58 characters,
// "?tx_amount=", then digits with at most one point; exactly one "?".
func assertMoneroHrefSafe(t testing.TB, s string) {
	t.Helper()
	rest, ok := strings.CutPrefix(s, "monero:")
	if !ok || strings.Count(s, "?") != 1 {
		t.Fatalf("not monero: with one ?: %q", s)
	}
	addr, amount, ok := strings.Cut(rest, "?tx_amount=")
	if !ok || !refValidSub(addr) || strings.Trim(amount, "0123456789.") != "" || strings.Count(amount, ".") > 1 {
		t.Fatalf("unsafe Monero URI %q", s)
	}
}

func TestMoneroURIProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	firsts := "8B7" + "49A5" + "1zZ0"
	odd := []string{"0", "O", "I", "l", "?", "&", "=", "#", "%", " ", "\x00", "<", "\"", "é", "\xff"}
	accepted := 0
	for i := 0; i < 20000; i++ {
		n := 95
		switch rng.Intn(6) {
		case 0:
			n = 94 + rng.Intn(3)
		case 1:
			n = rng.Intn(120)
		}
		var b strings.Builder
		if n > 0 {
			b.WriteByte(firsts[rng.Intn(len(firsts))])
		}
		for b.Len() < n {
			if rng.Intn(200) == 0 {
				b.WriteString(odd[rng.Intn(len(odd))])
			} else {
				b.WriteByte(moneroBase58[rng.Intn(len(moneroBase58))])
			}
		}
		addr := b.String()
		var pico int64
		switch rng.Intn(5) {
		case 0:
			pico = rng.Int63n(5) - 2 // -2 to 2
		case 1:
			pico = store.MaxPiconero - 2 + rng.Int63n(5) // around 2^53 − 1
		case 2:
			pico = rng.Int63n(4) * 1_000_000_000_000 // whole XMR, 0 included
		default:
			pico = 1 + rng.Int63n(store.MaxPiconero)
		}
		s, err := MoneroURI(addr, pico)
		ok := refValidSub(addr) && pico >= 1 && pico <= store.MaxPiconero
		if (err == nil) != ok {
			t.Fatalf("(%q, %d): err %v, validators say %v", addr, pico, err, ok)
		}
		if err != nil {
			continue
		}
		accepted++
		assertMoneroHrefSafe(t, s)
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "monero" || u.Opaque != addr {
			t.Fatalf("net/url: %v, scheme %q, opaque %q", err, u.Scheme, u.Opaque)
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(q) != 1 || len(q["tx_amount"]) != 1 {
			t.Fatalf("query %v %v", q, err)
		}
		if got, ok := parseXMR(q.Get("tx_amount")); !ok || got != pico {
			t.Fatalf("tx_amount %q -> %d %v; want %d", q.Get("tx_amount"), got, ok, pico)
		}
	}
	if accepted < 1000 {
		t.Fatalf("only %d inputs accepted", accepted)
	}
}

func FuzzMoneroURI(f *testing.F) {
	f.Add(testSub, int64(1_500_000_000_000))
	f.Add(testSub, int64(1))
	f.Add(testSub, store.MaxPiconero)
	f.Add(testSub, store.MaxPiconero+1)
	f.Add(testSub, int64(0))
	f.Add("4"+testSub[1:], int64(1))
	f.Add("B"+testSub[1:], int64(-1))
	f.Add("7"+testSub[1:94]+"<", int64(5))
	f.Add("", int64(1))
	f.Fuzz(func(t *testing.T, addr string, pico int64) {
		s, err := MoneroURI(addr, pico)
		ok := refValidSub(addr) && pico >= 1 && pico <= store.MaxPiconero
		if (err == nil) != ok {
			t.Fatalf("(%q, %d): err %v, validators say %v", addr, pico, err, ok)
		}
		if err != nil {
			if s != "" || !errors.Is(err, ErrURI) {
				t.Fatalf("refused with %q, %v", s, err)
			}
			return
		}
		assertMoneroHrefSafe(t, s)
		_, amount, _ := strings.Cut(s, "?tx_amount=")
		if got, ok := parseXMR(amount); !ok || got != pico {
			t.Fatalf("tx_amount %q -> %d; want %d", amount, got, pico)
		}
	})
}
