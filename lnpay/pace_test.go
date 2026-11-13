package lnpay

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/store"
)

type tagKey struct{}

func tagged(ctx context.Context, tag string) context.Context {
	return context.WithValue(ctx, tagKey{}, tag)
}

// timeline is a fake clock whose sleepers wake only when the test advances it.
type timeline struct {
	mu       sync.Mutex
	t        time.Time
	sleepers map[chan struct{}]time.Time
	log      []slotUse // every sleep started
}

type slotUse struct {
	tag string
	at  time.Time
}

func newTimeline() *timeline {
	return &timeline{t: epoch, sleepers: map[chan struct{}]time.Time{}}
}

func (tl *timeline) now() time.Time {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return tl.t
}

func (tl *timeline) sleep(ctx context.Context, d time.Duration) error {
	tag, _ := ctx.Value(tagKey{}).(string)
	ch := make(chan struct{})
	tl.mu.Lock()
	at := tl.t.Add(d)
	tl.sleepers[ch] = at
	tl.log = append(tl.log, slotUse{tag, at})
	tl.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		tl.mu.Lock()
		delete(tl.sleepers, ch)
		tl.mu.Unlock()
		return ctx.Err()
	}
}

func (tl *timeline) sleeping() int {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return len(tl.sleepers)
}

func (tl *timeline) logged() []slotUse {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return slices.Clone(tl.log)
}

// advance moves the clock to the earliest sleeper and wakes it.
func (tl *timeline) advance(t *testing.T) {
	t.Helper()
	tl.mu.Lock()
	defer tl.mu.Unlock()
	var first chan struct{}
	for ch, at := range tl.sleepers {
		if first == nil || at.Before(tl.sleepers[first]) {
			first = ch
		}
	}
	if first == nil {
		t.Fatal("nobody is sleeping")
	}
	tl.t = tl.sleepers[first]
	delete(tl.sleepers, first)
	close(first)
}

func (p *lookupPace) queued() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

func timelinePace(rate float64) (*lookupPace, *timeline) {
	tl := newTimeline()
	p := newLookupPace(rate)
	p.now, p.sleep = tl.now, tl.sleep
	p.after = func(time.Duration) <-chan time.Time { return nil }
	return p, tl
}

func TestPaceSpacesSlots(t *testing.T) {
	c := &paceClock{start: epoch, t: epoch}
	p := newLookupPace(5)
	p.now, p.sleep = c.now, c.sleep
	for i := range 3 {
		if err := p.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got, want := c.now().Sub(epoch), time.Duration(i)*200*time.Millisecond; got != want {
			t.Fatalf("slot %d at %v, want %v", i, got, want)
		}
	}
	// An idle pace does not save slots up: after a pause the next slot is now.
	c.sleep(context.Background(), time.Hour)
	at := c.now()
	if err := p.wait(context.Background()); err != nil || !c.now().Equal(at) {
		t.Fatalf("after a pause: %v, moved %v", err, c.now().Sub(at))
	}
}

func TestPreflightTakesEveryOtherSlot(t *testing.T) {
	p, tl := timelinePace(5)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for p.wait(tagged(ctx, "R")) == nil {
		}
	}()
	go func() {
		defer wg.Done()
		for p.preflight(tagged(ctx, "P"), time.Hour) == nil {
		}
	}()
	for len(tl.logged()) < 24 {
		eventually(t, "both asleep", func() bool { return tl.sleeping() == 2 })
		tl.advance(t)
	}
	cancel()
	wg.Wait()
	log := tl.logged()
	slices.SortFunc(log, func(a, b slotUse) int { return a.at.Compare(b.at) })
	log = log[2:22] // the first slots depend on which goroutine started first
	n := map[string]int{}
	for i, u := range log {
		n[u.tag]++
		if i > 0 && log[i-1].tag == u.tag {
			t.Fatalf("slots %d and %d went to %s", i-1, i, u.tag)
		}
	}
	if n["R"] != 10 || n["P"] != 10 {
		t.Fatalf("shares %v", n)
	}
}

