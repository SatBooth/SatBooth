package lninvoice

import (
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/approval"
)

var allNets = []approval.Network{approval.Mainnet, approval.Testnet, approval.Signet, approval.Regtest}

// bolt11 holds the invoices of BOLT 11's examples, copied unmodified from lightning/bolts at the commit
// named in the file (11-payment-encoding.md, section "Examples" for valid ones, "Examples of Invalid
// Invoices" for invalid ones). BOLT 11 is CC-BY-4.0; see ACKNOWLEDGEMENTS.md.
type bolt11 struct {
	Commit string `json:"commit"`
	Valid  []struct {
		Line    int    `json:"line"`
		Invoice string `json:"invoice"`
	} `json:"valid"`
	Policy []struct {
		Line    int    `json:"line"`
		Invoice string `json:"invoice"`
	} `json:"refused_by_policy"`
	Invalid []struct {
		Line     int    `json:"line"`
		Invoice  string `json:"invoice"`
		SyntaxOK bool   `json:"syntax_ok"`
	} `json:"invalid"`
}

func loadBolt11(t testing.TB) bolt11 {
	t.Helper()
	raw, err := os.ReadFile("testdata/bolt11.json")
	if err != nil {
		t.Fatal(err)
	}
	var v bolt11
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// encode builds hrp + "1" + data + checksum from 5-bit values.
func encode(hrp string, data []byte) string {
	v := append(checksumInput(hrp, data), 0, 0, 0, 0, 0, 0)
	mod := polymod(v) ^ 1
	out := []byte(hrp + "1")
	for _, d := range data {
		out = append(out, charset[d])
	}
	for i := 0; i < 6; i++ {
		out = append(out, charset[(mod>>uint(5*(5-i)))&31])
	}
	return string(out)
}

// split returns the hrp and the 5-bit data values (checksum included) of a string that has a separator and
// bech32 data; the helper is for tests that start from a valid invoice.
func split(t testing.TB, s string) (string, []byte) {
	t.Helper()
	i := strings.LastIndexByte(s, '1')
	data := make([]byte, 0, len(s)-i-1)
	for _, c := range []byte(s[i+1:]) {
		data = append(data, byte(strings.IndexByte(charset, c)))
	}
	return s[:i], data
}

// withHRP re-encodes a valid invoice under another human-readable part.
func withHRP(t testing.TB, s, hrp string) string {
	_, d := split(t, s)
	return encode(hrp, d[:len(d)-6])
}

var prefixOf = map[approval.Network]string{approval.Mainnet: "lnbc", approval.Testnet: "lntb", approval.Signet: "lntbs", approval.Regtest: "lnbcrt"}

// TestCheckAccepts: BOLT 11's own valid examples are accepted on the network they belong to. The spec has
// only mainnet and testnet examples, so the signet and regtest cases are those examples re-encoded with the
// network's prefix and a fresh checksum.
func TestCheckAccepts(t *testing.T) {
	v := loadBolt11(t)
	if v.Commit != "191462825ae341f0a27098765c075f01ca9bc6c2" || len(v.Valid) < 10 {
		t.Fatalf("vector file: commit %q, %d valid", v.Commit, len(v.Valid))
	}
	var mainnet, testnet int
	for _, e := range v.Valid {
		net := approval.Mainnet
		if strings.HasPrefix(e.Invoice, "lntb") {
			net = approval.Testnet
			testnet++
		} else {
			mainnet++
		}
		if err := Check(e.Invoice, net); err != nil {
			t.Errorf("line %d: %v", e.Line, err)
		}
		// The same invoice under each network's prefix, keeping its amount.
		hrp, _ := split(t, e.Invoice)
		amount := strings.TrimPrefix(strings.TrimPrefix(hrp, "lntb"), "lnbc")
		for _, n := range allNets {
			s := withHRP(t, e.Invoice, prefixOf[n]+amount)
			if err := Check(s, n); err != nil {
				t.Errorf("line %d re-encoded for network %d: %v", e.Line, n, err)
			}
			for _, other := range allNets {
				if other != n && Check(s, other) == nil {
					t.Errorf("line %d: %s invoice accepted for network %d", e.Line, prefixOf[n], other)
				}
			}
		}
	}
	if mainnet == 0 || testnet == 0 {
		t.Fatalf("mainnet %d, testnet %d examples", mainnet, testnet)
	}
}

// TestSpecRefusedByPolicy: BOLT 11 says an all-uppercase invoice is valid; the check accepts lowercase only, so it
// is refused, and its lowercase form is accepted.
func TestSpecRefusedByPolicy(t *testing.T) {
	v := loadBolt11(t)
	if len(v.Policy) != 1 {
		t.Fatalf("%d policy vectors", len(v.Policy))
	}
	for _, e := range v.Policy {
		rejects(t, fmt.Sprintf("line %d", e.Line), e.Invoice, approval.Mainnet)
		if err := Check(strings.ToLower(e.Invoice), approval.Mainnet); err != nil {
			t.Errorf("line %d lowercased: %v", e.Line, err)
		}
	}
}

// TestSpecInvalid: of BOLT 11's invalid examples, the ones broken in syntax are refused, and the ones that
// are broken only in content (field lengths, missing fields, signature) are accepted: Check reads no field.
func TestSpecInvalid(t *testing.T) {
	for _, e := range loadBolt11(t).Invalid {
		err := Check(e.Invoice, approval.Mainnet)
		if e.SyntaxOK != (err == nil) {
			t.Errorf("line %d: err %v, syntax ok %v", e.Line, err, e.SyntaxOK)
		}
	}
}

// TestBech32Vectors: the BIP-173 valid test strings run through the checksum (they are not invoices, so
// only polymod is used) and a flipped character breaks each.
func TestBech32Vectors(t *testing.T) {
	for _, s := range []string{
		"a12uel5l", "an83characterlonghumanreadablepartthatcontainsthenumber1andtheexcludedcharactersbio1tt5tgs",
		"abcdef1qpzry9x8gf2tvdw0s3jn54khce6mua7lmqqqxw", "split1checkupstagehandshakeupstreamerranterredcaperred2y9e3w",
	} {
		hrp, data := split(t, s)
		if polymod(checksumInput(hrp, data)) != 1 {
			t.Errorf("%q: checksum fails", s)
		}
		if polymod(checksumInput(hrp, append([]byte{}, data[:len(data)-1]...))) == 1 {
			t.Errorf("%q: truncated string verifies", s)
		}
	}
}

func rejects(t *testing.T, name, s string, net approval.Network) {
	t.Helper()
	if err := Check(s, net); err != ErrInvalid {
		t.Errorf("%s: got %v, want ErrInvalid", name, err)
	}
}

func TestCheckRejects(t *testing.T) {
	v := loadBolt11(t)
	var good, goodTB string
	for _, e := range v.Valid {
		if strings.HasPrefix(e.Invoice, "lnbc20m1") && good == "" {
			good = e.Invoice
		}
		if strings.HasPrefix(e.Invoice, "lntb") {
			goodTB = e.Invoice
		}
	}
	if good == "" || goodTB == "" {
		t.Fatal("no base invoices")
	}
	if err := Check(good, approval.Mainnet); err != nil {
		t.Fatal(err)
	}
	m := approval.Mainnet

	rejects(t, "empty", "", m)
	rejects(t, "uppercase", strings.ToUpper(good), m)
	rejects(t, "mixed case", good[:30]+strings.ToUpper(good[30:31])+good[31:], m)
	rejects(t, "uppercase prefix only", "LN"+good[2:], m)
	rejects(t, "leading space", " "+good, m)
	rejects(t, "trailing space", good+" ", m)
	rejects(t, "inner space", good[:40]+" "+good[40:], m)
	rejects(t, "trailing newline", good+"\n", m)
	rejects(t, "trailing CR LF", good+"\r\n", m)
	rejects(t, "tab", good[:40]+"\t"+good[40:], m)
	rejects(t, "NUL", good[:40]+"\x00"+good[41:], m)
	rejects(t, "DEL", good[:40]+"\x7f"+good[41:], m)
	rejects(t, "non-ASCII", good[:40]+"é"+good[42:], m)
	rejects(t, "lightning: prefix", "lightning:"+good, m)
	rejects(t, "LIGHTNING: prefix", "LIGHTNING:"+good, m)
	rejects(t, "no separator", strings.ReplaceAll(good, "1", "q"), m)
	rejects(t, "no hrp", good[strings.LastIndexByte(good, '1'):], m)
	rejects(t, "hrp only", good[:strings.LastIndexByte(good, '1')], m)

	// Network.
	rejects(t, "testnet for mainnet", goodTB, m)
	rejects(t, "mainnet for testnet", good, approval.Testnet)
	rejects(t, "mainnet for signet", good, approval.Signet)
	rejects(t, "mainnet for regtest", good, approval.Regtest)
	rejects(t, "lnbcrt for mainnet", withHRP(t, good, "lnbcrt20m"), m)
	rejects(t, "lnbc for regtest", good, approval.Regtest)
	rejects(t, "lntbs for testnet", withHRP(t, good, "lntbs20m"), approval.Testnet)
	rejects(t, "lntb for signet", goodTB, approval.Signet)
	rejects(t, "unknown network value", good, approval.Network(4))
	rejects(t, "network 255", good, approval.Network(255))

	// The human-readable part.
	for _, hrp := range []string{
		"", "ln", "lnbc ", "lnbcx", "lnbc20x", "lnbc20mm", "lnbcm", "lnbc-1", "lnbc2.5m", "lnbc20 m", "lnbc+20m",
		"lnbc٢٠m", "lnb", "lbc20m", "xlnbc20m", "lnbc20M", "lnbc20mu", "lnbcp1p", "bc20m", "LNBC20m", "lnbc20m ", "lnbc\n20m",
	} {
		rejects(t, fmt.Sprintf("hrp %q", hrp), withHRP(t, good, hrp), m)
	}
	for _, hrp := range []string{"lnbc", "lnbc1", "lnbc0", "lnbc0001", "lnbc20m", "lnbc9999999999999999999p", "lnbc5n", "lnbc7u", "lnbc1m"} {
		if err := Check(withHRP(t, good, hrp), m); err != nil {
			t.Errorf("hrp %q: %v", hrp, err)
		}
	}

	// Data part: too short with a valid checksum, one short of the minimum, the minimum.
	hrp := "lnbc20m"
	for n := 0; n < 117; n++ {
		s := encode(hrp, make([]byte, max(n-6, 0)))
		if n < 6 {
			continue
		}
		if err := Check(s, m); err == nil {
			t.Errorf("data length %d accepted", n)
		}
	}
	rejects(t, "empty data", hrp+"1", m)
	rejects(t, "data of five characters", hrp+"1qqqqq", m)
	if err := Check(encode(hrp, make([]byte, 111)), m); err != nil {
		t.Errorf("117-character data part: %v", err)
	}
	rejects(t, "116-character data part", encode(hrp, make([]byte, 110)), m)

	// Characters outside bech32 in the data: b, i, o, 1 and others, at several offsets.
	for _, c := range []string{"b", "i", "o", "1", "B", "-", "_", ".", "/", "=", "+", "$", "~", "!"} {
		for _, pos := range []int{len(good) - 1, len(good) - 7, len(good) - 100, strings.LastIndexByte(good, '1') + 1} {
			s := good[:pos] + c + good[pos+1:]
			rejects(t, fmt.Sprintf("char %q at %d", c, pos), s, m)
		}
	}

	// A bad checksum: every character of the data part replaced by every other bech32 character.
	sep := strings.LastIndexByte(good, '1')
	for i := sep + 1; i < len(good); i++ {
		for _, c := range []byte(charset) {
			if c == good[i] {
				continue
			}
			if Check(good[:i]+string(c)+good[i+1:], m) == nil {
				t.Fatalf("character %d replaced by %c and accepted", i, c)
			}
		}
	}
	// Two characters swapped, and a character dropped.
	rejects(t, "dropped", good[:50]+good[51:], m)
	rejects(t, "doubled", good[:50]+good[50:51]+good[50:], m)
	rejects(t, "last char dropped", good[:len(good)-1], m)
	rejects(t, "amount edited", "lnbc21m"+good[7:], m)

	// Length: 2,048 bytes pass with a valid checksum, 2,049 do not.
	long := func(n int) string {
		h := "lnbc20m"
		return encode(h, make([]byte, n-len(h)-1-6))
	}
	if s := long(2048); len(s) != 2048 {
		t.Fatalf("builder: %d", len(s))
	} else if err := Check(s, m); err != nil {
		t.Errorf("2,048 bytes: %v", err)
	}
	rejects(t, "2,049 bytes", long(2049), m)
	rejects(t, "4,096 bytes", long(4096), m)
	rejects(t, "huge", long(1<<20), m)
}

func randomData(rng *rand.Rand, n int) []byte {
	d := make([]byte, n)
	for i := range d {
		d[i] = byte(rng.Intn(32))
	}
	return d
}

// TestProperty: seeded random invoices. A string built with a correct checksum for the network's prefix and
// an optional amount is accepted exactly when its data is long enough and the total is within 2,048 bytes;
// a single replaced character always breaks it (the code detects any single-character error); the
// accepted set is closed under nothing else.
func TestProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261109))
	amounts := []string{"", "1", "20m", "2500u", "9678785340p", "5n", "0", "007", "123456789012345678901234567890"}
	for i := 0; i < 4000; i++ {
		net := allNets[rng.Intn(4)]
		hrp := prefixOf[net] + amounts[rng.Intn(len(amounts))]
		var n int
		switch rng.Intn(3) {
		case 0:
			n = rng.Intn(140)
		case 1:
			n = 100 + rng.Intn(20)
		default:
			n = 111 + rng.Intn(1200)
		}
		s := encode(hrp, randomData(rng, n))
		dataLen := n + 6
		want := dataLen >= 117 && len(s) <= 2048
		if got := Check(s, net) == nil; got != want {
			t.Fatalf("case %d: %d data chars, %d bytes: accepted %v", i, dataLen, len(s), got)
		}
		for _, other := range allNets {
			if other != net && Check(s, other) == nil {
				t.Fatalf("case %d: accepted for network %d as well as %d", i, other, net)
			}
		}
		if want {
			// Replace one data character.
			pos := len(hrp) + 1 + rng.Intn(dataLen)
			c := charset[(strings.IndexByte(charset, s[pos])+1+rng.Intn(31))%32]
			if Check(s[:pos]+string(c)+s[pos+1:], net) == nil {
				t.Fatalf("case %d: replacement at %d accepted", i, pos)
			}
			// Uppercase one letter, if there is one.
			if j := strings.IndexAny(s, "abcdefghjklmnpqrstuvwxyz"); j >= 0 {
				rejects(t, "uppercased", s[:j]+strings.ToUpper(s[j:j+1])+s[j+1:], net)
			}
		}
	}
	// Random bytes never panic and are never accepted.
	for i := 0; i < 20000; i++ {
		b := make([]byte, rng.Intn(300))
		rng.Read(b)
		if Check(string(b), approval.Network(rng.Intn(6))) == nil {
			t.Fatalf("random bytes accepted: %d bytes", len(b))
		}
	}
}

