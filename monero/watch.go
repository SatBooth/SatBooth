package monero

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/xmrrpc"
)

// ErrRunning is Run's answer when the watcher already runs.
var ErrRunning = errors.New("monero: Run is already running")

// Fixed log lines of the watcher. They carry the method and the code of a failed call, an order id or a count, never
// wallet-rpc's text, an address or a key.
const (
	logCallFailed      = "ALERT: wallet-rpc call failed"
	logPaymentFailed   = "wallet-rpc call failed for a payment"
	logDropped         = "ALERT: wallet-rpc listed entries SatBooth could not read; reconcile the wallet's transfers from from_height to to_height, and its pool, by hand"
	logOversized       = "ALERT: a Monero payment above 2^53-1 piconero to an issued subaddress is not recorded; reconcile it by hand"
	logOversizedMore   = "ALERT: more Monero payments above 2^53-1 piconero to issued subaddresses are not recorded; reconcile them by hand"
	logAfterWindow     = "ALERT: a Monero payment arrived after the subaddress's watch window"
	logLocked          = "ALERT: a Monero payment is locked by its sender beyond the normal lock; it is treated as late"
	logBadTransfer     = "ALERT: wallet-rpc answered with a bad transfer"
	logBadMore         = "ALERT: wallet-rpc answered with more bad transfers"
	logDoubleSpend     = "ALERT: a Monero payment's inputs were seen double spent; the block decides"
	logDoubleMore      = "ALERT: more Monero payments' inputs were seen double spent; the block decides"
	logFrozen          = "ALERT: the wallet's height has not moved for 30 minutes; is monerod running and synced?"
	logUnused          = "ALERT: unused Monero subaddresses exceed unused_alert; raise the merchant wallet's subaddress lookahead"
	logNoFinal         = "ALERT: a Monero order left its watch window without a final event; check it by hand"
	logUnobserved      = "ALERT: a Monero payment left its watch window without one successful observation; reconcile it from the wallet"
	logUnobservedMore  = "ALERT: more Monero payments left their watch window without one successful observation; reconcile them from the wallet"
	logCheckFailed     = "ALERT: the wallet-rpc check failed; subaddresses are not issued"
	logPaused          = "ALERT: Monero watching is paused: the wallet failed its startup check"
	logDiscarded       = "the wallet changed while the pass read it; nothing from the pass is written, the next pass reads the wallet again"
	logDiscardedRepeat = "ALERT: the wallet changed during several passes in a row; nothing was written from them: is wallet-rpc's wallet or monerod changing?"
	logLeftWallet      = "ALERT: an on-time Monero payment past its watch window left the wallet; SatBooth stops polling it and will notice it again only if a scan lists it mined"
	logScanFailed      = "scan failed"
	logTrackFailed     = "track failed"
	logSettleFailed    = "settle failed"
)

// The watcher's periods: the unused-run ALERT repeats once a day while the run lasts (as the on-chain gap ALERT), and
// a wallet whose height has not moved for 30 minutes (15 Monero blocks of 2 minutes, DIFFICULTY_TARGET_V2,
// src/cryptonote_config.h:80 at v0.18.5.4) is alerted once every 30 minutes.
const (
	unusedAlertEvery = 24 * time.Hour
	frozenAfter      = 30 * time.Minute
)

// tracked is a payment the pass polls, with its order and its subaddress's minor index.
type tracked struct {
	p     store.Payment
	o     store.Order
	index uint32
}

// crashError is a test hook's abort: it ends the pass at once, as a crash would.
type crashError struct{ err error }

func (c crashError) Error() string { return c.err.Error() }
func (c crashError) Unwrap() error { return c.err }

func isCrash(err error) bool {
	var c crashError
	return errors.As(err, &c)
}

// at runs the test hook at a point; a non-nil answer is a crashError.
func (s *Service) at(point string) error {
	if err := s.crash(point); err != nil {
		return crashError{err}
	}
	return nil
}

// passEnd is a failure that ends the pass: the method that failed and its error.
type passEnd struct {
	method string
	err    error
}

func (p passEnd) Error() string { return "monero: " + p.method + " failed" }
func (p passEnd) Unwrap() error { return p.err }

// errSkipped means a payment was skipped for this pass; its line is already logged.
var errSkipped = errors.New("monero: payment skipped for this pass")

// errPaused ends a pass while the wallet fails its Check other than behind (ruling 8-T7c).
var errPaused = errors.New("monero: watching is paused: the wallet failed its check")

// errNotKnown means a payment's transaction is unknown to a wallet that is catching up: no mark, and its order is
// settled on its stored marks.
var errNotKnown = errors.New("monero: the transaction is unknown to a wallet catching up")

// callFailed logs the one logCallFailed line of a pass.
func (s *Service) callFailed(method string, err error) {
	code, _ := rpcCode(err)
	s.log.Error(logCallFailed, "method", method, "code", code)
}

