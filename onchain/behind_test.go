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

// --- the wallet is behind the node's mempool: a payment is not dead for that (gettxspendingprevout) ---

// spends is the number of gettxspendingprevout calls so far.
func (e *env) spends() int { return e.node.Calls("gettxspendingprevout") }

// A fee bump of a paid 0-confirmation payment while bitcoind's wallet lags its node: the node has dropped the original
// and holds the replacement, the wallet shows neither a conflict nor the replacement. Five passes leave the order paid
// with nothing reversed (the rule: errBehind, no mark, one WARN and one gettxspendingprevout per pass); when the wallet
// has caught up, the end state is the one of TestZeroConfBumpedAcrossTwoPasses. Without the rule the first pass marks the
// original dead and the second reverses it with order.reverted.
func TestBehindBumpWhileTheWalletLagsStaysPaid(t *testing.T) {
	e := newZeroConfEnv(t)
	o, old := e.zeroConfPaid()
	e.node.HoldWallet()
	nw := e.node.Replace(old, true)
	calls := e.spends()
	for i := 1; i <= 5; i++ {
		e.clk.Advance(time.Second)
		if err := e.pass(); err != nil {
			t.Fatal(err)
		}
		if m, _ := e.mark(old); m.Kind != store.MarkMempool {
			t.Fatalf("pass %d: mark %+v", i, m)
		}
		if _, ok := e.rev(old, 0); ok || len(e.events(o)) != 1 || !e.rail(o).Paid || e.status(o) != "paid" || e.count(alertReorg) != 0 ||
			e.count(alertPaidAgain) != 0 || e.eventCount(o, store.EventReverted) != 0 {
			t.Fatalf("pass %d: reversed %v, events %+v, Rail %+v\n%s", i, ok, e.events(o), e.rail(o), e.log)
		}
		if e.count(logWalletBehind) != i || e.spends() != calls+i {
			t.Fatalf("pass %d: %d WARN lines, %d calls (from %d)", i, e.count(logWalletBehind), e.spends(), calls)
		}
		// the stale set: the order is not put in it (it would hold a peer rail's final event)
		if e.rail(o).Stale {
			t.Fatalf("pass %d: the order reads stale", i)
		}
	}
	if l := e.txos(o); len(l) != 1 {
		t.Fatalf("the replacement is known to the store: %+v", l)
	}
	e.node.ReleaseWallet()
	e.passes(2)
	if f, ok := e.fwd(nw, 0); !ok || f.Code != codePaid || !e.txo(nw).FirstSeen.Equal(e.txo(old).FirstSeen) {
		t.Fatalf("the replacement: %+v %v", f, ok)
	}
	if r, ok := e.rev(old, 0); !ok || r.UserData64 != 0 {
		t.Fatalf("the old record: %+v %v", r, ok)
	}
	if r := e.rail(o); !r.Paid || r.Overpaid || r.ConfirmedSat != 5_000 || len(e.events(o)) != 1 || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("after the release: %+v %+v", r, e.events(o))
	}
	e.passes(2)
	e.node.Mine(1)
	e.passes(2)
	if e.count(alertOverpaid) != 0 || e.count(alertReorg) != 1 || e.count(alertPaidAgain) != 0 || len(e.events(o)) != 1 || e.count(logWalletBehind) != 5 {
		t.Fatalf("events %+v\n%s", e.events(o), e.log)
	}
	e.safety(o)
}

