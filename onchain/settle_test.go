package onchain

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

const (
	alertReorg     = "ALERT: an on-chain payment was reverted by a reorg"
	alertPaidAgain = "ALERT: a reverted on-chain payment confirmed again"
	alertBoth      = "ALERT: an order was paid on both rails"
	alertOverpaid  = "ALERT: an order was overpaid; refund the excess"
	alertLate      = "ALERT: a late on-chain payment arrived after the order's expiry"
	alertUnderpaid = "ALERT: an order was underpaid at expiry"
)

// pastExpiry moves the clock to just after the order's expiry.
func (e *env) pastExpiry(o store.Order) {
	e.clk.Advance(o.ExpiresAt.Sub(e.clk.Now()) + time.Second)
}

// --- reorgs, reversals and paid again ---

func TestReorgAfterPaid(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000) // tier 1
	txid := e.payOrder(o, 100_000)
	e.node.Mine(1)
	e.pass()
	if !e.rail(o).Paid || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatal("not paid")
	}

	e.node.Invalidate(1)
	e.pass()
	r, ok := e.rev(txid, 0)
	if !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || e.count(alertReorg) != 1 {
		t.Fatalf("reversal %+v %v, events %+v\n%s", r, ok, e.events(o), e.log)
	}
	// The output is back in the mempool, so it is live and counted: the rail reads seen (a waiting state would not
	// follow from Rail.State's own rule for a mempool output).
	if rl := e.rail(o); rl.Paid || rl.State != "seen" || rl.ConfirmedSat != 0 || rl.ReceivedSat != 100_000 {
		t.Fatalf("after the reorg: %+v", rl)
	}

	e.node.Mine(1)
	e.pass()
	if _, ok := e.fwd(txid, 1); !ok || !e.rail(o).Paid || !e.hasEvent(o, store.EventPaidAgain, 1) || e.count(alertPaidAgain) != 1 {
		t.Fatalf("re-confirmation: %+v", e.events(o))
	}
	if e.eventCount(o, store.EventPaid) != 1 {
		t.Fatal("a second order.paid")
	}

	e.node.Invalidate(1)
	e.pass()
	if r, ok := e.rev(txid, 1); !ok || r.UserData64 != 2 || !e.hasEvent(o, store.EventReverted, 2) {
		t.Fatalf("second reversal: %+v %+v", r, e.events(o))
	}
	e.node.Mine(1)
	e.pass()
	if !e.hasEvent(o, store.EventPaidAgain, 2) || e.count(alertPaidAgain) != 2 || e.count(alertReorg) != 2 {
		t.Fatalf("second paid again: %+v", e.events(o))
	}
	e.passes(3)
	if got := e.sortedSeqs(o, store.EventPaidAgain); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("paid_again %v", got)
	}
	e.safety(o)
}

// A reorg that the next pass never sees out of the chain (the payment is mined again at the same height first) is not
// a reversal: the mark moves to the new block.
func TestReorgBetweenPassesIsNotAReversal(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	txid := e.payOrder(o, 100_000)
	e.node.Mine(1)
	e.pass()
	old, _ := e.mark(txid)
	e.node.Invalidate(1)
	e.node.Mine(2)
	e.pass()
	m, _ := e.mark(txid)
	if m.Kind != store.MarkMined || m.BlockHash == old.BlockHash || m.Height != old.Height {
		t.Fatalf("mark %+v, was %+v", m, old)
	}
	if _, ok := e.rev(txid, 0); ok || !e.rail(o).Paid || len(e.events(o)) != 1 || e.count(alertReorg) != 0 {
		t.Fatalf("a reversal: %+v", e.events(o))
	}
	e.safety(o)
}

// Ruling K: an on-chain reorg under a Lightning-paid order changes nothing the merchant acts on.
func TestRulingKReorgUnderLightningPaid(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(50_000_000, 50_000)
	address := e.addr(o) // issued before the Lightning payment: Address refuses a paid order
	e.lnPaid(o, strings.Repeat("11", 32))
	txid := e.node.Pay(address, 50_000)
	e.node.Mine(1)
	e.pass()
	if _, ok := e.fwd(txid, 0); !ok || e.eventCount(o, store.EventPaid) != 1 || e.count(alertBoth) != 1 {
		t.Fatalf("both rails: %+v\n%s", e.events(o), e.log)
	}
	e.node.Invalidate(1)
	e.pass()
	r, ok := e.rev(txid, 0)
	if !ok || r.UserData64 != 0 || e.eventCount(o, store.EventReverted) != 0 || e.count(alertReorg) != 1 {
		t.Fatalf("reversal %+v %v, events %+v", r, ok, e.events(o))
	}
	e.passes(2)
	if len(e.events(o)) != 1 {
		t.Fatalf("%+v", e.events(o))
	}
	e.safety(o)
}

// Ruling K: a Lightning payment after an on-chain reversal is a paid_again, found through OrdersAwaitingPaidAgain.
func TestRulingKLightningAfterReversal(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(50_000_000, 50_000)
	txid := e.payOrder(o, 50_000)
	e.node.Mine(1)
	e.pass()
	e.node.Invalidate(1)
	e.node.Evict(txid) // gone for good: the output is no longer tracked as live in the mempool
	e.pass()
	if !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("%+v", e.events(o))
	}
	e.lnPaid(o, strings.Repeat("22", 32))
	e.pass()
	if !e.hasEvent(o, store.EventPaidAgain, 1) || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("%+v", e.events(o))
	}
	// The ALERT names the rail whose money paid again: Lightning, not on-chain.
	if e.count(rail.LogPaidAgainLightning) != 1 || e.count(alertPaidAgain) != 0 {
		t.Fatalf("paid-again lines:\n%s", e.log)
	}
	e.safety(o)
}

// Two outputs each enough: the reversal of one leaves the order paid, so it carries 0 and gives no event.
func TestTwoSufficientOutputsOneReorged(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	address := e.addr(o)
	t1 := e.node.Pay(address, 100_000)
	e.node.Mine(1)
	e.pass()
	t2 := e.node.Pay(address, 100_000)
	e.node.Mine(1)
	e.pass()
	if r := e.rail(o); !r.Paid || !r.Overpaid || r.ConfirmedSat != 200_000 || r.State != "overpaid" {
		t.Fatalf("%+v", r)
	}
	e.node.Invalidate(1) // the block with t2
	e.pass()
	r, ok := e.rev(t2, 0)
	if !ok || r.UserData64 != 0 || e.eventCount(o, store.EventReverted) != 0 || !e.rail(o).Paid || e.count(alertReorg) != 1 {
		t.Fatalf("%+v %v %+v", r, ok, e.events(o))
	}
	if _, ok := e.rev(t1, 0); ok {
		t.Fatal("the other output was reversed")
	}
	e.safety(o)
}

// --- both rails, and two outputs ---

func TestBothRailsLightningFirst(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(50_000_000, 50_000)
	address := e.addr(o) // issued before the Lightning payment: Address refuses a paid order
	e.lnPaid(o, strings.Repeat("33", 32))
	txid := e.node.Pay(address, 50_000)
	e.node.Mine(1)
	e.passes(3)
	if _, ok := e.fwd(txid, 0); !ok || e.eventCount(o, store.EventPaid) != 1 || e.count(alertBoth) != 1 {
		t.Fatalf("%+v\n%s", e.events(o), e.log)
	}
	e.node.Mine(1)
	e.passes(3) // the output is still tracked, so the order is settled each pass: the ALERT is not repeated
	if e.count(alertBoth) != 1 {
		t.Fatalf("repeated:\n%s", e.log)
	}
	e.safety(o)
}

// mustPass runs one pass and fails the test on its error.
func (e *env) mustPass() {
	e.t.Helper()
	if err := e.pass(); err != nil {
		e.t.Fatalf("pass: %v", err)
	}
}

// mustPasses is passes, failing the test on a pass's error.
func (e *env) mustPasses(n int) {
	e.t.Helper()
	for range n {
		e.clk.Advance(time.Second)
		e.mustPass()
	}
}

// lnPaysAtFirstStatus makes Lightning pay the order right after the next settle's first Status read (between its
// steps 1 and 3): the Lightning transfer is written, the stub reads paid, and lnpay's completion calls LightningPaid
// before the on-chain transfer exists, which must not alert. *fired reports that it ran.
func (e *env) lnPaysAtFirstStatus(o store.Order, txid, hash string) *bool {
	fired := new(bool)
	e.ln.setOnStatus(func(n int) {
		if n != 1 {
			return
		}
		*fired = true
		if _, ok := e.fwd(txid, 0); ok {
			e.t.Error("the on-chain transfer exists at the first Status read")
		}
		e.lnPaid(o, hash)
		if err := e.svc.LightningPaid(bg, o); err != nil {
			e.t.Error(err)
		}
		if e.count(alertBoth) != 0 {
			e.t.Errorf("the hook alerted before the on-chain transfer exists:\n%s", e.log)
		}
	})
	return fired
}

// The on-chain transfer comes first, then Lightning's. The rail that reads second alerts: here the watcher's
// next pass (settle is level-triggered), and lnpay's completion (LightningPaid) does not repeat it; later passes do
// not either. The ALERT names the order.
func TestBothRailsOnchainFirst(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(50_000_000, 50_000)
	txid := e.payOrder(o, 50_000)
	e.node.Mine(1)
	e.mustPass()
	if e.count(alertBoth) != 0 {
		t.Fatalf("an ALERT before the second payment:\n%s", e.log)
	}
	e.lnPaid(o, strings.Repeat("44", 32))
	e.mustPasses(2)
	if _, ok := e.fwd(txid, 0); !ok || e.eventCount(o, store.EventPaid) != 1 || e.count(alertBoth) != 1 {
		t.Fatalf("ALERTs %d, records: %+v\n%s", e.count(alertBoth), e.events(o), e.log)
	}
	if !strings.Contains(e.log.String(), `msg="`+alertBoth+`" order=`+o.ID) {
		t.Fatalf("the ALERT does not name the order:\n%s", e.log)
	}
	for range 2 {
		if err := e.svc.LightningPaid(bg, o); err != nil {
			t.Fatal(err)
		}
	}
	e.node.Mine(1)
	e.mustPasses(3)
	if e.count(alertBoth) != 1 {
		t.Fatalf("repeated:\n%s", e.log)
	}
	if _, ok := e.lookup(lnPaidID("44")); !ok {
		t.Fatal("the Lightning transfer")
	}
	e.safety(o)
}

// Lightning records its transfer after settle's first Status read (step 1) and its completion calls
// LightningPaid before the on-chain transfer exists, so the hook sees no on-chain payment. settle reads Status again
// after step 3 created the transfer and logs the ALERT, once.
func TestBothRailsRaceAlertsOnce(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(50_000_000, 50_000)
	txid := e.payOrder(o, 50_000) // Address reads Status too: the callback is set after it
	e.node.Mine(1)
	fired := e.lnPaysAtFirstStatus(o, txid, strings.Repeat("55", 32))
	e.mustPass()
	e.ln.setOnStatus(nil)
	if !*fired {
		t.Fatal("the callback did not run")
	}
	if _, ok := e.fwd(txid, 0); !ok || e.count(alertBoth) != 1 {
		t.Fatalf("after the pass: ALERTs %d\n%s", e.count(alertBoth), e.log)
	}
	e.node.Mine(1)
	e.mustPasses(3)
	if err := e.svc.LightningPaid(bg, o); err != nil {
		t.Fatal(err)
	}
	if e.count(alertBoth) != 1 || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("repeated, or order.paid %d:\n%s", e.eventCount(o, store.EventPaid), e.log)
	}
	e.safety(o)
}

