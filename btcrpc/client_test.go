package btcrpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	tUser = "satbooth"
	tPass = "test-password-1"
	tTx1  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tTx2  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tBlk  = "00000000000000000000000000000000000000000000000000000000000000ff"
	tBlk2 = "1111111111111111111111111111111111111111111111111111111111111111"
	// tAddr is the address of a BIP-173 test vector (program 751e76e8...) with the regtest prefix.
	tAddr = "bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080"
)

type seen struct {
	n      atomic.Int32
	method string
	path   string
	auth   [2]string
	ctype  string
	body   map[string]any
}

// serve runs a test server whose handler records the request and answers with the given result (a JSON
// value) or, if rpcErr is non-nil, a JSON-RPC error.
func serve(t *testing.T, result string) (*Client, *seen) {
	t.Helper()
	return serveFn(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"result":%s,"error":null,"id":"satbooth"}`, result)
	})
}

func serveFn(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*Client, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.n.Add(1)
		s.method, s.path, s.ctype = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		s.auth[0], s.auth[1], _ = r.BasicAuth()
		raw, _ := io.ReadAll(r.Body)
		s.body = nil
		_ = json.Unmarshal(raw, &s.body)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL, User: tUser, Wallet: "shop", Timeout: 5 * time.Second}, []byte(tPass))
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func TestRequestShape(t *testing.T) {
	c, s := serve(t, `{"chain":"regtest"}`)
	var out map[string]string
	if err := c.Call(context.Background(), "getblockchaininfo", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out["chain"] != "regtest" {
		t.Fatalf("out = %v", out)
	}
	if s.method != "POST" || s.path != "/" || s.ctype != "application/json" {
		t.Errorf("method %q path %q content-type %q", s.method, s.path, s.ctype)
	}
	if s.auth != [2]string{tUser, tPass} {
		t.Errorf("basic auth user %q (password ok: %v)", s.auth[0], s.auth[1] == tPass)
	}
	if s.body["jsonrpc"] != "1.0" || s.body["id"] != "satbooth" || s.body["method"] != "getblockchaininfo" {
		t.Errorf("envelope %v", s.body)
	}
	if p, ok := s.body["params"].(map[string]any); !ok || len(p) != 0 {
		t.Errorf("params %#v, want {}", s.body["params"])
	}

	if err := c.CallWallet(context.Background(), "getwalletinfo", map[string]any{"a": 1}, nil); err != nil {
		t.Fatal(err)
	}
	if s.path != "/wallet/shop" {
		t.Errorf("wallet path %q", s.path)
	}
	if p, _ := s.body["params"].(map[string]any); p["a"] != float64(1) {
		t.Errorf("named params %v", s.body["params"])
	}
	// params that are not a JSON object are refused before any request
	n := s.n.Load()
	if err := c.Call(context.Background(), "getblockchaininfo", []int{1}, nil); err == nil || s.n.Load() != n {
		t.Errorf("array params: err %v, requests %d -> %d", err, n, s.n.Load())
	}
}

func TestAllowedMethodsOrder(t *testing.T) {
	want := []string{"getnewaddress", "getaddressinfo", "deriveaddresses", "listsinceblock", "gettransaction",
		"getmempoolentry", "getblockchaininfo", "getwalletinfo", "listdescriptors", "getdescriptorinfo", "getmempoolinfo",
		"gettxspendingprevout"}
	if strings.Join(allowedMethods, ",") != strings.Join(want, ",") {
		t.Fatalf("allowedMethods = %v", allowedMethods)
	}
}

func TestMethodAllowlist(t *testing.T) {
	c, s := serve(t, `{}`)
	for _, m := range []string{"stop", "getbalance", "sendtoaddress", "createwallet", "importdescriptors", "dumpprivkey", "listwallets",
		"", "GETNEWADDRESS", "getnewaddress ", "getnewaddress\n", "walletpassphrase", "help", "getrpcinfo", "getblockcount"} {
		for _, call := range []func() error{
			func() error { return c.Call(context.Background(), m, nil, nil) },
			func() error { return c.CallWallet(context.Background(), m, nil, nil) },
		} {
			if err := call(); !errors.Is(err, ErrMethod) {
				t.Errorf("method %q: err = %v, want ErrMethod", m, err)
			}
		}
	}
	if s.n.Load() != 0 {
		t.Fatalf("%d requests were made for refused methods", s.n.Load())
	}
	for _, m := range allowedMethods {
		if err := c.Call(context.Background(), m, nil, nil); err != nil {
			t.Errorf("allowed method %q: %v", m, err)
		}
	}
}

func rpcErrServer(t *testing.T, status int, code int, msg string) *Client {
	c, _ := serveFn(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		b, _ := json.Marshal(map[string]any{"result": nil, "error": map[string]any{"code": code, "message": msg}, "id": "satbooth"})
		w.Write(b)
	})
	return c
}

func TestJSONRPCErrorKeepsNoMessage(t *testing.T) {
	const msg = "key 'xprv9s21ZrQH143K2' is not valid"
	for _, status := range []int{200, 500, 404} {
		c := rpcErrServer(t, status, -5, msg)
		err := c.Call(context.Background(), "getdescriptorinfo", map[string]any{"descriptor": "x"}, nil)
		var e *Error
		if !errors.As(err, &e) || e.Code != -5 || e.Method != "getdescriptorinfo" {
			t.Fatalf("status %d: err = %#v", status, err)
		}
		if errors.Is(err, ErrTransport) {
			t.Errorf("a JSON-RPC error is not a transport failure")
		}
		want := "btcrpc: error -5 from getdescriptorinfo"
		if e.Error() != want {
			t.Errorf("Error() = %q", e.Error())
		}
		for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
			for _, x := range []any{err, e, *e} {
				if out := fmt.Sprintf(f, x); strings.Contains(out, "xprv") || strings.Contains(out, "not valid") {
					t.Errorf("%s of %T leaks bitcoind's text: %s", f, x, out)
				}
			}
		}
	}
}

func TestHTTPStatuses(t *testing.T) {
	for _, status := range []int{401, 403, 404, 500, 503, 302, 204} {
		c, _ := serveFn(t, func(w http.ResponseWriter, r *http.Request) {
			if status == 302 {
				w.Header().Set("Location", "/elsewhere")
			}
			w.WriteHeader(status)
		})
		err := c.Call(context.Background(), "getblockchaininfo", nil, nil)
		if !errors.Is(err, ErrHTTP) || !errors.Is(err, ErrTransport) {
			t.Errorf("status %d: err = %v", status, err)
		}
		if err != nil && !strings.Contains(err.Error(), fmt.Sprint(status)) {
			t.Errorf("status %d not in %q", status, err)
		}
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer other.Close()
	c, _ := serveFn(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	})
	if err := c.Call(context.Background(), "getblockchaininfo", nil, nil); !errors.Is(err, ErrHTTP) {
		t.Fatalf("err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestHangingServerTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c, err := New(Config{URL: srv.URL, User: tUser, Wallet: "shop", Timeout: time.Second}, []byte(tPass))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = c.Call(context.Background(), "getblockchaininfo", nil, nil)
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d < 900*time.Millisecond || d > 4*time.Second {
		t.Fatalf("took %v", d)
	}
	// a caller's context ends the call too
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "getblockchaininfo", nil, nil); !errors.Is(err, ErrTransport) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx err = %v", err)
	}
}

func TestUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, err := New(Config{URL: url, User: tUser, Wallet: "shop"}, []byte(tPass))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Call(context.Background(), "getblockchaininfo", nil, nil); !errors.Is(err, ErrTransport) {
		t.Fatalf("err = %v", err)
	}
}

func TestOversizeAnswer(t *testing.T) {
	for _, extra := range []int{1, 0} {
		c, _ := serveFn(t, func(w http.ResponseWriter, r *http.Request) {
			head := `{"result":"`
			tail := `","error":null,"id":"satbooth"}`
			pad := MaxResponseBytes + extra - len(head) - len(tail)
			io.WriteString(w, head)
			io.WriteString(w, strings.Repeat("a", pad))
			io.WriteString(w, tail)
		})
		var out string
		err := c.Call(context.Background(), "getblockchaininfo", nil, &out)
		if extra == 1 && !errors.Is(err, ErrBadAnswer) {
			t.Errorf("8 MiB + 1: err = %v", err)
		}
		if extra == 0 && err != nil {
			t.Errorf("exactly 8 MiB: err = %v", err)
		}
	}
}

