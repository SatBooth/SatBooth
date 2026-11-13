package alert

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var shapeRE = map[shape]*regexp.Regexp{
	shapeHex32: regexp.MustCompile(`\A[0-9a-f]{32}\z`),
	shapeUint:  regexp.MustCompile(`\A(0|[1-9][0-9]*)\z`),
	shapeInt32: regexp.MustCompile(`\A(0|-?[1-9][0-9]*)\z`),
	shapeWord:  regexp.MustCompile(`\A[a-z][a-z0-9_]{0,39}\z`),
	shapeUpper: regexp.MustCompile(`\A[A-Z]{3}\z`),
	shapeName:  regexp.MustCompile(`\A[A-Za-z0-9._-]{1,64}\z`),
}

// FuzzFilter runs the attribute filter over arbitrary values under every allowed key and an unknown one, as a string
// and as integers. Invariants: every attribute is either a field or counted in dropped; every field's key is
// allowed and its value has the key's shape (an int32 within its range); FieldsJSON is a JSON object with sorted keys
// that reads back to the same fields, integers as numbers; the same input gives the same bytes.
func FuzzFilter(f *testing.F) {
	seeds := []string{
		"", "a", "BTC", "usd", "too_few", hex32, strings.ToUpper(hex32), hex32 + hex32, hex32[:31], hex32 + "0",
		"a" + strings.Repeat("b", 39), "a" + strings.Repeat("b", 40), "_a", "1a", "a-b", "a b", "a.b", "A.b_c-9", strings.Repeat("n", 64), strings.Repeat("n", 65), "a\x00", "\xff\xfe", "é",
		// An invoice prefix in the shape of BOLT 11's examples (no payment secret, cut before the signature), BIP-173's
		// example address, and an xprv-shaped text (the beginning of BIP-32 test vector 1's xprv, with an altered end).
		"lnbc1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaq",
		"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", "xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LrVHy",
		"ALERT: x", `"},"x":{`, " ", strings.Repeat("z", 5000),
	}
	for _, s := range seeds {
		f.Add(s, int64(0), uint64(0))
	}
	f.Add("x", int64(-1), uint64(1<<63))
	f.Add("x", int64(-2147483648), uint64(2147483647))
	f.Add("x", int64(-2147483649), uint64(2147483648))
	f.Add("x", int64(9223372036854775807), uint64(18446744073709551615))
	keys := []string{"providers", "secret", ""}
	for k := range allowed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	f.Fuzz(func(t *testing.T, s string, i int64, u uint64) {
		for _, k := range keys {
			all := []slog.Attr{slog.String(k, s), slog.Int64(k, i), slog.Uint64(k, u), slog.String(s, s)}
			for _, attrs := range [][]slog.Attr{all[:1], all[1:2], all[2:3], all[3:], all} {
				check(t, k, attrs)
			}
		}
	})
}

// check asserts FuzzFilter's invariants over one list of attributes.
func check(t *testing.T, k string, attrs []slog.Attr) {
	t.Helper()
	fields, dropped := filter(attrs)
	if len(fields)+dropped != len(attrs) {
		t.Fatalf("%q: %d fields + %d dropped != %d attrs", k, len(fields), dropped, len(attrs))
	}
	for fk, fv := range fields {
		sh, ok := allowed[fk]
		if !ok {
			t.Fatalf("field %q is not allowed", fk)
		}
		if !shapeRE[sh].MatchString(fv) {
			t.Fatalf("field %q=%q fails its shape", fk, fv)
		}
		if sh == shapeUint {
			if n, err := strconv.ParseUint(fv, 10, 64); err != nil || n > 1<<53 {
				t.Fatalf("%q=%q is above 2^53", fk, fv)
			}
		}
		if sh == shapeInt32 {
			if n, err := strconv.ParseInt(fv, 10, 32); err != nil || strconv.FormatInt(n, 10) != fv {
				t.Fatalf("code %q out of range", fv)
			}
		}
	}
	js := FieldsJSON(fields)
	if js != FieldsJSON(fields) {
		t.Fatal("FieldsJSON is not deterministic")
	}
	var back map[string]any
	dec := json.NewDecoder(strings.NewReader(js))
	dec.UseNumber()
	if err := dec.Decode(&back); err != nil {
		t.Fatalf("FieldsJSON %q: %v", js, err)
	}
	if len(back) != len(fields) {
		t.Fatalf("FieldsJSON %q lost fields", js)
	}
	for fk, fv := range fields {
		switch v := back[fk].(type) {
		case json.Number:
			if sh := allowed[fk]; (sh != shapeUint && sh != shapeInt32) || v.String() != fv {
				t.Fatalf("%q: number %v for %q", fk, v, fv)
			}
		case string:
			if sh := allowed[fk]; sh == shapeUint || sh == shapeInt32 || v != fv {
				t.Fatalf("%q: string %q for %q", fk, v, fv)
			}
		default:
			t.Fatalf("%q: %T", fk, v)
		}
	}
	// Sorted keys: each key appears after the previous one.
	names := make([]string, 0, len(fields))
	for fk := range fields {
		names = append(names, fk)
	}
	sort.Strings(names)
	pos := 0
	for _, n := range names {
		at := strings.Index(js[pos:], `"`+n+`":`)
		if at < 0 {
			t.Fatalf("keys of %q out of order", js)
		}
		pos += at + 1
	}
}
