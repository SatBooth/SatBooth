package webhook

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
)

// The next due time of a pass (what Run's timer waits for) and the wake-ups of the store. Tests that run Run use real
// time and wait on a condition against a deadline of seconds, never on a short fixed sleep.

// passNext is one pass of d, returning its next due time.
func passNext(t *testing.T, d *Deliverer) time.Time {
	t.Helper()
	next, err := d.pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return next
}

// The next due time follows the retry steps: the same offsets as TestBackoffTimings.
func TestPassNextFollowsBackoff(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 503)
	d := f.deliverer(s.srv.URL, nil)
	f.event(store.EventPaid)
	for i, off := range []time.Duration{10 * time.Second, 30 * time.Second, 70 * time.Second, 150 * time.Second, 310 * time.Second} {
		next := passNext(t, d)
		if !next.Equal(t0.Add(off)) {
			t.Fatalf("pass %d: next %v after t0, want %v", i+1, next.Sub(t0), off)
		}
		// A pass before the due time attempts nothing and reports the same time.
		f.clk.advance(time.Second)
		if again := passNext(t, d); !again.Equal(next) {
			t.Fatalf("pass %d: waiting item gave next %v, want %v", i+1, again.Sub(t0), next.Sub(t0))
		}
		f.clk.set(next)
	}
}

// When the next retry would fall past the 24 hours, the next due time is the deadline itself: the last attempt.
func TestPassNextIsTheDeadline(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 503)
	d := f.deliverer(s.srv.URL, nil)
	e := f.event(store.EventPaid)
	passNext(t, d) // first attempt at t0
	f.clk.set(e.CreatedAt.Add(RetryWindow - 5*time.Second))
	next := passNext(t, d) // 2nd attempt; its retry (20 s) would pass the deadline
	if want := e.CreatedAt.Add(RetryWindow); !next.Equal(want) {
		t.Fatalf("next %v, want the deadline %v", next, want)
	}
	// The same for a skipped item whose due time is capped by the deadline.
	f.clk.advance(time.Second)
	if again := passNext(t, d); !again.Equal(e.CreatedAt.Add(RetryWindow)) {
		t.Fatalf("skipped item: next %v", again)
	}
}

// Nothing waits for a delivered item, an abandoned one or an empty store.
func TestPassNextZeroWhenNothingWaits(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200, 503)
	d := f.deliverer(s.srv.URL, nil)
	if next := passNext(t, d); !next.IsZero() {
		t.Fatalf("empty store: next %v", next)
	}
	f.event(store.EventPaid)
	if next := passNext(t, d); !next.IsZero() {
		t.Fatalf("delivered: next %v", next)
	}
	// An abandoned item: the last attempt at the deadline fails, and nothing waits afterwards.
	f2 := newFixture(t)
	s2 := newSink(t, f2.clk, 503)
	d2 := f2.deliverer(s2.srv.URL, nil)
	e := f2.event(store.EventPaid)
	passNext(t, d2)
	f2.clk.set(e.CreatedAt.Add(RetryWindow))
	if next := passNext(t, d2); !next.IsZero() {
		t.Fatalf("the last attempt: next %v", next)
	}
	if f2.log.count(GiveUpMessage) != 1 {
		t.Fatalf("no give-up line:\n%s", f2.log.String())
	}
	f2.clk.advance(time.Hour)
	if next := passNext(t, d2); !next.IsZero() {
		t.Fatalf("abandoned: next %v", next)
	}
}

// A receiver that gives no response ends the pass, and the next due time is now: the items after it have not been
// tried.
func TestPassNextIsNowWhenCutShort(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, -1)
	d := f.deliverer(s.srv.URL, &http.Client{Timeout: 100 * time.Millisecond})
	for range 3 {
		f.event(store.EventPaid)
	}
	next := passNext(t, d)
	if next.IsZero() || next.After(f.clk.now()) {
		t.Fatalf("next %v, want at most now %v", next, f.clk.now())
	}
	if n := len(s.requests()); n != 1 {
		t.Fatalf("%d requests, want the pass cut short after 1", n)
	}
}

