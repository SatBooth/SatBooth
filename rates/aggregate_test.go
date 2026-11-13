package rates

import (
	"errors"
	"math/big"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// now2030 is the aggregation time of these tests: a whole second, so stamps truncated to seconds or
// milliseconds land exactly on it.
var now2030 = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

// unitsDec is a Decimal of exactly u units.
func unitsDec(u int64) Decimal { return Decimal{units: big.NewInt(u)} }

func rate(t testing.TB, provider, value string, at time.Time) Rate {
	t.Helper()
	return Rate{Provider: provider, Currency: "USD", Value: dec(t, value), At: at}
}

func names(rs []Rate) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Provider
	}
	return out
}

func sameRates(a, b []Rate) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Provider != b[i].Provider || a[i].Currency != b[i].Currency || a[i].Value.Cmp(b[i].Value) != 0 || !a[i].At.Equal(b[i].At) {
			return false
		}
	}
	return true
}

func TestFreshBoundaries(t *testing.T) {
	const maxAge = 300 * time.Second
	in := []Rate{
		rate(t, "a", "1", now2030.Add(-maxAge)),                 // kept: the old end, inclusive
		rate(t, "b", "1", now2030.Add(-maxAge-time.Nanosecond)), // dropped
		rate(t, "c", "1", now2030.Add(FutureSkew)),              // kept: the future end, inclusive
		rate(t, "d", "1", now2030.Add(FutureSkew+time.Nanosecond)),
		rate(t, "e", "1", now2030),
	}
	got := Fresh(in, now2030, maxAge)
	if want := []string{"a", "c", "e"}; !equalStrings(names(got), want) {
		t.Fatalf("Fresh kept %v, want %v", names(got), want)
	}
	if FutureSkew != 60*time.Second {
		t.Fatalf("FutureSkew = %v", FutureSkew)
	}
	// The input is not changed, and a timestamp in another location compares by instant.
	if len(in) != 5 || in[1].Provider != "b" {
		t.Fatal("Fresh changed its input")
	}
	local := []Rate{{Provider: "x", Value: unitsDec(1), At: time.Unix(now2030.Add(-maxAge).Unix(), 0)}}
	if len(Fresh(local, now2030, maxAge)) != 1 {
		t.Fatal("a stamp in the local location at the bound was dropped")
	}
	if len(Fresh(nil, now2030, maxAge)) != 0 {
		t.Fatal("Fresh(nil) not empty")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMedian(t *testing.T) {
	vals := func(vs ...string) []Rate {
		rs := make([]Rate, len(vs))
		for i, v := range vs {
			rs[i] = rate(t, "p"+strconv.Itoa(i), v, now2030)
		}
		return rs
	}
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"76000"}, "76000"},
		{[]string{"76000", "76001"}, "76000.5"},
		{[]string{"76001", "76000"}, "76000.5"},
		{[]string{"3", "1", "2"}, "2"},
		{[]string{"4", "1", "3", "2"}, "2.5"},
		{[]string{"10", "40", "20", "30", "50"}, "30"},
		{[]string{"7", "7", "7", "7"}, "7"},
		{[]string{"7", "7", "7"}, "7"},
		{[]string{"1", "9", "9", "1"}, "5"},
	} {
		if got := Median(vals(c.in...)); got.Cmp(dec(t, c.want)) != 0 {
			t.Errorf("Median(%v) = %s, want %s", c.in, got, c.want)
		}
	}
	// Units ending in an odd digit: (1 + 2) / 2 = 1.5 units, floored to 1 unit.
	got := Median([]Rate{{Provider: "a", Value: unitsDec(2)}, {Provider: "b", Value: unitsDec(1)}})
	if got.Units().Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("Median of 1 and 2 units = %s units, want 1", got.Units())
	}
	got = Median([]Rate{{Provider: "a", Value: unitsDec(5)}, {Provider: "b", Value: unitsDec(8)}})
	if got.Units().Cmp(big.NewInt(6)) != 0 {
		t.Fatalf("Median of 5 and 8 units = %s units, want 6", got.Units())
	}
	// The input order is kept.
	in := vals("3", "1", "2")
	Median(in)
	if in[0].Value.Cmp(dec(t, "3")) != 0 || in[1].Value.Cmp(dec(t, "1")) != 0 {
		t.Fatal("Median reordered its input")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("Median of no rates did not panic")
		}
	}()
	Median(nil)
}

