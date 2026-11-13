package monero

import (
	"errors"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
)

// A mined mark persisted before a crash (track:after-mark), then a restart on a shorter
// chain where the transaction is absent and the wallet is below the cursor (catching up). The wallet does not know
// the payment this pass (errNotKnown), yet settle reads the old mined mark and counts confirmations from the lower
// wallet height: if they reach the tier, the payment is newly recorded and the order reads paid.
func TestUnrefreshedMarkIsNotNewlyRecordedWhileCatchingUp(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(30)
	o := e.order(2 * xmr) // tier 3
	a := e.addr(o)
	e.pass()
	txid := e.w.Pay(a, 2*xmr)
	e.w.Mine(4) // mined at 31, wallet height 35: 4 confirmations
	e.clk.Advance(time.Second)
	fired := e.crashAt("track:after-mark")
	if err := e.pass(); !errors.Is(err, errCrash) || !*fired {
		t.Fatalf("no crash: %v", err)
	}
	m, ok := e.mark(o, txid)
	if !ok || m.Kind != store.MarkMined {
		t.Fatalf("mark %+v %v", m, ok)
	}
	if _, ok := e.fwd(o, txid, 0); ok {
		t.Fatal("recorded before the crash point")
	}
	cur := e.cursor().Height
	// The restart is on a shorter chain without the transaction: wallet height 34, below the cursor.
	e.w.PopDrop(4)
	e.w.MineEmpty(3)
	if h := e.w.Height(); h >= cur || h-m.Height < 3 {
		t.Fatalf("setup: wallet %d, cursor %d, mark %d", h, cur, m.Height)
	}
	if l, _ := e.w.GetTransferByTxID(bg, txid, 0); len(l) != 0 {
		t.Fatalf("setup: the wallet still knows the transaction: %+v", l)
	}
	e.restart()
	e.clk.Advance(time.Second)
	e.pass()
	if _, ok := e.fwd(o, txid, 0); ok {
		t.Errorf("a payment the wallet does not know was newly recorded from its unrefreshed mark (mark %d, wallet %d, cursor %d)",
			m.Height, e.w.Height(), cur)
	}
	if n := e.eventCount(o, store.EventPaid); n != 0 || e.view(o).Paid {
		t.Errorf("order.paid %d, view %+v", n, e.view(o))
	}
}

// The same without a crash: the mark is written while the wallet is already catching up (1 confirmation, below the
// tier), then the wallet's chain loses the transaction and grows, still below the cursor.
func TestUnrefreshedMarkWithoutCrash(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(40)
	o := e.order(2 * xmr) // tier 3
	a := e.addr(o)
	e.pass()        // cursor 41
	e.w.PopDrop(10) // the node moves to a shorter chain: height 31, below the cursor
	txid := e.w.Pay(a, 2*xmr)
	e.w.Mine(1) // mined at 31, height 32: 1 confirmation of 3
	e.clk.Advance(time.Second)
	e.pass()
	m, ok := e.mark(o, txid)
	if !ok || m.Kind != store.MarkMined || e.cursor().Height != 41 {
		t.Fatalf("setup: mark %+v %v, cursor %+v", m, ok, e.cursor())
	}
	if _, ok := e.fwd(o, txid, 0); ok {
		t.Fatal("setup: recorded at 1 confirmation")
	}
	e.w.PopDrop(1)
	e.w.MineEmpty(3) // height 34, without the transaction, below the cursor
	e.clk.Advance(time.Second)
	e.pass()
	if _, ok := e.fwd(o, txid, 0); ok {
		t.Errorf("newly recorded from an unrefreshed mark without a crash (mark %d, wallet %d)", m.Height, e.w.Height())
	}
}

// Ruling RU keeps the narrow rule: only a payment the wallet did not know in this pass loses its new forward. A late
// payment (first seen after the expiry) whose mined mark at its need was stored before a crash, and whose order's watch
// window then closed, is no longer polled (late, so not stuck), so the wallet never answers for it again; it is still
// recorded from its mark, late, with order.late, when its order is next settled (here because an on-time payment of
// the same order, never mined, is still tracked). The strict rule ("refreshed in this pass") would never record it.
func TestLatePastWindowMarkStillRecorded(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr) // tier 1
	a := e.addr(o)
	onTime := e.w.Pay(a, xmr/2) // stays in the pool: tracked past the window
	e.pass()
	e.pastExpiry(o)
	e.clk.Advance(time.Second)
	late := e.w.Pay(a, xmr/2)
	e.pass() // discovers the late payment (in the pool) and marks it
	if m, ok := e.mark(o, late); !ok || m.Kind != store.MarkMempool {
		t.Fatalf("setup: late mark %+v %v", m, ok)
	}
	e.w.Drop(onTime)
	e.w.Mine(1) // the late payment mined at its need; the on-time one stays out of the block
	e.w.Rebroadcast(onTime)
	e.clk.Advance(time.Second)
	fired := e.crashAt("track:after-mark")
	if err := e.pass(); !errors.Is(err, errCrash) || !*fired {
		t.Fatalf("no crash: %v", err)
	}
	if m, ok := e.mark(o, late); !ok || m.Kind != store.MarkMined {
		t.Fatalf("setup: late mark %+v %v", m, ok)
	}
	if _, ok := e.fwd(o, late, 0); ok {
		t.Fatal("setup: recorded before the crash point")
	}
	e.toWindowEnd(o)
	e.clk.Advance(time.Second)
	if e.watched(o, late) || !e.watched(o, onTime) {
		t.Fatalf("setup: watched late %v, on time %v", e.watched(o, late), e.watched(o, onTime))
	}
	e.restart()
	e.pass()
	f, ok := e.fwd(o, late, 0)
	if !ok || f.Code != codeLate || !e.hasEvent(o, store.EventLate, 1) {
		t.Fatalf("the late payment's mined mark was not recorded: forward %+v %v, events %+v", f, ok, e.events(o))
	}
	e.safety(o)
}
