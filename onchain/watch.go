package onchain

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

// ErrRunning is Run's answer when the watcher already runs.
var ErrRunning = errors.New("onchain: Run is already running")

// Fixed log lines of the watcher. They carry the method and the code of a failed call, an order id or a count, never
// bitcoind's text.
const (
	logCallFailed   = "ALERT: bitcoind call failed"
	logOutputFailed = "bitcoind call failed for an output"
	logDropped      = "bitcoind listed entries SatBooth could not read"
	logAfterWindow  = "ALERT: an on-chain payment arrived after the address's watch window"
	logBadTx        = "ALERT: bitcoind answered with a bad transaction"
	logNoFinal      = "ALERT: an on-chain order left its watch window without a final event; check it by hand"
	logLeftPool     = "ALERT: an on-time on-chain payment past its watch window left bitcoind's mempool; SatBooth stops polling it and will notice it again only if it is mined"
	logGap          = "ALERT: unused on-chain addresses exceed gap_alert; raise the merchant wallet's gap limit"
	logCheckFailed  = "ALERT: the bitcoind check failed while the index is behind; addresses are not issued"
	logReanchor     = "ALERT: bitcoind cannot list from the scan's cursor block; the scan is " +
		"retried from the tip: if that succeeds, payments first mined between " +
		"that block and the tip are not discovered (tracked payments still " +
		"refresh)"
	logLoading      = "bitcoind is still loading its mempool; unconfirmed payments wait for the next pass"
	logWalletBehind = "bitcoind's wallet has not yet processed a transaction in the node's mempool that replaces a payment; the order is judged when it has"
	logScanFailed   = "scan failed"
	logTrackFailed  = "track failed"
	logSettleFailed = "settle failed"
)

// trackedOutput is an output the pass polls, with its order.
type trackedOutput struct {
	x store.TXO
	o store.Order
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

// passEnd is a failure that ends the pass with logCallFailed: the method that failed and its error.
type passEnd struct {
	method string
	err    error
}

func (p passEnd) Error() string { return "onchain: " + p.method + " failed" }
func (p passEnd) Unwrap() error { return p.err }

// errSkipped means an output was skipped for this pass; its line is already logged.
var errSkipped = errors.New("onchain: output skipped for this pass")

func rpcCode(err error) int {
	var e *btcrpc.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// callFailed logs the one logCallFailed line of a pass. A call that failed because the pass's context ended (the
// service stops) is no failed call: steps 0 to 2 return the context's error before calling it.
func (s *Service) callFailed(method string, err error) {
	s.log.Error(logCallFailed, "method", method, "code", rpcCode(err))
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

// pass is one pass of the watcher: re-check when behind, the tip, discover, track, settle, the gap. It returns the
// error that ended it early (nil for a pass that ran to its end), for the tests.
//
// A failure of step 0's Check for want of an answer (ErrUnavailable), or any failure of steps 1 or 2, ends the pass
// with one logCallFailed line; then lastGoodPass is not stamped and nothing is written, so the next good pass catches
// up from the stored cursor. In step 3 only a transport error that is not the call's own timeout ends
// the pass; other errors skip the output and its order (a skip does not spoil the pass: one bad output must not make
// every order read stale), except for a late output past its window read only because its order is touched, whose
// failure marks its order stale and skips nothing (trackAll). A failure of a store query in step 4 lets the pass run to
// its end but leaves lastGoodPass unstamped.
//
// Staleness is also per order (plan 6.1): an order with an output whose call failed in step 3, or whose settle
// failed in step 4, is in the stale set the pass stores when it reaches its end (Rail.Stale). An order skipped only
// because the mempool is loading (errLoading), or because bitcoind's wallet is behind its node's mempool (errBehind), is
// not: nothing failed. A pass that ends early learned nothing and leaves
// the previous set as it was.
func (s *Service) pass(ctx context.Context) error {
	now := s.now()

	// 0. Re-check when behind. The pass still watches while the index stays behind.
	checked := false
	if s.behind.Load() {
		checked = true
		switch err := s.Check(ctx); {
		case err == nil, errors.Is(err, ErrIndexBehind): // Check logs the index refusal itself (logBehind)
		case errors.Is(err, ErrUnavailable):
			if ctx.Err() != nil { // a stop, not a failed call
				return ctx.Err()
			}
			s.callFailed("check", err)
			return err
		default:
			if ctx.Err() != nil { // a stop that landed in one of Check's store calls
				return ctx.Err()
			}
			s.log.Error(logCheckFailed, "reason", checkReason(err))
		}
	}

	// 1. The tip.
	c, cancel := s.call(ctx)
	bi, err := s.rpc.GetBlockchainInfo(c)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.callFailed("getblockchaininfo", err)
		return passEnd{"getblockchaininfo", err}
	}
	if !checked && (bi.InitialBlockDownload || bi.Headers-bi.Blocks > 3) { // Check logs it itself
		s.log.Error(logIBD)
	}
	// Until bitcoind has loaded its persisted mempool after a restart, getmempoolentry answers -5 for a
	// transaction it is about to load (btcrpc.MempoolLoaded), so step 3 marks no output dead in this pass.
	c, cancel = s.call(ctx)
	loaded, err := s.rpc.MempoolLoaded(c)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.callFailed("getmempoolinfo", err)
		return passEnd{"getmempoolinfo", err}
	}

	// 2. Discover.
	orders := map[string]store.Order{}
	touched, err := s.discover(ctx, bi, now, orders)
	if err != nil {
		return err
	}

	// 3. Track.
	skip, stale := map[string]bool{}, map[string]struct{}{}
	refreshed, err := s.trackAll(ctx, now, loaded, orders, touched, skip, stale)
	if err != nil {
		return err
	}

	// 4. Settle: the orders touched above, then those past their expiry without a final event, then those awaiting a
	// paid_again. An order with a watched output that failed in step 3 waits for the next pass. A failure here is
	// logged and the other orders go on. A store query that fails leaves the pass unstamped (failed): every order reads stale
	// until a clean pass. A settle that fails puts only its order in the stale set.
	failed := false
	pastExpiry, err := s.pastExpiry(ctx, now)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.log.Error(logSettleFailed, "at", "past-expiry")
		failed = true
	}
	again, err := s.st.OrdersAwaitingPaidAgain(ctx, now)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.log.Error(logSettleFailed, "at", "paid-again")
		failed = true
	}
	done, settledOK := map[string]bool{}, map[string]bool{}
	settleOne := func(o store.Order) error {
		if done[o.ID] || skip[o.ID] {
			return nil
		}
		done[o.ID] = true
		if err := s.settle(ctx, o); err != nil {
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
	for _, l := range [][]store.Order{pastExpiry, again} {
		for _, o := range l {
			if err := settleOne(o); err != nil {
				return err
			}
		}
	}
	if run, err := s.st.UnusedRun(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.log.Error(logSettleFailed, "at", "unused-run")
		failed = true
	} else {
		s.gapCheck(now, run)
	}
	if err := s.leftWithoutFinal(ctx, now); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		failed = true
	}
	s.seenMu.Lock()
	s.heldSeen = maps.Clone(s.heldInPass) // after the settles: the final events held in this pass, for the peer's View
	s.seenMu.Unlock()
	if err := s.settleDone(ctx, now, refreshed, settledOK); err != nil {
		return err
	}
	s.staleMu.Lock()
	s.staleOrders = stale
	s.staleMu.Unlock()
	if failed {
		return nil // ran to its end, not good
	}

	// The duration first: a reader between the two stores sees the new duration with the old end, which errs
	// toward fresh by at most one pass.
	end := s.now()
	s.lastPassDur.Store(int64(max(end.Sub(now), 0)))
	s.lastGoodPass.Store(end.UnixNano())
	return nil
}

