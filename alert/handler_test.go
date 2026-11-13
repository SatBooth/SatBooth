package alert

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/webhook"

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

// textHandler is a JSON handler without the time, so two runs over the same records give the same bytes.
func textHandler(w *logBuf) slog.Handler {
	return slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}})
}

// fakeQueue records what is added and answers with queued and err.
type fakeQueue struct {
	mu     sync.Mutex
	got    []Alert
	queued bool
	err    error
}

func (q *fakeQueue) Add(_ context.Context, a Alert) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.got = append(q.got, a)
	return q.queued, q.err
}

func (q *fakeQueue) alerts() []Alert {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Alert(nil), q.got...)
}

func newTestHandler(q Queue) (*Handler, *logBuf, *clock) {
	buf, clk := &logBuf{}, &clock{t: t0}
	return NewHandler(textHandler(buf), q, clk.now), buf, clk
}

const hex32 = "0123456789abcdef0123456789abcdef"

// Every record reaches next unchanged: the bytes are those of next alone, for plain lines, ALERT lines, attributes,
// WithAttrs and WithGroup.
func TestHandlerPassesThrough(t *testing.T) {
	logAll := func(l *slog.Logger) {
		l.Info("satbooth started")
		l.Error("ALERT: an order was paid on both rails", "order", hex32)
		l.Warn("ALERT: x", "providers", []string{"kraken"}, "code", -28)
		l.Debug("debug line", slog.Group("g", "a", 1))
		l.With("reason", "store").Error("ALERT: with attrs", "count", 3)
		l.WithGroup("grp").Error("ALERT: in a group", "count", 3)
		l.Error("")
	}
	want, got := &logBuf{}, &logBuf{}
	logAll(slog.New(textHandler(want)))
	h := NewHandler(textHandler(got), &fakeQueue{queued: true}, time.Now)
	logAll(slog.New(h))
	if got.String() != want.String() {
		t.Fatalf("output changed\n got %s\nwant %s", got, want)
	}
	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("Enabled is not next's")
	}
	if NewHandler(slog.NewJSONHandler(&logBuf{}, nil), &fakeQueue{}, time.Now).Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("Enabled is not next's")
	}
}

// pending empties the channel without the drain.
func pending(h *Handler) []Alert {
	var out []Alert
	for {
		select {
		case a := <-h.c.ch:
			out = append(out, a)
		default:
			return out
		}
	}
}

// The prefix is exact and case-sensitive.
func TestHandlerQueuesOnlyAlert(t *testing.T) {
	h, _, _ := newTestHandler(&fakeQueue{queued: true})
	l := slog.New(h)
	for _, m := range []string{"ALERT", "ALERT x", "alert: x", "Alert: x", " ALERT: x", "XALERT: x", "ALERT;", "", "ALERT\x00:", "ＡLERT: x"} {
		l.Error(m)
	}
	if p := pending(h); len(p) != 0 {
		t.Fatalf("queued %+v", p)
	}
	for _, m := range []string{"ALERT:", "ALERT: x", "ALERT:x"} {
		l.Error(m)
	}
	l.Info("ALERT: at info") // the level does not matter, only the prefix
	var msgs []string
	for _, a := range pending(h) {
		msgs = append(msgs, a.Message)
	}
	if want := []string{"ALERT:", "ALERT: x", "ALERT:x", "ALERT: at info"}; !reflect.DeepEqual(msgs, want) {
		t.Fatalf("queued %q, want %q", msgs, want)
	}
}

type valuer struct{ v slog.Value }

func (v valuer) LogValue() slog.Value { return v.v }

type panicValuer struct{}

func (panicValuer) LogValue() slog.Value { panic("store") }

type stringer struct{}

func (stringer) String() string { return "store" }

