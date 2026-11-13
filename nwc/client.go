package nwc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/satbooth/satbooth/nip44"
	"github.com/satbooth/satbooth/nostr"
)

// Timeout bounds (Config.Timeout) and the fixed client limits.
const (
	DefaultTimeout = 70 * time.Second
	MinTimeout     = 10 * time.Second
	MaxTimeout     = 10 * time.Minute

	// MaxListLimit is the largest ListParams.Limit: a NIP-44 message is capped at 65,535 bytes, and 20
	// transactions with metadata stay under that.
	MaxListLimit = 20

	notifQueue   = 64   // capacity of the Notifications channel
	notifSeenMax = 4096 // event ids remembered to drop a backlog replay
	subID        = "nwc"
	writeTimeout = 10 * time.Second
	pingEvery    = 30 * time.Second
	pingTimeout  = 10 * time.Second
	maxFrame     = nostr.MaxEventJSON + 4096
)

// ErrNoResponse means no usable answer arrived before the call's deadline (Config.Timeout, or the
// caller's context if that ended first). It says nothing about whether the wallet carried the request
// out: the request may never have reached the relay, may have been dropped, or may have been executed
// with the answer lost. A late answer is dropped, never delivered to a later call. See the outcome rule
// in the package documentation.
var ErrNoResponse = errors.New("nwc: no response from the wallet; the request may or may not have been carried out")

// ErrClosed is returned by calls that are pending when Close runs and by every call after it. For a call
// that was pending, the request may already have been delivered: the outcome is unknown, exactly as with
// ErrNoResponse. A call made after Close was never sent.
var ErrClosed = errors.New("nwc: client closed")

// ErrExpiredBeforeSend is returned by PayInvoiceUntil when the expiration it was given is not after the
// client's clock (in whole seconds). The request was neither built nor published: it provably never
// reached the relay.
var ErrExpiredBeforeSend = errors.New("nwc: the request's expiration has passed; it was not sent")

// RelayRefusedError means the relay answered OK false to the request. The relay did not store the
// request, so the wallet cannot have received it through this relay. Reason is the relay's text.
type RelayRefusedError struct{ Reason string }

func (e *RelayRefusedError) Error() string {
	r := e.Reason
	if len(r) > 200 {
		r = r[:200]
	}
	return "nwc: relay refused the request: " + strconv.QuoteToASCII(r)
}

// Config describes one connection: one relay, one (client key, wallet key) pair.
type Config struct {
	// RelayURL is the ws:// or wss:// URL of the client listener. It is signed into the NIP-42 AUTH
	// event as given, so it must equal the relay's configured client_relay_url byte for byte.
	RelayURL string
	// WalletPubKey is the wallet's public key (64 lowercase hex).
	WalletPubKey string
	// Key is the client's secret key.
	Key *nostr.SecretKey
	// Timeout is how long a call waits for the wallet, and the lifetime in the request's expiration tag.
	// 0 means 70 s; otherwise it must be between 10 s and 10 min.
	Timeout time.Duration
	// Log receives fixed-string diagnostics. It never receives message content, keys, invoices or
	// payment ids. Nil discards.
	Log *slog.Logger
}

// tuning holds what tests override. The zero value means the production values.
type tuning struct {
	timeout    time.Duration // bypasses the Config.Timeout bounds
	backoffMin time.Duration
	backoffMax time.Duration
	handshake  time.Duration // dial, AUTH and the first EOSE
	seenMax    int           // notification ids remembered; 0 means 4096
}

