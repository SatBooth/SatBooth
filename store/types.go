package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/satbooth/satbooth/rates"
)

// Errors. Callers test them with errors.Is; the wrapped messages never contain a token, a
// payment request or any other secret-bearing value.
var (
	// ErrNotFound: the row asked for, or the parent row an insert needs, does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict: CreateOrder or CreateQuotedOrder found the merchant reference used for another order: another
	// account, another amount (explicit), another price, currency or rail set (priced), or the other kind of order.
	ErrConflict = errors.New("store: merchant reference already used for a different order")
	// ErrDuplicate: AddInvoice found the payment hash already stored.
	ErrDuplicate = errors.New("store: duplicate payment hash")
	// ErrInvalid: an argument failed validation, before anything was written.
	ErrInvalid = errors.New("store: invalid argument")
	// ErrAddressTaken: AddAddress found that the order has an address, or the address is already stored.
	ErrAddressTaken = errors.New("store: the order has an address, or the address is already stored")
	// ErrSubaddressTaken: AddSubaddress found that the order has a subaddress, or the subaddress is already stored.
	ErrSubaddressTaken = errors.New("store: the order has a subaddress, or the subaddress is already stored")
	// ErrOldSchema: Open found a database from before plan 8 (no amount_piconero column in orders).
	ErrOldSchema = errors.New("store: the database is from before the Monero tables; this version does not upgrade it; start with a new store and a new ledger (both database files)")
	// ErrOldMoneroSchema: Open found a version-5 database from an early plan 8 build, whose monero_cursor has no
	// wallet_height column (ruling R5), whose monero_marks has no confirmations column (ruling 8-A6), or that has no
	// monero_pending_settle table (ruling S4): migration 5 was edited in place before any release.
	ErrOldMoneroSchema = errors.New("store: the database is from an early build of the Monero tables (monero_cursor without wallet_height, monero_marks without confirmations, or no monero_pending_settle); this version does not upgrade it; start with a new store and a new ledger (both database files)")
	// ErrOldRefundSchema: Open found a database whose refund_results was made before plan 11 (it does not know the
	// reason cancelled): migration 6 was edited in place before any release.
	ErrOldRefundSchema = errors.New("store: the database is from before the refund void change; this version does not upgrade it; start with a new store and a new ledger (both database files)")
	// ErrIndexNotAbove: AddAddress or AddSubaddress was given an index that is not strictly above every stored one
	// of its table. It wraps ErrInvalid; no other cause of ErrInvalid is ErrIndexNotAbove.
	ErrIndexNotAbove = fmt.Errorf("%w: address index must be above every stored index", ErrInvalid)
	// ErrPaymentChanged: RecordMoneroScan was given a stored payment (txid, subaddress) with another amount or unlock
	// time, and wrote nothing. It wraps ErrInvalid; the error's text adds the txid and the subaddress index, nothing
	// else of what wallet-rpc reported. The Monero watcher drops such an entry from its scan and raises an ALERT.
	ErrPaymentChanged = fmt.Errorf("%w: a stored payment arrived with another amount or unlock time", ErrInvalid)
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Event types.
const (
	EventPaid      = "order.paid"
	EventExpired   = "order.expired"
	EventUnderpaid = "order.underpaid"
	EventLate      = "order.late"
	EventReverted  = "order.reverted"
	EventPaidAgain = "order.paid_again"
)

// MaxSat is the largest on-chain amount, 21 million bitcoin in satoshi: rates.MaxSats, the one definition.
const MaxSat = rates.MaxSats

// MaxAddressIdx is the largest address index (a non-hardened BIP 32 index).
const MaxAddressIdx = 2147483647

// MaxPiconero is the largest Monero amount, 2^53 − 1 piconero: rates.MaxPiconero, the one definition.
const MaxPiconero = rates.MaxPiconero

// MaxSubaddressIdx is the largest Monero subaddress minor index the store holds (index 0 is the primary address
// and is never stored).
const MaxSubaddressIdx = 2147483647

// WatchWindow is how long after an on-chain order's expiry its address and outputs are still watched.
const WatchWindow = 7 * 24 * time.Hour

// The reasons CloseInvoice takes.
const (
	// CloseLookup: a successful wallet lookup made after the invoice's ExpiresAt (plus lnpay's grace)
	// showed it not settled. The order can then expire, but lnpay keeps looking the invoice up until
	// its deadline (InvoicesClosedByLookup), because the wallet can still settle it.
	CloseLookup = "lookup"
	// CloseDeadline: past the deadline (at least ExpiresAt + DeadlineGrace), the wallet answered
	// NOT_FOUND. lnpay closes for no other answer: a transport failure or any other wallet error
	// leaves the invoice open.
	CloseDeadline = "deadline"
)

// DeadlineGrace is how long after an invoice's ExpiresAt a "deadline" close may be recorded.
const DeadlineGrace = 24 * time.Hour

// Order is a customer's request to pay AmountMsat (Lightning), AmountSat (on-chain) and/or AmountPiconero (Monero) to
// AccountID. An amount of 0 means no rail of that kind; at least one is positive. ID and Token are drawn by
// CreateOrder (callers pass them empty). CreatedAt is stamped by the store from its clock;
// ExpiresAt is set by the caller.
type Order struct {
	ID          string // 32 lowercase hex characters (128 random bits); opaque, used in invoice descriptions
	Token       string // 32 lowercase hex characters (128 random bits); the customer's capability for the checkout page
	AccountID   string // 1 to 64 characters of [A-Za-z0-9._-]
	MerchantRef string // 1 to 64 characters of [A-Za-z0-9._-]; unique across all orders
	AmountMsat  int64  // 0 (no Lightning rail) or 1 to 2^53
	AmountSat   int64  // 0 (no on-chain rail) or 1 to MaxSat
	// AmountPiconero is 0 (no Monero rail) or 1 to MaxPiconero.
	AmountPiconero int64
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

// MaxQuoteSources is the most sources a quote records.
const MaxQuoteSources = 8

// QuoteSource is one provider's rate behind a quote's rate. Provider is 1 to 16 bytes of [a-z]; Rate is a decimal
// (the currency per unit of the asset, in the grammar of rates.ParseDecimal); At is when the provider's rate was
// taken, after 1970. The store keeps At in whole seconds.
type QuoteSource struct {
	Provider string
	Rate     string
	At       time.Time
}

// AssetRate is the rate one asset of a priced order was converted at: Asset is rates.AssetBTC or rates.AssetXMR,
// Rate a decimal in the grammar of rates.ParseDecimal (the currency per unit of the asset), and Sources holds 1 to
// MaxQuoteSources provider rates, in the order given.
type AssetRate struct {
	Asset, Rate string
	Sources     []QuoteSource
}

// Quote is the price of a priced order and the rates it was converted at, recorded with the order and never changed.
// Price is a decimal in the grammar of rates.ParseDecimal (1 to 20 integer digits, optionally a point and 1 to 18
// fraction digits), kept as the text given; Currency passes rates.ValidCurrency (3 bytes of [A-Z], neither BTC nor
// XMR); MarginBps is rates.MinMarginBps to rates.MaxMarginBps; Rates holds 1 or 2 rates of distinct assets among BTC
// and XMR (QuoteForOrder returns them sorted by asset). OrderID is filled by the store. QuotedAt and each source's At
// are after 1970, and kept in whole seconds: a fraction is dropped, never refused.
type Quote struct {
	OrderID, Price, Currency string
	MarginBps                int64
	QuotedAt                 time.Time
	Rates                    []AssetRate
}

// Subaddress is the Monero subaddress issued for an order: Index is its minor index in account 0 of the merchant's
// view-only wallet, 1 to MaxSubaddressIdx (0 is the primary address, never issued). The store insists on a strictly
// rising index, so a wallet restored from an older file cannot hand out a subaddress again. CreatedAt is stamped
// from the clock.
type Subaddress struct {
	OrderID, Address string
	Index            int64
	CreatedAt        time.Time
}

// Payment is a Monero payment to an issued subaddress: the amount one transaction pays to it (wallet-rpc's
// get_transfers entry). UnlockTime is the transaction's unlock_time as wallet-rpc reports it (0 for none).
// FirstSeen is SatBooth's clock when the watcher's discovery first saw it (CommitMoneroPass stores the caller's;
// RecordMoneroScan stamps its own clock), never wallet-rpc's timestamp, and
// FirstSeenHeight the wallet's height then (the cursor's WalletHeight, never its scan position, ruling R6): the lock
// rule is judged against it once. Confirmed is read by RecordMoneroScan only (the scan listed the payment mined, type "in"), never stored
// or returned.
type Payment struct {
	TxID, Address                               string
	AmountPiconero, UnlockTime, FirstSeenHeight int64
	FirstSeen                                   time.Time
	Confirmed                                   bool
}

// PaymentKey identifies a Monero payment: one transaction's amount to one subaddress.
type PaymentKey struct {
	TxID, Address string
}

// MoneroCursor is a point of the Monero scan. Height is where the next scan starts (the highest wallet height a scan
// has reached; it never goes down), WalletHeight the wallet's own height at that scan (get_height), from which
// confirmations are counted (ruling 8-A2; below Height while the wallet catches up, never above it), and At when.
type MoneroCursor struct {
	Height       int64
	WalletHeight int64
	At           time.Time
}

// MoneroMark is one observation about a payment: in the pool (MarkMempool), mined (MarkMined, Height at least 1), or
// dead (MarkDead: dropped or double-spent). The latest Seq is the current state. There is no block hash: wallet-rpc
// reports none that SatBooth checks. Confirmations is wallet-rpc's own count in the same answer as the mark (0 to
// MaxMoneroConfirmations, 0 for a mempool or dead mark; ruling 8-A6): confirmations are counted as the least of it
// and the stored wallet height less Height, so a count is never above what the wallet said with this observation.
type MoneroMark struct {
	TxID, Address string
	Seq           int
	Kind          string
	Height        int64
	At            time.Time
	Confirmations int64
}

// MaxMoneroConfirmations bounds a mark's Confirmations: CRYPTONOTE_MAX_BLOCK_NUMBER (src/cryptonote_config.h at
// v0.18.5.4), the bound xmrrpc reads wallet-rpc's heights and confirmations against.
const MaxMoneroConfirmations = 500_000_000

// Invoice is one Lightning invoice made for an order. PaymentHash is 64 lowercase hex characters.
// A zero CreatedAt is stamped from the clock by AddInvoice.
type Invoice struct {
	OrderID     string
	PaymentHash string
	Bolt11      string // 1 to 4096 printable ASCII characters, no spaces
	AmountMsat  int64
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// Closure is the record that an invoice is closed: no more lookups, and it no longer keeps its
// order open. It is not a payment status; the caller checks the ledger for a paid transfer.
type Closure struct {
	PaymentHash string
	At          time.Time
	Reason      string // CloseLookup or CloseDeadline
}

// Event is a webhook event. Its ID is EventID(Type, OrderID, Seq), so an order has at most one event
// of each type and sequence number. Only order.reverted and order.paid_again repeat (Seq above 1).
// CreatedAt is stamped from the store's clock by AddEvent.
type Event struct {
	ID        string
	Type      string // one of the Event constants
	OrderID   string
	Seq       int // 0 in AddEvent means 1
	CreatedAt time.Time
}

// Attempt is one delivery attempt of a webhook event (not an invoice attempt; see
// AddInvoiceAttempt). Status is the HTTP status, or 0 when there was no response. OK is whether
// the receiver accepted the event. A zero At is stamped from the clock by AddAttempt.
type Attempt struct {
	Endpoint string // 1 to 64 characters of [A-Za-z0-9._-]
	EventID  string
	At       time.Time
	Status   int
	OK       bool
	// Final says that the caller makes no further attempt of this event for this endpoint. A Final attempt that is not
	// OK also writes the "abandoned, not yet announced" row (PendingAbandoned), in the same transaction.
	Final bool
}

// MainEndpoint is the name attempts recorded before migration 9 belong to.
const MainEndpoint = "main"

// EventID returns hex(SHA-256("satbooth/event/v1" || typ || orderID))[:32] for seq 1, and
// hex(SHA-256("satbooth/event/v1" || typ || orderID || "/" || decimal seq))[:32] for seq 2 and up.
// A seq of 0 or less gives the id of seq 1.
func EventID(typ, orderID string, seq int) string {
	in := "satbooth/event/v1" + typ + orderID
	if seq >= 2 {
		in += "/" + strconv.Itoa(seq)
	}
	sum := sha256.Sum256([]byte(in))
	return hex.EncodeToString(sum[:])[:32]
}

// randHex returns 128 random bits as 32 lowercase hex characters.
func randHex() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: reading random bytes: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func ts(t time.Time) int64     { return t.Unix() }
func fromTS(n int64) time.Time { return time.Unix(n, 0).UTC() }

// The Go validators. They mirror the CHECK constraints in schema.go exactly (FuzzValidators
// compares the two on the same input).

func validLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// validName: 1 to 64 characters of [A-Za-z0-9._-] (account ids and merchant references).
func validName(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// ValidEndpoint reports whether name is an endpoint name: 1 to 64 characters of [A-Za-z0-9._-].
func ValidEndpoint(name string) bool { return validName(name) }

// validBolt11: 1 to 4096 bytes in 0x21..0x7e.
func validBolt11(s string) bool {
	if len(s) < 1 || len(s) > 4096 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func validAmount(n int64) bool { return n >= 1 && n <= maxAmount }

// validDecimal reports whether s is in rates.ParseDecimal's grammar (checkDecimal is its SQL mirror).
func validDecimal(s string) bool {
	_, err := rates.ParseDecimal(s)
	return err == nil
}

// validProvider: 1 to 16 bytes of [a-z]. Sources are stored as JSON, so this is a Go-side check only.
func validProvider(s string) bool {
	if len(s) < 1 || len(s) > 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

// The on-chain validators mirror checkAddress and checkMarkShape.

// ValidAddress reports whether s is 14 to 90 bytes of [a-z0-9] starting bc1, tb1 or bcrt1.
func ValidAddress(s string) bool {
	if len(s) < 14 || len(s) > 90 {
		return false
	}
	if !strings.HasPrefix(s, "bc1") && !strings.HasPrefix(s, "tb1") && !strings.HasPrefix(s, "bcrt1") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('a' <= c && c <= 'z' || '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}

func validSat(n int64) bool { return n >= 1 && n <= MaxSat }

// validPiconero: 1 to MaxPiconero (2^53 − 1); the SQL CHECKs of orders.amount_piconero and
// monero_payments.amount_piconero are its mirrors.
func validPiconero(n int64) bool { return n >= 1 && n <= MaxPiconero }

// ValidSubaddress reports whether s has the shape of a Monero subaddress: 95 bytes of the base58 alphabet
// (123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz: no 0, O, I or l), starting 8 (mainnet, regtest), B
// (testnet) or 7 (stagenet). It is never decoded: wallet-rpc issues the subaddresses. checkSubaddress is its SQL
// mirror.
func ValidSubaddress(s string) bool {
	if len(s) != 95 || (s[0] != '8' && s[0] != 'B' && s[0] != '7') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isBase58(s[i]) {
			return false
		}
	}
	return true
}

func isBase58(c byte) bool {
	switch {
	case '1' <= c && c <= '9':
		return true
	case 'A' <= c && c <= 'Z':
		return c != 'I' && c != 'O'
	case 'a' <= c && c <= 'z':
		return c != 'l'
	}
	return false
}

// validMarkShape mirrors checkMarkShape.
func validMarkShape(kind string, height int64, hash string) bool {
	switch kind {
	case MarkMined:
		return height >= 1 && validLowerHex(hash, 64)
	case MarkMempool, MarkDead:
		return height == 0 && hash == ""
	}
	return false
}
