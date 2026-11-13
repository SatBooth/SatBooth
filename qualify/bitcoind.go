//go:build qualify

package qualify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/satbooth/satbooth/btcrpc"
)

const minerWallet = "miner"

// bitcoindRPC is a minimal JSON-RPC client for the regtest bitcoind. The credentials come from the
// environment (compose.yml), never from argv.
type bitcoindRPC struct {
	url, user, pass string
	hc              *http.Client
}

func newBitcoindRPC(t testing.TB) *bitcoindRPC {
	t.Helper()
	u := os.Getenv("BITCOIND_RPC_URL")
	if u == "" {
		u = bitcoindURL
	}
	user, pass := os.Getenv("BITCOIND_RPC_USER"), os.Getenv("BITCOIND_RPC_PASSWORD")
	if user == "" || pass == "" {
		t.Fatal("BITCOIND_RPC_USER or BITCOIND_RPC_PASSWORD is not set")
	}
	return &bitcoindRPC{url: u, user: user, pass: pass, hc: &http.Client{Timeout: 5 * time.Minute}}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("bitcoind error %d (message of %d bytes)", e.Code, len(e.Message))
}

// httpError is an answer that is not JSON-RPC, such as the 403 of a method outside the rpcwhitelist.
type httpError struct {
	Method string
	Status int
}

func (e *httpError) Error() string {
	return fmt.Sprintf("bitcoind %s: HTTP %d, unreadable answer", e.Method, e.Status)
}

