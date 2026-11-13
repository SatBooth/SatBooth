package webhook

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/satbooth/satbooth/rates"
	"github.com/satbooth/satbooth/store"
)

const (
	// RetryWindow is how long after an event is created the deliverer keeps trying.
	RetryWindow = 24 * time.Hour
	// GiveUpMessage is the fixed log line written once, at the error level, when the last attempt
	// inside RetryWindow has failed. The event id is its only attribute. It starts with ALERT, like
	// every line the operator should look at (threat model §9).
	GiveUpMessage = "ALERT: webhook delivery abandoned after 24 hours"
	// MalformedQuoteMessage is the fixed line logged at the error level for an event whose order's stored quote does
	// not read back (a row written past SatBooth): the event is held back (nothing sent, nothing recorded, it stays
	// pending) and the pass goes on. It is logged once per event per process; later passes log heldBackAgain at the
	// debug level. The event id is its only attribute; nothing of the quote is logged.
	MalformedQuoteMessage = "ALERT: a webhook event is held back: its order's stored quote is malformed"
	// heldBackAgain is the debug line of the later passes over a held-back event. It carries no ALERT, so that a
	// debug log does not repeat the alert at every pass.
	heldBackAgain = "webhook event still held back: its order's stored quote is malformed"
	// ClearFailedMessage is the fixed line logged at the error level when the mark "abandoned, not yet announced" of an
	// event could not be removed after its give-up line was logged. It carries no data from the error and no ALERT:
	// prefix. The pass goes on; the mark is removed by a later pass, or announced once more after a restart.
	ClearFailedMessage = "webhook give-up mark could not be removed; it will be retried"
	// AlertGiveUpMessage is the alert deliverer's GiveUpMessage. It carries no ALERT: prefix, so that a lost alert
	// never raises another alert (plan 10); the alert id is its only attribute.
	AlertGiveUpMessage = "alert delivery abandoned after 24 hours"

	requestTimeout = 10 * time.Second
	maxResponse    = 4 << 10
	// pollInterval is the shortest gap between the starts of two passes of one deliverer, and so the wait after a pass
	// that failed or was cut short by a receiver that gave no response.
	pollInterval = time.Second
	// fallbackInterval is the longest a deliverer waits between two passes, whatever it was told: the store's wake-up
	// says when to look, and a pass lists everything from the store, so a wake-up that never came (or a clock moved
	// forward while a timer ran) costs at most this long.
	fallbackInterval = 30 * time.Second
	// scanMargin is how far back past RetryWindow the deliverer still looks, so that an event whose
	// window ended while SatBooth was down still gets its final attempt and its give-up line.
	scanMargin = 7 * 24 * time.Hour
	minSecret  = 32
	maxSecret  = 64
	// maxHeldBack bounds heldBackRecord.seen; when full it is cleared (the worst case is one more ALERT per event).
	maxHeldBack = 4096
)

// RetryDelay is the wait before the next attempt after failures failed attempts: 10 s, doubling,
// up to 1 h.
func RetryDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	if failures > 9 { // 10 s << 9 is already over an hour
		return time.Hour
	}
	return min(10*time.Second<<(failures-1), time.Hour)
}

// Item is one undelivered item of a Source, with the facts the retry policy needs and the way to build its body.
type Item struct {
	ID        string
	CreatedAt time.Time
	Failures  int       // delivery attempts recorded, none successful
	LastAt    time.Time // the latest attempt; the zero time if there is none
	// Body returns the request body. ErrHeldBack holds this item back: nothing is sent or recorded, and the pass goes
	// on. Any other error ends the pass.
	Body func(ctx context.Context) ([]byte, error)
}

// ErrHeldBack is the error of an Item's Body that holds the item back for this pass.
var ErrHeldBack = errors.New("webhook: item held back")

