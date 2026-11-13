package rates

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// The grammars, written independently of the parser as the oracle for the property and fuzz tests. Go's
// regexp `$` matches only at the end of the text and `[0-9]` only ASCII digits.
var (
	decimalGrammar = regexp.MustCompile(`^[0-9]{1,20}(\.[0-9]{1,18})?$`)
	percentGrammar = regexp.MustCompile(`^-?[0-9]{1,4}(\.[0-9]{1,2})?$`)
)

// The fixed error texts: a refusal never carries the input.
var (
	decimalErrTexts = fixedTexts("rates: invalid decimal")
	percentErrTexts = fixedTexts("rates: invalid percent")
)

func fixedTexts(prefix string) map[string]bool {
	m := map[string]bool{}
	for _, r := range []string{"empty", "too long", "a byte other than a digit or a point",
		"more than one point", "no digit before the point", "too many integer digits",
		"no digit after the point", "too many fraction digits"} {
		m[prefix+": "+r] = true
	}
	return m
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

func digits(rng *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('0' + rng.Intn(10))
	}
	return string(b)
}

// ratUnits is the oracle for the units of an accepted decimal: big.Rat's own parse, times 10^18.
func ratUnits(t testing.TB, s string) *big.Int {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("oracle cannot parse an accepted input (%d bytes)", len(s))
	}
	r.Mul(r, new(big.Rat).SetInt(pow10(Scale)))
	if !r.IsInt() {
		t.Fatalf("an accepted input has more than %d fraction digits", Scale)
	}
	return r.Num()
}

func TestParseDecimalVectors(t *testing.T) {
	nines20 := strings.Repeat("9", 20)
	for _, v := range []struct{ in, units, str string }{
		{"1", "1000000000000000000", "1"},
		{"0.5", "500000000000000000", "0.5"},
		{"76005.70000", "76005700000000000000000", "76005.7"},
		{"007.50", "7500000000000000000", "7.5"},
		{"0", "0", "0"},
		{"0.000", "0", "0"},
		{"100", "100000000000000000000", "100"},
		{"12.5", "12500000000000000000", "12.5"},
		{"0.000000000000000001", "1", "0.000000000000000001"},
		{nines20, nines20 + strings.Repeat("0", 18), nines20},
		{nines20 + "." + strings.Repeat("9", 18), strings.Repeat("9", 38), nines20 + "." + strings.Repeat("9", 18)},
		{"11979726.3168333333308008", "11979726316833333330800800", "11979726.3168333333308008"},
	} {
		d, err := ParseDecimal(v.in)
		if err != nil {
			t.Errorf("ParseDecimal(%q): %v", v.in, err)
			continue
		}
		want, _ := new(big.Int).SetString(v.units, 10)
		if d.Units().Cmp(want) != 0 {
			t.Errorf("ParseDecimal(%q) units = %s, want %s", v.in, d.Units(), want)
		}
		if got := d.String(); got != v.str {
			t.Errorf("ParseDecimal(%q).String() = %q, want %q", v.in, got, v.str)
		}
		if d.IsZero() != (want.Sign() == 0) {
			t.Errorf("ParseDecimal(%q).IsZero() = %v", v.in, d.IsZero())
		}
		back, err := ParseDecimal(d.String())
		if err != nil || back.Cmp(d) != 0 {
			t.Errorf("ParseDecimal(%q) does not round-trip: %v", v.in, err)
		}
	}
}

