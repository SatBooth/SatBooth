package onchain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

// --- the watcher's test harness ---

// newWatchEnv is a checked env whose passes do not wait on the pacing (PollRate at its maximum).
func newWatchEnv(t *testing.T) *env {
	t.Helper()
	return newWatchEnvTiers(t, nil)
}

// newWatchEnvTiers is newWatchEnv with the given tiers (nil: the default). A restart keeps them (e.mod).
func newWatchEnvTiers(t *testing.T, tiers []Tier) *env {
	t.Helper()
	e := newEnv(t)
	e.mod = func(c *Config) { c.PollRate, c.Tiers = MaxPollRate, tiers }
	e.svc = e.newService()
	e.ready()
	return e
}

// zeroTiers has a 0 tier: an order of up to 10,000 sat is paid from the mempool.
var zeroTiers = []Tier{{UpTo: 10_000, Confirmations: 0}, {UpTo: 1_000_000, Confirmations: 1}, {UpTo: 0, Confirmations: 6}}

// newZeroConfEnv is a watch env on zeroTiers.
func newZeroConfEnv(t *testing.T) *env {
	t.Helper()
	return newWatchEnvTiers(t, zeroTiers)
}

// restart is a new Service on the same files and the same node, checked: what a process restart leaves.
func (e *env) restart() {
	e.t.Helper()
	e.svc = e.newService()
	e.ready()
}

// pass runs one pass of the watcher.
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

// mixed makes an order with both rails.
func (e *env) mixed(msat, sat int64) store.Order {
	e.t.Helper()
	o, _, err := e.st.CreateOrder(bg, store.Order{AccountID: "acct-1", MerchantRef: fmt.Sprintf("ref-%d", orderSeq.Add(1)), AmountMsat: msat,
		AmountSat: sat, ExpiresAt: e.clk.Now().Add(time.Hour)})
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

// addr issues the order's address through Address.
func (e *env) addr(o store.Order) string {
	e.t.Helper()
	a, err := e.svc.Address(bg, o.ID)
	if err != nil {
		e.t.Fatalf("Address: %v", err)
	}
	return a.Address
}

// payOrder issues the order's address and pays sat to it.
func (e *env) payOrder(o store.Order, sat int64) string {
	e.t.Helper()
	return e.node.Pay(e.addr(o), sat)
}

func (e *env) rail(o store.Order) Rail {
	e.t.Helper()
	r, err := e.svc.Rail(bg, o)
	if err != nil {
		e.t.Fatalf("Rail: %v", err)
	}
	return r
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

func (e *env) lookup(id ledger.U128) (ledger.Transfer, bool) {
	e.t.Helper()
	tr, ok, err := e.lg.LookupTransfer(bg, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return tr, ok
}

func (e *env) fwd(txid string, ep uint32) (ledger.Transfer, bool) {
	return e.lookup(PaidTransferID(txid, 0, ep))
}

func (e *env) rev(txid string, ep uint32) (ledger.Transfer, bool) {
	return e.lookup(ReversalTransferID(txid, 0, ep))
}

func (e *env) mark(txid string) (store.Mark, bool) {
	e.t.Helper()
	m, ok, err := e.st.CurrentMark(bg, store.TXOKey{TxID: txid})
	if err != nil {
		e.t.Fatal(err)
	}
	return m, ok
}

func (e *env) txos(o store.Order) []store.TXO {
	e.t.Helper()
	l, err := e.st.TXOsForOrder(bg, o.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

// lnAccounts creates the Lightning pair in the ledger, so a test can write a Lightning payment directly.
func (e *env) lnAccounts() {
	e.t.Helper()
	for _, a := range []ledger.Account{
		{ID: lnpay.LightningHub, Ledger: 1, Code: 1, Flags: ledger.AccountCreditsMustNotExceedDebits},
		{ID: lnpay.MerchantLightning, Ledger: 1, Code: 2, Flags: ledger.AccountDebitsMustNotExceedCredits},
	} {
		if r, err := e.lg.CreateAccount(bg, a); err != nil || (r != ledger.AccountCreated && r != ledger.AccountExists) {
			e.t.Fatal(r, err)
		}
	}
}

// lnPaid records a Lightning payment of the order in the ledger, as lnpay does, and makes the stub answer paid.
func (e *env) lnPaid(o store.Order, hash string) {
	e.t.Helper()
	e.lnAccounts()
	ud, _ := orderUserData(o.ID)
	r, err := e.lg.CreateTransfer(bg, ledger.Transfer{ID: lnpay.PaidTransferID(hash), DebitAccountID: lnpay.LightningHub,
		CreditAccountID: lnpay.MerchantLightning, Amount: ledger.U128From64(uint64(o.AmountMsat)), UserData128: ud, Ledger: 1, Code: 1})
	if err != nil || r != ledger.TransferCreated {
		e.t.Fatal(r, err)
	}
	e.ln.set(true, false)
}

func amountOf(tr ledger.Transfer) int64 { return int64(tr.Amount.Lo) }

// safety is the invariant every test ends with, computed from the rows and independently of the code under test:
//   - each output's transfers: a reversal only with its forward, a later epoch only after a reversal, the amounts the
//     output's, the forward's code 2 when counted and 4 when late, the reversal's 3;
//   - an unreversed forward only on a mined output, or, for a counted output of an order whose tier needs 0
//     confirmations, on one whose mark is mempool (a late forward only on a mined output, Ruling 6.1-10); and every
//     mined output at its tier recorded (a late one only inside the window: past it, it is not watched);
//   - the ledger balances: both accounts move by the sum of the unreversed forwards;
//   - order.paid at most once, the order.reverted seqs 1..n, the paid_again seqs a subset, each once;
//   - Rail.ConfirmedSat is the sum of the unreversed code-2 forwards, Paid follows from it, and a paid order has
//     order.paid.
func (e *env) safety(orders ...store.Order) {
	e.t.Helper()
	var total int64
	cur, _, _ := e.st.Cursor(bg)
	for _, o := range orders {
		var confirmed int64
		for _, x := range e.txos(o) {
			late := !x.FirstSeen.Before(o.ExpiresAt)
			var ep uint32
			unreversed := false
			for ; ep < 100; ep++ {
				f, okF := e.lookup(PaidTransferID(x.TxID, x.Vout, ep))
				r, okR := e.lookup(ReversalTransferID(x.TxID, x.Vout, ep))
				if !okF {
					if okR {
						e.t.Errorf("%s:%d epoch %d: a reversal without its forward", x.TxID[:8], x.Vout, ep)
					}
					if n, ok := e.lookup(PaidTransferID(x.TxID, x.Vout, ep+1)); ok {
						e.t.Errorf("%s:%d: a forward at epoch %d after a gap: %+v", x.TxID[:8], x.Vout, ep+1, n)
					}
					break
				}
				wantCode := uint16(codePaid)
				if late {
					wantCode = codeLate
				}
				if f.Code != wantCode || amountOf(f) != x.AmountSat || f.DebitAccountID != OnchainWallet || f.CreditAccountID != MerchantOnchain {
					e.t.Errorf("%s:%d epoch %d: forward %+v (late %v)", x.TxID[:8], x.Vout, ep, f, late)
				}
				if !okR {
					unreversed = true
					if _, ok := e.lookup(PaidTransferID(x.TxID, x.Vout, ep+1)); ok {
						e.t.Errorf("%s:%d: epoch %d is unreversed but epoch %d exists", x.TxID[:8], x.Vout, ep, ep+1)
					}
					if f.Code == codePaid {
						confirmed += x.AmountSat
					}
					total += x.AmountSat
					break
				}
				if r.Code != codeReversed || amountOf(r) != x.AmountSat || r.DebitAccountID != MerchantOnchain || r.CreditAccountID != OnchainWallet {
					e.t.Errorf("%s:%d epoch %d: reversal %+v", x.TxID[:8], x.Vout, ep, r)
				}
			}
			m, marked := e.mark(x.TxID)
			zeroConf := !late && Required(e.svc.cfg.Tiers, o.AmountSat) == 0
			if unreversed && (!marked || (m.Kind != store.MarkMined && !(zeroConf && m.Kind == store.MarkMempool))) {
				e.t.Errorf("%s:%d: recorded but its mark is %+v (late %v)", x.TxID[:8], x.Vout, m, late)
			}
			// Inside the window every output is watched; past it, the on-time ones still are (the stuck set).
			inWindow := e.clk.Now().Before(o.ExpiresAt.Add(store.WatchWindow))
			if (inWindow || !late) && marked && m.Kind == store.MarkMined && cur.Height-m.Height+1 >= int64(Required(e.svc.cfg.Tiers, o.AmountSat)) && !unreversed {
				e.t.Errorf("%s:%d: mined at its tier but not recorded", x.TxID[:8], x.Vout)
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
		r := e.rail(o)
		if r.ConfirmedSat != confirmed || r.Paid != (confirmed >= o.AmountSat) {
			e.t.Errorf("Rail %+v, the rows give confirmed %d of %d", r, confirmed, o.AmountSat)
		}
		if r.Paid && paid != 1 {
			e.t.Errorf("paid on-chain without order.paid")
		}
	}
	w, err := e.lg.Balance(bg, OnchainWallet)
	if err != nil {
		e.t.Fatal(err)
	}
	m, err := e.lg.Balance(bg, MerchantOnchain)
	if err != nil {
		e.t.Fatal(err)
	}
	wNet := int64(w.DebitsPosted.Lo) - int64(w.CreditsPosted.Lo)
	mNet := int64(m.CreditsPosted.Lo) - int64(m.DebitsPosted.Lo)
	if wNet != mNet || wNet != total {
		e.t.Errorf("ledger: wallet %d, merchant %d, unreversed forwards %d", wNet, mNet, total)
	}
}

var errOutage = fmt.Errorf("%w: test outage", btcrpc.ErrTransport)

// inject wraps the node and adds entries to every listsinceblock answer.
type inject struct {
	RPC
	extra   []btcrpc.WalletTx
	dropped int
}

func (i *inject) ListSinceBlock(ctx context.Context, hash string, target int) (btcrpc.SinceBlock, error) {
	sb, err := i.RPC.ListSinceBlock(ctx, hash, target)
	if err == nil {
		sb.Transactions = append(sb.Transactions, i.extra...)
		sb.Dropped += i.dropped
	}
	return sb, err
}

// --- bitcoind unreachable ---

func TestOutageOnEachMethod(t *testing.T) {
	for _, m := range []string{"getblockchaininfo", "getmempoolinfo", "listsinceblock", "gettransaction", "getmempoolentry", "listdescriptors", "getwalletinfo"} {
		t.Run(m, func(t *testing.T) {
			e := newWatchEnv(t)
			o := e.order(50_000)
			txid := e.payOrder(o, 50_000)
			e.pass() // discovered, in the mempool
			if mk, ok := e.mark(txid); !ok || mk.Kind != store.MarkMempool {
				t.Fatalf("setup: %+v", mk)
			}
			if m == "listdescriptors" || m == "getwalletinfo" {
				e.svc.behind.Store(true)
			}
			e.node.Break(m, errOutage)
			for i := 1; i <= 5; i++ {
				e.clk.Advance(e.svc.cfg.PollInterval)
				e.pass()
				if got := e.count("ALERT: bitcoind call failed"); got != i {
					t.Fatalf("pass %d: %d ALERT lines", i, got)
				}
				if mk, _ := e.mark(txid); mk.Kind != store.MarkMempool {
					t.Fatalf("pass %d: a mark was added: %+v", i, mk)
				}
				if _, ok := e.fwd(txid, 0); ok || len(e.events(o)) != 0 {
					t.Fatalf("pass %d: a transfer or an event was added", i)
				}
				if r := e.rail(o); r.Stale != (i >= 4) {
					t.Fatalf("pass %d: Stale %v", i, r.Stale)
				}
			}
			e.node.Mine(1) // mined during the outage (after its last pass, so that getmempoolentry is asked in each)
			e.node.Mend(m)
			e.clk.Advance(e.svc.cfg.PollInterval)
			e.pass()
			r := e.rail(o)
			if !r.Paid || r.Stale || e.eventCount(o, store.EventPaid) != 1 || e.count("ALERT: bitcoind call failed") != 5 {
				t.Fatalf("the sixth pass: %+v, events %+v", r, e.events(o))
			}
			if e.svc.behind.Load() {
				t.Fatal("behind not cleared by the pass's Check")
			}
			e.safety(o)
		})
	}
}

// A JSON-RPC error on one output skips that output and its order for the pass; another order is paid.
func TestOutputErrorDoesNotStopTheOthers(t *testing.T) {
	e := newWatchEnv(t)
	a, b := e.order(10_000), e.order(20_000)
	ta := e.payOrder(a, 10_000)
	e.payOrder(b, 20_000)
	e.node.FailTx(ta, &btcrpc.Error{Code: -5, Method: "gettransaction"})
	e.node.Mine(1)
	e.pass()
	e.pass()
	if !e.rail(b).Paid || e.rail(a).Paid || e.count("bitcoind call failed for an output") != 2 || e.count("ALERT: bitcoind call failed") != 0 {
		t.Fatalf("a %+v b %+v\n%s", e.rail(a), e.rail(b), e.log)
	}
	if !e.rail(a).Stale || e.rail(b).Stale { // plan 6.1: stale per order
		t.Fatalf("a per-output error: a stale %v, b stale %v", e.rail(a).Stale, e.rail(b).Stale)
	}
	if _, ok := e.mark(ta); ok {
		t.Fatal("the failing output got a mark")
	}
	e.node.MendTx(ta)
	e.pass()
	if !e.rail(a).Paid || e.rail(a).Stale {
		t.Fatal("not paid, or stale, once the output answers")
	}
	e.safety(a, b)
}

// --- plan 6.1: the re-read before a dead mark, and stale per order ---

// An output with no confirmation that is not in the mempool is marked dead only after a second read: getmempoolinfo
// again, getmempoolentry again, then gettransaction again, then gettxspendingprevout (once: nothing in the node's
// mempool spends its inputs). A live output in the mempool costs one of each of the first three and none of the last.
func TestDeadIsConfirmedByASecondRead(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(50_000)
	txid := e.payOrder(o, 50_000)
	e.pass()
	type counts struct{ info, entry, tx, spend int }
	read := func() counts {
		return counts{e.node.Calls("getmempoolinfo"), e.node.Calls("getmempoolentry"), e.node.TxCalls(txid), e.node.Calls("gettxspendingprevout")}
	}
	c := read()
	e.clk.Advance(time.Second)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkMempool {
		t.Fatalf("setup: %+v", m)
	}
	if d := read(); d != (counts{c.info + 1, c.entry + 1, c.tx + 1, c.spend}) {
		t.Fatalf("a live output: %+v then %+v", c, d)
	}
	e.node.Evict(txid)
	c = read()
	e.clk.Advance(time.Second)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkDead {
		t.Fatalf("evicted: %+v", m)
	}
	if d := read(); d != (counts{c.info + 2, c.entry + 2, c.tx + 2, c.spend + 1}) {
		t.Fatalf("the dead path: %+v then %+v, want each +2 and one gettxspendingprevout", c, d)
	}
	if e.count(logLoading) != 0 || e.count(logOutputFailed) != 0 {
		t.Fatalf("lines:\n%s", e.log)
	}
	e.safety(o)
}

// A block between the first gettransaction and the last getmempoolentry: the output left the mempool because it was
// mined, so the re-read gettransaction finds it confirmed and the mark is mined. On a 0 tier a dead mark would reverse
// the paid order.
func TestMinedBetweenTheReadsIsMined(t *testing.T) {
	e := newZeroConfEnv(t)
	o := e.order(5_000)
	txid := e.payOrder(o, 5_000)
	e.pass()
	if _, ok := e.fwd(txid, 0); !ok || !e.rail(o).Paid {
		t.Fatal("setup: not paid from the mempool")
	}
	fired := false
	e.svc.hook = func(p string) error {
		if p == "track:before-mempool" && !fired {
			fired = true
			e.node.Mine(1)
		}
		return nil
	}
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("track:before-mempool not reached")
	}
	h := e.node.Height()
	if m, _ := e.mark(txid); m.Kind != store.MarkMined || m.Height != h || m.BlockHash != e.node.BlockHash(h) {
		t.Fatalf("mark %+v, want mined at %d", m, h)
	}
	if _, ok := e.rev(txid, 0); ok {
		t.Fatal("reversed")
	}
	if r := e.rail(o); !r.Paid || e.eventCount(o, store.EventPaid) != 1 || e.eventCount(o, store.EventReverted) != 0 {
		t.Fatalf("%+v %+v", r, e.events(o))
	}
	e.svc.hook = nil
	e.passes(2)
	if _, ok := e.fwd(txid, 1); ok || len(e.events(o)) != 1 {
		t.Fatalf("recorded again: %+v", e.events(o))
	}
	e.safety(o)
}

// An output already marked dead is not read again: the re-read guards only the move into dead. It costs what a live
// output costs, one gettransaction and one getmempoolentry, and no getmempoolinfo beyond the pass's own, and no
// gettxspendingprevout.
func TestAlreadyDeadIsNotReadAgain(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(50_000)
	txid := e.payOrder(o, 50_000)
	e.pass()
	e.node.Evict(txid)
	e.clk.Advance(time.Second)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkDead {
		t.Fatalf("setup: %+v", m)
	}
	info, entry, tx := e.node.Calls("getmempoolinfo"), e.node.Calls("getmempoolentry"), e.node.TxCalls(txid)
	spend := e.node.Calls("gettxspendingprevout")
	e.clk.Advance(time.Second)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkDead || m.Seq != 2 || e.node.Calls("gettxspendingprevout") != spend {
		t.Fatalf("mark %+v, %d gettxspendingprevout calls (from %d)", m, e.node.Calls("gettxspendingprevout"), spend)
	}
	if d := [3]int{e.node.Calls("getmempoolinfo") - info, e.node.Calls("getmempoolentry") - entry, e.node.TxCalls(txid) - tx}; d != [3]int{1, 1, 1} {
		t.Fatalf("an already-dead output: getmempoolinfo, getmempoolentry, gettransaction +%v, want +1 each", d)
	}
	e.safety(o)
}

// A dead output mined while its pass reads it (here between its gettransaction and its getmempoolentry, so the pass
// that skips the re-read keeps it dead) reads mined on the next pass, and its order is paid.
func TestDeadThenMinedIsMinedOnTheNextPass(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000) // tier 1
	txid := e.payOrder(o, 10_000)
	e.pass()
	e.node.Evict(txid)
	e.clk.Advance(time.Second)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkDead {
		t.Fatalf("setup: %+v", m)
	}
	fired := false
	e.svc.hook = func(p string) error {
		if p == "track:before-mempool" && !fired {
			fired = true
			e.node.Rebroadcast(txid)
			e.node.Mine(1)
		}
		return nil
	}
	e.clk.Advance(time.Second)
	e.pass()
	if m, _ := e.mark(txid); !fired || m.Kind != store.MarkDead {
		t.Fatalf("the pass of the block: fired %v, %+v", fired, m)
	}
	e.svc.hook = nil
	e.clk.Advance(time.Second)
	e.pass()
	h := e.node.Height()
	if m, _ := e.mark(txid); m.Kind != store.MarkMined || m.Height != h || m.BlockHash != e.node.BlockHash(h) {
		t.Fatalf("the next pass: %+v, want mined at %d", m, h)
	}
	if r := e.rail(o); !r.Paid || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("%+v %+v", r, e.events(o))
	}
	e.safety(o)
}

// mineAfterEntry wraps the node: after the nth getmempoolentry for txid it answers as the node did, then puts the
// transaction back in the mempool and mines a block.
type mineAfterEntry struct {
	RPC
	node *onchaintest.Node
	txid string
	n    int
	seen int
}

func (w *mineAfterEntry) InMempool(ctx context.Context, txid string) (bool, error) {
	in, err := w.RPC.InMempool(ctx, txid)
	if txid == w.txid {
		if w.seen++; w.seen == w.n {
			w.node.Rebroadcast(txid)
			w.node.Mine(1)
		}
	}
	return in, err
}

// The re-read's gettransaction comes after its getmempoolentry: a block right after the second getmempoolentry (which
// misses the transaction) is seen by the second gettransaction, and the mark is mined, not dead.
func TestReReadGettransactionFollowsTheEntry(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	txid := e.payOrder(o, 10_000)
	e.pass()
	e.node.Evict(txid)
	w := &mineAfterEntry{RPC: e.node, node: e.node, txid: txid, n: 2}
	e.svc.rpc = w
	e.clk.Advance(time.Second)
	e.pass()
	if w.seen != 2 {
		t.Fatalf("%d getmempoolentry calls, want 2", w.seen)
	}
	h := e.node.Height()
	if m, _ := e.mark(txid); m.Kind != store.MarkMined || m.Height != h {
		t.Fatalf("mark %+v, want mined at %d", m, h)
	}
	e.svc.rpc = e.node
	e.clk.Advance(time.Second)
	e.pass() // the block came after this pass's cursor: the next pass counts its confirmation
	if !e.rail(o).Paid || e.eventCount(o, store.EventReverted) != 0 {
		t.Fatalf("%+v %+v", e.rail(o), e.events(o))
	}
	e.safety(o)
}

// A failure of the re-read's getmempoolinfo is step 3's rpcFailure: a JSON-RPC error skips the output (its line names
// the method, its order reads stale), a transport error ends the pass. Neither marks it dead.
func TestReReadMempoolInfoFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"rpc error", &btcrpc.Error{Code: -32603, Method: "getmempoolinfo"}},
		{"transport", errOutage},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newWatchEnv(t)
			o := e.order(50_000)
			txid := e.payOrder(o, 50_000)
			e.pass()
			e.node.Evict(txid)
			e.svc.hook = func(p string) error {
				if p == "track:before-mempool" {
					e.node.Fail("getmempoolinfo", c.err) // the next call: the re-read
				}
				return nil
			}
			e.clk.Advance(time.Second)
			err := e.pass()
			if m, _ := e.mark(txid); m.Kind != store.MarkMempool {
				t.Fatalf("marked %+v", m)
			}
			if c.err == errOutage {
				var end passEnd
				if !errors.As(err, &end) || end.method != "getmempoolinfo" ||
					!e.logHas(`msg="`+logCallFailed+`" method=getmempoolinfo`) {
					t.Fatalf("pass %v\n%s", err, e.log)
				}
				return
			}
			if err != nil || !e.logHas(`msg="`+logOutputFailed+`" method=getmempoolinfo code=-32603`) || e.count(logCallFailed) != 0 {
				t.Fatalf("pass %v\n%s", err, e.log)
			}
			if !e.rail(o).Stale {
				t.Fatal("the skipped order does not read stale")
			}
			e.svc.hook = nil
			e.clk.Advance(time.Second)
			e.pass()
			if m, _ := e.mark(txid); m.Kind != store.MarkDead || e.rail(o).Stale {
				t.Fatalf("the next pass: %+v", m)
			}
			e.safety(o)
		})
	}
}

