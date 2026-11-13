// Package monerotest is an in-memory monero-wallet-rpc for tests: a view-only wallet over a small chain it
// simulates, answering the typed calls of xmrrpc the way wallet-rpc v0.18.5.4 does (the shapes of
// src/wallet/wallet_rpc_server.cpp at that tag, cited where a rule matters). It is test support: it uses the
// standard library and xmrrpc only, and it does not import package monero, so monero's own tests can import it.
package monerotest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"

	"github.com/satbooth/satbooth/xmrrpc"
)

const (
	base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

	// version is wallet-rpc v0.18.5.4's: MAKE_WALLET_RPC_VERSION(1, 32), wallet_rpc_server_commands_defs.h:49-52.
	version = 1<<16 | 32
	// spendableAge is CRYPTONOTE_DEFAULT_TX_SPENDABLE_AGE (src/cryptonote_config.h:49): a mined entry reads locked
	// while height + 10 > the wallet's height.
	spendableAge = 10
	// maxBlockNumber is CRYPTONOTE_MAX_BLOCK_NUMBER (src/cryptonote_config.h:40): an unlock_time below it is a
	// height, at or above it a timestamp.
	maxBlockNumber = 500_000_000
	// lookahead is the minor-index lookahead past the created subaddresses within which the wallet still finds a
	// payment (wallet2's default subaddress lookahead, 200 minor indices).
	lookahead = 200

	codeWatchOnly  = -29 // WALLET_RPC_ERROR_CODE_WATCH_ONLY, wallet_rpc_server_error_codes.h:62
	codeUnknown    = -1  // WALLET_RPC_ERROR_CODE_UNKNOWN_ERROR, what a full wallet's probe answers here
	codeIndexRange = -15 // WALLET_RPC_ERROR_CODE_ADDRESS_INDEX_OUT_OF_BOUNDS, wallet_rpc_server_error_codes.h:48
)

type out struct {
	addr   string
	amount int64
}

type tx struct {
	id     string
	outs   []out
	unlock int64
	height int64 // the block it is mined in; 0 in the pool
	gone   bool  // dropped from the pool, or popped and dropped: the wallet does not know it
	dss    bool  // double_spend_seen, for a pool entry
}

type subIdx struct{ account, minor uint32 }

// Wallet implements monero.RPC in memory. It is safe for concurrent use.
type Wallet struct {
	mu     sync.Mutex
	seed   int64
	counts map[uint32]uint32 // subaddresses per account, index 0 included; an absent account has 1
	labels map[subIdx]string
	owner  map[string]subIdx // every address derived so far, for finding the wallet's payments
	upTo   map[uint32]uint32 // owner holds account a's indexes below upTo[a]

	height      int64 // the chain's height (blocks, the genesis included): blocks are 0 to height-1
	reported    int64 // SetHeight's value, -1 for the chain's height
	forgetAbove int64 // ForgetPayments' height, -1 for none
	full        bool
	txs         map[string]*tx
	order       []string
	txSeq       int

	calls   map[string]int
	txCalls map[string]int
	fails   map[string]error
	hangs   map[string]bool
	txFails map[string]error
}

// New returns a healthy view-only wallet at height 1 whose addresses follow from seed.
func New(seed int64) *Wallet {
	return &Wallet{seed: seed, counts: map[uint32]uint32{}, labels: map[subIdx]string{}, owner: map[string]subIdx{},
		upTo: map[uint32]uint32{}, height: 1, reported: -1, forgetAbove: -1, txs: map[string]*tx{},
		calls: map[string]int{}, txCalls: map[string]int{}, fails: map[string]error{}, hangs: map[string]bool{},
		txFails: map[string]error{}}
}

