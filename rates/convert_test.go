package rates

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"strings"
	"testing"
)

func dec(t testing.TB, s string) Decimal {
	t.Helper()
	d, err := ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return d
}

func units(u *big.Int) Decimal { return Decimal{units: new(big.Int).Set(u)} }

// assetScale is the oracle's own table of each asset's base units per coin and bound, written out here, not read
// from convert.go.
var assetScale = map[string]struct{ perCoin, max int64 }{
	AssetBTC: {100_000_000, 2_100_000_000_000_000},
	AssetXMR: {1_000_000_000_000, 1<<53 - 1},
}

// numDen is the rational the conversion takes the ceiling of, built from the decimals' text through
// big.Rat (not from the formula in convert.go): price × 10^8 × (1 + margin/10000) / rate.
func numDen(t testing.TB, price, rate Decimal, m int64) *big.Rat {
	t.Helper()
	return numDenIn(t, AssetBTC, price, rate, m)
}

// numDenIn is numDen for asset: price × the asset's base units per coin × (1 + margin/10000) / rate.
func numDenIn(t testing.TB, asset string, price, rate Decimal, m int64) *big.Rat {
	t.Helper()
	p, ok1 := new(big.Rat).SetString(price.String())
	r, ok2 := new(big.Rat).SetString(rate.String())
	if !ok1 || !ok2 {
		t.Fatal("oracle cannot read a decimal's text")
	}
	q := new(big.Rat).Mul(p, new(big.Rat).SetInt64(assetScale[asset].perCoin))
	q.Mul(q, new(big.Rat).Add(big.NewRat(1, 1), big.NewRat(m, 10000)))
	return q.Quo(q, r)
}

// checkCeiling asserts the ceiling property of a result: sats ≥ q > sats − 1, and ErrRange exactly when
// the ceiling exceeds MaxSats.
func checkCeiling(t testing.TB, price, rate Decimal, m, sats int64, err error) {
	t.Helper()
	checkCeilingIn(t, AssetBTC, price, rate, m, sats, err)
}

// checkCeilingIn is checkCeiling for asset: n ≥ q > n − 1 in the asset's base units, and ErrRange exactly when the
// ceiling exceeds the asset's bound.
func checkCeilingIn(t testing.TB, asset string, price, rate Decimal, m, n int64, err error) {
	t.Helper()
	q := numDenIn(t, asset, price, rate, m)
	bound := new(big.Rat).SetInt64(assetScale[asset].max)
	if err != nil {
		if !errors.Is(err, ErrRange) || q.Cmp(bound) <= 0 {
			t.Fatalf("Convert(%s, %s, %s, %d): %v, but the value is %s", asset, price, rate, m, err, q.FloatString(3))
		}
		return
	}
	s := new(big.Rat).SetInt64(n)
	if n < 1 || n > assetScale[asset].max || s.Cmp(q) < 0 || new(big.Rat).Sub(s, big.NewRat(1, 1)).Cmp(q) >= 0 {
		t.Fatalf("Convert(%s, %s, %s, %d) = %d, not the ceiling of %s", asset, price, rate, m, n, q.FloatString(3))
	}
}

// satsVectors are conversions to satoshis and their exact results.
var satsVectors = []struct {
	price, rate string
	m, want     int64
}{
	{"10", "76025", 150, 13351},
	{"100", "100000000", 0, 100},
	{"1", "3", 0, 33333334},
	{"0.01", "76000", 0, 14},
	{"0.000001", "100000000", 0, 1},
	{"0.000000000000000001", "99999999999999999999.999999999999999999", 0, 1},
	{"0.000000000000000001", "99999999999999999999.999999999999999999", -5000, 1},
	{"100", "100000000", -5000, 50},
	{"100", "100000000", 5000, 150},
	{"21000000", "1", 0, MaxSats},
	{"14000000", "1", 5000, MaxSats},
	{"42000000", "1", -5000, MaxSats},
}

