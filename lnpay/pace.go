package lnpay

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// lookupPace spaces the wallet lookups of the reconciler and of a press's preflight: PollRate a second in total, and
// no slot is saved up while it is idle. One preflight at a time holds the preflight place, and a preflight never takes
// two slots in a row: when the previous slot was a preflight's, the slot after it is left for the reconciler. So the
// reconciler keeps at least every other slot, however long its steps take; a slot it is too late to claim is lost, not
// given to a preflight.
//
// Its mutex is a leaf: nothing is called and no other lock is taken while it is held, and it is never held while the
// settle mutex or the service mutex is taken.
type lookupPace struct {
	mu      sync.Mutex
	every   time.Duration
	next    time.Time // the first slot not yet handed out
	lastP   time.Time // the latest slot a preflight took
	hole    time.Time // the slot left for the reconciler after a preflight's (zero: none)
	busy    bool
	queue   []chan struct{}
	now     func() time.Time                                 // time.Now (test seam)
	sleep   func(ctx context.Context, d time.Duration) error // a timer that ends with ctx (test seam)
	after   func(d time.Duration) <-chan time.Time           // time.After (test seam)
	granted func(preflight bool, at time.Time)               // called with each slot handed out (test seam)
}

// errPaceBusy: a preflight found no lookup slot within its bound.
var errPaceBusy = errors.New("lnpay: no lookup slot in time")

// logPreflightBusy is logged when a press's lookup of an expired invoice gets no slot in time.
const logPreflightBusy = "invoice lookup before a second invoice found no lookup slot in time; pay is refused"

func newLookupPace(rate float64) *lookupPace {
	return &lookupPace{every: time.Duration(float64(time.Second) / rate), now: time.Now, sleep: sleepCtx, after: time.After}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// slotReconciler hands out the reconciler's slot: the slot left for it, if it is not yet past, else the next one.
func (p *lookupPace) slotReconciler() (now, at time.Time) {
	p.mu.Lock()
	now = p.now()
	if h := p.hole; !h.IsZero() && !h.Before(now) {
		p.hole = time.Time{}
		at = h
	} else {
		p.hole = time.Time{}
		at = now
		if p.next.After(at) {
			at = p.next
		}
		p.next = at.Add(p.every)
	}
	p.mu.Unlock()
	p.report(false, at)
	return now, at
}

// slotPreflight hands out a preflight's slot: the next one, or the one after it when the previous went to a preflight
// too. It hands out none (ok false) when that slot lies more than limit after from.
func (p *lookupPace) slotPreflight(from time.Time, limit time.Duration) (now, at time.Time, ok bool) {
	p.mu.Lock()
	now = p.now()
	at = now
	if p.next.After(at) {
		at = p.next
	}
	free := at
	if !p.lastP.IsZero() && at.Equal(p.lastP.Add(p.every)) {
		at = at.Add(p.every)
	}
	if at.Sub(from) > limit {
		p.mu.Unlock()
		return now, at, false
	}
	if at.After(free) {
		p.hole = free
	}
	p.lastP = at
	p.next = at.Add(p.every)
	p.mu.Unlock()
	p.report(true, at)
	return now, at, true
}

func (p *lookupPace) report(preflight bool, at time.Time) {
	if p.granted != nil {
		p.granted(preflight, at)
	}
}

func (p *lookupPace) sleepTo(ctx context.Context, now, at time.Time) error {
	if d := at.Sub(now); d > 0 {
		return p.sleep(ctx, d)
	}
	return ctx.Err()
}

// wait returns when the reconciler's next slot comes, or with ctx's error.
func (p *lookupPace) wait(ctx context.Context) error {
	now, at := p.slotReconciler()
	return p.sleepTo(ctx, now, at)
}

// preflight returns when a press may look up an expired invoice: it waits, first come first served, for the single
// preflight place, then for the next slot. It returns errPaceBusy when that slot is more than bound away, or ctx's
// error.
func (p *lookupPace) preflight(ctx context.Context, bound time.Duration) error {
	p.mu.Lock()
	start := p.now()
	p.mu.Unlock()
	if err := p.acquire(ctx, bound); err != nil {
		return err
	}
	defer p.release()
	now, at, ok := p.slotPreflight(start, bound)
	if !ok {
		return errPaceBusy
	}
	return p.sleepTo(ctx, now, at)
}

func (p *lookupPace) acquire(ctx context.Context, bound time.Duration) error {
	p.mu.Lock()
	if !p.busy {
		p.busy = true
		p.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	p.queue = append(p.queue, ch)
	p.mu.Unlock()
	var err error
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-p.after(bound):
		err = errPaceBusy
	}
	p.mu.Lock()
	i := slices.Index(p.queue, ch)
	if i >= 0 {
		p.queue = slices.Delete(p.queue, i, i+1)
	}
	p.mu.Unlock()
	if i < 0 { // the place was handed over as we gave up: pass it on
		p.release()
	}
	return err
}

// release hands the preflight place to the first in the queue, or frees it.
func (p *lookupPace) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) > 0 {
		close(p.queue[0])
		p.queue = p.queue[1:]
		return
	}
	p.busy = false
}
