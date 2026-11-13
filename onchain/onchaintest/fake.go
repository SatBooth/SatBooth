// Package onchaintest is an in-memory bitcoind for tests: it answers the typed calls of btcrpc the way Bitcoin Core
// v32.1 does, over a small chain it simulates. It is test support: it uses the standard library
// and btcrpc only, and it does not import package onchain, so onchain's own tests can import it.
package onchaintest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/satbooth/satbooth/btcrpc"
)

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// State is what a test may set (through Node.Update). New fills a healthy regtest wallet.
type State struct {
	Chain              string // getblockchaininfo "chain"
	HeadersAhead       int64  // Headers = Blocks + HeadersAhead
	IBD                bool
	WalletDescriptors  bool // getwalletinfo "descriptors"
	PrivateKeysEnabled bool
	HasPrivateKeys     bool                      // forces getdescriptorinfo's hasprivatekeys (otherwise derived from the input)
	Unranged           bool                      // forces getdescriptorinfo's isrange false (otherwise derived from the input)
	Descriptor         string                    // the normalised descriptor, with Core's checksum
	Records            []btcrpc.DescriptorRecord // when non-nil, listdescriptors answers these instead
	NextIndex          int64                     // the active descriptor's next_index
	AddressType        string                    // the only type getnewaddress serves: bech32 or bech32m
	HRP                string                    // "bcrt1", "tb1" or "bc1"
	HDPrefix           string                    // what precedes the index in hdkeypath: "m/0/" or "m/84h/1h/0h/0/"
	InfoHook           func(*btcrpc.AddressInfo) // may alter getaddressinfo's answer
	DeriveHook         func(index int64, addrs []string) []string
	AddressAfterHook   func(index int64) // runs inside getnewaddress after the index advanced, for crash tests
	MempoolLoading     bool              // getmempoolinfo loaded false, and getmempoolentry finds nothing (a restart)
}

type out struct {
	addr string
	sat  int64
}

// Out is an output of a replacement (ReplaceWith): sat paid to Address.
type Out struct {
	Address string
	Sat     int64
}

type tx struct {
	id         string
	outs       []out
	inputs     []string // the inputs it spends: fresh ones for a payment (PayWith), the given ones for Spend
	block      int64    // height it is mined at, or -1
	conflictAt int64    // height of the block that conflicts it, or -1
	inMempool  bool
}

type block struct {
	hash, parent string
	height       int64
	txids        []string
}

// held is the wallet's frozen view (HoldWallet): the transactions as they were, and in the order they were made.
type held struct {
	txs   map[string]*tx
	order []string
}

// Node implements btcrpc's typed calls in memory. It is safe for concurrent use.
//
// The wallet lag. A real bitcoind learns of mempool changes in its WALLET through a queue, so for a moment after a
// replacement its node (getmempoolentry, getmempoolinfo, gettxspendingprevout, getblockchaininfo) already knows what
// its wallet (listsinceblock, gettransaction) does not. HoldWallet freezes the wallet's view: from then on the wallet
// calls answer from the transactions as they were at that moment (a transaction made later is unknown to the wallet, as
// gettransaction's -5; its confirmations, mempoolconflicts, walletconflicts and inputs are the frozen ones), while the
// node calls stay live. ReleaseWallet ends the lag. Mine and Invalidate release it first: a real wallet call waits for
// the wallet to have processed the chain tip, so the lag exists only for mempool events since the last block.
type Node struct {
	mu       sync.Mutex
	s        State
	seed     int64
	chain    []*block
	detached map[string]*block
	txs      map[string]*tx
	order    []string
	salt     int
	known    map[string]int64
	knownTo  int64
	calls    map[string]int
	fails    map[string]error
	hangs    map[string]bool
	breaks   map[string]error
	txFails  map[string]error
	txHangs  map[string]bool
	txCalls  map[string]int
	frozen   *held
}

// New returns a healthy regtest node with a wpkh wallet whose descriptor string is desc. seed makes the addresses
// deterministic.
func New(desc string, seed int64) *Node {
	n := &Node{
		seed: seed, detached: map[string]*block{}, txs: map[string]*tx{}, known: map[string]int64{},
		calls: map[string]int{}, fails: map[string]error{}, hangs: map[string]bool{},
		breaks: map[string]error{}, txFails: map[string]error{}, txHangs: map[string]bool{}, txCalls: map[string]int{},
		s: State{Chain: "regtest", WalletDescriptors: true, Descriptor: desc, AddressType: "bech32",
			HRP: "bcrt1", HDPrefix: "m/0/"},
	}
	n.chain = []*block{{hash: n.blockHash(0), height: 0}}
	return n
}

