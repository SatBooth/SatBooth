package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Refund errors. Callers test them with errors.Is.
var (
	// ErrRefundExists: AddRefund found the refund id already used by a stored refund.
	ErrRefundExists = errors.New("store: refund already exists")
	// ErrRefundInvoiceUsed: AddRefund found the invoice already used by a stored refund of another id (one refund per
	// invoice, ever).
	ErrRefundInvoiceUsed = errors.New("store: refund invoice already used")
	// ErrSendExists: AddRefundSend found the refund's send marker already written.
	ErrSendExists = errors.New("store: refund send marker already exists")
	// ErrResultExists: AddRefundResult found the refund already resolved.
	ErrResultExists = errors.New("store: refund result already exists")
)

// RefundVoidWait is how long after the send marker a void may resolve the refund: 11 minutes, refund.VoidAfter
// (nwc.MaxTimeout + 1 minute), which the refund package's tests pin it to. The SQL trigger is built from it.
const RefundVoidWait = 11 * time.Minute

// The outcomes and reasons of a refund result. A settled refund has reason RefundReasonSettled or
// RefundReasonLedgerPaid; a failed one has one of the others.
const (
	RefundOutcomeSettled = "settled"
	RefundOutcomeFailed  = "failed"

	RefundReasonSettled       = "settled"
	RefundReasonExpiredUnsent = "expired_unsent"
	RefundReasonKeyRevoked    = "key_revoked"
	RefundReasonInvoiceKnown  = "invoice_known"
	RefundReasonNotSent       = "not_sent"
	RefundReasonFailed        = "failed"
	RefundReasonVoided        = "voided"
	RefundReasonRecovered     = "recovered"
	RefundReasonCancelled     = "cancelled"
	RefundReasonLedgerPaid    = "ledger_paid"
)

// Refund is a claimed Lightning refund: the signed approval, the invoice to pay, and what the approval says (checked
// against the approval's bytes by the SQL, amount and expiry by the caller). ID, OrderID and KeyID are lowercase hex
// copies of fields of the approval (32, 32 and 16 characters), Approval is its 296 lowercase hex characters. A zero
// ClaimedAt is stamped from the clock by AddRefund.
type Refund struct {
	ID, OrderID, KeyID, Approval, Invoice string
	AmountMsat                            int64 // 1 to 2^53, a whole number of sats
	ClaimedAt, ExpiresAt                  time.Time
}

// RefundResult ends a refund: Outcome RefundOutcomeSettled with Reason RefundReasonSettled or RefundReasonLedgerPaid,
// or RefundOutcomeFailed with one of the failure reasons (RefundReasonCancelled among them).
type RefundResult struct {
	RefundID, Outcome, Reason string
	At                        time.Time
}

// RefundState is a refund joined with its send marker and its result: the one read the sender, the resolver and the
// void use. Sent is false when there is no marker (SentAt is then zero), Resolved false when there is no result.
type RefundState struct {
	Refund
	SentAt   time.Time
	Sent     bool
	Result   RefundResult
	Resolved bool
}

// validRefundInvoice mirrors checkRefundInvoice.
func validRefundInvoice(s string) bool {
	if len(s) < 2 || len(s) > refundInvoiceMax || s[0] != 'l' || s[1] != 'n' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'z') {
			return false
		}
	}
	return true
}

// validRefundMsat mirrors the amount CHECK of refunds: 1 to 2^53, a multiple of 1000.
func validRefundMsat(n int64) bool { return validAmount(n) && n%1000 == 0 }

// sqlSubstr is SQLite's substr(s, start, n) for ASCII s and a 1-based start of at least 1.
func sqlSubstr(s string, start, n int) string {
	if start > len(s) {
		return ""
	}
	return s[start-1 : min(start-1+n, len(s))]
}

// refundTies mirrors checkRefundTies: every signed field the row repeats equals the approval's bytes. The network byte
// is 00 to 03; the amount and expiry are 16 hex characters of the big-endian uint64 (SQLite's %x prints a negative
// number as its unsigned 64 bits, as uint64 does here).
func refundTies(r Refund) bool {
	switch sqlSubstr(r.Approval, 55, 2) {
	case "00", "01", "02", "03":
	default:
		return false
	}
	return r.ID == sqlSubstr(r.Approval, 73, 32) && r.OrderID == sqlSubstr(r.Approval, 105, 32) && r.KeyID == sqlSubstr(r.Approval, 57, 16) &&
		fmt.Sprintf("%016x", uint64(r.AmountMsat)) == sqlSubstr(r.Approval, 137, 16) &&
		fmt.Sprintf("%016x", uint64(ts(r.ExpiresAt))) == sqlSubstr(r.Approval, 153, 16) &&
		sqlSubstr(r.Approval, 1, 54) == refundDomainHex
}