// gapAlertEvery is how often the gap ALERT repeats while the run stays at or above gap_alert.
const gapAlertEvery = 24 * time.Hour

// gapCheck logs logGap when the run of unused addresses first reaches gap_alert, then at most once every
// gapAlertEvery while it stays there; a run below gap_alert resets it, so the next crossing logs at once. The state is
// per process (a restart logs it again on its first pass) and belongs to the pass.
func (s *Service) gapCheck(now time.Time, run int64) {
	if run < s.cfg.GapAlert {
		s.gapAlertAt = time.Time{}
		return
	}
	if !s.gapAlertAt.IsZero() && now.Sub(s.gapAlertAt) < gapAlertEvery {
		return
	}
	s.gapAlertAt = now
	s.log.Error(logGap, "unused", run)
}

// leftWithoutFinal logs logNoFinal for each on-chain order whose watch window closed since the last pass without a
// final event: an output whose calls kept failing, a settle that kept failing, or a mixed order whose Lightning side
// is not done yet, or whose peer rail was stale at its window's end (rulings 8-T7d, 8-T7e: the held event is not
// invented at the window's end; a person decides, and a settle after it writes the event once the peer is fresh). Such an order is no longer settled at its expiry (OnchainOrdersPastExpiry); only its on-time
// outputs still watched (the stuck set) can settle it later, so a person should check it. The first pass of a process
// looks back one WatchWindow, so a window that closed while SatBooth was down is reported too (again after each
// restart, for a week). No event is added.
//
// An order whose final event settle held in this pass (heldInPass: an on-time output died after the scan) is not
// reported yet: the next pass writes the event if nothing replaced the payment. So is an order that step 3 skipped
// because bitcoind's wallet is behind its node's mempool (errBehind), or an on-time output of it cannot be judged while
// the mempool is loading (errLoading): it is judged when the wallet has caught up or the mempool has loaded. Either
// is carried to the next pass's call (leftHeld), which reports it if it still has no final event and was not held
// again.
//
// Each carried order is checked on its own: one whose events read fails stays carried and is checked
// again by the next pass, while the others are reported (once: a reported one is not carried), this pass's orders are
// reported and leftSince moves on. The read's error is returned at the end, so the pass is not stamped good.
func (s *Service) leftWithoutFinal(ctx context.Context, now time.Time) error {
	since := s.leftSince
	if since.IsZero() {
		since = now.Add(-store.WatchWindow)
	}
	l, err := s.st.OnchainOrdersLeftWindow(ctx, since, now)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error(logSettleFailed, "at", "left-window")
		}
		return err
	}
	var carry []store.Order
	var readErr error
	for _, o := range s.leftHeld {
		if _, held := s.heldInPass[o.ID]; held {
			carry = append(carry, o)
			continue
		}
		ev, err := s.readEvents(ctx, o.ID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err() // a stop: leftHeld and leftSince stay as they were
			}
			s.log.Error(logSettleFailed, "order", o.ID, "at", "events")
			carry = append(carry, o)
			if readErr == nil {
				readErr = err
			}
			continue
		}
		if !ev.Final {
			s.log.Error(logNoFinal, "order", o.ID)
		}
	}
	for _, o := range l {
		if _, held := s.heldInPass[o.ID]; held {
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
	switch {
	case errors.Is(err, ErrNetwork):
		return "network"
	case errors.Is(err, ErrWallet):
		return "wallet"
	case errors.Is(err, ErrDescriptor):
		return "descriptor"
	}
	return "store"
}

// scanFailed is step 2's store failure: logScanFailed with at, the call's fixed word (as trackFailed's), unless the pass's
// context ended (a stop), when it is the context's error and no line.
func (s *Service) scanFailed(ctx context.Context, at string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.log.Error(logScanFailed, "at", at)
	return err
}

// trackFailed is scanFailed for step 3's store calls (logTrackFailed). at names the call, one fixed word and nothing
// from the data (the rule of checkReason): watched, addresses, order, views, mark-read or mark-write.
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

// discover is step 2: listsinceblock from the stored cursor, the receives to issued addresses recorded with the new
// cursor, and the after-window ALERT for the newly added rows. It returns the orders of the added rows.
func (s *Service) discover(ctx context.Context, bi btcrpc.BlockchainInfo, now time.Time, orders map[string]store.Order) (*orderList, error) {
	cur, ok, err := s.st.Cursor(ctx)
	if err != nil {
		return nil, s.scanFailed(ctx, "cursor", err)
	}
	from := bi.BestBlockHash // only when Check never ran on this store: Check records the first cursor
	if ok {
		from = cur.BlockHash
	}
	list := func(from string) (btcrpc.SinceBlock, error) {
		c, cancel := s.call(ctx)
		defer cancel()
		return s.rpc.ListSinceBlock(c, from, SettledDepth(s.cfg.Tiers)+1)
	}
	sb, err := list(from)
	// Ruling R: a JSON-RPC error (not a transport error) from the stored cursor means bitcoind cannot list from that
	// block: "Block not found" after a reindex, or the pruned node's read error for include_removed
	// (transactions.cpp:628, 663-665). An answer too large to read (ErrBadAnswer: over 8 MiB, btcrpc/client.go)
	// after a long outage on a busy wallet is the same: every later pass would get it again. Waiting would stop
	// discovery for good, so the scan is retried from the tip; the ALERT is logged before the retry, and its text
	// holds whether the retry succeeds or fails too (as on -18, no wallet loaded, when it ends the pass). A
	// transport error, the call's own timeout included, is an outage and never re-anchors. Tracked outputs are
	// refreshed by gettransaction whatever the cursor; what is lost is an output first mined between the lost block
	// and the tip (minus the SettledDepth lag the next scan re-lists), and the ALERT says so.
	var rpcErr *btcrpc.Error
	if err != nil && ok && !errors.Is(err, btcrpc.ErrTransport) && (errors.As(err, &rpcErr) || errors.Is(err, btcrpc.ErrBadAnswer)) {
		s.log.Error(logReanchor, "method", "listsinceblock", "code", rpcCode(err))
		sb, err = list(bi.BestBlockHash)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.callFailed("listsinceblock", err)
		return nil, passEnd{"listsinceblock", err}
	}
	if sb.Dropped > 0 {
		s.log.Warn(logDropped, "count", sb.Dropped)
	}
	kept, addrs, err := s.keep(ctx, sb.Transactions, now, orders)
	if err != nil {
		return nil, s.scanFailed(ctx, "keep", err)
	}
	added, err := s.st.RecordScan(ctx, kept, store.Cursor{BlockHash: sb.LastBlock, Height: bi.Blocks, At: now})
	if err != nil {
		if ctx.Err() != nil { // a stop, not a failed scan
			return nil, ctx.Err()
		}
		// ErrInvalid: a lastblock that is no hash. keep drops rows listed otherwise, and clamps an inherited first-seen
		// to now, so RecordScan's refusal of a first-seen after now is only its backstop.
		if errors.Is(err, store.ErrInvalid) {
			s.callFailed("listsinceblock", err)
			return nil, passEnd{"listsinceblock", err}
		}
		s.log.Error(logScanFailed, "at", "record-scan")
		return nil, err
	}
	// The cursor is persisted in the same transaction as the outputs it covers, after them, so a crash before
	// RecordScan re-delivers everything on the next pass.
	if err := s.at("scan:after-record"); err != nil {
		return nil, err
	}
	touched := &orderList{}
	for _, x := range added {
		o, err := s.order(ctx, orders, addrs[x.Address].OrderID)
		if err != nil {
			return nil, s.scanFailed(ctx, "order", err)
		}
		// Past the window a new row never carries an on-time first-seen: inherit gives one only inside the window
		// (decision 9), and RecordScan stores the clock's now otherwise. So every row added past the window is a
		// payment arriving after it.
		if !now.Before(o.ExpiresAt.Add(store.WatchWindow)) {
			s.log.Error(logAfterWindow, "order", o.ID)
		}
		touched.add(o.ID)
	}
	return touched, nil
}

// keep filters listsinceblock's entries: receives of a positive amount (at most MaxSat) to an address SatBooth issued,
// each Confirmed when listed with a confirmation (RecordScan reopens a dead settled output listed so).
// A (txid, vout) listed twice the same way is kept once; listed twice with different details, or with other details
// than its stored row, it is dropped with the bad-transaction ALERT (an output cannot change, so bitcoind or the store
// is wrong; RecordScan would refuse the whole scan, and discovery would stop for every order).
//
// keep also gives an RBF replacement the first-seen it inherits (inherit), which RecordScan stores for a new row; a
// stored row keeps its own. now is the pass's clock, orders its order cache.
func (s *Service) keep(ctx context.Context, l []btcrpc.WalletTx, now time.Time, orders map[string]store.Order) ([]store.TXO, map[string]store.Address, error) {
	var cand []btcrpc.WalletTx
	var addrList []string
	addrSeen := map[string]bool{}
	for _, w := range l {
		if w.Category != "receive" || w.AmountSat <= 0 || w.AmountSat > store.MaxSat || !store.ValidAddress(w.Address) {
			continue
		}
		cand = append(cand, w)
		if !addrSeen[w.Address] {
			addrSeen[w.Address] = true
			addrList = append(addrList, w.Address)
		}
	}
	if len(cand) == 0 {
		return nil, nil, nil
	}
	addrs, err := s.st.AddressesByAddress(ctx, addrList)
	if err != nil {
		return nil, nil, err
	}
	var kept []store.TXO
	var rivals [][]string // the walletconflicts of each kept entry, joined over its listings
	at := map[store.TXOKey]int{}
	conflict := map[store.TXOKey]bool{}
	for _, w := range cand {
		if _, ok := addrs[w.Address]; !ok {
			continue
		}
		k := store.TXOKey{TxID: w.TxID, Vout: w.Vout}
		if i, dup := at[k]; dup {
			if kept[i].Address != w.Address || kept[i].AmountSat != w.AmountSat {
				conflict[k] = true
			}
			kept[i].Confirmed = kept[i].Confirmed || w.Confirmations > 0
			rivals[i] = append(rivals[i], w.WalletConflicts...)
			continue
		}
		at[k] = len(kept)
		kept = append(kept, store.TXO{TxID: w.TxID, Vout: w.Vout, Address: w.Address, AmountSat: w.AmountSat,
			Confirmed: w.Confirmations > 0})
		rivals = append(rivals, append([]string(nil), w.WalletConflicts...))
	}
	stored := make([]bool, len(kept))
	for i, x := range kept {
		k := store.TXOKey{TxID: x.TxID, Vout: x.Vout}
		old, err := s.st.TXO(ctx, k)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return nil, nil, err
		default:
			stored[i] = true
			if old.Address != x.Address || old.AmountSat != x.AmountSat {
				conflict[k] = true
			}
		}
	}
	if len(conflict) > 0 {
		s.log.Error(logBadTx)
		var k2 []store.TXO
		var r2 [][]string
		var s2 []bool
		for i, x := range kept {
			if !conflict[store.TXOKey{TxID: x.TxID, Vout: x.Vout}] {
				k2, r2, s2 = append(k2, x), append(r2, rivals[i]), append(s2, stored[i])
			}
		}
		kept, rivals, stored = k2, r2, s2
	}
	if err := s.inherit(ctx, kept, rivals, stored, now, addrs, orders); err != nil {
		return nil, nil, err
	}
	return kept, addrs, nil
}

