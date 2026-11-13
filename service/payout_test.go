package service

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/refund/refundtest"
	"github.com/satbooth/satbooth/store"
)

// payoutPub returns a fixed Ed25519 key pair for approvals: the seed is the byte b repeated.
func payoutPair(b byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = b
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(ed25519.PublicKey), priv
}

func payoutBlock() map[string]any {
	pub, _ := payoutPair(7)
	return map[string]any{
		"relay_url": "ws://127.0.0.1:7778", "wallet_pubkey": otherPub(), "client_key_file": "/etc/satbooth/payout.key",
		"timeout_seconds": 70, "network": "regtest", "approval_keys": []any{hex.EncodeToString(pub)},
		"max_msat": 10_000_000, "daily_msat": 100_000_000, "budget_alert_msat": 5_000_000,
	}
}

// otherPub is a valid public key other than goodPub.
func otherPub() string {
	k, err := nostr.LoadSecretKeyFile(writeKey(os.TempDir()))
	if err != nil {
		panic(err)
	}
	defer k.Zero()
	return k.PublicKeyHex()
}

// writeKey is not used for real keys: it makes a throwaway key file for otherPub in a fresh directory.
func writeKey(string) string {
	dir, err := os.MkdirTemp("", "satbooth-otherpub")
	if err != nil {
		panic(err)
	}
	k, err := nostr.GenerateSecretKey()
	if err != nil {
		panic(err)
	}
	defer k.Zero()
	p := filepath.Join(dir, "k")
	if err := nostr.WriteNewSecretKeyFile(p, k); err != nil {
		panic(err)
	}
	return p
}

func withPayout(t testing.TB, mod func(p map[string]any)) []byte {
	t.Helper()
	m := goodConfig()
	p := payoutBlock()
	if mod != nil {
		mod(p)
	}
	m["payout"] = p
	return enc(t, m)
}

func TestPayoutConfigGood(t *testing.T) {
	b, err := os.ReadFile("../cmd/satbooth/testdata/config-example.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Payout == nil || c.Payout.Network != "testnet" || len(c.Payout.ApprovalKeys) != 1 {
		t.Fatalf("example payout block: %+v", c.Payout)
	}
	// Absent means refunds are off.
	c, err = ParseConfig(enc(t, goodConfig()))
	if err != nil || c.Payout != nil {
		t.Fatalf("no payout block: %+v %v", c.Payout, err)
	}
	// The defaults and the edges.
	c, err = ParseConfig(withPayout(t, func(p map[string]any) {
		delete(p, "timeout_seconds")
		delete(p, "budget_alert_msat")
		p["max_msat"], p["daily_msat"] = 1000, 1000
	}))
	if err != nil || c.Payout.TimeoutSeconds != 70 || c.Payout.BudgetAlertMsat != 0 {
		t.Fatalf("defaults: %+v %v", c.Payout, err)
	}
	rc, err := c.Payout.validate()
	if err != nil || rc.MaxMsat != 1000 || rc.Network != approval.Regtest || rc.Keys == nil {
		t.Fatalf("refund.Config: %+v %v", rc, err)
	}
	top := int64(1) << 53
	top -= top % 1000
	for _, n := range []string{"mainnet", "testnet", "signet", "regtest"} {
		m := goodConfig()
		m["nwc"].(map[string]any)["network"] = n // the payout network equals nwc.network
		p := payoutBlock()
		p["network"], p["max_msat"], p["daily_msat"], p["budget_alert_msat"] = n, top, top, 1<<53
		m["payout"] = p
		if _, err := ParseConfig(enc(t, m)); err != nil {
			t.Errorf("network %s at the top: %v", n, err)
		}
	}
	eight := make([]any, 8)
	for i := range eight {
		pub, _ := payoutPair(byte(10 + i))
		eight[i] = hex.EncodeToString(pub)
	}
	if _, err := ParseConfig(withPayout(t, func(p map[string]any) { p["approval_keys"] = eight })); err != nil {
		t.Errorf("eight approval keys: %v", err)
	}
}

