package monero

import (
	"log/slog"
	"testing"
	"time"

	"github.com/satbooth/satbooth/descriptor"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/onchain/onchaintest"
)

// An on-chain rail pass that does not reach an order (an earlier order's reads use up the poll interval) reports the
// order as held to the Monero rail, which then writes no final event for it before the on-chain side has been read.
func TestCrossRailDeferredOnchainSideHoldsTheMoneroFinalEvent(t *testing.T) {
	e := newWatchEnv(t, nil)
	desc, err := descriptor.Parse("wpkh(" + verifyTpub + "/0/*)")
	if err != nil {
		t.Fatal(err)
	}
	node := onchaintest.New(onchaintest.Normalise(desc.Canonical), 7)
	oc, err := onchain.New(onchain.Config{Descriptor: desc, Network: "regtest", PollInterval: onchain.MinPollInterval,
		CallTimeout: time.Second}, e.st, e.lg, node, e.ln, e.clk.Now, slog.New(slog.NewTextHandler(e.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := oc.Check(bg); err != nil {
		t.Fatal(err)
	}
	mu := e.svc.settleLock()
	e.svc.SetPeer(oc, mu)
	oc.SetPeer(e.svc, mu)

	// h is read first and takes the whole poll interval: two reads that each time out.
	h := e.mixed(0, 100_000, 0)
	ha, err := oc.Address(bg, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	h1, h2 := node.Pay(ha.Address, 10_000), node.Pay(ha.Address, 10_000)
	ocPass(t, e, oc, h)
	node.HangTx(h1)
	node.HangTx(h2)

	o := e.mixed(0, 100_000, xmr)
	a, err := oc.Address(bg, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	node.Pay(a.Address, 40_000) // mempool, below its tier: not recorded
	e.pastExpiry(o)
	ocPass(t, e, oc, o) // reads h; o is not reached
	e.passes(2)
	if len(e.events(o)) != 0 {
		t.Fatalf("a final event for an order whose on-chain side was not read: %+v\n%s", e.events(o), e.log)
	}
	ocPass(t, e, oc, o) // o's turn
	e.passes(2)
	if len(e.events(o)) != 1 {
		t.Fatalf("no final event once the on-chain side was read: %+v\n%s", e.events(o), e.log)
	}
}
