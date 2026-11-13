package rail

import (
	"math"
	"math/rand"
	"slices"
	"testing"

	"github.com/satbooth/satbooth/store"
)

// The plan 6 vectors on the default tiers, each side of each bound, and the empty list.
func TestRequiredBoundaries(t *testing.T) {
	tiers := []Tier{{1_000_000, 1}, {10_000_000, 3}, {0, 6}}
	for amount, want := range map[int64]int{1: 1, 1_000_000: 1, 1_000_001: 3, 10_000_000: 3, 10_000_001: 6, store.MaxSat: 6} {
		if got := Required(tiers, amount); got != want {
			t.Errorf("Required(%d) = %d, want %d", amount, got, want)
		}
	}
	if got := Required(nil, 1); got != MaxConfirmations || MaxConfirmations != 100 {
		t.Errorf("Required with no tiers = %d, want 100", got)
	}
	// A 0 tier: Required may be 0.
	zero := []Tier{{10_000, 0}, {0, 1}}
	if Required(zero, 10_000) != 0 || Required(zero, 10_001) != 1 || Required(zero, 1) != 0 {
		t.Fatal("Required with a 0 tier")
	}
}

// SettledDepth is the last tier's count, at least 1; with no tiers the largest allowed count.
func TestSettledDepth(t *testing.T) {
	for _, tc := range []struct {
		tiers []Tier
		want  int
	}{
		{[]Tier{{1_000_000, 1}, {10_000_000, 3}, {0, 6}}, 6},
		{[]Tier{{0, 0}}, 1},
		{[]Tier{{10_000, 0}, {0, 1}}, 1},
		{[]Tier{{0, 100}}, 100},
		{nil, 100},
	} {
		if got := SettledDepth(tc.tiers); got != tc.want {
			t.Errorf("SettledDepth(%v) = %d, want %d", tc.tiers, got, tc.want)
		}
	}
}

// Each of the six refusals, with its exact text, and maxAmount respected on both sides.
func TestValidTiers(t *testing.T) {
	const limit = 1_000
	for name, tc := range map[string]struct {
		tiers []Tier
		want  string
	}{
		"empty":             {nil, "rail: Tiers is empty"},
		"empty, not nil":    {[]Tier{}, "rail: Tiers is empty"},
		"confs -1":          {[]Tier{{0, -1}}, "rail: a tier's Confirmations must be 0 to 100"},
		"confs 101":         {[]Tier{{0, 101}}, "rail: a tier's Confirmations must be 0 to 100"},
		"confs decreasing":  {[]Tier{{100, 3}, {0, 2}}, "rail: tiers' Confirmations must not decrease"},
		"last not 0":        {[]Tier{{100, 1}, {200, 2}}, "rail: the last tier's UpTo must be 0 (no upper bound)"},
		"zero in middle":    {[]Tier{{0, 1}, {200, 2}, {0, 3}}, "rail: a tier's UpTo must be 1 to the maximum amount, 0 only for the last"},
		"negative upto":     {[]Tier{{-1, 1}, {0, 2}}, "rail: a tier's UpTo must be 1 to the maximum amount, 0 only for the last"},
		"upto over max":     {[]Tier{{limit + 1, 1}, {0, 2}}, "rail: a tier's UpTo must be 1 to the maximum amount, 0 only for the last"},
		"not increasing":    {[]Tier{{100, 1}, {100, 2}, {0, 3}}, "rail: tiers' UpTo must be strictly increasing"},
		"decreasing upto":   {[]Tier{{100, 1}, {50, 2}, {0, 3}}, "rail: tiers' UpTo must be strictly increasing"},
		"ok: one tier":      {[]Tier{{0, 2}}, ""},
		"ok: confs equal":   {[]Tier{{100, 2}, {0, 2}}, ""},
		"ok: confs 0":       {[]Tier{{0, 0}}, ""},
		"ok: confs 100":     {[]Tier{{0, 100}}, ""},
		"ok: upto at max":   {[]Tier{{limit, 1}, {0, 2}}, ""},
		"ok: upto 1":        {[]Tier{{1, 0}, {0, 2}}, ""},
		"ok: zero then up":  {[]Tier{{10, 0}, {100, 1}, {0, 6}}, ""},
		"ok: upto max - 1":  {[]Tier{{limit - 1, 0}, {limit, 1}, {0, 6}}, ""},
		"ok: confs 0 to 99": {[]Tier{{1, 0}, {0, 99}}, ""},
	} {
		err := ValidTiers(tc.tiers, limit)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s: ValidTiers = %q, want %q", name, got, tc.want)
		}
	}
	// The bound is the parameter: the same list is valid under a larger one.
	if err := ValidTiers([]Tier{{limit + 1, 1}, {0, 2}}, limit+1); err != nil {
		t.Fatalf("maxAmount + 1 under a larger bound: %v", err)
	}
	if err := ValidTiers([]Tier{{store.MaxSat, 1}, {0, 2}}, store.MaxSat); err != nil {
		t.Fatalf("MaxSat under MaxSat: %v", err)
	}
	if err := ValidTiers([]Tier{{store.MaxSat + 1, 1}, {0, 2}}, store.MaxSat); err == nil {
		t.Fatal("MaxSat + 1 under MaxSat accepted")
	}
}