// Run runs a pass at once and then one every PollInterval until ctx ends; it then returns ctx's error. Only one Run
// may run at a time (ErrRunning otherwise), so settle never runs concurrently with itself.
func (s *Service) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer s.running.Store(false)
	tick := time.NewTicker(s.cfg.PollInterval)
	defer tick.Stop()
	for {
		s.pass(ctx) // a pass logs its own failures
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// pass is one pass of the watcher: re-check or the identity read, the wallet's height, discover, track, the end check
// and the commit, settle, the unused run, the orders that left their window. It returns the error that ended it early
// (nil for a pass that ran to its end), for the tests.
//
// The pass is staged (ruling 8-A6): discover and track make no store write. What they would write (the new payments,
// the cursor with the wallet's height h, the marks) is held in memory until the last RPC call. Then the end check reads
// get_height and the wallet's identity again: a height below h, another identity, or a wallet generation that moved
// (walletChanged, from a press or the identity read) discards the whole pass (logDiscarded) and the next pass reads
// the wallet again. Otherwise everything is committed in one store transaction (CommitMoneroPass), and settle runs on
// what was committed. The generation check and the commit hold the settle mutex; no RPC call is made under it.
//
// A failure of the identity read or of steps 1 or 2 ends the pass with one logCallFailed line; then lastGoodPass is not stamped and nothing
// is written, so the next good pass catches up from the stored cursor. In step 3 only a transport error that is not
// the call's own timeout ends the pass; other errors skip the payment and its order. A failure of a store query in
// step 4 lets the pass run to its end but leaves lastGoodPass unstamped. An order with a payment whose call failed, or
// whose settle failed, is in the stale set the pass stores when it reaches its end (Rail's Stale); while the wallet
// is catching up, so is every order the pass touched. A pass that ends early leaves the previous set as it was.
func (s *Service) pass(ctx context.Context) error {
	now := s.now()
	defer s.flushAlerts()

	// 0. Check, while behind or before one has passed (ruling 8-T7a: a failed Check clears the base address, so
	// Address refuses until a Check passes). Behind (ErrIndexBehind, ErrSubaddress: the merchant's wallet, short of
	// subaddresses) the pass watches on. After any other failure the wallet may not be the merchant's: a wrong wallet
	// answers not-found for every txid, which would read as every tracked payment dead. So the pass ends here, with no
	// cursor, no mark and no settle (ruling 8-T7c); the payments are found once a Check passes. A pass that does not
	// run Check makes the identity read instead (sameWallet), so a wallet replaced mid-run pauses the
	// same way.
	s.heightLogged.Store(false)
	// The wallet generation as the pass starts, read before step 0: a press that sees another wallet while this pass's
	// Check runs (which sets the base again when it passes) still makes the commit discard the pass (ruling 8-A6). The
	// identity read's own walletChanged ends the pass here anyway.
	gen := s.gen.Load()
	if s.behind.Load() || s.baseAddress() == "" {
		if err := s.Check(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !s.watchable.Load() {
				s.log.Error(logPaused, "reason", checkReason(err))
				return errPaused
			}
			s.log.Error(logCheckFailed, "reason", checkReason(err))
		}
	} else if err := s.sameWallet(ctx); err != nil {
		return err
	}
	// The wallet as the pass found it: the identity Check last found.
	ident := s.checkedBase()

	// 1. The wallet's height. The pass's tip, where the next scan starts, is the larger of it and the stored cursor's:
	// a wallet below the cursor (restored, rescanning, or moved to a node on a shorter fork) is catching up, and the
	// cursor never goes down. Confirmations are counted from h, the wallet's own height, stored beside the cursor
	// (ruling 8-A2), never from the cursor.
	c, cancel := s.call(ctx)
	h, err := s.rpc.GetHeight(c)
	cancel()
	if err == nil && h < 1 {
		err = fmt.Errorf("%w: get_height: 0", xmrrpc.ErrBadAnswer)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.callFailed("get_height", err)
		return passEnd{"get_height", err}
	}
	cur, hasCur, err := s.st.MoneroCursor(ctx)
	if err != nil {
		return s.scanFailed(ctx, "cursor", err)
	}
	tip, catching := h, false
	if hasCur && cur.Height > h {
		tip, catching = cur.Height, true
		if !s.heightLogged.Load() { // step 0's Check may have logged it in this pass
			s.log.Error(logHeightBehind)
		}
	}

	// 2. Discover: the kept entries, and those of them not yet stored (staged, not written).
	orders := map[string]store.Order{}
	kept, fresh, touched, err := s.discover(ctx, now, cur, hasCur, tip, orders)
	if err != nil {
		return err
	}

	// 3. Track: the stored payments that are watched and the new ones, each mark staged.
	skip, marked, stale, unknown := map[string]bool{}, map[string]bool{}, map[string]struct{}{}, map[store.PaymentKey]bool{}
	refreshed, marks, err := s.trackAll(ctx, now, catching, orders, fresh, touched, skip, marked, stale, unknown)
	if err != nil {
		return err
	}

	// 3b. The end check and the commit (ruling 8-A6).
	if err := s.endCheck(ctx, h, ident); err != nil {
		return err
	}
	// The orders this pass settles after the commit are recorded pending settle by it (ruling S4): the touched ones,
	// less those skipped for a failed call, which this pass does not settle either (an order whose payment keeps
	// failing gets no final event invented once its window closes; it is reported instead, leftWithoutFinal). A skipped
	// order for which the commit writes a new or changed mark is recorded too (ruling T1): it is still not settled in
	// this pass, but the observation it committed is settled by a later pass whatever the window, so a late payment
	// observed in the window's last pass beside a sibling whose call failed is not lost once neither is polled.
	var pendingIDs []string
	for _, id := range touched.ids {
		if !skip[id] || marked[id] {
			pendingIDs = append(pendingIDs, id)
		}
	}
	// 3c and 4 hold the settle mutex once (ruling S2): the generation check, the commit and every settle of this pass,
	// then it is released. walletChanged takes the same mutex for its generation bump, so a press or an identity read
	// that sees another wallet either moves the generation before the check (the pass is discarded) or waits until
	// this pass's settles end; it never lands between the check and the commit, or between the commit and the settles.
	// No RPC call is made under it: the end check's calls come before, and settle makes none. Rail waits for the whole
	// section, not for one settle.
	failed := false
	settledOK := map[string]bool{}
	if err := func() error {
		mu := s.settleLock()
		mu.Lock()
		defer mu.Unlock()
		added, err := s.commit(ctx, now, gen, kept, store.MoneroCursor{Height: tip, WalletHeight: h, At: now}, marks, pendingIDs, orders)
		if err != nil {
			return err
		}
		// The new payments tracked in this pass carry their stored rows from here on (first seen and its height, which
		// counted reads), as WatchedPayments' rows would.
		rows := make(map[store.PaymentKey]store.Payment, len(added))
		for _, p := range added {
			rows[store.PaymentKey{TxID: p.TxID, Address: p.Address}] = p
		}
		for i, t := range refreshed {
			if p, ok := rows[store.PaymentKey{TxID: t.p.TxID, Address: t.p.Address}]; ok {
				refreshed[i].p = p
			}
		}
		if catching {
			for _, id := range touched.ids {
				stale[id] = struct{}{}
			}
		}

		// 4. Settle: the orders touched above, then those pending settle (ruling S4: touched by a pass that committed and
		// did not settle them, a crash between the two, whatever their window), then those past their expiry without a
		// final event, then those awaiting a paid_again. An order with a payment that failed in step 3 waits for the next
		// pass, and stays pending. An order that settled leaves the pending set.
		pending, err := s.st.MoneroPendingSettle(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Error(logSettleFailed, "at", "pending")
			failed = true
		}
		pastExpiry, err := s.st.MoneroOrdersPastExpiry(ctx, now)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Error(logSettleFailed, "at", "past-expiry")
			failed = true
		}
		again, err := s.st.MoneroOrdersAwaitingPaidAgain(ctx, now)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Error(logSettleFailed, "at", "paid-again")
			failed = true
		}
		done := map[string]bool{}
		settleOne := func(o store.Order) error {
			if done[o.ID] || skip[o.ID] {
				return nil
			}
			done[o.ID] = true
			if err := s.settleLocked(ctx, o, unknown); err != nil {
				if isCrash(err) || ctx.Err() != nil {
					return err
				}
				s.log.Error(logSettleFailed, "order", o.ID, "at", "settle")
				stale[o.ID] = struct{}{}
				return nil
			}
			settledOK[o.ID] = true
			return nil
		}
		for _, id := range touched.ids {
			if err := settleOne(orders[id]); err != nil {
				return err
			}
		}
		for _, l := range [][]store.Order{pending, pastExpiry, again} {
			for _, o := range l {
				if err := settleOne(o); err != nil {
					return err
				}
			}
		}
		if err := s.clearPending(ctx, settledOK); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failed = true
		}
		return nil
	}(); err != nil {
		return err
	}
	if run, err := s.st.UnusedSubaddressRun(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.log.Error(logSettleFailed, "at", "unused-run")
		failed = true
	} else {
		s.unusedCheck(now, run)
	}
	if err := s.leftWithoutFinal(ctx, now); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		failed = true
	}
	if err := s.settleDone(ctx, now, refreshed, settledOK); err != nil {
		return err
	}
	s.staleMu.Lock()
	s.staleOrders = stale
	s.staleMu.Unlock()
	if failed {
		return nil // ran to its end, not good
	}
	s.frozenCheck(now, h) // only good passes: a wallet that keeps failing gets logCallFailed instead
	end := s.now()
	s.lastPassDur.Store(int64(max(end.Sub(now), 0)))
	s.lastGoodPass.Store(end.UnixNano())
	return nil
}