// A double spend to another wallet while the wallet lags: the order stays paid and nothing is reversed while the wallet
// is behind. After the release the first pass marks the output dead (the wallet shows the conflict) and holds the
// reversal for the one pass; the second reverses it with order.reverted: as without the lag, one pass after the
// wallet has caught up. (The cost: a real double spend is reported when the wallet has seen it, not when the node has.)
func TestBehindDivertWhileTheWalletLagsIsRevertedAfterTheRelease(t *testing.T) {
	e := newZeroConfEnv(t)
	o, old := e.zeroConfPaid()
	e.node.HoldWallet()
	e.node.Replace(old, false)
	e.passes(5)
	if m, _ := e.mark(old); m.Kind != store.MarkMempool {
		t.Fatalf("mark %+v", m)
	}
	if _, ok := e.rev(old, 0); ok || len(e.events(o)) != 1 || !e.rail(o).Paid || e.count(alertReorg) != 0 {
		t.Fatalf("while held: reversed %v, events %+v", ok, e.events(o))
	}
	e.node.ReleaseWallet()
	e.passes(1)
	if m, _ := e.mark(old); m.Kind != store.MarkDead {
		t.Fatalf("the first pass after the release: mark %+v", m)
	}
	if _, ok := e.rev(old, 0); ok || len(e.events(o)) != 1 || !e.rail(o).Paid || e.count(alertReorg) != 0 {
		t.Fatalf("the first pass after the release: reversed %v, events %+v\n%s", ok, e.events(o), e.log)
	}
	e.passes(1)
	if r, ok := e.rev(old, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || e.count(alertReorg) != 1 || e.rail(o).Paid {
		t.Fatalf("the second pass after the release: %+v %v, events %+v", r, ok, e.events(o))
	}
	e.passes(2)
	if len(e.events(o)) != 2 || e.count(alertReorg) != 1 {
		t.Fatalf("replayed: %+v", e.events(o))
	}
	e.safety(o)
}

// An evicted or expired payment (no spender): dead and reverted one pass later, with the wallet behind or not (pins
// today's behaviour; the cost is one gettxspendingprevout).
func TestBehindEvictedIsDeadAndRevertedOnePassLater(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("held=%v", held), func(t *testing.T) {
			e := newZeroConfEnv(t)
			o, txid := e.zeroConfPaid()
			if held {
				e.node.HoldWallet()
			}
			e.node.Evict(txid)
			calls := e.spends()
			e.passes(1)
			if m, _ := e.mark(txid); m.Kind != store.MarkDead || e.spends() != calls+1 || e.count(logWalletBehind) != 0 {
				t.Fatalf("mark %+v, %d calls (from %d)", m, e.spends(), calls)
			}
			if _, ok := e.rev(txid, 0); ok || len(e.events(o)) != 1 {
				t.Fatalf("the held pass: reversed %v, events %+v", ok, e.events(o))
			}
			e.passes(1)
			if r, ok := e.rev(txid, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) {
				t.Fatalf("reversal %+v %v, events %+v", r, ok, e.events(o))
			}
			// an output already dead is not asked again (the !in && wasDead branch)
			calls = e.spends()
			e.passes(2)
			if e.spends() != calls {
				t.Fatalf("a dead output asked again: %d calls (from %d)", e.spends(), calls)
			}
			e.safety(o)
		})
	}
}

// The spender leaves the node's mempool while the wallet is behind (the replacement is evicted): nothing spends the
// inputs any more, and the next pass marks the original dead.
func TestBehindSpenderLeavesTheMempool(t *testing.T) {
	e := newZeroConfEnv(t)
	o, old := e.zeroConfPaid()
	e.node.HoldWallet()
	nw := e.node.Replace(old, true)
	e.passes(2)
	if m, _ := e.mark(old); m.Kind != store.MarkMempool || e.count(logWalletBehind) != 2 {
		t.Fatalf("mark %+v", m)
	}
	e.node.Evict(nw)
	e.passes(1)
	if m, _ := e.mark(old); m.Kind != store.MarkDead || e.count(logWalletBehind) != 2 {
		t.Fatalf("mark %+v\n%s", m, e.log)
	}
	e.passes(1)
	if !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("events %+v", e.events(o))
	}
	e.safety(o)
}

// backAfterEntry wraps the node: after the nth getmempoolentry for txid it answers as the node did, then puts the
// transaction back in the mempool.
type backAfterEntry struct {
	RPC
	node *onchaintest.Node
	txid string
	n    int
	seen int
}

func (w *backAfterEntry) InMempool(ctx context.Context, txid string) (bool, error) {
	in, err := w.RPC.InMempool(ctx, txid)
	if txid == w.txid {
		if w.seen++; w.seen == w.n {
			w.node.Rebroadcast(txid)
		}
	}
	return in, err
}

