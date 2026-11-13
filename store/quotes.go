package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/satbooth/satbooth/rates"
)

const quoteCols = `order_id, price, currency, margin_bps, quoted_at`

// sourceJSON is one element of the sources column: {"provider":"kraken","rate":"76005.65","at":1793902500}, the
// time in whole Unix seconds.
type sourceJSON struct {
	Provider string `json:"provider"`
	Rate     string `json:"rate"`
	At       int64  `json:"at"`
}

// ErrStoredQuote is wrapped by QuoteForOrder (and the reads built on it) when a stored quote does not read back as
// one the store accepts: a row written past the store. Its text names the order id, never the quote.
var ErrStoredQuote = errors.New("store: a stored quote is malformed")

// validQuote checks every field of q but OrderID: the price, the currency, the margin, the quote time after 1970, and
// 1 or 2 rates of distinct assets among BTC and XMR, each a decimal with 1 to MaxQuoteSources sources (times after
// 1970, whole seconds: a fraction is dropped on write, not refused).
func validQuote(q Quote) error {
	switch {
	case !validDecimal(q.Price):
		return invalid("price must be 1 to 20 digits, optionally a point and 1 to 18 digits")
	case !rates.ValidCurrency(q.Currency):
		return invalid("currency must be 3 letters of [A-Z], neither BTC nor XMR")
	case q.MarginBps < rates.MinMarginBps || q.MarginBps > rates.MaxMarginBps:
		return invalid("margin must be -5000 to 5000 basis points")
	case ts(q.QuotedAt) <= 0:
		return invalid("quote time must be after 1970")
	case len(q.Rates) < 1 || len(q.Rates) > 2:
		return invalid("a quote needs 1 or 2 rates")
	case len(q.Rates) == 2 && q.Rates[0].Asset == q.Rates[1].Asset:
		return invalid("a quote's rates must be of distinct assets")
	}
	for _, r := range q.Rates {
		switch {
		case r.Asset != rates.AssetBTC && r.Asset != rates.AssetXMR:
			return invalid("a rate's asset must be BTC or XMR")
		case !validDecimal(r.Rate):
			return invalid("rate must be 1 to 20 digits, optionally a point and 1 to 18 digits")
		case len(r.Sources) < 1 || len(r.Sources) > MaxQuoteSources:
			return invalid("a rate needs 1 to 8 sources")
		}
		for _, x := range r.Sources {
			switch {
			case !validProvider(x.Provider):
				return invalid("source provider must be 1 to 16 letters of [a-z]")
			case !validDecimal(x.Rate):
				return invalid("source rate must be 1 to 20 digits, optionally a point and 1 to 18 digits")
			case ts(x.At) <= 0:
				return invalid("source time must be after 1970")
			}
		}
	}
	return nil
}

// ratesMatchRails refuses a quote whose assets are not exactly the order's rails: a BTC rate when the order has a
// Lightning or an on-chain amount, an XMR rate when it has a Monero amount, and no other.
func ratesMatchRails(q Quote, o Order) error {
	var btc, xmr bool
	for _, r := range q.Rates {
		btc = btc || r.Asset == rates.AssetBTC
		xmr = xmr || r.Asset == rates.AssetXMR
	}
	if btc != (o.AmountMsat > 0 || o.AmountSat > 0) || xmr != (o.AmountPiconero > 0) {
		return invalid("a quote's rates must be those of the order's rails: BTC for Lightning or on-chain, XMR for Monero")
	}
	return nil
}

// quoteRateRow is one order_quote_rates row before its insert: the asset, the rate and the sources column.
type quoteRateRow struct {
	asset, rate string
	sources     []byte
}

// quoteRateRows returns the order_quote_rates rows of q, the sources marshalled in the order given.
func quoteRateRows(q Quote) ([]quoteRateRow, error) {
	out := make([]quoteRateRow, len(q.Rates))
	for i, r := range q.Rates {
		b, err := marshalSources(r.Sources)
		if err != nil {
			return nil, err
		}
		out[i] = quoteRateRow{asset: r.Asset, rate: r.Rate, sources: b}
	}
	return out, nil
}

// insertQuote writes the quote row of the order id and then its rate rows, in the caller's transaction.
func insertQuote(ctx context.Context, tx *sql.Tx, orderID string, q Quote, rows []quoteRateRow) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO order_quotes (`+quoteCols+`) VALUES (?, ?, ?, ?, ?)`,
		orderID, q.Price, q.Currency, q.MarginBps, ts(q.QuotedAt)); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO order_quote_rates (order_id, asset, rate, sources) VALUES (?, ?, ?, ?)`,
			orderID, r.asset, r.rate, string(r.sources)); err != nil {
			return err
		}
	}
	return nil
}

// marshalSources returns the sources column for src, in the order given, the times in whole seconds.
func marshalSources(src []QuoteSource) ([]byte, error) {
	out := make([]sourceJSON, len(src))
	for i, x := range src {
		out[i] = sourceJSON{Provider: x.Provider, Rate: x.Rate, At: ts(x.At)}
	}
	return json.Marshal(out)
}

