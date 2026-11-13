// Package refund is the Lightning refund service (plan 9). A refund is a payment out, so the package is deny by
// default: a refund enters the system only through Claim, only with exactly what plan 9's allowed set
// permits, and anything else is refused with one fixed error and nothing written.
//
// This file holds the first half: admission (Claim), the read (Get) and the ledger transfer ids. pay.go holds the
// second: the sender, the resolver, the void and recovery, on the same Service.
//
// # The transition mutex
//
// Service.transition is the one mutex of the package. Admission (Claim) holds it from the first check against the
// store to the AddRefund that makes the refund visible, so two claims never both see the same room left under an
// order's cap, the daily cap or MaxOpen. Every state transition of an existing refund (the send marker, a result,
// a void) holds the same mutex and re-reads the refund's state under it, so a claim and a transition never act on
// a stale picture either. It is never held across a call to the hub: admission makes none, and a transition takes
// it only around its store and ledger reads and writes.
package refund

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/lninvoice"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/store"
)

// Limits (plan 9).
const (
	// MaxOpen is the most open (claimed or sent) refunds at once.
	MaxOpen = 64
	// HubCallTimeout bounds one hub call in the workers.
	HubCallTimeout = 15 * time.Second
	// VoidLookupTimeout bounds the lookup in the void handler.
	VoidLookupTimeout = 8 * time.Second
	// PendingAlertAfter is how long a sent refund may read pending before the resolver alerts.
	PendingAlertAfter = time.Hour

	maxMsat = approval.MaxAmountMsat
)

// The refund errors. Each is a fixed string that carries no input; the API maps them to its error codes:
// ErrApproval invalid_approval, ErrApprovalExpired approval_expired, ErrRefundExists refund_exists, ErrUnknownOrder
// unknown_order, ErrInvoice invalid_invoice, ErrOwnInvoice own_invoice, ErrExceedsRefundable exceeds_refundable,
// ErrDailyCap daily_cap, ErrTooManyOpen too_many_open, ErrInFlight refund_in_flight, ErrHubUnavailable
// hub_unavailable, ErrState invalid_state, ErrNotFound a 404.
var (
	ErrApproval          = errors.New("refund: invalid approval")
	ErrApprovalExpired   = errors.New("refund: approval expired")
	ErrRefundExists      = errors.New("refund: refund already exists")
	ErrUnknownOrder      = errors.New("refund: unknown order")
	ErrInvoice           = errors.New("refund: invalid invoice")
	ErrOwnInvoice        = errors.New("refund: the invoice is one of SatBooth's own")
	ErrExceedsRefundable = errors.New("refund: amount exceeds what is refundable")
	ErrDailyCap          = errors.New("refund: daily cap reached")
	ErrTooManyOpen       = errors.New("refund: too many open refunds")
	ErrInFlight          = errors.New("refund: payment in flight")
	ErrHubUnavailable    = errors.New("refund: hub unavailable")
	ErrState             = errors.New("refund: invalid state for this request")
	ErrNotFound          = errors.New("refund: not found")
)

// The refund states a View shows.
const (
	StateClaimed = "claimed"
	StateSent    = "sent"
	StateSettled = "settled"
	StateFailed  = "failed"
)

// Wallet is the part of the payout wallet (an *nwc.Client) the refund service calls.
type Wallet interface {
	// PayInvoiceUntil pays with a request whose expiration is exactly expiresAt (*nwc.Client.PayInvoiceUntil).
	PayInvoiceUntil(ctx context.Context, invoice string, amountMsat int64, expiresAt time.Time) (*nwc.PayResult, error)
	LookupInvoiceByString(ctx context.Context, invoice string) (*nwc.Transaction, error)
	GetBudget(ctx context.Context) (*nwc.Budget, error)
}

// Paid reports the Lightning msat paid for an order (lnpay.Service.PaidMsat).
type Paid interface {
	PaidMsat(ctx context.Context, orderID string) (int64, error)
}

