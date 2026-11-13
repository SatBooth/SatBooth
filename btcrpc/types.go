package btcrpc

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The result types of the typed calls. Field shapes come from Bitcoin Core v32.1 (src/wallet/rpc/transactions.cpp,
// backup.cpp, addresses.cpp, src/rpc/blockchain.cpp). Decoding ignores fields it does not know, because Core adds
// fields between versions, but a field of the wrong JSON type, or a missing one that the caller relies on, is
// ErrBadAnswer.

// BlockchainInfo is the part of getblockchaininfo SatBooth uses.
type BlockchainInfo struct {
	Chain                string
	Blocks, Headers      int64
	BestBlockHash        string
	InitialBlockDownload bool
}

// WalletInfo is the part of getwalletinfo SatBooth uses: it asserts a descriptor wallet without private keys.
type WalletInfo struct {
	WalletName         string
	Descriptors        bool
	PrivateKeysEnabled bool
}

// DescriptorInfo is getdescriptorinfo's answer. Descriptor is the normalised public form with its checksum;
// Checksum is the checksum of the string that was sent.
type DescriptorInfo struct {
	Descriptor, Checksum                string
	IsRange, IsSolvable, HasPrivateKeys bool
}

// DescriptorRecord is one entry of listdescriptors. NextIndex and Range are zero for an unranged descriptor.
type DescriptorRecord struct {
	Desc      string
	Active    bool
	Internal  bool
	NextIndex int64
	Range     [2]int64
}

// Descriptors is listdescriptors' answer.
type Descriptors struct {
	WalletName  string
	Descriptors []DescriptorRecord
}

// AddressInfo is the part of getaddressinfo SatBooth uses: ParentDesc is the stored
// descriptor and HDKeyPath ends with the derivation index.
type AddressInfo struct {
	Address                     string
	IsMine                      bool
	ParentDesc, HDKeyPath, Desc string
}

// WalletTx is one entry of listsinceblock. AmountSat is signed: a send is negative.
type WalletTx struct {
	Category, Address string
	AmountSat         int64
	Vout              uint32
	TxID              string
	Confirmations     int64
	BlockHash         string
	BlockHeight       int64
	MempoolConflicts  []string
	WalletConflicts   []string
}

// SinceBlock is listsinceblock's answer. Dropped counts the entries (of transactions and removed together)
// that were malformed and left out; one odd entry never fails the answer.
type SinceBlock struct {
	Transactions, Removed []WalletTx
	LastBlock             string
	Dropped               int
}

// Output is one output of a decoded transaction; Address is empty if the script has none.
type Output struct {
	Vout     uint32
	ValueSat int64
	Address  string
}

// Outpoint names one output of a transaction: the txid and the output's index.
type Outpoint struct {
	TxID string
	Vout int
}

// Transaction is the part of gettransaction (verbose) SatBooth uses. Inputs are the outpoints the transaction spends,
// from decoded.vin; a coinbase input adds none.
type Transaction struct {
	TxID             string
	Confirmations    int64
	BlockHash        string
	BlockHeight      int64
	MempoolConflicts []string
	Outputs          []Output
	Inputs           []Outpoint
}

// --- decoders: each takes the "result" value and is pure, so it can be fuzzed ---

func bad(what string) error { return fmt.Errorf("%w: %s", ErrBadAnswer, what) }

// decodeObject unmarshals raw into v, which must be a pointer to a struct, after checking raw is an object.
func decodeObject(raw json.RawMessage, v any, what string) error {
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		return bad(what + ": not an object")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return bad(what + ": wrong field type")
	}
	return nil
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isTxid(s string) bool { return isHex(s, 64) }

// optHash is true for an empty string or a 64-character lowercase hex string.
func optHash(s string) bool { return s == "" || isHex(s, 64) }

func txidList(l []string) bool {
	for _, s := range l {
		if !isTxid(s) {
			return false
		}
	}
	return true
}

func decodeBlockchainInfo(raw json.RawMessage) (BlockchainInfo, error) {
	var a struct {
		Chain         *string `json:"chain"`
		Blocks        *int64  `json:"blocks"`
		Headers       *int64  `json:"headers"`
		BestBlockHash *string `json:"bestblockhash"`
		IBD           *bool   `json:"initialblockdownload"`
	}
	if err := decodeObject(raw, &a, "getblockchaininfo"); err != nil {
		return BlockchainInfo{}, err
	}
	if a.Chain == nil || a.Blocks == nil || a.Headers == nil || a.BestBlockHash == nil || a.IBD == nil ||
		!validChain(*a.Chain) || !inChain(*a.Blocks) || !inChain(*a.Headers) || !isHex(*a.BestBlockHash, 64) {
		return BlockchainInfo{}, bad("getblockchaininfo")
	}
	return BlockchainInfo{Chain: *a.Chain, Blocks: *a.Blocks, Headers: *a.Headers, BestBlockHash: *a.BestBlockHash, InitialBlockDownload: *a.IBD}, nil
}

// maxChainNum bounds block heights and confirmation counts: 10 million blocks is about 190 years.
const maxChainNum = 10_000_000

// maxIndex is the exclusive bound of a descriptor index (Core's range end is below 2^31).
const maxIndex = 1 << 31

func inChain(v int64) bool { return v >= 0 && v <= maxChainNum }
func inConfs(v int64) bool { return v >= -maxChainNum && v <= maxChainNum }

// validChain accepts Core's chain names (getblockchaininfo "chain", src/rpc/blockchain.cpp) ("main", "test",
// "testnet4", "signet", "regtest"): 1 to 32 of [a-z0-9].
func validChain(s string) bool {
	if len(s) < 1 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= 'a' && s[i] <= 'z' || s[i] >= '0' && s[i] <= '9') {
			return false
		}
	}
	return true
}