func TestSpreadBoundaries(t *testing.T) {
	two := func(lo, hi Decimal) []Rate {
		return []Rate{{Provider: "a", Value: hi}, {Provider: "b", Value: lo}}
	}
	min := dec(t, "100000")
	at := dec(t, "102000")
	over := Decimal{units: new(big.Int).Add(at.Units(), big.NewInt(1))}
	if SpreadExceeds(two(min, at), 200) {
		t.Error("exactly 2 % exceeds 200 bps")
	}
	if !SpreadExceeds(two(min, over), 200) {
		t.Error("2 % plus one unit does not exceed 200 bps")
	}
	if SpreadExceeds([]Rate{{Provider: "a", Value: min}}, 200) {
		t.Error("one rate exceeds")
	}
	if SpreadExceeds(nil, 200) {
		t.Error("no rate exceeds")
	}
	if !SpreadExceeds(two(unitsDec(1), unitsDec(2)), 200) {
		t.Error("1 and 2 units (100 %) do not exceed 200 bps")
	}
	if SpreadExceeds(two(unitsDec(1), unitsDec(2)), 10000) {
		t.Error("1 and 2 units exceed 10000 bps (exactly 100 %)")
	}
	if SpreadExceeds(two(min, min), 1) {
		t.Error("equal rates exceed")
	}
	// The middle rates do not matter, only the extremes; order does not matter.
	rs := []Rate{{Provider: "a", Value: dec(t, "101000")}, {Provider: "b", Value: at}, {Provider: "c", Value: min}}
	if SpreadExceeds(rs, 200) || !SpreadExceeds(rs, 199) {
		t.Error("three rates: the bound is not on max and min")
	}
}

func TestAggregateRules(t *testing.T) {
	p := Policy{MinSources: 2, MaxSpreadBps: 200, MaxAge: 300 * time.Second}
	stale := now2030.Add(-301 * time.Second)

	// 1 fresh of 3: too few.
	rs := []Rate{rate(t, "kraken", "76000", now2030), rate(t, "coinbase", "76000", stale), rate(t, "yadio", "76000", stale)}
	if _, err := Aggregate("BTC", "USD", rs, now2030, p); !errors.Is(err, ErrTooFew) {
		t.Fatalf("1 fresh of 3: %v, want ErrTooFew", err)
	}
	// 2 fresh: a quote, Sources sorted by provider.
	rs[2].At = now2030.Add(-time.Second)
	q, err := Aggregate("BTC", "USD", rs, now2030, p)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"kraken", "yadio"}; !equalStrings(names(q.Sources), want) {
		t.Fatalf("Sources %v, want %v", names(q.Sources), want)
	}
	if q.Asset != "BTC" || q.Currency != "USD" || !q.At.Equal(now2030) || q.Rate.Cmp(dec(t, "76000")) != 0 {
		t.Fatalf("quote %+v", q)
	}
	// The spread at the bound passes; one unit over fails.
	rs = []Rate{rate(t, "z", "100000", now2030), rate(t, "a", "102000", now2030)}
	q, err = Aggregate("BTC", "USD", rs, now2030, p)
	if err != nil {
		t.Fatalf("spread at the bound: %v", err)
	}
	if !equalStrings(names(q.Sources), []string{"a", "z"}) || q.Rate.Cmp(dec(t, "101000")) != 0 {
		t.Fatalf("quote %+v", q)
	}
	rs[1].Value = dec(t, "102000.000000000000000001")
	if _, err := Aggregate("BTC", "USD", rs, now2030, p); !errors.Is(err, ErrDisagree) {
		t.Fatalf("one unit over: %v, want ErrDisagree", err)
	}
	// A stale outlier is not counted in the spread.
	rs = append(rs, rate(t, "m", "1", stale))
	rs[1].Value = dec(t, "102000")
	if _, err := Aggregate("BTC", "USD", rs, now2030, p); err != nil {
		t.Fatalf("stale outlier counted: %v", err)
	}
	// MinSources 1 with one provider: a quote.
	p1 := Policy{MinSources: 1, MaxSpreadBps: 200, MaxAge: 300 * time.Second}
	q, err = Aggregate("BTC", "USD", []Rate{rate(t, "custom", "76000", now2030)}, now2030, p1)
	if err != nil || len(q.Sources) != 1 || q.Rate.Cmp(dec(t, "76000")) != 0 {
		t.Fatalf("MinSources 1: %+v, %v", q, err)
	}
	// No rate at all: too few, even with MinSources 1.
	if _, err := Aggregate("BTC", "USD", nil, now2030, p1); !errors.Is(err, ErrTooFew) {
		t.Fatalf("no rates: %v", err)
	}
	// Errors carry no values.
	for _, e := range []error{ErrTooFew, ErrDisagree} {
		for _, bad := range []string{"76000", "102000", "100000", "BTC", "USD"} {
			if strings.Contains(e.Error(), bad) {
				t.Errorf("%q contains %q", e, bad)
			}
		}
	}
}

