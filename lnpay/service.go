// Package lnpay is the Lightning service core: the "pay" action (one open invoice per order, behind
// the global limits), the reconciler that finds payments, and the order status derived from the
// store, the ledger and the clock.
//
// # What guarantees what
//
//   - An order is paid by a ledger transfer whose id is derived from the invoice's payment hash
//     (PaidTransferID), so a payment found twice, by a notification and a poll, or again after a
//     restart, is one transfer. The order.paid event is idempotent by its derived id.
//   - An invoice stays open (no closed_invoices row) until a lookup made after its expiry plus a
//     grace period shows it unpaid, or, at the deadline (24 hours after the later of its expiry and
//     the service's start), the wallet answers NOT_FOUND. Nothing else closes it: a transport failure,
//     another wallet error or a malformed answer is no evidence that it is unpaid, so such an invoice
//     past the deadline stops counting against MaxOpen (also after a restart), is polled every 10
//     minutes with an alert line and stays open. The reconciler looks an invoice up before it closes
//     it, so a payment that settled while SatBooth was down, however long, is recorded on the first
//     pass. An order reads "expired" only when it is past its expiry and every invoice is closed (or
//     none was ever made), and never while a Pay for it is in flight.
//   - A wallet can accept a payment for a while after an invoice's expiry, so an invoice closed by a
//     lookup is still looked up every 10 minutes until its deadline (design.md, "Late settlement"). A late
//     settlement is recorded like any other, with an alert line: order.paid then follows
//     order.expired, and the order reads paid, because Status reads the ledger first. After a
//     restart the watch is re-derived from the store (closed by lookup, own deadline not passed).
//   - Notifications are hints: they move an invoice to the front of the queue and nothing else.
//     The lookup is what is believed.
//
// # Locks
//
// Service.mu is one short mutex over memory only: the open-invoice counts, the day counters, the
// in-flight Pay table and the notification front queue. It is never held across a store, ledger or
// nwc call (one exception: the daily counters are loaded from the store once per UTC day under it).
// Concurrent Pay calls for one order share one in-flight call (the "flight"); different orders do
// not wait for each other, and a MakeInvoice that blocks for 70 s delays nobody else.
package lnpay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/lninvoice"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/store"
)

// Errors returned by Pay. The limits sentinels (limits.ErrRate and the others) and store.ErrNotFound
// are returned as they are.
var (
	// ErrPaid: the order is already paid.
	ErrPaid = errors.New("lnpay: order already paid")
	// ErrExpired: the order is past its expiry and takes no new invoice.
	ErrExpired = errors.New("lnpay: order expired")
	// ErrNotCreated: the wallet was never asked, so no invoice exists. The caller may press again.
	ErrNotCreated = errors.New("lnpay: invoice not created")
	// ErrUnknown: the outcome is unknown (an invoice may exist at the hub that SatBooth never saw,
	// which is harmless). The caller may press again.
	ErrUnknown = errors.New("lnpay: invoice outcome unknown")
	// ErrNoLightning: the order has no Lightning amount (on-chain only); Pay takes no limit token and
	// calls no wallet for it.
	ErrNoLightning = errors.New("lnpay: order has no Lightning amount")
	// ErrRunning: Run is already running on this service.
	ErrRunning = errors.New("lnpay: already running")
)

// Defaults and bounds of Config.
const (
	DefaultInvoiceExpiry = 20 * time.Minute
	MinInvoiceExpiry     = 15 * time.Minute
	MaxInvoiceExpiry     = 30 * time.Minute
	DefaultPollRate      = 5.0
	MaxPollRate          = 10000.0
	DefaultGrace         = 2 * time.Minute

	idleRound   = 10 * time.Second // minimum period of a round after one with nothing to look up
	staleEvery  = 10 * time.Minute // lookup period of an invoice past its deadline
	watchFloor  = 50               // watched invoices a round may carry when fewer invoices are open
	lookupBound = 15 * time.Second // bound of one lookup in the reconciler and in Pay
	// flightBound bounds a whole Pay flight, from the first press: the lookups of the order's expired
	// invoices (lookupBound each), the attempt, MakeInvoice (also bounded by the nwc client's own
	// timeout) and the store writes.
	flightBound = 3 * time.Minute
)

