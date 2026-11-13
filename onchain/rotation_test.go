package onchain

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

// wallStep makes step 3's deadline clock advance by d on each call.
func (e *env) wallStep(d time.Duration) {
	t := time.Unix(1_700_000_000, 0)
	e.svc.wall = func() time.Time {
		t = t.Add(d)
		return t
	}
}

// cap sets the most due transactions of one order that a pass reads.
func (e *env) cap(n int) { e.svc.passCap = n }

// stale is the rail's Stale for the order.
func (e *env) stale(o store.Order) bool { return e.rail(o).Stale }

// byTxid sorts the payments as the watched list does for outputs first seen in the same pass: by transaction id.
func byTxid(txs ...string) []string {
	s := slices.Clone(txs)
	slices.Sort(s)
	return s
}

func TestReachedOrdersSettleAndOthersWait(t *testing.T) {
	e := newWatchEnv(t)
	e.wallStep(11 * time.Second) // only the first order of each pass starts
	a, b := e.order(10_000), e.order(10_000)
	ta, tb := e.payOrder(a, 10_000), e.payOrder(b, 10_000)
	if ta > tb { // the list serves the lower transaction id first
		a, b, ta, tb = b, a, tb, ta
	}
	e.node.Mine(1)
	l := e.node.Calls("listsinceblock")
	e.mustPass()
	if _, ok := e.fwd(ta, 0); !ok {
		t.Fatal("a not recorded")
	}
	if _, ok := e.fwd(tb, 0); ok {
		t.Fatal("b recorded while not reached")
	}
	if e.node.Calls("listsinceblock")-l != 1 || e.node.TxCalls(tb) != 0 {
		t.Fatal("b read, or a second scan")
	}
	e.clk.Advance(10 * time.Second)
	e.mustPass() // starts at b
	if _, ok := e.fwd(tb, 0); !ok || e.eventCount(b, store.EventPaid) != 1 {
		t.Fatal("b not recorded on its turn")
	}
	e.safety(a, b)
}

// An order that a pass does not reach is settled from no list: not the discovery's, not the one of the orders past
// their expiry, not the one of the orders that await a paid_again.
func TestUnreachedOrderIsNotSettledFromAnyList(t *testing.T) {
	e := newWatchEnv(t)
	z := e.order(2_000_000) // tier 3: one block does not pay it, and it stays watched
	e.payOrder(z, 2_000_000)
	e.mustPasses(1)

	x := e.order(10_000)
	tx := e.payOrder(x, 10_000)
	e.mustPasses(1)
	e.node.Evict(tx)
	e.mustPasses(3) // dead, and the hold of the pass that marked it is over
	if m, ok := e.mark(tx); !ok || m.Kind != store.MarkDead {
		t.Fatalf("x mark %+v", m)
	}
	y := e.order(10_000)
	e.payOrder(y, 10_000)
	e.node.Mine(1)
	e.mustPasses(1)
	if e.eventCount(y, store.EventPaid) != 1 {
		t.Fatal("y not paid")
	}
	e.node.Invalidate(1)
	e.mustPasses(1)
	if !e.hasEvent(y, store.EventReverted, 1) {
		t.Fatalf("y not reverted: %+v", e.events(y))
	}
	e.node.Mine(1) // y's payment confirms again: its paid_again is due

	e.pastExpiry(x)

	e.wallStep(11 * time.Second) // one order per pass: z, then x, then y
	e.mustPass()                 // serves z
	if e.eventCount(x, store.EventExpired) != 0 || e.eventCount(y, store.EventPaidAgain) != 0 {
		t.Fatalf("an unreached order was settled: x %+v, y %+v", e.events(x), e.events(y))
	}
	e.clk.Advance(time.Second)
	e.mustPass() // serves x
	if e.eventCount(x, store.EventExpired) != 1 {
		t.Fatalf("x on its turn: %+v", e.events(x))
	}
	if e.eventCount(y, store.EventPaidAgain) != 0 {
		t.Fatalf("y settled while not reached: %+v", e.events(y))
	}
	e.clk.Advance(time.Second)
	e.mustPass() // serves y
	if e.eventCount(x, store.EventExpired) != 1 || e.eventCount(y, store.EventPaidAgain) != 1 {
		t.Fatalf("x %+v, y %+v", e.events(x), e.events(y))
	}
	e.safety(x, y, z)
}

