package limits

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock               { return &clock{time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)} }
func baseCfg() Config {
	return Config{MaxMsat: 1_000_000_000, DailyMsat: 10_000_000_000}
}

func mustNew(t testing.TB, cfg Config, c *clock) *Limiter {
	t.Helper()
	l, err := New(cfg, c.now)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	return l
}

func TestDefaults(t *testing.T) {
	l := mustNew(t, baseCfg(), newClock())
	got := l.Config()
	want := Config{RatePerSec: 1, Burst: 10, MaxOpen: 1000, NewShare: 0.2, DailyCount: 5000,
		DailyMsat: 10_000_000_000, MinMsat: 1000, MaxMsat: 1_000_000_000, MaxOpenPerAccount: 5}
	if got != want {
		t.Fatalf("defaults: got %+v want %+v", got, want)
	}
	if l.newCap != 200 {
		t.Fatalf("newCap %d", l.newCap)
	}
	// The per-account default is min(5, MaxOpen), applied after MaxOpen's own default (Ruling 6.1-N).
	for _, c := range []struct {
		open  int
		share float64
	}{{1, 0}, {2, 0.5}, {3, 0.5}, {4, 0.5}, {5, 0}, {6, 0}, {1 << 30, 0}} {
		cfg := baseCfg()
		cfg.MaxOpen, cfg.NewShare = c.open, c.share
		if c.open == 1 { // a share needs MaxOpen 2 or more; MaxOpen 1 is refused whatever the cap
			if _, err := New(cfg, newClock().now); !errors.Is(err, ErrConfig) {
				t.Errorf("MaxOpen 1: %v", err)
			}
			continue
		}
		want := min(DefaultMaxOpenPerAccount, c.open)
		if got := mustNew(t, cfg, newClock()).Config().MaxOpenPerAccount; got != want {
			t.Errorf("MaxOpen %d: MaxOpenPerAccount %d, want %d", c.open, got, want)
		}
	}
	if DefaultMaxOpenPerAccount != 5 {
		t.Fatalf("DefaultMaxOpenPerAccount %d", DefaultMaxOpenPerAccount)
	}
}

// MaxOpenPerAccount is 1 to MaxOpen; a refusal names the rule, and MaxOpen's own refusal comes first.
func TestNewPerAccountBounds(t *testing.T) {
	const rule = "MaxOpenPerAccount must be in [1, MaxOpen]"
	for _, c := range []struct {
		open, per int
		ok        bool
	}{
		{0, 0, true}, {0, 1, true}, {0, 5, true}, {0, 999, true}, {0, 1000, true}, {0, 1001, false}, {0, -1, false},
		{0, math.MinInt, false}, {0, math.MaxInt, false}, {3, 0, true}, {3, 1, true}, {3, 3, true}, {3, 4, false},
		{10, 10, true}, {10, 11, false}, {1 << 30, 1 << 30, true}, {1 << 30, 1<<30 + 1, false},
	} {
		cfg := baseCfg()
		cfg.MaxOpen, cfg.MaxOpenPerAccount = c.open, c.per
		if c.open == 3 {
			cfg.NewShare = 0.5
		}
		l, err := New(cfg, newClock().now)
		if c.ok != (err == nil) {
			t.Errorf("MaxOpen %d MaxOpenPerAccount %d: ok=%v, got %v", c.open, c.per, c.ok, err)
			continue
		}
		if err != nil && err.Error() != ErrConfig.Error()+": "+rule {
			t.Errorf("MaxOpen %d MaxOpenPerAccount %d: %v", c.open, c.per, err)
		}
		if err == nil && c.per != 0 && l.Config().MaxOpenPerAccount != c.per {
			t.Errorf("MaxOpen %d MaxOpenPerAccount %d: kept %d", c.open, c.per, l.Config().MaxOpenPerAccount)
		}
	}
	// A bad MaxOpen reads MaxOpen's refusal, not the per-account one its min(5, MaxOpen) default would fail.
	for _, open := range []int{-1, math.MinInt, 1<<30 + 1} {
		cfg := baseCfg()
		cfg.MaxOpen = open
		if _, err := New(cfg, newClock().now); err == nil || err.Error() != ErrConfig.Error()+": MaxOpen must be in [1, 2^30]" {
			t.Errorf("MaxOpen %d: %v", open, err)
		}
	}
}

