package lnpay

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/nwc"
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
	if !tl.tryAdvance() {
		t.Fatal("nobody is sleeping")
	}
}

func (tl *timeline) tryAdvance() bool {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	var first chan struct{}
	for ch, at := range tl.sleepers {
		if first == nil || at.Before(tl.sleepers[first]) {
			first = ch
		}
	}
	if first == nil {
		return false
	}
	tl.t = tl.sleepers[first]
	delete(tl.sleepers, first)
	close(first)
	return true
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
		go func() { results <- done{who, p.preflight(tagged(context.Background(), who), 700*time.Millisecond)} }()
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
	if len(log) != 2 || log[0].tag != "P1" || log[1].tag != "P2" || log[0].at.Sub(epoch) != 200*time.Millisecond ||
		log[1].at.Sub(epoch) != 600*time.Millisecond {
		t.Fatalf("slots %+v", log)
	}
	// P1 took 200 ms and P2, which may not follow it at once, 600 ms. A refused preflight took no slot: the next
	// one follows the second.
	next := make(chan error, 1)
	go func() { next <- p.wait(context.Background()) }()
	eventually(t, "reconciler asleep", func() bool { return tl.sleeping() == 1 })
	tl.advance(t)
	if err := <-next; err != nil {
		t.Fatal(err)
	}
	if got := tl.now().Sub(epoch); got != 800*time.Millisecond {
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

type grant struct {
	preflight bool
	at        time.Time
}

type grantLog struct {
	mu sync.Mutex
	g  []grant
}

func (l *grantLog) add(preflight bool, at time.Time) {
	l.mu.Lock()
	l.g = append(l.g, grant{preflight, at})
	l.mu.Unlock()
}

func (l *grantLog) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.g)
}

// sorted is the first n slots handed out, in order of time.
func (l *grantLog) sorted(n int) []grant {
	l.mu.Lock()
	defer l.mu.Unlock()
	g := slices.Clone(l.g[:n])
	slices.SortFunc(g, func(a, b grant) int { return a.at.Compare(b.at) })
	return g
}

// checkShares fails unless the slots are at least every apart, and no two preflight slots are adjacent in the grid of
// slots (a slot between them is left for the reconciler). It returns the number of preflight and of reconciler slots.
func checkShares(t *testing.T, g []grant, every time.Duration) (pre, rec int) {
	t.Helper()
	for i, x := range g {
		if x.preflight {
			pre++
		} else {
			rec++
		}
		if i == 0 {
			continue
		}
		if d := x.at.Sub(g[i-1].at); d < every {
			t.Fatalf("slots %d and %d are %v apart, under %v", i-1, i, d, every)
		}
	}
	var last time.Time
	for _, x := range g {
		if !x.preflight {
			continue
		}
		if !last.IsZero() && x.at.Sub(last) < 2*every {
			t.Fatalf("preflight slots %v apart, under %v", x.at.Sub(last), 2*every)
		}
		last = x.at
	}
	return pre, rec
}

// drive advances tl whenever both goroutines are asleep, until n slots are handed out; then it stops them and runs the
// clock until they end.
func drive(t *testing.T, tl *timeline, l *grantLog, n int, stop func(), done *sync.WaitGroup) {
	t.Helper()
	for l.len() < n {
		eventually(t, "both asleep", func() bool { return tl.sleeping() == 2 })
		tl.advance(t)
	}
	stop()
	ended := make(chan struct{})
	go func() { done.Wait(); close(ended) }()
	for {
		select {
		case <-ended:
			return
		default:
		}
		if !tl.tryAdvance() {
			runtime.Gosched()
		}
	}
}