// sameWallet is the identity read of a pass that did not run Check (an addition to 8-T7a): before the
// pass marks, settles or records anything, it asks get_address for the account's index 0 and compares the answer
// with the base address the last passed Check kept. A wallet replaced mid-run (wallet-rpc restarted on another wallet
// file, synced above the cursor) would answer not-found for every tracked txid and read as every payment gone. On
// another answer, or one that is not a base address (baseOf, as Check's step 2), the pass clears the base and the
// watchable flag, logs logPaused with reason address and ends (errPaused): the 8-T7c path, so the next pass re-runs
// Check. A failed call is a call failure, as get_height's: one logCallFailed line, the pass ends, nothing is written.
//
// A pass that ran Check in step 0 needs no second read: Check's step 2 compared the same answer with the configured
// address in this pass, and a Check that failed there paused the pass; a Check that failed with ErrIndexBehind or
// ErrSubaddress (step 5, after step 2) cleared the base, which is why the read is not made then.
func (s *Service) sameWallet(ctx context.Context) error {
	c, cancel := s.call(ctx)
	a, err := s.rpc.GetAddress(c, s.cfg.AccountIndex, []uint32{0})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.callFailed("get_address", err)
		return passEnd{"get_address", err}
	}
	if got, ok := baseOf(a); ok && got == s.baseAddress() {
		return nil
	}
	s.walletChanged()
	return errPaused
}

// frozenCheck is the frozen-wallet ALERT: a wallet that lost its daemon keeps answering with the same height. When
// the height read at h has not moved for frozenAfter, it logs logFrozen, then at most once every frozenAfter while it
// stays; a height that moves resets it. The state is per process and belongs to the pass.
func (s *Service) frozenCheck(now time.Time, h int64) {
	if s.heightSince.IsZero() || h != s.heightSeen {
		s.heightSeen, s.heightSince, s.frozenAt = h, now, time.Time{}
		return
	}
	if now.Sub(s.heightSince) < frozenAfter || (!s.frozenAt.IsZero() && now.Sub(s.frozenAt) < frozenAfter) {
		return
	}
	s.frozenAt = now
	s.log.Error(logFrozen)
}

// unusedCheck logs logUnused when the run of unused subaddresses first reaches unused_alert, then at most once every
// unusedAlertEvery while it stays there; a run below it resets it. The state is per process and belongs to the pass.
func (s *Service) unusedCheck(now time.Time, run int64) {
	if run < s.cfg.UnusedAlert {
		s.unusedAt = time.Time{}
		return
	}
	if !s.unusedAt.IsZero() && now.Sub(s.unusedAt) < unusedAlertEvery {
		return
	}
	s.unusedAt = now
	s.log.Error(logUnused, "unused", run)
}

// leftWithoutFinal logs logNoFinal for each Monero order whose watch window closed since the last pass without a final
// event: a payment whose calls kept failing, a settle that kept failing, or an order whose Lightning side (or peer
// rail) is not done, or whose peer rail was stale at its window's end (rulings 8-T7d, 8-T7e: the held event is
// not invented at the window's end; a person decides, and a settle after it writes the event once the peer is fresh). Such an order is no longer settled at its expiry; only its on-time payments still watched can
// settle it later, so a person should check it. The first pass of a process looks back one WatchWindow. No event is
// added.
//
// First, over the same window ends, it logs logUnobserved for each late payment that left its window without one
// successful observation in it (store.MoneroPaymentsLeftUnobserved: no mark, its call failed in every pass that
// committed), whatever its order's events: it is no longer polled and nothing records it, so a person
// reconciles it from the wallet. Once per payment per process, bounded per pass (boundedAlert; flushAlerts logs the
// summary). An on-time payment is not listed: it stays polled past the window.
func (s *Service) leftWithoutFinal(ctx context.Context, now time.Time) error {
	since := s.leftSince
	if since.IsZero() {
		since = now.Add(-store.WatchWindow)
	}
	u, err := s.st.MoneroPaymentsLeftUnobserved(ctx, since, now)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error(logSettleFailed, "at", "left-unobserved")
		}
		return err
	}
	for _, p := range u {
		s.unobsAlert.add(s.log, p.PaymentKey, p.OrderID)
	}
	l, err := s.st.MoneroOrdersLeftWindow(ctx, since, now)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error(logSettleFailed, "at", "left-window")
		}
		return err
	}
	// An order whose peer holds its final event for an output it cannot judge yet (rail.PeerFacts.Held: the on-chain
	// rail's output behind bitcoind's wallet) is not reported in this pass: it is carried (leftHeld) and reported by a
	// later pass only if it still has no final event and is no longer held. The peer reads held only while it is
	// fresh, so a stale peer (an outage, or a restart of the whole process before the on-chain rail's first pass) is
	// reported as before: after such a restart the ALERT may come once, and the final event still follows the catch-up.
	// The Monero rail itself never holds. A failed read keeps the order carried and ends the pass not good (the error
	// returned).
	var carry []store.Order
	var readErr error
	for _, o := range s.leftHeld {
		peer, err := s.peerState(ctx, o)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err() // a stop: leftHeld and leftSince stay as they were
			}
			s.log.Error(logSettleFailed, "order", o.ID, "at", "peer")
			carry = append(carry, o)
			readErr = err
			continue
		}
		if peer.Held {
			carry = append(carry, o)
			continue
		}
		ev, err := s.readEvents(ctx, o.ID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Error(logSettleFailed, "order", o.ID, "at", "events")
			carry = append(carry, o)
			readErr = err
			continue
		}
		if !ev.Final {
			s.log.Error(logNoFinal, "order", o.ID)
		}
	}
	for _, o := range l {
		peer, err := s.peerState(ctx, o)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Error(logSettleFailed, "order", o.ID, "at", "peer")
			carry = append(carry, o)
			readErr = err
			continue
		}
		if peer.Held {
			carry = append(carry, o)
			continue
		}
		s.log.Error(logNoFinal, "order", o.ID)
	}
	s.leftHeld = carry
	if now.After(since) {
		s.leftSince = now
	}
	return readErr
}