func TestNewRefuses(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	bad := map[string]func(*Config){
		"no MaxMsat":       func(c *Config) { c.MaxMsat = 0 },
		"no DailyMsat":     func(c *Config) { c.DailyMsat = 0 },
		"neg DailyMsat":    func(c *Config) { c.DailyMsat = -1 },
		"neg MaxMsat":      func(c *Config) { c.MaxMsat = -5 },
		"MaxMsat 2^53+1":   func(c *Config) { c.MaxMsat = 1<<53 + 1 },
		"DailyMsat 2^53+1": func(c *Config) { c.DailyMsat = 1<<53 + 1 },
		"min > max":        func(c *Config) { c.MinMsat = c.MaxMsat + 1 },
		"neg min":          func(c *Config) { c.MinMsat = -1 },
		"NaN rate":         func(c *Config) { c.RatePerSec = nan },
		"Inf rate":         func(c *Config) { c.RatePerSec = inf },
		"neg rate":         func(c *Config) { c.RatePerSec = -1 },
		"huge rate":        func(c *Config) { c.RatePerSec = 1e7 },
		"neg burst":        func(c *Config) { c.Burst = -1 },
		"huge burst":       func(c *Config) { c.Burst = 1<<20 + 1 },
		"neg open":         func(c *Config) { c.MaxOpen = -1 },
		"huge open":        func(c *Config) { c.MaxOpen = 1<<30 + 1 },
		"NaN share":        func(c *Config) { c.NewShare = nan },
		"neg share":        func(c *Config) { c.NewShare = -0.1 },
		"share > 1":        func(c *Config) { c.NewShare = 1.0000001 },
		"Inf share":        func(c *Config) { c.NewShare = inf },
		"neg daily count":  func(c *Config) { c.DailyCount = -1 },
	}
	for name, mut := range bad {
		cfg := baseCfg()
		mut(&cfg)
		if _, err := New(cfg, newClock().now); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: got %v, want ErrConfig", name, err)
		}
	}
	if _, err := New(baseCfg(), nil); !errors.Is(err, ErrConfig) {
		t.Errorf("nil clock: %v", err)
	}
	// A share must leave one slot to each class: floor(share × MaxOpen) in [1, MaxOpen-1].
	for _, c := range []struct {
		share float64
		open  int
		ok    bool
	}{
		{0.0000001, 1000, false}, {0.0009, 1000, false}, {0.001, 1000, true}, {0.999, 1000, true},
		{0.9999, 1000, true}, {0.9999999999999, 1000, false}, {1, 1000, false}, {1, 7, false}, {0.999, 1, false}, {0.5, 1, false},
		{0.2, 1, false}, {0.5, 2, true}, {0.29, 100, true},
	} {
		cfg := baseCfg()
		cfg.NewShare, cfg.MaxOpen = c.share, c.open
		_, err := New(cfg, newClock().now)
		if c.ok != (err == nil) || (err != nil && !errors.Is(err, ErrConfig)) {
			t.Errorf("share %v of %d: ok=%v, got %v", c.share, c.open, c.ok, err)
		}
	}
}

// Every refused share gets the one statement of the rule, the same as the Config doc.
func TestNewShareRuleIsOneStatement(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	for _, c := range []struct {
		share float64
		open  int
	}{{nan, 1000}, {inf, 1000}, {-0.1, 1000}, {1, 1000}, {1.0000001, 1000}, {0.0000001, 1000}, {0.5, 1}, {0.999, 1}} {
		cfg := baseCfg()
		cfg.NewShare, cfg.MaxOpen = c.share, c.open
		_, err := New(cfg, newClock().now)
		if !errors.Is(err, ErrConfig) || err.Error() != ErrConfig.Error()+": "+shareRule {
			t.Errorf("share %v of %d: %v", c.share, c.open, err)
		}
	}
	for _, ok := range []float64{0, 0.2, 0.5, 0.999} {
		cfg := baseCfg()
		cfg.NewShare = ok
		if _, err := New(cfg, newClock().now); err != nil {
			t.Errorf("share %v: %v", ok, err)
		}
	}
}

