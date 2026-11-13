package monero

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

// livePeer is the on-chain rail as a peer whose view a test changes; onView, when set, runs after View has read its
// answer and before it returns it, with the call's number (1 for the first call after it was set).
type livePeer struct {
	mu     sync.Mutex
	view   rail.View
	ok     bool
	onView func(n int)
	calls  int
	seqs   []int // what RevertedSeqs answers: the seqs of the peer's reversals
}

// RevertedSeqs answers the seqs a test set (setSeqs).
func (p *livePeer) RevertedSeqs(context.Context, store.Order) ([]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seqs, nil
}

func (p *livePeer) setSeqs(seqs ...int) {
	p.mu.Lock()
	p.seqs = seqs
	p.mu.Unlock()
}

func (p *livePeer) View(context.Context, store.Order) (rail.View, bool, error) {
	p.mu.Lock()
	v, ok := p.view, p.ok
	p.calls++
	n, f := p.calls, p.onView
	p.mu.Unlock()
	if f != nil {
		f(n)
	}
	return v, ok, nil
}

func (p *livePeer) set(v rail.View) {
	p.mu.Lock()
	p.view, p.ok = v, true
	p.mu.Unlock()
}

func (p *livePeer) setOnView(f func(n int)) {
	p.mu.Lock()
	p.onView, p.calls = f, 0
	p.mu.Unlock()
}

// --- the confirmation tiers: wallet-rpc's count (its height less the block's), 1 / 3 / 10 by default ---

func TestTiers(t *testing.T) {
	e := newWatchEnv(t, zeroTiers)
	tiny, edge := e.order(xmr/100), e.order(xmr/100+1)
	small, mid, big := e.order(xmr/2), e.order(5*xmr), e.order(50*xmr)
	for _, o := range []store.Order{tiny, edge, small, mid, big} {
		e.payOrder(o, o.AmountPiconero)
	}
	e.pass()
	if v := e.view(tiny); !v.Paid || v.State != rail.Paid || v.Confirmations != 0 || v.RequiredConfirmations != 0 {
		t.Fatalf("0.01 XMR in the pool: %+v", v)
	}
	for _, o := range []store.Order{edge, small, mid, big} {
		if v := e.view(o); v.State != rail.Seen || v.Confirmations != 0 || v.Received != o.AmountPiconero || v.Paid {
			t.Fatalf("in the pool: %+v", v)
		}
	}
	e.w.Mine(1)
	e.pass()
	if !e.view(edge).Paid || !e.view(small).Paid || e.view(mid).Paid || e.view(big).Paid {
		t.Fatal("after 1 block")
	}
	e.w.Mine(1)
	e.pass()
	if v := e.view(mid); v.Paid || v.Confirmations != 2 {
		t.Fatalf("a tier-3 payment after 2 blocks: %+v", v)
	}
	e.w.Mine(1)
	e.pass()
	if !e.view(mid).Paid || e.view(big).Paid {
		t.Fatal("after 3 blocks")
	}
	e.w.Mine(6)
	e.pass()
	if v := e.view(big); v.Paid || v.State != rail.Seen || v.Confirmations != 9 || v.RequiredConfirmations != 10 {
		t.Fatalf("after 9 blocks: %+v", v)
	}
	e.w.Mine(1)
	e.pass()
	if v := e.view(big); !v.Paid || v.State != rail.Paid || v.Confirmations != 10 {
		t.Fatalf("after 10 blocks: %+v", v)
	}
	if e.eventCount(tiny, store.EventPaid) != 1 {
		t.Fatalf("0.01 XMR: %+v", e.events(tiny))
	}
	e.safety(tiny, edge, small, mid, big)
}

// The default tiers at their bounds, block by block: an order needing N confirmations is not paid after N-1 blocks
// and is after N, and Confirmations is the blocks mined (wallet-rpc's count, wallet_rpc_server.cpp:163-168).
func TestTierBoundaries(t *testing.T) {
	e := newWatchEnv(t, nil)
	want := map[int64]int{xmr: 1, xmr + 1: 3, 10 * xmr: 3, 10*xmr + 1: 10}
	orders := map[int64]store.Order{}
	for pico := range want {
		orders[pico] = e.order(pico)
		e.payOrder(orders[pico], pico)
	}
	for blocks := 0; blocks <= 10; blocks++ {
		if blocks > 0 {
			e.w.Mine(1)
		}
		e.pass()
		for pico, req := range want {
			v := e.view(orders[pico])
			if v.RequiredConfirmations != req || v.Paid != (blocks >= req) || v.Confirmations != blocks {
				t.Fatalf("%d piconero after %d blocks: %+v", pico, blocks, v)
			}
		}
	}
	var all []store.Order
	for _, o := range orders {
		all = append(all, o)
	}
	e.safety(all...)
}

// A 0 tier: paid from the pool, polled until SettledDepth deep, recorded once.
func TestZeroConfPaidFromThePool(t *testing.T) {
	e := newWatchEnv(t, zeroTiers)
	o := e.order(xmr / 200)
	txid := e.payOrder(o, xmr/200)
	e.pass()
	if f, ok := e.fwd(o, txid, 0); !ok || f.Code != codePaid {
		t.Fatalf("no forward at epoch 0: %+v %v", f, ok)
	}
	if v := e.view(o); !v.Paid || v.State != rail.Paid || e.eventCount(o, store.EventPaid) != 1 || !e.watched(o, txid) {
		t.Fatalf("after one pass: %+v %+v", v, e.events(o))
	}
	e.w.Mine(1)
	e.pass()
	if _, ok := e.fwd(o, txid, 1); ok || !e.watched(o, txid) {
		t.Fatal("after 1 block: a second forward, or settled below SettledDepth")
	}
	e.w.Mine(9)
	e.pass()
	if e.watched(o, txid) || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("after 10 blocks: still watched, or %+v", e.events(o))
	}
	e.safety(o)
}

// --- reorgs ---

// A reorg after paid: the block popped, the payment back in the pool: a reversal and order.reverted 1; mined again:
// order.paid_again 1.
func TestReorgAfterPaid(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Mine(1)
	e.pass()
	if !e.view(o).Paid || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("setup: %+v", e.view(o))
	}
	e.w.Pop(1)
	e.pass()
	if r, ok := e.rev(o, txid, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || e.view(o).Paid ||
		e.count("ALERT: a Monero payment was reverted by a reorg or a double spend") != 1 {
		t.Fatalf("after the pop: %+v %+v\n%s", e.view(o), e.events(o), e.log)
	}
	if v := e.view(o); v.State != rail.Seen || v.Received != xmr {
		t.Fatalf("the payment back in the pool: %+v", v)
	}
	e.w.Mine(1)
	e.pass()
	if _, ok := e.fwd(o, txid, 1); !ok || !e.hasEvent(o, store.EventPaidAgain, 1) || !e.view(o).Paid ||
		e.count("ALERT: a reverted Monero payment confirmed again") != 1 {
		t.Fatalf("mined again: %+v %+v", e.view(o), e.events(o))
	}
	e.passes(3)
	if e.eventCount(o, store.EventReverted) != 1 || e.eventCount(o, store.EventPaidAgain) != 1 {
		t.Fatalf("replayed: %+v", e.events(o))
	}
	e.safety(o)
}

