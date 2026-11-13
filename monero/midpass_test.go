package monero

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/monero/monerotest"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/xmrrpc"
)

// Ruling 8-A6, the staged pass: the wallet changes while a pass reads it. afterHeight
// runs once, right after the pass's get_height has answered: what the wallet does between that read and
// discover/track.
type afterHeight struct {
	RPC
	once sync.Once
	f    func()
}

func (a *afterHeight) GetHeight(ctx context.Context) (int64, error) {
	h, err := a.RPC.GetHeight(ctx)
	if err == nil {
		a.once.Do(a.f)
	}
	return h, err
}

// The wallet's height drops after the pass's get_height (h0 -> h0-2, a new 2 XMR payment, tier 3
// confirmations, mined at h0-3). wallet-rpc's own count is 1; the pass used to count from the stale h0. The end check
// sees the height below h0 and discards the pass.
func TestHeightDropsAfterGetHeight(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(30)
	o := e.order(2 * xmr)
	a := e.addr(o)
	e.pass()
	h0 := e.w.Height()
	var txid string
	e.svc.rpc = &afterHeight{RPC: e.rpc, f: func() {
		e.w.PopDrop(3)
		txid = e.w.Pay(a, 2*xmr)
		e.w.Mine(1)
	}}
	e.clk.Advance(time.Second)
	err := e.pass()
	e.svc.rpc = e.rpc
	wh, _ := e.w.GetHeight(bg)
	m, _ := e.mark(o, txid)
	c := e.cursor()
	t.Logf("pass=%v h0=%d wallet now=%d payment height=%d cursor=%+v need=%d", err, h0, wh, m.Height, c,
		rail.Required(e.svc.cfg.Tiers, o.AmountPiconero))
	if _, ok := e.fwd(o, txid, 0); ok || e.view(o).Paid || e.eventCount(o, store.EventPaid) != 0 {
		t.Errorf("paid at wallet-rpc's %d confirmation(s) of 3: forward %v, view %+v, order.paid %d", wh-m.Height,
			ok, e.view(o), e.eventCount(o, store.EventPaid))
	}
	// The end check's own rule (the bound on confirmations alone would also have kept it unpaid): the pass is
	// discarded with the height reason, and nothing of it is stored, not even the new payment or the cursor.
	if !errors.Is(err, errDiscarded) || !e.logHas(`msg="`+logDiscarded+`" reason=height`) || len(e.payments(o)) != 0 ||
		c.WalletHeight != h0 {
		t.Errorf("pass %v, %d payments stored, cursor %+v: want discarded for its height, nothing stored", err,
			len(e.payments(o)), c)
	}
}

// Permanent settlement: the height drops by 10 after get_height; a payment with 1 real confirmation used
// to be stored as settled and never polled again, so when the longer chain won back and it left, nothing noticed.
// The pass is discarded (nothing stored, nothing settled); the payment leaves before a pass commits it.
func TestHeightDropsAfterGetHeightNotSettled(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(30)
	o := e.order(2 * xmr)
	a := e.addr(o)
	e.pass()
	var txid string
	e.svc.rpc = &afterHeight{RPC: e.rpc, f: func() {
		e.w.PopDrop(10)
		txid = e.w.Pay(a, 2*xmr)
		e.w.Mine(1)
	}}
	e.clk.Advance(time.Second)
	e.pass()
	e.svc.rpc = e.rpc
	wh, _ := e.w.GetHeight(bg)
	m, _ := e.mark(o, txid)
	settled, err := e.st.MoneroSettledKeys(bg, []store.PaymentKey{{TxID: txid, Address: a}})
	if err != nil || settled[store.PaymentKey{TxID: txid, Address: a}] {
		t.Errorf("a payment with %d real confirmation(s) was stored as settled (depth %d, cursor %+v, mark %+v): %v",
			wh-m.Height, rail.SettledDepth(e.svc.cfg.Tiers), e.cursor(), m, err)
	}
	e.w.PopDrop(1)
	e.w.Mine(15)
	e.passes(2)
	if e.view(o).Paid {
		t.Errorf("the payment left the chain, the order still reads paid; reversed %v, order.reverted %d",
			e.hasRev(o, txid), e.eventCount(o, store.EventReverted))
	}
}