// The transaction is back in the mempool when gettxspendingprevout is asked (between the node's two calls): its own
// id is the spender, so the mark is mempool and nothing is dead.
func TestBehindBackInTheMempoolAtTheSpendingCall(t *testing.T) {
	e := newZeroConfEnv(t)
	o, txid := e.zeroConfPaid()
	e.node.Evict(txid)
	e.svc.rpc = &backAfterEntry{RPC: e.node, node: e.node, txid: txid, n: 2}
	calls := e.spends()
	e.passes(1)
	if m, _ := e.mark(txid); m.Kind != store.MarkMempool || e.spends() != calls+1 {
		t.Fatalf("mark %+v, %d calls (from %d)", m, e.spends(), calls)
	}
	if len(e.diedInPassForTest()) != 0 || e.count(logWalletBehind) != 0 || !e.rail(o).Paid || len(e.events(o)) != 1 {
		t.Fatalf("died %v, events %+v\n%s", e.diedInPassForTest(), e.events(o), e.log)
	}
	e.passes(2)
	if _, ok := e.rev(txid, 0); ok || e.count(alertReorg) != 0 || len(e.events(o)) != 1 {
		t.Fatal("reversed")
	}
	e.safety(o)
}

func (e *env) diedInPassForTest() []store.TXOKey {
	var l []store.TXOKey
	for k := range e.svc.diedInPass {
		l = append(l, k)
	}
	return l
}

// noInputs wraps the node: its gettransaction answers carry no inputs (a coinbase's only input adds none).
type noInputs struct{ RPC }

func (w noInputs) GetTransaction(ctx context.Context, txid string) (btcrpc.Transaction, error) {
	tx, err := w.RPC.GetTransaction(ctx, txid)
	tx.Inputs = nil
	return tx, err
}

// A transaction with no inputs to ask about is dead as before, without the call.
func TestBehindNoInputsIsDeadWithoutTheCall(t *testing.T) {
	e := newZeroConfEnv(t)
	_, txid := e.zeroConfPaid()
	e.svc.rpc = noInputs{e.node}
	e.node.Evict(txid)
	calls := e.spends()
	e.passes(1)
	if m, _ := e.mark(txid); m.Kind != store.MarkDead || e.spends() != calls {
		t.Fatalf("mark %+v, %d calls (from %d)", m, e.spends(), calls)
	}
}

// A non-zero tier, bumped while the wallet lags, across the order's expiry: no dead mark, no final event made early,
// no ALERT; after the release the bump inherits the first-seen and the block pays the order as without the lag.
func TestBehindNonZeroTierBumpAcrossExpiry(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	first := e.txo(a).FirstSeen
	e.node.HoldWallet()
	b := e.node.Replace(a, true)
	e.pastExpiry(o)
	e.passes(3)
	if m, _ := e.mark(a); m.Kind != store.MarkMempool || len(e.events(o)) != 0 || e.count(logNoFinal) != 0 || e.count(logWalletBehind) != 3 {
		t.Fatalf("while held: mark %+v, events %+v\n%s", m, e.events(o), e.log)
	}
	if r := e.rail(o); r.ReceivedSat != 100_000 || r.State != RailSeen {
		t.Fatalf("Rail %+v", r)
	}
	e.node.ReleaseWallet()
	e.passes(2)
	if x := e.txo(b); !x.FirstSeen.Equal(first) {
		t.Fatalf("the bump's first-seen %v, the original's %v", x.FirstSeen, first)
	}
	if len(e.events(o)) != 0 {
		t.Fatalf("a final event while the bump is being paid: %+v", e.events(o))
	}
	e.node.Mine(1)
	e.passes(3)
	if f, ok := e.fwd(b, 0); !ok || f.Code != codePaid || !e.rail(o).Paid || len(e.events(o)) != 1 || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("mined: %+v %v %+v", f, ok, e.events(o))
	}
	if e.count(logPaidAfterFinal) != 0 || e.count(logUnderpaid) != 0 || e.count(logLate) != 0 || e.count(logNoFinal) != 0 {
		t.Fatalf("\n%s", e.log)
	}
	e.safety(o)
}

