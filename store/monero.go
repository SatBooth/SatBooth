package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The Monero rail's store: the twin of onchain.go. A payment is keyed by (txid, subaddress), as wallet-rpc's
// get_transfers reports one amount per transaction and subaddress; everything else has the on-chain semantics.

// AddSubaddress stores the subaddress issued for an order. ErrNotFound: unknown order. ErrSubaddressTaken: the order
// already has a subaddress, or the subaddress is stored. ErrInvalid: a malformed subaddress, an index outside 1 to
// MaxSubaddressIdx (index 0, the primary address, is never stored), an order without a Monero amount, or
// (ErrIndexNotAbove) an Index that is not strictly above MaxSubaddressIndex. The checks are made in the write
// transaction and again by triggers.
func (s *Store) AddSubaddress(ctx context.Context, a Subaddress) error {
	switch {
	case !validLowerHex(a.OrderID, 32):
		return invalid("order id must be 32 lowercase hex characters")
	case !ValidSubaddress(a.Address):
		return invalid("subaddress must be 95 base58 characters starting 8, B or 7")
	case a.Index < 1 || a.Index > MaxSubaddressIdx:
		return invalid("subaddress index must be 1 to 2147483647")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var pico int64
		if err := tx.QueryRowContext(ctx, `SELECT amount_piconero FROM orders WHERE id = ?`, a.OrderID).Scan(&pico); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if pico == 0 {
			return invalid("the order has no Monero amount")
		}
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM monero_subaddresses WHERE order_id = ? OR address = ?`, a.OrderID, a.Address).Scan(&one)
		if err == nil {
			return ErrSubaddressTaken
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var max sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT max(idx) FROM monero_subaddresses`).Scan(&max); err != nil {
			return err
		}
		if max.Valid && a.Index <= max.Int64 {
			return ErrIndexNotAbove
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO monero_subaddresses (address, order_id, idx, created_at) VALUES (?, ?, ?, ?)`,
			a.Address, a.OrderID, a.Index, ts(s.now()))
		return err
	})
}

const subaddressCols = `address, order_id, idx, created_at`

func scanSubaddress(sc interface{ Scan(...any) error }) (Subaddress, error) {
	var a Subaddress
	var created int64
	if err := sc.Scan(&a.Address, &a.OrderID, &a.Index, &created); err != nil {
		return Subaddress{}, err
	}
	a.CreatedAt = fromTS(created)
	return a, nil
}

// SubaddressForOrder returns the order's subaddress, or ErrNotFound.
func (s *Store) SubaddressForOrder(ctx context.Context, orderID string) (Subaddress, error) {
	if !validLowerHex(orderID, 32) {
		return Subaddress{}, ErrNotFound
	}
	a, err := scanSubaddress(s.db.QueryRowContext(ctx, `SELECT `+subaddressCols+` FROM monero_subaddresses WHERE order_id = ?`, orderID))
	if errors.Is(err, sql.ErrNoRows) {
		return Subaddress{}, ErrNotFound
	}
	return a, err
}

// SubaddressesByAddress returns the stored subaddresses among addrs, keyed by address, read in batches of sqlChunk.
func (s *Store) SubaddressesByAddress(ctx context.Context, addrs []string) (map[string]Subaddress, error) {
	out := make(map[string]Subaddress)
	for len(addrs) > 0 {
		n := min(len(addrs), sqlChunk)
		args := make([]any, n)
		for i, a := range addrs[:n] {
			args[i] = a
		}
		rows, err := s.db.QueryContext(ctx, `SELECT `+subaddressCols+` FROM monero_subaddresses WHERE address IN (`+
			placeholders(n)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			a, err := scanSubaddress(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[a.Address] = a
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		addrs = addrs[n:]
	}
	return out, nil
}

// placeholders returns n comma-separated host parameters.
func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// MaxSubaddressIndex returns the highest subaddress index issued; ok is false when none is stored.
func (s *Store) MaxSubaddressIndex(ctx context.Context) (idx int64, ok bool, err error) {
	var m sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT max(idx) FROM monero_subaddresses`).Scan(&m); err != nil {
		return 0, false, err
	}
	return m.Int64, m.Valid, nil
}

// UnusedSubaddressRun is the number of issued subaddress indexes above the highest one that has received a payment
// (or above 0, the primary address, when none has): the run a wallet restored from its keys must look ahead past.
// With no subaddresses it is 0. With indexes 1..5 and none paid it is 5; with a payment at index 2 it is 3.
func (s *Store) UnusedSubaddressRun(ctx context.Context) (int64, error) {
	var max, used sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT max(idx) FROM monero_subaddresses`).Scan(&max); err != nil {
		return 0, err
	}
	if !max.Valid {
		return 0, nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT max(a.idx) FROM monero_subaddresses a
WHERE EXISTS (SELECT 1 FROM monero_payments p WHERE p.address = a.address)`).Scan(&used); err != nil {
		return 0, err
	}
	return max.Int64 - used.Int64, nil // used.Int64 is 0 when no subaddress has a payment
}

