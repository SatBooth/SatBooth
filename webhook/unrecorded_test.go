package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
)

// holdAttempts makes every insert into webhook_attempts fail, as a store error would, until release is called. It
// uses its own connection and a trigger, so reads still work.
func (f *fixture) holdAttempts() (release func()) {
	f.t.Helper()
	raw, err := sql.Open("sqlite3", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER hold_attempts BEFORE INSERT ON webhook_attempts BEGIN SELECT RAISE(ABORT, 'held'); END`); err != nil {
		f.t.Fatal(err)
	}
	return func() {
		if _, err := raw.Exec(`DROP TRIGGER hold_attempts`); err != nil {
			f.t.Fatal(err)
		}
		raw.Close()
	}
}

func bodyID(t *testing.T, r received) string {
	t.Helper()
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// A 2xx whose record fails is posted once: the later passes retry the record only, and it is made with the time of the
// post when the store works again.
func TestUnrecordedTwoHundredIsNotPostedAgainByTheSameProcess(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	e := f.event(store.EventPaid)
	d := f.deliverer(s.srv.URL, nil)
	release := f.holdAttempts()
	if err := d.RunOnce(context.Background()); err == nil {
		t.Fatal("a pass whose record failed returned no error")
	}
	postedAt := f.clk.now()
	for i := 0; i < 4; i++ {
		f.clk.advance(time.Minute)
		if err := d.RunOnce(context.Background()); err == nil {
			t.Fatal("a pass whose record failed again returned no error")
		}
	}
	if n := len(s.requests()); n != 1 {
		t.Fatalf("%d posts while the store failed, want 1", n)
	}
	release()
	f.clk.advance(time.Minute)
	pass(t, d)
	if m := len(d.unrecorded); m != 0 { // at once, not at the next pass's listing
		t.Fatalf("%d entries remembered right after the record succeeded, want 0", m)
	}
	pass(t, d)
	if n := len(s.requests()); n != 1 {
		t.Fatalf("%d posts after the store came back, want 1", n)
	}
	a := f.attempts(e)
	if len(a) != 1 || !a[0].OK || a[0].Status != 200 || !a[0].At.Equal(postedAt) {
		t.Fatalf("attempts %+v, want one 200 made at the time of the post (%s)", a, postedAt)
	}
	if m := len(d.unrecorded); m != 0 {
		t.Fatalf("%d entries remembered after the record succeeded, want 0", m)
	}
}

// After a restart the memory is gone, so the event is posted again with the same id: the documented at-least-once.
func TestUnrecordedTwoHundredIsPostedAgainAfterARestart(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	e := f.event(store.EventPaid)
	release := f.holdAttempts()
	if err := f.deliverer(s.srv.URL, nil).RunOnce(context.Background()); err == nil {
		t.Fatal("no error from the failed record")
	}
	release()
	pass(t, f.deliverer(s.srv.URL, nil))
	got := s.requests()
	if len(got) != 2 || bodyID(t, got[0]) != e.ID || bodyID(t, got[1]) != e.ID {
		t.Fatalf("%d posts, want 2 with the same event id %s", len(got), e.ID)
	}
	if a := f.attempts(e); len(a) != 1 || !a[0].OK {
		t.Fatalf("attempts %+v", a)
	}
}

// A failed answer is not remembered: with the record failing, each pass posts again, as before.
func TestUnrecordedFailureIsNotRemembered(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500)
	f.event(store.EventPaid)
	d := f.deliverer(s.srv.URL, nil)
	release := f.holdAttempts()
	defer release()
	for i := 0; i < 3; i++ {
		if err := d.RunOnce(context.Background()); err == nil {
			t.Fatal("no error from the failed record")
		}
	}
	if n := len(s.requests()); n != 3 {
		t.Fatalf("%d posts, want one per pass (3)", n)
	}
}

// An entry is dropped when its event is no longer in the endpoint's listing (delivered by another process here, or past
// the scan window), never by the event's age alone: the pass posts nothing for it.
func TestUnrecordedEntryIsDroppedWhenItsEventLeavesTheListing(t *testing.T) {
	// Delivered by another route: the store records an OK attempt behind the deliverer's back.
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	e := f.event(store.EventPaid)
	d := f.deliverer(s.srv.URL, nil)
	release := f.holdAttempts()
	_ = d.RunOnce(context.Background())
	if n, m := len(s.requests()), len(d.unrecorded); n != 1 || m != 1 {
		t.Fatalf("%d posts, %d entries, want 1 and 1", n, m)
	}
	release()
	if err := f.st.AddAttempt(context.Background(), store.Attempt{Endpoint: store.MainEndpoint, EventID: e.ID, At: f.clk.now(), Status: 200, OK: true}); err != nil {
		t.Fatal(err)
	}
	pass(t, d)
	if n, m := len(s.requests()), len(d.unrecorded); n != 1 || m != 0 {
		t.Fatalf("after the event left the listing: %d posts, %d entries, want 1 and 0", n, m)
	}

	// Past the scan window (24 hours plus 7 days): not listed, so dropped, and nothing is posted.
	f = newFixture(t)
	s = newSink(t, f.clk, 200)
	f.event(store.EventPaid)
	d = f.deliverer(s.srv.URL, nil)
	release = f.holdAttempts()
	defer release()
	_ = d.RunOnce(context.Background())
	f.clk.advance(RetryWindow + scanMargin + time.Minute)
	_ = d.RunOnce(context.Background())
	if n, m := len(s.requests()), len(d.unrecorded); n != 1 || m != 0 {
		t.Fatalf("past the scan window: %d posts, %d entries, want 1 and 0", n, m)
	}
}

// A 2xx on the last attempt (at the 24-hour limit) whose record fails is not posted again, and the event is not marked
// abandoned: the record is made when the store works again.
func TestUnrecordedTwoHundredAtTheLimitIsNotPostedAgainNorAbandoned(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500, 200, 500)
	e := f.event(store.EventPaid)
	d := f.deliverer(s.srv.URL, nil)
	pass(t, d)
	f.clk.advance(RetryWindow)
	release := f.holdAttempts()
	for i := 0; i < 4; i++ {
		_ = d.RunOnce(context.Background())
		f.clk.advance(time.Second)
	}
	if n := len(s.requests()); n != 2 {
		t.Fatalf("%d posts while the store failed, want 2 (the 500 and the 200)", n)
	}
	release()
	pass(t, d)
	pass(t, d)
	if n := len(s.requests()); n != 2 {
		t.Fatalf("%d posts after the store came back, want 2", n)
	}
	a := f.attempts(e)
	if len(a) != 2 || a[1].OK != true || a[1].Status != 200 {
		t.Fatalf("attempts %+v, want the 500 then the 200", a)
	}
	if ids, _ := f.st.PendingAbandoned(context.Background(), store.MainEndpoint); len(ids) != 0 || f.log.count(GiveUpMessage) != 0 {
		t.Fatalf("an event the receiver accepted is marked abandoned: %v, %d lines", ids, f.log.count(GiveUpMessage))
	}
}

// The same for an event whose first attempt comes after more than 24 hours of downtime.
func TestUnrecordedTwoHundredAfterLongDowntimeIsNotPostedAgainNorAbandoned(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200, 500)
	d := f.deliverer(s.srv.URL, nil)
	pass(t, d) // registers the endpoint
	e := f.event(store.EventPaid)
	f.clk.advance(25 * time.Hour)
	release := f.holdAttempts()
	for i := 0; i < 4; i++ {
		_ = d.RunOnce(context.Background())
		f.clk.advance(time.Second)
	}
	if n := len(s.requests()); n != 1 {
		t.Fatalf("%d posts while the store failed, want 1", n)
	}
	release()
	pass(t, d)
	if n := len(s.requests()); n != 1 {
		t.Fatalf("%d posts after the store came back, want 1", n)
	}
	if a := f.attempts(e); len(a) != 1 || !a[0].OK {
		t.Fatalf("attempts %+v", a)
	}
	if ids, _ := f.st.PendingAbandoned(context.Background(), store.MainEndpoint); len(ids) != 0 || f.log.count(GiveUpMessage) != 0 {
		t.Fatalf("marked abandoned: %v, %d lines", ids, f.log.count(GiveUpMessage))
	}
}

// A 2xx just before the limit whose record can only be made after it: no second post, and so no later failure that
// would mark the event abandoned.
func TestUnrecordedTwoHundredBeforeTheLimitIsRecordedAfterItWithoutAFalseGiveUp(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500, 200, 500)
	e := f.event(store.EventPaid)
	d := f.deliverer(s.srv.URL, nil)
	pass(t, d)
	f.clk.advance(RetryWindow - time.Minute)
	release := f.holdAttempts()
	_ = d.RunOnce(context.Background())
	f.clk.advance(2 * time.Minute) // the limit has passed
	_ = d.RunOnce(context.Background())
	release()
	pass(t, d)
	pass(t, d)
	if n := len(s.requests()); n != 2 {
		t.Fatalf("%d posts, want 2", n)
	}
	if a := f.attempts(e); len(a) != 2 || !a[1].OK {
		t.Fatalf("attempts %+v, want the 500 then the recorded 200", a)
	}
	if ids, _ := f.st.PendingAbandoned(context.Background(), store.MainEndpoint); len(ids) != 0 || f.log.count(GiveUpMessage) != 0 {
		t.Fatalf("a false give-up: %v, %d lines", ids, f.log.count(GiveUpMessage))
	}
}