// A popped block whose transaction vanishes (PopDrop): while the wallet is below the cursor (the chain not yet
// regrown) it is catching up, and nothing is marked dead; once a competing block is mined, the payment is dead and its
// record reversed.
func TestPopDropIsDead(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Mine(1)
	e.pass()
	e.w.PopDrop(1)
	e.pass()
	if m, _ := e.mark(o, txid); m.Kind != store.MarkMined || !e.view(o).Paid || e.count(logHeightBehind) != 1 {
		t.Fatalf("below the cursor: mark %+v, %+v", m, e.view(o))
	}
	e.w.Mine(1)
	e.pass()
	if m, _ := e.mark(o, txid); m.Kind != store.MarkDead {
		t.Fatalf("mark %+v", m)
	}
	if _, ok := e.rev(o, txid, 0); !ok || !e.hasEvent(o, store.EventReverted, 1) || e.view(o).Paid || e.view(o).Received != 0 {
		t.Fatalf("%+v %+v", e.view(o), e.events(o))
	}
	e.pastExpiry(o)
	e.pass()
	if e.eventCount(o, store.EventExpired) != 0 || e.eventCount(o, store.EventUnderpaid) != 0 {
		t.Fatalf("a final event after order.paid: %+v", e.events(o))
	}
	e.safety(o)
}

// A pop followed by two blocks before a pass re-mines the transaction at the same height: no new kind or height (the
// new mark only carries wallet-rpc's higher count, ruling 8-A6), no event.
func TestPopThenMineBeforeAPass(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Mine(1)
	e.pass()
	m1, _ := e.mark(o, txid)
	e.w.Pop(1)
	e.w.Mine(2)
	e.pass()
	if m2, _ := e.mark(o, txid); m2.Kind != m1.Kind || m2.Height != m1.Height || m2.Confirmations != 2 || len(e.events(o)) != 1 {
		t.Fatalf("marks %+v → %+v, events %+v", m1, m2, e.events(o))
	}
	e.safety(o)
}

// --- partial, over and late payments ---

func TestPartialPayments(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	a := e.addr(o)
	e.w.Pay(a, xmr*6/10)
	e.w.Mine(1)
	e.pass()
	if v := e.view(o); v.Paid || v.State != rail.Seen || v.Confirmed != xmr*6/10 {
		t.Fatalf("60 %%: %+v", v)
	}
	e.w.Pay(a, xmr*4/10)
	e.pass()
	if v := e.view(o); v.Paid || v.Received != xmr || v.Confirmations != 0 {
		t.Fatalf("40 %% in the pool: %+v", v)
	}
	e.w.Mine(1)
	e.pass()
	if v := e.view(o); !v.Paid || v.Overpaid || e.eventCount(o, store.EventPaid) != 1 || e.count("ALERT: an order was overpaid; refund the excess") != 0 {
		t.Fatalf("100 %%: %+v", v)
	}
	if v := e.view(o); v.PaidAt.IsZero() {
		t.Fatalf("PaidAt: %+v", v)
	}
	e.safety(o)
}

func TestOverpaid(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	e.payOrder(o, xmr*12/10)
	e.w.Mine(3)
	e.pass()
	if v := e.view(o); !v.Paid || !v.Overpaid || v.State != rail.Overpaid || e.count("ALERT: an order was overpaid; refund the excess") != 1 {
		t.Fatalf("%+v\n%s", v, e.log)
	}
	e.safety(o)
}

// A partial payment at expiry gives order.underpaid once, with its ALERT; one never paid gives order.expired.
func TestUnderpaidAndExpired(t *testing.T) {
	e := newWatchEnv(t, nil)
	part, none := e.order(xmr), e.order(xmr)
	e.payOrder(part, xmr/2)
	e.addr(none)
	e.w.Mine(1)
	e.pass()
	e.pastExpiry(part)
	e.passes(3)
	if e.eventCount(part, store.EventUnderpaid) != 1 || e.count("ALERT: an order was underpaid at expiry") != 1 || e.view(part).State != rail.Underpaid {
		t.Fatalf("partial: %+v %+v", e.events(part), e.view(part))
	}
	if e.eventCount(none, store.EventExpired) != 1 || e.view(none).State != rail.Expired {
		t.Fatalf("none: %+v %+v", e.events(none), e.view(none))
	}
	e.safety(part, none)
}

// A payment first seen after the expiry is late: order.late once it is mined, with its ALERT; never paid.
func TestLatePayment(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	a := e.addr(o)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	txid := e.w.Pay(a, xmr)
	e.pass()
	if v := e.view(o); v.State != rail.Late || v.Late != xmr || e.eventCount(o, store.EventLate) != 0 {
		t.Fatalf("late in the pool: %+v %+v", v, e.events(o))
	}
	e.w.Mine(1)
	e.passes(2)
	if f, ok := e.fwd(o, txid, 0); !ok || f.Code != codeLate || e.eventCount(o, store.EventLate) != 1 ||
		e.count("ALERT: a late Monero payment arrived after the order's expiry") != 1 || e.view(o).Paid {
		t.Fatalf("late mined: %+v %v %+v", f, ok, e.events(o))
	}
	if e.eventCount(o, store.EventExpired) != 1 || e.eventCount(o, store.EventPaid) != 0 {
		t.Fatalf("events %+v", e.events(o))
	}
	e.safety(o)
}

// A tier-3 payment still in the pool at the window end: order.underpaid with the window's ALERT; mined later, it is
// recorded and the order reads paid (order.paid after order.underpaid, with that ALERT).
func TestWindowEnd(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(2 * xmr)
	txid := e.payOrder(o, 2*xmr)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	if len(e.events(o)) != 0 || e.view(o).State != rail.Seen {
		t.Fatalf("past the expiry, money live: %+v %+v", e.events(o), e.view(o))
	}
	e.toWindowEnd(o)
	e.pass()
	if e.eventCount(o, store.EventUnderpaid) != 1 || e.count(logUnderpaidAtWindow) != 1 || !e.watched(o, txid) {
		t.Fatalf("at the window end: %+v\n%s", e.events(o), e.log)
	}
	e.w.Mine(3)
	e.pass()
	if !e.view(o).Paid || e.eventCount(o, store.EventPaid) != 1 || e.count(rail.LogPaidAfterFinal) != 1 {
		t.Fatalf("mined after the window: %+v %+v", e.view(o), e.events(o))
	}
	e.safety(o)
}

