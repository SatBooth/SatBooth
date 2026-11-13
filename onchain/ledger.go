package onchain

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

// The two fixed ledger accounts, in ledger 2 (sat). A counted payment debits OnchainWallet (the money as it sits in
// the merchant's wallet) and credits MerchantOnchain (what SatBooth owes the merchants); a reversal does the
// opposite. The flags mirror lnpay's pair.
var (
	OnchainWallet   = ledger.U128From64(0x4f43_0001)
	MerchantOnchain = ledger.U128From64(0x4f43_0002)
)

const (
	ledgerID       = 2 // sat
	codeWallet     = 3
	codeMerchant   = 4
	codePaid       = rail.CodePaid     // a counted payment
	codeReversed   = rail.CodeReversed // a reversal
	codeLate       = rail.CodeLate     // a late payment
	paidDomain     = "satbooth/onchain-paid/v1"
	reversedDomain = "satbooth/onchain-reversed/v1"
	maxEpochs      = rail.MaxEpochs // a safety stop for the epoch walk
)

func transferID(domain, txid string, vout, epoch uint32) ledger.U128 {
	raw, err := hex.DecodeString(txid)
	if err != nil || len(raw) != 32 {
		return ledger.U128Zero
	}
	var n [8]byte
	binary.BigEndian.PutUint32(n[:4], vout)
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

// PaidTransferID is the id of the transfer that counts the output (txid, vout) in the given epoch: the first 16 bytes
// of SHA-256("satbooth/onchain-paid/v1" || the 32 raw bytes of the txid || vout, 4 bytes big-endian || epoch, 4 bytes
// big-endian). A bad txid gives the zero id, which the ledger refuses.
func PaidTransferID(txid string, vout uint32, epoch uint32) ledger.U128 {
	return transferID(paidDomain, txid, vout, epoch)
}

// ReversalTransferID is the id of the transfer that reverses the forward transfer of the same (txid, vout, epoch).
func ReversalTransferID(txid string, vout uint32, epoch uint32) ledger.U128 {
	return transferID(reversedDomain, txid, vout, epoch)
}

// orderUserData turns the order's 32 hex characters into a transfer's UserData128.
func orderUserData(orderID string) (ledger.U128, error) {
	raw, err := hex.DecodeString(orderID)
	if err != nil || len(raw) != 16 {
		return ledger.U128Zero, errors.New("onchain: bad order id")
	}
	return ledger.U128FromBytes(raw)
}

func ensureAccounts(ctx context.Context, lg *ledger.Ledger) error {
	for _, a := range []ledger.Account{
		{ID: OnchainWallet, Ledger: ledgerID, Code: codeWallet, Flags: ledger.AccountCreditsMustNotExceedDebits},
		{ID: MerchantOnchain, Ledger: ledgerID, Code: codeMerchant, Flags: ledger.AccountDebitsMustNotExceedCredits},
	} {
		r, err := lg.CreateAccount(ctx, a)
		if err != nil {
			return fmt.Errorf("onchain: creating a ledger account: %w", err)
		}
		if r != ledger.AccountCreated && r != ledger.AccountExists {
			return fmt.Errorf("onchain: ledger account refused: %s", r)
		}
	}
	return nil
}

// confirmedSum is the sum of the order's outputs that the ledger counts as paid: for each output, the forward
// transfers are walked from epoch 0 until one is missing or one has no reversal (outputView), and that last forward
// counts only if its code is 2 (a counted payment, never a late one, code 4) and its ledger, accounts, amount and order
// are the output's. It is rail.Sums' confirmed sum over orderViews, the one rule settle and Rail use (marks do not
// enter it).
func (s *Service) confirmedSum(ctx context.Context, o store.Order) (int64, error) {
	views, err := s.orderViews(ctx, o, false)
	if err != nil {
		return 0, err
	}
	sum, _, _ := rail.Sums(outs(views), store.MaxSat)
	return sum, nil
}

// paidOnchain reports whether the ledger counts at least the order's on-chain amount.
func (s *Service) paidOnchain(ctx context.Context, o store.Order) (bool, error) {
	if o.AmountSat == 0 {
		return false, nil
	}
	sum, err := s.confirmedSum(ctx, o)
	return err == nil && sum >= o.AmountSat, err
}