// Client is one connection to one relay for one (client key, wallet key) pair. It is safe for
// concurrent use.
//
// # Goroutines and locks
//
// One supervisor goroutine owns the connection: it dials, runs the session's read loop on its own
// stack, and on any failure reconnects with exponential backoff (1 s doubling to 30 s) and repeats the
// AUTH and the REQ. A session also runs a pinger goroutine. Calls run on the callers' goroutines.
//
// mu guards the pending map, the current ready session, closed and the notification dedup set. A
// response, an OK false and Close each claim a pending call by deleting it from the map under mu; the
// claimant is then the only one that completes it, through the call's buffered done channel. A call
// that times out claims itself the same way, and if someone else got there first it takes that
// result. So a call is completed exactly once, and the result that wins is never lost or duplicated.
//
// # A call and a disconnect
//
// A call is registered in the pending map before its EVENT is written, and its EVENT is written at most
// once, whatever happens to the connection. If the connection drops while the call waits, the call keeps
// waiting to its deadline: the REQ the client sends after reconnecting makes the relay replay its
// window, and a response stored while the client was away completes the call. Nothing is resent.
type Client struct {
	url, wallet, me string
	key             *nostr.SecretKey
	conv            [32]byte
	timeout         time.Duration
	tune            tuning
	log             *slog.Logger
	now             func() time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once

	mu      sync.Mutex
	closed  bool
	pending map[string]*call
	sess    *session      // the ready session, nil while connecting
	changed chan struct{} // closed and replaced whenever sess changes
	seen    map[string]struct{}
	seenQ   []string // ring of the ids in seen, oldest overwritten first
	seenN   int      // next slot in seenQ

	notif chan Notification

	// Counters for tests.
	dropped atomic.Int64 // events the client discarded
	ready   atomic.Int64 // sessions that reached ready
}

type call struct {
	method string
	out    any
	done   chan error // buffered 1; written once by whoever claimed the call
}

type session struct {
	ws  *websocket.Conn
	ctx context.Context
}

// Dial connects to the relay, answers its AUTH challenge if it sends one and subscribes. It returns
// when the subscription is live, or with the error that ended the first attempt: it does not retry
// the first connection. After that the client reconnects by itself.
func Dial(ctx context.Context, cfg Config) (*Client, error) { return dial(ctx, cfg, tuning{}) }

func dial(ctx context.Context, cfg Config, tune tuning) (*Client, error) {
	if cfg.Key == nil {
		return nil, errors.New("nwc: no client key")
	}
	if err := nostr.ValidatePubKey(cfg.WalletPubKey); err != nil {
		return nil, fmt.Errorf("nwc: wallet key: %w", err)
	}
	u, err := url.Parse(cfg.RelayURL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("nwc: relay URL must be ws:// or wss:// with a host, and no credentials")
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if tune.timeout != 0 {
		timeout = tune.timeout
	} else if timeout < MinTimeout || timeout > MaxTimeout {
		return nil, errors.New("nwc: timeout must be between 10 s and 10 min")
	}
	if tune.backoffMin == 0 {
		tune.backoffMin = time.Second
	}
	if tune.backoffMax == 0 {
		tune.backoffMax = 30 * time.Second
	}
	if tune.seenMax == 0 {
		tune.seenMax = notifSeenMax
	}
	if tune.handshake == 0 {
		tune.handshake = 10 * time.Second
	}
	conv, err := nip44.ConversationKey(cfg.Key, cfg.WalletPubKey)
	if err != nil {
		return nil, fmt.Errorf("nwc: %w", err)
	}
	logger := cfg.Log
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	c := &Client{
		url: cfg.RelayURL, wallet: cfg.WalletPubKey, me: cfg.Key.PublicKeyHex(), key: cfg.Key, conv: conv,
		timeout: timeout, tune: tune, log: logger, now: time.Now,
		pending: map[string]*call{}, changed: make(chan struct{}),
		seen:  make(map[string]struct{}, tune.seenMax),
		seenQ: make([]string, tune.seenMax),
		notif: make(chan Notification, notifQueue),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	first := make(chan error, 1)
	c.wg.Add(1)
	go c.supervise(first)
	select {
	case err := <-first:
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	case <-ctx.Done():
		_ = c.Close()
		return nil, ctx.Err()
	}
}

// Notifications returns the channel of validated payment notifications. Within one Client
// instance each notification (by event id) is delivered once. Across a process restart delivery is
// at-least-once: the subscription has no "since", so a new Client receives again whatever the relay
// replays from its window (default 3600 s, relay/config.go window_seconds), and a notification is a hint to
// be matched by payment_hash, never counted. The channel holds 64; when it is full the newest notification is dropped and
// a fixed line is logged, so a consumer that stops reading never blocks the client. Close closes it.
func (c *Client) Notifications() <-chan Notification { return c.notif }

// Close ends the connection. Pending calls return ErrClosed. It is safe to call more than once.
func (c *Client) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		claimed := make([]*call, 0, len(c.pending))
		for id, cl := range c.pending {
			claimed = append(claimed, cl)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		for _, cl := range claimed {
			cl.done <- ErrClosed
		}
		c.cancel()
		c.wg.Wait()
		close(c.notif) // the only sender, the read loop, has ended
	})
	return nil
}

