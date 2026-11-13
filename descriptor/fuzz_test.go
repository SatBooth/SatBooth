package descriptor

import (
	"math/rand"
	"strings"
	"testing"
)

// FuzzParse: no panic; only the three fixed errors; a success re-parses from its Canonical to the
// same value and the Canonical is clean; an xprv or tprv in the first 1024 bytes is always
// ErrPrivateKey.
func FuzzParse(f *testing.F) {
	r := rand.New(rand.NewSource(5))
	for _, s := range []string{
		"", "wpkh()", "tr(", "wpkh(" + xpubV1 + "/0/*)", "tr(" + tpubV1 + "/0/*)#qpzry9x8",
		"wpkh([d34db33f/84'/0h/0]" + xpubV1 + "/0/*)", "wpkh(" + xprvV1 + "/0/*)", tprvV1, wifEx,
		wifEx + string(rune(0x0a)), strings.Repeat("a", 1025), strings.Repeat("1", 300),
	} {
		f.Add(s)
	}
	for i := 0; i < 20; i++ {
		f.Add(randDescriptor(r))
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := Parse(s)
		head := s
		if len(head) > 1024 {
			head = head[:1024]
		}
		if strings.Contains(head, "xprv") || strings.Contains(head, "tprv") {
			if err != ErrPrivateKey {
				t.Fatalf("private prefix not refused: %v", err)
			}
			return
		}
		if err != nil {
			if err != ErrSyntax && err != ErrPrivateKey && err != ErrTooLong {
				t.Fatalf("unexpected error %v", err)
			}
			if len(s) > 1024 && err != ErrTooLong && err != ErrPrivateKey {
				t.Fatalf("long input: %v", err)
			}
			return
		}
		if len(s) > maxLen {
			t.Fatal("accepted over-long input")
		}
		d2, err := Parse(d.Canonical)
		if err != nil || d2 != d {
			t.Fatalf("canonical %q: %+v %v", d.Canonical, d2, err)
		}
		if hasPrivateMaterial(d.Canonical) {
			t.Fatal("canonical holds private material")
		}
		if d.Func != "wpkh" && d.Func != "tr" {
			t.Fatal("func")
		}
	})
}

// FuzzDecodeCheck: no panic; a success re-encodes to the same string.
func FuzzDecodeCheck(f *testing.F) {
	for _, s := range []string{"", "1", xpubV1, tprvV1, wifEx, "1111111111"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, ok := decodeCheck(s)
		if ok && encodeCheck(b) != s {
			t.Fatalf("re-encode %q -> %q", s, encodeCheck(b))
		}
	})
}
