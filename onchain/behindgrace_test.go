package onchain

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

// --- the wallet-behind wait is bounded: behindGrace after the pass first found the payment replaced ---

// gracePass runs one pass 10 s (the poll interval) after the last.
func (e *env) gracePass() {
	e.t.Helper()
	e.clk.Advance(10 * time.Second)
	if err := e.pass(); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) key(txid string) store.TXOKey { return store.TXOKey{TxID: txid, Vout: e.txo(txid).Vout} }

// graceChain runs this sequence: A is recorded on a 0 tier, the wallet never shows a conflict to any pass,
// and before every pass the payer replaces the node's last replacement again (to itself, or to the order's address
// with A's amount), so the node always has a spender. It returns the pass number (1 = the first) at which A's mark
// turned dead and the one at which A was reversed, over at most 14 passes.
func graceChain(t *testing.T, payOrder bool) (dead, reversed int) {
	t.Helper()
	e := newZeroConfEnv(t)
	o, a := e.zeroConfPaid()
	addr := e.txo(a).Address
	e.node.HoldWallet()
	last := a
	for i := 1; i <= 14; i++ {
		if payOrder {
			last = e.node.ReplaceWith(last, []onchaintest.Out{{Address: addr, Sat: 5_000}})
		} else {
			last = e.node.Replace(last, false)
		}
		e.gracePass()
		if m, _ := e.mark(a); dead == 0 && m.Kind == store.MarkDead {
			dead = i
			if _, ok := e.rev(a, 0); ok || len(e.events(o)) != 1 || !e.rail(o).Paid {
				t.Fatalf("pass %d (dead mark): reversed %v, events %+v", i, ok, e.events(o))
			}
		}
		if _, ok := e.rev(a, 0); ok && reversed == 0 {
			reversed = i
			if r, _ := e.rev(a, 0); r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || e.rail(o).Paid {
				t.Fatalf("pass %d: reversal %+v, events %+v", i, r, e.events(o))
			}
		}
	}
	if e.eventCount(o, store.EventReverted) != 1 {
		t.Fatalf("events %+v", e.events(o))
	}
	return dead, reversed
}

// The wallet never shows a conflict while the payer keeps replacing: the payment is behind for less than 60 s of
// 10 s passes (passes 1 to 6: first found at pass 1, 50 s later still behind), marked dead in the pass at 60 s (7), held
// that pass and reversed with order.reverted seq 1 in the next (8), as before the wallet-behind rule existed.
func TestBehindGraceEndsTheWaitWhenTheWalletNeverShowsAConflict(t *testing.T) {
	for _, payOrder := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacementsPayTheOrder=%v", payOrder), func(t *testing.T) {
			if dead, reversed := graceChain(t, payOrder); dead != 7 || reversed != 8 {
				t.Fatalf("marked dead in pass %d, reversed in pass %d; want 7 and 8", dead, reversed)
			}
		})
	}
}

// One fee bump with the wallet held 20 s: nothing changes (the case the rule was built for).
func TestBehindGraceDoesNotHurtASingleBump(t *testing.T) {
	e := newZeroConfEnv(t)
	o, old := e.zeroConfPaid()
	e.node.HoldWallet()
	nw := e.node.Replace(old, true)
	e.gracePass()
	e.gracePass()
	if m, _ := e.mark(old); m.Kind != store.MarkMempool || len(e.events(o)) != 1 || !e.rail(o).Paid {
		t.Fatalf("while held: %+v, events %+v", m, e.events(o))
	}
	e.node.ReleaseWallet()
	e.gracePass()
	e.gracePass()
	if f, ok := e.fwd(nw, 0); !ok || f.Code != codePaid {
		t.Fatalf("the replacement: %+v %v", f, ok)
	}
	if r, ok := e.rev(old, 0); !ok || r.UserData64 != 0 || len(e.events(o)) != 1 || !e.rail(o).Paid || e.count(alertPaidAgain) != 0 {
		t.Fatalf("the old record: %+v %v, events %+v", r, ok, e.events(o))
	}
	if _, ok := e.svc.behindSince[e.key(old)]; len(e.svc.behindSince) != 1 || !ok { // kept while watched (once rule)
		t.Fatalf("behindSince %v", e.svc.behindSince)
	}
	e.safety(o)
}

