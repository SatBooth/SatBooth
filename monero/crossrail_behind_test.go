package monero

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/descriptor"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

// wirePair wires a real on-chain rail (default tiers: 1 confirmation up to its first bound) to the env's Monero rail
// as service.Run does, over a new test bitcoind.
func wirePair(t *testing.T, e *env) (*onchain.Service, *onchaintest.Node) {
	t.Helper()
	desc, err := descriptor.Parse("wpkh(" + verifyTpub + "/0/*)")
	if err != nil {
		t.Fatal(err)
	}
	node := onchaintest.New(onchaintest.Normalise(desc.Canonical), 7)
	return wireOn(t, e, node), node
}

// wireOn is a new on-chain rail over node, wired to the env's current Monero rail (also after a restart of both).
func wireOn(t *testing.T, e *env, node *onchaintest.Node) *onchain.Service {
	t.Helper()
	desc, err := descriptor.Parse("wpkh(" + verifyTpub + "/0/*)")
	if err != nil {
		t.Fatal(err)
	}
	oc, err := onchain.New(onchain.Config{Descriptor: desc, Network: "regtest", PollInterval: onchain.MinPollInterval}, e.st, e.lg, node, e.ln, e.clk.Now,
		slog.New(slog.NewTextHandler(e.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := oc.Check(bg); err != nil {
		t.Fatalf("onchain Check: %v", err)
	}
	mu := e.svc.settleLock()
	e.svc.SetPeer(oc, mu)
	oc.SetPeer(e.svc, mu)
	return oc
}

// ocFreshPass runs one pass of the on-chain rail with the clock where it is, so that the Monero rail, which just ran,
// is fresh to it: Run's second pass starts after PollInterval (2 s, real time), which is when the first has ended.
func ocFreshPass(t *testing.T, e *env, node *onchaintest.Node, oc *onchain.Service) {
	t.Helper()
	before := node.Calls("getblockchaininfo")
	ctx, cancel := context.WithCancel(bg)
	fin := make(chan struct{})
	go func() { oc.Run(ctx); close(fin) }()
	deadline := time.Now().Add(15 * time.Second)
	for node.Calls("getblockchaininfo") < before+2 {
		if time.Now().After(deadline) {
			cancel()
			<-fin
			t.Fatalf("ocFreshPass: no second pass\n%s", e.log)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-fin
}

// ocRunUntil runs the on-chain rail's Run, with the clock where it is (the Monero rail is fresh to it), until done
// reports true.
func ocRunUntil(t *testing.T, e *env, oc *onchain.Service, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	fin := make(chan struct{})
	go func() { oc.Run(ctx); close(fin) }()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			cancel()
			<-fin
			t.Fatalf("ocRunUntil: not reached\n%s", e.log)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-fin
}

// logged counts the lines of both rails' log that contain text.
func logged(e *env, text string) int { return strings.Count(e.log.String(), text) }

// A mixed order has an on-time on-chain payment that is only in the mempool (a tier of 1 confirmation: not
// recorded) and that the node has replaced by a payment to someone else, while bitcoind's wallet lags. The on-chain
// rail holds its side; the Monero rail must too: it reads the on-chain rail's stale mempool mark, a live partial sum, and
// writes order.underpaid at the expiry. When the wallet has caught up the payment is gone and the order is expired.
func TestCrossRailBehindOnchainOutputHoldsTheMoneroFinalEvent(t *testing.T) {
	for _, partial := range []bool{true, false} {
		name := "partial"
		if !partial {
			name = "whole"
		}
		t.Run(name, func(t *testing.T) {
			e := newWatchEnv(t, nil)
			o := e.mixed(0, 100_000, xmr)
			oc, node := wirePair(t, e)
			sat := int64(100_000)
			if partial {
				sat = 40_000
			}
			a, err := oc.Address(bg, o.ID)
			if err != nil {
				t.Fatal(err)
			}
			p := node.Pay(a.Address, sat)
			ocPass(t, e, oc, o) // the payment is a mempool mark in the store; tier 1, so it is not recorded
			e.pastExpiry(o)
			node.HoldWallet()
			node.Replace(p, false) // the node has dropped it; the wallet shows neither a conflict nor a replacement
			ocPassStep(t, e, oc, o, 10*time.Second)
			if logged(e, "has not yet processed") == 0 {
				t.Fatalf("setup: the on-chain rail is not behind\n%s", e.log)
			}
			ocPassStep(t, e, oc, o, 10*time.Second) // the order is past its expiry; the on-chain rail holds its event
			for i := 0; i < 3; i++ {
				e.passes(1)
			}
			if n := len(e.events(o)); n != 0 || logged(e, "without a final event") != 0 {
				t.Fatalf("while behind: events %+v\n%s", e.events(o), e.log)
			}
			node.ReleaseWallet()
			ocPass(t, e, oc, o)
			ocPass(t, e, oc, o)
			e.passes(2)
			if !e.hasEvent(o, store.EventExpired, 1) || len(e.events(o)) != 1 || logged(e, "without a final event") != 0 {
				t.Fatalf("after the catch-up: events %+v\n%s", e.events(o), e.log)
			}
			e.safety(o)
		})
	}
}

// lagAcrossTheWindowEnd leaves the order past the Monero window end with its on-chain output (40 000 sat, replaced by
// a payment to someone else) behind the wallet's lag. The on-chain rail passes between the Monero rail's passes, so
// each sees the other fresh.
func lagAcrossTheWindowEnd(t *testing.T, e *env) (store.Order, *onchain.Service, *onchaintest.Node) {
	t.Helper()
	o := e.mixed(0, 100_000, xmr)
	oc, node := wirePair(t, e)
	a, err := oc.Address(bg, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := node.Pay(a.Address, 40_000)
	ocPass(t, e, oc, o)
	e.pastExpiry(o)
	node.HoldWallet()
	node.Replace(p, false)
	ocPassStep(t, e, oc, o, 10*time.Second)
	e.passes(1)
	// A week passes in one step, far beyond behindGrace: the process restarts (both rails), which starts the wait again,
	// as a lag that really spans the window end would find the order.
	e.toWindowEnd(o)
	e.restart()
	oc = wireOn(t, e, node)
	ocPassStep(t, e, oc, o, 10*time.Second)
	e.passes(2)
	return o, oc, node
}

const (
	monNoFinal = "a Monero order left its watch window without a final event"
	ocNoFinal  = "an on-chain order left its watch window"
)

// catchUp releases the wallet and lets both rails run until the order has its final event: the on-chain pass marks the
// output dead (and holds the event once), the Monero rail carries the order meanwhile, and the next on-chain pass, with
// a fresh Monero rail, writes the event.
func catchUp(t *testing.T, e *env, o store.Order, oc *onchain.Service, node *onchaintest.Node) {
	t.Helper()
	node.ReleaseWallet()
	e.passes(1)
	ocFreshPass(t, e, node, oc) // the output dies: the event is held once more
	e.passes(1)
	ocRunUntil(t, e, oc, func() bool { return len(e.events(o)) > 0 })
	e.passes(1)
}

// The lag spans the end of the Monero rail's watch window. Neither rail writes a final event while the on-chain output
// is behind, and the Monero rail logs no "left its watch window" ALERT for it: it carries the order (leftHeld) while its
// fresh peer holds the event. After the catch-up the final event is the one without the lag.
func TestCrossRailBehindAcrossTheWindowEnd(t *testing.T) {
	e := newWatchEnv(t, nil)
	o, oc, node := lagAcrossTheWindowEnd(t, e)
	if len(e.events(o)) != 0 || logged(e, ocNoFinal) != 0 || logged(e, monNoFinal) != 0 {
		t.Fatalf("while behind: events %+v\n%s", e.events(o), e.log)
	}
	catchUp(t, e, o, oc, node)
	if !e.hasEvent(o, store.EventExpired, 1) || len(e.events(o)) != 1 || logged(e, monNoFinal) != 0 || logged(e, ocNoFinal) != 0 {
		t.Fatalf("after the catch-up: events %+v\n%s", e.events(o), e.log)
	}
}

// The ALERT is not lost: an order held at its window end that still has no final event once the hold has ended is
// reported once. (The on-chain rail is left holding for a stale Monero peer, so no event is written.)
func TestCrossRailBehindHoldEndsWithoutAFinalEventIsReported(t *testing.T) {
	e := newWatchEnv(t, nil)
	o, oc, node := lagAcrossTheWindowEnd(t, e)
	node.ReleaseWallet()
	e.passes(1)
	ocFreshPass(t, e, node, oc) // the output died: the event is held once more
	e.passes(1)                 // carried
	ocPass(t, e, oc, o)         // nothing is held now, and the event waits only for the Monero rail, stale to this pass
	e.passes(3)
	if len(e.events(o)) != 0 || logged(e, monNoFinal) != 1 {
		t.Fatalf("events %+v, %d ALERT lines\n%s", e.events(o), logged(e, monNoFinal), e.log)
	}
}

// The hold expires with the on-chain rail's freshness: no on-chain pass completes (bitcoind is down) for longer
// than its stale bound while the Monero window ends, and the Monero rail reports the order, once, as for any stale peer.
func TestCrossRailBehindHeldFactExpiresWithTheOnchainRail(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.mixed(0, 100_000, xmr)
	oc, node := wirePair(t, e)
	a, err := oc.Address(bg, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := node.Pay(a.Address, 40_000)
	ocPass(t, e, oc, o)
	node.HoldWallet()
	node.Replace(p, false)
	e.pastExpiry(o)
	ocPass(t, e, oc, o) // behind: the order is held; the snapshot says so
	e.passes(1)
	if v, _, err := oc.View(bg, o); err != nil || !v.Held || v.Stale {
		t.Fatalf("setup: the fresh rail's view %+v %v", v, err)
	}
	e.toWindowEnd(o) // no on-chain pass completes from here on
	e.passes(3)
	if v, _, err := oc.View(bg, o); err != nil || v.Held || !v.Stale {
		t.Fatalf("the stale rail's view %+v %v", v, err)
	}
	if n := logged(e, monNoFinal); n != 1 || len(e.events(o)) != 0 {
		t.Fatalf("%d ALERT lines, events %+v\n%s", n, e.events(o), e.log)
	}
}

// A restart of the whole process (both rails, as service.Run starts them) while an order is held across its window
// end: the first Monero pass runs before the new on-chain rail's first pass, reads it stale and reports the order once;
// the final event still follows the catch-up, and nothing is reported twice.
func TestCrossRailBehindAcrossAWholeRestart(t *testing.T) {
	e := newWatchEnv(t, nil)
	o, oc, node := lagAcrossTheWindowEnd(t, e)
	e.restart()
	oc = wireOn(t, e, node)
	e.passes(1)
	if n := logged(e, monNoFinal); n != 1 || len(e.events(o)) != 0 {
		t.Fatalf("the first Monero pass after the restart: %d ALERT lines, events %+v\n%s", n, e.events(o), e.log)
	}
	ocPass(t, e, oc, o) // the new on-chain rail's first pass: behind again
	e.passes(2)
	catchUp(t, e, o, oc, node)
	if !e.hasEvent(o, store.EventExpired, 1) || len(e.events(o)) != 1 || logged(e, monNoFinal) != 1 {
		t.Fatalf("after the catch-up: events %+v\n%s", e.events(o), e.log)
	}
}

// An on-chain output first found behind before the expiry, the expiry crossed during the lag (no restart): neither rail
// writes a final event while it is behind, and after the catch-up the order is expired.
func TestCrossRailBehindExpiryCrossedDuringTheLag(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.mixed(0, 100_000, xmr)
	oc, node := wirePair(t, e)
	a, err := oc.Address(bg, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := node.Pay(a.Address, 40_000)
	ocPass(t, e, oc, o)
	e.clk.Advance(o.ExpiresAt.Sub(e.clk.Now()) - 15*time.Second)
	node.HoldWallet()
	node.Replace(p, false)
	ocPassStep(t, e, oc, o, 10*time.Second) // first found behind, 5 s before the expiry
	e.passes(1)
	ocPassStep(t, e, oc, o, 10*time.Second) // the expiry is crossed
	e.passes(3)
	if e.clk.Now().Before(o.ExpiresAt) || len(e.events(o)) != 0 || logged(e, monNoFinal) != 0 || logged(e, ocNoFinal) != 0 {
		t.Fatalf("while behind: events %+v\n%s", e.events(o), e.log)
	}
	node.ReleaseWallet()
	ocPass(t, e, oc, o)
	ocPass(t, e, oc, o)
	e.passes(2)
	if !e.hasEvent(o, store.EventExpired, 1) || len(e.events(o)) != 1 || logged(e, monNoFinal) != 0 {
		t.Fatalf("after the catch-up: events %+v\n%s", e.events(o), e.log)
	}
}

// The lag starts 5 s before the Monero window's end and crosses it within the 60 s, in one process (no restart): the
// Monero rail, with its leftSince set, carries the order across the window end (no ALERT) and the final event follows
// the catch-up.
func TestCrossRailBehindAcrossTheWindowEndInProcess(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.mixed(0, 100_000, xmr)
	oc, node := wirePair(t, e)
	a, err := oc.Address(bg, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := node.Pay(a.Address, 40_000)
	ocPass(t, e, oc, o)
	e.pastExpiry(o)
	e.passes(1) // the Monero rail has run in this process before the window ends
	e.toWindowEnd(o)
	e.clk.Advance(-15 * time.Second)
	node.HoldWallet()
	node.Replace(p, false)
	ocPassStep(t, e, oc, o, 10*time.Second) // first found behind, 5 s before the window's end
	e.passes(1)
	ocPassStep(t, e, oc, o, 10*time.Second) // the window's end is crossed
	e.passes(3)
	if e.clk.Now().Before(o.ExpiresAt.Add(store.WatchWindow)) || len(e.events(o)) != 0 || logged(e, monNoFinal) != 0 ||
		logged(e, ocNoFinal) != 0 {
		t.Fatalf("while behind: events %+v\n%s", e.events(o), e.log)
	}
	catchUp(t, e, o, oc, node)
	if !e.hasEvent(o, store.EventExpired, 1) || len(e.events(o)) != 1 || logged(e, monNoFinal) != 0 || logged(e, ocNoFinal) != 0 {
		t.Fatalf("after the catch-up: events %+v\n%s", e.events(o), e.log)
	}
}
