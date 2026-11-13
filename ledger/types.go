// Package ledger is SatBooth's ledger of immutable transfers between accounts, shaped after TigerBeetle's model.
// Where the comments of this package cite TigerBeetle's source files or documents, with or without line numbers, "the
// pinned commit" and the line numbers refer to TigerBeetle at commit 0450c32b2c3c.
package ledger

// Account is a ledger account. Field names are TigerBeetle's. Balances are not stored: they
// are derived from transfers. Callers pass Timestamp 0; the ledger assigns it.
type Account struct {
	ID          U128
	UserData128 U128
	UserData64  uint64
	UserData32  uint32
	Ledger      uint32
	Code        uint16
	Flags       AccountFlags
	Timestamp   uint64
}

// AccountFlags are TigerBeetle's account flag bits (src/tigerbeetle.zig at the pinned commit:
// linked = 1<<0, debits_must_not_exceed_credits = 1<<1, credits_must_not_exceed_debits = 1<<2,
// history = 1<<3, imported = 1<<4, closed = 1<<5). Only the two below are supported; every
// other bit is refused with reserved_flag.
type AccountFlags uint16

const (
	AccountDebitsMustNotExceedCredits AccountFlags = 1 << 1
	AccountCreditsMustNotExceedDebits AccountFlags = 1 << 2

	accountFlagsSupported = AccountDebitsMustNotExceedCredits | AccountCreditsMustNotExceedDebits
)

// CreateAccountResult is TigerBeetle's create_accounts result, by name and value.
type CreateAccountResult string

const (
	AccountCreated                        CreateAccountResult = "created"
	AccountExists                         CreateAccountResult = "exists"
	AccountTimestampMustBeZero            CreateAccountResult = "timestamp_must_be_zero"
	AccountReservedFlag                   CreateAccountResult = "reserved_flag"
	AccountIDMustNotBeZero                CreateAccountResult = "id_must_not_be_zero"
	AccountIDMustNotBeIntMax              CreateAccountResult = "id_must_not_be_int_max"
	AccountExistsWithDifferentFlags       CreateAccountResult = "exists_with_different_flags"
	AccountExistsWithDifferentUserData128 CreateAccountResult = "exists_with_different_user_data_128"
	AccountExistsWithDifferentUserData64  CreateAccountResult = "exists_with_different_user_data_64"
	AccountExistsWithDifferentUserData32  CreateAccountResult = "exists_with_different_user_data_32"
	AccountExistsWithDifferentLedger      CreateAccountResult = "exists_with_different_ledger"
	AccountExistsWithDifferentCode        CreateAccountResult = "exists_with_different_code"
	AccountFlagsAreMutuallyExclusive      CreateAccountResult = "flags_are_mutually_exclusive"
	AccountLedgerMustNotBeZero            CreateAccountResult = "ledger_must_not_be_zero"
	AccountCodeMustNotBeZero              CreateAccountResult = "code_must_not_be_zero"
)

// Transfer is a movement of Amount from the debit account to the credit account. Field names are
// TigerBeetle's. Callers pass Timestamp 0; the ledger assigns it.
type Transfer struct {
	ID              U128
	DebitAccountID  U128
	CreditAccountID U128
	Amount          U128
	PendingID       U128
	UserData128     U128
	UserData64      uint64
	UserData32      uint32
	Timeout         uint32
	Ledger          uint32
	Code            uint16
	Flags           TransferFlags
	Timestamp       uint64
}

// TransferFlags are TigerBeetle's transfer flag bits (src/tigerbeetle.zig at the pinned commit:
// linked = 1<<0, pending = 1<<1, post_pending_transfer = 1<<2, void_pending_transfer = 1<<3,
// balancing_debit = 1<<4, balancing_credit = 1<<5, closing_debit = 1<<6, closing_credit = 1<<7,
// imported = 1<<8). Only the three below are supported; every other bit is refused with
// reserved_flag.
type TransferFlags uint16

const (
	TransferPending             TransferFlags = 1 << 1
	TransferPostPendingTransfer TransferFlags = 1 << 2
	TransferVoidPendingTransfer TransferFlags = 1 << 3

	transferFlagsSupported = TransferPending | TransferPostPendingTransfer | TransferVoidPendingTransfer
)

// CreateTransferResult is TigerBeetle's create_transfers result, by name and value. Constants
// are the code in CamelCase with a Transfer prefix.
type CreateTransferResult string

