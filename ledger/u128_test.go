package ledger

import (
	"bytes"
	"math/big"
	"math/rand/v2"
	"strings"
	"testing"
)

var (
	bigOne  = big.NewInt(1)
	bigMax  = new(big.Int).Sub(new(big.Int).Lsh(bigOne, 128), bigOne)
	bigMod  = new(big.Int).Lsh(bigOne, 128)
	bigZero = new(big.Int)
)

func toBig(a U128) *big.Int {
	b := new(big.Int).SetUint64(a.Hi)
	b.Lsh(b, 64)
	return b.Or(b, new(big.Int).SetUint64(a.Lo))
}

func fromBig(t testing.TB, b *big.Int) U128 {
	t.Helper()
	if b.Sign() < 0 || b.Cmp(bigMax) > 0 {
		t.Fatalf("fromBig: %s out of range", b)
	}
	lo := new(big.Int).And(b, new(big.Int).SetUint64(^uint64(0)))
	hi := new(big.Int).Rsh(b, 64)
	return U128{hi.Uint64(), lo.Uint64()}
}

func pow2(n uint) *big.Int { return new(big.Int).Lsh(bigOne, n) }

func boundarySet() []*big.Int {
	sub1 := func(n uint) *big.Int { return new(big.Int).Sub(pow2(n), bigOne) }
	pow10 := func(n int64) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil) }
	// 10^19 and 10^38 are the edges of String's decimal chunks.
	return []*big.Int{
		big.NewInt(0), big.NewInt(1), sub1(63), pow2(63), sub1(64), pow2(64),
		pow2(127), new(big.Int).Sub(bigMax, bigOne), bigMax,
		new(big.Int).Sub(pow10(19), bigOne), pow10(19), new(big.Int).Add(pow10(19), bigOne),
		new(big.Int).Sub(pow10(38), bigOne), pow10(38), new(big.Int).Add(pow10(38), bigOne),
	}
}

func TestU128StringChunkEdges(t *testing.T) {
	for _, c := range []struct {
		hi, lo uint64
		want   string
	}{
		{0, 0, "0"},
		{0, 9999999999999999999, "9999999999999999999"},
		{0, 10000000000000000000, "10000000000000000000"},
		{0, 10000000000000000001, "10000000000000000001"},
	} {
		if got := (U128{c.hi, c.lo}).String(); got != c.want {
			t.Errorf("U128{%d,%d}.String() = %q, want %q", c.hi, c.lo, got, c.want)
		}
	}
	for _, s := range []string{
		"99999999999999999999999999999999999999",
		"100000000000000000000000000000000000000",
		"100000000000000000000000000000000000001",
	} {
		v, err := ParseU128(s)
		if err != nil || v.String() != s {
			t.Errorf("ParseU128(%q) round trip = %v, %v", s, v, err)
		}
	}
}

func randU128(r *rand.Rand) U128 {
	// Mix full-width and narrow values so carries and borrows are exercised.
	v := U128{r.Uint64(), r.Uint64()}
	switch r.IntN(4) {
	case 0:
		v.Hi = 0
	case 1:
		v.Lo >>= r.UintN(64)
		v.Hi >>= r.UintN(64)
	}
	return v
}

func checkPair(t *testing.T, a, b U128) {
	t.Helper()
	ba, bb := toBig(a), toBig(b)

	sum := new(big.Int).Add(ba, bb)
	got, ok := a.Add(b)
	wantOK := sum.Cmp(bigMax) <= 0
	if ok != wantOK {
		t.Fatalf("%s+%s: ok=%v want %v", a, b, ok, wantOK)
	}
	if want := new(big.Int).Mod(sum, bigMod); toBig(got).Cmp(want) != 0 {
		t.Fatalf("%s+%s = %s, want wrapped %s", a, b, got, want)
	}

	diff := new(big.Int).Sub(ba, bb)
	got, ok = a.Sub(b)
	wantOK = diff.Sign() >= 0
	if ok != wantOK {
		t.Fatalf("%s-%s: ok=%v want %v", a, b, ok, wantOK)
	}
	if want := new(big.Int).Mod(diff, bigMod); toBig(got).Cmp(want) != 0 {
		t.Fatalf("%s-%s = %s, want wrapped %s", a, b, got, want)
	}

	if c := a.Cmp(b); c != ba.Cmp(bb) {
		t.Fatalf("Cmp(%s,%s) = %d, want %d", a, b, c, ba.Cmp(bb))
	}
	if a.String() != ba.String() {
		t.Fatalf("String = %q, want %q", a.String(), ba.String())
	}
	if a.IsZero() != (ba.Sign() == 0) || a.IsMax() != (ba.Cmp(bigMax) == 0) {
		t.Fatalf("IsZero/IsMax wrong for %s", a)
	}
}

