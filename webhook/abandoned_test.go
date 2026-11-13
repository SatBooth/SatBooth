package webhook

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
)

// crashHandler is a slog.Handler that stands for a crash: when armed, it panics instead of writing the give-up line,
// as a process killed between recording the last attempt and logging would never write it. Every other line goes to
// the wrapped handler.
type crashHandler struct {
	slog.Handler
	armed *bool
}

func (h crashHandler) Handle(ctx context.Context, r slog.Record) error {
	if *h.armed && r.Message == GiveUpMessage {
		panic("crash before the give-up line")
	}
	return h.Handler.Handle(ctx, r)
}

func (h crashHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return crashHandler{h.Handler.WithAttrs(a), h.armed}
}

func (h crashHandler) WithGroup(n string) slog.Handler {
	return crashHandler{h.Handler.WithGroup(n), h.armed}
}

// crashingDeliverers builds the deliverers of the named endpoints (all posting to url) whose give-up line panics while
// *armed is true.
func (f *fixture) crashingDeliverers(url string, armed *bool, names ...string) []*Deliverer {
	f.t.Helper()
	eps := make([]Endpoint, len(names))
	for i, n := range names {
		eps[i] = Endpoint{Name: n, URL: url, Secret: secretA}
	}
	ds, err := NewEventDeliverers(eps, f.st, f.clk.now, nil, slog.New(crashHandler{slog.NewTextHandler(f.log, nil), armed}))
	if err != nil {
		f.t.Fatal(err)
	}
	return ds
}

// crashingPass runs one pass and reports whether the give-up line "crashed" it.
func crashingPass(t *testing.T, d *Deliverer) (crashed bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			crashed = true
		}
	}()
	pass(t, d)
	return false
}

// The last attempt is recorded, then the process dies before it logs the line; after the restart the line is logged,
// once, and the event is not sent again.
func TestAbandonedLineSurvivesACrashBeforeIt(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500)
	e := f.event(store.EventPaid)
	armed := false
	d := f.crashingDeliverers(s.srv.URL, &armed, store.MainEndpoint)[0]
	pass(t, d)
	f.clk.advance(RetryWindow)
	armed = true
	if !crashingPass(t, d) {
		t.Fatal("the pass at the deadline did not reach the give-up line")
	}
	if a := f.attempts(e); len(a) != 2 || a[1].OK || !a[1].At.Equal(t0.Add(RetryWindow)) {
		t.Fatalf("the last attempt is not recorded: %+v", a)
	}
	if n := f.log.count(GiveUpMessage); n != 0 {
		t.Fatalf("the line was logged by the crashed process: %d", n)
	}
	posts := len(s.requests())
	// The restart: a new deliverer, no memory of the crash.
	armed = false
	d = f.crashingDeliverers(s.srv.URL, &armed, store.MainEndpoint)[0]
	for i := 0; i < 3; i++ {
		pass(t, d)
		f.clk.advance(time.Hour)
	}
	if n := giveUpLines(f.log, store.MainEndpoint, e); n != 1 {
		t.Fatalf("%d give-up lines after the restart, want 1\n%s", n, f.log.String())
	}
	if got := len(s.requests()); got != posts {
		t.Fatalf("%d posts after the restart, want none more than the %d before", got, posts)
	}
	// Another restart says nothing more.
	pass(t, f.crashingDeliverers(s.srv.URL, &armed, store.MainEndpoint)[0])
	if n := giveUpLines(f.log, store.MainEndpoint, e); n != 1 {
		t.Fatalf("%d give-up lines after a second restart, want 1", n)
	}
}

// abandonBoth drives the endpoints "a" and "b" to the abandonment of one event, each crashing at its line.
func abandonBoth(t *testing.T, f *fixture, s *sink, armed *bool) store.Event {
	t.Helper()
	e := f.event(store.EventPaid)
	ds := f.crashingDeliverers(s.srv.URL, armed, "a", "b")
	for _, d := range ds {
		pass(t, d)
	}
	f.clk.advance(RetryWindow)
	*armed = true
	for _, d := range ds {
		if !crashingPass(t, d) {
			t.Fatal("a pass at the deadline did not reach the give-up line")
		}
	}
	*armed = false
	if n := f.log.count(GiveUpMessage); n != 0 {
		t.Fatalf("%d lines written by the crashed process", n)
	}
	return e
}

