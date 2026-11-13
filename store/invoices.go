package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const invoiceCols = `order_id, payment_hash, bolt11, amount_msat, created_at, expires_at`

// invoiceColsOf is invoiceCols qualified by a table alias.
func invoiceColsOf(alias string) string {
	return alias + `.order_id, ` + alias + `.payment_hash, ` + alias + `.bolt11, ` + alias + `.amount_msat, ` +
		alias + `.created_at, ` + alias + `.expires_at`
}

func collectInvoices(rows *sql.Rows) ([]Invoice, error) {
	defer rows.Close()
	var out []Invoice
	for rows.Next() {
		var i Invoice
		var created, expires int64
		if err := rows.Scan(&i.OrderID, &i.PaymentHash, &i.Bolt11, &i.AmountMsat, &created, &expires); err != nil {
			return nil, err
		}
		i.CreatedAt, i.ExpiresAt = fromTS(created), fromTS(expires)
		out = append(out, i)
	}
	return out, rows.Err()
}

// AddInvoice stores an invoice for an existing order. The order must exist (ErrNotFound) and the
// payment hash must be new (ErrDuplicate). The amount must equal the order's (ErrInvalid), in Go
// and in a trigger. A zero CreatedAt is stamped from the clock.
func (s *Store) AddInvoice(ctx context.Context, inv Invoice) error {
	switch {
	case !validLowerHex(inv.OrderID, 32):
		return invalid("order id must be 32 lowercase hex characters")
	case !validLowerHex(inv.PaymentHash, 64):
		return invalid("payment hash must be 64 lowercase hex characters")
	case !validBolt11(inv.Bolt11):
		return invalid("payment request must be 1 to 4096 printable ASCII characters without spaces")
	case !validAmount(inv.AmountMsat):
		return invalid("amount must be 1 to 2^53 msat")
	case inv.ExpiresAt.IsZero():
		return invalid("expiry is required")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		created := inv.CreatedAt
		if created.IsZero() {
			created = s.now()
		}
		if ts(inv.ExpiresAt) <= ts(created) {
			return invalid("expiry must be after the creation time")
		}
		var orderAmount int64
		if err := tx.QueryRowContext(ctx, `SELECT amount_msat FROM orders WHERE id = ?`, inv.OrderID).Scan(&orderAmount); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if inv.AmountMsat != orderAmount {
			return invalid("invoice amount must equal the order amount")
		}
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM invoices WHERE payment_hash = ?`, inv.PaymentHash).Scan(&one)
		if err == nil {
			return ErrDuplicate
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO invoices (`+invoiceCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
			inv.OrderID, inv.PaymentHash, inv.Bolt11, inv.AmountMsat, ts(created), ts(inv.ExpiresAt))
		return err
	})
}

// newestInvoiceSQL is the query of NewestInvoice, shared with the test of its plan.
const newestInvoiceSQL = `SELECT bolt11 FROM invoices ORDER BY created_at DESC, payment_hash DESC LIMIT 1`

// NewestInvoice returns the text of the invoice stored last, or ok false if the store holds none. "Last" is the latest
// creation time, then the greater payment hash.
func (s *Store) NewestInvoice(ctx context.Context) (bolt11 string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, newestInvoiceSQL).Scan(&bolt11)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return bolt11, true, nil
}

// InvoicesForOrder returns the order's invoices, oldest first (none if the order is unknown).
func (s *Store) InvoicesForOrder(ctx context.Context, orderID string) ([]Invoice, error) {
	if !validLowerHex(orderID, 32) {
		return nil, invalid("order id must be 32 lowercase hex characters")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+invoiceCols+` FROM invoices WHERE order_id = ? ORDER BY created_at, payment_hash`, orderID)
	if err != nil {
		return nil, err
	}
	return collectInvoices(rows)
}

// OpenInvoices returns every invoice that has no closed_invoices row, oldest first. Open means
// "not closed": the caller checks the ledger for a paid transfer separately.
func (s *Store) OpenInvoices(ctx context.Context) ([]Invoice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+invoiceCols+` FROM invoices i
WHERE NOT EXISTS (SELECT 1 FROM closed_invoices c WHERE c.payment_hash = i.payment_hash)
ORDER BY created_at, payment_hash`)
	if err != nil {
		return nil, err
	}
	return collectInvoices(rows)
}

