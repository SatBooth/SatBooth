//go:build qualify

package qualify

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/relay"
)

const (
	merchantFundingBTC = "0.1"
	channelSats        = 1_000_000
	topUpSats          = 300_000 // paid to SatBooth's hub, so it has outbound liquidity
	payoutBudgetSat    = 50_000  // the payout connection's budget, as the pairing link sets it
)

// poll calls fn every interval until it reports done or the deadline passes; a timeout fails the test with the
// last error. Waits in this suite are always conditions with a deadline, never bare sleeps.
func poll(t testing.TB, what string, deadline, interval time.Duration, fn func() (bool, error)) {
	t.Helper()
	end := time.Now().Add(deadline)
	var last error
	for {
		done, err := fn()
		if done {
			return
		}
		last = err
		if time.Now().After(end) {
			if last != nil {
				t.Fatalf("%s: not done within %s (last error: %v)", what, deadline, last)
			}
			t.Fatalf("%s: not done within %s", what, deadline)
		}
		time.Sleep(interval)
	}
}

// bringUp sets a hub up, starts it and waits for running: true. A hub with no internet must reach running: the wait is
// bounded by 3 minutes and the measured time is logged.
func bringUp(t *testing.T, name string) *hubAPI {
	t.Helper()
	h := newHubAPI(hubURL(name), "")
	poll(t, name+" /api/info", 3*time.Minute, 2*time.Second, func() (bool, error) {
		_, err := h.info()
		return err == nil, err
	})
	if err := h.setup(); err != nil {
		t.Fatalf("%s setup: %v", name, shownErr(err))
	}
	began := time.Now()
	if _, err := h.start(); err != nil {
		t.Fatalf("%s start: %v", name, shownErr(err))
	}
	var i *hubInfo
	poll(t, name+" running", 3*time.Minute, time.Second, func() (bool, error) {
		var err error
		i, err = h.info()
		return err == nil && i.Running, err
	})
	t.Logf("MEASURED hub=%s seconds_from_start_to_running=%.1f network=%s", name, time.Since(began).Seconds(), netName(i.Network))
	if i.Network != "regtest" {
		t.Fatalf("%s reports network %s, want regtest", name, netName(i.Network))
	}
	return h
}