// inherit is plan 6.1's inheritance rule (narrowed by Rulings 6.1-S
// and 6.1-U). It decides, per transaction and address, whether a replacement inherits the first-seen of an on-time
// transaction it replaced, so a fee bump of a payment the customer committed on time counts as a normal payment.
//
// A group is the kept outputs of one transaction to one address. It inherits only when all of these hold:
//   - the pass's clock now is inside the order's watch window, before ExpiresAt + WatchWindow (decision 9:
//     without it, an on-time payment kept unconfirmed could be bumped at the quoted amount weeks after the expiry and
//     count on time, a free option on the quote);
//   - bitcoind lists it as conflicting with a stored transaction T (rivals, its walletconflicts: every wallet
//     transaction that spends one of its inputs, *source*: Core 32.1 src/wallet/wallet.cpp:695-715 and 2093-2100,
//     src/wallet/rpc/transactions.cpp:47-50);
//   - every stored output of T to the address was first seen before the order's expiry;
//   - the group's total to the address equals T's total to it, exactly (with "at least", or per output, a small
//     on-time payment bumped or split into more after the expiry would make a late payment count as on time);
//   - exclusivity: every other stored output of the order first seen before the expiry, and every group decided
//     counted earlier in this scan, belongs to a transaction in the group's walletconflicts. A counted transaction
//     that does not conflict with it could confirm with it, and the two would count more than was committed on time.
//
// The group then takes the earliest such T's time (the latest of T's own outputs' times), all its outputs together;
// otherwise none of them inherits. The rule errs toward late: a customer who paid in two independent on-time parts and
// bumps one after the expiry gets that part late.
//
// With it, a set of outputs that can confirm together and holds an inheritor holds no other counted transaction, and
// the inheritor's total is a total committed on time (TestInheritanceNeverExceedsTheOnTimeCommitment).
//
// A group with a stored output keeps its rows (the stored row wins). An inherited time after now (the clock stepped
// back) is clamped to now, so RecordScan, which refuses a first-seen after now, never fails the scan for every order.
//
// The rule reads the listing, stored rows (which never change) and the pass's clock, never the marks. A crash before
// RecordScan re-runs it on the next pass (the cursor re-lists the entry), not always to the same result: the clock may
// cross the expiry in between (a group that would have been counted by its own time is then decided late) or the
// window's end (a group that would have inherited is then decided late, decision 9), the clamp
// may give another now, and outputs stored meanwhile join the exclusivity test. What holds whatever the order of
// scans and crashes is the on-time invariant above, since every decision is made against the stored rows and the
// scan's own earlier decisions. After RecordScan the stored row stands. A chain of bumps lists every ancestor
// (bumpfee keeps the inputs), and a stored middle bump already carries the first one's time.
// It is decided once, when the replacement is first stored: a wallet that hears of the original only later gives the
// replacement its own.
func (s *Service) inherit(ctx context.Context, kept []store.TXO, rivals [][]string, stored []bool, now time.Time,
	addrs map[string]store.Address, orders map[string]store.Order) error {
	type group struct {
		txid, addr string
		idx        []int
	}
	var all []*group
	byKey := map[[2]string]*group{}
	for i, x := range kept {
		k := [2]string{x.TxID, x.Address}
		g := byKey[k]
		if g == nil {
			g = &group{txid: x.TxID, addr: x.Address}
			byKey[k] = g
			all = append(all, g)
		}
		g.idx = append(g.idx, i)
	}
	// A group with a stored output keeps its rows: it is left out before any store read.
	var groups []*group
	var ids []string
	for _, g := range all {
		fresh := true
		for _, i := range g.idx {
			fresh = fresh && !stored[i]
		}
		if !fresh {
			continue
		}
		groups = append(groups, g)
		for _, i := range g.idx {
			ids = append(ids, rivals[i]...)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.st.TXOsByTxIDs(ctx, ids)
	if err != nil {
		return err
	}
	byTx := map[string][]store.TXO{}
	for _, r := range rows {
		byTx[r.TxID] = append(byTx[r.TxID], r)
	}
	counted := map[string][]string{} // per order: the transactions this scan decided counted, in order
	for _, g := range groups {
		o, err := s.order(ctx, orders, addrs[g.addr].OrderID)
		if err != nil {
			return err
		}
		conf := map[string]bool{}
		var total int64
		for _, i := range g.idx {
			for _, id := range rivals[i] {
				conf[id] = true
			}
			total = addSat(total, kept[i].AmountSat)
		}
		var at time.Time
		// Decision 9: only inside the order's watch window. Past it the replacement is a new output (late), so an
		// on-time payment kept unconfirmed cannot be bumped at the quoted amount weeks later and count on time.
		inWindow := now.Before(o.ExpiresAt.Add(store.WatchWindow))
		for id := range conf {
			if t, ok := onTimeTotal(byTx[id], g.addr, o.ExpiresAt, total); inWindow && ok && id != g.txid && (at.IsZero() || t.Before(at)) {
				at = t
			}
		}
		if !at.IsZero() {
			ok, err := s.exclusive(ctx, o, g.txid, conf, counted[o.ID])
			if err != nil {
				return err
			}
			if !ok {
				at = time.Time{}
			}
		}
		if at.After(now) {
			at = now
		}
		for _, i := range g.idx {
			kept[i].FirstSeen = at // zero: its own, the clock's now in RecordScan
		}
		seen := at
		if seen.IsZero() {
			seen = now
		}
		if seen.Before(o.ExpiresAt) {
			counted[o.ID] = append(counted[o.ID], g.txid)
		}
	}
	return nil
}

// onTimeTotal reports T's time for inherit: the latest first-seen of T's stored outputs to addr, when they are all
// before expires and sum to exactly total.
func onTimeTotal(rows []store.TXO, addr string, expires time.Time, total int64) (time.Time, bool) {
	var sum int64
	var at time.Time
	for _, r := range rows {
		if r.Address != addr {
			continue
		}
		if !r.FirstSeen.Before(expires) {
			return time.Time{}, false
		}
		sum = addSat(sum, r.AmountSat)
		if r.FirstSeen.After(at) {
			at = r.FirstSeen
		}
	}
	return at, !at.IsZero() && sum == total
}

// exclusive is inherit's exclusivity test: every stored output of the order first seen before its expiry, and every
// transaction this scan decided counted (batch), is txid's own or belongs to a transaction in conf.
func (s *Service) exclusive(ctx context.Context, o store.Order, txid string, conf map[string]bool, batch []string) (bool, error) {
	for _, id := range batch {
		if id != txid && !conf[id] {
			return false, nil
		}
	}
	l, err := s.st.TXOsForOrder(ctx, o.ID)
	if err != nil {
		return false, err
	}
	for _, y := range l {
		if y.FirstSeen.Before(o.ExpiresAt) && y.TxID != txid && !conf[y.TxID] {
			return false, nil
		}
	}
	return true, nil
}

// trackAll is step 3: every output that is not settled (the store leaves the settled ones out, in any process) of an
// order inside its watch window, and every on-time one past it (store.WatchedTXOs), refreshed with gettransaction (and
// getmempoolentry), paced at PollRate on the real clock. touched gains the orders of the outputs refreshed; skip gains
// those of the outputs that failed, and of those that cannot be judged while the mempool is not loaded (logLoading,
// once per pass); stale gains only the first. An output that cannot be judged because the wallet is behind the node's
// mempool (errBehind, logWalletBehind once per pass) skips nothing: it is in behindInPass, its order in heldInPass (so
// that settle writes no final event and leftWithoutFinal waits), and the order's other outputs go on. After them it reads the late outputs of the touched orders that are past
// their window (unwatched), so that nothing is recorded or settled for good without a reading of this pass; a failed
// one of those puts its order in stale only. It returns the outputs refreshed in this pass: only their marks were
// verified after this pass's scan.
func (s *Service) trackAll(ctx context.Context, now time.Time, loaded bool, orders map[string]store.Order, touched *orderList,
	skip map[string]bool, stale map[string]struct{}) ([]trackedOutput, error) {
	// Every mark of this step comes after the pass's listsinceblock (step 2), so an output marked dead here may have
	// a replacement that the next pass's scan lists: settle holds the order's final event until then (diedInPass).
	s.diedInPass, s.heldInPass = map[store.TXOKey]struct{}{}, map[string]struct{}{}
	s.behindInPass = map[store.TXOKey]struct{}{}
	watched, err := s.st.WatchedTXOs(ctx, now)
	if err != nil {
		return nil, s.trackFailed(ctx, "watched", err)
	}
	var addrList []string
	for _, x := range watched {
		addrList = append(addrList, x.Address)
	}
	addrs, err := s.st.AddressesByAddress(ctx, addrList)
	if err != nil {
		return nil, s.trackFailed(ctx, "addresses", err)
	}
	list := make([]trackedOutput, 0, len(watched))
	for _, x := range watched {
		o, err := s.order(ctx, orders, addrs[x.Address].OrderID)
		if err != nil {
			return nil, s.trackFailed(ctx, "order", err)
		}
		list = append(list, trackedOutput{x: x, o: o})
	}

	tick := time.NewTicker(time.Duration(float64(time.Second) / s.cfg.PollRate))
	defer tick.Stop()
	var end passEnd
	var refreshed []trackedOutput
	seen := map[store.TXOKey]observed{} // the pass's observations, published at the normal return
	waiting, walletBehind := false, false
	// one refreshes one output. An extra output (a late one past its window, read only because its order is about to
	// be settled: unwatched) that fails puts its order in the stale set and nowhere else: the zero default of boundOut
	// already keeps it unrecorded, and a skip would let a bad late answer block the on-time payment of the same order.
	one := func(t trackedOutput, extra bool) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		obs, observedOK, err := s.track(ctx, t.x, loaded)
		switch {
		case err == nil:
			touched.add(t.o.ID)
			refreshed = append(refreshed, t)
			if observedOK {
				seen[store.TXOKey{TxID: t.x.TxID, Vout: t.x.Vout}] = obs
			}
		case isCrash(err) || ctx.Err() != nil:
			return err
		case errors.As(err, &end):
			s.callFailed(end.method, end.err)
			return err
		case errors.Is(err, errLoading):
			waiting = true
			if !extra {
				skip[t.o.ID] = true
				if t.x.FirstSeen.Before(t.o.ExpiresAt) { // on time, as errBehind: the event comes once the mempool has loaded
					s.heldInPass[t.o.ID] = struct{}{}
				}
			}
		case errors.Is(err, errBehind):
			// Per output: only this output is left unjudged (behindInPass: no refresh, no observation, its mark
			// unchanged, zero in this pass's settle if not recorded); the order's other outputs go on, and its final
			// event waits (heldInPass), as for an output that died after the scan.
			walletBehind = true
			s.behindInPass[store.TXOKey{TxID: t.x.TxID, Vout: t.x.Vout}] = struct{}{}
			if !extra && t.x.FirstSeen.Before(t.o.ExpiresAt) { // on time, as diedNow: a late payment never counts
				s.heldInPass[t.o.ID] = struct{}{}
			}
		default:
			if !extra {
				skip[t.o.ID] = true
			}
			stale[t.o.ID] = struct{}{}
		}
		return nil
	}
	listed := make(map[store.TXOKey]bool, len(list))
	for _, t := range list {
		listed[store.TXOKey{TxID: t.x.TxID, Vout: t.x.Vout}] = true
		if err := one(t, false); err != nil {
			return nil, err
		}
	}
	// Every output a pass records or settles for good was read in the pass: the late outputs of an order past its
	// window that this pass will settle are not watched, so they are read here, before the observations are published.
	extra, err := s.unwatched(ctx, now, orders, touched, skip, listed, stale)
	if err != nil {
		return nil, err
	}
	for _, t := range extra {
		if err := one(t, true); err != nil {
			return nil, err
		}
	}
	if waiting {
		s.log.Warn(logLoading)
	}
	if walletBehind {
		s.log.Warn(logWalletBehind)
	}
	// Published only here, as a map nothing writes afterwards: a pass that ended early settles nothing, and a later
	// settle never reads its observations.
	s.seenMu.Lock()
	s.seen, s.behindSeen = seen, s.behindInPass
	s.seenMu.Unlock()
	return refreshed, nil
}

// unwatched lists the outputs that settle could record in this pass and no read of the pass covers: of the orders in
// touched that are past their watch window (those are the only orders settled past it), the outputs whose current mark
// is mempool or mined, that are not recorded, not foreign and not in listed. They are late outputs: an on-time one is
// watched (store.WatchedTXOs) until it is settled, and a settled one is recorded or dead. A mempool mark is read too:
// the extra read itself can turn a mined mark into a mempool one (its block was reorged out), and the output must be
// read again when it is mined again. An output with no mark (a new row after the window) is not read: such a payment
// is not recorded, by design. It relies on the two other selections of settle's orders,
// store.OnchainOrdersPastExpiry and store.OrdersAwaitingPaidAgain, returning only orders inside their window (a test
// pins it): every output of those is read by the first arm of WatchedTXOs. A store error puts the order in stale and
// goes on; it is not skipped, as an unread output counts zero (boundOut).
func (s *Service) unwatched(ctx context.Context, now time.Time, orders map[string]store.Order, touched *orderList,
	skip map[string]bool, listed map[store.TXOKey]bool, stale map[string]struct{}) ([]trackedOutput, error) {
	var out []trackedOutput
	for _, id := range touched.ids {
		o := orders[id]
		if skip[id] || now.Before(o.ExpiresAt.Add(store.WatchWindow)) {
			continue
		}
		views, err := s.orderViews(ctx, o, false)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			s.log.Error(logTrackFailed, "at", "views")
			stale[id] = struct{}{}
			continue
		}
		for _, v := range views {
			if k := (store.TXOKey{TxID: v.x.TxID, Vout: v.x.Vout}); !listed[k] && v.Marked && v.Kind != store.MarkDead &&
				!v.Recorded && !v.Foreign {
				out = append(out, trackedOutput{x: v.x, o: o})
			}
		}
	}
	return out, nil
}