// decimalRefusals are inputs ParseDecimal must refuse; byte-level values are built from bytes.
func decimalRefusals() []string {
	return []string{
		"", ".", "1.", ".5", "1.2.3", "1..2", "-1", "+1", "1e5", "1E5", " 1", "1 ", "1,5", "0x10", "1_000",
		strings.Repeat("1", 21),                                 // 21 integer digits
		"1." + strings.Repeat("1", 19),                          // 19 fraction digits
		strings.Repeat("1", 20) + "." + strings.Repeat("1", 19), // 40 bytes
		strings.Repeat("1", 40),                                 // 40 bytes, digits only
		"1" + string([]byte{0x00}),                              // 1 then NUL
		string([]byte{0x00}) + "1",                              // NUL then 1
		"1" + string([]byte{0xc2, 0xa0}),                        // 1 then a non-breaking space
		string([]byte{0xef, 0xbc, 0x91}),                        // a full-width 1
		string([]byte{0xef, 0xbc, 0x90}),                        // a full-width 0
		"1" + string([]byte{0xef, 0xbc, 0x8e}) + "5",            // a full-width full stop
		string([]byte{0xd9, 0xa1}),                              // an Arabic-Indic 1
		"1" + string([]byte{0x2f}), "1" + string([]byte{0x3a}),  // the bytes either side of the digits
		"1" + string([]byte{0x2d}) + "1", "1" + string([]byte{0x7f}), // a minus inside, DEL
		"1\n", "1\t", "\n1",
		"NaN", "Inf", "inf", "-Inf", "Infinity", "0x1p3",
	}
}

func TestParseDecimalRefusals(t *testing.T) {
	for _, s := range decimalRefusals() {
		d, err := ParseDecimal(s)
		if err == nil {
			t.Errorf("ParseDecimal(% x) = %s, want a refusal", s, d)
			continue
		}
		if !errors.Is(err, ErrDecimal) {
			t.Errorf("ParseDecimal(% x): error does not wrap ErrDecimal: %v", s, err)
		}
		if !decimalErrTexts[err.Error()] {
			t.Errorf("ParseDecimal(% x): error text %q is not one of the fixed texts", s, err.Error())
		}
		if !d.IsZero() {
			t.Errorf("ParseDecimal(% x): a refusal returned a non-zero value", s)
		}
	}
	// A distinctive refused input never appears in the error.
	if _, err := ParseDecimal("1234567.89xyz"); err == nil || strings.Contains(err.Error(), "xyz") ||
		strings.Contains(err.Error(), "1234567") {
		t.Errorf("error carries the input: %v", err)
	}
}

func TestDecimalRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(20261106))
	intLens := []int{0, 1, 20, 21}
	fracLens := []int{-1, 0, 1, 18, 19} // -1: no point at all; 0: a trailing point
	var prev Decimal
	var prevUnits *big.Int
	for i := 0; i < 10000; i++ {
		il := intLens[rng.Intn(len(intLens))]
		if rng.Intn(3) == 0 {
			il = rng.Intn(22)
		}
		fl := fracLens[rng.Intn(len(fracLens))]
		if rng.Intn(3) == 0 {
			fl = rng.Intn(20)
		}
		ip := digits(rng, il)
		if il > 1 && rng.Intn(2) == 0 { // leading zeros
			z := 1 + rng.Intn(il-1)
			ip = strings.Repeat("0", z) + ip[z:]
		}
		s := ip
		if fl >= 0 {
			s += "." + digits(rng, fl)
		}
		wantOK := il >= 1 && il <= MaxIntDigits && fl != 0 && fl <= MaxFracDigits
		d, err := ParseDecimal(s)
		if (err == nil) != wantOK {
			t.Fatalf("ParseDecimal(%q): err %v, want accepted %v", s, err, wantOK)
		}
		if err != nil {
			if !errors.Is(err, ErrDecimal) || !decimalErrTexts[err.Error()] {
				t.Fatalf("ParseDecimal(%q): bad refusal %v", s, err)
			}
			continue
		}
		units := ratUnits(t, s)
		if d.Units().Cmp(units) != 0 {
			t.Fatalf("ParseDecimal(%q) units %s, want %s", s, d.Units(), units)
		}
		str := d.String()
		back, err := ParseDecimal(str)
		if err != nil || back.Cmp(d) != 0 || back.String() != str {
			t.Fatalf("ParseDecimal(%q): String %q does not round-trip (%v)", s, str, err)
		}
		if str != "0" && (strings.HasPrefix(str, "0") && !strings.HasPrefix(str, "0.") || strings.HasSuffix(str, "0") && strings.Contains(str, ".") || strings.HasSuffix(str, ".")) {
			t.Fatalf("String %q is not canonical", str)
		}
		if prevUnits != nil && d.Cmp(prev) != units.Cmp(prevUnits) {
			t.Fatalf("Cmp(%s, %s) = %d, big.Int says %d", d, prev, d.Cmp(prev), units.Cmp(prevUnits))
		}
		if d.Cmp(d) != 0 {
			t.Fatalf("Cmp(%s, itself) != 0", d)
		}
		prev, prevUnits = d, units
	}
}