// FuzzCheck: no panic, and anything accepted is exactly its own re-encode: lowercase, within the limits,
// its checksum equals the one computed from its human-readable part and data.
func FuzzCheck(f *testing.F) {
	v := loadBolt11(f)
	for _, e := range v.Valid {
		f.Add(e.Invoice, uint8(0))
	}
	for _, e := range v.Invalid {
		f.Add(e.Invoice, uint8(0))
	}
	f.Add("", uint8(0))
	f.Add("lightning:lnbc1", uint8(1))
	f.Add(encode("lnbcrt", make([]byte, 111)), uint8(3))
	f.Add(encode("lntbs1u", make([]byte, 111)), uint8(2))
	f.Fuzz(func(t *testing.T, s string, n uint8) {
		net := approval.Network(n % 5)
		if Check(s, net) != nil {
			return
		}
		if len(s) < 1 || len(s) > 2048 || strings.ToLower(s) != s || strings.ContainsAny(s, " \t\r\n") {
			t.Fatalf("accepted a string out of bounds: %d bytes", len(s))
		}
		hrp, data := split(t, s)
		if !strings.HasPrefix(hrp, prefixOf[net]) || len(data) < 117 {
			t.Fatalf("accepted with hrp %q and %d data characters for network %d", hrp, len(data), net)
		}
		if re := encode(hrp, data[:len(data)-6]); re != s {
			t.Fatalf("accepted string is not its own re-encode")
		}
	})
}

