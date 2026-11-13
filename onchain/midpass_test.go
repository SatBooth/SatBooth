package onchain

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/store"
)

// onceAt makes fn run once, the first time the pass reaches point.
func (e *env) onceAt(point string, fn func()) {
	done := false
	prev := e.svc.hook
	e.svc.hook = func(p string) error {
		if p == point && !done {
			done = true
			fn()
		}
		if prev != nil {
			return prev(p)
		}
		return nil
	}
}

// The tip drops after the pass read it (getblockchaininfo): the cursor says 6 confirmations, bitcoind's gettransaction
// says 3. The output is not recorded on the cursor's count, and is recorded once bitcoind itself reaches 6.
func TestTipDropsAfterGetBlockchainInfoNotPaid(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(20_000_000) // above 10,000,000 sat: 6 confirmations
	txid := e.payOrder(o, 20_000_000)
	e.node.Mine(1)
	e.pass()
	e.node.Mine(5)
	e.onceAt("scan:after-record", func() { e.node.Invalidate(3) })
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fwd(txid, 0); ok || e.eventCount(o, store.EventPaid) != 0 || e.rail(o).Paid {
		t.Fatalf("recorded on the cursor's count: %+v %+v", e.rail(o), e.events(o))
	}
	if !e.watched(txid) {
		t.Fatal("the output is no longer watched")
	}
	if r := e.rail(o); r.Confirmations != 3 {
		t.Fatalf("displayed %d confirmations, want bitcoind's 3", r.Confirmations)
	}
	e.svc.hook = nil
	e.node.Mine(3)
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if r := e.rail(o); !r.Paid || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("not paid at 6: %+v %+v", r, e.events(o))
	}
	if e.watched(txid) {
		t.Fatal("still watched at 6")
	}
	e.safety(o)
}

// The same drop, an output recorded at 1 confirmation (tier 1): it is not settled for good on the cursor's count, so
// when its block leaves the chain and it is evicted, the next pass reads it again and reverts the order.
func TestTipDropsAfterGetBlockchainInfoNotSettled(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	txid := e.payOrder(o, 10_000)
	e.node.Mine(1)
	e.pass()
	if !e.rail(o).Paid {
		t.Fatal("setup: not recorded at 1 confirmation")
	}
	e.node.Mine(5)
	e.onceAt("scan:after-record", func() { e.node.Invalidate(3) })
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	e.svc.hook = nil
	if !e.watched(txid) {
		t.Fatal("settled for good on the cursor's count")
	}
	e.node.Invalidate(3) // the block of the payment leaves
	e.node.Evict(txid)
	e.node.Mine(10)
	n, ns := e.node.TxCalls(txid), e.node.Calls("gettxspendingprevout")
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if got := e.node.TxCalls(txid) - n; got != 2 { // evicted: read twice before its dead mark
		t.Fatalf("%d gettransaction calls, want 2", got)
	}
	if got := e.node.Calls("gettxspendingprevout") - ns; got != 0 { // its mark is mined: the inputs are not asked
		t.Fatalf("%d gettxspendingprevout calls, want 0", got)
	}
	if _, ok := e.rev(txid, 0); !ok || e.rail(o).Paid || e.eventCount(o, store.EventReverted) != 1 {
		t.Fatalf("not reverted: %+v %+v", e.rail(o), e.events(o))
	}
	e.safety(o)
}

// tipDropsAndRecovers is the chain of the two tests below: after the scan the tip drops and the payment is mined with
// 1 confirmation; after its mark the chain grows past the first tip without the payment.
func tipDropsAndRecovers(e *env, txid string) {
	e.onceAt("scan:after-record", func() {
		e.node.Invalidate(9)
		e.node.Mine(1)
	})
	e.onceAt("track:after-mark", func() {
		e.node.Invalidate(1)
		e.node.Evict(txid)
		e.node.Mine(11)
	})
}

// The cursor's tip is recovered by the end of the pass, so a check of the tip at the end could not tell: only the count
// of the answer that gave the mark can.
func TestTipDropsAndRecoversWithinPass(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(20_000_000)
	e.node.Mine(30)
	txid := e.payOrder(o, 20_000_000)
	tipDropsAndRecovers(e, txid)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fwd(txid, 0); ok || e.rail(o).Paid || !e.watched(txid) {
		t.Fatalf("recorded or settled: %+v %+v", e.rail(o), e.events(o))
	}
	e.svc.hook = nil
	e.passes(2)
	if e.rail(o).Paid || e.eventCount(o, store.EventPaid) != 0 {
		t.Fatalf("paid: %+v %+v", e.rail(o), e.events(o))
	}
	e.safety(o)
}

