package nostr

import (
	"encoding/hex"
	"errors"
	"math/big"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
)

var (
	origOnce sync.Once
	origEv   Event
)

// original returns one correctly signed event, signed once so fuzz inputs can be compared with it.
func original(t testing.TB) Event {
	origOnce.Do(func() {
		k, err := ParseSecretKeyHex(testKeyA)
		if err != nil {
			t.Fatal(err)
		}
		origEv = Event{CreatedAt: 1700000000, Kind: 23194, Tags: [][]string{{"p", testKeyB}}, Content: "hello"}
		// Fixed aux randomness: the fuzz coordinator and its workers are separate processes and must agree.
		origEv.PubKey = k.PublicKeyHex()
		id, err := ComputeID(&origEv)
		if err != nil {
			t.Fatal(err)
		}
		origEv.ID = id
		var raw [32]byte
		if _, err := hex.Decode(raw[:], []byte(id)); err != nil {
			t.Fatal(err)
		}
		aux := [32]byte{1}
		if origEv.Sig, err = signID(k, raw, &aux); err != nil {
			t.Fatal(err)
		}
	})
	e := origEv
	e.Tags = [][]string{append([]string(nil), origEv.Tags[0]...)}
	return e
}

// checkVerify builds an event from the fields (optionally recomputing the id) and asserts that Verify
// never panics and accepts only an event identical to the correctly signed original.
func checkVerify(t testing.TB, id, pub, sig, content string, createdAt int64, recompute bool) {
	t.Helper()
	o := original(t)
	e := Event{ID: id, PubKey: pub, CreatedAt: createdAt, Kind: o.Kind, Tags: o.Tags, Content: content, Sig: sig}
	if recompute {
		if h, err := ComputeID(&e); err == nil {
			e.ID = h
		}
	}
	err := Verify(&e)
	same := e.ID == o.ID && e.PubKey == o.PubKey && e.Sig == o.Sig && e.Content == o.Content && e.CreatedAt == o.CreatedAt
	if (err == nil) != same {
		t.Fatalf("Verify err=%v but identical-to-original=%v for %+v", err, same, e)
	}
}

func FuzzVerify(f *testing.F) {
	o := original(f)
	f.Add(o.ID, o.PubKey, o.Sig, o.Content, o.CreatedAt, false)
	f.Add(o.ID, o.PubKey, o.Sig, o.Content, o.CreatedAt, true)
	for _, n := range []int{0, 63, 64, 65} {
		f.Add(strings.Repeat("a", n), o.PubKey, o.Sig, o.Content, o.CreatedAt, false)
		f.Add(o.ID, strings.Repeat("a", n), o.Sig, o.Content, o.CreatedAt, true)
	}
	for _, n := range []int{0, 127, 128, 129} {
		f.Add(o.ID, o.PubKey, strings.Repeat("a", n), o.Content, o.CreatedAt, false)
	}
	f.Add(strings.ToUpper(o.ID), strings.ToUpper(o.PubKey), strings.ToUpper(o.Sig), o.Content, o.CreatedAt, false)
	f.Add(o.ID, strings.Repeat("g", 64), o.Sig, o.Content, o.CreatedAt, true)
	f.Add(o.ID, "eefdea4cdb677750a420fee807eacf21eb9898ae79b9768766e4faa04a2d4a34", o.Sig, o.Content, o.CreatedAt, true)
	f.Add(o.ID, "fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc30", o.Sig, o.Content, o.CreatedAt, true)
	f.Add(o.ID, strings.Repeat("0", 64), strings.Repeat("0", 128), o.Content, o.CreatedAt, true)
	f.Add(o.ID, o.PubKey, o.Sig, "hello!", o.CreatedAt, true)
	f.Add(o.ID, o.PubKey, o.Sig, "café  \x00", o.CreatedAt, true)
	f.Add(o.ID, o.PubKey, o.Sig, o.Content, o.CreatedAt+1, true)
	f.Fuzz(func(t *testing.T, id, pub, sig, content string, createdAt int64, recompute bool) {
		checkVerify(t, id, pub, sig, content, createdAt, recompute)
	})
}

func TestVerifyProperty(t *testing.T) {
	o := original(t)
	r := rand.New(rand.NewPCG(7, 8))
	const hexd = "0123456789abcdef"
	mutHex := func(s string, n int) string {
		b := []byte(s)
		if len(b) > 0 {
			b[r.IntN(len(b))] = hexd[r.IntN(16)]
		}
		for len(b) < n {
			b = append(b, hexd[r.IntN(16)])
		}
		return string(b[:n])
	}
	for i := 0; i < 3000; i++ {
		e := o
		e.Tags = [][]string{append([]string(nil), o.Tags[0]...)}
		field := r.IntN(6)
		switch field {
		case 0:
			e.ID = mutHex(e.ID, 64)
		case 1:
			e.PubKey = mutHex(e.PubKey, 64)
		case 2:
			e.Sig = mutHex(e.Sig, 128)
		case 3:
			e.Content += string(rune(0x20 + r.IntN(0x5f)))
		case 4:
			e.CreatedAt += 1 + int64(r.IntN(1000))
		default:
			e.Tags[0][1] += string(rune(0x20 + r.IntN(0x5f)))
		}
		unchanged := e.ID == o.ID && e.PubKey == o.PubKey && e.Sig == o.Sig // a hex mutation may pick the same digit
		if field <= 2 && unchanged {
			continue
		}
		if err := Verify(&e); err == nil {
			t.Fatalf("mutation of field %d verified", field)
		} else if field != 2 && !errors.Is(err, ErrBadID) {
			t.Fatalf("field %d without a recomputed id: %v, want ErrBadID", field, err)
		}
		if field == 0 {
			continue
		}
		// Recompute the id: now only the signature (or the key) can reject it.
		resign(t, &e)
		if err := Verify(&e); err == nil {
			t.Fatalf("recomputed mutation of field %d verified", field)
		} else if !errors.Is(err, ErrBadSig) && !errors.Is(err, ErrBadPubKey) {
			t.Fatalf("recomputed field %d: %v, want ErrBadSig or ErrBadPubKey", field, err)
		}
	}
}