// errLoading means an output with no confirmation is not in bitcoind's mempool while the mempool is not loaded yet:
// it may be about to be loaded, so it is neither dead nor live for this pass and is skipped.
var errLoading = errors.New("onchain: the mempool is not loaded yet")

// behindGrace is how long an output may stay behind (errBehind) after the pass first found it so: then the node's word
// is taken and it is marked dead, whatever the wallet shows. It trades two things: it must be longer than bitcoind's
// wallet can lag behind its node (a lag of 16 seconds was measured on storage with slow sync writes), or a bump would
// be reported reverted; and it bounds how long a double-spent 0-tier payment can read paid when the wallet never shows
// the conflict (Core's wallet removes a conflict again when the replacement is itself replaced, so a payer who keeps
// replacing could renew the wait). The time is kept in memory (Service.behindSince) from the first find, through any
// mempool or dead mark in between, for as long as the process runs (never pruned: an output settled as dead and
// reopened by a confirmation, then back in the mempool by a reorg, must not obtain it again): the wait is granted once
// per output, so an output that comes back into the mempool and is found replaced again gets no new 60 seconds, only
// what is left of the first ones. Each restart grants it once more. It is checked at each pass that reads the output
// and finds it replaced, so the dead mark comes at the first such pass at or after it; a pass that cannot read it
// (loading, a failed call) does not count.
const behindGrace = 60 * time.Second

