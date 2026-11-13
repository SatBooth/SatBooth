package rates

import (
	"errors"
	"math/big"
	"sort"
	"time"
)

// FutureSkew is how far ahead of the clock a rate's timestamp may be and still count as fresh.
const FutureSkew = 60 * time.Second

// The refusals of Aggregate. None carries a rate, a provider or a currency.
var (
	ErrTooFew    = errors.New("rates: too few fresh rate sources")
	ErrDisagree  = errors.New("rates: rate sources disagree beyond the spread bound")
	ErrDuplicate = errors.New("rates: two fresh rates from one provider")
)

// Policy is when a set of rates makes a quote: at least MinSources fresh rates (no older than MaxAge),
// whose highest and lowest differ by at most MaxSpreadBps basis points of the lowest. There is no outlier
// dropping: a spread over the bound refuses the quote (fail closed).
type Policy struct {
	MinSources   int
	MaxSpreadBps int64
	MaxAge       time.Duration
}

// Quote is the rate of an asset in a currency: Rate is the median of Sources before any margin,
// Sources the fresh rates that went into it, sorted by provider name, and At the aggregation time.
type Quote struct {
	Asset, Currency string
	Rate            Decimal
	Sources         []Rate
	At              time.Time
}

// Fresh returns the rates r with now − maxAge ≤ r.At ≤ now + FutureSkew (both ends inclusive), in their
// order, in a new slice.
func Fresh(rs []Rate, now time.Time, maxAge time.Duration) []Rate {
	oldest, newest := now.Add(-maxAge), now.Add(FutureSkew)
	out := make([]Rate, 0, len(rs))
	for _, r := range rs {
		if !r.At.Before(oldest) && !r.At.After(newest) {
			out = append(out, r)
		}
	}
	return out
}

// Median returns the median Value of rs: the middle one for an odd count, and for an even count the
// floor of the mean of the two middle ones, in units. It panics on an empty slice; callers check.
func Median(rs []Rate) Decimal {
	if len(rs) == 0 {
		panic("rates: Median of no rates")
	}
	vs := make([]Decimal, len(rs))
	for i, r := range rs {
		vs[i] = r.Value
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].Cmp(vs[j]) < 0 })
	mid := len(vs) / 2
	if len(vs)%2 == 1 {
		return vs[mid]
	}
	sum := new(big.Int).Add(vs[mid-1].value(), vs[mid].value())
	sum.Rsh(sum, 1) // floor for a non-negative sum
	if sum.Sign() == 0 {
		return Decimal{}
	}
	return Decimal{units: sum}
}

// SpreadExceeds reports whether the highest and lowest Value of rs differ by more than maxBps basis
// points of the lowest: (max − min) × 10000 > maxBps × min, exactly, in units. It is false for fewer than
// 2 rates.
func SpreadExceeds(rs []Rate, maxBps int64) bool {
	if len(rs) < 2 {
		return false
	}
	lo, hi := rs[0].Value, rs[0].Value
	for _, r := range rs[1:] {
		if r.Value.Cmp(lo) < 0 {
			lo = r.Value
		}
		if r.Value.Cmp(hi) > 0 {
			hi = r.Value
		}
	}
	diff := new(big.Int).Sub(hi.value(), lo.value())
	diff.Mul(diff, bpsPerOne)
	bound := new(big.Int).Mul(big.NewInt(maxBps), lo.value())
	return diff.Cmp(bound) > 0
}

// Aggregate makes the quote of asset in currency from rs at now: the fresh rates (Fresh), then
// ErrDuplicate when two of them come from one provider (a provider never counts twice), then ErrTooFew
// when fewer than p.MinSources are left, then ErrDisagree when their spread exceeds p.MaxSpreadBps, else
// the median. The caller passes the rates of that asset and currency only.
func Aggregate(asset, currency string, rs []Rate, now time.Time, p Policy) (Quote, error) {
	q, _, err := aggregate(asset, currency, rs, now, p)
	return q, err
}

// aggregate is Aggregate that also returns the fresh rates, sorted by provider, on every outcome: the
// service names them in its log lines.
func aggregate(asset, currency string, rs []Rate, now time.Time, p Policy) (Quote, []Rate, error) {
	fresh := Fresh(rs, now, p.MaxAge)
	sortRates(fresh)
	for i := 1; i < len(fresh); i++ {
		if fresh[i].Provider == fresh[i-1].Provider { // sorted by provider: duplicates are adjacent
			return Quote{}, fresh, ErrDuplicate
		}
	}
	if len(fresh) == 0 || len(fresh) < p.MinSources {
		return Quote{}, fresh, ErrTooFew
	}
	if SpreadExceeds(fresh, p.MaxSpreadBps) {
		return Quote{}, fresh, ErrDisagree
	}
	return Quote{Asset: asset, Currency: currency, Rate: Median(fresh), Sources: fresh, At: now}, fresh, nil
}

// sortRates sorts by provider name, then value, then timestamp, so the order never depends on the input.
func sortRates(rs []Rate) {
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if c := a.Value.Cmp(b.Value); c != 0 {
			return c < 0
		}
		return a.At.Before(b.At)
	})
}
