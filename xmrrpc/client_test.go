package xmrrpc

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	tUser = "satbooth"
	tPass = "test-password-2"
)

func md5hex(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }

// refResponse is the test's own computation of a qop=auth MD5 response (RFC 2617 §3.2.2.1).
func refResponse(user, realm, pass, method, uri, nonce, nc, cnonce string) string {
	return md5hex(md5hex(user+":"+realm+":"+pass) + ":" + nonce + ":" + nc + ":" + cnonce + ":auth:" + md5hex(method+":"+uri))
}

// fake is wallet-rpc's HTTP front as epee implements it (contrib/epee/src/http_auth.cpp,
// http_server_auth::do_get_response and auth_message::verify), with one session nonce and one counter for all
// connections. epee keeps them per connection (http_protocol_handler.h:190 and :227 at v0.18.5.4); one for all is
// stricter, so two requests in flight through one client fail here where they might pass on epee. Every request
// that carries an Authorization header increments the counter; it passes when the response is right, the nonce is
// the current one and nc equals the counter (stricter than epee, which also passes counter 1 whatever nc says); a
// right response with an old nonce or a wrong nc is stale; anything else fails. A request that does not pass gets a new nonce, counter 0, and the recorded challenge lines.
type fake struct {
	t     *testing.T
	pass  string
	lines []string // the recorded WWW-authenticate values

	mu      sync.Mutex
	nonce   string
	counter uint32
	nonces  []string // every nonce issued
	seq     int

	requests  atomic.Int32
	inFlight  atomic.Int32
	overlap   atomic.Bool
	always401 bool
	staleAll  bool        // with always401: every challenge says stale=true
	rotate    atomic.Bool // the next authenticated request is treated as stale
	sleep     time.Duration
	hang      chan struct{} // under mu: if set, an authenticated request waits on it (or on its context)

	last struct {
		sync.Mutex
		method, path, ctype string
		body                map[string]json.RawMessage
	}
	// reply answers an authenticated request; nil answers with a result of {}.
	reply func(w http.ResponseWriter, method string, params json.RawMessage)
}

func (f *fake) setHang(ch chan struct{}) { f.mu.Lock(); f.hang = ch; f.mu.Unlock() }

func newFake(t *testing.T) *fake {
	return &fake{t: t, pass: tPass, lines: recordedChallenges(t)}
}

func (f *fake) newNonce() string {
	f.seq++
	n := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("nonce-%010d", f.seq)))
	f.nonces = append(f.nonces, n)
	return n
}

func (f *fake) verify(r *http.Request, auth string) (pass, stale bool) {
	if !strings.HasPrefix(auth, "Digest ") {
		return false, false
	}
	vals, quoted, err := parseAuth(auth)
	if err != nil {
		f.t.Error(err)
		return false, false
	}
	checkAuthShape(f.t, vals, quoted)
	if vals["uri"] != r.URL.RequestURI() {
		f.t.Errorf("uri %q, request %q", vals["uri"], r.URL.RequestURI())
	}
	if vals["username"] != tUser || vals["realm"] != "monero-rpc" ||
		vals["response"] != refResponse(tUser, "monero-rpc", f.pass, r.Method, vals["uri"], vals["nonce"], vals["nc"], vals["cnonce"]) {
		return false, false
	}
	nc, _ := strconv.ParseUint(vals["nc"], 16, 32)
	if vals["nonce"] != f.nonce || uint32(nc) != f.counter || f.rotate.Swap(false) {
		return false, true
	}
	return true, false
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	pass, stale := false, false
	if auth := r.Header.Get("Authorization"); auth != "" {
		f.counter++
		pass, stale = f.verify(r, auth)
	}
	if f.always401 {
		pass, stale = false, f.staleAll
	}
	if !pass {
		f.counter = 0
		f.nonce = f.newNonce()
		for _, l := range f.lines {
			l = strings.Replace(l, recordedNonce, f.nonce, 1)
			if stale {
				l = strings.Replace(l, "stale=false", "stale=true", 1)
			}
			w.Header()["WWW-authenticate"] = append(w.Header()["WWW-authenticate"], l)
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "<html><head><title>Unauthorized Access</title></head><body><h1>401 Unauthorized</h1></body></html>")
		return
	}
	hang := f.hang
	f.mu.Unlock()

	if f.inFlight.Add(1) > 1 {
		f.overlap.Store(true)
	}
	defer f.inFlight.Add(-1)
	if f.sleep > 0 {
		time.Sleep(f.sleep)
	}
	if hang != nil {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
		return
	}
	var env map[string]json.RawMessage
	_ = json.Unmarshal(raw, &env)
	var method string
	_ = json.Unmarshal(env["method"], &method)
	f.last.Lock()
	f.last.method, f.last.path, f.last.ctype, f.last.body = r.Method, r.URL.Path, r.Header.Get("Content-Type"), env
	f.last.Unlock()
	if f.reply != nil {
		f.reply(w, method, env["params"])
		return
	}
	io.WriteString(w, `{"id":"0","jsonrpc":"2.0","result":{}}`)
}

