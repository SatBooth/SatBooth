package rail

import "errors"

// MaxConfirmations is the largest confirmation count a tier may ask for.
const MaxConfirmations = 100

// Tier says how many confirmations an order of up to UpTo (in the rail's base unit) needs (0: no upper bound, the last
// tier only). Confirmations is 0 to 100 and never decreases from one tier to the next; 0 pays an order from the
// mempool.
type Tier struct {
	UpTo          int64
	Confirmations int
}

// ValidTiers checks a list of tiers: not empty, each count 0 to 100 and none below the previous one, each UpTo 1 to
// maxAmount and strictly increasing, and the last one's UpTo 0. The texts name the fields; a rail maps them to its
// own.
func ValidTiers(t []Tier, maxAmount int64) error {
	if len(t) == 0 {
		return errors.New("rail: Tiers is empty")
	}
	for i, x := range t {
		last := i == len(t)-1
		switch {
		case x.Confirmations < 0 || x.Confirmations > MaxConfirmations:
			return errors.New("rail: a tier's Confirmations must be 0 to 100")
		case i > 0 && x.Confirmations < t[i-1].Confirmations:
			return errors.New("rail: tiers' Confirmations must not decrease")
		case last && x.UpTo != 0:
			return errors.New("rail: the last tier's UpTo must be 0 (no upper bound)")
		case !last && (x.UpTo < 1 || x.UpTo > maxAmount):
			return errors.New("rail: a tier's UpTo must be 1 to the maximum amount, 0 only for the last")
		case !last && i > 0 && x.UpTo <= t[i-1].UpTo:
			return errors.New("rail: tiers' UpTo must be strictly increasing")
		}
	}
	return nil
}

// Required is the confirmations an order of amount needs: those of the first tier whose UpTo is 0 or at least the
// amount. It may be 0: then a counted output is paid as soon as it is in the mempool (a late output still needs a
// block, Ruling 6.1-10; Out.Need). With no tiers it is the largest allowed count, so nothing settles on bad input.
func Required(tiers []Tier, amount int64) int {
	for _, t := range tiers {
		if t.UpTo == 0 || t.UpTo >= amount {
			return t.Confirmations
		}
	}
	return MaxConfirmations
}

// SettledDepth is the last tier's confirmations, at least 1 (Ruling 6.1-4): the depth at which an output is final. A
// record made from the mempool on a 0 tier is therefore polled until it is mined at that depth.
func SettledDepth(tiers []Tier) int {
	if len(tiers) == 0 {
		return MaxConfirmations
	}
	return max(tiers[len(tiers)-1].Confirmations, 1)
}