// --- unlock_time ---

// A payment locked by its sender beyond the normal lock (judged at first sight) is never counted: late, order.late
// and both ALERTs, code 4 once mined, never paid; still late after the lock has passed. A timestamp lock is the same;
// a lock within the normal 10 blocks counts.
func TestUnlockTimeIsLate(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(20)
	tip := e.w.Height()
	var all []store.Order
	for _, unlock := range []int64{tip + 1000, 1_500_000_000} {
		o := e.order(xmr)
		txid := e.w.PayLocked(e.addr(o), xmr, unlock)
		e.pass()
		v := e.view(o)
		if v.Late != xmr || v.Received != 0 || v.State != rail.Late || e.count("ALERT: a Monero payment is locked by its sender beyond the normal lock; it is treated as late") < 1 {
			t.Fatalf("unlock %d, seen: %+v\n%s", unlock, v, e.log)
		}
		e.w.Mine(1)
		e.pass()
		if f, ok := e.fwd(o, txid, 0); !ok || f.Code != codeLate || e.eventCount(o, store.EventLate) != 1 || e.view(o).Paid {
			t.Fatalf("unlock %d, mined: %+v %v %+v", unlock, f, ok, e.events(o))
		}
		before := e.view(o)
		e.w.Mine(1000)
		e.passes(3)
		after := e.view(o)
		after.Stale, before.Stale = false, false
		if _, ok := e.fwd(o, txid, 1); ok || after.Paid || after.Late != xmr || e.eventCount(o, store.EventPaid) != 0 ||
			e.count(logDifferent) != 0 || after.State != before.State || after.Confirmed != before.Confirmed {
			t.Fatalf("unlock %d, after the lock: %+v (before %+v)\n%s", unlock, after, before, e.log)
		}
		all = append(all, o)
	}
	if e.count("ALERT: a Monero payment is locked by its sender beyond the normal lock; it is treated as late") != 2 ||
		e.count("ALERT: a late Monero payment arrived after the order's expiry") != 2 {
		t.Fatalf("the ALERTs are once per payment:\n%s", e.log)
	}
	o := e.order(xmr)
	e.w.PayLocked(e.addr(o), xmr, e.w.Height()+5)
	e.w.Mine(1)
	e.pass()
	if v := e.view(o); !v.Paid || v.Late != 0 {
		t.Fatalf("a lock within the normal lock: %+v", v)
	}
	e.safety(append(all, o)...)
}

// One transaction paying two orders' subaddresses: two rows, both tracked, both paid at their tiers.
func TestOneTxTwoOrders(t *testing.T) {
	e := newWatchEnv(t, nil)
	a, b := e.order(xmr), e.order(5*xmr)
	txid := e.w.PayMany(map[string]int64{e.addr(a): xmr, e.addr(b): 5 * xmr})
	e.pass()
	if len(e.payments(a)) != 1 || len(e.payments(b)) != 1 || !e.watched(a, txid) || !e.watched(b, txid) {
		t.Fatalf("rows %+v %+v", e.payments(a), e.payments(b))
	}
	e.w.Mine(1)
	e.pass()
	if !e.view(a).Paid || e.view(b).Paid {
		t.Fatal("after 1 block")
	}
	e.w.Mine(2)
	e.pass()
	if !e.view(b).Paid || e.count("ALERT: wallet-rpc answered with a bad transfer") != 0 {
		t.Fatalf("after 3 blocks: %+v\n%s", e.view(b), e.log)
	}
	e.safety(a, b)
}

// --- double spends and pool drops ---

// Tier 0: paid from the pool; double spend seen: dead, the record reversed, order.reverted 1 and the ALERT;
// rebroadcast and mined: paid again.
func TestZeroConfDoubleSpend(t *testing.T) {
	e := newWatchEnv(t, zeroTiers)
	o := e.order(xmr / 200)
	txid := e.payOrder(o, xmr/200)
	e.pass()
	if !e.view(o).Paid {
		t.Fatal("setup")
	}
	e.w.DoubleSpend(txid)
	e.pass()
	if m, _ := e.mark(o, txid); m.Kind != store.MarkDead {
		t.Fatalf("mark %+v", m)
	}
	if _, ok := e.rev(o, txid, 0); !ok || !e.hasEvent(o, store.EventReverted, 1) || e.view(o).Paid ||
		e.count("ALERT: a Monero payment was reverted by a reorg or a double spend") != 1 {
		t.Fatalf("%+v %+v", e.view(o), e.events(o))
	}
	e.w.Drop(txid)
	e.w.Rebroadcast(txid)
	e.w.Mine(1)
	e.pass()
	if !e.view(o).Paid || !e.hasEvent(o, store.EventPaidAgain, 1) {
		t.Fatalf("mined after the double spend: %+v %+v", e.view(o), e.events(o))
	}
	e.safety(o)
}

// Tier 1: a double spend seen in the pool keeps the mark at mempool, logs the ALERT once, reverts nothing; mined, it
// is paid. The variant: the transaction then drops: dead, order.expired at the expiry, no order.paid.
func TestDoubleSpendOnATierNeedingABlock(t *testing.T) {
	const line = "ALERT: a Monero payment's inputs were seen double spent; the block decides"
	t.Run("mined", func(t *testing.T) {
		e := newWatchEnv(t, nil)
		o := e.order(xmr)
		txid := e.payOrder(o, xmr)
		e.pass()
		e.w.DoubleSpend(txid)
		e.passes(3)
		if m, _ := e.mark(o, txid); m.Kind != store.MarkMempool || m.Seq != 1 || e.count(line) != 1 || len(e.events(o)) != 0 {
			t.Fatalf("mark %+v, events %+v\n%s", m, e.events(o), e.log)
		}
		e.w.Mine(1)
		e.pass()
		if !e.view(o).Paid {
			t.Fatal("not paid once mined")
		}
		e.safety(o)
	})
	t.Run("dropped", func(t *testing.T) {
		e := newWatchEnv(t, nil)
		o := e.order(xmr)
		txid := e.payOrder(o, xmr)
		e.pass()
		e.w.DoubleSpend(txid)
		e.pass()
		e.w.Drop(txid)
		e.pass()
		if m, _ := e.mark(o, txid); m.Kind != store.MarkDead {
			t.Fatalf("mark %+v", m)
		}
		e.pastExpiry(o)
		e.pass()
		if e.eventCount(o, store.EventExpired) != 1 || e.eventCount(o, store.EventPaid) != 0 || e.count(line) != 1 {
			t.Fatalf("events %+v", e.events(o))
		}
		e.safety(o)
	})
}

