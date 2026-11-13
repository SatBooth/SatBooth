package nostr

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"reflect"
	"strings"
	"testing"
)

// testdata/bip340-test-vectors.csv is BIP-340's test vectors (bitcoin/bips, bip-0340/test-vectors.csv), copied
// unmodified; bip340SHA256 is its SHA-256.
const bip340SHA256 = "34c9d1d9c3a88d524bc80778540dc43f8306ec249a7485293063c376db851c2d"

func TestBIP340Vectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/bip340-test-vectors.csv")
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != bip340SHA256 {
		t.Fatalf("vector file SHA-256 is %x, want %s", sum, bip340SHA256)
	}
	rows, err := csv.NewReader(strings.NewReader(string(raw))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 20 {
		t.Fatalf("%d rows, want 20 (header + 19)", len(rows))
	}
	var signed, verified, skipped int
	for _, r := range rows[1:] {
		idx, secHex, pubHex, auxHex, msgHex, sigHex, result := r[0], strings.ToLower(r[1]), strings.ToLower(r[2]), r[3], r[4], strings.ToLower(r[5]), r[6]
		want := result == "TRUE"
		if len(msgHex) != 64 {
			t.Logf("vector %s: message is %d bytes, skipped (events always sign a 32-byte id)", idx, len(msgHex)/2)
			skipped++
			continue
		}
		msg, err := hex.DecodeString(msgHex)
		if err != nil {
			t.Fatalf("vector %s: %v", idx, err)
		}
		var id [32]byte
		copy(id[:], msg)
		if secHex != "" {
			aux, err := hex.DecodeString(auxHex)
			if err != nil || len(aux) != 32 {
				t.Fatalf("vector %s: bad aux_rand", idx)
			}
			var a [32]byte
			copy(a[:], aux)
			k := mustKey(t, secHex)
			if k.PublicKeyHex() != pubHex {
				t.Errorf("vector %s: public key %s, want %s", idx, k.PublicKeyHex(), pubHex)
			}
			sig, err := signID(k, id, &a)
			if err != nil {
				t.Errorf("vector %s: sign: %v", idx, err)
			} else if sig != sigHex {
				t.Errorf("vector %s: signature\n got %s\nwant %s", idx, sig, sigHex)
			}
			signed++
		}
		got := verifyID(pubHex, id, sigHex) == nil
		if got != want {
			t.Errorf("vector %s: verify = %v, want %v (%s)", idx, got, want, r[7])
		}
		verified++
	}
	if signed != 4 || verified != 15 || skipped != 4 {
		t.Errorf("signed=%d verified=%d skipped=%d, want 4/15/4", signed, verified, skipped)
	}
}

func signedEvent(t *testing.T) (*Event, *SecretKey) {
	t.Helper()
	k := mustKey(t, testKeyA)
	e := &Event{CreatedAt: 1700000000, Kind: 23194, Tags: [][]string{{"p", testKeyB}, {"e", "x y"}}, Content: "hello \"world\" \\"}
	if err := Sign(e, k); err != nil {
		t.Fatal(err)
	}
	return e, k
}

func flipHex(s string, i int) string {
	c := byte('0')
	if s[i] == '0' {
		c = '1'
	}
	return s[:i] + string(c) + s[i+1:]
}

func TestSignVerifyRoundTrip(t *testing.T) {
	e, k := signedEvent(t)
	if e.PubKey != k.PublicKeyHex() {
		t.Fatal("pubkey not set")
	}
	if !lowerHex(e.ID, 64) || !lowerHex(e.Sig, 128) {
		t.Fatalf("bad id/sig shape: %q %q", e.ID, e.Sig)
	}
	if err := Verify(e); err != nil {
		t.Fatal(err)
	}
	// Signing is randomised (fresh aux_rand) but always verifies.
	e2 := *e
	if err := Sign(&e2, k); err != nil {
		t.Fatal(err)
	}
	if e2.ID != e.ID || e2.Sig == e.Sig {
		t.Error("expected same id and a different signature from fresh aux randomness")
	}
	if err := Verify(&e2); err != nil {
		t.Fatal(err)
	}
	// A caller-set PubKey from someone else is overwritten.
	e3 := Event{PubKey: strings.Repeat("a", 64), Kind: 1, Tags: [][]string{}}
	if err := Sign(&e3, k); err != nil || e3.PubKey != k.PublicKeyHex() {
		t.Fatalf("pubkey not overwritten: %v", err)
	}

	for i := 0; i < 64; i++ {
		m := *e
		m.ID = flipHex(e.ID, i)
		if Verify(&m) == nil {
			t.Fatalf("ID flip at %d verified", i)
		}
		m = *e
		m.PubKey = flipHex(e.PubKey, i)
		if Verify(&m) == nil {
			t.Fatalf("PubKey flip at %d verified", i)
		}
	}
	for i := 0; i < 128; i++ {
		m := *e
		m.Sig = flipHex(e.Sig, i)
		if Verify(&m) == nil {
			t.Fatalf("Sig flip at %d verified", i)
		}
	}
	for i := 0; i < len(e.Content); i++ {
		m := *e
		b := []byte(e.Content)
		b[i] ^= 1 // stays printable for this content
		m.Content = string(b)
		if Verify(&m) == nil {
			t.Fatalf("Content flip at %d verified", i)
		}
	}
	for ti := range e.Tags {
		for vi := range e.Tags[ti] {
			m := *e
			m.Tags = [][]string{append([]string(nil), e.Tags[0]...), append([]string(nil), e.Tags[1]...)}
			m.Tags[ti][vi] += "a"
			if Verify(&m) == nil {
				t.Fatalf("tag [%d][%d] change verified", ti, vi)
			}
		}
	}
	m := *e
	m.CreatedAt++
	if Verify(&m) == nil {
		t.Error("created_at change verified")
	}
	m = *e
	m.Kind++
	if Verify(&m) == nil {
		t.Error("kind change verified")
	}
}

