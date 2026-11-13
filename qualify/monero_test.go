//go:build qualify

package qualify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/checkout"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/monero"
	"github.com/satbooth/satbooth/qr"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/rates"
	"github.com/satbooth/satbooth/rates/ratestest"
	"github.com/satbooth/satbooth/service"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/xmrrpc"
)

// The Monero phases (plan 8) run the whole service in-process against the regtest monerod and SatBooth's view-only
// wallet-rpc (wallet-satbooth, Digest login), as the on-chain phases run it against bitcoind. Every Monero phase shares
// one environment (/state/monero, one data directory), except the stuck pair (/state/monero-stuck, a clock 7 days
// ahead). The environment is an on-chain one (ocEnv: the registry, the events and the on-chain side of a mixed order,
// with the on-chain rail configured beside Monero) plus the Monero side of the safety check: per payment the forward
// and reversal transfers of every epoch in ledger 3, paired, with the code the payment's classification gives; the net
// per subaddress equal to what the merchant's own full wallet receives there (get_transfers on the customer
// wallet-rpc, at the order's depth); the ledger accounts equal to the sums. Every ERROR or WARN line not expected
// fails the phase.
//
// wallet-rpc refreshes its wallet from monerod every 20 s (DEFAULT_AUTO_REFRESH_PERIOD,
// src/wallet/wallet_rpc_server.cpp:64 at v0.18.5.4), while get_transfers and get_transfer_by_txid update the pool on
// every call (:2966, :3049): a pool payment shows within one poll, a block within one refresh plus one poll.

const (
	xmName        = "monero"
	xmStuckName   = "monero-stuck"
	xmBound       = 90 * time.Second // one auto-refresh of 20 s, one poll of 2 s, a pass, and slack
	xmPollSeconds = 2
	xmQuiet       = 30 * time.Second // longer than one auto-refresh: a negative check has to wait
	xmFeeMargin   = 100_000_000_000  // 0.1 XMR: what a transfer needs unlocked beyond its amount
	xmShort       = 300              // the shortest expires_in (api.MinExpiresIn)

	xmT1  = 500_000_000_000    // 0.5 XMR: up to 1 XMR, 1 confirmation (0 in the 0-conf configuration)
	xmT3  = 5_000_000_000_000  // 5 XMR: up to 10 XMR, 3 confirmations
	xmT10 = 20_000_000_000_000 // 20 XMR: above, 10 confirmations
)

// The ALERT lines of the Monero rail (monero/settle.go, monero/watch.go, monero/service.go, rail/paid.go): compared
// exactly, so a change of wording fails here. The underpaid and overpaid lines are the on-chain ones' texts.
const (
	alXmReorg        = "ALERT: a Monero payment was reverted by a reorg or a double spend"
	alXmPaidAgain    = "ALERT: a reverted Monero payment confirmed again"
	alXmLate         = "ALERT: a late Monero payment arrived after the order's expiry"
	alXmRecreated    = "ALERT: wallet-rpc had forgotten issued subaddresses; re-created up to the record"
	alXmHeightBehind = "ALERT: the wallet's height is below SatBooth's cursor; payments are found again as it catches up"
	alBothRails      = "ALERT: an order was paid on both rails"
	wnFetchFailed    = "WARN " + lnFetchFailed
)

// xmSetup is /state/monero/setup.json: public data of the throwaway regtest wallets. The view key and the wallet's
// password are only in view.json and wallet.pass (state volume, 0600, the monero user's).
type xmSetup struct {
	Customer string `json:"customer"` // the customer wallet's primary address (the miner's too)
	Merchant string `json:"merchant"` // the merchant wallet's primary address: the service's monero.address
	Version  uint32 `json:"version"`  // get_version of wallet-satbooth
}

// xmEnv is one Monero phase's world.
type xmEnv struct {
	*ocEnv
	su xmSetup
	cw *custWallet
}

func xmRoot(t testing.TB) string { return xmRootAt(t, xmName) }

// xmRootAt makes /state/<name>; 0711 so that wallet-satbooth's monero user reaches the two files TestMoneroSetup
// gives it, and nothing else (every other file is root's, 0600).
func xmRootAt(t testing.TB, name string) string {
	root := filepath.Join(stateDir(t), name)
	if err := os.MkdirAll(root, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o711); err != nil {
		t.Fatal(err)
	}
	return root
}

// xmTiers are the Monero tiers of every phase: 1 XMR, 10 XMR, above; first is the first tier's confirmations (1, or 0
// for the 0-conf configuration of TestMoneroPoolDrop).
func xmTiers(first int) []service.MoneroTierConfig {
	n := func(v int) *int { return &v }
	return []service.MoneroTierConfig{{UpToPiconero: 1_000_000_000_000, Confirmations: n(first)},
		{UpToPiconero: 10_000_000_000_000, Confirmations: n(3)}, {UpToPiconero: 0, Confirmations: n(10)}}
}

func xmLogin(t testing.TB) (user, pass string) {
	t.Helper()
	user, pass = os.Getenv("SATBOOTH_XMR_RPC_USER"), os.Getenv("SATBOOTH_XMR_RPC_PASSWORD")
	if user == "" || pass == "" {
		t.Fatal("SATBOOTH_XMR_RPC_USER or SATBOOTH_XMR_RPC_PASSWORD is not set")
	}
	return user, pass
}

// newViewClient is the harness's own xmrrpc client to wallet-satbooth. Use it only while no service runs.
func newViewClient(t testing.TB) *xmrrpc.Client {
	t.Helper()
	user, pass := xmLogin(t)
	c, err := xmrrpc.New(xmrrpc.Config{URL: walletSatbooth, User: user, Timeout: 30 * time.Second}, []byte(pass))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Zero)
	return c
}

func newXmEnv(t *testing.T, first int) *xmEnv { return newXmEnvAt(t, xmName, 0, nil, first) }

