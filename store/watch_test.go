package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// woken reports whether a wake-up is pending, taking it.
func woken(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// blockBound is how long a write may take before the test calls it blocked. A send that blocks does so for ever, so
// the bound only has to be far above what a thousand synchronous writes take on a loaded machine (they took over 10 s
// in one run of the whole suite).
const blockBound = 3 * time.Minute

// bounded runs fn in a goroutine and fails the test if it does not return within blockBound. On a block it first
// drains the given channels for good, so that the blocked send completes and the deferred stops can take the
// watchers' lock: a writer blocked on a subscriber that does not read must fail the test with a message, not hang
// the binary.
func bounded(t *testing.T, what string, fn func() error, chans ...<-chan struct{}) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(blockBound):
		for _, ch := range chans {
			go func() {
				for range ch {
				}
			}()
		}
		t.Fatalf("%s did not return in %v: blocked on a subscriber that does not read", what, blockBound)
	}
}

func addPaid(t *testing.T, s *Store, orderID string) bool {
	t.Helper()
	ok, err := s.AddEvent(context.Background(), Event{Type: EventPaid, OrderID: orderID})
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// A wake-up comes for an inserted event only, once the write lock is released and the event can be read; never for a
// duplicate, a refusal or an unknown order.
func TestWatchEventsAfterCommit(t *testing.T) {
	s, _, _ := freshStore(t)
	ctx := context.Background()
	ch, stop := s.WatchEvents()
	defer stop()
	o := mkOrder(t, s, "w1", 1000, t0.Add(time.Hour))
	if woken(ch) {
		t.Fatal("a wake-up before any event")
	}
	if !addPaid(t, s, o.ID) {
		t.Fatal("event not added")
	}
	if !woken(ch) {
		t.Fatal("no wake-up for an inserted event")
	}
	if addPaid(t, s, o.ID) {
		t.Fatal("duplicate added")
	}
	if woken(ch) {
		t.Fatal("wake-up for a duplicate")
	}
	if _, err := s.AddEvent(ctx, Event{Type: EventPaid, OrderID: strings.Repeat("a", 32)}); err == nil {
		t.Fatal("unknown order accepted")
	}
	if _, err := s.AddEvent(ctx, Event{Type: "order.nope", OrderID: o.ID}); err == nil {
		t.Fatal("unknown type accepted")
	}
	if woken(ch) {
		t.Fatal("wake-up for a failed AddEvent")
	}
	// A final event after the order's final event is not added: no wake-up either.
	if ok, err := s.AddEvent(ctx, Event{Type: EventExpired, OrderID: o.ID}); err != nil || ok || woken(ch) {
		t.Fatalf("expired after paid: %v %v", ok, err)
	}
}

// The wake-up is raised after the transaction, with the write lock released, and the event is readable when it
// arrives. A watcher that gets the lock at once proves it was not raised inside the transaction.
func TestWatchEventsOnlyAfterTheWriteLockIsReleased(t *testing.T) {
	s, _, _ := freshStore(t)
	ctx := context.Background()
	ch, stop := s.WatchEvents()
	defer stop()
	type seen struct {
		locked bool
		events int
	}
	var order atomic.Value // the order id of the round, set before its event is added
	got := make(chan seen)
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		for {
			select {
			case <-quit:
				return
			case <-ch:
			}
			var r seen
			if r.locked = !s.mu.TryLock(); !r.locked {
				s.mu.Unlock()
			}
			evs, _ := s.EventsForOrder(ctx, order.Load().(string))
			r.events = len(evs)
			select {
			case got <- r:
			case <-quit:
				return
			}
		}
	}()
	for i := range 20 {
		o := mkOrder(t, s, fmt.Sprintf("wl%d", i), 1000, t0.Add(time.Hour))
		order.Store(o.ID)
		if !addPaid(t, s, o.ID) {
			t.Fatal("not added")
		}
		select {
		case r := <-got:
			if r.locked {
				t.Fatalf("round %d: the wake-up came while the write lock was held", i)
			}
			if r.events != 1 {
				t.Fatalf("round %d: the event was not readable at the wake-up (%d)", i, r.events)
			}
		case <-time.After(time.Minute):
			t.Fatalf("round %d: no wake-up", i)
		}
	}
}

