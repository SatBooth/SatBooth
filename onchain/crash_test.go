package onchain

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
)

var crashPoints = []string{"scan:after-record", "track:after-mark", "settle:after-transfers", "settle:after-account",
	"settle:after-event", "settle:after-settled"}

// A scenario prepares an order up to its last pass; the crash, when there is one, is armed for that last pass. fires is
// the set of hook points that last pass reaches, so every cell of the matrix either crashes or is known not to. tiers
// and amount, when set, replace the default tiers and the 100,000-sat order.
type scenario struct {
	name    string
	prepare func(e *env, o store.Order)
	fires   []string
	tiers   []Tier
	amount  int64
}

// env makes the scenario's env and order.
func (sc scenario) env(t *testing.T) (*env, store.Order) {
	t.Helper()
	e := newWatchEnvTiers(t, sc.tiers)
	amount := sc.amount
	if amount == 0 {
		amount = 100_000
	}
	return e, e.order(amount)
}

var (
	firstPaid  = []string{"scan:after-record", "track:after-mark", "settle:after-transfers", "settle:after-account", "settle:after-event"}
	paidAgain  = []string{"scan:after-record", "track:after-mark", "settle:after-transfers", "settle:after-event"}
	reversedOK = []string{"scan:after-record", "track:after-mark", "settle:after-transfers", "settle:after-event"}
	// A 0-tier record whose output dies after the pass's scan is held: that pass only marks it dead,
	// and the next one reverses it.
	heldDead     = []string{"scan:after-record", "track:after-mark"}
	reversedHeld = []string{"scan:after-record", "settle:after-transfers", "settle:after-event"}
)

