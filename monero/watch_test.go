package monero

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/monero/monerotest"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/xmrrpc"
)

// --- the watcher's test harness ---

// xmr is one monero in piconero: the default tiers' first bound.
const xmr = 1_000_000_000_000

// zeroTiers has a 0 tier: an order of up to 0.01 XMR is paid from the pool.
var zeroTiers = []rail.Tier{{UpTo: xmr / 100, Confirmations: 0}, {UpTo: xmr, Confirmations: 1}, {UpTo: 10 * xmr, Confirmations: 3},
	{UpTo: 0, Confirmations: 10}}

// newWatchEnv is a checked env whose passes do not wait on the pacing (PollEvery at its minimum) and whose calls time
// out after a second, with the given tiers (nil: the default). A restart keeps them (e.mod).
func newWatchEnv(t *testing.T, tiers []rail.Tier) *env {
	t.Helper()
	e := newEnv(t)
	e.mod = func(c *Config) { c.PollEvery, c.CallTimeout, c.Tiers = MinPollEvery, MinCallTimeout, tiers }
	e.svc = e.newService()
	e.ready()
	return e
}

// restart is a new Service on the same files and the same wallet, checked: what a process restart leaves.
func (e *env) restart() {
	e.t.Helper()
	peer, lock := e.svc.peerRail(), e.svc.settleLock()
	e.svc = e.newService()
	e.svc.SetPeer(peer, lock)
	e.ready()
}

func (e *env) pass() error {
	e.t.Helper()
	return e.svc.pass(bg)
}

// passes runs n passes, advancing the clock by a second before each.
func (e *env) passes(n int) {
	e.t.Helper()
	for i := 0; i < n; i++ {
		e.clk.Advance(time.Second)
		e.pass()
	}
}

// addr issues the order's subaddress through Address.
func (e *env) addr(o store.Order) string {
	e.t.Helper()
	a, err := e.svc.Address(bg, o.ID)
	if err != nil {
		e.t.Fatalf("Address: %v", err)
	}
	return a.Address
}

// payOrder issues the order's subaddress and pays pico to it.
func (e *env) payOrder(o store.Order, pico int64) string {
	e.t.Helper()
	return e.w.Pay(e.addr(o), pico)
}

func (e *env) view(o store.Order) rail.View {
	e.t.Helper()
	v, err := e.svc.Rail(bg, o)
	if err != nil {
		e.t.Fatalf("Rail: %v", err)
	}
	return v
}

