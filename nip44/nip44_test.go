package nip44

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/satbooth/satbooth/nostr"
)

const vectorsSHA256 = "269ed0f69e4c192512cc779e78c555090cebc7c785b609e338a62afc3ce25040"

type vectorFile struct {
	V2 struct {
		Valid struct {
			ConvKey []struct {
				Sec1    string `json:"sec1"`
				Pub2    string `json:"pub2"`
				ConvKey string `json:"conversation_key"`
			} `json:"get_conversation_key"`
			MsgKeys struct {
				ConvKey string `json:"conversation_key"`
				Keys    []struct {
					Nonce       string `json:"nonce"`
					ChachaKey   string `json:"chacha_key"`
					ChachaNonce string `json:"chacha_nonce"`
					HmacKey     string `json:"hmac_key"`
				} `json:"keys"`
			} `json:"get_message_keys"`
			PaddedLen      [][2]int `json:"calc_padded_len"`
			EncryptDecrypt []struct {
				Sec1    string `json:"sec1"`
				Sec2    string `json:"sec2"`
				ConvKey string `json:"conversation_key"`
				Nonce   string `json:"nonce"`
				Plain   string `json:"plaintext"`
				Payload string `json:"payload"`
			} `json:"encrypt_decrypt"`
			Long []struct {
				ConvKey       string `json:"conversation_key"`
				Nonce         string `json:"nonce"`
				Pattern       string `json:"pattern"`
				Repeat        int    `json:"repeat"`
				PlainSHA256   string `json:"plaintext_sha256"`
				PayloadSHA256 string `json:"payload_sha256"`
			} `json:"encrypt_decrypt_long_msg"`
		} `json:"valid"`
		Invalid struct {
			ConvKey []struct {
				Sec1 string `json:"sec1"`
				Pub2 string `json:"pub2"`
				Note string `json:"note"`
			} `json:"get_conversation_key"`
			Lengths []int `json:"encrypt_msg_lengths"`
			Decrypt []struct {
				ConvKey string `json:"conversation_key"`
				Payload string `json:"payload"`
				Note    string `json:"note"`
			} `json:"decrypt"`
		} `json:"invalid"`
	} `json:"v2"`
}

var (
	vecOnce sync.Once
	vec     vectorFile
	vecErr  error
)

// vectors loads testdata/nip44.vectors.json: the NIP-44 v2 test vectors of github.com/paulmillr/nip44, copied
// unmodified.
func vectors(t testing.TB) *vectorFile {
	t.Helper()
	vecOnce.Do(func() {
		b, err := os.ReadFile("testdata/nip44.vectors.json")
		if err != nil {
			vecErr = err
			return
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != vectorsSHA256 {
			vecErr = errors.New("vector file SHA-256 does not match the pinned value")
			return
		}
		vecErr = json.Unmarshal(b, &vec)
	})
	if vecErr != nil {
		t.Fatal(vecErr)
	}
	return &vec
}

func h32(t testing.TB, s string) (out [32]byte) {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("bad 32-byte hex %q", s)
	}
	copy(out[:], b)
	return
}