// newXmEnvAt prepares the Monero environment at /state/<name>: the Digest password file (0600, from the environment,
// never printed), the service's configuration with the Monero rail (and the on-chain one when TestOnchainSetup ran),
// rates when set, and the service's clock shifted by shift.
func newXmEnvAt(t *testing.T, name string, shift time.Duration, rates *service.RatesConfig, first int) *xmEnv {
	t.Helper()
	var su xmSetup
	if err := readJSONFile(filepath.Join(xmRoot(t), "setup.json"), &su); err != nil {
		t.Fatalf("setup file (did TestMoneroSetup run?): %v", shownErr(err))
	}
	root := xmRootAt(t, name)
	user, pass := xmLogin(t)
	passFile := filepath.Join(root, "xmr-rpc.pass")
	if err := os.WriteFile(passFile, []byte(pass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mc := &service.MoneroConfig{WalletRPCURL: walletSatbooth, RPCUser: user, RPCPasswordFile: passFile, Address: su.Merchant,
		Network: "regtest", Confirmations: xmTiers(first), PollSeconds: xmPollSeconds}
	oe := newOcEnvWith(t, name, root, false, rates, mc)
	oe.lateLine, oe.againLine = alXmLate, alXmPaidAgain
	if shift != 0 {
		oe.setClock(func() time.Time { return time.Now().Add(shift) })
		oe.t0 = oe.now()
	}
	return &xmEnv{ocEnv: oe, su: su, cw: &custWallet{}}
}

// setFirstTier rewrites the service's configuration with the first Monero tier at confs (the service restarts on it).
func (e *xmEnv) setFirstTier(confs int) {
	e.t.Helper()
	cfg, err := service.ReadConfig(e.cfgPath)
	if err != nil {
		e.t.Fatal(err)
	}
	cfg.Monero.Confirmations = xmTiers(confs)
	writeJSON0600(e.t, e.cfgPath, cfg)
}

// ---- monerod and the customer's wallets ----

func xmMine(n int, addr string) error {
	return monerodRPC("generateblocks", map[string]any{"amount_of_blocks": n, "wallet_address": addr}, nil)
}

func monerodHeight() (int64, error) {
	var r struct {
		Height int64 `json:"height"`
	}
	err := monerodRPC("get_info", nil, &r)
	return r.Height, err
}

// mineXM mines n blocks to the customer (its coinbase unlocks after 60 blocks).
func (e *xmEnv) mineXM(n int) {
	e.t.Helper()
	if err := xmMine(n, e.su.Customer); err != nil {
		e.t.Fatalf("generateblocks %d: %v", n, shownErr(err))
	}
}

func (e *xmEnv) heightXM() int64 {
	e.t.Helper()
	h, err := monerodHeight()
	if err != nil || h < 1 {
		e.t.Fatalf("get_info: %d, %v", h, shownErr(err))
	}
	return h
}

func (e *xmEnv) flush(txid string) {
	e.t.Helper()
	if err := monerodRPC("flush_txpool", map[string]any{"txids": []string{txid}}, nil); err != nil {
		e.t.Fatalf("flush_txpool: %v", shownErr(err))
	}
}

// ensureUnlocked mines 10 blocks at a time (a transfer's change is locked for 10) while the customer's unlocked balance
// is short of pico: back-to-back payments need spendable outputs.
func (e *xmEnv) ensureUnlocked(pico int64) {
	t := e.t
	t.Helper()
	for i := 0; ; i++ {
		var b struct {
			Unlocked uint64 `json:"unlocked_balance"`
		}
		if err := e.cw.call(walletCustomer, "get_balance", map[string]any{"account_index": 0}, &b); err != nil {
			t.Fatalf("get_balance: %v", shownErr(err))
		}
		if b.Unlocked >= uint64(pico)+xmFeeMargin {
			return
		}
		if i == 6 {
			t.Fatalf("the customer's unlocked balance stays at %d piconero", b.Unlocked)
		}
		t.Logf("NOTE the customer has %d piconero unlocked, short of %d: mining 10 blocks", b.Unlocked, pico)
		e.mineXM(10)
	}
}

// payXM sends pico from the customer to addr with the given unlock_time (0: none) and returns the transfer and when it
// was sent. The transfer's metadata is kept for relay_tx.
func (e *xmEnv) payXM(addr string, pico int64, unlock int64) (xferResult, time.Time) {
	e.t.Helper()
	return e.payMany(map[string]int64{addr: pico}, unlock)
}

func (e *xmEnv) payMany(dests map[string]int64, unlock int64) (xferResult, time.Time) {
	t := e.t
	t.Helper()
	var sum int64
	var ds []any
	for a, p := range dests {
		ds = append(ds, map[string]any{"address": a, "amount": p})
		sum += p
	}
	e.ensureUnlocked(sum)
	var r xferResult
	if err := e.cw.call(walletCustomer, "transfer", map[string]any{"destinations": ds, "account_index": 0, "unlock_time": unlock,
		"get_tx_key": true, "get_tx_hex": true, "get_tx_metadata": true}, &r); err != nil {
		t.Fatalf("transfer: %v", shownErr(err))
	}
	if len(r.TxHash) != 64 {
		t.Fatalf("transfer: tx_hash %q", r.TxHash)
	}
	return r, time.Now()
}

// merchantReceipts is the merchant's own full wallet's incoming entries (mined and pool), per subaddress.
func (e *xmEnv) merchantReceipts() map[string][]xferIn {
	e.t.Helper()
	var r struct {
		In   []xferIn `json:"in"`
		Pool []xferIn `json:"pool"`
	}
	if err := e.cw.call(walletMerchant, "get_transfers", map[string]any{"in": true, "pool": true, "account_index": 0}, &r); err != nil {
		e.t.Fatalf("get_transfers on the merchant wallet: %v", shownErr(err))
	}
	m := map[string][]xferIn{}
	for _, x := range slices.Concat(r.In, r.Pool) {
		m[x.Address] = append(m[x.Address], x)
	}
	return m
}

// merchantTx is the merchant wallet's get_transfer_by_txid of txid: its entries, or none with the error code.
func (e *xmEnv) merchantTx(txid string) ([]xferIn, int) {
	e.t.Helper()
	var r struct {
		Transfers []xferIn `json:"transfers"`
	}
	err := e.cw.call(walletMerchant, "get_transfer_by_txid", map[string]any{"txid": txid, "account_index": 0}, &r)
	var xe *xmrError
	if errors.As(err, &xe) {
		return nil, xe.Code
	}
	if err != nil {
		e.t.Fatalf("get_transfer_by_txid on the merchant wallet: %v", shownErr(err))
	}
	return r.Transfers, 0
}

// waitViewHeight waits until wallet-satbooth's height reaches h (no service may run).
func waitViewHeight(t *testing.T, cl *xmrrpc.Client, h int64) time.Duration {
	t.Helper()
	began := time.Now()
	poll(t, fmt.Sprintf("wallet-satbooth at height %d", h), 3*time.Minute, time.Second, func() (bool, error) {
		got, err := cl.GetHeight(context.Background())
		if err != nil {
			return false, err
		}
		return got >= h, fmt.Errorf("height %d", got)
	})
	return time.Since(began)
}

// ---- orders, presses and the page ----

var subRe = regexp.MustCompile(`id="monero" type="text" readonly="readonly" value="([1-9A-HJ-NP-Za-km-z]{95})"`)

// newXOrder creates an order with a Monero amount (and an on-chain one when sat is above 0) and registers it.
func (e *xmEnv) newXOrder(tag string, pico, sat, expiresIn int64, ev map[string]rng, post map[string]int) (*ocOrder, orderJSON) {
	t := e.t
	t.Helper()
	body := map[string]any{"account_id": acctID, "merchant_ref": tag + "-" + randHex(t)[:12], "monero_piconero": pico, "expires_in": expiresIn}
	if sat > 0 {
		body["onchain_sat"] = sat
	}
	var o orderJSON
	if code := e.api("POST", "/v1/orders", e.writeKey, body, &o); code != 201 {
		t.Fatalf("create order: status %d", code)
	}
	if o.State != "new" || o.Monero == nil || o.Monero.State != "waiting" || o.Monero.Address != nil || o.MoneroPiconero != pico ||
		(sat > 0) != (o.Onchain != nil) || o.Lightning != nil {
		t.Fatalf("created order looks wrong: state %q, monero %+v, onchain %v", o.State, o.Monero, o.Onchain != nil)
	}
	return e.registerX(o, tag, ev, post), o
}

// registerX adds an order with a Monero amount (a priced one too) to the registry with the events it must have.
func (e *xmEnv) registerX(o orderJSON, tag string, ev map[string]rng, post map[string]int) *ocOrder {
	e.t.Helper()
	exp, err := time.Parse(time.RFC3339, o.ExpiresAt)
	if err != nil || o.Monero == nil {
		e.t.Fatalf("order %s: expires_at %q, monero %v", o.ID, o.ExpiresAt, o.Monero != nil)
	}
	if ev == nil {
		ev = map[string]rng{}
	}
	ro := &ocOrder{ID: o.ID, Token: tokenOf(o), Tag: tag, Sat: o.OnchainSat, Pico: o.MoneroPiconero, XReq: o.Monero.RequiredConfirmations,
		Expiry: exp.Unix(), Ev: ev, Post: post}
	if o.Onchain != nil {
		ro.Required = o.Onchain.RequiredConfirmations
	}
	e.reg.Orders = append(e.reg.Orders, ro)
	e.saveReg()
	return ro
}

// pressXM is the customer pressing "Pay with Monero" (rail=monero), then the page, which carries the subaddress. A
// press answered busy is pressed again. The subaddress is checked against the API's.
func (e *xmEnv) pressXM(ro *ocOrder) string {
	t := e.t
	t.Helper()
	var sub string
	poll(t, "subaddress on the page", 60*time.Second, time.Second, func() (bool, error) {
		resp, err := e.hc.Post("http://"+publicAddr+"/c/"+ro.Token+"/pay", "application/x-www-form-urlencoded", strings.NewReader("rail=monero"))
		if err != nil {
			return false, err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			return false, fmt.Errorf("pay: status %d", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/c/"+ro.Token {
			return false, fmt.Errorf("pay: redirected to %q", loc)
		}
		code, page := e.page(ro.Token)
		m := subRe.FindStringSubmatch(page)
		if code != 200 || m == nil || !strings.Contains(page, "Waiting for payment") {
			return false, errors.New("no subaddress on the page yet")
		}
		sub = m[1]
		return true, nil
	})
	if o := e.order(ro.ID); o.Monero == nil || o.Monero.Address == nil || *o.Monero.Address != sub {
		t.Fatalf("the API's subaddress differs from the page's for order %s", ro.ID)
	}
	if !xmrrpc.ValidAddress(sub, "regtest", true) {
		t.Fatalf("order %s was given a subaddress that is not one", ro.ID)
	}
	if ro.Sub != "" && ro.Sub != sub {
		t.Fatalf("order %s was given a second subaddress", ro.ID)
	}
	ro.Sub = sub
	e.saveReg()
	return sub
}

// checkMoneroQR checks that the served Monero QR is the QR of MoneroURI(subaddress, amount) and the page's wallet link
// is that URI; the SVG is saved for the manual phone-wallet scan.
func (e *xmEnv) checkMoneroQR(ro *ocOrder, sub string, pico int64) {
	t := e.t
	t.Helper()
	uri, err := checkout.MoneroURI(sub, pico)
	if err != nil {
		t.Fatalf("MoneroURI of the page's fields: %v", shownErr(err))
	}
	resp, err := e.hc.Get("http://" + publicAddr + "/c/" + ro.Token + "/qr.svg?rail=monero")
	if err != nil {
		t.Fatal(err)
	}
	svg, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if rerr != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("qr.svg?rail=monero: status %d, type %q, %v", resp.StatusCode, resp.Header.Get("Content-Type"), shownErr(rerr))
	}
	want, err := qr.SVG(uri)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(svg, want) {
		t.Fatalf("the served Monero QR is not the QR of MoneroURI(subaddress, amount) (%d vs %d bytes)", len(svg), len(want))
	}
	if _, page := e.page(ro.Token); !strings.Contains(page, `href="`+uri+`"`) {
		t.Error("the page's Monero wallet link is not the Monero URI")
	}
	if err := os.WriteFile(filepath.Join(e.root, "monero.svg"), svg, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURED monero=pay qr_bytes=%d uri_bytes=%d (limit %d); the SVG is saved as monero.svg; the phone-wallet scan is a manual step", len(svg), len(uri), qr.MaxInput)
}

// waitXM polls the API until cond holds; it returns how long it took from since and the order.
func (e *xmEnv) waitXM(id, what string, since time.Time, bound time.Duration, cond func(orderJSON) bool) (time.Duration, orderJSON) {
	e.t.Helper()
	var d time.Duration
	var last orderJSON
	poll(e.t, what, bound, 200*time.Millisecond, func() (bool, error) {
		last = e.order(id)
		if last.Monero != nil && cond(last) {
			d = time.Since(since)
			return true, nil
		}
		xm := "none"
		if last.Monero != nil {
			xm = fmt.Sprintf("%+v", *last.Monero)
		}
		return false, fmt.Errorf("state %s, monero %s", last.State, xm)
	})
	return d, last
}

// ---- finishing a run ----

// finish stops the service and runs the safety check (events and the on-chain side, then the Monero side) and the
// ALERT check, as the on-chain phases do.
func (e *xmEnv) finish(s *svcRun, alerts map[string]rng) {
	e.t.Helper()
	e.quiesce()
	s.stopQuiet()
	runEnd := time.Now()
	added := e.safety(e.runStart, runEnd)
	e.safetyXM()
	e.checkAlerts(s.logs.String(), alerts, added)
	e.saveReg()
}

// safetyXM is the Monero side of the safety check (the service must not be running).
func (e *xmEnv) safetyXM() {
	t := e.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	lg, err := ledger.Open(ctx, filepath.Join(e.dataDir, "ledger.db"))
	if err != nil {
		t.Fatalf("safety: ledger: %v", shownErr(err))
	}
	defer lg.Close()
	st, err := store.Open(ctx, filepath.Join(e.dataDir, "store.db"))
	if err != nil {
		t.Fatalf("safety: store: %v", shownErr(err))
	}
	defer st.Close()
	recv := e.merchantReceipts()
	var sumFwd, sumRev uint64
	checked := 0
	for _, o := range e.reg.Orders {
		if o.Sub == "" {
			continue
		}
		checked++
		a, err := st.SubaddressForOrder(ctx, o.ID)
		if err != nil || a.Address != o.Sub {
			t.Errorf("order %s (%s): the stored subaddress differs from the page's (%v)", o.ID, o.Tag, shownErr(err))
			continue
		}
		raw, err := hex.DecodeString(o.ID)
		if err != nil {
			t.Fatal(err)
		}
		ud, err := ledger.U128FromBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		pays, err := st.PaymentsForOrder(ctx, o.ID)
		if err != nil {
			t.Fatal(err)
		}
		exp := time.Unix(o.Expiry, 0)
		var net int64
		seen := map[store.PaymentKey]bool{}
		for _, p := range pays {
			k := store.PaymentKey{TxID: p.TxID, Address: p.Address}
			if seen[k] || p.Address != o.Sub {
				t.Errorf("order %s: payment %s stored twice or to another subaddress", o.ID, p.TxID)
			}
			seen[k] = true
			// Counted: first seen before the expiry, and not locked beyond the normal lock (10 blocks) at first sight
			// (monero/ledger.go counted); a counted forward has code 2, a late one code 4.
			counted := p.FirstSeen.Before(exp) && !(p.UnlockTime > 0 && p.UnlockTime > p.FirstSeenHeight+10)
			wantCode := rail.CodeLate
			if counted {
				wantCode = rail.CodePaid
			}
			amt := ledger.U128From64(uint64(p.AmountPiconero))
			live := false
			for ep := uint32(0); ep < 64; ep++ {
				f, found, err := lg.LookupTransfer(ctx, monero.PaidTransferID(p.TxID, uint32(a.Index), ep))
				if err != nil {
					t.Fatal(err)
				}
				if !found {
					break
				}
				if live {
					t.Errorf("order %s: payment %s has a forward of epoch %d after an unreversed one", o.ID, p.TxID, ep)
				}
				if f.Code != wantCode {
					t.Errorf("order %s: forward of %s epoch %d has code %d, want %d (first seen %s at height %d, unlock_time %d, expiry %s)", o.ID, p.TxID, ep, f.Code, wantCode,
						p.FirstSeen.UTC().Format(time.RFC3339), p.FirstSeenHeight, p.UnlockTime, exp.UTC().Format(time.RFC3339))
				}
				if f.Amount != amt || f.UserData128 != ud || f.DebitAccountID != monero.MoneroWallet || f.CreditAccountID != monero.MerchantMonero || f.Flags != 0 {
					t.Errorf("order %s: forward of %s epoch %d has other details", o.ID, p.TxID, ep)
				}
				sumFwd += f.Amount.Lo
				rv, rfound, err := lg.LookupTransfer(ctx, monero.ReversalTransferID(p.TxID, uint32(a.Index), ep))
				if err != nil {
					t.Fatal(err)
				}
				if rfound {
					if rv.Amount != f.Amount || rv.UserData128 != ud || rv.Code != rail.CodeReversed || rv.DebitAccountID != monero.MerchantMonero || rv.CreditAccountID != monero.MoneroWallet {
						t.Errorf("order %s: reversal of %s epoch %d does not mirror its forward", o.ID, p.TxID, ep)
					}
					sumRev += rv.Amount.Lo
					continue
				}
				live = true
				net += p.AmountPiconero
			}
		}
		// The merchant's own wallet at the order's depth (the pool too for a 0 tier).
		var got int64
		for _, x := range recv[o.Sub] {
			if x.Type == "in" && x.Confirmations >= int64(o.XReq) || x.Type == "pool" && o.XReq == 0 {
				got += int64(x.Amount)
			}
		}
		if got != net {
			t.Errorf("order %s (%s): the ledger holds %d piconero net, the merchant wallet receives %d at %d confirmations", o.ID, o.Tag, net, got, o.XReq)
		}
	}
	for _, c := range []struct {
		name          string
		acct          ledger.U128
		debit, credit uint64
	}{
		{"monero wallet", monero.MoneroWallet, sumFwd, sumRev},
		{"merchant monero", monero.MerchantMonero, sumRev, sumFwd},
	} {
		b, err := lg.Balance(ctx, c.acct)
		if err != nil {
			t.Fatal(err)
		}
		if b.DebitsPosted != ledger.U128From64(c.debit) || b.CreditsPosted != ledger.U128From64(c.credit) {
			t.Errorf("ledger account %s: debits %s credits %s, want %d and %d", c.name, b.DebitsPosted, b.CreditsPosted, c.debit, c.credit)
		}
	}
	t.Logf("SAFETY monero=%s orders_with_subaddress=%d forward_piconero=%d reversed_piconero=%d (net per subaddress compared with the merchant's full wallet)", e.name, checked, sumFwd, sumRev)
}

// logCount is the number of records with the message msg whose attribute key equals want (key "" counts every one).
func logCount(t *testing.T, logs, msg, key string, want any) int {
	t.Helper()
	n := 0
	for _, l := range strings.Split(logs, "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil || m["msg"] != msg {
			continue
		}
		if key == "" || fmt.Sprint(m[key]) == fmt.Sprint(want) {
			n++
		}
	}
	return n
}

// ---- the phases ----

// createOrOpen makes the full wallet name on the customer wallet-rpc (or opens it, for a rerun) and returns its primary
// address.
func createOrOpen(t *testing.T, cw *custWallet, name string) string {
	t.Helper()
	err := walletRPC(walletCustPort, "create_wallet", map[string]any{"filename": name, "language": "English", "password": ""}, nil)
	if err == nil {
		cw.open = name
	} else {
		t.Logf("NOTE create_wallet %s: %v; opening it", name, shownErr(err))
	}
	var a struct {
		Address string `json:"address"`
	}
	if err := cw.call(name, "get_address", map[string]any{"account_index": 0}, &a); err != nil {
		t.Fatalf("get_address %s: %v", name, shownErr(err))
	}
	if !xmrrpc.ValidAddress(a.Address, "regtest", false) {
		t.Fatalf("the %s wallet's primary address is not a regtest primary address", name)
	}
	return a.Address
}

// writeMoneroFile writes data 0600 for wallet-satbooth's monero user.
func writeMoneroFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, moneroUID, moneroUID); err != nil {
		t.Fatal(err)
	}
}

