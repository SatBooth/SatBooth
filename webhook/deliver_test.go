package webhook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"

	_ "github.com/ncruces/go-sqlite3/driver"
)

var t0 = time.Unix(1_800_000_000, 0).UTC()

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var testSecret = bytes.Repeat([]byte{0x5a}, 32)

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuf) count(sub string) int { return strings.Count(l.String(), sub) }

type received struct {
	at     time.Time
	header string
	body   []byte
	ctype  string
}

// sink is a receiver whose answers are scripted: statuses[i] answers the i-th request (the last
// one repeats). It records each request with the fake clock's time.
type sink struct {
	t        *testing.T
	clk      *clock
	srv      *httptest.Server
	mu       sync.Mutex
	statuses []int
	got      []received
	hang     chan struct{}
}

func newSink(t *testing.T, clk *clock, statuses ...int) *sink {
	s := &sink{t: t, clk: clk, statuses: statuses, hang: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		i := len(s.got)
		s.got = append(s.got, received{at: clk.now(), header: r.Header.Get("SatBooth-Signature"), body: body, ctype: r.Header.Get("Content-Type")})
		st := s.statuses[min(i, len(s.statuses)-1)]
		s.mu.Unlock()
		if st < 0 { // hang until the test ends
			select {
			case <-s.hang:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(st)
		w.Write([]byte("ok"))
	}))
	t.Cleanup(func() { close(s.hang); s.srv.Close() })
	return s
}

func (s *sink) requests() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received(nil), s.got...)
}

