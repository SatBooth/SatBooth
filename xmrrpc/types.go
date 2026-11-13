package xmrrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// The typed calls. Shapes come from wallet-rpc v0.18.5.4 (src/wallet/wallet_rpc_server_commands_defs.h).
// Decoding ignores fields it does not know; a field of the wrong JSON type, or a missing one the caller
// relies on, is ErrBadAnswer, except inside get_transfers' lists and get_transfer_by_txid's "transfers",
// which are decoded entry by entry: an odd entry is dropped, never the answer. epee writes no key for an
// empty list, so a missing list is an empty one.

// maxHeight is CRYPTONOTE_MAX_BLOCK_NUMBER (src/cryptonote_config.h), get_transfers' default max_height.
const maxHeight = 500_000_000

// Addresses is get_address' answer.
type Addresses struct {
	Address   string // the account's primary address (account 0) or its first one
	Addresses []AddressEntry
}

// AddressEntry is one entry of get_address' "addresses".
type AddressEntry struct {
	Address, Label string
	Index          uint32 // address_index
	Used           bool
}

// Created is create_address' answer.
type Created struct {
	Address string
	Index   uint32
}

// TransfersQuery is get_transfers' request. out, pending and failed are always false; MaxHeight 0 is sent as
// 500,000,000 (CRYPTONOTE_MAX_BLOCK_NUMBER).
type TransfersQuery struct {
	In, Pool       bool
	Account        uint32
	FilterByHeight bool
	MinHeight      int64
	MaxHeight      int64
}

// Transfer is one incoming entry (type "in" or "pool") of get_transfers or get_transfer_by_txid.
type Transfer struct {
	TxID            string // 64 lowercase hex
	Address         string
	Account         uint32 // subaddr_index.major
	Index           uint32 // subaddr_index.minor
	Amount          int64  // piconero, 1 to 2^63-1
	Height          int64  // 0 for a pool entry
	UnlockTime      int64  // as sent; a value above 2^63-1 reads as 2^63-1 (still a lock)
	Locked          bool
	DoubleSpendSeen bool
	Type            string // "in" or "pool"
	// Confirmations is wallet-rpc's own count, 0 to 500,000,000: the wallet's height less the entry's height, 0 for a
	// pool entry or one at or above the wallet's height (set_confirmations, src/wallet/wallet_rpc_server.cpp:163-168
	// at v0.18.5.4), computed in the same call as the entry, so it is bound to this observation (ruling 8-A6). wallet-rpc
	// leaves the key out when it is 0 (KV_SERIALIZE_OPT, wallet_rpc_server_commands_defs.h:1486), read as 0.
	Confirmations int64
}

// Transfers is get_transfers' answer. Dropped counts the entries of both lists that were left out, except a
// well-formed coinbase in "in" (type "block": fill_transfer_entry, src/wallet/wallet_rpc_server.cpp:520, tag
// v0.18.5.4), which is skipped without being counted.
type Transfers struct {
	In, Pool []Transfer
	Dropped  int
}

// GetVersion calls get_version.
func (c *Client) GetVersion(ctx context.Context) (version uint32, err error) {
	raw, err := c.rpc(ctx, "get_version", nil)
	if err != nil {
		return 0, err
	}
	return decodeVersion(raw)
}

// GetHeight calls get_height: the wallet's height, which can be below the daemon's.
func (c *Client) GetHeight(ctx context.Context) (int64, error) {
	raw, err := c.rpc(ctx, "get_height", nil)
	if err != nil {
		return 0, err
	}
	return decodeHeight(raw)
}

// GetAddress calls get_address for account and the given minor indices (nil: all of them).
func (c *Client) GetAddress(ctx context.Context, account uint32, indices []uint32) (Addresses, error) {
	if indices == nil {
		indices = []uint32{}
	}
	raw, err := c.rpc(ctx, "get_address", map[string]any{"account_index": account, "address_index": indices})
	if err != nil {
		return Addresses{}, err
	}
	return decodeAddresses(raw)
}