// heldBackEvent stores an event whose order's quote does not read back.
func (f *fixture) heldBackEvent() store.Event {
	f.t.Helper()
	bad := f.event(store.EventPaid)
	raw, err := sql.Open("sqlite3", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		f.t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO order_quotes VALUES (?, '12.50', 'EUR', 150, 1799999999)`, bad.OrderID); err != nil {
		f.t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO order_quote_rates VALUES (?, 'BTC', '70000.5', '["kraken"]')`, bad.OrderID); err != nil {
		f.t.Fatal(err)
	}
	return bad
}

// A held-back item never sets the next due time, or Run would spin on it: with a one-hour fallback and a 1 ms gap,
// 200 ms of Run pass over it once (and log the debug line at most once).
func TestHeldBackDoesNotDriveTheTimer(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	f.heldBackEvent()
	ds, err := NewEventDeliverers([]Endpoint{{Name: "main", URL: s.srv.URL, Secret: secretA}}, f.st, f.clk.now, nil,
		slog.New(slog.NewTextHandler(f.log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	d := ds[0]
	if next := passNext(t, d); !next.IsZero() {
		t.Fatalf("held back: next %v", next)
	}
	WithIntervals(time.Hour, time.Millisecond).apply(d)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	// Run's first pass logs the debug line once; a spin would log it about 200 times.
	if n := f.log.count(heldBackAgain); n > 1 {
		t.Fatalf("%d later passes over a held-back event in 200 ms: Run spins", n)
	}
}

// source wraps a Source and counts its listings.
type countingSource struct {
	Source
	mu     sync.Mutex
	starts []time.Time
	listed chan struct{} // receives after the first listing
	once   sync.Once
}

func (c *countingSource) Undelivered(ctx context.Context, since time.Time) ([]Item, error) {
	c.mu.Lock()
	c.starts = append(c.starts, time.Now())
	c.mu.Unlock()
	items, err := c.Source.Undelivered(ctx, since)
	c.once.Do(func() { close(c.listed) })
	return items, err
}

func (c *countingSource) times() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.starts...)
}

// runWith starts d.Run and returns a function that stops it and waits.
func runWith(d *Deliverer) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	return func() { cancel(); <-done }
}