// resign recomputes the id after a mutation, so Verify gets past the id check to the signature.
func resign(t *testing.T, m *Event) {
	t.Helper()
	id, err := ComputeID(m)
	if err != nil {
		t.Fatal(err)
	}
	m.ID = id
}

func TestVerifyRefusesMalformed(t *testing.T) {
	e, _ := signedEvent(t)
	const offCurve = "eefdea4cdb677750a420fee807eacf21eb9898ae79b9768766e4faa04a2d4a34"
	cases := map[string]struct {
		mut    func(*Event)
		resign bool
		want   error
	}{
		"upper id":    {func(m *Event) { m.ID = strings.ToUpper(m.ID) }, false, ErrBadFormat},
		"upper sig":   {func(m *Event) { m.Sig = strings.ToUpper(m.Sig) }, false, ErrBadFormat},
		"upper pub":   {func(m *Event) { m.PubKey = strings.ToUpper(m.PubKey) }, false, ErrBadFormat},
		"short id":    {func(m *Event) { m.ID = m.ID[:63] }, false, ErrBadFormat},
		"short sig":   {func(m *Event) { m.Sig = m.Sig[:127] }, false, ErrBadFormat},
		"long sig":    {func(m *Event) { m.Sig += "0" }, false, ErrBadFormat},
		"empty sig":   {func(m *Event) { m.Sig = "" }, false, ErrBadFormat},
		"short pub":   {func(m *Event) { m.PubKey = m.PubKey[:62] }, false, ErrBadFormat},
		"zero sig":    {func(m *Event) { m.Sig = strings.Repeat("0", 128) }, false, ErrBadSig},
		"s over n":    {func(m *Event) { m.Sig = m.Sig[:64] + strings.Repeat("f", 64) }, false, ErrBadSig},
		"r over p":    {func(m *Event) { m.Sig = strings.Repeat("f", 64) + m.Sig[64:] }, false, ErrBadSig},
		"pub off crv": {func(m *Event) { m.PubKey = offCurve }, false, ErrBadID}, // id covers the pubkey
		// With the id recomputed the check reaches the key and the signature.
		"pub off crv, id recomputed": {func(m *Event) { m.PubKey = offCurve }, true, ErrBadPubKey},
		"pub x>=p, id recomputed": {func(m *Event) {
			m.PubKey = "fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc30"
		}, true, ErrBadPubKey},
		"pub zero, id recomputed":      {func(m *Event) { m.PubKey = strings.Repeat("0", 64) }, true, ErrBadPubKey},
		"pub other key, id recomputed": {func(m *Event) { m.PubKey = mustKey(t, testKeyB).PublicKeyHex() }, true, ErrBadSig},
		"content, id recomputed":       {func(m *Event) { m.Content += "x" }, true, ErrBadSig},
		"created_at, id recomputed":    {func(m *Event) { m.CreatedAt++ }, true, ErrBadSig},
		"kind, id recomputed":          {func(m *Event) { m.Kind++ }, true, ErrBadSig},
		"tag value, id recomputed":     {func(m *Event) { m.Tags = [][]string{{"p", "y"}, {"e", "x y"}} }, true, ErrBadSig},
		"tag added, id recomputed":     {func(m *Event) { m.Tags = append(append([][]string{}, m.Tags...), []string{"t"}) }, true, ErrBadSig},
		"zero sig, id recomputed":      {func(m *Event) { m.Sig = strings.Repeat("0", 128) }, true, ErrBadSig},
	}
	for name, c := range cases {
		m := *e
		m.Tags = append([][]string(nil), e.Tags...)
		c.mut(&m)
		if c.resign {
			resign(t, &m)
		}
		if err := Verify(&m); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	nonASCII := *e
	nonASCII.Content = "caf\u00e9"
	if Verify(&nonASCII) == nil {
		t.Error("non-ASCII content verified")
	}
	if Verify(nil) == nil {
		t.Error("nil event verified")
	}
	if Sign(nil, mustKey(t, testKeyA)) == nil {
		t.Error("Sign(nil event) succeeded")
	}
	if Sign(&Event{}, nil) == nil {
		t.Error("Sign(nil key) succeeded")
	}
	if Sign(&Event{}, &SecretKey{}) == nil {
		t.Error("Sign(zero key) succeeded")
	}
	bad := &Event{Content: "café"}
	if Sign(bad, mustKey(t, testKeyA)) == nil || bad.ID != "" || bad.Sig != "" || bad.PubKey != "" {
		t.Error("Sign of an unserialisable event must fail and leave it untouched")
	}
}

// Sign must only produce events that ParseEvent accepts and Verify passes.
func TestSignRoundTripsThroughParseEvent(t *testing.T) {
	k := mustKey(t, testKeyA)
	e := &Event{CreatedAt: 1700000000, Kind: 1, Content: "hi"} // nil Tags
	if err := Sign(e, k); err != nil {
		t.Fatal(err)
	}
	if e.Tags == nil {
		t.Fatal("Sign left Tags nil")
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseEvent(raw)
	if err != nil {
		t.Fatalf("ParseEvent refused a signed event: %v", err)
	}
	if err := Verify(p); err != nil {
		t.Fatal(err)
	}
}

func TestSignRefusesOutOfRange(t *testing.T) {
	k := mustKey(t, testKeyA)
	for name, e := range map[string]*Event{
		"kind -1":       {Kind: -1},
		"kind 65536":    {Kind: 65536},
		"created_at -1": {CreatedAt: -1},
	} {
		before := *e
		if err := Sign(e, k); err == nil {
			t.Errorf("%s: Sign accepted", name)
		}
		if e.ID != before.ID || e.Sig != before.Sig || e.PubKey != before.PubKey || e.Tags != nil {
			t.Errorf("%s: event changed on error", name)
		}
	}
	for _, kind := range []int{0, 65535} {
		if err := Sign(&Event{Kind: kind}, k); err != nil {
			t.Errorf("kind %d refused: %v", kind, err)
		}
	}
}

// marshalLen is the length of the JSON of e once it carries a real-shaped id and sig.
func marshalLen(t *testing.T, e Event) int {
	t.Helper()
	e.PubKey, e.ID, e.Sig = strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 128)
	b, err := json.Marshal(&e)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func TestSignRefusesWhatParseEventRefuses(t *testing.T) {
	k := mustKey(t, testKeyA)
	base := Event{CreatedAt: 1700000000, Kind: 1, Tags: [][]string{}}
	overhead := marshalLen(t, base)
	atLimit := base
	atLimit.Content = strings.Repeat("a", MaxEventJSON-overhead)
	over := base
	over.Content = strings.Repeat("a", MaxEventJSON-overhead+1)
	// '<' marshals as the six bytes <, so it counts for six.
	escaped := base
	escaped.Content = strings.Repeat("<", (MaxEventJSON-overhead)/6+1)
	cases := []struct {
		name string
		e    Event
		ok   bool
	}{
		{"inner nil tag", Event{Kind: 1, Tags: [][]string{{"p", "x"}, nil}}, false},
		{"only nil tag", Event{Kind: 1, Tags: [][]string{nil}}, false},
		{"empty inner tag", Event{Kind: 1, Tags: [][]string{{}}}, true},
		{"nil tags", Event{Kind: 1}, true},
		{"exactly at the limit", atLimit, true},
		{"one byte over", over, false},
		{"escapes push it over", escaped, false},
	}
	for _, tc := range cases {
		e := tc.e
		before := e
		err := Sign(&e, k)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
				continue
			}
			raw, _ := json.Marshal(&e)
			if len(raw) > MaxEventJSON {
				t.Errorf("%s: signed JSON is %d bytes", tc.name, len(raw))
			}
			if _, err := ParseEvent(raw); err != nil {
				t.Errorf("%s: ParseEvent: %v", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: Sign accepted it", tc.name)
		}
		if !reflect.DeepEqual(e, before) {
			t.Errorf("%s: event changed on refusal", tc.name)
		}
	}
	if n := marshalLen(t, atLimit); n != MaxEventJSON {
		t.Fatalf("limit case is %d bytes, want %d", n, MaxEventJSON)
	}
}

func TestSignParseProperty(t *testing.T) {
	k := mustKey(t, testKeyA)
	rng := rand.New(rand.NewPCG(20261103, 1))
	kinds := []int{-1, 0, 1, 23194, maxKind, maxKind + 1, 1 << 40}
	times := []int64{-1, 0, 1, 1700000000, 1 << 62}
	classes := [][]byte{
		[]byte(" abcXYZ019~"),
		[]byte("\"\\\"\\"),
		[]byte("<>&/'"),
		{0x00, 0x01, 0x1f, 0x7f},
		{0x80, 0xc3, 0xa9, 0xff, 0xe2, 0x80, 0xa8},
	}
	str := func(max int) string {
		n := rng.IntN(max + 1)
		b := make([]byte, n)
		cls := classes[rng.IntN(len(classes))]
		for i := range b {
			if rng.IntN(4) == 0 {
				b[i] = byte(0x20 + rng.IntN(0x5f))
			} else {
				b[i] = cls[rng.IntN(len(cls))]
			}
		}
		if rng.IntN(5) != 0 { // mostly the allowed alphabet only
			for i := range b {
				if b[i] < 0x20 || b[i] > 0x7e {
					b[i] = byte(0x20 + rng.IntN(0x5f))
				}
			}
		}
		return string(b)
	}
	var signed, refused int
	for it := 0; it < 2000; it++ {
		e := Event{CreatedAt: times[rng.IntN(len(times))], Kind: kinds[rng.IntN(len(kinds))]}
		if rng.IntN(4) != 0 { // mostly valid fields
			e.CreatedAt, e.Kind = 1700000000+int64(rng.IntN(1000)), rng.IntN(maxKind+1)
		}
		switch rng.IntN(6) {
		case 0: // nil Tags
		case 1:
			e.Tags = [][]string{}
		default:
			for n := rng.IntN(6); n > 0; n-- {
				var tag []string
				switch rng.IntN(8) {
				case 0: // nil inner tag
				case 1:
					tag = []string{}
				default:
					for m := rng.IntN(4); m >= 0; m-- {
						tag = append(tag, str(40))
					}
				}
				e.Tags = append(e.Tags, tag)
			}
		}
		switch rng.IntN(10) {
		case 0:
			e.Content = ""
		case 1:
			e.Content = strings.Repeat("a", 64*1024)
		case 2:
			e.Content = strings.Repeat("a", MaxEventJSON-rng.IntN(300)) // around the limit
		case 3:
			e.Content = strings.Repeat("<", MaxEventJSON/6-30+rng.IntN(60))
		default:
			e.Content = str(200)
		}
		before := e
		if _, perr := ComputeID(&Event{Tags: e.Tags, Content: e.Content}); perr == nil && !hasNilTag(e.Tags) {
			c := e
			c.PubKey, c.ID, c.Sig = strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 128)
			if c.Tags == nil {
				c.Tags = [][]string{}
			}
			if got, want := signedJSONLen(&c), marshalLen(t, c); got != want {
				t.Fatalf("iteration %d: signedJSONLen = %d, json.Marshal gives %d", it, got, want)
			}
		}
		err := Sign(&e, k)
		if err != nil {
			refused++
			if !reflect.DeepEqual(e, before) {
				t.Fatalf("iteration %d: event changed on refusal", it)
			}
			c := e
			c.PubKey, c.ID, c.Sig = strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 128)
			raw, merr := json.Marshal(&c)
			if merr != nil {
				continue // cannot even be marshalled: ParseEvent has nothing to accept
			}
			if _, perr := ParseEvent(raw); perr == nil {
				t.Fatalf("iteration %d: Sign refused (%v) but ParseEvent accepts its JSON", it, err)
			}
			continue
		}
		signed++
		raw, merr := json.Marshal(&e)
		if merr != nil {
			t.Fatalf("iteration %d: %v", it, merr)
		}
		got, perr := ParseEvent(raw)
		if perr != nil {
			t.Fatalf("iteration %d: signed event refused by ParseEvent: %v", it, perr)
		}
		if !reflect.DeepEqual(*got, e) {
			t.Fatalf("iteration %d: round trip differs", it)
		}
		if err := Verify(got); err != nil {
			t.Fatalf("iteration %d: Verify: %v", it, err)
		}
	}
	if signed < 200 || refused < 200 {
		t.Errorf("weak coverage: signed=%d refused=%d", signed, refused)
	}
}

func hasNilTag(ts [][]string) bool {
	for _, t := range ts {
		if t == nil {
			return true
		}
	}
	return false
}
