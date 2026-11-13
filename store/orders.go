package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const orderCols = `id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at`

func scanOrder(sc interface{ Scan(...any) error }) (Order, error) {
	var o Order
	var created, expires int64
	if err := sc.Scan(&o.ID, &o.Token, &o.AccountID, &o.MerchantRef, &o.AmountMsat, &o.AmountSat, &o.AmountPiconero, &created, &expires); err != nil {
		return Order{}, err
	}
	o.CreatedAt, o.ExpiresAt = fromTS(created), fromTS(expires)
	return o, nil
}

// orderBy reads the one order whose column col (a constant, never user input) equals val.
func orderBy(ctx context.Context, q queryer, col, val string) (Order, error) {
	o, err := scanOrder(q.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE `+col+` = ?`, val))
	if errors.Is(err, sql.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	return o, err
}

func collectOrders(rows *sql.Rows) ([]Order, error) {
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CreateOrder stores a new explicit order (amounts given by the merchant, no quote) and returns it with true. ID
// and Token must be empty: the store draws them from crypto/rand. CreatedAt is stamped from the clock, and
// ExpiresAt must be after it.
//
// MerchantRef is idempotent: if an explicit order with that reference exists and has the same AccountID,
// AmountMsat, AmountSat and AmountPiconero, CreateOrder returns that order (with its original ID, Token and times) and false; if
// the account or an amount differs, or the existing order is priced (has a quote), it returns ErrConflict. The
// check and the insert are one transaction on the single writer, backed by a unique constraint, so concurrent
// calls with one reference create exactly one order.
func (s *Store) CreateOrder(ctx context.Context, o Order) (Order, bool, error) {
	return s.createOrder(ctx, o, nil)
}

// createOrder is CreateOrder (q nil) and CreateQuotedOrder (q the quote): every check on o and q runs before the
// write, and the order and its quote are inserted in one transaction.
func (s *Store) createOrder(ctx context.Context, o Order, q *Quote) (Order, bool, error) {
	switch {
	case o.ID != "" || o.Token != "":
		return Order{}, false, invalid("ID and Token are drawn by the store")
	case !validName(o.AccountID):
		return Order{}, false, invalid("account id must be 1 to 64 characters of [A-Za-z0-9._-]")
	case !validName(o.MerchantRef):
		return Order{}, false, invalid("merchant reference must be 1 to 64 characters of [A-Za-z0-9._-]")
	case o.AmountMsat != 0 && !validAmount(o.AmountMsat):
		return Order{}, false, invalid("amount must be 0 or 1 to 2^53 msat")
	case o.AmountSat != 0 && !validSat(o.AmountSat):
		return Order{}, false, invalid("on-chain amount must be 0 or 1 to 2100000000000000 sat")
	case o.AmountPiconero != 0 && !validPiconero(o.AmountPiconero):
		return Order{}, false, invalid("Monero amount must be 0 or 1 to 9007199254740991 piconero")
	case o.AmountMsat == 0 && o.AmountSat == 0 && o.AmountPiconero == 0:
		return Order{}, false, invalid("an order needs a Lightning, an on-chain or a Monero amount")
	case o.ExpiresAt.IsZero():
		return Order{}, false, invalid("expiry is required")
	}
	var rows []quoteRateRow
	if q != nil {
		if q.OrderID != "" {
			return Order{}, false, invalid("the quote's order id is filled by the store")
		}
		if err := validQuote(*q); err != nil {
			return Order{}, false, err
		}
		if err := ratesMatchRails(*q, o); err != nil {
			return Order{}, false, err
		}
		var err error
		if rows, err = quoteRateRows(*q); err != nil {
			return Order{}, false, err
		}
	}
	id, err := randHex()
	if err != nil {
		return Order{}, false, err
	}
	token, err := randHex()
	if err != nil {
		return Order{}, false, err
	}
	var out Order
	var created bool
	err = s.write(ctx, func(tx *sql.Tx) error {
		existing, err := orderBy(ctx, tx, "merchant_ref", o.MerchantRef)
		switch {
		case err == nil:
			var same bool
			if q == nil {
				same, err = repeatsExplicit(ctx, tx, existing, o)
			} else {
				// Priced: the amounts follow the rate, which may have moved, so the first order and its locked rate
				// are returned.
				same, err = repeatsPriced(ctx, tx, existing, PricedRequest{AccountID: o.AccountID, MerchantRef: o.MerchantRef,
					Price: q.Price, Currency: q.Currency, Lightning: o.AmountMsat > 0, Onchain: o.AmountSat > 0, Monero: o.AmountPiconero > 0})
			}
			switch {
			case err != nil:
				return err
			case !same:
				return ErrConflict
			}
			out = existing
			return nil
		case !errors.Is(err, ErrNotFound):
			return err
		}
		now := ts(s.now())
		if ts(o.ExpiresAt) <= now {
			return invalid("expiry must be after the creation time")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO orders (`+orderCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, token, o.AccountID, o.MerchantRef, o.AmountMsat, o.AmountSat, o.AmountPiconero, now, ts(o.ExpiresAt)); err != nil {
			return err
		}
		if q != nil {
			if err := insertQuote(ctx, tx, id, *q, rows); err != nil {
				return err
			}
		}
		out = Order{ID: id, Token: token, AccountID: o.AccountID, MerchantRef: o.MerchantRef, AmountMsat: o.AmountMsat, AmountSat: o.AmountSat,
			AmountPiconero: o.AmountPiconero, CreatedAt: fromTS(now), ExpiresAt: fromTS(ts(o.ExpiresAt))}
		created = true
		return nil
	})
	if err != nil {
		return Order{}, false, err
	}
	return out, created, nil
}