// derive is an address of mainnet shape: first, then 94 characters of the base58 alphabet drawn from SHA-256 of the
// seed and the parts.
func (w *Wallet) derive(first byte, parts ...uint32) string {
	b := make([]byte, 0, 95)
	b = append(b, first)
	for block := uint32(0); len(b) < 95; block++ {
		h := sha256.New()
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(w.seed))
		h.Write([]byte("monerotest address"))
		h.Write(n[:])
		for _, p := range append(parts, block) {
			binary.BigEndian.PutUint32(n[:4], p)
			h.Write(n[:4])
		}
		for _, c := range h.Sum(nil) {
			if len(b) == 95 {
				break
			}
			b = append(b, base58[int(c)%len(base58)])
		}
	}
	return string(b)
}

func (w *Wallet) addressOf(account, minor uint32) string {
	if account == 0 && minor == 0 {
		return w.derive('4', 0)
	}
	return w.derive('8', 1, account, minor)
}

// Primary is the wallet's primary address (account 0, index 0).
func (w *Wallet) Primary() string { return w.addressOf(0, 0) }

// SubaddressAt is account 0's address at minor index i (the primary address for 0).
func (w *Wallet) SubaddressAt(i uint32) string { return w.addressOf(0, i) }

func (w *Wallet) count(account uint32) uint32 {
	if c, ok := w.counts[account]; ok {
		return c
	}
	return 1
}

// find reports which of the wallet's addresses addr is, within the lookahead of every account it has used.
func (w *Wallet) find(addr string) (subIdx, bool) {
	accounts := []uint32{0}
	for a := range w.counts {
		if a != 0 {
			accounts = append(accounts, a)
		}
	}
	for _, a := range accounts {
		for want := w.count(a) + lookahead; w.upTo[a] < want; w.upTo[a]++ {
			w.owner[w.addressOf(a, w.upTo[a])] = subIdx{a, w.upTo[a]}
		}
	}
	s, ok := w.owner[addr]
	if ok && s.minor >= w.count(s.account)+lookahead {
		return subIdx{}, false
	}
	return s, ok
}

// --- test controls ---

// Fail makes the next call of method (the RPC name, such as "create_address") return err, once. For
// "sign_transfer", an *xmrrpc.Error becomes the probe's code, as the client gives it.
func (w *Wallet) Fail(method string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fails[method] = err
}

// Hang makes every call of method block until its context ends, then fail as a transport error.
func (w *Wallet) Hang(method string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hangs[method] = true
}

// Unhang undoes Hang.
func (w *Wallet) Unhang(method string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.hangs, method)
}

// FailTx makes every get_transfer_by_txid call for txid return err; a nil err undoes it.
func (w *Wallet) FailTx(txid string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err == nil {
		delete(w.txFails, txid)
		return
	}
	w.txFails[txid] = err
}

// Calls is the number of calls of method so far, failed and hung ones included.
func (w *Wallet) Calls(method string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[method]
}

// TxCalls is the number of get_transfer_by_txid calls for txid so far, failed and hung ones included.
func (w *Wallet) TxCalls(txid string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.txCalls[txid]
}

// Full makes the wallet a full one: the probe answers -1 instead of -29.
func (w *Wallet) Full(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.full = on
}

// Forget makes the wallet forget the last n subaddresses of every account (never index 0): what a wallet-rpc
// killed after create_address and before store loads from its file.
func (w *Wallet) Forget(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for a, c := range w.counts {
		w.counts[a] = uint32(max(int64(c)-int64(n), 1))
	}
}

// SetHeight sets the height get_height reports and the lock rule reads (a wallet behind the chain); a negative h
// goes back to the chain's height.
func (w *Wallet) SetHeight(h int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reported = max(h, -1)
}

// ForgetPayments makes the wallet forget the payments mined above height (a wallet behind SatBooth): get_transfers
// does not list them and get_transfer_by_txid does not know them. A negative height undoes it.
func (w *Wallet) ForgetPayments(above int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.forgetAbove = max(above, -1)
}

// --- the chain ---

// Height is the chain's height: the number of blocks, the genesis included (1 for a new wallet).
func (w *Wallet) Height() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.height
}

