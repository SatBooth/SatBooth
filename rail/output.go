package rail

import (
	"slices"
	"time"

	"github.com/satbooth/satbooth/store"
)

// The transfer codes both rails use.
const (
	CodePaid     uint16 = 2 // a counted payment
	CodeReversed uint16 = 3 // a reversal
	CodeLate     uint16 = 4 // a late payment
)

// Out is what the store and the ledger say about one payment of an order, in the rail's base unit.
//
// The fields describe the payment as loaded at the start of a settle call. Within the call, a settle that records a
// payment sets Recorded, Code and Timestamp, and one that reverses a record clears Recorded, then sets Reversed once
// its batch is written (ruling T3: LateRecorded reads it in the same call). Code, Timestamp and Seqs still describe the
// record as loaded, so no rule may read them for a reversal made in the same call (every rule here gates Code on
// Recorded).
type Out struct {
	Amount    int64
	Counted   bool // on time (and, for Monero, not locked beyond the normal lock); otherwise late
	Marked    bool
	Kind      string   // store.MarkMempool, MarkMined or MarkDead when Marked
	Height    int64    // when Kind is MarkMined
	Recorded  bool     // the forward of the current epoch exists and has no reversal
	Code      uint16   // the recorded forward's code (CodePaid or CodeLate)
	Timestamp uint64   // the recorded forward's ledger timestamp (for PaidAt)
	Reversed  bool     // at least one reversal exists for the payment (a reversed late forward still counts as recorded-late)
	Foreign   bool     // a transfer under one of its ids does not match it: it counts for nothing
	Seqs      []uint64 // the UserData64 of the payment's reversals, in epoch order (RevertedSeqs); Reversed is len > 0
	// Bounded makes Confirmations at most Observed: the Monero rail's mined outputs carry wallet-rpc's own count from
	// the answer that gave the mark (ruling 8-A6), and the on-chain rail's carry bitcoind's own count from the pass's
	// gettransaction that set or confirmed the mark, so a count from the stored tip or wallet height never exceeds what
	// the node said with this observation.
	Bounded  bool
	Observed int64
}

// Live is the one definition of a live output, for settle and Rail: no mark yet, mempool or mined. Only a dead mark
// makes it dead.
func (o Out) Live() bool { return !o.Marked || o.Kind != store.MarkDead }

// Mined reports whether the output's current mark is mined.
func (o Out) Mined() bool { return o.Marked && o.Kind == store.MarkMined }

// Confirmations is tip - height + 1 for a mined output, never below 0, and at most Observed (never below 0) when
// Bounded (ruling 8-A6).
func (o Out) Confirmations(tip int64) int64 {
	if !o.Mined() {
		return 0
	}
	c := max(tip-o.Height+1, 0)
	if o.Bounded {
		c = min(c, max(o.Observed, 0))
	}
	return c
}

// Need is the confirmations the output needs to be recorded: its order's tier's, and at least 1 for a late output
// (Ruling 6.1-10: a late output is never recorded from the mempool, so order.late waits for a block on every tier,
// Ruling Z2).
func (o Out) Need(tierNeed int64) int64 {
	if !o.Counted {
		return max(tierNeed, 1)
	}
	return tierNeed
}

// Recordable is step 3's rule: the output is mined at its need, or its need is 0 and it is in the mempool. An output
// without a mark yet is not recordable on any tier.
func (o Out) Recordable(tierNeed, tip int64) bool {
	n := o.Need(tierNeed)
	return (o.Mined() && o.Confirmations(tip) >= n) || (n == 0 && o.Marked && o.Kind == store.MarkMempool)
}

// Reverts is step 4's rule for a recorded output: it is dead, or it needs a confirmation and is no longer mined (Ruling
// Q). A 0-conf record back in the mempool after a reorg is still counted, so it stays recorded.
func (o Out) Reverts(tierNeed int64) bool {
	return o.Recorded && ((o.Marked && o.Kind == store.MarkDead) || (o.Need(tierNeed) > 0 && !o.Mined()))
}

// Add is a + b, at most limit.
func Add(a, b, limit int64) int64 { return min(a+b, limit) }