// The race above, but the re-read fails. The settle fails with it, and the next pass's level-triggered
// check alerts, once.
func TestBothRailsRereadErrorAlertsNextPass(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(50_000_000, 50_000)
	txid := e.payOrder(o, 50_000)
	e.node.Mine(1)
	errStatus := errors.New("status failed")
	fired := e.lnPaysAtFirstStatus(o, txid, strings.Repeat("56", 32))
	inner := e.ln.onStatus
	e.ln.setOnStatus(func(n int) {
		inner(n)
		if n == 1 {
			e.ln.setFail(errStatus) // the next read, the re-read, fails
		}
	})
	e.mustPass() // the order's settle fails: the pass logs it and goes on, and the order is stale
	e.ln.setOnStatus(nil)
	e.ln.setFail(nil)
	if e.count(logSettleFailed) != 1 || !e.rail(o).Stale {
		t.Fatalf("setup: the settle did not fail:\n%s", e.log)
	}
	if _, ok := e.fwd(txid, 0); !*fired || !ok || e.count(alertBoth) != 0 {
		t.Fatalf("setup: fired %v, transfer %v, ALERTs %d\n%s", *fired, ok, e.count(alertBoth), e.log)
	}
	e.mustPasses(1)
	if e.count(alertBoth) != 1 {
		t.Fatalf("after the next pass: ALERTs %d\n%s", e.count(alertBoth), e.log)
	}
	e.mustPasses(2)
	if e.count(alertBoth) != 1 {
		t.Fatalf("repeated:\n%s", e.log)
	}
	e.safety(o)
}

// The race above, but the process dies between the on-chain transfer and the re-read. After the restart the
// first pass alerts, once.
func TestBothRailsCrashBeforeRereadAlertsAfterRestart(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(50_000_000, 50_000)
	txid := e.payOrder(o, 50_000)
	e.node.Mine(1)
	fired := e.lnPaysAtFirstStatus(o, txid, strings.Repeat("57", 32))
	crashed := e.crashAt("settle:after-transfers")
	if err := e.pass(); !isCrash(err) {
		t.Fatalf("pass: %v, want the crash", err)
	}
	e.ln.setOnStatus(nil)
	if _, ok := e.fwd(txid, 0); !*fired || !*crashed || !ok || e.count(alertBoth) != 0 {
		t.Fatalf("setup: fired %v, crashed %v, transfer %v, ALERTs %d\n%s", *fired, *crashed, ok, e.count(alertBoth), e.log)
	}
	e.restart()
	e.mustPass()
	if e.count(alertBoth) != 1 {
		t.Fatalf("after the restart: ALERTs %d\n%s", e.count(alertBoth), e.log)
	}
	e.mustPasses(2)
	if e.count(alertBoth) != 1 {
		t.Fatalf("repeated:\n%s", e.log)
	}
	e.safety(o)
}

// A partial on-chain payment and a full Lightning payment is a payment on both rails (one is refunded by
// hand): one ALERT, whichever rail is recorded first.
func TestBothRailsPartialOnchain(t *testing.T) {
	t.Run("onchain first", func(t *testing.T) {
		e := newWatchEnv(t)
		o := e.mixed(50_000_000, 50_000)
		txid := e.payOrder(o, 20_000)
		e.node.Mine(1)
		e.mustPass()
		if f, ok := e.fwd(txid, 0); !ok || f.Code != codePaid || e.rail(o).Paid || e.count(alertBoth) != 0 {
			t.Fatalf("setup: forward %+v %v, rail %+v\n%s", f, ok, e.rail(o), e.log)
		}
		e.lnPaid(o, strings.Repeat("58", 32))
		if err := e.svc.LightningPaid(bg, o); err != nil {
			t.Fatal(err)
		}
		if e.count(alertBoth) != 1 {
			t.Fatalf("LightningPaid: ALERTs %d\n%s", e.count(alertBoth), e.log)
		}
		e.mustPasses(3)
		if e.count(alertBoth) != 1 || e.eventCount(o, store.EventPaid) != 1 {
			t.Fatalf("repeated, or order.paid %d:\n%s", e.eventCount(o, store.EventPaid), e.log)
		}
		e.safety(o)
	})
	t.Run("lightning first", func(t *testing.T) {
		e := newWatchEnv(t)
		o := e.mixed(50_000_000, 50_000)
		address := e.addr(o)
		e.lnPaid(o, strings.Repeat("59", 32))
		if err := e.svc.LightningPaid(bg, o); err != nil {
			t.Fatal(err)
		}
		txid := e.node.Pay(address, 20_000)
		e.node.Mine(1)
		e.mustPasses(3)
		if _, ok := e.fwd(txid, 0); !ok || e.count(alertBoth) != 1 || e.eventCount(o, store.EventPaid) != 1 {
			t.Fatalf("ALERTs %d, order.paid %d\n%s", e.count(alertBoth), e.eventCount(o, store.EventPaid), e.log)
		}
		e.safety(o)
	})
}

// A late on-chain payment (code 4) is an on-chain record too: Lightning then pays (a late settlement of its
// invoice), and LightningPaid alerts, once.
func TestBothRailsLateOnchainThenLightning(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(50_000_000, 50_000)
	address := e.addr(o)
	e.pastExpiry(o)
	txid := e.node.Pay(address, 50_000)
	e.node.Mine(1)
	e.mustPasses(2)
	if f, ok := e.fwd(txid, 0); !ok || f.Code != codeLate || e.count(alertBoth) != 0 {
		t.Fatalf("setup: forward %+v %v\n%s", f, ok, e.log)
	}
	e.lnPaid(o, strings.Repeat("5a", 32))
	if err := e.svc.LightningPaid(bg, o); err != nil {
		t.Fatal(err)
	}
	if e.count(alertBoth) != 1 {
		t.Fatalf("LightningPaid: ALERTs %d\n%s", e.count(alertBoth), e.log)
	}
	e.mustPasses(2)
	if e.count(alertBoth) != 1 {
		t.Fatalf("repeated:\n%s", e.log)
	}
	e.safety(o)
}

// Decision 3's gap: the on-chain output is settled deep and no longer watched when Lightning pays; the watcher never
// settles the order again, and LightningPaid still logs the ALERT, once.
func TestBothRailsOnchainSettledThenLightning(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(50_000_000, 50_000)
	txid := e.payOrder(o, 50_000)
	e.node.Mine(6)
	e.mustPasses(3)
	if _, ok := e.fwd(txid, 0); !ok || e.watched(txid) {
		t.Fatalf("setup: the output is not recorded and settled (watched %v)", e.watched(txid))
	}
	e.lnPaid(o, strings.Repeat("66", 32))
	e.mustPasses(2)
	if e.count(alertBoth) != 0 {
		t.Fatalf("an ALERT from the watcher, which no longer reads the order:\n%s", e.log)
	}
	for range 2 {
		if err := e.svc.LightningPaid(bg, o); err != nil {
			t.Fatal(err)
		}
	}
	if e.count(alertBoth) != 1 {
		t.Fatalf("ALERTs %d, want 1:\n%s", e.count(alertBoth), e.log)
	}
	e.safety(o)
}

// lnpay calls LightningPaid from several goroutines (the reconciler, the listener, Pay's flights) while the pass runs:
// the ALERT is logged once, and -race sees no unguarded access to bothAlerted.
func TestLightningPaidConcurrentAlertsOnce(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(50_000_000, 50_000)
	other := e.mixed(50_000_000, 50_000)
	e.payOrder(o, 50_000)
	e.node.Mine(1)
	e.mustPass()
	otherAddr := e.addr(other) // before the stub reads paid: Address refuses a paid order
	e.lnPaid(o, strings.Repeat("88", 32))
	e.node.Pay(otherAddr, 50_000)
	e.node.Mine(1)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := e.svc.LightningPaid(bg, o); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Go(func() { // the pass alerts too: for o (Lightning paid, recorded) and for other (recorded now)
		if err := e.pass(); err != nil {
			t.Error(err)
		}
	})
	wg.Wait()
	if e.count(alertBoth) != 2 || strings.Count(e.log.String(), `msg="`+alertBoth+`" order=`+o.ID) != 1 {
		t.Fatalf("ALERTs %d, want one per order:\n%s", e.count(alertBoth), e.log)
	}
}

// alertBoth itself, from goroutines released together (LightningPaid's ledger reads order its callers, which would hide
// a missing bothMu from -race): one ALERT per order.
func TestAlertBothIsSafeForConcurrentUse(t *testing.T) {
	e := newWatchEnv(t)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			<-start
			e.svc.alertBoth(fmt.Sprintf("order-%d", i%4))
		})
	}
	close(start)
	wg.Wait()
	if e.count(alertBoth) != 4 {
		t.Fatalf("ALERTs %d, want 4:\n%s", e.count(alertBoth), e.log)
	}
}

// LightningPaid for an order with no on-chain record (nothing paid, or an output
// only in the mempool on a tier that needs a block) logs nothing, however often lnpay replays its completion.
func TestLightningPaidWithoutAnOnchainRecordIsSilent(t *testing.T) {
	e := newWatchEnv(t)
	none := e.mixed(50_000_000, 50_000)
	pending := e.mixed(50_000_000, 50_000)
	txid := e.payOrder(pending, 50_000)
	e.mustPass()
	if _, ok := e.fwd(txid, 0); ok {
		t.Fatal("setup: a mempool output was recorded on tier 1")
	}
	e.lnPaid(none, strings.Repeat("77", 32))
	e.lnPaid(pending, strings.Repeat("78", 32))
	for range 2 {
		for _, o := range []store.Order{none, pending} {
			if err := e.svc.LightningPaid(bg, o); err != nil {
				t.Fatal(err)
			}
		}
	}
	if e.count(alertBoth) != 0 {
		t.Fatalf("an ALERT without an on-chain record:\n%s", e.log)
	}
}

func TestTwoSufficientOutputsInTwoPasses(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	address := e.addr(o)
	e.node.Pay(address, 100_000)
	e.node.Mine(1)
	e.pass()
	if e.count(alertOverpaid) != 0 {
		t.Fatal("overpaid on an exact payment")
	}
	e.node.Pay(address, 100_000)
	e.node.Mine(1)
	e.passes(3)
	if r := e.rail(o); !r.Overpaid || e.eventCount(o, store.EventPaid) != 1 || e.count(alertOverpaid) != 1 {
		t.Fatalf("%+v %+v", r, e.events(o))
	}
	e.safety(o)
}

// --- partial, late and overpaid ---

func TestPartialThenRest(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	address := e.addr(o)
	e.node.Pay(address, 60_000)
	e.node.Mine(1)
	e.pass()
	if r := e.rail(o); r.Paid || r.ConfirmedSat != 60_000 || r.State != "seen" || r.ReceivedSat != 60_000 {
		t.Fatalf("%+v", r)
	}
	e.clk.Advance(10 * time.Minute)
	t2 := e.node.Pay(address, 40_000)
	e.pass()
	if e.rail(o).Paid {
		t.Fatal("paid on a mempool output")
	}
	e.clk.Advance(time.Minute)
	e.node.Mine(1)
	e.pass()
	r := e.rail(o)
	tr, _ := e.fwd(t2, 0)
	if !r.Paid || r.Overpaid || !r.PaidAt.Equal(time.Unix(0, int64(tr.Timestamp)).UTC()) || e.count(alertOverpaid) != 0 {
		t.Fatalf("%+v, second transfer at %v", r, time.Unix(0, int64(tr.Timestamp)))
	}
	e.safety(o)
}

