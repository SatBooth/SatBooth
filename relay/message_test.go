package relay

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/nostr"
)

func i64(v int64) *int64 { return &v }

func TestParseHubFrames(t *testing.T) {
	w := testKey(t, 1)
	cases := []struct {
		name string
		in   string
		want Filter
	}{
		{"plain", `["REQ","1:",{"kinds":[23194],"#p":["` + w + `"]}]`,
			Filter{Kinds: []int{23194}, P: []string{w}}},
		{"since", `["REQ","1:",{"kinds":[23194],"since":1700000000,"#p":["` + w + `"]}]`,
			Filter{Kinds: []int{23194}, P: []string{w}, Since: i64(1700000000)}},
	}
	for _, c := range cases {
		m, err := ParseClientMessage([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if m.Label != "REQ" || m.SubID != "1:" || len(m.Filters) != 1 || !reflect.DeepEqual(m.Filters[0], c.want) {
			t.Errorf("%s: got %+v", c.name, m)
		}
	}
}

func TestParseRejects(t *testing.T) {
	h64 := strings.Repeat("a", 64)
	req := func(sub, filter string) string { return `["REQ","` + sub + `",` + filter + `]` }
	manyAuthors := `{"authors":[` + strings.TrimSuffix(strings.Repeat(`"`+h64+`",`, 65), ",") + `]}`
	okAuthors := `{"authors":[` + strings.TrimSuffix(strings.Repeat(`"`+h64+`",`, 64), ",") + `]}`
	if _, err := ParseClientMessage([]byte(req("s", okAuthors))); err != nil {
		t.Fatalf("64 authors must parse: %v", err)
	}
	cases := []struct{ name, in string }{
		{"object", `{}`},
		{"string", `"REQ"`},
		{"empty array", `[]`},
		{"label not string", `[1]`},
		{"unknown label", `["COUNT","s",{}]`},
		{"REQ no filters", `["REQ","s"]`},
		{"REQ filter not object", `["REQ","s",[]]`},
		{"sub empty", req("", `{}`)},
		{"sub 65 bytes", req(strings.Repeat("x", 65), `{}`)},
		{"sub quote byte", `["REQ","a\"b",{}]`},
		{"sub backslash byte", `["REQ","a\\b",{}]`},
		{"sub 0x1f", `["REQ","a\u001fb",{}]`},
		{"sub 0x7f", `["REQ","a` + "\x7f" + `b",{}]`},
		{"sub space", req("a b", `{}`)},
		{"sub non-ASCII", req("café", `{}`)},
		{"close bad sub", `["CLOSE",""]`},
		{"close extra", `["CLOSE","s",1]`},
		{"close missing", `["CLOSE"]`},
		{"ids", req("s", `{"ids":["`+h64+`"]}`)},
		{"search", req("s", `{"search":"x"}`)},
		{"#t", req("s", `{"#t":["x"]}`)},
		{"kinds -1", req("s", `{"kinds":[-1]}`)},
		{"kinds 65536", req("s", `{"kinds":[65536]}`)},
		{"kinds 1.0", req("s", `{"kinds":[1.0]}`)},
		{"kinds 1e2", req("s", `{"kinds":[1e2]}`)},
		{"kinds null", req("s", `{"kinds":null}`)},
		{"kinds string", req("s", `{"kinds":["1"]}`)},
		{"kinds not list", req("s", `{"kinds":1}`)},
		{"65 kinds", req("s", `{"kinds":[`+strings.TrimSuffix(strings.Repeat("1,", 65), ",")+`]}`)},
		{"since string", req("s", `{"since":"1"}`)},
		{"since leading zero", req("s", `{"since":01}`)},
		{"since negative", req("s", `{"since":-1}`)},
		{"until overflow", req("s", `{"until":9223372036854775808}`)},
		{"limit float", req("s", `{"limit":1.5}`)},
		{"limit overflow", req("s", `{"limit":2147483648}`)},
		{"65 authors", req("s", manyAuthors)},
		{"author 63 hex", req("s", `{"authors":["`+h64[:63]+`"]}`)},
		{"author 65 hex", req("s", `{"authors":["`+h64+`a"]}`)},
		{"author uppercase", req("s", `{"authors":["`+strings.Repeat("A", 64)+`"]}`)},
		{"author not hex", req("s", `{"authors":["`+strings.Repeat("g", 64)+`"]}`)},
		{"#p uppercase", req("s", `{"#p":["`+strings.Repeat("A", 64)+`"]}`)},
		{"#e short", req("s", `{"#e":["ab"]}`)},
		{"dup key", req("s", `{"kinds":[1],"kinds":[2]}`)},
		{"dup since", req("s", `{"since":1,"since":2}`)},
		{"trailing bytes", `["CLOSE","s"]x`},
		{"trailing array", `["CLOSE","s"] ["CLOSE","s"]`},
		{"truncated", `["CLOSE","s"`},
		{"EVENT not object", `["EVENT",1]`},
		{"EVENT missing", `["EVENT"]`},
		{"EVENT bad event", `["EVENT",{}]`},
		{"EVENT extra", `["EVENT",{},{}]`},
		{"AUTH bad event", `["AUTH",{"id":1}]`},
		{"AUTH string", `["AUTH","challenge"]`},
		{"empty input", ``},
	}
	for _, c := range cases {
		m, err := ParseClientMessage([]byte(c.in))
		if err == nil {
			t.Errorf("%s: accepted: %+v", c.name, m)
		}
	}
	// The caller can tell the failure kinds apart.
	if _, err := ParseClientMessage([]byte(req("", `{}`))); err != ErrBadSubID {
		t.Errorf("bad sub id error = %v", err)
	}
	m, err := ParseClientMessage([]byte(req("s", `{"ids":[]}`)))
	var ue *UnsupportedFieldError
	if !asUnsupported(err, &ue) || ue.Name != "ids" || m == nil || m.SubID != "s" {
		t.Errorf("unsupported field: %v %+v", err, m)
	}
}

// TestParseReqPartial: once the id is valid, every later REQ error returns the partial message.
func TestParseReqPartial(t *testing.T) {
	h64 := strings.Repeat("a", 64)
	n := func(k int) string { return strings.TrimSuffix(strings.Repeat(`{},`, k), ",") }
	cases := []struct{ name, in string }{
		{"bad filter", `["REQ","s",{"kinds":[-1]}]`},
		{"unsupported", `["REQ","s",{"ids":["` + h64 + `"]}]`},
		{"no filter", `["REQ","s"]`},
		{"filter not object", `["REQ","s",1]`},
		{"5 filters", `["REQ","s",` + n(5) + `]`},
		{"trailing bytes", `["REQ","s",{}]x`},
		{"missing bracket", `["REQ","s",{}`},
		{"truncated filter", `["REQ","s",{"kinds":[1`},
		{"extra after filters", `["REQ","s",{} 5]`},
	}
	for _, c := range cases {
		m, err := ParseClientMessage([]byte(c.in))
		if err == nil || m == nil || m.Label != "REQ" || m.SubID != "s" {
			t.Errorf("%s: m=%+v err=%v", c.name, m, err)
		}
	}
	// The cause is distinguishable.
	if _, err := ParseClientMessage([]byte(`["REQ","s",` + n(5) + `]`)); err != ErrTooManyFilters {
		t.Errorf("5 filters: %v", err)
	}
	if m, err := ParseClientMessage([]byte(`["REQ","s",` + n(4) + `]`)); err != nil || len(m.Filters) != 4 {
		t.Errorf("4 filters: %v", err)
	}
	// A huge frame of filters stops at the fifth.
	m, err := ParseClientMessage([]byte(`["REQ","s",` + strings.Repeat(`{},`, MaxMessage/3) + `{}]`))
	if err != ErrTooManyFilters || len(m.Filters) != MaxFiltersPerReq+1 {
		t.Errorf("huge: %v", err)
	}
	// A bad id gives no message, and the cause is ErrBadSubID.
	for _, in := range []string{`["REQ","",{}]`, `["REQ","a b",{}]`, `["REQ","` + strings.Repeat("x", 65) + `",{}]`} {
		if m, err := ParseClientMessage([]byte(in)); m != nil || err != ErrBadSubID {
			t.Errorf("%q: %v %v", in, m, err)
		}
	}
}

func asUnsupported(err error, target **UnsupportedFieldError) bool {
	u, ok := err.(*UnsupportedFieldError)
	if ok {
		*target = u
	}
	return ok
}

// TestParseControlBytes builds the subscription ids from explicit bytes and checks the bytes are there.
func TestParseControlBytes(t *testing.T) {
	for _, c := range []byte{0x00, 0x1f, 0x20, 0x22, 0x5c, 0x7f, 0x80} {
		raw := append(append([]byte(`["CLOSE","a`), c), []byte(`b"]`)...)
		if !bytes.Contains(raw, []byte{'a', c, 'b'}) {
			t.Fatal("test bytes lost")
		}
		if c == 0x22 || c == 0x5c {
			raw = append(append([]byte(`["CLOSE","a\`), c), []byte(`b"]`)...)
		}
		if _, err := ParseClientMessage(raw); err == nil {
			t.Errorf("byte %#x accepted in a subscription id", c)
		}
	}
	for _, c := range []byte{0x21, 0x23, 0x5b, 0x5d, 0x7e, ':'} {
		raw := append(append([]byte(`["CLOSE","a`), c), []byte(`b"]`)...)
		if _, err := ParseClientMessage(raw); err != nil {
			t.Errorf("byte %#x refused: %v", c, err)
		}
	}
	if _, err := ParseClientMessage([]byte(`["CLOSE","` + strings.Repeat("x", 64) + `"]`)); err != nil {
		t.Errorf("64-byte id: %v", err)
	}
}

func testEvent(t testing.TB, content string, tags [][]string) *nostr.Event {
	return &nostr.Event{
		ID: strings.Repeat("1", 64), PubKey: testKey(t, 3), CreatedAt: 1700000000, Kind: 23194,
		Tags: tags, Content: content, Sig: strings.Repeat("2", 128),
	}
}

func TestParseEventAndAuth(t *testing.T) {
	e := testEvent(t, "hi", [][]string{{"p", strings.Repeat("a", 64)}})
	for _, label := range []string{"EVENT", "AUTH"} {
		raw := `["` + label + `",` + string(eventJSON(e)) + `]`
		m, err := ParseClientMessage([]byte(raw))
		if err != nil || m.Label != label || !reflect.DeepEqual(m.Event, e) {
			t.Fatalf("%s: %v %+v", label, err, m)
		}
	}
}

func TestParseSizes(t *testing.T) {
	// An event of exactly MaxEventJSON bytes parses; one byte more does not.
	e := testEvent(t, "", nil)
	e.Content = strings.Repeat("x", nostr.MaxEventJSON-len(eventJSON(e)))
	if n := len(eventJSON(e)); n != nostr.MaxEventJSON {
		t.Fatalf("size %d", n)
	}
	if _, err := ParseClientMessage([]byte(`["EVENT",` + string(eventJSON(e)) + `]`)); err != nil {
		t.Errorf("max event: %v", err)
	}
	e.Content += "x"
	if _, err := ParseClientMessage([]byte(`["EVENT",` + string(eventJSON(e)) + `]`)); err == nil {
		t.Error("oversize event accepted")
	}
	// A REQ of exactly MaxMessage bytes parses.
	req := `["REQ","s",{"kinds":[1]}` + `]`
	pad := strings.Repeat(" ", MaxMessage-len(req))
	raw := `["REQ","s",{"kinds":[1]}` + pad + `]`
	if len(raw) != MaxMessage {
		t.Fatalf("len %d", len(raw))
	}
	if _, err := ParseClientMessage([]byte(raw)); err != nil {
		t.Errorf("MaxMessage REQ: %v", err)
	}
}

// encodeMessage is a test-only encoder: the inverse of ParseClientMessage for accepted messages.
func encodeMessage(m *ClientMessage) []byte {
	var b []byte
	b = append(b, `["`+m.Label+`"`...)
	switch m.Label {
	case "EVENT", "AUTH":
		b = append(b, ',')
		b = append(b, eventJSON(m.Event)...)
	case "CLOSE":
		b = append(b, ',')
		b = append(b, quote(m.SubID)...)
	case "REQ":
		b = append(b, ',')
		b = append(b, quote(m.SubID)...)
		for _, f := range m.Filters {
			b = append(b, ',')
			b = append(b, encodeFilter(&f)...)
		}
	}
	return append(b, ']')
}

func encodeFilter(f *Filter) []byte {
	var parts []string
	strs := func(name string, l []string) {
		if l == nil {
			return
		}
		q := make([]string, len(l))
		for i, s := range l {
			q[i] = string(quote(s))
		}
		parts = append(parts, fmt.Sprintf(`%q:[%s]`, name, strings.Join(q, ",")))
	}
	if f.Kinds != nil {
		q := make([]string, len(f.Kinds))
		for i, k := range f.Kinds {
			q[i] = fmt.Sprint(k)
		}
		parts = append(parts, `"kinds":[`+strings.Join(q, ",")+`]`)
	}
	strs("authors", f.Authors)
	strs("#p", f.P)
	strs("#e", f.E)
	if f.Since != nil {
		parts = append(parts, fmt.Sprintf(`"since":%d`, *f.Since))
	}
	if f.Until != nil {
		parts = append(parts, fmt.Sprintf(`"until":%d`, *f.Until))
	}
	if f.Limit != nil {
		parts = append(parts, fmt.Sprintf(`"limit":%d`, *f.Limit))
	}
	return []byte("{" + strings.Join(parts, ",") + "}")
}