// call runs one RPC; wallet "" is the node endpoint, otherwise /wallet/<name>.
// params is a JSON array ([]any) or a JSON object (map[string]any, named parameters); nil is an empty array.
func (b *bitcoindRPC) call(wallet, method string, params any, out any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": "qualify", "method": method, "params": params})
	if err != nil {
		return err
	}
	u := b.url
	if wallet != "" {
		u += "/wallet/" + wallet
	}
	req, err := http.NewRequest("POST", u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(b.user, b.pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return &httpError{Method: method, Status: resp.StatusCode}
	}
	if r.Error != nil {
		return r.Error
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (b *bitcoindRPC) createWallet(name string) error {
	return b.call("", "createwallet", []any{name}, nil)
}

func (b *bitcoindRPC) getNewAddress() (string, error) {
	var a string
	err := b.call(minerWallet, "getnewaddress", nil, &a)
	return a, err
}

func (b *bitcoindRPC) generateToAddress(n int, addr string) error {
	return b.call("", "generatetoaddress", []any{n, addr}, nil)
}

// sendToAddress sends btc (a decimal string avoids float rounding) from the miner wallet.
func (b *bitcoindRPC) sendToAddress(addr, btc string) error {
	return b.call(minerWallet, "sendtoaddress", []any{addr, json.Number(btc)}, nil)
}

func (b *bitcoindRPC) getBlockCount() (int, error) {
	var n int
	err := b.call("", "getblockcount", nil, &n)
	return n, err
}

// ---- on-chain helpers (plan 6) ----

// newRPCAs is a client for another user of the same bitcoind, with the password from the environment.
func newRPCAs(t testing.TB, userEnv, passEnv string) *bitcoindRPC {
	t.Helper()
	b := newBitcoindRPC(t)
	b.user, b.pass = os.Getenv(userEnv), os.Getenv(passEnv)
	if b.user == "" || b.pass == "" {
		t.Fatalf("%s or %s is not set", userEnv, passEnv)
	}
	return b
}

// createWalletWatch makes a descriptor wallet without private keys and without descriptors; the operator imports one.
func (b *bitcoindRPC) createWalletWatch(name string) error {
	return b.call("", "createwallet", map[string]any{"wallet_name": name, "disable_private_keys": true, "blank": true,
		"descriptors": true, "load_on_startup": true}, nil)
}

// createWalletKeys makes a normal descriptor wallet with keys (the test's merchant).
func (b *bitcoindRPC) createWalletKeys(name string) error {
	return b.call("", "createwallet", map[string]any{"wallet_name": name, "load_on_startup": true}, nil)
}

// loadWallet loads a wallet; error code -35 (already loaded) is success.
func (b *bitcoindRPC) loadWallet(name string) error {
	err := b.call("", "loadwallet", map[string]any{"filename": name}, nil)
	var re *rpcError
	if errors.As(err, &re) && re.Code == -35 {
		return nil
	}
	return err
}

type descriptorRow struct {
	Desc      string  `json:"desc"`
	Active    bool    `json:"active"`
	Internal  bool    `json:"internal"`
	Range     []int64 `json:"range"`
	Next      int64   `json:"next"`
	NextIndex int64   `json:"next_index"`
}

func (b *bitcoindRPC) listDescriptors(wallet string) ([]descriptorRow, error) {
	var r struct {
		Descriptors []descriptorRow `json:"descriptors"`
	}
	err := b.call(wallet, "listdescriptors", nil, &r)
	return r.Descriptors, err
}

// importDescriptor imports desc as the wallet's active external descriptor, "now" (no rescan).
func (b *bitcoindRPC) importDescriptor(wallet, desc string) error {
	return b.importDescriptorOpts(wallet, desc, nil)
}

// importDescriptorOpts is importDescriptor with extra request members (range, next_index).
func (b *bitcoindRPC) importDescriptorOpts(wallet, desc string, extra map[string]any) error {
	req := map[string]any{"desc": desc, "active": true, "internal": false, "timestamp": "now"}
	for k, v := range extra {
		req[k] = v
	}
	var res []struct {
		Success bool      `json:"success"`
		Error   *rpcError `json:"error"`
	}
	if err := b.call(wallet, "importdescriptors", map[string]any{"requests": []any{req}}, &res); err != nil {
		return err
	}
	if len(res) != 1 || !res[0].Success {
		if len(res) == 1 && res[0].Error != nil {
			return res[0].Error
		}
		return errors.New("importdescriptors: not successful")
	}
	return nil
}

// sendRBF sends btc (a decimal string) from the wallet, signalling replaceability, and returns the txid.
func (b *bitcoindRPC) sendRBF(wallet, addr, btc string) (string, error) {
	var txid string
	err := b.call(wallet, "sendtoaddress", map[string]any{"address": addr, "amount": json.Number(btc), "replaceable": true}, &txid)
	return txid, err
}

// bumpFee replaces txid. outputs nil keeps the outputs; otherwise it is the new outputs ([]map[address]amount).
func (b *bitcoindRPC) bumpFee(wallet, txid string, outputs []any) (string, error) {
	p := map[string]any{"txid": txid}
	if outputs != nil {
		p["options"] = map[string]any{"outputs": outputs}
	}
	var r struct {
		TxID string `json:"txid"`
	}
	err := b.call(wallet, "bumpfee", p, &r)
	return r.TxID, err
}

func (b *bitcoindRPC) invalidateBlock(hash string) error {
	return b.call("", "invalidateblock", map[string]any{"blockhash": hash}, nil)
}

func (b *bitcoindRPC) reconsiderBlock(hash string) error {
	return b.call("", "reconsiderblock", map[string]any{"blockhash": hash}, nil)
}

func (b *bitcoindRPC) getBlockHash(height int) (string, error) {
	var h string
	err := b.call("", "getblockhash", map[string]any{"height": height}, &h)
	return h, err
}

// getReceivedByAddress is the wallet's receipts to addr with at least minconf confirmations, in sat.
func (b *bitcoindRPC) getReceivedByAddress(wallet, addr string, minconf int) (int64, error) {
	var n json.Number
	if err := b.call(wallet, "getreceivedbyaddress", map[string]any{"address": addr, "minconf": minconf}, &n); err != nil {
		return 0, err
	}
	return btcrpc.ParseBTC(n.String())
}

type addressInfo struct {
	IsMine     bool   `json:"ismine"`
	HDKeyPath  string `json:"hdkeypath"`
	ParentDesc string `json:"parent_desc"`
	Desc       string `json:"desc"`
}

func (b *bitcoindRPC) getAddressInfo(wallet, addr string) (addressInfo, error) {
	var a addressInfo
	err := b.call(wallet, "getaddressinfo", map[string]any{"address": addr}, &a)
	return a, err
}

// mine mines n blocks to a fresh address of the miner wallet.
func (b *bitcoindRPC) mine(n int) error {
	a, err := b.getNewAddress()
	if err != nil {
		return err
	}
	return b.generateToAddress(n, a)
}

// rawTx is gettransaction (verbose) reduced to what the phases read.
type rawTx struct {
	Confirmations int `json:"confirmations"`
	Decoded       struct {
		Vin []struct {
			TxID string `json:"txid"`
			Vout uint32 `json:"vout"`
		} `json:"vin"`
		Vout []struct {
			Value json.Number `json:"value"`
			N     uint32      `json:"n"`
			SPK   struct {
				Address string `json:"address"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	} `json:"decoded"`
}

func (b *bitcoindRPC) getTransaction(wallet, txid string) (rawTx, json.RawMessage, error) {
	var raw json.RawMessage
	err := b.call(wallet, "gettransaction", map[string]any{"txid": txid, "verbose": true}, &raw)
	var t rawTx
	if err == nil {
		err = json.Unmarshal(raw, &t)
	}
	return t, raw, err
}

// prevoutSpend is one element of gettxspendingprevout's answer; SpendingTxID is empty when nothing spends the output.
type prevoutSpend struct {
	TxID         string `json:"txid"`
	Vout         uint32 `json:"vout"`
	SpendingTxID string `json:"spendingtxid"`
}

// spendingPrevout is the node's gettxspendingprevout of the outputs, looking at the mempool only.
func (b *bitcoindRPC) spendingPrevout(outs []prevoutRef) ([]prevoutSpend, error) {
	var res []prevoutSpend
	err := b.call("", "gettxspendingprevout", map[string]any{"outputs": outs, "options": map[string]any{"mempool_only": true}}, &res)
	return res, err
}

// prevoutRef names an output in gettxspendingprevout's request.
type prevoutRef struct {
	TxID string `json:"txid"`
	Vout uint32 `json:"vout"`
}

// spendersOf is, for each input of txid (read from the wallet's verbose gettransaction, decoded.vin), the txid of the
// mempool transaction the node says spends it ("" for none).
func (b *bitcoindRPC) spendersOf(wallet, txid string) ([]string, error) {
	t, _, err := b.getTransaction(wallet, txid)
	if err != nil {
		return nil, err
	}
	var ins []prevoutRef
	for _, in := range t.Decoded.Vin {
		ins = append(ins, prevoutRef{TxID: in.TxID, Vout: in.Vout})
	}
	res, err := b.spendingPrevout(ins)
	if err != nil {
		return nil, err
	}
	if len(res) != len(ins) {
		return nil, fmt.Errorf("gettxspendingprevout answered %d elements for %d outputs", len(res), len(ins))
	}
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.SpendingTxID
	}
	return out, nil
}

// mempoolConflicts is the wallet's mempoolconflicts of txid (gettransaction).
func (b *bitcoindRPC) mempoolConflicts(wallet, txid string) ([]string, error) {
	var r struct {
		MempoolConflicts []string `json:"mempoolconflicts"`
	}
	err := b.call(wallet, "gettransaction", map[string]any{"txid": txid}, &r)
	return r.MempoolConflicts, err
}

// divert replaces txid (sent by wallet to custAddr) by a transaction that pays custAddr nothing: the amount goes to
// toMiner1 and the change to toMiner2, the fee rising by extraFeeSat. It tries bumpfee with the outputs option and,
// if Core refuses it, falls back to a hand-built conflicting spend (createrawtransaction, signrawtransactionwithwallet,
// sendrawtransaction). usedFallback says which one worked; bumpErr is bumpfee's refusal.
func (b *bitcoindRPC) divert(wallet, txid, custAddr, toMiner1, toMiner2 string, extraFeeSat int64) (newTxid string, usedFallback bool, bumpErr error, err error) {
	t, _, err := b.getTransaction(wallet, txid)
	if err != nil {
		return "", false, nil, err
	}
	var custSat, otherSat int64
	for _, o := range t.Decoded.Vout {
		sat, perr := btcrpc.ParseBTC(o.Value.String())
		if perr != nil {
			return "", false, nil, perr
		}
		if o.SPK.Address == custAddr {
			custSat = sat
		} else {
			otherSat += sat
		}
	}
	if custSat == 0 || otherSat <= extraFeeSat {
		return "", false, nil, fmt.Errorf("divert: unexpected outputs (customer %d sat, other %d sat)", custSat, otherSat)
	}
	out1 := map[string]any{toMiner1: json.Number(btcrpc.FormatBTC(custSat))}
	out2 := map[string]any{toMiner2: json.Number(btcrpc.FormatBTC(otherSat - extraFeeSat))}
	newTxid, bumpErr = b.bumpFee(wallet, txid, []any{out1, out2})
	if bumpErr == nil {
		return newTxid, false, nil, nil
	}
	var ins []any
	for _, in := range t.Decoded.Vin {
		ins = append(ins, map[string]any{"txid": in.TxID, "vout": in.Vout, "sequence": 0xfffffffd})
	}
	var rawHex string
	if err = b.call("", "createrawtransaction", map[string]any{"inputs": ins, "outputs": []any{out1, out2}}, &rawHex); err != nil {
		return "", true, bumpErr, err
	}
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err = b.call(wallet, "signrawtransactionwithwallet", map[string]any{"hexstring": rawHex}, &signed); err != nil || !signed.Complete {
		return "", true, bumpErr, fmt.Errorf("divert: signing failed (%v)", shownErr(err))
	}
	err = b.call("", "sendrawtransaction", map[string]any{"hexstring": signed.Hex}, &newTxid)
	return newTxid, true, bumpErr, err
}