// After the pass's identity read and get_height, wallet-rpc switches to wallet B and a press detects it
// (walletChanged: base cleared, watchable false). The running pass continues against B, which answers -8 (an empty
// list) for wallet A's txids; its end check sees B (and the generation moved), so nothing it read is written: no dead
// mark, no reversal, no order.reverted. When A is back the order is still paid, with no reverted/paid_again pair.
func TestPauseDuringRunningPass(t *testing.T) {
	e := newWatchEnv(t, nil)
	paid := e.order(xmr)
	txid := e.payOrder(paid, xmr)
	e.w.Mine(1)
	e.pass()
	if !e.view(paid).Paid || !e.watched(paid, txid) {
		t.Fatalf("setup: %+v watched %v", e.view(paid), e.watched(paid, txid))
	}
	wrong := monerotest.New(8)
	wrong.Mine(100)
	for range 5 {
		wrong.CreateAddress(bg, 0, "")
	}
	var pressErr error
	e.svc.rpc = &afterHeight{RPC: e.rpc, f: func() {
		e.svc.rpc = &override{RPC: wrong} // wallet-rpc now serves wallet B
		_, pressErr = e.svc.Address(bg, e.order(xmr).ID)
	}}
	e.clk.Advance(time.Second)
	err := e.pass()
	e.svc.rpc = e.rpc
	if !errors.Is(pressErr, errWalletChanged) || e.svc.watchable.Load() || e.svc.baseAddress() != "" {
		t.Fatalf("setup: the press did not pause: %v watchable %v", pressErr, e.svc.watchable.Load())
	}
	m, _ := e.mark(paid, txid)
	t.Logf("pass=%v press=%v B get_transfer_by_txid calls=%d mark=%+v", err, pressErr,
		wrong.Calls("get_transfer_by_txid"), m)
	if m.Kind == store.MarkDead || e.hasRev(paid, txid) || e.eventCount(paid, store.EventReverted) != 0 ||
		!e.view(paid).Paid {
		t.Errorf("the running pass wrote after the pause: mark %v, reversed %v, order.reverted %d, view %+v",
			m.Kind, e.hasRev(paid, txid), e.eventCount(paid, store.EventReverted), e.view(paid))
	}
	if !errors.Is(err, errPaused) && !errors.Is(err, errDiscarded) {
		t.Errorf("the pass that read wallet B ended with %v, want it discarded", err)
	}
	// Wallet A comes back.
	e.passes(2)
	if !e.view(paid).Paid || e.eventCount(paid, store.EventPaid) != 1 || e.eventCount(paid, store.EventReverted) != 0 ||
		e.eventCount(paid, store.EventPaidAgain) != 0 {
		t.Errorf("after A is back: paid %v, order.paid %d, order.reverted %d, order.paid_again %d", e.view(paid).Paid,
			e.eventCount(paid, store.EventPaid), e.eventCount(paid, store.EventReverted), e.eventCount(paid, store.EventPaidAgain))
	}
}

