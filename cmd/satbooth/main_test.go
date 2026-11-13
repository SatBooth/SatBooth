package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/memlock"
	"github.com/satbooth/satbooth/monero/monerotest"
	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/service"
	"github.com/satbooth/satbooth/store"
)

func TestExampleConfigParses(t *testing.T) {
	b, err := os.ReadFile("testdata/config-example.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := service.ParseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	r := c.Rates
	if r == nil || len(r.Providers) != 4 || r.Providers[3] != "bitfinex" || r.CustomURL != "" || r.CustomURLFile != "/etc/satbooth/rates.url" || len(r.Currencies) != 2 ||
		r.MarginPercent != "1" || r.MinSources != 2 || r.MaxSpreadPercent != "2" || r.MaxAgeSeconds != 300 || r.FetchSeconds != 60 {
		t.Fatalf("rates block: %+v", r)
	}
	if p := c.Payout; p == nil || p.Network != "testnet" || len(p.ApprovalKeys) != 1 || p.MaxMsat != 10000000 || p.DailyMsat != 100000000 {
		t.Fatalf("payout block: %+v", p)
	}
	if a := c.Alerts; a == nil || a.URL != "https://monitor.example.com/satbooth-alert" || a.SecretFile != "/etc/satbooth/alerts.secret" {
		t.Fatalf("alerts block: %+v", a)
	}
	// The example's Monero address is the test wallet's primary address: one that no wallet owns.
	x := c.Monero
	if x == nil || x.Address != monerotest.New(1).Primary() || x.Network != "mainnet" || x.PollRate != 5 || len(x.Confirmations) != 3 {
		t.Fatalf("monero block: %+v", x)
	}
}

// exampleKeySeed is the string the example approval key is derived from (docs/configuration.md, "Replace the example
// approval key").
const exampleKeySeed = "satbooth example approval key: nobody holds its private key"

// onCurve reports whether b is the canonical encoding of a point on Ed25519 (RFC 8032, 5.1.3): y below p, and
// x^2 = (y^2 - 1) / (d y^2 + 1) a square mod p, with x = 0 only for a clear sign bit.
func onCurve(b []byte) bool {
	p := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	le := slices.Clone(b)
	sign := le[31] >> 7
	le[31] &= 0x7f
	slices.Reverse(le)
	y := new(big.Int).SetBytes(le)
	if y.Cmp(p) >= 0 {
		return false
	}
	pm2 := new(big.Int).Sub(p, big.NewInt(2))
	d := new(big.Int).Mul(big.NewInt(-121665), new(big.Int).Exp(big.NewInt(121666), pm2, p))
	d.Mod(d, p)
	y2 := new(big.Int).Mul(y, y)
	u := new(big.Int).Sub(y2, big.NewInt(1))
	v := new(big.Int).Add(new(big.Int).Mul(d, y2), big.NewInt(1))
	x2 := new(big.Int).Mul(u, new(big.Int).Exp(v, pm2, p))
	x2.Mod(x2, p)
	if x2.Sign() == 0 {
		return sign == 0
	}
	return big.Jacobi(x2, p) == 1
}

// TestExampleApprovalKey: the example approval key (docs/configuration.md and testdata/config-example.json) is the
// first of
// SHA-256(exampleKeySeed), SHA-256(exampleKeySeed || 0), SHA-256(exampleKeySeed || 1), ... that is a point on the
// curve and that approval.NewKeys accepts. A hash output is a point nobody knows the private key of.
func TestExampleApprovalKey(t *testing.T) {
	want := ""
	for i := -1; i < 256 && want == ""; i++ {
		in := []byte(exampleKeySeed)
		if i >= 0 {
			in = append(in, byte(i))
		}
		h := sha256.Sum256(in)
		k := hex.EncodeToString(h[:])
		if _, err := approval.NewKeys([]string{k}); err == nil && onCurve(h[:]) {
			want = k
		}
	}
	b, err := os.ReadFile("testdata/config-example.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := service.ParseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Payout == nil || len(c.Payout.ApprovalKeys) != 1 || c.Payout.ApprovalKeys[0] != want {
		t.Fatalf("example approval key is not the derived one %s", want)
	}
	readme, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(readme, []byte(`"approval_keys": ["`+want+`"]`)) ||
		!bytes.Contains(readme, []byte(exampleKeySeed[:40])) {
		t.Fatal("docs/configuration.md does not carry the derived example approval key and its derivation")
	}
	// The check itself: a known point passes, and a value off the curve fails.
	if !onCurve(ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)) {
		t.Fatal("onCurve refuses a real public key")
	}
	off := make([]byte, 32)
	off[0] = 2 // y = 2: x^2 = 3 / (4d + 1), not a square mod p
	if onCurve(off) {
		t.Fatal("onCurve accepts y = 2")
	}
}

func TestUsageAndConfigErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"nope":1}`), 0o600)
	ex, _ := filepath.Abs("testdata/config-example.json") // parses, but its files do not exist
	for name, args := range map[string][]string{
		"no flags":       nil,
		"unknown flag":   {"-x"},
		"extra argument": {"-config", bad, "more"},
		"missing file":   {"-config", filepath.Join(dir, "nope.json")},
		"bad config":     {"-config", bad},
		"missing keys":   {"-config", ex},
	} {
		var stderr bytes.Buffer
		sigs := make(chan os.Signal)
		if code := run(context.Background(), args, &stderr, sigs, func(int) { t.Error("exit called") }, okProtect); code != 2 {
			t.Errorf("%s: exit code %d, stderr %q", name, code, stderr.String())
		}
		if strings.Contains(name, "config") || strings.Contains(name, "missing") {
			checkJSONLines(t, name, stderr.String())
		}
	}
}

func TestStartFailureIsExit1(t *testing.T) {
	// The files exist and parse, but nothing listens at the relay: the dial fails.
	dir := t.TempDir()
	k, err := nostr.GenerateSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "client.key")
	if err := nostr.WriteNewSecretKeyFile(keyPath, k); err != nil {
		t.Fatal(err)
	}
	k.Zero()
	apiPath := filepath.Join(dir, "api-keys.json")
	os.WriteFile(apiPath, []byte(`{"keys":[{"id":"m","capability":"orders:write","sha256":"`+strings.Repeat("ab", 32)+`"}]}`), 0o600)
	whPath := filepath.Join(dir, "webhook.secret")
	os.WriteFile(whPath, []byte(strings.Repeat("cd", 32)+"\n"), 0o600)
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	relayAddr := free.Addr().String()
	free.Close()
	b, _ := os.ReadFile("testdata/config-example.json")
	var m map[string]any
	json.Unmarshal(b, &m)
	delete(m, "onchain") // the dial fails first; this test is about the relay
	delete(m, "monero")  // likewise
	delete(m, "rates")   // no test dials the real rate providers
	delete(m, "payout")  // likewise: the invoice relay fails first
	delete(m, "alerts")  // this test is about the relay
	m["data_dir"] = filepath.Join(dir, "data")
	m["api_keys_file"] = apiPath
	m["nwc"] = map[string]any{"relay_url": "ws://" + relayAddr, "wallet_pubkey": m["nwc"].(map[string]any)["wallet_pubkey"], "client_key_file": keyPath, "network": "testnet"}
	m["webhooks"] = []any{map[string]any{"name": "main", "url": "http://127.0.0.1:9/h", "secret_file": whPath}}
	cfg := filepath.Join(dir, "config.json")
	out, _ := json.Marshal(m)
	os.WriteFile(cfg, out, 0o600)
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), []string{"-config", cfg}, &stderr, make(chan os.Signal), func(int) {}, okProtect)
	}()
	select {
	case code := <-done:
		if code != 1 || !strings.Contains(stderr.String(), "service: ") {
			t.Fatalf("exit %d, stderr %q", code, stderr.String())
		}
		checkJSONLines(t, "start failure", stderr.String())
		if !strings.Contains(stderr.String(), `"reason":"wallet_`) {
			t.Fatalf("no startup reason: %q", stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not return")
	}
}

// checkJSONLines: every line the binary wrote is a JSON object with a message.
func checkJSONLines(t *testing.T, what, out string) {
	t.Helper()
	if strings.TrimSpace(out) == "" {
		t.Errorf("%s: nothing written", what)
	}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil || m["msg"] == nil {
			t.Errorf("%s: not a JSON log line: %q", what, l)
		}
	}
}