// Tier 1: a payment in the pool, then dropped: waiting, nothing recorded; order.expired at the expiry.
func TestPoolDrop(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.pass()
	e.w.Drop(txid)
	e.pass()
	if v := e.view(o); v.State != rail.Waiting || v.Received != 0 {
		t.Fatalf("%+v", v)
	}
	if _, ok := e.fwd(o, txid, 0); ok {
		t.Fatal("recorded")
	}
	e.pastExpiry(o)
	e.pass()
	if e.eventCount(o, store.EventExpired) != 1 {
		t.Fatalf("events %+v", e.events(o))
	}
	e.safety(o)
}

// --- the peer rail and Lightning ---

// An order with on-chain and Monero amounts, nothing on Monero: the peer pending holds the final event; the peer
// partial gives order.underpaid once; the peer paid makes the order read paid, and a Monero record then logs the
// both-rails ALERT; the peer with late money and a Monero record logs it too.
func TestPeerHoldsTheFinalEvent(t *testing.T) {
	const both = "ALERT: an order was paid on both rails"
	t.Run("pending, then partial", func(t *testing.T) {
		e := newWatchEnv(t, nil)
		p := &livePeer{}
		e.svc.SetPeer(p, nil)
		o := e.mixed(0, 50_000, xmr)
		e.addr(o)
		p.set(rail.View{Received: 50_000})
		e.pastExpiry(o)
		e.passes(2)
		if len(e.events(o)) != 0 {
			t.Fatalf("the peer pending: %+v", e.events(o))
		}
		p.set(rail.View{Received: 20_000})
		e.passes(2)
		if e.eventCount(o, store.EventUnderpaid) != 1 || len(e.events(o)) != 1 {
			t.Fatalf("the peer partial: %+v", e.events(o))
		}
		e.safety(o)
	})
	t.Run("paid on the peer, then a Monero record", func(t *testing.T) {
		e := newWatchEnv(t, nil)
		p := &livePeer{}
		e.svc.SetPeer(p, nil)
		o := e.mixed(0, 50_000, xmr)
		a := e.addr(o)
		p.set(rail.View{Received: 50_000, Confirmed: 50_000, Paid: true})
		e.pastExpiry(o)
		e.passes(2)
		if e.eventCount(o, store.EventPaid) != 1 || len(e.events(o)) != 1 || e.count(both) != 0 {
			t.Fatalf("paid on the peer: %+v", e.events(o))
		}
		e.w.Pay(a, xmr/10)
		e.w.Mine(1)
		e.passes(2)
		if e.count(both) != 1 || len(e.events(o)) != 2 || e.eventCount(o, store.EventLate) != 1 {
			t.Fatalf("a Monero record after the peer paid: %+v\n%s", e.events(o), e.log)
		}
		e.safety(o)
	})
	t.Run("late money on the peer, then a Monero record", func(t *testing.T) {
		e := newWatchEnv(t, nil)
		p := &livePeer{}
		e.svc.SetPeer(p, nil)
		o := e.mixed(0, 50_000, xmr)
		a := e.addr(o)
		p.set(rail.View{Late: 50_000})
		e.w.Pay(a, xmr/10)
		e.w.Mine(1)
		e.passes(2)
		if e.count(both) != 1 {
			t.Fatalf("money on two rails, neither paid:\n%s", e.log)
		}
		e.safety(o)
	})
	t.Run("paid on Lightning, then a Monero record", func(t *testing.T) {
		e := newWatchEnv(t, nil)
		o := e.mixed(1_000_000, 0, xmr)
		a := e.addr(o)
		e.ln.set(true)
		e.w.Pay(a, xmr)
		e.w.Mine(1)
		e.passes(2)
		if e.count(both) != 1 || e.eventCount(o, store.EventPaid) != 1 {
			t.Fatalf("\n%s", e.log)
		}
		e.safety(o)
	})
}

// A peer that turns Received > 0 between the settle's first read and its re-read after the creation: the ALERT fires.
// The same with Lightning turning paid.
func TestBothRailsSimultaneous(t *testing.T) {
	const both = "ALERT: an order was paid on both rails"
	t.Run("peer", func(t *testing.T) {
		e := newWatchEnv(t, nil)
		p := &livePeer{}
		e.svc.SetPeer(p, nil)
		o := e.mixed(0, 50_000, xmr)
		e.payOrder(o, xmr)
		p.set(rail.View{})
		e.pass()
		e.w.Mine(1)
		p.setOnView(func(n int) {
			if n == 1 {
				p.set(rail.View{Received: 10_000})
			}
		})
		e.pass()
		if e.count(both) != 1 {
			t.Fatalf("the re-read after the creation missed the peer's money:\n%s", e.log)
		}
		e.safety(o)
	})
	t.Run("lightning", func(t *testing.T) {
		e := newWatchEnv(t, nil)
		o := e.mixed(1_000_000, 0, xmr)
		e.payOrder(o, xmr)
		e.pass()
		e.w.Mine(1)
		e.ln.mu.Lock()
		e.ln.onStatus = func(n int) {
			if n == 1 {
				e.ln.set(true)
			}
		}
		e.ln.calls = 0
		e.ln.mu.Unlock()
		e.pass()
		if e.count(both) != 1 {
			t.Fatalf("the re-read after the creation missed Lightning:\n%s", e.log)
		}
		e.safety(o)
	})
}

// LightningPaid logs the both-rails ALERT for an order with a Monero record, once; none without one.
func TestLightningPaid(t *testing.T) {
	e := newWatchEnv(t, nil)
	a, b := e.mixed(1_000_000, 0, xmr), e.mixed(1_000_000, 0, xmr)
	e.payOrder(a, xmr/2)
	e.payOrder(b, xmr)
	e.w.Mine(1)
	e.pass()
	e.payments(b)
	if err := e.svc.LightningPaid(bg, b); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.LightningPaid(bg, b); err != nil {
		t.Fatal(err)
	}
	if e.count("ALERT: an order was paid on both rails") != 1 {
		t.Fatalf("\n%s", e.log)
	}
	c := e.mixed(1_000_000, 0, xmr)
	e.addr(c)
	if err := e.svc.LightningPaid(bg, c); err != nil || e.count("ALERT: an order was paid on both rails") != 1 {
		t.Fatalf("no Monero record: %v\n%s", err, e.log)
	}
	if err := e.svc.LightningPaid(bg, a); err != nil || e.count("ALERT: an order was paid on both rails") != 2 {
		t.Fatalf("a partial Monero record: %v\n%s", err, e.log)
	}
	e.safety(a, b, c)
}

