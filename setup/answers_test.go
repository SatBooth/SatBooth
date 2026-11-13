package setup

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"github.com/satbooth/satbooth/relay"
	"math/rand"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/service"
)

// Test values: none is a credential. The descriptors' keys are BIP32 test vector 1's public keys.
const (
	xpubV1 = "xpub661MyMwAqRbcFtXgS5sYJABqqG9YLmC4Q1Rdap9gSE8NqtwybGhePY2gZ29ESFjqJoCu1Rupje8YtGqsefD265TMg7usUDFdp6W1EGMcet8"
	tpubV1 = "tpubD6NzVbkrYhZ4XgiXtGrdW5XDAPFCL9h7we1vwNCpn8tGbBcgfVYjXyhWo4E1xkh56hjod1RhGjxbaTLV3X4FyWuejifB9jusQ46QzG87VKp"
	// The documentation example address of cmd/satbooth/testdata/config-example.json (95 characters, leading 4).
	xmrMain = "4kcfbiPsJFptZtA4KDH26YKnA9APQpr28aLUWg4PJ5Q9a7KN35LsHRRQ7MVDS1gXyYNVo8PqLy639jeTZQN6urMNCVRUWMc"
)

// Curve points: the x-coordinates of 1G to 6G, valid public keys for the relay and the NWC settings.
var curveKeys = []string{
	"79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
	"c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5",
	"f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9",
	"e493dbf1c10d80f3581e4904930b1404cc6c13900ee0758474fa94abe8c4cd13",
	"2f8bde4d1a07209355b4a7250a5c5128e88b84bddc619ab7cba8d569b240efe4",
	"fff97bd5755eeea420453a14355235d382f6472f8568a18b2f057a1460297556",
}

// edKeys returns n distinct Ed25519 public keys from fixed seeds.
func edKeys(n int) []string {
	out := make([]string, n)
	for i := range out {
		seed := make([]byte, 32)
		seed[0], seed[1] = byte(i+1), 0x5a
		out[i] = hex.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	}
	return out
}

