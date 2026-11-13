package btcrpc

import (
	"math/big"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/store"
)

func TestMaxSatAgreesWithStore(t *testing.T) {
	if MaxSat != store.MaxSat {
		t.Fatalf("btcrpc.MaxSat %d != store.MaxSat %d", MaxSat, store.MaxSat)
	}
}

func TestParseBTCAccepted(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0", 0}, {".5", 50_000_000}, {"0.00000001", 1}, {"21000000", MaxSat}, {"21000000.00000000", MaxSat},
		{"1", 100_000_000}, {"1.5", 150_000_000}, {"0.1", 10_000_000}, {"00000000000000000001", 100_000_000},
		{"0.00010000", 10_000}, {".00000001", 1}, {"20999999.99999999", MaxSat - 1},
	}
	for _, c := range cases {
		got, err := ParseBTC(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseBTC(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
}

func TestParseBTCRefused(t *testing.T) {
	bad := []string{
		"", ".", "0.", "1.", "0.000000001", "21000000.00000001", "21000001", "1e8", "1E8", " 1", "1 ", "+1", "-1", "-0",
		"0x10", "1,5", "1.5.5", "..5", "٣", "1_000", "\t1", "1\n", "\x00",
		strings.Repeat("0", 26), strings.Repeat("9", 25), "99999999999999999999999.9",
		"0." + strings.Repeat("0", 24), // 26 bytes
	}
	for _, s := range bad {
		if v, err := ParseBTC(s); err == nil {
			t.Errorf("ParseBTC(%q) = %d, want an error", s, v)
		}
	}
	// exactly 25 bytes is within the length bound
	if _, err := ParseBTC(strings.Repeat("0", 25)); err != nil {
		t.Errorf("25 zero digits are the number 0: %v", err)
	}
}

func TestParseSignedBTC(t *testing.T) {
	ok := map[string]int64{"-0.00010000": -10_000, "0.5": 50_000_000, "-21000000": -MaxSat, "-0": 0, "-.5": -50_000_000}
	for in, want := range ok {
		got, err := ParseSignedBTC(in)
		if err != nil || got != want {
			t.Errorf("ParseSignedBTC(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, s := range []string{"", "-", "--1", "+1", "-1e3", "- 1", "-21000000.00000001", "-0.", "-."} {
		if v, err := ParseSignedBTC(s); err == nil {
			t.Errorf("ParseSignedBTC(%q) = %d, want an error", s, v)
		}
	}
}

func TestFormatBTC(t *testing.T) {
	cases := map[int64]string{0: "0", 1: "0.00000001", 100_000_000: "1", 150_000_000: "1.5", 10_000: "0.0001",
		MaxSat: "21000000", MaxSat - 1: "20999999.99999999", 123_456_789: "1.23456789"}
	for sat, want := range cases {
		if got := FormatBTC(sat); got != want {
			t.Errorf("FormatBTC(%d) = %q, want %q", sat, got, want)
		}
	}
}

func TestFormatBTCPanics(t *testing.T) {
	for _, sat := range []int64{-1, MaxSat + 1, -MaxSat} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("FormatBTC(%d) did not panic", sat)
				}
			}()
			FormatBTC(sat)
		}()
	}
}

// 5,000 seeded random values (the boundaries, random magnitudes and random fractions) round-trip.
func TestBTCRoundTripProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	vals := []int64{0, 1, 99_999_999, 100_000_000, 100_000_001, MaxSat - 1, MaxSat}
	for len(vals) < 5000 {
		switch r.IntN(3) {
		case 0:
			vals = append(vals, r.Int64N(MaxSat+1))
		case 1:
			vals = append(vals, r.Int64N(1000)*100_000_000+r.Int64N(100_000_000))
		default:
			vals = append(vals, r.Int64N(MaxSat/100_000_000+1)*100_000_000)
		}
	}
	for _, v := range vals {
		s := FormatBTC(v)
		got, err := ParseBTC(s)
		if err != nil || got != v {
			t.Fatalf("round trip of %d via %q gave %d, %v", v, s, got, err)
		}
		if len(s) > 25 || strings.HasSuffix(s, ".") || (strings.Contains(s, ".") && strings.HasSuffix(s, "0")) {
			t.Fatalf("FormatBTC(%d) = %q is not canonical", v, s)
		}
	}
}

var btcRe = regexp.MustCompile(`^([0-9]+(\.[0-9]{1,8})?|\.[0-9]{1,8})$`)

// refBTC is the reference: the grammar as a regexp, at most 25 bytes, the value computed with math/big.
func refBTC(s string) (int64, bool) {
	if len(s) > 25 || !btcRe.MatchString(s) {
		return 0, false
	}
	intPart, frac, _ := strings.Cut(s, ".")
	frac += strings.Repeat("0", 8-len(frac))
	v, ok := new(big.Int).SetString(intPart+frac, 10) // intPart may be empty only with a fraction
	if intPart == "" {
		v, ok = new(big.Int).SetString(frac, 10)
	}
	if !ok || v.Cmp(big.NewInt(MaxSat)) > 0 {
		return 0, false
	}
	return v.Int64(), true
}

func FuzzParseBTC(f *testing.F) {
	for _, s := range []string{"0", ".5", "0.00000001", "21000000", "", ".", "0.", "1e8", "21000000.00000001", "-1", "1.50000000",
		"00000000000000000000001", "99999999999999999999999.9", "0.000000001", "٣"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		want, wantOK := refBTC(s)
		v, err := ParseBTC(s)
		if (err == nil) != wantOK || (wantOK && v != want) {
			t.Fatalf("ParseBTC(%q) = %d, %v; reference %d, %v", s, v, err, want, wantOK)
		}
		if err != nil {
			return
		}
		back, err := ParseBTC(FormatBTC(v))
		if err != nil || back != v {
			t.Fatalf("FormatBTC(ParseBTC(%q)) re-parses to %d, %v; want %d", s, back, err, v)
		}
		if sv, err := ParseSignedBTC(s); err != nil || sv != v {
			t.Fatalf("ParseSignedBTC(%q) = %d, %v; want %d", s, sv, err, v)
		}
	})
}

// FuzzParseSignedBTC: its own inputs, with the leading minus in play; compared with the reference.
func FuzzParseSignedBTC(f *testing.F) {
	for _, s := range []string{"-0", "-1.5", "--1", "-", "-.5", "-21000000", "-21000000.00000001", "+1", "0.1", "-0.", "- 1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var want int64
		var wantOK bool
		if rest, neg := strings.CutPrefix(s, "-"); neg {
			v, ok := refBTC(rest)
			want, wantOK = -v, ok
		} else {
			want, wantOK = refBTC(s)
		}
		v, err := ParseSignedBTC(s)
		if (err == nil) != wantOK || (wantOK && v != want) {
			t.Fatalf("ParseSignedBTC(%q) = %d, %v; reference %d, %v", s, v, err, want, wantOK)
		}
	})
}