func waitUntil(t *testing.T, what string, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// With a one-hour fallback, only the store's wake-up can deliver an event added after the first pass.
func TestWakeDeliversWithoutFallback(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	d := f.deliverer(s.srv.URL, nil)
	cs := &countingSource{Source: d.src, listed: make(chan struct{})}
	d.src = cs
	WithIntervals(time.Hour, time.Millisecond).apply(d)
	stop := runWith(d)
	defer stop()
	select {
	case <-cs.listed:
	case <-time.After(10 * time.Second):
		t.Fatal("no first pass")
	}
	e := f.event(store.EventPaid)
	waitUntil(t, "the delivery after the wake-up", 5*time.Second, func() bool { return len(f.attempts(e)) == 1 })
	if a := f.attempts(e); !a[0].OK {
		t.Fatalf("attempts %+v", a)
	}
}

// With no watch the timers alone drive Run: an event added while it runs is delivered, and an item past its deadline
// gets its last attempt and its line.
func TestFallbackDeliversWithoutWake(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	d := f.deliverer(s.srv.URL, nil)
	d.watch = nil
	cs := &countingSource{Source: d.src, listed: make(chan struct{})}
	d.src = cs
	WithIntervals(20*time.Millisecond, time.Millisecond).apply(d)
	stop := runWith(d)
	select { // the event is added after a pass has listed: only the timer can find it
	case <-cs.listed:
	case <-time.After(10 * time.Second):
		t.Fatal("no first pass")
	}
	e := f.event(store.EventPaid)
	waitUntil(t, "the delivery by the fallback", 10*time.Second, func() bool { return len(f.attempts(e)) == 1 })
	stop()

	g := newFixture(t)
	s2 := newSink(t, g.clk, 503)
	d2 := g.deliverer(s2.srv.URL, nil)
	d2.watch = nil
	e2 := g.event(store.EventPaid)
	WithIntervals(20*time.Millisecond, time.Millisecond).apply(d2)
	stop2 := runWith(d2)
	defer stop2()
	waitUntil(t, "the first attempt", 10*time.Second, func() bool { return len(g.attempts(e2)) == 1 })
	g.clk.set(e2.CreatedAt.Add(RetryWindow + time.Second)) // moved while Run waits: noticed at its next timer
	waitUntil(t, "the last attempt and its line", 5*time.Second, func() bool { return g.log.count(GiveUpMessage) == 1 })
	if n := len(g.attempts(e2)); n != 2 {
		t.Fatalf("%d attempts, want 2", n)
	}
}

// liveClock runs at real time from an offset: the deadline of an item is a moment ahead on it.
type liveClock struct {
	mu     sync.Mutex
	offset time.Duration
	start  time.Time
}

func (c *liveClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return t0.Add(c.offset + time.Since(c.start))
}

// With a one-hour fallback and no write at all, the next due time alone brings the last attempt at the deadline. The
// endpoint is registered and listed once before the window starts counting (those are writes, and a write can stall on
// a loaded machine); then the clock is set so that the deadline is deadlineAhead away and Run starts. Its first
// listing reads only, and must come before the deadline: the item is then skipped and its timer armed, and the last
// attempt can only come from a later pass. A first listing that came after the deadline is not a defect of the timer,
// and the message says so.
func TestDeadlineAttemptWithoutAnyWrite(t *testing.T) {
	const deadlineAhead = 6 * time.Second
	ctx := context.Background()
	lc := &liveClock{start: time.Now()} // offset 0: the clock reads t0, a day before the deadline
	st, err := store.Open(ctx, t.TempDir()+"/store.db", store.WithClock(lc.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	o, _, err := st.CreateOrder(ctx, store.Order{AccountID: "acct-1", MerchantRef: "deadline-timer", AmountMsat: 1_000_000, ExpiresAt: t0.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.AddEvent(ctx, store.Event{Type: store.EventPaid, OrderID: o.ID}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	id := store.EventID(store.EventPaid, o.ID, 1)
	deadline := t0.Add(RetryWindow) // the event is stored at t0 (whole seconds)
	// One failed attempt 5 s before the deadline: the next is due at min(LastAt + 10 s, deadline) = the deadline.
	if err := st.AddAttempt(ctx, store.Attempt{Endpoint: store.MainEndpoint, EventID: id, At: deadline.Add(-5 * time.Second), Status: 503}); err != nil {
		t.Fatal(err)
	}
	s := newSink(t, &clock{t: t0}, 503)
	logs := &logBuf{}
	ds, err := NewEventDeliverers([]Endpoint{{Name: store.MainEndpoint, URL: s.srv.URL, Secret: secretA}}, st, lc.now, nil,
		slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	d := ds[0]
	// The registration and a first listing, a day before the deadline: nothing is due, nothing is sent.
	if next := passNext(t, d); !next.Equal(deadline) {
		t.Fatalf("the setup pass: next %v, want the deadline %v", next, deadline)
	}
	cs := &countingSource{Source: d.src, listed: make(chan struct{})}
	d.src = cs
	WithIntervals(time.Hour, 10*time.Millisecond).apply(d)
	lc.mu.Lock()
	lc.offset = RetryWindow - deadlineAhead - time.Since(lc.start) // the clock is now deadlineAhead before the deadline
	offset := lc.offset
	lc.mu.Unlock()
	stop := runWith(d)
	defer stop()
	limit := time.Now().Add(40 * time.Second)
	for logs.count(GiveUpMessage) != 1 {
		if time.Now().After(limit) {
			n := len(cs.times())
			if n == 1 {
				t.Fatal("one listing and no give-up line in 40 s: no pass came at the deadline, so the next due time " +
					"did not arm the timer")
			}
			t.Fatalf("%d listings and no give-up line in 40 s: the machine stalled, or the last attempt was lost", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	starts := cs.times()
	if first := t0.Add(offset + starts[0].Sub(lc.start)); !first.Before(deadline) {
		t.Fatalf("the first listing came %v after the deadline: the machine stalled for more than %v, "+
			"which this test cannot tell from a missing timer", first.Sub(deadline), deadlineAhead)
	}
	if len(starts) < 2 {
		t.Fatalf("%d listings: the last attempt was not made by a later pass", len(starts))
	}
	a, err := st.AttemptsFor(ctx, store.MainEndpoint, id)
	if err != nil || len(a) != 2 || a[1].At.Before(deadline) {
		t.Fatalf("attempts %+v, %v: want the manual one and the last, at or after the deadline", a, err)
	}
}

// addingSource runs add once, right after its first listing has read its rows, inside the pass.
type addingSource struct {
	Source
	once sync.Once
	add  func()
}

func (a *addingSource) Undelivered(ctx context.Context, since time.Time) ([]Item, error) {
	items, err := a.Source.Undelivered(ctx, since)
	a.once.Do(a.add)
	return items, err
}

// An event stored while a pass is going, after its listing, is delivered at once: the wake-up it raised is taken before
// the listing, so it is still there when the pass ends. A wake-up taken after the listing would be lost here, and the
// event would wait for the fallback.
func TestWakeAfterTheListingIsKept(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	d := f.deliverer(s.srv.URL, nil)
	ctx := context.Background()
	type stored struct {
		id  string
		err error
	}
	added := make(chan stored, 1) // reported on a channel: add runs on Run's goroutine, which must not call t.Fatal
	d.src = &addingSource{Source: d.src, add: func() {
		o, _, err := f.st.CreateOrder(ctx, store.Order{AccountID: "acct-1", MerchantRef: "after-the-listing", AmountMsat: 1_000_000, ExpiresAt: t0.Add(time.Hour)})
		if err == nil {
			_, err = f.st.AddEvent(ctx, store.Event{Type: store.EventPaid, OrderID: o.ID})
		}
		added <- stored{store.EventID(store.EventPaid, o.ID, 1), err}
	}}
	WithIntervals(time.Hour, time.Millisecond).apply(d)
	stop := runWith(d)
	defer stop()
	var e stored
	select {
	case e = <-added:
		if e.err != nil {
			t.Fatal(e.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no first pass")
	}
	waitUntil(t, "the event stored after the first listing", 5*time.Second, func() bool {
		a, err := f.st.AttemptsFor(ctx, store.MainEndpoint, e.id)
		return err == nil && len(a) == 1
	})
}

// A wake-up on every millisecond still gives passes at least a gap apart: n passes started inside a window of length w
// need (n-1) gaps, so n <= w/gap + 1. The bound holds whatever the machine does (a stall lengthens w), so it has no
// absolute time to miss; the window is the time from before Run started to after it returned, a superset of the time
// between the first and the last pass start. The wake-ups go on until the window is at least 400 ms and three passes
// have run, so a stall cannot leave too little to measure.
func TestPassesAreAGapApart(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	d := f.deliverer(s.srv.URL, nil)
	cs := &countingSource{Source: d.src, listed: make(chan struct{})}
	d.src = cs
	wakes := make(chan struct{}, 1)
	d.watch = func() (<-chan struct{}, func()) { return wakes, func() {} }
	const gap = 50 * time.Millisecond
	WithIntervals(time.Hour, gap).apply(d)
	begin := time.Now()
	stop := runWith(d)
	for time.Since(begin) < 30*time.Second && (time.Since(begin) < 400*time.Millisecond || len(cs.times()) < 3) {
		select {
		case wakes <- struct{}{}:
		default:
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	elapsed := time.Since(begin)
	n := len(cs.times())
	if n < 3 {
		t.Fatalf("%d passes in %v: Run did not wake", n, elapsed)
	}
	if max := int(elapsed/gap) + 1; n > max {
		t.Fatalf("%d passes in %v, at most %d with a %v gap", n, elapsed, max, gap)
	}
}

// A hung endpoint holds its own deliverer only: the wake-up of another endpoint still delivers at once.
func TestHungEndpointDoesNotDelayAWakeOfAnother(t *testing.T) {
	f := newFixture(t)
	sa, sb := newSink(t, f.clk, -1), newSink(t, f.clk, 200)
	da, db := f.pair(sa.srv.URL, sb.srv.URL, nil)
	for _, d := range []*Deliverer{da, db} {
		WithIntervals(time.Hour, 10*time.Millisecond).apply(d)
	}
	e1 := f.event(store.EventPaid)
	stopA := runWith(da)
	defer stopA()
	waitUntil(t, "a's request to be held", 10*time.Second, func() bool { return len(sa.requests()) == 1 })
	stopB := runWith(db)
	defer stopB()
	waitUntil(t, "b's first delivery", 10*time.Second, func() bool { return len(f.attemptsOf("b", e1)) == 1 })
	e2 := f.event(store.EventExpired) // a is still held: its wake-up waits in its own channel
	waitUntil(t, "b's delivery after the wake-up", 5*time.Second, func() bool { return len(f.attemptsOf("b", e2)) == 1 })
	if n := len(sa.requests()); n != 1 {
		t.Fatalf("the hung endpoint got %d requests", n)
	}
}

// Both constructors subscribe to the store: an event wakes the event deliverers and an alert the alert deliverer.
func TestConstructorsWatchTheStore(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	ds, err := NewEventDeliverers([]Endpoint{{Name: "a", URL: s.srv.URL, Secret: secretA}, {Name: "b", URL: s.srv.URL, Secret: secretB}},
		f.st, f.clk.now, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ad := f.alertDeliverer(s.srv.URL)
	woken := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	var evs []<-chan struct{}
	for _, d := range ds {
		if d.watch == nil {
			t.Fatal("an event deliverer does not watch the store")
		}
		ch, stop := d.watch()
		defer stop()
		evs = append(evs, ch)
	}
	if ad.watch == nil {
		t.Fatal("the alert deliverer does not watch the store")
	}
	ach, stopA := ad.watch()
	defer stopA()
	f.event(store.EventPaid)
	for i, ch := range evs {
		if !woken(ch) {
			t.Fatalf("event deliverer %d not woken by an event", i)
		}
	}
	if woken(ach) {
		t.Fatal("an event woke the alert deliverer")
	}
	f.alert(store.Alert{Message: "ALERT: an order was paid on both rails", Fields: `{"k":1}`})
	if !woken(ach) {
		t.Fatal("the alert deliverer was not woken by an alert")
	}
	for i, ch := range evs {
		if woken(ch) {
			t.Fatalf("event deliverer %d woken by an alert", i)
		}
	}
}

// The zero Option, which another package can write, changes nothing and does not panic.
func TestZeroOptionChangesNothing(t *testing.T) {
	f := newFixture(t)
	ds, err := NewEventDeliverers([]Endpoint{{Name: "a", URL: "https://example.com/hook", Secret: secretA}}, f.st, f.clk.now, nil, nil, Option{})
	if err != nil {
		t.Fatal(err)
	}
	if ds[0].interval != fallbackInterval || ds[0].gap != pollInterval {
		t.Fatalf("intervals %v %v", ds[0].interval, ds[0].gap)
	}
}