// checkReason names a failed Check for the log: one fixed word, never the error's text.
func checkReason(err error) string {
	for _, r := range []struct {
		err  error
		word string
	}{
		{ErrVersion, "version"}, {ErrAddress, "address"}, {ErrNotViewOnly, "view-only"}, {ErrHeight, "height"},
		{ErrIndexBehind, "index-behind"}, {ErrSubaddress, "subaddress"}, {ErrNoWallet, "no-wallet"}, {ErrLogin, "login"},
		{ErrAccount, "account"}, {ErrRestricted, "restricted"}, {ErrUnavailable, "unavailable"},
	} {
		if errors.Is(err, r.err) {
			return r.word
		}
	}
	return "store"
}

// scanFailed is step 1 and 2's store failure: logScanFailed with at, the call's fixed word, unless the pass's context
// ended (a stop), when it is the context's error and no line.
func (s *Service) scanFailed(ctx context.Context, at string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.log.Error(logScanFailed, "at", at)
	return err
}

// trackFailed is scanFailed for step 3's store reads (logTrackFailed). at names the read, one fixed word and nothing
// from the data: watched, addresses, order or mark-read (the same words as the on-chain rail's).
func (s *Service) trackFailed(ctx context.Context, at string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.log.Error(logTrackFailed, "at", at)
	return err
}

// orderList is a set of order ids that keeps their first-seen order.
type orderList struct {
	ids  []string
	seen map[string]bool
}

func (l *orderList) add(id string) {
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	if !l.seen[id] {
		l.seen[id] = true
		l.ids = append(l.ids, id)
	}
}

// order reads an order once per pass.
func (s *Service) order(ctx context.Context, orders map[string]store.Order, id string) (store.Order, error) {
	if o, ok := orders[id]; ok {
		return o, nil
	}
	o, err := s.st.OrderByID(ctx, id)
	if err != nil {
		return store.Order{}, err
	}
	orders[id] = o
	return o, nil
}

// discover is step 2: get_transfers for the account's in and pool entries, mined above max(0, cursor height -
// SettledDepth - 1); min_height is exclusive (wallet2::get_payments keeps a payment when min_height <
// m_block_height, src/wallet/wallet2.cpp:7243 at v0.18.5.4, which on_get_transfers calls with the request's
// min_height, src/wallet/wallet_rpc_server.cpp:2914-2932), so a payment exactly SettledDepth deep is listed again: one
// block of slack, harmless since CommitMoneroPass de-duplicates. It writes nothing (ruling 8-A6): it returns the kept
// entries (keep), each with the clock read when get_transfers answered as its FirstSeen (ruling S3), which the pass
// commits with the cursor {tip, h, now} after its end check (h the wallet's height,
// from which settle counts confirmations, ruling 8-A2; it stamps the new rows' FirstSeenHeight, ruling R6), the kept
// entries not yet stored with their orders and indexes (fresh, which track polls in this pass), and the orders of
// those (touched).
func (s *Service) discover(ctx context.Context, now time.Time, cur store.MoneroCursor, hasCur bool, tip int64,
	orders map[string]store.Order) (kept []store.Payment, fresh []tracked, touched *orderList, err error) {
	minHeight := int64(0)
	if hasCur {
		minHeight = max(0, cur.Height-int64(rail.SettledDepth(s.cfg.Tiers))-1)
	}
	c, cancel := s.call(ctx)
	tr, err := s.rpc.GetTransfers(c, xmrrpc.TransfersQuery{In: true, Pool: true, Account: s.cfg.AccountIndex,
		FilterByHeight: true, MinHeight: minHeight})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		s.callFailed("get_transfers", err)
		return nil, nil, nil, passEnd{"get_transfers", err}
	}
	// The local clock when discovery first sees the payments (ruling S3): staged with each entry, and stored by the
	// commit for the new ones, so the time track spends on wallet-rpc before the commit never makes an on-time payment
	// late. A stored row keeps its own.
	seenAt := s.now()
	kept, subs, stored, dropped, err := s.keep(ctx, tr)
	if err != nil {
		return nil, nil, nil, s.scanFailed(ctx, "keep", err)
	}
	for i := range kept {
		kept[i].FirstSeen = seenAt
	}
	// Ruling 8-A4: an entry SatBooth could not read may be a payment to an issued subaddress. The cursor still moves
	// (holding it would let one bad entry grow every listing without bound), so the next listings may start above it:
	// the ALERT names this listing's range, min_height to the tip, for a person to reconcile from the wallet. Once
	// per pass while it lasts, with the count and the two heights only.
	if dropped > 0 {
		s.log.Error(logDropped, "count", dropped, "from_height", minHeight, "to_height", tip)
	}
	reopen, err := s.reopened(ctx, kept, stored)
	if err != nil {
		return nil, nil, nil, s.scanFailed(ctx, "reopened", err)
	}
	touched = &orderList{}
	for _, p := range kept {
		k := store.PaymentKey{TxID: p.TxID, Address: p.Address}
		row, isStored := stored[k]
		if isStored && !reopen[k] {
			continue
		}
		a := subs[p.Address]
		o, err := s.order(ctx, orders, a.OrderID)
		if err != nil {
			return nil, nil, nil, s.scanFailed(ctx, "order", err)
		}
		if !isStored {
			// Its row is the commit's (first seen at seenAt, at h): the pass puts it in place of this entry after the
			// commit, before anything reads the row's first-seen height.
			touched.add(o.ID)
			row = p
		}
		// WatchedPayments' rule once the payment is stored and not settled: its order's window is open, or it was
		// first seen before the expiry (a new one at seenAt).
		if now.Before(o.ExpiresAt.Add(store.WatchWindow)) || row.FirstSeen.Before(o.ExpiresAt) {
			fresh = append(fresh, tracked{p: row, o: o, index: uint32(a.Index)})
		}
	}
	return kept, fresh, touched, nil
}