func (e *env) events(o store.Order) []store.Event {
	e.t.Helper()
	l, err := e.st.EventsForOrder(bg, o.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

func (e *env) eventCount(o store.Order, typ string) int {
	n := 0
	for _, ev := range e.events(o) {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

func (e *env) hasEvent(o store.Order, typ string, seq int) bool {
	for _, ev := range e.events(o) {
		if ev.Type == typ && ev.Seq == seq {
			return true
		}
	}
	return false
}

// count is the number of log lines whose message is exactly msg.
func (e *env) count(msg string) int { return strings.Count(e.log.String(), `msg="`+msg+`"`) }

func (e *env) sub(o store.Order) store.Subaddress {
	e.t.Helper()
	a, err := e.st.SubaddressForOrder(bg, o.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) lookup(id ledger.U128) (ledger.Transfer, bool) {
	e.t.Helper()
	tr, ok, err := e.lg.LookupTransfer(bg, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return tr, ok
}

func (e *env) fwd(o store.Order, txid string, ep uint32) (ledger.Transfer, bool) {
	return e.lookup(PaidTransferID(txid, uint32(e.sub(o).Index), ep))
}

func (e *env) rev(o store.Order, txid string, ep uint32) (ledger.Transfer, bool) {
	return e.lookup(ReversalTransferID(txid, uint32(e.sub(o).Index), ep))
}

func (e *env) mark(o store.Order, txid string) (store.MoneroMark, bool) {
	e.t.Helper()
	m, ok, err := e.st.CurrentMoneroMark(bg, store.PaymentKey{TxID: txid, Address: e.sub(o).Address})
	if err != nil {
		e.t.Fatal(err)
	}
	return m, ok
}

func (e *env) payments(o store.Order) []store.Payment {
	e.t.Helper()
	l, err := e.st.PaymentsForOrder(bg, o.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

// watched reports whether the payment of txid to the order's subaddress is polled (store.WatchedPayments).
func (e *env) watched(o store.Order, txid string) bool {
	e.t.Helper()
	l, err := e.st.WatchedPayments(bg, e.clk.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range l {
		if p.TxID == txid && p.Address == e.sub(o).Address {
			return true
		}
	}
	return false
}

func (e *env) cursor() store.MoneroCursor {
	e.t.Helper()
	c, _, err := e.st.MoneroCursor(bg)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

var errCrash = errors.New("crash")

// crashAt makes the next arrival at point abort the pass, once.
func (e *env) crashAt(point string) *bool {
	fired := new(bool)
	e.svc.hook = func(p string) error {
		if p == point && !*fired {
			*fired = true
			return errCrash
		}
		return nil
	}
	return fired
}

func (e *env) pastExpiry(o store.Order) {
	if d := o.ExpiresAt.Sub(e.clk.Now()); d > 0 {
		e.clk.Advance(d)
	}
}

func (e *env) toWindowEnd(o store.Order) {
	if d := o.ExpiresAt.Add(store.WatchWindow).Sub(e.clk.Now()); d > 0 {
		e.clk.Advance(d)
	}
}

// mixed makes an order with the given Lightning, on-chain and Monero amounts.
func (e *env) mixed(msat, sat, pico int64) store.Order {
	return e.orderWith(store.Order{AmountMsat: msat, AmountSat: sat, AmountPiconero: pico})
}

func amountOf(tr ledger.Transfer) int64 { return int64(tr.Amount.Lo) }

// safety is the invariant every test ends with, computed from the rows and independently of the code under test:
//   - each payment's transfers: a reversal only with its forward, a later epoch only after a reversal, the amounts the
//     payment's, the forward's code 2 when counted and 4 when late (first seen at or after the expiry, or locked by
//     its sender beyond FirstSeenHeight + 10), the reversal's 3;
//   - an unreversed forward only on a mined payment, or, for a counted payment of an order whose tier needs 0
//     confirmations, on one whose mark is mempool; and every payment mined at its need (confirmations are the
//     cursor's height less the block's height, as wallet-rpc counts them) recorded (a late one only inside the
//     window: past it, it is not watched);
//   - the ledger balances: both accounts move by the sum of the unreversed forwards;
//   - order.paid at most once, the order.reverted seqs 1..n, the paid_again seqs a subset, each once;
//   - Rail's Confirmed is the sum of the unreversed code-2 forwards, Paid follows from it, and a paid order has
//     order.paid.
func (e *env) safety(orders ...store.Order) {
	e.t.Helper()
	var total int64
	cur := e.cursor()
	for _, o := range orders {
		var confirmed int64
		sub, err := e.st.SubaddressForOrder(bg, o.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		} else if err != nil {
			e.t.Fatal(err)
		}
		idx := uint32(sub.Index)
		for _, p := range e.payments(o) {
			late := !p.FirstSeen.Before(o.ExpiresAt) || (p.UnlockTime > 0 && p.UnlockTime > p.FirstSeenHeight+10)
			unreversed := false
			for ep := uint32(0); ep < 100; ep++ {
				f, okF := e.lookup(PaidTransferID(p.TxID, idx, ep))
				r, okR := e.lookup(ReversalTransferID(p.TxID, idx, ep))
				if !okF {
					if okR {
						e.t.Errorf("%s epoch %d: a reversal without its forward", p.TxID[:8], ep)
					}
					if n, ok := e.lookup(PaidTransferID(p.TxID, idx, ep+1)); ok {
						e.t.Errorf("%s: a forward at epoch %d after a gap: %+v", p.TxID[:8], ep+1, n)
					}
					break
				}
				wantCode := uint16(codePaid)
				if late {
					wantCode = codeLate
				}
				if f.Code != wantCode || amountOf(f) != p.AmountPiconero || f.DebitAccountID != MoneroWallet || f.CreditAccountID != MerchantMonero {
					e.t.Errorf("%s epoch %d: forward %+v (late %v)", p.TxID[:8], ep, f, late)
				}
				if !okR {
					unreversed = true
					if _, ok := e.lookup(PaidTransferID(p.TxID, idx, ep+1)); ok {
						e.t.Errorf("%s: epoch %d is unreversed but epoch %d exists", p.TxID[:8], ep, ep+1)
					}
					if f.Code == codePaid {
						confirmed += p.AmountPiconero
					}
					total += p.AmountPiconero
					break
				}
				if r.Code != codeReversed || amountOf(r) != p.AmountPiconero || r.DebitAccountID != MerchantMonero || r.CreditAccountID != MoneroWallet {
					e.t.Errorf("%s epoch %d: reversal %+v", p.TxID[:8], ep, r)
				}
			}
			m, marked, err := e.st.CurrentMoneroMark(bg, store.PaymentKey{TxID: p.TxID, Address: p.Address})
			if err != nil {
				e.t.Fatal(err)
			}
			req := int64(rail.Required(e.svc.cfg.Tiers, o.AmountPiconero))
			zeroConf := !late && req == 0
			if unreversed && (!marked || (m.Kind != store.MarkMined && !(zeroConf && m.Kind == store.MarkMempool))) {
				e.t.Errorf("%s: recorded but its mark is %+v (late %v)", p.TxID[:8], m, late)
			}
			need := req
			if late {
				need = max(req, 1)
			}
			inWindow := e.clk.Now().Before(o.ExpiresAt.Add(store.WatchWindow))
			if (inWindow || !late) && marked && m.Kind == store.MarkMined && cur.Height-m.Height >= need && !unreversed {
				e.t.Errorf("%s: mined at its need (%d) but not recorded", p.TxID[:8], need)
			}
		}
		paid, reverted, again := 0, map[int]bool{}, map[int]int{}
		for _, ev := range e.events(o) {
			switch ev.Type {
			case store.EventPaid:
				paid++
			case store.EventReverted:
				reverted[ev.Seq] = true
			case store.EventPaidAgain:
				again[ev.Seq]++
			}
		}
		if paid > 1 {
			e.t.Errorf("order.paid %d times", paid)
		}
		for i := 1; i <= len(reverted); i++ {
			if !reverted[i] {
				e.t.Errorf("order.reverted seqs are not 1..n: %v", reverted)
			}
		}
		for s, n := range again {
			if !reverted[s] || n != 1 {
				e.t.Errorf("order.paid_again seq %d (%d times) without its reversal: %v", s, n, reverted)
			}
		}
		v := e.view(o)
		if v.Confirmed != confirmed || v.Paid != (confirmed >= o.AmountPiconero) {
			e.t.Errorf("Rail %+v, the rows give confirmed %d of %d", v, confirmed, o.AmountPiconero)
		}
		if v.Paid && paid != 1 {
			e.t.Errorf("paid in Monero without order.paid")
		}
	}
	w, err := e.lg.Balance(bg, MoneroWallet)
	if err != nil {
		e.t.Fatal(err)
	}
	m, err := e.lg.Balance(bg, MerchantMonero)
	if err != nil {
		e.t.Fatal(err)
	}
	wNet := int64(w.DebitsPosted.Lo) - int64(w.CreditsPosted.Lo)
	mNet := int64(m.CreditsPosted.Lo) - int64(m.DebitsPosted.Lo)
	if wNet != mNet || wNet != total {
		e.t.Errorf("ledger: wallet %d, merchant %d, unreversed forwards %d", wNet, mNet, total)
	}
}

var errOutage = fmt.Errorf("%w: test outage", xmrrpc.ErrTransport)

// inject wraps the service's RPC: it can add to or change get_transfers' answer, change get_transfer_by_txid's, hang
// get_transfer_by_txid for one txid until the call's context ends, and records the get_transfers queries.
type inject struct {
	RPC
	mu      sync.Mutex
	list    func(t *xmrrpc.Transfers)
	byTx    func(txid string, l []xmrrpc.Transfer) []xmrrpc.Transfer
	hangTx  string
	queries []xmrrpc.TransfersQuery
}

func (i *inject) GetTransfers(ctx context.Context, q xmrrpc.TransfersQuery) (xmrrpc.Transfers, error) {
	t, err := i.RPC.GetTransfers(ctx, q)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.queries = append(i.queries, q)
	if err == nil && i.list != nil {
		i.list(&t)
	}
	return t, err
}

func (i *inject) GetTransferByTxID(ctx context.Context, txid string, account uint32) ([]xmrrpc.Transfer, error) {
	i.mu.Lock()
	hang, f := i.hangTx == txid, i.byTx
	i.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil, fmt.Errorf("%w: get_transfer_by_txid: %w", xmrrpc.ErrTransport, ctx.Err())
	}
	l, err := i.RPC.GetTransferByTxID(ctx, txid, account)
	if err == nil && f != nil {
		l = f(txid, l)
	}
	return l, err
}

func (i *inject) set(f func(*inject)) {
	i.mu.Lock()
	f(i)
	i.mu.Unlock()
}

// wrap puts an inject around the service's RPC.
func (e *env) wrap() *inject {
	in := &inject{RPC: e.svc.rpc}
	e.svc.rpc = in
	return in
}

// --- wallet-rpc unreachable ---

// A transport failure of each of the pass's calls, 5 passes running: one ALERT per pass, no mark, no transfer, no
// event, stale from the fourth; the sixth pass catches up.
func TestOutageOnEachMethod(t *testing.T) {
	for _, m := range []string{"get_address", "get_height", "get_transfers", "get_transfer_by_txid"} { // get_address: the identity read
		t.Run(m, func(t *testing.T) {
			e := newWatchEnv(t, nil)
			o := e.order(xmr)
			txid := e.payOrder(o, xmr)
			e.pass() // discovered, in the pool
			if mk, ok := e.mark(o, txid); !ok || mk.Kind != store.MarkMempool {
				t.Fatalf("setup: %+v", mk)
			}
			for i := 1; i <= 5; i++ {
				e.clk.Advance(e.svc.cfg.PollInterval)
				e.w.Fail(m, errOutage)
				if err := e.pass(); err == nil {
					t.Fatalf("pass %d ran to its end", i)
				}
				if got := e.count("ALERT: wallet-rpc call failed"); got != i {
					t.Fatalf("pass %d: %d ALERT lines\n%s", i, got, e.log)
				}
				if mk, _ := e.mark(o, txid); mk.Kind != store.MarkMempool {
					t.Fatalf("pass %d: a mark was added: %+v", i, mk)
				}
				if _, ok := e.fwd(o, txid, 0); ok || len(e.events(o)) != 0 {
					t.Fatalf("pass %d: a transfer or an event was added", i)
				}
				if v := e.view(o); v.Stale != (i >= 4) {
					t.Fatalf("pass %d: Stale %v", i, v.Stale)
				}
			}
			if !e.logHas(`msg="ALERT: wallet-rpc call failed" method=` + m) {
				t.Fatalf("the line does not name %s:\n%s", m, e.log)
			}
			e.w.Mine(1)
			e.clk.Advance(e.svc.cfg.PollInterval)
			if err := e.pass(); err != nil {
				t.Fatal(err)
			}
			v := e.view(o)
			if !v.Paid || v.Stale || e.eventCount(o, store.EventPaid) != 1 || e.count("ALERT: wallet-rpc call failed") != 5 {
				t.Fatalf("the sixth pass: %+v, events %+v", v, e.events(o))
			}
			e.safety(o)
		})
	}
}

// A JSON-RPC error on one payment skips that payment and its order for the pass; another order is paid.
func TestPaymentErrorDoesNotStopTheOthers(t *testing.T) {
	e := newWatchEnv(t, nil)
	a, b := e.order(xmr), e.order(xmr/2)
	ta := e.payOrder(a, xmr)
	e.payOrder(b, xmr/2)
	e.w.FailTx(ta, &xmrrpc.Error{Code: -1, Method: "get_transfer_by_txid"})
	e.w.Mine(1)
	e.pass()
	e.pass()
	if !e.view(b).Paid || e.view(a).Paid || e.count("wallet-rpc call failed for a payment") != 2 || e.count("ALERT: wallet-rpc call failed") != 0 {
		t.Fatalf("a %+v b %+v\n%s", e.view(a), e.view(b), e.log)
	}
	if !e.view(a).Stale || e.view(b).Stale {
		t.Fatalf("a per-payment error: a stale %v, b stale %v", e.view(a).Stale, e.view(b).Stale)
	}
	if _, ok := e.mark(a, ta); ok {
		t.Fatal("the failing payment got a mark")
	}
	e.w.FailTx(ta, nil)
	e.pass()
	if !e.view(a).Paid || e.view(a).Stale {
		t.Fatal("not paid, or stale, once the payment answers")
	}
	e.safety(a, b)
}

// The same entries listed in ten passes: one row, one mark, one forward.
func TestReplayGivesOneRow(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.passes(10)
	if l := e.payments(o); len(l) != 1 || l[0].TxID != txid {
		t.Fatalf("%+v", l)
	}
	if m, _ := e.mark(o, txid); m.Seq != 1 {
		t.Fatalf("an unchanged payment was marked again: %+v", m)
	}
	e.w.Mine(1)
	e.passes(10)
	if m, _ := e.mark(o, txid); m.Seq != 2 {
		t.Fatalf("marks %+v", m)
	}
	if _, ok := e.fwd(o, txid, 1); ok || !e.view(o).Paid || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatal("recorded twice")
	}
	e.safety(o)
}

// A stop of the service while a pass waits on wallet-rpc in steps 1 and 2 is no failed call: the pass returns the
// context's error and logs no ALERT. The same call failing without a cancel logs it, once.
func TestStopDuringAPassCallLogsNoAlert(t *testing.T) {
	for _, m := range []string{"get_address", "get_height", "get_transfers"} {
		t.Run(m, func(t *testing.T) {
			e := newWatchEnv(t, nil)
			e.w.Hang(m)
			ctx, cancel := context.WithCancel(bg)
			time.AfterFunc(50*time.Millisecond, cancel)
			if err := e.svc.pass(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("pass: %v, want the context's error", err)
			}
			if n := e.count(logCallFailed); n != 0 {
				t.Fatalf("%d call-failed lines on a stop:\n%s", n, e.log)
			}
			e.w.Unhang(m)
			e.w.Fail(m, errOutage)
			if err := e.pass(); err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("pass with a failed call: %v", err)
			}
			if n := e.count(logCallFailed); n != 1 || !e.logHas(`msg="`+logCallFailed+`" method=`+m) {
				t.Fatalf("%d call-failed lines on a failed call, want 1 for %s:\n%s", n, m, e.log)
			}
		})
	}
}

// execDB runs a statement through a second connection to the store's file.
func (e *env) execDB(q string) {
	e.t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(e.dir, "orders.db"))
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q); err != nil {
		e.t.Fatal(err)
	}
}

// afterStop counts the end check's wallet-rpc calls (get_height, get_address) made after stopped is set.
type afterStop struct {
	RPC
	stopped *atomic.Bool
	n       atomic.Int32
}

func (a *afterStop) GetHeight(ctx context.Context) (int64, error) {
	if a.stopped.Load() {
		a.n.Add(1)
	}
	return a.RPC.GetHeight(ctx)
}

func (a *afterStop) GetAddress(ctx context.Context, account uint32, indices []uint32) (xmrrpc.Addresses, error) {
	if a.stopped.Load() {
		a.n.Add(1)
	}
	return a.RPC.GetAddress(ctx, account, indices)
}

// A stop that lands in step 3's store read of a payment's current mark (the first store call after its
// get_transfer_by_txid answers) is no failed track: the pass returns the context's error and logs no failure line.
// The reverse: the same read failing with a live context logs logTrackFailed once, with the site's word.
func TestStopDuringTrackLogsNoTrackFailed(t *testing.T) {
	lines := []string{logCheckFailed, logCallFailed, logScanFailed, logTrackFailed, logSettleFailed, logPaymentFailed}
	t.Run("stop", func(t *testing.T) {
		e := newWatchEnv(t, nil)
		o := e.order(xmr)
		e.payOrder(o, xmr)
		ctx, cancel := context.WithCancel(bg)
		var stopped atomic.Bool
		e.wrap().set(func(i *inject) {
			i.byTx = func(txid string, l []xmrrpc.Transfer) []xmrrpc.Transfer { stopped.Store(true); cancel(); return l }
		})
		after := &afterStop{RPC: e.svc.rpc, stopped: &stopped}
		e.svc.rpc = after
		if err := e.svc.pass(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("pass: %v, want the context's error", err)
		}
		for _, l := range lines {
			if e.count(l) != 0 {
				t.Fatalf("%q on a stop:\n%s", l, e.log)
			}
		}
		if len(e.payments(o)) != 0 {
			t.Fatal("a stopped pass wrote a payment")
		}
		// A pass that went on past the stop would make the end check's calls (trackAll's context arm ends it first).
		if n := after.n.Load(); n != 0 {
			t.Fatalf("%d wallet-rpc calls after the stop: the pass went on", n)
		}
	})
	t.Run("failure", func(t *testing.T) {
		e := newWatchEnv(t, nil)
		e.payOrder(e.order(xmr), xmr)
		e.wrap().set(func(i *inject) {
			i.byTx = func(txid string, l []xmrrpc.Transfer) []xmrrpc.Transfer {
				e.execDB(`ALTER TABLE monero_marks RENAME TO marks_x`)
				return l
			}
		})
		e.pass()
		if e.count(logTrackFailed) != 1 || !e.logHas(`msg="`+logTrackFailed+`" at=mark-read`+"\n") {
			t.Fatalf("want one %q line at mark-read:\n%s", logTrackFailed, e.log)
		}
	})
}

// The first read of step 3 failing with a live context logs logTrackFailed once, with its site's word and no other
// attribute (the orders table is renamed under a stored payment, which WatchedPayments joins).
func TestTrackFailedNamesItsSite(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.payOrder(e.order(xmr), xmr)
	e.pass()
	e.w.Mine(1)
	e.execDB(`ALTER TABLE orders RENAME TO orders_x`)
	e.pass()
	if e.count(logTrackFailed) != 1 || !e.logHas(`msg="`+logTrackFailed+`" at=watched`+"\n") {
		t.Fatalf("want one %q line at watched:\n%s", logTrackFailed, e.log)
	}
}

// Ruling 8-A6: the pass commits its scan, cursor and marks in one transaction, so a crash right after the commit
// (scan:after-record, before settle) leaves the payment and its mark stored together, and nothing recorded; the
// restart records it once.
func TestRestartBetweenCommitAndSettle(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Mine(1)
	fired := e.crashAt("scan:after-record")
	if err := e.pass(); !errors.Is(err, errCrash) || !*fired {
		t.Fatalf("no crash: %v", err)
	}
	if len(e.payments(o)) != 1 {
		t.Fatal("not discovered")
	}
	if m, ok := e.mark(o, txid); !ok || m.Kind != store.MarkMined {
		t.Fatalf("the mark was not committed with the payment: %+v %v", m, ok)
	}
	if _, ok := e.fwd(o, txid, 0); ok {
		t.Fatal("recorded before the crash point")
	}
	e.restart()
	e.pass()
	if _, ok := e.fwd(o, txid, 0); !ok || !e.view(o).Paid {
		t.Fatal("not recorded after the restart")
	}
	e.restart()
	e.passes(2)
	if _, ok := e.fwd(o, txid, 1); ok {
		t.Fatal("recorded twice")
	}
	e.safety(o)
}

func TestRestartBetweenMarkAndTransfer(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Mine(1)
	e.crashAt("track:after-mark")
	if err := e.pass(); !errors.Is(err, errCrash) {
		t.Fatalf("no crash: %v", err)
	}
	if m, _ := e.mark(o, txid); m.Kind != store.MarkMined {
		t.Fatalf("%+v", m)
	}
	if _, ok := e.fwd(o, txid, 0); ok {
		t.Fatal("recorded before the crash point")
	}
	e.restart()
	e.passes(3)
	if _, ok := e.fwd(o, txid, 0); !ok || !e.view(o).Paid {
		t.Fatal("not recorded after the restart")
	}
	if _, ok := e.fwd(o, txid, 1); ok {
		t.Fatal("recorded twice")
	}
	e.safety(o)
}

// --- discover ---

// Rulings 8-T10a-2 and 8-A4: entries SatBooth must not read are left out and counted into one ALERT
// per pass naming the listing's height range, and the pass is not failed: a second entry for the same (txid,
// subaddress) in the same list with another amount (the first is kept), and the client's Dropped. A payment of 2^53
// piconero (above store.MaxPiconero) to an issued subaddress gets its own ALERT with the order id, once per payment
// per process, and is not stored. A payment to the wallet's primary address and one to a subaddress SatBooth did not
// issue are skipped without being counted: they are not SatBooth's orders.
func TestDiscoverIgnoresForeignEntries(t *testing.T) {
	e := newWatchEnv(t, nil)
	o, big := e.order(xmr), e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Pay(e.w.Primary(), 5*xmr)
	e.w.Pay(e.w.SubaddressAt(50), 5*xmr)
	e.w.Pay(e.addr(big), 1<<53)
	in := e.wrap()
	in.set(func(i *inject) {
		i.list = func(t *xmrrpc.Transfers) {
			for _, x := range t.Pool {
				if x.TxID == txid {
					x.Amount = 2 * xmr
					t.Pool = append(t.Pool, x)
					break
				}
			}
			t.Dropped += 2
		}
	})
	if err := e.pass(); err != nil {
		t.Fatalf("the pass failed: %v", err)
	}
	if l := e.payments(o); len(l) != 1 || l[0].AmountPiconero != xmr {
		t.Fatalf("payments %+v", l)
	}
	if l := e.payments(big); len(l) != 0 {
		t.Fatalf("an amount above the bound was stored: %+v", l)
	}
	if n := e.count(logDropped); n != 1 || !e.logHas(fmt.Sprintf(`msg="%s" count=3 from_height=0 to_height=%d`, logDropped, e.cursor().Height)) {
		t.Fatalf("%d dropped lines (want one, count=3: the repeat, the client's 2, from 0 to the wallet's height):\n%s", n, e.log)
	}
	if n := e.count(logOversized); n != 1 || !e.logHas(`msg="`+logOversized+`" order=`+big.ID) {
		t.Fatalf("%d oversized lines (want one, naming the order):\n%s", n, e.log)
	}
	if e.count("ALERT: wallet-rpc answered with a bad transfer") != 0 || e.count(logCallFailed) != 0 {
		t.Fatalf("another ALERT for a dropped entry:\n%s", e.log)
	}
	e.w.Mine(1)
	e.pass()
	if !e.view(o).Paid || e.view(big).Received != 0 {
		t.Fatalf("%+v %+v", e.view(o), e.view(big))
	}
	if n := e.count(logOversized); n != 1 {
		t.Fatalf("the oversized ALERT repeated for the same payment: %d lines", n)
	}
	e.safety(o, big)
}

// Ruling R8: a listing with 4,097 distinct oversized payments to an issued subaddress.
// The dedupe set held 4,096 keys and was cleared when full, so every pass logged all 4,097 lines again. Now a pass logs
// at most maxAlertLines lines, then one summary ALERT with the count of the rest; every payment logged or counted
// is remembered (the set is never cleared), so the next passes add at most the summary for the ones the full set
// cannot hold.
func TestOversizedAlertBounded(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	big := e.w.Pay(e.addr(o), 1<<53)
	const n = 4097
	in := e.wrap()
	in.set(func(i *inject) {
		i.list = func(t *xmrrpc.Transfers) {
			for _, x := range t.Pool {
				if x.TxID != big {
					continue
				}
				for j := 1; j < n; j++ {
					y := x
					y.TxID = fmt.Sprintf("%064x", j)
					t.Pool = append(t.Pool, y)
				}
				return
			}
		}
	})
	var lines, summaries []int
	for pass := range 3 {
		before, beforeSum := e.count(logOversized), e.count(logOversizedMore)
		if err := e.pass(); err != nil {
			t.Fatalf("pass %d: %v", pass+1, err)
		}
		lines = append(lines, e.count(logOversized)-before)
		summaries = append(summaries, e.count(logOversizedMore)-beforeSum)
	}
	if lines[0] != maxAlertLines || summaries[0] != 1 ||
		!e.logHas(fmt.Sprintf(`msg="%s" count=%d`, logOversizedMore, n-maxAlertLines)) {
		t.Fatalf("pass 1: %d lines and %d summaries, want %d and one with count=%d", lines[0], summaries[0], maxAlertLines,
			n-maxAlertLines)
	}
	for p := 1; p < 3; p++ {
		if lines[p] != 0 || summaries[p] > 1 {
			t.Fatalf("pass %d: %d lines and %d summaries, want none and at most one (lines per pass %v)", p+1, lines[p],
				summaries[p], lines)
		}
	}
	if l := e.payments(o); len(l) != 0 {
		t.Fatalf("an oversized payment was stored: %d", len(l))
	}
}

// The R8 treatment for the other two sets: the double-spend ALERT and the bad-transfer ALERT of track
// were deduplicated by sets cleared when full, so 4,097 distinct payments logged 4,097 lines on every pass. One
// issued subaddress, one real pool payment, the listing cloned to 4,097 distinct txids (stored by the first pass), and
// get_transfer_by_txid answering each as double spent (a tier-1 order: mempool and the ALERT) or with another amount
// (the bad-transfer ALERT): over three passes, the first logs maxAlertLines lines and one summary with the count of
// the rest, the next two no line and at most one summary.
func TestTrackAlertsBounded(t *testing.T) {
	for _, tc := range []struct{ name, line, more string }{
		{"double spend", logDoubleSpend, logDoubleMore},
		{"bad transfer", logBadTransfer, logBadMore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newWatchEnv(t, nil)
			o := e.order(xmr)
			real := e.payOrder(o, xmr)
			addr := e.addr(o)
			const n = 4097
			in := e.wrap()
			in.set(func(i *inject) {
				i.list = func(t *xmrrpc.Transfers) {
					for _, x := range t.Pool {
						if x.TxID != real {
							continue
						}
						for j := 1; j < n; j++ {
							y := x
							y.TxID = fmt.Sprintf("%064x", j)
							t.Pool = append(t.Pool, y)
						}
						return
					}
				}
				i.byTx = func(txid string, l []xmrrpc.Transfer) []xmrrpc.Transfer {
					y := xmrrpc.Transfer{TxID: txid, Address: addr, Amount: xmr, Type: "pool", Locked: true}
					if tc.line == logDoubleSpend {
						y.DoubleSpendSeen = true
					} else {
						y.Amount = 2 * xmr
					}
					return []xmrrpc.Transfer{y}
				}
			})
			var lines, summaries []int
			for pass := range 3 {
				e.clk.Advance(time.Second)
				before, beforeSum := e.count(tc.line), e.count(tc.more)
				if err := e.pass(); err != nil {
					t.Fatalf("pass %d: %v", pass+1, err)
				}
				lines = append(lines, e.count(tc.line)-before)
				summaries = append(summaries, e.count(tc.more)-beforeSum)
			}
			if len(e.payments(o)) != n {
				t.Fatalf("setup: %d payments stored, want %d", len(e.payments(o)), n)
			}
			if lines[0] != maxAlertLines || summaries[0] != 1 ||
				!e.logHas(fmt.Sprintf(`msg="%s" count=%d`, tc.more, n-maxAlertLines)) {
				t.Fatalf("pass 1: %d lines and %d summaries, want %d and one with count=%d", lines[0], summaries[0],
					maxAlertLines, n-maxAlertLines)
			}
			for p := 1; p < 3; p++ {
				if lines[p] != 0 || summaries[p] > 1 {
					t.Fatalf("pass %d: %d lines and %d summaries, want none and at most one (lines per pass %v)", p+1,
						lines[p], summaries[p], lines)
				}
			}
		})
	}
}

// Ruling 8-T10a-2: payments to addresses SatBooth never issued, the primary address and a subaddress not in the store,
// in the pool and mined, are skipped without the dropped line, on every pass, and nothing is stored for them.
func TestDiscoverSkipsUnissuedSilently(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	e.payOrder(o, xmr)
	e.w.Pay(e.w.Primary(), 5*xmr)
	e.w.Pay(e.w.SubaddressAt(50), 5*xmr)
	e.passes(2)
	e.w.Mine(1)
	e.passes(2)
	if n := e.count(logDropped); n != 0 {
		t.Fatalf("%d dropped lines for payments to unissued addresses:\n%s", n, e.log)
	}
	if l := e.payments(o); len(l) != 1 || !e.view(o).Paid {
		t.Fatalf("payments %+v, view %+v", l, e.view(o))
	}
	if rows, err := e.st.SubaddressesByAddress(bg, []string{e.w.Primary(), e.w.SubaddressAt(50)}); err != nil || len(rows) != 0 {
		t.Fatalf("unissued addresses in the store: %v %v", rows, err)
	}
	e.safety(o)
}

// Ruling 8-T10a-2: a (txid, subaddress) listed both mined ("in") and in the pool, as wallet-rpc can list it around a
// reorg, counts once and the mined entry wins (the tier-1 order is paid), without the dropped line. A key repeated
// within the in list itself is still dropped.
func TestDiscoverInAndPoolOnce(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Mine(1)
	in := e.wrap()
	in.set(func(i *inject) {
		i.list = func(t *xmrrpc.Transfers) {
			for _, x := range t.In {
				if x.TxID == txid {
					x.Type, x.Height = "pool", 0
					t.Pool = append([]xmrrpc.Transfer{x}, t.Pool...)
					break
				}
			}
		}
	})
	e.passes(2)
	if n := e.count(logDropped); n != 0 {
		t.Fatalf("%d dropped lines for an entry listed in and pool:\n%s", n, e.log)
	}
	if l := e.payments(o); len(l) != 1 || !e.view(o).Paid {
		t.Fatalf("payments %+v, view %+v: the mined entry must win", l, e.view(o))
	}
	// keep itself: one payment, the mined one (Confirmed), nothing counted, whichever list carries the pool copy first.
	tr, err := e.w.GetTransfers(bg, xmrrpc.TransfersQuery{In: true, Pool: true})
	if err != nil || len(tr.In) == 0 {
		t.Fatalf("get_transfers: %d in, %v", len(tr.In), err)
	}
	var mined xmrrpc.Transfer
	for _, x := range tr.In {
		if x.TxID == txid {
			mined = x
		}
	}
	copyPool := mined
	copyPool.Type, copyPool.Height = "pool", 0
	kept, _, _, dropped, err := e.svc.keep(bg, xmrrpc.Transfers{In: []xmrrpc.Transfer{mined}, Pool: []xmrrpc.Transfer{copyPool}})
	if err != nil || dropped != 0 || len(kept) != 1 || !kept[0].Confirmed {
		t.Fatalf("keep of an entry listed in and pool: %+v, dropped %d, %v; want the mined entry alone", kept, dropped, err)
	}
	in.set(func(i *inject) {
		i.list = func(t *xmrrpc.Transfers) {
			for _, x := range t.In {
				if x.TxID == txid {
					t.In = append(t.In, x)
					break
				}
			}
		}
	})
	e.pass()
	if n := e.count(logDropped); n != 1 || !e.logHas(`msg="`+logDropped+`" count=1 from_height=0 to_height=`) {
		t.Fatalf("a key repeated within the in list: %d dropped lines:\n%s", n, e.log)
	}
	e.safety(o)
}

// get_transfers is asked for the account's in and pool entries from max(0, cursor - SettledDepth - 1): min_height is
// exclusive (wallet2.cpp:7243), so a payment SettledDepth deep is still listed.
func TestDiscoverQuery(t *testing.T) {
	e := newWatchEnv(t, nil)
	in := e.wrap()
	e.pass()
	e.w.Mine(30)
	e.pass()
	e.pass()
	q := in.queries
	if len(q) != 3 || !q[0].In || !q[0].Pool || !q[0].FilterByHeight || q[0].Account != 0 || q[0].MinHeight != 0 {
		t.Fatalf("queries %+v", q)
	}
	if q[2].MinHeight != 31-10-1 {
		t.Fatalf("the third query's min_height %d, want %d", q[2].MinHeight, 31-10-1)
	}
	// A payment exactly SettledDepth deep against the cursor is listed again by the next scan (one block of slack),
	// and the scan keeps its one row and its mark.
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Mine(1)
	e.pass()
	e.w.Mine(9)
	e.pass() // the cursor is now 10 above the payment's block
	m1, _ := e.mark(o, txid)
	listed := false
	in.set(func(i *inject) {
		i.list = func(t *xmrrpc.Transfers) {
			for _, x := range t.In {
				listed = listed || x.TxID == txid
			}
		}
	})
	e.pass()
	m2, _ := e.mark(o, txid)
	if c := e.cursor().Height; c-m1.Height != 10 || !listed || len(e.payments(o)) != 1 || m2.Seq != m1.Seq {
		t.Fatalf("cursor %d, mark %+v → %+v, listed %v, payments %+v", c, m1, m2, listed, e.payments(o))
	}
	e.safety(o)
}

// A stored payment listed with another amount (or unlock_time) is left out with the bad-transfer ALERT; the other
// entries of the scan are recorded and the pass is not failed.
func TestRelistedDifferentlyDoesNotWedge(t *testing.T) {
	e := newWatchEnv(t, nil)
	a, b := e.order(xmr), e.order(xmr)
	ta := e.payOrder(a, xmr)
	e.pass()
	in := e.wrap()
	in.set(func(i *inject) {
		i.list = func(t *xmrrpc.Transfers) {
			for k := range t.Pool {
				if t.Pool[k].TxID == ta {
					t.Pool[k].UnlockTime = 7
				}
			}
		}
	})
	tb := e.payOrder(b, xmr)
	if err := e.pass(); err != nil {
		t.Fatalf("the pass failed: %v", err)
	}
	if e.count("ALERT: wallet-rpc answered with a bad transfer") != 1 || len(e.payments(b)) != 1 {
		t.Fatalf("payments of b %+v\n%s", e.payments(b), e.log)
	}
	if l := e.payments(a); len(l) != 1 || l[0].UnlockTime != 0 {
		t.Fatalf("the stored row changed: %+v", l)
	}
	e.passes(3) // still listed so, still left out, alerted once per process
	if e.count("ALERT: wallet-rpc answered with a bad transfer") != 1 {
		t.Fatalf("the ALERT repeats:\n%s", e.log)
	}
	in.set(func(i *inject) { i.list = nil })
	e.w.Mine(1)
	e.pass()
	if !e.view(a).Paid || !e.view(b).Paid {
		t.Fatalf("%+v %+v (%s)", e.view(a), e.view(b), tb)
	}
	e.safety(a, b)
}

// A payment discovered after its subaddress's watch window logs the ALERT once, when it is added.
func TestReceiveAfterTheWindow(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	a := e.addr(o)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	e.toWindowEnd(o)
	e.pass()
	e.w.Pay(a, xmr)
	e.passes(3)
	if e.count("ALERT: a Monero payment arrived after the subaddress's watch window") != 1 || len(e.payments(o)) != 1 {
		t.Fatalf("payments %+v\n%s", e.payments(o), e.log)
	}
	if e.eventCount(o, store.EventExpired) != 1 || e.eventCount(o, store.EventPaid) != 0 {
		t.Fatalf("events %+v", e.events(o))
	}
	e.safety(o)
}

// --- track ---

// A per-payment answer whose amount differs from the stored row logs the bad-transfer ALERT and skips the payment;
// a known transaction with no entry for the subaddress is no mark (never dead), a line, and the order stale.
func TestTrackBadAnswers(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.pass()
	in := e.wrap()
	in.set(func(i *inject) {
		i.byTx = func(_ string, l []xmrrpc.Transfer) []xmrrpc.Transfer {
			for k := range l {
				l[k].Amount++
			}
			return l
		}
	})
	e.w.Mine(1)
	e.pass()
	if m, _ := e.mark(o, txid); m.Kind != store.MarkMempool || e.view(o).Paid || !e.view(o).Stale ||
		e.count("ALERT: wallet-rpc answered with a bad transfer") != 1 {
		t.Fatalf("a changed amount: %+v %+v\n%s", m, e.view(o), e.log)
	}
	e.passes(2) // once per payment per process
	if e.count("ALERT: wallet-rpc answered with a bad transfer") != 1 || !e.view(o).Stale {
		t.Fatalf("the ALERT repeats:\n%s", e.log)
	}
	in.set(func(i *inject) {
		i.byTx = func(_ string, l []xmrrpc.Transfer) []xmrrpc.Transfer {
			for k := range l {
				l[k].Address = e.w.SubaddressAt(90)
			}
			return l
		}
	})
	before := e.count("wallet-rpc call failed for a payment")
	e.pass()
	if m, _ := e.mark(o, txid); m.Kind != store.MarkMempool || !e.view(o).Stale || e.count("wallet-rpc call failed for a payment") != before+1 {
		t.Fatalf("no entry for the subaddress: %+v %+v\n%s", m, e.view(o), e.log)
	}
	in.set(func(i *inject) { i.byTx = nil })
	e.pass()
	if !e.view(o).Paid || e.view(o).Stale {
		t.Fatalf("%+v", e.view(o))
	}
	e.safety(o)
}

// A call that hangs on one payment times out after CallTimeout and skips only that payment.
func TestHangOnOnePayment(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.svc.callTimeout = 300 * time.Millisecond
	a, b, c := e.order(xmr), e.order(xmr), e.order(xmr)
	e.payOrder(a, xmr)
	tb := e.payOrder(b, xmr)
	e.payOrder(c, xmr)
	e.pass()
	in := e.wrap()
	in.set(func(i *inject) { i.hangTx = tb })
	e.w.Mine(1)
	start := time.Now()
	e.pass()
	if d := time.Since(start); d < 300*time.Millisecond || d > 300*time.Millisecond+time.Second {
		t.Fatalf("the pass took %v", d)
	}
	if !e.view(a).Paid || !e.view(c).Paid || e.view(b).Paid {
		t.Fatal("the other payments were not refreshed")
	}
	if e.count("wallet-rpc call failed for a payment") != 1 || e.count(logCallFailed) != 0 || e.view(a).Stale || !e.view(b).Stale {
		t.Fatalf("a timeout is per payment:\n%s", e.log)
	}
	in.set(func(i *inject) { i.hangTx = "" })
	e.pass()
	if !e.view(b).Paid {
		t.Fatal("b")
	}
	e.safety(a, b, c)
}

// --- Run, pacing, staleness, the cursor ---

func TestRunStopsWithItsContext(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	in := e.wrap()
	in.set(func(i *inject) { i.hangTx = txid })
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	if err := e.svc.Run(ctx); !errors.Is(err, ErrRunning) {
		t.Fatalf("a second Run: %v", err)
	}
	cancel()
	start := time.Now()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(e.svc.cfg.PollInterval):
		t.Fatal("Run did not stop within one PollInterval")
	}
	if time.Since(start) > e.svc.cfg.PollInterval {
		t.Fatal("slow stop")
	}
	if e.w.Calls("get_transfers") < 1 {
		t.Fatal("Run made no pass")
	}
}

func TestRunPassesAtPollInterval(t *testing.T) {
	e := newEnv(t)
	e.mod = func(c *Config) { c.PollEvery, c.PollInterval = MinPollEvery, MinPollInterval }
	e.svc = e.newService()
	e.ready()
	o := e.order(xmr)
	e.payOrder(o, xmr)
	e.w.Mine(1)
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	deadline := time.Now().Add(2*MinPollInterval + time.Second)
	for time.Now().Before(deadline) && e.w.Calls("get_transfers") < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if n := e.w.Calls("get_transfers"); n < 2 || n > 3 {
		t.Fatalf("%d passes in about one PollInterval", n)
	}
	if !e.view(o).Paid {
		t.Fatal("Run did not record the payment")
	}
	e.safety(o)
}

// The tracked payments are polled PollEvery apart: 50 payments at 20 ms take at least a second, in each pass.
func TestPacing(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("wall-clock pacing")
	}
	e := newEnv(t)
	e.mod = func(c *Config) { c.PollEvery = 20 * time.Millisecond }
	e.svc = e.newService()
	e.ready()
	var orders []store.Order
	for i := 0; i < 50; i++ {
		o := e.order(xmr)
		e.payOrder(o, xmr)
		orders = append(orders, o)
	}
	for p := 1; p <= 2; p++ {
		before := e.w.Calls("get_transfers")
		start := time.Now()
		e.pass()
		if d := time.Since(start); d < time.Second {
			t.Fatalf("pass %d took %v for 50 payments at 20 ms", p, d)
		}
		if e.w.Calls("get_transfers") != before+1 || e.w.Calls("get_transfer_by_txid") != 50*p {
			t.Fatalf("calls: get_transfers %d, get_transfer_by_txid %d", e.w.Calls("get_transfers"), e.w.Calls("get_transfer_by_txid"))
		}
	}
	e.safety(orders...)
}

// Stale reads the last good pass, not the cursor: a quiet wallet writes no cursor row.
func TestStaleFollowsTheLastGoodPass(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	if !e.view(o).Stale {
		t.Fatal("not stale before any pass")
	}
	e.pass()
	for i := 0; i < 10; i++ {
		e.clk.Advance(e.svc.cfg.PollInterval)
		e.pass()
		if e.view(o).Stale {
			t.Fatal("stale on a quiet wallet")
		}
	}
	e.clk.Advance(3*e.svc.cfg.PollInterval + time.Second)
	if !e.view(o).Stale {
		t.Fatal("not stale after 3 intervals")
	}
	if v, err := e.svc.Rail(bg, store.Order{ID: o.ID, AmountMsat: 1}); !errors.Is(err, ErrNoMonero) {
		t.Fatalf("%+v %v", v, err)
	}
	if v, ok, err := e.svc.View(bg, store.Order{ID: o.ID, AmountSat: 1}); ok || err != nil || v != (rail.View{}) {
		t.Fatalf("View without a Monero amount: %+v %v %v", v, ok, err)
	}
	if v := e.view(o); v.Issued || v.Address != "" || v.State != rail.Waiting {
		t.Fatalf("an order with no subaddress: %+v", v)
	}
	e.clk.Advance(time.Hour)
	if v := e.view(o); v.State != rail.Waiting { // expired comes from order.expired, which the next pass writes
		t.Fatalf("past the expiry before a pass: %+v", v)
	}
	e.pass()
	if v := e.view(o); v.State != rail.Expired || !e.hasEvent(o, store.EventExpired, 1) {
		t.Fatalf("%+v %+v", v, e.events(o))
	}
	e.safety(o)
}

// The cursor follows the wallet's height; a quiet wallet adds no cursor row.
func TestCursorFollowsTheWalletHeight(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(20)
	e.pass()
	if c := e.cursor(); c.Height != 21 {
		t.Fatalf("cursor %+v", c)
	}
	at := e.cursor().At
	e.passes(3)
	if c := e.cursor(); !c.At.Equal(at) || c.Height != 21 {
		t.Fatalf("a quiet wallet added a cursor row: %+v", c)
	}
}

// Ruling 8-T7a: a Check that has not passed (it failed at startup) is run again at the start of each pass; ruling
// 8-T7c: while it fails other than behind, the pass ends there (no scan), with the paused ALERT once per pass. Once it
// passes, the pass watches and Address issues subaddresses.
func TestCheckRetriedUntilItPasses(t *testing.T) {
	e := newEnv(t)
	e.mod = func(c *Config) { c.PollEvery = MinPollEvery }
	e.svc = e.newService()
	e.w.Fail("get_version", errOutage)
	if err := e.svc.Check(bg); err == nil {
		t.Fatal("Check passed")
	}
	o := e.order(xmr)
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Address before a passed Check: %v", err)
	}
	e.w.Fail("get_version", errOutage)
	if err := e.pass(); !errors.Is(err, errPaused) {
		t.Fatalf("the pass ran on after a failed Check: %v", err)
	}
	if e.w.Calls("get_transfers") != 0 || e.w.Calls("get_height") != 0 || e.count(logPaused) != 1 || e.count(logCheckFailed) != 0 ||
		!e.logHas(`msg="`+logPaused+`" reason=unavailable`) {
		t.Fatalf("get_transfers %d, paused lines %d\n%s", e.w.Calls("get_transfers"), e.count(logPaused), e.log)
	}
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Address after a failed re-check: %v", err)
	}
	versions := e.w.Calls("get_version")
	e.pass()
	if e.w.Calls("get_version") != versions+1 {
		t.Fatal("the pass did not re-run Check")
	}
	a := e.addr(o)
	e.w.Pay(a, xmr)
	e.w.Mine(1)
	versions = e.w.Calls("get_version")
	e.pass()
	if e.w.Calls("get_version") != versions || !e.view(o).Paid {
		t.Fatalf("a passed Check is not re-run, and the payment is paid: %+v", e.view(o))
	}
	e.svc.behind.Store(true)
	e.pass()
	if e.w.Calls("get_version") != versions+1 || e.svc.behind.Load() {
		t.Fatal("behind is re-checked and cleared")
	}
	e.safety(o)
}