func validPayment(p Payment) error {
	switch {
	case !validLowerHex(p.TxID, 64):
		return invalid("txid must be 64 lowercase hex characters")
	case !ValidSubaddress(p.Address):
		return invalid("payment subaddress is malformed")
	case !validPiconero(p.AmountPiconero):
		return invalid("payment amount must be 1 to 9007199254740991 piconero")
	case p.UnlockTime < 0:
		return invalid("payment unlock time must not be negative")
	case ts(p.FirstSeen) < 1: // the zero time too: the caller's time is stored (ruling S3)
		return invalid("payment first seen must be after 1970-01-01T00:00:00Z")
	}
	return nil
}

// RecordMoneroScan stores the payments of one wallet scan and the scan's cursor, in one transaction: CommitMoneroPass
// without marks, with every payment's FirstSeen stamped from the clock (the caller's value is not read). It inserts
// every (txid, subaddress) not already stored and returns the rows it added. FirstSeenHeight is cursor.WalletHeight
// (the wallet's own height, never the scan position, ruling R6), both for the new rows only. A stored payment
// keeps its row; one listed with another amount or unlock time is ErrPaymentChanged (an ErrInvalid: wallet-rpc and
// the store disagree) and nothing is written. A stored payment listed Confirmed that is settled with a dead current mark is reopened, in the
// same transaction (the rule of RecordScan, reopenIfDead). A subaddress that is not stored is ErrNotFound and
// nothing is written: the caller filters first. The cursor row (next seq) is added only if a payment was added or
// the height or the wallet height differs from the latest stored one, so a quiet wallet adds no rows. A negative
// height, or a wallet height below 0 or above the height, is ErrInvalid.
// Confirmed is read only: the rows returned have it false.
func (s *Store) RecordMoneroScan(ctx context.Context, ps []Payment, cursor MoneroCursor) ([]Payment, error) {
	now := s.now()
	stamped := make([]Payment, len(ps))
	for i, p := range ps {
		p.FirstSeen = now
		stamped[i] = p
	}
	return s.CommitMoneroPass(ctx, stamped, cursor, nil, nil)
}

