package onchain

import (
	"testing"

	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

// A mempool that is still loading across the end of an order's watch window: the on-time output cannot be judged, so
// the order's final event waits (heldInPass) and no "no final event" ALERT is logged; once the mempool has loaded the
// event comes. TestHeldAtTheWindowCloseIsNoFalseNoFinal (watch_test.go, the case "next pass fails true") pins that an
// order that really has no final event still logs the ALERT.
func TestMempoolLoadingAtTheWindowEndHoldsTheFinalEvent(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	a := e.payOrder(o, 100_000)
	e.pass()
	e.node.Update(func(s *onchaintest.State) { s.MempoolLoading = true })
	e.node.Evict(a)
	e.pastExpiry(o)
	e.passes(2)
	e.toWindowEnd(o)
	e.passes(3)
	if e.count(logNoFinal) != 0 || len(e.events(o)) != 0 || e.count(logLoading) == 0 {
		t.Fatalf("while loading: events %+v\n%s", e.events(o), e.log)
	}
	e.node.Update(func(s *onchaintest.State) { s.MempoolLoading = false })
	e.passes(4)
	if e.count(logNoFinal) != 0 || len(e.events(o)) != 1 || e.eventCount(o, store.EventExpired) != 1 {
		t.Fatalf("after the load: events %+v\n%s", e.events(o), e.log)
	}
	e.safety(o)
}

// A late output (first seen at or after the expiry) that returns errLoading holds nothing.
func TestMempoolLoadingLateOutputHoldsNothing(t *testing.T) {
	e := newWatchEnv(t)
	o := e.order(100_000)
	addr := e.addr(o)
	e.pastExpiry(o)
	late := e.node.Pay(addr, 1_000)
	e.passes(1)
	if x := e.txo(late); x.FirstSeen.Before(o.ExpiresAt) {
		t.Fatalf("setup: not late: %v", x.FirstSeen)
	}
	e.node.Update(func(s *onchaintest.State) { s.MempoolLoading = true })
	e.node.Evict(late)
	for i := 0; i < 3; i++ {
		e.passes(1)
		if _, held := e.svc.heldInPass[o.ID]; held {
			t.Fatalf("pass %d: a late output holds the final event", i)
		}
	}
	if e.count(logLoading) == 0 {
		t.Fatalf("setup: never loading\n%s", e.log)
	}
}
