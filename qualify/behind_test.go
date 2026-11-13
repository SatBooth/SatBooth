//go:build qualify

package qualify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/service"
	"github.com/satbooth/satbooth/store"
)

// lagRPC is the service's real bitcoind client with a wallet that can be made to lag. While frozen, the wallet calls
// the rail reads (gettransaction, listsinceblock) answer what they last answered before the freeze, which is what a
// wallet that has not yet processed its node's mempool events looks like to SatBooth; every other call is live,
// including the node's (InMempool, MempoolLoaded, SpentInMempool, GetBlockchainInfo). It is safe for concurrent use.
//
// No block may be mined while frozen: a real wallet call waits for the chain tip, so the lag would not be the one of a
// busy disk. thaw fails the test if the height changed while frozen.
type lagRPC struct {
	onchain.RPC
	t *testing.T

	mu          sync.Mutex
	frozen      bool
	txs         map[string]btcrpc.Transaction // the last live answer of GetTransaction per txid
	since       btcrpc.SinceBlock             // the last live answer of ListSinceBlock
	haveSince   bool
	frozenAt    int64 // the height when freeze ran
	heightMoved int64 // a different height seen by GetBlockchainInfo while frozen (0: none)
}

func newLagRPC(t *testing.T, inner onchain.RPC) *lagRPC {
	return &lagRPC{RPC: inner, t: t, txs: map[string]btcrpc.Transaction{}}
}

