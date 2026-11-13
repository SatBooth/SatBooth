package setup

import (
	"bytes"
	"encoding/json"
	"flag"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/api"
	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/relay"
	"github.com/satbooth/satbooth/service"
	"github.com/satbooth/satbooth/webhook"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

// rep is n repetitions of one hex digit: a deterministic fake secret that looks like no real one.
func rep(d string) string { return strings.Repeat(d, 64) }

// fixture is one case: the answers of both hosts, the handoffs, and every generated secret (fake, deterministic).
type fixture struct {
	name    string
	pub     PublicAnswers
	hub     HubAnswers
	in      *PublicToHub
	out     *HubToPublic
	secrets map[string]string
	files   map[string][]byte
}

// fixedReader gives RPCAuthLine a deterministic salt.
func fixedReader(b byte) *bytes.Reader { return bytes.NewReader(bytes.Repeat([]byte{b}, 16)) }

func newFixture(t *testing.T, name string, pub PublicAnswers, in *PublicToHub, out *HubToPublic, hub HubAnswers) *fixture {
	t.Helper()
	f := &fixture{name: name, pub: pub, hub: hub, in: in, out: out, files: map[string][]byte{}, secrets: map[string]string{
		"hub": rep("1"), "fulcrum": rep("2"), "satbooth": rep("3"), "webhook": rep("4"), "alerts": rep("5"), "wallet": rep("6"),
		"orders-write-key": rep("7"), "orders-read-key": rep("8"), "refunds-write-key": rep("9"), "refunds-read-key": rep("a"),
	}}
	hubAuth, err := RPCAuthLine(fixedReader(0x11), "hub", f.secrets["hub"])
	if err != nil {
		t.Fatal(err)
	}
	fulAuth, err := RPCAuthLine(fixedReader(0x22), "fulcrum", f.secrets["fulcrum"])
	if err != nil {
		t.Fatal(err)
	}
	if in.SatboothRPCAuth != "" {
		in.SatboothRPCAuth, err = RPCAuthLine(fixedReader(0x33), "satbooth", f.secrets["satbooth"])
		if err != nil {
			t.Fatal(err)
		}
	}
	ids := []string{"orders-write", "orders-read"}
	caps := []string{api.CapWrite, api.CapRead}
	keys := []string{f.secrets["orders-write-key"], f.secrets["orders-read-key"]}
	if len(pub.ApprovalKeys) > 0 {
		ids = append(ids, "refunds-write", "refunds-read")
		caps = append(caps, api.CapRefundsWrite, api.CapRefundsRead)
		keys = append(keys, f.secrets["refunds-write-key"], f.secrets["refunds-read-key"])
	}
	apiKeys, err := APIKeysJSON(ids, caps, keys)
	if err != nil {
		t.Fatal(err)
	}
	f.files["relay.json"] = RelayJSON(&hub, in, out)
	f.files["bitcoin.conf"] = BitcoinConf(&hub, in, hubAuth, fulAuth)
	f.files["hub.env"] = HubEnv(f.secrets["hub"])
	f.files["fulcrum.conf"] = FulcrumConf(f.secrets["fulcrum"])
	f.files["monerod.conf"] = MonerodConf()
	f.files["rpc-login"] = WalletRPCLogin("satbooth", f.secrets["wallet"])
	f.files["hub-compose.env"] = HubComposeEnv(&hub)
	f.files["public-compose.env"] = PublicComposeEnv(&pub)
	f.files["api-keys.json"] = apiKeys
	f.files["satbooth.json"] = SatboothJSON(&pub, out)
	return f
}

// hubFor is the hub host's answers for a public host's: the same network and addresses, typed again.
func hubFor(pub PublicAnswers) HubAnswers {
	h := baseHub()
	h.Network, h.HubPrivateAddr, h.PublicPrivateAddr = pub.Network, pub.HubPrivateAddr, pub.PublicPrivateAddr
	return h
}

func fxLightning(t *testing.T) *fixture {
	pub := baseAnswers()
	pub.Network = "regtest"
	pub.HubPrivateAddr, pub.PublicPrivateAddr = "192.0.2.20", "192.0.2.10"
	in := &PublicToHub{Version: 1, Network: "regtest", HubPrivateAddr: "192.0.2.20", PublicPrivateAddr: "192.0.2.10", InvoiceClient: curveKeys[0]}
	return newFixture(t, "lightning-only", pub, in, fxHubToPublic(false), hubFor(pub))
}

func fxEveryRail(t *testing.T) *fixture {
	pub := baseAnswers()
	pub.Network = "mainnet"
	pub.HubPrivateAddr, pub.PublicPrivateAddr = "192.0.2.20", "192.0.2.10"
	pub.AlertsURL = "https://monitor.example.com/satbooth-alert"
	pub.Descriptor = "wpkh(" + xpubV1 + "/0/*)"
	pub.MoneroAddress, pub.MoneroNetwork = xmrMain, "mainnet"
	withRefunds(&pub, 2, 100000)
	pub.Currencies = []string{"USD", "EUR"}
	in := &PublicToHub{Version: 1, Network: "mainnet", HubPrivateAddr: "192.0.2.20", PublicPrivateAddr: "192.0.2.10",
		InvoiceClient: curveKeys[0], PayoutClient: curveKeys[1], SatboothRPCAuth: "rpcauth=satbooth:placeholder", Monero: true, MoneroRPCUser: "satbooth"}
	return newFixture(t, "every-rail", pub, in, fxHubToPublic(true), hubFor(pub))
}

// check parses the answers and handoffs the way the stages do, so a fixture the parsers refuse fails here.
func (f *fixture) check(t *testing.T) {
	t.Helper()
	if _, err := ParsePublicAnswers(mustJSON(f.pub)); err != nil {
		t.Fatalf("%s: public answers: %v", f.name, err)
	}
	if _, err := ParseHubAnswers(mustJSON(f.hub)); err != nil {
		t.Fatalf("%s: hub answers: %v", f.name, err)
	}
	if _, err := ParsePublicToHub(f.in.Marshal()); err != nil {
		t.Fatalf("%s: handoff: %v", f.name, err)
	}
	if _, err := ParseHubToPublic(f.out.Marshal()); err != nil {
		t.Fatalf("%s: handoff back: %v", f.name, err)
	}
	if err := f.hub.CheckAgainst(f.in); err != nil {
		t.Fatalf("%s: %v", f.name, err)
	}
}

func (f *fixture) golden(t *testing.T) {
	t.Helper()
	dir := filepath.Join("testdata", f.name)
	for name, got := range f.files {
		p := filepath.Join(dir, name)
		if *update {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, got, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v (run go test ./setup/ -update)", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s/%s differs from its golden file:\n--- got\n%s\n--- want\n%s", f.name, name, got, want)
		}
	}
}

