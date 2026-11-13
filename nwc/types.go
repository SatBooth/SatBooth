package nwc

import (
	"encoding/json"
	"errors"
	"strconv"
)

// ErrMalformed is wrapped by every error that says a wallet message broke the format. The wrapped
// detail is a fixed description, never wallet-supplied text.
var ErrMalformed = errors.New("nwc: malformed wallet message")

// maxSafeInt is 2^53, the largest integer every JSON implementation carries exactly.
const maxSafeInt = 1 << 53

// Transaction types and states (getAlby/hub 921003b nip47/models/models.go:67-82,
// nip47/models/transactions.go:13-59).
const (
	TypeIncoming = "incoming"
	TypeOutgoing = "outgoing"

	StatePending  = "pending"
	StateSettled  = "settled"
	StateFailed   = "failed"
	StateAccepted = "accepted"
	StateExpired  = "expired"
)

// Notification types (getAlby/hub 921003b nip47/notifications/models.go).
const (
	NotifPaymentReceived     = "payment_received"
	NotifPaymentSent         = "payment_sent"
	NotifHoldInvoiceAccepted = "hold_invoice_accepted"
)

// Transaction is a wallet's view of one invoice or payment. Amounts are millisatoshis.
type Transaction struct {
	Type            string          `json:"type"`
	State           string          `json:"state"`
	Invoice         string          `json:"invoice"`
	Description     string          `json:"description"`
	DescriptionHash string          `json:"description_hash"`
	Preimage        string          `json:"preimage"`
	PaymentHash     string          `json:"payment_hash"`
	AmountMsat      int64           `json:"amount"`
	FeesPaidMsat    int64           `json:"fees_paid"`
	CreatedAt       int64           `json:"created_at"`
	ExpiresAt       *int64          `json:"expires_at"`
	SettledAt       *int64          `json:"settled_at"`
	Metadata        json.RawMessage `json:"metadata,omitempty"`
}

// TransactionList is the result of list_transactions.
type TransactionList struct {
	Transactions []Transaction
	TotalCount   int64
}

// PayResult is the result of pay_invoice.
type PayResult struct {
	Preimage     string
	FeesPaidMsat int64
}

// Budget is the result of get_budget. Set is false when the wallet answers {}: no budget.
type Budget struct {
	Set           bool
	UsedMsat      int64
	TotalMsat     int64
	RenewsAt      *int64
	RenewalPeriod string
}

// Info is the result of get_info. Fields the wallet withholds (it does so without the get_info
// scope) are zero.
type Info struct {
	Network       string
	Methods       []string
	Notifications []string
	BlockHeight   int64
}

// Notification is a validated payment notification.
type Notification struct {
	Type    string
	Tx      Transaction
	EventID string
}

// WalletError is an error the wallet reported in a response.
type WalletError struct {
	Code    string
	Message string
}

// Error quotes and truncates the wallet's message, which is untrusted text.
func (e *WalletError) Error() string {
	m := e.Message
	if len(m) > 200 {
		m = m[:200]
	}
	return "nwc: wallet error " + e.Code + ": " + strconv.QuoteToASCII(m)
}