// reopened are the kept entries the commit will reopen (CommitMoneroPass, the rule of reopenIfDead): stored, listed
// mined (Confirmed), settled, with a dead current mark. WatchedPayments leaves them out (settled), so the pass tracks
// them itself, as it does a new payment: in the pass that lists them mined, as before the pass was staged.
func (s *Service) reopened(ctx context.Context, kept []store.Payment, stored map[store.PaymentKey]store.Payment) (map[store.PaymentKey]bool, error) {
	var keys []store.PaymentKey
	for _, p := range kept {
		if k := (store.PaymentKey{TxID: p.TxID, Address: p.Address}); p.Confirmed {
			if _, ok := stored[k]; ok {
				keys = append(keys, k)
			}
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	settled, err := s.st.MoneroSettledKeys(ctx, keys)
	if err != nil || len(settled) == 0 {
		return nil, err
	}
	var dead []store.PaymentKey
	for _, k := range keys {
		if settled[k] {
			dead = append(dead, k)
		}
	}
	marks, err := s.st.CurrentMoneroMarks(ctx, dead)
	if err != nil {
		return nil, err
	}
	out := map[store.PaymentKey]bool{}
	for _, k := range dead {
		if m, ok := marks[k]; ok && m.Kind == store.MarkDead {
			out[k] = true
		}
	}
	return out, nil
}

// keep filters get_transfers' entries: of type in or pool, of the configured account, 1 to store.MaxPiconero
// piconero, to a subaddress SatBooth issued, and the first of any (txid, subaddress) listed twice in one list. dropped
// counts the entries that fail those checks (an entry SatBooth could not read), with the client's Dropped. Entries for
// addresses SatBooth never issued (the configured primary address, subaddresses not in the store) are not SatBooth's
// orders: they are skipped without being counted (ruling 8-T10a-2), as the client already skips a coinbase (type
// "block"). So is the pool entry of a (txid, subaddress) also
// listed in (mined), as wallet-rpc can list it around a reorg: the mined entry wins. An entry above store.MaxPiconero
// to an issued subaddress is not stored and not counted: it gets its own ALERT with the order id, once per payment per
// process, at most maxAlertLines a pass and one summary for the rest (alertOversized, rulings 8-A4 and R8). Each kept entry is
// Confirmed when listed in (mined): the commit (CommitMoneroPass) reopens a dead settled payment only then. An entry whose amount
// or unlock_time differs from its stored row is left out with the bad-transfer ALERT (a payment cannot change, so
// wallet-rpc or the store is wrong; CommitMoneroPass would refuse the whole pass, and discovery would stop for every
// order). It also returns the stored rows of the kept entries' txids.
func (s *Service) keep(ctx context.Context, tr xmrrpc.Transfers) ([]store.Payment, map[string]store.Subaddress,
	map[store.PaymentKey]store.Payment, int, error) {
	dropped := tr.Dropped
	var cand []xmrrpc.Transfer
	var addrList []string
	addrSeen := map[string]bool{}
	for _, l := range [][]xmrrpc.Transfer{tr.In, tr.Pool} {
		for _, x := range l {
			if (x.Type != "in" && x.Type != "pool") || x.Account != s.cfg.AccountIndex || x.Amount < 1 {
				dropped++
				continue
			}
			if x.Address == s.cfg.Address {
				continue // the primary address: never issued
			}
			if !store.ValidSubaddress(x.Address) {
				dropped++
				continue
			}
			cand = append(cand, x)
			if !addrSeen[x.Address] {
				addrSeen[x.Address] = true
				addrList = append(addrList, x.Address)
			}
		}
	}
	if len(cand) == 0 {
		return nil, nil, nil, dropped, nil
	}
	subs, err := s.st.SubaddressesByAddress(ctx, addrList)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	var kept []store.Payment
	var txids []string
	// The keys seen per list: the in entries come first (cand keeps tr.In before tr.Pool), so a pool entry whose key
	// is already kept from in is the same payment, mined; a key twice in one list is a repeat.
	at := map[string]map[store.PaymentKey]bool{"in": {}, "pool": {}}
	var big []oversized
	defer func() { s.alertOversized(big) }()
	for _, x := range cand {
		k := store.PaymentKey{TxID: x.TxID, Address: x.Address}
		switch _, issued := subs[x.Address]; {
		case !issued:
			continue // a subaddress SatBooth did not issue
		case x.Amount > store.MaxPiconero:
			big = append(big, oversized{k, subs[x.Address].OrderID})
			continue
		case at[x.Type][k]:
			dropped++
			continue
		case x.Type == "pool" && at["in"][k]:
			continue // listed in and pool: the mined entry wins
		}
		at[x.Type][k] = true
		kept = append(kept, store.Payment{TxID: x.TxID, Address: x.Address, AmountPiconero: x.Amount, UnlockTime: x.UnlockTime,
			Confirmed: x.Type == "in"})
		txids = append(txids, x.TxID)
	}
	if len(kept) == 0 {
		return nil, subs, nil, dropped, nil
	}
	rows, err := s.st.PaymentsByTxIDs(ctx, txids)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	stored := make(map[store.PaymentKey]store.Payment, len(rows))
	for _, r := range rows {
		stored[store.PaymentKey{TxID: r.TxID, Address: r.Address}] = r
	}
	out := kept[:0]
	for _, p := range kept {
		k := store.PaymentKey{TxID: p.TxID, Address: p.Address}
		if old, ok := stored[k]; ok && (old.AmountPiconero != p.AmountPiconero || old.UnlockTime != p.UnlockTime) {
			s.alertBad(k, subs[p.Address].OrderID)
			continue
		}
		out = append(out, p)
	}
	return out, subs, stored, dropped, nil
}

// trackAll is step 3: every payment that is not settled (the store leaves the settled ones out, in any process) of an
// order inside its watch window, and every on-time one past it (store.WatchedPayments), and the new payments discover
// staged (fresh, not yet stored), refreshed with get_transfer_by_txid, PollEvery apart on the real clock. Only txids
// that get_transfers listed are stored, so none is a coinbase entry. touched gains the orders of the payments
// refreshed, and marked those whose payment has a new or changed mark to commit (ruling T1); skip and stale gain
// those of the payments that failed; unknown gains the payments a wallet catching up does not know (errNotKnown),
// which settle does not newly record in this pass (ruling RU). It returns the payments refreshed in this pass (only
// their marks were read after this pass's scan) and the marks to commit (ruling 8-A6: nothing is written here).
func (s *Service) trackAll(ctx context.Context, now time.Time, catching bool, orders map[string]store.Order, fresh []tracked,
	touched *orderList, skip, marked map[string]bool, stale map[string]struct{}, unknown map[store.PaymentKey]bool) ([]tracked,
	[]store.MoneroMark, error) {
	watched, err := s.st.WatchedPayments(ctx, now)
	if err != nil {
		return nil, nil, s.trackFailed(ctx, "watched", err)
	}
	var addrList []string
	for _, p := range watched {
		addrList = append(addrList, p.Address)
	}
	subs, err := s.st.SubaddressesByAddress(ctx, addrList)
	if err != nil {
		return nil, nil, s.trackFailed(ctx, "addresses", err)
	}
	list := make([]tracked, 0, len(watched)+len(fresh))
	for _, p := range watched {
		a := subs[p.Address]
		o, err := s.order(ctx, orders, a.OrderID)
		if err != nil {
			return nil, nil, s.trackFailed(ctx, "order", err)
		}
		list = append(list, tracked{p: p, o: o, index: uint32(a.Index)})
	}
	list = append(list, fresh...) // first seen now: after every stored one, as WatchedPayments orders them

	tick := time.NewTicker(s.cfg.PollEvery)
	defer tick.Stop()
	var end passEnd
	var refreshed []tracked
	var marks []store.MoneroMark
	for _, t := range list {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-tick.C:
		}
		m, changed, err := s.track(ctx, t, catching)
		switch {
		case err == nil:
			touched.add(t.o.ID)
			refreshed = append(refreshed, t)
			if changed {
				marks = append(marks, m)
				marked[t.o.ID] = true
			}
		case errors.Is(err, errNotKnown):
			touched.add(t.o.ID)
			unknown[store.PaymentKey{TxID: t.p.TxID, Address: t.p.Address}] = true
		case ctx.Err() != nil:
			return nil, nil, err
		case errors.As(err, &end):
			s.callFailed(end.method, end.err)
			return nil, nil, err
		default:
			skip[t.o.ID] = true
			stale[t.o.ID] = struct{}{}
		}
	}
	return refreshed, marks, nil
}

// track refreshes one payment and returns its mark, and whether it differs from the stored current one (kind, height
// or the confirmations it carries), so the pass commits it. The entry is the answer's one in or pool entry for the
// payment's subaddress:
//   - an empty answer (the transaction unknown: dropped from the pool, or popped and gone) is dead, except while the
//     wallet is catching up: a wallet that does not yet know a payment is not a wallet that saw it die (errNotKnown);
//   - a known transaction with no entry for the subaddress is no mark, a line, and the order stale (never dead);
//   - in, mined at its height with wallet-rpc's confirmations, at most SettledDepth (no count above it changes a
//     rule, so a mark is not added on every block after it; ruling 8-A6); pool with double_spend_seen, dead on a 0
//     tier (the 0-conf payment reverts) and mempool with the double-spend ALERT otherwise (the block decides); pool,
//     mempool;
//   - an entry whose amount or unlock_time differs from the stored row, a second entry for the subaddress, or an in
//     entry without a height logs the bad-transfer ALERT and skips the payment.
//
// A transport error that is not the call's own timeout ends the pass (passEnd); any other failure skips the payment.
func (s *Service) track(ctx context.Context, t tracked, catching bool) (store.MoneroMark, bool, error) {
	c, cancel := s.call(ctx)
	l, err := s.rpc.GetTransferByTxID(c, t.p.TxID, s.cfg.AccountIndex)
	timedOut := errors.Is(c.Err(), context.DeadlineExceeded)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return store.MoneroMark{}, false, ctx.Err()
		}
		if errors.Is(err, xmrrpc.ErrTransport) && !timedOut {
			return store.MoneroMark{}, false, passEnd{"get_transfer_by_txid", err}
		}
		code, _ := rpcCode(err)
		s.log.Warn(logPaymentFailed, "method", "get_transfer_by_txid", "code", code, "order", t.o.ID)
		return store.MoneroMark{}, false, errSkipped
	}
	k := store.PaymentKey{TxID: t.p.TxID, Address: t.p.Address}
	m := store.MoneroMark{TxID: k.TxID, Address: k.Address}
	if len(l) == 0 {
		m.Kind = store.MarkDead
	} else {
		var e xmrrpc.Transfer
		n := 0
		for _, x := range l {
			if x.Address == k.Address && (x.Type == "in" || x.Type == "pool") {
				e, n = x, n+1
			}
		}
		switch {
		case n == 0:
			s.log.Warn(logPaymentFailed, "method", "get_transfer_by_txid", "code", 0, "order", t.o.ID)
			return store.MoneroMark{}, false, errSkipped
		case n > 1 || e.TxID != k.TxID || e.Amount != t.p.AmountPiconero || e.UnlockTime != t.p.UnlockTime ||
			(e.Type == "in" && e.Height < 1):
			s.alertBad(k, t.o.ID)
			return store.MoneroMark{}, false, errSkipped
		}
		switch {
		case e.Type == "in":
			m.Kind, m.Height = store.MarkMined, e.Height
			m.Confirmations = min(e.Confirmations, int64(rail.SettledDepth(s.cfg.Tiers)))
		case e.DoubleSpendSeen && rail.Required(s.cfg.Tiers, t.o.AmountPiconero) == 0:
			m.Kind = store.MarkDead
		case e.DoubleSpendSeen:
			m.Kind = store.MarkMempool
			s.alertDoubleSpend(k, t.o.ID)
		default:
			m.Kind = store.MarkMempool
		}
	}
	if catching && m.Kind == store.MarkDead {
		return store.MoneroMark{}, false, errNotKnown
	}
	cur, ok, err := s.st.CurrentMoneroMark(ctx, k)
	if err != nil {
		return store.MoneroMark{}, false, s.trackFailed(ctx, "mark-read", errSkipped)
	}
	return m, !ok || cur.Kind != m.Kind || cur.Height != m.Height || cur.Confirmations != m.Confirmations, nil
}