// refundSendInTime: a send marker is written from the claim up to, not including, the approval's expiry.
func refundSendInTime(claimed, expires, at time.Time) bool {
	return ts(at) >= ts(claimed) && ts(at) < ts(expires)
}

// refundResultInTime mirrors the time triggers of refund_results: not before the claim, not before the send; a void
// waits RefundVoidWait after the send; expired_unsent comes after the approval's expiry.
func refundResultInTime(r RefundResult, claimed, expires, sent time.Time, hasSent bool) bool {
	at := ts(r.At)
	switch {
	case at < ts(claimed):
		return false
	case hasSent && at < ts(sent):
		return false
	case hasSent && r.Reason == RefundReasonVoided && at < ts(sent)+int64(RefundVoidWait/time.Second):
		return false
	case r.Reason == RefundReasonExpiredUnsent && at < ts(expires):
		return false
	}
	return true
}

// validRefund mirrors the CHECKs of refunds, for a claimed time that is set.
func validRefund(r Refund) error {
	switch {
	case !validLowerHex(r.ID, 32):
		return invalid("refund id must be 32 lowercase hex characters")
	case !validLowerHex(r.OrderID, 32):
		return invalid("order id must be 32 lowercase hex characters")
	case !validLowerHex(r.KeyID, 16):
		return invalid("key id must be 16 lowercase hex characters")
	case !validLowerHex(r.Approval, refundApprovalHex):
		return invalid("approval must be %d lowercase hex characters", refundApprovalHex)
	case !refundTies(r):
		return invalid("the refund's fields must equal those in the approval, which must start with the refund domain and a network byte")
	case !validRefundInvoice(r.Invoice):
		return invalid("refund invoice must be 2 to %d characters of [0-9a-z] starting ln", refundInvoiceMax)
	case !validRefundMsat(r.AmountMsat):
		return invalid("refund amount must be a whole number of sats, 1000 msat to 2^53")
	case ts(r.ClaimedAt) <= 0:
		return invalid("claimed time must be after 1970")
	case ts(r.ExpiresAt) <= ts(r.ClaimedAt):
		return invalid("refund expiry must be after the claimed time")
	}
	return nil
}

// validRefundResult mirrors the CHECKs of refund_results.
func validRefundResult(r RefundResult) error {
	if !validLowerHex(r.RefundID, 32) {
		return invalid("refund id must be 32 lowercase hex characters")
	}
	if ts(r.At) <= 0 {
		return invalid("result time must be after 1970")
	}
	if !validRefundOutcomeReason(r.Outcome, r.Reason) {
		return invalid("outcome and reason do not go together")
	}
	return nil
}

// validRefundOutcomeReason mirrors the CHECK pairs: settled with settled or ledger_paid; failed with one of the
// failure reasons.
func validRefundOutcomeReason(outcome, reason string) bool {
	switch outcome {
	case RefundOutcomeSettled:
		return reason == RefundReasonSettled || reason == RefundReasonLedgerPaid
	case RefundOutcomeFailed:
		switch reason {
		case RefundReasonExpiredUnsent, RefundReasonKeyRevoked, RefundReasonInvoiceKnown, RefundReasonNotSent,
			RefundReasonFailed, RefundReasonVoided, RefundReasonRecovered, RefundReasonCancelled:
			return true
		}
	}
	return false
}

// refundNeedsSend: the reasons that need a send row; refundNeedsNoSend: the reasons that need none. Recovered is in
// neither.
func refundNeedsSend(reason string) bool {
	switch reason {
	case RefundReasonSettled, RefundReasonNotSent, RefundReasonFailed, RefundReasonVoided:
		return true
	}
	return false
}

func refundNeedsNoSend(reason string) bool {
	switch reason {
	case RefundReasonExpiredUnsent, RefundReasonKeyRevoked, RefundReasonInvoiceKnown, RefundReasonCancelled, RefundReasonLedgerPaid:
		return true
	}
	return false
}

const refundCols = `r.id, r.order_id, r.key_id, r.approval, r.invoice, r.amount_msat, r.claimed_at, r.expires_at`