// CommitMoneroPass is the one write of a watcher pass (ruling 8-A6): RecordMoneroScan's scan and cursor, then each
// mark (AddMoneroMark's rule, in order), all in one transaction. A mark may be for a payment the same scan adds. A new
// row's FirstSeen is the caller's (ruling S3: the watcher's clock when its discovery first saw the payment, not the
// commit's), and every payment must carry one after 1970 (ErrInvalid); a stored row keeps its own. Each order in
// pending is recorded as pending settle in the same transaction (ruling S4: the orders the pass touched), unless it
// already is, so a crash after the commit leaves them for the next pass (MoneroPendingSettle). Any refusal
// (RecordMoneroScan's, a mark of the wrong shape, ErrInvalid, or of a payment that is neither stored nor added,
// ErrNotFound, a malformed order id, ErrInvalid, or an unknown one, ErrNotFound) writes nothing. It returns the
// payments the scan added.
func (s *Store) CommitMoneroPass(ctx context.Context, ps []Payment, cursor MoneroCursor, marks []MoneroMark, pending []string) ([]Payment, error) {
	if cursor.Height < 0 {
		return nil, invalid("cursor height must not be negative")
	}
	if cursor.WalletHeight < 0 || cursor.WalletHeight > cursor.Height {
		return nil, invalid("cursor wallet height must be 0 to the cursor height")
	}
	for _, p := range ps {
		if err := validPayment(p); err != nil {
			return nil, err
		}
	}
	for _, m := range marks {
		if err := validMoneroMark(m); err != nil {
			return nil, err
		}
	}
	for _, id := range pending {
		if !validLowerHex(id, 32) {
			return nil, invalid("order id must be 32 lowercase hex characters")
		}
	}
	var added []Payment
	err := s.write(ctx, func(tx *sql.Tx) error {
		var err error
		if added, err = s.recordMoneroScan(ctx, tx, ps, cursor); err != nil {
			return err
		}
		for _, m := range marks {
			if err := s.addMoneroMark(ctx, tx, m); err != nil {
				return err
			}
		}
		for _, id := range pending {
			if err := s.addPendingSettle(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return added, nil
}

// recordMoneroScan is RecordMoneroScan's write, inside tx.
func (s *Store) recordMoneroScan(ctx context.Context, tx *sql.Tx, ps []Payment, cursor MoneroCursor) ([]Payment, error) {
	var added []Payment
	now := ts(s.now())
	for _, p := range ps {
		var oldPico, oldUnlock int64
		err := tx.QueryRowContext(ctx, `SELECT amount_piconero, unlock_time FROM monero_payments WHERE txid = ? AND address = ?`,
			p.TxID, p.Address).Scan(&oldPico, &oldUnlock)
		if err == nil {
			if oldPico != p.AmountPiconero || oldUnlock != p.UnlockTime {
				var idx int64
				if err := tx.QueryRowContext(ctx, `SELECT idx FROM monero_subaddresses WHERE address = ?`, p.Address).Scan(&idx); err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("%w (txid %s, subaddress index %d)", ErrPaymentChanged, p.TxID, idx)
			}
			if p.Confirmed {
				if err := reopenIfDead(ctx, tx, "monero_settled", "monero_marks", "txid = ? AND address = ?", p.TxID, p.Address); err != nil {
					return nil, err
				}
			}
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM monero_subaddresses WHERE address = ?`, p.Address).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		} else if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO monero_payments (txid, address, amount_piconero, unlock_time, first_seen, first_seen_height)
VALUES (?, ?, ?, ?, ?, ?)`, p.TxID, p.Address, p.AmountPiconero, p.UnlockTime, ts(p.FirstSeen), cursor.WalletHeight); err != nil {
			return nil, err
		}
		p.FirstSeen, p.FirstSeenHeight, p.Confirmed = fromTS(ts(p.FirstSeen)), cursor.WalletHeight, false
		added = append(added, p)
	}
	var seq int64
	var height, walletHeight sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT seq, height, wallet_height FROM monero_cursor ORDER BY seq DESC LIMIT 1`).Scan(&seq, &height, &walletHeight); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if len(added) == 0 && height.Valid && height.Int64 == cursor.Height && walletHeight.Int64 == cursor.WalletHeight {
		return nil, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO monero_cursor (seq, height, wallet_height, at) VALUES (?, ?, ?, ?)`, seq+1, cursor.Height,
		cursor.WalletHeight, now); err != nil {
		return nil, err
	}
	return added, nil
}

// addPendingSettle records an order as pending settle inside tx, unless it already is. ErrNotFound: unknown order.
func (s *Store) addPendingSettle(ctx context.Context, tx *sql.Tx, orderID string) error {
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM orders WHERE id = ?`, orderID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO monero_pending_settle (order_id, at) SELECT ?, ?
WHERE NOT EXISTS (SELECT 1 FROM monero_pending_settle WHERE order_id = ?)`, orderID, ts(s.now()), orderID)
	return err
}

// MoneroPendingSettle returns the orders recorded as pending settle (CommitMoneroPass), whatever their window and
// events: the watcher settles each of them on its next pass (ruling S4). Oldest expiry first.
func (s *Store) MoneroPendingSettle(ctx context.Context) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+orderCols+` FROM orders o
WHERE EXISTS (SELECT 1 FROM monero_pending_settle q WHERE q.order_id = o.id)
ORDER BY o.expires_at, o.id`)
	if err != nil {
		return nil, err
	}
	return collectOrders(rows)
}

