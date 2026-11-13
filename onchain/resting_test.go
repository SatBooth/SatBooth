package onchain

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/satbooth/satbooth/ledger"
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
	outs := make([]onchaintest.Out, 300)
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

// deadTxs pays the order n times and replaces each payment away, and returns the transactions.
func (e *env) deadTxs(o store.Order, n int) []string {
	e.t.Helper()
	var txs []string
	for range n {
		tx := e.payOrder(o, 10_000)
		txs = append(txs, tx)
	}
	e.mustPass()
	for _, tx := range txs {
		e.node.Replace(tx, false)
	}
	e.mustPass()
	return txs
}

// reads is the number of the transactions that were read since calls.
func (e *env) readSince(txs []string, calls map[string]int) int {
	n := 0
	for _, tx := range txs {
		if e.node.TxCalls(tx) > calls[tx] {
			n++
		}
	}
	return n
}

func (e *env) callsOf(txs []string) map[string]int {
	m := map[string]int{}
	for _, tx := range txs {
		m[tx] = e.node.TxCalls(tx)
	}
	return m
}

// finalWithDead is an order with n dead transactions and its final event, each read once since it (so each has a
// backoff entry), and the clock 30 seconds on: all of them are due.
func (e *env) finalWithDead(n int) (store.Order, []string) {
	e.t.Helper()
	e.node.Mine(1)
	o := e.order(10_000 * int64(n))
	txs := e.deadTxs(o, n)
	e.toFinal(o)
	e.tick(1)
	e.clk.Advance(30 * time.Second)
	return o, txs
}

func TestDueRestingOutputsAreReadWithinTheCap(t *testing.T) {
	e := newWatchEnv(t)
	o, txs := e.finalWithDead(12)
	e.cap(8)
	c := e.callsOf(txs)
	e.mustPass()
	if e.count(logTooWide) != 0 || e.stale(o) {
		t.Fatalf("too wide %d, stale %v", e.count(logTooWide), e.stale(o))
	}
	if n := e.readSince(txs, c); n != 8 {
		t.Fatalf("%d transactions read in the first pass, want 8", n)
	}
	e.tick(1)
	if n := e.readSince(txs, c); n != 12 || e.count(logTooWide) != 0 || e.stale(o) {
		t.Fatalf("%d transactions read in 2 passes, want 12; too wide %d, stale %v", n, e.count(logTooWide), e.stale(o))
	}
	e.safety(o)
}

func TestRestingOutputsAfterARestartAreReadWithinTheCap(t *testing.T) {
	e := newWatchEnv(t)
	o, txs := e.finalWithDead(12)
	e.restart()
	e.cap(8)
	c := e.callsOf(txs)
	e.mustPass()
	e.tick(1)
	if n := e.readSince(txs, c); n != 12 || e.count(logTooWide) != 0 || e.stale(o) {
		t.Fatalf("%d transactions read in 2 passes after the restart, want 12; too wide %d, stale %v", n, e.count(logTooWide), e.stale(o))
	}
	e.safety(o)
}

func TestRestingOutputsAtTheWindowEndAreReadWithinTheCap(t *testing.T) {
	e := newWatchEnv(t)
	o, txs := e.finalWithDead(12)
	end := o.ExpiresAt.Add(store.WatchWindow)
	for _, tx := range txs {
		e.svc.deadPolls[e.key(tx)] = deadPoll{step: 9, next: end.Add(time.Hour), readAt: e.clk.Now()}
	}
	e.cap(8)
	e.clk.Advance(end.Sub(e.clk.Now()))
	e.mustPass()
	e.tick(1)
	for _, tx := range txs {
		if e.watched(tx) {
			t.Fatal("a payment is still watched two passes past the window's end")
		}
	}
	if e.count(logTooWide) != 0 || e.count(alertLeftPool) != 12 {
		t.Fatalf("too wide %d, ALERTs %d", e.count(logTooWide), e.count(alertLeftPool))
	}
	e.safety(o)
}

