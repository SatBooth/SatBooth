package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
)

// Address is the deposit address issued for an on-chain order. Index is its position in the
// merchant's descriptor; the store holds it so that a restored wallet backup cannot hand out an
// address again (AddAddress insists on a strictly rising index). CreatedAt is stamped from the clock.
type Address struct {
	OrderID   string
	Address   string
	Index     int64
	CreatedAt time.Time
}

// TXO is an output paid to an issued address. FirstSeen is SatBooth's clock when RecordScan first
// stored it, or the earlier time of the output it replaced that its caller gives (an RBF replacement inherits the
// original's), never bitcoind's own time. Confirmed is read by RecordScan only
// (the scan listed the output with confirmations above 0), never stored or returned (RecordScan's added rows read
// false).
type TXO struct {
	TxID      string
	Vout      uint32
	Address   string
	AmountSat int64
	FirstSeen time.Time
	Confirmed bool
}

// TXOKey identifies an output.
type TXOKey struct {
	TxID string
	Vout uint32
}

// Cursor is a point of the chain scan: the hash and height of the block the node reported as the
// tip when the scan was made, and when.
type Cursor struct {
	BlockHash string
	Height    int64
	At        time.Time
}

// Mark kinds.
const (
	MarkMempool = "mempool"
	MarkMined   = "mined"
	MarkDead    = "dead"
)

// Mark is one observation about an output: seen in the mempool, mined in a block (Height >= 1 and
// BlockHash set), or dead (replaced or conflicted). The latest Seq is the current state. Marks are
// facts about the chain at At, appended, never changed.
type Mark struct {
	TxID      string
	Vout      uint32
	Seq       int
	Kind      string
	Height    int64
	BlockHash string
	At        time.Time
}

// AddAddress stores the address issued for an order. ErrNotFound: unknown order. ErrAddressTaken:
// the order already has an address, or the address is stored. ErrInvalid: a malformed address, or
// (ErrIndexNotAbove) an Index that is not strictly above MaxAddressIndex. The check is made in the write
// transaction and again by a trigger.
func (s *Store) AddAddress(ctx context.Context, a Address) error {
	switch {
	case !validLowerHex(a.OrderID, 32):
		return invalid("order id must be 32 lowercase hex characters")
	case !ValidAddress(a.Address):
		return invalid("address must be 14 to 90 characters of [a-z0-9] starting bc1, tb1 or bcrt1")
	case a.Index < 0 || a.Index > MaxAddressIdx:
		return invalid("address index must be 0 to 2147483647")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM orders WHERE id = ?`, a.OrderID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var sat int64
		if err := tx.QueryRowContext(ctx, `SELECT amount_sat FROM orders WHERE id = ?`, a.OrderID).Scan(&sat); err != nil {
			return err
		}
		if sat == 0 {
			return invalid("the order has no on-chain amount")
		}
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM addresses WHERE order_id = ? OR address = ?`, a.OrderID, a.Address).Scan(&one)
		if err == nil {
			return ErrAddressTaken
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var max sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT max(idx) FROM addresses`).Scan(&max); err != nil {
			return err
		}
		if max.Valid && a.Index <= max.Int64 {
			return ErrIndexNotAbove
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO addresses (address, order_id, idx, created_at) VALUES (?, ?, ?, ?)`,
			a.Address, a.OrderID, a.Index, ts(s.now()))
		return err
	})
}

func scanAddress(sc interface{ Scan(...any) error }) (Address, error) {
	var a Address
	var created int64
	if err := sc.Scan(&a.Address, &a.OrderID, &a.Index, &created); err != nil {
		return Address{}, err
	}
	a.CreatedAt = fromTS(created)
	return a, nil
}

// AddressForOrder returns the order's address, or ErrNotFound.
func (s *Store) AddressForOrder(ctx context.Context, orderID string) (Address, error) {
	if !validLowerHex(orderID, 32) {
		return Address{}, ErrNotFound
	}
	a, err := scanAddress(s.db.QueryRowContext(ctx, `SELECT address, order_id, idx, created_at FROM addresses WHERE order_id = ?`, orderID))
	if errors.Is(err, sql.ErrNoRows) {
		return Address{}, ErrNotFound
	}
	return a, err
}

// sqlChunk bounds the number of host parameters of one statement.
const sqlChunk = 500