// CreateAddress calls create_address for one new subaddress of account. wallet-rpc does not save the wallet
// for it: the caller calls Store.
func (c *Client) CreateAddress(ctx context.Context, account uint32, label string) (Created, error) {
	raw, err := c.rpc(ctx, "create_address", map[string]any{"account_index": account, "label": label})
	if err != nil {
		return Created{}, err
	}
	return decodeCreated(raw)
}

// Store calls store: wallet-rpc writes the wallet file.
func (c *Client) Store(ctx context.Context) error {
	_, err := c.rpc(ctx, "store", nil)
	return err
}

// GetTransfers calls get_transfers with out, pending and failed false.
func (c *Client) GetTransfers(ctx context.Context, q TransfersQuery) (Transfers, error) {
	if q.MinHeight < 0 || q.MinHeight > maxHeight || q.MaxHeight < 0 || q.MaxHeight > maxHeight {
		return Transfers{}, fmt.Errorf("%w: height", ErrArgument)
	}
	max := q.MaxHeight
	if max == 0 {
		max = maxHeight
	}
	raw, err := c.rpc(ctx, "get_transfers", map[string]any{
		"in": q.In, "out": false, "pending": false, "failed": false, "pool": q.Pool,
		"filter_by_height": q.FilterByHeight, "min_height": q.MinHeight, "max_height": max, "account_index": q.Account,
	})
	if err != nil {
		return Transfers{}, err
	}
	return decodeTransfers(raw)
}

// codeTxNotFound is WALLET_RPC_ERROR_CODE_WRONG_TXID (-8, src/wallet/wallet_rpc_server_error_codes.h:41), which
// on_get_transfer_by_txid answers with "Transaction not found." when the wallet has no entry for the
// transaction (src/wallet/wallet_rpc_server.cpp:3069-3070, tag v0.18.5.4). The same handler gives -8 for a
// txid that is not 64 hex digits (:2993-3008), which GetTransferByTxID never sends, so -8 means not found.
// Every other code (-13 for no open wallet, recorded from wallet-rpc v0.18.5.4; -3 for a busy daemon; -14 for
// a bad account) is an error.
const codeTxNotFound = -8

// GetTransferByTxID calls get_transfer_by_txid and returns the in and pool entries of its "transfers" (one
// per subaddress and direction), not "transfer", which is the first entry only. A transaction wallet-rpc
// does not know (the code -8 only) gives an empty slice and no error, and so does a known transaction with
// no incoming entry; a success with no entries is ErrBadAnswer (decodeTransferList).
func (c *Client) GetTransferByTxID(ctx context.Context, txid string, account uint32) ([]Transfer, error) {
	if !isTxid(txid) {
		return nil, fmt.Errorf("%w: txid", ErrArgument)
	}
	raw, err := c.rpc(ctx, "get_transfer_by_txid", map[string]any{"txid": txid, "account_index": account})
	var e *Error
	if errors.As(err, &e) && e.Code == codeTxNotFound {
		return []Transfer{}, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeTransferList(raw)
}

// SignTransferProbe calls sign_transfer with an empty unsigned_txset: the view-only check. A view-only
// wallet refuses with WALLET_RPC_ERROR_CODE_WATCH_ONLY (-29) before reading the set. It returns the JSON-RPC
// error's code, or 0 for a success (which the caller reads as "not view-only"); any other failure is err.
func (c *Client) SignTransferProbe(ctx context.Context) (code int, err error) {
	_, err = c.rpc(ctx, "sign_transfer", map[string]any{"unsigned_txset": "", "export_raw": false})
	var e *Error
	if errors.As(err, &e) {
		return e.Code, nil
	}
	return 0, err
}

// --- decoders: each takes the "result" value and is pure, so it can be fuzzed ---

func decodeObject(raw []byte, v any, what string) error {
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		return bad(what + ": not an object")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return bad(what + ": wrong field type")
	}
	return nil
}