// validTiersModel is the rule of ValidTiers written again, for the property test and the fuzz target.
func validTiersModel(t []Tier, maxAmount int64) bool {
	if len(t) == 0 {
		return false
	}
	for i, x := range t {
		last := i == len(t)-1
		if x.Confirmations < 0 || x.Confirmations > 100 || (i > 0 && x.Confirmations < t[i-1].Confirmations) {
			return false
		}
		if last != (x.UpTo == 0) || x.UpTo < 0 || x.UpTo > maxAmount || (i > 0 && !last && x.UpTo <= t[i-1].UpTo) {
			return false
		}
	}
	return true
}

// checkTiers is the invariant shared by the property test and the fuzz target: ValidTiers agrees with the model, and
// on an accepted list Required is a tier's count (0 to 100) that never decreases with the amount, and SettledDepth is
// the last tier's, at least 1.
func checkTiers(t *testing.T, tiers []Tier, maxAmount int64, amounts []int64) {
	t.Helper()
	got := ValidTiers(tiers, maxAmount) == nil
	if got != validTiersModel(tiers, maxAmount) {
		t.Fatalf("ValidTiers(%v, %d) accepted %v, the model says %v", tiers, maxAmount, got, !got)
	}
	if !got {
		return
	}
	depth := SettledDepth(tiers)
	if depth < 1 || depth != max(tiers[len(tiers)-1].Confirmations, 1) {
		t.Fatalf("SettledDepth(%v) = %d", tiers, depth)
	}
	slices.Sort(amounts)
	prev := -1
	for _, a := range amounts {
		r := Required(tiers, a)
		if r < 0 || r > 100 || r < prev || r > depth {
			t.Fatalf("Required(%v, %d) = %d after %d (depth %d)", tiers, a, r, prev, depth)
		}
		prev = r
	}
}

// The tier rule, on seeded random lists whose values cross every boundary under two bounds (MaxSat and the piconero
// bound 2^53 - 1): confirmations -1, 0, 1, 99, 100, 101 and equal neighbours; UpTo -1, 0, 1, equal neighbours, the
// bound and the bound + 1.
func TestTiersProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261107))
	confs := []int{-1, 0, 0, 1, 1, 2, 6, 99, 100, 101, math.MaxInt, math.MinInt}
	for _, bound := range []int64{store.MaxSat, 1<<53 - 1} {
		ups := []int64{-1, 0, 1, 2, 10_000, 10_001, bound - 1, bound, bound + 1}
		amounts := []int64{1, 2, 10_000, 10_001, bound - 1, bound}
		accepted := 0
		for i := 0; i < 20000; i++ {
			tiers := make([]Tier, rng.Intn(5))
			for j := range tiers {
				tiers[j] = Tier{ups[rng.Intn(len(ups))], confs[rng.Intn(len(confs))]}
				if j > 0 && rng.Intn(3) == 0 { // a neighbour's value, or one more than it
					tiers[j].Confirmations = tiers[j-1].Confirmations + rng.Intn(2)
					tiers[j].UpTo = tiers[j-1].UpTo + int64(rng.Intn(2))
				}
			}
			if len(tiers) > 0 && rng.Intn(2) == 0 {
				tiers[len(tiers)-1].UpTo = 0
			}
			if ValidTiers(tiers, bound) == nil {
				accepted++
			}
			checkTiers(t, tiers, bound, append([]int64(nil), amounts...))
		}
		if accepted < 500 {
			t.Fatalf("bound %d: only %d accepted lists: the generator does not reach the valid side", bound, accepted)
		}
	}
}

func FuzzTiers(f *testing.F) {
	f.Add(int64(10_000), 0, int64(1_000_000), 1, int64(0), 6, uint8(3), int64(10_001), store.MaxSat)
	f.Add(int64(0), 0, int64(0), 0, int64(0), 0, uint8(1), int64(1), store.MaxSat)
	f.Add(int64(1000), 1, int64(0), 0, int64(0), 0, uint8(2), int64(1), int64(1000))
	f.Add(int64(1001), 1, int64(0), 0, int64(0), 0, uint8(2), int64(1), int64(1000))
	f.Add(int64(0), 101, int64(0), 0, int64(0), 0, uint8(1), store.MaxSat, store.MaxSat)
	f.Fuzz(func(t *testing.T, u0 int64, c0 int, u1 int64, c1 int, u2 int64, c2 int, n uint8, amount, bound int64) {
		if bound < 1 {
			t.Skip() // a bound is a positive amount
		}
		tiers := []Tier{{u0, c0}, {u1, c1}, {u2, c2}}[:n%4]
		checkTiers(t, tiers, bound, []int64{1, amount, bound})
	})
}
