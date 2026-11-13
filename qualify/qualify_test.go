//go:build qualify

package qualify

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/nwc"
)

// Hub behaviour asserted here is pinned to getAlby/hub 921003b (image v1.24.6); each subtest names the file it
// relies on. Where the hub differs from what was expected, the test asserts what the hub does and
// qualify/REPORT.md records the difference.

// notifLog collects a client's notifications so that a test can wait for one by type and payment hash while
// others pass by.
type notifLog struct {
	mu   sync.Mutex
	got  map[string]time.Time // type + "/" + payment hash -> arrival
	last map[string]nwc.Notification
	ch   chan struct{}
}

func newNotifLog(c *nwc.Client) *notifLog {
	l := &notifLog{got: map[string]time.Time{}, last: map[string]nwc.Notification{}, ch: make(chan struct{}, 1)}
	go func() {
		for n := range c.Notifications() {
			l.mu.Lock()
			k := n.Type + "/" + n.Tx.PaymentHash
			if _, dup := l.got[k]; !dup {
				l.got[k] = time.Now()
				l.last[k] = n
			}
			l.mu.Unlock()
			select {
			case l.ch <- struct{}{}:
			default:
			}
		}
	}()
	return l
}

// wait returns the arrival time of the (type, hash) notification, within d.
func (l *notifLog) wait(t testing.TB, typ, hash string, d time.Duration) (time.Time, nwc.Notification) {
	t.Helper()
	end := time.NewTimer(d)
	defer end.Stop()
	for {
		l.mu.Lock()
		at, ok := l.got[typ+"/"+hash]
		n := l.last[typ+"/"+hash]
		l.mu.Unlock()
		if ok {
			return at, n
		}
		select {
		case <-l.ch:
		case <-end.C:
			t.Fatalf("no %s notification for the payment within %s", typ, d)
		}
	}
}

// has reports whether a (type, hash) notification has arrived.
func (l *notifLog) has(typ, hash string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.got[typ+"/"+hash]
	return ok
}

type world struct {
	st                 *state
	sat, mer           *hubAPI
	inv, pay, mc, info *nwc.Client
	invN, payN, mcN    *notifLog
	keys               map[string]*nostr.SecretKey
	payoutUsedBaseMsat int64
}

