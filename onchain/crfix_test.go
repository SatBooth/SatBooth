package onchain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

// Per order (plan 6.1): a settle error makes only its own order read stale. Here lnpay's Status fails, so
// the mixed order's settle fails in every pass while the on-chain-only order settles: the pass is still stamped good,
// the mixed order reads stale and the other does not, also past 3 PollIntervals. A pass whose settle succeeds again
// clears it.
func TestStaleIsPerOrder(t *testing.T) {
	e := newWatchEnv(t)
	m := e.mixed(1_000, 10_000)
	c := e.order(20_000)
	e.payOrder(m, 10_000)
	e.payOrder(c, 20_000)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	good := e.svc.lastGoodPass.Load()
	if good == 0 || e.rail(m).Stale || e.rail(c).Stale {
		t.Fatal("setup: no good pass, or stale")
	}
	e.ln.setFail(errors.New("lnpay failed"))
	for i := 1; i <= 4; i++ {
		e.clk.Advance(e.svc.cfg.PollInterval)
		if err := e.pass(); err != nil {
			t.Fatalf("pass %d ended early: %v", i, err)
		}
		if got := e.svc.lastGoodPass.Load(); got <= good {
			t.Fatalf("pass %d: a pass with a settle error was not stamped good", i)
		}
		good = e.svc.lastGoodPass.Load()
		if !e.rail(m).Stale || e.rail(c).Stale {
			t.Fatalf("pass %d: mixed stale %v, on-chain-only stale %v", i, e.rail(m).Stale, e.rail(c).Stale)
		}
		if e.count(logSettleFailed) != i || !e.logHas(`msg="`+logSettleFailed+`" order=`+m.ID) {
			t.Fatalf("pass %d: %d settle-failed lines:\n%s", i, e.count(logSettleFailed), e.log)
		}
	}
	e.ln.setFail(nil)
	e.clk.Advance(e.svc.cfg.PollInterval)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if e.rail(m).Stale || e.rail(c).Stale {
		t.Fatal("still stale after a clean settle")
	}
	e.safety(m, c)
}

// The gap ALERT is logged when the run first reaches gap_alert, then at most once every 24 h while it stays there;
// a run that drops below and reaches it again logs at once.
func TestGapAlertIsThrottled(t *testing.T) {
	e := newEnv(t)
	e.mod = func(c *Config) { c.PollRate, c.GapAlert = MaxPollRate, 3 }
	e.svc = e.newService()
	idx := int64(0)
	add := func() {
		t.Helper()
		o := e.order(1_000)
		if err := e.st.AddAddress(bg, store.Address{OrderID: o.ID, Address: e.node.AddressAt(idx), Index: idx}); err != nil {
			t.Fatal(err)
		}
		idx++
		e.node.Update(func(s *onchaintest.State) { s.NextIndex = idx })
	}
	add()
	add()
	e.ready()
	step := func(d time.Duration, want int) {
		t.Helper()
		e.clk.Advance(d)
		if err := e.pass(); err != nil {
			t.Fatal(err)
		}
		if got := e.count(logGap); got != want {
			t.Fatalf("%d gap lines, want %d:\n%s", got, want, e.log)
		}
	}
	step(time.Second, 0) // a run of 2
	add()                // 3: crosses
	step(time.Second, 1)
	step(time.Second, 1)
	step(gapAlertEvery-2*time.Second, 1)
	step(time.Second, 2) // 24 h after the first line
	step(time.Second, 2)
	e.node.Pay(e.node.AddressAt(idx-1), 1_000) // the run drops to 0
	step(time.Second, 2)
	add()
	add()
	step(time.Second, 2) // 2
	add()                // 3 again: logged at once
	step(time.Second, 3)
	step(time.Second, 3)

	e.svc = e.newService() // a new process logs it again on its first pass
	e.ready()
	step(time.Second, 4)
}

