package rail

import (
	"math/big"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/store"
)

func TestReversalSeq(t *testing.T) {
	for _, tc := range []struct {
		before, after bool
		reverted      int
		want          int
	}{
		{true, false, 0, 1},
		{true, false, 4, 5},
		{true, true, 4, 0},
		{false, false, 4, 0},
		{false, true, 0, 0},
	} {
		if got := ReversalSeq(tc.before, tc.after, tc.reverted); got != tc.want {
			t.Errorf("ReversalSeq(%v, %v, %d) = %d, want %d", tc.before, tc.after, tc.reverted, got, tc.want)
		}
	}
}

func TestStamps(t *testing.T) {
	c := func(a int64) Out { return Out{Amount: a, Counted: true, Recorded: true, Code: CodePaid} }
	late := Out{Amount: 500, Recorded: true, Code: CodeLate}
	const limit = 1 << 40
	for name, tc := range map[string]struct {
		outs   []Out // the reversals first, then the records that survive
		idx    []int
		amount int64
		k      int
		want   []uint64
	}{
		// Only the second takes the surviving sum (200 → 140 → 80) below the amount.
		"the second flips":     {[]Out{c(60), c(60), c(80)}, []int{0, 1}, 100, 3, []uint64{0, 3}},
		"k 0":                  {[]Out{c(60), c(60), c(80)}, []int{0, 1}, 100, 0, []uint64{0, 0}},
		"the first flips":      {[]Out{c(60), c(60), c(30)}, []int{0, 1, 2}, 100, 2, []uint64{2, 0, 0}},
		"a late one counts 0":  {[]Out{late, c(60), c(60), c(80)}, []int{0, 1, 2}, 100, 1, []uint64{0, 0, 1}},
		"at the amount exact":  {[]Out{c(100), c(1), c(99)}, []int{0, 1}, 100, 1, []uint64{0, 1}},
		"nothing in the plan":  {[]Out{c(200)}, nil, 100, 1, []uint64{}},
		"never below":          {[]Out{c(10), c(190)}, []int{0}, 100, 1, []uint64{0}},
		"only late, k nonzero": {[]Out{late, c(50)}, []int{0}, 100, 1, []uint64{1}},
		"out of order":         {[]Out{c(80), c(60), c(60)}, []int{2, 1}, 100, 4, []uint64{0, 4}},
	} {
		got := Stamps(tc.outs, tc.idx, tc.amount, limit, tc.k)
		if len(got) != len(tc.want) || (len(got) > 0 && !slices.Equal(got, tc.want)) {
			t.Errorf("%s: Stamps = %v, want %v", name, got, tc.want)
		}
	}
}

// Ruling 8-A5: the surviving sum is summed again, never subtracted from a sum capped at the limit. Two records, L-1
// and 2, cap at L; reversing L-1 leaves 2, which still pays an amount of 2. Subtracting from the capped sum read 1.
func TestStampsAndPlanAboveTheLimit(t *testing.T) {
	const limit = store.MaxPiconero
	rec := func(a int64, kind string) Out {
		return Out{Amount: a, Counted: true, Recorded: true, Code: CodePaid, Marked: true, Kind: kind, Height: tip}
	}
	outs := []Out{rec(limit-1, store.MarkMempool), rec(2, store.MarkMined)}
	if got := Stamps(outs, []int{0}, 2, limit, 1); !slices.Equal(got, []uint64{0}) {
		t.Errorf("Stamps = %v, want [0]", got)
	}
	got := PlanReversals(outs, PlanInput{TierNeed: 1, Amount: 2, Limit: limit, Before: true, Reverted: 0})
	if !slices.Equal(got.Idx, []int{0}) || !slices.Equal(got.Stamps, []uint64{0}) || got.K != 0 {
		t.Errorf("PlanReversals = %+v, want the big one reversed with no event", got)
	}
	// Both revert: the order does turn unpaid.
	outs[1] = rec(2, store.MarkDead)
	got = PlanReversals(outs, PlanInput{TierNeed: 1, Amount: 2, Limit: limit, Before: true, Reverted: 0})
	if !slices.Equal(got.Idx, []int{0, 1}) || !slices.Equal(got.Stamps, []uint64{0, 1}) || got.K != 1 {
		t.Errorf("PlanReversals = %+v, want both reversed, the second stamped 1", got)
	}
}