func baseAnswers() PublicAnswers {
	return PublicAnswers{Network: "mainnet", HostDir: "/srv/satbooth", HubPrivateAddr: "192.0.2.20",
		PublicPrivateAddr: "192.0.2.10", PublicAddr: "127.0.0.1", PublicPort: 8080,
		CheckoutBaseURL: "https://pay.example.com", WebhookURL: "https://merchant.example.com/hooks/satbooth"}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// withRefunds turns refunds on: k approval keys and every cap, as the operator gives them.
func withRefunds(a *PublicAnswers, k int, budget uint64) {
	a.ApprovalKeys, a.PayoutBudgetSat = edKeys(k), budget
	a.PayoutMaxSat, a.PayoutDailySat, a.PayoutAlertSat = min(budget, 10000), min(budget, 50000), 0
	if budget > 20000 {
		a.PayoutAlertSat = 5000
	}
}

func fxHubToPublic(payout bool) *HubToPublic {
	h := &HubToPublic{Version: 1, InvoiceWallet: curveKeys[2]}
	if payout {
		h.PayoutWallet = curveKeys[3]
	}
	return h
}

func TestPublicAnswersRefusals(t *testing.T) {
	good := baseAnswers()
	if _, err := ParsePublicAnswers(mustJSON(good)); err != nil {
		t.Fatalf("the base answers: %v", err)
	}
	set := func(f func(a *PublicAnswers)) []byte { a := baseAnswers(); f(&a); return mustJSON(a) }
	// setRaw writes every field, the refund keys included (MarshalJSON leaves them out without approval_keys).
	setRaw := func(f func(a *PublicAnswers)) []byte {
		type plain PublicAnswers
		a := baseAnswers()
		f(&a)
		return mustJSON(plain(a))
	}
	cases := []struct {
		name  string
		input []byte
		field string
	}{
		{"unknown key", []byte(`{"network":"mainnet","bogus":1}`), "unknown"},
		{"duplicate key", []byte(`{"network":"mainnet","network":"mainnet"}`), "network"},
		{"16385 bytes", padded(mustJSON(good), 16385), "16 KiB"},
		{"not an object", []byte(`[]`), "JSON object"},
		{"null network", []byte(`{"network":null}`), "network"},
		{"trailing data", append(mustJSON(good), []byte(` {}`)...), "JSON object"},
		{"wrong type", []byte(`{"public_port":"8080"}`), "public_port"},
		{"testnet", set(func(a *PublicAnswers) { a.Network = "testnet" }), "network"},
		{"no network", set(func(a *PublicAnswers) { a.Network = "" }), "network"},
		{"relative dir", set(func(a *PublicAnswers) { a.HostDir = "srv/satbooth" }), "host_dir"},
		{"dir with space", set(func(a *PublicAnswers) { a.HostDir = "/srv/sat booth" }), "host_dir"},
		{"dir with dollar", set(func(a *PublicAnswers) { a.HostDir = "/srv/$HOME" }), "host_dir"},
		{"dir with quote", set(func(a *PublicAnswers) { a.HostDir = `/srv/"x` }), "host_dir"},
		{"dir with newline", set(func(a *PublicAnswers) { a.HostDir = "/srv/x\nHUB_IP=1" }), "host_dir"},
		{"dir with dotdot", set(func(a *PublicAnswers) { a.HostDir = "/srv/../etc" }), "host_dir"},
		{"root dir", set(func(a *PublicAnswers) { a.HostDir = "/" }), "host_dir"},
		{"same addresses", set(func(a *PublicAnswers) { a.PublicPrivateAddr = a.HubPrivateAddr }), "public_private_addr"},
		{"same addresses by value", set(func(a *PublicAnswers) { a.HubPrivateAddr, a.PublicPrivateAddr = "192.0.2.5", "192.0.2.5" }), "public_private_addr"},
		{"hub loopback on mainnet", set(func(a *PublicAnswers) { a.HubPrivateAddr = "127.0.0.1" }), "hub_private_addr"},
		{"public loopback on signet", set(func(a *PublicAnswers) { a.Network, a.PublicPrivateAddr = "signet", "127.0.0.1" }), "public_private_addr"},
		{"multicast", set(func(a *PublicAnswers) { a.HubPrivateAddr = "224.0.0.1" }), "hub_private_addr"},
		{"ipv6 multicast", set(func(a *PublicAnswers) { a.HubPrivateAddr = "ff02::1" }), "hub_private_addr"},
		{"link-local", set(func(a *PublicAnswers) { a.PublicPrivateAddr = "169.254.1.1" }), "public_private_addr"},
		{"ipv6 link-local", set(func(a *PublicAnswers) { a.PublicPrivateAddr = "fe80::1" }), "public_private_addr"},
		{"broadcast", set(func(a *PublicAnswers) { a.HubPrivateAddr = "255.255.255.255" }), "hub_private_addr"},
		{"ipv4-mapped", set(func(a *PublicAnswers) { a.HubPrivateAddr = "::ffff:192.0.2.1" }), "hub_private_addr"},
		{"hostname", set(func(a *PublicAnswers) { a.HubPrivateAddr = "hub.example.com" }), "hub_private_addr"},
		{"zone", set(func(a *PublicAnswers) { a.HubPrivateAddr = "fe80::1%eth0" }), "hub_private_addr"},
		{"unspecified", set(func(a *PublicAnswers) { a.PublicPrivateAddr = "0.0.0.0" }), "public_private_addr"},
		{"public addr hostname", set(func(a *PublicAnswers) { a.PublicAddr = "pay.example.com" }), "public_addr"},
		{"port 0", set(func(a *PublicAnswers) { a.PublicPort = 0 }), "public_port"},
		{"port 65536", set(func(a *PublicAnswers) { a.PublicPort = 65536 }), "public_port"},
		{"checkout with query", set(func(a *PublicAnswers) { a.CheckoutBaseURL = "https://pay.example.com/?a=1" }), "checkout_base_url"},
		{"checkout ftp", set(func(a *PublicAnswers) { a.CheckoutBaseURL = "ftp://pay.example.com" }), "checkout_base_url"},
		{"webhook with user info", set(func(a *PublicAnswers) { a.WebhookURL = "https://u:p@merchant.example.com/" }), "webhook_url"},
		{"webhook with fragment", set(func(a *PublicAnswers) { a.WebhookURL = "https://merchant.example.com/#x" }), "webhook_url"},
		{"webhook 513 bytes", set(func(a *PublicAnswers) { a.WebhookURL = "https://m.example.com/" + strings.Repeat("a", 513-22) }), "webhook_url"},
		{"alerts equal to webhook", set(func(a *PublicAnswers) { a.AlertsURL = "HTTPS://Merchant.example.com:443/hooks/satbooth" }), "alerts_url"},
		{"keys without any cap", set(func(a *PublicAnswers) { a.ApprovalKeys = edKeys(1) }), "payout_budget_sat"},
		{"keys without a max", set(func(a *PublicAnswers) { withRefunds(a, 1, 100000); a.PayoutMaxSat = 0 }), "payout_max_sat"},
		{"keys without a daily cap", set(func(a *PublicAnswers) { withRefunds(a, 1, 100000); a.PayoutDailySat = 0 }), "payout_daily_sat"},
		{"alert just above the service's bound", set(func(a *PublicAnswers) { withRefunds(a, 1, maxAmountSat); a.PayoutAlertSat = 9007199254741 }), "payout_alert_sat"},
		{"alert of 2^53 sat", set(func(a *PublicAnswers) { withRefunds(a, 1, maxAmountSat); a.PayoutAlertSat = 1 << 53 }), "payout_alert_sat"},
		{"max just above the service's bound", set(func(a *PublicAnswers) {
			withRefunds(a, 1, maxAmountSat)
			a.PayoutMaxSat, a.PayoutDailySat = 9007199254741, 9007199254741
		}), "payout_max_sat"},
		{"max above daily", set(func(a *PublicAnswers) { withRefunds(a, 1, 100000); a.PayoutMaxSat, a.PayoutDailySat = 20000, 10000 }), "payout_max_sat"},
		{"max above budget", set(func(a *PublicAnswers) { withRefunds(a, 1, 100000); a.PayoutMaxSat, a.PayoutDailySat = 100001, 200000 }), "payout_max_sat"},
		{"daily too large", set(func(a *PublicAnswers) { withRefunds(a, 1, 100000); a.PayoutDailySat = 9007199254741 }), "payout_daily_sat"},
		{"alert equal to budget", set(func(a *PublicAnswers) { withRefunds(a, 1, 100000); a.PayoutAlertSat = 100000 }), "payout_alert_sat"},
		{"alert above budget", set(func(a *PublicAnswers) { withRefunds(a, 1, 100000); a.PayoutAlertSat = 100001 }), "payout_alert_sat"},
		{"budget without keys", setRaw(func(a *PublicAnswers) { a.PayoutBudgetSat = 1000 }), "payout_budget_sat"},
		{"max without keys", setRaw(func(a *PublicAnswers) { a.PayoutMaxSat = 1000 }), "payout_budget_sat"},
		{"daily without keys", setRaw(func(a *PublicAnswers) { a.PayoutDailySat = 1000 }), "payout_budget_sat"},
		{"alert without keys", setRaw(func(a *PublicAnswers) { a.PayoutAlertSat = 1000 }), "payout_budget_sat"},
		{"nine keys", set(func(a *PublicAnswers) { withRefunds(a, 9, 100000) }), "approval_keys"},
		{"duplicate keys", set(func(a *PublicAnswers) {
			withRefunds(a, 1, 100000)
			k := a.ApprovalKeys[0]
			a.ApprovalKeys = []string{k, k}
		}), "approval_keys"},
		{"uppercase key", set(func(a *PublicAnswers) {
			withRefunds(a, 1, 100000)
			a.ApprovalKeys = []string{strings.ToUpper(a.ApprovalKeys[0])}
		}), "approval_keys"},
		{"budget too large", set(func(a *PublicAnswers) { withRefunds(a, 1, 100000); a.PayoutBudgetSat = 1<<63 - 1 }), "payout_budget_sat"},
		{"hub address inside the public bundle network", set(func(a *PublicAnswers) { a.HubPrivateAddr = "172.30.78.1" }), "hub_private_addr"},
		{"hub address on the bundle's own address", set(func(a *PublicAnswers) { a.HubPrivateAddr = "172.30.78.40" }), "hub_private_addr"},
		{"public address inside the public bundle network", set(func(a *PublicAnswers) { a.PublicPrivateAddr = "172.30.78.1" }), "public_private_addr"},
		{"public address on the bundle's own address", set(func(a *PublicAnswers) { a.PublicPrivateAddr = "172.30.78.40" }), "public_private_addr"},
		{"reserved public_addr", set(func(a *PublicAnswers) { a.PublicAddr = "240.0.0.1" }), "public_addr"},
		{"this-network public_addr", set(func(a *PublicAnswers) { a.PublicAddr = "0.1.2.3" }), "public_addr"},
		{"monero without network", set(func(a *PublicAnswers) { a.MoneroAddress = xmrMain }), "monero_network"},
		{"monero network without address", set(func(a *PublicAnswers) { a.MoneroNetwork = "mainnet" }), "monero_network"},
		{"monero bad network", set(func(a *PublicAnswers) { a.MoneroAddress, a.MoneroNetwork = xmrMain, "signet" }), "monero_network"},
		{"monero short address", set(func(a *PublicAnswers) { a.MoneroAddress, a.MoneroNetwork = xmrMain[:94], "mainnet" }), "monero_address"},
		{"monero stagenet-shaped address", set(func(a *PublicAnswers) { a.MoneroAddress, a.MoneroNetwork = "5"+xmrMain[1:], "mainnet" }), "monero_address"},
		{"monero testnet-shaped address", set(func(a *PublicAnswers) { a.MoneroAddress, a.MoneroNetwork = "9"+xmrMain[1:], "mainnet" }), "monero_address"},
		{"monero testnet-shaped address (A)", set(func(a *PublicAnswers) { a.MoneroAddress, a.MoneroNetwork = "A"+xmrMain[1:], "mainnet" }), "monero_address"},
		{"monero subaddress", set(func(a *PublicAnswers) { a.MoneroAddress, a.MoneroNetwork = "8"+xmrMain[1:], "mainnet" }), "monero_address"},
		{"descriptor with xprv", set(func(a *PublicAnswers) {
			// BIP-32 test vector 1's master xprv: a private key, which is refused.
			a.Descriptor = "wpkh(xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHi/0/*)"
		}), "descriptor"},
		{"descriptor for the other network", set(func(a *PublicAnswers) { a.Descriptor = "wpkh(" + tpubV1 + "/0/*)" }), "descriptor"},
		{"descriptor garbage", set(func(a *PublicAnswers) { a.Descriptor = "wpkh(nonsense)" }), "descriptor"},
		{"33 currencies", set(func(a *PublicAnswers) {
			for i := 0; i < 33; i++ {
				a.Currencies = append(a.Currencies, string([]byte{'A' + byte(i/26), 'A' + byte(i%26), 'Z'}))
			}
		}), "currencies"},
		{"BTC currency", set(func(a *PublicAnswers) { a.Currencies = []string{"BTC"} }), "currencies"},
		{"lowercase currency", set(func(a *PublicAnswers) { a.Currencies = []string{"usd"} }), "currencies"},
		{"duplicate currency", set(func(a *PublicAnswers) { a.Currencies = []string{"USD", "USD"} }), "currencies"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := ParsePublicAnswers(c.input)
			if err == nil || a != nil {
				t.Fatalf("accepted: %+v", a)
			}
			if !strings.Contains(err.Error(), c.field) {
				t.Fatalf("error %q does not name %q", err, c.field)
			}
			var raw PublicAnswers
			_ = json.Unmarshal(c.input, &raw)
			for _, secret := range []string{raw.Descriptor, raw.MoneroAddress} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Fatalf("the error echoes a private value: %q", err)
				}
			}
		})
	}
}