// Each shape is accepted at its edge and refused one past it; an unknown key, a group or a wrong kind is counted in
// dropped.
func TestHandlerFieldAllowlist(t *testing.T) {
	w40, w41 := "a"+strings.Repeat("b_9", 13), "a"+strings.Repeat("b_9", 13)+"c"
	cases := []struct {
		name   string
		attr   slog.Attr
		want   string // the field's value; "" means dropped
		wantOK bool
	}{
		{"hex32", slog.String("order", hex32), hex32, true},
		{"hex32 order_id", slog.String("order_id", hex32), hex32, true},
		{"hex32 refund_id", slog.String("refund_id", hex32), hex32, true},
		{"hex32 event", slog.String("event", hex32), hex32, true},
		{"hex31", slog.String("order", hex32[:31]), "", false},
		{"hex33", slog.String("order", hex32+"0"), "", false},
		{"hex64", slog.String("order", hex32+hex32), "", false},
		{"hex upper", slog.String("order", strings.ToUpper(hex32)), "", false},
		{"hex g", slog.String("order", hex32[:31]+"g"), "", false},
		{"hex as int", slog.Int("order", 5), "", false},
		{"event_id not allowed", slog.String("event_id", hex32), "", false},

		{"uint 0", slog.Int("count", 0), "0", true},
		{"uint 2^53", slog.Int64("step", 1<<53), "9007199254740992", true},
		{"uint 2^53 uint64", slog.Uint64("from_height", 1<<53), "9007199254740992", true},
		{"uint 2^53+1", slog.Int64("step", 1<<53+1), "", false},
		{"uint 2^53+1 uint64", slog.Uint64("from_height", 1<<53+1), "", false},
		{"uint max int64", slog.Int64("step", math.MaxInt64), "", false},
		{"uint max uint64", slog.Uint64("from_height", math.MaxUint64), "", false},
		{"uint to_height", slog.Int("to_height", 7), "7", true},
		{"uint unused", slog.Int("unused", 1000), "1000", true},
		{"uint passes", slog.Int("passes", 3), "3", true},
		{"uint -1", slog.Int("count", -1), "", false},
		{"uint as string", slog.String("count", "5"), "", false},
		{"uint as float", slog.Float64("count", 5), "", false},
		{"generation not allowed", slog.Int("generation", 1), "", false},

		{"code min", slog.Int64("code", math.MinInt32), "-2147483648", true},
		{"code max", slog.Int64("code", math.MaxInt32), "2147483647", true},
		{"code uint max", slog.Uint64("code", math.MaxInt32), "2147483647", true},
		{"code min-1", slog.Int64("code", math.MinInt32-1), "", false},
		{"code max+1", slog.Int64("code", math.MaxInt32+1), "", false},
		{"code uint max+1", slog.Uint64("code", math.MaxInt32+1), "", false},
		{"code string", slog.String("code", "-5"), "", false},

		{"word 1", slog.String("reason", "a"), "a", true},
		{"word 40", slog.String("method", w40), w40, true},
		{"word 41", slog.String("method", w41), "", false},
		{"word empty", slog.String("reason", ""), "", false},
		{"word underscore", slog.String("reason", "too_few"), "too_few", true},
		{"word result", slog.String("result", "exists_with_different_credit_account_id"), "exists_with_different_credit_account_id", true},
		{"word low", slog.String("low", "bitfinex"), "bitfinex", true},
		{"word high", slog.String("high", "kraken"), "kraken", true},
		{"word leading digit", slog.String("reason", "1a"), "", false},
		{"word leading _", slog.String("reason", "_a"), "", false},
		{"word upper", slog.String("reason", "Store"), "", false},
		{"word dash", slog.String("reason", "a-b"), "", false},
		{"word space", slog.String("reason", "a b"), "", false},
		{"word as int", slog.Int("reason", 1), "", false},
		{"endpoint 1", slog.String("endpoint", "a"), "a", true},
		{"endpoint 64", slog.String("endpoint", "Ab.9_-"+strings.Repeat("x", 58)), "Ab.9_-" + strings.Repeat("x", 58), true},
		{"endpoint 65", slog.String("endpoint", strings.Repeat("x", 65)), "", false},
		{"endpoint empty", slog.String("endpoint", ""), "", false},
		{"endpoint space", slog.String("endpoint", "a b"), "", false},
		{"endpoint slash", slog.String("endpoint", "a/b"), "", false},
		{"endpoint non-ASCII", slog.String("endpoint", "caf\u00e9"), "", false},
		{"endpoint as int", slog.Int("endpoint", 1), "", false},
		{"rail not allowed", slog.String("rail", "onchain"), "", false},

		{"upper3 BTC", slog.String("asset", "BTC"), "BTC", true},
		{"upper3 USD", slog.String("currency", "USD"), "USD", true},
		{"upper3 2", slog.String("asset", "BT"), "", false},
		{"upper3 4", slog.String("currency", "USDT"), "", false},
		{"upper3 lower", slog.String("currency", "usd"), "", false},
		{"upper3 digit", slog.String("currency", "U5D"), "", false},

		{"providers", slog.Any("providers", []string{"kraken", "bitfinex"}), "", false},
		{"unknown", slog.String("secret", "store"), "", false},
		{"group", slog.Group("order", slog.String("x", "y")), "", false},
		{"inline group", slog.Group("", slog.String("reason", "store")), "", false},
		{"valuer to string", slog.Any("reason", valuer{slog.StringValue("store")}), "store", true},
		{"valuer to bad", slog.Any("reason", valuer{slog.StringValue("Store!")}), "", false},
		{"stringer", slog.Any("reason", stringer{}), "", false},
		{"valuer to group", slog.Any("order", valuer{slog.GroupValue(slog.String("order", hex32))}), "", false},
		{"valuer to any", slog.Any("order", valuer{slog.AnyValue([]byte(hex32))}), "", false},
		{"valuer panics", slog.Any("reason", panicValuer{}), "", false},
		{"error", slog.Any("reason", errors.New("store")), "", false},
		{"bool", slog.Bool("reason", true), "", false},
		{"duration", slog.Duration("count", time.Second), "", false},
	}
	for _, c := range cases {
		h, _, _ := newTestHandler(&fakeQueue{queued: true})
		slog.New(h).LogAttrs(context.Background(), slog.LevelError, "ALERT: x", c.attr)
		p := pending(h)
		if len(p) != 1 {
			t.Fatalf("%s: %d alerts", c.name, len(p))
		}
		v, ok := p[0].Fields[c.attr.Key]
		if ok != c.wantOK || v != c.want || (c.wantOK && len(p[0].Fields) != 1) {
			t.Errorf("%s: fields %v", c.name, p[0].Fields)
		}
		if wantDropped := map[bool]int{true: 0, false: 1}[c.wantOK]; p[0].Dropped != wantDropped {
			t.Errorf("%s: dropped %d, want %d", c.name, p[0].Dropped, wantDropped)
		}
	}
}