// ClearMoneroPendingSettle deletes the pending-settle rows of the orders in ids (those that have one), in batches of
// sqlChunk, in one transaction: the watcher calls it once the orders have settled. A malformed id is ErrInvalid and
// nothing is deleted.
func (s *Store) ClearMoneroPendingSettle(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if !validLowerHex(id, 32) {
			return invalid("order id must be 32 lowercase hex characters")
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		for rest := ids; len(rest) > 0; {
			n := min(len(rest), sqlChunk)
			args := make([]any, n)
			for i, id := range rest[:n] {
				args[i] = id
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM monero_pending_settle WHERE order_id IN (`+placeholders(n)+`)`, args...); err != nil {
				return err
			}
			rest = rest[n:]
		}
		return nil
	})
}

// MoneroCursor returns the latest cursor row; ok is false before the first scan.
func (s *Store) MoneroCursor(ctx context.Context) (MoneroCursor, bool, error) {
	var c MoneroCursor
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT height, wallet_height, at FROM monero_cursor ORDER BY seq DESC LIMIT 1`).Scan(&c.Height,
		&c.WalletHeight, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return MoneroCursor{}, false, nil
	}
	if err != nil {
		return MoneroCursor{}, false, err
	}
	c.At = fromTS(at)
	return c, true, nil
}

// paymentCols names its columns, so that the ORDER BY of a compound watch query (which joins two tables with an
// address column) matches them.
const paymentCols = `p.txid AS txid, p.address AS address, p.amount_piconero, p.unlock_time, p.first_seen AS first_seen, p.first_seen_height`

func collectPayments(rows *sql.Rows) ([]Payment, error) {
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		var p Payment
		var seen int64
		if err := rows.Scan(&p.TxID, &p.Address, &p.AmountPiconero, &p.UnlockTime, &seen, &p.FirstSeenHeight); err != nil {
			return nil, err
		}
		p.FirstSeen = fromTS(seen)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PaymentsForOrder returns the payments to the order's subaddress, oldest first.
func (s *Store) PaymentsForOrder(ctx context.Context, orderID string) ([]Payment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+paymentCols+` FROM monero_payments p JOIN monero_subaddresses a ON a.address = p.address
WHERE a.order_id = ? ORDER BY p.first_seen, p.txid, p.address`, orderID)
	if err != nil {
		return nil, err
	}
	return collectPayments(rows)
}

// PaymentsByTxIDs returns the stored payments of the txids (every subaddress), read in batches of sqlChunk; unknown
// txids are left out and a txid given twice is read once. Oldest first (first-seen, txid, subaddress), across the
// batches.
func (s *Store) PaymentsByTxIDs(ctx context.Context, txids []string) ([]Payment, error) {
	seen := make(map[string]bool, len(txids))
	var ids []string
	for _, id := range txids {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	var out []Payment
	for len(ids) > 0 {
		n := min(len(ids), sqlChunk)
		args := make([]any, n)
		for i, id := range ids[:n] {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT `+paymentCols+` FROM monero_payments p WHERE p.txid IN (`+placeholders(n)+`)`, args...)
		if err != nil {
			return nil, err
		}
		l, err := collectPayments(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l...)
		ids = ids[n:]
	}
	slices.SortFunc(out, func(a, b Payment) int {
		return cmp.Or(a.FirstSeen.Compare(b.FirstSeen), cmp.Compare(a.TxID, b.TxID), cmp.Compare(a.Address, b.Address))
	})
	return out, nil
}

// Payment returns one payment, or ErrNotFound.
func (s *Store) Payment(ctx context.Context, k PaymentKey) (Payment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+paymentCols+` FROM monero_payments p WHERE p.txid = ? AND p.address = ?`, k.TxID, k.Address)
	if err != nil {
		return Payment{}, err
	}
	l, err := collectPayments(rows)
	if err != nil {
		return Payment{}, err
	}
	if len(l) == 0 {
		return Payment{}, ErrNotFound
	}
	return l[0], nil
}

// stuckPaymentSQL is the condition of a stuck payment p of order o: first seen before the order's expiry (on time).
// It is stuckTXOSQL's twin: such a payment is watched past its order's window until the watcher settles it.
const stuckPaymentSQL = `(p.first_seen < o.expires_at)`

// The per-pass Monero watch queries, the twins of the on-chain ones (watchedTXOsSQL and the rest): each starts from
// orders_expires, and the stuck arm is a UNION for the same reason.
const (
	watchedPaymentsSQL = `SELECT ` + paymentCols + ` FROM orders o
JOIN monero_subaddresses a ON a.order_id = o.id JOIN monero_payments p ON p.address = a.address
WHERE o.expires_at > ?
  AND NOT EXISTS (SELECT 1 FROM monero_settled d WHERE d.txid = p.txid AND d.address = p.address)
UNION
SELECT ` + paymentCols + ` FROM monero_payments p
JOIN monero_subaddresses a ON a.address = p.address JOIN orders o ON o.id = a.order_id
WHERE ` + stuckPaymentSQL + `
  AND NOT EXISTS (SELECT 1 FROM monero_settled d WHERE d.txid = p.txid AND d.address = p.address)
ORDER BY first_seen, txid, address`
	moneroPastExpirySQL = `SELECT ` + orderCols + ` FROM orders o
WHERE o.expires_at <= ? AND o.expires_at > ? AND o.amount_piconero > 0
  AND NOT EXISTS (SELECT 1 FROM events e WHERE e.order_id = o.id AND e.type IN ('order.paid', 'order.expired', 'order.underpaid'))
ORDER BY o.expires_at, o.id`
	openMoneroSQL = `SELECT 1 FROM orders o
WHERE o.expires_at > ? AND o.amount_piconero > 0
  AND NOT EXISTS (SELECT 1 FROM events e WHERE e.order_id = o.id AND e.type IN ('order.paid', 'order.expired', 'order.underpaid'))
UNION ALL
SELECT 1 FROM monero_payments p
JOIN monero_subaddresses a ON a.address = p.address JOIN orders o ON o.id = a.order_id
WHERE ` + stuckPaymentSQL + `
  AND NOT EXISTS (SELECT 1 FROM monero_settled d WHERE d.txid = p.txid AND d.address = p.address)
UNION ALL
SELECT 1 FROM monero_pending_settle
LIMIT 1`
	moneroAwaitingPaidAgainSQL = `SELECT ` + orderCols + ` FROM orders o
WHERE o.expires_at > ? AND o.amount_piconero > 0
  AND (SELECT count(*) FROM events e WHERE e.order_id = o.id AND e.type = 'order.reverted')
    > (SELECT count(*) FROM events e WHERE e.order_id = o.id AND e.type = 'order.paid_again')
ORDER BY o.expires_at, o.id`
	moneroLeftUnobservedSQL = `SELECT p.txid, p.address, o.id FROM orders o
JOIN monero_subaddresses a ON a.order_id = o.id JOIN monero_payments p ON p.address = a.address
WHERE o.expires_at <= ? AND o.expires_at > ? AND o.amount_piconero > 0 AND NOT ` + stuckPaymentSQL + `
  AND NOT EXISTS (SELECT 1 FROM monero_marks m WHERE m.txid = p.txid AND m.address = p.address)
ORDER BY o.expires_at, o.id, p.txid, p.address`
)

// WatchedPayments returns the payments that are not settled (AddMoneroSettled) and whose subaddress's order has
// ExpiresAt + WatchWindow after now, or that are stuck (first seen before the order's expiry), whatever the window.
// Oldest first.
func (s *Store) WatchedPayments(ctx context.Context, now time.Time) ([]Payment, error) {
	rows, err := s.db.QueryContext(ctx, watchedPaymentsSQL, windowStart(now))
	if err != nil {
		return nil, err
	}
	return collectPayments(rows)
}

// MoneroOrdersPastExpiry returns the Monero orders at or past their expiry, still inside the watch window, that have
// no order.paid, order.expired or order.underpaid event: the ones the watcher must still settle. Oldest expiry first.
func (s *Store) MoneroOrdersPastExpiry(ctx context.Context, now time.Time) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, moneroPastExpirySQL, ts(now), windowStart(now))
	if err != nil {
		return nil, err
	}
	return collectOrders(rows)
}

// HasOpenMoneroOrder reports whether the Monero watcher must still run: an order with a Monero amount is inside its
// watch window at now without an order.paid, order.expired or order.underpaid event, or a payment is stuck and not
// settled, whatever its order's events and window, or an order is pending settle (ruling S4).
func (s *Store) HasOpenMoneroOrder(ctx context.Context, now time.Time) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, openMoneroSQL, windowStart(now)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// MoneroOrdersLeftWindow returns the Monero orders whose watch window closed after since and at or before now
// (since < ExpiresAt + WatchWindow <= now) without an order.paid, order.expired or order.underpaid event. Oldest
// expiry first. It is MoneroOrdersPastExpiry's query on other bounds.
func (s *Store) MoneroOrdersLeftWindow(ctx context.Context, since, now time.Time) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, moneroPastExpirySQL, windowStart(now), windowStart(since))
	if err != nil {
		return nil, err
	}
	return collectOrders(rows)
}

// UnobservedPayment is a payment MoneroPaymentsLeftUnobserved lists, with its order.
type UnobservedPayment struct {
	PaymentKey
	OrderID string
}

// MoneroPaymentsLeftUnobserved returns the payments whose order's watch window closed after since and at or before
// now (since < ExpiresAt + WatchWindow <= now), first seen at or after the order's expiry (late: no longer polled once
// the window closes; an on-time payment is stuck and stays polled), with no mark: no call for them succeeded in a pass
// that committed. Whatever the order's events. Oldest expiry first.
func (s *Store) MoneroPaymentsLeftUnobserved(ctx context.Context, since, now time.Time) ([]UnobservedPayment, error) {
	rows, err := s.db.QueryContext(ctx, moneroLeftUnobservedSQL, windowStart(now), windowStart(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnobservedPayment
	for rows.Next() {
		var u UnobservedPayment
		if err := rows.Scan(&u.TxID, &u.Address, &u.OrderID); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// MoneroOrdersAwaitingPaidAgain returns the Monero orders inside the watch window with more order.reverted events
// than order.paid_again events. Oldest expiry first.
func (s *Store) MoneroOrdersAwaitingPaidAgain(ctx context.Context, now time.Time) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, moneroAwaitingPaidAgainSQL, windowStart(now))
	if err != nil {
		return nil, err
	}
	return collectOrders(rows)
}

func validPaymentKey(k PaymentKey) error {
	if !validLowerHex(k.TxID, 64) {
		return invalid("txid must be 64 lowercase hex characters")
	}
	if !ValidSubaddress(k.Address) {
		return invalid("payment subaddress is malformed")
	}
	return nil
}

// AddMoneroSettled records that a payment is settled for good, against the scan at height (at least 1): the watcher
// does not poll it again, in any process. added is false when the fact was already there (the first height is kept).
// ErrNotFound: unknown payment. ErrInvalid: a bad key or height.
func (s *Store) AddMoneroSettled(ctx context.Context, k PaymentKey, height int64) (bool, error) {
	if err := validPaymentKey(k); err != nil {
		return false, err
	}
	if height < 1 {
		return false, invalid("a settled height is at least 1")
	}
	var added bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM monero_payments WHERE txid = ? AND address = ?`, k.TxID, k.Address).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM monero_settled WHERE txid = ? AND address = ?`, k.TxID, k.Address).Scan(&one)
		if err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO monero_settled (txid, address, height, at) VALUES (?, ?, ?, ?)`,
			k.TxID, k.Address, height, ts(s.now())); err != nil {
			return err
		}
		added = true
		return nil
	})
	return added, err
}

// validMoneroMarkShape mirrors checkMoneroMarkShape: a mined mark has a height of at
// least 1 and 0 to MaxMoneroConfirmations confirmations, a mempool or dead mark height 0 and no confirmation.
func validMoneroMarkShape(kind string, height, confirmations int64) bool {
	switch kind {
	case MarkMined:
		return height >= 1 && confirmations >= 0 && confirmations <= MaxMoneroConfirmations
	case MarkMempool, MarkDead:
		return height == 0 && confirmations == 0
	}
	return false
}

// validMoneroMark is AddMoneroMark's check of m before any write.
func validMoneroMark(m MoneroMark) error {
	if err := validPaymentKey(PaymentKey{TxID: m.TxID, Address: m.Address}); err != nil {
		return err
	}
	if !validMoneroMarkShape(m.Kind, m.Height, m.Confirmations) {
		return invalid("a mined mark needs a height of at least 1 and 0 to 500000000 confirmations; mempool and dead marks have height 0 and no confirmation")
	}
	return nil
}

// addMoneroMark is AddMoneroMark's write, inside tx.
func (s *Store) addMoneroMark(ctx context.Context, tx *sql.Tx, m MoneroMark) error {
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM monero_payments WHERE txid = ? AND address = ?`, m.TxID, m.Address).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var seq int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) + 1 FROM monero_marks WHERE txid = ? AND address = ?`, m.TxID, m.Address).Scan(&seq); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO monero_marks (txid, address, seq, kind, height, at, confirmations) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.TxID, m.Address, seq, m.Kind, m.Height, ts(s.now()), m.Confirmations)
	return err
}