// AddressesByAddress returns the stored addresses among addrs, keyed by address.
func (s *Store) AddressesByAddress(ctx context.Context, addrs []string) (map[string]Address, error) {
	out := make(map[string]Address)
	for len(addrs) > 0 {
		n := min(len(addrs), sqlChunk)
		args := make([]any, n)
		for i, a := range addrs[:n] {
			args[i] = a
		}
		rows, err := s.db.QueryContext(ctx, `SELECT address, order_id, idx, created_at FROM addresses WHERE address IN (`+
			strings.TrimSuffix(strings.Repeat("?,", n), ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			a, err := scanAddress(rows)
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

// MaxAddressIndex returns the highest index issued; ok is false when no address is stored.
func (s *Store) MaxAddressIndex(ctx context.Context) (idx int64, ok bool, err error) {
	var m sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT max(idx) FROM addresses`).Scan(&m); err != nil {
		return 0, false, err
	}
	return m.Int64, m.Valid, nil
}

// UnusedRun is the number of issued indexes above the highest one whose address has received an
// output: the gap a wallet scanning past it must cover. With no addresses it is 0. With indexes
// 0..4 and none paid it is 5; with a payment at index 2 it is 2.
func (s *Store) UnusedRun(ctx context.Context) (int64, error) {
	var max, used sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT max(idx) FROM addresses`).Scan(&max); err != nil {
		return 0, err
	}
	if !max.Valid {
		return 0, nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT max(a.idx) FROM addresses a
WHERE EXISTS (SELECT 1 FROM onchain_txos t WHERE t.address = a.address)`).Scan(&used); err != nil {
		return 0, err
	}
	if !used.Valid {
		return max.Int64 + 1, nil
	}
	return max.Int64 - used.Int64, nil
}

func validTXO(x TXO) error {
	switch {
	case !validLowerHex(x.TxID, 64):
		return invalid("txid must be 64 lowercase hex characters")
	case !ValidAddress(x.Address):
		return invalid("output address is malformed")
	case !validSat(x.AmountSat):
		return invalid("output amount must be 1 to 2100000000000000 sat")
	case !x.FirstSeen.IsZero() && ts(x.FirstSeen) < 1:
		return invalid("output first-seen must be after 1970-01-01T00:00:00Z")
	}
	return nil
}

func validCursor(c Cursor) error {
	if !validLowerHex(c.BlockHash, 64) {
		return invalid("cursor block hash must be 64 lowercase hex characters")
	}
	if c.Height < 0 {
		return invalid("cursor height must not be negative")
	}
	return nil
}

// RecordScan stores the outputs of one chain scan and the scan's cursor, in one transaction. It
// inserts every (txid, vout) not already stored and returns the rows it added. FirstSeen is the caller's when it is
// not zero (an inherited time), in whole seconds; one after the clock's now is ErrInvalid and nothing is written.
// Zero is the clock's now. A stored row keeps its own: the caller's value is not read for it. A stored output listed
// Confirmed that is settled with a dead current mark is reopened (reopen), in the same transaction. An address that
// is not stored is ErrNotFound and nothing is written: the caller filters first. The cursor row (next seq) is added
// only if an output was added or the block hash or the height differs from the latest stored one, so a quiet node
// adds no rows. A cursor without a valid block hash is ErrInvalid.
func (s *Store) RecordScan(ctx context.Context, txos []TXO, cursor Cursor) ([]TXO, error) {
	if err := validCursor(cursor); err != nil {
		return nil, err
	}
	for _, x := range txos {
		if err := validTXO(x); err != nil {
			return nil, err
		}
	}
	var added []TXO
	err := s.write(ctx, func(tx *sql.Tx) error {
		added = nil
		now := s.now()
		for _, x := range txos {
			var one int
			var oldAddr string
			var oldSat int64
			err := tx.QueryRowContext(ctx, `SELECT address, amount_sat FROM onchain_txos WHERE txid = ? AND vout = ?`, x.TxID, int64(x.Vout)).Scan(&oldAddr, &oldSat)
			if err == nil {
				if oldAddr != x.Address || oldSat != x.AmountSat {
					// An output cannot change address or amount: bitcoind and the store disagree.
					return invalid("a stored output arrived with a different address or amount")
				}
				if x.Confirmed {
					if err := reopen(ctx, tx, TXOKey{TxID: x.TxID, Vout: x.Vout}); err != nil {
						return err
					}
				}
				continue
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM addresses WHERE address = ?`, x.Address).Scan(&one); errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			} else if err != nil {
				return err
			}
			seen := ts(now)
			if !x.FirstSeen.IsZero() {
				if ts(x.FirstSeen) > seen {
					return invalid("output first-seen must not be after now")
				}
				seen = ts(x.FirstSeen)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO onchain_txos (txid, vout, address, amount_sat, first_seen) VALUES (?, ?, ?, ?, ?)`,
				x.TxID, int64(x.Vout), x.Address, x.AmountSat, seen); err != nil {
				return err
			}
			x.FirstSeen, x.Confirmed = fromTS(seen), false // Confirmed is read only, never returned
			added = append(added, x)
		}
		var seq int64
		var latest sql.NullString
		var height sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT seq, block_hash, height FROM onchain_cursor ORDER BY seq DESC LIMIT 1`).Scan(&seq, &latest, &height); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if len(added) == 0 && latest.Valid && latest.String == cursor.BlockHash && height.Int64 == cursor.Height {
			return nil
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO onchain_cursor (seq, block_hash, height, at) VALUES (?, ?, ?, ?)`,
			seq+1, cursor.BlockHash, cursor.Height, ts(now))
		return err
	})
	if err != nil {
		return nil, err
	}
	return added, nil
}

// reopen deletes the settled fact of a stored output listed mined whose current mark is dead (a dead output settled
// past its window, then rebroadcast and mined): it is watched again, and the watcher records it by the usual rules.
// Any other output is left alone.
func reopen(ctx context.Context, tx *sql.Tx, k TXOKey) error {
	return reopenIfDead(ctx, tx, "onchain_settled", "onchain_marks", "txid = ? AND vout = ?", k.TxID, int64(k.Vout))
}

// reopenIfDead is the one reopen rule of both chains (RecordScan, RecordMoneroScan): the settled fact in the table
// settled of the key that where (a constant over the key columns, never input) and args select is deleted when the
// key's current mark in the table marks is dead. A key without a settled fact or whose current mark is anything else
// is left alone. The caller has checked that the scan lists the key mined.
func reopenIfDead(ctx context.Context, tx *sql.Tx, settled, marks, where string, args ...any) error {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM `+settled+` WHERE `+where, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	var kind string
	err = tx.QueryRowContext(ctx, `SELECT kind FROM `+marks+` WHERE `+where+` ORDER BY seq DESC LIMIT 1`, args...).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && kind != MarkDead) {
		return nil
	} else if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM `+settled+` WHERE `+where, args...)
	return err
}

// Cursor returns the latest cursor row; ok is false before the first scan.
func (s *Store) Cursor(ctx context.Context) (Cursor, bool, error) {
	var c Cursor
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT block_hash, height, at FROM onchain_cursor ORDER BY seq DESC LIMIT 1`).Scan(&c.BlockHash, &c.Height, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Cursor{}, false, nil
	}
	if err != nil {
		return Cursor{}, false, err
	}
	c.At = fromTS(at)
	return c, true, nil
}

func collectTXOs(rows *sql.Rows) ([]TXO, error) {
	defer rows.Close()
	var out []TXO
	for rows.Next() {
		var x TXO
		var vout, seen int64
		if err := rows.Scan(&x.TxID, &vout, &x.Address, &x.AmountSat, &seen); err != nil {
			return nil, err
		}
		x.Vout, x.FirstSeen = uint32(vout), fromTS(seen)
		out = append(out, x)
	}
	return out, rows.Err()
}

const txoCols = `t.txid, t.vout, t.address, t.amount_sat, t.first_seen`

// TXOsForOrder returns the outputs paid to the order's address, oldest first.
func (s *Store) TXOsForOrder(ctx context.Context, orderID string) ([]TXO, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+txoCols+` FROM onchain_txos t JOIN addresses a ON a.address = t.address
WHERE a.order_id = ? ORDER BY t.first_seen, t.txid, t.vout`, orderID)
	if err != nil {
		return nil, err
	}
	return collectTXOs(rows)
}