// A failed events read leaves the order's dead outputs to be read from the resting budget: no too-wide ALERT, the
// order is stale for that pass and served after.
func TestFailedEventsReadDoesNotMakeAnOrderTooWide(t *testing.T) {
	e := newWatchEnv(t)
	o, txs := e.finalWithDead(12)
	e.cap(8)
	failed := false
	e.svc.eventsFor = func(ctx context.Context, id string) ([]store.Event, error) {
		if id == o.ID && !failed {
			failed = true
			return nil, errors.New("events read failed")
		}
		return e.st.EventsForOrder(ctx, id)
	}
	c := e.callsOf(txs)
	e.mustPass()
	if !failed || e.count(logTooWide) != 0 || !e.stale(o) || !e.logHas(`msg="`+logTrackFailed+`" at=events`) {
		t.Fatalf("failed %v, too wide %d, stale %v\n%s", failed, e.count(logTooWide), e.stale(o), e.log)
	}
	if n := e.readSince(txs, c); n == 0 || n > 8 {
		t.Fatalf("%d transactions read in the failed pass", n)
	}
	e.tick(2)
	if n := e.readSince(txs, c); n != 12 || e.stale(o) || e.count(logTooWide) != 0 {
		t.Fatalf("%d read after, stale %v", n, e.stale(o))
	}
}

// A dead output whose record does not match its payment is foreign: it never rests.
func TestForeignDeadOutputNeverRests(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(30_000)
	tx := e.payOrder(o, 30_000)
	e.mustPass()
	e.node.Replace(tx, false)
	e.mustPass()
	e.toFinal(o)
	e.forward(o, tx, e.txo(tx).Vout, 0, 1) // a record under its id with other details
	c := e.node.TxCalls(tx)
	e.tick(10)
	if got := e.node.TxCalls(tx) - c; got < 10 {
		t.Fatalf("%d reads in 10 passes, want one at least in each", got)
	}
	if _, ok := e.svc.deadPolls[e.key(tx)]; ok {
		t.Fatal("a foreign output has a backoff entry")
	}
}

// brokenLedger is a ledger whose calls fail.
func (e *env) brokenLedger() *ledger.Ledger {
	e.t.Helper()
	lg, err := ledger.Open(bg, filepath.Join(e.t.TempDir(), "broken.db"))
	if err != nil {
		e.t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		e.t.Fatal(err)
	}
	return lg
}

// A failed events read, when step 3 chooses its work, holds the order (no final event in that pass) and leaves the pass
// unstamped, whether or not the order is too wide.
func TestFailedEventsReadHoldsTheOrder(t *testing.T) {
	e := newWatchEnv(t)
	e.node.Mine(1)
	o := e.order(120_000)
	txs := e.deadTxs(o, 12)
	e.cap(8)
	e.pastExpiry(o)
	e.tick(1)
	if e.count(logTooWide) != 1 || len(e.events(o)) != 0 {
		t.Fatalf("setup: too wide %d, events %+v", e.count(logTooWide), e.events(o))
	}
	failed := false
	e.svc.eventsFor = func(ctx context.Context, id string) ([]store.Event, error) {
		if id == o.ID && !failed {
			failed = true
			return nil, errors.New("events read failed")
		}
		return e.st.EventsForOrder(ctx, id)
	}
	good := e.svc.lastGoodPass.Load()
	c := e.callsOf(txs)
	e.tick(1)
	if !failed {
		t.Fatal("the events read was not made")
	}
	if len(e.events(o)) != 0 {
		t.Fatalf("a final event from a pass that read part of the order: %+v (%d of 12 read)", e.events(o), e.readSince(txs, c))
	}
	if e.svc.lastGoodPass.Load() != good {
		t.Fatal("a pass whose events read failed was stamped good")
	}
	if _, held := e.svc.heldSeen[o.ID]; !held {
		t.Fatal("the order was not held")
	}
}