// Sums are the order's sums over its outputs, each at most limit: the confirmed sum (recorded counted outputs, whose
// forward has CodePaid and no reversal), the counted live sum and the live late sum. The live sums follow the marks
// only: an output whose ledger record has other details counts for nothing confirmed, but it still holds the order as
// a payment seen.
func Sums(outs []Out, limit int64) (confirmed, countedLive, lateLive int64) {
	for _, o := range outs {
		if o.Recorded && o.Counted && o.Code == CodePaid {
			confirmed = Add(confirmed, o.Amount, limit)
		}
		if o.Live() {
			if o.Counted {
				countedLive = Add(countedLive, o.Amount, limit)
			} else {
				lateLive = Add(lateLive, o.Amount, limit)
			}
		}
	}
	return
}

// Least is the least confirmation count of the counted live outputs at tip, at most 2^30. ok is false when one of them
// is in the mempool or not yet marked, or none is counted and live.
func Least(outs []Out, tip int64) (confirmations int, ok bool) {
	least, counted, unconfirmed := int64(0), false, false
	for _, o := range outs {
		if !o.Live() || !o.Counted {
			continue
		}
		if !o.Mined() {
			unconfirmed = true
			continue
		}
		if c := o.Confirmations(tip); !counted || c < least {
			least = c
		}
		counted = true
	}
	if counted && !unconfirmed {
		return int(min(least, 1<<30)), true
	}
	return 0, false
}

// BothRecorded is the rail's half of the both-rails rule, the same for every call (settle and LightningPaid): the
// order has a recorded payment on the rail, an output whose last forward is its own (not foreign), has no reversal,
// and has CodePaid (counted) or CodeLate (late), whatever its amount. A person refunds one rail, so a partial or late
// payment on top of a Lightning payment is alerted too.
func BothRecorded(outs []Out) bool {
	for _, o := range outs {
		if o.Recorded && !o.Foreign && (o.Code == CodePaid || o.Code == CodeLate) {
			return true
		}
	}
	return false
}

// LateRecorded reports whether a late output's forward (CodeLate) was ever recorded: it is recorded now, or it was and
// a reorg reversed it. Ruling Z2: order.late and its ALERT wait for that, the same rule as a counted payment (mined at
// its tier), so a late output that is replaced or double spent before it confirms gives no event; the rail reads late
// as soon as it is seen (Rail). A reversed one still counts, so a crash between the record and the event is healed
// even if a reorg follows.
func LateRecorded(outs []Out) bool {
	for _, o := range outs {
		if !o.Counted && !o.Foreign && (o.Recorded || o.Reversed) {
			return true
		}
	}
	return false
}

// PaidAt is the timestamp of the forward that first made the confirmed sum reach the amount, or zero. The running sum
// is at most limit.
func PaidAt(outs []Out, amount, limit int64) time.Time {
	var fs []Out
	for _, o := range outs {
		if o.Recorded && o.Counted && o.Code == CodePaid {
			fs = append(fs, o)
		}
	}
	slices.SortFunc(fs, func(a, b Out) int {
		switch {
		case a.Timestamp < b.Timestamp:
			return -1
		case a.Timestamp > b.Timestamp:
			return 1
		}
		return 0
	})
	var sum int64
	for _, f := range fs {
		if sum = Add(sum, f.Amount, limit); sum >= amount {
			return time.Unix(0, int64(f.Timestamp)).UTC()
		}
	}
	return time.Time{}
}

// Keep is the sum, at most limit, of the records step 4 keeps at tierNeed: recorded, counted, CodePaid and not reverting.
// Settle's step 3 checks an overpayment against it, so a 0-conf fee bump that keeps the output records the new output
// in the same call that reverses the old one, and that is no overpayment.
func Keep(outs []Out, tierNeed, limit int64) int64 {
	var keep int64
	for _, o := range outs {
		if o.Recorded && o.Counted && o.Code == CodePaid && !o.Reverts(tierNeed) {
			keep = Add(keep, o.Amount, limit)
		}
	}
	return keep
}

// Overpays is step 3's overpaid check for a record just made of o: o is counted and, added to keep (Keep, plus the
// records made before it in the call), takes the sum above amount. The sum is capped at limit first, as every sum.
func Overpays(o Out, keep, amount, limit int64) bool {
	return o.Counted && Add(keep, o.Amount, limit) > amount
}