// An order whose output keeps failing is skipped in every pass, so it never gets a final event; when it leaves
// its watch window, an ALERT names it, whatever the cause and whatever the output's state (here a counted output in
// the mempool and a late one), once, also when the process restarted after the window closed. An order that got its
// final event gives no such line. The counted output stays in the stuck set, skipped while it fails: mended, its
// order gets order.underpaid.
func TestOrderLeavingTheWindowWithoutAFinalEventIsAlerted(t *testing.T) {
	const noFinal = "ALERT: an on-chain order left its watch window without a final event; check it by hand"
	failing := &btcrpc.Error{Code: -5, Method: "gettransaction"}
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "same process", true: "after a restart"}[restart], func(t *testing.T) {
			e := newWatchEnv(t)
			counted, late, fine := e.order(10_000), e.order(20_000), e.order(30_000)
			tc := e.payOrder(counted, 10_000)
			e.addr(late)
			e.addr(fine)
			e.pass()
			e.clk.Advance(time.Hour) // all three expire
			tl := e.node.Pay(e.rail(late).Address, 20_000)
			e.node.FailTx(tc, failing)
			e.node.FailTx(tl, failing)
			e.pass() // late is first seen after the expiry, and skipped; fine gets order.expired
			e.passes(3)
			if !e.hasEvent(fine, store.EventExpired, 1) || len(e.events(counted)) != 0 || len(e.events(late)) != 0 {
				t.Fatalf("setup: fine %+v counted %+v late %+v", e.events(fine), e.events(counted), e.events(late))
			}
			if e.count(noFinal) != 0 {
				t.Fatal("alerted inside the window")
			}
			e.clk.Advance(store.WatchWindow)
			if restart {
				e.restart()
			}
			e.pass()
			for _, o := range []store.Order{counted, late} {
				if !e.logHas(`msg="` + noFinal + `" order=` + o.ID) {
					t.Errorf("no ALERT for %s:\n%s", o.ID, e.log)
				}
			}
			if e.logHas(`msg="`+noFinal+`" order=`+fine.ID) || e.count(noFinal) != 2 {
				t.Fatalf("%d lines:\n%s", e.count(noFinal), e.log)
			}
			e.passes(3)
			if e.count(noFinal) != 2 {
				t.Fatalf("repeated: %d lines", e.count(noFinal))
			}
			if len(e.events(counted)) != 0 || len(e.events(late)) != 0 {
				t.Fatal("a final event was invented")
			}
			// The counted output is on time, so it stays in the stuck set past the window: once its calls succeed again,
			// its order settles and gets order.underpaid (its payment is not confirmed). The late one is not polled.
			e.node.MendTx(tc)
			e.node.MendTx(tl)
			e.pass()
			if ev := e.events(counted); len(ev) != 1 || ev[0].Type != store.EventUnderpaid || e.count(alertAtWindow) != 1 {
				t.Fatalf("after MendTx: %+v\n%s", ev, e.log)
			}
			if len(e.events(late)) != 0 || e.count(noFinal) != 2 {
				t.Fatalf("late %+v, %d no-final lines", e.events(late), e.count(noFinal))
			}
		})
	}
}

