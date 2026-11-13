package relay

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/nostr"
)

type winClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *winClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *winClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func winNew(life time.Duration, maxEvt int, maxBytes int64) (*Window, *winClock) {
	c := &winClock{t: time.Unix(1_800_000_000, 0)}
	return NewWindow(life, maxEvt, maxBytes, c.now), c
}

func winID(n int) string { return fmt.Sprintf("%064x", n) }

func winEv(id, kind int, created int64, pub string, tags ...[]string) *nostr.Event {
	return &nostr.Event{ID: winID(id), PubKey: pub, CreatedAt: created, Kind: kind, Tags: tags}
}

func winExp(sec int64) []string { return []string{"expiration", strconv.FormatInt(sec, 10)} }

func winIDs(es []*nostr.Event) []string {
	out := []string{}
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func winWant(ids ...int) []string {
	out := []string{}
	for _, i := range ids {
		out = append(out, winID(i))
	}
	return out
}

func winEq(t *testing.T, got []*nostr.Event, want []string) {
	t.Helper()
	g := winIDs(got)
	if fmt.Sprint(g) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", g, want)
	}
}

var winAll = []Filter{{}}

func TestWindowLifetime(t *testing.T) {
	w, c := winNew(time.Hour, 100, 1<<20)
	// created_at far in the past: age counts from arrival.
	if r := w.Add(winEv(1, 23195, 5, "a"), 10); r != AddAdded {
		t.Fatal(r)
	}
	c.add(time.Hour - 1)
	winEq(t, w.Query(winAll), winWant(1))
	c.add(1)
	winEq(t, w.Query(winAll), winWant())
	// The id is free again once gone.
	if r := w.Add(winEv(1, 23195, 5, "a"), 10); r != AddAdded {
		t.Fatal(r)
	}
}

func TestWindowCaps(t *testing.T) {
	w, _ := winNew(time.Hour, 3, 1<<20)
	for i := 1; i <= 3; i++ {
		w.Add(winEv(i, 23195, int64(i), "a"), 1)
	}
	winEq(t, w.Query(winAll), winWant(1, 2, 3))
	if r := w.Add(winEv(4, 23195, 4, "a"), 1); r != AddAdded {
		t.Fatal(r)
	}
	winEq(t, w.Query(winAll), winWant(2, 3, 4))

	w, _ = winNew(time.Hour, 100, 30)
	for i := 1; i <= 3; i++ {
		w.Add(winEv(i, 23195, int64(i), "a"), 10)
	}
	winEq(t, w.Query(winAll), winWant(1, 2, 3)) // exactly at the cap
	if r := w.Add(winEv(4, 23195, 4, "a"), 1); r != AddAdded {
		t.Fatal(r)
	}
	winEq(t, w.Query(winAll), winWant(2, 3, 4)) // one byte over evicts the oldest
	// An event larger than the whole cap is accepted, then evicted itself.
	if r := w.Add(winEv(5, 23195, 5, "a"), 100); r != AddAdded {
		t.Fatal(r)
	}
	if got := w.Query(winAll); len(got) != 0 || w.bytes != 0 {
		t.Fatalf("oversized event kept: %d events, %d bytes", len(got), w.bytes)
	}
	// 13194 does not count toward the caps.
	w, _ = winNew(time.Hour, 1, 10)
	w.Add(winEv(1, 13194, 1, "a"), 1000)
	w.Add(winEv(2, 23195, 2, "a"), 10)
	winEq(t, w.Query(winAll), winWant(1, 2))
}

func TestWindowExpiration(t *testing.T) {
	w, c := winNew(time.Hour, 100, 1<<20)
	exp := c.now().Unix() + 1
	e := winEv(1, 23194, 1, "a", winExp(exp))
	w.Add(e, 10)
	if !w.Live(e) {
		t.Fatal("not live")
	}
	winEq(t, w.Query(winAll), winWant(1))
	c.add(time.Second - time.Nanosecond) // still the previous second
	winEq(t, w.Query(winAll), winWant(1))
	c.add(time.Nanosecond)
	if w.Live(e) {
		t.Fatal("live at expiration")
	}
	winEq(t, w.Query(winAll), winWant())
	// Unparsable: treated as expired.
	for _, v := range []string{"", "x", "-5", "0", "9999999999999999999999"} {
		bad := winEv(2, 23194, 1, "a", []string{"expiration", v})
		if w.Live(bad) {
			t.Errorf("%q live", v)
		}
	}
	if !w.Live(winEv(3, 23194, 1, "a")) {
		t.Error("no tag must be live")
	}
	// Any event with the tag, a 13194 too.
	w.Add(winEv(4, 13194, 1, "b", winExp(c.now().Unix()+1)), 10)
	winEq(t, w.Query(winAll), winWant(4))
	c.add(time.Second)
	winEq(t, w.Query(winAll), winWant())
}

