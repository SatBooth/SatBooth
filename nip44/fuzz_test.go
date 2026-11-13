package nip44

import (
	"encoding/base64"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
)

var fuzzConv = [32]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

func FuzzDecrypt(f *testing.F) {
	_, good := validPayload(f, 40)
	f.Add("")
	f.Add("#")
	f.Add("#" + good)
	f.Add(good)
	f.Add(strings.TrimRight(good, "="))
	f.Add(good[:131])
	f.Add(strings.Repeat("A", 131))
	f.Add(strings.Repeat("A", 132))
	f.Add(strings.Repeat("A", 87472))
	f.Add(strings.Repeat("A", 87473))
	f.Add(good[:20] + "\n" + good[20:])
	f.Add("é\u2028\u2029" + good)
	for b := 0; b < 256; b++ {
		f.Add(string([]byte{byte(b)}) + good[1:])
	}
	for _, v := range []byte{0, 1, 3, 255} {
		f.Add(base64.StdEncoding.EncodeToString(append([]byte{v}, make([]byte, 98)...)))
	}
	f.Fuzz(func(t *testing.T, payload string) {
		pt, err := Decrypt(fuzzConv, payload)
		if err == nil {
			// Acceptance needs a valid MAC, which the fuzzer should not forge; if it does, the
			// plaintext must still be within bounds.
			if len(pt) < 1 || len(pt) > maxPlaintext {
				t.Fatalf("accepted plaintext of length %d", len(pt))
			}
		}
	})
}

func FuzzRoundTrip(f *testing.F) {
	f.Add("a")
	f.Add(strings.Repeat("x", 32))
	f.Add(strings.Repeat("x", 33))
	f.Add("é\u2028\u2029\x00\xff")
	f.Add(strings.Repeat("y", 65535))
	f.Add(strings.Repeat("y", 65536))
	f.Add("")
	f.Fuzz(func(t *testing.T, p string) {
		enc, err := Encrypt(fuzzConv, p)
		if len(p) < 1 || len(p) > maxPlaintext {
			if !errors.Is(err, ErrPlaintextLength) {
				t.Fatalf("length %d: err = %v", len(p), err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decrypt(fuzzConv, enc)
		if err != nil || got != p {
			t.Fatalf("round trip failed: %v", err)
		}
	})
}

// TestRoundTripProperty crosses every plaintext length boundary with seeded random bytes.
func TestRoundTripProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 9))
	lens := []int{1, 2, 31, 32, 33, 63, 64, 65, 255, 256, 257, 511, 512, 513, 1023, 1024, 1025, 32767, 32768, 32769, 65534, 65535}
	for _, n := range lens {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(r.UintN(256))
		}
		p := string(b)
		enc, err := Encrypt(fuzzConv, p)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := Decrypt(fuzzConv, enc); err != nil || got != p {
			t.Fatalf("n=%d: %v", n, err)
		}
	}
	for _, n := range []int{0, 65536, 65537} {
		if _, err := Encrypt(fuzzConv, strings.Repeat("a", n)); !errors.Is(err, ErrPlaintextLength) {
			t.Errorf("n=%d: %v", n, err)
		}
	}
}