func TestBadAnswers(t *testing.T) {
	bodies := []string{
		``, `not json`, `[]`, `null`, `"x"`, `{}`, `{"result":1}`,
		`{"result":1,"error":null,"id":"other"}`, `{"result":1,"error":null,"id":1}`,
		`{"result":null,"error":null,"id":"satbooth"}`,
		`{"result":null,"error":"boom","id":"satbooth"}`,
		`{"result":null,"error":{"message":"x"},"id":"satbooth"}`,
		`{"result":null,"error":{"code":"-5"},"id":"satbooth"}`,
		`{"result":null,"error":{"code":1.5},"id":"satbooth"}`,
		`{"result":1,"error":null,"id":"satbooth"} trailing`,
	}
	for _, b := range bodies {
		c, _ := serveFn(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, b) })
		var out any
		if err := c.Call(context.Background(), "getblockchaininfo", nil, &out); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("body %q: err = %v, want ErrBadAnswer", b, err)
		}
	}
	// a result of the wrong type for out
	c, _ := serve(t, `"text"`)
	var out struct{ A int }
	if err := c.Call(context.Background(), "getblockchaininfo", nil, &out); !errors.Is(err, ErrBadAnswer) {
		t.Errorf("wrong type: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	ok := Config{URL: "http://127.0.0.1:18443", User: "u", Wallet: "w"}
	if _, err := New(ok, []byte("p")); err != nil {
		t.Fatal(err)
	}
	mut := func(f func(c *Config)) Config { c := ok; f(&c); return c }
	bad := map[string]Config{
		"empty url":     mut(func(c *Config) { c.URL = "" }),
		"ftp":           mut(func(c *Config) { c.URL = "ftp://127.0.0.1" }),
		"userinfo":      mut(func(c *Config) { c.URL = "http://u:p@127.0.0.1" }),
		"query":         mut(func(c *Config) { c.URL = "http://127.0.0.1/?a=b" }),
		"fragment":      mut(func(c *Config) { c.URL = "http://127.0.0.1/#x" }),
		"no host":       mut(func(c *Config) { c.URL = "http:///x" }),
		"empty user":    mut(func(c *Config) { c.User = "" }),
		"long user":     mut(func(c *Config) { c.User = strings.Repeat("a", 65) }),
		"odd user":      mut(func(c *Config) { c.User = "a:b" }),
		"empty wallet":  mut(func(c *Config) { c.Wallet = "" }),
		"slash wallet":  mut(func(c *Config) { c.Wallet = "a/b" }),
		"dot wallet":    mut(func(c *Config) { c.Wallet = "." }),
		"dotdot wallet": mut(func(c *Config) { c.Wallet = ".." }),
		"short timeout": mut(func(c *Config) { c.Timeout = 999 * time.Millisecond }),
		"long timeout":  mut(func(c *Config) { c.Timeout = 10*time.Minute + time.Nanosecond }),
		"negative":      mut(func(c *Config) { c.Timeout = -time.Second }),
	}
	for name, cfg := range bad {
		if _, err := New(cfg, []byte("p")); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, d := range []time.Duration{0, time.Second, 10 * time.Minute} {
		if _, err := New(mut(func(c *Config) { c.Timeout = d }), []byte("p")); err != nil {
			t.Errorf("timeout %v: %v", d, err)
		}
	}
	for name, pw := range map[string][]byte{"no password": nil, "empty password": {}, "long password": []byte(strings.Repeat("p", 129))} {
		if _, err := New(ok, pw); err == nil {
			t.Errorf("%s accepted", name)
		} else if strings.Contains(err.Error(), strings.Repeat("p", 100)) {
			t.Error("the error holds the password")
		}
	}
	if _, err := New(ok, []byte(strings.Repeat("p", 128))); err != nil {
		t.Errorf("128-byte password: %v", err)
	}
}

func TestNewCopiesPasswordAndZeroClearsIt(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		io.WriteString(w, `{"result":{},"error":null,"id":"satbooth"}`)
	}))
	defer srv.Close()
	pw := []byte(tPass)
	c, err := New(Config{URL: srv.URL, User: tUser, Wallet: "shop"}, pw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range pw {
		pw[i] = 'x' // the caller wipes its own copy
	}
	if err := c.Call(context.Background(), "getblockchaininfo", nil, nil); err != nil {
		t.Fatalf("the client lost its password when the caller wiped theirs: %v", err)
	}
	old := c.password.bytes()
	c.Zero()
	if len(c.password.bytes()) != 0 || !c.zeroed || len(old) != len(tPass) {
		t.Fatal("Zero left password bytes")
	}
	for i, b := range old {
		if b != 0 {
			t.Fatalf("Zero left byte %d of the password's copy", i)
		}
	}
	before := n.Load()
	if err := c.Call(context.Background(), "getblockchaininfo", nil, nil); !errors.Is(err, ErrZeroed) || n.Load() != before {
		t.Fatalf("a zeroed client made a request: %v", err)
	}
	c.Zero() // twice is fine
}