// writeFile writes b at dir/name with mode 0600, whatever the umask.
func writeFile(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// loaders feeds the files to the programs that read them.
func (f *fixture) loaders(t *testing.T, pairs int) {
	t.Helper()
	cfg, err := service.ParseConfig(f.files["satbooth.json"])
	if err != nil {
		t.Fatalf("%s: service.ParseConfig: %v", f.name, err)
	}
	if (cfg.Onchain != nil) != (f.pub.Descriptor != "") || (cfg.Monero != nil) != (f.pub.MoneroAddress != "") ||
		(cfg.Payout != nil) != (len(f.pub.ApprovalKeys) > 0) || (cfg.Rates != nil) != (len(f.pub.Currencies) > 0) ||
		(cfg.Alerts != nil) != (f.pub.AlertsURL != "") {
		t.Errorf("%s: the blocks present do not follow the answers", f.name)
	}
	if cfg.Onchain != nil && cfg.Onchain.Descriptor != f.pub.Descriptor {
		t.Errorf("%s: the descriptor was not written as given", f.name)
	}
	rc, err := relay.ParseConfig(f.files["relay.json"])
	if err != nil || len(rc.Pairs) != pairs {
		t.Fatalf("%s: relay.ParseConfig: %v (%v pairs)", f.name, err, rc)
	}
	if rc.ClientRelayURL != cfg.NWC.RelayURL {
		t.Errorf("%s: the relay's public URL and SatBooth's differ", f.name)
	}
	dir := t.TempDir()
	kp := writeFile(t, dir, "api-keys.json", f.files["api-keys.json"])
	keys, err := api.LoadKeys(kp)
	if err != nil {
		t.Fatalf("%s: api.LoadKeys: %v", f.name, err)
	}
	all := []string{api.CapWrite, api.CapRead, api.CapRefundsWrite, api.CapRefundsRead}
	owner := map[string]string{"orders-write-key": api.CapWrite, "orders-read-key": api.CapRead,
		"refunds-write-key": api.CapRefundsWrite, "refunds-read-key": api.CapRefundsRead}
	for name, capab := range owner {
		_, present := f.secrets[name]
		wantKey := capab == api.CapWrite || capab == api.CapRead || len(f.pub.ApprovalKeys) > 0
		if !present {
			continue
		}
		for _, c := range all {
			if got, want := keys.Check(f.secrets[name], c), wantKey && c == capab; got != want {
				t.Errorf("%s: key %s under %s: got %v, want %v", f.name, name, c, got, want)
			}
		}
	}
	// A secret written with its final newline, as the setup command writes it.
	sp := writeFile(t, dir, "webhook.secret", []byte(f.secrets["webhook"]+"\n"))
	if s, err := webhook.LoadSecret(sp); err != nil || len(s) != 32 {
		t.Errorf("%s: webhook.LoadSecret: %v", f.name, err)
	}
	pp := writeFile(t, dir, "bitcoind.pass", []byte(f.secrets["satbooth"]+"\n"))
	if pw, err := btcrpc.LoadPassword(pp); err != nil || string(pw) != f.secrets["satbooth"] {
		t.Errorf("%s: btcrpc.LoadPassword: %v", f.name, err)
	}
}

func TestRenderLightningOnly(t *testing.T) {
	f := fxLightning(t)
	f.check(t)
	f.golden(t)
	f.loaders(t, 1)
	for _, c := range []string{"rpcauth=satbooth", "rpcwhitelist=satbooth", "/32"} {
		if strings.Contains(string(f.files["bitcoin.conf"]), c) {
			t.Errorf("bitcoin.conf of a Lightning-only host holds %q", c)
		}
	}
}

func TestRenderEveryRail(t *testing.T) {
	f := fxEveryRail(t)
	f.check(t)
	f.golden(t)
	f.loaders(t, 2)
	conf := string(f.files["bitcoin.conf"])
	for _, c := range []string{"chain=main\n", "[main]\n", "rpcallowip=192.0.2.10/32\n", "rpcallowip=172.30.78.0/25\n", "rpcallowip=172.30.78.128/25\n",
		"rpcauth=hub:", "rpcauth=fulcrum:", "rpcauth=satbooth:", "rpcwhitelistdefault=0\n"} {
		if !strings.Contains(conf, c) {
			t.Errorf("bitcoin.conf lacks %q", c)
		}
	}
	if !strings.Contains(string(f.files["hub-compose.env"]), "HUB_NETWORK=bitcoin\n") {
		t.Error("mainnet is HUB_NETWORK=bitcoin")
	}
}

func TestURLBuilders(t *testing.T) {
	if got := RelayURL("192.0.2.20"); got != "ws://192.0.2.20:7448" {
		t.Fatalf("RelayURL: %s", got)
	}
	if got := BitcoindURL("192.0.2.20"); got != "http://192.0.2.20:8332" {
		t.Fatalf("BitcoindURL: %s", got)
	}
	if got := WalletRPCURL("192.0.2.20"); got != "http://192.0.2.20:18083" {
		t.Fatalf("WalletRPCURL: %s", got)
	}
}

// A handoff may not decide the network or an address: the hub-side renderers use the answers' values only, whatever
// the handoff says (CheckAgainst is bypassed here).
func TestHubRenderersIgnoreHandoffNetworkAndAddresses(t *testing.T) {
	f := fxEveryRail(t)
	evil := *f.in
	evil.Network, evil.HubPrivateAddr, evil.PublicPrivateAddr = "regtest", "198.51.100.1", "198.51.100.2"
	hubAuth, fulAuth := "rpcauth=hub:x", "rpcauth=fulcrum:y"
	outs := map[string]string{
		"relay.json":      string(RelayJSON(&f.hub, &evil, f.out)),
		"bitcoin.conf":    string(BitcoinConf(&f.hub, &evil, hubAuth, fulAuth)),
		"hub-compose.env": string(HubComposeEnv(&f.hub)),
	}
	for name, got := range outs {
		for _, bad := range []string{"198.51.100", "regtest"} {
			if strings.Contains(got, bad) {
				t.Errorf("%s carries the handoff's value %q", name, bad)
			}
		}
	}
	for name, want := range map[string]string{"relay.json": "ws://192.0.2.20:7448", "bitcoin.conf": "chain=main\n", "hub-compose.env": "HUB_PRIVATE_ADDR=192.0.2.20\n"} {
		if !strings.Contains(outs[name], want) {
			t.Errorf("%s lacks %q", name, want)
		}
	}
	if !strings.Contains(outs["bitcoin.conf"], "rpcallowip=192.0.2.10/32\n") || !strings.Contains(outs["hub-compose.env"], "HUB_NETWORK=bitcoin\n") {
		t.Error("bitcoin.conf or the env file does not follow the answers")
	}
	if !strings.Contains(outs["bitcoin.conf"], evil.SatboothRPCAuth) {
		t.Error("the handoff's rpcauth line is missing")
	}
}

func TestWhitelistsMatchTheBundle(t *testing.T) {
	bundle, err := os.ReadFile("../deploy/hub/config-example/bitcoin.conf")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(bundle), "\n") {
		if strings.HasPrefix(l, "rpcwhitelist=") && !strings.HasPrefix(l, "rpcwhitelistdefault") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 3 {
		t.Fatalf("the bundle has %d rpcwhitelist lines", len(lines))
	}
	conf := strings.Split(string(fxEveryRail(t).files["bitcoin.conf"]), "\n")
	for _, l := range lines {
		found := false
		for _, c := range conf {
			found = found || c == l
		}
		if !found {
			t.Errorf("a whitelist line of the bundle is not in the rendered file byte for byte: %.40s...", l)
		}
	}
}

func TestMonerodConfIsTheBundlesWithoutItsHeader(t *testing.T) {
	bundle, err := os.ReadFile("../deploy/hub/config-example/monerod.conf")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(bundle), "\n\n")
	if !ok || string(MonerodConf()) != rest {
		t.Fatalf("MonerodConf differs from the bundle's file after its header:\n%s", MonerodConf())
	}
}