// Update changes the state under the node's lock.
func (n *Node) Update(f func(*State)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f(&n.s)
	n.known, n.knownTo = map[string]int64{}, 0 // the address cache may be stale after any change
}

// Fail makes the next call of method (the RPC name, such as "getnewaddress") return err, once.
func (n *Node) Fail(method string, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.fails[method] = err
}

// Hang makes every call of method block until its context ends, then fail as a transport error.
func (n *Node) Hang(method string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.hangs[method] = true
}

// Unhang undoes Hang.
func (n *Node) Unhang(method string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.hangs, method)
}

// Break makes every call of method return err until Mend.
func (n *Node) Break(method string, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.breaks[method] = err
}

// Mend undoes Break.
func (n *Node) Mend(method string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.breaks, method)
}

// FailTx makes every gettransaction and getmempoolentry call for txid return err until MendTx.
func (n *Node) FailTx(txid string, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.txFails[txid] = err
}

// MendTx undoes FailTx.
func (n *Node) MendTx(txid string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.txFails, txid)
}

// HangTx makes every gettransaction and getmempoolentry call for txid block until its context ends, then fail as
// a transport error.
func (n *Node) HangTx(txid string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.txHangs[txid] = true
}

// UnhangTx undoes HangTx.
func (n *Node) UnhangTx(txid string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.txHangs, txid)
}

// TxCalls is the number of gettransaction calls for txid so far, failed and hung ones included.
func (n *Node) TxCalls(txid string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.txCalls[txid]
}

// Calls is the number of calls of method so far, failed and hung ones included.
func (n *Node) Calls(method string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls[method]
}

// HoldWallet freezes the wallet's view of the mempool (see Node). Holding again keeps the first freeze.
func (n *Node) HoldWallet() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.frozen != nil {
		return
	}
	f := &held{txs: make(map[string]*tx, len(n.txs)), order: append([]string(nil), n.order...)}
	for id, t := range n.txs {
		c := *t
		f.txs[id] = &c
	}
	n.frozen = f
}

// ReleaseWallet ends the lag: the wallet calls answer from the live state again.
func (n *Node) ReleaseWallet() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.frozen = nil
}

// walletView makes the node's transactions the wallet's frozen ones until the returned function runs. The lock is held.
func (n *Node) walletView() func() {
	if n.frozen == nil {
		return func() {}
	}
	txs, order := n.txs, n.order
	n.txs, n.order = n.frozen.txs, n.frozen.order
	return func() { n.txs, n.order = txs, order }
}

// enter counts the call and applies Fail, Break and Hang. It returns with the lock held, or with an error and
// without it.
func (n *Node) enter(ctx context.Context, method string) error { return n.enterTx(ctx, method, "") }

// enterTx is enter, and also applies FailTx and HangTx when txid is not empty.
func (n *Node) enterTx(ctx context.Context, method, txid string) error {
	n.mu.Lock()
	n.calls[method]++
	if txid != "" && method == "gettransaction" {
		n.txCalls[txid]++
	}
	if err := n.fails[method]; err != nil {
		delete(n.fails, method)
		n.mu.Unlock()
		return err
	}
	if err := n.breaks[method]; err != nil {
		n.mu.Unlock()
		return err
	}
	if err := n.txFails[txid]; txid != "" && err != nil {
		n.mu.Unlock()
		return err
	}
	if n.hangs[method] || (txid != "" && n.txHangs[txid]) {
		n.mu.Unlock()
		<-ctx.Done()
		return fmt.Errorf("%w: %s %w", btcrpc.ErrTransport, method, ctx.Err())
	}
	return nil
}

func rpcErr(method string, code int) error { return &btcrpc.Error{Code: code, Method: method} }

func (n *Node) blockHash(height int64) string {
	n.salt++
	sum := sha256.Sum256([]byte("block " + strconv.FormatInt(height, 10) + " " + strconv.Itoa(n.salt) + " " + strconv.FormatInt(n.seed, 10)))
	return hex.EncodeToString(sum[:])
}

