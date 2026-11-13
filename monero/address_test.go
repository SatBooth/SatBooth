package monero

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/monero/monerotest"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/xmrrpc"
)

var (
	bg    = context.Background()
	epoch = time.Unix(1_800_000_000, 0).UTC()

	_ RPC = (*monerotest.Wallet)(nil)
	_ RPC = (*xmrrpc.Client)(nil)
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

// stubLN answers every order as paid, expired or new, and records MarkKnown.
type stubLN struct {
	mu      sync.Mutex
	paid    bool
	expired bool
	marked  []string
	onMark  func(acct string)
	// onStatus, when set, runs after Status has read its answer and before it returns it, outside the lock, with the
	// call's number (1 for the first call after it was set).
	onStatus func(n int)
	calls    int
}

func (l *stubLN) Status(context.Context, string) (lnpay.Status, error) {
	l.mu.Lock()
	st := lnpay.Status{State: lnpay.StateNew}
	switch {
	case l.paid:
		st.State = lnpay.StatePaid
	case l.expired:
		st.State = lnpay.StateExpired
	}
	l.calls++
	n, f := l.calls, l.onStatus
	l.mu.Unlock()
	if f != nil {
		f(n)
	}
	return st, nil
}

// MarkKnown records the call; onMark, when set, runs first.
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

func (l *stubLN) set(paid bool) {
	l.mu.Lock()
	l.paid = paid
	l.mu.Unlock()
}

func (l *stubLN) setExpired(expired bool) {
	l.mu.Lock()
	l.expired = expired
	l.mu.Unlock()
}

// stubPeer is the on-chain rail as a peer: its view of every order.
type stubPeer struct {
	view rail.View
	ok   bool
	err  error
}

func (p stubPeer) View(context.Context, store.Order) (rail.View, bool, error) {
	return p.view, p.ok, p.err
}

func (p stubPeer) RevertedSeqs(context.Context, store.Order) ([]int, error) { return nil, p.err }

// override wraps the wallet with the answers the fake does not give: another version, another probe code, a
// different create_address or get_address answer, or a gate inside create_address.
type override struct {
	RPC
	mu       sync.Mutex
	version  *uint32
	probe    *int
	create   func(xmrrpc.Created) xmrrpc.Created
	onCreate func()
	getAddr  func(account uint32, indices []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error)
}

func (o *override) set(f func(*override)) {
	o.mu.Lock()
	f(o)
	o.mu.Unlock()
}

func (o *override) GetVersion(ctx context.Context) (uint32, error) {
	v, err := o.RPC.GetVersion(ctx)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.version != nil && err == nil {
		return *o.version, nil
	}
	return v, err
}

func (o *override) SignTransferProbe(ctx context.Context) (int, error) {
	c, err := o.RPC.SignTransferProbe(ctx)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.probe != nil && err == nil {
		return *o.probe, nil
	}
	return c, err
}

func (o *override) CreateAddress(ctx context.Context, account uint32, label string) (xmrrpc.Created, error) {
	o.mu.Lock()
	gate, f := o.onCreate, o.create
	o.mu.Unlock()
	c, err := o.RPC.CreateAddress(ctx, account, label)
	if gate != nil {
		gate()
	}
	if f != nil && err == nil {
		c = f(c)
	}
	return c, err
}

func (o *override) GetAddress(ctx context.Context, account uint32, indices []uint32) (xmrrpc.Addresses, error) {
	a, err := o.RPC.GetAddress(ctx, account, indices)
	o.mu.Lock()
	f := o.getAddr
	o.mu.Unlock()
	if f != nil {
		return f(account, indices, a, err)
	}
	return a, err
}

type env struct {
	t   *testing.T
	clk *clock
	st  *store.Store
	lg  *ledger.Ledger
	w   *monerotest.Wallet
	rpc *override
	ln  *stubLN
	log *syncBuf
	svc *Service
	dir string
	mod func(*Config) // changes the configuration of the services made after it is set
	// lnReal, when set, is the Lightning the services are built over instead of the stub; wire, when set, runs on each
	// new service (a test wires the shared settle lock there).
	lnReal rail.Lightning
	wire   func(*Service)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, clk: &clock{t: epoch}, ln: &stubLN{}, log: &syncBuf{}}
	dir := t.TempDir()
	e.dir = dir
	var err error
	if e.st, err = store.Open(bg, filepath.Join(dir, "orders.db"), store.WithClock(e.clk.Now)); err != nil {
		t.Fatal(err)
	}
	if e.lg, err = ledger.Open(bg, filepath.Join(dir, "ledger.db"), ledger.WithClock(e.clk.Now)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.st.Close(); e.lg.Close() })
	e.setWallet(monerotest.New(7))
	e.svc = e.newService()
	return e
}

