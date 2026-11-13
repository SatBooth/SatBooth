//go:build qualify

package qualify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
)

// The plan 6.1 follow-ups on regtest: the per-account cap on open invoices, a 0-confirmation tier paid from the
// mempool (then replaced, or bumped), an RBF bump after the expiry that inherits the original's first-seen, and a
// payment stuck in the mempool past its order's watch window (a second environment whose clock runs a week ahead).

// ---- helpers ----

// createOrderAcct is createOrder for the given account_id.
func (e *lnEnv) createOrderAcct(acct, ref string, msat, expiresIn int64) orderJSON {
	e.t.Helper()
	var o orderJSON
	body := map[string]any{"account_id": acct, "merchant_ref": ref, "lightning_msat": msat, "expires_in": expiresIn}
	if code := e.api("POST", "/v1/orders", e.writeKey, body, &o); code != 201 {
		e.t.Fatalf("create order: status %d", code)
	}
	if o.State != "new" || o.AccountID != acct {
		e.t.Fatalf("created order looks wrong: state %q", o.State)
	}
	return o
}

// pressLocation presses pay and returns the 303's Location (it fails on any other status).
func (e *lnEnv) pressLocation(token string) string {
	e.t.Helper()
	resp, err := e.hc.Post("http://"+publicAddr+"/c/"+token+"/pay", "application/x-www-form-urlencoded", nil)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("pay: status %d, want 303", resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

// pageAt fetches the checkout page at a path the service redirected to (it may carry ?busy=1).
func (e *lnEnv) pageAt(path string) (int, string) {
	e.t.Helper()
	resp, err := e.hc.Get("http://" + publicAddr + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b)
}

// noWarnings fails on every ERROR or WARN record of the service's log, and on any line that is not a JSON record.
func noWarnings(t *testing.T, logs string) {
	t.Helper()
	behind := 0
	for _, l := range strings.Split(logs, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var m struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(l), &m); err != nil || m.Msg == "" {
			t.Errorf("a log line that is not a JSON record: %s", l)
			continue
		}
		if m.Level == "WARN" && m.Msg == alWalletBehind {
			behind++ // the real wallet is a little behind its node: tolerated, any number of times
			continue
		}
		if m.Level == "ERROR" || m.Level == "WARN" {
			t.Errorf("unexpected log line: %s %s", m.Level, m.Msg)
		}
	}
	if behind > 0 {
		t.Logf("OBSERVED the WARN %q was logged %d times (tolerated)", alWalletBehind, behind)
	}
}

// received records getreceivedbyaddress of addr at minconf 0 and 1 in both wallets that hold its key or descriptor,
// and returns the merchant wallet's at minconf 0.
func (e *ocEnv) received(label, addr string) int64 {
	e.t.Helper()
	var out int64
	var parts []string
	for _, w := range []string{merchantWallet, watchWallet} {
		for _, mc := range []int{0, 1} {
			got, err := e.btc.getReceivedByAddress(w, addr, mc)
			if err != nil {
				e.t.Fatalf("getreceivedbyaddress: %v", shownErr(err))
			}
			if w == merchantWallet && mc == 0 {
				out = got
			}
			parts = append(parts, fmt.Sprintf("%s minconf %d: %d sat", w, mc, got))
		}
	}
	e.t.Logf("OBSERVED %s getreceivedbyaddress: %s", label, strings.Join(parts, ", "))
	return out
}

// settledOut reports whether the output is in onchain_settled, read straight from the store's database (the service
// must not be running; the store has no reader of that table for callers).
func (e *ocEnv) settledOut(k store.TXOKey) bool {
	e.t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(e.dataDir, "store.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM onchain_settled WHERE txid = ? AND vout = ?`, k.TxID, int64(k.Vout)).Scan(&n); err != nil {
		e.t.Fatalf("onchain_settled: %v", shownErr(err))
	}
	return n == 1
}

// marksOf returns the current mark kind of every stored output of the order, by txid.
func marksOf(ctx context.Context, t *testing.T, st *store.Store, orderID string) (map[string]string, []store.TXO) {
	t.Helper()
	xs, err := st.TXOsForOrder(ctx, orderID)
	if err != nil {
		t.Fatal(err)
	}
	marks := map[string]string{}
	for _, x := range xs {
		m, ok, err := st.CurrentMark(ctx, store.TXOKey{TxID: x.TxID, Vout: x.Vout})
		if err != nil {
			t.Fatal(err)
		}
		kind := "none"
		if ok {
			kind = m.Kind
		}
		marks[x.TxID] = kind
	}
	return marks, xs
}

// ---- Lightning: the per-account cap ----

// TestLightningAccountCap: one account may hold 5 open invoices (the default cap). The sixth press is busy, another
// account is not held back, and a payment releases the slot for the sixth.
func TestLightningAccountCap(t *testing.T) {
	e := newLnEnv(t, "accountcap")
	s := e.start()
	const msat = 21_000_000
	acct := "cap-" + randHex(t)[:8]
	var orders []orderJSON
	for i := 0; i < 6; i++ {
		orders = append(orders, e.createOrderAcct(acct, fmt.Sprintf("cap-%d-%s", i, randHex(t)[:8]), msat, 86400))
	}
	var invoices []string
	for _, o := range orders[:5] {
		invoices = append(invoices, e.pressPay(tokenOf(o)))
	}
	sixth := tokenOf(orders[5])
	loc := e.pressLocation(sixth)
	if loc != "/c/"+sixth+"?busy=1" {
		t.Fatalf("the sixth press redirects to %q, want the page with ?busy=1", loc)
	}
	if code, page := e.pageAt(loc); code != 200 || invoiceRe.MatchString(page) || !strings.Contains(page, "Busy, try again.") {
		t.Fatalf("the sixth order's busy page: status %d, invoice %v, busy line %v", code, invoiceRe.MatchString(page), strings.Contains(page, "Busy, try again."))
	}
	other := e.createOrderAcct("cap-"+randHex(t)[:8], "cap-other-"+randHex(t)[:8], msat, 86400)
	e.pressPay(tokenOf(other))
	t.Logf("OBSERVED lightning=accountcap five invoices for one account, the sixth press answered 303 to ?busy=1 with no invoice, another account's order got an invoice")

	paidAt := time.Now()
	e.customerPays(invoices[0])
	e.waitOrder(orders[0].ID, "paid", paidAt, paidBound)
	released := time.Now()
	poll(t, "the sixth order gets an invoice once a slot is released", 30*time.Second, 500*time.Millisecond, func() (bool, error) {
		if loc := e.pressLocation(sixth); loc != "/c/"+sixth {
			return false, fmt.Errorf("pay redirects to %q", loc)
		}
		code, page := e.page(sixth)
		if code != 200 || !invoiceRe.MatchString(page) {
			return false, fmt.Errorf("status %d and no invoice", code)
		}
		return true, nil
	})
	t.Logf("MEASURED lightning=accountcap release_to_invoice_seconds=%.2f (from the paid order reading paid to the sixth press getting an invoice; presses every 0.5 s)", time.Since(released).Seconds())
	e.waitHook(orders[0].ID, store.EventPaid, hookBound)
	for _, o := range append(orders[1:], other) {
		if st := e.order(o.ID).State; st != "new" {
			t.Errorf("order %s reads %s, want new", o.ID, st)
		}
	}
	e.settleQuiet()
	s.stop()
	noWarnings(t, s.logs.String())
	wants := []want{{orderID: orders[0].ID, msat: msat, paid: true}, {orderID: other.ID, msat: msat}}
	for _, o := range orders[1:] {
		wants = append(wants, want{orderID: o.ID, msat: msat})
	}
	e.safety(wants)
}

// ---- On-chain: the 0-confirmation tier ----

// TestOnchainZeroConf: an order of the 0 tier is paid from the mempool. A replacement that takes the output away
// reverts it (order.reverted, one pass after the dead mark when the replacement landed after that pass's
// listsinceblock); a fee bump that keeps the output leaves it paid with no order.reverted, in one pass or when the bump
// lands between a pass's listsinceblock and its gettransaction of the old output (that pass holds the reversal).
func TestOnchainZeroConf(t *testing.T) {
	e := newOcEnv(t, true)
	s := e.start()

	// A: paid from the mempool, then diverted.
	ra, oa := e.newOrder("zeroconf-replaced", satT0, 0, 86400, map[string]rng{store.EventPaid: {1, 1}, store.EventReverted: {1, 1}}, nil)
	if oa.Onchain.RequiredConfirmations != 0 {
		t.Fatalf("an order of %d sat needs %d confirmations, want 0", satT0, oa.Onchain.RequiredConfirmations)
	}
	addrA, _ := e.press(ra, false)
	h0 := e.height()
	txA, sent := e.pay(addrA, satT0)
	ra.TxID = txA
	d, o := e.waitOC(ra.ID, "paid from the mempool", sent, ocBound, func(o orderJSON) bool {
		return o.State == "paid" && o.Onchain.State == "paid"
	})
	if o.Onchain.Confirmations != 0 || o.Onchain.RequiredConfirmations != 0 || o.Onchain.ConfirmedSat != satT0 || o.PaidAt == nil {
		t.Errorf("paid from the mempool reads %+v", *o.Onchain)
	}
	if h := e.height(); h != h0 {
		t.Fatalf("a block was mined (height %d, then %d): not paid from the mempool", h0, h)
	}
	t.Logf("MEASURED onchain=zeroconf send_to_paid_seconds=%.2f (poll interval 2 s, no block)", d.Seconds())
	e.waitHook(ra.ID, store.EventPaid, hookBound)
	e.pageHas(ra.Token, ">Paid<")
	e.received("zeroconf A paid, unmined", addrA)

	m1, m2 := e.newAddr(), e.newAddr()
	diverted := time.Now()
	txD, fallback, bumpErr, err := e.btc.divert(minerWallet, txA, addrA, m1, m2, 20_000)
	if err != nil {
		t.Fatalf("replacing the payment (bumpfee refused: %v): %v", shownErr(bumpErr), shownErr(err))
	}
	t.Logf("OBSERVED zeroconf divert by bumpfee with outputs: fallback used %v", fallback)
	for _, w := range []string{watchWallet, merchantWallet} {
		e.viewTx("zeroconf A diverted, unmined", w, txA)
	}
	e.received("zeroconf A diverted, unmined", addrA)
	// What the rule rests on (onchain/watch.go): the watch wallet itself reports the diverting transaction as a mempool
	// conflict of the original.
	poll(t, "the watch wallet's mempoolconflicts of A lists the diverting transaction", ocBound, 200*time.Millisecond, func() (bool, error) {
		c, err := e.btc.mempoolConflicts(watchWallet, txA)
		if err != nil {
			return false, err
		}
		if !slices.Contains(c, txD) {
			return false, fmt.Errorf("mempoolconflicts %v, want it to contain %s", c, txD)
		}
		return true, nil
	})
	d, _ = e.waitOC(ra.ID, "reverted to waiting", diverted, ocBound, func(o orderJSON) bool {
		return o.State == "new" && o.Onchain.State == "waiting" && o.Onchain.ReceivedSat == 0 && o.Onchain.ConfirmedSat == 0
	})
	t.Logf("MEASURED onchain=zeroconf divert_to_new_seconds=%.2f", d.Seconds())
	e.waitHook(ra.ID, store.EventReverted, hookBound)
	if n := e.rcv.countSeq(ra.ID, store.EventReverted, 1); n != 1 {
		t.Errorf("order.reverted seq 1 delivered %d times", n)
	}
	e.mine(1)
	time.Sleep(ocQuiet)
	if o := e.order(ra.ID); o.State != "new" || o.Onchain.ReceivedSat != 0 || o.Onchain.ConfirmedSat != 0 {
		t.Errorf("after the replacement was mined the order reads %s with %d sat received", o.State, o.Onchain.ReceivedSat)
	}
	for _, w := range []string{watchWallet, merchantWallet} {
		e.viewTx("zeroconf A after D mined", w, txA)
	}
	e.viewTx("zeroconf D mined", minerWallet, txD)
	if got := e.received("zeroconf A after D mined", addrA); got != 0 {
		t.Errorf("getreceivedbyaddress at minconf 0 reads %d sat for the diverted address after the block, want 0", got)
	}

	// B: paid from the mempool, then bumped keeping the output.
	rb, _ := e.newOrder("zeroconf-bumped", satT0, 0, 86400,
		map[string]rng{store.EventPaid: {1, 1}}, nil)
	addrB, _ := e.press(rb, false)
	txB, sent := e.pay(addrB, satT0)
	e.waitOC(rb.ID, "paid from the mempool", sent, ocBound, func(o orderJSON) bool {
		return o.State == "paid" && o.Onchain.ConfirmedSat == satT0 && o.Onchain.Confirmations == 0
	})
	e.waitHook(rb.ID, store.EventPaid, hookBound)
	txB2, err := e.btc.bumpFee(minerWallet, txB, nil)
	if err != nil {
		t.Fatalf("bumpfee keeping the output: %v", shownErr(err))
	}
	rb.TxID = txB2
	e.saveReg()
	// What the rule rests on: the node names the replacement as the mempool spender of every input of the original.
	spenders, err := e.btc.spendersOf(watchWallet, txB)
	if err != nil || len(spenders) == 0 {
		t.Fatalf("gettxspendingprevout for the inputs of B (%d answers): %v", len(spenders), shownErr(err))
	}
	for i, id := range spenders {
		if id != txB2 {
			t.Errorf("gettxspendingprevout for input %d of B names %q, want the replacement %s", i, id, txB2)
		}
	}
	for _, w := range []string{watchWallet, merchantWallet} {
		e.viewTx("zeroconf B (replaced), both unmined", w, txB)
		e.viewTx("zeroconf B2 (replacement), both unmined", w, txB2)
	}
	e.viewSince("zeroconf B bumped, both unmined", watchWallet, txB, txB2)
	e.received("zeroconf B bumped, both unmined", addrB)
	time.Sleep(ocQuiet)
	// The same reads once the wallets have processed the replacement (the first ones may come before they learn it).
	for _, w := range []string{watchWallet, merchantWallet} {
		e.viewTx("zeroconf B (replaced), both unmined, after the quiet period", w, txB)
		e.viewTx("zeroconf B2 (replacement), both unmined, after the quiet period", w, txB2)
	}
	e.viewSince("zeroconf B bumped, both unmined, after the quiet period", watchWallet, txB, txB2)
	e.received("zeroconf B bumped, both unmined, after the quiet period", addrB)
	if o := e.order(rb.ID); o.State != "paid" || o.Onchain.ConfirmedSat != satT0 || o.Onchain.ReceivedSat != satT0 {
		t.Errorf("after the bump the order reads %s, confirmed %d, received %d; want paid and %d once", o.State, o.Onchain.ConfirmedSat, o.Onchain.ReceivedSat, satT0)
	}
	nRev, nAgain := e.rcv.count(rb.ID, store.EventReverted), e.rcv.count(rb.ID, store.EventPaidAgain)
	t.Logf("OBSERVED zeroconf bump: order.reverted delivered %d, order.paid_again delivered %d (0 and 0 whether one pass saw both or the bump landed between a pass's listsinceblock and its gettransaction of the old output: that pass holds the reversal)", nRev, nAgain)
	if nRev != 0 || nAgain != 0 {
		t.Errorf("order.reverted %d times and order.paid_again %d times, want neither", nRev, nAgain)
	}
	mined := time.Now()
	e.mine(6)
	e.waitOC(rb.ID, "6 confirmations", mined, ocBound, func(o orderJSON) bool { return o.Onchain.Confirmations >= 6 && o.State == "paid" })
	time.Sleep(ocQuiet) // a pass after the depth settles the output (settleDone)
	for _, w := range []string{watchWallet, merchantWallet} {
		e.viewTx("zeroconf B (replaced) after B2 mined", w, txB)
	}
	if got := e.received("zeroconf B after B2 mined", addrB); got != satT0 {
		t.Errorf("getreceivedbyaddress at minconf 0 reads %d sat after the bump was mined, want %d", got, satT0)
	}
	// One reversal for A, and one for B's old record in either case (reversed in the bump's pass, or in the pass after
	// the one that held it).
	e.finish(s, map[string]rng{alReorg: {2, 2}})

	e.withStore(func(ctx context.Context, st *store.Store) {
		ma, xa := marksOf(ctx, t, st, ra.ID)
		if len(xa) != 1 || ma[txA] != store.MarkDead {
			t.Errorf("order A: outputs %d, marks %v; want one, dead", len(xa), ma)
		}
		mb, xb := marksOf(ctx, t, st, rb.ID)
		if len(xb) != 2 || mb[txB] != store.MarkDead || mb[txB2] != store.MarkMined {
			t.Errorf("order B: outputs %d, marks %v; want the replaced one dead and the replacement mined", len(xb), mb)
		}
		var vout uint32
		for _, x := range xb {
			if x.TxID == txB2 {
				vout = x.Vout
			}
		}
		if !e.settledOut(store.TXOKey{TxID: txB2, Vout: vout}) {
			t.Error("the replacement, mined 6 deep, is not in onchain_settled")
		}
		w, err := st.WatchedTXOs(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range w {
			if x.TxID == txB2 {
				t.Error("the settled replacement is still watched")
			}
		}
		t.Logf("OBSERVED zeroconf store: A %v, B %v; the replacement settled and not watched (%d watched outputs)", ma, mb, len(w))
	})
}

// ---- On-chain: an RBF bump after the expiry ----

// TestOnchainBumpAfterExpiry: the full amount is seen before the expiry and bumped after it. The replacement inherits
// the original's first-seen: it is counted, not late, and the block pays the order. No final event comes in between.
func TestOnchainBumpAfterExpiry(t *testing.T) {
	e := newOcEnv(t, true)
	s := e.start()
	r, _ := e.newOrder("bump-after-expiry", satT1, 0, 300, map[string]rng{store.EventPaid: {1, 1}, store.EventLate: {0, 0}}, nil)
	addr, _ := e.press(r, false)
	txA, sent := e.pay(addr, satT1)
	e.waitOC(r.ID, "seen", sent, ocBound, func(o orderJSON) bool { return o.Onchain.State == "seen" && o.Onchain.ReceivedSat == satT1 })
	if wait := time.Until(time.Unix(r.Expiry, 0).Add(time.Second)); wait > 0 {
		t.Logf("waiting %.0f s for the expiry of %s", wait.Seconds(), r.Tag)
		time.Sleep(wait)
	}
	time.Sleep(ocQuiet)
	if o := e.order(r.ID); o.State != "new" || o.Onchain.State != "seen" || o.Onchain.ReceivedSat != satT1 || o.Onchain.LateSat != 0 {
		t.Fatalf("past the expiry, before the bump, the order reads %s / %+v; want new and seen", o.State, *o.Onchain)
	}
	if n := e.rcv.forOrder(r.ID); n != 0 {
		t.Fatalf("%d webhooks for the order before the bump, want none (the full amount was seen on time)", n)
	}

	bumped := time.Now()
	txB, err := e.btc.bumpFee(minerWallet, txA, nil)
	if err != nil {
		t.Fatalf("bumpfee keeping the output: %v", shownErr(err))
	}
	r.TxID = txB
	e.saveReg()
	// The API from the bump on, every 100 ms for ocQuiet: the last read that did not count the full amount on time.
	var lastOff time.Duration
	states := map[string]int{}
	for end := time.Now().Add(ocQuiet); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		o := e.order(r.ID)
		states[o.State+"/"+o.Onchain.State]++
		if o.State != "new" || o.Onchain.ReceivedSat != satT1 || o.Onchain.LateSat != 0 {
			lastOff = time.Since(bumped)
		}
	}
	e.viewTx("bump A (replaced), both unmined", watchWallet, txA)
	e.viewTx("bump B (replacement), both unmined", watchWallet, txB)
	e.viewSince("bump, both unmined", watchWallet, txA, txB)
	e.received("bump, both unmined", addr)
	t.Logf("MEASURED onchain=bump bump_to_counted_seconds=%.2f (the last API read after the bump that did not show the full amount counted on time; 0: none did); reads by state %v", lastOff.Seconds(), states)
	// In the pass that holds the final event (the original dead, the bump not yet listed) the order reads new, never
	// expired: the API shows no final state that no event backs.
	for st, n := range states {
		if strings.HasPrefix(st, "expired/") || strings.HasSuffix(st, "/expired") {
			t.Errorf("%d API reads of %s after the bump, want none", n, st)
		}
	}
	_, o := e.waitOC(r.ID, "the replacement counted", bumped, ocBound, func(o orderJSON) bool {
		return o.State == "new" && o.Onchain.ReceivedSat == satT1 && o.Onchain.LateSat == 0
	})
	if o.Onchain.State != "seen" {
		t.Errorf("after the bump the order reads %s / %s", o.State, o.Onchain.State)
	}
	mined := time.Now()
	e.mine(1)
	d, o := e.waitOC(r.ID, "paid", mined, ocBound, func(o orderJSON) bool { return o.State == "paid" })
	if o.Onchain.LateSat != 0 || o.Onchain.ConfirmedSat != satT1 {
		t.Errorf("paid by the bump reads %+v", *o.Onchain)
	}
	t.Logf("MEASURED onchain=bump block_to_paid_seconds=%.2f", d.Seconds())
	e.waitHook(r.ID, store.EventPaid, hookBound)
	e.viewTx("bump A (replaced) after B mined", watchWallet, txA)
	time.Sleep(ocQuiet)
	e.finish(s, map[string]rng{alReorg: {0, 0}})

	e.withStore(func(ctx context.Context, st *store.Store) {
		marks, xs := marksOf(ctx, t, st, r.ID)
		if len(xs) != 2 || marks[txA] != store.MarkDead || marks[txB] != store.MarkMined {
			t.Errorf("outputs %d, marks %v; want the original dead and the bump mined", len(xs), marks)
		}
		var fa, fb time.Time
		for _, x := range xs {
			switch x.TxID {
			case txA:
				fa = x.FirstSeen
			case txB:
				fb = x.FirstSeen
			}
		}
		if !fb.Equal(fa) || !fb.Before(time.Unix(r.Expiry, 0)) {
			t.Errorf("the bump's first-seen %s, the original's %s, the expiry %s: want the original's, before the expiry", fb.UTC().Format(time.RFC3339), fa.UTC().Format(time.RFC3339), time.Unix(r.Expiry, 0).UTC().Format(time.RFC3339))
		}
		t.Logf("OBSERVED bump store: marks %v; the bump inherited the original's first-seen (%.0f s before the expiry)", marks, time.Unix(r.Expiry, 0).Sub(fb).Seconds())
	})
}

// ---- On-chain: a payment stuck past its window ----

const stuckName = "onchain-stuck"

// TestOnchainStuckBefore: in the stuck environment, an order paid in full and left in the mempool. The phase ends well
// before the order's real expiry; the After phase runs the service a week ahead.
func TestOnchainStuckBefore(t *testing.T) {
	e := newOcEnvIn(t, stuckName, 0)
	s := e.start()
	r, _ := e.newOrder("stuck", satT1, 0, 300, nil, nil)
	addr, _ := e.press(r, false)
	txid, sent := e.pay(addr, satT1)
	r.TxID = txid
	e.saveReg()
	_, o := e.waitOC(r.ID, "seen", sent, ocBound, func(o orderJSON) bool { return o.Onchain.State == "seen" && o.Onchain.ReceivedSat == satT1 })
	if o.Onchain.Confirmations != 0 || o.State != "new" {
		t.Errorf("seen reads %s / %+v", o.State, *o.Onchain)
	}
	if left := time.Until(time.Unix(r.Expiry, 0)); left < 2*time.Minute {
		t.Fatalf("only %.0f s left before the real expiry: the After phase must start well before it", left.Seconds())
	}
	e.finish(s, nil)
}

// TestOnchainStuckAfter: the same order with the service's clock 7 days and 10 minutes ahead, past its watch window,
// its payment still in the mempool: order.underpaid at the window end (the payment stays tracked); then mined, it pays
// the order after the final event.
func TestOnchainStuckAfter(t *testing.T) {
	e := newOcEnvIn(t, stuckName, 7*24*time.Hour+10*time.Minute)
	r := e.tagged("stuck")
	if r.TxID == "" {
		t.Fatal("the stuck order has no txid (did StuckBefore run?)")
	}
	r.Ev = map[string]rng{store.EventUnderpaid: {1, 1}, store.EventPaid: {1, 1}}
	inPool := func(label string) (int, bool) {
		var v struct {
			Confirmations int `json:"confirmations"`
		}
		var raw json.RawMessage
		if err := e.btc.call(minerWallet, "gettransaction", map[string]any{"txid": r.TxID}, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		var entry json.RawMessage
		err := e.btc.call("", "getmempoolentry", map[string]any{"txid": r.TxID}, &entry)
		var re *rpcError
		if err != nil && !errors.As(err, &re) {
			t.Fatal(err)
		}
		t.Logf("OBSERVED stuck %s: gettransaction in %s confirmations=%d, getmempoolentry found=%v", label, minerWallet, v.Confirmations, err == nil)
		return v.Confirmations, err == nil
	}
	if _, found := inPool("before the service starts"); !found {
		t.Errorf("getmempoolentry does not find the stuck payment before the service starts")
	}
	s := e.start()
	d, o := e.waitOC(r.ID, "underpaid at the window end", e.runStart, 3*time.Minute, func(o orderJSON) bool {
		return o.State == "underpaid" && o.Onchain.State == "underpaid"
	})
	if o.Onchain.ReceivedSat != satT1 || o.Onchain.Confirmations != 0 || o.Onchain.ConfirmedSat != 0 {
		t.Errorf("underpaid at the window end reads %+v", *o.Onchain)
	}
	t.Logf("MEASURED onchain=stuck window_pass_to_underpaid_seconds=%.2f (from the service start; its first pass runs at once)", d.Seconds())
	e.waitHook(r.ID, store.EventUnderpaid, hookBound)
	e.pageHas(r.Token, "not yet confirmed enough")
	if c, found := inPool("after order.underpaid"); c != 0 || !found {
		t.Errorf("the stuck payment reads %d confirmations, getmempoolentry found %v; want 0 and found (still in the mempool)", c, found)
	}
	mined := time.Now()
	e.mine(1)
	d, o = e.waitOC(r.ID, "paid", mined, ocBound, func(o orderJSON) bool { return o.State == "paid" })
	if o.Onchain.Confirmations != 1 || o.Onchain.ConfirmedSat != satT1 || o.Onchain.State != "paid" {
		t.Errorf("paid after the window reads %+v", *o.Onchain)
	}
	t.Logf("MEASURED onchain=stuck block_to_paid_seconds=%.2f", d.Seconds())
	e.waitHook(r.ID, store.EventPaid, hookBound)
	e.mine(6)
	time.Sleep(ocQuiet)
	e.finish(s, map[string]rng{alUnderpaidWindow: {1, 1}, alPaidAfterFinal: {1, 1}})
	e.withStore(func(ctx context.Context, st *store.Store) {
		marks, xs := marksOf(ctx, t, st, r.ID)
		if len(xs) != 1 || marks[r.TxID] != store.MarkMined || !e.settledOut(store.TXOKey{TxID: xs[0].TxID, Vout: xs[0].Vout}) {
			t.Errorf("outputs %d, marks %v: want one, mined and settled", len(xs), marks)
		}
		w, err := st.WatchedTXOs(ctx, e.now())
		if err != nil {
			t.Fatal(err)
		}
		if len(w) != 0 {
			t.Errorf("%d outputs still watched, want none", len(w))
		}
	})
}
