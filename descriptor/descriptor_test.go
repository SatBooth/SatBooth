package descriptor

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"math/rand"
	"os"
	"strings"
	"testing"
)

// The keys of BIP-32 test vector 1 (bitcoin/bips, bip-0032.mediawiki), their testnet encodings (tpub, tprv), and
// the WIF forms of its chain m key.
const (
	xpubV1 = "xpub661MyMwAqRbcFtXgS5sYJABqqG9YLmC4Q1Rdap9gSE8NqtwybGhePY2gZ29ESFjqJoCu1Rupje8YtGqsefD265TMg7usUDFdp6W1EGMcet8"
	xprvV1 = "xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHi"
	tpubV1 = "tpubD6NzVbkrYhZ4XgiXtGrdW5XDAPFCL9h7we1vwNCpn8tGbBcgfVYjXyhWo4E1xkh56hjod1RhGjxbaTLV3X4FyWuejifB9jusQ46QzG87VKp"
	tprvV1 = "tprv8ZgxMBicQKsPeDgjzdC36fs6bMjGApWDNLR9erAXMs5skhMv36j9MV5ecvfavji5khqjWaWSFhN3YcCUUdiKH6isR4Pwy3U5y5egddBr16m"
	wifEx  = "5KasyVKwgbH5VmDomdJdevZXRMMrbWcePkW17vxeg8daJWoeqHQ"  // BIP32 vector 1 chain m key, uncompressed
	wifC   = "L52XzL2cMkHxqxBXRyEpnPQZGUs3uKiL3R11XbAdHigRzDozKZeW" // compressed
	wifT   = "cVPXTF2TnozE1PenpP3x9huctiATZmp27T9Ue1d8nqLSExoPwfN5" // testnet compressed
)