// An order with an output whose call failed reads stale for that pass; the other order does not. A pass that ends early
// learns nothing, so it leaves the set as it was; the next pass that reaches its end clears it.
func TestSkippedOrderIsStale(t *testing.T) {
	e := newWatchEnv(t)
	a, b := e.order(10_000), e.order(20_000)
	ta := e.payOrder(a, 10_000)
	e.payOrder(b, 20_000)
	e.pass()
	if e.rail(a).Stale || e.rail(b).Stale {
		t.Fatal("setup: stale")
	}
	e.node.FailTx(ta, &btcrpc.Error{Code: -5, Method: "gettransaction"})
	e.clk.Advance(e.svc.cfg.PollInterval)
	e.pass()
	if !e.rail(a).Stale || e.rail(b).Stale {
		t.Fatalf("a stale %v, b stale %v", e.rail(a).Stale, e.rail(b).Stale)
	}
	e.node.MendTx(ta)
	e.node.Break("getblockchaininfo", errOutage)
	e.clk.Advance(e.svc.cfg.PollInterval)
	if err := e.pass(); err == nil {
		t.Fatal("the pass did not end early")
	}
	if !e.rail(a).Stale || e.rail(b).Stale {
		t.Fatalf("after a pass that ended early: a stale %v, b stale %v", e.rail(a).Stale, e.rail(b).Stale)
	}
	e.node.Mend("getblockchaininfo")
	e.clk.Advance(e.svc.cfg.PollInterval)
	e.pass()
	if e.rail(a).Stale || e.rail(b).Stale {
		t.Fatalf("mended: a stale %v, b stale %v", e.rail(a).Stale, e.rail(b).Stale)
	}
	e.safety(a, b)
}

// A store query failure in step 4 still spoils the whole pass: lastGoodPass stays where it was, so every order reads
// stale once 3 PollIntervals have passed; a clean pass stamps it again.
func TestStoreFailureStillSpoilsThePass(t *testing.T) {
	e := newWatchEnv(t)
	a, b := e.order(10_000), e.order(20_000)
	e.payOrder(a, 10_000)
	e.payOrder(b, 20_000)
	e.pass()
	good := e.svc.lastGoodPass.Load()
	if good == 0 {
		t.Fatal("no good pass")
	}
	e.svc.pastExpiry = func(context.Context, time.Time) ([]store.Order, error) { return nil, errors.New("store failed") }
	for i := 1; i <= 4; i++ {
		e.clk.Advance(e.svc.cfg.PollInterval)
		if err := e.pass(); err != nil {
			t.Fatalf("pass %d ended early: %v", i, err)
		}
		if e.svc.lastGoodPass.Load() != good {
			t.Fatalf("pass %d was stamped good", i)
		}
	}
	if e.count(logSettleFailed) != 4 {
		t.Fatalf("%d settle-failed lines:\n%s", e.count(logSettleFailed), e.log)
	}
	if !e.rail(a).Stale || !e.rail(b).Stale {
		t.Fatal("not every order stale")
	}
	e.svc.pastExpiry = e.st.OnchainOrdersPastExpiry
	e.clk.Advance(e.svc.cfg.PollInterval)
	e.pass()
	if e.svc.lastGoodPass.Load() == good || e.rail(a).Stale || e.rail(b).Stale {
		t.Fatal("a clean pass was not stamped good")
	}
	e.safety(a, b)
}

// A bad answer for one output (an amount that differs from the stored row, a missing vout) is skipped with an ALERT.
type badTx struct {
	RPC
	txid string
	f    func(*btcrpc.Transaction)
}

func (b *badTx) GetTransaction(ctx context.Context, txid string) (btcrpc.Transaction, error) {
	tx, err := b.RPC.GetTransaction(ctx, txid)
	if err == nil && txid == b.txid {
		b.f(&tx)
	}
	return tx, err
}

func TestBadTransactionIsSkipped(t *testing.T) {
	for name, f := range map[string]func(*btcrpc.Transaction){
		"amount":  func(tx *btcrpc.Transaction) { tx.Outputs[0].ValueSat++ },
		"address": func(tx *btcrpc.Transaction) { tx.Outputs[0].Address = "bcrt1qother000000000000000000000000000000" },
		"no vout": func(tx *btcrpc.Transaction) { tx.Outputs[0].Vout = 1 },
		"no outs": func(tx *btcrpc.Transaction) { tx.Outputs = nil },
		"txid":    func(tx *btcrpc.Transaction) { tx.TxID = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newWatchEnv(t)
			o := e.order(10_000)
			txid := e.payOrder(o, 10_000)
			e.svc.rpc = &badTx{RPC: e.node, txid: txid, f: f}
			e.node.Mine(1)
			e.pass()
			if _, ok := e.mark(txid); ok || e.rail(o).Paid || e.count("ALERT: bitcoind answered with a bad transaction") != 1 {
				t.Fatalf("%+v\n%s", e.rail(o), e.log)
			}
			e.safety(o)
		})
	}
}

// The output is found by its vout field, not by its place in the slice.
func TestOutputFoundByVout(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	txid := e.payOrder(o, 10_000)
	e.svc.rpc = &badTx{RPC: e.node, txid: txid, f: func(tx *btcrpc.Transaction) {
		tx.Outputs = append([]btcrpc.Output{{Vout: 9, ValueSat: 1, Address: "bcrt1qother000000000000000000000000000000"}}, tx.Outputs...)
	}}
	e.node.Mine(1)
	e.pass()
	if !e.rail(o).Paid {
		t.Fatalf("%+v\n%s", e.rail(o), e.log)
	}
	e.safety(o)
}

// --- replay and restarts ---

func TestReplayGivesOneRow(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	txid := e.payOrder(o, 10_000)
	e.passes(10)
	if l := e.txos(o); len(l) != 1 || l[0].TxID != txid {
		t.Fatalf("%+v", l)
	}
	if m, _ := e.mark(txid); m.Seq != 1 {
		t.Fatalf("an unchanged output was marked again: %+v", m)
	}
	e.node.Mine(1)
	e.passes(10)
	if _, ok := e.fwd(txid, 1); ok || !e.rail(o).Paid || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatal("recorded twice")
	}
	// Plan 6.1: a bump after the expiry, listed in each of 10 passes: one row, its inherited first-seen unchanged.
	o2 := e.order(100_000)
	a := e.payOrder(o2, 100_000)
	e.pass()
	first := e.txo(a).FirstSeen
	e.pastExpiry(o2)
	b := e.node.Replace(a, true)
	e.passes(10)
	if l := e.txos(o2); len(l) != 2 || !e.txo(b).FirstSeen.Equal(first) {
		t.Fatalf("the bump: %+v", l)
	}
	e.node.Mine(1)
	e.passes(10)
	if _, ok := e.fwd(b, 1); ok || !e.rail(o2).Paid || e.eventCount(o2, store.EventPaid) != 1 || !e.txo(b).FirstSeen.Equal(first) {
		t.Fatal("the bump recorded twice, or its first-seen changed")
	}
	if _, ok := e.fwd(a, 0); ok {
		t.Fatal("the replaced output was recorded")
	}
	e.safety(o, o2)
}