func TestTipDropsAndRecoversWithinPassTierOne(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	e.node.Mine(30)
	txid := e.payOrder(o, 10_000)
	tipDropsAndRecovers(e, txid)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fwd(txid, 0); !ok || !e.rail(o).Paid {
		t.Fatalf("not recorded at the observed 1: %+v %+v", e.rail(o), e.events(o))
	}
	if !e.watched(txid) {
		t.Fatal("settled for good at 1 confirmation")
	}
	e.svc.hook = nil
	e.passes(2)
	if _, ok := e.rev(txid, 0); !ok || e.rail(o).Paid || e.eventCount(o, store.EventReverted) != 1 {
		t.Fatalf("not reverted: %+v %+v", e.rail(o), e.events(o))
	}
	e.safety(o)
}

// lowered answers gettransaction with 3 confirmations fewer and the same height.
type lowered struct{ RPC }

func (w lowered) GetTransaction(ctx context.Context, txid string) (btcrpc.Transaction, error) {
	tx, err := w.RPC.GetTransaction(ctx, txid)
	if tx.Confirmations > 3 {
		tx.Confirmations -= 3
	}
	return tx, err
}

// The count used is the lower of the cursor's and bitcoind's, with the height unchanged.
func TestBoundUsesTheLowerCount(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(20_000_000)
	txid := e.payOrder(o, 20_000_000)
	e.node.Mine(6)
	e.svc.rpc = lowered{e.node}
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if r := e.rail(o); r.Paid || r.Confirmations != 3 || !e.watched(txid) {
		t.Fatalf("%+v", r)
	}
	e.svc.rpc = e.node
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if r := e.rail(o); !r.Paid || r.Confirmations != 6 {
		t.Fatalf("%+v", r)
	}
	e.safety(o)
}

// An on-time output whose mined mark reaches the store without a reading in the pass is not recorded by that pass (the
// paid decision fails closed) and is recorded by the next one, which reads it.
func TestUnobservedOutputIsNotRecorded(t *testing.T) {
	e := newWatchEnv(t)
	oa, ob := e.order(10_000), e.order(20_000)
	ta := e.payOrder(oa, 10_000)
	e.clk.Advance(time.Second)
	tb := e.payOrder(ob, 10_000) // half of b's order; c is the other half
	e.node.Mine(1)
	var tc string
	e.onceAt("track:after-mark", func() { // a is tracked first: c gets a row and a mined mark the pass never reads
		tc = e.node.Pay(e.addr(ob), 10_000)
		e.clk.Advance(time.Second)
		cur := e.cursor()
		if _, err := e.st.RecordScan(bg, []store.TXO{{TxID: tc, Address: e.addr(ob), AmountSat: 10_000, FirstSeen: e.clk.Now()}}, cur); err != nil {
			t.Fatal(err)
		}
		if err := e.st.AddMark(bg, store.Mark{TxID: tc, Kind: store.MarkMined, Height: cur.Height, BlockHash: strings.Repeat("ee", 32)}); err != nil {
			t.Fatal(err)
		}
	})
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if tc == "" {
		t.Fatal("the hook did not run")
	}
	if _, ok := e.fwd(ta, 0); !ok {
		t.Fatal("a is not recorded")
	}
	if _, ok := e.fwd(tb, 0); !ok {
		t.Fatal("b is not recorded")
	}
	if _, ok := e.fwd(tc, 0); ok || e.rail(ob).Paid {
		t.Fatalf("c recorded without a reading: %+v", e.rail(ob))
	}
	e.svc.hook = nil
	e.node.Mine(1)
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fwd(tc, 0); !ok || !e.rail(ob).Paid || e.eventCount(ob, store.EventPaid) != 1 {
		t.Fatalf("c not recorded by the pass that read it: %+v %+v", e.rail(ob), e.events(ob))
	}
	e.safety(oa, ob)
}

// An output first seen with no confirmation and read a second time (the re-read before a dead mark) carries the count
// of the second answer: a block between the two reads gives it 1, so a tier-1 order is recorded in that pass. The
// first answer's count (0 by construction) would leave it for the next pass.
func TestSecondReadingCarriesTheObservation(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000)
	txid := e.payOrder(o, 10_000)
	e.node.Mine(1)
	e.onceAt("scan:after-record", func() { e.node.Invalidate(1) }) // the payment is back in the mempool for the pass
	e.onceAt("track:before-mempool", func() { e.node.Mine(1) })    // and mined again before the second reading
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if m, ok := e.mark(txid); !ok || m.Kind != store.MarkMined {
		t.Fatalf("mark %+v %v, want mined", m, ok)
	}
	if _, ok := e.fwd(txid, 0); !ok || !e.rail(o).Paid {
		t.Fatalf("not recorded at the re-read's count: %+v", e.rail(o))
	}
	e.safety(o)
}