// --- supervisor and session ------------------------------------------------------------------------

func (c *Client) supervise(first chan<- error) {
	defer c.wg.Done()
	backoff := c.tune.backoffMin
	for attempt := 0; c.ctx.Err() == nil; attempt++ {
		wasReady, err := c.runSession(func() {
			if attempt == 0 {
				first <- nil
			}
		})
		if attempt == 0 && !wasReady {
			first <- err
		}
		if c.ctx.Err() != nil {
			return
		}
		if wasReady {
			backoff = c.tune.backoffMin
		}
		c.log.Info("relay connection ended", "reconnect_in", backoff.String())
		t := time.NewTimer(backoff)
		select {
		case <-t.C:
		case <-c.ctx.Done():
			t.Stop()
			return
		}
		if backoff *= 2; backoff > c.tune.backoffMax {
			backoff = c.tune.backoffMax
		}
	}
}

// runSession runs one connection from dial to failure. onReady runs once, when the subscription is
// live. It reports whether the session got that far and why it ended.
func (c *Client) runSession(onReady func()) (wasReady bool, err error) {
	dctx, dcancel := context.WithTimeout(c.ctx, c.tune.handshake)
	ws, _, derr := websocket.Dial(dctx, c.url, nil)
	dcancel()
	if derr != nil {
		c.log.Info("relay dial failed")
		return false, fmt.Errorf("nwc: cannot connect to the relay: %w", derr)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(maxFrame)
	sctx, scancel := context.WithCancel(c.ctx)
	s := &session{ws: ws, ctx: sctx}
	var pingWG sync.WaitGroup
	defer func() {
		scancel()
		pingWG.Wait()
		c.clearSession(s)
	}()

	var isReady atomic.Bool
	var timedOut atomic.Bool
	hs := time.AfterFunc(c.tune.handshake, func() {
		if !isReady.Load() {
			timedOut.Store(true)
			_ = ws.CloseNow()
		}
	})
	defer hs.Stop()

	// The REQ goes out at once, so a relay that needs no AUTH is ready after one round trip. A relay
	// that does need it answers CLOSED "auth-required" and sends (or has sent) an AUTH challenge;
	// after the AUTH is accepted the REQ is sent again.
	if err := c.sendREQ(s); err != nil {
		return false, errors.New("nwc: cannot subscribe")
	}
	var (
		authID string // id of the AUTH event awaiting its OK
		authed bool
	)
	for {
		_, data, rerr := ws.Read(sctx)
		if rerr != nil {
			if timedOut.Load() {
				return isReady.Load(), errors.New("nwc: relay did not finish AUTH and subscription in time")
			}
			return isReady.Load(), errors.New("nwc: relay connection lost")
		}
		var parts []json.RawMessage
		if json.Unmarshal(data, &parts) != nil || len(parts) == 0 {
			c.drop("bad frame")
			continue
		}
		var label string
		if json.Unmarshal(parts[0], &label) != nil {
			c.drop("bad frame")
			continue
		}
		switch label {
		case "EVENT":
			var sub string
			if len(parts) != 3 || json.Unmarshal(parts[1], &sub) != nil || sub != subID {
				c.drop("event for another subscription")
				continue
			}
			ev, perr := nostr.ParseEvent(parts[2])
			if perr != nil {
				c.drop("unparseable event")
				continue
			}
			c.onEvent(ev)
		case "EOSE":
			if authID == "" && !isReady.Load() {
				isReady.Store(true)
				hs.Stop()
				c.setSession(s)
				wasReady = true
				c.log.Info("relay connected")
				onReady()
				pingWG.Add(1)
				go func() { defer pingWG.Done(); c.pinger(s) }()
			}
		case "AUTH":
			var challenge string
			if len(parts) != 2 || json.Unmarshal(parts[1], &challenge) != nil {
				c.drop("bad frame")
				continue
			}
			ae, aerr := nostr.NewAuth(c.key, c.url, challenge, c.now())
			if aerr != nil {
				return isReady.Load(), errors.New("nwc: cannot answer the relay's AUTH challenge")
			}
			authID = ae.ID
			if werr := c.write(s, frameAuth(ae)); werr != nil {
				return isReady.Load(), errors.New("nwc: relay connection lost")
			}
		case "OK":
			id, accepted, msg, okFrame := parseOK(parts)
			if !okFrame {
				c.drop("bad frame")
				continue
			}
			if id == authID && authID != "" {
				authID = ""
				if !accepted {
					return isReady.Load(), &RelayRefusedError{Reason: msg}
				}
				authed = true
				if !isReady.Load() { // subscribe again, now that the relay knows us
					if err := c.sendREQ(s); err != nil {
						return false, errors.New("nwc: cannot subscribe")
					}
				}
				continue
			}
			if !accepted {
				if cl := c.claim(id); cl != nil {
					cl.done <- &RelayRefusedError{Reason: msg}
				}
			}
		case "CLOSED":
			var msg string
			if len(parts) >= 3 {
				_ = json.Unmarshal(parts[2], &msg)
			}
			if !isReady.Load() && !authed && strings.HasPrefix(msg, "auth-required:") {
				continue // expected: the AUTH answer is on its way
			}
			return isReady.Load(), &RelayRefusedError{Reason: msg}
		default: // NOTICE and anything unknown
		}
	}
}

func (c *Client) sendREQ(s *session) error {
	req := `["REQ","` + subID + `",{"kinds":[23195,23197],"#p":["` + c.me + `"],"authors":["` + c.wallet + `"]}]`
	return c.write(s, []byte(req))
}

func (c *Client) write(s *session, frame []byte) error {
	ctx, cancel := context.WithTimeout(s.ctx, writeTimeout)
	defer cancel()
	return s.ws.Write(ctx, websocket.MessageText, frame)
}

func (c *Client) pinger(s *session) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(s.ctx, pingTimeout)
			err := s.ws.Ping(ctx)
			cancel()
			if err != nil {
				_ = s.ws.CloseNow()
				return
			}
		}
	}
}

