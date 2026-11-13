package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/monero"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/rates"
	"github.com/satbooth/satbooth/store"
)

const goodPub = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"

// goodConfig is a valid configuration as a generic map, so a test can change or drop one key.
func goodConfig() map[string]any {
	return map[string]any{
		"public_listen":     "192.0.2.10:8080",
		"private_listen":    "127.0.0.1:8081",
		"checkout_base_url": "https://pay.example.com",
		"data_dir":          "/var/lib/satbooth",
		"nwc": map[string]any{
			"relay_url":       "ws://127.0.0.1:7777",
			"wallet_pubkey":   goodPub,
			"client_key_file": "/etc/satbooth/client.key",
			"timeout_seconds": 70,
			"network":         "regtest",
		},
		"api_keys_file": "/etc/satbooth/api-keys.json",
		"webhooks": []any{map[string]any{
			"name":        "main",
			"url":         "https://merchant.example.com/hook",
			"secret_file": "/etc/satbooth/webhook.secret",
		}},
		"limits": map[string]any{
			"rate_per_sec": 1, "burst": 10, "max_open": 1000, "new_share": 0.2, "daily_count": 5000,
			"daily_msat": 100000000000, "min_msat": 1000, "max_msat": 10000000000,
		},
		"invoice_expiry_minutes": 20,
	}
}