// --- the unused run, the wallet's height ---

// The unused run reaching unused_alert logs the ALERT once, then at most once a day while it lasts; a payment to the
// last subaddress ends it.
func TestUnusedRunAlert(t *testing.T) {
	e := newWatchEnv(t, nil)
	const line = "ALERT: unused Monero subaddresses exceed unused_alert; raise the merchant wallet's subaddress lookahead"
	var last store.Order
	for i := 0; i < 99; i++ {
		last = e.order(xmr)
		e.addr(last)
	}
	e.pass()
	if e.count(line) != 0 {
		t.Fatal("99 alerted")
	}
	last = e.order(xmr)
	a := e.addr(last)
	e.pass()
	e.pass()
	if e.count(line) != 1 || !e.logHas(`msg="`+line+`" unused=100`) {
		t.Fatalf("%d unused lines\n%s", e.count(line), e.log)
	}
	e.clk.Advance(24 * time.Hour)
	e.pass()
	if e.count(line) != 2 {
		t.Fatalf("not repeated after a day: %d", e.count(line))
	}
	e.w.Pay(a, xmr)
	e.clk.Advance(24 * time.Hour)
	e.pass()
	if e.count(line) != 2 {
		t.Fatal("a payment to the last subaddress did not stop the ALERT")
	}
	e.safety(last)
}