// decodeWalletInfo: its fuzz target (FuzzDecode, via checkAll) asserts only that it does not panic; the
// fields have no range or format to check.
func decodeWalletInfo(raw json.RawMessage) (WalletInfo, error) {
	var a struct {
		WalletName  *string `json:"walletname"`
		Descriptors *bool   `json:"descriptors"`
		PrivateKeys *bool   `json:"private_keys_enabled"`
	}
	if err := decodeObject(raw, &a, "getwalletinfo"); err != nil {
		return WalletInfo{}, err
	}
	if a.WalletName == nil || a.Descriptors == nil || a.PrivateKeys == nil {
		return WalletInfo{}, bad("getwalletinfo")
	}
	return WalletInfo{WalletName: *a.WalletName, Descriptors: *a.Descriptors, PrivateKeysEnabled: *a.PrivateKeys}, nil
}

// decodeMempoolInfo reads getmempoolinfo's "loaded", which must be a boolean.
func decodeMempoolInfo(raw json.RawMessage) (bool, error) {
	var a struct {
		Loaded *bool `json:"loaded"`
	}
	if err := decodeObject(raw, &a, "getmempoolinfo"); err != nil {
		return false, err
	}
	if a.Loaded == nil {
		return false, bad("getmempoolinfo")
	}
	return *a.Loaded, nil
}

func decodeDescriptorInfo(raw json.RawMessage) (DescriptorInfo, error) {
	var a struct {
		Descriptor *string `json:"descriptor"`
		Checksum   *string `json:"checksum"`
		IsRange    *bool   `json:"isrange"`
		IsSolvable *bool   `json:"issolvable"`
		HasPrivate *bool   `json:"hasprivatekeys"`
	}
	if err := decodeObject(raw, &a, "getdescriptorinfo"); err != nil {
		return DescriptorInfo{}, err
	}
	if a.Descriptor == nil || a.Checksum == nil || a.IsRange == nil || a.IsSolvable == nil || a.HasPrivate == nil ||
		*a.Descriptor == "" || len(*a.Checksum) != 8 {
		return DescriptorInfo{}, bad("getdescriptorinfo")
	}
	return DescriptorInfo{Descriptor: *a.Descriptor, Checksum: *a.Checksum, IsRange: *a.IsRange, IsSolvable: *a.IsSolvable, HasPrivateKeys: *a.HasPrivate}, nil
}

func decodeDescriptors(raw json.RawMessage) (Descriptors, error) {
	var a struct {
		WalletName  *string `json:"wallet_name"`
		Descriptors []struct {
			Desc      *string `json:"desc"`
			Active    *bool   `json:"active"`
			Internal  *bool   `json:"internal"`
			NextIndex *int64  `json:"next_index"`
			Range     []int64 `json:"range"`
		} `json:"descriptors"`
	}
	if err := decodeObject(raw, &a, "listdescriptors"); err != nil {
		return Descriptors{}, err
	}
	if a.WalletName == nil || a.Descriptors == nil {
		return Descriptors{}, bad("listdescriptors")
	}
	out := Descriptors{WalletName: *a.WalletName, Descriptors: make([]DescriptorRecord, 0, len(a.Descriptors))}
	for _, d := range a.Descriptors {
		if d.Desc == nil || *d.Desc == "" || d.Active == nil {
			return Descriptors{}, bad("listdescriptors")
		}
		r := DescriptorRecord{Desc: *d.Desc, Active: *d.Active}
		if d.Internal != nil {
			r.Internal = *d.Internal
		}
		if d.NextIndex != nil {
			if *d.NextIndex < 0 || *d.NextIndex >= maxIndex {
				return Descriptors{}, bad("listdescriptors")
			}
			r.NextIndex = *d.NextIndex
		}
		if d.Range != nil {
			if len(d.Range) != 2 || d.Range[0] < 0 || d.Range[1] < d.Range[0] || d.Range[1] >= maxIndex {
				return Descriptors{}, bad("listdescriptors")
			}
			r.Range = [2]int64{d.Range[0], d.Range[1]}
		}
		out.Descriptors = append(out.Descriptors, r)
	}
	return out, nil
}