// errBehind means an output with no confirmation and no conflict in bitcoind's wallet is not in the node's mempool, and
// the node's mempool holds another transaction that spends its inputs: it was replaced (a fee bump) or double-spent, and
// the wallet has not processed that yet (it learns of mempool changes through a queue and can be seconds behind its
// node). The output is neither dead nor live for this pass: nothing is marked, it is not refreshed, and if it is not
// recorded it counts zero in the pass's settle (behindInPass); when the wallet has caught up, its gettransaction
// shows the conflict and the output is marked dead (and the replacement listed) as usual. Only for behindGrace: after
// that the node's word is taken (track).
var errBehind = errors.New("onchain: the wallet is behind the node's mempool")

// rpcFailure classifies a failed call of step 3: a transport failure that is not the call's own timeout ends the pass
// (passEnd); anything else (a JSON-RPC error such as -5, a bad answer, the call's timeout) skips the output.
func (s *Service) rpcFailure(method string, err error, timedOut bool) error {
	if errors.Is(err, btcrpc.ErrTransport) && !timedOut {
		return passEnd{method, err}
	}
	s.log.Warn(logOutputFailed, "method", method, "code", rpcCode(err))
	return errSkipped
}

// track refreshes one output and adds a mark when its state changed (the sign of confirmations first,
// then mempoolconflicts, then getmempoolentry, because 0 confirmations also means evicted or replaced). Not in the
// mempool while the mempool is not loaded is errLoading, and no mark is added; unconfirmed decides it.
//
// For a mined mark it returns the observation: the confirmations of the gettransaction answer that set the mark (the
// second answer, for an output first seen with none and read again in unconfirmed), with the mark's height
// and block hash, returned whether or not the mark changed. Any other mark has none (ok is false).
func (s *Service) track(ctx context.Context, x store.TXO, loaded bool) (obs observed, ok bool, err error) {
	tx, err := s.readTx(ctx, x)
	if err != nil {
		return observed{}, false, err
	}
	k := store.TXOKey{TxID: x.TxID, Vout: x.Vout}
	cur, have, err := s.st.CurrentMark(ctx, k)
	if err != nil {
		return observed{}, false, s.trackFailed(ctx, "mark-read", errSkipped)
	}
	m := store.Mark{TxID: x.TxID, Vout: x.Vout}
	if !chainMark(tx, &m) {
		if err := s.at("track:before-mempool"); err != nil {
			return observed{}, false, err
		}
		if tx, err = s.unconfirmed(ctx, x, loaded, have && cur.Kind == store.MarkDead, have && cur.Kind == store.MarkMined, &m, tx); err != nil {
			return observed{}, false, err
		}
	}
	if m.Kind == store.MarkMined {
		obs, ok = observed{height: m.Height, hash: m.BlockHash, confirmations: tx.Confirmations}, true
	}
	if have && cur.Kind == m.Kind && cur.Height == m.Height && cur.BlockHash == m.BlockHash {
		return obs, ok, nil
	}
	if err := s.at("track:before-mark-write"); err != nil {
		return observed{}, false, err
	}
	if err := s.st.AddMark(ctx, m); err != nil {
		if !errors.Is(err, store.ErrInvalid) {
			return observed{}, false, s.trackFailed(ctx, "mark-write", errSkipped)
		}
		s.log.Error(logBadTx)
		return observed{}, false, errSkipped
	}
	if _, held := s.heldBefore[k]; m.Kind == store.MarkDead && !held {
		// The first dead mark of the output in this process: the reversal and the final event wait one pass (diedInPass).
		// A later one (the output came back and left again) is not held: the hold is granted once.
		s.diedInPass[k] = struct{}{}
		s.heldBefore[k] = struct{}{}
	}
	return obs, ok, s.at("track:after-mark")
}