// Source is what a Deliverer delivers: the merchant's events, or the operator's alerts. It states facts only; the
// retry policy is the Deliverer's.
type Source interface {
	// Undelivered lists the items created at or after since with no successful attempt, oldest first.
	Undelivered(ctx context.Context, since time.Time) ([]Item, error)
	// AddAttempt records one attempt of the item id. final says that no attempt follows (the deadline has passed): a
	// failed final attempt of an event is also marked "abandoned, not yet announced" in the same write (an announcer).
	AddAttempt(ctx context.Context, id string, at time.Time, status int, ok, final bool) error
}

// announcer is implemented by the Sources whose give-up line must survive a crash (the events'): the record of the last
// failed attempt and the mark that its line is owed are one write, so the line is logged at least once, across a crash
// or a restart at any point.
type announcer interface {
	// Abandoned lists the items marked "abandoned, not yet announced", oldest first.
	Abandoned(ctx context.Context) ([]string, error)
	// Announced removes the mark of the item id, once its line is logged.
	Announced(ctx context.Context, id string) error
}

// lines are a Deliverer's fixed log lines and the key of the item id they carry.
type lines struct {
	giveUp, retry, passFailed, idKey string
}

var (
	eventLines = lines{giveUp: GiveUpMessage, retry: "webhook delivery failed, will retry", passFailed: "webhook pass failed", idKey: "event"}
	alertLines = lines{giveUp: AlertGiveUpMessage, retry: "alert delivery failed, will retry", passFailed: "alert pass failed", idKey: "alert"}
)

// Deliverer posts each undelivered item of its Source to one fixed URL. Run (or RunOnce) must not be called
// from more than one goroutine, or from more than one process on the same store.
type Deliverer struct {
	url    string
	secret []byte
	src    Source
	now    func() time.Time
	client *http.Client
	log    *slog.Logger
	// interval is the fallback: the longest wait between two passes. gap is the shortest time between the starts of
	// two passes. watch subscribes to the store's wake-ups (nil: none, only the timers drive Run). All are set when
	// the Deliverer is built and never change.
	interval time.Duration
	gap      time.Duration
	watch    func() (<-chan struct{}, func())
	lines    lines
	// unrecorded is the posts of this process the receiver answered with a 2xx and that could not be recorded, by item
	// id: a later pass records them without posting again. announced is the ids whose give-up line this process has
	// logged and whose mark could not be removed, so that the line is not logged twice in one run. Only the pass
	// touches them (Run and RunOnce are never concurrent).
	unrecorded map[string]unrecordedPost
	announced  map[string]struct{}
}

// unrecordedPost is a 2xx answer awaiting its record: the time and status of the post. The entry is kept until the
// record succeeds or the item is no longer in the endpoint's listing.
type unrecordedPost struct {
	at     time.Time
	status int
}

// Option changes a Deliverer while it is built. Its function is unexported, so only the constructors can apply one.
type Option struct{ apply func(*Deliverer) }

// use applies the option; the zero Option, which any package can write, changes nothing.
func (o Option) use(d *Deliverer) {
	if o.apply != nil {
		o.apply(d)
	}
}

// WithIntervals is for tests: the longest wait between two passes (fallback) and the shortest time between the starts
// of two (gap). A value of zero or less keeps the default.
func WithIntervals(fallback, gap time.Duration) Option {
	return Option{func(d *Deliverer) {
		if fallback > 0 {
			d.interval = fallback
		}
		if gap > 0 {
			d.gap = gap
		}
	}}
}

// Endpoint is one receiver of the merchant's events.
type Endpoint struct {
	Name   string
	URL    string
	Secret []byte
}