// The wallet below SatBooth's cursor (restored, rescanning) is catching up: the ALERT once per pass,
// the cursor's height kept (one row carries the wallet's height, 8-A2), no dead mark, no reversal, no order.reverted; the order stale and still paid. Once the wallet is back
// above the cursor, nothing is reversed, a payment mined meanwhile is found, and the cursor advances.
func TestHeightBehind(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(20)
	o, o2 := e.order(xmr), e.order(xmr)
	txid := e.payOrder(o, xmr)
	a2 := e.addr(o2)
	e.w.Mine(1)
	e.pass()
	e.w.Mine(5)
	e.pass()
	if !e.view(o).Paid {
		t.Fatalf("setup: %+v", e.view(o))
	}
	cur := e.cursor().Height
	e.w.ForgetPayments(cur - 10)
	e.w.SetHeight(cur - 5)
	at := e.cursor().At
	for i := 1; i <= 3; i++ {
		e.clk.Advance(time.Second)
		e.pass()
		if e.count(logHeightBehind) != i {
			t.Fatalf("pass %d: %d ALERT lines", i, e.count(logHeightBehind))
		}
	}
	// The cursor's height stays (where the next scan starts); the wallet's own height is stored beside it, in one
	// row for the three passes (ruling 8-A2).
	if c := e.cursor(); !c.At.Equal(at.Add(time.Second)) || c.Height != cur || c.WalletHeight != cur-5 {
		t.Fatalf("cursor %+v, want height %d and wallet height %d from the first pass", c, cur, cur-5)
	}
	if m, _ := e.mark(o, txid); m.Kind != store.MarkMined {
		t.Fatalf("mark %+v", m)
	}
	if _, ok := e.rev(o, txid, 0); ok || e.eventCount(o, store.EventReverted) != 0 {
		t.Fatal("a reversal while catching up")
	}
	if v := e.view(o); !v.Paid || !v.Stale {
		t.Fatalf("while catching up: %+v", v)
	}
	t2 := e.w.Pay(a2, xmr)
	e.w.Mine(1)
	e.w.ForgetPayments(-1)
	e.w.SetHeight(-1)
	e.pass()
	if e.cursor().Height != e.w.Height() || e.cursor().Height <= cur {
		t.Fatalf("the cursor did not advance: %+v", e.cursor())
	}
	if l := e.payments(o2); len(l) != 1 || l[0].TxID != t2 || !e.view(o2).Paid {
		t.Fatalf("the payment mined meanwhile: %+v %+v", l, e.view(o2))
	}
	if _, ok := e.rev(o, txid, 0); ok || !e.view(o).Paid || e.view(o).Stale {
		t.Fatalf("after catching up: %+v", e.view(o))
	}
	e.safety(o, o2)
}