func TestU128AgainstBig(t *testing.T) {
	bs := boundarySet()
	for _, x := range bs {
		for _, y := range bs {
			checkPair(t, fromBig(t, x), fromBig(t, y))
		}
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 100000 {
		checkPair(t, randU128(r), randU128(r))
	}
}

func TestU128Constants(t *testing.T) {
	if toBig(U128Zero).Cmp(bigZero) != 0 || toBig(U128Max).Cmp(bigMax) != 0 {
		t.Fatal("U128Zero/U128Max wrong")
	}
	if v := U128From64(^uint64(0)); v.Hi != 0 || v.Lo != ^uint64(0) {
		t.Fatalf("U128From64 = %v", v)
	}
	if _, ok := U128Max.Add(U128From64(1)); ok {
		t.Fatal("max+1 must overflow")
	}
	if _, ok := U128Zero.Sub(U128From64(1)); ok {
		t.Fatal("0-1 must underflow")
	}
}

func TestU128BytesRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for range 10000 {
		a := randU128(r)
		b := a.Bytes()
		want := toBig(a).FillBytes(make([]byte, 16))
		if !bytes.Equal(b[:], want) {
			t.Fatalf("Bytes(%s) = %x, want %x", a, b, want)
		}
		back, err := U128FromBytes(b[:])
		if err != nil || back != a {
			t.Fatalf("round trip %s: %v %v", a, back, err)
		}
	}
	for n := 0; n < 40; n++ {
		if n == 16 {
			continue
		}
		if _, err := U128FromBytes(make([]byte, n)); err == nil {
			t.Fatalf("U128FromBytes accepted %d bytes", n)
		}
	}
	if _, err := U128FromBytes(nil); err == nil {
		t.Fatal("nil accepted")
	}
}

func TestU128BytesOrderIsNumericOrder(t *testing.T) {
	check := func(a, b U128) {
		ab, bb := a.Bytes(), b.Bytes()
		if got := bytes.Compare(ab[:], bb[:]); got != a.Cmp(b) {
			t.Fatalf("bytes.Compare(%s,%s) = %d, Cmp = %d", a, b, got, a.Cmp(b))
		}
	}
	bs := boundarySet()
	for _, x := range bs {
		for _, y := range bs {
			check(fromBig(t, x), fromBig(t, y))
		}
	}
	r := rand.New(rand.NewPCG(5, 6))
	for range 100000 {
		check(randU128(r), randU128(r))
	}
}

func TestParseU128Boundaries(t *testing.T) {
	for _, x := range boundarySet() {
		v, err := ParseU128(x.String())
		if err != nil || toBig(v).Cmp(x) != 0 {
			t.Fatalf("ParseU128(%s) = %v, %v", x, v, err)
		}
	}
	if len(bigMax.String()) != 39 {
		t.Fatal("test assumption: max has 39 digits")
	}
}

func TestParseU128Rejects(t *testing.T) {
	over := bigMod.String()
	bad := []string{
		"", "-1", "+1", "01", "00", " 1", "1 ", "1\n", "0x10", "1_0", "1e3", "1.0", "١",
		strings.Repeat("1", 40), strings.Repeat("9", 39), over,
		new(big.Int).Add(bigMax, bigOne).String(),
		"340282366920938463463374607431768211456",
		"0" + bigMax.String(),
	}
	for _, s := range bad {
		if v, err := ParseU128(s); err == nil {
			t.Errorf("ParseU128(%q) accepted as %s", s, v)
		}
	}
}

func TestParseU128Random(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	for range 20000 {
		a := randU128(r)
		got, err := ParseU128(a.String())
		if err != nil || got != a {
			t.Fatalf("ParseU128(%s) = %v, %v", a, got, err)
		}
	}
	// Random strings of digits: accepted exactly when big agrees.
	for range 20000 {
		n := 1 + r.IntN(41)
		var sb strings.Builder
		for range n {
			sb.WriteByte(byte('0' + r.IntN(10)))
		}
		s := sb.String()
		b, _ := new(big.Int).SetString(s, 10)
		canonical := b.String() == s
		want := canonical && b.Cmp(bigMax) <= 0 && n <= 39
		v, err := ParseU128(s)
		if (err == nil) != want {
			t.Fatalf("ParseU128(%q): err=%v want accepted=%v", s, err, want)
		}
		if err == nil && v.String() != s {
			t.Fatalf("ParseU128(%q).String() = %q", s, v.String())
		}
	}
}

func FuzzParseU128(f *testing.F) {
	for _, s := range []string{"", "0", "1", "-1", "+1", "01", " 1", strings.Repeat("9", 39),
		strings.Repeat("1", 40), bigMax.String(), bigMod.String(), "18446744073709551616"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := ParseU128(s)
		if err != nil {
			return
		}
		if v.String() != s {
			t.Fatalf("accepted %q but String = %q", s, v.String())
		}
		back, err := ParseU128(v.String())
		if err != nil || back != v {
			t.Fatalf("round trip %q: %v %v", s, back, err)
		}
	})
}