// encodeCheck is the test-side Base58Check encoder (the code under test only decodes).
func encodeCheck(data []byte) string {
	h := sha256.Sum256(data)
	h = sha256.Sum256(h[:])
	raw := append(append([]byte{}, data...), h[:4]...)
	n := new(big.Int).SetBytes(raw)
	var out []byte
	m := new(big.Int)
	for n.Sign() > 0 {
		n.DivMod(n, big58, m)
		out = append(out, alphabet[m.Int64()])
	}
	for _, c := range raw {
		if c != 0 {
			break
		}
		out = append(out, '1')
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

type vectors struct {
	Accept []struct {
		Input, Func, Origin, Canonical string
		Testnet                        bool
	}
	Refuse []struct{ Name, Input, Err string }
}

func errFor(name string) error {
	switch name {
	case "private":
		return ErrPrivateKey
	case "syntax":
		return ErrSyntax
	case "long":
		return ErrTooLong
	}
	panic(name)
}

// TestVectors loads testdata/vectors.json: the keys of BIP-32 test vectors 1 and 5 (bitcoin/bips, bip-0032.mediawiki)
// and the descriptors of a Bitcoin Core 32.1 regtest wallet; the file's "source" field says which.
func TestVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, a := range v.Accept {
		d, err := Parse(a.Input)
		if err != nil {
			t.Fatalf("%q: %v", a.Input, err)
		}
		if d.Func != a.Func || d.Origin != a.Origin || d.Testnet != a.Testnet || d.Canonical != a.Canonical {
			t.Fatalf("%q: got %+v", a.Input, d)
		}
		if !strings.Contains(a.Input, d.Key) || len(d.Key) != 111 {
			t.Fatalf("key %q", d.Key)
		}
		if d2, err := Parse(d.Canonical); err != nil || d2 != d {
			t.Fatalf("canonical re-parse: %+v %v", d2, err)
		}
	}
	for _, r := range v.Refuse {
		if _, err := Parse(r.Input); err != errFor(r.Err) {
			t.Fatalf("%s: got %v want %v", r.Name, err, errFor(r.Err))
		}
	}
}

func TestAddressType(t *testing.T) {
	if (Descriptor{Func: "wpkh"}).AddressType() != "bech32" || (Descriptor{Func: "tr"}).AddressType() != "bech32m" {
		t.Fatal("address type")
	}
}

// BIP32 test vector 1, chain m: the xpub and xprv decode with their versions and share a payload.
func TestBase58Vectors(t *testing.T) {
	pub, ok := decodeCheck(xpubV1)
	if !ok || len(pub) != 78 || !bytes.Equal(pub[:4], []byte{0x04, 0x88, 0xB2, 0x1E}) {
		t.Fatal("xpub")
	}
	prv, ok := decodeCheck(xprvV1)
	if !ok || len(prv) != 78 || !bytes.Equal(prv[:4], []byte{0x04, 0x88, 0xAD, 0xE4}) {
		t.Fatal("xprv")
	}
	if !bytes.Equal(pub[4:13], prv[4:13]) || !bytes.Equal(pub[13:45], prv[13:45]) {
		t.Fatal("chain code differs")
	}
	if encodeCheck(pub) != xpubV1 || encodeCheck(prv) != xprvV1 {
		t.Fatal("encoder disagrees with BIP32 vector")
	}
	w, ok := decodeCheck(wifEx)
	if !ok || len(w) != 33 || w[0] != 0x80 {
		t.Fatal("wif")
	}
	// Leading '1' is a zero byte (Bitcoin wiki Base58Check page: version 0x00 addresses start with 1).
	z, ok := decodeCheck(encodeCheck([]byte{0, 0, 7}))
	if !ok || !bytes.Equal(z, []byte{0, 0, 7}) {
		t.Fatalf("leading zeros: %v", z)
	}
	for _, bad := range []string{"", "1", "1111", xpubV1[:110], xpubV1 + "1", "0" + xpubV1[1:], "I" + xpubV1[1:], "l" + xpubV1[1:], "O" + xpubV1[1:]} {
		if _, ok := decodeCheck(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// The testnet keys are the BIP32 vector 1 payload with the testnet versions and a recomputed checksum.
func TestTestnetVectorsDerived(t *testing.T) {
	pub, _ := decodeCheck(xpubV1)
	prv, _ := decodeCheck(xprvV1)
	tp := append([]byte{0x04, 0x35, 0x87, 0xCF}, pub[4:]...)
	tr := append([]byte{0x04, 0x35, 0x83, 0x94}, prv[4:]...)
	if encodeCheck(tp) != tpubV1 || encodeCheck(tr) != tprvV1 {
		t.Fatal("pinned testnet strings differ from the derivation")
	}
}

func TestOddInputs(t *testing.T) {
	ok := "wpkh(" + xpubV1 + "/0/*)"
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"257 bytes", ok + strings.Repeat("a", 257-len(ok)), ErrSyntax},
		{"1024 bytes", ok + strings.Repeat("a", 1024-len(ok)), ErrSyntax},
		{"1025 bytes", ok + strings.Repeat("a", 1025-len(ok)), ErrTooLong},
		{"space inside", "wpkh(" + xpubV1 + "/0/*" + string(rune(0x20)) + ")", ErrSyntax},
		{"trailing newline", ok + string(rune(0x0a)), ErrSyntax},
		{"byte 0x7f", ok[:10] + string(rune(0x7f)) + ok[11:], ErrSyntax},
		{"byte 0x80", ok[:10] + string([]byte{0x80}) + ok[11:], ErrSyntax},
		{"xprv then newline", "wpkh(" + xprvV1 + "/0/*)" + string(rune(0x0a)), ErrPrivateKey},
		{"xprv then space", xprvV1 + string(rune(0x20)), ErrPrivateKey},
		{"xprv in last scanned bytes", strings.Repeat("(", 1020) + "xprv", ErrPrivateKey},
		{"xprv after 1024 bytes", strings.Repeat("(", 1024) + "xprv", ErrTooLong},
		{"yprv", "yprv" + strings.Repeat("a", 5), ErrPrivateKey},
		{"Zprv", "Zprv", ErrPrivateKey},
	}
	for _, c := range cases {
		if _, err := Parse(c.in); err != c.want {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
		}
	}
}

// WIF forms that the prefix check cannot see: testnet (0xEF) and compressed (34 bytes).
func TestWIFRuns(t *testing.T) {
	body := bytes.Repeat([]byte{0x11}, 32)
	forms := [][]byte{
		append([]byte{0x80}, body...),
		append(append([]byte{0x80}, body...), 0x01),
		append([]byte{0xEF}, body...),
		append(append([]byte{0xEF}, body...), 0x01),
	}
	for _, f := range forms {
		w := encodeCheck(f)
		for _, in := range []string{w, "wpkh(" + w + ")", w + string(rune(0x0a)), "[" + w + "]"} {
			if _, err := Parse(in); err != ErrPrivateKey {
				t.Errorf("%d-byte wif %q: %v", len(f), in, err)
			}
		}
	}
	// 0x00 version, 33 bytes: an address-shaped payload, not private.
	if hasPrivateMaterial(encodeCheck(append([]byte{0x00}, body...))) {
		t.Error("flagged a non-WIF payload")
	}
}

func TestErrorsNeverEchoInput(t *testing.T) {
	const marker = "MARKERzz"
	inputs := []string{
		marker, "wpkh(" + marker + ")", "wpkh(" + xprvV1 + "/0/*)" + marker, wifEx + marker,
		"wpkh(" + xpubV1 + "/1/*)" + marker, strings.Repeat(marker, 200), tprvV1 + marker,
	}
	for _, in := range inputs {
		_, err := Parse(in)
		if err == nil {
			t.Fatalf("%q accepted", in)
		}
		for _, secret := range []string{marker, xprvV1[:8], tprvV1[:8], wifEx[:8]} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error %q echoes input", err)
			}
		}
	}
}