// txo is the stored row of output 0 of txid.
func (e *env) txo(txid string) store.TXO {
	e.t.Helper()
	x, err := e.st.TXO(bg, store.TXOKey{TxID: txid})
	if err != nil {
		e.t.Fatalf("TXO %s: %v", txid[:8], err)
	}
	return x
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

// A stop of the service (its context cancelled) while a pass waits on bitcoind in steps 0 to 2 is no failed call: the
// pass returns the context's error and logs no logCallFailed (found by the regtest qualification, plan 6.1: a
// stop that landed in a pass's first calls logged a false ALERT). The same call failing without a cancel still logs
// it, once.
func TestStopDuringAPassCallLogsNoAlert(t *testing.T) {
	for _, c := range []struct {
		name, method string
		behind       bool // step 0: the re-check when behind
	}{
		{"check", "getblockchaininfo", true},
		{"getblockchaininfo", "getblockchaininfo", false},
		{"getmempoolinfo", "getmempoolinfo", false},
		{"listsinceblock", "listsinceblock", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newWatchEnv(t)
			e.svc.behind.Store(c.behind)
			e.node.Hang(c.method)
			ctx, cancel := context.WithCancel(bg)
			time.AfterFunc(50*time.Millisecond, cancel)
			if err := e.svc.pass(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("pass: %v, want the context's error", err)
			}
			if n := e.count(logCallFailed); n != 0 {
				t.Fatalf("%d call-failed lines on a stop:\n%s", n, e.log)
			}
			e.node.Unhang(c.method)
			e.svc.behind.Store(c.behind)
			e.node.Fail(c.method, fmt.Errorf("%w: %s refused", btcrpc.ErrTransport, c.method))
			if err := e.pass(); err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("pass with a failed call: %v", err)
			}
			if n := e.count(logCallFailed); n != 1 || !e.logHas(`msg="`+logCallFailed+`" method=`+c.name) {
				t.Fatalf("%d call-failed lines on a failed call, want 1 for %s:\n%s", n, c.name, e.log)
			}
		})
	}
}

func TestRestartBetweenDiscoveryAndMark(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	txid := e.payOrder(o, 10_000)
	e.node.Mine(1)
	fired := e.crashAt("scan:after-record")
	if err := e.pass(); !errors.Is(err, errCrash) || !*fired {
		t.Fatalf("no crash: %v", err)
	}
	if len(e.txos(o)) != 1 {
		t.Fatal("not discovered")
	}
	if _, ok := e.mark(txid); ok {
		t.Fatal("marked after the crash point")
	}
	e.restart()
	e.pass()
	if _, ok := e.fwd(txid, 0); !ok || !e.rail(o).Paid {
		t.Fatal("not recorded after the restart")
	}
	e.restart()
	e.passes(2)
	if _, ok := e.fwd(txid, 1); ok {
		t.Fatal("recorded twice")
	}
	e.safety(o)
}

func TestRestartBetweenMarkAndTransfer(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	txid := e.payOrder(o, 10_000)
	e.node.Mine(1)
	e.crashAt("track:after-mark")
	if err := e.pass(); !errors.Is(err, errCrash) {
		t.Fatalf("no crash: %v", err)
	}
	if m, _ := e.mark(txid); m.Kind != store.MarkMined {
		t.Fatalf("%+v", m)
	}
	if _, ok := e.fwd(txid, 0); ok {
		t.Fatal("recorded before the crash point")
	}
	e.restart()
	e.passes(3)
	if _, ok := e.fwd(txid, 0); !ok || !e.rail(o).Paid {
		t.Fatal("not recorded after the restart")
	}
	if _, ok := e.fwd(txid, 1); ok {
		t.Fatal("recorded twice")
	}
	e.safety(o)
}

// --- the confirmation tiers ---

// fourTiers is the default list under a 0 tier.
var fourTiers = []Tier{{UpTo: 10_000, Confirmations: 0}, {UpTo: 1_000_000, Confirmations: 1}, {UpTo: 10_000_000, Confirmations: 3}, {UpTo: 0, Confirmations: 6}}

func TestTiers(t *testing.T) {
	e := newWatchEnvTiers(t, fourTiers)
	tiny, edge := e.order(10_000), e.order(10_001)
	small, mid, big := e.order(500_000), e.order(5_000_000), e.order(50_000_000)
	for _, o := range []store.Order{tiny, edge, small, mid, big} {
		e.payOrder(o, o.AmountSat)
	}
	e.pass()
	if r := e.rail(tiny); !r.Paid || r.State != "paid" || r.Confirmations != 0 || r.RequiredConfirmations != 0 {
		t.Fatalf("10,000 sat in the mempool: %+v", r)
	}
	for _, o := range []store.Order{edge, small, mid, big} {
		if r := e.rail(o); r.State != "seen" || r.Confirmations != 0 || r.ReceivedSat != o.AmountSat || r.Paid {
			t.Fatalf("in the mempool: %+v", r)
		}
	}
	e.node.Mine(1)
	e.pass()
	if !e.rail(edge).Paid || !e.rail(small).Paid || e.rail(mid).Paid || e.rail(big).Paid {
		t.Fatal("after 1 block")
	}
	e.node.Mine(2)
	e.pass()
	if !e.rail(mid).Paid || e.rail(big).Paid {
		t.Fatal("after 3 blocks")
	}
	e.node.Mine(2)
	e.pass()
	if r := e.rail(big); r.Paid || r.State != "seen" || r.Confirmations != 5 || r.RequiredConfirmations != 6 {
		t.Fatalf("after 5 blocks: %+v", r)
	}
	e.node.Mine(1)
	e.pass()
	if r := e.rail(big); !r.Paid || r.State != "paid" || r.Confirmations != 6 {
		t.Fatalf("after 6 blocks: %+v", r)
	}
	if e.eventCount(tiny, store.EventPaid) != 1 {
		t.Fatalf("10,000 sat: %+v", e.events(tiny))
	}
	e.safety(tiny, edge, small, mid, big)
}

func TestTierBoundaries(t *testing.T) {
	e := newWatchEnvTiers(t, fourTiers)
	want := map[int64]int{10_000: 0, 10_001: 1, 1_000_000: 1, 1_000_001: 3, 10_000_000: 3, 10_000_001: 6}
	orders := map[int64]store.Order{}
	for sat := range want {
		orders[sat] = e.order(sat)
		e.payOrder(orders[sat], sat)
	}
	for blocks := 0; blocks <= 6; blocks++ {
		if blocks > 0 { // one pass with every payment in the mempool
			e.node.Mine(1)
		}
		e.pass()
		for sat, req := range want {
			r := e.rail(orders[sat])
			if r.RequiredConfirmations != req || r.Paid != (blocks >= req) || r.Confirmations != blocks {
				t.Fatalf("%d sat after %d blocks: %+v", sat, blocks, r)
			}
		}
	}
	var all []store.Order
	for _, o := range orders {
		all = append(all, o)
	}
	e.safety(all...)
}

// --- a 0 tier: paid from the mempool ---

func TestZeroConfPaidFromTheMempool(t *testing.T) {
	e := newZeroConfEnv(t)
	o := e.order(5_000)
	txid := e.payOrder(o, 5_000)
	e.pass()
	if f, ok := e.fwd(txid, 0); !ok || f.Code != codePaid {
		t.Fatalf("no forward at epoch 0: %+v %v", f, ok)
	}
	if r := e.rail(o); !r.Paid || r.Confirmations != 0 || r.RequiredConfirmations != 0 || r.State != "paid" || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("after one pass: %+v %+v", r, e.events(o))
	}
	watched := func() bool {
		l, err := e.st.WatchedTXOs(bg, e.clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range l {
			if x.TxID == txid {
				return true
			}
		}
		return false
	}
	if !watched() {
		t.Fatal("settled from the mempool")
	}
	e.node.Mine(1)
	e.pass()
	if _, ok := e.fwd(txid, 1); ok || !watched() {
		t.Fatal("after 1 block: a second forward, or settled below SettledDepth")
	}
	e.node.Mine(5)
	e.pass()
	if watched() || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("after 6 blocks: still watched, or %+v", e.events(o))
	}
	if _, ok := e.fwd(txid, 1); ok {
		t.Fatal("recorded twice")
	}
	e.safety(o)
}

// env.safety itself guards the record rule, with no assertion of the test's own: on a tier above 0 a payment in the
// mempool is never recorded, and on a 0 tier a late one is not either (Ruling 6.1-10). A settle that recorded either
// fails the helper's "recorded but its mark" check.
func TestSafetyGuardsTheMempoolRule(t *testing.T) {
	t.Run("tier 1, in the mempool", func(t *testing.T) {
		e := newWatchEnv(t)
		o := e.order(100_000)
		e.payOrder(o, o.AmountSat)
		e.pass()
		e.safety(o)
	})
	t.Run("tier 0, late, in the mempool", func(t *testing.T) {
		e := newZeroConfEnv(t)
		o := e.order(5_000)
		address := e.addr(o)
		e.pass()
		e.pastExpiry(o)
		e.node.Pay(address, 5_000)
		e.pass()
		e.safety(o)
	})
}

// --- odd entries and the window ---

func TestReceiveAfterTheWindow(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	address := e.addr(o)
	e.pass()
	e.clk.Advance(o.ExpiresAt.Sub(e.clk.Now()) + store.WatchWindow + time.Second)
	txid := e.node.Pay(address, 10_000)
	e.pass()
	if l := e.txos(o); len(l) != 1 || l[0].TxID != txid {
		t.Fatalf("no row: %+v", l)
	}
	e.node.Mine(6)
	e.passes(3) // re-listed while below the target depth
	if e.count("ALERT: an on-chain payment arrived after the address's watch window") != 1 {
		t.Fatalf("after-window ALERT:\n%s", e.log)
	}
	// It reads late, but it is never recorded, so it gets no order.late (Ruling Z2); the after-window ALERT is the
	// operator's signal.
	if e.hasEvent(o, store.EventLate, 1) || e.rail(o).State != "late" {
		t.Fatalf("%+v %+v", e.events(o), e.rail(o))
	}
	if e.node.TxCalls(txid) != 0 {
		t.Fatal("an output past its window was tracked")
	}
	if _, ok := e.fwd(txid, 0); ok {
		t.Fatal("an output past its window is in the ledger")
	}
	e.safety(o)
}

func TestOddEntriesAreIgnored(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	address := e.addr(o)
	txid := e.node.Pay(address, 10_000)
	stranger := e.node.Pay(e.node.AddressAt(500), 7_000) // the wallet's, but never issued by SatBooth
	other := strings.Repeat("ab", 32)
	inj := &inject{RPC: e.node, dropped: 2, extra: []btcrpc.WalletTx{
		{Category: "send", Address: address, AmountSat: -5_000, TxID: other, Vout: 0},
		{Category: "receive", Address: address, AmountSat: -5_000, TxID: other, Vout: 1},
		{Category: "receive", Address: address, AmountSat: 0, TxID: other, Vout: 2},
		{Category: "generate", Address: address, AmountSat: 5_000, TxID: other, Vout: 3},
		{Category: "receive", Address: "bcrt1qsomeoneelse0000000000000000000000000", AmountSat: 5_000, TxID: other, Vout: 4},
		{Category: "receive", Address: address, AmountSat: 10_000, TxID: txid, Vout: 0}, // a duplicate
	}}
	e.svc.rpc = inj
	e.pass()
	e.pass()
	l := e.txos(o)
	if len(l) != 1 || l[0].TxID != txid {
		t.Fatalf("%+v", l)
	}
	if _, err := e.st.TXO(bg, store.TXOKey{TxID: stranger}); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a payment to an address SatBooth never issued was stored")
	}
	if e.count("bitcoind listed entries SatBooth could not read") != 2 {
		t.Fatalf("dropped line:\n%s", e.log)
	}
	e.node.Mine(1)
	e.pass()
	if !e.rail(o).Paid || e.rail(o).ConfirmedSat != 10_000 {
		t.Fatalf("%+v", e.rail(o))
	}
	e.safety(o)
}

// --- the window end: an on-time payment unconfirmed at the window close ---

const (
	alertAtWindow = "ALERT: an order was underpaid at the end of its watch window; its payment is not yet confirmed enough and stays tracked"
	alertNoFinal  = "ALERT: an on-chain order left its watch window without a final event; check it by hand"
	alertLeftPool = "ALERT: an on-time on-chain payment past its watch window left bitcoind's mempool; SatBooth stops polling it and will notice it again only if it is mined"
)

// StatusOf is the state a real status.Resolver over st, svc and ln reads for o at now. status imports onchain, so the
// resolver is built in an external test file (status_ext_test.go), whose init sets this.
var StatusOf func(st *store.Store, svc *Service, ln Lightning, now func() time.Time, o store.Order) (string, error)