// result answers with the epee success shape around result.
func result(result string) func(http.ResponseWriter, string, json.RawMessage) {
	return func(w http.ResponseWriter, _ string, _ json.RawMessage) {
		io.WriteString(w, `{"id":"0","jsonrpc":"2.0","result":`+result+`}`)
	}
}

// rpcError answers with the epee error shape.
func rpcError(code int, msg string) func(http.ResponseWriter, string, json.RawMessage) {
	return func(w http.ResponseWriter, _ string, _ json.RawMessage) {
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": msg}, "id": "0", "jsonrpc": "2.0"})
		w.Write(b)
	}
}

func serveFake(t *testing.T, f *fake) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL, User: tUser, Timeout: 5 * time.Second}, []byte(tPass))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCallAuthenticates(t *testing.T) {
	f := newFake(t)
	c := serveFake(t, f)
	if err := c.Call(context.Background(), "get_version", nil, nil); err != nil {
		t.Fatal(err)
	}
	if n := f.requests.Load(); n != 2 {
		t.Fatalf("first call made %d requests, want 2", n)
	}
	var out map[string]any
	if err := c.Call(context.Background(), "get_height", map[string]any{"a": 1}, &out); err != nil {
		t.Fatal(err)
	}
	if n := f.requests.Load(); n != 3 {
		t.Fatalf("second call made %d requests in all, want 3", n)
	}
	f.last.Lock()
	if f.last.method != "POST" || f.last.path != "/json_rpc" || f.last.ctype != "application/json" {
		t.Errorf("method %q path %q content-type %q", f.last.method, f.last.path, f.last.ctype)
	}
	b := f.last.body
	if string(b["jsonrpc"]) != `"2.0"` || string(b["id"]) != `"0"` || string(b["method"]) != `"get_height"` || string(b["params"]) != `{"a":1}` {
		t.Errorf("envelope %s %s %s %s", b["jsonrpc"], b["id"], b["method"], b["params"])
	}
	f.last.Unlock()
	if err := c.Call(context.Background(), "store", nil, nil); err != nil {
		t.Fatal(err)
	}
	if p := paramsOf(t, f); len(p) != 0 {
		t.Errorf("nil params sent as %v, want {}", p)
	}
}

func TestDigestOneRetry(t *testing.T) {
	for _, stale := range []bool{false, true} {
		f := newFake(t)
		f.always401, f.staleAll = true, stale
		c := serveFake(t, f)
		for call := 1; call <= 2; call++ {
			err := c.Call(context.Background(), "get_version", nil, nil)
			if !errors.Is(err, ErrAuth) {
				t.Fatalf("stale=%v call %d: err = %v, want ErrAuth", stale, call, err)
			}
			if n := f.requests.Load(); n != int32(2*call) {
				t.Fatalf("stale=%v: %d requests after call %d, want %d", stale, n, call, 2*call)
			}
			text := fmt.Sprintf("%v %+v %#v", err, err, err)
			if strings.Contains(text, tPass) {
				t.Fatal("the error holds the password")
			}
			f.mu.Lock()
			for _, n := range f.nonces {
				if strings.Contains(text, n) {
					t.Fatalf("the error holds the nonce %q", n)
				}
			}
			f.mu.Unlock()
		}
	}
}

