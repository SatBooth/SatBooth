package monero

import (
	"context"
	"errors"
	"fmt"

	"github.com/satbooth/satbooth/xmrrpc"
)

// Check runs the startup checks against wallet-rpc, in a fixed order. Each failure is a fixed error: -13 (no wallet
// open) at any step is ErrNoWallet (ruling 8-T6a), a refused login at any step ErrLogin, -14 at step 2 ErrAccount and
// -7 from the probe ErrRestricted (ruling 8-T6d); any other failed call is ErrUnavailable with the step number. A pass
// keeps the account's base address and clears behind; ErrIndexBehind or ErrSubaddress sets behind; any failure
// withdraws an earlier pass, so Address issues nothing until the next pass. The watcher watches only while the last
// Check passed or failed with ErrIndexBehind or ErrSubaddress (the merchant's wallet, behind on subaddresses; ruling
// 8-T7c): after any other failure the wallet may not be the merchant's, and its answers say nothing about the
// merchant's payments. Between Checks, each pass compares the account's base address with the one kept here
// (sameWallet), so a wallet replaced after a passed Check withdraws it the same way.
func (s *Service) Check(ctx context.Context) error {
	if err := s.check(ctx); err != nil {
		s.setBase("")
		behind := errors.Is(err, ErrIndexBehind) || errors.Is(err, ErrSubaddress)
		if behind {
			s.behind.Store(true)
		}
		s.watchable.Store(behind)
		return err
	}
	s.watchable.Store(true)
	return nil
}

// failed maps a failed call of Check's step to the sentinel that names its cause, or to ErrUnavailable with the step.
func failed(step int, err error) error {
	c, ok := rpcCode(err)
	switch {
	case ok && c == codeNotOpen:
		return ErrNoWallet
	case ok && c == codeAccountRange && step == 2:
		return ErrAccount
	case errors.Is(err, xmrrpc.ErrAuth):
		return ErrLogin
	}
	return &stepError{step: step, err: err}
}

// stepError is ErrUnavailable at one of Check's steps: its text is "<ErrUnavailable>: step <n>: <err>", and it wraps both
// ErrUnavailable and err. CheckStep reads the step without the text, which carries the failed call's error.
type stepError struct {
	step int
	err  error
}

func (e *stepError) Error() string {
	return fmt.Sprintf("%s: step %d: %s", ErrUnavailable.Error(), e.step, e.err.Error())
}

func (e *stepError) Unwrap() []error { return []error{ErrUnavailable, e.err} }

// CheckStep is the number of Check's step whose call failed when err is ErrUnavailable from Check (1 the version, 2 the
// base address, 3 the view-only probe, 4 the height, 5 the subaddress record), and false otherwise. The number is safe
// to log: it carries nothing of wallet-rpc's answer.
func CheckStep(err error) (int, bool) {
	var se *stepError
	if errors.As(err, &se) {
		return se.step, true
	}
	return 0, false
}

// baseOf is the base address get_address answered for index 0 alone: the answer's address, which must also be its
// one entry, at index 0.
func baseOf(a xmrrpc.Addresses) (string, bool) {
	ok := len(a.Addresses) == 1 && a.Addresses[0].Index == 0 && a.Addresses[0].Address == a.Address
	return a.Address, ok
}

func (s *Service) check(ctx context.Context) error {
	// 1. the version: the major must be 1
	c, cancel := s.call(ctx)
	v, err := s.rpc.GetVersion(c)
	cancel()
	if err != nil {
		return failed(1, err)
	}
	if v>>16 != 1 {
		return ErrVersion
	}
	if !s.versionLogged.Swap(true) {
		s.log.Info(logVersion, "major", v>>16, "minor", v&0xffff)
	}

	// 2. the primary address, account 0's; for another account, its own base address is kept. Only index 0 is asked
	// for: an empty list makes on_getaddress list every subaddress of the account (wallet_rpc_server.cpp:684-688),
	// an answer that grows with each one issued until it passes xmrrpc's 8 MiB cap.
	acct := s.cfg.AccountIndex
	c, cancel = s.call(ctx)
	a, err := s.rpc.GetAddress(c, acct, []uint32{0})
	cancel()
	if err != nil {
		return failed(2, err)
	}
	base, ok := baseOf(a)
	if !ok {
		return ErrAddress
	}
	primary := base
	if acct != 0 {
		if !xmrrpc.ValidAddress(base, s.cfg.Network, true) {
			return ErrAddress
		}
		c, cancel = s.call(ctx)
		a, err = s.rpc.GetAddress(c, 0, []uint32{0})
		cancel()
		if err != nil {
			return failed(2, err)
		}
		if primary, ok = baseOf(a); !ok {
			return ErrAddress
		}
	}
	if primary != s.cfg.Address {
		return ErrAddress
	}
	s.setChecked(base)

	// 3. the view-only probe: only -29 (watch-only) passes; it sends no key material and an empty set
	c, cancel = s.call(ctx)
	code, err := s.rpc.SignTransferProbe(c)
	cancel()
	if err != nil {
		return failed(3, err)
	}
	switch code {
	case codeWatchOnly:
	case codeNotOpen:
		return ErrNoWallet
	case codeDenied:
		return ErrRestricted
	default:
		return ErrNotViewOnly
	}

	// 4. the wallet's height
	c, cancel = s.call(ctx)
	h, err := s.rpc.GetHeight(c)
	cancel()
	if err != nil {
		return failed(4, err)
	}
	if h < 1 {
		return ErrHeight
	}
	if cur, ok, err := s.st.MoneroCursor(ctx); err != nil {
		return err
	} else if ok && cur.Height > h {
		s.log.Error(logHeightBehind)
		s.heightLogged.Store(true) // the pass's step 1 logs it no second time (one line per pass)
	}

	// 5. the subaddress record, under the allocation lock, so no allocation moves the wallet's index or the record
	// between the reads.
	if err := s.crash("check:before-lock"); err != nil {
		return err
	}
	select {
	case s.alloc <- struct{}{}:
		defer func() { <-s.alloc }()
	case <-ctx.Done():
		return failed(5, ctx.Err())
	}
	if err := s.checkRecord(ctx); err != nil {
		return err
	}
	s.setBase(base)
	s.behind.Store(false)
	return nil
}

