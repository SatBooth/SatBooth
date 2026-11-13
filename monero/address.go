package monero

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/xmrrpc"
)

// Address returns the order's subaddress: the stored one, or a new one made by the index protocol (the identity read,
// create_address, the index above every recorded one and the address's shape, get_address as the
// cross-check, store, then the store row), in this order, and shown only after the row is stored.
//
// Concurrent calls for one order share one flight, detached from the callers' contexts (a closed page must not
// abandon a call that may already have advanced the wallet's index). Flights for different orders run one at a time
// under the service-wide allocation lock, because the index must rise strictly across all orders. A caller waits
// PressWait at most, then gets ErrBusy; the flight finishes on its own and the next call finds the stored subaddress.
// Before a Check has passed (or after one failed) it refuses with ErrUnavailable; while behind, with ErrIndexBehind.
func (s *Service) Address(ctx context.Context, orderID string) (store.Subaddress, error) {
	o, err := s.eligible(ctx, orderID)
	if err != nil {
		return store.Subaddress{}, err
	}
	if a, err := s.st.SubaddressForOrder(ctx, o.ID); err == nil {
		return a, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Subaddress{}, err
	}
	if s.behind.Load() {
		return store.Subaddress{}, ErrIndexBehind
	}
	if s.baseAddress() == "" {
		return store.Subaddress{}, errNotChecked
	}

	s.mu.Lock()
	f, running := s.flights[o.ID]
	if !running {
		f = &flight{done: make(chan struct{})}
		if len(s.flights) == 0 {
			s.landed = make(chan struct{})
		}
		s.flights[o.ID] = f
	}
	s.mu.Unlock()
	if !running {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*s.callTimeout)
		go func() {
			defer func() {
				cancel()
				s.mu.Lock()
				delete(s.flights, o.ID)
				if len(s.flights) == 0 {
					close(s.landed)
				}
				s.mu.Unlock()
				close(f.done)
			}()
			f.a, f.err = s.allocate(fctx, o)
		}()
	}
	t := time.NewTimer(s.pressWait)
	defer t.Stop()
	select {
	case <-f.done:
		return f.a, f.err
	case <-t.C:
		return store.Subaddress{}, ErrBusy
	case <-ctx.Done():
		return store.Subaddress{}, fmt.Errorf("%w: %w", ErrBusy, ctx.Err())
	}
}

// eligible reads the order and refuses it with ErrNotFound (the store's), ErrNoMonero, ErrExpired or ErrPaid (on any
// rail: Lightning when the order has a Lightning amount, the peer when one is set, or Monero). The flight reads it
// again under the allocation lock, so an order that expired or was paid while the flight waited gets no subaddress.
func (s *Service) eligible(ctx context.Context, orderID string) (store.Order, error) {
	o, err := s.st.OrderByID(ctx, orderID)
	if err != nil {
		return store.Order{}, err
	}
	switch {
	case o.AmountPiconero == 0:
		return store.Order{}, ErrNoMonero
	case !s.now().Before(o.ExpiresAt):
		return store.Order{}, ErrExpired
	}
	if o.AmountMsat > 0 {
		st, err := s.ln.Status(ctx, o.ID)
		if err != nil {
			return store.Order{}, fmt.Errorf("monero: reading the Lightning status: %w", err)
		}
		if st.State == lnpay.StatePaid {
			return store.Order{}, ErrPaid
		}
	}
	if p := s.peerRail(); p != nil {
		v, ok, err := p.View(ctx, o)
		if err != nil {
			return store.Order{}, fmt.Errorf("monero: reading the on-chain rail: %w", err)
		}
		if ok && v.Paid {
			return store.Order{}, ErrPaid
		}
	}
	if paid, err := s.paidMonero(ctx, o); err != nil {
		return store.Order{}, err
	} else if paid {
		return store.Order{}, ErrPaid
	}
	return o, nil
}

