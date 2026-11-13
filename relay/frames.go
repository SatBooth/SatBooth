package relay

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/satbooth/satbooth/nostr"
)

// quote returns s as a JSON string without HTML escaping.
func quote(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// eventJSON returns e as compact JSON with keys id, pubkey, created_at, kind, tags, content, sig.
// tags is [] for nil or empty tags.
func eventJSON(e *nostr.Event) []byte {
	b := make([]byte, 0, 256+len(e.Content))
	b = append(b, `{"id":`...)
	b = append(b, quote(e.ID)...)
	b = append(b, `,"pubkey":`...)
	b = append(b, quote(e.PubKey)...)
	b = append(b, `,"created_at":`...)
	b = strconv.AppendInt(b, e.CreatedAt, 10)
	b = append(b, `,"kind":`...)
	b = strconv.AppendInt(b, int64(e.Kind), 10)
	b = append(b, `,"tags":[`...)
	for i, t := range e.Tags {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '[')
		for j, v := range t {
			if j > 0 {
				b = append(b, ',')
			}
			b = append(b, quote(v)...)
		}
		b = append(b, ']')
	}
	b = append(b, `],"content":`...)
	b = append(b, quote(e.Content)...)
	b = append(b, `,"sig":`...)
	b = append(b, quote(e.Sig)...)
	return append(b, '}')
}

// FrameEvent returns ["EVENT",subID,event].
func FrameEvent(subID string, e *nostr.Event) []byte {
	b := append([]byte(`["EVENT",`), quote(subID)...)
	b = append(b, ',')
	b = append(b, eventJSON(e)...)
	return append(b, ']')
}

// FrameOK returns ["OK",id,ok,msg].
func FrameOK(id string, ok bool, msg string) []byte {
	b := append([]byte(`["OK",`), quote(id)...)
	if ok {
		b = append(b, ",true,"...)
	} else {
		b = append(b, ",false,"...)
	}
	b = append(b, quote(msg)...)
	return append(b, ']')
}

// FrameEOSE returns ["EOSE",subID].
func FrameEOSE(subID string) []byte {
	return append(append([]byte(`["EOSE",`), quote(subID)...), ']')
}

// FrameClosed returns ["CLOSED",subID,msg].
func FrameClosed(subID, msg string) []byte {
	b := append([]byte(`["CLOSED",`), quote(subID)...)
	b = append(b, ',')
	b = append(b, quote(msg)...)
	return append(b, ']')
}

// FrameNotice returns ["NOTICE",msg].
func FrameNotice(msg string) []byte {
	return append(append([]byte(`["NOTICE",`), quote(msg)...), ']')
}

// FrameAuth returns ["AUTH",challenge].
func FrameAuth(challenge string) []byte {
	return append(append([]byte(`["AUTH",`), quote(challenge)...), ']')
}
