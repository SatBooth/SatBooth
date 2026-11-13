// Package status derives one order status from the three rails: Lightning (lnpay), on-chain (onchain) and Monero
// (monero). The api and the checkout use it, so they cannot disagree.
package status

import (
	"context"
	"errors"
	"time"

	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

// The order states.
const (
	StateNew       = "new"
	StatePaid      = "paid"
	StateExpired   = "expired"
	StateUnderpaid = "underpaid"
	StateLate      = "late"
)

// ErrUnavailable is returned for an order with an amount on a rail that is not configured. The error returned is one
// of the two sentinels that wrap it, which name the rail: ErrOnchainUnavailable for an on-chain amount, checked first,
// and ErrMoneroUnavailable for a Monero amount.
var ErrUnavailable = errors.New("status: a rail of the order is not configured")

var (
	ErrOnchainUnavailable error = unavailable("status: the on-chain rail is not configured")
	ErrMoneroUnavailable  error = unavailable("status: the Monero rail is not configured")
)

// unavailable is a sentinel that wraps ErrUnavailable with its own text.
type unavailable string

func (u unavailable) Error() string { return string(u) }
func (unavailable) Unwrap() error   { return ErrUnavailable }

// Order is an order with its derived state. Lightning is nil for AmountMsat == 0, Onchain nil for AmountSat == 0,
// Monero nil for AmountPiconero == 0. Quote is the price and rates of a priced order, as recorded when it was created;
// nil for an order created with explicit amounts. PaidAt is the zero time unless State is paid.
type Order struct {
	Order     store.Order
	Quote     *store.Quote
	State     string
	Lightning *lnpay.Status
	Onchain   *onchain.Rail
	Monero    *rail.View
	PaidAt    time.Time
}

// Lightning is what the resolver needs of *lnpay.Service.
type Lightning interface {
	Status(ctx context.Context, orderID string) (lnpay.Status, error)
	Pay(ctx context.Context, token string) (store.Invoice, error)
	ValidateAmount(msat int64) error
}

// Onchain is what the resolver needs of *onchain.Service.
type Onchain interface {
	Rail(ctx context.Context, o store.Order) (onchain.Rail, error)
	Address(ctx context.Context, orderID string) (store.Address, error)
}

// Monero is what the resolver needs of *monero.Service.
type Monero interface {
	Rail(ctx context.Context, o store.Order) (rail.View, error)
	Address(ctx context.Context, orderID string) (store.Subaddress, error)
}

// Resolver combines the three rails. Oc and Xm are a plain nil when their rail is not configured: assign each only
// inside the configured branch, never a typed nil pointer (a nil *onchain.Service or *monero.Service in the interface
// is not == nil). Now is the clock; nil means time.Now.
type Resolver struct {
	St  *store.Store
	Ln  Lightning
	Oc  Onchain
	Xm  Monero
	Now func() time.Time
}

// HasOnchain reports whether the on-chain rail is configured.
func (r *Resolver) HasOnchain() bool { return r.Oc != nil }

// HasMonero reports whether the Monero rail is configured.
func (r *Resolver) HasMonero() bool { return r.Xm != nil }

// ByID resolves the order with this id.
func (r *Resolver) ByID(ctx context.Context, orderID string) (Order, error) {
	o, err := r.St.OrderByID(ctx, orderID)
	if err != nil {
		return Order{}, err
	}
	return r.resolve(ctx, o)
}

// ByToken resolves the order with this checkout token.
func (r *Resolver) ByToken(ctx context.Context, token string) (Order, error) {
	o, err := r.St.OrderByToken(ctx, token)
	if err != nil {
		return Order{}, err
	}
	return r.resolve(ctx, o)
}

func (r *Resolver) resolve(ctx context.Context, o store.Order) (Order, error) {
	switch {
	case o.AmountSat > 0 && r.Oc == nil:
		return Order{}, ErrOnchainUnavailable
	case o.AmountPiconero > 0 && r.Xm == nil:
		return Order{}, ErrMoneroUnavailable
	}
	out := Order{Order: o}
	q, priced, err := r.St.QuoteForOrder(ctx, o.ID)
	if err != nil {
		return Order{}, err
	}
	if priced {
		out.Quote = &q
	}
	if o.AmountMsat > 0 {
		s, err := r.Ln.Status(ctx, o.ID)
		if err != nil {
			return Order{}, err
		}
		out.Lightning = &s
	}
	if o.AmountSat > 0 {
		rl, err := r.Oc.Rail(ctx, o)
		if err != nil {
			return Order{}, err
		}
		out.Onchain = &rl
	}
	if o.AmountPiconero > 0 {
		v, err := r.Xm.Rail(ctx, o)
		if err != nil {
			return Order{}, err
		}
		out.Monero = &v
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	var views []rail.View
	if out.Onchain != nil {
		views = append(views, out.Onchain.View())
	}
	if out.Monero != nil {
		views = append(views, *out.Monero)
	}
	out.State, out.PaidAt = derive(o, out.Lightning, views, now)
	return out, nil
}

// derive applies the precedence paid > late > underpaid > expired > new over Lightning and the views of the other
// rails the order has (on-chain, Monero). Paid on any rail wins, at the earliest PaidAt of the paid rails.
func derive(o store.Order, ln *lnpay.Status, views []rail.View, now time.Time) (string, time.Time) {
	paid, paidAt := false, time.Time{}
	pay := func(at time.Time) {
		if !paid || at.Before(paidAt) {
			paid, paidAt = true, at
		}
	}
	if ln != nil && ln.State == lnpay.StatePaid {
		pay(ln.PaidAt)
	}
	for _, v := range views {
		if v.Paid {
			pay(v.PaidAt)
		}
	}
	if paid {
		return StatePaid, paidAt
	}
	for _, v := range views {
		if v.Late > 0 {
			return StateLate, time.Time{}
		}
	}
	// underpaid and expired follow settle's rule for order.underpaid and order.expired: past the expiry, and only
	// once Lightning is done (rail.LightningDone), so the API never shows a final state the events lack. Each rail's
	// view already reads those two states from the events; the gate stays for the rest.
	var lns lnpay.Status
	if ln != nil {
		lns = *ln
	}
	if now.Before(o.ExpiresAt) || !rail.LightningDone(o, lns) {
		return StateNew, time.Time{}
	}
	expired := true
	for _, v := range views {
		if v.State == rail.Underpaid {
			return StateUnderpaid, time.Time{}
		}
		expired = expired && v.State == rail.Expired
	}
	if expired {
		return StateExpired, time.Time{}
	}
	return StateNew, time.Time{}
}
