package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/approval"
)

var testNow = time.Unix(1_800_000_000, 0)

func fixedNow() time.Time { return testNow }

type seededReader struct{ r *rand.ChaCha8 }

func newRand(seed byte) *seededReader {
	var s [32]byte
	s[0] = seed
	return &seededReader{rand.NewChaCha8(s)}
}
func (s *seededReader) Read(p []byte) (int, error) { return s.r.Read(p) }

func do(t *testing.T, rnd *seededReader, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb, fixedNow, rnd)
	return code, out.String(), errb.String()
}

// makeKey runs keygen and returns the key path and public key hex.
func makeKey(t *testing.T) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "approval.key")
	code, out, errs := do(t, newRand(1), "keygen", "-out", p)
	if code != 0 {
		t.Fatalf("keygen: %d %q", code, errs)
	}
	return p, strings.TrimSpace(out)
}

const oid = "0123456789abcdef0123456789abcdef"

func TestKeygen(t *testing.T) {
	p, pub := makeKey(t)
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		t.Fatalf("mode: %v %v", fi, err)
	}
	b, _ := os.ReadFile(p)
	if len(b) != 65 || b[64] != '\n' {
		t.Fatalf("content length %d", len(b))
	}
	seed, ok := decodeHex(string(b[:64]))
	if !ok {
		t.Fatal("seed not lowercase hex")
	}
	if got := hex.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)); got != pub {
		t.Fatal("printed public key does not match the file")
	}
	if len(pub) != 64 || strings.Contains(pub, string(b[:64])) {
		t.Fatal("bad public output")
	}
	// An existing path is refused, and the file is untouched.
	code, out, errs := do(t, newRand(2), "keygen", "-out", p)
	if code == 0 || out != "" || errs == "" {
		t.Fatalf("existing path: %d %q %q", code, out, errs)
	}
	if b2, _ := os.ReadFile(p); !bytes.Equal(b, b2) {
		t.Fatal("existing file changed")
	}
	// A symlink target is refused too.
	link := filepath.Join(t.TempDir(), "l")
	os.Symlink(filepath.Join(t.TempDir(), "nowhere"), link)
	if code, _, _ := do(t, newRand(2), "keygen", "-out", link); code == 0 {
		t.Fatal("dangling symlink accepted")
	}
	if code, _, _ := do(t, newRand(2), "keygen"); code != 2 {
		t.Fatal("missing -out")
	}
}

func TestRefundVerifies(t *testing.T) {
	p, pub := makeKey(t)
	keys, err := approval.NewKeys([]string{pub})
	if err != nil {
		t.Fatal(err)
	}
	code, out, errs := do(t, newRand(7), "refund", "-key", p, "-network", "regtest", "-order-id", oid,
		"-amount-msat", "21000", "-expires-in", "1h")
	if code != 0 || errs != "" {
		t.Fatalf("%d %q", code, errs)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "refund_id=") || !strings.HasPrefix(lines[1], "approval=") {
		t.Fatalf("output %q", out)
	}
	r, err := keys.VerifyRefund(strings.TrimPrefix(lines[1], "approval="), approval.Regtest, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(r.RefundID[:]) != strings.TrimPrefix(lines[0], "refund_id=") || hex.EncodeToString(r.OrderID[:]) != oid ||
		r.AmountMsat != 21000 || r.ExpiresAt != testNow.Unix()+3600 {
		t.Fatalf("fields %+v", r)
	}
	// Wrong network does not verify; refund ids are fresh.
	if _, err := keys.VerifyRefund(strings.TrimPrefix(lines[1], "approval="), approval.Mainnet, testNow); err == nil {
		t.Fatal("wrong network verified")
	}
	_, out2, _ := do(t, newRand(8), "refund", "-key", p, "-network", "regtest", "-order-id", oid,
		"-amount-msat", "21000", "-expires-in", "1h")
	if strings.Split(out2, "\n")[0] == lines[0] {
		t.Fatal("refund id repeated")
	}
}

func TestVoidVerifies(t *testing.T) {
	p, pub := makeKey(t)
	keys, _ := approval.NewKeys([]string{pub})
	code, out, errs := do(t, newRand(1), "void", "-key", p, "-network", "signet", "-refund-id", oid, "-expires-in", "719h")
	if code != 0 || errs != "" {
		t.Fatalf("%d %q", code, errs)
	}
	if strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, "approval=") {
		t.Fatalf("output %q", out)
	}
	v, err := keys.VerifyVoid(strings.TrimSpace(strings.TrimPrefix(out, "approval=")), approval.Signet, testNow)
	if err != nil || hex.EncodeToString(v.RefundID[:]) != oid || v.ExpiresAt != testNow.Unix()+719*3600 {
		t.Fatalf("%+v %v", v, err)
	}
}

