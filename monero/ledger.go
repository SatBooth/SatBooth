package monero

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

// The two fixed ledger accounts, in ledger 3 (piconero). A counted payment debits MoneroWallet (the money as it sits
// in the merchant's wallet) and credits MerchantMonero (what SatBooth owes the merchants); a reversal does the
// opposite. The flags mirror the on-chain pair.
var (
	MoneroWallet   = ledger.U128From64(0x584d_0001)
	MerchantMonero = ledger.U128From64(0x584d_0002)
)

const (
	ledgerID       = 3 // piconero
	codeWallet     = 5
	codeMerchant   = 6
	codePaid       = rail.CodePaid     // a counted payment
	codeReversed   = rail.CodeReversed // a reversal
	codeLate       = rail.CodeLate     // a late payment
	paidDomain     = "satbooth/monero-paid/v1"
	reversedDomain = "satbooth/monero-reversed/v1"
	maxEpochs      = rail.MaxEpochs // a safety stop for the epoch walk

	// spendableAge is CRYPTONOTE_DEFAULT_TX_SPENDABLE_AGE (src/cryptonote_config.h:49, tag v0.18.5.4): the normal
	// lock of a received output, in blocks.
	spendableAge = 10
)

func transferID(domain, txid string, index, epoch uint32) ledger.U128 {
	raw, err := hex.DecodeString(txid)
	if err != nil || len(raw) != 32 {
		return ledger.U128Zero
	}
	var n [8]byte
	binary.BigEndian.PutUint32(n[:4], index)
	binary.BigEndian.PutUint32(n[4:], epoch)
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write(raw)
	h.Write(n[:])
	id, err := ledger.U128FromBytes(h.Sum(nil)[:16])
	if err != nil {
		return ledger.U128Zero
	}
	return id
}

// PaidTransferID is the id of the transfer that counts the payment of txid to the subaddress at minor index index in
// the given epoch: the first 16 bytes of SHA-256("satbooth/monero-paid/v1" || the 32 raw bytes of the txid || index,
// 4 bytes big-endian || epoch, 4 bytes big-endian). A bad txid gives the zero id, which the ledger refuses.
func PaidTransferID(txid string, index uint32, epoch uint32) ledger.U128 {
	return transferID(paidDomain, txid, index, epoch)
}

// ReversalTransferID is the id of the transfer that reverses the forward transfer of the same (txid, index, epoch),
// under the domain "satbooth/monero-reversed/v1".
func ReversalTransferID(txid string, index uint32, epoch uint32) ledger.U128 {
	return transferID(reversedDomain, txid, index, epoch)
}

// orderUserData turns the order's 32 hex characters into a transfer's UserData128.
func orderUserData(orderID string) (ledger.U128, error) {
	raw, err := hex.DecodeString(orderID)
	if err != nil || len(raw) != 16 {
		return ledger.U128Zero, errors.New("monero: bad order id")
	}
	return ledger.U128FromBytes(raw)
}

func ensureAccounts(ctx context.Context, lg *ledger.Ledger) error {
	for _, a := range []ledger.Account{
		{ID: MoneroWallet, Ledger: ledgerID, Code: codeWallet, Flags: ledger.AccountCreditsMustNotExceedDebits},
		{ID: MerchantMonero, Ledger: ledgerID, Code: codeMerchant, Flags: ledger.AccountDebitsMustNotExceedCredits},
	} {
		r, err := lg.CreateAccount(ctx, a)
		if err != nil {
			return fmt.Errorf("monero: creating a ledger account: %w", err)
		}
		if r != ledger.AccountCreated && r != ledger.AccountExists {
			return fmt.Errorf("monero: ledger account refused: %s", r)
		}
	}
	return nil
}

// lockedBeyondNormal reports whether a payment's unlock_time u locks it beyond the normal lock, judged against
// seenAt, the scan's height when the payment was first stored: u > 0 and u > seenAt + 10. A timestamp form
// (at least 500,000,000) always exceeds it.
func lockedBeyondNormal(u, seenAt int64) bool { return u > 0 && u > seenAt+spendableAge }

// counted is the one predicate of a counted Monero payment: first seen before the order's expiry, and not locked
// beyond the normal lock at first sight. It is fixed per row, so a classification never flips as the chain grows.
func counted(p store.Payment, expiresAt time.Time) bool {
	return p.FirstSeen.Before(expiresAt) && !lockedBeyondNormal(p.UnlockTime, p.FirstSeenHeight)
}

// payView is what the store and the ledger say about one payment of an order: the rail's Out, and the payment's key
// and epochs in the ledger. Marks are not read here: the confirmed sum does not need them.
type payView struct {
	rail.Out
	p     store.Payment
	index uint32 // the subaddress's minor index, part of the transfer ids
	epoch uint32 // the recorded forward's epoch
	next  uint32 // the epoch of the next forward, when not recorded
}

func outs(views []payView) []rail.Out {
	r := make([]rail.Out, len(views))
	for i, v := range views {
		r[i] = v.Out
	}
	return r
}

// book is the rail's two accounts in ledger 3 (rail.Book): the forward and reversal transfers and their checks.
var book = rail.Book{Ledger: ledgerID, Wallet: MoneroWallet, Merchant: MerchantMonero}

// paymentView walks the payment's epochs in the ledger (rail.Book.Walk, the walk the on-chain rail makes, with the
// Monero ids): from epoch 0, each forward and its reversal, until a forward is missing or one has no reversal. A
// transfer that does not match the payment makes it Foreign (it counts for nothing).
func (s *Service) paymentView(ctx context.Context, o store.Order, p store.Payment, index uint32) (payView, error) {
	ud, err := orderUserData(o.ID)
	if err != nil {
		return payView{}, err
	}
	v := payView{p: p, index: index}
	v.Amount, v.Counted = p.AmountPiconero, counted(p, o.ExpiresAt)
	ids := func(ep uint32) (ledger.U128, ledger.U128) {
		return PaidTransferID(p.TxID, index, ep), ReversalTransferID(p.TxID, index, ep)
	}
	if v.epoch, v.next, err = book.Walk(ctx, s.lg, ids, ud, &v.Out, maxEpochs); err != nil {
		return payView{}, err
	}
	return v, nil
}

// paymentViews reads the order's payments (all to its one subaddress) and their ledger records. An order without a
// subaddress has none.
func (s *Service) paymentViews(ctx context.Context, o store.Order) ([]payView, error) {
	sub, err := s.st.SubaddressForOrder(ctx, o.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	ps, err := s.st.PaymentsForOrder(ctx, o.ID)
	if err != nil {
		return nil, err
	}
	views := make([]payView, 0, len(ps))
	for _, p := range ps {
		v, err := s.paymentView(ctx, o, p, uint32(sub.Index))
		if err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return views, nil
}

// confirmedSum is the sum of the order's payments that the ledger counts as paid: rail.Sums' confirmed sum over
// paymentViews (a recorded counted payment whose last forward has code 2 and no reversal), at most MaxPiconero.
func (s *Service) confirmedSum(ctx context.Context, o store.Order) (int64, error) {
	views, err := s.paymentViews(ctx, o)
	if err != nil {
		return 0, err
	}
	sum, _, _ := rail.Sums(outs(views), store.MaxPiconero)
	return sum, nil
}

// paidMonero reports whether the ledger counts at least the order's Monero amount.
func (s *Service) paidMonero(ctx context.Context, o store.Order) (bool, error) {
	if o.AmountPiconero == 0 {
		return false, nil
	}
	sum, err := s.confirmedSum(ctx, o)
	return err == nil && sum >= o.AmountPiconero, err
}
