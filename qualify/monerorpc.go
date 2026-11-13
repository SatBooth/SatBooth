//go:build qualify

package qualify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// The harness's side of the Monero regtest services (dev/regtest/compose.yml): monerod's RPC, and the customer's
// wallet-rpc, which runs without a login and holds the two full wallets the phases use, customer (pays) and merchant
// (the merchant's own wallet: it confirms receipts). SatBooth's view-only wallet-rpc (wallet-satbooth) is reached only
// through xmrrpc.New, with the Digest login, and only while no service runs. That is caution, not a known limit: a
// second Digest client beside the service was not tried, and epee keeps the Digest state (nonce and counter) per
// connection (http_server_auth m_auth in http_custom_handler, contrib/epee/include/net/http_protocol_handler.h:190 and
// :227, the per-connection m_handler of abstract_tcp_server2.h:271, at v0.18.5.4).
const (
	monerodURL       = "http://172.30.78.3:18081"
	walletSatbooth   = "http://172.30.78.5:18083"
	walletCustPort   = 18084
	walletCustomer   = "customer"
	walletMerchant   = "merchant"
	viewWalletFile   = "/data/satbooth-view"
	moneroUID        = 10002 // the monero user of dev/regtest/monero/Dockerfile
	moneroHTTPBudget = 5 * time.Minute
)

var moneroHC = &http.Client{Timeout: moneroHTTPBudget}

// xmrError is a JSON-RPC error from monerod or a wallet-rpc of the test network (regtest data only).
type xmrError struct {
	Method  string
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *xmrError) Error() string {
	return fmt.Sprintf("%s: error %d (message of %d bytes)", e.Method, e.Code, len(e.Message))
}

// postJSON posts body to url and decodes the answer into v.
func postJSON(url string, body any, v any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	resp, err := moneroHC.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return resp.StatusCode, fmt.Errorf("%s: HTTP %d, unreadable answer", url, resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// jsonRPC is one JSON-RPC 2.0 call with named parameters (nil: {}) to base/json_rpc.
func jsonRPC(base, method string, params any, out any) error {
	if params == nil {
		params = map[string]any{}
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *xmrError       `json:"error"`
	}
	if _, err := postJSON(base+"/json_rpc", map[string]any{"jsonrpc": "2.0", "id": "0", "method": method, "params": params}, &r); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if r.Error != nil {
		r.Error.Method = method
		return r.Error
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// monerodRPC calls monerod's JSON-RPC: generateblocks{wallet_address, amount_of_blocks}, flush_txpool{txids},
// get_info, get_block_header_by_height. pop_blocks is not a JSON-RPC method at v0.18.5.4 (it is the URI /pop_blocks,
// src/rpc/core_rpc_server.h:142): monerodPopBlocks calls it.
func monerodRPC(method string, params any, out any) error {
	return jsonRPC(monerodURL, method, params, out)
}

// monerodPopBlocks pops n blocks off monerod's chain (/pop_blocks{nblocks}) and returns the new height. The popped
// blocks' transactions go back to the pool (Blockchain::pop_blocks).
func monerodPopBlocks(n int) (int64, error) {
	var r struct {
		Status string `json:"status"`
		Height int64  `json:"height"`
	}
	if _, err := postJSON(monerodURL+"/pop_blocks", map[string]any{"nblocks": n}, &r); err != nil {
		return 0, err
	}
	if r.Status != "OK" {
		return 0, fmt.Errorf("pop_blocks: status %s", shown(r.Status, "OK", "BUSY"))
	}
	return r.Height, nil
}

// walletRPC calls the customer's wallet-rpc on port (no login): create_wallet, open_wallet, refresh, get_address,
// create_address, query_key, transfer, get_balance, get_transfers, get_transfer_by_txid, relay_tx, store,
// sign_transfer (the full-wallet probe, through xmrrpc).
func walletRPC(port int, method string, params any, out any) error {
	return jsonRPC("http://172.30.78.4:"+strconv.Itoa(port), method, params, out)
}

// custWallet is walletRPC on the customer wallet-rpc that remembers which of its wallets is open (wallet-rpc holds one
// at a time; open_wallet stores the one it closes).
type custWallet struct{ open string }

// use opens name (unless it is open already) and refreshes it from monerod.
func (w *custWallet) use(name string) error {
	if w.open != name {
		if err := walletRPC(walletCustPort, "open_wallet", map[string]any{"filename": name, "password": ""}, nil); err != nil {
			return err
		}
		w.open = name
	}
	return walletRPC(walletCustPort, "refresh", nil, nil)
}

func (w *custWallet) call(name, method string, params any, out any) error {
	if err := w.use(name); err != nil {
		return err
	}
	return walletRPC(walletCustPort, method, params, out)
}

// xferIn is one incoming entry of get_transfers or get_transfer_by_txid on a full wallet.
type xferIn struct {
	TxID          string `json:"txid"`
	Address       string `json:"address"`
	Amount        uint64 `json:"amount"`
	Height        int64  `json:"height"`
	Confirmations int64  `json:"confirmations"`
	UnlockTime    uint64 `json:"unlock_time"`
	Locked        bool   `json:"locked"`
	Type          string `json:"type"`
	SubaddrIndex  struct {
		Major uint32 `json:"major"`
		Minor uint32 `json:"minor"`
	} `json:"subaddr_index"`
}

// xferResult is transfer's answer, reduced to what the phases read.
type xferResult struct {
	TxHash     string `json:"tx_hash"`
	Fee        uint64 `json:"fee"`
	TxMetadata string `json:"tx_metadata"`
}
