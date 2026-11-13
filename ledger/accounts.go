package ledger

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// CreateAccount creates an account, or reports why not, with TigerBeetle's result codes checked
// in the order of create_accounts.md at the pinned commit. The error is for storage failures only.
//
// Codes of that document that are not reachable here, because our Account has no such field or
// flag: linked_event_failed, linked_event_chain_open (linked), imported_* (imported),
// reserved_field, the *_must_be_zero balance codes (balances are not stored), and closed.
func (l *Ledger) CreateAccount(ctx context.Context, a Account) (CreateAccountResult, error) {
	if r := validateAccountPre(a); r != "" {
		return r, nil
	}
	var result CreateAccountResult
	err := l.write(ctx, func(tx *sql.Tx) error {
		existing, found, err := selectAccount(ctx, tx, a.ID)
		if err != nil {
			return err
		}
		if found {
			result = compareExisting(existing, a)
			return nil
		}
		if r := validateAccountPost(a); r != "" {
			result = r
			return nil
		}
		ts, err := nextTimestamp(ctx, tx, l.now().UnixNano())
		if err != nil {
			return err
		}
		ud64 := make([]byte, 8)
		binary.BigEndian.PutUint64(ud64, a.UserData64)
		ud128, id := a.UserData128.Bytes(), a.ID.Bytes()
		// A plain INSERT: the existence check above is the only duplicate handling; a trigger
		// or constraint error here is a bug and is returned as an error.
		_, err = tx.ExecContext(ctx, `INSERT INTO accounts
			(id, user_data_128, user_data_64, user_data_32, ledger, code, flags, timestamp)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id[:], ud128[:], ud64, int64(a.UserData32), int64(a.Ledger), int64(a.Code), int64(a.Flags), ts)
		if err != nil {
			return fmt.Errorf("ledger: insert account: %w", err)
		}
		result = AccountCreated
		return nil
	})
	if err != nil {
		return "", err
	}
	return result, nil
}

// validateAccountPre holds the checks that come before the existing-account lookup.
func validateAccountPre(a Account) CreateAccountResult {
	switch {
	case a.Timestamp != 0:
		return AccountTimestampMustBeZero
	case a.Flags&^accountFlagsSupported != 0:
		return AccountReservedFlag
	case a.ID.IsZero():
		return AccountIDMustNotBeZero
	case a.ID.IsMax():
		return AccountIDMustNotBeIntMax
	}
	return ""
}

// compareExisting returns exists, or the first differing field, in the document's order.
func compareExisting(e, a Account) CreateAccountResult {
	switch {
	case e.Flags != a.Flags:
		return AccountExistsWithDifferentFlags
	case e.UserData128 != a.UserData128:
		return AccountExistsWithDifferentUserData128
	case e.UserData64 != a.UserData64:
		return AccountExistsWithDifferentUserData64
	case e.UserData32 != a.UserData32:
		return AccountExistsWithDifferentUserData32
	case e.Ledger != a.Ledger:
		return AccountExistsWithDifferentLedger
	case e.Code != a.Code:
		return AccountExistsWithDifferentCode
	}
	return AccountExists
}

// validateAccountPost holds the checks that come after the existing-account lookup.
func validateAccountPost(a Account) CreateAccountResult {
	switch {
	case a.Flags&AccountDebitsMustNotExceedCredits != 0 && a.Flags&AccountCreditsMustNotExceedDebits != 0:
		return AccountFlagsAreMutuallyExclusive
	case a.Ledger == 0:
		return AccountLedgerMustNotBeZero
	case a.Code == 0:
		return AccountCodeMustNotBeZero
	}
	return ""
}

// LookupAccount returns the account with the given id.
func (l *Ledger) LookupAccount(ctx context.Context, id U128) (Account, bool, error) {
	return selectAccount(ctx, l.db, id)
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func selectAccount(ctx context.Context, q queryRower, id U128) (Account, bool, error) {
	b := id.Bytes()
	var ud128, ud64 []byte
	var ud32, led, code, flags, ts int64
	err := q.QueryRowContext(ctx, `SELECT user_data_128, user_data_64, user_data_32, ledger, code, flags, timestamp
		FROM accounts WHERE id = ?`, b[:]).Scan(&ud128, &ud64, &ud32, &led, &code, &flags, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	if len(ud128) != 16 || len(ud64) != 8 {
		return Account{}, false, errors.New("ledger: corrupt account row")
	}
	u128, err := U128FromBytes(ud128)
	if err != nil {
		return Account{}, false, err
	}
	a := Account{ID: id, UserData128: u128, UserData64: binary.BigEndian.Uint64(ud64)}
	var e1, e2, e3, e4, e5 error
	a.UserData32, e1 = checkedUint32(ud32)
	a.Ledger, e2 = checkedUint32(led)
	a.Code, e3 = checkedUint16(code)
	var f uint16
	f, e4 = checkedUint16(flags)
	a.Flags = AccountFlags(f)
	a.Timestamp, e5 = checkedUint64(ts)
	if err := errors.Join(e1, e2, e3, e4, e5); err != nil {
		return Account{}, false, fmt.Errorf("ledger: corrupt account row: %w", err)
	}
	return a, true, nil
}

// The checked conversions guard decoding of INTEGER columns: the schema's CHECK constraints
// keep them in range, and a value outside is corruption, not something to truncate.
func checkedUint32(v int64) (uint32, error) {
	if v < 0 || v > math.MaxUint32 {
		return 0, fmt.Errorf("value %d out of range for uint32", v)
	}
	return uint32(v), nil
}

func checkedUint16(v int64) (uint16, error) {
	if v < 0 || v > math.MaxUint16 {
		return 0, fmt.Errorf("value %d out of range for uint16", v)
	}
	return uint16(v), nil
}

func checkedUint64(v int64) (uint64, error) {
	if v < 0 {
		return 0, fmt.Errorf("value %d out of range for uint64", v)
	}
	return uint64(v), nil
}

// lastTimestamp is the largest timestamp in accounts, transfers and expired_transfers: ledger time
// never goes back before it.
func lastTimestamp(ctx context.Context, q queryRower) (int64, error) {
	var last int64
	err := q.QueryRowContext(ctx, `SELECT max(
		(SELECT coalesce(max(timestamp), 0) FROM accounts),
		(SELECT coalesce(max(timestamp), 0) FROM transfers),
		(SELECT coalesce(max(timestamp), 0) FROM expired_transfers))`).Scan(&last)
	return last, err
}

// nextTimestamp returns max(now, last+1), where last is the largest timestamp in accounts,
// transfers and expired_transfers. It must run inside the write transaction. Timestamps are
// strictly increasing across all three tables.
func nextTimestamp(ctx context.Context, tx *sql.Tx, now int64) (int64, error) {
	last, err := lastTimestamp(ctx, tx)
	if err != nil {
		return 0, err
	}
	if now < 1 {
		return 0, errors.New("ledger: clock is before 1970")
	}
	if last == math.MaxInt64 {
		return 0, errors.New("ledger: timestamps exhausted")
	}
	if now <= last {
		now = last + 1
	}
	return now, nil
}