// TestDigestUnusableChallenge: a 401 whose challenges SatBooth cannot use is ErrAuth after one request.
func TestDigestUnusableChallenge(t *testing.T) {
	for _, lines := range [][]string{
		nil,
		{`Basic realm="monero-rpc"`},
		{`Digest qop="auth",algorithm=MD5-sess,realm="monero-rpc",nonce="` + recordedNonce + `"`},
		{`Digest algorithm=MD5,realm="monero-rpc",nonce="` + recordedNonce + `"`},
	} {
		f := newFake(t)
		f.lines = lines
		c := serveFake(t, f)
		if err := c.Call(context.Background(), "get_version", nil, nil); !errors.Is(err, ErrAuth) {
			t.Errorf("challenges %q: err = %v", lines, err)
		}
		if n := f.requests.Load(); n != 1 {
			t.Errorf("challenges %q: %d requests", lines, n)
		}
	}
}

// TestStaleNonce: call 1 authenticates in two requests; the server then rotates the nonce,
// so call 2's request with the kept nonce gets a stale 401, and its one retry with the new nonce succeeds:
// four requests in all.
func TestStaleNonce(t *testing.T) {
	f := newFake(t)
	c := serveFake(t, f)
	if err := c.Call(context.Background(), "get_version", nil, nil); err != nil {
		t.Fatal(err)
	}
	f.rotate.Store(true)
	if err := c.Call(context.Background(), "get_version", nil, nil); err != nil {
		t.Fatalf("after a stale nonce: %v", err)
	}
	if n := f.requests.Load(); n != 4 {
		t.Fatalf("%d requests, want 4", n)
	}
	// and the new nonce is kept: one request for the next call
	if err := c.Call(context.Background(), "get_version", nil, nil); err != nil || f.requests.Load() != 5 {
		t.Fatalf("err %v, %d requests", err, f.requests.Load())
	}
}

// TestCounterHealsAfterLostRequest: a request the server counted but whose answer the client never read
// (here: a timeout) leaves the counters apart; the next call heals with one stale retry.
func TestCounterHealsAfterLostRequest(t *testing.T) {
	f := newFake(t)
	c := serveFake(t, f)
	if err := c.Call(context.Background(), "get_version", nil, nil); err != nil {
		t.Fatal(err)
	}
	hang := make(chan struct{})
	f.setHang(hang)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := c.Call(ctx, "get_version", nil, nil)
	cancel()
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("err = %v", err)
	}
	close(hang)
	f.setHang(nil)
	// the server's counter is now 2 and the client's 2: in step. Lose one the server never saw:
	c.flight <- struct{}{}
	c.creds.d.nc++
	<-c.flight
	before := f.requests.Load()
	if err := c.Call(context.Background(), "get_version", nil, nil); err != nil {
		t.Fatal(err)
	}
	if n := f.requests.Load() - before; n != 2 {
		t.Fatalf("%d requests to heal, want 2", n)
	}
}