// A wallet whose height has not moved for 30 minutes of passes logs the ALERT once, again after another 30 minutes,
// not in between; a height that moves resets it.
func TestFrozenHeight(t *testing.T) {
	e := newWatchEnv(t, nil)
	const line = "ALERT: the wallet's height has not moved for 30 minutes; is monerod running and synced?"
	e.pass()
	for _, step := range []struct {
		d    time.Duration
		want int
	}{{29 * time.Minute, 0}, {2 * time.Minute, 1}, {14 * time.Minute, 1}, {15 * time.Minute, 1}, {time.Minute, 2}} {
		e.clk.Advance(step.d)
		e.pass()
		if e.count(line) != step.want {
			t.Fatalf("after %v: %d lines, want %d", step.d, e.count(line), step.want)
		}
	}
	e.w.Mine(1)
	e.clk.Advance(time.Minute)
	e.pass()
	e.clk.Advance(29 * time.Minute)
	e.pass()
	if e.count(line) != 2 {
		t.Fatal("a height that moved did not reset the clock")
	}
}

// An order whose watch window closes without a final event (its Lightning side not done) is reported once.
func TestLeftWindowWithoutFinal(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.mixed(1_000_000, 0, xmr)
	e.addr(o)
	e.pass()
	e.toWindowEnd(o)
	e.pass()
	e.pass()
	if e.count("ALERT: a Monero order left its watch window without a final event; check it by hand") != 1 {
		t.Fatalf("\n%s", e.log)
	}
	if len(e.events(o)) != 0 {
		t.Fatalf("events %+v", e.events(o))
	}
	e.safety(o)
}

// A payment settled at SettledDepth is not polled again, here or after a restart.
func TestSettledPaymentIsNotPolled(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Mine(9)
	e.pass()
	if !e.watched(o, txid) {
		t.Fatal("settled at 9 blocks")
	}
	e.w.Mine(1)
	e.pass()
	if e.watched(o, txid) {
		t.Fatal("not settled at 10 blocks")
	}
	calls := e.w.TxCalls(txid)
	e.restart()
	e.passes(3)
	if e.w.TxCalls(txid) != calls || !e.view(o).Paid {
		t.Fatalf("polled after it settled: %d → %d", calls, e.w.TxCalls(txid))
	}
	e.safety(o)
}

// --- ruling 8-T7c: watching pauses while the wallet fails its Check other than behind ---

// pausedState is what a paused pass must leave alone: the order's marks, transfers and events, and the cursor.
func (e *env) pausedState(orders ...store.Order) string {
	e.t.Helper()
	c := e.cursor()
	st := fmt.Sprintf("cursor %d %d |", c.Height, c.At.Unix())
	for _, o := range orders {
		st += " " + e.outcome(o)
		for _, p := range e.payments(o) {
			m, _ := e.mark(o, p.TxID)
			st += fmt.Sprintf(" mark %s %d %d", m.Kind, m.Height, m.Seq)
		}
	}
	return st
}

