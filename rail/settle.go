package rail

import (
	"context"
	"slices"
	"time"

	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/store"
)

// Lightning is the part of lnpay a rail uses: whether an order already reads paid there, and MarkKnown, which tells
// lnpay's limits that an account paid an order on the rail (after the account_paid fact is written).
type Lightning interface {
	Status(ctx context.Context, orderID string) (lnpay.Status, error)
	MarkKnown(account string)
}

var _ Lightning = (*lnpay.Service)(nil)

// maxSeq bounds the seq a reversal may carry: a larger UserData64 is not one of SatBooth's (step 1 ignores it).
const maxSeq = 1 << 20

// RevertedSeqs is step 1's heal: the seqs that the payments' reversals carry, each in 1 to 2^20, sorted, once each.
// Every one of them has its order.reverted, so the count of those events is complete before step 4 derives the next
// seq. It reads every payment, a foreign one included (its reversals were read before its mismatch).
func RevertedSeqs(outs []Out) []int {
	var seqs []int
	for _, o := range outs {
		for _, k := range o.Seqs {
			if k > 0 && k <= maxSeq {
				seqs = append(seqs, int(k))
			}
		}
	}
	slices.Sort(seqs)
	return slices.Compact(seqs)
}

// HealSeqs is step 1's whole heal list (ruling R2): this rail's seqs (RevertedSeqs over its outputs) and the peer
// rail's (Peer.RevertedSeqs), each in 1 to 2^20, sorted, once each. The order.reverted events are the order's, not a
// rail's: a reversal stamped on the peer whose event a crash or a store error lost is healed here too, before this
// rail derives the next seq from the count of those events.
func HealSeqs(own, peer []int) []int {
	var seqs []int
	for _, k := range slices.Concat(own, peer) {
		if k > 0 && k <= maxSeq {
			seqs = append(seqs, k)
		}
	}
	slices.Sort(seqs)
	return slices.Compact(seqs)
}

// IsPaid is the paid rule on the combined state (Ruling K): Lightning reads paid, or the confirmed sum reaches the
// amount. Step 2 reads it before the call's records and reversals, step 4 after its reversals, step 5 at the end.
func IsPaid(lightningPaid bool, confirmed, amount int64) bool {
	return lightningPaid || confirmed >= amount
}

// ReversalSeq is the seq of the order.reverted that this call's reversals give: the next one (reverted + 1) when they
// turn an order paid before them (before) unpaid after them (!after), else 0: a reversal that leaves the order paid
// gives no event.
func ReversalSeq(before, after bool, reverted int) int {
	if before && !after {
		return reverted + 1
	}
	return 0
}

// Stamps are the UserData64 of the reversals at idx (indexes into outs, in order): the reversal that takes the
// surviving sum below the amount carries k, the others 0. The surviving sum after the j-th reversal is the sum, at
// most limit, of the confirmed records (recorded, counted, CodePaid) that are not among the first j+1 reversals; it is
// never subtracted from a capped sum, so money above limit is not lost (ruling 8-A5). So a crash between two
// reversals leaves the ledger saying exactly what the uninterrupted run says: a first reversal that still left the
// order paid carries 0 (and the next pass numbers the one that flips it), and a first reversal that flipped it carries
// k (and the next pass reads the order unpaid before the second). With k 0 every stamp is 0. An index given twice is
// gone from its first place on; one that names no output removes nothing. Linear in len(outs) + len(idx) (ruling R7).
func Stamps(outs []Out, idx []int, amount, limit int64, k int) []uint64 {
	stamps := make([]uint64, len(idx))
	if k <= 0 {
		return stamps
	}
	for j, sum := range survivingSums(outs, idx, limit) {
		if sum < amount {
			stamps[j] = uint64(k)
			break
		}
	}
	return stamps
}

// confirmedOut is whether an output counts in the confirmed sum (rail.Sums' rule).
func confirmedOut(o Out) bool { return o.Recorded && o.Counted && o.Code == CodePaid }

// survivingSums are the surviving sums after each reversal at idx: sums[j] is the confirmed sum of outs, at most
// limit, without the outputs at idx[:j+1]. They are built backwards (ruling R7): the final survivors' sum first, then
// each reversal's output added back at the place it first appears, from the last reversal to the first. Add is
// min(a+b, limit), so adding back is exact for amounts of 0 or more, where subtracting from a capped sum is not.
func survivingSums(outs []Out, idx []int, limit int64) []int64 {
	first := make([]int, len(outs)) // 1 + the place in idx where each output is first reversed; 0 for none
	for j, i := range idx {
		if i >= 0 && i < len(outs) && first[i] == 0 {
			first[i] = j + 1
		}
	}
	var sum int64
	for i, o := range outs {
		if first[i] == 0 && confirmedOut(o) {
			sum = Add(sum, o.Amount, limit)
		}
	}
	sums := make([]int64, len(idx))
	for j := len(idx) - 1; j >= 0; j-- {
		sums[j] = sum
		if i := idx[j]; i >= 0 && i < len(outs) && first[i] == j+1 && confirmedOut(outs[i]) {
			sum = Add(sum, outs[i].Amount, limit)
		}
	}
	return sums
}