// Fixed log lines. Logs carry no payment hash, invoice, key or wallet-supplied text.
const (
	logLookupFailed     = "invoice lookup failed; the invoice stays open"
	logShortSettle      = "ALERT: settled amount is below the invoice amount; not recorded"
	logDeadline         = "ALERT: invoice closed at its 24 h deadline without a settled lookup"
	logDoublePaid       = "ALERT: a second invoice of an already paid order was paid"
	logDifferentRecord  = "ALERT: a payment record exists with different details"
	logBadLookup        = "ALERT: lookup answered for another payment hash or a bad row"
	logMakeFailed       = "make_invoice failed"
	logMakeBadResponse  = "ALERT: make_invoice answered with a bad invoice"
	logMakeWrongInvoice = "ALERT: make_invoice answered with an invoice for another network or amount"
	logStoreInvoiceFail = "ALERT: storing a new invoice failed"
	logStepFailed       = "reconciler step failed"
	logHealFailed       = "startup completion of a paid invoice failed"
	logStale            = "ALERT: invoice past its 24 h deadline and the wallet's answers do not prove it unpaid; it stays open and is polled every 10 min"
	logAmountRefused    = "ALERT: an order's amount is outside the configured limits; pay refuses it"
	logPaidHookFailed   = "the Lightning-paid hook failed"
	logLatePaid         = "ALERT: an invoice closed as unpaid after its expiry was paid late; the payment is recorded and the order reads paid"
)

// Crash points of the test hook (Service.hook). A non-nil error from the hook aborts the step at that
// point, as if the process had died there.
const (
	crashAfterAttempt  = "pay:after-attempt"
	crashAfterMake     = "pay:after-make"
	crashAfterLookup   = "rec:after-lookup"
	crashAfterTransfer = "rec:after-transfer"
	crashAfterEvent    = "rec:after-event"
	crashAfterClose    = "rec:after-close"
	// hookJoined is not a crash point: Pay calls it when a press joins a flight in progress, so a
	// test can tell; its error is ignored.
	hookJoined = "pay:joined"
)

// Wallet is what the service needs of a wallet. *nwc.Client satisfies it.
type Wallet interface {
	MakeInvoice(ctx context.Context, p nwc.MakeInvoiceParams) (*nwc.Transaction, error)
	LookupInvoice(ctx context.Context, paymentHash string) (*nwc.Transaction, error)
	Notifications() <-chan nwc.Notification
}

// Config. A zero field takes its default.
type Config struct {
	InvoiceExpiry time.Duration // 15 to 30 minutes; default 20
	PollRate      float64       // lookups per second in total; default 5
	Grace         time.Duration // wait after an invoice's expiry before the lookup that may close it; default 2 minutes
	// Network is the network of the hub's invoices: every invoice the hub makes is checked against it (prefix,
	// checksum) and against the order's amount before it is stored. The zero value is approval.Mainnet.
	Network approval.Network
}

// openInv is a stored, unclosed invoice. released: it no longer holds a slot in the counts (it is
// paid, or past its deadline with only transport-level lookup failures). done: its payment is
// completely recorded (transfer and event) and it only waits for its expiry to be closed. stale:
// past the deadline; looked up only every staleEvery. polled: the time of the latest lookup of a
// stale invoice.
type openInv struct {
	inv      store.Invoice
	acct     string
	released bool
	done     bool
	stale    bool
	polled   time.Time
}

// watchInv is an invoice closed by a lookup that showed it not settled, still looked up every
// staleEvery until its deadline, because the wallet can settle it after its expiry. polled: the
// time of its latest lookup.
type watchInv struct {
	inv    store.Invoice
	polled time.Time
}

type flight struct {
	done chan struct{}
	inv  store.Invoice
	err  error
}

