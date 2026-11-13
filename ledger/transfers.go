package ledger

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// CreateTransfer creates a single-phase or pending transfer, or posts or voids a pending one, or
// reports why not, with TigerBeetle's result codes checked in the order of fn create_transfer and
// fn post_or_void_pending_transfer in src/state_machine.zig at the pinned commit. The error is for
// storage failures only.
//
// Transient failures (an account or pending transfer that was not found, exceeds_credits,
// exceeds_debits) are recorded in
// failed_transfers in the same transaction: retrying the id returns id_already_failed even if
// the cause has since gone, as in TigerBeetle.
//
// Codes of create_transfers.md that are not reachable for our subset: linked_event_* (CreateTransfers answers
// linked_event_failed for a batch's other items), imported_*,
// closing_transfer_must_be_pending and the *_already_closed codes (those flags are reserved_flag),
// amount_must_not_be_zero (removed in TigerBeetle 0.16).
func (l *Ledger) CreateTransfer(ctx context.Context, t Transfer) (CreateTransferResult, error) {
	if r := validateTransferPre(t); r != "" {
		return r, nil
	}
	var result CreateTransferResult
	err := l.write(ctx, func(tx *sql.Tx) error {
		r, err := l.createTransfer(ctx, tx, t)
		if err != nil {
			return err
		}
		result = r
		if isTransient(r) {
			id := t.ID.Bytes()
			if _, err := tx.ExecContext(ctx, `INSERT INTO failed_transfers (id, result) VALUES (?, ?)`, id[:], string(r)); err != nil {
				return fmt.Errorf("ledger: record failed transfer: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return result, nil
}

// CreateTransfers creates the transfers in ts in one SQLite transaction, in order, each as CreateTransfer would at
// that point (so an item sees the items before it). An item that already exists identically is TransferExists and
// changes nothing. If every item is TransferCreated or TransferExists the batch is committed; otherwise nothing in it
// is applied: the transaction is rolled back to a savepoint, the first refused item's result is its code, and every
// other item's is TransferLinkedEventFailed (TigerBeetle's linked chain, whose events all succeed or fail together;
// its flag is not ours to set, the whole batch is the chain). The expiries the batch observed are kept (keepExpiries),
// as CreateTransfer keeps them: an expiry is the ledger's own fact, not an item. Unlike CreateTransfer, a transient
// refusal in a batch is not recorded in failed_transfers (the rollback takes it too), so retrying its id may succeed
// later. The error is for storage
// failures only, and then no result is returned. An empty batch writes nothing.
func (l *Ledger) CreateTransfers(ctx context.Context, ts []Transfer) ([]CreateTransferResult, error) {
	results := make([]CreateTransferResult, len(ts))
	if len(ts) == 0 {
		return results, nil
	}
	refused := -1
	err := l.write(ctx, func(tx *sql.Tx) error {
		start, err := lastTimestamp(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SAVEPOINT batch`); err != nil {
			return err
		}
		for i, t := range ts {
			r := validateTransferPre(t)
			if r == "" {
				if r, err = l.createTransfer(ctx, tx, t); err != nil {
					return err
				}
			}
			results[i] = r
			if r != TransferCreated && r != TransferExists {
				refused = i
				return keepExpiries(ctx, tx, start)
			}
		}
		_, err = tx.ExecContext(ctx, `RELEASE batch`)
		return err
	})
	if err != nil {
		return nil, err
	}
	if refused >= 0 {
		for i := range results {
			if i != refused {
				results[i] = TransferLinkedEventFailed
			}
		}
	}
	return results, nil
}

// keepExpiries undoes a refused batch back to its savepoint and keeps the expiries the batch observed on the way (a
// post or void answered pending_transfer_expired, or recordDueExpiries before a balance check): an observed expiry is
// final, as CreateTransfer commits it whatever it answers. They are the expired_transfers rows stamped after start
// (the last timestamp before the batch), put back with their own timestamps (still strictly increasing: nothing else
// is left after start), and only for pending transfers that are still stored (one the batch created is gone with it).
func keepExpiries(ctx context.Context, tx *sql.Tx, start int64) error {
	type expiry struct {
		pid []byte
		ts  int64
	}
	rows, err := tx.QueryContext(ctx, `SELECT pending_id, timestamp FROM expired_transfers WHERE timestamp > ? ORDER BY timestamp`, start)
	if err != nil {
		return err
	}
	var seen []expiry
	for rows.Next() {
		var e expiry
		if err := rows.Scan(&e.pid, &e.ts); err != nil {
			rows.Close()
			return err
		}
		seen = append(seen, e)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ROLLBACK TO batch`); err != nil {
		return err
	}
	for _, e := range seen {
		if _, err := tx.ExecContext(ctx, `INSERT INTO expired_transfers (pending_id, timestamp)
			SELECT ?1, ?2 WHERE EXISTS (SELECT 1 FROM transfers WHERE id = ?1)`, e.pid, e.ts); err != nil {
			return fmt.Errorf("ledger: keep an observed expiry: %w", err)
		}
	}
	_, err = tx.ExecContext(ctx, `RELEASE batch`)
	return err
}

// isTransient is TigerBeetle's CreateTransferStatus.transient() (tigerbeetle.zig:322) restricted
// to our subset (the closed-account codes are not reachable).
func isTransient(r CreateTransferResult) bool {
	switch r {
	case TransferDebitAccountNotFound, TransferCreditAccountNotFound, TransferPendingTransferNotFound,
		TransferExceedsCredits, TransferExceedsDebits:
		return true
	}
	return false
}

// validateTransferPre holds the checks before the lookup by id: the event checks (timestamp)
// and the first lines of create_transfer.
func validateTransferPre(t Transfer) CreateTransferResult {
	switch {
	case t.Timestamp != 0: // state_machine.zig:3054-3056 (event check)
		return TransferTimestampMustBeZero
	case t.Flags&^transferFlagsSupported != 0: // :3703
		return TransferReservedFlag
	case t.ID.IsZero(): // :3705
		return TransferIDMustNotBeZero
	case t.ID.IsMax(): // :3706
		return TransferIDMustNotBeIntMax
	}
	return ""
}

func (l *Ledger) createTransfer(ctx context.Context, tx *sql.Tx, t Transfer) (CreateTransferResult, error) {
	existing, found, err := selectTransfer(ctx, tx, t.ID)
	if err != nil {
		return "", err
	}
	if found { // :3709 found_object
		return existsTransfer(ctx, tx, existing, t)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM failed_transfers WHERE id = ?`, idBytes(t.ID)).Scan(&n); err != nil {
		return "", err
	}
	if n > 0 { // :3710 found_orphaned
		return TransferIDAlreadyFailed, nil
	}
	if t.Flags&(TransferPostPendingTransfer|TransferVoidPendingTransfer) != 0 { // :3718
		return l.resolvePending(ctx, tx, t)
	}
	if r := validateTransferFields(t); r != "" {
		return r, nil
	}
	dr, found, err := selectAccount(ctx, tx, t.DebitAccountID)
	if err != nil {
		return "", err
	}
	if !found { // :3748-3755
		return TransferDebitAccountNotFound, nil
	}
	cr, found, err := selectAccount(ctx, tx, t.CreditAccountID)
	if err != nil {
		return "", err
	}
	if !found { // :3757-3764
		return TransferCreditAccountNotFound, nil
	}
	if dr.Ledger != cr.Ledger { // :3769
		return TransferAccountsMustHaveTheSameLedger, nil
	}
	if t.Ledger != dr.Ledger { // :3771
		return TransferTransferMustHaveTheSameLedgerAsAccounts, nil
	}
	// Expiries the ledger sees now are recorded first, so they stay final.
	// The clock is read once, for the expiries and for the timestamp.
	now := l.now().UnixNano()
	if err := recordDueExpiries(ctx, tx, now, dr.ID, cr.ID); err != nil {
		return "", err
	}
	ts, err := nextTimestamp(ctx, tx, now)
	if err != nil {
		return "", err
	}
	// Balances as of the timestamp this transfer would get: expired pending amounts are gone.
	drBal, err := accountBalance(ctx, tx, dr.ID, ts)
	if err != nil {
		return "", err
	}
	crBal, err := accountBalance(ctx, tx, cr.ID, ts)
	if err != nil {
		return "", err
	}
	if r, err := checkBalances(t, ts, dr, cr, drBal, crBal); err != nil || r != "" {
		return r, err
	}

	if err := storeTransfer(ctx, tx, t, ts); err != nil {
		return "", err
	}
	return TransferCreated, nil
}

// resolvePending is fn post_or_void_pending_transfer (state_machine.zig:4036): post or void the
// pending transfer t.PendingID, once. Every decision is a read inside the write transaction; the
// unique index on resolutions is the backstop, never the decision.
func (l *Ledger) resolvePending(ctx context.Context, tx *sql.Tx, t Transfer) (CreateTransferResult, error) {
	post, void := t.Flags&TransferPostPendingTransfer != 0, t.Flags&TransferVoidPendingTransfer != 0
	switch {
	case post && void, t.Flags&TransferPending != 0: // :4048-4057 (balancing/closing are reserved_flag)
		return TransferFlagsAreMutuallyExclusive, nil
	case t.PendingID.IsZero(): // :4058
		return TransferPendingIDMustNotBeZero, nil
	case t.PendingID.IsMax(): // :4059
		return TransferPendingIDMustNotBeIntMax, nil
	case t.PendingID == t.ID: // :4060
		return TransferPendingIDMustBeDifferent, nil
	case t.Timeout != 0: // :4061
		return TransferTimeoutReservedForPendingTransfer, nil
	}
	p, found, err := selectTransfer(ctx, tx, t.PendingID)
	if err != nil {
		return "", err
	}
	if !found { // :4063
		return TransferPendingTransferNotFound, nil
	}
	if p.Flags&TransferPending == 0 { // :4075
		return TransferPendingTransferNotPending, nil
	}
	switch { // inherit-or-match, :4084-4095
	case !t.DebitAccountID.IsZero() && t.DebitAccountID != p.DebitAccountID:
		return TransferPendingTransferHasDifferentDebitAccountID, nil
	case !t.CreditAccountID.IsZero() && t.CreditAccountID != p.CreditAccountID:
		return TransferPendingTransferHasDifferentCreditAccountID, nil
	case t.Ledger != 0 && t.Ledger != p.Ledger:
		return TransferPendingTransferHasDifferentLedger, nil
	case t.Code != 0 && t.Code != p.Code:
		return TransferPendingTransferHasDifferentCode, nil
	}
	amount := t.Amount // :4097-4106
	if (void && t.Amount.IsZero()) || (post && t.Amount.IsMax()) {
		amount = p.Amount
	}
	if amount.Cmp(p.Amount) > 0 { // :4108
		return TransferExceedsPendingTransferAmount, nil
	}
	if void && amount.Cmp(p.Amount) < 0 { // :4110
		return TransferPendingTransferHasDifferentAmount, nil
	}
	// :4113-4121 the status of the pending transfer: has it been resolved? Read, never inferred
	// from an insert error.
	var flags int64
	err = tx.QueryRowContext(ctx, `SELECT flags FROM transfers WHERE pending_id = ? AND (flags & 12) != 0`, idBytes(t.PendingID)).Scan(&flags)
	switch {
	case err == nil:
		if flags&int64(TransferPostPendingTransfer) != 0 {
			return TransferPendingTransferAlreadyPosted, nil
		}
		return TransferPendingTransferAlreadyVoided, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}
	// A recorded expiry is final, whatever the clock says now: a backward clock step must not undo an observed expiry.
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM expired_transfers WHERE pending_id = ?`, idBytes(t.PendingID)).Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return TransferPendingTransferExpired, nil
	}
	ts, err := nextTimestamp(ctx, tx, l.now().UnixNano())
	if err != nil {
		return "", err
	}
	// :4122-4137 expiry is judged against the timestamp this transfer gets (ledger time). The sum
	// fits: overflows_timeout was checked when the pending transfer was created. The decision is
	// recorded in this transaction, and is not a failed_transfers row: it is not transient.
	if p.Timeout != 0 && p.Timestamp+uint64(p.Timeout)*1_000_000_000 <= uint64(ts) {
		if err := recordExpiry(ctx, tx, idBytes(t.PendingID), ts); err != nil {
			return "", err
		}
		return TransferPendingTransferExpired, nil
	}

	// :4188-4206 the stored row has the pending transfer's accounts, ledger and code and the amount
	// actually moved; user data comes from the request when nonzero, else from the pending transfer.
	ud128, ud64, ud32 := p.UserData128, p.UserData64, p.UserData32
	if !t.UserData128.IsZero() {
		ud128 = t.UserData128
	}
	if t.UserData64 != 0 {
		ud64 = t.UserData64
	}
	if t.UserData32 != 0 {
		ud32 = t.UserData32
	}
	if err := storeTransfer(ctx, tx, Transfer{
		ID: t.ID, DebitAccountID: p.DebitAccountID, CreditAccountID: p.CreditAccountID, Amount: amount,
		PendingID: t.PendingID, UserData128: ud128, UserData64: ud64, UserData32: ud32,
		Ledger: p.Ledger, Code: p.Code, Flags: t.Flags,
	}, ts); err != nil {
		return "", err
	}
	return TransferCreated, nil
}

// existsTransfer is fn create_transfer_exists (state_machine.zig:3971) for a request whose id is
// already stored as e.
func existsTransfer(ctx context.Context, tx *sql.Tx, e, t Transfer) (CreateTransferResult, error) {
	if t.Flags&(TransferPostPendingTransfer|TransferVoidPendingTransfer) == 0 || e.Flags != t.Flags ||
		e.PendingID != t.PendingID || e.Timeout != t.Timeout {
		return compareExistingTransfer(e, t), nil // flags, pending_id and timeout come first (:3977-3983)
	}
	p, found, err := selectTransfer(ctx, tx, t.PendingID)
	if err != nil {
		return "", err
	}
	if !found { // :3989 "it must be a valid transfer"
		return "", errors.New("ledger: stored resolution has no pending transfer")
	}
	return postVoidExists(e, t, p), nil
}

// postVoidExists is fn post_or_void_pending_transfer_exists (state_machine.zig:4296-4377), for
// equal flags, pending_id and timeout. e is the stored resolution, p its pending transfer. A zero
// request field means "as the pending transfer", so it is compared with what was stored.
func postVoidExists(e, t, p Transfer) CreateTransferResult {
	switch {
	case !t.DebitAccountID.IsZero() && t.DebitAccountID != e.DebitAccountID: // :4316
		return TransferExistsWithDifferentDebitAccountID
	case !t.CreditAccountID.IsZero() && t.CreditAccountID != e.CreditAccountID: // :4319
		return TransferExistsWithDifferentCreditAccountID
	}
	if t.Flags&TransferVoidPendingTransfer != 0 { // :4323
		want := t.Amount
		if want.IsZero() {
			want = p.Amount
		}
		if want != e.Amount {
			return TransferExistsWithDifferentAmount
		}
	} else { // :4330
		want := t.Amount
		if want.IsMax() {
			want = p.Amount
		}
		if want != e.Amount {
			return TransferExistsWithDifferentAmount
		}
	}
	switch {
	case t.UserData128.IsZero() && e.UserData128 != p.UserData128, !t.UserData128.IsZero() && t.UserData128 != e.UserData128: // :4339
		return TransferExistsWithDifferentUserData128
	case t.UserData64 == 0 && e.UserData64 != p.UserData64, t.UserData64 != 0 && t.UserData64 != e.UserData64: // :4349
		return TransferExistsWithDifferentUserData64
	case t.UserData32 == 0 && e.UserData32 != p.UserData32, t.UserData32 != 0 && t.UserData32 != e.UserData32: // :4359
		return TransferExistsWithDifferentUserData32
	case t.Ledger != 0 && t.Ledger != e.Ledger: // :4369
		return TransferExistsWithDifferentLedger
	case t.Code != 0 && t.Code != e.Code: // :4372
		return TransferExistsWithDifferentCode
	}
	return TransferExists
}

// storeTransfer is the one INSERT into transfers: a plain INSERT, because the lookup before it is
// the only duplicate handling; a trigger or constraint error here is a bug and is returned.
func storeTransfer(ctx context.Context, tx *sql.Tx, t Transfer, ts int64) error {
	id, d, c, amt := t.ID.Bytes(), t.DebitAccountID.Bytes(), t.CreditAccountID.Bytes(), t.Amount.Bytes()
	pid, ud128 := t.PendingID.Bytes(), t.UserData128.Bytes()
	ud64 := make([]byte, 8)
	binary.BigEndian.PutUint64(ud64, t.UserData64)
	_, err := tx.ExecContext(ctx, `INSERT INTO transfers
		(id, debit_account_id, credit_account_id, amount, pending_id, user_data_128, user_data_64,
		 user_data_32, timeout, ledger, code, flags, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id[:], d[:], c[:], amt[:], pid[:], ud128[:], ud64,
		int64(t.UserData32), int64(t.Timeout), int64(t.Ledger), int64(t.Code), int64(t.Flags), ts)
	if err != nil {
		return fmt.Errorf("ledger: insert transfer: %w", err)
	}
	return nil
}

func idBytes(id U128) []byte { b := id.Bytes(); return b[:] }

// validateTransferFields holds state_machine.zig:3722-3741, which need no lookup.
func validateTransferFields(t Transfer) CreateTransferResult {
	switch {
	case t.DebitAccountID.IsZero(): // :3722
		return TransferDebitAccountIDMustNotBeZero
	case t.DebitAccountID.IsMax(): // :3723-3725
		return TransferDebitAccountIDMustNotBeIntMax
	case t.CreditAccountID.IsZero(): // :3726
		return TransferCreditAccountIDMustNotBeZero
	case t.CreditAccountID.IsMax(): // :3727-3729
		return TransferCreditAccountIDMustNotBeIntMax
	case t.CreditAccountID == t.DebitAccountID: // :3730
		return TransferAccountsMustBeDifferent
	case !t.PendingID.IsZero(): // :3732
		return TransferPendingIDMustBeZero
	case t.Flags&TransferPending == 0 && t.Timeout != 0: // :3733-3735
		return TransferTimeoutReservedForPendingTransfer
	case t.Ledger == 0: // :3740
		return TransferLedgerMustNotBeZero
	case t.Code == 0: // :3741
		return TransferCodeMustNotBeZero
	}
	return ""
}

// checkBalances holds the overflow and limit checks, state_machine.zig:3841-3887 (amount_actual
// is the amount: balancing flags are not supported). It returns an error only for corrupt state.
func checkBalances(t Transfer, ts int64, dr, cr Account, drB, crB Balance) (CreateTransferResult, error) {
	pending := t.Flags&TransferPending != 0
	overflows := func(a, b U128) bool { _, ok := a.Add(b); return !ok }
	if pending {
		if overflows(t.Amount, drB.DebitsPending) { // :3841
			return TransferOverflowsDebitsPending, nil
		}
		if overflows(t.Amount, crB.CreditsPending) { // :3844
			return TransferOverflowsCreditsPending, nil
		}
	}
	if overflows(t.Amount, drB.DebitsPosted) { // :3848
		return TransferOverflowsDebitsPosted, nil
	}
	if overflows(t.Amount, crB.CreditsPosted) { // :3851
		return TransferOverflowsCreditsPosted, nil
	}
	// TigerBeetle asserts that pending + posted cannot overflow.
	drTotal, ok1 := drB.DebitsPending.Add(drB.DebitsPosted)
	crTotal, ok2 := crB.CreditsPending.Add(crB.CreditsPosted)
	if !ok1 || !ok2 {
		return "", errors.New("ledger: pending plus posted balance overflows 128 bits")
	}
	if overflows(t.Amount, drTotal) { // :3859
		return TransferOverflowsDebits, nil
	}
	if overflows(t.Amount, crTotal) { // :3866
		return TransferOverflowsCredits, nil
	}
	// timestamp + timeout seconds must fit in 63 bits. Both terms are below 2^63, so the uint64
	// sum cannot wrap.
	if uint64(ts)+uint64(t.Timeout)*1_000_000_000 > math.MaxInt64 { // :3883
		return TransferOverflowsTimeout, nil
	}
	// debits_exceed_credits / credits_exceed_debits (tigerbeetle.zig:34-42). The sums cannot
	// overflow: the checks above passed.
	if dr.Flags&AccountDebitsMustNotExceedCredits != 0 {
		total, _ := drTotal.Add(t.Amount)
		if total.Cmp(drB.CreditsPosted) > 0 { // :3886
			return TransferExceedsCredits, nil
		}
	}
	if cr.Flags&AccountCreditsMustNotExceedDebits != 0 {
		total, _ := crTotal.Add(t.Amount)
		if total.Cmp(crB.DebitsPosted) > 0 { // :3887
			return TransferExceedsDebits, nil
		}
	}
	return "", nil
}

// compareExistingTransfer is fn create_transfer_exists (state_machine.zig:3971): the first
// differing field in TigerBeetle's order, else exists. Our subset has no balancing flags, so the
// amount must be equal.
func compareExistingTransfer(e Transfer, t Transfer) CreateTransferResult {
	switch {
	case e.Flags != t.Flags:
		return TransferExistsWithDifferentFlags
	case e.PendingID != t.PendingID:
		return TransferExistsWithDifferentPendingID
	case e.Timeout != t.Timeout:
		return TransferExistsWithDifferentTimeout
	case e.DebitAccountID != t.DebitAccountID:
		return TransferExistsWithDifferentDebitAccountID
	case e.CreditAccountID != t.CreditAccountID:
		return TransferExistsWithDifferentCreditAccountID
	case e.Amount != t.Amount:
		return TransferExistsWithDifferentAmount
	case e.UserData128 != t.UserData128:
		return TransferExistsWithDifferentUserData128
	case e.UserData64 != t.UserData64:
		return TransferExistsWithDifferentUserData64
	case e.UserData32 != t.UserData32:
		return TransferExistsWithDifferentUserData32
	case e.Ledger != t.Ledger:
		return TransferExistsWithDifferentLedger
	case e.Code != t.Code:
		return TransferExistsWithDifferentCode
	}
	return TransferExists
}

// LookupTransfer returns the transfer with the given id.
func (l *Ledger) LookupTransfer(ctx context.Context, id U128) (Transfer, bool, error) {
	return selectTransfer(ctx, l.db, id)
}

func selectTransfer(ctx context.Context, q queryRower, id U128) (Transfer, bool, error) {
	b := id.Bytes()
	var d, c, amt, pid, ud128, ud64 []byte
	var ud32, timeout, led, code, flags, ts int64
	err := q.QueryRowContext(ctx, `SELECT debit_account_id, credit_account_id, amount, pending_id,
		user_data_128, user_data_64, user_data_32, timeout, ledger, code, flags, timestamp
		FROM transfers WHERE id = ?`, b[:]).Scan(&d, &c, &amt, &pid, &ud128, &ud64, &ud32, &timeout, &led, &code, &flags, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return Transfer{}, false, nil
	}
	if err != nil {
		return Transfer{}, false, err
	}
	if len(ud64) != 8 {
		return Transfer{}, false, errors.New("ledger: corrupt transfer row")
	}
	t := Transfer{ID: id, UserData64: binary.BigEndian.Uint64(ud64)}
	var errs [10]error
	t.DebitAccountID, errs[0] = U128FromBytes(d)
	t.CreditAccountID, errs[1] = U128FromBytes(c)
	t.Amount, errs[2] = U128FromBytes(amt)
	t.PendingID, errs[3] = U128FromBytes(pid)
	t.UserData128, errs[4] = U128FromBytes(ud128)
	t.UserData32, errs[5] = checkedUint32(ud32)
	t.Timeout, errs[6] = checkedUint32(timeout)
	t.Ledger, errs[7] = checkedUint32(led)
	t.Code, errs[8] = checkedUint16(code)
	var f uint16
	f, errs[9] = checkedUint16(flags)
	t.Flags = TransferFlags(f)
	var tsErr error
	t.Timestamp, tsErr = checkedUint64(ts)
	if err := errors.Join(append(errs[:], tsErr)...); err != nil {
		return Transfer{}, false, fmt.Errorf("ledger: corrupt transfer row: %w", err)
	}
	return t, true, nil
}
