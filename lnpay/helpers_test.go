package lnpay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/refund/refundtest"
	"github.com/satbooth/satbooth/store"
)

var epoch = time.Unix(1_800_000_000, 0).UTC()

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// eventually polls cond until it holds or 10 s pass. It waits on a condition, never on a fixed delay.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeWallet implements Wallet for the unit-level cases.
type fakeWallet struct {
	mu        sync.Mutex
	seq       int
	invs      map[string]*nwc.Transaction
	descs     []string
	lookups   map[string]int
	makeCalls atomic.Int64
	lookupErr error
	// lookupBlock, when set, makes LookupInvoice announce itself on lookupEntered and wait for its
	// context to end.
	lookupBlock   bool
	lookupEntered chan struct{}
	// before runs at the start of MakeInvoice (it may block). lose = create the invoice at the hub
	// but return a lost-response error.
	before func(ctx context.Context, p nwc.MakeInvoiceParams) (lose bool, err error)
	notifs chan nwc.Notification
}

func newFakeWallet() *fakeWallet {
	return &fakeWallet{invs: map[string]*nwc.Transaction{}, lookups: map[string]int{}, notifs: make(chan nwc.Notification, 64)}
}

func (w *fakeWallet) Notifications() <-chan nwc.Notification { return w.notifs }

func (w *fakeWallet) MakeInvoice(ctx context.Context, p nwc.MakeInvoiceParams) (*nwc.Transaction, error) {
	w.makeCalls.Add(1)
	var lose bool
	if w.before != nil {
		var err error
		if lose, err = w.before(ctx, p); err != nil {
			return nil, err
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	sum := sha256.Sum256([]byte(fmt.Sprintf("invoice %d", w.seq)))
	h := hex.EncodeToString(sum[:])
	tx := &nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StatePending, Invoice: refundtest.InvoiceFor(approval.Regtest, w.seq, p.AmountMsat),
		Description: p.Description, PaymentHash: h, AmountMsat: p.AmountMsat}
	w.invs[h] = tx
	w.descs = append(w.descs, p.Description)
	if lose {
		return nil, fmt.Errorf("%w: lost", nwc.ErrNoResponse)
	}
	c := *tx
	return &c, nil
}

func (w *fakeWallet) LookupInvoice(ctx context.Context, h string) (*nwc.Transaction, error) {
	w.mu.Lock()
	if w.lookupBlock {
		w.lookups[h]++
		ch := w.lookupEntered
		w.mu.Unlock()
		ch <- struct{}{}
		<-ctx.Done()
		// A non-transport answer on cancel: only the reconciler's ctx guard keeps it from closing.
		return nil, &nwc.WalletError{Code: "NOT_FOUND", Message: "x"}
	}
	defer w.mu.Unlock()
	w.lookups[h]++
	if w.lookupErr != nil {
		return nil, w.lookupErr
	}
	tx, ok := w.invs[h]
	if !ok {
		return nil, &nwc.WalletError{Code: "NOT_FOUND", Message: "not found"}
	}
	c := *tx
	return &c, nil
}

func (w *fakeWallet) settle(h string, amount int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.invs[h].State, w.invs[h].AmountMsat = nwc.StateSettled, amount
}

func (w *fakeWallet) lookupCount(h string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lookups[h]
}

func (w *fakeWallet) notify(h string) {
	w.notifs <- nwc.Notification{Type: nwc.NotifPaymentReceived, Tx: nwc.Transaction{PaymentHash: h}}
}

type env struct {
	t      *testing.T
	dir    string
	clk    *clock
	st     *store.Store
	lg     *ledger.Ledger
	w      *fakeWallet
	limCfg limits.Config
	cfg    Config
	log    *syncBuf
	svc    *Service
	refs   int
}

func newEnv(t *testing.T, opts ...func(*env)) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), clk: &clock{t: epoch}, w: newFakeWallet(), log: &syncBuf{}, cfg: Config{Network: approval.Regtest},
		limCfg: limits.Config{RatePerSec: 1e6, Burst: 1 << 20, MaxMsat: 1_000_000_000, DailyMsat: 1 << 50}}
	for _, o := range opts {
		o(e)
	}
	e.open()
	t.Cleanup(e.closeFiles)
	return e
}

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
	lim, err := limits.New(e.limCfg, e.clk.Now)
	if err != nil {
		e.t.Fatal(err)
	}
	cfg := e.cfg
	if cfg.PollRate == 0 {
		cfg.PollRate = 1000
	}
	e.svc, err = New(cfg, e.st, e.lg, e.w, lim, e.clk.Now, slog.New(slog.NewTextHandler(e.log, nil)))
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) closeFiles() {
	if e.st != nil {
		e.st.Close()
		e.lg.Close()
		e.st, e.lg = nil, nil
	}
}

