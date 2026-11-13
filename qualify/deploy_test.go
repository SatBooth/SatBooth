//go:build qualify

package qualify

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/relay"
	"github.com/satbooth/satbooth/setup"
)

// The public host's bundle (plan 10): deploy/public/compose.yml, run unmodified by scripts/qualify.sh with the
// override dev/regtest/deploy-public.override.yml and the values of dev/regtest/deploy-public.env, from the files
// that `satbooth-setup public init` and `public finish` wrote (plan 12). It is a second
// SatBooth with its own data volume and its own configuration directory, bind-mounted from the host as an operator does.
// It joins the regtest network as a second network (the hub's relay, the webhook receiver and its private listener
// are there), and keeps the bundle's own network, where its public listener is and where its one port is published.
// These phases run in the qualify-deploy container, which is on both networks, and drive it over HTTP only. Lightning
// plus refunds on: it starts with the generated payout block but makes no refund and no payout (TestDeployPublicNoPayout),
// and its payout connection is its own, so TestImageBudget's figure, read after it, still holds.
// qualify.sh starts and stops it, and never while the satbooth container runs.
const (
	deployPubIP   = "192.0.2.40"   // SATBOOTH_IP in deploy-public.env: the bundle's network
	deployPrvIP   = "172.30.78.41" // the override's address on the regtest network
	deployPublic  = deployPubIP + ":8080"
	deployPrivate = deployPrvIP + ":8081"
	deployDirName = "deploy" // /state/deploy: public values and the merchant-side keys, copied in and out by qualify.sh
	deployHarness = "deploy-harness.json"
	// deployPayoutBudgetSat is the payout_budget_sat of the public step's answers (qualify.sh).
	deployPayoutBudgetSat = 1_000_000
)

func deployHarnessPath(t testing.TB) string { return filepath.Join(stateDir(t), deployHarness) }

// deployFile reads a file qualify.sh put into /state/deploy from the setup command's output.
func deployFile(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(stateDir(t), deployDirName, name))
	if err != nil {
		t.Fatalf("reading /state/%s/%s: %v", deployDirName, name, shownErr(err))
	}
	return b
}