// Service is safe for concurrent use.
type Service struct {
	cfg Config
	st  *store.Store
	lg  *ledger.Ledger
	w   Wallet
	lim *limits.Limiter
	now func() time.Time
	log *slog.Logger

	hook    func(point string) error // tests only
	onPaid  OnLightningPaid          // SetOnLightningPaid; set before Run and Pay, read without the lock
	running atomic.Bool
	// startedAt is when New ran: the 24 h deadline of an invoice is measured from the later of its
	// expiry and this, so a long downtime never counts against an invoice.
	startedAt time.Time
	idle      time.Duration // minimum period of a reconciler round when the last had nothing to look up
	// addInvoice stores a new invoice (store.AddInvoice), and readBack reads an order's invoices
	// back after an indefinite error from it (store.InvoicesForOrder); tests replace them to inject
	// errors.
	addInvoice func(ctx context.Context, inv store.Invoice) error
	readBack   func(ctx context.Context, orderID string) ([]store.Invoice, error)

	mu       sync.Mutex
	total    int            // open invoices plus reserved slots, all classes
	newOpen  int            // the part of total held by accounts that are not Known
	byAcct   map[string]int // total, by account
	known    map[string]bool
	open     map[string]openInv  // stored, not closed, by payment hash
	watch    map[string]watchInv // closed by lookup, watched for a late settlement until the deadline
	flights  map[string]*flight  // Pay calls in flight, by order id
	landed   chan struct{}       // closed when the last flight in progress ends (WaitFlights)
	front    []string            // payment hashes a notification moved to the front
	alerted  map[string]bool     // invoices whose short settlement was already reported
	dayStart time.Time
	dayCount int
	dayMsat  int64
	dayReady bool
}

// OnLightningPaid is called when lnpay completes a Lightning payment of an order that also has a watched rail (an
// on-chain or a Monero amount): the rails' LightningPaid, which logs the both-rails ALERT when the order has a payment
// recorded on one.
// o is the order as the store holds it. An error is completePaid's: the reconciler retries the completion on its next
// lookup; Pay's flight answers ErrPaid (order.paid exists by then). settleOrder's heal of order.paid calls it too, and
// returns its error to the reconciler's step (logged), which does not repeat that heal.
type OnLightningPaid func(ctx context.Context, o store.Order) error

// SetOnLightningPaid sets the hook completePaid calls for an order with a watched rail (nil clears it). Call it before Run and before
// the first Pay: it is read without a lock.
func (s *Service) SetOnLightningPaid(f OnLightningPaid) { s.onPaid = f }