// padded returns b followed by spaces up to n bytes: the same content, only longer.
func padded(b []byte, n int) []byte { return append(b, bytes.Repeat([]byte(" "), n-len(b))...) }

// The size limit is exactly 16 KiB: the same content is accepted at 16384 bytes and refused at 16385.
func TestAnswersSizeBoundary(t *testing.T) {
	pub, hub := mustJSON(baseAnswers()), mustJSON(baseHub())
	for _, n := range []int{16384, 16385} {
		_, errP := ParsePublicAnswers(padded(pub, n))
		_, errH := ParseHubAnswers(padded(hub, n))
		if n == 16384 && (errP != nil || errH != nil) {
			t.Fatalf("16384 bytes refused: %v, %v", errP, errH)
		}
		if n == 16385 && (errP == nil || errH == nil || !strings.Contains(errP.Error(), "16 KiB") || !strings.Contains(errH.Error(), "16 KiB")) {
			t.Fatalf("16385 bytes: %v, %v", errP, errH)
		}
	}
}

func TestPublicAnswersDefaults(t *testing.T) {
	a, err := ParsePublicAnswers([]byte(`{"network":"signet","host_dir":"/srv/x","hub_private_addr":"192.0.2.20",` +
		`"public_private_addr":"192.0.2.10","checkout_base_url":"https://pay.example.com","webhook_url":"https://m.example.com/h"}`))
	if err != nil || a.PublicAddr != "127.0.0.1" || a.PublicPort != 8080 {
		t.Fatalf("%+v, %v", a, err)
	}
	// An explicit empty address or port 0 is a refusal, not a default.
	for _, extra := range []string{`,"public_addr":""`, `,"public_port":0`} {
		_, err := ParsePublicAnswers([]byte(`{"network":"signet","host_dir":"/srv/x","hub_private_addr":"192.0.2.20",` +
			`"public_private_addr":"192.0.2.10","checkout_base_url":"https://pay.example.com","webhook_url":"https://m.example.com/h"` + extra + `}`))
		if err == nil {
			t.Fatalf("accepted %s", extra)
		}
	}
}

func baseHub() HubAnswers {
	return HubAnswers{Network: "mainnet", HubPrivateAddr: "192.0.2.20", PublicPrivateAddr: "192.0.2.10", HostDir: "/srv/satbooth-hub", HubAdminAddr: "127.0.0.1", HubSubnet: "172.30.78.0/25",
		HubIP: "172.30.78.10", ElectrumSubnet: "172.30.78.128/25"}
}

func TestHubAnswersDefaults(t *testing.T) {
	a, err := ParseHubAnswers([]byte(`{"network":"mainnet","hub_private_addr":"192.0.2.20","public_private_addr":"192.0.2.10","host_dir":"/srv/satbooth-hub"}`))
	if err != nil || *a != baseHub() {
		t.Fatalf("%+v, %v", a, err)
	}
}