// TestAggregateProperty draws 10,000 sets of 1 to 8 rates, some stale, some spread, and checks: the
// median lies between the fresh min and max; the result does not depend on the input order; removing a
// stale rate changes nothing.
func TestAggregateProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	p := Policy{MinSources: 2, MaxSpreadBps: 200, MaxAge: 300 * time.Second}
	var quotes, tooFew, disagree int
	for draw := 0; draw < 10000; draw++ {
		n := 1 + rng.Intn(8)
		base := 1 + rng.Int63n(1_000_000_000_000)
		rs := make([]Rate, n)
		for i := range rs {
			u := base + rng.Int63n(base/40+2) // mostly within 2.5 %: both sides of the bound
			if rng.Intn(10) == 0 {
				u = 1 + rng.Int63n(4*base) // an outlier
			}
			age := time.Duration(rng.Int63n(int64(400 * time.Second)))
			at := now2030.Add(-age)
			switch rng.Intn(8) {
			case 0:
				at = now2030.Add(-p.MaxAge) // the old bound
			case 1:
				at = now2030.Add(-p.MaxAge - time.Nanosecond)
			case 2:
				at = now2030.Add(FutureSkew + time.Duration(rng.Intn(2)))
			}
			rs[i] = Rate{Provider: "p" + strconv.Itoa(i), Currency: "USD", Value: unitsDec(u), At: at}
		}
		q, err := Aggregate("BTC", "USD", rs, now2030, p)
		fresh := Fresh(rs, now2030, p.MaxAge)
		switch {
		case err == nil:
			quotes++
			lo, hi := fresh[0].Value, fresh[0].Value
			for _, r := range fresh {
				if r.Value.Cmp(lo) < 0 {
					lo = r.Value
				}
				if r.Value.Cmp(hi) > 0 {
					hi = r.Value
				}
			}
			if q.Rate.Cmp(lo) < 0 || q.Rate.Cmp(hi) > 0 {
				t.Fatalf("draw %d: median %s outside [%s, %s]", draw, q.Rate, lo, hi)
			}
			if len(q.Sources) != len(fresh) || !sort.SliceIsSorted(q.Sources, func(i, j int) bool { return q.Sources[i].Provider < q.Sources[j].Provider }) {
				t.Fatalf("draw %d: Sources %v, fresh %d", draw, names(q.Sources), len(fresh))
			}
		case errors.Is(err, ErrTooFew):
			tooFew++
			if len(fresh) >= p.MinSources {
				t.Fatalf("draw %d: ErrTooFew with %d fresh", draw, len(fresh))
			}
		case errors.Is(err, ErrDisagree):
			disagree++
			if !SpreadExceeds(fresh, p.MaxSpreadBps) {
				t.Fatalf("draw %d: ErrDisagree within the bound", draw)
			}
		default:
			t.Fatalf("draw %d: %v", draw, err)
		}

		// Order independence.
		shuffled := append([]Rate(nil), rs...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		q2, err2 := Aggregate("BTC", "USD", shuffled, now2030, p)
		if !sameResult(q, err, q2, err2) {
			t.Fatalf("draw %d: the result depends on the input order", draw)
		}
		// Removing a stale rate changes nothing.
		for i, r := range rs {
			if len(Fresh([]Rate{r}, now2030, p.MaxAge)) == 1 {
				continue
			}
			without := append(append([]Rate(nil), rs[:i]...), rs[i+1:]...)
			q3, err3 := Aggregate("BTC", "USD", without, now2030, p)
			if !sameResult(q, err, q3, err3) {
				t.Fatalf("draw %d: removing stale %s changed the result", draw, r.Provider)
			}
			break
		}
	}
	if quotes < 1000 || tooFew < 1000 || disagree < 1000 {
		t.Fatalf("the draws miss a case: %d quotes, %d too few, %d disagree", quotes, tooFew, disagree)
	}
}

func sameResult(q1 Quote, e1 error, q2 Quote, e2 error) bool {
	if e1 != nil || e2 != nil {
		return e1 == e2
	}
	return q1.Asset == q2.Asset && q1.Currency == q2.Currency && q1.At.Equal(q2.At) && q1.Rate.Cmp(q2.Rate) == 0 && sameRates(q1.Sources, q2.Sources)
}

// TestAggregateOneRatePerProvider: two fresh rates from one provider never count twice toward
// MinSources; Aggregate refuses them.
func TestAggregateOneRatePerProvider(t *testing.T) {
	p := Policy{MinSources: 2, MaxSpreadBps: 200, MaxAge: 300 * time.Second}
	rs := []Rate{rate(t, "kraken", "76000", now2030), rate(t, "kraken", "76001", now2030.Add(-time.Second))}
	if _, err := Aggregate("BTC", "USD", rs, now2030, p); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("two rates from one provider: %v, want ErrDuplicate", err)
	}
	// Beside other providers too, and whatever the order.
	rs = append(rs, rate(t, "coinbase", "76000", now2030))
	if _, err := Aggregate("BTC", "USD", []Rate{rs[2], rs[0], rs[1]}, now2030, p); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("a duplicate beside another provider: %v, want ErrDuplicate", err)
	}
	// A stale duplicate is not counted, so it changes nothing.
	rs[1].At = now2030.Add(-301 * time.Second)
	if q, err := Aggregate("BTC", "USD", rs, now2030, p); err != nil || len(q.Sources) != 2 {
		t.Fatalf("a stale duplicate: %v, %v", names(q.Sources), err)
	}
	if strings.Contains(ErrDuplicate.Error(), "kraken") {
		t.Fatal("ErrDuplicate names a provider")
	}
}