func TestFailedLedgerReadHoldsTheOrder(t *testing.T) {
	e := newWatchEnv(t)
	o, txs := e.finalWithDead(12)
	e.cap(8)
	good := e.svc.lastGoodPass.Load()
	real := e.svc.lg
	e.svc.hook = func(p string) error {
		switch p {
		case "scan:after-record":
			e.svc.lg = e.brokenLedger()
		case "track:before-mempool":
			e.svc.lg = real
		}
		return nil
	}
	c := e.callsOf(txs)
	e.mustPass()
	e.svc.hook, e.svc.lg = nil, real
	if !e.logHas(`msg="`+logTrackFailed+`" at=ledger`) || e.count(logTooWide) != 0 {
		t.Fatalf("too wide %d\n%s", e.count(logTooWide), e.log)
	}
	if e.svc.lastGoodPass.Load() != good {
		t.Fatal("a pass whose ledger read failed was stamped good")
	}
	if _, held := e.svc.heldSeen[o.ID]; !held || !e.stale(o) {
		t.Fatalf("held %v, stale %v", held, e.stale(o))
	}
	if n := e.readSince(txs, c); n == 0 || n > 8 {
		t.Fatalf("%d transactions read", n)
	}
	e.tick(2)
	if e.stale(o) || e.readSince(txs, c) != 12 {
		t.Fatalf("not served after: stale %v, %d read", e.stale(o), e.readSince(txs, c))
	}
}

// An order with live transactions and due resting ones, more together than a pass reads, is served: only the live
// ones count toward the cap.
func TestLiveAndRestingTogetherOverTheCapAreServed(t *testing.T) {
	e := newWatchEnv(t)
	e.node.Mine(1)
	o := e.order(200_000)
	var live, dead []string
	for range 8 {
		dead = append(dead, e.payOrder(o, 10_000))
	}
	for range 5 {
		live = append(live, e.payOrder(o, 1_000))
	}
	e.mustPass()
	for _, tx := range dead {
		e.node.Replace(tx, false)
	}
	e.mustPass()
	e.toFinal(o)
	e.tick(1)
	e.clk.Advance(30 * time.Second)
	e.cap(8)
	all := append(slices.Clone(live), dead...)
	c := e.callsOf(all)
	e.mustPass()
	if e.count(logTooWide) != 0 || e.stale(o) {
		t.Fatalf("too wide %d, stale %v", e.count(logTooWide), e.stale(o))
	}
	if n := e.readSince(all, c); n != 13 {
		t.Fatalf("%d of 13 transactions read, want all (5 live, 8 resting)", n)
	}
}

// The backoff reaches deadPollMax exactly, at every allowed poll interval.
func TestBackoffGapsReachDeadPollMax(t *testing.T) {
	for _, pi := range []time.Duration{MinPollInterval, 5 * time.Second, DefaultPollInterval, 45 * time.Second, MaxPollInterval} {
		s := &Service{cfg: Config{PollInterval: pi}, deadPolls: map[store.TXOKey]deadPoll{}}
		k := store.TXOKey{TxID: "a", Vout: 0}
		now := time.Unix(1_700_000_000, 0)
		var last time.Duration
		for n := 1; n <= 14; n++ {
			s.polledDead(k, true, now)
			p := s.deadPolls[k]
			gap := p.next.Sub(now)
			if want := min(pi<<n, deadPollMax); gap != want {
				t.Fatalf("poll %v read %d: gap %v, want %v", pi, n, gap, want)
			}
			last = gap
			now = p.next
		}
		if last != deadPollMax {
			t.Fatalf("poll %v: the gaps end at %v, want %v", pi, last, deadPollMax)
		}
		s.polledDead(k, false, now)
		if _, ok := s.deadPolls[k]; ok {
			t.Fatal("an alive read kept the entry")
		}
	}
}