func TestHubAnswersRefusals(t *testing.T) {
	set := func(f func(a *HubAnswers)) []byte { a := baseHub(); f(&a); return mustJSON(a) }
	cases := []struct {
		name  string
		input []byte
		field string
	}{
		{"unknown key", []byte(`{"network":"mainnet","hub_admin":"127.0.0.1"}`), "unknown"},
		{"duplicate key", []byte(`{"network":"mainnet","host_dir":"/srv/x","host_dir":"/srv/x"}`), "host_dir"},
		{"relative dir", set(func(a *HubAnswers) { a.HostDir = "x" }), "host_dir"},
		{"no network", set(func(a *HubAnswers) { a.Network = "" }), "network"},
		{"hub_ip is the gateway", set(func(a *HubAnswers) { a.HubIP = "172.30.78.1" }), "hub_ip"},
		{"hub_ip is the broadcast address", set(func(a *HubAnswers) { a.HubIP = "172.30.78.127" }), "hub_ip"},
		{"hub_ip in the other subnet's gateway", set(func(a *HubAnswers) {
			a.HubSubnet, a.ElectrumSubnet, a.HubIP = "172.30.78.128/25", "172.30.78.0/25", "172.30.78.129"
		}), "hub_ip"},
		{"/30 subnet", set(func(a *HubAnswers) { a.HubSubnet, a.HubIP = "172.30.78.0/30", "172.30.78.2" }), "hub_subnet"},
		{"/15 subnet", set(func(a *HubAnswers) { a.ElectrumSubnet = "172.30.0.0/15" }), "electrum_subnet"},
		{"/16 subnet overlapping", set(func(a *HubAnswers) { a.ElectrumSubnet = "172.30.0.0/16" }), "electrum_subnet"},
		{"hub address inside hub_subnet", set(func(a *HubAnswers) { a.HubPrivateAddr = "172.30.78.20" }), "hub_private_addr"},
		{"hub address inside electrum_subnet", set(func(a *HubAnswers) { a.HubPrivateAddr = "172.30.78.200" }), "hub_private_addr"},
		{"public address inside hub_subnet", set(func(a *HubAnswers) { a.PublicPrivateAddr = "172.30.78.20" }), "public_private_addr"},
		{"public address inside electrum_subnet", set(func(a *HubAnswers) { a.PublicPrivateAddr = "172.30.78.200" }), "public_private_addr"},
		{"hub address inside a custom subnet", set(func(a *HubAnswers) { a.HubSubnet, a.HubIP, a.HubPrivateAddr = "10.9.0.0/24", "10.9.0.10", "10.9.0.77" }), "hub_private_addr"},
		{"admin inside hub_subnet", set(func(a *HubAnswers) { a.HubAdminAddr = "172.30.78.1" }), "hub_admin_addr"},
		{"admin inside electrum_subnet", set(func(a *HubAnswers) { a.HubAdminAddr = "172.30.78.130" }), "hub_admin_addr"},
		{"admin equals the public host's address", set(func(a *HubAnswers) { a.HubAdminAddr, a.PublicPrivateAddr = "10.0.0.1", "10.0.0.1" }), "hub_admin_addr"},
		{"admin reserved", set(func(a *HubAnswers) { a.HubAdminAddr = "240.0.0.1" }), "hub_admin_addr"},
		{"testnet", set(func(a *HubAnswers) { a.Network = "testnet" }), "network"},
		{"hostname", set(func(a *HubAnswers) { a.HubPrivateAddr = "hub.example.com" }), "hub_private_addr"},
		{"zone", set(func(a *HubAnswers) { a.PublicPrivateAddr = "fe80::1%eth0" }), "public_private_addr"},
		{"unspecified", set(func(a *HubAnswers) { a.HubPrivateAddr = "0.0.0.0" }), "hub_private_addr"},
		{"multicast", set(func(a *HubAnswers) { a.HubPrivateAddr = "224.0.0.1" }), "hub_private_addr"},
		{"link-local", set(func(a *HubAnswers) { a.PublicPrivateAddr = "169.254.1.1" }), "public_private_addr"},
		{"broadcast", set(func(a *HubAnswers) { a.PublicPrivateAddr = "255.255.255.255" }), "public_private_addr"},
		{"ipv4-mapped", set(func(a *HubAnswers) { a.HubPrivateAddr = "::ffff:192.0.2.20" }), "hub_private_addr"},
		{"loopback on mainnet", set(func(a *HubAnswers) { a.HubPrivateAddr = "127.0.0.1" }), "hub_private_addr"},
		{"equal private addresses", set(func(a *HubAnswers) { a.PublicPrivateAddr = a.HubPrivateAddr }), "public_private_addr"},
		{"admin equals the private address", set(func(a *HubAnswers) { a.HubAdminAddr = "10.0.0.5"; a.HubPrivateAddr = "10.0.0.5" }), "hub_admin_addr"},
		{"ip outside subnet", set(func(a *HubAnswers) { a.HubIP = "172.30.78.130" }), "hub_ip"},
		{"ip is the network address", set(func(a *HubAnswers) { a.HubIP = "172.30.78.0" }), "hub_ip"},
		{"ipv6 ip", set(func(a *HubAnswers) { a.HubIP = "fd00::10" }), "hub_ip"},
		{"overlapping subnets", set(func(a *HubAnswers) { a.ElectrumSubnet = "172.30.78.64/26" }), "electrum_subnet"},
		{"equal subnets", set(func(a *HubAnswers) { a.ElectrumSubnet = a.HubSubnet }), "electrum_subnet"},
		{"unmasked subnet", set(func(a *HubAnswers) { a.HubSubnet = "172.30.78.10/25" }), "hub_subnet"},
		{"ipv6 subnet", set(func(a *HubAnswers) { a.ElectrumSubnet = "fd00::/64" }), "electrum_subnet"},
		{"huge subnet", set(func(a *HubAnswers) { a.HubSubnet = "172.0.0.0/4" }), "hub_subnet"},
		{"admin on a public address", set(func(a *HubAnswers) { a.HubAdminAddr = "198.51.100.7" }), "hub_admin_addr"},
		{"admin wildcard", set(func(a *HubAnswers) { a.HubAdminAddr = "0.0.0.0" }), "hub_admin_addr"},
		{"admin hostname", set(func(a *HubAnswers) { a.HubAdminAddr = "localhost" }), "hub_admin_addr"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if a, err := ParseHubAnswers(c.input); err == nil || !strings.Contains(err.Error(), c.field) {
				t.Fatalf("got %+v, %v", a, err)
			}
		})
	}
	// Loopback is accepted on regtest only.
	r := baseHub()
	r.Network, r.HubPrivateAddr = "regtest", "127.0.0.1"
	r.HubAdminAddr = "127.0.0.2"
	if _, err := ParseHubAnswers(mustJSON(r)); err != nil {
		t.Fatalf("loopback on regtest: %v", err)
	}
	// Against the handoff: the two hosts' operators must have typed the same network and addresses.
	h := baseHub()
	in := &PublicToHub{Version: 1, Network: "mainnet", HubPrivateAddr: "192.0.2.20", PublicPrivateAddr: "192.0.2.10", InvoiceClient: curveKeys[0]}
	if err := h.CheckAgainst(in); err != nil {
		t.Fatalf("matching handoff: %v", err)
	}
	for name, f := range map[string]func(in *PublicToHub){
		"network":             func(in *PublicToHub) { in.Network = "signet" },
		"hub_private_addr":    func(in *PublicToHub) { in.HubPrivateAddr = "192.0.2.21" },
		"public_private_addr": func(in *PublicToHub) { in.PublicPrivateAddr = "192.0.2.11" },
	} {
		c := *in
		f(&c)
		err := h.CheckAgainst(&c)
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "different values") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The admin address differs from both private addresses.
	for _, addr := range []string{"192.0.2.20", "192.0.2.10"} {
		h.HubAdminAddr = addr
		if err := h.CheckAgainst(in); err == nil || !strings.Contains(err.Error(), "hub_admin_addr") {
			t.Errorf("admin equals %s: %v", addr, err)
		}
	}
}