// The first order.paid writes the account_paid fact and then calls MarkKnown with the order's account; a replay after
// a crash before the event calls it again, harmlessly.
func TestPaidMarksTheAccountKnown(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	e.payOrder(o, xmr)
	e.w.Mine(1)
	e.ln.mu.Lock()
	e.ln.onMark = func(acct string) {
		if known, err := e.st.AccountPaid(bg, acct); err != nil || !known {
			t.Errorf("MarkKnown before the fact: %v %v", known, err)
		}
	}
	e.ln.mu.Unlock()
	e.crashAt("settle:after-event")
	if err := e.pass(); !errors.Is(err, errCrash) {
		t.Fatalf("no crash: %v", err)
	}
	if got := e.ln.markedAccounts(); !slices.Equal(got, []string{"acct-1"}) || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("marked %v, events %+v", got, e.events(o))
	}
	e.svc.hook = nil
	e.passes(2)
	if got := e.ln.markedAccounts(); !slices.Equal(got, []string{"acct-1"}) {
		t.Fatalf("marked again with order.paid stored: %v", got)
	}
	e.safety(o)
	// A crash between the fact and the event: the replay writes the event and calls MarkKnown again.
	o2 := e.order(xmr)
	e.payOrder(o2, xmr)
	e.w.Mine(1)
	e.crashAt("settle:after-account")
	e.pass()
	if e.eventCount(o2, store.EventPaid) != 0 || len(e.ln.markedAccounts()) != 1 {
		t.Fatalf("after the crash: %+v %v", e.events(o2), e.ln.markedAccounts())
	}
	e.svc.hook = nil
	e.pass()
	if e.eventCount(o2, store.EventPaid) != 1 || len(e.ln.markedAccounts()) != 2 {
		t.Fatalf("after the replay: %+v %v", e.events(o2), e.ln.markedAccounts())
	}
	e.safety(o, o2)
}

// Expired on Lightning and a Monero partial at expiry: order.underpaid. Lightning not done: no final event.
func TestFinalWaitsForLightning(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.mixed(1_000_000, 0, xmr)
	e.payOrder(o, xmr/2)
	e.w.Mine(1)
	e.pass()
	e.pastExpiry(o)
	e.passes(2)
	if len(e.events(o)) != 0 {
		t.Fatalf("Lightning not done: %+v", e.events(o))
	}
	e.ln.setExpired(true)
	e.clk.Advance(time.Second)
	e.pass()
	if e.eventCount(o, store.EventUnderpaid) != 1 {
		t.Fatalf("%+v", e.events(o))
	}
	e.safety(o)
}

// Ruling 8-T7d: a stale peer (failing or paused, not paid) holds a mixed order's final event as a pending
// one does, until the watch window's end. Fresh again inside the window with nothing received (the order still listed
// past its expiry): order.expired once. At the window's end a held order is not settled: the left-the-window ALERT
// reports it and no final event is invented, whether the peer is still stale then or turned fresh only at the
// first pass after the window's end.
func TestStalePeerHoldsTheFinalEvent(t *testing.T) {
	const noFinal = "ALERT: a Monero order left its watch window without a final event; check it by hand"
	held := func(t *testing.T) (*env, *livePeer, store.Order) {
		e := newWatchEnv(t, nil)
		p := &livePeer{}
		e.svc.SetPeer(p, nil)
		o := e.mixed(0, 50_000, xmr)
		e.addr(o)
		p.set(rail.View{Stale: true})
		e.pastExpiry(o)
		e.passes(3)
		if len(e.events(o)) != 0 {
			t.Fatalf("the peer stale: %+v", e.events(o))
		}
		return e, p, o
	}
	lastSecond := func(e *env, o store.Order) {
		e.clk.Advance(o.ExpiresAt.Add(store.WatchWindow).Add(-time.Second).Sub(e.clk.Now()))
		e.pass()
		if len(e.events(o)) != 0 {
			e.t.Fatalf("a second before the window's end: %+v", e.events(o))
		}
	}
	t.Run("fresh again inside the window, nothing received", func(t *testing.T) {
		e, p, o := held(t)
		l, err := e.st.MoneroOrdersPastExpiry(bg, e.clk.Now())
		if err != nil || !slices.ContainsFunc(l, func(x store.Order) bool { return x.ID == o.ID }) {
			t.Fatalf("the held order is not listed past its expiry: %v %v", l, err)
		}
		p.set(rail.View{})
		e.passes(2)
		if !e.clk.Now().Before(o.ExpiresAt.Add(store.WatchWindow)) {
			t.Fatal("setup: past the window")
		}
		if e.eventCount(o, store.EventExpired) != 1 || len(e.events(o)) != 1 || e.count(noFinal) != 0 {
			t.Fatalf("the peer fresh: %+v", e.events(o))
		}
		e.safety(o)
	})
	t.Run("still stale at the window's end", func(t *testing.T) {
		e, _, o := held(t)
		lastSecond(e, o)
		e.toWindowEnd(o)
		e.pass()
		e.passes(2)
		if len(e.events(o)) != 0 || e.count(noFinal) != 1 {
			t.Fatalf("at the window's end: %+v\n%s", e.events(o), e.log)
		}
		e.safety(o)
	})
	// Ruling 8-T7e: past the window, a late payment makes settle reach the held order; while the peer stays stale it
	// writes no final event, and once the peer is fresh the next settle writes it.
	t.Run("a late payment after the window, the peer stale, then fresh", func(t *testing.T) {
		e, p, o := held(t)
		lastSecond(e, o)
		e.toWindowEnd(o)
		e.pass()
		if len(e.events(o)) != 0 || e.count(noFinal) != 1 {
			t.Fatalf("at the window's end: %+v\n%s", e.events(o), e.log)
		}
		e.w.Pay(e.sub(o).Address, xmr/10)
		e.passes(2)
		if len(e.events(o)) != 0 || e.count("ALERT: a Monero payment arrived after the subaddress's watch window") != 1 {
			t.Fatalf("a late payment, the peer stale: %+v\n%s", e.events(o), e.log)
		}
		p.set(rail.View{})
		e.w.Pay(e.sub(o).Address, xmr/10)
		e.passes(2)
		if e.eventCount(o, store.EventExpired) != 1 || len(e.events(o)) != 1 || e.count(noFinal) != 1 {
			t.Fatalf("the peer fresh: %+v\n%s", e.events(o), e.log)
		}
		e.safety(o)
	})
	t.Run("stale at the last pass inside, fresh at the first after", func(t *testing.T) {
		e, p, o := held(t)
		lastSecond(e, o)
		p.set(rail.View{})
		e.toWindowEnd(o)
		e.pass()
		e.passes(2)
		if len(e.events(o)) != 0 || e.count(noFinal) != 1 {
			t.Fatalf("fresh only past the window: %+v\n%s", e.events(o), e.log)
		}
		e.safety(o)
	})
}

