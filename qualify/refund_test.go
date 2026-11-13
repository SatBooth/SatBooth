//go:build qualify

package qualify

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/memlock"
	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/refund"
	"github.com/satbooth/satbooth/service"
	"github.com/satbooth/satbooth/store"
)

// The refund phases (plan 9): the whole service runs in-process with the payout block, against relay-satbooth and the
// bootstrap's payout connection (pay_invoice, lookup_invoice and get_info, a 50,000 sat budget that is never renewed, shared with
// the crash phases). The merchant's hub plays both the customer (it pays the order) and the claimant's wallet (it makes
// the invoice the refund pays). The approval key is generated here from a fresh random seed, kept in the phase file
// (regtest only, like everything under /state), and approvals are signed in-process with approval.SignRefund and
// approval.SignVoid. Every phase but the budget phase shares one environment (/state/lightning/refund).
//
// Budget arithmetic (the payout connection): before these phases about 7,000 sat are used (QualifyTest's 3,000 sat
// payout, the two crash phases' 2,000 sat each; the cancelled holds free theirs). The refunds pay 1,000 (Pay) + 2,000
// (Partials) + 100 (HubKnown's direct payment) + 2,000 (FailedAfter) = 5,100 sat with zero fees on the direct channel,
// so about 38,000 sat stay; the budget phase claims more than whatever is left and pays nothing. The release image's
// TestImageRefund pays 1,000 sat more after these phases (qualify/image_test.go).
const (
	refundEnv       = "refund"
	refundBudgetEnv = "refund-budget"
	refundOrderMsat = 2_000_000
	// refundBound covers the sender (woken by the claim), the hub's answer and one resolver round (30 s).
	refundBound = 2 * time.Minute
	// childEnv names the service config of the crash phase's child process.
	childEnv = "SATBOOTH_REFUND_CHILD_CONFIG"
)

// refundFile is the refund environment's phase file: the approval seed and what a later phase reads.
type refundFile struct {
	Seed string `json:"seed"` // the regtest-only approval key's seed, 64 hex characters

	CrashID, CrashOrder, CrashHash string
	CutID, CutOrder, CutHash       string

	FailedOrder, FailedInvoice, FailedHash, FailedID string
}

// rfEnv is a Lightning environment with the payout block and the refund API keys.
type rfEnv struct {
	*lnEnv
	relayURL     string
	priv         ed25519.PrivateKey
	rwKey, rrKey string
	f            *refundFile
	fpath        string
}

// refundJSON is the API's refund view (the allowlist).
type refundJSON struct {
	ID         string  `json:"id"`
	OrderID    string  `json:"order_id"`
	AmountMsat int64   `json:"amount_msat"`
	State      string  `json:"state"`
	Reason     string  `json:"reason"`
	ClaimedAt  string  `json:"claimed_at"`
	ResolvedAt *string `json:"resolved_at"`
}