// After a bitcoind restart, getmempoolentry answers -5 for every transaction until the mempool is
// loaded. While getmempoolinfo says loaded false, no output is marked dead: an open order keeps its payment (the
// page does not offer the amount again) and a past-expiry one gets no final event. Its unconfirmed outputs wait, with
// one line per pass; the pass is still good. Once loaded, an output that really left the mempool is marked dead.
func TestMempoolLoadingMarksNothingDead(t *testing.T) {
	const loading = "bitcoind is still loading its mempool; unconfirmed payments wait for the next pass"
	e := newWatchEnv(t)
	open, past, gone := e.order(10_000), e.order(20_000), e.order(30_000)
	to := e.payOrder(open, 10_000)
	tp := e.payOrder(past, 20_000)
	tg := e.payOrder(gone, 30_000)
	e.pass()
	e.clk.Advance(time.Hour - 2*time.Minute)
	open2 := e.order(15_000) // still open after the others expire
	t2 := e.payOrder(open2, 15_000)
	e.pass()
	for _, id := range []string{to, tp, tg, t2} {
		if m, ok := e.mark(id); !ok || m.Kind != store.MarkMempool {
			t.Fatalf("setup: %+v", m)
		}
	}
	e.clk.Advance(5 * time.Minute) // open, past and gone expire; open2 does not
	e.node.Update(func(s *onchaintest.State) { s.MempoolLoading = true })
	e.node.Evict(tg) // gone really leaves the mempool, but bitcoind cannot tell yet
	for i := 1; i <= 3; i++ {
		e.clk.Advance(time.Second)
		if err := e.pass(); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		for _, id := range []string{to, tp, tg, t2} {
			if m, _ := e.mark(id); m.Kind != store.MarkMempool {
				t.Fatalf("pass %d: %s marked %s while the mempool loads", i, id[:8], m.Kind)
			}
		}
		if n := len(e.events(open)) + len(e.events(past)) + len(e.events(gone)) + len(e.events(open2)); n != 0 {
			t.Fatalf("pass %d: %d events while the mempool loads", i, n)
		}
		if r := e.rail(open2); r.ReceivedSat != 15_000 || r.State != RailSeen || r.Stale {
			t.Fatalf("pass %d: open order %+v", i, r)
		}
		if e.count(loading) != i || e.count(logCallFailed) != 0 {
			t.Fatalf("pass %d: %d loading lines, %d call-failed:\n%s", i, e.count(loading), e.count(logCallFailed), e.log)
		}
	}
	e.node.Update(func(s *onchaintest.State) { s.MempoolLoading = false })
	e.clk.Advance(time.Second)
	e.pass()
	// The pass that marks the output dead holds its order's final event for one pass (diedInPass).
	if m, _ := e.mark(tg); m.Kind != store.MarkDead || len(e.events(gone)) != 0 {
		t.Fatalf("gone in the pass of its dead mark: %+v, events %+v", m, e.events(gone))
	}
	e.clk.Advance(time.Second)
	e.pass()
	if m, _ := e.mark(tg); m.Kind != store.MarkDead || !e.hasEvent(gone, store.EventExpired, 1) {
		t.Fatalf("gone after the load: %+v, events %+v", m, e.events(gone))
	}
	if m, _ := e.mark(tp); m.Kind != store.MarkMempool || len(e.events(past)) != 0 {
		t.Fatalf("past after the load: %+v, events %+v", m, e.events(past))
	}
	e.node.Mine(1)
	e.clk.Advance(time.Second)
	e.pass()
	for _, o := range []store.Order{open, past, open2} {
		if !e.hasEvent(o, store.EventPaid, 1) {
			t.Fatalf("not paid once mined: %+v", e.events(o))
		}
	}
	if e.count(loading) != 3 {
		t.Fatal("a loading line after the load")
	}
	e.safety(open, past, gone, open2)
}

// Inside a pass (plan 6.1): bitcoind restarts between the pass's getmempoolinfo and an output's
// getmempoolentry. The pass read loaded true, but the second output is not found because the new process is still
// loading its mempool: getmempoolinfo is read again before a dead mark, says loading, and the output gets no mark (its
// order is skipped, one loading line, and it does not read stale: nothing failed). On a 0 tier the first order is paid
// from the mempool in that pass; the second is paid in the next one, once the mempool is loaded.
func TestRestartInsideAPassMarksNothingDead(t *testing.T) {
	const loading = "bitcoind is still loading its mempool; unconfirmed payments wait for the next pass"
	e := newZeroConfEnv(t)
	o1, o2 := e.order(5_000), e.order(6_000)
	t1, t2 := e.payOrder(o1, 5_000), e.payOrder(o2, 6_000)
	arrivals := 0
	e.svc.hook = func(p string) error {
		if p == "track:before-mempool" {
			if arrivals++; arrivals == 2 {
				e.node.Update(func(s *onchaintest.State) { s.MempoolLoading = true })
			}
		}
		return nil
	}
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if arrivals != 2 {
		t.Fatalf("track:before-mempool reached %d times, want 2", arrivals)
	}
	byTx := map[string]store.Order{t1: o1, t2: o2}
	var first, second string
	for _, id := range []string{t1, t2} {
		switch m, ok := e.mark(id); {
		case ok && m.Kind == store.MarkMempool:
			first = id
		case !ok:
			second = id
		default:
			t.Fatalf("%s marked %s", id[:8], m.Kind)
		}
	}
	if first == "" || second == "" {
		t.Fatalf("want one mempool mark and one unmarked output: first %q second %q", first, second)
	}
	if r := e.rail(byTx[first]); !r.Paid || e.eventCount(byTx[first], store.EventPaid) != 1 {
		t.Fatalf("the first order: %+v %+v", r, e.events(byTx[first]))
	}
	if r := e.rail(byTx[second]); r.Paid || r.Stale || len(e.events(byTx[second])) != 0 {
		t.Fatalf("the skipped order: %+v %+v", r, e.events(byTx[second]))
	}
	if _, ok := e.fwd(second, 0); ok {
		t.Fatal("the skipped output was recorded")
	}
	if e.count(loading) != 1 || e.count(logCallFailed) != 0 {
		t.Fatalf("%d loading lines, %d call-failed:\n%s", e.count(loading), e.count(logCallFailed), e.log)
	}
	e.svc.hook = nil
	e.node.Update(func(s *onchaintest.State) { s.MempoolLoading = false })
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if m, ok := e.mark(second); !ok || m.Kind != store.MarkMempool {
		t.Fatalf("after the load: %+v %v", m, ok)
	}
	for _, o := range []store.Order{o1, o2} {
		if r := e.rail(o); !r.Paid || r.Stale || e.eventCount(o, store.EventPaid) != 1 {
			t.Fatalf("after the load: %+v %+v", r, e.events(o))
		}
	}
	if e.count(loading) != 1 {
		t.Fatal("a loading line after the load")
	}
	e.safety(o1, o2)
}