var scenarios = []scenario{
	{"first payment", func(e *env, o store.Order) {
		e.payOrder(o, o.AmountSat)
		e.pass()
		e.node.Mine(1)
	}, firstPaid, nil, 0},
	{"reversal", func(e *env, o store.Order) {
		e.payOrder(o, o.AmountSat)
		e.node.Mine(1)
		e.pass()
		e.node.Invalidate(1)
	}, reversedOK, nil, 0},
	{"re-confirmation", func(e *env, o store.Order) {
		e.payOrder(o, o.AmountSat)
		e.node.Mine(1)
		e.pass()
		e.node.Invalidate(1)
		e.pass()
		e.node.Mine(1)
	}, paidAgain, nil, 0},
	// A recorded output reaches SettledDepth: the last pass records the settled fact.
	{"settled", func(e *env, o store.Order) {
		e.payOrder(o, o.AmountSat)
		e.node.Mine(1)
		e.pass()
		e.node.Mine(5)
	}, []string{"scan:after-record", "settle:after-settled"}, nil, 0},
	// Two outputs, each enough, reorged out together: one order.reverted, never a paid_again.
	{"two sufficient reversed", func(e *env, o store.Order) {
		a := e.addr(o)
		e.node.Pay(a, o.AmountSat)
		e.node.Pay(a, o.AmountSat)
		e.node.Mine(1)
		e.pass()
		e.node.Invalidate(1)
	}, reversedOK, nil, 0},
	// Two outputs, both needed, reorged out together: one order.reverted.
	{"two needed reversed", func(e *env, o store.Order) {
		a := e.addr(o)
		e.node.Pay(a, o.AmountSat*6/10)
		e.node.Pay(a, o.AmountSat*4/10)
		e.node.Mine(1)
		e.pass()
		e.node.Invalidate(1)
	}, reversedOK, nil, 0},
	// A 0 tier: the last pass discovers, marks and records the payment from the mempool.
	{"zero-conf first payment", func(e *env, o store.Order) {
		e.payOrder(o, o.AmountSat)
	}, firstPaid, zeroTiers, 5_000},
	// A 0 tier: paid from the mempool, then replaced away; the last pass marks it dead and holds its reversal.
	{"zero-conf replaced", func(e *env, o store.Order) {
		txid := e.payOrder(o, o.AmountSat)
		e.pass()
		e.node.Replace(txid, false)
	}, heldDead, zeroTiers, 5_000},
	// The same, one pass on: nothing replaced it, so the last pass reverses it with order.reverted.
	{"zero-conf replaced, the pass after the hold", func(e *env, o store.Order) {
		txid := e.payOrder(o, o.AmountSat)
		e.pass()
		e.node.Replace(txid, false)
		e.pass()
	}, reversedHeld, zeroTiers, 5_000},
	// A 0 tier: paid from the mempool, then a fee bump that keeps the output. The last pass records the new output in
	// step 3 and reverses the old one (dead) in step 4, with no event: the order stays paid.
	{"zero-conf bumped", func(e *env, o store.Order) {
		txid := e.payOrder(o, o.AmountSat)
		e.pass()
		e.node.Replace(txid, true)
	}, []string{"scan:after-record", "track:after-mark", "settle:after-transfers"}, zeroTiers, 5_000},
	// A tier-3 payment never mined, at its window end: the last pass adds order.underpaid.
	{"stuck at the window end", func(e *env, o store.Order) {
		e.payOrder(o, o.AmountSat)
		e.pass()
		e.pastExpiry(o)
		e.pass()
		e.toWindowEnd(o)
	}, []string{"scan:after-record", "settle:after-event"}, nil, 2_000_000},
	// A 0 tier: paid from the mempool, still unconfirmed at its window end, then replaced away. The last pass marks it
	// dead and holds its reversal; a crash there is healed by the restarted pass, which reverses it.
	{"zero-conf replaced after the window", func(e *env, o store.Order) {
		e.node.Mine(1) // a tip above the genesis: a settled fact needs a height of at least 1
		txid := e.payOrder(o, o.AmountSat)
		e.pass()
		e.toWindowEnd(o)
		e.pass()
		e.node.Replace(txid, false)
	}, heldDead, zeroTiers, 5_000},
	// The same, evicted instead of replaced, one pass on: the last pass reverses it with order.reverted and settles it:
	// a crash anywhere before that is healed, because a dead output leaves the stuck set only once settled.
	{"zero-conf evicted after the window", func(e *env, o store.Order) {
		e.node.Mine(1)
		txid := e.payOrder(o, o.AmountSat)
		e.pass()
		e.toWindowEnd(o)
		e.pass()
		e.node.Evict(txid)
		e.pass()
	}, append(slices.Clone(reversedHeld), "settle:after-settled"), zeroTiers, 5_000},
	// Tier 1, never mined: underpaid at its window end, then evicted and settled dead; rebroadcast and mined, the last
	// pass's scan reopens it, and it is recorded at its tier: order.paid after order.underpaid (Ruling S).
	{"dead settled, then rebroadcast and mined", func(e *env, o store.Order) {
		e.node.Mine(1)
		txid := e.payOrder(o, o.AmountSat)
		e.pass()
		e.pastExpiry(o)
		e.pass()
		e.toWindowEnd(o)
		e.pass()
		e.node.Evict(txid)
		e.pass()
		e.node.Rebroadcast(txid)
		e.node.Mine(1)
	}, firstPaid, nil, 0},
	// Tier 1: recorded at 1 block, not SettledDepth deep at its window end, then reorged out and double spent.
	{"tier-1 record double spent after the window", func(e *env, o store.Order) {
		e.node.Mine(1) // the tip stays above the genesis after the reorg
		txid := e.payOrder(o, o.AmountSat)
		e.node.Mine(1)
		e.pass()
		e.toWindowEnd(o)
		e.pass()
		e.node.Invalidate(1)
		e.node.Replace(txid, false)
	}, append(slices.Clone(reversedOK), "settle:after-settled"), nil, 0},
	// Plan 6.1: an on-time payment bumped after the expiry, keeping the address and the amount, and the bump mined. The
	// last pass discovers the bump with the original's first-seen and records it; the original is never recorded.
	{"bump after expiry", func(e *env, o store.Order) {
		txid := e.payOrder(o, o.AmountSat)
		e.pass()
		e.pastExpiry(o)
		e.node.Replace(txid, true)
		e.node.Mine(1)
	}, firstPaid, nil, 0},
	// The same bump in the window's last second (decision 9: it still inherits). The last pass discovers and records
	// it; the original is never recorded.
	{"bump in the window's last second", func(e *env, o store.Order) {
		txid := e.payOrder(o, o.AmountSat)
		e.pass()
		e.pastExpiry(o)
		e.pass()
		e.clk.Advance(o.ExpiresAt.Add(store.WatchWindow).Add(-time.Second).Sub(e.clk.Now()))
		e.node.Replace(txid, true)
		e.node.Mine(1)
	}, firstPaid, nil, 0},
	// The same bump after the window close (order.underpaid): decision 9, it inherits nothing. The last pass discovers
	// it late (not tracked past the window, never recorded) and settles the original, dead past the window with its
	// order final.
	{"bump after the window", func(e *env, o store.Order) {
		txid := e.payOrder(o, o.AmountSat)
		e.pass()
		e.pastExpiry(o)
		e.pass()
		e.toWindowEnd(o)
		e.pass()
		e.node.Replace(txid, true)
		e.node.Mine(1)
	}, []string{"scan:after-record", "track:after-mark", "settle:after-settled"}, nil, 0},
}