func TestNoErrorMentionsPassword(t *testing.T) {
	// the unreachable and the timeout cases: the transport's own error (a *url.Error, which can quote the
	// answer) is not kept, and nothing holds the password
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer hang.Close()
	defer close(release)
	for _, u := range []string{deadURL, hang.URL} {
		c, err := New(Config{URL: u, User: tUser, Wallet: "shop", Timeout: time.Second}, []byte(tPass))
		if err != nil {
			t.Fatal(err)
		}
		err = c.Call(context.Background(), "getblockchaininfo", nil, nil)
		var ue *url.Error
		if !errors.Is(err, ErrTransport) || errors.As(err, &ue) {
			t.Fatalf("want ErrTransport without the *url.Error, got %v", err)
		}
		if strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), tPass) {
			t.Fatal("a transport error holds the password")
		}
	}
	c, _ := serveFn(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	err := c.Call(context.Background(), "getblockchaininfo", nil, nil)
	if err == nil || strings.Contains(err.Error(), tPass) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(fmt.Sprintf("%+v %#v %v %s", c, c, c, c), tPass) {
		t.Fatal("formatting the client prints the password")
	}
}

// --- typed calls against canned Core-shaped answers ---

const (
	cannedChainInfo  = `{"chain":"regtest","blocks":210,"headers":210,"bestblockhash":"` + tBlk + `","difficulty":4.656542373906925e-10,"time":1700000000,"mediantime":1700000000,"verificationprogress":1,"initialblockdownload":false,"chainwork":"00","size_on_disk":1,"pruned":false,"warnings":[]}`
	cannedWalletInfo = `{"walletname":"shop","walletversion":169900,"format":"sqlite","txcount":3,"keypoolsize":1000,"keypoolsize_hd_internal":0,"paytxfee":0.00000000,"private_keys_enabled":false,"avoid_reuse":false,"scanning":false,"descriptors":true,"external_signer":false,"blank":true,"birthheight":1,"lastprocessedblock":{"hash":"` + tBlk + `","height":210}}`
	cannedDescInfo   = `{"descriptor":"wpkh([d34db33f/84h/1h/0h]tpubD6NzVbkrYhZ4/0/*)#abcd1234","checksum":"wxyz5678","isrange":true,"issolvable":true,"hasprivatekeys":false}`
	cannedDescs      = `{"wallet_name":"shop","descriptors":[{"desc":"wpkh([d34db33f/84h/1h/0h]tpubD6NzVbkrYhZ4/0/*)#abcd1234","timestamp":1700000000,"active":true,"internal":false,"range":[0,1011],"next":12,"next_index":12},{"desc":"addr(` + tAddr + `)#zz","timestamp":1,"active":false}]}`
	cannedAddrInfo   = `{"address":"` + tAddr + `","scriptPubKey":"0014","ismine":true,"solvable":true,"desc":"wpkh([d34db33f/84h/1h/0h/0/7]02aa)#dd","parent_desc":"wpkh([d34db33f/84h/1h/0h]tpubD6NzVbkrYhZ4/0/*)#abcd1234","iswatchonly":false,"isscript":false,"iswitness":true,"witness_version":0,"ischange":false,"timestamp":1700000000,"hdkeypath":"m/84h/1h/0h/0/7","hdseedid":"0000","hdmasterfingerprint":"d34db33f","labels":[""]}`
	cannedSince      = `{"transactions":[` +
		`{"address":"` + tAddr + `","category":"receive","amount":0.00150000,"label":"","vout":1,"confirmations":0,"txid":"` + tTx1 + `","walletconflicts":[],"mempoolconflicts":["` + tTx2 + `"],"time":1700000000,"timereceived":1700000000,"bip125-replaceable":"no"},` +
		`{"address":"bcrt1qsendsendsendsendsendsendsendsendsend0x","category":"send","amount":-0.00200000,"vout":0,"fee":-0.00000141,"confirmations":3,"blockhash":"` + tBlk + `","blockheight":208,"blockindex":1,"txid":"` + tTx2 + `","walletconflicts":["` + tTx1 + `"],"time":1,"timereceived":1}` +
		`],"removed":[{"address":"` + tAddr + `","category":"receive","amount":1.00000000,"vout":0,"confirmations":-2,"txid":"` + tTx1 + `"}],"lastblock":"` + tBlk2 + `"}`
	cannedMempoolInfo = `{"loaded":true,"size":3,"bytes":420,"usage":3000,"total_fee":0.00000420,"maxmempool":300000000,"mempoolminfee":0.00001000,"minrelaytxfee":0.00001000,"incrementalrelayfee":0.00001000,"unbroadcastcount":0,"permitbaremultisig":true,"maxdatacarriersize":100000,"limitclustercount":64,"limitclustersize":101000,"optimal":true}`
	cannedGetTx       = `{"amount":0.0015,"confirmations":4,"blockhash":"` + tBlk + `","blockheight":207,"blockindex":2,"txid":"` + tTx1 + `","walletconflicts":[],"mempoolconflicts":["` + tTx2 + `"],"time":1,"timereceived":1,"details":[],"hex":"00","decoded":{"txid":"` + tTx1 + `","version":2,"vin":[{"txid":"` + tTx2 + `","vout":1,"scriptSig":{"asm":"","hex":""},"txinwitness":["00"],"sequence":4294967293}],"vout":[{"value":0.00150000,"n":0,"scriptPubKey":{"type":"witness_v0_keyhash","address":"` + tAddr + `"}},{"value":0.5,"n":1,"scriptPubKey":{"asm":"OP_RETURN 00","type":"nulldata"}}]}}`
)