func okProtect() (memlock.Status, error) {
	return memlock.Status{Available: true, Locked: true, NotDumpable: true, CoreZero: true}, nil
}

// secretConfig writes a config naming secret files that do not exist yet and returns its path and a
// function that creates the files.
func secretConfig(t *testing.T) (cfgPath string, create func()) {
	t.Helper()
	dir := t.TempDir()
	b, _ := os.ReadFile("testdata/config-example.json")
	var m map[string]any
	json.Unmarshal(b, &m)
	keyPath, apiPath, whPath := filepath.Join(dir, "client.key"), filepath.Join(dir, "api-keys.json"), filepath.Join(dir, "webhook.secret")
	delete(m, "onchain")                        // these tests are about the Lightning files
	delete(m, "monero")                         // likewise
	delete(m, "rates")                          // no test dials the real rate providers
	payPath := filepath.Join(dir, "payout.key") // the payout block stays: its key file is a secret file too
	m["payout"].(map[string]any)["client_key_file"] = payPath
	alPath := filepath.Join(dir, "alerts.secret") // the alerts block stays: its secret is a secret file too
	m["alerts"].(map[string]any)["secret_file"] = alPath
	m["data_dir"] = filepath.Join(dir, "data")
	m["api_keys_file"] = apiPath
	m["nwc"] = map[string]any{"relay_url": "ws://127.0.0.1:1", "wallet_pubkey": m["nwc"].(map[string]any)["wallet_pubkey"], "client_key_file": keyPath, "network": "testnet"}
	m["webhooks"] = []any{map[string]any{"name": "main", "url": "http://127.0.0.1:9/h", "secret_file": whPath}}
	cfgPath = filepath.Join(dir, "config.json")
	out, _ := json.Marshal(m)
	os.WriteFile(cfgPath, out, 0o600)
	return cfgPath, func() {
		k, _ := nostr.GenerateSecretKey()
		nostr.WriteNewSecretKeyFile(keyPath, k)
		k.Zero()
		k, _ = nostr.GenerateSecretKey()
		nostr.WriteNewSecretKeyFile(payPath, k)
		k.Zero()
		os.WriteFile(apiPath, []byte(`{"keys":[{"id":"m","capability":"orders:write","sha256":"`+strings.Repeat("ab", 32)+`"}]}`), 0o600)
		os.WriteFile(whPath, []byte(strings.Repeat("cd", 32)+"\n"), 0o600)
		os.WriteFile(alPath, []byte(strings.Repeat("ef", 32)+"\n"), 0o600)
	}
}

// TestProtectRunsBeforeAnySecretIsOpened: the secret files do not exist until the injected Protect
// creates them. If a secret were read first, startup would fail on the missing file; with the right
// order it gets past the file checks (and stops later, at the unreachable relay).
func TestProtectRunsBeforeAnySecretIsOpened(t *testing.T) {
	cfg, create := secretConfig(t)
	called := false
	protect := func() (memlock.Status, error) {
		called = true
		create()
		return okProtect()
	}
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"-config", cfg}, &stderr, make(chan os.Signal), func(int) {}, protect)
	if !called {
		t.Fatal("Protect was not called")
	}
	if strings.Contains(stderr.String(), "configuration refused") {
		t.Fatalf("a secret file was read before Protect ran (exit %d): %q", code, stderr.String())
	}
}

// TestFailedProtectOpensNoSecret: when locking is refused the process exits 1 with the fixed error,
// and no secret file was ever touched (they do not exist, and the error is not a file error).
func TestFailedProtectOpensNoSecret(t *testing.T) {
	cfg, _ := secretConfig(t)
	var stderr bytes.Buffer
	protect := func() (memlock.Status, error) {
		return memlock.Status{Available: true, NotDumpable: true, CoreZero: true}, errors.New("EPERM")
	}
	code := run(context.Background(), []string{"-config", cfg}, &stderr, make(chan os.Signal), func(int) {}, protect)
	out := stderr.String()
	if code != 1 || !strings.Contains(out, "cannot lock memory (mlockall)") || !strings.Contains(out, `"reason":"memlock"`) ||
		strings.Contains(out, "configuration refused") || strings.Contains(out, "EPERM") {
		t.Fatalf("exit %d, stderr %q", code, out)
	}
	checkJSONLines(t, "memlock refusal", out)
}