func TestPartialThenExpiryThenLate(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	address := e.addr(o)
	e.node.Pay(address, 60_000)
	e.node.Mine(1)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	if !e.hasEvent(o, store.EventUnderpaid, 1) || e.count(alertUnderpaid) != 1 || e.rail(o).State != "underpaid" {
		t.Fatalf("%+v %+v", e.events(o), e.rail(o))
	}
	t2 := e.node.Pay(address, 40_000)
	e.pass()
	// Ruling Z2: the rail reads late at once; order.late waits for the late output's record.
	if e.hasEvent(o, store.EventLate, 1) || e.rail(o).State != "late" || e.rail(o).LateSat != 40_000 || e.count(alertLate) != 0 {
		t.Fatalf("%+v %+v", e.events(o), e.rail(o))
	}
	e.node.Mine(1)
	e.passes(2)
	tr, ok := e.fwd(t2, 0)
	if !ok || tr.Code != codeLate || e.hasEvent(o, store.EventPaid, 1) || e.rail(o).Paid || e.rail(o).State != "late" ||
		!e.hasEvent(o, store.EventLate, 1) {
		t.Fatalf("%+v %v %+v %+v", tr, ok, e.events(o), e.rail(o))
	}
	if e.hasEvent(o, store.EventExpired, 1) || e.count(alertLate) != 1 || e.count(alertUnderpaid) != 1 {
		t.Fatalf("%+v", e.events(o))
	}
	e.safety(o)
}

func TestOverpaidAtOnce(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	e.payOrder(o, 120_000)
	e.node.Mine(1)
	e.passes(3)
	if r := e.rail(o); !r.Paid || !r.Overpaid || r.State != "overpaid" || e.count(alertOverpaid) != 1 {
		t.Fatalf("%+v", r)
	}
	e.safety(o)
}

func TestLatePayment(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	address := e.addr(o)
	e.pass()
	e.pastExpiry(o)
	txid := e.node.Pay(address, 100_000)
	e.pass()
	if e.hasEvent(o, store.EventLate, 1) || e.rail(o).State != "late" {
		t.Fatalf("%+v %+v", e.events(o), e.rail(o))
	}
	e.node.Mine(6)
	e.passes(2)
	tr, ok := e.fwd(txid, 0)
	if !ok || tr.Code != codeLate || e.rail(o).Paid || e.hasEvent(o, store.EventPaid, 1) || e.rail(o).ConfirmedSat != 0 ||
		e.eventCount(o, store.EventLate) != 1 || e.count(alertLate) != 1 {
		t.Fatalf("%+v %v %+v", tr, ok, e.events(o))
	}
	e.safety(o)
}

// Ruling Z2: the rail reads late as soon as a late output is seen, but order.late and its ALERT wait for the late
// forward to be recorded (the output mined at its tier). A late output replaced before it is mined gives neither.
func TestLateEventWaitsForTheRecord(t *testing.T) {
	t.Run("mempool, then mined", func(t *testing.T) {
		e := newWatchEnv(t)
		o := e.order(100_000) // tier 1
		address := e.addr(o)
		e.pastExpiry(o)
		e.pass()
		e.node.Pay(address, 100_000)
		e.passes(3)
		if e.hasEvent(o, store.EventLate, 1) || e.count(alertLate) != 0 {
			t.Fatalf("order.late for a late output in the mempool: %+v\n%s", e.events(o), e.log)
		}
		if r := e.rail(o); r.State != "late" || r.LateSat != 100_000 {
			t.Fatalf("the rail does not read late: %+v", r)
		}
		e.node.Mine(1)
		e.passes(3)
		if e.eventCount(o, store.EventLate) != 1 || e.count(alertLate) != 1 {
			t.Fatalf("mined at its tier: %+v\n%s", e.events(o), e.log)
		}
		e.safety(o)
	})
	t.Run("replaced before mining", func(t *testing.T) {
		e := newWatchEnv(t)
		o := e.order(100_000)
		address := e.addr(o)
		e.pastExpiry(o)
		e.pass()
		txid := e.node.Pay(address, 100_000)
		e.pass()
		e.node.Replace(txid, false) // the replacement no longer pays the address
		e.node.Mine(1)
		e.passes(3)
		if e.hasEvent(o, store.EventLate, 1) || e.count(alertLate) != 0 {
			t.Fatalf("order.late for a late output that was replaced: %+v\n%s", e.events(o), e.log)
		}
		if _, ok := e.fwd(txid, 0); ok {
			t.Fatal("the replaced output was recorded")
		}
		if r := e.rail(o); r.State != "expired" || r.LateSat != 0 {
			t.Fatalf("%+v", r)
		}
		e.safety(o)
	})
}

// A late payment counts for nothing in confirmedSum or paidOnchain.
func TestLateIsNeverConfirmed(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	address := e.addr(o)
	e.pastExpiry(o)
	txid := e.node.Pay(address, 100_000)
	e.node.Mine(1)
	e.passes(2)
	sum, err := e.svc.confirmedSum(bg, o)
	paid, err2 := e.svc.paidOnchain(bg, o)
	if _, ok := e.fwd(txid, 0); !ok || sum != 0 || paid || err != nil || err2 != nil {
		t.Fatalf("late counted: %d %v %v %v", sum, paid, err, err2)
	}
	e.safety(o)
}

// A forward transfer whose details are not the output's is not counted, and its ALERT is logged.
func TestForeignRecordIsNotCounted(t *testing.T) {
	for name, tamper := range map[string]func(*ledger.Transfer){
		"code":     func(tr *ledger.Transfer) { tr.Code = codeLate },
		"order":    func(tr *ledger.Transfer) { tr.UserData128 = ledger.U128From64(9) },
		"amount":   func(tr *ledger.Transfer) { tr.Amount = ledger.U128From64(1) },
		"accounts": func(tr *ledger.Transfer) { tr.DebitAccountID, tr.CreditAccountID = MerchantOnchain, OnchainWallet },
	} {
		t.Run(name, func(t *testing.T) {
			e := newWatchEnv(t)
			o := e.order(100_000)
			txid := e.payOrder(o, 100_000)
			e.pass()
			ud, _ := orderUserData(o.ID)
			tr := ledger.Transfer{ID: PaidTransferID(txid, 0, 0), DebitAccountID: OnchainWallet, CreditAccountID: MerchantOnchain,
				Amount: ledger.U128From64(100_000), UserData128: ud, Ledger: ledgerID, Code: codePaid}
			if name == "accounts" { // the merchant account must hold something to be debited
				e.transfer(PaidTransferID(strings.Repeat("ef", 32), 0, 0), OnchainWallet, MerchantOnchain, o, 100_000, codePaid, 0)
			}
			tamper(&tr)
			if r, err := e.lg.CreateTransfer(bg, tr); err != nil || r != ledger.TransferCreated {
				t.Fatal(r, err)
			}
			e.node.Mine(1)
			e.pass()
			sum, _ := e.svc.confirmedSum(bg, o)
			if sum != 0 || e.rail(o).Paid || e.count("ALERT: a payment record exists with different details") == 0 || len(e.events(o)) != 0 {
				t.Fatalf("sum %d %+v\n%s", sum, e.events(o), e.log)
			}
			// Past the expiry it still holds the order as a payment seen: no final event on a confused record.
			e.pastExpiry(o)
			e.pass()
			if r := e.rail(o); r.ReceivedSat != 100_000 || r.State != "seen" || len(e.events(o)) != 0 {
				t.Fatalf("after expiry: %+v %+v", r, e.events(o))
			}
		})
	}
}

// --- RBF ---

// A bump that keeps the output, before the expiry: it inherits the original's first-seen (plan 6.1; the rule has no
// expiry condition, so it holds before the expiry too).
func TestRBFKeepingTheOutput(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	old := e.payOrder(o, 100_000)
	e.pass()
	first := e.txo(old).FirstSeen
	e.clk.Advance(time.Minute)
	nw := e.node.Replace(old, true)
	e.pass()
	if m, _ := e.mark(old); m.Kind != store.MarkDead {
		t.Fatalf("old: %+v", m)
	}
	x, err := e.st.TXO(bg, store.TXOKey{TxID: nw})
	if err != nil || !x.FirstSeen.Equal(first) {
		t.Fatalf("the replacement's first-seen: %+v %v, want %v", x, err, first)
	}
	if r := e.rail(o); r.ReceivedSat != 100_000 || r.State != "seen" {
		t.Fatalf("%+v", r)
	}
	e.node.Mine(1)
	e.pass()
	if _, ok := e.fwd(nw, 0); !ok || !e.rail(o).Paid {
		t.Fatal("the replacement is not paid")
	}
	if _, ok := e.fwd(old, 0); ok {
		t.Fatal("the replaced output was recorded")
	}
	e.passes(2)
	if m, _ := e.mark(old); m.Kind != store.MarkDead { // now conflicted in a block
		t.Fatalf("old: %+v", m)
	}
	e.safety(o)
}

func TestRBFAwayThenExpiry(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	old := e.payOrder(o, 100_000)
	e.pass()
	e.node.Replace(old, false)
	e.pass()
	if r := e.rail(o); r.ReceivedSat != 0 || r.State != "waiting" {
		t.Fatalf("%+v", r)
	}
	e.node.Mine(1)
	e.pastExpiry(o)
	e.passes(2)
	if !e.hasEvent(o, store.EventExpired, 1) || e.hasEvent(o, store.EventUnderpaid, 1) || e.rail(o).State != "expired" {
		t.Fatalf("%+v %+v", e.events(o), e.rail(o))
	}
	e.safety(o)
}

// Ruling D, as narrowed by plan 6.1: a bump after the expiry to a lower amount is a new output with its own first-seen,
// late; the order is expired (its on-time payment died), order.late when the bump is mined, never paid.
func TestBumpWithALowerAmountIsLate(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	ad := e.addr(o)
	old := e.node.Pay(ad, 100_000)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	if len(e.events(o)) != 0 || e.rail(o).State != "seen" {
		t.Fatalf("a mempool payment holds the order: %+v %+v", e.events(o), e.rail(o))
	}
	e.clk.Advance(time.Minute)
	nw := e.node.ReplaceWith(old, []onchaintest.Out{{Address: ad, Sat: 90_000}})
	e.pass()
	if x := e.txo(nw); !x.FirstSeen.Equal(e.clk.Now()) {
		t.Fatalf("first-seen %v, now %v", x.FirstSeen, e.clk.Now())
	}
	// The pass that marks the original dead holds the final event for one pass (diedInPass).
	if len(e.events(o)) != 0 {
		t.Fatalf("a final event in the pass of the dead mark: %+v", e.events(o))
	}
	e.pass()
	if r := e.rail(o); e.hasEvent(o, store.EventLate, 1) || !e.hasEvent(o, store.EventExpired, 1) || r.State != "late" || r.LateSat != 90_000 || r.ReceivedSat != 0 {
		t.Fatalf("%+v %+v", e.events(o), r)
	}
	e.node.Mine(1)
	e.pass()
	if tr, ok := e.fwd(nw, 0); !ok || tr.Code != codeLate || e.rail(o).Paid || !e.hasEvent(o, store.EventLate, 1) || e.eventCount(o, store.EventPaid) != 0 {
		t.Fatal("the late replacement")
	}
	e.safety(o)
}

// A bump after the expiry to a higher amount is a new output too.
// Otherwise a tiny on-time output, bumped into the full amount after the expiry, would make a late payment count as
// on time. It gets its own first-seen and is late: order.late when mined, the order never paid.
func TestBumpWithAHigherAmountIsLate(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	ad := e.addr(o)
	old := e.node.Pay(ad, 1)
	e.pass()
	if x := e.txo(old); !x.FirstSeen.Before(o.ExpiresAt) {
		t.Fatalf("setup: %+v", x)
	}
	e.pastExpiry(o)
	nw := e.node.ReplaceWith(old, []onchaintest.Out{{Address: ad, Sat: 100_000}})
	e.pass()
	if x := e.txo(nw); !x.FirstSeen.Equal(e.clk.Now()) {
		t.Fatalf("first-seen %v, now %v", x.FirstSeen, e.clk.Now())
	}
	if r := e.rail(o); r.State != RailLate || r.LateSat != 100_000 || r.ReceivedSat != 0 {
		t.Fatalf("Rail %+v", r)
	}
	e.node.Mine(1)
	e.pass()
	if tr, ok := e.fwd(nw, 0); !ok || tr.Code != codeLate || e.rail(o).Paid || !e.hasEvent(o, store.EventLate, 1) || e.eventCount(o, store.EventPaid) != 0 {
		t.Fatalf("mined: %+v %v %+v", tr, ok, e.events(o))
	}
	e.safety(o)
}