// refundStateCols adds the send marker and the result (both LEFT JOINed as s and x).
const refundStateCols = refundCols + `, s.sent_at, x.outcome, x.reason, x.resolved_at`
const refundStateFrom = ` FROM refunds r LEFT JOIN refund_sends s ON s.refund_id = r.id LEFT JOIN refund_results x ON x.refund_id = r.id`

func scanRefund(sc interface{ Scan(...any) error }) (Refund, error) {
	var r Refund
	var claimed, expires int64
	if err := sc.Scan(&r.ID, &r.OrderID, &r.KeyID, &r.Approval, &r.Invoice, &r.AmountMsat, &claimed, &expires); err != nil {
		return Refund{}, err
	}
	r.ClaimedAt, r.ExpiresAt = fromTS(claimed), fromTS(expires)
	return r, nil
}

func scanRefundState(sc interface{ Scan(...any) error }) (RefundState, error) {
	var st RefundState
	var claimed, expires int64
	var sent, resolved sql.NullInt64
	var outcome, reason sql.NullString
	if err := sc.Scan(&st.ID, &st.OrderID, &st.KeyID, &st.Approval, &st.Invoice, &st.AmountMsat, &claimed, &expires,
		&sent, &outcome, &reason, &resolved); err != nil {
		return RefundState{}, err
	}
	st.ClaimedAt, st.ExpiresAt = fromTS(claimed), fromTS(expires)
	if sent.Valid {
		st.Sent, st.SentAt = true, fromTS(sent.Int64)
	}
	if resolved.Valid {
		st.Resolved = true
		st.Result = RefundResult{RefundID: st.ID, Outcome: outcome.String, Reason: reason.String, At: fromTS(resolved.Int64)}
	}
	return st, nil
}

// AddRefund stores a claimed refund. The order must exist (ErrNotFound); a refund id already used by a stored refund
// is ErrRefundExists, and an invoice already used by a stored refund of another id ErrRefundInvoiceUsed. Everything
// else invalid is ErrInvalid, before anything is written; the SQL checks and triggers refuse the same inputs again.
func (s *Store) AddRefund(ctx context.Context, r Refund) error {
	if r.ClaimedAt.IsZero() {
		r.ClaimedAt = s.now()
	}
	if err := validRefund(r); err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM orders WHERE id = ?`, r.OrderID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM refunds WHERE id = ?`, r.ID).Scan(&one)
		if err == nil {
			return ErrRefundExists
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM refunds WHERE invoice = ?`, r.Invoice).Scan(&one)
		if err == nil {
			return ErrRefundInvoiceUsed
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO refunds (id, order_id, key_id, approval, invoice, amount_msat, claimed_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.ID, r.OrderID, r.KeyID, r.Approval, r.Invoice, r.AmountMsat, ts(r.ClaimedAt), ts(r.ExpiresAt))
		return err
	})
}

// RefundByID returns the refund, or false if there is none.
func (s *Store) RefundByID(ctx context.Context, id string) (Refund, bool, error) {
	if !validLowerHex(id, 32) {
		return Refund{}, false, invalid("refund id must be 32 lowercase hex characters")
	}
	r, err := scanRefund(s.db.QueryRowContext(ctx, `SELECT `+refundCols+` FROM refunds r WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Refund{}, false, nil
	}
	return r, err == nil, err
}

// OpenRefunds returns every refund that has no result row, oldest claimed first.
func (s *Store) OpenRefunds(ctx context.Context) ([]RefundState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+refundStateCols+refundStateFrom+` WHERE x.refund_id IS NULL ORDER BY r.claimed_at, r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefundState
	for rows.Next() {
		st, err := scanRefundState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// RefundStateByID returns the refund with its send marker and result in one read, or false if there is no refund.
func (s *Store) RefundStateByID(ctx context.Context, id string) (RefundState, bool, error) {
	if !validLowerHex(id, 32) {
		return RefundState{}, false, invalid("refund id must be 32 lowercase hex characters")
	}
	st, err := scanRefundState(s.db.QueryRowContext(ctx, `SELECT `+refundStateCols+refundStateFrom+` WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RefundState{}, false, nil
	}
	return st, err == nil, err
}

// RefundFigures returns the order's settled msat (refunds with result outcome settled) and open msat (refunds with no
// result row). Failed refunds count in neither.
func (s *Store) RefundFigures(ctx context.Context, orderID string) (settledMsat, openMsat int64, err error) {
	if !validLowerHex(orderID, 32) {
		return 0, 0, invalid("order id must be 32 lowercase hex characters")
	}
	err = s.db.QueryRowContext(ctx, `SELECT
coalesce(sum(CASE WHEN x.outcome = 'settled' THEN r.amount_msat END), 0),
coalesce(sum(CASE WHEN x.refund_id IS NULL THEN r.amount_msat END), 0)
FROM refunds r LEFT JOIN refund_results x ON x.refund_id = r.id WHERE r.order_id = ?`, orderID).Scan(&settledMsat, &openMsat)
	return settledMsat, openMsat, err
}