// Behind across the end of the watch window, also when the order was held in the pass before (heldInPass, leftHeld): no
// "no final event" ALERT while the wallet lags, and the final event comes once it has caught up. The payment is
// diverted, so the order ends without a payment on the rail.
func TestBehindAcrossTheWindowEndRaisesNoFalseAlert(t *testing.T) {
	for _, heldBefore := range []bool{false, true} {
		t.Run(fmt.Sprintf("held-before=%v", heldBefore), func(t *testing.T) {
			e := newWatchEnv(t)
			o := e.order(100_000)
			a := e.payOrder(o, 100_000)
			e.pass()
			e.node.HoldWallet()
			e.node.Replace(a, false)
			e.passes(2)
			if heldBefore {
				e.pastExpiry(o)
				e.restart() // the clock jumped an hour, past behindGrace: a restart starts the wait again
				e.passes(2)
			}
			e.toWindowEnd(o)
			e.restart() // the clock jumped a week, far past behindGrace: a restart starts the wait again, as a lag that spans the window end would
			e.passes(3)
			if e.count(logNoFinal) != 0 || len(e.events(o)) != 0 || e.count(logWalletBehind) == 0 {
				t.Fatalf("while held: events %+v\n%s", e.events(o), e.log)
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

// A restart (a new Service on the same store and node) while the wallet is behind: the same outcome, within the grace.
// What the restart loses is the time the output was first found behind (behindSince): its 60 s start again
// (TestBehindGraceRestartStartsTheWaitAgain).
func TestBehindAcrossARestart(t *testing.T) {
	e := newZeroConfEnv(t)
	o, old := e.zeroConfPaid()
	e.node.HoldWallet()
	nw := e.node.Replace(old, true)
	e.passes(2)
	e.restart()
	e.passes(3)
	if m, _ := e.mark(old); m.Kind != store.MarkMempool || len(e.events(o)) != 1 || !e.rail(o).Paid || e.count(alertReorg) != 0 {
		t.Fatalf("while held: mark %+v, events %+v\n%s", m, e.events(o), e.log)
	}
	e.node.ReleaseWallet()
	e.passes(2)
	if f, ok := e.fwd(nw, 0); !ok || f.Code != codePaid {
		t.Fatalf("the replacement: %+v %v", f, ok)
	}
	if r, ok := e.rev(old, 0); !ok || r.UserData64 != 0 || !e.rail(o).Paid || len(e.events(o)) != 1 {
		t.Fatalf("the old record: %+v %v, events %+v", r, ok, e.events(o))
	}
	e.passes(2)
	e.node.Mine(1)
	e.passes(2)
	if e.count(alertPaidAgain) != 0 || len(e.events(o)) != 1 {
		t.Fatalf("events %+v\n%s", e.events(o), e.log)
	}
	e.safety(o)
}

// A failed gettxspendingprevout never marks dead. A transport failure ends the pass (ALERT: bitcoind call failed, as
// for the other calls) and makes the order stale; a JSON-RPC error or a bad answer skips the output (logOutputFailed
// with the method), makes the order stale and marks nothing.
func TestBehindTheSpendingCallFails(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		e := newZeroConfEnv(t)
		o, txid := e.zeroConfPaid()
		e.node.Evict(txid)
		e.node.Break("gettxspendingprevout", errOutage)
		e.clk.Advance(time.Second)
		if err := e.pass(); err == nil {
			t.Fatal("the pass did not end")
		}
		if m, _ := e.mark(txid); m.Kind != store.MarkMempool || e.count(logCallFailed) != 1 ||
			!strings.Contains(e.log.String(), "method=gettxspendingprevout") {
			t.Fatalf("mark %+v\n%s", m, e.log)
		}
		e.node.Mend("gettxspendingprevout")
		e.passes(1)
		if m, _ := e.mark(txid); m.Kind != store.MarkDead {
			t.Fatalf("mended: %+v", m)
		}
		e.passes(1)
		e.safety(o)
	})
	for name, failure := range map[string]error{
		"json-rpc error": &btcrpc.Error{Code: -8, Method: "gettxspendingprevout"},
		"bad answer":     fmt.Errorf("%w: gettxspendingprevout", btcrpc.ErrBadAnswer),
	} {
		t.Run(name, func(t *testing.T) {
			e := newZeroConfEnv(t)
			o, txid := e.zeroConfPaid()
			e.node.Evict(txid)
			e.node.Break("gettxspendingprevout", failure)
			e.passes(3)
			if m, _ := e.mark(txid); m.Kind != store.MarkMempool || e.count(logOutputFailed) != 3 || e.count(logCallFailed) != 0 ||
				!strings.Contains(e.log.String(), "method=gettxspendingprevout") {
				t.Fatalf("mark %+v\n%s", m, e.log)
			}
			if !e.rail(o).Paid || !e.rail(o).Stale || len(e.events(o)) != 1 {
				t.Fatalf("Rail %+v, events %+v", e.rail(o), e.events(o))
			}
			e.node.Mend("gettxspendingprevout")
			e.passes(1)
			if m, _ := e.mark(txid); m.Kind != store.MarkDead {
				t.Fatalf("mended: %+v", m)
			}
			e.passes(1)
			e.safety(o)
		})
	}
}

// Check probes gettxspendingprevout as it probes getmempoolinfo: a bitcoind whose rpcwhitelist lacks it (an upgrade
// that did not add the twelfth method: HTTP 403) or that refuses it (a JSON-RPC error) fails the startup check with the
// unavailable class, instead of every pass at the first fee bump. A mempool still loading is no failure.
func TestCheckNeedsGettxspendingprevout(t *testing.T) {
	for name, failure := range map[string]error{
		"whitelist":        fmt.Errorf("%w: %w: 403", btcrpc.ErrHTTP, btcrpc.ErrTransport),
		"method not found": &btcrpc.Error{Code: -32601, Method: "gettxspendingprevout"},
		"bad options":      &btcrpc.Error{Code: -8, Method: "gettxspendingprevout"},
	} {
		e := newEnv(t)
		e.node.Break("gettxspendingprevout", failure)
		err := e.svc.Check(bg)
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s: Check = %v, want ErrUnavailable", name, err)
		}
		e.node.Mend("gettxspendingprevout")
		e.node.Update(func(s *onchaintest.State) { s.MempoolLoading = true })
		if err := e.svc.Check(bg); err != nil {
			t.Fatalf("%s: Check while the mempool loads: %v", name, err)
		}
		if e.node.Calls("gettxspendingprevout") == 0 {
			t.Fatal("not probed")
		}
	}
}

// --- per output: the order is not skipped as a whole ---

// A payment A that the wallet knows is gone keeps its schedule while a second, tiny payment B of the same order is
// behind: A is marked dead in the first pass and reversed in the next, with order.reverted, although B stays behind for
// six passes. (With the order skipped as a whole, A stayed unreversed and the order read paid for as long as B lagged.)
func TestBehindASiblingBehindDoesNotKeepAGonePaymentPaid(t *testing.T) {
	e := newZeroConfEnv(t)
	o := e.order(5_000)
	addr := e.addr(o)
	a := e.node.Pay(addr, 5_000)
	b := e.node.Pay(addr, 100)
	e.passes(1)
	if !e.rail(o).Paid {
		t.Fatalf("setup: %+v", e.rail(o))
	}
	e.node.Replace(a, false) // the wallet sees this one
	e.node.HoldWallet()
	b2 := e.node.Replace(b, true) // and lags on this one
	e.passes(1)
	if m, _ := e.mark(a); m.Kind != store.MarkDead || !e.rail(o).Paid || len(e.events(o)) != 1 {
		t.Fatalf("the first pass: A %+v, Rail %+v, events %+v", m, e.rail(o), e.events(o))
	}
	e.passes(1)
	if r, ok := e.rev(a, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || e.rail(o).Paid {
		t.Fatalf("the second pass: A's reversal %+v %v, events %+v, Rail %+v\n%s", r, ok, e.events(o), e.rail(o), e.log)
	}
	e.passes(4)
	if m, _ := e.mark(b); m.Kind != store.MarkMempool || e.count(logWalletBehind) != 6 {
		t.Fatalf("B %+v, %d WARN lines", m, e.count(logWalletBehind))
	}
	if _, ok := e.rev(b, 0); ok || e.eventCount(o, store.EventReverted) != 1 {
		t.Fatalf("B's record is reversed while it is behind: %+v", e.events(o))
	}
	e.node.ReleaseWallet()
	e.passes(3)
	if f, ok := e.fwd(b2, 0); !ok || f.Code != codePaid {
		t.Fatalf("B's replacement: %+v %v", f, ok)
	}
	if r, ok := e.rev(b, 0); !ok || r.UserData64 != 0 || e.rail(o).Paid || len(e.events(o)) != 2 || e.count(alertPaidAgain) != 0 {
		t.Fatalf("after the release: B's reversal %+v %v, events %+v", r, ok, e.events(o))
	}
	e.safety(o)
}

// An output with a mempool mark and no record (a crash after its mark) that is behind, in an order that another
// payment touches, is not recorded from the mark the node contradicts, and sends no order.paid. After the catch-up the
// outcome is the non-lagging one: it is dead and never recorded. (With the output counted from its mark, settle recorded
// it and paid the order.)
func TestBehindAnUnrecordedOutputCountsZero(t *testing.T) {
	e := newZeroConfEnv(t)
	o := e.order(5_000)
	p := e.payOrder(o, 5_000)
	e.svc.hook = func(point string) error {
		if point == "track:after-mark" {
			return errCrash
		}
		return nil
	}
	if err := e.pass(); !errors.Is(err, errCrash) {
		t.Fatalf("no crash: %v", err)
	}
	if m, _ := e.mark(p); m.Kind != store.MarkMempool {
		t.Fatalf("setup: %+v", m)
	}
	if _, ok := e.fwd(p, 0); ok {
		t.Fatal("setup: recorded")
	}
	e.restart()
	e.payOrder(o, 100) // a second payment touches the order
	e.node.HoldWallet()
	e.node.Replace(p, false)
	e.passes(3)
	if _, ok := e.fwd(p, 0); ok || e.eventCount(o, store.EventPaid) != 0 || e.rail(o).Paid || e.count(logWalletBehind) != 3 {
		t.Fatalf("while behind: recorded %v, events %+v, Rail %+v\n%s", ok, e.events(o), e.rail(o), e.log)
	}
	e.node.ReleaseWallet()
	e.passes(3)
	if m, _ := e.mark(p); m.Kind != store.MarkDead {
		t.Fatalf("after the catch-up: %+v", m)
	}
	if _, ok := e.fwd(p, 0); ok || e.eventCount(o, store.EventPaid) != 0 || e.eventCount(o, store.EventReverted) != 0 {
		t.Fatalf("after the catch-up: recorded %v, events %+v", ok, e.events(o))
	}
	e.safety(o)
}

// A behind output that IS recorded stands for the pass, in an order that gets a second, live payment in the same pass:
// the second is recorded, the first's record is not reversed, and the order is paid by the two.
func TestBehindARecordedOutputStandsBesideANewPayment(t *testing.T) {
	e := newZeroConfEnv(t)
	o := e.order(5_000)
	addr := e.addr(o)
	p := e.node.Pay(addr, 2_000)
	e.passes(1)
	if _, ok := e.fwd(p, 0); !ok {
		t.Fatal("setup: not recorded")
	}
	q := e.node.Pay(addr, 3_000) // known to the wallet before it lags
	e.node.HoldWallet()
	p2 := e.node.Replace(p, true)
	e.passes(1)
	if _, ok := e.fwd(q, 0); !ok {
		t.Fatalf("the new payment is not recorded\n%s", e.log)
	}
	if _, ok := e.rev(p, 0); ok || e.count(logWalletBehind) != 1 {
		t.Fatalf("the behind record was reversed, or no WARN\n%s", e.log)
	}
	if !e.rail(o).Paid || len(e.events(o)) != 1 || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("Rail %+v, events %+v", e.rail(o), e.events(o))
	}
	e.passes(2)
	e.node.ReleaseWallet()
	e.passes(3)
	if f, ok := e.fwd(p2, 0); !ok || f.Code != codePaid {
		t.Fatalf("the replacement: %+v %v", f, ok)
	}
	if r, ok := e.rev(p, 0); !ok || r.UserData64 != 0 || !e.rail(o).Paid || len(e.events(o)) != 1 {
		t.Fatalf("after the release: %+v %v, events %+v", r, ok, e.events(o))
	}
	e.safety(o)
}

// The final event of an order past its expiry whose only output is behind: none, and no logNoFinal, while it is behind;
// the event comes after the catch-up (here the payment was diverted: order.expired).
func TestBehindOnlyOutputPastTheExpiryHoldsTheFinalEvent(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	e.node.HoldWallet()
	e.node.Replace(a, false)
	e.pastExpiry(o)
	e.passes(4)
	if len(e.events(o)) != 0 || e.count(logNoFinal) != 0 || e.count(logWalletBehind) != 4 {
		t.Fatalf("while behind: events %+v\n%s", e.events(o), e.log)
	}
	e.node.ReleaseWallet()
	e.passes(4)
	if len(e.events(o)) != 1 || e.eventCount(o, store.EventExpired) != 1 || e.count(logNoFinal) != 0 {
		t.Fatalf("after the catch-up: events %+v\n%s", e.events(o), e.log)
	}
	e.safety(o)
}

// --- more per-output cases ---

// The views of Rail (the API, the checkout page, the status goroutines) must not read behindInPass: the pass
// replaces and writes it. Rail runs in a loop while passes leave an output behind: under -race this is a data race
// (and Go can abort with "concurrent map read and map write") when a reader touches the map.
func TestBehindRailReadersDoNotTouchThePassMap(t *testing.T) {
	e := newZeroConfEnv(t)
	o, old := e.zeroConfPaid()
	e.node.HoldWallet()
	e.node.Replace(old, true)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := e.svc.Rail(bg, o); err != nil {
					t.Error(err)
					return
				}
			}
		}
	}()
	e.passes(30)
	close(stop)
	<-done
	if e.count(logWalletBehind) != 30 {
		t.Fatalf("%d WARN lines", e.count(logWalletBehind))
	}
}