// setWallet puts w behind the override, for the services made after it.
func (e *env) setWallet(w *monerotest.Wallet) {
	e.w = w
	e.rpc = &override{RPC: w}
}

func (e *env) config() Config {
	c := Config{Address: e.w.Primary(), Network: "regtest"}
	if e.mod != nil {
		e.mod(&c)
	}
	return c
}

func (e *env) newService() *Service {
	e.t.Helper()
	var ln rail.Lightning = e.ln
	if e.lnReal != nil {
		ln = e.lnReal
	}
	s, err := New(e.config(), e.st, e.lg, e.rpc, ln, e.clk.Now, slog.New(slog.NewTextHandler(e.log, nil)))
	if err != nil {
		e.t.Fatal(err)
	}
	if e.wire != nil {
		e.wire(s)
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

var orderSeq atomic.Int64

func (e *env) orderWith(o store.Order) store.Order {
	e.t.Helper()
	o.AccountID, o.MerchantRef = "acct-1", fmt.Sprintf("ref-%d", orderSeq.Add(1))
	if o.ExpiresAt.IsZero() {
		o.ExpiresAt = e.clk.Now().Add(time.Hour)
	}
	got, _, err := e.st.CreateOrder(bg, o)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func (e *env) order(pico int64) store.Order { return e.orderWith(store.Order{AmountPiconero: pico}) }

func (e *env) logHas(s string) bool { return strings.Contains(e.log.String(), s) }

// payment stores a payment to a subaddress, first seen now at height 1.
func (e *env) payment(txid, addr string, pico, unlock int64) {
	e.t.Helper()
	if _, err := e.st.RecordMoneroScan(bg, []store.Payment{{TxID: txid, Address: addr, AmountPiconero: pico, UnlockTime: unlock}},
		store.MoneroCursor{Height: 1, At: e.clk.Now()}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) transfer(id, debit, credit ledger.U128, o store.Order, pico int64, code uint16) {
	e.t.Helper()
	ud, _ := orderUserData(o.ID)
	r, err := e.lg.CreateTransfer(bg, ledger.Transfer{ID: id, DebitAccountID: debit, CreditAccountID: credit,
		Amount: ledger.U128From64(uint64(pico)), UserData128: ud, Ledger: ledgerID, Code: code})
	if err != nil || r != ledger.TransferCreated {
		e.t.Fatal(r, err)
	}
}

func (e *env) forward(o store.Order, txid string, index, epoch uint32, pico int64, code uint16) {
	e.transfer(PaidTransferID(txid, index, epoch), MoneroWallet, MerchantMonero, o, pico, code)
}

func (e *env) reverse(o store.Order, txid string, index, epoch uint32, pico int64) {
	e.transfer(ReversalTransferID(txid, index, epoch), MerchantMonero, MoneroWallet, o, pico, codeReversed)
}

// waitLanded waits for the order's flight to end.
func (e *env) waitLanded(orderID string) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.svc.inFlight(orderID) {
		if time.Now().After(deadline) {
			e.t.Fatal("the flight did not end")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAddressFiftyConcurrentCallsShareOneAllocation(t *testing.T) {
	e := newEnv(t)
	e.ready()
	o := e.order(50_000)
	var wg sync.WaitGroup
	res := make([]store.Subaddress, 50)
	errs := make([]error, 50)
	for i := range res {
		wg.Add(1)
		go func() { defer wg.Done(); res[i], errs[i] = e.svc.Address(bg, o.ID) }()
	}
	wg.Wait()
	for i := range res {
		if errs[i] != nil || res[i] != res[0] {
			t.Fatalf("call %d: %+v, %v", i, res[i], errs[i])
		}
	}
	if n := e.w.Calls("create_address"); n != 1 {
		t.Fatalf("create_address called %d times", n)
	}
	if m, ok, _ := e.st.MaxSubaddressIndex(bg); !ok || m != 1 || res[0].Index != 1 || res[0].Address != e.w.SubaddressAt(1) {
		t.Fatalf("max index %d %v; %+v", m, ok, res[0])
	}
	again, err := e.svc.Address(bg, o.ID)
	if err != nil || again != res[0] || e.w.Calls("create_address") != 1 {
		t.Fatalf("second round: %+v %v", again, err)
	}
}

func TestAddressTwentyOrdersRiseStrictly(t *testing.T) {
	e := newEnv(t)
	e.ready()
	orders := make([]store.Order, 20)
	for i := range orders {
		orders[i] = e.order(1000)
	}
	var wg sync.WaitGroup
	got := make([]store.Subaddress, 20)
	errs := make([]error, 20)
	for i := range orders {
		wg.Add(1)
		go func() { defer wg.Done(); got[i], errs[i] = e.svc.Address(bg, orders[i].ID) }()
	}
	wg.Wait()
	var idx []int64
	seen := map[string]bool{}
	for i := range got {
		if errs[i] != nil {
			t.Fatalf("order %d: %v", i, errs[i])
		}
		idx = append(idx, got[i].Index)
		if seen[got[i].Address] {
			t.Fatal("duplicate subaddress")
		}
		seen[got[i].Address] = true
	}
	sort.Slice(idx, func(a, b int) bool { return idx[a] < idx[b] })
	for i, v := range idx {
		if v != int64(i+1) {
			t.Fatalf("indexes %v", idx)
		}
	}
	if n := e.w.Calls("create_address"); n != 20 {
		t.Fatalf("create_address called %d times", n)
	}
	if n := e.w.Calls("store"); n != 20 {
		t.Fatalf("store called %d times", n)
	}
	if e.logHas("ALERT") {
		t.Fatalf("ALERT: %s", e.log.String())
	}
}

func TestAddressSecondOrderIsIndexPlusOne(t *testing.T) {
	e := newEnv(t)
	e.ready()
	a, err := e.svc.Address(bg, e.order(1000).ID)
	if err != nil || a.Index != 1 {
		t.Fatal(a, err)
	}
	b, err := e.svc.Address(bg, e.order(1000).ID)
	if err != nil || b.Index != 2 || b.Address == a.Address {
		t.Fatal(b, err)
	}
	if !xmrrpc.ValidAddress(a.Address, "regtest", true) || !store.ValidSubaddress(b.Address) {
		t.Fatalf("subaddress %q", a.Address)
	}
}

func TestAddressRefusalsBeforeStepOne(t *testing.T) {
	e := newEnv(t)
	e.ready()
	if _, err := e.svc.Address(bg, strings.Repeat("a", 32)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown order: %v", err)
	}
	if _, err := e.svc.Address(bg, e.orderWith(store.Order{AmountMsat: 1_000_000}).ID); !errors.Is(err, ErrNoMonero) {
		t.Fatalf("Lightning-only order: %v", err)
	}
	if _, err := e.svc.Address(bg, e.orderWith(store.Order{AmountSat: 1000}).ID); !errors.Is(err, ErrNoMonero) {
		t.Fatalf("on-chain-only order: %v", err)
	}
	o := e.order(1000)
	e.clk.Advance(time.Hour)
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired order: %v", err)
	}
	e.clk.Advance(-time.Hour)
	e.ln.set(true)
	both := e.orderWith(store.Order{AmountMsat: 1_000_000, AmountPiconero: 1000})
	if _, err := e.svc.Address(bg, both.ID); !errors.Is(err, ErrPaid) {
		t.Fatalf("paid on Lightning: %v", err)
	}
	// Lightning is asked only when the order has a Lightning amount.
	if _, err := e.svc.Address(bg, e.order(1000).ID); err != nil {
		t.Fatalf("a Monero-only order read Lightning: %v", err)
	}
	if n := e.w.Calls("create_address"); n != 1 {
		t.Fatalf("create_address called %d times", n)
	}
}

func TestAddressPaidOnThePeerIsRefused(t *testing.T) {
	e := newEnv(t)
	e.ready()
	o := e.orderWith(store.Order{AmountSat: 1000, AmountPiconero: 1000})
	e.svc.SetPeer(stubPeer{view: rail.View{Paid: true}, ok: true}, nil)
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrPaid) {
		t.Fatalf("paid on the peer: %v", err)
	}
	boom := errors.New("peer down")
	e.svc.SetPeer(stubPeer{err: boom}, nil)
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, boom) {
		t.Fatalf("the peer's error: %v", err)
	}
	e.svc.SetPeer(stubPeer{view: rail.View{Received: 1000}, ok: true}, nil) // seen but not paid
	if _, err := e.svc.Address(bg, o.ID); err != nil {
		t.Fatal(err)
	}
	if e.w.Calls("create_address") != 1 {
		t.Fatal("refused orders called wallet-rpc")
	}
	e.svc.SetPeer(nil, nil)
	if _, err := e.svc.Address(bg, e.order(1).ID); err != nil {
		t.Fatal(err)
	}
}

func TestAddressPaidMoneroIsRefused(t *testing.T) {
	e := newEnv(t)
	e.ready()
	o := e.order(1000)
	a, err := e.svc.Address(bg, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	txid := strings.Repeat("ab", 32)
	e.payment(txid, a.Address, 1000, 0)
	e.forward(o, txid, uint32(a.Index), 0, 1000, codePaid)
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrPaid) {
		t.Fatalf("paid in Monero: %v", err)
	}
	e.reverse(o, txid, uint32(a.Index), 0, 1000)
	if got, err := e.svc.Address(bg, o.ID); err != nil || got != a {
		t.Fatalf("a reversed payment still refuses: %+v %v", got, err)
	}
}

func TestAddressCheckFailuresStoreNothing(t *testing.T) {
	other := monerotest.New(99)
	for _, c := range []struct {
		name string
		set  func(o *override)
	}{
		{"a primary address", func(o *override) {
			o.create = func(c xmrrpc.Created) xmrrpc.Created { c.Address = "4" + c.Address[1:]; return c }
		}},
		{"a testnet subaddress", func(o *override) {
			o.create = func(c xmrrpc.Created) xmrrpc.Created { c.Address = "B" + c.Address[1:]; return c }
		}},
		{"94 bytes", func(o *override) {
			o.create = func(c xmrrpc.Created) xmrrpc.Created { c.Address = c.Address[:94]; return c }
		}},
		{"a character outside base58", func(o *override) {
			o.create = func(c xmrrpc.Created) xmrrpc.Created { c.Address = c.Address[:94] + "0"; return c }
		}},
		{"an index above the store's range", func(o *override) {
			o.create = func(c xmrrpc.Created) xmrrpc.Created { c.Index = 1 << 31; return c }
		}},
		{"get_address answers another subaddress", func(o *override) {
			o.getAddr = func(_ uint32, ix []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
				if len(ix) == 1 && ix[0] > 0 && err == nil {
					a.Addresses[0].Address = other.SubaddressAt(1)
				}
				return a, err
			}
		}},
		{"get_address answers another index", func(o *override) {
			o.getAddr = func(_ uint32, ix []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
				if len(ix) == 1 && ix[0] > 0 && err == nil {
					a.Addresses[0].Index++
				}
				return a, err
			}
		}},
		{"get_address answers two", func(o *override) {
			o.getAddr = func(_ uint32, ix []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
				if len(ix) == 1 && ix[0] > 0 && err == nil {
					a.Addresses = append(a.Addresses, a.Addresses[0])
				}
				return a, err
			}
		}},
		{"get_address answers an RPC error", func(o *override) {
			o.getAddr = func(_ uint32, ix []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
				if len(ix) == 1 && ix[0] > 0 { // the record's index, not step 2's 0
					return xmrrpc.Addresses{}, &xmrrpc.Error{Code: -15, Method: "get_address"}
				}
				return a, err
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.ready()
			o := e.order(1000)
			e.rpc.set(c.set)
			a, err := e.svc.Address(bg, o.ID)
			if !errors.Is(err, ErrSubaddress) || errors.Is(err, ErrIndexBehind) {
				t.Fatalf("Address = %+v, %v", a, err)
			}
			if _, err := e.st.SubaddressForOrder(bg, o.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("stored: %v", err)
			}
			if !e.logHas("ALERT: a new subaddress failed its checks; not shown") {
				t.Fatalf("no ALERT: %s", e.log.String())
			}
			if strings.Contains(e.log.String(), e.w.SubaddressAt(1)[:20]) || strings.Contains(e.log.String(), other.SubaddressAt(1)[:20]) {
				t.Fatal("a subaddress was logged")
			}
			if e.w.Calls("store") != 0 || e.svc.behind.Load() {
				t.Fatal("stored the wallet, or set behind")
			}
			e.rpc.set(func(o *override) { o.create, o.getAddr = nil, nil })
			got, err := e.svc.Address(bg, o.ID)
			if err != nil || got.OrderID != o.ID || got.Index != 2 {
				t.Fatalf("next call: %+v, %v (the failed index is burned, the next is 2)", got, err)
			}
		})
	}
}

func TestAddressIndexZeroIsRefusedAndSetsBehind(t *testing.T) {
	e := newEnv(t)
	e.ready()
	o := e.order(1000)
	e.rpc.set(func(o *override) {
		o.create = func(c xmrrpc.Created) xmrrpc.Created { return xmrrpc.Created{Address: c.Address, Index: 0} }
	})
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrSubaddress) || !errors.Is(err, ErrIndexBehind) {
		t.Fatalf("Address = %v", err)
	}
	if !e.svc.behind.Load() || !e.logHas("step=2") {
		t.Fatalf("behind %v; log %s", e.svc.behind.Load(), e.log.String())
	}
	calls := e.w.Calls("create_address")
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrIndexBehind) || e.w.Calls("create_address") != calls {
		t.Fatalf("a behind service called wallet-rpc: %v", err)
	}
	if _, err := e.st.SubaddressForOrder(bg, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("stored")
	}
	e.rpc.set(func(o *override) { o.create = nil })
	e.ready()
	if e.svc.behind.Load() {
		t.Fatal("Check did not clear behind")
	}
	if a, err := e.svc.Address(bg, o.ID); err != nil || a.Index != 2 {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestAddressIndexNotAboveMaximumSetsBehind(t *testing.T) {
	e := newEnv(t)
	e.ready()
	for i := 0; i < 2; i++ {
		if _, err := e.svc.Address(bg, e.order(1000).ID); err != nil {
			t.Fatal(err)
		}
	}
	e.w.Forget(2) // the next create_address answers index 1, below the record's 2
	o := e.order(1000)
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrIndexBehind) || !errors.Is(err, ErrSubaddress) {
		t.Fatalf("Address = %v", err)
	}
	if !e.svc.behind.Load() || !e.logHas("ALERT: a new subaddress failed its checks; not shown") || !e.logHas("step=2") {
		t.Fatalf("log: %s", e.log.String())
	}
	if _, err := e.st.SubaddressForOrder(bg, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("stored")
	}
	// Check re-creates index 2 (the refused call re-created index 1) and clears behind.
	e.ready()
	if e.svc.behind.Load() {
		t.Fatal("Check did not clear behind")
	}
	if a, err := e.svc.Address(bg, o.ID); err != nil || a.Index != 3 {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestAddressCrashAtEachHookPoint(t *testing.T) {
	for _, point := range []string{"address:after-create", "address:after-checks", "address:after-store", "address:after-record"} {
		t.Run(point, func(t *testing.T) {
			e := newEnv(t)
			e.ready()
			o := e.order(1000)
			boom := errors.New("crash")
			var once sync.Once
			e.svc.hook = func(p string) error {
				if p != point {
					return nil
				}
				var err error
				once.Do(func() { err = boom })
				return err
			}
			shown, err := e.svc.Address(bg, o.ID)
			if !errors.Is(err, boom) || shown.Address != "" {
				t.Fatalf("crashed call returned %+v, %v", shown, err)
			}
			row, rowErr := e.st.SubaddressForOrder(bg, o.ID)
			again, err := e.svc.Address(bg, o.ID)
			if err != nil {
				t.Fatal(err)
			}
			if point == "address:after-record" {
				if rowErr != nil || again != row || again.Index != 1 || e.w.Calls("create_address") != 1 {
					t.Fatalf("the stored subaddress was not returned on retry: %+v %v", again, rowErr)
				}
				return
			}
			if !errors.Is(rowErr, store.ErrNotFound) || again.Index != 2 {
				t.Fatalf("row %v; retry %+v", rowErr, again)
			}
		})
	}
}

func TestAddressHangGivesBusyThenSucceeds(t *testing.T) {
	e := newEnv(t)
	e.ready()
	e.svc.pressWait = 50 * time.Millisecond
	e.svc.callTimeout = 400 * time.Millisecond
	o := e.order(1000)
	e.w.Hang("create_address")
	start := time.Now()
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("Address = %v, want ErrBusy", err)
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatalf("ErrBusy after %v", time.Since(start))
	}
	bound := 2 * 5 * e.svc.callTimeout
	for e.svc.inFlight(o.ID) {
		if time.Since(start) > bound {
			t.Fatalf("the flight did not end within %v", bound)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if took := time.Since(start); took < e.svc.callTimeout/2 {
		t.Fatalf("the flight ended after %v, before the call timeout", took)
	}
	e.w.Unhang("create_address")
	e.svc.pressWait = 10 * time.Second
	if a, err := e.svc.Address(bg, o.ID); err != nil || a.Index != 1 {
		t.Fatalf("later call: %+v %v", a, err)
	}
}

func TestAddressFlightSurvivesTheCallersContext(t *testing.T) {
	e := newEnv(t)
	e.ready()
	e.svc.pressWait = time.Minute
	o := e.order(1000)
	gate := make(chan struct{})
	e.rpc.set(func(x *override) { x.onCreate = func() { <-gate } })
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { _, err := e.svc.Address(ctx, o.ID); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for e.w.Calls("create_address") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no call")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, ErrBusy) || !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled caller: %v", err)
	}
	close(gate)
	e.waitLanded(o.ID)
	if a, err := e.svc.Address(bg, o.ID); err != nil || a.Index != 1 || e.w.Calls("create_address") != 1 {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestAddressTransportErrorIsUnavailable(t *testing.T) {
	for _, m := range []string{"create_address", "get_address", "store"} {
		t.Run(m, func(t *testing.T) {
			e := newEnv(t)
			e.ready()
			o := e.order(1000)
			e.w.Fail(m, fmt.Errorf("%w: down", xmrrpc.ErrTransport))
			if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("%s: %v", m, err)
			}
			if e.logHas("ALERT") {
				t.Fatal("an unavailable wallet-rpc is a failed check")
			}
			if _, err := e.svc.Address(bg, o.ID); err != nil {
				t.Fatalf("%s: retry: %v", m, err)
			}
		})
	}
}

// A store failure is ErrUnavailable whatever its kind; nothing is recorded, and the next press records the next index.
func TestAddressStoreFailureRecordsNothing(t *testing.T) {
	e := newEnv(t)
	e.ready()
	o := e.order(1000)
	e.w.Fail("store", &xmrrpc.Error{Code: -1, Method: "store"})
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrSubaddress) {
		t.Fatalf("Address = %v", err)
	}
	if _, err := e.st.SubaddressForOrder(bg, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("recorded after a failed store")
	}
	if _, ok, _ := e.st.MaxSubaddressIndex(bg); ok {
		t.Fatal("a row exists")
	}
	if a, err := e.svc.Address(bg, o.ID); err != nil || a.Index != 2 || e.w.Calls("store") != 2 {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestAddressBeforeCheckIsRefused(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Address(bg, e.order(1000).ID); !errors.Is(err, ErrUnavailable) || err.Error() != errNotChecked.Error() {
		t.Fatalf("%v", err)
	}
	if e.w.Calls("create_address") != 0 {
		t.Fatal("called wallet-rpc")
	}
	// A Check that fails withdraws a pass: a wallet found full after a restart issues nothing.
	e.ready()
	e.w.Full(true)
	if err := e.svc.Check(bg); !errors.Is(err, ErrNotViewOnly) {
		t.Fatal(err)
	}
	if _, err := e.svc.Address(bg, e.order(1000).ID); err == nil || err.Error() != errNotChecked.Error() {
		t.Fatalf("%v", err)
	}
}

func TestAddressRestartKeepsIndexes(t *testing.T) {
	e := newEnv(t)
	e.ready()
	o := e.order(1000)
	a, err := e.svc.Address(bg, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = e.newService() // a restart: the store and the wallet remain
	e.ready()
	if b, err := e.svc.Address(bg, o.ID); err != nil || b != a {
		t.Fatalf("%+v %v", b, err)
	}
	if c, err := e.svc.Address(bg, e.order(1000).ID); err != nil || c.Index != 2 {
		t.Fatalf("%+v %v", c, err)
	}
}

// An order that expires, or is paid, while its flight waits for the allocation lock gets no subaddress and no
// create_address call.
func TestAddressFlightRechecksExpiryAndPaymentUnderTheLock(t *testing.T) {
	for _, how := range []string{"expired", "paid on Lightning", "paid on the peer"} {
		t.Run(how, func(t *testing.T) {
			e := newEnv(t)
			e.ready()
			e.svc.pressWait = 20 * time.Millisecond
			o := e.orderWith(store.Order{AmountMsat: 1_000_000, AmountSat: 1000, AmountPiconero: 1000})
			e.svc.alloc <- struct{}{} // another allocation holds the lock
			if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrBusy) {
				t.Fatalf("Address = %v", err)
			}
			switch how {
			case "expired":
				e.clk.Advance(2 * time.Hour)
			case "paid on Lightning":
				e.ln.set(true)
			default:
				e.svc.SetPeer(stubPeer{view: rail.View{Paid: true}, ok: true}, nil)
			}
			<-e.svc.alloc
			e.waitLanded(o.ID)
			if _, err := e.st.SubaddressForOrder(bg, o.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("a row was stored: %v", err)
			}
			if e.w.Calls("create_address") != 0 {
				t.Fatal("create_address was called")
			}
		})
	}
}

// Another writer stores a higher index between the maximum's read and the record: the store's refusal sets behind.
func TestAddressStoreIndexRefusalSetsBehind(t *testing.T) {
	e := newEnv(t)
	e.ready()
	other := e.order(1000)
	o := e.order(1000)
	e.svc.hook = func(p string) error {
		if p != "address:after-store" {
			return nil
		}
		return e.st.AddSubaddress(bg, store.Subaddress{OrderID: other.ID, Address: e.w.SubaddressAt(50), Index: 50})
	}
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrIndexBehind) || !errors.Is(err, ErrSubaddress) {
		t.Fatalf("Address = %v", err)
	}
	if !e.svc.behind.Load() || !e.logHas("step=5") {
		t.Fatal("behind not set")
	}
	if _, err := e.st.SubaddressForOrder(bg, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stored: %v", err)
	}
}

func TestWaitFlights(t *testing.T) {
	e := newEnv(t)
	e.ready()
	if err := e.svc.WaitFlights(bg); err != nil {
		t.Fatalf("no flights: %v", err)
	}
	e.svc.pressWait = 10 * time.Millisecond
	o := e.order(1000)
	e.svc.alloc <- struct{}{}
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(bg, 20*time.Millisecond)
	defer cancel()
	if err := e.svc.WaitFlights(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitFlights = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- e.svc.WaitFlights(bg) }()
	<-e.svc.alloc
	if err := <-done; err != nil {
		t.Fatalf("WaitFlights = %v", err)
	}
	if e.svc.inFlight(o.ID) {
		t.Fatal("still in flight")
	}
	ended, stop := context.WithCancel(bg)
	stop()
	if err := e.svc.WaitFlights(ended); err != nil {
		t.Fatalf("no flights, ended ctx: %v", err)
	}
	if a, err := e.svc.Address(bg, o.ID); err != nil || a.Index != 1 {
		t.Fatalf("%+v %v", a, err)
	}
}

// Ruling 8-A3: the wallet is replaced between the press's identity read and create_address.
// create_address and the step-3 get_address are then answered by the other wallet, consistently with each other; only
// its base address (get_address's address) gives it away. The press stores nothing, gets errWalletChanged (?busy=1),
// and the rail pauses as a pass's identity read does: the base cleared, watchable false, logPaused with reason address.
func TestAddressRefusesAWalletReplacedAfterTheIdentityRead(t *testing.T) {
	e := newWatchEnv(t, nil)
	first := e.order(xmr)
	e.addr(first)
	kept, err := e.st.SubaddressForOrder(bg, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	wrong := monerotest.New(8)
	for range 5 {
		wrong.CreateAddress(bg, 0, "")
	}
	swapped := false
	e.rpc.set(func(ov *override) {
		ov.getAddr = func(_ uint32, idx []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
			if !swapped && len(idx) == 1 && idx[0] == 0 {
				swapped = true
				e.svc.rpc = &override{RPC: wrong} // wallet-rpc now serves the other wallet
			}
			return a, err
		}
	})
	o := e.order(xmr)
	got, err := e.svc.Address(bg, o.ID)
	e.svc.rpc = e.rpc
	if !swapped {
		t.Fatal("setup: the identity read was not made")
	}
	if !errors.Is(err, errWalletChanged) || !errors.Is(err, ErrUnavailable) {
		t.Errorf("Address = %+v, %v; want errWalletChanged (the other wallet's index %d: %v)", got.Index, err, got.Index,
			got.Address != "" && got.Address == wrong.SubaddressAt(uint32(got.Index)))
	}
	if row, err := e.st.SubaddressForOrder(bg, o.ID); err == nil {
		t.Errorf("stored %s... at index %d (kept max %d), the other wallet's subaddress: %v",
			row.Address[:8], row.Index, kept.Index, row.Address == wrong.SubaddressAt(uint32(row.Index)))
	} else if !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	if e.svc.baseAddress() != "" || e.svc.watchable.Load() || !e.logHas(`msg="`+logPaused+`" reason=address`) {
		t.Errorf("not paused: base %q, watchable %v; log:\n%s", e.svc.baseAddress(), e.svc.watchable.Load(), e.log)
	}
	if max, _, _ := e.st.MaxSubaddressIndex(bg); max != kept.Index {
		t.Errorf("max index %d, want %d", max, kept.Index)
	}
}