// amountOracle is AmountMsat's rule computed with math/big, independently of the code under test: the hrp amount
// in msat, whether there is one, and whether it is accepted.
func amountOracle(amt string) (msat int64, present, ok bool) {
	if amt == "" {
		return 0, false, true
	}
	digits, exp := amt, 11 // msat per BTC = 10^11
	switch amt[len(amt)-1] {
	case 'm':
		digits, exp = amt[:len(amt)-1], 8
	case 'u':
		digits, exp = amt[:len(amt)-1], 5
	case 'n':
		digits, exp = amt[:len(amt)-1], 2
	case 'p':
		digits, exp = amt[:len(amt)-1], -1
	}
	if digits == "" || digits[0] == '0' {
		return 0, true, false
	}
	v, good := new(big.Int).SetString(digits, 10)
	if !good {
		return 0, true, false
	}
	if exp < 0 {
		q, r := new(big.Int).QuoRem(v, big.NewInt(10), new(big.Int))
		if r.Sign() != 0 {
			return 0, true, false
		}
		v = q
	} else {
		v.Mul(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil))
	}
	if v.Sign() <= 0 || v.Cmp(big.NewInt(approval.MaxAmountMsat)) > 0 {
		return 0, true, false
	}
	return v.Int64(), true, true
}

// checkAmount builds a valid invoice with hrp amount amt for net and compares AmountMsat with the oracle.
func checkAmount(t *testing.T, amt string, net approval.Network, data []byte) {
	t.Helper()
	s := encode(prefixOf[net]+amt, data)
	if Check(s, net) != nil {
		t.Fatalf("amount %q: the test invoice does not pass Check", amt)
	}
	msat, present, err := AmountMsat(s, net)
	wMsat, wPresent, wOK := amountOracle(amt)
	switch {
	case wOK && (err != nil || present != wPresent || msat != wMsat):
		t.Fatalf("amount %q: got %d %v %v, want %d %v", amt, msat, present, err, wMsat, wPresent)
	case !wOK && err != ErrInvalid:
		t.Fatalf("amount %q: got %d %v %v, want ErrInvalid", amt, msat, present, err)
	case err != nil && (msat != 0 || present):
		t.Fatalf("amount %q: a refusal returned %d %v", amt, msat, present)
	}
}

