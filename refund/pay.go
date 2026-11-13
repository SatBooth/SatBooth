package refund

// This file is the second half of the service: the sender (the only code that calls pay_invoice), the resolver
// (which reads payout outcomes from the hub), the person's void, and start-up recovery. Money leaves here, so every
// rule is plan 9's, read literally:
//
//   - One pay_invoice per refund, ever. The ledger pending and the send marker are written, under the transition
//     mutex, before the call; once the marker exists only lookups follow, whatever they say.
//   - Settled is paid, failed is failed. Only a lookup that is outgoing, of the refund's invoice and amount, and
//     settled or failed resolves a sent refund; a *nwc.RelayRefusedError from pay_invoice is the one proof that it
//     was never sent.
//   - Anything unexpected raises an ALERT line and changes nothing, so a person decides. There is no automatic
//     repair beyond those rules; the person's void (plan 11, V1) ends any open refund the hub does not report pending.
//
// Every transition takes the transition mutex, re-reads the refund's state with RefundStateByID and does nothing if
// the state is not the one it expects. The mutex is never held across a hub call: a worker reads under it,
// releases it, calls the hub, then retakes it and re-reads before it writes.
//
// The ledger always moves first: a refund's result row is written only after its pending transfer is posted
// (settled) or voided (failed, when there is one), so a crash between the two leaves a resolved pending and no
// result, which recovery finishes on the next start.

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/store"
)

// Timing (plan 9).
const (
	// SendEvery is how often the sender looks for claimed refunds when no claim wakes it.
	SendEvery = 5 * time.Second
	// ResolveEvery is how often the resolver looks up the sent refunds.
	ResolveEvery = 30 * time.Second
	// LookupGap is the least time between two of the resolver's lookups: one a second at most.
	LookupGap = time.Second
	// VoidWriteTimeout bounds the void's ledger and store writes, which run detached from the request.
	VoidWriteTimeout = 5 * time.Second
	// VoidAfter is how long after the send marker a void may resolve the refund: strictly longer than nwc.MaxTimeout,
	// the cap on payout.timeout_seconds and so on a refund request's lifetime (its NIP-40 expiration is sent_at plus
	// that timeout), so by then a relay whose clock agrees with SatBooth's serves the request to no one. The store
	// enforces the same wait (store.RefundVoidWait; TestVoidAfter pins the two together).
	VoidAfter = nwc.MaxTimeout + time.Minute
)

// codeRefund is the ledger code of a refund's transfers (in lnpay.LedgerMsat).
const codeRefund = 5

// The operator-visible lines. Each is raised at most once per refund and condition while the process runs (a
// restart may repeat it once), with the refund id and the order id as its only fields.
const (
	AlertBudgetUnset    = "ALERT: refund: the payout connection has no budget at the hub; the refund waits"
	AlertBudgetShort    = "ALERT: refund: the hub budget left is less than the refund and its fee headroom; the refund waits"
	AlertBudgetLow      = "ALERT: refund: the hub budget left after this refund is below budget_alert_msat"
	AlertBadAmount      = "ALERT: refund: a refund's amount is not positive; nothing is sent"
	AlertLedgerRefused  = "ALERT: refund: the ledger refused a refund transfer; nothing changed"
	AlertLedgerOpposite = "ALERT: refund: the ledger already resolved the refund the other way; nothing changed, a person decides"
	AlertLedgerResolved = "ALERT: refund: the ledger shows a refund resolved that has no result; nothing is sent, " +
		"restart SatBooth to finish it or void it if the ledger shows it paid"
	AlertVoidLedgerResolved = "ALERT: refund: a void was refused because the ledger already resolved the refund; " +
		"nothing changed, restart SatBooth to finish it"
	AlertNoPending     = "ALERT: refund: a sent refund has no pending ledger transfer; nothing changed, a person voids it"
	AlertSendRefused   = "ALERT: refund: the store refused the send marker; nothing is sent, the refund waits"
	AlertResultRefused = "ALERT: refund: the store refused a refund's result (has the clock stepped back?); it is retried"
	AlertPending       = "ALERT: refund: a sent refund still reads pending at the hub an hour after it was sent"
	AlertNotFound      = "ALERT: refund: the hub has no record of a sent refund; a person decides and voids it"
	AlertExpired       = "ALERT: refund: the hub shows a sent refund as expired, which proves neither outcome; a person decides"
	AlertAnomaly       = "ALERT: refund: the hub's answer does not match the sent refund; nothing changed"
	AlertLookupOther   = "ALERT: refund: the hub answered a lookup with another invoice; nothing changed"
	AlertApproval      = "ALERT: refund: the stored approval no longer verifies, for a reason " +
		"other than its key or its expiry (has the clock or payout.network " +
		"changed?); nothing is sent, the refund ends expired_unsent at its " +
		"expiry"
	AlertPostedUnsent = "ALERT: refund: the ledger shows a refund paid that has no send marker; nothing changed, a person voids it"

	// logRetry is not an alert: a read or write failed for a reason that is not a refusal, and the round retries. It is
	// logged once per refund and condition (a store or ledger call, or a hub call), again only after it changed.
	logRetry = "refund: a store, ledger or hub call failed; the refund is retried on a later round"
)

// The conditions logRetry is logged once for, per refund.
const (
	condStore = "store" // a store or ledger call failed
	condHub   = "hub"   // a hub call failed
)

// The crash points the tests stop at (see Service.crash): right after each step of the sender and each ledger
// resolution.
const (
	stepPreChecks = "pre-checks"
	stepPending   = "pending"
	stepMarker    = "marker"
	stepSent      = "sent"
	stepPost      = "post"
	stepVoid      = "void"
	// stepVoidPending is right after the void of a sent refund with no ledger pending created it (V1e).
	stepVoidPending = "void-pending"
)

