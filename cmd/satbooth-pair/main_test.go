package main

import (
	"bytes"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/setup"
)

func runPair(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var o, e bytes.Buffer
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

// secretHex reads the key file the way an attacker would, for comparison only.
func secretHex(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func pubFrom(t *testing.T, stdout string) string {
	t.Helper()
	first, _, _ := strings.Cut(stdout, "\n")
	pub, ok := strings.CutPrefix(first, "client public key: ")
	if !ok || len(pub) != 64 {
		t.Fatalf("first line = %q", first)
	}
	return pub
}

func linkFrom(t *testing.T, stdout string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d: %q", len(lines), stdout)
	}
	return lines[1]
}

func TestGoldenLinks(t *testing.T) {
	// Parameter names and separators: getAlby/hub 921003b frontend/src/screens/apps/NewApp.tsx:90-136, 225-230.
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	code, out, errOut := runPair(t, "-key", key, "-hub", "http://127.0.0.1:8080", "-connection", "invoice")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
	pub := pubFrom(t, out)
	want := "http://127.0.0.1:8080/apps/new?name=SatBooth+invoice&notification_types=payment_received" +
		"&pubkey=" + pub + "&request_methods=make_invoice+lookup_invoice+list_transactions+get_info"
	if got := linkFrom(t, out); got != want {
		t.Errorf("invoice link\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(strings.Split(out, "\n")[2], setup.TickSentence) {
		t.Errorf("the instruction does not say to leave the node-info permission ticked: %s", out)
	}
	if !strings.Contains(strings.Split(out, "\n")[2], "Wallet Public Key") {
		t.Errorf("instruction line = %q", strings.Split(out, "\n")[2])
	}

	code, out, _ = runPair(t, "-key", key, "-hub", "https://hub.example/", "-connection", "payout", "-max-amount-sat", "2500")
	if code != 0 {
		t.Fatalf("payout code %d", code)
	}
	want = "https://hub.example/apps/new?budget_renewal=never&max_amount=2500000&name=SatBooth+payout" +
		"&pubkey=" + pub + "&request_methods=pay_invoice+lookup_invoice+get_info"
	if got := linkFrom(t, out); got != want {
		t.Errorf("payout link\n got %s\nwant %s", got, want)
	}

	_, out, _ = runPair(t, "-key", key, "-hub", "http://127.0.0.1:1", "-connection", "invoice", "-name", "a b&c=d")
	u, err := url.Parse(linkFrom(t, out))
	if err != nil || u.Query().Get("name") != "a b&c=d" {
		t.Errorf("custom name did not round-trip: %v %v", err, u)
	}
}

func TestHubValidation(t *testing.T) {
	ok := map[string]string{
		"http://127.0.0.1:8080":    "http://127.0.0.1:8080/apps/new?",
		"https://hub.example/":     "https://hub.example/apps/new?",
		"https://hub.example/sub/": "https://hub.example/sub/apps/new?",
		"https://hub.example/sub":  "https://hub.example/sub/apps/new?",
	}
	for hub, prefix := range ok {
		key := filepath.Join(t.TempDir(), "k")
		code, out, errOut := runPair(t, "-key", key, "-hub", hub, "-connection", "invoice")
		if code != 0 {
			t.Errorf("%q: code %d %q", hub, code, errOut)
			continue
		}
		if l := linkFrom(t, out); !strings.HasPrefix(l, prefix) {
			t.Errorf("%q: link %q lacks prefix %q", hub, l, prefix)
		}
	}
	bad := []string{"", "hub.example", "ftp://hub.example", "javascript:alert(1)", "http://", "https:///x",
		"https://hub.example?x=1", "https://hub.example/?", "https://hub.example/#f", "https://hub.example#",
		"https://hub.example//", "https://u:p@hub.example", "https://hub.example/a b", "http://hub.example\n"}
	for _, hub := range bad {
		key := filepath.Join(t.TempDir(), "k")
		code, out, errOut := runPair(t, "-key", key, "-hub", hub, "-connection", "invoice")
		if code != 2 || out != "" {
			t.Errorf("%q: code %d stdout %q", hub, code, out)
		}
		if _, err := os.Stat(key); err == nil {
			t.Errorf("%q: key file created despite usage error", hub)
		}
		_ = errOut
	}
}

func TestAmountRules(t *testing.T) {
	hub := "http://127.0.0.1:1"
	cases := []struct {
		name string
		args []string
	}{
		{"payout without amount", []string{"-connection", "payout"}},
		{"payout zero", []string{"-connection", "payout", "-max-amount-sat", "0"}},
		{"payout negative", []string{"-connection", "payout", "-max-amount-sat", "-5"}},
		{"payout junk", []string{"-connection", "payout", "-max-amount-sat", "1e3"}},
		{"payout overflow", []string{"-connection", "payout", "-max-amount-sat", "9223372036854775807"}},
		{"invoice with amount", []string{"-connection", "invoice", "-max-amount-sat", "5"}},
		{"invoice with zero amount", []string{"-connection", "invoice", "-max-amount-sat", "0"}},
		{"unknown connection", []string{"-connection", "both"}},
		{"no connection", nil},
		{"stray argument", []string{"-connection", "invoice", "extra"}},
	}
	for _, c := range cases {
		key := filepath.Join(t.TempDir(), "k")
		code, out, _ := runPair(t, append([]string{"-key", key, "-hub", hub}, c.args...)...)
		if code != 2 || out != "" {
			t.Errorf("%s: code %d stdout %q", c.name, code, out)
		}
		if _, err := os.Stat(key); err == nil {
			t.Errorf("%s: key file created despite usage error", c.name)
		}
	}
	if code, _, _ := runPair(t, "-hub", hub, "-connection", "invoice"); code != 2 {
		t.Errorf("missing -key: code %d", code)
	}
	if code, _, _ := runPair(t, "-key", filepath.Join(t.TempDir(), "k"), "-connection", "invoice"); code != 2 {
		t.Errorf("missing -hub: code %d", code)
	}
	if code, _, _ := runPair(t, "-nope"); code != 2 {
		t.Errorf("unknown flag: code %d", code)
	}
	// The largest amount that does not overflow msat.
	key := filepath.Join(t.TempDir(), "k")
	code, out, _ := runPair(t, "-key", key, "-hub", hub, "-connection", "payout", "-max-amount-sat", "9223372036854775")
	if code != 0 || !strings.Contains(linkFrom(t, out), "max_amount=9223372036854775000&") {
		t.Errorf("max amount: code %d out %q", code, out)
	}
	if code, _, _ := runPair(t, "-key", filepath.Join(t.TempDir(), "k"), "-hub", hub, "-connection", "payout", "-max-amount-sat", "9223372036854776"); code != 2 {
		t.Errorf("one above max: code %d", code)
	}
}

func TestKeyCreatedThenReused(t *testing.T) {
	key := filepath.Join(t.TempDir(), "k")
	args := []string{"-key", key, "-hub", "http://127.0.0.1:1", "-connection", "invoice"}
	code, out1, _ := runPair(t, args...)
	if code != 0 {
		t.Fatal(code)
	}
	fi, err := os.Stat(key)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	before := secretHex(t, key)
	code, out2, _ := runPair(t, args...)
	if code != 0 || out1 != out2 {
		t.Errorf("second run differs: %d\n%q\n%q", code, out1, out2)
	}
	if secretHex(t, key) != before {
		t.Error("key file changed")
	}
}

func TestSecretNeverPrinted(t *testing.T) {
	dir := t.TempDir()
	hub := "http://127.0.0.1:1"
	check := func(label string, code int, out, errOut, secret string) {
		t.Helper()
		if secret == "" {
			t.Fatalf("%s: no secret to compare", label)
		}
		if strings.Contains(out, secret) || strings.Contains(errOut, secret) {
			t.Errorf("%s: secret appears in output (code %d)", label, code)
		}
	}
	key := filepath.Join(dir, "k")
	code, out, errOut := runPair(t, "-key", key, "-hub", hub, "-connection", "invoice")
	secret := secretHex(t, key)
	check("success first run", code, out, errOut, secret)
	code, out, errOut = runPair(t, "-key", key, "-hub", hub, "-connection", "payout", "-max-amount-sat", "10")
	check("success reuse", code, out, errOut, secret)

	// Usage errors with an existing key.
	for _, args := range [][]string{
		{"-key", key, "-hub", "ftp://x", "-connection", "invoice"},
		{"-key", key, "-hub", hub, "-connection", "payout"},
		{"-key", key, "-hub", hub, "-connection", "invoice", "-max-amount-sat", "1"},
		{"-key", key, "-hub", hub, "-connection", "x"},
		{"-key", key, "-bogus"},
		{"-key", key, "-hub", hub, "-connection", "invoice", "stray"},
		{"-h"},
	} {
		code, out, errOut := runPair(t, args...)
		check("usage "+strings.Join(args, " "), code, out, errOut, secret)
		if code == 0 && args[0] != "-h" {
			t.Errorf("%v: want failure", args)
		}
	}

	// Key file errors: valid secret in a file that is refused (mode), a file with the secret plus junk,
	// a directory, a dangling symlink, and an unwritable directory.
	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, []byte(secret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = runPair(t, "-key", loose, "-hub", hub, "-connection", "invoice")
	check("loose mode", code, out, errOut, secret)
	if code != 1 || out != "" {
		t.Errorf("loose mode: code %d stdout %q", code, out)
	}
	junk := filepath.Join(dir, "junk")
	if err := os.WriteFile(junk, []byte(secret+"extra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = runPair(t, "-key", junk, "-hub", hub, "-connection", "invoice")
	check("junk", code, out, errOut, secret)
	if code != 1 || out != "" {
		t.Errorf("junk: code %d stdout %q", code, out)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = runPair(t, "-key", link, "-hub", hub, "-connection", "invoice")
	check("symlink to key", code, out, errOut, secret)
	if code != 1 {
		t.Errorf("symlink: code %d", code)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = runPair(t, "-key", dangling, "-hub", hub, "-connection", "invoice")
	check("dangling symlink", code, out, errOut, secret)
	if code != 1 || out != "" {
		t.Errorf("dangling: code %d stdout %q", code, out)
	}
	code, out, errOut = runPair(t, "-key", dir, "-hub", hub, "-connection", "invoice")
	check("directory", code, out, errOut, secret)
	if code != 1 {
		t.Errorf("directory: code %d", code)
	}
	code, out, errOut = runPair(t, "-key", filepath.Join(dir, "no", "such", "dir", "k"), "-hub", hub, "-connection", "invoice")
	check("missing parent", code, out, errOut, secret)
	if code != 1 || out != "" {
		t.Errorf("missing parent: code %d stdout %q", code, out)
	}
}

// A seeded property test: random flag values never panic, never print a secret, and a success always
// prints exactly three lines whose link parses back to the inputs.
func TestRandomArgsProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	key := filepath.Join(t.TempDir(), "k")
	if code, _, _ := runPair(t, "-key", key, "-hub", "http://127.0.0.1:1", "-connection", "invoice"); code != 0 {
		t.Fatal(code)
	}
	secret := secretHex(t, key)
	alphabet := []string{"a", "Z", "0", " ", "&", "=", "?", "#", "/", "%", "+", "\n", "\x00", "é", "\U0001F600", "\xff", ":", "@"}
	hubs := []string{"http://127.0.0.1:1", "https://h.example/x/", "ftp://h", "http://", "https://h.example?q"}
	conns := []string{"invoice", "payout", "", "x"}
	amts := []string{"", "0", "1", "99", "-1", "x", "9223372036854775"}
	for i := 0; i < 500; i++ {
		var name strings.Builder
		for n := rng.Intn(40); n > 0; n-- {
			name.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		args := []string{"-key", key, "-hub", hubs[rng.Intn(len(hubs))], "-connection", conns[rng.Intn(len(conns))],
			"-name", name.String()}
		if a := amts[rng.Intn(len(amts))]; a != "" {
			args = append(args, "-max-amount-sat", a)
		}
		code, out, errOut := runPair(t, args...)
		if strings.Contains(out, secret) || strings.Contains(errOut, secret) {
			t.Fatalf("secret printed for %q", args)
		}
		if code != 0 {
			if out != "" {
				t.Fatalf("stdout on failure for %q", args)
			}
			continue
		}
		u, err := url.Parse(linkFrom(t, out))
		if err != nil {
			t.Fatalf("link does not parse for %q: %v", args, err)
		}
		if want := name.String(); want != "" && u.Query().Get("name") != want {
			t.Fatalf("name %q came back as %q", want, u.Query().Get("name"))
		}
	}
}