// repeatsExplicit reports whether an explicit request o repeats the existing order under its reference: the same
// account and amounts, and no quote.
func repeatsExplicit(ctx context.Context, tx queryer, existing, o Order) (bool, error) {
	if existing.AccountID != o.AccountID {
		return false, nil
	}
	_, hasQuote, err := quoteBy(ctx, tx, existing.ID)
	if err != nil {
		return false, err
	}
	return !hasQuote && existing.AmountMsat == o.AmountMsat && existing.AmountSat == o.AmountSat &&
		existing.AmountPiconero == o.AmountPiconero, nil
}

// OrderByID returns the order with that ID, or ErrNotFound.
func (s *Store) OrderByID(ctx context.Context, id string) (Order, error) {
	if !validLowerHex(id, 32) {
		return Order{}, ErrNotFound
	}
	return orderBy(ctx, s.db, "id", id)
}

// OrderByToken returns the order with that token, or ErrNotFound. A malformed token is simply not found.
func (s *Store) OrderByToken(ctx context.Context, token string) (Order, error) {
	if !validLowerHex(token, 32) {
		return Order{}, ErrNotFound
	}
	return orderBy(ctx, s.db, "token", token)
}

// OrdersByAccount returns the account's orders, oldest first.
func (s *Store) OrdersByAccount(ctx context.Context, accountID string) ([]Order, error) {
	if !validName(accountID) {
		return nil, invalid("account id must be 1 to 64 characters of [A-Za-z0-9._-]")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+orderCols+` FROM orders WHERE account_id = ? ORDER BY created_at, id`, accountID)
	if err != nil {
		return nil, err
	}
	return collectOrders(rows)
}

// OrdersToExpire returns the orders whose ExpiresAt is at or before now and that have no
// order.expired or order.paid event and no open invoice (every invoice has a closed_invoices
// row), including orders that never got an invoice. Oldest expiry first. The caller still checks
// the ledger before recording an expiry. Orders with an on-chain or a Monero amount are left out:
// package lnpay owns order.expired for Lightning-only orders.
func (s *Store) OrdersToExpire(ctx context.Context, now time.Time) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+orderCols+` FROM orders o
WHERE o.expires_at <= ? AND o.amount_sat = 0 AND o.amount_piconero = 0
  AND NOT EXISTS (SELECT 1 FROM events e WHERE e.order_id = o.id AND e.type IN ('order.expired', 'order.paid'))
  AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.order_id = o.id
                  AND NOT EXISTS (SELECT 1 FROM closed_invoices c WHERE c.payment_hash = i.payment_hash))
ORDER BY o.expires_at, o.id`, ts(now))
	if err != nil {
		return nil, err
	}
	return collectOrders(rows)
}
