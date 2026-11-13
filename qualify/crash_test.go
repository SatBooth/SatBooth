//go:build qualify

package qualify

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/nwc"
)

// Crash qualification: payouts in flight across a graceful hub stop, a hub kill and a relay loss. scripts/qualify.sh
// runs a "Before" phase, then the stop, kill or relay restart, then the "After" phase; what the After phase needs
// travels in /state/crash-<name>.json. The safety property under test:
//
//	the merchant is credited at most once per invoice, and SatBooth's hub pays at most once.
//
// It is checked from both hubs' own records (rows for the payment hash and stabilised balances), not from SatBooth's
// client view. Hub behaviour is pinned to getAlby/hub 921003b: a payout row is marked FAILED when SendPaymentSync
// returns an error, node shutdown included, and a FAILED row can later settle (transactions/transactions_service.go:
// 449-460, 913-927, 967; lnclient/ldk/ldk.go:664-669); a second pay_invoice is refused while a PENDING or SETTLED
// row exists (transactions_service.go:301-380).

const (
	crashSats      = 2_000
	crashSettleMax = 5 * time.Minute
)

// crashFile is what a Before phase leaves for its After phase.
type crashFile struct {
	Hash         string `json:"hash"`
	Preimage     string `json:"preimage"`
	Invoice      string `json:"invoice"`
	AmountMsat   int64  `json:"amount_msat"`
	SatBalBefore int64  `json:"satbooth_spendable_msat_before"`
	MerBalBefore int64  `json:"merchant_spendable_msat_before"`

	// relay loss
	DedupEvent    json.RawMessage `json:"dedup_event,omitempty"`
	StrayDesc     string          `json:"stray_description,omitempty"`
	NotifyHash    string          `json:"notify_hash,omitempty"`
	NotifyInvoice string          `json:"notify_invoice,omitempty"`
	NotifyMsat    int64           `json:"notify_msat,omitempty"`
}

func crashPath(t testing.TB, name string) string {
	return filepath.Join(stateDir(t), "crash-"+name+".json")
}

