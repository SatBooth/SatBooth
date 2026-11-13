package btcrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// --- ValidName and ValidURL ---

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func refValidName(s string) bool { return nameRe.MatchString(s) }

func TestValidNameBoundaries(t *testing.T) {
	// lengths 0, 1, 64, 65 and the longer cases of one good byte
	for _, n := range []int{0, 1, 2, 63, 64, 65, 66, 1000} {
		s := strings.Repeat("a", n)
		if got, want := ValidName(s), n >= 1 && n <= 64; got != want {
			t.Errorf("ValidName(%d x a) = %v", n, got)
		}
	}
	// every byte at every position of strings of length 1, 2, 3 and 64, and in position 65
	for _, n := range []int{1, 2, 3, 64, 65} {
		for pos := 0; pos < n; pos++ {
			for b := 0; b < 256; b++ {
				buf := []byte(strings.Repeat("a", n))
				buf[pos] = byte(b)
				if got, want := ValidName(string(buf)), refValidName(string(buf)); got != want {
					t.Fatalf("len %d pos %d byte %#x: ValidName = %v, reference %v", n, pos, b, got, want)
				}
			}
		}
	}
	for _, s := range []string{"a\n", "\na", "a b", "é", "a/b", "a:b", "a\x00", "..", "a "} {
		if got, want := ValidName(s), refValidName(s); got != want {
			t.Errorf("ValidName(%q) = %v, reference %v", s, got, want)
		}
	}
}

// refValidURL: an absolute http or https URL of 1 to 512 printable ASCII bytes (0x21 to 0x7e), with a
// host, no user info, no query (not even an empty one) and no fragment.
func refValidURL(s string) bool {
	if len(s) < 1 || len(s) > 512 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	if strings.ContainsAny(s, "#?") {
		return false
	}
	var rest string
	switch l := strings.ToLower(s); { // url.Parse lowercases the scheme, as the service's plainURL does
	case strings.HasPrefix(l, "http://"):
		rest = s[len("http://"):]
	case strings.HasPrefix(l, "https://"):
		rest = s[len("https://"):]
	default:
		return false
	}
	host := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host = rest[:i]
	}
	if host == "" || strings.HasPrefix(host, ":") {
		return false
	}
	// A port, when present (a colon after the last "]"), is 1 to 65535 in digits with no sign and no leading zero.
	if c := strings.LastIndexByte(host, ':'); c >= 0 && c > strings.LastIndexByte(host, ']') {
		port := host[c+1:]
		if len(port) < 1 || len(port) > 5 || port[0] == '0' {
			return false
		}
		n := 0
		for i := 0; i < len(port); i++ {
			if port[i] < '0' || port[i] > '9' {
				return false
			}
			n = n*10 + int(port[i]-'0')
		}
		if n > 65535 {
			return false
		}
	}
	return urlParses(s)
}

func TestValidURL(t *testing.T) {
	good := []string{"HTTP://h", "http://127.0.0.1:18443", "https://node.example/", "http://192.0.2.1:8332/rpc", "http://[::1]:8332", "https://172.30.78.5", "http://h:1", "http://h:65535/x"}
	bad := []string{"", "ftp://x", "http://", "http:///x", "http://u@h", "http://u:p@h", "http://h/?", "http://h/?a=1", "http://h/#", "http://h/#f",
		"http://h/a b", "http://h/\x7f", "http://h/é", "//h", "h", "http:h", "http://:80", "http://h/\n", "javascript:alert(1)",
		"http://h:0", "http://h:65536", "http://h:99999", "http://h:", "http://h:/x", "http://[::1]:", "http://[::1]:0", "http://h:+80", "http://h:080",
		"http://" + strings.Repeat("a", 506), "https://h/" + strings.Repeat("a", 510)}
	for _, s := range good {
		if !ValidURL(s) {
			t.Errorf("ValidURL(%q) = false", s)
		}
	}
	for _, s := range bad {
		if ValidURL(s) {
			t.Errorf("ValidURL(%q) = true", s)
		}
	}
	if !ValidURL("http://h/" + strings.Repeat("a", 512-9)) {
		t.Error("512 bytes refused")
	}
	if ValidURL("http://h/" + strings.Repeat("a", 512-9+1)) {
		t.Error("513 bytes accepted")
	}
}

func TestValidURLEveryByteEveryPosition(t *testing.T) {
	bases := []string{"http://127.0.0.1:8332/x", "https://h.example/a/b", "http://h:65535", "http://h:0/", "http://h:/"}
	for _, base := range bases {
		for pos := 0; pos < len(base); pos++ {
			for b := 0; b < 256; b++ {
				buf := []byte(base)
				buf[pos] = byte(b)
				s := string(buf)
				if got, want := ValidURL(s), refValidURL(s); got != want {
					t.Fatalf("%q: ValidURL = %v, reference %v", s, got, want)
				}
			}
		}
	}
}