func TestNewShareFloor(t *testing.T) {
	for _, c := range []struct {
		share float64
		open  int
		want  int
	}{
		{0.2, 1000, 200}, {0.29, 100, 29}, {0.7, 10, 7}, {0.5, 3, 1}, {0.3, 10, 3}, {0.1, 30, 3},
		{0.9, 10, 9}, {0.5, 2, 1},
	} {
		cfg := baseCfg()
		cfg.NewShare, cfg.MaxOpen = c.share, c.open
		l := mustNew(t, cfg, newClock())
		if l.newCap != c.want {
			t.Errorf("share %v of %d: got %d want %d", c.share, c.open, l.newCap, c.want)
		}
	}
}

// fresh returns a limiter whose bucket is large and whose refill is fast, so only the check
// under test can refuse.
func fresh(t testing.TB, mut func(*Config)) *Limiter {
	cfg := baseCfg()
	cfg.Burst = 1 << 20
	cfg.MaxOpen = 10
	cfg.NewShare = 0.2 // newCap 2
	cfg.DailyCount = 5
	cfg.DailyMsat = 10_000
	cfg.MinMsat, cfg.MaxMsat = 1000, 5000
	if mut != nil {
		mut(&cfg)
	}
	return mustNew(t, cfg, newClock())
}

func TestAmountBounds(t *testing.T) {
	l := fresh(t, nil)
	for _, c := range []struct {
		amt  int64
		want error
	}{
		{math.MinInt64, ErrAmount}, {-1, ErrAmount}, {0, ErrAmount}, {999, ErrAmount}, {1000, nil},
		{1001, nil}, {4999, nil}, {5000, nil}, {5001, ErrAmount}, {math.MaxInt64, ErrAmount},
	} {
		if err := l.Admit(ClassKnown, c.amt, 0, 0, 0, 0, 0); err != c.want {
			t.Errorf("amount %d: got %v want %v", c.amt, err, c.want)
		}
	}
}

func TestDailyCount(t *testing.T) {
	l := fresh(t, nil)
	for _, c := range []struct {
		n    int
		want error
	}{{0, nil}, {4, nil}, {5, ErrDailyCount}, {6, ErrDailyCount}, {math.MaxInt, ErrDailyCount}} {
		if err := l.Admit(ClassKnown, 1000, 0, 0, 0, c.n, 0); err != c.want {
			t.Errorf("dayCount %d: got %v want %v", c.n, err, c.want)
		}
	}
}

func TestDailyAmountOverflowSafe(t *testing.T) {
	l := fresh(t, nil) // DailyMsat 10000
	for _, c := range []struct {
		day, amt int64
		want     error
	}{
		{0, 5000, nil}, {5000, 5000, nil}, {5001, 5000, ErrDailyAmount}, {9000, 1000, nil},
		{9001, 1000, ErrDailyAmount}, {10000, 1000, ErrDailyAmount},
		{math.MaxInt64, 5000, ErrDailyAmount}, {math.MaxInt64 - 1, 1000, ErrDailyAmount},
		{math.MaxInt64 - 5000, 5000, ErrDailyAmount},
	} {
		if err := l.Admit(ClassKnown, c.amt, 0, 0, 0, 0, c.day); err != c.want {
			t.Errorf("day %d + %d: got %v want %v", c.day, c.amt, err, c.want)
		}
	}
}

func TestOpenCapAndShare(t *testing.T) {
	l := fresh(t, nil) // MaxOpen 10, newCap 2; known may also use unused new slots
	check := func(name string, class Class, openNow, openNew int, want error) {
		t.Helper()
		if err := l.Admit(class, 1000, openNow, openNew, 0, 0, 0); err != want {
			t.Errorf("%s: got %v want %v", name, err, want)
		}
	}
	check("new below share", ClassNew, 1, 1, nil)
	check("new at share", ClassNew, 2, 2, ErrClassShare)
	check("new above share", ClassNew, 3, 3, ErrClassShare)
	check("new, share full, room in total", ClassNew, 5, 2, ErrClassShare)
	check("new, total full and share free", ClassNew, 10, 0, ErrOpenCap)
	check("new, total full and share full", ClassNew, 10, 2, ErrOpenCap)
	check("known with new at share", ClassKnown, 2, 2, nil)
	check("known, 7 known open", ClassKnown, 7, 0, nil)
	check("known, 8 known open, new slots unused", ClassKnown, 8, 0, nil)
	check("known, 8 known + 2 new, total full", ClassKnown, 10, 2, ErrOpenCap)
	check("known, 7 known + 2 new", ClassKnown, 9, 2, nil)
	check("known, total over", ClassKnown, 11, 0, ErrOpenCap)
	check("unknown class is new", Class(77), 2, 2, ErrClassShare)
}

