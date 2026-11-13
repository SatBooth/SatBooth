// Package xmrrpc is SatBooth's client for monero-wallet-rpc: JSON-RPC 2.0 over HTTP with Digest
// authentication (RFC 7616, MD5, qop=auth: what epee's http_server_auth offers), a method allowlist, typed
// calls with exact decoding, and the address and amount rules of the Monero rail. It never decodes an
// address and holds no key: the wallet is view-only and the password is the RPC login's.
package xmrrpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/satbooth/satbooth/btcrpc"
)

// MaxResponseBytes is the most the client reads from one answer.
const MaxResponseBytes = 8 << 20

const (
	maxPassword    = 128
	minTimeout     = time.Second
	maxTimeout     = 10 * time.Minute
	defaultTimeout = 30 * time.Second
	requestID      = "0"
	endpoint       = "/json_rpc"
	maxDrain       = 64 << 10 // the most read from a 401's body before the connection is reused
)

var (
	// ErrMethod means the method is outside the allowlist; nothing was sent.
	ErrMethod = errors.New("xmrrpc: method not allowed")
	// ErrArgument means an argument was refused before anything was sent.
	ErrArgument = errors.New("xmrrpc: bad argument")
	// ErrAuth means wallet-rpc answered 401 and the Digest exchange could not satisfy it: no usable
	// challenge, or a 401 on the one authenticated retry (a wrong login, or a stale nonce twice).
	ErrAuth = errors.New("xmrrpc: authentication failed")
	// ErrTransport marks a failure to get an answer: dial, timeout, a cancelled context, or (via ErrHTTP) an
	// HTTP status other than 200 and 401.
	ErrTransport = errors.New("xmrrpc: transport failure")
	// ErrHTTP means an HTTP status other than 200 (and other than 401) with no JSON-RPC error in the body. It
	// also satisfies errors.Is(err, ErrTransport), as in btcrpc.
	ErrHTTP = errors.New("xmrrpc: unexpected HTTP status")
	// ErrBadAnswer means the body was not a JSON-RPC 2.0 envelope with id "0", or a result had the wrong shape.
	ErrBadAnswer = errors.New("xmrrpc: bad answer")
	// ErrZeroed means the client's password was cleared by Zero; nothing was sent.
	ErrZeroed = errors.New("xmrrpc: client closed (password cleared)")
)

// allowedMethods is the wallet-rpc method set, in the order it is written everywhere (docs/monero.md, the threat
// model, here). The client refuses any other method before sending it; sign_transfer is sent only by
// SignTransferProbe, with an empty unsigned_txset.
var allowedMethods = []string{"get_version", "get_height", "get_address", "create_address", "store",
	"get_transfers", "get_transfer_by_txid", "sign_transfer"}

// Error is a JSON-RPC error from wallet-rpc. It keeps the code and the method only: the message text is
// dropped, so nothing that formats this error can print what wallet-rpc wrote.
type Error struct {
	Code   int
	Method string
}

func (e *Error) Error() string { return "xmrrpc: error " + strconv.Itoa(e.Code) + " from " + e.Method }

// Config is the client's configuration. It holds no secret: New takes the password beside it, so no
// rendering of a Config can print one.
type Config struct {
	URL     string // btcrpc.ValidURL; one trailing "/" is trimmed, and the endpoint is <URL>/json_rpc
	User    string // btcrpc.ValidName
	Timeout time.Duration
}

// secret holds the password and the Digest state (the kept nonce and its counter) two pointers away from
// anything fmt can be given: the Client keeps a *secret, and secret keeps a *[]byte and a *digest. fmt calls no method for %p of a non-pointer nor for an unexported struct field,
// so no method can guard the bytes; what does is that fmt prints a pointer below the top level as an address,
// and the one way it dereferences a nested pointer (a verb the field's kind does not take, such as %s on a
// pointer: "%!s(*T=&{...})") restarts with %v, under which the inner pointer prints as an address. One level
// is not enough (TestClientEveryVerb).
type secret struct {
	b *[]byte
	d *digest // read and written holding flight
}

// digest is the client's Digest state: the last usable challenge and the nc sent with it.
type digest struct {
	ch   challenge
	have bool
	nc   uint32
}

func newSecret(password []byte) *secret {
	b := append([]byte(nil), password...)
	return &secret{b: &b, d: &digest{}}
}