// A wake-up raised even when the commit fails is not testable without a fault; the placement is: wake follows the
// write call unconditionally on added. Two inserts while one is pending merge into one.
func TestWatchMerges(t *testing.T) {
	s, _, _ := freshStore(t)
	ch, stop := s.WatchEvents()
	defer stop()
	a := mkOrder(t, s, "m1", 1000, t0.Add(time.Hour))
	b := mkOrder(t, s, "m2", 1000, t0.Add(time.Hour))
	bounded(t, "two inserts", func() error {
		for _, o := range []Order{a, b} {
			if _, err := s.AddEvent(context.Background(), Event{Type: EventPaid, OrderID: o.ID}); err != nil {
				return err
			}
		}
		return nil
	}, ch)
	if !woken(ch) {
		t.Fatal("no wake-up")
	}
	if woken(ch) {
		t.Fatal("two inserts gave two wake-ups")
	}
}

// Eight subscribers that never read cannot block the writers.
func TestWatchNeverBlocks(t *testing.T) {
	s, _, _ := freshStore(t)
	ctx := context.Background()
	var chans []<-chan struct{}
	for range 8 {
		ch, stop := s.WatchEvents()
		chA, stopA := s.WatchAlerts()
		defer stop()
		defer stopA()
		chans = append(chans, ch, chA)
	}
	bounded(t, "100 events and 100 alerts", func() error {
		for i := range 100 {
			o, _, err := s.CreateOrder(ctx, Order{AccountID: "acct-1", MerchantRef: fmt.Sprintf("nb%d", i), AmountMsat: 1000, ExpiresAt: t0.Add(time.Hour)})
			if err != nil {
				return err
			}
			if _, err := s.AddEvent(ctx, Event{Type: EventPaid, OrderID: o.ID}); err != nil {
				return err
			}
		}
		for i := range 100 {
			if _, err := s.AddAlert(ctx, Alert{Message: testAlertMsg, Fields: fmt.Sprintf(`{"n":%d}`, i)}); err != nil {
				return err
			}
		}
		return nil
	}, chans...)
}

func TestWatchStop(t *testing.T) {
	s, _, _ := freshStore(t)
	ch, stop := s.WatchEvents()
	other, stopOther := s.WatchEvents()
	defer stopOther()
	stop()
	stop() // idempotent
	o := mkOrder(t, s, "st1", 1000, t0.Add(time.Hour))
	addPaid(t, s, o.ID)
	if woken(ch) {
		t.Fatal("a wake-up after the stop")
	}
	if !woken(other) {
		t.Fatal("stopping one watch ended another")
	}
}

// A wake-up pending when the watch stops stays in the channel (which is never closed), and no second one follows.
func TestWatchStopKeepsThePendingWakeUp(t *testing.T) {
	s, _, _ := freshStore(t)
	ch, stop := s.WatchEvents()
	a := mkOrder(t, s, "sp1", 1000, t0.Add(time.Hour))
	b := mkOrder(t, s, "sp2", 1000, t0.Add(time.Hour))
	addPaid(t, s, a.ID)
	stop()
	addPaid(t, s, b.ID)
	if !woken(ch) {
		t.Fatal("the wake-up pending at the stop is gone")
	}
	if woken(ch) {
		t.Fatal("a wake-up after the stop")
	}
}

