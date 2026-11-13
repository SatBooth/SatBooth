package lnpay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/store"
)

// watchedEnv makes w orders whose invoices are closed by lookup (and so watched), each polled just now.
func watchedEnv(t *testing.T, w int, opts ...func(*env)) (*env, []store.Invoice) {
	t.Helper()
	e := newEnv(t, opts...)
	var invs []store.Invoice
	for i := 0; i < w; i++ {
		invs = append(invs, e.mustPay(e.order(fmt.Sprintf("w%d", i), 1_000_000)))
	}
	e.clk.Advance(DefaultInvoiceExpiry + 5*time.Minute)
	e.pass()
	e.svc.mu.Lock()
	n := len(e.svc.watch)
	e.svc.mu.Unlock()
	if n != w {
		t.Fatalf("%d watched, want %d", n, w)
	}
	return e, invs
}

func lookupsOf(e *env, invs []store.Invoice) int {
	n := 0
	for _, inv := range invs {
		n += e.w.lookupCount(inv.PaymentHash)
	}
	return n
}

// Watched invoices that are not due cost the real Run loop no ticks: an open invoice is looked up
// about as often with 40 of them as with none.
func TestWatchedNotDueDoNotSlowOpenPolling(t *testing.T) {
	const window = 1500 * time.Millisecond
	rate := func(w int) int {
		e, _ := watchedEnv(t, w, func(e *env) { e.cfg.PollRate = 50 })
		z := e.mustPay(e.order("z", 1_000_000))
		before := e.w.lookupCount(z.PaymentHash)
		ctx, cancel := context.WithTimeout(context.Background(), window)
		defer cancel()
		_ = e.svc.Run(ctx)
		return e.w.lookupCount(z.PaymentHash) - before
	}
	none, forty := rate(0), rate(40)
	t.Logf("open-invoice lookups in %v at PollRate 50: %d with no watched invoices, %d with 40", window, none, forty)
	if none < 5 {
		t.Fatalf("only %d lookups with no watched invoices: the loop is too slow to compare", none)
	}
	if 2*forty < none {
		t.Fatalf("40 watched invoices cut the open invoice's lookups from %d to %d", none, forty)
	}
}

// A snapshot carries only the watched invoices that are due, most overdue first, at most
// max(open invoices, watchFloor) of them.
func TestSnapshotTakesDueWatchedOnly(t *testing.T) {
	e, invs := watchedEnv(t, watchFloor+10)
	ctx := context.Background()
	base := lookupsOf(e, invs)
	if snap, err := e.svc.snapshot(ctx); err != nil || len(snap) != 0 {
		t.Fatalf("just polled: %d entries, %v", len(snap), err)
	}
	e.clk.Advance(staleEvery)
	snap, err := e.svc.snapshot(ctx)
	if err != nil || len(snap) != watchFloor {
		t.Fatalf("all due: %d entries, %v; want the cap %d", len(snap), err, watchFloor)
	}
	for _, inv := range snap {
		if _, err := e.svc.reconcileInv(ctx, inv); err != nil {
			t.Fatal(err)
		}
	}
	snap, _ = e.svc.snapshot(ctx)
	if len(snap) != 10 {
		t.Fatalf("the rest: %d entries, want 10", len(snap))
	}
	if got := lookupsOf(e, invs) - base; got != watchFloor {
		t.Fatalf("%d lookups, want the cap's worth (%d)", got, watchFloor)
	}
}