// The fulcrum.conf and the environment files follow the bundle's example files: the same keys in the same order.
func TestEnvAndFulcrumFollowTheExamples(t *testing.T) {
	keysOf := func(b []byte, sep string) []string {
		var out []string
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				k, _, _ := strings.Cut(l, sep)
				out = append(out, strings.TrimSpace(k))
			}
		}
		return out
	}
	f := fxEveryRail(t)
	for _, c := range []struct {
		example, sep string
		got          []byte
	}{{"../deploy/hub/example.env", "=", f.files["hub-compose.env"]}, {"../deploy/public/example.env", "=", f.files["public-compose.env"]},
		{"../deploy/hub/config-example/fulcrum.conf", "=", f.files["fulcrum.conf"]}} {
		want, err := os.ReadFile(c.example)
		if err != nil {
			t.Fatal(err)
		}
		if g, w := keysOf(c.got, c.sep), keysOf(want, c.sep); !reflect.DeepEqual(g, w) {
			t.Errorf("%s: keys %v, example has %v", c.example, g, w)
		}
	}
	// The fixed values of the public example: the subnet and address of SatBooth on its bundle network.
	for _, l := range []string{"SATBOOTH_SUBNET=172.30.78.0/24\n", "SATBOOTH_IP=172.30.78.40\n", "PUBLIC_PORT=8080\n"} {
		if !strings.Contains(string(f.files["public-compose.env"]), l) {
			t.Errorf("public env lacks %q", l)
		}
	}
	if string(f.files["hub.env"]) != "LDK_BITCOIND_RPC_PASSWORD="+rep("1")+"\n" || string(f.files["rpc-login"]) != "satbooth:"+rep("6")+"\n" {
		t.Error("hub.env or rpc-login")
	}
}