// status is StatusOf for e.
func (e *env) status(o store.Order) string {
	e.t.Helper()
	if StatusOf == nil {
		e.t.Fatal("StatusOf is not set")
	}
	s, err := StatusOf(e.st, e.svc, e.ln, e.clk.Now, o)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// toWindowEnd moves the clock to exactly the order's ExpiresAt + WatchWindow.
func (e *env) toWindowEnd(o store.Order) {
	e.clk.Advance(o.ExpiresAt.Add(store.WatchWindow).Sub(e.clk.Now()))
}

// watched reports whether the output is in the store's watched set at the clock's now.
func (e *env) watched(txid string) bool {
	e.t.Helper()
	l, err := e.st.WatchedTXOs(bg, e.clk.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, x := range l {
		if x.TxID == txid {
			return true
		}
	}
	return false
}

// stuckAtWindowEnd is the setup of a 2,000,000-sat order (tier 3) paid in full and never mined, past its
// expiry (a pass: no final event, the counted live sum reaches the amount), then at its window end.
func (e *env) stuckAtWindowEnd() (store.Order, string) {
	e.t.Helper()
	e.node.Mine(1) // a tip above the genesis: a settled fact needs a height of at least 1
	o := e.order(2_000_000)
	txid := e.payOrder(o, 2_000_000)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	if len(e.events(o)) != 0 || e.rail(o).State != "seen" {
		e.t.Fatalf("setup: past the expiry %+v %+v", e.events(o), e.rail(o))
	}
	e.toWindowEnd(o)
	return o, txid
}

// At the window close the stuck payment gives order.underpaid, the window ALERT once and no
// order.expired; the rail and the status read underpaid (the events and the API agree), and the output is still
// polled. Mined later at its tier, it is recorded: order.paid with the Ruling S ALERT, then settled and no longer
// polled. A crash right after the event, or a restart before the pass, changes nothing: no event twice, one ALERT.
func TestStuckPaymentAtWindowClose(t *testing.T) {
	for _, c := range []string{"no crash", "crash after the event", "restart before the pass"} {
		t.Run(c, func(t *testing.T) {
			e := newWatchEnv(t)
			o, txid := e.stuckAtWindowEnd()
			switch c {
			case "crash after the event":
				fired := e.crashAt("settle:after-event")
				if err := e.pass(); !errors.Is(err, errCrash) || !*fired {
					t.Fatalf("no crash: %v", err)
				}
				e.restart()
			case "restart before the pass":
				e.restart()
			}
			e.pass()
			e.pass() // a replay: nothing more
			ev := e.events(o)
			if len(ev) != 1 || ev[0].Type != store.EventUnderpaid || ev[0].Seq != 1 || ev[0].ID != store.EventID("order.underpaid", o.ID, 1) {
				t.Fatalf("events %+v", ev)
			}
			if e.count(alertAtWindow) != 1 || e.count(alertUnderpaid) != 0 || e.count(alertNoFinal) != 0 {
				t.Fatalf("ALERTs:\n%s", e.log)
			}
			if r := e.rail(o); r.State != RailUnderpaid || r.ReceivedSat != 2_000_000 || r.Confirmations != 0 || r.Paid {
				t.Fatalf("Rail %+v", r)
			}
			if s := e.status(o); s != "underpaid" {
				t.Fatalf("status %q, want underpaid", s)
			}
			calls := e.node.TxCalls(txid)
			e.passes(2)
			if e.node.TxCalls(txid) != calls+2 || !e.watched(txid) {
				t.Fatalf("not polled past the window: %d then %d", calls, e.node.TxCalls(txid))
			}
			e.node.Mine(3)
			e.pass()
			if _, ok := e.fwd(txid, 0); !ok || !e.hasEvent(o, store.EventPaid, 1) || e.count(logPaidAfterFinal) != 1 {
				t.Fatalf("mined at its tier: %+v\n%s", e.events(o), e.log)
			}
			if r := e.rail(o); r.State != RailPaid || !r.Paid {
				t.Fatalf("Rail %+v", r)
			}
			if s := e.status(o); s != "paid" {
				t.Fatalf("status %q, want paid", s)
			}
			e.node.Mine(3) // SettledDepth 6
			e.pass()
			if e.watched(txid) {
				t.Fatal("not settled at SettledDepth")
			}
			calls = e.node.TxCalls(txid)
			e.passes(2)
			if e.node.TxCalls(txid) != calls {
				t.Fatal("polled after it settled")
			}
			if e.eventCount(o, store.EventUnderpaid) != 1 || e.eventCount(o, store.EventExpired) != 0 || len(e.events(o)) != 2 {
				t.Fatalf("events %+v", e.events(o))
			}
			e.safety(o)
		})
	}
}

// A stuck payment replaced away after its window closed: the output is dead and settled at once (its order has its
// final event and no record to reverse), no event, and the order still reads underpaid, as its order.underpaid says
// (the API never shows a final state the events contradict). No more gettransaction.
func TestStuckThenReplacedAfterTheWindow(t *testing.T) {
	e := newWatchEnv(t)
	o, txid := e.stuckAtWindowEnd()
	e.pass()
	if !e.hasEvent(o, store.EventUnderpaid, 1) {
		t.Fatalf("setup: %+v", e.events(o))
	}
	e.node.Replace(txid, false)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkDead {
		t.Fatalf("mark %+v", m)
	}
	if e.watched(txid) || e.count(alertLeftPool) != 1 {
		t.Fatalf("a dead output past its window, its order settled: watched %v, %d ALERTs", e.watched(txid), e.count(alertLeftPool))
	}
	if len(e.events(o)) != 1 {
		t.Fatalf("events %+v", e.events(o))
	}
	if r := e.rail(o); r.State != RailUnderpaid || r.ReceivedSat != 0 {
		t.Fatalf("Rail %+v", r)
	}
	if s := e.status(o); s != "underpaid" {
		t.Fatalf("status %q, want underpaid", s)
	}
	calls := e.node.TxCalls(txid)
	e.passes(2)
	if e.node.TxCalls(txid) != calls || e.count(alertLeftPool) != 1 {
		t.Fatal("a settled dead output was polled, or alerted again")
	}
	e.safety(o)
}

// A dead on-time output settled past its window (the ALERT says SatBooth notices it again only if it is mined),
// then rebroadcast: seen in the mempool only, it stays settled and unpolled; mined, the scan lists it and it is
// watched again. Below its tier it is not yet recorded; at its tier it is: order.paid after order.underpaid (Ruling
// S), the rail and the status paid; at SettledDepth it is settled again.
func TestDeadSettledThenRebroadcastIsRecorded(t *testing.T) {
	e := newWatchEnv(t)
	o, txid := e.stuckAtWindowEnd()
	e.pass()
	e.node.Evict(txid)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkDead || e.watched(txid) || e.count(alertLeftPool) != 1 {
		t.Fatalf("evicted: %+v watched %v\n%s", m, e.watched(txid), e.log)
	}
	e.node.Rebroadcast(txid)
	calls := e.node.TxCalls(txid)
	e.passes(2)
	if e.watched(txid) || e.node.TxCalls(txid) != calls {
		t.Fatal("reopened from the mempool")
	}
	e.node.Mine(1)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkMined || !e.watched(txid) {
		t.Fatalf("mined: %+v watched %v", m, e.watched(txid))
	}
	if _, ok := e.fwd(txid, 0); ok || e.rail(o).Paid {
		t.Fatal("recorded below its tier")
	}
	e.node.Mine(2)
	e.pass()
	if _, ok := e.fwd(txid, 0); !ok || !e.hasEvent(o, store.EventPaid, 1) || e.count(logPaidAfterFinal) != 1 {
		t.Fatalf("at its tier: %+v\n%s", e.events(o), e.log)
	}
	if r := e.rail(o); r.State != RailPaid || !r.Paid || e.status(o) != "paid" {
		t.Fatalf("Rail %+v status %s", r, e.status(o))
	}
	e.node.Mine(3)
	e.pass()
	if e.watched(txid) {
		t.Fatal("not settled again at SettledDepth")
	}
	if e.count(alertLeftPool) != 1 || e.eventCount(o, store.EventUnderpaid) != 1 || len(e.events(o)) != 2 {
		t.Fatalf("events %+v", e.events(o))
	}
	e.safety(o)
}

// Past its window a dead output is settled only once its order has its final event. A mixed order whose
// Lightning side is not done keeps its dead output polled (no ALERT); once Lightning is done, order.expired, then
// settled with the ALERT once.
func TestDeadPastTheWindowWaitsForTheFinalEvent(t *testing.T) {
	e := newWatchEnv(t)
	e.node.Mine(1)
	o := e.mixed(21_000, 2_000_000)
	txid := e.payOrder(o, 2_000_000)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	e.toWindowEnd(o)
	e.pass()
	e.node.Evict(txid)
	e.pass()
	calls := e.node.TxCalls(txid)
	e.passes(2)
	// polled in both passes, once each: an output already dead is not read again
	if m, _ := e.mark(txid); m.Kind != store.MarkDead || !e.watched(txid) || e.node.TxCalls(txid) != calls+2 {
		t.Fatalf("dead, Lightning open: %+v watched %v", m, e.watched(txid))
	}
	if len(e.events(o)) != 0 || e.count(alertLeftPool) != 0 {
		t.Fatalf("events %+v\n%s", e.events(o), e.log)
	}
	e.ln.set(false, true)
	e.pass()
	if !e.hasEvent(o, store.EventExpired, 1) || e.watched(txid) || e.count(alertLeftPool) != 1 {
		t.Fatalf("Lightning done: %+v watched %v", e.events(o), e.watched(txid))
	}
	calls = e.node.TxCalls(txid)
	e.passes(2)
	if e.node.TxCalls(txid) != calls || e.count(alertLeftPool) != 1 {
		t.Fatal("polled or alerted after it settled")
	}
	e.safety(o)
}

// Ruling 6.1-2: the window-close order.underpaid waits for Lightning, like every final event. While the stub reads
// new, no event (and the order, past its window without one, is named by the no-final ALERT once); once Lightning is
// expired, order.underpaid follows.
func TestStuckMixedOrderWaitsForLightning(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(21_000, 2_000_000)
	txid := e.payOrder(o, 2_000_000)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	e.toWindowEnd(o)
	e.pass()
	e.passes(2)
	if len(e.events(o)) != 0 || e.count(alertAtWindow) != 0 {
		t.Fatalf("an event while Lightning may still pay: %+v", e.events(o))
	}
	if e.count(alertNoFinal) != 1 {
		t.Fatalf("no-final ALERT %d times:\n%s", e.count(alertNoFinal), e.log)
	}
	if s := e.status(o); s != "new" {
		t.Fatalf("status %q, want new while Lightning is open", s)
	}
	if !e.watched(txid) {
		t.Fatal("not watched")
	}
	e.ln.set(false, true)
	e.pass()
	if ev := e.events(o); len(ev) != 1 || ev[0].Type != store.EventUnderpaid || e.count(alertAtWindow) != 1 {
		t.Fatalf("events %+v\n%s", ev, e.log)
	}
	if s := e.status(o); s != "underpaid" {
		t.Fatalf("status %q, want underpaid", s)
	}
	e.safety(o)
}

// The stuck set is bounded to on-time outputs that are not settled. At the window close, a late output (first seen
// after the expiry) is not polled, and a dead one is polled once more (its order settles) and then settled; the
// on-time live one is polled; and a recorded output not yet SettledDepth deep (tier 1, paid) is polled past the
// window until it is settled, then not.
func TestStuckSetIsBoundedToOnTimeLiveOutputs(t *testing.T) {
	e := newWatchEnv(t)
	late, dead, live, done := e.order(20_000), e.order(30_000), e.order(2_000_000), e.order(10_000)
	td := e.payOrder(done, 10_000)
	e.node.Mine(2)
	tdead := e.payOrder(dead, 30_000)
	tlive := e.payOrder(live, 2_000_000)
	al := e.addr(late)
	e.pass()
	e.node.Replace(tdead, false)
	e.pass()
	if m, _ := e.mark(tdead); m.Kind != store.MarkDead || !e.rail(done).Paid {
		t.Fatalf("setup: %+v %+v", m, e.rail(done))
	}
	e.pastExpiry(live)
	tlate := e.node.Pay(al, 20_000)
	e.pass()
	if !e.watched(tlate) || !e.hasEvent(dead, store.EventExpired, 1) {
		t.Fatalf("setup: late watched %v, dead %+v", e.watched(tlate), e.events(dead))
	}
	e.toWindowEnd(live)
	cl, cd := e.node.TxCalls(tlate), e.node.TxCalls(tdead)
	e.pass()
	if e.node.TxCalls(tlate) != cl || e.node.TxCalls(tdead) != cd+1 {
		t.Fatalf("at the window close: late %d→%d, dead %d→%d", cl, e.node.TxCalls(tlate), cd, e.node.TxCalls(tdead))
	}
	if e.count(alertLeftPool) != 1 || !e.logHas(`msg="`+alertLeftPool+`" order=`+dead.ID) {
		t.Fatalf("the dead one's ALERT:\n%s", e.log)
	}
	if e.watched(tlate) || e.watched(tdead) || !e.watched(tlive) || !e.watched(td) {
		t.Fatalf("watched: late %v dead %v live %v done %v", e.watched(tlate), e.watched(tdead), e.watched(tlive), e.watched(td))
	}
	cl, cd = e.node.TxCalls(tlate), e.node.TxCalls(tdead)
	cv, cdone := e.node.TxCalls(tlive), e.node.TxCalls(td)
	e.passes(2)
	if e.node.TxCalls(tlate) != cl || e.node.TxCalls(tdead) != cd || e.node.TxCalls(tlive) != cv+2 || e.node.TxCalls(td) != cdone+2 {
		t.Fatal("the stuck set is not on-time and unsettled")
	}
	e.node.Mine(4) // done reaches SettledDepth 6; live is mined too, at 4 of its 3 (recorded) but not 6 deep
	e.pass()
	if e.watched(td) || !e.watched(tlive) {
		t.Fatalf("after 6 blocks: done %v live %v", e.watched(td), e.watched(tlive))
	}
	cdone = e.node.TxCalls(td)
	e.passes(2)
	if e.node.TxCalls(td) != cdone {
		t.Fatal("a settled output was polled")
	}
	if !e.rail(live).Paid || !e.hasEvent(live, store.EventPaid, 1) || !e.hasEvent(live, store.EventUnderpaid, 1) {
		t.Fatalf("live: %+v %+v", e.rail(live), e.events(live))
	}
	if e.hasEvent(late, store.EventLate, 1) || len(e.events(done)) != 1 {
		t.Fatalf("late %+v done %+v", e.events(late), e.events(done))
	}
	e.safety(late, dead, live, done)
}

// A tier-1 record not yet SettledDepth deep at its window end, reorged back to the mempool after it: order.reverted,
// and it stays watched (OrdersAwaitingPaidAgain is window-bounded, so the stuck set is what settles the order);
// mined again, order.paid_again with a forward at epoch 1.
func TestRecordReorgedPastTheWindowIsPaidAgain(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	txid := e.payOrder(o, 100_000)
	e.node.Mine(1)
	e.pass()
	e.toWindowEnd(o)
	e.pass()
	e.node.Invalidate(1)
	e.pass()
	if r, ok := e.rev(txid, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || !e.watched(txid) {
		t.Fatalf("reorged: %+v %v, events %+v, watched %v", r, ok, e.events(o), e.watched(txid))
	}
	e.node.Mine(1)
	e.pass()
	if _, ok := e.fwd(txid, 1); !ok || !e.hasEvent(o, store.EventPaidAgain, 1) || !e.rail(o).Paid {
		t.Fatalf("mined again: %+v %+v", e.events(o), e.rail(o))
	}
	e.safety(o)
}

// On a 0 tier, a 0-conf payment recorded from the mempool and still unconfirmed past the window
// stays tracked and paid; replaced or evicted after the window, it is reversed with order.reverted, then settled.
func TestZeroConfRecordPastTheWindow(t *testing.T) {
	for _, gone := range []string{"replaced", "evicted"} {
		t.Run(gone, func(t *testing.T) {
			e := newZeroConfEnv(t)
			e.node.Mine(1) // a tip above the genesis: a settled fact needs a height of at least 1
			o, txid := e.zeroConfPaid()
			e.toWindowEnd(o)
			calls := e.node.TxCalls(txid)
			e.pass()
			e.passes(2)
			if e.node.TxCalls(txid) != calls+3 || !e.watched(txid) {
				t.Fatal("a 0-conf record past the window is not tracked")
			}
			if r := e.rail(o); !r.Paid || r.State != RailPaid || len(e.events(o)) != 1 || e.count(alertAtWindow) != 0 {
				t.Fatalf("Rail %+v, events %+v", r, e.events(o))
			}
			if s := e.status(o); s != "paid" {
				t.Fatalf("status %q", s)
			}
			if gone == "replaced" {
				e.node.Replace(txid, false)
			} else {
				e.node.Evict(txid)
			}
			e.pass()
			if _, ok := e.rev(txid, 0); ok || !e.rail(o).Paid || len(e.events(o)) != 1 || !e.watched(txid) {
				t.Fatalf("the held pass: reversed %v, Rail %+v, events %+v", ok, e.rail(o), e.events(o))
			}
			e.pass()
			r, ok := e.rev(txid, 0)
			if !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || e.count(alertReorg) != 1 {
				t.Fatalf("reversal %+v %v, events %+v", r, ok, e.events(o))
			}
			if rl := e.rail(o); rl.Paid || rl.State != RailExpired || rl.ReceivedSat != 0 {
				t.Fatalf("Rail %+v", rl)
			}
			if s := e.status(o); s != "expired" {
				t.Fatalf("status %q", s)
			}
			if e.watched(txid) || e.count(alertLeftPool) != 1 {
				t.Fatalf("reversed and dead, its order settled: watched %v, %d ALERTs", e.watched(txid), e.count(alertLeftPool))
			}
			calls = e.node.TxCalls(txid)
			e.passes(2)
			if e.node.TxCalls(txid) != calls || len(e.events(o)) != 2 {
				t.Fatalf("polled or more events: %+v", e.events(o))
			}
			e.safety(o)
		})
	}
}

// --- plan 6.1: an RBF replacement inherits the original's first-seen ---

// A bump after the expiry that keeps the address and the amount inherits the on-time original's first-seen: counted,
// never late; mined, it pays the order, and the replaced output is dead and never recorded.
func TestBumpAfterExpiryIsCounted(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	first := e.txo(a).FirstSeen
	if !first.Before(o.ExpiresAt) || e.rail(o).State != RailSeen {
		t.Fatalf("setup: %v %+v", first, e.rail(o))
	}
	e.pastExpiry(o)
	b := e.node.Replace(a, true)
	e.pass()
	if x := e.txo(b); !x.FirstSeen.Equal(first) {
		t.Fatalf("the bump's first-seen %v, the original's %v", x.FirstSeen, first)
	}
	if r := e.rail(o); r.ReceivedSat != 100_000 || r.LateSat != 0 || r.State != RailSeen {
		t.Fatalf("Rail %+v", r)
	}
	if m, _ := e.mark(a); m.Kind != store.MarkDead || len(e.events(o)) != 0 {
		t.Fatalf("the original %+v, events %+v", m, e.events(o))
	}
	e.node.Mine(1)
	e.pass()
	if f, ok := e.fwd(b, 0); !ok || f.Code != codePaid || !e.rail(o).Paid || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("mined: %+v %v %+v", f, ok, e.events(o))
	}
	if _, ok := e.fwd(a, 0); ok || e.eventCount(o, store.EventLate) != 0 || e.count(logLate) != 0 {
		t.Fatalf("the original recorded, or late: %+v", e.events(o))
	}
	e.passes(2)
	if m, _ := e.mark(a); m.Kind != store.MarkDead || len(e.events(o)) != 1 {
		t.Fatalf("the original %+v, events %+v", m, e.events(o))
	}
	e.safety(o)
}

// bumpAfterScan replaces the order's paid output once, right after the next pass's listsinceblock (keep: the bump keeps
// the output; otherwise its amount goes to a foreign address). The pass marks the original dead and cannot discover
// the replacement before the next pass: the split that the regtest qualification met (plan 6.1).
func (e *env) bumpAfterScan(txid string, keep bool, crashAfterMark bool) *string {
	nw := new(string)
	crashed := false
	e.svc.hook = func(p string) error {
		switch {
		case p == "scan:after-record" && *nw == "":
			*nw = e.node.Replace(txid, keep)
		case p == "track:after-mark" && crashAfterMark && *nw != "" && !crashed:
			crashed = true
			return errCrash
		}
		return nil
	}
	return nw
}

// A bump after the expiry that lands after a pass's listsinceblock: that pass marks the original dead and does not see
// the replacement yet. It writes no final event (the order is being paid); the next pass discovers the replacement,
// which inherits the first-seen, and the block pays the order with no paid-after-final ALERT.
func TestBumpAfterExpiryAcrossTwoPassesIsNotFinal(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	first := e.txo(a).FirstSeen
	e.pastExpiry(o)
	b := e.bumpAfterScan(a, true, false)
	e.pass()
	if *b == "" {
		t.Fatal("the hook did not run")
	}
	if m, _ := e.mark(a); m.Kind != store.MarkDead {
		t.Fatalf("the original's mark %+v", m)
	}
	if l := e.txos(o); len(l) != 1 {
		t.Fatalf("the replacement was discovered in the first pass: %+v", l)
	}
	if ev := e.events(o); len(ev) != 0 {
		t.Fatalf("a final event in the pass that marked the original dead: %+v", ev)
	}
	e.pass()
	if x := e.txo(*b); !x.FirstSeen.Equal(first) {
		t.Fatalf("the bump's first-seen %v, the original's %v", x.FirstSeen, first)
	}
	if r := e.rail(o); r.ReceivedSat != 100_000 || r.LateSat != 0 || len(e.events(o)) != 0 {
		t.Fatalf("the second pass: %+v %+v", r, e.events(o))
	}
	e.node.Mine(1)
	e.pass()
	e.passes(2)
	if f, ok := e.fwd(*b, 0); !ok || f.Code != codePaid || !e.rail(o).Paid || len(e.events(o)) != 1 || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("mined: %+v %v %+v", f, ok, e.events(o))
	}
	if e.count(logPaidAfterFinal) != 0 || e.count(logUnderpaid) != 0 || e.count(logLate) != 0 {
		t.Fatalf("\n%s", e.log)
	}
	e.safety(o)
}

// The same split when the replacement takes the payment away: the final event is not lost, it comes one pass later.
func TestDivertAfterExpiryAcrossTwoPassesIsFinalOnePassLater(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	e.pastExpiry(o)
	b := e.bumpAfterScan(a, false, false)
	e.pass()
	if m, _ := e.mark(a); *b == "" || m.Kind != store.MarkDead || len(e.events(o)) != 0 {
		t.Fatalf("the first pass: %q %+v %+v", *b, m, e.events(o))
	}
	// While settle holds the final event, the order does not read expired.
	if r, s := e.rail(o), e.status(o); r.State != RailWaiting || r.ReceivedSat != 0 || s != "new" {
		t.Fatalf("the held pass: Rail %+v, status %q", r, s)
	}
	e.pass()
	if !e.hasEvent(o, store.EventExpired, 1) || len(e.events(o)) != 1 || e.rail(o).State != RailExpired || e.status(o) != "expired" {
		t.Fatalf("the second pass: %+v %+v", e.rail(o), e.events(o))
	}
	e.passes(2)
	if len(e.events(o)) != 1 {
		t.Fatalf("%+v", e.events(o))
	}
	e.safety(o)
}

// An on-time payment of a mixed order dies (replaced away) after Lightning paid the order. Past the
// window its dead output settles without the left-pool ALERT: the order is paid, a person has nothing to do. The
// both-rails ALERT does not come either (nothing on-chain was recorded).
func TestDeadOutputOfAPaidOrderSettlesWithoutTheLeftPoolAlert(t *testing.T) {
	e := newWatchEnv(t)
	e.node.Mine(1) // a tip above the genesis: a settled fact needs a height of at least 1
	o := e.mixed(50_000_000, 100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	e.lnPaid(o, strings.Repeat("44", 32))
	e.pass()
	if !e.hasEvent(o, store.EventPaid, 1) {
		t.Fatalf("setup: %+v", e.events(o))
	}
	e.node.Replace(a, false)
	e.pass()
	if m, _ := e.mark(a); m.Kind != store.MarkDead || !e.watched(a) {
		t.Fatalf("the dead output inside the window: %+v, watched %v", m, e.watched(a))
	}
	e.toWindowEnd(o)
	e.pass()
	e.passes(2)
	if e.watched(a) || e.count(alertLeftPool) != 0 || e.count(logBothRails) != 0 || len(e.events(o)) != 1 {
		t.Fatalf("past the window: watched %v, events %+v\n%s", e.watched(a), e.events(o), e.log)
	}
	if s := e.status(o); s != "paid" {
		t.Fatalf("status %q", s)
	}
}

// The dead branch of settled, when its events read fails, does not settle the output (it is polled
// again) and logs logSettleFailed with the order id. The read is made to fail by renaming the events table under the
// store, through a second connection to its file.
func TestSettledLogsAFailedEventsRead(t *testing.T) {
	e := newWatchEnv(t)
	e.node.Mine(1)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	e.pastExpiry(o)
	e.node.Replace(a, false)
	e.pass()
	e.toWindowEnd(o)
	db, err := sql.Open("sqlite3", filepath.Join(e.dir, "orders.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`ALTER TABLE events RENAME TO events_m4`); err != nil {
		t.Fatal(err)
	}
	cur, _, err := e.st.Cursor(bg)
	if err != nil {
		t.Fatal(err)
	}
	if ok, dead, paid := e.svc.settled(bg, e.clk.Now(), cur, trackedOutput{x: e.txo(a), o: o}); ok || dead || paid {
		t.Fatalf("settled %v %v %v on a failed events read", ok, dead, paid)
	}
	if !e.logHas(`msg="` + logSettleFailed + `" order=` + o.ID) {
		t.Fatalf("no settle-failed line with the order:\n%s", e.log)
	}
	if _, err := db.Exec(`ALTER TABLE events_m4 RENAME TO events`); err != nil {
		t.Fatal(err)
	}
}

// The hold and the window close in one pass. The pass that marks the order's only on-time
// output dead after its scan, at the window end, holds the final event; it logs no "left its watch window without a
// final event" ALERT for the order, since the next pass writes the event. When the next pass cannot settle the order
// either (its output's call fails), that pass reports it, once.
func TestHeldAtTheWindowCloseIsNoFalseNoFinal(t *testing.T) {
	for _, nextFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("next pass fails %v", nextFails), func(t *testing.T) {
			e := newWatchEnv(t)
			o := e.order(100_000)
			a := e.payOrder(o, 100_000)
			e.pass()
			e.pastExpiry(o)
			e.pass()
			e.toWindowEnd(o)
			b := e.bumpAfterScan(a, false, false)
			e.pass()
			if m, _ := e.mark(a); *b == "" || m.Kind != store.MarkDead || len(e.events(o)) != 0 {
				t.Fatalf("the held pass: %q %+v %+v", *b, m, e.events(o))
			}
			if e.count(alertNoFinal) != 0 {
				t.Fatalf("a no-final ALERT in the held pass:\n%s", e.log)
			}
			e.svc.hook = nil
			if nextFails {
				e.node.FailTx(a, &btcrpc.Error{Code: -5, Method: "gettransaction"})
				e.passes(1)
				if len(e.events(o)) != 0 || e.count(alertNoFinal) != 1 {
					t.Fatalf("the failed pass: %+v, %d no-final ALERTs\n%s", e.events(o), e.count(alertNoFinal), e.log)
				}
				e.node.MendTx(a)
			}
			e.passes(2)
			if !e.hasEvent(o, store.EventExpired, 1) || len(e.events(o)) != 1 {
				t.Fatalf("after the hold: %+v", e.events(o))
			}
			if want := map[bool]int{false: 0, true: 1}[nextFails]; e.count(alertNoFinal) != want {
				t.Fatalf("%d no-final ALERTs, want %d\n%s", e.count(alertNoFinal), want, e.log)
			}
			e.safety(o)
		})
	}
}

// The hold covers on-time outputs only (diedAfterScan's v.counted). A late output of a mixed order
// that dies after the scan, in the pass where Lightning is done, does not hold the final event: order.expired comes in
// that same pass.
func TestLateOutputDyingAfterTheScanHoldsNothing(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(50_000_000, 100_000)
	ad := e.addr(o)
	e.pastExpiry(o)
	l := e.node.Pay(ad, 100_000)
	e.pass()
	if r := e.rail(o); r.LateSat != 100_000 || len(e.events(o)) != 0 {
		t.Fatalf("setup: %+v %+v", r, e.events(o))
	}
	e.ln.set(false, true)
	b := e.bumpAfterScan(l, false, false)
	e.pass()
	if m, _ := e.mark(l); *b == "" || m.Kind != store.MarkDead {
		t.Fatalf("the late output: %q %+v", *b, m)
	}
	if !e.hasEvent(o, store.EventExpired, 1) || len(e.events(o)) != 1 {
		t.Fatalf("a late output held the final event: %+v", e.events(o))
	}
	e.safety(o)
}

// The window-end branch (atWindowEnd) under the hold. The whole amount is live on time at the window
// end, and another on-time output of the order dies after that pass's scan: the window's order.underpaid is held for
// that pass (the order reads seen, status new, and no no-final ALERT), and the next pass writes it.
func TestWindowEndUnderpaidIsHeld(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	ad := e.addr(o)
	e.node.Pay(ad, 100_000)
	extra := e.node.Pay(ad, 50_000)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	e.toWindowEnd(o)
	b := e.bumpAfterScan(extra, false, false)
	e.pass()
	if m, _ := e.mark(extra); *b == "" || m.Kind != store.MarkDead || len(e.events(o)) != 0 {
		t.Fatalf("the held pass: %q %+v %+v", *b, m, e.events(o))
	}
	if r, s := e.rail(o), e.status(o); r.State != RailSeen || r.ReceivedSat != 100_000 || s != "new" || e.count(alertNoFinal) != 0 {
		t.Fatalf("the held pass: Rail %+v, status %q\n%s", r, s, e.log)
	}
	e.svc.hook = nil
	e.passes(1)
	if !e.hasEvent(o, store.EventUnderpaid, 1) || len(e.events(o)) != 1 || e.count(alertAtWindow) != 1 {
		t.Fatalf("the next pass: %+v\n%s", e.events(o), e.log)
	}
	if r, s := e.rail(o), e.status(o); r.State != RailUnderpaid || s != "underpaid" || e.count(alertNoFinal) != 0 {
		t.Fatalf("after the event: Rail %+v, status %q", r, s)
	}
	e.safety(o)
}

// A crash right after the dead mark of the split pass: the restarted pass's listsinceblock comes after the mark and
// lists the replacement, so no final event is written either; the order is paid when it is mined.
func TestBumpAfterExpiryAcrossACrashIsNotFinal(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	e.pastExpiry(o)
	b := e.bumpAfterScan(a, true, true)
	if err := e.pass(); !errors.Is(err, errCrash) {
		t.Fatalf("no crash: %v", err)
	}
	if m, _ := e.mark(a); *b == "" || m.Kind != store.MarkDead || len(e.events(o)) != 0 {
		t.Fatalf("the crashed pass: %q %+v %+v", *b, m, e.events(o))
	}
	e.restart()
	e.pass()
	if len(e.events(o)) != 0 || e.rail(o).ReceivedSat != 100_000 {
		t.Fatalf("after the restart: %+v %+v", e.rail(o), e.events(o))
	}
	e.node.Mine(1)
	e.pass()
	if !e.rail(o).Paid || len(e.events(o)) != 1 || e.count(logPaidAfterFinal) != 0 {
		t.Fatalf("mined: %+v %+v\n%s", e.rail(o), e.events(o), e.log)
	}
	e.safety(o)
}

// The same bump after the window close (order.underpaid): decision 9, inheritance only inside the
// window. The replacement is a new output with its own first-seen: late, with the after-window ALERT, not in the stuck
// set and never recorded; the order reads late. The dead original settles with the left-pool ALERT (nothing carries
// it on). A person credits the payment.
func TestBumpAfterTheWindowIsLate(t *testing.T) {
	e := newWatchEnv(t)
	e.node.Mine(1) // a tip above the genesis: a settled fact needs a height of at least 1
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	e.toWindowEnd(o)
	e.pass()
	if !e.hasEvent(o, store.EventUnderpaid, 1) || e.count(alertAtWindow) != 1 {
		t.Fatalf("setup: %+v\n%s", e.events(o), e.log)
	}
	e.clk.Advance(time.Second)
	b := e.node.Replace(a, true)
	e.pass()
	if e.watched(b) || !e.txo(b).FirstSeen.Equal(e.clk.Now()) || e.count(logAfterWindow) != 1 {
		t.Fatalf("the bump: watched %v, first-seen %v (now %v)\n%s", e.watched(b), e.txo(b).FirstSeen, e.clk.Now(), e.log)
	}
	if r := e.rail(o); r.ReceivedSat != 0 || r.LateSat != 100_000 || r.State != RailLate {
		t.Fatalf("Rail %+v", r)
	}
	if s := e.status(o); s != "late" {
		t.Fatalf("status %q, want late", s)
	}
	e.node.Mine(1)
	e.passes(3)
	if _, ok := e.fwd(b, 0); ok || e.rail(o).Paid || e.hasEvent(o, store.EventPaid, 1) || e.count(logPaidAfterFinal) != 0 {
		t.Fatalf("mined: %+v\n%s", e.events(o), e.log)
	}
	if _, ok := e.fwd(a, 0); ok || e.watched(a) || e.count(alertLeftPool) != 1 || len(e.events(o)) != 1 {
		t.Fatalf("the original: watched %v, events %+v\n%s", e.watched(a), e.events(o), e.log)
	}
	e.safety(o)
}

// Decision 9's boundary: a bump first listed one second before the window end inherits (counted, tracked, and paid
// when mined); one first listed at the window end, or one second after it, does not (late).
func TestBumpInheritsOnlyInsideTheWindow(t *testing.T) {
	for _, c := range []struct {
		name    string
		at      time.Duration // from the window end
		inherit bool
	}{{"before", -time.Second, true}, {"at", 0, false}, {"after", time.Second, false}} {
		t.Run(c.name, func(t *testing.T) {
			e := newWatchEnv(t)
			o := e.order(100_000)
			a := e.payOrder(o, 100_000)
			e.pass()
			first := e.txo(a).FirstSeen
			e.pastExpiry(o)
			e.pass()
			e.clk.Advance(o.ExpiresAt.Add(store.WatchWindow).Add(c.at).Sub(e.clk.Now()))
			b := e.node.Replace(a, true)
			e.pass()
			x := e.txo(b)
			if got := x.FirstSeen.Equal(first); got != c.inherit {
				t.Fatalf("the bump's first-seen %v, the original's %v, now %v: inherited %v, want %v", x.FirstSeen, first,
					e.clk.Now(), got, c.inherit)
			}
			r := e.rail(o)
			if c.inherit && (r.ReceivedSat != 100_000 || r.LateSat != 0 || !e.watched(b) || e.count(logAfterWindow) != 0) {
				t.Fatalf("inherited: Rail %+v, watched %v\n%s", r, e.watched(b), e.log)
			}
			if !c.inherit && (r.ReceivedSat != 0 || r.LateSat != 100_000 || r.State != RailLate || e.count(logAfterWindow) != 1) {
				t.Fatalf("late: Rail %+v\n%s", r, e.log)
			}
			e.node.Mine(1)
			e.passes(2)
			if paid := e.rail(o).Paid; paid != c.inherit {
				t.Fatalf("paid %v after the block, want %v: %+v", paid, c.inherit, e.events(o))
			}
			e.safety(o)
		})
	}
}

// The original was itself late (first seen after the expiry): its bump has nothing on time to inherit and gets its own
// first-seen; it stays late.
func TestBumpOfALateOriginalStaysLate(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	ad := e.addr(o)
	e.pastExpiry(o)
	a := e.node.Pay(ad, 100_000)
	e.pass()
	e.clk.Advance(time.Minute)
	b := e.node.Replace(a, true)
	e.pass()
	if x := e.txo(b); !x.FirstSeen.Equal(e.clk.Now()) || x.FirstSeen.Equal(e.txo(a).FirstSeen) {
		t.Fatalf("the bump's first-seen %v, now %v", x.FirstSeen, e.clk.Now())
	}
	if r := e.rail(o); r.LateSat != 100_000 || r.ReceivedSat != 0 || r.State != RailLate {
		t.Fatalf("Rail %+v", r)
	}
	e.node.Mine(1)
	e.pass()
	if f, ok := e.fwd(b, 0); !ok || f.Code != codeLate || e.rail(o).Paid || !e.hasEvent(o, store.EventLate, 1) || e.eventCount(o, store.EventPaid) != 0 {
		t.Fatalf("mined: %+v %v %+v", f, ok, e.events(o))
	}
	e.safety(o)
}

// A bump of a bump: walletconflicts lists both ancestors (they spend the same input), so the last one inherits the
// first's on-time first-seen, also when the middle one paid less and was late itself.
func TestChainOfTwoBumps(t *testing.T) {
	for _, mid := range []int64{100_000, 90_000} {
		t.Run(fmt.Sprint(mid), func(t *testing.T) {
			e := newWatchEnv(t)
			o := e.order(100_000)
			ad := e.addr(o)
			a := e.node.Pay(ad, 100_000)
			e.pass()
			first := e.txo(a).FirstSeen
			e.pastExpiry(o)
			b := e.node.ReplaceWith(a, []onchaintest.Out{{Address: ad, Sat: mid}})
			e.pass()
			want := first
			if mid < 100_000 {
				want = e.clk.Now() // a lower amount: its own first-seen, late
			}
			if x := e.txo(b); !x.FirstSeen.Equal(want) {
				t.Fatalf("the first bump's first-seen %v, want %v", x.FirstSeen, want)
			}
			e.clk.Advance(time.Minute)
			c := e.node.ReplaceWith(b, []onchaintest.Out{{Address: ad, Sat: 100_000}})
			e.pass()
			if x := e.txo(c); !x.FirstSeen.Equal(first) {
				t.Fatalf("the second bump's first-seen %v, the first payment's %v", x.FirstSeen, first)
			}
			e.node.Mine(1)
			e.pass()
			if f, ok := e.fwd(c, 0); !ok || f.Code != codePaid || !e.rail(o).Paid || e.eventCount(o, store.EventPaid) != 1 {
				t.Fatalf("mined: %+v %v %+v", f, ok, e.events(o))
			}
			for _, id := range []string{a, b} {
				if _, ok := e.fwd(id, 0); ok {
					t.Fatalf("%s was recorded", id[:8])
				}
			}
			if e.eventCount(o, store.EventLate) != 0 {
				t.Fatalf("events %+v", e.events(o))
			}
			e.passes(2)
			e.safety(o)
		})
	}
}

// The rule reads the listing and the stored rows, never the marks: while the original's gettransaction fails (no dead
// mark for two passes), the replacement listed and stored inherits all the same; mended, the original is marked dead,
// and in the end there is one forward, the replacement's.
func TestBumpSeenBeforeTheOriginalIsDead(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	first := e.txo(a).FirstSeen
	e.pastExpiry(o)
	e.node.FailTx(a, &btcrpc.Error{Code: -5, Method: "gettransaction"})
	b := e.node.Replace(a, true)
	e.pass()
	e.pass()
	if m, _ := e.mark(a); m.Kind != store.MarkMempool {
		t.Fatalf("the original was marked: %+v", m)
	}
	if !e.txo(b).FirstSeen.Equal(first) {
		t.Fatalf("the bump's first-seen %v, the original's %v", e.txo(b).FirstSeen, first)
	}
	e.node.MendTx(a)
	e.pass()
	if m, _ := e.mark(a); m.Kind != store.MarkDead {
		t.Fatalf("the original: %+v", m)
	}
	e.node.Mine(1)
	e.pass()
	if f, ok := e.fwd(b, 0); !ok || f.Code != codePaid || !e.rail(o).Paid || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("mined: %+v %v %+v", f, ok, e.events(o))
	}
	if _, ok := e.fwd(a, 0); ok {
		t.Fatal("the original was recorded")
	}
	e.safety(o)
}

