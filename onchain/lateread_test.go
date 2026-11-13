package onchain

import (
	"maps"
	"math/rand"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/store"
)

// lateAtWindowEnd is the setup of the late-read tests: a 20,000,000-sat order (6 confirmations), a late output mined
// once while the order's window was open (so it has a mined mark), and the clock at the window end.
func lateAtWindowEnd(e *env) (o store.Order, addr, late string) {
	e.t.Helper()
	o = e.order(20_000_000)
	addr = e.addr(o)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	late = e.node.Pay(addr, 20_000_000)
	e.node.Mine(1)
	e.passes(1)
	if m, ok := e.mark(late); !ok || m.Kind != store.MarkMined {
		e.t.Fatalf("setup: mark %+v %v", m, ok)
	}
	if _, ok := e.fwd(late, 0); ok {
		e.t.Fatal("setup: recorded at 1 confirmation")
	}
	e.toWindowEnd(o)
	e.clk.Advance(time.Second)
	if e.watched(late) {
		e.t.Fatal("setup: still watched past the window")
	}
	return o, addr, late
}

// A late output past its order's watch window is not watched, but when its order is settled again (here by a new output
// on the address) it is read in that pass, before it is recorded: recorded late from that reading, then settled, and
// never read again. It is never counted.
func TestLateOutputPastItsWindowIsReadBeforeItIsRecorded(t *testing.T) {
	e := newWatchEnv(t)
	o, a, late := lateAtWindowEnd(e)
	e.node.Mine(6)
	e.node.Pay(a, 1_000) // a new row on the address: the order is settled again
	n := e.node.TxCalls(late)
	e.pass()
	if got := e.node.TxCalls(late) - n; got != 1 {
		t.Fatalf("%d gettransaction calls for the late output, want 1", got)
	}
	if f, ok := e.fwd(late, 0); !ok || f.Code != codeLate || !e.hasEvent(o, store.EventLate, 1) {
		t.Fatalf("not recorded late: %v %+v", ok, e.events(o))
	}
	if r := e.rail(o); r.Paid || r.ConfirmedSat != 0 {
		t.Fatalf("a late output counted: %+v", r)
	}
	if s, err := e.st.SettledKeys(bg, []store.TXOKey{{TxID: late}}); err != nil || !s[store.TXOKey{TxID: late}] {
		t.Fatalf("not settled at 7 confirmations: %v %v", s, err)
	}
	e.node.Pay(a, 2_000)
	n = e.node.TxCalls(late)
	e.clk.Advance(time.Second)
	e.pass()
	if e.node.TxCalls(late) != n {
		t.Fatal("the recorded, settled late output was read again")
	}
	e.safety(o)
}

// A late output that left the chain after its mined mark is read before it is recorded: the cursor's count would
// record it, the reading (a double spend) does not, and no ALERT says an on-time payment left the pool.
func TestLateOutputPastItsWindowGoneIsNotRecorded(t *testing.T) {
	e := newWatchEnv(t)
	o, a, late := lateAtWindowEnd(e)
	e.node.Invalidate(1) // back in the mempool
	e.node.Replace(late, false)
	e.node.Mine(7)
	e.node.Pay(a, 1_000)
	n := e.node.TxCalls(late)
	e.pass()
	if got := e.node.TxCalls(late) - n; got != 1 {
		t.Fatalf("%d gettransaction calls for the late output, want 1", got)
	}
	if m, ok := e.mark(late); !ok || m.Kind != store.MarkDead {
		t.Fatalf("mark %+v %v, want dead", m, ok)
	}
	if _, ok := e.fwd(late, 0); ok || e.hasEvent(o, store.EventLate, 1) {
		t.Fatalf("a gone output was recorded: %+v", e.events(o))
	}
	if e.count(logLeftPool) != 0 {
		t.Fatalf("the left-the-pool ALERT for a late output:\n%s", e.log)
	}
	// A dead late output is not read again, however often its order is touched: it could not be recorded, and a read
	// would cost calls for ever.
	e.node.Pay(a, 2_000)
	n = e.node.TxCalls(late)
	e.clk.Advance(time.Second)
	e.pass()
	if got := e.node.TxCalls(late) - n; got != 0 {
		t.Fatalf("%d gettransaction calls for a dead late output in a later pass, want 0", got)
	}
	e.safety(o)
}