// Check calls getmempoolinfo too, so a bitcoind whose rpcwhitelist lacks it (an upgrade without the new method:
// rpcwhitelist answers HTTP 403, btcrpc.ErrHTTP) fails the startup check instead of every pass. A mempool still
// loading is no failure.
func TestCheckNeedsGetmempoolinfo(t *testing.T) {
	e := newEnv(t)
	e.node.Break("getmempoolinfo", fmt.Errorf("%w: %w: 403", btcrpc.ErrHTTP, btcrpc.ErrTransport))
	if err := e.svc.Check(bg); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Check = %v, want ErrUnavailable", err)
	}
	e.node.Mend("getmempoolinfo")
	e.node.Update(func(s *onchaintest.State) { s.MempoolLoading = true })
	if err := e.svc.Check(bg); err != nil {
		t.Fatalf("Check while the mempool loads: %v", err)
	}
}

// --- plan 6.1: more cases ---

// noFinalFor is the number of logNoFinal lines that name the order.
func (e *env) noFinalFor(o store.Order) int {
	return strings.Count(e.log.String(), `msg="`+logNoFinal+`" order=`+o.ID)
}

// leftWithoutFinal checks each carried order on its own. Two orders are held at their window close in one pass
// (each on-time output dies after the scan); in the next pass the first one's events read fails while the second
// still has no final event (its output's call fails), and a third order's window closes without a final event. The
// pass reports the second and the third, keeps the first carried, and is not stamped good. The next pass, with the
// read mended, reports neither the second again nor the first (settled by then).
func TestLeftWithoutFinalChecksEachOrderOnItsOwn(t *testing.T) {
	e := newWatchEnv(t)
	o2 := e.order(100_000) // expires first: carried before o1
	a2 := e.payOrder(o2, 100_000)
	e.clk.Advance(time.Second)
	o1 := e.order(100_000)
	a1 := e.payOrder(o1, 100_000)
	e.clk.Advance(time.Second)
	o3 := e.mixed(50_000_000, 100_000) // its Lightning side never ends: never due, so never final
	e.pass()
	e.pastExpiry(o1)
	e.pass()
	e.toWindowEnd(o1)
	var b1, b2 string
	e.svc.hook = func(p string) error {
		if p == "scan:after-record" && b1 == "" {
			b1, b2 = e.node.Replace(a1, false), e.node.Replace(a2, false)
		}
		return nil
	}
	e.pass()
	e.svc.hook = nil
	if b1 == "" || b2 == "" || len(e.events(o1)) != 0 || len(e.events(o2)) != 0 || e.count(logNoFinal) != 0 {
		t.Fatalf("the held pass: %q %+v %+v\n%s", b1, e.events(o1), e.events(o2), e.log)
	}
	if got := len(e.svc.leftHeld); got != 2 {
		t.Fatalf("%d orders carried, want 2", got)
	}

	errRead := errors.New("events read failed")
	e.svc.eventsFor = func(ctx context.Context, id string) ([]store.Event, error) {
		if id == o1.ID {
			return nil, errRead
		}
		return e.st.EventsForOrder(ctx, id)
	}
	e.node.FailTx(a2, &btcrpc.Error{Code: -5, Method: "gettransaction"})
	good := e.svc.lastGoodPass.Load()
	e.clk.Advance(time.Second) // o3's window end
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if e.noFinalFor(o1) != 0 || e.noFinalFor(o2) != 1 || e.noFinalFor(o3) != 1 {
		t.Fatalf("the failed read's pass: no-final o1 %d, o2 %d, o3 %d\n%s", e.noFinalFor(o1), e.noFinalFor(o2), e.noFinalFor(o3), e.log)
	}
	if e.svc.lastGoodPass.Load() != good {
		t.Fatal("a pass with a failed events read was stamped good")
	}
	if l := e.svc.leftHeld; len(l) != 1 || l[0].ID != o1.ID {
		t.Fatalf("carried %+v, want o1 alone", l)
	}

	e.svc.eventsFor = e.st.EventsForOrder
	e.passes(2) // o2's call still fails: it stays without a final event
	if !e.hasEvent(o1, store.EventExpired, 1) || len(e.events(o2)) != 0 {
		t.Fatalf("after the mend: %+v %+v", e.events(o1), e.events(o2))
	}
	if e.noFinalFor(o1) != 0 || e.noFinalFor(o2) != 1 || e.noFinalFor(o3) != 1 || len(e.svc.leftHeld) != 0 {
		t.Fatalf("after the mend: no-final o1 %d, o2 %d, o3 %d, carried %d\n%s", e.noFinalFor(o1), e.noFinalFor(o2), e.noFinalFor(o3),
			len(e.svc.leftHeld), e.log)
	}
}