// satsRefusals are conversions to satoshis and the error each must give.
var satsRefusals = []struct {
	price, rate string
	m           int64
	want        error
}{
	{"10", "76025", 5001, ErrMargin},
	{"10", "76025", -5001, ErrMargin},
	{"10", "76025", math.MaxInt64, ErrMargin},
	{"10", "76025", math.MinInt64, ErrMargin},
	{"10", "0", 0, ErrZero},
	{"0", "76025", 0, ErrZero},
	{"0", "0.000", 0, ErrZero},
	{"0", "76025", 5001, ErrZero}, // zero is checked before the margin
	{"21000000.00000001", "1", 0, ErrRange},
	{"14000000.00000001", "1", 5000, ErrRange},
	{"99999999999999999999.999999999999999999", "0.000000000000000001", 5000, ErrRange},
}

func TestSatsVectors(t *testing.T) {
	for _, v := range satsVectors {
		got, err := Sats(dec(t, v.price), dec(t, v.rate), v.m)
		if err != nil || got != v.want {
			t.Errorf("Sats(%s, %s, %d) = %d, %v; want %d", v.price, v.rate, v.m, got, err, v.want)
		}
		checkCeiling(t, dec(t, v.price), dec(t, v.rate), v.m, got, err)
	}
	for _, v := range satsRefusals {
		got, err := Sats(dec(t, v.price), dec(t, v.rate), v.m)
		if !errors.Is(err, v.want) || got != 0 {
			t.Errorf("Sats(%s, %s, %d) = %d, %v; want %v", v.price, v.rate, v.m, got, err, v.want)
		}
	}
	if _, err := Sats(Decimal{}, dec(t, "1"), 0); !errors.Is(err, ErrZero) {
		t.Errorf("the zero Decimal: %v", err)
	}
}

// The boundary units: the smallest, one, the largest ParseDecimal accepts, and one past it.
func boundaryUnits() []*big.Int {
	maxUnits := new(big.Int).Sub(pow10(MaxIntDigits+Scale), big.NewInt(1))
	return []*big.Int{big.NewInt(1), big.NewInt(2), pow10(Scale), new(big.Int).Sub(pow10(Scale), big.NewInt(1)),
		new(big.Int).Add(pow10(Scale), big.NewInt(1)), pow10(Scale + 8), maxUnits, pow10(MaxIntDigits + Scale),
		new(big.Int).Mul(big.NewInt(MaxSats), pow10(10))}
}

var boundaryMargins = []int64{MinMarginBps, MinMarginBps + 1, -1, 0, 1, MaxMarginBps - 1, MaxMarginBps}

func TestSatsBoundaries(t *testing.T) {
	for _, p := range boundaryUnits() {
		for _, r := range boundaryUnits() {
			for _, m := range boundaryMargins {
				price, rate := units(p), units(r)
				sats, err := Sats(price, rate, m)
				if err == nil && sats < 1 {
					t.Fatalf("Sats(%s, %s, %d) = %d", price, rate, m, sats)
				}
				checkCeiling(t, price, rate, m, sats, err)
				if price.Units().Cmp(p) != 0 || rate.Units().Cmp(r) != 0 {
					t.Fatal("Sats changed its arguments")
				}
			}
		}
	}
	// The largest price over the smallest rate overflows int64 many times over: a refusal, never a wrap.
	maxDec := units(boundaryUnits()[6])
	if _, err := Sats(maxDec, units(big.NewInt(1)), MaxMarginBps); !errors.Is(err, ErrRange) {
		t.Errorf("the largest ratio: %v", err)
	}
	// The smallest price over the largest rate is still one sat.
	if s, err := Sats(units(big.NewInt(1)), maxDec, MinMarginBps); err != nil || s != 1 {
		t.Errorf("the smallest ratio: %d, %v", s, err)
	}
}

