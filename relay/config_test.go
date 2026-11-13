package relay

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/nostr"
)

// testKey derives a valid x-only public key from the fixed secret key n (1..).
func testKey(t testing.TB, n int) string {
	t.Helper()
	k, err := nostr.ParseSecretKeyHex(fmt.Sprintf("%064x", n))
	if err != nil {
		t.Fatal(err)
	}
	return k.PublicKeyHex()
}

// validMap returns a valid config as a generic map with the given number of pairs.
func validMap(t testing.TB, pairs int) map[string]any {
	ps := make([]any, 0, pairs)
	for i := 0; i < pairs; i++ {
		ps = append(ps, map[string]any{"wallet": testKey(t, 2*i+1), "client": testKey(t, 2*i+2)})
	}
	return map[string]any{
		"hub_listen":       "127.0.0.1:7447",
		"client_listen":    "192.0.2.10:7448",
		"client_relay_url": "ws://192.0.2.10:7448",
		"pairs":            ps,
	}
}

func marshal(t testing.TB, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseConfigExample(t *testing.T) {
	c, err := LoadConfig(filepath.Join("testdata", "config-example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.HubListen != "127.0.0.1:7447" || c.ClientListen != "192.0.2.10:7448" || c.ClientRelayURL != "ws://192.0.2.10:7448" {
		t.Fatalf("bad listeners: %+v", c)
	}
	if len(c.Pairs) != 2 {
		t.Fatalf("pairs = %d", len(c.Pairs))
	}
	if c.WindowSeconds != 3600 || c.WindowMaxEvents != 10000 || c.WindowMaxBytes != 67108864 {
		t.Fatalf("defaults: %+v", c)
	}
	p, ok := c.PairByWallet(c.Pairs[1].Wallet)
	if !ok || p != c.Pairs[1] {
		t.Fatalf("PairByWallet: %v %v", p, ok)
	}
	p, ok = c.PairByClient(c.Pairs[0].Client)
	if !ok || p != c.Pairs[0] {
		t.Fatalf("PairByClient: %v %v", p, ok)
	}
	if _, ok := c.PairByWallet(c.Pairs[0].Client); ok {
		t.Fatal("client key found as wallet")
	}
	if _, ok := c.PairByClient(c.Pairs[0].Wallet); ok {
		t.Fatal("wallet key found as client")
	}
	if _, ok := c.PairByWallet("nope"); ok {
		t.Fatal("unknown key found")
	}
}

func TestLoadConfigErrors(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file accepted")
	}
	big := filepath.Join(t.TempDir(), "big.json")
	if err := os.WriteFile(big, []byte(strings.Repeat(" ", maxConfigBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(big); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("oversized file: %v", err)
	}
}

// breakers maps a name to a mutation of a valid config and the field its error must name.
type breaker struct {
	name  string
	field string
	mut   func(t testing.TB, m map[string]any)
}

func pairs(m map[string]any) []any { return m["pairs"].([]any) }
func pair(m map[string]any, i int) map[string]any {
	return pairs(m)[i].(map[string]any)
}

func breakers() []breaker {
	set := func(k string, v any) func(testing.TB, map[string]any) {
		return func(_ testing.TB, m map[string]any) { m[k] = v }
	}
	del := func(k string) func(testing.TB, map[string]any) {
		return func(_ testing.TB, m map[string]any) { delete(m, k) }
	}
	return []breaker{
		{"missing hub_listen", "hub_listen", del("hub_listen")},
		{"missing client_listen", "client_listen", del("client_listen")},
		{"hub not host:port", "hub_listen", set("hub_listen", "127.0.0.1")},
		{"hub port 0", "hub_listen", set("hub_listen", "127.0.0.1:0")},
		{"hub port 65536", "hub_listen", set("hub_listen", "127.0.0.1:65536")},
		{"hub port non-canonical", "hub_listen", set("hub_listen", "127.0.0.1:07447")},
		{"hub port signed", "hub_listen", set("hub_listen", "127.0.0.1:+7447")},
		{"hub all interfaces", "hub_listen", set("hub_listen", "0.0.0.0:7447")},
		{"hub empty host", "hub_listen", set("hub_listen", ":7447")},
		{"hub localhost name", "hub_listen", set("hub_listen", "localhost:7447")},
		{"hub other 127 address", "hub_listen", set("hub_listen", "127.0.0.2:7447")},
		{"hub public", "hub_listen", set("hub_listen", "192.0.2.10:7447")},
		{"hub ipv6 any", "hub_listen", set("hub_listen", "[::]:7447")},
		{"hub ipv6 long loopback", "hub_listen", set("hub_listen", "[0:0:0:0:0:0:0:1]:7447")},
		{"client bad port", "client_listen", set("client_listen", "192.0.2.10:x")},
		{"client no port", "client_listen", set("client_listen", "192.0.2.10")},
		{"client empty host", "client_listen", set("client_listen", ":7448")},
		{"client 0.0.0.0", "client_listen", set("client_listen", "0.0.0.0:7448")},
		{"client bare zero", "client_listen", set("client_listen", "0:7448")},
		{"client dotted zero", "client_listen", set("client_listen", "0.0:7448")},
		{"client localhost", "client_listen", set("client_listen", "localhost:7448")},
		{"client hostname", "client_listen", set("client_listen", "example.com:7448")},
		{"client ipv6 any", "client_listen", set("client_listen", "[::]:7448")},
		{"client ipv6 any long", "client_listen", set("client_listen", "[0:0:0:0:0:0:0:0]:7448")},
		{"client same as hub spelled differently", "client_listen", func(_ testing.TB, m map[string]any) {
			m["hub_listen"] = "127.0.0.1:7447"
			m["client_listen"] = "[::ffff:127.0.0.1]:7447"
		}},
		{"client same as hub ipv6 spelled differently", "client_listen", func(_ testing.TB, m map[string]any) {
			m["hub_listen"] = "[::1]:7447"
			m["client_listen"] = "[0:0:0:0:0:0:0:1]:7447"
		}},
		{"client same as hub", "client_listen", func(_ testing.TB, m map[string]any) { m["client_listen"] = m["hub_listen"] }},
		{"url empty", "client_relay_url", set("client_relay_url", "")},
		{"url http", "client_relay_url", set("client_relay_url", "http://192.0.2.10:7448")},
		{"url no host", "client_relay_url", set("client_relay_url", "ws://")},
		{"url space", "client_relay_url", set("client_relay_url", "ws://192.0.2.10 :7448")},
		{"url quote", "client_relay_url", set("client_relay_url", "ws://192.0.2.10\"")},
		{"url backslash", "client_relay_url", set("client_relay_url", "ws://192.0.2.10\\")},
		{"url DEL", "client_relay_url", set("client_relay_url", "ws://192.0.2.10\x7f")},
		{"url non-ASCII", "client_relay_url", set("client_relay_url", "ws://192.0.2.10/é")},
		{"url 513 bytes", "client_relay_url", set("client_relay_url", "ws://"+strings.Repeat("a", 508))},
		{"url uppercase scheme", "client_relay_url", set("client_relay_url", "WS://192.0.2.10:7448")},
		{"zero pairs", "pairs", set("pairs", []any{})},
		{"null pairs", "pairs", set("pairs", nil)},
		{"65 pairs", "pairs", func(t testing.TB, m map[string]any) { m["pairs"] = validMap(t, 65)["pairs"] }},
		{"wallet bad", "pairs[1].wallet", func(_ testing.TB, m map[string]any) { pair(m, 1)["wallet"] = strings.Repeat("0", 64) }},
		{"wallet uppercase", "pairs[0].wallet", func(_ testing.TB, m map[string]any) {
			pair(m, 0)["wallet"] = strings.ToUpper(pair(m, 0)["wallet"].(string))
		}},
		{"wallet short", "pairs[0].wallet", func(_ testing.TB, m map[string]any) { pair(m, 0)["wallet"] = "ab" }},
		{"wallet missing", "pairs[0].wallet", func(_ testing.TB, m map[string]any) { delete(pair(m, 0), "wallet") }},
		{"client bad", "pairs[1].client", func(_ testing.TB, m map[string]any) { pair(m, 1)["client"] = "zz" }},
		{"client missing", "pairs[0].client", func(_ testing.TB, m map[string]any) { delete(pair(m, 0), "client") }},
		{"wallet twice", "pairs[1].wallet", func(_ testing.TB, m map[string]any) { pair(m, 1)["wallet"] = pair(m, 0)["wallet"] }},
		{"client twice", "pairs[1].client", func(_ testing.TB, m map[string]any) { pair(m, 1)["client"] = pair(m, 0)["client"] }},
		{"wallet is also a client", "pairs[0].wallet", func(_ testing.TB, m map[string]any) { pair(m, 1)["client"] = pair(m, 0)["wallet"] }},
		{"client is also a wallet", "pairs[0].client", func(_ testing.TB, m map[string]any) { pair(m, 1)["wallet"] = pair(m, 0)["client"] }},
		{"same key both roles in one pair", "pairs[0]", func(_ testing.TB, m map[string]any) { pair(m, 0)["client"] = pair(m, 0)["wallet"] }},
		{"window_seconds 59", "window_seconds", set("window_seconds", 59)},
		{"window_seconds 21601", "window_seconds", set("window_seconds", 21601)},
		{"window_seconds negative", "window_seconds", set("window_seconds", -1)},
		{"window_max_events 99", "window_max_events", set("window_max_events", 99)},
		{"window_max_events 1000001", "window_max_events", set("window_max_events", 1000001)},
		{"window_max_bytes below", "window_max_bytes", set("window_max_bytes", 1048575)},
		{"window_max_bytes above", "window_max_bytes", set("window_max_bytes", 1073741825)},
		{"unknown key", "extra", set("extra", 1)},
	}
}

func TestParseConfigRejects(t *testing.T) {
	for _, b := range breakers() {
		t.Run(b.name, func(t *testing.T) {
			m := validMap(t, 2)
			b.mut(t, m)
			_, err := ParseConfig(marshal(t, m))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), b.field) {
				t.Fatalf("error %q does not name %q", err, b.field)
			}
		})
	}
	good := string(marshal(t, validMap(t, 1)))
	raw := map[string]string{
		"trailing object": good + good,
		"trailing junk":   good + " x",
		"empty":           "",
		"not an object":   "[]",
		"null":            "null",
		"truncated":       good[:len(good)-1],
	}
	for name, in := range raw {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(in)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// Trailing whitespace is fine.
	if _, err := ParseConfig([]byte(good + " \n")); err != nil {
		t.Fatalf("trailing newline refused: %v", err)
	}
}

func TestParseConfigBoundariesAccepted(t *testing.T) {
	cases := map[string]map[string]any{
		"seconds low":          {"window_seconds": 60},
		"seconds high":         {"window_seconds": 21600},
		"events low":           {"window_max_events": 100},
		"events high":          {"window_max_events": 1000000},
		"bytes low":            {"window_max_bytes": 1048576},
		"bytes high":           {"window_max_bytes": 1073741824},
		"ipv6 loopback":        {"hub_listen": "[::1]:7447"},
		"port 65535":           {"client_listen": "192.0.2.10:65535"},
		"client ipv6 specific": {"client_listen": "[::1]:7448"},
		"wss":                  {"client_relay_url": "wss://relay.example.test"},
		"url 512 bytes":        {"client_relay_url": "ws://" + strings.Repeat("a", 507)},
	}
	for name, over := range cases {
		t.Run(name, func(t *testing.T) {
			m := validMap(t, 1)
			for k, v := range over {
				m[k] = v
			}
			if _, err := ParseConfig(marshal(t, m)); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := ParseConfig(marshal(t, validMap(t, 64))); err != nil {
		t.Fatalf("64 pairs: %v", err)
	}
}

func TestParseConfigProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	bs := breakers()
	for i := 0; i < 300; i++ {
		n := 2 + rng.Intn(20)
		m := validMap(t, n)
		if rng.Intn(2) == 0 {
			m["window_seconds"] = 60 + rng.Intn(21541)
			m["window_max_events"] = 100 + rng.Intn(999901)
			m["window_max_bytes"] = int64(1048576) + rng.Int63n(1<<30-1048576+1)
		}
		if rng.Intn(2) == 0 {
			m["hub_listen"] = fmt.Sprintf("127.0.0.1:%d", 1+rng.Intn(65535))
			m["client_listen"] = fmt.Sprintf("192.0.2.%d:%d", 1+rng.Intn(254), 1+rng.Intn(65535))
		}
		if _, err := ParseConfig(marshal(t, m)); err != nil {
			t.Fatalf("valid config refused: %v", err)
		}
		// Break one field at a time. The pair mutations address pairs 0 and 1, which always exist.
		b := bs[rng.Intn(len(bs))]
		if strings.HasPrefix(b.name, "65 pairs") {
			m["pairs"] = validMap(t, 65)["pairs"]
		} else {
			b.mut(t, m)
		}
		_, err := ParseConfig(marshal(t, m))
		if err == nil {
			t.Fatalf("%s: accepted", b.name)
		}
		if !strings.Contains(err.Error(), b.field) {
			t.Fatalf("%s: error %q does not name %q", b.name, err, b.field)
		}
	}
}

func FuzzParseConfig(f *testing.F) {
	f.Add(marshal(f, validMap(f, 2)))
	f.Add(marshal(f, validMap(f, 1)))
	f.Add([]byte(`{"hub_listen":"[::1]:1","client_listen":"[::1]:2","client_relay_url":"wss://x","pairs":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"window_seconds":60}x`))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := ParseConfig(b)
		if err != nil {
			return
		}
		if err := c.validate(); err != nil {
			t.Fatalf("accepted a config that does not re-validate: %v", err)
		}
		out, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		c2, err := ParseConfig(out)
		if err != nil {
			t.Fatalf("re-encoded config refused: %v", err)
		}
		if c2.HubListen != c.HubListen || len(c2.Pairs) != len(c.Pairs) || c2.WindowMaxBytes != c.WindowMaxBytes {
			t.Fatal("round trip changed the config")
		}
	})
}