// However long the reconciler's steps take, presses never take two slots in a row (the slot between is the
// reconciler's, claimed or not), the slots are PollRate apart at most, and a step of 100 ms keeps the reconciler to
// half of the slots handed out.
func TestReconcilerKeepsEveryOtherSlot(t *testing.T) {
	for _, step := range []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, time.Second} {
		t.Run(step.String(), func(t *testing.T) {
			p, tl := timelinePace(5)
			var l grantLog
			p.granted = l.add
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				for p.wait(tagged(ctx, "R")) == nil && tl.sleep(tagged(ctx, "W"), step) == nil {
				}
			}()
			go func() {
				defer wg.Done()
				for p.preflight(tagged(ctx, "P"), time.Hour) == nil {
				}
			}()
			drive(t, tl, &l, 40, cancel, &wg)
			pre, rec := checkShares(t, l.sorted(40), 200*time.Millisecond)
			t.Logf("step %v: %d preflight slots, %d reconciler slots", step, pre, rec)
			if step == 100*time.Millisecond && rec < pre-1 {
				t.Fatalf("the reconciler took %d of 40 slots, the presses %d", rec, pre)
			}
		})
	}
}

// slowWallet makes each lookup take 300 ms of the timeline's time.
type slowWallet struct {
	*fakeWallet
	tl *timeline
}

func (w slowWallet) LookupInvoice(ctx context.Context, h string) (*nwc.Transaction, error) {
	if err := w.tl.sleep(ctx, 300*time.Millisecond); err != nil {
		return nil, err
	}
	return w.fakeWallet.LookupInvoice(ctx, h)
}

// The real reconciler and a loop of presses on an expired invoice share one pace on a simulated clock, while every
// lookup takes 300 ms. The slots are PollRate apart, the presses never take two in a row, and the reconciler keeps
// reading the open invoices.
func TestReconcilerKeepsItsRateDuringPresses(t *testing.T) {
	e := newEnv(t, func(e *env) { e.cfg.PollRate = 10 })
	tl := newTimeline()
	var l grantLog
	pc := e.svc.pace
	pc.now, pc.sleep, pc.after, pc.granted = tl.now, tl.sleep, func(time.Duration) <-chan time.Time { return nil }, l.add
	var others []store.Invoice
	for i := range 20 {
		others = append(others, e.mustPay(e.order(string(rune('A'+i)), 1_000_000)))
	}
	o, target := e.accountAtCapWithExpiredInvoice()
	e.svc.w = slowWallet{e.w, tl}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	var stop atomic.Bool
	wg.Add(2)
	go func() { defer wg.Done(); _ = e.svc.Run(ctx) }()
	go func() {
		defer wg.Done()
		for !stop.Load() {
			if _, err := e.pay(o); !errors.Is(err, ErrUnknown) && !errors.Is(err, limits.ErrAccountCap) {
				t.Error(err)
				return
			}
		}
	}()
	drive(t, tl, &l, 40, func() { stop.Store(true); cancel() }, &wg)
	pre, rec := checkShares(t, l.sorted(40), 100*time.Millisecond)
	read := 0
	for _, inv := range others {
		read += e.w.lookupCount(inv.PaymentHash)
	}
	t.Logf("%d preflight slots, %d reconciler slots, %d reads of open invoices, %d of the expired one", pre, rec, read,
		e.w.lookupCount(target.PaymentHash))
	if read < 5 {
		t.Fatalf("%d reads of the open invoices", read)
	}
}

// A preflight whose context ends during its slot wait gives the place up, even though it held it.
func TestHolderEndingDuringItsSlotFreesThePlace(t *testing.T) {
	p, tl := timelinePace(5)
	if err := p.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- p.preflight(ctx, time.Hour) }()
	eventually(t, "first asleep", func() bool { return tl.sleeping() == 1 })
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first: %v", err)
	}
	second := make(chan error, 1)
	go func() { second <- p.preflight(context.Background(), time.Hour) }()
	eventually(t, "second asleep (it has the place)", func() bool { return tl.sleeping() == 1 })
	tl.advance(t)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy || len(p.queue) != 0 {
		t.Fatal("the place was not freed")
	}
}