// TestMoneroSetup: the customer's and the merchant's full wallets, 120 blocks to the customer, the merchant's view-only
// wallet in wallet-satbooth, and facts inferred beforehand, read on the real wallet-rpc: the sign_transfer codes on
// both wallets, the unknown-transaction and index-out-of-range codes, the Digest challenge lines, concurrent calls
// through one client, and one transaction paying two subaddresses. Then the first start proves the configuration.
func TestMoneroSetup(t *testing.T) {
	root := xmRoot(t)
	ctx := context.Background()
	cw := &custWallet{}
	// -13, the no-wallet code (monero.ErrNoWallet), on the customer wallet-rpc with no wallet open.
	// wallet-customer answers once it has started (a JSON-RPC answer, an error included).
	poll(t, "wallet-customer answers", 2*time.Minute, time.Second, func() (bool, error) {
		err := walletRPC(walletCustPort, "get_version", nil, nil)
		var xe *xmrError
		return err == nil || errors.As(err, &xe), err
	})
	_ = walletRPC(walletCustPort, "close_wallet", nil, nil)
	var ne *xmrError
	if err := walletRPC(walletCustPort, "get_height", nil, nil); !errors.As(err, &ne) || ne.Code != -13 {
		t.Fatalf("get_height with no wallet open: %v; want code -13", shownErr(err))
	}
	t.Logf("OBSERVED monero get_height on a wallet-rpc with no wallet open: code %d", ne.Code)
	cust := createOrOpen(t, cw, walletCustomer)
	merch := createOrOpen(t, cw, walletMerchant)
	var vk struct {
		Key string `json:"key"`
	}
	if err := cw.call(walletMerchant, "query_key", map[string]any{"key_type": "view_key"}, &vk); err != nil || len(vk.Key) != 64 {
		t.Fatalf("query_key view_key: %v", shownErr(err))
	}
	// The view-only wallet's description for monero-wallet-cli --generate-from-json: the address and the view key, no
	// spend key. wallet2::make_from_json gives the new wallet the JSON's password (src/wallet/wallet2.cpp, the
	// field_password of every generate call in make_from_json; --password-file is not read for it), so the JSON carries
	// the same password as wallet.pass, which wallet-rpc's --password-file opens it with.
	// A rerun keeps the password: wallet-satbooth generates the wallet only once, with the first one.
	pw := randHex(t)
	if b, err := os.ReadFile(filepath.Join(root, "wallet.pass")); err == nil && len(b) == 64 {
		pw = string(b)
	}
	view, err := json.Marshal(map[string]any{"version": 1, "filename": viewWalletFile, "scan_from_height": 0, "address": merch,
		"viewkey": vk.Key, "password": pw})
	if err != nil {
		t.Fatal(err)
	}
	writeMoneroFile(t, filepath.Join(root, "wallet.pass"), []byte(pw))
	writeMoneroFile(t, filepath.Join(root, "view.json"), view)

	if err := xmMine(120, cust); err != nil {
		t.Fatalf("generateblocks 120: %v", shownErr(err))
	}
	h, err := monerodHeight()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURED monero=setup monerod_height=%d (120 blocks mined to the customer: coinbase unlocks after 60, cryptonote_config.h:44)", h)

	cl := newViewClient(t)
	began := time.Now()
	var ver uint32
	poll(t, "wallet-satbooth answers get_version with the Digest login", 3*time.Minute, 2*time.Second, func() (bool, error) {
		v, err := cl.GetVersion(ctx)
		ver = v
		return err == nil, err
	})
	synced := waitViewHeight(t, cl, h)
	t.Logf("MEASURED monero=setup wallet_rpc_version=%d.%d (get_version major.minor) answer_seconds=%.1f sync_seconds=%.1f", ver>>16, ver&0xffff, time.Since(began).Seconds(), synced.Seconds())

	// The view-only probe on both wallets.
	viewCode, err := cl.SignTransferProbe(ctx)
	if err != nil || viewCode != -29 {
		t.Fatalf("sign_transfer on the view-only wallet: code %d, %v; want -29", viewCode, shownErr(err))
	}
	// The full wallet, on the customer wallet-rpc: the same request as SignTransferProbe's, sent by the harness (xmrrpc
	// refuses a wallet-rpc that asks for no login, as this one runs with --disable-rpc-login).
	var fe *xmrError
	err = cw.call(walletMerchant, "sign_transfer", map[string]any{"unsigned_txset": "", "export_raw": false}, nil)
	if !errors.As(err, &fe) || fe.Code == -29 {
		t.Fatalf("sign_transfer on the merchant's full wallet: %v; want an error code other than -29", shownErr(err))
	}
	fullCode := fe.Code
	t.Logf("OBSERVED monero sign_transfer with an empty unsigned_txset: view-only wallet code %d, full wallet (merchant) code %d (inferred: confirmed)", viewCode, fullCode)

	// Check step 5's out-of-range code, on the fresh view-only wallet.
	var xe *xmrrpc.Error
	if _, err := cl.GetAddress(ctx, 0, []uint32{1000}); !errors.As(err, &xe) || xe.Code != -15 {
		t.Fatalf("get_address index 1000 on the fresh view-only wallet: %v; want code -15", shownErr(err))
	}
	t.Logf("OBSERVED monero get_address{address_index:[1000]} on the fresh view-only wallet: code %d", xe.Code)
	// GetTransferByTxID's unknown-transaction code.
	xe = nil
	if err := cl.Call(ctx, "get_transfer_by_txid", map[string]any{"txid": randHex(t), "account_index": 0}, nil); !errors.As(err, &xe) || xe.Code != -8 {
		t.Fatalf("get_transfer_by_txid of a random txid: %v; want code -8", shownErr(err))
	}
	t.Logf("OBSERVED monero get_transfer_by_txid of a random txid on the view-only wallet: code %d", xe.Code)

	checkChallenge(t)

	// 20 concurrent calls through one client (it sends one at a time).
	var wg sync.WaitGroup
	errs := make([]error, 20)
	callsBegan := time.Now()
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = cl.GetHeight(ctx)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("20 concurrent get_height through one client: %v", shownErr(err))
	}
	t.Logf("OBSERVED monero 20 concurrent get_height through one xmrrpc.Client: all 20 succeeded in %.2f s (the client sends one request at a time)", time.Since(callsBegan).Seconds())

	// One transaction paying two subaddresses of the merchant: get_transfer_by_txid lists both in transfers.
	var subs []xmrrpc.AddressEntry
	for range 2 {
		var c struct {
			Address string `json:"address"`
			Index   uint32 `json:"address_index"`
		}
		if err := cw.call(walletMerchant, "create_address", map[string]any{"account_index": 0, "label": "qualify-setup"}, &c); err != nil {
			t.Fatalf("create_address on the merchant wallet: %v", shownErr(err))
		}
		subs = append(subs, xmrrpc.AddressEntry{Address: c.Address, Index: c.Index})
	}
	e0 := &xmEnv{su: xmSetup{Customer: cust}, cw: cw, ocEnv: &ocEnv{lnEnv: &lnEnv{t: t}}}
	r, _ := e0.payMany(map[string]int64{subs[0].Address: 100_000_000_000, subs[1].Address: 200_000_000_000}, 0)
	e0.mineXM(1)
	var ts []xmrrpc.Transfer
	poll(t, "the view-only wallet lists both subaddresses of the transaction", xmBound, 2*time.Second, func() (bool, error) {
		var err error
		ts, err = cl.GetTransferByTxID(ctx, r.TxHash, 0)
		if err != nil {
			return false, err
		}
		return len(ts) == 2 && ts[0].Height > 0, fmt.Errorf("%d entries", len(ts))
	})
	for _, s := range subs {
		if !slices.ContainsFunc(ts, func(x xmrrpc.Transfer) bool { return x.Index == s.Index && x.Address == s.Address }) {
			t.Errorf("get_transfer_by_txid does not list the subaddress at index %d", s.Index)
		}
	}
	t.Logf("OBSERVED monero get_transfer_by_txid of one transaction paying two subaddresses (indices %d and %d): %d entries in transfers", subs[0].Index, subs[1].Index, len(ts))
	cl.Zero()

	writeJSON0600(t, filepath.Join(root, "setup.json"), xmSetup{Customer: cust, Merchant: merch, Version: ver})
	// The first start proves the configuration is accepted: Check passes against the real wallet-rpc.
	e := newXmEnv(t, 1)
	s := e.start()
	e.finish(s, nil)
}