// NewEventDeliverers returns one Deliverer per endpoint, in the order given. Each delivers every event of the store to
// its own URL with its own secret and records its attempts under its name. They share one record of the events held
// back in this run, so "a webhook event is held back" is logged once per event, not once per endpoint. A URL must be an
// http or https URL with a host and no user info or fragment, a secret 32 to 64 bytes (see LoadSecret), a name 1 to 64
// characters of [A-Za-z0-9._-], unique in the list. client may be nil; a copy is used with a 10 s timeout at most and
// redirects switched off (a redirect is a failed attempt, never followed). Errors name the endpoint (or its position,
// if the name itself is bad) and never contain a URL or a secret. Nothing is written to the store until a deliverer
// runs.
func NewEventDeliverers(eps []Endpoint, st *store.Store, now func() time.Time, client *http.Client, log *slog.Logger, opts ...Option) ([]*Deliverer, error) {
	if st == nil || now == nil {
		return nil, errors.New("webhook: a store and a clock are required")
	}
	if len(eps) == 0 {
		return nil, errors.New("webhook: at least one endpoint is required")
	}
	log = orDiscard(log)
	held := &heldBackRecord{seen: map[string]struct{}{}}
	names := make(map[string]struct{}, len(eps))
	out := make([]*Deliverer, 0, len(eps))
	for i, ep := range eps {
		if !store.ValidEndpoint(ep.Name) {
			return nil, fmt.Errorf("webhook: endpoints[%d]: the name must be 1 to 64 characters of A-Z a-z 0-9 . _ -", i)
		}
		if _, dup := names[ep.Name]; dup {
			return nil, fmt.Errorf("webhook: endpoint %q: the name is used twice", ep.Name)
		}
		names[ep.Name] = struct{}{}
		src := &eventSource{st: st, now: now, log: log, endpoint: ep.Name, held: held}
		d, err := newDeliverer(ep.URL, ep.Secret, src, now, client, log.With("endpoint", ep.Name), eventLines)
		if err != nil {
			return nil, fmt.Errorf("webhook: endpoint %q: %s", ep.Name, strings.TrimPrefix(err.Error(), "webhook: "))
		}
		d.watch = st.WatchEvents
		for _, opt := range opts {
			opt.use(d)
		}
		out = append(out, d)
	}
	return out, nil
}

// NewAlertDeliverer returns the operator's Deliverer, of the store's alerts (plan 10), with NewEventDeliverers' rules
// for a URL and a secret and its retry policy. Its lines carry no ALERT: prefix; it gives up with AlertGiveUpMessage.
func NewAlertDeliverer(rawURL string, secret []byte, st *store.Store, now func() time.Time, client *http.Client, log *slog.Logger, opts ...Option) (*Deliverer, error) {
	if st == nil {
		return nil, errors.New("webhook: a store and a clock are required")
	}
	d, err := newDeliverer(rawURL, secret, alertSource{st}, now, client, orDiscard(log), alertLines)
	if err != nil {
		return nil, err
	}
	d.watch = st.WatchAlerts
	for _, opt := range opts {
		opt.use(d)
	}
	return d, nil
}

func orDiscard(log *slog.Logger) *slog.Logger {
	if log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return log
}

func newDeliverer(rawURL string, secret []byte, src Source, now func() time.Time, client *http.Client, log *slog.Logger, ln lines) (*Deliverer, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("webhook: the URL must be an http or https URL with a host, without user info or fragment")
	}
	for _, c := range rawURL {
		if c <= ' ' || c == 0x7f {
			return nil, errors.New("webhook: the URL must not contain spaces or control characters")
		}
	}
	if n := len(secret); n < minSecret || n > maxSecret {
		return nil, fmt.Errorf("webhook: the secret must be %d to %d bytes", minSecret, maxSecret)
	}
	if now == nil {
		return nil, errors.New("webhook: a store and a clock are required")
	}
	c := http.Client{}
	if client != nil {
		c = *client
	}
	if c.Timeout <= 0 || c.Timeout > requestTimeout {
		c.Timeout = requestTimeout
	}
	// The only destination is the configured URL: an environment proxy must not reroute it.
	switch tr := c.Transport.(type) {
	case nil:
		// A host program may have replaced http.DefaultTransport with another RoundTripper: then a
		// transport of our own, never theirs.
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			base = &http.Transport{}
		}
		c.Transport = noProxyTransport(base)
	case *http.Transport:
		c.Transport = noProxyTransport(tr)
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Deliverer{url: u.String(), secret: bytes.Clone(secret), src: src, now: now, client: &c, log: log,
		interval: fallbackInterval, gap: pollInterval, lines: ln,
		unrecorded: map[string]unrecordedPost{}, announced: map[string]struct{}{}}, nil
}