func TestWindowReplaceable(t *testing.T) {
	w, c := winNew(time.Minute, 100, 1<<20)
	if r := w.Add(winEv(5, 13194, 100, "a"), 1); r != AddAdded {
		t.Fatal(r)
	}
	if r := w.Add(winEv(9, 13194, 99, "a"), 1); r != AddStale {
		t.Fatal("older", r)
	}
	if r := w.Add(winEv(7, 13194, 100, "a"), 1); r != AddStale {
		t.Fatal("equal, higher id", r)
	}
	if r := w.Add(winEv(5, 13194, 100, "a"), 1); r != AddDuplicate {
		t.Fatal("same id", r)
	}
	if r := w.Add(winEv(3, 13194, 100, "a"), 1); r != AddAdded {
		t.Fatal("equal, lower id", r)
	}
	winEq(t, w.Query(winAll), winWant(3))
	// Whichever arrives first, the lowest id wins.
	w2, _ := winNew(time.Minute, 100, 1<<20)
	w2.Add(winEv(3, 13194, 100, "a"), 1)
	w2.Add(winEv(5, 13194, 100, "a"), 1)
	winEq(t, w2.Query(winAll), winWant(3))
	if r := w.Add(winEv(4, 13194, 101, "a"), 1); r != AddAdded {
		t.Fatal("newer", r)
	}
	// Another wallet is independent.
	w.Add(winEv(8, 13194, 1, "b"), 1)
	winEq(t, w.Query(winAll), winWant(8, 4))
	// A replaced id is no longer held.
	if r := w.Add(winEv(3, 13194, 100, "a"), 1); r != AddStale {
		t.Fatal("replaced", r)
	}
	c.add(24 * time.Hour)
	winEq(t, w.Query(winAll), winWant(8, 4))
}

func TestWindowDuplicate(t *testing.T) {
	w, _ := winNew(time.Hour, 100, 1<<20)
	if r := w.Add(winEv(1, 23194, 1, "a"), 1); r != AddAdded {
		t.Fatal(r)
	}
	if r := w.Add(winEv(1, 23194, 1, "a"), 1); r != AddDuplicate {
		t.Fatal(r)
	}
	winEq(t, w.Query(winAll), winWant(1))
}

func TestWindowQueryOrderAndLimit(t *testing.T) {
	w, _ := winNew(time.Hour, 100, 1<<20)
	w.Add(winEv(1, 23195, 10, "a"), 1)
	w.Add(winEv(2, 23195, 10, "a"), 1)
	w.Add(winEv(3, 23195, 5, "a"), 1)
	w.Add(winEv(4, 13194, 10, "a"), 1)
	w.Add(winEv(5, 23195, 10, "a"), 1)
	winEq(t, w.Query(winAll), winWant(3, 1, 2, 4, 5))
	lim := func(n int) []Filter { return []Filter{{Limit: &n}} }
	winEq(t, w.Query(lim(0)), winWant())
	winEq(t, w.Query(lim(2)), winWant(4, 5))
	winEq(t, w.Query(lim(99)), winWant(3, 1, 2, 4, 5))
	// Union of filters, no duplicates, delivery order, limit per filter.
	one := 1
	k := []Filter{{Kinds: []int{13194}}, {Kinds: []int{23195}, Limit: &one}, {Kinds: []int{23195}}}
	winEq(t, w.Query(k), winWant(3, 1, 2, 4, 5))
	k = []Filter{{Kinds: []int{23195}, Limit: &one}, {Kinds: []int{13194}}}
	winEq(t, w.Query(k), winWant(4, 5))
	winEq(t, w.Query(nil), winWant())
	winEq(t, w.Query([]Filter{{Kinds: []int{}}}), winWant())
}

// winModel is a naive reference for the window's rules.
type winModel struct {
	life     time.Duration
	maxEvt   int
	maxBytes int64
	evs      []winEntry
	info     map[string]winEntry
	seq      uint64
}

func (m *winModel) prune(now time.Time) {
	var keep []winEntry
	for _, en := range m.evs {
		if now.Sub(en.arrived) < m.life && !expired(en.ev, now) {
			keep = append(keep, en)
		}
	}
	m.evs = keep
	for {
		var b int64
		for _, en := range m.evs {
			b += int64(en.size)
		}
		if len(m.evs) <= m.maxEvt && b <= m.maxBytes {
			break
		}
		m.evs = m.evs[1:]
	}
	for k, en := range m.info {
		if expired(en.ev, now) {
			delete(m.info, k)
		}
	}
}

func (m *winModel) has(id string) bool {
	for _, en := range m.evs {
		if en.ev.ID == id {
			return true
		}
	}
	for _, en := range m.info {
		if en.ev.ID == id {
			return true
		}
	}
	return false
}