func drawUnits(rng *rand.Rand) *big.Int {
	b := boundaryUnits()
	switch rng.Intn(4) {
	case 0:
		return b[rng.Intn(len(b))]
	case 1: // up to 38 digits
		return new(big.Int).Rand(rng, pow10(MaxIntDigits+Scale))
	case 2: // a realistic price or rate: up to 10 integer digits, up to 8 fraction digits
		u := new(big.Int).Rand(rng, pow10(18))
		return u.Mul(u, pow10(10))
	default: // a boundary nudged by a small amount
		u := new(big.Int).Add(b[rng.Intn(len(b))], big.NewInt(rng.Int63n(2001)-1000))
		if u.Sign() < 0 {
			u.SetInt64(0)
		}
		return u
	}
}

func drawMargin(rng *rand.Rand) int64 {
	if rng.Intn(2) == 0 {
		return boundaryMargins[rng.Intn(len(boundaryMargins))]
	}
	return rng.Int63n(MaxMarginBps-MinMarginBps+1) + MinMarginBps
}

// satsOrMax maps ErrRange to one past MaxSats, so monotonicity can be compared across the refusal.
func satsOrMax(t *testing.T, price, rate Decimal, m int64) int64 {
	t.Helper()
	s, err := Sats(price, rate, m)
	switch {
	case err == nil:
	case errors.Is(err, ErrRange):
		s = MaxSats + 1
	default:
		t.Fatalf("Sats(%s, %s, %d): unexpected %v", price, rate, m, err)
	}
	checkCeiling(t, price, rate, m, func() int64 {
		if err != nil {
			return 0
		}
		return s
	}(), err)
	return s
}

func TestSatsProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261106))
	for i := 0; i < 20000; i++ {
		p1, p2 := drawUnits(rng), drawUnits(rng)
		if p1.Cmp(p2) > 0 {
			p1, p2 = p2, p1
		}
		r1, r2 := drawUnits(rng), drawUnits(rng)
		if r1.Cmp(r2) > 0 {
			r1, r2 = r2, r1
		}
		m1, m2 := drawMargin(rng), drawMargin(rng)
		if m1 > m2 {
			m1, m2 = m2, m1
		}
		if p1.Sign() == 0 || r1.Sign() == 0 {
			if _, err := Sats(units(p1), units(r1), m1); !errors.Is(err, ErrZero) {
				t.Fatalf("a zero price or rate: %v", err)
			}
			continue
		}
		base := satsOrMax(t, units(p1), units(r1), m1)
		if s := satsOrMax(t, units(p2), units(r1), m1); s < base {
			t.Fatalf("decreasing in price: %s→%d, %s→%d", p1, base, p2, s)
		}
		if s := satsOrMax(t, units(p1), units(r2), m1); s > base {
			t.Fatalf("increasing in rate: %s→%d, %s→%d", r1, base, r2, s)
		}
		if s := satsOrMax(t, units(p1), units(r1), m2); s < base {
			t.Fatalf("decreasing in margin: %d→%d, %d→%d", m1, base, m2, s)
		}
		// A margin outside the bounds is refused after the zero check, before any arithmetic.
		bad := []int64{MaxMarginBps + 1, MinMarginBps - 1, math.MaxInt64, math.MinInt64, MaxMarginBps + 1 + rng.Int63n(math.MaxInt32)}
		if _, err := Sats(units(p1), units(r1), bad[rng.Intn(len(bad))]); !errors.Is(err, ErrMargin) {
			t.Fatalf("an out-of-range margin: %v", err)
		}
	}
}