// Config is the refund service's configuration (plan 9): MaxMsat and DailyMsat are required, with
// 0 < MaxMsat <= DailyMsat <= 2^53; BudgetAlertMsat is >= 0. PayTimeout is the payout connection's timeout
// (payout.timeout_seconds, nwc.MinTimeout to nwc.MaxTimeout): a refund's pay request expires at its sent_at plus it.
type Config struct {
	Network                             approval.Network
	Keys                                *approval.Keys
	MaxMsat, DailyMsat, BudgetAlertMsat int64
	PayTimeout                          time.Duration
}

// Service admits and (in the second half) pays refunds.
type Service struct {
	cfg  Config
	st   *store.Store
	lg   *ledger.Ledger
	w    Wallet
	paid Paid
	now  func() time.Time
	log  *slog.Logger

	// transition is the one transition mutex: see the package comment. Admission and every state transition hold it.
	transition sync.Mutex
	// retry holds the results whose write the store refused, by refund id; guarded by transition.
	retry map[string]waitingResult
	// inFlight is the refund the sender has written the send marker of and may still publish a pay request for ("" when
	// none); guarded by transition. prepare sets it in the critical section that writes the marker, and the sender
	// clears it only once its pay_invoice call has returned (or it decided not to call), so Void, which checks it in
	// every hold of the mutex it takes (the one that writes a claimed refund's result; the two around a sent refund's
	// lookup), never ends a refund this process can still send, whatever the clock says. One field is enough: the
	// sender is serial. Memory is enough: the data directory's lock makes this process the only sender, a sender that
	// crashed publishes nothing, and after a restart no refund that has a marker is ever sent again.
	inFlight string

	sendMu, resolveMu sync.Mutex // one round of each worker at a time
	// lookupFailed holds the claimed refunds whose pre-send lookup failed since its last answer, or whose prepare
	// stopped with an ALERT after it; guarded by sendMu. The sender passes over them while another refund can go, so
	// one refund the hub keeps failing on, or one in a state that needs a person, holds back none.
	lookupFailed map[string]bool
	wake         chan struct{} // a Claim wakes the sender

	amu      sync.Mutex
	alerted  map[string]bool // the ALERT lines raised, per refund
	retrying map[string]bool // the conditions logRetry is logged for, per refund ("" for the round)

	// The workers' timing (SendEvery, ResolveEvery, LookupGap); tests shorten them.
	sendEvery, resolveEvery, lookupGap time.Duration
	// crash, set only by tests, runs after each step of the sender and each ledger resolution; true stops the work
	// there, as a crash would.
	crash func(step string) bool
}