// The wallet catches up at 59 s: the conflict path writes the dead mark (no gettxspendingprevout call in that pass),
// not the grace path (which asks the node once more).
func TestBehindGraceTheWalletCatchesUpAt59Seconds(t *testing.T) {
	e := newZeroConfEnv(t)
	o, old := e.zeroConfPaid()
	e.node.HoldWallet()
	e.node.Replace(old, true)
	e.gracePass() // first found behind
	k := e.key(old)
	if _, ok := e.svc.behindSince[k]; !ok {
		t.Fatal("no entry after the first behind pass")
	}
	e.node.ReleaseWallet()
	e.clk.Advance(59 * time.Second) // 59 s after the first find
	calls := e.spends()
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if m, _ := e.mark(old); m.Kind != store.MarkDead || e.spends() != calls || len(e.svc.behindSince) != 1 {
		t.Fatalf("conflict path: mark %+v, %d gettxspendingprevout calls (from %d), entries %v", m, e.spends(), calls, e.svc.behindSince)
	}
	e.gracePass()
	e.gracePass()
	if r, ok := e.rev(old, 0); !ok || r.UserData64 != 0 || len(e.events(o)) != 1 {
		t.Fatalf("%+v %v, events %+v", r, ok, e.events(o))
	}
	e.safety(o)
}

// The grace path asks the node once more in the pass that marks the output dead (the second half of the pin above).
func TestBehindGraceMarkIsWrittenByTheGracePathAt60Seconds(t *testing.T) {
	e := newZeroConfEnv(t)
	_, old := e.zeroConfPaid()
	e.node.HoldWallet()
	e.node.Replace(old, true)
	e.gracePass()
	e.clk.Advance(60 * time.Second) // 60 s after the first find
	calls := e.spends()
	if err := e.pass(); err != nil {
		t.Fatal(err)
	}
	if m, _ := e.mark(old); m.Kind != store.MarkDead || e.spends() != calls+1 {
		t.Fatalf("grace path: mark %+v, %d calls (from %d)", m, e.spends(), calls)
	}
}

// An output that returns to the mempool while behind keeps its entry: the 60 s count from the first find for as long as
// it is watched in this process. Found behind again after them, it is marked dead in that pass (pass 7, 60 s after pass
// 1), and reversed in the next.
func TestBehindGraceBackInTheMempoolKeepsTheFirstFind(t *testing.T) {
	e := newZeroConfEnv(t)
	o, a := e.zeroConfPaid()
	k := e.key(a)
	e.node.HoldWallet()
	b := e.node.Replace(a, false)
	e.gracePass() // pass 1: first found
	first := e.svc.behindSince[k]
	e.gracePass()
	e.gracePass()
	e.node.Evict(b)
	e.node.Rebroadcast(a) // the original is back
	e.gracePass()         // pass 4
	if m, _ := e.mark(a); m.Kind != store.MarkMempool || !e.svc.behindSince[k].Equal(first) {
		t.Fatalf("back in the mempool: mark %+v, since %v (first %v)", m, e.svc.behindSince[k], first)
	}
	e.node.Replace(a, false)
	e.gracePass() // 5
	e.gracePass() // 6
	if m, _ := e.mark(a); m.Kind != store.MarkMempool {
		t.Fatalf("pass 6: mark %+v", m)
	}
	e.gracePass() // 7: 60 s after the first find
	if m, _ := e.mark(a); m.Kind != store.MarkDead || len(e.svc.behindSince) != 1 {
		t.Fatalf("pass 7: mark %+v, entries %v", m, e.svc.behindSince)
	}
	if _, ok := e.rev(a, 0); ok {
		t.Fatal("reversed in the pass that marked it dead")
	}
	e.gracePass()
	if r, ok := e.rev(a, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("pass 8: %+v %v, events %+v", r, ok, e.events(o))
	}
	e.gracePass()
	e.safety(o)
}

// The cycle: A is replaced, the replacement leaves and A is back in the mempool for one pass, A is replaced
// again, and so on. A is marked dead no later than 60 s after it was first found behind (pass 7), reversed in pass 8.
func TestBehindGraceCyclingTheOriginalBackDoesNotRestartTheWait(t *testing.T) {
	e := newZeroConfEnv(t)
	o, a := e.zeroConfPaid()
	e.node.HoldWallet()
	dead, pass := 0, 0
	step := func() {
		pass++
		e.gracePass()
		if m, _ := e.mark(a); dead == 0 && m.Kind == store.MarkDead {
			dead = pass
		}
	}
	for cycle := 0; cycle < 4 && dead == 0; cycle++ {
		b := e.node.Replace(a, false)
		step()
		step()
		step()
		if dead != 0 {
			break
		}
		e.node.Evict(b)
		e.node.Rebroadcast(a)
		step() // A is in the mempool for this pass
	}
	if dead != 7 {
		t.Fatalf("A marked dead in pass %d, want 7", dead)
	}
	e.gracePass()
	if r, ok := e.rev(a, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("reversal %+v %v, events %+v", r, ok, e.events(o))
	}
}

