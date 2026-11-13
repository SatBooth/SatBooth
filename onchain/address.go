package onchain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/store"
)

// parseHDKeyPath reads getaddressinfo's hdkeypath: "m", then 2 to 11 steps of "/" and a decimal index
// with an optional h or ' marker (Core prints h for a descriptor wallet, addresses.cpp:536-538). The last two steps
// must be exactly 0 and the unhardened address index. Indexes are 0 to 2147483647 with no leading zero except 0.
// Both forms occur: m/0/<i> for a descriptor without an origin, and m/84h/1h/0h/0/<i> with the origin path first.
func parseHDKeyPath(s string) (index int64, ok bool) {
	if len(s) < 5 || len(s) > 1+11*12 || s[0] != 'm' || s[1] != '/' {
		return 0, false
	}
	steps := strings.Split(s[2:], "/")
	if len(steps) < 2 || len(steps) > 11 {
		return 0, false
	}
	for i, p := range steps {
		hardened := false
		if n := len(p); n > 0 && (p[n-1] == 'h' || p[n-1] == '\'') {
			hardened, p = true, p[:n-1]
		}
		v, ok := parseIndex(p)
		if !ok {
			return 0, false
		}
		switch i {
		case len(steps) - 1:
			if hardened {
				return 0, false
			}
			index = v
		case len(steps) - 2:
			if hardened || v != 0 {
				return 0, false
			}
		}
	}
	return index, true
}

func parseIndex(p string) (int64, bool) {
	if len(p) < 1 || len(p) > 10 || (len(p) > 1 && p[0] == '0') {
		return 0, false
	}
	var v int64
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return 0, false
		}
		v = v*10 + int64(p[i]-'0')
	}
	return v, v <= store.MaxAddressIdx
}

// addrPrefix is the prefix of the configured network's addresses.
func (s *Service) addrPrefix() string {
	switch s.cfg.Network {
	case "mainnet":
		return "bc1"
	case "regtest":
		return "bcrt1"
	}
	return "tb1"
}

// Address returns the order's deposit address: the stored one, or a new one made by the protocol
// (getnewaddress, getaddressinfo, deriveaddresses, an index above every recorded one, then the store row), in this
// order, and shown only after the row is stored.
//
// Concurrent calls for one order share one flight, detached from the callers' contexts (a closed page must not
// abandon a call that may already have advanced bitcoind's index). Flights for different orders run one at a time
// under the service-wide allocation lock, because the index must rise strictly across all orders. A caller waits
// PressWait at most, then gets ErrBusy; the flight finishes on its own and the next call finds the stored address.
func (s *Service) Address(ctx context.Context, orderID string) (store.Address, error) {
	o, err := s.eligible(ctx, orderID)
	if err != nil {
		return store.Address{}, err
	}
	if a, err := s.st.AddressForOrder(ctx, o.ID); err == nil {
		return a, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Address{}, err
	}
	if s.behind.Load() {
		return store.Address{}, ErrIndexBehind
	}
	desc := s.descriptor()
	if desc == "" {
		return store.Address{}, errNotChecked
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
			f.a, f.err = s.allocate(fctx, o, desc)
		}()
	}
	t := time.NewTimer(s.pressWait)
	defer t.Stop()
	select {
	case <-f.done:
		return f.a, f.err
	case <-t.C:
		return store.Address{}, ErrBusy
	case <-ctx.Done():
		return store.Address{}, fmt.Errorf("%w: %w", ErrBusy, ctx.Err())
	}
}

// eligible reads the order and refuses it with ErrNotFound (the store's), ErrNoOnchain, ErrExpired or ErrPaid (on
// any rail: Lightning when the order has a Lightning amount, the peer when one is set, or on-chain). The flight reads
// it again under the allocation lock, so an order that expired or was paid while the flight waited gets no address.
func (s *Service) eligible(ctx context.Context, orderID string) (store.Order, error) {
	o, err := s.st.OrderByID(ctx, orderID)
	if err != nil {
		return store.Order{}, err
	}
	switch {
	case o.AmountSat == 0:
		return store.Order{}, ErrNoOnchain
	case !s.now().Before(o.ExpiresAt):
		return store.Order{}, ErrExpired
	}
	if o.AmountMsat > 0 {
		st, err := s.ln.Status(ctx, o.ID)
		if err != nil {
			return store.Order{}, fmt.Errorf("onchain: reading the Lightning status: %w", err)
		}
		if st.State == lnpay.StatePaid {
			return store.Order{}, ErrPaid
		}
	}
	if p := s.peerRail(); p != nil {
		v, ok, err := p.View(ctx, o)
		if err != nil {
			return store.Order{}, fmt.Errorf("onchain: reading the Monero rail: %w", err)
		}
		if ok && v.Paid {
			return store.Order{}, ErrPaid
		}
	}
	if paid, err := s.paidOnchain(ctx, o); err != nil {
		return store.Order{}, err
	} else if paid {
		return store.Order{}, ErrPaid
	}
	return o, nil
}