func TestCallsAreSerial(t *testing.T) {
	f := newFake(t)
	f.sleep = 20 * time.Millisecond
	c := serveFake(t, f)
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for range 50 {
		wg.Go(func() { errs <- c.Call(context.Background(), "get_height", nil, nil) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.overlap.Load() {
		t.Fatal("two requests were in flight at once")
	}
	if n := f.requests.Load(); n != 51 {
		t.Fatalf("%d requests, want 51 (one challenge, then one each)", n)
	}
}

// TestQueuedCallHonoursContext: a call waiting for the one in flight gives up when its context ends.
func TestQueuedCallHonoursContext(t *testing.T) {
	f := newFake(t)
	c := serveFake(t, f)
	if err := c.Call(context.Background(), "get_version", nil, nil); err != nil {
		t.Fatal(err)
	}
	hang := make(chan struct{})
	f.setHang(hang)
	defer close(hang)
	go c.Call(context.Background(), "get_version", nil, nil)
	for f.inFlight.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	before := f.requests.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Call(ctx, "get_version", nil, nil)
	if !errors.Is(err, ErrTransport) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("waited %v", d)
	}
	if f.requests.Load() != before {
		t.Fatal("the queued call sent a request")
	}
}

func TestAllowedMethodsOrder(t *testing.T) {
	want := "get_version,get_height,get_address,create_address,store,get_transfers,get_transfer_by_txid,sign_transfer"
	if got := strings.Join(allowedMethods, ","); got != want {
		t.Fatalf("allowedMethods = %s", got)
	}
}

func TestMethodAllowlist(t *testing.T) {
	f := newFake(t)
	c := serveFake(t, f)
	for _, m := range []string{"set_daemon", "query_key", "transfer", "open_wallet", "stop_wallet", "rescan_blockchain",
		"generate_from_keys", "create_wallet", "refresh", "get_balance", "", "GET_VERSION", "get_version ", "get_version\x00",
		"sign_transfer\n", "transfer_split", "relay_tx", "submit_transfer"} {
		if err := c.Call(context.Background(), m, nil, nil); !errors.Is(err, ErrMethod) {
			t.Errorf("method %q: err = %v, want ErrMethod", m, err)
		}
	}
	if n := f.requests.Load(); n != 0 {
		t.Fatalf("%d requests for refused methods", n)
	}
	// sign_transfer is on the allowlist for SignTransferProbe only: Call refuses it, whatever the params
	for _, p := range []any{nil, map[string]any{"unsigned_txset": ""}, map[string]any{"unsigned_txset": "4d6f6e65726f"}} {
		if err := c.Call(context.Background(), "sign_transfer", p, nil); !errors.Is(err, ErrMethod) {
			t.Errorf("Call sign_transfer %v: err = %v, want ErrMethod", p, err)
		}
	}
	if n := f.requests.Load(); n != 0 {
		t.Fatalf("%d requests for refused methods", n)
	}
	for _, m := range allowedMethods {
		if m == "sign_transfer" {
			continue
		}
		if err := c.Call(context.Background(), m, nil, nil); err != nil {
			t.Errorf("allowed %q: %v", m, err)
		}
	}
	// params that are not a JSON object are refused before any request
	n := f.requests.Load()
	for _, p := range []any{[]int{1}, "x", 3, make(chan int)} {
		if err := c.Call(context.Background(), "get_version", p, nil); !errors.Is(err, ErrArgument) || f.requests.Load() != n {
			t.Errorf("params %T: err %v", p, err)
		}
	}
}

func TestErrorKeepsNoMessage(t *testing.T) {
	const msg = "NEVER-IN-A-LOG"
	f := newFake(t)
	f.reply = rpcError(-13, msg)
	c := serveFake(t, f)
	err := c.Call(context.Background(), "get_height", nil, nil)
	var e *Error
	if !errors.As(err, &e) || e.Code != -13 || e.Method != "get_height" {
		t.Fatalf("err = %#v", err)
	}
	if e.Error() != "xmrrpc: error -13 from get_height" {
		t.Errorf("Error() = %q", e.Error())
	}
	if errors.Is(err, ErrTransport) || errors.Is(err, ErrHTTP) || errors.Is(err, ErrBadAnswer) {
		t.Error("a JSON-RPC error carries another sentinel")
	}
	for _, v := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		for _, x := range []any{err, e, *e} {
			if out := fmt.Sprintf(v, x); strings.Contains(out, msg) {
				t.Errorf("%s of %T holds the message: %s", v, x, out)
			}
		}
	}
	// the same under a non-200 status: still the *Error
	f.reply = func(w http.ResponseWriter, _ string, _ json.RawMessage) {
		w.WriteHeader(500)
		io.WriteString(w, `{"error":{"code":-1,"message":"`+msg+`"},"id":"0","jsonrpc":"2.0"}`)
	}
	if err := c.Call(context.Background(), "get_height", nil, nil); !errors.As(err, &e) || e.Code != -1 || strings.Contains(err.Error(), msg) {
		t.Fatalf("500 with an error body: %v", err)
	}
}

func TestHTTPStatuses(t *testing.T) {
	for _, status := range []int{403, 404, 500, 503, 302, 204} {
		f := newFake(t)
		f.reply = func(w http.ResponseWriter, _ string, _ json.RawMessage) {
			if status == 302 {
				w.Header().Set("Location", "/elsewhere")
			}
			w.WriteHeader(status)
		}
		c := serveFake(t, f)
		err := c.Call(context.Background(), "get_height", nil, nil)
		if !errors.Is(err, ErrHTTP) || !errors.Is(err, ErrTransport) || !strings.Contains(fmt.Sprint(err), strconv.Itoa(status)) {
			t.Errorf("status %d: err = %v", status, err)
		}
	}
}

func TestTrailingSlash(t *testing.T) {
	for _, suffix := range []string{"/", "/base/", "/base"} {
		f := newFake(t)
		srv := httptest.NewServer(f)
		c, err := New(Config{URL: srv.URL + suffix, User: tUser}, []byte(tPass))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Call(context.Background(), "get_height", nil, nil); err != nil {
			t.Fatalf("%s: %v", suffix, err)
		}
		want := strings.TrimSuffix(suffix, "/") + "/json_rpc"
		if f.last.path != want {
			t.Errorf("URL ending %q posted to %q, want %q", suffix, f.last.path, want)
		}
		srv.Close()
	}
}

func TestNoRedirect(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer other.Close()
	// before authentication, and after it
	redirect := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}
	srv := httptest.NewServer(http.HandlerFunc(redirect))
	defer srv.Close()
	c, err := New(Config{URL: srv.URL, User: tUser}, []byte(tPass))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Call(context.Background(), "get_height", nil, nil); !errors.Is(err, ErrHTTP) {
		t.Errorf("unauthenticated redirect: %v", err)
	}
	f := newFake(t)
	f.reply = func(w http.ResponseWriter, _ string, _ json.RawMessage) {
		w.Header().Set("Location", other.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}
	c = serveFake(t, f)
	if err := c.Call(context.Background(), "get_height", nil, nil); !errors.Is(err, ErrHTTP) {
		t.Errorf("authenticated redirect: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("a redirect was followed")
	}
}

func TestSizeCap(t *testing.T) {
	for _, extra := range []int{1, 0} {
		f := newFake(t)
		f.reply = func(w http.ResponseWriter, _ string, _ json.RawMessage) {
			head, tail := `{"id":"0","jsonrpc":"2.0","result":"`, `"}`
			io.WriteString(w, head+strings.Repeat("a", MaxResponseBytes+extra-len(head)-len(tail))+tail)
		}
		c := serveFake(t, f)
		var out string
		err := c.Call(context.Background(), "get_height", nil, &out)
		if extra == 1 && !errors.Is(err, ErrBadAnswer) {
			t.Errorf("8 MiB + 1: %v", err)
		}
		if extra == 0 && (err != nil || len(out) == 0) {
			t.Errorf("exactly 8 MiB: %v", err)
		}
	}
}

// TestTimeout: the timeout bounds the whole call, the challenge round trip and the authenticated one.
func TestTimeout(t *testing.T) {
	f := newFake(t)
	f.hang = make(chan struct{})
	defer close(f.hang)
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := New(Config{URL: srv.URL, User: tUser, Timeout: time.Second}, []byte(tPass))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = c.Call(context.Background(), "get_height", nil, nil)
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d < 900*time.Millisecond || d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
	if f.requests.Load() != 2 {
		t.Fatalf("%d requests", f.requests.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "get_height", nil, nil); !errors.Is(err, ErrTransport) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx: %v", err)
	}
	// unreachable
	dead := httptest.NewServer(http.NotFoundHandler())
	u := dead.URL
	dead.Close()
	c, _ = New(Config{URL: u, User: tUser, Timeout: time.Second}, []byte(tPass))
	if err := c.Call(context.Background(), "get_height", nil, nil); !errors.Is(err, ErrTransport) || strings.Contains(err.Error(), tPass) {
		t.Fatalf("unreachable: %v", err)
	}
}

// TestNoProxy: the proxy environment is never read, and the transport is btcrpc's with TLS 1.2 at least.
func TestNoProxy(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(502) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	c, err := New(Config{URL: "http://xmrrpc-test.invalid:18083", User: tUser, Timeout: time.Second}, []byte(tPass))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Call(context.Background(), "get_height", nil, nil); !errors.Is(err, ErrTransport) {
		t.Fatalf("err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the request went to the proxy")
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || !tr.DisableCompression || tr.MaxResponseHeaderBytes != 64<<10 || tr.MaxIdleConns != 2 ||
		tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 || c.http.CheckRedirect == nil {
		t.Fatalf("transport %+v", tr)
	}
}

func TestBadAnswers(t *testing.T) {
	bodies := []string{
		``, `not json`, `[]`, `null`, `"x"`, `{}`, `{"result":{}}`,
		`{"id":"0","result":{}}`,
		`{"id":"0","jsonrpc":"1.0","result":{}}`,
		`{"id":"0","jsonrpc":2,"result":{}}`,
		`{"id":"1","jsonrpc":"2.0","result":{}}`,
		`{"id":0,"jsonrpc":"2.0","result":{}}`,
		`{"jsonrpc":"2.0","result":{}}`,
		`{"id":"0","jsonrpc":"2.0"}`,
		`{"id":"0","jsonrpc":"2.0","result":null}`,
		`{"id":"0","jsonrpc":"2.0","error":"boom"}`,
		`{"id":"0","jsonrpc":"2.0","error":{"message":"x"}}`,
		`{"id":"0","jsonrpc":"2.0","error":{"code":"-8"}}`,
		`{"id":"0","jsonrpc":"2.0","error":{"code":1.5}}`,
		`{"id":"0","jsonrpc":"2.0","result":{}} trailing`,
		`{"id":"0","jsonrpc":"2.0","result":{}}{}`,
	}
	for _, b := range bodies {
		f := newFake(t)
		f.reply = func(w http.ResponseWriter, _ string, _ json.RawMessage) { io.WriteString(w, b) }
		c := serveFake(t, f)
		var out any
		if err := c.Call(context.Background(), "get_height", nil, &out); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("body %q: err = %v, want ErrBadAnswer", b, err)
		}
	}
	f := newFake(t)
	f.reply = result(`"text"`)
	c := serveFake(t, f)
	var out struct{ A int }
	if err := c.Call(context.Background(), "get_height", nil, &out); !errors.Is(err, ErrBadAnswer) {
		t.Errorf("wrong type for out: %v", err)
	}
	// epee's own shapes pass: no "error" key on success, an "error" and "result" null are both fine
	for _, b := range []string{`{"id":"0","jsonrpc":"2.0","result":{"height":1}}`, `{"id":"0","jsonrpc":"2.0","result":{"height":1},"error":null}`,
		"{\r\n  \"id\": \"0\",\r\n  \"jsonrpc\": \"2.0\",\r\n  \"result\": {\r\n    \"height\": 1\r\n  }\r\n}"} {
		f.reply = func(w http.ResponseWriter, _ string, _ json.RawMessage) { io.WriteString(w, b) }
		if err := c.Call(context.Background(), "get_height", nil, nil); err != nil {
			t.Errorf("body %q: %v", b, err)
		}
	}
}

func TestNewValidation(t *testing.T) {
	ok := Config{URL: "http://127.0.0.1:18083", User: "u"}
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
		"empty user":    mut(func(c *Config) { c.User = "" }),
		"long user":     mut(func(c *Config) { c.User = strings.Repeat("a", 65) }),
		"colon user":    mut(func(c *Config) { c.User = "a:b" }),
		"quote user":    mut(func(c *Config) { c.User = `a"b` }),
		"short timeout": mut(func(c *Config) { c.Timeout = 999 * time.Millisecond }),
		"long timeout":  mut(func(c *Config) { c.Timeout = 10*time.Minute + time.Nanosecond }),
		"negative":      mut(func(c *Config) { c.Timeout = -time.Second }),
	}
	for name, cfg := range bad {
		if _, err := New(cfg, []byte("p")); err == nil {
			t.Errorf("%s accepted", name)
		} else if strings.Contains(err.Error(), strings.Repeat("p", 100)) {
			t.Error("the error holds the password")
		}
	}
	for _, d := range []time.Duration{0, time.Second, 10 * time.Minute} {
		c, err := New(mut(func(c *Config) { c.Timeout = d }), []byte("p"))
		if err != nil {
			t.Errorf("timeout %v: %v", d, err)
		} else if d == 0 && c.timeout != 30*time.Second {
			t.Errorf("default timeout %v", c.timeout)
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
	if _, err := New(mut(func(c *Config) { c.User = strings.Repeat("a", 64) }), []byte("p")); err != nil {
		t.Errorf("64-byte user: %v", err)
	}
}

func TestNewCopiesPasswordAndZeroClearsIt(t *testing.T) {
	f := newFake(t)
	srv := httptest.NewServer(f)
	defer srv.Close()
	pw := []byte(tPass)
	c, err := New(Config{URL: srv.URL, User: tUser}, pw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range pw {
		pw[i] = 'x'
	}
	if err := c.Call(context.Background(), "get_height", nil, nil); err != nil {
		t.Fatalf("the client lost its password when the caller wiped theirs: %v", err)
	}
	old := c.creds.bytes()
	c.Zero()
	if len(c.creds.bytes()) != 0 || !c.zeroed || len(old) != len(tPass) {
		t.Fatal("Zero left password bytes")
	}
	for i, b := range old {
		if b != 0 {
			t.Fatalf("Zero left byte %d of the password's copy", i)
		}
	}
	before := f.requests.Load()
	if err := c.Call(context.Background(), "get_height", nil, nil); !errors.Is(err, ErrZeroed) || f.requests.Load() != before {
		t.Fatalf("a zeroed client made a request: %v", err)
	}
	c.Zero()
}

// TestRedaction: the encoders and loggers a Client can reach print no form of the password, and the
// Client prints as xmrrpc.Client. Config holds none (New takes it apart); fmt's verbs are
// TestClientEveryVerb's.
func TestRedaction(t *testing.T) {
	c, err := New(Config{URL: "http://127.0.0.1:1", User: tUser}, []byte(tPass))
	if err != nil {
		t.Fatal(err)
	}
	leaks := []string{tPass, base64.StdEncoding.EncodeToString([]byte(tPass)), hex.EncodeToString([]byte(tPass)),
		strings.ToUpper(hex.EncodeToString([]byte(tPass))), strings.Trim(fmt.Sprint([]byte(tPass)), "[]")}
	var outs []string
	for _, x := range []any{c, []*Client{c}, map[string]any{"k": c}, struct{ C *Client }{c}} {
		j, err := json.Marshal(x)
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, string(j))
	}
	b, err := c.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	outs = append(outs, string(b))
	for _, mk := range []func(io.Writer) slog.Handler{
		func(w io.Writer) slog.Handler { return slog.NewJSONHandler(w, nil) },
		func(w io.Writer) slog.Handler { return slog.NewTextHandler(w, nil) },
	} {
		var buf strings.Builder
		l := slog.New(mk(&buf))
		l.Info("start", "client", c, slog.Any("c", c), slog.Group("g", slog.Any("cl", struct{ C *Client }{c})))
		outs = append(outs, buf.String())
	}
	for _, o := range outs {
		for _, l := range leaks {
			if o == "" || strings.Contains(o, l) {
				t.Errorf("a rendering holds %q (or is empty): %.120q", l, o)
			}
		}
	}
	for _, v := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x"} {
		if s := fmt.Sprintf(v, c); s != "xmrrpc.Client" {
			t.Errorf("%s of the client prints %q", v, s)
		}
	}
}

// TestTransportErrorHoldsNoAnswerBytes: Go's HTTP transport quotes a malformed answer into its
// error ("malformed HTTP response \"...\""), so the client keeps none of that error: whatever answers at the
// URL puts none of its bytes into the client's error text. Each answer is sent to the first request, and
// again after a usable challenge (so that the authenticated path and the body read are crossed too).
func TestTransportErrorHoldsNoAnswerBytes(t *testing.T) {
	const sentinel = "SENTINEL-ANSWER-BYTES"
	const challenge401 = "HTTP/1.1 401 Unauthorized\r\nWWW-authenticate: Digest qop=\"auth\",algorithm=MD5,realm=\"monero-rpc\"," +
		"nonce=\"abc\",stale=false\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
	for _, answer := range []string{
		sentinel + "\r\n\r\n",
		"HTTP/1.1 2x0 " + sentinel + "\r\n\r\n",
		"HTTP/1.1 200 OK\r\n" + sentinel + "\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Length: " + sentinel + "\r\n\r\n",
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" + sentinel + "\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n" + sentinel,
		"HTTP/1.1 401 Unauthorized\r\nWWW-authenticate: " + sentinel + "\r\nContent-Length: 0\r\n\r\n",
	} {
		for _, challengeFirst := range []bool{false, true} {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var n atomic.Int32
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
						if challengeFirst && n.Add(1) == 1 {
							_, _ = io.WriteString(conn, challenge401)
							return
						}
						_, _ = io.WriteString(conn, answer)
					}()
				}
			}()
			c, err := New(Config{URL: "http://" + ln.Addr().String(), User: tUser, Timeout: 2 * time.Second}, []byte(tPass))
			if err != nil {
				t.Fatal(err)
			}
			err = c.Call(context.Background(), "get_height", nil, nil)
			ln.Close()
			if err == nil {
				t.Fatalf("answer %q (challenge first %v): no error", answer, challengeFirst)
			}
			if text := fmt.Sprintf("%v %+v %#v", err, err, err); strings.Contains(text, sentinel) {
				t.Errorf("answer %q (challenge first %v): the error holds the answer's bytes: %s", answer, challengeFirst, text)
			}
			if !errors.Is(err, ErrTransport) && !errors.Is(err, ErrBadAnswer) && !errors.Is(err, ErrHTTP) && !errors.Is(err, ErrAuth) {
				t.Errorf("answer %q (challenge first %v): err %v", answer, challengeFirst, err)
			}
			if challengeFirst && errors.Is(err, ErrAuth) && !strings.HasPrefix(answer, "HTTP/1.1 401") {
				t.Errorf("answer %q after a challenge: %v, want the transport's or the answer's error", answer, err)
			}
		}
	}
}