func TestDefaultsMatchTheExample(t *testing.T) {
	got, err := service.ParseConfig(fxEveryRail(t).files["satbooth.json"])
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../cmd/satbooth/testdata/config-example.json")
	if err != nil {
		t.Fatal(err)
	}
	want, err := service.ParseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	eq := func(name string, g, w any) {
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s: got %v, the example has %v", name, g, w)
		}
	}
	eq("limits", got.Limits, want.Limits)
	// The setup command writes the one endpoint main, with the answer's URL and the fixed secret file.
	eq("webhooks", got.Webhooks, []service.WebhookEndpoint{{Name: "main", URL: "https://merchant.example.com/hooks/satbooth", SecretFile: "/etc/satbooth/webhook.secret"}})
	eq("invoice_expiry_minutes", got.InvoiceExpiryMinutes, want.InvoiceExpiryMinutes)
	eq("allow_swap", got.AllowSwap, want.AllowSwap)
	eq("nwc.timeout_seconds", got.NWC.TimeoutSeconds, want.NWC.TimeoutSeconds)
	eq("onchain tiers", got.Onchain.Confirmations, want.Onchain.Confirmations)
	eq("onchain.gap_alert", got.Onchain.GapAlert, want.Onchain.GapAlert)
	eq("onchain poll", []any{got.Onchain.PollSeconds, got.Onchain.PollRate, got.Onchain.TimeoutSeconds},
		[]any{want.Onchain.PollSeconds, want.Onchain.PollRate, want.Onchain.TimeoutSeconds})
	eq("monero tiers", got.Monero.Confirmations, want.Monero.Confirmations)
	eq("monero", []any{got.Monero.UnusedAlert, got.Monero.PollSeconds, got.Monero.PollRate, got.Monero.TimeoutSeconds, got.Monero.AccountIndex},
		[]any{want.Monero.UnusedAlert, want.Monero.PollSeconds, want.Monero.PollRate, want.Monero.TimeoutSeconds, want.Monero.AccountIndex})
	// The refund caps are not in this list: with refunds on the operator gives every one (TestRenderedPayoutCaps).
	eq("payout.timeout_seconds", got.Payout.TimeoutSeconds, want.Payout.TimeoutSeconds)
	// The one value that is not the example's: margin_percent. The example says "1"; SatBooth writes the documented
	// default "0", because a margin is the merchant's pricing decision.
	eq("rates.margin_percent (the default, not the example's)", got.Rates.MarginPercent, "0")
	if want.Rates.MarginPercent == "0" {
		t.Error("the example's margin is now the default: drop this special case")
	}
	eq("rates providers", got.Rates.Providers, want.Rates.Providers)
	eq("rates thresholds", []any{got.Rates.MinSources, got.Rates.MaxSpreadPercent, got.Rates.MaxAgeSeconds, got.Rates.FetchSeconds},
		[]any{want.Rates.MinSources, want.Rates.MaxSpreadPercent, want.Rates.MaxAgeSeconds, want.Rates.FetchSeconds})
	if got.Rates.CustomURL != "" || got.Rates.CustomURLFile != "" {
		t.Error("the rates block has a custom URL")
	}
	// The fixed names of the example's secret files, under /etc/satbooth (the example's own directory differs).
	for _, p := range []string{got.NWC.ClientKeyFile, got.Payout.ClientKeyFile, got.APIKeysFile, got.Webhooks[0].SecretFile, got.Alerts.SecretFile,
		got.Onchain.RPCPasswordFile, got.Monero.RPCPasswordFile} {
		if !strings.HasPrefix(p, "/etc/satbooth/") {
			t.Errorf("a file outside /etc/satbooth/: %s", p)
		}
	}
	for _, name := range []string{"nwc-client.key", "payout.key", "api-keys.json", "webhook.secret", "alerts.secret", "bitcoind.pass", "wallet-rpc.pass"} {
		if !strings.Contains(string(fxEveryRail(t).files["satbooth.json"]), "/etc/satbooth/"+name) {
			t.Errorf("satbooth.json does not name %s", name)
		}
	}
}