func noProxyTransport(base *http.Transport) *http.Transport {
	t := base.Clone()
	t.Proxy = nil
	// One request at a time at a low rate: no idle connection stays open, because net/http logs the first bytes a
	// server sends on an idle connection to the standard logger, and a receiver could put its own URL's token there.
	t.DisableKeepAlives = true
	return t
}

// Run delivers pending items until ctx is done. It returns ctx.Err(). It subscribes to the store's wake-ups, then
// loops: one pass; then a wait for the first of a wake-up, the time the pass said the next item falls due (at most
// the fallback away) and the fallback; and never a pass starting less than the gap after the previous one started. A
// store error is logged and the pass tried again after the gap. Every pass reads everything from the store: a wake-up
// or a timer says when to look, never what to send, so a wake-up that is missed costs time, not a delivery. Every wait
// ends when ctx is done.
func (d *Deliverer) Run(ctx context.Context) error {
	interval, gap := d.interval, d.gap
	var wake <-chan struct{}
	if d.watch != nil {
		ch, stop := d.watch()
		defer stop()
		wake = ch
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		started := time.Now()
		// The wake-up is taken before the pass lists: an item stored after the listing leaves one behind.
		select {
		case <-wake:
		default:
		}
		wait := interval
		next, err := d.pass(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d.log.Error(d.lines.passFailed, "err", err)
			wait = 0
		} else if !next.IsZero() {
			wait = min(max(next.Sub(d.now()), 0), interval)
		}
		if !sleep(ctx, timer, wait, wake) {
			return ctx.Err()
		}
		if rest := gap - time.Since(started); rest > 0 && !sleep(ctx, timer, rest, nil) {
			return ctx.Err()
		}
	}
}

// sleep waits for wait, or for a value on wake if it is not nil, and reports false if ctx ended first.
func sleep(ctx context.Context, timer *time.Timer, wait time.Duration, wake <-chan struct{}) bool {
	timer.Reset(wait)
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-timer.C:
		return true
	}
}

// RunOnce makes one pass: every item with no successful attempt that is due gets one attempt.
//
// Policy (the store only reports facts): an item is due at once if it has no attempt, otherwise
// RetryDelay(failures) after its last attempt, but no later than RetryWindow after it was created.
// The attempt made at or after that deadline is the last: if it fails the deliverer logs
// its give-up line (GiveUpMessage, or AlertGiveUpMessage) and the item is never listed as due again. That line is
// owed from the store, not from memory: for an event, the last failed attempt and a mark "abandoned, not yet
// announced" are written together, and a pass logs the line of every marked event (so does the first pass after a
// start) and then removes the mark. A crash between the two repeats the line after the restart (once), and never
// loses it; within one run it is logged once, even if removing the mark fails.
func (d *Deliverer) RunOnce(ctx context.Context) error {
	_, err := d.pass(ctx)
	return err
}

