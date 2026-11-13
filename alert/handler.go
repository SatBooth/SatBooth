// Package alert offers every ALERT: log line to the operator's alert queue (plan 10).
//
// A Handler wraps the service's slog.Handler. Every record is passed to the wrapped handler unchanged; a record whose
// message starts with "ALERT:" is also offered, without blocking, to a channel of QueueSize entries, with its
// attributes filtered by a fixed allowlist of keys and value shapes. One goroutine (Run) drains the channel into a
// Queue: the store, which coalesces repeats by hour and bounds the alerts of a day. The handler never writes to the
// store inside a log call, and nothing the package logs carries the ALERT: prefix, so no alert can raise another.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/satbooth/satbooth/store"
)

const (
	// Prefix starts the message of every line a person must act on.
	Prefix = "ALERT:"
	// QueueSize is the capacity of the in-memory channel between the handler and the drain.
	QueueSize = 256

	// LogOverflow is logged, with the count, when alerts were lost because the channel was full.
	LogOverflow = "alert queue overflow"
	// LogFull is logged once a UTC day when the store refuses an alert for the daily bound.
	LogFull = "alert queue full; alerts are logged only until tomorrow"
	// LogWriteFailed is logged, with the count, when alerts could not be written to the store. The error's text is
	// never logged: it can carry a driver's message.
	LogWriteFailed = "alert write failed"

	writeTimeout = 5 * time.Second
	maxWord      = 40
	// maxUint is the largest non-negative integer field: 2^53, the largest every JSON consumer reads exactly.
	maxUint = 1 << 53
)

// flushTimeout bounds everything Run still writes after its stop, as a whole, so a slow store cannot hold the shutdown
// (and the data-directory lock) for a write timeout per queued alert. A variable for the tests.
var flushTimeout = 10 * time.Second

// Alert is one queued alert: the message, the allowed fields (integers as their decimal digits), and how many
// attributes were dropped.
type Alert struct {
	Message string
	Fields  map[string]string
	Dropped int
}

// Queue stores alerts. queued is false when the alert was refused for the daily bound.
type Queue interface {
	Add(ctx context.Context, a Alert) (queued bool, err error)
}

// shape is the form an allowed key's value must have.
type shape int

const (
	shapeHex32 shape = iota + 1 // 32 lowercase hex characters: an opaque id
	shapeUint                   // an integer from 0 to 2^53
	shapeInt32                  // a signed integer from -2^31 to 2^31-1
	shapeWord                   // [a-z][a-z0-9_]*, 1 to 40 characters
	shapeUpper                  // exactly 3 uppercase ASCII letters: an asset or a currency
	shapeName                   // 1 to 64 characters of [A-Za-z0-9._-]: the name of a webhook endpoint
)

// allowed is the allowlist: the keys the ALERT call sites log, each with its shape. Any other key (providers among
// them) is dropped.
var allowed = map[string]shape{
	"order": shapeHex32, "order_id": shapeHex32, "refund_id": shapeHex32, "event": shapeHex32,
	"count": shapeUint, "step": shapeUint, "from_height": shapeUint, "to_height": shapeUint, "unused": shapeUint, "passes": shapeUint,
	"code":   shapeInt32,
	"reason": shapeWord, "method": shapeWord, "result": shapeWord, "low": shapeWord, "high": shapeWord,
	"asset": shapeUpper, "currency": shapeUpper,
	"endpoint": shapeName,
}

// Handler is the slog.Handler wrapper. Handlers made by WithAttrs and WithGroup share the channel and the counters.
type Handler struct {
	next    slog.Handler
	attrs   []slog.Attr // from WithAttrs before any WithGroup
	dropped int         // attributes from WithAttrs after a WithGroup
	grouped bool        // a WithGroup is open: every later attribute is dropped
	c       *core
}

type core struct {
	q        Queue
	now      func() time.Time
	ch       chan Alert
	overflow atomic.Int64
	log      *slog.Logger // logs to the wrapped handler, never through a Handler
	fullDay  int64        // the UTC day LogFull was last logged, plus 1 (0: never); only the drain touches it
}