func TestTypedCalls(t *testing.T) {
	ctx := context.Background()

	c, s := serve(t, cannedChainInfo)
	bi, err := c.GetBlockchainInfo(ctx)
	if err != nil || bi != (BlockchainInfo{Chain: "regtest", Blocks: 210, Headers: 210, BestBlockHash: tBlk}) {
		t.Fatalf("GetBlockchainInfo = %+v, %v", bi, err)
	}
	if s.body["method"] != "getblockchaininfo" || s.path != "/" {
		t.Errorf("sent %v on %s", s.body, s.path)
	}

	c, s = serve(t, cannedWalletInfo)
	wi, err := c.GetWalletInfo(ctx)
	if err != nil || wi != (WalletInfo{WalletName: "shop", Descriptors: true, PrivateKeysEnabled: false}) {
		t.Fatalf("GetWalletInfo = %+v, %v", wi, err)
	}
	if s.path != "/wallet/shop" {
		t.Errorf("getwalletinfo path %s", s.path)
	}

	c, s = serve(t, cannedDescInfo)
	di, err := c.GetDescriptorInfo(ctx, "wpkh(tpub/0/*)")
	if err != nil || !di.IsRange || !di.IsSolvable || di.HasPrivateKeys || di.Checksum != "wxyz5678" || !strings.HasPrefix(di.Descriptor, "wpkh(") {
		t.Fatalf("GetDescriptorInfo = %+v, %v", di, err)
	}
	if p := s.body["params"].(map[string]any); p["descriptor"] != "wpkh(tpub/0/*)" || s.path != "/" {
		t.Errorf("sent %v on %s", s.body, s.path)
	}

	c, s = serve(t, cannedDescs)
	ds, err := c.ListDescriptors(ctx)
	if err != nil || ds.WalletName != "shop" || len(ds.Descriptors) != 2 {
		t.Fatalf("ListDescriptors = %+v, %v", ds, err)
	}
	if d := ds.Descriptors[0]; !d.Active || d.Internal || d.NextIndex != 12 || d.Range != [2]int64{0, 1011} {
		t.Errorf("descriptor 0 = %+v", d)
	}
	if d := ds.Descriptors[1]; d.Active || d.NextIndex != 0 || d.Range != [2]int64{} || !strings.HasPrefix(d.Desc, "addr(") {
		t.Errorf("descriptor 1 = %+v", d)
	}
	if s.path != "/wallet/shop" {
		t.Errorf("listdescriptors path %s", s.path)
	}

	c, s = serve(t, `"`+tAddr+`"`)
	a, err := c.GetNewAddress(ctx, "bech32")
	if err != nil || a != tAddr {
		t.Fatalf("GetNewAddress = %q, %v", a, err)
	}
	if p := s.body["params"].(map[string]any); p["label"] != "" || p["address_type"] != "bech32" || len(p) != 2 || s.path != "/wallet/shop" {
		t.Errorf("sent %v on %s", s.body, s.path)
	}

	c, s = serve(t, cannedAddrInfo)
	ai, err := c.GetAddressInfo(ctx, tAddr)
	if err != nil || ai.Address != tAddr || !ai.IsMine || ai.HDKeyPath != "m/84h/1h/0h/0/7" || !strings.HasPrefix(ai.ParentDesc, "wpkh(") || !strings.HasPrefix(ai.Desc, "wpkh(") {
		t.Fatalf("GetAddressInfo = %+v, %v", ai, err)
	}
	if p := s.body["params"].(map[string]any); p["address"] != tAddr || s.path != "/wallet/shop" {
		t.Errorf("sent %v on %s", s.body, s.path)
	}

	// a bare xpub/0/* descriptor has no origin: the key path is m/0/<i>
	c, _ = serve(t, `{"address":"`+tAddr+`","ismine":true,"parent_desc":"wpkh(tpubD6NzVbkrYhZ4/0/*)#abcd1234","hdkeypath":"m/0/7"}`)
	if ai, err = c.GetAddressInfo(ctx, tAddr); err != nil || ai.HDKeyPath != "m/0/7" {
		t.Fatalf("no-origin GetAddressInfo = %+v, %v", ai, err)
	}

	c, s = serve(t, `["`+tAddr+`"]`)
	addrs, err := c.DeriveAddresses(ctx, "wpkh(tpub/0/*)#abcd1234", 7)
	if err != nil || len(addrs) != 1 || addrs[0] != tAddr {
		t.Fatalf("DeriveAddresses = %v, %v", addrs, err)
	}
	p := s.body["params"].(map[string]any)
	if r, _ := p["range"].([]any); len(r) != 2 || r[0] != float64(7) || r[1] != float64(7) || p["descriptor"] != "wpkh(tpub/0/*)#abcd1234" || s.path != "/" {
		t.Errorf("sent %v on %s", s.body, s.path)
	}

	c, s = serve(t, cannedSince)
	sb, err := c.ListSinceBlock(ctx, tBlk, 7)
	if err != nil || sb.Dropped != 0 || sb.LastBlock != tBlk2 || len(sb.Transactions) != 2 || len(sb.Removed) != 1 {
		t.Fatalf("ListSinceBlock = %+v, %v", sb, err)
	}
	r0 := sb.Transactions[0]
	if r0.Category != "receive" || r0.Address != tAddr || r0.AmountSat != 150_000 || r0.Vout != 1 || r0.TxID != tTx1 || r0.Confirmations != 0 || r0.BlockHash != "" ||
		len(r0.MempoolConflicts) != 1 || r0.MempoolConflicts[0] != tTx2 || len(r0.WalletConflicts) != 0 {
		t.Errorf("receive entry = %+v", r0)
	}
	s1 := sb.Transactions[1]
	if s1.Category != "send" || s1.AmountSat != -200_000 || s1.Confirmations != 3 || s1.BlockHash != tBlk || s1.BlockHeight != 208 || len(s1.WalletConflicts) != 1 {
		t.Errorf("send entry = %+v", s1)
	}
	if rm := sb.Removed[0]; rm.AmountSat != 100_000_000 || rm.Confirmations != -2 {
		t.Errorf("removed entry = %+v", rm)
	}
	p = s.body["params"].(map[string]any)
	if p["blockhash"] != tBlk || p["target_confirmations"] != float64(7) || p["include_removed"] != true || p["include_change"] != false || s.path != "/wallet/shop" {
		t.Errorf("sent %v on %s", s.body, s.path)
	}
	if _, err := c.ListSinceBlock(ctx, "", 1); err != nil {
		t.Fatal(err)
	}
	p = s.body["params"].(map[string]any)
	if _, has := p["blockhash"]; has {
		t.Errorf("blockhash sent when empty: %v", p)
	}

	c, s = serve(t, cannedGetTx)
	tx, err := c.GetTransaction(ctx, tTx1)
	if err != nil {
		t.Fatal(err)
	}
	if tx.TxID != tTx1 || tx.Confirmations != 4 || tx.BlockHash != tBlk || tx.BlockHeight != 207 || len(tx.MempoolConflicts) != 1 || len(tx.Outputs) != 2 || len(tx.Inputs) != 1 || tx.Inputs[0] != (Outpoint{TxID: tTx2, Vout: 1}) ||
		tx.Outputs[0] != (Output{Vout: 0, ValueSat: 150_000, Address: tAddr}) || tx.Outputs[1] != (Output{Vout: 1, ValueSat: 50_000_000}) {
		t.Errorf("GetTransaction = %+v", tx)
	}
	if p = s.body["params"].(map[string]any); p["txid"] != tTx1 || p["verbose"] != true || s.path != "/wallet/shop" {
		t.Errorf("sent %v on %s", s.body, s.path)
	}

	c, s = serve(t, `{"vsize":141,"fees":{"base":0.00000141}}`)
	in, err := c.InMempool(ctx, tTx1)
	if err != nil || !in {
		t.Fatalf("InMempool = %v, %v", in, err)
	}
	if p = s.body["params"].(map[string]any); p["txid"] != tTx1 || s.body["method"] != "getmempoolentry" || s.path != "/" {
		t.Errorf("sent %v on %s", s.body, s.path)
	}
	c = rpcErrServer(t, 500, -5, "Transaction not in mempool")
	if in, err = c.InMempool(ctx, tTx1); err != nil || in {
		t.Fatalf("code -5: %v, %v", in, err)
	}
	c = rpcErrServer(t, 500, -28, "Loading block index")
	if _, err = c.InMempool(ctx, tTx1); err == nil {
		t.Fatal("code -28 must be an error")
	}

	// getmempoolinfo's loaded (Core src/rpc/mempool.cpp, MempoolInfoToJSON: GetLoadTried), on the node endpoint.
	for _, want := range []bool{true, false} {
		c, s = serve(t, strings.Replace(cannedMempoolInfo, `"loaded":true`, fmt.Sprintf(`"loaded":%v`, want), 1))
		loaded, err := c.MempoolLoaded(ctx)
		if err != nil || loaded != want {
			t.Fatalf("MempoolLoaded = %v, %v, want %v", loaded, err, want)
		}
		if s.body["method"] != "getmempoolinfo" || s.path != "/" {
			t.Errorf("sent %v on %s", s.body, s.path)
		}
		if p, ok := s.body["params"].(map[string]any); !ok || len(p) != 0 {
			t.Errorf("params %v, want an empty object", s.body["params"])
		}
	}
	c = rpcErrServer(t, 500, -5, "x") // unlike getmempoolentry, no code of getmempoolinfo is an answer
	if _, err = c.MempoolLoaded(ctx); err == nil {
		t.Fatal("an error from getmempoolinfo must be an error")
	}
}