// cellChecks are the extra checks of a scenario, on its reference run and on every cell.
var cellChecks = map[string]func(e *env, o store.Order){
	"bump after expiry":                (*env).bumpEnd,
	"bump in the window's last second": (*env).bumpEnd,
	"bump after the window":            (*env).lateBumpEnd,
}

// lateBumpEnd is decision 9's check of an order bumped after its window: two outputs, the replacement with its own
// first-seen (late), neither recorded, the original dead, and order.underpaid the only event.
func (e *env) lateBumpEnd(o store.Order) {
	e.t.Helper()
	l := e.txos(o)
	if len(l) != 2 || l[0].FirstSeen.Equal(l[1].FirstSeen) {
		e.t.Fatalf("outputs %+v", l)
	}
	for _, x := range l {
		if _, f0 := e.lookup(PaidTransferID(x.TxID, x.Vout, 0)); f0 {
			e.t.Errorf("%s: recorded", x.TxID[:8])
		}
	}
	if ev := e.events(o); len(ev) != 1 || ev[0].Type != store.EventUnderpaid || e.rail(o).State != RailLate {
		e.t.Errorf("events %+v, Rail %+v", ev, e.rail(o))
	}
}

// bumpEnd is plan 6.1's check of a bumped order: two outputs with the same first-seen, the original dead and never
// recorded, one forward for the replacement (epoch 0), order.paid once and no order.late.
func (e *env) bumpEnd(o store.Order) {
	e.t.Helper()
	l := e.txos(o)
	if len(l) != 2 || !l[0].FirstSeen.Equal(l[1].FirstSeen) || !l[0].FirstSeen.Before(o.ExpiresAt) {
		e.t.Fatalf("outputs %+v", l)
	}
	forwards := 0
	for _, x := range l {
		_, f0 := e.lookup(PaidTransferID(x.TxID, x.Vout, 0))
		_, f1 := e.lookup(PaidTransferID(x.TxID, x.Vout, 1))
		m, _ := e.mark(x.TxID)
		switch {
		case m.Kind == store.MarkDead && !f0:
		case m.Kind == store.MarkMined && f0 && !f1:
			forwards++
		default:
			e.t.Errorf("%s: mark %+v, forwards %v %v", x.TxID[:8], m, f0, f1)
		}
	}
	if forwards != 1 || e.eventCount(o, store.EventPaid) != 1 || e.eventCount(o, store.EventLate) != 0 {
		e.t.Errorf("%d forwards, events %+v", forwards, e.events(o))
	}
}

// outcome is what a run leaves for its order: each output's first-seen and every transfer by epoch (with its code,
// amount and UserData64), the events by type and seq, and the ledger's balance.
func (e *env) outcome(o store.Order) string {
	e.t.Helper()
	s := ""
	for i, x := range e.txos(o) {
		s += fmt.Sprintf("%d.seen:%d ", i, x.FirstSeen.Unix())
		for ep := uint32(0); ep < 10; ep++ {
			if f, ok := e.lookup(PaidTransferID(x.TxID, x.Vout, ep)); ok {
				s += fmt.Sprintf("%d.f%d:%d:%d ", i, ep, f.Code, amountOf(f))
			}
			if r, ok := e.lookup(ReversalTransferID(x.TxID, x.Vout, ep)); ok {
				s += fmt.Sprintf("%d.r%d:%d:%d:%d ", i, ep, r.Code, amountOf(r), r.UserData64)
			}
		}
	}
	var evs []string
	for _, ev := range e.events(o) {
		evs = append(evs, fmt.Sprintf("%s/%d", ev.Type, ev.Seq))
	}
	slices.Sort(evs)
	b, err := e.lg.Balance(bg, MerchantOnchain)
	if err != nil {
		e.t.Fatal(err)
	}
	var w []string
	for i, x := range e.txos(o) {
		w = append(w, fmt.Sprintf("%d.watched:%v", i, e.watched(x.TxID)))
	}
	return fmt.Sprintf("%s| %v | %d-%d | %v", s, evs, b.CreditsPosted.Lo, b.DebitsPosted.Lo, w)
}