// WIFs and testnet keys are derived from BIP32 vector 1 chain m, so the pinned strings are checked.
func TestWIFDerived(t *testing.T) {
	prv, _ := decodeCheck(xprvV1)
	key := prv[46:78] // after the 0x00 pad byte
	if prv[45] != 0 {
		t.Fatal("layout")
	}
	unc := append([]byte{0x80}, key...)
	cmp := append(append([]byte{0x80}, key...), 0x01)
	tst := append(append([]byte{0xEF}, key...), 0x01)
	if encodeCheck(unc) != wifEx || encodeCheck(cmp) != wifC || encodeCheck(tst) != wifT {
		t.Fatal("pinned WIFs differ from the derivation")
	}
	for _, w := range []string{wifEx, wifC, wifT} {
		if _, err := Parse(w); err != ErrPrivateKey {
			t.Fatalf("%q: %v", w, err)
		}
	}
}

// Each check in checkKey must be load-bearing: build payloads that differ from the valid vector-1
// xpub in exactly one respect.
func TestCheckKeyRules(t *testing.T) {
	pub, _ := decodeCheck(xpubV1)
	with := func(f func(b []byte)) string {
		b := append([]byte{}, pub...)
		f(b)
		return "wpkh(" + encodeCheck(b) + "/0/*)"
	}
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"unchanged", with(func(b []byte) {}), nil},
		{"depth 0 with parent fingerprint", with(func(b []byte) { b[5] = 1 }), ErrSyntax},
		{"depth 0 with child number", with(func(b []byte) { b[12] = 1 }), ErrSyntax},
		{"depth 1 with parent fingerprint", with(func(b []byte) { b[4] = 1; b[5] = 1; b[12] = 1 }), nil},
		{"prefix 04", with(func(b []byte) { b[45] = 4 }), ErrSyntax},
		{"prefix 01", with(func(b []byte) { b[45] = 1 }), ErrSyntax},
		{"prefix 00 is private", with(func(b []byte) { b[45] = 0 }), ErrPrivateKey},
		{"prefix 00 under a testnet version", with(func(b []byte) { copy(b, []byte{4, 0x35, 0x87, 0xCF}); b[45] = 0 }), ErrPrivateKey},
		{"prefix 00 under an unknown version", with(func(b []byte) { copy(b, []byte{1, 2, 3, 4}); b[45] = 0 }), ErrPrivateKey},
		{"unknown version", with(func(b []byte) { copy(b, []byte{1, 2, 3, 4}) }), ErrSyntax},
		{"79 bytes", "wpkh(" + encodeCheck(append(append([]byte{}, pub...), 2)) + "/0/*)", ErrSyntax},
	}
	for _, c := range cases {
		_, err := Parse(c.in)
		if err != c.want {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
		}
	}
	// SLIP-132 public versions with a valid checksum: not accepted.
	for name, v := range map[string]uint32{
		"ypub": 0x049d7cb2, "zpub": 0x04b24746, "upub": 0x044a5262, "vpub": 0x045f1cf6,
		"Ypub": 0x0295b43f, "Zpub": 0x02aa7ed3, "Upub": 0x024289ef, "Vpub": 0x02575483,
	} {
		in := with(func(b []byte) { b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v) })
		if _, err := Parse(in); err != ErrSyntax {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// ---- seeded properties ----

func randHex(r *rand.Rand, n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hex[r.Intn(16)]
	}
	return string(b)
}