// kind is the refund's state as the transitions see it.
type kind int

const (
	kindClaimed  kind = iota + 1 // no send marker, no result
	kindSent                     // a send marker, no result
	kindResolved                 // a result
)

func kindOf(st store.RefundState) kind {
	switch {
	case st.Resolved:
		return kindResolved
	case st.Sent:
		return kindSent
	}
	return kindClaimed
}

// outcome is a result to write: an outcome and a reason of store's.
type outcome struct{ outcome, reason string }

// outSettled is the one settled result.
var outSettled = outcome{store.RefundOutcomeSettled, store.RefundReasonSettled}

func failedWith(reason string) outcome { return outcome{store.RefundOutcomeFailed, reason} }

// ledgerStep is the ledger step that goes with a result: the one done before its row is written, and the one a retry
// repeats.
type ledgerStep int

const (
	ledgerByOutcome     ledgerStep = iota // post the pending (settled) or void it (failed): the sender, the resolver, recovery, V1b, V1d
	ledgerNone                            // no ledger write: V1c, V1e's two failures, and V1a when the ledger refuses its pending
	ledgerCreateAndPost                   // the refund's pending transfer, then its post: V1e settled
	ledgerCreateAndVoid                   // the refund's pending transfer and its void, in one batch: V1a
)

// waitingResult is a result that was decided (its ledger step done) but whose write the store refused: it is retried,
// the same result with the same ledger step, until it is written. Kept in memory only: after a restart, recovery reads
// the ledger again.
type waitingResult struct {
	want kind
	o    outcome
	step ledgerStep
	// void marks the void's own first attempt: a ledger or store call that fails for a reason that is not a refusal is
	// not logged as a retry, because nothing retries it (the void answers its error and a second void runs the row
	// again). A result the store refuses is stored without the mark, and the workers retry it as any other.
	void bool
}

// ledgerFailed notes a failed ledger call of the result w: logged as a retry, unless it is the void's own attempt.
func (s *Service) ledgerFailed(st store.RefundState, w waitingResult) {
	if !w.void {
		s.retryLater(st.ID, condStore)
	}
}

// finished is what a transition did.
type finished int

const (
	done    finished = iota + 1 // the result is written
	changed                     // the refund was not in the expected state: nothing done
	stopped                     // an ALERT or a failure stopped it: nothing written (a refused result is retried)
)

// nowSec is the clock in whole seconds (the store keeps no more).
func (s *Service) nowSec() time.Time { return s.now().UTC().Truncate(time.Second) }

// alert logs msg once per refund while the process runs.
func (s *Service) alert(msg string, st store.RefundState) {
	s.amu.Lock()
	key := msg + "\x00" + st.ID
	seen := s.alerted[key]
	s.alerted[key] = true
	s.amu.Unlock()
	if !seen {
		s.log.Error(msg, "refund_id", st.ID, "order_id", st.OrderID)
	}
}

// retryLater logs logRetry for the refund id (or, with id empty, for the round: then without a refund id) once per
// condition: again only after retryCleared ran for it.
func (s *Service) retryLater(id, cond string) {
	key := id + "\x00" + cond
	s.amu.Lock()
	seen := s.retrying[key]
	s.retrying[key] = true
	s.amu.Unlock()
	switch {
	case seen:
	case id == "":
		s.log.Warn(logRetry)
	default:
		s.log.Warn(logRetry, "refund_id", id)
	}
}

// retryCleared forgets the logged conditions of the refund id (or of the round, with id empty): a step went through.
// With no cond it forgets all of them.
func (s *Service) retryCleared(id string, conds ...string) {
	if len(conds) == 0 {
		conds = []string{condStore, condHub}
	}
	s.amu.Lock()
	for _, c := range conds {
		delete(s.retrying, id+"\x00"+c)
	}
	s.amu.Unlock()
}

// crashAt runs the test crash hook after step; true means the test crashed the process here.
func (s *Service) crashAt(step string) bool { return s.crash != nil && s.crash(step) }

func rawID(id string) [16]byte {
	var b [16]byte
	hex.Decode(b[:], []byte(id)) // the store holds 32 lowercase hex characters
	return b
}

// readLocked re-reads the refund under the transition mutex and reports whether it is in state want and has no
// refused result waiting.
func (s *Service) readLocked(ctx context.Context, id string, want kind) (store.RefundState, bool) {
	if _, waiting := s.retry[id]; waiting {
		return store.RefundState{}, false
	}
	st, found, err := s.st.RefundStateByID(ctx, id)
	if err != nil {
		s.retryLater(id, condStore)
		return store.RefundState{}, false
	}
	return st, found && kindOf(st) == want
}

// snapshot is readLocked under the mutex, for a worker about to call the hub.
func (s *Service) snapshot(ctx context.Context, id string, want kind) (store.RefundState, bool) {
	s.transition.Lock()
	defer s.transition.Unlock()
	return s.readLocked(ctx, id, want)
}

// finish ends the refund with o if it is still in state want: the ledger first, then the result row.
func (s *Service) finish(ctx context.Context, id string, want kind, o outcome) finished {
	s.transition.Lock()
	defer s.transition.Unlock()
	return s.finishHeld(ctx, id, waitingResult{want: want, o: o})
}

// finishHeld is finish with the mutex held, for the decided result w (its ledger step included).
func (s *Service) finishHeld(ctx context.Context, id string, w waitingResult) finished {
	if p, waiting := s.retry[id]; waiting && p != w {
		return changed // another result is decided and waits for its write: never a different one
	}
	return s.finishLocked(ctx, id, w)
}