// Each field's bound is refused once, with a fixed text that names the key and carries no value.
func TestPayoutConfigRefusals(t *testing.T) {
	pub1, _ := payoutPair(1)
	k1 := hex.EncodeToString(pub1)
	nine := make([]any, 9)
	for i := range nine {
		pub, _ := payoutPair(byte(20 + i))
		nine[i] = hex.EncodeToString(pub)
	}
	smallOrder := "0100000000000000000000000000000000000000000000000000000000000000" // the identity point
	cases := []struct {
		name string
		mod  func(p map[string]any)
		want string
	}{
		{"relay_url missing", func(p map[string]any) { delete(p, "relay_url") }, "payout.relay_url"},
		{"relay_url http", func(p map[string]any) { p["relay_url"] = "http://127.0.0.1:1" }, "payout.relay_url"},
		{"wallet_pubkey missing", func(p map[string]any) { delete(p, "wallet_pubkey") }, "payout.wallet_pubkey"},
		{"wallet_pubkey short", func(p map[string]any) { p["wallet_pubkey"] = "abcd" }, "payout.wallet_pubkey"},
		{"client_key_file missing", func(p map[string]any) { delete(p, "client_key_file") }, "payout.client_key_file"},
		{"timeout 9", func(p map[string]any) { p["timeout_seconds"] = 9 }, "payout.timeout_seconds"},
		{"timeout 601", func(p map[string]any) { p["timeout_seconds"] = 601 }, "payout.timeout_seconds"},
		{"timeout negative", func(p map[string]any) { p["timeout_seconds"] = -1 }, "payout.timeout_seconds"},
		{"network missing", func(p map[string]any) { delete(p, "network") }, "payout.network"},
		{"network Regtest", func(p map[string]any) { p["network"] = "Regtest" }, "payout.network"},
		{"network testnet4", func(p map[string]any) { p["network"] = "testnet4" }, "payout.network"},
		{"approval_keys missing", func(p map[string]any) { delete(p, "approval_keys") }, "payout.approval_keys"},
		{"approval_keys empty", func(p map[string]any) { p["approval_keys"] = []any{} }, "payout.approval_keys"},
		{"approval_keys nine", func(p map[string]any) { p["approval_keys"] = nine }, "payout.approval_keys"},
		{"approval_keys duplicate", func(p map[string]any) { p["approval_keys"] = []any{k1, k1} }, "payout.approval_keys"},
		{"approval_keys short", func(p map[string]any) { p["approval_keys"] = []any{k1[:62]} }, "payout.approval_keys"},
		{"approval_keys upper case", func(p map[string]any) { p["approval_keys"] = []any{strings.ToUpper(k1)} }, "payout.approval_keys"},
		{"approval_keys small order", func(p map[string]any) { p["approval_keys"] = []any{smallOrder} }, "payout.approval_keys"},
		{"max_msat missing", func(p map[string]any) { delete(p, "max_msat") }, "payout.max_msat"},
		{"max_msat zero", func(p map[string]any) { p["max_msat"] = 0 }, "payout.max_msat"},
		{"max_msat negative", func(p map[string]any) { p["max_msat"] = -1000 }, "payout.max_msat"},
		{"max_msat not whole sats", func(p map[string]any) { p["max_msat"] = 1500 }, "payout.max_msat"},
		{"max_msat above daily", func(p map[string]any) { p["max_msat"], p["daily_msat"] = 2000, 1000 }, "payout.max_msat"},
		{"daily_msat missing", func(p map[string]any) { delete(p, "daily_msat") }, "payout.max_msat"},
		{"daily_msat not whole sats", func(p map[string]any) { p["daily_msat"] = 100_000_500 }, "payout.max_msat"},
		{"daily_msat above 2^53", func(p map[string]any) { p["daily_msat"] = (int64(1) << 53) + 1000 }, "payout.max_msat"},
		{"daily_msat huge", func(p map[string]any) { p["daily_msat"] = int64(math.MaxInt64) - int64(math.MaxInt64)%1000 }, "payout.max_msat"},
		{"budget_alert negative", func(p map[string]any) { p["budget_alert_msat"] = -1 }, "payout.budget_alert_msat"},
		{"budget_alert above 2^53", func(p map[string]any) { p["budget_alert_msat"] = (int64(1) << 53) + 1 }, "payout.budget_alert_msat"},
	}
	for _, tc := range cases {
		_, err := ParseConfig(withPayout(t, tc.mod))
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %q does not name %s", tc.name, err, tc.want)
		}
	}
	// A wrong type or an unknown key is the shape error; no text echoes the value.
	for _, mod := range []func(p map[string]any){
		func(p map[string]any) { p["max_msat"] = "SECRETISH-VALUE-123" },
		func(p map[string]any) { p["approval_keys"] = "SECRETISH-VALUE-123" },
		func(p map[string]any) { p["SECRETISH-VALUE-123"] = 1 },
		func(p map[string]any) { p["network"] = "SECRETISH-VALUE-123" },
		func(p map[string]any) { p["approval_keys"] = []any{"SECRETISH-VALUE-123"} },
		func(p map[string]any) { p["relay_url"] = "ws://SECRETISH-VALUE-123:x@127.0.0.1:1" },
	} {
		if _, err := ParseConfig(withPayout(t, mod)); err == nil || strings.Contains(err.Error(), "SECRETISH") {
			t.Errorf("echo or accepted: %v", err)
		}
	}
}