func (c *Client) setSession(s *session) {
	c.mu.Lock()
	c.sess = s
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
	c.ready.Add(1)
}

func (c *Client) clearSession(s *session) {
	c.mu.Lock()
	if c.sess == s {
		c.sess = nil
	}
	c.mu.Unlock()
}

func parseOK(parts []json.RawMessage) (id string, accepted bool, msg string, ok bool) {
	if len(parts) != 4 {
		return
	}
	if json.Unmarshal(parts[1], &id) != nil || json.Unmarshal(parts[2], &accepted) != nil ||
		json.Unmarshal(parts[3], &msg) != nil {
		return "", false, "", false
	}
	return id, accepted, msg, true
}

func frameEvent(e *nostr.Event) []byte { return frameWith("EVENT", e) }

// frameAuth is the client's NIP-42 answer: ["AUTH",<event>].
func frameAuth(e *nostr.Event) []byte { return frameWith("AUTH", e) }

func frameWith(label string, e *nostr.Event) []byte {
	var buf bytes.Buffer
	buf.WriteString(`["` + label + `",`)
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(e) // a struct of strings and ints always encodes
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	return append(b, ']')
}

// --- incoming events -------------------------------------------------------------------------------

func (c *Client) drop(reason string) {
	c.dropped.Add(1)
	c.log.Debug("dropped", "reason", reason)
}

// claim removes and returns the pending call id, or nil. The caller must complete the call.
func (c *Client) claim(id string) *call {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl := c.pending[id]
	delete(c.pending, id)
	return cl
}

func (c *Client) onEvent(ev *nostr.Event) {
	switch ev.Kind {
	case nostr.KindNWCResponse:
		c.onResponse(ev)
	case nostr.KindNWCNotification:
		c.onNotification(ev)
	default: // including 23196, the NIP-04 notification
		c.drop("unexpected kind")
	}
}

func (c *Client) onResponse(ev *nostr.Event) {
	id := tagValue(ev, "e")
	c.mu.Lock()
	_, known := c.pending[id]
	c.mu.Unlock()
	if !known { // late, repeated or someone else's: skip the cryptography too
		c.drop("response for no pending call")
		return
	}
	// The event must pass every check (kind, grammar, signature, author, addressee, decryption)
	// before it may touch a call.
	reqID, plain, err := openResponse(ev, c.conv, c.wallet, c.me)
	if err != nil || reqID != id {
		c.drop("response failed validation")
		return
	}
	cl := c.claim(id)
	if cl == nil { // the call timed out, or was closed, while we checked
		c.drop("response for no pending call")
		return
	}
	cl.done <- decodeResponse(plain, cl.method, cl.out)
}