// storeEvery is how many indices the restore creates between two stores of the wallet.
const storeEvery = 100

// checkRecord is step 5: the wallet must know the recorded maximum index m with the recorded subaddress. A wallet
// that answers m out of range (-15) forgot subaddresses SatBooth recorded (wallet-rpc killed after create_address
// and before store): they are created again, one create_address call per index, each index at least 1 and strictly
// above the last, until the wallet's last index reaches m (at most m calls, each under CallTimeout); then m must give
// the recorded subaddress, and the wallet is stored. It is also stored after every storeEvery indices created, short
// of the last: Check runs under the start's bound, and a restore of many indices that a start cuts off
// keeps what it stored, so the next start goes on from there. create_address takes a count (1 to 65,536,
// wallet_rpc_server_commands_defs.h:213-218, wallet_rpc_server.cpp:765-781), which xmrrpc.CreateAddress does not
// send: hence one call per index.
func (s *Service) checkRecord(ctx context.Context) error {
	m, ok, err := s.st.MaxSubaddressIndex(ctx)
	if err != nil || !ok {
		return err
	}
	acct := s.cfg.AccountIndex
	addr, known, err := s.addressAt(ctx, acct, m)
	switch {
	case err != nil:
		return err
	case known:
		return s.matchRecord(ctx, addr, m, ErrSubaddress)
	}

	var made int64
	last := int64(0)
	for made < m {
		c, cancel := s.call(ctx)
		cr, err := s.rpc.CreateAddress(c, acct, labelSatBooth)
		cancel()
		if err != nil {
			return failed(5, err)
		}
		made++
		if int64(cr.Index) < 1 || int64(cr.Index) <= last {
			return ErrIndexBehind
		}
		last = int64(cr.Index)
		if last >= m {
			break
		}
		if made%storeEvery == 0 { // keep the progress of a start that hits its bound
			c, cancel := s.call(ctx)
			err := s.rpc.Store(c)
			cancel()
			if err != nil {
				return failed(5, err)
			}
		}
	}
	addr, known, err = s.addressAt(ctx, acct, m)
	switch {
	case err != nil:
		return err
	case !known:
		return ErrIndexBehind
	}
	if err := s.matchRecord(ctx, addr, m, ErrIndexBehind); err != nil {
		return err
	}
	c, cancel := s.call(ctx)
	err = s.rpc.Store(c)
	cancel()
	if err != nil {
		return failed(5, err)
	}
	s.log.Error(logRecreated, "count", made)
	return nil
}

// addressAt asks the wallet for the account's subaddress at index m. known is false when wallet-rpc answers -15, the
// index out of range.
func (s *Service) addressAt(ctx context.Context, acct uint32, m int64) (addr string, known bool, err error) {
	c, cancel := s.call(ctx)
	a, err := s.rpc.GetAddress(c, acct, []uint32{uint32(m)})
	cancel()
	if err != nil {
		if code, ok := rpcCode(err); ok && code == codeIndexRange {
			return "", false, nil
		}
		return "", false, failed(5, err)
	}
	if len(a.Addresses) != 1 || a.Addresses[0].Index != uint32(m) {
		return "", true, ErrSubaddress
	}
	return a.Addresses[0].Address, true, nil
}

// matchRecord checks that the stored subaddress at index m is addr (the store has one row per address and per
// index, so the row of addr must carry m), else returns bad.
func (s *Service) matchRecord(ctx context.Context, addr string, m int64, bad error) error {
	rows, err := s.st.SubaddressesByAddress(ctx, []string{addr})
	if err != nil {
		return err
	}
	if r, ok := rows[addr]; !ok || r.Index != m {
		return bad
	}
	return nil
}