func TestTypedCallArgumentChecks(t *testing.T) {
	c, s := serve(t, `{}`)
	ctx := context.Background()
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrArgument) {
			t.Errorf("%s: err = %v, want ErrArgument", name, err)
		}
	}
	_, err := c.GetNewAddress(ctx, "legacy")
	check("legacy type", err)
	_, err = c.GetNewAddress(ctx, "")
	check("empty type", err)
	_, err = c.GetTransaction(ctx, strings.ToUpper(tTx1))
	check("upper txid", err)
	_, err = c.GetTransaction(ctx, tTx1[:63])
	check("short txid", err)
	_, err = c.InMempool(ctx, "zz")
	check("txid", err)
	_, err = c.ListSinceBlock(ctx, "xyz", 1)
	check("blockhash", err)
	_, err = c.ListSinceBlock(ctx, "", 0)
	check("target 0", err)
	_, err = c.ListSinceBlock(ctx, "", 100001)
	check("target big", err)
	_, err = c.DeriveAddresses(ctx, "wpkh(x)", -1)
	check("neg index", err)
	_, err = c.DeriveAddresses(ctx, "wpkh(x)", 1<<31)
	check("big index", err)
	_, err = c.DeriveAddresses(ctx, "", 0)
	check("empty descriptor", err)
	_, err = c.GetDescriptorInfo(ctx, "")
	check("empty descriptor", err)
	_, err = c.GetAddressInfo(ctx, "")
	check("empty address", err)
	_, err = c.GetAddressInfo(ctx, strings.Repeat("a", 129))
	check("long address", err)
	if s.n.Load() != 0 {
		t.Fatalf("%d requests were made", s.n.Load())
	}
}

