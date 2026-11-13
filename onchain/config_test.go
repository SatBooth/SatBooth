package onchain

import (
	"math"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/descriptor"
	"github.com/satbooth/satbooth/store"
)

func TestRequiredAtTheTierBoundaries(t *testing.T) {
	tiers := []Tier{{UpTo: 1_000_000, Confirmations: 1}, {UpTo: 10_000_000, Confirmations: 3}, {UpTo: 0, Confirmations: 6}}
	for amount, want := range map[int64]int{1: 1, 1_000_000: 1, 1_000_001: 3, 10_000_000: 3, 10_000_001: 6, store.MaxSat: 6} {
		if got := Required(tiers, amount); got != want {
			t.Errorf("Required(%d) = %d, want %d", amount, got, want)
		}
	}
	if SettledDepth(tiers) != 6 {
		t.Fatal("SettledDepth")
	}
	// A 0 tier: Required may be 0, and SettledDepth is floored at 1 (Ruling 6.1-4).
	zero := []Tier{{UpTo: 10_000, Confirmations: 0}, {UpTo: 0, Confirmations: 1}}
	if Required(zero, 10_000) != 0 || Required(zero, 10_001) != 1 || Required(zero, 1) != 0 {
		t.Fatal("Required with a 0 tier")
	}
	if SettledDepth([]Tier{{UpTo: 0, Confirmations: 0}}) != 1 || SettledDepth(zero) != 1 || SettledDepth(nil) != maxConfirmations {
		t.Fatal("SettledDepth with a 0 tier")
	}
}

