package relay

import (
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/satbooth/satbooth/nostr"
)

// AddResult is the outcome of Window.Add.
type AddResult int

const (
	// AddAdded: the event is stored; the server broadcasts it.
	AddAdded AddResult = iota
	// AddDuplicate: the id is already held; the server does not broadcast it.
	AddDuplicate
	// AddStale: a 13194 that is older than the one held, or has an equal created_at and a higher id;
	// the server does not broadcast it.
	AddStale
)

type winEntry struct {
	ev      *nostr.Event
	size    int
	arrived time.Time
	seq     uint64
}

// Window holds the relay's events in memory. Ephemeral events (everything except 13194) stay for the
// lifetime, counted from arrival, and are capped in count and bytes; at a cap the oldest is evicted. The
// latest 13194 per wallet (the event's pubkey) is kept outside the lifetime and the caps, and its size does
// not count toward maxBytes. An event with a NIP-40 expiration that has passed is never served: this is
// the relay's rule for expired requests (23194). It is safe for concurrent use.
type Window struct {
	mu       sync.Mutex
	lifetime time.Duration
	maxEvt   int
	maxBytes int64
	now      func() time.Time

	events []winEntry // ephemeral, in arrival order
	bytes  int64
	info   map[string]winEntry // 13194 per pubkey
	ids    map[string]struct{} // ids of everything held
	seq    uint64
}

// NewWindow returns an empty window. now is the injected clock.
func NewWindow(lifetime time.Duration, maxEvents int, maxBytes int64, now func() time.Time) *Window {
	return &Window{
		lifetime: lifetime, maxEvt: maxEvents, maxBytes: maxBytes, now: now,
		info: map[string]winEntry{}, ids: map[string]struct{}{},
	}
}

// expired reports whether e carries an expiration at or before now. An unparsable value counts as
// expired (fail closed: ValidateNWC has checked the grammar before an event reaches the window).
func expired(e *nostr.Event, now time.Time) bool {
	for _, t := range e.Tags {
		if len(t) < 2 || t[0] != "expiration" {
			continue
		}
		x, err := strconv.ParseInt(t[1], 10, 64)
		if err != nil || x <= 0 || now.Unix() >= x {
			return true
		}
	}
	return false
}

// Live reports whether e may still be broadcast: it has no expiration tag, or it has not passed.
func (w *Window) Live(e *nostr.Event) bool {
	return !expired(e, w.now())
}

// prune drops expired and over-age events, then evicts the oldest until within the caps. Caller holds mu.
func (w *Window) prune(now time.Time) {
	keep := w.events[:0]
	for _, en := range w.events {
		if now.Sub(en.arrived) >= w.lifetime || expired(en.ev, now) {
			w.bytes -= int64(en.size)
			delete(w.ids, en.ev.ID)
			continue
		}
		keep = append(keep, en)
	}
	for i := len(keep); i < len(w.events); i++ {
		w.events[i] = winEntry{}
	}
	w.events = keep
	for len(w.events) > 0 && (len(w.events) > w.maxEvt || w.bytes > w.maxBytes) {
		en := w.events[0]
		w.events[0] = winEntry{}
		w.events = w.events[1:]
		w.bytes -= int64(en.size)
		delete(w.ids, en.ev.ID)
	}
	for k, en := range w.info {
		// Defensive: the grammar allows an expiration only on 23194, so a 13194 never carries one.
		if expired(en.ev, now) {
			delete(w.ids, en.ev.ID)
			delete(w.info, k)
		}
	}
}

// Add stores e (size is its serialized length in bytes).
func (w *Window) Add(e *nostr.Event, size int) AddResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.prune(now)
	if _, ok := w.ids[e.ID]; ok {
		return AddDuplicate
	}
	w.seq++
	en := winEntry{ev: e, size: size, arrived: now, seq: w.seq}
	if e.Kind == nostr.KindNWCInfo {
		if old, ok := w.info[e.PubKey]; ok {
			if e.CreatedAt < old.ev.CreatedAt || (e.CreatedAt == old.ev.CreatedAt && e.ID > old.ev.ID) {
				return AddStale
			}
			delete(w.ids, old.ev.ID)
		}
		w.info[e.PubKey] = en
		w.ids[e.ID] = struct{}{}
		return AddAdded
	}
	w.events = append(w.events, en)
	w.bytes += int64(size)
	w.ids[e.ID] = struct{}{}
	w.prune(now)
	return AddAdded
}

// Query returns the held events that match any filter, in delivery order: ascending (created_at,
// arrival). With a limit n, a filter contributes only the last n of its matches. The union has no
// duplicates.
func (w *Window) Query(f []Filter) []*nostr.Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prune(w.now())
	all := make([]winEntry, 0, len(w.events)+len(w.info))
	all = append(all, w.events...)
	for _, en := range w.info {
		all = append(all, en)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].ev.CreatedAt != all[j].ev.CreatedAt {
			return all[i].ev.CreatedAt < all[j].ev.CreatedAt
		}
		return all[i].seq < all[j].seq
	})
	chosen := map[uint64]bool{}
	for i := range f {
		var m []uint64
		for _, en := range all {
			if f[i].Matches(en.ev) {
				m = append(m, en.seq)
			}
		}
		if f[i].Limit != nil && *f[i].Limit < len(m) {
			m = m[len(m)-*f[i].Limit:]
		}
		for _, s := range m {
			chosen[s] = true
		}
	}
	var out []*nostr.Event
	for _, en := range all {
		if chosen[en.seq] {
			out = append(out, en.ev)
		}
	}
	return out
}
