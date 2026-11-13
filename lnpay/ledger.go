package lnpay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/store"
)

// The two fixed ledger accounts. Both are created at startup, in ledger 1 (msat).
//
// A payment is a transfer that debits LightningHub (the money as it sits at the hub) and credits
// MerchantLightning (what SatBooth owes the merchants). The flags:
//   - LightningHub has AccountCreditsMustNotExceedDebits: payments debit it and refunds (plan 9, the
//     refund package's pending transfers, posted when a refund settles) credit it, and nothing else
//     touches it, so it must never be credited more than it was debited: no refund can take out more
//     than was paid in. It mirrors the money the hub holds for us.
//   - MerchantLightning has AccountDebitsMustNotExceedCredits: nothing may be taken out of it
//     beyond what was paid in, so a later bug in a payout path cannot overdraw it.
var (
	LightningHub      = ledger.U128From64(0x4c4e_0001)
	MerchantLightning = ledger.U128From64(0x4c4e_0002)
)

// LedgerMsat is the ledger of the two accounts and of every transfer between them (refund's too): msat.
const LedgerMsat = 1

const (
	codePaid       = 1 // a Lightning payment received
	codeHubAccount = 1
	codeMerchant   = 2
	paidDomain     = "satbooth/ln-paid/v1"
)

// PaidTransferID is the id of the ledger transfer that records the payment of the invoice with the
// given payment hash: the first 16 bytes of SHA-256("satbooth/ln-paid/v1" || the 32 raw bytes of
// the payment hash), read big-endian. The hash is 64 hex characters, either case; any other input
// gives the zero id, which the ledger refuses.
func PaidTransferID(paymentHash string) ledger.U128 {
	raw, err := hex.DecodeString(paymentHash)
	if err != nil || len(raw) != 32 {
		return ledger.U128Zero
	}
	h := sha256.New()
	h.Write([]byte(paidDomain))
	h.Write(raw)
	id, err := ledger.U128FromBytes(h.Sum(nil)[:16])
	if err != nil {
		return ledger.U128Zero
	}
	return id
}

// orderUserData turns the order's 32 hex characters into the transfer's UserData128.
func orderUserData(orderID string) (ledger.U128, error) {
	raw, err := hex.DecodeString(orderID)
	if err != nil || len(raw) != 16 {
		return ledger.U128Zero, fmt.Errorf("lnpay: bad order id")
	}
	return ledger.U128FromBytes(raw)
}

func ensureAccounts(ctx context.Context, lg *ledger.Ledger) error {
	for _, a := range []ledger.Account{
		{ID: LightningHub, Ledger: LedgerMsat, Code: codeHubAccount, Flags: ledger.AccountCreditsMustNotExceedDebits},
		{ID: MerchantLightning, Ledger: LedgerMsat, Code: codeMerchant, Flags: ledger.AccountDebitsMustNotExceedCredits},
	} {
		r, err := lg.CreateAccount(ctx, a)
		if err != nil {
			return fmt.Errorf("lnpay: creating a ledger account: %w", err)
		}
		if r != ledger.AccountCreated && r != ledger.AccountExists {
			return fmt.Errorf("lnpay: ledger account refused: %s", r)
		}
	}
	return nil
}

// recordPaid creates the transfer for the invoice, with amount the settled amount. A transfer that
// already exists is success. It reports whether this call created it.
func (s *Service) recordPaid(ctx context.Context, inv store.Invoice, amountMsat int64) (created bool, err error) {
	ud, err := orderUserData(inv.OrderID)
	if err != nil {
		return false, err
	}
	// The settle lock is held for the write only: Status reads "paid" from this transfer, so a watched rail's settle,
	// which holds the same lock, either reads it before it exists or ends before it is written. No other lock is held
	// here, and nothing under the settle lock calls recordPaid.
	r, err := s.createPaidTransfer(ctx, ledger.Transfer{
		ID: PaidTransferID(inv.PaymentHash), DebitAccountID: LightningHub, CreditAccountID: MerchantLightning,
		Amount: ledger.U128From64(uint64(amountMsat)), UserData128: ud, Ledger: LedgerMsat, Code: codePaid,
	})
	if err != nil {
		return false, err
	}
	switch r {
	case ledger.TransferCreated:
		return true, nil
	case ledger.TransferExists:
		return false, nil
	}
	if strings.HasPrefix(string(r), "exists_with_different_") {
		// The payment is recorded, with other details than this lookup shows. Never create a second
		// one; the caller carries on, and the alert line is for the operator.
		s.log.Error(logDifferentRecord, "result", string(r))
		return false, nil
	}
	return false, fmt.Errorf("lnpay: ledger refused the payment record: %s", r)
}

// createPaidTransfer writes the paid transfer under the settle lock, when one is set.
func (s *Service) createPaidTransfer(ctx context.Context, t ledger.Transfer) (ledger.CreateTransferResult, error) {
	if s.settleMu != nil {
		s.settleMu.Lock()
		defer s.settleMu.Unlock()
	}
	return s.lg.CreateTransfer(ctx, t)
}

// paidTransfer returns the transfer recording the invoice's payment, if there is one. Only Lightning
// invoices have one: an on-chain-only order has no invoice, so it never reads paid here.
func (s *Service) paidTransfer(ctx context.Context, paymentHash string) (ledger.Transfer, bool, error) {
	return s.lg.LookupTransfer(ctx, PaidTransferID(paymentHash))
}

// maxPaidMsat is 2^53, the largest total every JSON consumer reads exactly.
const maxPaidMsat = 1 << 53

// PaidMsat returns the sum of the amounts of the ledger's paid transfers of every invoice of the order; an invoice
// with no paid transfer adds nothing. A transfer above 2^53 msat, or a sum above it, is an error: nothing wraps.
func (s *Service) PaidMsat(ctx context.Context, orderID string) (int64, error) {
	invs, err := s.st.InvoicesForOrder(ctx, orderID)
	if err != nil {
		return 0, err
	}
	var sum int64
	for _, inv := range invs {
		x, found, err := s.paidTransfer(ctx, inv.PaymentHash)
		if err != nil {
			return 0, err
		}
		if !found {
			continue
		}
		if x.Amount.Hi != 0 || x.Amount.Lo > maxPaidMsat {
			return 0, fmt.Errorf("lnpay: a paid transfer is above 2^53 msat")
		}
		sum += int64(x.Amount.Lo) // at most 2^53 + 2^53: no wrap
		if sum > maxPaidMsat {
			return 0, fmt.Errorf("lnpay: the order's paid total is above 2^53 msat")
		}
	}
	return sum, nil
}