func TestDecimalImmutable(t *testing.T) {
	d, err := ParseDecimal("12.5")
	if err != nil {
		t.Fatal(err)
	}
	u := d.Units()
	u.SetInt64(7)
	if d.String() != "12.5" {
		t.Errorf("mutating Units() changed the decimal: %s", d)
	}
	var zero Decimal
	if !zero.IsZero() || zero.String() != "0" || zero.Units().Sign() != 0 || zero.Cmp(Decimal{}) != 0 || zero.Cmp(d) != -1 || d.Cmp(zero) != 1 {
		t.Error("the zero value is not 0")
	}
	zero.Units().SetInt64(5)
	if !zero.IsZero() {
		t.Error("mutating the zero value's Units() changed it")
	}
}

func TestPercent(t *testing.T) {
	for _, v := range []struct {
		in    string
		bps   int64
		canon string
	}{
		{"1.5", 150, "1.5"}, {"-2", -200, "-2"}, {"0", 0, "0"}, {"0.01", 1, "0.01"}, {"-0", 0, "0"},
		{"-0.00", 0, "0"}, {"-0.05", -5, "-0.05"}, {"1.50", 150, "1.5"}, {"0001.5", 150, "1.5"},
		{"9999.99", 999999, "9999.99"}, {"-9999.99", -999999, "-9999.99"}, {"50", 5000, "50"}, {"-50", -5000, "-50"},
		{"12.34", 1234, "12.34"}, {"0.1", 10, "0.1"},
	} {
		got, err := ParsePercent(v.in)
		if err != nil || got != v.bps {
			t.Errorf("ParsePercent(%q) = %d, %v; want %d", v.in, got, err, v.bps)
		}
		if f := FormatPercent(v.bps); f != v.canon {
			t.Errorf("FormatPercent(%d) = %q, want %q", v.bps, f, v.canon)
		}
	}
	for _, s := range []string{"", "1.555", "+1", "--1", "-", ".5", "1.", "1e2", "12345", "1-", "1.5-", "- 1", " 1",
		"1 ", "1,5", "-.5", "1..5", "1.2.3", "00001", "NaN", "-Inf", "1%", "1" + string([]byte{0x00}), "1" + string([]byte{0x3a}), "1" + string([]byte{0x2f}),
		string([]byte{0xef, 0xbc, 0x91}), string([]byte{0xe2, 0x88, 0x92}) + "1"} { // a full-width 1; U+2212 minus
		b, err := ParsePercent(s)
		if err == nil {
			t.Errorf("ParsePercent(% x) = %d, want a refusal", s, b)
			continue
		}
		if !errors.Is(err, ErrPercent) || !percentErrTexts[err.Error()] {
			t.Errorf("ParsePercent(% x): bad refusal %v", s, err)
		}
	}
	for _, b := range []int64{1000000, -1000000, math.MaxInt64, math.MinInt64, math.MinInt64 + 1} {
		f := FormatPercent(b) // formats without panicking
		if _, err := ParsePercent(f); err == nil {
			t.Errorf("ParsePercent(FormatPercent(%d)) = %q accepted, outside the grammar", b, f)
		}
	}
	if f := FormatPercent(math.MinInt64); f != "-92233720368547758.08" {
		t.Errorf("FormatPercent(MinInt64) = %q", f)
	}
}

