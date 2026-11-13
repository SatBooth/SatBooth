package xmrrpc

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestFormatXMR(t *testing.T) {
	cases := map[int64]string{
		1_000_000_000_000:     "1",
		1_500_000_000_000:     "1.5",
		1:                     "0.000000000001",
		0:                     "0",
		10:                    "0.00000000001",
		999_999_999_999:       "0.999999999999",
		1_000_000_000_001:     "1.000000000001",
		100_000_000_000_000:   "100",
		123_456_789_012_345:   "123.456789012345",
		9_007_199_254_740_991: "9007.199254740991",
		math.MaxInt64:         "9223372.036854775807",
		-1:                    "-0.000000000001",
		-1_500_000_000_000:    "-1.5",
		math.MinInt64:         "-9223372.036854775808",
	}
	for v, want := range cases {
		if got := FormatXMR(v); got != want {
			t.Errorf("FormatXMR(%d) = %q, want %q", v, got, want)
		}
	}
}

// parseXMR is the test's own reader of FormatXMR's output: an optional "-", digits, and an optional "."
// with 1 to 12 digits, the last not 0. It returns the piconero, or false.
func parseXMR(s string) (int64, bool) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, dot := strings.Cut(s, ".")
	if whole == "" || (len(whole) > 1 && whole[0] == '0') || (dot && (frac == "" || len(frac) > 12 || strings.HasSuffix(frac, "0"))) {
		return 0, false
	}
	frac += strings.Repeat("0", 12-len(frac))
	var mag uint64
	for _, c := range whole + frac {
		if c < '0' || c > '9' {
			return 0, false
		}
		if mag > (math.MaxUint64-9)/10 {
			return 0, false
		}
		mag = mag*10 + uint64(c-'0')
	}
	if neg {
		if mag == 0 || mag > 1<<63 {
			return 0, false
		}
		return int64(-mag), true // two's complement: -(1<<63) is MinInt64
	}
	if mag > math.MaxInt64 {
		return 0, false
	}
	return int64(mag), true
}

func TestFormatXMRRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(8, 12))
	vals := []int64{0, 1, -1, math.MaxInt64, math.MinInt64, math.MinInt64 + 1, 1_000_000_000_000, -1_000_000_000_000}
	for range 20000 {
		v := r.Int64() >> r.IntN(64) // every magnitude
		if r.IntN(2) == 0 {
			v = -v
		}
		if r.IntN(4) == 0 {
			v -= v % 1_000_000 // trailing zeros
		}
		vals = append(vals, v)
	}
	for _, v := range vals {
		s := FormatXMR(v)
		back, ok := parseXMR(s)
		if !ok || back != v {
			t.Fatalf("FormatXMR(%d) = %q reads back as %d %v", v, s, back, ok)
		}
	}
}