func TestRenderedPayoutCaps(t *testing.T) {
	f := fxEveryRail(t)
	cfg, err := service.ParseConfig(f.files["satbooth.json"])
	if err != nil {
		t.Fatal(err)
	}
	p := f.pub
	if cfg.Payout.MaxMsat != int64(p.PayoutMaxSat)*1000 || cfg.Payout.DailyMsat != int64(p.PayoutDailySat)*1000 || cfg.Payout.BudgetAlertMsat != int64(p.PayoutAlertSat)*1000 {
		t.Errorf("payout caps %+v do not follow the answers %d/%d/%d sat", cfg.Payout, p.PayoutMaxSat, p.PayoutDailySat, p.PayoutAlertSat)
	}
}

func TestRenderersHoldNoSecretTheyShouldNot(t *testing.T) {
	for _, f := range []*fixture{fxLightning(t), fxEveryRail(t)} {
		mayHold := map[string][]string{ // the files that must hold a secret, and which
			"hub.env": {"hub"}, "fulcrum.conf": {"fulcrum"}, "rpc-login": {"wallet"},
		}
		for name, content := range f.files {
			for sname, s := range f.secrets {
				allowed := false
				for _, a := range mayHold[name] {
					allowed = allowed || a == sname
				}
				if !allowed && strings.Contains(string(content), s) {
					t.Errorf("%s/%s holds the secret %s", f.name, name, sname)
				}
			}
		}
		for _, name := range []string{"hub.env", "fulcrum.conf", "rpc-login"} {
			if len(mayHold[name]) == 1 && !strings.Contains(string(f.files[name]), f.secrets[mayHold[name][0]]) {
				t.Errorf("%s/%s lacks its secret", f.name, name)
			}
		}
	}
}

