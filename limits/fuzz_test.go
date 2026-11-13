package limits

import (
	"errors"
	"math"
	"testing"
	"time"
)

// FuzzConfig: New never panics; a refusal wraps ErrConfig; an accepted limiter holds a resolved
// configuration inside every documented bound, and Admit on it never panics, respects the amount
// bounds and refuses with ErrAccountCap exactly at the per-account cap.
func FuzzConfig(f *testing.F) {
	f.Add(1.0, 10, 1000, 0.2, 5000, int64(1e10), int64(1000), int64(1e9), 0)
	f.Add(0.0, 0, 0, 0.0, 0, int64(0), int64(0), int64(0), 0)
	f.Add(math.NaN(), -1, -1, math.NaN(), -1, int64(-1), int64(-1), int64(-1), -1)
	f.Add(math.Inf(1), math.MaxInt, math.MaxInt, math.Inf(-1), math.MaxInt, int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MinInt64), math.MaxInt)
	f.Add(1e6, 1<<20, 1<<30, 1.0, 1, int64(1<<53), int64(1), int64(1<<53), 1<<30)
	f.Add(1e-300, 1, 1, 1e-300, 1, int64(1), int64(1), int64(1), 1)
	f.Add(0.5, 3, 3, 0.29, 100, int64(5), int64(6), int64(5), 4)
	f.Add(1.0, 10, 3, 0.5, 5000, int64(1e10), int64(1000), int64(1e9), 0)       // the default is MaxOpen
	f.Add(1.0, 10, 1000, 0.2, 5000, int64(1e10), int64(1000), int64(1e9), 1001) // MaxOpen + 1
	f.Add(1.0, 10, 1000, 0.2, 5000, int64(1e10), int64(1000), int64(1e9), 1000) // MaxOpen
	f.Add(1.0, 10, 1000, 0.2, 5000, int64(1e10), int64(1000), int64(1e9), math.MinInt)
	f.Add(1.0, 10, 2, 0.5, 5000, int64(1e10), int64(1000), int64(1e9), 1)
	f.Fuzz(func(t *testing.T, rate float64, burst, openCap int, share float64, dc int, dailyIn, minIn, maxIn int64, perAcct int) {
		l, err := New(Config{RatePerSec: rate, Burst: burst, MaxOpen: openCap, NewShare: share,
			DailyCount: dc, DailyMsat: dailyIn, MinMsat: minIn, MaxMsat: maxIn, MaxOpenPerAccount: perAcct},
			func() time.Time { return time.Unix(0, 0) })
		if err != nil {
			if !errors.Is(err, ErrConfig) || l != nil {
				t.Fatalf("bad refusal: %v", err)
			}
			return
		}
		c := l.Config()
		if math.IsNaN(c.RatePerSec) || c.RatePerSec <= 0 || c.RatePerSec > maxRate ||
			c.Burst < 1 || c.Burst > maxBurst || c.MaxOpen < 1 || c.MaxOpen > 1<<30 ||
			math.IsNaN(c.NewShare) || c.NewShare < 0 || c.NewShare > 1 || c.DailyCount < 1 ||
			c.MinMsat < 1 || c.MinMsat > c.MaxMsat || c.MaxMsat > maxMsat ||
			c.DailyMsat < 1 || c.DailyMsat > maxMsat || c.MaxOpenPerAccount < 1 || c.MaxOpenPerAccount > c.MaxOpen {
			t.Fatalf("accepted out-of-bounds config %+v", c)
		}
		if (perAcct == 0 && c.MaxOpenPerAccount != min(DefaultMaxOpenPerAccount, c.MaxOpen)) || (perAcct != 0 && c.MaxOpenPerAccount != perAcct) {
			t.Fatalf("MaxOpenPerAccount %d resolved to %d (MaxOpen %d)", perAcct, c.MaxOpenPerAccount, c.MaxOpen)
		}
		if l.newCap < 1 || l.newCap >= c.MaxOpen {
			t.Fatalf("newCap %d of %d", l.newCap, c.MaxOpen)
		}
		for _, amt := range []int64{math.MinInt64, 0, c.MinMsat - 1, c.MinMsat, c.MaxMsat, c.MaxMsat + 1, math.MaxInt64} {
			for _, class := range []Class{ClassNew, ClassKnown} {
				err := l.Admit(class, amt, 0, 0, 0, 0, math.MaxInt64)
				if err == nil {
					t.Fatalf("admitted with a full day (amount %d)", amt)
				}
				if (amt < c.MinMsat || amt > c.MaxMsat) != (err == ErrAmount) {
					t.Fatalf("amount %d bounds [%d,%d]: %v", amt, c.MinMsat, c.MaxMsat, err)
				}
			}
		}
		// The account cap, around its edge; a Known request passes the share, so with a day that has room
		// only the open cap, the account cap and the rate can refuse.
		if c.MinMsat <= c.DailyMsat {
			p := c.MaxOpenPerAccount
			for _, oa := range []int{0, p - 1, p, p + 1} {
				if oa < 0 {
					continue
				}
				openNow := max(oa, p)
				err := l.Admit(ClassKnown, c.MinMsat, openNow, 0, oa, 0, 0)
				switch {
				case openNow >= c.MaxOpen:
					if err != ErrOpenCap {
						t.Fatalf("open %d of %d: %v", openNow, c.MaxOpen, err)
					}
				case (oa >= p) != (err == ErrAccountCap):
					t.Fatalf("account %d of %d: %v", oa, p, err)
				case err != nil && err != ErrAccountCap && err != ErrRate:
					t.Fatalf("account %d of %d: %v", oa, p, err)
				}
			}
		}
	})
}
