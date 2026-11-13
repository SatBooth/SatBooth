package onchain

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/satbooth/satbooth/store"
)

// calledFrom reports whether a function whose name contains name is on the calling goroutine's stack.
func calledFrom(name string) bool {
	pcs := make([]uintptr, 48)
	n := runtime.Callers(2, pcs)
	fr := runtime.CallersFrames(pcs[:n])
	for {
		f, more := fr.Next()
		if strings.Contains(f.Function, name) {
			return true
		}
		if !more {
			return false
		}
	}
}

// flipCtx is a context whose Err answers context.Canceled from the first call made after the named function has
// returned (a call made under it answers nil), while its Done never fires: a stop that lands right after that
// function's write, with nothing cut short inside it.
type flipCtx struct {
	context.Context
	name string
	in   atomic.Bool
}

func (c *flipCtx) Err() error {
	if calledFrom(c.name) {
		c.in.Store(true)
		return nil
	}
	if c.in.Load() {
		return context.Canceled
	}
	return c.Context.Err()
}

// stopIn is a context that ends from the first call that a function named name makes on it (its Done, as database/sql
// reads it before a query): from then on Done is closed and Err is context.Canceled.
type stopIn struct {
	context.Context
	name string
	hit  atomic.Bool
}

var closedDone = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

func (c *stopIn) Done() <-chan struct{} {
	if c.hit.Load() || calledFrom(c.name) {
		c.hit.Store(true)
		return closedDone
	}
	return c.Context.Done()
}

func (c *stopIn) Err() error {
	if c.hit.Load() {
		return context.Canceled
	}
	return c.Context.Err()
}

// replacedPastWindow is the setup of TestStuckThenReplacedAfterTheWindow up to the pass that kills the output and
// settles it: the next pass marks it dead and AddSettled records it (the left-pool ALERT's case).
func replacedPastWindow(t *testing.T) (*env, store.Order, string) {
	t.Helper()
	e := newWatchEnv(t)
	o, txid := e.stuckAtWindowEnd()
	e.pass()
	e.node.Replace(txid, false)
	return e, o, txid
}

// A stop landing in AddSettled's write is no failed settle, and a stop before the write (here in carriedOn) settles
// nothing and logs no left-pool ALERT; a stop after the write still logs it, once. The reverse, an AddSettled failing on a live
// context, logs logSettleFailed once with the order; a real left-pool case still logs its ALERT once
// (TestStuckThenReplacedAfterTheWindow).
func TestStopDuringSettledWriteLogsNoSettleFailed(t *testing.T) {
	t.Run("in-write", func(t *testing.T) {
		e, o, txid := replacedPastWindow(t)
		ctx, cancel := context.WithCancel(bg)
		e.clk.setOnNow(func() {
			if calledFrom("AddSettled") {
				cancel()
			}
		})
		if err := e.svc.pass(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("pass: %v, want the context's error", err)
		}
		e.clk.setOnNow(nil)
		if e.count(logSettleFailed) != 0 || e.count(alertLeftPool) != 0 {
			t.Fatalf("a failure line on a stop:\n%s", e.log)
		}
		if k, err := e.st.SettledKeys(bg, []store.TXOKey{{TxID: txid}}); err != nil || k[store.TXOKey{TxID: txid}] {
			t.Fatalf("settled %v %v", k, err)
		}
		_ = o
	})
	// A stop inside carriedOn, before the write: nothing is settled and no ALERT is logged; the next pass, on a live
	// context, settles the output and logs the ALERT once.
	t.Run("in-carriedOn", func(t *testing.T) {
		e, _, txid := replacedPastWindow(t)
		ctx := &stopIn{Context: bg, name: "carriedOn"}
		if err := e.svc.pass(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("pass: %v, want the context's error\n%s", err, e.log)
		}
		if !ctx.hit.Load() {
			t.Fatal("the stop did not land in carriedOn")
		}
		if k, err := e.st.SettledKeys(bg, []store.TXOKey{{TxID: txid}}); err != nil || k[store.TXOKey{TxID: txid}] {
			t.Fatalf("settled %v %v", k, err)
		}
		if e.count(logSettleFailed) != 0 || e.count(alertLeftPool) != 0 {
			t.Fatalf("a line on a stop:\n%s", e.log)
		}
		e.passes(2)
		if e.count(alertLeftPool) != 1 || e.count(logSettleFailed) != 0 {
			t.Fatalf("the next passes: %d ALERTs\n%s", e.count(alertLeftPool), e.log)
		}
	})
	// A stop right after the write committed: the output is settled and never read again, so the ALERT is logged
	// now, once, and a restart logs no second one.
	t.Run("after-write", func(t *testing.T) {
		e, _, txid := replacedPastWindow(t)
		ctx := &flipCtx{Context: bg, name: "AddSettled"}
		e.clk.setOnNow(func() {
			if calledFrom("AddSettled") {
				ctx.in.Store(true)
			}
		})
		e.svc.pass(ctx)
		e.clk.setOnNow(nil)
		if k, err := e.st.SettledKeys(bg, []store.TXOKey{{TxID: txid}}); err != nil || !k[store.TXOKey{TxID: txid}] {
			t.Fatalf("the stop did not land after a successful AddSettled: %v %v", k, err)
		}
		if e.count(logSettleFailed) != 0 || e.count(alertLeftPool) != 1 {
			t.Fatalf("%d ALERTs on a stop after the write:\n%s", e.count(alertLeftPool), e.log)
		}
		e.restart()
		e.passes(5)
		if e.count(alertLeftPool) != 1 {
			t.Fatalf("%d ALERTs after the restart:\n%s", e.count(alertLeftPool), e.log)
		}
	})
	t.Run("failure", func(t *testing.T) {
		e, o, _ := replacedPastWindow(t)
		e.execDB(`CREATE TRIGGER settled_fail BEFORE INSERT ON onchain_settled BEGIN SELECT RAISE(ABORT, 'x'); END`)
		if err := e.pass(); err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("pass: %v, want the store's error", err)
		}
		if e.count(logSettleFailed) != 1 || !e.logHas(`msg="`+logSettleFailed+`" order=`+o.ID) {
			t.Fatalf("want one %q line with the order:\n%s", logSettleFailed, e.log)
		}
	})
}