// Only a new alert row wakes: not a hit, not a settled repeat, not an alert refused for a full day. Events do not wake
// the alert watch and alerts do not wake the event watch.
func TestWatchAlertsOnNewRowOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	clk := &testClock{t: time.Unix(1_800_057_600, 0)} // 00:00 UTC
	s, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ach, stopA := s.WatchAlerts()
	defer stopA()
	ech, stopE := s.WatchEvents()
	defer stopE()
	a := Alert{Message: testAlertMsg, Fields: `{"k":1}`}
	addAlert(t, s, a)
	if !woken(ach) {
		t.Fatal("no wake-up for a new alert")
	}
	if woken(ech) {
		t.Fatal("an alert woke the event watch")
	}
	addAlert(t, s, a) // a hit
	if woken(ach) {
		t.Fatal("wake-up for a repeat (a hit)")
	}
	// Repeats up to the cap, then a settled repeat (no write).
	for range MaxAlertRepeats + 2 {
		addAlert(t, s, a)
	}
	if woken(ach) {
		t.Fatal("wake-up for a repeat")
	}
	// A delivered alert's repeat records nothing.
	b := Alert{Message: testAlertMsg, Fields: `{"k":2}`}
	addAlert(t, s, b)
	woken(ach)
	if err := s.AddAlertAttempt(ctx, AlertAttempt{AlertID: AlertID(b.Message, b.Fields, clk.now()), At: clk.now(), Status: 200, OK: true}); err != nil {
		t.Fatal(err)
	}
	addAlert(t, s, b)
	if woken(ach) {
		t.Fatal("wake-up for the repeat of a delivered alert")
	}
	// The rest of the day, then the refused one.
	bounded(t, "a day of alerts", func() error {
		for i := range MaxAlertsPerDay {
			clk.set(time.Unix(1_800_057_600+int64(i)*60, 0))
			if _, err := s.AddAlert(ctx, Alert{Message: testAlertMsg, Fields: fmt.Sprintf(`{"count":%d}`, i+10)}); err != nil {
				return err
			}
		}
		return nil
	}, ach, ech)
	woken(ach)
	if addAlert(t, s, Alert{Message: testAlertMsg, Fields: `{"count":99999}`}) {
		t.Fatal("the full day queued an alert")
	}
	if woken(ach) {
		t.Fatal("wake-up for an alert refused for a full day")
	}
	if woken(ech) {
		t.Fatal("alerts woke the event watch")
	}
}

// insertSites match every way SQLite writes a row of a watched table: INSERT, INSERT OR <action> and REPLACE, with any
// white space (a line break included) between the words.
var insertSites = map[string][]*regexp.Regexp{}

func init() {
	for _, table := range []string{"events", "alerts"} {
		insertSites[table] = []*regexp.Regexp{
			regexp.MustCompile(`(?i)\binsert\s+(or\s+\w+\s+)?into\s+"?` + table + `\b`),
			regexp.MustCompile(`(?i)\breplace\s+into\s+"?` + table + `\b`),
		}
	}
}

// Every event and alert the process writes goes through AddEvent and AddAlert, which raise the wake-up: no second
// insert site may appear without this test being read again.
func TestOneInsertSitePerWatchedTable(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for table, res := range insertSites {
			for _, re := range res {
				counts[table] += len(re.FindAll(b, -1))
			}
		}
	}
	if counts["events"] != 1 || counts["alerts"] != 1 {
		t.Fatalf("INSERT INTO events: %d, INSERT INTO alerts: %d; want one each (AddEvent and AddAlert raise the wake-up)", counts["events"], counts["alerts"])
	}
}

// BenchmarkUndeliveredEvents is one listing with nothing pending: 23,000 delivered events and 8 endpoints, on the real
// driver. It is the cost of one pass of one deliverer over that store. The size is synthetic: about a year of events at
// 63 a day.
func BenchmarkUndeliveredEvents(b *testing.B) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(b.TempDir(), "store.db"), WithClock(func() time.Time { return t0 }))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	const events = 23000
	endpoints := []string{"e1", "e2", "e3", "e4", "e5", "e6", "e7", "e8"}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	for i := range events {
		id := fmt.Sprintf("%032x", i+1)
		order := fmt.Sprintf("%032x", i+1+events)
		at := t0.Add(-time.Duration(events-i) * 5 * time.Second)
		if _, err := tx.ExecContext(ctx, `INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at)
VALUES (?, ?, 'acct', ?, 1000, 0, 0, ?, ?)`, order, fmt.Sprintf("%032x", i+1+2*events), fmt.Sprintf("r%d", i), ts(at), ts(at.Add(time.Hour))); err != nil {
			b.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (id, type, order_id, seq, created_at) VALUES (?, 'order.paid', ?, 1, ?)`,
			id, order, ts(at)); err != nil {
			b.Fatal(err)
		}
		for _, ep := range endpoints {
			if _, err := tx.ExecContext(ctx, `INSERT INTO webhook_attempts (endpoint, event_id, seq, at, status, ok) VALUES (?, ?, 1, ?, 200, 1)`,
				ep, id, ts(at)); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	since := t0.Add(-RetryWindowForBench - 7*24*time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := s.UndeliveredEvents(ctx, endpoints[i%len(endpoints)], since, time.Time{})
		if err != nil || len(p) != 0 {
			b.Fatalf("%d pending, err %v", len(p), err)
		}
	}
}

// RetryWindowForBench is webhook.RetryWindow, which store cannot import.
const RetryWindowForBench = 24 * time.Hour