// errDiscarded ends a pass whose end check failed (ruling 8-A6): nothing of it is written.
var errDiscarded = errors.New("monero: the wallet changed during the pass; nothing from it was written")

// discardAlertAfter is how many passes in a row may be discarded before logDiscardedRepeat (once, until a pass
// commits).
const discardAlertAfter = 3

// discarded logs one discarded pass (logDiscarded, a WARN with the reason: height, address or generation) and, at the
// discardAlertAfter-th in a row, logDiscardedRepeat once: a wallet whose height keeps going down, or that keeps being
// replaced, during every pass is not watched at all, and a person should look. Only the pass calls it.
func (s *Service) discarded(reason string) {
	s.discards++
	s.log.Warn(logDiscarded, "reason", reason)
	if s.discards == discardAlertAfter {
		s.log.Error(logDiscardedRepeat, "passes", s.discards)
	}
}

// endCheck is the end of the staged pass's reads (ruling 8-A6), after its last get_transfer_by_txid: get_height again,
// which must not be below h (a wallet that moved to a shorter chain while the pass read it), and the identity read of
// sameWallet, which must still give ident (the base address Check found). A failed call is a call failure as at the
// start: one logCallFailed line, the pass ends, nothing is written. Another identity pauses the rail as sameWallet does
// (walletChanged) and ends the pass (errPaused); a lower height ends it (errDiscarded). Either is logged as a discarded
// pass. A switch to another wallet and back between the identity reads (A, B, A) with no press in between is not seen
// here: both reads see A and the generation does not move.
func (s *Service) endCheck(ctx context.Context, h int64, ident string) error {
	c, cancel := s.call(ctx)
	h2, err := s.rpc.GetHeight(c)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.callFailed("get_height", err)
		return passEnd{"get_height", err}
	}
	c, cancel = s.call(ctx)
	a, err := s.rpc.GetAddress(c, s.cfg.AccountIndex, []uint32{0})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.callFailed("get_address", err)
		return passEnd{"get_address", err}
	}
	if got, ok := baseOf(a); !ok || ident == "" || got != ident {
		s.discarded("address")
		s.walletChanged()
		return errPaused
	}
	if h2 < h {
		s.discarded("height")
		return errDiscarded
	}
	return nil
}

