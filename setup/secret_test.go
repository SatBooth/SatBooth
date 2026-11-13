package setup

import (
	"bytes"
	"errors"
	"math/rand"
	"strings"
	"testing"
)

func TestNewSecret(t *testing.T) {
	in := make([]byte, 32)
	for i := range in {
		in[i] = byte(i)
	}
	got, err := NewSecret(bytes.NewReader(in))
	if err != nil || got != "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = NewSecret(bytes.NewReader(in[:31]))
	if err == nil || got != "" {
		t.Fatalf("31 bytes: got %q, %v", got, err)
	}
}

// The vectors were made by running Bitcoin Core v32.1's share/rpcauth/rpcauth.py <user> <password> with
// python3 -I; the script prints rpcauth=<user>:<salt>$<hmac>. The values are test values, not credentials.
func TestRPCAuthLineVectors(t *testing.T) {
	vectors := []struct{ user, password, salt, hmac string }{
		{"alice", "testpass-one", "f1120813b9e2e6694f0a577b757605a8", "f51621113c805e2c98210fcdcb8d15c088524db8bf4e7bbf6bcc37df15d127f2"},
		{"satbooth", "Zx9.Qv-test_2", "050d3d81a6180866e2c546a2b8f190b9", "81145d38a808ca3951721c582f5c64840d70c216a738bffd52b105a637f4528c"},
		{"bob.rpc_1", "a", "1b7ca1ea9a3c6b357d5a85a20c1f4416", "3682a57be897af45a3d3b56f01b3deeb47d7e354d24e439df5915bddfc77fd19"},
	}
	for _, v := range vectors {
		salt := make([]byte, 16)
		for i := range salt {
			hi, lo := v.salt[2*i], v.salt[2*i+1]
			salt[i] = unhex(hi)<<4 | unhex(lo)
		}
		r := bytes.NewReader(salt)
		got, err := RPCAuthLine(r, v.user, v.password)
		want := "rpcauth=" + v.user + ":" + v.salt + "$" + v.hmac
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v\nwant %q", v.user, got, err, want)
		}
		if r.Len() != 0 {
			t.Errorf("%s: read more or less than 16 bytes", v.user)
		}
	}
}

func unhex(c byte) byte {
	if c >= 'a' {
		return c - 'a' + 10
	}
	return c - '0'
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestRPCAuthLineRefuses(t *testing.T) {
	ok := make([]byte, 16)
	cases := map[string][2]string{
		"empty user":        {"", "pw"},
		"colon in user":     {"a:b", "pw"},
		"65 char user":      {strings.Repeat("u", 65), "pw"},
		"space in user":     {"a b", "pw"},
		"empty password":    {"u", ""},
		"space password":    {"u", "a b"},
		"0x7f password":     {"u", "a\x7fb"},
		"0x20 password":     {"u", " "},
		"129 byte password": {"u", strings.Repeat("p", 129)},
		"non-ASCII user":    {"é", "pw"},
	}
	for name, c := range cases {
		if got, err := RPCAuthLine(bytes.NewReader(ok), c[0], c[1]); err == nil || got != "" {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
	}
	if _, err := RPCAuthLine(failReader{}, "u", "pw"); err == nil {
		t.Error("a failing reader was accepted")
	}
	if _, err := RPCAuthLine(bytes.NewReader(ok[:15]), "u", "pw"); err == nil {
		t.Error("15 bytes were accepted")
	}
	// The limits themselves are accepted.
	if _, err := RPCAuthLine(bytes.NewReader(ok), strings.Repeat("u", 64), strings.Repeat("~", 128)); err != nil {
		t.Errorf("limits refused: %v", err)
	}
}

// Property: for any user and password the line is accepted exactly when they obey the rules. (That a satbooth line
// fits the handoff rule is TestRPCAuthLineFitsHandoff.)
func TestRPCAuthLineProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []byte("aZ09._-:$ =\x00\x1f\x20\x21\x7e\x7f\x80\xff")
	gen := func(max int) string {
		b := make([]byte, rng.Intn(max+2))
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(b)
	}
	for i := 0; i < 3000; i++ {
		user, pw := gen(70), gen(135)
		seed := make([]byte, 16)
		rng.Read(seed)
		_, err := RPCAuthLine(bytes.NewReader(seed), user, pw)
		passOK := len(pw) >= 1 && len(pw) <= 128
		for j := 0; j < len(pw); j++ {
			passOK = passOK && pw[j] >= 0x21 && pw[j] <= 0x7e
		}
		if (err == nil) != (validName(user) && passOK) {
			t.Fatalf("user %q pw %q: err %v", user, pw, err)
		}
	}
}
