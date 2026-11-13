package rates

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

// nest wraps the scalar 1 in n containers, each an array or an object as kinds says (bit i set: the i-th from the
// outside is an object with the key "k").
func nest(n int, kinds uint) string {
	var open, close strings.Builder
	for i := 0; i < n; i++ {
		if kinds&(1<<i) != 0 {
			open.WriteString(`{"k":`)
		} else {
			open.WriteString(`[`)
		}
	}
	for i := n - 1; i >= 0; i-- {
		if kinds&(1<<i) != 0 {
			close.WriteString(`}`)
		} else {
			close.WriteString(`]`)
		}
	}
	return open.String() + "1" + close.String()
}

// scanDepth is the deepest nesting of brackets and braces outside strings: an independent count for the fuzz target.
func scanDepth(b []byte) int {
	depth, deepest, inString, escaped := 0, 0, false, false
	for _, c := range b {
		switch {
		case inString && escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case inString && c == '"':
			inString = false
		case inString:
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			depth++
			deepest = max(deepest, depth)
		case c == ']' || c == '}':
			depth--
		}
	}
	return deepest
}

// CheckJSON takes the depth from its caller: a document nested exactly maxDepth deep passes and one level more fails,
// for every depth and every mix of arrays and objects.
func TestCheckJSONDepth(t *testing.T) {
	rng := rand.New(rand.NewSource(20261109))
	for maxDepth := 0; maxDepth <= 9; maxDepth++ {
		for n := 0; n <= 11; n++ {
			for range 8 {
				doc := nest(n, uint(rng.Intn(1<<12)))
				if got := CheckJSON([]byte(doc), maxDepth); got != (n <= maxDepth) {
					t.Fatalf("CheckJSON(%s, %d) = %v", doc, maxDepth, got)
				}
			}
		}
	}
}

// One JSON document of valid UTF-8 with no duplicate key in any object, at any depth; nothing else.
func TestCheckJSON(t *testing.T) {
	const ok, refused = true, false
	for _, c := range []struct {
		doc  string
		want bool
	}{
		{`{}`, ok}, {`[]`, ok}, {`1`, ok}, {`"s"`, ok}, {`null`, ok}, {" \t\n{} \n", ok},
		{``, refused}, {` `, refused}, {`{}{}`, refused}, {`{} 1`, refused}, {`1 2`, refused}, {`{`, refused}, {`{]`, refused},
		{`[1,]`, refused}, {`{"a":1,}`, refused}, {`{"a"}`, refused}, {`{1:2}`, refused}, {`'a'`, refused}, {`NaN`, refused},
		{`{"a":1,"a":2}`, refused},                       // a duplicate key
		{`{"a":1,"b":{"c":1,"c":1}}`, refused},           // one level down
		{`[{"a":1},{"a":1}]`, ok},                        // the same key in two objects
		{`{"a":{"a":1}}`, ok},                            // in an object and the one inside it
		{`{"a":1,"A":1}`, ok},                            // case variants are two keys
		{`{"a":1,"a":2}`, refused},                       // an escape that decodes to the same key
		{`{"a":[{"b":1,"b":2}]}`, refused},               // inside an array inside an object
		{"\"\xff\"", refused}, {"{\"\xc3\":1}", refused}, // invalid UTF-8 in a string and in a key
		{"\"\xc3\xa9\"", ok}, // valid UTF-8
	} {
		if got := CheckJSON([]byte(c.doc), 8); got != c.want {
			t.Errorf("CheckJSON(%q) = %v, want %v", c.doc, got, c.want)
		}
	}
}

// Over any input: what CheckJSON accepts is valid JSON, nested no deeper than allowed by an independent count, and
// still accepted with one more level allowed; the scalar documents are the only ones accepted at depth 0.
func FuzzCheckJSON(f *testing.F) {
	for _, s := range []string{`{}`, `[]`, `1`, `{"a":1,"a":2}`, `{"a":1,"a":2}`, `[[[[[[[1]]]]]]]`, nest(6, 0b101010),
		nest(7, 0), "\"\xff\"", `{}{}`, `{"k":[{"k":{"k":1}}]}`, `"\\"`, `"[["`} {
		for _, d := range []int{0, 1, 6, 8} {
			f.Add([]byte(s), d)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte, d int) {
		d = max(0, min(d, 12))
		if !CheckJSON(b, d) {
			return
		}
		if !json.Valid(b) {
			t.Fatalf("CheckJSON accepted invalid JSON %q", b)
		}
		if n := scanDepth(b); n > d {
			t.Fatalf("CheckJSON(%q, %d) accepted depth %d", b, d, n)
		}
		if !CheckJSON(b, d+1) {
			t.Fatalf("CheckJSON(%q) accepted at depth %d, refused at %d", b, d, d+1)
		}
	})
}
