package rail

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
)

const tip = 1000

// The marks of TestOutPredicates.
var marks = []struct {
	name string
	out  Out
}{
	{"unmarked", Out{}},
	{"mempool", Out{Marked: true, Kind: store.MarkMempool}},
	{"mined at tip", Out{Marked: true, Kind: store.MarkMined, Height: tip}},
	{"mined at tip - 2", Out{Marked: true, Kind: store.MarkMined, Height: tip - 2}},
	{"dead", Out{Marked: true, Kind: store.MarkDead, Height: tip - 2}},
}

// The predicates of one output, each mark x counted or late x tier 0, 1 or 3, written as data (settle.go's rules of
// plan 6: a late output needs at least 1 block; a 0 need is recordable from the mempool; a recorded output reverts
// when dead, or when it needs a block and is not mined).
func TestOutPredicates(t *testing.T) {
	live := map[string]bool{"unmarked": true, "mempool": true, "mined at tip": true, "mined at tip - 2": true, "dead": false}
	mined := map[string]bool{"mined at tip": true, "mined at tip - 2": true}
	confs := map[string]int64{"mined at tip": 1, "mined at tip - 2": 3}
	// By need 0, 1, 3.
	recordable := map[string][3]bool{
		"unmarked":         {false, false, false},
		"mempool":          {true, false, false},
		"mined at tip":     {true, true, false},
		"mined at tip - 2": {true, true, true},
		"dead":             {false, false, false},
	}
	reverts := map[string][3]bool{
		"unmarked":         {false, true, true},
		"mempool":          {false, true, true},
		"mined at tip":     {false, false, false},
		"mined at tip - 2": {false, false, false},
		"dead":             {true, true, true},
	}
	needIndex := map[int64]int{0: 0, 1: 1, 3: 2}
	for _, m := range marks {
		for _, counted := range []bool{true, false} {
			for _, tier := range []int64{0, 1, 3} {
				o := m.out
				o.Amount, o.Counted = 50_000, counted
				wantNeed := tier
				if !counted && tier == 0 {
					wantNeed = 1
				}
				name := m.name
				if !counted {
					name += ", late"
				}
				if o.Live() != live[m.name] || o.Mined() != mined[m.name] || o.Confirmations(tip) != confs[m.name] {
					t.Errorf("%s: Live %v Mined %v Confirmations %d", name, o.Live(), o.Mined(), o.Confirmations(tip))
				}
				if got := o.Need(tier); got != wantNeed {
					t.Errorf("%s, tier %d: Need = %d, want %d", name, tier, got, wantNeed)
				}
				i := needIndex[wantNeed]
				if got := o.Recordable(tier, tip); got != recordable[m.name][i] {
					t.Errorf("%s, tier %d: Recordable = %v, want %v", name, tier, got, recordable[m.name][i])
				}
				if o.Reverts(tier) {
					t.Errorf("%s, tier %d: an unrecorded output reverts", name, tier)
				}
				o.Recorded = true
				if got := o.Reverts(tier); got != reverts[m.name][i] {
					t.Errorf("%s, tier %d: Reverts = %v, want %v", name, tier, got, reverts[m.name][i])
				}
			}
		}
	}
	// A mark above the tip (a reorg the cursor has not seen) never gives a negative count.
	if c := (Out{Marked: true, Kind: store.MarkMined, Height: tip + 5}).Confirmations(tip); c != 0 {
		t.Fatalf("mined above the tip: %d", c)
	}
}

func TestAdd(t *testing.T) {
	for _, tc := range [][4]int64{{1, 2, 10, 3}, {5, 5, 10, 10}, {5, 6, 10, 10}, {store.MaxSat, store.MaxSat, store.MaxSat, store.MaxSat},
		{0, 0, 1, 0}, {1<<53 - 2, 1, 1<<53 - 1, 1<<53 - 1}, {1<<53 - 1, 1, 1<<53 - 1, 1<<53 - 1}} {
		if got := Add(tc[0], tc[1], tc[2]); got != tc[3] {
			t.Errorf("Add(%d, %d, %d) = %d, want %d", tc[0], tc[1], tc[2], got, tc[3])
		}
	}
}

func paid(amount int64) Out {
	return Out{Amount: amount, Counted: true, Recorded: true, Code: CodePaid, Marked: true, Kind: store.MarkMined, Height: tip}
}