// decodeAddress decodes a result that is one non-empty string.
func decodeAddress(raw json.RawMessage) (string, error) {
	var s string
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '"' || json.Unmarshal(raw, &s) != nil || s == "" {
		return "", bad("address")
	}
	return s, nil
}

func decodeAddressInfo(raw json.RawMessage) (AddressInfo, error) {
	var a struct {
		Address    *string `json:"address"`
		IsMine     *bool   `json:"ismine"`
		ParentDesc string  `json:"parent_desc"`
		HDKeyPath  string  `json:"hdkeypath"`
		Desc       string  `json:"desc"`
	}
	if err := decodeObject(raw, &a, "getaddressinfo"); err != nil {
		return AddressInfo{}, err
	}
	if a.Address == nil || a.IsMine == nil || *a.Address == "" {
		return AddressInfo{}, bad("getaddressinfo")
	}
	return AddressInfo{Address: *a.Address, IsMine: *a.IsMine, ParentDesc: a.ParentDesc, HDKeyPath: a.HDKeyPath, Desc: a.Desc}, nil
}

// decodeAddressList decodes deriveaddresses' answer for a range [i,i]: exactly one non-empty string.
func decodeAddressList(raw json.RawMessage) ([]string, error) {
	var l []string
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '[' || json.Unmarshal(raw, &l) != nil || len(l) != 1 || l[0] == "" {
		return nil, bad("deriveaddresses")
	}
	return l, nil
}

// walletTxFromEntry decodes one listsinceblock entry; ok is false for any malformed one.
func walletTxFromEntry(raw json.RawMessage) (WalletTx, bool) {
	var a struct {
		Category         *string         `json:"category"`
		Address          string          `json:"address"`
		Amount           json.RawMessage `json:"amount"`
		Vout             *int64          `json:"vout"`
		TxID             *string         `json:"txid"`
		Confirmations    *int64          `json:"confirmations"`
		BlockHash        string          `json:"blockhash"`
		BlockHeight      int64           `json:"blockheight"`
		MempoolConflicts []string        `json:"mempoolconflicts"`
		WalletConflicts  []string        `json:"walletconflicts"`
	}
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' || json.Unmarshal(raw, &a) != nil {
		return WalletTx{}, false
	}
	if a.Category == nil || *a.Category == "" || a.Vout == nil || a.TxID == nil || a.Confirmations == nil || a.Amount == nil {
		return WalletTx{}, false
	}
	if *a.Vout < 0 || *a.Vout > 1<<32-1 || !isTxid(*a.TxID) || !optHash(a.BlockHash) || !inChain(a.BlockHeight) || !inConfs(*a.Confirmations) ||
		!txidList(a.MempoolConflicts) || !txidList(a.WalletConflicts) {
		return WalletTx{}, false
	}
	sat, err := ParseSignedBTC(string(a.Amount))
	if err != nil {
		return WalletTx{}, false
	}
	return WalletTx{
		Category: *a.Category, Address: a.Address, AmountSat: sat, Vout: uint32(*a.Vout), TxID: *a.TxID,
		Confirmations: *a.Confirmations, BlockHash: a.BlockHash, BlockHeight: a.BlockHeight,
		MempoolConflicts: a.MempoolConflicts, WalletConflicts: a.WalletConflicts,
	}, true
}

