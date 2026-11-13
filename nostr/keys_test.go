package nostr

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testKeyA is the secret key of BIP-340 test vector 1, testKeyB the secret key 3 (BIP-340 test vector 0), and curveN
// the order of secp256k1.
const (
	testKeyA = "b7e151628aed2a6abf7158809cf4f3c762e7160f38b4da56a784d9045190cfef"
	testKeyB = "0000000000000000000000000000000000000000000000000000000000000003"
	curveN   = "fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141"
)

func writeKeyFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // WriteFile is subject to the umask
		t.Fatal(err)
	}
	return p
}

func mustKey(t testing.TB, h string) *SecretKey {
	t.Helper()
	k, err := ParseSecretKeyHex(h)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestParseSecretKeyHex(t *testing.T) {
	k := mustKey(t, testKeyB)
	// The public key of the secret key 3 is the BIP-340 vector 0 key.
	want := "f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9"
	if got := k.PublicKeyHex(); got != want {
		t.Fatalf("PublicKeyHex = %s, want %s", got, want)
	}
	bad := map[string]string{
		"empty":     "",
		"63":        testKeyA[:63],
		"65":        testKeyA + "0",
		"uppercase": strings.ToUpper(testKeyA),
		"non-hex":   "g" + testKeyA[1:],
		"zero":      strings.Repeat("0", 64),
		"n":         curveN,
		"n+1":       "fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364142",
		"max":       strings.Repeat("f", 64),
		"newline":   testKeyA + "\n",
		"nsec":      "nsec1" + strings.Repeat("q", 58),
	}
	for name, s := range bad {
		k, err := ParseSecretKeyHex(s)
		if err == nil || k != nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if len(s) >= 8 && strings.Contains(err.Error(), s[:8]) {
			t.Errorf("%s: error holds input: %v", name, err)
		}
	}
	// n-1 is the largest valid key.
	if _, err := ParseSecretKeyHex("fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364140"); err != nil {
		t.Errorf("n-1 refused: %v", err)
	}
}

func TestLoadSecretKeyFile(t *testing.T) {
	ok := map[string]string{"bare": testKeyA, "newline": testKeyA + "\n"}
	for name, c := range ok {
		k, err := LoadSecretKeyFile(writeKeyFile(t, c, 0o600))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if k.PublicKeyHex() != mustKey(t, testKeyA).PublicKeyHex() {
			t.Errorf("%s: wrong key", name)
		}
	}
	if _, err := LoadSecretKeyFile(writeKeyFile(t, testKeyA, 0o400)); err != nil {
		t.Errorf("0400: %v", err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o606, 0o666, 0o601, 0o610} {
		p := writeKeyFile(t, testKeyA, mode)
		k, err := LoadSecretKeyFile(p)
		if err == nil || k != nil {
			t.Errorf("mode %o accepted", mode)
		} else if strings.Contains(err.Error(), testKeyA) {
			t.Errorf("mode %o: error holds the key", mode)
		}
	}
	badContent := map[string]string{
		"crlf":        testKeyA + "\r\n",
		"two newline": testKeyA + "\n\n",
		"lead space":  " " + testKeyA,
		"trail space": testKeyA + " ",
		"63":          testKeyA[:63],
		"65":          testKeyA + "0",
		"nsec":        "nsec1" + strings.Repeat("q", 58),
		"zero":        strings.Repeat("0", 64),
		"n":           curveN,
		"empty":       "",
		"newline":     "\n",
		"huge":        testKeyA + strings.Repeat("a", 1<<16),
		"upper":       strings.ToUpper(testKeyA),
		"nul":         testKeyA + "\x00",
		"two keys":    testKeyA + "\n" + testKeyA + "\n",
	}
	for name, c := range badContent {
		k, err := LoadSecretKeyFile(writeKeyFile(t, c, 0o600))
		if err == nil || k != nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		for _, frag := range []string{testKeyA, testKeyA[:16], curveN[:16]} {
			if strings.Contains(err.Error(), frag) {
				t.Errorf("%s: error string holds key material: %v", name, err)
			}
		}
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Logf("mkfifo unavailable: %v", err)
	} else {
		done := make(chan error, 1)
		go func() { _, err := LoadSecretKeyFile(fifo); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Error("FIFO accepted")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("LoadSecretKeyFile blocked on a FIFO")
		}
	}
	good := writeKeyFile(t, testKeyA, 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSecretKeyFile(link); err == nil {
		t.Error("symlink accepted")
	}
	if _, err := LoadSecretKeyFile(dir); err == nil {
		t.Error("directory accepted")
	}
	if _, err := LoadSecretKeyFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file accepted")
	}
	if _, err := LoadSecretKeyFile(""); err == nil {
		t.Error("empty path accepted")
	}
}

type unexportedHolder struct {
	k SecretKey
	p *SecretKey
}

type holder struct {
	Ptr *SecretKey
	Val SecretKey
	N   int
}

func TestSecretKeyNeverPrints(t *testing.T) {
	k := mustKey(t, testKeyA)
	any64 := regexp.MustCompile(`(?i)[0-9a-f]{64}`)
	h := holder{Ptr: k, Val: *k, N: 1}
	uh := unexportedHolder{k: *k, p: k}
	subjects := []any{k, *k, h, &h, uh, &uh, []*SecretKey{k}, map[string]*SecretKey{"k": k}, []any{k, h}}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%p", "%T", "%10.3v", "%-40s", "%08x"}
	for _, s := range subjects {
		for _, verb := range verbs {
			out := fmt.Sprintf(verb, s)
			if any64.MatchString(out) || strings.Contains(strings.ToLower(out), testKeyA[:16]) {
				t.Errorf("%s of %T leaks: %q", verb, s, out)
			}
		}
		for _, out := range []string{fmt.Sprint(s), fmt.Sprintln(s)} {
			if any64.MatchString(out) {
				t.Errorf("Sprint of %T leaks: %q", s, out)
			}
		}
	}
	if got := fmt.Sprintf("%v", k); got != "nostr.SecretKey{…}" {
		t.Errorf("%%v = %q", got)
	}
	if got := fmt.Sprintf("%#v", *k); got != "nostr.SecretKey{…}" {
		t.Errorf("%%#v = %q", got)
	}
	if k.String() != "nostr.SecretKey{…}" || k.GoString() != "nostr.SecretKey{…}" {
		t.Error("String/GoString")
	}
	// The public key is not secret and must still be printable.
	if !any64.MatchString(k.PublicKeyHex()) {
		t.Error("public key not 64 hex")
	}
}

func TestSharedXSymmetric(t *testing.T) {
	a, b := mustKey(t, testKeyA), mustKey(t, testKeyB)
	ab, err := a.SharedX(b.PublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	ba, err := b.SharedX(a.PublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	if ab != ba {
		t.Fatal("shared x differs")
	}
	if ab == ([32]byte{}) {
		t.Fatal("shared x is zero")
	}
	// Key 3 with the public key of key 1 (the generator G): 3G has x = vector 0's public key.
	one := mustKey(t, strings.Repeat("0", 63)+"1")
	x, err := b.SharedX(one.PublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", x); got != b.PublicKeyHex() {
		t.Fatalf("3*G x = %s, want %s", got, b.PublicKeyHex())
	}
	for name, p := range map[string]string{
		"empty":     "",
		"short":     b.PublicKeyHex()[:62],
		"long":      b.PublicKeyHex() + "00",
		"non-hex":   "zz" + b.PublicKeyHex()[2:],
		"uppercase": strings.ToUpper(b.PublicKeyHex()),
		"zero":      strings.Repeat("0", 64),
		"off curve": "eefdea4cdb677750a420fee807eacf21eb9898ae79b9768766e4faa04a2d4a34",
		"x >= p":    "fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc30",
		"0x02 pfx":  "02" + b.PublicKeyHex()[2:],
	} {
		if _, err := a.SharedX(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// checkParseSecret asserts ParseSecretKeyHex accepts exactly the lowercase 64-digit values in 1..n-1
// and that no error echoes the input.
func checkParseSecret(t testing.TB, s string) {
	t.Helper()
	n, _ := new(big.Int).SetString(curveN, 16)
	want := false
	if lowerHex(s, 64) {
		v, _ := new(big.Int).SetString(s, 16)
		want = v.Sign() > 0 && v.Cmp(n) < 0
	}
	k, err := ParseSecretKeyHex(s)
	if (err == nil) != want || (k == nil) != !want {
		t.Fatalf("ParseSecretKeyHex(%q): err=%v, want accepted=%v", s, err, want)
	}
	if err != nil && len(s) >= 8 && strings.Contains(err.Error(), s) {
		t.Fatalf("error echoes input %q", s)
	}
	if k != nil && !lowerHex(k.PublicKeyHex(), 64) {
		t.Fatalf("bad public key for %q", s)
	}
}

func TestParseSecretKeyHexProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	const hexd = "0123456789abcdefABCDEF"
	for i := 0; i < 3000; i++ {
		var s string
		switch i % 4 {
		case 0: // random length around the boundary, hex alphabet
			b := make([]byte, 62+r.IntN(5))
			for j := range b {
				b[j] = hexd[r.IntN(len(hexd))]
			}
			s = string(b)
		case 1: // random lowercase 64-digit values, some near n
			b := make([]byte, 32)
			for j := range b {
				b[j] = byte(r.IntN(256))
			}
			if r.IntN(2) == 0 {
				copy(b, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe})
			}
			s = hex.EncodeToString(b)
		case 2: // any bytes
			b := make([]byte, r.IntN(70))
			for j := range b {
				b[j] = byte(r.IntN(256))
			}
			s = string(b)
		default: // a valid key with one byte replaced by every odd value
			b := []byte(testKeyA)
			b[r.IntN(64)] = byte(r.IntN(256))
			s = string(b)
		}
		checkParseSecret(t, s)
	}
	for _, s := range []string{"", strings.Repeat("0", 64), curveN, strings.Repeat("0", 63) + "1", strings.Repeat("f", 64), " ", " "} {
		checkParseSecret(t, s)
	}
	for c := 0; c < 256; c++ {
		checkParseSecret(t, testKeyA[:63]+string([]byte{byte(c)}))
	}
}

func FuzzParseSecretKeyHex(f *testing.F) {
	for _, s := range []string{"", testKeyA, testKeyB, curveN, strings.Repeat("0", 64), strings.Repeat("f", 64), testKeyA + "\n", " ", " ", strings.Repeat("a", 65)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { checkParseSecret(t, s) })
}

func TestLoadSecretKeyFileWrapsPathError(t *testing.T) {
	_, err := LoadSecretKeyFile(filepath.Join(t.TempDir(), "missing"))
	var pe *fs.PathError
	if !errors.Is(err, errKeyFileIO) || !errors.As(err, &pe) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("got %v, want errKeyFileIO wrapping a not-exist PathError", err)
	}
}

func TestValidatePubKey(t *testing.T) {
	good := mustKey(t, testKeyB).PublicKeyHex()
	const (
		p        = "fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f" // field prime
		offCurve = "eefdea4cdb677750a420fee807eacf21eb9898ae79b9768766e4faa04a2d4a34" // no point with this x
	)
	for name, c := range map[string]struct {
		in string
		ok bool
	}{
		"valid":                 {good, true},
		"uppercase":             {strings.ToUpper(good), false},
		"63 characters":         {good[:63], false},
		"65 characters":         {good + "0", false},
		"empty":                 {"", false},
		"x equals p":            {p, false},
		"x above p":             {strings.Repeat("f", 64), false},
		"x with no curve point": {offCurve, false},
	} {
		err := ValidatePubKey(c.in)
		if (err == nil) != c.ok {
			t.Errorf("%s: got %v, want ok=%v", name, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrBadPubKey) {
			t.Errorf("%s: error %v does not wrap ErrBadPubKey", name, err)
		}
	}
}

func FuzzValidatePubKey(f *testing.F) {
	b := mustKey(f, testKeyB).PublicKeyHex()
	for _, s := range []string{"", b, strings.ToUpper(b), b[:63], b + "0", strings.Repeat("f", 64), strings.Repeat("0", 64), "\xff", strings.Repeat("g", 64)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		err := ValidatePubKey(s)
		_, perr := parsePub(s)
		if (err == nil) != (perr == nil) {
			t.Fatalf("ValidatePubKey(%q) = %v, parsePub error = %v", s, err, perr)
		}
		if err != nil && !errors.Is(err, ErrBadPubKey) {
			t.Fatalf("ValidatePubKey(%q): %v does not wrap ErrBadPubKey", s, err)
		}
	})
}

func TestZero(t *testing.T) {
	k := mustKey(t, testKeyA)
	pub := k.PublicKeyHex()
	if pub == "" {
		t.Fatal("no public key before Zero")
	}
	if _, err := k.SharedX(mustKey(t, testKeyB).PublicKeyHex()); err != nil {
		t.Fatal(err)
	}
	k.Zero()
	k.Zero() // harmless twice
	e := &Event{Kind: 1, Tags: [][]string{}}
	if err := Sign(e, k); err == nil {
		t.Error("Sign worked with a zeroed key")
	}
	if e.ID != "" || e.Sig != "" || e.PubKey != "" {
		t.Error("Sign changed the event with a zeroed key")
	}
	if x, err := k.SharedX(pub); err == nil || x != ([32]byte{}) {
		t.Errorf("SharedX with a zeroed key: %x, %v", x, err)
	}
	if got := k.PublicKeyHex(); got != "" {
		t.Errorf("PublicKeyHex = %q after Zero", got)
	}
	if err := WriteNewSecretKeyFile(filepath.Join(t.TempDir(), "k"), k); err == nil {
		t.Error("WriteNewSecretKeyFile worked with a zeroed key")
	}
	if _, err := signID(k, [32]byte{1}, nil); err == nil {
		t.Error("signID worked with a zeroed key")
	}
	var nilKey *SecretKey
	nilKey.Zero() // harmless
}