// watchEnd is the check on every cell: past the window, a late output is not watched, and neither is a dead one
// with no standing record whose order has its final event (it is settled), so no cell leaves a dead output polled
// for ever.
func (e *env) watchEnd(o store.Order) {
	e.t.Helper()
	if e.clk.Now().Before(o.ExpiresAt.Add(store.WatchWindow)) {
		return
	}
	final := e.eventCount(o, store.EventPaid)+e.eventCount(o, store.EventExpired)+e.eventCount(o, store.EventUnderpaid) > 0
	for _, x := range e.txos(o) {
		if !e.watched(x.TxID) {
			continue
		}
		if !x.FirstSeen.Before(o.ExpiresAt) {
			e.t.Errorf("%s: a late output is watched past the window", x.TxID[:8])
		}
		_, standing := e.lookup(PaidTransferID(x.TxID, x.Vout, 0))
		for ep := uint32(0); standing; ep++ {
			if _, rev := e.lookup(ReversalTransferID(x.TxID, x.Vout, ep)); !rev {
				break
			}
			_, standing = e.lookup(PaidTransferID(x.TxID, x.Vout, ep+1))
		}
		if m, _ := e.mark(x.TxID); m.Kind == store.MarkDead && !standing && final {
			e.t.Errorf("%s: dead, no standing record, order final, and still watched", x.TxID[:8])
		}
	}
}

// Every hook point, on each scenario: the crashed run, restarted, ends with the same ledger and events as the
// uninterrupted one, nothing recorded twice; and each point fires exactly where the scenario says it does.
func TestCrashAtEveryPoint(t *testing.T) {
	for _, sc := range scenarios {
		ref, ro := sc.env(t)
		sc.prepare(ref, ro)
		ref.pass()
		ref.passes(2)
		want := ref.outcome(ro)
		ref.safety(ro)
		ref.watchEnd(ro)
		check := cellChecks[sc.name]
		if check != nil {
			check(ref, ro)
		}
		for _, point := range crashPoints {
			t.Run(sc.name+"/"+point, func(t *testing.T) {
				e, o := sc.env(t)
				sc.prepare(e, o)
				fired := e.crashAt(point)
				err := e.pass()
				if *fired != errors.Is(err, errCrash) {
					t.Fatalf("fired %v, pass %v", *fired, err)
				}
				if *fired != slices.Contains(sc.fires, point) {
					t.Fatalf("fired %v, the scenario says %v", *fired, slices.Contains(sc.fires, point))
				}
				e.restart()
				e.pass()
				e.passes(2)
				if got := e.outcome(o); got != want {
					t.Fatalf("fired %v:\n got %s\nwant %s", *fired, got, want)
				}
				e.safety(o)
				e.watchEnd(o)
				if check != nil {
					check(e, o)
				}
			})
		}
	}
}

// The two named heals: a crash after a reversal is completed from the reversal's UserData64, and a crash after the
// re-confirmation's transfer gives paid_again on the next pass.
func TestCrashAfterReversalHealsTheEvent(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	scenarios[1].prepare(e, o)
	txid := e.txos(o)[0].TxID
	e.crashAt("settle:after-transfers")
	e.pass()
	if r, ok := e.rev(txid, 0); !ok || r.UserData64 != 1 || e.eventCount(o, store.EventReverted) != 0 {
		t.Fatalf("before the heal: %+v %v %+v", r, ok, e.events(o))
	}
	e.restart()
	e.pass()
	if !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("%+v", e.events(o))
	}
	e.safety(o)
}

func TestCrashAfterReconfirmationGivesPaidAgain(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	scenarios[2].prepare(e, o)
	txid := e.txos(o)[0].TxID
	e.crashAt("settle:after-transfers")
	e.pass()
	if _, ok := e.fwd(txid, 1); !ok || e.hasEvent(o, store.EventPaidAgain, 1) {
		t.Fatalf("before the heal: %+v", e.events(o))
	}
	e.restart()
	e.pass()
	if !e.hasEvent(o, store.EventPaidAgain, 1) || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("%+v", e.events(o))
	}
	e.safety(o)
}