// Ruling 8-A5: an order of 2 piconero paid 2 and then L-1 (L = MaxPiconero); the confirmed sum
// saturates at L. A reorg puts only the L-1 payment back in the pool; the 2-piconero payment stays mined and
// confirmed, so the order never stops being paid. Subtracting L-1 from the capped L read 1 < 2: a false
// order.reverted, then order.paid_again. The surviving sum is 2.
func TestSaturatedSumReversal(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(2)
	a := e.addr(o)
	small := e.w.Pay(a, 2)
	e.w.Mine(1)
	big := e.w.Pay(a, store.MaxPiconero-1)
	e.w.Mine(1)
	e.passes(2)
	if _, ok := e.fwd(o, small, 0); !ok {
		t.Fatal("setup: the small payment is not recorded")
	}
	if _, ok := e.fwd(o, big, 0); !ok {
		t.Fatal("setup: the big payment is not recorded")
	}
	e.w.Pop(1) // the big payment back in the pool; the small one stays mined
	e.passes(3)
	v := e.view(o)
	if !v.Paid || v.Confirmed != 2 {
		t.Errorf("view: Paid=%v Confirmed=%d, want paid with 2 confirmed", v.Paid, v.Confirmed)
	}
	if e.eventCount(o, store.EventReverted) != 0 || e.eventCount(o, store.EventPaidAgain) != 0 {
		t.Fatalf("the order never stopped being paid (the 2-piconero payment stayed confirmed), but got %d order.reverted and %d order.paid_again; log:\n%s",
			e.eventCount(o, store.EventReverted), e.eventCount(o, store.EventPaidAgain), e.log)
	}
}

// peerSettle models the on-chain rail's settle of a reversal, run on its own goroutine as service runs it: it takes
// the settle mutex the rails share, flips the peer's view to unpaid and, when the Monero rail then reads unpaid (View,
// the path a peer's settle reads under that mutex), writes order.reverted 1 (what onchain.settle writes: its own sum
// was the order's paid state, so k = 1); when Monero still reads paid it stamps its reversal 0 and writes nothing.
// Called inside the Monero settle's peer read. The goroutine first tries the mutex: if it gets it, the settle that
// called this does not hold it (the interleavings of ruling 8-A1), so the reversal runs to its end inside that
// settle's window and peerSettle waits for it; if not, the settle holds it, peerSettle returns at once and the
// goroutine waits on the mutex, so the reversal lands after the settle. No sleep: both outcomes are decided by the
// mutex alone. done is closed when it has run.
func peerSettle(e *env, mu *sync.Mutex, p *livePeer, o store.Order) (done chan struct{}) {
	done = make(chan struct{})
	got := make(chan bool, 1)
	go func() {
		defer close(done)
		free := mu.TryLock()
		got <- free
		if !free {
			mu.Lock()
		}
		defer mu.Unlock()
		p.set(rail.View{State: rail.Waiting})
		v, _, err := e.svc.View(bg, o)
		if err != nil {
			e.t.Error(err)
			return
		}
		if !v.Paid {
			if _, err := e.st.AddEvent(bg, store.Event{Type: store.EventReverted, OrderID: o.ID, Seq: 1}); err != nil {
				e.t.Error(err)
			}
		}
	}()
	if <-got {
		<-done
	}
	return done
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the peer's settle never ran")
	}
}

// Ruling 8-A1 (a false order.paid_again): a mixed order paid on-chain, with a partial Monero payment
// that keeps the Monero pass settling it. The on-chain rail reverses its payment during the Monero settle's peer read.
// Before the shared settle mutex, the on-chain order.reverted 1 landed between that read and the events read, and the
// Monero settle answered it with order.paid_again 1 from its stale peer.Paid, though no rail paid the order.
func TestSettleSharedLockNoFalsePaidAgain(t *testing.T) {
	e := newWatchEnv(t, nil)
	p := &livePeer{}
	mu := &sync.Mutex{}
	e.svc.SetPeer(p, mu)
	o := e.mixed(0, 50_000, xmr)
	p.set(rail.View{})
	a := e.addr(o) // issued before the on-chain payment
	p.set(rail.View{State: rail.Paid, Received: 50_000, Confirmed: 50_000, Paid: true})
	e.w.Pay(a, xmr/2)
	e.w.Mine(1)
	e.pass()
	if !e.hasEvent(o, store.EventPaid, 1) || e.eventCount(o, store.EventReverted) != 0 {
		t.Fatalf("setup: %+v", e.events(o))
	}
	var done chan struct{}
	p.setOnView(func(n int) {
		if n == 1 {
			done = peerSettle(e, mu, p, o)
		}
	})
	e.w.Mine(1)
	e.pass()
	p.setOnView(nil)
	if done == nil {
		t.Fatal("the Monero pass did not settle the order")
	}
	waitDone(t, done)
	e.passes(3)
	if e.eventCount(o, store.EventReverted) != 1 || e.hasEvent(o, store.EventPaidAgain, 1) {
		t.Fatalf("events %+v: want order.reverted 1 and no order.paid_again (Monero %d of %d confirmed, on-chain unpaid)",
			e.events(o), e.view(o).Confirmed, o.AmountPiconero)
	}
	e.clk.Advance(2 * time.Hour) // past the expiry: the order reads underpaid, not seen
	e.passes(2)
	if v := e.view(o); v.State != rail.Underpaid || v.Paid {
		t.Fatalf("past the expiry: %+v", v)
	}
}

// Ruling 8-A1 (a lost order.reverted): a mixed order paid on both rails; both payments revert at
// once. Before the shared settle mutex each settle read the other as paid, each stamped its reversal 0, and no
// order.reverted was ever written. Serialised, the second settle reads the first's reversal: exactly one.
func TestSettleSharedLockNoLostReverted(t *testing.T) {
	e := newWatchEnv(t, nil)
	p := &livePeer{}
	mu := &sync.Mutex{}
	e.svc.SetPeer(p, mu)
	o := e.mixed(0, 50_000, xmr)
	p.set(rail.View{})
	a := e.addr(o)
	p.set(rail.View{State: rail.Paid, Received: 50_000, Confirmed: 50_000, Paid: true})
	e.w.Pay(a, xmr)
	e.w.Mine(1)
	e.pass()
	if !e.hasEvent(o, store.EventPaid, 1) {
		t.Fatalf("setup: %+v", e.events(o))
	}
	var done chan struct{}
	p.setOnView(func(n int) {
		if n == 1 {
			done = peerSettle(e, mu, p, o)
		}
	})
	e.w.Pop(1) // the Monero payment back in the pool: its record (tier 1) reverts
	e.pass()
	p.setOnView(nil)
	if done == nil {
		t.Fatal("the Monero pass did not settle the order")
	}
	waitDone(t, done)
	e.passes(3)
	if v := e.view(o); v.Paid || e.eventCount(o, store.EventReverted) != 1 {
		t.Fatalf("the order was paid and reads unpaid on every rail (%+v), with %d order.reverted; want 1; events %+v",
			v, e.eventCount(o, store.EventReverted), e.events(o))
	}
}