// ---- public-key parser / SharedX ----

var curveP, _ = new(big.Int).SetString("fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f", 16)

// onCurveX reports whether x < p and x^3+7 is a square mod p (so some y exists).
func onCurveX(x *big.Int) (y *big.Int, ok bool) {
	if x.Cmp(curveP) >= 0 {
		return nil, false
	}
	y2 := new(big.Int).Exp(x, big.NewInt(3), curveP)
	y2.Add(y2, big.NewInt(7)).Mod(y2, curveP)
	e := new(big.Int).Add(curveP, big.NewInt(1))
	e.Rsh(e, 2) // p = 3 mod 4
	y = new(big.Int).Exp(y2, e, curveP)
	if new(big.Int).Mod(new(big.Int).Mul(y, y), curveP).Cmp(y2) != 0 {
		return nil, false
	}
	if y.Bit(0) == 1 { // lift to even y
		y.Sub(curveP, y)
	}
	return y, true
}

type pt struct{ x, y *big.Int } // nil x means infinity

func add(a, b pt) pt {
	if a.x == nil {
		return b
	}
	if b.x == nil {
		return a
	}
	var l *big.Int
	if a.x.Cmp(b.x) == 0 {
		if new(big.Int).Add(a.y, b.y).Mod(new(big.Int).Add(a.y, b.y), curveP).Sign() == 0 {
			return pt{}
		}
		num := new(big.Int).Mul(a.x, a.x)
		num.Mul(num, big.NewInt(3))
		l = num.Mul(num, new(big.Int).ModInverse(new(big.Int).Lsh(a.y, 1), curveP))
	} else {
		num := new(big.Int).Sub(b.y, a.y)
		den := new(big.Int).Sub(b.x, a.x)
		l = num.Mul(num, den.ModInverse(den.Mod(den, curveP), curveP))
	}
	l.Mod(l, curveP)
	x := new(big.Int).Mul(l, l)
	x.Sub(x, a.x).Sub(x, b.x).Mod(x, curveP)
	y := new(big.Int).Sub(a.x, x)
	y.Mul(y, l).Sub(y, a.y).Mod(y, curveP)
	return pt{x, y}
}

// refSharedX is an independent big.Int double-and-add computation of x(k*P) for the even-y lift of x.
func refSharedX(kHex string, px, py *big.Int) [32]byte {
	k, _ := new(big.Int).SetString(kHex, 16)
	acc, base := pt{}, pt{px, py}
	for i := 0; i < k.BitLen(); i++ {
		if k.Bit(i) == 1 {
			acc = add(acc, base)
		}
		base = add(base, base)
	}
	var out [32]byte
	acc.x.FillBytes(out[:])
	return out
}

func checkPub(t testing.TB, pubHex string) {
	t.Helper()
	a := mustKey(t, testKeyA)
	got, err := a.SharedX(pubHex)
	var wantOK bool
	var y *big.Int
	if lowerHex(pubHex, 64) {
		x, _ := new(big.Int).SetString(pubHex, 16)
		y, wantOK = onCurveX(x)
		if wantOK {
			if want := refSharedX(testKeyA, x, y); err == nil && got != want {
				t.Fatalf("SharedX(%s) = %x, reference %x", pubHex, got, want)
			}
		}
	}
	if (err == nil) != wantOK {
		t.Fatalf("SharedX(%q): err=%v, want ok=%v", pubHex, err, wantOK)
	}
	if err != nil && !errors.Is(err, ErrBadPubKey) {
		t.Fatalf("SharedX(%q): wrong error %v", pubHex, err)
	}
}

func FuzzParsePub(f *testing.F) {
	b := mustKey(f, testKeyB)
	for _, s := range []string{
		"", b.PublicKeyHex(), b.PublicKeyHex()[:63], b.PublicKeyHex() + "0", strings.ToUpper(b.PublicKeyHex()),
		strings.Repeat("0", 64), strings.Repeat("f", 64), "eefdea4cdb677750a420fee807eacf21eb9898ae79b9768766e4faa04a2d4a34",
		"fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f", "fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc30",
		"79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798", strings.Repeat("g", 64), " ", " ",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { checkPub(t, s) })
}

func TestParsePubProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	const hexd = "0123456789abcdefABCDEFg"
	valid := 0
	for i := 0; i < 600; i++ {
		var s string
		switch i % 3 {
		case 0: // random 64-digit lowercase: about half are on the curve
			b := make([]byte, 64)
			for j := range b {
				b[j] = hexd[r.IntN(16)]
			}
			s = string(b)
		case 1: // near p and near the boundary lengths / alphabets
			base := "fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f"
			b := []byte(base)
			b[63-r.IntN(4)] = hexd[r.IntN(16)]
			s = string(b[:64-r.IntN(2)])
		default:
			b := make([]byte, r.IntN(70))
			for j := range b {
				b[j] = hexd[r.IntN(len(hexd))]
			}
			s = string(b)
		}
		checkPub(t, s)
		if _, err := mustKey(t, testKeyA).SharedX(s); err == nil {
			valid++
		}
	}
	if valid < 100 {
		t.Errorf("only %d valid points exercised", valid)
	}
	for c := 0; c < 256; c++ {
		checkPub(t, strings.Repeat("a", 63)+string([]byte{byte(c)}))
	}
}