// A seeded property: for any set of records and any plan, the stamps are those of a reference that sums the survivors
// with big integers (no cap), clamped only where the reference's sum crosses the amount; amounts and the limit cross
// every boundary around the cap.
func TestStampsProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(8, 5))
	const limit = store.MaxPiconero
	amounts := []int64{1, 2, limit / 2, limit - 2, limit - 1, limit}
	for range 20_000 {
		n := 1 + r.IntN(5)
		outs := make([]Out, n)
		for i := range outs {
			outs[i] = Out{Amount: amounts[r.IntN(len(amounts))], Counted: r.IntN(4) != 0, Recorded: true}
			outs[i].Code = CodePaid
			if !outs[i].Counted {
				outs[i].Code = CodeLate
			}
		}
		var idx []int
		for i := range outs {
			if r.IntN(2) == 0 {
				idx = append(idx, i)
			}
		}
		amount := amounts[r.IntN(len(amounts))]
		k := r.IntN(3)
		got := Stamps(outs, idx, amount, limit, k)
		want := make([]uint64, len(idx))
		for j := range idx {
			sum := new(big.Int)
			for i, o := range outs {
				if o.Counted && !slices.Contains(idx[:j+1], i) {
					sum.Add(sum, big.NewInt(o.Amount))
				}
			}
			if k > 0 && sum.Cmp(big.NewInt(amount)) < 0 {
				want[j] = uint64(k)
				break
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("outs %+v idx %v amount %d k %d: Stamps = %v, want %v", outs, idx, amount, k, got, want)
		}
	}
}