// bad logs the failed step (never the address) and returns ErrAddress.
func (s *Service) bad(step int, cause error) error {
	s.log.Error(logBadAddress, "step", step)
	if cause == nil {
		return fmt.Errorf("%w: step %d", ErrAddress, step)
	}
	return fmt.Errorf("%w: step %d: %w", ErrAddress, step, cause)
}

// rpcFail maps a failed call of step n: no answer is ErrUnavailable, anything else a failed check.
func (s *Service) rpcFail(step int, err error) error {
	if down(err) {
		return unavailable(err)
	}
	return s.bad(step, err)
}

// allocate is the flight: ctx is detached from the pressers and bounded.
func (s *Service) allocate(ctx context.Context, o store.Order, desc string) (store.Address, error) {
	select {
	case s.alloc <- struct{}{}:
		defer func() { <-s.alloc }()
	case <-ctx.Done():
		return store.Address{}, unavailable(ctx.Err())
	}
	// The order is read again under the lock: it may have expired or been paid while the flight waited.
	if _, err := s.eligible(ctx, o.ID); err != nil {
		return store.Address{}, err
	}
	// A flight that ended after this caller's read must not allocate a second address for the order.
	if a, err := s.st.AddressForOrder(ctx, o.ID); err == nil {
		return a, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Address{}, err
	}
	if s.behind.Load() {
		return store.Address{}, ErrIndexBehind
	}
	max, hasMax, err := s.st.MaxAddressIndex(ctx)
	if err != nil {
		return store.Address{}, err
	}

	// 1. getnewaddress with the explicit type
	c, cancel := s.call(ctx)
	addr, err := s.rpc.GetNewAddress(c, s.cfg.Descriptor.AddressType())
	cancel()
	if err != nil {
		return store.Address{}, s.rpcFail(1, err)
	}
	if err := s.crash("address:after-getnewaddress"); err != nil {
		return store.Address{}, err
	}

	// 2. getaddressinfo: ours, from the configured descriptor, with a path that ends in 0/<index>
	c, cancel = s.call(ctx)
	info, err := s.rpc.GetAddressInfo(c, addr)
	cancel()
	if err != nil {
		return store.Address{}, s.rpcFail(2, err)
	}
	idx, ok := parseHDKeyPath(info.HDKeyPath)
	if info.Address != addr || !info.IsMine || info.ParentDesc != desc || !ok ||
		!store.ValidAddress(addr) || !strings.HasPrefix(addr, s.addrPrefix()) {
		return store.Address{}, s.bad(2, nil)
	}

	// 3. deriveaddresses: a pure function of the descriptor, the independent cross-check
	c, cancel = s.call(ctx)
	l, err := s.rpc.DeriveAddresses(c, desc, idx)
	cancel()
	if err != nil {
		return store.Address{}, s.rpcFail(3, err)
	}
	if len(l) != 1 || l[0] != addr {
		return store.Address{}, s.bad(3, nil)
	}

	// 4. the index must be strictly above every recorded one
	if hasMax && idx <= max {
		s.behind.Store(true)
		s.log.Error(logBadAddress, "step", 4)
		s.log.Error(logBehind)
		return store.Address{}, fmt.Errorf("%w: %w", ErrAddress, ErrIndexBehind)
	}
	if err := s.crash("address:after-checks"); err != nil {
		return store.Address{}, err
	}

	// 5. the record, before the address is shown
	a := store.Address{OrderID: o.ID, Address: addr, Index: idx}
	if err := s.st.AddAddress(ctx, a); err != nil {
		switch {
		case errors.Is(err, store.ErrAddressTaken):
			if got, err2 := s.st.AddressForOrder(ctx, o.ID); err2 == nil {
				return got, nil
			}
			return store.Address{}, s.bad(5, err)
		case errors.Is(err, store.ErrIndexNotAbove):
			// Another writer stored a higher index since MaxAddressIndex was read. The store's other ErrInvalid
			// causes (order id, address shape, index range, an order with no on-chain amount) are ruled out by the
			// checks above, so they are not the symptom of a reset and do not set behind.
			s.behind.Store(true)
			s.log.Error(logBadAddress, "step", 5)
			s.log.Error(logBehind)
			return store.Address{}, fmt.Errorf("%w: %w", ErrAddress, ErrIndexBehind)
		}
		return store.Address{}, err
	}
	got, err := s.st.AddressForOrder(ctx, o.ID)
	if err != nil {
		return store.Address{}, err
	}
	if err := s.crash("address:after-store"); err != nil {
		return store.Address{}, err
	}
	return got, nil
}
