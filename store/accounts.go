package store

import (
	"context"
	"database/sql"
	"errors"
)

// AddAccountPaid records that the order's account has a paid order. The caller writes it after the
// order's payment transfer exists in the ledger (package lnpay), on every path that completes a
// payment, so a replay after a crash writes it too. One row per account: it returns true if it wrote
// the row and false if the account already had one (the first row, naming the first order, stands).
// An unknown order is ErrNotFound. CreatedAt is stamped from the store's clock.
func (s *Store) AddAccountPaid(ctx context.Context, orderID string) (bool, error) {
	if !validLowerHex(orderID, 32) {
		return false, invalid("order id must be 32 lowercase hex characters")
	}
	var wrote bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		var acct string
		if err := tx.QueryRowContext(ctx, `SELECT account_id FROM orders WHERE id = ?`, orderID).Scan(&acct); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM account_paid WHERE account_id = ?`, acct).Scan(&one)
		if err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_paid (account_id, first_order_id, created_at) VALUES (?, ?, ?)`,
			acct, orderID, ts(s.now())); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	return wrote, err
}

// AccountPaid reports whether the account has a paid order (an AddAccountPaid row): one read by
// primary key.
func (s *Store) AccountPaid(ctx context.Context, accountID string) (bool, error) {
	if !validName(accountID) {
		return false, invalid("account id must be 1 to 64 characters of [A-Za-z0-9._-]")
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM account_paid WHERE account_id = ?`, accountID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