// finishLocked is finishHeld without the check for a waiting result.
func (s *Service) finishLocked(ctx context.Context, id string, w waitingResult) finished {
	st, found, err := s.st.RefundStateByID(ctx, id)
	switch {
	case err != nil:
		s.ledgerFailed(store.RefundState{ID: id}, w) // st is empty here
		return stopped
	case !found || kindOf(st) != w.want:
		delete(s.retry, id)
		return changed
	}
	if !s.resolveLedger(ctx, st, &w) {
		return stopped
	}
	o := w.o
	err = s.st.AddRefundResult(ctx, store.RefundResult{RefundID: id, Outcome: o.outcome, Reason: o.reason, At: s.nowSec()})
	switch {
	case err == nil:
		delete(s.retry, id)
		s.retryCleared(id)
		return done
	case errors.Is(err, store.ErrResultExists): // unreachable under the mutex after the re-read
		delete(s.retry, id)
		return changed
	}
	w.void = false
	s.retry[id] = w
	if errors.Is(err, store.ErrInvalid) {
		s.alert(AlertResultRefused, st)
	} else {
		s.retryLater(id, condStore)
	}
	return stopped
}

// resolveLedger does the ledger step of the decided result w. By outcome it posts the refund's pending transfer
// (settled) or voids it (failed); a claimed refund may have no pending (it was never prepared): then a failure needs
// no ledger step; a sent refund has one (without it the resolver raises AlertNoPending). The void's rows say
// otherwise through w.step. It may change w.step, which a refused result then waits with: V1a, when the ledger
// refuses the pending, becomes the store-only cancel (ledgerNone).
func (s *Service) resolveLedger(ctx context.Context, st store.RefundState, wp *waitingResult) bool {
	w := *wp
	want, o := w.want, w.o
	switch w.step {
	case ledgerNone:
		return true
	case ledgerCreateAndVoid:
		// V1a: the refund's pending and its void in ONE batch, which the ledger applies whole or not at all: a cancel the
		// store refuses to record is still decided in the ledger, where a restart finds it (recovery ends the refund
		// recovered), and no crash leaves an open pending behind. The refused item carries its code and the other
		// answers linked_event_failed. A pending the ledger refuses (the sender's prepare stops on the same refusal)
		// leaves nothing to void: the cancel is then the result row alone, with no ALERT. A refused void (impossible
		// for a fresh pending) raises AlertLedgerRefused.
		if o != failedWith(store.RefundReasonCancelled) {
			return false // unreachable: only a cancel has this step
		}
		pend, err := pendingTransfer(st)
		if err != nil {
			s.ledgerFailed(st, w)
			return false
		}
		rs, err := s.lg.CreateTransfers(ctx, []ledger.Transfer{pend, voidTransfer(rawID(st.ID))})
		if err != nil || len(rs) != 2 {
			s.ledgerFailed(st, w)
			return false
		}
		applied := func(r ledger.CreateTransferResult) bool {
			return r == ledger.TransferCreated || r == ledger.TransferExists
		}
		switch {
		case applied(rs[0]) && applied(rs[1]):
			return !s.crashAt(stepVoid)
		case rs[0] != ledger.TransferLinkedEventFailed && !applied(rs[0]):
			wp.step = ledgerNone
			return true
		}
		s.alert(AlertLedgerRefused, st)
		return false
	case ledgerCreateAndPost:
		// V1e settled: the pending, then the post below. A post the ledger refuses after the pending was created leaves
		// the pending: the refund is then in row V1d's state (sent, pending open), as after a crash between the two;
		// nothing is rolled back.
		if o != outSettled {
			// Unreachable: sentStep gives this step only with a settled result. Nothing was refused by the ledger, so no
			// ALERT; the result is not written.
			return false
		}
		switch r, ok := s.createPending(ctx, st); {
		case !ok:
			s.ledgerFailed(st, w)
			return false
		case r != ledger.TransferCreated && r != ledger.TransferExists:
			s.alert(AlertLedgerRefused, st)
			return false
		}
		if s.crashAt(stepVoidPending) {
			return false
		}
	}
	rid := rawID(st.ID)
	pid := RefundTransferID(rid)
	if _, found, err := s.lg.LookupTransfer(ctx, pid); err != nil {
		s.ledgerFailed(st, w)
		return false
	} else if !found {
		if want == kindSent || o == outSettled {
			s.alert(AlertNoPending, st)
			return false
		}
		return true
	}
	t := voidTransfer(rid)
	opposite, step := ledger.TransferPendingTransferAlreadyPosted, stepVoid
	if o == outSettled {
		t.ID, t.Amount, t.Flags = PostTransferID(rid), ledger.U128Max, ledger.TransferPostPendingTransfer // the whole pending
		opposite, step = ledger.TransferPendingTransferAlreadyVoided, stepPost
	}
	rs, err := s.lg.CreateTransfers(ctx, []ledger.Transfer{t})
	if err != nil || len(rs) != 1 {
		s.ledgerFailed(st, w)
		return false
	}
	switch rs[0] {
	case ledger.TransferCreated, ledger.TransferExists:
	case opposite:
		s.alert(AlertLedgerOpposite, st)
		return false
	default:
		s.alert(AlertLedgerRefused, st)
		return false
	}
	return !s.crashAt(step)
}

// ledgerResolution reports whether the refund's pending transfer has been posted or voided (by this package's
// transfer ids, the only ones that resolve it).
func (s *Service) ledgerResolution(ctx context.Context, rid [16]byte) (posted, voided bool, err error) {
	if _, posted, err = s.lg.LookupTransfer(ctx, PostTransferID(rid)); err != nil {
		return false, false, err
	}
	_, voided, err = s.lg.LookupTransfer(ctx, VoidTransferID(rid))
	return posted, voided, err
}