// --- addresses ---

// AddressAt is the address at an index for the current state: HRP, "q" (bech32) or "p" (bech32m), then 36
// characters of the bech32 charset derived from SHA-256 of the node's seed and the index.
func (n *Node) AddressAt(i int64) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.addressAt(i)
}

func (n *Node) addressAt(i int64) string {
	sum := sha256.Sum256([]byte("address " + strconv.FormatInt(n.seed, 10) + " " + strconv.FormatInt(i, 10)))
	b := make([]byte, 36)
	for k := range b {
		b[k] = bech32Charset[int(sum[k%32]+byte(k/32))&31]
	}
	v := "q"
	if n.s.AddressType == "bech32m" {
		v = "p"
	}
	return n.s.HRP + v + string(b)
}

// mine reports the index of a wallet address (one the wallet watches: below next_index plus the 1000 look-ahead).
func (n *Node) mine(addr string) (int64, bool) {
	for n.knownTo < n.s.NextIndex+1000 {
		n.known[n.addressAt(n.knownTo)] = n.knownTo
		n.knownTo++
	}
	i, ok := n.known[addr]
	return i, ok
}

// checksum is a stand-in for Core's descriptor checksum: 8 bech32 characters of SHA-256 of the body.
func checksum(body string) string {
	sum := sha256.Sum256([]byte("checksum " + body))
	b := make([]byte, 8)
	for i := range b {
		b[i] = bech32Charset[int(sum[i])&31]
	}
	return string(b)
}

// Normalise is what the fake's getdescriptorinfo answers for a descriptor: the input without any checksum, then "#"
// and the stand-in checksum. Tests build the wallet's descriptor string with it.
func Normalise(desc string) string {
	body, _, _ := strings.Cut(desc, "#")
	return body + "#" + checksum(body)
}

// --- the typed calls ---

func (n *Node) GetBlockchainInfo(ctx context.Context) (btcrpc.BlockchainInfo, error) {
	if err := n.enter(ctx, "getblockchaininfo"); err != nil {
		return btcrpc.BlockchainInfo{}, err
	}
	defer n.mu.Unlock()
	tip := n.chain[len(n.chain)-1]
	return btcrpc.BlockchainInfo{Chain: n.s.Chain, Blocks: tip.height, Headers: tip.height + n.s.HeadersAhead,
		BestBlockHash: tip.hash, InitialBlockDownload: n.s.IBD}, nil
}

func (n *Node) GetWalletInfo(ctx context.Context) (btcrpc.WalletInfo, error) {
	if err := n.enter(ctx, "getwalletinfo"); err != nil {
		return btcrpc.WalletInfo{}, err
	}
	defer n.mu.Unlock()
	return btcrpc.WalletInfo{WalletName: "satbooth", Descriptors: n.s.WalletDescriptors, PrivateKeysEnabled: n.s.PrivateKeysEnabled}, nil
}

func (n *Node) GetDescriptorInfo(ctx context.Context, desc string) (btcrpc.DescriptorInfo, error) {
	if err := n.enter(ctx, "getdescriptorinfo"); err != nil {
		return btcrpc.DescriptorInfo{}, err
	}
	defer n.mu.Unlock()
	if desc == "" {
		return btcrpc.DescriptorInfo{}, rpcErr("getdescriptorinfo", -5)
	}
	body, _, _ := strings.Cut(desc, "#")
	return btcrpc.DescriptorInfo{Descriptor: Normalise(desc), Checksum: checksum(body), IsRange: strings.Contains(body, "*") && !n.s.Unranged,
		IsSolvable: true, HasPrivateKeys: strings.Contains(body, "prv") || n.s.HasPrivateKeys}, nil
}

func (n *Node) ListDescriptors(ctx context.Context) (btcrpc.Descriptors, error) {
	if err := n.enter(ctx, "listdescriptors"); err != nil {
		return btcrpc.Descriptors{}, err
	}
	defer n.mu.Unlock()
	recs := n.s.Records
	if recs == nil {
		recs = []btcrpc.DescriptorRecord{{Desc: n.s.Descriptor, Active: true, NextIndex: n.s.NextIndex, Range: [2]int64{0, n.s.NextIndex + 999}}}
	}
	return btcrpc.Descriptors{WalletName: "satbooth", Descriptors: append([]btcrpc.DescriptorRecord(nil), recs...)}, nil
}

