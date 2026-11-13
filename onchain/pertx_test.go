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

// manyOuts is n outputs of sat each, all to addr.
func manyOuts(addr string, n int, sat int64) []onchaintest.Out {
	l := make([]onchaintest.Out, n)
	for i := range l {
		l[i] = onchaintest.Out{Address: addr, Sat: sat}
	}
	return l
}

// markOf is the current mark of one output.
func (e *env) markOf(txid string, vout uint32) (store.Mark, bool) {
	e.t.Helper()
	m, ok, err := e.st.CurrentMark(bg, store.TXOKey{TxID: txid, Vout: vout})
	if err != nil {
		e.t.Fatal(err)
	}
	return m, ok
}

// The outputs of one transaction share its reads: one gettransaction and one getmempoolentry for 300 outputs, and
// one gettransaction once it is mined.
func TestOneGettransactionPerTransaction(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(500_000)
	tx := e.node.PayWith(1, manyOuts(e.addr(o), 300, 1_000))
	g, p := e.node.Calls("gettransaction"), e.node.Calls("getmempoolentry")
	e.mustPass()
	if e.node.Calls("gettransaction")-g != 1 || e.node.Calls("getmempoolentry")-p != 1 {
		t.Fatalf("not shared: %d gettransaction, %d getmempoolentry", e.node.Calls("gettransaction")-g, e.node.Calls("getmempoolentry")-p)
	}
	e.node.Mine(1)
	g = e.node.Calls("gettransaction")
	e.mustPass()
	if e.node.Calls("gettransaction")-g != 1 {
		t.Fatal("not shared once mined")
	}
	for v := range uint32(300) {
		if m, _ := e.markOf(tx, v); m.Kind != store.MarkMined {
			t.Fatal(v)
		}
	}
	e.safety(o)
}

// A shared answer still gets each output's own checks, and one output's bad check does not reach the others: of two
// outputs of one transaction, the one whose amount differs from its row is skipped with its order stale, and the other
// is mined, recorded and not stale, whichever of the two is read first.
func TestSharedReadKeepsPerOutputChecks(t *testing.T) {
	for _, bad := range []uint32{0, 1} {
		t.Run(fmt.Sprintf("bad vout %d", bad), func(t *testing.T) {
			e := newWatchEnv(t)
			orders := []store.Order{e.order(10_000), e.order(20_000)}
			amounts := []int64{10_000, 20_000}
			tx := e.node.PayWith(1, []onchaintest.Out{{Address: e.addr(orders[0]), Sat: amounts[0]},
				{Address: e.addr(orders[1]), Sat: amounts[1]}})
			e.svc.rpc = &badTx{RPC: e.node, txid: tx, f: func(t *btcrpc.Transaction) {
				for i := range t.Outputs {
					if t.Outputs[i].Vout == bad {
						t.Outputs[i].ValueSat++
					}
				}
			}}
			e.node.Mine(1)
			before := e.node.TxCalls(tx)
			e.mustPass()
			good := 1 - bad
			if n := e.count(logBadTx); n != 1 {
				t.Fatalf("%d bad-transaction lines\n%s", n, e.log)
			}
			if m, ok := e.markOf(tx, good); !ok || m.Kind != store.MarkMined {
				t.Fatalf("the good output's mark: %+v %v", m, ok)
			}
			if _, ok := e.lookup(PaidTransferID(tx, good, 0)); !ok {
				t.Fatal("the good output is not recorded")
			}
			if e.rail(orders[good]).Stale {
				t.Fatal("the good output's order is stale")
			}
			if !e.rail(orders[bad]).Stale {
				t.Fatal("the bad output's order is not stale")
			}
			if _, ok := e.markOf(tx, bad); ok {
				t.Fatal("the bad output has a mark")
			}
			if _, ok := e.lookup(PaidTransferID(tx, bad, 0)); ok {
				t.Fatal("the bad output is recorded")
			}
			if n := e.node.TxCalls(tx) - before; n != 1 {
				t.Fatalf("%d gettransaction calls", n)
			}
		})
	}
}

// One transaction takes one tick of the pacing, however many outputs it has: 40 outputs at 10 a second take one tick
// (0.1 s) and the time to record them, not four seconds of ticks.
func TestSharedReadTakesOneTick(t *testing.T) {
	e := newEnv(t)
	e.mod = func(c *Config) { c.PollRate = 10 }
	e.svc = e.newService()
	e.ready()
	o := e.order(500_000)
	tx := e.node.PayWith(1, manyOuts(e.addr(o), 40, 1_000))
	start := time.Now()
	e.mustPass()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("a pass over one transaction took %v", d)
	}
	for v := range uint32(40) {
		if _, ok := e.markOf(tx, v); !ok {
			t.Fatalf("output %d has no mark", v)
		}
	}
}

// A failed shared read costs one call and one log line for all of a transaction's outputs.
func TestSharedReadFailureCostsOneCall(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(500_000)
	tx := e.node.PayWith(1, manyOuts(e.addr(o), 50, 1_000))
	e.node.FailTx(tx, &btcrpc.Error{Code: -5})
	before := e.node.TxCalls(tx)
	e.pass()
	if n := e.node.TxCalls(tx) - before; n != 1 {
		t.Fatalf("%d gettransaction calls", n)
	}
	if n := e.count(logOutputFailed); n != 1 {
		t.Fatalf("%d failure lines", n)
	}
	if !e.rail(o).Stale {
		t.Fatal("not stale")
	}
	for v := range uint32(50) {
		if _, ok := e.markOf(tx, v); ok {
			t.Fatalf("output %d has a mark", v)
		}
	}
}

// An output keeps its own call order when another output of the transaction has already read it. Of two outputs of an
// evicted transaction, one is dead already (a crash after its mark) and one is not. In the next pass the transaction is
// mined between the second output's reads: its second gettransaction is a fresh call, it sees the block, and the
// mark is mined, not dead. The dead output, read before the block, stays dead for this pass.
func TestSharedReadsKeepEachOutputsCallOrder(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(500_000)
	tx := e.node.PayWith(1, manyOuts(e.addr(o), 2, 1_000))
	e.mustPass()
	e.node.Evict(tx)
	fired := e.crashAt("track:after-mark")
	if err := e.pass(); !errors.Is(err, errCrash) || !*fired {
		t.Fatalf("no crash: %v", err)
	}
	dead, live := -1, -1
	for v := range uint32(2) {
		if m, _ := e.markOf(tx, v); m.Kind == store.MarkDead {
			dead = int(v)
		} else {
			live = int(v)
		}
	}
	if dead < 0 || live < 0 {
		t.Fatalf("setup: dead %d, other %d", dead, live)
	}
	e.restart()
	n := 0
	e.svc.hook = func(p string) error {
		if p == "track:before-mempool" {
			if n++; n == 2 {
				e.node.Rebroadcast(tx)
				e.node.Mine(1)
			}
		}
		return nil
	}
	g := e.node.Calls("gettransaction")
	e.mustPass()
	if n != 2 {
		t.Fatalf("setup: %d outputs reached the mempool reads", n)
	}
	if m, _ := e.markOf(tx, uint32(live)); m.Kind != store.MarkMined {
		t.Fatalf("the output that was not dead: %+v", m)
	}
	if c := e.node.Calls("gettransaction") - g; c != 2 {
		t.Fatalf("%d gettransaction calls", c)
	}
	e.safety(o)
}