// TXOsByTxIDs returns the stored outputs of the txids (every vout), read in batches of sqlChunk as
// AddressesByAddress; unknown txids are left out and a txid given twice is read once. Oldest first (first-seen, txid,
// vout), across the batches.
func (s *Store) TXOsByTxIDs(ctx context.Context, txids []string) ([]TXO, error) {
	seen := make(map[string]bool, len(txids))
	var ids []string
	for _, id := range txids {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	var out []TXO
	for len(ids) > 0 {
		n := min(len(ids), sqlChunk)
		args := make([]any, n)
		for i, id := range ids[:n] {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT `+txoCols+` FROM onchain_txos t WHERE t.txid IN (`+
			strings.TrimSuffix(strings.Repeat("?,", n), ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		l, err := collectTXOs(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l...)
		ids = ids[n:]
	}
	slices.SortFunc(out, func(a, b TXO) int {
		return cmp.Or(a.FirstSeen.Compare(b.FirstSeen), cmp.Compare(a.TxID, b.TxID), cmp.Compare(a.Vout, b.Vout))
	})
	return out, nil
}

// TXO returns one output, or ErrNotFound.
func (s *Store) TXO(ctx context.Context, k TXOKey) (TXO, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+txoCols+` FROM onchain_txos t WHERE t.txid = ? AND t.vout = ?`, k.TxID, int64(k.Vout))
	if err != nil {
		return TXO{}, err
	}
	l, err := collectTXOs(rows)
	if err != nil {
		return TXO{}, err
	}
	if len(l) == 0 {
		return TXO{}, ErrNotFound
	}
	return l[0], nil
}

// windowStart is the earliest expiry still inside the watch window at now: expires_at + WatchWindow >
// now is written expires_at > windowStart(now), so that SQLite can use the index on expires_at.
func windowStart(now time.Time) int64 { return ts(now) - int64(WatchWindow/time.Second) }

// stuckTXOSQL is the condition of a stuck output t of order o: first seen before the order's expiry (on time). The
// queries that use it add that it is not settled. Such an output is watched past its order's window until the watcher
// settles it: mined SettledDepth deep with its record, or dead with no standing record once its order settled (so a
// crash between a dead mark and its reversal is healed by the next pass). An on-time output whose ledger record is
// foreign never settles, and stays polled until its record is put right by hand.
const stuckTXOSQL = `(t.first_seen < o.expires_at)`

// The per-pass watch queries. Each starts from orders_expires (a test checks the plan), so a pass
// reads the orders in the window, not every order ever made. WatchedTXOs and HasOpenOnchainOrder have a second arm,
// the stuck outputs past their window, which scans onchain_txos once a pass: it is a UNION, because an OR across
// o.expires_at and t.first_seen stops SQLite from using orders_expires for the first arm.
const (
	watchedTXOsSQL = `SELECT ` + txoCols + ` FROM orders o
JOIN addresses a ON a.order_id = o.id JOIN onchain_txos t ON t.address = a.address
WHERE o.expires_at > ?
  AND NOT EXISTS (SELECT 1 FROM onchain_settled d WHERE d.txid = t.txid AND d.vout = t.vout)
UNION
SELECT ` + txoCols + ` FROM onchain_txos t
JOIN addresses a ON a.address = t.address JOIN orders o ON o.id = a.order_id
WHERE ` + stuckTXOSQL + `
  AND NOT EXISTS (SELECT 1 FROM onchain_settled d WHERE d.txid = t.txid AND d.vout = t.vout)
ORDER BY first_seen, txid, vout`
	onchainPastExpirySQL = `SELECT ` + orderCols + ` FROM orders o
WHERE o.expires_at <= ? AND o.expires_at > ? AND o.amount_sat > 0
  AND NOT EXISTS (SELECT 1 FROM events e WHERE e.order_id = o.id AND e.type IN ('order.paid', 'order.expired', 'order.underpaid'))
ORDER BY o.expires_at, o.id`
	openOnchainSQL = `SELECT 1 FROM orders o
WHERE o.expires_at > ? AND o.amount_sat > 0
  AND NOT EXISTS (SELECT 1 FROM events e WHERE e.order_id = o.id AND e.type IN ('order.paid', 'order.expired', 'order.underpaid'))
UNION ALL
SELECT 1 FROM onchain_txos t
JOIN addresses a ON a.address = t.address JOIN orders o ON o.id = a.order_id
WHERE ` + stuckTXOSQL + `
  AND NOT EXISTS (SELECT 1 FROM onchain_settled d WHERE d.txid = t.txid AND d.vout = t.vout)
LIMIT 1`
	awaitingPaidAgainSQL = `SELECT ` + orderCols + ` FROM orders o
WHERE o.expires_at > ? AND o.amount_sat > 0
  AND (SELECT count(*) FROM events e WHERE e.order_id = o.id AND e.type = 'order.reverted')
    > (SELECT count(*) FROM events e WHERE e.order_id = o.id AND e.type = 'order.paid_again')
ORDER BY o.expires_at, o.id`
)

// WatchedTXOs returns the outputs that are not settled (AddSettled) and whose address's order has ExpiresAt +
// WatchWindow after now, or that are stuck (stuckTXOSQL: first seen before the order's expiry), whatever the window:
// an on-time payment still unconfirmed at the window close stays watched until it is mined and settled, or dead and
// settled. Oldest first.
func (s *Store) WatchedTXOs(ctx context.Context, now time.Time) ([]TXO, error) {
	rows, err := s.db.QueryContext(ctx, watchedTXOsSQL, windowStart(now))
	if err != nil {
		return nil, err
	}
	return collectTXOs(rows)
}

// OnchainOrdersPastExpiry returns the on-chain orders at or past their expiry, still inside the watch
// window, that have no order.paid, order.expired or order.underpaid event: the ones the watcher must
// still settle. Oldest expiry first.
func (s *Store) OnchainOrdersPastExpiry(ctx context.Context, now time.Time) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, onchainPastExpirySQL, ts(now), windowStart(now))
	if err != nil {
		return nil, err
	}
	return collectOrders(rows)
}

// HasOpenOnchainOrder reports whether the on-chain watcher must still run for an order: one with an on-chain amount
// is inside its watch window at now (ExpiresAt + WatchWindow after now, before its expiry included) without an
// order.paid, order.expired or order.underpaid event, or an output is stuck (stuckTXOSQL) and not settled, whatever
// its order's events and its window: a stuck payment mined later must still be recorded.
func (s *Store) HasOpenOnchainOrder(ctx context.Context, now time.Time) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, openOnchainSQL, windowStart(now)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// OnchainOrdersLeftWindow returns the on-chain orders whose watch window closed after since and at or before now
// (since < ExpiresAt + WatchWindow <= now) without an order.paid, order.expired or order.underpaid event: no pass
// settles them any more. Oldest expiry first. It is OnchainOrdersPastExpiry's query on other bounds.
func (s *Store) OnchainOrdersLeftWindow(ctx context.Context, since, now time.Time) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, onchainPastExpirySQL, windowStart(now), windowStart(since))
	if err != nil {
		return nil, err
	}
	return collectOrders(rows)
}

// OrdersAwaitingPaidAgain returns the on-chain orders inside the watch window with more
// order.reverted events than order.paid_again events (a Lightning payment after a reversal must
// still produce paid_again). Oldest expiry first.
func (s *Store) OrdersAwaitingPaidAgain(ctx context.Context, now time.Time) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, awaitingPaidAgainSQL, windowStart(now))
	if err != nil {
		return nil, err
	}
	return collectOrders(rows)
}

// AddSettled records that an output is settled for good, against the scan at height (at least 1):
// the watcher does not poll it again, in any process. added is false when the fact was already there
// (the first height is kept). ErrNotFound: unknown output. ErrInvalid: a bad txid or height.
func (s *Store) AddSettled(ctx context.Context, k TXOKey, height int64) (bool, error) {
	if !validLowerHex(k.TxID, 64) {
		return false, invalid("txid must be 64 lowercase hex characters")
	}
	if height < 1 {
		return false, invalid("a settled height is at least 1")
	}
	var added bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM onchain_txos WHERE txid = ? AND vout = ?`, k.TxID, int64(k.Vout)).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM onchain_settled WHERE txid = ? AND vout = ?`, k.TxID, int64(k.Vout)).Scan(&one)
		if err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO onchain_settled (txid, vout, height, at) VALUES (?, ?, ?, ?)`,
			k.TxID, int64(k.Vout), height, ts(s.now())); err != nil {
			return err
		}
		added = true
		return nil
	})
	return added, err
}