// TestBootstrap builds the whole test world once: two running hubs, funded, one channel, SatBooth's connections
// and the relay configs. It is not re-runnable on the same volumes (setup refuses a second time).
func TestBootstrap(t *testing.T) {
	dir := stateDir(t)
	st := &state{
		Hubs:      map[string]*hubState{},
		KeyFiles:  map[string]string{},
		Wallets:   map[string]string{},
		RelayURLs: map[string]string{hubSatbooth: relayClientURL(hubSatbooth), hubMerchant: relayClientURL(hubMerchant)},
	}

	// 1. Set up and start both hubs.
	sat := bringUp(t, hubSatbooth)
	mer := bringUp(t, hubMerchant)
	for name, h := range map[string]*hubAPI{hubSatbooth: sat, hubMerchant: mer} {
		id, err := h.connectionInfo()
		if err != nil || id == "" {
			t.Fatalf("%s node id: %v", name, shownErr(err))
		}
		st.Hubs[name] = &hubState{URL: hubURL(name), Token: h.token, NodeID: id}
	}

	// 2. A bitcoind wallet and 101 blocks, so the first coinbase is spendable.
	btc := newBitcoindRPC(t)
	if err := btc.createWallet(minerWallet); err != nil {
		var re *rpcError
		if !errors.As(err, &re) || !strings.Contains(re.Message, "already exists") {
			t.Fatalf("createwallet: %v", shownErr(err))
		}
	}
	minerAddr, err := btc.getNewAddress()
	if err != nil {
		t.Fatalf("getnewaddress: %v", shownErr(err))
	}
	if err := btc.generateToAddress(101, minerAddr); err != nil {
		t.Fatalf("generatetoaddress: %v", shownErr(err))
	}
	mine := func(n int) {
		t.Helper()
		if err := btc.generateToAddress(n, minerAddr); err != nil {
			t.Fatalf("generatetoaddress: %v", shownErr(err))
		}
	}

	// 3. Fund the merchant with 0.1 BTC, mine one block, wait for its balance.
	addr, err := mer.newAddress()
	if err != nil {
		t.Fatalf("merchant new address: %v", shownErr(err))
	}
	if err := btc.sendToAddress(addr, merchantFundingBTC); err != nil {
		t.Fatalf("sendtoaddress: %v", shownErr(err))
	}
	mine(1)
	fundBegan := time.Now()
	poll(t, "merchant on-chain balance", 3*time.Minute, 2*time.Second, func() (bool, error) {
		_ = mer.walletSync()
		b, err := mer.balances()
		if err != nil {
			return false, err
		}
		return b.Onchain.SpendableSat >= 10_000_000, nil
	})
	t.Logf("MEASURED seconds_from_block_to_merchant_balance=%.1f", time.Since(fundBegan).Seconds())

	// 4. Connect to the LDK listening address (the default, not what connection-info reports), open the
	// channel from the merchant to SatBooth and mine one block at a time until it is active.
	// 9735 is LDK_LISTENING_ADDRESSES' default, [::]:9735 (getAlby/hub 921003b config/models.go:42).
	satHost, satPort := hubIP[hubSatbooth], 9735
	poll(t, "connect peer", time.Minute, 2*time.Second, func() (bool, error) {
		err := mer.connectPeer(st.Hubs[hubSatbooth].NodeID, satHost, satPort)
		return err == nil, err
	})
	if _, err := mer.openChannel(st.Hubs[hubSatbooth].NodeID, channelSats); err != nil {
		t.Fatalf("open channel: %v", shownErr(err))
	}
	blocks, ch := 0, channel{}
	for blocks < 30 && !ch.Active {
		mine(1)
		blocks++
		end := time.Now().Add(20 * time.Second)
		for {
			chs, err := mer.channels()
			if err == nil && len(chs) > 0 {
				ch = chs[0]
			}
			if ch.Active || (ch.Confirmations != nil && int(*ch.Confirmations) >= blocks) || time.Now().After(end) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		var conf, req any = "-", "-"
		if ch.Confirmations != nil {
			conf = *ch.Confirmations
		}
		if ch.ConfirmationsRequired != nil {
			req = *ch.ConfirmationsRequired
		}
		t.Logf("after block %d: confirmations=%v required=%v active=%v status=%s", blocks, conf, req, ch.Active, shown(ch.Status, "online", "offline", "opening", "pending", "Online", "Offline", "Opening", "Pending"))
		if !ch.Active && ch.Confirmations != nil && ch.ConfirmationsRequired != nil && *ch.Confirmations >= *ch.ConfirmationsRequired {
			// Deep enough: the channel becomes active without more blocks, once both sides exchange channel_ready.
			poll(t, "channel active after the required confirmations", time.Minute, time.Second, func() (bool, error) {
				chs, err := mer.channels()
				if err == nil && len(chs) > 0 {
					ch = chs[0]
				}
				return ch.Active, err
			})
		}
	}
	if !ch.Active {
		t.Fatal("the channel is not active after 30 blocks")
	}
	t.Logf("MEASURED blocks_until_channel_active=%d confirmations_required=%v", blocks, derefU32(ch.ConfirmationsRequired))
	st.ChannelID = ch.ID
	poll(t, "SatBooth's hub sees the channel active", 2*time.Minute, time.Second, func() (bool, error) {
		chs, err := sat.channels()
		return err == nil && len(chs) > 0 && chs[0].Active, err
	})

	// 5. The merchant pays SatBooth's hub 300,000 sat so SatBooth has outbound liquidity.
	inv, err := sat.makeInvoice(topUpSats, "top-up")
	if err != nil {
		t.Fatalf("top-up invoice: %v", shownErr(err))
	}
	began := time.Now()
	paid, err := mer.payInvoice(inv.Invoice)
	if err != nil {
		t.Fatalf("top-up payment: %v", shownErr(err))
	}
	t.Logf("MEASURED top_up_payment_seconds=%.2f state=%s", time.Since(began).Seconds(), shownState(paid.State))
	poll(t, "SatBooth outbound liquidity", time.Minute, time.Second, func() (bool, error) {
		b, err := sat.balances()
		return err == nil && b.Lightning.TotalSpendableMsat >= 250_000_000, err
	})

	// 6. Client keys, 0600 files under /state/keys.
	keyDir := filepath.Join(dir, "keys")
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keys := map[string]*nostr.SecretKey{}
	for _, n := range keyNames {
		k, err := nostr.GenerateSecretKey()
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(keyDir, n+".key")
		if err := nostr.WriteNewSecretKeyFile(p, k); err != nil {
			t.Fatalf("key %s: %v", n, shownErr(err))
		}
		keys[n], st.KeyFiles[n] = k, p
	}

	// 7. The apps (getAlby/hub 921003b api/models.go:152-165, 294-304), with the scopes and budget the pairing links set
	// (cmd/satbooth-pair).
	mk := func(h *hubAPI, name, keyName string, scopes []string, budget uint64) {
		t.Helper()
		w, _, err := h.createApp(name, keys[keyName].PublicKeyHex(), scopes, budget, "never")
		if err != nil {
			t.Fatalf("create app %s: %v", name, shownErr(err))
		}
		if err := nostr.ValidatePubKey(w); err != nil {
			t.Fatalf("app %s: hub returned a bad wallet key", name)
		}
		st.Wallets[name] = w
	}
	mk(sat, "invoice", "invoice", []string{"make_invoice", "lookup_invoice", "list_transactions", "get_info", "notifications"}, 0)
	// Exactly pay_invoice, lookup_invoice and get_info (the SatBooth links' list): the service's start-up check refuses
	// any other method, and a connection without get_info states no network, which it refuses too.
	mk(sat, "payout", "payout", []string{"pay_invoice", "lookup_invoice", "get_info"}, payoutBudgetSat)
	// The wallet key that the stranger is paired with: a live app whose client key is another one (decoy), so
	// the hub's lookup by the request's author finds nothing for the stranger.
	mk(sat, "decoy", "decoy", []string{"lookup_invoice"}, 0)
	mk(sat, "info", "info", []string{"get_info", "lookup_invoice"}, 0)
	mk(mer, "merchant", "merchant", []string{"make_invoice", "lookup_invoice", "notifications"}, 0)
	st.Wallets["stranger"] = st.Wallets["decoy"]

	// 8. The state and the relay configs.
	pub := func(n string) string { return keys[n].PublicKeyHex() }
	err = writeRelayConfigs(dir, map[string][]relay.Pair{
		hubSatbooth: {
			{Wallet: st.Wallets["invoice"], Client: pub("invoice")},
			{Wallet: st.Wallets["payout"], Client: pub("payout")},
			{Wallet: st.Wallets["decoy"], Client: pub("stranger")},
			{Wallet: st.Wallets["info"], Client: pub("info")},
		},
		hubMerchant: {{Wallet: st.Wallets["merchant"], Client: pub("merchant")}},
	})
	if err != nil {
		t.Fatalf("relay configs: %v", shownErr(err))
	}
	for name, h := range map[string]*hubAPI{hubSatbooth: sat, hubMerchant: mer} {
		st.Hubs[name].Token = h.token
	}
	if err := st.save(dir); err != nil {
		t.Fatalf("state: %v", shownErr(err))
	}
}

func derefU32(p *uint32) any {
	if p == nil {
		return "-"
	}
	return *p
}