func decodeVersion(raw []byte) (uint32, error) {
	var a struct {
		Version *uint32 `json:"version"`
	}
	if err := decodeObject(raw, &a, "get_version"); err != nil {
		return 0, err
	}
	if a.Version == nil {
		return 0, bad("get_version")
	}
	return *a.Version, nil
}

func decodeHeight(raw []byte) (int64, error) {
	var a struct {
		Height *int64 `json:"height"`
	}
	if err := decodeObject(raw, &a, "get_height"); err != nil {
		return 0, err
	}
	if a.Height == nil || *a.Height < 0 || *a.Height > maxHeight {
		return 0, bad("get_height")
	}
	return *a.Height, nil
}

func decodeAddresses(raw []byte) (Addresses, error) {
	var a struct {
		Address   *string `json:"address"`
		Addresses []struct {
			Address *string `json:"address"`
			Label   string  `json:"label"`
			Index   *uint32 `json:"address_index"`
			Used    bool    `json:"used"`
		} `json:"addresses"`
	}
	if err := decodeObject(raw, &a, "get_address"); err != nil {
		return Addresses{}, err
	}
	if a.Address == nil || *a.Address == "" {
		return Addresses{}, bad("get_address")
	}
	out := Addresses{Address: *a.Address, Addresses: make([]AddressEntry, 0, len(a.Addresses))}
	for _, e := range a.Addresses {
		if e.Address == nil || *e.Address == "" || e.Index == nil {
			return Addresses{}, bad("get_address")
		}
		out.Addresses = append(out.Addresses, AddressEntry{Address: *e.Address, Label: e.Label, Index: *e.Index, Used: e.Used})
	}
	return out, nil
}

func decodeCreated(raw []byte) (Created, error) {
	var a struct {
		Address *string `json:"address"`
		Index   *uint32 `json:"address_index"`
	}
	if err := decodeObject(raw, &a, "create_address"); err != nil {
		return Created{}, err
	}
	if a.Address == nil || *a.Address == "" || a.Index == nil {
		return Created{}, bad("create_address")
	}
	return Created{Address: *a.Address, Index: *a.Index}, nil
}

func isTxid(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// parseUint reads a JSON number that is a plain non-negative integer (digits only, no leading zero, no
// sign, fraction or exponent) into a uint64.
func parseUint(raw json.RawMessage) (uint64, bool) {
	s := string(raw)
	if s == "" || len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(s, 10, 64)
	return v, err == nil
}

// transferFromEntry decodes one transfer_entry; ok is false for any entry SatBooth must not use: a field
// missing or of the wrong type, a txid that is not 64 lowercase hex, an amount outside 1 to 2^63-1, a height
// outside 0 to 500,000,000, an unlock_time that is not an integer of 64 bits, confirmations that are present and not
// an integer of 0 to 500,000,000 (set_confirmations, wallet_rpc_server.cpp:163-168 at v0.18.5.4; a missing key is 0:
// KV_SERIALIZE_OPT(confirmations, (uint64_t)0), wallet_rpc_server_commands_defs.h:1486, leaves the key out when the
// value is 0, as for every pool entry), or a type other than want (want "" accepts "in" and "pool").
func transferFromEntry(raw json.RawMessage, want string) (Transfer, bool) {
	var a struct {
		TxID     *string         `json:"txid"`
		Address  *string         `json:"address"`
		Amount   json.RawMessage `json:"amount"`
		Height   *int64          `json:"height"`
		Unlock   json.RawMessage `json:"unlock_time"`
		Locked   *bool           `json:"locked"`
		DSS      *bool           `json:"double_spend_seen"`
		Type     *string         `json:"type"`
		Confs    json.RawMessage `json:"confirmations"`
		Subaddrs *struct {
			Major *uint32 `json:"major"`
			Minor *uint32 `json:"minor"`
		} `json:"subaddr_index"`
	}
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' || json.Unmarshal(raw, &a) != nil {
		return Transfer{}, false
	}
	if a.TxID == nil || a.Address == nil || a.Height == nil || a.Locked == nil || a.DSS == nil || a.Type == nil ||
		a.Subaddrs == nil || a.Subaddrs.Major == nil || a.Subaddrs.Minor == nil {
		return Transfer{}, false
	}
	if !isTxid(*a.TxID) || *a.Height < 0 || *a.Height > maxHeight {
		return Transfer{}, false
	}
	if want == "" && *a.Type != "in" && *a.Type != "pool" || want != "" && *a.Type != want {
		return Transfer{}, false
	}
	amount, ok := parseUint(a.Amount)
	if !ok || amount < 1 || amount > math.MaxInt64 {
		return Transfer{}, false
	}
	unlock, ok := parseUint(a.Unlock)
	if !ok {
		return Transfer{}, false
	}
	confs := uint64(0)
	if a.Confs != nil {
		if confs, ok = parseUint(a.Confs); !ok || confs > maxHeight {
			return Transfer{}, false
		}
	}
	return Transfer{
		TxID: *a.TxID, Address: *a.Address, Account: *a.Subaddrs.Major, Index: *a.Subaddrs.Minor,
		Amount: int64(amount), Height: *a.Height, UnlockTime: int64(min(unlock, math.MaxInt64)),
		Locked: *a.Locked, DoubleSpendSeen: *a.DSS, Type: *a.Type, Confirmations: int64(confs),
	}, true
}