// TestAccountCap: check 7 refuses at openAcct >= MaxOpenPerAccount for both classes.
func TestAccountCap(t *testing.T) {
	l := fresh(t, func(c *Config) { c.NewShare = 0.9 }) // MaxOpen 10, newCap 9, MaxOpenPerAccount 5 by default
	if l.Config().MaxOpenPerAccount != 5 {
		t.Fatalf("cap %d", l.Config().MaxOpenPerAccount)
	}
	for _, c := range []struct {
		name                       string
		class                      Class
		openNow, openNew, openAcct int
		want                       error
	}{
		{"known 5 of 5", ClassKnown, 5, 0, 5, ErrAccountCap},
		{"new 5 of 5", ClassNew, 5, 5, 5, ErrAccountCap},
		{"unknown class 5 of 5", Class(9), 7, 5, 5, ErrAccountCap},
		{"known 4 of 5", ClassKnown, 4, 0, 4, nil},
		{"new 4 of 5", ClassNew, 4, 4, 4, nil},
		{"known 4 of 5, others open", ClassKnown, 9, 3, 4, nil},
		{"known 0", ClassKnown, 9, 0, 0, nil},
		{"known 6 of 5", ClassKnown, 6, 0, 6, ErrAccountCap},
		{"known 9 of 5", ClassKnown, 9, 0, 9, ErrAccountCap},
		{"new 6 open, account 6", ClassNew, 6, 6, 6, ErrAccountCap},
		{"account 6 with 5 open", ClassKnown, 5, 0, 6, ErrBadCount},
		{"account 1 with 0 open", ClassNew, 0, 0, 1, ErrBadCount},
		{"account -1", ClassKnown, 5, 0, -1, ErrBadCount},
		{"account MinInt", ClassKnown, 5, 0, math.MinInt, ErrBadCount},
		{"account MaxInt", ClassKnown, 5, 0, math.MaxInt, ErrBadCount},
		{"total full, account full", ClassKnown, 10, 0, 5, ErrOpenCap},
	} {
		if err := l.Admit(c.class, 1000, c.openNow, c.openNew, c.openAcct, 0, 0); err != c.want {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
		}
	}
	// The cap's edges: 1 (one invoice an account) and MaxOpen (no per-account cap beyond the total).
	one := fresh(t, func(c *Config) { c.MaxOpenPerAccount = 1 })
	if err := one.Admit(ClassKnown, 1000, 3, 0, 0, 0, 0); err != nil {
		t.Errorf("cap 1, account 0: %v", err)
	}
	if err := one.Admit(ClassKnown, 1000, 3, 0, 1, 0, 0); err != ErrAccountCap {
		t.Errorf("cap 1, account 1: %v", err)
	}
	all := fresh(t, func(c *Config) { c.MaxOpenPerAccount = 10 })
	if err := all.Admit(ClassKnown, 1000, 9, 0, 9, 0, 0); err != nil {
		t.Errorf("cap MaxOpen, account 9: %v", err)
	}
	if err := all.Admit(ClassKnown, 1000, 10, 0, 10, 0, 0); err != ErrOpenCap {
		t.Errorf("cap MaxOpen, account 10: %v", err)
	}
}

func TestBadCounts(t *testing.T) {
	l := fresh(t, nil)
	for _, a := range [][5]int64{{-1, 0, 0, 0, 0}, {0, -1, 0, 0, 0}, {0, 0, 0, -1, 0}, {0, 0, 0, 0, -1}, {1, 2, 0, 0, 0},
		{0, 0, -1, 0, 0}, {5, 0, 6, 0, 0}, {0, 0, 1, 0, 0}, {3, 0, math.MaxInt, 0, 0}, {-1, 0, -1, 0, 0}} {
		if err := l.Admit(ClassKnown, 1000, int(a[0]), int(a[1]), int(a[2]), int(a[3]), a[4]); !errors.Is(err, ErrBadCount) {
			t.Errorf("%v: got %v", a, err)
		}
	}
}

