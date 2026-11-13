package nostr

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

var oddPieces = []string{"é", " ", " ", "�", "\xff", "\xc3\x28"}

func randString(r *rand.Rand, printableOnly bool) string {
	n := r.IntN(301)
	var sb strings.Builder
	for i := 0; i < n; i++ {
		switch {
		case printableOnly:
			sb.WriteByte(byte(0x20 + r.IntN(0x5f)))
		case r.IntN(20) == 0:
			sb.WriteString(oddPieces[r.IntN(len(oddPieces))])
		default:
			sb.WriteByte(byte(r.IntN(256)))
		}
	}
	return sb.String()
}

func TestSerializeProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	okCount := 0
	for i := 0; i < 10000; i++ {
		po := r.IntN(2) == 0
		e := &Event{PubKey: randString(r, po), CreatedAt: r.Int64N(1 << 40), Kind: r.IntN(70000), Content: randString(r, po)}
		for j := r.IntN(4); j > 0; j-- {
			tag := []string{}
			for k := r.IntN(4); k > 0; k-- {
				tag = append(tag, randString(r, po))
			}
			e.Tags = append(e.Tags, tag)
		}
		wantOK := printable(e.PubKey) && printable(e.Content)
		for _, tg := range e.Tags {
			for _, v := range tg {
				wantOK = wantOK && printable(v)
			}
		}
		out, err := Serialize(e)
		if (err == nil) != wantOK {
			t.Fatalf("iteration %d: err=%v wantOK=%v", i, err, wantOK)
		}
		if err != nil {
			continue
		}
		okCount++
		for _, c := range out {
			if c < 0x20 || c > 0x7e {
				t.Fatalf("iteration %d: output byte %#x", i, c)
			}
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(out, &arr); err != nil || len(arr) != 6 {
			t.Fatalf("iteration %d: unmarshal: %v", i, err)
		}
		var zero int
		var pk, content string
		var ca int64
		var kind int
		var tags [][]string
		for idx, dst := range []any{&zero, &pk, &ca, &kind, &tags, &content} {
			if err := json.Unmarshal(arr[idx], dst); err != nil {
				t.Fatalf("iteration %d: field %d: %v", i, idx, err)
			}
		}
		if zero != 0 || pk != e.PubKey || ca != e.CreatedAt || kind != e.Kind || content != e.Content {
			t.Fatalf("iteration %d: scalar mismatch", i)
		}
		if len(tags) != len(e.Tags) || (len(tags) > 0 && !reflect.DeepEqual(tags, e.Tags)) {
			t.Fatalf("iteration %d: tags mismatch", i)
		}
	}
	if okCount < 1000 {
		t.Fatalf("only %d successful serialisations; generator too hostile", okCount)
	}
}

func seedEvents() []*Event {
	p, enc := []string{"p", hex64('b')}, []string{"encryption", "nip44_v2"}
	return []*Event{
		ev(KindNWCInfo, "get_info pay_invoice", enc, p, []string{"notifications", "payment_received"}),
		ev(KindNWCRequest, payload(132), p, enc, []string{"expiration", "1700000000"}),
		ev(KindNWCResponse, payload(132), p, []string{"e", hex64('d')}),
		ev(KindNWCNotification, payload(132), p),
		ev(KindNWCNotificationNIP04, "abc?iv=def", p),
		ev(KindAuth, "", []string{"relay", "wss://r.example"}, []string{"challenge", strings.Repeat("a", 32)}),
	}
}