// Past the window, the dead original of a bump that inherited its first-seen (a bump inside the window, decision 9) is
// settled without the left-pool ALERT: the replacement carries the payment on and is still tracked. A bump that does
// not inherit (a lower amount: late, so the order gets order.expired at the window end, nothing counted) leaves the
// original's ALERT as before.
func TestInheritedBumpSilencesTheLeftPoolAlert(t *testing.T) {
	for _, sat := range []int64{100_000, 90_000} {
		t.Run(fmt.Sprint(sat), func(t *testing.T) {
			e := newWatchEnv(t)
			e.node.Mine(1) // a tip above the genesis: a settled fact needs a height of at least 1
			o := e.order(100_000)
			ad := e.addr(o)
			a := e.node.Pay(ad, 100_000)
			e.pass()
			e.pastExpiry(o)
			e.pass()
			// A minute before the window end: inside it, so a bump of the exact amount inherits (decision 9).
			e.clk.Advance(o.ExpiresAt.Add(store.WatchWindow).Add(-time.Minute).Sub(e.clk.Now()))
			b := e.node.ReplaceWith(a, []onchaintest.Out{{Address: ad, Sat: sat}})
			e.pass()
			if len(e.events(o)) != 0 || e.count(alertLeftPool) != 0 {
				t.Fatalf("inside the window: %+v\n%s", e.events(o), e.log)
			}
			e.toWindowEnd(o)
			e.pass()
			if want := map[bool]string{true: store.EventUnderpaid, false: store.EventExpired}[sat == 100_000]; !e.hasEvent(o, want, 1) {
				t.Fatalf("the window end: %+v, want %s", e.events(o), want)
			}
			if m, _ := e.mark(a); m.Kind != store.MarkDead || e.watched(a) {
				t.Fatalf("the original: %+v, watched %v", m, e.watched(a))
			}
			inherited := sat == 100_000
			if want := map[bool]int{true: 0, false: 1}[inherited]; e.count(alertLeftPool) != want {
				t.Fatalf("%d left-pool ALERTs, want %d\n%s", e.count(alertLeftPool), want, e.log)
			}
			if inherited != e.watched(b) {
				t.Fatalf("the bump watched %v", e.watched(b))
			}
			e.passes(2)
			if e.count(alertLeftPool) != map[bool]int{true: 0, false: 1}[inherited] {
				t.Fatalf("replayed:\n%s", e.log)
			}
			e.safety(o)
		})
	}
}