func (c *Client) onNotification(ev *nostr.Event) {
	n, err := openNotification(ev, c.conv, c.wallet, c.me)
	if err != nil {
		c.drop("notification failed validation")
		return
	}
	c.mu.Lock()
	_, dup := c.seen[n.EventID]
	c.mu.Unlock()
	if dup {
		c.drop("repeated notification")
		return
	}
	select {
	case c.notif <- n:
	default:
		c.drop("notification channel full")
		c.log.Warn("notification dropped: channel full")
		return
	}
	c.mu.Lock()
	if old := c.seenQ[c.seenN]; old != "" {
		delete(c.seen, old)
	}
	c.seenQ[c.seenN] = n.EventID
	c.seen[n.EventID] = struct{}{}
	c.seenN = (c.seenN + 1) % len(c.seenQ)
	c.mu.Unlock()
}

// --- calls -----------------------------------------------------------------------------------------

// call publishes one request and waits for its answer. out is written by the read loop before the call
// returns; it belongs to the caller again once call returns.
func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	return c.callUntil(ctx, method, params, out, time.Time{})
}

// callUntil is call with the request's expiration tag set to expires; the zero time means now + the
// client's timeout. A non-zero expires at or before now (in whole seconds) is ErrExpiredBeforeSend,
// before anything is built or published.
func (c *Client) callUntil(ctx context.Context, method string, params any, out any, expires time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := c.now()
	if expires.IsZero() {
		// The floor only matters to tests with a sub-second Timeout: a one-second expiration can already
		// have passed when the relay checks it.
		expires = now.Add(max(c.timeout, 5*time.Second))
	} else if expires.Unix() <= now.Unix() {
		return ErrExpiredBeforeSend
	}
	ev, err := buildRequest(c.key, c.conv, c.wallet, method, params, now, expires)
	if err != nil {
		return err
	}
	frame := frameEvent(ev)
	cl := &call{method: method, out: out, done: make(chan error, 1)}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.pending[ev.ID] = cl
	c.mu.Unlock()

	timer := time.NewTimer(c.timeout)
	defer timer.Stop()

	// Wait for a ready session (AUTH done, subscription live) before publishing.
	var s *session
	for s == nil {
		c.mu.Lock()
		s = c.sess
		changed := c.changed
		c.mu.Unlock()
		if s != nil {
			break
		}
		select {
		case <-changed:
		case err := <-cl.done: // Close
			return err
		case <-timer.C:
			return c.abandon(cl, ev.ID, ErrNoResponse)
		case <-ctx.Done():
			return c.abandon(cl, ev.ID, fmt.Errorf("%w: %w", ErrNoResponse, ctx.Err()))
		}
	}

	// The one and only write of this request. If it fails the session is dying; the call still waits
	// for its answer, which the relay may hold, and the request is never written again.
	if err := c.write(s, frame); err != nil {
		c.log.Info("publish failed", "method", method)
	} else {
		c.log.Debug("request published", "method", method)
	}
	select {
	case err := <-cl.done:
		return err
	case <-timer.C:
		return c.abandon(cl, ev.ID, ErrNoResponse)
	case <-ctx.Done():
		return c.abandon(cl, ev.ID, fmt.Errorf("%w: %w", ErrNoResponse, ctx.Err()))
	}
}

// abandon ends a call at its deadline with err, unless a response, an OK false or Close claimed it
// first, in which case that result is returned.
func (c *Client) abandon(cl *call, id string, err error) error {
	if c.claim(id) == cl {
		return err
	}
	return <-cl.done
}

func hex64(what, s string) error {
	if !isHex64(s) {
		return fmt.Errorf("nwc: %s must be 64 lowercase hex", what)
	}
	return nil
}

// MakeInvoiceParams are the parameters of make_invoice.
type MakeInvoiceParams struct {
	AmountMsat      int64
	Description     string
	DescriptionHash string
	ExpirySeconds   int64
}

// MakeInvoice asks the wallet for an invoice.
func (c *Client) MakeInvoice(ctx context.Context, p MakeInvoiceParams) (*Transaction, error) {
	if p.AmountMsat <= 0 || p.AmountMsat > maxSafeInt || p.ExpirySeconds < 0 {
		return nil, errors.New("nwc: make_invoice amount or expiry out of range")
	}
	var tx Transaction
	err := c.call(ctx, "make_invoice", struct {
		Amount          int64  `json:"amount"`
		Description     string `json:"description,omitempty"`
		DescriptionHash string `json:"description_hash,omitempty"`
		Expiry          int64  `json:"expiry,omitempty"`
	}{p.AmountMsat, p.Description, p.DescriptionHash, p.ExpirySeconds}, &tx)
	if err != nil {
		return nil, err
	}
	return &tx, nil
}