func FuzzSats(f *testing.F) {
	f.Add("10", "76025", int64(150))
	f.Add("100", "100000000", int64(0))
	f.Add("1", "3", int64(0))
	f.Add("0.01", "76000", int64(0))
	f.Add("0.000001", "100000000", int64(0))
	f.Add("21000000", "1", int64(0))
	f.Add("21000000.00000001", "1", int64(0))
	f.Add("0", "1", int64(0))
	f.Add("1", "0", int64(0))
	f.Add("1", "1", int64(5001))
	f.Add("1", "1", int64(math.MinInt64))
	f.Add(strings.Repeat("9", 20)+"."+strings.Repeat("9", 18), "0.000000000000000001", int64(5000))
	f.Add("0.000000000000000001", strings.Repeat("9", 20)+"."+strings.Repeat("9", 18), int64(-5000))
	f.Fuzz(func(t *testing.T, ps, rs string, m int64) {
		price, err1 := ParseDecimal(ps)
		rate, err2 := ParseDecimal(rs)
		if err1 != nil || err2 != nil {
			return
		}
		sats, err := Sats(price, rate, m)
		switch {
		case price.IsZero() || rate.IsZero():
			if !errors.Is(err, ErrZero) {
				t.Fatalf("zero: %v", err)
			}
		case m < MinMarginBps || m > MaxMarginBps:
			if !errors.Is(err, ErrMargin) {
				t.Fatalf("margin %d: %v", m, err)
			}
		default:
			checkCeiling(t, price, rate, m, sats, err)
		}
	})
}

func TestValidCurrency(t *testing.T) {
	for _, s := range []string{"USD", "EUR", "JPY", "AAA", "ZZZ", "BTD", "XBT"} {
		if !ValidCurrency(s) {
			t.Errorf("ValidCurrency(%q) = false", s)
		}
	}
	for _, s := range []string{"", "usd", "Usd", "US", "USDT", "BTC", "XMR", "US1", "U D", " USD", "USD ", "US-",
		string([]byte{0xc3, 0x89}) + "UR",                                                   // ÉUR, four bytes
		string([]byte{0xc9}) + "UR",                                                         // a high byte, three bytes
		"US" + string([]byte{0x00}), "@AB", "AB[", string([]byte{0xef, 0xbc, 0xa1}) + "B"} { // full-width A
		if ValidCurrency(s) {
			t.Errorf("ValidCurrency(% x) = true", s)
		}
	}
	// Every byte in every position against the rule.
	for i := 0; i < 3; i++ {
		for c := 0; c < 256; c++ {
			b := []byte("EUR")
			b[i] = byte(c)
			want := c >= 'A' && c <= 'Z'
			if got := ValidCurrency(string(b)); got != want {
				t.Errorf("ValidCurrency(% x) = %v, want %v", b, got, want)
			}
		}
	}
	if MaxSats != 21_000_000*100_000_000 || MaxPiconero != 1<<53-1 || MinMarginBps != -5000 || MaxMarginBps != 5000 {
		t.Error("the constants moved")
	}
}