// applyRetries writes the refused results again, the same results.
func (s *Service) applyRetries(ctx context.Context) {
	s.transition.Lock()
	defer s.transition.Unlock()
	ids := make([]string, 0, len(s.retry))
	for id := range s.retry {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		p := s.retry[id]
		s.finishLocked(ctx, id, p)
	}
}

// recover is start-up recovery (the last row of the state machine in threat-model.md §4), with no hub call: every open
// refund whose pending transfer the ledger shows posted is settled, and one it shows voided is failed (recovered), both
// stamped now. A result the store refuses is retried by the workers.
func (s *Service) recover(ctx context.Context) error {
	open, err := s.st.OpenRefunds(ctx)
	if err != nil {
		return err
	}
	s.transition.Lock()
	for _, st := range open {
		posted, voided, err := s.ledgerResolution(ctx, rawID(st.ID))
		if err != nil {
			s.transition.Unlock()
			return err
		}
		switch {
		case posted && voided: // the ledger resolves a pending once; never reached
			s.alert(AlertLedgerOpposite, st)
		case posted && !st.Sent: // a post only ever follows a send: a lost store write or tampering
			s.alert(AlertPostedUnsent, st)
		case posted:
			s.retry[st.ID] = waitingResult{want: kindOf(st), o: outSettled}
		case voided:
			s.retry[st.ID] = waitingResult{want: kindOf(st), o: failedWith(store.RefundReasonRecovered)}
		}
	}
	s.transition.Unlock()
	s.applyRetries(ctx)
	return nil
}

// Run runs the sender and the resolver until ctx ends, then returns ctx's error. Recovery has already run in New.
// The sender runs a round at once, then on every Claim and every SendEvery; the resolver at once, then every
// ResolveEvery. Each round first writes again any result the store refused.
func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.loop(ctx, s.sendEvery, s.wake, s.sendRound) }()
	go func() { defer wg.Done(); s.loop(ctx, s.resolveEvery, nil, s.resolveRound) }()
	wg.Wait()
	return ctx.Err()
}

func (s *Service) loop(ctx context.Context, every time.Duration, wake <-chan struct{}, round func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-wake:
		}
	}
}

// sendRound is one round of the sender. First, for every claimed refund, oldest first, the checks that read only
// the store and the configuration (storeChecks): an expired approval ends the refund expired_unsent, every round.
// Then the hub, in sendHub: one get_budget for the round, and one lookup and the send for one refund. So a round
// makes at most 2 hub calls before its one pay_invoice (get_budget and one lookup), however many refunds are claimed,
// and 1 when get_budget fails or the budget covers none. Rounds never overlap, so two pay_invoice calls are never in
// flight.
func (s *Service) sendRound(ctx context.Context) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.applyRetries(ctx)
	open, err := s.st.OpenRefunds(ctx)
	if err != nil {
		s.retryLater("", condStore)
		return
	}
	s.retryCleared("", condStore)
	var ready []store.RefundState
	claimed := map[string]bool{}
	for _, st := range open {
		if ctx.Err() != nil {
			return
		}
		if st.Sent {
			continue
		}
		claimed[st.ID] = true
		if cur, ok := s.storeChecks(ctx, st.ID); ok {
			ready = append(ready, cur)
		}
	}
	for id := range s.lookupFailed { // forget the refunds that are no longer claimed
		if !claimed[id] {
			delete(s.lookupFailed, id)
		}
	}
	if len(ready) > 0 {
		s.sendHub(ctx, ready)
	}
}

// sendHub is the hub's part of a sender round, over the refunds that passed storeChecks, oldest first. It reads the
// budget once. A refund whose amount and fee headroom the budget does not cover gets its ALERT once and is skipped
// this round; it does not hold back the others. Among those the budget covers, it takes the oldest not in
// lookupFailed (its lookup failed, or its prepare raised an ALERT, since its last good lookup; when every one is in
// it, it forgets them and starts again from the oldest), and sendOne looks its invoice up and sends it.
func (s *Service) sendHub(ctx context.Context, ready []store.RefundState) {
	bctx, cancel := context.WithTimeout(ctx, HubCallTimeout)
	b, err := s.w.GetBudget(bctx)
	cancel()
	switch {
	case err != nil || b == nil:
		s.retryLater("", condHub)
		return
	case !b.Set:
		for _, st := range ready {
			s.alert(AlertBudgetUnset, st)
		}
		return
	}
	s.retryCleared("", condHub)
	left := b.TotalMsat - b.UsedMsat
	var fits []store.RefundState
	for _, st := range ready {
		if left < st.AmountMsat+max(st.AmountMsat/100, 10_000) {
			s.alert(AlertBudgetShort, st)
		} else {
			fits = append(fits, st)
		}
	}
	if len(fits) == 0 {
		return
	}
	i := slices.IndexFunc(fits, func(st store.RefundState) bool { return !s.lookupFailed[st.ID] })
	if i < 0 {
		for _, st := range fits {
			delete(s.lookupFailed, st.ID)
		}
		i = 0
	}
	s.sendOne(ctx, fits[i].ID, left)
}

// standing is what the send-time re-check of a stored approval found.
type standing int

const (
	stands     standing = iota + 1 // it verifies and says exactly what the row says
	keyGone                        // its key is no longer configured: key_revoked
	expiredNow                     // it has expired: expired_unsent
	broken                         // any other failure: an ALERT, no transition
)