// MakeHoldInvoiceParams are the parameters of make_hold_invoice.
type MakeHoldInvoiceParams struct {
	AmountMsat    int64
	PaymentHash   string
	Description   string
	ExpirySeconds int64
}

// MakeHoldInvoice asks the wallet for a hold invoice. Only the qualification suite uses it, to play the
// merchant; SatBooth's own code paths never do.
func (c *Client) MakeHoldInvoice(ctx context.Context, p MakeHoldInvoiceParams) (*Transaction, error) {
	if p.AmountMsat <= 0 || p.AmountMsat > maxSafeInt || p.ExpirySeconds < 0 {
		return nil, errors.New("nwc: make_hold_invoice amount or expiry out of range")
	}
	if err := hex64("payment hash", p.PaymentHash); err != nil {
		return nil, err
	}
	var tx Transaction
	err := c.call(ctx, "make_hold_invoice", struct {
		Amount      int64  `json:"amount"`
		PaymentHash string `json:"payment_hash"`
		Description string `json:"description,omitempty"`
		Expiry      int64  `json:"expiry,omitempty"`
	}{p.AmountMsat, p.PaymentHash, p.Description, p.ExpirySeconds}, &tx)
	if err != nil {
		return nil, err
	}
	return &tx, nil
}

// SettleHoldInvoice settles a hold invoice with its preimage. Only the qualification suite uses it.
func (c *Client) SettleHoldInvoice(ctx context.Context, preimage string) error {
	if err := hex64("preimage", preimage); err != nil {
		return err
	}
	return c.call(ctx, "settle_hold_invoice", struct {
		Preimage string `json:"preimage"`
	}{preimage}, nil)
}

// CancelHoldInvoice cancels a hold invoice. Only the qualification suite uses it.
func (c *Client) CancelHoldInvoice(ctx context.Context, paymentHash string) error {
	if err := hex64("payment hash", paymentHash); err != nil {
		return err
	}
	return c.call(ctx, "cancel_hold_invoice", struct {
		PaymentHash string `json:"payment_hash"`
	}{paymentHash}, nil)
}

// LookupInvoice returns the wallet's row for a payment hash.
func (c *Client) LookupInvoice(ctx context.Context, paymentHash string) (*Transaction, error) {
	if err := hex64("payment hash", paymentHash); err != nil {
		return nil, err
	}
	var tx Transaction
	err := c.call(ctx, "lookup_invoice", struct {
		PaymentHash string `json:"payment_hash"`
	}{paymentHash}, &tx)
	if err != nil {
		return nil, err
	}
	return &tx, nil
}

// maxInvoiceLen is the longest invoice string LookupInvoiceByString and PayInvoice send.
const maxInvoiceLen = 8192

// LookupInvoiceByString returns the wallet's row for a BOLT11 invoice string (lookup_invoice with
// "invoice"). The invoice must be 1 to maxInvoiceLen bytes of printable ASCII, else nothing is sent.
// Errors and the never-sent rules are those of LookupInvoice and PayInvoice. The wallet may answer
// with a row that is not the invoice's: the caller must check that the returned row's invoice (and
// type and amount) is the one it asked about before acting on it.
func (c *Client) LookupInvoiceByString(ctx context.Context, invoice string) (*Transaction, error) {
	if invoice == "" || len(invoice) > maxInvoiceLen {
		return nil, errors.New("nwc: lookup_invoice invoice out of range")
	}
	for i := 0; i < len(invoice); i++ {
		if invoice[i] < 0x20 || invoice[i] > 0x7e {
			return nil, errors.New("nwc: lookup_invoice invoice must be printable ASCII")
		}
	}
	var tx Transaction
	err := c.call(ctx, "lookup_invoice", struct {
		Invoice string `json:"invoice"`
	}{invoice}, &tx)
	if err != nil {
		return nil, err
	}
	return &tx, nil
}

// ListParams are the parameters of list_transactions. Limit must be 1 to 20, because a NIP-44 message
// is capped at 65,535 bytes. Type is "", "incoming" or "outgoing".
type ListParams struct {
	From, Until, Limit, Offset int64
	Unpaid                     bool
	Type                       string
}