// NewHandler returns a Handler that passes every record to next and offers each ALERT: record to q, through Run. now
// is the clock of the once-a-day LogFull line.
func NewHandler(next slog.Handler, q Queue, now func() time.Time) *Handler {
	return &Handler{next: next, c: &core{q: q, now: now, ch: make(chan Alert, QueueSize), log: slog.New(next)}}
}

// Enabled is the wrapped handler's: a record the log does not take is not queued either.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

// Handle passes r to the wrapped handler, then offers it to the channel if its message starts with Prefix. It never
// blocks: when the channel is full the alert is counted, and the drain logs LogOverflow with the count.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	err := h.next.Handle(ctx, r)
	if strings.HasPrefix(r.Message, Prefix) {
		attrs := make([]slog.Attr, 0, len(h.attrs)+r.NumAttrs())
		attrs = append(attrs, h.attrs...)
		dropped := h.dropped
		r.Attrs(func(a slog.Attr) bool {
			if h.grouped {
				dropped++
			} else {
				attrs = append(attrs, a)
			}
			return true
		})
		fields, d := filter(attrs)
		select {
		case h.c.ch <- Alert{Message: r.Message, Fields: fields, Dropped: dropped + d}:
		default:
			h.c.overflow.Add(1)
		}
	}
	return err
}

// WithAttrs returns a Handler whose records carry attrs, as next.WithAttrs does.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.next = h.next.WithAttrs(attrs)
	if h.grouped {
		n.dropped += len(attrs)
	} else {
		n.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	}
	return &n
}

// WithGroup returns a Handler whose later attributes are inside the group: none of them is a field.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.next = h.next.WithGroup(name)
	n.grouped = true
	return &n
}

// filter returns the attributes that are on the allowlist with a value of their key's shape, and the number of the
// others. The first attribute of a key wins; a repeated key is dropped. A value is resolved (LogValuer), then only
// string and integer kinds are read: nothing else is ever turned into text.
func filter(attrs []slog.Attr) (map[string]string, int) {
	fields := map[string]string{}
	dropped := 0
	for _, a := range attrs {
		sh, ok := allowed[a.Key]
		if _, dup := fields[a.Key]; !ok || dup {
			dropped++
			continue
		}
		v, ok := value(sh, a.Value.Resolve())
		if !ok {
			dropped++
			continue
		}
		fields[a.Key] = v
	}
	return fields, dropped
}

func value(sh shape, v slog.Value) (string, bool) {
	switch sh {
	case shapeHex32:
		s, ok := str(v)
		return s, ok && isHex32(s)
	case shapeWord:
		s, ok := str(v)
		return s, ok && isWord(s)
	case shapeUpper:
		s, ok := str(v)
		return s, ok && isUpper3(s)
	case shapeName:
		s, ok := str(v)
		return s, ok && store.ValidEndpoint(s)
	case shapeUint:
		switch v.Kind() {
		case slog.KindInt64:
			if n := v.Int64(); n >= 0 && n <= maxUint {
				return strconv.FormatInt(n, 10), true
			}
		case slog.KindUint64:
			if n := v.Uint64(); n <= maxUint {
				return strconv.FormatUint(n, 10), true
			}
		}
	case shapeInt32:
		switch v.Kind() {
		case slog.KindInt64:
			if n := v.Int64(); n >= math.MinInt32 && n <= math.MaxInt32 {
				return strconv.FormatInt(n, 10), true
			}
		case slog.KindUint64:
			if n := v.Uint64(); n <= math.MaxInt32 {
				return strconv.FormatUint(n, 10), true
			}
		}
	}
	return "", false
}