// TestConvertVectors: piconero at its vectors and bounds; every satoshi vector through Convert(AssetBTC).
func TestConvertVectors(t *testing.T) {
	for _, v := range []struct {
		price, rate string
		m, want     int64
	}{
		{"1", "250", 0, 4_000_000_000},                   // 10^12 / 250
		{"0.000001", "100000000", 0, 1},                  // 10^-2 piconero: never 0
		{"9007.199254740991", "1", 0, MaxPiconero},       // 2^53 − 1: the bound itself
		{"1", "3", 0, 333_333_333_334},                   // the ceiling
		{"100", "150.25", 150, 675_540_765_392},          // 675540765391.01…, with a margin
		{"6004.79950316066", "1", 5000, MaxPiconero - 1}, // +50 %: exactly 2^53 − 2
	} {
		got, err := Piconero(dec(t, v.price), dec(t, v.rate), v.m)
		checkCeilingIn(t, AssetXMR, dec(t, v.price), dec(t, v.rate), v.m, got, err)
		if err != nil || got != v.want {
			t.Errorf("Piconero(%s, %s, %d) = %d, %v; want %d", v.price, v.rate, v.m, got, err, v.want)
		}
		if c, err2 := Convert(AssetXMR, dec(t, v.price), dec(t, v.rate), v.m); c != got || !errors.Is(err2, err) {
			t.Errorf("Convert(XMR, %s, %s, %d) = %d, %v; Piconero gives %d, %v", v.price, v.rate, v.m, c, err2, got, err)
		}
	}
	for _, v := range []struct {
		price, rate string
		m           int64
		want        error
	}{
		{"9007.199254740992", "1", 0, ErrRange},    // 2^53
		{"6004.799503160661", "1", 5000, ErrRange}, // +50 %: 2^53 − 0.5, whose ceiling is 2^53
		{"6004.799503160661", "1", 5001, ErrMargin},
		{"0", "150", 0, ErrZero},
		{"1", "0", 0, ErrZero},
		{"99999999999999999999.999999999999999999", "0.000000000000000001", 5000, ErrRange},
	} {
		got, err := Piconero(dec(t, v.price), dec(t, v.rate), v.m)
		if !errors.Is(err, v.want) || got != 0 {
			t.Errorf("Piconero(%s, %s, %d) = %d, %v; want %v", v.price, v.rate, v.m, got, err, v.want)
		}
	}
	// The satoshi vectors through Convert(AssetBTC) are unchanged.
	for _, v := range satsVectors {
		got, err := Convert(AssetBTC, dec(t, v.price), dec(t, v.rate), v.m)
		if err != nil || got != v.want {
			t.Errorf("Convert(BTC, %s, %s, %d) = %d, %v; want %d", v.price, v.rate, v.m, got, err, v.want)
		}
	}
	for _, v := range satsRefusals {
		got, err := Convert(AssetBTC, dec(t, v.price), dec(t, v.rate), v.m)
		if !errors.Is(err, v.want) || got != 0 {
			t.Errorf("Convert(BTC, %s, %s, %d) = %d, %v; want %v", v.price, v.rate, v.m, got, err, v.want)
		}
	}
	// Any other asset is ErrAsset, checked first: before the zero and the margin.
	for _, asset := range []string{"ETH", "", "btc", "xmr", "XBT", "BTC ", "XMR\x00" /* XMR then a NUL byte */} {
		for _, args := range [][2]string{{"1", "1"}, {"0", "0"}} {
			if got, err := Convert(asset, dec(t, args[0]), dec(t, args[1]), MaxMarginBps+1); !errors.Is(err, ErrAsset) || got != 0 {
				t.Errorf("Convert(%q) = %d, %v; want ErrAsset", asset, got, err)
			}
		}
	}
	if ErrRange.Error() != "rates: amount above the asset's bound" {
		t.Errorf("ErrRange %q", ErrRange)
	}
}

// convertBoundaryUnits are boundaryUnits plus the units of a price that converts at a rate of 1 to MaxPiconero, and
// one unit either side.
func convertBoundaryUnits() []*big.Int {
	atMax := new(big.Int).Mul(big.NewInt(MaxPiconero), pow10(Scale-12))
	return append(boundaryUnits(), atMax, new(big.Int).Sub(atMax, big.NewInt(1)), new(big.Int).Add(atMax, big.NewInt(1)))
}

// convertOrMax maps ErrRange to one past the asset's bound, so monotonicity can be compared across the refusal; it
// checks the ceiling property, and that the asset's own function (Sats, Piconero) gives the same.
func convertOrMax(t *testing.T, asset string, price, rate Decimal, m int64) int64 {
	t.Helper()
	n, err := Convert(asset, price, rate, m)
	own := Sats
	if asset == AssetXMR {
		own = Piconero
	}
	if n2, err2 := own(price, rate, m); n2 != n || !errors.Is(err2, err) {
		t.Fatalf("Convert(%s, %s, %s, %d) = %d, %v; its own function %d, %v", asset, price, rate, m, n, err, n2, err2)
	}
	switch {
	case err == nil:
		checkCeilingIn(t, asset, price, rate, m, n, nil)
	case errors.Is(err, ErrRange):
		checkCeilingIn(t, asset, price, rate, m, 0, err)
		n = assetScale[asset].max + 1
	default:
		t.Fatalf("Convert(%s, %s, %s, %d): unexpected %v", asset, price, rate, m, err)
	}
	return n
}

