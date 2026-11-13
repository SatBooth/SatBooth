package onchain

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satbooth/satbooth/descriptor"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

// tpub is BIP-32 test vector 1's master public key with the testnet version bytes.
const tpub = "tpubD6NzVbkrYhZ4XgiXtGrdW5XDAPFCL9h7we1vwNCpn8tGbBcgfVYjXyhWo4E1xkh56hjod1RhGjxbaTLV3X4FyWuejifB9jusQ46QzG87VKp"

var (
	bg    = context.Background()
	epoch = time.Unix(1_800_000_000, 0).UTC()
)

type clock struct {
	mu    sync.Mutex
	t     time.Time
	onNow func() // test seam: runs on every Now, after the time is read (nil: none)
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	t, f := c.t, c.onNow
	c.mu.Unlock()
	if f != nil {
		f()
	}
	return t
}

// setOnNow sets the hook of Now.
func (c *clock) setOnNow(f func()) {
	c.mu.Lock()
	c.onNow = f
	c.mu.Unlock()
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
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

// stubLN answers every order as paid, or expired, or new.
type stubLN struct {
	mu      sync.Mutex
	paid    bool
	expired bool
	marked  []string
	onMark  func(acct string)
	fail    error // when set, Status answers it
	// onStatus, when set, runs after Status has read its answer and before it returns it, outside the lock, with the
	// call's number (1 for the first call after it was set), so a test can change the world between a caller's read
	// and its next step.
	onStatus func(n int)
	calls    int
}

func (l *stubLN) Status(_ context.Context, _ string) (lnpay.Status, error) {
	l.mu.Lock()
	st, err := l.answer()
	l.calls++
	n, f := l.calls, l.onStatus
	l.mu.Unlock()
	if f != nil {
		f(n)
	}
	return st, err
}

func (l *stubLN) answer() (lnpay.Status, error) {
	switch {
	case l.fail != nil:
		return lnpay.Status{}, l.fail
	case l.paid:
		return lnpay.Status{State: lnpay.StatePaid}, nil
	case l.expired:
		return lnpay.Status{State: lnpay.StateExpired}, nil
	}
	return lnpay.Status{State: lnpay.StateNew}, nil
}

// setOnStatus sets the callback and restarts the call count.
func (l *stubLN) setOnStatus(f func(n int)) {
	l.mu.Lock()
	l.onStatus, l.calls = f, 0
	l.mu.Unlock()
}

// MarkKnown records the call; onMark, when set, runs first (a test checks the fact is already written).
func (l *stubLN) MarkKnown(acct string) {
	l.mu.Lock()
	f := l.onMark
	l.marked = append(l.marked, acct)
	l.mu.Unlock()
	if f != nil {
		f(acct)
	}
}

func (l *stubLN) markedAccounts() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.marked...)
}

func (l *stubLN) setFail(err error) {
	l.mu.Lock()
	l.fail = err
	l.mu.Unlock()
}

func (l *stubLN) set(paid, expired bool) {
	l.mu.Lock()
	l.paid, l.expired = paid, expired
	l.mu.Unlock()
}

type env struct {
	t    *testing.T
	clk  *clock
	st   *store.Store
	lg   *ledger.Ledger
	node *onchaintest.Node
	ln   *stubLN
	log  *syncBuf
	desc descriptor.Descriptor
	norm string
	svc  *Service
	dir  string
	mod  func(*Config) // changes the configuration of the services made after it is set
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, clk: &clock{t: epoch}, ln: &stubLN{}, log: &syncBuf{}, dir: t.TempDir()}
	var err error
	if e.desc, err = descriptor.Parse("wpkh(" + tpub + "/0/*)"); err != nil {
		t.Fatal(err)
	}
	e.norm = onchaintest.Normalise(e.desc.Canonical)
	if e.st, err = store.Open(bg, filepath.Join(e.dir, "orders.db"), store.WithClock(e.clk.Now)); err != nil {
		t.Fatal(err)
	}
	if e.lg, err = ledger.Open(bg, filepath.Join(e.dir, "ledger.db"), ledger.WithClock(e.clk.Now)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.st.Close(); e.lg.Close() })
	e.node = onchaintest.New(e.norm, 7)
	e.svc = e.newService()
	return e
}

func (e *env) config() Config {
	c := Config{Descriptor: e.desc, Network: "regtest"}
	if e.mod != nil {
		e.mod(&c)
	}
	return c
}

func (e *env) newService() *Service {
	e.t.Helper()
	log := slog.New(slog.NewTextHandler(e.log, nil))
	s, err := New(e.config(), e.st, e.lg, e.node, e.ln, e.clk.Now, log)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// ready runs Check and fails the test if it does not pass.
func (e *env) ready() {
	e.t.Helper()
	if err := e.svc.Check(bg); err != nil {
		e.t.Fatalf("Check: %v", err)
	}
}

var orderSeq atomic.Int64 // tests may run in parallel

func (e *env) order(sat int64) store.Order {
	e.t.Helper()
	o, _, err := e.st.CreateOrder(bg, store.Order{AccountID: "acct-1", MerchantRef: fmt.Sprintf("ref-%d", orderSeq.Add(1)), AmountSat: sat,
		ExpiresAt: e.clk.Now().Add(time.Hour)})
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

func (e *env) logHas(s string) bool { return strings.Contains(e.log.String(), s) }

func contextWithCancel() (context.Context, context.CancelFunc) { return context.WithCancel(bg) }

type storeTXO = store.TXO

func (e *env) cursor() store.Cursor {
	return store.Cursor{BlockHash: strings.Repeat("cd", 32), Height: 1, At: e.clk.Now()}
}

func (e *env) transfer(id, debit, credit ledger.U128, o store.Order, sat int64, code uint16, rev uint64) {
	e.t.Helper()
	ud, _ := orderUserData(o.ID)
	r, err := e.lg.CreateTransfer(bg, ledger.Transfer{ID: id, DebitAccountID: debit, CreditAccountID: credit, Amount: ledger.U128From64(uint64(sat)),
		UserData128: ud, UserData64: rev, Ledger: ledgerID, Code: code})
	if err != nil || r != ledger.TransferCreated {
		e.t.Fatal(r, err)
	}
}

func (e *env) forward(o store.Order, txid string, vout, epoch uint32, sat int64) {
	e.transfer(PaidTransferID(txid, vout, epoch), OnchainWallet, MerchantOnchain, o, sat, codePaid, 0)
}

func (e *env) reverse(o store.Order, txid string, vout, epoch uint32, sat int64) {
	e.transfer(ReversalTransferID(txid, vout, epoch), MerchantOnchain, OnchainWallet, o, sat, codeReversed, 0)
}