// A bump to another address is its own output: to a foreign address it is not kept at all (as before plan 6.1); to the
// address of a second order, in full, it gets its own first-seen (late for that order, past its expiry) and the first
// order reads expired.
func TestBumpToAnotherAddressIsItsOwn(t *testing.T) {
	t.Run("foreign", func(t *testing.T) {
		e := newWatchEnv(t)
		o := e.order(100_000)
		old := e.payOrder(o, 100_000)
		e.pass()
		e.pastExpiry(o)
		nw := e.node.Replace(old, false)
		e.pass()
		if _, err := e.st.TXO(bg, store.TXOKey{TxID: nw}); !errors.Is(err, store.ErrNotFound) || len(e.txos(o)) != 1 {
			t.Fatalf("the foreign output was kept: %v", err)
		}
		if len(e.events(o)) != 0 { // the pass of the dead mark holds the final event for one pass (diedInPass)
			t.Fatalf("a final event in the pass of the dead mark: %+v", e.events(o))
		}
		e.pass()
		if r := e.rail(o); r.State != RailExpired || r.ReceivedSat != 0 || !e.hasEvent(o, store.EventExpired, 1) {
			t.Fatalf("%+v %+v", r, e.events(o))
		}
		e.safety(o)
	})
	t.Run("another order's address", func(t *testing.T) {
		e := newWatchEnv(t)
		o1, o2 := e.order(100_000), e.order(100_000)
		a1, a2 := e.addr(o1), e.addr(o2)
		old := e.node.Pay(a1, 100_000)
		e.pass()
		e.pastExpiry(o2)
		nw := e.node.ReplaceWith(old, []onchaintest.Out{{Address: a2, Sat: 100_000}})
		e.pass()
		if x := e.txo(nw); !x.FirstSeen.Equal(e.clk.Now()) || !e.clk.Now().After(o2.ExpiresAt) {
			t.Fatalf("first-seen %v, now %v", x.FirstSeen, e.clk.Now())
		}
		if len(e.events(o1)) != 0 { // the pass of the dead mark holds the final event for one pass (diedInPass)
			t.Fatalf("a final event in the pass of the dead mark: %+v", e.events(o1))
		}
		e.pass()
		if r := e.rail(o1); r.State != RailExpired || r.ReceivedSat != 0 || !e.hasEvent(o1, store.EventExpired, 1) {
			t.Fatalf("the first order: %+v %+v", r, e.events(o1))
		}
		if r := e.rail(o2); r.State != RailLate || r.LateSat != 100_000 {
			t.Fatalf("the second order: %+v", r)
		}
		e.node.Mine(1)
		e.pass()
		if tr, ok := e.fwd(nw, 0); !ok || tr.Code != codeLate || e.rail(o2).Paid || e.rail(o1).Paid || !e.hasEvent(o2, store.EventLate, 1) {
			t.Fatalf("mined: %+v %v", tr, ok)
		}
		e.safety(o1, o2)
	})
}

// --- final events and the Lightning side ---

// An open Lightning invoice holds a mixed order past its expiry; once Lightning reads expired, the final event comes.
func TestFinalEventWaitsForLightning(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(50_000_000, 50_000)
	e.addr(o)
	e.pastExpiry(o)
	e.pass()
	if len(e.events(o)) != 0 {
		t.Fatalf("%+v", e.events(o))
	}
	e.ln.set(false, true)
	e.pass()
	if !e.hasEvent(o, store.EventExpired, 1) {
		t.Fatalf("%+v", e.events(o))
	}
	e.safety(o)
}

// Past the expiry, not paid and nothing late, the final unpaid states come from the events, never from the clock alone:
// an order reads expired or underpaid only once order.expired or order.underpaid exists, and live on-time money never
// reads expired. Live money reads seen only while no such event exists; with one it reads underpaid, the whole amount
// before the window end included. No pass runs after the expiry: the events are added directly, as settle would (or as
// a crash that lost the hold would leave them).
func TestRailFinalStatesFollowTheEvents(t *testing.T) {
	for _, received := range []int64{0, 60_000, 100_000} {
		for _, window := range []bool{false, true} {
			for _, event := range []string{"", store.EventExpired, store.EventUnderpaid} {
				var want string
				switch { // the whole amount live before the window end no longer outranks the event
				case event == "" && received > 0:
					want = RailSeen
				case event == "":
					want = RailWaiting
				case event == store.EventUnderpaid || received > 0:
					want = RailUnderpaid
				default:
					want = RailExpired
				}
				t.Run(fmt.Sprintf("%d/window=%v/%q", received, window, event), func(t *testing.T) {
					e := newWatchEnv(t)
					o := e.order(100_000) // tier 1: an output in the mempool is live and not recorded
					address := e.addr(o)
					if received > 0 {
						e.node.Pay(address, received)
					}
					e.pass()
					if window {
						e.toWindowEnd(o)
					} else {
						e.pastExpiry(o)
					}
					if event != "" {
						if _, err := e.st.AddEvent(bg, store.Event{Type: event, OrderID: o.ID, Seq: 1}); err != nil {
							t.Fatal(err)
						}
					}
					if r := e.rail(o); r.State != want || r.ReceivedSat != received || r.Paid {
						t.Fatalf("Rail %+v, want %s", r, want)
					}
				})
			}
		}
	}
}

// An evicted payment past expiry is dead: expired, not underpaid.
func TestEvictedIsDead(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	txid := e.payOrder(o, 50_000)
	e.pass()
	e.node.Evict(txid)
	e.pastExpiry(o)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkDead || len(e.events(o)) != 0 {
		t.Fatalf("the pass of the dead mark holds the final event for one pass (diedInPass): %+v %+v", m, e.events(o))
	}
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkDead || !e.hasEvent(o, store.EventExpired, 1) {
		t.Fatalf("%+v %+v", m, e.events(o))
	}
	e.safety(o)
}

func lnPaidID(b string) ledger.U128 { return lnpay.PaidTransferID(strings.Repeat(b, 32)) }

// Ruling S: a counted payment evicted before the expiry gives order.expired (or underpaid); rebroadcast and
// mined later it is still counted, so order.paid follows, with an ALERT, once.
func TestPaidAfterAFinalEvent(t *testing.T) {
	for _, c := range []struct {
		name  string
		first int64 // a payment that stays, 0 for none
		final string
	}{{"expired", 0, store.EventExpired}, {"underpaid", 60_000, store.EventUnderpaid}} {
		t.Run(c.name, func(t *testing.T) {
			e := newWatchEnv(t)
			o := e.order(100_000)
			address := e.addr(o)
			if c.first > 0 {
				e.node.Pay(address, c.first)
				e.node.Mine(1)
			}
			txid := e.node.Pay(address, 100_000-c.first)
			e.pass()
			e.node.Evict(txid)
			e.pastExpiry(o)
			e.pass()
			if len(e.events(o)) != 0 { // the pass of the dead mark holds the final event for one pass (diedInPass)
				t.Fatalf("%+v", e.events(o))
			}
			// The held pass reads no final state: seen with the payment that stays, waiting with none.
			held := RailWaiting
			if c.first > 0 {
				held = RailSeen
			}
			if r, s := e.rail(o), e.status(o); r.State != held || r.ReceivedSat != c.first || s != "new" {
				t.Fatalf("the held pass: Rail %+v, status %q", r, s)
			}
			e.pass()
			if !e.hasEvent(o, c.final, 1) || e.hasEvent(o, store.EventPaid, 1) || e.rail(o).State != c.name || e.status(o) != c.name {
				t.Fatalf("%+v", e.events(o))
			}
			e.node.Rebroadcast(txid)
			e.node.Mine(1)
			e.passes(3)
			if !e.hasEvent(o, store.EventPaid, 1) || !e.rail(o).Paid || e.count(logPaidAfterFinal) != 1 {
				t.Fatalf("%+v %+v\n%s", e.events(o), e.rail(o), e.log)
			}
			e.safety(o)
		})
	}
}

// An order paid before its expiry logs no such ALERT.
func TestPaidInTimeHasNoAfterFinalAlert(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	e.payOrder(o, 100_000)
	e.node.Mine(1)
	e.pass()
	e.pastExpiry(o)
	e.passes(2)
	if e.count(logPaidAfterFinal) != 0 || len(e.events(o)) != 1 {
		t.Fatalf("%+v", e.events(o))
	}
	e.safety(o)
}

// Ruling T2: the first order.paid that settle adds writes the account_paid fact, then marks the account
// known in lnpay; the fact is in the store when MarkKnown runs, and later passes mark nothing again.
func TestOnchainPaidMakesTheAccountKnown(t *testing.T) {
	e := newWatchEnv(t)
	factFirst := true
	e.ln.onMark = func(acct string) {
		if ok, err := e.st.AccountPaid(bg, acct); err != nil || !ok {
			factFirst = false
		}
	}
	o := e.order(100_000)
	e.payOrder(o, 100_000)
	e.pass()
	if ok, _ := e.st.AccountPaid(bg, o.AccountID); ok || len(e.ln.markedAccounts()) != 0 {
		t.Fatal("known before the payment confirmed")
	}
	e.node.Mine(1)
	e.passes(3)
	if ok, err := e.st.AccountPaid(bg, o.AccountID); err != nil || !ok {
		t.Fatalf("no account_paid fact: %v", err)
	}
	if m := e.ln.markedAccounts(); len(m) != 1 || m[0] != o.AccountID || !factFirst {
		t.Fatalf("MarkKnown %v, fact first %v", m, factFirst)
	}
	e.safety(o)
}

// When order.paid already exists (lnpay added it for a Lightning payment, with its own fact and mark), settle adds
// neither.
func TestLightningPaidOrderIsNotMarkedAgain(t *testing.T) {
	e := newWatchEnv(t)
	o := e.mixed(50_000_000, 50_000)
	address := e.addr(o)
	e.lnPaid(o, strings.Repeat("55", 32))
	if _, err := e.st.AddEvent(bg, store.Event{Type: store.EventPaid, OrderID: o.ID}); err != nil {
		t.Fatal(err)
	}
	e.node.Pay(address, 50_000)
	e.node.Mine(1)
	e.passes(2)
	if m := e.ln.markedAccounts(); len(m) != 0 {
		t.Fatalf("marked %v", m)
	}
	e.safety(o)
}

// A crash right after the account fact, before the mark and order.paid, is completed on the next pass (a restarted
// lnpay derives the known set from the facts anyway).
func TestCrashAfterTheAccountFact(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	e.payOrder(o, 100_000)
	e.node.Mine(1)
	e.crashAt("settle:after-account")
	if err := e.pass(); !errors.Is(err, errCrash) {
		t.Fatalf("no crash: %v", err)
	}
	if ok, _ := e.st.AccountPaid(bg, o.AccountID); !ok || e.hasEvent(o, store.EventPaid, 1) {
		t.Fatal("the crash point")
	}
	e.restart()
	e.pass()
	if !e.hasEvent(o, store.EventPaid, 1) || len(e.ln.markedAccounts()) != 1 { // the crash came before the mark
		t.Fatalf("%+v %v", e.events(o), e.ln.markedAccounts())
	}
	e.safety(o)
}

// --- a 0 tier: recorded from the mempool, reversed only when dead ---

// zeroConfPaid makes a 5,000-sat order on zeroTiers, pays it and runs one pass: it is paid from the mempool.
func (e *env) zeroConfPaid() (store.Order, string) {
	e.t.Helper()
	o := e.order(5_000)
	txid := e.payOrder(o, 5_000)
	e.pass()
	if _, ok := e.fwd(txid, 0); !ok || !e.rail(o).Paid || e.eventCount(o, store.EventPaid) != 1 {
		e.t.Fatalf("setup: not paid from the mempool: %+v %+v", e.rail(o), e.events(o))
	}
	return o, txid
}