func enc(t testing.TB, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// with returns a copy of the good config with the dotted path set to v (nil v deletes the key).
func with(t testing.TB, path string, v any) []byte {
	t.Helper()
	m := goodConfig()
	parts := strings.Split(path, ".")
	cur := parent(m, parts)
	last := parts[len(parts)-1]
	if v == nil {
		delete(cur, last)
	} else {
		cur[last] = v
	}
	return enc(t, m)
}

func TestParseConfigGood(t *testing.T) {
	c, err := ParseConfig(enc(t, goodConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicListen != "192.0.2.10:8080" || c.PrivateListen != "127.0.0.1:8081" || c.CheckoutBaseURL != "https://pay.example.com" ||
		c.DataDir != "/var/lib/satbooth" || c.APIKeysFile != "/etc/satbooth/api-keys.json" || c.InvoiceExpiryMinutes != 20 {
		t.Fatalf("bad values: %+v", c)
	}
	if c.NWC.RelayURL != "ws://127.0.0.1:7777" || c.NWC.WalletPubKey != goodPub || c.NWC.ClientKeyFile != "/etc/satbooth/client.key" || c.NWC.TimeoutSeconds != 70 {
		t.Fatalf("bad nwc: %+v", c.NWC)
	}
	if len(c.Webhooks) != 1 || c.Webhooks[0] != (WebhookEndpoint{Name: "main", URL: "https://merchant.example.com/hook", SecretFile: "/etc/satbooth/webhook.secret"}) {
		t.Fatalf("bad webhooks: %+v", c.Webhooks)
	}
	l := c.Limits
	if l.RatePerSec != 1 || l.Burst != 10 || l.MaxOpen != 1000 || l.NewShare != 0.2 || l.DailyCount != 5000 ||
		l.DailyMsat != 100000000000 || l.MinMsat != 1000 || l.MaxMsat != 10000000000 {
		t.Fatalf("bad limits: %+v", l)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	m := goodConfig()
	delete(m, "invoice_expiry_minutes")
	delete(m["nwc"].(map[string]any), "timeout_seconds")
	l := m["limits"].(map[string]any)
	for _, k := range []string{"rate_per_sec", "burst", "max_open", "new_share", "daily_count", "min_msat"} {
		delete(l, k)
	}
	c, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if c.InvoiceExpiryMinutes != 20 || c.NWC.TimeoutSeconds != 70 {
		t.Fatalf("defaults: expiry %d timeout %d", c.InvoiceExpiryMinutes, c.NWC.TimeoutSeconds)
	}
}

func TestParseConfigRefusals(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"not json", []byte("public_listen")},
		{"array", []byte("[]")},
		{"null", []byte("null")},
		{"trailing data", append(enc(t, goodConfig()), []byte(" {}")...)},
		{"unknown top key", with(t, "extra", "x")},
		{"unknown nwc key", with(t, "nwc.extra", 1)},
		{"unknown webhook key", with(t, "webhooks.0.extra", 1)},
		{"unknown limits key", with(t, "limits.extra", 1)},
		{"case variant key", bytes.Replace(enc(t, goodConfig()), []byte(`"data_dir"`), []byte(`"Data_Dir"`), 1)},
		{"duplicate top key", []byte(strings.Replace(string(enc(t, goodConfig())), `"data_dir"`, `"data_dir":"/x","data_dir"`, 1))},
		{"duplicate nested key", []byte(strings.Replace(string(enc(t, goodConfig())), `"burst"`, `"burst":3,"burst"`, 1))},
		{"missing public_listen", with(t, "public_listen", nil)},
		{"missing private_listen", with(t, "private_listen", nil)},
		{"missing checkout_base_url", with(t, "checkout_base_url", nil)},
		{"missing data_dir", with(t, "data_dir", nil)},
		{"missing nwc", with(t, "nwc", nil)},
		{"missing relay_url", with(t, "nwc.relay_url", nil)},
		{"missing wallet_pubkey", with(t, "nwc.wallet_pubkey", nil)},
		{"missing client_key_file", with(t, "nwc.client_key_file", nil)},
		{"missing api_keys_file", with(t, "api_keys_file", nil)},
		{"missing webhooks", with(t, "webhooks", nil)},
		{"missing webhook url", with(t, "webhooks.0.url", nil)},
		{"missing secret_file", with(t, "webhooks.0.secret_file", nil)},
		{"missing limits", with(t, "limits", nil)},
		{"missing daily_msat", with(t, "limits.daily_msat", nil)},
		{"missing max_msat", with(t, "limits.max_msat", nil)},
		{"wrong type listen", with(t, "public_listen", 5)},
		{"wrong type nwc", with(t, "nwc", "x")},
		{"wrong type expiry", with(t, "invoice_expiry_minutes", "20")},
		{"fractional expiry", with(t, "invoice_expiry_minutes", 20.5)},
		{"wildcard public", with(t, "public_listen", "0.0.0.0:8080")},
		{"wildcard v6 public", with(t, "public_listen", "[::]:8080")},
		{"hostname public", with(t, "public_listen", "localhost:8080")},
		{"hostname private", with(t, "private_listen", "pay.example.com:8081")},
		{"empty host", with(t, "private_listen", ":8081")},
		{"no port", with(t, "public_listen", "192.0.2.10")},
		{"port 0", with(t, "public_listen", "192.0.2.10:0")},
		{"port 65536", with(t, "public_listen", "192.0.2.10:65536")},
		{"port leading zero", with(t, "public_listen", "192.0.2.10:08080")},
		{"zone", with(t, "private_listen", "[fe80::1%eth0]:8081")},
		{"same listeners", with(t, "private_listen", "192.0.2.10:8080")},
		{"same listeners v6 spelling", func() []byte {
			m := goodConfig()
			m["public_listen"], m["private_listen"] = "[2001:db8::1]:80", "[2001:0db8:0::1]:80"
			return enc(t, m)
		}()},
		{"base ftp", with(t, "checkout_base_url", "ftp://pay.example.com")},
		{"base no scheme", with(t, "checkout_base_url", "pay.example.com")},
		{"base no host", with(t, "checkout_base_url", "https:///x")},
		{"base userinfo", with(t, "checkout_base_url", "https://u:p@pay.example.com")},
		{"base query", with(t, "checkout_base_url", "https://pay.example.com/?a=1")},
		{"base empty query", with(t, "checkout_base_url", "https://pay.example.com/?")},
		{"base fragment", with(t, "checkout_base_url", "https://pay.example.com/#x")},
		{"base space", with(t, "checkout_base_url", "https://pay.example.com/a b")},
		{"base control", with(t, "checkout_base_url", "https://pay.example.com/\u0001")},
		{"base opaque", with(t, "checkout_base_url", "https:pay.example.com")},
		{"base too long", with(t, "checkout_base_url", "https://pay.example.com/"+strings.Repeat("a", 600))},
		{"relay http", with(t, "nwc.relay_url", "http://127.0.0.1:7777")},
		{"relay userinfo", with(t, "nwc.relay_url", "ws://u@127.0.0.1:7777")},
		{"relay fragment", with(t, "nwc.relay_url", "ws://127.0.0.1:7777/#x")},
		{"relay no host", with(t, "nwc.relay_url", "ws://")},
		{"wallet pubkey short", with(t, "nwc.wallet_pubkey", "abcd")},
		{"wallet pubkey upper", with(t, "nwc.wallet_pubkey", strings.ToUpper(goodPub))},
		{"wallet pubkey off curve", with(t, "nwc.wallet_pubkey", strings.Repeat("f", 64))},
		{"timeout 9", with(t, "nwc.timeout_seconds", 9)},
		{"timeout 601", with(t, "nwc.timeout_seconds", 601)},
		{"timeout wraps to 70", with(t, "nwc.timeout_seconds", int64(1)<<55+70)},
		{"timeout wraps to 10", with(t, "nwc.timeout_seconds", int64(1)<<55+10)},
		{"timeout max int64", with(t, "nwc.timeout_seconds", int64(math.MaxInt64))},
		{"timeout negative", with(t, "nwc.timeout_seconds", -1)},
		{"expiry 14", with(t, "invoice_expiry_minutes", 14)},
		{"expiry 31", with(t, "invoice_expiry_minutes", 31)},
		{"expiry wraps to 20", with(t, "invoice_expiry_minutes", int64(1)<<53+20)},
		{"expiry max int64", with(t, "invoice_expiry_minutes", int64(math.MaxInt64))},
		{"expiry min int64", with(t, "invoice_expiry_minutes", int64(math.MinInt64))},
		{"expiry negative", with(t, "invoice_expiry_minutes", -20)},
		{"webhook ftp", with(t, "webhooks.0.url", "ftp://m.example.com/h")},
		{"webhook userinfo", with(t, "webhooks.0.url", "https://u:p@m.example.com/h")},
		{"webhook fragment", with(t, "webhooks.0.url", "https://m.example.com/h#x")},
		{"webhook space", with(t, "webhooks.0.url", "https://m.example.com/h x")},
		{"webhook no host", with(t, "webhooks.0.url", "https:///h")},
		{"limits negative rate", with(t, "limits.rate_per_sec", -1)},
		{"limits share 1", with(t, "limits.new_share", 1)},
		{"limits share too small", with(t, "limits.new_share", 0.0000001)},
		{"limits max below min", with(t, "limits.max_msat", 500)},
		{"limits max above 2^53", with(t, "limits.max_msat", int64(1)<<54)},
		{"limits negative burst", with(t, "limits.burst", -1)},
		{"limits daily_msat zero", with(t, "limits.daily_msat", 0)},
		{"limits max_open_per_account above max_open", with(t, "limits.max_open_per_account", 1001)},
		{"limits max_open_per_account negative", with(t, "limits.max_open_per_account", -1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseConfig(tc.body)
			if err == nil {
				t.Fatalf("accepted: %+v", c)
			}
			if !strings.HasPrefix(err.Error(), "config: ") {
				t.Fatalf("error without the config prefix: %v", err)
			}
		})
	}
}

// max_open_per_account: parsed and passed to the limiter; 0 or missing takes min(5, max_open); 1 to
// max_open, else service's one fixed limits text.
func TestParseConfigMaxOpenPerAccount(t *testing.T) {
	const invalid = "config: limits are invalid (daily_msat and max_msat are required; see the limits package for the bounds)"
	resolved := func(t *testing.T, body []byte) int {
		t.Helper()
		c, err := ParseConfig(body)
		if err != nil {
			t.Fatal(err)
		}
		l, err := limits.New(c.Limits.limits(), time.Now)
		if err != nil {
			t.Fatal(err)
		}
		return l.Config().MaxOpenPerAccount
	}
	for _, v := range []int{1, 7, 999, 1000} {
		body := with(t, "limits.max_open_per_account", v)
		c, err := ParseConfig(body)
		if err != nil || c.Limits.MaxOpenPerAccount != v || c.Limits.limits().MaxOpenPerAccount != v {
			t.Fatalf("%d: %v %+v", v, err, c)
		}
		if got := resolved(t, body); got != v {
			t.Fatalf("%d: the limiter has %d", v, got)
		}
	}
	if got := resolved(t, with(t, "limits.max_open_per_account", 0)); got != 5 {
		t.Fatalf("0 takes %d, want 5", got)
	}
	if got := resolved(t, enc(t, goodConfig())); got != 5 {
		t.Fatalf("missing takes %d, want 5", got)
	}
	// Ruling 6.1-N: an existing max_open below 5 with no per-account key still starts, capped at max_open.
	m := goodConfig()
	m["limits"].(map[string]any)["max_open"] = 3
	m["limits"].(map[string]any)["new_share"] = 0.5
	if got := resolved(t, enc(t, m)); got != 3 {
		t.Fatalf("max_open 3 takes %d, want 3", got)
	}
	for _, v := range []any{1001, -1, int64(1) << 40, "5", 2.5} {
		_, err := ParseConfig(with(t, "limits.max_open_per_account", v))
		if err == nil {
			t.Fatalf("%v accepted", v)
		}
		switch v.(type) {
		case int, int64: // 2^40 parses into the int field and is refused by the limits, not by JSON
			if err.Error() != invalid {
				t.Fatalf("%v: %q, want the fixed limits text", v, err)
			}
		}
	}
}

// TestListenAccepts: specific IP literals of both families are fine, in any spelling.
func TestListenAccepts(t *testing.T) {
	for _, a := range []string{"127.0.0.1:1", "192.0.2.1:65535", "[::1]:8080", "[2001:db8::1]:443"} {
		if _, err := ParseConfig(with(t, "public_listen", a)); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
}

func TestBaseURLAccepts(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:8080", "https://pay.example.com", "https://pay.example.com/", "https://pay.example.com/shop"} {
		if _, err := ParseConfig(with(t, "checkout_base_url", u)); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestErrorsDoNotEchoValues(t *testing.T) {
	const secretish = "SECRETISH-VALUE-123"
	for _, b := range [][]byte{
		with(t, "nwc.client_key_file", 5),
		with(t, "checkout_base_url", "ftp://"+secretish),
		with(t, "webhooks.0.url", "ftp://"+secretish),
		with(t, "public_listen", secretish),
		with(t, "nwc.wallet_pubkey", secretish),
		// The rates block: the custom URL may carry a token.
		withRates(t, func(r map[string]any) { r["custom_url"] = "http://rates.example.invalid/?token=" + secretish }),
		withRates(t, func(r map[string]any) { r["custom_url"] = "https://rates.example.invalid/?token=" + secretish }),
		withRates(t, func(r map[string]any) { r["custom_url_file"] = "/etc/" + secretish }),
		withRates(t, func(r map[string]any) { r["custom_url"] = "https://" + secretish + ":x@rates.example.invalid/" }),
		withRates(t, func(r map[string]any) { r["custom_url"] = "https://rates.example.invalid/" + secretish + "#f" }),
		withRates(t, func(r map[string]any) { r["custom_url"] = urlOfLen(513) + secretish }),
		withRates(t, func(r map[string]any) { r["providers"] = []any{secretish} }),
		withRates(t, func(r map[string]any) { r["currencies"] = []any{secretish} }),
		withRates(t, func(r map[string]any) { r["margin_percent"] = secretish }),
		withRates(t, func(r map[string]any) { r["max_spread_percent"] = secretish }),
		withRates(t, func(r map[string]any) { r["min_sources"] = secretish }),
		withRates(t, func(r map[string]any) { r[secretish] = 1 }),
		// The monero block: the URL, the user, the address and the network are never echoed.
		withMonero(t, func(x map[string]any) { x["wallet_rpc_url"] = "http://" + secretish + ":x@127.0.0.1:18083" }),
		withMonero(t, func(x map[string]any) { x["wallet_rpc_url"] = "http://127.0.0.1:18083/?t=" + secretish }),
		withMonero(t, func(x map[string]any) { x["wallet_rpc_url"] = "ws://" + secretish }),
		withMonero(t, func(x map[string]any) { x["rpc_user"] = secretish + " x" }),
		withMonero(t, func(x map[string]any) { x["address"] = "9" + secretish + testXMRAddress[1+len(secretish):] }),
		withMonero(t, func(x map[string]any) { x["address"] = secretish }),
		withMonero(t, func(x map[string]any) { x["network"] = secretish }),
		withMonero(t, func(x map[string]any) { x["poll_rate"] = 10001.123456 }),
		withMonero(t, func(x map[string]any) { x[secretish] = 1 }),
	} {
		if _, err := ParseConfig(b); err == nil {
			t.Fatal("accepted")
		} else if strings.Contains(err.Error(), secretish) {
			t.Fatalf("error echoes a value: %v", err)
		}
	}
}

func TestLoadConfigFiles(t *testing.T) {
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	m := goodConfig()
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	m["webhooks"].([]any)[0].(map[string]any)["secret_file"] = whPath
	path := filepath.Join(dir, "config.json")
	write := func(m map[string]any) {
		if err := os.WriteFile(path, enc(t, m), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(m)
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing config accepted")
	}
	// Each file path must exist and pass its loader.
	for _, tc := range []struct{ name, field string }{{"client key", "nwc.client_key_file"}, {"api keys", "api_keys_file"}, {"webhook secret", "webhooks.0.secret_file"}} {
		bad := goodConfig()
		m2 := map[string]any{}
		b, _ := json.Marshal(m)
		_ = json.Unmarshal(b, &m2)
		set(m2, tc.field, filepath.Join(dir, "nope"))
		_ = bad
		write(m2)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("%s: missing file accepted", tc.name)
		}
		// Wrong mode.
		m3 := map[string]any{}
		_ = json.Unmarshal(b, &m3)
		loose := filepath.Join(dir, "loose-"+strings.ReplaceAll(tc.name, " ", "-"))
		src, _ := os.ReadFile(get(m, tc.field))
		if err := os.WriteFile(loose, src, 0o644); err != nil {
			t.Fatal(err)
		}
		set(m3, tc.field, loose)
		write(m3)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("%s: loose file accepted", tc.name)
		}
	}
	// Oversized config.
	if err := os.WriteFile(path, bytes.Repeat([]byte(" "), 1<<20+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("oversized config accepted")
	}
}

func set(m map[string]any, path string, v any) {
	parts := strings.Split(path, ".")
	parent(m, parts)[parts[len(parts)-1]] = v
}

func get(m map[string]any, path string) string {
	parts := strings.Split(path, ".")
	return parent(m, parts)[parts[len(parts)-1]].(string)
}

// parent is the object that holds the last key of the dotted path; a numeric part indexes a list ("webhooks.1.url").
func parent(m map[string]any, parts []string) map[string]any {
	var cur any = m
	for _, p := range parts[:len(parts)-1] {
		if i, err := strconv.Atoi(p); err == nil {
			cur = cur.([]any)[i]
		} else {
			cur = cur.(map[string]any)[p]
		}
	}
	return cur.(map[string]any)
}

// TestConfigMutations: a seeded walk over single-byte mutations of the good config never panics, and
// whatever ParseConfig accepts passes its own invariants and survives a re-encode.
func TestConfigMutations(t *testing.T) {
	base := enc(t, goodConfig())
	rng := rand.New(rand.NewSource(20261103))
	odd := []byte{0, '"', '\\', '{', '}', '[', ']', ',', ':', 0x7f, 0xff, ' ', '\n', '%', '#', '?', '@', '/'}
	for i := 0; i < 20000; i++ {
		b := bytes.Clone(base)
		for n := rng.Intn(3) + 1; n > 0; n-- {
			p := rng.Intn(len(b))
			switch rng.Intn(3) {
			case 0:
				b[p] = odd[rng.Intn(len(odd))]
			case 1:
				b = append(b[:p], b[p+1:]...)
			default:
				b = append(b[:p], append([]byte{odd[rng.Intn(len(odd))]}, b[p:]...)...)
			}
			if len(b) == 0 {
				break
			}
		}
		checkParse(t, b)
	}
}

// checkParse is the invariant shared by the mutation test and the fuzz target.
func checkParse(t testing.TB, b []byte) {
	t.Helper()
	c, err := ParseConfig(b)
	if err != nil {
		if !strings.HasPrefix(err.Error(), "config: ") {
			t.Fatalf("error without prefix: %v", err)
		}
		return
	}
	if c.InvoiceExpiryMinutes < 15 || c.InvoiceExpiryMinutes > 30 || c.NWC.TimeoutSeconds < 10 || c.NWC.TimeoutSeconds > 600 {
		t.Fatalf("accepted out-of-bounds values: %+v", c)
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := ParseConfig(out)
	if err != nil {
		t.Fatalf("accepted config does not survive re-encoding: %v", err)
	}
	if o := c.Onchain; o != nil {
		if (o.PollSeconds != 0 && (o.PollSeconds < 2 || o.PollSeconds > 60)) || o.TimeoutSeconds < 0 || o.TimeoutSeconds > 600 {
			t.Fatalf("accepted out-of-bounds onchain values: %+v", o)
		}
		switch o.Network {
		case "mainnet", "testnet", "signet", "regtest":
		default:
			t.Fatalf("accepted network %q", o.Network)
		}
		if _, err := o.railConfig(); err != nil {
			t.Fatalf("accepted block does not map to a valid onchain.Config: %v", err)
		}
	}
	if r := c.Rates; r != nil {
		if r.MinSources < 2 || r.MinSources > 5 || r.MaxAgeSeconds < 30 || r.MaxAgeSeconds > 3600 || r.FetchSeconds < 10 ||
			r.FetchSeconds > 600 || 2*r.FetchSeconds > r.MaxAgeSeconds || len(r.Currencies) < 1 || len(r.Currencies) > 32 {
			t.Fatalf("accepted out-of-bounds rates values: %+v", r)
		}
		for _, cur := range r.Currencies {
			if !rates.ValidCurrency(cur) {
				t.Fatalf("accepted currency %q", cur)
			}
		}
		sources := len(r.Providers)
		if r.CustomURL != "" && r.CustomURLFile != "" {
			t.Fatal("accepted both custom_url and custom_url_file")
		}
		if r.CustomURL != "" || r.CustomURLFile != "" {
			sources++
		}
		if sources < r.MinSources || sources < 2 { // the custom API never prices alone
			t.Fatalf("accepted %d sources for min_sources %d", sources, r.MinSources)
		}
		custom := r.CustomURL
		if r.CustomURL != "" {
			if u, err := url.Parse(r.CustomURL); err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery {
				t.Fatalf("accepted custom_url %q", r.CustomURL)
			}
		}
		if r.CustomURLFile != "" { // the file is not read here: a URL as one would hold
			custom = "https://rates.example.invalid/s?token=x"
		}
		rc, err := r.ratesConfig(custom, nil, time.Now, nil, c.Monero != nil)
		if err != nil {
			t.Fatalf("accepted block does not map to a rates.Config: %v", err)
		}
		if rc.MarginBps < rates.MinMarginBps || rc.MarginBps > rates.MaxMarginBps || rc.Policy.MaxSpreadBps < 1 || rc.Policy.MaxSpreadBps > 5000 {
			t.Fatalf("accepted out-of-bounds percent: %+v", rc)
		}
		if _, err := rates.New(rc); err != nil {
			t.Fatalf("accepted block does not make a rates.Service: %v", err)
		}
	}
	if x := c.Monero; x != nil {
		mc, err := x.railConfig()
		if err != nil {
			t.Fatalf("accepted block does not map to a valid monero.Config: %v", err)
		}
		if err := mc.Validate(); err != nil || !btcrpc.ValidURL(x.WalletRPCURL) || !btcrpc.ValidName(x.RPCUser) || x.RPCPasswordFile == "" {
			t.Fatalf("accepted monero block out of bounds: %+v %v", x, err)
		}
		if !(x.PollRate == 0 || x.PollRate >= 1 && x.PollRate <= 10000) || x.PollSeconds < 0 || x.PollSeconds > 60 || x.TimeoutSeconds < 0 || x.TimeoutSeconds > 600 {
			t.Fatalf("accepted out-of-bounds monero values: %+v", x)
		}
	}
	if !reflect.DeepEqual(c2, c) {
		t.Fatalf("re-encoding changed the config: %+v != %+v", c, c2)
	}
}

func FuzzConfig(f *testing.F) {
	f.Add(enc(f, goodConfig()))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"public_listen":"1.2.3.4:5","public_listen":"1.2.3.4:6"}`))
	f.Add(with(f, "invoice_expiry_minutes", 31))
	f.Add(with(f, "invoice_expiry_minutes", int64(1)<<53+20))
	f.Add(with(f, "nwc.timeout_seconds", int64(1)<<55+70))
	f.Add(with(f, "checkout_base_url", "https://pay.example.com/?x"))
	f.Add(withOnchain(f, nil))
	f.Add(withOnchain(f, func(o map[string]any) { o["poll_seconds"] = int64(1)<<53 + 4 }))
	f.Add(withOnchain(f, func(o map[string]any) { o["network"] = "testnet4" }))
	f.Add(withRates(f, nil))
	f.Add(withRates(f, func(r map[string]any) {
		delete(r, "custom_url")
		r["custom_url_file"] = "/etc/satbooth/rates.url"
		for _, k := range []string{"margin_percent", "min_sources", "max_spread_percent", "max_age_seconds", "fetch_seconds"} {
			delete(r, k)
		}
	}))
	f.Add(withRates(f, func(r map[string]any) { r["min_sources"] = 1 }))
	f.Add(withRates(f, func(r map[string]any) { r["min_sources"] = 4 }))
	f.Add(withRates(f, func(r map[string]any) { r["fetch_seconds"] = 151 }))
	f.Add(withRates(f, func(r map[string]any) { r["fetch_seconds"], r["max_age_seconds"] = 15, 30 }))
	f.Add(withRates(f, func(r map[string]any) { r["max_age_seconds"] = 3601 }))
	f.Add(withRates(f, func(r map[string]any) { r["margin_percent"] = "-50.01" }))
	f.Add(withRates(f, func(r map[string]any) { r["margin_percent"] = "50" }))
	f.Add(withRates(f, func(r map[string]any) { r["margin_percent"] = 1.5 }))
	f.Add(withRates(f, func(r map[string]any) { r["max_spread_percent"] = "0.01" }))
	f.Add(withRates(f, func(r map[string]any) { r["currencies"] = codes(33) }))
	f.Add(withRates(f, func(r map[string]any) { r["currencies"] = []any{"USD", "ARS"}; r["providers"] = []any{"kraken"} }))
	f.Add(withRates(f, func(r map[string]any) { r["providers"] = []any{"kraken", "kraken"} }))
	f.Add(withRates(f, func(r map[string]any) { r["custom_url"] = urlOfLen(513) }))
	f.Add(withRates(f, func(r map[string]any) { r["custom_url"] = "https://u@rates.example.invalid/" }))
	f.Add(withRates(f, func(r map[string]any) { r["min_sources"] = 5 }))
	f.Add(withRates(f, func(r map[string]any) { r["providers"] = []any{}; r["min_sources"] = 1 }))
	f.Add(withRates(f, func(r map[string]any) { delete(r, "custom_url"); r["min_sources"] = 4 }))
	f.Add(withRates(f, func(r map[string]any) { r["providers"] = []any{}; r["min_sources"] = 2 }))
	f.Add(withRates(f, func(r map[string]any) { r["providers"] = []any{"kraken"}; delete(r, "custom_url") }))
	f.Add(withRates(f, func(r map[string]any) { r["max_age_seconds"], r["fetch_seconds"] = 29, 10 }))
	f.Add(withRates(f, func(r map[string]any) { r["max_age_seconds"], r["fetch_seconds"] = 30, 10 }))
	f.Add(withRates(f, func(r map[string]any) { r["max_spread_percent"] = "0" }))
	f.Add(withRates(f, func(r map[string]any) { r["max_spread_percent"] = "50.01" }))
	f.Add(withRates(f, func(r map[string]any) { r["max_spread_percent"] = "50" }))
	f.Add(withRates(f, func(r map[string]any) { r["fetch_seconds"] = 9 }))
	f.Add(withRates(f, func(r map[string]any) { r["fetch_seconds"] = 10 }))
	f.Add(withRates(f, func(r map[string]any) { r["fetch_seconds"], r["max_age_seconds"] = 600, 1200 }))
	f.Add(withRates(f, func(r map[string]any) { r["fetch_seconds"], r["max_age_seconds"] = 601, 3600 }))
	f.Add(withRates(f, func(r map[string]any) { r["custom_url"] = urlOfLen(512) }))
	f.Add(withRates(f, func(r map[string]any) { r["custom_url"] = "https://rates.example.invalid/s?token=x" }))
	f.Add(withRates(f, func(r map[string]any) { r["custom_url_file"] = "/etc/satbooth/rates.url" }))
	f.Add(withRates(f, func(r map[string]any) { r["currencies"] = []any{} }))
	f.Add(withRates(f, func(r map[string]any) { r["currencies"] = []any{"usd"} }))
	f.Add(withRates(f, func(r map[string]any) { r["currencies"] = []any{"BTC"} }))
	f.Add(withRates(f, func(r map[string]any) { r["currencies"] = []any{"USD", "USD"} }))
	f.Add(withMonero(f, nil))
	f.Add(withMonero(f, func(x map[string]any) { x["poll_rate"] = 0.5 }))
	f.Add(withMonero(f, func(x map[string]any) { x["poll_rate"] = 10000 }))
	f.Add(withMonero(f, func(x map[string]any) { x["poll_rate"] = 1 }))
	f.Add(withMonero(f, func(x map[string]any) { x["poll_seconds"] = int64(1)<<53 + 4 }))
	f.Add(withMonero(f, func(x map[string]any) { x["timeout_seconds"] = int64(1)<<55 + 30 }))
	f.Add(withMonero(f, func(x map[string]any) { x["network"] = "stagenet" }))
	f.Add(withMonero(f, func(x map[string]any) { x["address"] = "9" + testXMRAddress[1:] }))
	f.Add(withMonero(f, func(x map[string]any) { x["account_index"] = int64(1) << 32 }))
	f.Add(withMonero(f, func(x map[string]any) { x["unused_alert"] = 10000 }))
	f.Add(withMonero(f, func(x map[string]any) {
		x["confirmations"] = []any{map[string]any{"up_to_piconero": store.MaxPiconero, "confirmations": 0}, map[string]any{"up_to_piconero": 0, "confirmations": 100}}
	}))
	f.Add(withMonero(f, func(x map[string]any) { x["confirmations"] = []any{map[string]any{"up_to_piconero": 0}} }))
	f.Fuzz(func(t *testing.T, b []byte) { checkParse(t, b) })
}

const testTpub = "tpubD6NzVbkrYhZ4XgiXtGrdW5XDAPFCL9h7we1vwNCpn8tGbBcgfVYjXyhWo4E1xkh56hjod1RhGjxbaTLV3X4FyWuejifB9jusQ46QzG87VKp"

// onchainBlock is a valid on-chain block as a generic map.
func onchainBlock() map[string]any {
	return map[string]any{
		"rpc_url": "http://127.0.0.1:18443", "rpc_user": "satbooth", "rpc_password_file": "/etc/satbooth/rpc.pass",
		"wallet": "satbooth", "descriptor": "wpkh(" + testTpub + "/0/*)", "network": "regtest",
	}
}

func withOnchain(t testing.TB, mod func(o map[string]any)) []byte {
	t.Helper()
	m := goodConfig()
	o := onchainBlock()
	if mod != nil {
		mod(o)
	}
	m["onchain"] = o
	return enc(t, m)
}

func TestOnchainConfigGood(t *testing.T) {
	c, err := ParseConfig(withOnchain(t, func(o map[string]any) {
		o["confirmations"] = []any{map[string]any{"up_to_sat": 500000, "confirmations": 2}, map[string]any{"up_to_sat": 0, "confirmations": 5}}
		o["gap_alert"] = 50
		o["poll_seconds"] = 4
		o["poll_rate"] = 2.5
		o["timeout_seconds"] = 20
	}))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Onchain
	if o == nil || o.RPCURL != "http://127.0.0.1:18443" || o.RPCUser != "satbooth" || o.RPCPasswordFile != "/etc/satbooth/rpc.pass" ||
		o.Wallet != "satbooth" || o.Network != "regtest" || o.GapAlert != 50 || o.PollSeconds != 4 || o.PollRate != 2.5 || o.TimeoutSeconds != 20 ||
		len(o.Confirmations) != 2 || o.Confirmations[0].UpToSat != 500000 || o.Confirmations[0].Confirmations == nil || *o.Confirmations[0].Confirmations != 2 ||
		o.Confirmations[1].UpToSat != 0 || o.Confirmations[1].Confirmations == nil || *o.Confirmations[1].Confirmations != 5 {
		t.Fatalf("bad block: %+v", o)
	}
	rc, err := o.railConfig()
	if err != nil || rc.PollInterval != 4*time.Second || rc.CallTimeout != 20*time.Second || rc.Descriptor.Func != "wpkh" ||
		!reflect.DeepEqual(rc.Tiers, []onchain.Tier{{UpTo: 500000, Confirmations: 2}, {UpTo: 0, Confirmations: 5}}) {
		t.Fatalf("bad mapping: %+v %v", rc, err)
	}
}

// Ruling 6.1-5: a tier without a confirmations key is refused, so an omitted key never reads as
// 0-conf; an explicit 0 is accepted; an empty list still takes the default tiers.
func TestTierConfirmationsRequired(t *testing.T) {
	const want = "config: onchain.confirmations: every tier needs a confirmations key (0 to 100)"
	for name, tiers := range map[string][]any{
		"one tier":        {map[string]any{"up_to_sat": 0}},
		"the first":       {map[string]any{"up_to_sat": 10000}, map[string]any{"up_to_sat": 0, "confirmations": 1}},
		"the last":        {map[string]any{"up_to_sat": 10000, "confirmations": 0}, map[string]any{"up_to_sat": 0}},
		"a null":          {map[string]any{"up_to_sat": 0, "confirmations": nil}},
		"an empty object": {map[string]any{}},
	} {
		_, err := ParseConfig(withOnchain(t, func(o map[string]any) { o["confirmations"] = tiers }))
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v", name, err)
		}
	}
	c, err := ParseConfig(withOnchain(t, func(o map[string]any) {
		o["confirmations"] = []any{map[string]any{"up_to_sat": 10000, "confirmations": 0}, map[string]any{"up_to_sat": 0, "confirmations": 1}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.Onchain.railConfig()
	if err != nil || !reflect.DeepEqual(rc.Tiers, []onchain.Tier{{UpTo: 10000, Confirmations: 0}, {UpTo: 0, Confirmations: 1}}) {
		t.Fatalf("%+v %v", rc.Tiers, err)
	}
	if onchain.Required(rc.Tiers, 10000) != 0 || onchain.Required(rc.Tiers, 10001) != 1 {
		t.Fatal("Required on the mapped tiers")
	}
	c, err = ParseConfig(withOnchain(t, func(o map[string]any) { o["confirmations"] = []any{} }))
	if err != nil {
		t.Fatal(err)
	}
	if rc, err = c.Onchain.railConfig(); err != nil || !reflect.DeepEqual(rc.WithDefaults().Tiers, onchain.DefaultTiers()) {
		t.Fatalf("an empty list: %+v %v", rc.Tiers, err)
	}
}

// Every edge of every range is accepted (and one past each is refused by TestOnchainConfigRefusals).
func TestOnchainConfigEdgesAccepted(t *testing.T) {
	for name, mod := range map[string]func(o map[string]any){
		"poll 2":      func(o map[string]any) { o["poll_seconds"] = 2 },
		"poll 60":     func(o map[string]any) { o["poll_seconds"] = 60 },
		"timeout 1":   func(o map[string]any) { o["timeout_seconds"] = 1 },
		"timeout 600": func(o map[string]any) { o["timeout_seconds"] = 600 },
		"gap 1":       func(o map[string]any) { o["gap_alert"] = 1 },
		"gap 1000000": func(o map[string]any) { o["gap_alert"] = 1000000 },
		"rate 10000":  func(o map[string]any) { o["poll_rate"] = 10000 },
	} {
		if _, err := ParseConfig(withOnchain(t, mod)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// An operator reads the configuration key, not the Go field, in a refusal.
func TestOnchainErrorsNameConfigKeys(t *testing.T) {
	for key, mod := range map[string]func(o map[string]any){
		"poll_seconds":    func(o map[string]any) { o["poll_seconds"] = 1 },
		"timeout_seconds": func(o map[string]any) { o["timeout_seconds"] = 601 },
		"gap_alert":       func(o map[string]any) { o["gap_alert"] = -1 },
		"poll_rate":       func(o map[string]any) { o["poll_rate"] = -1 },
		"network":         func(o map[string]any) { o["network"] = "testnet4" },
		"up_to_sat":       func(o map[string]any) { o["confirmations"] = []any{map[string]any{"up_to_sat": 5, "confirmations": 1}} },
		"confirmations": func(o map[string]any) {
			o["confirmations"] = []any{map[string]any{"up_to_sat": 0, "confirmations": 101}}
		},
	} {
		_, err := ParseConfig(withOnchain(t, mod))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: %v", key, err)
		}
		for _, goName := range []string{"PollInterval", "CallTimeout", "GapAlert", "PollRate", "Network", "Tiers", "UpTo"} {
			if err != nil && strings.Contains(err.Error(), goName) {
				t.Errorf("%s: error names the Go field %s: %v", key, goName, err)
			}
		}
	}
}

// The three tier texts that name the bound read the operator's key in full (rail's UpTo, onchain's tier, keyNames).
func TestOnchainTierTextsNameUpToSat(t *testing.T) {
	tier := func(up, confs int) map[string]any { return map[string]any{"up_to_sat": up, "confirmations": confs} }
	for want, tiers := range map[string][]any{
		"config: onchain: the last tier's up_to_sat must be 0 (no upper bound)":                    {tier(5, 1)},
		"config: onchain: a tier's up_to_sat must be 1 to the maximum amount, 0 only for the last": {tier(0, 1), tier(200, 2), tier(0, 3)},
		"config: onchain: tiers' up_to_sat must be strictly increasing":                            {tier(100, 1), tier(100, 2), tier(0, 3)},
	} {
		_, err := ParseConfig(withOnchain(t, func(o map[string]any) { o["confirmations"] = tiers }))
		if err == nil || err.Error() != want {
			t.Errorf("got %v, want %q", err, want)
		}
	}
}

// Key matching holds inside the onchain block and inside a tier: a case variant or a duplicate is refused by walk.
func TestOnchainKeyMatching(t *testing.T) {
	base := string(withOnchain(t, func(o map[string]any) {
		o["confirmations"] = []any{map[string]any{"up_to_sat": 0, "confirmations": 1}}
	}))
	for name, tc := range map[string][2]string{
		"case variant in onchain": {`"wallet"`, `"Wallet"`},
		"case variant in a tier":  {`"up_to_sat"`, `"Up_To_Sat"`},
		"duplicate in onchain":    {`"wallet"`, `"wallet":"x","wallet"`},
		"duplicate in a tier":     {`"up_to_sat"`, `"up_to_sat":0,"up_to_sat"`},
	} {
		if !strings.Contains(base, tc[0]) {
			t.Fatalf("%s: key not found", name)
		}
		_, err := ParseConfig([]byte(strings.Replace(base, tc[0], tc[1], 1)))
		if err != errShape {
			t.Errorf("%s: %v, want the shape error from walk", name, err)
		}
	}
}

func TestOnchainConfigDefaults(t *testing.T) {
	c, err := ParseConfig(withOnchain(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.Onchain.railConfig()
	if err != nil {
		t.Fatal(err)
	}
	rc = rc.WithDefaults()
	if !reflect.DeepEqual(rc.Tiers, onchain.DefaultTiers()) || rc.CallTimeout != 30*time.Second || rc.PollInterval != onchain.DefaultPollInterval || rc.GapAlert != onchain.DefaultGapAlert {
		t.Fatalf("defaults not applied: %+v", rc)
	}
	// An empty list is the default too.
	c, err = ParseConfig(withOnchain(t, func(o map[string]any) { o["confirmations"] = []any{} }))
	if err != nil {
		t.Fatal(err)
	}
	if rc, _ = c.Onchain.railConfig(); len(rc.WithDefaults().Tiers) != 3 {
		t.Fatal("an empty tier list did not take the default")
	}
	// No block, no rail; an explicit null too.
	for _, b := range [][]byte{enc(t, goodConfig()), bytes.Replace(enc(t, goodConfig()), []byte(`"invoice_expiry_minutes"`), []byte(`"onchain":null,"invoice_expiry_minutes"`), 1)} {
		if c, err := ParseConfig(b); err != nil || c.Onchain != nil {
			t.Fatalf("absent onchain: %v %v", c, err)
		}
	}
}

func TestOnchainConfigRefusals(t *testing.T) {
	tprv := "wpkh(tprv8ZgxMBicQKsPd7Uf69XL1XwhmjHopUGep8GuEiJDZmbQz6o58LninorQAfcKZWARbtRtfnLcJ5MQ2AtHcQJCCRUcMRvmDUjyEmNUWwx8UbK/0/*)"
	cases := map[string]func(o map[string]any){
		"no rpc_url":        func(o map[string]any) { delete(o, "rpc_url") },
		"rpc_url scheme":    func(o map[string]any) { o["rpc_url"] = "ws://127.0.0.1:1" },
		"rpc_url query":     func(o map[string]any) { o["rpc_url"] = "http://127.0.0.1:1/?a=b" },
		"rpc_url user info": func(o map[string]any) { o["rpc_url"] = "http://u:p@127.0.0.1:1" },
		"no rpc_user":       func(o map[string]any) { delete(o, "rpc_user") },
		"bad rpc_user":      func(o map[string]any) { o["rpc_user"] = "a b" },
		"no password file":  func(o map[string]any) { delete(o, "rpc_password_file") },
		"no wallet":         func(o map[string]any) { delete(o, "wallet") },
		"bad wallet":        func(o map[string]any) { o["wallet"] = "../x" },
		"dot wallet":        func(o map[string]any) { o["wallet"] = ".." },
		"no descriptor":     func(o map[string]any) { delete(o, "descriptor") },
		"tprv descriptor":   func(o map[string]any) { o["descriptor"] = tprv },
		"junk descriptor":   func(o map[string]any) { o["descriptor"] = "pkh(x)" },
		"no network":        func(o map[string]any) { delete(o, "network") },
		"testnet4":          func(o map[string]any) { o["network"] = "testnet4" },
		"mainnet with tpub": func(o map[string]any) { o["network"] = "mainnet" },
		"unknown key":       func(o map[string]any) { o["extra"] = 1 },
		"unknown tier key":  func(o map[string]any) { o["confirmations"] = []any{map[string]any{"up_to": 1}} },
		"tier order": func(o map[string]any) {
			o["confirmations"] = []any{map[string]any{"up_to_sat": 0, "confirmations": 2}, map[string]any{"up_to_sat": 0, "confirmations": 3}}
		},
		"tier not last zero": func(o map[string]any) { o["confirmations"] = []any{map[string]any{"up_to_sat": 5, "confirmations": 2}} },
		"gap negative":       func(o map[string]any) { o["gap_alert"] = -1 },
		"gap huge":           func(o map[string]any) { o["gap_alert"] = 1000001 },
		"poll 1":             func(o map[string]any) { o["poll_seconds"] = 1 },
		"poll 61":            func(o map[string]any) { o["poll_seconds"] = 61 },
		"poll negative":      func(o map[string]any) { o["poll_seconds"] = -2 },
		"poll wraps":         func(o map[string]any) { o["poll_seconds"] = int64(1)<<53 + 4 },
		"rate negative":      func(o map[string]any) { o["poll_rate"] = -1 },
		"rate huge":          func(o map[string]any) { o["poll_rate"] = 10001 },
		"timeout 601":        func(o map[string]any) { o["timeout_seconds"] = 601 },
		"timeout negative":   func(o map[string]any) { o["timeout_seconds"] = -1 },
		"timeout wraps":      func(o map[string]any) { o["timeout_seconds"] = int64(1)<<55 + 30 },
	}
	for name, mod := range cases {
		if name == "mainnet with tpub" {
			continue // handled below: the descriptor's network is part of the rule
		}
		_, err := ParseConfig(withOnchain(t, mod))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.HasPrefix(err.Error(), "config: ") {
			t.Errorf("%s: error without prefix: %v", name, err)
		}
	}
	// The descriptor's own error, not the nwc.network cross-check's (goodConfig's nwc.network is regtest).
	if _, err := ParseConfig(withOnchain(t, cases["mainnet with tpub"])); err == nil ||
		!strings.Contains(err.Error(), "the descriptor's key does not match the network") || strings.Contains(err.Error(), "nwc.network") {
		t.Errorf("a tpub descriptor on mainnet: %v", err)
	}
	// A refused private descriptor: the fixed text, and nothing of the input.
	_, err := ParseConfig(withOnchain(t, cases["tprv descriptor"]))
	if err == nil || strings.Contains(err.Error(), "tprv") || !strings.Contains(err.Error(), "private key material") {
		t.Fatalf("tprv: %v", err)
	}
	for name, mod := range cases {
		if _, err := ParseConfig(withOnchain(t, mod)); err != nil && (strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), testTpub[:20])) {
			t.Errorf("%s: error echoes input: %v", name, err)
		}
	}
}

func TestOnchainCheckFilesPasswordMode(t *testing.T) {
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	pw := filepath.Join(dir, "rpc.pass")
	if err := os.WriteFile(pw, []byte("test-password-1-rpc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := goodConfig()
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	m["webhooks"].([]any)[0].(map[string]any)["secret_file"] = whPath
	o := onchainBlock()
	o["rpc_password_file"] = pw
	m["onchain"] = o
	c, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFiles(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pw, 0o644); err != nil {
		t.Fatal(err)
	}
	err = c.CheckFiles()
	if err == nil || !strings.Contains(err.Error(), "onchain.rpc_password_file") || strings.Contains(err.Error(), "test-password-1") {
		t.Fatalf("0644 password file: %v", err)
	}
	if err := os.Remove(pw); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFiles(); err == nil {
		t.Fatal("missing password file accepted")
	}
}

// The service uses btcrpc's URL rule, and it agrees with plainURL (the rule the other URLs use) at every byte at every
// position of a small corpus.
func TestOnchainURLRuleAgreesWithPlainURL(t *testing.T) {
	bases := []string{"http://127.0.0.1:18443", "https://h.example/x", "http://h", "https://[::1]:8/", "http://a.b/c?d", "ftp://h",
		"http://h:0", "http://h:1", "http://h:65535", "http://h:65536", "http://h:", "http://[::1]:", "http://h:+80", "http://h:080"}
	check := func(s string) {
		_, err := plainURL("rpc_url", s, true, "http", "https")
		if (err == nil) != btcrpc.ValidURL(s) {
			t.Fatalf("rules disagree on %q: plainURL err=%v ValidURL=%v", s, err, btcrpc.ValidURL(s))
		}
	}
	for _, b := range bases {
		check(b)
		for i := 0; i <= len(b); i++ {
			for c := 0; c < 256; c++ {
				check(b[:i] + string([]byte{byte(c)}) + b[i:])
				if i < len(b) {
					check(b[:i] + string([]byte{byte(c)}) + b[i+1:])
				}
			}
		}
	}
	// The length limit: 512 bytes is accepted by both, 513 refused by both.
	for n, want := range map[int]bool{511: true, 512: true, 513: false, 600: false} {
		u := "http://" + strings.Repeat("a", n-len("http://"))
		check(u)
		if btcrpc.ValidURL(u) != want {
			t.Errorf("%d-byte URL: ValidURL %v, want %v", n, !want, want)
		}
	}
}

func TestOnchainConfigMutations(t *testing.T) {
	base := withOnchain(t, func(o map[string]any) {
		o["confirmations"] = []any{map[string]any{"up_to_sat": 5, "confirmations": 1}, map[string]any{"up_to_sat": 0, "confirmations": 2}}
		o["poll_seconds"] = 5
	})
	rng := rand.New(rand.NewSource(20261104))
	odd := []byte{0, '"', '\\', '{', '}', '[', ']', ',', ':', 0x7f, 0xff, ' ', '\n', '%', '#', '?', '@', '/', '-', '0', '9'}
	for i := 0; i < 20000; i++ {
		b := bytes.Clone(base)
		for n := rng.Intn(3) + 1; n > 0 && len(b) > 0; n-- {
			p := rng.Intn(len(b))
			switch rng.Intn(3) {
			case 0:
				b[p] = odd[rng.Intn(len(odd))]
			case 1:
				b = append(b[:p], b[p+1:]...)
			default:
				b = append(b[:p], append([]byte{odd[rng.Intn(len(odd))]}, b[p:]...)...)
			}
		}
		checkParse(t, b)
	}
}

// ratesBlock is the example configuration's rates block, with the tests' custom host.
func ratesBlock() map[string]any {
	return map[string]any{
		"providers":          []any{"kraken", "coinbase", "yadio"},
		"custom_url":         "https://rates.example.invalid/satbooth",
		"currencies":         []any{"USD", "EUR"},
		"margin_percent":     "1",
		"min_sources":        2,
		"max_spread_percent": "2",
		"max_age_seconds":    300,
		"fetch_seconds":      60,
	}
}

// withRates is the good configuration with ratesBlock changed by mod (mod may be nil).
func withRates(t testing.TB, mod func(r map[string]any)) []byte {
	t.Helper()
	m := goodConfig()
	r := ratesBlock()
	if mod != nil {
		mod(r)
	}
	m["rates"] = r
	return enc(t, m)
}

func providerNames(ps []rates.Provider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name()
	}
	return out
}

func TestRatesConfigGood(t *testing.T) {
	c, err := ParseConfig(withRates(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	rc, err := c.Rates.ratesConfig(c.Rates.CustomURL, nil, func() time.Time { return at }, log, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := providerNames(rc.Providers); !reflect.DeepEqual(got, []string{"kraken", "coinbase", "yadio", "custom"}) {
		t.Fatalf("providers %v", got)
	}
	if !reflect.DeepEqual(rc.Assets, []string{rates.AssetBTC}) || !reflect.DeepEqual(rc.Currencies, []string{"USD", "EUR"}) {
		t.Fatalf("assets %v currencies %v", rc.Assets, rc.Currencies)
	}
	if rc.Policy != (rates.Policy{MinSources: 2, MaxSpreadBps: 200, MaxAge: 300 * time.Second}) || rc.Interval != 60*time.Second || rc.MarginBps != 100 {
		t.Fatalf("policy %+v interval %v margin %d", rc.Policy, rc.Interval, rc.MarginBps)
	}
	if rc.Now == nil || !rc.Now().Equal(at) || rc.Log != log {
		t.Fatal("the clock or the logger was not passed through")
	}
	if _, err := rates.New(rc); err != nil {
		t.Fatalf("rates.New refuses the example block: %v", err)
	}
}

func TestRatesConfigDefaults(t *testing.T) {
	m := goodConfig()
	m["rates"] = map[string]any{"providers": []any{"kraken", "coinbase"}, "currencies": []any{"USD"}}
	c, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Rates
	if r.MarginPercent != "0" || r.MinSources != 2 || r.MaxSpreadPercent != "2" || r.MaxAgeSeconds != 300 || r.FetchSeconds != 60 || r.CustomURL != "" {
		t.Fatalf("defaults not applied: %+v", r)
	}
	rc, err := r.ratesConfig(r.CustomURL, nil, time.Now, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := providerNames(rc.Providers); !reflect.DeepEqual(got, []string{"kraken", "coinbase"}) {
		t.Fatalf("providers %v", got)
	}
	if rc.Policy != (rates.Policy{MinSources: 2, MaxSpreadBps: 200, MaxAge: 300 * time.Second}) || rc.Interval != time.Minute || rc.MarginBps != 0 {
		t.Fatalf("policy %+v interval %v margin %d", rc.Policy, rc.Interval, rc.MarginBps)
	}
	// No block, and an explicit null: no fiat pricing.
	for _, b := range [][]byte{enc(t, goodConfig()), bytes.Replace(enc(t, goodConfig()), []byte(`"invoice_expiry_minutes"`), []byte(`"rates":null,"invoice_expiry_minutes"`), 1)} {
		if c, err := ParseConfig(b); err != nil || c.Rates != nil {
			t.Fatalf("absent rates: %v %v", c, err)
		}
	}
}

func TestRatesConfigAccepted(t *testing.T) {
	cases := map[string]func(r map[string]any){
		"min_sources 0 (the default)": func(r map[string]any) { r["min_sources"] = 0 },
		"min_sources absent":          func(r map[string]any) { delete(r, "min_sources") },
		"min_sources 4":               func(r map[string]any) { r["min_sources"] = 4 },
		"min_sources 5, four built-ins and custom_url": func(r map[string]any) {
			r["min_sources"], r["providers"] = 5, []any{"kraken", "coinbase", "yadio", "bitfinex"}
		},
		"fetch_seconds 150 of 300":     func(r map[string]any) { r["fetch_seconds"] = 150 },
		"fetch_seconds 10":             func(r map[string]any) { r["fetch_seconds"] = 10 },
		"fetch_seconds 600 of 1200":    func(r map[string]any) { r["fetch_seconds"], r["max_age_seconds"] = 600, 1200 },
		"fetch_seconds 15 of 31":       func(r map[string]any) { r["fetch_seconds"], r["max_age_seconds"] = 15, 31 },
		"max_age_seconds 30":           func(r map[string]any) { r["max_age_seconds"], r["fetch_seconds"] = 30, 15 },
		"max_age_seconds 3600":         func(r map[string]any) { r["max_age_seconds"] = 3600 },
		"margin -50":                   func(r map[string]any) { r["margin_percent"] = "-50" },
		"margin 50":                    func(r map[string]any) { r["margin_percent"] = "50" },
		"margin 0.01":                  func(r map[string]any) { r["margin_percent"] = "0.01" },
		"margin -0":                    func(r map[string]any) { r["margin_percent"] = "-0" },
		"margin 50.00":                 func(r map[string]any) { r["margin_percent"] = "50.00" },
		"spread 0.01":                  func(r map[string]any) { r["max_spread_percent"] = "0.01" },
		"spread 50":                    func(r map[string]any) { r["max_spread_percent"] = "50" },
		"one built-in and custom_url":  func(r map[string]any) { r["providers"] = []any{"coinbase"} },
		"two built-ins, no custom_url": func(r map[string]any) { r["providers"] = []any{"yadio", "kraken"}; delete(r, "custom_url") },
		"custom_url_file in place of custom_url": func(r map[string]any) {
			delete(r, "custom_url")
			r["custom_url_file"] = "/etc/satbooth/rates.url"
		},
		"custom_url_file, custom_url empty": func(r map[string]any) { r["custom_url"], r["custom_url_file"] = "", "/etc/satbooth/rates.url" },
		"custom_url_file empty is absent":   func(r map[string]any) { r["custom_url_file"] = "" },
		"custom_url 512 bytes":              func(r map[string]any) { r["custom_url"] = urlOfLen(512) },
		"custom_url empty is absent":        func(r map[string]any) { r["custom_url"] = "" },
		"32 currencies":                     func(r map[string]any) { r["providers"] = []any{"coinbase", "yadio"}; r["currencies"] = codes(32) },
		"a non-Kraken code with two others": func(r map[string]any) {
			r["currencies"] = []any{"USD", "ARS"}
			r["providers"] = []any{"kraken", "yadio"}
		},
		"a non-Kraken code with min_sources 3":   func(r map[string]any) { r["currencies"] = []any{"ARS"}; r["min_sources"] = 3 },
		"every Kraken code with Kraken + custom": func(r map[string]any) { r["providers"] = []any{"kraken"}; r["currencies"] = anys(rates.KrakenFiats()) },
	}
	for name, mod := range cases {
		if _, err := ParseConfig(withRates(t, mod)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// codes returns n distinct currency codes (none of them Kraken's): AAA, AAB, ...
func codes(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = string([]byte{'A', 'A' + byte(i/26), 'A' + byte(i%26)})
	}
	return out
}

// urlOfLen is a https URL of the custom host of exactly n bytes.
func urlOfLen(n int) string {
	u := "https://rates.example.invalid/"
	return u + strings.Repeat("a", n-len(u))
}

const (
	msgRatesProviders  = "config: rates.providers must list kraken, coinbase, yadio or bitfinex, each at most once"
	msgRatesCustomURL  = "config: rates.custom_url must be a https URL with a host, without user info, query or fragment, spaces or control characters, at most 512 bytes"
	msgRatesCurrencies = "config: rates.currencies must be 1 to 32 distinct codes of 3 uppercase letters, not BTC or XMR"
	msgRatesMargin     = "config: rates.margin_percent must be -50 to 50 with at most 2 decimals"
	msgRatesMinSources = "config: rates.min_sources must be 2 to 5"
	msgRatesSpread     = "config: rates.max_spread_percent must be 0.01 to 50 with at most 2 decimals"
	msgRatesMaxAge     = "config: rates.max_age_seconds must be 30 to 3600"
	msgRatesFetch      = "config: rates.fetch_seconds must be 10 to 600 and at most half of max_age_seconds"
	msgRatesCount      = "config: rates needs at least min_sources providers (providers plus custom_url or custom_url_file)"
	msgRatesQuery      = "config: rates.custom_url must not have a query: a URL that carries a token goes in rates.custom_url_file, a file with mode 0600"
	msgRatesBoth       = "config: rates.custom_url and rates.custom_url_file exclude each other: set one"
	msgRatesReach      = "config: rates.currencies has a code that fewer than min_sources providers quote for BTC (kraken quotes only AUD, CAD, CHF, EUR, GBP, JPY and USD)"
	msgRatesReachBfx   = "config: rates.currencies has a code that fewer than min_sources providers quote for BTC (bitfinex quotes only EUR, GBP and USD)"
	msgRatesReachBoth  = "config: rates.currencies has a code that fewer than min_sources providers quote for BTC (kraken quotes only AUD, CAD, CHF, EUR, GBP, JPY and USD; bitfinex quotes only EUR, GBP and USD)"
	msgNotAnObject     = "config: not a valid configuration object"
)

func TestRatesConfigRefusals(t *testing.T) {
	big := int64(1)<<53 + 2
	cases := []struct {
		name string
		mod  func(r map[string]any)
		want string
	}{
		{"providers duplicate", func(r map[string]any) { r["providers"] = []any{"kraken", "kraken"} }, msgRatesProviders},
		{"providers unknown", func(r map[string]any) { r["providers"] = []any{"bitstamp"} }, msgRatesProviders},
		{"providers case variant", func(r map[string]any) { r["providers"] = []any{"Kraken", "coinbase"} }, msgRatesProviders},
		{"providers custom by name", func(r map[string]any) { r["providers"] = []any{"custom", "coinbase"} }, msgRatesProviders},
		{"providers empty name", func(r map[string]any) { r["providers"] = []any{"", "coinbase"} }, msgRatesProviders},
		{"custom_url http", func(r map[string]any) { r["custom_url"] = "http://rates.example.invalid/satbooth" }, msgRatesCustomURL},
		{"custom_url user info", func(r map[string]any) { r["custom_url"] = "https://u:p@rates.example.invalid/satbooth" }, msgRatesCustomURL},
		{"custom_url 513 bytes", func(r map[string]any) { r["custom_url"] = urlOfLen(513) }, msgRatesCustomURL},
		{"custom_url fragment", func(r map[string]any) { r["custom_url"] = "https://rates.example.invalid/satbooth#x" }, msgRatesCustomURL},
		{"custom_url no host", func(r map[string]any) { r["custom_url"] = "https:///satbooth" }, msgRatesCustomURL},
		{"custom_url space", func(r map[string]any) { r["custom_url"] = "https://rates.example.invalid/a b" }, msgRatesCustomURL},
		{"custom_url relative", func(r map[string]any) { r["custom_url"] = "/satbooth" }, msgRatesCustomURL},
		{"custom_url wrong scheme and a query", func(r map[string]any) { r["custom_url"] = "http://rates.example.invalid/satbooth?k=v" }, msgRatesCustomURL},
		{"custom_url user info and a query", func(r map[string]any) { r["custom_url"] = "https://u:p@rates.example.invalid/satbooth?k=v" }, msgRatesCustomURL},
		{"custom_url port 0", func(r map[string]any) { r["custom_url"] = "https://rates.example.invalid:0/satbooth" }, msgRatesCustomURL},
		{"custom_url port 65536", func(r map[string]any) { r["custom_url"] = "https://rates.example.invalid:65536/satbooth" }, msgRatesCustomURL},
		{"custom_url port 99999 and a query", func(r map[string]any) { r["custom_url"] = "https://rates.example.invalid:99999/s?k=v" }, msgRatesCustomURL},
		{"custom_url empty port", func(r map[string]any) { r["custom_url"] = "https://rates.example.invalid:/satbooth" }, msgRatesCustomURL},
		{"custom_url with a query", func(r map[string]any) { r["custom_url"] = "https://rates.example.invalid/satbooth?token=abc" }, msgRatesQuery},
		{"custom_url with an asset query", func(r map[string]any) { r["custom_url"] = "https://rates.example.invalid/satbooth?asset=BTC" }, msgRatesQuery},
		{"custom_url with an empty query", func(r map[string]any) { r["custom_url"] = "https://rates.example.invalid/satbooth?" }, msgRatesQuery},
		{"custom_url and custom_url_file", func(r map[string]any) { r["custom_url_file"] = "/etc/satbooth/rates.url" }, msgRatesBoth},
		{"custom_url_file alone", func(r map[string]any) {
			delete(r, "custom_url")
			r["providers"], r["custom_url_file"] = []any{}, "/etc/satbooth/rates.url"
		}, msgRatesCount},
		{"currencies empty", func(r map[string]any) { r["currencies"] = []any{} }, msgRatesCurrencies},
		{"currencies absent", func(r map[string]any) { delete(r, "currencies") }, msgRatesCurrencies},
		{"currencies 33", func(r map[string]any) { r["providers"] = []any{"coinbase", "yadio"}; r["currencies"] = codes(33) }, msgRatesCurrencies},
		{"currencies lowercase", func(r map[string]any) { r["currencies"] = []any{"usd"} }, msgRatesCurrencies},
		{"currencies BTC", func(r map[string]any) { r["currencies"] = []any{"BTC"} }, msgRatesCurrencies},
		{"currencies duplicate", func(r map[string]any) { r["currencies"] = []any{"USD", "USD"} }, msgRatesCurrencies},
		{"currencies 4 letters", func(r map[string]any) { r["currencies"] = []any{"USDT"} }, msgRatesCurrencies},
		{"currencies empty code", func(r map[string]any) { r["currencies"] = []any{""} }, msgRatesCurrencies},
		{"margin -50.01", func(r map[string]any) { r["margin_percent"] = "-50.01" }, msgRatesMargin},
		{"margin 50.01", func(r map[string]any) { r["margin_percent"] = "50.01" }, msgRatesMargin},
		{"margin 1.555", func(r map[string]any) { r["margin_percent"] = "1.555" }, msgRatesMargin},
		{"margin abc", func(r map[string]any) { r["margin_percent"] = "abc" }, msgRatesMargin},
		{"margin +1", func(r map[string]any) { r["margin_percent"] = "+1" }, msgRatesMargin},
		{"margin 1.", func(r map[string]any) { r["margin_percent"] = "1." }, msgRatesMargin},
		{"margin 1e1", func(r map[string]any) { r["margin_percent"] = "1e1" }, msgRatesMargin},
		{"margin a JSON number", func(r map[string]any) { r["margin_percent"] = 1.5 }, msgNotAnObject},
		{"min_sources 1", func(r map[string]any) { r["min_sources"] = 1 }, msgRatesMinSources},
		{"min_sources 6", func(r map[string]any) { r["min_sources"] = 6 }, msgRatesMinSources},
		{"min_sources -1", func(r map[string]any) { r["min_sources"] = -1 }, msgRatesMinSources},
		{"min_sources 2^53+2", func(r map[string]any) { r["min_sources"] = big }, msgRatesMinSources},
		{"spread 0", func(r map[string]any) { r["max_spread_percent"] = "0" }, msgRatesSpread},
		{"spread 50.01", func(r map[string]any) { r["max_spread_percent"] = "50.01" }, msgRatesSpread},
		{"spread -1", func(r map[string]any) { r["max_spread_percent"] = "-1" }, msgRatesSpread},
		{"spread 0.001", func(r map[string]any) { r["max_spread_percent"] = "0.001" }, msgRatesSpread},
		{"spread a JSON number", func(r map[string]any) { r["max_spread_percent"] = 2 }, msgNotAnObject},
		{"max_age 29", func(r map[string]any) { r["max_age_seconds"], r["fetch_seconds"] = 29, 10 }, msgRatesMaxAge},
		{"max_age 3601", func(r map[string]any) { r["max_age_seconds"] = 3601 }, msgRatesMaxAge},
		{"max_age -300", func(r map[string]any) { r["max_age_seconds"] = -300 }, msgRatesMaxAge},
		{"max_age 2^53+2", func(r map[string]any) { r["max_age_seconds"] = big }, msgRatesMaxAge},
		{"fetch 9", func(r map[string]any) { r["fetch_seconds"] = 9 }, msgRatesFetch},
		{"fetch 601", func(r map[string]any) { r["fetch_seconds"], r["max_age_seconds"] = 601, 3600 }, msgRatesFetch},
		{"fetch 151 of 300", func(r map[string]any) { r["fetch_seconds"] = 151 }, msgRatesFetch},
		{"fetch 16 of 31", func(r map[string]any) { r["fetch_seconds"], r["max_age_seconds"] = 16, 31 }, msgRatesFetch},
		{"fetch default 60 of 30", func(r map[string]any) { delete(r, "fetch_seconds"); r["max_age_seconds"] = 30 }, msgRatesFetch},
		{"fetch -60", func(r map[string]any) { r["fetch_seconds"] = -60 }, msgRatesFetch},
		{"fetch 2^53+2", func(r map[string]any) { r["fetch_seconds"] = big }, msgRatesFetch},
		{"kraken alone", func(r map[string]any) { r["providers"] = []any{"kraken"}; delete(r, "custom_url") }, msgRatesCount},
		{"custom_url alone", func(r map[string]any) { r["providers"] = []any{} }, msgRatesCount},
		{"custom_url alone, providers absent", func(r map[string]any) { delete(r, "providers") }, msgRatesCount},
		{"nothing", func(r map[string]any) { delete(r, "providers"); delete(r, "custom_url") }, msgRatesCount},
		{"three for min_sources 4", func(r map[string]any) { delete(r, "custom_url"); r["min_sources"] = 4 }, msgRatesCount},
		{"a non-Kraken code, Kraken + custom", func(r map[string]any) { r["providers"] = []any{"kraken"}; r["currencies"] = []any{"USD", "ARS"} }, msgRatesReach},
		{"a non-Kraken code, Kraken + Coinbase", func(r map[string]any) {
			r["providers"] = []any{"kraken", "coinbase"}
			r["currencies"] = []any{"NGN"}
			delete(r, "custom_url")
		}, msgRatesReach},
		{"a non-Kraken code, min_sources 4", func(r map[string]any) { r["min_sources"] = 4; r["currencies"] = []any{"EUR", "ARS"} }, msgRatesReach},
	}
	for _, tc := range cases {
		_, err := ParseConfig(withRates(t, tc.mod))
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	// Each refusal text names its key.
	for _, msg := range []string{msgRatesProviders, msgRatesCustomURL, msgRatesCurrencies, msgRatesMargin, msgRatesMinSources,
		msgRatesSpread, msgRatesMaxAge, msgRatesFetch, msgRatesReach, msgRatesReachBfx, msgRatesReachBoth, msgRatesQuery, msgRatesBoth} {
		if !strings.Contains(msg, "rates.") {
			t.Errorf("%q names no key", msg)
		}
	}
}

// Key matching holds inside the rates block: a case variant, a key with a trailing space or a duplicate is refused by
// walk, as is a value of the wrong JSON kind.
func TestRatesKeyMatching(t *testing.T) {
	base := string(withRates(t, nil))
	for name, tc := range map[string][2]string{
		"case variant":       {`"providers"`, `"Providers"`},
		"trailing space":     {`"providers"`, `"providers "`},
		"duplicate":          {`"providers"`, `"providers":[],"providers"`},
		"duplicate scalar":   {`"min_sources"`, `"min_sources":2,"min_sources"`},
		"block as an array":  {`"rates":{`, `"rates":[{`},
		"providers a object": {`"providers":[`, `"providers":{"x":[`},
	} {
		if !strings.Contains(base, tc[0]) {
			t.Fatalf("%s: key not found", name)
		}
		_, err := ParseConfig([]byte(strings.Replace(base, tc[0], tc[1], 1)))
		if err != errShape {
			t.Errorf("%s: %v, want the shape error from walk", name, err)
		}
	}
}

func TestRatesConfigMutations(t *testing.T) {
	base := withRates(t, func(r map[string]any) { r["custom_url"] = "https://rates.example.invalid/s" })
	rng := rand.New(rand.NewSource(20261106))
	odd := []byte{0, '"', '\\', '{', '}', '[', ']', ',', ':', 0x7f, 0xff, ' ', '\n', '%', '#', '?', '@', '/', '-', '.', '0', '1', '5', '9', 'A', 'a'}
	for i := 0; i < 20000; i++ {
		b := bytes.Clone(base)
		for n := rng.Intn(3) + 1; n > 0 && len(b) > 0; n-- {
			p := rng.Intn(len(b))
			switch rng.Intn(3) {
			case 0:
				b[p] = odd[rng.Intn(len(odd))]
			case 1:
				b = append(b[:p], b[p+1:]...)
			default:
				b = append(b[:p], append([]byte{odd[rng.Intn(len(odd))]}, b[p:]...)...)
			}
		}
		checkParse(t, b)
	}
}

// Every provider set the configuration accepts yields quotes the store records: the providers' names meet the store's
// source rule and their count stays within store.MaxQuoteSources (one source per provider: the rates service keeps
// one entry per name, and Aggregate refuses two from one). The quote is built as the API builds it, so a priced order
// on a valid configuration never meets the store's refusal (a 500).
func TestEveryAcceptedProviderSetFitsTheStore(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	builtins := []string{"kraken", "coinbase", "yadio", "bitfinex"}
	accepted := 0
	for mask := 0; mask < 32; mask++ {
		for minSources := 2; minSources <= 5; minSources++ {
			var ps []any
			for i, p := range builtins {
				if mask&(1<<i) != 0 {
					ps = append(ps, p)
				}
			}
			c, err := ParseConfig(withRates(t, func(r map[string]any) {
				r["providers"], r["min_sources"], r["currencies"] = ps, minSources, []any{"USD"}
				if mask&16 == 0 {
					delete(r, "custom_url")
				}
			}))
			if err != nil {
				continue
			}
			accepted++
			rc, err := c.Rates.ratesConfig(c.Rates.CustomURL, nil, func() time.Time { return now }, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(rc.Providers) > store.MaxQuoteSources {
				t.Fatalf("%d providers, the store records at most %d sources", len(rc.Providers), store.MaxQuoteSources)
			}
			var rs []rates.Rate
			for _, p := range rc.Providers {
				v, _ := rates.ParseDecimal("76000.123456789012345678")
				rs = append(rs, rates.Rate{Provider: p.Name(), Currency: "USD", Value: v, At: now})
			}
			q, err := rates.Aggregate(rates.AssetBTC, "USD", rs, now, rc.Policy)
			if err != nil {
				t.Fatalf("mask %d: %v", mask, err)
			}
			r := store.AssetRate{Asset: rates.AssetBTC, Rate: q.Rate.String()}
			for _, s := range q.Sources {
				r.Sources = append(r.Sources, store.QuoteSource{Provider: s.Provider, Rate: s.Value.String(), At: s.At})
			}
			sq := store.Quote{Price: "12.50", Currency: "USD", MarginBps: rc.MarginBps, QuotedAt: q.At, Rates: []store.AssetRate{r}}
			ref := "set-" + string("0123456789abcdefghijklmnopqrstuvwxyz"[mask]) + string(rune('0'+minSources))
			if _, _, err := st.CreateQuotedOrder(context.Background(), store.Order{AccountID: "acct", MerchantRef: ref, AmountMsat: 1000,
				ExpiresAt: now.Add(time.Hour)}, sq); err != nil {
				t.Fatalf("mask %d, min_sources %d: the store refuses the quote: %v", mask, minSources, err)
			}
		}
	}
	// Of the 5 sources: 26 sets of at least 2, 16 of at least 3, 6 of at least 4 and 1 of 5.
	if accepted != 49 {
		t.Fatalf("accepted %d configurations, want 49", accepted)
	}
}

// ratesConfig refuses a custom URL that disagrees with the block, with the fixed text that names no value: a URL beside
// a custom_url that is another one or none, one for a block without a custom API, and none for a custom_url_file.
func TestRatesConfigCustomMismatch(t *testing.T) {
	const want = "config: the custom rate URL does not match the rates block"
	block := func(url, file string) *RatesConfig {
		return &RatesConfig{Providers: []string{"kraken", "coinbase"}, CustomURL: url, CustomURLFile: file, Currencies: []string{"USD"},
			MarginPercent: "0", MinSources: 2, MaxSpreadPercent: "2", MaxAgeSeconds: 300, FetchSeconds: 60}
	}
	const set, other, tokened = "https://rates.example.invalid/v1", "https://rates.example.invalid/v2", "https://rates.example.invalid/v1?token=x"
	for _, c := range []struct {
		name   string
		r      *RatesConfig
		custom string
	}{
		{"custom_url set, another URL", block(set, ""), other},
		{"custom_url set, the URL with a query", block(set, ""), tokened},
		{"custom_url set, none", block(set, ""), ""},
		{"custom_url_file set, none", block("", "custom-url"), ""},
		{"no custom API, a URL", block("", ""), set},
	} {
		rc, err := c.r.ratesConfig(c.custom, nil, time.Now, nil, false)
		if err == nil || err.Error() != want || rc.Providers != nil {
			t.Errorf("%s: %v (%d providers), want %q", c.name, err, len(rc.Providers), want)
		}
		if err != nil && (strings.Contains(err.Error(), "example") || strings.Contains(err.Error(), "token")) {
			t.Errorf("%s: the error carries a value: %v", c.name, err)
		}
	}
	for _, c := range []struct {
		name   string
		r      *RatesConfig
		custom string
	}{
		{"custom_url, the same", block(set, ""), set},
		{"custom_url_file, its URL", block("", "custom-url"), tokened},
		{"no custom API, none", block("", ""), ""},
	} {
		if _, err := c.r.ratesConfig(c.custom, nil, time.Now, nil, false); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// --- the monero block ---

// testXMRAddress is monerotest.New(1).Primary(): a mainnet-shaped primary address that no wallet owns (the example's).
const testXMRAddress = "4kcfbiPsJFptZtA4KDH26YKnA9APQpr28aLUWg4PJ5Q9a7KN35LsHRRQ7MVDS1gXyYNVo8PqLy639jeTZQN6urMNCVRUWMc"

// moneroBlock is a valid monero block with only the required keys, as a generic map.
func moneroBlock() map[string]any {
	return map[string]any{
		"wallet_rpc_url": "http://127.0.0.1:18083", "rpc_user": "satbooth", "rpc_password_file": "/etc/satbooth/wallet-rpc.pass",
		"address": testXMRAddress, "network": "mainnet",
	}
}

func withMonero(t testing.TB, mod func(x map[string]any)) []byte {
	t.Helper()
	m := goodConfig()
	x := moneroBlock()
	if mod != nil {
		mod(x)
	}
	m["monero"] = x
	return enc(t, m)
}

// The example's block parses and maps to the expected monero.Config: poll_rate 5 is PollEvery 200 ms.
func TestMoneroConfigGood(t *testing.T) {
	b, err := os.ReadFile("../cmd/satbooth/testdata/config-example.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	x := c.Monero
	if x == nil || x.WalletRPCURL != "http://127.0.0.1:18083" || x.RPCUser != "satbooth" || x.RPCPasswordFile != "/etc/satbooth/wallet-rpc.pass" ||
		x.Address != testXMRAddress || x.Network != "mainnet" || x.AccountIndex != 0 || x.UnusedAlert != 100 || x.PollSeconds != 10 ||
		x.PollRate != 5 || x.TimeoutSeconds != 30 || len(x.Confirmations) != 3 {
		t.Fatalf("bad block: %+v", x)
	}
	mc, err := x.railConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := monero.Config{Address: testXMRAddress, Network: "mainnet", AccountIndex: 0,
		Tiers:       []rail.Tier{{UpTo: 1_000_000_000_000, Confirmations: 1}, {UpTo: 10_000_000_000_000, Confirmations: 3}, {UpTo: 0, Confirmations: 10}},
		UnusedAlert: 100, PollInterval: 10 * time.Second, PollEvery: 200 * time.Millisecond, CallTimeout: 30 * time.Second}
	if !reflect.DeepEqual(mc, want) {
		t.Fatalf("mapping:\n got %+v\nwant %+v", mc, want)
	}
	// Other rates map exactly to their pause.
	for rate, every := range map[float64]time.Duration{1: time.Second, 2.5: 400 * time.Millisecond, 3: 333333333, 10000: 100 * time.Microsecond} {
		c, err := ParseConfig(withMonero(t, func(x map[string]any) { x["poll_rate"] = rate }))
		if err != nil {
			t.Fatalf("poll_rate %v: %v", rate, err)
		}
		if mc, err := c.Monero.railConfig(); err != nil || mc.PollEvery != every {
			t.Fatalf("poll_rate %v: PollEvery %v (%v), want %v", rate, mc.PollEvery, err, every)
		}
	}
	// account_index is passed through.
	c, err = ParseConfig(withMonero(t, func(x map[string]any) { x["account_index"] = 4294967295 }))
	if err != nil {
		t.Fatal(err)
	}
	if mc, err := c.Monero.railConfig(); err != nil || mc.AccountIndex != 4294967295 {
		t.Fatalf("account_index: %+v %v", mc, err)
	}
}

func TestMoneroConfigDefaults(t *testing.T) {
	c, err := ParseConfig(withMonero(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	mc, err := c.Monero.railConfig()
	if err != nil {
		t.Fatal(err)
	}
	if mc.Tiers != nil || mc.UnusedAlert != 0 || mc.PollInterval != 0 || mc.PollEvery != 0 || mc.CallTimeout != 0 {
		t.Fatalf("an absent key set a value: %+v", mc)
	}
	d := mc.WithDefaults()
	if !reflect.DeepEqual(d.Tiers, monero.DefaultTiers()) || d.UnusedAlert != 100 || d.PollInterval != 10*time.Second ||
		d.PollEvery != 200*time.Millisecond || d.CallTimeout != 30*time.Second || d.AccountIndex != 0 {
		t.Fatalf("defaults: %+v", d)
	}
	// 0 is absent, for every numeric key, and an empty tier list is the default too.
	c, err = ParseConfig(withMonero(t, func(x map[string]any) {
		x["unused_alert"], x["poll_seconds"], x["poll_rate"], x["timeout_seconds"], x["account_index"] = 0, 0, 0, 0, 0
		x["confirmations"] = []any{}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if mc2, err := c.Monero.railConfig(); err != nil || !reflect.DeepEqual(mc2, mc) {
		t.Fatalf("zeros: %+v %v", mc2, err)
	}
	// No block, no rail; an explicit null too.
	for _, b := range [][]byte{enc(t, goodConfig()), bytes.Replace(enc(t, goodConfig()), []byte(`"invoice_expiry_minutes"`), []byte(`"monero":null,"invoice_expiry_minutes"`), 1)} {
		if c, err := ParseConfig(b); err != nil || c.Monero != nil {
			t.Fatalf("absent monero: %v %v", c, err)
		}
	}
}

const (
	msgXMRURL      = "config: monero.wallet_rpc_url must be an http or https URL with a host, without user info, query or fragment, spaces or control characters, at most 512 bytes"
	msgXMRUser     = "config: monero.rpc_user must be 1 to 64 characters of A-Z a-z 0-9 . _ -"
	msgXMRPassword = "config: monero.rpc_password_file is missing"
	msgXMRTierKey  = "config: monero.confirmations: every tier needs a confirmations key (0 to 100)"
	msgXMRPoll     = "config: monero.poll_seconds must be 2 to 60"
	msgXMRTimeout  = "config: monero.timeout_seconds must be 1 to 600"
	msgXMRRate     = "config: monero.poll_rate must be 1 to 10000"
	msgXMRAddress  = "config: monero: address must be the wallet's primary address for network"
	msgXMRNetwork  = "config: monero: network must be mainnet, testnet, stagenet or regtest"
	msgXMRConfs    = "config: monero: a tier's confirmations must be 0 to 100"
	msgXMRUnused   = "config: monero: unused_alert must be 1 to 10000"
	msgXMRPollEdge = "config: monero: poll_seconds must be 2 s to 60 s"
	msgXMRLastTier = "config: monero: the last tier's up_to_piconero must be 0 (no upper bound)"
	msgXMRUpTo     = "config: monero: a tier's up_to_piconero must be 1 to the maximum amount, 0 only for the last"
	msgXMRIncrease = "config: monero: tiers' up_to_piconero must be strictly increasing"
	msgXMRDecrease = "config: monero: tiers' confirmations must not decrease"
)

func TestMoneroConfigRefusals(t *testing.T) {
	tier := func(up int64, confs any) map[string]any {
		m := map[string]any{"up_to_piconero": up}
		if confs != nil {
			m["confirmations"] = confs
		}
		return m
	}
	cases := []struct {
		name string
		mod  func(x map[string]any)
		want string
	}{
		{"a 9 address on mainnet", func(x map[string]any) { x["address"] = "9" + testXMRAddress[1:] }, msgXMRAddress},
		{"a 94-byte address", func(x map[string]any) { x["address"] = testXMRAddress[:94] }, msgXMRAddress},
		{"a 96-byte address", func(x map[string]any) { x["address"] = testXMRAddress + "1" }, msgXMRAddress},
		{"a subaddress", func(x map[string]any) { x["address"] = "8" + testXMRAddress[1:] }, msgXMRAddress},
		{"a 0 in the address", func(x map[string]any) { x["address"] = testXMRAddress[:50] + "0" + testXMRAddress[51:] }, msgXMRAddress},
		{"no address", func(x map[string]any) { delete(x, "address") }, msgXMRAddress},
		{"a mainnet address on stagenet", func(x map[string]any) { x["network"] = "stagenet" }, msgXMRAddress},
		{"network testnet4", func(x map[string]any) { x["network"] = "testnet4" }, msgXMRNetwork},
		{"network signet", func(x map[string]any) { x["network"] = "signet" }, msgXMRNetwork},
		{"network Mainnet", func(x map[string]any) { x["network"] = "Mainnet" }, msgXMRNetwork},
		{"no network", func(x map[string]any) { delete(x, "network") }, msgXMRNetwork},
		{"a tier without confirmations", func(x map[string]any) { x["confirmations"] = []any{tier(0, nil)} }, msgXMRTierKey},
		{"a tier with confirmations null", func(x map[string]any) {
			x["confirmations"] = []any{tier(1_000_000_000_000, 1), map[string]any{"up_to_piconero": 0, "confirmations": nil}}
		}, msgXMRTierKey},
		{"a tier's confirmations 101", func(x map[string]any) { x["confirmations"] = []any{tier(0, 101)} }, msgXMRConfs},
		{"a tier's confirmations -1", func(x map[string]any) { x["confirmations"] = []any{tier(0, -1)} }, msgXMRConfs},
		{"the last tier bounded", func(x map[string]any) { x["confirmations"] = []any{tier(5, 1)} }, msgXMRLastTier},
		{"a tier above the maximum", func(x map[string]any) {
			x["confirmations"] = []any{tier(store.MaxPiconero+1, 1), tier(0, 2)}
		}, msgXMRUpTo},
		{"tiers not increasing", func(x map[string]any) { x["confirmations"] = []any{tier(100, 1), tier(100, 2), tier(0, 3)} }, msgXMRIncrease},
		{"confirmations decreasing", func(x map[string]any) { x["confirmations"] = []any{tier(100, 3), tier(0, 2)} }, msgXMRDecrease},
		{"unused_alert -1", func(x map[string]any) { x["unused_alert"] = -1 }, msgXMRUnused},
		{"unused_alert 10001", func(x map[string]any) { x["unused_alert"] = 10001 }, msgXMRUnused},
		{"poll_seconds 1", func(x map[string]any) { x["poll_seconds"] = 1 }, msgXMRPollEdge},
		{"poll_seconds 61", func(x map[string]any) { x["poll_seconds"] = 61 }, msgXMRPoll},
		{"poll_seconds -2", func(x map[string]any) { x["poll_seconds"] = -2 }, msgXMRPoll},
		{"poll_seconds wraps to 4", func(x map[string]any) { x["poll_seconds"] = int64(1)<<53 + 4 }, msgXMRPoll},
		{"poll_rate -1", func(x map[string]any) { x["poll_rate"] = -1 }, msgXMRRate},
		{"poll_rate 10001", func(x map[string]any) { x["poll_rate"] = 10001 }, msgXMRRate},
		{"poll_rate 10000.5", func(x map[string]any) { x["poll_rate"] = 10000.5 }, msgXMRRate},
		{"poll_rate 1e300", func(x map[string]any) { x["poll_rate"] = 1e300 }, msgXMRRate},
		{"poll_rate 0.999", func(x map[string]any) { x["poll_rate"] = 0.999 }, msgXMRRate},
		{"poll_rate 0.5", func(x map[string]any) { x["poll_rate"] = 0.5 }, msgXMRRate},
		{"poll_rate 5e-324", func(x map[string]any) { x["poll_rate"] = 5e-324 }, msgXMRRate},
		{"timeout_seconds 601", func(x map[string]any) { x["timeout_seconds"] = 601 }, msgXMRTimeout},
		{"timeout_seconds -1", func(x map[string]any) { x["timeout_seconds"] = -1 }, msgXMRTimeout},
		{"timeout_seconds wraps to 30", func(x map[string]any) { x["timeout_seconds"] = int64(1)<<55 + 30 }, msgXMRTimeout},
		{"wallet_rpc_url with a query", func(x map[string]any) { x["wallet_rpc_url"] = "http://127.0.0.1:18083/?a=b" }, msgXMRURL},
		{"wallet_rpc_url with user info", func(x map[string]any) { x["wallet_rpc_url"] = "http://u:p@127.0.0.1:18083" }, msgXMRURL},
		{"wallet_rpc_url ws", func(x map[string]any) { x["wallet_rpc_url"] = "ws://127.0.0.1:18083" }, msgXMRURL},
		{"wallet_rpc_url with a fragment", func(x map[string]any) { x["wallet_rpc_url"] = "http://127.0.0.1:18083/#x" }, msgXMRURL},
		{"wallet_rpc_url 513 bytes", func(x map[string]any) { x["wallet_rpc_url"] = "http://" + strings.Repeat("a", 506) }, msgXMRURL},
		{"no wallet_rpc_url", func(x map[string]any) { delete(x, "wallet_rpc_url") }, msgXMRURL},
		{"no rpc_user", func(x map[string]any) { delete(x, "rpc_user") }, msgXMRUser},
		{"rpc_user with a space", func(x map[string]any) { x["rpc_user"] = "a b" }, msgXMRUser},
		{"rpc_user 65 bytes", func(x map[string]any) { x["rpc_user"] = strings.Repeat("a", 65) }, msgXMRUser},
		{"no rpc_password_file", func(x map[string]any) { delete(x, "rpc_password_file") }, msgXMRPassword},
		{"account_index -1", func(x map[string]any) { x["account_index"] = -1 }, msgNotAnObject},
		{"account_index 2^32", func(x map[string]any) { x["account_index"] = int64(1) << 32 }, msgNotAnObject},
		{"poll_rate a string", func(x map[string]any) { x["poll_rate"] = "5" }, msgNotAnObject},
		{"an unknown key", func(x map[string]any) { x["spend_key"] = "x" }, errShape.Error()},
		{"an unknown tier key", func(x map[string]any) { x["confirmations"] = []any{map[string]any{"up_to_sat": 0, "confirmations": 1}} }, errShape.Error()},
	}
	for _, tc := range cases {
		_, err := ParseConfig(withMonero(t, tc.mod))
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	// No text names a Go field.
	for _, tc := range cases {
		if _, err := ParseConfig(withMonero(t, tc.mod)); err != nil {
			for _, goName := range []string{"Address", "Network", "UnusedAlert", "PollInterval", "PollEvery", "CallTimeout", "Tiers", "Confirmations", "UpTo"} {
				if strings.Contains(err.Error(), goName) {
					t.Errorf("%s: names the Go field %s: %v", tc.name, goName, err)
				}
			}
		}
	}
}

// Every edge of every range is accepted.
func TestMoneroConfigEdgesAccepted(t *testing.T) {
	for name, mod := range map[string]func(x map[string]any){
		"poll 2":       func(x map[string]any) { x["poll_seconds"] = 2 },
		"poll 60":      func(x map[string]any) { x["poll_seconds"] = 60 },
		"timeout 1":    func(x map[string]any) { x["timeout_seconds"] = 1 },
		"timeout 600":  func(x map[string]any) { x["timeout_seconds"] = 600 },
		"unused 1":     func(x map[string]any) { x["unused_alert"] = 1 },
		"unused 10000": func(x map[string]any) { x["unused_alert"] = 10000 },
		"rate 1":       func(x map[string]any) { x["poll_rate"] = 1 },
		"rate 10000":   func(x map[string]any) { x["poll_rate"] = 10000 },
		"0-conf tier": func(x map[string]any) {
			x["confirmations"] = []any{map[string]any{"up_to_piconero": 0, "confirmations": 0}}
		},
		"100-conf tier": func(x map[string]any) {
			x["confirmations"] = []any{map[string]any{"up_to_piconero": 0, "confirmations": 100}}
		},
		"https":      func(x map[string]any) { x["wallet_rpc_url"] = "https://[::1]:18083/" },
		"regtest":    func(x map[string]any) { x["network"] = "regtest" },
		"testnet 9":  func(x map[string]any) { x["network"], x["address"] = "testnet", "9"+testXMRAddress[1:] },
		"testnet A":  func(x map[string]any) { x["network"], x["address"] = "testnet", "A"+testXMRAddress[1:] },
		"stagenet 5": func(x map[string]any) { x["network"], x["address"] = "stagenet", "5"+testXMRAddress[1:] },
		"the max tier": func(x map[string]any) {
			x["confirmations"] = []any{map[string]any{"up_to_piconero": store.MaxPiconero, "confirmations": 1}, map[string]any{"up_to_piconero": 0, "confirmations": 2}}
		},
		"user 64 bytes":   func(x map[string]any) { x["rpc_user"] = strings.Repeat("a", 64) },
		"url 512 bytes":   func(x map[string]any) { x["wallet_rpc_url"] = "http://" + strings.Repeat("a", 505) },
		"account 1":       func(x map[string]any) { x["account_index"] = 1 },
		"no tiers at all": func(x map[string]any) { delete(x, "confirmations") },
	} {
		if _, err := ParseConfig(withMonero(t, mod)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Key matching holds inside the monero block and inside a tier: a case variant or a duplicate is refused by walk.
func TestMoneroKeyMatching(t *testing.T) {
	base := string(withMonero(t, func(x map[string]any) {
		x["confirmations"] = []any{map[string]any{"up_to_piconero": 0, "confirmations": 1}}
	}))
	for name, tc := range map[string][2]string{
		"case variant in monero": {`"rpc_user"`, `"RPC_User"`},
		"case variant of block":  {`"monero"`, `"Monero"`},
		"case variant in a tier": {`"up_to_piconero"`, `"Up_To_Piconero"`},
		"trailing space":         {`"address"`, `"address "`},
		"duplicate in monero":    {`"address"`, `"address":"x","address"`},
		"duplicate in a tier":    {`"up_to_piconero"`, `"up_to_piconero":0,"up_to_piconero"`},
		"block as an array":      {`"monero":{`, `"monero":[{`},
	} {
		if !strings.Contains(base, tc[0]) {
			t.Fatalf("%s: key not found", name)
		}
		_, err := ParseConfig([]byte(strings.Replace(base, tc[0], tc[1], 1)))
		if err != errShape {
			t.Errorf("%s: %v, want the shape error from walk", name, err)
		}
	}
}

func TestMoneroConfigMutations(t *testing.T) {
	base := withMonero(t, func(x map[string]any) {
		x["confirmations"] = []any{map[string]any{"up_to_piconero": 5, "confirmations": 1}, map[string]any{"up_to_piconero": 0, "confirmations": 2}}
		x["poll_seconds"], x["poll_rate"], x["unused_alert"] = 5, 2.5, 7
	})
	rng := rand.New(rand.NewSource(20261108))
	odd := []byte{0, '"', '\\', '{', '}', '[', ']', ',', ':', 0x7f, 0xff, ' ', '\n', '%', '#', '?', '@', '/', '-', '.', 'e', '0', '1', '4', '5', '8', '9', 'A', 'l'}
	for i := 0; i < 20000; i++ {
		b := bytes.Clone(base)
		for n := rng.Intn(3) + 1; n > 0 && len(b) > 0; n-- {
			p := rng.Intn(len(b))
			switch rng.Intn(3) {
			case 0:
				b[p] = odd[rng.Intn(len(odd))]
			case 1:
				b = append(b[:p], b[p+1:]...)
			default:
				b = append(b[:p], append([]byte{odd[rng.Intn(len(odd))]}, b[p:]...)...)
			}
		}
		checkParse(t, b)
	}
}

// CheckFiles loads the Monero password when the block is set: a 0644 file, or none, is refused with the key's name and
// nothing of the password.
func TestMoneroCheckFilesPasswordMode(t *testing.T) {
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	pw := filepath.Join(dir, "wallet-rpc.pass")
	if err := os.WriteFile(pw, []byte("test-password-1-xmr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := goodConfig()
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	m["webhooks"].([]any)[0].(map[string]any)["secret_file"] = whPath
	x := moneroBlock()
	x["rpc_password_file"] = pw
	m["monero"] = x
	c, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFiles(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pw, 0o644); err != nil {
		t.Fatal(err)
	}
	err = c.CheckFiles()
	if err == nil || !strings.HasPrefix(err.Error(), "config: monero.rpc_password_file: ") || strings.Contains(err.Error(), "test-password-1") || strings.Contains(err.Error(), dir) {
		t.Fatalf("0644 password file: %v", err)
	}
	if err := os.Remove(pw); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFiles(); err == nil || !strings.HasPrefix(err.Error(), "config: monero.rpc_password_file: ") {
		t.Fatalf("missing password file: %v", err)
	}
	// Without the block the file is not looked at.
	c.Monero = nil
	if err := c.CheckFiles(); err != nil {
		t.Fatal(err)
	}
}

// ratesConfig asks the providers for XMR exactly when the Monero rail is configured.
func TestRatesAssetsFollowMonero(t *testing.T) {
	c, err := ParseConfig(withRates(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	for withXMR, want := range map[bool][]string{false: {rates.AssetBTC}, true: {rates.AssetBTC, rates.AssetXMR}} {
		rc, err := c.Rates.ratesConfig(c.Rates.CustomURL, nil, time.Now, nil, withXMR)
		if err != nil || !reflect.DeepEqual(rc.Assets, want) {
			t.Fatalf("withXMR %v: assets %v (%v), want %v", withXMR, rc.Assets, err, want)
		}
	}
}

// Bitfinex is a configurable provider: one of the four built-ins, so min_sources reaches 5 with the custom API.
func TestRatesBitfinexConfig(t *testing.T) {
	c, err := ParseConfig(withRates(t, func(r map[string]any) { r["providers"] = []any{"kraken", "bitfinex"}; delete(r, "custom_url") }))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.Rates.ratesConfig("", nil, time.Now, nil, false)
	if err != nil || !reflect.DeepEqual(providerNames(rc.Providers), []string{"kraken", "bitfinex"}) {
		t.Fatalf("providers %v (%v)", providerNames(rc.Providers), err)
	}
	all := []any{"kraken", "coinbase", "yadio", "bitfinex"}
	if _, err := ParseConfig(withRates(t, func(r map[string]any) { r["providers"], r["min_sources"] = all, 5 })); err != nil {
		t.Fatalf("min_sources 5 with four built-ins and custom_url: %v", err)
	}
	for name, mod := range map[string]func(r map[string]any){
		"bitfinex twice": func(r map[string]any) { r["providers"] = []any{"bitfinex", "bitfinex"} },
		"Bitfinex":       func(r map[string]any) { r["providers"] = []any{"Bitfinex", "kraken"} },
	} {
		if _, err := ParseConfig(withRates(t, mod)); err == nil || err.Error() != msgRatesProviders {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, mod := range map[string]func(r map[string]any){
		"min_sources 6":                    func(r map[string]any) { r["providers"], r["min_sources"] = all, 6 },
		"min_sources 5 without custom_url": func(r map[string]any) { r["providers"], r["min_sources"] = all, 5; delete(r, "custom_url") },
	} {
		want := msgRatesMinSources
		if name != "min_sources 6" {
			want = msgRatesCount
		}
		if _, err := ParseConfig(withRates(t, mod)); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
}

// The startup reach rule counts the providers that can quote each code for BTC (rates.Quotes, Bitfinex's list
// included); XMR has no startup rule.
func TestRatesReachConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		providers  []any
		custom     bool
		currencies []any
		want       string // "" accepted
	}{
		"EUR, kraken and bitfinex":           {[]any{"kraken", "bitfinex"}, false, []any{"EUR"}, ""},
		"USD EUR GBP, kraken and bitfinex":   {[]any{"kraken", "bitfinex"}, false, []any{"USD", "EUR", "GBP"}, ""},
		"JPY, kraken and bitfinex":           {[]any{"kraken", "bitfinex"}, false, []any{"JPY"}, msgRatesReachBoth},
		"COP, kraken and bitfinex":           {[]any{"kraken", "bitfinex"}, false, []any{"COP"}, msgRatesReachBoth},
		"COP, bitfinex and kraken":           {[]any{"bitfinex", "kraken"}, false, []any{"COP"}, msgRatesReachBoth},
		"COP, kraken, bitfinex and custom":   {[]any{"kraken", "bitfinex"}, true, []any{"COP"}, msgRatesReachBoth},
		"ARS, coinbase and bitfinex":         {[]any{"coinbase", "bitfinex"}, false, []any{"ARS"}, msgRatesReachBfx},
		"ARS, coinbase, bitfinex and custom": {[]any{"coinbase", "bitfinex"}, true, []any{"ARS"}, ""},
		"ARS, coinbase and yadio":            {[]any{"coinbase", "yadio"}, false, []any{"ARS"}, ""},
		"GBP, bitfinex and custom":           {[]any{"bitfinex"}, true, []any{"GBP"}, ""},
		"CHF, bitfinex and custom":           {[]any{"bitfinex"}, true, []any{"CHF"}, msgRatesReachBfx},
		"COP, kraken and custom":             {[]any{"kraken"}, true, []any{"COP"}, msgRatesReach},
		"COP, kraken and coinbase":           {[]any{"kraken", "coinbase"}, false, []any{"COP"}, msgRatesReach},
		"COP, kraken, yadio and custom":      {[]any{"kraken", "yadio"}, true, []any{"USD", "COP"}, ""},
	} {
		_, err := ParseConfig(withRates(t, func(r map[string]any) {
			r["providers"], r["currencies"] = tc.providers, tc.currencies
			if !tc.custom {
				delete(r, "custom_url")
			}
		}))
		if tc.want == "" && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if tc.want != "" && (err == nil || err.Error() != tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		if err != nil {
			for _, c := range tc.currencies {
				listed := slices.Contains(rates.KrakenFiats(), c.(string)) || slices.Contains(rates.BitfinexFiats(), c.(string))
				if !listed && strings.Contains(err.Error(), c.(string)) {
					t.Errorf("%s: the text echoes the code %s", name, c)
				}
			}
		}
	}
}

// nwc.network is required, one of the four, and the onchain and payout blocks name the same network (plan 12).
func TestNWCNetwork(t *testing.T) {
	for _, n := range []string{"mainnet", "testnet", "signet", "regtest"} {
		c, err := ParseConfig(with(t, "nwc.network", n))
		if err != nil || c.NWC.Network != n {
			t.Errorf("network %s: %v", n, err)
		}
		want, _ := approval.ParseNetwork(n)
		if got, err := c.lightningNetwork(); err != nil || got != want {
			t.Errorf("network %s: mapping %v %v", n, got, err)
		}
	}
	const want = "config: nwc.network is required and must be mainnet, testnet, signet or regtest"
	for name, body := range map[string][]byte{
		"missing":      with(t, "nwc.network", nil),
		"empty":        with(t, "nwc.network", ""),
		"unknown":      with(t, "nwc.network", "testnet4"),
		"capitalised":  with(t, "nwc.network", "Mainnet"),
		"stagenet":     with(t, "nwc.network", "stagenet"),
		"with a space": with(t, "nwc.network", "regtest "),
	} {
		_, err := ParseConfig(body)
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestNWCNetworkCrossChecks(t *testing.T) {
	for _, n := range []string{"mainnet", "testnet", "signet", "regtest"} {
		for _, other := range []string{"mainnet", "testnet", "signet", "regtest"} {
			m := goodConfig()
			m["nwc"].(map[string]any)["network"] = n
			m["payout"] = payoutBlock()
			m["payout"].(map[string]any)["network"] = other
			_, err := ParseConfig(enc(t, m))
			if n == other && err != nil || n != other && (err == nil || err.Error() != "config: payout.network must equal nwc.network") {
				t.Errorf("payout %s with nwc %s: %v", other, n, err)
			}
		}
	}
	m := goodConfig()
	o := onchainBlock()
	o["network"] = "signet"
	m["onchain"] = o
	m["nwc"].(map[string]any)["network"] = "regtest"
	if _, err := ParseConfig(enc(t, m)); err == nil || !strings.Contains(err.Error(), "onchain.network") ||
		!strings.Contains(err.Error(), "nwc.network") {
		t.Errorf("onchain differs: %v", err)
	}
	o["network"] = "regtest"
	if _, err := ParseConfig(enc(t, m)); err != nil {
		t.Errorf("onchain equal: %v", err)
	}
	// A configuration built by struct literal gets the same check in Run.
	cfg := &Config{NWC: NWCConfig{Network: "regtest"}, Payout: &PayoutConfig{Network: "testnet"}}
	if _, err := cfg.lightningNetwork(); err == nil || !strings.Contains(err.Error(), "payout.network") {
		t.Errorf("literal: %v", err)
	}
}

// A number, an object, an array or a bool for nwc.network is refused. The parser's messages for a wrong type are the same
// generic ones for every key (they name none, and carry no value), so the test pins those two, and that the refusal never
// echoes the value.
func TestNWCNetworkWrongType(t *testing.T) {
	for name, v := range map[string]any{"number": 1, "object": map[string]any{"a": "secretish"}, "array": []any{"regtest"}, "bool": true} {
		_, err := ParseConfig(with(t, "nwc.network", v))
		if err == nil || (err.Error() != "config: not a valid configuration object" && err != errShape) ||
			strings.Contains(err.Error(), "secretish") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Run itself, not only the helper it calls, refuses a struct-literal configuration (which never went through
// ParseConfig) whose networks differ or whose nwc.network is missing, before any file is opened: the data directory is
// not even created, and the key file named is never looked for.
func TestRunRefusesDifferentNetworks(t *testing.T) {
	for name, cfg := range map[string]*Config{
		"payout differs":  {NWC: NWCConfig{Network: "regtest"}, Payout: &PayoutConfig{Network: "testnet"}},
		"onchain differs": {NWC: NWCConfig{Network: "mainnet"}, Onchain: &OnchainConfig{Network: "signet"}},
		"nwc missing":     {},
		"nwc unknown":     {NWC: NWCConfig{Network: "testnet4"}},
	} {
		dir := filepath.Join(t.TempDir(), "data")
		cfg.DataDir = dir
		cfg.NWC.ClientKeyFile = filepath.Join(t.TempDir(), "missing.key")
		err := Run(context.Background(), cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		if err == nil || err.Error() != "service: nwc.network is missing or another block names another network" {
			t.Errorf("%s: %v", name, err)
		}
		if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
			t.Errorf("%s: the data directory was touched: %v", name, serr)
		}
	}
}

// A port that is present is 1 to 65535 in decimal digits, without a sign, a leading zero or a space; a colon with
// nothing after it is refused. The same table is in setup's tests for checkURL, and btcrpc.ValidURL agrees.
func TestURLPorts(t *testing.T) {
	for u, want := range map[string]bool{
		"https://h/": true, "https://h:1/": true, "https://h:80/": true, "https://h:443": true, "https://h:65535/": true,
		"https://h:0/": false, "https://h:65536/": false, "https://h:99999/": false, "https://h:/": false, "https://h:": false,
		"https://h:+80/": false, "https://h:-1/": false, "https://h:080/": false, "https://h:00/": false, "https://h:8a/": false,
		"https://h:99999999999999999999/": false, "https://h:0x50/": false, "https://h:8 0/": false,
		"https://[::1]/": true, "https://[::1]:8080/": true, "https://[::1]:65535/": true, "https://[::1]:0/": false,
		"https://[::1]:65536/": false, "https://[::1]:/": false, "https://[::1]:+1/": false,
		"https://[fe80::1%25en0]:80/": true, "https://[fe80::1%25en0]:0/": false,
	} {
		for _, noQuery := range []bool{true, false} {
			_, err := plainURL("f", u, noQuery, "https")
			if (err == nil) != want {
				t.Errorf("%q (noQuery %v): err %v, want accepted %v", u, noQuery, err, want)
			}
		}
		if strings.HasPrefix(u, "https") && btcrpc.ValidURL(u) != want {
			t.Errorf("btcrpc.ValidURL(%q) = %v, want %v", u, !want, want)
		}
	}
}
