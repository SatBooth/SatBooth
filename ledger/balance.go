package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrAccountNotFound is returned by Balance for an account that does not exist.
var ErrAccountNotFound = errors.New("ledger: account not found")

// Balance returns the account's balance as of ledger time (the clock, or the last committed
// timestamp if later): the sums of its transfers, computed with checked 128-bit arithmetic. A
// pending transfer counts in the pending columns until it is posted or voided or expires
// (Timestamp + Timeout seconds); a post adds the posted amount to the posted columns, a void
// adds nothing.
//
// Balance runs in a write transaction (l.mu, which is not reentrant): never call it from inside
// the function passed to write, or it deadlocks.
func (l *Ledger) Balance(ctx context.Context, account U128) (Balance, error) {
	var b Balance
	// One transaction: the existence check, the ledger time and the sums are one snapshot.
	err := l.write(ctx, func(tx *sql.Tx) error {
		if _, found, err := selectAccount(ctx, tx, account); err != nil {
			return err
		} else if !found {
			return ErrAccountNotFound
		}
		// Ledger time: the clock, but never before the last committed row, so a clock that
		// stepped back (or is before 1970) cannot bring an expired pending transfer back. An
		// expiry seen here is recorded in expired_transfers, because an expiry is final once
		// observed, as in TigerBeetle. The clock is read once, and ledger time is
		// taken before anything is recorded: the expiries recorded and the balance are judged at
		// the same instant (the records get later timestamps, which do not move this instant).
		last, err := lastTimestamp(ctx, tx)
		if err != nil {
			return err
		}
		asof := max(l.now().UnixNano(), last)
		if err := recordDueExpiries(ctx, tx, asof, account); err != nil {
			return err
		}
		b, err = accountBalance(ctx, tx, account, asof)
		return err
	})
	return b, err
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// accountBalance sums the account's transfers as of asof (Unix nanoseconds). One statement, so
// the debit and credit sides come from one snapshot. A pending transfer with a resolution row is
// skipped (its post adds to the posted columns through its own row, a void adds nothing), and so
// is an unresolved one that has expired (recorded in expired_transfers, or due at asof).
func accountBalance(ctx context.Context, q queryer, account U128, asof int64) (Balance, error) {
	id := account.Bytes()
	rows, err := q.QueryContext(ctx, `
		SELECT 1, t.amount, t.flags, t.timestamp, t.timeout,
		  EXISTS (SELECT 1 FROM transfers r WHERE r.pending_id = t.id AND (r.flags & 12) != 0)
		  OR EXISTS (SELECT 1 FROM expired_transfers e WHERE e.pending_id = t.id)
		FROM transfers t WHERE t.debit_account_id = ?1
		UNION ALL
		SELECT 0, t.amount, t.flags, t.timestamp, t.timeout,
		  EXISTS (SELECT 1 FROM transfers r WHERE r.pending_id = t.id AND (r.flags & 12) != 0)
		  OR EXISTS (SELECT 1 FROM expired_transfers e WHERE e.pending_id = t.id)
		FROM transfers t WHERE t.credit_account_id = ?1`, id[:])
	if err != nil {
		return Balance{}, err
	}
	defer rows.Close()
	var b Balance
	for rows.Next() {
		var isDebit, resolved int
		var amtB []byte
		var flags, ts, timeout int64
		if err := rows.Scan(&isDebit, &amtB, &flags, &ts, &timeout, &resolved); err != nil {
			return Balance{}, err
		}
		amt, err := U128FromBytes(amtB)
		if err != nil {
			return Balance{}, err
		}
		f, err := checkedUint16(flags)
		if err != nil {
			return Balance{}, err
		}
		tsU, err := checkedUint64(ts)
		if err != nil {
			return Balance{}, err
		}
		to, err := checkedUint32(timeout)
		if err != nil {
			return Balance{}, err
		}
		var dst *U128
		switch fl := TransferFlags(f); {
		case fl&TransferVoidPendingTransfer != 0:
			continue // releases the pending amount, moves nothing
		case fl&TransferPostPendingTransfer != 0:
			dst = pick(&b.CreditsPosted, &b.DebitsPosted, isDebit == 1)
		case fl&TransferPending != 0:
			if resolved != 0 {
				continue
			}
			// Expired pending amounts no longer count. Both terms are below 2^63: no wrap.
			if to != 0 && tsU+uint64(to)*1_000_000_000 <= uint64(asof) {
				continue
			}
			dst = pick(&b.CreditsPending, &b.DebitsPending, isDebit == 1)
		default:
			dst = pick(&b.CreditsPosted, &b.DebitsPosted, isDebit == 1)
		}
		sum, ok := dst.Add(amt)
		if !ok {
			return Balance{}, fmt.Errorf("ledger: balance of account %v overflows 128 bits", account)
		}
		*dst = sum
	}
	return b, rows.Err()
}

func pick(credit, debit *U128, isDebit bool) *U128 {
	if isDebit {
		return debit
	}
	return credit
}

// recordDueExpiries records, in expired_transfers, every unresolved pending transfer of the given
// accounts that is expired at ledger time (the clock, or the last timestamp if later) and not yet
// recorded. Each record gets a fresh timestamp, so ledger time moves forward and the expiry is
// final: no later clock step can undo it. It must run inside the write transaction.
func recordDueExpiries(ctx context.Context, tx *sql.Tx, now int64, accounts ...U128) error {
	last, err := lastTimestamp(ctx, tx)
	if err != nil {
		return err
	}
	lt := max(now, last)
	var due [][]byte
	seen := map[string]bool{} // a pending transfer between two of the accounts is listed twice
	for _, a := range accounts {
		id := a.Bytes()
		// timestamp + timeout*1e9 fits in 63 bits (overflows_timeout is checked at creation), so
		// SQLite's integer arithmetic cannot turn it into a REAL.
		rows, err := tx.QueryContext(ctx, `SELECT t.id FROM transfers t
			WHERE (t.debit_account_id = ?1 OR t.credit_account_id = ?1)
			  AND (t.flags & 2) != 0 AND t.timeout != 0 AND t.timestamp + t.timeout * 1000000000 <= ?2
			  AND NOT EXISTS (SELECT 1 FROM transfers r WHERE r.pending_id = t.id AND (r.flags & 12) != 0)
			  AND NOT EXISTS (SELECT 1 FROM expired_transfers e WHERE e.pending_id = t.id)
			ORDER BY t.timestamp`, id[:], lt)
		if err != nil {
			return err
		}
		for rows.Next() {
			var pid []byte
			if err := rows.Scan(&pid); err != nil {
				rows.Close()
				return err
			}
			if !seen[string(pid)] {
				seen[string(pid)] = true
				due = append(due, pid)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	for _, pid := range due {
		if err := recordExpiry(ctx, tx, pid, lt); err != nil {
			return err
		}
	}
	return nil
}

// recordExpiry inserts the expiry record for pid (a plain INSERT; the callers have just read that
// there is none, and a trigger error is a bug) with a fresh timestamp.
func recordExpiry(ctx context.Context, tx *sql.Tx, pid []byte, now int64) error {
	ts, err := nextTimestamp(ctx, tx, now)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO expired_transfers (pending_id, timestamp) VALUES (?, ?)`, pid, ts); err != nil {
		return fmt.Errorf("ledger: record expiry: %w", err)
	}
	return nil
}
