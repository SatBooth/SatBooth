package onchain

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

// --- the per-transaction rule and its exclusivity ---

// lateRows fails unless every stored output of txid is late for o (first seen at or after its expiry).
func (e *env) lateRows(o store.Order, txid string) {
	e.t.Helper()
	n := 0
	for _, x := range e.txos(o) {
		if x.TxID == txid {
			n++
			if x.FirstSeen.Before(o.ExpiresAt) {
				e.t.Fatalf("%s:%d is counted (first-seen %v, expiry %v)", txid[:8], x.Vout, x.FirstSeen, o.ExpiresAt)
			}
		}
	}
	if n == 0 {
		e.t.Fatalf("%s has no stored output", txid[:8])
	}
}

// 50,000 on time, bumped after the expiry into two outputs of 50,000. The bump's total (100,000) is not
// the original's, so neither output inherits: both are late, and the order is never paid.
func TestSplitBumpIsLate(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	ad := e.addr(o)
	a := e.node.Pay(ad, 50_000)
	e.pass()
	e.pastExpiry(o)
	b := e.node.ReplaceWith(a, []onchaintest.Out{{Address: ad, Sat: 50_000}, {Address: ad, Sat: 50_000}})
	e.pass()
	e.lateRows(o, b)
	e.node.Mine(1)
	e.pass()
	if r := e.rail(o); r.Paid || r.ConfirmedSat != 0 || !e.hasEvent(o, store.EventLate, 1) || e.eventCount(o, store.EventPaid) != 0 {
		t.Fatalf("Rail %+v, events %+v", r, e.events(o))
	}
	e.safety(o)
}

// A (50,000) bumped to b (50,000, inherits), then b split into two outputs of 50,000: c's total
// matches no ancestor's, so c is late.
func TestChainThenSplitIsLate(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	ad := e.addr(o)
	a := e.node.Pay(ad, 50_000)
	e.pass()
	first := e.txo(a).FirstSeen
	e.pastExpiry(o)
	b := e.node.ReplaceWith(a, []onchaintest.Out{{Address: ad, Sat: 50_000}})
	e.pass()
	if !e.txo(b).FirstSeen.Equal(first) {
		t.Fatalf("b did not inherit: %v", e.txo(b).FirstSeen)
	}
	c := e.node.ReplaceWith(b, []onchaintest.Out{{Address: ad, Sat: 50_000}, {Address: ad, Sat: 50_000}})
	e.pass()
	e.lateRows(o, c)
	e.node.Mine(1)
	e.pass()
	if r := e.rail(o); r.Paid || r.ConfirmedSat != 0 || e.eventCount(o, store.EventPaid) != 0 {
		t.Fatalf("Rail %+v, events %+v", r, e.events(o))
	}
	e.safety(o)
}

// The multi-input variant of the per-transaction rule: A spends two inputs and pays 100,000 on time; after the expiry B
// spends one input and C the other, each paying 100,000. B and C do not conflict, so both can be mined: only the first
// to be decided inherits, the other is late, whether C comes in a later scan or in the same one.
func TestMultiInputReplacementInheritsOnce(t *testing.T) {
	for _, sameScan := range []bool{false, true} {
		t.Run(fmt.Sprint("same scan ", sameScan), func(t *testing.T) {
			e := newWatchEnv(t)
			o := e.order(100_000)
			ad := e.addr(o)
			a := e.node.PayWith(2, []onchaintest.Out{{Address: ad, Sat: 100_000}})
			e.pass()
			first := e.txo(a).FirstSeen
			in := e.node.Inputs(a)
			e.pastExpiry(o)
			b := e.node.Spend(in[:1], []onchaintest.Out{{Address: ad, Sat: 100_000}})
			if !sameScan {
				e.pass()
			}
			c := e.node.Spend(in[1:], []onchaintest.Out{{Address: ad, Sat: 100_000}})
			e.pass()
			if !e.txo(b).FirstSeen.Equal(first) {
				t.Fatalf("b did not inherit: %v", e.txo(b).FirstSeen)
			}
			e.lateRows(o, c)
			e.node.Mine(1)
			e.pass()
			fb, okB := e.fwd(b, 0)
			fc, okC := e.fwd(c, 0)
			if !okB || fb.Code != codePaid || !okC || fc.Code != codeLate || e.rail(o).ConfirmedSat != 100_000 || e.eventCount(o, store.EventPaid) != 1 {
				t.Fatalf("b %+v %v, c %+v %v, Rail %+v", fb, okB, fc, okC, e.rail(o))
			}
			e.safety(o)
		})
	}
}