var nonceRe = regexp.MustCompile(`nonce="[^"]*"`)

// maskChallenge hides what changes from one wallet-rpc start to the next: the nonce and the Last-Modified date.
func maskChallenge(l string) string {
	if k, _, ok := strings.Cut(l, ":"); ok && strings.EqualFold(k, "Last-Modified") {
		return k + ": <date>"
	}
	return nonceRe.ReplaceAllString(l, `nonce="<nonce>"`)
}

// checkChallenge sends one request without a login to wallet-satbooth over a raw connection (net/http would fold the
// header names) and compares the 401's header lines with xmrrpc/testdata/challenge.txt, the nonce and the date masked.
func checkChallenge(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(walletSatbooth, "http://"), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"jsonrpc":"2.0","id":"0","method":"get_version","params":{}}`
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "POST /json_rpc HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		strings.TrimPrefix(walletSatbooth, "http://"), len(body), body); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	var got []string
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the 401's headers: %v", shownErr(err))
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			break
		}
		got = append(got, maskChallenge(l))
	}
	raw, err := os.ReadFile("../xmrrpc/testdata/challenge.txt")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, l := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		want = append(want, maskChallenge(strings.TrimRight(l, "\r")))
	}
	// A line of the answer is printed only if the expected challenge has it; any other is "other(N bytes)".
	gotShown := make([]string, len(got))
	for i, l := range got {
		gotShown[i] = shown(l, want...)
	}
	t.Logf("OBSERVED monero the 401 of wallet-satbooth (nonce and date masked): %d lines, equal to the expected challenge %v", len(got), slices.Equal(got, want))
	if !slices.Equal(got, want) {
		t.Errorf("the 401's header lines differ from xmrrpc/testdata/challenge.txt:\n got %q\nwant %q", gotShown, want)
	}
}

// TestMoneroPay: three orders at the three tiers, the picker, the press, the page and its QR, paid from the customer,
// mined one block at a time.
func TestMoneroPay(t *testing.T) {
	e := newXmEnv(t, 1)
	s := e.start()
	paid := map[string]rng{store.EventPaid: {1, 1}}
	type tiered struct {
		ro   *ocOrder
		pico int64
		req  int
		sub  string
		sent time.Time
	}
	var ts []*tiered
	for _, c := range []struct {
		pico int64
		req  int
	}{{xmT1, 1}, {xmT3, 3}, {xmT10, 10}} {
		ro, o := e.newXOrder(fmt.Sprintf("xm-pay-t%d", c.req), c.pico, 0, 86400, paid, nil)
		if o.Monero.RequiredConfirmations != c.req {
			t.Fatalf("an order of %d piconero needs %d confirmations, want %d", c.pico, o.Monero.RequiredConfirmations, c.req)
		}
		// The picker of a Monero-only order: the Monero button alone.
		code, page := e.page(ro.Token)
		if code != 200 || !strings.Contains(page, `<input type="hidden" name="rail" value="monero" />`) || !strings.Contains(page, "Pay with Monero") ||
			strings.Contains(page, `value="bitcoin"`) || !strings.Contains(page, "Ready to pay") {
			t.Fatalf("the page of a Monero-only order does not offer exactly the Monero button (status %d)", code)
		}
		sub := e.pressXM(ro)
		_, page = e.page(ro.Token)
		if !strings.Contains(page, sub) || !strings.Contains(page, "Send exactly "+xmrrpc.FormatXMR(c.pico)+" XMR.") || strings.Contains(page, "Pay with Monero") {
			t.Fatalf("the page of the %d piconero order lacks its subaddress or amount, or still offers the button", c.pico)
		}
		e.checkMoneroQR(ro, sub, c.pico)
		ts = append(ts, &tiered{ro: ro, pico: c.pico, req: c.req, sub: sub})
	}
	for _, x := range ts {
		_, x.sent = e.payXM(x.sub, x.pico, 0)
	}
	for _, x := range ts {
		d, o := e.waitXM(x.ro.ID, "seen", x.sent, xmBound, func(o orderJSON) bool {
			return o.Monero.State == "seen" && o.Monero.ReceivedPiconero == x.pico
		})
		if o.State != "new" || o.Monero.Confirmations != 0 || o.Monero.ConfirmedPiconero != 0 {
			t.Errorf("tier %d: seen order reads %+v", x.req, *o.Monero)
		}
		e.pageHas(x.ro.Token, fmt.Sprintf("Seen, 0 of %d confirmations", x.req))
		t.Logf("MEASURED monero=pay tier=%d send_to_seen_seconds=%.2f (poll interval 2 s; the pool is read on each call)", x.req, d.Seconds())
	}
	for n := 1; n <= 10; n++ {
		mined := time.Now()
		e.mineXM(1)
		for _, x := range ts {
			if n >= x.req {
				d, o := e.waitXM(x.ro.ID, fmt.Sprintf("tier %d paid at block %d", x.req, n), mined, xmBound, func(o orderJSON) bool {
					return o.State == "paid" && o.PaidAt != nil && o.Monero.State == "paid" && o.Monero.ConfirmedPiconero == x.pico
				})
				if n == x.req {
					t.Logf("MEASURED monero=pay tier=%d confirmations=%d tier_block_to_paid_seconds=%.2f (wallet-rpc refreshes every 20 s)", x.req, n, d.Seconds())
				}
				if o.Monero.Overpaid {
					t.Errorf("tier %d reads overpaid", x.req)
				}
				continue
			}
			_, o := e.waitXM(x.ro.ID, fmt.Sprintf("tier %d at %d of %d", x.req, n, x.req), mined, xmBound, func(o orderJSON) bool {
				return o.Monero.Confirmations == n
			})
			if o.State != "new" || o.Monero.State != "seen" || o.Monero.ConfirmedPiconero != 0 {
				t.Errorf("tier %d at %d of %d confirmations already reads %s / %s", x.req, n, x.req, o.State, o.Monero.State)
			}
		}
	}
	for _, x := range ts {
		e.waitHook(x.ro.ID, store.EventPaid, hookBound)
		e.pageHas(x.ro.Token, ">Paid<")
	}
	time.Sleep(xmQuiet)
	e.finish(s, nil)
}

// TestMoneroPartial: 60 % then 40 %, 120 %, and an order with a short expiry paid 60 % (read by TestMoneroLateAfter).
func TestMoneroPartial(t *testing.T) {
	e := newXmEnv(t, 1)
	s := e.start()

	r1, _ := e.newXOrder("xm-partial-split", xmT1, 0, 86400, map[string]rng{store.EventPaid: {1, 1}}, nil)
	a1 := e.pressXM(r1)
	e.payXM(a1, xmT1*60/100, 0)
	e.mineXM(1)
	_, o := e.waitXM(r1.ID, "60 % confirmed", time.Now(), xmBound, func(o orderJSON) bool { return o.Monero.ConfirmedPiconero == xmT1*60/100 })
	if o.State != "new" || o.Monero.State != "seen" {
		t.Errorf("60 %% paid reads %s / %s, want new / seen", o.State, o.Monero.State)
	}
	e.pageHas(r1.Token, xmrrpc.FormatXMR(xmT1*60/100)+" XMR seen so far.")
	e.payXM(a1, xmT1*40/100, 0)
	mined := time.Now()
	e.mineXM(1)
	d, o := e.waitXM(r1.ID, "paid by the sum", mined, xmBound, func(o orderJSON) bool { return o.State == "paid" })
	if o.Monero.ConfirmedPiconero != xmT1 || o.Monero.Overpaid {
		t.Errorf("60 %% + 40 %% reads confirmed %d, overpaid %v", o.Monero.ConfirmedPiconero, o.Monero.Overpaid)
	}
	t.Logf("MEASURED monero=partial second_part_block_to_paid_seconds=%.2f", d.Seconds())
	e.waitHook(r1.ID, store.EventPaid, hookBound)

	r2, _ := e.newXOrder("xm-partial-over", xmT1, 0, 86400, map[string]rng{store.EventPaid: {1, 1}}, nil)
	a2 := e.pressXM(r2)
	e.payXM(a2, xmT1*120/100, 0)
	mined = time.Now()
	e.mineXM(1)
	d, o = e.waitXM(r2.ID, "overpaid", mined, xmBound, func(o orderJSON) bool { return o.State == "paid" && o.Monero.Overpaid })
	if o.Monero.ConfirmedPiconero != xmT1*120/100 {
		t.Errorf("120 %% reads confirmed %d", o.Monero.ConfirmedPiconero)
	}
	t.Logf("MEASURED monero=partial overpaid_block_to_paid_seconds=%.2f", d.Seconds())
	e.waitHook(r2.ID, store.EventPaid, hookBound)
	e.pageHas(r2.Token, "The merchant will refund the extra.")

	r3, _ := e.newXOrder("xm-partial-short", xmT1, 0, xmShort, nil, map[string]int{store.EventUnderpaid: 1})
	a3 := e.pressXM(r3)
	e.payXM(a3, xmT1*60/100, 0)
	e.mineXM(1)
	e.waitXM(r3.ID, "60 % confirmed", time.Now(), xmBound, func(o orderJSON) bool { return o.Monero.ConfirmedPiconero == xmT1*60/100 })
	time.Sleep(xmQuiet)
	e.finish(s, map[string]rng{alOverpaid: {1, 1}})
}

// TestMoneroPoolDrop: a payment flushed from the pool before a block leaves nothing recorded and its order expires;
// then, on a 0-conf configuration, a payment paid from the pool, flushed (order.reverted) and relayed again and mined
// (order.paid_again).
func TestMoneroPoolDrop(t *testing.T) {
	e := newXmEnv(t, 1)
	s := e.start()
	ra, _ := e.newXOrder("xm-pooldrop", xmT1, 0, xmShort, nil, map[string]int{store.EventExpired: 1})
	sa := e.pressXM(ra)
	xa, sent := e.payXM(sa, xmT1, 0)
	e.waitXM(ra.ID, "seen", sent, xmBound, func(o orderJSON) bool { return o.Monero.State == "seen" && o.Monero.ReceivedPiconero == xmT1 })
	e.flush(xa.TxHash)
	flushed := time.Now()
	d, _ := e.waitXM(ra.ID, "back to waiting", flushed, xmBound, func(o orderJSON) bool {
		return o.State == "new" && o.Monero.State == "waiting" && o.Monero.ReceivedPiconero == 0
	})
	t.Logf("MEASURED monero=pooldrop flush_to_waiting_seconds=%.2f", d.Seconds())
	e.pageHas(ra.Token, "Waiting for payment")
	e.mineXM(1)
	time.Sleep(xmQuiet)
	if o := e.order(ra.ID); o.State != "new" || o.Monero.ReceivedPiconero != 0 || o.Monero.ConfirmedPiconero != 0 {
		t.Errorf("the flushed payment's order reads %s with %d piconero received", o.State, o.Monero.ReceivedPiconero)
	}
	_, pageA := e.page(ra.Token)
	if strings.Contains(pageA, "Payment seen") {
		t.Error("the flushed payment's page still says Payment seen")
	}
	e.finish(s, nil)

	// The 0-conf configuration: the first tier at 0, the service restarted.
	e.setFirstTier(0)
	s = e.start()
	rb, ob := e.newXOrder("xm-zeroconf", xmT1, 0, 86400,
		map[string]rng{store.EventPaid: {1, 1}, store.EventReverted: {1, 1}, store.EventPaidAgain: {1, 1}}, nil)
	if ob.Monero.RequiredConfirmations != 0 {
		t.Fatalf("an order of %d piconero needs %d confirmations on the 0-conf configuration, want 0", xmT1, ob.Monero.RequiredConfirmations)
	}
	sb := e.pressXM(rb)
	h0 := e.heightXM()
	xb, sent := e.payXM(sb, xmT1, 0)
	rb.TxID = xb.TxHash
	d, o := e.waitXM(rb.ID, "paid from the pool", sent, xmBound, func(o orderJSON) bool { return o.State == "paid" && o.Monero.State == "paid" })
	if o.Monero.Confirmations != 0 || o.Monero.ConfirmedPiconero != xmT1 || o.PaidAt == nil {
		t.Errorf("paid from the pool reads %+v", *o.Monero)
	}
	if h := e.heightXM(); h != h0 {
		t.Fatalf("a block was mined (height %d, then %d): not paid from the pool", h0, h)
	}
	t.Logf("MEASURED monero=zeroconf send_to_paid_seconds=%.2f (poll interval 2 s, no block)", d.Seconds())
	e.waitHook(rb.ID, store.EventPaid, hookBound)
	e.flush(xb.TxHash)
	flushed = time.Now()
	d, _ = e.waitXM(rb.ID, "reverted to waiting", flushed, xmBound, func(o orderJSON) bool {
		return o.State == "new" && o.Monero.State == "waiting" && o.Monero.ReceivedPiconero == 0
	})
	t.Logf("MEASURED monero=zeroconf flush_to_new_seconds=%.2f", d.Seconds())
	e.waitHook(rb.ID, store.EventReverted, hookBound)
	// relay_tx takes the transfer's tx_metadata (wallet_rpc_server.cpp on_relay_tx parses a pending_tx), not the raw hex.
	var rr struct {
		TxHash string `json:"tx_hash"`
	}
	if err := e.cw.call(walletCustomer, "relay_tx", map[string]any{"hex": xb.TxMetadata}, &rr); err != nil {
		t.Fatalf("relay_tx of the saved transaction: %v", shownErr(err))
	}
	t.Logf("OBSERVED monero=zeroconf relay_tx of the flushed transaction's metadata: accepted, same txid %v", rr.TxHash == xb.TxHash)
	relayed := time.Now()
	e.mineXM(1)
	d, o = e.waitXM(rb.ID, "paid again", relayed, xmBound, func(o orderJSON) bool { return o.State == "paid" && o.Monero.ConfirmedPiconero == xmT1 })
	t.Logf("MEASURED monero=zeroconf relay_to_paid_again_seconds=%.2f (confirmations %d)", d.Seconds(), o.Monero.Confirmations)
	e.waitHook(rb.ID, store.EventPaidAgain, hookBound)

	// The first order's expiry: order.expired, its payment never recorded.
	if wait := time.Until(time.Unix(ra.Expiry, 0)); wait > 0 {
		t.Logf("waiting %.0f s for the expiry of %s", wait.Seconds(), ra.Tag)
	}
	e.waitXM(ra.ID, "expired", time.Now(), time.Until(time.Unix(ra.Expiry, 0))+xmBound, func(o orderJSON) bool { return o.State == "expired" })
	e.waitHook(ra.ID, store.EventExpired, hookBound)
	time.Sleep(xmQuiet)
	e.finish(s, map[string]rng{alXmReorg: {1, 1}})
}

// TestMoneroReorg: a payment mined, its block popped and the chain extended (the transaction returns to the pool and
// is mined again, or not: recorded); then a payment whose block is popped and which is flushed: order.reverted.
func TestMoneroReorg(t *testing.T) {
	e := newXmEnv(t, 1)
	s := e.start()

	r1, _ := e.newXOrder("xm-reorg-plain", xmT1, 0, 86400,
		map[string]rng{store.EventPaid: {1, 1}, store.EventReverted: {0, 1}, store.EventPaidAgain: {0, 1}}, nil)
	a1 := e.pressXM(r1)
	x1, sent := e.payXM(a1, xmT1, 0)
	e.waitXM(r1.ID, "seen", sent, xmBound, func(o orderJSON) bool { return o.Monero.State == "seen" })
	e.mineXM(1)
	e.waitXM(r1.ID, "paid", time.Now(), xmBound, func(o orderJSON) bool { return o.State == "paid" })
	e.waitHook(r1.ID, store.EventPaid, hookBound)
	// The service stops for the pop, so the harness may ask wallet-satbooth itself what it shows between the pop and the
	// next block (the harness never runs a second Digest client beside the service: see monerorpc.go).
	e.finish(s, nil)
	cursor := e.heightXM()
	h, err := monerodPopBlocks(1)
	if err != nil {
		t.Fatalf("pop_blocks 1: %v", shownErr(err))
	}
	// Whether wallet-satbooth drops the popped block before the next one: its height is read every second for two
	// auto-refresh periods and a poll (45 s); detached is a height below the cursor seen in that time.
	ctx := context.Background()
	cl := newViewClient(t)
	popped := time.Now()
	var vh int64
	detached := false
	for time.Since(popped) < 45*time.Second {
		if vh, err = cl.GetHeight(ctx); err != nil {
			t.Fatal(err)
		}
		if vh < cursor {
			detached = true
			break
		}
		time.Sleep(time.Second)
	}
	vts, verr := cl.GetTransferByTxID(ctx, x1.TxHash, 0)
	cl.Zero()
	view := []string{}
	for _, x := range vts {
		view = append(view, fmt.Sprintf("%s height %d", shown(x.Type, "in", "out", "pending", "pool", "failed", "block"), x.Height))
	}
	mv, code := e.merchantTx(x1.TxHash)
	merch := []string{}
	for _, x := range mv {
		merch = append(merch, fmt.Sprintf("%s height %d", shown(x.Type, "in", "out", "pending", "pool", "failed", "block"), x.Height))
	}
	var pool struct {
		Hashes []string `json:"tx_hashes"`
	}
	if _, err := postJSON(monerodURL+"/get_transaction_pool_hashes", map[string]any{}, &pool); err != nil {
		t.Fatal(err)
	}
	t.Logf("OBSERVED monero=reorg %.0f s after pop_blocks 1 (monerod from height %d to %d), before the next block: in monerod's pool %v; wallet-satbooth (auto-refresh only) at height %d, detached %v, get_transfer_by_txid %v (err %v); the merchant wallet after an explicit refresh: get_transfer_by_txid %v (code %d)",
		time.Since(popped).Seconds(), cursor, h, slices.Contains(pool.Hashes, x1.TxHash), vh, detached, view, verr, merch, code)
	s = e.start()
	time.Sleep(xmQuiet)
	ob := e.order(r1.ID)
	t.Logf("OBSERVED monero=reorg the restarted service before the next block reads %s / %s, %d confirmations, %d piconero confirmed, stale %v",
		ob.State, ob.Monero.State, ob.Monero.Confirmations, ob.Monero.ConfirmedPiconero, ob.Monero.Stale)
	mined := time.Now()
	e.mineXM(2)
	// Re-mined in the first new block: 2 confirmations once wallet-satbooth has the two blocks.
	d, o := e.waitXM(r1.ID, "paid after the plain reorg", mined, xmBound, func(o orderJSON) bool {
		return o.State == "paid" && o.Monero.State == "paid" && o.Monero.Confirmations == 2
	})
	t.Logf("MEASURED monero=reorg plain_reorg_mined_to_paid_seconds=%.2f (confirmations %d: the popped transaction was re-mined in the first new block)", d.Seconds(), o.Monero.Confirmations)
	time.Sleep(xmQuiet)
	nRev, nAgain := e.rcv.count(r1.ID, store.EventReverted), e.rcv.count(r1.ID, store.EventPaidAgain)
	t.Logf("OBSERVED monero=reorg plain: order.reverted delivered %d, order.paid_again delivered %d", nRev, nAgain)
	if nRev != nAgain {
		t.Errorf("order.reverted %d times but order.paid_again %d times", nRev, nAgain)
	}
	r1.Ev[store.EventReverted], r1.Ev[store.EventPaidAgain] = rng{nRev, nRev}, rng{nAgain, nAgain}

	r2, _ := e.newXOrder("xm-reorg-drop", xmT1, 0, 86400, map[string]rng{store.EventPaid: {1, 1}, store.EventReverted: {1, 1}}, nil)
	a2 := e.pressXM(r2)
	x2, sent := e.payXM(a2, xmT1, 0)
	e.waitXM(r2.ID, "seen", sent, xmBound, func(o orderJSON) bool { return o.Monero.State == "seen" })
	e.mineXM(1)
	e.waitXM(r2.ID, "paid", time.Now(), xmBound, func(o orderJSON) bool { return o.State == "paid" })
	e.waitHook(r2.ID, store.EventPaid, hookBound)
	if _, err := monerodPopBlocks(1); err != nil {
		t.Fatalf("pop_blocks 1: %v", shownErr(err))
	}
	e.flush(x2.TxHash)
	dropped := time.Now()
	e.mineXM(2)
	d, _ = e.waitXM(r2.ID, "reverted: new and waiting", dropped, xmBound, func(o orderJSON) bool {
		return o.State == "new" && o.Monero.State == "waiting" && o.Monero.ConfirmedPiconero == 0
	})
	t.Logf("MEASURED monero=reorg pop_and_flush_to_order_new_seconds=%.2f", d.Seconds())
	e.waitHook(r2.ID, store.EventReverted, hookBound)
	time.Sleep(xmQuiet)
	// One reversal for r2, and one for r1 exactly when its order.reverted was delivered. A wallet below the cursor logs
	// the height ALERT on each pass until it catches up: only when wallet-satbooth dropped the popped block before the
	// next one (r2's pop is followed by two blocks at once).
	behind := rng{0, 0}
	if detached {
		behind = rng{1, 1000}
	}
	e.finish(s, map[string]rng{alXmReorg: {1 + nRev, 1 + nRev}, alXmHeightBehind: behind})
}

// TestMoneroUnlockTime: a payment locked by its sender 1,000 blocks ahead cannot be made on regtest
// at v0.18.5.4: wallet-rpc's transfer refuses a non-zero unlock_time with -50 (WALLET_RPC_ERROR_CODE_NONZERO_UNLOCK_TIME,
// src/wallet/wallet_rpc_server.cpp:1266-1271), and monerod's pool refuses such a transaction from relay
// (src/cryptonote_core/tx_pool.cpp:185-191, tvc.m_nonzero_unlock_time; only a block may carry one, so only a miner can
// still pay with a lock). The phase asserts wallet-rpc's refusal, so a later release that allows it again fails here,
// and that nothing reached the order. The pool's refusal is cited, not asserted: no such transaction can be built
// through wallet-rpc. The late-by-lock rule itself is covered by monerotest.
func TestMoneroUnlockTime(t *testing.T) {
	e := newXmEnv(t, 1)
	s := e.start()
	r, _ := e.newXOrder("xm-unlock", xmT1, 0, 86400, nil, nil)
	a := e.pressXM(r)
	unlock := e.heightXM() + 1000
	e.ensureUnlocked(xmT1)
	err := e.cw.call(walletCustomer, "transfer", map[string]any{"destinations": []any{map[string]any{"address": a, "amount": xmT1}},
		"account_index": 0, "unlock_time": unlock, "get_tx_key": true}, nil)
	var xe *xmrError
	if !errors.As(err, &xe) || xe.Code != -50 {
		t.Fatalf("transfer with unlock_time tip+1000: %v; want the refusal -50 of wallet-rpc v0.18.5.4", shownErr(err))
	}
	t.Logf("OBSERVED monero=unlock transfer with unlock_time %d (tip+1000): refused with code %d (message of %d bytes); nothing was sent", unlock, xe.Code, len(xe.Message))
	e.mineXM(1)
	time.Sleep(xmQuiet)
	if o := e.order(r.ID); o.State != "new" || o.Monero.State != "waiting" || o.Monero.ReceivedPiconero != 0 || o.Monero.LatePiconero != 0 {
		t.Errorf("after the refused transfer the order reads %s / %+v", o.State, *o.Monero)
	}
	e.finish(s, nil)
}

// TestMoneroRestartBefore: a tier-3 order paid and mined once, then the service stops.
func TestMoneroRestartBefore(t *testing.T) {
	e := newXmEnv(t, 1)
	s := e.start()
	r, _ := e.newXOrder("xm-restart", xmT3, 0, 86400, nil, nil)
	a := e.pressXM(r)
	e.payXM(a, xmT3, 0)
	e.mineXM(1)
	_, o := e.waitXM(r.ID, "1 of 3", time.Now(), xmBound, func(o orderJSON) bool { return o.Monero.Confirmations == 1 })
	if o.State != "new" {
		t.Fatalf("the order reads %s at 1 of 3", o.State)
	}
	e.finish(s, nil)
}

// TestMoneroRestartAfter: two blocks while the service is down; no block is mined after the start. The assertion is that
// the restart catches up within three polls of the start; whether the first pass did it is logged (on_first_pass).
func TestMoneroRestartAfter(t *testing.T) {
	e := newXmEnv(t, 1)
	r := e.tagged("xm-restart")
	r.Ev = map[string]rng{store.EventPaid: {1, 1}}
	e.mineXM(2)
	// wallet-satbooth refreshes every 20 s: the phase waits until it has the two blocks, so the first pass can see them.
	cl := newViewClient(t)
	waited := waitViewHeight(t, cl, e.heightXM())
	cl.Zero()
	launched := time.Now()
	s := e.start()
	startDur := time.Since(launched)
	d, _ := e.waitXM(r.ID, "paid on the first pass", launched, xmBound, func(o orderJSON) bool { return o.State == "paid" && o.Monero.Confirmations == 3 })
	firstPass := startDur + xmPollSeconds*time.Second
	bound := startDur + 3*xmPollSeconds*time.Second
	t.Logf("MEASURED monero=restart launch_to_paid_seconds=%.2f start_seconds=%.2f on_first_pass=%v (paid before the second pass at %d s) bound_seconds=%.2f wallet_refresh_wait_seconds=%.1f",
		d.Seconds(), startDur.Seconds(), d < firstPass, xmPollSeconds, bound.Seconds(), waited.Seconds())
	if d > bound {
		t.Errorf("paid %.2f s after the launch, later than the restart's catch-up allows (%.2f s: three polls after the start, which took %.2f s)", d.Seconds(), bound.Seconds(), startDur.Seconds())
	}
	e.waitHook(r.ID, store.EventPaid, hookBound)
	time.Sleep(xmQuiet)
	e.finish(s, nil)
}

// xmKill is /state/monero/kill.json: the three orders the kill phase pressed and the highest index they got.
type xmKill struct {
	Orders []string `json:"orders"`
	Max    int64    `json:"max"`
}

// TestMoneroKillBefore: three orders pressed (their indices recorded). The script copied wallet-satbooth's files aside
// before this phase; it then kills wallet-satbooth and restores the copy: the wallet forgets the three subaddresses.
func TestMoneroKillBefore(t *testing.T) {
	e := newXmEnv(t, 1)
	s := e.start()
	var rec xmKill
	for i := range 3 {
		r, _ := e.newXOrder(fmt.Sprintf("xm-kill-%d", i), xmT1, 0, 86400, nil, nil)
		e.pressXM(r)
		rec.Orders = append(rec.Orders, r.ID)
	}
	e.finish(s, nil)
	var idx []int64
	e.withStore(func(ctx context.Context, st *store.Store) {
		for _, id := range rec.Orders {
			a, err := st.SubaddressForOrder(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			idx = append(idx, a.Index)
		}
		m, ok, err := st.MaxSubaddressIndex(ctx)
		if err != nil || !ok {
			t.Fatalf("MaxSubaddressIndex: %v %v", ok, shownErr(err))
		}
		rec.Max = m
	})
	if rec.Max != idx[2] || idx[0] >= idx[1] || idx[1] >= idx[2] {
		t.Fatalf("the three indices %v, the record's maximum %d: not rising to the maximum", idx, rec.Max)
	}
	t.Logf("OBSERVED monero=kill the three orders got indices %v; the record's maximum is %d", idx, rec.Max)
	writeJSON0600(t, filepath.Join(e.root, "kill.json"), rec)
}

// TestMoneroKillAfter: the restored wallet lacks the three subaddresses. The service's Check re-creates them (the
// ALERT with count 3); the wallet's subaddress at the recorded maximum equals the stored one; three new orders get
// three new, higher indices; a payment to a re-created subaddress is found.
func TestMoneroKillAfter(t *testing.T) {
	e := newXmEnv(t, 1)
	var rec xmKill
	if err := readJSONFile(filepath.Join(e.root, "kill.json"), &rec); err != nil {
		t.Fatalf("kill record (did KillBefore run?): %v", shownErr(err))
	}
	cl := newViewClient(t)
	var xe *xmrrpc.Error
	_, err := cl.GetAddress(context.Background(), 0, []uint32{uint32(rec.Max)})
	if !errors.As(err, &xe) || xe.Code != -15 {
		t.Fatalf("the restored wallet's get_address of the recorded maximum %d: %v; want -15 (the wallet forgot it)", rec.Max, shownErr(err))
	}
	t.Logf("OBSERVED monero=kill the restored wallet answers get_address of index %d with code %d", rec.Max, xe.Code)
	cl.Zero()
	s := e.start()
	logs := s.logs.String()
	if n := logCount(t, logs, alXmRecreated, "count", 3); n != 1 {
		t.Errorf("the re-creation ALERT with count 3 was logged %d times: %s", n, firstLines(logs, 8))
	}
	var rs []*ocOrder
	for i := range 3 {
		r, _ := e.newXOrder(fmt.Sprintf("xm-kill-after-%d", i), xmT1, 0, 86400, nil, nil)
		e.pressXM(r)
		rs = append(rs, r)
	}
	paid := e.byID(rec.Orders[0])
	paid.Ev = map[string]rng{store.EventPaid: {1, 1}}
	_, sent := e.payXM(paid.Sub, xmT1, 0)
	e.waitXM(paid.ID, "seen", sent, xmBound, func(o orderJSON) bool { return o.Monero.State == "seen" })
	mined := time.Now()
	e.mineXM(1)
	d, _ := e.waitXM(paid.ID, "paid", mined, xmBound, func(o orderJSON) bool { return o.State == "paid" })
	t.Logf("MEASURED monero=kill block_to_paid_seconds=%.2f (a re-created subaddress)", d.Seconds())
	e.waitHook(paid.ID, store.EventPaid, hookBound)
	time.Sleep(xmQuiet)
	e.finish(s, map[string]rng{alXmRecreated: {1, 1}})

	var stored string
	var newIdx []int64
	e.withStore(func(ctx context.Context, st *store.Store) {
		a, err := st.SubaddressForOrder(ctx, rec.Orders[2])
		if err != nil || a.Index != rec.Max {
			t.Fatalf("the stored subaddress at the maximum: index %d, %v", a.Index, shownErr(err))
		}
		stored = a.Address
		prev := rec.Max
		for _, r := range rs {
			a, err := st.SubaddressForOrder(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if a.Index <= prev {
				t.Errorf("order %s got index %d after %d", r.Tag, a.Index, prev)
			}
			prev = a.Index
			newIdx = append(newIdx, a.Index)
		}
	})
	cl = newViewClient(t)
	got, err := cl.GetAddress(context.Background(), 0, []uint32{uint32(rec.Max)})
	if err != nil || len(got.Addresses) != 1 || got.Addresses[0].Address != stored {
		t.Fatalf("the re-created wallet's subaddress at %d differs from the stored one (%v)", rec.Max, shownErr(err))
	}
	t.Logf("OBSERVED monero=kill after the re-creation get_address of index %d equals the stored subaddress (inferred determinism: observed); new indices %v", rec.Max, newIdx)
}

// TestMoneroLateBefore: an order with a short expiry, pressed, nothing paid.
func TestMoneroLateBefore(t *testing.T) {
	e := newXmEnv(t, 1)
	s := e.start()
	r, _ := e.newXOrder("xm-late", xmT1, 0, xmShort, nil, map[string]int{store.EventExpired: 1})
	e.pressXM(r)
	if e.order(r.ID).State != "new" {
		t.Fatal("the order is not new")
	}
	e.finish(s, nil)
}

// TestMoneroStuckBefore: in its own environment, an order with a short expiry paid on time and left in the pool.
func TestMoneroStuckBefore(t *testing.T) {
	e := newXmEnvAt(t, xmStuckName, 0, nil, 1)
	s := e.start()
	r, _ := e.newXOrder("xm-stuck", xmT1, 0, xmShort, nil, nil)
	a := e.pressXM(r)
	x, sent := e.payXM(a, xmT1, 0)
	r.TxID = x.TxHash
	e.saveReg()
	_, o := e.waitXM(r.ID, "seen", sent, xmBound, func(o orderJSON) bool { return o.Monero.State == "seen" && o.Monero.ReceivedPiconero == xmT1 })
	if o.Monero.Confirmations != 0 || o.State != "new" {
		t.Errorf("seen reads %s / %+v", o.State, *o.Monero)
	}
	if left := time.Until(time.Unix(r.Expiry, 0)); left < 2*time.Minute {
		t.Fatalf("only %.0f s left before the real expiry: the After phase must start well before it", left.Seconds())
	}
	e.finish(s, nil)
}

// TestMoneroStuckAfter: the same order with the service's clock 7 days and 10 minutes ahead, past its watch window,
// its payment still in the pool: order.underpaid at the window end (the payment stays tracked); then mined, it pays the
// order after the final event.
func TestMoneroStuckAfter(t *testing.T) {
	e := newXmEnvAt(t, xmStuckName, 7*24*time.Hour+10*time.Minute, nil, 1)
	r := e.tagged("xm-stuck")
	if r.TxID == "" {
		t.Fatal("the stuck order has no txid (did StuckBefore run?)")
	}
	r.Ev = map[string]rng{store.EventUnderpaid: {1, 1}, store.EventPaid: {1, 1}}
	inPool := func(label string) bool {
		var pool struct {
			Hashes []string `json:"tx_hashes"`
		}
		if _, err := postJSON(monerodURL+"/get_transaction_pool_hashes", map[string]any{}, &pool); err != nil {
			t.Fatal(err)
		}
		found := slices.Contains(pool.Hashes, r.TxID)
		t.Logf("OBSERVED monero=stuck %s: in monerod's pool %v", label, found)
		return found
	}
	if !inPool("before the service starts") {
		t.Error("the stuck payment is not in monerod's pool before the service starts")
	}
	s := e.start()
	d, o := e.waitXM(r.ID, "underpaid at the window end", e.runStart, 3*time.Minute, func(o orderJSON) bool { return o.State == "underpaid" })
	if o.Monero.ReceivedPiconero != xmT1 || o.Monero.Confirmations != 0 || o.Monero.ConfirmedPiconero != 0 {
		t.Errorf("underpaid at the window end reads %+v", *o.Monero)
	}
	t.Logf("MEASURED monero=stuck window_pass_to_underpaid_seconds=%.2f (from the service start; its first pass runs at once)", d.Seconds())
	e.waitHook(r.ID, store.EventUnderpaid, hookBound)
	e.pageHas(r.Token, "not yet confirmed enough")
	if !inPool("after order.underpaid") {
		t.Error("the stuck payment left monerod's pool")
	}
	mined := time.Now()
	e.mineXM(1)
	d, o = e.waitXM(r.ID, "paid", mined, xmBound, func(o orderJSON) bool { return o.State == "paid" })
	if o.Monero.Confirmations != 1 || o.Monero.ConfirmedPiconero != xmT1 || o.Monero.State != "paid" {
		t.Errorf("paid after the window reads %+v", *o.Monero)
	}
	t.Logf("MEASURED monero=stuck block_to_paid_seconds=%.2f", d.Seconds())
	e.waitHook(r.ID, store.EventPaid, hookBound)
	// Past the deepest tier (10), the payment is settled and no longer watched.
	e.mineXM(10)
	e.waitXM(r.ID, "11 confirmations", time.Now(), xmBound, func(o orderJSON) bool { return o.Monero.Confirmations >= 11 })
	time.Sleep(xmQuiet)
	e.finish(s, map[string]rng{alUnderpaidWindow: {1, 1}, alPaidAfterFinal: {1, 1}})
	e.withStore(func(ctx context.Context, st *store.Store) {
		w, err := st.WatchedPayments(ctx, e.now())
		if err != nil {
			t.Fatal(err)
		}
		settled, err := st.MoneroSettledKeys(ctx, []store.PaymentKey{{TxID: r.TxID, Address: r.Sub}})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("OBSERVED monero=stuck after 11 confirmations: %d payments still watched, the stuck one settled %v", len(w), settled[store.PaymentKey{TxID: r.TxID, Address: r.Sub}])
		if len(w) != 0 || !settled[store.PaymentKey{TxID: r.TxID, Address: r.Sub}] {
			t.Errorf("%d payments still watched, the stuck one settled %v; want none and settled", len(w), settled)
		}
	})
}

// TestMoneroLateAfter: after the short expiry, the unpaid order reads expired, then a payment makes it late and never
// paid; the 60 % order is underpaid, then late after its top-up.
func TestMoneroLateAfter(t *testing.T) {
	e := newXmEnv(t, 1)
	late, part := e.tagged("xm-late"), e.tagged("xm-partial-short")
	for _, o := range []*ocOrder{late, part} {
		if wait := time.Until(time.Unix(o.Expiry, 0).Add(time.Second)); wait > 0 {
			t.Logf("waiting %.0f s for the expiry of %s", wait.Seconds(), o.Tag)
			time.Sleep(wait)
		}
	}
	snap := e.snapshot()
	s := e.start()
	hadExpired := snap[evKey{late.ID, store.EventExpired, 1}]
	hadUnderpaid := snap[evKey{part.ID, store.EventUnderpaid, 1}]
	e.waitXM(late.ID, "expired", e.runStart, 3*time.Minute, func(o orderJSON) bool { return o.State == "expired" })
	e.waitXM(part.ID, "underpaid", e.runStart, 3*time.Minute, func(o orderJSON) bool { return o.State == "underpaid" })
	if !hadExpired {
		e.waitHook(late.ID, store.EventExpired, hookBound)
	}
	if !hadUnderpaid {
		e.waitHook(part.ID, store.EventUnderpaid, hookBound)
	}
	e.pageHas(late.Token, ">Expired<")
	e.pageHas(part.Token, ">Underpaid<")

	// Late payments: first seen after the expiry. Their order.late comes once they are mined at the tier.
	late.Ev = map[string]rng{store.EventLate: {1, 1}}
	part.Ev = map[string]rng{store.EventLate: {1, 1}}
	_, sent := e.payXM(late.Sub, xmT1, 0)
	e.waitXM(late.ID, "late seen", sent, xmBound, func(o orderJSON) bool { return o.Monero.LatePiconero == xmT1 })
	e.payXM(part.Sub, xmT1*40/100, 0)
	time.Sleep(xmQuiet)
	for _, o := range []*ocOrder{late, part} {
		if n := e.rcv.countSeq(o.ID, store.EventLate, 1); n != 0 {
			t.Errorf("%s: order.late arrived %d times before its payment was mined", o.Tag, n)
		}
	}
	mined := time.Now()
	e.mineXM(1)
	for _, o := range []*ocOrder{late, part} {
		d, got := e.waitXM(o.ID, o.Tag+" late", mined, xmBound, func(g orderJSON) bool {
			if g.State == "paid" {
				t.Fatalf("%s reads paid: a late payment must never pay an expired order", o.Tag)
			}
			return g.State == "late"
		})
		t.Logf("MEASURED monero=late tag=%s block_to_late_seconds=%.2f late_piconero=%d", o.Tag, d.Seconds(), got.Monero.LatePiconero)
		e.waitHook(o.ID, store.EventLate, hookBound)
		e.pageHas(o.Token, ">Late<")
	}
	time.Sleep(xmQuiet)
	for _, o := range []*ocOrder{late, part} {
		if g := e.order(o.ID); g.State != "late" || g.PaidAt != nil {
			t.Errorf("%s reads %s after the quiet period", o.Tag, g.State)
		}
	}
	e.finish(s, nil)
}

// xmFiatBTC are the fake's BTC rates of TestMoneroFiat: Kraken, Coinbase and Bitfinex in USD and EUR. The phase sets XMR
// in USD from Bitfinex (250) and Kraken (251), and in EUR from Kraken alone.
var xmFiatBTC = fiatTable{
	"USD": {"kraken": "76000", "coinbase": "76100", "bitfinex": "76050"},
	"EUR": {"kraken": "67000", "coinbase": "67100", "bitfinex": "67050"},
}

// TestMoneroFiat: a priced order on the on-chain and Monero rails gets both amounts from one quote; XMR in EUR is
// unreachable (400 invalid_currency, no ALERT) while BTC in EUR is fine; a Bitfinex outage makes XMR in USD a 503 with
// the ALERT; the first order is paid in Monero and its webhook carries the pricing.
func TestMoneroFiat(t *testing.T) {
	fake := ratestest.New(t)
	seedFiat(fake, xmFiatBTC)
	fake.SetAsset("bitfinex", "XMR", "USD", "250")
	fake.SetAsset("kraken", "XMR", "USD", "251")
	fake.SetAsset("kraken", "XMR", "EUR", "231")
	rc := &service.RatesConfig{Providers: []string{"kraken", "coinbase", "bitfinex"}, Currencies: []string{"USD", "EUR"},
		MarginPercent: fiatMargin, MinSources: 2, MaxSpreadPercent: "2", MaxAgeSeconds: 60, FetchSeconds: 10}
	e := newXmEnvAt(t, xmName, 0, rc, 1)
	if e.setup.Descriptor == "" {
		t.Fatal("the on-chain rail is not configured (did TestOnchainSetup run?)")
	}
	e.rateRT = fake.Transport()
	s := e.start()
	rails := []string{"onchain", "monero"}
	var o orderJSON
	var extra []orderJSON
	ref := "xm-fiat-" + randHex(t)[:12]
	poll(t, "a priced order from every rate", fiatQuoteBound, 200*time.Millisecond, func() (bool, error) {
		got, status, code := e.createFiatOrder(ref, "25.00", "USD", rails, 86400)
		switch {
		case status == 503 && code == "rate_unavailable":
			return false, errors.New("503 rate_unavailable")
		case status != 201:
			t.Fatalf("priced order: status %d, code %q, want 201", status, code)
		case got.Pricing == nil:
			t.Fatal("a priced order has no pricing")
		case len(got.Pricing.Rates["BTC"].Sources) != 3 || len(got.Pricing.Rates["XMR"].Sources) != 2:
			extra = append(extra, got)
			ref = "xm-fiat-" + randHex(t)[:12]
			return false, errors.New("priced from fewer rates")
		}
		o = got
		return true, nil
	})
	for _, x := range extra {
		e.registerX(x, "xm-fiat-extra", nil, nil)
	}
	ro := e.registerX(o, "xm-fiat", map[string]rng{store.EventPaid: {1, 1}}, nil)
	price, _ := rates.ParseDecimal("25.00")
	margin, err := rates.ParsePercent(fiatMargin)
	if err != nil {
		t.Fatal(err)
	}
	xmrRate := fiatMedian(t, map[string]string{"bitfinex": "250", "kraken": "251"})
	pico, err := rates.Piconero(price, xmrRate, margin)
	if err != nil {
		t.Fatal(err)
	}
	sat := fiatSats(t, "25.00", fiatMedian(t, xmFiatBTC["USD"]))
	p := o.Pricing
	if xmrRate.String() != "250.5" || o.MoneroPiconero != pico || o.OnchainSat != sat || o.Msat != 0 || o.Lightning != nil ||
		len(p.Rates) != 2 || p.Rates["XMR"].Rate != "250.5" || p.Rates["BTC"].Rate != fiatMedian(t, xmFiatBTC["USD"]).String() {
		t.Fatalf("the priced order: monero_piconero %d (want %d), onchain_sat %d (want %d), lightning_msat %d, rates %v", o.MoneroPiconero, pico, o.OnchainSat, sat, o.Msat, p.Rates)
	}
	t.Logf("OBSERVED monero=fiat 25.00 USD with a %s %% margin: monero_piconero %d at XMR/USD %s (Bitfinex 250, Kraken 251), onchain_sat %d at BTC/USD %s; pricing.rates has %d assets",
		fiatMargin, o.MoneroPiconero, p.Rates["XMR"].Rate, o.OnchainSat, p.Rates["BTC"].Rate, len(p.Rates))

	// XMR in EUR: unreachable with Kraken and Bitfinex (Kraken alone quotes it): a permanent 400, no Retry-After.
	status, code, ra := e.apiErr("POST", "/v1/orders", e.writeKey, map[string]any{"account_id": acctID,
		"merchant_ref": "xm-fiat-eur-" + randHex(t)[:12], "price": "25.00", "currency": "EUR", "rails": rails, "expires_in": 86400})
	if status != 400 || code != "invalid_currency" || ra != "" {
		t.Fatalf("a priced EUR order naming monero: status %d, code %q, Retry-After %q; want 400 invalid_currency and none", status, code, ra)
	}
	oe, status, code := e.createFiatOrder("xm-fiat-eur-btc-"+randHex(t)[:12], "25.00", "EUR", []string{"onchain"}, 86400)
	if status != 201 || oe.Monero != nil || oe.Onchain == nil {
		t.Fatalf("a priced EUR order on the on-chain rail: status %d, code %q; want 201", status, code)
	}
	e.register(oe, "xm-fiat-eur-btc", nil)
	t.Logf("OBSERVED monero=fiat EUR naming monero: 400 invalid_currency, no Retry-After; EUR on the on-chain rail alone: 201")

	// A Bitfinex outage: XMR in USD falls to one source, past max_age.
	fake.Fail("bitfinex", 500)
	failed := time.Now()
	var raOut string
	// The probe of TestFiatOutage, which creates nothing: a price no rail can hold (fiatTooBig), on the Monero rail alone
	// (the API quotes BTC first, so a BTC rail would answer for BTC). While Bitfinex's last rate counts it is converted
	// and refused, 400 invalid_monero_piconero; once the rate is max_age old, 503 rate_unavailable.
	probes := 0
	poll(t, "XMR in USD unavailable", 3*time.Minute, time.Second, func() (bool, error) {
		st, c, r := e.apiErr("POST", "/v1/orders", e.writeKey, map[string]any{"account_id": acctID,
			"merchant_ref": "xm-fiat-probe-" + randHex(t)[:12], "price": fiatTooBig, "currency": "USD", "rails": []string{"monero"}, "expires_in": 86400})
		probes++
		switch {
		case st == 503 && c == "rate_unavailable":
			raOut = r
			return true, nil
		case st == 400 && c == "invalid_monero_piconero":
			return false, errors.New("400 invalid_monero_piconero: the cached rate still counts")
		}
		t.Fatalf("the probe: status %d code %q, want 400 invalid_monero_piconero or 503 rate_unavailable", st, c)
		return false, nil
	})
	t.Logf("OBSERVED monero=fiat the probe answered 400 invalid_monero_piconero until the 503 (%d probes, no order created)", probes)
	t.Logf("MEASURED monero=fiat outage_to_503_seconds=%.2f (max_age 60 s, fetch every 10 s) Retry-After %q", time.Since(failed).Seconds(), raOut)
	if raOut == "" {
		t.Error("the outage's 503 has no Retry-After")
	}
	poll(t, "the ALERT naming XMR USD", 30*time.Second, 500*time.Millisecond, func() (bool, error) {
		for _, r := range fiatRecords(t, s.logs.String()) {
			if r.Msg == alFiat && r.Asset == "XMR" && r.Currency == "USD" {
				return true, nil
			}
		}
		return false, errors.New("no ALERT naming XMR USD yet")
	})
	fake.Fail("bitfinex", 0)
	// The recovery line (rates/service.go logState: none on the first pass of a healthy start).
	poll(t, "XMR in USD available again", 2*time.Minute, 500*time.Millisecond, func() (bool, error) {
		for _, r := range fiatRecords(t, s.logs.String()) {
			if r.Msg == lnFiatAvailable && r.Asset == "XMR" && r.Currency == "USD" {
				return true, nil
			}
		}
		return false, errors.New("no available line for XMR USD yet")
	})

	// The first order, paid in Monero.
	sub := e.pressXM(ro)
	_, sent := e.payXM(sub, pico, 0)
	mined := time.Now()
	e.mineXM(1)
	d, got := e.waitXM(ro.ID, "paid", mined, xmBound, func(o orderJSON) bool { return o.State == "paid" && o.Monero.ConfirmedPiconero == pico })
	t.Logf("MEASURED monero=fiat send_to_paid_seconds=%.2f block_to_paid_seconds=%.2f", time.Since(sent).Seconds(), d.Seconds())
	if !reflect.DeepEqual(got.Pricing, o.Pricing) {
		t.Fatal("the paid order's pricing differs from the one it was created with")
	}
	e.waitHook(ro.ID, store.EventPaid, hookBound)
	e.checkHookPricing(o, store.EventPaid)
	time.Sleep(xmQuiet)
	// One rate fetch failed WARN per provider and asset (rates/service.go: each asset has its own run of failures):
	// Bitfinex for BTC and for XMR.
	e.finish(s, map[string]rng{alFiat: {1, 1}, wnFetchFailed: {2, 2}})
	var failedAssets []string
	for _, r := range fiatRecords(t, s.logs.String()) {
		if r.Msg == lnFetchFailed {
			if r.Provider != "bitfinex" || r.Class != "status" || r.Status != 500 {
				t.Errorf("rate fetch failed: provider %q class %q status %d, want bitfinex, status, 500", r.Provider, r.Class, r.Status)
			}
			failedAssets = append(failedAssets, r.Asset)
		}
	}
	if slices.Sort(failedAssets); !slices.Equal(failedAssets, []string{"BTC", "XMR"}) {
		t.Errorf("rate fetch failed for the assets %v, want BTC and XMR once each", failedAssets)
	}
	if strings.Contains(s.logs.String(), ratestest.Sentinel) {
		t.Error("a provider's body reached the service's log")
	}
	for _, r := range fiatRecords(t, s.logs.String()) {
		if r.Msg == alFiat && r.Asset == "XMR" && r.Currency == "EUR" {
			t.Error("the log holds a fiat ALERT naming XMR EUR, an unreachable pair")
		}
		if r.Msg == alFiat && (r.Asset != "XMR" || r.Currency != "USD") {
			t.Errorf("a fiat ALERT names %s %s", r.Asset, r.Currency)
		}
	}
}

// TestMoneroBothRails: a mixed on-chain and Monero order paid on both (on-chain first, then Monero, mined): paid once,
// the both-rails ALERT, and the page's refund line.
func TestMoneroBothRails(t *testing.T) {
	e := newXmEnv(t, 1)
	if e.setup.Descriptor == "" {
		t.Fatal("the on-chain rail is not configured (did TestOnchainSetup run?)")
	}
	s := e.start()
	r, o := e.newXOrder("xm-both", xmT1, satT1, 86400, map[string]rng{store.EventPaid: {1, 1}}, nil)
	if o.Onchain == nil || o.Onchain.RequiredConfirmations != 1 {
		t.Fatalf("the mixed order's on-chain side: %+v", o.Onchain)
	}
	if _, page := e.page(r.Token); !strings.Contains(page, `name="rail" value="bitcoin"`) || !strings.Contains(page, `name="rail" value="monero"`) {
		t.Fatal("the mixed order's page does not offer both rails")
	}
	addr, _ := e.press(r, false)
	sub := e.pressXM(r)
	_, sent := e.ocEnv.pay(addr, satT1)
	e.waitOC(r.ID, "seen on-chain", sent, ocBound, func(o orderJSON) bool { return o.Onchain.State == "seen" })
	e.ocEnv.mine(1)
	e.waitOC(r.ID, "paid on-chain", time.Now(), ocBound, func(o orderJSON) bool { return o.State == "paid" && o.Onchain.State == "paid" })
	e.waitHook(r.ID, store.EventPaid, hookBound)
	_, sent = e.payXM(sub, xmT1, 0)
	mined := time.Now()
	e.mineXM(1)
	d, got := e.waitXM(r.ID, "the Monero payment recorded", mined, xmBound, func(o orderJSON) bool { return o.Monero.ConfirmedPiconero == xmT1 })
	t.Logf("MEASURED monero=both block_to_recorded_seconds=%.2f; the order reads %s, on-chain %s, Monero %s", d.Seconds(), got.State, got.Onchain.State, got.Monero.State)
	if got.State != "paid" {
		t.Errorf("the order paid on both rails reads %s", got.State)
	}
	e.pageHas(r.Token, "The merchant will refund the extra.")
	time.Sleep(xmQuiet)
	if n := e.rcv.count(r.ID, store.EventPaid); n != 1 {
		t.Errorf("order.paid delivered %d times", n)
	}
	e.finish(s, map[string]rng{alBothRails: {1, 2}})
}