// Once an unpaid final event exists, money live again reads underpaid, never seen (the event would contradict it),
// until order.paid or order.paid_again. Two ways to it before the window end, with no crash: an on-time payment
// evicted after the scan past the expiry (held one pass, then order.expired) and rebroadcast; and a paid order whose
// block is reorged away past the expiry (order.reverted), its payment back in the mempool. Mined, each reads paid.
func TestLiveMoneyAfterAnUnpaidEventReadsUnderpaid(t *testing.T) {
	t.Run("evicted then rebroadcast", func(t *testing.T) {
		e := newWatchEnv(t)
		o := e.order(100_000)
		a := e.payOrder(o, 100_000)
		e.pass()
		e.pastExpiry(o)
		e.svc.hook = func(p string) error {
			if p == "scan:after-record" {
				e.node.Evict(a)
			}
			return nil
		}
		e.pass()
		e.svc.hook = nil
		e.pass()
		if !e.hasEvent(o, store.EventExpired, 1) || len(e.events(o)) != 1 || e.rail(o).State != RailExpired {
			t.Fatalf("setup: %+v %+v", e.rail(o), e.events(o))
		}
		e.node.Rebroadcast(a)
		e.passes(1)
		if m, _ := e.mark(a); m.Kind != store.MarkMempool {
			t.Fatalf("the rebroadcast output: %+v", m)
		}
		if r, s := e.rail(o), e.status(o); r.ReceivedSat != 100_000 || r.State != RailUnderpaid || s != "underpaid" {
			t.Fatalf("live after order.expired: Rail %+v, status %q", r, s)
		}
		e.node.Mine(1)
		e.passes(1)
		if r := e.rail(o); !r.Paid || r.State != RailPaid || !e.hasEvent(o, store.EventPaid, 1) || e.count(logPaidAfterFinal) != 1 {
			t.Fatalf("mined: %+v %+v\n%s", r, e.events(o), e.log)
		}
		e.safety(o)
	})
	t.Run("reorged back into the mempool", func(t *testing.T) {
		e := newWatchEnv(t)
		o := e.order(100_000) // tier 1
		a := e.payOrder(o, 100_000)
		e.node.Mine(1)
		e.pass()
		if !e.rail(o).Paid || !e.hasEvent(o, store.EventPaid, 1) {
			t.Fatalf("setup: %+v %+v", e.rail(o), e.events(o))
		}
		e.pastExpiry(o)
		e.node.Invalidate(1)
		e.pass()
		if m, _ := e.mark(a); m.Kind != store.MarkMempool || !e.hasEvent(o, store.EventReverted, 1) {
			t.Fatalf("the reorg: %+v %+v", m, e.events(o))
		}
		if r, s := e.rail(o), e.status(o); r.ReceivedSat != 100_000 || r.Paid || r.State != RailUnderpaid || s != "underpaid" {
			t.Fatalf("live after order.reverted: Rail %+v, status %q", r, s)
		}
		e.node.Mine(1)
		e.passes(1)
		if r := e.rail(o); !r.Paid || r.State != RailPaid || !e.hasEvent(o, store.EventPaidAgain, 1) {
			t.Fatalf("mined again: %+v %+v", r, e.events(o))
		}
		e.safety(o)
	})
}