// A failure at the dead mark's write (the hook stands for a failed AddMark) does not start the wait again: the entry is
// deleted only after the write, so the next pass marks the output dead, not 60 s later.
func TestBehindGraceAFailedMarkWriteKeepsTheEntry(t *testing.T) {
	e := newZeroConfEnv(t)
	o, a := e.zeroConfPaid()
	e.node.HoldWallet()
	e.node.Replace(a, false)
	for i := 1; i <= 6; i++ {
		e.gracePass()
	}
	failed := false
	e.svc.hook = func(point string) error {
		if point == "track:before-mark-write" && !failed {
			failed = true
			return errors.New("injected")
		}
		return nil
	}
	e.clk.Advance(10 * time.Second) // pass 7, 60 s after the first find: the grace path, its write fails
	if err := e.pass(); err == nil || !failed {
		t.Fatalf("the pass did not fail (%v, failed %v)", err, failed)
	}
	if m, _ := e.mark(a); m.Kind != store.MarkMempool {
		t.Fatalf("mark %+v", m)
	}
	e.svc.hook = nil
	e.gracePass() // pass 8
	if m, _ := e.mark(a); m.Kind != store.MarkDead {
		t.Fatalf("the next pass: mark %+v", m)
	}
	e.gracePass()
	if _, ok := e.rev(a, 0); !ok || !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("events %+v", e.events(o))
	}
}

// A pass in which the output's read fails, or the mempool is loading, leaves the entry: the grace is still counted
// from the first find (pins that no such arm of trackAll deletes it).
func TestBehindGraceAFailedReadOrALoadingMempoolKeepsTheEntry(t *testing.T) {
	for _, name := range []string{"json-rpc error", "mempool loading"} {
		t.Run(name, func(t *testing.T) {
			e := newZeroConfEnv(t)
			o, a := e.zeroConfPaid()
			e.node.HoldWallet()
			e.node.Replace(a, false)
			e.gracePass() // pass 1: first found
			if name == "json-rpc error" {
				e.node.Break("gettxspendingprevout", &btcrpc.Error{Code: -8, Method: "gettxspendingprevout"})
			} else {
				e.node.Update(func(st *onchaintest.State) { st.MempoolLoading = true })
			}
			e.gracePass() // pass 2: the read fails, or the mempool is loading
			if name == "json-rpc error" {
				e.node.Mend("gettxspendingprevout")
			} else {
				e.node.Update(func(st *onchaintest.State) { st.MempoolLoading = false })
			}
			for i := 3; i <= 6; i++ {
				e.gracePass()
			}
			if m, _ := e.mark(a); m.Kind != store.MarkMempool {
				t.Fatalf("pass 6: mark %+v", m)
			}
			e.gracePass() // pass 7: 60 s after the first find
			if m, _ := e.mark(a); m.Kind != store.MarkDead {
				t.Fatalf("pass 7: mark %+v\n%s", m, e.log)
			}
			e.gracePass()
			e.safety(o)
		})
	}
}

// The map is never pruned: an entry for an output that is not tracked stays, and so do those of a dead output after it
// is mined and settled and no longer watched (the memories live as long as the process).
func TestBehindGraceMapIsNeverPruned(t *testing.T) {
	e := newZeroConfEnv(t)
	o, a := e.zeroConfPaid()
	e.svc.behindSince[store.TXOKey{TxID: "ff", Vout: 7}] = e.clk.Now()
	e.gracePass()
	if len(e.svc.behindSince) != 1 {
		t.Fatalf("entries %v", e.svc.behindSince)
	}
	e.node.HoldWallet()
	b := e.node.Replace(a, false)
	for i := 0; i < 9; i++ {
		e.gracePass()
	}
	if m, _ := e.mark(a); m.Kind != store.MarkDead || len(e.svc.behindSince) != 2 || len(e.svc.heldBefore) != 1 {
		t.Fatalf("mark %+v, entries %v %v", m, e.svc.behindSince, e.svc.heldBefore)
	}
	e.node.Evict(b) // the original comes back and is mined deep: settled for good, so no longer watched
	e.node.Rebroadcast(a)
	e.node.ReleaseWallet()
	e.node.Mine(10)
	e.passes(4)
	if e.watched(a) {
		t.Fatal("the output is still watched")
	}
	if _, ok := e.svc.behindSince[e.key(a)]; !ok || len(e.svc.behindSince) != 2 || len(e.svc.heldBefore) != 1 {
		t.Fatalf("entries %v %v", e.svc.behindSince, e.svc.heldBefore)
	}
	e.safety(o)
}