// pass is RunOnce's pass. It also returns the earliest time at which an item falls due again, for Run's timer: the due
// time of an item skipped as not yet due, the time of the next retry (at most the deadline) of an item whose attempt
// failed before its deadline, and now if the pass was cut short by a receiver that gave no response. It is the zero
// time when nothing waits. A delivered item, an abandoned one and a held-back one never set it: a held-back item is
// tried again at the next wake-up or fallback, and must not make Run spin.
func (d *Deliverer) pass(ctx context.Context) (next time.Time, err error) {
	soonest := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	now := d.now()
	if err := d.announcePending(ctx); err != nil {
		return next, err
	}
	pending, err := d.src.Undelivered(ctx, now.Add(-RetryWindow-scanMargin))
	if err != nil {
		return next, err
	}
	d.dropUnlistedUnrecorded(pending)
	for _, p := range pending {
		if err := ctx.Err(); err != nil {
			return next, err
		}
		if u, ok := d.unrecorded[p.ID]; ok {
			// The receiver answered 2xx and the record failed: only the record is retried, never the post.
			if err := d.record(ctx, p.ID, u.at, u.status, true, false); err != nil {
				return next, err
			}
			delete(d.unrecorded, p.ID)
			continue
		}
		deadline := p.CreatedAt.Add(RetryWindow)
		if p.Failures > 0 && !p.LastAt.Before(deadline) {
			continue // gave up already
		}
		if now = d.now(); p.Failures > 0 {
			due := p.LastAt.Add(RetryDelay(p.Failures))
			if due.After(deadline) {
				due = deadline
			}
			if now.Before(due) {
				soonest(due)
				continue
			}
		}
		out, err := d.attempt(ctx, p, deadline)
		if err != nil {
			return next, err
		}
		if out.failed && out.at.Before(deadline) {
			soonest(minTime(out.at.Add(RetryDelay(p.Failures+1)), deadline))
		}
		if out.noResponse {
			// A hung or unreachable receiver would stall every other event for 10 s each:
			// end the pass; the next one resumes from the store.
			soonest(d.now())
			return next, nil
		}
	}
	return next, nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// payload is the body of a delivery. Pricing is present only for a priced order, so an explicit order's body is the
// five members it always had, byte for byte.
type payload struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Seq       int             `json:"seq"`
	OrderID   string          `json:"order_id"`
	CreatedAt int64           `json:"created_at"`
	Pricing   *pricingPayload `json:"pricing,omitempty"`
}

// pricingPayload is the recorded quote of a priced order: the members of the API's pricing object, with the times in
// Unix seconds like created_at. rates has one member per asset priced ("BTC", "XMR"); encoding/json writes a map's
// keys sorted, so the body is the same bytes for the same quote.
type pricingPayload struct {
	Price         string                      `json:"price"`
	Currency      string                      `json:"currency"`
	MarginPercent string                      `json:"margin_percent"`
	QuotedAt      int64                       `json:"quoted_at"`
	Rates         map[string]assetRatePayload `json:"rates"`
}

type assetRatePayload struct {
	Rate    string          `json:"rate"`
	Sources []sourcePayload `json:"sources"`
}

type sourcePayload struct {
	Provider string `json:"provider"`
	Rate     string `json:"rate"`
	At       int64  `json:"at"`
}

func newPricingPayload(q store.Quote) *pricingPayload {
	pp := &pricingPayload{Price: q.Price, Currency: q.Currency, MarginPercent: rates.FormatPercent(q.MarginBps), QuotedAt: q.QuotedAt.Unix(),
		Rates: make(map[string]assetRatePayload, len(q.Rates))}
	for _, r := range q.Rates {
		a := assetRatePayload{Rate: r.Rate, Sources: make([]sourcePayload, len(r.Sources))}
		for i, s := range r.Sources {
			a.Sources[i] = sourcePayload{Provider: s.Provider, Rate: s.Rate, At: s.At.Unix()}
		}
		pp.Rates[r.Asset] = a
	}
	return pp
}

// outcome is what attempt did. A held-back item sends and records nothing (the zero outcome). failed is a recorded
// attempt that was not a 2xx, at is its time, and noResponse is a failed one the receiver gave no response to.
type outcome struct {
	failed, noResponse bool
	at                 time.Time
}