// GetNewAddress serves the address at next_index and advances it, as Core does. Only the node's
// AddressType is served: another type is error -12.
func (n *Node) GetNewAddress(ctx context.Context, addressType string) (string, error) {
	if err := n.enter(ctx, "getnewaddress"); err != nil {
		return "", err
	}
	if addressType != n.s.AddressType {
		n.mu.Unlock()
		return "", rpcErr("getnewaddress", -12)
	}
	i := n.s.NextIndex
	a := n.addressAt(i)
	n.s.NextIndex++
	h := n.s.AddressAfterHook
	n.mu.Unlock()
	if h != nil {
		h(i)
	}
	return a, nil
}

func (n *Node) GetAddressInfo(ctx context.Context, address string) (btcrpc.AddressInfo, error) {
	if err := n.enter(ctx, "getaddressinfo"); err != nil {
		return btcrpc.AddressInfo{}, err
	}
	info := btcrpc.AddressInfo{Address: address}
	if i, ok := n.mine(address); ok {
		info.IsMine = true
		info.ParentDesc = n.s.Descriptor
		info.HDKeyPath = n.s.HDPrefix + strconv.FormatInt(i, 10)
	}
	hook := n.s.InfoHook
	n.mu.Unlock()
	if hook != nil {
		hook(&info)
	}
	return info, nil
}

func (n *Node) DeriveAddresses(ctx context.Context, desc string, index int64) ([]string, error) {
	if err := n.enter(ctx, "deriveaddresses"); err != nil {
		return nil, err
	}
	if desc != n.s.Descriptor || index < 0 {
		n.mu.Unlock()
		return nil, rpcErr("deriveaddresses", -5)
	}
	l := []string{n.addressAt(index)}
	hook := n.s.DeriveHook
	n.mu.Unlock()
	if hook != nil {
		l = hook(index, l)
	}
	return l, nil
}

// --- the chain ---

func (n *Node) tip() *block { return n.chain[len(n.chain)-1] }

// newTx adds a mempool transaction paying outs. It spends inputs, or fresh inputs of its own (fresh of them) when
// inputs is nil.
func (n *Node) newTx(outs []out, inputs []string, fresh int) *tx {
	sum := sha256.Sum256([]byte("tx " + strconv.Itoa(len(n.order)) + " " + strconv.FormatInt(n.seed, 10)))
	t := &tx{id: hex.EncodeToString(sum[:]), outs: outs, inputs: inputs, block: -1, conflictAt: -1, inMempool: true}
	for i := 0; inputs == nil && i < fresh; i++ {
		t.inputs = append(t.inputs, t.id+":"+strconv.Itoa(i))
	}
	n.txs[t.id] = t
	n.order = append(n.order, t.id)
	return t
}

func outsOf(l []Out) []out {
	r := make([]out, len(l))
	for i, o := range l {
		r[i] = out{o.Address, o.Sat}
	}
	return r
}

// rivals are the other transactions that spend one of t's inputs, in the order they were made.
func (n *Node) rivals(t *tx) []*tx {
	var l []*tx
	for _, id := range n.order {
		if r := n.txs[id]; r != t && shareInput(r, t) {
			l = append(l, r)
		}
	}
	return l
}

func shareInput(a, b *tx) bool {
	for _, i := range a.inputs {
		for _, j := range b.inputs {
			if i == j {
				return true
			}
		}
	}
	return false
}

// Pay puts a transaction paying sat to address in the mempool and returns its txid.
func (n *Node) Pay(address string, sat int64) string {
	return n.PayWith(1, []Out{{address, sat}})
}

// PayWith puts a transaction that spends inputs fresh inputs (at least 1) and pays outs in the mempool, and returns
// its txid.
func (n *Node) PayWith(inputs int, outs []Out) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.newTx(outsOf(outs), nil, max(inputs, 1)).id
}

// Inputs are the inputs txid spends (nil for an unknown txid), in order.
func (n *Node) Inputs(txid string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if t := n.txs[txid]; t != nil {
		return append([]string(nil), t.inputs...)
	}
	return nil
}