// freeze starts the lag. prime are txids whose live answer is read first, so that the frozen answer for them is the
// one of this moment (a txid the rail has not asked about before would otherwise be answered live).
func (l *lagRPC) freeze(prime ...string) {
	l.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, id := range prime {
		tx, err := l.RPC.GetTransaction(ctx, id)
		if err != nil {
			l.t.Fatalf("lagRPC.freeze: reading %s before the freeze: %v", id, shownErr(err))
		}
		l.mu.Lock()
		l.txs[id] = tx
		l.mu.Unlock()
	}
	bi, err := l.RPC.GetBlockchainInfo(ctx)
	if err != nil {
		l.t.Fatalf("lagRPC.freeze: getblockchaininfo: %v", shownErr(err))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.frozen {
		l.t.Fatal("lagRPC.freeze: already frozen")
	}
	l.frozen, l.frozenAt, l.heightMoved = true, bi.Blocks, 0
}

// thaw ends the lag and fails the test if a block was mined while it lasted.
func (l *lagRPC) thaw() {
	l.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bi, err := l.RPC.GetBlockchainInfo(ctx)
	l.mu.Lock()
	was, at, moved := l.frozen, l.frozenAt, l.heightMoved
	l.frozen = false
	l.mu.Unlock()
	if !was {
		l.t.Fatal("lagRPC.thaw: not frozen")
	}
	if err != nil {
		l.t.Fatalf("lagRPC.thaw: getblockchaininfo: %v", shownErr(err))
	}
	if bi.Blocks != at || moved != 0 {
		l.t.Fatalf("a block was mined while the wallet was frozen: height %d at the freeze, %d now, %d seen by the service in between (0: none); the phase must not mine while frozen", at, bi.Blocks, moved)
	}
}

func (l *lagRPC) GetTransaction(ctx context.Context, txid string) (btcrpc.Transaction, error) {
	l.mu.Lock()
	if l.frozen {
		if tx, ok := l.txs[txid]; ok {
			l.mu.Unlock()
			return tx, nil
		}
	}
	l.mu.Unlock()
	tx, err := l.RPC.GetTransaction(ctx, txid)
	if err == nil {
		l.mu.Lock()
		if !l.frozen { // an answer that arrives after the freeze is not "before the freeze"
			l.txs[txid] = tx
		}
		l.mu.Unlock()
	}
	return tx, err
}

func (l *lagRPC) ListSinceBlock(ctx context.Context, blockhash string, targetConfirmations int) (btcrpc.SinceBlock, error) {
	l.mu.Lock()
	if l.frozen && l.haveSince {
		sb := l.since
		l.mu.Unlock()
		return sb, nil
	}
	l.mu.Unlock()
	sb, err := l.RPC.ListSinceBlock(ctx, blockhash, targetConfirmations)
	if err == nil {
		l.mu.Lock()
		if !l.frozen {
			l.since, l.haveSince = sb, true
		}
		l.mu.Unlock()
	}
	return sb, err
}

func (l *lagRPC) GetBlockchainInfo(ctx context.Context) (btcrpc.BlockchainInfo, error) {
	bi, err := l.RPC.GetBlockchainInfo(ctx)
	if err == nil {
		l.mu.Lock()
		if l.frozen && bi.Blocks != l.frozenAt {
			l.heightMoved = bi.Blocks
		}
		l.mu.Unlock()
	}
	return bi, err
}

// walletBehindLogCount counts the service's log records with this level and message.
func walletBehindLogCount(logs, level, msg string) int {
	n := 0
	for _, l := range strings.Split(logs, "\n") {
		var m struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if json.Unmarshal([]byte(l), &m) == nil && m.Level == level && m.Msg == msg {
			n++
		}
	}
	return n
}

// lagEnv is newOcEnv(t, true) whose service uses a real btcrpc client behind a lagRPC: the same URL, wallet, user and
// password file as the configuration. The password is read in-process from the 0600 file newOcEnv wrote; it is never
// printed.
func lagEnv(t *testing.T) (*ocEnv, *lagRPC) {
	t.Helper()
	e := newOcEnv(t, true)
	pw, err := btcrpc.LoadPassword(filepath.Join(e.root, "rpc.pass"))
	if err != nil {
		t.Fatalf("loading the rpc password file: %v", shownErr(err))
	}
	client, err := btcrpc.New(btcrpc.Config{URL: bitcoindURL, User: os.Getenv("SATBOOTH_RPC_USER"), Wallet: watchWallet,
		Timeout: onchain.DefaultCallTimeout}, pw)
	clear(pw)
	if err != nil {
		t.Fatalf("building the bitcoind client: %v", shownErr(err))
	}
	t.Cleanup(client.Zero) // registered before the service starts, so it runs after the service has stopped
	lag := newLagRPC(t, client)
	e.opts = append(e.opts, service.WithRPC(lag))
	return e, lag
}

// waitBehind sleeps five polls of the service and returns how many WARN records of logWalletBehind the log holds.
func waitBehind(s *svcRun) int {
	time.Sleep(5 * ocPollSeconds * time.Second)
	return walletBehindLogCount(s.logs.String(), "WARN", alWalletBehind)
}

// TestOnchainWalletBehind: a paid 0-confirmation order is bumped (keeping the output) or diverted (a double spend that
// pays another wallet) while the wallet the rail reads is held behind its node. The rule (onchain/watch.go, errBehind):
// the node has dropped the original and names another mempool transaction that spends its inputs, so that payment
// is neither marked gone nor reversed until the wallet shows the conflict; the order stays paid meanwhile, with the
// WARN logged.
func TestOnchainWalletBehind(t *testing.T) {
	e, lag := lagEnv(t)
	s := e.start()

	// W1: bumped keeping the output, with the wallet behind.
	r1, _ := e.newOrder("behind-bumped", satT0, 0, 86400, map[string]rng{store.EventPaid: {1, 1}}, nil)
	addr1, _ := e.press(r1, false)
	tx1, sent := e.pay(addr1, satT0)
	e.waitOC(r1.ID, "paid from the mempool", sent, ocBound, func(o orderJSON) bool {
		return o.State == "paid" && o.Onchain.ConfirmedSat == satT0 && o.Onchain.Confirmations == 0
	})
	e.waitHook(r1.ID, store.EventPaid, hookBound)
	warns := walletBehindLogCount(s.logs.String(), "WARN", alWalletBehind)
	lag.freeze(tx1)
	tx1b, err := e.btc.bumpFee(minerWallet, tx1, nil)
	if err != nil {
		t.Fatalf("bumpfee keeping the output: %v", shownErr(err))
	}
	r1.TxID = tx1b
	e.saveReg()
	e.expectSpender("W1 bumped", tx1, tx1b)
	if n := waitBehind(s); n <= warns {
		t.Errorf("after 5 polls with the wallet behind the log holds %d WARN lines %q, had %d before the bump; want at least one more (the rule's branch did not run against the real gettxspendingprevout)", n, alWalletBehind, warns)
	}
	e.expectPaidOnce("W1 bumped, wallet behind, 5 polls", r1)
	lag.thaw()
	time.Sleep(ocQuiet)
	e.expectPaidOnce("W1 bumped, wallet caught up", r1)
	mined := time.Now()
	e.mine(6)
	e.waitOC(r1.ID, "6 confirmations", mined, ocBound, func(o orderJSON) bool { return o.Onchain.Confirmations >= 6 && o.State == "paid" })
	time.Sleep(ocQuiet) // a pass after the depth settles the output
	if o := e.order(r1.ID); o.State != "paid" || o.Onchain.ConfirmedSat != satT0 || o.Onchain.ReceivedSat != satT0 {
		t.Errorf("W1 after the replacement was mined reads %s, confirmed %d, received %d; want paid and %d once", o.State, o.Onchain.ConfirmedSat, o.Onchain.ReceivedSat, satT0)
	}
	e.expectPaidOnce("W1 bumped, mined 6 deep", r1)

	// W2: diverted (the output taken away), with the wallet behind.
	r2, _ := e.newOrder("behind-diverted", satT0, 0, 86400, map[string]rng{store.EventPaid: {1, 1}, store.EventReverted: {1, 1}}, nil)
	addr2, _ := e.press(r2, false)
	tx2, sent := e.pay(addr2, satT0)
	r2.TxID = tx2
	e.saveReg()
	e.waitOC(r2.ID, "paid from the mempool", sent, ocBound, func(o orderJSON) bool {
		return o.State == "paid" && o.Onchain.ConfirmedSat == satT0 && o.Onchain.Confirmations == 0
	})
	e.waitHook(r2.ID, store.EventPaid, hookBound)
	warns = walletBehindLogCount(s.logs.String(), "WARN", alWalletBehind)
	lag.freeze(tx2)
	m1, m2 := e.newAddr(), e.newAddr()
	diverted := time.Now()
	txD, fallback, bumpErr, err := e.btc.divert(minerWallet, tx2, addr2, m1, m2, 20_000)
	if err != nil {
		t.Fatalf("replacing the payment (bumpfee refused: %v): %v", shownErr(bumpErr), shownErr(err))
	}
	t.Logf("OBSERVED walletbehind divert by bumpfee with outputs: fallback used %v", fallback)
	e.expectSpender("W2 diverted", tx2, txD)
	if n := waitBehind(s); n <= warns {
		t.Errorf("after 5 polls with the wallet behind the log holds %d WARN lines %q, had %d before the divert; want at least one more", n, alWalletBehind, warns)
	}
	if o := e.order(r2.ID); o.State != "paid" || o.Onchain.State != "paid" || o.Onchain.ConfirmedSat != satT0 {
		t.Errorf("W2 diverted, wallet behind, 5 polls: the order reads %s (on-chain %+v); want paid with %d sat confirmed (the rule: judged when the wallet has seen the conflict)", o.State, o.Onchain, satT0)
	}
	if n := e.rcv.count(r2.ID, store.EventReverted); n != 0 {
		t.Errorf("W2 diverted, wallet behind, 5 polls: order.reverted delivered %d times, want 0", n)
	}
	lag.thaw()
	e.waitHook(r2.ID, store.EventReverted, ocBound)
	d, _ := e.waitOC(r2.ID, "reverted to waiting", diverted, ocBound, func(o orderJSON) bool {
		return o.State == "new" && o.Onchain.State == "waiting" && o.Onchain.ReceivedSat == 0 && o.Onchain.ConfirmedSat == 0
	})
	t.Logf("MEASURED onchain=walletbehind divert_to_new_seconds=%.2f (includes the 5 polls held)", d.Seconds())
	if n := e.rcv.countSeq(r2.ID, store.EventReverted, 1); n != 1 {
		t.Errorf("W2: order.reverted seq 1 delivered %d times, want 1", n)
	}
	if n := e.rcv.count(r2.ID, store.EventReverted); n != 1 {
		t.Errorf("W2: order.reverted delivered %d times in all, want 1", n)
	}
	e.mine(1)
	time.Sleep(ocQuiet)
	if o := e.order(r2.ID); o.State != "new" || o.Onchain.ReceivedSat != 0 || o.Onchain.ConfirmedSat != 0 {
		t.Errorf("W2 after the replacement was mined the order reads %s with %d sat received, %d confirmed; want new, 0 and 0", o.State, o.Onchain.ReceivedSat, o.Onchain.ConfirmedSat)
	}
	if n := walletBehindLogCount(s.logs.String(), "WARN", alWalletBehind); n < 1 {
		t.Errorf("the service logged the WARN %q %d times in the phase, want at least once", alWalletBehind, n)
	}
	// One reorg ALERT per order: W2's reversal, and W1's old record reversed when the wallet showed the bump (the
	// unit test TestBehindBumpWhileTheWalletLagsStaysPaid counts alertReorg 1 for the same bump).
	e.finish(s, map[string]rng{alReorg: {2, 2}, "WARN " + alWalletBehind: {1, 1 << 30}})

	e.withStore(func(ctx context.Context, st *store.Store) {
		m1, x1 := marksOf(ctx, t, st, r1.ID)
		if len(x1) != 2 || m1[tx1] != store.MarkDead || m1[tx1b] != store.MarkMined {
			t.Errorf("order W1: outputs %d, marks %v; want the replaced one dead and the replacement mined", len(x1), m1)
		}
		var vout uint32
		for _, x := range x1 {
			if x.TxID == tx1b {
				vout = x.Vout
			}
		}
		if !e.settledOut(store.TXOKey{TxID: tx1b, Vout: vout}) {
			t.Error("W1's replacement, mined 6 deep, is not in onchain_settled")
		}
		m2, x2 := marksOf(ctx, t, st, r2.ID)
		if len(x2) != 1 || m2[tx2] != store.MarkDead {
			t.Errorf("order W2: outputs %d, marks %v; want one, dead", len(x2), m2)
		}
		t.Logf("OBSERVED walletbehind store: W1 %v, W2 %v", m1, m2)
	})
}

// expectSpender asks the node who spends the inputs of old in the mempool (from the watch wallet's verbose
// gettransaction of old) and fails unless it is the replacement.
func (e *ocEnv) expectSpender(label, old, replacement string) {
	e.t.Helper()
	got, err := e.btc.spendersOf(watchWallet, old)
	if err != nil {
		e.t.Fatalf("%s: gettxspendingprevout for the inputs of %s: %v", label, old, shownErr(err))
	}
	if len(got) == 0 {
		e.t.Fatalf("%s: %s has no inputs in the watch wallet's gettransaction", label, old)
	}
	for i, id := range got {
		if id != replacement {
			e.t.Errorf("%s: gettxspendingprevout for input %d of %s names %q, want the replacement %s", label, i, old, id, replacement)
		}
	}
}

// expectPaidOnce is the state of an order that was paid and bumped: paid, confirmed and received the amount once,
// and neither order.reverted nor order.paid_again delivered.
func (e *ocEnv) expectPaidOnce(label string, ro *ocOrder) {
	e.t.Helper()
	if o := e.order(ro.ID); o.State != "paid" || o.Onchain.ConfirmedSat != satT0 || o.Onchain.ReceivedSat != satT0 {
		e.t.Errorf("%s: the order reads %s, confirmed %d, received %d; want paid and %d once", label, o.State, o.Onchain.ConfirmedSat, o.Onchain.ReceivedSat, satT0)
	}
	if n := e.rcv.count(ro.ID, store.EventReverted); n != 0 {
		e.t.Errorf("%s: order.reverted delivered %d times, want 0", label, n)
	}
	if n := e.rcv.count(ro.ID, store.EventPaidAgain); n != 0 {
		e.t.Errorf("%s: order.paid_again delivered %d times, want 0", label, n)
	}
}