// AddMark appends a mark to an existing output (ErrNotFound otherwise). Seq is assigned as the
// highest stored plus 1 and At is stamped from the clock; both are ignored in m.
func (s *Store) AddMark(ctx context.Context, m Mark) error {
	switch {
	case !validLowerHex(m.TxID, 64):
		return invalid("txid must be 64 lowercase hex characters")
	case !validMarkShape(m.Kind, m.Height, m.BlockHash):
		return invalid("a mined mark needs a height of at least 1 and a block hash; mempool and dead marks have neither")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM onchain_txos WHERE txid = ? AND vout = ?`, m.TxID, int64(m.Vout)).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var seq int
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) + 1 FROM onchain_marks WHERE txid = ? AND vout = ?`, m.TxID, int64(m.Vout)).Scan(&seq); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO onchain_marks (txid, vout, seq, kind, height, block_hash, at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			m.TxID, int64(m.Vout), seq, m.Kind, m.Height, m.BlockHash, ts(s.now()))
		return err
	})
}

const markCols = `txid, vout, seq, kind, height, block_hash, at`

func scanMark(sc interface{ Scan(...any) error }) (Mark, error) {
	var m Mark
	var vout, at int64
	if err := sc.Scan(&m.TxID, &vout, &m.Seq, &m.Kind, &m.Height, &m.BlockHash, &at); err != nil {
		return Mark{}, err
	}
	m.Vout, m.At = uint32(vout), fromTS(at)
	return m, nil
}