// When the clock steps back below an on-time original's stored first-seen, its bump inherits a time
// clamped to now, so the scan still commits (RecordScan refuses a first-seen after now, which would stop discovery
// for every order): another order's payment in the same scan is discovered, and the bump pays its order.
func TestBumpAfterAClockStepBackIsClamped(t *testing.T) {
	e := newWatchEnv(t)
	o, other := e.order(100_000), e.order(50_000)
	e.clk.Advance(20 * time.Minute)
	a := e.payOrder(o, 100_000)
	e.pass()
	first := e.txo(a).FirstSeen
	e.clk.Advance(-10 * time.Minute) // the host clock steps back
	b := e.node.Replace(a, true)
	c := e.payOrder(other, 50_000)
	if err := e.pass(); err != nil || e.count(logCallFailed) != 0 {
		t.Fatalf("the pass: %v\n%s", err, e.log)
	}
	x := e.txo(b)
	if !x.FirstSeen.Equal(e.clk.Now()) || !x.FirstSeen.Before(first) || !x.FirstSeen.Before(o.ExpiresAt) {
		t.Fatalf("the bump's first-seen %v, now %v, the original's %v", x.FirstSeen, e.clk.Now(), first)
	}
	if _, err := e.st.TXO(bg, store.TXOKey{TxID: c}); err != nil {
		t.Fatalf("the other order's payment was not discovered: %v", err)
	}
	e.node.Mine(1)
	e.pass()
	if f, ok := e.fwd(b, 0); !ok || f.Code != codePaid || !e.rail(o).Paid || !e.rail(other).Paid {
		t.Fatalf("mined: %+v %v", f, ok)
	}
	if _, ok := e.fwd(a, 0); ok {
		t.Fatal("the original was recorded")
	}
	e.safety(o, other)
}

// --- the gap alert ---

func TestGapAlert(t *testing.T) {
	t.Parallel() // slow on wall time; nothing shared but the order counter
	e := newWatchEnv(t)
	const gap = "ALERT: unused on-chain addresses exceed gap_alert; raise the merchant wallet's gap limit"
	var last store.Order
	add := func(i int64) {
		last = e.order(1_000)
		if err := e.st.AddAddress(bg, store.Address{OrderID: last.ID, Address: e.node.AddressAt(i), Index: i}); err != nil {
			t.Fatal(err)
		}
	}
	for i := int64(0); i < 999; i++ {
		add(i)
	}
	e.node.Update(func(s *onchaintest.State) { s.NextIndex = 1000 })
	e.pass()
	if e.count(gap) != 0 {
		t.Fatal("999 alerted")
	}
	add(999)
	e.pass()
	e.pass()
	if e.count(gap) != 1 { // once at the crossing, then at most every 24 h (TestGapAlertIsThrottled)
		t.Fatalf("%d gap lines", e.count(gap))
	}
	e.node.Pay(e.node.AddressAt(999), 1_000)
	e.clk.Advance(gapAlertEvery)
	e.pass()
	if e.count(gap) != 1 {
		t.Fatal("a payment to the last address did not stop the ALERT")
	}
	e.safety(last)
}

// --- timeouts, Run and pacing ---

func TestHangOnOneOutput(t *testing.T) {
	e := newWatchEnv(t)
	e.svc.callTimeout = 300 * time.Millisecond
	a, b, c := e.order(10_000), e.order(10_000), e.order(10_000)
	e.payOrder(a, 10_000)
	tb := e.payOrder(b, 10_000)
	e.payOrder(c, 10_000)
	e.pass()
	e.node.HangTx(tb)
	e.node.Mine(1)
	start := time.Now()
	e.pass()
	if d := time.Since(start); d < 300*time.Millisecond || d > 300*time.Millisecond+time.Second {
		t.Fatalf("the pass took %v", d)
	}
	if !e.rail(a).Paid || !e.rail(c).Paid || e.rail(b).Paid {
		t.Fatal("the other outputs were not refreshed")
	}
	if e.count("bitcoind call failed for an output") != 1 || e.count("ALERT: bitcoind call failed") != 0 || e.rail(a).Stale {
		t.Fatalf("a timeout is per output:\n%s", e.log)
	}
	e.node.UnhangTx(tb)
	e.pass()
	if !e.rail(b).Paid {
		t.Fatal("b")
	}
	e.safety(a, b, c)
}