// A late read that fails does not block the on-time payment of the same order: the late output is not recorded (its
// reading is missing), the order is stale for the pass, and the on-time output is recorded in the same pass.
func TestLateReadFailureDoesNotBlockTheOnTimePayment(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(2_000_000) // 3 confirmations
	a := e.addr(o)
	onTime := e.node.Pay(a, 2_000_000)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	late := e.node.Pay(a, 500_000)
	e.node.Mine(1)
	e.passes(1) // both mined at 1: neither at its need
	if m, ok := e.mark(late); !ok || m.Kind != store.MarkMined {
		t.Fatalf("setup: late mark %+v %v", m, ok)
	}
	e.toWindowEnd(o)
	e.clk.Advance(time.Second)
	e.node.FailTx(late, &btcrpc.Error{Code: -5, Method: "gettransaction"})
	e.node.Mine(2)
	nOnTime := e.node.TxCalls(onTime)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fwd(onTime, 0); !ok || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("the on-time payment is not recorded: %+v", e.events(o))
	}
	if _, ok := e.fwd(late, 0); ok {
		t.Fatal("the late output was recorded without a reading")
	}
	if got := e.node.TxCalls(onTime) - nOnTime; got != 1 {
		t.Fatalf("%d gettransaction calls for the on-time output, want 1 (read twice in a pass)", got)
	}
	e.svc.staleMu.Lock()
	_, stale := e.svc.staleOrders[o.ID]
	e.svc.staleMu.Unlock()
	if !stale || !e.rail(o).Stale {
		t.Fatal("the order is not in the stale set")
	}
	e.node.MendTx(late)
	e.clk.Advance(time.Second)
	e.pass()
	if _, ok := e.fwd(late, 0); !ok {
		t.Fatal("the late output is not recorded once it is read")
	}
	e.safety(o)
}