// RefundMsatClaimedSince returns the sum of the amounts of the refunds claimed at or after t (whole seconds) that are
// not failed (open or settled): what the daily cap counts.
func (s *Store) RefundMsatClaimedSince(ctx context.Context, t time.Time) (int64, error) {
	var sum int64
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(sum(r.amount_msat), 0)
FROM refunds r LEFT JOIN refund_results x ON x.refund_id = r.id
WHERE r.claimed_at >= ? AND (x.refund_id IS NULL OR x.outcome != 'failed')`, ts(t)).Scan(&sum)
	return sum, err
}

// AddRefundSend writes the refund's send marker at time at. The refund must exist (ErrNotFound) and have no result
// (ErrInvalid); a second marker is ErrSendExists. The time is from the claim up to, not including, the approval's
// expiry (ErrInvalid): an expired approval is never sent.
func (s *Store) AddRefundSend(ctx context.Context, id string, at time.Time) error {
	switch {
	case !validLowerHex(id, 32):
		return invalid("refund id must be 32 lowercase hex characters")
	case ts(at) <= 0:
		return invalid("send time must be after 1970")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var claimed, expires int64
		if err := tx.QueryRowContext(ctx, `SELECT claimed_at, expires_at FROM refunds WHERE id = ?`, id).Scan(&claimed, &expires); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM refund_sends WHERE refund_id = ?`, id).Scan(&one)
		if err == nil {
			return ErrSendExists
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM refund_results WHERE refund_id = ?`, id).Scan(&one)
		if err == nil {
			return invalid("refund is already resolved")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !refundSendInTime(fromTS(claimed), fromTS(expires), at) {
			return invalid("send time must be from the claim up to the approval's expiry")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO refund_sends (refund_id, sent_at) VALUES (?, ?)`, id, ts(at))
		return err
	})
}

// AddRefundResult resolves the refund. The refund must exist (ErrNotFound); a second result is ErrResultExists. The
// reasons settled, not_sent, failed and voided need a send marker; expired_unsent, key_revoked, invoice_known,
// cancelled and ledger_paid need none (ErrInvalid otherwise); the time must not precede the claim or the send, a void waits RefundVoidWait after the
// send and expired_unsent follows the approval's expiry (ErrInvalid). The triggers refuse the same.
func (s *Store) AddRefundResult(ctx context.Context, r RefundResult) error {
	if err := validRefundResult(r); err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var claimed, expires int64
		if err := tx.QueryRowContext(ctx, `SELECT claimed_at, expires_at FROM refunds WHERE id = ?`, r.RefundID).Scan(&claimed, &expires); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM refund_results WHERE refund_id = ?`, r.RefundID).Scan(&one)
		if err == nil {
			return ErrResultExists
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		sent := true
		var sentAt int64
		err = tx.QueryRowContext(ctx, `SELECT sent_at FROM refund_sends WHERE refund_id = ?`, r.RefundID).Scan(&sentAt)
		if errors.Is(err, sql.ErrNoRows) {
			sent = false
		} else if err != nil {
			return err
		}
		if (refundNeedsSend(r.Reason) && !sent) || (refundNeedsNoSend(r.Reason) && sent) {
			return invalid("reason %q does not fit the refund's send state", r.Reason)
		}
		if !refundResultInTime(r, fromTS(claimed), fromTS(expires), fromTS(sentAt), sent) {
			return invalid("result time does not fit the claim, the send, the void wait or the approval's expiry")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO refund_results (refund_id, outcome, reason, resolved_at) VALUES (?, ?, ?, ?)`,
			r.RefundID, r.Outcome, r.Reason, ts(r.At))
		return err
	})
}

// InvoiceIssued reports whether bolt11 is one of SatBooth's own invoices, comparing lower(bolt11) (the
// invoices_bolt11_lower index). A claimant invoice that is one of ours is refused by the caller.
func (s *Store) InvoiceIssued(ctx context.Context, bolt11 string) (bool, error) {
	if !validBolt11(bolt11) {
		return false, invalid("payment request must be 1 to 4096 printable ASCII characters without spaces")
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM invoices WHERE lower(bolt11) = lower(?)`, bolt11).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
