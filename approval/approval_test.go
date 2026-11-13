package approval

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/vectors.json")

const (
	vectorExpiry = 1_900_000_000
	largeAmount  = 9_007_199_254_740_000 // 2^53 - 992, the largest multiple of 1000 <= 2^53
)

var testNow = time.Unix(1_800_000_000, 0)

// The test keys are generated for these tests: the seed is the byte b repeated 32 times.
func seedKey(b byte) ed25519.PrivateKey { return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, 32)) }

func pubHex(priv ed25519.PrivateKey) string { return encodeHex(priv.Public().(ed25519.PublicKey)) }

func idOf(b byte) (id [16]byte) {
	for i := range id {
		id[i] = b + byte(i)
	}
	return
}

func testKeys(t testing.TB, privs ...ed25519.PrivateKey) *Keys {
	t.Helper()
	var hexKeys []string
	for _, p := range privs {
		hexKeys = append(hexKeys, pubHex(p))
	}
	k, err := NewKeys(hexKeys)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func validRefund(priv ed25519.PrivateKey) Refund {
	return Refund{Network: Mainnet, KeyID: KeyID(priv.Public().(ed25519.PublicKey)), RefundID: idOf(0x10),
		OrderID: idOf(0x40), AmountMsat: 5_000_000, ExpiresAt: testNow.Unix() + 3600}
}

func validVoid(priv ed25519.PrivateKey) Void {
	return Void{Network: Mainnet, KeyID: KeyID(priv.Public().(ed25519.PublicKey)), RefundID: idOf(0x10),
		ExpiresAt: testNow.Unix() + 3600}
}

type vectorKey struct {
	Seed   string `json:"seed"`
	Public string `json:"public"`
	KeyID  string `json:"key_id"`
}

type vectorFile struct {
	Keys    []vectorKey       `json:"keys"`
	Refunds map[string]string `json:"refunds"` // "key/network/amount" -> hex
	Voids   map[string]string `json:"voids"`   // "key/network" -> hex
}

var networks = []Network{Mainnet, Testnet, Signet, Regtest}

func buildVectors() vectorFile {
	f := vectorFile{Refunds: map[string]string{}, Voids: map[string]string{}}
	for i, b := range []byte{1, 2} {
		priv := seedKey(b)
		id := KeyID(priv.Public().(ed25519.PublicKey))
		f.Keys = append(f.Keys, vectorKey{Seed: encodeHex(priv.Seed()), Public: pubHex(priv), KeyID: encodeHex(id[:])})
		for _, n := range networks {
			for _, amt := range []uint64{1000, largeAmount} {
				r := Refund{Network: n, KeyID: id, RefundID: idOf(0x10), OrderID: idOf(0x40), AmountMsat: amt, ExpiresAt: vectorExpiry}
				f.Refunds[fmt.Sprintf("%d/%d/%d", i, n, amt)] = SignRefund(r, priv)
			}
			v := Void{Network: n, KeyID: id, RefundID: idOf(0x10), ExpiresAt: vectorExpiry}
			f.Voids[fmt.Sprintf("%d/%d", i, n)] = SignVoid(v, priv)
		}
	}
	return f
}

const vectorsPath = "testdata/vectors.json"

// TestVectors: Sign reproduces, byte for byte, the approvals committed in testdata/vectors.json, and
// Verify reads each back to its fields. Run with -update to write the file once.
func TestVectors(t *testing.T) {
	got := buildVectors()
	if *update {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(vectorsPath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var want vectorFile
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want.Keys) != 2 || len(want.Refunds) != 16 || len(want.Voids) != 8 {
		t.Fatalf("vector file shape: %d keys, %d refunds, %d voids", len(want.Keys), len(want.Refunds), len(want.Voids))
	}
	if fmt.Sprint(got.Keys) != fmt.Sprint(want.Keys) {
		t.Errorf("keys differ:\n got %v\nwant %v", got.Keys, want.Keys)
	}
	keys := testKeys(t, seedKey(1), seedKey(2))
	now := time.Unix(vectorExpiry-100, 0)
	for name, hexA := range want.Refunds {
		if got.Refunds[name] != hexA {
			t.Errorf("refund %s: Sign gives\n%s\nfile has\n%s", name, got.Refunds[name], hexA)
		}
		var ki, n int
		var amt uint64
		if _, err := fmt.Sscanf(name, "%d/%d/%d", &ki, &n, &amt); err != nil {
			t.Fatal(err)
		}
		r, err := keys.VerifyRefund(hexA, Network(n), now)
		if err != nil {
			t.Errorf("refund %s: %v", name, err)
			continue
		}
		wantID, _ := decodeHex(want.Keys[ki].KeyID)
		if r.AmountMsat != amt || r.ExpiresAt != vectorExpiry || r.RefundID != idOf(0x10) || r.OrderID != idOf(0x40) ||
			r.Network != Network(n) || !bytes.Equal(r.KeyID[:], wantID) {
			t.Errorf("refund %s: read back %+v", name, r)
		}
	}
	for name, hexA := range want.Voids {
		if got.Voids[name] != hexA {
			t.Errorf("void %s: Sign gives\n%s\nfile has\n%s", name, got.Voids[name], hexA)
		}
		var ki, n int
		if _, err := fmt.Sscanf(name, "%d/%d", &ki, &n); err != nil {
			t.Fatal(err)
		}
		v, err := keys.VerifyVoid(hexA, Network(n), now)
		if err != nil {
			t.Errorf("void %s: %v", name, err)
			continue
		}
		if v.RefundID != idOf(0x10) || v.ExpiresAt != vectorExpiry || v.Network != Network(n) {
			t.Errorf("void %s: read back %+v", name, v)
		}
	}
}

func TestLayout(t *testing.T) {
	p := seedKey(1)
	r := validRefund(p)
	b := marshalRefund(r)
	if len(b) != 84 || string(b[:27]) != "satbooth/refund-approval/v1" {
		t.Fatalf("refund body: %d bytes, domain %q", len(b), b[:27])
	}
	if got := len(SignRefund(r, p)); got != 296 {
		t.Errorf("refund hex length %d", got)
	}
	v := marshalVoid(validVoid(p))
	if len(v) != 56 || string(v[:23]) != "satbooth/refund-void/v1" {
		t.Fatalf("void body: %d bytes, domain %q", len(v), v[:23])
	}
	if got := len(SignVoid(validVoid(p), p)); got != 240 {
		t.Errorf("void hex length %d", got)
	}
}

func TestParseNetwork(t *testing.T) {
	for s, want := range map[string]Network{"mainnet": Mainnet, "testnet": Testnet, "signet": Signet, "regtest": Regtest} {
		if got, err := ParseNetwork(s); err != nil || got != want {
			t.Errorf("%s: %d, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "Mainnet", "main", "bitcoin", " mainnet", "mainnet\n", "testnet4", "MAINNET", "regtest "} {
		if _, err := ParseNetwork(s); !errors.Is(err, ErrNetwork) {
			t.Errorf("%q: %v", s, err)
		}
	}
}

// mixedCase upper-cases the first hex letter of s.
func mixedCase(s string) string {
	i := strings.IndexAny(s, "abcdef")
	return s[:i] + strings.ToUpper(s[i:i+1]) + s[i+1:]
}

func TestNewKeys(t *testing.T) {
	var many []string
	for i := 1; i <= 9; i++ {
		many = append(many, pubHex(seedKey(byte(i))))
	}
	for n := 1; n <= 8; n++ {
		if _, err := NewKeys(many[:n]); err != nil {
			t.Errorf("%d keys: %v", n, err)
		}
	}
	good := many[0]
	cases := map[string][]string{
		"none": nil, "empty list": {}, "nine": many[:9],
		"duplicate":   {good, many[1], good},
		"uppercase":   {strings.ToUpper(good)},
		"mixed":       {mixedCase(good)},
		"short":       {good[:62]},
		"long":        {good + "00"},
		"empty key":   {""},
		"not hex":     {good[:63] + "g"},
		"space":       {good[:63] + " "},
		"0x prefix":   {"0x" + good[2:]},
		"odd":         {good[:63]},
		"one bad two": {good, "zz"},
	}
	for name, in := range cases {
		if _, err := NewKeys(in); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestNewKeysRefusesSmallOrder: every entry of the small-order blacklist, with and without the sign bit,
// and the same keys as lowercase hex, is refused.
func TestNewKeysRefusesSmallOrder(t *testing.T) {
	if len(smallOrder) != 7 {
		t.Fatalf("blacklist has %d entries", len(smallOrder))
	}
	for i, bad := range smallOrder {
		for _, sign := range []byte{0, 0x80} {
			k := bad
			k[31] |= sign
			if _, err := NewKeys([]string{encodeHex(k[:])}); !errors.Is(err, ErrMalformed) {
				t.Errorf("entry %d sign %#x: %v", i, sign, err)
			}
			// Refused among good keys too, at any position.
			if _, err := NewKeys([]string{pubHex(seedKey(1)), encodeHex(k[:])}); !errors.Is(err, ErrMalformed) {
				t.Errorf("entry %d sign %#x after a good key: %v", i, sign, err)
			}
		}
	}
	// The literal hex of the named keys.
	for _, h := range []string{
		"0000000000000000000000000000000000000000000000000000000000000000",
		"0100000000000000000000000000000000000000000000000000000000000000",
		"0100000000000000000000000000000000000000000000000000000000000080",
		"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05",
		"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85",
		"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a",
		"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa",
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	} {
		if _, err := NewKeys([]string{h}); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", h, err)
		}
	}
	// A byte of difference anywhere else is not on the list (it is not a claim of being good, only that the
	// check is exact).
	near := smallOrder[2]
	near[0] ^= 1
	if hasSmallOrder(near[:]) {
		t.Error("near miss refused")
	}
}

// TestSmallOrderForgeryRefused: with a small-order public key configured, R = identity and S = 0 would
// verify against any message under Go's cofactorless ed25519.Verify. NewKeys refuses such a key, so the
// forgery can never reach VerifyRefund.
func TestSmallOrderForgeryRefused(t *testing.T) {
	identity := make([]byte, 32)
	identity[0] = 1
	for _, pub := range [][]byte{identity, make([]byte, 32)} {
		r := Refund{Network: Mainnet, KeyID: KeyID(pub), RefundID: idOf(0x10), OrderID: idOf(0x40),
			AmountMsat: largeAmount, ExpiresAt: testNow.Unix() + 3600}
		body := marshalRefund(r)
		sig := make([]byte, 64) // S = 0
		copy(sig, identity)     // R = identity
		forged := encodeHex(append(append([]byte{}, body...), sig...))
		if len(forged) != 296 {
			t.Fatalf("forged length %d", len(forged))
		}
		if ed25519.Verify(ed25519.PublicKey(pub), body, sig) {
			t.Logf("premise holds: the standard library verifies the forged signature for key %x", pub[:1])
		}
		keys, err := NewKeys([]string{encodeHex(pub)})
		if err == nil {
			t.Fatalf("NewKeys accepted small-order key %x", pub[:1])
		}
		if keys != nil {
			t.Error("a key set came back with the error")
		}
		// And a nil set verifies nothing.
		if _, err := keys.VerifyRefund(forged, Mainnet, testNow); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("forged approval against no keys: %v", err)
		}
	}
}

// TestNilKeys: a nil *Keys has no keys and does not panic.
func TestNilKeys(t *testing.T) {
	var k *Keys
	p := seedKey(1)
	if _, err := k.VerifyRefund(SignRefund(validRefund(p), p), Mainnet, testNow); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("VerifyRefund: %v", err)
	}
	if _, err := k.VerifyVoid(SignVoid(validVoid(p), p), Mainnet, testNow); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("VerifyVoid: %v", err)
	}
	if k.Has(KeyID(p.Public().(ed25519.PublicKey))) {
		t.Error("Has on nil")
	}
}

func TestVerifyAccepts(t *testing.T) {
	p, q := seedKey(1), seedKey(2)
	keys := testKeys(t, p, q)
	for _, priv := range []ed25519.PrivateKey{p, q} {
		r := validRefund(priv)
		got, err := keys.VerifyRefund(SignRefund(r, priv), Mainnet, testNow)
		if err != nil || got != r {
			t.Errorf("refund: %+v, %v", got, err)
		}
		v := validVoid(priv)
		gv, err := keys.VerifyVoid(SignVoid(v, priv), Mainnet, testNow)
		if err != nil || gv != v {
			t.Errorf("void: %+v, %v", gv, err)
		}
	}
	// The window ends at now + MaxLifetime inclusive.
	r := validRefund(p)
	r.ExpiresAt = testNow.Unix() + int64(MaxLifetime/time.Second)
	if _, err := keys.VerifyRefund(SignRefund(r, p), Mainnet, testNow); err != nil {
		t.Errorf("now+MaxLifetime: %v", err)
	}
	// The amount ends at 2^53 rounded down to whole sats.
	r.ExpiresAt = testNow.Unix() + 60
	r.AmountMsat = largeAmount
	if _, err := keys.VerifyRefund(SignRefund(r, p), Mainnet, testNow); err != nil {
		t.Errorf("large amount: %v", err)
	}
	r.AmountMsat = 1000
	if _, err := keys.VerifyRefund(SignRefund(r, p), Mainnet, testNow); err != nil {
		t.Errorf("1000 msat: %v", err)
	}
}

func hasErr(t *testing.T, name string, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Errorf("%s: got %v, want %v", name, got, want)
	}
}

// raw signs arbitrary bytes with priv and returns the hex approval, so a test can break one field and
// still carry a valid signature.
func raw(priv ed25519.PrivateKey, body []byte) string {
	return encodeHex(append(append([]byte{}, body...), ed25519.Sign(priv, body)...))
}

func TestParseRejects(t *testing.T) {
	p, q := seedKey(1), seedKey(2)
	stranger := seedKey(9)
	keys := testKeys(t, p, q)
	good := SignRefund(validRefund(p), p)
	goodVoid := SignVoid(validVoid(p), p)

	// Every length from 0 to 400 other than the exact one.
	pad := strings.Repeat("0", 400)
	for n := 0; n <= 400; n++ {
		var in string
		if n <= len(good) {
			in = good[:n]
		} else {
			in = good + pad[:n-len(good)]
		}
		if n != 296 {
			_, err := keys.VerifyRefund(in, Mainnet, testNow)
			hasErr(t, fmt.Sprintf("refund length %d", n), err, ErrMalformed)
		}
		if n <= len(goodVoid) {
			in = goodVoid[:n]
		} else {
			in = goodVoid + pad[:n-len(goodVoid)]
		}
		if n != 240 {
			_, err := keys.VerifyVoid(in, Mainnet, testNow)
			hasErr(t, fmt.Sprintf("void length %d", n), err, ErrMalformed)
		}
	}

	// Uppercase, mixed case, odd length, non-hex bytes.
	_, err := keys.VerifyRefund(strings.ToUpper(good), Mainnet, testNow)
	hasErr(t, "uppercase", err, ErrMalformed)
	_, err = keys.VerifyRefund(good[:290]+strings.ToUpper(good[290:]), Mainnet, testNow)
	hasErr(t, "mixed case tail", err, ErrMalformed)
	_, err = keys.VerifyRefund(good+"0", Mainnet, testNow)
	hasErr(t, "odd length", err, ErrMalformed)
	for _, c := range []string{"g", " ", "\n", "-", "é", "\x00", "G"} {
		for _, pos := range []int{0, 1, 100, 295} {
			if pos+len(c) > len(good) {
				continue
			}
			in := good[:pos] + c + good[pos+len(c):]
			_, err = keys.VerifyRefund(in, Mainnet, testNow)
			hasErr(t, fmt.Sprintf("char %q at %d", c, pos), err, ErrMalformed)
		}
	}
	_, err = keys.VerifyRefund("0x"+good[2:], Mainnet, testNow)
	hasErr(t, "0x prefix", err, ErrMalformed)

	// Each byte of the domain altered (the signature is valid over the altered bytes).
	body := marshalRefund(validRefund(p))
	for i := 0; i < 27; i++ {
		b := append([]byte{}, body...)
		b[i] ^= 1
		_, err := keys.VerifyRefund(raw(p, b), Mainnet, testNow)
		hasErr(t, fmt.Sprintf("refund domain byte %d", i), err, ErrMalformed)
	}
	vbody := marshalVoid(validVoid(p))
	for i := 0; i < 23; i++ {
		b := append([]byte{}, vbody...)
		b[i] ^= 1
		_, err := keys.VerifyVoid(raw(p, b), Mainnet, testNow)
		hasErr(t, fmt.Sprintf("void domain byte %d", i), err, ErrMalformed)
	}

	// Each network byte 0..255 against a configured network.
	for _, cfg := range networks {
		for n := 0; n < 256; n++ {
			r := validRefund(p)
			r.Network = Network(n)
			v := validVoid(p)
			v.Network = Network(n)
			_, errR := keys.VerifyRefund(SignRefund(r, p), cfg, testNow)
			_, errV := keys.VerifyVoid(SignVoid(v, p), cfg, testNow)
			if Network(n) == cfg {
				if errR != nil || errV != nil {
					t.Errorf("network %d: %v %v", n, errR, errV)
				}
				continue
			}
			hasErr(t, fmt.Sprintf("refund net %d vs %d", n, cfg), errR, ErrNetwork)
			hasErr(t, fmt.Sprintf("void net %d vs %d", n, cfg), errV, ErrNetwork)
		}
	}
	// A configured network outside 0..3 matches nothing, even an approval that carries the same byte.
	r := validRefund(p)
	r.Network = 7
	_, err = keys.VerifyRefund(SignRefund(r, p), 7, testNow)
	hasErr(t, "configured network 7", err, ErrNetwork)

	// Unknown key id: a stranger's key, and a configured key's id with one bit flipped.
	r = validRefund(stranger)
	_, err = keys.VerifyRefund(SignRefund(r, stranger), Mainnet, testNow)
	hasErr(t, "stranger", err, ErrUnknownKey)
	_, err = keys.VerifyVoid(SignVoid(validVoid(stranger), stranger), Mainnet, testNow)
	hasErr(t, "stranger void", err, ErrUnknownKey)
	// A configured key's id with one bit flipped, at every bit, is unknown.
	for bit := 0; bit < 64; bit++ {
		r := validRefund(p)
		r.KeyID[bit/8] ^= 1 << (bit % 8)
		_, err := keys.VerifyRefund(SignRefund(r, p), Mainnet, testNow)
		hasErr(t, fmt.Sprintf("refund key id bit %d", bit), err, ErrUnknownKey)
		v := validVoid(p)
		v.KeyID = r.KeyID
		_, err = keys.VerifyVoid(SignVoid(v, p), Mainnet, testNow)
		hasErr(t, fmt.Sprintf("void key id bit %d", bit), err, ErrUnknownKey)
	}
	// A known key id signed by the wrong key.
	r = validRefund(p)
	_, err = keys.VerifyRefund(SignRefund(r, stranger), Mainnet, testNow)
	hasErr(t, "wrong signer", err, ErrSignature)

	// One flipped bit at every offset: the domain, network and key id bytes fail earlier, every byte from
	// sigFrom on fails the signature. None is accepted.
	flipAll := func(name string, hexA string, sigFrom int, verify func(string) error) {
		b, _ := decodeHex(hexA)
		for i := 0; i < len(b)*8; i++ {
			c := append([]byte{}, b...)
			c[i/8] ^= 1 << (i % 8)
			err := verify(encodeHex(c))
			if err == nil {
				t.Fatalf("%s: bit %d flipped and accepted", name, i)
			}
			if i/8 >= sigFrom && !errors.Is(err, ErrSignature) {
				t.Errorf("%s: bit %d: %v, want signature failure", name, i, err)
			}
		}
	}
	flipAll("refund", good, 36, func(s string) error { _, err := keys.VerifyRefund(s, Mainnet, testNow); return err })
	flipAll("void", goodVoid, 32, func(s string) error { _, err := keys.VerifyVoid(s, Mainnet, testNow); return err })
	// Flipped in the hex text: every character replaced by every other hex digit.
	for i := 0; i < len(good); i++ {
		for _, d := range "0123456789abcdef" {
			if byte(d) == good[i] {
				continue
			}
			in := good[:i] + string(d) + good[i+1:]
			if _, err := keys.VerifyRefund(in, Mainnet, testNow); err == nil {
				t.Fatalf("refund char %d replaced by %c and accepted", i, d)
			}
		}
	}

	// Expiry.
	now := testNow.Unix()
	for _, c := range []struct {
		name string
		exp  int64
		want error
	}{
		{"now", now, ErrExpired}, {"now-1", now - 1, ErrExpired}, {"zero", 0, ErrExpired},
		{"now+max+1", now + int64(MaxLifetime/time.Second) + 1, ErrLifetime},
		{"max int64", 1<<63 - 1, ErrLifetime},
	} {
		r := validRefund(p)
		r.ExpiresAt = c.exp
		_, err := keys.VerifyRefund(SignRefund(r, p), Mainnet, testNow)
		hasErr(t, "refund expiry "+c.name, err, c.want)
		v := validVoid(p)
		v.ExpiresAt = c.exp
		_, err = keys.VerifyVoid(SignVoid(v, p), Mainnet, testNow)
		hasErr(t, "void expiry "+c.name, err, c.want)
	}
	// A negative int64 is a huge uint64 on the wire: never accepted.
	r = validRefund(p)
	r.ExpiresAt = -1
	_, err = keys.VerifyRefund(SignRefund(r, p), Mainnet, testNow)
	hasErr(t, "negative expiry", err, ErrLifetime)
	// Fractional now: expires_at equal to the whole second is expired.
	r = validRefund(p)
	r.ExpiresAt = now
	_, err = keys.VerifyRefund(SignRefund(r, p), Mainnet, testNow.Add(500*time.Millisecond))
	hasErr(t, "expiry at fractional now", err, ErrExpired)

	// Amount.
	for _, amt := range []uint64{0, 1, 999, 1001, 1500, 1<<53 + 1000, 1 << 53, 1<<64 - 1, largeAmount + 1000} {
		r := validRefund(p)
		r.AmountMsat = amt
		_, err := keys.VerifyRefund(SignRefund(r, p), Mainnet, testNow)
		hasErr(t, fmt.Sprintf("amount %d", amt), err, ErrAmount)
	}

	// Zero ids.
	r = validRefund(p)
	r.OrderID = [16]byte{}
	_, err = keys.VerifyRefund(SignRefund(r, p), Mainnet, testNow)
	hasErr(t, "zero order id", err, ErrMalformed)
	r = validRefund(p)
	r.RefundID = [16]byte{}
	_, err = keys.VerifyRefund(SignRefund(r, p), Mainnet, testNow)
	hasErr(t, "zero refund id", err, ErrMalformed)
	v := validVoid(p)
	v.RefundID = [16]byte{}
	_, err = keys.VerifyVoid(SignVoid(v, p), Mainnet, testNow)
	hasErr(t, "zero void refund id", err, ErrMalformed)

	// One kind given to the other's verifier.
	_, err = keys.VerifyVoid(good, Mainnet, testNow)
	hasErr(t, "refund to VerifyVoid", err, ErrMalformed)
	_, err = keys.VerifyRefund(goodVoid, Mainnet, testNow)
	hasErr(t, "void to VerifyRefund", err, ErrMalformed)
	// A void body padded to refund length with the refund domain-prefix swapped: same length, wrong domain.
	swapped := raw(p, append([]byte(voidDomain), make([]byte, refundBody-len(voidDomain))...))
	_, err = keys.VerifyRefund(swapped, Mainnet, testNow)
	hasErr(t, "void domain at refund length", err, ErrMalformed)
}

// TestErrorsCarryNoInput: every error is one of the fixed sentinels, so none holds any part of the input.
func TestErrorsCarryNoInput(t *testing.T) {
	p := seedKey(1)
	keys := testKeys(t, p)
	sentinels := []error{ErrMalformed, ErrNetwork, ErrUnknownKey, ErrSignature, ErrExpired, ErrLifetime, ErrAmount}
	r := validRefund(p)
	r.AmountMsat = 1001
	for _, in := range []string{SignRefund(r, p), strings.Repeat("ab", 148), "x", "", strings.Repeat("Z", 296)} {
		_, err := keys.VerifyRefund(in, Mainnet, testNow)
		known := false
		for _, s := range sentinels {
			known = known || err == s
		}
		if !known {
			t.Errorf("error %v is not a fixed sentinel", err)
		}
	}
}

// TestProperty: seeded random approvals round-trip through Sign and Verify exactly when their fields are
// in range, and a valid approval never verifies after any mutation.
func TestProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261109))
	privs := []ed25519.PrivateKey{seedKey(1), seedKey(2), seedKey(3)}
	keys := testKeys(t, privs[0], privs[1]) // the third key is not configured
	for i := 0; i < 3000; i++ {
		priv := privs[rng.Intn(3)]
		r := Refund{Network: Network(rng.Intn(5)), KeyID: KeyID(priv.Public().(ed25519.PublicKey))}
		rng.Read(r.RefundID[:])
		rng.Read(r.OrderID[:])
		if rng.Intn(10) == 0 {
			r.RefundID = [16]byte{}
		}
		switch rng.Intn(4) {
		case 0:
			r.AmountMsat = uint64(rng.Int63n(1<<43)) * 1000
		case 1:
			r.AmountMsat = rng.Uint64()
		case 2:
			r.AmountMsat = []uint64{0, 999, 1000, 1001, 1 << 53, largeAmount, largeAmount + 1000}[rng.Intn(7)]
		default:
			r.AmountMsat = uint64(1+rng.Intn(1000)) * 1000
		}
		r.ExpiresAt = testNow.Unix() + rng.Int63n(2*int64(MaxLifetime/time.Second)) - 10
		net := Network(rng.Intn(4))
		in := SignRefund(r, priv)
		got, err := keys.VerifyRefund(in, net, testNow)
		ok := r.Network == net && priv.Equal(privs[0]) || r.Network == net && priv.Equal(privs[1])
		ok = ok && r.RefundID != [16]byte{} && r.OrderID != [16]byte{} && r.AmountMsat > 0 && r.AmountMsat <= 1<<53 &&
			r.AmountMsat%1000 == 0 && r.ExpiresAt > testNow.Unix() && r.ExpiresAt <= testNow.Unix()+int64(MaxLifetime/time.Second)
		if ok != (err == nil) {
			t.Fatalf("case %d: %+v net %d: err %v, want ok=%v", i, r, net, err, ok)
		}
		if err == nil && got != r {
			t.Fatalf("case %d: read back %+v, signed %+v", i, got, r)
		}
		// Same for a void.
		v := Void{Network: r.Network, KeyID: r.KeyID, RefundID: r.RefundID, ExpiresAt: r.ExpiresAt}
		gv, err := keys.VerifyVoid(SignVoid(v, priv), net, testNow)
		okV := r.Network == net && !priv.Equal(privs[2]) && v.RefundID != [16]byte{} &&
			r.ExpiresAt > testNow.Unix() && r.ExpiresAt <= testNow.Unix()+int64(MaxLifetime/time.Second)
		if okV != (err == nil) || (err == nil && gv != v) {
			t.Fatalf("void case %d: %+v net %d: %+v, %v, want ok=%v", i, v, net, gv, err, okV)
		}
		// A random in-range edit of a valid approval is refused.
		if ok {
			b := []byte(in)
			pos := rng.Intn(len(b))
			b[pos] = hexDigits[(strings.IndexByte(hexDigits, b[pos])+1+rng.Intn(15))%16]
			if _, err := keys.VerifyRefund(string(b), net, testNow); err == nil {
				t.Fatalf("case %d: edit at %d accepted", i, pos)
			}
		}
	}
}