func TestPreflightsQueueInOrder(t *testing.T) {
	p, tl := timelinePace(5)
	if err := p.wait(context.Background()); err != nil { // the slot at the start is taken
		t.Fatal(err)
	}
	type done struct {
		who string
		err error
	}
	results := make(chan done, 3)
	run := func(who string) {
		go func() { results <- done{who, p.preflight(tagged(context.Background(), who), 500*time.Millisecond)} }()
	}
	run("P1")
	eventually(t, "P1 asleep", func() bool { return tl.sleeping() == 1 })
	run("P2")
	eventually(t, "P2 queued", func() bool { return p.queued() == 1 })
	run("P3")
	eventually(t, "P3 queued", func() bool { return p.queued() == 2 })
	tl.advance(t) // P1's slot
	if r := <-results; r.who != "P1" || r.err != nil {
		t.Fatalf("first: %+v", r)
	}
	eventually(t, "P2 asleep", func() bool { return tl.sleeping() == 1 })
	tl.advance(t)
	r2, r3 := <-results, <-results
	if r2.who != "P2" || r2.err != nil || r3.who != "P3" || !errors.Is(r3.err, errPaceBusy) {
		t.Fatalf("then: %+v %+v", r2, r3)
	}
	log := tl.logged()
	if len(log) != 2 || log[0].tag != "P1" || log[1].tag != "P2" || !log[0].at.Before(log[1].at) {
		t.Fatalf("slots %+v", log)
	}
	// A refused preflight took no slot: the next one follows the second.
	next := make(chan error, 1)
	go func() { next <- p.wait(context.Background()) }()
	eventually(t, "reconciler asleep", func() bool { return tl.sleeping() == 1 })
	tl.advance(t)
	if err := <-next; err != nil {
		t.Fatal(err)
	}
	if got := tl.now().Sub(epoch); got != 600*time.Millisecond {
		t.Fatalf("clock at %v", got)
	}
}

func TestPreflightEndsWithItsContext(t *testing.T) {
	p, tl := timelinePace(5)
	if err := p.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- p.preflight(context.Background(), time.Hour) }()
	eventually(t, "first asleep", func() bool { return tl.sleeping() == 1 })
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { second <- p.preflight(ctx, time.Hour) }()
	eventually(t, "second queued", func() bool { return p.queued() == 1 })
	third := make(chan error, 1)
	go func() { third <- p.preflight(context.Background(), time.Hour) }()
	eventually(t, "third queued", func() bool { return p.queued() == 2 })
	cancel()
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled preflight: %v", err)
	}
	if p.queued() != 1 {
		t.Fatalf("%d queued", p.queued())
	}
	tl.advance(t)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	eventually(t, "third asleep", func() bool { return tl.sleeping() == 1 })
	tl.advance(t)
	if err := <-third; err != nil {
		t.Fatalf("the next preflight: %v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy || len(p.queue) != 0 {
		t.Fatal("the place was not freed")
	}
}

func TestRepeatedPressesCostAtMostThePaceRate(t *testing.T) {
	e := newEnv(t, withFakePace, func(e *env) { e.cfg.PollRate = 5 })
	o, inv := e.accountAtCapWithExpiredInvoice()
	before := e.w.makeCalls.Load()
	for range 50 {
		if _, err := e.pay(o); !errors.Is(err, ErrUnknown) && !errors.Is(err, limits.ErrAccountCap) {
			t.Fatal(err)
		}
	}
	if n := e.w.lookupCount(inv.PaymentHash); n != 50 || e.paceElapsed() < 49*200*time.Millisecond {
		t.Fatalf("%d lookups in %v", n, e.paceElapsed())
	}
	if e.w.makeCalls.Load() != before {
		t.Fatal("an invoice was made")
	}
}

func TestPreflightRefusalNeverMakesASecondInvoice(t *testing.T) {
	e := newEnv(t, withFakePace)
	o := e.order("a", 3_000_000)
	inv := e.mustPay(o)
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + 30*time.Second)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	release := e.holdPreflightPlace()
	if _, err := e.pay(o); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	if e.w.makeCalls.Load() != 1 || e.transfers(o) != 0 || e.count(logPreflightBusy) != 1 {
		t.Fatal("refusal leaked")
	}
	release()
	if _, err := e.pay(o); !errors.Is(err, ErrPaid) {
		t.Fatal(err)
	}
	if e.w.makeCalls.Load() != 1 || e.transfers(o) != 1 || e.eventCount(store.EventPaid, o.ID) != 1 {
		t.Fatal("paid")
	}
}

// A first press makes no lookup and takes no slot.
func TestFirstPressTakesNoSlot(t *testing.T) {
	e := newEnv(t, withFakePace)
	e.mustPay(e.order("a", 3_000_000))
	if e.paceElapsed() != 0 || e.svc.pace.next != (time.Time{}) {
		t.Fatal("a first press took a slot")
	}
}

func TestReconcilerKeepsItsRateDuringPresses(t *testing.T) {
	e := newEnv(t, func(e *env) { e.cfg.PollRate = 10 })
	for i := range 20 {
		e.mustPay(e.order(string(rune('A'+i)), 1_000_000))
	}
	o, target := e.accountAtCapWithExpiredInvoice()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = e.svc.Run(ctx) }()
	for ctx.Err() == nil {
		if _, err := e.pay(o); !errors.Is(err, ErrUnknown) && !errors.Is(err, limits.ErrAccountCap) {
			t.Error(err)
			break
		}
	}
	wg.Wait()
	e.w.mu.Lock()
	all := 0
	for _, n := range e.w.lookups {
		all += n
	}
	e.w.mu.Unlock()
	if reconciler := all - e.w.lookupCount(target.PaymentHash); reconciler < 9 || all > 21 {
		t.Fatalf("%d lookups of other invoices, %d in all", reconciler, all)
	}
}