const (
	TransferCreated                                    CreateTransferResult = "created"
	TransferExists                                     CreateTransferResult = "exists"
	TransferTimestampMustBeZero                        CreateTransferResult = "timestamp_must_be_zero"
	TransferReservedFlag                               CreateTransferResult = "reserved_flag"
	TransferIDMustNotBeZero                            CreateTransferResult = "id_must_not_be_zero"
	TransferIDMustNotBeIntMax                          CreateTransferResult = "id_must_not_be_int_max"
	TransferIDAlreadyFailed                            CreateTransferResult = "id_already_failed"
	TransferExistsWithDifferentFlags                   CreateTransferResult = "exists_with_different_flags"
	TransferExistsWithDifferentPendingID               CreateTransferResult = "exists_with_different_pending_id"
	TransferExistsWithDifferentTimeout                 CreateTransferResult = "exists_with_different_timeout"
	TransferExistsWithDifferentDebitAccountID          CreateTransferResult = "exists_with_different_debit_account_id"
	TransferExistsWithDifferentCreditAccountID         CreateTransferResult = "exists_with_different_credit_account_id"
	TransferExistsWithDifferentAmount                  CreateTransferResult = "exists_with_different_amount"
	TransferExistsWithDifferentUserData128             CreateTransferResult = "exists_with_different_user_data_128"
	TransferExistsWithDifferentUserData64              CreateTransferResult = "exists_with_different_user_data_64"
	TransferExistsWithDifferentUserData32              CreateTransferResult = "exists_with_different_user_data_32"
	TransferExistsWithDifferentLedger                  CreateTransferResult = "exists_with_different_ledger"
	TransferExistsWithDifferentCode                    CreateTransferResult = "exists_with_different_code"
	TransferDebitAccountIDMustNotBeZero                CreateTransferResult = "debit_account_id_must_not_be_zero"
	TransferDebitAccountIDMustNotBeIntMax              CreateTransferResult = "debit_account_id_must_not_be_int_max"
	TransferCreditAccountIDMustNotBeZero               CreateTransferResult = "credit_account_id_must_not_be_zero"
	TransferCreditAccountIDMustNotBeIntMax             CreateTransferResult = "credit_account_id_must_not_be_int_max"
	TransferAccountsMustBeDifferent                    CreateTransferResult = "accounts_must_be_different"
	TransferPendingIDMustBeZero                        CreateTransferResult = "pending_id_must_be_zero"
	TransferTimeoutReservedForPendingTransfer          CreateTransferResult = "timeout_reserved_for_pending_transfer"
	TransferLedgerMustNotBeZero                        CreateTransferResult = "ledger_must_not_be_zero"
	TransferCodeMustNotBeZero                          CreateTransferResult = "code_must_not_be_zero"
	TransferDebitAccountNotFound                       CreateTransferResult = "debit_account_not_found"
	TransferCreditAccountNotFound                      CreateTransferResult = "credit_account_not_found"
	TransferAccountsMustHaveTheSameLedger              CreateTransferResult = "accounts_must_have_the_same_ledger"
	TransferTransferMustHaveTheSameLedgerAsAccounts    CreateTransferResult = "transfer_must_have_the_same_ledger_as_accounts"
	TransferOverflowsDebitsPending                     CreateTransferResult = "overflows_debits_pending"
	TransferOverflowsCreditsPending                    CreateTransferResult = "overflows_credits_pending"
	TransferOverflowsDebitsPosted                      CreateTransferResult = "overflows_debits_posted"
	TransferOverflowsCreditsPosted                     CreateTransferResult = "overflows_credits_posted"
	TransferOverflowsDebits                            CreateTransferResult = "overflows_debits"
	TransferOverflowsCredits                           CreateTransferResult = "overflows_credits"
	TransferOverflowsTimeout                           CreateTransferResult = "overflows_timeout"
	TransferExceedsCredits                             CreateTransferResult = "exceeds_credits"
	TransferExceedsDebits                              CreateTransferResult = "exceeds_debits"
	TransferFlagsAreMutuallyExclusive                  CreateTransferResult = "flags_are_mutually_exclusive"
	TransferPendingIDMustNotBeZero                     CreateTransferResult = "pending_id_must_not_be_zero"
	TransferPendingIDMustNotBeIntMax                   CreateTransferResult = "pending_id_must_not_be_int_max"
	TransferPendingIDMustBeDifferent                   CreateTransferResult = "pending_id_must_be_different"
	TransferPendingTransferNotFound                    CreateTransferResult = "pending_transfer_not_found"
	TransferPendingTransferNotPending                  CreateTransferResult = "pending_transfer_not_pending"
	TransferPendingTransferHasDifferentDebitAccountID  CreateTransferResult = "pending_transfer_has_different_debit_account_id"
	TransferPendingTransferHasDifferentCreditAccountID CreateTransferResult = "pending_transfer_has_different_credit_account_id"
	TransferPendingTransferHasDifferentLedger          CreateTransferResult = "pending_transfer_has_different_ledger"
	TransferPendingTransferHasDifferentCode            CreateTransferResult = "pending_transfer_has_different_code"
	TransferExceedsPendingTransferAmount               CreateTransferResult = "exceeds_pending_transfer_amount"
	TransferPendingTransferHasDifferentAmount          CreateTransferResult = "pending_transfer_has_different_amount"
	TransferPendingTransferAlreadyPosted               CreateTransferResult = "pending_transfer_already_posted"
	TransferPendingTransferAlreadyVoided               CreateTransferResult = "pending_transfer_already_voided"
	TransferPendingTransferExpired                     CreateTransferResult = "pending_transfer_expired"
	// TransferLinkedEventFailed is TigerBeetle's linked_event_failed: in a CreateTransfers batch, every item other
	// than the one refused (the batch is one linked chain, so nothing in it is applied).
	TransferLinkedEventFailed CreateTransferResult = "linked_event_failed"
)

// Balance is an account's balance as of a moment: sums over its transfers, never stored.
type Balance struct {
	DebitsPending, DebitsPosted, CreditsPending, CreditsPosted U128
}