// The counterexample to same-time exclusivity (Ruling 6.1-U): A spends two inputs and pays 100,000 on time; before the
// expiry A2 spends one of them and pays 60,000 (its own on-time first-seen: another total); after the expiry X spends
// the other and pays 100,000. X's total is A's, but A2 is on time and does not conflict with X: both could confirm, so
// X is late. On time, at most 100,000 was ever committed; 60,000 is counted.
func TestPartialDoubleSpendBeforeExpiryThenBumpIsLate(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	ad := e.addr(o)
	a := e.node.PayWith(2, []onchaintest.Out{{Address: ad, Sat: 100_000}})
	e.pass()
	in := e.node.Inputs(a)
	e.clk.Advance(time.Minute)
	a2 := e.node.Spend(in[1:], []onchaintest.Out{{Address: ad, Sat: 60_000}})
	e.pass()
	if !e.txo(a2).FirstSeen.Before(o.ExpiresAt) {
		t.Fatal("setup: A2 is late")
	}
	e.pastExpiry(o)
	x := e.node.Spend(in[:1], []onchaintest.Out{{Address: ad, Sat: 100_000}})
	e.pass()
	e.lateRows(o, x)
	e.node.Mine(1)
	e.pass()
	if fx, ok := e.fwd(x, 0); !ok || fx.Code != codeLate || e.rail(o).ConfirmedSat != 60_000 || e.rail(o).Paid {
		t.Fatalf("x %+v %v, Rail %+v", fx, ok, e.rail(o))
	}
	e.safety(o)
}

// The cost of Ruling 6.1-U: two independent on-time payments of 50,000, the first bumped after the expiry. The other is
// on time and does not conflict with the bump, so the bump is late.
func TestTwoOnTimePartsBumpOneIsLate(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	ad := e.addr(o)
	p1 := e.node.Pay(ad, 50_000)
	e.pass()
	e.clk.Advance(time.Minute)
	e.node.Pay(ad, 50_000)
	e.pass()
	e.pastExpiry(o)
	b := e.node.ReplaceWith(p1, []onchaintest.Out{{Address: ad, Sat: 50_000}})
	e.pass()
	e.lateRows(o, b)
	e.node.Mine(1)
	e.pass()
	if r := e.rail(o); r.ConfirmedSat != 50_000 || r.Paid {
		t.Fatalf("Rail %+v", r)
	}
	e.safety(o)
}

// Past the window, an inherited bump settles its dead original without the ALERT; the bump is then
// double spent away and mined over: its own death is reported, once. The bump comes a minute before the window end,
// so it inherits (decision 9: only inside the window).
func TestDeadInheritedBumpAlerts(t *testing.T) {
	e := newWatchEnv(t)
	e.node.Mine(1)
	o := e.order(100_000)
	ad := e.addr(o)
	a := e.node.Pay(ad, 100_000)
	e.pass()
	e.pastExpiry(o)
	e.pass()
	e.clk.Advance(o.ExpiresAt.Add(store.WatchWindow).Add(-time.Minute).Sub(e.clk.Now()))
	b := e.node.ReplaceWith(a, []onchaintest.Out{{Address: ad, Sat: 100_000}})
	e.pass()
	e.toWindowEnd(o)
	e.pass()
	if !e.hasEvent(o, store.EventUnderpaid, 1) || e.count(alertLeftPool) != 0 || !e.watched(b) {
		t.Fatalf("after the bump: %d ALERTs, watched %v", e.count(alertLeftPool), e.watched(b))
	}
	e.node.Replace(b, false)
	e.node.Mine(1)
	e.passes(3)
	if m, _ := e.mark(b); m.Kind != store.MarkDead || e.watched(b) || e.count(alertLeftPool) != 1 {
		t.Fatalf("the bump double spent: %+v, watched %v, %d ALERTs\n%s", m, e.watched(b), e.count(alertLeftPool), e.log)
	}
	e.safety(o)
}