// attempt makes and records one attempt.
func (d *Deliverer) attempt(ctx context.Context, p Item, deadline time.Time) (outcome, error) {
	// A failed read sends and records nothing: the item is tried again, never sent incomplete. A held-back item is
	// skipped alone; any other error ends the pass.
	body, err := p.Body(ctx)
	if errors.Is(err, ErrHeldBack) {
		return outcome{}, nil
	}
	if err != nil {
		return outcome{}, err
	}
	at := d.now()
	status, ok := d.post(ctx, at, body)
	if ctx.Err() != nil && status == 0 {
		return outcome{}, ctx.Err() // shutting down mid-request: the outcome is unknown, so nothing is recorded
	}
	final := !ok && !at.Before(deadline)
	if err := d.record(ctx, p.ID, at, status, ok, final); err != nil {
		if ok {
			// Remember the 2xx: the next passes record it without posting again (until a restart, or until the item
			// leaves the listing).
			d.unrecorded[p.ID] = unrecordedPost{at: at, status: status}
		}
		return outcome{}, err
	}
	if ok {
		return outcome{at: at}, nil
	}
	if final {
		if _, durable := d.src.(announcer); durable {
			d.announce(ctx, p.ID)
		} else {
			d.log.Error(d.lines.giveUp, d.lines.idKey, p.ID)
		}
	} else {
		d.log.Info(d.lines.retry, d.lines.idKey, p.ID, "status", status)
	}
	return outcome{failed: true, noResponse: status == 0, at: at}, nil
}

// record writes one attempt. A 2xx must be recorded even if shutdown starts now, or the item would be sent again.
func (d *Deliverer) record(ctx context.Context, id string, at time.Time, status int, ok, final bool) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return d.src.AddAttempt(rctx, id, at, status, ok, final)
}

// dropUnlistedUnrecorded drops the entries of the items that are not in the listing: delivered by a record that did
// commit after all, or past the scan window for good. Never by age alone: the last attempt and the first one after a
// long downtime are made at or after the 24-hour limit, and a 2xx there must be recorded, not posted again. So the map
// holds only undelivered, listed items.
func (d *Deliverer) dropUnlistedUnrecorded(listed []Item) {
	if len(d.unrecorded) == 0 {
		return
	}
	in := make(map[string]struct{}, len(listed))
	for _, p := range listed {
		in[p.ID] = struct{}{}
	}
	for id := range d.unrecorded {
		if _, ok := in[id]; !ok {
			delete(d.unrecorded, id)
		}
	}
}

// announcePending logs the give-up line of every item the store marks "abandoned, not yet announced" for this
// deliverer: those whose attempt was recorded by a process that died before its line, and, in the first pass after a
// start, any of them. Sources that are not announcers have none.
func (d *Deliverer) announcePending(ctx context.Context) error {
	a, ok := d.src.(announcer)
	if !ok {
		return nil
	}
	ids, err := a.Abandoned(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		d.announce(ctx, id)
	}
	return nil
}