func (w *Wallet) newTx(outs []out, unlock int64) string {
	w.txSeq++
	var n [16]byte
	binary.BigEndian.PutUint64(n[:8], uint64(w.seed))
	binary.BigEndian.PutUint64(n[8:], uint64(w.txSeq))
	sum := sha256.Sum256(append([]byte("monerotest tx"), n[:]...))
	id := hex.EncodeToString(sum[:])
	w.txs[id] = &tx{id: id, outs: outs, unlock: unlock}
	w.order = append(w.order, id)
	return id
}

// Pay puts a transaction paying piconero to address in the pool and returns its txid.
func (w *Wallet) Pay(address string, piconero int64) string {
	return w.PayLocked(address, piconero, 0)
}

// PayLocked is Pay with the transaction's unlock_time (a height below 500,000,000, else a timestamp, which the fake
// keeps locked).
func (w *Wallet) PayLocked(address string, piconero, unlockTime int64) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.newTx([]out{{address, piconero}}, unlockTime)
}

// PayMany puts one transaction paying each address its amount in the pool and returns its txid.
func (w *Wallet) PayMany(outs map[string]int64) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	l := make([]out, 0, len(outs))
	for a, v := range outs {
		l = append(l, out{a, v})
	}
	slices.SortFunc(l, func(a, b out) int {
		switch {
		case a.addr < b.addr:
			return -1
		case a.addr > b.addr:
			return 1
		}
		return 0
	})
	return w.newTx(l, 0)
}

// Mine adds n blocks; the pool's transactions are mined in the first of them.
func (w *Wallet) Mine(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n < 1 {
		return
	}
	for _, id := range w.order {
		if t := w.txs[id]; !t.gone && t.height == 0 {
			t.height, t.dss = w.height, false
		}
	}
	w.height += int64(n)
}

// MineEmpty adds n blocks without mining the pool: with Pop, a reorg that replaces blocks at the same height by ones
// without their transactions, which stay in the pool.
func (w *Wallet) MineEmpty(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.height += int64(max(n, 0))
}

func (w *Wallet) pop(n int, drop bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n = int(min(int64(max(n, 0)), w.height-1))
	w.height -= int64(n)
	for _, t := range w.txs {
		if t.height >= w.height {
			t.height = 0
			t.gone = drop
		}
	}
}

// Pop removes the last n blocks (never the genesis); their transactions return to the pool.
func (w *Wallet) Pop(n int) { w.pop(n, false) }

// PopDrop removes the last n blocks; their transactions vanish.
func (w *Wallet) PopDrop(n int) { w.pop(n, true) }

// Drop takes a pool transaction out of the pool: the wallet no longer knows it.
func (w *Wallet) Drop(txid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t := w.txs[txid]; t != nil && t.height == 0 {
		t.gone = true
	}
}

// DoubleSpend marks a pool transaction's entry double_spend_seen.
func (w *Wallet) DoubleSpend(txid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t := w.txs[txid]; t != nil && !t.gone && t.height == 0 {
		t.dss = true
	}
}

// Rebroadcast puts a dropped transaction back in the pool.
func (w *Wallet) Rebroadcast(txid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t := w.txs[txid]; t != nil && t.gone {
		t.gone, t.dss, t.height = false, false, 0
	}
}

// --- the typed calls ---

// enter counts the call and applies Fail, FailTx and Hang. It returns with the lock held, or with an error and
// without it.
func (w *Wallet) enter(ctx context.Context, method, txid string) error {
	w.mu.Lock()
	w.calls[method]++
	if txid != "" {
		w.txCalls[txid]++
	}
	if err := w.fails[method]; err != nil {
		delete(w.fails, method)
		w.mu.Unlock()
		return err
	}
	if err := w.txFails[txid]; txid != "" && err != nil {
		w.mu.Unlock()
		return err
	}
	if w.hangs[method] {
		w.mu.Unlock()
		<-ctx.Done()
		return fmt.Errorf("%w: %s: %w", xmrrpc.ErrTransport, method, ctx.Err())
	}
	return nil
}

func rpcErr(method string, code int) error { return &xmrrpc.Error{Code: code, Method: method} }