// approvalStanding re-verifies the stored approval against the keys and network configured now.
func (s *Service) approvalStanding(st store.RefundState, now time.Time) standing {
	ap, err := s.cfg.Keys.VerifyRefund(st.Approval, s.cfg.Network, now)
	switch {
	case errors.Is(err, approval.ErrUnknownKey):
		return keyGone
	case errors.Is(err, approval.ErrExpired):
		return expiredNow
	case err != nil:
		return broken
	case hex.EncodeToString(ap.RefundID[:]) != st.ID, hex.EncodeToString(ap.OrderID[:]) != st.OrderID,
		hex.EncodeToString(ap.KeyID[:]) != st.KeyID, ap.AmountMsat != uint64(st.AmountMsat),
		ap.ExpiresAt != st.ExpiresAt.Unix():
		return broken
	}
	return stands
}

// isNotFound reports whether err is the hub's NOT_FOUND.
func isNotFound(err error) bool {
	var we *nwc.WalletError
	return errors.As(err, &we) && we.Code == "NOT_FOUND"
}

// storeChecks takes one claimed refund through the rows of that state machine for a claimed refund that need no hub
// call: the approval's expiry and its key, re-verified against the configuration in force (an unknown key ends it
// key_revoked, an expired approval expired_unsent, any other failure raises AlertApproval and changes nothing). It
// returns the refund and whether it is still claimed and may be sent.
func (s *Service) storeChecks(ctx context.Context, id string) (store.RefundState, bool) {
	st, ok := s.snapshot(ctx, id, kindClaimed)
	if !ok {
		return st, false
	}
	now := s.nowSec()
	if !now.Before(st.ExpiresAt) {
		s.finish(ctx, id, kindClaimed, failedWith(store.RefundReasonExpiredUnsent))
		return st, false
	}
	switch s.approvalStanding(st, now) {
	case keyGone:
		s.finish(ctx, id, kindClaimed, failedWith(store.RefundReasonKeyRevoked))
		return st, false
	case expiredNow:
		s.finish(ctx, id, kindClaimed, failedWith(store.RefundReasonExpiredUnsent))
		return st, false
	case broken:
		s.alert(AlertApproval, st)
		return st, false
	}
	if st.AmountMsat <= 0 {
		s.alert(AlertBadAmount, st)
		return st, false
	}
	return st, true
}

// sendOne takes the claimed refund about to be sent, whose amount the budget left covers, through the hub's
// knowledge of the invoice (a hub that knows it ends the refund invoice_known; a failed lookup, or a row of another
// invoice, which also raises AlertLookupOther, marks the refund in lookupFailed), then the ledger pending and the
// send marker (a prepare that raises an ALERT marks it in lookupFailed too), then the one pay_invoice.
func (s *Service) sendOne(ctx context.Context, id string, left int64) {
	st, ok := s.snapshot(ctx, id, kindClaimed)
	if !ok {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, HubCallTimeout)
	tx, err := s.w.LookupInvoiceByString(lctx, st.Invoice)
	cancel()
	switch {
	case err == nil && tx != nil && ofInvoice(st, tx): // the hub knows the invoice, any type or state
		delete(s.lookupFailed, id)
		s.finish(ctx, id, kindClaimed, failedWith(store.RefundReasonInvoiceKnown))
		return
	case err == nil && tx != nil: // a row of another invoice proves nothing: as after a failed lookup
		s.alert(AlertLookupOther, st)
		s.lookupFailed[id] = true
		s.retryLater(id, condHub)
		return
	case !isNotFound(err):
		s.lookupFailed[id] = true
		s.retryLater(id, condHub)
		return
	}
	delete(s.lookupFailed, id)
	s.retryCleared(id)
	if s.crashAt(stepPreChecks) {
		return
	}

	st, p := s.prepare(ctx, id)
	if p == prepSent && left-st.AmountMsat < s.cfg.BudgetAlertMsat { // only for a refund that is really being sent
		s.alert(AlertBudgetLow, st)
	}
	if p == prepAlerted {
		s.lookupFailed[id] = true // passed over while another refund can go, as after a failed lookup
	}
	if p == prepSent {
		// prepare marked the refund in flight; it stays marked, so Void refuses it, until the pay_invoice call has
		// returned or the sender decided not to make it, on every path out of here (a panic too).
		defer func() {
			s.transition.Lock()
			if s.inFlight == id {
				s.inFlight = ""
			}
			s.transition.Unlock()
		}()
	}
	if p != prepSent || s.crashAt(stepMarker) {
		return
	}
	if st.AmountMsat <= 0 { // asserted again at the call
		s.alert(AlertBadAmount, st)
		return
	}
	// The request expires at the stored sent_at + PayTimeout (at most nwc.MaxTimeout), whenever it is built: that bounds
	// its life at the relay. Past that expiry nothing is sent and the refund ends not_sent. Neither depends on a void:
	// while this call can still publish, the in-flight mark makes Void refuse, whatever the clock reads.
	expires := st.SentAt.Add(s.cfg.PayTimeout)
	if !s.nowSec().Before(expires) {
		s.finish(ctx, id, kindSent, failedWith(store.RefundReasonNotSent))
		return
	}
	pctx, cancel := context.WithTimeout(ctx, HubCallTimeout)
	_, err = s.w.PayInvoiceUntil(pctx, st.Invoice, st.AmountMsat, expires)
	cancel()
	if s.crashAt(stepSent) {
		return
	}
	var refused *nwc.RelayRefusedError
	if errors.As(err, &refused) || errors.Is(err, nwc.ErrExpiredBeforeSend) {
		s.finish(ctx, id, kindSent, failedWith(store.RefundReasonNotSent))
	}
	// Anything else, success, ErrNoResponse, ErrClosed and context errors included: the resolver decides.
}

