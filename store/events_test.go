package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEventID(t *testing.T) {
	oid := strings.Repeat("ab", 16)
	sum := sha256.Sum256([]byte("satbooth/event/v1" + "order.paid" + oid))
	want := hex.EncodeToString(sum[:])[:32]
	if got := EventID("order.paid", oid, 1); got != want {
		t.Fatalf("EventID = %s, want %s", got, want)
	}
	if EventID("order.paid", oid, 1) == EventID("order.expired", oid, 1) {
		t.Fatal("types collide")
	}
}

func TestAddEvent(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	o := mkOrder(t, s, "r", 1000, t0.Add(time.Hour))
	clk.set(t0.Add(time.Minute))

	// A caller-set CreatedAt is ignored: the store stamps its own clock.
	ev := Event{ID: EventID(EventPaid, o.ID, 1), Type: EventPaid, OrderID: o.ID, CreatedAt: t0.Add(30 * time.Second)}
	added, err := s.AddEvent(ctx, ev)
	if err != nil || !added {
		t.Fatalf("add: %v %v", added, err)
	}
	added, err = s.AddEvent(ctx, ev)
	if err != nil || added {
		t.Fatalf("add again: %v %v", added, err)
	}
	// ID and CreatedAt left empty are filled in (from the derivation and the clock); same event, so not added again.
	added, err = s.AddEvent(ctx, Event{Type: EventPaid, OrderID: o.ID})
	if err != nil || added {
		t.Fatalf("add with derived id: %v %v", added, err)
	}
	// The order is paid: an order.expired after it is not added (one final event per order), an order.late is.
	added, err = s.AddEvent(ctx, Event{Type: EventExpired, OrderID: o.ID})
	if err != nil || added {
		t.Fatalf("expired after paid: %v %v", added, err)
	}
	added, err = s.AddEvent(ctx, Event{Type: EventLate, OrderID: o.ID})
	if err != nil || !added {
		t.Fatalf("late: %v %v", added, err)
	}
	pend, err := s.UndeliveredEvents(ctx, MainEndpoint, t0, time.Time{})
	if err != nil || len(pend) != 2 {
		t.Fatalf("pending: %+v %v", pend, err)
	}
	for _, p := range pend {
		if p.Type == EventExpired {
			t.Fatalf("the refused order.expired is pending: %+v", p)
		}
	}
	for _, p := range pend {
		if !p.CreatedAt.Equal(t0.Add(time.Minute)) || p.Failures != 0 || !p.LastAt.IsZero() {
			t.Fatalf("pending: %+v", p)
		}
	}
	if pend[0].ID != EventID(EventPaid, o.ID, 1) && pend[1].ID != EventID(EventPaid, o.ID, 1) {
		t.Fatalf("pending: %+v", pend)
	}

	bad := map[string]Event{
		"unknown type": {Type: "order.refunded", OrderID: o.ID},
		"empty type":   {OrderID: o.ID},
		"wrong id":     {ID: strings.Repeat("a", 32), Type: EventPaid, OrderID: o.ID},
		"bad order id": {Type: EventPaid, OrderID: "zz"},
	}
	for name, e := range bad {
		if _, err := s.AddEvent(ctx, e); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.AddEvent(ctx, Event{Type: EventPaid, OrderID: strings.Repeat("0", 32)}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown order: %v", err)
	}
}

func TestAddEventConcurrent(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	o := mkOrder(t, s, "r", 1000, t0.Add(time.Hour))
	const n = 32
	var wg sync.WaitGroup
	res := make([]bool, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := s.AddEvent(ctx, Event{Type: EventPaid, OrderID: o.ID})
			if err != nil {
				t.Error(err)
			}
			res[i] = ok
		}()
	}
	close(start)
	wg.Wait()
	c := 0
	for _, r := range res {
		if r {
			c++
		}
	}
	if c != 1 {
		t.Fatalf("%d adds won", c)
	}
}