// edgeAnswers is the table of TestAcceptedAnswersAreAcceptedByService: values at and beyond the edges of
// service.ParseConfig's rules. Whenever ParsePublicAnswers accepts one, the real parser must accept the file made
// from it.
func edgeAnswers() []PublicAnswers {
	var out []PublicAnswers
	add := func(f func(a *PublicAnswers)) { a := baseAnswers(); f(&a); out = append(out, a) }
	long := func(prefix string) string { return prefix + strings.Repeat("a", maxURLBytes-len(prefix)) }
	add(func(a *PublicAnswers) {})
	for _, u := range []string{"https://pay.example.com/", "https://pay.example.com/shop/checkout", "https://pay.example.com:8443",
		"http://pay.example.com", "http://127.0.0.1:8080/x", "https://[fd00::1]/", "HTTPS://PAY.EXAMPLE.COM", long("https://pay.example.com/"),
		"https://pay.example.com/" + strings.Repeat("a", maxURLBytes), "https://pay.example.com?", "https://pay.example.com/a?b=c",
		"https://pay.example.com:1", "https://pay.example.com:65535/", "https://pay.example.com:0/", "https://pay.example.com:65536/",
		"https://pay.example.com:99999/", "https://pay.example.com:/", "https://pay.example.com:080/", "https://[fd00::1]:8443/",
		"https://[fd00::1]:/", "https://[fd00::1]:0/"} {
		add(func(a *PublicAnswers) { a.CheckoutBaseURL = u })
	}
	for _, u := range []string{"https://m.example.com", "https://m.example.com/h?x=1&y=2", "http://m.example.com:80/", long("https://m.example.com/"),
		"https://m.example.com/é", "https://m.example.com/a b",
		"https://m.example.com:1/h", "https://m.example.com:65535/h", "https://m.example.com:0/h", "https://m.example.com:65536/h",
		"https://m.example.com:/h", "https://m.example.com:+443/h", "https://m.example.com:-1/h", "https://m.example.com:44a/h"} {
		add(func(a *PublicAnswers) { a.WebhookURL = u })
	}
	for _, u := range []string{"https://monitor.example.com/alert", "http://monitor.example.com:80", "https://merchant.example.com/hooks/satbooth/",
		"https://merchant.example.com/hooks/satbooth", "https://merchant.example.com:443/hooks/satbooth", long("https://monitor.example.com/"),
		"https://monitor.example.com:65535/a", "https://monitor.example.com:0/a", "https://monitor.example.com:65536/a",
		"https://monitor.example.com:/a"} {
		add(func(a *PublicAnswers) { a.AlertsURL = u })
	}
	for _, n := range []string{"mainnet", "signet", "regtest"} {
		key := tpubV1
		if n == "mainnet" {
			key = xpubV1
		}
		for _, d := range []string{"wpkh(" + key + "/0/*)", "tr(" + key + "/0/*)", "wpkh([d34db33f/84h/0h/0h]" + key + "/0/*)",
			"wpkh([d34db33f/84'/0'/0']" + key + "/0/*)#qpzry9x8", "wpkh(" + key + "/1/*)", "wpkh(" + tpubV1 + "/0/*)", "wpkh(" + xpubV1 + "/0/*)"} {
			add(func(a *PublicAnswers) { a.Network, a.Descriptor = n, d })
		}
		for _, mn := range []string{"mainnet", "stagenet", "testnet", "regtest"} {
			for _, first := range []string{"4", "5", "9", "A", "8"} {
				add(func(a *PublicAnswers) { a.Network, a.MoneroNetwork, a.MoneroAddress = n, mn, first+xmrMain[1:] })
			}
		}
		for _, k := range []int{1, 2, 8} {
			add(func(a *PublicAnswers) { a.Network = n; withRefunds(a, k, 100000) })
		}
	}
	for _, b := range []uint64{1, 1000, 1 << 40, maxAmountSat, maxAmountSat + 1} {
		add(func(a *PublicAnswers) { withRefunds(a, 1, b) })
	}
	add(func(a *PublicAnswers) { a.Currencies = []string{"USD"} })
	add(func(a *PublicAnswers) { a.Currencies = []string{"USD", "EUR", "GBP", "JPY"} })
	add(func(a *PublicAnswers) { a.Currencies = []string{"ZZZ", "AAA"} })
	add(func(a *PublicAnswers) {
		for i := 0; i < 32; i++ {
			a.Currencies = append(a.Currencies, string([]byte{'A' + byte(i/26), 'A' + byte(i%26), 'Q'}))
		}
	})
	add(func(a *PublicAnswers) {
		a.Network, a.Descriptor, a.MoneroAddress, a.MoneroNetwork = "mainnet", "tr("+xpubV1+"/0/*)", xmrMain, "mainnet"
		withRefunds(a, 8, 21000)
		a.Currencies = []string{"USD", "EUR"}
		a.AlertsURL = "https://monitor.example.com/alert"
	})
	for _, pa := range []string{"127.0.0.1", "192.0.2.5", "0.0.0.0", "::1", "::", "224.0.0.1", "169.254.1.1", "255.255.255.255", "fd00::1", "example.com", "127.0.0.1%lo"} {
		add(func(a *PublicAnswers) { a.PublicAddr = pa })
	}
	add(func(a *PublicAnswers) { a.Network, a.HubPrivateAddr = "regtest", "127.0.0.1" })
	add(func(a *PublicAnswers) { a.HubPrivateAddr = "127.0.0.1" })
	return out
}

func TestAcceptedAnswersAreAcceptedByService(t *testing.T) {
	accepted := 0
	for i, a := range edgeAnswers() {
		pa, err := ParsePublicAnswers(mustJSON(a))
		if err != nil {
			continue
		}
		accepted++
		cfg := SatboothJSON(pa, fxHubToPublic(len(pa.ApprovalKeys) > 0))
		if _, err := service.ParseConfig(cfg); err != nil {
			t.Errorf("case %d: setup accepts, service refuses: %v", i, err)
		}
	}
	if accepted < 40 {
		t.Fatalf("only %d of the table is accepted: the table is not exercising the mirror", accepted)
	}
}

// A seeded property test: mutate a valid answers object field by field with values from every character class and
// length class. The parser never panics; whenever it accepts, service.ParseConfig accepts the file made from it; no
// error carries the descriptor or the Monero address.
func TestParsePublicAnswersProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261111))
	odd := []string{"", " ", "\n", "\x00", "é", "‮", "\"", "\\", "$", "%", "#", "?", "@", "/", "..", "ftp://x", "https://",
		"https://a", "http://[::1]:99999/", "https://a:0/", "https://a:1/", "https://a:65535/", "https://a:65536/", "https://a:/",
		"https://[::1]:/", "https://a:+1/", "https://a:001/", "https://a/b c", "::", "1.2.3.4", "300.1.1.1", "fd00::1", "999999999999999999999",
		strings.Repeat("a", 511), strings.Repeat("a", 512), strings.Repeat("a", 513), strings.Repeat("a", 4000)}
	fields := []string{"network", "host_dir", "hub_private_addr", "public_private_addr", "public_addr", "public_port", "checkout_base_url",
		"webhook_url", "alerts_url", "descriptor", "monero_address", "monero_network", "approval_keys", "payout_budget_sat", "payout_max_sat", "payout_daily_sat", "payout_alert_sat", "currencies"}
	table := edgeAnswers()
	accepted := 0
	for i := 0; i < 4000; i++ {
		var m map[string]any
		_ = json.Unmarshal(mustJSON(table[rng.Intn(len(table))]), &m)
		for n := rng.Intn(3); n >= 0; n-- {
			f := fields[rng.Intn(len(fields))]
			s := odd[rng.Intn(len(odd))]
			switch f {
			case "public_port", "payout_budget_sat", "payout_max_sat", "payout_daily_sat", "payout_alert_sat":
				m[f] = []any{0, 1, 65535, 65536, -1, 1 << 53, "x", 9007199254740, 9007199254741, uint64(maxAmountSat), uint64(maxAmountSat) + 1, 10000, 50000}[rng.Intn(13)]
			case "approval_keys":
				m[f] = []any{[]string{s}, edKeys(1 + rng.Intn(10)), []string{}}[rng.Intn(3)]
			case "currencies":
				m[f] = []string{[]string{"USD", s, "EUR", "usd", "XMR"}[rng.Intn(5)], "GBP"}
			default:
				if rng.Intn(3) == 0 {
					m[f] = asString(m[f]) + s
				} else {
					m[f] = s
				}
			}
		}
		in, _ := json.Marshal(m)
		a, err := ParsePublicAnswers(in)
		if err != nil {
			for _, secret := range []string{asString(m["descriptor"]), asString(m["monero_address"])} {
				if len(secret) > 3 && strings.Contains(err.Error(), secret) {
					t.Fatalf("the error echoes a private value: %q", err)
				}
			}
			continue
		}
		accepted++
		if _, err := service.ParseConfig(SatboothJSON(a, fxHubToPublic(len(a.ApprovalKeys) > 0))); err != nil {
			t.Fatalf("iteration %d: setup accepts, service refuses: %v\ninput: %s", i, err, strconv.Quote(string(in)))
		}
	}
	if accepted < 50 {
		t.Fatalf("only %d of 4000 mutations accepted", accepted)
	}
}