// prepared is how prepare ended.
type prepared int

const (
	prepSent    prepared = iota + 1 // the marker is written: the one pay_invoice follows
	prepAlerted                     // it raised an ALERT: the sender skips the refund like one whose lookup failed
	prepStopped                     // anything else (state changed, store error, expiry, shutdown): nothing to skip
)

// pendingTransfer is the refund's pending transfer, the one every path writes (the sender's prepare, and the void of a
// claimed refund or of a sent one that has none): the merchant's account debited, the hub's credited, the order in
// UserData128.
func pendingTransfer(st store.RefundState) (ledger.Transfer, error) {
	ud, err := ledger.U128FromBytes(func() []byte { b := rawID(st.OrderID); return b[:] }())
	if err != nil {
		return ledger.Transfer{}, err
	}
	return ledger.Transfer{
		ID: RefundTransferID(rawID(st.ID)), DebitAccountID: lnpay.MerchantLightning, CreditAccountID: lnpay.LightningHub,
		Amount: ledger.U128From64(uint64(st.AmountMsat)), UserData128: ud, Ledger: lnpay.LedgerMsat, Code: codeRefund,
		Flags: ledger.TransferPending,
	}, nil
}

// voidTransfer is the transfer that voids the refund's pending one: a zero amount voids the whole pending.
func voidTransfer(rid [16]byte) ledger.Transfer {
	return ledger.Transfer{ID: VoidTransferID(rid), PendingID: RefundTransferID(rid), Ledger: lnpay.LedgerMsat,
		Code: codeRefund, Flags: ledger.TransferVoidPendingTransfer}
}

// createPending writes the refund's pending transfer alone (the sender's prepare, and V1e's). ok is false when the
// transfer could not be built or the ledger did not answer for it.
func (s *Service) createPending(ctx context.Context, st store.RefundState) (r ledger.CreateTransferResult, ok bool) {
	t, err := pendingTransfer(st)
	if err != nil {
		return "", false
	}
	rs, err := s.lg.CreateTransfers(ctx, []ledger.Transfer{t})
	if err != nil || len(rs) != 1 {
		return "", false
	}
	return rs[0], true
}

// prepare writes, under the mutex, the ledger pending and then the send marker of a refund that is still claimed.
// After it returns prepSent the refund is sent (st carries the sent_at written) and only lookups may follow.
func (s *Service) prepare(ctx context.Context, id string) (store.RefundState, prepared) {
	s.transition.Lock()
	defer s.transition.Unlock()
	st, ok := s.readLocked(ctx, id, kindClaimed)
	if !ok {
		return st, prepStopped
	}
	rid := rawID(st.ID)
	posted, voided, err := s.ledgerResolution(ctx, rid)
	switch {
	case err != nil:
		s.retryLater(id, condStore)
		return st, prepStopped
	case posted || voided:
		s.alert(AlertLedgerResolved, st)
		return st, prepAlerted
	}
	switch r, ok := s.createPending(ctx, st); {
	case !ok:
		s.retryLater(id, condStore)
		return st, prepStopped
	case r != ledger.TransferCreated && r != ledger.TransferExists:
		s.alert(AlertLedgerRefused, st)
		return st, prepAlerted
	}
	if s.crashAt(stepPending) {
		return st, prepStopped
	}
	now := s.nowSec()
	switch {
	case !now.Before(st.ExpiresAt):
		return st, prepStopped // the next round ends it expired_unsent and voids the pending
	case ctx.Err() != nil:
		return st, prepStopped // shutting down: no marker that no pay call follows
	}
	if err := s.st.AddRefundSend(ctx, id, now); err != nil {
		if errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrSendExists) {
			s.alert(AlertSendRefused, st)
			return st, prepAlerted
		}
		s.retryLater(id, condStore)
		return st, prepStopped
	}
	st.Sent, st.SentAt = true, now // as written: the pay request's expiry counts from this sent_at
	s.inFlight = id                // in the marker's critical section: Void refuses until the sender clears it
	return st, prepSent
}

// verdict is what one lookup says about a sent refund.
type verdict int

const (
	vSettled verdict = iota + 1
	vFailed
	vPending
	vNotFound
	vExpired
	vAnomaly
	vError
)

// ofInvoice reports whether the lookup row tx is of the refund's own invoice (the hub may send it in capitals).
func ofInvoice(st store.RefundState, tx *nwc.Transaction) bool {
	return strings.ToLower(tx.Invoice) == st.Invoice
}

// classify reads a lookup of a sent refund. An answer counts only if it is outgoing, of the refund's invoice (the
// hub may send it in capitals) and of its amount; anything else is an anomaly, as is a malformed answer.
func classify(st store.RefundState, tx *nwc.Transaction, err error) verdict {
	switch {
	case isNotFound(err):
		return vNotFound
	case errors.Is(err, nwc.ErrMalformed):
		return vAnomaly
	case err != nil:
		return vError
	case tx == nil, tx.Type != nwc.TypeOutgoing, !ofInvoice(st, tx), tx.AmountMsat != st.AmountMsat:
		return vAnomaly
	}
	switch tx.State {
	case nwc.StateSettled:
		return vSettled
	case nwc.StateFailed:
		return vFailed
	case nwc.StatePending:
		return vPending
	case nwc.StateExpired:
		return vExpired
	}
	return vAnomaly
}

