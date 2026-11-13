package rail

import (
	"context"
	"errors"
	"testing"

	"github.com/satbooth/satbooth/store"
)

// The state of a rail's view: every branch of onchain.Rail's switch, reduced to View and Events. events is read only
// past the expiry, and only when the view is neither paid nor late.
func TestState(t *testing.T) {
	paidV := View{Received: 100, Confirmed: 100, Paid: true}
	overV := View{Received: 150, Confirmed: 150, Paid: true, Overpaid: true}
	none := Events{}
	for name, tc := range map[string]struct {
		v       View
		expired bool
		ev      Events
		want    string
		read    bool // events is called
	}{
		"overpaid":                          {overV, false, none, Overpaid, false},
		"overpaid, past expiry":             {overV, true, Events{Unpaid: true}, Overpaid, false},
		"paid":                              {paidV, false, none, Paid, false},
		"paid, past expiry, reversed":       {paidV, true, Events{ReversedNow: true}, Paid, false},
		"late":                              {View{Late: 5}, false, none, Late, false},
		"late and received, past expiry":    {View{Late: 5, Received: 50}, true, Events{Unpaid: true}, Late, false},
		"seen":                              {View{Received: 1}, false, none, Seen, false},
		"waiting":                           {View{}, false, none, Waiting, false},
		"waiting with a confirmed partial":  {View{Confirmed: 50}, false, none, Waiting, false},
		"past expiry, no event, received":   {View{Received: 50}, true, none, Seen, true},
		"past expiry, no event, nothing":    {View{}, true, none, Waiting, true},
		"underpaid event, nothing live":     {View{}, true, Events{Unpaid: true, Underpaid: true}, Underpaid, true},
		"expired event, money live again":   {View{Received: 50}, true, Events{Unpaid: true}, Underpaid, true},
		"expired event, nothing live":       {View{}, true, Events{Unpaid: true}, Expired, true},
		"reversed now, nothing live":        {View{}, true, Events{ReversedNow: true}, Expired, true},
		"reversed now, received":            {View{Received: 10}, true, Events{ReversedNow: true}, Underpaid, true},
		"underpaid flag without unpaid":     {View{}, true, Events{Underpaid: true}, Waiting, true},
		"underpaid flag, received, no flag": {View{Received: 10}, true, Events{Underpaid: true}, Seen, true},
	} {
		called := 0
		got, err := State(tc.v, tc.expired, func() (Events, error) { called++; return tc.ev, nil })
		if err != nil || got != tc.want {
			t.Errorf("%s: State = %q, %v; want %q", name, got, err, tc.want)
		}
		if (called == 1) != tc.read || called > 1 {
			t.Errorf("%s: events called %d times", name, called)
		}
	}
	boom := errors.New("boom")
	if got, err := State(View{Received: 1}, true, func() (Events, error) { return Events{}, boom }); !errors.Is(err, boom) || got != "" {
		t.Fatalf("an events error: %q, %v", got, err)
	}
	if got, err := State(View{Received: 1}, false, func() (Events, error) { return Events{}, boom }); err != nil || got != Seen {
		t.Fatalf("an events error before expiry is not read: %q, %v", got, err)
	}
	if got, err := State(View{}, false, nil); err != nil || got != Waiting {
		t.Fatalf("nil events before expiry: %q, %v", got, err)
	}
}

// The state strings are the API's, unchanged from plan 6.
func TestStateNames(t *testing.T) {
	got := []string{Waiting, Seen, Paid, Overpaid, Late, Underpaid, Expired}
	want := []string{"waiting", "seen", "paid", "overpaid", "late", "underpaid", "expired"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s, want %s", got[i], want[i])
		}
	}
}

// A Peer is anything with a View and RevertedSeqs (onchain.Service is one); this stub pins the signatures.
type stubPeer struct{}

func (stubPeer) View(context.Context, store.Order) (View, bool, error) { return View{}, false, nil }

func (stubPeer) RevertedSeqs(context.Context, store.Order) ([]int, error) { return nil, nil }

var _ Peer = stubPeer{}