// After a restart the watched invoices (paid replays included) are spread over one slow-poll
// period instead of all entering the first round.
func TestRestartSpreadsTheWatch(t *testing.T) {
	const w = 20
	e, invs := watchedEnv(t, w)
	e.w.settle(invs[0].PaymentHash, invs[0].AmountMsat) // a late payment, recorded below
	e.restart()
	base := lookupsOf(e, invs)
	e.pass()
	if got := lookupsOf(e, invs) - base; got > 2 {
		t.Fatalf("%d of %d watched invoices looked up in the first round after the restart", got, w)
	}
	e.clk.Advance(staleEvery / 2)
	e.pass()
	if got := lookupsOf(e, invs) - base; got < w/2-2 || got > w/2+2 {
		t.Fatalf("half a period later: %d looked up, want about %d", got, w/2)
	}
	e.clk.Advance(staleEvery/2 - time.Second) // the last is due; the first, polled at the start, is not yet
	e.pass()
	if got := lookupsOf(e, invs) - base; got != w {
		t.Fatalf("a period later: %d looked up, want each once (%d)", got, w)
	}
	if e.transfers(store.Order{ID: invs[0].OrderID}) != 1 {
		t.Fatal("the late payment was not recorded")
	}
}

// A watched invoice unwatched through the front queue (at its deadline) is skipped when the same
// round's snapshot reaches it: it is closed, so not looked up as an open invoice, and nothing is
// recorded or alerted.
func TestUnwatchedEntryInTheSameRoundIsSkipped(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	oy := e.order("y", 1_000_000)
	y := e.mustPay(oy)
	e.clk.Advance(time.Minute)
	ox := e.order("x", 2_000_000)
	x := e.mustPay(ox)
	e.clk.Advance(x.ExpiresAt.Sub(e.clk.Now()) + 5*time.Minute)
	e.pass()
	if closures(t, e, ox) != 1 || closures(t, e, oy) != 1 {
		t.Fatal("setup: not closed")
	}
	e.clk.Advance(x.ExpiresAt.Add(store.DeadlineGrace).Sub(e.clk.Now()) + time.Second) // both past their deadline
	e.w.settle(x.PaymentHash, x.AmountMsat)                                            // a settlement after the deadline
	logs := e.log.String()
	n := e.w.lookupCount(x.PaymentHash)
	var c cursor
	got, ok, err := e.svc.nextInvoice(ctx, &c) // a new snapshot [y, x]: y first
	if err != nil || !ok || got.PaymentHash != y.PaymentHash {
		t.Fatalf("first: %v %v", ok, err)
	}
	if _, err := e.svc.reconcileInv(ctx, got); err != nil {
		t.Fatal(err)
	}
	e.svc.prioritise(x.PaymentHash) // a notification for x
	got, _, _ = e.svc.nextInvoice(ctx, &c)
	if _, err := e.svc.reconcileInv(ctx, got); err != nil { // from the front: past its deadline, unwatched
		t.Fatal(err)
	}
	got, ok, _ = e.svc.nextInvoice(ctx, &c)
	if !ok || got.PaymentHash != x.PaymentHash {
		t.Fatal("x is not in the rest of the snapshot")
	}
	looked, err := e.svc.reconcileInv(ctx, got)
	if err != nil || looked {
		t.Fatalf("looked %v, err %v: want the closed, unwatched invoice skipped", looked, err)
	}
	if e.transfers(ox) != 0 || e.status(ox).State != StateExpired || e.w.lookupCount(x.PaymentHash) != n {
		t.Fatalf("transfers %d, state %s, lookups %d (was %d)", e.transfers(ox), e.status(ox).State, e.w.lookupCount(x.PaymentHash), n)
	}
	if extra := strings.TrimPrefix(e.log.String(), logs); strings.Contains(extra, "ALERT") {
		t.Fatalf("an alert for a closed, unwatched invoice:\n%s", extra)
	}
}