// A 0-conf record replaced away: the pass that marks it dead (after its scan) holds the reversal, so the order still
// reads paid with no new event and no ALERT; the next pass, which lists no replacement, reverses it.
func TestZeroConfReplacedIsReverted(t *testing.T) {
	e := newZeroConfEnv(t)
	o, txid := e.zeroConfPaid()
	e.node.Replace(txid, false) // the replacement pays a foreign address
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkDead {
		t.Fatalf("mark %+v", m)
	}
	if _, ok := e.rev(txid, 0); ok || len(e.events(o)) != 1 || e.count(alertReorg) != 0 || !e.rail(o).Paid || e.status(o) != "paid" {
		t.Fatalf("the held pass: reversed %v, events %+v, Rail %+v\n%s", ok, e.events(o), e.rail(o), e.log)
	}
	e.pass()
	r, ok := e.rev(txid, 0)
	if !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || e.count(alertReorg) != 1 {
		t.Fatalf("reversal %+v %v, events %+v\n%s", r, ok, e.events(o), e.log)
	}
	if rl := e.rail(o); rl.Paid || rl.State != "waiting" || rl.ReceivedSat != 0 {
		t.Fatalf("after the replacement: %+v", rl)
	}
	before := e.outcome(o)
	e.passes(2)
	if got := e.outcome(o); got != before || e.count(alertReorg) != 1 {
		t.Fatalf("two more passes changed it:\n got %s\nwant %s", got, before)
	}
	e.safety(o)
}

func TestZeroConfEvictedThenBackIsPaidAgain(t *testing.T) {
	e := newZeroConfEnv(t)
	o, txid := e.zeroConfPaid()
	e.node.Evict(txid)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkDead {
		t.Fatalf("mark %+v", m)
	}
	if _, ok := e.rev(txid, 0); ok || len(e.events(o)) != 1 {
		t.Fatalf("the held pass: reversed %v, events %+v", ok, e.events(o))
	}
	e.pass() // the pass after the hold reverses it
	if r, ok := e.rev(txid, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("reversal %+v %v, events %+v", r, ok, e.events(o))
	}
	e.node.Rebroadcast(txid)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkMempool {
		t.Fatalf("mark %+v", m)
	}
	if f, ok := e.fwd(txid, 1); !ok || f.Code != codePaid {
		t.Fatalf("no forward at epoch 1: %+v %v", f, ok)
	}
	if !e.rail(o).Paid || !e.hasEvent(o, store.EventPaidAgain, 1) || e.count(alertPaidAgain) != 1 || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("back in the mempool: %+v %+v", e.rail(o), e.events(o))
	}
	e.passes(2)
	if e.count(alertPaidAgain) != 1 || e.eventCount(o, store.EventPaidAgain) != 1 {
		t.Fatalf("replayed: %+v", e.events(o))
	}
	e.safety(o)
}

// The other side: an evicted 0-conf record rebroadcast before the pass after the hold is never reversed. The held
// pass keeps it, the next pass sees it back in the mempool, and the order stays paid with no event and no ALERT.
func TestZeroConfEvictedAndBackWithinTheHoldStaysPaid(t *testing.T) {
	e := newZeroConfEnv(t)
	o, txid := e.zeroConfPaid()
	e.node.Evict(txid)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkDead || !e.rail(o).Paid {
		t.Fatalf("the held pass: %+v %+v", m, e.rail(o))
	}
	e.node.Rebroadcast(txid)
	e.passes(3)
	if m, _ := e.mark(txid); m.Kind != store.MarkMempool {
		t.Fatalf("mark %+v", m)
	}
	if _, ok := e.rev(txid, 0); ok || !e.rail(o).Paid || len(e.events(o)) != 1 || e.count(alertReorg) != 0 || e.count(alertPaidAgain) != 0 {
		t.Fatalf("back within the hold: reversed %v, Rail %+v, events %+v\n%s", ok, e.rail(o), e.events(o), e.log)
	}
	e.safety(o)
}

// A fee bump that keeps the output, seen in one pass: the old record (now dead) is reversed while the new output is
// recorded, the order stays paid, and the overpaid check does not count the record being reversed.
func TestZeroConfBumpedToTheSameAddressStaysPaid(t *testing.T) {
	e := newZeroConfEnv(t)
	o, old := e.zeroConfPaid()
	nw := e.node.Replace(old, true)
	e.pass()
	if f, ok := e.fwd(nw, 0); !ok || f.Code != codePaid {
		t.Fatalf("the replacement is not recorded: %+v %v", f, ok)
	}
	if r, ok := e.rev(old, 0); !ok || r.UserData64 != 0 {
		t.Fatalf("the old record: %+v %v", r, ok)
	}
	if e.eventCount(o, store.EventReverted) != 0 || !e.rail(o).Paid || e.rail(o).Overpaid || e.count(alertReorg) != 1 || e.count(alertOverpaid) != 0 {
		t.Fatalf("%+v %+v\n%s", e.rail(o), e.events(o), e.log)
	}
	e.passes(2)
	e.node.Mine(1)
	e.passes(2)
	if e.count(alertOverpaid) != 0 || e.count(alertReorg) != 1 || e.eventCount(o, store.EventPaid) != 1 || len(e.events(o)) != 1 {
		t.Fatalf("after the block: %+v\n%s", e.events(o), e.log)
	}
	e.safety(o)
}

// The same bump, with the old output dead in one pass and the new one listed only in the next. The
// first pass holds the old record's reversal: the order stays paid, with no order.reverted and no ALERT. The second
// pass records the replacement (it inherits the first-seen) and reverses the old record as a same-pass bump: stamp 0,
// no event, so no order.paid_again either, and the one reorg ALERT a same-pass bump gives.
func TestZeroConfBumpedAcrossTwoPasses(t *testing.T) {
	e := newZeroConfEnv(t)
	o, old := e.zeroConfPaid()
	var nw string
	e.svc.hook = func(p string) error {
		if p == "scan:after-record" && nw == "" {
			nw = e.node.Replace(old, true) // after this pass's discovery: the new output waits for the next one
		}
		return nil
	}
	e.pass()
	if nw == "" {
		t.Fatal("the hook did not run")
	}
	if m, _ := e.mark(old); m.Kind != store.MarkDead {
		t.Fatalf("old mark %+v", m)
	}
	if l := e.txos(o); len(l) != 1 {
		t.Fatalf("the replacement was discovered in the first pass: %+v", l)
	}
	if _, ok := e.rev(old, 0); ok || len(e.events(o)) != 1 || !e.rail(o).Paid || e.status(o) != "paid" || e.count(alertReorg) != 0 {
		t.Fatalf("the first pass: reversed %v, events %+v, Rail %+v\n%s", ok, e.events(o), e.rail(o), e.log)
	}
	e.pass()
	if f, ok := e.fwd(nw, 0); !ok || f.Code != codePaid || !e.txo(nw).FirstSeen.Equal(e.txo(old).FirstSeen) {
		t.Fatalf("the replacement: %+v %v", f, ok)
	}
	if r, ok := e.rev(old, 0); !ok || r.UserData64 != 0 {
		t.Fatalf("the old record: %+v %v", r, ok)
	}
	if r := e.rail(o); !r.Paid || r.Overpaid || r.ConfirmedSat != 5_000 || len(e.events(o)) != 1 || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("the second pass: %+v %+v", r, e.events(o))
	}
	e.passes(2)
	e.node.Mine(1)
	e.passes(2)
	if e.count(alertOverpaid) != 0 || e.count(alertReorg) != 1 || e.count(alertPaidAgain) != 0 || len(e.events(o)) != 1 {
		t.Fatalf("events %+v\n%s", e.events(o), e.log)
	}
	e.safety(o)
}

// The split pass when the replacement takes the payment away: a real double spend is reversed one
// pass later, with order.reverted and the reorg ALERT, and the order reads unpaid from then on.
func TestZeroConfDivertedAcrossTwoPassesIsRevertedOnePassLater(t *testing.T) {
	e := newZeroConfEnv(t)
	o, old := e.zeroConfPaid()
	nw := e.bumpAfterScan(old, false, false)
	e.pass()
	if _, ok := e.rev(old, 0); *nw == "" || ok || len(e.events(o)) != 1 || !e.rail(o).Paid || e.count(alertReorg) != 0 {
		t.Fatalf("the first pass: %q reversed %v, events %+v", *nw, ok, e.events(o))
	}
	e.pass()
	if r, ok := e.rev(old, 0); !ok || r.UserData64 != 1 || !e.hasEvent(o, store.EventReverted, 1) || e.count(alertReorg) != 1 {
		t.Fatalf("the second pass: %+v %v, events %+v", r, ok, e.events(o))
	}
	if r, s := e.rail(o), e.status(o); r.Paid || r.State != RailWaiting || s != "new" {
		t.Fatalf("the second pass: Rail %+v, status %q", r, s)
	}
	e.passes(2)
	if len(e.events(o)) != 2 || e.count(alertReorg) != 1 || e.count(alertPaidAgain) != 0 {
		t.Fatalf("replayed: %+v", e.events(o))
	}
	e.safety(o)
}

// Across a crash: the hold lives in memory only. A crash right after the dead mark, then a restart: the restarted
// pass's scan lists the replacement, which is recorded before the old record is reversed (stamp 0, no event); when
// the replacement took the payment away, the restarted pass reverses the old record with order.reverted.
func TestZeroConfBumpAfterTheScanAcrossACrash(t *testing.T) {
	for _, keep := range []bool{true, false} {
		t.Run(map[bool]string{true: "bumped", false: "diverted"}[keep], func(t *testing.T) {
			e := newZeroConfEnv(t)
			o, old := e.zeroConfPaid()
			nw := e.bumpAfterScan(old, keep, true)
			if err := e.pass(); !errors.Is(err, errCrash) {
				t.Fatalf("no crash: %v", err)
			}
			if m, _ := e.mark(old); *nw == "" || m.Kind != store.MarkDead || len(e.events(o)) != 1 {
				t.Fatalf("the crashed pass: %q %+v %+v", *nw, m, e.events(o))
			}
			e.restart()
			e.pass()
			r, ok := e.rev(old, 0)
			if keep && (!ok || r.UserData64 != 0 || !e.rail(o).Paid || len(e.events(o)) != 1) {
				t.Fatalf("bumped, after the restart: %+v %v, events %+v", r, ok, e.events(o))
			}
			if !keep && (!ok || r.UserData64 != 1 || e.rail(o).Paid || !e.hasEvent(o, store.EventReverted, 1)) {
				t.Fatalf("diverted, after the restart: %+v %v, events %+v", r, ok, e.events(o))
			}
			e.passes(2)
			if e.count(alertReorg) != 1 || e.count(alertPaidAgain) != 0 || e.eventCount(o, store.EventPaid) != 1 {
				t.Fatalf("events %+v\n%s", e.events(o), e.log)
			}
			e.safety(o)
		})
	}
}

// On a 0 tier, a record whose transaction goes back to the mempool after a reorg is still counted.
func TestZeroConfReorgToMempoolKeepsTheRecord(t *testing.T) {
	e := newZeroConfEnv(t)
	o, txid := e.zeroConfPaid()
	e.node.Mine(1)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkMined {
		t.Fatalf("mark %+v", m)
	}
	e.node.Invalidate(1)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkMempool {
		t.Fatalf("mark %+v", m)
	}
	if _, ok := e.rev(txid, 0); ok || !e.rail(o).Paid || len(e.events(o)) != 1 || e.count(alertReorg) != 0 {
		t.Fatalf("a reversal: %+v %+v", e.rail(o), e.events(o))
	}
	e.safety(o)
}