func TestLightningDone(t *testing.T) {
	onchainOnly := store.Order{AmountSat: 1}
	mixed := store.Order{AmountSat: 1, AmountMsat: 1000}
	for name, tc := range map[string]struct {
		o    store.Order
		ln   lnpay.Status
		want bool
	}{
		"no Lightning amount":     {onchainOnly, lnpay.Status{}, true},
		"no Lightning, paid read": {onchainOnly, lnpay.Status{State: lnpay.StatePaid}, true},
		"mixed, new":              {mixed, lnpay.Status{State: lnpay.StateNew}, false},
		"mixed, zero status":      {mixed, lnpay.Status{}, false},
		"mixed, paid":             {mixed, lnpay.Status{State: lnpay.StatePaid}, false},
		"mixed, expired":          {mixed, lnpay.Status{State: lnpay.StateExpired}, true},
	} {
		if got := LightningDone(tc.o, tc.ln); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestFinal(t *testing.T) {
	exp := time.Unix(1_700_000_000, 0)
	end := exp.Add(store.WatchWindow)
	base := FinalInput{Now: exp, ExpiresAt: exp, Amount: 100, LightningDone: true}
	type want struct {
		typ         string
		window, due bool
	}
	for name, tc := range map[string]struct {
		mod  func(*FinalInput)
		want want
	}{
		"before expiry":               {func(i *FinalInput) { i.Now = exp.Add(-time.Second) }, want{store.EventExpired, false, false}},
		"before expiry, partial":      {func(i *FinalInput) { i.Now, i.CountedLive = exp.Add(-time.Nanosecond), 50 }, want{store.EventUnderpaid, false, false}},
		"at expiry, nothing":          {func(*FinalInput) {}, want{store.EventExpired, false, true}},
		"Lightning open":              {func(i *FinalInput) { i.LightningDone = false }, want{store.EventExpired, false, false}},
		"a final event exists":        {func(i *FinalInput) { i.HasFinal = true }, want{store.EventExpired, false, false}},
		"a peer pending":              {func(i *FinalInput) { i.PeerPending = true }, want{store.EventExpired, false, false}},
		"a peer received, 0 live":     {func(i *FinalInput) { i.PeerReceived = true }, want{store.EventUnderpaid, false, true}},
		"partial":                     {func(i *FinalInput) { i.CountedLive = 99 }, want{store.EventUnderpaid, false, true}},
		"whole amount, in the window": {func(i *FinalInput) { i.Now, i.CountedLive = end.Add(-time.Second), 100 }, want{store.EventUnderpaid, true, false}},
		"whole amount, window end":    {func(i *FinalInput) { i.Now, i.CountedLive = end, 100 }, want{store.EventUnderpaid, true, true}},
		"more, window end":            {func(i *FinalInput) { i.Now, i.CountedLive = end.Add(time.Hour), 150 }, want{store.EventUnderpaid, true, true}},
		"partial, window end":         {func(i *FinalInput) { i.Now, i.CountedLive = end, 40 }, want{store.EventUnderpaid, false, true}},
		"nothing, window end":         {func(i *FinalInput) { i.Now = end }, want{store.EventExpired, false, true}},
		"window end, Lightning open":  {func(i *FinalInput) { i.Now, i.CountedLive, i.LightningDone = end, 100, false }, want{store.EventUnderpaid, true, false}},
		"window end, a peer pending":  {func(i *FinalInput) { i.Now, i.CountedLive, i.PeerPending = end, 100, true }, want{store.EventUnderpaid, true, false}},
		"S9: partial, a peer partial": {func(i *FinalInput) { i.CountedLive, i.PeerReceived = 60, true }, want{store.EventUnderpaid, false, true}},
		// Rulings 8-T7d and 8-T7e: a stale peer holds the final event as long as it stays stale, past the window's end too.
		"a peer stale":                      {func(i *FinalInput) { i.PeerStale = true }, want{store.EventExpired, false, false}},
		"a peer stale, partial":             {func(i *FinalInput) { i.PeerStale, i.CountedLive = true, 50 }, want{store.EventUnderpaid, false, false}},
		"a peer stale, a second before end": {func(i *FinalInput) { i.Now, i.PeerStale = end.Add(-time.Second), true }, want{store.EventExpired, false, false}},
		"a peer stale, window end":          {func(i *FinalInput) { i.Now, i.PeerStale = end, true }, want{store.EventExpired, false, false}},
		"a peer stale, whole, window end":   {func(i *FinalInput) { i.Now, i.PeerStale, i.CountedLive = end, true, 100 }, want{store.EventUnderpaid, true, false}},
		"a peer stale, a week past the end": {func(i *FinalInput) { i.Now, i.PeerStale = end.Add(store.WatchWindow), true }, want{store.EventExpired, false, false}},
		"a peer fresh again past the end":   {func(i *FinalInput) { i.Now = end.Add(store.WatchWindow) }, want{store.EventExpired, false, true}},
		"a peer stale and pending, end":     {func(i *FinalInput) { i.Now, i.PeerStale, i.PeerPending = end, true, true }, want{store.EventExpired, false, false}},
	} {
		in := base
		tc.mod(&in)
		typ, window, due := Final(in)
		if (want{typ, window, due}) != tc.want {
			t.Errorf("%s: Final = %s, %v, %v; want %+v", name, typ, window, due, tc.want)
		}
	}
}

// Step 1's heal: every reversal's seq in (0, 2^20], sorted, once each, over every output (a foreign one included).
func TestRevertedSeqs(t *testing.T) {
	for name, tc := range map[string]struct {
		outs []Out
		want []int
	}{
		"none":              {nil, nil},
		"no reversals":      {[]Out{{Amount: 1}, {Amount: 2, Recorded: true}}, nil},
		"0 dropped":         {[]Out{{Seqs: []uint64{0, 0}}}, nil},
		"1 kept":            {[]Out{{Seqs: []uint64{1}}}, []int{1}},
		"2^20 kept":         {[]Out{{Seqs: []uint64{1 << 20}}}, []int{1 << 20}},
		"2^20 + 1 dropped":  {[]Out{{Seqs: []uint64{1<<20 + 1, 3}}}, []int{3}},
		"max uint64":        {[]Out{{Seqs: []uint64{^uint64(0), 1<<63 + 1}}}, nil},
		"sorted, once each": {[]Out{{Seqs: []uint64{5, 0, 2}}, {Seqs: []uint64{2, 1}}, {Seqs: []uint64{5}}}, []int{1, 2, 5}},
		"a foreign output":  {[]Out{{Foreign: true, Seqs: []uint64{4}}}, []int{4}},
	} {
		got := RevertedSeqs(tc.outs)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: RevertedSeqs = %v, want %v", name, got, tc.want)
		}
	}
}

// Step 2's paid rule, and step 4's and 5's: Lightning paid, or the confirmed sum reaches the amount.
func TestIsPaid(t *testing.T) {
	for _, tc := range []struct {
		ln                bool
		confirmed, amount int64
		want              bool
	}{
		{false, 100, 100, true},
		{false, 99, 100, false},
		{false, 101, 100, true},
		{true, 0, 100, true},
		{true, 99, 100, true},
		{false, 0, 1, false},
	} {
		if got := IsPaid(tc.ln, tc.confirmed, tc.amount); got != tc.want {
			t.Errorf("IsPaid(%v, %d, %d) = %v", tc.ln, tc.confirmed, tc.amount, got)
		}
	}
}

// Step 3's overpaid comparison: a counted record that takes the kept sum above the amount.
func TestOverpays(t *testing.T) {
	c := func(a int64) Out { return Out{Amount: a, Counted: true} }
	for name, tc := range map[string]struct {
		o                   Out
		keep, amount, limit int64
		want                bool
	}{
		"exactly the amount":  {c(40), 60, 100, 1 << 40, false},
		"one over":            {c(41), 60, 100, 1 << 40, true},
		"alone, over":         {c(101), 0, 100, 1 << 40, true},
		"late never overpays": {Out{Amount: 1_000}, 60, 100, 1 << 40, false},
		// The sum is capped at the limit before the comparison (today's addSat), so at limit == amount nothing reads
		// overpaid: pinned as the old behaviour.
		"capped at the limit": {c(50), 60, 100, 100, false},
		"limit above amount":  {c(50), 60, 100, 101, true},
	} {
		if got := Overpays(tc.o, tc.keep, tc.amount, tc.limit); got != tc.want {
			t.Errorf("%s: Overpays = %v", name, got)
		}
	}
}

// Step 4's plan: the reverting records in order, the 0-tier hold only when the reversals would turn a paid order
// unpaid, the seq and the stamps.
func TestPlanReversals(t *testing.T) {
	rec := func(a int64, kind string) Out {
		return Out{Amount: a, Counted: true, Recorded: true, Code: CodePaid, Marked: true, Kind: kind, Height: tip}
	}
	dead := func(a int64) Out { return rec(a, store.MarkDead) }
	mined := rec(60, store.MarkMined)
	lateDead := Out{Amount: 500, Recorded: true, Code: CodeLate, Marked: true, Kind: store.MarkDead}
	heldAt := func(idx ...int) func(int) bool { return func(i int) bool { return slices.Contains(idx, i) } }
	never := func(int) bool { return false }
	for name, tc := range map[string]struct {
		outs []Out
		in   PlanInput
		want ReversalPlan
	}{
		"nothing reverts": {[]Out{mined, mined}, PlanInput{TierNeed: 1, Amount: 100, Before: true, Reverted: 2},
			ReversalPlan{}},
		"the second flips": {[]Out{dead(60), dead(60), mined}, PlanInput{TierNeed: 1, Amount: 100, Before: true, Reverted: 2},
			ReversalPlan{Idx: []int{0, 1}, Stamps: []uint64{0, 3}, K: 3}},
		"leaves it paid": {[]Out{mined, dead(10), mined}, PlanInput{TierNeed: 1, Amount: 100, Before: true, Reverted: 2},
			ReversalPlan{Idx: []int{1}, Stamps: []uint64{0}}},
		"not paid before": {[]Out{dead(60)}, PlanInput{TierNeed: 1, Amount: 100, Reverted: 2},
			ReversalPlan{Idx: []int{0}, Stamps: []uint64{0}}},
		"Lightning paid": {[]Out{dead(60), dead(60)}, PlanInput{TierNeed: 1, Amount: 100, Before: true, LightningPaid: true},
			ReversalPlan{Idx: []int{0, 1}, Stamps: []uint64{0, 0}}},
		"a late record counts nothing": {[]Out{lateDead, dead(60)}, PlanInput{TierNeed: 1, Amount: 60, Before: true},
			ReversalPlan{Idx: []int{0, 1}, Stamps: []uint64{0, 1}, K: 1}},
		"a late reversal leaves it paid": {[]Out{lateDead, mined}, PlanInput{TierNeed: 1, Amount: 60, Before: true, Reverted: 4},
			ReversalPlan{Idx: []int{0}, Stamps: []uint64{0}}},
		// The 0-tier hold.
		"tier 0, the only one held": {[]Out{dead(100)}, PlanInput{Amount: 100, Before: true, Reverted: 1, Held: heldAt(0)},
			ReversalPlan{}},
		"tier 0, one of two held, still flips": {[]Out{dead(60), dead(60)}, PlanInput{Amount: 100, Before: true, Reverted: 1, Held: heldAt(0)},
			ReversalPlan{Idx: []int{1}, Stamps: []uint64{2}, K: 2}},
		"tier 0, held one keeps it paid": {[]Out{dead(100), dead(10)}, PlanInput{Amount: 100, Before: true, Held: heldAt(0)},
			ReversalPlan{Idx: []int{1}, Stamps: []uint64{0}}},
		"tier 1, the hold ignored": {[]Out{dead(60), dead(60)}, PlanInput{TierNeed: 1, Amount: 100, Before: true, Held: heldAt(0)},
			ReversalPlan{Idx: []int{0, 1}, Stamps: []uint64{1, 0}, K: 1}},
		"tier 0, not paid before: no hold": {[]Out{dead(60)}, PlanInput{Amount: 100, Held: heldAt(0)},
			ReversalPlan{Idx: []int{0}, Stamps: []uint64{0}}},
		"tier 0, no flip: no hold": {[]Out{dead(10), mined, mined}, PlanInput{Amount: 100, Before: true, Held: heldAt(0)},
			ReversalPlan{Idx: []int{0}, Stamps: []uint64{0}}},
		"tier 0, Lightning paid: no hold": {[]Out{dead(100)}, PlanInput{Amount: 100, Before: true, LightningPaid: true, Held: heldAt(0)},
			ReversalPlan{Idx: []int{0}, Stamps: []uint64{0}}},
		"tier 0, nil Held": {[]Out{dead(100)}, PlanInput{Amount: 100, Before: true},
			ReversalPlan{Idx: []int{0}, Stamps: []uint64{1}, K: 1}},
		"tier 0, none held": {[]Out{dead(100)}, PlanInput{Amount: 100, Before: true, Held: never},
			ReversalPlan{Idx: []int{0}, Stamps: []uint64{1}, K: 1}},
		// A 0-conf record back in the mempool stays recorded on a 0 tier, and reverts on tier 1.
		"tier 0, in the mempool": {[]Out{rec(100, store.MarkMempool)}, PlanInput{Amount: 100, Before: true},
			ReversalPlan{}},
		"tier 1, in the mempool": {[]Out{rec(100, store.MarkMempool)}, PlanInput{TierNeed: 1, Amount: 100, Before: true},
			ReversalPlan{Idx: []int{0}, Stamps: []uint64{1}, K: 1}},
	} {
		asked := map[string]bool{"tier 0, the only one held": true, "tier 0, one of two held, still flips": true,
			"tier 0, held one keeps it paid": true, "tier 0, none held": true}
		calls := 0
		in := tc.in
		in.Limit = 1 << 40
		if in.Held != nil {
			h := in.Held
			in.Held = func(i int) bool { calls++; return h(i) }
		}
		got := PlanReversals(tc.outs, in)
		if !slices.Equal(got.Idx, tc.want.Idx) || !slices.Equal(got.Stamps, tc.want.Stamps) || got.K != tc.want.K ||
			len(got.Idx) != len(got.Stamps) {
			t.Errorf("%s: PlanReversals = %+v, want %+v", name, got, tc.want)
		}
		// The hold is asked only on a 0 tier when the plan without it would turn a paid order unpaid.
		if (calls > 0) != asked[name] {
			t.Errorf("%s: Held asked %d times", name, calls)
		}
	}
}

// HealSeqs (ruling R2): the union of the two rails' seqs, each in 1 to 2^20, sorted, once each, whatever the inputs:
// seeded inputs cross both bounds (0, 1, 2^20, 2^20+1, negatives) and repeat values across and within the lists.
func TestHealSeqsProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(8, 2))
	pick := []int{-1, 0, 1, 2, 3, maxSeq - 1, maxSeq, maxSeq + 1, 1 << 30}
	gen := func() []int {
		var l []int
		for range r.IntN(6) {
			if r.IntN(2) == 0 {
				l = append(l, pick[r.IntN(len(pick))])
			} else {
				l = append(l, r.IntN(2*maxSeq)-maxSeq/2)
			}
		}
		return l
	}
	for range 20_000 {
		own, peer := gen(), gen()
		got := HealSeqs(own, peer)
		want := map[int]bool{}
		for _, k := range slices.Concat(own, peer) {
			if k >= 1 && k <= maxSeq {
				want[k] = true
			}
		}
		if len(got) != len(want) || !slices.IsSorted(got) {
			t.Fatalf("HealSeqs(%v, %v) = %v", own, peer, got)
		}
		for i, k := range got {
			if !want[k] || i > 0 && got[i-1] == k {
				t.Fatalf("HealSeqs(%v, %v) = %v", own, peer, got)
			}
		}
	}
	if got := HealSeqs(nil, nil); len(got) != 0 {
		t.Fatalf("HealSeqs(nil, nil) = %v", got)
	}
}