// commit is the staged pass's one write (ruling 8-A6): with the settle mutex held by the caller (the pass, ruling S2),
// if the wallet generation is still gen
// (no walletChanged since the pass's identity read), the scan, the cursor, the marks and the orders the pass touched as
// pending settle (ruling S4) in one store transaction (CommitMoneroPass); else nothing, and the pass is discarded. No RPC call is made under the mutex. After the commit
// it logs the after-window and locked ALERTs of the payments it added, and returns them. A store refusal of a payment that changed ends
// the pass with the bad-transfer ALERT (keep leaves such entries out, so this is the store's backstop); any other store
// error with logScanFailed.
func (s *Service) commit(ctx context.Context, now time.Time, gen uint64, kept []store.Payment, cursor store.MoneroCursor,
	marks []store.MoneroMark, pending []string, orders map[string]store.Order) ([]store.Payment, error) {
	if s.gen.Load() != gen {
		s.discarded("generation")
		return nil, errDiscarded
	}
	added, err := s.st.CommitMoneroPass(ctx, kept, cursor, marks, pending)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, store.ErrPaymentChanged) {
			s.log.Error(logBadTransfer)
			return nil, passEnd{"get_transfers", err}
		}
		s.log.Error(logScanFailed, "at", "record-scan")
		return nil, err
	}
	s.discards = 0
	// The scan, the cursor, the marks and the pending orders are one transaction: a crash before it re-reads everything
	// on the next pass; a crash after it and before settle is completed by the next pass's settle of the pending
	// orders, whatever their window (ruling S4). The test hooks keep their names from
	// before the pass was staged: scan:after-record after every commit, track:after-mark after one that wrote a mark.
	if err := s.at("scan:after-record"); err != nil {
		return nil, err
	}
	if len(marks) > 0 {
		if err := s.at("track:after-mark"); err != nil {
			return nil, err
		}
	}
	for _, p := range added {
		a, err := s.st.SubaddressesByAddress(ctx, []string{p.Address})
		if err != nil {
			return nil, s.scanFailed(ctx, "addresses", err)
		}
		o, err := s.order(ctx, orders, a[p.Address].OrderID)
		if err != nil {
			return nil, s.scanFailed(ctx, "order", err)
		}
		if !now.Before(o.ExpiresAt.Add(store.WatchWindow)) {
			s.log.Error(logAfterWindow, "order", o.ID)
		}
		if lockedBeyondNormal(p.UnlockTime, p.FirstSeenHeight) {
			s.log.Error(logLocked, "order", o.ID)
		}
	}
	return added, nil
}

// clearPending clears the pending-settle rows of the orders that settled in this pass (ruling S4); a crash before it
// settles them once more, which changes nothing. A store error is logged (logSettleFailed) and returned: the pass runs
// to its end, not good.
func (s *Service) clearPending(ctx context.Context, settledOK map[string]bool) error {
	ids := make([]string, 0, len(settledOK))
	for id := range settledOK {
		ids = append(ids, id)
	}
	if err := s.st.ClearMoneroPendingSettle(ctx, ids); err != nil {
		if ctx.Err() == nil {
			s.log.Error(logSettleFailed, "at", "clear-pending")
		}
		return err
	}
	return nil
}

// maxAlerted bounds each per-process ALERT set (boundedAlert): a full set is never cleared.
const maxAlerted = 4096

// maxAlertLines bounds the lines of one ALERT of a payment in one pass (ruling R8); the rest get one
// summary line with their count.
const maxAlertLines = 16

// boundedAlert is an ALERT logged once per payment per process with the order id, bounded per pass (ruling R8, which
// also covers the double-spend and bad-transfer ALERTs): a payment not logged or counted before is logged, at most
// maxAlertLines a pass, and the rest are counted into one summary line (more, with the count only) when the pass
// flushes it. Each payment logged or counted joins seen, which is never cleared: when it holds maxAlerted payments, a
// new one is counted in the summary on every pass it comes back instead (at most one line a pass), so a listing larger
// than the set does not log every payment again on every pass, as clearing the set did. Only the pass's goroutine uses
// it.
type boundedAlert struct {
	line, more   string
	seen         map[store.PaymentKey]struct{}
	lines, extra int // this pass's lines and the payments counted for its summary
}