// Two endpoints abandon the same event, both crash: after the restart each logs its own line, once.
func TestAbandonedLineOfTwoEndpointsAfterACrash(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500)
	armed := false
	e := abandonBoth(t, f, s, &armed)
	for i := 0; i < 2; i++ {
		for _, d := range f.crashingDeliverers(s.srv.URL, &armed, "a", "b") {
			pass(t, d)
		}
	}
	for _, name := range []string{"a", "b"} {
		if n := giveUpLines(f.log, name, e); n != 1 {
			t.Fatalf("%d lines for %s, want 1\n%s", n, name, f.log.String())
		}
	}
	if n := f.log.count(GiveUpMessage); n != 2 {
		t.Fatalf("%d lines in all, want 2", n)
	}
}

// An endpoint removed from the configuration before its line was announced says nothing while it is gone, and its mark
// stays in the store. The line is logged when the endpoint is configured again (a name that comes back picks up its
// own record). The other endpoint's line is not held up.
func TestAbandonedLineOfARemovedEndpointWaitsForItsReturn(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500)
	armed := false
	e := abandonBoth(t, f, s, &armed)
	pass(t, f.crashingDeliverers(s.srv.URL, &armed, "a")[0]) // restart with only a
	if giveUpLines(f.log, "a", e) != 1 || giveUpLines(f.log, "b", e) != 0 {
		t.Fatalf("after the restart without b:\n%s", f.log.String())
	}
	ids, err := f.st.PendingAbandoned(context.Background(), "b")
	if err != nil || len(ids) != 1 || ids[0] != e.ID {
		t.Fatalf("b's mark: %v %v", ids, err)
	}
	if ids, _ := f.st.PendingAbandoned(context.Background(), "a"); len(ids) != 0 {
		t.Fatalf("a's mark stays: %v", ids)
	}
	f.clk.advance(30 * 24 * time.Hour) // long after: the mark does not age out
	pass(t, f.crashingDeliverers(s.srv.URL, &armed, "a", "b")[1])
	if giveUpLines(f.log, "b", e) != 1 || giveUpLines(f.log, "a", e) != 1 {
		t.Fatalf("after b returned:\n%s", f.log.String())
	}
	if ids, _ := f.st.PendingAbandoned(context.Background(), "b"); len(ids) != 0 {
		t.Fatalf("b's mark stays: %v", ids)
	}
}

// If removing the mark fails, the line is not logged again by the same process; a restart logs it again (a duplicate,
// never a loss); once the store works the mark is gone.
func TestAbandonedLineIsLoggedOncePerRunWhenTheMarkCannotBeRemoved(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500)
	e := f.event(store.EventPaid)
	d := f.deliverer(s.srv.URL, nil)
	pass(t, d)
	f.clk.advance(RetryWindow)
	raw, err := sql.Open("sqlite3", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER hold_clear BEFORE DELETE ON webhook_abandoned BEGIN SELECT RAISE(ABORT, 'held'); END`); err != nil {
		t.Fatal(err)
	}
	// The failing clear does not stop the endpoint's deliveries: in this pass the first event's last attempt is made and
	// an event stored now is posted after it.
	e2 := f.event(store.EventExpired)
	before := len(s.requests())
	pass(t, d)
	if n := len(s.requests()); n != before+2 || len(f.attempts(e2)) != 1 {
		t.Fatalf("%d posts, want %d: the failed clear stopped the pass", n, before+2)
	}
	if n := f.log.count(ClearFailedMessage); n != 1 {
		t.Fatalf("%d clear-failed lines, want 1\n%s", n, f.log.String())
	}
	for _, l := range strings.Split(f.log.String(), "\n") {
		if strings.Contains(l, ClearFailedMessage) && (strings.Contains(l, "held") || strings.Contains(l, "err")) {
			t.Fatalf("the line carries data from the error: %s", l)
		}
	}
	for i := 0; i < 3; i++ {
		pass(t, d)
	}
	if n := giveUpLines(f.log, store.MainEndpoint, e); n != 1 {
		t.Fatalf("%d lines in one run, want 1", n)
	}
	_ = f.deliverer(s.srv.URL, nil).RunOnce(context.Background()) // a restart
	if n := giveUpLines(f.log, store.MainEndpoint, e); n != 2 {
		t.Fatalf("%d lines after the restart, want 2 (a duplicate is allowed)", n)
	}
	if _, err := raw.Exec(`DROP TRIGGER hold_clear`); err != nil {
		t.Fatal(err)
	}
	pass(t, d)
	pass(t, d)
	if n := giveUpLines(f.log, store.MainEndpoint, e); n != 2 {
		t.Fatalf("%d lines after the store came back, want 2", n)
	}
	if ids, _ := f.st.PendingAbandoned(context.Background(), store.MainEndpoint); len(ids) != 0 {
		t.Fatalf("the mark stays: %v", ids)
	}
	if len(d.announced) != 0 {
		t.Fatalf("%d ids remembered", len(d.announced))
	}
}