func TestPayoutKeyMatching(t *testing.T) {
	base := string(withPayout(t, nil))
	for name, tc := range map[string][2]string{
		"case variant in payout": {`"max_msat"`, `"Max_Msat"`},
		"case variant of block":  {`"payout"`, `"Payout"`},
		"duplicate in payout":    {`"network"`, `"network":"regtest","network"`},
		"unknown key":            {`"network"`, `"extra":1,"network"`},
		"block as an array":      {`"payout":{`, `"payout":[{`},
		"keys as an object":      {`"approval_keys":[`, `"approval_keys":{`},
	} {
		if !strings.Contains(base, tc[0]) {
			t.Fatalf("%s: key not found", name)
		}
		if _, err := ParseConfig([]byte(strings.Replace(base, tc[0], tc[1], 1))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// CheckFiles proves the payout client key loads: a loose or missing file is refused with the key's name.
func TestPayoutCheckFiles(t *testing.T) {
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	payPath := filepath.Join(dir, "payout.key")
	k, _ := nostr.GenerateSecretKey()
	if err := nostr.WriteNewSecretKeyFile(payPath, k); err != nil {
		t.Fatal(err)
	}
	k.Zero()
	m := goodConfig()
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	m["webhooks"] = []any{map[string]any{"name": "main", "url": "https://merchant.example.com/hook", "secret_file": whPath}}
	p := payoutBlock()
	p["client_key_file"] = payPath
	m["payout"] = p
	c, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFiles(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(payPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFiles(); err == nil || !strings.Contains(err.Error(), "payout.client_key_file") {
		t.Fatalf("loose payout key: %v", err)
	}
	os.Remove(payPath)
	if err := c.CheckFiles(); err == nil || !strings.Contains(err.Error(), "payout.client_key_file") {
		t.Fatalf("missing payout key: %v", err)
	}
}

// payoutWallet is the scripted payout wallet with get_info, which Run's start-up checks ask of it.
type payoutWallet struct {
	*refundtest.Wallet
	methods []string
	infoErr error
	network string // the network get_info states ("" states none)
}

func (w payoutWallet) GetInfo(context.Context) (*nwc.Info, error) {
	if w.infoErr != nil {
		return nil, w.infoErr
	}
	return &nwc.Info{Network: w.network, Methods: w.methods}, nil
}

var goodPayoutMethods = []string{"pay_invoice", "lookup_invoice", "get_info", "get_budget"}

func newPayoutWallet() payoutWallet {
	w := refundtest.New()
	w.Budget(&nwc.Budget{Set: true, UsedMsat: 0, TotalMsat: 1_000_000_000, RenewalPeriod: "never"}, nil)
	return payoutWallet{Wallet: w, methods: goodPayoutMethods, network: "regtest"}
}

// payoutFiles writes the fixtures and a payout client key, and returns a Config with (or without) the payout block.
func payoutFiles(t *testing.T, withBlock bool, mod func(dir string, m, p map[string]any)) *Config {
	t.Helper()
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	payPath := filepath.Join(dir, "payout.key")
	k, _ := nostr.GenerateSecretKey()
	if err := nostr.WriteNewSecretKeyFile(payPath, k); err != nil {
		t.Fatal(err)
	}
	k.Zero()
	m := goodConfig()
	m["data_dir"] = filepath.Join(dir, "data")
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	m["webhooks"] = []any{map[string]any{"name": "main", "url": "http://127.0.0.1:9", "secret_file": whPath}}
	p := payoutBlock()
	p["client_key_file"] = payPath
	if mod != nil {
		mod(dir, m, p)
	}
	if withBlock {
		m["payout"] = p
	}
	c, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// refuses runs Run and expects a start-up failure with the given log reason, and no secret or wallet text in the error.
func startRefuses(t *testing.T, cfg *Config, reason, text string, opts ...Option) (error, string) {
	t.Helper()
	logs := &syncBuf{}
	opts = append(opts, WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), cfg, opts...) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not refuse")
	}
	if err == nil {
		t.Fatalf("Run started; want a refusal for %s", reason)
	}
	if got := reasonOf(t, logs.String()); got != reason {
		t.Fatalf("reason %q, want %q (error %v, logs %s)", got, reason, err, logs.String())
	}
	if !strings.Contains(err.Error(), text) {
		t.Fatalf("error %q lacks %q", err, text)
	}
	if strings.Contains(logs.String(), logStarted) {
		t.Fatal("the service started")
	}
	return err, logs.String()
}

func TestStartRefusesPayoutSameWalletKey(t *testing.T) {
	cfg := payoutFiles(t, true, func(_ string, m, p map[string]any) { p["wallet_pubkey"] = goodPub })
	startRefuses(t, cfg, "payout", "must differ", WithWallet(newFakeWallet()), WithPayoutWallet(newPayoutWallet()))
}

func TestStartRefusesPayoutSameClientKey(t *testing.T) {
	cfg := payoutFiles(t, true, func(_ string, m, p map[string]any) {
		p["client_key_file"] = m["nwc"].(map[string]any)["client_key_file"]
	})
	startRefuses(t, cfg, "payout", "must differ", WithWallet(newFakeWallet()), WithPayoutWallet(newPayoutWallet()))
}

func TestStartRefusesPayoutMethods(t *testing.T) {
	plus := func(extra string) []string { return append([]string{extra}, goodPayoutMethods...) }
	for name, tc := range map[string]struct {
		methods []string
		text    string
	}{
		"no lookup_invoice":      {[]string{"pay_invoice", "get_info", "get_budget"}, "must hold pay_invoice and lookup_invoice"},
		"no pay_invoice":         {[]string{"lookup_invoice", "get_info", "get_budget"}, "must hold pay_invoice and lookup_invoice"},
		"no methods":             {nil, "must hold pay_invoice and lookup_invoice"},
		"make_invoice as well":   {plus("make_invoice"), `holds the method "make_invoice"`},
		"list_transactions also": {plus("list_transactions"), `holds the method "list_transactions"`},
		"notifications also":     {plus("notifications"), `holds the method "notifications"`},
		"sign_message also":      {plus("sign_message"), unknownPayoutMethod},
		"a preimage also":        {plus(preimageLike), unknownPayoutMethod},
		"odd bytes also":         {plus("pay\n\x00" + strings.Repeat("x", 100)), unknownPayoutMethod},
	} {
		t.Run(name, func(t *testing.T) {
			pw := newPayoutWallet()
			pw.methods = tc.methods
			err, logs := startRefuses(t, payoutFiles(t, true, nil), "payout_connection", tc.text, WithWallet(newFakeWallet()),
				WithPayoutWallet(pw))
			for _, m := range tc.methods { // only SatBooth's own method names are ever named
				known := slices.Contains(invoiceMethods, m) || slices.Contains(payoutMethods, m)
				if !known && (strings.Contains(err.Error(), m) || strings.Contains(logs, m)) {
					t.Fatalf("%q echoed: %v / %s", m, err, logs)
				}
			}
			if strings.Contains(tc.text, "holds") && !strings.Contains(err.Error(), payoutHint) {
				t.Fatalf("no hint: %v", err)
			}
		})
	}
	t.Run("get_info fails", func(t *testing.T) {
		pw := newPayoutWallet()
		pw.infoErr = &nwc.WalletError{Code: "RESTRICTED", Message: "SECRETISH"}
		startRefuses(t, payoutFiles(t, true, nil), "payout_connection", "cannot read get_info", WithWallet(newFakeWallet()), WithPayoutWallet(pw))
	})
	// What the hub grants with pay_invoice is allowed.
	t.Run("hub extras are allowed", func(t *testing.T) {
		pw := newPayoutWallet()
		pw.methods = []string{"pay_invoice", "lookup_invoice", "pay_keysend", "multi_pay_invoice", "multi_pay_keysend", "get_info", "get_budget"}
		startRigWith(t, payoutRig(), WithPayoutWallet(pw))
	})
}

func TestStartRefusesPayoutBudget(t *testing.T) {
	for name, b := range map[string]*nwc.Budget{
		"not set":         {},
		"renews daily":    {Set: true, TotalMsat: 1_000_000_000, RenewalPeriod: "daily"},
		"renewal absent":  {Set: true, TotalMsat: 1_000_000_000},
		"renews yearly":   {Set: true, TotalMsat: 1_000_000_000, RenewalPeriod: "yearly"},
		"renewal Never":   {Set: true, TotalMsat: 1_000_000_000, RenewalPeriod: "Never"},
		"renewal 'never ": {Set: true, TotalMsat: 1_000_000_000, RenewalPeriod: "never "},
	} {
		t.Run(name, func(t *testing.T) {
			pw := newPayoutWallet()
			pw.Budget(b, nil)
			startRefuses(t, payoutFiles(t, true, nil), "payout_connection", "never renewed", WithWallet(newFakeWallet()), WithPayoutWallet(pw))
		})
	}
	t.Run("get_budget fails", func(t *testing.T) {
		pw := newPayoutWallet()
		pw.Budget(nil, &nwc.WalletError{Code: "RESTRICTED", Message: "x"})
		startRefuses(t, payoutFiles(t, true, nil), "payout_connection", "cannot read get_budget", WithWallet(newFakeWallet()), WithPayoutWallet(pw))
	})
}

// unknownPayoutMethod is the fixed refusal for a payout-connection method SatBooth does not know, and payoutHint how
// to fix any refused payout method.
const (
	payoutHint          = "; pair it with satbooth-pair -connection payout"
	unknownPayoutMethod = "the payout connection holds a method that refunds do not allow" + payoutHint
)

// unknownInvoiceMethod is the fixed refusal for an invoice-connection method SatBooth does not know.
const unknownInvoiceMethod = "the invoice connection holds a method SatBooth does not know"

// preimageLike is 64 hex characters, the shape of a payment preimage, as a hub might put one in a method list.
var preimageLike = strings.Repeat("5e", 32)

// measuredInvoiceMethods is the invoice connection's method list as the hub reports it for SatBooth's own pairing
// (qualify/REPORT.md, "13194 content, invoice connection").
var measuredInvoiceMethods = []string{"make_invoice", "make_hold_invoice", "settle_hold_invoice", "cancel_hold_invoice",
	"lookup_invoice", "list_transactions", "get_info", "get_budget", "notifications"}

// The invoice connection holds only allowlisted methods, make_invoice and lookup_invoice among them, with a payout
// block and without one.
func TestStartRefusesInvoiceConnectionMethods(t *testing.T) {
	with := func(extra string) []string { return append(slices.Clone(measuredInvoiceMethods), extra) }
	cases := map[string]struct {
		methods []string
		text    string
	}{
		"empty list":         {[]string{}, "lists no methods"},
		"pay_invoice":        {with("pay_invoice"), `holds the method "pay_invoice"`},
		"pay_keysend":        {with("pay_keysend"), `holds the method "pay_keysend"`},
		"multi_pay_invoice":  {with("multi_pay_invoice"), `holds the method "multi_pay_invoice"`},
		"multi_pay_keysend":  {with("multi_pay_keysend"), `holds the method "multi_pay_keysend"`},
		"unknown method":     {with("sign_message"), unknownInvoiceMethod},
		"odd bytes":          {with("pay\n\x00" + strings.Repeat("x", 100)), unknownInvoiceMethod},
		"a preimage":         {with(preimageLike), unknownInvoiceMethod},
		"no make_invoice":    {[]string{"lookup_invoice", "get_info"}, "must hold make_invoice and lookup_invoice"},
		"no lookup_invoice":  {[]string{"make_invoice", "get_info"}, "must hold make_invoice and lookup_invoice"},
		"pay only, no makes": {[]string{"pay_invoice"}, `holds the method "pay_invoice"`},
		"capitalised make":   {[]string{"Make_invoice", "lookup_invoice"}, unknownInvoiceMethod},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for _, withBlock := range []bool{true, false} {
				fw := newFakeWallet()
				fw.methods = tc.methods
				opts := []Option{WithWallet(fw)}
				if withBlock {
					opts = append(opts, WithPayoutWallet(newPayoutWallet()))
				}
				err, logs := startRefuses(t, payoutFiles(t, withBlock, nil), "invoice_connection", tc.text, opts...)
				for _, m := range tc.methods { // only SatBooth's own method names are ever named
					known := slices.Contains(invoiceMethods, m) || slices.Contains(payoutMethods, m)
					if !known && (strings.Contains(err.Error(), m) || strings.Contains(logs, m)) {
						t.Fatalf("%q echoed: %v / %s", m, err, logs)
					}
				}
			}
		})
	}
	t.Run("get_info fails", func(t *testing.T) {
		fw := newFakeWallet()
		fw.infoErr = &nwc.WalletError{Code: "RESTRICTED", Message: "x"}
		startRefuses(t, payoutFiles(t, false, nil), "invoice_connection", "cannot read get_info", WithWallet(fw))
	})
	t.Run("nil info", func(t *testing.T) {
		startRefuses(t, payoutFiles(t, false, nil), "invoice_connection", "cannot read get_info",
			WithWallet(nilInfoWallet{newFakeWallet()}))
		startRefuses(t, payoutFiles(t, true, nil), "payout_connection", "cannot read get_info", WithWallet(newFakeWallet()),
			WithPayoutWallet(nilInfoPayout{newPayoutWallet()}))
	})
	// The measured list, and its two required methods alone, start.
	t.Run("measured list starts", func(t *testing.T) {
		for _, m := range [][]string{measuredInvoiceMethods, {"make_invoice", "lookup_invoice"}} {
			fw := newFakeWallet()
			fw.methods = m
			startRigWith(t, nil, WithWallet(fw))
		}
	})
}

// nilInfoWallet and nilInfoPayout answer get_info with neither an answer nor an error.
type nilInfoWallet struct{ *fakeWallet }

func (nilInfoWallet) GetInfo(context.Context) (*nwc.Info, error) { return nil, nil }

type nilInfoPayout struct{ payoutWallet }

func (nilInfoPayout) GetInfo(context.Context) (*nwc.Info, error) { return nil, nil }

// A wallet that cannot answer get_info is refused: a test double cannot skip a check.
func TestStartRefusesWalletWithoutGetInfo(t *testing.T) {
	startRefuses(t, payoutFiles(t, false, nil), "invoice_connection", "cannot be checked", WithWallet(noInfoWallet{newFakeWallet()}))
	startRefuses(t, payoutFiles(t, true, nil), "payout_connection", "cannot be checked", WithWallet(newFakeWallet()),
		WithPayoutWallet(noInfoPayout{refundtest.New()}))
}

type noInfoWallet struct{ *fakeWallet }

func (noInfoWallet) GetInfo() {} // hides fakeWallet's GetInfo (a different signature)

type noInfoPayout struct{ *refundtest.Wallet }

// An open refund with no payout block: the store holds a refund nothing would send or resolve.
func TestStartRefusesOpenRefundWithoutPayoutBlock(t *testing.T) {
	cfg := payoutFiles(t, false, nil)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	o, _, err := st.CreateOrder(ctx, store.Order{AccountID: "acct1",
		MerchantRef: "ref1", AmountMsat: 21_000_000, CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	id, key := strings.Repeat("c", 32), strings.Repeat("d", 16)
	exp := now.Add(24 * time.Hour)
	ap := hex.EncodeToString([]byte("satbooth/refund-approval/v1")) + "03" + key + id + o.ID +
		hex.EncodeToString(be64(1_000_000)) + hex.EncodeToString(be64(exp.Unix())) + strings.Repeat("5a", 64)
	if err := st.AddRefund(ctx, store.Refund{ID: id, OrderID: o.ID, KeyID: key, Approval: ap, Invoice: "lnbcrt" + strings.Repeat("q", 40),
		AmountMsat: 1_000_000, ClaimedAt: now, ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
	if open, err := st.OpenRefunds(ctx); err != nil || len(open) != 1 {
		t.Fatalf("seed: %v %v", open, err)
	}
	st.Close()
	startRefuses(t, cfg, "payout", "no payout block", WithWallet(newFakeWallet()))
}

func lnr(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func be64(n int64) []byte {
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = byte(n)
		n >>= 8
	}
	return b
}

// payoutRig is the rig's configuration change for refunds: the payout block, a payout client key and an API key file
// that also holds refunds:write and refunds:read keys.
func payoutRig() func(dir string, m map[string]any) {
	return func(dir string, m map[string]any) {
		payPath := filepath.Join(dir, "payout.key")
		k, _ := nostr.GenerateSecretKey()
		if err := nostr.WriteNewSecretKeyFile(payPath, k); err != nil {
			panic(err)
		}
		k.Zero()
		apiPath := filepath.Join(dir, "api-keys-refunds.json")
		if err := os.WriteFile(apiPath, keysJSON([3]string{"merchant", "orders:write", testAPIKey},
			[3]string{"refunder", "refunds:write", testRefundWriteKey}, [3]string{"refreader", "refunds:read", testRefundReadKey}), 0o600); err != nil {
			panic(err)
		}
		m["api_keys_file"] = apiPath
		p := payoutBlock()
		p["client_key_file"] = payPath
		m["payout"] = p
	}
}

const (
	testRefundWriteKey = "refund-write-key-0123456789abcdef"
	testRefundReadKey  = "refund-read-key-0123456789abcdef"
)

// A started service with both wallets serves the refund routes: a claim over HTTP reaches refund.Claim (an unknown
// order is its unknown_order, not the API's refunds_disabled), and the capabilities are enforced.
func TestRunServesRefundRoutes(t *testing.T) {
	pw := newPayoutWallet()
	r := startRigWith(t, payoutRig(), WithPayoutWallet(pw))
	pub, priv := payoutPair(7)
	refundID, orderID := [16]byte{1}, [16]byte{2}
	ap := approval.SignRefund(approval.Refund{Network: approval.Regtest, KeyID: approval.KeyID(pub), RefundID: refundID, OrderID: orderID,
		AmountMsat: 1_000_000, ExpiresAt: time.Now().Add(time.Hour).Unix()}, priv)
	body, _ := json.Marshal(map[string]string{"approval": ap, "invoice": refundtest.Invoice(approval.Regtest, 1)})

	code, b := do(t, "POST", r.prvURL+"/v1/refunds", testRefundWriteKey, body)
	if code != http.StatusNotFound || !strings.Contains(string(b), "unknown_order") {
		t.Fatalf("claim: %d %s", code, b)
	}
	if code, b = do(t, "POST", r.prvURL+"/v1/refunds", testRefundReadKey, body); code != http.StatusForbidden {
		t.Fatalf("claim with a read key: %d %s", code, b)
	}
	if code, b = do(t, "GET", r.prvURL+"/v1/refunds/"+hex.EncodeToString(refundID[:]), testRefundReadKey, nil); code != http.StatusNotFound ||
		strings.Contains(string(b), "refunds_disabled") {
		t.Fatalf("get: %d %s", code, b)
	}
	if n := pw.TotalCalls(); n < 1 {
		t.Fatalf("the payout wallet was never asked (the start-up checks): %d", n)
	}
}

// Without the block every refund route answers 404 refunds_disabled, and the order view still carries its figures.
func TestRunWithoutPayoutBlockRefundsDisabled(t *testing.T) {
	r := startRigWith(t, func(dir string, m map[string]any) {
		apiPath := filepath.Join(dir, "api-keys-refunds.json")
		if err := os.WriteFile(apiPath, keysJSON([3]string{"refunder", "refunds:write", testRefundWriteKey}), 0o600); err != nil {
			panic(err)
		}
		m["api_keys_file"] = apiPath
	})
	code, b := do(t, "POST", r.prvURL+"/v1/refunds", testRefundWriteKey, []byte(`{"approval":"","invoice":""}`))
	if code != http.StatusNotFound || !strings.Contains(string(b), "refunds_disabled") {
		t.Fatalf("claim: %d %s", code, b)
	}
}

// Run closes the payout key's use at exit and leaves no goroutine behind: a start and stop with refunds on returns nil.
func TestRunWithPayoutStopsCleanly(t *testing.T) {
	cfg := payoutFiles(t, true, nil)
	ctx, cancel := context.WithCancel(context.Background())
	logs := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, WithWallet(newFakeWallet()), WithPayoutWallet(newPayoutWallet()),
			WithLogger(slog.New(slog.NewJSONHandler(logs, nil))), WithListeners(lnr(t), lnr(t)))
	}()
	eventually(t, "the service to start", func() bool { return strings.Contains(logs.String(), logStarted) })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
	}
}
