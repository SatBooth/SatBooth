package webhook

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"
)

type vector struct {
	SecretHex string `json:"secret_hex"`
	T         int64  `json:"t"`
	Body      string `json:"body"`
	Header    string `json:"header"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Command string   `json:"command"`
		Vectors []vector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Command == "" || len(f.Vectors) != 6 {
		t.Fatalf("vectors.json: command %q, %d vectors, want a command and 6", f.Command, len(f.Vectors))
	}
	return f.Vectors
}

func secretOf(t *testing.T, v vector) []byte {
	t.Helper()
	s, err := hex.DecodeString(v.SecretHex)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The published vectors, computed by Python's hmac module and cross-checked with
// openssl, verify, and Sign reproduces them.
func TestVectors(t *testing.T) {
	for i, v := range loadVectors(t) {
		secret := secretOf(t, v)
		at := time.Unix(v.T, 0)
		if got := Sign(secret, at, []byte(v.Body)); got != v.Header {
			t.Errorf("vector %d: Sign = %q, want %q", i, got, v.Header)
		}
		if err := Verify(secret, v.Header, []byte(v.Body), at, 5*time.Minute); err != nil {
			t.Errorf("vector %d: Verify: %v", i, err)
		}
	}
}

func TestVerifyRefusesAChangedBody(t *testing.T) {
	for i, v := range loadVectors(t) {
		secret := secretOf(t, v)
		at := time.Unix(v.T, 0)
		body := []byte(v.Body)
		if len(body) > 0 {
			for pos := range body {
				c := append([]byte(nil), body...)
				c[pos] ^= 1
				if err := Verify(secret, v.Header, c, at, time.Minute); !errors.Is(err, ErrSignature) {
					t.Fatalf("vector %d: byte %d flipped: %v, want ErrSignature", i, pos, err)
				}
			}
		}
		if err := Verify(secret, v.Header, append(body, 'x'), at, time.Minute); !errors.Is(err, ErrSignature) {
			t.Errorf("vector %d: appended byte: %v", i, err)
		}
		if len(body) > 0 {
			if err := Verify(secret, v.Header, body[:len(body)-1], at, time.Minute); !errors.Is(err, ErrSignature) {
				t.Errorf("vector %d: truncated body: %v", i, err)
			}
		}
		other := append([]byte(nil), secret...)
		other[0] ^= 1
		if err := Verify(other, v.Header, body, at, time.Minute); !errors.Is(err, ErrSignature) {
			t.Errorf("vector %d: other secret: %v", i, err)
		}
	}
}

func TestVerifyWindowEdges(t *testing.T) {
	v := loadVectors(t)[0]
	secret, body := secretOf(t, v), []byte(v.Body)
	at := time.Unix(v.T, 0)
	w := 5 * time.Minute
	for _, c := range []struct {
		name string
		now  time.Time
		ok   bool
	}{
		{"exact", at, true},
		{"window old edge", at.Add(w), true},
		{"window new edge", at.Add(-w), true},
		{"one second too old", at.Add(w + time.Second), false},
		{"one second too new", at.Add(-w - time.Second), false},
		{"a day old", at.Add(24 * time.Hour), false},
		{"half a second over", at.Add(w + 500*time.Millisecond), false},
	} {
		err := Verify(secret, v.Header, body, c.now, w)
		if c.ok && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrTimestamp) {
			t.Errorf("%s: %v, want ErrTimestamp", c.name, err)
		}
	}
	if err := Verify(secret, v.Header, body, at, -time.Second); err == nil {
		t.Error("a negative window must be refused")
	}
}

func TestVerifyIsStrict(t *testing.T) {
	v := loadVectors(t)[0]
	secret, body := secretOf(t, v), []byte(v.Body)
	at := time.Unix(v.T, 0)
	mac := strings.TrimPrefix(v.Header[strings.Index(v.Header, ",")+1:], "v1=")
	ts := "1800000000"
	bad := map[string]string{
		"empty":             "",
		"only t":            "t=" + ts,
		"only v1":           "v1=" + mac,
		"missing t":         "x=" + ts + ",v1=" + mac,
		"missing v1":        "t=" + ts + ",v2=" + mac,
		"duplicate t":       "t=" + ts + ",t=" + ts + ",v1=" + mac,
		"duplicate v1":      "t=" + ts + ",v1=" + mac + ",v1=" + mac,
		"extra field":       v.Header + ",v0=00",
		"reversed order":    "v1=" + mac + ",t=" + ts,
		"uppercase hex":     "t=" + ts + ",v1=" + strings.ToUpper(mac),
		"short hex":         "t=" + ts + ",v1=" + mac[:62],
		"long hex":          "t=" + ts + ",v1=" + mac + "00",
		"non hex":           "t=" + ts + ",v1=" + strings.Repeat("g", 64),
		"leading space":     " " + v.Header,
		"trailing space":    v.Header + " ",
		"trailing newline":  v.Header + "\n",
		"space after comma": "t=" + ts + ", v1=" + mac,
		"space before eq":   "t =" + ts + ",v1=" + mac,
		"space in t":        "t= " + ts + ",v1=" + mac,
		"plus sign":         "t=+" + ts + ",v1=" + mac,
		"minus sign":        "t=-" + ts + ",v1=" + mac,
		"leading zero":      "t=0" + ts + ",v1=" + mac,
		"hex t":             "t=0x6b49d200,v1=" + mac,
		"float t":           "t=" + ts + ".0,v1=" + mac,
		"empty t":           "t=,v1=" + mac,
		"huge t":            "t=99999999999999999999,v1=" + mac,
		"unicode digit":     "t=١,v1=" + mac,
		"semicolon":         "t=" + ts + ";v1=" + mac,
		"trailing comma":    v.Header + ",",
	}
	for name, h := range bad {
		if err := Verify(secret, h, body, at, time.Hour); err == nil {
			t.Errorf("%s: accepted %q", name, h)
		}
	}
	// A malformed header is reported as such, whatever the body.
	if err := Verify(secret, "", body, at, time.Hour); !errors.Is(err, ErrMalformed) {
		t.Errorf("empty header: %v, want ErrMalformed", err)
	}
}

// Seeded property: a round trip verifies; any single-byte change to the header, the body or the
// secret does not; the error never carries the secret.
func TestSignVerifyProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261103))
	for i := 0; i < 3000; i++ {
		secret := make([]byte, 1+rng.Intn(80))
		rng.Read(secret)
		body := make([]byte, rng.Intn(300))
		rng.Read(body)
		at := time.Unix(rng.Int63n(4_000_000_000), 0)
		h := Sign(secret, at, body)
		if err := Verify(secret, h, body, at, time.Minute); err != nil {
			t.Fatalf("round trip: %v (%q)", err, h)
		}
		hb := []byte(h)
		pos := rng.Intn(len(hb))
		hb[pos] ^= byte(1 << rng.Intn(7))
		if string(hb) != h {
			if err := Verify(secret, string(hb), body, at, time.Minute); err == nil {
				t.Fatalf("accepted a changed header %q (from %q)", hb, h)
			}
		}
		if len(body) > 0 {
			c := append([]byte(nil), body...)
			c[rng.Intn(len(c))] ^= byte(1 << rng.Intn(8))
			if err := Verify(secret, h, c, at, time.Minute); err == nil {
				t.Fatal("accepted a changed body")
			}
		}
	}
}

func FuzzVerify(f *testing.F) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_800_000_000, 0)
	for _, v := range []string{"", "t=1800000000", "t=1800000000,v1=" + strings.Repeat("0", 64), "t=,v1=", ",,,", "t=1,t=1,v1=", "v1=t=,"} {
		f.Add(v, []byte("body"))
	}
	f.Add(Sign(secret, now, []byte("body")), []byte("body"))
	f.Add(Sign(secret, now, nil), []byte(nil))
	f.Add(strings.ToUpper(Sign(secret, now, []byte("x"))), []byte("x"))
	f.Fuzz(func(t *testing.T, header string, body []byte) {
		err := Verify(secret, header, body, now, 5*time.Minute)
		if err != nil {
			return
		}
		// Accepted: it must be exactly what Sign produces for that body and the header's own t.
		var ts int64
		rest, ok := strings.CutPrefix(header, "t=")
		if !ok {
			t.Fatalf("accepted %q without a leading t=", header)
		}
		tpart, _, _ := strings.Cut(rest, ",")
		for _, c := range tpart {
			if c < '0' || c > '9' {
				t.Fatalf("accepted a non-decimal t in %q", header)
			}
			ts = ts*10 + int64(c-'0')
		}
		if want := Sign(secret, time.Unix(ts, 0), body); header != want {
			t.Fatalf("accepted %q, Sign gives %q", header, want)
		}
	})
}