// TestAmountMsat: every boundary of the hrp amount, each against a fixed answer.
func TestAmountMsat(t *testing.T) {
	data := make([]byte, 111)
	const max = approval.MaxAmountMsat // 9007199254740992
	cases := []struct {
		amt     string
		msat    int64
		present bool
		ok      bool
	}{
		{"", 0, false, true},
		{"1", 100_000_000_000, true, true},
		{"1m", 100_000_000, true, true},
		{"1u", 100_000, true, true},
		{"1n", 100, true, true},
		{"10p", 1, true, true},
		{"2500u", 250_000_000, true, true},
		{"20m", 2_000_000_000, true, true},
		{"9678785340p", 967_878_534, true, true},
		{"1p", 0, true, false},  // a tenth of a msat
		{"11p", 0, true, false}, // not a whole msat
		{"0", 0, true, false},   // zero
		{"0m", 0, true, false},  // zero
		{"00p", 0, true, false}, // zero, leading zero
		{"01u", 0, true, false}, // leading zero
		{"007", 0, true, false}, // leading zero
		{"90071", 9_007_100_000_000_000, true, true},
		{"90072", 0, true, false}, // above 2^53 msat
		{"90071992547409920p", max, true, true},
		{"90071992547409910p", max - 1, true, true},
		{"90071992547409930p", 0, true, false}, // 2^53 + 1
		{"9007199254740992n", 0, true, false},  // 100 * 2^53
		{"90071992547409n", 9_007_199_254_740_900, true, true},
		{"90071992547410n", 0, true, false},
		{"90071992547u", 9_007_199_254_700_000, true, true},
		{"90071992548u", 0, true, false},
		{"90071992m", 9_007_199_200_000_000, true, true},
		{"90071993m", 0, true, false},
		{strings.Repeat("9", 1800), 0, true, false}, // the longest digit runs: no overflow
		{strings.Repeat("9", 1800) + "p", 0, true, false},
		{"1" + strings.Repeat("0", 1800) + "p", 0, true, false},
	}
	for _, c := range cases {
		for _, net := range allNets {
			s := encode(prefixOf[net]+c.amt, data)
			msat, present, err := AmountMsat(s, net)
			if c.ok && (err != nil || msat != c.msat || present != c.present) {
				t.Fatalf("%q on %d: got %d %v %v, want %d %v", c.amt, net, msat, present, err, c.msat, c.present)
			}
			if !c.ok && (err != ErrInvalid || msat != 0 || present) {
				t.Fatalf("%q on %d: got %d %v %v, want ErrInvalid", c.amt, net, msat, present, err)
			}
			checkAmount(t, c.amt, net, data)
		}
	}
	// An invoice that fails Check is refused before any amount is read: another network, a bad checksum, empty.
	s := encode("lnbcrt20m", data)
	for _, bad := range []struct {
		s   string
		net approval.Network
	}{{s, approval.Mainnet}, {s[:len(s)-1] + "q", approval.Regtest}, {"", approval.Regtest}, {s, approval.Network(4)}} {
		if msat, present, err := AmountMsat(bad.s, bad.net); err != ErrInvalid || msat != 0 || present {
			t.Fatalf("an invoice that fails Check: got %d %v %v", msat, present, err)
		}
	}
}

