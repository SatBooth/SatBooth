package nostr

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
)

// MaxEventJSON is the largest event, in bytes of JSON, that ParseEvent looks at.
const MaxEventJSON = 131072

const maxKind = 65535

// Event is a Nostr event (NIP-01).
type Event struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

// printable reports whether s holds only bytes 0x20-0x7e.
func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// lowerHex reports whether s is exactly n lowercase hex digits.
func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < n; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// canonicalInt reports whether s matches ^(0|[1-9][0-9]*)$.
func canonicalInt(s string) bool {
	if s == "0" {
		return true
	}
	if s == "" || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// appendString appends s as a JSON string. s must already be printable ASCII; only '"' and '\'
// need escaping in that alphabet, and NIP-01 prescribes exactly those escapes.
func appendString(b []byte, s string) ([]byte, error) {
	if !printable(s) {
		return nil, errors.New("nostr: string holds a byte outside 0x20-0x7e")
	}
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b = append(b, '\\')
		}
		b = append(b, s[i])
	}
	return append(b, '"'), nil
}

// Serialize returns the NIP-01 array [0,pubkey,created_at,kind,tags,content]. It refuses any string
// with a byte outside 0x20-0x7e, so the serialisation (and the id) is the same in every implementation.
func Serialize(e *Event) ([]byte, error) {
	if e == nil {
		return nil, errors.New("nostr: nil event")
	}
	b := make([]byte, 0, 128+len(e.Content))
	b = append(b, "[0,"...)
	var err error
	if b, err = appendString(b, e.PubKey); err != nil {
		return nil, err
	}
	b = append(b, ',')
	b = strconv.AppendInt(b, e.CreatedAt, 10)
	b = append(b, ',')
	b = strconv.AppendInt(b, int64(e.Kind), 10)
	b = append(b, ",["...)
	for i, tag := range e.Tags {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '[')
		for j, v := range tag {
			if j > 0 {
				b = append(b, ',')
			}
			if b, err = appendString(b, v); err != nil {
				return nil, err
			}
		}
		b = append(b, ']')
	}
	b = append(b, "],"...)
	if b, err = appendString(b, e.Content); err != nil {
		return nil, err
	}
	return append(b, ']'), nil
}

// ComputeID returns the lowercase hex SHA-256 of Serialize(e).
func ComputeID(e *Event) (string, error) {
	s, err := Serialize(e)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(s)
	return hex.EncodeToString(sum[:]), nil
}

var (
	errMalformed = errors.New("nostr: malformed event JSON")
	errNonASCII  = errors.New("nostr: event string holds a byte outside 0x20-0x7e")
)

func newDecoder(raw []byte) *json.Decoder {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return d
}

func expectDelim(d *json.Decoder, want json.Delim) error {
	t, err := d.Token()
	if err != nil {
		return errMalformed
	}
	if got, ok := t.(json.Delim); !ok || got != want {
		return errMalformed
	}
	return nil
}

// readString reads the next token, which must be a printable-ASCII string.
func readString(d *json.Decoder) (string, error) {
	t, err := d.Token()
	if err != nil {
		return "", errMalformed
	}
	s, ok := t.(string)
	if !ok {
		return "", errMalformed
	}
	if !printable(s) {
		return "", errNonASCII
	}
	return s, nil
}

func readInt(d *json.Decoder, bits int) (int64, error) {
	t, err := d.Token()
	if err != nil {
		return 0, errMalformed
	}
	n, ok := t.(json.Number)
	if !ok || !canonicalInt(n.String()) {
		return 0, errors.New("nostr: number is not a canonical non-negative integer")
	}
	v, err := strconv.ParseInt(n.String(), 10, bits)
	if err != nil {
		return 0, errors.New("nostr: number out of range")
	}
	return v, nil
}

func readTags(d *json.Decoder) ([][]string, error) {
	if err := expectDelim(d, '['); err != nil {
		return nil, err
	}
	tags := [][]string{}
	for d.More() {
		if err := expectDelim(d, '['); err != nil {
			return nil, err
		}
		tag := []string{}
		for d.More() {
			s, err := readString(d)
			if err != nil {
				return nil, err
			}
			tag = append(tag, s)
		}
		if err := expectDelim(d, ']'); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	if err := expectDelim(d, ']'); err != nil {
		return nil, err
	}
	return tags, nil
}

// ParseEvent strictly parses one event. It refuses input over MaxEventJSON bytes before decoding,
// duplicate, unknown or missing keys, numbers that are not canonical non-negative integers, id/pubkey
// that are not 64 lowercase hex digits, kind above 65535, sig that is not 128, and any string (including tag values and
// content) with a byte outside 0x20-0x7e after JSON decoding. It does not check the signature or the id.
func ParseEvent(raw []byte) (*Event, error) {
	if len(raw) > MaxEventJSON {
		return nil, errors.New("nostr: event too large")
	}
	d := newDecoder(raw)
	if err := expectDelim(d, '{'); err != nil {
		return nil, err
	}
	var e Event
	seen := map[string]bool{}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, errMalformed
		}
		key, ok := t.(string)
		if !ok {
			return nil, errMalformed
		}
		if seen[key] {
			return nil, errors.New("nostr: duplicate key in event")
		}
		switch key {
		case "id":
			e.ID, err = readString(d)
		case "pubkey":
			e.PubKey, err = readString(d)
		case "content":
			e.Content, err = readString(d)
		case "sig":
			e.Sig, err = readString(d)
		case "created_at":
			e.CreatedAt, err = readInt(d, 64)
		case "kind":
			var k int64
			k, err = readInt(d, 32)
			if err == nil && k > maxKind {
				err = errors.New("nostr: kind out of range")
			}
			e.Kind = int(k)
		case "tags":
			e.Tags, err = readTags(d)
		default:
			return nil, errors.New("nostr: unknown key in event")
		}
		if err != nil {
			return nil, err
		}
		seen[key] = true
	}
	if err := expectDelim(d, '}'); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("nostr: data after event")
	}
	if len(seen) != 7 {
		return nil, errors.New("nostr: missing key in event")
	}
	if !lowerHex(e.ID, 64) || !lowerHex(e.PubKey, 64) || !lowerHex(e.Sig, 128) {
		return nil, errors.New("nostr: id, pubkey or sig is not lowercase hex of the right length")
	}
	return &e, nil
}
