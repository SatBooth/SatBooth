package onchain

import (
	"fmt"
	"testing"

	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

// --- bumps inside the wallet's lag: what is held and what is not ---

// twoBumps is the setup where B pays the order (0 tier, recorded), is bumped to B2 (the wallet sees it: B is
// dead, B2 is listed and inherits B's first-seen), and B2 is bumped to B3 while the wallet lags (B2 is behind).
func (e *env) twoBumps() (o store.Order, b, b2, b3 string) {
	e.t.Helper()
	o, b = e.zeroConfPaid()
	b2 = e.node.Replace(b, true)
	e.node.HoldWallet()
	b3 = e.node.Replace(b2, true)
	return o, b, b2, b3
}

// Two bumps inside the wallet's lag, the documented outcome (docs/rules-onchain.md): the first payment's reversal is
// held one pass (diedNow) and then goes ahead while the second bump is still behind: order.reverted; when the wallet
// has caught up the last replacement is recorded and order.paid_again follows. A false event, never a false paid. There
// is no longer hold: a payer could renew it (TestBehindARenewingChainOfReplacementsDoesNotKeepAGonePaymentPaid).
func TestBehindTwoBumpsReadRevertedThenPaidAgain(t *testing.T) {
	e := newZeroConfEnv(t)
	o, b, _, b3 := e.twoBumps()
	e.passes(1)
	if _, ok := e.rev(b, 0); ok || len(e.events(o)) != 1 || !e.rail(o).Paid {
		t.Fatalf("the first pass: B reversed %v, events %+v", ok, e.events(o))
	}
	e.passes(2)
	if r, ok := e.rev(b, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || e.rail(o).Paid {
		t.Fatalf("B's reversal %+v %v, events %+v, Rail %+v\n%s", r, ok, e.events(o), e.rail(o), e.log)
	}
	e.node.ReleaseWallet()
	e.passes(4)
	if f, ok := e.fwd(b3, 0); !ok || f.Code != codePaid || !e.rail(o).Paid || e.eventCount(o, store.EventReverted) != 1 ||
		!e.hasEvent(o, store.EventPaidAgain, 1) || len(e.events(o)) != 3 {
		t.Fatalf("after the catch-up: %+v %v, events %+v, Rail %+v\n%s", f, ok, e.events(o), e.rail(o), e.log)
	}
	e.safety(o)
}

// A payer who double-spends a paid 0-tier payment A and then, before every pass, replaces the last output with a
// new one that pays A's address and amount and again replaces that by a payment to itself keeps nothing paid: A is
// reversed in the first pass after its dead mark, whatever stands in its place, and stays so for the whole chain
// (only the pass that marks an output dead holds its reversal, diedNow).
func TestBehindARenewingChainOfReplacementsDoesNotKeepAGonePaymentPaid(t *testing.T) {
	for _, lag := range []bool{false, true} {
		t.Run(fmt.Sprintf("walletBehind=%v", lag), func(t *testing.T) {
			e := newZeroConfEnv(t)
			o, a := e.zeroConfPaid()
			addr, first := e.txo(a).Address, e.txo(a).FirstSeen
			x := e.node.Replace(a, false) // the payer double-spends A to itself
			e.passes(1)
			if m, _ := e.mark(a); m.Kind != store.MarkDead {
				t.Fatalf("A %+v", m)
			}
			for k := 1; k <= 6; k++ {
				r := e.node.ReplaceWith(x, []onchaintest.Out{{Address: addr, Sat: 5_000}}) // A's input, the order's address, A's amount
				if lag {
					e.node.HoldWallet() // the wallet has r and lags on its replacement
				}
				x = e.node.Replace(r, false)
				e.passes(1)
				if lag {
					e.node.ReleaseWallet()
				}
				// The attack's precondition: each r stands in A's place (it inherited A's first-seen).
				if y := e.txo(r); !y.FirstSeen.Equal(first) {
					t.Fatalf("replacement %d: first-seen %v, A's %v: the chain no longer stands in A's place", k, y.FirstSeen, first)
				}
				if k == 1 {
					if rv, ok := e.rev(a, 0); !ok || rv.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || e.rail(o).Paid {
						t.Fatalf("pass 1 of the chain: A's reversal %+v %v, events %+v\n%s", rv, ok, e.events(o), e.log)
					}
				}
			}
			if e.eventCount(o, store.EventReverted) != 1 || e.rail(o).Paid {
				t.Fatalf("events %+v", e.events(o))
			}
			e.passes(3)
			e.safety(o)
		})
	}
}

// A non-0 tier, two bumps, the second behind: the final event is held (the older rule, per output) while the bump is behind, past the
// expiry, and the end state is the one without the lag.
func TestBehindTwoBumpsNonZeroTierFinalEventHeld(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	first := e.txo(a).FirstSeen
	a2 := e.node.Replace(a, true)
	e.passes(1)
	e.node.HoldWallet()
	a3 := e.node.Replace(a2, true)
	e.pastExpiry(o)
	e.passes(4)
	if len(e.events(o)) != 0 || e.count(logNoFinal) != 0 {
		t.Fatalf("while held: events %+v\n%s", e.events(o), e.log)
	}
	e.node.ReleaseWallet()
	e.passes(2)
	if x := e.txo(a3); !x.FirstSeen.Equal(first) {
		t.Fatalf("the last bump's first-seen %v, the original's %v", x.FirstSeen, first)
	}
	if len(e.events(o)) != 0 {
		t.Fatalf("a final event while the bump is being paid: %+v", e.events(o))
	}
	e.node.Mine(1)
	e.passes(3)
	if f, ok := e.fwd(a3, 0); !ok || f.Code != codePaid || !e.rail(o).Paid || len(e.events(o)) != 1 || e.eventCount(o, store.EventPaid) != 1 ||
		e.count(logNoFinal) != 0 || e.count(logUnderpaid) != 0 {
		t.Fatalf("mined: %+v %v %+v\n%s", f, ok, e.events(o), e.log)
	}
	e.safety(o)
}

// The peer's view (rail.Peer: View) reads held while an on-time output of the order is behind, and only the peer's:
// Rail, which the API shows, does not read stale for it. It does not for a paid order, for a late output, or once the wallet has caught up.
func TestBehindPeerViewReadsStaleOnlyForAnOnTimeOutput(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	peerStale := func() bool {
		v, ok, err := e.svc.View(bg, o)
		if err != nil || !ok {
			t.Fatalf("View: %v %v", ok, err)
		}
		if v.Stale {
			t.Fatal("the peer's view reads stale, not held")
		}
		return v.Held
	}
	if peerStale() {
		t.Fatal("stale before the lag")
	}
	e.node.HoldWallet()
	b := e.node.Replace(a, false)
	e.passes(1)
	if !peerStale() || e.rail(o).Stale {
		t.Fatalf("behind: peer %v, Rail %v", peerStale(), e.rail(o).Stale)
	}
	e.node.ReleaseWallet()
	e.passes(1)
	if peerStale() {
		t.Fatalf("after the catch-up (B %s)", b)
	}
	e.safety(o)
}

// A late output (first seen after the expiry) that is behind does not make the peer's view stale: it never counts.
func TestBehindPeerViewIgnoresALateOutput(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	addr := e.addr(o)
	e.pastExpiry(o)
	a := e.node.Pay(addr, 100_000)
	e.pass()
	e.node.HoldWallet()
	e.node.Replace(a, false)
	e.passes(2)
	if e.count(logWalletBehind) == 0 {
		t.Fatalf("setup: not behind\n%s", e.log)
	}
	if v, ok, err := e.svc.View(bg, o); err != nil || !ok || v.Stale || v.Held {
		t.Fatalf("View: %+v %v %v", v, ok, err)
	}
}

// An output that dies in a pass does not hold another dead output's reversal: A (5 000) is dead since the last pass, a
// tiny payment T dies in this one, and A is reversed in this pass, on its schedule (only the dead output's own pass
// holds, diedNow).
func TestBehindAnotherOutputDyingInThePassHoldsNothing(t *testing.T) {
	e := newZeroConfEnv(t)
	o := e.order(5_000)
	addr := e.addr(o)
	a := e.node.Pay(addr, 5_000)
	tiny := e.node.Pay(addr, 100)
	e.passes(1)
	if !e.rail(o).Paid {
		t.Fatalf("setup: %+v", e.rail(o))
	}
	e.node.Replace(a, false)
	e.passes(1) // A is dead, its reversal held for this pass
	if m, _ := e.mark(a); m.Kind != store.MarkDead || e.rail(o).Paid == false {
		t.Fatalf("setup: A %+v, Rail %+v", m, e.rail(o))
	}
	e.node.Replace(tiny, false)
	e.passes(1) // T dies in this pass; A is reversed in it
	if r, ok := e.rev(a, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("A's reversal %+v %v, events %+v\n%s", r, ok, e.events(o), e.log)
	}
	e.passes(2) // T's own reversal follows
	e.safety(o)
}