// TestAmountMsatProperty: seeded random amounts in every unit and length, compared with the math/big oracle.
func TestAmountMsatProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261110))
	units := []string{"", "m", "u", "n", "p"}
	data := randomData(rng, 111)
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		n := 1 + rng.Intn(20)
		if rng.Intn(50) == 0 {
			n = 1 + rng.Intn(1800)
		}
		for j := 0; j < n; j++ {
			d := byte('0' + rng.Intn(10))
			if j == 0 && rng.Intn(4) != 0 && d == '0' {
				d = '1' + byte(rng.Intn(9))
			}
			if j == n-1 && rng.Intn(3) == 0 {
				d = '0'
			}
			b.WriteByte(d)
		}
		checkAmount(t, b.String()+units[rng.Intn(len(units))], allNets[rng.Intn(4)], data)
	}
	// Around 2^53 msat in each unit.
	for _, u := range []struct {
		unit string
		per  int64 // msat per unit, or -10 for p (ten units per msat)
	}{{"", 100_000_000_000}, {"m", 100_000_000}, {"u", 100_000}, {"n", 100}, {"p", -10}} {
		for d := int64(-3); d <= 3; d++ {
			var v int64
			if u.per < 0 {
				v = (approval.MaxAmountMsat + d) * 10
			} else {
				v = approval.MaxAmountMsat/u.per + d
			}
			if v > 0 {
				checkAmount(t, fmt.Sprint(v)+u.unit, approval.Regtest, data)
			}
		}
	}
}