type fixture struct {
	t    *testing.T
	st   *store.Store
	path string
	clk  *clock
	log  *logBuf
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := &clock{t: t0}
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := store.Open(context.Background(), path, store.WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &fixture{t: t, st: st, path: path, clk: clk, log: &logBuf{}}
}

func (f *fixture) deliverer(url string, client *http.Client) *Deliverer {
	f.t.Helper()
	d, err := mainDeliverer(url, testSecret, f.st, f.clk.now, client, slog.New(slog.NewTextHandler(f.log, nil)))
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

// orderN numbers the orders event creates, across tests and -count runs, so each has its own reference. It is written
// in decimal: a reference made by repeating a letter outgrew the store's 64 characters after 60 orders (-count=5).
var orderN int

// event creates an order and its order.paid event, stamped by the fixture clock.
func (f *fixture) event(typ string) store.Event {
	f.t.Helper()
	orderN++
	o, _, err := f.st.CreateOrder(context.Background(), store.Order{AccountID: "acct-1", MerchantRef: fmt.Sprintf("ref-%d", orderN), AmountMsat: 1_000_000, ExpiresAt: f.clk.now().Add(time.Hour)})
	if err != nil {
		f.t.Fatal(err)
	}
	e := store.Event{Type: typ, OrderID: o.ID}
	added, err := f.st.AddEvent(context.Background(), e)
	if err != nil || !added {
		f.t.Fatalf("AddEvent: %v %v", added, err)
	}
	e.ID = store.EventID(typ, o.ID, 1)
	e.CreatedAt = f.clk.now()
	return e
}

func (f *fixture) attempts(e store.Event) []store.Attempt {
	f.t.Helper()
	a, err := f.st.AttemptsFor(context.Background(), store.MainEndpoint, e.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func pass(t *testing.T, d *Deliverer) {
	t.Helper()
	if err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDeliversSignedEventOnce(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	d := f.deliverer(s.srv.URL, nil)
	paid := f.event(store.EventPaid)
	exp := f.event(store.EventExpired)
	pass(t, d)
	got := s.requests()
	if len(got) != 2 {
		t.Fatalf("%d requests, want 2", len(got))
	}
	byOrder := map[string]received{}
	for _, r := range got {
		var m struct {
			OrderID string `json:"order_id"`
		}
		if err := json.Unmarshal(r.body, &m); err != nil {
			t.Fatal(err)
		}
		byOrder[m.OrderID] = r
	}
	for _, e := range []store.Event{paid, exp} {
		r, found := byOrder[e.OrderID]
		if !found {
			t.Fatalf("no request for %s", e.Type)
		}
		var m map[string]any
		if err := json.Unmarshal(r.body, &m); err != nil {
			t.Fatal(err)
		}
		want := `{"id":"` + e.ID + `","type":"` + e.Type + `","seq":1,"order_id":"` + e.OrderID + `","created_at":1800000000}`
		if string(r.body) != want {
			t.Fatalf("body %s, want %s", r.body, want)
		}
		if len(m) != 5 {
			t.Fatalf("body has %d fields", len(m))
		}
		if err := Verify(testSecret, r.header, r.body, f.clk.now(), 5*time.Minute); err != nil {
			t.Fatalf("signature: %v", err)
		}
		if r.ctype != "application/json" {
			t.Fatalf("content type %q", r.ctype)
		}
		a := f.attempts(e)
		if len(a) != 1 || !a[0].OK || a[0].Status != 200 || !a[0].At.Equal(t0) {
			t.Fatalf("attempts %+v", a)
		}
	}
	// Never again after a 2xx, however much time passes or however often it runs.
	for _, adv := range []time.Duration{0, time.Second, time.Hour, 30 * time.Hour} {
		f.clk.advance(adv)
		pass(t, d)
		pass(t, f.deliverer(s.srv.URL, nil))
	}
	if n := len(s.requests()); n != 2 {
		t.Fatalf("%d requests after 2xx, want 2", n)
	}
	if strings.Contains(f.log.String(), hex.EncodeToString(testSecret)) {
		t.Fatal("secret in the log")
	}
}

// Every event type carries seq, and the repeating ones carry their own: the receiver reads order.reverted 2 and
// order.paid_again 2 from the body, in whatever order they arrive.
func TestBodyCarriesSeqForEveryType(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	d := f.deliverer(s.srv.URL, nil)
	ctx := context.Background()
	// One order per final type: the store adds one order.paid, order.expired or order.underpaid per order (a second
	// final event is not added), so the paid order carries the repeating types and order.late, and the other two
	// final types have an order each.
	var orders []store.Order
	for _, ref := range []string{"seq", "seq-expired", "seq-underpaid"} {
		o, _, err := f.st.CreateOrder(ctx, store.Order{AccountID: "acct-1", MerchantRef: ref, AmountMsat: 1_000_000, ExpiresAt: f.clk.now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		orders = append(orders, o)
	}
	type ev struct {
		typ   string
		seq   int
		order int
	}
	want := []ev{{store.EventPaid, 1, 0}, {store.EventReverted, 1, 0}, {store.EventPaidAgain, 1, 0}, {store.EventReverted, 2, 0},
		{store.EventPaidAgain, 2, 0}, {store.EventExpired, 1, 1}, {store.EventUnderpaid, 1, 2}, {store.EventLate, 1, 0}}
	for _, e := range want {
		if added, err := f.st.AddEvent(ctx, store.Event{Type: e.typ, OrderID: orders[e.order].ID, Seq: e.seq}); err != nil || !added {
			t.Fatalf("AddEvent %v: %v %v", e, added, err)
		}
	}
	ofType := map[string]string{store.EventExpired: orders[1].ID, store.EventUnderpaid: orders[2].ID}
	pass(t, d)
	got := map[string]int{}
	for _, r := range s.requests() {
		var m struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Seq     *int   `json:"seq"`
			OrderID string `json:"order_id"`
		}
		if err := json.Unmarshal(r.body, &m); err != nil || m.Seq == nil {
			t.Fatalf("body %s: %v", r.body, err)
		}
		wantOrder, ok := ofType[m.Type]
		if !ok {
			wantOrder = orders[0].ID
		}
		if m.OrderID != wantOrder {
			t.Fatalf("body %s: order %s, want %s", r.body, m.OrderID, wantOrder)
		}
		if !strings.HasPrefix(string(r.body), `{"id":"`+m.ID+`","type":"`+m.Type+`","seq":`) {
			t.Fatalf("seq is not right after type: %s", r.body)
		}
		if m.ID != store.EventID(m.Type, m.OrderID, *m.Seq) {
			t.Fatalf("seq %d does not match the event id: %s", *m.Seq, r.body)
		}
		if err := Verify(testSecret, r.header, r.body, f.clk.now(), 5*time.Minute); err != nil {
			t.Fatalf("signature: %v", err)
		}
		got[fmt.Sprintf("%s/%d", m.Type, *m.Seq)]++
	}
	for _, e := range want {
		if k := fmt.Sprintf("%s/%d", e.typ, e.seq); got[k] != 1 {
			t.Errorf("%s delivered %d times", k, got[k])
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %v", got)
	}
}

func TestAnyTwoHundredSeriesIsDone(t *testing.T) {
	for _, st := range []int{200, 201, 202, 204, 299} {
		f := newFixture(t)
		s := newSink(t, f.clk, st)
		e := f.event(store.EventPaid)
		pass(t, f.deliverer(s.srv.URL, nil))
		if a := f.attempts(e); len(a) != 1 || !a[0].OK || a[0].Status != st {
			t.Fatalf("status %d: %+v", st, a)
		}
	}
	for _, st := range []int{300, 301, 400, 404, 410, 429, 500, 503} {
		f := newFixture(t)
		s := newSink(t, f.clk, st)
		e := f.event(store.EventPaid)
		pass(t, f.deliverer(s.srv.URL, nil))
		if a := f.attempts(e); len(a) != 1 || a[0].OK || a[0].Status != st {
			t.Fatalf("status %d: %+v", st, a)
		}
	}
}

func TestRetriesAfterAServerError(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500, 200)
	d := f.deliverer(s.srv.URL, nil)
	e := f.event(store.EventPaid)
	pass(t, d)
	if n := len(s.requests()); n != 1 {
		t.Fatalf("%d requests", n)
	}
	f.clk.advance(9 * time.Second)
	pass(t, d)
	if n := len(s.requests()); n != 1 {
		t.Fatalf("retried after 9 s: %d requests", n)
	}
	f.clk.advance(time.Second)
	pass(t, d)
	got := s.requests()
	if len(got) != 2 || !got[1].at.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("requests %+v", got)
	}
	a := f.attempts(e)
	if len(a) != 2 || a[0].OK || a[0].Status != 500 || !a[1].OK || a[1].Status != 200 {
		t.Fatalf("attempts %+v", a)
	}
	// Each retry carries a fresh timestamp in its signature.
	if got[0].header == got[1].header {
		t.Fatal("the retry reused the first signature")
	}
	if err := Verify(testSecret, got[1].header, got[1].body, t0.Add(10*time.Second), time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestBackoffTimings(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 503)
	d := f.deliverer(s.srv.URL, nil)
	f.event(store.EventPaid)
	// Step the clock 10 s at a time (every delay is a multiple of 10 s); record when requests arrive.
	end := t0.Add(16000 * time.Second)
	for f.clk.now().Before(end) {
		pass(t, d)
		f.clk.advance(10 * time.Second)
	}
	var offsets []time.Duration
	for _, r := range s.requests() {
		offsets = append(offsets, r.at.Sub(t0))
	}
	want := []time.Duration{0, 10 * time.Second, 30 * time.Second, 70 * time.Second, 150 * time.Second, 310 * time.Second,
		630 * time.Second, 1270 * time.Second, 2550 * time.Second, 5110 * time.Second, 5110*time.Second + time.Hour}
	for i := range want {
		if i >= len(offsets) || offsets[i] != want[i] {
			t.Fatalf("offsets %v, want prefix %v", offsets, want)
		}
	}
	// Hourly after the cap: 5110 s + 1 h = 8710 s, then 12310 s, 15910 s.
	if len(offsets) != 13 || offsets[11] != 12310*time.Second || offsets[12] != 15910*time.Second {
		t.Fatalf("offsets %v", offsets)
	}
}

func TestRetryDelay(t *testing.T) {
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second,
		320 * time.Second, 640 * time.Second, 1280 * time.Second, 2560 * time.Second, time.Hour, time.Hour}
	for i, w := range want {
		if got := RetryDelay(i + 1); got != w {
			t.Errorf("RetryDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
	for _, n := range []int{0, -1, 62, 63, 64, 65, 1000, 1 << 30} {
		if got := RetryDelay(n); got < 10*time.Second || got > time.Hour {
			t.Errorf("RetryDelay(%d) = %v out of [10 s, 1 h]", n, got)
		}
	}
	prev := time.Duration(0)
	for n := 1; n < 200; n++ {
		d := RetryDelay(n)
		if d < prev {
			t.Fatalf("RetryDelay decreases at %d", n)
		}
		prev = d
	}
	if RetryWindow != 24*time.Hour {
		t.Fatal("RetryWindow")
	}
}

func TestTimeoutIsAFailureAndRetried(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, -1, 200)
	e := f.event(store.EventPaid)
	d := f.deliverer(s.srv.URL, &http.Client{Timeout: 100 * time.Millisecond})
	pass(t, d)
	a := f.attempts(e)
	if len(a) != 1 || a[0].OK || a[0].Status != 0 {
		t.Fatalf("attempts %+v", a)
	}
	f.clk.advance(10 * time.Second)
	pass(t, d)
	a = f.attempts(e)
	if len(a) != 2 || !a[1].OK {
		t.Fatalf("attempts %+v", a)
	}
}

func TestTimeoutIsCappedAtTenSeconds(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	for _, c := range []*http.Client{nil, {}, {Timeout: time.Hour}} {
		if d := f.deliverer(s.srv.URL, c); d.client.Timeout != 10*time.Second {
			t.Errorf("timeout %v for %+v", d.client.Timeout, c)
		}
	}
	if d := f.deliverer(s.srv.URL, &http.Client{Timeout: time.Second}); d.client.Timeout != time.Second {
		t.Error("a shorter timeout must be kept")
	}
}

func TestRedirectIsNotFollowedAndIsAFailure(t *testing.T) {
	f := newFixture(t)
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	for _, code := range []int{301, 302, 303, 307, 308} {
		redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, code)
		}))
		e := f.event(store.EventPaid)
		pass(t, f.deliverer(redir.URL, nil))
		redir.Close()
		a := f.attempts(e)
		if len(a) != 1 || a[0].OK || a[0].Status != code {
			t.Fatalf("code %d: attempts %+v", code, a)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("a redirect was followed")
	}
	// A caller's client with its own redirect policy does not get to follow them either.
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redir.Close()
	e := f.event(store.EventPaid)
	pass(t, f.deliverer(redir.URL, &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}))
	if hits.Load() != 0 || f.attempts(e)[0].OK {
		t.Fatal("a redirect was followed with the caller's client")
	}
}

type countingBody struct {
	n *atomic.Int64
}

func (b countingBody) Read(p []byte) (int, error) { b.n.Add(int64(len(p))); return len(p), nil }

type closeSpy struct {
	io.Reader
	closed *atomic.Bool
}

func (c closeSpy) Close() error { c.closed.Store(true); return nil }

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReadsAtMost4KiBAndClosesTheBody(t *testing.T) {
	f := newFixture(t)
	var read atomic.Int64
	var closed atomic.Bool
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{}, Request: r,
			Body: closeSpy{Reader: countingBody{&read}, closed: &closed}}, nil
	})}
	e := f.event(store.EventPaid)
	pass(t, f.deliverer("http://192.0.2.1/hook", client))
	if got := read.Load(); got > 4096 {
		t.Fatalf("read %d bytes of the response, want at most 4096", got)
	}
	if !closed.Load() {
		t.Fatal("body not closed")
	}
	if a := f.attempts(e); len(a) != 1 || !a[0].OK {
		t.Fatalf("attempts %+v", a)
	}
}

func TestGivesUpAfter24HoursWithOneLogLineAcrossRestarts(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500)
	e := f.event(store.EventPaid)
	d := f.deliverer(s.srv.URL, nil)
	for f.clk.now().Before(t0.Add(26 * time.Hour)) {
		pass(t, d)
		if f.clk.now().Sub(t0)%(37*time.Minute) == 0 { // a restart every so often
			d = f.deliverer(s.srv.URL, nil)
		}
		f.clk.advance(10 * time.Second)
	}
	// And a few restarts after the end.
	for i := 0; i < 3; i++ {
		d = f.deliverer(s.srv.URL, nil)
		pass(t, d)
		f.clk.advance(time.Hour)
		pass(t, d)
	}
	a := f.attempts(e)
	last := a[len(a)-1]
	if last.OK || !last.At.Equal(t0.Add(RetryWindow)) {
		t.Fatalf("last attempt %+v, want a failure at exactly +24 h", last)
	}
	for _, x := range a {
		if x.At.After(t0.Add(RetryWindow)) {
			t.Fatalf("attempt after the window: %+v", x)
		}
	}
	if n := f.log.count(GiveUpMessage); n != 1 {
		t.Fatalf("%d give-up lines, want 1\n%s", n, f.log.String())
	}
	// The give-up line is alertable like lnpay's: it starts with ALERT, and it is the only ALERT.
	if !strings.HasPrefix(GiveUpMessage, "ALERT: ") || f.log.count("ALERT") != 1 {
		t.Fatalf("give-up line not alertable:\n%s", f.log.String())
	}
	if !strings.Contains(f.log.String(), e.ID) {
		t.Fatal("the give-up line should name the event")
	}
}

