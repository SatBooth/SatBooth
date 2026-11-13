package onchain

import (
	"errors"
	"fmt"
	"testing"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

// --- each of the two waits of a payment (the wallet-behind grace, the one-pass hold after a dead mark) is granted once ---

// A is replaced with the wallet never showing a conflict, marked dead at 60 s and held
// one pass; during the hold the replacement leaves and A is back in the mempool; A is then replaced again. The second
// time A is marked dead and reversed in the first pass that finds it gone: no second 60 s, no second hold. When A
// returns after the reversal the order is paid again.
func TestOnceTheGraceAndTheHoldAreNotGrantedAgainWhenTheOriginalComesBack(t *testing.T) {
	e := newZeroConfEnv(t)
	o, a := e.zeroConfPaid()
	e.node.HoldWallet()
	b := e.node.Replace(a, false)
	for i := 1; i <= 6; i++ { // behind from the first find (pass 1) to 50 s later
		e.gracePass()
		if m, _ := e.mark(a); m.Kind != store.MarkMempool || !e.rail(o).Paid {
			t.Fatalf("pass %d: mark %+v", i, m)
		}
	}
	e.gracePass() // pass 7, 60 s: dead, and the reversal is held this pass
	if m, _ := e.mark(a); m.Kind != store.MarkDead || !e.rail(o).Paid {
		t.Fatalf("pass 7: mark %+v, Rail %+v", m, e.rail(o))
	}
	if _, ok := e.rev(a, 0); ok || len(e.events(o)) != 1 {
		t.Fatalf("pass 7: reversed %v, events %+v", ok, e.events(o))
	}
	e.node.Evict(b) // during the hold the replacement leaves and A is back
	e.node.Rebroadcast(a)
	e.gracePass() // pass 8
	if m, _ := e.mark(a); m.Kind != store.MarkMempool || !e.rail(o).Paid || len(e.events(o)) != 1 {
		t.Fatalf("pass 8: mark %+v, events %+v", m, e.events(o))
	}
	e.node.Replace(a, false) // replaced again, the wallet still shows no conflict
	e.gracePass()            // pass 9: dead and reversed at once
	if m, _ := e.mark(a); m.Kind != store.MarkDead {
		t.Fatalf("pass 9: mark %+v (a second grace was granted)", m)
	}
	if r, ok := e.rev(a, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || e.rail(o).Paid {
		t.Fatalf("pass 9: reversal %+v %v, events %+v, Rail %+v (a second hold was granted)", r, ok, e.events(o), e.rail(o))
	}
	e.safety(o)
}

// The same, after the reversal: A comes back and the order is paid again, once.
func TestOnceAfterTheReversalTheOriginalBackIsPaidAgain(t *testing.T) {
	e := newZeroConfEnv(t)
	o, a := e.zeroConfPaid()
	e.node.HoldWallet()
	b := e.node.Replace(a, false)
	for i := 0; i < 7; i++ {
		e.gracePass()
	}
	e.node.Evict(b)
	e.node.Rebroadcast(a)
	e.gracePass()
	c := e.node.Replace(a, false)
	e.gracePass()
	if _, ok := e.rev(a, 0); !ok {
		t.Fatalf("not reversed: %+v", e.events(o))
	}
	e.node.Evict(c)
	e.node.Rebroadcast(a)
	e.gracePass()
	if !e.rail(o).Paid || !e.hasEvent(o, store.EventPaidAgain, 1) || e.eventCount(o, store.EventReverted) != 1 {
		t.Fatalf("paid again: %+v %+v", e.rail(o), e.events(o))
	}
	e.safety(o)
}

// The plain cycle, the wallet showing every conflict: dead and held one pass, back in the mempool during the hold (the
// first round of TestZeroConfEvictedAndBackWithinTheHoldStaysPaid), then gone again: reversed in that pass, no second hold.
func TestOnceThePlainCycleHoldsOnlyTheFirstDeadMark(t *testing.T) {
	e := newZeroConfEnv(t)
	o, a := e.zeroConfPaid()
	e.node.Evict(a)
	e.pass()
	if m, _ := e.mark(a); m.Kind != store.MarkDead || !e.rail(o).Paid {
		t.Fatalf("the held pass: %+v %+v", m, e.rail(o))
	}
	e.node.Rebroadcast(a)
	e.pass()
	if m, _ := e.mark(a); m.Kind != store.MarkMempool || !e.rail(o).Paid || len(e.events(o)) != 1 {
		t.Fatalf("back: %+v %+v", m, e.events(o))
	}
	e.node.Evict(a)
	e.pass()
	if m, _ := e.mark(a); m.Kind != store.MarkDead {
		t.Fatalf("gone again: %+v", m)
	}
	if r, ok := e.rev(a, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || e.rail(o).Paid {
		t.Fatalf("the second dead mark was held: reversal %+v %v, events %+v", r, ok, e.events(o))
	}
	e.safety(o)
}

// The memories live as long as the process: an output that is no longer watched keeps its entries (nothing prunes them),
// so it cannot obtain either wait again.
func TestOnceTheMemoriesAreNeverPruned(t *testing.T) {
	e := newZeroConfEnv(t)
	_, a := e.zeroConfPaid()
	fake := store.TXOKey{TxID: "ff", Vout: 7}
	e.svc.heldBefore[fake] = struct{}{}
	e.svc.behindSince[fake] = e.clk.Now()
	e.pass()
	if _, ok := e.svc.heldBefore[fake]; !ok {
		t.Fatalf("heldBefore pruned: %v", e.svc.heldBefore)
	}
	if _, ok := e.svc.behindSince[fake]; !ok {
		t.Fatalf("behindSince pruned: %v", e.svc.behindSince)
	}
	e.node.Evict(a)
	e.pass()
	if _, ok := e.svc.heldBefore[e.key(a)]; !ok {
		t.Fatalf("the first dead mark is not remembered: %v", e.svc.heldBefore)
	}
}

// An output settled as dead past its window, reopened by a real confirmation and put back
// into the mempool by a shallow reorg, is found replaced again: it gets no second 60 s and no second hold, although it
// left the tracked set in between.
func TestOnceAReopenedOutputGetsNeitherWaitAgain(t *testing.T) {
	e := newZeroConfEnv(t)
	e.node.Mine(1) // a tip above the genesis: a settled fact needs a height of at least 1
	o, a := e.zeroConfPaid()
	e.node.HoldWallet()
	b := e.node.Replace(a, false)
	for i := 0; i < 8; i++ { // behind from pass 1, dead at 60 s (pass 7) and held, reversed in pass 8
		e.gracePass()
	}
	if _, ok := e.rev(a, 0); !ok || !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("setup: not reversed: %+v", e.events(o))
	}
	e.toWindowEnd(o)
	e.passes(3)
	if e.watched(a) {
		t.Fatalf("setup: the dead output is still watched: events %+v", e.events(o))
	}
	e.node.Evict(b) // the payer's replacement leaves, the original is mined once: the listing reopens it
	e.node.Rebroadcast(a)
	e.node.ReleaseWallet()
	e.node.Mine(1)
	e.pass()
	if m, _ := e.mark(a); m.Kind != store.MarkMined || !e.watched(a) {
		t.Fatalf("setup: not reopened: mark %+v watched %v", m, e.watched(a))
	}
	e.node.Invalidate(1) // a shallow reorg: back in the mempool
	e.pass()
	if m, _ := e.mark(a); m.Kind != store.MarkMempool {
		t.Fatalf("setup: after the reorg: mark %+v", m)
	}
	e.node.HoldWallet()
	e.node.Replace(a, false) // replaced again, the wallet shows no conflict
	e.gracePass()
	if m, _ := e.mark(a); m.Kind != store.MarkDead {
		t.Fatalf("a second 60 s was granted: mark %+v\n%s", m, e.log)
	}
	if e.rail(o).Paid {
		t.Fatalf("a second hold was granted: Rail %+v, events %+v", e.rail(o), e.events(o))
	}
	e.safety(o)
}

// The hold is remembered only once the dead mark is written: a failed write leaves heldBefore empty, and the pass that
// writes the mark holds as usual. The failure is the hook before the write (as TestBehindGraceAFailedMarkWriteKeepsTheEntry),
// or a real failure of the store's insert (a trigger), which a set placed between the hook and the write would not survive.
func TestOnceAFailedMarkWriteDoesNotUseUpTheHold(t *testing.T) {
	for _, name := range []string{"hook", "store"} {
		t.Run(name, func(t *testing.T) {
			e := newZeroConfEnv(t)
			o, a := e.zeroConfPaid()
			e.node.Evict(a)
			failed := false
			if name == "hook" {
				e.svc.hook = func(point string) error {
					if point == "track:before-mark-write" && !failed {
						failed = true
						return errors.New("injected")
					}
					return nil
				}
			} else {
				e.execDB(`CREATE TRIGGER marks_fail BEFORE INSERT ON onchain_marks BEGIN SELECT RAISE(ABORT, 'x'); END`)
			}
			_ = e.pass() // the dead mark's write fails
			if m, _ := e.mark(a); m.Kind != store.MarkMempool || len(e.svc.heldBefore) != 0 {
				t.Fatalf("after a failed write: mark %+v, heldBefore %v\n%s", m, e.svc.heldBefore, e.log)
			}
			e.svc.hook = nil
			if name == "store" {
				e.execDB(`DROP TRIGGER marks_fail`)
			}
			e.pass() // the mark is written now, and held
			if m, _ := e.mark(a); m.Kind != store.MarkDead || !e.rail(o).Paid {
				t.Fatalf("the writing pass: mark %+v, Rail %+v", m, e.rail(o))
			}
			if _, ok := e.rev(a, 0); ok {
				t.Fatal("reversed in the pass that wrote the mark: the hold was used up by the failed write")
			}
			e.pass()
			if _, ok := e.rev(a, 0); !ok || !e.hasEvent(o, store.EventReverted, 1) {
				t.Fatalf("events %+v", e.events(o))
			}
			e.safety(o)
		})
	}
}

// heldBefore is kept through a pass whose mempool is loading, a pass whose read of the output fails, and a pass that
// ends early (a transport failure), as behindSince is (TestBehindGraceAFailedReadOrALoadingMempoolKeepsTheEntry).
func TestOnceHeldBeforeSurvivesLoadingAFailedReadAndAnEarlyEnd(t *testing.T) {
	for _, name := range []string{"mempool loading", "json-rpc error", "early end"} {
		t.Run(name, func(t *testing.T) {
			e := newZeroConfEnv(t)
			o, a := e.zeroConfPaid()
			e.node.Evict(a)
			e.pass() // dead, held
			if _, ok := e.svc.heldBefore[e.key(a)]; !ok {
				t.Fatal("setup: not remembered")
			}
			mend := func() {}
			switch name {
			case "mempool loading":
				e.node.Update(func(st *onchaintest.State) { st.MempoolLoading = true })
				mend = func() { e.node.Update(func(st *onchaintest.State) { st.MempoolLoading = false }) }
			case "json-rpc error":
				e.node.Break("gettransaction", &btcrpc.Error{Code: -8, Method: "gettransaction"})
				mend = func() { e.node.Mend("gettransaction") }
			default:
				e.node.Break("gettransaction", fmt.Errorf("%w: test outage", btcrpc.ErrTransport))
				mend = func() { e.node.Mend("gettransaction") }
			}
			_ = e.pass() // an early end is an error
			if _, ok := e.svc.heldBefore[e.key(a)]; !ok {
				t.Fatalf("forgotten by the pass: %v", e.svc.heldBefore)
			}
			mend()
			e.pass() // the pass after the hold reverses it
			e.node.Rebroadcast(a)
			e.pass()
			e.node.Evict(a)
			e.pass() // dead again: not held again
			if r, ok := e.rev(a, 1); !ok || !e.hasEvent(o, store.EventReverted, 2) {
				t.Fatalf("the second death was held: reversal %+v %v, events %+v", r, ok, e.events(o))
			}
		})
	}
}

// A restart forgets both memories: each wait is granted once more.
func TestOnceARestartGrantsBothWaitsAgain(t *testing.T) {
	e := newZeroConfEnv(t)
	o, a := e.zeroConfPaid()
	e.node.HoldWallet()
	b := e.node.Replace(a, false)
	for i := 0; i < 7; i++ { // dead at 60 s, held one pass
		e.gracePass()
	}
	e.node.Evict(b)
	e.node.Rebroadcast(a)
	e.gracePass()
	e.restart()
	e.node.HoldWallet() // the restart does not change the node's wallet view
	e.node.Replace(a, false)
	for i := 1; i <= 6; i++ { // a new 60 s
		e.gracePass()
		if m, _ := e.mark(a); m.Kind != store.MarkMempool || !e.rail(o).Paid {
			t.Fatalf("restarted pass %d: mark %+v", i, m)
		}
	}
	e.gracePass() // dead, and held one pass again
	if m, _ := e.mark(a); m.Kind != store.MarkDead || !e.rail(o).Paid {
		t.Fatalf("the new 60 s: mark %+v, Rail %+v", m, e.rail(o))
	}
	if _, ok := e.rev(a, 0); ok {
		t.Fatalf("no hold after the restart")
	}
	e.gracePass()
	if _, ok := e.rev(a, 0); !ok || !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("events %+v", e.events(o))
	}
	e.safety(o)
}

// A non-0 tier: the final event's hold is granted once per output too. The first death of the unconfirmed payment
// after the expiry holds order.expired one pass; after it comes back and dies again, order.expired is written in the
// pass that marks it dead.
func TestOnceNonZeroTierFinalEventHoldOnce(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	e.pastExpiry(o)
	e.node.Evict(a)
	e.pass()
	if m, _ := e.mark(a); m.Kind != store.MarkDead || len(e.events(o)) != 0 {
		t.Fatalf("the held pass: mark %+v, events %+v", m, e.events(o))
	}
	e.node.Rebroadcast(a)
	e.pass()
	if m, _ := e.mark(a); m.Kind != store.MarkMempool || len(e.events(o)) != 0 {
		t.Fatalf("back: mark %+v, events %+v", m, e.events(o))
	}
	e.node.Evict(a)
	e.pass()
	if m, _ := e.mark(a); m.Kind != store.MarkDead || !e.hasEvent(o, store.EventExpired, 1) || len(e.events(o)) != 1 {
		t.Fatalf("the second death: mark %+v, events %+v\n%s", m, e.events(o), e.log)
	}
	e.safety(o)
}