// WaitFlights returns once the Pay flights in progress end, even if their pressers gave up, and
// gives up itself when its context ends.
func TestWaitFlights(t *testing.T) {
	e := newEnv(t)
	o := e.order("a", 1_000_000)
	entered, release := make(chan struct{}), make(chan struct{})
	e.w.before = func(context.Context, nwc.MakeInvoiceParams) (bool, error) {
		close(entered)
		<-release
		return false, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = e.svc.Pay(ctx, o.Token) }()
	<-entered
	cancel() // the presser gives up; the flight goes on
	short, scancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer scancel()
	if err := e.svc.WaitFlights(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a flight in MakeInvoice: %v, want the context's error", err)
	}
	done := make(chan error, 1)
	go func() { done <- e.svc.WaitFlights(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("returned before the flight ended: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(e.open_(o)) != 1 {
		t.Fatal("the flight did not store its invoice before WaitFlights returned")
	}
	if err := e.svc.WaitFlights(context.Background()); err != nil {
		t.Fatalf("no flight: %v", err)
	}
}

// A watched invoice whose late payment was recorded (the transfer) but whose completion failed with
// a returned error is completed at its next visit, also when that visit is past its deadline: the
// deadline drop reads the ledger first.
func TestDeadlineDropCompletesARecordedPayment(t *testing.T) {
	e := newEnv(t)
	o, inv := closedExpired(t, e)
	e.w.settle(inv.PaymentHash, inv.AmountMsat)
	e.clk.Advance(staleEvery)
	e.svc.hook = func(p string) error {
		if p == crashAfterTransfer {
			return errors.New("store error") // returned, not a crash: the service keeps running
		}
		return nil
	}
	if err := e.svc.pass(context.Background()); err == nil {
		t.Fatal("no error")
	}
	e.svc.hook = nil
	if e.transfers(o) != 1 || e.eventCount(store.EventPaid, o.ID) != 0 {
		t.Fatal("setup: want the transfer without the event")
	}
	e.clk.Advance(inv.ExpiresAt.Add(store.DeadlineGrace).Sub(e.clk.Now()) + time.Minute) // past the deadline
	e.pass()
	paidLate(t, e, o)
	e.svc.mu.Lock()
	_, still := e.svc.watch[inv.PaymentHash]
	e.svc.mu.Unlock()
	if still {
		t.Fatal("still watched after its completion")
	}
}

// The spread after a restart goes newest expiry first: with four invoices of different expiries,
// one more becomes due every quarter of a slow-poll period, newest first.
func TestRestartSpreadNewestExpiryFirst(t *testing.T) {
	e := newEnv(t)
	var invs []store.Invoice
	for i := 0; i < 4; i++ {
		invs = append(invs, e.mustPay(e.order(fmt.Sprintf("s%d", i), 1_000_000)))
		e.clk.Advance(time.Minute) // distinct expiries, oldest first
	}
	e.clk.Advance(DefaultInvoiceExpiry + 5*time.Minute)
	e.pass()
	e.restart()
	base := make([]int, len(invs))
	for i, inv := range invs {
		base[i] = e.w.lookupCount(inv.PaymentHash)
	}
	for step := 0; step < len(invs); step++ {
		if step > 0 {
			e.clk.Advance(staleEvery / time.Duration(len(invs)))
		}
		e.pass()
		for i, inv := range invs {
			want := 0
			if i >= len(invs)-1-step { // newest (last made) first
				want = 1
			}
			if got := e.w.lookupCount(inv.PaymentHash) - base[i]; got != want {
				t.Fatalf("step %d: invoice %d (expiry rank %d) looked up %d times, want %d", step, i, i, got, want)
			}
		}
	}
}

// WaitFlights reports no abandoned flight when the last one has landed, even if its context is done
// too: when both are ready, landed wins. (The state is set up directly: in production it is the
// window in which the last flight lands while the grace runs out.)
func TestWaitFlightsLandedWinsOverDoneContext(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	landed := make(chan struct{})
	close(landed)
	e.svc.mu.Lock()
	e.svc.flights["x"] = &flight{done: make(chan struct{})}
	e.svc.landed = landed
	e.svc.mu.Unlock()
	for i := 0; i < 200; i++ {
		if err := e.svc.WaitFlights(ctx); err != nil {
			t.Fatalf("run %d: %v, want nil: the last flight had landed", i, err)
		}
	}
}