// AddMoneroMark appends a mark to an existing payment (ErrNotFound otherwise). Seq is assigned as the highest stored
// plus 1 and At is stamped from the clock; both are ignored in m. A mark of the wrong shape (validMoneroMarkShape) is
// ErrInvalid.
func (s *Store) AddMoneroMark(ctx context.Context, m MoneroMark) error {
	if err := validMoneroMark(m); err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error { return s.addMoneroMark(ctx, tx, m) })
}

const moneroMarkCols = `txid, address, seq, kind, height, at, confirmations`

func scanMoneroMark(sc interface{ Scan(...any) error }) (MoneroMark, error) {
	var m MoneroMark
	var at int64
	if err := sc.Scan(&m.TxID, &m.Address, &m.Seq, &m.Kind, &m.Height, &at, &m.Confirmations); err != nil {
		return MoneroMark{}, err
	}
	m.At = fromTS(at)
	return m, nil
}

// CurrentMoneroMark returns the payment's latest mark; ok is false when it has none.
func (s *Store) CurrentMoneroMark(ctx context.Context, k PaymentKey) (MoneroMark, bool, error) {
	m, err := scanMoneroMark(s.db.QueryRowContext(ctx, `SELECT `+moneroMarkCols+` FROM monero_marks WHERE txid = ? AND address = ?
ORDER BY seq DESC LIMIT 1`, k.TxID, k.Address))
	if errors.Is(err, sql.ErrNoRows) {
		return MoneroMark{}, false, nil
	}
	return m, err == nil, err
}