func TestTotalNeverExceedsMaxOpen(t *testing.T) {
	l := fresh(t, func(c *Config) {
		c.MaxOpen = 50
		c.NewShare = 0.3
		c.DailyCount = 1 << 20
		c.DailyMsat = 1 << 50
		c.MaxOpenPerAccount = 7
	})
	rng := rand.New(rand.NewSource(7))
	// Twelve accounts; the even ones are Known. Each account's invoices are in its own class.
	var acct [12]int
	var openKnown, openNew, capped int
	for i := 0; i < 5000; i++ {
		a := rng.Intn(len(acct))
		class := ClassNew
		if a%2 == 0 {
			class = ClassKnown
		}
		switch err := l.Admit(class, 1000, openKnown+openNew, openNew, acct[a], 0, 0); err {
		case nil:
			acct[a]++
			if class == ClassNew {
				openNew++
			} else {
				openKnown++
			}
		case ErrAccountCap:
			capped++
		}
		if rng.Intn(3) == 0 { // an invoice expires
			if b := rng.Intn(len(acct)); acct[b] > 0 {
				acct[b]--
				if b%2 == 0 {
					openKnown--
				} else {
					openNew--
				}
			}
		}
		if openKnown+openNew > 50 || openNew > l.newCap {
			t.Fatalf("step %d: known %d new %d", i, openKnown, openNew)
		}
		for b, n := range acct {
			if n > 7 {
				t.Fatalf("step %d: account %d holds %d", i, b, n)
			}
		}
	}
	if capped == 0 {
		t.Fatal("the account cap never refused")
	}
}

func TestKnownUsesUnusedNewSlots(t *testing.T) {
	l := fresh(t, func(c *Config) { c.MaxOpen = 10; c.NewShare = 0.2 })
	if err := l.Admit(ClassKnown, 1000, 9, 0, 0, 0, 0); err != nil {
		t.Errorf("known at 9 open, no new: got %v", err)
	}
	if err := l.Admit(ClassKnown, 1000, 9, 2, 0, 0, 0); err != nil {
		t.Errorf("known at 9 open with 2 new: got %v", err)
	}
	if err := l.Admit(ClassKnown, 1000, 10, 0, 0, 0, 0); err != ErrOpenCap {
		t.Errorf("known at 10 open: got %v want ErrOpenCap", err)
	}
	if err := l.Admit(ClassNew, 1000, 2, 2, 0, 0, 0); err != ErrClassShare {
		t.Errorf("new at its share: got %v want ErrClassShare", err)
	}
}

// TestCheckOrder pins the order: amount, daily count, daily amount, open cap, class share, account
// cap, rate.
func TestCheckOrder(t *testing.T) {
	c := newClock()
	cfg := baseCfg()
	cfg.Burst, cfg.RatePerSec, cfg.MaxOpen, cfg.NewShare = 1, 1, 10, 0.2
	cfg.DailyCount, cfg.DailyMsat, cfg.MinMsat, cfg.MaxMsat = 5, 10_000, 1000, 5000
	l := mustNew(t, cfg, c)                                          // MaxOpenPerAccount 5 by default
	if err := l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 0); err != nil { // spend the only token
		t.Fatal(err)
	}
	// Everything is wrong at once: the rate is empty too.
	steps := []struct {
		class                    Class
		amt                      int64
		openNow, openNew, oa, dc int
		dm                       int64
		want                     error
	}{
		{ClassNew, 99999, 10, 2, 11, 5, 10_000, ErrBadCount},
		{ClassNew, 99999, 10, 2, 5, 5, 10_000, ErrAmount},
		{ClassNew, 1000, 10, 2, 5, 5, 10_000, ErrDailyCount},
		{ClassNew, 1000, 10, 2, 5, 4, 10_000, ErrDailyAmount},
		{ClassNew, 1000, 10, 2, 5, 4, 9_000, ErrOpenCap},
		{ClassNew, 1000, 5, 2, 5, 4, 9_000, ErrClassShare}, // the share and the account cap: the share
		{ClassNew, 1000, 5, 1, 5, 4, 9_000, ErrAccountCap}, // the account cap and the rate: the cap
		{ClassKnown, 1000, 5, 2, 5, 4, 9_000, ErrAccountCap},
		{ClassNew, 1000, 5, 1, 4, 4, 9_000, ErrRate},
	}
	for i, s := range steps {
		if err := l.Admit(s.class, s.amt, s.openNow, s.openNew, s.oa, s.dc, s.dm); err != s.want {
			t.Errorf("step %d: got %v want %v", i, err, s.want)
		}
	}
	// The account cap burned no token: one second later exactly one request passes.
	c.add(time.Second)
	if err := l.Admit(ClassKnown, 1000, 5, 0, 5, 0, 0); err != ErrAccountCap {
		t.Fatalf("at the cap with a token: %v", err)
	}
	if err := l.Admit(ClassKnown, 1000, 5, 0, 4, 0, 0); err != nil {
		t.Fatalf("below the cap: %v", err)
	}
	if err := l.Admit(ClassKnown, 1000, 5, 0, 4, 0, 0); err != ErrRate {
		t.Fatalf("the token is spent: %v", err)
	}
}

