package rail

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/satbooth/satbooth/store"
)

// fakePaying records PaidEvents' calls in order; failAt, when set, fails the call it names.
type fakePaying struct {
	calls  []string
	events map[string]bool // type/seq of the events stored
	failAt string
}

var errFake = errors.New("fake failure")

func (f *fakePaying) fail(name string) error {
	f.calls = append(f.calls, name)
	if f.failAt == name {
		return errFake
	}
	return nil
}

func (f *fakePaying) AddAccountPaid(_ context.Context, orderID string) (bool, error) {
	return true, f.fail("fact:" + orderID)
}

func (f *fakePaying) MarkKnown(account string) { f.calls = append(f.calls, "known:"+account) }

func (f *fakePaying) AddEvent(_ context.Context, typ, orderID string, seq int) (bool, error) {
	key := typ + "/" + string(rune('0'+seq))
	if err := f.fail("event:" + key); err != nil {
		return false, err
	}
	if f.events == nil {
		f.events = map[string]bool{}
	}
	added := !f.events[key]
	f.events[key] = true
	return added, nil
}

func (f *fakePaying) At(point string) error { return f.fail("at:" + point) }

func (f *fakePaying) Alert(line, orderID string) { f.calls = append(f.calls, "alert:"+line) }

func TestPaidEvents(t *testing.T) {
	o := store.Order{ID: "o1", AccountID: "acct"}
	const again = "AGAIN"
	for name, tc := range map[string]struct {
		ev     OrderEvents
		failAt string
		want   []string
		err    bool
		after  OrderEvents
	}{
		"first paid: the fact, the hook, MarkKnown, the event": {
			ev:    OrderEvents{PaidAgain: map[int]bool{}},
			want:  []string{"fact:o1", "at:settle:after-account", "known:acct", "event:order.paid/1"},
			after: OrderEvents{Paid: true, Final: true, PaidAgain: map[int]bool{}},
		},
		"paid after a final unpaid event alerts": {
			ev:    OrderEvents{Final: true, Unpaid: true, PaidAgain: map[int]bool{}},
			want:  []string{"fact:o1", "at:settle:after-account", "known:acct", "event:order.paid/1", "alert:" + LogPaidAfterFinal},
			after: OrderEvents{Paid: true, Final: true, Unpaid: true, PaidAgain: map[int]bool{}},
		},
		"already paid, nothing reverted: nothing": {
			ev:    OrderEvents{Paid: true, Final: true, PaidAgain: map[int]bool{}},
			after: OrderEvents{Paid: true, Final: true, PaidAgain: map[int]bool{}},
		},
		"reverted twice, paid_again 2 missing": {
			ev:    OrderEvents{Paid: true, Final: true, Reverted: 2, PaidAgain: map[int]bool{1: true}},
			want:  []string{"event:order.paid_again/2", "alert:" + again},
			after: OrderEvents{Paid: true, Final: true, Reverted: 2, PaidAgain: map[int]bool{1: true, 2: true}},
		},
		"reverted, paid_again there": {
			ev:    OrderEvents{Paid: true, Final: true, Reverted: 1, PaidAgain: map[int]bool{1: true}},
			after: OrderEvents{Paid: true, Final: true, Reverted: 1, PaidAgain: map[int]bool{1: true}},
		},
		"a crash after the fact: no MarkKnown, no event": {
			ev:     OrderEvents{PaidAgain: map[int]bool{}},
			failAt: "at:settle:after-account",
			want:   []string{"fact:o1", "at:settle:after-account"},
			err:    true,
			after:  OrderEvents{PaidAgain: map[int]bool{}},
		},
		"the fact fails: nothing else": {
			ev:     OrderEvents{PaidAgain: map[int]bool{}},
			failAt: "fact:o1",
			want:   []string{"fact:o1"},
			err:    true,
			after:  OrderEvents{PaidAgain: map[int]bool{}},
		},
		"the paid event fails: the flags stay": {
			ev:     OrderEvents{PaidAgain: map[int]bool{}},
			failAt: "event:order.paid/1",
			want:   []string{"fact:o1", "at:settle:after-account", "known:acct", "event:order.paid/1"},
			err:    true,
			after:  OrderEvents{PaidAgain: map[int]bool{}},
		},
		"the paid_again event fails: no alert": {
			ev:     OrderEvents{Paid: true, Final: true, Reverted: 1, PaidAgain: map[int]bool{}},
			failAt: "event:order.paid_again/1",
			want:   []string{"event:order.paid_again/1"},
			err:    true,
			after:  OrderEvents{Paid: true, Final: true, Reverted: 1, PaidAgain: map[int]bool{}},
		},
	} {
		f := &fakePaying{failAt: tc.failAt}
		ev := tc.ev
		err := PaidEvents(context.Background(), f, o, &ev, again)
		if (err != nil) != tc.err || !slices.Equal(f.calls, tc.want) {
			t.Errorf("%s: err %v, calls %q, want %q", name, err, f.calls, tc.want)
		}
		if ev.Paid != tc.after.Paid || ev.Final != tc.after.Final || ev.Unpaid != tc.after.Unpaid ||
			len(ev.PaidAgain) != len(tc.after.PaidAgain) {
			t.Errorf("%s: events after %+v, want %+v", name, ev, tc.after)
		}
		for k := range tc.after.PaidAgain {
			if !ev.PaidAgain[k] {
				t.Errorf("%s: paid_again %d not set", name, k)
			}
		}
	}
}

// A replay of an order whose events already stored (AddEvent answers false) logs no ALERT.
func TestPaidEventsReplayLogsNothing(t *testing.T) {
	o := store.Order{ID: "o1", AccountID: "acct"}
	f := &fakePaying{events: map[string]bool{"order.paid/1": true, "order.paid_again/1": true}}
	ev := OrderEvents{Unpaid: true, Final: true, Reverted: 1, PaidAgain: map[int]bool{}}
	if err := PaidEvents(context.Background(), f, o, &ev, "AGAIN"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if len(c) > 6 && c[:6] == "alert:" {
			t.Fatalf("an ALERT on a replay: %q", f.calls)
		}
	}
	if !ev.Paid || !ev.PaidAgain[1] {
		t.Fatalf("events after %+v", ev)
	}
}