// readTx is track's gettransaction for x, with the checks of its answer: the txid, the output found by its vout (once),
// its amount and address unchanged, and a height and a block hash when it has a confirmation. A bad answer is logged
// (logBadTx) and skips the output.
func (s *Service) readTx(ctx context.Context, x store.TXO) (btcrpc.Transaction, error) {
	c, cancel := s.call(ctx)
	tx, err := s.rpc.GetTransaction(c, x.TxID)
	timedOut := errors.Is(c.Err(), context.DeadlineExceeded)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return btcrpc.Transaction{}, ctx.Err()
		}
		return btcrpc.Transaction{}, s.rpcFailure("gettransaction", err, timedOut)
	}
	var out *btcrpc.Output
	n := 0
	for i := range tx.Outputs {
		if tx.Outputs[i].Vout == x.Vout {
			out = &tx.Outputs[i]
			n++
		}
	}
	if tx.TxID != x.TxID || n != 1 || out.ValueSat != x.AmountSat || out.Address != x.Address ||
		(tx.Confirmations > 0 && (tx.BlockHeight < 1 || tx.BlockHash == "")) {
		s.log.Error(logBadTx)
		return btcrpc.Transaction{}, errSkipped
	}
	return tx, nil
}

// chainMark sets m from a gettransaction answer alone: dead for a negative count (conflicted by a block), mined at its
// height and hash for a positive one, dead for a replacement in the mempool (mempoolconflicts). It reports false for
// an answer with no confirmation and no conflict: only the mempool can tell that one.
func chainMark(tx btcrpc.Transaction, m *store.Mark) bool {
	switch {
	case tx.Confirmations < 0:
		m.Kind = store.MarkDead
	case tx.Confirmations > 0:
		m.Kind, m.Height, m.BlockHash = store.MarkMined, tx.BlockHeight, tx.BlockHash
	case len(tx.MempoolConflicts) > 0:
		m.Kind = store.MarkDead
	default:
		return false
	}
	return true
}