func decodeTransfers(raw []byte) (Transfers, error) {
	var a struct {
		In   []json.RawMessage `json:"in"`
		Pool []json.RawMessage `json:"pool"`
	}
	if err := decodeObject(raw, &a, "get_transfers"); err != nil {
		return Transfers{}, err
	}
	var out Transfers
	list := func(entries []json.RawMessage, want string) []Transfer {
		var l []Transfer
		for _, e := range entries {
			if tr, ok := transferFromEntry(e, want); ok {
				l = append(l, tr)
				continue
			}
			if want == "in" {
				if _, ok := transferFromEntry(e, "block"); ok {
					continue // a coinbase: not a payment, and not an entry SatBooth could not read
				}
			}
			out.Dropped++
		}
		return l
	}
	out.In = list(a.In, "in")
	out.Pool = list(a.Pool, "pool")
	return out, nil
}

// decodeTransferList decodes get_transfer_by_txid's "transfers", keeping the in and pool entries. wallet-rpc
// answers success only with a non-empty "transfers" (src/wallet/wallet_rpc_server.cpp:3063-3067; otherwise
// it answers -8), so a success with "transfers" missing or empty is ErrBadAnswer, never "not found", and so
// is one whose entries are all malformed. An entry that is well formed but outgoing (out, pending, failed) is
// skipped: a transaction with no incoming entry gives an empty slice. A malformed entry beside a usable one
// is dropped, entry by entry.
func decodeTransferList(raw []byte) ([]Transfer, error) {
	var a struct {
		Transfers []json.RawMessage `json:"transfers"`
	}
	if err := decodeObject(raw, &a, "get_transfer_by_txid"); err != nil {
		return nil, err
	}
	if len(a.Transfers) == 0 {
		return nil, bad("get_transfer_by_txid: no transfers in a success")
	}
	out := []Transfer{}
	malformed := 0
	for _, e := range a.Transfers {
		if tr, ok := transferFromEntry(e, ""); ok {
			out = append(out, tr)
			continue
		}
		outgoing := false
		for _, typ := range []string{"out", "pending", "failed"} {
			if _, ok := transferFromEntry(e, typ); ok {
				outgoing = true
			}
		}
		if !outgoing {
			malformed++
		}
	}
	if len(out) == 0 && malformed > 0 {
		return nil, bad("get_transfer_by_txid: every entry malformed")
	}
	return out, nil
}