// TestDeployPublicConnect is the hub's side of stage 2 and 3 for the public bundle, done by hand on the regtest hub:
// it reads the handoff public-to-hub.json that stage 1 wrote (public values only), creates the connection for the
// invoice client key stage 1 generated with the scopes of the invoice link (setup/pairlink.go), pairs it in the
// regtest relay's configuration (qualify.sh recreates the relay), and writes hub-to-public.json with the connection's
// Wallet Public Key, canonical, and its digest, for stage 4. Stage 1 has refunds on, so it made a payout client key too:
// its connection has exactly the scopes and the never-renewed budget of the payout link (setup/pairlink.go).
func TestDeployPublicConnect(t *testing.T) {
	st, err := loadState(stateDir(t))
	if err != nil {
		t.Fatalf("state (did TestBootstrap run?): %v", shownErr(err))
	}
	h, err := setup.ParsePublicToHub(deployFile(t, "public-to-hub.json"))
	if err != nil {
		t.Fatalf("the handoff of stage 1: %v", shownErr(err))
	}
	if h.Network != "regtest" || h.PayoutClient == "" || h.SatboothRPCAuth != "" || h.Monero {
		t.Fatalf("the handoff of stage 1 is not the Lightning plus refunds regtest one: network %s, payout %t, on-chain %t, monero %t",
			h.Network, h.PayoutClient != "", h.SatboothRPCAuth != "", h.Monero)
	}
	sat := newHubAPI(st.Hubs[hubSatbooth].URL, st.Hubs[hubSatbooth].Token)
	sat.ensureToken(t)
	// The invoice link: make_invoice lookup_invoice list_transactions get_info and payment_received notifications, no
	// budget.
	wallet, _, err := sat.createApp("deploy-invoice", h.InvoiceClient,
		[]string{"make_invoice", "lookup_invoice", "list_transactions", "get_info", "notifications"}, 0, "never")
	if err != nil {
		t.Fatalf("create the invoice connection: %v", shownErr(err))
	}
	if err := nostr.ValidatePubKey(wallet); err != nil {
		t.Fatal("the hub returned a bad wallet key")
	}
	// The payout link: pay_invoice, lookup_invoice and get_info, a budget (the answers' payout_budget_sat) that never
	// renews.
	payWallet, _, err := sat.createApp("deploy-payout", h.PayoutClient,
		[]string{"pay_invoice", "lookup_invoice", "get_info"}, deployPayoutBudgetSat, "never")
	if err != nil {
		t.Fatalf("create the payout connection: %v", shownErr(err))
	}
	if err := nostr.ValidatePubKey(payWallet); err != nil || payWallet == wallet {
		t.Fatal("the hub returned a bad or repeated wallet key")
	}
	cfgPath := filepath.Join(stateDir(t), "relay-"+hubSatbooth+".json")
	cur, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := relay.ParseConfig(cur)
	if err != nil {
		t.Fatalf("the relay's configuration: %v", shownErr(err))
	}
	pairs := append(slices.Clone(cfg.Pairs), relay.Pair{Wallet: wallet, Client: h.InvoiceClient},
		relay.Pair{Wallet: payWallet, Client: h.PayoutClient})
	if err := writeRelayConfigs(stateDir(t), map[string][]relay.Pair{hubSatbooth: pairs}); err != nil {
		t.Fatalf("pairing in the relay: %v", shownErr(err))
	}
	out := (&setup.HubToPublic{Version: 1, InvoiceWallet: wallet, PayoutWallet: payWallet}).Marshal()
	if _, err := setup.ParseHubToPublic(out); err != nil {
		t.Fatalf("the handoff is not one stage 4 reads: %v", shownErr(err))
	}
	dir := filepath.Join(stateDir(t), deployDirName)
	digest := strings.ReplaceAll(setup.Digest(out), " ", "")
	// Public values: 0644, so that qualify.sh (the calling user) reads them.
	if err := os.WriteFile(filepath.Join(dir, "hub-to-public.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hub-to-public.digest"), []byte(digest+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The payout connection's client public key (public), for TestDeployPublicNoPayout.
	if err := os.WriteFile(filepath.Join(dir, "payout-client"), []byte(h.PayoutClient+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Log("the invoice and payout connections are made and paired in the regtest relay; hub-to-public.json and its digest are written")
}

// TestDeployPublicNoPayout: the bundle started with the generated payout block and made no refund, so its payout
// connection's budget is unused (the connection is the hub's, not the suite's "payout" one, so TestImageBudget's figure
// is untouched).
func TestDeployPublicNoPayout(t *testing.T) {
	st, err := loadState(stateDir(t))
	if err != nil {
		t.Fatalf("state: %v", shownErr(err))
	}
	client := strings.TrimSpace(string(deployFile(t, "payout-client")))
	sat := newHubAPI(st.Hubs[hubSatbooth].URL, st.Hubs[hubSatbooth].Token)
	sat.ensureToken(t)
	used, max, err := sat.appBudgetUsage(client)
	if err != nil {
		t.Fatal(err)
	}
	if max != deployPayoutBudgetSat || used != 0 {
		t.Errorf("the bundle's payout connection: budget %d sat (want %d), used %d sat (want 0)", max, deployPayoutBudgetSat, used)
	}
}

// TestDeployPublicPrepare reads what stage 1 generated for the merchant's application, which qualify.sh copied into
// /state/deploy: the two plain API keys and the webhook secret. The bundle's own files (satbooth.json, the NWC client
// key, the keys' hashes, the webhook secret) are the setup command's, and nothing here writes one. The harness file
// holds the plaintext keys for the phases that call the API.
func TestDeployPublicPrepare(t *testing.T) {
	trim := func(name string) string {
		s := strings.TrimSpace(string(deployFile(t, name)))
		if len(s) != 64 {
			t.Fatalf("%s is not a 64-character value", name)
		}
		return s
	}
	h := &imgHarness{
		WriteKey:      trim("api-keys/orders-write.key"),
		ReadKey:       trim("api-keys/orders-read.key"),
		WebhookSecret: trim("webhook.secret"),
	}
	writeJSON0600(t, deployHarnessPath(t), h)
	t.Log("the merchant API's two keys and the webhook secret are read from the setup command's files")
}

// TestDeployPublicStart: the bundle's container serves, so every start-up check passed against the real hub (it opens
// its listeners only after them; qualify.sh checks the log lines, the published port and the hardening). The API takes
// the bundle's keys and refuses others, and a Lightning order opens its checkout page on the public listener.
func TestDeployPublicStart(t *testing.T) {
	e := newImgEnvAt(t, deployHarnessPath(t), deployPublic, deployPrivate, true)
	if code, _ := e.call("GET", "/v1/orders/"+randHex(t)[:32], e.h.ReadKey, nil); code != 404 {
		t.Errorf("an unknown order with the read key: status %d, want 404", code)
	}
	if code, _ := e.call("GET", "/v1/orders/"+randHex(t)[:32], randHex(t), nil); code != 401 {
		t.Errorf("an unknown key: status %d, want 401", code)
	}
	o := e.create(map[string]any{"merchant_ref": e.ref("deploy-start"), "lightning_msat": imageOrderMsat})
	if code, _ := e.page(tokenOf(o)); code != 200 {
		t.Errorf("checkout page: status %d", code)
	}
	t.Log("the bundle's container serves the API on the regtest network and the checkout page on the bundle's network")
}

// TestDeployPublicPay: one Lightning order, paid end to end: the customer pays from the merchant's hub, the order reads
// paid, the order.paid webhook arrives signed, and the page says Paid.
func TestDeployPublicPay(t *testing.T) {
	e := newImgEnvAt(t, deployHarnessPath(t), deployPublic, deployPrivate, true)
	e.h.PaidOrder = e.payLN("deploy-ln")
	e.save()
}

// TestDeployPublicAfterBackup runs after the backup d1 (qualify.sh), on a start of the same data: one more order, which
// the backup does not hold, so the restore must remove it.
func TestDeployPublicAfterBackup(t *testing.T) {
	e := newImgEnvAt(t, deployHarnessPath(t), deployPublic, deployPrivate, true)
	e.h.AfterBackupOrder = e.create(map[string]any{"merchant_ref": e.ref("deploy-after-backup"), "lightning_msat": imageOrderMsat}).ID
	e.save()
}

// TestDeployPublicRestored runs after deploy/README.md's "Restore" of d1 and a start: the paid order of
// TestDeployPublicPay still reads paid, the order made after the backup is unknown, and a new order is taken.
func TestDeployPublicRestored(t *testing.T) {
	e := newImgEnvAt(t, deployHarnessPath(t), deployPublic, deployPrivate, true)
	if e.h.PaidOrder == "" || e.h.AfterBackupOrder == "" {
		t.Fatal("no orders recorded (did TestDeployPublicPay and TestDeployPublicAfterBackup run?)")
	}
	if got := e.order(e.h.PaidOrder).State; got != "paid" {
		t.Errorf("the paid order reads %s after the restore, want paid", got)
	}
	if code, _ := e.call("GET", "/v1/orders/"+e.h.AfterBackupOrder, e.h.ReadKey, nil); code != 404 {
		t.Errorf("the order made after the backup: status %d after the restore, want 404", code)
	}
	e.create(map[string]any{"merchant_ref": e.ref("deploy-after-restore"), "lightning_msat": imageOrderMsat})
}