func FuzzParseEvent(f *testing.F) {
	for _, e := range seedEvents() {
		b, _ := json.Marshal(e)
		f.Add(b)
	}
	f.Add([]byte(strings.Replace(baseRaw(), `"kind":23197,`, `"kind":23194,"kind":23195,`, 1)))
	f.Add(make([]byte, MaxEventJSON+1))
	f.Add([]byte(strings.Repeat("[", 5000)))
	f.Add([]byte(baseRaw()))
	f.Add([]byte(""))
	f.Add(paddedRaw(MaxEventJSON))
	f.Add(paddedRaw(MaxEventJSON + 1))
	// At the limit with content json.Marshal escapes: the marshalled form is over MaxEventJSON.
	f.Add([]byte(strings.ReplaceAll(string(paddedRaw(MaxEventJSON)), "x", "<")))
	f.Add([]byte(strings.Replace(baseRaw(), `"AQ=="`, "\"a\u2028\u2029b\"", 1)))
	f.Add([]byte(strings.Replace(baseRaw(), `"AQ=="`, "\"a\xe2\x80\xa8\xe2\x80\xa9b\"", 1)))
	sweep := make([]byte, 0, 256)
	for b := 0; b < 256; b++ {
		sweep = append(sweep, byte(b))
	}
	f.Add([]byte(strings.Replace(baseRaw(), `"AQ=="`, `"`+string(sweep)+`"`, 1)))
	f.Fuzz(func(t *testing.T, raw []byte) {
		e, err := ParseEvent(raw)
		if err != nil {
			return
		}
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		// json.Marshal escapes <, >, & and U+2028/U+2029 as \uXXXX, so an event near MaxEventJSON can grow past it
		// when marshalled again. ParseEvent refuses that by its size rule alone; the round trip is checked whenever
		// the marshalled form is within the limit.
		if len(b) > MaxEventJSON {
			return
		}
		e2, err := ParseEvent(b)
		if err != nil {
			t.Fatalf("re-parse refused: %v", err)
		}
		if !reflect.DeepEqual(e, e2) {
			t.Fatalf("round trip differs: %+v vs %+v", e, e2)
		}
		if _, err := ComputeID(e); err != nil {
			t.Fatalf("accepted event cannot be serialised: %v", err)
		}
		_ = ValidateNWC(e)
	})
}

func FuzzValidateNWC(f *testing.F) {
	f.Add(KindNWCInfo, "get_info", "encryption", "nip44_v2", "p", hex64('b'))
	f.Add(KindNWCRequest, payload(132), "p", hex64('b'), "expiration", "5")
	f.Add(KindAuth, "", "relay", "wss://r", "challenge", strings.Repeat("a", 32))
	f.Add(KindNWCNotificationNIP04, "x", "p", hex64('b'), "e", hex64('b'))
	f.Add(KindNWCRequest, payload(87472), "p", hex64('b'), "encryption", "nip44_v2")
	f.Add(KindNWCRequest, payload(87476), "p", hex64('b'), "encryption", "nip44_v2")
	f.Add(KindNWCInfo, strings.Repeat("a", 4096), "encryption", strings.Repeat("a", 1024), "p", hex64('b'))
	f.Add(KindNWCInfo, strings.Repeat("a", 4097), "encryption", strings.Repeat("a", 1025), "p", hex64('b'))
	f.Add(KindAuth, "", "relay", "wss://"+strings.Repeat("a", 506), "challenge", strings.Repeat("a", 128))
	f.Add(KindAuth, "", "relay", "wss://"+strings.Repeat("a", 507), "challenge", strings.Repeat("a", 129))
	f.Add(0, "", "", "", "", "")
	f.Add(KindNWCResponse, "\xff é", "e", "\x00", "p", " ")
	f.Fuzz(func(t *testing.T, kind int, content, n1, v1, n2, v2 string) {
		e := ev(kind, content, []string{n1, v1}, []string{n2, v2})
		err := ValidateNWC(e)
		if err == nil || err == ErrLegacyNIP04 {
			// anything accepted must be serialisable
			if _, serr := Serialize(e); serr != nil {
				t.Fatalf("accepted but not serialisable: %v", serr)
			}
		}
		e.Tags = [][]string{{n1}, nil, {n1, v1, n2}}
		_ = ValidateNWC(e)
		e.Tags = nil
		_ = ValidateNWC(e)
	})
}

// paddedRaw returns a valid event JSON of exactly total bytes.
func paddedRaw(total int) []byte {
	base := validRaw(hex64('1'), hex64('a'), hex128('c'), `""`, "1", "1", `[]`)
	return []byte(validRaw(hex64('1'), hex64('a'), hex128('c'), `"`+strings.Repeat("x", total-len(base))+`"`, "1", "1", `[]`))
}

