package btcrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxResponseBytes is the most the client reads from one answer.
const MaxResponseBytes = 8 << 20

const (
	maxURLBytes    = 512
	maxNameBytes   = 64
	maxUserPass    = 128
	minTimeout     = time.Second
	maxTimeout     = 10 * time.Minute
	defaultTimeout = 30 * time.Second
	requestID      = "satbooth"
)

var (
	// ErrMethod means the method is outside the allowlist; nothing was sent.
	ErrMethod = errors.New("btcrpc: method not allowed")
	// ErrArgument means an argument of a typed call was refused before anything was sent.
	ErrArgument = errors.New("btcrpc: bad argument")
	// ErrTransport marks a failure to get an answer: dial, timeout, or an HTTP status other than 200.
	ErrTransport = errors.New("btcrpc: transport failure")
	// ErrHTTP means an HTTP status other than 200 with no JSON-RPC error in the body (what rpcwhitelist
	// answers with 403 and a wrong password with 401: src/httprpc.cpp:110-118, :207, :224). It also satisfies errors.Is(err, ErrTransport).
	ErrHTTP = errors.New("btcrpc: unexpected HTTP status")
	// ErrBadAnswer means the body was not a JSON-RPC envelope, or a result had the wrong shape.
	ErrBadAnswer = errors.New("btcrpc: bad answer")
	// ErrZeroed means the client's password was cleared by Zero; nothing was sent.
	ErrZeroed = errors.New("btcrpc: client closed (password cleared)")
)

// allowedMethods is the runtime RPC method set, the whole rpcwhitelist, in the order it is written everywhere
// (docs/bitcoind.md, the regtest bitcoin.conf, here). The client refuses any other method before sending it.
var allowedMethods = []string{
	"getnewaddress", "getaddressinfo", "deriveaddresses", "listsinceblock", "gettransaction",
	"getmempoolentry", "getblockchaininfo", "getwalletinfo", "listdescriptors", "getdescriptorinfo", "getmempoolinfo",
	"gettxspendingprevout",
}

// Error is a JSON-RPC error from bitcoind. It keeps the code and the method only: bitcoind's message text
// is dropped because it can echo a key back, so nothing that formats this error can print it.
type Error struct {
	Code   int
	Method string
}

func (e *Error) Error() string { return "btcrpc: error " + strconv.Itoa(e.Code) + " from " + e.Method }

// Config is the client's configuration. It holds no secret: New takes the password beside it, so no
// rendering of a Config can print one.
type Config struct {
	URL     string // http or https, with a host, no user info, query or fragment
	User    string // ValidName
	Wallet  string // ValidName; the wallet that CallWallet addresses
	Timeout time.Duration
}

// secret holds the password two pointers away from anything fmt can be given: the Client keeps a *secret,
// and secret keeps a *[]byte. fmt calls no method for %p of a non-pointer nor for an unexported struct field,
// so no method can guard the bytes; what does is that fmt prints a pointer below the top level as an address,
// and the one way it dereferences a nested pointer (a verb the field's kind does not take, such as %s on a
// pointer: "%!s(*T=&{...})") restarts with %v, under which the inner pointer prints as an address. One level
// is not enough (TestClientEveryVerb).
type secret struct{ b *[]byte }

func newSecret(password []byte) *secret {
	b := append([]byte(nil), password...)
	return &secret{b: &b}
}

func (s *secret) bytes() []byte { return *s.b }

func (s *secret) clear() {
	clear(*s.b)
	*s.b = nil
}

// Client is a bitcoind JSON-RPC client. It is safe for concurrent use.
type Client struct {
	base    string
	user    string
	wallet  string
	timeout time.Duration
	http    *http.Client

	mu       sync.RWMutex
	password *secret // never nil; Zero clears its bytes
	zeroed   bool
}

func (*Client) String() string   { return "btcrpc.Client" }
func (*Client) GoString() string { return "btcrpc.Client" }