// Each typed call refuses an answer of the wrong shape.
func TestTypedCallsBadShapes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		call func(c *Client) error
		bad  []string
	}{
		{"getblockchaininfo", func(c *Client) error { _, e := c.GetBlockchainInfo(ctx); return e },
			[]string{`null`, `[]`, `"x"`, `{}`, `{"chain":"regtest","blocks":"1","headers":1,"bestblockhash":"` + tBlk + `","initialblockdownload":false}`,
				`{"chain":"regtest","blocks":1,"headers":1,"bestblockhash":"zz","initialblockdownload":false}`,
				`{"chain":"regtest","blocks":1,"headers":1,"bestblockhash":"` + tBlk + `"}`,
				`{"chain":"regtest","blocks":-1,"headers":1,"bestblockhash":"` + tBlk + `","initialblockdownload":false}`,
				`{"chain":"","blocks":1,"headers":1,"bestblockhash":"` + tBlk + `","initialblockdownload":false}`,
				`{"chain":7,"blocks":1,"headers":1,"bestblockhash":"` + tBlk + `","initialblockdownload":false}`}},
		{"getwalletinfo", func(c *Client) error { _, e := c.GetWalletInfo(ctx); return e },
			[]string{`null`, `{}`, `{"walletname":"w","descriptors":true}`, `{"walletname":"w","private_keys_enabled":false}`,
				`{"walletname":"w","descriptors":"true","private_keys_enabled":false}`}},
		{"getdescriptorinfo", func(c *Client) error { _, e := c.GetDescriptorInfo(ctx, "x"); return e },
			[]string{`null`, `{}`, `{"descriptor":"d","checksum":"abcdefgh","isrange":true,"issolvable":true}`,
				`{"descriptor":"d","checksum":"abc","isrange":true,"issolvable":true,"hasprivatekeys":false}`,
				`{"descriptor":"","checksum":"abcdefgh","isrange":true,"issolvable":true,"hasprivatekeys":false}`,
				`{"descriptor":"d","checksum":"abcdefgh","isrange":1,"issolvable":true,"hasprivatekeys":false}`}},
		{"listdescriptors", func(c *Client) error { _, e := c.ListDescriptors(ctx); return e },
			[]string{`null`, `{}`, `{"wallet_name":"w"}`, `{"wallet_name":"w","descriptors":{}}`,
				`{"wallet_name":"w","descriptors":[{"desc":"d"}]}`,
				`{"wallet_name":"w","descriptors":[{"desc":"d","active":true,"range":[0],"next_index":0}]}`,
				`{"wallet_name":"w","descriptors":[{"desc":"d","active":true,"range":[0,1,2],"next_index":0}]}`,
				`{"wallet_name":"w","descriptors":[{"desc":"d","active":true,"range":[5,1],"next_index":0}]}`,
				`{"wallet_name":"w","descriptors":[{"desc":"d","active":true,"range":[0,1],"next_index":-1}]}`,
				`{"wallet_name":"w","descriptors":[{"desc":"d","active":true,"range":[0,1],"next_index":"1"}]}`,
				`{"wallet_name":"w","descriptors":[{"desc":"d","active":true,"range":[0,1.5],"next_index":0}]}`}},
		{"getnewaddress", func(c *Client) error { _, e := c.GetNewAddress(ctx, "bech32"); return e },
			[]string{`null`, `""`, `1`, `{}`, `["a"]`}},
		{"getaddressinfo", func(c *Client) error { _, e := c.GetAddressInfo(ctx, "a"); return e },
			[]string{`null`, `{}`, `{"address":"a"}`, `{"address":"a","ismine":"yes"}`, `{"ismine":true}`, `{"address":"a","ismine":true,"hdkeypath":5}`}},
		{"deriveaddresses", func(c *Client) error { _, e := c.DeriveAddresses(ctx, "d", 1); return e },
			[]string{`null`, `{}`, `[]`, `[1]`, `["a","b"]`, `[""]`, `"a"`}},
		{"listsinceblock", func(c *Client) error { _, e := c.ListSinceBlock(ctx, "", 1); return e },
			[]string{`null`, `{}`, `{"transactions":{},"lastblock":"` + tBlk + `"}`, `{"transactions":[],"lastblock":"` + tBlk + `"}x`,
				`{"transactions":[],"removed":{},"lastblock":"` + tBlk + `"}`,
				`{"transactions":[]}`, `{"transactions":[],"lastblock":"abc"}`, `{"transactions":[],"lastblock":1}`, `{"lastblock":"` + tBlk + `"}`}},
		{"gettransaction", func(c *Client) error { _, e := c.GetTransaction(ctx, tTx1); return e },
			[]string{`null`, `{}`, `{"txid":"` + tTx1 + `"}`,
				`{"txid":"` + tTx1 + `","confirmations":"1","decoded":{"vin":[],"vout":[]}}`,
				`{"txid":"` + tTx2 + `","confirmations":1,"decoded":{"vin":[],"vout":[]}}`,
				`{"txid":"` + tTx1 + `","confirmations":1,"decoded":{"vin":[],"vout":[{"value":"abc","n":0}]}}`,
				`{"txid":"` + tTx1 + `","confirmations":1,"decoded":{"vin":[],"vout":[{"value":1,"n":4294967296}]}}`,
				`{"txid":"` + tTx1 + `","confirmations":1,"decoded":{"vin":[],"vout":[{"value":1,"n":-1}]}}`,
				`{"txid":"` + tTx1 + `","confirmations":1,"decoded":{"vin":[],"vout":[{"value":-1,"n":0}]}}`,
				`{"txid":"` + tTx1 + `","confirmations":1,"decoded":{"vin":[],"vout":[{"value":21000000.00000001,"n":0}]}}`,
				`{"txid":"` + tTx1 + `","confirmations":1,"decoded":{"vin":[],"vout":{}}}`,
				`{"txid":"` + tTx1 + `","confirmations":1,"decoded":{"vin":[],"vout":[{"value":1,"n":0,"scriptPubKey":{"address":5}}]}}`,
				`{"txid":"` + tTx1 + `","confirmations":1,"mempoolconflicts":["zz"],"decoded":{"vin":[],"vout":[]}}`}},
		{"getmempoolentry", func(c *Client) error { _, e := c.InMempool(ctx, tTx1); return e },
			[]string{`null`}},
		{"gettxspendingprevout", func(c *Client) error { _, e := c.SpentInMempool(ctx, []Outpoint{{tTx1, 0}}); return e },
			[]string{`null`, `{}`, `[]`, `[{}]`}},
		{"getmempoolinfo", func(c *Client) error { _, e := c.MempoolLoaded(ctx); return e },
			[]string{`null`, `[]`, `true`, `{}`, `{"size":1}`, `{"loaded":"true"}`, `{"loaded":1}`, `{"loaded":null}`}},
	}
	for _, tc := range cases {
		for _, b := range tc.bad {
			c, _ := serve(t, b)
			if err := tc.call(c); !errors.Is(err, ErrBadAnswer) {
				t.Errorf("%s with %s: err = %v, want ErrBadAnswer", tc.name, b, err)
			}
		}
	}
}