// The residual of a cheap end check: with B's dead marks committed before the check, wallet
// A coming back while catching up (its height below the cursor, the payment not yet rescanned) answers errNotKnown,
// writes no mark, and settle used to reverse A's payment on B's stale dead mark. Under the staged pass the pass that
// read B commits nothing (the press moved the generation, and the end identity read sees B), so no dead mark exists
// when A comes back behind: the payment stays recorded and the order paid.
func TestDiscardedPassLeavesNoDeadMark(t *testing.T) {
	e := newWatchEnv(t, nil)
	paid := e.order(xmr)
	txid := e.payOrder(paid, xmr)
	e.w.Mine(1)
	e.pass()
	pm, _ := e.mark(paid, txid)
	if !e.view(paid).Paid || pm.Kind != store.MarkMined {
		t.Fatalf("setup: %+v %+v", e.view(paid), pm)
	}
	wrong := monerotest.New(8)
	wrong.Mine(100)
	for range 5 {
		wrong.CreateAddress(bg, 0, "")
	}
	e.svc.rpc = &afterHeight{RPC: e.rpc, f: func() {
		e.svc.rpc = &override{RPC: wrong}
		e.svc.Address(bg, e.order(xmr).ID)
	}}
	e.clk.Advance(time.Second)
	err := e.pass()
	e.svc.rpc = e.rpc
	m, _ := e.mark(paid, txid)
	if !errors.Is(err, errPaused) && !errors.Is(err, errDiscarded) {
		t.Fatalf("the pass that read wallet B ended with %v, want it discarded", err)
	}
	if m.Kind != store.MarkMined || m.Seq != pm.Seq || e.hasRev(paid, txid) {
		t.Fatalf("the discarded pass wrote: mark %+v, reversed %v", m, e.hasRev(paid, txid))
	}
	c := e.cursor()
	e.w.SetHeight(pm.Height) // A restored behind: below the cursor, without the payment yet
	e.w.ForgetPayments(pm.Height - 1)
	e.clk.Advance(time.Second)
	err = e.pass()
	m2, _ := e.mark(paid, txid)
	if e.hasRev(paid, txid) || !e.view(paid).Paid || m2.Kind != store.MarkMined {
		t.Errorf("cursor %+v, A height %d, pass=%v, mark %v, reversed %v, order.reverted %d, paid %v", c, pm.Height, err,
			m2.Kind, e.hasRev(paid, txid), e.eventCount(paid, store.EventReverted), e.view(paid).Paid)
	}
}

// afterTxid runs once, right after a get_transfer_by_txid has answered.
type afterTxid struct {
	RPC
	once sync.Once
	f    func()
}

func (a *afterTxid) GetTransferByTxID(ctx context.Context, txid string, account uint32) ([]xmrrpc.Transfer, error) {
	l, err := a.RPC.GetTransferByTxID(ctx, txid, account)
	if err == nil {
		a.once.Do(a.f)
	}
	return l, err
}

// Against the end check alone: the height drops after get_height (h0 -> h0-9, a payment mined at h0-10),
// track reads it there, then the longer chain wins back before the pass ends (the payment gone, the height above h0).
// The end check sees a height >= h0 and the same wallet, so the pass commits; the mark carries wallet-rpc's own count
// (1), so the payment is neither recorded nor settled at 10 counted confirmations, and the next pass sees it dead.
// A 2 XMR order (tier 3): Rail's bound keeps the payment from being recorded at 1 confirmation.
func TestHeightDropsAndRecoversWithinPass(t *testing.T) { heightDropsAndRecovers(t, 2*xmr) }

// The same on a tier-1 order: the payment is recorded at wallet-rpc's 1 confirmation, and the
// committed wallet height is 11 above its block, so only the settled rule's "the observed count reaches the depth"
// (markConfirmations) keeps it from being stored settled while it is not in the wallet's chain.
func TestHeightDropsAndRecoversWithinPassTierOne(t *testing.T) { heightDropsAndRecovers(t, xmr/2) }