func TestAPIKeysJSONRefusesUnequalLists(t *testing.T) {
	for _, c := range [][3][]string{
		{nil, nil, nil},
		{{"a", "b"}, {"orders:write"}, {"k1", "k2"}},
		{{"a"}, {"orders:write", "orders:read"}, {"k1"}},
		{{"a"}, {"orders:write"}, {"k1", "k2"}},
		{{"a"}, {"orders:write"}, {""}},
	} {
		if b, err := APIKeysJSON(c[0], c[1], c[2]); err == nil || b != nil {
			t.Errorf("accepted %v", c)
		}
	}
	b, err := APIKeysJSON([]string{"a"}, []string{"orders:write"}, []string{"abc"})
	if err != nil || !strings.Contains(string(b), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad") || strings.Contains(string(b), "\"abc\"") {
		t.Fatalf("%s, %v", b, err)
	}
}

// Every renderer is pure and total over parsed inputs: the same inputs give the same bytes, every JSON file is
// followed by exactly one newline, and no file holds a carriage return or a NUL.
func TestRenderedFilesAreWellFormed(t *testing.T) {
	for _, f := range []*fixture{fxLightning(t), fxEveryRail(t)} {
		for name, b := range f.files {
			if len(b) == 0 || b[len(b)-1] != '\n' || (len(b) > 1 && b[len(b)-2] == '\n') || bytes.ContainsAny(b, "\r\x00") {
				t.Errorf("%s/%s is not newline-terminated text", f.name, name)
			}
		}
		g := map[string][]byte{}
		for name, b := range f.files {
			g[name] = append([]byte(nil), b...)
		}
		if again := fxByName(t, f.name); !reflect.DeepEqual(again.files, g) {
			t.Errorf("%s: a second rendering differs", f.name)
		}
	}
}

func fxByName(t *testing.T, name string) *fixture {
	if name == "lightning-only" {
		return fxLightning(t)
	}
	return fxEveryRail(t)
}

// The setup binary must stay free of network and database capabilities: the non-test files of this package import
// the standard library, this repository's leaf packages descriptor and approval, and nothing else.
func TestImportsAreTheAllowedLeaves(t *testing.T) {
	allowed := map[string]bool{"github.com/satbooth/satbooth/descriptor": true, "github.com/satbooth/satbooth/approval": true,
		"github.com/satbooth/satbooth/nostr": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(p, ".") && !allowed[p] { // not the standard library
				t.Errorf("%s imports %s", name, p)
			}
		}
	}
}

// satbooth.json carries nwc.network equal to the answers' network, and the configuration the service reads from it
// holds the same network in every block that has one.
func TestSatboothJSONNWCNetwork(t *testing.T) {
	for _, name := range []string{"lightning-only", "every-rail"} {
		f := fxByName(t, name)
		var v struct {
			NWC     struct{ Network string }
			Onchain *struct{ Network string }
			Payout  *struct{ Network string }
		}
		if err := json.Unmarshal(f.files["satbooth.json"], &v); err != nil {
			t.Fatal(err)
		}
		if v.NWC.Network == "" || v.NWC.Network != f.pub.Network {
			t.Errorf("%s: nwc.network %q, answers' network %q", name, v.NWC.Network, f.pub.Network)
		}
		if v.Onchain != nil && v.Onchain.Network != v.NWC.Network || v.Payout != nil && v.Payout.Network != v.NWC.Network {
			t.Errorf("%s: the blocks name different networks", name)
		}
		if _, err := service.ParseConfig(f.files["satbooth.json"]); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// SatboothJSON writes nwc.network for every network the answers can name, not only the fixtures' two.
func TestSatboothJSONNWCNetworkEveryNetwork(t *testing.T) {
	for _, n := range []string{"mainnet", "signet", "regtest"} {
		pub := baseAnswers()
		pub.Network = n
		var v struct {
			NWC struct{ Network string }
		}
		if err := json.Unmarshal(SatboothJSON(&pub, fxHubToPublic(false)), &v); err != nil {
			t.Fatal(err)
		}
		if v.NWC.Network != n {
			t.Errorf("answers' network %s: nwc.network %q", n, v.NWC.Network)
		}
	}
}