// WithAttrs before a group count; anything after WithGroup is dropped; a repeated key keeps the first and drops the
// rest.
func TestHandlerWithAttrsAndGroups(t *testing.T) {
	h, _, _ := newTestHandler(&fakeQueue{queued: true})
	l := slog.New(h).With("reason", "store", "providers", []string{"x"})
	l.Error("ALERT: x", "count", 3, "count", 4, "reason", "wallet")
	l.WithGroup("g").With("step", 1).Error("ALERT: y", "count", 5)
	p := pending(h)
	if len(p) != 2 {
		t.Fatalf("%d alerts", len(p))
	}
	if want := (Alert{Message: "ALERT: x", Fields: map[string]string{"reason": "store", "count": "3"}, Dropped: 3}); !reflect.DeepEqual(p[0], want) {
		t.Fatalf("got %+v, want %+v", p[0], want)
	}
	if want := (Alert{Message: "ALERT: y", Fields: map[string]string{"reason": "store"}, Dropped: 3}); !reflect.DeepEqual(p[1], want) {
		t.Fatalf("got %+v, want %+v", p[1], want)
	}
}

// Integers are JSON numbers, everything else strings, keys sorted.
func TestFieldsJSON(t *testing.T) {
	got := FieldsJSON(map[string]string{"reason": "too_few", "order": hex32, "count": "3", "code": "-28", "asset": "BTC", "step": "x1"})
	want := `{"asset":"BTC","code":-28,"count":3,"order":"` + hex32 + `","reason":"too_few","step":"x1"}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// No HTML escaping: the id and the payload carry the same plain bytes.
	if got := FieldsJSON(map[string]string{"<&>": "a<b>&c"}); got != `{"<&>":"a<b>&c"}` {
		t.Fatalf("escaped: %s", got)
	}
	if FieldsJSON(nil) != "{}" {
		t.Fatal("empty fields")
	}
}

func openStore(t *testing.T, path string, clk *clock) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path, store.WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func undelivered(t *testing.T, st *store.Store) []store.PendingAlert {
	t.Helper()
	p, err := st.UndeliveredAlerts(context.Background(), time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The same alert twice within one UTC hour bucket is 1 alert with repeats 2; the next bucket is a new alert.
func TestHandlerCoalesce(t *testing.T) {
	clk := &clock{t: time.Unix(1_799_996_400, 0)}
	st := openStore(t, filepath.Join(t.TempDir(), "store.db"), clk)
	defer st.Close()
	buf := &logBuf{}
	h := NewHandler(textHandler(buf), NewStoreQueue(st), clk.now)
	l := slog.New(h)
	l.Error("ALERT: an order was paid on both rails", "order", hex32)
	clk.set(time.Unix(1_799_996_400+3599, 0))
	l.Error("ALERT: an order was paid on both rails", "order", hex32)
	h.c.flush(context.Background())
	p := undelivered(t, st)
	if len(p) != 1 || p[0].Repeats != 2 || p[0].Fields != `{"order":"`+hex32+`"}` {
		t.Fatalf("pending %+v", p)
	}
	clk.set(time.Unix(1_799_996_400+3600, 0))
	l.Error("ALERT: an order was paid on both rails", "order", hex32)
	h.c.flush(context.Background())
	if p := undelivered(t, st); len(p) != 2 || p[1].Repeats != 1 {
		t.Fatalf("next bucket: %+v", p)
	}
}

// 1,001 distinct alerts in one day give 1,000 rows and one "queue full" line, and the count survives a restart.
func TestHandlerDailyBound(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	clk := &clock{t: time.Unix(1_800_057_600, 0)}
	st := openStore(t, path, clk)
	buf := &logBuf{}
	h := NewHandler(textHandler(buf), NewStoreQueue(st), clk.now)
	l := slog.New(h)
	for i := range store.MaxAlertsPerDay + 1 {
		l.Error("ALERT: x", "count", i)
		if i%200 == 0 {
			h.c.flush(ctx)
		}
	}
	h.c.flush(ctx)
	l.Error("ALERT: x", "count", 5000) // still full: no second line the same day
	h.c.flush(ctx)
	countRows := func(st *store.Store) int { return len(undelivered(t, st)) }
	if n := countRows(st); n != store.MaxAlertsPerDay {
		t.Fatalf("%d rows", n)
	}
	if c := strings.Count(buf.String(), LogFull); c != 1 {
		t.Fatalf("%d queue-full lines:\n%s", c, buf)
	}
	st.Close()

	st = openStore(t, path, clk)
	defer st.Close()
	buf2 := &logBuf{}
	h2 := NewHandler(textHandler(buf2), NewStoreQueue(st), clk.now)
	slog.New(h2).Error("ALERT: x", "count", 5001)
	h2.c.flush(ctx)
	if n := countRows(st); n != store.MaxAlertsPerDay {
		t.Fatalf("after restart %d rows", n)
	}
	if c := strings.Count(buf2.String(), LogFull); c != 1 {
		t.Fatalf("after restart %d queue-full lines", c)
	}
	// The next UTC day: queued again, and a full queue then logs again.
	clk.set(time.Unix(1_800_057_600+86_400, 0))
	slog.New(h2).Error("ALERT: x", "count", 5002)
	h2.c.flush(ctx)
	if n := countRows(st); n != store.MaxAlertsPerDay+1 {
		t.Fatalf("next day %d rows", n)
	}
}

// No line logged by the alert machinery or the alert deliverer is queued: the queue-full, overflow, failed-write and
// give-up lines carry no ALERT: prefix, and the machinery logs past the wrapper.
func TestHandlerNoReentry(t *testing.T) {
	ctx := context.Background()
	for _, line := range []string{LogFull, LogOverflow, LogWriteFailed, webhook.AlertGiveUpMessage} {
		if strings.Contains(line, "ALERT") {
			t.Fatalf("%q carries ALERT", line)
		}
	}
	q := &fakeQueue{err: errors.New("disk I/O error near ALERT: secret")}
	h, buf, _ := newTestHandler(q)
	l := slog.New(h)
	for range QueueSize + 5 { // 5 overflow
		l.Error("ALERT: x")
	}
	h.c.flush(ctx) // every write fails: LogWriteFailed, then LogOverflow
	q.mu.Lock()
	q.err, q.queued = nil, false
	q.mu.Unlock()
	l.Error("ALERT: y")
	h.c.flush(ctx) // LogFull
	l.Error(webhook.AlertGiveUpMessage, "alert", hex32)
	if p := pending(h); len(p) != 0 {
		t.Fatalf("queued after the machinery's lines: %+v", p)
	}
	if n := len(q.alerts()); n != QueueSize+1 {
		t.Fatalf("%d adds, want %d", n, QueueSize+1)
	}
	out := buf.String()
	for _, want := range []string{`"msg":"` + LogWriteFailed + `"`, `"msg":"` + LogOverflow + `","count":5`, `"msg":"` + LogFull + `"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, "disk I/O") || strings.Contains(out, "secret") {
		t.Fatal("an error's text reached the log")
	}
	// A failed write is logged once per pass of the drain, not once per alert.
	if c := strings.Count(out, LogWriteFailed); c != 1 {
		t.Fatalf("%d failed-write lines", c)
	}
}