func TestConfigDefaults(t *testing.T) {
	d, _ := descriptor.Parse("wpkh(" + tpub + "/0/*)")
	c := Config{Descriptor: d, Network: "regtest"}.WithDefaults()
	if len(c.Tiers) != 3 || c.Tiers[0] != (Tier{UpTo: 1_000_000, Confirmations: 1}) || c.Tiers[1] != (Tier{UpTo: 10_000_000, Confirmations: 3}) || c.Tiers[2] != (Tier{UpTo: 0, Confirmations: 6}) ||
		c.GapAlert != 1000 || c.PollInterval != 10*time.Second || c.PollRate != 5 || c.CallTimeout != 30*time.Second {
		t.Fatalf("%+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Config{Descriptor: d, Network: "regtest"}).Validate(); err != nil {
		t.Fatalf("Validate applies the defaults: %v", err)
	}
}

func TestConfigValidate(t *testing.T) {
	// The xpub below is the master public key of BIP-32 test vector 1.
	dt, _ := descriptor.Parse("wpkh(" + tpub + "/0/*)")
	dm, _ := descriptor.Parse("wpkh(xpub661MyMwAqRbcFtXgS5sYJABqqG9YLmC4Q1Rdap9gSE8NqtwybGhePY2gZ29ESFjqJoCu1Rupje8YtGqsefD265TMg7usUDFdp6W1EGMcet8/0/*)")
	base := func() Config { return Config{Descriptor: dt, Network: "regtest"} }
	ok := func(m func(*Config)) bool { c := base(); m(&c); return c.Validate() == nil }
	for name, tc := range map[string]struct {
		mut  func(*Config)
		good bool
	}{
		"base":              {func(*Config) {}, true},
		"mainnet with xpub": {func(c *Config) { c.Network, c.Descriptor = "mainnet", dm }, true},
		"testnet":           {func(c *Config) { c.Network = "testnet" }, true},
		"signet":            {func(c *Config) { c.Network = "signet" }, true},
		"testnet4":          {func(c *Config) { c.Network = "testnet4" }, false},
		"empty network":     {func(c *Config) { c.Network = "" }, false},
		"tpub with mainnet": {func(c *Config) { c.Network = "mainnet" }, false},
		"xpub with regtest": {func(c *Config) { c.Descriptor = dm }, false},
		"no descriptor":     {func(c *Config) { c.Descriptor = descriptor.Descriptor{} }, false},
		"one tier":          {func(c *Config) { c.Tiers = []Tier{{UpTo: 0, Confirmations: 2}} }, true},
		"not increasing": {func(c *Config) {
			c.Tiers = []Tier{{UpTo: 100, Confirmations: 1}, {UpTo: 100, Confirmations: 2}, {UpTo: 0, Confirmations: 3}}
		}, false},
		"decreasing": {func(c *Config) {
			c.Tiers = []Tier{{UpTo: 100, Confirmations: 1}, {UpTo: 50, Confirmations: 2}, {UpTo: 0, Confirmations: 3}}
		}, false},
		"last not 0": {func(c *Config) { c.Tiers = []Tier{{UpTo: 100, Confirmations: 1}, {UpTo: 200, Confirmations: 2}} }, false},
		"zero in the middle": {func(c *Config) {
			c.Tiers = []Tier{{UpTo: 0, Confirmations: 1}, {UpTo: 200, Confirmations: 2}, {UpTo: 0, Confirmations: 3}}
		}, false},
		"confs decreasing": {func(c *Config) { c.Tiers = []Tier{{UpTo: 100, Confirmations: 3}, {UpTo: 0, Confirmations: 2}} }, false},
		"confs equal":      {func(c *Config) { c.Tiers = []Tier{{UpTo: 100, Confirmations: 2}, {UpTo: 0, Confirmations: 2}} }, true},
		"confs 0":          {func(c *Config) { c.Tiers = []Tier{{UpTo: 0, Confirmations: 0}} }, true},
		"zero then up": {func(c *Config) {
			c.Tiers = []Tier{{UpTo: 10_000, Confirmations: 0}, {UpTo: 1_000_000, Confirmations: 1}, {UpTo: 0, Confirmations: 6}}
		}, true},
		"up then zero":  {func(c *Config) { c.Tiers = []Tier{{UpTo: 1000, Confirmations: 1}, {UpTo: 0, Confirmations: 0}} }, false},
		"confs -1":      {func(c *Config) { c.Tiers = []Tier{{UpTo: 0, Confirmations: -1}} }, false},
		"confs 100":     {func(c *Config) { c.Tiers = []Tier{{UpTo: 0, Confirmations: 100}} }, true},
		"confs 101":     {func(c *Config) { c.Tiers = []Tier{{UpTo: 0, Confirmations: 101}} }, false},
		"negative upto": {func(c *Config) { c.Tiers = []Tier{{UpTo: -1, Confirmations: 1}, {UpTo: 0, Confirmations: 2}} }, false},
		"upto over max": {func(c *Config) {
			c.Tiers = []Tier{{UpTo: store.MaxSat + 1, Confirmations: 1}, {UpTo: 0, Confirmations: 2}}
		}, false},
		"gap 1":               {func(c *Config) { c.GapAlert = 1 }, true},
		"gap 1000000":         {func(c *Config) { c.GapAlert = 1_000_000 }, true},
		"gap 1000001":         {func(c *Config) { c.GapAlert = 1_000_001 }, false},
		"gap negative":        {func(c *Config) { c.GapAlert = -1 }, false},
		"poll 2s":             {func(c *Config) { c.PollInterval = 2 * time.Second }, true},
		"poll 1.9s":           {func(c *Config) { c.PollInterval = 1900 * time.Millisecond }, false},
		"poll 60s":            {func(c *Config) { c.PollInterval = 60 * time.Second }, true},
		"poll 61s":            {func(c *Config) { c.PollInterval = 61 * time.Second }, false},
		"rate 10000":          {func(c *Config) { c.PollRate = 10000 }, true},
		"rate 10001":          {func(c *Config) { c.PollRate = 10001 }, false},
		"rate negative":       {func(c *Config) { c.PollRate = -1 }, false},
		"rate NaN":            {func(c *Config) { c.PollRate = math.NaN() }, false},
		"rate Inf":            {func(c *Config) { c.PollRate = math.Inf(1) }, false},
		"call timeout 1s":     {func(c *Config) { c.CallTimeout = time.Second }, true},
		"call timeout 999ms":  {func(c *Config) { c.CallTimeout = 999 * time.Millisecond }, false},
		"call timeout 10min":  {func(c *Config) { c.CallTimeout = 10 * time.Minute }, true},
		"call timeout >10min": {func(c *Config) { c.CallTimeout = 10*time.Minute + 1 }, false},
		"call timeout neg":    {func(c *Config) { c.CallTimeout = -time.Second }, false},
	} {
		if got := ok(tc.mut); got != tc.good {
			t.Errorf("%s: valid = %v, want %v", name, got, tc.good)
		}
	}
	if err := (Config{Descriptor: dt, Network: "testnet4"}).Validate(); err == nil || !strings.Contains(strings.ToLower(err.Error()), "network") {
		t.Fatalf("testnet4: %v", err)
	}
	for _, n := range []int{-1, 101} {
		c := base()
		c.Tiers = []Tier{{UpTo: 0, Confirmations: n}}
		if err := c.Validate(); err == nil || err.Error() != "onchain: a tier's Confirmations must be 0 to 100" {
			t.Fatalf("confirmations %d: %v", n, err)
		}
	}
}

// validTiersModel is the rule of validTiers written again, for the property test and the fuzz target.
func validTiersModel(t []Tier) bool {
	if len(t) == 0 {
		return false
	}
	for i, x := range t {
		last := i == len(t)-1
		if x.Confirmations < 0 || x.Confirmations > 100 || (i > 0 && x.Confirmations < t[i-1].Confirmations) {
			return false
		}
		if last != (x.UpTo == 0) || x.UpTo < 0 || x.UpTo > store.MaxSat || (i > 0 && !last && x.UpTo <= t[i-1].UpTo) {
			return false
		}
	}
	return true
}

// checkTiers is the invariant shared by the property test and the fuzz target: validTiers agrees with the model, and
// on an accepted list Required is a tier's count (0 to 100) that never decreases with the amount, and SettledDepth is
// the largest of them, at least 1.
func checkTiers(t *testing.T, tiers []Tier, amounts []int64) {
	t.Helper()
	got := validTiers(tiers) == nil
	if got != validTiersModel(tiers) {
		t.Fatalf("validTiers(%v) accepted %v, the model says %v", tiers, got, !got)
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

// The tier rule, on seeded random lists whose values cross every boundary: confirmations -1, 0, 1, 99, 100, 101 and
// equal neighbours; up_to_sat -1, 0, 1, equal neighbours, MaxSat and MaxSat + 1.
func TestTiersProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261104))
	confs := []int{-1, 0, 0, 1, 1, 2, 6, 99, 100, 101, math.MaxInt, math.MinInt}
	ups := []int64{-1, 0, 1, 2, 10_000, 10_001, store.MaxSat - 1, store.MaxSat, store.MaxSat + 1}
	amounts := []int64{1, 2, 10_000, 10_001, store.MaxSat - 1, store.MaxSat}
	accepted := 0
	for i := 0; i < 20000; i++ {
		tiers := make([]Tier, rng.Intn(5))
		for j := range tiers {
			tiers[j] = Tier{UpTo: ups[rng.Intn(len(ups))], Confirmations: confs[rng.Intn(len(confs))]}
			if j > 0 && rng.Intn(3) == 0 { // a neighbour's value, or one more than it
				tiers[j].Confirmations = tiers[j-1].Confirmations + rng.Intn(2)
				tiers[j].UpTo = tiers[j-1].UpTo + int64(rng.Intn(2))
			}
		}
		if len(tiers) > 0 && rng.Intn(2) == 0 {
			tiers[len(tiers)-1].UpTo = 0
		}
		if validTiers(tiers) == nil {
			accepted++
		}
		checkTiers(t, tiers, append([]int64(nil), amounts...))
	}
	if accepted < 500 {
		t.Fatalf("only %d accepted lists: the generator does not reach the valid side", accepted)
	}
}

func FuzzTiers(f *testing.F) {
	f.Add(int64(10_000), 0, int64(1_000_000), 1, int64(0), 6, uint8(3), int64(10_001))
	f.Add(int64(0), 0, int64(0), 0, int64(0), 0, uint8(1), int64(1))
	f.Add(int64(1000), 1, int64(0), 0, int64(0), 0, uint8(2), int64(1))
	f.Add(int64(0), 101, int64(0), 0, int64(0), 0, uint8(1), store.MaxSat)
	f.Fuzz(func(t *testing.T, u0 int64, c0 int, u1 int64, c1 int, u2 int64, c2 int, n uint8, amount int64) {
		tiers := []Tier{{UpTo: u0, Confirmations: c0}, {UpTo: u1, Confirmations: c1}, {UpTo: u2, Confirmations: c2}}[:n%4]
		checkTiers(t, tiers, []int64{1, amount, store.MaxSat})
	})
}