// Ruling 8-A1: SetPeer installs the settle mutex service shares between the rails; nil keeps the rail's own, and a
// later nil does not undo a shared one.
func TestSetPeerInstallsTheSettleLock(t *testing.T) {
	e := newWatchEnv(t, nil)
	own := e.svc.settleLock()
	if own == nil {
		t.Fatal("no settle mutex of its own")
	}
	e.svc.SetPeer(nil, nil)
	if e.svc.settleLock() != own {
		t.Fatal("SetPeer(nil, nil) replaced the rail's own mutex")
	}
	shared := &sync.Mutex{}
	e.svc.SetPeer(nil, shared)
	if e.svc.settleLock() != shared {
		t.Fatal("SetPeer did not install the shared mutex")
	}
	e.svc.SetPeer(nil, nil)
	if e.svc.settleLock() != shared {
		t.Fatal("SetPeer(nil, nil) dropped the shared mutex")
	}
}

// readDuringSettle reads the rail on its own goroutine from inside a settle (a test hook) and returns once the read
// has answered (answered) or its goroutine waits on a mutex inside Rail (read in the goroutine dump): never a sleep.
// res gets the read's answer whenever it comes.
func readDuringSettle(t *testing.T, read func() (rail.View, error)) (answered bool, res chan railRead) {
	t.Helper()
	res = make(chan railRead, 1)
	go func() {
		v, err := read()
		res <- railRead{v, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(res) > 0 {
			return true, res
		}
		if waitsInRail() {
			return false, res
		}
		runtime.Gosched()
	}
	t.Fatal("the read neither answered nor waited on the settle mutex")
	return false, res
}

type railRead struct {
	v   rail.View
	err error
}

// waitsInRail reports whether a goroutine is in sync.(*Mutex).Lock called from (*Service).Rail.
func waitsInRail() bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "sync.(*Mutex).Lock") && strings.Contains(g, "/monero.(*Service).Rail(") {
			return true
		}
	}
	return false
}

// Ruling R1: settle creates a new forward before it reverses an old one, so between the
// two writes the ledger holds both. An order of 1 XMR paid 0.6 that is reorged out (dropped, dead) and a second 0.6
// mined before the next pass: the pass records the second (settle:after-transfers) and then reverses the first. A
// status read at that hook summed both, 1.2: Paid, for an order that never was. Rail takes the settle mutex, so it
// reads the settle's end: 0.6, unpaid.
func TestRailReadsNoHalfSettle(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	first := e.w.Pay(e.addr(o), 6*xmr/10)
	e.w.Mine(1)
	e.passes(2)
	if _, ok := e.fwd(o, first, 0); !ok || e.view(o).Paid {
		t.Fatalf("setup: the first payment is not recorded, or the order reads paid: %+v", e.view(o))
	}
	e.w.PopDrop(1)
	e.w.Pay(e.addr(o), 6*xmr/10)
	e.w.Mine(1)
	var answered, hooked bool
	var res chan railRead
	e.svc.hook = func(p string) error {
		if p == "settle:after-transfers" && !hooked {
			hooked = true
			answered, res = readDuringSettle(t, func() (rail.View, error) { return e.svc.Rail(bg, o) })
		}
		return nil
	}
	e.pass()
	e.svc.hook = nil
	if !hooked {
		t.Fatal("the pass created no transfer")
	}
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.v.Paid || r.v.Confirmed != 6*xmr/10 {
		t.Errorf("a read inside the settle (answered there: %v): Paid=%v Confirmed=%d, want unpaid with 0.6 XMR", answered,
			r.v.Paid, r.v.Confirmed)
	}
	if _, ok := e.rev(o, first, 0); !ok || e.view(o).Paid || e.eventCount(o, store.EventPaid) != 0 {
		t.Fatalf("after the pass: reversed %v, %+v, events %+v", ok, e.view(o), e.events(o))
	}
}

// Ruling R1: View is the path a peer's settle reads under the settle mutex the rails share, so it never takes that
// mutex itself (it would wait on its own caller).
func TestViewUnderTheSettleMutex(t *testing.T) {
	e := newWatchEnv(t, nil)
	mu := &sync.Mutex{}
	e.svc.SetPeer(nil, mu)
	o := e.order(xmr)
	e.addr(o)
	mu.Lock()
	defer mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, _, err := e.svc.View(bg, o)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("View waited on the settle mutex")
	}
}

// Ruling R2: step 1 of every settle heals the peer rail's stamped reversals too. The peer holds a reversal stamped 1
// whose order.reverted is missing (a crash or a store error between the two databases); this rail's next settle of
// the order adds it, before it derives anything, and only the seqs the peer reports. With no peer (SetPeer(nil)),
// nothing is read.
func TestSettleHealsThePeersReversal(t *testing.T) {
	e := newWatchEnv(t, nil)
	p := &livePeer{}
	e.svc.SetPeer(p, nil)
	o := e.mixed(0, 50_000, xmr)
	e.payOrder(o, xmr/2) // a partial payment, still tracked, so every pass settles the order
	e.w.Mine(1)
	e.passes(2)
	if n := e.eventCount(o, store.EventReverted); n != 0 {
		t.Fatalf("setup: %d order.reverted", n)
	}
	p.setSeqs(1, 1<<21) // 2^21 is above the bound of a SatBooth seq: ignored
	e.passes(1)
	if !e.hasEvent(o, store.EventReverted, 1) || e.eventCount(o, store.EventReverted) != 1 {
		t.Fatalf("the peer's reversal stamped 1 was not healed: %+v", e.events(o))
	}
	p.setSeqs(1, 2)
	e.passes(1)
	if !e.hasEvent(o, store.EventReverted, 2) || e.eventCount(o, store.EventReverted) != 2 {
		t.Fatalf("the peer's reversal stamped 2: %+v", e.events(o))
	}
	e.svc.SetPeer(nil, nil)
	e.passes(1)
	if e.eventCount(o, store.EventReverted) != 2 {
		t.Fatalf("without a peer: %+v", e.events(o))
	}
}

// Ruling S1: no partial settle is ever durable. The replacement case of
// TestRailReadsNoHalfSettle, interrupted: the settle that records the second 0.6 XMR and reverses the first ends early
// (a crash at the first hook after a ledger write: before the batch, the forward written alone). Rail never reads
// 1.2, Paid, and the restarted pass publishes no order.paid: the forward and its reversal are one batch.
func TestInterruptedSettleIsNotPaid(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	first := e.w.Pay(e.addr(o), 6*xmr/10)
	e.w.Mine(1)
	e.passes(2)
	if _, ok := e.fwd(o, first, 0); !ok || e.view(o).Paid {
		t.Fatalf("setup: the first payment is not recorded, or the order reads paid: %+v", e.view(o))
	}
	e.w.PopDrop(1)
	e.w.Pay(e.addr(o), 6*xmr/10)
	e.w.Mine(1)
	fired := false
	e.svc.hook = func(p string) error {
		if p == "settle:after-transfers" && !fired {
			fired = true
			return errCrash
		}
		return nil
	}
	err := e.pass()
	e.svc.hook = nil
	if !fired || !errors.Is(err, errCrash) {
		t.Fatalf("no crash in the settle: %v", err)
	}
	if v := e.view(o); v.Paid || v.Confirmed != 6*xmr/10 {
		t.Errorf("after the interrupted settle: Paid=%v Confirmed=%d, want unpaid with 0.6 XMR", v.Paid, v.Confirmed)
	}
	e.restart()
	e.pass()
	e.passes(2)
	if n := e.eventCount(o, store.EventPaid); n != 0 || e.view(o).Paid {
		t.Fatalf("after the restart: %d order.paid, %+v, events %+v", n, e.view(o), e.events(o))
	}
	if _, ok := e.rev(o, first, 0); !ok {
		t.Fatal("the first payment was never reversed")
	}
	e.safety(o)
}