func randKey(r *rand.Rand, testnet bool) string {
	b := make([]byte, 78)
	r.Read(b)
	if testnet {
		copy(b, []byte{0x04, 0x35, 0x87, 0xCF})
	} else {
		copy(b, []byte{0x04, 0x88, 0xB2, 0x1E})
	}
	if r.Intn(8) == 0 {
		b[4] = 0
		for i := 5; i < 13; i++ {
			b[i] = 0
		}
	} else if b[4] == 0 {
		b[4] = 1
	}
	b[45] = byte(2 + r.Intn(2))
	return encodeCheck(b)
}

func randDescriptor(r *rand.Rand) string {
	fn := [...]string{"wpkh", "tr"}[r.Intn(2)]
	origin := ""
	if r.Intn(3) > 0 {
		origin = "[" + randHex(r, 8)
		for i, n := 0, r.Intn(11); i < n; i++ {
			idx := r.Int63n(2147483648)
			if r.Intn(4) == 0 {
				idx = r.Int63n(3)
			}
			origin += "/" + big.NewInt(idx).String() + [...]string{"", "h", "'"}[r.Intn(3)]
		}
		origin += "]"
	}
	s := fn + "(" + origin + randKey(r, r.Intn(2) == 0) + "/0/*)"
	if r.Intn(3) == 0 {
		cs := make([]byte, 8)
		for i := range cs {
			cs[i] = checkAlpha[r.Intn(32)]
		}
		s += "#" + string(cs)
	}
	return s
}

func TestPropertyRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		s := randDescriptor(r)
		d, err := Parse(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		d2, err := Parse(d.Canonical)
		if err != nil || d2 != d {
			t.Fatalf("%q: canonical %q -> %+v %v", s, d.Canonical, d2, err)
		}
		if strings.Contains(d.Canonical, "'") || strings.Contains(d.Canonical, "#") {
			t.Fatalf("not normalised: %q", d.Canonical)
		}
	}
}

func mutate(r *rand.Rand, s string) (string, int) {
	b := []byte(s)
	pos := r.Intn(len(b))
	switch r.Intn(3) {
	case 0:
		c := byte(r.Intn(256))
		for c == b[pos] {
			c = byte(r.Intn(256))
		}
		b[pos] = c
	case 1:
		b = append(b[:pos], append([]byte{byte(r.Intn(256))}, b[pos:]...)...)
	default:
		b = append(b[:pos], b[pos+1:]...)
	}
	return string(b), pos
}

func TestPropertyMutants(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for i := 0; i < 5000; i++ {
		s := randDescriptor(r)
		m, _ := mutate(r, s)
		d, err := Parse(m)
		if err != nil {
			if err != ErrSyntax && err != ErrPrivateKey && err != ErrTooLong {
				t.Fatalf("unexpected error %v", err)
			}
			continue
		}
		d2, err := Parse(d.Canonical)
		if err != nil || d2 != d || hasPrivateMaterial(d.Canonical) {
			t.Fatalf("%q accepted but canonical %q is not stable/clean", m, d.Canonical)
		}
	}
}

// A single-character change inside the key's 111 characters is always caught.
func TestPropertyKeyMutationsAlwaysError(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for i := 0; i < 5000; i++ {
		s := randDescriptor(r)
		d, err := Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		at := strings.Index(s, d.Key)
		b := []byte(s)
		pos := at + r.Intn(len(d.Key))
		switch r.Intn(3) {
		case 0:
			c := byte(r.Intn(256))
			for c == b[pos] {
				c = byte(r.Intn(256))
			}
			b[pos] = c
		case 1:
			b = append(b[:pos], append([]byte{byte(r.Intn(256))}, b[pos:]...)...)
		default:
			b = append(b[:pos], b[pos+1:]...)
		}
		if _, err := Parse(string(b)); err == nil {
			t.Fatalf("mutated key accepted: %q", b)
		}
	}
}

// Every private prefix, anywhere, with any trailing junk, is ErrPrivateKey.
func TestPropertyPrivatePrefixFirst(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	for i := 0; i < 5000; i++ {
		p := privPrefixes[r.Intn(len(privPrefixes))]
		junk := make([]byte, r.Intn(300))
		r.Read(junk)
		pos := r.Intn(len(junk) + 1) // at most 300 + 4 bytes: always inside the scan window
		in := string(junk[:pos]) + p + string(junk[pos:])
		if _, err := Parse(in); err != ErrPrivateKey {
			t.Fatalf("%q: %v", in, err)
		}
	}
}
