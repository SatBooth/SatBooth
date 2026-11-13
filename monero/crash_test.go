package monero

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

var crashPoints = []string{"scan:after-record", "track:after-mark", "settle:after-transfers", "settle:after-account",
	"settle:after-event", "settle:after-settled"}

// A scenario prepares an order up to its last pass; the crash, when there is one, is armed for that last pass. fires is
// the set of hook points that last pass reaches, so every cell of the matrix either crashes or is known not to. tiers
// and amount, when set, replace the default tiers and the 1 XMR order.
type scenario struct {
	name    string
	prepare func(e *env, o store.Order)
	fires   []string
	tiers   []rail.Tier
	amount  int64
}

func (sc scenario) env(t *testing.T) (*env, store.Order) {
	t.Helper()
	e := newWatchEnv(t, sc.tiers)
	amount := sc.amount
	if amount == 0 {
		amount = xmr
	}
	return e, e.order(amount)
}

var (
	firstPaid  = []string{"scan:after-record", "track:after-mark", "settle:after-transfers", "settle:after-account", "settle:after-event"}
	paidAgain  = []string{"scan:after-record", "track:after-mark", "settle:after-transfers", "settle:after-event"}
	reversedOK = []string{"scan:after-record", "track:after-mark", "settle:after-transfers", "settle:after-event"}
)

var scenarios = []scenario{
	{"first payment", func(e *env, o store.Order) {
		e.payOrder(o, o.AmountPiconero)
		e.pass()
		e.w.Mine(1)
	}, firstPaid, nil, 0},
	{"reversal", func(e *env, o store.Order) {
		e.payOrder(o, o.AmountPiconero)
		e.w.Mine(1)
		e.pass()
		e.w.Pop(1)
	}, reversedOK, nil, 0},
	{"re-confirmation", func(e *env, o store.Order) {
		e.payOrder(o, o.AmountPiconero)
		e.w.Mine(1)
		e.pass()
		e.w.Pop(1)
		e.pass()
		e.w.Mine(1)
	}, paidAgain, nil, 0},
	// A recorded payment reaches SettledDepth: the last pass records the settled fact.
	{"settled", func(e *env, o store.Order) {
		e.payOrder(o, o.AmountPiconero)
		e.w.Mine(1)
		e.pass()
		e.w.Mine(9)
	}, []string{"scan:after-record", "track:after-mark", "settle:after-settled"}, nil, 0},
	// Two payments, each enough, reorged out together: one order.reverted, never a paid_again.
	{"two sufficient reversed", func(e *env, o store.Order) {
		a := e.addr(o)
		e.w.Pay(a, o.AmountPiconero)
		e.w.Pay(a, o.AmountPiconero)
		e.w.Mine(1)
		e.pass()
		e.w.Pop(1)
	}, reversedOK, nil, 0},
	// Two payments, both needed, reorged out together: one order.reverted.
	{"two needed reversed", func(e *env, o store.Order) {
		a := e.addr(o)
		e.w.Pay(a, o.AmountPiconero*6/10)
		e.w.Pay(a, o.AmountPiconero*4/10)
		e.w.Mine(1)
		e.pass()
		e.w.Pop(1)
	}, reversedOK, nil, 0},
	// A 0 tier: the last pass discovers, marks and records the payment from the pool.
	{"zero-conf first payment", func(e *env, o store.Order) {
		e.payOrder(o, o.AmountPiconero)
	}, firstPaid, zeroTiers, xmr / 200},
	// A 0 tier: paid from the pool, then its inputs seen double spent: the last pass marks it dead and reverses it.
	{"zero-conf double spent", func(e *env, o store.Order) {
		txid := e.payOrder(o, o.AmountPiconero)
		e.pass()
		e.w.DoubleSpend(txid)
	}, reversedOK, zeroTiers, xmr / 200},
	// A tier-3 payment never mined, at its window end: the last pass adds order.underpaid.
	{"stuck at the window end", func(e *env, o store.Order) {
		e.payOrder(o, o.AmountPiconero)
		e.pass()
		e.pastExpiry(o)
		e.pass()
		e.toWindowEnd(o)
	}, []string{"scan:after-record", "settle:after-event"}, nil, 2 * xmr},
	// Tier 1, never mined: underpaid at its window end, then dropped and settled dead; rebroadcast and mined, the last
	// pass's scan reopens it, and it is recorded: order.paid after order.underpaid.
	{"dead settled, then rebroadcast and mined", func(e *env, o store.Order) {
		txid := e.payOrder(o, o.AmountPiconero)
		e.pass()
		e.pastExpiry(o)
		e.pass()
		e.toWindowEnd(o)
		e.pass()
		e.w.Drop(txid)
		e.pass()
		e.w.Rebroadcast(txid)
		e.w.Mine(1)
	}, firstPaid, nil, 0},
	// Tier 1: recorded at 1 block, not SettledDepth deep at its window end, then reorged out by a block without it
	// (popped and dropped, and a competing block mined): reversed, and settled dead (its order has order.paid).
	{"tier-1 record dropped after the window", func(e *env, o store.Order) {
		e.payOrder(o, o.AmountPiconero)
		e.w.Mine(1)
		e.pass()
		e.toWindowEnd(o)
		e.pass()
		e.w.PopDrop(1)
		e.w.Mine(1)
	}, append(slices.Clone(reversedOK), "settle:after-settled"), nil, 0},
	// A payment locked beyond the normal lock: recorded late (code 4) when mined, with order.late.
	{"locked", func(e *env, o store.Order) {
		e.w.PayLocked(e.addr(o), o.AmountPiconero, e.w.Height()+1000)
		e.pass()
		e.w.Mine(1)
	}, []string{"scan:after-record", "track:after-mark", "settle:after-transfers", "settle:after-event"}, nil, 0},
}

