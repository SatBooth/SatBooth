package relay

import (
	"math/rand"
	"testing"

	"github.com/satbooth/satbooth/nostr"
)

func TestFilterMatches(t *testing.T) {
	a, b := hex64('a'), hex64('b')
	e := &nostr.Event{
		Kind: 23194, PubKey: a, CreatedAt: 100,
		Tags: [][]string{{"p", b}, {"e", a, "extra"}, {"x"}, {"p"}},
	}
	n := func(v int) *int { return &v }
	cases := []struct {
		name string
		f    Filter
		want bool
	}{
		{"empty filter", Filter{}, true},
		{"limit only", Filter{Limit: n(1)}, true},
		{"kind hit", Filter{Kinds: []int{1, 23194}}, true},
		{"kind miss", Filter{Kinds: []int{1}}, false},
		{"kinds empty list", Filter{Kinds: []int{}}, false},
		{"author hit", Filter{Authors: []string{b, a}}, true},
		{"author miss", Filter{Authors: []string{b}}, false},
		{"authors empty list", Filter{Authors: []string{}}, false},
		{"p hit", Filter{P: []string{b}}, true},
		{"p miss (author is not a p tag)", Filter{P: []string{a}}, false},
		{"p empty list", Filter{P: []string{}}, false},
		{"e hit", Filter{E: []string{a}}, true},
		{"e miss", Filter{E: []string{b}}, false},
		{"e empty list", Filter{E: []string{}}, false},
		{"since equal", Filter{Since: i64(100)}, true},
		{"since after", Filter{Since: i64(101)}, false},
		{"since before", Filter{Since: i64(99)}, true},
		{"until equal", Filter{Until: i64(100)}, true},
		{"until before", Filter{Until: i64(99)}, false},
		{"until after", Filter{Until: i64(101)}, true},
		{"since=until=created_at", Filter{Since: i64(100), Until: i64(100)}, true},
		{"all fields hit", Filter{Kinds: []int{23194}, Authors: []string{a}, P: []string{b}, E: []string{a}, Since: i64(1), Until: i64(200)}, true},
		{"AND: one field misses", Filter{Kinds: []int{23194}, Authors: []string{a}, P: []string{a}}, false},
		{"AND: kind misses", Filter{Kinds: []int{1}, Authors: []string{a}, P: []string{b}}, false},
	}
	for _, c := range cases {
		if got := c.f.Matches(e); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func hex64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

// refMatches is a naive reference for Filter.Matches.
func refMatches(f *Filter, e *nostr.Event) bool {
	if f.Kinds != nil {
		ok := false
		for i := range f.Kinds {
			ok = ok || f.Kinds[i] == e.Kind
		}
		if !ok {
			return false
		}
	}
	if f.Authors != nil {
		ok := false
		for i := range f.Authors {
			ok = ok || f.Authors[i] == e.PubKey
		}
		if !ok {
			return false
		}
	}
	for _, spec := range []struct {
		name string
		list []string
	}{{"p", f.P}, {"e", f.E}} {
		if spec.list == nil {
			continue
		}
		ok := false
		for _, tag := range e.Tags {
			for _, v := range spec.list {
				if len(tag) > 1 && tag[0] == spec.name && tag[1] == v {
					ok = true
				}
			}
		}
		if !ok {
			return false
		}
	}
	if f.Since != nil && !(e.CreatedAt >= *f.Since) {
		return false
	}
	if f.Until != nil && !(e.CreatedAt <= *f.Until) {
		return false
	}
	return true
}

func TestFilterMatchesProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	pool := []string{hex64('a'), hex64('b'), hex64('c')}
	pickStrs := func() []string {
		switch rng.Intn(4) {
		case 0:
			return nil
		case 1:
			return []string{}
		}
		var l []string
		for i := rng.Intn(3) + 1; i > 0; i-- {
			l = append(l, pool[rng.Intn(len(pool))])
		}
		return l
	}
	pickTime := func() *int64 {
		if rng.Intn(2) == 0 {
			return nil
		}
		v := int64(rng.Intn(7))
		return &v
	}
	for i := 0; i < 20000; i++ {
		f := Filter{Authors: pickStrs(), P: pickStrs(), E: pickStrs(), Since: pickTime(), Until: pickTime()}
		switch rng.Intn(3) {
		case 0:
		case 1:
			f.Kinds = []int{}
		default:
			for j := rng.Intn(3) + 1; j > 0; j-- {
				f.Kinds = append(f.Kinds, rng.Intn(3))
			}
		}
		e := &nostr.Event{Kind: rng.Intn(3), PubKey: pool[rng.Intn(3)], CreatedAt: int64(rng.Intn(7))}
		for j := rng.Intn(4); j > 0; j-- {
			tag := []string{[]string{"p", "e", "t"}[rng.Intn(3)]}
			for k := rng.Intn(3); k > 0; k-- {
				tag = append(tag, pool[rng.Intn(3)])
			}
			e.Tags = append(e.Tags, tag)
		}
		if got, want := f.Matches(e), refMatches(&f, e); got != want {
			t.Fatalf("iter %d: filter %+v event %+v: got %v want %v", i, f, e, got, want)
		}
	}
}

// TestParseFilterRoundTripProperty: random filters (lists at and around 64 values, extreme numbers)
// survive encode and parse unchanged.
func TestParseFilterRoundTripProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	hexs := func() []string {
		if rng.Intn(3) == 0 {
			return nil
		}
		l := make([]string, []int{0, 1, 63, 64}[rng.Intn(4)])
		for i := range l {
			b := make([]byte, 64)
			for j := range b {
				b[j] = "0123456789abcdef"[rng.Intn(16)]
			}
			l[i] = string(b)
		}
		return l
	}
	nums := []int64{0, 1, 9, 10, 1700000000, 1<<63 - 1}
	for i := 0; i < 2000; i++ {
		f := Filter{Authors: hexs(), P: hexs(), E: hexs()}
		if rng.Intn(2) == 0 {
			f.Kinds = make([]int, rng.Intn(65))
			for j := range f.Kinds {
				f.Kinds[j] = rng.Intn(65536)
			}
		}
		if rng.Intn(2) == 0 {
			v := nums[rng.Intn(len(nums))]
			f.Since = &v
		}
		if rng.Intn(2) == 0 {
			v := nums[rng.Intn(len(nums))]
			f.Until = &v
		}
		if rng.Intn(2) == 0 {
			v := int(nums[rng.Intn(4)])
			f.Limit = &v
		}
		in := encodeMessage(&ClientMessage{Label: "REQ", SubID: "s:1", Filters: []Filter{f}})
		m, err := ParseClientMessage(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := encodeMessage(m); string(got) != string(in) {
			t.Fatalf("round trip: %s -> %s", in, got)
		}
	}
}