// TestUsage: a usage error prints the message, then the help once, one line per command, with no line repeated; the
// void line says that a void ends an open refund, and that one the hub reports pending waits.
func TestUsage(t *testing.T) {
	code, out, errs := do(t, newRand(1), "void")
	if code != 2 || out != "" {
		t.Fatalf("%d %q", code, out)
	}
	lines := strings.Split(strings.TrimSuffix(errs, "\n"), "\n")
	if len(lines) != 4 || lines[0] != "satbooth-approve: -key is required" {
		t.Fatalf("usage output %q", errs)
	}
	seen := map[string]bool{}
	for i, cmd := range []string{"keygen", "refund", "void"} {
		l := lines[i+1]
		if seen[l] || !strings.Contains(l, "satbooth-approve "+cmd+" -") {
			t.Errorf("help line %d %q, want the %s line once", i+1, l, cmd)
		}
		seen[l] = true
	}
	if !strings.HasSuffix(lines[3], "(ends an open refund; one the hub reports pending waits)") {
		t.Errorf("void help %q does not say what a void ends", lines[3])
	}
}

func TestFlagBoundaries(t *testing.T) {
	p, _ := makeKey(t)
	base := func(extra ...string) []string {
		return append([]string{"refund", "-key", p, "-network", "regtest", "-order-id", oid, "-amount-msat", "1000", "-expires-in", "1h"}, extra...)
	}
	with := func(flag, val string) []string {
		a := base()
		for i := range a {
			if a[i] == flag {
				a[i+1] = val
			}
		}
		return a
	}
	good := [][]string{
		with("-amount-msat", "1000"), with("-amount-msat", "9007199254740000"), with("-amount-msat", "2000"),
		with("-expires-in", "1m"), with("-expires-in", "719h"), with("-expires-in", "90s"),
	}
	for _, a := range good {
		if code, _, errs := do(t, newRand(1), a...); code != 0 {
			t.Errorf("%v: %d %q", a, code, errs)
		}
	}
	bad := [][]string{
		with("-amount-msat", "0"), with("-amount-msat", "-1000"), with("-amount-msat", "1500"), with("-amount-msat", "999"),
		with("-amount-msat", "9007199254741000"), with("-amount-msat", "9223372036854775000"), with("-amount-msat", "x"),
		// Plain decimal only: no base prefix, leading zero, sign, underscore or space.
		with("-amount-msat", "0x3e8"), with("-amount-msat", "0o1750"), with("-amount-msat", "0b1111101000"),
		with("-amount-msat", "01000"), with("-amount-msat", "017500"), with("-amount-msat", "+1000"),
		with("-amount-msat", "1_000"), with("-amount-msat", " 1000"), with("-amount-msat", "1000 "), with("-amount-msat", ""),
		with("-amount-msat", "00"), with("-amount-msat", "99999999999999999000"),
		with("-expires-in", "59s"), with("-expires-in", "719h0m1s"), with("-expires-in", "720h"), with("-expires-in", "0"),
		with("-expires-in", "-1h"),
		with("-expires-in", "1"), with("-expires-in", ""),
		with("-order-id", strings.Repeat("0", 32)), with("-order-id", strings.ToUpper(oid)), with("-order-id", oid[:31]),
		with("-order-id", oid+"0"), with("-order-id", "g"+oid[1:]), with("-order-id", ""),
		with("-network", "Regtest"), with("-network", ""), with("-network", "bitcoin"),
		with("-key", ""), base("extra"), {"refund"}, {}, {"bogus"}, {"refund", "-seed", "00"},
	}
	for _, a := range bad {
		code, out, errs := do(t, newRand(1), a...)
		if code == 0 || out != "" || errs == "" {
			t.Errorf("%v accepted: %d %q %q", a, code, out, errs)
		}
	}
	// void ids
	for _, id := range []string{strings.Repeat("0", 32), strings.ToUpper(oid), oid[:30], ""} {
		if code, _, _ := do(t, newRand(1), "void", "-key", p, "-network", "regtest", "-refund-id", id, "-expires-in", "1h"); code == 0 {
			t.Errorf("void id %q accepted", id)
		}
	}
}