// bad logs the failed step (never the subaddress) and returns ErrSubaddress.
func (s *Service) bad(step int, cause error) error {
	s.log.Error(logBadSubaddress, "step", step)
	if cause == nil {
		return fmt.Errorf("%w: step %d", ErrSubaddress, step)
	}
	return fmt.Errorf("%w: step %d: %w", ErrSubaddress, step, cause)
}

// behindAt sets behind, logs the failed step, and returns ErrSubaddress with ErrIndexBehind.
func (s *Service) behindAt(step int) error {
	s.behind.Store(true)
	s.log.Error(logBadSubaddress, "step", step)
	return fmt.Errorf("%w: %w", ErrSubaddress, ErrIndexBehind)
}

// rpcFail maps a failed call of step n: no answer is ErrUnavailable, anything else a failed check.
func (s *Service) rpcFail(step int, err error) error {
	if down(err) {
		return unavailable(err)
	}
	return s.bad(step, err)
}

// errWalletChanged is a press refused because the wallet's base address is not the one the last passed Check kept. It
// wraps ErrUnavailable, so the checkout answers ?busy=1 (ruling 8-T8a).
var errWalletChanged = fmt.Errorf("%w: the wallet's base address changed", ErrUnavailable)

// sameWalletForPress is step 0 of a new subaddress, the identity read of a pass (sameWallet) for a press: in the
// time between a wallet swap and the next pass, a press would otherwise show the wrong wallet's subaddress, and a
// customer would pay a wallet the merchant may not own. Under the allocation lock, so Check's step 5 cannot set the
// base between the read and the comparison. On another base address, or an answer that is not one, the rail pauses as
// a pass's identity read does (the base cleared, watchable false, logPaused with reason address) and the press gets
// errWalletChanged; nothing is created or stored. A failed call is step 0's call failure (rpcFail). A base cleared
// meanwhile (a failed Check) is errNotChecked. It returns the base it checked, which step 3 compares again (8-A3).
func (s *Service) sameWalletForPress(ctx context.Context) (string, error) {
	base := s.baseAddress()
	if base == "" {
		return "", errNotChecked
	}
	c, cancel := s.call(ctx)
	a, err := s.rpc.GetAddress(c, s.cfg.AccountIndex, []uint32{0})
	cancel()
	if err != nil {
		return "", s.rpcFail(0, err)
	}
	if got, ok := baseOf(a); ok && got == base {
		return base, nil
	}
	s.walletChanged()
	return "", errWalletChanged
}

// walletChanged is the pause of an identity read: the wallet's base address is not the kept one, so the base is
// cleared (Address refuses, the next pass re-runs Check) and the watcher stops reading the wallet until a Check
// passes. It moves the wallet generation, so a pass running meanwhile commits nothing (ruling 8-A6). The bump (with
// the base and the watchable flag) is made under the settle mutex (ruling S2), which a pass holds from its generation
// check through its commit and settles, so it lands before the check or after the settles, never between. It makes no
// RPC call under it. None of its callers holds the settle mutex: the press (sameWalletForPress, allocate), and the
// pass's sameWallet and endCheck, which run before the pass takes it.
func (s *Service) walletChanged() {
	mu := s.settleLock()
	mu.Lock()
	s.gen.Add(1)
	s.setBase("")
	s.watchable.Store(false)
	mu.Unlock()
	s.log.Error(logPaused, "reason", checkReason(ErrAddress))
}