func asString(v any) string { s, _ := v.(string); return s }

// TestAddressRule: every private-link field of both structs refuses these addresses, names its field and gives the
// reason; loopback only on regtest.
func TestAddressRule(t *testing.T) {
	type target struct {
		field string
		parse func(network, addr string) error
	}
	pubFor := func(field string) func(network, addr string) error {
		return func(network, addr string) error {
			a := baseAnswers()
			a.Network = network
			if field == "hub_private_addr" {
				a.HubPrivateAddr = addr
			} else {
				a.PublicPrivateAddr = addr
			}
			_, err := ParsePublicAnswers(mustJSON(a))
			return err
		}
	}
	hubFor := func(field string) func(network, addr string) error {
		return func(network, addr string) error {
			a := baseHub()
			a.Network, a.HubAdminAddr = network, "127.0.0.9"
			if field == "hub_private_addr" {
				a.HubPrivateAddr = addr
			} else {
				a.PublicPrivateAddr = addr
			}
			_, err := ParseHubAnswers(mustJSON(a))
			return err
		}
	}
	var targets []target
	for _, f := range []string{"hub_private_addr", "public_private_addr"} {
		targets = append(targets, target{f, pubFor(f)}, target{f, hubFor(f)})
	}
	bad := []string{"fd00::20", "::ffff:192.0.2.1", "::1", "0.0.0.0", "0.1.2.3", "224.0.0.1", "239.255.255.255", "240.0.0.1", "169.254.1.1",
		"255.255.255.255", "192.0.2.1%eth0", "fe80::1%eth0", "", "hub.example.com", "192.0.2.1:7448", "192.0.2.0/24", " 192.0.2.1"}
	for _, tg := range targets {
		for _, network := range []string{"mainnet", "signet", "regtest"} {
			for _, addr := range bad {
				err := tg.parse(network, addr)
				if err == nil || !strings.Contains(err.Error(), tg.field) {
					t.Errorf("%s %s %q: %v", tg.field, network, addr, err)
					continue
				}
				reason := "IPv4 address of the private link"
				if ip, perr := netip.ParseAddr(addr); perr == nil && !ip.Is4() {
					reason = "set up an IPv6 private link by hand (deploy/README.md)"
				}
				if !strings.Contains(err.Error(), reason) {
					t.Errorf("%s %s %q: reason %q missing from %v", tg.field, network, addr, reason, err)
				}
			}
		}
		for _, network := range []string{"mainnet", "signet", "regtest"} {
			err := tg.parse(network, "127.0.0.1")
			if (err == nil) != (network == "regtest") {
				t.Errorf("%s loopback on %s: %v", tg.field, network, err)
			}
			if err != nil && (!strings.Contains(err.Error(), tg.field) || !strings.Contains(err.Error(), "loopback only on regtest")) {
				t.Errorf("%s loopback on %s: %v", tg.field, network, err)
			}
		}
	}
	// public_addr: IPv4 loopback or unicast; never a wildcard.
	for addr, ok := range map[string]bool{"127.0.0.1": true, "192.0.2.5": true, "0.0.0.0": false, "::": false, "::1": false, "fd00::1": false,
		"224.0.0.1": false, "169.254.1.1": false, "255.255.255.255": false, "240.0.0.1": false, "pay.example.com": false, "192.0.2.1%eth0": false} {
		a := baseAnswers()
		a.PublicAddr = addr
		_, err := ParsePublicAnswers(mustJSON(a))
		if (err == nil) != ok || (err != nil && !strings.Contains(err.Error(), "public_addr")) {
			t.Errorf("public_addr %q: %v", addr, err)
		}
		if addr == "0.0.0.0" && (err == nil || !strings.Contains(err.Error(), "127.0.0.1 behind a reverse proxy")) {
			t.Errorf("the wildcard message: %v", err)
		}
	}
}

// Legitimate answers are accepted: a refusal here is a regression toward refusing what the service takes.
func TestLegitimateAnswersAreAccepted(t *testing.T) {
	long := "https://pay.example.com/" + strings.Repeat("a", maxURLBytes-len("https://pay.example.com/"))
	cases := map[string]func(a *PublicAnswers){
		"checkout with a path":       func(a *PublicAnswers) { a.CheckoutBaseURL = "https://pay.example.com/shop" },
		"checkout with a slash":      func(a *PublicAnswers) { a.CheckoutBaseURL = "https://pay.example.com/" },
		"checkout with a port":       func(a *PublicAnswers) { a.CheckoutBaseURL = "https://pay.example.com:8443" },
		"checkout over http":         func(a *PublicAnswers) { a.CheckoutBaseURL = "http://pay.example.com" },
		"checkout of 512 bytes":      func(a *PublicAnswers) { a.CheckoutBaseURL = long },
		"checkout with an IPv6 host": func(a *PublicAnswers) { a.CheckoutBaseURL = "https://[fd00::1]/" },
		"taproot descriptor":         func(a *PublicAnswers) { a.Descriptor = "tr(" + xpubV1 + "/0/*)" },
		"origin with apostrophes":    func(a *PublicAnswers) { a.Descriptor = "wpkh([d34db33f/84'/0'/0']" + xpubV1 + "/0/*)" },
		"origin with h":              func(a *PublicAnswers) { a.Descriptor = "wpkh([d34db33f/84h/0h/0h]" + xpubV1 + "/0/*)" },
		// The checksums were computed with BIP 380's algorithm (verified on its test vector raw(deadbeef)#89f8spxm).
		"valid checksum":             func(a *PublicAnswers) { a.Descriptor = "wpkh(" + xpubV1 + "/0/*)#wvk84d79" },
		"valid checksum with origin": func(a *PublicAnswers) { a.Descriptor = "wpkh([d34db33f/84'/0'/0']" + xpubV1 + "/0/*)#zpuvqnlw" },
		"one approval key":           func(a *PublicAnswers) { withRefunds(a, 1, 100000) },
		"the largest alert":          func(a *PublicAnswers) { withRefunds(a, 1, 20000000000000); a.PayoutAlertSat = 9007199254740 },
		"the largest caps": func(a *PublicAnswers) {
			withRefunds(a, 1, maxAmountSat)
			a.PayoutMaxSat, a.PayoutDailySat, a.PayoutAlertSat = 9007199254740, 9007199254740, 9007199254740
		},
		"eight approval keys": func(a *PublicAnswers) { withRefunds(a, 8, 100000) },
		"one currency":        func(a *PublicAnswers) { a.Currencies = []string{"USD"} },
		"several currencies":  func(a *PublicAnswers) { a.Currencies = []string{"USD", "EUR", "GBP", "JPY", "CHF"} },
		"monero":              func(a *PublicAnswers) { a.MoneroAddress, a.MoneroNetwork = xmrMain, "mainnet" },
		"alerts":              func(a *PublicAnswers) { a.AlertsURL = "https://monitor.example.com/alert" },
		"32 currencies": func(a *PublicAnswers) {
			for i := 0; i < 32; i++ {
				a.Currencies = append(a.Currencies, string([]byte{'A' + byte(i/26), 'A' + byte(i%26), 'Q'}))
			}
		},
	}
	for name, f := range cases {
		for _, network := range []string{"mainnet", "signet", "regtest"} {
			a := baseAnswers()
			a.Network = network
			if strings.Contains(name, "descriptor") || strings.Contains(name, "origin") || strings.Contains(name, "checksum") {
				key := tpubV1
				if network == "mainnet" {
					key = xpubV1
				}
				f(&a)
				a.Descriptor = strings.ReplaceAll(a.Descriptor, xpubV1, key)
				if network != "mainnet" && strings.Contains(name, "checksum") {
					continue // the checksums are of the xpub forms
				}
			} else {
				f(&a)
			}
			pa, err := ParsePublicAnswers(mustJSON(a))
			if err != nil {
				t.Errorf("%s on %s: refused: %v", name, network, err)
				continue
			}
			if _, err := service.ParseConfig(SatboothJSON(pa, fxHubToPublic(len(pa.ApprovalKeys) > 0))); err != nil {
				t.Errorf("%s on %s: service refuses: %v", name, network, err)
			}
		}
	}
}