func FuzzValidators(f *testing.F) {
	for _, s := range []string{"", "a", "http://h", "http://h/?", "a b", "http://h/#", strings.Repeat("a", 65), "http://u@h"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := ValidName(s), refValidName(s); got != want {
			t.Fatalf("ValidName(%q) = %v, reference %v", s, got, want)
		}
		if got, want := ValidURL(s), refValidURL(s); got != want {
			t.Fatalf("ValidURL(%q) = %v, reference %v", s, got, want)
		}
		if ValidURL(s) && !utf8.ValidString(s) {
			t.Fatalf("ValidURL accepted invalid UTF-8 %q", s)
		}
	})
}

// --- decoders ---

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}

func okSat(v int64, signed bool) bool {
	if signed {
		return v >= -MaxSat && v <= MaxSat
	}
	return v >= 0 && v <= MaxSat
}

// checkAll runs every decoder on raw; none may panic and every success must meet its invariants.
func checkAll(t *testing.T, raw []byte) {
	t.Helper()
	if res, err := decodeEnvelope(raw, "getblockchaininfo"); err == nil {
		if !json.Valid(res) {
			t.Fatalf("envelope result is not JSON: %q", res)
		}
	} else {
		var e *Error
		if !errors.As(err, &e) && !errors.Is(err, ErrBadAnswer) {
			t.Fatalf("envelope error of unexpected kind: %v", err)
		}
	}
	r := json.RawMessage(raw)
	if v, err := decodeBlockchainInfo(r); err == nil {
		if !isLowerHex(v.BestBlockHash, 64) || !inRange(v.Blocks, 0, 10_000_000) || !inRange(v.Headers, 0, 10_000_000) || v.Chain == "" {
			t.Fatalf("blockchaininfo %+v", v)
		}
	}
	// decodeWalletInfo and decodeMempoolInfo have no range or format to check: this asserts only that they do not panic.
	_, _ = decodeWalletInfo(r)
	_, _ = decodeMempoolInfo(r)
	if v, err := decodeDescriptorInfo(r); err == nil {
		if v.Descriptor == "" || len(v.Checksum) != 8 {
			t.Fatalf("descriptorinfo %+v", v)
		}
	}
	if v, err := decodeDescriptors(r); err == nil {
		for _, d := range v.Descriptors {
			if !inRange(d.NextIndex, 0, 1<<31-1) || !inRange(d.Range[0], 0, 1<<31-1) || !inRange(d.Range[1], d.Range[0], 1<<31-1) || d.Desc == "" {
				t.Fatalf("descriptor %+v", d)
			}
		}
	}
	if v, err := decodeAddress(r); err == nil && v == "" {
		t.Fatal("empty address accepted")
	}
	if v, err := decodeAddressInfo(r); err == nil && v.Address == "" {
		t.Fatalf("addressinfo %+v", v)
	}
	if v, err := decodeAddressList(r); err == nil && (len(v) != 1 || v[0] == "") {
		t.Fatalf("addresses %q", v)
	}
	if v, err := decodeSinceBlock(r); err == nil {
		if !isLowerHex(v.LastBlock, 64) || v.Dropped < 0 {
			t.Fatalf("sinceblock %+v", v)
		}
		for _, tx := range append(append([]WalletTx{}, v.Transactions...), v.Removed...) {
			if !inRange(tx.Confirmations, -10_000_000, 10_000_000) || !inRange(tx.BlockHeight, 0, 10_000_000) || !isLowerHex(tx.TxID, 64) || !okSat(tx.AmountSat, true) || (tx.BlockHash != "" && !isLowerHex(tx.BlockHash, 64)) || tx.Category == "" {
				t.Fatalf("wallet tx %+v", tx)
			}
			for _, c := range append(append([]string{}, tx.MempoolConflicts...), tx.WalletConflicts...) {
				if !isLowerHex(c, 64) {
					t.Fatalf("conflict %q", c)
				}
			}
		}
	}
	if v, err := decodeTransaction(r); err == nil {
		if !inRange(v.Confirmations, -10_000_000, 10_000_000) || !inRange(v.BlockHeight, 0, 10_000_000) || !isLowerHex(v.TxID, 64) || (v.BlockHash != "" && !isLowerHex(v.BlockHash, 64)) {
			t.Fatalf("transaction %+v", v)
		}
		for _, c := range v.MempoolConflicts {
			if !isLowerHex(c, 64) {
				t.Fatalf("conflict %q", c)
			}
		}
		for _, in := range v.Inputs {
			if !isLowerHex(in.TxID, 64) || in.Vout < 0 || in.Vout > 1<<32-1 {
				t.Fatalf("input %+v", in)
			}
		}
		for _, o := range v.Outputs {
			if !okSat(o.ValueSat, false) {
				t.Fatalf("output %+v", o)
			}
		}
	}
}

func decoderSeeds() []string {
	env := func(res string) string { return `{"result":` + res + `,"error":null,"id":"satbooth"}` }
	return []string{
		cannedChainInfo, cannedWalletInfo, cannedDescInfo, cannedDescs, cannedAddrInfo, cannedSince, cannedGetTx, cannedMempoolInfo,
		`"` + tAddr + `"`, `["` + tAddr + `"]`, env(cannedChainInfo), env(cannedSince),
		`{"result":null,"error":{"code":-5,"message":"x"},"id":"satbooth"}`, `{}`, `[]`, `null`, ``, `{"transactions":[],"lastblock":"` + tBlk + `"}`,
	}
}