func newBoundedAlert(line, more string) *boundedAlert {
	return &boundedAlert{line: line, more: more, seen: map[store.PaymentKey]struct{}{}}
}

// add logs the payment's line, counts it for the summary, or does nothing when it was logged or counted before.
func (b *boundedAlert) add(log *slog.Logger, k store.PaymentKey, orderID string) {
	if _, seen := b.seen[k]; seen {
		return
	}
	full := len(b.seen) >= maxAlerted
	if !full {
		b.seen[k] = struct{}{}
	}
	if !full && b.lines < maxAlertLines {
		b.lines++
		log.Error(b.line, "order", orderID)
		return
	}
	b.extra++
}

// flush logs the summary of the payments counted since the last flush, if any, and starts the next pass's count.
func (b *boundedAlert) flush(log *slog.Logger) {
	if b.extra > 0 {
		log.Error(b.more, "count", b.extra)
	}
	b.lines, b.extra = 0, 0
}

// alertDoubleSpend logs logDoubleSpend for a payment (track; boundedAlert, flushed at the pass's end).
func (s *Service) alertDoubleSpend(k store.PaymentKey, orderID string) {
	s.dssAlert.add(s.log, k, orderID)
}

// oversized is a payment above store.MaxPiconero to an issued subaddress, and its order.
type oversized struct {
	k       store.PaymentKey
	orderID string
}

// alertOversized logs the oversized payments of one listing (keep; ruling R8): boundedAlert's rule, flushed at the
// end of the listing.
func (s *Service) alertOversized(big []oversized) {
	for _, b := range big {
		s.bigAlert.add(s.log, b.k, b.orderID)
	}
	s.bigAlert.flush(s.log)
}

// alertBad logs logBadTransfer for a payment (keep and track; boundedAlert, flushed at the pass's end): a listing
// that stays wrong is left out every pass, and alerted once.
func (s *Service) alertBad(k store.PaymentKey, orderID string) { s.badAlert.add(s.log, k, orderID) }

// flushAlerts logs the double-spend, bad-transfer and unobserved-payment summaries of the pass that ends (pass defers
// it, so a pass that ends early flushes too).
func (s *Service) flushAlerts() {
	s.dssAlert.flush(s.log)
	s.badAlert.flush(s.log)
	s.unobsAlert.flush(s.log)
}

// settleDone records the payments settled for good in this pass (store.AddMoneroSettled): the store leaves them out
// of WatchedPayments from then on, in any process, so a restart polls only the unsettled ones (a reorg of
// SettledDepth blocks or more is not detected). Only the payments refreshed in this pass count, and only when their
// order settled without an error. A store error ends the pass (it is not stamped good).
func (s *Service) settleDone(ctx context.Context, now time.Time, refreshed []tracked, settledOK map[string]bool) error {
	cur, ok, err := s.st.MoneroCursor(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.log.Error(logSettleFailed, "at", "cursor")
		return err
	}
	if !ok || cur.WalletHeight < 1 {
		return nil
	}
	for _, t := range refreshed {
		if !settledOK[t.o.ID] {
			continue
		}
		ok, dead, paid := s.settled(ctx, now, cur, t)
		if !ok {
			continue
		}
		added, err := s.st.AddMoneroSettled(ctx, store.PaymentKey{TxID: t.p.TxID, Address: t.p.Address}, cur.WalletHeight)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Error(logSettleFailed, "order", t.o.ID, "at", "add-settled")
			return err
		}
		// The twin of onchain's logLeftPool: a person is told that SatBooth stopped polling an on-time payment of an
		// unpaid order (a paid order, on any rail, needs no person for it).
		if added && dead && !paid {
			s.log.Error(logLeftWallet, "order", t.o.ID)
		}
		if err := s.at("settle:after-settled"); err != nil {
			return err
		}
	}
	return nil
}

// markConfirmations is a mined mark's confirmations at the stored wallet height: the least of the wallet height less
// the mark's height (ruling 8-A2) and the confirmations wallet-rpc answered with the mark (ruling 8-A6), so a wallet
// height read at another moment of the pass never counts more than the wallet said. It is rail.Out.Confirmations'
// count for a Monero output (railTip, Bounded).
func markConfirmations(m store.MoneroMark, walletHeight int64) int64 {
	return min(max(walletHeight-m.Height, 0), m.Confirmations)
}

// settled is the rule of a settled payment, with no RPC call: either its current mark is mined at least SettledDepth
// confirmations deep (markConfirmations: below the wallet's height in cur, wallet-rpc's count, ruling 8-A2: never the
// cursor's height, which a wallet on a shorter chain is below; and wallet-rpc's own count with the mark reaches the
// depth too, ruling 8-A6) and its forward is recorded without a reversal, or its order's watch
// window has closed at now, its order has its final event, its current mark is dead and it has no standing record
// (never recorded, or every forward reversed). The second leaves a dead payment in the stuck set until a pass has
// reversed it and its order is final, so a crash between its dead mark and its reversal is healed. Once settled, it is
// not polled; if a later scan lists it mined, the commit reopens it. dead reports the second branch, and paid
// that the order then reads paid by its events (order.paid, and no order.reverted that no order.paid_again answers).
func (s *Service) settled(ctx context.Context, now time.Time, cur store.MoneroCursor, t tracked) (ok, dead, paid bool) {
	m, marked, err := s.st.CurrentMoneroMark(ctx, store.PaymentKey{TxID: t.p.TxID, Address: t.p.Address})
	if err != nil || !marked {
		return false, false, false
	}
	switch {
	case m.Kind == store.MarkMined && markConfirmations(m, cur.WalletHeight) >= int64(rail.SettledDepth(s.cfg.Tiers)):
		v, err := s.paymentView(ctx, t.o, t.p, t.index)
		return err == nil && v.Recorded, false, false
	case m.Kind == store.MarkDead && !now.Before(t.o.ExpiresAt.Add(store.WatchWindow)):
		v, err := s.paymentView(ctx, t.o, t.p, t.index)
		if err != nil || v.Recorded || v.Foreign {
			return false, false, false
		}
		ev, err := s.readEvents(ctx, t.o.ID)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error(logSettleFailed, "order", t.o.ID, "at", "events")
			}
			return false, false, false
		}
		return ev.Final, true, ev.Paid && !ev.ReversedNow()
	}
	return false, false, false
}