func heightDropsAndRecovers(t *testing.T, amount int64) {
	e := newWatchEnv(t, nil)
	e.w.Mine(30)
	o := e.order(amount)
	a := e.addr(o)
	e.pass()
	h0 := e.w.Height()
	var txid string
	var endHeight int64
	tx := &afterTxid{RPC: e.rpc, f: func() {
		e.w.PopDrop(1) // the block holding the payment leaves; the payment is dropped
		e.w.Mine(11)
		endHeight, _ = e.w.GetHeight(bg)
	}}
	e.svc.rpc = &afterHeight{RPC: tx, f: func() {
		e.w.PopDrop(10)
		txid = e.w.Pay(a, amount)
		e.w.Mine(1)
	}}
	e.clk.Advance(time.Second)
	err := e.pass()
	e.svc.rpc = e.rpc
	t.Logf("pass=%v h0=%d height at the last RPC's end=%d (>= h0: an end check passes)", err, h0, endHeight)
	if endHeight < h0 {
		t.Fatalf("setup: end height %d < h0 %d", endHeight, h0)
	}
	if !e.watched(o, txid) {
		t.Errorf("stored as settled (no longer polled) although the payment is not in the wallet's chain")
	}
	e.passes(2)
	if e.view(o).Paid {
		t.Errorf("the payment is gone, the order still reads paid; reversed %v", e.hasRev(o, txid))
	}
}

// Ruling 8-A6, the identity half of the end check alone: after the pass's get_height wallet-rpc serves wallet B, and
// no press runs (the generation does not move). B answers -8 for A's txid; the end identity read sees B, pauses the
// rail (walletChanged) and the pass writes nothing: no dead mark, no reversal.
func TestEndIdentityReadDiscardsAPass(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Mine(1)
	e.pass()
	pm, _ := e.mark(o, txid)
	wrong := monerotest.New(8)
	wrong.Mine(100)
	e.svc.rpc = &afterHeight{RPC: e.rpc, f: func() { e.svc.rpc = &override{RPC: wrong} }}
	e.clk.Advance(time.Second)
	err := e.pass()
	e.svc.rpc = e.rpc
	m, _ := e.mark(o, txid)
	if !errors.Is(err, errPaused) || !e.logHas(`msg="`+logDiscarded+`" reason=address`) || e.svc.watchable.Load() {
		t.Fatalf("pass %v, watchable %v: want paused by the end identity read\n%s", err, e.svc.watchable.Load(), e.log)
	}
	if m.Seq != pm.Seq || e.hasRev(o, txid) || !e.view(o).Paid {
		t.Fatalf("the pass wrote: mark %+v, reversed %v, view %+v", m, e.hasRev(o, txid), e.view(o))
	}
	e.passes(2) // A again: Check passes, nothing changed
	if !e.view(o).Paid || e.eventCount(o, store.EventReverted) != 0 {
		t.Fatalf("after A is back: %+v %+v", e.view(o), e.events(o))
	}
}

// Ruling 8-A6, the generation half alone: wallet-rpc serves B after the pass's get_height, a press sees B (walletChanged
// moves the generation), and wallet-rpc serves A again before the pass's end (A, B, A with a press in between). The end
// identity read sees A, but the generation moved: the pass commits nothing (reason generation).
func TestGenerationDiscardsAPass(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Mine(1)
	e.pass()
	pm, _ := e.mark(o, txid)
	wrong := monerotest.New(8)
	wrong.Mine(100)
	for range 5 {
		wrong.CreateAddress(bg, 0, "")
	}
	back := &afterTxid{RPC: e.rpc} // A again, once B has answered for the payment
	b := &afterTxid{RPC: &override{RPC: wrong}}
	b.f = func() { e.svc.rpc = back }
	e.svc.rpc = &afterHeight{RPC: e.rpc, f: func() {
		e.svc.rpc = b
		if _, err := e.svc.Address(bg, e.order(xmr).ID); !errors.Is(err, errWalletChanged) {
			t.Errorf("setup: the press did not see B: %v", err)
		}
	}}
	e.clk.Advance(time.Second)
	err := e.pass()
	e.svc.rpc = e.rpc
	m, _ := e.mark(o, txid)
	if !errors.Is(err, errDiscarded) || !e.logHas(`msg="`+logDiscarded+`" reason=generation`) {
		t.Fatalf("pass %v: want discarded for the generation\n%s", err, e.log)
	}
	if m.Seq != pm.Seq || e.hasRev(o, txid) || !e.view(o).Paid {
		t.Fatalf("the pass wrote: mark %+v, reversed %v, view %+v", m, e.hasRev(o, txid), e.view(o))
	}
}