// FuzzAmountMsat: no panic; a refusal returns nothing; an accepted amount passes Check, lies in (0, 2^53] and equals
// the math/big oracle's reading of the hrp amount.
func FuzzAmountMsat(f *testing.F) {
	data := make([]byte, 111)
	for _, a := range []string{"", "1", "1m", "1u", "1n", "10p", "1p", "0", "01u", "90071992547409920p",
		"90071992547409930p", strings.Repeat("9", 1800)} {
		f.Add(encode("lnbcrt"+a, data), uint8(3))
	}
	f.Add("", uint8(0))
	f.Fuzz(func(t *testing.T, s string, n uint8) {
		net := approval.Network(n % 5)
		msat, present, err := AmountMsat(s, net)
		if err != nil {
			if msat != 0 || present {
				t.Fatalf("a refusal returned %d %v", msat, present)
			}
			return
		}
		if Check(s, net) != nil {
			t.Fatalf("accepted an invoice that fails Check")
		}
		hrp, _ := split(t, s)
		wMsat, wPresent, wOK := amountOracle(hrp[len(prefixOf[net]):])
		if !wOK || msat != wMsat || present != wPresent {
			t.Fatalf("got %d %v, oracle %d %v %v", msat, present, wMsat, wPresent, wOK)
		}
		if present && (msat <= 0 || msat > approval.MaxAmountMsat) {
			t.Fatalf("amount %d out of range", msat)
		}
	})
}
