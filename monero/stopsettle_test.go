package monero

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
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

// A stop landing in AddMoneroSettled's write is no failed settle: the pass returns the context's error and logs no
// logSettleFailed. The reverse, the write failing on a live context, logs it once with the order.
func TestStopDuringSettledWriteLogsNoSettleFailed(t *testing.T) {
	setup := func(t *testing.T) (*env, string) {
		e := newWatchEnv(t, nil)
		o := e.order(xmr)
		e.payOrder(o, xmr)
		e.w.Mine(9)
		e.pass() // marked mined at 9 blocks; the next block settles it
		e.w.Mine(1)
		return e, o.ID
	}
	t.Run("stop", func(t *testing.T) {
		e, _ := setup(t)
		ctx, cancel := context.WithCancel(bg)
		e.clk.setOnNow(func() {
			if calledFrom("AddMoneroSettled") {
				cancel()
			}
		})
		if err := e.svc.pass(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("pass: %v, want the context's error\n%s", err, e.log)
		}
		e.clk.setOnNow(nil)
		if e.count(logSettleFailed) != 0 {
			t.Fatalf("a failure line on a stop:\n%s", e.log)
		}
	})
	t.Run("failure", func(t *testing.T) {
		e, id := setup(t)
		e.execDB(`CREATE TRIGGER settled_fail BEFORE INSERT ON monero_settled BEGIN SELECT RAISE(ABORT, 'x'); END`)
		if err := e.pass(); err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("pass: %v, want the store's error", err)
		}
		if e.count(logSettleFailed) != 1 || !e.logHas(`msg="`+logSettleFailed+`" order=`+id) {
			t.Fatalf("want one %q line with the order:\n%s", logSettleFailed, e.log)
		}
	})
}