func TestSuccessAtTheLastAttemptIsNotAGiveUp(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500)
	d := f.deliverer(s.srv.URL, nil)
	e := f.event(store.EventPaid)
	pass(t, d)
	f.clk.advance(RetryWindow - time.Second)
	pass(t, d) // some retry; whatever it is
	s.mu.Lock()
	s.statuses = []int{200}
	s.mu.Unlock()
	f.clk.advance(time.Second)
	pass(t, d)
	a := f.attempts(e)
	if !a[len(a)-1].OK || f.log.count(GiveUpMessage) != 0 {
		t.Fatalf("attempts %+v\nlog %s", a, f.log.String())
	}
}

func TestRestartResumesFromRecordedAttempts(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500)
	e := f.event(store.EventPaid)
	d := f.deliverer(s.srv.URL, nil)
	pass(t, d)                      // attempt 1 at +0
	f.clk.advance(10 * time.Second) // due
	pass(t, d)                      // attempt 2 at +10 s
	f.clk.advance(19 * time.Second) // +29 s: next due at +30 s
	f.st.Close()                    // restart: a new store on the same file, a new deliverer
	st2, err := store.Open(context.Background(), f.path, store.WithClock(f.clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	d2, err := mainDeliverer(s.srv.URL, testSecret, st2, f.clk.now, nil, slog.New(slog.NewTextHandler(f.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(s.requests()); n != 2 {
		t.Fatalf("%d requests after restart at +29 s, want 2 (no early retry)", n)
	}
	f.clk.advance(time.Second)
	if err := d2.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := s.requests()
	if len(got) != 3 || !got[2].at.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("requests %v", got)
	}
	a, err := st2.AttemptsFor(context.Background(), store.MainEndpoint, e.ID)
	if err != nil || len(a) != 3 {
		t.Fatalf("attempts %v %v", a, err)
	}
}

func TestRunLoopDeliversAndStops(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	d := f.deliverer(s.srv.URL, nil)
	WithIntervals(5*time.Millisecond, 0).apply(d)
	e := f.event(store.EventPaid)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(f.attempts(e)) == 0 { // wait on the condition, not a fixed time
		if time.Now().After(deadline) {
			t.Fatal("no delivery")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
	if n := len(s.requests()); n != 1 {
		t.Fatalf("%d requests", n)
	}
}

func TestShutdownMidRequestRecordsNothing(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, -1)
	e := f.event(store.EventPaid)
	d := f.deliverer(s.srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for len(s.requests()) == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	err := d.RunOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce: %v", err)
	}
	if a := f.attempts(e); len(a) != 0 {
		t.Fatalf("a cancelled request was recorded as an attempt: %+v", a)
	}
}

func TestMainDelivererValidation(t *testing.T) {
	f := newFixture(t)
	good := func(url string, secret []byte) error {
		_, err := mainDeliverer(url, secret, f.st, f.clk.now, nil, nil)
		return err
	}
	for _, u := range []string{"http://127.0.0.1:8080/hook", "https://192.0.2.7/a/b?x=1"} {
		if err := good(u, testSecret); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
	for _, u := range []string{"", "ftp://192.0.2.1/", "file:///etc/passwd", "//192.0.2.1/x", "192.0.2.1/x", "http://", "https:///path",
		"http://user@192.0.2.1/", "http://user:pass@192.0.2.1/", "http://:pw@192.0.2.1/", "http://192.0.2.1/x#frag", "javascript:alert(1)", "http://[::1", " http://192.0.2.1/", "http://192.0.2.1/\n"} {
		err := good(u, testSecret)
		if err == nil {
			t.Errorf("accepted %q", u)
			continue
		}
		if u != "" && strings.Contains(err.Error(), "pass") && strings.Contains(u, "pass") {
			t.Errorf("error echoes the url: %v", err)
		}
	}
	for _, n := range []int{0, 1, 31, 65, 100} {
		if err := good("http://127.0.0.1/", make([]byte, n)); err == nil {
			t.Errorf("accepted a %d-byte secret", n)
		}
	}
	for _, n := range []int{32, 33, 64} {
		if err := good("http://127.0.0.1/", make([]byte, n)); err != nil {
			t.Errorf("%d-byte secret: %v", n, err)
		}
	}
	if _, err := mainDeliverer("http://127.0.0.1/", testSecret, nil, f.clk.now, nil, nil); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := mainDeliverer("http://127.0.0.1/", testSecret, f.st, nil, nil, nil); err == nil {
		t.Error("nil clock accepted")
	}
}

func TestErrorsNeverEchoTheURLOrSecret(t *testing.T) {
	f := newFixture(t)
	secret := bytes.Repeat([]byte{0xab}, 40)
	_, err := mainDeliverer("http://u:test-password-1@192.0.2.1/", secret, f.st, f.clk.now, nil, nil)
	if err == nil || strings.Contains(err.Error(), "test-password-1") || strings.Contains(err.Error(), "192.0.2.1") {
		t.Fatalf("error %v", err)
	}
	_, err = mainDeliverer("http://192.0.2.1/", secret[:5], f.st, f.clk.now, nil, nil)
	if err == nil || strings.Contains(err.Error(), "abababab") {
		t.Fatalf("error %v", err)
	}
}

func writeSecretFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "webhook.secret")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSecret(t *testing.T) {
	h32 := strings.Repeat("ab", 32)
	h64 := strings.Repeat("cd", 64)
	for name, c := range map[string]struct {
		content string
		mode    os.FileMode
		ok      bool
		n       int
	}{
		"32 bytes":            {h32, 0o600, true, 32},
		"32 bytes newline":    {h32 + "\n", 0o600, true, 32},
		"64 bytes":            {h64, 0o600, true, 64},
		"48 bytes read-only":  {strings.Repeat("0f", 48), 0o400, true, 48},
		"31 bytes":            {strings.Repeat("ab", 31), 0o600, false, 0},
		"65 bytes":            {strings.Repeat("ab", 65), 0o600, false, 0},
		"empty":               {"", 0o600, false, 0},
		"odd length":          {h32 + "a", 0o600, false, 0},
		"uppercase":           {strings.ToUpper(h32), 0o600, false, 0},
		"not hex":             {strings.Repeat("zz", 32), 0o600, false, 0},
		"inner space":         {h32[:10] + " " + h32[10:], 0o600, false, 0},
		"two newlines":        {h32 + "\n\n", 0o600, false, 0},
		"group readable":      {h32, 0o640, false, 0},
		"world readable":      {h32, 0o644, false, 0},
		"group writable":      {h32, 0o620, false, 0},
		"leading whitespace":  {" " + h32, 0o600, false, 0},
		"windows line ending": {h32 + "\r\n", 0o600, false, 0},
		"huge file":           {strings.Repeat("ab", 100000), 0o600, false, 0},
	} {
		got, err := LoadSecret(writeSecretFile(t, c.content, c.mode))
		if c.ok != (err == nil) {
			t.Errorf("%s: err %v", name, err)
			continue
		}
		if err != nil {
			if strings.Contains(err.Error(), "abab") || strings.Contains(err.Error(), "cdcd") {
				t.Errorf("%s: error echoes the content: %v", name, err)
			}
			continue
		}
		if len(got) != c.n {
			t.Errorf("%s: %d bytes, want %d", name, len(got), c.n)
		}
	}
	if _, err := LoadSecret(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file accepted")
	}
	dir := t.TempDir()
	if _, err := LoadSecret(dir); err == nil {
		t.Error("directory accepted")
	}
	target := writeSecretFile(t, h32, 0o600)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSecret(link); err == nil {
		t.Error("symlink accepted")
	}
}

// The secret file's path is not in LoadSecret's errors: they reach the startup log.
func TestLoadSecretErrorsNameNoPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private-dir-name")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"missing":   filepath.Join(dir, "missing-file-name"),
		"directory": dir,
		"group":     writeSecretFileIn(t, dir, "group-file-name", strings.Repeat("ab", 32), 0o640),
	} {
		_, err := LoadSecret(p)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), "private-dir-name") || strings.Contains(err.Error(), "file-name") {
			t.Errorf("%s: error names the path: %v", name, err)
		}
	}
}