// CurrentMark returns the output's latest mark; ok is false when it has none.
func (s *Store) CurrentMark(ctx context.Context, k TXOKey) (Mark, bool, error) {
	return currentMark(ctx, s.db, k)
}

func currentMark(ctx context.Context, q queryer, k TXOKey) (Mark, bool, error) {
	m, err := scanMark(q.QueryRowContext(ctx, `SELECT `+markCols+` FROM onchain_marks WHERE txid = ? AND vout = ? ORDER BY seq DESC LIMIT 1`, k.TxID, int64(k.Vout)))
	if errors.Is(err, sql.ErrNoRows) {
		return Mark{}, false, nil
	}
	return m, err == nil, err
}

// SettledKeys returns the outputs in keys that have a settled fact (AddSettled): WatchedTXOs leaves exactly those out.
// It reads the facts of the keys' txids in batches of sqlChunk, as CurrentMarks does.
func (s *Store) SettledKeys(ctx context.Context, keys []TXOKey) (map[TXOKey]bool, error) {
	out := make(map[TXOKey]bool, len(keys))
	want := make(map[TXOKey]bool, len(keys))
	var txids []string
	seen := map[string]bool{}
	for _, k := range keys {
		want[k] = true
		if !seen[k.TxID] {
			seen[k.TxID] = true
			txids = append(txids, k.TxID)
		}
	}
	for len(txids) > 0 {
		n := min(len(txids), sqlChunk)
		args := make([]any, n)
		for i, id := range txids[:n] {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT txid, vout FROM onchain_settled WHERE txid IN (`+
			strings.TrimSuffix(strings.Repeat("?,", n), ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k TXOKey
			var vout int64
			if err := rows.Scan(&k.TxID, &vout); err != nil {
				rows.Close()
				return nil, err
			}
			k.Vout = uint32(vout)
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

// CurrentMarks returns the latest mark of each output in keys that has one. It reads the marks of the keys' txids in
// batches of sqlChunk (an output has few marks), highest seq first, and keeps the first row of each key asked for.
func (s *Store) CurrentMarks(ctx context.Context, keys []TXOKey) (map[TXOKey]Mark, error) {
	out := make(map[TXOKey]Mark, len(keys))
	want := make(map[TXOKey]bool, len(keys))
	var txids []string
	seen := map[string]bool{}
	for _, k := range keys {
		want[k] = true
		if !seen[k.TxID] {
			seen[k.TxID] = true
			txids = append(txids, k.TxID)
		}
	}
	for len(txids) > 0 {
		n := min(len(txids), sqlChunk)
		args := make([]any, n)
		for i, id := range txids[:n] {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT `+markCols+` FROM onchain_marks WHERE txid IN (`+
			strings.TrimSuffix(strings.Repeat("?,", n), ",")+`) ORDER BY txid, vout, seq DESC`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			m, err := scanMark(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			k := TXOKey{TxID: m.TxID, Vout: m.Vout}
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
