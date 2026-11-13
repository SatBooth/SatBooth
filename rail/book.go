package rail

import (
	"context"

	"github.com/satbooth/satbooth/ledger"
)

// MaxEpochs is a safety stop for Walk: a payment with this many forwards and reversals counts for nothing.
const MaxEpochs = 1 << 16

// Book is a rail's two ledger accounts in its ledger: Wallet (the money as it sits in the merchant's wallet) and
// Merchant (what SatBooth owes the merchants). A forward debits Wallet and credits Merchant; a reversal does the
// opposite. Each payment's transfers are its forwards and reversals, one pair per epoch, under ids the rail derives.
type Book struct {
	Ledger           uint32
	Wallet, Merchant ledger.U128
}

// code is a forward's code: CodePaid for a counted payment, CodeLate for a late one.
func code(counted bool) uint16 {
	if counted {
		return CodePaid
	}
	return CodeLate
}

// Forward is the forward transfer of a payment of amount (at least 1) for the order whose UserData128 is ud.
func (b Book) Forward(id, ud ledger.U128, amount int64, counted bool) ledger.Transfer {
	return ledger.Transfer{ID: id, DebitAccountID: b.Wallet, CreditAccountID: b.Merchant, Amount: ledger.U128From64(uint64(amount)),
		UserData128: ud, Ledger: b.Ledger, Code: code(counted)}
}

// Reversal is the reversal of a forward of amount; seq is its UserData64 (Stamps).
func (b Book) Reversal(id, ud ledger.U128, amount int64, seq uint64) ledger.Transfer {
	return ledger.Transfer{ID: id, DebitAccountID: b.Merchant, CreditAccountID: b.Wallet, Amount: ledger.U128From64(uint64(amount)),
		UserData128: ud, UserData64: seq, Ledger: b.Ledger, Code: CodeReversed}
}

// ForwardOK is the check of a stored forward against its payment: the ledger, the accounts, the amount, the order,
// and the code that the payment's classification gives. A late payment's forward has CodeLate and never counts.
func (b Book) ForwardOK(t ledger.Transfer, ud ledger.U128, amount int64, counted bool) bool {
	return t.Ledger == b.Ledger && t.DebitAccountID == b.Wallet && t.CreditAccountID == b.Merchant &&
		t.Amount == ledger.U128From64(uint64(amount)) && t.UserData128 == ud && t.Code == code(counted) &&
		t.Flags == 0 && t.PendingID.IsZero()
}

// ReversalOK is ForwardOK for a stored reversal.
func (b Book) ReversalOK(t ledger.Transfer, ud ledger.U128, amount int64) bool {
	return t.Ledger == b.Ledger && t.DebitAccountID == b.Merchant && t.CreditAccountID == b.Wallet &&
		t.Amount == ledger.U128From64(uint64(amount)) && t.UserData128 == ud && t.Code == CodeReversed &&
		t.Flags == 0 && t.PendingID.IsZero()
}

// Lookup is the ledger read Walk makes; *ledger.Ledger satisfies it.
type Lookup interface {
	LookupTransfer(ctx context.Context, id ledger.U128) (ledger.Transfer, bool, error)
}

var _ Lookup = (*ledger.Ledger)(nil)

// Walk walks a payment's epochs in the ledger: from epoch 0, each forward and its reversal (ids gives both ids of an
// epoch), until a forward is missing or one has no reversal. o carries the payment's Amount and Counted; Walk sets
// its Recorded, Code, Timestamp, Reversed, Foreign and Seqs. A transfer that does not match the payment
// (ForwardOK, ReversalOK), or a walk that reaches maxEpochs, makes it Foreign: it counts for nothing, and is not
// Recorded. epoch is the recorded forward's epoch, next the epoch of the next forward when none is recorded. It only
// reads.
func (b Book) Walk(ctx context.Context, lg Lookup, ids func(epoch uint32) (fwd, rev ledger.U128), ud ledger.U128, o *Out,
	maxEpochs uint32) (epoch, next uint32, err error) {
	o.Recorded, o.Foreign, o.Seqs, o.Code, o.Timestamp = false, false, nil, 0, 0
	for ep := uint32(0); ; ep++ {
		if ep == maxEpochs {
			o.Foreign = true
			break
		}
		fid, rid := ids(ep)
		f, ok, err := lg.LookupTransfer(ctx, fid)
		if err != nil {
			return 0, 0, err
		}
		if !ok {
			next = ep
			break
		}
		if !b.ForwardOK(f, ud, o.Amount, o.Counted) {
			o.Foreign = true
			break
		}
		r, ok, err := lg.LookupTransfer(ctx, rid)
		if err != nil {
			return 0, 0, err
		}
		if !ok {
			o.Recorded, epoch, o.Code, o.Timestamp = true, ep, f.Code, f.Timestamp
			break
		}
		if !b.ReversalOK(r, ud, o.Amount) {
			o.Foreign = true
			break
		}
		o.Seqs = append(o.Seqs, r.UserData64)
	}
	o.Reversed = len(o.Seqs) > 0
	if o.Foreign {
		o.Recorded = false
	}
	return epoch, next, nil
}