func TestRefusalKeepsToken(t *testing.T) {
	c := newClock()
	cfg := fresh(t, nil).Config()
	cfg.Burst = 1
	l := mustNew(t, cfg, c)
	for i := 0; i < 50; i++ { // refused for every non-rate reason
		_ = l.Admit(ClassKnown, 1, 0, 0, 0, 0, 0)
		_ = l.Admit(ClassKnown, 1000, 0, 0, 0, 5, 0)
		_ = l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 10_000)
		_ = l.Admit(ClassNew, 1000, 10, 0, 0, 0, 0)
		_ = l.Admit(ClassNew, 1000, 2, 2, 0, 0, 0)
		_ = l.Admit(ClassNew, 1000, -1, 0, 0, 0, 0)
		_ = l.Admit(ClassKnown, 1000, 5, 0, 5, 0, 0) // the account cap
		_ = l.Admit(ClassKnown, 1000, 5, 0, 6, 0, 0) // an account count above the total
	}
	if err := l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 0); err != nil {
		t.Fatalf("token was burned by refusals: %v", err)
	}
	if err := l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 0); err != ErrRate {
		t.Fatalf("second: %v", err)
	}
}

func TestBucketRefillAndBurst(t *testing.T) {
	c := newClock()
	cfg := baseCfg()
	cfg.RatePerSec, cfg.Burst = 2, 3
	l := mustNew(t, cfg, c)
	adm := func() error { return l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 0) }
	for i := 0; i < 3; i++ {
		if err := adm(); err != nil {
			t.Fatalf("burst %d: %v", i, err)
		}
	}
	if err := adm(); err != ErrRate {
		t.Fatalf("over burst: %v", err)
	}
	c.add(499 * time.Millisecond)
	if err := adm(); err != ErrRate {
		t.Fatalf("499ms: %v", err)
	}
	c.add(1 * time.Millisecond) // 500 ms total at 2/s is one token
	if err := adm(); err != nil {
		t.Fatalf("500ms: %v", err)
	}
	if err := adm(); err != ErrRate {
		t.Fatalf("after: %v", err)
	}
	c.add(24 * time.Hour * 365) // a long idle never builds more than the burst
	n := 0
	for adm() == nil {
		n++
		if n > 10 {
			break
		}
	}
	if n != 3 {
		t.Fatalf("burst after idle: %d", n)
	}
}

func TestBucketSlowRate(t *testing.T) {
	// 0.5/s with a call every nanosecond must still refill: the fraction is not lost.
	c := newClock()
	cfg := baseCfg()
	cfg.RatePerSec, cfg.Burst = 0.5, 1
	l := mustNew(t, cfg, c)
	if err := l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	ok := false
	for i := 0; i < 3000; i++ {
		c.add(time.Millisecond)
		if l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 0) == nil {
			ok = true
			if i < 1990 {
				t.Fatalf("refilled early at %d ms", i)
			}
			break
		}
	}
	if !ok {
		t.Fatal("never refilled")
	}
	// A thousand 1 ns calls first (each earns less than a unit), then the rest of the 2 s.
	l = mustNew(t, cfg, c)
	_ = l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 0)
	for i := 0; i < 1000; i++ {
		c.add(time.Nanosecond)
		_ = l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 0)
	}
	c.add(2*time.Second - 1000*time.Nanosecond)
	if err := l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 0); err != nil {
		t.Fatalf("fraction lost: %v", err)
	}
}