// ListTransactions returns a page of the wallet's transactions and the total count.
func (c *Client) ListTransactions(ctx context.Context, p ListParams) ([]Transaction, int64, error) {
	if p.Limit < 1 || p.Limit > MaxListLimit {
		return nil, 0, errors.New("nwc: list_transactions limit must be 1 to 20")
	}
	if p.From < 0 || p.Until < 0 || p.Offset < 0 || p.From > maxSafeInt || p.Until > maxSafeInt || p.Offset > maxSafeInt {
		return nil, 0, errors.New("nwc: list_transactions range out of bounds")
	}
	if p.Type != "" && p.Type != TypeIncoming && p.Type != TypeOutgoing {
		return nil, 0, errors.New("nwc: list_transactions type must be incoming or outgoing")
	}
	var l TransactionList
	err := c.call(ctx, "list_transactions", struct {
		From   int64  `json:"from,omitempty"`
		Until  int64  `json:"until,omitempty"`
		Limit  int64  `json:"limit"`
		Offset int64  `json:"offset,omitempty"`
		Unpaid bool   `json:"unpaid,omitempty"`
		Type   string `json:"type,omitempty"`
	}{p.From, p.Until, p.Limit, p.Offset, p.Unpaid, p.Type}, &l)
	if err != nil {
		return nil, 0, err
	}
	return l.Transactions, l.TotalCount, nil
}

// PayInvoice asks the wallet to pay a BOLT11 invoice; amountMsat 0 means the invoice's own amount. The
// request expires the client's timeout after it is built.
//
// No error from PayInvoice proves the payment did not happen (ErrNoResponse, ErrMalformed and a
// *WalletError included); resolve the outcome with LookupInvoice.
//
// The request is known never to have been sent only when the error is a *RelayRefusedError; an
// argument-validation error returned by this method; ErrExpiredBeforeSend (PayInvoiceUntil only);
// ErrClosed from a call made after Close (the same value is returned to a call that was pending when
// Close ran, which is not known to be unsent, so treat ErrClosed as never sent only if you know Close
// had already returned); or a context error that does not wrap ErrNoResponse (the context was already
// done before the request was built or registered). Everything else, including every error that wraps
// ErrNoResponse, is outcome unknown.
func (c *Client) PayInvoice(ctx context.Context, invoice string, amountMsat int64) (*PayResult, error) {
	return c.payInvoice(ctx, invoice, amountMsat, time.Time{})
}

// PayInvoiceUntil is PayInvoice with the request's NIP-40 expiration set to exactly expiresAt (whole
// seconds), so the request outlives no deadline the caller has recorded. If expiresAt is not after the
// client's clock it returns ErrExpiredBeforeSend and publishes nothing. It sets no upper bound: a
// relay that refuses the expiration answers a *RelayRefusedError. The call still waits at most the
// client's timeout, or ctx, for the answer.
func (c *Client) PayInvoiceUntil(ctx context.Context, invoice string, amountMsat int64,
	expiresAt time.Time) (*PayResult, error) {
	if expiresAt.IsZero() {
		return nil, ErrExpiredBeforeSend
	}
	return c.payInvoice(ctx, invoice, amountMsat, expiresAt)
}

func (c *Client) payInvoice(ctx context.Context, invoice string, amountMsat int64,
	expires time.Time) (*PayResult, error) {
	if invoice == "" || len(invoice) > maxInvoiceLen || amountMsat < 0 || amountMsat > maxSafeInt {
		return nil, errors.New("nwc: pay_invoice invoice or amount out of range")
	}
	p := struct {
		Invoice string `json:"invoice"`
		Amount  int64  `json:"amount,omitempty"`
	}{invoice, amountMsat}
	var r PayResult
	if err := c.callUntil(ctx, "pay_invoice", p, &r, expires); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetBudget returns the connection's budget; Set is false when the wallet reports none.
func (c *Client) GetBudget(ctx context.Context) (*Budget, error) {
	var b Budget
	if err := c.call(ctx, "get_budget", nil, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// GetInfo returns the wallet's get_info answer.
func (c *Client) GetInfo(ctx context.Context) (*Info, error) {
	var in Info
	if err := c.call(ctx, "get_info", nil, &in); err != nil {
		return nil, err
	}
	return &in, nil
}