func saveCrash(t testing.TB, name string, c *crashFile) {
	t.Helper()
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crashPath(t, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadCrash(t testing.TB, name string) *crashFile {
	t.Helper()
	b, err := os.ReadFile(crashPath(t, name))
	if err != nil {
		t.Fatalf("crash file (did the Before phase run?): %v", shownErr(err))
	}
	var c crashFile
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// logRows records both hubs' rows for a hash, in a form REPORT.md can quote.
func (w *world) logRows(t *testing.T, label, hash string) {
	t.Helper()
	for _, h := range []struct {
		name string
		api  *hubAPI
	}{{"satbooth", w.sat}, {"merchant", w.mer}} {
		rows, err := h.api.rowsFor(hash)
		if err != nil {
			t.Logf("MEASURED rows label=%s hub=%s error=%v", label, h.name, shownErr(err))
			continue
		}
		if len(rows) == 0 {
			t.Logf("MEASURED rows label=%s hub=%s none", label, h.name)
		}
		for _, r := range rows {
			t.Logf("MEASURED rows label=%s hub=%s type=%s state=%s amount_msat=%d fees_msat=%d failure_bytes=%d", label, h.name, shownType(r.Type), shownState(r.State), r.AmountMsat, r.FeesMsat, len(r.Failure))
		}
		if single, err := h.api.transaction(hash); err == nil {
			t.Logf("MEASURED lookup label=%s hub=%s type=%s state=%s", label, h.name, shownType(single.Type), shownState(single.State))
		} else {
			t.Logf("MEASURED lookup label=%s hub=%s error=%v", label, h.name, shownErr(err))
		}
	}
}

// sendDedupRequest publishes a get_info request with a 10 minute expiration on the info connection and waits for the
// hub's one answer to it (answers to other requests in the relay's window are skipped). It returns the event for a
// replay after the hub restarts.
func (w *world) sendDedupRequest(t *testing.T) *nostr.Event {
	t.Helper()
	ctx := ctxFor(t, 2*time.Minute)
	k, wallet := w.keys["info"], w.st.Wallets["info"]
	rc, err := dialRaw(ctx, w.st.RelayURLs[hubSatbooth], k)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.close()
	if err := rc.req(ctx, "r", `{"kinds":[23195],"#p":["`+k.PublicKeyHex()+`"],"authors":["`+wallet+`"]}`); err != nil {
		t.Fatal(err)
	}
	ev, err := nwc.BuildRequest(k, wallet, "get_info", map[string]any{}, time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ok, msg, err := rc.publish(ctx, ev); err != nil || !ok {
		t.Fatalf("publish get_info: %v %s", shownErr(err), msg)
	}
	for {
		resp, err := rc.nextEvent(ctx, "r")
		if err != nil {
			t.Fatalf("no answer to get_info: %v", shownErr(err))
		}
		if id, _, err := nwc.OpenResponse(resp, k, wallet); err == nil && id == ev.ID {
			return ev
		}
	}
}

// replayDedupRequest publishes the saved request again after the hub restarted (a fresh subscription without a
// "since", an empty pool cache, the relay recreated with an empty window; the hub's database persists). The hub must
// not answer it: only an answer whose request id is the replayed event's counts. That the hub received it at all is
// proven by scripts/qualify.sh, which counts the hub's "Event already processed" log line (getAlby/hub 921003b
// nip47/event_handler.go:57-66) before and after the phase.
func (w *world) replayDedupRequest(t *testing.T, name string, raw json.RawMessage) {
	t.Helper()
	ev, err := nostr.ParseEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	k, wallet := w.keys["info"], w.st.Wallets["info"]
	ctx := ctxFor(t, 2*time.Minute)
	rc, err := dialRaw(ctx, w.st.RelayURLs[hubSatbooth], k)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.close()
	if err := rc.req(ctx, "r", `{"kinds":[23195],"#p":["`+k.PublicKeyHex()+`"],"authors":["`+wallet+`"]}`); err != nil {
		t.Fatal(err)
	}
	ok, msg, err := rc.publish(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURED crash=%s replayed_request accepted=%v message_bytes=%d", name, ok, len(msg))
	if !ok {
		t.Fatalf("the relay refused the replayed request (message of %d bytes): the hub never saw it, the dedup is not tested", len(msg))
	}
	wctx := ctxFor(t, 15*time.Second)
	for {
		resp, err := rc.nextEvent(wctx, "r")
		if err != nil {
			t.Logf("MEASURED crash=%s replayed_request no answer to the replayed id within 15 s (%v)", name, shownErr(err))
			return
		}
		if id, _, err := nwc.OpenResponse(resp, k, wallet); err == nil && id == ev.ID {
			t.Errorf("the hub answered the replayed request a second time (response %s)", resp.ID)
			return
		}
	}
}

// crashBefore: the payout of a hold invoice is in flight (the merchant saw the HTLC, SatBooth's row is pending). The
// phase ends there; the PayInvoice call may still be pending, the hub has the request.
func crashBefore(t *testing.T, name string) {
	w := newWorld(t, true, true)
	cf := &crashFile{AmountMsat: crashSats * 1000}
	cf.SatBalBefore = stableSpendable(t, w.sat)
	cf.MerBalBefore = stableSpendable(t, w.mer)
	ev := w.sendDedupRequest(t)
	var err error
	if cf.DedupEvent, err = json.Marshal(ev); err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURED crash=%s before get_info answered once; request saved for the replay", name)
	h := w.startHoldKeep(t, crashSats, true)
	cf.Hash, cf.Preimage, cf.Invoice = h.hash, h.preimage, h.invoice
	w.logRows(t, name+"-before", h.hash)
	b := w.budget(t)
	t.Logf("MEASURED crash=%s before sats=%d budget_used_msat=%d", name, crashSats, b.UsedMsat)
	saveCrash(t, name, cf)
}

// lookupUntilAnswered repeats lookup_invoice while the hub is not answering yet (it restarted a moment ago) and
// returns the first answer with the time it took.
func (w *world) lookupUntilAnswered(t *testing.T, hash string) (*nwc.Transaction, error, time.Duration, int) {
	t.Helper()
	began := time.Now()
	for attempts := 1; ; attempts++ {
		tx, err := w.pay.LookupInvoice(ctxFor(t, 3*time.Minute), hash)
		if err == nil || !errors.Is(err, nwc.ErrNoResponse) || time.Since(began) > 4*time.Minute {
			return tx, err, time.Since(began), attempts
		}
	}
}

// crashAfter runs these steps: look the payout up, retry it, settle the hold invoice, wait for settled, then check the
// safety property on both hubs. Observations that break an expectation but not the property are recorded and fail
// the test at the end; a broken property is reported as SAFETY VIOLATION.
func crashAfter(t *testing.T, name string) {
	cf := loadCrash(t, name)
	w := newWorld(t, false, true)
	hash := cf.Hash
	w.logRows(t, name+"-after-restart", hash)
	// Precondition: the merchant still holds the HTLC (its row is accepted), so the payout really was in flight.
	if row, err := w.mer.transaction(hash); err != nil || !strings.EqualFold(row.State, "accepted") {
		t.Fatalf("precondition: the merchant's row after the restart is %+v (%v), want accepted", row, shownErr(err))
	}
	// The hub's own request dedup, across a hub restart.
	w.replayDedupRequest(t, name, cf.DedupEvent)

	// 1. lookup_invoice right after the restart.
	tx, err, took, attempts := w.lookupUntilAnswered(t, hash)
	if err != nil {
		t.Logf("MEASURED crash=%s lookup_after_restart error=%v seconds=%.1f attempts=%d", name, shownErr(err), took.Seconds(), attempts)
	} else {
		t.Logf("MEASURED crash=%s lookup_after_restart type=%s state=%s seconds=%.1f attempts=%d", name, shownType(tx.Type), shownState(tx.State), took.Seconds(), attempts)
	}
	w.waitChannel(t)

	// 2. the same invoice again.
	began := time.Now()
	r, err := w.pay.PayInvoice(ctxFor(t, 3*time.Minute), cf.Invoice, 0)
	var we *nwc.WalletError
	switch {
	case err == nil:
		t.Logf("MEASURED crash=%s retry answer=SUCCESS after_seconds=%.1f fees_msat=%d preimage_matches=%v", name, time.Since(began).Seconds(), r.FeesPaidMsat, hashOf(r.Preimage) == hash)
	case errors.As(err, &we):
		t.Logf("MEASURED crash=%s retry answer=wallet_error code=%s message_bytes=%d after_seconds=%.1f", name, shownCode(we.Code), len(we.Message), time.Since(began).Seconds())
	case errors.Is(err, nwc.ErrNoResponse):
		t.Logf("MEASURED crash=%s retry answer=none ErrNoResponse after_seconds=%.1f", name, time.Since(began).Seconds())
	default:
		t.Logf("MEASURED crash=%s retry answer=other error=%v after_seconds=%.1f", name, shownErr(err), time.Since(began).Seconds())
	}
	w.logRows(t, name+"-after-retry", hash)

	// 3. settle the hold invoice on the merchant's side.
	var settleErr error
	settleAttempts := 0
	settleBegan := time.Now()
	poll(t, "settle_hold_invoice", 3*time.Minute, 3*time.Second, func() (bool, error) {
		settleAttempts++
		settleErr = w.mc.SettleHoldInvoice(ctxFor(t, time.Minute), cf.Preimage)
		return settleErr == nil, settleErr
	})
	t.Logf("MEASURED crash=%s settle ok after_seconds=%.1f attempts=%d", name, time.Since(settleBegan).Seconds(), settleAttempts)

	// 4. wait for the payout to read settled, logging each state change.
	last := ""
	settled := false
	settleStart := time.Now()
	for time.Since(settleStart) < crashSettleMax {
		got, err := w.pay.LookupInvoice(ctxFor(t, 2*time.Minute), hash)
		cur := "error: " + errString(err)
		if err == nil {
			cur = got.Type + "/" + got.State
		}
		if cur != last {
			t.Logf("MEASURED crash=%s payout_lookup t+%.1fs %s", name, time.Since(settleStart).Seconds(), cur)
			last = cur
		}
		if err == nil && got.State == nwc.StateSettled {
			settled = true
			break
		}
		time.Sleep(2 * time.Second)
	}

	// 5. the safety property, from both hubs' records.
	w.checkSafety(t, name, cf, settled)
	if !settled {
		t.Errorf("the payout did not read settled within %s", crashSettleMax)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return shownErr(err)
}

// checkSafety reads the merchant hub's rows (incoming, credited once) and SatBooth's hub's rows (outgoing, paid once)
// and both stabilised balances.
func (w *world) checkSafety(t *testing.T, name string, cf *crashFile, payoutSettled bool) {
	t.Helper()
	hash := cf.Hash
	// Give a late second credit or payment the chance to show: wait for the merchant's row to read settled, then for
	// balances to stop moving.
	poll(t, "merchant row settled", 2*time.Minute, 2*time.Second, func() (bool, error) {
		row, err := w.mer.transaction(hash)
		if err != nil {
			return false, err
		}
		return strings.EqualFold(row.State, "settled"), errors.New("merchant row state " + row.State)
	})
	merBal := stableSpendable(t, w.mer)
	satBal := stableSpendable(t, w.sat)
	merRows, err := w.mer.rowsFor(hash)
	if err != nil {
		t.Fatal(err)
	}
	satRows, err := w.sat.rowsFor(hash)
	if err != nil {
		t.Fatal(err)
	}
	w.logRows(t, name+"-final", hash)

	var merIn, merInSettled, satOut, satOutSettled, satOutPending int
	var fee int64
	for _, r := range merRows {
		if r.Type == nwc.TypeIncoming {
			merIn++
			if strings.EqualFold(r.State, "settled") {
				merInSettled++
			}
		}
	}
	for _, r := range satRows {
		if r.Type == nwc.TypeOutgoing {
			satOut++
			switch strings.ToLower(r.State) {
			case "settled":
				satOutSettled++
				fee = int64(r.FeesMsat)
			case "pending":
				satOutPending++
			}
		}
	}
	merDelta := merBal - cf.MerBalBefore
	satDelta := satBal - cf.SatBalBefore
	t.Logf("MEASURED crash=%s safety merchant_incoming_rows=%d settled=%d merchant_balance_delta_msat=%d (invoice %d)", name, merIn, merInSettled, merDelta, cf.AmountMsat)
	t.Logf("MEASURED crash=%s safety satbooth_outgoing_rows=%d settled=%d pending=%d satbooth_balance_delta_msat=%d (amount+fee %d)", name, satOut, satOutSettled, satOutPending, satDelta, cf.AmountMsat+fee)

	if merInSettled > 1 || merDelta > cf.AmountMsat {
		t.Errorf("SAFETY VIOLATION: the merchant was credited more than once (settled incoming rows %d, balance +%d msat for an invoice of %d)", merInSettled, merDelta, cf.AmountMsat)
	}
	if satOutSettled > 1 || -satDelta > cf.AmountMsat+fee {
		t.Errorf("SAFETY VIOLATION: SatBooth's hub paid more than once (settled outgoing rows %d, balance %d msat for an invoice of %d + fee %d)", satOutSettled, satDelta, cf.AmountMsat, fee)
	}
	// Expected end state, beyond the property: paid and credited exactly once.
	if payoutSettled {
		if merInSettled != 1 || merDelta != cf.AmountMsat {
			t.Errorf("the merchant should be credited exactly once: settled rows %d, balance +%d msat of %d", merInSettled, merDelta, cf.AmountMsat)
		}
		if satOutSettled != 1 || satDelta != -(cf.AmountMsat+fee) {
			t.Errorf("SatBooth should have paid exactly once: settled rows %d, balance %d msat, want -%d", satOutSettled, satDelta, cf.AmountMsat+fee)
		}
		if satOutPending != 0 {
			t.Errorf("%d outgoing rows still pending", satOutPending)
		}
	}
}

func TestCrashBefore(t *testing.T)    { crashBefore(t, "stop") }
func TestCrashAfterStop(t *testing.T) { crashAfter(t, "stop") }
func TestKillBefore(t *testing.T)     { crashBefore(t, "kill") }
func TestKillAfter(t *testing.T)      { crashAfter(t, "kill") }

// --- relay loss -------------------------------------------------------------------------------------------------

const strayDesc = "relay loss stray invoice"

// TestRelayLossBefore prepares two things, then scripts/qualify.sh stops relay-satbooth:
//   - a make_invoice request whose response nobody reads: the hub makes the invoice, the response sits in the relay's
//     window, and the restart discards it;
//   - an invoice the customer pays in TestRelayLossDuring while the relay is down.
//
// The relay answers a client within a tenth of a second, so no restart can catch a make_invoice of the real client in
// flight; publishing without a subscription loses the response the same way.
func TestRelayLossBefore(t *testing.T) {
	w := newWorld(t, true, true)
	cf := &crashFile{StrayDesc: strayDesc}
	ctx := ctxFor(t, 2*time.Minute)

	// The make_invoice request whose response is never read.
	rc2, err := dialRaw(ctx, w.st.RelayURLs[hubSatbooth], w.keys["invoice"])
	if err != nil {
		t.Fatal(err)
	}
	defer rc2.close()
	mi, err := nwc.BuildRequest(w.keys["invoice"], w.st.Wallets["invoice"], "make_invoice", map[string]any{"amount": 7_000_000, "description": strayDesc}, time.Now(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ok, msg, err := rc2.publish(ctx, mi); err != nil || !ok {
		t.Fatalf("publish make_invoice: %v %s", shownErr(err), msg)
	}
	poll(t, "the stray invoice exists", time.Minute, time.Second, func() (bool, error) {
		return findByDescription(t, w, strayDesc) > 0, errors.New("not listed yet")
	})
	t.Logf("MEASURED relayloss before stray invoices=%d (response never read)", findByDescription(t, w, strayDesc))

	// The invoice to be paid while the relay is down.
	n, err := w.inv.MakeInvoice(ctxFor(t, time.Minute), nwc.MakeInvoiceParams{AmountMsat: 4_000_000, Description: "paid while the relay is down"})
	if err != nil {
		t.Fatal(err)
	}
	cf.NotifyHash, cf.NotifyInvoice, cf.NotifyMsat = n.PaymentHash, n.Invoice, n.AmountMsat
	saveCrash(t, "relayloss", cf)
}

// findByDescription counts SatBooth's invoice-connection rows (unpaid included) with this description.
func findByDescription(t *testing.T, w *world, desc string) int {
	t.Helper()
	list, _, err := w.inv.ListTransactions(ctxFor(t, time.Minute), nwc.ListParams{Limit: nwc.MaxListLimit, Unpaid: true, Type: nwc.TypeIncoming})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, x := range list {
		if x.Description == desc {
			n++
		}
	}
	return n
}

// TestRelayLossDuring runs while relay-satbooth is stopped: the customer pays through the hub API, which needs no
// relay. The hub's notification for it has nowhere to go.
func TestRelayLossDuring(t *testing.T) {
	cf := loadCrash(t, "relayloss")
	w := newWorld(t, false, false)
	began := time.Now()
	if _, err := w.mer.payInvoice(cf.NotifyInvoice); err != nil {
		t.Fatalf("customer payment: %v", shownErr(err))
	}
	t.Logf("MEASURED relayloss during customer_payment_seconds=%.2f", time.Since(began).Seconds())
	row, err := w.sat.transaction(cf.NotifyHash)
	if err != nil || !strings.EqualFold(row.State, "settled") {
		t.Fatalf("SatBooth hub row after the payment: %+v %v", row, shownErr(err))
	}
}

// TestRelayLossAfter runs after relay-satbooth is back (its window is empty).
func TestRelayLossAfter(t *testing.T) {
	cf := loadCrash(t, "relayloss")
	w := newWorld(t, false, true)

	// Liveness: the hub answers a fresh request, so it is subscribed again. Time it from the first call.
	began := time.Now()
	for attempts := 1; ; attempts++ {
		_, err := w.info.GetInfo(ctxFor(t, 3*time.Minute))
		if err == nil {
			t.Logf("MEASURED relayloss after hub_answers_again_seconds=%.1f attempts=%d", time.Since(began).Seconds(), attempts)
			break
		}
		if !errors.Is(err, nwc.ErrNoResponse) || time.Since(began) > 4*time.Minute {
			t.Fatalf("get_info after the relay came back: %v", shownErr(err))
		}
	}

	// Lost notification: the payment happened while the relay was down.
	if w.invN.has(nwc.NotifPaymentReceived, cf.NotifyHash) {
		t.Error("a payment_received notification arrived for the payment made while the relay was down")
	}
	got, err := w.inv.LookupInvoice(ctxFor(t, time.Minute), cf.NotifyHash)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURED relayloss after lost_notification lookup_state=%s notification_seen=%v", shownState(got.State), w.invN.has(nwc.NotifPaymentReceived, cf.NotifyHash))
	if got.State != nwc.StateSettled {
		t.Errorf("lookup_invoice of the invoice paid while the relay was down says %s, want settled", shownState(got.State))
	}

	// Lost invoice response: the stray invoice exists, a resend works.
	if n := findByDescription(t, w, cf.StrayDesc); n != 1 {
		t.Errorf("list_transactions shows %d stray invoices, want 1", n)
	}
	again, err := w.inv.MakeInvoice(ctxFor(t, time.Minute), nwc.MakeInvoiceParams{AmountMsat: 7_000_000, Description: cf.StrayDesc})
	if err != nil {
		t.Fatalf("resent make_invoice: %v", shownErr(err))
	}
	n := findByDescription(t, w, cf.StrayDesc)
	t.Logf("MEASURED relayloss after resend ok hash_new=%v invoices_with_the_description=%d", again.PaymentHash != "", n)
	if n != 2 {
		t.Errorf("after the resend %d invoices carry the description, want 2 (the stray one and the new one)", n)
	}
}