func (s *secret) bytes() []byte { return *s.b }

func (s *secret) clear() {
	clear(*s.b)
	*s.b = nil
}

const clientRedacted = "xmrrpc.Client"

// Client is a wallet-rpc client. It is safe for concurrent use, but it sends one request at a time. epee keeps the
// Digest state (the nonce and the request counter) per connection: http_server_auth m_auth is a member of the
// connection's handler (contrib/epee/include/net/http_protocol_handler.h:190 and :227, the per-connection m_handler of
// abstract_tcp_server2.h:271, at v0.18.5.4), and a request passes only when its nc equals that session's counter
// (contrib/epee/src/http_auth.cpp, auth_message::verify). The client keeps one Digest state (the challenge and nc)
// for all its pooled connections (MaxIdleConns 2). With one call at a time, nc follows the one server session whose
// challenge the client holds; a call that lands on another connection gets a 401 and retries once with the challenge
// of that 401. Two calls in flight could send their nc out of order and both fail. A call waits for the one in
// flight, or for its context.
type Client struct {
	base    string
	user    string
	timeout time.Duration
	http    *http.Client

	// flight is the one-slot semaphore a call holds over its whole round trip; creds.d is its state.
	flight chan struct{}

	mu     sync.Mutex // guards creds.b and zeroed
	creds  *secret    // never nil; Zero clears the password's bytes
	zeroed bool
}

// The Client prints as xmrrpc.Client under every verb, in JSON and in slog: a stable text in logs, not a
// guard (the password is behind a pointer, see secret).

func (*Client) Format(f fmt.State, _ rune)   { io.WriteString(f, clientRedacted) }
func (*Client) String() string               { return clientRedacted }
func (*Client) GoString() string             { return clientRedacted }
func (*Client) MarshalJSON() ([]byte, error) { return []byte(`"` + clientRedacted + `"`), nil }
func (*Client) MarshalText() ([]byte, error) { return []byte(clientRedacted), nil }
func (*Client) LogValue() slog.Value         { return slog.StringValue(clientRedacted) }