// A restart while behind at 50 s: the new process waits its own 60 s (the times are in memory), then marks dead.
func TestBehindGraceRestartStartsTheWaitAgain(t *testing.T) {
	e := newZeroConfEnv(t)
	_, a := e.zeroConfPaid()
	e.node.HoldWallet()
	e.node.Replace(a, false)
	for i := 0; i < 6; i++ { // first found at pass 1; pass 6 is at 50 s
		e.gracePass()
	}
	if m, _ := e.mark(a); m.Kind != store.MarkMempool {
		t.Fatalf("mark %+v", m)
	}
	e.restart()
	for i := 0; i < 6; i++ { // the new process: found at its first pass, behind for 50 s more
		e.gracePass()
		if m, _ := e.mark(a); m.Kind != store.MarkMempool {
			t.Fatalf("restarted pass %d: mark %+v", i+1, m)
		}
	}
	e.gracePass()
	if m, _ := e.mark(a); m.Kind != store.MarkDead {
		t.Fatalf("after the new 60 s: mark %+v", m)
	}
}

// A non-0 tier: behind, the output is not recorded and the order's final event waits; after the grace it is dead and
// the final event is written as for any dead output (one pass later: the hold of the pass that marked it).
func TestBehindGraceNonZeroTierFinalEvent(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	e.node.HoldWallet()
	e.node.Replace(a, false)
	e.pastExpiry(o)
	for i := 0; i < 6; i++ {
		e.gracePass()
	}
	if m, _ := e.mark(a); m.Kind != store.MarkMempool || len(e.events(o)) != 0 || e.count(logNoFinal) != 0 {
		t.Fatalf("while behind: mark %+v, events %+v", m, e.events(o))
	}
	e.gracePass() // 60 s
	if m, _ := e.mark(a); m.Kind != store.MarkDead || len(e.events(o)) != 0 {
		t.Fatalf("the grace pass: mark %+v, events %+v", m, e.events(o))
	}
	e.gracePass()
	if !e.hasEvent(o, store.EventExpired, 1) || len(e.events(o)) != 1 || e.count(logNoFinal) != 0 {
		t.Fatalf("one pass later: events %+v\n%s", e.events(o), e.log)
	}
	e.safety(o)
}

// The window end crossed inside the lag, in one process (no restart): the lag starts 5 s before the window's end and
// crosses it within the 60 s. No "no final event" ALERT, no event while behind, the final event after the catch-up.
func TestBehindGraceWindowEndCrossedInProcess(t *testing.T) {
	for _, heldBefore := range []bool{false, true} {
		t.Run(fmt.Sprintf("held-before=%v", heldBefore), func(t *testing.T) {
			e := newWatchEnv(t)
			o := e.order(100_000)
			a := e.payOrder(o, 100_000)
			e.pass()
			if heldBefore {
				e.pastExpiry(o)
				e.pass()
			}
			e.toWindowEnd(o)
			e.clk.Advance(-5 * time.Second)
			e.node.HoldWallet()
			e.node.Replace(a, false)
			e.passes(2) // before the window end, behind
			e.passes(5) // crosses the window end, still behind (under 60 s)
			if e.count(logNoFinal) != 0 || len(e.events(o)) != 0 || e.count(logWalletBehind) == 0 {
				t.Fatalf("while held: noFinal %d events %+v\n%s", e.count(logNoFinal), e.events(o), e.log)
			}
			e.node.ReleaseWallet()
			e.passes(4)
			if e.count(logNoFinal) != 0 || len(e.events(o)) != 1 || e.eventCount(o, store.EventExpired) != 1 {
				t.Fatalf("after the release: events %+v\n%s", e.events(o), e.log)
			}
			e.safety(o)
		})
	}
}

// An output first found behind before the expiry, the expiry crossed during the lag: no final event while behind, and
// order.expired once the wallet has caught up (a diverted payment).
func TestBehindGraceExpiryCrossedDuringTheLag(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	e.clk.Advance(o.ExpiresAt.Sub(e.clk.Now()) - 15*time.Second)
	e.node.HoldWallet()
	e.node.Replace(a, false)
	for i := 0; i < 5; i++ { // the second pass crosses the expiry
		e.gracePass()
	}
	if e.clk.Now().Before(o.ExpiresAt) || len(e.events(o)) != 0 || e.count(logNoFinal) != 0 || e.count(logWalletBehind) == 0 {
		t.Fatalf("while held: events %+v\n%s", e.events(o), e.log)
	}
	e.node.ReleaseWallet()
	e.passes(4)
	if len(e.events(o)) != 1 || e.eventCount(o, store.EventExpired) != 1 || e.count(logNoFinal) != 0 {
		t.Fatalf("after the release: events %+v\n%s", e.events(o), e.log)
	}
	e.safety(o)
}
