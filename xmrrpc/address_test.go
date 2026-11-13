package xmrrpc

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

const tAlphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// refAddress is the reference: 95 characters of the alphabet, the first one fixed by network and kind.
var refAddress = map[string]map[bool]*regexp.Regexp{}

func init() {
	body := "[" + tAlphabet + "]{94}$"
	for n, firsts := range map[string][2]string{
		"mainnet": {"4", "8"}, "regtest": {"4", "8"}, "testnet": {"9A", "B"}, "stagenet": {"5", "7"},
	} {
		refAddress[n] = map[bool]*regexp.Regexp{
			false: regexp.MustCompile("^[" + firsts[0] + "]" + body),
			true:  regexp.MustCompile("^[" + firsts[1] + "]" + body),
		}
	}
}

func refValid(s, network string, sub bool) bool {
	r, ok := refAddress[network]
	return ok && r[sub].MatchString(s)
}

func addr(first byte, n int) string {
	b := make([]byte, n)
	b[0] = first
	for i := 1; i < n; i++ {
		b[i] = tAlphabet[(i*7)%len(tAlphabet)]
	}
	return string(b)
}

func TestValidNetwork(t *testing.T) {
	for _, n := range []string{Mainnet, Testnet, Stagenet, Regtest} {
		if !ValidNetwork(n) {
			t.Errorf("%q refused", n)
		}
	}
	for _, n := range []string{"", "Mainnet", "main", "testnet ", "regtest\x00", "signet", "fakechain"} {
		if ValidNetwork(n) {
			t.Errorf("%q accepted", n)
		}
	}
	if Mainnet != "mainnet" || Testnet != "testnet" || Stagenet != "stagenet" || Regtest != "regtest" {
		t.Error("network names")
	}
}

func TestValidAddress(t *testing.T) {
	type c struct {
		s       string
		network string
		sub     bool
		want    bool
	}
	cases := []c{
		{addr('4', 95), Mainnet, false, true},
		{addr('4', 94), Mainnet, false, false},
		{addr('4', 96), Mainnet, false, false},
		{addr('4', 95), Regtest, false, true},
		{addr('8', 95), Mainnet, true, true},
		{addr('8', 95), Regtest, true, true},
		{addr('8', 94), Mainnet, true, false},
		{addr('8', 96), Mainnet, true, false},
		{addr('9', 95), Testnet, false, true},
		{addr('A', 95), Testnet, false, true},
		{addr('B', 95), Testnet, true, true},
		{addr('5', 95), Stagenet, false, true},
		{addr('7', 95), Stagenet, true, true},
		// a primary address is not a subaddress, and the other way round
		{addr('4', 95), Mainnet, true, false},
		{addr('8', 95), Mainnet, false, false},
		{addr('B', 95), Testnet, false, false},
		{addr('9', 95), Testnet, true, false},
		{addr('A', 95), Testnet, true, false},
		{addr('7', 95), Stagenet, false, false},
		{addr('5', 95), Stagenet, true, false},
		// another network's prefix
		{addr('9', 95), Mainnet, false, false},
		{addr('5', 95), Mainnet, false, false},
		{addr('4', 95), Testnet, false, false},
		{addr('4', 95), Stagenet, false, false},
		{addr('B', 95), Mainnet, true, false},
		{addr('7', 95), Mainnet, true, false},
		{addr('8', 95), Testnet, true, false},
		{addr('8', 95), Stagenet, true, false},
		{addr('C', 95), Testnet, false, false},
		{addr('6', 95), Stagenet, true, false},
		// unknown networks
		{addr('4', 95), "", false, false},
		{addr('4', 95), "Mainnet", false, false},
		{addr('4', 95), "fakechain", false, false},
		{"", Mainnet, false, false},
	}
	for _, bad := range []string{"0", "O", "I", "l", "\x00", "+", "/", " ", "\xc3", "\xff"} {
		for _, pos := range []int{1, 50, 94} {
			s := addr('4', 95)
			cases = append(cases, c{s[:pos] + bad + s[pos+1:], Mainnet, false, false})
		}
	}
	// a two-byte character: in place of one character (96 bytes) and of two (95 bytes)
	s := addr('8', 95)
	cases = append(cases, c{s[:10] + "\xc3\xa9" + s[11:], Mainnet, true, false}, c{s[:10] + "\xc3\xa9" + s[12:], Mainnet, true, false},
		c{"\xc3\xa9" + s[2:], Mainnet, true, false})
	for i, tc := range cases {
		if got := ValidAddress(tc.s, tc.network, tc.sub); got != tc.want {
			t.Errorf("case %d: ValidAddress(%q, %q, %v) = %v", i, tc.s, tc.network, tc.sub, got)
		}
		if ref := refValid(tc.s, tc.network, tc.sub); ref != tc.want {
			t.Errorf("case %d: the reference disagrees with the table", i)
		}
	}
}

func TestAnySubaddress(t *testing.T) {
	for _, f := range []byte{'8', 'B', '7'} {
		if !AnySubaddress(addr(f, 95)) {
			t.Errorf("%c refused", f)
		}
		if AnySubaddress(addr(f, 94)) || AnySubaddress(addr(f, 96)) {
			t.Errorf("%c at 94 or 96 accepted", f)
		}
	}
	for _, f := range []byte{'4', '9', 'A', '5', 'C', '6', '1', 'z'} {
		if AnySubaddress(addr(f, 95)) {
			t.Errorf("%c accepted", f)
		}
	}
	if AnySubaddress(addr('8', 94) + "0") {
		t.Error("0 accepted")
	}
}

// TestValidAddressProperty: seeded strings near the boundary agree with the reference for every network.
func TestValidAddressProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(8, 95))
	odd := []string{"0", "O", "I", "l", "\x00", "\xc3\xa9", "\xff", " ", "-", "_"}
	var valid int
	for range 20000 {
		n := 93 + r.IntN(5)
		var b strings.Builder
		b.WriteByte("4895B7A1Cz"[r.IntN(10)])
		for b.Len() < n {
			if r.IntN(200) == 0 {
				b.WriteString(odd[r.IntN(len(odd))])
			} else {
				b.WriteByte(tAlphabet[r.IntN(len(tAlphabet))])
			}
		}
		s := b.String()
		anySub := false
		for _, network := range []string{Mainnet, Testnet, Stagenet, Regtest, "other"} {
			for _, sub := range []bool{false, true} {
				got := ValidAddress(s, network, sub)
				if got != refValid(s, network, sub) {
					t.Fatalf("ValidAddress(%q, %q, %v) = %v", s, network, sub, got)
				}
				if got && sub {
					anySub = true
				}
				if got {
					valid++
				}
			}
		}
		if AnySubaddress(s) != anySub {
			t.Fatalf("AnySubaddress(%q) = %v", s, !anySub)
		}
	}
	if valid < 2000 {
		t.Fatalf("only %d valid: the generator does not reach the boundary", valid)
	}
}
