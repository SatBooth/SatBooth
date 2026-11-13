package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"github.com/satbooth/satbooth/nostr"
)

// ClientMessage is one parsed client frame. Label is EVENT, REQ, CLOSE or AUTH.
type ClientMessage struct {
	Label   string
	Event   *nostr.Event // EVENT, AUTH
	SubID   string       // REQ, CLOSE
	Filters []Filter     // REQ, at least one
}

// maxListValues is the most values one filter list may hold.
const maxListValues = 64

// ErrBadSubID marks a REQ or CLOSE whose subscription id is not 1-64 bytes of 0x21-0x7e without '"' or '\'.
var ErrBadSubID = errors.New("invalid: bad subscription id")

// UnsupportedFieldError marks a filter key outside kinds, authors, #p, #e, since, until and limit.
// Name is the key if it is short printable ASCII, otherwise "?".
type UnsupportedFieldError struct{ Name string }

func (e *UnsupportedFieldError) Error() string {
	return "invalid: unsupported filter field " + e.Name
}

// ErrTooManyFilters marks a REQ with more than MaxFiltersPerReq filters. The parser stops reading at the
// first filter over the limit, so a frame of empty filters cannot make it allocate in proportion to its size.
var ErrTooManyFilters = errors.New("blocked: too many filters")

var errMalformedMessage = errors.New("invalid: malformed message")

func validSubID(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x21 || c > 0x7e || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

func fieldName(s string) string {
	if len(s) == 0 || len(s) > 32 {
		return "?"
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return "?"
		}
	}
	return s
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < 64; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func tokDelim(d *json.Decoder, want json.Delim) error {
	t, err := d.Token()
	if err != nil {
		return errMalformedMessage
	}
	if got, ok := t.(json.Delim); !ok || got != want {
		return errMalformedMessage
	}
	return nil
}

func tokString(d *json.Decoder) (string, error) {
	t, err := d.Token()
	if err != nil {
		return "", errMalformedMessage
	}
	s, ok := t.(string)
	if !ok {
		return "", errMalformedMessage
	}
	return s, nil
}

func tokUint(d *json.Decoder, maxVal int64) (int64, error) {
	t, err := d.Token()
	if err != nil {
		return 0, errMalformedMessage
	}
	n, ok := t.(json.Number)
	if !ok {
		return 0, errMalformedMessage
	}
	s := n.String()
	if !(s == "0" || (s != "" && s[0] >= '1' && s[0] <= '9')) {
		return 0, errors.New("invalid: number is not a canonical non-negative integer")
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("invalid: number is not a canonical non-negative integer")
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v > maxVal {
		return 0, errors.New("invalid: number out of range")
	}
	return v, nil
}

func hexList(d *json.Decoder) ([]string, error) {
	if err := tokDelim(d, '['); err != nil {
		return nil, err
	}
	out := []string{}
	for d.More() {
		s, err := tokString(d)
		if err != nil {
			return nil, err
		}
		if !isHex64(s) {
			return nil, errors.New("invalid: filter value is not 64 lowercase hex characters")
		}
		if len(out) == maxListValues {
			return nil, errors.New("invalid: filter list holds more than 64 values")
		}
		out = append(out, s)
	}
	return out, tokDelim(d, ']')
}

func kindList(d *json.Decoder) ([]int, error) {
	if err := tokDelim(d, '['); err != nil {
		return nil, err
	}
	out := []int{}
	for d.More() {
		k, err := tokUint(d, 65535)
		if err != nil {
			return nil, err
		}
		if len(out) == maxListValues {
			return nil, errors.New("invalid: filter list holds more than 64 values")
		}
		out = append(out, int(k))
	}
	return out, tokDelim(d, ']')
}

func readFilter(d *json.Decoder) (Filter, error) {
	var f Filter
	if err := tokDelim(d, '{'); err != nil {
		return f, err
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := tokString(d)
		if err != nil {
			return f, err
		}
		if seen[key] {
			return f, errors.New("invalid: duplicate filter key")
		}
		seen[key] = true
		switch key {
		case "kinds":
			f.Kinds, err = kindList(d)
		case "authors":
			f.Authors, err = hexList(d)
		case "#p":
			f.P, err = hexList(d)
		case "#e":
			f.E, err = hexList(d)
		case "since", "until":
			var v int64
			v, err = tokUint(d, 1<<63-1)
			if key == "since" {
				f.Since = &v
			} else {
				f.Until = &v
			}
		case "limit":
			var v int64
			v, err = tokUint(d, 1<<31-1)
			n := int(v)
			f.Limit = &n
		default:
			return f, &UnsupportedFieldError{Name: fieldName(key)}
		}
		if err != nil {
			return f, err
		}
	}
	return f, tokDelim(d, '}')
}

// ParseClientMessage strictly parses one client frame: a JSON array, no trailing bytes, labelled
// EVENT, REQ, CLOSE or AUTH. It does not enforce a size limit (the reader does) and does not verify
// signatures. A REQ carries 1 to MaxFiltersPerReq filters; reading stops at the first one over the
// limit and the error is ErrTooManyFilters.
//
// Once a REQ's subscription id is valid, every later error (bad filter, too many filters, trailing
// bytes, a missing "]", truncation) returns the partial message with Label and SubID set, so the caller
// can answer CLOSED. A bad id is reported as ErrBadSubID (with a nil message) and an unknown filter key
// as *UnsupportedFieldError.
func ParseClientMessage(b []byte) (*ClientMessage, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := tokDelim(d, '['); err != nil {
		return nil, err
	}
	label, err := tokString(d)
	if err != nil {
		return nil, err
	}
	m := &ClientMessage{Label: label}
	switch label {
	case "EVENT", "AUTH":
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, errMalformedMessage
		}
		if m.Event, err = nostr.ParseEvent(raw); err != nil {
			return nil, err
		}
	case "CLOSE":
		if m.SubID, err = tokString(d); err != nil {
			return nil, err
		}
		if !validSubID(m.SubID) {
			return nil, ErrBadSubID
		}
	case "REQ":
		if m.SubID, err = tokString(d); err != nil {
			return nil, err
		}
		if !validSubID(m.SubID) {
			return nil, ErrBadSubID
		}
		for d.More() {
			f, err := readFilter(d)
			if err != nil {
				return m, err
			}
			m.Filters = append(m.Filters, f)
			if len(m.Filters) > MaxFiltersPerReq {
				return m, ErrTooManyFilters
			}
		}
		if len(m.Filters) == 0 {
			return m, errors.New("invalid: REQ has no filter")
		}
	default:
		return nil, errors.New("invalid: unknown message label")
	}
	// A REQ keeps its partial message from here on, so the caller can answer CLOSED.
	partial := m
	if label != "REQ" {
		partial = nil
	}
	if err := tokDelim(d, ']'); err != nil {
		return partial, err
	}
	if _, err := d.Token(); err != io.EOF {
		return partial, errors.New("invalid: data after message")
	}
	return m, nil
}