func TestClockBackwards(t *testing.T) {
	c := newClock()
	cfg := baseCfg()
	cfg.RatePerSec, cfg.Burst = 1, 1
	l := mustNew(t, cfg, c)
	_ = l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 0)
	c.add(-time.Hour)
	if err := l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 0); err != ErrRate {
		t.Fatalf("backwards: %v", err)
	}
	c.add(time.Hour + time.Second) // one second after the backwards reading
	if err := l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 0); err != nil {
		t.Fatalf("forward again: %v", err)
	}
}

func TestConcurrentBurstExact(t *testing.T) {
	c := newClock() // frozen: only the burst can pass
	cfg := baseCfg()
	cfg.Burst = 37
	l := mustNew(t, cfg, c)
	var ok, rate atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				switch err := l.Admit(ClassKnown, 1000, 0, 0, 0, 0, 0); err {
				case nil:
					ok.Add(1)
				case ErrRate:
					rate.Add(1)
				default:
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 37 || rate.Load() != 1600-37 {
		t.Fatalf("ok %d rate %d", ok.Load(), rate.Load())
	}
}

func TestDayStart(t *testing.T) {
	loc := time.FixedZone("x", 5*3600+30*60)
	for _, c := range []struct{ in, want time.Time }{
		{time.Date(2026, 11, 3, 23, 59, 59, 999, time.UTC), time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 11, 4, 3, 0, 0, 0, loc), time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)}, // 21:30 UTC the day before
		{time.Date(2026, 11, 4, 7, 0, 0, 0, loc), time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)},
	} {
		got := DayStart(c.in)
		if !got.Equal(c.want) || got.Location() != time.UTC {
			t.Errorf("DayStart(%v) = %v want %v", c.in, got, c.want)
		}
	}
}

// refModel is an independent restatement of the rules: big integers for the sums, integer
// milli-tokens for the bucket (the test uses whole rates and millisecond steps, so it is exact).
type refModel struct {
	cfg        Config
	newCap     int
	perAcct    int   // the resolved per-account cap
	milli      int64 // milli-tokens
	rate       int64 // tokens per second
	lastMillis int64
}

func (m *refModel) admit(nowMs int64, class Class, amt int64, openNow, openNew, openAcct, dc int, dm int64) error {
	if openNow < 0 || openNew < 0 || dc < 0 || dm < 0 || openNew > openNow || openAcct < 0 || openAcct > openNow {
		return ErrBadCount
	}
	if amt < m.cfg.MinMsat || amt > m.cfg.MaxMsat {
		return ErrAmount
	}
	if dc >= m.cfg.DailyCount {
		return ErrDailyCount
	}
	sum := new(big.Int).Add(big.NewInt(dm), big.NewInt(amt))
	if sum.Cmp(big.NewInt(m.cfg.DailyMsat)) > 0 {
		return ErrDailyAmount
	}
	if openNow >= m.cfg.MaxOpen {
		return ErrOpenCap
	}
	if class != ClassKnown && openNew >= m.newCap {
		return ErrClassShare
	}
	if openAcct >= m.perAcct {
		return ErrAccountCap
	}
	// bucket
	if nowMs > m.lastMillis {
		m.milli += (nowMs - m.lastMillis) * m.rate
		if lim := int64(m.cfg.Burst) * 1000; m.milli > lim {
			m.milli = lim
		}
	}
	m.lastMillis = nowMs
	if m.milli < 1000 {
		return ErrRate
	}
	m.milli -= 1000
	return nil
}

