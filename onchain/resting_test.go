package onchain

import (
	"testing"
	"time"

	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

// toFinal moves past the order's expiry and runs passes until the order has its final event.
func (e *env) toFinal(o store.Order) {
	e.t.Helper()
	e.pastExpiry(o)
	for range 10 {
		e.clk.Advance(10 * time.Second)
		e.mustPass()
		for _, ev := range e.events(o) {
			if ev.Type == store.EventPaid || ev.Type == store.EventExpired || ev.Type == store.EventUnderpaid {
				return
			}
		}
	}
	e.t.Fatalf("no final event: %+v", e.events(o))
}

// tick runs n passes, 10 seconds apart.
func (e *env) tick(n int) {
	e.t.Helper()
	for range n {
		e.clk.Advance(10 * time.Second)
		e.mustPass()
	}
}

// restUntilFar runs passes until the output rests with its next read more than 100 seconds ahead.
func (e *env) restUntilFar(txid string) {
	e.t.Helper()
	for range 100 {
		if p, ok := e.svc.deadPolls[e.key(txid)]; ok && p.next.Sub(e.clk.Now()) > 100*time.Second {
			return
		}
		e.tick(1)
	}
	e.t.Fatal("the output never rested")
}

func TestDeadAfterFinalPolledWithBackoff(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(30_000)
	tx := e.payOrder(o, 30_000)
	e.mustPass()
	e.node.Replace(tx, false)
	e.mustPass()
	e.toFinal(o) // order.expired
	c := e.node.TxCalls(tx)
	e.tick(31)
	if got := e.node.TxCalls(tx) - c; got != 5 {
		t.Fatalf("%d reads, want 5 (passes 0, 2, 6, 14, 30)", got)
	}
	if e.rail(o).Stale {
		t.Fatal("a resting output made the order stale")
	}
	e.safety(o)
}

func TestDeadBeforeFinalIsReadEveryPass(t *testing.T) {
	e := newWatchEnv(t)
	e.node.Mine(1)
	o := e.mixed(21_000, 2_000_000)
	tx := e.payOrder(o, 2_000_000)
	e.mustPass()
	e.pastExpiry(o)
	e.mustPass()
	e.node.Evict(tx)
	e.tick(1)
	c := e.node.TxCalls(tx)
	e.tick(5)
	if got := e.node.TxCalls(tx) - c; got != 5 {
		t.Fatalf("%d reads in 5 passes, want 5", got)
	}
	if len(e.events(o)) != 0 {
		t.Fatalf("events %+v", e.events(o))
	}
	e.safety(o)
}

func TestBackoffIsCappedAtDeadPollMax(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(30_000)
	tx := e.payOrder(o, 30_000)
	e.mustPass()
	e.node.Replace(tx, false)
	e.mustPass()
	e.toFinal(o)
	e.tick(31)
	c := e.node.TxCalls(tx)
	e.tick(180)
	if got := e.node.TxCalls(tx) - c; got != 3 {
		t.Fatalf("%d reads in 180 passes, want 3 (one every 600 s)", got)
	}
	if p := e.svc.deadPolls[e.key(tx)]; p.next.Sub(p.readAt) != deadPollMax {
		t.Fatalf("backoff %+v", p)
	}
	e.safety(o)
}

// A dead output that the scan lists mined is read in that pass, whatever its backoff.
func TestRestingOutputListedMinedIsReadInThatPass(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(30_000) // tier 1
	tx := e.payOrder(o, 30_000)
	e.mustPass()
	e.node.Evict(tx)
	e.tick(1)
	e.toFinal(o) // order.expired
	e.restUntilFar(tx)
	e.node.Rebroadcast(tx)
	e.node.Mine(1)
	c := e.node.TxCalls(tx)
	e.tick(1)
	if got := e.node.TxCalls(tx) - c; got != 1 {
		t.Fatalf("%d reads in the pass that lists it mined, want 1", got)
	}
	if m, _ := e.mark(tx); m.Kind != store.MarkMined {
		t.Fatalf("mark %+v", m)
	}
	if _, ok := e.fwd(tx, 0); !ok || !e.hasEvent(o, store.EventPaid, 1) || e.count(logPaidAfterFinal) != 1 {
		t.Fatalf("not recorded: %+v\n%s", e.events(o), e.log)
	}
	if _, ok := e.svc.deadPolls[e.key(tx)]; ok {
		t.Fatal("a backoff entry of a live output")
	}
	e.safety(o)
}

// A reversed 0-tier payment that comes back to the mempool without a block is seen within deadPollMax.
func TestRestingOutputBackInMempoolSeenWithinTheCap(t *testing.T) {
	e := newZeroConfEnv(t)
	o, tx := e.zeroConfPaid()
	e.node.Evict(tx)
	e.tick(2)
	if _, ok := e.rev(tx, 0); !ok || e.eventCount(o, store.EventReverted) != 1 {
		t.Fatalf("not reversed: %+v", e.events(o))
	}
	e.restUntilFar(tx)
	e.node.Rebroadcast(tx)
	e.tick(1)
	if _, ok := e.fwd(tx, 1); ok {
		t.Fatal("re-recorded before the next due read")
	}
	for range 60 {
		if _, ok := e.fwd(tx, 1); ok {
			break
		}
		e.tick(1)
	}
	if _, ok := e.fwd(tx, 1); !ok || e.eventCount(o, store.EventPaidAgain) != 1 {
		t.Fatalf("not recorded again: %+v", e.events(o))
	}
	e.safety(o)
}

// A recorded output that dies is read every pass until its reversal is written; only then does it rest.
func TestRecordedDeadOutputDoesNotRest(t *testing.T) {
	e := newZeroConfEnv(t)
	o, tx := e.zeroConfPaid()
	e.node.Evict(tx)
	for i := range 2 {
		c := e.node.TxCalls(tx)
		e.tick(1)
		if e.node.TxCalls(tx)-c < 1 {
			t.Fatalf("pass %d did not read the recorded dead output", i)
		}
	}
	if _, ok := e.rev(tx, 0); !ok {
		t.Fatal("not reversed")
	}
	c := e.node.TxCalls(tx)
	e.tick(1) // the first read after the reversal
	d := e.node.TxCalls(tx) - c
	if d < 1 {
		t.Fatal("the reversed output was not read")
	}
	e.tick(1)
	if e.node.TxCalls(tx)-c != d {
		t.Fatal("the reversed output did not rest")
	}
	e.safety(o)
}

func TestDeadOutputReadAtWindowEnd(t *testing.T) {
	e := newWatchEnv(t)
	e.node.Mine(1) // a settled fact needs a height of at least 1
	o := e.order(30_000)
	tx := e.payOrder(o, 30_000)
	e.mustPass()
	e.node.Evict(tx)
	e.tick(1)
	e.toFinal(o)
	e.restUntilFar(tx)
	end := o.ExpiresAt.Add(store.WatchWindow)
	k := e.key(tx)
	e.svc.deadPolls[k] = deadPoll{step: 6, next: end.Add(time.Hour), readAt: e.clk.Now()}
	e.clk.Advance(end.Sub(e.clk.Now()) - 10*time.Second)
	c := e.node.TxCalls(tx)
	e.mustPass()
	if e.node.TxCalls(tx) != c || !e.watched(tx) {
		t.Fatal("read before the window's end")
	}
	e.clk.Advance(10 * time.Second)
	e.mustPass()
	if e.node.TxCalls(tx)-c != 1 || e.watched(tx) || e.count(alertLeftPool) != 1 {
		t.Fatalf("at the window's end: %d reads, watched %v, %d ALERTs\n%s", e.node.TxCalls(tx)-c, e.watched(tx), e.count(alertLeftPool), e.log)
	}
	e.tick(3)
	if e.node.TxCalls(tx)-c != 1 || e.count(alertLeftPool) != 1 {
		t.Fatal("read or alerted after it settled")
	}
	e.safety(o)
}

func TestBackoffRestartsAfterARestart(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(30_000)
	tx := e.payOrder(o, 30_000)
	e.mustPass()
	e.node.Evict(tx)
	e.tick(1)
	e.toFinal(o)
	e.restUntilFar(tx)
	e.restart()
	c := e.node.TxCalls(tx)
	e.tick(1)
	if e.node.TxCalls(tx)-c != 1 {
		t.Fatal("not read in the first pass after the restart")
	}
	e.tick(1)
	if e.node.TxCalls(tx)-c != 1 {
		t.Fatal("read again too soon")
	}
	e.tick(1)
	if e.node.TxCalls(tx)-c != 2 {
		t.Fatal("not read 2 passes later")
	}
	e.safety(o)
}

func TestThousandsOfDeadOutputsCostLittle(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(20_000)
	outs := make([]onchaintest.Out, 2_000)
	for i := range outs {
		outs[i] = onchaintest.Out{Address: e.addr(o), Sat: 10}
	}
	tx := e.node.PayWith(1, outs)
	e.mustPass()
	e.node.Replace(tx, false)
	e.mustPass()
	e.toFinal(o)
	c := e.node.TxCalls(tx)
	b := e.order(10_000)
	tb := e.payOrder(b, 10_000)
	e.node.Mine(1)
	e.tick(1)
	if _, ok := e.fwd(tb, 0); !ok {
		t.Fatal("the other order was not recorded in the pass after its block")
	}
	e.tick(19)
	if got := e.node.TxCalls(tx) - c; got > 6 {
		t.Fatalf("%d reads of the dead transaction in 20 passes", got)
	}
	e.safety(b)
}

func TestRestingOutputsDoNotCountTowardTheCap(t *testing.T) {
	e := newWatchEnv(t)
	final := e.order(120_000)
	open := e.mixed(21_000, 2_000_000)
	e.node.Mine(1)
	for _, o := range []store.Order{final, open} {
		for range 12 {
			e.node.Replace(e.payOrder(o, 10_000), false)
		}
	}
	e.mustPass()
	e.toFinal(final)
	e.tick(1) // every dead output of `final` has been read once since its final event
	e.cap(8)
	e.tick(1)
	if e.count(logTooWide) != 1 || !e.logHas(`msg="`+logTooWide+`" order=`+open.ID) {
		t.Fatalf("alerts %d\n%s", e.count(logTooWide), e.log)
	}
	if e.logHas(`msg="` + logTooWide + `" order=` + final.ID) {
		t.Fatal("the order with a final event is too wide")
	}
	if e.stale(final) {
		t.Fatal("the order with resting outputs is stale")
	}
}
