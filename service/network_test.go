package service

import (
	"database/sql"
	stdlog "log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/store"
)

// withNetwork returns a copy of cfg under another Lightning network: the three network keys stay consistent, as
// lightningNetwork cross-checks them.
func withNetwork(cfg *Config, network string) *Config {
	c := *cfg
	c.NWC.Network = network
	if c.Onchain != nil {
		o := *c.Onchain
		o.Network = network
		c.Onchain = &o
	}
	if c.Payout != nil {
		p := *c.Payout
		p.Network = network
		c.Payout = &p
	}
	return &c
}

// The store's network names are the ones nwc.network accepts.
func TestStoreNetworksAreTheConfigurationsNetworks(t *testing.T) {
	ctx := t.Context()
	for _, n := range []string{"mainnet", "testnet", "signet", "regtest"} {
		if _, err := approval.ParseNetwork(n); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		st, err := store.Open(ctx, filepath.Join(t.TempDir(), "store.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.BindNetwork(ctx, n); err != nil {
			t.Errorf("the store refuses %s: %v", n, err)
		}
		st.Close()
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := approval.ParseNetwork("testnet4"); err == nil || st.BindNetwork(ctx, "testnet4") == nil {
		t.Fatal("an unknown network accepted")
	}
}

// A start records the network; a start of the same data directory under another network is refused with the stored
// network named, before the pair stamp advances and before anything starts; the same network starts again.
func TestRunRefusesAnotherNetwork(t *testing.T) {
	r := startRig(t)
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	dataDir := r.cfg.DataDir
	checkCompletedStart(t, dataDir, 1)
	for _, other := range []string{"mainnet", "testnet", "signet"} {
		err, logs, infos := runAgain(t, withNetwork(r.cfg, other))
		if err == nil || err.Error() != "service: this data directory belongs to regtest" {
			t.Fatalf("%s: err %v\n%s", other, err, logs)
		}
		if infos != 0 || strings.Contains(logs, logStarted) || !strings.Contains(logs, logStartFailed) ||
			!strings.Contains(logs, `"reason":"network"`) {
			t.Fatalf("%s: infos %d\n%s", other, infos, logs)
		}
		checkCompletedStart(t, dataDir, 1) // the pair stamp did not advance
	}
	if err, logs, _ := runAgain(t, r.cfg); err != nil {
		t.Fatalf("the same network: %v\n%s", err, logs)
	}
	checkCompletedStart(t, dataDir, 2)
}

// recordedNetwork reads the network a stopped service's data directory holds ("" for none).
func recordedNetwork(t *testing.T, dataDir string) string {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(dataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n, err := st.Network(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A start that is refused records nothing: here the on-chain check finds bitcoind on another chain than the
// configuration's, on a fresh data directory; the corrected start then records the right network and not the wrong one.
func TestRefusedFirstStartRecordsNoNetwork(t *testing.T) {
	node := testNode(t)
	r := startRigWith(t, onchainMod("http://127.0.0.1:18443"), WithRPC(node))
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	dataDir := r.cfg.DataDir
	if n := recordedNetwork(t, dataDir); n != "regtest" {
		t.Fatalf("the first start recorded %q", n)
	}
	// A fresh directory: both databases gone together, so the pair check sees a new pair.
	for _, db := range []string{"store.db", "ledger.db"} {
		removeDB(t, dataDir, db)
	}
	// The configuration says signet (all three keys); bitcoind is on regtest.
	wrong := withNetwork(r.cfg, "signet")
	fw := newFakeWallet()
	fw.network = "signet" // the hub agrees with the configuration; bitcoind does not
	_, logs := startRefuses(t, wrong, "onchain", "the on-chain startup check failed", WithWallet(fw), WithRPC(node))
	if n := recordedNetwork(t, dataDir); n != "" {
		t.Fatalf("a refused start recorded %q\n%s", n, logs)
	}
	if err, logs, _ := runAgain(t, r.cfg, WithRPC(node)); err != nil {
		t.Fatalf("the corrected start: %v\n%s", err, logs)
	}
	if n := recordedNetwork(t, dataDir); n != "regtest" {
		t.Fatalf("the corrected start recorded %q", n)
	}
}

// Another refusal of a fresh directory (the hub's get_info fails) records nothing either.
func TestStartRefusedByTheHubRecordsNoNetwork(t *testing.T) {
	cfg := payoutFiles(t, false, nil)
	fw := newFakeWallet()
	fw.infoErr = &nwc.WalletError{Code: "RESTRICTED", Message: "x"}
	startRefuses(t, cfg, "invoice_connection", "cannot read get_info", WithWallet(fw))
	if n := recordedNetwork(t, cfg.DataDir); n != "" {
		t.Fatalf("a refused start recorded %q", n)
	}
	if err, logs, _ := runAgain(t, cfg); err != nil {
		t.Fatalf("the start: %v\n%s", err, logs)
	}
	if n := recordedNetwork(t, cfg.DataDir); n != cfg.NWC.Network {
		t.Fatalf("recorded %q", n)
	}
}

// forgetNetwork takes a stopped service's store back to a version-9 store: the network table dropped (migration 10
// undone, as store's dropMigrationTen does), so the next start records the network again. Its invoices stay.
func forgetNetwork(t *testing.T, dataDir string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{`DROP TABLE webhook_abandoned`, `DROP INDEX invoices_created`, `DROP TABLE store_network`, `UPDATE store_schema_version SET version = 9`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// A store that holds invoices and no network record (made before migration 10) is recorded only under the network
// of its newest invoice: another configured network is refused at the early comparison, nothing is recorded, and the
// right network then starts and is recorded.
func TestRunRefusesToRecordAnotherNetworkThanTheInvoices(t *testing.T) {
	r := startRig(t)
	storeInvoice(t, r, "held-1")
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	dataDir := r.cfg.DataDir
	forgetNetwork(t, dataDir)
	for _, other := range []string{"mainnet", "testnet", "signet"} {
		err, logs, infos := runAgain(t, withNetwork(r.cfg, other))
		if err == nil || err.Error() != "service: this data directory holds invoices of another network" {
			t.Fatalf("%s: err %v\n%s", other, err, logs)
		}
		if infos != 0 || strings.Contains(logs, logStarted) || !strings.Contains(logs, `"reason":"network"`) {
			t.Fatalf("%s: infos %d\n%s", other, infos, logs)
		}
		if n := recordedNetwork(t, dataDir); n != "" {
			t.Fatalf("%s: a refused start recorded %q", other, n)
		}
	}
	if err, logs, _ := runAgain(t, r.cfg); err != nil {
		t.Fatalf("the right network: %v\n%s", err, logs)
	}
	if n := recordedNetwork(t, dataDir); n != "regtest" {
		t.Fatalf("recorded %q", n)
	}
}

// The same store under the network of its invoices is recorded as before, and a store with no invoice is bound to any
// configured network (TestRefusedFirstStartRecordsNoNetwork and the first starts above).
func TestRunRecordsTheNetworkOfTheInvoices(t *testing.T) {
	r := startRig(t)
	storeInvoice(t, r, "held-2")
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	forgetNetwork(t, r.cfg.DataDir)
	if n := recordedNetwork(t, r.cfg.DataDir); n != "" {
		t.Fatalf("recorded %q before the start", n)
	}
	if err, logs, _ := runAgain(t, r.cfg); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if n := recordedNetwork(t, r.cfg.DataDir); n != "regtest" {
		t.Fatalf("recorded %q", n)
	}
}

// storeInvoice creates an order and presses pay, so the store holds one Lightning invoice.
func storeInvoice(t *testing.T, r *rig, ref string) {
	t.Helper()
	o := r.createOrder(ref)
	if code, _ := do(t, "POST", o.CheckoutURL+"/pay", "", nil); code != 303 {
		t.Fatalf("pay: %d", code)
	}
	if n := len(r.wallet.invoices()); n != 2 {
		t.Fatalf("the wallet made %d invoice fields, want 2", n)
	}
}

// An uppercase copy of the newest invoice (a text the payment path's check cannot read, as an earlier build could
// store) states no network: it does not refuse, and the start records the configured network.
func TestRunRecordsTheNetworkWhenTheNewestInvoiceIsUnreadable(t *testing.T) {
	r := startRig(t)
	storeInvoice(t, r, "held-3")
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	dataDir := r.cfg.DataDir
	forgetNetwork(t, dataDir)
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	// The newest row: the same invoice upper-cased, under another payment hash, one hour later.
	_, err = db.Exec(`INSERT INTO invoices (order_id, payment_hash, bolt11, amount_msat, created_at, expires_at)
		SELECT order_id, ?, upper(bolt11), amount_msat, created_at + 3600, expires_at + 7200 FROM invoices LIMIT 1`,
		strings.Repeat("f", 64))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"mainnet", "regtest"} {
		cfg := withNetwork(r.cfg, other)
		if err, logs, _ := runAgain(t, cfg); err != nil {
			t.Fatalf("%s: %v\n%s", other, err, logs)
		}
		if n := recordedNetwork(t, dataDir); n != other {
			t.Fatalf("%s: recorded %q", other, n)
		}
		forgetNetwork(t, dataDir)
	}
}

// The standard logger is routed through the fixed writer: what the HTTP library writes there (a client logs the first
// bytes a receiver sent unasked) becomes one fixed line, and the bytes appear nowhere.
func TestRunRoutesTheStandardLoggerThroughTheFixedWriter(t *testing.T) {
	oldOut, oldFlags := stdlog.Writer(), stdlog.Flags()
	t.Cleanup(func() { stdlog.SetOutput(oldOut); stdlog.SetFlags(oldFlags) })
	r := startRig(t)
	stdlog.Printf("Unsolicited response received on idle HTTP channel starting with %q", "TOKEN-LEAK-marker")
	logs := r.logs.String()
	if !strings.Contains(logs, logHTTPError) || strings.Contains(logs, "TOKEN-LEAK-marker") || strings.Contains(logs, "Unsolicited") {
		t.Fatalf("the standard logger is not routed:\n%s", logs)
	}
	if stdlog.Flags() != 0 {
		t.Fatalf("flags %d", stdlog.Flags())
	}
}