// newRefundEnv is newEnvAt plus the payout block (the payout connection through relayURL, empty: relay-satbooth's own),
// the approval key from the phase file (made on first use) and two more API keys, refunds:write and refunds:read.
func newRefundEnv(t *testing.T, name, relayURL string) *rfEnv {
	t.Helper()
	root := filepath.Join(stateDir(t), "lightning", name)
	le := newEnvAt(t, name, root, relayURL, nil, nil, nil)
	if relayURL == "" {
		relayURL = le.st.RelayURLs[hubSatbooth]
	}
	e := &rfEnv{lnEnv: le, relayURL: relayURL, fpath: filepath.Join(root, "refund.json"), f: &refundFile{}}
	if b, err := os.ReadFile(e.fpath); err == nil {
		if err := json.Unmarshal(b, e.f); err != nil {
			t.Fatal(err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if e.f.Seed == "" {
		var seed [ed25519.SeedSize]byte
		if _, err := rand.Read(seed[:]); err != nil {
			t.Fatal(err)
		}
		e.f.Seed = hex.EncodeToString(seed[:])
		e.saveFile()
	}
	seed, err := hex.DecodeString(e.f.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatal("refund.json: bad seed")
	}
	e.priv = ed25519.NewKeyFromSeed(seed)

	e.rwKey, e.rrKey = randHex(t), randHex(t)
	writeJSON0600(t, filepath.Join(root, "api-keys.json"), map[string]any{"keys": []map[string]string{
		{"id": "qualify-write", "capability": "orders:write", "sha256": sha256Hex(e.writeKey)},
		{"id": "qualify-read", "capability": "orders:read", "sha256": sha256Hex(e.readKey)},
		{"id": "qualify-refunds-write", "capability": "refunds:write", "sha256": sha256Hex(e.rwKey)},
		{"id": "qualify-refunds-read", "capability": "refunds:read", "sha256": sha256Hex(e.rrKey)},
	}})

	b, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg service.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Payout = &service.PayoutConfig{
		RelayURL: relayURL, WalletPubKey: le.st.Wallets["payout"], ClientKeyFile: le.st.KeyFiles["payout"],
		TimeoutSeconds: 70, Network: "regtest",
		ApprovalKeys: []string{hex.EncodeToString(e.priv.Public().(ed25519.PublicKey))},
		MaxMsat:      100_000_000, DailyMsat: 400_000_000,
	}
	writeJSON0600(t, e.cfgPath, cfg)
	return e
}

func (e *rfEnv) saveFile() { writeJSON0600(e.t, e.fpath, e.f) }

// paidOrder creates an order of msat, the customer pays it from the merchant's hub, and it reads paid.
func (e *rfEnv) paidOrder(ref string, msat int64) orderJSON {
	e.t.Helper()
	o := e.createOrder(ref+"-"+randHex(e.t)[:8], msat, 3600)
	bolt11 := e.pressPay(tokenOf(o))
	began := time.Now()
	e.customerPays(bolt11)
	e.waitOrder(o.ID, "paid", began, paidBound)
	return o
}

// figures reads the order's Lightning figures: paid, refunded (settled) and open (claimed or sent) msat.
func (e *rfEnv) figures(orderID string) (paid, refunded, open int64) {
	e.t.Helper()
	o := e.order(orderID)
	l := o.Lightning
	if l == nil || l.PaidMsat == nil || l.RefundedMsat == nil || l.RefundOpenMsat == nil {
		e.t.Fatalf("order %s: the Lightning figures are missing", orderID)
	}
	return *l.PaidMsat, *l.RefundedMsat, *l.RefundOpenMsat
}

func (e *rfEnv) wantFigures(orderID string, paid, refunded, open int64) {
	e.t.Helper()
	p, r, o := e.figures(orderID)
	if p != paid || r != refunded || o != open {
		e.t.Errorf("order %s: figures paid %d refunded %d open %d, want %d %d %d", orderID, p, r, o, paid, refunded, open)
	}
	e.t.Logf("MEASURED refund order figures paid_msat=%d refunded_msat=%d refund_open_msat=%d", p, r, o)
}

// approve signs a refund approval for msat of orderID, valid for ttl, under a fresh random refund id.
func (e *rfEnv) approve(orderID string, msat int64, ttl time.Duration) (id, hexApproval string) {
	e.t.Helper()
	var r approval.Refund
	if _, err := rand.Read(r.RefundID[:]); err != nil {
		e.t.Fatal(err)
	}
	ob, err := hex.DecodeString(orderID)
	if err != nil || len(ob) != 16 {
		e.t.Fatalf("order id %q is not 32 hex characters", orderID)
	}
	copy(r.OrderID[:], ob)
	r.Network, r.KeyID = approval.Regtest, approval.KeyID(e.priv.Public().(ed25519.PublicKey))
	r.AmountMsat, r.ExpiresAt = uint64(msat), time.Now().Add(ttl).Unix()
	return hex.EncodeToString(r.RefundID[:]), approval.SignRefund(r, e.priv)
}

// voidApproval signs a void approval for the refund id, valid for 10 minutes from now (sign it when the void is sent).
func (e *rfEnv) voidApproval(id string) string {
	e.t.Helper()
	var v approval.Void
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != 16 {
		e.t.Fatalf("refund id %q is not 32 hex characters", id)
	}
	copy(v.RefundID[:], b)
	v.Network, v.KeyID = approval.Regtest, approval.KeyID(e.priv.Public().(ed25519.PublicKey))
	v.ExpiresAt = time.Now().Add(10 * time.Minute).Unix()
	return approval.SignVoid(v, e.priv)
}

// decodeRefund decodes a refund view strictly (an unknown field fails).
func (e *rfEnv) decodeRefund(raw []byte) refundJSON {
	e.t.Helper()
	var v refundJSON
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		e.t.Fatalf("refund view: unexpected shape: %v", shownErr(err))
	}
	return v
}

// claim is POST /v1/refunds. It returns the status, the view of a success and the error code of a refusal.
func (e *rfEnv) claim(hexApproval, invoice string) (int, refundJSON, string) {
	e.t.Helper()
	status, raw, _ := e.call("POST", "/v1/refunds", e.rwKey, map[string]string{"approval": hexApproval, "invoice": invoice})
	if status >= 300 {
		return status, refundJSON{}, e.errCode("POST", "/v1/refunds", status, raw)
	}
	return status, e.decodeRefund(raw), ""
}

// mustClaim claims and requires a 202 with a claimed refund of the approval's id and amount.
func (e *rfEnv) mustClaim(id, hexApproval, invoice, orderID string, msat int64) refundJSON {
	e.t.Helper()
	status, v, code := e.claim(hexApproval, invoice)
	if status != 202 {
		e.t.Fatalf("claim: status %d (%s), want 202", status, code)
	}
	if v.ID != id || v.OrderID != orderID || v.AmountMsat != msat || v.State != refund.StateClaimed || v.Reason != "" || v.ResolvedAt != nil {
		e.t.Fatalf("claim: view %+v", v)
	}
	return v
}

// wantRefused claims and requires the refusal status and code.
func (e *rfEnv) wantRefused(what, hexApproval, invoice string, status int, code string) {
	e.t.Helper()
	s, _, c := e.claim(hexApproval, invoice)
	if s != status || c != code {
		e.t.Errorf("%s: status %d code %q, want %d %q", what, s, c, status, code)
	}
	e.t.Logf("MEASURED refund refused case=%s status=%d code=%s", what, s, c)
}

// refund is GET /v1/refunds/{id}.
func (e *rfEnv) refund(id string) refundJSON {
	e.t.Helper()
	status, raw, _ := e.call("GET", "/v1/refunds/"+id, e.rrKey, nil)
	if status != 200 {
		e.t.Fatalf("get refund: status %d", status)
	}
	return e.decodeRefund(raw)
}

// waitRefund polls until the refund reads state (and reason), and returns how long it took from since.
func (e *rfEnv) waitRefund(id, state, reason string, since time.Time, bound time.Duration) (refundJSON, time.Duration) {
	e.t.Helper()
	var v refundJSON
	var d time.Duration
	poll(e.t, "refund "+state+" "+reason, bound, 250*time.Millisecond, func() (bool, error) {
		v = e.refund(id)
		if v.State == state && v.Reason == reason {
			d = time.Since(since)
			return true, nil
		}
		return false, fmt.Errorf("state %s reason %q", v.State, v.Reason)
	})
	if (v.ResolvedAt != nil) != (state == refund.StateSettled || state == refund.StateFailed) {
		e.t.Errorf("refund %s: resolved_at %v for state %s", id, v.ResolvedAt, state)
	}
	return v, d
}

// merchantInvoice is an invoice of sats from the merchant's hub: the claimant's wallet.
func (e *rfEnv) merchantInvoice(sats uint64, desc string) *hubTx {
	e.t.Helper()
	tx, err := e.mer.makeInvoice(sats, desc)
	if err != nil || tx.Invoice == "" || tx.PaymentHash == "" {
		e.t.Fatalf("merchant invoice: %v", shownErr(err))
	}
	return tx
}

// rows lists a hub's own rows for the payment hash as type/state/amount strings.
func (e *rfEnv) rows(h *hubAPI, hash string) []hubTx {
	e.t.Helper()
	rs, err := h.rowsFor(hash)
	if err != nil {
		e.t.Fatal(err)
	}
	return rs
}

// paidOnce checks both hubs' records of a settled refund: one settled outgoing row of msat at SatBooth's hub and one
// settled incoming row of msat at the merchant's (the claimant received exactly the amount, once). It returns the fee.
func (e *rfEnv) paidOnce(hash string, msat int64) int64 {
	e.t.Helper()
	out := e.rows(e.sat, hash)
	in := e.rows(e.mer, hash)
	if len(out) != 1 || out[0].Type != "outgoing" || out[0].State != "settled" || int64(out[0].AmountMsat) != msat {
		e.t.Errorf("SatBooth's hub: rows %v for the refund, want one outgoing settled of %d msat", rowStates(out), msat)
	}
	if len(in) != 1 || in[0].Type != "incoming" || in[0].State != "settled" || int64(in[0].AmountMsat) != msat {
		e.t.Errorf("merchant hub: rows %v for the refund, want one incoming settled of %d msat", rowStates(in), msat)
	}
	var fee int64
	if len(out) == 1 {
		fee = int64(out[0].FeesMsat)
	}
	e.t.Logf("MEASURED refund hub rows satbooth=%v merchant=%v amount_msat=%d fee_msat=%d", rowStates(out), rowStates(in), msat, fee)
	return fee
}

// noRows checks that neither hub has a row for the payment hash: the request never reached SatBooth's hub, and nothing
// was paid to the merchant's.
func (e *rfEnv) noRows(hash string) {
	e.t.Helper()
	out, in := e.rows(e.sat, hash), e.rows(e.mer, hash)
	if len(out) != 0 || len(in) != 0 {
		e.t.Errorf("rows for a refund the hub never received: satbooth %v merchant %v, want none", rowStates(out), rowStates(in))
	}
}

// dialPayout dials the payout connection as the service does (through e.relayURL).
func (e *rfEnv) dialPayout() *nwc.Client {
	e.t.Helper()
	k := e.st.key(e.t, "payout")
	c, err := nwc.Dial(ctxFor(e.t, time.Minute), nwc.Config{RelayURL: e.relayURL, WalletPubKey: e.st.Wallets["payout"], Key: k, Timeout: 70 * time.Second})
	if err != nil {
		e.t.Fatalf("dial payout: %v", shownErr(err))
	}
	e.t.Cleanup(func() { _ = c.Close() })
	return c
}

// budget reads the payout connection's budget on a connection of its own (while no service runs).
func (e *rfEnv) budget() *nwc.Budget {
	e.t.Helper()
	c := e.dialPayout()
	defer c.Close()
	b, err := c.GetBudget(ctxFor(e.t, time.Minute))
	if err != nil || b == nil || !b.Set {
		e.t.Fatalf("payout budget: %+v %v", b, shownErr(err))
	}
	e.t.Logf("MEASURED refund payout_budget used_msat=%d total_msat=%d renewal=%s", b.UsedMsat, b.TotalMsat, shownRenewal(b.RenewalPeriod))
	return b
}

// ledgerState reads the refund's three ledger transfers (after the service stopped): the pending, its post and its
// void.
func (e *rfEnv) ledgerState(id string) (pending, posted, voided bool) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	lg, err := ledger.Open(ctx, filepath.Join(e.dataDir, "ledger.db"))
	if err != nil {
		e.t.Fatalf("ledger: %v", shownErr(err))
	}
	defer lg.Close()
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != 16 {
		e.t.Fatalf("refund id %q", id)
	}
	rid := [16]byte(b)
	for _, x := range []struct {
		id  ledger.U128
		out *bool
	}{{refund.RefundTransferID(rid), &pending}, {refund.PostTransferID(rid), &posted}, {refund.VoidTransferID(rid), &voided}} {
		_, found, err := lg.LookupTransfer(ctx, x.id)
		if err != nil {
			e.t.Fatal(err)
		}
		*x.out = found
	}
	return pending, posted, voided
}

// wantLedger checks the refund's ledger transfers: settled posts the pending, failed after a send voids it, and a
// refund that failed before the send has none.
func (e *rfEnv) wantLedger(id string, pending, posted, voided bool) {
	e.t.Helper()
	p, po, v := e.ledgerState(id)
	if p != pending || po != posted || v != voided {
		e.t.Errorf("refund %s: ledger pending %v posted %v voided %v, want %v %v %v", id, p, po, v, pending, posted, voided)
	}
	e.t.Logf("SAFETY refund=%s ledger pending=%v posted=%v voided=%v", id, p, po, v)
}

// storeState reads the refund's row (after the service stopped).
func (e *rfEnv) storeState(id string) store.RefundState {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	st, err := store.Open(ctx, filepath.Join(e.dataDir, "store.db"))
	if err != nil {
		e.t.Fatalf("store: %v", shownErr(err))
	}
	defer st.Close()
	rs, found, err := st.RefundStateByID(ctx, id)
	if err != nil || !found {
		e.t.Fatalf("refund %s in the store: found %v, %v", id, found, shownErr(err))
	}
	return rs
}

// allowAlert lets an ALERT line through stopAllow: its message, and the refund ids it may name (none: any refund).
type allowAlert struct {
	msg string
	ids []string
}

// allowed reports whether the log line l is the ALERT a, for one of its refund ids.
func (a allowAlert) allowed(l string) bool {
	if !strings.Contains(l, `"msg":"`+a.msg+`"`) {
		return false
	}
	if len(a.ids) == 0 {
		return true
	}
	for _, id := range a.ids {
		if id != "" && strings.Contains(l, `"refund_id":"`+id+`"`) {
			return true
		}
	}
	return false
}

// stopAllow stops the service like svcRun.stop, except that an ALERT line allowed by one of allowed passes. It
// returns the allowed ALERT lines, which the phase checks.
func stopAllow(s *svcRun, allowed ...allowAlert) []string {
	s.t.Helper()
	s.stopQuiet()
	logs := s.logs.String()
	s.t.Logf("service log (%d lines)", strings.Count(logs, "\n"))
	var seen []string
	for _, l := range strings.Split(logs, "\n") {
		if !strings.Contains(l, "ALERT") && !strings.Contains(l, "startup failed") && !strings.Contains(l, "unexpectedly") {
			continue
		}
		ok := false
		for _, a := range allowed {
			if a.allowed(l) {
				ok = true
			}
		}
		if !ok {
			s.t.Errorf("service log line: %s", l)
			continue
		}
		seen = append(seen, l)
		s.t.Logf("expected ALERT line: %s", l)
	}
	return seen
}

// alertFor reports whether the service's log so far holds the ALERT msg for the refund id.
func alertFor(s *svcRun, msg, id string) bool {
	for _, l := range strings.Split(s.logs.String(), "\n") {
		if strings.Contains(l, `"msg":"`+msg+`"`) && strings.Contains(l, `"refund_id":"`+id+`"`) {
			return true
		}
	}
	return false
}

// earlier is the ALERT a phase in the shared environment may see from earlier phases' refunds: the crash and cut
// refunds (and the ids given, a phase's own) read NOT_FOUND at the hub, once per process each, until
// TestRefundVoidUnknown voids them. Any other refund's NOT_FOUND fails the phase.
func (e *rfEnv) earlier(ids ...string) allowAlert {
	return allowAlert{refund.AlertNotFound, append([]string{e.f.CrashID, e.f.CutID}, ids...)}
}

// countingWallet is the real payout connection with its pay_invoice and lookup_invoice calls counted.
type countingWallet struct {
	*nwc.Client
	pays, lookups atomic.Int64
}

func (w *countingWallet) PayInvoiceUntil(ctx context.Context, inv string, msat int64,
	until time.Time) (*nwc.PayResult, error) {
	w.pays.Add(1)
	return w.Client.PayInvoiceUntil(ctx, inv, msat, until)
}

func (w *countingWallet) LookupInvoiceByString(ctx context.Context, inv string) (*nwc.Transaction, error) {
	w.lookups.Add(1)
	return w.Client.LookupInvoiceByString(ctx, inv)
}

// ---- the phases ----

// TestRefundPay: a claim of half a paid order settles; the merchant's hub received exactly the amount once; the order
// figures move; the budget counts amount + fee; a replayed approval (refund_exists) and a reused invoice (invalid_invoice)
// are refused. First, an invoice whose own amount differs from the approval's by one sat is refused at claim
// (invalid_invoice) with no hub call: no lookup, no pay, no row at either hub.
func TestRefundPay(t *testing.T) {
	e := newRefundEnv(t, refundEnv, "")
	b0 := e.budget()
	cw := &countingWallet{Client: e.dialPayout()}
	e.opts = []service.Option{service.WithPayoutWallet(cw)}
	s := e.start()
	o := e.paidOrder("refund-pay", refundOrderMsat)
	e.wantFigures(o.ID, refundOrderMsat, 0, 0)

	const msat = 1_000_000
	other := e.merchantInvoice(msat/1000+1, "refund pay, other amount")
	_, apOther := e.approve(o.ID, msat, time.Hour)
	l0, p0 := cw.lookups.Load(), cw.pays.Load()
	e.wantRefused("invoice amount differs", apOther, other.Invoice, 400, "invalid_invoice")
	time.Sleep(refund.SendEvery + time.Second) // a sender round: a negative check has to wait
	if l, p := cw.lookups.Load()-l0, cw.pays.Load()-p0; l != 0 || p != 0 {
		t.Errorf("the refused claim made %d lookups and %d pays, want none", l, p)
	}
	e.noRows(other.PaymentHash)
	e.wantFigures(o.ID, refundOrderMsat, 0, 0)

	inv := e.merchantInvoice(msat/1000, "refund pay")
	id, ap := e.approve(o.ID, msat, time.Hour)
	began := time.Now()
	e.mustClaim(id, ap, inv.Invoice, o.ID, msat)
	t.Logf("MEASURED refund claim_seconds=%.2f", time.Since(began).Seconds())
	_, d := e.waitRefund(id, refund.StateSettled, "", began, refundBound)
	t.Logf("MEASURED refund pay claim_to_settled_seconds=%.1f (one resolver round is 30 s)", d.Seconds())

	e.wantRefused("replayed approval", ap, inv.Invoice, 409, "refund_exists")
	_, ap2 := e.approve(o.ID, msat, time.Hour)
	e.wantRefused("reused invoice", ap2, inv.Invoice, 400, "invalid_invoice") // one refund per invoice
	e.wantFigures(o.ID, refundOrderMsat, msat, 0)
	fee := e.paidOnce(inv.PaymentHash, msat)
	e.settleQuiet()
	stopAllow(s, e.earlier())

	e.wantLedger(id, true, true, false)
	b1 := e.budget()
	if want := b0.UsedMsat + msat + fee; b1.UsedMsat != want {
		t.Errorf("payout used budget %d, want %d (before + amount + fee)", b1.UsedMsat, want)
	}
	if again := e.paidOnce(inv.PaymentHash, msat); again != fee {
		t.Errorf("fee changed after the stop: %d then %d", fee, again)
	}
}

// TestRefundPartials: two partial refunds take the whole order; a third is refused before and after they settle.
func TestRefundPartials(t *testing.T) {
	e := newRefundEnv(t, refundEnv, "")
	s := e.start()
	o := e.paidOrder("refund-partials", refundOrderMsat)
	parts := []int64{800_000, 1_200_000}
	type claimed struct {
		id  string
		inv *hubTx
	}
	var cs []claimed
	began := time.Now()
	for i, msat := range parts {
		inv := e.merchantInvoice(uint64(msat/1000), fmt.Sprintf("refund partial %d", i+1))
		id, ap := e.approve(o.ID, msat, time.Hour)
		e.mustClaim(id, ap, inv.Invoice, o.ID, msat)
		cs = append(cs, claimed{id, inv})
	}
	third := e.merchantInvoice(1, "refund partial 3")
	_, ap3 := e.approve(o.ID, 1_000, time.Hour)
	e.wantRefused("third partial while open", ap3, third.Invoice, 409, "exceeds_refundable")
	for i, c := range cs {
		_, d := e.waitRefund(c.id, refund.StateSettled, "", began, refundBound)
		t.Logf("MEASURED refund partial=%d claim_to_settled_seconds=%.1f", i+1, d.Seconds())
	}
	_, ap4 := e.approve(o.ID, 1_000, time.Hour)
	e.wantRefused("third partial after settle", ap4, third.Invoice, 409, "exceeds_refundable")
	e.wantFigures(o.ID, refundOrderMsat, refundOrderMsat, 0)
	for i, c := range cs {
		e.paidOnce(c.inv.PaymentHash, parts[i])
	}
	e.settleQuiet()
	stopAllow(s, e.earlier())
	for _, c := range cs {
		e.wantLedger(c.id, true, true, false)
	}
}

// TestRefundOwnInvoice: SatBooth's own invoice (the order's, issued by SatBooth's store) is refused at admission.
func TestRefundOwnInvoice(t *testing.T) {
	e := newRefundEnv(t, refundEnv, "")
	s := e.start()
	o := e.createOrder("refund-own-"+randHex(t)[:8], refundOrderMsat, 3600)
	own := e.pressPay(tokenOf(o))
	began := time.Now()
	e.customerPays(own)
	e.waitOrder(o.ID, "paid", began, paidBound)
	_, ap := e.approve(o.ID, 1_000_000, time.Hour)
	e.wantRefused("own invoice", ap, own, 400, "own_invoice")
	e.wantFigures(o.ID, refundOrderMsat, 0, 0)
	stopAllow(s, e.earlier())
}

// TestRefundHubKnown: an invoice the payout hub already paid (directly on the payout connection, before the service
// starts) is claimed; the sender's lookup finds it and the refund fails invoice_known without a payment or a ledger
// transfer.
func TestRefundHubKnown(t *testing.T) {
	e := newRefundEnv(t, refundEnv, "")
	const sats = 100
	inv := e.merchantInvoice(sats, "refund hub known")
	c := e.dialPayout()
	if r, err := c.PayInvoice(ctxFor(t, 2*time.Minute), inv.Invoice, 0); err != nil {
		t.Fatalf("direct payout: %v", shownErr(err))
	} else {
		t.Logf("MEASURED refund hub_known direct_payout fee_msat=%d", r.FeesPaidMsat)
	}
	_ = c.Close()
	b0 := e.budget()

	s := e.start()
	o := e.paidOrder("refund-known", refundOrderMsat)
	id, ap := e.approve(o.ID, sats*1000, time.Hour)
	began := time.Now()
	e.mustClaim(id, ap, inv.Invoice, o.ID, sats*1000)
	_, d := e.waitRefund(id, refund.StateFailed, "invoice_known", began, refundBound)
	t.Logf("MEASURED refund hub_known claim_to_failed_seconds=%.1f", d.Seconds())
	e.wantFigures(o.ID, refundOrderMsat, 0, 0)
	e.paidOnce(inv.PaymentHash, sats*1000) // the direct payment only
	stopAllow(s, e.earlier())

	e.wantLedger(id, false, false, false)
	if b1 := e.budget(); b1.UsedMsat != b0.UsedMsat {
		t.Errorf("payout used budget moved from %d to %d", b0.UsedMsat, b1.UsedMsat)
	}
}

// lockedBuf is a bytes.Buffer safe for one writer and concurrent readers.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// killWallet is the crash child's payout connection: its pay_invoice kills the process (SIGKILL) before anything is
// sent. The sender calls it right after the send marker is stored, so the kill falls between the marker and the request.
type killWallet struct{ *nwc.Client }

func (killWallet) PayInvoiceUntil(context.Context, string, int64, time.Time) (*nwc.PayResult, error) {
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {}
}

// TestRefundCrashChild is the crash phase's child process: SatBooth's service as cmd/satbooth starts it, with the
// killing payout connection. It runs only when the crash phase starts it.
func TestRefundCrashChild(t *testing.T) {
	path := os.Getenv(childEnv)
	if path == "" {
		t.Skip("run by TestRefundCrashAfterMarker only")
	}
	lg := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := service.ReadConfig(path)
	if err != nil {
		t.Fatalf("ReadConfig: %v", shownErr(err))
	}
	if err := service.Lock(cfg, lg, memlock.Protect); err != nil {
		t.Fatalf("Lock: %v", shownErr(err))
	}
	if err := cfg.CheckFiles(); err != nil {
		t.Fatalf("CheckFiles: %v", shownErr(err))
	}
	k, err := nostr.LoadSecretKeyFile(cfg.Payout.ClientKeyFile)
	if err != nil {
		t.Fatal("payout key")
	}
	c, err := nwc.Dial(ctxFor(t, time.Minute), nwc.Config{RelayURL: cfg.Payout.RelayURL, WalletPubKey: cfg.Payout.WalletPubKey, Key: k,
		Timeout: time.Duration(cfg.Payout.TimeoutSeconds) * time.Second})
	if err != nil {
		t.Fatalf("dial payout: %v", shownErr(err))
	}
	err = service.Run(context.Background(), cfg, service.WithLogger(lg), service.WithPayoutWallet(killWallet{c}))
	t.Fatalf("Run returned %v; the child must be killed in pay_invoice", shownErr(err))
}

// TestRefundCrashAfterMarker: SatBooth is killed (SIGKILL, a real process) between the send marker and the
// pay_invoice request. After the restart the refund reads sent, the hub has no record (NOT_FOUND, one ALERT), and no
// second pay_invoice is made: the restarted service's payout calls are counted, and both hubs have no row.
// TestRefundVoidUnknown voids it after the void wait (refund.VoidAfter, 11 minutes). The resolver keeps a NOT_FOUND
// silent for HubCallTimeout (15 s) after the marker; the poll's bound (refundBound, 2 min) covers that and more than
// two resolver rounds (30 s each), so the ALERT is seen whatever the gap between the kill and the restart.
func TestRefundCrashAfterMarker(t *testing.T) {
	e := newRefundEnv(t, refundEnv, "")
	var out lockedBuf // the child writes it from exec's copying goroutines while the phase reads it
	cmd := exec.Command(os.Args[0], "-test.run=^TestRefundCrashChild$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), childEnv+"="+e.cfgPath)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("child: %v", shownErr(err))
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	poll(t, "the child's service answers", time.Minute, 200*time.Millisecond, func() (bool, error) {
		select {
		case err := <-exited:
			t.Fatalf("the child ended while starting: %v\n%s", shownErr(err), out.String())
		default:
		}
		resp, err := e.hc.Get("http://" + publicAddr + "/static/style.css")
		if err != nil {
			return false, err
		}
		resp.Body.Close()
		return resp.StatusCode == 200, nil
	})

	o := e.paidOrder("refund-crash", refundOrderMsat)
	const msat = 1_000_000
	inv := e.merchantInvoice(msat/1000, "refund crash")
	id, ap := e.approve(o.ID, msat, time.Hour)
	began := time.Now()
	e.mustClaim(id, ap, inv.Invoice, o.ID, msat)
	select {
	case err := <-exited:
		var ee *exec.ExitError
		ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !errors.As(err, &ee) || !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Fatalf("the child ended with %v, want killed by SIGKILL\n%s", shownErr(err), out.String())
		}
		t.Logf("MEASURED refund crash child_killed_seconds_after_claim=%.2f signal=%v", time.Since(began).Seconds(), ws.Signal())
	case <-time.After(time.Minute):
		t.Fatalf("the child was not killed within a minute of the claim\n%s", out.String())
	}
	t.Logf("child output (%d lines): %s", strings.Count(out.String(), "\n"), firstLines(out.String(), 5))

	// The data as the kill left it: the marker and the ledger pending, no result; nothing at either hub.
	rs := e.storeState(id)
	if !rs.Sent || rs.Resolved {
		t.Fatalf("after the kill: sent %v resolved %v, want a send marker and no result", rs.Sent, rs.Resolved)
	}
	e.wantLedger(id, true, false, false)
	e.noRows(inv.PaymentHash)

	cw := &countingWallet{Client: e.dialPayout()}
	e.opts = []service.Option{service.WithPayoutWallet(cw)}
	restarted := time.Now()
	s := e.start()
	poll(t, "ALERT NOT_FOUND after the restart", refundBound, 250*time.Millisecond, func() (bool, error) {
		return alertFor(s, refund.AlertNotFound, id), errors.New("no ALERT yet")
	})
	t.Logf("MEASURED refund crash restart_to_not_found_alert_seconds=%.1f", time.Since(restarted).Seconds())
	if v := e.refund(id); v.State != refund.StateSent || v.ResolvedAt != nil {
		t.Errorf("after the restart the refund reads %+v, want sent", v)
	}
	e.wantFigures(o.ID, refundOrderMsat, 0, msat)
	time.Sleep(35 * time.Second) // one more resolver round: a negative check has to wait
	if n := cw.pays.Load(); n != 0 {
		t.Errorf("the restarted service made %d pay_invoice calls, want 0", n)
	}
	e.noRows(inv.PaymentHash)
	stopAllow(s, e.earlier(id))
	t.Logf("MEASURED refund crash pay_invoice_calls_after_restart=%d", cw.pays.Load())

	e.f.CrashID, e.f.CrashOrder, e.f.CrashHash = id, o.ID, inv.PaymentHash
	e.saveFile()
}

// cutWallet is the real payout connection, through the in-process proxy; its pay_invoice cuts the proxy first (every
// live connection closed, new ones refused), so the request goes out on a dead connection and never reaches the relay.
type cutWallet struct {
	*nwc.Client
	proxy *cutProxy
	mu    sync.Mutex
	errs  []string
	done  chan struct{}
}

func (w *cutWallet) PayInvoiceUntil(ctx context.Context, inv string, msat int64,
	until time.Time) (*nwc.PayResult, error) {
	w.proxy.cut()
	r, err := w.Client.PayInvoiceUntil(ctx, inv, msat, until)
	w.mu.Lock()
	w.errs = append(w.errs, errString(err))
	w.mu.Unlock()
	select {
	case w.done <- struct{}{}:
	default:
	}
	return r, err
}

// TestRefundCut: SatBooth's payout connection to the relay is cut right before the request (the script gives
// relay-satbooth the proxy's URL around this phase). Both of the service's connections, the invoice connection and the
// payout connection, reach relay-satbooth through the in-process proxy, so the cut closes both. The request never
// reaches the hub: after the proxy is restored
// the hub has no record (NOT_FOUND, one ALERT) and the refund stays sent until TestRefundVoidUnknown voids it.
func TestRefundCut(t *testing.T) {
	proxy := newCutProxy(t, hubIP[hubSatbooth]+":7448")
	e := newRefundEnv(t, refundEnv, cutProxyURL)
	cw := &cutWallet{Client: e.dialPayout(), proxy: proxy, done: make(chan struct{}, 1)}
	e.opts = []service.Option{service.WithPayoutWallet(cw)}
	s := e.start()
	o := e.paidOrder("refund-cut", refundOrderMsat)
	const msat = 1_000_000
	inv := e.merchantInvoice(msat/1000, "refund cut")
	id, ap := e.approve(o.ID, msat, time.Hour)
	began := time.Now()
	e.mustClaim(id, ap, inv.Invoice, o.ID, msat)
	select {
	case <-cw.done:
	case <-time.After(time.Minute):
		t.Fatal("pay_invoice did not return within a minute")
	}
	cw.mu.Lock()
	t.Logf("MEASURED refund cut pay_invoice_returned_seconds=%.1f error=%v", time.Since(began).Seconds(), cw.errs)
	cw.mu.Unlock()
	if v := e.refund(id); v.State != refund.StateSent {
		t.Errorf("after the cut the refund reads %s, want sent", v.State)
	}
	proxy.restore()
	restored := time.Now()
	poll(t, "ALERT NOT_FOUND after the restore", refundBound, 250*time.Millisecond, func() (bool, error) {
		return alertFor(s, refund.AlertNotFound, id), errors.New("no ALERT yet")
	})
	t.Logf("MEASURED refund cut restore_to_not_found_alert_seconds=%.1f", time.Since(restored).Seconds())
	// A void before the send marker is refund.VoidAfter (11 minutes) old is refused, whatever the hub says.
	voidPath := "/v1/refunds/" + id + "/void"
	status, raw, _ := e.call("POST", voidPath, e.rwKey, map[string]string{"approval": e.voidApproval(id)})
	if code := e.errCode("POST", voidPath, status, raw); status != 409 || code != "refund_in_flight" {
		t.Errorf("early void: status %d code %q, want 409 refund_in_flight", status, code)
	} else {
		t.Logf("MEASURED refund void early status=%d code=%s", status, code)
	}
	if v := e.refund(id); v.State != refund.StateSent {
		t.Errorf("after the restore the refund reads %s, want sent", v.State)
	}
	e.wantFigures(o.ID, refundOrderMsat, 0, msat)
	e.noRows(inv.PaymentHash)
	stopAllow(s, e.earlier(id))
	if len(cw.errs) != 1 {
		t.Errorf("%d pay_invoice calls, want 1", len(cw.errs))
	}
	e.wantLedger(id, true, false, false)
	e.noRows(inv.PaymentHash)

	e.f.CutID, e.f.CutOrder, e.f.CutHash = id, o.ID, inv.PaymentHash
	e.saveFile()
}

// TestRefundFailedBefore: a paid order and the claimant's invoice for all of it, made while the merchant's hub runs.
// The script then stops the merchant's hub.
func TestRefundFailedBefore(t *testing.T) {
	e := newRefundEnv(t, refundEnv, "")
	s := e.start()
	o := e.paidOrder("refund-failed", refundOrderMsat)
	inv := e.merchantInvoice(refundOrderMsat/1000, "refund failed")
	stopAllow(s, e.earlier())
	e.f.FailedOrder, e.f.FailedInvoice, e.f.FailedHash = o.ID, inv.Invoice, inv.PaymentHash
	e.saveFile()
}

// TestRefundFailed: the merchant's hub (the claimant's node, SatBooth's only channel peer) is stopped, so the payment
// cannot be routed; the hub reports the payout failed, the refund fails (failed) and frees its amount.
func TestRefundFailed(t *testing.T) {
	merchantDown = true
	e := newRefundEnv(t, refundEnv, "")
	if _, err := e.mer.info(); err == nil {
		t.Fatal("the merchant's hub answers; the script should have stopped it")
	}
	b0 := e.budget()
	s := e.start()
	id, ap := e.approve(e.f.FailedOrder, refundOrderMsat, time.Hour)
	began := time.Now()
	e.mustClaim(id, ap, e.f.FailedInvoice, e.f.FailedOrder, refundOrderMsat)
	var v refundJSON
	poll(t, "refund resolved while the claimant's node is down", 4*time.Minute, 250*time.Millisecond, func() (bool, error) {
		v = e.refund(id)
		if v.State == refund.StateFailed || v.State == refund.StateSettled {
			return true, nil
		}
		if alertFor(s, refund.AlertNotFound, id) || alertFor(s, refund.AlertExpired, id) || alertFor(s, refund.AlertPending, id) {
			t.Fatalf("observation: the hub did not report failed; the refund reads %s with an ALERT (see the log)\n%s", v.State, s.logs.String())
		}
		return false, fmt.Errorf("state %s", v.State)
	})
	if v.State != refund.StateFailed || v.Reason != "failed" {
		t.Fatalf("refund %s/%s, want failed/failed", v.State, v.Reason)
	}
	t.Logf("MEASURED refund failed claim_to_failed_seconds=%.1f", time.Since(began).Seconds())
	out := e.rows(e.sat, e.f.FailedHash)
	for _, r := range out {
		t.Logf("MEASURED refund failed satbooth_row %s/%s amount_msat=%d failure_reason_bytes=%d", shownType(r.Type), shownState(r.State), r.AmountMsat, len(r.Failure))
	}
	if len(out) != 1 || out[0].Type != "outgoing" || out[0].State != "failed" {
		t.Errorf("SatBooth's hub: rows %v, want one outgoing failed", rowStates(out))
	}
	e.wantFigures(e.f.FailedOrder, refundOrderMsat, 0, 0)
	e.settleQuiet()
	stopAllow(s, e.earlier())
	e.wantLedger(id, true, false, true)
	if b1 := e.budget(); b1.UsedMsat != b0.UsedMsat {
		t.Errorf("payout used budget %d after the failed payout, want %d (unchanged)", b1.UsedMsat, b0.UsedMsat)
	}
	e.f.FailedID = id
	e.saveFile()
}

// TestRefundFailedAfter: the merchant's hub is back; a new approval for the whole order is admitted (the failed refund
// freed its amount) and pays a fresh invoice.
func TestRefundFailedAfter(t *testing.T) {
	e := newRefundEnv(t, refundEnv, "")
	poll(t, "channel active on both hubs", 3*time.Minute, 2*time.Second, func() (bool, error) {
		for _, h := range []*hubAPI{e.sat, e.mer} {
			chs, err := h.channels()
			if err != nil {
				return false, err
			}
			if len(chs) == 0 || !chs[0].Active {
				_ = e.mer.connectPeer(e.st.Hubs[hubSatbooth].NodeID, hubIP[hubSatbooth], 9735) // harmless when connected
				return false, errors.New("channel not active")
			}
		}
		return true, nil
	})
	b0 := e.budget()
	s := e.start()
	inv := e.merchantInvoice(refundOrderMsat/1000, "refund after failed")
	id, ap := e.approve(e.f.FailedOrder, refundOrderMsat, time.Hour)
	began := time.Now()
	e.mustClaim(id, ap, inv.Invoice, e.f.FailedOrder, refundOrderMsat)
	_, d := e.waitRefund(id, refund.StateSettled, "", began, refundBound)
	t.Logf("MEASURED refund failed_then_new_approval claim_to_settled_seconds=%.1f", d.Seconds())
	e.wantFigures(e.f.FailedOrder, refundOrderMsat, refundOrderMsat, 0)
	fee := e.paidOnce(inv.PaymentHash, refundOrderMsat)
	if out := e.rows(e.sat, e.f.FailedHash); len(out) != 1 || out[0].State != "failed" {
		t.Errorf("the failed payout's rows changed: %v", rowStates(out))
	}
	if in := e.rows(e.mer, e.f.FailedHash); len(in) != 0 {
		t.Errorf("the merchant's hub has rows %v for the failed refund's invoice, want none", rowStates(in))
	}
	e.settleQuiet()
	stopAllow(s, e.earlier())
	e.wantLedger(id, true, true, false)
	if b1 := e.budget(); b1.UsedMsat != b0.UsedMsat+refundOrderMsat+fee {
		t.Errorf("payout used budget %d, want %d", b1.UsedMsat, b0.UsedMsat+refundOrderMsat+fee)
	}
}

// TestRefundBudget: a claim larger than what is left of the payout budget stays claimed with one ALERT and pays
// nothing; when its approval expires it fails expired_unsent. Beside it, a second such claim on a second order is
// voided while claimed (plan 11): 200, failed, cancelled, its order's amount free again, its ledger pending created
// and voided (the ledger moves first), and a second void is refused. Its own environment, so the ALERTs stay here.
func TestRefundBudget(t *testing.T) {
	e := newRefundEnv(t, refundBudgetEnv, "")
	b0 := e.budget()
	left := b0.TotalMsat - b0.UsedMsat
	msat := left/1000*1000 + 1000 // larger than the budget left
	t.Logf("MEASURED refund budget left_msat=%d claim_msat=%d", left, msat)
	s := e.start()
	// Both orders are paid before either approval is signed: the first approval's 150 s start at its signing.
	o := e.paidOrder("refund-budget", msat)
	o2 := e.paidOrder("refund-budget-void", msat)
	const ttl = 150 * time.Second
	id, ap := e.approve(o.ID, msat, ttl)
	id2, ap2 := e.approve(o2.ID, msat, time.Hour) // voided long before it could expire
	began := time.Now()
	inv := e.merchantInvoice(uint64(msat/1000), "refund budget")
	inv2 := e.merchantInvoice(uint64(msat/1000), "refund budget void")
	e.mustClaim(id, ap, inv.Invoice, o.ID, msat)
	e.mustClaim(id2, ap2, inv2.Invoice, o2.ID, msat)
	poll(t, "ALERT budget short for both refunds", refundBound, 250*time.Millisecond, func() (bool, error) {
		both := alertFor(s, refund.AlertBudgetShort, id) && alertFor(s, refund.AlertBudgetShort, id2)
		return both, errors.New("no ALERT yet")
	})
	t.Logf("MEASURED refund budget claim_to_alert_seconds=%.1f (both refunds)", time.Since(began).Seconds())
	for _, x := range []string{id, id2} {
		if v := e.refund(x); v.State != refund.StateClaimed {
			t.Errorf("refund %s reads %s, want claimed", x, v.State)
		}
	}
	e.wantFigures(o.ID, msat, 0, msat)
	e.wantFigures(o2.ID, msat, 0, msat)

	// The void of the second, still claimed: it ends the refund with no payment (neither hub has a row for its
	// invoice, checked below); the ledger moves first, so its pending is created and voided.
	voidPath := "/v1/refunds/" + id2 + "/void"
	voided := time.Now()
	status, raw, _ := e.call("POST", voidPath, e.rwKey, map[string]string{"approval": e.voidApproval(id2)})
	t.Logf("MEASURED refund void claimed status=%d seconds=%.2f", status, time.Since(voided).Seconds())
	if status != 200 {
		t.Fatalf("void of the claimed refund: status %d %s", status, raw)
	}
	if v := e.decodeRefund(raw); v.ID != id2 || v.State != refund.StateFailed || v.Reason != "cancelled" ||
		v.ResolvedAt == nil {
		t.Errorf("void of the claimed refund: view %+v, want failed/cancelled with resolved_at", v)
	} else {
		t.Logf("MEASURED refund void claimed state=%s reason=%s", v.State, v.Reason)
	}
	e.wantFigures(o2.ID, msat, 0, 0)
	status, raw, _ = e.call("POST", voidPath, e.rwKey, map[string]string{"approval": e.voidApproval(id2)})
	if code := e.errCode("POST", voidPath, status, raw); status != 409 || code != "invalid_state" {
		t.Errorf("second void of the cancelled refund: status %d code %q, want 409 invalid_state", status, code)
	} else {
		t.Logf("MEASURED refund void claimed second_void status=%d code=%s", status, code)
	}

	_, d := e.waitRefund(id, refund.StateFailed, "expired_unsent", began, ttl+time.Minute)
	t.Logf("MEASURED refund budget claim_to_expired_unsent_seconds=%.1f (approval valid %s)", d.Seconds(), ttl)
	e.wantFigures(o.ID, msat, 0, 0)
	if v := e.refund(id2); v.State != refund.StateFailed || v.Reason != "cancelled" {
		t.Errorf("the voided refund reads %s/%s at the end, want failed/cancelled", v.State, v.Reason)
	}
	e.noRows(inv.PaymentHash)
	e.noRows(inv2.PaymentHash)
	alerts := stopAllow(s, allowAlert{refund.AlertBudgetShort, []string{id}},
		allowAlert{refund.AlertBudgetShort, []string{id2}})
	n1, n2 := 0, 0
	for _, l := range alerts {
		if strings.Contains(l, `"refund_id":"`+id+`"`) {
			n1++
		}
		if strings.Contains(l, `"refund_id":"`+id2+`"`) {
			n2++
		}
	}
	if len(alerts) != 2 || n1 != 1 || n2 != 1 {
		t.Errorf("%d ALERT lines (%d for the first refund, %d for the second), want one budget-short line each",
			len(alerts), n1, n2)
	}
	e.wantLedger(id, false, false, false) // expired_unsent: no pending was ever made
	e.wantLedger(id2, true, false, true)  // cancelled by a void: the pending is created and voided, never posted
	for _, x := range []string{id, id2} {
		if rs := e.storeState(x); rs.Sent {
			t.Errorf("refund %s has a send marker", x)
		}
	}
	if b1 := e.budget(); b1.UsedMsat != b0.UsedMsat {
		t.Errorf("payout used budget moved from %d to %d", b0.UsedMsat, b1.UsedMsat)
	}
}

// TestRefundVoidUnknown: the two refunds the hub never received (the crash and the cut) are voided by a signed void
// approval once their send marker is refund.VoidAfter (11 minutes) old: a void before that is refused
// (refund_in_flight), after it each fails voided, the ledger pending is voided and the order's amount is refundable
// again.
func TestRefundVoidUnknown(t *testing.T) {
	e := newRefundEnv(t, refundEnv, "")
	type unknown struct{ name, id, order, hash string }
	us := []unknown{{"crash", e.f.CrashID, e.f.CrashOrder, e.f.CrashHash}, {"cut", e.f.CutID, e.f.CutOrder, e.f.CutHash}}
	var last time.Time
	for _, u := range us {
		if u.id == "" {
			t.Fatalf("no %s refund in the phase file (did its phase run?)", u.name)
		}
		rs := e.storeState(u.id)
		if !rs.Sent || rs.Resolved {
			t.Fatalf("%s refund: sent %v resolved %v, want sent", u.name, rs.Sent, rs.Resolved)
		}
		if rs.SentAt.After(last) {
			last = rs.SentAt
		}
		t.Logf("MEASURED refund void %s sent_age_seconds_at_phase_start=%.0f", u.name, time.Since(rs.SentAt).Seconds())
	}
	s := e.start()
	voidURL := func(id string) string { return "/v1/refunds/" + id + "/void" }
	if time.Until(last.Add(refund.VoidAfter)) > 5*time.Second {
		u := us[len(us)-1]
		status, raw, _ := e.call("POST", voidURL(u.id), e.rwKey, map[string]string{"approval": e.voidApproval(u.id)})
		code := ""
		if status >= 300 {
			code = e.errCode("POST", voidURL(u.id), status, raw)
		}
		if status != 409 || code != "refund_in_flight" {
			t.Errorf("early void: status %d code %q, want 409 refund_in_flight", status, code)
		}
		t.Logf("MEASURED refund void early status=%d code=%s", status, code)
	}
	wait := time.Until(last.Add(refund.VoidAfter + 2*time.Second))
	t.Logf("MEASURED refund void waited_seconds=%.0f for the 11 minute void wait", max(wait, 0).Seconds())
	if wait > 0 {
		time.Sleep(wait) // the void's 11 minutes are real time, by design
	}
	for _, u := range us {
		status, raw, _ := e.call("POST", voidURL(u.id), e.rwKey, map[string]string{"approval": e.voidApproval(u.id)})
		if status != 200 {
			t.Fatalf("%s void: status %d %s", u.name, status, raw)
		}
		v := e.decodeRefund(raw)
		if v.ID != u.id || v.State != refund.StateFailed || v.Reason != "voided" || v.ResolvedAt == nil {
			t.Errorf("%s void: view %+v, want failed/voided", u.name, v)
		}
		e.wantFigures(u.order, refundOrderMsat, 0, 0)
		e.noRows(u.hash)
		// A second void of a failed refund is refused.
		status, raw, _ = e.call("POST", voidURL(u.id), e.rwKey, map[string]string{"approval": e.voidApproval(u.id)})
		if code := e.errCode("POST", voidURL(u.id), status, raw); status != 409 || code != "invalid_state" {
			t.Errorf("%s second void: status %d code %q, want 409 invalid_state", u.name, status, code)
		}
	}
	stopAllow(s, e.earlier())
	for _, u := range us {
		e.wantLedger(u.id, true, false, true)
	}
}