// unconfirmed sets m for an output whose gettransaction showed no confirmation and no conflict: mempool when
// getmempoolentry finds it. Not found, it is errLoading while the pass's loaded is false, and dead at once when its
// current mark is already dead (wasDead). Otherwise, before a dead mark, every read is made again, in this order (plan
// 6.1):
//   - getmempoolinfo: bitcoind may have restarted after the pass's read (loaded false again; then
//     getmempoolentry answers -5 for a transaction it is about to load, *source*: Core 32.1 src/rpc/mempool.cpp:1113
//     and :1143, "loaded" is GetLoadTried). Loading is errLoading.
//   - getmempoolentry: the mempool may have finished loading between the first getmempoolentry and getmempoolinfo.
//   - gettransaction, after the last getmempoolentry: a block between the first gettransaction and that call took the
//     transaction out of the mempool. Then the second gettransaction sees it mined (*source*: Core 32.1
//     src/validation.cpp:3019-3020, 3082 and 3086: Chainstate::ConnectTip holds cs_main and the mempool's lock while it
//     removes the block's transactions from the mempool and sets the new tip, and src/rpc/mempool.cpp:951:
//     getmempoolentry reads the mempool under that lock, so one that misses the transaction runs after the tip moved;
//     src/wallet/rpc/transactions.cpp:755 and src/wallet/wallet.cpp:1651:
//     gettransaction first waits for the wallet to process the notifications up to the current tip). The mark is mined
//     at its height; a dead mark would reverse a paid order on a 0 tier. A negative count or a conflict is dead.
//   - gettxspendingprevout (the node's view), when that gettransaction still shows no confirmation and no conflict and
//     the transaction has inputs: bitcoind's wallet learns of mempool changes through a queue and can be seconds
//     behind its node, so after a fee bump the node has dropped the original and holds the replacement while the
//     wallet shows neither a conflict nor the replacement. Another mempool transaction that spends the inputs is
//     errBehind (nothing is marked, until behindGrace has passed since the output was first found so: the wallet will
//     show the conflict by then, or the node's word is taken and the output is dead); the transaction itself is mempool
//     (it came back between the calls); none is dead (evicted or expired). A failed call never marks dead. It is not asked when
//     the output's current mark is mined (a reorg took its block): the wallet has just said it is not mined, and a
//     record that needs a block is reversed as before, not held for the wallet's lag.
//
// It returns the answer that set m: first, or the second gettransaction when it made one (whose count is the one a
// mined mark's observation carries, track).
//
// Only then is it dead: the move into dead costs one more call of each method (getmempoolinfo, getmempoolentry,
// gettransaction) than a live output, and one gettxspendingprevout. The re-read guards only that move. An output already dead was reversed or never
// recorded, so a block that mines it later gives its mined mark on the next pass (and RecordScan reopens a settled
// one); it costs what a live output costs.
func (s *Service) unconfirmed(ctx context.Context, x store.TXO, loaded, wasDead, wasMined bool, m *store.Mark,
	first btcrpc.Transaction) (btcrpc.Transaction, error) {
	in, err := s.inMempool(ctx, x.TxID)
	if err != nil {
		return first, err
	}
	if !in && !loaded {
		return first, errLoading
	}
	if !in && wasDead {
		m.Kind = store.MarkDead
		return first, nil
	}
	if !in {
		c, cancel := s.call(ctx)
		loaded, err = s.rpc.MempoolLoaded(c)
		timedOut := errors.Is(c.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return first, ctx.Err()
			}
			return first, s.rpcFailure("getmempoolinfo", err, timedOut)
		}
		if !loaded {
			return first, errLoading
		}
		if in, err = s.inMempool(ctx, x.TxID); err != nil {
			return first, err
		}
	}
	if in {
		m.Kind = store.MarkMempool
		return first, nil
	}
	tx, err := s.readTx(ctx, x)
	if err != nil {
		return first, err
	}
	if chainMark(tx, m) {
		return tx, nil
	}
	// Dead only when no mempool transaction spends the inputs: the node says the transaction is gone, and the wallet
	// neither shows a conflict nor a confirmation, which is also what a wallet that is behind its node shows after a
	// replacement. A transaction with no input to ask about (only a coinbase) has nothing to ask.
	// An output whose current mark is mined (its block was reorged out) is not asked: the wallet, synced to the tip, has
	// just said it is not mined, and a record that needs a block must not wait for the wallet's lag (wasMined).
	if len(tx.Inputs) > 0 && !wasMined {
		spenders, err := s.spentInMempool(ctx, tx.Inputs)
		if err != nil {
			return first, err
		}
		if slices.ContainsFunc(spenders, func(id string) bool { return id != "" && id != x.TxID }) {
			k := store.TXOKey{TxID: x.TxID, Vout: x.Vout}
			now := s.now()
			since, ok := s.behindSince[k]
			if !ok {
				since = now
				s.behindSince[k] = since // set once; a later replacement does not reset it
			}
			if now.Sub(since) < behindGrace {
				return first, errBehind
			}
			// The grace is over: the node's word is taken and the output is marked dead below, whatever the wallet shows.
		}
		if slices.Contains(spenders, x.TxID) { // back in the mempool between the two calls
			m.Kind = store.MarkMempool
			return tx, nil
		}
	}
	m.Kind = store.MarkDead
	return tx, nil
}

// spentInMempool is gettxspendingprevout for track, with step 3's classification of a failure (rpcFailure).
func (s *Service) spentInMempool(ctx context.Context, outs []btcrpc.Outpoint) ([]string, error) {
	c, cancel := s.call(ctx)
	l, err := s.rpc.SpentInMempool(c, outs)
	timedOut := errors.Is(c.Err(), context.DeadlineExceeded)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, s.rpcFailure("gettxspendingprevout", err, timedOut)
	}
	return l, nil
}

// inMempool is getmempoolentry for track, with step 3's classification of a failure (rpcFailure).
func (s *Service) inMempool(ctx context.Context, txid string) (bool, error) {
	c, cancel := s.call(ctx)
	in, err := s.rpc.InMempool(c, txid)
	timedOut := errors.Is(c.Err(), context.DeadlineExceeded)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, s.rpcFailure("getmempoolentry", err, timedOut)
	}
	return in, nil
}

// settleDone records the outputs settled for good in this pass (store.AddSettled): the store leaves them out of
// WatchedTXOs from then on, in any process, so a restart polls only the unsettled outputs (Ruling G: a reorg of
// SettledDepth blocks or more is not detected). Only the outputs refreshed in this pass count, and only when their
// order settled without an error: then the mark was read from bitcoind after this pass's scan stored the cursor, with
// bitcoind's own count of its confirmations (the observation settled counts with), and the order's records and events
// are complete. An output whose call failed, or that the pass did not reach, keeps an older mark and is polled
// again. A store error ends the pass (it is not stamped good).
func (s *Service) settleDone(ctx context.Context, now time.Time, refreshed []trackedOutput, settledOK map[string]bool) error {
	cur, ok, err := s.st.Cursor(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.log.Error(logSettleFailed, "at", "cursor")
		return err
	}
	if !ok || cur.Height < 1 { // a settled fact needs a height of at least 1 (only a chain at its genesis has none)
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
		k := store.TXOKey{TxID: t.x.TxID, Vout: t.x.Vout}
		// A paid order (on either rail) needs no person for a dead output: the ALERT is not logged. Nor is it for a
		// late output (read only because its order was settled, unwatched): it was never an on-time payment. It is asked
		// before the write: once settled the output is never read again, so a stop between the write and the ALERT
		// would lose it; a stop before the write settles nothing and the next pass asks again.
		leftPool := dead && !paid && t.x.FirstSeen.Before(t.o.ExpiresAt) && !s.carriedOn(ctx, t)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		added, err := s.st.AddSettled(ctx, k, cur.Height)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Error(logSettleFailed, "order", t.o.ID, "at", "add-settled")
			return err
		}
		if added && leftPool {
			s.log.Error(logLeftPool, "order", t.o.ID)
		}
		if err := s.at("settle:after-settled"); err != nil {
			return err
		}
	}
	return nil
}

