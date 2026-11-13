package setup

import (
	"bytes"
	"math/rand"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	keyA = strings.Repeat("aa", 32)
	keyB = strings.Repeat("bb", 32)
	keyC = strings.Repeat("cc", 32)
	keyD = strings.Repeat("dd", 32)
	auth = "rpcauth=satbooth:" + strings.Repeat("0a", 16) + "$" + strings.Repeat("1b", 32)
)

func lightningOnly() *PublicToHub {
	return &PublicToHub{Version: 1, Network: "mainnet", HubPrivateAddr: "192.0.2.10", PublicPrivateAddr: "192.0.2.20",
		InvoiceClient: keyA}
}

func everyRail() *PublicToHub {
	return &PublicToHub{Version: 1, Network: "signet", HubPrivateAddr: "192.0.2.10", PublicPrivateAddr: "192.0.2.20",
		InvoiceClient: keyA, PayoutClient: keyB, SatboothRPCAuth: auth, Monero: true, MoneroRPCUser: "xmr.user-1"}
}

func TestHandoffRoundTrip(t *testing.T) {
	for _, h := range []*PublicToHub{lightningOnly(), everyRail()} {
		got, err := ParsePublicToHub(h.Marshal())
		if err != nil || *got != *h {
			t.Errorf("round trip of %+v: got %+v, %v", h, got, err)
		}
	}
	for _, h := range []*HubToPublic{{1, keyC, ""}, {1, keyC, keyD}} {
		got, err := ParseHubToPublic(h.Marshal())
		if err != nil || *got != *h {
			t.Errorf("round trip of %+v: got %+v, %v", h, got, err)
		}
	}
}

func TestMarshalCanonical(t *testing.T) {
	want := "{\n  \"version\": 1,\n  \"invoice_wallet\": \"" + keyC + "\",\n  \"payout_wallet\": \"\"\n}\n"
	if got := string((&HubToPublic{1, keyC, ""}).Marshal()); got != want {
		t.Errorf("got %q", got)
	}
	got := string(lightningOnly().Marshal())
	order := []string{"version", "network", "hub_private_addr", "public_private_addr", "invoice_client", "payout_client",
		"satbooth_rpcauth", "monero", "monero_rpc_user"}
	last := -1
	for _, k := range order {
		i := strings.Index(got, "\""+k+"\"")
		if i <= last {
			t.Errorf("key %s out of order in %q", k, got)
		}
		last = i
	}
}

// edit returns the canonical bytes of h with the first occurrence of old replaced by new.
func edit(t testing.TB, h interface{ Marshal() []byte }, old, new string) []byte {
	t.Helper()
	s := string(h.Marshal())
	if !strings.Contains(s, old) {
		t.Fatalf("%q not in %q", old, s)
	}
	return []byte(strings.Replace(s, old, new, 1))
}

type refusal struct {
	name string
	in   []byte
	want error
}