// announce logs the give-up line of the item id and removes its mark. The line is logged at most once per run: if
// removing the mark fails, ClearFailedMessage is logged (nothing of the error in it), the id is remembered (until it
// succeeds) and a later pass only retries the removal; the pass goes on, so the endpoint's deliveries never wait for it.
// If the process dies between the line and the removal, the next run logs the line again: a duplicate, never a loss.
func (d *Deliverer) announce(ctx context.Context, id string) {
	if _, done := d.announced[id]; !done {
		d.log.Error(d.lines.giveUp, d.lines.idKey, id)
		d.announced[id] = struct{}{}
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := d.src.(announcer).Announced(rctx, id); err != nil {
		d.log.Error(ClearFailedMessage, d.lines.idKey, id)
		return
	}
	delete(d.announced, id)
}

// heldBackRecord is the events whose MalformedQuoteMessage this process logged, shared by the event sources of all
// endpoints and guarded by its mutex.
type heldBackRecord struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// first reports whether the event is new to the record, and records it. When the record is full it is cleared (the
// worst case is one more ALERT per event).
func (h *heldBackRecord) first(eventID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, seen := h.seen[eventID]; seen {
		return false
	}
	if len(h.seen) >= maxHeldBack {
		clear(h.seen)
	}
	h.seen[eventID] = struct{}{}
	return true
}

// eventSource is the Source of the merchant's webhook events for one endpoint.
type eventSource struct {
	st       *store.Store
	now      func() time.Time
	log      *slog.Logger // without the endpoint attribute: the held-back lines are the event's, not the endpoint's
	endpoint string
	held     *heldBackRecord
	// firstPass is the time of the endpoint's first delivery pass, once registered. Only the pass touches it (Run and
	// RunOnce are never concurrent), so it needs no lock.
	firstPass  time.Time
	registered bool
}

// Undelivered lists fewer items than the Source contract's "created at or after since": the admitted ones. An event is
// admitted if it was created at or after min(first pass, now) - RetryWindow, or if it already has an attempt for this
// endpoint. So an endpoint new to the store is sent the events of the last 24 hours, not a week of old ones, while an
// event once tried stays listed until it is delivered or abandoned, whatever the floor does. The floor is capped by
// now because the first pass is written once: if a clock that ran ahead stamped it, the floor taken from it alone would
// hide, for good and without a line, events that are inside their own 24 hours by the current clock. since is the scan
// bound and passes through unchanged. The first call registers the endpoint; if that fails, so does the call, and the
// next pass tries again.
func (e *eventSource) Undelivered(ctx context.Context, since time.Time) ([]Item, error) {
	if !e.registered {
		fp, err := e.st.RegisterEndpoint(ctx, e.endpoint)
		if err != nil {
			return nil, err
		}
		e.firstPass, e.registered = fp, true
	}
	anchor := e.firstPass
	if now := e.now(); now.Before(anchor) {
		anchor = now
	}
	admit := anchor.Add(-RetryWindow)
	pending, err := e.st.UndeliveredEvents(ctx, e.endpoint, since, admit)
	if err != nil {
		return nil, err
	}
	items := make([]Item, len(pending))
	for i, p := range pending {
		items[i] = Item{ID: p.ID, CreatedAt: p.CreatedAt, Failures: p.Failures, LastAt: p.LastAt,
			Body: func(ctx context.Context) ([]byte, error) { return e.body(ctx, p) }}
	}
	return items, nil
}

func (e *eventSource) AddAttempt(ctx context.Context, id string, at time.Time, status int, ok, final bool) error {
	return e.st.AddAttempt(ctx, store.Attempt{Endpoint: e.endpoint, EventID: id, At: at, Status: status, OK: ok, Final: final})
}

func (e *eventSource) Abandoned(ctx context.Context) ([]string, error) {
	return e.st.PendingAbandoned(ctx, e.endpoint)
}

func (e *eventSource) Announced(ctx context.Context, id string) error {
	return e.st.ClearAbandoned(ctx, e.endpoint, id)
}

// body is the event's payload. A malformed stored quote holds back this event alone, with a fixed line.
func (e *eventSource) body(ctx context.Context, p store.PendingEvent) ([]byte, error) {
	pl := payload{ID: p.ID, Type: p.Type, Seq: p.Seq, OrderID: p.OrderID, CreatedAt: p.CreatedAt.Unix()}
	q, priced, err := e.st.QuoteForOrder(ctx, p.OrderID)
	if errors.Is(err, store.ErrStoredQuote) {
		e.alertHeldBack(p.ID)
		return nil, ErrHeldBack
	}
	if err != nil {
		return nil, err
	}
	if priced {
		pl.Pricing = newPricingPayload(q)
	}
	return json.Marshal(pl)
}

// alertHeldBack logs MalformedQuoteMessage for the event once per process whatever the number of
// endpoints, and heldBackAgain at the debug level at every later pass of any endpoint.
func (e *eventSource) alertHeldBack(eventID string) {
	if !e.held.first(eventID) {
		e.log.Debug(heldBackAgain, "event", eventID)
		return
	}
	e.log.Error(MalformedQuoteMessage, "event", eventID)
}

// alertPayload is the body of an alert (plan 10). fields is the stored JSON object, embedded as it is: the bytes
// the alert's id was derived from.
type alertPayload struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Message   string          `json:"message"`
	Fields    json.RawMessage `json:"fields"`
	Dropped   int             `json:"dropped"`
	Repeats   int             `json:"repeats"`
	CreatedAt int64           `json:"created_at"`
}