// carriedOn reports whether a dead output's payment is carried on by a bump that inherited its first-seen (inherit):
// another stored output of its order to the same address, with the same first-seen, that is live (its current mark is
// not dead) and still watched. Then its settle past the window logs no logLeftPool. The link is not stored, so this
// matches on the rows: two unrelated on-time payments first seen in the same second, one dying while the other is
// live, also read carried on. A store error reads false (the ALERT is logged). A bump whose time was clamped to now
// (the clock stepped back) has another first-seen, and the ALERT is logged for its original.
//
// "Still watched" is read as "not settled" (store.SettledKeys), with the candidates' marks in one batch: settled's
// dead branch runs only past the order's window, where t.x is watched by WatchedTXOs's stuck arm alone (first seen
// before the expiry), and a candidate with the same first-seen is in that arm too, so it is watched exactly when it is
// not settled (no WatchedTXOs query per dead output).
func (s *Service) carriedOn(ctx context.Context, t trackedOutput) bool {
	l, err := s.st.TXOsForOrder(ctx, t.o.ID)
	if err != nil {
		return false
	}
	var keys []store.TXOKey
	for _, y := range l {
		if y.TxID != t.x.TxID && y.Address == t.x.Address && y.FirstSeen.Equal(t.x.FirstSeen) {
			keys = append(keys, store.TXOKey{TxID: y.TxID, Vout: y.Vout})
		}
	}
	if len(keys) == 0 {
		return false
	}
	settled, err := s.st.SettledKeys(ctx, keys)
	if err != nil {
		return false
	}
	marks, err := s.st.CurrentMarks(ctx, keys)
	if err != nil {
		return false
	}
	for _, k := range keys {
		if m, marked := marks[k]; !settled[k] && (!marked || m.Kind != store.MarkDead) {
			return true
		}
	}
	return false
}

// settled is the rule of a settled output, with no RPC call (dead reports the second branch): either its current mark
// is mined with at least SettledDepth confirmations (the least of cur.Height - height + 1 and the count bitcoind
// answered with the mark in this pass; no such answer, not settled) and its forward is recorded without a reversal, or
// its order's watch window has closed at now, its order has its final event, its current mark is dead and it has no
// standing record (never recorded, or every forward reversed: its net in the ledger is 0). The second leaves a dead
// output in the stuck set (store.WatchedTXOs) until a pass has reversed it and its order is final, so a crash between
// its dead mark and its reversal is healed. Once settled, it is not polled; if a later scan lists it mined
// (rebroadcast), RecordScan reopens it. The bound keeps the first sound when the tip falls inside a pass; settleDone
// passes only outputs refreshed in this pass. paid reports, for the second branch, that the order reads paid by its
// events: order.paid, and no order.reverted that no order.paid_again answers.
func (s *Service) settled(ctx context.Context, now time.Time, cur store.Cursor, t trackedOutput) (ok, dead, paid bool) {
	m, marked, err := s.st.CurrentMark(ctx, store.TXOKey{TxID: t.x.TxID, Vout: t.x.Vout})
	if err != nil || !marked {
		return false, false, false
	}
	// The mined arm counts as settle does (boundOut, on the mark just loaded, as unrecorded): at most bitcoind's count
	// from this pass's answer. A mined output with no observation of this pass counts zero and is not settled for good.
	k := store.TXOKey{TxID: t.x.TxID, Vout: t.x.Vout}
	depth := rail.Out{Marked: true, Kind: m.Kind, Height: m.Height}
	obs, have := s.seenNow()[k]
	boundOut(&depth, obs, have, m.BlockHash)
	switch {
	case depth.Mined() && depth.Bounded && depth.Confirmations(cur.Height) >= int64(SettledDepth(s.cfg.Tiers)):
		v, err := s.outputView(ctx, t.o, t.x, false)
		return err == nil && v.Recorded, false, false
	case m.Kind == store.MarkDead && !now.Before(t.o.ExpiresAt.Add(store.WatchWindow)):
		v, err := s.outputView(ctx, t.o, t.x, false)
		if err != nil || v.Recorded || v.Foreign {
			return false, false, false
		}
		ev, err := s.readEvents(ctx, t.o.ID)
		if err != nil {
			// Safe (the output is polled again), but a store that keeps failing must show.
			if ctx.Err() == nil {
				s.log.Error(logSettleFailed, "order", t.o.ID, "at", "events")
			}
			return false, false, false
		}
		return ev.Final, true, ev.Paid && !ev.ReversedNow()
	}
	return false, false, false
}

// seenNow is the observations of the last pass whose step 3 completed. The map is published whole and never written
// afterwards, so it is read without the lock; seenMu is held only to copy the pointer.
func (s *Service) seenNow() map[store.TXOKey]observed {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	return s.seen
}

// heldNow is the orders whose final event this rail held in the last pass that reached its settles (heldInPass copied
// at their end); read by the peer's View, never written after the publish.
func (s *Service) heldNow() map[string]struct{} {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	return s.heldSeen
}

// behindNow is the outputs that were behind in the last pass whose step 3 completed (errBehind), published with seen:
// the map is never written afterwards, so it is read without the lock. The peer's View reads it; behindInPass is the
// pass's alone.
func (s *Service) behindNow() map[store.TXOKey]struct{} {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	return s.behindSeen
}

// boundOut is the confirmation bound, applied where an order's outputs are loaded (orderViews) and in settled. A mined
// v with an observation of its mark (o, have: the same height and the same block hash, hash being the mark's) counts at
// most the observation (rail.Out.Confirmations). Without one, an unrecorded v counts zero (it has not been read by the
// pass that decides, so it is not recorded on a tier that needs a confirmation; a 0 tier records from any mined mark),
// and a recorded v is left unbounded (it only reverts; its count decides nothing, and a settled payment's shown count
// goes on growing). A v that is not mined is left as it is.
func boundOut(v *rail.Out, o observed, have bool, hash string) {
	if !v.Mined() {
		return
	}
	switch {
	case have && o.height == v.Height && o.hash == hash:
		v.Bounded, v.Observed = true, o.confirmations
	case !v.Recorded:
		v.Bounded, v.Observed = true, 0
	}
}