// A wallet whose primary address is not the merchant's (ErrAddress): it knows none of SatBooth's transactions and is
// above the cursor, so a pass that watched it would mark every tracked payment dead. The swap happens mid-run, after a
// passed Check: the next pass's identity read sees another base address and pauses before it marks
// anything. Passes write no dead mark, no reversal, no event and no cursor, past the expiry too; once the merchant's
// wallet is back, watching resumes and nothing was lost.
func TestPausedOnAWrongWallet(t *testing.T) {
	e := newWatchEnv(t, nil)
	paid, seen, none := e.order(xmr), e.order(xmr), e.order(xmr)
	e.payOrder(paid, xmr)
	e.w.Mine(1)
	e.payOrder(seen, xmr)
	e.addr(none) // unpaid, past its expiry while paused: a settle would write order.expired
	e.pass()
	if !e.view(paid).Paid || e.view(seen).State != rail.Seen || e.svc.baseAddress() == "" {
		t.Fatalf("setup: %+v %+v", e.view(paid), e.view(seen))
	}
	// The swap: wallet-rpc restarted on another wallet, synced above the cursor, which answers -8 (an empty list) for
	// every tracked txid.
	wrong := monerotest.New(8)
	wrong.Mine(100)
	e.svc.rpc = &override{RPC: wrong}
	before := e.pausedState(paid, seen, none)
	e.pastExpiry(seen)
	e.w.Mine(1) // the merchant's chain moves on meanwhile: seen's payment is mined
	for i := 1; i <= 3; i++ {
		e.clk.Advance(time.Second)
		err := e.pass()
		if got := e.pausedState(paid, seen, none); got != before || len(e.events(none)) != 0 ||
			e.eventCount(paid, store.EventReverted) != 0 || e.count(logReorg) != 0 {
			t.Fatalf("pass %d wrote while paused:\n got %s\nwant %s\n%s", i, got, before, e.log)
		}
		if !errors.Is(err, errPaused) {
			t.Fatalf("pass %d: %v", i, err)
		}
		if i == 1 {
			// The identity read caught it, not Check: no version call, one get_address, one paused line, and the base
			// address cleared, so Address refuses and the next pass re-runs Check.
			if wrong.Calls("get_version") != 0 || wrong.Calls("get_address") != 1 || e.count(logPaused) != 1 ||
				e.svc.baseAddress() != "" || e.svc.watchable.Load() {
				t.Fatalf("first paused pass: version %d, get_address %d, paused lines %d\n%s",
					wrong.Calls("get_version"), wrong.Calls("get_address"), e.count(logPaused), e.log)
			}
			if _, err := e.svc.Address(bg, e.order(xmr).ID); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("Address on a wrong wallet: %v", err)
			}
		}
	}
	// Passes 2 and 3 pause through step 0's Check (the base is cleared), which fails with ErrAddress: one line each.
	if e.count(logPaused) != 3 || strings.Count(e.log.String(), `msg="`+logPaused+`" reason=address`) != 3 || wrong.Calls("get_version") != 2 ||
		wrong.Calls("get_transfer_by_txid") != 0 || wrong.Calls("get_transfers") != 0 {
		t.Fatalf("paused lines %d, txid calls %d\n%s", e.count(logPaused), wrong.Calls("get_transfer_by_txid"), e.log)
	}
	e.svc.rpc = e.rpc
	if err := e.pass(); err != nil {
		t.Fatalf("watching did not resume: %v", err)
	}
	if !e.view(paid).Paid || !e.view(seen).Paid || len(e.events(paid)) != 1 || e.eventCount(seen, store.EventPaid) != 1 ||
		e.eventCount(seen, store.EventExpired) != 0 || e.count(logReorg) != 0 || e.svc.baseAddress() == "" {
		t.Fatalf("after resuming: %+v %+v %+v", e.view(paid), e.view(seen), e.events(seen))
	}
	if e.eventCount(none, store.EventExpired) != 1 || len(e.events(none)) != 1 {
		t.Fatalf("the unpaid order after resuming: %+v", e.events(none))
	}
	e.safety(paid, seen, none)
}

// A pass that does not run Check makes one identity read, get_address of the account's index 0, and
// compares it with the base address kept from the last passed Check: for another account than 0, that account's own
// base address, not the primary one. A pass that runs Check makes no second read at its start. Every pass makes the
// same read once more at its end (ruling 8-A6's end check), so a pass that does not run Check makes two. An answer that is not a base address
// (two entries) pauses as a different one does; the next pass re-runs Check, which passes on the merchant's wallet.
func TestIdentityReadEveryPass(t *testing.T) {
	e := newEnv(t)
	e.mod = func(c *Config) { c.PollEvery, c.CallTimeout, c.AccountIndex = MinPollEvery, MinCallTimeout, 1 }
	e.svc = e.newService()
	e.ready()
	o := e.order(xmr)
	e.payOrder(o, xmr)
	e.w.Mine(1)
	for i := 1; i <= 3; i++ {
		e.clk.Advance(time.Second)
		before := e.w.Calls("get_address")
		if err := e.pass(); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if n := e.w.Calls("get_address") - before; n != 2 {
			t.Fatalf("pass %d made %d get_address calls, want 2 (the identity read and the end check)", i, n)
		}
	}
	if !e.view(o).Paid || e.count(logPaused) != 0 {
		t.Fatalf("account 1 watched: %+v\n%s", e.view(o), e.log)
	}
	two := true
	e.rpc.set(func(r *override) {
		r.getAddr = func(account uint32, ix []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
			if two && err == nil && len(ix) == 1 && ix[0] == 0 {
				two = false
				a.Addresses = append(a.Addresses, xmrrpc.AddressEntry{Address: e.w.SubaddressAt(1), Index: 1})
			}
			return a, err
		}
	})
	versions := e.w.Calls("get_version")
	e.clk.Advance(time.Second)
	if err := e.pass(); !errors.Is(err, errPaused) {
		t.Fatalf("a malformed identity answer: %v", err)
	}
	if e.count(logPaused) != 1 || !e.logHas(`msg="`+logPaused+`" reason=address`) || e.svc.baseAddress() != "" ||
		e.w.Calls("get_version") != versions {
		t.Fatalf("paused lines %d, base %q\n%s", e.count(logPaused), e.svc.baseAddress(), e.log)
	}
	before := e.w.Calls("get_address")
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatalf("the pass after: %v", err)
	}
	// Check ran, with its three get_address calls for account 1 (step 2: the account's base and the primary address;
	// step 5: the recorded index), and the pass made no identity read of its own at its start, only the end check's.
	if e.w.Calls("get_version") != versions+1 || e.w.Calls("get_address")-before != 4 || e.svc.baseAddress() == "" {
		t.Fatalf("version %d, get_address %d, base %q", e.w.Calls("get_version")-versions, e.w.Calls("get_address")-before, e.svc.baseAddress())
	}
	e.safety(o)
}

// A press between a mid-run wallet swap and the next pass makes the identity read of a pass before it
// creates a subaddress, so the wrong wallet's subaddress is never handed out: Address refuses with an error that
// wraps ErrUnavailable (the checkout's ?busy=1, ruling 8-T8a), nothing is created, stored or recorded, and the rail
// pauses as a pass does (one paused line, the base cleared, so the next pass re-runs Check). An issued subaddress is
// still answered from the store. A failed identity call is one of Address's call failures: no answer is
// ErrUnavailable, a JSON-RPC error a failed check (step 0); neither pauses.
func TestAddressRefusesAReplacedWallet(t *testing.T) {
	e := newWatchEnv(t, nil)
	paid := e.order(xmr)
	e.payOrder(paid, xmr)
	e.w.Mine(1)
	e.pass()
	unpaid := e.order(xmr)
	e.addr(unpaid) // issued, not paid yet
	if !e.view(paid).Paid {
		t.Fatalf("setup: %+v", e.view(paid))
	}
	kept := e.sub(unpaid)
	wrong := monerotest.New(8)
	for range 5 { // above SatBooth's record: its next index would pass Address's index check
		wrong.CreateAddress(bg, 0, "")
	}
	wrong.Mine(100)
	e.svc.rpc = &override{RPC: wrong}
	o := e.order(xmr)
	a, err := e.svc.Address(bg, o.ID)
	if !errors.Is(err, ErrUnavailable) || a.Address != "" {
		t.Fatalf("Address on a replaced wallet = %+v, %v", a, err)
	}
	if _, err := e.st.SubaddressForOrder(bg, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a subaddress was recorded: %v", err)
	}
	if wrong.Calls("create_address") != 5 || wrong.Calls("store") != 0 || wrong.Calls("get_address") != 1 {
		t.Fatalf("create_address %d, store %d, get_address %d", wrong.Calls("create_address")-5, wrong.Calls("store"), wrong.Calls("get_address"))
	}
	if e.count(logPaused) != 1 || !e.logHas(`msg="`+logPaused+`" reason=address`) || e.svc.baseAddress() != "" ||
		e.svc.watchable.Load() || e.count(logBadSubaddress) != 0 {
		t.Fatalf("paused lines %d, base %q\n%s", e.count(logPaused), e.svc.baseAddress(), e.log)
	}
	if got, err := e.svc.Address(bg, unpaid.ID); err != nil || got != kept {
		t.Fatalf("the issued subaddress: %+v, %v", got, err)
	}
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrUnavailable) || wrong.Calls("get_address") != 1 || e.count(logPaused) != 1 {
		t.Fatalf("a second press: %v, get_address %d", err, wrong.Calls("get_address"))
	}
	e.clk.Advance(time.Second)
	if err := e.pass(); !errors.Is(err, errPaused) {
		t.Fatalf("the next pass: %v", err)
	}
	e.svc.rpc = e.rpc
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatalf("watching did not resume: %v", err)
	}
	if got, err := e.svc.Address(bg, o.ID); err != nil || got.Index != kept.Index+1 || got.Address != e.w.SubaddressAt(uint32(kept.Index+1)) {
		t.Fatalf("the press on the merchant's wallet: %+v, %v", got, err)
	}
	if !e.view(paid).Paid || e.eventCount(paid, store.EventReverted) != 0 {
		t.Fatalf("after: %+v %+v", e.view(paid), e.events(paid))
	}

	// A failed identity call: no answer, then a JSON-RPC error. Neither pauses or records anything.
	o2 := e.order(xmr)
	e.w.Fail("get_address", errOutage)
	if _, err := e.svc.Address(bg, o2.ID); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrSubaddress) {
		t.Fatalf("Address with get_address down: %v", err)
	}
	e.w.Fail("get_address", &xmrrpc.Error{Code: -1, Method: "get_address"})
	if _, err := e.svc.Address(bg, o2.ID); !errors.Is(err, ErrSubaddress) || !e.logHas(`msg="`+logBadSubaddress+`" step=0`) {
		t.Fatalf("Address with a get_address error: %v\n%s", err, e.log)
	}
	if _, err := e.st.SubaddressForOrder(bg, o2.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("recorded after a failed identity call: %v", err)
	}
	if e.count(logPaused) != 2 || e.svc.baseAddress() == "" {
		t.Fatalf("a failed call paused: %d lines, base %q", e.count(logPaused), e.svc.baseAddress())
	}
	if got, err := e.svc.Address(bg, o2.ID); err != nil || got.Index <= kept.Index+1 {
		t.Fatalf("after the failures: %+v, %v", got, err)
	}
	e.safety(paid, unpaid, o, o2)
}

// The merchant's wallet opened with its spend key (ErrNotViewOnly): passes are paused the same way; a payment mined
// meanwhile is recorded once the view-only wallet is back.
func TestPausedWhileNotViewOnly(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.pass()
	e.w.Full(true)
	e.svc.setBase("")
	before := e.pausedState(o)
	e.w.Mine(1)
	e.passes(2)
	if got := e.pausedState(o); got != before || e.count(logPaused) != 2 || !e.logHas(`reason=view-only`) {
		t.Fatalf("paused: %s\n%s", got, e.log)
	}
	e.w.Full(false)
	e.pass()
	if m, _ := e.mark(o, txid); m.Kind != store.MarkMined || !e.view(o).Paid {
		t.Fatalf("after resuming: %+v %+v", m, e.view(o))
	}
	e.safety(o)
}