func TestUndeliveredEvents(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	o := mkOrder(t, s, "r", 1000, t0.Add(time.Hour))
	clk.set(t0.Add(time.Minute))
	if _, err := s.AddEvent(ctx, Event{Type: EventPaid, OrderID: o.ID}); err != nil {
		t.Fatal(err)
	}
	id := EventID(EventPaid, o.ID, 1)
	undelivered := func(since time.Time) []PendingEvent {
		t.Helper()
		p, err := s.UndeliveredEvents(ctx, MainEndpoint, since, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := undelivered(t0); len(p) != 1 || p[0].Failures != 0 || !p[0].LastAt.IsZero() || p[0].ID != id || p[0].Type != EventPaid || p[0].OrderID != o.ID {
		t.Fatalf("new event: %+v", p)
	}
	// since is inclusive and decides what is listed; there is no built-in give-up.
	if len(undelivered(t0.Add(time.Minute))) != 1 || len(undelivered(t0.Add(time.Minute+time.Second))) != 0 {
		t.Fatal("since boundary")
	}
	var want []Attempt
	for n := 1; n <= 12; n++ {
		a := Attempt{Endpoint: MainEndpoint, EventID: id, At: t0.Add(time.Duration(n) * time.Minute), Status: 500 + n}
		want = append(want, a)
		if err := s.AddAttempt(ctx, a); err != nil {
			t.Fatal(err)
		}
		p := undelivered(t0)
		if len(p) != 1 || p[0].Failures != n || !p[0].LastAt.Equal(a.At) {
			t.Fatalf("after %d failures: %+v", n, p)
		}
	}
	got, err := s.AttemptsFor(ctx, MainEndpoint, id)
	if err != nil || len(got) != 12 {
		t.Fatalf("attempts: %d %v", len(got), err)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("attempt %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// An event far older than any window is still listed when since allows it.
	if len(undelivered(t0.Add(-1000*time.Hour))) != 1 {
		t.Fatal("old since")
	}
	// A successful attempt ends it, even though earlier ones failed.
	if err := s.AddAttempt(ctx, Attempt{Endpoint: MainEndpoint, EventID: id, At: t0.Add(time.Hour), Status: 200, OK: true}); err != nil {
		t.Fatal(err)
	}
	if p := undelivered(t0); len(p) != 0 {
		t.Fatalf("delivered event still listed: %+v", p)
	}
}

func TestAddAttemptRefusals(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	o := mkOrder(t, s, "r", 1000, t0.Add(time.Hour))
	if _, err := s.AddEvent(ctx, Event{Type: EventPaid, OrderID: o.ID}); err != nil {
		t.Fatal(err)
	}
	id := EventID(EventPaid, o.ID, 1)
	for name, a := range map[string]Attempt{
		"status -1":    {Endpoint: MainEndpoint, EventID: id, Status: -1},
		"status 1000":  {Endpoint: MainEndpoint, EventID: id, Status: 1000},
		"bad event id": {Endpoint: MainEndpoint, EventID: "x", Status: 200},
	} {
		if err := s.AddAttempt(ctx, a); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.AddAttempt(ctx, Attempt{Endpoint: MainEndpoint, EventID: strings.Repeat("0", 32), Status: 200}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown event: %v", err)
	}
	// Status 0 means no HTTP response (a timeout or a refused connection); a zero At is stamped from the clock.
	clk.set(t0.Add(9 * time.Second))
	if err := s.AddAttempt(ctx, Attempt{Endpoint: MainEndpoint, EventID: id, Status: 0}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.AttemptsFor(ctx, MainEndpoint, id)
	if len(got) != 1 || !got[0].At.Equal(t0.Add(9*time.Second)) || got[0].OK || got[0].Status != 0 {
		t.Fatalf("%+v", got)
	}
	if got, err := s.AttemptsFor(ctx, MainEndpoint, strings.Repeat("0", 32)); err != nil || len(got) != 0 {
		t.Fatalf("unknown event: %+v %v", got, err)
	}
}

func TestOrdersToExpire(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	exp := t0.Add(time.Hour)
	clk.set(t0.Add(45 * time.Minute)) // closes are never in the store clock's future
	noInvoice := mkOrder(t, s, "none", 1000, exp)
	openInv := mkOrder(t, s, "open", 1000, exp)
	mkInvoice(t, s, openInv, 'a', t0.Add(30*time.Minute))
	closedInv := mkOrder(t, s, "closed", 1000, exp)
	mkInvoice(t, s, closedInv, 'b', t0.Add(30*time.Minute))
	if _, err := s.CloseInvoice(ctx, hash('b'), t0.Add(31*time.Minute), "lookup"); err != nil {
		t.Fatal(err)
	}
	halfClosed := mkOrder(t, s, "half", 1000, exp)
	mkInvoice(t, s, halfClosed, 'c', t0.Add(30*time.Minute))
	mkInvoice(t, s, halfClosed, 'd', t0.Add(40*time.Minute))
	if _, err := s.CloseInvoice(ctx, hash('c'), t0.Add(31*time.Minute), "lookup"); err != nil {
		t.Fatal(err)
	}
	expiredAlready := mkOrder(t, s, "expired", 1000, exp)
	paid := mkOrder(t, s, "paid", 1000, exp)
	later := mkOrder(t, s, "later", 1000, exp.Add(time.Hour))
	quoted := mkQuotedOrder(t, s, "quoted", 1000, 0, exp, testQuote()) // a priced order expires like any other
	mkMoneroOrder(t, s, "monero", 7000, exp)                           // the Monero rail settles a Monero order, never lnpay
	both, _, err := s.CreateOrder(ctx, Order{AccountID: "acct-1", MerchantRef: "ln-xmr", AmountMsat: 1000, AmountPiconero: 7000, ExpiresAt: exp})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Event{{Type: EventExpired, OrderID: expiredAlready.ID}, {Type: EventPaid, OrderID: paid.ID}} {
		if _, err := s.AddEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(os []Order) map[string]bool {
		m := map[string]bool{}
		for _, o := range os {
			m[o.ID] = true
		}
		return m
	}
	got, err := s.OrdersToExpire(ctx, exp.Add(-time.Second))
	if err != nil || len(got) != 0 {
		t.Fatalf("before expiry: %+v %v", got, err)
	}
	got, err = s.OrdersToExpire(ctx, exp)
	if err != nil {
		t.Fatal(err)
	}
	m := ids(got)
	if len(got) != 3 || !m[noInvoice.ID] || !m[closedInv.ID] || !m[quoted.ID] {
		t.Fatalf("at expiry: %+v", got)
	}
	// Close the last open invoices and the half-closed order joins.
	if _, err := s.CloseInvoice(ctx, hash('d'), t0.Add(41*time.Minute), "lookup"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseInvoice(ctx, hash('a'), t0.Add(31*time.Minute), "lookup"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.OrdersToExpire(ctx, exp.Add(time.Hour))
	m = ids(got)
	if len(got) != 6 || !m[openInv.ID] || !m[halfClosed.ID] || !m[later.ID] || !m[quoted.ID] || m[paid.ID] || m[expiredAlready.ID] || m[both.ID] {
		t.Fatalf("later: %+v", got)
	}
	// An expired event removes it from the list.
	if _, err := s.AddEvent(ctx, Event{Type: EventExpired, OrderID: noInvoice.ID}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.OrdersToExpire(ctx, exp.Add(time.Hour))
	if len(got) != 5 || ids(got)[noInvoice.ID] {
		t.Fatalf("after event: %+v", got)
	}
}

// EventsForOrder lists one order's events, oldest first, with their seqs, and nothing of another order.
func TestEventsForOrder(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	o := mkBothOrder(t, s, "a", 1000, 500, t0.Add(time.Hour))
	other := mkBothOrder(t, s, "b", 1000, 500, t0.Add(time.Hour))
	if l, err := s.EventsForOrder(ctx, o.ID); err != nil || len(l) != 0 {
		t.Fatalf("empty: %+v %v", l, err)
	}
	add := func(id, typ string, seq int) {
		t.Helper()
		if _, err := s.AddEvent(ctx, Event{Type: typ, OrderID: id, Seq: seq}); err != nil {
			t.Fatal(err)
		}
	}
	add(o.ID, EventPaid, 0)
	clk.set(t0.Add(time.Second))
	add(o.ID, EventReverted, 1)
	add(other.ID, EventExpired, 0)
	clk.set(t0.Add(2 * time.Second))
	add(o.ID, EventPaidAgain, 1)
	add(o.ID, EventReverted, 2)
	l, err := s.EventsForOrder(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		typ string
		seq int
	}{{EventPaid, 1}, {EventReverted, 1}, {EventPaidAgain, 1}, {EventReverted, 2}}
	if len(l) != len(want) {
		t.Fatalf("got %+v", l)
	}
	for i, w := range want {
		e := l[i]
		if e.Type != w.typ || e.Seq != w.seq || e.OrderID != o.ID || e.ID != EventID(w.typ, o.ID, w.seq) || e.CreatedAt.IsZero() {
			t.Errorf("event %d: %+v, want %s seq %d", i, e, w.typ, w.seq)
		}
	}
	if l, err := s.EventsForOrder(ctx, "not an id"); err != nil || len(l) != 0 {
		t.Fatalf("bad id: %+v %v", l, err)
	}
}

// One final event per order: an order.expired or order.underpaid is not added when the order has order.paid,
// order.expired or order.underpaid; order.paid after a final event is (Ruling S), and so are order.late,
// order.reverted and order.paid_again. Nothing is written for a refused one.
func TestFinalEventOnce(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	i := 0
	order := func() Order {
		i++
		return mkBothOrder(t, s, "f"+itoa(int64(i)), 1000, 500, t0.Add(time.Hour))
	}
	add := func(o Order, typ string, seq int) bool {
		t.Helper()
		ok, err := s.AddEvent(ctx, Event{Type: typ, OrderID: o.ID, Seq: seq})
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		return ok
	}
	for _, c := range []struct {
		first, second string
		want          bool
	}{
		{EventExpired, EventUnderpaid, false},
		{EventUnderpaid, EventExpired, false},
		{EventPaid, EventExpired, false},
		{EventPaid, EventUnderpaid, false},
		{EventExpired, EventPaid, true},
		{EventUnderpaid, EventPaid, true},
		{EventExpired, EventLate, true},
		{EventUnderpaid, EventReverted, true},
		{EventLate, EventExpired, true},
		{EventReverted, EventUnderpaid, true},
	} {
		o := order()
		if !add(o, c.first, 1) {
			t.Fatalf("%s first: not added", c.first)
		}
		if got := add(o, c.second, 1); got != c.want {
			t.Errorf("%s then %s: added %v, want %v", c.first, c.second, got, c.want)
		}
		l, err := s.EventsForOrder(ctx, o.ID)
		if err != nil || len(l) != map[bool]int{true: 2, false: 1}[c.want] {
			t.Errorf("%s then %s: events %+v %v", c.first, c.second, l, err)
		}
	}
	// After a final event and a reversal, the paid_again of that reversal is still added; a final after it is not.
	o := order()
	add(o, EventPaid, 1)
	add(o, EventReverted, 1)
	if !add(o, EventPaidAgain, 1) {
		t.Error("paid_again after a final event was refused")
	}
	if add(o, EventUnderpaid, 1) {
		t.Error("underpaid after paid, reverted and paid_again was added")
	}
	// 50 goroutines race order.expired and order.underpaid on one order: exactly one final event is stored. Repeated
	// on fresh orders, so the race goes both ways.
	var expiredWon, underpaidWon int
	for round := 0; round < 10; round++ {
		o := order()
		var wg sync.WaitGroup
		start := make(chan struct{})
		var mu sync.Mutex
		won := 0
		for g := 0; g < 50; g++ {
			typ := []string{EventExpired, EventUnderpaid}[g%2]
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ok, err := s.AddEvent(ctx, Event{Type: typ, OrderID: o.ID})
				if err != nil {
					t.Error(err)
				}
				if ok {
					mu.Lock()
					won++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		l, err := s.EventsForOrder(ctx, o.ID)
		if err != nil || len(l) != 1 || won != 1 {
			t.Fatalf("round %d: %d added, events %+v %v", round, won, l, err)
		}
		if l[0].Type == EventExpired {
			expiredWon++
		} else {
			underpaidWon++
		}
	}
	t.Logf("order.expired won %d rounds, order.underpaid %d", expiredWon, underpaidWon)
}