func (m *winModel) add(e *nostr.Event, size int, now time.Time) AddResult {
	m.prune(now)
	if m.has(e.ID) {
		return AddDuplicate
	}
	m.seq++
	en := winEntry{ev: e, size: size, arrived: now, seq: m.seq}
	if e.Kind == 13194 {
		if old, ok := m.info[e.PubKey]; ok {
			if e.CreatedAt < old.ev.CreatedAt || (e.CreatedAt == old.ev.CreatedAt && e.ID > old.ev.ID) {
				return AddStale
			}
		}
		m.info[e.PubKey] = en
		return AddAdded
	}
	m.evs = append(m.evs, en)
	m.prune(now)
	return AddAdded
}

func (m *winModel) query(fs []Filter, now time.Time) []string {
	m.prune(now)
	all := append([]winEntry{}, m.evs...)
	for _, en := range m.info {
		all = append(all, en)
	}
	// insertion sort by (created_at, seq)
	for i := 1; i < len(all); i++ {
		for j := i; j > 0; j-- {
			a, b := all[j-1], all[j]
			if a.ev.CreatedAt > b.ev.CreatedAt || (a.ev.CreatedAt == b.ev.CreatedAt && a.seq > b.seq) {
				all[j-1], all[j] = b, a
			}
		}
	}
	out := []string{}
	for _, en := range all {
		for i := range fs {
			if !fs[i].Matches(en.ev) {
				continue
			}
			// rank from the end among this filter's matches
			after := 0
			for _, o := range all {
				if (o.ev.CreatedAt > en.ev.CreatedAt || (o.ev.CreatedAt == en.ev.CreatedAt && o.seq > en.seq)) && fs[i].Matches(o.ev) {
					after++
				}
			}
			if fs[i].Limit == nil || after < *fs[i].Limit {
				out = append(out, en.ev.ID)
				break
			}
		}
	}
	return out
}

func TestWindowProperty(t *testing.T) {
	for seed := int64(0); seed < 40; seed++ {
		r := rand.New(rand.NewSource(seed))
		life := time.Duration(5+r.Intn(30)) * time.Second
		maxEvt, maxBytes := 1+r.Intn(8), int64(10+r.Intn(60))
		w, c := winNew(life, maxEvt, maxBytes)
		m := &winModel{life: life, maxEvt: maxEvt, maxBytes: maxBytes, info: map[string]winEntry{}}
		kinds := []int{23194, 23195, 23197, 13194}
		pubs := []string{"a", "b"}
		for step := 0; step < 300; step++ {
			switch r.Intn(4) {
			case 0:
				c.add(time.Duration(r.Intn(4000)) * time.Millisecond)
			case 1, 2:
				id := r.Intn(40)
				var tags [][]string
				k := kinds[r.Intn(len(kinds))]
				if k == 23194 || r.Intn(8) == 0 {
					tags = append(tags, winExp(c.now().Unix()+int64(r.Intn(12))-2))
				}
				e := winEv(id, k, 100+int64(r.Intn(5)), pubs[r.Intn(2)], tags...)
				size := 1 + r.Intn(20)
				now := c.now()
				if got, want := w.Add(e, size), m.add(e, size, now); got != want {
					t.Fatalf("seed %d step %d: add %v want %v", seed, step, got, want)
				}
			default:
				var fs []Filter
				for i := r.Intn(3); i >= 0; i-- {
					var f Filter
					if r.Intn(2) == 0 {
						f.Kinds = []int{kinds[r.Intn(len(kinds))]}
					}
					if r.Intn(3) == 0 {
						n := r.Intn(4)
						f.Limit = &n
					}
					fs = append(fs, f)
				}
				now := c.now()
				got := w.Query(fs)
				want := m.query(fs, now)
				if fmt.Sprint(winIDs(got)) != fmt.Sprint(want) {
					t.Fatalf("seed %d step %d: got %v want %v", seed, step, winIDs(got), want)
				}
				seen := map[string]bool{}
				var prev int64
				for _, e := range got {
					if seen[e.ID] {
						t.Fatalf("duplicate %s", e.ID)
					}
					seen[e.ID] = true
					if e.CreatedAt < prev {
						t.Fatal("order")
					}
					prev = e.CreatedAt
					if expired(e, now) {
						t.Fatal("expired event returned")
					}
				}
				var eph int
				var b int64
				for _, en := range w.events {
					eph++
					b += int64(en.size)
				}
				if eph > maxEvt || b > maxBytes || b != w.bytes {
					t.Fatalf("caps: %d events, %d bytes (tracked %d)", eph, b, w.bytes)
				}
			}
		}
	}
}

func TestWindowRace(t *testing.T) {
	w, c := winNew(time.Minute, 50, 1000)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				id := g*1000 + i
				k := []int{23194, 23195, 13194}[i%3]
				e := winEv(id, k, int64(i), "a", winExp(c.now().Unix()+5))
				w.Add(e, 10)
				w.Live(e)
				w.Query(winAll)
				if i%50 == 0 {
					c.add(time.Second)
				}
			}
		}(g)
	}
	wg.Wait()
}