// Behind on subaddresses (ErrSubaddress: the merchant's wallet, its record differing) still watches: the pass runs
// on with the check-failed line, and a payment is paid; Address refuses while behind.
func TestBehindStillWatches(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	a := e.addr(o)
	m := e.sub(o).Index
	e.rpc.set(func(r *override) {
		r.getAddr = func(account uint32, indices []uint32, got xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
			if err == nil && len(indices) == 1 && int64(indices[0]) == m {
				got.Addresses[0].Address = e.w.SubaddressAt(uint32(m) + 50)
			}
			return got, err
		}
	})
	e.svc.behind.Store(true)
	e.w.Pay(a, xmr)
	e.w.Mine(1)
	if err := e.pass(); err != nil {
		t.Fatalf("a behind pass did not run: %v", err)
	}
	if !e.view(o).Paid || e.count(logPaused) != 0 || !e.logHas(`msg="`+logCheckFailed+`" reason=subaddress`) || !e.svc.behind.Load() {
		t.Fatalf("%+v\n%s", e.view(o), e.log)
	}
	if _, err := e.svc.Address(bg, e.order(xmr).ID); !errors.Is(err, ErrIndexBehind) {
		t.Fatalf("Address while behind: %v", err)
	}
	e.safety(o)
}

// Ruling R6: the sender-lock rule is judged against the wallet's height when the payment
// is first seen, never the cursor's. The cursor at 200 and the wallet at 190 (on a shorter fork, or rescanning): a
// payment with unlock_time 210 is locked 20 blocks past the wallet's height, beyond the normal 10, so it is late. Judged
// against the cursor (210 > 200 + 10 is false) it counted, and could pay the order.
func TestLockJudgedAtTheWalletHeight(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(int(200 - e.w.Height()))
	o := e.order(xmr)
	a := e.addr(o)
	e.pass()
	if c := e.cursor(); c.Height != 200 || c.WalletHeight != 200 {
		t.Fatalf("setup: cursor %+v", c)
	}
	e.w.SetHeight(190)
	e.w.PayLocked(a, xmr, 210)
	e.clk.Advance(time.Second)
	e.pass()
	if c := e.cursor(); c.Height != 200 || c.WalletHeight != 190 {
		t.Fatalf("cursor %+v, want height 200 and wallet height 190", c)
	}
	v := e.view(o)
	if v.Late != xmr || v.Received != 0 || v.Paid ||
		e.count("ALERT: a Monero payment is locked by its sender beyond the normal lock; it is treated as late") != 1 {
		t.Errorf("unlock_time 210 at wallet height 190: Late=%d Received=%d Paid=%v, want late", v.Late, v.Received, v.Paid)
	}
	l := e.payments(o)
	if len(l) != 1 {
		t.Fatalf("%d payments, want one", len(l))
	}
	if l[0].FirstSeenHeight != 190 {
		t.Fatalf("first seen at %d, want the wallet's height 190", l[0].FirstSeenHeight)
	}
}

// The cursor never decreases while the wallet catches up, also when a new payment is found then (its row's
// first-seen height is the wallet's, ruling R6; the cursor stays where it was).
func TestCursorNeverDecreasesWhileCatchingUp(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(20)
	o := e.order(xmr)
	e.pass()
	cur := e.cursor().Height
	e.w.SetHeight(cur - 5)
	e.pass()
	if e.cursor().Height != cur {
		t.Fatalf("cursor %d, was %d", e.cursor().Height, cur)
	}
	e.payOrder(o, xmr)
	e.clk.Advance(time.Second)
	e.pass()
	l := e.payments(o)
	if len(l) != 1 || l[0].FirstSeenHeight != cur-5 || e.cursor().Height != cur {
		t.Fatalf("a payment found while catching up: %+v, cursor %+v", l, e.cursor())
	}
	for i := 0; i < 3; i++ {
		e.clk.Advance(time.Second)
		e.pass()
		if e.cursor().Height < cur {
			t.Fatalf("the cursor went down: %+v", e.cursor())
		}
	}
	e.w.SetHeight(-1)
	e.pass()
	if e.cursor().Height != e.w.Height() {
		t.Fatalf("cursor %+v, wallet %d", e.cursor(), e.w.Height())
	}
	e.safety(o)
}

// One height ALERT per pass, also when step 0's Check runs while the wallet is below the cursor.
func TestOneHeightAlertPerPass(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(20)
	e.pass()
	e.w.SetHeight(e.cursor().Height - 3)
	for i := 1; i <= 3; i++ {
		e.svc.behind.Store(true) // step 0 re-runs Check, which logs the line too
		e.clk.Advance(time.Second)
		e.pass()
		if e.count(logHeightBehind) != i {
			t.Fatalf("pass %d: %d height lines", i, e.count(logHeightBehind))
		}
	}
	e.svc.behind.Store(false)
	e.pass()
	if e.count(logHeightBehind) != 4 {
		t.Fatalf("a pass without Check: %d lines", e.count(logHeightBehind))
	}
}

// The twin of onchain's stopped-polling ALERT: an on-time payment of an unpaid order, dead past its window, is
// settled with the ALERT once; a paid order's needs no person.
func TestLeftWalletAlert(t *testing.T) {
	const line = "ALERT: an on-time Monero payment past its watch window left the wallet; SatBooth stops polling it and will notice it again only if a scan lists it mined"
	e := newWatchEnv(t, nil)
	unpaid, paid := e.order(2*xmr), e.order(xmr)
	tu := e.payOrder(unpaid, 2*xmr)
	tp := e.payOrder(paid, xmr)
	e.w.Mine(1)
	e.pass() // paid's payment recorded at 1 block; unpaid's needs 3
	e.w.Pop(1)
	e.w.MineEmpty(1) // both back in the pool at the same height: paid reversed
	e.pass()
	e.pastExpiry(unpaid)
	e.pass()
	e.toWindowEnd(unpaid)
	e.pass() // underpaid at the window end for both
	e.w.Drop(tu)
	e.w.Drop(tp)
	e.passes(3)
	if e.count(line) != 2 || e.watched(unpaid, tu) || e.watched(paid, tp) {
		t.Fatalf("%d lines, watched %v %v\n%s", e.count(line), e.watched(unpaid, tu), e.watched(paid, tp), e.log)
	}
	e.safety(unpaid, paid)

	// A paid order: its dead payment settles without the ALERT.
	e2 := newWatchEnv(t, nil)
	o := e2.order(xmr)
	a, b := e2.addr(o), ""
	e2.w.Pay(a, xmr)
	b = e2.w.Pay(a, xmr/10)
	e2.w.Mine(1)
	e2.pass() // paid by the first; the second recorded too
	e2.w.Pop(1)
	e2.w.Drop(b)
	e2.w.Mine(1) // the first mined again, the second gone
	e2.toWindowEnd(o)
	e2.passes(3)
	if e2.count(line) != 0 || !e2.view(o).Paid || e2.watched(o, b) {
		t.Fatalf("a paid order: %d lines, %+v\n%s", e2.count(line), e2.view(o), e2.log)
	}
	e2.safety(o)
}

// A reorg that replaces the payment's block at the same height (the wallet never below the cursor) puts it back
// in the pool: reversed with order.reverted 1, no height ALERT, the order not stale; mined again, paid_again 1.
func TestReorgToThePoolAtTheSameHeight(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Mine(1)
	e.pass()
	h := e.w.Height()
	e.w.Pop(1)
	e.w.MineEmpty(1)
	if e.w.Height() != h {
		t.Fatal("setup: the height changed")
	}
	e.pass()
	if m, _ := e.mark(o, txid); m.Kind != store.MarkMempool {
		t.Fatalf("mark %+v", m)
	}
	if _, ok := e.rev(o, txid, 0); !ok || !e.hasEvent(o, store.EventReverted, 1) || e.view(o).Paid || e.view(o).Stale ||
		e.count(logHeightBehind) != 0 {
		t.Fatalf("%+v %+v\n%s", e.view(o), e.events(o), e.log)
	}
	e.w.Mine(1)
	e.pass()
	if !e.hasEvent(o, store.EventPaidAgain, 1) || !e.view(o).Paid || e.count(rail.LogPaidAgainMonero) != 1 {
		t.Fatalf("%+v\n%s", e.events(o), e.log)
	}
	e.safety(o)
}

// The paid_again ALERT names the rail whose money paid again: a Monero reversal answered by the on-chain peer
// reading paid logs the on-chain line, from the Monero settle that adds the event.
func TestPaidAgainNamesTheRail(t *testing.T) {
	e := newWatchEnv(t, nil)
	p := &livePeer{}
	e.svc.SetPeer(p, nil)
	o := e.mixed(0, 50_000, xmr)
	e.payOrder(o, xmr)
	p.set(rail.View{})
	e.w.Mine(1)
	e.pass()
	e.w.Pop(1)
	e.w.MineEmpty(1)
	e.pass()
	if !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("setup: %+v", e.events(o))
	}
	p.set(rail.View{Received: 50_000, Confirmed: 50_000, Paid: true})
	e.pass()
	if !e.hasEvent(o, store.EventPaidAgain, 1) || e.count(rail.LogPaidAgainOnchain) != 1 || e.count(rail.LogPaidAgainMonero) != 0 {
		t.Fatalf("%+v\n%s", e.events(o), e.log)
	}
	e.safety(o)
}

// The twin of onchain's TestOrderLeavingTheWindowWithoutAFinalEventIsAlerted: an order whose late payment keeps
// failing is skipped in every pass, so it never gets a final event; when it leaves its window it is reported, and no
// final event is invented then (the window-end settle of ruling 8-T7d is only for an order a stale peer held).
func TestLeftWindowWithAFailingPaymentIsReported(t *testing.T) {
	const noFinal = "ALERT: a Monero order left its watch window without a final event; check it by hand"
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	a := e.addr(o)
	e.pass()
	e.pastExpiry(o)
	txid := e.w.Pay(a, xmr)
	e.w.FailTx(txid, &xmrrpc.Error{Code: -1, Method: "get_transfer_by_txid"})
	e.pass() // the late payment is discovered, and its order skipped
	e.passes(2)
	if len(e.events(o)) != 0 {
		t.Fatalf("setup: %+v", e.events(o))
	}
	e.toWindowEnd(o)
	e.pass()
	e.passes(2)
	if e.count(noFinal) != 1 || len(e.events(o)) != 0 {
		t.Fatalf("events %+v\n%s", e.events(o), e.log)
	}
	e.safety(o)
}