// quoteBy reads the quote of the order id and its rates, sorted by asset, through q (a transaction or the database).
// An order without a quote gives false. A quote without a rate row, or one that fails validQuote, is ErrStoredQuote.
func quoteBy(ctx context.Context, q queryer, orderID string) (Quote, bool, error) {
	var out Quote
	var quoted int64
	err := q.QueryRowContext(ctx, `SELECT `+quoteCols+` FROM order_quotes WHERE order_id = ?`, orderID).Scan(
		&out.OrderID, &out.Price, &out.Currency, &out.MarginBps, &quoted)
	if errors.Is(err, sql.ErrNoRows) {
		return Quote{}, false, nil
	}
	if err != nil {
		return Quote{}, false, err
	}
	out.QuotedAt = fromTS(quoted)
	rows, err := q.QueryContext(ctx, `SELECT asset, rate, sources FROM order_quote_rates WHERE order_id = ? ORDER BY asset`, orderID)
	if err != nil {
		return Quote{}, false, err
	}
	defer rows.Close()
	malformed := false
	for rows.Next() {
		var r AssetRate
		var sources string
		if err := rows.Scan(&r.Asset, &r.Rate, &sources); err != nil {
			return Quote{}, false, err
		}
		var src []sourceJSON
		if err := json.Unmarshal([]byte(sources), &src); err != nil {
			malformed = true
			continue
		}
		r.Sources = make([]QuoteSource, len(src))
		for i, x := range src {
			r.Sources[i] = QuoteSource{Provider: x.Provider, Rate: x.Rate, At: fromTS(x.At)}
		}
		out.Rates = append(out.Rates, r)
	}
	if err := rows.Err(); err != nil {
		return Quote{}, false, err
	}
	// This read-side check may only ever be loosened, never tightened: quotes are immutable, so a stricter rule
	// would make the quotes already stored unreadable.
	if malformed || validQuote(out) != nil {
		return Quote{}, false, fmt.Errorf("%w (order %s)", ErrStoredQuote, orderID)
	}
	return out, true, nil
}

// PricedRequest is what makes a priced request the same order again: the account, the merchant reference, the price
// text as sent (compared byte for byte), the currency and the rail set (Lightning, Onchain, Monero). The amounts are
// not part of it: they follow the rates.
type PricedRequest struct {
	AccountID, MerchantRef     string
	Price, Currency            string
	Lightning, Onchain, Monero bool
}

// repeatsPriced is the one rule of a priced repeat, for PricedRepeat and CreateQuotedOrder alike: the existing order
// under r's reference has r's account, and a quote with r's price text and currency, and r's rail set. The account
// is compared first, so another account's quote is never read.
func repeatsPriced(ctx context.Context, q queryer, existing Order, r PricedRequest) (bool, error) {
	if existing.AccountID != r.AccountID {
		return false, nil
	}
	had, hasQuote, err := quoteBy(ctx, q, existing.ID)
	if err != nil {
		return false, err
	}
	return hasQuote && had.Price == r.Price && had.Currency == r.Currency &&
		(existing.AmountMsat > 0) == r.Lightning && (existing.AmountSat > 0) == r.Onchain &&
		(existing.AmountPiconero > 0) == r.Monero, nil
}

// PricedRepeat looks up r's merchant reference in plain reads, outside a transaction, and returns the order under it
// when that order is a repeat of r (CreateQuotedOrder's rule), ErrConflict when it is any other order (CreateQuotedOrder
// would answer ErrConflict), or ErrNotFound when no order has that reference (a malformed one included). The API
// calls it before pricing; CreateQuotedOrder's own check stays the arbiter when two first requests race.
func (s *Store) PricedRepeat(ctx context.Context, r PricedRequest) (Order, error) {
	if !validName(r.MerchantRef) {
		return Order{}, ErrNotFound
	}
	existing, err := orderBy(ctx, s.db, "merchant_ref", r.MerchantRef)
	if err != nil {
		return Order{}, err
	}
	same, err := repeatsPriced(ctx, s.db, existing, r)
	switch {
	case err != nil:
		return Order{}, err
	case !same:
		return Order{}, ErrConflict
	}
	return existing, nil
}

// CreateQuotedOrder stores a new priced order and its quote in one transaction, and returns the order with true.
// The checks on o are CreateOrder's; q.OrderID must be empty (the store fills it), every other field of q is
// validated, and q's assets must be exactly o's rails (BTC when AmountMsat or AmountSat is positive, XMR when
// AmountPiconero is), all before anything is written. The quote row and its rate rows are written in the order's
// transaction. QuotedAt and each source's At are stored in whole seconds: a fraction is dropped, never refused.
//
// MerchantRef is idempotent: if an order with that reference exists, has the same AccountID and a quote with the
// same Price (byte for byte), Currency and rail set (AmountMsat > 0, AmountSat > 0 and AmountPiconero > 0 each agree
// with o), that order is returned with false, and its quote stays the one first recorded, whatever the rates,
// sources and amounts of this call. Any other existing order with that reference, an explicit one included, gives ErrConflict.
func (s *Store) CreateQuotedOrder(ctx context.Context, o Order, q Quote) (Order, bool, error) {
	return s.createOrder(ctx, o, &q)
}

// QuoteForOrder returns the quote of a priced order, its rates sorted by asset, and true, or the zero Quote and false
// for an explicit or unknown order. A malformed id is simply not found.
func (s *Store) QuoteForOrder(ctx context.Context, orderID string) (Quote, bool, error) {
	if !validLowerHex(orderID, 32) {
		return Quote{}, false, nil
	}
	return quoteBy(ctx, s.db, orderID)
}