func TestPropertyAgainstModel(t *testing.T) {
	// The per-account cap of each seed: the default (5), its edges 1 and MaxOpen, and values between.
	caps := []struct{ set, resolved int }{{0, 5}, {1, 1}, {6, 6}, {39, 39}, {40, 40}}
	for seed := int64(1); seed <= 5; seed++ {
		rng := rand.New(rand.NewSource(seed))
		pc := caps[seed-1]
		cfg := Config{RatePerSec: 3, Burst: 5, MaxOpen: 40, NewShare: 0.25, DailyCount: 30,
			DailyMsat: 100_000, MinMsat: 1000, MaxMsat: 20_000, MaxOpenPerAccount: pc.set}
		c := newClock()
		l := mustNew(t, cfg, c)
		m := &refModel{cfg: cfg, newCap: 10, perAcct: pc.resolved, milli: 5000, rate: 3}
		start := c.t
		edge := []int64{math.MinInt64, -1, 0, 999, 1000, 20_000, 20_001, math.MaxInt64}
		counts := func(max int) int {
			if rng.Intn(20) == 0 {
				return []int{-1, math.MaxInt}[rng.Intn(2)]
			}
			return rng.Intn(max)
		}
		stats := map[error]int{}
		burst := 0 // calls left in a burst on a frozen clock, so the bucket also runs dry
		for i := 0; i < 10_000; i++ {
			switch {
			case burst > 0:
				burst--
			case rng.Intn(100) == 0:
				burst = 20
			case rng.Intn(3) > 0:
				c.add(time.Duration(rng.Intn(700)) * time.Millisecond)
			}
			var amt int64
			if rng.Intn(6) == 0 {
				amt = edge[rng.Intn(len(edge))]
			} else {
				amt = 1000 + rng.Int63n(19_001)
			}
			openNow := rng.Intn(45)
			openNew := rng.Intn(12)
			if rng.Intn(4) > 0 && openNew > openNow {
				openNew = openNow
			}
			if rng.Intn(30) == 0 {
				openNow, openNew = counts(45), counts(12)
			}
			// openAcct: in [0, openNow], mostly below the cap (so the bucket still drains), often at
			// openNow or around the cap; now and then out of range.
			var openAcct int
			switch r := rng.Intn(24); {
			case openNow < 0 || openNow > 1000: // counts gave -1 or MaxInt
				openAcct = []int{-1, 0, 5, math.MaxInt}[rng.Intn(4)]
			case r == 0:
				openAcct = []int{-1, math.MinInt, openNow + 1, math.MaxInt}[rng.Intn(4)]
			case r < 4:
				openAcct = openNow
			case r < 8:
				openAcct = min(openNow, max(0, pc.resolved-1+rng.Intn(3))) // cap - 1, cap, cap + 1
			case r < 12:
				openAcct = rng.Intn(openNow + 1)
			default:
				openAcct = rng.Intn(min(openNow, pc.resolved-1) + 1)
			}
			dc := rng.Intn(35)
			var dm int64
			switch rng.Intn(10) {
			case 0:
				dm = math.MaxInt64 - rng.Int63n(30_000)
			case 1:
				dm = -1
			default:
				dm = rng.Int63n(110_000)
			}
			class := Class(rng.Intn(3)) // 2 is invalid, treated as ClassNew
			want := m.admit(c.t.Sub(start).Milliseconds(), class, amt, openNow, openNew, openAcct, dc, dm)
			got := l.Admit(class, amt, openNow, openNew, openAcct, dc, dm)
			if got != want && !(errors.Is(got, ErrBadCount) && errors.Is(want, ErrBadCount)) {
				t.Fatalf("seed %d call %d: got %v want %v (class %d amt %d open %d/%d account %d day %d/%d)",
					seed, i, got, want, class, amt, openNow, openNew, openAcct, dc, dm)
			}
			stats[got]++
		}
		outcomes := []error{nil, ErrRate, ErrOpenCap, ErrClassShare, ErrDailyCount, ErrDailyAmount, ErrAmount, ErrBadCount}
		if pc.resolved < cfg.MaxOpen { // at MaxOpen the open cap always refuses first
			outcomes = append(outcomes, ErrAccountCap)
		} else if stats[ErrAccountCap] != 0 {
			t.Errorf("seed %d: the account cap refused at MaxOpen", seed)
		}
		for _, e := range outcomes {
			if stats[e] == 0 {
				t.Errorf("seed %d: outcome %v never exercised", seed, e)
			}
		}
	}
}