// Ruling 8-A2: a wallet whose chain gets shorter than the cursor (wallet-rpc moved to a monerod on
// another fork). The last 3 blocks of the chain are replaced by 1, and a new 2 XMR payment (tier: 3 confirmations) is
// mined in it, so wallet-rpc's own count is 1 (get_height − height). Counted from the cursor it read 3: recorded,
// paid. Confirmations are counted from the wallet's height the pass read, never from the cursor.
func TestConfirmationsFromWalletHeightShorterChain(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(30)
	o := e.order(2 * xmr)
	a := e.addr(o)
	e.pass()
	if h0 := e.cursor().Height; h0 != e.w.Height() {
		t.Fatalf("setup: cursor %d, wallet %d", h0, e.w.Height())
	}
	e.w.PopDrop(3) // the replacement chain is shorter
	txid := e.w.Pay(a, 2*xmr)
	e.w.Mine(1)
	wh, _ := e.w.GetHeight(bg)
	e.clk.Advance(time.Second)
	e.pass()
	m, ok := e.mark(o, txid)
	if !ok || m.Kind != store.MarkMined {
		t.Fatalf("mark %+v %v", m, ok)
	}
	if c := e.cursor(); c.WalletHeight != wh || c.Height <= wh {
		t.Fatalf("cursor %+v, wallet height %d", c, wh)
	}
	if _, ok := e.fwd(o, txid, 0); ok {
		t.Errorf("the payment was recorded with %d real confirmation of the 3 its tier needs", wh-m.Height)
	}
	v := e.view(o)
	if v.Paid || v.Confirmations != int(wh-m.Height) || e.eventCount(o, store.EventPaid) != 0 {
		t.Errorf("the order reads %+v with %d order.paid; want unpaid at wallet-rpc's %d confirmations", v,
			e.eventCount(o, store.EventPaid), wh-m.Height)
	}
	// The wallet's chain grows to the tier: recorded at wallet-rpc's third confirmation, not before.
	e.w.Mine(1)
	e.passes(1)
	if _, ok := e.fwd(o, txid, 0); ok {
		t.Fatal("recorded at 2 confirmations")
	}
	e.w.Mine(1)
	e.passes(1)
	if _, ok := e.fwd(o, txid, 0); !ok || !e.view(o).Paid {
		t.Fatalf("not recorded at 3 confirmations: %+v", e.view(o))
	}
}

// Ruling 8-A2, permanent settlement: the same with a replacement 10 blocks shallower (the settled depth). Counted
// from the cursor the payment was stored as settled in its first pass and never polled again, so when the longer
// chain won back and the payment left it, nothing noticed. Counted from the wallet's height it stays tracked and is
// reversed.
func TestSettledFromWalletHeightShorterChain(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(30)
	o := e.order(2 * xmr)
	a := e.addr(o)
	e.pass()
	e.w.PopDrop(10)
	txid := e.w.Pay(a, 2*xmr)
	e.w.Mine(1)
	e.clk.Advance(time.Second)
	e.pass()
	if !e.watched(o, txid) {
		t.Errorf("a payment with 1 real confirmation was stored as settled (no longer polled)")
	}
	// The longer chain wins back: the payment is gone.
	e.w.PopDrop(1)
	e.w.Mine(15)
	e.passes(2)
	if e.view(o).Paid {
		t.Errorf("the payment left the chain, the order still reads paid; reversed %v, order.reverted %d",
			e.hasRev(o, txid), e.eventCount(o, store.EventReverted))
	}
}

// Ruling 8-A2, the restart or rescan case for contrast: the wallet is below the cursor but the chain is not (wallet-rpc
// restarted on an older wallet file, rescanning). Counted from the wallet's height, a payment it lists has wallet-rpc's
// own count, which is never above the real chain's.
func TestConfirmationsFromWalletHeightWhileRescanning(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(30)
	o := e.order(2 * xmr)
	txid := e.payOrder(o, 2*xmr)
	e.pass() // in the pool
	e.w.Mine(2)
	e.pass() // mined, 2 confirmations of 3: not recorded
	cur := e.cursor().Height
	mk, _ := e.mark(o, txid)
	e.w.SetHeight(cur - 1)
	e.w.ForgetPayments(cur - 1)
	e.w.Mine(5)
	e.clk.Advance(time.Second)
	e.pass()
	wh, real := cur-1, e.w.Height()
	c := e.cursor()
	if c.Height != cur || c.WalletHeight != wh {
		t.Fatalf("cursor %+v, want height %d and wallet height %d", c, cur, wh)
	}
	v := e.view(o)
	if int64(v.Confirmations) != wh-mk.Height || int64(v.Confirmations) > real-mk.Height {
		t.Errorf("confirmations %d; wallet-rpc's count %d, the real chain's %d", v.Confirmations, wh-mk.Height, real-mk.Height)
	}
	if _, ok := e.fwd(o, txid, 0); ok || v.Paid {
		t.Errorf("recorded while rescanning at %d confirmations: %+v", v.Confirmations, v)
	}
}

func (e *env) hasRev(o store.Order, txid string) bool { _, ok := e.rev(o, txid, 0); return ok }

// Ruling 8-A4: the listing that first carries an issued subaddress's payment drops it as unreadable
// (what xmrrpc does for an entry that fails its shape checks: it is left out and Dropped counts it), after an outage
// of 21 blocks, so the next listings start above the payment. The cursor still advances (holding it would let one bad
// entry grow every listing without bound), so the payment is not found again: the ALERT names the listing's height
// range, which holds the payment's height, to reconcile by hand. The line carries no wallet-rpc text.
func TestDroppedEntryAlertNamesTheRange(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(5)
	o := e.order(xmr)
	a := e.addr(o)
	e.pass()
	txid := e.w.Pay(a, xmr)
	e.w.Mine(1)
	txHeight := e.w.Height() - 1
	e.w.Mine(20) // SatBooth sees nothing meanwhile (an outage)
	in := e.wrap()
	in.set(func(i *inject) {
		i.list = func(t *xmrrpc.Transfers) {
			for j, x := range t.In {
				if x.TxID == txid {
					t.In = append(t.In[:j], t.In[j+1:]...)
					t.Dropped++
					break
				}
			}
		}
	})
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	in.set(func(i *inject) { i.list = nil }) // wallet-rpc answers correctly from now on
	from, to := in.queries[len(in.queries)-1].MinHeight, e.cursor().Height
	if to != e.w.Height() || from > txHeight || to < txHeight {
		t.Fatalf("setup: range %d to %d, payment at %d, wallet %d", from, to, txHeight, e.w.Height())
	}
	want := fmt.Sprintf(`level=ERROR msg="%s" count=1 from_height=%d to_height=%d`, logDropped, from, to)
	if e.count(logDropped) != 1 || !e.logHas(want) {
		t.Fatalf("want one line %q:\n%s", want, e.log)
	}
	if !strings.HasPrefix(logDropped, "ALERT: ") {
		t.Fatalf("not an ALERT: %q", logDropped)
	}
	e.passes(3)
	e.restart() // Check
	e.w.Mine(1)
	e.passes(3)
	if n := e.count(logDropped); n != 1 {
		t.Fatalf("%d dropped lines after the listings came right", n)
	}
	// The documented limit (docs/rules-monero.md, "Not detected"): the payment is not found again; the ALERT is the
	// operator's cue.
	if l := e.payments(o); len(l) != 0 || e.view(o).Paid {
		t.Fatalf("payments %+v, view %+v", l, e.view(o))
	}
}

// Ruling T1: an order skipped in a pass (a sibling payment's call failed) still gets a
// pending row when the pass committed a new or changed mark for it; it is not settled in that pass. An expired order
// (order.expired) gets two late payments in the last pass of its window: A is observed mined and recordable, B's call
// fails. The next pass is past the window, where neither is polled: the pending row still brings the order back, and
// A's late forward and order.late are written.
func TestSkippedOrderWithACommittedMarkStaysPending(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	a := e.addr(o)
	e.pass()
	e.pastExpiry(o)
	e.passes(1)
	if !e.hasEvent(o, store.EventExpired, 1) {
		t.Fatalf("setup: no order.expired: %+v", e.events(o))
	}
	e.clk.Advance(o.ExpiresAt.Add(store.WatchWindow).Sub(e.clk.Now()) - 30*time.Second)
	payA := e.w.Pay(a, xmr/2)
	payB := e.w.Pay(a, xmr/2)
	e.w.Mine(1)
	e.w.FailTx(payB, &xmrrpc.Error{Code: -1, Method: "get_transfer_by_txid"})
	if err := e.pass(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if m, ok := e.mark(o, payA); !ok || m.Kind != store.MarkMined {
		t.Fatalf("setup: A's mark %+v %v", m, ok)
	}
	if _, ok := e.fwd(o, payA, 0); ok {
		t.Fatal("setup: the skipped order was settled in the pass its sibling failed")
	}
	e.toWindowEnd(o)
	e.passes(1)
	f, ok := e.fwd(o, payA, 0)
	if !ok || f.Code != codeLate || !e.hasEvent(o, store.EventLate, 1) {
		t.Fatalf("A was never settled: forward %+v %v, events %+v", f, ok, e.events(o))
	}
	e.passes(2)
	if e.eventCount(o, store.EventLate) != 1 {
		t.Fatalf("events %+v", e.events(o))
	}
	if pending, err := e.st.MoneroPendingSettle(bg); err != nil || len(pending) != 0 {
		t.Fatalf("pending %+v %v", pending, err)
	}
	e.safety(o)
}

// A payment that leaves the watch window without one successful
// observation in it (its call failed on every pass, so it has no mark) is no longer polled, and nothing records it.
// One ALERT names its order, even when the order already has its final event, so a person reconciles it from the
// wallet; once per payment per process. A sibling observed in the window gives no line.
func TestUnobservedPaymentLeavingTheWindowIsAlerted(t *testing.T) {
	const unobserved = "ALERT: a Monero payment left its watch window without one successful observation; reconcile it from the wallet"
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	a := e.addr(o)
	e.pass()
	e.pastExpiry(o)
	e.passes(1)
	if !e.hasEvent(o, store.EventExpired, 1) {
		t.Fatalf("setup: no order.expired: %+v", e.events(o))
	}
	e.clk.Advance(time.Second)
	e.w.Pay(a, xmr/2) // observed
	failing := e.w.Pay(a, xmr/2)
	e.w.FailTx(failing, &xmrrpc.Error{Code: -1, Method: "get_transfer_by_txid"})
	e.w.Mine(1)
	e.passes(3)
	if e.count(unobserved) != 0 {
		t.Fatalf("alerted inside the window\n%s", e.log)
	}
	e.toWindowEnd(o)
	e.pass()
	e.passes(2)
	if e.count(unobserved) != 1 || !e.logHas(`msg="`+unobserved+`" order=`+o.ID) {
		t.Fatalf("want one line naming the order\n%s", e.log)
	}
}

// The ALERT of unobserved payments is bounded per pass as the other per-payment ALERTs: maxAlertLines lines, then one
// summary with the count of the rest.
func TestUnobservedPaymentsAlertIsCapped(t *testing.T) {
	const (
		unobserved = "ALERT: a Monero payment left its watch window without one successful observation; reconcile it from the wallet"
		more       = "ALERT: more Monero payments left their watch window without one successful observation; reconcile them from the wallet"
	)
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	a := e.addr(o)
	e.pass()
	e.pastExpiry(o)
	e.passes(1)
	e.clk.Advance(time.Second)
	for range maxAlertLines + 2 {
		e.w.FailTx(e.w.Pay(a, xmr/100), &xmrrpc.Error{Code: -1, Method: "get_transfer_by_txid"})
	}
	e.w.Mine(1)
	e.passes(1)
	e.toWindowEnd(o)
	e.pass()
	e.passes(2)
	if e.count(unobserved) != maxAlertLines || e.count(more) != 1 || !e.logHas(`msg="`+more+`" count=2`) {
		t.Fatalf("%d lines, %d summaries\n%s", e.count(unobserved), e.count(more), e.log)
	}
}