// New checks cfg and the password (1 to 128 bytes, copied; Zero clears the copy) and returns a client. The
// transport is btcrpc's (no proxy from the environment, no
// compression, 64 KiB of headers, 2 idle connections) with TLS 1.2 at least; it follows no redirect and reads
// at most MaxResponseBytes.
func New(cfg Config, password []byte) (*Client, error) {
	if !btcrpc.ValidURL(cfg.URL) {
		return nil, errors.New("xmrrpc: url must be an http or https URL with a host, without user info, query or fragment")
	}
	if !btcrpc.ValidName(cfg.User) {
		return nil, errors.New("xmrrpc: user must be 1 to 64 characters of A-Z a-z 0-9 . _ -")
	}
	if len(password) < 1 || len(password) > maxPassword {
		return nil, errors.New("xmrrpc: password must be 1 to 128 bytes")
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if timeout < minTimeout || timeout > maxTimeout {
		return nil, errors.New("xmrrpc: timeout must be 1 s to 10 min")
	}
	tr := &http.Transport{
		Proxy:                  nil, // never from the environment: the login goes to the configured wallet-rpc only
		DisableCompression:     true,
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConns:           2,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &Client{
		base:    strings.TrimSuffix(cfg.URL, "/"),
		user:    cfg.User,
		timeout: timeout,
		creds:   newSecret(password),
		flight:  make(chan struct{}, 1),
		http: &http.Client{
			Transport:     tr,
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Zero clears the client's copy of the password. Later calls fail with ErrZeroed without sending anything.
func (c *Client) Zero() {
	c.mu.Lock()
	c.creds.clear()
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

// Call sends method with named params to <URL>/json_rpc and decodes the result into out (nil to ignore it).
// params nil becomes {}; otherwise it must marshal to a JSON object. It refuses sign_transfer, which only
// SignTransferProbe sends, and only with an empty unsigned_txset.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	if method == "sign_transfer" {
		return fmt.Errorf("%w: %q is sent by SignTransferProbe only", ErrMethod, method)
	}
	raw, err := c.rpc(ctx, method, params)
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

func (c *Client) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
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
	}{"2.0", requestID, method, p})
	if err != nil {
		return nil, fmt.Errorf("%w: request does not marshal", ErrArgument)
	}

	// One request in flight (see Client). Waiting ends with the caller's context; the round trip itself is
	// bounded by the timeout from here.
	select {
	case c.flight <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %s waiting for the call in flight: %w", ErrTransport, method, ctx.Err())
	}
	defer func() { <-c.flight }()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	d := c.creds.d
	resp, authed, err := c.send(ctx, method, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		ch, ok := pickChallenge(resp.Header.Values("WWW-Authenticate"))
		drain(resp)
		if !ok {
			d.have = false
			return nil, fmt.Errorf("%w: %s: no usable Digest challenge", ErrAuth, method)
		}
		d.ch, d.have, d.nc = ch, true, 0
		// The one retry, whether the challenge says stale or not.
		if resp, authed, err = c.send(ctx, method, body); err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized {
			// Keep a usable new challenge for the next call (a stale nonce then costs it nothing); this call
			// is over either way.
			ch, ok := pickChallenge(resp.Header.Values("WWW-Authenticate"))
			drain(resp)
			d.ch, d.have, d.nc = ch, ok, 0
			return nil, fmt.Errorf("%w: %s: refused after one retry", ErrAuth, method)
		}
	}
	if !authed && resp.StatusCode/100 == 2 {
		// wallet-rpc answered without asking for a login (started with --disable-rpc-login): refused, so that
		// the Digest login the threat model counts on is checked at every start (Check's first call) and on
		// every call after it.
		drain(resp)
		return nil, fmt.Errorf("%w: %s: wallet-rpc asked for no login", ErrAuth, method)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, transportError(ctx, method, err)
	}
	if len(data) > MaxResponseBytes {
		return nil, bad(method + ": answer over 8 MiB")
	}
	// The body is read before the status: a JSON-RPC error is the *Error whatever the status.
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

// send makes one POST with the kept challenge's Authorization, if there is one, and says whether it sent one.
// It must be called holding flight. nc goes up when a request is sent, as epee's counter goes up when one
// arrives: a request that is lost on the way leaves the two apart, and the next call heals with one stale 401.
func (c *Client) send(ctx context.Context, method string, body []byte) (resp *http.Response, authed bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("%w: request", ErrArgument)
	}
	req.Header.Set("Content-Type", "application/json")
	d := c.creds.d
	if d.have && d.nc == math.MaxUint32 {
		d.have = false // the counter would wrap: ask for a new nonce
	}
	var cn [8]byte
	if _, err := rand.Read(cn[:]); err != nil {
		return nil, false, fmt.Errorf("%w: cnonce", ErrTransport)
	}
	c.mu.Lock()
	if c.zeroed {
		c.mu.Unlock()
		return nil, false, ErrZeroed
	}
	if d.have {
		d.nc++
		req.Header.Set("Authorization", authorize(d.ch, c.user, c.creds.bytes(), http.MethodPost, req.URL.RequestURI(), d.nc, hex.EncodeToString(cn[:])))
	}
	c.mu.Unlock()
	resp, err = c.http.Do(req)
	if err != nil {
		return nil, false, transportError(ctx, method, err)
	}
	return resp, d.have, nil
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

// drain reads a little of a body that will not be used, so that the connection can be reused, and closes it.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	resp.Body.Close()
}

// decodeEnvelope parses a JSON-RPC 2.0 answer as epee writes it: an object with "jsonrpc" "2.0", "id" "0",
// and either "result" or "error" (epee leaves the other key out). A non-null error object with an integer
// code is an *Error (its message is not read); a body that is not that shape, or whose result is missing or
// null, is ErrBadAnswer.
func decodeEnvelope(data []byte, method string) (json.RawMessage, error) {
	var env struct {
		JSONRPC json.RawMessage `json:"jsonrpc"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
		ID      json.RawMessage `json:"id"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if t := bytes.TrimSpace(data); len(t) == 0 || t[0] != '{' || dec.Decode(&env) != nil {
		return nil, bad(method + ": not a JSON-RPC envelope")
	}
	if _, err := dec.Token(); err != io.EOF { // nothing but whitespace after the object
		return nil, bad(method + ": data after the envelope")
	}
	var version, id string
	if json.Unmarshal(env.JSONRPC, &version) != nil || version != "2.0" {
		return nil, bad(method + ": not JSON-RPC 2.0")
	}
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

func bad(what string) error { return fmt.Errorf("%w: %s", ErrBadAnswer, what) }