func TestUnknownFieldsIgnored(t *testing.T) {
	c, _ := serve(t, `{"chain":"main","blocks":1,"headers":1,"bestblockhash":"`+tBlk+`","initialblockdownload":true,"brand_new_field":{"a":[1,2]}}`)
	bi, err := c.GetBlockchainInfo(context.Background())
	if err != nil || !bi.InitialBlockDownload {
		t.Fatalf("%+v, %v", bi, err)
	}
}

func TestListSinceBlockDropsOddEntries(t *testing.T) {
	good := func(vout int, txid string) string {
		return fmt.Sprintf(`{"address":"%s","category":"receive","amount":0.001,"vout":%d,"confirmations":1,"txid":"%s"}`, tAddr, vout, txid)
	}
	entries := []string{
		good(0, tTx1),
		`{"address":"` + tAddr + `","category":"receive","amount":0.001,"vout":4294967296,"confirmations":1,"txid":"` + tTx1 + `"}`,
		good(1, tTx1),
		`{"address":"` + tAddr + `","category":"receive","amount":"abc","vout":1,"confirmations":1,"txid":"` + tTx1 + `"}`,
		good(2, tTx2),
		`{"address":"` + tAddr + `","category":"receive","amount":0.001,"vout":1,"confirmations":1,"txid":"` + tTx1[:63] + `"}`,
		good(4294967295, tTx2),
		`{"address":"` + tAddr + `","category":"receive","amount":0.001,"vout":1,"confirmations":"1","txid":"` + tTx1 + `"}`,
	}
	body := `{"transactions":[` + strings.Join(entries, ",") + `],"lastblock":"` + tBlk + `"}`
	c, _ := serve(t, body)
	sb, err := c.ListSinceBlock(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Dropped != 4 || len(sb.Transactions) != 4 {
		t.Fatalf("Dropped %d, kept %d", sb.Dropped, len(sb.Transactions))
	}
	wantVouts := []uint32{0, 1, 2, 4294967295}
	for i, tx := range sb.Transactions {
		if tx.Vout != wantVouts[i] {
			t.Errorf("entry %d vout %d, want %d", i, tx.Vout, wantVouts[i])
		}
	}

	// more odd entries: not an object, missing fields, bad hash, negative vout, fractional vout, a bad conflict txid
	odd := []string{
		`1`, `null`, `[]`, `"x"`, `{}`,
		`{"category":"receive","amount":1,"confirmations":1,"txid":"` + tTx1 + `"}`,
		`{"category":"receive","amount":1,"vout":-1,"confirmations":1,"txid":"` + tTx1 + `"}`,
		`{"category":"receive","amount":1,"vout":1.5,"confirmations":1,"txid":"` + tTx1 + `"}`,
		`{"category":"receive","amount":1,"vout":0,"confirmations":1,"txid":"` + strings.ToUpper(tTx1) + `"}`,
		`{"category":"receive","amount":1,"vout":0,"confirmations":1,"txid":"` + tTx1 + `","blockhash":"zz"}`,
		`{"category":"receive","amount":1,"vout":0,"confirmations":1,"txid":"` + tTx1 + `","mempoolconflicts":["zz"]}`,
		`{"category":"receive","amount":1,"vout":0,"confirmations":1,"txid":"` + tTx1 + `","walletconflicts":"x"}`,
		`{"category":"receive","amount":1e-5,"vout":0,"confirmations":1,"txid":"` + tTx1 + `"}`,
		`{"category":"receive","amount":21000000.00000001,"vout":0,"confirmations":1,"txid":"` + tTx1 + `"}`,
		`{"category":"receive","amount":0.123456789,"vout":0,"confirmations":1,"txid":"` + tTx1 + `"}`,
		`{"category":7,"amount":1,"vout":0,"confirmations":1,"txid":"` + tTx1 + `"}`,
		`{"category":"receive","amount":1,"vout":0,"confirmations":1,"txid":"` + tTx1 + `","address":3}`,
	}
	body = `{"transactions":[` + strings.Join(odd, ",") + `],"removed":[` + strings.Join(odd, ",") + `],"lastblock":"` + tBlk + `"}`
	c, _ = serve(t, body)
	sb, err = c.ListSinceBlock(context.Background(), "", 1)
	if err != nil || len(sb.Transactions) != 0 || len(sb.Removed) != 0 || sb.Dropped != 2*len(odd) {
		t.Fatalf("odd: %+v, %v", sb, err)
	}
}

func TestGetTransactionOutputWithoutAddress(t *testing.T) {
	c, _ := serve(t, `{"txid":"`+tTx1+`","confirmations":0,"decoded":{"vin":[],"vout":[{"value":0.1,"n":3,"scriptPubKey":{"type":"nulldata"}}]}}`)
	tx, err := c.GetTransaction(context.Background(), tTx1)
	if err != nil || len(tx.Outputs) != 1 || tx.Outputs[0] != (Output{Vout: 3, ValueSat: 10_000_000}) || tx.BlockHash != "" || tx.BlockHeight != 0 {
		t.Fatalf("%+v, %v", tx, err)
	}
}

// Config never prints its password: not through fmt, encoding/json, slog's JSON and text handlers.
// TestClientRedaction: the encoders and loggers a Client can reach print no form of the password. Config
// holds none (New takes it apart); fmt's verbs are TestClientEveryVerb's.
func TestClientRedaction(t *testing.T) {
	c, err := New(Config{URL: "http://127.0.0.1:1", User: tUser, Wallet: "shop"}, []byte(tPass))
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(tPass))
	hex := fmt.Sprintf("%x", []byte(tPass))
	var outs []string
	for _, x := range []any{c, []*Client{c}, map[string]any{"c": c}, struct{ C *Client }{c}} {
		j, err := json.Marshal(x)
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, string(j))
	}
	for _, mk := range []func(io.Writer) slog.Handler{
		func(w io.Writer) slog.Handler { return slog.NewJSONHandler(w, nil) },
		func(w io.Writer) slog.Handler { return slog.NewTextHandler(w, nil) },
	} {
		var buf strings.Builder
		l := slog.New(mk(&buf))
		l.Info("start", "client", c, slog.Any("c", c), slog.Group("g", slog.Any("c", struct{ C *Client }{c})))
		outs = append(outs, buf.String())
	}
	for _, o := range outs {
		if o == "" || strings.Contains(o, tPass) || strings.Contains(o, b64) || strings.Contains(o, hex) {
			t.Errorf("a rendering of the client holds the password (or is empty): %.80q", o)
		}
	}
}