func ctxFor(t testing.TB, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func newPreimage(t testing.TB) (preimage, hash string) {
	t.Helper()
	var p [32]byte
	if _, err := rand.Read(p[:]); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(p[:])
	return hex.EncodeToString(p[:]), hex.EncodeToString(h[:])
}

func hashOf(preimageHex string) string {
	b, _ := hex.DecodeString(preimageHex)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// wantWalletError fails unless err is a *nwc.WalletError with this code; it logs the observed code and message
// and checks the message contains want (the hub's wording).
func wantWalletError(t *testing.T, label string, err error, code, want string) *nwc.WalletError {
	t.Helper()
	var we *nwc.WalletError
	if !errors.As(err, &we) {
		t.Fatalf("%s: want a WalletError %s, got %v", label, code, shownErr(err))
	}
	t.Logf("MEASURED wallet_error label=%s code=%s message_bytes=%d", label, shownCode(we.Code), len(we.Message))
	if we.Code != code {
		t.Fatalf("%s: code %s, want %q (message of %d bytes)", label, shownCode(we.Code), code, len(we.Message))
	}
	if want != "" && !strings.Contains(we.Message, want) {
		t.Fatalf("%s: the message (%d bytes) does not contain %q", label, len(we.Message), want)
	}
	return we
}

// TestQualify runs the NWC protocol checks against the two hubs through SatBooth's relays. The subtests run in
// order and share the world TestBootstrap built.
func TestQualify(t *testing.T) {
	w := newWorld(t, true, true)

	t.Run("InfoEvent", w.infoEvent)
	t.Run("GetInfo", w.getInfo)
	t.Run("ReceivePayment", w.receivePayment)
	t.Run("HubWideNotifications", w.hubWideNotifications)
	t.Run("UnknownKey", w.unknownKey)
	t.Run("PayInvoice", w.payInvoice)
	t.Run("PayTwice", w.payTwice)
	t.Run("AmountMismatch", w.amountMismatch)
	t.Run("Expired", w.expired)
	t.Run("BudgetReserve", w.budgetReserve)
	t.Run("InvoiceCannotPay", w.invoiceCannotPay)
	t.Run("ListPageSize", w.listPageSize)
}

// newWorld loads the state TestBootstrap saved, logs in to both hubs and dials the NWC connections. With waitChannel
// it first waits until the channel is active on both hubs (it must be again after a hub restart, before any payment).
func newWorld(t *testing.T, waitChannel, dial bool) *world {
	t.Helper()
	st, err := loadState(stateDir(t))
	if err != nil {
		t.Fatalf("state (did TestBootstrap run?): %v", shownErr(err))
	}
	w := &world{st: st, keys: map[string]*nostr.SecretKey{}}
	w.sat = newHubAPI(st.Hubs[hubSatbooth].URL, st.Hubs[hubSatbooth].Token)
	w.mer = newHubAPI(st.Hubs[hubMerchant].URL, st.Hubs[hubMerchant].Token)
	w.sat.ensureToken(t)
	w.mer.ensureToken(t)
	if waitChannel {
		w.waitChannel(t)
	}
	if dial {
		w.dialAll(t)
	}
	return w
}

// waitChannel waits for the channel to be active on both hubs, reconnecting the peer link meanwhile.
func (w *world) waitChannel(t *testing.T) {
	t.Helper()
	st := w.st
	// After hub restarts the channel must be active again before any payment.
	poll(t, "channel active on both hubs", 3*time.Minute, 2*time.Second, func() (bool, error) {
		for _, h := range []*hubAPI{w.sat, w.mer} {
			chs, err := h.channels()
			if err != nil {
				return false, err
			}
			if len(chs) == 0 || !chs[0].Active {
				_ = w.mer.connectPeer(st.Hubs[hubSatbooth].NodeID, hubIP[hubSatbooth], 9735) // reconnect, harmless when connected
				return false, errors.New("channel not active")
			}
		}
		return true, nil
	})

}

func (w *world) dialAll(t *testing.T) {
	t.Helper()
	st := w.st
	dial := func(name, relayHub, wallet string, timeout time.Duration) *nwc.Client {
		t.Helper()
		k := st.key(t, name)
		w.keys[name] = k
		c, err := nwc.Dial(ctxFor(t, time.Minute), nwc.Config{RelayURL: st.RelayURLs[relayHub], WalletPubKey: st.Wallets[wallet], Key: k, Timeout: timeout})
		if err != nil {
			t.Fatalf("dial %s: %v", name, shownErr(err))
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	w.inv = dial("invoice", hubSatbooth, "invoice", 0)
	w.pay = dial("payout", hubSatbooth, "payout", 0)
	w.mc = dial("merchant", hubMerchant, "merchant", 0)
	w.info = dial("info", hubSatbooth, "info", 0)
	w.invN, w.payN, w.mcN = newNotifLog(w.inv), newNotifLog(w.pay), newNotifLog(w.mc)
}

// infoEvent: the relay accepted each connection's 13194, so a client sees it. The hub retries a refused publish
// with growing backoff (getAlby/hub 921003b nip47/nip47_service.go:109-135), hence the 3 minute bound.
func (w *world) infoEvent(t *testing.T) {
	cases := []struct{ key, relayHub, wallet, want string }{
		{"invoice", hubSatbooth, "invoice", "make_invoice"},
		{"payout", hubSatbooth, "payout", "pay_invoice"},
		{"merchant", hubMerchant, "merchant", "make_invoice"},
	}
	for _, c := range cases {
		began := time.Now()
		ctx := ctxFor(t, 3*time.Minute)
		rc, err := dialRaw(ctx, w.st.RelayURLs[c.relayHub], w.keys[c.key])
		if err != nil {
			t.Fatalf("%s: raw dial: %v", c.key, shownErr(err))
		}
		defer rc.close()
		wallet := w.st.Wallets[c.wallet]
		if err := rc.req(ctx, "info", `{"kinds":[13194],"authors":["`+wallet+`"]}`); err != nil {
			t.Fatal(err)
		}
		ev, err := rc.nextEvent(ctx, "info")
		if err != nil {
			t.Fatalf("%s: no 13194 within 3 minutes: %v", c.key, shownErr(err))
		}
		if ev.Kind != nostr.KindNWCInfo || ev.PubKey != wallet {
			t.Fatalf("%s: unexpected event kind %d", c.key, ev.Kind)
		}
		if !strings.Contains(" "+ev.Content+" ", " "+c.want+" ") {
			t.Fatalf("%s: 13194 content (%s) lacks %s", c.key, names(strings.Fields(ev.Content)), c.want)
		}
		var tagNames []string
		for _, tg := range ev.Tags {
			tagNames = append(tagNames, tg[0])
		}
		t.Logf("MEASURED info_event conn=%s seconds_to_see=%.1f content(%s) tags(%s)", c.key, time.Since(began).Seconds(), names(strings.Fields(ev.Content)), names(tagNames))
	}
}

// netName is what a test may print of a hub's network string: the four network names, "(none)" for an empty one and
// "(unknown)" for anything else (a hub's own strings are never printed).
func netName(s string) string {
	switch s {
	case "mainnet", "testnet", "signet", "regtest":
		return s
	case "":
		return "(none)"
	}
	return "(unknown)"
}

// nip47Names is the fixed list of NIP-47 method and notification names SatBooth knows: the only strings of a hub's
// get_info answer or info event that a test prints.
var nip47Names = []string{
	"pay_invoice", "make_invoice", "lookup_invoice", "list_transactions", "get_info", "get_balance", "pay_keysend",
	"multi_pay_invoice", "multi_pay_keysend", "make_hold_invoice", "settle_hold_invoice", "cancel_hold_invoice",
	"sign_message", "notifications", "payment_received", "payment_sent", "hold_invoice_accepted",
}

// names is what a test may print of a list of strings a hub returned: the count, the names that are on nip47Names (in
// the list's order, each once) and the number of others, which are not printed (a hub's own strings are never printed).
func names(list []string) string {
	var known []string
	other := 0
	for _, x := range list {
		switch {
		case !contains(nip47Names, x):
			other++
		case !contains(known, x):
			known = append(known, x)
		}
	}
	return fmt.Sprintf("count=%d known=%v other=%d", len(list), known, other)
}

func (w *world) getInfo(t *testing.T) {
	in, err := w.inv.GetInfo(ctxFor(t, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURED get_info conn=invoice network=%q methods(%s) notifications(%s) block_height=%d", netName(in.Network), names(in.Methods), names(in.Notifications), in.BlockHeight)
	for _, m := range []string{"make_invoice", "lookup_invoice", "list_transactions", "get_info", "get_budget"} {
		if !contains(in.Methods, m) {
			t.Errorf("invoice connection lacks method %s", m)
		}
	}
	if contains(in.Methods, "pay_invoice") {
		t.Error("invoice connection offers pay_invoice")
	}
	if !contains(in.Notifications, "payment_received") {
		t.Errorf("invoice connection lacks the payment_received notification: notifications(%s)", names(in.Notifications))
	}
	// The invoice connection holds the get_info scope, as the SatBooth link asks: the hub states its network, which the
	// service's start-up check compares with nwc.network (without the scope the hub withholds it, getAlby/hub 921003b
	// nip47/controllers/get_info_controller.go:69-102).
	if in.Network != "regtest" {
		t.Errorf("invoice connection (get_info scope) reports network %q, want regtest", netName(in.Network))
	}
	out, err := w.pay.GetInfo(ctxFor(t, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURED get_info conn=payout network=%q methods(%s) notifications(%s)", netName(out.Network), names(out.Methods), names(out.Notifications))
	if !contains(out.Methods, "pay_invoice") || contains(out.Methods, "make_invoice") {
		t.Errorf("payout methods(%s)", names(out.Methods))
	}
	if out.Network != "regtest" {
		t.Errorf("payout connection (get_info scope) reports network %q, want regtest", netName(out.Network))
	}
	full, err := w.info.GetInfo(ctxFor(t, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURED get_info conn=info(get_info scope) network=%q block_height=%d", netName(full.Network), full.BlockHeight)
	if full.Network != "regtest" {
		t.Fatalf("network %q, want regtest", netName(full.Network))
	}
	if full.BlockHeight <= 0 {
		t.Errorf("block height %d", full.BlockHeight)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (w *world) receivePayment(t *testing.T) {
	const amountMsat = 10_000_000
	tx, err := w.inv.MakeInvoice(ctxFor(t, time.Minute), nwc.MakeInvoiceParams{AmountMsat: amountMsat, Description: "receive test"})
	if err != nil {
		t.Fatal(err)
	}
	if tx.Type != nwc.TypeIncoming || tx.AmountMsat != amountMsat || tx.PaymentHash == "" {
		t.Fatalf("make_invoice answered %+v", tx)
	}
	began := time.Now()
	if _, err := w.mer.payInvoice(tx.Invoice); err != nil {
		t.Fatalf("customer payment: %v", shownErr(err))
	}
	payTook := time.Since(began)
	at, n := w.invN.wait(t, nwc.NotifPaymentReceived, tx.PaymentHash, 30*time.Second)
	t.Logf("MEASURED receive customer_payment_seconds=%.2f seconds_pay_start_to_notification=%.2f", payTook.Seconds(), at.Sub(began).Seconds())
	if n.Tx.AmountMsat != amountMsat || n.Tx.State != nwc.StateSettled {
		t.Errorf("notification transaction %+v", n.Tx)
	}
	got, err := w.inv.LookupInvoice(ctxFor(t, time.Minute), tx.PaymentHash)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != nwc.StateSettled || got.Type != nwc.TypeIncoming || hashOf(got.Preimage) != tx.PaymentHash {
		t.Fatalf("lookup_invoice: %+v", got)
	}
	list, _, err := w.inv.ListTransactions(ctxFor(t, time.Minute), nwc.ListParams{Limit: 20, Type: nwc.TypeIncoming})
	if err != nil {
		t.Fatal(err)
	}
	if !hasSettled(list, tx.PaymentHash) {
		t.Fatalf("list_transactions (%d rows) does not show the settled invoice", len(list))
	}
}

func hasSettled(list []nwc.Transaction, hash string) bool {
	for _, x := range list {
		if x.PaymentHash == hash && x.State == nwc.StateSettled {
			return true
		}
	}
	return false
}

// hubWideNotifications: an invoice made through the hub's own API (no NWC connection) is still announced to the
// invoice connection, and found by it (getAlby/hub 921003b nip47/notifications/nip47_notifier.go:108-116,
// transactions/transactions_service.go:606-640).
func (w *world) hubWideNotifications(t *testing.T) {
	hub, err := w.sat.makeInvoice(5000, "hub-wide")
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if _, err := w.mer.payInvoice(hub.Invoice); err != nil {
		t.Fatalf("customer payment: %v", shownErr(err))
	}
	at, n := w.invN.wait(t, nwc.NotifPaymentReceived, hub.PaymentHash, 30*time.Second)
	t.Logf("MEASURED hubwide seconds_pay_start_to_notification=%.2f", at.Sub(began).Seconds())
	if n.Tx.AmountMsat != 5_000_000 {
		t.Errorf("notification amount %d", n.Tx.AmountMsat)
	}
	got, err := w.inv.LookupInvoice(ctxFor(t, time.Minute), hub.PaymentHash)
	if err != nil {
		t.Fatalf("lookup_invoice of the hub's own invoice: %v", shownErr(err))
	}
	if got.State != nwc.StateSettled {
		t.Fatalf("lookup state %s", shownState(got.State))
	}
	// The payout connection holds no notifications scope: it hears nothing.
	if w.payN.has(nwc.NotifPaymentReceived, hub.PaymentHash) {
		t.Error("the payout connection received a notification without the notifications scope")
	}
}

// unknownKey: a request from a key with no app on the hub gets no answer at all (getAlby/hub 921003b
// nip47/event_handler.go:73-82). The stranger is paired in the relay with the wallet key of the decoy app.
// The suite cannot show that the hub is subscribed on that wallet: the relay refuses the decoy app's own 13194
// (its p tag names the decoy client, not the paired stranger: "restricted: p tag does not match the author's
// pair"), so no 13194 exists to read. The hub subscribes for every app it creates, the same way as for the
// working connections; qualify/REPORT.md records this limit.
func (w *world) unknownKey(t *testing.T) {
	k := w.st.key(t, "stranger")
	c, err := nwc.Dial(ctxFor(t, time.Minute), nwc.Config{RelayURL: w.st.RelayURLs[hubSatbooth], WalletPubKey: w.st.Wallets["decoy"], Key: k, Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	began := time.Now()
	_, err = c.GetInfo(ctxFor(t, time.Minute))
	took := time.Since(began)
	var refused *nwc.RelayRefusedError
	if errors.As(err, &refused) {
		t.Fatalf("the relay refused the request (%v): the test did not reach the hub", shownErr(err))
	}
	if !errors.Is(err, nwc.ErrNoResponse) {
		t.Fatalf("want ErrNoResponse, got %v", shownErr(err))
	}
	t.Logf("MEASURED unknown_key no_response_after_seconds=%.1f", took.Seconds())
	if took < 14*time.Second {
		t.Errorf("gave up after %s, before the 15 s timeout", took)
	}
}

// merchantInvoice makes an invoice on the merchant's wallet over NWC.
func (w *world) merchantInvoice(t *testing.T, sats int64, desc string, expiry int64) *nwc.Transaction {
	t.Helper()
	tx, err := w.mc.MakeInvoice(ctxFor(t, time.Minute), nwc.MakeInvoiceParams{AmountMsat: sats * 1000, Description: desc, ExpirySeconds: expiry})
	if err != nil {
		t.Fatalf("merchant make_invoice: %v", shownErr(err))
	}
	return tx
}

func (w *world) budget(t *testing.T) *nwc.Budget {
	t.Helper()
	b, err := w.pay.GetBudget(ctxFor(t, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// stableSpendable returns the hub's spendable lightning balance once it has not changed for three looks one second
// apart: the hub shows a payment in its balance a moment after the payment settled.
func stableSpendable(t *testing.T, h *hubAPI) int64 {
	t.Helper()
	var last int64 = -1
	same := 0
	poll(t, "stable balance", time.Minute, time.Second, func() (bool, error) {
		b, err := h.balances()
		if err != nil {
			return false, err
		}
		if b.Lightning.TotalSpendableMsat == last {
			same++
		} else {
			last, same = b.Lightning.TotalSpendableMsat, 0
		}
		return same >= 3, errors.New("balance still moving")
	})
	return last
}

// payOutSettled state, shared between PayInvoice and PayTwice.
var settledPayout struct {
	hash           string
	msat           int64
	fee            int64
	merchantBefore int64 // the merchant hub's spendable msat before the payout
}

// payInvoice: the payout settles and the budget counts amount plus fee (getAlby/hub 921003b
// db/queries/get_budget_usage.go:11-23, transactions/transactions_service.go:1499-1506). On the direct channel the
// fee is 0, so the fee term is exercised only with 0.
func (w *world) payInvoice(t *testing.T) {
	settledPayout.merchantBefore = stableSpendable(t, w.mer)
	b0 := w.budget(t)
	t.Logf("MEASURED budget_before set=%v used_msat=%d total_msat=%d renewal=%s", b0.Set, b0.UsedMsat, b0.TotalMsat, shownRenewal(b0.RenewalPeriod))
	if !b0.Set || b0.TotalMsat != payoutBudgetSat*1000 {
		t.Fatalf("payout budget %+v", b0)
	}
	w.payoutUsedBaseMsat = b0.UsedMsat
	const sats = 3000
	m := w.merchantInvoice(t, sats, "payout test", 0)
	began := time.Now()
	r, err := w.pay.PayInvoice(ctxFor(t, 2*time.Minute), m.Invoice, sats*1000)
	took := time.Since(began)
	if err != nil {
		t.Fatalf("pay_invoice: %v", shownErr(err))
	}
	if hashOf(r.Preimage) != m.PaymentHash {
		t.Fatal("the preimage does not hash to the invoice's payment hash")
	}
	at, _ := w.mcN.wait(t, nwc.NotifPaymentReceived, m.PaymentHash, 30*time.Second)
	t.Logf("MEASURED pay_invoice seconds=%.2f fees_paid_msat=%d seconds_pay_start_to_merchant_notification=%.2f", took.Seconds(), r.FeesPaidMsat, at.Sub(began).Seconds())

	got, err := w.pay.LookupInvoice(ctxFor(t, time.Minute), m.PaymentHash)
	if err != nil {
		t.Fatalf("lookup_invoice on the payout connection: %v", shownErr(err))
	}
	if got.Type != nwc.TypeOutgoing || got.State != nwc.StateSettled || got.AmountMsat != sats*1000 || got.FeesPaidMsat != r.FeesPaidMsat {
		t.Fatalf("lookup_invoice: %+v", got)
	}
	b1 := w.budget(t)
	t.Logf("MEASURED budget_after_settle used_msat=%d", b1.UsedMsat)
	if want := w.payoutUsedBaseMsat + sats*1000 + r.FeesPaidMsat; b1.UsedMsat != want {
		t.Fatalf("used budget %d, want %d (before %d + amount + fee %d)", b1.UsedMsat, want, w.payoutUsedBaseMsat, r.FeesPaidMsat)
	}
	settledPayout.hash, settledPayout.msat, settledPayout.fee = m.PaymentHash, sats*1000, r.FeesPaidMsat
}

// payTwice: the hub refuses a second payment of a settled invoice with INTERNAL (getAlby/hub 921003b
// transactions/transactions_service.go:368-380, nip47/controllers/map_nip47_error.go:12-32).
func (w *world) payTwice(t *testing.T) {
	if settledPayout.hash == "" {
		t.Skip("PayInvoice did not settle a payout")
	}
	row, err := w.mc.LookupInvoice(ctxFor(t, time.Minute), settledPayout.hash)
	if err != nil {
		t.Fatal(err)
	}
	b0 := w.budget(t)
	_, err = w.pay.PayInvoice(ctxFor(t, 2*time.Minute), row.Invoice, settledPayout.msat)
	wantWalletError(t, "pay_twice", err, "INTERNAL", "this invoice has already been paid")
	// The merchant was credited once: its balance (which the hub updates a moment after the payment) rises by
	// the amount and stays there.
	if d := stableSpendable(t, w.mer) - settledPayout.merchantBefore; d != settledPayout.msat {
		t.Errorf("merchant credited %d msat, want exactly %d", d, settledPayout.msat)
	}
	if b1 := w.budget(t); b1.UsedMsat != b0.UsedMsat {
		t.Errorf("used budget moved from %d to %d", b0.UsedMsat, b1.UsedMsat)
	}
	tx, err := w.mer.transaction(settledPayout.hash)
	if err != nil || !strings.EqualFold(tx.State, "settled") {
		t.Fatalf("merchant hub row: %+v, %v", tx, shownErr(err))
	}
}

// amountMismatch: a different amount for an invoice that has one (getAlby/hub 921003b
// transactions/transactions_service.go:352-357).
func (w *world) amountMismatch(t *testing.T) {
	m := w.merchantInvoice(t, 2000, "mismatch", 0)
	b0 := w.budget(t)
	_, err := w.pay.PayInvoice(ctxFor(t, 2*time.Minute), m.Invoice, 1_000_000)
	wantWalletError(t, "amount_mismatch", err, "INTERNAL", "amount cannot be specified for an invoice that already has an amount")
	if b1 := w.budget(t); b1.UsedMsat != b0.UsedMsat {
		t.Errorf("used budget moved from %d to %d", b0.UsedMsat, b1.UsedMsat)
	}
	got, err := w.mc.LookupInvoice(ctxFor(t, time.Minute), m.PaymentHash)
	if err != nil || got.State == nwc.StateSettled {
		t.Fatalf("merchant row after the refused payment: %+v, %v", got, shownErr(err))
	}
}

// expired: an expired invoice is refused (getAlby/hub 921003b transactions/transactions_service.go:325-332), and the
// receiver's unsettled row reads expired (nip47/models/transactions.go:28-31).
func (w *world) expired(t *testing.T) {
	const expiry = 5
	m := w.merchantInvoice(t, 1000, "expires", expiry)
	if m.ExpiresAt == nil {
		t.Fatal("the invoice has no expires_at")
	}
	if d := time.Until(time.Unix(*m.ExpiresAt, 0).Add(2 * time.Second)); d > 0 {
		time.Sleep(d) // the invoice's own expiry is the condition
	}
	b0 := w.budget(t)
	_, err := w.pay.PayInvoice(ctxFor(t, 2*time.Minute), m.Invoice, 1_000_000)
	wantWalletError(t, "expired", err, "INTERNAL", "this invoice has expired")
	if b1 := w.budget(t); b1.UsedMsat != b0.UsedMsat {
		t.Errorf("used budget moved from %d to %d", b0.UsedMsat, b1.UsedMsat)
	}
	got, err := w.mc.LookupInvoice(ctxFor(t, time.Minute), m.PaymentHash)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURED expired merchant_lookup_state=%s", shownState(got.State))
	if got.State != nwc.StateExpired {
		t.Errorf("merchant row state %s, want %s (getAlby/hub 921003b nip47/models/transactions.go:28-31)", shownState(got.State), nwc.StateExpired)
	}
}

// budgetReserve: a pending payout counts amount + max(1%, 10 sat) against the budget and a payout over the rest
// gets QUOTA_EXCEEDED (getAlby/hub 921003b transactions/transactions_service.go:1142-1145, 1196-1225;
// db/queries/get_budget_usage.go:11-23).
type holdPayout struct {
	hash     string
	preimage string
	invoice  string
	done     chan holdResult
}

type holdResult struct {
	err  error
	took time.Duration
}

func holdExpirySeconds(keep bool) int64 {
	if keep {
		return 3600
	}
	return 600
}

// startHold makes a hold invoice on the merchant, starts the payout of it and waits until the payout row is
// pending and the merchant saw the HTLC. The hold invoice is cancelled at cleanup if the test did not do it.
func (w *world) startHold(t *testing.T, sats int64) *holdPayout {
	t.Helper()
	return w.startHoldKeep(t, sats, false)
}

// startHoldKeep is startHold; with keep the hold invoice is not cancelled at cleanup (the crash phases leave it
// open across the hub restart and settle it later).
func (w *world) startHoldKeep(t *testing.T, sats int64, keep bool) *holdPayout {
	t.Helper()
	preimage, hash := newPreimage(t)
	hold, err := w.mc.MakeHoldInvoice(ctxFor(t, time.Minute), nwc.MakeHoldInvoiceParams{AmountMsat: sats * 1000, PaymentHash: hash, Description: "hold", ExpirySeconds: holdExpirySeconds(keep)})
	if err != nil {
		t.Fatalf("make_hold_invoice: %v", shownErr(err))
	}
	h := &holdPayout{hash: hash, preimage: preimage, invoice: hold.Invoice, done: make(chan holdResult, 1)}
	if !keep {
		t.Cleanup(func() { _ = w.mc.CancelHoldInvoice(context.Background(), hash) }) // harmless when already cancelled
	}
	began := time.Now()
	go func() {
		_, err := w.pay.PayInvoice(context.Background(), hold.Invoice, 0)
		h.done <- holdResult{err, time.Since(began)}
	}()
	at, _ := w.mcN.wait(t, nwc.NotifHoldInvoiceAccepted, hash, time.Minute)
	t.Logf("MEASURED hold sats=%d seconds_pay_start_to_hold_invoice_accepted=%.2f", sats, at.Sub(began).Seconds())
	poll(t, "payout row pending", time.Minute, time.Second, func() (bool, error) {
		row, err := w.pay.LookupInvoice(ctxFor(t, 30*time.Second), hash)
		if err != nil {
			return false, err
		}
		return row.Type == nwc.TypeOutgoing && row.State == nwc.StatePending, fmt.Errorf("row state %s", shownState(row.State))
	})
	return h
}

// cancel cancels the hold invoice and checks the payout call fails with a WalletError INTERNAL and the row ends failed.
func (h *holdPayout) cancel(t *testing.T, w *world) {
	t.Helper()
	if err := w.mc.CancelHoldInvoice(ctxFor(t, time.Minute), h.hash); err != nil {
		t.Fatalf("cancel_hold_invoice: %v", shownErr(err))
	}
	select {
	case r := <-h.done:
		t.Logf("MEASURED hold_cancel payout_call_returned_after_seconds=%.2f", r.took.Seconds())
		wantWalletError(t, "hold_cancelled", r.err, "INTERNAL", "")
	case <-time.After(2 * time.Minute):
		t.Fatal("the payout call did not return within 2 minutes of the cancel")
	}
	poll(t, "payout row failed", time.Minute, time.Second, func() (bool, error) {
		row, err := w.pay.LookupInvoice(ctxFor(t, 30*time.Second), h.hash)
		if err != nil {
			return false, err
		}
		return row.State == nwc.StateFailed, fmt.Errorf("row state %s", shownState(row.State))
	})
}

const quotaMessage = "Your app does not have enough budget remaining to make this payment. Please review this app in the connections page of your Alby Hub."

func (w *world) budgetReserve(t *testing.T) {
	const holdSats = 15_000
	b0 := w.budget(t)
	big := w.startHold(t, holdSats)
	b1 := w.budget(t)
	var reserve int64 = max(holdSats*1000/100, 10_000) // max(1%, 10 sat), in msat
	t.Logf("MEASURED budget_while_pending used_msat=%d before=%d amount_msat=%d reserve_msat=%d", b1.UsedMsat, b0.UsedMsat, holdSats*1000, reserve)
	if want := b0.UsedMsat + holdSats*1000 + reserve; b1.UsedMsat != want {
		t.Errorf("used budget while pending %d, want %d = before + amount + 1%%", b1.UsedMsat, want)
	}

	// Below 1,000 sat the floor of 10 sat applies: 1% of 500 sat would be only 5 sat.
	const smallSats = 500
	small := w.startHold(t, smallSats)
	b2 := w.budget(t)
	t.Logf("MEASURED budget_small_hold used_msat=%d before=%d amount_msat=%d", b2.UsedMsat, b1.UsedMsat, smallSats*1000)
	if want := b1.UsedMsat + smallSats*1000 + 10_000; b2.UsedMsat != want {
		t.Errorf("used budget with the small hold %d, want %d = before + amount + 10 sat floor (a plain 1%% gives %d)", b2.UsedMsat, want, b1.UsedMsat+smallSats*1000+5_000)
	}

	// An invoice that fits the remaining budget only without its reserve is refused.
	remainingSat := (b2.TotalMsat - b2.UsedMsat) / 1000
	over := w.merchantInvoice(t, remainingSat-1, "over budget by the reserve", 0)
	_, err := w.pay.PayInvoice(ctxFor(t, 2*time.Minute), over.Invoice, 0)
	wantWalletError(t, "quota_exceeded", err, "QUOTA_EXCEEDED", quotaMessage)
	bigger := w.merchantInvoice(t, remainingSat+10_000, "over budget", 0)
	_, err = w.pay.PayInvoice(ctxFor(t, 2*time.Minute), bigger.Invoice, 0)
	wantWalletError(t, "quota_exceeded_plain", err, "QUOTA_EXCEEDED", quotaMessage)

	// Cancel both: the pending payouts fail and the budget drops back.
	small.cancel(t, w)
	big.cancel(t, w)
	poll(t, "budget back", time.Minute, time.Second, func() (bool, error) {
		b := w.budget(t)
		return b.UsedMsat == b0.UsedMsat, fmt.Errorf("used %d, want %d", b.UsedMsat, b0.UsedMsat)
	})
}

// invoiceCannotPay: a method outside the connection's scopes gets RESTRICTED (getAlby/hub 921003b
// nip47/event_handler.go:362-390, nip47/permissions/permissions.go:38-61).
func (w *world) invoiceCannotPay(t *testing.T) {
	m := w.merchantInvoice(t, 1000, "restricted", 0)
	_, err := w.inv.PayInvoice(ctxFor(t, time.Minute), m.Invoice, 0)
	wantWalletError(t, "invoice_cannot_pay", err, "RESTRICTED", "This app does not have the pay_invoice scope")
	got, err := w.mc.LookupInvoice(ctxFor(t, time.Minute), m.PaymentHash)
	if err != nil || got.State == nwc.StateSettled {
		t.Fatalf("merchant row: %+v, %v", got, shownErr(err))
	}
}

// listPageSize: a page of 20 transactions with 100-character descriptions fits in one NIP-44 message
// (65,535 bytes of plaintext). The raw connection measures the plaintext of the response itself.
func (w *world) listPageSize(t *testing.T) {
	desc := func(i int) string {
		s := fmt.Sprintf("page-size row %02d ", i)
		return s + strings.Repeat("x", 100-len(s))
	}
	for i := 0; i < 25; i++ {
		if _, err := w.inv.MakeInvoice(ctxFor(t, time.Minute), nwc.MakeInvoiceParams{AmountMsat: 1_000_000, Description: desc(i)}); err != nil {
			t.Fatalf("invoice %d: %v", i, shownErr(err))
		}
	}
	page, total, err := w.inv.ListTransactions(ctxFor(t, time.Minute), nwc.ListParams{Limit: 20, Unpaid: true})
	if err != nil {
		t.Fatalf("list_transactions limit 20: %v", shownErr(err))
	}
	if len(page) != 20 {
		t.Fatalf("page of %d rows (total %d), want 20", len(page), total)
	}
	for _, x := range page {
		if !strings.HasPrefix(x.Description, "page-size row ") || len(x.Description) != 100 {
			t.Errorf("description of %d bytes, want the 100 sent", len(x.Description))
		}
	}

	ctx := ctxFor(t, 2*time.Minute)
	rc, err := dialRaw(ctx, w.st.RelayURLs[hubSatbooth], w.keys["invoice"])
	if err != nil {
		t.Fatal(err)
	}
	defer rc.close()
	me, wallet := w.keys["invoice"].PublicKeyHex(), w.st.Wallets["invoice"]
	if err := rc.req(ctx, "r", `{"kinds":[23195],"#p":["`+me+`"],"authors":["`+wallet+`"]}`); err != nil {
		t.Fatal(err)
	}
	sizeOf := func(limit int) (plain int, rows int, err error) {
		ev, err := nwc.BuildRequest(w.keys["invoice"], wallet, "list_transactions", map[string]any{"limit": limit, "unpaid": true}, time.Now(), time.Minute)
		if err != nil {
			return 0, 0, err
		}
		ok, msg, err := rc.publish(ctx, ev)
		if err != nil || !ok {
			return 0, 0, fmt.Errorf("publish: %v (the relay's message: %d bytes)", shownErr(err), len(msg))
		}
		for {
			resp, err := rc.nextEvent(ctx, "r")
			if err != nil {
				return 0, 0, err
			}
			id, plaintext, err := nwc.OpenResponse(resp, w.keys["invoice"], wallet)
			if err != nil || id != ev.ID {
				continue // an older response replayed from the window
			}
			var env struct {
				Result struct {
					Transactions []json.RawMessage `json:"transactions"`
				} `json:"result"`
				Error *struct{ Code, Message string } `json:"error"`
			}
			if err := json.Unmarshal([]byte(plaintext), &env); err != nil {
				return 0, 0, fmt.Errorf("the response: %s", jsonReason(err))
			}
			if env.Error != nil {
				return 0, 0, fmt.Errorf("wallet error %s (message of %d bytes)", shownCode(env.Error.Code), len(env.Error.Message))
			}
			return len(plaintext), len(env.Result.Transactions), nil
		}
	}
	plain20, rows20, err := sizeOf(20)
	if err != nil {
		t.Fatalf("raw page of 20: %v", shownErr(err))
	}
	t.Logf("MEASURED list_page limit=20 rows=%d plaintext_bytes=%d (NIP-44 maximum 65535)", rows20, plain20)
	if rows20 != 20 || plain20 >= 65535 {
		t.Fatalf("page of 20: rows %d, plaintext %d bytes", rows20, plain20)
	}
	// For the record: the hub's own maximum (50) with as many rows as exist.
	if plain50, rows50, err := sizeOf(50); err != nil {
		t.Logf("MEASURED list_page limit=50 error=%v", shownErr(err))
	} else {
		t.Logf("MEASURED list_page limit=50 rows=%d plaintext_bytes=%d", rows50, plain50)
	}
}