// Discarded passes are a WARN each; the third in a row also logs logDiscardedRepeat once, until a pass commits.
func TestRepeatedDiscardsAlertOnce(t *testing.T) {
	e := newWatchEnv(t, nil)
	e.w.Mine(30)
	e.pass()
	drop := func() {
		e.svc.rpc = &afterHeight{RPC: e.rpc, f: func() {
			wh, _ := e.w.GetHeight(bg)
			e.w.SetHeight(wh - 1)
		}}
		e.clk.Advance(time.Second)
		if err := e.pass(); !errors.Is(err, errDiscarded) {
			t.Fatalf("pass %v, want discarded", err)
		}
		e.svc.rpc = e.rpc
	}
	for range 4 {
		drop()
	}
	if e.count(logDiscarded) != 4 || e.count(logDiscardedRepeat) != 1 {
		t.Fatalf("%d discarded lines, %d repeat ALERTs; want 4 and 1\n%s", e.count(logDiscarded), e.count(logDiscardedRepeat), e.log)
	}
	e.w.SetHeight(-1) // the chain's height again, above the cursor
	e.passes(1)       // commits: the count starts again
	for range 3 {
		drop()
	}
	if e.count(logDiscardedRepeat) != 2 {
		t.Fatalf("%d repeat ALERTs after a commit and three more discards, want 2", e.count(logDiscardedRepeat))
	}
}

