package rail

import "testing"

// The four facts a rail's settle reads of its peer, at the amount's boundaries; no amount on the peer reads nothing.
func TestPeerFactsOf(t *testing.T) {
	const amt = 1000
	for name, tc := range map[string]struct {
		v    View
		ok   bool
		want PeerFacts
	}{
		"no amount on the peer":   {View{Received: amt, Late: 5, Paid: true}, false, PeerFacts{}},
		"nothing":                 {View{}, true, PeerFacts{}},
		"received one":            {View{Received: 1}, true, PeerFacts{Money: true, Received: true}},
		"received below":          {View{Received: amt - 1}, true, PeerFacts{Money: true, Received: true}},
		"received at the amount":  {View{Received: amt}, true, PeerFacts{Money: true, Pending: true, Received: true}},
		"received above":          {View{Received: amt + 1}, true, PeerFacts{Money: true, Pending: true, Received: true}},
		"paid":                    {View{Received: amt, Confirmed: amt, Paid: true}, true, PeerFacts{Paid: true, Money: true, Received: true}},
		"late only":               {View{Late: 1}, true, PeerFacts{Money: true}},
		"paid, nothing live":      {View{Confirmed: amt, Paid: true}, true, PeerFacts{Paid: true}},
		"late and received below": {View{Received: 1, Late: 1}, true, PeerFacts{Money: true, Received: true}},
		"stale, nothing":          {View{Stale: true}, true, PeerFacts{Stale: true}},
		"stale, received":         {View{Received: 1, Stale: true}, true, PeerFacts{Money: true, Received: true, Stale: true}},
		"stale, paid":             {View{Confirmed: amt, Paid: true, Stale: true}, true, PeerFacts{Paid: true}},
		"stale, no amount":        {View{Stale: true}, false, PeerFacts{}},
		"held, received":          {View{Received: 1, Held: true}, true, PeerFacts{Money: true, Received: true, Stale: true, Held: true}},
		"held, paid":              {View{Confirmed: amt, Paid: true, Held: true}, true, PeerFacts{Paid: true}},
		"held, no amount":         {View{Held: true}, false, PeerFacts{}},
	} {
		if got := PeerFactsOf(tc.v, tc.ok, amt); got != tc.want {
			t.Errorf("%s: %+v, want %+v", name, got, tc.want)
		}
	}
}

// The both-rails rule: a record on this rail beside money elsewhere (Lightning paid, the peer paid or holding money).
func TestBothRails(t *testing.T) {
	rec := []Out{{Amount: 5, Counted: true, Recorded: true, Code: CodePaid}}
	late := []Out{{Amount: 5, Recorded: true, Code: CodeLate}}
	none := []Out{{Amount: 5, Counted: true, Marked: true, Kind: "mempool"}}
	for name, tc := range map[string]struct {
		outs   []Out
		lnPaid bool
		peer   PeerFacts
		want   bool
	}{
		"a record, nothing elsewhere":        {rec, false, PeerFacts{}, false},
		"a record, Lightning paid":           {rec, true, PeerFacts{}, true},
		"a record, the peer paid":            {rec, false, PeerFacts{Paid: true}, true},
		"a record, money on the peer":        {rec, false, PeerFacts{Money: true}, true},
		"a late record, money on the peer":   {late, false, PeerFacts{Money: true}, true},
		"the peer pending only counts money": {rec, false, PeerFacts{Pending: true}, false},
		"no record, Lightning paid":          {none, true, PeerFacts{Paid: true, Money: true}, false},
	} {
		if got := BothRails(tc.outs, tc.lnPaid, tc.peer); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
		if MoneyElsewhere(tc.lnPaid, tc.peer) != (tc.lnPaid || tc.peer.Paid || tc.peer.Money) {
			t.Errorf("%s: MoneyElsewhere", name)
		}
	}
}

// The paid-again ALERT names the rail whose money makes the order paid: the rail's own first, then the peer, then
// Lightning.
func TestPaidAgainLine(t *testing.T) {
	for _, tc := range []struct {
		own, peer bool
		want      string
	}{
		{true, true, LogPaidAgainMonero},
		{true, false, LogPaidAgainMonero},
		{false, true, LogPaidAgainOnchain},
		{false, false, LogPaidAgainLightning},
	} {
		if got := PaidAgainLine(tc.own, tc.peer, LogPaidAgainMonero, LogPaidAgainOnchain); got != tc.want {
			t.Errorf("own %v peer %v: %q", tc.own, tc.peer, got)
		}
	}
}