// pmodel is what the property test knows of a run: each transaction it made, its inputs and its total to the address.
type pmodel struct {
	e      *env
	ad     string
	txs    []string
	total  map[string]int64
	inputs map[string][]string
}

func (m *pmodel) add(txid string, outs []onchaintest.Out) {
	if txid == "" {
		return
	}
	var sum int64
	for _, o := range outs {
		if o.Address == m.ad {
			sum += o.Sat
		}
	}
	m.txs = append(m.txs, txid)
	m.total[txid] = sum
	m.inputs[txid] = m.e.node.Inputs(txid)
}

func (m *pmodel) disjoint(a, b string) bool {
	for _, i := range m.inputs[a] {
		for _, j := range m.inputs[b] {
			if i == j {
				return false
			}
		}
	}
	return true
}

// maxTogether is the largest sum of val over a set of the txids whose inputs are pairwise disjoint: what can confirm
// together. Brute force over the subsets.
func (m *pmodel) maxTogether(ids []string, val map[string]int64) int64 {
	var best int64
	for mask := 0; mask < 1<<len(ids); mask++ {
		var sum int64
		ok := true
		for i := 0; i < len(ids) && ok; i++ {
			if mask&(1<<i) == 0 {
				continue
			}
			for j := 0; j < i && ok; j++ {
				if mask&(1<<j) != 0 && !m.disjoint(ids[i], ids[j]) {
					ok = false
				}
			}
			sum += val[ids[i]]
		}
		if ok && sum > best {
			best = sum
		}
	}
	return best
}

// outs draws one or two outputs to the address: the same total as want, split in two, more, or less.
func (m *pmodel) outs(rng *rand.Rand, want int64) []onchaintest.Out {
	pool := []int64{25_000, 50_000, 60_000, 100_000}
	switch rng.Intn(5) {
	case 0, 1:
		return []onchaintest.Out{{Address: m.ad, Sat: want}}
	case 2:
		return []onchaintest.Out{{Address: m.ad, Sat: want / 2}, {Address: m.ad, Sat: want - want/2}}
	case 3:
		return []onchaintest.Out{{Address: m.ad, Sat: pool[rng.Intn(len(pool))]}, {Address: m.ad, Sat: pool[rng.Intn(len(pool))]}}
	}
	return []onchaintest.Out{{Address: m.ad, Sat: pool[rng.Intn(len(pool))]}}
}

// op makes one random spend of the transactions so far: a bump of one (ReplaceWith, same inputs), a spend of part of
// its inputs, or a spend merging the inputs of two.
func (m *pmodel) op(rng *rand.Rand) {
	t := m.txs[rng.Intn(len(m.txs))]
	in := m.inputs[t]
	switch rng.Intn(3) {
	case 0:
		outs := m.outs(rng, m.total[t])
		m.add(m.e.node.ReplaceWith(t, outs), outs)
	case 1:
		k := rng.Intn(len(in))
		sub := in[k : k+1]
		if len(in) == 1 || rng.Intn(4) == 0 {
			sub = in
		}
		outs := m.outs(rng, m.total[t])
		m.add(m.e.node.Spend(sub, outs), outs)
	default:
		u := m.txs[rng.Intn(len(m.txs))]
		merged := append(append([]string(nil), in...), m.inputs[u]...)
		outs := m.outs(rng, m.total[t])
		m.add(m.e.node.Spend(merged, outs), outs)
	}
}

// burst spends each input of one transaction on its own, at its total to the address, with no pass between: replacements
// that each could inherit and do not conflict with each other, so the scan that lists them decides them together
// (inherit's in-scan exclusivity).
func (m *pmodel) burst(rng *rand.Rand) {
	t := m.txs[rng.Intn(len(m.txs))]
	for _, in := range m.inputs[t] {
		outs := []onchaintest.Out{{Address: m.ad, Sat: m.total[t]}}
		m.add(m.e.node.Spend([]string{in}, outs), outs)
	}
}