func TestRunStopsWithItsContext(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	txid := e.payOrder(o, 10_000)
	e.node.HangTx(txid)
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
	if e.node.Calls("listsinceblock") < 1 {
		t.Fatal("Run made no pass")
	}
}

func TestRunPassesAtPollInterval(t *testing.T) {
	e := newEnv(t)
	e.mod = func(c *Config) { c.PollRate = MaxPollRate; c.PollInterval = MinPollInterval }
	e.svc = e.newService()
	e.ready()
	o := e.order(10_000)
	e.payOrder(o, 10_000)
	e.node.Mine(1)
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	deadline := time.Now().Add(2*MinPollInterval + time.Second)
	for time.Now().Before(deadline) && e.node.Calls("listsinceblock") < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if n := e.node.Calls("listsinceblock"); n < 2 || n > 3 {
		t.Fatalf("%d passes in about one PollInterval", n)
	}
	if !e.rail(o).Paid {
		t.Fatal("Run did not record the payment")
	}
	e.safety(o)
}

func TestPacing(t *testing.T) {
	t.Parallel() // slow on wall time; nothing shared but the order counter
	if testing.Short() {
		t.Skip("wall-clock pacing")
	}
	e := newEnv(t)
	e.mod = func(c *Config) { c.PollRate = 50 }
	e.svc = e.newService()
	e.ready()
	var orders []store.Order
	for i := int64(0); i < 100; i++ {
		o := e.order(1_000)
		if err := e.st.AddAddress(bg, store.Address{OrderID: o.ID, Address: e.node.AddressAt(i), Index: i}); err != nil {
			t.Fatal(err)
		}
		e.node.Pay(e.node.AddressAt(i), 1_000)
		orders = append(orders, o)
	}
	e.node.Update(func(s *onchaintest.State) { s.NextIndex = 100 })
	for p := 1; p <= 2; p++ {
		before := e.node.Calls("listsinceblock")
		start := time.Now()
		e.pass()
		if d := time.Since(start); d < 2*time.Second {
			t.Fatalf("pass %d took %v for 100 outputs at 50/s", p, d)
		}
		if e.node.Calls("listsinceblock") != before+1 {
			t.Fatal("listsinceblock more than once per pass")
		}
	}
	e.safety(orders...)
}

// --- the cursor ---

// The first scan starts at the block Check saw as the tip, never at the genesis; later scans start from the stored
// lastblock, which lags the tip by SettledDepth blocks.
func TestCursorStartsAtCheckTip(t *testing.T) {
	e := newEnv(t)
	e.mod = func(c *Config) { c.PollRate = MaxPollRate }
	e.node.Mine(20)
	e.svc = e.newService()
	e.ready()
	c, ok, err := e.st.Cursor(bg)
	if err != nil || !ok || c.BlockHash != e.node.Tip() || c.Height != 20 {
		t.Fatalf("Check's cursor: %+v %v %v", c, ok, err)
	}
	rec := &recordLSB{RPC: e.node}
	e.svc.rpc = rec
	e.pass()
	if rec.hashes[0] != e.node.BlockHash(20) || rec.targets[0] != SettledDepth(e.svc.cfg.Tiers)+1 {
		t.Fatalf("first call: %v %v", rec.hashes, rec.targets)
	}
	e.node.Mine(10)
	e.pass()
	e.pass()
	if rec.hashes[2] != e.node.BlockHash(30-6) {
		t.Fatalf("the cursor does not lag the tip by SettledDepth: %v", rec.hashes)
	}
	if c, _, _ := e.st.Cursor(bg); c.Height != 30 {
		t.Fatalf("the cursor's height is not the tip: %+v", c)
	}
	// A second Check does not move a stored cursor.
	e.ready()
	if c2, _, _ := e.st.Cursor(bg); c2.BlockHash != e.node.BlockHash(24) {
		t.Fatalf("%+v", c2)
	}
}

type recordLSB struct {
	RPC
	hashes  []string
	targets []int
}

func (r *recordLSB) ListSinceBlock(ctx context.Context, hash string, target int) (btcrpc.SinceBlock, error) {
	r.hashes = append(r.hashes, hash)
	r.targets = append(r.targets, target)
	return r.RPC.ListSinceBlock(ctx, hash, target)
}

// Stale reads the last good pass, not the cursor: a quiet node writes no cursor row.
func TestStaleFollowsTheLastGoodPass(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	if !e.rail(o).Stale {
		t.Fatal("not stale before any pass")
	}
	e.pass()
	for i := 0; i < 10; i++ {
		e.clk.Advance(e.svc.cfg.PollInterval)
		e.pass()
		if e.rail(o).Stale {
			t.Fatal("stale on a quiet node")
		}
	}
	e.clk.Advance(3*e.svc.cfg.PollInterval + time.Second)
	if !e.rail(o).Stale {
		t.Fatal("not stale after 3 intervals")
	}
	if r, err := e.svc.Rail(bg, store.Order{ID: o.ID, AmountMsat: 1}); !errors.Is(err, ErrNoOnchain) {
		t.Fatalf("%+v %v", r, err)
	}
	if r := e.rail(o); r.Issued || r.Address != "" || r.State != "waiting" {
		t.Fatalf("an order with no address: %+v", r)
	}
	e.clk.Advance(time.Hour)
	// expired comes from order.expired, which the next pass writes.
	if r := e.rail(o); r.State != "waiting" {
		t.Fatalf("past the expiry before a pass: %+v", r)
	}
	e.pass()
	if r := e.rail(o); r.State != "expired" || !e.hasEvent(o, store.EventExpired, 1) {
		t.Fatalf("%+v %+v", r, e.events(o))
	}
	e.safety(o)
}

// After a restart, an output already settled (recorded SettledDepth deep against the stored cursor) goes to
// final without a gettransaction; only the unsettled one is polled.
func TestRestartPollsOnlyUnsettledOutputs(t *testing.T) {
	e := newWatchEnv(t)
	a, b := e.order(10_000), e.order(20_000)
	ta := e.payOrder(a, 10_000)
	e.node.Mine(6)
	e.pass() // a recorded 6 deep: final in this process
	tb := e.payOrder(b, 20_000)
	e.node.Mine(1)
	e.pass() // b recorded at 1 confirmation, still tracked
	if !e.rail(a).Paid || !e.rail(b).Paid {
		t.Fatal("setup")
	}
	e.restart()
	ca, cb := e.node.TxCalls(ta), e.node.TxCalls(tb)
	e.pass()
	if got := e.node.TxCalls(ta) - ca; got != 0 {
		t.Errorf("the settled output was polled %d times after the restart", got)
	}
	if got := e.node.TxCalls(tb) - cb; got != 1 {
		t.Errorf("the unsettled output was polled %d times, want 1", got)
	}
	e.passes(2)
	if got := e.node.TxCalls(ta) - ca; got != 0 {
		t.Errorf("the settled output was polled %d times in later passes", got)
	}
	if !e.rail(a).Paid || !e.rail(b).Paid || e.eventCount(a, store.EventPaid) != 1 || e.eventCount(b, store.EventPaid) != 1 {
		t.Fatalf("%+v %+v", e.events(a), e.events(b))
	}
	e.safety(a, b)
}

// The restart rule reads the cursor stored before the pass's scan, the last height a process verified the marks
// against, never the new tip: a reorg that took a recorded output away while no process ran (here it is reorged out
// and evicted, and the chain grows past SettledDepth) is still caught by one gettransaction and reversed.
func TestRestartAfterADowntimeReorgPollsTheOutput(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000) // tier 1
	txid := e.payOrder(o, 10_000)
	e.node.Mine(1)
	e.pass()
	if !e.rail(o).Paid {
		t.Fatal("setup")
	}
	e.node.Invalidate(1)
	e.node.Evict(txid)
	e.node.Mine(10)
	e.restart()
	c := e.node.TxCalls(txid)
	e.pass()
	if got := e.node.TxCalls(txid) - c; got != 2 { // polled once: an evicted output is read twice before its dead mark
		t.Fatalf("%d gettransaction calls after the restart, want 2", got)
	}
	if _, ok := e.rev(txid, 0); !ok || e.rail(o).Paid || !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("the downtime reorg was not reversed: %+v %+v", e.rail(o), e.events(o))
	}
	e.safety(o)
}

// An output is settled for good only against a mark verified at or after the cursor's height.
// Here the cursor moves SettledDepth past the output's mark while the output is not refreshed (its gettransaction
// fails, or the pass ends at another output first). Then a reorg takes it away: it is reorged out, evicted, and the
// chain grows. The next pass, in this process or after a restart, must poll it and reverse the record.
func TestUnrefreshedOutputIsNotSettled(t *testing.T) {
	for _, c := range []struct {
		name    string
		restart bool
		fail    func(e *env, a, b string) // makes the pass skip a, or end before it
	}{
		{"skipped", false, func(e *env, a, b string) { e.node.FailTx(a, &btcrpc.Error{Code: -5, Method: "gettransaction"}) }},
		{"skipped, restart", true, func(e *env, a, b string) { e.node.FailTx(a, &btcrpc.Error{Code: -5, Method: "gettransaction"}) }},
		{"pass ended, restart", true, func(e *env, a, b string) { e.node.FailTx(b, errOutage) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newWatchEnv(t)
			ob, oa := e.order(20_000), e.order(10_000) // tier 1 both; b is first seen first, so tracked first
			tb := e.payOrder(ob, 20_000)
			e.clk.Advance(time.Second)
			ta := e.payOrder(oa, 10_000)
			e.node.Mine(1)
			e.pass() // both recorded at 1 confirmation
			if !e.rail(oa).Paid || !e.rail(ob).Paid {
				t.Fatal("setup")
			}
			e.node.Mine(5) // a's mark is now SettledDepth below the tip
			c.fail(e, ta, tb)
			e.clk.Advance(time.Second)
			e.pass() // the cursor moves to the tip; a's mark is not refreshed
			e.node.MendTx(ta)
			e.node.MendTx(tb)
			e.node.Invalidate(6)
			e.node.Evict(ta)
			e.node.Mine(10)
			if c.restart {
				e.restart()
			}
			n := e.node.TxCalls(ta)
			e.clk.Advance(time.Second)
			e.pass()
			if got := e.node.TxCalls(ta) - n; got != 2 { // polled once: evicted, it is read twice before its dead mark
				t.Fatalf("a: %d gettransaction calls, want 2", got)
			}
			if _, ok := e.rev(ta, 0); !ok || e.rail(oa).Paid || !e.hasEvent(oa, store.EventReverted, 1) {
				t.Fatalf("not reversed: %+v %+v", e.rail(oa), e.events(oa))
			}
			e.safety(oa, ob)
		})
	}
}

// The top tier needs SettledDepth confirmations, so "mined at its tier" and "settled" are the same height. A crash
// between the forward and order.paid, then a restart: the pass that crashed recorded no settled fact, so the output
// is polled once more, its order is settled, and order.paid comes at once (not at the expiry).
func TestCrashAtTheTopTierThenRestartAddsPaid(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(20_000_000) // above 10,000,000 sat: 6 confirmations
	txid := e.payOrder(o, 20_000_000)
	e.node.Mine(6)
	e.crashAt("settle:after-transfers")
	if err := e.pass(); !errors.Is(err, errCrash) {
		t.Fatalf("no crash: %v", err)
	}
	if _, ok := e.fwd(txid, 0); !ok || e.eventCount(o, store.EventPaid) != 0 {
		t.Fatal("setup: want the forward and no order.paid")
	}
	e.restart()
	c := e.node.TxCalls(txid)
	e.pass()
	if e.eventCount(o, store.EventPaid) != 1 || !e.rail(o).Paid {
		t.Fatalf("order.paid after the restart: %+v", e.events(o))
	}
	if got := e.node.TxCalls(txid) - c; got != 1 {
		t.Errorf("polled %d times, want 1", got)
	}
	c = e.node.TxCalls(txid)
	e.passes(2) // settled now: not polled again
	if got := e.node.TxCalls(txid) - c; got != 0 {
		t.Errorf("polled %d times after it settled", got)
	}
	e.safety(o)
}

// Stale allows 3 poll intervals plus the last completed pass's duration, so a long healthy pass (many
// tracked outputs at poll_rate) does not read stale while the next one runs; no pass for that long does.
func TestStaleScalesWithTheLastPass(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	pi := e.svc.cfg.PollInterval
	slow := 5 * pi // longer than 3 poll intervals
	e.svc.hook = func(p string) error {
		if p == "scan:after-record" {
			e.clk.Advance(slow)
		}
		return nil
	}
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	e.svc.hook = nil
	if e.rail(o).Stale {
		t.Fatal("stale right after a good pass")
	}
	e.clk.Advance(slow) // the next pass, as slow, is still running
	if e.rail(o).Stale {
		t.Fatal("stale while a pass as long as the last one runs")
	}
	e.clk.Advance(3 * pi) // exactly 3 intervals + the last pass's duration: not yet
	if e.rail(o).Stale {
		t.Fatal("stale at the bound")
	}
	e.clk.Advance(time.Second)
	if !e.rail(o).Stale {
		t.Fatal("not stale past 3 intervals + the last pass's duration")
	}
	// A fast pass brings the bound back to 3 intervals.
	e.pass()
	e.clk.Advance(3*pi + time.Second)
	if !e.rail(o).Stale {
		t.Fatal("the bound did not follow the last (fast) pass")
	}
}

// sortedSeqs is the seqs of the order's events of a type.
func (e *env) sortedSeqs(o store.Order, typ string) []int {
	var l []int
	for _, ev := range e.events(o) {
		if ev.Type == typ {
			l = append(l, ev.Seq)
		}
	}
	sort.Ints(l)
	return l
}

// The lag ALERT: one line per pass while it lasts, also when step 0's Check (which logs it itself) runs.
func TestLagAlertOncePerPass(t *testing.T) {
	e := newWatchEnv(t)
	base := e.count(logIBD)
	e.node.Update(func(s *onchaintest.State) { s.HeadersAhead = 5 })
	e.passes(3)
	if got := e.count(logIBD) - base; got != 3 {
		t.Fatalf("%d lag lines in 3 passes", got)
	}
	e.svc.behind.Store(true)
	e.passes(2)
	if got := e.count(logIBD) - base; got != 5 {
		t.Fatalf("%d lag lines in 5 passes", got)
	}
	e.node.Update(func(s *onchaintest.State) { s.HeadersAhead = 0 })
	e.passes(2)
	if got := e.count(logIBD) - base; got != 5 {
		t.Fatalf("the ALERT outlasted the lag: %d", got)
	}
}