// paymentTxIDs returns the distinct txids of keys, in first-seen order, and the set of keys asked for.
func paymentTxIDs(keys []PaymentKey) ([]string, map[PaymentKey]bool) {
	want := make(map[PaymentKey]bool, len(keys))
	var txids []string
	seen := map[string]bool{}
	for _, k := range keys {
		want[k] = true
		if !seen[k.TxID] {
			seen[k.TxID] = true
			txids = append(txids, k.TxID)
		}
	}
	return txids, want
}

// MoneroSettledKeys returns the payments in keys that have a settled fact (AddMoneroSettled): WatchedPayments leaves
// exactly those out. It reads the facts of the keys' txids in batches of sqlChunk.
func (s *Store) MoneroSettledKeys(ctx context.Context, keys []PaymentKey) (map[PaymentKey]bool, error) {
	out := make(map[PaymentKey]bool, len(keys))
	txids, want := paymentTxIDs(keys)
	for len(txids) > 0 {
		n := min(len(txids), sqlChunk)
		args := make([]any, n)
		for i, id := range txids[:n] {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT txid, address FROM monero_settled WHERE txid IN (`+placeholders(n)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k PaymentKey
			if err := rows.Scan(&k.TxID, &k.Address); err != nil {
				rows.Close()
				return nil, err
			}
			if want[k] {
				out[k] = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		txids = txids[n:]
	}
	return out, nil
}

// CurrentMoneroMarks returns the latest mark of each payment in keys that has one. It reads the marks of the keys'
// txids in batches of sqlChunk, highest seq first, and keeps the first row of each key asked for.
func (s *Store) CurrentMoneroMarks(ctx context.Context, keys []PaymentKey) (map[PaymentKey]MoneroMark, error) {
	out := make(map[PaymentKey]MoneroMark, len(keys))
	txids, want := paymentTxIDs(keys)
	for len(txids) > 0 {
		n := min(len(txids), sqlChunk)
		args := make([]any, n)
		for i, id := range txids[:n] {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT `+moneroMarkCols+` FROM monero_marks WHERE txid IN (`+placeholders(n)+`)
ORDER BY txid, address, seq DESC`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			m, err := scanMoneroMark(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			k := PaymentKey{TxID: m.TxID, Address: m.Address}
			if _, done := out[k]; want[k] && !done {
				out[k] = m
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		txids = txids[n:]
	}
	return out, nil
}