func decodeSinceBlock(raw json.RawMessage) (SinceBlock, error) {
	var a struct {
		Transactions []json.RawMessage `json:"transactions"`
		Removed      []json.RawMessage `json:"removed"`
		LastBlock    *string           `json:"lastblock"`
	}
	if err := decodeObject(raw, &a, "listsinceblock"); err != nil {
		return SinceBlock{}, err
	}
	if a.Transactions == nil || a.LastBlock == nil || !isHex(*a.LastBlock, 64) {
		return SinceBlock{}, bad("listsinceblock")
	}
	out := SinceBlock{LastBlock: *a.LastBlock}
	entries := func(in []json.RawMessage) []WalletTx {
		var l []WalletTx
		for _, e := range in {
			if tx, ok := walletTxFromEntry(e); ok {
				l = append(l, tx)
			} else {
				out.Dropped++
			}
		}
		return l
	}
	out.Transactions = entries(a.Transactions)
	out.Removed = entries(a.Removed)
	return out, nil
}

func decodeTransaction(raw json.RawMessage) (Transaction, error) {
	var a struct {
		TxID             *string  `json:"txid"`
		Confirmations    *int64   `json:"confirmations"`
		BlockHash        string   `json:"blockhash"`
		BlockHeight      int64    `json:"blockheight"`
		MempoolConflicts []string `json:"mempoolconflicts"`
		Decoded          *struct {
			Vin  []map[string]json.RawMessage `json:"vin"`
			Vout []struct {
				Value        json.RawMessage `json:"value"`
				N            *int64          `json:"n"`
				ScriptPubKey struct {
					Address string `json:"address"`
				} `json:"scriptPubKey"`
			} `json:"vout"`
		} `json:"decoded"`
	}
	if err := decodeObject(raw, &a, "gettransaction"); err != nil {
		return Transaction{}, err
	}
	if a.TxID == nil || a.Confirmations == nil || a.Decoded == nil || a.Decoded.Vout == nil || a.Decoded.Vin == nil ||
		!isTxid(*a.TxID) || !optHash(a.BlockHash) || !inChain(a.BlockHeight) || !inConfs(*a.Confirmations) || !txidList(a.MempoolConflicts) {
		return Transaction{}, bad("gettransaction")
	}
	tx := Transaction{TxID: *a.TxID, Confirmations: *a.Confirmations, BlockHash: a.BlockHash, BlockHeight: a.BlockHeight,
		MempoolConflicts: a.MempoolConflicts, Outputs: make([]Output, 0, len(a.Decoded.Vout))}
	for _, in := range a.Decoded.Vin {
		if _, coinbase := in["coinbase"]; coinbase {
			continue
		}
		var txid string
		var vout int64
		if string(in["vout"]) == "null" || json.Unmarshal(in["txid"], &txid) != nil || json.Unmarshal(in["vout"], &vout) != nil || !isTxid(txid) || vout < 0 || vout > 1<<32-1 {
			return Transaction{}, bad("gettransaction")
		}
		tx.Inputs = append(tx.Inputs, Outpoint{TxID: txid, Vout: int(vout)})
	}
	for _, o := range a.Decoded.Vout {
		if o.N == nil || *o.N < 0 || *o.N > 1<<32-1 || o.Value == nil {
			return Transaction{}, bad("gettransaction")
		}
		sat, err := ParseBTC(string(o.Value))
		if err != nil {
			return Transaction{}, bad("gettransaction")
		}
		tx.Outputs = append(tx.Outputs, Output{Vout: uint32(*o.N), ValueSat: sat, Address: o.ScriptPubKey.Address})
	}
	return tx, nil
}

// decodeSpendingPrevout checks gettxspendingprevout's answer against the outpoints asked: an array of exactly as many
// objects, in the asked order, each naming the same txid and vout, with a spendingtxid (64 lowercase hex) only where a
// mempool transaction spends the output. It returns the spending txid per outpoint, or "".
func decodeSpendingPrevout(raw json.RawMessage, asked []Outpoint) ([]string, error) {
	var a []struct {
		TxID         *string         `json:"txid"`
		Vout         *int64          `json:"vout"`
		SpendingTxID json.RawMessage `json:"spendingtxid"` // raw: null is not "no spender", it is a bad answer
	}
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '[' {
		return nil, bad("gettxspendingprevout: not an array")
	}
	if err := json.Unmarshal(raw, &a); err != nil || len(a) != len(asked) {
		return nil, bad("gettxspendingprevout")
	}
	out := make([]string, len(a))
	for i, e := range a {
		if e.TxID == nil || e.Vout == nil || *e.TxID != asked[i].TxID || *e.Vout != int64(asked[i].Vout) {
			return nil, bad("gettxspendingprevout")
		}
		if e.SpendingTxID != nil {
			var id string
			if json.Unmarshal(e.SpendingTxID, &id) != nil || !isTxid(id) {
				return nil, bad("gettxspendingprevout")
			}
			out[i] = id
		}
	}
	return out, nil
}