// TestNoLoginRefused: a wallet-rpc that answers a request without an Authorization
// header with a success (started with --disable-rpc-login) is ErrAuth, so "a Digest login" holds by
// construction.
func TestNoLoginRefused(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		io.WriteString(w, `{"id":"0","jsonrpc":"2.0","result":{"height":5}}`)
	}))
	defer srv.Close()
	c, err := New(Config{URL: srv.URL, User: tUser}, []byte(tPass))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := c.GetHeight(context.Background()); !errors.Is(err, ErrAuth) {
			t.Fatalf("err = %v, want ErrAuth", err)
		}
	}
	if n.Load() != 2 {
		t.Fatalf("%d requests", n.Load())
	}
}

// TestClientValueHidesDigestState: a copy of a Client printed with no method (a value, as in an
// unexported field) shows none of the nonces the server issued.
func TestClientValueHidesDigestState(t *testing.T) {
	f := newFake(t)
	c := serveFake(t, f)
	for range 3 {
		if err := c.Call(context.Background(), "get_version", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	nonces := append([]string(nil), f.nonces...)
	f.mu.Unlock()
	v := clientValue(c)
	for _, verb := range formatVerbs {
		for _, x := range []any{v, struct{ c any }{v}, []any{v}} {
			out := fmt.Sprintf(verb, x)
			for _, n := range nonces {
				if strings.Contains(out, n) {
					t.Fatalf("%s of a Client value holds the nonce %q: %.200q", verb, n, out)
				}
			}
		}
	}
}