// propertySeed is the seed of the property test: SATBOOTH_INHERIT_SEED when set (to rotate it), else 26.
func propertySeed(t *testing.T) int64 {
	t.Helper()
	v := os.Getenv("SATBOOTH_INHERIT_SEED")
	if v == "" {
		return 26
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatalf("SATBOOTH_INHERIT_SEED: %v", err)
	}
	return n
}

// The invariant of the per-transaction rule: whatever the payer does after the expiry (bumps, splits,
// chains, spends of part of the inputs, merges, and bursts of these in one scan), the largest on-time sum that can
// confirm together never exceeds the largest that the transactions SatBooth saw before the expiry could. Before the
// expiry the generator also replaces payments with other totals, so the run holds conflicting on-time transactions.
func TestInheritanceNeverExceedsTheOnTimeCommitment(t *testing.T) {
	seed := propertySeed(t)
	t.Logf("seed %d", seed)
	rng := rand.New(rand.NewSource(seed))
	inherited := 0
	const runs = 120
	for run := 0; run < runs; run++ {
		e := newWatchEnv(t)
		e.node.Mine(1)
		o := e.order(100_000)
		m := &pmodel{e: e, ad: e.addr(o), total: map[string]int64{}, inputs: map[string][]string{}}
		for k := 1 + rng.Intn(3); k > 0; k-- {
			outs := m.outs(rng, []int64{25_000, 50_000, 100_000}[rng.Intn(3)])
			m.add(e.node.PayWith(1+rng.Intn(2), outs), outs)
			if rng.Intn(2) == 0 {
				e.pass()
			}
		}
		for k := rng.Intn(3); k > 0; k-- {
			m.op(rng)
			if rng.Intn(2) == 0 {
				e.pass()
			}
		}
		e.pass()
		// Committed on time: the transactions SatBooth stored before the expiry, at their totals to the address.
		var onTime []string
		for _, id := range m.txs {
			if _, err := e.st.TXO(bg, store.TXOKey{TxID: id}); err == nil {
				onTime = append(onTime, id)
			}
		}
		committed := m.maxTogether(onTime, m.total)
		e.pastExpiry(o)
		pastWindow := rng.Intn(4) == 0 // a quarter of the runs make the changes after the watch window (decision 9)
		if pastWindow {
			e.toWindowEnd(o)
		}
		oneScan := rng.Intn(2) == 0 // half the runs make every change after the expiry before one scan
		for k := 1 + rng.Intn(4); k > 0; k-- {
			if rng.Intn(3) == 0 {
				m.burst(rng)
			} else {
				m.op(rng)
			}
			if !oneScan && rng.Intn(2) == 0 {
				e.pass()
			}
		}
		e.pass()
		e.node.Mine(1)
		e.passes(2)
		// Counted: each stored transaction's outputs first seen before the expiry.
		counted := map[string]int64{}
		var ids []string
		for _, x := range e.txos(o) {
			if !x.FirstSeen.Before(o.ExpiresAt) {
				continue
			}
			if counted[x.TxID] == 0 {
				ids = append(ids, x.TxID)
			}
			counted[x.TxID] += x.AmountSat
		}
		late := map[string]bool{}
		for _, id := range onTime {
			late[id] = true
		}
		for _, id := range ids {
			if !late[id] {
				if pastWindow {
					t.Fatalf("run %d: %s, first stored past the window, inherited an on-time first-seen\n%s", run, id, e.log)
				}
				inherited++
				break
			}
		}
		if got := m.maxTogether(ids, counted); got > committed {
			t.Fatalf("run %d: %d counted on time can confirm together, %d committed on time\n%s", run, got, committed, e.log)
		}
		if r := e.rail(o); r.ConfirmedSat > committed {
			t.Fatalf("run %d: Rail %+v, committed %d", run, r, committed)
		}
		e.safety(o)
	}
	if inherited < runs/10 {
		t.Fatalf("only %d of %d runs inherited: the generator does not exercise the rule", inherited, runs)
	}
	t.Logf("%d of %d runs inherited", inherited, runs)
}