// GetVersion answers wallet-rpc v0.18.5.4's version, 1.32.
func (w *Wallet) GetVersion(ctx context.Context) (uint32, error) {
	if err := w.enter(ctx, "get_version", ""); err != nil {
		return 0, err
	}
	defer w.mu.Unlock()
	return version, nil
}

func (w *Wallet) walletHeight() int64 {
	if w.reported >= 0 {
		return w.reported
	}
	return w.height
}

// GetHeight answers the wallet's height (SetHeight's, or the chain's).
func (w *Wallet) GetHeight(ctx context.Context) (int64, error) {
	if err := w.enter(ctx, "get_height", ""); err != nil {
		return 0, err
	}
	defer w.mu.Unlock()
	return w.walletHeight(), nil
}

// GetAddress answers the account's base address (index 0) and the entries of indices (nil or empty: every
// subaddress the wallet has). An index the wallet does not have is -15, as on_getaddress throws
// address_index_outofbound (wallet_rpc_server.cpp:697, mapped at :3860-3862). Every account exists.
func (w *Wallet) GetAddress(ctx context.Context, account uint32, indices []uint32) (xmrrpc.Addresses, error) {
	if err := w.enter(ctx, "get_address", ""); err != nil {
		return xmrrpc.Addresses{}, err
	}
	defer w.mu.Unlock()
	c := w.count(account)
	if len(indices) == 0 {
		indices = make([]uint32, c)
		for i := range indices {
			indices[i] = uint32(i)
		}
	}
	r := xmrrpc.Addresses{Address: w.addressOf(account, 0)}
	for _, i := range indices {
		if i >= c {
			return xmrrpc.Addresses{}, rpcErr("get_address", codeIndexRange)
		}
		a := w.addressOf(account, i)
		r.Addresses = append(r.Addresses, xmrrpc.AddressEntry{Address: a, Label: w.labels[subIdx{account, i}], Index: i, Used: w.used(a)})
	}
	return r, nil
}

func (w *Wallet) used(addr string) bool {
	for _, t := range w.txs {
		if w.known(t) && t.height > 0 {
			for _, o := range t.outs {
				if o.addr == addr {
					return true
				}
			}
		}
	}
	return false
}

// CreateAddress adds the account's next subaddress, as on_create_address does with count 1
// (wallet_rpc_server.cpp:761-781): its index is the number the account had.
func (w *Wallet) CreateAddress(ctx context.Context, account uint32, label string) (xmrrpc.Created, error) {
	if err := w.enter(ctx, "create_address", ""); err != nil {
		return xmrrpc.Created{}, err
	}
	defer w.mu.Unlock()
	i := w.count(account)
	w.counts[account] = i + 1
	w.labels[subIdx{account, i}] = label
	return xmrrpc.Created{Address: w.addressOf(account, i), Index: i}, nil
}

// Store saves the wallet (it only counts the call).
func (w *Wallet) Store(ctx context.Context) error {
	if err := w.enter(ctx, "store", ""); err != nil {
		return err
	}
	w.mu.Unlock()
	return nil
}

// known reports whether the wallet knows t: not gone, and not mined above ForgetPayments' height.
func (w *Wallet) known(t *tx) bool {
	return !t.gone && !(t.height > 0 && w.forgetAbove >= 0 && t.height > w.forgetAbove)
}

