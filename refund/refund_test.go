package refund_test

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/refund"
	"github.com/satbooth/satbooth/refund/refundtest"
	"github.com/satbooth/satbooth/store"
)

const net = approval.Regtest

// payTimeout is the payout connection's timeout in the tests' configuration.
const payTimeout = 70 * time.Second

var epoch = time.Date(2027, 1, 10, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

// paidMap is the Paid source: a settable msat total per order.
type paidMap struct {
	mu sync.Mutex
	m  map[string]int64
}

func (p *paidMap) PaidMsat(_ context.Context, id string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.m[id], nil
}

type env struct {
	t    *testing.T
	dir  string
	logs *recorder
	// wallet, when set, is the wallet the service gets instead of w (a wrapper around it).
	wallet refund.Wallet
	clk    *clock
	st     *store.Store
	lg     *ledger.Ledger
	w      *refundtest.Wallet
	paid   *paidMap
	pub    ed25519.PublicKey
	priv   ed25519.PrivateKey
	svc    *refund.Service
	cfg    refund.Config
	n      int
}

func newEnv(t *testing.T, maxMsat, dailyMsat int64) *env {
	t.Helper()
	ctx := context.Background()
	e := &env{t: t, clk: &clock{t: epoch}, w: refundtest.New(), paid: &paidMap{m: map[string]int64{}}, logs: &recorder{}}
	// A budget with room for every test that does not test the budget.
	e.w.Budget(&nwc.Budget{Set: true, TotalMsat: 1 << 50}, nil)
	var err error
	if e.pub, e.priv, err = ed25519.GenerateKey(rand.New(rand.NewSource(1))); err != nil {
		t.Fatal(err)
	}
	keys, err := approval.NewKeys([]string{hex.EncodeToString(e.pub)})
	if err != nil {
		t.Fatal(err)
	}
	e.dir = t.TempDir()
	e.open()
	t.Cleanup(func() { e.st.Close(); e.lg.Close() })
	// The two ledger accounts, as lnpay creates them at startup.
	for _, a := range []ledger.Account{
		{ID: lnpay.LightningHub, Ledger: 1, Code: 1, Flags: ledger.AccountCreditsMustNotExceedDebits},
		{ID: lnpay.MerchantLightning, Ledger: 1, Code: 2, Flags: ledger.AccountDebitsMustNotExceedCredits},
	} {
		if r, err := e.lg.CreateAccount(ctx, a); err != nil || r != ledger.AccountCreated {
			t.Fatalf("account: %v %v", r, err)
		}
	}
	e.cfg = refund.Config{Network: net, Keys: keys, MaxMsat: maxMsat, DailyMsat: dailyMsat, PayTimeout: payTimeout}
	e.newService()
	return e
}

// open opens the store and the ledger files of the environment.
func (e *env) open() {
	e.t.Helper()
	ctx := context.Background()
	var err error
	if e.st, err = store.Open(ctx, filepath.Join(e.dir, "orders.db"), store.WithClock(e.clk.Now)); err != nil {
		e.t.Fatal(err)
	}
	if e.lg, err = ledger.Open(ctx, filepath.Join(e.dir, "ledger.db"), ledger.WithClock(e.clk.Now)); err != nil {
		e.t.Fatal(err)
	}
}

// newService builds a service on the open files (New runs recovery), with no lookup gap.
func (e *env) newService() {
	e.t.Helper()
	var w refund.Wallet = e.w
	if e.wallet != nil {
		w = e.wallet
	}
	var err error
	if e.svc, err = refund.New(context.Background(), e.cfg, e.st, e.lg, w, e.paid, e.clk.Now, slog.New(e.logs)); err != nil {
		e.t.Fatal(err)
	}
	refund.SetTiming(e.svc, refund.SendEvery, refund.ResolveEvery, 0)
}

// restart is a process restart: the store and the ledger are closed and opened again, and a new service is built
// on them. The wallet (the hub) carries on.
func (e *env) restart() {
	e.t.Helper()
	e.st.Close()
	e.lg.Close()
	e.open()
	e.newService()
}

// order creates an order whose Lightning paid total is paid msat.
func (e *env) order(paid int64) string {
	e.t.Helper()
	e.n++
	o, _, err := e.st.CreateOrder(context.Background(), store.Order{AccountID: "acct", MerchantRef: fmt.Sprintf("ref-%d", e.n),
		AmountMsat: 1_000_000_000, ExpiresAt: epoch.Add(24 * time.Hour)})
	if err != nil {
		e.t.Fatal(err)
	}
	e.paid.mu.Lock()
	e.paid.m[o.ID] = paid
	e.paid.mu.Unlock()
	if paid > 0 { // the payment in the ledger, as lnpay records it: what the merchant's account holds
		r, err := e.lg.CreateTransfer(context.Background(), ledger.Transfer{ID: ledger.U128From64(uint64(0x7000_0000 + e.n)),
			DebitAccountID: lnpay.LightningHub, CreditAccountID: lnpay.MerchantLightning, Amount: ledger.U128From64(uint64(paid)),
			Ledger: 1, Code: 1})
		if err != nil || r != ledger.TransferCreated {
			e.t.Fatalf("paid transfer: %v %v", r, err)
		}
	}
	return o.ID
}

// id16 turns n into a nonzero 16-byte id.
func id16(n int) (b [16]byte) {
	b[0], b[1], b[14], b[15] = 0xa5, byte(n>>8), byte(n), 1
	return b
}

func orderBytes(t *testing.T, orderID string) (b [16]byte) {
	t.Helper()
	raw, err := hex.DecodeString(orderID)
	if err != nil || len(raw) != 16 {
		t.Fatal("bad order id")
	}
	copy(b[:], raw)
	return b
}

// sign returns a refund approval for refund number n of the order.
func (e *env) sign(n int, orderID string, amount int64, expires time.Time) string {
	return approval.SignRefund(approval.Refund{Network: net, KeyID: approval.KeyID(e.pub), RefundID: id16(n),
		OrderID: orderBytes(e.t, orderID), AmountMsat: uint64(amount), ExpiresAt: expires.Unix()}, e.priv)
}

func (e *env) claim(n int, orderID string, amount int64) (refund.View, error) {
	e.t.Helper()
	ap := e.sign(n, orderID, amount, e.clk.Now().Add(time.Hour))
	return e.svc.Claim(context.Background(), ap, refundtest.InvoiceFor(net, n, amount))
}

// count is how many refunds have no result: in the tests that use it, which run no sender round and write no result
// before counting, every refund the store holds.
func (e *env) count() int {
	e.t.Helper()
	rs, err := e.st.OpenRefunds(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return len(rs)
}

func (e *env) fail(n int) {
	e.t.Helper()
	id := hex.EncodeToString(func() []byte { b := id16(n); return b[:] }())
	if err := e.st.AddRefundResult(context.Background(), store.RefundResult{RefundID: id, Outcome: store.RefundOutcomeFailed,
		Reason: store.RefundReasonInvoiceKnown, At: e.clk.Now()}); err != nil {
		e.t.Fatal(err)
	}
}

// recorder is a slog handler that keeps every record's message and attributes.
type recorder struct {
	mu   sync.Mutex
	recs []rec
}

type rec struct {
	msg   string
	attrs map[string]string
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *recorder) WithGroup(string) slog.Handler            { return r }
func (r *recorder) Handle(_ context.Context, x slog.Record) error {
	a := map[string]string{}
	x.Attrs(func(at slog.Attr) bool { a[at.Key] = at.Value.String(); return true })
	r.mu.Lock()
	r.recs = append(r.recs, rec{x.Message, a})
	r.mu.Unlock()
	return nil
}

// count is how many records have the message msg (for refund id, if it is not empty).
func (r *recorder) count(msg, id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, x := range r.recs {
		if x.msg == msg && (id == "" || x.attrs["refund_id"] == id) {
			n++
		}
	}
	return n
}

func TestClaimAccepts(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	order := e.order(10_000_000)
	v, err := e.claim(1, order, 4_000_000)
	if err != nil {
		t.Fatal(err)
	}
	want := refund.View{ID: hex.EncodeToString(func() []byte { b := id16(1); return b[:] }()), OrderID: order, State: "claimed",
		AmountMsat: 4_000_000, ClaimedAt: epoch}
	if v != want {
		t.Fatalf("view %+v, want %+v", v, want)
	}
	got, found, err := e.svc.Get(context.Background(), v.ID)
	if err != nil || !found || got != want {
		t.Fatalf("Get = %+v, %v, %v", got, found, err)
	}
	// Claim makes no hub call and no ledger write.
	if e.w.TotalCalls() != 0 {
		t.Fatalf("hub calls: %d", e.w.TotalCalls())
	}
	var id [16]byte = id16(1)
	for _, tid := range []ledger.U128{refund.RefundTransferID(id), refund.PostTransferID(id), refund.VoidTransferID(id)} {
		if _, found, err := e.lg.LookupTransfer(context.Background(), tid); err != nil || found {
			t.Fatalf("ledger transfer: found %v, err %v", found, err)
		}
	}
}

// TestClaimInvoiceAmount: an invoice whose amount equals the approval's, in any unit, is admitted; an amountless one
// is refused (the pinned hub cannot issue one, so paying one cannot be qualified), and so is a different amount
// (TestClaimRejects).
func TestClaimInvoiceAmount(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	order := e.order(10_000_000)
	claim := func(n int, msat int64, invoice string) (refund.View, error) {
		return e.svc.Claim(context.Background(), e.sign(n, order, msat, epoch.Add(time.Hour)), invoice)
	}
	if _, err := claim(59, 1_000_000, refundtest.Invoice(net, 59)); !errors.Is(err, refund.ErrInvoice) {
		t.Fatalf("amountless: %v", err)
	}
	for i, amt := range []string{"10u", "10000n", "10000000p"} {
		n := 60 + i
		v, err := claim(n, 1_000_000, refundtest.InvoiceWithAmount(net, n, amt))
		if err != nil || v.AmountMsat != 1_000_000 {
			t.Fatalf("amount %q: %+v, %v", amt, v, err)
		}
	}
	// 2m is 200,000,000 msat, not 2,000,000: refused; 20u is 2,000,000: admitted.
	if _, err := claim(70, 2_000_000, refundtest.InvoiceWithAmount(net, 70, "2m")); !errors.Is(err, refund.ErrInvoice) {
		t.Fatalf("2m for 2,000,000 msat: %v", err)
	}
	if _, err := claim(71, 2_000_000, refundtest.InvoiceWithAmount(net, 71, "20u")); err != nil {
		t.Fatalf("20u for 2,000,000 msat: %v", err)
	}
}

// flipLast changes the last hex character of s (the order id is random, so a fixed replacement could equal it).
func flipLast(s string) string {
	if s[len(s)-1] == '0' {
		return s[:len(s)-1] + "1"
	}
	return s[:len(s)-1] + "0"
}

func TestClaimRejects(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 5_000_000, 50_000_000)
	order := e.order(10_000_000)
	good := func(n int) string { return e.sign(n, order, 1_000_000, epoch.Add(time.Hour)) }
	if _, err := e.claim(1, order, 1_000_000); err != nil { // refund 1 exists; invoice 1 used
		t.Fatal(err)
	}
	// An invoice of ours, in the store.
	own := refundtest.Invoice(net, 900)
	if err := e.st.AddInvoice(ctx, store.Invoice{OrderID: order, PaymentHash: strings.Repeat("ab", 32), Bolt11: own, AmountMsat: 1_000_000_000,
		ExpiresAt: epoch.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	otherKeyPub, otherKey, _ := ed25519.GenerateKey(rand.New(rand.NewSource(2)))
	badKey := approval.SignRefund(approval.Refund{Network: net, KeyID: approval.KeyID(otherKeyPub), RefundID: id16(20),
		OrderID: orderBytes(t, order), AmountMsat: 1_000_000, ExpiresAt: epoch.Add(time.Hour).Unix()}, otherKey)
	wrongNet := approval.SignRefund(approval.Refund{Network: approval.Mainnet, KeyID: approval.KeyID(e.pub), RefundID: id16(21),
		OrderID: orderBytes(t, order), AmountMsat: 1_000_000, ExpiresAt: epoch.Add(time.Hour).Unix()}, e.priv)
	unknownOrder := e.sign(22, strings.Repeat("11", 16), 1_000_000, epoch.Add(time.Hour))
	tests := []struct {
		name          string
		approval, inv string
		want          error
	}{
		{"empty approval", "", refundtest.Invoice(net, 30), refund.ErrApproval},
		{"short approval", good(31)[:200], refundtest.Invoice(net, 31), refund.ErrApproval},
		{"uppercase approval", strings.ToUpper(good(32)), refundtest.Invoice(net, 32), refund.ErrApproval},
		{"flipped signature", flipLast(good(33)), refundtest.Invoice(net, 33), refund.ErrApproval},
		{"unknown key", badKey, refundtest.Invoice(net, 20), refund.ErrApproval},
		{"wrong network", wrongNet, refundtest.Invoice(net, 21), refund.ErrApproval},
		{"expired", e.sign(34, order, 1_000_000, epoch.Add(-time.Second)), refundtest.Invoice(net, 34), refund.ErrApprovalExpired},
		{"over 30 days", e.sign(35, order, 1_000_000, epoch.Add(31*24*time.Hour)), refundtest.Invoice(net, 35), refund.ErrApproval},
		{"amount over max", e.sign(36, order, 5_001_000, epoch.Add(time.Hour)), refundtest.Invoice(net, 36), refund.ErrApproval},
		{"amount not whole sats", e.sign(37, order, 1_500, epoch.Add(time.Hour)), refundtest.Invoice(net, 37), refund.ErrApproval},
		{"amount zero", e.sign(38, order, 0, epoch.Add(time.Hour)), refundtest.Invoice(net, 38), refund.ErrApproval},
		{"refund id used", good(1), refundtest.Invoice(net, 39), refund.ErrRefundExists},
		{"unknown order", unknownOrder, refundtest.Invoice(net, 22), refund.ErrUnknownOrder},
		{"exceeds refundable", e.sign(40, order, 5_000_000, epoch.Add(time.Hour)), refundtest.Invoice(net, 40), nil}, // below: needs the cap spent first
		{"invoice garbage", good(41), "not an invoice", refund.ErrInvoice},
		{"invoice empty", good(42), "", refund.ErrInvoice},
		{"invoice uppercase", good(43), strings.ToUpper(refundtest.Invoice(net, 43)), refund.ErrInvoice},
		{"invoice other network", good(44), refundtest.Invoice(approval.Mainnet, 44), refund.ErrInvoice},
		{"invoice bad checksum", good(45), func() string { s := refundtest.Invoice(net, 45); return s[:len(s)-1] + "q" }(), refund.ErrInvoice},
		{"invoice with prefix", good(46), "lightning:" + refundtest.Invoice(net, 46), refund.ErrInvoice},
		{"invoice already used", good(47), refundtest.InvoiceFor(net, 1, 1_000_000), refund.ErrInvoice},
		{"invoice amountless", good(56), refundtest.Invoice(net, 56), refund.ErrInvoice},
		{"own invoice", good(48), own, refund.ErrOwnInvoice},
		// The approval is for 1,000,000 msat (10u); an invoice amount must be exactly that.
		{"invoice amount lower", good(49), refundtest.InvoiceWithAmount(net, 49, "5u"), refund.ErrInvoice},
		{"invoice amount higher", good(50), refundtest.InvoiceWithAmount(net, 50, "10001n"), refund.ErrInvoice},
		{"invoice amount 1 msat off", good(51), refundtest.InvoiceWithAmount(net, 51, "10000010p"), refund.ErrInvoice},
		{"invoice amount sub-msat", good(52), refundtest.InvoiceWithAmount(net, 52, "10000001p"), refund.ErrInvoice},
		{"invoice amount leading zero", good(53), refundtest.InvoiceWithAmount(net, 53, "010u"), refund.ErrInvoice},
		{"invoice amount zero", good(54), refundtest.InvoiceWithAmount(net, 54, "0"), refund.ErrInvoice},
		{"invoice amount over 2^53", good(55), refundtest.InvoiceWithAmount(net, 55, "90072"), refund.ErrInvoice},
	}
	before := e.count()
	for _, tc := range tests {
		if tc.want == nil {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.svc.Claim(ctx, tc.approval, tc.inv)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if e.count() != before {
				t.Fatal("a refusal wrote a refund")
			}
		})
	}
	if e.w.TotalCalls() != 0 {
		t.Fatal("a refusal called the hub")
	}

	t.Run("exceeds refundable", func(t *testing.T) {
		o := e.order(3_000_000)
		n := e.count()
		if _, err := e.claim(50, o, 2_000_000); err != nil {
			t.Fatal(err)
		}
		if _, err := e.claim(51, o, 2_000_000); !errors.Is(err, refund.ErrExceedsRefundable) {
			t.Fatalf("got %v", err)
		}
		if e.count() != n+1 {
			t.Fatal("a refusal wrote a refund")
		}
		unpaid := e.order(0)
		if _, err := e.claim(52, unpaid, 1_000); !errors.Is(err, refund.ErrExceedsRefundable) {
			t.Fatalf("unpaid order: got %v", err)
		}
	})

	t.Run("daily cap", func(t *testing.T) {
		d := newEnv(t, 3_000, 5_000)
		o := d.order(1_000_000)
		for i := 1; i <= 2; i++ {
			if _, err := d.claim(i, o, 2_000); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := d.claim(3, o, 2_000); !errors.Is(err, refund.ErrDailyCap) {
			t.Fatalf("got %v", err)
		}
		if d.count() != 2 {
			t.Fatal("a refusal wrote a refund")
		}
	})

	t.Run("too many open", func(t *testing.T) {
		d := newEnv(t, 1_000, 1_000_000)
		o := d.order(1_000_000)
		for i := 1; i <= refund.MaxOpen; i++ {
			if _, err := d.claim(i, o, 1_000); err != nil {
				t.Fatalf("claim %d: %v", i, err)
			}
		}
		if _, err := d.claim(refund.MaxOpen+1, o, 1_000); !errors.Is(err, refund.ErrTooManyOpen) {
			t.Fatalf("got %v", err)
		}
		if d.count() != refund.MaxOpen {
			t.Fatal("a refusal wrote a refund")
		}
		d.fail(1) // a failed refund is not open: room again
		if _, err := d.claim(refund.MaxOpen+1, o, 1_000); err != nil {
			t.Fatalf("after a failure: %v", err)
		}
	})
}

func TestClaimPartials(t *testing.T) {
	e := newEnv(t, 5_000, 1_000_000)
	order := e.order(6_000)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if _, err := e.claim(i, order, 2_000); err != nil {
			t.Fatalf("partial %d: %v", i, err)
		}
	}
	if _, err := e.claim(4, order, 1_000); !errors.Is(err, refund.ErrExceedsRefundable) {
		t.Fatalf("fourth partial: %v", err)
	}
	// A failed refund frees its amount; a settled one does not.
	e.fail(2)
	if _, err := e.claim(4, order, 3_000); !errors.Is(err, refund.ErrExceedsRefundable) {
		t.Fatalf("3000 over 2000 freed: %v", err)
	}
	if _, err := e.claim(5, order, 2_000); err != nil {
		t.Fatalf("freed amount: %v", err)
	}
	id1 := hex.EncodeToString(func() []byte { b := id16(1); return b[:] }())
	if err := e.st.AddRefundSend(ctx, id1, epoch); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddRefundResult(ctx, store.RefundResult{RefundID: id1, Outcome: store.RefundOutcomeSettled, Reason: store.RefundReasonSettled, At: epoch}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.claim(6, order, 1_000); !errors.Is(err, refund.ErrExceedsRefundable) {
		t.Fatalf("settled refund must keep counting: %v", err)
	}
}

// TestClaimConcurrentPartialsCapped: 20 concurrent claims for twice the paid msat.
func TestClaimConcurrentPartialsCapped(t *testing.T) {
	const paid, each, workers = 10_000_000, 1_000_000, 20
	e := newEnv(t, each, 100_000_000)
	order := e.order(paid)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var accepted int64
	start := make(chan struct{})
	for i := 1; i <= workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := e.claim(i, order, each)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted += each
			case !errors.Is(err, refund.ErrExceedsRefundable):
				t.Errorf("claim %d: %v", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if accepted > paid {
		t.Fatalf("accepted %d msat of %d paid", accepted, paid)
	}
	settled, open, err := e.st.RefundFigures(context.Background(), order)
	if err != nil {
		t.Fatal(err)
	}
	if settled != 0 || open != accepted {
		t.Fatalf("store: settled %d, open %d; accepted %d", settled, open, accepted)
	}
	if accepted != paid {
		t.Fatalf("accepted %d, want exactly %d (the cap is reachable)", accepted, paid)
	}
}

func TestDailyCapUTC(t *testing.T) {
	e := newEnv(t, 2_000, 3_000)
	order := e.order(1_000_000)
	// A clock that reads in a zone far from UTC: the day is still the UTC day.
	zone := time.FixedZone("far", 13*3600)
	e.clk.Set(time.Date(2027, 3, 1, 23, 59, 59, 0, time.UTC).In(zone))
	if _, err := e.claim(1, order, 2_000); err != nil {
		t.Fatal(err)
	}
	if _, err := e.claim(2, order, 2_000); !errors.Is(err, refund.ErrDailyCap) {
		t.Fatalf("same UTC day: %v", err)
	}
	e.clk.Set(time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC).In(zone))
	if _, err := e.claim(2, order, 2_000); err != nil {
		t.Fatalf("next UTC day: %v", err)
	}
	if _, err := e.claim(3, order, 2_000); !errors.Is(err, refund.ErrDailyCap) {
		t.Fatalf("second claim of the new day: %v", err)
	}
	// A failed refund frees the day's amount.
	e.fail(2)
	if _, err := e.claim(3, order, 2_000); err != nil {
		t.Fatalf("after a failure: %v", err)
	}
}

func TestOwnInvoiceRefused(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 5_000, 50_000)
	order := e.order(10_000)
	inv := refundtest.InvoiceFor(net, 7, 1_000) // what e.claim(7, ..., 1_000) sends
	// The hub reported our invoice in capitals; the claimant sends the lowercase form (or the reverse).
	if err := e.st.AddInvoice(ctx, store.Invoice{OrderID: order, PaymentHash: strings.Repeat("cd", 32), Bolt11: strings.ToUpper(inv),
		AmountMsat: 1_000_000_000, ExpiresAt: epoch.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.claim(7, order, 1_000); !errors.Is(err, refund.ErrOwnInvoice) {
		t.Fatalf("got %v", err)
	}
	// Our own invoice whose amount differs from the approval's (the usual case: an order's invoice is for the whole
	// order) is still named as ours.
	own := refundtest.InvoiceWithAmount(net, 8, "10m") // 1,000,000,000 msat, the order's amount
	if err := e.st.AddInvoice(ctx, store.Invoice{OrderID: order, PaymentHash: strings.Repeat("ce", 32), Bolt11: own,
		AmountMsat: 1_000_000_000, ExpiresAt: epoch.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Claim(ctx, e.sign(8, order, 1_000, epoch.Add(time.Hour)), own)
	if !errors.Is(err, refund.ErrOwnInvoice) {
		t.Fatalf("own invoice with another amount: got %v", err)
	}
	if e.count() != 0 {
		t.Fatal("a refusal wrote a refund")
	}
}

func TestGet(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 5_000, 50_000)
	order := e.order(10_000)
	hexID := func(n int) string { b := id16(n); return hex.EncodeToString(b[:]) }
	for i := 1; i <= 4; i++ {
		if _, err := e.claim(i, order, 1_000); err != nil {
			t.Fatal(err)
		}
	}
	e.clk.Set(epoch.Add(time.Minute))
	at := e.clk.Now()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.st.AddRefundSend(ctx, hexID(2), at))
	must(e.st.AddRefundSend(ctx, hexID(3), at))
	must(e.st.AddRefundResult(ctx, store.RefundResult{RefundID: hexID(3), Outcome: "settled", Reason: "settled", At: at}))
	must(e.st.AddRefundSend(ctx, hexID(4), at))
	must(e.st.AddRefundResult(ctx, store.RefundResult{RefundID: hexID(4), Outcome: "failed", Reason: "not_sent", At: at}))
	cases := []struct {
		n             int
		state, reason string
		resolved      bool
	}{{1, "claimed", "", false}, {2, "sent", "", false}, {3, "settled", "", true}, {4, "failed", "not_sent", true}}
	for _, c := range cases {
		v, found, err := e.svc.Get(ctx, hexID(c.n))
		if err != nil || !found {
			t.Fatalf("refund %d: %v %v", c.n, found, err)
		}
		if v.State != c.state || v.Reason != c.reason || v.ResolvedAt.IsZero() == c.resolved || v.OrderID != order || v.AmountMsat != 1_000 || !v.ClaimedAt.Equal(epoch) {
			t.Fatalf("refund %d: %+v", c.n, v)
		}
		if c.resolved && !v.ResolvedAt.Equal(at) {
			t.Fatalf("refund %d resolved at %v", c.n, v.ResolvedAt)
		}
	}
	for _, id := range []string{hexID(99), "", "xyz", strings.ToUpper(hexID(1)), hexID(1) + "00"} {
		if _, found, err := e.svc.Get(ctx, id); found || err != nil {
			t.Fatalf("Get(%q) = %v, %v", id, found, err)
		}
	}
}

func TestNewRefusesBadConfig(t *testing.T) {
	e := newEnv(t, 1_000, 2_000)
	mk := func(f func(*refund.Config)) error {
		c := e.cfg
		f(&c)
		_, err := refund.New(context.Background(), c, e.st, e.lg, e.w, e.paid, nil, nil)
		return err
	}
	for name, f := range map[string]func(*refund.Config){
		"no keys": func(c *refund.Config) { c.Keys = nil }, "zero max": func(c *refund.Config) { c.MaxMsat = 0 },
		"max over daily": func(c *refund.Config) { c.MaxMsat = 3_000 }, "daily over 2^53": func(c *refund.Config) { c.DailyMsat = 1<<53 + 1 },
		"negative alert": func(c *refund.Config) { c.BudgetAlertMsat = -1 }, "bad network": func(c *refund.Config) { c.Network = 9 },
		"no pay timeout":    func(c *refund.Config) { c.PayTimeout = 0 },
		"pay timeout short": func(c *refund.Config) { c.PayTimeout = nwc.MinTimeout - time.Second },
		"pay timeout long":  func(c *refund.Config) { c.PayTimeout = nwc.MaxTimeout + time.Second },
	} {
		if mk(f) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := mk(func(c *refund.Config) { c.MaxMsat, c.DailyMsat = 1<<53, 1<<53 }); err != nil {
		t.Errorf("limit config refused: %v", err)
	}
	for _, d := range []time.Duration{nwc.MinTimeout, nwc.MaxTimeout} {
		if err := mk(func(c *refund.Config) { c.PayTimeout = d }); err != nil {
			t.Errorf("pay timeout %v refused: %v", d, err)
		}
	}
	if _, err := refund.New(context.Background(), e.cfg, nil, e.lg, e.w, e.paid, nil, nil); err == nil {
		t.Error("nil store accepted")
	}
}

func TestTransferIDs(t *testing.T) {
	var id [16]byte
	for i := range id {
		id[i] = byte(i + 1)
	}
	// SHA-256(domain || 01 02 .. 10), first 16 bytes, big-endian (computed with Python's hashlib).
	cases := []struct {
		name   string
		got    ledger.U128
		hi, lo uint64
	}{
		{"refund", refund.RefundTransferID(id), 0xb7914d1d88e6d168, 0x6504b6dcc18d3a4a},
		{"post", refund.PostTransferID(id), 0x52208dd4ed64a986, 0xf212a22a707f4a20},
		{"void", refund.VoidTransferID(id), 0x58e94962cc814e5e, 0x6527d499accf8ecd},
	}
	for _, c := range cases {
		if c.got.Hi != c.hi || c.got.Lo != c.lo {
			t.Errorf("%s: got %016x%016x, want %016x%016x", c.name, c.got.Hi, c.got.Lo, c.hi, c.lo)
		}
	}
	if cases[0].got == cases[1].got || cases[1].got == cases[2].got || cases[0].got == cases[2].got {
		t.Error("the three domains give equal ids")
	}
	if refund.RefundTransferID([16]byte{}) == refund.RefundTransferID(id) {
		t.Error("ids ignore the refund id")
	}
	// No refund id collides with a payment id of the same bytes.
	if lnpay.PaidTransferID(strings.Repeat("00", 32)) == refund.RefundTransferID([16]byte{}) {
		t.Error("collides with a payment")
	}
}