// restart kills the service (no goodbye: the files are simply closed) and starts a new one on the
// same files.
func (e *env) restart() {
	e.closeFiles()
	e.open()
}

func (e *env) order(account string, msat int64) store.Order {
	e.t.Helper()
	e.refs++
	o, _, err := e.st.CreateOrder(context.Background(), store.Order{AccountID: account,
		MerchantRef: fmt.Sprintf("ref-%d", e.refs), AmountMsat: msat, ExpiresAt: e.clk.Now().Add(time.Hour)})
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

func (e *env) pay(o store.Order) (store.Invoice, error) {
	return e.svc.Pay(context.Background(), o.Token)
}

func (e *env) mustPay(o store.Order) store.Invoice {
	e.t.Helper()
	inv, err := e.pay(o)
	if err != nil {
		e.t.Fatalf("Pay: %v", err)
	}
	return inv
}

func (e *env) pass() {
	e.t.Helper()
	if err := e.svc.pass(context.Background()); err != nil {
		e.t.Fatalf("pass: %v", err)
	}
}

func (e *env) transfers(o store.Order) int {
	e.t.Helper()
	invs, err := e.st.InvoicesForOrder(context.Background(), o.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	n := 0
	for _, i := range invs {
		_, found, err := e.lg.LookupTransfer(context.Background(), PaidTransferID(i.PaymentHash))
		if err != nil {
			e.t.Fatal(err)
		}
		if found {
			n++
		}
	}
	return n
}

func (e *env) eventCount(typ, orderID string) int {
	e.t.Helper()
	evs, err := e.st.UndeliveredEvents(context.Background(), store.MainEndpoint, epoch.Add(-time.Hour), time.Time{})
	if err != nil {
		e.t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Type == typ && ev.OrderID == orderID {
			n++
		}
	}
	return n
}

func (e *env) merchantCredits() uint64 {
	e.t.Helper()
	b, err := e.lg.Balance(context.Background(), MerchantLightning)
	if err != nil {
		e.t.Fatal(err)
	}
	return b.CreditsPosted.Lo
}

func (e *env) status(o store.Order) Status {
	e.t.Helper()
	s, err := e.svc.Status(context.Background(), o.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) open_(o store.Order) []store.Invoice {
	invs, err := e.st.InvoicesForOrder(context.Background(), o.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return invs
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// countsAcct returns a copy of the live per-account counts (byAcct): each account's open invoices
// plus reserved slots.
func (s *Service) countsAcct() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.byAcct)
}

// recountAcct returns the per-account counts derived from the store and the ledger.
func (s *Service) recountAcct(ctx context.Context) (map[string]int, error) {
	d, err := s.derive(ctx)
	if err != nil {
		return nil, err
	}
	return d.byAcct, nil
}

// sameAcct fails unless the live per-account counts equal a recount, and returns them.
func sameAcct(t *testing.T, e *env, when string) map[string]int {
	t.Helper()
	live := e.svc.countsAcct()
	re, err := e.svc.recountAcct(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(live, re) {
		t.Fatalf("%s: byAcct %v, recount %v", when, live, re)
	}
	return live
}