// The descriptor and the Monero address are in satbooth.json and in no other rendered file; no error holds them.
func TestPrivateValuesAreOnlyInSatboothJSON(t *testing.T) {
	f := fxEveryRail(t)
	for name, content := range f.files {
		for what, v := range map[string]string{"descriptor": f.pub.Descriptor, "monero address": f.pub.MoneroAddress} {
			if strings.Contains(string(content), v) != (name == "satbooth.json") {
				t.Errorf("%s: %s present = %v", name, what, !(name == "satbooth.json"))
			}
		}
	}
	// And in no error text: put them into answers that fail elsewhere.
	a := f.pub
	a.PublicPort = 0
	if _, err := ParsePublicAnswers(mustJSON(a)); err == nil || strings.Contains(err.Error(), a.Descriptor) || strings.Contains(err.Error(), a.MoneroAddress) {
		t.Errorf("error: %v", err)
	}
}

func u32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func inRange(v, lo, hi uint32) bool { return v >= lo && v <= hi }

// A seeded property test of ParseHubAnswers and CheckAgainst: CIDRs of every prefix length (aligned and not),
// addresses on and around every boundary, every character class in the other fields. Whenever the parser accepts,
// each field is checked here with integer arithmetic, not with the parser's helpers.
func TestParseHubAnswersProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261112))
	addrs := []string{"127.0.0.1", "127.0.0.9", "10.0.0.5", "10.9.0.77", "192.0.2.10", "192.0.2.20", "192.168.1.1", "172.16.0.1", "172.31.255.255",
		"172.32.0.1", "172.30.78.0", "172.30.78.1", "172.30.78.2", "172.30.78.10", "172.30.78.126", "172.30.78.127", "172.30.78.128", "172.30.78.129",
		"172.30.78.130", "172.30.78.254", "172.30.78.255", "172.30.79.1", "0.0.0.0", "0.1.1.1", "169.254.0.1", "224.0.0.1", "240.0.0.1",
		"255.255.255.255", "::1", "fd00::1", "::ffff:10.0.0.1", "10.0.0.1%eth0", "", "x", "10.0.0.256", "1.2.3"}
	cidr := func() string {
		base := []string{"172.30.78.0", "172.30.78.128", "172.30.0.0", "172.30.78.64", "10.0.0.0", "10.9.0.0", "172.30.78.5", "192.0.2.0", "127.0.0.0"}[rng.Intn(9)]
		return base + "/" + strconv.Itoa(rng.Intn(34))
	}
	odd := []string{"", " ", "\n", "/", "..", "$", "\u00e9", strings.Repeat("a", 300)}
	networks := []string{"mainnet", "signet", "regtest", "testnet", "", "MAINNET"}
	accepted := 0
	for i := 0; i < 20000; i++ {
		var m map[string]any
		_ = json.Unmarshal(mustJSON(baseHub()), &m)
		for n := rng.Intn(4); n >= 0; n-- {
			switch rng.Intn(8) {
			case 0:
				m["network"] = networks[rng.Intn(len(networks))]
			case 1:
				m["hub_private_addr"] = addrs[rng.Intn(len(addrs))]
			case 2:
				m["public_private_addr"] = addrs[rng.Intn(len(addrs))]
			case 3:
				m["hub_admin_addr"] = addrs[rng.Intn(len(addrs))]
			case 4:
				m["hub_subnet"] = cidr()
			case 5:
				m["electrum_subnet"] = cidr()
			case 6:
				m["hub_ip"] = addrs[rng.Intn(len(addrs))]
			default:
				m["host_dir"] = []string{"/srv/h", "/srv/h/", "/", "srv", "/srv/" + odd[rng.Intn(len(odd))]}[rng.Intn(5)]
			}
		}
		in, _ := json.Marshal(m)
		a, err := ParseHubAnswers(in)
		if err != nil {
			continue
		}
		accepted++
		hubP, errH := netip.ParsePrefix(a.HubSubnet)
		eleP, errE := netip.ParsePrefix(a.ElectrumSubnet)
		if errH != nil || errE != nil {
			t.Fatalf("accepted subnets that do not parse: %s", in)
		}
		for _, p := range []netip.Prefix{hubP, eleP} {
			if !p.Addr().Is4() || p.Bits() < 16 || p.Bits() > 29 || p.Masked() != p {
				t.Fatalf("subnet %s accepted: %s", p, in)
			}
		}
		lo := func(p netip.Prefix) uint32 { return u32(p.Addr()) }
		hi := func(p netip.Prefix) uint32 { return u32(p.Addr()) | (uint32(1)<<(32-p.Bits()) - 1) }
		if lo(hubP) <= hi(eleP) && lo(eleP) <= hi(hubP) {
			t.Fatalf("overlapping subnets accepted: %s", in)
		}
		ip := netip.MustParseAddr(a.HubIP)
		if v := u32(ip); !ip.Is4() || v <= lo(hubP)+1 || v >= hi(hubP) || v > hi(hubP) {
			t.Fatalf("hub_ip %s outside the usable hosts of %s: %s", ip, hubP, in)
		}
		inNet := func(v uint32) bool { return inRange(v, lo(hubP), hi(hubP)) || inRange(v, lo(eleP), hi(eleP)) }
		for _, s := range []string{a.HubPrivateAddr, a.PublicPrivateAddr} {
			p := netip.MustParseAddr(s)
			v := u32(p)
			if !p.Is4() || p.Zone() != "" || v>>24 == 0 || v>>24 >= 224 || v>>16 == 0xa9fe || inNet(v) || (v>>24 == 127 && a.Network != "regtest") {
				t.Fatalf("private-link address %s accepted: %s", s, in)
			}
		}
		if a.HubPrivateAddr == a.PublicPrivateAddr {
			t.Fatalf("equal private addresses accepted: %s", in)
		}
		ad := netip.MustParseAddr(a.HubAdminAddr)
		av := u32(ad)
		private := av>>24 == 127 || av>>24 == 10 || inRange(av, 0xac100000, 0xac1fffff) || av>>16 == 0xc0a8
		if !ad.Is4() || !private || inNet(av) || ad.String() == a.HubPrivateAddr || ad.String() == a.PublicPrivateAddr {
			t.Fatalf("admin address %s accepted: %s", ad, in)
		}
		if a.Network != "mainnet" && a.Network != "signet" && a.Network != "regtest" {
			t.Fatalf("network %q accepted", a.Network)
		}
		// CheckAgainst: the handoff of the same operator matches; any difference is refused.
		h := &PublicToHub{Version: 1, Network: a.Network, HubPrivateAddr: a.HubPrivateAddr, PublicPrivateAddr: a.PublicPrivateAddr, InvoiceClient: curveKeys[0]}
		if err := a.CheckAgainst(h); err != nil {
			t.Fatalf("matching handoff refused: %v: %s", err, in)
		}
		for name, mutate := range map[string]func(h *PublicToHub){
			"network":             func(h *PublicToHub) { h.Network = map[string]string{"mainnet": "signet"}[h.Network] + "x" },
			"hub_private_addr":    func(h *PublicToHub) { h.HubPrivateAddr = "198.51.100.1" },
			"public_private_addr": func(h *PublicToHub) { h.PublicPrivateAddr = "198.51.100.2" },
		} {
			c := *h
			mutate(&c)
			if err := a.CheckAgainst(&c); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("handoff with another %s: %v", name, err)
			}
		}
		// The rendered hub files are well formed.
		out := &HubToPublic{Version: 1, InvoiceWallet: curveKeys[2]}
		if _, err := relay.ParseConfig(RelayJSON(a, h, out)); err != nil {
			t.Fatalf("relay refuses the rendered file: %v: %s", err, in)
		}
		env := string(HubComposeEnv(a))
		if lines := strings.Split(strings.TrimSuffix(env, "\n"), "\n"); len(lines) != 8 || strings.ContainsAny(env, " \"'$\r\t") {
			t.Fatalf("hub env file: %q", env)
		}
	}
	if accepted < 300 {
		t.Fatalf("only %d of 20000 accepted: the generator does not reach the accepting side", accepted)
	}
}