// Spend puts a transaction that spends inputs and pays outs in the mempool, as a replacement (RBF) does: every
// mempool transaction that spends one of those inputs leaves the mempool. It returns the new txid, or "" when inputs
// is empty or a mined transaction spends one of them.
func (n *Node) Spend(inputs []string, outs []Out) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(inputs) == 0 {
		return ""
	}
	nw := &tx{inputs: inputs}
	for _, r := range n.rivals(nw) {
		if r.block >= 0 {
			return ""
		}
	}
	for _, r := range n.rivals(nw) {
		r.inMempool = false
	}
	return n.newTx(outsOf(outs), append([]string(nil), inputs...), 0).id
}

// Replace replaces the mempool transaction txid by a new one that spends the same inputs (ReplaceWith): it keeps the
// outputs when keepOutput is true and pays the same amounts to foreign addresses otherwise. It returns the new txid,
// or "".
func (n *Node) Replace(txid string, keepOutput bool) string {
	n.mu.Lock()
	old := n.txs[txid]
	var outs []Out
	if old != nil {
		for i, o := range old.outs {
			if !keepOutput {
				o.addr = n.s.HRP + "qforeign" + strconv.Itoa(i)
			}
			outs = append(outs, Out{o.addr, o.sat})
		}
	}
	n.mu.Unlock()
	return n.ReplaceWith(txid, outs)
}

// ReplaceWith replaces the mempool transaction txid by a new one that spends the same inputs and pays outs (a fee bump,
// or a double spend; Spend with txid's inputs): the old one leaves the mempool. It returns the new txid, or "" when
// txid is not in the mempool. A chain of bumps made with it spends the same inputs at every step, as Core's bumpfee
// does (it keeps the original's inputs and may add some), so each one lists every ancestor in walletconflicts; a
// replacement that drops the inputs of an ancestor is a Spend of its own.
func (n *Node) ReplaceWith(txid string, outs []Out) string {
	n.mu.Lock()
	old := n.txs[txid]
	if old == nil || !old.inMempool {
		n.mu.Unlock()
		return ""
	}
	in := append([]string(nil), old.inputs...)
	n.mu.Unlock()
	return n.Spend(in, outs)
}

// Evict removes a transaction from the mempool without replacing it.
func (n *Node) Evict(txid string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if t := n.txs[txid]; t != nil && t.block < 0 {
		t.inMempool = false
	}
}

// Rebroadcast puts an evicted transaction back in the mempool, if it is neither mined nor conflicted and no mempool
// transaction spends one of its inputs.
func (n *Node) Rebroadcast(txid string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.txs[txid]
	if t == nil || t.block >= 0 || t.conflictAt >= 0 {
		return
	}
	for _, r := range n.rivals(t) {
		if r.inMempool {
			return
		}
	}
	t.inMempool = true
}

// Mine adds count blocks. The first holds every transaction in the mempool. A transaction that replaced another one
// becomes mined, and every other unmined transaction that spends one of its inputs becomes conflicted.
func (n *Node) Mine(count int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.frozen = nil // the wallet catches up with a new tip before it answers
	for k := 0; k < count; k++ {
		parent := n.tip()
		b := &block{height: parent.height + 1, parent: parent.hash}
		b.hash = n.blockHash(b.height)
		for _, id := range n.order {
			t := n.txs[id]
			if t.inMempool && t.block < 0 {
				t.block = b.height
				t.inMempool = false
				b.txids = append(b.txids, id)
			}
		}
		for _, id := range b.txids {
			for _, o := range n.rivals(n.txs[id]) {
				if o.block < 0 && o.conflictAt < 0 {
					o.conflictAt = b.height
				}
			}
		}
		n.chain = append(n.chain, b)
	}
}

// Invalidate removes the last count blocks (never the genesis). Their transactions return to the mempool.
func (n *Node) Invalidate(count int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.frozen = nil
	for k := 0; k < count && len(n.chain) > 1; k++ {
		b := n.tip()
		n.chain = n.chain[:len(n.chain)-1]
		n.detached[b.hash] = b
		for _, id := range b.txids {
			t := n.txs[id]
			t.block = -1
			t.inMempool = true
		}
		for _, t := range n.txs {
			if t.conflictAt >= b.height {
				t.conflictAt = -1
			}
		}
	}
}