func publicRefusals(t testing.TB) []refusal {
	base := everyRail()
	good := base.Marshal()
	var r []refusal
	add := func(name string, in []byte, want error) { r = append(r, refusal{name, in, want}) }
	add("empty", nil, ErrHandoffSize)
	add("4097 bytes", padTo(good, 4097), ErrHandoffSize)
	add("array", []byte("[]"), ErrHandoffFormat)
	add("truncated", good[:len(good)-3], ErrHandoffFormat)
	add("not json", []byte("version=1"), ErrHandoffFormat)
	add("duplicate key", edit(t, base, `"monero": true,`, `"monero": true, "monero": true,`), ErrHandoffFormat)
	add("unknown key", edit(t, base, `"monero": true,`, `"monero": true, "extra": 1,`), ErrHandoffFormat)
	add("case variant key", edit(t, base, `"network"`, `"Network"`), ErrHandoffFormat)
	add("missing key", edit(t, base, "  \"monero_rpc_user\": \"xmr.user-1\"\n", ""), ErrHandoffFormat)
	add("trailing object", append(append([]byte{}, good...), good...), ErrHandoffFormat)
	add("trailing junk", append(append([]byte{}, good...), 'x'), ErrHandoffFormat)
	add("version 2", edit(t, base, `"version": 1`, `"version": 2`), ErrHandoffFormat)
	add("version string", edit(t, base, `"version": 1`, `"version": "1"`), ErrHandoffFormat)
	add("version float", edit(t, base, `"version": 1`, `"version": 1.0`), ErrHandoffFormat)
	add("null value", edit(t, base, `"payout_client": "`+keyB+`"`, `"payout_client": null`), ErrHandoffFormat)
	add("monero string", edit(t, base, `"monero": true`, `"monero": "true"`), ErrHandoffFormat)
	add("nested value", edit(t, base, `"network": "signet"`, `"network": {"a": 1}`), ErrHandoffFormat)
	add("other kind", (&HubToPublic{1, keyC, keyD}).Marshal(), ErrHandoffKind)
	add("network testnet", edit(t, base, `"signet"`, `"testnet"`), ErrHandoffContent)
	add("network empty", edit(t, base, `"signet"`, `""`), ErrHandoffContent)
	add("hostname", edit(t, base, `"192.0.2.10"`, `"hub.example"`), ErrHandoffContent)
	add("empty address", edit(t, base, `"192.0.2.10"`, `""`), ErrHandoffContent)
	for _, bad := range []string{"fd00::20", "::1", "::ffff:192.0.2.1", "::ffff:0.0.0.0", "0.0.0.0", "::", "224.0.0.1",
		"239.255.255.250", "0.1.2.3", "240.0.0.1", "255.255.255.254", "ff02::1", "169.254.1.1", "255.255.255.255", "192.0.2.1%eth0", "fe80::1%eth0", "192.0.2.1/32",
		"192.0.2.1:8332", "[192.0.2.1]", " 192.0.2.1", "192.0.2.1 ", "192.0.2.1\n", "192.0.2", "192.0.2.256", "192.000.2.1",
		"3232235777", "0x7f000001"} {
		add("hub addr "+bad, edit(t, base, `"192.0.2.10"`, strconv.Quote(bad)), ErrHandoffContent)
		add("public addr "+bad, edit(t, base, `"192.0.2.20"`, strconv.Quote(bad)), ErrHandoffContent)
	}
	add("loopback on signet", edit(t, base, `"192.0.2.10"`, `"127.0.0.1"`), ErrHandoffContent)
	add("loopback on mainnet", edit(t, lightningOnly(), `"192.0.2.10"`, `"127.0.0.1"`), ErrHandoffContent)
	for _, good := range []string{"10.0.0.1", "100.64.0.1", "192.0.2.255", "223.255.255.255", "1.0.0.1"} {
		h := everyRail()
		h.PublicPrivateAddr = good
		if _, err := ParsePublicToHub(h.Marshal()); err != nil {
			t.Errorf("%s refused: %v", good, err)
		}
	}
	add("same address", edit(t, base, `"192.0.2.20"`, `"192.0.2.10"`), ErrHandoffContent)
	add("mapped twin of the other address", edit(t, base, `"192.0.2.20"`, `"::ffff:192.0.2.10"`), ErrHandoffContent)
	add("duplicate key by escape", edit(t, base, `"monero": true,`, `"monero": true, "\u006donero": true,`), ErrHandoffFormat)
	add("key matching by case folding", edit(t, base, `"monero_rpc_user"`, `"monero_rpc_u\u017fer"`), ErrHandoffFormat)
	add("byte order mark", append([]byte("\xef\xbb\xbf"), good...), ErrHandoffFormat)
	add("invalid UTF-8 in a key", edit(t, base, `"monero"`, "\"mone\xffro\""), ErrHandoffFormat)
	add("invalid UTF-8 in a value", edit(t, base, `"signet"`, "\"sig\xffnet\""), ErrHandoffContent)
	add("version 1e0", edit(t, base, `"version": 1`, `"version": 1e0`), ErrHandoffFormat)
	add("huge version", edit(t, base, `"version": 1`, `"version": 123456789012345678901234`), ErrHandoffFormat)
	add("short client", edit(t, base, keyA, keyA[:63]), ErrHandoffContent)
	add("upper-case client", edit(t, base, keyA, strings.ToUpper(keyA)), ErrHandoffContent)
	add("empty invoice client", edit(t, base, keyA, ""), ErrHandoffContent)
	add("client equal to the other", edit(t, base, keyB, keyA), ErrHandoffContent)
	add("both clients equal, no refunds", edit(t, lightningOnly(), `"payout_client": ""`, `"payout_client": "`+keyA+`"`), ErrHandoffContent)
	add("rpcauth for user hub", edit(t, base, "rpcauth=satbooth:", "rpcauth=hub:"), ErrHandoffContent)
	add("rpcauth with newline", edit(t, base, "$"+strings.Repeat("1b", 32), `\n$`+strings.Repeat("1b", 31)), ErrHandoffContent)
	add("rpcauth trailing newline", edit(t, base, strings.Repeat("1b", 32)+`"`, strings.Repeat("1b", 32)+`\n"`), ErrHandoffContent)
	add("rpcauth no prefix", edit(t, base, "rpcauth=", ""), ErrHandoffContent)
	add("rpcauth upper-case salt", edit(t, base, strings.Repeat("0a", 16), strings.Repeat("0A", 16)), ErrHandoffContent)
	add("rpcauth short hmac", edit(t, base, strings.Repeat("1b", 32), strings.Repeat("1b", 31)), ErrHandoffContent)
	add("monero on, empty user", edit(t, base, `"xmr.user-1"`, `""`), ErrHandoffContent)
	add("monero off, user set", edit(t, base, `"monero": true`, `"monero": false`), ErrHandoffContent)
	add("monero user with colon", edit(t, base, `"xmr.user-1"`, `"a:b"`), ErrHandoffContent)
	add("monero user of 65", edit(t, base, `"xmr.user-1"`, `"`+strings.Repeat("u", 65)+`"`), ErrHandoffContent)
	return r
}

