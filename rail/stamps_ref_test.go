package rail

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
)

// The functions below are Stamps and PlanReversals as ruling 8-A5 wrote them, kept as the reference the linear ones
// are checked against (ruling R7): each surviving sum is summed again from scratch over every output, with a linear
// membership search, so a call is cubic in the number of reversals.

func refStamps(outs []Out, idx []int, amount, limit int64, k int) []uint64 {
	stamps := make([]uint64, len(idx))
	if k <= 0 {
		return stamps
	}
	for j := range idx {
		if refSurvivingSum(outs, idx[:j+1], limit) < amount {
			stamps[j] = uint64(k)
			break
		}
	}
	return stamps
}

func refSurvivingSum(outs []Out, gone []int, limit int64) int64 {
	var sum int64
	for i, o := range outs {
		if o.Recorded && o.Counted && o.Code == CodePaid && !slices.Contains(gone, i) {
			sum = Add(sum, o.Amount, limit)
		}
	}
	return sum
}

func refPlanReversals(outs []Out, in PlanInput) ReversalPlan {
	plan := func(hold bool) []int {
		var idx []int
		for i, o := range outs {
			if o.Reverts(in.TierNeed) && !(hold && in.Held(i)) {
				idx = append(idx, i)
			}
		}
		return idx
	}
	idx := plan(false)
	after := IsPaid(in.LightningPaid, refSurvivingSum(outs, idx, in.Limit), in.Amount)
	if in.Before && !after && in.TierNeed == 0 && in.Held != nil {
		idx = plan(true)
		after = IsPaid(in.LightningPaid, refSurvivingSum(outs, idx, in.Limit), in.Amount)
	}
	k := ReversalSeq(in.Before, after, in.Reverted)
	return ReversalPlan{Idx: idx, Stamps: refStamps(outs, idx, in.Amount, in.Limit, k), K: k}
}

// randOuts is n random outputs whose amounts sit at the small edges, at and around limit/2 and limit, so that sums
// reach and pass limit, with every field Reverts and the sums read drawn at random.
func randOuts(r *rand.Rand, n int, limit int64) []Out {
	amounts := []int64{1, 2, 3, limit / 2, limit/2 + 1, limit - 2, limit - 1, limit}
	kinds := []string{store.MarkMempool, store.MarkMined, store.MarkDead}
	outs := make([]Out, n)
	for i := range outs {
		a := amounts[r.IntN(len(amounts))]
		if r.IntN(4) == 0 {
			a = 1 + r.Int64N(limit)
		}
		o := Out{Amount: a, Counted: r.IntN(4) != 0, Recorded: r.IntN(5) != 0, Marked: r.IntN(6) != 0}
		o.Code = CodePaid
		if r.IntN(5) == 0 {
			o.Code = CodeLate
		}
		if o.Marked {
			o.Kind = kinds[r.IntN(len(kinds))]
			if o.Kind == store.MarkMined {
				o.Height = 1 + r.Int64N(10)
			}
		}
		outs[i] = o
	}
	return outs
}

// Ruling R7: Stamps equals the reference on random inputs: outputs whose sums reach and pass the limit, idx a random
// subset in random order, at times with an index twice or out of range (the reference ignores both: an output is gone
// from its first reversal on, and an index no output has removes nothing), amounts at the edges.
func TestStampsMatchesTheReference(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 21))
	for _, limit := range []int64{store.MaxPiconero, store.MaxSat, 10} {
		for range 30_000 {
			n := r.IntN(13)
			outs := randOuts(r, n, limit)
			var idx []int
			for _, i := range r.Perm(n) {
				if r.IntN(3) != 0 {
					idx = append(idx, i)
				}
			}
			if len(idx) > 0 && r.IntN(8) == 0 {
				idx = slices.Insert(idx, r.IntN(len(idx)+1), idx[r.IntN(len(idx))])
			}
			if r.IntN(10) == 0 {
				idx = slices.Insert(idx, r.IntN(len(idx)+1), []int{-1, n, n + 5}[r.IntN(3)])
			}
			amounts := []int64{0, 1, 2, limit / 2, limit - 1, limit, limit + 1, 1 + r.Int64N(limit)}
			amount := amounts[r.IntN(len(amounts))]
			k := r.IntN(3)
			if got, want := Stamps(outs, idx, amount, limit, k), refStamps(outs, idx, amount, limit, k); !slices.Equal(got, want) {
				t.Fatalf("limit %d outs %+v idx %v amount %d k %d: Stamps = %v, reference %v", limit, outs, idx, amount, k, got, want)
			}
		}
	}
}

// Ruling R7: PlanReversals equals the reference on random orders: the tier, Before, Lightning, the count of
// order.reverted and a random hold, over outputs whose sums reach and pass the limit.
func TestPlanReversalsMatchesTheReference(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 22))
	for _, limit := range []int64{store.MaxPiconero, store.MaxSat, 10} {
		for range 30_000 {
			n := r.IntN(13)
			outs := randOuts(r, n, limit)
			held := make([]bool, n)
			for i := range held {
				held[i] = r.IntN(3) == 0
			}
			amounts := []int64{1, 2, limit / 2, limit - 1, limit, 1 + r.Int64N(limit)}
			in := PlanInput{TierNeed: int64(r.IntN(3)), Amount: amounts[r.IntN(len(amounts))], Limit: limit,
				Before: r.IntN(2) == 0, LightningPaid: r.IntN(4) == 0, Reverted: r.IntN(3)}
			if r.IntN(2) == 0 {
				in.Held = func(i int) bool { return held[i] }
			}
			got, want := PlanReversals(outs, in), refPlanReversals(outs, in)
			if !slices.Equal(got.Idx, want.Idx) || !slices.Equal(got.Stamps, want.Stamps) || got.K != want.K {
				t.Fatalf("limit %d outs %+v in %+v held %v: PlanReversals = %+v, reference %+v", limit, outs, in, held, got, want)
			}
		}
	}
}

// Ruling R7: 10,000 recorded payments that all revert, any one of which pays the order.
// Stamps must try every prefix before the sum falls below the amount; the reference does that in about 3e11
// comparisons, under the settle mutex both rails share. The linear functions take microseconds.
func TestStampsTenThousandOutputs(t *testing.T) {
	const n = 10_000
	outs := make([]Out, n)
	idx := make([]int, n)
	for i := range outs {
		outs[i] = Out{Amount: 1, Counted: true, Recorded: true, Code: CodePaid, Marked: true, Kind: store.MarkDead}
		idx[i] = i
	}
	done := make(chan ReversalPlan, 1)
	go func() {
		done <- PlanReversals(outs, PlanInput{TierNeed: 1, Amount: 1, Limit: store.MaxPiconero, Before: true, Reverted: 2})
	}()
	select {
	case p := <-done:
		if len(p.Idx) != n || p.K != 3 || p.Stamps[n-1] != 3 || slices.Index(p.Stamps, 3) != n-1 {
			t.Fatalf("plan: %d reversals, K %d, last stamp %d, first stamp 3 at %d", len(p.Idx), p.K, p.Stamps[n-1],
				slices.Index(p.Stamps, 3))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PlanReversals over 10,000 reversals took more than 5 s")
	}
	start := time.Now()
	s := Stamps(outs, idx, 1, store.MaxPiconero, 1)
	if d := time.Since(start); d > time.Second || s[n-1] != 1 {
		t.Fatalf("Stamps over 10,000 reversals: %v, last stamp %d", d, s[n-1])
	}
}