// A stored output that bitcoind lists with another amount or address is skipped with the bad-transaction
// ALERT; discovery goes on for the others, and the stored row is unchanged.
func TestRelistedDifferentlyDoesNotWedge(t *testing.T) {
	for name, f := range map[string]func(*btcrpc.WalletTx){
		"amount":  func(w *btcrpc.WalletTx) { w.AmountSat++ },
		"address": func(w *btcrpc.WalletTx) { w.Address = "" },
	} {
		t.Run(name, func(t *testing.T) {
			e := newWatchEnv(t)
			a, b, c := e.order(10_000), e.order(20_000), e.order(30_000)
			addrA, addrC := e.addr(a), e.addr(c)
			ta := e.node.Pay(addrA, 10_000)
			e.node.Mine(7)
			e.pass()
			e.pass() // a is now below the cursor: bitcoind no longer lists it, so only the odd entry names it
			if !e.rail(a).Paid {
				t.Fatal("setup")
			}
			bad := btcrpc.WalletTx{Category: "receive", Address: addrA, AmountSat: 10_000, TxID: ta, Vout: 0}
			f(&bad)
			if bad.Address == "" {
				bad.Address = addrC // another issued address
			}
			e.svc.rpc = &inject{RPC: e.node, extra: []btcrpc.WalletTx{bad}}
			tb := e.payOrder(b, 20_000)
			e.pass()
			e.pass()
			if l := e.txos(b); len(l) != 1 || l[0].TxID != tb {
				t.Fatalf("discovery stopped: %+v\n%s", l, e.log)
			}
			if x, err := e.st.TXO(bg, store.TXOKey{TxID: ta}); err != nil || x.AmountSat != 10_000 || x.Address != addrA {
				t.Fatalf("the stored row: %+v %v", x, err)
			}
			if e.count("ALERT: bitcoind call failed") != 0 || e.count(logBadTx) != 2 || e.rail(b).Stale {
				t.Fatalf("lines:\n%s", e.log)
			}
			e.node.Mine(1)
			e.pass()
			if !e.rail(a).Paid || !e.rail(b).Paid || len(e.txos(c)) != 0 {
				t.Fatalf("%+v %+v", e.rail(a), e.rail(b))
			}
			e.safety(a, b, c)
		})
	}
}

// failFrom fails listsinceblock with err when it is asked from one block hash.
type failFrom struct {
	RPC
	hash string
	err  error
}

func (f *failFrom) ListSinceBlock(ctx context.Context, hash string, target int) (btcrpc.SinceBlock, error) {
	if hash == f.hash {
		return btcrpc.SinceBlock{}, f.err
	}
	return f.RPC.ListSinceBlock(ctx, hash, target)
}

// Ruling R: a cursor block bitcoind no longer knows (-5, "Block not found") re-anchors the scan at the
// tip with its own ALERT. Tracked outputs keep refreshing; an output first mined between the lost block and the
// re-anchor (deeper than the lag) is the named loss; new payments are discovered again.
func TestUnknownCursorBlockReanchors(t *testing.T) {
	e := newWatchEnv(t)
	a, b, d := e.order(10_000), e.order(20_000), e.order(30_000)
	addrB, addrD := e.addr(b), e.addr(d)
	ta := e.payOrder(a, 10_000)
	e.pass() // a is tracked
	if _, err := e.st.RecordScan(bg, nil, store.Cursor{BlockHash: strings.Repeat("ee", 32), Height: e.node.Height()}); err != nil {
		t.Fatal(err)
	}
	td := e.node.Pay(addrD, 30_000)
	e.node.Mine(10) // a and d mined 10 deep
	tb := e.node.Pay(addrB, 20_000)
	e.passes(3)
	if e.count(logReanchor) != 1 || e.count("ALERT: bitcoind call failed") != 0 || e.rail(a).Stale {
		t.Fatalf("lines:\n%s", e.log)
	}
	if _, ok := e.fwd(ta, 0); !ok || !e.rail(a).Paid {
		t.Fatal("a tracked output stopped refreshing")
	}
	if l := e.txos(b); len(l) != 1 || l[0].TxID != tb {
		t.Fatalf("a new payment after the re-anchor: %+v", l)
	}
	if _, err := e.st.TXO(bg, store.TXOKey{TxID: td}); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the lost window was scanned after all; the ALERT's loss is wrong")
	}
	e.safety(a, b, d)
}

// The pruned-node read error of include_removed (transactions.cpp:663-665) re-anchors the same way.
func TestPrunedCursorBlockReanchors(t *testing.T) {
	e := newWatchEnv(t)
	a := e.order(10_000)
	e.node.Mine(20) // long enough that the re-anchored lastblock is another block than the failing one
	e.pass()
	cur, _, _ := e.st.Cursor(bg)
	e.svc.rpc = &failFrom{RPC: e.node, hash: cur.BlockHash, err: &btcrpc.Error{Code: -1, Method: "listsinceblock"}}
	e.node.Mine(1)
	ta := e.payOrder(a, 10_000)
	e.passes(2)
	if e.count(logReanchor) != 1 || e.count("ALERT: bitcoind call failed") != 0 {
		t.Fatalf("lines:\n%s", e.log)
	}
	if l := e.txos(a); len(l) != 1 || l[0].TxID != ta {
		t.Fatalf("%+v", l)
	}
	e.safety(a)
}

// An answer too large to read (ErrBadAnswer, over 8 MiB after a long outage on a busy wallet) from the
// stored cursor would end every pass for good; it re-anchors like a JSON-RPC error, with the same one ALERT.
func TestOversizeAnswerReanchors(t *testing.T) {
	e := newWatchEnv(t)
	a := e.order(10_000)
	e.node.Mine(20)
	e.pass()
	cur, _, _ := e.st.Cursor(bg)
	e.svc.rpc = &failFrom{RPC: e.node, hash: cur.BlockHash, err: fmt.Errorf("%w: listsinceblock: answer over 8 MiB", btcrpc.ErrBadAnswer)}
	e.node.Mine(1)
	ta := e.payOrder(a, 10_000)
	e.passes(2)
	if e.count(logReanchor) != 1 || e.count("ALERT: bitcoind call failed") != 0 {
		t.Fatalf("lines:\n%s", e.log)
	}
	if l := e.txos(a); len(l) != 1 || l[0].TxID != ta {
		t.Fatalf("discovery after the re-anchor: %+v", l)
	}
	if c, _, _ := e.st.Cursor(bg); c.BlockHash == cur.BlockHash {
		t.Fatal("the cursor did not move")
	}
	e.safety(a)
}

// A re-anchor that fails too ends the pass with the call-failed ALERT; a transport error, the call's own timeout
// included, is an outage, not a lost cursor, and never re-anchors.
func TestReanchorFailureAndTransportError(t *testing.T) {
	e := newWatchEnv(t)
	e.node.Break("listsinceblock", &btcrpc.Error{Code: -1, Method: "listsinceblock"})
	e.pass()
	if e.count(logReanchor) != 1 || e.count("ALERT: bitcoind call failed") != 1 {
		t.Fatalf("an error on the re-anchor itself:\n%s", e.log)
	}
	e.node.Break("listsinceblock", errOutage)
	e.pass()
	if e.count(logReanchor) != 1 || e.count("ALERT: bitcoind call failed") != 2 {
		t.Fatalf("a transport error re-anchored:\n%s", e.log)
	}
	e.node.Break("listsinceblock", fmt.Errorf("%w: listsinceblock %w", btcrpc.ErrTransport, context.DeadlineExceeded))
	e.pass()
	if e.count(logReanchor) != 1 || e.count("ALERT: bitcoind call failed") != 3 {
		t.Fatalf("a timeout re-anchored:\n%s", e.log)
	}
}

// A stop while the index is behind, landing in one of Check's store calls (here the context is
// already cancelled and the fake node ignores it, so Check's calls answer and its Cursor read fails), is no failed
// check: the pass returns the context's error and logs no logCheckFailed ALERT, nor any other failure line.
func TestStopDuringStepZeroLogsNoCheckFailed(t *testing.T) {
	e := newWatchEnv(t)
	e.svc.behind.Store(true)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if err := e.svc.pass(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pass: %v, want the context's error", err)
	}
	for _, l := range []string{logCheckFailed, logCallFailed, logScanFailed, logTrackFailed, logSettleFailed} {
		if e.count(l) != 0 {
			t.Fatalf("%q on a stop:\n%s", l, e.log)
		}
	}
}

// cancelAfterList cancels the pass's context right after listsinceblock answers.
type cancelAfterList struct {
	RPC
	cancel func()
}

func (c *cancelAfterList) ListSinceBlock(ctx context.Context, hash string, target int) (btcrpc.SinceBlock, error) {
	sb, err := c.RPC.ListSinceBlock(ctx, hash, target)
	c.cancel()
	return sb, err
}

// A stop that lands between listsinceblock and RecordScan is no failed scan: the
// pass returns the context's error and logs no logScanFailed. The listing is empty, so keep makes no store call and
// RecordScan is the first to see the cancelled context. The same with a listing: keep's store read sees it.
func TestStopBeforeRecordScanLogsNoScanFailed(t *testing.T) {
	for _, paid := range []bool{false, true} {
		t.Run(fmt.Sprint(paid), func(t *testing.T) {
			e := newWatchEnv(t)
			if paid {
				e.payOrder(e.order(10_000), 10_000)
			}
			ctx, cancel := context.WithCancel(bg)
			e.svc.rpc = &cancelAfterList{RPC: e.node, cancel: cancel}
			if err := e.svc.pass(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("pass: %v, want the context's error", err)
			}
			if e.count(logScanFailed) != 0 || e.count(logCallFailed) != 0 {
				t.Fatalf("a failure line on a stop:\n%s", e.log)
			}
		})
	}
}

// afterTx runs fn right after gettransaction answers: the point between step 3's read of an output and its first
// store call (CurrentMark).
type afterTx struct {
	RPC
	fn func()
}

func (a *afterTx) GetTransaction(ctx context.Context, txid string) (btcrpc.Transaction, error) {
	tx, err := a.RPC.GetTransaction(ctx, txid)
	a.fn()
	return tx, err
}

// failureLines are the failure lines a stop must not log.
var failureLines = []string{logCheckFailed, logCallFailed, logScanFailed, logTrackFailed, logSettleFailed, logOutputFailed}

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

// A stop that lands in one of step 3's store calls on an output (CurrentMark, then AddMark) is no failed track: the
// pass returns the context's error, logs no failure line, settles nothing and publishes no observation. The reverse,
// the same store failure with a live context, logs logTrackFailed once, with the site's word.
func TestStopDuringTrackLogsNoTrackFailed(t *testing.T) {
	sites := []struct {
		name  string
		stop  func(e *env, cancel func())
		fault func(e *env)
	}{
		{"mark-read",
			func(e *env, cancel func()) { e.svc.rpc = &afterTx{RPC: e.node, fn: cancel} },
			func(e *env) {
				e.svc.rpc = &afterTx{RPC: e.node, fn: func() { e.execDB(`ALTER TABLE onchain_marks RENAME TO marks_x`) }}
			}},
		{"mark-write",
			func(e *env, cancel func()) {
				e.onceAt("track:before-mempool", cancel)
			},
			func(e *env) {
				e.execDB(`CREATE TRIGGER marks_fail BEFORE INSERT ON onchain_marks BEGIN SELECT RAISE(ABORT, 'x'); END`)
			}},
	}
	for _, c := range sites {
		t.Run(c.name+"/stop", func(t *testing.T) {
			e := newWatchEnv(t)
			o := e.order(10_000)
			e.payOrder(o, 10_000)
			// A sentinel in the published observations: a pass that went on past the stop would replace the map.
			sent := map[store.TXOKey]observed{{TxID: "sentinel"}: {height: 7}}
			e.svc.seen = sent
			ctx, cancel := context.WithCancel(bg)
			c.stop(e, cancel)
			if err := e.svc.pass(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("pass: %v, want the context's error", err)
			}
			for _, l := range failureLines {
				if e.count(l) != 0 {
					t.Fatalf("%q on a stop:\n%s", l, e.log)
				}
			}
			e.svc.seenMu.Lock()
			same := fmt.Sprintf("%p", e.svc.seen) == fmt.Sprintf("%p", sent) && len(sent) == 1 && sent[store.TXOKey{TxID: "sentinel"}].height == 7
			e.svc.seenMu.Unlock()
			if !same || e.rail(o).Paid || len(e.events(o)) != 0 {
				t.Fatalf("a stopped pass changed something: %+v", e.rail(o))
			}
		})
		t.Run(c.name+"/failure", func(t *testing.T) {
			e := newWatchEnv(t)
			e.payOrder(e.order(10_000), 10_000)
			c.fault(e)
			e.pass()
			if e.count(logTrackFailed) != 1 || !e.logHas(`msg="`+logTrackFailed+`" at=`+c.name+"\n") {
				t.Fatalf("want one %q line at %s:\n%s", logTrackFailed, c.name, e.log)
			}
		})
	}
}

// The record of step 3's first read names its site with `at`, and carries no other attribute. mark-read and
// mark-write are pinned by TestStopDuringTrackLogsNoTrackFailed. The addresses and order sites cannot fail on their
// own through the store (WatchedTXOs joins both tables, so it fails first), and views is unwatched's, which a
// failing store reaches only past a window's end: their words are read from the code.
func TestTrackFailedNamesItsSite(t *testing.T) {
	for _, c := range []struct{ name, table string }{
		{"watched", "onchain_txos"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newWatchEnv(t)
			e.payOrder(e.order(10_000), 10_000)
			e.pass()
			e.node.Mine(1)
			e.onceAt("scan:after-record", func() { e.execDB(`ALTER TABLE ` + c.table + ` RENAME TO tbl_x`) })
			e.pass()
			if e.count(logTrackFailed) != 1 || !e.logHas(`msg="`+logTrackFailed+`" at=`+c.name+"\n") {
				t.Fatalf("want one %q line at %s:\n%s", logTrackFailed, c.name, e.log)
			}
		})
	}
}

// A step-0 Check failure other than want of an answer (here the wallet changed) is logged once per pass, and the
// pass goes on watching.
func TestCheckFailureIsLoggedInThePass(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	e.payOrder(o, 10_000)
	e.svc.behind.Store(true)
	e.node.Update(func(s *onchaintest.State) { s.PrivateKeysEnabled = true })
	e.node.Mine(1)
	e.passes(2)
	if e.count(logCheckFailed) != 2 || e.count("ALERT: bitcoind call failed") != 0 {
		t.Fatalf("lines:\n%s", e.log)
	}
	if !strings.Contains(e.log.String(), "reason=wallet") || !e.rail(o).Paid || e.rail(o).Stale {
		t.Fatalf("%+v\n%s", e.rail(o), e.log)
	}
	// The index refusal has its own line from Check (logBehind), not a second one here.
	e.node.Update(func(s *onchaintest.State) { s.PrivateKeysEnabled = false; s.NextIndex = 0 })
	e.pass()
	if e.count(logCheckFailed) != 2 || e.count(logBehind) < 1 {
		t.Fatalf("lines:\n%s", e.log)
	}
	e.safety(o)
}

// TestLogReanchorText pins logReanchor's exact text: operators match on it, and it is written as concatenated
// constants to keep the lines short.
func TestLogReanchorText(t *testing.T) {
	const want = "ALERT: bitcoind cannot list from the scan's cursor block; the scan is retried from the tip: if that " +
		"succeeds, payments first mined between that block and the tip are not discovered (tracked payments still refresh)"
	if logReanchor != want {
		t.Fatalf("logReanchor = %q", logReanchor)
	}
}