// CloseInvoice records that the invoice is closed, at time at, for reason CloseLookup or
// CloseDeadline. It returns true if it wrote the row and false if the invoice was already closed
// (the first row stands, whatever the second reason), so it is idempotent by hash. An unknown
// hash is ErrNotFound.
//
// The store refuses a close that is too early, so a bug cannot let an order expire with a live
// invoice. CloseLookup needs at >= the invoice's ExpiresAt. CloseDeadline needs at >= ExpiresAt +
// DeadlineGrace. Neither may be later than the store clock's now.
//
// When the caller (lnpay) closes: with CloseLookup after a successful lookup, made after ExpiresAt
// plus its grace, shows the invoice not settled; with CloseDeadline only when, past the deadline,
// the wallet answers NOT_FOUND. Any other answer leaves the invoice open, however late. A closure
// ends the invoice's hold on its order, not the watch for its payment: lnpay still looks a
// CloseLookup invoice up until its deadline and records a late settlement.
func (s *Store) CloseInvoice(ctx context.Context, paymentHash string, at time.Time, reason string) (bool, error) {
	switch {
	case !validLowerHex(paymentHash, 64):
		return false, invalid("payment hash must be 64 lowercase hex characters")
	case reason != CloseLookup && reason != CloseDeadline:
		return false, invalid("reason must be %q or %q", CloseLookup, CloseDeadline)
	case at.IsZero():
		return false, invalid("close time is required")
	}
	var wrote bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		var expires int64
		if err := tx.QueryRowContext(ctx, `SELECT expires_at FROM invoices WHERE payment_hash = ?`, paymentHash).Scan(&expires); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM closed_invoices WHERE payment_hash = ?`, paymentHash).Scan(&one)
		if err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		earliest := expires
		if reason == CloseDeadline {
			earliest += int64(DeadlineGrace / time.Second)
		}
		if ts(at) < earliest {
			return invalid("invoice closed before its time")
		}
		if ts(at) > ts(s.now()) {
			return invalid("close time is in the future")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO closed_invoices (payment_hash, closed_at, reason) VALUES (?, ?, ?)`,
			paymentHash, ts(at), reason); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	return wrote, err
}

// InvoicesClosedByLookup returns the invoices closed with reason CloseLookup whose ExpiresAt is
// after expiresAfter (in whole seconds, as every stored time), oldest expiry first. lnpay keeps
// looking such an invoice up until its deadline, because the wallet can still settle it after its
// expiry; at startup it passes now - DeadlineGrace to find those still in their window.
func (s *Store) InvoicesClosedByLookup(ctx context.Context, expiresAfter time.Time) ([]Invoice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+invoiceColsOf("i")+` FROM invoices i
JOIN closed_invoices c ON c.payment_hash = i.payment_hash
WHERE c.reason = 'lookup' AND i.expires_at > ?
ORDER BY i.expires_at, i.payment_hash`, ts(expiresAfter))
	if err != nil {
		return nil, err
	}
	return collectInvoices(rows)
}

// ClosuresForOrder returns the closed_invoices rows of the order's invoices (InvoicesForOrder alone
// cannot say which of an order's invoices are open).
func (s *Store) ClosuresForOrder(ctx context.Context, orderID string) ([]Closure, error) {
	if !validLowerHex(orderID, 32) {
		return nil, invalid("order id must be 32 lowercase hex characters")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.payment_hash, c.closed_at, c.reason FROM invoices i
JOIN closed_invoices c ON c.payment_hash = i.payment_hash WHERE i.order_id = ? ORDER BY c.closed_at, c.payment_hash`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Closure
	for rows.Next() {
		var c Closure
		var at int64
		if err := rows.Scan(&c.PaymentHash, &at, &c.Reason); err != nil {
			return nil, err
		}
		c.At = fromTS(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClosedInvoicesWithoutPaid returns the closed invoices (any reason) with closed_at at or after
// since whose order has no order.paid event, oldest closure first.
func (s *Store) ClosedInvoicesWithoutPaid(ctx context.Context, since time.Time) ([]Invoice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+invoiceColsOf("i")+` FROM invoices i
JOIN closed_invoices c ON c.payment_hash = i.payment_hash
WHERE c.closed_at >= ?
  AND NOT EXISTS (SELECT 1 FROM events e WHERE e.order_id = i.order_id AND e.type = 'order.paid')
ORDER BY c.closed_at, i.payment_hash`, ts(since))
	if err != nil {
		return nil, err
	}
	return collectInvoices(rows)
}

// AddInvoiceAttempt records one MakeInvoice attempt for the order, at time at, before the call
// is made. The attempt carries the order's amount. (A delivery attempt of a webhook is a
// different thing: AddAttempt.) An unknown order is ErrNotFound.
func (s *Store) AddInvoiceAttempt(ctx context.Context, orderID string, at time.Time) error {
	if !validLowerHex(orderID, 32) {
		return invalid("order id must be 32 lowercase hex characters")
	}
	if at.IsZero() {
		return invalid("attempt time is required")
	}
	id, err := randHex()
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO invoice_attempts (id, order_id, at, amount_msat)
SELECT ?, id, ?, amount_msat FROM orders WHERE id = ?`, id, ts(at), orderID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// AttemptsSince returns how many invoice attempts were recorded at or after t, and the sum of
// their order amounts in msat: the figures for the daily limits.
func (s *Store) AttemptsSince(ctx context.Context, t time.Time) (count int, msat int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(amount_msat), 0) FROM invoice_attempts WHERE at >= ?`, ts(t)).Scan(&count, &msat)
	return count, msat, err
}