// An output whose current mark is mined (its block was reorged out) and that a mempool double spend has replaced, with
// the wallet behind on the conflict, is not held as behind: the wallet, synced to the tip, has just said it is not mined,
// so the old path applies. On a tier that needs a block the record is reversed in that pass.
func TestBehindReorgedOutAndDoubleSpentIsReversedAtOnce(t *testing.T) {
	e := newWatchEnv(t) // 100,000 sat: 1 confirmation
	o := e.order(100_000)
	p := e.payOrder(o, 100_000)
	e.node.Mine(1)
	e.passes(1)
	if m, _ := e.mark(p); m.Kind != store.MarkMined || !e.rail(o).Paid {
		t.Fatalf("setup: %+v %+v", m, e.rail(o))
	}
	e.node.Invalidate(1)
	e.node.HoldWallet()
	e.node.Replace(p, false)
	e.passes(1)
	if m, _ := e.mark(p); m.Kind != store.MarkDead || e.count(logWalletBehind) != 0 {
		t.Fatalf("mark %+v, %d WARN lines\n%s", m, e.count(logWalletBehind), e.log)
	}
	if r, ok := e.rev(p, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || e.rail(o).Paid {
		t.Fatalf("not reversed in that pass: %+v %v, events %+v", r, ok, e.events(o))
	}
	e.safety(o)
}

// 0 tier: the same sequence is dead with the usual one-pass hold, and reversed in the pass after.
func TestBehindReorgedOutAndDoubleSpentAtTheZeroTier(t *testing.T) {
	e := newZeroConfEnv(t)
	o, p := e.zeroConfPaid()
	e.node.Mine(1)
	e.passes(1)
	if m, _ := e.mark(p); m.Kind != store.MarkMined {
		t.Fatalf("setup: %+v", m)
	}
	e.node.Invalidate(1)
	e.node.HoldWallet()
	e.node.Replace(p, false)
	e.passes(1)
	if m, _ := e.mark(p); m.Kind != store.MarkDead || e.count(logWalletBehind) != 0 {
		t.Fatalf("mark %+v, %d WARN lines\n%s", m, e.count(logWalletBehind), e.log)
	}
	if _, ok := e.rev(p, 0); ok || len(e.events(o)) != 1 {
		t.Fatalf("the held pass: reversed %v, events %+v", ok, e.events(o))
	}
	e.passes(1)
	if r, ok := e.rev(p, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("the next pass: %+v %v, events %+v", r, ok, e.events(o))
	}
	e.safety(o)
}

// A late payment (first seen at or after the expiry) that is behind does not hold the order's final event, and does
// not hide the "no final event" ALERT: it never counts toward paid, so its replacement cannot change the event.
func TestBehindALatePaymentDoesNotHoldTheFinalEvent(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	addr := e.addr(o)
	a := e.node.Pay(addr, 100_000)
	e.pass()
	e.node.Replace(a, false) // the on-time payment is diverted (the wallet sees it)
	e.pastExpiry(o)
	late := e.node.Pay(addr, 1_000)
	e.passes(1)
	if x := e.txo(late); !x.FirstSeen.After(o.ExpiresAt) && !x.FirstSeen.Equal(o.ExpiresAt) {
		t.Fatalf("setup: not late: %v", x.FirstSeen)
	}
	e.node.HoldWallet()
	e.node.Replace(late, true)
	e.passes(4)
	if e.count(logWalletBehind) == 0 || e.eventCount(o, store.EventExpired) != 1 || len(e.events(o)) != 1 {
		t.Fatalf("events %+v, %d WARN lines\n%s", e.events(o), e.count(logWalletBehind), e.log)
	}
	e.safety(o)
}

// The heal step (rail.HealSeqs) sees the views before the behind outputs are removed: an output that was reversed
// (seq 1) with its order.reverted lost by a crash, and is then back in the mempool unrecorded and behind, gets its
// event healed in that pass, so the seq is not reused.
func TestBehindTheHealStepSeesTheRemovedOutput(t *testing.T) {
	e := newZeroConfEnv(t)
	o, p := e.zeroConfPaid()
	e.node.Evict(p)
	e.passes(1) // dead, held
	crashed := false
	e.svc.hook = func(point string) error {
		if point == "settle:after-transfers" && !crashed {
			crashed = true
			return errCrash
		}
		return nil
	}
	e.clk.Advance(time.Second)
	if err := e.pass(); !errors.Is(err, errCrash) {
		t.Fatalf("no crash: %v", err)
	}
	if _, ok := e.rev(p, 0); !ok || e.eventCount(o, store.EventReverted) != 0 {
		t.Fatalf("setup: reversed %v, events %+v", ok, e.events(o))
	}
	e.restart()
	e.node.Rebroadcast(p)
	e.svc.hook = func(point string) error {
		if point == "track:after-mark" {
			return errCrash
		}
		return nil
	}
	e.clk.Advance(time.Second)
	if err := e.pass(); !errors.Is(err, errCrash) {
		t.Fatalf("no crash: %v", err)
	}
	if m, _ := e.mark(p); m.Kind != store.MarkMempool {
		t.Fatalf("setup: %+v", m)
	}
	e.restart()
	e.payOrder(o, 100) // touches the order
	e.node.HoldWallet()
	e.node.Replace(p, false)
	e.passes(1)
	if e.count(logWalletBehind) != 1 || !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("the lost event was not healed: events %+v\n%s", e.events(o), e.log)
	}
}