func FuzzDecode(f *testing.F) {
	for _, s := range decoderSeeds() {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) { checkAll(t, raw) })
}

// A seeded mutation sweep over the canned answers, so the invariants are checked on every plain test run.
func TestDecodeMutationProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 13))
	alphabet := []byte(`{}[]",:0123456789.-eE abcdef"` + "\x00\xff")
	n := 0
	for _, seed := range decoderSeeds() {
		for k := 0; k < 400; k++ {
			b := []byte(seed)
			for m := 0; m < 1+r.IntN(4) && len(b) > 0; m++ {
				i := r.IntN(len(b))
				switch r.IntN(3) {
				case 0:
					b[i] = alphabet[r.IntN(len(alphabet))]
				case 1:
					b = append(b[:i], b[i+1:]...)
				default:
					b = append(b[:i], append([]byte{alphabet[r.IntN(len(alphabet))]}, b[i:]...)...)
				}
			}
			checkAll(t, b)
			n++
		}
	}
	if n < 5000 {
		t.Fatalf("only %d mutations", n)
	}
}

func TestDecodeEnvelope(t *testing.T) {
	res, err := decodeEnvelope([]byte(`{"result":{"a":1},"error":null,"id":"satbooth"}`), "m")
	if err != nil || string(res) != `{"a":1}` {
		t.Fatalf("%s, %v", res, err)
	}
	// extra keys and Core's whitespace are fine
	if _, err := decodeEnvelope([]byte("{\"result\":1,\"error\":null,\"id\":\"satbooth\",\"x\":2}\n"), "m"); err != nil {
		t.Fatal(err)
	}
	// an error wins over a result
	_, err = decodeEnvelope([]byte(`{"result":1,"error":{"code":-1,"message":"m"},"id":"satbooth"}`), "m")
	var e *Error
	if !errors.As(err, &e) || e.Code != -1 || e.Method != "m" {
		t.Fatalf("%v", err)
	}
}

func TestConfigTimeoutDefault(t *testing.T) {
	c, err := New(Config{URL: "http://127.0.0.1:1", User: "u", Wallet: "w"}, []byte("p"))
	if err != nil || c.timeout != 30*time.Second {
		t.Fatalf("timeout %v, %v", c.timeout, err)
	}
}

func urlParses(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Hostname() != "" && u.User == nil && u.Opaque == ""
}

func inRange(v, lo, hi int64) bool { return v >= lo && v <= hi }

// The bounds hold at their edges: one past each is dropped or refused.
func TestNumberBounds(t *testing.T) {
	entry := func(confs, height int64) string {
		return fmt.Sprintf(`{"category":"receive","amount":1,"vout":0,"confirmations":%d,"blockheight":%d,"txid":"%s"}`, confs, height, tTx1)
	}
	body := `{"transactions":[` + strings.Join([]string{entry(10_000_000, 10_000_000), entry(-10_000_000, 0), entry(10_000_001, 0), entry(-10_000_001, 0), entry(0, 10_000_001)}, ",") + `],"lastblock":"` + tBlk + `"}`
	sb, err := decodeSinceBlock(json.RawMessage(body))
	if err != nil || len(sb.Transactions) != 2 || sb.Dropped != 3 {
		t.Fatalf("%+v, %v", sb, err)
	}
	info := func(blocks, headers int64) string {
		return fmt.Sprintf(`{"chain":"main","blocks":%d,"headers":%d,"bestblockhash":"%s","initialblockdownload":false}`, blocks, headers, tBlk)
	}
	if _, err := decodeBlockchainInfo(json.RawMessage(info(10_000_000, 10_000_000))); err != nil {
		t.Error(err)
	}
	for _, b := range []string{info(10_000_001, 0), info(0, 10_000_001)} {
		if _, err := decodeBlockchainInfo(json.RawMessage(b)); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("%s: %v", b, err)
		}
	}
	desc := func(next, end int64) string {
		return fmt.Sprintf(`{"wallet_name":"w","descriptors":[{"desc":"d","active":true,"range":[0,%d],"next_index":%d}]}`, end, next)
	}
	if _, err := decodeDescriptors(json.RawMessage(desc(1<<31-1, 1<<31-1))); err != nil {
		t.Error(err)
	}
	for _, b := range []string{desc(1<<31, 5), desc(0, 1<<31)} {
		if _, err := decodeDescriptors(json.RawMessage(b)); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("%s: %v", b, err)
		}
	}
	tx := func(confs, height int64) string {
		return fmt.Sprintf(`{"txid":"%s","confirmations":%d,"blockheight":%d,"decoded":{"vin":[],"vout":[]}}`, tTx1, confs, height)
	}
	if _, err := decodeTransaction(json.RawMessage(tx(10_000_000, 10_000_000))); err != nil {
		t.Error(err)
	}
	for _, b := range []string{tx(10_000_001, 0), tx(-10_000_001, 0), tx(0, 10_000_001)} {
		if _, err := decodeTransaction(json.RawMessage(b)); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("%s: %v", b, err)
		}
	}
}