func str(v slog.Value) (string, bool) {
	if v.Kind() != slog.KindString {
		return "", false
	}
	return v.String(), true
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isWord(s string) bool {
	if len(s) < 1 || len(s) > maxWord || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func isUpper3(s string) bool {
	return len(s) == 3 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z' && s[2] >= 'A' && s[2] <= 'Z'
}

// FieldsJSON is the fields as a JSON object with sorted keys: the bytes the alert's id is derived from and its payload
// carries. A value of an integer key that is a canonical integer is written as a JSON number, everything else as a
// JSON string.
func FieldsJSON(fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(jsonString(k))
		b.WriteByte(':')
		v := fields[k]
		if sh := allowed[k]; (sh == shapeUint || sh == shapeInt32) && canonicalInt(v) {
			b.WriteString(v)
		} else {
			b.Write(jsonString(v))
		}
	}
	b.WriteByte('}')
	return b.String()
}

// jsonString is s as a JSON string, without HTML escaping: the bytes the id hashes are the bytes the payload carries.
func jsonString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// canonicalInt reports whether s is what strconv.FormatInt or FormatUint writes.
func canonicalInt(s string) bool {
	d := strings.TrimPrefix(s, "-")
	if d == "" || (len(d) > 1 && d[0] == '0') || (d == "0" && s != "0") {
		return false
	}
	for i := 0; i < len(d); i++ {
		if d[i] < '0' || d[i] > '9' {
			return false
		}
	}
	return true
}

// Run drains the channel into the queue until ctx is done, then writes what is left in the channel (so an alert logged
// during the shutdown still gets its row) and returns ctx.Err(). Run must run in one goroutine only, and must have
// returned before the store closes.
//
// Every write runs under wctx: a write already started is not cut short by the stop, but from the stop on, all that is
// left (the in-flight batch and the final flush alike) shares one deadline, flushTimeout after the stop. The stop is
// checked before every receive, so it never waits behind a random select.
func (h *Handler) Run(ctx context.Context) error {
	wctx, cancelW := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelW()
	// After the stop, end wctx flushTimeout later. A timer still pending when Run returns only cancels a context
	// nobody uses any more.
	timeout := flushTimeout
	stopTimer := context.AfterFunc(ctx, func() { time.AfterFunc(timeout, cancelW) })
	defer stopTimer()
	for {
		if ctx.Err() != nil {
			h.c.flush(wctx)
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
		case a := <-h.c.ch:
			h.c.write(wctx, a)
		}
	}
}

// write stores first and then whatever else is already in the channel, and logs the outcome of the batch. Each write
// has writeTimeout within ctx; once ctx is done the rest of the batch counts as failed writes, untried.
func (c *core) write(ctx context.Context, first Alert) {
	failed := 0
	c.add(ctx, first, &failed)
	for {
		select {
		case a := <-c.ch:
			if ctx.Err() != nil {
				failed++
			} else {
				c.add(ctx, a, &failed)
			}
			continue
		default:
		}
		break
	}
	if failed > 0 {
		c.log.Error(LogWriteFailed, "count", failed)
	}
	if n := c.overflow.Swap(0); n > 0 {
		c.log.Error(LogOverflow, "count", n)
	}
}

// flush writes what is in the channel now, if anything.
func (c *core) flush(ctx context.Context) {
	select {
	case a := <-c.ch:
		c.write(ctx, a)
	default:
		if n := c.overflow.Swap(0); n > 0 {
			c.log.Error(LogOverflow, "count", n)
		}
	}
}

func (c *core) add(ctx context.Context, a Alert, failed *int) {
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	queued, err := c.q.Add(wctx, a)
	switch {
	case err != nil:
		*failed++
	case !queued:
		if day := c.now().Unix()/86400 + 1; day != c.fullDay {
			c.fullDay = day
			c.log.Warn(LogFull)
		}
	}
}

// storeQueue is the Queue over the store.
type storeQueue struct{ st *store.Store }

// NewStoreQueue returns the Queue that stores each alert with store.AddAlert, its fields as FieldsJSON.
func NewStoreQueue(st *store.Store) Queue { return storeQueue{st} }

func (q storeQueue) Add(ctx context.Context, a Alert) (bool, error) {
	return q.st.AddAlert(ctx, store.Alert{Message: a.Message, Fields: FieldsJSON(a.Fields), Dropped: a.Dropped})
}