// resolveRound is one round of the resolver: every sent refund, oldest sent first, at most one lookup per LookupGap.
func (s *Service) resolveRound(ctx context.Context) {
	s.resolveMu.Lock()
	defer s.resolveMu.Unlock()
	s.applyRetries(ctx)
	open, err := s.st.OpenRefunds(ctx)
	if err != nil {
		// The sender's round key, shared on purpose: only the sender clears it, so a store that fails only here logs
		// again after each good sender round, at most one line per resolver round (30 s). Harmless, and simpler.
		s.retryLater("", condStore)
		return
	}
	var sent []store.RefundState
	for _, st := range open {
		if st.Sent {
			sent = append(sent, st)
		}
	}
	sort.SliceStable(sent, func(i, j int) bool { return sent[i].SentAt.Before(sent[j].SentAt) })
	for i, st := range sent {
		if i > 0 && s.lookupGap > 0 {
			t := time.NewTimer(s.lookupGap)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		if ctx.Err() != nil {
			return
		}
		s.resolveOne(ctx, st.ID)
	}
}

// resolveOne looks up one sent refund and applies the lookup rows of that state machine.
func (s *Service) resolveOne(ctx context.Context, id string) {
	st, ok := s.snapshot(ctx, id, kindSent)
	if !ok {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, HubCallTimeout)
	tx, err := s.w.LookupInvoiceByString(lctx, st.Invoice)
	cancel()
	v := classify(st, tx, err)
	if v != vError {
		s.retryCleared(id)
	}
	switch v {
	case vSettled:
		s.finish(ctx, id, kindSent, outSettled)
	case vFailed:
		s.finish(ctx, id, kindSent, failedWith(store.RefundReasonFailed))
	case vPending:
		if !s.nowSec().Before(st.SentAt.Add(PendingAlertAfter)) {
			s.alert(AlertPending, st)
		}
	case vNotFound:
		// Silent for the first HubCallTimeout after the marker: the request may not have reached the hub yet.
		if !s.nowSec().Before(st.SentAt.Add(HubCallTimeout)) {
			s.alert(AlertNotFound, st)
		}
	case vExpired:
		s.alert(AlertExpired, st)
	case vAnomaly:
		s.alert(AlertAnomaly, st)
	default:
		s.retryLater(id, condHub)
	}
}

// The ledger letters of the void's check 5: what the ledger holds of the refund's pending transfer.
const (
	letterN byte = 'N' // no pending transfer
	letterP byte = 'P' // pending, neither posted nor voided
	letterS byte = 'S' // posted
	letterV byte = 'V' // voided, or both posted and voided: resolved, with no result to match
)

// The void's rows (plan 11, V1).
const (
	rowV1a = "V1a" // claimed, N: the pending is created and voided, cancelled
	rowV1b = "V1b" // claimed, P: the pending is voided, cancelled
	rowV1c = "V1c" // claimed, S: settled, ledger_paid
	rowV1d = "V1d" // sent, P: the hub's fresh answer decides; the pending is posted or voided
	rowV1e = "V1e" // sent, N: the hub's fresh answer decides; settled creates the pending and posts it
)

// voidRow is the one place that says what the void does with a refund that has no result: the row for its store kind
// and ledger letter, or "" for a refusal (a voided pending, and a posted one on a sent refund). Deny by default.
func voidRow(k kind, letter byte) string {
	switch {
	case k == kindClaimed && letter == letterN:
		return rowV1a
	case k == kindClaimed && letter == letterP:
		return rowV1b
	case k == kindClaimed && letter == letterS:
		return rowV1c
	case k == kindSent && letter == letterP:
		return rowV1d
	case k == kindSent && letter == letterN:
		return rowV1e
	}
	return ""
}

// ledgerLetter reads the refund's pending transfer (this package's transfer ids; a post or a void cannot exist
// without the pending).
func (s *Service) ledgerLetter(ctx context.Context, rid [16]byte) (byte, error) {
	if _, found, err := s.lg.LookupTransfer(ctx, RefundTransferID(rid)); err != nil || !found {
		return letterN, err
	}
	posted, voided, err := s.ledgerResolution(ctx, rid)
	switch {
	case err != nil:
		return 0, err
	case voided:
		return letterV, nil
	case posted:
		return letterS, nil
	}
	return letterP, nil
}

var errVoidRead = errors.New("refund: reading the refund failed")

// voidChecks is the void's checks 2 to 5, with the mutex held: the refund exists (ErrNotFound); it has no result and
// no decided result waits for its write (ErrState); its sender cannot still publish it (ErrInFlight); and the ledger
// gives it a row (otherwise ErrState and AlertVoidLedgerResolved, nothing written).
func (s *Service) voidChecks(ctx context.Context, id string) (st store.RefundState, row string, letter byte, err error) {
	st, found, err := s.st.RefundStateByID(ctx, id)
	switch {
	case err != nil:
		return st, "", 0, errVoidRead
	case !found:
		return st, "", 0, ErrNotFound
	}
	if _, waiting := s.retry[id]; waiting || kindOf(st) == kindResolved {
		return st, "", 0, ErrState
	}
	if s.inFlight == id {
		return st, "", 0, ErrInFlight
	}
	if letter, err = s.ledgerLetter(ctx, rawID(id)); err != nil {
		return st, "", 0, errVoidRead
	}
	if row = voidRow(kindOf(st), letter); row == "" {
		s.alert(AlertVoidLedgerResolved, st)
		return st, "", 0, ErrState
	}
	return st, row, letter, nil
}

// Void is a person ending an open refund (plan 11, V1). The void approval must verify and name this refund
// (ErrApproval, ErrApprovalExpired); then voidChecks (ErrNotFound, ErrState, ErrInFlight) and the row they find:
//
//   - V1a to V1c, a claimed refund, make no hub call: the checks, the row's choice and its writes run in one hold of
//     the mutex. The send marker is written under the same mutex before the one pay_invoice, so a refund without a
//     marker was never sent, and a sender that reads after the void finds the result.
//     The ledger moves before the result row, as in every transition: V1a creates the refund's pending and voids it in one
//     ledger batch, applied whole or not at all, so a cancel the store refuses to record survives a restart (recovery
//     ends the refund recovered).
//   - V1d and V1e, a sent refund, answer ErrInFlight before sent_at + VoidAfter (11 minutes), then release the mutex
//     for one fresh lookup, bounded by VoidLookupTimeout, that decides: settled is settled (posting the pending, or
//     creating it first when there is none), failed is failed, NOT_FOUND or expired is voided; pending is ErrInFlight;
//     an error is ErrHubUnavailable, and so is an answer that does not match the refund (with an ALERT: nothing
//     changes). The mutex is taken again for the write, the checks repeat, and a refund whose store kind or ledger
//     letter changed answers ErrState.
//
// A ledger or store failure answers an internal error and leaves the refund as it was; a result the store refuses
// waits with its ledger step. The writes run on a context detached from the caller's, bounded by VoidWriteTimeout, so
// a client that hangs up cannot cut them in half.
func (s *Service) Void(ctx context.Context, id, approvalHex string) (View, error) {
	v, err := s.cfg.Keys.VerifyVoid(approvalHex, s.cfg.Network, s.nowSec())
	switch {
	case errors.Is(err, approval.ErrExpired):
		return View{}, ErrApprovalExpired
	case err != nil, hex.EncodeToString(v.RefundID[:]) != id:
		return View{}, ErrApproval
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), VoidWriteTimeout)
	defer cancel()
	s.transition.Lock()
	st, row, letter, err := s.voidChecks(wctx, id)
	if err != nil {
		s.transition.Unlock()
		return View{}, err
	}
	switch row {
	case rowV1a, rowV1b, rowV1c:
		w, ok := claimedResult(row)
		if !ok {
			s.transition.Unlock()
			return View{}, ErrState
		}
		w.void = true
		r := s.finishHeld(wctx, id, w)
		s.transition.Unlock()
		return s.voided(wctx, id, r)
	case rowV1d, rowV1e:
		s.transition.Unlock()
	default: // deny by default: a row that is not listed writes nothing
		s.transition.Unlock()
		return View{}, ErrState
	}
	if s.nowSec().Before(st.SentAt.Add(VoidAfter)) {
		return View{}, ErrInFlight
	}

	lctx, cancelLookup := context.WithTimeout(ctx, VoidLookupTimeout)
	tx, err := s.w.LookupInvoiceByString(lctx, st.Invoice)
	cancelLookup()
	w := waitingResult{want: kindSent, step: ledgerByOutcome}
	switch classify(st, tx, err) {
	case vSettled:
		w.o = outSettled
	case vFailed:
		w.o = failedWith(store.RefundReasonFailed)
	case vNotFound, vExpired:
		w.o = failedWith(store.RefundReasonVoided)
	case vPending:
		return View{}, ErrInFlight
	case vAnomaly:
		s.alert(AlertAnomaly, st)
		return View{}, ErrHubUnavailable
	default:
		return View{}, ErrHubUnavailable
	}
	w.step, w.void = sentStep(row, w.o), true

	wctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), VoidWriteTimeout)
	defer cancel()
	s.transition.Lock()
	defer s.transition.Unlock()
	cur, _, letterNow, err := s.voidChecks(wctx, id) // the in-flight mark included: never a void while the sender can publish
	switch {
	case err != nil:
		return View{}, err
	case kindOf(cur) != kindOf(st) || letterNow != letter:
		return View{}, ErrState
	}
	return s.voided(wctx, id, s.finishHeld(wctx, id, w))
}