// Tip is the hash of the best block; Height its height; BlockHash the main chain's hash at a height ("" if none).
func (n *Node) Tip() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.tip().hash
}

func (n *Node) Height() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.tip().height
}

func (n *Node) BlockHash(height int64) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if height < 0 || height >= int64(len(n.chain)) {
		return ""
	}
	return n.chain[height].hash
}

func (n *Node) confirmations(t *tx) int64 {
	switch {
	case t.block >= 0:
		return n.tip().height - t.block + 1
	case t.conflictAt >= 0:
		return -(n.tip().height - t.conflictAt + 1)
	}
	return 0
}

// mempoolConflicts: the transactions in the mempool that spend this one's input, while this one is neither mined nor
// conflicted.
func (n *Node) mempoolConflicts(t *tx) []string {
	if t.block >= 0 || t.conflictAt >= 0 {
		return nil
	}
	var l []string
	for _, r := range n.rivals(t) {
		if r.inMempool {
			l = append(l, r.id)
		}
	}
	return l
}

// walletConflicts is listsinceblock's walletconflicts: every other wallet transaction (one with an output to a wallet
// address) that spends an input of this one, whatever its state, both ways and through a chain of replacements.
// *source*: Core 32.1 src/wallet/wallet.cpp:1108 (AddToSpends for every transaction added to the wallet), 841-848
// (each input), 695-715 (GetConflicts: every wallet transaction spending one of its inputs), 2093-2100 (less itself);
// src/wallet/rpc/transactions.cpp:47-50 (WalletTxToJSON), 654 (listsinceblock lists through it).
func (n *Node) walletConflicts(t *tx) []string {
	var l []string
	for _, r := range n.rivals(t) {
		for _, o := range r.outs {
			if _, ok := n.mine(o.addr); ok {
				l = append(l, r.id)
				break
			}
		}
	}
	return l
}

func (n *Node) blockOf(t *tx) string {
	if t.block >= 0 && t.block < int64(len(n.chain)) {
		return n.chain[t.block].hash
	}
	return ""
}

func (n *Node) entries(t *tx) []btcrpc.WalletTx {
	var l []btcrpc.WalletTx
	for v, o := range t.outs {
		if _, ok := n.mine(o.addr); !ok {
			continue
		}
		e := btcrpc.WalletTx{Category: "receive", Address: o.addr, AmountSat: o.sat, Vout: uint32(v), TxID: t.id,
			Confirmations: n.confirmations(t), MempoolConflicts: n.mempoolConflicts(t), WalletConflicts: n.walletConflicts(t)}
		if t.block >= 0 {
			e.BlockHash, e.BlockHeight = n.blockOf(t), t.block
		}
		l = append(l, e)
	}
	return l
}

// ListSinceBlock lists the wallet's receive entries mined above blockhash's height (above the fork point, when that
// block was invalidated) and every unconfirmed one; conflicted ones drop out. Removed holds the
// entries of the transactions of the detached blocks on the way from blockhash to the main chain. LastBlock is the
// main chain's block targetConfirmations - 1 below the tip, or the genesis
// (src/wallet/rpc/transactions.cpp:678-680); a target below 1 is error -8.
func (n *Node) ListSinceBlock(ctx context.Context, blockhash string, targetConfirmations int) (btcrpc.SinceBlock, error) {
	if err := n.enter(ctx, "listsinceblock"); err != nil {
		return btcrpc.SinceBlock{}, err
	}
	defer n.mu.Unlock()
	defer n.walletView()()
	if targetConfirmations < 1 {
		return btcrpc.SinceBlock{}, rpcErr("listsinceblock", -8)
	}
	fork := int64(-1)
	var removed []btcrpc.WalletTx
	if blockhash != "" {
		found := false
		for _, b := range n.chain {
			if b.hash == blockhash {
				fork, found = b.height, true
			}
		}
		if !found {
			b := n.detached[blockhash]
			if b == nil {
				return btcrpc.SinceBlock{}, rpcErr("listsinceblock", -5)
			}
			for b != nil {
				for _, id := range b.txids {
					removed = append(removed, n.entries(n.txs[id])...)
				}
				p := b.parent
				if b = n.detached[p]; b == nil {
					for _, m := range n.chain {
						if m.hash == p {
							fork = m.height
						}
					}
				}
			}
		}
	}
	var txs []btcrpc.WalletTx
	for _, id := range n.order {
		t := n.txs[id]
		if t.conflictAt >= 0 || (t.block >= 0 && t.block <= fork) {
			continue
		}
		txs = append(txs, n.entries(t)...)
	}
	last := max(n.tip().height-int64(targetConfirmations-1), 0)
	return btcrpc.SinceBlock{Transactions: txs, Removed: removed, LastBlock: n.chain[last].hash}, nil
}