var escapePieces = []struct{ text, decoded string }{
	{`\u0000`, "\x00"}, {`\u0001`, "\x01"}, {`\u0009`, "\t"}, {`\n`, "\n"}, {`\u001f`, "\x1f"},
	{`\u007f`, "\x7f"}, {`\u00e9`, "é"}, {`\u2028`, "\u2028"}, {`\u2029`, "\u2029"}, {`\ufffd`, "\ufffd"},
	{`\ud800`, "\ufffd"}, {`\u0041`, "A"}, {`\u0022`, `"`}, {`\u005c`, `\`}, {`\/`, "/"},
}

// genJSONString returns a JSON string literal and the value it decodes to. With injectRate > 0 it
// mixes in raw non-ASCII, raw control bytes, invalid UTF-8 and \u escapes.
func genJSONString(r *rand.Rand, base string, injectRate int) (text, decoded string) {
	var tb, db strings.Builder
	tb.WriteByte('"')
	put := func(c byte) {
		if c == '"' || c == '\\' {
			tb.WriteByte('\\')
		}
		tb.WriteByte(c)
		db.WriteByte(c)
	}
	for i := 0; i < len(base); i++ {
		if injectRate > 0 && r.IntN(injectRate) == 0 {
			switch r.IntN(4) {
			case 0:
				p := escapePieces[r.IntN(len(escapePieces))]
				tb.WriteString(p.text)
				db.WriteString(p.decoded)
			case 1:
				p := oddPieces[r.IntN(len(oddPieces))]
				tb.WriteString(p)
				if p == "\xff" || p == "\xc3\x28" {
					db.WriteString("\ufffd") // only for the expectation; it is non-printable either way
				} else {
					db.WriteString(p)
				}
			case 2:
				c := byte(r.IntN(256)) // raw byte: invalid JSON if < 0x20, otherwise as-is
				tb.WriteByte(c)
				db.WriteByte(c)
			default:
				put(base[i])
			}
			continue
		}
		put(base[i])
	}
	tb.WriteByte('"')
	return tb.String(), db.String()
}

func randHex(r *rand.Rand, n int) string {
	const h = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = h[r.IntN(16)]
	}
	return string(b)
}

func randPrintable(r *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(0x20 + r.IntN(0x5f))
	}
	return string(b)
}

func TestParseEventProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	accepted, refused := 0, 0
	for i := 0; i < 20000; i++ {
		rate := []int{0, 0, 3, 20, 60}[r.IntN(5)]
		hexField := func(n int) (string, string) {
			base := randHex(r, n)
			if r.IntN(8) == 0 {
				base = randPrintable(r, r.IntN(2*n))
			}
			return genJSONString(r, base, rate)
		}
		idT, id := hexField(64)
		pkT, pk := hexField(64)
		sgT, sg := hexField(128)
		cT, content := genJSONString(r, randPrintable(r, r.IntN(300)), rate)
		var tagsT strings.Builder
		var tags [][]string
		tagsT.WriteByte('[')
		for j := r.IntN(4); j > 0; j-- {
			if len(tags) > 0 {
				tagsT.WriteByte(',')
			}
			tagsT.WriteByte('[')
			tag := []string{}
			for k := r.IntN(4); k > 0; k-- {
				if len(tag) > 0 {
					tagsT.WriteByte(',')
				}
				vt, v := genJSONString(r, randPrintable(r, r.IntN(60)), rate)
				tagsT.WriteString(vt)
				tag = append(tag, v)
			}
			tagsT.WriteByte(']')
			tags = append(tags, tag)
		}
		tagsT.WriteByte(']')
		if tags == nil {
			tags = [][]string{}
		}
		raw := fmt.Sprintf(`{"id":%s,"pubkey":%s,"created_at":%d,"kind":%d,"tags":%s,"content":%s,"sig":%s}`,
			idT, pkT, r.Int64N(1<<40), r.IntN(65536), tagsT.String(), cT, sgT)

		// Expectation: the raw JSON must be valid, and every decoded string printable ASCII, hex valid.
		wantOK := json.Valid([]byte(raw)) && printable(content) &&
			lowerHex(id, 64) && lowerHex(pk, 64) && lowerHex(sg, 128)
		for _, tg := range tags {
			for _, v := range tg {
				wantOK = wantOK && printable(v)
			}
		}
		// Invalid UTF-8 makes json.Valid true but decoding substitutes U+FFFD, which is refused
		// anyway; the expectation's decoded text for it is already non-printable.
		e, err := ParseEvent([]byte(raw))
		if (err == nil) != wantOK {
			t.Fatalf("iteration %d: err=%v wantOK=%v raw=%.200q", i, err, wantOK, raw)
		}
		if err != nil {
			refused++
			continue
		}
		accepted++
		if e.ID != id || e.PubKey != pk || e.Sig != sg || e.Content != content || !reflect.DeepEqual(e.Tags, tags) {
			t.Fatalf("iteration %d: decoded fields differ", i)
		}
		b, _ := json.Marshal(e)
		e2, err := ParseEvent(b)
		if err != nil || !reflect.DeepEqual(e, e2) {
			t.Fatalf("iteration %d: round trip: %v", i, err)
		}
	}
	if accepted < 2000 || refused < 2000 {
		t.Fatalf("accepted=%d refused=%d: generator unbalanced", accepted, refused)
	}
}

func randB64(r *rand.Rand, n int) string {
	const a = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := make([]byte, n)
	for i := range b {
		b[i] = a[r.IntN(64)]
	}
	switch r.IntN(3) { // padding only at the end, 0-2 characters
	case 1:
		if n >= 1 {
			b[n-1] = '='
		}
	case 2:
		if n >= 2 {
			b[n-1], b[n-2] = '=', '='
		}
	}
	return string(b)
}

// randWords returns exactly n bytes of words separated by single spaces.
func randWords(r *rand.Rand, n int, digits bool) string {
	alpha := "abcdefghijklmnopqrstuvwxyz_"
	if digits {
		alpha += "0123456789"
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[r.IntN(len(alpha))]
		if i > 0 && i < n-1 && b[i-1] != ' ' && r.IntN(6) == 0 {
			b[i] = ' '
		}
	}
	return string(b)
}

func TestValidateNWCProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(21, 22))
	p, enc := []string{"p", hex64('b')}, []string{"encryption", "nip44_v2"}
	check := func(what string, n int, want bool, e *Event) {
		t.Helper()
		err := ValidateNWC(e)
		if (err == nil) != want {
			t.Fatalf("%s length %d: err=%v want accepted=%v", what, n, err, want)
		}
	}
	for i := 0; i < 300; i++ {
		for _, n := range []int{0, 4, 128, 131, 132, 133, 136, 87468, 87471, 87472, 87473, 87476, 87480} {
			want := n >= 132 && n <= 87472 && n%4 == 0
			check("payload req", n, want, ev(KindNWCRequest, randB64(r, n), p, enc))
			check("payload resp", n, want, ev(KindNWCResponse, randB64(r, n), p, []string{"e", hex64('d')}))
			check("payload notif", n, want, ev(KindNWCNotification, randB64(r, n), p))
		}
		for _, n := range []int{1, 2, 1023, 1024, 1025, 1100} {
			want := n <= 1024
			nm := randWords(r, n, true)
			if n >= 2 && (nm[0] == ' ' || nm[n-1] == ' ') {
				continue
			}
			check("names", n, want, ev(KindNWCInfo, "get_info", []string{"encryption", nm}, p))
			check("notifications", n, want, ev(KindNWCInfo, "get_info", enc, p, []string{"notifications", nm}))
		}
		for _, n := range []int{1, 4095, 4096, 4097, 5000} {
			c := randWords(r, n, false)
			if n >= 2 && (c[0] == ' ' || c[n-1] == ' ') {
				continue
			}
			check("info content", n, n <= 4096, ev(KindNWCInfo, c, enc, p))
		}
		for _, n := range []int{7, 8, 511, 512, 513, 600} { // total URL length
			u := "wss://" + randPrintable2(r, n-6)
			check("relay", n, n >= 7 && n <= 512, ev(KindAuth, "", []string{"relay", u}, []string{"challenge", randHex(r, 32)}))
		}
		for _, n := range []int{0, 31, 32, 33, 127, 128, 129} {
			check("challenge", n, n >= 32 && n <= 128, ev(KindAuth, "", []string{"relay", "ws://r"}, []string{"challenge", randHex(r, n)}))
		}
	}
}

// randPrintable2 is printable ASCII without space, '"' or '\\'.
func randPrintable2(r *rand.Rand, n int) string {
	b := make([]byte, 0, n)
	for len(b) < n {
		c := byte(0x21 + r.IntN(0x5e))
		if c != '"' && c != '\\' {
			b = append(b, c)
		}
	}
	return string(b)
}