// claimedResult is what V1a to V1c write, stated once: the decided result and its ledger step. V1a: cancelled, the
// pending created and then voided (the ledger moves first; a pending the ledger refuses falls back to the store row
// alone). V1b: cancelled, the pending voided. V1c: settled as ledger_paid, no ledger write (the ledger's post is
// the record). Any other row is refused (false), deny by default: the caller then writes nothing.
func claimedResult(row string) (waitingResult, bool) {
	switch row {
	case rowV1a:
		return waitingResult{want: kindClaimed, o: failedWith(store.RefundReasonCancelled), step: ledgerCreateAndVoid}, true
	case rowV1b:
		return waitingResult{want: kindClaimed, o: failedWith(store.RefundReasonCancelled), step: ledgerByOutcome}, true
	case rowV1c:
		return waitingResult{want: kindClaimed, o: outcome{store.RefundOutcomeSettled, store.RefundReasonLedgerPaid},
			step: ledgerNone}, true
	}
	return waitingResult{}, false
}

// sentStep is the ledger step of V1d and V1e for the result o the lookup decided. V1d (a pending exists) posts or
// voids it by outcome. V1e (none exists) creates it and posts it for settled, and writes no transfer for a failure.
func sentStep(row string, o outcome) ledgerStep {
	switch {
	case row != rowV1e:
		return ledgerByOutcome
	case o == outSettled:
		return ledgerCreateAndPost
	}
	return ledgerNone
}

// voided answers the void after its write r: the refund's view, or the error for r.
func (s *Service) voided(ctx context.Context, id string, r finished) (View, error) {
	switch r {
	case changed:
		return View{}, ErrState
	case stopped:
		return View{}, errors.New("refund: the void could not be completed")
	}
	after, found, err := s.st.RefundStateByID(ctx, id)
	if err != nil || !found {
		return View{}, errVoidRead
	}
	return viewOf(after), nil
}