// allocate is the flight: ctx is detached from the pressers and bounded.
func (s *Service) allocate(ctx context.Context, o store.Order) (store.Subaddress, error) {
	select {
	case s.alloc <- struct{}{}:
		defer func() { <-s.alloc }()
	case <-ctx.Done():
		return store.Subaddress{}, unavailable(ctx.Err())
	}
	// The order is read again under the lock: it may have expired or been paid while the flight waited.
	if _, err := s.eligible(ctx, o.ID); err != nil {
		return store.Subaddress{}, err
	}
	// A flight that ended after this caller's read must not allocate a second subaddress for the order.
	if a, err := s.st.SubaddressForOrder(ctx, o.ID); err == nil {
		return a, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Subaddress{}, err
	}
	if s.behind.Load() {
		return store.Subaddress{}, ErrIndexBehind
	}
	base, err := s.sameWalletForPress(ctx)
	if err != nil {
		return store.Subaddress{}, err
	}
	max, hasMax, err := s.st.MaxSubaddressIndex(ctx)
	if err != nil {
		return store.Subaddress{}, err
	}
	acct := s.cfg.AccountIndex

	// 1. create_address
	c, cancel := s.call(ctx)
	cr, err := s.rpc.CreateAddress(c, acct, labelSatBooth)
	cancel()
	if err != nil {
		return store.Subaddress{}, s.rpcFail(1, err)
	}
	if err := s.crash("address:after-create"); err != nil {
		return store.Subaddress{}, err
	}

	// 2. the index: at least 1 (0 is the primary address) and strictly above every recorded one; a wallet that
	// answers otherwise is behind the record. Then the range the store holds and the subaddress's shape.
	idx := int64(cr.Index)
	if idx < 1 || (hasMax && idx <= max) {
		return store.Subaddress{}, s.behindAt(2)
	}
	if idx > store.MaxSubaddressIdx || !xmrrpc.ValidAddress(cr.Address, s.cfg.Network, true) {
		return store.Subaddress{}, s.bad(2, nil)
	}

	// 3. get_address at that index: the wallet's own record of it, the cross-check. Its address is the account's base
	// address (wallet-rpc answers get_subaddress_as_str({account_index, 0}), wallet_rpc_server.cpp:706 at
	// v0.18.5.4), which must still be the one step 0 checked: a wallet swapped between that read and create_address
	// answers both calls consistently, and only its base address gives it away (ruling 8-A3). On a difference the
	// rail pauses as step 0 does, and nothing is stored or shown.
	c, cancel = s.call(ctx)
	g, err := s.rpc.GetAddress(c, acct, []uint32{cr.Index})
	cancel()
	if err != nil {
		return store.Subaddress{}, s.rpcFail(3, err)
	}
	if g.Address != base {
		s.walletChanged()
		return store.Subaddress{}, errWalletChanged
	}
	if len(g.Addresses) != 1 || g.Addresses[0].Index != cr.Index || g.Addresses[0].Address != cr.Address {
		return store.Subaddress{}, s.bad(3, nil)
	}
	if err := s.crash("address:after-checks"); err != nil {
		return store.Subaddress{}, err
	}

	// 4. store: the wallet file holds the index before SatBooth records it. On a failure nothing is recorded and the
	// index is skipped (the next create_address yields a higher one), so a wallet that loses it in a crash forgets
	// nothing SatBooth recorded.
	c, cancel = s.call(ctx)
	err = s.rpc.Store(c)
	cancel()
	if err != nil {
		return store.Subaddress{}, unavailable(err)
	}
	if err := s.crash("address:after-store"); err != nil {
		return store.Subaddress{}, err
	}

	// 5. the record, before the subaddress is shown
	a := store.Subaddress{OrderID: o.ID, Address: cr.Address, Index: idx}
	if err := s.st.AddSubaddress(ctx, a); err != nil {
		switch {
		case errors.Is(err, store.ErrSubaddressTaken):
			if got, err2 := s.st.SubaddressForOrder(ctx, o.ID); err2 == nil {
				return got, nil
			}
			return store.Subaddress{}, s.bad(5, err)
		case errors.Is(err, store.ErrIndexNotAbove):
			// Another writer stored a higher index since MaxSubaddressIndex was read. The store's other ErrInvalid
			// causes are ruled out by the checks above, so they are not the symptom of a reset.
			return store.Subaddress{}, s.behindAt(5)
		}
		return store.Subaddress{}, err
	}
	got, err := s.st.SubaddressForOrder(ctx, o.ID)
	if err != nil {
		return store.Subaddress{}, err
	}
	if err := s.crash("address:after-record"); err != nil {
		return store.Subaddress{}, err
	}
	return got, nil
}