func writeSecretFileIn(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// A host program that replaced http.DefaultTransport with another RoundTripper does not make
// NewEventDeliverers panic, and the deliverer does not use that transport (it could add a proxy).
func TestReplacedDefaultTransport(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	var used atomic.Bool
	saved := http.DefaultTransport
	http.DefaultTransport = rtFunc(func(r *http.Request) (*http.Response, error) {
		used.Store(true)
		return nil, errors.New("replaced transport used")
	})
	t.Cleanup(func() { http.DefaultTransport = saved })
	e := f.event(store.EventPaid)
	d := f.deliverer(s.srv.URL, nil)
	pass(t, d)
	if a := f.attempts(e); len(a) != 1 || !a[0].OK || used.Load() {
		t.Fatalf("attempts %+v, replaced transport used %v", a, used.Load())
	}
}

// A 2xx that arrives as the context is cancelled is recorded, so a restart does not send it again.
func TestCancelRacingATwoHundredIsRecorded(t *testing.T) {
	f := newFixture(t)
	e := f.event(store.EventPaid)
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		cancel() // shutdown starts while the answer is on its way
		return &http.Response{StatusCode: 204, Status: "204 No Content", Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	d := f.deliverer("http://192.0.2.1/hook", client)
	if err := d.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if a := f.attempts(e); len(a) != 1 || !a[0].OK || a[0].Status != 204 {
		t.Fatalf("attempts %+v", a)
	}
	// Restart: nothing is sent.
	d2 := f.deliverer("http://192.0.2.1/hook", client)
	pass(t, d2)
	f.clk.advance(time.Hour)
	pass(t, d2)
	if calls.Load() != 1 {
		t.Fatalf("%d requests, want 1", calls.Load())
	}
}

func TestPassEndsAtTheFirstTransportFailure(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, -1)
	e1, e2, e3 := f.event(store.EventPaid), f.event(store.EventExpired), f.event(store.EventPaid)
	d := f.deliverer(s.srv.URL, &http.Client{Timeout: 100 * time.Millisecond})
	pass(t, d)
	if n := len(s.requests()); n != 1 {
		t.Fatalf("%d requests in one pass, want 1", n)
	}
	recorded := 0
	for _, e := range []store.Event{e1, e2, e3} {
		recorded += len(f.attempts(e))
	}
	if recorded != 1 {
		t.Fatalf("%d attempts recorded, want 1", recorded)
	}
	// The next pass resumes from the store: the failed event waits for its backoff, the others go on.
	pass(t, d)
	if n := len(s.requests()); n != 2 {
		t.Fatalf("%d requests after the second pass, want 2", n)
	}
	// An HTTP error status is a response: the pass goes on.
	g := newFixture(t)
	s2 := newSink(t, g.clk, 500)
	g.event(store.EventPaid)
	g.event(store.EventExpired)
	pass(t, g.deliverer(s2.srv.URL, nil))
	if n := len(s2.requests()); n != 2 {
		t.Fatalf("%d requests after 500s, want 2", n)
	}
}

func TestEnvironmentProxyIsIgnored(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	f := newFixture(t)
	for _, c := range []*http.Client{nil, {}, {Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}} {
		d := f.deliverer("http://webhook.example.invalid/hook", c)
		tr, ok := d.client.Transport.(*http.Transport)
		if !ok || tr.Proxy != nil {
			t.Fatalf("transport %#v keeps a proxy", d.client.Transport)
		}
		e := f.event(store.EventPaid)
		pass(t, d)
		if a := f.attempts(e); len(a) != 1 || a[0].OK {
			t.Fatalf("attempts %+v", a)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the proxy saw %d requests", hits.Load())
	}
}

// quotedEvent creates a priced order on Lightning and Monero with a fixed quote of two rates (the XMR one given first)
// and its order.paid event.
func (f *fixture) quotedEvent() store.Event {
	f.t.Helper()
	ctx := context.Background()
	q := store.Quote{Price: "12.50", Currency: "EUR", MarginBps: 150, QuotedAt: t0.Add(500 * time.Millisecond),
		Rates: []store.AssetRate{
			{Asset: "XMR", Rate: "150.25", Sources: []store.QuoteSource{
				{Provider: "kraken", Rate: "150.25", At: t0.Add(-20 * time.Second)},
			}},
			{Asset: "BTC", Rate: "70000.5", Sources: []store.QuoteSource{
				{Provider: "kraken", Rate: "70000", At: t0.Add(-10*time.Second + 999*time.Millisecond)},
				{Provider: "yadio", Rate: "70001", At: t0.Add(-210 * time.Second)},
			}}}}
	o, _, err := f.st.CreateQuotedOrder(ctx, store.Order{AccountID: "acct-1", MerchantRef: "priced", AmountMsat: 18_120_000,
		AmountPiconero: 84_430_000_000, ExpiresAt: f.clk.now().Add(time.Hour)}, q)
	if err != nil {
		f.t.Fatal(err)
	}
	e := store.Event{Type: store.EventPaid, OrderID: o.ID}
	if added, err := f.st.AddEvent(ctx, e); err != nil || !added {
		f.t.Fatalf("AddEvent: %v %v", added, err)
	}
	e.ID = store.EventID(e.Type, o.ID, 1)
	return e
}

// A priced order's event carries the recorded pricing, its times in whole Unix seconds like created_at, one member of
// rates per asset in the order of their names.
func TestPricedEventBody(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	e := f.quotedEvent()
	pass(t, f.deliverer(s.srv.URL, nil))
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d requests", len(got))
	}
	want := `{"id":"` + e.ID + `","type":"order.paid","seq":1,"order_id":"` + e.OrderID + `","created_at":1800000000,` +
		`"pricing":{"price":"12.50","currency":"EUR","margin_percent":"1.5","quoted_at":1800000000,"rates":{` +
		`"BTC":{"rate":"70000.5","sources":[{"provider":"kraken","rate":"70000","at":1799999990},{"provider":"yadio","rate":"70001","at":1799999790}]},` +
		`"XMR":{"rate":"150.25","sources":[{"provider":"kraken","rate":"150.25","at":1799999980}]}}}}`
	if string(got[0].body) != want {
		t.Fatalf("body\n%s\nwant\n%s", got[0].body, want)
	}
	if err := Verify(testSecret, got[0].header, got[0].body, f.clk.now(), 5*time.Minute); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if a := f.attempts(e); len(a) != 1 || !a[0].OK {
		t.Fatalf("attempts %+v", a)
	}
}

// An explicit order's event body is the five members of before plan 7, byte for byte: no pricing member at all.
func TestExplicitEventBodyUnchanged(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	e := f.event(store.EventExpired)
	pass(t, f.deliverer(s.srv.URL, nil))
	got := s.requests()
	want := `{"id":"` + e.ID + `","type":"order.expired","seq":1,"order_id":"` + e.OrderID + `","created_at":1800000000}`
	if len(got) != 1 || string(got[0].body) != want {
		t.Fatalf("requests %d, body %s, want %s", len(got), got[0].body, want)
	}
}

// A failed quote read leaves the event undelivered, with no request and no attempt recorded: it is retried, never
// sent without the pricing it should carry.
func TestQuoteReadFailureSendsNothing(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	e := f.quotedEvent()
	raw, err := sql.Open("sqlite3", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// The rates first: they reference the quotes.
	for _, q := range []string{`DROP TABLE order_quote_rates`, `DROP TABLE order_quotes`} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.deliverer(s.srv.URL, nil).RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce: no error with the quote table gone")
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("%d requests", n)
	}
	if a := f.attempts(e); len(a) != 0 {
		t.Errorf("attempts %+v", a)
	}
	if p, err := f.st.UndeliveredEvents(context.Background(), store.MainEndpoint, t0.Add(-time.Hour), time.Time{}); err != nil || len(p) != 1 || p[0].ID != e.ID {
		t.Errorf("undelivered %+v %v", p, err)
	}
}

// A stored quote that does not read back (a row written past the store) holds back that one event: nothing is sent or
// recorded for it, it stays undelivered, and the rest of the pass goes on. The ALERT naming the event (and nothing of
// the quote) is logged once per event per process; the later passes log at Debug. A restart (a new Deliverer) alerts
// once again.
func TestMalformedQuoteHoldsBackOnlyItsEvent(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	bad := f.event(store.EventPaid)
	raw, err := sql.Open("sqlite3", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// The SQL checks allow it; the store's Go validator refuses sources that are not source objects.
	if _, err := raw.Exec(`INSERT INTO order_quotes VALUES (?, '12.50', 'EUR', 150, 1799999999)`, bad.OrderID); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO order_quote_rates VALUES (?, 'BTC', '70000.5', '["kraken"]')`, bad.OrderID); err != nil {
		t.Fatal(err)
	}
	f.clk.advance(time.Second) // the events are listed by creation time: the malformed one comes first
	good := f.event(store.EventExpired)
	// A logger at Debug, so the later passes' line is captured too.
	debugLog := slog.New(slog.NewTextHandler(f.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	newD := func() *Deliverer {
		d, err := mainDeliverer(s.srv.URL, testSecret, f.st, f.clk.now, nil, debugLog)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := newD()
	alert := "level=ERROR msg=\"" + MalformedQuoteMessage + "\" event=" + bad.ID
	again := "level=DEBUG msg=\"" + heldBackAgain + "\" event=" + bad.ID
	for i, c := range []struct{ alerts, debugs int }{{1, 0}, {1, 1}, {1, 2}, {2, 2}} {
		if i == 3 {
			d = newD() // a restart
		}
		if err := d.RunOnce(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
		got := s.requests()
		if len(got) != 1 || !strings.Contains(string(got[0].body), good.ID) {
			t.Fatalf("pass %d: %d requests, want the other event's one", i+1, len(got))
		}
		if a := f.attempts(bad); len(a) != 0 {
			t.Fatalf("pass %d: attempts for the held event %+v", i+1, a)
		}
		if a := f.attempts(good); len(a) != 1 || !a[0].OK {
			t.Fatalf("pass %d: attempts for the other event %+v", i+1, a)
		}
		if p, err := f.st.UndeliveredEvents(context.Background(), store.MainEndpoint, t0.Add(-time.Hour), time.Time{}); err != nil || len(p) != 1 || p[0].ID != bad.ID {
			t.Fatalf("pass %d: undelivered %+v %v", i+1, p, err)
		}
		logs := f.log.String()
		if n, m := f.log.count(alert), f.log.count(again); n != c.alerts || m != c.debugs || strings.Count(logs, "ALERT") != c.alerts {
			t.Fatalf("pass %d: %d ALERT lines and %d Debug lines, want %d and %d:\n%s", i+1, n, m, c.alerts, c.debugs, logs)
		}
		for _, leak := range []string{"kraken", "12.50", "EUR", "70000", "malformed (order", bad.OrderID} {
			if strings.Contains(logs, leak) {
				t.Fatalf("pass %d: the log carries %q:\n%s", i+1, leak, logs)
			}
		}
	}
}

// mainDeliverer is the single-endpoint form the early tests use: one endpoint named main.
func mainDeliverer(rawURL string, secret []byte, st *store.Store, now func() time.Time, client *http.Client, log *slog.Logger) (*Deliverer, error) {
	ds, err := NewEventDeliverers([]Endpoint{{Name: store.MainEndpoint, URL: rawURL, Secret: secret}}, st, now, client, log)
	if err != nil {
		return nil, err
	}
	return ds[0], nil
}

var (
	secretA = bytes.Repeat([]byte{0x11}, 32)
	secretB = bytes.Repeat([]byte{0x22}, 32)
)

// pair builds the deliverers of two endpoints, "a" and "b", over the fixture's store, each with its own secret.
func (f *fixture) pair(urlA, urlB string, client *http.Client) (a, b *Deliverer) {
	f.t.Helper()
	ds, err := NewEventDeliverers([]Endpoint{{Name: "a", URL: urlA, Secret: secretA}, {Name: "b", URL: urlB, Secret: secretB}},
		f.st, f.clk.now, client, slog.New(slog.NewTextHandler(f.log, nil)))
	if err != nil {
		f.t.Fatal(err)
	}
	if len(ds) != 2 {
		f.t.Fatalf("%d deliverers", len(ds))
	}
	return ds[0], ds[1]
}

func (f *fixture) attemptsOf(endpoint string, e store.Event) []store.Attempt {
	f.t.Helper()
	a, err := f.st.AttemptsFor(context.Background(), endpoint, e.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func TestEventReachesBothEndpointsSignedWithEachOwnSecret(t *testing.T) {
	f := newFixture(t)
	sa, sb := newSink(t, f.clk, 200), newSink(t, f.clk, 200)
	da, db := f.pair(sa.srv.URL, sb.srv.URL, nil)
	e := f.event(store.EventPaid)
	pass(t, da)
	pass(t, db)
	for _, c := range []struct {
		s           *sink
		own, others []byte
	}{{sa, secretA, secretB}, {sb, secretB, secretA}} {
		got := c.s.requests()
		if len(got) != 1 || !strings.Contains(string(got[0].body), e.ID) {
			t.Fatalf("requests %v", got)
		}
		if err := Verify(c.own, got[0].header, got[0].body, f.clk.now(), 5*time.Minute); err != nil {
			t.Fatalf("own secret: %v", err)
		}
		if err := Verify(c.others, got[0].header, got[0].body, f.clk.now(), 5*time.Minute); err == nil {
			t.Fatal("verified with the other endpoint's secret")
		}
	}
	for _, n := range []string{"a", "b"} {
		if a := f.attemptsOf(n, e); len(a) != 1 || !a[0].OK {
			t.Fatalf("attempts of %s: %+v", n, a)
		}
	}
	if a := f.attempts(e); len(a) != 0 {
		t.Fatalf("attempts of main: %+v", a)
	}
}

func TestFailingEndpointDoesNotDelayTheOther(t *testing.T) {
	f := newFixture(t)
	sa, sb := newSink(t, f.clk, 500), newSink(t, f.clk, 200)
	da, db := f.pair(sa.srv.URL, sb.srv.URL, nil)
	e := f.event(store.EventPaid)
	pass(t, da)
	pass(t, db)
	if n := len(sb.requests()); n != 1 {
		t.Fatalf("b got %d requests in its first pass, want 1", n)
	}
	// a retries on its schedule; b has nothing more to do.
	for i := 0; i < 3; i++ {
		f.clk.advance(time.Hour)
		pass(t, da)
		pass(t, db)
	}
	if a := f.attemptsOf("a", e); len(a) != 4 {
		t.Fatalf("a: %d attempts, want 4", len(a))
	}
	if a := f.attemptsOf("b", e); len(a) != 1 || !a[0].OK {
		t.Fatalf("b: %+v", a)
	}
	if n := len(sb.requests()); n != 1 {
		t.Fatalf("b got %d requests", n)
	}
}

// A receiver that does not answer ends the pass of its own deliverer only, and holds no lock the other needs: a's Run
// starts first and is held inside its request, and only then does b's Run start; b's two requests must arrive while a
// is still held.
func TestHungEndpointEndsOnlyItsOwnPass(t *testing.T) {
	f := newFixture(t)
	sa, sb := newSink(t, f.clk, -1), newSink(t, f.clk, 200)
	da, db := f.pair(sa.srv.URL, sb.srv.URL, nil)
	e1 := f.event(store.EventPaid)
	e2 := f.event(store.EventExpired)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	run := func(d *Deliverer) {
		WithIntervals(2*time.Millisecond, 0).apply(d)
		wg.Add(1)
		go func() { defer wg.Done(); d.Run(ctx) }()
	}
	stop := func() { cancel(); wg.Wait() }
	waitFor := func(what string, limit time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for !cond() {
			if time.Now().After(deadline) {
				stop()
				t.Fatalf("timeout waiting for %s: a got %d requests, b got %d", what, len(sa.requests()), len(sb.requests()))
			}
			time.Sleep(time.Millisecond)
		}
	}
	run(da)
	waitFor("a's request to be held", 10*time.Second, func() bool { return len(sa.requests()) >= 1 })
	run(db)
	waitFor("b's two requests while a is held", 3*time.Second, func() bool { return len(sb.requests()) >= 2 })
	// b records an attempt after its receiver has answered: wait for both before looking.
	waitFor("b's attempts", 3*time.Second, func() bool {
		return len(f.attemptsOf("b", e1)) == 1 && len(f.attemptsOf("b", e2)) == 1
	})
	// a is still held in its first request: its pass has not gone on to the second event.
	if n := len(sa.requests()); n != 1 {
		stop()
		t.Fatalf("hung endpoint got %d requests, want 1", n)
	}
	stop()
	for _, e := range []store.Event{e1, e2} {
		if a := f.attemptsOf("b", e); len(a) != 1 || !a[0].OK {
			t.Fatalf("b: %+v", a)
		}
	}
}

func TestEachEndpointGivesUpOnItsOwnWithItsName(t *testing.T) {
	f := newFixture(t)
	sa, sb := newSink(t, f.clk, 500), newSink(t, f.clk, 500)
	da, db := f.pair(sa.srv.URL, sb.srv.URL, nil)
	e := f.event(store.EventPaid)
	pass(t, da)
	f.clk.advance(RetryWindow) // b's first attempt comes late: its attempt is the last one
	pass(t, da)
	pass(t, db)
	if n := f.log.count(GiveUpMessage); n != 2 {
		t.Fatalf("%d give-up lines, want 2\n%s", n, f.log.String())
	}
	for _, name := range []string{"a", "b"} {
		found := 0
		for _, l := range strings.Split(f.log.String(), "\n") {
			if strings.Contains(l, GiveUpMessage) && strings.Contains(l, "endpoint="+name+" ") && strings.Contains(l, "event="+e.ID) {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("%d give-up lines name %s\n%s", found, name, f.log.String())
		}
	}
	// Neither repeats.
	f.clk.advance(RetryWindow)
	pass(t, da)
	pass(t, db)
	if n := f.log.count(GiveUpMessage); n != 2 {
		t.Fatalf("%d give-up lines after more passes", n)
	}
}

// The retry and pass-failed lines carry the endpoint's name too.
func TestEventLinesCarryTheEndpointName(t *testing.T) {
	f := newFixture(t)
	sa, sb := newSink(t, f.clk, 500), newSink(t, f.clk, 200)
	da, db := f.pair(sa.srv.URL, sb.srv.URL, nil)
	f.event(store.EventPaid)
	pass(t, da)
	pass(t, db)
	if f.log.count("will retry") != 1 {
		t.Fatalf("retry lines:\n%s", f.log.String())
	}
	for _, l := range strings.Split(f.log.String(), "\n") {
		if strings.Contains(l, "will retry") && !strings.Contains(l, "endpoint=a ") {
			t.Fatalf("retry line: %s", l)
		}
	}
	f.st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	d := da
	WithIntervals(time.Millisecond, 0).apply(d)
	_ = d.Run(ctx)
	found := false
	for _, l := range strings.Split(f.log.String(), "\n") {
		if strings.Contains(l, "webhook pass failed") {
			found = true
			if !strings.Contains(l, "endpoint=a ") {
				t.Fatalf("pass-failed line: %s", l)
			}
		}
	}
	if !found {
		t.Fatalf("no pass-failed line:\n%s", f.log.String())
	}
}

// An endpoint's first pass fixes the oldest event it can be sent: 24 hours before that pass.
func TestFirstPassBoundsWhatANewEndpointIsSent(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500)
	f.clk.set(t0.Add(-30 * time.Hour))
	e30 := f.event(store.EventPaid)
	f.clk.set(t0.Add(-23 * time.Hour))
	e23 := f.event(store.EventExpired)
	f.clk.set(t0.Add(-time.Hour))
	e1 := f.event(store.EventUnderpaid)
	f.clk.set(t0)
	newD := func(st *store.Store) *Deliverer {
		ds, err := NewEventDeliverers([]Endpoint{{Name: "logger", URL: s.srv.URL, Secret: secretA}}, st, f.clk.now, nil, slog.New(slog.NewTextHandler(f.log, nil)))
		if err != nil {
			t.Fatal(err)
		}
		return ds[0]
	}
	check := func(when string) {
		t.Helper()
		if a := f.attemptsOf("logger", e30); len(a) != 0 {
			t.Fatalf("%s: the 30-hour-old event was tried: %+v", when, a)
		}
		if strings.Contains(f.log.String(), e30.ID) {
			t.Fatalf("%s: a line names the 30-hour-old event:\n%s", when, f.log.String())
		}
	}
	d := newD(f.st)
	pass(t, d)
	check("first pass")
	if n := len(s.requests()); n != 2 {
		t.Fatalf("%d requests, want the two younger events", n)
	}
	for _, e := range []store.Event{e23, e1} {
		if a := f.attemptsOf("logger", e); len(a) != 1 {
			t.Fatalf("attempts %+v", a)
		}
	}
	// A restart: a new store on the same file and a new deliverer for the same name.
	f.st.Close()
	st2, err := store.Open(context.Background(), f.path, store.WithClock(f.clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	f.st = st2
	d = newD(st2)
	pass(t, d)
	check("after a restart")
	// A day on: the younger events get their last attempts, the oldest never.
	f.clk.advance(24 * time.Hour)
	pass(t, d)
	pass(t, newD(st2))
	check("a day on")
	for _, e := range []store.Event{e23, e1} {
		a := f.attemptsOf("logger", e)
		if len(a) < 2 || a[len(a)-1].At.Before(e.CreatedAt.Add(RetryWindow)) {
			t.Fatalf("attempts %+v", a)
		}
	}
	if n := f.log.count(GiveUpMessage); n != 2 {
		t.Fatalf("%d give-up lines, want 2\n%s", n, f.log.String())
	}
}

// The endpoint main of a migrated store has its first pass at the oldest event, so it still gets one last attempt for
// an untried event older than 24 hours, as the single webhook did. The state is made by registering main while the
// clock is at the oldest event's time.
func TestMainOfAMigratedStoreStillGetsItsLastAttempt(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500)
	f.clk.set(t0.Add(-30 * time.Hour))
	e30 := f.event(store.EventPaid)
	if _, err := f.st.RegisterEndpoint(context.Background(), store.MainEndpoint); err != nil {
		t.Fatal(err)
	}
	f.clk.set(t0.Add(-time.Hour))
	e1 := f.event(store.EventExpired)
	f.clk.set(t0)
	d, err := mainDeliverer(s.srv.URL, testSecret, f.st, f.clk.now, nil, slog.New(slog.NewTextHandler(f.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	pass(t, d)
	if a := f.attempts(e30); len(a) != 1 || a[0].OK {
		t.Fatalf("the old event: %+v", a)
	}
	if a := f.attempts(e1); len(a) != 1 {
		t.Fatalf("the young event: %+v", a)
	}
	if n := f.log.count(GiveUpMessage); n != 1 || !strings.Contains(f.log.String(), e30.ID) {
		t.Fatalf("%d give-up lines\n%s", n, f.log.String())
	}
}

func TestConstructingRegistersNothing(t *testing.T) {
	f := newFixture(t)
	f.pair("http://127.0.0.1:1/", "http://127.0.0.1:2/", nil)
	f.clk.advance(time.Hour)
	for _, n := range []string{"a", "b"} {
		got, err := f.st.RegisterEndpoint(context.Background(), n)
		if err != nil || !got.Equal(t0.Add(time.Hour)) {
			t.Fatalf("%s registered at %v (%v), want the later time", n, got, err)
		}
	}
}

// A failing registration ends the pass with the error, sends nothing, and the next pass registers and then bounds the
// events by the real first pass. Only the registration fails here: the endpoints table is made unavailable through a
// raw connection, so a deliverer that ignored the error and went on with a zero first pass would list the old event.
func TestRegistrationFailureEndsThePass(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	f.clk.set(t0.Add(-30 * time.Hour))
	old := f.event(store.EventPaid)
	f.clk.set(t0.Add(-time.Hour))
	recent := f.event(store.EventExpired)
	f.clk.set(t0)
	ds, err := NewEventDeliverers([]Endpoint{{Name: "reg", URL: s.srv.URL, Secret: secretA}}, f.st, f.clk.now, nil, slog.New(slog.NewTextHandler(f.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	d := ds[0]
	raw, err := sql.Open("sqlite3", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`ALTER TABLE webhook_endpoints RENAME TO webhook_endpoints_held`); err != nil {
		t.Fatal(err)
	}
	if err := d.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce with the endpoints table unavailable: no error")
	}
	if n := len(s.requests()); n != 0 {
		t.Fatalf("%d requests in the failed pass", n)
	}
	if _, err := raw.Exec(`ALTER TABLE webhook_endpoints_held RENAME TO webhook_endpoints`); err != nil {
		t.Fatal(err)
	}
	pass(t, d)
	got := s.requests()
	if len(got) != 1 || !strings.Contains(string(got[0].body), recent.ID) {
		t.Fatalf("requests after the good pass: %d", len(got))
	}
	if a := f.attemptsOf("reg", old); len(a) != 0 {
		t.Fatalf("the 30-hour-old event was tried: %+v", a)
	}
	if strings.Contains(f.log.String(), old.ID) {
		t.Fatalf("a line names the 30-hour-old event:\n%s", f.log.String())
	}
}

// The first pass is written once, so a clock that ran ahead at that moment must not hide events that are inside their
// own 24 hours by the clock as it is now.
func TestFirstPassFromAClockAheadHidesNothing(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 200)
	f.clk.set(t0.Add(48 * time.Hour))
	if _, err := f.st.RegisterEndpoint(context.Background(), "ahead"); err != nil {
		t.Fatal(err)
	}
	f.clk.set(t0)
	e := f.event(store.EventPaid)
	ds, err := NewEventDeliverers([]Endpoint{{Name: "ahead", URL: s.srv.URL, Secret: secretA}}, f.st, f.clk.now, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pass(t, ds[0])
	if a := f.attemptsOf("ahead", e); len(a) != 1 || !a[0].OK {
		t.Fatalf("attempts %+v", a)
	}
}

// The edge of the bound, to the second: an event created exactly 24 hours before the first pass is sent (its attempt
// is its last: it gives up if it fails); one second older is skipped, with no line.
func TestFirstPassBoundEdge(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500)
	f.clk.set(t0)
	if _, err := f.st.RegisterEndpoint(context.Background(), "edge"); err != nil {
		t.Fatal(err)
	}
	f.clk.set(t0.Add(-RetryWindow - time.Second))
	tooOld := f.event(store.EventPaid)
	f.clk.set(t0.Add(-RetryWindow))
	onEdge := f.event(store.EventExpired)
	f.clk.set(t0)
	ds, err := NewEventDeliverers([]Endpoint{{Name: "edge", URL: s.srv.URL, Secret: secretA}}, f.st, f.clk.now, nil, slog.New(slog.NewTextHandler(f.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	pass(t, ds[0])
	if a := f.attemptsOf("edge", onEdge); len(a) != 1 {
		t.Fatalf("the event on the edge: %+v", a)
	}
	if a := f.attemptsOf("edge", tooOld); len(a) != 0 {
		t.Fatalf("the event one second older: %+v", a)
	}
	if n := f.log.count(GiveUpMessage); n != 1 || !strings.Contains(f.log.String(), onEdge.ID) || strings.Contains(f.log.String(), tooOld.ID) {
		t.Fatalf("%d give-up lines\n%s", n, f.log.String())
	}
}

func newHeldBackSource(log *slog.Logger) *eventSource {
	return &eventSource{log: log, endpoint: "x", held: &heldBackRecord{seen: map[string]struct{}{}}}
}

// Many goroutines report one held-back event at once: exactly one ALERT line, the rest at Debug.
func TestHeldBackRecordIsSafeAndLogsOnce(t *testing.T) {
	const n = 16
	var buf logBuf
	src := newHeldBackSource(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; src.alertHeldBack("event-1") }()
	}
	close(start)
	wg.Wait()
	if a, d := buf.count(MalformedQuoteMessage), buf.count(heldBackAgain); a != 1 || d != n-1 {
		t.Fatalf("%d ALERT lines and %d Debug lines, want 1 and %d\n%s", a, d, n-1, buf.String())
	}
}

// A full record is cleared at the next new event: the worst case is one more ALERT per forgotten event.
func TestHeldBackRecordCap(t *testing.T) {
	h := &heldBackRecord{seen: map[string]struct{}{}}
	id := func(i int) string { return fmt.Sprintf("event-%d", i) }
	for i := 0; i < maxHeldBack; i++ {
		if !h.first(id(i)) {
			t.Fatalf("event %d seen before", i)
		}
	}
	if len(h.seen) != maxHeldBack || h.first(id(0)) {
		t.Fatalf("a full record: %d entries, event 0 reported new again", len(h.seen))
	}
	if !h.first("another") || len(h.seen) != 1 {
		t.Fatalf("the record was not cleared by a new event: %d entries", len(h.seen))
	}
	if !h.first(id(0)) {
		t.Fatal("a forgotten event is not new again")
	}
}

// One held-back event logs its ALERT once with two endpoints whose Run loops work at once (the race run sees the
// shared record).
func TestHeldBackIsAlertedOnceWithTwoEndpoints(t *testing.T) {
	f := newFixture(t)
	sa, sb := newSink(t, f.clk, 200), newSink(t, f.clk, 200)
	bad := f.event(store.EventPaid)
	raw, err := sql.Open("sqlite3", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO order_quotes VALUES (?, '12.50', 'EUR', 150, 1799999999)`, bad.OrderID); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO order_quote_rates VALUES (?, 'BTC', '70000.5', '["kraken"]')`, bad.OrderID); err != nil {
		t.Fatal(err)
	}
	f.clk.advance(time.Second)
	good := f.event(store.EventExpired)
	ds, err := NewEventDeliverers([]Endpoint{{Name: "a", URL: sa.srv.URL, Secret: secretA}, {Name: "b", URL: sb.srv.URL, Secret: secretB}},
		f.st, f.clk.now, nil, slog.New(slog.NewTextHandler(f.log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, d := range ds {
		WithIntervals(2*time.Millisecond, 2*time.Millisecond).apply(d)
		wg.Add(1)
		go func() { defer wg.Done(); d.Run(ctx) }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(sa.requests()) < 1 || len(sb.requests()) < 1 || f.log.count(heldBackAgain) < 6 {
		if time.Now().After(deadline) {
			cancel()
			wg.Wait()
			t.Fatalf("timeout:\n%s", f.log.String())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	wg.Wait()
	if n := f.log.count(MalformedQuoteMessage); n != 1 || f.log.count("ALERT") != 1 {
		t.Fatalf("%d held-back ALERT lines, want 1\n%s", n, f.log.String())
	}
	for _, n := range []string{"a", "b"} {
		if a := f.attemptsOf(n, bad); len(a) != 0 {
			t.Fatalf("%s: attempts for the held event %+v", n, a)
		}
		if a := f.attemptsOf(n, good); len(a) != 1 || !a[0].OK {
			t.Fatalf("%s: %+v", n, a)
		}
	}
}

func TestNewEventDeliverersValidation(t *testing.T) {
	f := newFixture(t)
	ok := func(name string) Endpoint {
		return Endpoint{Name: name, URL: "http://192.0.2.1/hook-token-9", Secret: secretA}
	}
	build := func(eps ...Endpoint) ([]*Deliverer, error) {
		return NewEventDeliverers(eps, f.st, f.clk.now, nil, nil)
	}
	for _, c := range []struct {
		name string
		eps  []Endpoint
		want string // a substring of the error
	}{
		{"empty list", nil, "at least one"},
		{"empty name", []Endpoint{ok("a"), ok("b"), ok("")}, "endpoints[2]"},
		{"space in name", []Endpoint{ok("a b")}, "endpoints[0]"},
		{"long name", []Endpoint{ok("a"), ok(strings.Repeat("n", 65))}, "endpoints[1]"},
		{"slash in name", []Endpoint{ok("a/b")}, "endpoints[0]"},
		{"repeated name", []Endpoint{ok("x"), ok("y"), ok("x")}, `endpoint "x"`},
		{"bad url", []Endpoint{ok("a"), {Name: "logger", URL: "ftp://192.0.2.1/tok-77", Secret: secretA}}, `endpoint "logger"`},
		{"url with user info", []Endpoint{{Name: "logger", URL: "http://u:test-password-1@192.0.2.1/", Secret: secretA}}, `endpoint "logger"`},
		{"short secret", []Endpoint{{Name: "books", URL: "http://192.0.2.1/tok-77", Secret: secretA[:5]}}, `endpoint "books"`},
	} {
		ds, err := build(c.eps...)
		if err == nil || ds != nil {
			t.Errorf("%s: accepted (%d deliverers)", c.name, len(ds))
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, c.want) {
			t.Errorf("%s: error %q lacks %q", c.name, msg, c.want)
		}
		for _, leak := range []string{"192.0.2.1", "tok-77", "test-password-1", "hook-token", "ftp:", "a b", "a/b", strings.Repeat("n", 65), "11111111"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%s: error %q carries %q", c.name, msg, leak)
			}
		}
	}
	if _, err := NewEventDeliverers([]Endpoint{ok("a")}, nil, f.clk.now, nil, nil); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := NewEventDeliverers([]Endpoint{ok("a")}, f.st, nil, nil, nil); err == nil {
		t.Error("nil clock accepted")
	}
	ds, err := build(ok("z"), ok("A.b_c-9"), ok(strings.Repeat("n", 64)))
	if err != nil || len(ds) != 3 {
		t.Fatalf("%d deliverers, %v", len(ds), err)
	}
	// In the order given: each delivers under its own name.
	s := newSink(t, f.clk, 200)
	e := f.event(store.EventPaid)
	ds, err = NewEventDeliverers([]Endpoint{{Name: "second", URL: s.srv.URL, Secret: secretA}, {Name: "first", URL: s.srv.URL, Secret: secretB}},
		f.st, f.clk.now, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pass(t, ds[0])
	if a := f.attemptsOf("second", e); len(a) != 1 {
		t.Fatalf("deliverers not in the order given: %+v", a)
	}
	if a := f.attemptsOf("first", e); len(a) != 0 {
		t.Fatalf("first: %+v", a)
	}
}

// giveUpLines counts the give-up lines that carry the endpoint's name and the event's id.
func giveUpLines(l *logBuf, endpoint string, e store.Event) int {
	n := 0
	for _, line := range strings.Split(l.String(), "\n") {
		if strings.Contains(line, GiveUpMessage) && strings.Contains(line, "endpoint="+endpoint+" ") && strings.Contains(line, "event="+e.ID) {
			n++
		}
	}
	return n
}

// An event that already has an attempt stays listed until it is abandoned, whatever the first-pass floor does. The
// first pass was stamped by a clock that ran ahead by 30 h: past the event's own 24 hours, so the current-clock floor
// slides while the event's last attempt falls due (a smaller offset is the next test); the passes around the deadline
// avoid the one second where the old floor and the deadline overlapped.
func TestAttemptedEventStaysListedUntilAbandoned(t *testing.T) {
	for _, ahead := range []time.Duration{30 * time.Hour} {
		t.Run(ahead.String(), func(t *testing.T) {
			f := newFixture(t)
			s := newSink(t, f.clk, 500)
			f.clk.set(t0.Add(ahead))
			if _, err := f.st.RegisterEndpoint(context.Background(), "ahead"); err != nil {
				t.Fatal(err)
			}
			f.clk.set(t0)
			e := f.event(store.EventPaid)
			ds, err := NewEventDeliverers([]Endpoint{{Name: "ahead", URL: s.srv.URL, Secret: secretA}}, f.st, f.clk.now, nil, slog.New(slog.NewTextHandler(f.log, nil)))
			if err != nil {
				t.Fatal(err)
			}
			d := ds[0]
			pass(t, d)
			for h := 1; h < 24; h++ {
				f.clk.set(t0.Add(time.Duration(h) * time.Hour))
				pass(t, d)
			}
			deadline := t0.Add(RetryWindow)
			f.clk.set(deadline.Add(-5 * time.Second))
			pass(t, d)
			if n := giveUpLines(f.log, "ahead", e); n != 0 {
				t.Fatalf("%d give-up lines before the deadline", n)
			}
			f.clk.set(deadline.Add(30 * time.Second))
			pass(t, d)
			a := f.attemptsOf("ahead", e)
			if len(a) == 0 || a[len(a)-1].At.Before(deadline) {
				t.Fatalf("no last attempt: %+v", a)
			}
			if n := giveUpLines(f.log, "ahead", e); n != 1 {
				t.Fatalf("%d give-up lines, want 1\n%s", n, f.log.String())
			}
			// The clock passes the stored first pass: nothing more is attempted, no second line.
			f.clk.set(t0.Add(ahead + time.Hour))
			pass(t, d)
			pass(t, d)
			if b := f.attemptsOf("ahead", e); len(b) != len(a) {
				t.Fatalf("attempts after the clock passed the first pass: %d, were %d", len(b), len(a))
			}
			if n := giveUpLines(f.log, "ahead", e); n != 1 {
				t.Fatalf("%d give-up lines after the first pass, want 1", n)
			}
		})
	}
}

// With several events pending and a receiver that gives no response (each pass ends after one attempt), every event
// still gets its last attempt and its line.
func TestEveryPendingEventGetsItsLastAttemptWhenTheReceiverIsSilent(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, -1)
	f.clk.set(t0.Add(30 * time.Hour))
	if _, err := f.st.RegisterEndpoint(context.Background(), "silent"); err != nil {
		t.Fatal(err)
	}
	f.clk.set(t0)
	var evs []store.Event
	for i := 0; i < 3; i++ {
		evs = append(evs, f.event(store.EventPaid))
		f.clk.advance(time.Second)
	}
	ds, err := NewEventDeliverers([]Endpoint{{Name: "silent", URL: s.srv.URL, Secret: secretA}}, f.st, f.clk.now,
		&http.Client{Timeout: 50 * time.Millisecond}, slog.New(slog.NewTextHandler(f.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	d := ds[0]
	// Each pass ends at its first silent attempt, so it takes one pass per event to try each of them.
	passes := func(at time.Time) {
		f.clk.set(at)
		for range evs {
			pass(t, d)
		}
	}
	passes(t0.Add(3 * time.Second))
	passes(t0.Add(RetryWindow - 5*time.Second))
	passes(t0.Add(RetryWindow + time.Minute))
	for _, e := range evs {
		a := f.attemptsOf("silent", e)
		if len(a) == 0 || a[len(a)-1].At.Before(e.CreatedAt.Add(RetryWindow)) {
			t.Fatalf("no last attempt for %s: %+v", e.ID, a)
		}
		if n := giveUpLines(f.log, "silent", e); n != 1 {
			t.Fatalf("%d give-up lines for %s\n%s", n, e.ID, f.log.String())
		}
	}
}

// No deliverer keeps a connection open between requests: net/http logs what a server sends on an idle connection to
// the standard logger.
func TestDeliverersDisableKeepAlives(t *testing.T) {
	f := newFixture(t)
	custom := &http.Transport{}
	for _, c := range []*http.Client{nil, {}, {Transport: custom}} {
		d := f.deliverer("http://127.0.0.1:1/", c)
		tr, ok := d.client.Transport.(*http.Transport)
		if !ok || !tr.DisableKeepAlives || tr.Proxy != nil {
			t.Errorf("event transport for %+v: %+v", c, d.client.Transport)
		}
		ad, err := NewAlertDeliverer("http://127.0.0.1:1/", testSecret, f.st, f.clk.now, c, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tr, ok := ad.client.Transport.(*http.Transport); !ok || !tr.DisableKeepAlives {
			t.Errorf("alert transport for %+v: %+v", c, ad.client.Transport)
		}
	}
	if custom.DisableKeepAlives {
		t.Error("the caller's transport was changed")
	}
}

func TestTwoPostsUseTwoConnections(t *testing.T) {
	f := newFixture(t)
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	d := f.deliverer(srv.URL, nil)
	f.event(store.EventPaid)
	f.event(store.EventExpired)
	pass(t, d)
	if n := conns.Load(); n != 2 {
		t.Fatalf("%d connections for two posts, want 2", n)
	}
}

// A small offset loses an event on a floor that slides: a catch-up event created 22 h before registration, the first
// pass stamped 3 h ahead (25 h after the event), so the event's deadline falls before the stamp.
func TestCatchUpEventGetsItsLastAttemptWhenTheFirstPassRanAhead(t *testing.T) {
	f := newFixture(t)
	s := newSink(t, f.clk, 500)
	f.clk.set(t0.Add(-22 * time.Hour))
	e := f.event(store.EventPaid)
	f.clk.set(t0.Add(3 * time.Hour)) // the stamp: 25 h after the event
	if _, err := f.st.RegisterEndpoint(context.Background(), "late"); err != nil {
		t.Fatal(err)
	}
	f.clk.set(t0) // corrected
	ds, err := NewEventDeliverers([]Endpoint{{Name: "late", URL: s.srv.URL, Secret: secretA}}, f.st, f.clk.now, nil,
		slog.New(slog.NewTextHandler(f.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	d := ds[0]
	pass(t, d) // admitted: created at or after now - 24 h
	if a := f.attemptsOf("late", e); len(a) != 1 {
		t.Fatalf("first pass: %+v", a)
	}
	f.clk.set(t0.Add(time.Hour))
	pass(t, d)
	deadline := e.CreatedAt.Add(RetryWindow) // t0+2h, before the stamp
	f.clk.set(deadline.Add(30 * time.Second))
	pass(t, d)
	a := f.attemptsOf("late", e)
	if len(a) == 0 || a[len(a)-1].At.Before(deadline) {
		t.Fatalf("no last attempt: %+v", a)
	}
	if n := giveUpLines(f.log, "late", e); n != 1 {
		t.Fatalf("%d give-up lines", n)
	}
}