// A payment found already SettledDepth deep is recorded and settled in the pass that finds it, judged on its stored
// row: a late one (first seen after the expiry) keeps its late forward, and the settled rule reads the same row, not
// the listing's entry without its first-seen time (ruling 8-A6: the new payment is tracked before it is stored).
func TestLateFoundDeepSettlesInOnePass(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	a := e.addr(o)
	e.pass()
	e.pastExpiry(o)
	e.clk.Advance(time.Second)
	txid := e.w.Pay(a, xmr)
	e.w.Mine(rail.SettledDepth(e.svc.cfg.Tiers) + 1)
	if err := e.pass(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	f, ok := e.fwd(o, txid, 0)
	if !ok || f.Code != codeLate || !e.hasEvent(o, store.EventLate, 1) {
		t.Fatalf("forward %+v %v, events %+v: want the late forward", f, ok, e.events(o))
	}
	if e.watched(o, txid) {
		t.Fatalf("a recorded payment %d deep is still watched after the pass that found it", rail.SettledDepth(e.svc.cfg.Tiers)+1)
	}
	e.safety(o)
}

// The generation is read before step 0: a press that sees another wallet while the pass's own Check runs (Check then
// sets the base again when it passes) still discards the pass.
func TestGenerationReadBeforeTheCheck(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	e.payOrder(o, xmr)
	e.w.Mine(1)
	e.svc.setBase("") // the next pass runs Check
	once := true
	e.rpc.set(func(r *override) {
		r.getAddr = func(account uint32, ix []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
			if once && err == nil && len(ix) == 1 && ix[0] == 0 {
				once = false
				e.svc.walletChanged() // what a press that saw another wallet does
			}
			return a, err
		}
	})
	e.clk.Advance(time.Second)
	if err := e.pass(); !errors.Is(err, errDiscarded) || !e.logHas(`msg="`+logDiscarded+`" reason=generation`) {
		t.Fatalf("pass %v: want discarded for the generation\n%s", err, e.log)
	}
	if len(e.payments(o)) != 0 {
		t.Fatalf("the discarded pass stored %d payments", len(e.payments(o)))
	}
}

// The bound min(observed, WalletHeight − height) applies only below the settled depth. A settled payment
// (observed at the depth) shows WalletHeight − height, so its count keeps growing across passes; a payment below the
// depth stays capped at wallet-rpc's own count.
func TestSettledConfirmationsKeepGrowing(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	depth := rail.SettledDepth(e.svc.cfg.Tiers)
	e.w.Mine(depth)
	e.passes(2)
	if e.watched(o, txid) {
		t.Fatalf("setup: not settled at %d confirmations", depth)
	}
	var seen []int
	for range 3 {
		e.w.Mine(5)
		e.passes(1)
		seen = append(seen, e.view(o).Confirmations)
	}
	if seen[0] != depth+5 || seen[1] != depth+10 || seen[2] != depth+15 {
		t.Fatalf("a settled payment's confirmations across passes %v, want %d, %d, %d", seen, depth+5, depth+10, depth+15)
	}

	// Below the depth: the payment track saw on a shorter chain (wallet-rpc's count 1) reads 1, not WalletHeight − height.
	e2 := newWatchEnv(t, nil)
	e2.w.Mine(30)
	o2 := e2.order(2 * xmr)
	a := e2.addr(o2)
	e2.pass()
	tx := &afterTxid{RPC: e2.rpc, f: func() {
		e2.w.PopDrop(1)
		e2.w.Mine(11)
	}}
	e2.svc.rpc = &afterHeight{RPC: tx, f: func() {
		e2.w.PopDrop(10)
		e2.w.Pay(a, 2*xmr)
		e2.w.Mine(1)
	}}
	e2.clk.Advance(time.Second)
	if err := e2.pass(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	e2.svc.rpc = e2.rpc
	if c := e2.view(o2).Confirmations; c != 1 {
		t.Fatalf("a payment below the depth with wallet-rpc's count 1 reads %d confirmations, want 1", c)
	}
}

// Ruling S5: a settled payment's count is unbounded because it is settled (the stored
// fact), not because its stored count reached today's deepest tier. Settled at depth 10, then restarted with a deepest
// tier of 20: its count keeps growing.
func TestSettledCountGrowsAfterTheDepthRises(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	depth := rail.SettledDepth(e.svc.cfg.Tiers)
	e.w.Mine(depth)
	e.passes(2)
	if e.watched(o, txid) {
		t.Fatalf("setup: not settled at %d confirmations", depth)
	}
	deeper := []rail.Tier{{UpTo: xmr, Confirmations: 1}, {UpTo: 10 * xmr, Confirmations: 3}, {UpTo: 0, Confirmations: 2 * depth}}
	e.mod = func(c *Config) { c.PollEvery, c.CallTimeout, c.Tiers = MinPollEvery, MinCallTimeout, deeper }
	e.restart()
	if rail.SettledDepth(e.svc.cfg.Tiers) != 2*depth {
		t.Fatalf("setup: the restarted depth is %d", rail.SettledDepth(e.svc.cfg.Tiers))
	}
	var seen []int
	for range 2 {
		e.w.Mine(5)
		e.passes(1)
		seen = append(seen, e.view(o).Confirmations)
	}
	if seen[0] != depth+5 || seen[1] != depth+10 {
		t.Fatalf("a settled payment's confirmations after the depth rose: %v, want %d, %d", seen, depth+5, depth+10)
	}
}

// Ruling S3: FirstSeen is the local clock when discovery first sees the payment, staged
// and stored by the commit. A payment discovered a second before the expiry whose pass ends after it (the clock moves
// while track waits on wallet-rpc) is on time: counted, recorded with code 2, no order.late.
func TestFirstSeenAtDiscovery(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	a := e.addr(o)
	e.pass()
	e.clk.Advance(o.ExpiresAt.Sub(e.clk.Now()) - time.Second)
	txid := e.w.Pay(a, xmr)
	e.w.Mine(1)
	e.svc.rpc = &afterTxid{RPC: e.rpc, f: func() { e.clk.Advance(2 * time.Second) }}
	err := e.pass()
	e.svc.rpc = e.rpc
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if !e.clk.Now().After(o.ExpiresAt) {
		t.Fatalf("setup: the pass ended at %v, not after the expiry %v", e.clk.Now(), o.ExpiresAt)
	}
	ps := e.payments(o)
	if len(ps) != 1 || !ps[0].FirstSeen.Before(o.ExpiresAt) {
		t.Fatalf("payments %+v: want one first seen before the expiry %v", ps, o.ExpiresAt)
	}
	f, ok := e.fwd(o, txid, 0)
	if !ok || f.Code != codePaid || e.hasEvent(o, store.EventLate, 1) || !e.view(o).Paid {
		t.Fatalf("forward %+v %v, events %+v, view %+v: want an on-time payment", f, ok, e.events(o), e.view(o))
	}
	e.safety(o)
}

// Ruling S4: the commit records the orders it touched as pending settle, and a pass
// settles them whatever their window. An expired order (order.expired) gets a late payment found already deep; the
// pass crashes between the commit and settle, and restarts after the window closed. The payment is no longer watched
// (late, past the window) and its row is stored (discovery skips it), so only the pending row brings the order back:
// the late forward and order.late still happen.
func TestLateDeepCrashAfterCommitSettlesPastWindow(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	a := e.addr(o)
	e.pass()
	e.pastExpiry(o)
	e.passes(1)
	if !e.hasEvent(o, store.EventExpired, 1) {
		t.Fatalf("setup: no order.expired: %+v", e.events(o))
	}
	e.clk.Advance(time.Second)
	late := e.w.Pay(a, xmr)
	e.w.Mine(rail.SettledDepth(e.svc.cfg.Tiers) + 1)
	fired := e.crashAt("scan:after-record")
	if err := e.pass(); !errors.Is(err, errCrash) || !*fired {
		t.Fatalf("no crash: %v", err)
	}
	if len(e.payments(o)) != 1 {
		t.Fatal("setup: the late payment was not committed")
	}
	if _, ok := e.fwd(o, late, 0); ok {
		t.Fatal("setup: recorded before the crash point")
	}
	e.toWindowEnd(o)
	e.clk.Advance(time.Second)
	if e.watched(o, late) {
		t.Fatal("setup: the late payment is still watched past the window")
	}
	e.restart()
	e.pass()
	f, ok := e.fwd(o, late, 0)
	if !ok || f.Code != codeLate || !e.hasEvent(o, store.EventLate, 1) {
		t.Fatalf("the late payment was never settled: forward %+v %v, events %+v", f, ok, e.events(o))
	}
	e.passes(2)
	if _, ok := e.fwd(o, late, 1); ok || e.eventCount(o, store.EventLate) != 1 {
		t.Fatalf("settled twice: %+v", e.events(o))
	}
	e.safety(o)
}

// Ruling S2: walletChanged takes the settle mutex for the generation bump, and the pass
// holds that mutex once, across the generation check, the commit and every settle of the pass. So a walletChanged
// that starts while the pass holds it (here: right after the commit) waits until the pass's settles end, instead of
// moving the generation between the check and the commit, or between the commit and the settles; once it returns, the
// rail is paused and the next pass commits nothing until a Check passes.
func TestWalletChangedWaitsForThePass(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Mine(1)
	var heldAtCommit, waitedInSettle, inSettle bool
	done := make(chan struct{})
	started := false
	e.svc.hook = func(p string) error {
		switch {
		case p == "scan:after-record" && !started:
			started = true
			mu := e.svc.settleLock()
			if mu.TryLock() {
				mu.Unlock()
			} else {
				heldAtCommit = true
			}
			go func() {
				e.svc.walletChanged()
				close(done)
			}()
		case p == "settle:after-event" && started && !inSettle:
			inSettle = true
			mu := e.svc.settleLock()
			if !mu.TryLock() {
				select {
				case <-done:
				default:
					waitedInSettle = true
				}
			} else {
				mu.Unlock()
			}
		}
		return nil
	}
	e.clk.Advance(time.Second)
	err := e.pass()
	e.svc.hook = nil
	<-done
	if err != nil || !started || !inSettle {
		t.Fatalf("pass %v, hooks: commit %v settle %v", err, started, inSettle)
	}
	if !heldAtCommit {
		t.Errorf("the settle mutex was free right after the commit: walletChanged can move the generation before the settles")
	}
	if !waitedInSettle {
		t.Errorf("walletChanged was not waiting while the pass settled")
	}
	if _, ok := e.fwd(o, txid, 0); !ok || !e.view(o).Paid {
		t.Fatalf("the pass's settle did not complete: %+v", e.view(o))
	}
	if e.svc.watchable.Load() || e.svc.baseAddress() != "" {
		t.Fatal("walletChanged did not pause the rail")
	}
}

// Ruling S2: a walletChanged after the pass's end identity read (which still saw the same wallet) and before the
// commit moves the generation, and the commit discards the pass.
func TestWalletChangedAfterTheEndReadDiscards(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	txid := e.payOrder(o, xmr)
	e.w.Mine(1)
	e.pass()
	pm, _ := e.mark(o, txid)
	e.w.Mine(1)
	calls := 0
	e.rpc.set(func(r *override) {
		r.getAddr = func(account uint32, ix []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
			if err == nil && len(ix) == 1 && ix[0] == 0 {
				if calls++; calls == 2 { // the end identity read (the first is sameWallet's)
					e.svc.walletChanged()
				}
			}
			return a, err
		}
	})
	e.clk.Advance(time.Second)
	err := e.pass()
	e.rpc.set(func(r *override) { r.getAddr = nil })
	if !errors.Is(err, errDiscarded) || !e.logHas(`msg="`+logDiscarded+`" reason=generation`) {
		t.Fatalf("pass %v: want discarded for the generation\n%s", err, e.log)
	}
	if m, _ := e.mark(o, txid); m.Seq != pm.Seq {
		t.Fatalf("the discarded pass wrote a mark: %+v", m)
	}
}

// Ruling T3: a reversal written by a settle is reflected in its views, so the
// historical-event checks (rail.LateRecorded) see it in the same call. A late payment's forward is committed and the
// pass crashes before order.late; a reorg then takes the payment out, and the recovery pass, the last inside the
// window, reverses the forward. It still writes order.late (a late payment was recorded) before the pending row is
// cleared; past the window nothing settles the order again.
func TestLateReversedInRecoveryStillGetsOrderLate(t *testing.T) {
	e := newWatchEnv(t, nil)
	o := e.order(xmr)
	a := e.addr(o)
	e.pass()
	e.pastExpiry(o)
	e.passes(1)
	if !e.hasEvent(o, store.EventExpired, 1) {
		t.Fatalf("setup: no order.expired: %+v", e.events(o))
	}
	e.clk.Advance(o.ExpiresAt.Add(store.WatchWindow).Sub(e.clk.Now()) - 30*time.Second)
	late := e.w.Pay(a, xmr)
	e.w.Mine(1)
	fired := e.crashAt("settle:after-transfers")
	if err := e.pass(); !errors.Is(err, errCrash) || !*fired {
		t.Fatalf("no crash: %v", err)
	}
	if _, ok := e.fwd(o, late, 0); !ok || e.hasEvent(o, store.EventLate, 1) {
		t.Fatalf("setup: forward %v, events %+v: want the late forward and no order.late", ok, e.events(o))
	}
	e.w.PopDrop(1)
	e.w.MineEmpty(2)
	e.restart()
	e.clk.Advance(time.Second)
	if err := e.pass(); err != nil {
		t.Fatalf("recovery pass: %v", err)
	}
	if _, ok := e.rev(o, late, 0); !ok {
		t.Fatal("setup: the recovery pass did not reverse the late forward")
	}
	if !e.hasEvent(o, store.EventLate, 1) || e.count(logLate) != 1 {
		t.Fatalf("no order.late after the recovery reversed the late forward: %+v", e.events(o))
	}
	e.toWindowEnd(o)
	e.passes(2)
	if e.eventCount(o, store.EventLate) != 1 {
		t.Fatalf("events %+v", e.events(o))
	}
	e.safety(o)
}
