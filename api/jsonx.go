package api

import (
	"bytes"
	"encoding/json"

	"github.com/satbooth/satbooth/rates"
)

// maxDepth bounds the nesting of any document this package reads (rates.CheckJSON's depth). The deepest
// legitimate one is the key file: an object, an array, an object.
const maxDepth = 8

// strictObject checks b with rates.CheckJSON and returns its members. b must be a JSON object whose keys
// are exactly the names in required (each present once) plus any of optional. Keys are matched
// exactly: encoding/json's struct decoding would also accept "Account_ID" for "account_id", and a
// case-variant pair would slip past the duplicate check.
func strictObject(b []byte, required, optional []string) (map[string]json.RawMessage, bool) {
	if !rates.CheckJSON(b, maxDepth) {
		return nil, false
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, false
	}
	known := make(map[string]bool, len(required)+len(optional))
	for _, k := range required {
		known[k] = true
		if _, ok := m[k]; !ok {
			return nil, false
		}
	}
	for _, k := range optional {
		known[k] = true
	}
	for k := range m {
		if !known[k] {
			return nil, false
		}
	}
	return m, true
}

// rawString returns the value of a JSON string member. A null, a number or anything else is refused
// (json.Unmarshal would turn a null into the empty string without complaint).
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

// validName reports whether s is 1 to 64 characters of [A-Za-z0-9._-], the store's rule for account
// ids, merchant references and key ids.
func validName(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// rawArray returns the elements of a JSON array member.
func rawArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var l []json.RawMessage
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, false
	}
	return l, true
}