func TestSums(t *testing.T) {
	late := Out{Amount: 7, Marked: true, Kind: store.MarkMempool}
	deadPaid := paid(100)
	deadPaid.Kind = store.MarkDead
	odd := paid(1_000) // a counted recorded forward with the late code counts for nothing confirmed
	odd.Code = CodeLate
	unrecorded := Out{Amount: 30, Counted: true}
	c, cl, ll := Sums([]Out{paid(10), late, deadPaid, odd, unrecorded}, 1<<40)
	// confirmed: 10 + 100 (recorded, though dead: its reversal is step 4's); counted live: 10 + 1,000 + 30; late: 7.
	if c != 110 || cl != 1_040 || ll != 7 {
		t.Fatalf("Sums = %d, %d, %d", c, cl, ll)
	}
	if c, cl, ll := Sums(nil, 1<<40); c != 0 || cl != 0 || ll != 0 {
		t.Fatal("Sums of nothing")
	}
	// The bound caps each sum.
	l2 := late
	l2.Amount = 60
	c, cl, ll = Sums([]Out{paid(60), paid(60), l2, l2}, 100)
	if c != 100 || cl != 100 || ll != 100 {
		t.Fatalf("capped Sums = %d, %d, %d", c, cl, ll)
	}
}

func TestLeast(t *testing.T) {
	at := func(h int64) Out {
		return Out{Amount: 1, Counted: true, Marked: true, Kind: store.MarkMined, Height: h}
	}
	lateMempool := Out{Amount: 1, Marked: true, Kind: store.MarkMempool}
	deadCounted := Out{Amount: 1, Counted: true, Marked: true, Kind: store.MarkDead}
	for name, tc := range map[string]struct {
		outs []Out
		want int
		ok   bool
	}{
		"none":                    {nil, 0, false},
		"only late":               {[]Out{lateMempool}, 0, false},
		"only dead":               {[]Out{deadCounted}, 0, false},
		"one at tip":              {[]Out{at(tip)}, 1, true},
		"the least of two":        {[]Out{at(tip - 5), at(tip - 2)}, 3, true},
		"late and dead ignored":   {[]Out{lateMempool, deadCounted, at(tip - 2)}, 3, true},
		"unmined among counted":   {[]Out{at(tip - 2), {Amount: 1, Counted: true, Marked: true, Kind: store.MarkMempool}}, 0, false},
		"unmarked among counted":  {[]Out{at(tip - 2), {Amount: 1, Counted: true}}, 0, false},
		"above the tip":           {[]Out{at(tip + 3)}, 0, true},
		"clamped at 2^30":         {[]Out{at(-(1 << 40))}, 1 << 30, true},
		"recorded flag no matter": {[]Out{paid(5)}, 1, true},
	} {
		got, ok := Least(tc.outs, tip)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: Least = %d, %v; want %d, %v", name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestBothRecorded(t *testing.T) {
	lateRec := Out{Amount: 1, Recorded: true, Code: CodeLate}
	foreign := paid(1)
	foreign.Foreign = true
	revCode := paid(1)
	revCode.Code = CodeReversed
	unrec := paid(1)
	unrec.Recorded = false
	for name, tc := range map[string]struct {
		outs []Out
		want bool
	}{
		"none":                {nil, false},
		"a counted record":    {[]Out{paid(1)}, true},
		"a late record":       {[]Out{lateRec}, true},
		"foreign":             {[]Out{foreign}, false},
		"another code":        {[]Out{revCode}, false},
		"not recorded":        {[]Out{unrec}, false},
		"one of several":      {[]Out{unrec, foreign, lateRec}, true},
		"reversed, not again": {[]Out{{Amount: 1, Code: CodePaid, Reversed: true}}, false},
	} {
		if got := BothRecorded(tc.outs); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestLateRecorded(t *testing.T) {
	for name, tc := range map[string]struct {
		outs []Out
		want bool
	}{
		"none":                         {nil, false},
		"late, recorded":               {[]Out{{Amount: 1, Recorded: true, Code: CodeLate}}, true},
		"late, reversed":               {[]Out{{Amount: 1, Reversed: true}}, true},
		"late, recorded after reverse": {[]Out{{Amount: 1, Recorded: true, Reversed: true, Code: CodeLate}}, true},
		"late, never recorded":         {[]Out{{Amount: 1, Marked: true, Kind: store.MarkMempool}}, false},
		"late, foreign":                {[]Out{{Amount: 1, Foreign: true, Reversed: true}}, false},
		"counted, recorded":            {[]Out{paid(1)}, false},
		"counted, reversed":            {[]Out{{Amount: 1, Counted: true, Reversed: true}}, false},
	} {
		if got := LateRecorded(tc.outs); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestPaidAt(t *testing.T) {
	at := func(amount int64, ts uint64) Out { o := paid(amount); o.Timestamp = ts; return o }
	lateRec := Out{Amount: 1_000, Recorded: true, Code: CodeLate, Timestamp: 1}
	unrec := at(1_000, 2)
	unrec.Recorded = false
	odd := at(1_000, 3)
	odd.Code = CodeLate
	// Out of order: 40 at t=30, 50 at t=10, 20 at t=20. Sorted: 50, 70, 110: the forward at t=30 reaches 100.
	outs := []Out{at(40, 30_000_000_000), lateRec, at(50, 10_000_000_000), unrec, odd, at(20, 20_000_000_000)}
	if got := PaidAt(outs, 100, 1<<40); !got.Equal(time.Unix(30, 0)) || got.Location() != time.UTC {
		t.Fatalf("PaidAt = %v", got)
	}
	if got := PaidAt(outs, 70, 1<<40); !got.Equal(time.Unix(20, 0)) {
		t.Fatalf("PaidAt at exactly the amount = %v", got)
	}
	if got := PaidAt(outs, 111, 1<<40); !got.IsZero() {
		t.Fatalf("PaidAt below the amount = %v", got)
	}
	if got := PaidAt(nil, 1, 1<<40); !got.IsZero() {
		t.Fatal("PaidAt of nothing")
	}
	// The bound caps the running sum: 2 x 60 under a bound of 100 never reaches 101.
	if got := PaidAt([]Out{at(60, 1), at(60, 2)}, 101, 100); !got.IsZero() {
		t.Fatalf("PaidAt past the bound = %v", got)
	}
}

func TestKeep(t *testing.T) {
	dead := paid(1_000)
	dead.Kind = store.MarkDead // reverts on every tier
	mempool := paid(500)
	mempool.Kind = store.MarkMempool // reverts when it needs a block
	odd := paid(70)
	odd.Code = CodeLate
	late := Out{Amount: 9, Recorded: true, Code: CodeLate, Marked: true, Kind: store.MarkMined, Height: tip}
	unrec := paid(3)
	unrec.Recorded = false
	outs := []Out{paid(10), dead, mempool, odd, late, unrec}
	if got := Keep(outs, 0, 1<<40); got != 510 {
		t.Fatalf("Keep at tier 0 = %d, want 510", got)
	}
	if got := Keep(outs, 1, 1<<40); got != 10 {
		t.Fatalf("Keep at tier 1 = %d, want 10", got)
	}
	if got := Keep([]Out{paid(60), paid(60)}, 1, 100); got != 100 {
		t.Fatalf("Keep past the bound = %d", got)
	}
}

// Ruling 8-A6: a Bounded output (Monero) counts at most its Observed confirmations, wallet-rpc's own count with the
// mark, so a count from a wallet height read at another moment never exceeds what the wallet said; an unbounded one
// (on-chain) is tip - height + 1. Recordable and Least read the same count. Seeded: tips, heights and observations
// cross every boundary (0, 1, equal, one apart, negative observations).
func TestBoundedConfirmations(t *testing.T) {
	r := rand.New(rand.NewPCG(8, 9))
	for range 20_000 {
		tip, h, obs := r.Int64N(40), 1+r.Int64N(40), r.Int64N(44)-2
		need := r.Int64N(12)
		o := Out{Marked: true, Kind: store.MarkMined, Height: h, Counted: true, Bounded: true, Observed: obs}
		plain := max(tip-h+1, 0)
		want := min(plain, max(obs, 0))
		if got := o.Confirmations(tip); got != want {
			t.Fatalf("tip %d height %d observed %d: %d, want %d", tip, h, obs, got, want)
		}
		if got := o.Recordable(need, tip); got != (want >= need) {
			t.Fatalf("Recordable(%d) tip %d height %d observed %d: %v", need, tip, h, obs, got)
		}
		if c, ok := Least([]Out{o}, tip); !ok || int64(c) != want {
			t.Fatalf("Least: %d %v, want %d", c, ok, want)
		}
		o.Bounded = false
		if got := o.Confirmations(tip); got != plain {
			t.Fatalf("unbounded tip %d height %d: %d, want %d", tip, h, got, plain)
		}
	}
}
