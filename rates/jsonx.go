package rates

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"
)

// maxDepth bounds the nesting of a provider's answer. The deepest legitimate one is Kraken's: an object
// (the body), an object (result), an object (the pair), an array (a or b).
const maxDepth = 6

// CheckJSON verifies that b is exactly one JSON document of valid UTF-8, nested at most maxDepth deep
// (a scalar is depth 0, each array or object one more), with no duplicate key in any object (keys compared
// after decoding, exactly: no case folding). encoding/json alone accepts duplicates (the last one wins) and
// replaces invalid UTF-8, so a document could be read two ways. It is the one copy: a provider's answer here
// and the merchant API's request bodies and key file (package api) are checked by it, each with its own depth.
func CheckJSON(b []byte, maxDepth int) bool {
	if !utf8.Valid(b) {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	type frame struct {
		isObj   bool
		keys    map[string]struct{}
		wantKey bool
	}
	var stack []frame
	started, done := false, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if len(stack) != 0 {
				return false
			}
			break
		}
		if err != nil || done {
			return false
		}
		started = true
		if n := len(stack); n > 0 && stack[n-1].isObj && stack[n-1].wantKey {
			if d, ok := tok.(json.Delim); !ok || d != '}' {
				key, ok := tok.(string)
				if !ok {
					return false
				}
				if _, dup := stack[n-1].keys[key]; dup {
					return false
				}
				stack[n-1].keys[key] = struct{}{}
				stack[n-1].wantKey = false
				continue
			}
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if len(stack) >= maxDepth {
					return false
				}
				stack = append(stack, frame{isObj: d == '{', keys: map[string]struct{}{}, wantKey: d == '{'})
				continue
			default: // '}' or ']'
				stack = stack[:len(stack)-1]
			}
		}
		// A value (or a closed container) finished: an enclosing object wants its next key.
		if n := len(stack); n > 0 && stack[n-1].isObj {
			stack[n-1].wantKey = true
		} else if n == 0 {
			done = true // a second top-level value is an error
		}
	}
	return started
}

// decode reads raw into v (a map or slice of json.RawMessage) with UseNumber, so no number is ever
// turned into a float on the way.
func decode(raw []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v) == nil
}

// body checks b with CheckJSON and returns the members of its top-level object.
func body(b []byte) (map[string]json.RawMessage, bool) {
	if !CheckJSON(b, maxDepth) {
		return nil, false
	}
	return rawObject(bytes.TrimSpace(b))
}

// rawObject returns the members of a JSON object value.
func rawObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var m map[string]json.RawMessage
	if !decode(raw, &m) || m == nil {
		return nil, false
	}
	return m, true
}

// rawArray returns the elements of a JSON array value.
func rawArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var l []json.RawMessage
	if !decode(raw, &l) || l == nil {
		return nil, false
	}
	return l, true
}

// rawString returns the text of a JSON string value. A null, a number or anything else is refused.
func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// rawNumber returns the literal text of a JSON number value (CheckJSON has checked its grammar). A
// string holding a number is not a number.
func rawNumber(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || !(raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9')) {
		return "", false
	}
	return string(raw), true
}

// rawUint returns a JSON number value that is a non-negative integer that fits an int64: no sign, no
// point, no exponent.
func rawUint(raw json.RawMessage) (int64, bool) {
	s, ok := rawNumber(raw)
	if !ok {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// positive parses s with ParseDecimal and returns it when it is above zero.
func positive(s string) (Decimal, bool) {
	d, err := ParseDecimal(s)
	if err != nil || d.IsZero() {
		return Decimal{}, false
	}
	return d, true
}

// wanted returns currencies without repeats, in their order.
func wanted(currencies []string) []string {
	seen := make(map[string]bool, len(currencies))
	out := make([]string, 0, len(currencies))
	for _, c := range currencies {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}