// TestTransportErrorHoldsNoAnswerBytes: Go's HTTP transport quotes a malformed answer into its
// error ("malformed HTTP response \"...\""), so the client keeps none of that error: whatever answers at the
// URL puts none of its bytes into the client's error text.
func TestTransportErrorHoldsNoAnswerBytes(t *testing.T) {
	const sentinel = "SENTINEL-ANSWER-BYTES"
	for _, answer := range []string{
		sentinel + "\r\n\r\n",
		"HTTP/1.1 2x0 " + sentinel + "\r\n\r\n",
		"HTTP/1.1 200 OK\r\n" + sentinel + "\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Length: " + sentinel + "\r\n\r\n",
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" + sentinel + "\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n" + sentinel,
	} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					buf := make([]byte, 64<<10)
					_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
					_, _ = conn.Read(buf)
					_, _ = io.WriteString(conn, answer)
				}()
			}
		}()
		c, err := New(Config{URL: "http://" + ln.Addr().String(), User: tUser, Wallet: "shop", Timeout: 2 * time.Second}, []byte(tPass))
		if err != nil {
			t.Fatal(err)
		}
		err = c.Call(context.Background(), "getblockchaininfo", nil, nil)
		ln.Close()
		if err == nil {
			t.Fatalf("answer %q: no error", answer)
		}
		if text := fmt.Sprintf("%v %+v %#v", err, err, err); strings.Contains(text, sentinel) {
			t.Errorf("answer %q: the error holds the answer's bytes: %s", answer, text)
		}
		if !errors.Is(err, ErrTransport) && !errors.Is(err, ErrBadAnswer) && !errors.Is(err, ErrHTTP) {
			t.Errorf("answer %q: err %v", answer, err)
		}
	}
}

func TestValidPort(t *testing.T) {
	for p, want := range map[string]bool{"0": false, "1": true, "80": true, "65535": true, "65536": false, "": false,
		"01": false, "+80": false, "-1": false, "8 0": false, "0x50": false} {
		if got := ValidPort(p); got != want {
			t.Errorf("ValidPort(%q) = %v", p, got)
		}
	}
}