// Ruling 6.1-10: a late output needs a block, whatever the order's tier, so order.late (Ruling Z2) never follows a
// payment that can still be replaced.
func TestZeroConfLateWaitsForABlock(t *testing.T) {
	e := newZeroConfEnv(t)
	o := e.order(5_000)
	address := e.addr(o)
	e.pass()
	e.pastExpiry(o)
	txid := e.node.Pay(address, 5_000)
	e.passes(2)
	if m, _ := e.mark(txid); m.Kind != store.MarkMempool {
		t.Fatalf("mark %+v", m)
	}
	if _, ok := e.fwd(txid, 0); ok || e.hasEvent(o, store.EventLate, 1) || e.count(alertLate) != 0 {
		t.Fatalf("a late output recorded from the mempool: %+v\n%s", e.events(o), e.log)
	}
	if r := e.rail(o); r.State != "late" || r.Paid {
		t.Fatalf("%+v", r)
	}
	e.safety(o)
	e.node.Mine(1)
	e.pass()
	f, ok := e.fwd(txid, 0)
	if !ok || f.Code != codeLate || e.eventCount(o, store.EventLate) != 1 || e.count(alertLate) != 1 {
		t.Fatalf("after a block: %+v %v %+v", f, ok, e.events(o))
	}
	e.passes(2)
	if r := e.rail(o); r.Paid || e.hasEvent(o, store.EventPaid, 1) || e.eventCount(o, store.EventLate) != 1 || e.count(alertLate) != 1 {
		t.Fatalf("%+v %+v", r, e.events(o))
	}
	e.safety(o)
}

// A late record sent back to the mempool by a reorg is reversed on a 0 tier too: a late output needs a block.
func TestZeroConfLateRecordBackInTheMempoolIsReversed(t *testing.T) {
	e := newZeroConfEnv(t)
	o := e.order(5_000)
	address := e.addr(o)
	e.pass()
	e.pastExpiry(o)
	txid := e.node.Pay(address, 5_000)
	e.node.Mine(1)
	e.pass()
	if f, ok := e.fwd(txid, 0); !ok || f.Code != codeLate {
		t.Fatalf("setup: %+v %v", f, ok)
	}
	e.node.Invalidate(1)
	e.pass()
	if m, _ := e.mark(txid); m.Kind != store.MarkMempool {
		t.Fatalf("mark %+v", m)
	}
	if r, ok := e.rev(txid, 0); !ok || r.UserData64 != 0 || e.eventCount(o, store.EventLate) != 1 || e.hasEvent(o, store.EventReverted, 1) {
		t.Fatalf("reversal %+v %v, events %+v", r, ok, e.events(o))
	}
	e.safety(o)
}

// An output with no mark yet is not recorded on a 0 tier: only a mempool or mined mark makes it recordable.
func TestZeroConfUnmarkedIsNotRecorded(t *testing.T) {
	e := newZeroConfEnv(t)
	o := e.order(5_000)
	address := e.addr(o)
	txid := e.node.Pay(address, 5_000)
	cur := store.Cursor{BlockHash: e.node.Tip(), Height: e.node.Height(), At: e.clk.Now()}
	if _, err := e.st.RecordScan(bg, []store.TXO{{TxID: txid, Address: address, AmountSat: 5_000}}, cur); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.settle(bg, o); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fwd(txid, 0); ok || len(e.events(o)) != 0 {
		t.Fatalf("an unmarked output was recorded: %+v", e.events(o))
	}
	if err := e.st.AddMark(bg, store.Mark{TxID: txid, Kind: store.MarkMempool}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.settle(bg, o); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fwd(txid, 0); !ok || e.eventCount(o, store.EventPaid) != 1 {
		t.Fatalf("a mempool output was not recorded: %+v", e.events(o))
	}
	e.safety(o)
}

// --- the peer rail (Monero) ---

// stubPeer is the Monero rail as a peer whose view a test changes; onView, when set, runs after View has read its
// answer and before it returns it, with the call's number (1 for the first call after it was set).
type stubPeer struct {
	mu     sync.Mutex
	view   rail.View
	ok     bool
	onView func(n int)
	calls  int
	seqs   []int // what RevertedSeqs answers: the seqs of the peer's reversals
}

// RevertedSeqs answers the seqs a test set (setSeqs).
func (p *stubPeer) RevertedSeqs(context.Context, store.Order) ([]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seqs, nil
}

func (p *stubPeer) setSeqs(seqs ...int) {
	p.mu.Lock()
	p.seqs = seqs
	p.mu.Unlock()
}

func (p *stubPeer) View(context.Context, store.Order) (rail.View, bool, error) {
	p.mu.Lock()
	v, ok := p.view, p.ok
	p.calls++
	n, f := p.calls, p.onView
	p.mu.Unlock()
	if f != nil {
		f(n)
	}
	return v, ok, nil
}

func (p *stubPeer) set(v rail.View) {
	p.mu.Lock()
	p.view, p.ok = v, true
	p.mu.Unlock()
}

func (p *stubPeer) setOnView(f func(n int)) {
	p.mu.Lock()
	p.onView, p.calls = f, 0
	p.mu.Unlock()
}

// withMonero makes an order with on-chain and Monero amounts (1 XMR).
func (e *env) withMonero(sat int64) store.Order {
	e.t.Helper()
	o, _, err := e.st.CreateOrder(bg, store.Order{AccountID: "acct-1", MerchantRef: fmt.Sprintf("ref-%d", orderSeq.Add(1)),
		AmountSat: sat, AmountPiconero: 1_000_000_000_000, ExpiresAt: e.clk.Now().Add(time.Hour)})
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

// The mirror of the Monero rail's case: an order with on-chain and Monero amounts, nothing on-chain. The peer pending
// (its counted live sum at its amount, not paid) holds the final event; the peer partial gives order.underpaid once,
// and when the peer stored it first, between this settle's read of the events and its write, AddEvent's false is no
// error; the peer paid makes the order read paid, and an on-chain record then logs the both-rails ALERT; the peer with
// late money and an on-chain record logs it too.
func TestPeerHoldsTheFinalEvent(t *testing.T) {
	t.Run("pending, then partial", func(t *testing.T) {
		e := newWatchEnv(t)
		p := &stubPeer{}
		e.svc.SetPeer(p, nil)
		o := e.withMonero(100_000)
		e.addr(o)
		p.set(rail.View{Received: 1_000_000_000_000})
		e.pastExpiry(o)
		e.passes(2)
		if len(e.events(o)) != 0 {
			t.Fatalf("the peer pending: %+v", e.events(o))
		}
		p.set(rail.View{Received: 1})
		e.passes(2)
		if e.eventCount(o, store.EventUnderpaid) != 1 || len(e.events(o)) != 1 {
			t.Fatalf("the peer partial: %+v", e.events(o))
		}
		e.safety(o)
	})
	t.Run("partial, the peer's event stored first", func(t *testing.T) {
		e := newWatchEnv(t)
		p := &stubPeer{}
		e.svc.SetPeer(p, nil)
		o := e.withMonero(100_000)
		e.addr(o)
		p.set(rail.View{Received: 1})
		read := e.svc.eventsFor
		e.svc.eventsFor = func(ctx context.Context, id string) ([]store.Event, error) {
			l, err := read(ctx, id)
			if id == o.ID {
				if _, err := e.st.AddEvent(ctx, store.Event{Type: store.EventUnderpaid, OrderID: id, Seq: 1}); err != nil {
					t.Error(err)
				}
			}
			return l, err
		}
		e.pastExpiry(o)
		if err := e.pass(); err != nil {
			t.Fatal(err)
		}
		if e.eventCount(o, store.EventUnderpaid) != 1 || e.count(alertUnderpaid) != 0 || e.count(logSettleFailed) != 0 || e.rail(o).Stale {
			t.Fatalf("events %+v\n%s", e.events(o), e.log)
		}
		e.safety(o)
	})
	t.Run("paid on the peer, then an on-chain record", func(t *testing.T) {
		e := newWatchEnv(t)
		p := &stubPeer{}
		e.svc.SetPeer(p, nil)
		o := e.withMonero(100_000)
		a := e.addr(o)
		p.set(rail.View{Received: 1_000_000_000_000, Confirmed: 1_000_000_000_000, Paid: true})
		e.pastExpiry(o)
		e.passes(2)
		if e.eventCount(o, store.EventPaid) != 1 || len(e.events(o)) != 1 || e.count(alertBoth) != 0 {
			t.Fatalf("paid on the peer: %+v", e.events(o))
		}
		e.node.Pay(a, 10_000)
		e.node.Mine(1)
		e.passes(2)
		if e.count(alertBoth) != 1 {
			t.Fatalf("an on-chain record after the peer paid:\n%s", e.log)
		}
		e.safety(o)
	})
	t.Run("late money on the peer, then an on-chain record", func(t *testing.T) {
		e := newWatchEnv(t)
		p := &stubPeer{}
		e.svc.SetPeer(p, nil)
		o := e.withMonero(100_000)
		a := e.addr(o)
		p.set(rail.View{Late: 1})
		e.node.Pay(a, 10_000)
		e.node.Mine(1)
		e.passes(2)
		if e.count(alertBoth) != 1 {
			t.Fatalf("money on two rails, neither paid:\n%s", e.log)
		}
		e.safety(o)
	})
	t.Run("a reorg under the peer's payment gives no order.reverted", func(t *testing.T) {
		e := newWatchEnv(t)
		p := &stubPeer{}
		e.svc.SetPeer(p, nil)
		o := e.withMonero(100_000)
		txid := e.payOrder(o, 100_000)
		e.node.Mine(1)
		e.pass()
		p.set(rail.View{Received: 1_000_000_000_000, Confirmed: 1_000_000_000_000, Paid: true})
		e.node.Invalidate(1)
		e.pass()
		if _, ok := e.rev(txid, 0); !ok || e.eventCount(o, store.EventReverted) != 0 || e.eventCount(o, store.EventPaid) != 1 {
			t.Fatalf("events %+v", e.events(o))
		}
		e.safety(o)
	})
}

// A peer that turns Received > 0 between the settle's first read and its re-read after the creation: the ALERT fires.
func TestBothRailsSimultaneousPeer(t *testing.T) {
	e := newWatchEnv(t)
	p := &stubPeer{}
	e.svc.SetPeer(p, nil)
	o := e.withMonero(100_000)
	e.payOrder(o, 100_000)
	p.set(rail.View{})
	e.pass()
	e.node.Mine(1)
	p.setOnView(func(n int) {
		if n == 1 {
			p.set(rail.View{Received: 1})
		}
	})
	e.pass()
	if e.count(alertBoth) != 1 {
		t.Fatalf("the re-read after the creation missed the peer's money:\n%s", e.log)
	}
	e.safety(o)
}

// Address refuses an order the peer reads paid.
func TestAddressPaidOnThePeerIsRefused(t *testing.T) {
	e := newWatchEnv(t)
	p := &stubPeer{}
	e.svc.SetPeer(p, nil)
	o := e.withMonero(100_000)
	p.set(rail.View{Received: 1, Paid: true})
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrPaid) {
		t.Fatalf("Address: %v", err)
	}
	p.set(rail.View{Received: 1})
	if _, err := e.svc.Address(bg, o.ID); err != nil {
		t.Fatalf("Address with the peer not paid: %v", err)
	}
	e.svc.SetPeer(nil, nil)
	e.safety(o)
}

// Ruling 8-T7d: a stale peer (failing or paused, not paid) holds a mixed order's final event as a pending
// one does, until the watch window's end. Fresh again inside the window with nothing received (the order still listed
// past its expiry): order.expired once. At the window's end a held order is not settled: the left-the-window ALERT
// reports it and no final event is invented, whether the peer is still stale then or turned fresh only at the
// first pass after the window's end.
func TestStalePeerHoldsTheFinalEvent(t *testing.T) {
	const noFinal = logNoFinal
	held := func(t *testing.T) (*env, *stubPeer, store.Order) {
		e := newWatchEnv(t)
		p := &stubPeer{}
		e.svc.SetPeer(p, nil)
		o := e.withMonero(100_000)
		e.addr(o)
		p.set(rail.View{Stale: true})
		e.pastExpiry(o)
		e.passes(3)
		if len(e.events(o)) != 0 {
			t.Fatalf("the peer stale: %+v", e.events(o))
		}
		return e, p, o
	}
	lastSecond := func(e *env, o store.Order) {
		e.clk.Advance(o.ExpiresAt.Add(store.WatchWindow).Add(-time.Second).Sub(e.clk.Now()))
		e.pass()
		if len(e.events(o)) != 0 {
			e.t.Fatalf("a second before the window's end: %+v", e.events(o))
		}
	}
	t.Run("fresh again inside the window, nothing received", func(t *testing.T) {
		e, p, o := held(t)
		l, err := e.st.OnchainOrdersPastExpiry(bg, e.clk.Now())
		if err != nil || !slices.ContainsFunc(l, func(x store.Order) bool { return x.ID == o.ID }) {
			t.Fatalf("the held order is not listed past its expiry: %v %v", l, err)
		}
		p.set(rail.View{})
		e.passes(2)
		if !e.clk.Now().Before(o.ExpiresAt.Add(store.WatchWindow)) {
			t.Fatal("setup: past the window")
		}
		if e.eventCount(o, store.EventExpired) != 1 || len(e.events(o)) != 1 || e.count(noFinal) != 0 {
			t.Fatalf("the peer fresh: %+v", e.events(o))
		}
		e.safety(o)
	})
	t.Run("still stale at the window's end", func(t *testing.T) {
		e, _, o := held(t)
		lastSecond(e, o)
		e.toWindowEnd(o)
		e.pass()
		e.passes(2)
		if len(e.events(o)) != 0 || e.count(noFinal) != 1 {
			t.Fatalf("at the window's end: %+v\n%s", e.events(o), e.log)
		}
		e.safety(o)
	})
	// Ruling 8-T7e: past the window, a late payment makes settle reach the held order; while the peer stays stale it
	// writes no final event, and once the peer is fresh the next settle writes it.
	t.Run("a late payment after the window, the peer stale, then fresh", func(t *testing.T) {
		e, p, o := held(t)
		lastSecond(e, o)
		e.toWindowEnd(o)
		e.pass()
		if len(e.events(o)) != 0 || e.count(noFinal) != 1 {
			t.Fatalf("at the window's end: %+v\n%s", e.events(o), e.log)
		}
		e.node.Pay(e.rail(o).Address, 1_000)
		e.passes(2)
		if len(e.events(o)) != 0 || e.count("ALERT: an on-chain payment arrived after the address's watch window") != 1 {
			t.Fatalf("a late payment, the peer stale: %+v\n%s", e.events(o), e.log)
		}
		p.set(rail.View{})
		e.node.Pay(e.rail(o).Address, 1_000)
		e.passes(2)
		if e.eventCount(o, store.EventExpired) != 1 || len(e.events(o)) != 1 || e.count(noFinal) != 1 {
			t.Fatalf("the peer fresh: %+v\n%s", e.events(o), e.log)
		}
		e.safety(o)
	})
	t.Run("stale at the last pass inside, fresh at the first after", func(t *testing.T) {
		e, p, o := held(t)
		lastSecond(e, o)
		p.set(rail.View{})
		e.toWindowEnd(o)
		e.pass()
		e.passes(2)
		if len(e.events(o)) != 0 || e.count(noFinal) != 1 {
			t.Fatalf("fresh only past the window: %+v\n%s", e.events(o), e.log)
		}
		e.safety(o)
	})
}

// peerSettle models the Monero rail's settle of a reversal on its own goroutine, as service runs it: under the settle
// mutex the rails share it flips the peer's view to unpaid and, when the on-chain rail then reads unpaid (View, the
// path a peer's settle reads under that mutex), writes order.reverted 1 (monero.settle's k = 1); when on-chain still
// reads paid it stamps its reversal 0 and writes nothing. Called inside the on-chain settle's peer read. The goroutine
// first tries the mutex: if it gets it, the settle that called this does not hold it, so the
// reversal runs to its end inside that settle's window and peerSettle waits for it; if not, peerSettle returns at
// once and the goroutine waits on the mutex, so the reversal lands after the settle. No sleep.
func peerSettle(e *env, mu *sync.Mutex, p *stubPeer, o store.Order) (done chan struct{}) {
	done = make(chan struct{})
	got := make(chan bool, 1)
	go func() {
		defer close(done)
		free := mu.TryLock()
		got <- free
		if !free {
			mu.Lock()
		}
		defer mu.Unlock()
		p.set(rail.View{State: rail.Waiting})
		v, _, err := e.svc.View(bg, o)
		if err != nil {
			e.t.Error(err)
			return
		}
		if !v.Paid {
			if _, err := e.st.AddEvent(bg, store.Event{Type: store.EventReverted, OrderID: o.ID, Seq: 1}); err != nil {
				e.t.Error(err)
			}
		}
	}()
	if <-got {
		<-done
	}
	return done
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the peer's settle never ran")
	}
}