// entries are t's incoming entries for account, one per subaddress it pays (amounts to one subaddress summed),
// by minor index.
func (w *Wallet) entries(t *tx, account uint32) []xmrrpc.Transfer {
	var l []xmrrpc.Transfer
	at := map[string]int{}
	for _, o := range t.outs {
		s, ok := w.find(o.addr)
		if !ok || s.account != account {
			continue
		}
		if k, ok := at[o.addr]; ok {
			l[k].Amount += o.amount
			continue
		}
		e := xmrrpc.Transfer{TxID: t.id, Address: o.addr, Account: s.account, Index: s.minor, Amount: o.amount,
			UnlockTime: t.unlock, Type: "pool", Locked: true, DoubleSpendSeen: t.dss}
		if t.height > 0 {
			// fill_transfer_entry (wallet_rpc_server.cpp:517): locked unless is_transfer_unlocked.
			e.Type, e.Height, e.DoubleSpendSeen = "in", t.height, false
			e.Locked = t.height+spendableAge > w.walletHeight() || w.lockedBy(t.unlock)
			// set_confirmations (wallet_rpc_server.cpp:163-168): the wallet's height less the entry's, at the call.
			if wh := w.walletHeight(); t.height < wh {
				e.Confirmations = wh - t.height
			}
		}
		at[o.addr] = len(l)
		l = append(l, e)
	}
	slices.SortFunc(l, func(a, b xmrrpc.Transfer) int { return int(int64(a.Index) - int64(b.Index)) })
	return l
}

// lockedBy is the sender's lock: a height form is locked while the wallet's height is below it (wallet2's
// is_tx_spendtime_unlocked with CRYPTONOTE_LOCKED_TX_ALLOWED_DELTA_BLOCKS 1); the fake keeps a timestamp form locked.
func (w *Wallet) lockedBy(u int64) bool {
	switch {
	case u == 0:
		return false
	case u < maxBlockNumber:
		return w.walletHeight() < u
	}
	return true
}

// GetTransfers lists the incoming entries of the account: "in" for mined transactions (above MinHeight and at most
// MaxHeight when FilterByHeight, as wallet2::get_payments filters, min_height exclusive; MaxHeight 0 is no bound)
// and "pool" for pool ones, in the order the transactions were made.
func (w *Wallet) GetTransfers(ctx context.Context, q xmrrpc.TransfersQuery) (xmrrpc.Transfers, error) {
	if err := w.enter(ctx, "get_transfers", ""); err != nil {
		return xmrrpc.Transfers{}, err
	}
	defer w.mu.Unlock()
	var r xmrrpc.Transfers
	for _, id := range w.order {
		t := w.txs[id]
		if !w.known(t) {
			continue
		}
		switch {
		case t.height > 0 && q.In:
			if q.FilterByHeight && (t.height <= q.MinHeight || q.MaxHeight > 0 && t.height > q.MaxHeight) {
				continue
			}
			r.In = append(r.In, w.entries(t, q.Account)...)
		case t.height == 0 && q.Pool:
			r.Pool = append(r.Pool, w.entries(t, q.Account)...)
		}
	}
	return r, nil
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

// GetTransferByTxID answers one entry per subaddress of the account that the transaction pays, and an empty slice
// for a transaction the wallet does not know (dropped, popped and dropped, forgotten, or never seen), as the client
// turns wallet-rpc's -8 into one.
func (w *Wallet) GetTransferByTxID(ctx context.Context, txid string, account uint32) ([]xmrrpc.Transfer, error) {
	if !isTxid(txid) {
		return nil, fmt.Errorf("%w: txid", xmrrpc.ErrArgument)
	}
	if err := w.enter(ctx, "get_transfer_by_txid", txid); err != nil {
		return nil, err
	}
	defer w.mu.Unlock()
	t := w.txs[txid]
	if t == nil || !w.known(t) {
		return []xmrrpc.Transfer{}, nil
	}
	l := w.entries(t, account)
	if l == nil {
		l = []xmrrpc.Transfer{}
	}
	return l, nil
}

// SignTransferProbe answers -29 (WALLET_RPC_ERROR_CODE_WATCH_ONLY), or -1 for a full wallet (Full). A Fail with an
// *xmrrpc.Error (itself, not wrapped) gives its code and no error, as the client does.
func (w *Wallet) SignTransferProbe(ctx context.Context) (int, error) {
	if err := w.enter(ctx, "sign_transfer", ""); err != nil {
		if e, ok := err.(*xmrrpc.Error); ok { // a type assertion, not errors.As: the fake keeps no capability
			return e.Code, nil
		}
		return 0, err
	}
	defer w.mu.Unlock()
	if w.full {
		return codeUnknown, nil
	}
	return codeWatchOnly, nil
}