// The count shown is the one the paid decision uses: an on-time mined output with no observation of the last pass
// shows 0 and is not paid, and the next pass reads it and shows its count.
func TestShownCountAgreesWithThePaidDecision(t *testing.T) {
	e := newWatchEnv(t)
	oa, ob := e.order(10_000), e.order(20_000)
	e.payOrder(oa, 10_000)
	e.clk.Advance(time.Second)
	e.payOrder(ob, 10_000)
	e.node.Mine(1)
	var tc string
	e.onceAt("track:after-mark", func() { // c gets a row and a mined mark the pass never reads
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
	if r := e.rail(ob); r.Confirmations != 0 || r.Paid {
		t.Fatalf("shown %d confirmations, paid %v: want 0 and not paid", r.Confirmations, r.Paid)
	}
	e.svc.hook = nil
	e.node.Mine(1)
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if r := e.rail(ob); !r.Paid || r.Confirmations < 1 {
		t.Fatalf("after the reading: %+v", r)
	}
	e.safety(oa, ob)
}

// After a restart nothing unread counts: the stored cursor is high, but no pass has read the output, so it shows 0 and
// settle records nothing; a pass that reads it below its need records nothing; one that reads it at its need does.
func TestRestartCountsNothingUnread(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(20_000_000) // 6 confirmations
	txid := e.payOrder(o, 20_000_000)
	e.node.Mine(3)
	e.pass()
	e.restart()
	cur := e.cursor()
	high := store.Cursor{BlockHash: e.node.Tip(), Height: cur.Height + 100, At: e.clk.Now()}
	if _, err := e.st.RecordScan(bg, nil, high); err != nil {
		t.Fatal(err)
	}
	if r := e.rail(o); !r.Stale || r.Confirmations != 0 {
		t.Fatalf("before any pass: %+v, want stale with 0 confirmations", r)
	}
	if err := e.svc.settle(bg, o); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fwd(txid, 0); ok || e.rail(o).Paid {
		t.Fatal("recorded on the stored cursor's count, unread")
	}
	e.node.Invalidate(2) // bitcoind says 1
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fwd(txid, 0); ok {
		t.Fatal("recorded below its need")
	}
	e.node.Mine(5)
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fwd(txid, 0); !ok || !e.rail(o).Paid {
		t.Fatalf("not recorded at its need: %+v", e.rail(o))
	}
	e.safety(o)
}

// An output mined again at the same height in another block needs its own observation: the old one (same height, the
// old block's hash) is not applied to the new mark.
func TestReminedAtTheSameHeightNeedsItsOwnObservation(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(20_000_000) // 6 confirmations
	txid := e.payOrder(o, 20_000_000)
	e.node.Mine(1)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	x, ok := e.mark(txid)
	if !ok || x.Kind != store.MarkMined {
		t.Fatalf("setup: %+v %v", x, ok)
	}
	e.node.Invalidate(1)
	e.node.Mine(6) // mined at the same height, in another block
	fired := e.crashAt("track:after-mark")
	e.clk.Advance(time.Second)
	e.pass()
	if !*fired {
		t.Fatal("the hook did not run")
	}
	y, _ := e.mark(txid)
	if y.Height != x.Height || y.BlockHash == x.BlockHash || y.BlockHash != e.node.BlockHash(y.Height) {
		t.Fatalf("setup: marks %+v then %+v", x, y)
	}
	if r := e.rail(o); r.Confirmations != 0 {
		t.Fatalf("the old block's observation was applied: %d confirmations", r.Confirmations)
	}
	if err := e.svc.settle(bg, o); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fwd(txid, 0); ok {
		t.Fatal("recorded without a reading of the new mark")
	}
	e.svc.hook = nil
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if r := e.rail(o); !r.Paid || r.Confirmations != 6 {
		t.Fatalf("after the reading of the new mark: %+v", r)
	}
	e.safety(o)
}

// settle's orders (the past-expiry and the awaiting-paid-again selections) are inside their watch window at any clock,
// the premise of unwatched: an order past its window is settled only when a pass touched it.
func TestOrderSelectionsAreInsideTheWindow(t *testing.T) {
	e := newWatchEnv(t)
	var orders []store.Order
	for i := 1; i <= 5; i++ {
		o, _, err := e.st.CreateOrder(bg, store.Order{AccountID: "acct-1", MerchantRef: "sel-" + string(rune('a'+i)), AmountSat: 1_000,
			ExpiresAt: e.clk.Now().Add(time.Duration(i) * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.AddEvent(bg, store.Event{Type: store.EventReverted, OrderID: o.ID, Seq: 1}); err != nil {
			t.Fatal(err)
		}
		orders = append(orders, o)
	}
	start := e.clk.Now()
	r := rand.New(rand.NewSource(14))
	offsets := []time.Duration{0, 5 * time.Hour, 5*time.Hour + store.WatchWindow - time.Second, 5*time.Hour + store.WatchWindow,
		5*time.Hour + store.WatchWindow + time.Second}
	for _, o := range orders {
		for _, d := range []time.Duration{-time.Second, 0, time.Second} {
			offsets = append(offsets, o.ExpiresAt.Sub(start)+d, o.ExpiresAt.Sub(start)+store.WatchWindow+d)
		}
	}
	for i := 0; i < 200; i++ {
		offsets = append(offsets, time.Duration(r.Int63n(int64(5*time.Hour+store.WatchWindow+48*time.Hour))))
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	insideBoth := false
	for _, d := range offsets {
		now := start.Add(d)
		e.clk.Advance(now.Sub(e.clk.Now()))
		past, err := e.st.OnchainOrdersPastExpiry(bg, now)
		if err != nil {
			t.Fatal(err)
		}
		again, err := e.st.OrdersAwaitingPaidAgain(bg, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range slices.Concat(past, again) {
			if !now.Before(o.ExpiresAt.Add(store.WatchWindow)) {
				t.Fatalf("at +%s an order expiring at +%s is selected past its window", d, o.ExpiresAt.Sub(start))
			}
			insideBoth = true
		}
	}
	if !insideBoth {
		t.Fatal("no order was ever selected: the test checks nothing")
	}
}

// settled counts as the paid decision does (boundOut): an output is settled for good only at the depth of an
// observation of its own mark (its height and its block hash); with none, with another's, it counts zero.
func TestSettledNeedsTheBoundedDepth(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(10_000) // 1 confirmation
	txid := e.payOrder(o, 10_000)
	e.node.Mine(1)
	e.pass()
	if _, ok := e.fwd(txid, 0); !ok {
		t.Fatal("setup: not recorded at 1 confirmation")
	}
	e.node.Mine(10)
	e.clk.Advance(time.Second)
	cur := store.Cursor{BlockHash: e.node.Tip(), Height: e.node.Height(), At: e.clk.Now()}
	if _, err := e.st.RecordScan(bg, nil, cur); err != nil {
		t.Fatal(err)
	}
	m, _ := e.mark(txid)
	k := store.TXOKey{TxID: txid}
	out := trackedOutput{x: e.txo(txid), o: o}
	for _, c := range []struct {
		name string
		seen map[store.TXOKey]observed
		want bool
	}{
		{"no observation", nil, false},
		{"another block", map[store.TXOKey]observed{k: {height: m.Height, hash: "other", confirmations: 11}}, false},
		{"another height", map[store.TXOKey]observed{k: {height: m.Height + 1, hash: m.BlockHash, confirmations: 11}}, false},
		{"observed below the depth", map[store.TXOKey]observed{k: {height: m.Height, hash: m.BlockHash, confirmations: 2}}, false},
		{"observed deep", map[store.TXOKey]observed{k: {height: m.Height, hash: m.BlockHash, confirmations: 11}}, true},
	} {
		e.svc.seenMu.Lock()
		e.svc.seen = c.seen
		e.svc.seenMu.Unlock()
		if ok, _, _ := e.svc.settled(bg, e.clk.Now(), cur, out); ok != c.want {
			t.Errorf("%s: settled = %v, want %v", c.name, ok, c.want)
		}
	}
}

// A late output past its window whose block is reorged out is read again, in every pass that touches its order: the
// extra read marks it mempool (a mempool mark is followed too, unwatched), and once it is mined again the next touching
// pass reads it and records it late, once, from that reading. Cost per pass while it is in the mempool: one
// gettransaction and one getmempoolentry.
func TestLateOutputBackInThePoolIsReadAgain(t *testing.T) {
	e := newWatchEnv(t)
	o, a, late := lateAtWindowEnd(e)
	e.node.Invalidate(1) // back in the mempool
	e.node.Pay(a, 1_000)
	n := e.node.TxCalls(late)
	e.pass()
	if m, ok := e.mark(late); !ok || m.Kind != store.MarkMempool || e.node.TxCalls(late)-n != 1 {
		t.Fatalf("mark %+v %v, %d calls: want a mempool mark from one reading", m, ok, e.node.TxCalls(late)-n)
	}
	e.node.Pay(a, 2_000)
	n, ne := e.node.TxCalls(late), e.node.Calls("getmempoolentry")
	e.clk.Advance(time.Second)
	e.pass()
	if got, ent := e.node.TxCalls(late)-n, e.node.Calls("getmempoolentry")-ne; got != 1 || ent != 1 {
		t.Fatalf("a mempool-marked late output cost %d gettransaction and %d getmempoolentry per pass, want 1 and 1", got, ent)
	}
	e.node.Mine(8) // mined again, 8 deep
	e.node.Pay(a, 3_000)
	n = e.node.TxCalls(late)
	e.clk.Advance(time.Second)
	e.pass()
	if got := e.node.TxCalls(late) - n; got != 1 {
		t.Fatalf("%d gettransaction calls for the late output after it was mined again, want 1", got)
	}
	if f, ok := e.fwd(late, 0); !ok || f.Code != codeLate || e.eventCount(o, store.EventLate) != 1 {
		t.Fatalf("not recorded late once: %v %+v", ok, e.events(o))
	}
	e.safety(o)
}

// A late row with no mark (a payment that arrived after the window) is not read: it is not recorded, by design.
func TestLateRowWithoutAMarkIsNotRead(t *testing.T) {
	e := newWatchEnv(t)
	o, a, _ := lateAtWindowEnd(e)
	row := e.node.Pay(a, 1_000)
	e.node.Mine(1)
	for i := 0; i < 2; i++ {
		e.clk.Advance(time.Second)
		e.pass()
	}
	if _, ok := e.mark(row); ok || e.node.TxCalls(row) != 0 {
		t.Fatalf("the row of a payment after the window was read: %d calls", e.node.TxCalls(row))
	}
	e.safety(o)
}

// The pass that stops inside an extra read has published nothing: the service's observations are the previous pass's
// map (same pointer, same contents), since seenNow hands the map out without a lock and nothing may write it
// afterwards.
func TestPublishedObservationsAreNeverWritten(t *testing.T) {
	e := newWatchEnv(t)
	_, a, late := lateAtWindowEnd(e)
	e.node.Invalidate(1)
	e.node.Replace(late, false)
	e.node.Mine(7)
	e.node.Pay(a, 1_000)
	before, ptr := maps.Clone(e.svc.seenNow()), reflect.ValueOf(e.svc.seenNow()).Pointer()
	fired := e.crashAt("track:after-mark") // the first mark change of the pass is the extra read's dead mark
	e.pass()
	if !*fired {
		t.Fatal("the hook did not run")
	}
	if m, _ := e.mark(late); m.Kind != store.MarkDead {
		t.Fatalf("the extra read did not run: mark %+v", m)
	}
	if reflect.ValueOf(e.svc.seenNow()).Pointer() != ptr || !maps.Equal(e.svc.seenNow(), before) {
		t.Fatal("a pass that ended inside an extra read published its observations, or wrote the published map")
	}
}

// unwatched reads only what the pass has not read: a foreign late output costs no call, and an output of the watched
// list is read once (TestLateReadFailureDoesNotBlockTheOnTimePayment counts the latter).
func TestUnwatchedSkipsAForeignOutput(t *testing.T) {
	e := newWatchEnv(t)
	o, a, late := lateAtWindowEnd(e)
	e.forward(o, late, 0, 0, 1) // a record under its id with other details: the output is foreign
	e.node.Mine(6)
	e.node.Pay(a, 1_000)
	n := e.node.TxCalls(late)
	e.pass()
	if got := e.node.TxCalls(late) - n; got != 0 {
		t.Fatalf("%d gettransaction calls for a foreign late output, want 0", got)
	}
}