func TestPercentRoundTrip(t *testing.T) {
	check := func(b int64) {
		f := FormatPercent(b)
		got, err := ParsePercent(f)
		if err != nil || got != b {
			t.Fatalf("ParsePercent(FormatPercent(%d) = %q) = %d, %v", b, f, got, err)
		}
		// The text is canonical: no "-0", no trailing fraction zero, no trailing point.
		if f == "-0" || strings.HasSuffix(f, ".") || strings.Contains(f, ".") && strings.HasSuffix(f, "0") {
			t.Fatalf("FormatPercent(%d) = %q is not canonical", b, f)
		}
		// The oracle: big.Rat reads the percent text as b/100.
		r, ok := new(big.Rat).SetString(f)
		if !ok || r.Cmp(big.NewRat(b, 100)) != 0 {
			t.Fatalf("FormatPercent(%d) = %q is not %d/100", b, f, b)
		}
	}
	for b := int64(-999999); b <= 999999; b += 7 {
		check(b)
	}
	for _, b := range []int64{-999999, -100, -10, -1, 0, 1, 10, 100, 999999} {
		check(b)
	}
	rng := rand.New(rand.NewSource(20261106))
	for i := 0; i < 10000; i++ {
		check(rng.Int63n(1999999) - 999999)
	}
}

func FuzzParseDecimal(f *testing.F) {
	for _, s := range []string{"1", "0.5", "76005.70000", "007.50", "0", "0.000", strings.Repeat("9", 20),
		strings.Repeat("9", 20) + "." + strings.Repeat("9", 18), "11979726.3168333333308008"} {
		f.Add(s)
	}
	for _, s := range decimalRefusals() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDecimal(s)
		if err != nil {
			if !errors.Is(err, ErrDecimal) || !decimalErrTexts[err.Error()] {
				t.Fatalf("bad refusal: %v", err)
			}
			if decimalGrammar.MatchString(s) {
				t.Fatalf("refused an input of the grammar: %q", s)
			}
			return
		}
		if !decimalGrammar.MatchString(s) {
			t.Fatalf("accepted an input outside the grammar: % x", s)
		}
		if d.Units().Cmp(ratUnits(t, s)) != 0 {
			t.Fatalf("ParseDecimal(%q) has the wrong value %s", s, d)
		}
		back, err := ParseDecimal(d.String())
		if err != nil || back.Cmp(d) != 0 {
			t.Fatalf("ParseDecimal(%q): String %q does not round-trip", s, d.String())
		}
	})
}

func FuzzParsePercent(f *testing.F) {
	for _, s := range []string{"1.5", "-2", "0", "0.01", "-0", "-0.00", "9999.99", "-9999.99", "1.555", "+1", "--1",
		"-", ".5", "1.", "1e2", "12345", "1-", "0001.5", "00001", "1" + string([]byte{0x00})} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, err := ParsePercent(s)
		if err != nil {
			if !errors.Is(err, ErrPercent) || !percentErrTexts[err.Error()] {
				t.Fatalf("bad refusal: %v", err)
			}
			if percentGrammar.MatchString(s) {
				t.Fatalf("refused an input of the grammar: %q", s)
			}
			return
		}
		if !percentGrammar.MatchString(s) {
			t.Fatalf("accepted an input outside the grammar: % x", s)
		}
		r, ok := new(big.Rat).SetString(s)
		if !ok || r.Cmp(big.NewRat(b, 100)) != 0 {
			t.Fatalf("ParsePercent(%q) = %d, the wrong value", s, b)
		}
		if got, err := ParsePercent(FormatPercent(b)); err != nil || got != b {
			t.Fatalf("FormatPercent(%d) does not round-trip", b)
		}
	})
}