func walletRefusals(t testing.TB) []refusal {
	base := &HubToPublic{1, keyC, keyD}
	good := base.Marshal()
	var r []refusal
	add := func(name string, in []byte, want error) { r = append(r, refusal{name, in, want}) }
	add("empty", nil, ErrHandoffSize)
	add("4097 bytes", padTo(good, 4097), ErrHandoffSize)
	add("array", []byte("[]"), ErrHandoffFormat)
	add("truncated", good[:len(good)-3], ErrHandoffFormat)
	add("duplicate key", edit(t, base, `"version": 1,`, `"version": 1, "version": 1,`), ErrHandoffFormat)
	add("unknown key", edit(t, base, `"version": 1,`, `"version": 1, "x": 1,`), ErrHandoffFormat)
	add("missing key", edit(t, base, ",\n  \"payout_wallet\": \""+keyD+"\"", ""), ErrHandoffFormat)
	add("trailing bytes", append(append([]byte{}, good...), '{'), ErrHandoffFormat)
	add("version 2", edit(t, base, `"version": 1`, `"version": 2`), ErrHandoffFormat)
	add("other kind", everyRail().Marshal(), ErrHandoffKind)
	add("short wallet", edit(t, base, keyC, keyC[:62]), ErrHandoffContent)
	add("upper-case wallet", edit(t, base, keyD, strings.ToUpper(keyD)), ErrHandoffContent)
	add("empty invoice wallet", edit(t, base, keyC, ""), ErrHandoffContent)
	add("wallets equal", edit(t, base, keyD, keyC), ErrHandoffContent)
	return r
}