// GetTransaction follows Core: confirmations negative for a conflicted transaction, 0 with mempoolconflicts for
// a replaced one still in memory, blockhash only when mined.
func (n *Node) GetTransaction(ctx context.Context, txid string) (btcrpc.Transaction, error) {
	if err := n.enterTx(ctx, "gettransaction", txid); err != nil {
		return btcrpc.Transaction{}, err
	}
	defer n.mu.Unlock()
	defer n.walletView()()
	t := n.txs[txid]
	if t == nil {
		return btcrpc.Transaction{}, rpcErr("gettransaction", -5)
	}
	r := btcrpc.Transaction{TxID: t.id, Confirmations: n.confirmations(t), MempoolConflicts: n.mempoolConflicts(t)}
	for _, in := range t.inputs {
		r.Inputs = append(r.Inputs, outpointOf(in))
	}
	if t.block >= 0 {
		r.BlockHash, r.BlockHeight = n.blockOf(t), t.block
	}
	for v, o := range t.outs {
		r.Outputs = append(r.Outputs, btcrpc.Output{Vout: uint32(v), ValueSat: o.sat, Address: o.addr})
	}
	return r, nil
}

// InMempool is getmempoolentry: false for a transaction that left the mempool or never was in it, and for every
// transaction while the mempool is loading (Core answers -5 for one not loaded yet).
func (n *Node) InMempool(ctx context.Context, txid string) (bool, error) {
	if err := n.enterTx(ctx, "getmempoolentry", txid); err != nil {
		return false, err
	}
	defer n.mu.Unlock()
	t := n.txs[txid]
	return t != nil && t.inMempool && !n.s.MempoolLoading, nil
}

// MempoolLoaded is getmempoolinfo's loaded: false while MempoolLoading.
func (n *Node) MempoolLoaded(ctx context.Context) (bool, error) {
	if err := n.enter(ctx, "getmempoolinfo"); err != nil {
		return false, err
	}
	defer n.mu.Unlock()
	return !n.s.MempoolLoading, nil
}

// outpointOf is the outpoint of one of the fake's input names ("<txid>:<n>" for the fresh inputs of a payment). A name
// of another shape stands for the outpoint of the SHA-256 of the name, index 0, so that any input a test gives Spend
// has a well-formed one.
func outpointOf(in string) btcrpc.Outpoint {
	if i := strings.LastIndexByte(in, ':'); i == 64 {
		if v, err := strconv.Atoi(in[i+1:]); err == nil && v >= 0 {
			return btcrpc.Outpoint{TxID: in[:i], Vout: v}
		}
	}
	sum := sha256.Sum256([]byte("input " + in))
	return btcrpc.Outpoint{TxID: hex.EncodeToString(sum[:])}
}

// SpentInMempool is gettxspendingprevout with mempool_only: the node's live view of which mempool transaction spends
// each outpoint (the first one made, as only one can), or "". An empty list is btcrpc.ErrArgument, as the client's.
func (n *Node) SpentInMempool(ctx context.Context, outs []btcrpc.Outpoint) ([]string, error) {
	if len(outs) == 0 {
		return nil, fmt.Errorf("%w: no outpoints", btcrpc.ErrArgument)
	}
	if err := n.enter(ctx, "gettxspendingprevout"); err != nil {
		return nil, err
	}
	defer n.mu.Unlock()
	res := make([]string, len(outs))
	for i, o := range outs {
		for _, id := range n.order {
			t := n.txs[id]
			if !t.inMempool {
				continue
			}
			for _, in := range t.inputs {
				if outpointOf(in) == o {
					res[i] = t.id
				}
			}
			if res[i] != "" {
				break
			}
		}
	}
	return res, nil
}