// With the drain stopped, 10,000 ALERT records return promptly; the overflow is counted and logged once the drain
// runs.
func TestHandlerNeverBlocks(t *testing.T) {
	q := &fakeQueue{queued: true}
	h, buf, _ := newTestHandler(q)
	l := slog.New(h)
	done := make(chan struct{})
	go func() {
		for i := range 10_000 {
			l.Error("ALERT: x", "count", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("logging blocked")
	}
	if n := h.c.overflow.Load(); n != 10_000-QueueSize {
		t.Fatalf("overflow %d", n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- h.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(q.alerts()) < QueueSize || !strings.Contains(buf.String(), LogOverflow) {
		if time.Now().After(deadline) {
			t.Fatalf("drain: %d alerts, log %s", len(q.alerts()), buf)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if want := fmt.Sprintf(`"msg":"%s","count":%d`, LogOverflow, 10_000-QueueSize); !strings.Contains(buf.String(), want) {
		t.Fatalf("no %s in %s", want, buf)
	}
	if h.c.overflow.Load() != 0 {
		t.Fatal("overflow not reset after it was logged")
	}
}

// Run drains what is left in the channel when it stops, so an alert logged during the shutdown still gets its row.
func TestRunFlushesOnStop(t *testing.T) {
	q := &fakeQueue{queued: true}
	h, _, _ := newTestHandler(q)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	slog.New(h).Error("ALERT: during shutdown")
	if err := h.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if a := q.alerts(); len(a) != 1 || a[0].Message != "ALERT: during shutdown" {
		t.Fatalf("adds %+v", a)
	}
}

// Hostile values never reach the stored fields: hashes, invoices, addresses, keys and secrets are refused by shape.
func TestHandlerHostileValues(t *testing.T) {
	clk := &clock{t: t0}
	st := openStore(t, filepath.Join(t.TempDir(), "store.db"), clk)
	defer st.Close()
	h := NewHandler(textHandler(&logBuf{}), NewStoreQueue(st), clk.now)
	hostile := map[string]string{
		"order":     strings.Repeat("ab", 32), // a payment hash or preimage
		"order_id":  strings.Repeat("c", 31) + "Z",
		"refund_id": hex32 + "\n",
		"reason":    "lnbcrt210u1pjqqqqqpp5qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqsdqqcqzzsxqyz5vqsp5",
		"method":    "bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080",
		// xprv-shaped text: the beginning of BIP-32 test vector 1's xprv, with an altered end.
		"result":   "xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LrVHy",
		"low":      "4" + strings.Repeat("a", 94), // a Monero address
		"asset":    "nsec",
		"currency": "sk_",
		"secret":   "abc",
		"invoice":  "lnbc1",
		"preimage": hex32,
		"address":  "bc1q",
		"password": "a",
		"count":    "1",
		"code":     "-1",
	}
	var attrs []any
	for k, v := range hostile {
		attrs = append(attrs, k, v)
	}
	slog.New(h).Error("ALERT: hostile", attrs...)
	h.c.flush(context.Background())
	p := undelivered(t, st)
	if len(p) != 1 || p[0].Fields != "{}" || p[0].Dropped != len(hostile) {
		t.Fatalf("pending %+v", p)
	}
}

// blockingQueue's Add waits until its context ends.
type blockingQueue struct{ calls atomic.Int64 }

func (q *blockingQueue) Add(ctx context.Context, _ Alert) (bool, error) {
	q.calls.Add(1)
	<-ctx.Done()
	return false, ctx.Err()
}

// The final flush has an overall deadline: with a store that never answers and a full channel, Run returns within
// flushTimeout (plus one write's slack), not 256 write timeouts later.
func TestRunFlushDeadline(t *testing.T) {
	old := flushTimeout
	flushTimeout = 300 * time.Millisecond
	defer func() { flushTimeout = old }()
	q := &blockingQueue{}
	h, buf, _ := newTestHandler(q)
	for range QueueSize {
		slog.New(h).Error("ALERT: x")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := h.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the final flush took %v", d)
	}
	if n := q.calls.Load(); n == 0 || n == QueueSize {
		t.Fatalf("%d writes tried", n)
	}
	if !strings.Contains(buf.String(), LogWriteFailed) {
		t.Fatalf("no failed-write line:\n%s", buf)
	}
}

// A stop that arrives while a batch is being written (the store stuck) is bounded by the same overall flushTimeout:
// the in-flight write and the rest of the channel end at the deadline, not 256 write timeouts later.
func TestRunStopMidBatch(t *testing.T) {
	old := flushTimeout
	flushTimeout = 300 * time.Millisecond
	defer func() { flushTimeout = old }()
	q := &blockingQueue{}
	h, _, _ := newTestHandler(q)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	for range QueueSize {
		slog.New(h).Error("ALERT: x")
	}
	deadline := time.Now().Add(5 * time.Second)
	for q.calls.Load() == 0 { // the drain is inside its first write
		if time.Now().After(deadline) {
			t.Fatal("the drain did not start writing")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within the flush deadline after a stop mid-batch")
	}
}

// Two endpoints that abandon one event are two alerts (two ids), not one alert with a repeat; the field is written as
// a JSON string like any text value, and the store accepts the record.
func TestEndpointDistinguishesAlertIDs(t *testing.T) {
	fields := func(ep string) string {
		h, _, _ := newTestHandler(&fakeQueue{queued: true})
		slog.New(h).LogAttrs(context.Background(), slog.LevelError, "ALERT: webhook delivery abandoned after 24 hours",
			slog.String("endpoint", ep), slog.String("event", hex32))
		p := pending(h)
		if len(p) != 1 || p[0].Fields["endpoint"] != ep {
			t.Fatalf("fields %+v", p)
		}
		return FieldsJSON(p[0].Fields)
	}
	a, b := fields("logger"), fields("books")
	if !strings.Contains(a, `"endpoint":"logger"`) {
		t.Fatalf("fields %s", a)
	}
	at := time.Unix(1_800_000_000, 0)
	const msg = "ALERT: webhook delivery abandoned after 24 hours"
	if store.AlertID(msg, a, at) == store.AlertID(msg, b, at) {
		t.Fatal("two endpoints, one alert id")
	}
	clk := &clock{}
	clk.set(at)
	st := openStore(t, filepath.Join(t.TempDir(), "s.db"), clk)
	if _, err := st.AddAlert(context.Background(), store.Alert{Message: msg, Fields: a}); err != nil {
		t.Fatal(err)
	}
}