func TestHandoffRefusals(t *testing.T) {
	for _, c := range publicRefusals(t) {
		if got, err := ParsePublicToHub(c.in); err != c.want || got != nil {
			t.Errorf("public %s: got %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	for _, c := range walletRefusals(t) {
		if got, err := ParseHubToPublic(c.in); err != c.want || got != nil {
			t.Errorf("wallet %s: got %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	// The same valid content padded with trailing whitespace: exactly 4096 bytes is accepted, 4097 is the size error.
	if h, err := ParsePublicToHub(padTo(everyRail().Marshal(), 4096)); err != nil || *h != *everyRail() {
		t.Errorf("4096 bytes: %v, %v", h, err)
	}
	if w, err := ParseHubToPublic(padTo((&HubToPublic{1, keyC, keyD}).Marshal(), 4096)); err != nil || w == nil {
		t.Errorf("wallets, 4096 bytes: %v", err)
	}
	if _, err := ParsePublicToHub(padTo(everyRail().Marshal(), 4097)); err != ErrHandoffSize {
		t.Errorf("4097 bytes: %v", err)
	}
}

// padTo returns b followed by spaces, exactly n bytes long.
func padTo(b []byte, n int) []byte {
	return append(append([]byte{}, b...), bytes.Repeat([]byte(" "), n-len(b))...)
}

func TestLoopbackOnlyOnRegtest(t *testing.T) {
	for _, nw := range []string{"mainnet", "signet", "regtest"} {
		h := lightningOnly()
		h.Network = nw
		h.HubPrivateAddr = "127.0.0.1"
		_, err := ParsePublicToHub(h.Marshal())
		if (nw == "regtest") != (err == nil) {
			t.Errorf("%s: %v", nw, err)
		}
	}
}

// A satbooth line made by RPCAuthLine is accepted by the handoff rule, through Marshal and the parser.
func TestRPCAuthLineFitsHandoff(t *testing.T) {
	check := func(line string) {
		t.Helper()
		h := lightningOnly()
		h.SatboothRPCAuth = line
		got, err := ParsePublicToHub(h.Marshal())
		if err != nil || got.SatboothRPCAuth != line {
			t.Fatalf("line %q: %v", line, err)
		}
	}
	salt := bytes.NewReader([]byte{0x05, 0x0d, 0x3d, 0x81, 0xa6, 0x18, 0x08, 0x66, 0xe2, 0xc5, 0x46, 0xb2, 0xf8, 0xf1, 0x90, 0xb9})
	line, err := RPCAuthLine(salt, "satbooth", "Zx9.Qv-test_2")
	if err != nil {
		t.Fatal(err)
	}
	check(line)
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 200; i++ {
		pw := make([]byte, 1+rng.Intn(128))
		for j := range pw {
			pw[j] = byte(0x21 + rng.Intn(0x7e-0x21+1))
		}
		seed := make([]byte, 16)
		rng.Read(seed)
		line, err := RPCAuthLine(bytes.NewReader(seed), "satbooth", string(pw))
		if err != nil {
			t.Fatal(err)
		}
		check(line)
	}
}

func TestDigest(t *testing.T) {
	want := "ba7816bf 8f01cfea 414140de 5dae2223 b00361a3 96177a9c b410ff61 f20015ad"
	if got := Digest([]byte("abc")); got != want {
		t.Errorf("got %q", got)
	}
}

var rpcauthRE = regexp.MustCompile(`^rpcauth=satbooth:[0-9a-f]{32}\$[0-9a-f]{64}$`)

// oracleAddr re-derives the address rule without the parser's helper: an IPv4 literal as netip reads it, none of
// unspecified, multicast, link-local or the broadcast address, loopback only on regtest.
func oracleAddr(s, network string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() || a.Is4In6() || a.Zone() != "" {
		return false
	}
	b := a.As4()
	switch {
	case b[0] == 0, b[0] >= 224, b[0] == 169 && b[1] == 254:
		return false
	case b[0] == 127:
		return network == "regtest"
	}
	return true
}

func checkPublic(t *testing.T, in []byte) {
	t.Helper()
	h, err := ParsePublicToHub(in)
	if err != nil {
		if h != nil {
			t.Fatal("an error came with a value")
		}
		switch err {
		case ErrHandoffSize, ErrHandoffFormat, ErrHandoffKind, ErrHandoffContent:
		default:
			t.Fatalf("an error outside the fixed set: %v", err)
		}
		return
	}
	if h.Version != 1 || (h.Network != "mainnet" && h.Network != "signet" && h.Network != "regtest") ||
		!oracleAddr(h.HubPrivateAddr, h.Network) || !oracleAddr(h.PublicPrivateAddr, h.Network) ||
		netip.MustParseAddr(h.HubPrivateAddr) == netip.MustParseAddr(h.PublicPrivateAddr) ||
		!isHex(h.InvoiceClient, 64) || (h.PayoutClient != "" && !isHex(h.PayoutClient, 64)) || h.PayoutClient == h.InvoiceClient ||
		(h.SatboothRPCAuth != "" && !rpcauthRE.MatchString(h.SatboothRPCAuth)) ||
		h.Monero != (h.MoneroRPCUser != "") || (h.Monero && !validName(h.MoneroRPCUser)) {
		t.Fatalf("a value breaks its rule: %+v", h)
	}
	again, err := ParsePublicToHub(h.Marshal())
	if err != nil || *again != *h {
		t.Fatalf("the canonical bytes do not parse back: %+v, %v", again, err)
	}
}

func checkWallets(t *testing.T, in []byte) {
	t.Helper()
	h, err := ParseHubToPublic(in)
	if err != nil {
		if h != nil {
			t.Fatal("an error came with a value")
		}
		switch err {
		case ErrHandoffSize, ErrHandoffFormat, ErrHandoffKind, ErrHandoffContent:
		default:
			t.Fatalf("an error outside the fixed set: %v", err)
		}
		return
	}
	if h.Version != 1 || !isHex(h.InvoiceWallet, 64) || (h.PayoutWallet != "" && !isHex(h.PayoutWallet, 64)) ||
		h.PayoutWallet == h.InvoiceWallet {
		t.Fatalf("a value breaks its rule: %+v", h)
	}
	again, err := ParseHubToPublic(h.Marshal())
	if err != nil || *again != *h {
		t.Fatalf("the canonical bytes do not parse back: %+v, %v", again, err)
	}
}

func FuzzParsePublicToHub(f *testing.F) {
	f.Add(lightningOnly().Marshal())
	f.Add(everyRail().Marshal())
	f.Add((&HubToPublic{1, keyC, keyD}).Marshal())
	f.Add([]byte(`{"version":1,"network":"regtest","hub_private_addr":"127.0.0.1","public_private_addr":"127.0.0.2","invoice_client":"` +
		keyA + `","payout_client":"","satbooth_rpcauth":"","monero":false,"monero_rpc_user":""}`))
	f.Add([]byte(`{"version":1,"network":"signet"}`))
	for _, c := range publicRefusals(f) {
		f.Add(c.in)
	}
	f.Fuzz(func(t *testing.T, in []byte) { checkPublic(t, in) })
}

func FuzzParseHubToPublic(f *testing.F) {
	f.Add((&HubToPublic{1, keyC, ""}).Marshal())
	f.Add((&HubToPublic{1, keyC, keyD}).Marshal())
	f.Add(everyRail().Marshal())
	for _, c := range walletRefusals(f) {
		f.Add(c.in)
	}
	f.Fuzz(func(t *testing.T, in []byte) { checkWallets(t, in) })
}

// A seeded mutation run that crosses every byte of the canonical files: each single-byte change and each cut.
func TestHandoffMutations(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	pub := [][]byte{lightningOnly().Marshal(), everyRail().Marshal()}
	wal := [][]byte{(&HubToPublic{1, keyC, keyD}).Marshal()}
	for _, seeds := range [][][]byte{pub, wal} {
		for _, s := range seeds {
			for i := 0; i <= len(s); i++ {
				checkPublic(t, s[:i])
				checkWallets(t, s[:i])
				for n := 0; n < 4; n++ {
					m := append([]byte{}, s...)
					if i < len(m) {
						m[i] = byte(rng.Intn(256))
					}
					checkPublic(t, m)
					checkWallets(t, m)
				}
			}
		}
	}
}

func TestLinkAddrErrorsDoNotEchoNonAddresses(t *testing.T) {
	for _, in := range []string{"secret-host.internal", "pa55w0rd", "192.0.2.1:8332", "192.0.2.1/32", "[192.0.2.1]", ""} {
		err := linkAddr(in, "mainnet")
		if err == nil || (in != "" && strings.Contains(err.Error(), in)) {
			t.Errorf("%q: %v", in, err)
		}
	}
	h := everyRail()
	h.Network = "regtest"
	h.HubPrivateAddr, h.PublicPrivateAddr = "127.0.0.1", "127.0.0.2"
	if _, err := ParsePublicToHub(h.Marshal()); err != nil {
		t.Errorf("two loopback addresses on regtest: %v", err)
	}
}