// ValidName reports whether s is 1 to 64 characters of [A-Za-z0-9._-]: the rule for an RPC user name and a
// wallet name, exported so the service uses it rather than writing the rule twice.
func ValidName(s string) bool {
	if len(s) < 1 || len(s) > maxNameBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// ValidWallet reports whether s is a usable wallet name: ValidName, and not "." or "..", which as a path segment
// would leave /wallet/. New applies it; the service uses it rather than writing the rule twice.
func ValidWallet(s string) bool { return ValidName(s) && s != "." && s != ".." }

// ValidPort reports whether s is a port: 1 to 65535 in decimal digits, no sign, no leading zero. It is the one port
// rule of the service's configuration; the service uses it rather than writing it twice.
func ValidPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == s
}

// portOK reports whether the host part of a URL has no port, or a valid one (a colon with nothing after it is
// refused).
func portOK(host string) bool {
	i := strings.LastIndex(host, ":")
	if i < 0 || i < strings.LastIndex(host, "]") {
		return true
	}
	return ValidPort(host[i+1:])
}

// ValidURL reports whether s is an absolute http or https URL with a host, without user info, query (not
// even an empty one) or fragment, spaces or control characters, at most 512 bytes. It is the rule of the
// service's plainURL, reimplemented because service imports this package.
func ValidURL(s string) bool {
	if len(s) == 0 || len(s) > maxURLBytes || strings.Contains(s, "#") {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f || r > '~' {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || !portOK(u.Host) {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.RawQuery == "" && !u.ForceQuery
}

// New checks cfg and returns a client. The URL, user and wallet are validated; the password (1 to 128 bytes)
// is copied (Zero clears the copy); the timeout is 1 s to 10 min, default 30 s. The transport ignores the proxy environment,
// follows no redirects and reads at most MaxResponseBytes.
func New(cfg Config, password []byte) (*Client, error) {
	if !ValidURL(cfg.URL) {
		return nil, errors.New("btcrpc: url must be an http or https URL with a host, without user info, query or fragment")
	}
	if !ValidName(cfg.User) {
		return nil, errors.New("btcrpc: user must be 1 to 64 characters of A-Z a-z 0-9 . _ -")
	}
	if !ValidWallet(cfg.Wallet) {
		return nil, errors.New("btcrpc: wallet must be 1 to 64 characters of A-Z a-z 0-9 . _ - and not . or ..")
	}
	if len(password) < 1 || len(password) > maxUserPass {
		return nil, errors.New("btcrpc: password must be 1 to 128 bytes")
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if timeout < minTimeout || timeout > maxTimeout {
		return nil, errors.New("btcrpc: timeout must be 1 s to 10 min")
	}
	tr := &http.Transport{
		Proxy:                  nil, // never from the environment: the credentials go to the configured node only
		DisableCompression:     true,
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConns:           2,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
	}
	return &Client{
		base:     strings.TrimRight(cfg.URL, "/"),
		user:     cfg.User,
		wallet:   cfg.Wallet,
		timeout:  timeout,
		password: newSecret(password),
		http: &http.Client{
			Transport:     tr,
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Zero clears the client's copy of the password. Later calls fail without sending anything.
func (c *Client) Zero() {
	c.mu.Lock()
	c.password.clear()
	c.zeroed = true
	c.mu.Unlock()
	c.http.CloseIdleConnections()
}

func allowed(method string) bool {
	for _, m := range allowedMethods {
		if m == method {
			return true
		}
	}
	return false
}

// Call sends method with named params to the node endpoint and decodes the result into out (nil to ignore it).
// params is marshalled as given; nil becomes {}; it must marshal to a JSON object.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	return c.call(ctx, c.base, method, params, out)
}

// CallWallet is Call to /wallet/<Wallet>.
func (c *Client) CallWallet(ctx context.Context, method string, params any, out any) error {
	return c.call(ctx, c.base+"/wallet/"+url.PathEscape(c.wallet), method, params, out)
}

func (c *Client) call(ctx context.Context, endpoint, method string, params any, out any) error {
	raw, err := c.rpc(ctx, endpoint, method, params)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return bad(method + ": result does not fit")
	}
	return nil
}

func (c *Client) rpc(ctx context.Context, endpoint, method string, params any) (json.RawMessage, error) {
	if !allowed(method) {
		return nil, fmt.Errorf("%w: %q", ErrMethod, method)
	}
	if params == nil {
		params = struct{}{}
	}
	p, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("%w: params do not marshal", ErrArgument)
	}
	if bytes.Equal(p, []byte("null")) {
		p = []byte("{}")
	}
	if len(p) == 0 || p[0] != '{' {
		return nil, fmt.Errorf("%w: params must be a JSON object", ErrArgument)
	}
	body, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      string          `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{"1.0", requestID, method, p})
	if err != nil {
		return nil, fmt.Errorf("%w: request does not marshal", ErrArgument)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: request", ErrArgument)
	}
	req.Header.Set("Content-Type", "application/json")
	c.mu.RLock()
	if c.zeroed {
		c.mu.RUnlock()
		return nil, ErrZeroed
	}
	req.SetBasicAuth(c.user, string(c.password.bytes()))
	c.mu.RUnlock()

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(ctx, method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, transportError(ctx, method, err)
	}
	if len(data) > MaxResponseBytes {
		return nil, bad(method + ": answer over 8 MiB")
	}
	// Core answers an RPC error with HTTP 404 (method not found), 400 (invalid request) or 500 and a
	// JSON-RPC body (src/httprpc.cpp:49-58), so the body is read before the status.
	raw, envErr := decodeEnvelope(data, method)
	var rpcErr *Error
	if errors.As(envErr, &rpcErr) {
		return nil, envErr
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d from %s: %w", ErrHTTP, resp.StatusCode, method, ErrTransport)
	}
	return raw, envErr
}

// transportError is the error for a request that got no usable answer. It keeps none of the transport's own
// error, whose text can quote the answer's bytes (net/http writes `malformed HTTP response "..."` and the
// like): only the method, a fixed reason, and, when the context ended or a timeout struck, the context's
// sentinel, so that errors.Is still finds context.Canceled and context.DeadlineExceeded.
func transportError(ctx context.Context, method string, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%w: %s: %w", ErrTransport, method, cerr)
	}
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %s: %w", ErrTransport, method, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return fmt.Errorf("%w: %s: %w", ErrTransport, method, context.DeadlineExceeded)
	}
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case errors.As(err, &dns):
		return fmt.Errorf("%w: %s: host not found", ErrTransport, method)
	case errors.As(err, &op) && op.Op == "dial":
		return fmt.Errorf("%w: %s: cannot connect", ErrTransport, method)
	}
	return fmt.Errorf("%w: %s: no usable answer", ErrTransport, method)
}

// decodeEnvelope parses a JSON-RPC 1.0 answer: an object with "result", "error" and "id" "satbooth". A non-null
// error object with an integer code is an *Error (its message is not read into the result); a body that is not
// that shape, or whose result is missing or null, is ErrBadAnswer.
func decodeEnvelope(data []byte, method string) (json.RawMessage, error) {
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
		ID     json.RawMessage `json:"id"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if t := bytes.TrimSpace(data); len(t) == 0 || t[0] != '{' || dec.Decode(&env) != nil {
		return nil, bad(method + ": not a JSON-RPC envelope")
	}
	if _, err := dec.Token(); err != io.EOF { // nothing but whitespace after the object
		return nil, bad(method + ": data after the envelope")
	}
	var id string
	if json.Unmarshal(env.ID, &id) != nil || id != requestID {
		return nil, bad(method + ": wrong id")
	}
	if e := bytes.TrimSpace(env.Error); len(e) > 0 && !bytes.Equal(e, []byte("null")) {
		var ce struct {
			Code *int `json:"code"`
		}
		if e[0] != '{' || json.Unmarshal(e, &ce) != nil || ce.Code == nil {
			return nil, bad(method + ": malformed error")
		}
		return nil, &Error{Code: *ce.Code, Method: method}
	}
	if r := bytes.TrimSpace(env.Result); len(r) == 0 || bytes.Equal(r, []byte("null")) {
		return nil, bad(method + ": no result")
	}
	return env.Result, nil
}

// --- typed calls ---

// GetBlockchainInfo calls getblockchaininfo on the node endpoint.
func (c *Client) GetBlockchainInfo(ctx context.Context) (BlockchainInfo, error) {
	raw, err := c.rpc(ctx, c.base, "getblockchaininfo", nil)
	if err != nil {
		return BlockchainInfo{}, err
	}
	return decodeBlockchainInfo(raw)
}

// GetWalletInfo calls getwalletinfo on the wallet endpoint.
func (c *Client) GetWalletInfo(ctx context.Context) (WalletInfo, error) {
	raw, err := c.rpc(ctx, c.walletEndpoint(), "getwalletinfo", nil)
	if err != nil {
		return WalletInfo{}, err
	}
	return decodeWalletInfo(raw)
}

func (c *Client) walletEndpoint() string { return c.base + "/wallet/" + url.PathEscape(c.wallet) }

// GetDescriptorInfo calls getdescriptorinfo on the node endpoint. The descriptor goes to bitcoind: refuse
// private keys before calling (the intake grammar), because bitcoind accepts them and only answers.
func (c *Client) GetDescriptorInfo(ctx context.Context, desc string) (DescriptorInfo, error) {
	if desc == "" {
		return DescriptorInfo{}, fmt.Errorf("%w: empty descriptor", ErrArgument)
	}
	raw, err := c.rpc(ctx, c.base, "getdescriptorinfo", map[string]any{"descriptor": desc})
	if err != nil {
		return DescriptorInfo{}, err
	}
	return decodeDescriptorInfo(raw)
}

// ListDescriptors calls listdescriptors on the wallet endpoint.
func (c *Client) ListDescriptors(ctx context.Context) (Descriptors, error) {
	raw, err := c.rpc(ctx, c.walletEndpoint(), "listdescriptors", nil)
	if err != nil {
		return Descriptors{}, err
	}
	return decodeDescriptors(raw)
}

// GetNewAddress calls getnewaddress with an empty label. addressType is "bech32" or "bech32m".
func (c *Client) GetNewAddress(ctx context.Context, addressType string) (string, error) {
	if addressType != "bech32" && addressType != "bech32m" {
		return "", fmt.Errorf("%w: address type", ErrArgument)
	}
	raw, err := c.rpc(ctx, c.walletEndpoint(), "getnewaddress", map[string]any{"label": "", "address_type": addressType})
	if err != nil {
		return "", err
	}
	return decodeAddress(raw)
}

// GetAddressInfo calls getaddressinfo on the wallet endpoint.
func (c *Client) GetAddressInfo(ctx context.Context, address string) (AddressInfo, error) {
	if address == "" || len(address) > 128 {
		return AddressInfo{}, fmt.Errorf("%w: address", ErrArgument)
	}
	raw, err := c.rpc(ctx, c.walletEndpoint(), "getaddressinfo", map[string]any{"address": address})
	if err != nil {
		return AddressInfo{}, err
	}
	return decodeAddressInfo(raw)
}

// DeriveAddresses calls deriveaddresses on the node endpoint for the single index (range [i,i]); it needs a
// descriptor with a checksum. It returns exactly one address.
func (c *Client) DeriveAddresses(ctx context.Context, desc string, index int64) ([]string, error) {
	if desc == "" || index < 0 || index >= 1<<31 {
		return nil, fmt.Errorf("%w: descriptor or index", ErrArgument)
	}
	raw, err := c.rpc(ctx, c.base, "deriveaddresses", map[string]any{"descriptor": desc, "range": [2]int64{index, index}})
	if err != nil {
		return nil, err
	}
	return decodeAddressList(raw)
}

// ListSinceBlock calls listsinceblock with include_removed true and include_change false. An empty blockhash
// is left out of the params. Malformed entries are dropped and counted in SinceBlock.Dropped.
func (c *Client) ListSinceBlock(ctx context.Context, blockhash string, targetConfirmations int) (SinceBlock, error) {
	if !optHash(blockhash) || targetConfirmations < 1 || targetConfirmations > 100_000 {
		return SinceBlock{}, fmt.Errorf("%w: blockhash or target confirmations", ErrArgument)
	}
	p := map[string]any{"target_confirmations": targetConfirmations, "include_removed": true, "include_change": false}
	if blockhash != "" {
		p["blockhash"] = blockhash
	}
	raw, err := c.rpc(ctx, c.walletEndpoint(), "listsinceblock", p)
	if err != nil {
		return SinceBlock{}, err
	}
	return decodeSinceBlock(raw)
}

// GetTransaction calls gettransaction with verbose true; Outputs come from decoded.vout.
func (c *Client) GetTransaction(ctx context.Context, txid string) (Transaction, error) {
	if !isTxid(txid) {
		return Transaction{}, fmt.Errorf("%w: txid", ErrArgument)
	}
	raw, err := c.rpc(ctx, c.walletEndpoint(), "gettransaction", map[string]any{"txid": txid, "verbose": true})
	if err != nil {
		return Transaction{}, err
	}
	tx, err := decodeTransaction(raw)
	if err != nil {
		return Transaction{}, err
	}
	if tx.TxID != txid {
		return Transaction{}, bad("gettransaction: another txid")
	}
	return tx, nil
}

// SpentInMempool calls gettxspendingprevout on the node endpoint with the named params outputs and options {mempool_only: true} and returns, for each asked
// outpoint in order, the id of the mempool transaction that spends it, or "". Core answers an empty list of outpoints
// with an error, so none is sent: no outpoints is ErrArgument. The answer must name the same outpoints in the same order.
func (c *Client) SpentInMempool(ctx context.Context, outs []Outpoint) ([]string, error) {
	if len(outs) == 0 {
		return nil, fmt.Errorf("%w: no outpoints", ErrArgument)
	}
	list := make([]map[string]any, len(outs))
	for i, o := range outs {
		if !isTxid(o.TxID) || o.Vout < 0 || int64(o.Vout) > 1<<32-1 {
			return nil, fmt.Errorf("%w: outpoint", ErrArgument)
		}
		list[i] = map[string]any{"txid": o.TxID, "vout": o.Vout}
	}
	raw, err := c.rpc(ctx, c.base, "gettxspendingprevout", map[string]any{"outputs": list, "options": map[string]any{"mempool_only": true}})
	if err != nil {
		return nil, err
	}
	return decodeSpendingPrevout(raw, outs)
}

// MempoolLoaded calls getmempoolinfo on the node endpoint and returns its "loaded": true once the node's first attempt
// to load the persisted mempool (mempool.dat) has finished (Core 32.1 src/rpc/mempool.cpp:1113 and :1143,
// GetLoadTried; src/init.cpp:2130-2136 sets it in the background "initload" thread, which RPC does not wait for,
// SetRPCWarmupFinished at :2409). Until then getmempoolentry answers -5 for a transaction that is about to be loaded.
func (c *Client) MempoolLoaded(ctx context.Context) (bool, error) {
	raw, err := c.rpc(ctx, c.base, "getmempoolinfo", nil)
	if err != nil {
		return false, err
	}
	return decodeMempoolInfo(raw)
}

// InMempool calls getmempoolentry on the node endpoint. Error code -5 (RPC_INVALID_ADDRESS_OR_KEY,
// src/rpc/protocol.h:67; getmempoolentry fails for a transaction that left the mempool) is false, nil.
func (c *Client) InMempool(ctx context.Context, txid string) (bool, error) {
	if !isTxid(txid) {
		return false, fmt.Errorf("%w: txid", ErrArgument)
	}
	raw, err := c.rpc(ctx, c.base, "getmempoolentry", map[string]any{"txid": txid})
	var e *Error
	if errors.As(err, &e) && e.Code == -5 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		return false, bad("getmempoolentry: not an object")
	}
	return true, nil
}