// A transaction that pays two orders is read for the one that is served: the other is neither recorded nor settled.
func TestOneTransactionPayingTwoOrders(t *testing.T) {
	e := newWatchEnv(t)
	e.wallStep(11 * time.Second)
	x, y := e.order(10_000), e.order(20_000)
	T := e.node.PayWith(1, []onchaintest.Out{{Address: e.addr(x), Sat: 10_000}, {Address: e.addr(y), Sat: 20_000}})
	e.node.Mine(1)
	e.mustPass() // serves x: its output is listed first
	if _, ok := e.fwd(T, 0); !ok || e.eventCount(x, store.EventPaid) != 1 {
		t.Fatalf("x not recorded: %+v", e.events(x))
	}
	if _, ok := e.lookup(PaidTransferID(T, 1, 0)); ok || e.eventCount(y, store.EventPaid) != 0 {
		t.Fatal("y recorded while not reached")
	}
	y1, err := e.st.TXO(bg, store.TXOKey{TxID: T, Vout: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !e.watched(y1.TxID) {
		t.Fatal("y's output is no longer watched")
	}
	e.clk.Advance(10 * time.Second)
	e.mustPass() // serves y
	if _, ok := e.lookup(PaidTransferID(T, 1, 0)); !ok || e.eventCount(y, store.EventPaid) != 1 {
		t.Fatalf("y not recorded on its turn: %+v", e.events(y))
	}
	if _, ok := e.lookup(PaidTransferID(T, 1, 1)); ok {
		t.Fatal("y recorded twice")
	}
	e.safety(x, y)
}

func TestOrderWiderThanAPassIsNotSettledAndReported(t *testing.T) {
	e := newWatchEnv(t)
	e.cap(8)
	wide, small := e.order(90_000), e.order(10_000)
	var txs []string
	for range 9 {
		txs = append(txs, e.payOrder(wide, 10_000))
	}
	ts := e.payOrder(small, 10_000)
	e.node.Mine(1)
	e.mustPasses(3)
	for _, tx := range txs {
		if e.node.TxCalls(tx) != 0 {
			t.Fatal("a wide order's transaction was read")
		}
	}
	if _, ok := e.fwd(txs[0], 0); ok || !e.stale(wide) || e.count(logTooWide) != 1 {
		t.Fatalf("wide order: stale %v, alerts %d", e.stale(wide), e.count(logTooWide))
	}
	if !e.logHas(`msg="` + logTooWide + `" order=` + wide.ID) {
		t.Fatal("the ALERT names the order")
	}
	if _, ok := e.fwd(ts, 0); !ok || e.stale(small) {
		t.Fatal("small order held back")
	}
	e.safety(small)
}

func TestDeferredOrderReadsStaleAfterThreeIntervals(t *testing.T) {
	e := newWatchEnv(t)
	e.wallStep(11 * time.Second)
	var os []store.Order
	var txs []string
	for range 6 {
		o := e.order(10_000)
		os = append(os, o)
		txs = append(txs, e.payOrder(o, 10_000))
	}
	sixth := os[slices.Index(txs, byTxid(txs...)[5])]
	for i := 1; i <= 6; i++ {
		e.clk.Advance(10 * time.Second)
		e.mustPass()
		switch i {
		case 4:
			if e.stale(sixth) {
				t.Fatal("stale after 30 s")
			}
		case 5:
			if !e.stale(sixth) {
				t.Fatal("not stale after 40 s")
			}
		case 6:
			if e.stale(sixth) {
				t.Fatal("stale after it was served")
			}
		}
	}
}

func TestOnTimePaymentDuringBacklogCountsOnTime(t *testing.T) {
	e := newWatchEnv(t)
	e.cap(50)
	e.wallStep(time.Second)
	big := e.order(400_000)
	for range 40 {
		e.payOrder(big, 10_000)
	}
	e.mustPass()
	o2, _, err := e.st.CreateOrder(bg, store.Order{AccountID: "acct-1", MerchantRef: "ref-backlog", AmountSat: 10_000,
		ExpiresAt: e.clk.Now().Add(60 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	t2 := e.payOrder(o2, 10_000)
	e.clk.Advance(30 * time.Second)
	e.mustPass()
	if x := e.txo(t2); !x.FirstSeen.Equal(e.clk.Now()) {
		t.Fatalf("first seen %v, the pass %v", x.FirstSeen, e.clk.Now())
	}
	if r := e.rail(o2); r.ReceivedSat != 10_000 || r.LateSat != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestRotationRestartsFromTheStart(t *testing.T) {
	e := newWatchEnv(t)
	e.wallStep(11 * time.Second)
	var os []store.Order
	var txs []string
	for range 3 {
		o := e.order(10_000)
		os = append(os, o)
		txs = append(txs, e.payOrder(o, 10_000))
	}
	e.node.Mine(1)
	sorted := byTxid(txs...)
	e.mustPass() // serves the first
	first := sorted[0]
	if _, ok := e.fwd(first, 0); !ok {
		t.Fatal("the first order is not recorded")
	}
	before := e.node.TxCalls(first)
	e.svc = e.newService()
	e.ready()
	e.wallStep(11 * time.Second)
	e.clk.Advance(10 * time.Second)
	e.mustPass()
	if e.node.TxCalls(first) != before+1 || e.node.TxCalls(sorted[1]) != 0 || e.node.TxCalls(sorted[2]) != 0 {
		t.Fatalf("after the restart the pass did not start at the head: %d %d %d", e.node.TxCalls(first),
			e.node.TxCalls(sorted[1]), e.node.TxCalls(sorted[2]))
	}
	for range 2 {
		e.clk.Advance(10 * time.Second)
		e.mustPass()
	}
	for _, tx := range txs {
		if _, ok := e.fwd(tx, 0); !ok {
			t.Fatal("a payment is not recorded")
		}
		if _, ok := e.fwd(tx, 1); ok {
			t.Fatal("a payment is recorded twice")
		}
	}
	e.safety(os...)
}

func TestTooWideAlertAgainAfterARestart(t *testing.T) {
	e := newWatchEnv(t)
	e.cap(8)
	wide := e.order(90_000)
	for range 9 {
		e.payOrder(wide, 10_000)
	}
	e.node.Mine(1)
	e.mustPasses(2)
	if e.count(logTooWide) != 1 {
		t.Fatalf("alerts %d", e.count(logTooWide))
	}
	e.svc = e.newService()
	e.ready()
	e.cap(8)
	e.mustPasses(1)
	if e.count(logTooWide) != 2 {
		t.Fatalf("alerts %d", e.count(logTooWide))
	}
}

func TestDeferredPaymentIsRecordedOnce(t *testing.T) {
	e := newWatchEnv(t)
	e.wallStep(11 * time.Second)
	var os []store.Order
	var txs []string
	for range 3 {
		o := e.order(10_000)
		os = append(os, o)
		txs = append(txs, e.payOrder(o, 10_000))
	}
	e.node.Mine(1)
	m := byTxid(txs...)[2] // deferred in the first two passes
	e.mustPasses(2)
	if _, ok := e.fwd(m, 0); ok {
		t.Fatal("m recorded before its turn")
	}
	e.mustPasses(1)
	if _, ok := e.fwd(m, 0); !ok {
		t.Fatal("m not recorded on its turn")
	}
	e.mustPasses(3) // it is served again
	if _, ok := e.fwd(m, 1); ok {
		t.Fatal("m recorded twice")
	}
	if n := e.node.TxCalls(m); n < 2 {
		t.Fatalf("m was not served again: %d reads", n)
	}
	forwards := 0
	for ep := range uint32(3) {
		if _, ok := e.fwd(m, ep); ok {
			forwards++
		}
	}
	if forwards != 1 {
		t.Fatalf("%d forwards of m, want 1", forwards)
	}
	n := 0
	for i, tx := range txs {
		if tx == m {
			n = e.eventCount(os[i], store.EventPaid)
		}
	}
	if n != 1 {
		t.Fatalf("order.paid %d", n)
	}
	e.safety(os...)
}

func TestHungTransactionDefersOnlyItsOrder(t *testing.T) {
	e := newWatchEnv(t)
	e.mod = func(c *Config) { c.PollRate, c.CallTimeout = MaxPollRate, time.Second }
	e.restart()
	big := e.order(10_000)
	bigTx := e.payOrder(big, 10_000)
	e.mustPasses(1) // big is first seen before small
	small := e.order(10_000)
	ts := e.payOrder(small, 10_000)
	e.node.Mine(1)
	e.node.HangTx(bigTx)
	e.wallStep(time.Millisecond)
	e.clk.Advance(time.Second)
	begin := time.Now()
	e.mustPass()
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("the pass took %v", d)
	}
	if _, ok := e.fwd(ts, 0); !ok {
		t.Fatal("small not recorded")
	}
	if !e.stale(big) || e.count(logOutputFailed) != 1 {
		t.Fatalf("big: stale %v, failures %d", e.stale(big), e.count(logOutputFailed))
	}
	e.safety(small)
}

func TestCrashMidRotationIsSafe(t *testing.T) {
	e := newWatchEnv(t)
	e.wallStep(11 * time.Second)
	var os []store.Order
	var txs []string
	for range 4 {
		o := e.order(10_000)
		os = append(os, o)
		txs = append(txs, e.payOrder(o, 10_000))
	}
	e.node.Mine(1)
	marks := 0
	e.svc.hook = func(p string) error {
		if p == "track:after-mark" {
			marks++
			if marks == 2 {
				return errCrash
			}
		}
		return nil
	}
	e.clk.Advance(time.Second)
	e.mustPass()
	e.clk.Advance(time.Second)
	if err := e.pass(); err == nil {
		t.Fatal("the pass did not crash")
	}
	e.svc = e.newService()
	e.ready()
	e.wallStep(11 * time.Second)
	for range 6 {
		e.clk.Advance(time.Second)
		e.mustPass()
	}
	for _, tx := range txs {
		if _, ok := e.fwd(tx, 0); !ok {
			t.Fatal("a payment is not recorded")
		}
		if _, ok := e.fwd(tx, 1); ok {
			t.Fatal("a payment is recorded twice")
		}
	}
	e.safety(os...)
}

// A past-window order that only discovery touched, and that a pass did not reach, is touched again in the next pass:
// its late output is recorded once the order is served.
func TestDeferredDiscoveredOrderIsCarriedToItsTurn(t *testing.T) {
	e := newWatchEnv(t)
	o, a, late := lateAtWindowEnd(e)
	e.node.Mine(6)
	z := e.order(2_000_000) // watched, and listed before o
	e.payOrder(z, 2_000_000)
	e.node.Pay(a, 1_000) // a new row on o's address: discovery touches o
	e.wallStep(11 * time.Second)
	for range 2 {
		e.clk.Advance(time.Second)
		e.mustPass()
	}
	if _, ok := e.fwd(late, 0); !ok || !e.hasEvent(o, store.EventLate, 1) {
		t.Fatalf("the late output is not recorded: %+v", e.events(o))
	}
	e.safety(o, z)
}

// An order that is only deferred when its window closes is not reported as left without a final event: it is held
// until its turn.
func TestDeferredAtWindowEndIsNotReported(t *testing.T) {
	e := newWatchEnv(t)
	z := e.order(20_000)
	e.payOrder(z, 20_000)
	e.mustPass()
	o := e.order(10_000)
	e.payOrder(o, 10_000) // mempool, never mined
	e.mustPass()
	e.pastExpiry(o)
	e.mustPass()
	e.toWindowEnd(o)
	e.wallStep(11 * time.Second)
	e.mustPass() // serves z; o is deferred in the pass in which its window closes
	if e.count(logNoFinal) != 0 {
		t.Fatal("reported while deferred")
	}
	e.clk.Advance(time.Second)
	e.mustPass() // serves o
	e.clk.Advance(time.Second)
	e.mustPass()
	if e.count(logNoFinal) != 0 || e.eventCount(o, store.EventUnderpaid) != 1 {
		t.Fatalf("o: %+v, reports %d", e.events(o), e.count(logNoFinal))
	}
}

// The peer rail sees a deferred order as held, so it writes no final event for it before it is read.
func TestDeferredOrderIsHeldForThePeer(t *testing.T) {
	e := newWatchEnv(t)
	a, b := e.order(10_000), e.order(10_000)
	e.payOrder(a, 10_000)
	e.payOrder(b, 10_000)
	e.wallStep(11 * time.Second)
	held := func(o store.Order) bool {
		v, ok, err := e.svc.View(bg, o)
		if err != nil || !ok {
			t.Fatal(err)
		}
		return v.Held
	}
	e.mustPass()
	if held(a) == held(b) {
		t.Fatalf("held: a %v, b %v; want exactly the order not reached", held(a), held(b))
	}
	notReached := a
	if held(b) {
		notReached = b
	}
	e.clk.Advance(time.Second)
	e.mustPass() // it is served now
	if held(notReached) {
		t.Fatal("held after it was reached")
	}
}

func TestOrderWithExactlyThePassCapIsServed(t *testing.T) {
	e := newWatchEnv(t)
	e.cap(8)
	o := e.order(80_000)
	var txs []string
	for range 8 {
		txs = append(txs, e.payOrder(o, 10_000))
	}
	e.node.Mine(1)
	e.mustPasses(2)
	for _, tx := range txs {
		if e.node.TxCalls(tx) == 0 {
			t.Fatal("a transaction of an order at the cap was not read")
		}
	}
	if e.count(logTooWide) != 0 || e.stale(o) || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("alerts %d, events %+v", e.count(logTooWide), e.events(o))
	}
	e.safety(o)
}

func TestPassCapOf(t *testing.T) {
	for _, c := range []struct {
		interval time.Duration
		rate     float64
		want     int
	}{
		{10 * time.Second, 5, 50},
		{10 * time.Second, 1.01, 11}, // 10.1 rounds up
		{2 * time.Second, 1, 8},      // 2 is below the floor
		{10 * time.Second, 0.8, 8},   // exactly 8
		{60 * time.Second, 10_000, 600_000},
	} {
		if got := passCapOf(Config{PollInterval: c.interval, PollRate: c.rate}); got != c.want {
			t.Errorf("passCapOf(%v, %v) = %d, want %d", c.interval, c.rate, got, c.want)
		}
	}
	e := newWatchEnv(t)
	if want := passCapOf(e.svc.cfg); e.svc.passCap != want || want != int(math.Ceil(10*MaxPollRate)) {
		t.Fatalf("New set passCap %d, want %d", e.svc.passCap, want)
	}
	d := newEnv(t)
	if d.svc.passCap != 50 {
		t.Fatalf("default passCap %d", d.svc.passCap)
	}
}