// TestConvertProperty: over seeded draws of both assets, prices, rates and margins at every boundary, the result is the
// ceiling of the exact value in the asset's base units, ErrRange exactly when that exceeds the asset's bound, and it
// never decreases in the price or the margin nor increases in the rate.
func TestConvertProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261107))
	b := convertBoundaryUnits()
	draw := func() *big.Int {
		if rng.Intn(3) == 0 {
			return b[rng.Intn(len(b))]
		}
		return drawUnits(rng)
	}
	assets := []string{AssetBTC, AssetXMR}
	for i := 0; i < 20000; i++ {
		asset := assets[rng.Intn(2)]
		p1, p2 := draw(), draw()
		if p1.Cmp(p2) > 0 {
			p1, p2 = p2, p1
		}
		r1, r2 := draw(), draw()
		if r1.Cmp(r2) > 0 {
			r1, r2 = r2, r1
		}
		m1, m2 := drawMargin(rng), drawMargin(rng)
		if m1 > m2 {
			m1, m2 = m2, m1
		}
		if p1.Sign() == 0 || r1.Sign() == 0 {
			if _, err := Convert(asset, units(p1), units(r1), m1); !errors.Is(err, ErrZero) {
				t.Fatalf("a zero price or rate: %v", err)
			}
			continue
		}
		base := convertOrMax(t, asset, units(p1), units(r1), m1)
		if s := convertOrMax(t, asset, units(p2), units(r1), m1); s < base {
			t.Fatalf("%s decreasing in price: %s→%d, %s→%d", asset, p1, base, p2, s)
		}
		if s := convertOrMax(t, asset, units(p1), units(r2), m1); s > base {
			t.Fatalf("%s increasing in rate: %s→%d, %s→%d", asset, r1, base, r2, s)
		}
		if s := convertOrMax(t, asset, units(p1), units(r1), m2); s < base {
			t.Fatalf("%s decreasing in margin: %d→%d, %d→%d", asset, m1, base, m2, s)
		}
		bad := []int64{MaxMarginBps + 1, MinMarginBps - 1, math.MaxInt64, math.MinInt64}
		if _, err := Convert(asset, units(p1), units(r1), bad[rng.Intn(len(bad))]); !errors.Is(err, ErrMargin) {
			t.Fatalf("an out-of-range margin: %v", err)
		}
	}
}

func FuzzConvert(f *testing.F) {
	for _, asset := range []string{AssetBTC, AssetXMR, "ETH", "", "xmr"} {
		f.Add(asset, "1", "250", int64(0))
		f.Add(asset, "9007.199254740991", "1", int64(0))
		f.Add(asset, "9007.199254740992", "1", int64(0))
		f.Add(asset, "21000000.00000001", "1", int64(0))
		f.Add(asset, "0.000001", "100000000", int64(0))
		f.Add(asset, "0", "1", int64(0))
		f.Add(asset, "1", "1", int64(5001))
		f.Add(asset, strings.Repeat("9", 20)+"."+strings.Repeat("9", 18), "0.000000000000000001", int64(5000))
	}
	f.Fuzz(func(t *testing.T, asset, ps, rs string, m int64) {
		price, err1 := ParseDecimal(ps)
		rate, err2 := ParseDecimal(rs)
		if err1 != nil || err2 != nil {
			return
		}
		n, err := Convert(asset, price, rate, m)
		switch {
		case asset != AssetBTC && asset != AssetXMR:
			if !errors.Is(err, ErrAsset) || n != 0 {
				t.Fatalf("asset %q: %d, %v", asset, n, err)
			}
		case price.IsZero() || rate.IsZero():
			if !errors.Is(err, ErrZero) {
				t.Fatalf("zero: %v", err)
			}
		case m < MinMarginBps || m > MaxMarginBps:
			if !errors.Is(err, ErrMargin) {
				t.Fatalf("margin %d: %v", m, err)
			}
		default:
			checkCeilingIn(t, asset, price, rate, m, n, err)
		}
	})
}