// New checks cfg, creates the two fixed ledger accounts and rebuilds the open-invoice counts from
// the store and the ledger. It starts nothing: call Run for the reconciler.
func New(cfg Config, st *store.Store, lg *ledger.Ledger, w Wallet, lim *limits.Limiter, now func() time.Time, log *slog.Logger) (*Service, error) {
	if st == nil || lg == nil || w == nil || lim == nil || now == nil {
		return nil, errors.New("lnpay: nil dependency")
	}
	if cfg.InvoiceExpiry == 0 {
		cfg.InvoiceExpiry = DefaultInvoiceExpiry
	}
	if cfg.PollRate == 0 {
		cfg.PollRate = DefaultPollRate
	}
	if cfg.Grace == 0 {
		cfg.Grace = DefaultGrace
	}
	switch {
	case cfg.InvoiceExpiry < MinInvoiceExpiry || cfg.InvoiceExpiry > MaxInvoiceExpiry:
		return nil, errors.New("lnpay: InvoiceExpiry must be 15 to 30 minutes")
	case !(cfg.PollRate > 0 && cfg.PollRate <= MaxPollRate):
		return nil, errors.New("lnpay: PollRate must be in (0, 10000]")
	case cfg.Grace < 0 || cfg.Grace > time.Hour:
		return nil, errors.New("lnpay: Grace must be in [0, 1h]")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Service{cfg: cfg, st: st, lg: lg, w: w, lim: lim, now: now, log: log, startedAt: now(), idle: idleRound,
		addInvoice: st.AddInvoice, readBack: st.InvoicesForOrder, watch: map[string]watchInv{}, flights: map[string]*flight{}, alerted: map[string]bool{}}
	ctx := context.Background()
	if err := ensureAccounts(ctx, lg); err != nil {
		return nil, err
	}
	if err := s.rebuild(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// ValidateAmount reports whether an order amount is acceptable: whole sats, within the limiter's
// bounds. The error is limits.ErrAmount.
func (s *Service) ValidateAmount(msat int64) error {
	c := s.lim.Config()
	if msat < c.MinMsat || msat > c.MaxMsat || msat%1000 != 0 {
		return limits.ErrAmount
	}
	return nil
}

func (s *Service) crash(point string) error {
	if s.hook == nil {
		return nil
	}
	return s.hook(point)
}

// --- counts ----------------------------------------------------------------------------------------

type derived struct {
	open    map[string]openInv
	byAcct  map[string]int
	known   map[string]bool
	total   int
	newOpen int
}

// deadline is 24 h after the later of the invoice's expiry and the service's start. From then on a
// NOT_FOUND answer closes the invoice (reason deadline); see reconcile.
func (s *Service) deadline(inv store.Invoice) time.Time {
	base := inv.ExpiresAt
	if s.startedAt.After(base) {
		base = s.startedAt
	}
	return base.Add(store.DeadlineGrace)
}

// classify reads what the counts need about a stored, unclosed invoice of account acct. It holds no
// slot once it is paid (a ledger transfer exists) or is past its own deadline (ExpiresAt +
// store.DeadlineGrace), so an invoice that went stale before a restart does not count again after
// it. It is stale (slow polling, and NOT_FOUND closes it) only past deadline(), which a restart
// moves to 24 h after the start.
func (s *Service) classify(ctx context.Context, inv store.Invoice, acct string, now time.Time) (openInv, error) {
	_, paid, err := s.paidTransfer(ctx, inv.PaymentHash)
	if err != nil {
		return openInv{}, err
	}
	past := !now.Before(inv.ExpiresAt.Add(store.DeadlineGrace))
	return openInv{inv: inv, acct: acct, released: paid || past, stale: !now.Before(s.deadline(inv))}, nil
}

// derive recounts the open invoices from the store, each account's class from its account_paid
// fact, and each invoice's slot with classify.
func (s *Service) derive(ctx context.Context) (*derived, error) {
	invs, err := s.st.OpenInvoices(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	d := &derived{open: map[string]openInv{}, byAcct: map[string]int{}, known: map[string]bool{}}
	acctOf := map[string]string{}
	checked := map[string]bool{}
	for _, inv := range invs {
		acct, ok := acctOf[inv.OrderID]
		if !ok {
			o, err := s.st.OrderByID(ctx, inv.OrderID)
			if err != nil {
				return nil, err
			}
			acct = o.AccountID
			acctOf[inv.OrderID] = acct
		}
		if !checked[acct] {
			checked[acct] = true
			k, err := s.accountKnown(ctx, acct)
			if err != nil {
				return nil, err
			}
			d.known[acct] = k
		}
		o, err := s.classify(ctx, inv, acct, now)
		if err != nil {
			return nil, err
		}
		d.open[inv.PaymentHash] = o
		if o.released {
			continue
		}
		d.byAcct[acct]++
		d.total++
		if !d.known[acct] {
			d.newOpen++
		}
	}
	return d, nil
}

// rebuild replaces the in-memory counts with a recount, and the watched closed invoices with those the
// store holds as closed by lookup whose own deadline (ExpiresAt + store.DeadlineGrace) has not passed.
// Paid ones are watched too: their first lookup round completes what a crash may have left undone
// (the order may already have order.expired, so nothing else revisits it). Used at New, when no Pay
// can be in flight.
func (s *Service) rebuild(ctx context.Context) error {
	d, err := s.derive(ctx)
	if err != nil {
		return err
	}
	closed, err := s.st.InvoicesClosedByLookup(ctx, s.now().Add(-store.DeadlineGrace))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Spread over one slow-poll period, newest expiry first (the likeliest to settle late), so a
	// day of closed invoices does not crowd the first rounds: entry i is due i/n of a period from now.
	s.watch = make(map[string]watchInv, len(closed))
	now := s.now()
	for i := range closed {
		inv := closed[len(closed)-1-i]
		polled := now.Add(-staleEvery + time.Duration(i)*staleEvery/time.Duration(len(closed)))
		s.watch[inv.PaymentHash] = watchInv{inv: inv, polled: polled}
	}
	s.open, s.byAcct, s.total, s.newOpen = d.open, d.byAcct, d.total, d.newOpen
	s.known = map[string]bool{}
	for a, k := range d.known {
		if k {
			s.known[a] = true
		}
	}
	return nil
}

// counts returns the live open-invoice total (including reserved slots) and the part held by
// accounts that are not Known.
func (s *Service) counts() (total, newOpen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total, s.newOpen
}

// recount returns the same two figures derived from the store and the ledger, for tests.
func (s *Service) recount(ctx context.Context) (total, newOpen int, err error) {
	d, err := s.derive(ctx)
	if err != nil {
		return 0, 0, err
	}
	return d.total, d.newOpen, nil
}

// MarkKnown records that the account has a paid order on another rail (the on-chain watcher): its open invoices
// leave the New share at once, as for a Lightning payment. The caller writes the store's account_paid fact first, so
// the in-memory set never knows an account that the store does not (a restart derives the set from the facts).
func (s *Service) MarkKnown(acct string) { s.markKnown(acct) }

// markKnown records that the account has a paid order: its open invoices leave the New share.
func (s *Service) markKnown(acct string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markKnownLocked(acct)
}

func (s *Service) markKnownLocked(acct string) {
	if s.known == nil {
		s.known = map[string]bool{}
	}
	if s.known[acct] {
		return
	}
	s.known[acct] = true
	s.newOpen -= s.byAcct[acct]
}

// accountKnown: the account has a paid order, which the store's account_paid fact records once the
// payment's transfer exists (completePaid). One indexed read; s.known, which the counts need anyway
// to keep each open invoice in one class, answers first (the fact is insert-only: a known account
// stays known).
func (s *Service) accountKnown(ctx context.Context, acct string) (bool, error) {
	s.mu.Lock()
	k := s.known[acct]
	s.mu.Unlock()
	if k {
		return true, nil
	}
	return s.st.AccountPaid(ctx, acct)
}

// orderPaidAt reports whether the ledger holds a payment transfer for any invoice of the order,
// and the time of the earliest.
func (s *Service) orderPaidAt(ctx context.Context, orderID string) (bool, time.Time, error) {
	invs, err := s.st.InvoicesForOrder(ctx, orderID)
	if err != nil {
		return false, time.Time{}, err
	}
	var at time.Time
	var paid bool
	for _, inv := range invs {
		tr, found, err := s.paidTransfer(ctx, inv.PaymentHash)
		if err != nil {
			return false, time.Time{}, err
		}
		if !found {
			continue
		}
		t := time.Unix(0, int64(tr.Timestamp)).UTC()
		if !paid || t.Before(at) {
			at = t
		}
		paid = true
	}
	return paid, at, nil
}

// detach removes a closed invoice from the counts, once.
func (s *Service) detach(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.open[hash]
	if !ok {
		return
	}
	delete(s.open, hash)
	delete(s.alerted, hash)
	if !o.released {
		s.releaseLocked(o.acct)
	}
}

// release takes the invoice out of the counts (paid, or stale) while it stays open; stale marks it
// for slow polling. done records that its payment is completely recorded.
func (s *Service) release(hash string, stale, done bool, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.open[hash]
	if !ok {
		return
	}
	if !o.released {
		o.released = true
		s.releaseLocked(o.acct)
	}
	o.stale = o.stale || stale
	o.done = o.done || done
	if stale {
		o.polled = at
	}
	s.open[hash] = o
}

// adopt starts tracking a stored, unclosed invoice that is not in s.open: one whose Pay could not
// read it back after an indefinite store error, or one stored by a flight that has not tracked it
// yet. The latter is left to its flight: an invoice whose order has a Pay in flight is skipped, and
// the check is made under the same lock as the count, after the flight would have tracked it.
func (s *Service) adopt(ctx context.Context, inv store.Invoice) error {
	if s.trackedOrFlying(inv) {
		return nil
	}
	ord, err := s.st.OrderByID(ctx, inv.OrderID)
	if err != nil {
		return err
	}
	acct := ord.AccountID
	known, err := s.accountKnown(ctx, acct)
	if err != nil {
		return err
	}
	o, err := s.classify(ctx, inv, acct, s.now())
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.open[inv.PaymentHash]; ok || s.flights[inv.OrderID] != nil {
		return nil
	}
	if known {
		s.markKnownLocked(acct)
	}
	s.open[inv.PaymentHash] = o
	if !o.released {
		s.total++
		if s.byAcct == nil {
			s.byAcct = map[string]int{}
		}
		s.byAcct[acct]++
		if !s.known[acct] {
			s.newOpen++
		}
	}
	return nil
}

func (s *Service) trackedOrFlying(inv store.Invoice) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.open[inv.PaymentHash]
	return ok || s.flights[inv.OrderID] != nil
}

func (s *Service) releaseLocked(acct string) {
	s.total--
	if s.byAcct[acct]--; s.byAcct[acct] <= 0 {
		delete(s.byAcct, acct)
	}
	if !s.known[acct] {
		s.newOpen--
	}
}

// --- Pay -------------------------------------------------------------------------------------------

// Pay is the "pay" action for the order with that token. It returns the order's open, unexpired
// invoice, making one if there is none. At most one Pay per order is in flight: concurrent presses
// wait for it and get its result.
//
// The flight runs on its own goroutine, detached from every presser's context from its start
// (context.WithoutCancel, bounded by flightBound and the nwc client's own timeout). So a presser who
// gives up, at any point, neither fails the presses waiting on the same flight nor abandons a
// MakeInvoice that has been counted: the flight still stores the invoice, and the next press gets it.
// A press whose own context ends while it waits returns ErrUnknown wrapping its context's error (it
// may press again).
func (s *Service) Pay(ctx context.Context, token string) (store.Invoice, error) {
	o, err := s.st.OrderByToken(ctx, token)
	if err != nil {
		return store.Invoice{}, err
	}
	if o.AmountMsat == 0 {
		return store.Invoice{}, ErrNoLightning
	}
	s.mu.Lock()
	f, running := s.flights[o.ID]
	if !running {
		f = &flight{done: make(chan struct{})}
		if len(s.flights) == 0 {
			s.landed = make(chan struct{})
		}
		s.flights[o.ID] = f
	}
	s.mu.Unlock()
	if running {
		_ = s.crash(hookJoined)
	}
	if !running {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flightBound)
		go func() {
			defer func() {
				cancel()
				s.mu.Lock()
				delete(s.flights, o.ID)
				if len(s.flights) == 0 {
					close(s.landed)
				}
				s.mu.Unlock()
				close(f.done)
			}()
			f.inv, f.err = s.pay(fctx, o)
		}()
	}
	select {
	case <-f.done:
		return f.inv, f.err
	case <-ctx.Done():
		return store.Invoice{}, fmt.Errorf("%w: %w", ErrUnknown, ctx.Err())
	}
}

// WaitFlights waits for the Pay flights in progress to end (they are detached from their pressers,
// so a shutdown that closes the store and the ledger must wait for them first), or for ctx to end:
// then it returns ctx's error and the flights still running are abandoned (each is bounded by
// flightBound; their writes are idempotent and re-derived at the next start). A Pay that starts
// while it waits may or may not be waited for.
func (s *Service) WaitFlights(ctx context.Context) error {
	s.mu.Lock()
	if len(s.flights) == 0 {
		s.mu.Unlock()
		return nil
	}
	landed := s.landed
	s.mu.Unlock()
	select {
	case <-landed:
		return nil
	case <-ctx.Done():
		// Both may be ready (the last flight landed as the grace ran out): then nothing was abandoned.
		select {
		case <-landed:
			return nil
		default:
			return ctx.Err()
		}
	}
}

func (s *Service) inFlight(orderID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.flights[orderID]
	return ok
}

// openInvoiceOf returns the order's newest invoice that is not closed and not past its expiry.
func (s *Service) openInvoiceOf(ctx context.Context, orderID string, now time.Time) (store.Invoice, bool, error) {
	invs, err := s.st.InvoicesForOrder(ctx, orderID)
	if err != nil {
		return store.Invoice{}, false, err
	}
	closed, err := s.closedSet(ctx, orderID)
	if err != nil {
		return store.Invoice{}, false, err
	}
	for i := len(invs) - 1; i >= 0; i-- {
		if !closed[invs[i].PaymentHash] && invs[i].ExpiresAt.After(now) {
			return invs[i], true, nil
		}
	}
	return store.Invoice{}, false, nil
}

func (s *Service) closedSet(ctx context.Context, orderID string) (map[string]bool, error) {
	cl, err := s.st.ClosuresForOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	m := make(map[string]bool, len(cl))
	for _, c := range cl {
		m[c.PaymentHash] = true
	}
	return m, nil
}

// settledEnough: the wallet's row shows the invoice itself settled for at least its amount.
func settledEnough(inv store.Invoice, tx *nwc.Transaction) bool {
	return tx != nil && strings.ToLower(tx.PaymentHash) == inv.PaymentHash && tx.Type == nwc.TypeIncoming &&
		tx.State == nwc.StateSettled && tx.AmountMsat >= inv.AmountMsat
}

// checkExpired looks up, before a second invoice is made, each invoice of the order that is past its
// expiry but not yet closed: the customer may have paid it late. Settled: the payment is recorded
// through the normal path and Pay answers ErrPaid. Any lookup error: ErrUnknown, no second invoice.
// Not settled: carry on.
func (s *Service) checkExpired(ctx context.Context, orderID string, now time.Time) error {
	invs, err := s.st.InvoicesForOrder(ctx, orderID)
	if err != nil {
		return err
	}
	closed, err := s.closedSet(ctx, orderID)
	if err != nil {
		return err
	}
	for _, inv := range invs {
		if closed[inv.PaymentHash] || inv.ExpiresAt.After(now) {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, lookupBound)
		tx, err := s.w.LookupInvoice(lctx, inv.PaymentHash)
		cancel()
		if err != nil {
			s.log.Info(logLookupFailed)
			return ErrUnknown
		}
		if settledEnough(inv, tx) {
			err := s.recordAndComplete(ctx, inv, tx.AmountMsat, s.now())
			if err != nil && !errors.Is(err, errOnPaid) {
				return err
			}
			// Paid. A failed hook (errOnPaid) comes after order.paid: it is not the payer's error, and the
			// reconciler completes the invoice again (it is still open and not done). Its one fixed line lets an
			// operator see why the account's slot stays held meanwhile.
			if err != nil {
				s.log.Error(logPaidHookFailed, "order", orderID)
			}
			return ErrPaid
		}
	}
	return nil
}

// pay is the flight: ctx is already detached from the pressers and bounded by flightBound.
func (s *Service) pay(ctx context.Context, o store.Order) (store.Invoice, error) {
	now := s.now()
	if paid, _, err := s.orderPaidAt(ctx, o.ID); err != nil {
		return store.Invoice{}, err
	} else if paid {
		return store.Invoice{}, ErrPaid
	}
	if inv, ok, err := s.openInvoiceOf(ctx, o.ID, now); err != nil {
		return store.Invoice{}, err
	} else if ok {
		return inv, nil
	}
	// A new invoice only while the order is open. (Its flight is registered, so the reconciler
	// does not expire it from under us.)
	if !now.Before(o.ExpiresAt) {
		return store.Invoice{}, ErrExpired
	}
	if err := s.checkExpired(ctx, o.ID, now); err != nil {
		return store.Invoice{}, err
	}
	// The API refuses such an amount, so an order with one was made under other limits: the
	// customer sees "busy", and the operator needs to know.
	if err := s.ValidateAmount(o.AmountMsat); err != nil {
		s.log.Error(logAmountRefused, "order", o.ID)
		return store.Invoice{}, err
	}
	known, err := s.accountKnown(ctx, o.AccountID)
	if err != nil {
		return store.Invoice{}, err
	}
	at, err := s.reserve(ctx, o, known)
	if err != nil {
		if errors.Is(err, limits.ErrAmount) {
			s.log.Error(logAmountRefused, "order", o.ID)
		}
		return store.Invoice{}, err
	}
	// The attempt is recorded after Admit and before the call, so a refused request costs nothing
	// and a call that times out still counts.
	if err := s.st.AddInvoiceAttempt(ctx, o.ID, at); err != nil {
		s.unreserve(o.AccountID, true, o.AmountMsat)
		return store.Invoice{}, fmt.Errorf("lnpay: recording the invoice attempt: %w", err)
	}
	if err := s.crash(crashAfterAttempt); err != nil {
		return store.Invoice{}, err
	}
	tx, err := s.w.MakeInvoice(ctx, nwc.MakeInvoiceParams{
		AmountMsat: o.AmountMsat, Description: "SatBooth order " + o.ID, ExpirySeconds: int64(s.cfg.InvoiceExpiry / time.Second),
	})
	if err != nil {
		s.unreserve(o.AccountID, false, 0)
		s.log.Info(logMakeFailed)
		return store.Invoice{}, outcome(err)
	}
	if err := s.crash(crashAfterMake); err != nil {
		return store.Invoice{}, err
	}
	hash := strings.ToLower(tx.PaymentHash)
	if tx.AmountMsat != o.AmountMsat || !validHash(hash) {
		s.unreserve(o.AccountID, false, 0)
		s.log.Error(logMakeBadResponse)
		return store.Invoice{}, ErrUnknown
	}
	// The invoice's own text, as the hub returned it: a well-formed invoice of the configured network that carries
	// exactly the order's amount. Nothing else of it is read. Not logged.
	if msat, present, err := lninvoice.AmountMsat(tx.Invoice, s.cfg.Network); err != nil || !present || msat != o.AmountMsat {
		s.unreserve(o.AccountID, false, 0)
		s.log.Error(logMakeWrongInvoice)
		return store.Invoice{}, ErrUnknown
	}
	created := time.Unix(s.now().Unix(), 0).UTC() // the store keeps whole seconds
	inv := store.Invoice{OrderID: o.ID, PaymentHash: hash, Bolt11: tx.Invoice, AmountMsat: o.AmountMsat,
		CreatedAt: created, ExpiresAt: created.Add(s.cfg.InvoiceExpiry)}
	if err := s.addInvoice(ctx, inv); err != nil {
		s.log.Error(logStoreInvoiceFail)
		if errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrDuplicate) || errors.Is(err, store.ErrNotFound) {
			s.unreserve(o.AccountID, false, 0)
			return store.Invoice{}, ErrUnknown
		}
		// Indefinite: the row may have been committed. Read it back.
		invs, rerr := s.readBack(ctx, o.ID)
		if rerr != nil {
			// Unknown. Free the slot: if the row was committed, the reconciler's next snapshot
			// adopts the invoice and counts it (adopt).
			s.unreserve(o.AccountID, false, 0)
			return store.Invoice{}, ErrUnknown
		}
		if !slices.ContainsFunc(invs, func(i store.Invoice) bool { return i.PaymentHash == hash }) {
			s.unreserve(o.AccountID, false, 0)
			return store.Invoice{}, ErrUnknown
		}
	}
	// The slot reserved above is this invoice's: it stays counted until the invoice is paid or closed.
	s.mu.Lock()
	s.open[hash] = openInv{inv: inv, acct: o.AccountID}
	s.mu.Unlock()
	return inv, nil
}

func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		if c := h[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// outcome maps a MakeInvoice error to the caller's sentinel (nwc/doc.go, "Outcome rule"). ErrNoResponse
// is checked before the context errors, because a timed-out call wraps both.
func outcome(err error) error {
	var refused *nwc.RelayRefusedError
	switch {
	case errors.Is(err, nwc.ErrNoResponse):
		return ErrUnknown
	case errors.As(err, &refused):
		return ErrNotCreated
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ErrNotCreated
	}
	return ErrUnknown // including ErrClosed (it may have been pending), wallet errors and anything else
}

// reserve takes a slot for the order under the service mutex: it loads the day's counters if needed,
// asks the limiter, counts the slot and the attempt. It returns the time it read under the lock (the
// attempt's time, so a midnight crossing cannot split the figures from the row).
func (s *Service) reserve(ctx context.Context, o store.Order, known bool) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if ds := limits.DayStart(now); !s.dayReady || !ds.Equal(s.dayStart) {
		n, msat, err := s.st.AttemptsSince(ctx, ds)
		if err != nil {
			return now, fmt.Errorf("lnpay: reading the day's attempts: %w", err)
		}
		s.dayStart, s.dayCount, s.dayMsat, s.dayReady = ds, n, msat, true
	}
	if known {
		s.markKnownLocked(o.AccountID)
	}
	class := limits.ClassNew
	if s.known[o.AccountID] {
		class = limits.ClassKnown
	}
	if err := s.lim.Admit(class, o.AmountMsat, s.total, s.newOpen, s.byAcct[o.AccountID], s.dayCount, s.dayMsat); err != nil {
		return now, err
	}
	s.total++
	if s.byAcct == nil {
		s.byAcct = map[string]int{}
	}
	s.byAcct[o.AccountID]++
	if !s.known[o.AccountID] {
		s.newOpen++
	}
	s.dayCount++
	s.dayMsat += o.AmountMsat
	return now, nil
}

// unreserve frees the slot reserved for the account and, with undoDay, the day's counters.
func (s *Service) unreserve(acct string, undoDay bool, amount int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseLocked(acct)
	if undoDay {
		s.dayCount--
		s.dayMsat -= amount
	}
}