// Ruling 8-A1, the on-chain mirror of the Monero rail's case (a false order.paid_again): an order paid in Monero, with
// a partial on-chain payment. The Monero rail reverses its payment during the on-chain settle's peer read; before the
// shared settle mutex the on-chain settle answered its order.reverted 1 with order.paid_again 1 from its stale
// peer.Paid.
func TestSettleSharedLockNoFalsePaidAgainOnchain(t *testing.T) {
	e := newWatchEnv(t)
	p := &stubPeer{}
	mu := &sync.Mutex{}
	e.svc.SetPeer(p, mu)
	o := e.withMonero(100_000)
	p.set(rail.View{})
	a := e.addr(o)
	p.set(rail.View{State: rail.Paid, Received: 1_000_000_000_000, Confirmed: 1_000_000_000_000, Paid: true})
	e.node.Pay(a, 50_000)
	e.node.Mine(1)
	e.passes(2)
	if !e.hasEvent(o, store.EventPaid, 1) || e.eventCount(o, store.EventReverted) != 0 {
		t.Fatalf("setup: %+v", e.events(o))
	}
	var done chan struct{}
	p.setOnView(func(n int) {
		if n == 1 {
			done = peerSettle(e, mu, p, o)
		}
	})
	e.node.Mine(1)
	e.passes(1)
	p.setOnView(nil)
	if done == nil {
		t.Fatal("the on-chain pass did not settle the order")
	}
	waitDone(t, done)
	e.passes(3)
	if e.eventCount(o, store.EventReverted) != 1 || e.hasEvent(o, store.EventPaidAgain, 1) {
		t.Fatalf("events %+v: want order.reverted 1 and no order.paid_again (on-chain 50000 of 100000 sat)", e.events(o))
	}
}

// Ruling 8-A1, the on-chain mirror of the lost order.reverted: an order paid on both rails; both payments revert at
// once (here a reorg on-chain during the on-chain settle's peer read). Serialised, the second settle reads the first's
// reversal, so exactly one order.reverted is written.
func TestSettleSharedLockNoLostRevertedOnchain(t *testing.T) {
	e := newWatchEnv(t)
	p := &stubPeer{}
	mu := &sync.Mutex{}
	e.svc.SetPeer(p, mu)
	o := e.withMonero(100_000)
	p.set(rail.View{})
	a := e.addr(o)
	p.set(rail.View{State: rail.Paid, Received: 1_000_000_000_000, Confirmed: 1_000_000_000_000, Paid: true})
	e.node.Pay(a, 100_000)
	e.node.Mine(1)
	e.passes(2)
	if !e.hasEvent(o, store.EventPaid, 1) {
		t.Fatalf("setup: %+v", e.events(o))
	}
	var done chan struct{}
	p.setOnView(func(n int) {
		if n == 1 {
			done = peerSettle(e, mu, p, o)
		}
	})
	e.node.Invalidate(1) // the on-chain payment back in the mempool: its record (tier 1) reverts
	e.passes(1)
	p.setOnView(nil)
	if done == nil {
		t.Fatal("the on-chain pass did not settle the order")
	}
	waitDone(t, done)
	e.passes(3)
	if r := e.rail(o); r.Paid || e.eventCount(o, store.EventReverted) != 1 {
		t.Fatalf("the order was paid and reads unpaid on every rail (%+v), with %d order.reverted; want 1; events %+v",
			r, e.eventCount(o, store.EventReverted), e.events(o))
	}
}

// Ruling 8-A1: SetPeer installs the settle mutex service shares between the rails; nil keeps the rail's own, and a
// later nil does not undo a shared one.
func TestSetPeerInstallsTheSettleLock(t *testing.T) {
	e := newWatchEnv(t)
	own := e.svc.settleLock()
	if own == nil {
		t.Fatal("no settle mutex of its own")
	}
	e.svc.SetPeer(nil, nil)
	if e.svc.settleLock() != own {
		t.Fatal("SetPeer(nil, nil) replaced the rail's own mutex")
	}
	shared := &sync.Mutex{}
	e.svc.SetPeer(nil, shared)
	if e.svc.settleLock() != shared {
		t.Fatal("SetPeer did not install the shared mutex")
	}
	e.svc.SetPeer(nil, nil)
	if e.svc.settleLock() != shared {
		t.Fatal("SetPeer(nil, nil) dropped the shared mutex")
	}
}

// readDuringSettle reads the rail on its own goroutine from inside a settle (a test hook) and returns once the read
// has answered (answered) or its goroutine waits on a mutex inside Rail (read in the goroutine dump): never a sleep.
// res gets the read's answer whenever it comes.
func readDuringSettle(t *testing.T, read func() (Rail, error)) (answered bool, res chan railRead) {
	t.Helper()
	res = make(chan railRead, 1)
	go func() {
		r, err := read()
		res <- railRead{r, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(res) > 0 {
			return true, res
		}
		if waitsInRail() {
			return false, res
		}
		runtime.Gosched()
	}
	t.Fatal("the read neither answered nor waited on the settle mutex")
	return false, res
}

type railRead struct {
	r   Rail
	err error
}

// waitsInRail reports whether a goroutine is in sync.(*Mutex).Lock called from (*Service).Rail.
func waitsInRail() bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "sync.(*Mutex).Lock") && strings.Contains(g, "/onchain.(*Service).Rail(") {
			return true
		}
	}
	return false
}

// Ruling R1, on-chain: settle creates a new forward before it reverses an old one. An
// order of 100,000 sat paid 60,000 whose transaction is replaced by one that does not pay it (dead), and a second
// 60,000 mined in the same block: the pass records the second (settle:after-transfers), then reverses the first. A
// status read at that hook summed both: Paid. Rail takes the settle mutex, so it reads the settle's end.
func TestRailReadsNoHalfSettleOnchain(t *testing.T) {
	e := newZeroConfEnv(t)
	o := e.order(100_000)
	first := e.node.Pay(e.addr(o), 60_000)
	e.node.Mine(1)
	e.passes(2)
	if _, ok := e.fwd(first, 0); !ok || e.rail(o).Paid {
		t.Fatalf("setup: the first output is not recorded, or the order reads paid: %+v", e.rail(o))
	}
	e.node.Invalidate(1)
	e.node.Replace(first, false)
	e.node.Pay(e.addr(o), 60_000)
	e.node.Mine(1)
	var answered, hooked bool
	var res chan railRead
	e.svc.hook = func(p string) error {
		if p == "settle:after-transfers" && !hooked {
			hooked = true
			answered, res = readDuringSettle(t, func() (Rail, error) { return e.svc.Rail(bg, o) })
		}
		return nil
	}
	e.pass()
	e.svc.hook = nil
	if !hooked {
		t.Fatal("the pass created no transfer")
	}
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.r.Paid || r.r.ConfirmedSat != 60_000 {
		t.Errorf("a read inside the settle (answered there: %v): Paid=%v ConfirmedSat=%d, want unpaid with 60000", answered,
			r.r.Paid, r.r.ConfirmedSat)
	}
	if _, ok := e.rev(first, 0); !ok || e.rail(o).Paid || e.eventCount(o, store.EventPaid) != 0 {
		t.Fatalf("after the pass: reversed %v, %+v, events %+v", ok, e.rail(o), e.events(o))
	}
}