// FuzzVerify: no panic, and whatever is accepted is exactly what a re-encode of the result gives: the
// accepted hex starts with the encoding of the parsed fields.
func FuzzVerify(f *testing.F) {
	p, q := seedKey(1), seedKey(2)
	keys := testKeys(f, p, q)
	r := validRefund(p)
	f.Add(SignRefund(r, p))
	f.Add(SignVoid(validVoid(q), q))
	f.Add(SignRefund(r, q))
	f.Add("")
	f.Add(strings.Repeat("0", 296))
	f.Add(strings.Repeat("0", 240))
	f.Fuzz(func(t *testing.T, s string) {
		if r, err := keys.VerifyRefund(s, Mainnet, testNow); err == nil {
			if enc := encodeHex(marshalRefund(r)); !strings.HasPrefix(s, enc) || len(s) != 296 {
				t.Fatalf("accepted refund does not re-encode: %+v", r)
			}
			if !keys.Has(r.KeyID) || r.Network != Mainnet || r.AmountMsat == 0 || r.AmountMsat%1000 != 0 || r.AmountMsat > 1<<53 {
				t.Fatalf("accepted refund out of bounds: %+v", r)
			}
		}
		if v, err := keys.VerifyVoid(s, Mainnet, testNow); err == nil {
			if enc := encodeHex(marshalVoid(v)); !strings.HasPrefix(s, enc) || len(s) != 240 {
				t.Fatalf("accepted void does not re-encode: %+v", v)
			}
		}
	})
}