// outcome is what a run leaves for its order: each payment's first-seen and every transfer by epoch (with its code,
// amount and UserData64), the events by type and seq, the ledger's balance, and which payments are watched.
func (e *env) outcome(o store.Order) string {
	e.t.Helper()
	s := ""
	for i, p := range e.payments(o) {
		s += fmt.Sprintf("%d.seen:%d ", i, p.FirstSeen.Unix())
		for ep := uint32(0); ep < 10; ep++ {
			if f, ok := e.fwd(o, p.TxID, ep); ok {
				s += fmt.Sprintf("%d.f%d:%d:%d ", i, ep, f.Code, amountOf(f))
			}
			if r, ok := e.rev(o, p.TxID, ep); ok {
				s += fmt.Sprintf("%d.r%d:%d:%d:%d ", i, ep, r.Code, amountOf(r), r.UserData64)
			}
		}
	}
	var evs []string
	for _, ev := range e.events(o) {
		evs = append(evs, fmt.Sprintf("%s/%d", ev.Type, ev.Seq))
	}
	slices.Sort(evs)
	b, err := e.lg.Balance(bg, MerchantMonero)
	if err != nil {
		e.t.Fatal(err)
	}
	var w []string
	for i, p := range e.payments(o) {
		w = append(w, fmt.Sprintf("%d.watched:%v", i, e.watched(o, p.TxID)))
	}
	return fmt.Sprintf("%s| %v | %d-%d | %v", s, evs, b.CreditsPosted.Lo, b.DebitsPosted.Lo, w)
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
			})
		}
	}
}

// A crash after a reversal is completed from the reversal's UserData64, and one after the re-confirmation's transfer
// gives paid_again on the next pass.
func TestCrashAfterReversalHealsTheEvent(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	scenarios[1].prepare(e, o)
	txid := e.payments(o)[0].TxID
	e.crashAt("settle:after-transfers")
	e.pass()
	if r, ok := e.rev(o, txid, 0); !ok || r.UserData64 != 1 || e.eventCount(o, store.EventReverted) != 0 {
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
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	scenarios[2].prepare(e, o)
	txid := e.payments(o)[0].TxID
	e.crashAt("settle:after-transfers")
	e.pass()
	if _, ok := e.fwd(o, txid, 1); !ok || e.hasEvent(o, store.EventPaidAgain, 1) {
		t.Fatalf("before the heal: %+v", e.events(o))
	}
	e.restart()
	e.pass()
	if !e.hasEvent(o, store.EventPaidAgain, 1) || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("%+v", e.events(o))
	}
	e.safety(o)
}
