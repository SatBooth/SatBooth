package nostr

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func hex64(c byte) string  { return strings.Repeat(string(rune(c)), 64) }
func hex128(c byte) string { return strings.Repeat(string(rune(c)), 128) }

// payload returns a syntactically valid NIP-44 base64 string of n characters (n%4==0, n>=4).
func payload(n int) string { return strings.Repeat("A", n-2) + "==" }

func validRaw(id, pub, sig, content string, createdAt, kind string, tags string) string {
	return fmt.Sprintf(`{"id":%q,"pubkey":%q,"created_at":%s,"kind":%s,"tags":%s,"content":%s,"sig":%q}`,
		id, pub, createdAt, kind, tags, content, sig)
}

func baseRaw() string {
	return validRaw(hex64('1'), hex64('a'), hex128('c'), `"AQ=="`, "1700000000", "23197", `[["p","`+hex64('b')+`"]]`)
}

func TestSerializeCanonical(t *testing.T) {
	e := &Event{PubKey: hex64('a'), CreatedAt: 1700000000, Kind: 23197,
		Tags: [][]string{{"p", hex64('b')}}, Content: `AQ"\`}
	got, err := Serialize(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `[0,"` + hex64('a') + `",1700000000,23197,[["p","` + hex64('b') + `"]],"AQ\"\\"]`
	if string(got) != want {
		t.Fatalf("got %s", got)
	}
}

func TestSerializeEmptyTags(t *testing.T) {
	got, err := Serialize(&Event{PubKey: hex64('a'), Kind: 22242})
	if err != nil {
		t.Fatal(err)
	}
	want := `[0,"` + hex64('a') + `",0,22242,[],""]`
	if string(got) != want {
		t.Fatalf("got %s", got)
	}
}

func TestSerializeRefusesNonPrintable(t *testing.T) {
	bad := []string{" ", "é", "\xef\xbf\xbd", "\xff", " ", " "}
	for b := 0; b < 0x20; b++ {
		bad = append(bad, string([]byte{byte(b)}))
	}
	bad = append(bad, "\x7f")
	for _, s := range bad {
		if _, err := Serialize(&Event{PubKey: hex64('a'), Content: "a" + s}); err == nil {
			t.Errorf("content %q accepted", s)
		}
		if _, err := Serialize(&Event{PubKey: hex64('a'), Tags: [][]string{{"p", s}}}); err == nil {
			t.Errorf("tag value %q accepted", s)
		}
		if _, err := Serialize(&Event{PubKey: hex64('a'), Tags: [][]string{{s, "x"}}}); err == nil {
			t.Errorf("tag name %q accepted", s)
		}
		if _, err := Serialize(&Event{PubKey: s}); err == nil {
			t.Errorf("pubkey %q accepted", s)
		}
	}
	if _, err := Serialize(nil); err == nil {
		t.Error("nil event accepted")
	}
}

func TestParseValid(t *testing.T) {
	e, err := ParseEvent([]byte(baseRaw()))
	if err != nil {
		t.Fatal(err)
	}
	want := &Event{ID: hex64('1'), PubKey: hex64('a'), CreatedAt: 1700000000, Kind: 23197,
		Tags: [][]string{{"p", hex64('b')}}, Content: "AQ==", Sig: hex128('c')}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("got %+v", e)
	}
}

func TestParseRejectsDuplicateKey(t *testing.T) {
	raw := strings.Replace(baseRaw(), `"kind":23197,`, `"kind":23194,"kind":23195,`, 1)
	if _, err := ParseEvent([]byte(raw)); err == nil {
		t.Fatal("duplicate key accepted")
	}
}

func TestParseRejectsUnknownAndMissingKeys(t *testing.T) {
	raw := strings.Replace(baseRaw(), `{`, `{"foo":1,`, 1)
	if _, err := ParseEvent([]byte(raw)); err == nil {
		t.Fatal("unknown key accepted")
	}
	for _, k := range []string{"id", "pubkey", "created_at", "kind", "tags", "content", "sig"} {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(baseRaw()), &m); err != nil {
			t.Fatal(err)
		}
		delete(m, k)
		b, _ := json.Marshal(m)
		if _, err := ParseEvent(b); err == nil {
			t.Errorf("missing %s accepted", k)
		}
	}
}

func TestParseRejectsNonIntegerNumbers(t *testing.T) {
	for _, ca := range []string{"1.0", "1e9", "-1", `"1"`, "01", "1.5", "99999999999999999999", "null", "true", "-0", "+1"} {
		raw := strings.Replace(baseRaw(), `"created_at":1700000000`, `"created_at":`+ca, 1)
		if _, err := ParseEvent([]byte(raw)); err == nil {
			t.Errorf("created_at %s accepted", ca)
		}
	}
	for _, k := range []string{"23194.0", "2.3194e4", "-23194", `"23194"`, "99999999999999999999"} {
		raw := strings.Replace(baseRaw(), `"kind":23197`, `"kind":`+k, 1)
		if _, err := ParseEvent([]byte(raw)); err == nil {
			t.Errorf("kind %s accepted", k)
		}
	}
	raw := strings.Replace(baseRaw(), `"created_at":1700000000`, `"created_at":0`, 1)
	if _, err := ParseEvent([]byte(raw)); err != nil {
		t.Errorf("created_at 0 refused: %v", err)
	}
}

func TestParseRejectsBadHex(t *testing.T) {
	cases := map[string]string{
		"upper id":     validRaw(strings.Repeat("A", 64), hex64('a'), hex128('c'), `"x"`, "1", "1", `[]`),
		"upper pubkey": validRaw(hex64('1'), strings.Repeat("A", 64), hex128('c'), `"x"`, "1", "1", `[]`),
		"upper sig":    validRaw(hex64('1'), hex64('a'), strings.Repeat("C", 128), `"x"`, "1", "1", `[]`),
		"short id":     validRaw(hex64('1')[:63], hex64('a'), hex128('c'), `"x"`, "1", "1", `[]`),
		"long pubkey":  validRaw(hex64('1'), hex64('a')+"a", hex128('c'), `"x"`, "1", "1", `[]`),
		"short sig":    validRaw(hex64('1'), hex64('a'), hex64('c'), `"x"`, "1", "1", `[]`),
		"non-hex id":   validRaw(strings.Repeat("g", 64), hex64('a'), hex128('c'), `"x"`, "1", "1", `[]`),
	}
	for name, raw := range cases {
		if _, err := ParseEvent([]byte(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestParseRejectsMalformedStructure(t *testing.T) {
	good := baseRaw()
	cases := map[string]string{
		"empty":         ``,
		"array":         `[]`,
		"null":          `null`,
		"trailing":      good + ` {}`,
		"trailing junk": good + `x`,
		"tags null":     strings.Replace(good, `[["p","`+hex64('b')+`"]]`, `null`, 1),
		"tags nested":   strings.Replace(good, `[["p","`+hex64('b')+`"]]`, `[["p",["x"]]]`, 1),
		"tag number":    strings.Replace(good, `[["p","`+hex64('b')+`"]]`, `[["p",1]]`, 1),
		"tag null":      strings.Replace(good, `[["p","`+hex64('b')+`"]]`, `[null]`, 1),
		"tag elem null": strings.Replace(good, `[["p","`+hex64('b')+`"]]`, `[["p",null]]`, 1),
		"content num":   strings.Replace(good, `"content":"AQ=="`, `"content":1`, 1),
		"truncated":     good[:len(good)-1],
		"deep":          strings.Replace(good, `[["p","`+hex64('b')+`"]]`, strings.Repeat("[", 10000), 1),
	}
	for name, raw := range cases {
		if _, err := ParseEvent([]byte(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Every string field must be bytes 0x20-0x7e after JSON decoding.
func TestParseRefusesNonPrintableStrings(t *testing.T) {
	contents := []string{
		// JSON escapes (decoded to non-printable bytes)
		`"a\u0001b"`, `"a\u0009b"`, `"a\tb"`, `"a\nb"`, `"a\u007fb"`, `"\u00a0"`, `"\u00e9"`, `"\u2028"`, `"\u2029"`,
		`"\ud800"`, `"\ufffd"`, `"\u0000"`, `"\udbff\udfff"`,
		// raw bytes (UTF-8 and invalid UTF-8)
		"\"a\xffb\"", "\"a\xc3\x28b\"", "\"\xc3\xa9\"", "\"\xe2\x80\xa8\"", "\"\xe2\x80\xa9\"", "\"\xef\xbf\xbd\"", "\"\xc2\xa0\"",
	}
	// every raw byte 0x80-0xff on its own, and every JSON \u00XX escape for 0x00-0x1f and 0x7f-0xff
	for b := 0x80; b <= 0xff; b++ {
		contents = append(contents, "\"x"+string([]byte{byte(b)})+"\"")
	}
	for b := 0; b <= 0xff; b++ {
		if b < 0x20 || b >= 0x7f {
			contents = append(contents, fmt.Sprintf(`"\u%04x"`, b))
		}
	}
	for _, c := range contents {
		raw := validRaw(hex64('1'), hex64('a'), hex128('c'), c, "1", "23197", `[["p","`+hex64('b')+`"]]`)
		if _, err := ParseEvent([]byte(raw)); err == nil {
			t.Errorf("content %q accepted", c)
		}
		raw = validRaw(hex64('1'), hex64('a'), hex128('c'), `"x"`, "1", "23197", `[["p",`+c+`]]`)
		if _, err := ParseEvent([]byte(raw)); err == nil {
			t.Errorf("tag value %q accepted", c)
		}
		raw = validRaw(hex64('1'), hex64('a'), hex128('c'), `"x"`, "1", "23197", `[[`+c+`,"v"]]`)
		if _, err := ParseEvent([]byte(raw)); err == nil {
			t.Errorf("tag name %q accepted", c)
		}
	}
	// id, pubkey, sig containing such bytes
	for _, bad := range []string{"é", "\x01", " ", " "} {
		for _, raw := range []string{
			validRaw(bad+hex64('1')[1:], hex64('a'), hex128('c'), `"x"`, "1", "1", `[]`),
			validRaw(hex64('1'), bad+hex64('a')[1:], hex128('c'), `"x"`, "1", "1", `[]`),
			validRaw(hex64('1'), hex64('a'), bad+hex128('c')[1:], `"x"`, "1", "1", `[]`),
		} {
			if _, err := ParseEvent([]byte(raw)); err == nil {
				t.Errorf("bad hex field %q accepted", bad)
			}
		}
	}
	// escaped quote and backslash are printable and fine
	raw := validRaw(hex64('1'), hex64('a'), hex128('c'), `"a\"\\b"`, "1", "23197", `[]`)
	e, err := ParseEvent([]byte(raw))
	if err != nil || e.Content != `a"\b` {
		t.Errorf("printable escapes: %v %v", e, err)
	}
}

func TestParseSizeCap(t *testing.T) {
	pad := func(total int) []byte {
		base := validRaw(hex64('1'), hex64('a'), hex128('c'), `""`, "1", "1", `[]`)
		fill := total - len(base)
		raw := validRaw(hex64('1'), hex64('a'), hex128('c'), `"`+strings.Repeat("x", fill)+`"`, "1", "1", `[]`)
		if len(raw) != total {
			t.Fatalf("len %d want %d", len(raw), total)
		}
		return []byte(raw)
	}
	if _, err := ParseEvent(pad(MaxEventJSON)); err != nil {
		t.Errorf("max size refused: %v", err)
	}
	// Not even valid JSON: must be refused for size, before decoding.
	big := make([]byte, MaxEventJSON+1)
	_, err := ParseEvent(big)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("oversize: %v", err)
	}
	if _, err := ParseEvent(pad(MaxEventJSON + 1)); err == nil {
		t.Error("max+1 accepted")
	}
}

func TestIDOverCanonicalForm(t *testing.T) {
	// The content "AQ==" written with JSON escapes (\u0041 = A, \u0051 = Q, \u003d = =).
	a := strings.Replace(baseRaw(), `"AQ=="`, `"\u0041\u0051\u003d\u003d"`, 1)
	if a == baseRaw() {
		t.Fatal("replacement did not happen")
	}
	ea, err := ParseEvent([]byte(a))
	if err != nil {
		t.Fatal(err)
	}
	if ea.Content != "AQ==" {
		t.Fatalf("content %q", ea.Content)
	}
	eb, err := ParseEvent([]byte(baseRaw()))
	if err != nil {
		t.Fatal(err)
	}
	ia, err := ComputeID(ea)
	if err != nil {
		t.Fatal(err)
	}
	ib, _ := ComputeID(eb)
	if ia != ib {
		t.Fatalf("ids differ: %s %s", ia, ib)
	}
	// Pinned: SHA-256 of exactly this canonical serialisation.
	canon := `[0,"` + hex64('a') + `",1700000000,23197,[["p","` + hex64('b') + `"]],"AQ=="]`
	const pinned = "ff5dc0ff9fe6aa12afe643257029f61aeecf6e5f276e9e1e2866ff7ede2dfdf1"
	ser, _ := Serialize(ea)
	if string(ser) != canon {
		t.Fatalf("serialisation %s", ser)
	}
	if ia != pinned {
		t.Fatalf("id %s, want pinned %s", ia, pinned)
	}
	if _, err := ComputeID(&Event{PubKey: "é"}); err == nil {
		t.Error("ComputeID of non-printable accepted")
	}
}

func TestParseKindRange(t *testing.T) {
	for k, ok := range map[string]bool{"0": true, "65535": true, "65536": false, "2147483647": false, "2147483648": false} {
		raw := strings.Replace(baseRaw(), `"kind":23197`, `"kind":`+k, 1)
		_, err := ParseEvent([]byte(raw))
		if (err == nil) != ok {
			t.Errorf("kind %s: err=%v", k, err)
		}
	}
}
