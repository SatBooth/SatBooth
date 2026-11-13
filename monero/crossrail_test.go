package monero

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/satbooth/satbooth/descriptor"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

// verifyTpub is onchain's test tpub: BIP-32 test vector 1's master public key with the testnet
// version bytes.
const verifyTpub = "tpubD6NzVbkrYhZ4XgiXtGrdW5XDAPFCL9h7we1vwNCpn8tGbBcgfVYjXyhWo4E1xkh56hjod1RhGjxbaTLV3X4FyWuejifB9jusQ46QzG87VKp"

// ocPass runs one pass of the real on-chain rail through its exported Run: Run passes at once, and the pass's end is
// seen as Rail's Stale turning false (lastGoodPass stamped at the end of a good pass; Rail, not View: the peer's View
// also reads held while an on-time output is behind the wallet's lag). The fake clock is moved past the stale bound
// first, so the previous pass's stamp reads stale: the Monero rail's next pass (a second later) sees a fresh peer.
func ocPass(t *testing.T, e *env, oc *onchain.Service, o store.Order) {
	t.Helper()
	ocPassStep(t, e, oc, o, 2*time.Minute)
}

// ocPassStep is ocPass moving the clock by step, which must pass the on-chain rail's stale bound (3 poll intervals):
// the lag tests step 10 s over a rail wired with the minimum poll interval, since a wallet behind its node is waited
// for only behindGrace (60 s) after the rail first found the payment replaced.
func ocPassStep(t *testing.T, e *env, oc *onchain.Service, o store.Order, step time.Duration) {
	t.Helper()
	e.clk.Advance(step)
	if v, err := oc.Rail(bg, o); err != nil || !v.Stale {
		t.Fatalf("ocPass: not stale before the pass: %+v %v", v, err)
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { oc.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, err := oc.Rail(bg, o)
		if err == nil && !v.Stale {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("ocPass: the on-chain pass did not end well\n%s", e.log)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

// Ruling R2: Monero writes a reversal stamped 1 and crashes before order.reverted(1).
// After the restart wallet-rpc fails, so Monero's settle (the only place its stamps are healed) does not run; the
// on-chain rail settles first. Correct history: paid, reverted(1) [Monero], paid_again(1) [bitcoin], reverted(2)
// [bitcoin]. This test asserts that history.
func TestCrossRailCrashKeepsReversalCycles(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.mixed(0, 100_000, xmr) // on-chain: 1 conf (default tiers); Monero: 1 conf

	// Monero pays and is recorded: order.paid.
	xtx := e.payOrder(o, xmr)
	e.w.Mine(1)
	if err := e.pass(); err != nil {
		t.Fatalf("monero pass: %v", err)
	}
	if !e.hasEvent(o, store.EventPaid, 1) {
		t.Fatalf("setup: no order.paid: %+v", e.events(o))
	}

	// The Monero payment is reorged out; settle writes the reversal (stamp 1) and crashes before order.reverted(1).
	e.w.Pop(1)
	fired := e.crashAt("settle:after-transfers")
	if err := e.pass(); !isCrash(err) || !*fired {
		t.Fatalf("expected the crash at settle:after-transfers, got %v", err)
	}
	xrev, ok := e.rev(o, xtx, 0)
	if !ok || xrev.UserData64 != 1 {
		t.Fatalf("setup: Monero reversal %+v %v", xrev, ok)
	}
	if e.eventCount(o, store.EventReverted) != 0 {
		t.Fatalf("setup: order.reverted was written: %+v", e.events(o))
	}

	// Restart: both rails, one settle mutex, as service.Run wires them (no heal runs before the watchers).
	e.restart()
	desc, err := descriptor.Parse("wpkh(" + verifyTpub + "/0/*)")
	if err != nil {
		t.Fatal(err)
	}
	node := onchaintest.New(onchaintest.Normalise(desc.Canonical), 7)
	oc, err := onchain.New(onchain.Config{Descriptor: desc, Network: "regtest"}, e.st, e.lg, node, e.ln, e.clk.Now,
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

	// wallet-rpc fails: Monero's pass ends at get_height, before any settle, so its reversal stays unhealed.
	e.w.Fail("get_height", errOutage)
	if err := e.pass(); err == nil {
		t.Fatal("the Monero pass ran to its end")
	}
	if e.eventCount(o, store.EventReverted) != 0 {
		t.Fatalf("Monero healed without a good pass: %+v", e.events(o))
	}

	// Bitcoin pays the whole order and confirms; the on-chain rail settles.
	a, err := oc.Address(bg, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	node.Pay(a.Address, 100_000)
	node.Mine(1)
	ocPass(t, e, oc, o)
	t.Logf("events after the bitcoin payment: %+v", e.events(o))
	if !e.hasEvent(o, store.EventPaidAgain, 1) {
		t.Errorf("the bitcoin payment that made the reverted order paid again gave no order.paid_again(1)")
	}

	// The bitcoin payment is reorged out; the on-chain rail reverses it.
	node.Invalidate(1)
	ocPass(t, e, oc, o)
	txos, err := e.st.TXOsForOrder(bg, o.ID)
	if err != nil || len(txos) != 1 {
		t.Fatalf("txos %+v %v", txos, err)
	}
	brev, ok, err := e.lg.LookupTransfer(bg, onchain.ReversalTransferID(txos[0].TxID, txos[0].Vout, 0))
	if err != nil || !ok {
		t.Fatalf("no on-chain reversal: %v %v", ok, err)
	}
	t.Logf("stamps: Monero reversal %d, on-chain reversal %d", xrev.UserData64, brev.UserData64)
	if brev.UserData64 == xrev.UserData64 {
		t.Errorf("two reversal cycles carry the same stamp %d", brev.UserData64)
	}

	// wallet-rpc mends; Monero's settle heals its stamp (a no-op now: order.reverted(1) exists).
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatalf("monero pass: %v", err)
	}
	t.Logf("final events: %+v", e.events(o))
	if n := e.eventCount(o, store.EventReverted); n != 2 {
		t.Errorf("order.reverted count %d, want 2 (two paid-to-unpaid cycles)", n)
	}
	if !e.hasEvent(o, store.EventReverted, 2) {
		t.Errorf("no order.reverted(2)")
	}
}