// Each cap key is checked for presence and for null before the ordering rules, and without approval_keys a cap key
// that is present is refused whatever its value.
func TestPayoutCapKeys(t *testing.T) {
	keys := []string{"payout_budget_sat", "payout_max_sat", "payout_daily_sat", "payout_alert_sat"}
	build := func(refunds bool, f func(m map[string]any)) []byte {
		a := baseAnswers()
		if refunds {
			withRefunds(&a, 1, 100000)
			a.PayoutAlertSat = 5000
		}
		var m map[string]any
		_ = json.Unmarshal(mustJSON(a), &m)
		f(m)
		b, _ := json.Marshal(m)
		return b
	}
	if _, err := ParsePublicAnswers(build(true, func(m map[string]any) {})); err != nil {
		t.Fatalf("base with refunds: %v", err)
	}
	for _, k := range keys {
		// Without approval_keys: present, even as 0 or null, is refused; absent is fine.
		for _, v := range []any{0, 1, nil} {
			in := build(false, func(m map[string]any) { m[k] = v })
			if _, err := ParsePublicAnswers(in); err == nil || !strings.Contains(err.Error(), k) || !strings.Contains(err.Error(), "without approval_keys") {
				t.Errorf("%s=%v without keys: %v", k, v, err)
			}
		}
		if _, err := ParsePublicAnswers(build(false, func(m map[string]any) { delete(m, k) })); err != nil {
			t.Errorf("%s absent without keys: %v", k, err)
		}
		// With approval_keys: null is refused, naming the key.
		in := build(true, func(m map[string]any) { m[k] = nil })
		_, err := ParsePublicAnswers(in)
		if err == nil || !strings.Contains(err.Error(), k) || !strings.Contains(err.Error(), "not null") {
			t.Errorf("%s null with keys: %v", k, err)
		}
		if k == "payout_alert_sat" && (err == nil || !strings.Contains(err.Error(), "give 0 for no alert")) {
			t.Errorf("the alert's null message: %v", err)
		}
		// With approval_keys: an absent key is refused under its own name, before any ordering rule.
		_, err = ParsePublicAnswers(build(true, func(m map[string]any) { delete(m, k) }))
		if err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s absent with keys: %v", k, err)
		}
	}
	// An alert of 0 is a given value: no alert.
	if _, err := ParsePublicAnswers(build(true, func(m map[string]any) { m["payout_alert_sat"] = 0 })); err != nil {
		t.Errorf("alert 0: %v", err)
	}
}

// The hub bundle runs Monero on mainnet only: any other network is refused, on every bitcoin network, with the field
// name and the way out, and the address is never echoed.
func TestMoneroIsMainnetOnly(t *testing.T) {
	const want = `the hub bundle runs Monero on mainnet only: set up a stagenet or testnet monerod and wallet-rpc by hand ` +
		`(docs/monero.md, "Setting up monerod and monero-wallet-rpc (Monero)") and add the monero block to satbooth.json`
	for _, btc := range []string{"mainnet", "signet", "regtest"} {
		for _, mn := range []string{"stagenet", "testnet", "regtest"} {
			a := baseAnswers()
			a.Network, a.MoneroAddress, a.MoneroNetwork = btc, xmrMain, mn
			_, err := ParsePublicAnswers(mustJSON(a))
			if err == nil || !strings.Contains(err.Error(), "monero_network") || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), xmrMain) {
				t.Errorf("%s/%s: %v", btc, mn, err)
			}
		}
		a := baseAnswers()
		a.Network, a.MoneroAddress, a.MoneroNetwork = btc, xmrMain, "mainnet"
		if _, err := ParsePublicAnswers(mustJSON(a)); err != nil {
			t.Errorf("mainnet Monero on %s: %v", btc, err)
		}
	}
	a := baseAnswers()
	a.MoneroAddress, a.MoneroNetwork = xmrMain, "mainnet-ish"
	if _, err := ParsePublicAnswers(mustJSON(a)); err == nil || !strings.Contains(err.Error(), "monero_network") || strings.Contains(err.Error(), xmrMain) {
		t.Errorf("unknown network: %v", err)
	}
}

// A port that is present is 1 to 65535 in decimal digits, without a sign, a leading zero or a space; a colon with
// nothing after it is refused. The same table is in service's tests (TestURLPorts) for plainURL.
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
			_, err := checkURL("f", u, noQuery, "https")
			if (err == nil) != want {
				t.Errorf("%q (noQuery %v): err %v, want accepted %v", u, noQuery, err, want)
			}
		}
	}
}

// setup's port rule is a copy of btcrpc.ValidPort (the one service uses), kept apart so satbooth-setup links no
// package with a network capability: the two agree on the boundary inputs.
func TestPortRuleAgreesWithTheServices(t *testing.T) {
	for _, p := range []string{"0", "1", "80", "65535", "65536", "99999", "", "01", "00080", "+80", "-1", "8 0", " 80", "80 ", "0x50", "８０", "1e2"} {
		if got, want := portOK("example.test:"+p), btcrpc.ValidPort(p); got != want {
			t.Errorf("port %q: setup %v, service %v", p, got, want)
		}
	}
	if !portOK("example.test") || !portOK("[::1]") {
		t.Error("a host without a port is refused")
	}
}