// The place is handed to a waiter at the moment the waiter gives up: it is passed on, not lost.
func TestPlaceHandedOverAsAWaiterGivesUpIsPassedOn(t *testing.T) {
	for i := range 200 {
		p := newLookupPace(5)
		fired := make(chan time.Time)
		p.after = func(time.Duration) <-chan time.Time { return fired }
		if err := p.acquire(context.Background(), time.Hour); err != nil { // the holder
			t.Fatal(err)
		}
		res := make(chan error, 1)
		go func() { res <- p.acquire(context.Background(), time.Hour) }()
		eventually(t, "waiter queued", func() bool { return p.queued() == 1 })
		p.mu.Lock()
		close(fired) // the waiter's bound ends ...
		for range 20 {
			runtime.Gosched()
		}
		close(p.queue[0]) // ... as the holder hands the place over (what release does)
		p.queue = p.queue[1:]
		p.mu.Unlock()
		if err := <-res; err == nil { // it took the place after all
			p.release()
		} else if !errors.Is(err, errPaceBusy) {
			t.Fatal(err)
		}
		p.mu.Lock()
		stuck := p.busy || len(p.queue) != 0
		p.mu.Unlock()
		if stuck {
			t.Fatalf("round %d: the place is stuck", i)
		}
	}
}

// A press whose context ends while it waits for its slot answers ErrUnknown (with the context's error) and goes no
// further: no attempt row and no invoice at the wallet.
func TestPreflightEndedByContextMakesNoInvoice(t *testing.T) {
	e := newEnv(t, func(e *env) { e.cfg.PollRate = 5 })
	tl := newTimeline()
	e.svc.pace.now, e.svc.pace.sleep = tl.now, tl.sleep
	o := e.order("a", 3_000_000)
	inv := e.mustPay(o)
	e.clk.Advance(inv.ExpiresAt.Sub(e.clk.Now()) + 30*time.Second)
	if err := e.svc.pace.wait(context.Background()); err != nil { // the slot at the start is taken
		t.Fatal(err)
	}
	made := e.w.makeCalls.Load()
	attempts := func() int {
		n, _, err := e.st.AttemptsSince(context.Background(), epoch.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := attempts()
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { _, err := e.svc.pay(ctx, o); res <- err }()
	eventually(t, "the press waits for its slot", func() bool { return tl.sleeping() == 1 })
	cancel()
	if err := <-res; !errors.Is(err, ErrUnknown) || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if e.w.makeCalls.Load() != made || attempts() != before || e.w.lookupCount(inv.PaymentHash) != 0 {
		t.Fatal("the press went on after its context ended")
	}
}

// The slot left after a preflight's is the reconciler's: it takes that one, not a later one.
func TestReconcilerClaimsTheSlotLeftForIt(t *testing.T) {
	p := newLookupPace(5)
	p.now = func() time.Time { return epoch }
	slots := func(n int) (at []time.Duration) {
		for range n {
			_, s, ok := p.slotPreflight(epoch, time.Hour)
			if !ok {
				t.Fatal("refused")
			}
			at = append(at, s.Sub(epoch))
		}
		return at
	}
	if got := slots(2); got[0] != 0 || got[1] != 400*time.Millisecond {
		t.Fatalf("preflight slots %v", got)
	}
	if _, at := p.slotReconciler(); at.Sub(epoch) != 200*time.Millisecond {
		t.Fatalf("reconciler slot at %v", at.Sub(epoch))
	}
	if got := slots(1); got[0] != 800*time.Millisecond {
		t.Fatalf("next preflight slot %v", got)
	}
	// A slot it was too late for is lost: the next one after the preflights is taken.
	p.now = func() time.Time { return epoch.Add(time.Second) }
	if _, at := p.slotReconciler(); at.Sub(epoch) != time.Second {
		t.Fatalf("late reconciler slot at %v", at.Sub(epoch))
	}
}