// survivingSum is the confirmed sum of outs, at most limit, without the outputs at the indexes gone.
func survivingSum(outs []Out, gone []int, limit int64) int64 {
	if len(gone) == 0 {
		sum, _, _ := Sums(outs, limit)
		return sum
	}
	sums := survivingSums(outs, gone, limit)
	return sums[len(sums)-1]
}

// PlanInput is what PlanReversals needs of an order beside its payments.
type PlanInput struct {
	TierNeed int64 // the order's tier's confirmations (Required)
	Amount   int64
	Limit    int64 // the rail's bound on a sum (store.MaxSat, store.MaxPiconero)
	Before   bool  // the order read paid before this call (IsPaid at step 2)
	// LightningPaid is whether Lightning reads paid.
	LightningPaid bool
	Reverted      int // the order's count of order.reverted (after step 1's heal)
	// Held reports whether the payment at index i is held on a 0 tier: not reversed in this call when the reversals
	// would turn the paid order unpaid. Which payments are held is the rail's own rule (on-chain: an output that the
	// current pass marked dead after its scan, whose replacement may not be listed yet). nil holds none.
	Held func(i int) bool
}

// ReversalPlan is step 4's plan: the payments to reverse, as indexes into the outs given, in their order; each
// reversal's UserData64 (Stamps); and K, the seq of the order.reverted they give (ReversalSeq), 0 for none.
type ReversalPlan struct {
	Idx    []int
	Stamps []uint64
	K      int
}

// PlanReversals is step 4's rule: each recorded payment that reverts (Out.Reverts) is reversed, in order. When that
// would turn the paid order unpaid on a 0 tier, the payments Held reports are left out and the plan is made again (a
// hold that leaves the order paid gives no event; a record that needs a block is never held). K and the stamps follow
// from the plan that stands. outs must already carry step 3's records. The sum after the plan is the surviving
// records' (survivingSum), not the confirmed sum less the reversals: a sum capped at Limit has lost the money above it
// (ruling 8-A5).
func PlanReversals(outs []Out, in PlanInput) ReversalPlan {
	plan := func(hold bool) []int {
		var idx []int
		for i, o := range outs {
			if o.Reverts(in.TierNeed) && !(hold && in.Held(i)) {
				idx = append(idx, i)
			}
		}
		return idx
	}
	idx := plan(false)
	after := IsPaid(in.LightningPaid, survivingSum(outs, idx, in.Limit), in.Amount)
	if in.Before && !after && in.TierNeed == 0 && in.Held != nil {
		idx = plan(true)
		after = IsPaid(in.LightningPaid, survivingSum(outs, idx, in.Limit), in.Amount)
	}
	k := ReversalSeq(in.Before, after, in.Reverted)
	return ReversalPlan{Idx: idx, Stamps: Stamps(outs, idx, in.Amount, in.Limit, k), K: k}
}

// LightningDone reports whether an order's Lightning side can no longer pay it: the order has no Lightning amount, or
// lnpay reads it expired (every invoice closed, past the expiry). settle adds order.expired or order.underpaid only
// then. Rail reads those states from the events themselves, so the API never shows a final state the events lack;
// status keeps the same LightningDone gate on top, which also covers a reversal's order.reverted, the one backing of
// Rail's unpaid states that settle writes whatever Lightning's state.
func LightningDone(o store.Order, ln lnpay.Status) bool {
	return o.AmountMsat == 0 || ln.State == lnpay.StateExpired
}

// FinalInput is what Final needs of an order on one rail.
type FinalInput struct {
	Now, ExpiresAt      time.Time
	CountedLive, Amount int64
	HasFinal            bool // order.paid, order.expired or order.underpaid exists
	LightningDone       bool
	PeerPending         bool // a peer rail's counted live sum reaches its amount and it is not yet paid
	PeerReceived        bool // a peer rail has a counted live sum above 0
	// PeerStale is a peer rail whose view is stale (failing or paused) and not paid: it holds the final event as a
	// pending peer does, as long as it stays stale (rulings 8-T7d and 8-T7e). An order still held at its window's end
	// is reported by the rail's left-the-window ALERT; a settle after that writes the event once the peer is fresh.
	PeerStale bool
}

// Final is the rule of the final event, past the expiry and once Lightning is done (and no peer rail is still being
// paid, nor stale): below the amount, order.expired (nothing live on any rail) or order.underpaid; at the window end, an on-time
// payment that is live but not yet confirmed enough to be recorded is order.underpaid too. Its output stays tracked
// (store.WatchedTXOs), so a later confirmation still records it (Ruling S: order.paid after the final event). due says
// whether the event is written now, typ which one, and window whether the counted live sum reaches the amount (the
// window-end case, which the rail logs with its own line).
func Final(in FinalInput) (typ string, window, due bool) {
	atWindowEnd := !in.Now.Before(in.ExpiresAt.Add(store.WatchWindow)) && in.CountedLive > 0
	due = !in.Now.Before(in.ExpiresAt) && !in.HasFinal && in.LightningDone && !in.PeerPending && !in.PeerStale &&
		(in.CountedLive < in.Amount || atWindowEnd)
	typ = store.EventExpired
	if in.CountedLive > 0 || in.PeerReceived {
		typ = store.EventUnderpaid
	}
	return typ, in.CountedLive >= in.Amount, due
}