// Ruling S1: a batch item that exists with other details is logged (logDifferent) and left out, nothing of the batch
// is applied, and the caller retries without it; any other refusal is an error and nothing is applied either.
func TestCreateTransfersLeavesOutADifferentRecord(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	ud, _ := orderUserData(o.ID)
	txid := strings.Repeat("ab", 32)
	good := book.Forward(PaidTransferID(txid, 1, 0), ud, xmr/2, true)
	clash := book.Forward(PaidTransferID(txid, 2, 0), ud, xmr/2, true)
	e.transfer(clash.ID, MoneroWallet, MerchantMonero, o, xmr/3, codePaid) // the same id, another amount
	left := map[ledger.U128]bool{}
	res, retry, err := e.svc.createTransfers(bg, o, []ledger.Transfer{good, clash}, 2, left)
	if err != nil || !retry || res != nil || !left[clash.ID] || e.count(logDifferent) != 1 {
		t.Fatalf("results %v retry %v err %v left %v, %d logDifferent", res, retry, err, left, e.count(logDifferent))
	}
	if _, ok := e.lookup(good.ID); ok {
		t.Fatal("the refused batch wrote its other item")
	}
	res, retry, err = e.svc.createTransfers(bg, o, []ledger.Transfer{good}, 1, left)
	if err != nil || retry || len(res) != 1 || res[0] != ledger.TransferCreated {
		t.Fatalf("the retry: %v %v %v", res, retry, err)
	}
	bad := good
	bad.ID = PaidTransferID(txid, 3, 0)
	bad.Ledger = 9
	if _, _, err := e.svc.createTransfers(bg, o, []ledger.Transfer{book.Forward(PaidTransferID(txid, 4, 0), ud, 1, true), bad}, 2, left); err == nil {
		t.Fatal("another refusal is no error")
	}
	if _, ok := e.lookup(PaidTransferID(txid, 4, 0)); ok {
		t.Fatal("a batch refused for another reason wrote its other item")
	}
	// Ruling TU1: a reversal (from index nFwd) that exists with other details is an error, not a retry.
	rev := book.Reversal(ReversalTransferID(txid, 5, 0), ud, xmr/2, 0)
	e.reverse(o, txid, 5, 0, xmr/3)
	fwd := book.Forward(PaidTransferID(txid, 6, 0), ud, xmr/2, true)
	before := len(left)
	res, retry, err = e.svc.createTransfers(bg, o, []ledger.Transfer{fwd, rev}, 1, left)
	if err == nil || retry || res != nil || len(left) != before || e.count(logDifferent) != 2 {
		t.Fatalf("a refused reversal: results %v retry %v err %v left %v, %d logDifferent", res, retry, err, left, e.count(logDifferent))
	}
	if _, ok := e.lookup(fwd.ID); ok {
		t.Fatal("the batch with a refused reversal wrote its forward")
	}
}

// Ruling TU1: a reversal refused as exists_with_different is never left out. The
// replacement case of TestInterruptedSettleIsNotPaid, with a reversal under the first payment's reversal id but
// another amount inserted after the settle read its views (the peer's View runs then): leaving it out and writing the
// rest would record the second 0.6 XMR beside the first, 1.2 XMR, and a false order.paid. The settle fails instead:
// logDifferent, nothing written, no order.paid, and the order stays pending settle and stale.
func TestRefusedReversalFailsTheSettle(t *testing.T) {
	e := newWatchEnv(t, nil)
	p := &livePeer{}
	p.set(rail.View{})
	e.svc.SetPeer(p, nil)
	o := e.mixed(0, 100_000, xmr)
	first := e.w.Pay(e.addr(o), 6*xmr/10)
	e.w.Mine(1)
	e.passes(2)
	if _, ok := e.fwd(o, first, 0); !ok || e.view(o).Paid {
		t.Fatalf("setup: the first payment is not recorded, or the order reads paid: %+v", e.view(o))
	}
	e.w.PopDrop(1)
	second := e.w.Pay(e.addr(o), 6*xmr/10)
	e.w.Mine(1)
	index := uint32(e.sub(o).Index)
	inserted := false
	p.setOnView(func(int) {
		if !inserted {
			inserted = true
			e.reverse(o, first, index, 0, xmr/3) // the first payment's reversal id, another amount
		}
	})
	e.clk.Advance(time.Second)
	err := e.pass()
	p.setOnView(nil)
	if err != nil || !inserted {
		t.Fatalf("pass %v, inserted %v", err, inserted)
	}
	if _, ok := e.fwd(o, second, 0); ok {
		t.Errorf("the second payment was recorded although the batch's reversal was refused")
	}
	if e.count(logDifferent) != 1 || !e.logHas(`msg="`+logSettleFailed+`" order=`+o.ID) {
		t.Errorf("want one logDifferent and the settle failed for the order\n%s", e.log)
	}
	if e.eventCount(o, store.EventPaid) != 0 || e.view(o).Paid {
		t.Errorf("the order reads paid: %+v, events %+v", e.view(o), e.events(o))
	}
	pending, err := e.st.MoneroPendingSettle(bg)
	if err != nil || len(pending) != 1 || pending[0].ID != o.ID {
		t.Errorf("pending settle %+v %v: want the order", pending, err)
	}
	if !e.view(o).Stale {
		t.Errorf("the order does not read stale: %+v", e.view(o))
	}
	// The retry: the next settle reads the conflicting reversal with the views, so the first payment's record is
	// foreign (Book.Walk) and counts for nothing, as for any record with other details found up front; the second is
	// recorded alone, and the order never reads paid.
	e.passes(1)
	if v := e.view(o); v.Paid || v.Confirmed != 6*xmr/10 || e.eventCount(o, store.EventPaid) != 0 || e.count(logDifferent) < 2 {
		t.Fatalf("the next pass: %+v, events %+v, %d logDifferent", v, e.events(o), e.count(logDifferent))
	}
}