// New builds the service and runs start-up recovery before it returns (the last row of the state machine in
// threat-model.md §4; no hub call): every open refund whose ledger pending is posted is settled, and one whose pending
// is voided is failed (recovered). It refuses a configuration outside plan 9's limits and a missing dependency, and
// returns a storage error recovery met. A nil now is time.Now; a nil log discards.
func New(ctx context.Context, cfg Config, st *store.Store, lg *ledger.Ledger, w Wallet, paid Paid, now func() time.Time, log *slog.Logger) (*Service, error) {
	switch {
	case cfg.Keys == nil:
		return nil, errors.New("refund: no approval keys")
	case cfg.Network > approval.Regtest:
		return nil, errors.New("refund: unknown network")
	case cfg.MaxMsat <= 0 || cfg.MaxMsat > cfg.DailyMsat || cfg.DailyMsat > maxMsat:
		return nil, errors.New("refund: need 0 < max_msat <= daily_msat <= 2^53")
	case cfg.BudgetAlertMsat < 0:
		return nil, errors.New("refund: budget_alert_msat must not be negative")
	case cfg.PayTimeout < nwc.MinTimeout || cfg.PayTimeout > nwc.MaxTimeout:
		return nil, errors.New("refund: the payout timeout must be 10 s to 10 min")
	case st == nil || lg == nil || w == nil || paid == nil:
		return nil, errors.New("refund: store, ledger, wallet and paid source are required")
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{cfg: cfg, st: st, lg: lg, w: w, paid: paid, now: now, log: log, retry: map[string]waitingResult{},
		wake: make(chan struct{}, 1), alerted: map[string]bool{}, retrying: map[string]bool{},
		lookupFailed: map[string]bool{}, sendEvery: SendEvery, resolveEvery: ResolveEvery, lookupGap: LookupGap}
	if err := s.recover(ctx); err != nil {
		return nil, fmt.Errorf("refund: start-up recovery: %w", err)
	}
	return s, nil
}

// View is what the API shows of a refund (the allowlist). State is claimed (no send marker, no result), sent
// (marker, no result), settled or failed. Reason is set only when failed. ResolvedAt is zero unless settled or failed.
type View struct {
	ID, OrderID, State, Reason string
	AmountMsat                 int64
	ClaimedAt, ResolvedAt      time.Time
}

func viewOf(st store.RefundState) View {
	v := View{ID: st.ID, OrderID: st.OrderID, AmountMsat: st.AmountMsat, ClaimedAt: st.ClaimedAt.UTC(), State: StateClaimed}
	switch {
	case st.Resolved:
		v.State, v.ResolvedAt = st.Result.Outcome, st.Result.At.UTC()
		if st.Result.Outcome == store.RefundOutcomeFailed {
			v.Reason = st.Result.Reason
		}
	case st.Sent:
		v.State = StateSent
	}
	return v
}

// addSat adds two non-negative amounts without wrapping.
func addSat(a, b int64) int64 {
	if a > (1<<62)-b {
		return 1 << 62
	}
	return a + b
}

// Claim admits a refund: the only way a refund enters the system. It verifies the approval and the limit
// before taking the transition mutex; under the mutex it checks, in this order, that the refund id is unused, the
// order exists, the order's paid msat minus its refunds that are not failed covers the amount, the invoice is a
// well-formed invoice of the network, is not one of SatBooth's own and carries exactly the approved amount,
// the daily cap (UTC day, every refund not failed) and MaxOpen; then it stores the refund. The
// one-refund-per-invoice rule is enforced by the store in the same write. Claim makes no hub call and no ledger
// write. A refusal writes nothing and is one of the package errors; any other error is an internal failure.
func (s *Service) Claim(ctx context.Context, approvalHex, invoice string) (View, error) {
	now := s.now().UTC().Truncate(time.Second) // times are whole seconds: the store keeps no more
	ap, err := s.cfg.Keys.VerifyRefund(approvalHex, s.cfg.Network, now)
	switch {
	case errors.Is(err, approval.ErrExpired):
		return View{}, ErrApprovalExpired
	case err != nil:
		return View{}, ErrApproval
	case ap.AmountMsat > uint64(s.cfg.MaxMsat):
		return View{}, ErrApproval
	}
	amount := int64(ap.AmountMsat)
	id, orderID := hex.EncodeToString(ap.RefundID[:]), hex.EncodeToString(ap.OrderID[:])

	s.transition.Lock()
	defer s.transition.Unlock()

	if _, found, err := s.st.RefundByID(ctx, id); err != nil {
		return View{}, fmt.Errorf("refund: reading the refund: %w", err)
	} else if found {
		return View{}, ErrRefundExists
	}
	if _, err := s.st.OrderByID(ctx, orderID); errors.Is(err, store.ErrNotFound) {
		return View{}, ErrUnknownOrder
	} else if err != nil {
		return View{}, fmt.Errorf("refund: reading the order: %w", err)
	}
	paid, err := s.paid.PaidMsat(ctx, orderID)
	if err != nil {
		return View{}, fmt.Errorf("refund: reading the order's paid total: %w", err)
	}
	settled, open, err := s.st.RefundFigures(ctx, orderID)
	if err != nil {
		return View{}, fmt.Errorf("refund: reading the order's refunds: %w", err)
	}
	if used := addSat(settled, open); paid < 0 || paid-used < amount {
		return View{}, ErrExceedsRefundable
	}
	inv, present, err := lninvoice.AmountMsat(invoice, s.cfg.Network)
	if err != nil {
		return View{}, ErrInvoice
	}
	// SatBooth's own invoice is named as such whatever its amount: it is the sharper signal.
	if issued, err := s.st.InvoiceIssued(ctx, invoice); err != nil {
		return View{}, fmt.Errorf("refund: reading the issued invoices: %w", err)
	} else if issued {
		return View{}, ErrOwnInvoice
	}
	// The invoice must carry exactly the approved amount. A different one: the hub refuses a pay_invoice whose amount
	// differs from the invoice's, which would leave the refund sent until a person voids it. None: deny by default,
	// since the pinned hub cannot issue an amountless invoice, so paying one cannot be qualified.
	if !present || inv != amount {
		return View{}, ErrInvoice
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	usedToday, err := s.st.RefundMsatClaimedSince(ctx, day)
	if err != nil {
		return View{}, fmt.Errorf("refund: reading today's refunds: %w", err)
	}
	if addSat(usedToday, amount) > s.cfg.DailyMsat {
		return View{}, ErrDailyCap
	}
	openRefunds, err := s.st.OpenRefunds(ctx)
	if err != nil {
		return View{}, fmt.Errorf("refund: reading the open refunds: %w", err)
	}
	if len(openRefunds) >= MaxOpen {
		return View{}, ErrTooManyOpen
	}

	err = s.st.AddRefund(ctx, store.Refund{
		ID: id, OrderID: orderID, KeyID: hex.EncodeToString(ap.KeyID[:]), Approval: approvalHex, Invoice: invoice,
		AmountMsat: amount, ClaimedAt: now, ExpiresAt: time.Unix(ap.ExpiresAt, 0).UTC(),
	})
	switch {
	case errors.Is(err, store.ErrRefundExists):
		return View{}, ErrRefundExists
	case errors.Is(err, store.ErrRefundInvoiceUsed):
		return View{}, ErrInvoice
	case errors.Is(err, store.ErrNotFound):
		return View{}, ErrUnknownOrder
	case err != nil:
		return View{}, fmt.Errorf("refund: storing the refund: %w", err)
	}
	select {
	case s.wake <- struct{}{}:
	default: // the sender is already due to run
	}
	return View{ID: id, OrderID: orderID, State: StateClaimed, AmountMsat: amount, ClaimedAt: now}, nil
}

// Get returns the refund, or false if there is none (a malformed id is none).
func (s *Service) Get(ctx context.Context, id string) (View, bool, error) {
	st, found, err := s.st.RefundStateByID(ctx, id)
	switch {
	case errors.Is(err, store.ErrInvalid):
		return View{}, false, nil
	case err != nil:
		return View{}, false, fmt.Errorf("refund: reading the refund: %w", err)
	case !found:
		return View{}, false, nil
	}
	return viewOf(st), true, nil
}

// The ledger transfer ids of a refund: the first 16 bytes of SHA-256(domain || the 16 raw bytes of the refund id),
// read big-endian like lnpay.PaidTransferID. The three domains differ, so the three transfers of one refund
// (pending, post, void) have three ids, and no id collides with a payment's.
const (
	refundDomain     = "satbooth/ln-refund/v1"
	refundPostDomain = "satbooth/ln-refund-post/v1"
	refundVoidDomain = "satbooth/ln-refund-void/v1"
)

func transferID(domain string, id [16]byte) ledger.U128 {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write(id[:])
	u, err := ledger.U128FromBytes(h.Sum(nil)[:16])
	if err != nil {
		return ledger.U128Zero // unreachable (16 bytes); the ledger refuses the zero id
	}
	return u
}

// RefundTransferID is the id of the refund's pending transfer.
func RefundTransferID(id [16]byte) ledger.U128 { return transferID(refundDomain, id) }

// PostTransferID is the id of the transfer that posts the pending one (the refund settled).
func PostTransferID(id [16]byte) ledger.U128 { return transferID(refundPostDomain, id) }

// VoidTransferID is the id of the transfer that voids the pending one (the refund failed).
func VoidTransferID(id [16]byte) ledger.U128 { return transferID(refundVoidDomain, id) }