// Ruling R1: View is the path a peer's settle reads under the settle mutex the rails share, so it never takes that
// mutex itself (it would wait on its own caller).
func TestViewUnderTheSettleMutexOnchain(t *testing.T) {
	e := newWatchEnv(t)
	mu := &sync.Mutex{}
	e.svc.SetPeer(nil, mu)
	o := e.order(100_000)
	e.addr(o)
	mu.Lock()
	defer mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, _, err := e.svc.View(bg, o)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("View waited on the settle mutex")
	}
}

// Ruling R2: step 1 of every settle heals the peer rail's stamped reversals too. The peer holds a reversal stamped 1
// whose order.reverted is missing (a crash or a store error between the two databases); this rail's next settle of
// the order adds it, before it derives anything, and only the seqs the peer reports. With no peer (SetPeer(nil)),
// nothing is read.
func TestSettleHealsThePeersReversal(t *testing.T) {
	e := newWatchEnv(t)
	p := &stubPeer{}
	e.svc.SetPeer(p, nil)
	o := e.withMonero(100_000)
	e.payOrder(o, 50_000) // a partial payment, still tracked, so every pass settles the order
	e.node.Mine(1)
	e.passes(2)
	if n := e.eventCount(o, store.EventReverted); n != 0 {
		t.Fatalf("setup: %d order.reverted", n)
	}
	p.setSeqs(1, 1<<21) // 2^21 is above the bound of a SatBooth seq: ignored
	e.passes(1)
	if !e.hasEvent(o, store.EventReverted, 1) || e.eventCount(o, store.EventReverted) != 1 {
		t.Fatalf("the peer's reversal stamped 1 was not healed: %+v", e.events(o))
	}
	p.setSeqs(1, 2)
	e.passes(1)
	if !e.hasEvent(o, store.EventReverted, 2) || e.eventCount(o, store.EventReverted) != 2 {
		t.Fatalf("the peer's reversal stamped 2: %+v", e.events(o))
	}
	e.svc.SetPeer(nil, nil)
	e.passes(1)
	if e.eventCount(o, store.EventReverted) != 2 {
		t.Fatalf("without a peer: %+v", e.events(o))
	}
}

// Ruling S1, on-chain: the replacement case of TestRailReadsNoHalfSettleOnchain,
// interrupted at the first hook after a ledger write. Rail never reads Paid, and the restarted pass publishes no
// order.paid: the new forward and the old one's reversal are one batch.
func TestInterruptedSettleIsNotPaidOnchain(t *testing.T) {
	e := newZeroConfEnv(t)
	o := e.order(100_000)
	first := e.node.Pay(e.addr(o), 60_000)
	e.node.Mine(1)
	e.passes(2)
	if _, ok := e.fwd(first, 0); !ok || e.rail(o).Paid {
		t.Fatalf("setup: the first output is not recorded, or the order reads paid: %+v", e.rail(o))
	}
	e.node.Invalidate(1)
	e.node.Replace(first, false)
	e.node.Pay(e.addr(o), 60_000)
	e.node.Mine(1)
	fired := false
	e.svc.hook = func(p string) error {
		if p == "settle:after-transfers" && !fired {
			fired = true
			return errCrash
		}
		return nil
	}
	err := e.pass()
	e.svc.hook = nil
	if !fired || !errors.Is(err, errCrash) {
		t.Fatalf("no crash in the settle: %v", err)
	}
	if r := e.rail(o); r.Paid || r.ConfirmedSat != 60_000 {
		t.Errorf("after the interrupted settle: Paid=%v ConfirmedSat=%d, want unpaid with 60000", r.Paid, r.ConfirmedSat)
	}
	e.restart()
	e.pass()
	e.passes(2)
	if n := e.eventCount(o, store.EventPaid); n != 0 || e.rail(o).Paid {
		t.Fatalf("after the restart: %d order.paid, %+v, events %+v", n, e.rail(o), e.events(o))
	}
	if _, ok := e.rev(first, 0); !ok {
		t.Fatal("the first output was never reversed")
	}
	e.safety(o)
}

// Ruling S1, on-chain: a batch item that exists with other details is logged and left out, nothing of the batch is
// applied, and the caller retries without it; any other refusal is an error and nothing is applied either.
func TestCreateTransfersLeavesOutADifferentRecordOnchain(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	ud, _ := orderUserData(o.ID)
	txid := strings.Repeat("ab", 32)
	good := book.Forward(PaidTransferID(txid, 0, 0), ud, 50_000, true)
	clash := book.Forward(PaidTransferID(txid, 1, 0), ud, 50_000, true)
	e.transfer(clash.ID, OnchainWallet, MerchantOnchain, o, 30_000, codePaid, 0)
	left := map[ledger.U128]bool{}
	res, retry, err := e.svc.createTransfers(bg, o, []ledger.Transfer{good, clash}, 2, left)
	if err != nil || !retry || res != nil || !left[clash.ID] || e.count(logDifferent) != 1 {
		t.Fatalf("results %v retry %v err %v left %v, %d logDifferent", res, retry, err, left, e.count(logDifferent))
	}
	if _, ok := e.lookup(good.ID); ok {
		t.Fatal("the refused batch wrote its other item")
	}
	res, retry, err = e.svc.createTransfers(bg, o, []ledger.Transfer{good}, 1, left)
	if err != nil || retry || len(res) != 1 || res[0] != ledger.TransferCreated {
		t.Fatalf("the retry: %v %v %v", res, retry, err)
	}
	bad := good
	bad.ID, bad.Ledger = PaidTransferID(txid, 2, 0), 9
	if _, _, err := e.svc.createTransfers(bg, o, []ledger.Transfer{book.Forward(PaidTransferID(txid, 3, 0), ud, 1, true), bad}, 2, left); err == nil {
		t.Fatal("another refusal is no error")
	}
	if _, ok := e.lookup(PaidTransferID(txid, 3, 0)); ok {
		t.Fatal("a batch refused for another reason wrote its other item")
	}
	// Ruling TU1: a reversal (from index nFwd) that exists with other details is an error, not a retry.
	rev := book.Reversal(ReversalTransferID(txid, 4, 0), ud, 50_000, 0)
	e.reverse(o, txid, 4, 0, 30_000)
	fwd := book.Forward(PaidTransferID(txid, 5, 0), ud, 50_000, true)
	before := len(left)
	res, retry, err = e.svc.createTransfers(bg, o, []ledger.Transfer{fwd, rev}, 1, left)
	if err == nil || retry || res != nil || len(left) != before || e.count(logDifferent) != 2 {
		t.Fatalf("a refused reversal: results %v retry %v err %v left %v, %d logDifferent", res, retry, err, left, e.count(logDifferent))
	}
	if _, ok := e.lookup(fwd.ID); ok {
		t.Fatal("the batch with a refused reversal wrote its forward")
	}
}

// Ruling TU1, on-chain: a reversal refused as exists_with_different is never left out. The replacement case of
// TestInterruptedSettleIsNotPaidOnchain, with a reversal under the first output's reversal id but another amount
// inserted after the settle read its views (the peer's View runs then). The settle fails: logDifferent, nothing
// written, no order.paid, and the order reads stale.
func TestRefusedReversalFailsTheSettleOnchain(t *testing.T) {
	e := newZeroConfEnv(t)
	p := &stubPeer{}
	p.set(rail.View{})
	e.svc.SetPeer(p, nil)
	o := e.withMonero(100_000)
	first := e.node.Pay(e.addr(o), 60_000)
	e.node.Mine(1)
	e.passes(2)
	if _, ok := e.fwd(first, 0); !ok || e.rail(o).Paid {
		t.Fatalf("setup: the first output is not recorded, or the order reads paid: %+v", e.rail(o))
	}
	vout := e.txo(first).Vout
	e.node.Invalidate(1)
	e.node.Replace(first, false)
	second := e.node.Pay(e.addr(o), 60_000)
	e.node.Mine(1)
	inserted := false
	p.setOnView(func(int) {
		if !inserted {
			inserted = true
			e.reverse(o, first, vout, 0, 30_000) // the first output's reversal id, another amount
		}
	})
	err := e.pass()
	p.setOnView(nil)
	if err != nil || !inserted {
		t.Fatalf("pass %v, inserted %v", err, inserted)
	}
	if _, ok := e.fwd(second, 0); ok {
		t.Errorf("the second output was recorded although the batch's reversal was refused")
	}
	if e.count(logDifferent) != 1 || !e.logHas(`msg="`+logSettleFailed+`" order=`+o.ID) {
		t.Errorf("want one logDifferent and the settle failed for the order\n%s", e.log)
	}
	if e.eventCount(o, store.EventPaid) != 0 || e.rail(o).Paid {
		t.Errorf("the order reads paid: %+v, events %+v", e.rail(o), e.events(o))
	}
	if !e.rail(o).Stale {
		t.Errorf("the order does not read stale: %+v", e.rail(o))
	}
	// The retry: the next settle reads the conflicting reversal with the views, so the first output's record is
	// foreign and counts for nothing; the order never reads paid.
	e.passes(1)
	if r := e.rail(o); r.Paid || r.ConfirmedSat > 60_000 || e.eventCount(o, store.EventPaid) != 0 || e.count(logDifferent) < 2 {
		t.Fatalf("the next pass: %+v, events %+v, %d logDifferent", r, e.events(o), e.count(logDifferent))
	}
}

// Ruling T3, on-chain: a reversal written by a settle is reflected in its views, so rail.LateRecorded sees it in the
// same call. A late output's forward is committed and the pass crashes before order.late; a reorg then puts the output
// back in the mempool, and the recovery pass, the last inside the window, reverses the forward. It still writes
// order.late; past the window the late output is no longer polled and nothing settles the order again.
func TestLateReversedInRecoveryStillGetsOrderLateOnchain(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.addr(o)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	if !e.hasEvent(o, store.EventExpired, 1) {
		t.Fatalf("setup: no order.expired: %+v", e.events(o))
	}
	e.clk.Advance(o.ExpiresAt.Add(store.WatchWindow).Sub(e.clk.Now()) - 30*time.Second)
	late := e.node.Pay(a, 100_000)
	e.node.Mine(1)
	fired := e.crashAt("settle:after-transfers")
	if err := e.pass(); !errors.Is(err, errCrash) || !*fired {
		t.Fatalf("no crash: %v", err)
	}
	if _, ok := e.fwd(late, 0); !ok || e.hasEvent(o, store.EventLate, 1) {
		t.Fatalf("setup: forward %v, events %+v: want the late forward and no order.late", ok, e.events(o))
	}
	e.node.Invalidate(1)
	e.restart()
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatalf("recovery pass: %v", err)
	}
	if _, ok := e.rev(late, 0); !ok {
		t.Fatal("setup: the recovery pass did not reverse the late forward")
	}
	if !e.hasEvent(o, store.EventLate, 1) || e.count(alertLate) != 1 {
		t.Fatalf("no order.late after the recovery reversed the late forward: %+v", e.events(o))
	}
	e.toWindowEnd(o)
	e.passes(2)
	if e.eventCount(o, store.EventLate) != 1 {
		t.Fatalf("events %+v", e.events(o))
	}
	e.safety(o)
}