// backupConfig writes a configuration whose data directory holds a store and a ledger stamped as a pair at
// generation 1, as a stopped SatBooth leaves them. Backup reads no secret file, so none exists.
func backupConfig(t *testing.T) string {
	t.Helper()
	cfg, _ := secretConfig(t)
	c, err := service.ReadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(c.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lg, err := ledger.Open(ctx, filepath.Join(c.DataDir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	st, err := store.Open(ctx, filepath.Join(c.DataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, nonce := strings.Repeat("ab", 16), strings.Repeat("cd", 16)
	if err := lg.SetPair(ctx, ledger.PairStamp{ID: id, Gen: 1, Nonce: nonce}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPair(ctx, store.PairStamp{ID: id, Gen: 1, Nonce: nonce}); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// satbooth backup: usage errors exit 2 with the usage line, a bad configuration exits 2, a failure exits 1, and a
// success exits 0 with one fixed line that carries the generation.
func TestBackupCommand(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"nope":1}`), 0o600)
	to := filepath.Join(dir, "b")
	const usage = "usage: satbooth backup -config <path> -to <dir>\n"
	for name, args := range map[string][]string{
		"no flags":       {"backup"},
		"no -to":         {"backup", "-config", bad},
		"no -config":     {"backup", "-to", to},
		"unknown flag":   {"backup", "-config", bad, "-to", to, "-x"},
		"extra argument": {"backup", "-config", bad, "-to", to, "more"},
	} {
		var stderr bytes.Buffer
		if code := run(ctx, args, &stderr, make(chan os.Signal), func(int) { t.Error("exit called") }, okProtect); code != 2 {
			t.Errorf("%s: exit code %d, stderr %q", name, code, stderr.String())
		}
		if name != "unknown flag" && stderr.String() != usage {
			t.Errorf("%s: stderr %q", name, stderr.String())
		}
	}
	var stderr bytes.Buffer
	if code := run(ctx, []string{"backup", "-config", bad, "-to", to}, &stderr, make(chan os.Signal), func(int) {}, okProtect); code != 2 ||
		!strings.Contains(stderr.String(), "configuration refused") {
		t.Fatalf("bad config: exit %d, stderr %q", code, stderr.String())
	}
	checkJSONLines(t, "bad config", stderr.String())

	cfg := backupConfig(t)
	stderr.Reset()
	if code := run(ctx, []string{"backup", "-config", cfg, "-to", to}, &stderr, make(chan os.Signal), func(int) {}, okProtect); code != 0 {
		t.Fatalf("backup: exit %d, stderr %q", code, stderr.String())
	}
	var line map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &line); err != nil || line["msg"] != "backup written" || line["generation"] != float64(1) ||
		len(line) != 4 || strings.Count(stderr.String(), "\n") != 1 {
		t.Fatalf("success line %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(to, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := run(ctx, []string{"backup", "-config", cfg, "-to", to}, &stderr, make(chan os.Signal), func(int) {}, okProtect); code != 1 ||
		!strings.Contains(stderr.String(), `"msg":"backup failed"`) || !strings.Contains(stderr.String(), "the backup directory already exists") ||
		strings.Contains(stderr.String(), dir) {
		t.Fatalf("existing dir: exit %d, stderr %q", code, stderr.String())
	}
	checkJSONLines(t, "backup failure", stderr.String())
}

// A stop signal cancels the backup: it exits 1 and leaves no manifest.
func TestBackupStopsOnSignal(t *testing.T) {
	cfg := backupConfig(t)
	to := filepath.Join(t.TempDir(), "b")
	sigs := make(chan os.Signal, 1)
	sigs <- os.Interrupt
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"backup", "-config", cfg, "-to", to}, &stderr, sigs, func(int) {}, okProtect); code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(to, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stopped backup left a manifest: %v", err)
	}
	checkJSONLines(t, "stopped backup", stderr.String())
}