// alertSource is the Source of the operator's alerts.
type alertSource struct{ st *store.Store }

func (a alertSource) Undelivered(ctx context.Context, since time.Time) ([]Item, error) {
	pending, err := a.st.UndeliveredAlerts(ctx, since)
	if err != nil {
		return nil, err
	}
	items := make([]Item, len(pending))
	for i, p := range pending {
		// repeats is read with the list, so the payload carries it as it is when this pass sends it.
		pl := alertPayload{ID: p.ID, Type: "alert", Message: p.Message, Fields: json.RawMessage(p.Fields), Dropped: p.Dropped,
			Repeats: p.Repeats, CreatedAt: p.CreatedAt.Unix()}
		items[i] = Item{ID: p.ID, CreatedAt: p.CreatedAt, Failures: p.Failures, LastAt: p.LastAt,
			Body: func(context.Context) ([]byte, error) { return marshalPlain(pl) }}
	}
	return items, nil
}

// marshalPlain is json.Marshal without HTML escaping, so the message and the fields go out as the bytes stored (and,
// for the fields, hashed into the id).
func marshalPlain(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func (a alertSource) AddAttempt(ctx context.Context, id string, at time.Time, status int, ok, _ bool) error {
	return a.st.AddAlertAttempt(ctx, store.AlertAttempt{AlertID: id, At: at, Status: status, OK: ok})
}

// post sends one request and returns the HTTP status (0 if there was no response) and whether it
// was a 2xx. Nothing about the request error is kept: it can carry the URL.
func (d *Deliverer) post(ctx context.Context, at time.Time, body []byte) (int, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "SatBooth-Webhook")
	req.Header.Set(HeaderName, Sign(d.secret, at, body))
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
	return resp.StatusCode, resp.StatusCode >= 200 && resp.StatusCode < 300
}

var errSecretFile = errors.New("webhook: secret file must be a regular file with mode 0600 (no group or other access) holding 32 to 64 bytes as lowercase hex and an optional final newline")

// LoadSecret reads the webhook secret from path: a regular file (symbolic links are refused) with
// no group or other permission bits, holding 64 to 128 lowercase hex digits and at most one final
// newline. Errors never contain the file's content or its path (they are logged at startup): an
// open failure keeps only the operating system's reason, as api's key file does.
func LoadSecret(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("webhook: opening the secret file: " + osReason(err))
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, errors.New("webhook: reading the secret file")
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return nil, errSecretFile
	}
	raw, err := io.ReadAll(io.LimitReader(f, 2*maxSecret+2))
	if err != nil {
		return nil, errors.New("webhook: reading the secret file")
	}
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		raw = raw[:len(raw)-1]
	}
	if len(raw) < 2*minSecret || len(raw) > 2*maxSecret {
		return nil, errSecretFile
	}
	for _, c := range raw {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return nil, errSecretFile
		}
	}
	secret, err := hex.DecodeString(string(raw))
	if err != nil {
		return nil, errSecretFile
	}
	return secret, nil
}

// osReason is the operating system's reason for a failed open, without the path a *os.PathError
// carries.
func osReason(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return "error"
}