func mustKey(t testing.TB, h string) *nostr.SecretKey {
	t.Helper()
	k, err := nostr.ParseSecretKeyHex(h)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestVectorFilePinned(t *testing.T) { vectors(t) }

func TestConversationKeyVectors(t *testing.T) {
	v := vectors(t).V2.Valid.ConvKey
	if len(v) != 35 {
		t.Fatalf("got %d vectors, want 35", len(v))
	}
	for i, c := range v {
		got, err := ConversationKey(mustKey(t, c.Sec1), c.Pub2)
		if err != nil {
			t.Fatalf("#%d: %v", i, err)
		}
		if got != h32(t, c.ConvKey) {
			t.Errorf("#%d: conversation key mismatch", i)
		}
	}
}

func TestConversationKeyInvalid(t *testing.T) {
	v := vectors(t).V2.Invalid.ConvKey
	if len(v) != 8 {
		t.Fatalf("got %d vectors, want 8", len(v))
	}
	for i, c := range v {
		k, err := nostr.ParseSecretKeyHex(c.Sec1)
		if err != nil {
			// Only the vectors about an invalid sec1 may fail at key parsing.
			if c.Note != "sec1 higher than curve.n" && c.Note != "sec1 is 0" && c.Note != "sec1 == curve.n" {
				t.Errorf("#%d (%s): valid sec1 refused: %v", i, c.Note, err)
			}
			continue
		}
		if strings.HasPrefix(c.Note, "sec1") {
			t.Errorf("#%d (%s): invalid sec1 parsed", i, c.Note)
			continue
		}
		if _, err := ConversationKey(k, c.Pub2); err == nil {
			t.Errorf("#%d (%s): accepted", i, c.Note)
		}
	}
	if _, err := ConversationKey(nil, strings.Repeat("00", 32)); !errors.Is(err, ErrNilKey) {
		t.Error("nil key accepted")
	}
}

func TestMessageKeysVectors(t *testing.T) {
	m := vectors(t).V2.Valid.MsgKeys
	if len(m.Keys) != 32 {
		t.Fatalf("got %d vectors, want 32", len(m.Keys))
	}
	conv := h32(t, m.ConvKey)
	for i, k := range m.Keys {
		ck, cn, hk, err := MessageKeys(conv, h32(t, k.Nonce))
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(ck[:]) != k.ChachaKey || hex.EncodeToString(cn[:]) != k.ChachaNonce ||
			hex.EncodeToString(hk[:]) != k.HmacKey {
			t.Errorf("#%d: message keys mismatch", i)
		}
	}
}

func TestCalcPaddedLenVectors(t *testing.T) {
	v := vectors(t).V2.Valid.PaddedLen
	if len(v) != 24 {
		t.Fatalf("got %d vectors, want 24", len(v))
	}
	for _, p := range v {
		got, err := CalcPaddedLen(p[0])
		if err != nil || got != p[1] {
			t.Errorf("CalcPaddedLen(%d) = %d, %v; want %d", p[0], got, err, p[1])
		}
	}
	for _, n := range []int{-1, 0, 65537, 1 << 40} {
		if _, err := CalcPaddedLen(n); !errors.Is(err, ErrPlaintextLength) {
			t.Errorf("CalcPaddedLen(%d): err = %v", n, err)
		}
	}
}

func TestEncryptDecryptVectors(t *testing.T) {
	v := vectors(t).V2.Valid.EncryptDecrypt
	if len(v) != 10 {
		t.Fatalf("got %d vectors, want 10", len(v))
	}
	for i, c := range v {
		k1, k2 := mustKey(t, c.Sec1), mustKey(t, c.Sec2)
		conv, err := ConversationKey(k1, k2.PublicKeyHex())
		if err != nil {
			t.Fatal(err)
		}
		if conv != h32(t, c.ConvKey) {
			t.Fatalf("#%d: conversation key mismatch", i)
		}
		// The other side derives the same key.
		if conv2, err := ConversationKey(k2, k1.PublicKeyHex()); err != nil || conv2 != conv {
			t.Fatalf("#%d: asymmetric conversation key: %v", i, err)
		}
		got, err := EncryptWithNonce(conv, c.Plain, h32(t, c.Nonce))
		if err != nil || got != c.Payload {
			t.Errorf("#%d: EncryptWithNonce = %q, %v", i, got, err)
		}
		pt, err := Decrypt(conv, c.Payload)
		if err != nil || pt != c.Plain {
			t.Errorf("#%d: Decrypt = %q, %v", i, pt, err)
		}
	}
}

func TestLongMessageVectors(t *testing.T) {
	v := vectors(t).V2.Valid.Long
	if len(v) != 3 {
		t.Fatalf("got %d vectors, want 3", len(v))
	}
	for i, c := range v {
		conv := h32(t, c.ConvKey)
		pt := strings.Repeat(c.Pattern, c.Repeat)
		if s := sha256.Sum256([]byte(pt)); hex.EncodeToString(s[:]) != c.PlainSHA256 {
			t.Fatalf("#%d: plaintext hash mismatch (bad pattern/repeat)", i)
		}
		payload, err := EncryptWithNonce(conv, pt, h32(t, c.Nonce))
		if err != nil {
			t.Fatalf("#%d: %v", i, err)
		}
		if s := sha256.Sum256([]byte(payload)); hex.EncodeToString(s[:]) != c.PayloadSHA256 {
			t.Errorf("#%d: payload hash mismatch", i)
		}
		back, err := Decrypt(conv, payload)
		if err != nil || back != pt {
			t.Errorf("#%d: round trip failed: %v", i, err)
		}
	}
}

func TestEncryptLengthInvalid(t *testing.T) {
	v := vectors(t).V2.Invalid.Lengths
	if len(v) != 4 {
		t.Fatalf("got %d lengths, want 4", len(v))
	}
	var conv [32]byte
	for _, n := range v {
		if _, err := EncryptWithNonce(conv, strings.Repeat("a", n), [32]byte{}); !errors.Is(err, ErrPlaintextLength) {
			t.Errorf("length %d: err = %v", n, err)
		}
		if _, err := Encrypt(conv, strings.Repeat("a", n)); !errors.Is(err, ErrPlaintextLength) {
			t.Errorf("Encrypt length %d: err = %v", n, err)
		}
	}
}

func TestDecryptInvalid(t *testing.T) {
	v := vectors(t).V2.Invalid.Decrypt
	if len(v) != 12 {
		t.Fatalf("got %d vectors, want 12", len(v))
	}
	for i, c := range v {
		_, err := Decrypt(h32(t, c.ConvKey), c.Payload)
		if err == nil {
			t.Errorf("#%d (%s): accepted", i, c.Note)
			continue
		}
		want := map[string]error{
			"unknown encryption version":   ErrUnsupportedVersion,
			"unknown encryption version 0": ErrUnsupportedVersion,
			"invalid base64":               ErrBase64,
			"invalid MAC":                  ErrMAC,
			"invalid padding":              ErrPadding,
		}
		for prefix, sentinel := range want {
			if c.Note == prefix && !errors.Is(err, sentinel) {
				t.Errorf("#%d (%s): err = %v, want %v", i, c.Note, err, sentinel)
			}
		}
		if strings.HasPrefix(c.Note, "invalid payload length") && !errors.Is(err, ErrPayloadLength) {
			t.Errorf("#%d (%s): err = %v, want ErrPayloadLength", i, c.Note, err)
		}
	}
}

// validPayload returns a conversation key and a payload of n plaintext bytes.
func validPayload(t testing.TB, n int) ([32]byte, string) {
	t.Helper()
	conv := h32(t, strings.Repeat("11", 32))
	p, err := EncryptWithNonce(conv, strings.Repeat("a", n), h32(t, strings.Repeat("22", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return conv, p
}

func TestDecryptNearMisses(t *testing.T) {
	conv, good := validPayload(t, 40)
	if _, err := Decrypt(conv, good); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(good)

	reenc := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	withVersion := func(v byte) string { b := append([]byte(nil), raw...); b[0] = v; return reenc(b) }
	// force a payload whose standard form contains + or / so URL-safe differs
	var urlCase string
	for n := 1; n < 300 && urlCase == ""; n++ {
		c, p := validPayload(t, n)
		if strings.ContainsAny(p, "+/") {
			_, err := Decrypt(c, base64.URLEncoding.EncodeToString(mustDecode(t, p)))
			if !errors.Is(err, ErrBase64) {
				t.Fatalf("URL-safe payload: err = %v", err)
			}
			urlCase = p
		}
	}
	if urlCase == "" {
		t.Fatal("no sample with + or /")
	}

	cases := []struct {
		name    string
		payload string
		want    error
	}{
		{"empty", "", ErrPayloadLength},
		{"hash prefix", "#" + good, ErrUnsupportedVersion},
		{"hash only", "#", ErrUnsupportedVersion},
		{"missing padding", strings.TrimRight(good, "="), ErrBase64},
		{"b64 length 131", good[:131], ErrPayloadLength},
		{"b64 length 128", good[:128], ErrPayloadLength},
		{"b64 length 87473", strings.Repeat("A", 87473), ErrPayloadLength},
		{"b64 length 87476", strings.Repeat("A", 87476), ErrPayloadLength},
		{"b64 length 87472 zero bytes", strings.Repeat("A", 87472), ErrUnsupportedVersion},
		{"newline inside", good[:20] + "\n" + good[20:], ErrBase64},
		{"CRLF suffix", good + "\r\n", ErrBase64},
		{"space inside", good[:20] + " " + good[20:], ErrBase64},
		{"nul byte", good[:20] + "\x00" + good[20:], ErrBase64},
		{"version 0", withVersion(0), ErrUnsupportedVersion},
		{"version 1", withVersion(1), ErrUnsupportedVersion},
		{"version 3", withVersion(3), ErrUnsupportedVersion},
		{"version 255", withVersion(255), ErrUnsupportedVersion},
		{"decoded 98 bytes", reenc(append([]byte{2}, make([]byte, 97)...)), ErrDataLength},
		{"decoded 99 bytes bad MAC", reenc(append([]byte{2}, make([]byte, 98)...)), ErrMAC},
		{"decoded 65604 bytes", reenc(append([]byte{2}, make([]byte, 65603)...)), ErrDataLength},
		{"decoded 65603 bytes bad MAC", reenc(append([]byte{2}, make([]byte, 65602)...)), ErrMAC},
	}
	for _, c := range cases {
		_, err := Decrypt(conv, c.payload)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
		} else if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

func mustDecode(t testing.TB, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestDecryptProperty mutates valid payloads and asserts every mutation is refused with the
// sentinel for the rule it breaks.
func TestDecryptProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(44, 2))
	conv := h32(t, strings.Repeat("5a", 32))
	lens := []int{1, 2, 31, 32, 33, 64, 65, 255, 256, 257, 1000, 4096, 65535}
	for _, n := range lens {
		var nonce [32]byte
		for i := range nonce {
			nonce[i] = byte(r.UintN(256))
		}
		pt := strings.Repeat("é", 1) // multi-byte start
		for len(pt) < n {
			pt += string(rune('a' + r.UintN(26)))
		}
		pt = pt[:n]
		if n == 1 {
			pt = "z"
		}
		if n >= 2 && !strings.HasPrefix(pt, "é") {
			t.Fatal("setup")
		}
		payload, err := EncryptWithNonce(conv, pt, nonce)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := Decrypt(conv, payload); err != nil || got != pt {
			t.Fatalf("n=%d round trip: %v", n, err)
		}
		raw := mustDecode(t, payload)

		// Every byte flipped (all bytes for small payloads, a seeded sample for big ones).
		idx := make([]int, 0, len(raw))
		if len(raw) <= 600 {
			for i := range raw {
				idx = append(idx, i)
			}
		} else {
			idx = append(idx, 0, 1, 32, 33, 34, 35, len(raw)-33, len(raw)-32, len(raw)-1)
			for len(idx) < 200 {
				idx = append(idx, r.IntN(len(raw)))
			}
		}
		for _, i := range idx {
			b := append([]byte(nil), raw...)
			b[i] ^= byte(1 + r.UintN(255))
			_, err := Decrypt(conv, base64.StdEncoding.EncodeToString(b))
			want := ErrMAC
			if i == 0 {
				want = ErrUnsupportedVersion
			}
			if !errors.Is(err, want) {
				t.Fatalf("n=%d flip byte %d: err = %v, want %v", n, i, err, want)
			}
		}
		// Every bit of the MAC flipped.
		for bit := 0; bit < 256; bit++ {
			b := append([]byte(nil), raw...)
			b[len(b)-32+bit/8] ^= 1 << (bit % 8)
			if _, err := Decrypt(conv, base64.StdEncoding.EncodeToString(b)); !errors.Is(err, ErrMAC) {
				t.Fatalf("n=%d MAC bit %d: err = %v", n, bit, err)
			}
		}
		// Truncations and extensions of the decoded bytes change the MAC input or the layout.
		for _, cut := range []int{1, 2, 32, 33} {
			if cut < len(raw) {
				if _, err := Decrypt(conv, base64.StdEncoding.EncodeToString(raw[:len(raw)-cut])); err == nil {
					t.Fatalf("n=%d truncated by %d accepted", n, cut)
				}
			}
		}
		if _, err := Decrypt(conv, base64.StdEncoding.EncodeToString(append(append([]byte(nil), raw...), 0))); err == nil {
			t.Fatalf("n=%d extended accepted", n)
		}
		// Text-level mutations.
		if _, err := Decrypt(conv, "#"+payload); !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("n=%d '#': %v", n, err)
		}
		if _, err := Decrypt(conv, payload[:len(payload)-1]); err == nil {
			t.Fatalf("n=%d last char dropped accepted", n)
		}
		if strings.HasSuffix(payload, "=") {
			if _, err := Decrypt(conv, strings.TrimRight(payload, "=")); err == nil {
				t.Fatalf("n=%d missing padding accepted", n)
			}
		}
		if us := base64.URLEncoding.EncodeToString(raw); us != payload {
			if _, err := Decrypt(conv, us); !errors.Is(err, ErrBase64) {
				t.Fatalf("n=%d URL-safe: %v", n, err)
			}
		}
		// A wrong conversation key fails the MAC.
		other := conv
		other[0] ^= 1
		if _, err := Decrypt(other, payload); !errors.Is(err, ErrMAC) {
			t.Fatalf("n=%d wrong key: %v", n, err)
		}
	}
	// Payload-length boundaries on the base64 string.
	for _, l := range []int{0, 1, 130, 131, 87473, 87474, 1 << 20} {
		if _, err := Decrypt(conv, strings.Repeat("A", l)); !errors.Is(err, ErrPayloadLength) {
			t.Errorf("b64 length %d: %v", l, err)
		}
	}
	// Version byte 0/1/3/255 at the two extreme valid sizes.
	for _, dl := range []int{99, 65603} {
		for _, v := range []byte{0, 1, 3, 255} {
			b := make([]byte, dl)
			b[0] = v
			if _, err := Decrypt(conv, base64.StdEncoding.EncodeToString(b)); !errors.Is(err, ErrUnsupportedVersion) {
				t.Errorf("len %d version %d: %v", dl, v, err)
			}
		}
	}
	// Forged padding under a valid MAC: wrong prefix, zero prefix, prefix too long.
	for _, bad := range []struct {
		prefix uint16
		padded int
	}{{0, 32}, {33, 32}, {5, 64}, {32, 64}, {65535, 32}} {
		payload := forge(t, conv, bad.prefix, bad.padded)
		if _, err := Decrypt(conv, payload); !errors.Is(err, ErrPadding) {
			t.Errorf("forged prefix %d padded %d: %v", bad.prefix, bad.padded, err)
		}
	}
	// A correctly padded forged message is accepted (the forger holds the key).
	if got, err := Decrypt(conv, forge(t, conv, 3, 32)); err != nil || got != "\x00\x00\x00" {
		t.Errorf("forge control: %q, %v", got, err)
	}
}

// forge builds an authenticated payload whose plaintext block is a 2-byte prefix plus padded zero
// bytes, so only the padding rules can reject it.
func forge(t testing.TB, conv [32]byte, prefix uint16, padded int) string {
	t.Helper()
	nonce := h32(t, strings.Repeat("77", 32))
	ck, cn, hk, err := MessageKeys(conv, nonce)
	if err != nil {
		t.Fatal(err)
	}
	ct := make([]byte, 2+padded)
	ct[0], ct[1] = byte(prefix>>8), byte(prefix)
	if err := xorChaCha(ck, cn, ct); err != nil {
		t.Fatal(err)
	}
	buf := append([]byte{2}, nonce[:]...)
	buf = append(buf, ct...)
	buf = append(buf, mac(hk, nonce, ct)...)
	return base64.StdEncoding.EncodeToString(buf)
}

func TestRoundTripRandomNonce(t *testing.T) {
	conv := h32(t, strings.Repeat("01", 32))
	a, err := Encrypt(conv, "hello")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encrypt(conv, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two encryptions identical: nonce not random")
	}
	if got, err := Decrypt(conv, a); err != nil || got != "hello" {
		t.Errorf("round trip: %q %v", got, err)
	}
}

func TestPaddedLenProperty(t *testing.T) {
	prev := 0
	for n := 1; n <= 65535; n++ {
		p, err := CalcPaddedLen(n)
		if err != nil || p < n || p < 32 || p%32 != 0 || p < prev || p > 65536 {
			t.Fatalf("n=%d: %d, %v", n, p, err)
		}
		if p-n >= max(32, n/4+1) {
			t.Fatalf("n=%d: padding %d too large", n, p-n)
		}
		prev = p
	}
}
