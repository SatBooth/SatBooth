package qr

import (
	"strings"
	"testing"
)

// FuzzSVG: SVG never panics; a success is a well-formed document that uses only the allowed
// elements, and a refusal carries one of the fixed error values.
func FuzzSVG(f *testing.F) {
	for _, s := range []string{
		"", "A", "a", "HELLO WORLD", "0123456789", "lnbc1...", "\x00", "\xff\xfe",
		strings.Repeat("A", MaxInput), strings.Repeat("A", MaxInput+1), strings.Repeat("\xff", MaxInput),
		"LIGHTNING:LNBC10U1PEXAMPLE", "é", "<script>",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, err := SVG(s)
		if err != nil {
			if err != ErrEmpty && err != ErrTooLong {
				t.Fatalf("unexpected error %q", err)
			}
			if len(s) > 0 && len(s) <= MaxInput {
				t.Fatalf("refused a %d-byte input: %v", len(s), err)
			}
			return
		}
		if len(s) == 0 || len(s) > MaxInput {
			t.Fatalf("accepted a %d-byte input", len(s))
		}
		doc, err := parseSVG(out)
		if err != nil {
			t.Fatalf("malformed output: %v", err)
		}
		if len(doc.paths) != 1 {
			t.Fatalf("%d paths", len(doc.paths))
		}
		_, v, _ := encode(s)
		n := 17 + 4*int(v) + 8
		parsePath(t, doc.paths[0], n)
	})
}