func TestKeyFileRefusals(t *testing.T) {
	dir := t.TempDir()
	valid := strings.Repeat("ab", 32)
	args := func(p string) []string {
		return []string{"void", "-key", p, "-network", "regtest", "-refund-id", oid, "-expires-in", "1h"}
	}
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		return p
	}
	ok := []string{valid, valid + "\n"}
	for i, c := range ok {
		if code, _, errs := do(t, newRand(1), args(write("ok"+string(rune('0'+i)), c, 0o600))...); code != 0 {
			t.Errorf("valid %d refused: %q", i, errs)
		}
	}
	if code, _, _ := do(t, newRand(1), args(write("ro", valid, 0o400))...); code != 0 {
		t.Error("0400 refused")
	}
	cases := map[string]string{
		"short": valid[:62], "long": valid + "00", "twonl": valid + "\n\n", "crlf": valid + "\r\n",
		"upper": strings.ToUpper(valid), "nonhex": "zz" + valid[2:], "empty": "", "space": valid + " ",
		"leadnl": "\n" + valid, "prefix": "0x" + valid[2:], "nl-mid": valid[:32] + "\n" + valid[33:],
		"huge": strings.Repeat("a", 1<<16),
	}
	for name, c := range cases {
		code, out, errs := do(t, newRand(1), args(write(name, c, 0o600))...)
		if code == 0 || out != "" {
			t.Errorf("%s accepted", name)
		}
		if c != "" && strings.Contains(errs, strings.TrimSpace(c)) {
			t.Errorf("%s: error contains file content", name)
		}
	}
	for _, m := range []os.FileMode{0o640, 0o604, 0o660, 0o606, 0o644, 0o666, 0o610, 0o601} {
		if code, _, _ := do(t, newRand(1), args(write("m"+m.String(), valid, m))...); code == 0 {
			t.Errorf("mode %v accepted", m)
		}
	}
	good := write("target", valid, 0o600)
	link := filepath.Join(dir, "link")
	os.Symlink(good, link)
	if code, _, _ := do(t, newRand(1), args(link)...); code == 0 {
		t.Error("symlink accepted")
	}
	if code, _, _ := do(t, newRand(1), args(dir)...); code == 0 {
		t.Error("directory accepted")
	}
	if code, _, _ := do(t, newRand(1), args(filepath.Join(dir, "missing"))...); code == 0 {
		t.Error("missing accepted")
	}
}

// Property: for many random inputs inside the bounds, the output verifies and carries the inputs.
func TestSeededProperty(t *testing.T) {
	p, pub := makeKey(t)
	keys, _ := approval.NewKeys([]string{pub})
	r := rand.New(rand.NewPCG(5, 9))
	nets := []string{"mainnet", "testnet", "signet", "regtest"}
	for i := 0; i < 200; i++ {
		var o [16]byte
		for j := range o {
			o[j] = byte(r.Uint32())
		}
		o[0] |= 1
		amt := (1 + r.Int64N(approval.MaxAmountMsat/1000)) * 1000
		secs := 60 + r.Int64N(719*3600-60+1)
		ni := r.IntN(4)
		code, out, errs := do(t, newRand(byte(i)), "refund", "-key", p, "-network", nets[ni], "-order-id", hex.EncodeToString(o[:]),
			"-amount-msat", strconv.FormatInt(amt, 10), "-expires-in", strconv.FormatInt(secs, 10)+"s")
		if code != 0 {
			t.Fatalf("iter %d: %d %q", i, code, errs)
		}
		net, _ := approval.ParseNetwork(nets[ni])
		// Verify at a time inside the window: the lifetime limit is 30 days (720h), the tool's cap 719h.
		rf, err := keys.VerifyRefund(strings.TrimSpace(strings.Split(out, "approval=")[1]), net, testNow)
		if err != nil || rf.OrderID != o || int64(rf.AmountMsat) != amt || rf.ExpiresAt != testNow.Unix()+secs {
			t.Fatalf("iter %d: %+v %v", i, rf, err)
		}
	}
}

// FuzzParseAmount: parseAmount accepts exactly ^[1-9][0-9]{0,15}$, with strconv's base-10 value.
func FuzzParseAmount(f *testing.F) {
	for _, s := range []string{"1", "1000", "9007199254740992", "9999999999999999", "99999999999999999", "0", "01", "0x10",
		"+1", "-1", "1_000", " 1", "", "１"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, ok := parseAmount(s)
		want := len(s) >= 1 && len(s) <= 16 && s[0] >= '1' && s[0] <= '9' && strings.Trim(s, "0123456789") == ""
		if ok != want {
			t.Fatalf("%q: accepted %v, want %v", s, ok, want)
		}
		if ok {
			if w, err := strconv.ParseInt(s, 10, 64); err != nil || w != v {
				t.Fatalf("%q: %d, strconv %d %v", s, v, w, err)
			}
		} else if v != 0 {
			t.Fatalf("%q: refused with value %d", s, v)
		}
	})
}
