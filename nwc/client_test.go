package nwc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	mrand "math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/satbooth/satbooth/nip44"
	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/relay"
)

const (
	client2KeyHex = "0000000000000000000000000000000000000000000000000000000000000007"
	wallet2KeyHex = "0000000000000000000000000000000000000000000000000000000000000009"
)

var fastTune = tuning{backoffMin: 5 * time.Millisecond, backoffMax: 20 * time.Millisecond}

// eventually polls cond until it holds or 5 s pass. It synchronises on a condition, never on a fixed delay.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// --- the relay rig ------------------------------------------------------------------------------------

// trackListener records accepted connections so a test can cut them. (httptest forgets hijacked
// connections, so CloseClientConnections would not reach a WebSocket.)
type trackListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *trackListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *trackListener) closeAll() {
	l.mu.Lock()
	cs := l.conns
	l.conns = nil
	l.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
}

// gate holds new connections to the client listener at the HTTP handler until released.
type gate struct {
	mu      sync.Mutex
	on      bool
	open    chan struct{}
	arrived chan struct{}
}

func (g *gate) hold() {
	g.mu.Lock()
	g.on, g.open = true, make(chan struct{})
	g.mu.Unlock()
}

func (g *gate) release() {
	g.mu.Lock()
	if g.on {
		g.on = false
		close(g.open)
	}
	g.mu.Unlock()
}

func (g *gate) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		on, open := g.on, g.open
		g.mu.Unlock()
		if on {
			select {
			case g.arrived <- struct{}{}:
			default:
			}
			select {
			case <-open:
			case <-r.Context().Done():
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

type rig struct {
	t                 *testing.T
	hubURL, clientURL string
	cL                *trackListener
	gate              *gate
	client, wallet    *nostr.SecretKey
	client2, wallet2  *nostr.SecretKey
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{
		t: t, client: key(t, clientKeyHex), wallet: key(t, walletKeyHex),
		client2: key(t, client2KeyHex), wallet2: key(t, wallet2KeyHex),
		gate: &gate{arrived: make(chan struct{}, 16)},
	}
	hubL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.cL = &trackListener{Listener: cl}
	r.hubURL = "ws://" + hubL.Addr().String()
	r.clientURL = "ws://" + cl.Addr().String()
	cfgJSON := fmt.Sprintf(`{"hub_listen":%q,"client_listen":%q,"client_relay_url":%q,"pairs":[`+
		`{"wallet":%q,"client":%q},{"wallet":%q,"client":%q}]}`,
		hubL.Addr().String(), cl.Addr().String(), r.clientURL,
		r.wallet.PublicKeyHex(), r.client.PublicKeyHex(), r.wallet2.PublicKeyHex(), r.client2.PublicKeyHex())
	cfg, err := relay.ParseConfig([]byte(cfgJSON))
	if err != nil {
		t.Fatal(err)
	}
	srv := relay.NewServer(cfg, nil, nil)
	hubSrv := httptest.NewUnstartedServer(srv.Handler(relay.HubListener))
	hubSrv.Listener.Close()
	hubSrv.Listener = hubL
	hubSrv.Start()
	cSrv := httptest.NewUnstartedServer(r.gate.wrap(srv.Handler(relay.ClientListener)))
	cSrv.Listener.Close()
	cSrv.Listener = r.cL
	cSrv.Start()
	t.Cleanup(func() {
		r.gate.release()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		hubSrv.Close()
		cSrv.Close()
	})
	return r
}

func (r *rig) dial(tune tuning) *Client { return r.dialAs(r.client, r.wallet, tune, nil) }

func (r *rig) dialAs(k, w *nostr.SecretKey, tune tuning, log *slog.Logger) *Client {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dial(ctx, Config{RelayURL: r.clientURL, WalletPubKey: w.PublicKeyHex(), Key: k, Log: log}, tune)
	if err != nil {
		r.t.Fatalf("dial: %v", err)
	}
	r.t.Cleanup(func() { _ = c.Close() })
	return c
}

// --- the fake wallet ----------------------------------------------------------------------------------

type walletReq struct {
	ev     *nostr.Event
	Method string
	Params json.RawMessage
}

type okFrame struct {
	id  string
	ok  bool
	msg string
}

// fakeWallet plays the Alby Hub: it reads requests on the hub listener and publishes whatever the test
// tells it to.
type fakeWallet struct {
	t        *testing.T
	key      *nostr.SecretKey
	clientPK string
	conv     [32]byte
	ws       *websocket.Conn
	reqs     chan walletReq
	oks      chan okFrame
	received atomic.Int64
}

func (r *rig) startWallet() *fakeWallet { return r.startWalletFor(r.wallet, r.client) }

func (r *rig) startWalletFor(wk, ck *nostr.SecretKey) *fakeWallet {
	r.t.Helper()
	conv, err := nip44.ConversationKey(wk, ck.PublicKeyHex())
	if err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, r.hubURL, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	ws.SetReadLimit(1 << 20)
	w := &fakeWallet{t: r.t, key: wk, clientPK: ck.PublicKeyHex(), conv: conv, ws: ws,
		reqs: make(chan walletReq, 256), oks: make(chan okFrame, 256)}
	r.t.Cleanup(func() { ws.CloseNow() })
	req := fmt.Sprintf(`["REQ","w",{"kinds":[23194],"#p":[%q]}]`, wk.PublicKeyHex())
	if err := ws.Write(ctx, websocket.MessageText, []byte(req)); err != nil {
		r.t.Fatal(err)
	}
	eose := make(chan struct{})
	go w.readLoop(eose)
	select {
	case <-eose:
	case <-ctx.Done():
		r.t.Fatal("wallet: no EOSE")
	}
	return w
}

func (w *fakeWallet) readLoop(eose chan struct{}) {
	var once sync.Once
	for {
		_, data, err := w.ws.Read(context.Background())
		if err != nil {
			return
		}
		var parts []json.RawMessage
		if json.Unmarshal(data, &parts) != nil || len(parts) == 0 {
			continue
		}
		var label string
		_ = json.Unmarshal(parts[0], &label)
		switch label {
		case "EOSE":
			once.Do(func() { close(eose) })
		case "OK":
			id, ok, msg, good := parseOK(parts)
			if good {
				w.oks <- okFrame{id, ok, msg}
			}
		case "EVENT":
			ev, err := nostr.ParseEvent(parts[2])
			if err != nil {
				continue
			}
			plain, err := nip44.Decrypt(w.conv, ev.Content)
			if err != nil {
				continue
			}
			var m struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal([]byte(plain), &m) != nil {
				continue
			}
			w.received.Add(1)
			w.reqs <- walletReq{ev, m.Method, m.Params}
		}
	}
}

// next returns the next request the wallet received.
func (w *fakeWallet) next() walletReq {
	w.t.Helper()
	select {
	case r := <-w.reqs:
		return r
	case <-time.After(5 * time.Second):
		w.t.Fatal("wallet: no request within 5 s")
		return walletReq{}
	}
}

// build signs an event of kind from the wallet to the client.
func (w *fakeWallet) build(kind int, tags [][]string, plaintext string) *nostr.Event {
	w.t.Helper()
	payload, err := nip44.Encrypt(w.conv, plaintext)
	if err != nil {
		w.t.Fatal(err)
	}
	e := &nostr.Event{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: payload}
	if err := nostr.Sign(e, w.key); err != nil {
		w.t.Fatal(err)
	}
	return e
}

func (w *fakeWallet) respEvent(req walletReq, plaintext string) *nostr.Event {
	return w.build(nostr.KindNWCResponse, [][]string{{"p", w.clientPK}, {"e", req.ev.ID}}, plaintext)
}

func (w *fakeWallet) notifEvent(plaintext string) *nostr.Event {
	return w.build(nostr.KindNWCNotification, [][]string{{"p", w.clientPK}}, plaintext)
}

// publish sends e to the relay and waits for the relay's OK.
func (w *fakeWallet) publish(e *nostr.Event) okFrame {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.ws.Write(ctx, websocket.MessageText, append(append([]byte(`["EVENT",`), mustJSON(w.t, e)...), ']')); err != nil {
		w.t.Fatal(err)
	}
	for {
		select {
		case o := <-w.oks:
			if o.id == e.ID {
				return o
			}
		case <-ctx.Done():
			w.t.Fatal("wallet: no OK within 5 s")
		}
	}
}

// reply publishes an ok response of resultType with the given result JSON.
func (w *fakeWallet) reply(req walletReq, resultType, resultJSON string) *nostr.Event {
	w.t.Helper()
	e := w.respEvent(req, fmt.Sprintf(`{"result_type":%q,"result":%s}`, resultType, resultJSON))
	if o := w.publish(e); !o.ok {
		w.t.Fatalf("relay refused the wallet's response: %s", o.msg)
	}
	return e
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const infoJSON = `{"network":"regtest","methods":["pay_invoice","get_info"],"notifications":["payment_received"],"block_height":7}`

func txJSONFor(hash, typ, state string) string {
	return fmt.Sprintf(`{"type":%q,"state":%q,"invoice":"lnbcrt10n1pabc","description":"coffee","description_hash":"",`+
		`"preimage":%q,"payment_hash":%q,"amount":10000,"fees_paid":0,"created_at":1800000000,`+
		`"expires_at":1800086400,"settled_at":null,"metadata":{"order":"a1"}}`, typ, state, preimageA, hash)
}

type result struct {
	v   any
	err error
}

// async runs f on a goroutine and returns the channel of its result.
func async(f func() (any, error)) <-chan result {
	ch := make(chan result, 1)
	go func() {
		v, err := f()
		ch <- result{v, err}
	}()
	return ch
}

func await(t *testing.T, ch <-chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("call did not return within 10 s")
		return result{}
	}
}

func notDone(t *testing.T, ch <-chan result, why string) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("call returned (%v, %v): %s", r.v, r.err, why)
	default:
	}
}

// --- a scripted relay (for what the real relay will not do) ------------------------------------------------

// fakeRelay is a minimal relay: it answers the client's REQ with EOSE, its EVENTs with OK, and lets
// the test push any frame. With auth set it behaves like the private listener: AUTH challenge first, and
// nothing works before the client answers it.
type fakeRelay struct {
	t         *testing.T
	url       string
	auth      bool
	refuse    string // when set, EVENT gets OK false with this text
	in        chan string
	events    chan *nostr.Event
	connected chan struct{}
	mu        sync.Mutex
	ws        *websocket.Conn
	okAuth    atomic.Int64
}

func newFakeRelay(t *testing.T, auth bool) *fakeRelay {
	t.Helper()
	f := &fakeRelay{t: t, auth: auth, in: make(chan string, 1024), events: make(chan *nostr.Event, 256), connected: make(chan struct{}, 16)}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.url = "ws://" + l.Addr().String()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	t.Cleanup(func() {
		f.mu.Lock()
		if f.ws != nil {
			f.ws.CloseNow()
		}
		f.mu.Unlock()
		srv.Close()
	})
	return f
}

func (f *fakeRelay) sendRaw(ws *websocket.Conn, frame string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ws.Write(ctx, websocket.MessageText, []byte(frame))
}

func (f *fakeRelay) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	f.mu.Lock()
	f.ws = ws
	f.mu.Unlock()
	defer ws.CloseNow()
	var challenge string
	authed := !f.auth
	if f.auth {
		var b [16]byte
		_, _ = rand.Read(b[:])
		challenge = fmt.Sprintf("%x", b)
		f.sendRaw(ws, fmt.Sprintf(`["AUTH",%q]`, challenge))
	}
	f.connected <- struct{}{}
	for {
		_, data, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		f.in <- string(data)
		var parts []json.RawMessage
		if json.Unmarshal(data, &parts) != nil || len(parts) == 0 {
			continue
		}
		var label string
		_ = json.Unmarshal(parts[0], &label)
		switch label {
		case "REQ":
			if !authed {
				f.sendRaw(ws, `["CLOSED","nwc","auth-required: authenticate first"]`)
			} else {
				f.sendRaw(ws, `["EOSE","nwc"]`)
			}
		case "AUTH":
			ev, err := nostr.ParseEvent(parts[1])
			if err != nil {
				return
			}
			cerr := nostr.CheckAuth(ev, f.url, challenge, time.Now(), func(string) bool { return true })
			f.sendRaw(ws, fmt.Sprintf(`["OK",%q,%t,""]`, ev.ID, cerr == nil))
			if cerr == nil {
				authed = true
				f.okAuth.Add(1)
			}
		case "EVENT":
			ev, err := nostr.ParseEvent(parts[1])
			if err != nil {
				return
			}
			switch {
			case !authed:
				f.sendRaw(ws, fmt.Sprintf(`["OK",%q,false,"auth-required: authenticate first"]`, ev.ID))
			case f.refuse != "":
				f.sendRaw(ws, fmt.Sprintf(`["OK",%q,false,%q]`, ev.ID, f.refuse))
			default:
				f.events <- ev
				f.sendRaw(ws, fmt.Sprintf(`["OK",%q,true,""]`, ev.ID))
			}
		}
	}
}

func (f *fakeRelay) push(frame string) {
	f.t.Helper()
	f.mu.Lock()
	ws := f.ws
	f.mu.Unlock()
	f.sendRaw(ws, frame)
}

func (f *fakeRelay) pushEvent(e *nostr.Event) {
	f.t.Helper()
	f.push(`["EVENT","nwc",` + string(mustJSON(f.t, e)) + `]`)
}

func (f *fakeRelay) client(tune tuning, log *slog.Logger) (*Client, env) {
	f.t.Helper()
	v := newEnv(f.t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dial(ctx, Config{RelayURL: f.url, WalletPubKey: v.wallet.PublicKeyHex(), Key: v.client, Log: log}, tune)
	if err != nil {
		f.t.Fatalf("dial: %v", err)
	}
	f.t.Cleanup(func() { _ = c.Close() })
	return c, v
}

// --- tests ----------------------------------------------------------------------------------------------

func TestClientHappyPath(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)
	ctx := context.Background()
	hash2 := strings.Repeat("ab", 32)

	// The wallet answers on its own goroutine, per method. The cleanup stops it and waits for it while the
	// relay and the wallet's socket are still open (cleanups run last-registered first), so a reply still
	// waiting for its OK gets it, and nothing reports to the test after it has completed.
	stop, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		for {
			var req walletReq
			select {
			case req = <-w.reqs:
			case <-stop:
				return
			}
			switch req.Method {
			case "make_invoice":
				w.reply(req, req.Method, txJSONFor(hashA, "incoming", "pending"))
			case "make_hold_invoice":
				var p struct {
					PaymentHash string `json:"payment_hash"`
				}
				_ = json.Unmarshal(req.Params, &p)
				w.reply(req, req.Method, txJSONFor(p.PaymentHash, "incoming", "pending"))
			case "settle_hold_invoice", "cancel_hold_invoice":
				w.reply(req, req.Method, `{}`)
			case "lookup_invoice":
				w.reply(req, req.Method, txJSONFor(hashA, "outgoing", "settled"))
			case "list_transactions":
				w.reply(req, req.Method, `{"transactions":[`+txJSONFor(hashA, "incoming", "settled")+`],"total_count":41}`)
			case "pay_invoice":
				w.reply(req, req.Method, `{"preimage":"`+preimageA+`","fees_paid":1000}`)
			case "get_budget":
				w.reply(req, req.Method, `{"used_budget":5000,"total_budget":100000,"renewal_period":"daily"}`)
			case "get_info":
				w.reply(req, req.Method, infoJSON)
			}
		}
	}()

	tx, err := c.MakeInvoice(ctx, MakeInvoiceParams{AmountMsat: 10000, Description: "coffee", ExpirySeconds: 600})
	if err != nil || tx.PaymentHash != hashA || tx.State != StatePending {
		t.Fatalf("MakeInvoice: %+v, %v", tx, err)
	}
	tx, err = c.MakeHoldInvoice(ctx, MakeHoldInvoiceParams{AmountMsat: 10000, PaymentHash: hash2, ExpirySeconds: 600})
	if err != nil || tx.PaymentHash != hash2 {
		t.Fatalf("MakeHoldInvoice: %+v, %v", tx, err)
	}
	if err := c.SettleHoldInvoice(ctx, preimageA); err != nil {
		t.Fatalf("SettleHoldInvoice: %v", err)
	}
	if err := c.CancelHoldInvoice(ctx, hash2); err != nil {
		t.Fatalf("CancelHoldInvoice: %v", err)
	}
	tx, err = c.LookupInvoice(ctx, hashA)
	if err != nil || tx.State != StateSettled || tx.Type != TypeOutgoing {
		t.Fatalf("LookupInvoice: %+v, %v", tx, err)
	}
	txs, total, err := c.ListTransactions(ctx, ListParams{Limit: 20, Type: TypeIncoming})
	if err != nil || len(txs) != 1 || total != 41 {
		t.Fatalf("ListTransactions: %+v, %d, %v", txs, total, err)
	}
	pr, err := c.PayInvoice(ctx, "lnbcrt10n1pabc", 0)
	if err != nil || pr.Preimage != preimageA || pr.FeesPaidMsat != 1000 {
		t.Fatalf("PayInvoice: %+v, %v", pr, err)
	}
	b, err := c.GetBudget(ctx)
	if err != nil || !b.Set || b.TotalMsat != 100000 {
		t.Fatalf("GetBudget: %+v, %v", b, err)
	}
	in, err := c.GetInfo(ctx)
	if err != nil || in.Network != "regtest" || in.BlockHeight != 7 {
		t.Fatalf("GetInfo: %+v, %v", in, err)
	}
}

func TestClientWalletError(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)
	res := async(func() (any, error) { return c.PayInvoice(context.Background(), "lnbcrt10n1pabc", 0) })
	req := w.next()
	if o := w.publish(w.respEvent(req, `{"result_type":"pay_invoice","error":{"code":"QUOTA_EXCEEDED","message":"over budget"}}`)); !o.ok {
		t.Fatal("relay refused")
	}
	var we *WalletError
	if got := await(t, res); !errors.As(got.err, &we) || we.Code != "QUOTA_EXCEEDED" {
		t.Fatalf("want a QUOTA_EXCEEDED wallet error, got %v", got.err)
	}
}

// A response that is almost right ends the call with ErrMalformed.
func TestClientMalformedResponse(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)
	res := async(func() (any, error) { return c.GetInfo(context.Background()) })
	req := w.next()
	w.reply(req, "get_budget", `{}`) // result_type names another method
	if got := await(t, res); !errors.Is(got.err, ErrMalformed) {
		t.Fatalf("want ErrMalformed, got %v", got.err)
	}
}

func TestClientRequestValidation(t *testing.T) {
	r := newRig(t)
	c := r.dial(fastTune)
	ctx := context.Background()
	bad := []func() error{
		func() error { _, _, err := c.ListTransactions(ctx, ListParams{Limit: 0}); return err },
		func() error { _, _, err := c.ListTransactions(ctx, ListParams{Limit: 21}); return err },
		func() error { _, _, err := c.ListTransactions(ctx, ListParams{Limit: 5, Type: "sideways"}); return err },
		func() error { _, err := c.MakeInvoice(ctx, MakeInvoiceParams{}); return err },
		func() error { _, err := c.LookupInvoice(ctx, strings.ToUpper(hashA)); return err },
		func() error { return c.SettleHoldInvoice(ctx, "abc") },
		func() error { _, err := c.PayInvoice(ctx, "", 0); return err },
		func() error { _, err := c.PayInvoice(ctx, "lnbc1", -1); return err },
	}
	for i, f := range bad {
		if err := f(); err == nil {
			t.Errorf("case %d: want an error", i)
		}
	}
}

func TestClientConfig(t *testing.T) {
	k := key(t, clientKeyHex)
	wp := key(t, walletKeyHex).PublicKeyHex()
	ctx := context.Background()
	for name, cfg := range map[string]Config{
		"no key":       {RelayURL: "ws://127.0.0.1:1", WalletPubKey: wp},
		"bad wallet":   {RelayURL: "ws://127.0.0.1:1", WalletPubKey: "zz", Key: k},
		"http scheme":  {RelayURL: "http://127.0.0.1:1", WalletPubKey: wp, Key: k},
		"credentials":  {RelayURL: "ws://u:p@127.0.0.1:1", WalletPubKey: wp, Key: k},
		"timeout low":  {RelayURL: "ws://127.0.0.1:1", WalletPubKey: wp, Key: k, Timeout: 9 * time.Second},
		"timeout high": {RelayURL: "ws://127.0.0.1:1", WalletPubKey: wp, Key: k, Timeout: 11 * time.Minute},
	} {
		if _, err := Dial(ctx, cfg); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	// The bounds themselves are accepted by validation: the dial then fails to connect.
	for _, to := range []time.Duration{10 * time.Second, 10 * time.Minute} {
		_, err := dial(ctx, Config{RelayURL: "ws://127.0.0.1:1", WalletPubKey: wp, Key: k, Timeout: to}, tuning{handshake: time.Second})
		if err == nil || strings.Contains(err.Error(), "timeout must be") {
			t.Errorf("timeout %v: want a connect error, got %v", to, err)
		}
	}
}

func TestClientREQShape(t *testing.T) {
	f := newFakeRelay(t, false)
	_, v := f.client(fastTune, nil)
	want := fmt.Sprintf(`["REQ","nwc",{"kinds":[23195,23197],"#p":[%q],"authors":[%q]}]`, v.client.PublicKeyHex(), v.wallet.PublicKeyHex())
	if got := <-f.in; got != want {
		t.Fatalf("REQ = %s\nwant %s", got, want)
	}
}

func TestAuth(t *testing.T) {
	t.Run("answers the challenge before it publishes", func(t *testing.T) {
		f := newFakeRelay(t, true)
		c, v := f.client(fastTune, nil)
		if f.okAuth.Load() != 1 {
			t.Fatal("the relay did not accept an AUTH before Dial returned")
		}
		res := async(func() (any, error) { return c.GetInfo(context.Background()) })
		ev := <-f.events // only reached if the relay saw the AUTH first
		w := &fakeWallet{t: t, key: v.wallet, clientPK: v.client.PublicKeyHex(), conv: v.conv}
		f.pushEvent(w.respEvent(walletReq{ev: ev}, `{"result_type":"get_info","result":`+infoJSON+`}`))
		if got := await(t, res); got.err != nil {
			t.Fatalf("GetInfo: %v", got.err)
		}
		// Frame order the relay saw: REQ, AUTH, REQ again, then the EVENT.
		var labels []string
		for len(f.in) > 0 {
			var parts []json.RawMessage
			_ = json.Unmarshal([]byte(<-f.in), &parts)
			var l string
			_ = json.Unmarshal(parts[0], &l)
			labels = append(labels, l)
		}
		if got := strings.Join(labels, ","); got != "REQ,AUTH,REQ,EVENT" {
			t.Fatalf("frames = %s", got)
		}
	})
	t.Run("a relay that sends no AUTH works", func(t *testing.T) {
		f := newFakeRelay(t, false)
		c, v := f.client(fastTune, nil)
		res := async(func() (any, error) { return c.GetInfo(context.Background()) })
		ev := <-f.events
		w := &fakeWallet{t: t, key: v.wallet, clientPK: v.client.PublicKeyHex(), conv: v.conv}
		f.pushEvent(w.respEvent(walletReq{ev: ev}, `{"result_type":"get_info","result":`+infoJSON+`}`))
		if got := await(t, res); got.err != nil {
			t.Fatalf("GetInfo: %v", got.err)
		}
		if f.okAuth.Load() != 0 {
			t.Fatal("an AUTH was sent to a relay that did not ask")
		}
	})
	t.Run("an unknown key is refused at Dial", func(t *testing.T) {
		r := newRig(t)
		stranger := key(t, otherKeyHex)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := dial(ctx, Config{RelayURL: r.clientURL, WalletPubKey: r.wallet.PublicKeyHex(), Key: stranger}, fastTune)
		var rr *RelayRefusedError
		if !errors.As(err, &rr) {
			t.Fatalf("want *RelayRefusedError, got %v", err)
		}
	})
}

// A response with no answer after Timeout ends in ErrNoResponse; the late answer is dropped and does not
// reach the next call.
func TestTimeout(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	tune := fastTune
	tune.timeout = 150 * time.Millisecond
	c := r.dial(tune)

	start := time.Now()
	_, err := c.GetInfo(context.Background())
	if !errors.Is(err, ErrNoResponse) {
		t.Fatalf("want ErrNoResponse, got %v", err)
	}
	if el := time.Since(start); el < 150*time.Millisecond {
		t.Fatalf("returned after %v, before the timeout", el)
	}
	late := w.next()

	// Dangerous form: a second call is already pending (same method) when the late answer arrives.
	res := async(func() (any, error) { return c.GetInfo(context.Background()) })
	req := w.next()
	before := c.dropped.Load()
	w.reply(late, "get_info", `{"network":"stale","methods":["x"],"notifications":[],"block_height":1}`)
	eventually(t, "the late response to be dropped", func() bool { return c.dropped.Load() == before+1 })
	notDone(t, res, "the late response reached the pending call")
	w.reply(req, "get_info", infoJSON)
	got := await(t, res)
	if got.err != nil || got.v.(*Info).Network != "regtest" {
		t.Fatalf("second call: %+v, %v", got.v, got.err)
	}
}

func TestTimeoutContext(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)
	ctx, cancel := context.WithCancel(context.Background())
	res := async(func() (any, error) { return c.GetInfo(ctx) })
	w.next()
	cancel()
	got := await(t, res)
	if !errors.Is(got.err, ErrNoResponse) || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("want ErrNoResponse wrapping context.Canceled, got %v", got.err)
	}
}

func TestDuplicateResponse(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)

	res := async(func() (any, error) { return c.GetInfo(context.Background()) })
	req := w.next()
	w.reply(req, "get_info", infoJSON)
	if got := await(t, res); got.err != nil || got.v.(*Info).Network != "regtest" {
		t.Fatalf("first call: %+v, %v", got.v, got.err)
	}

	// Dangerous form: the next call (same method) is pending when the second copy arrives.
	res = async(func() (any, error) { return c.GetInfo(context.Background()) })
	req2 := w.next()
	before := c.dropped.Load()
	w.reply(req, "get_info", `{"network":"copy","methods":["x"],"notifications":[],"block_height":1}`) // a second, different copy
	eventually(t, "the second copy to be dropped", func() bool { return c.dropped.Load() == before+1 })
	notDone(t, res, "the duplicate reached the next call")
	w.reply(req2, "get_info", infoJSON)
	if got := await(t, res); got.err != nil || got.v.(*Info).Network != "regtest" {
		t.Fatalf("next call: %+v, %v", got.v, got.err)
	}
}

func TestForeignEvents(t *testing.T) {
	t.Run("hostile events change nothing", func(t *testing.T) {
		f := newFakeRelay(t, false)
		var logs syncBuf
		c, v := f.client(fastTune, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		res := async(func() (any, error) { return c.GetInfo(context.Background()) })
		req := <-f.events
		evil := `{"result_type":"get_info","result":{"network":"evil","methods":["x"],"notifications":[],"block_height":1}}`
		clientPK, walletPK := v.client.PublicKeyHex(), v.wallet.PublicKeyHex()
		convOther, err := nip44.ConversationKey(v.other, clientPK)
		if err != nil {
			t.Fatal(err)
		}
		tagsFor := func(p string) [][]string { return [][]string{{"p", p}, {"e", req.ID}} }

		// 1. signed by another key (a valid event in every other respect)
		byOther := signed(t, v.other, nostr.KindNWCResponse, tagsFor(clientPK), convOther, evil)
		// 2. from the wallet but addressed to another key
		toOther := signed(t, v.wallet, nostr.KindNWCResponse, tagsFor(v.other.PublicKeyHex()), v.conv, evil)
		// 3. a bad signature
		badSig := signed(t, v.wallet, nostr.KindNWCResponse, tagsFor(clientPK), v.conv, evil)
		sig := []byte(badSig.Sig)
		if sig[0] == '0' {
			sig[0] = '1'
		} else {
			sig[0] = '0'
		}
		badSig.Sig = string(sig)
		// 4. a body tampered after signing (the id no longer matches)
		tampered := signed(t, v.wallet, nostr.KindNWCResponse, tagsFor(clientPK), v.conv, evil)
		tampered.CreatedAt++
		// 5. NIP-04 notification kind 23196
		nip04 := signed(t, v.wallet, nostr.KindNWCNotificationNIP04, [][]string{{"p", clientPK}}, v.conv, evil)
		// 6. the wallet's response to a request this client never made
		unknownReq := signed(t, v.wallet, nostr.KindNWCResponse, [][]string{{"p", clientPK}, {"e", hashA}}, v.conv, evil)
		// 7. an undecryptable body from the wallet, for our request
		garbage := signed(t, v.wallet, nostr.KindNWCResponse, tagsFor(clientPK), convOther, evil)
		// 8. a notification signed by another key
		notifOther := signed(t, v.other, nostr.KindNWCNotification, [][]string{{"p", clientPK}}, convOther,
			`{"notification_type":"payment_received","notification":`+hubTx("incoming", "settled")+`}`)
		// 9. a notification from the wallet for another key
		notifToOther := signed(t, v.wallet, nostr.KindNWCNotification, [][]string{{"p", v.other.PublicKeyHex()}}, v.conv,
			`{"notification_type":"payment_received","notification":`+hubTx("incoming", "settled")+`}`)
		hostile := []*nostr.Event{byOther, toOther, badSig, tampered, nip04, unknownReq, garbage, notifOther, notifToOther}
		for _, e := range hostile {
			f.pushEvent(e)
		}
		// 10. a valid response under another subscription id
		valid := signed(t, v.wallet, nostr.KindNWCResponse, tagsFor(clientPK), v.conv, evil)
		f.push(`["EVENT","other",` + string(mustJSON(t, valid)) + `]`)
		// 11. a frame that is not an event
		f.push(`["EVENT","nwc",{"id":1}]`)

		// Frames arrive in order, so once the real response is handled every hostile frame was seen.
		real := signed(t, v.wallet, nostr.KindNWCResponse, tagsFor(clientPK), v.conv,
			`{"result_type":"get_info","result":`+infoJSON+`}`)
		notDone(t, res, "a hostile event completed the call")
		f.pushEvent(real)
		got := await(t, res)
		if got.err != nil || got.v.(*Info).Network != "regtest" {
			t.Fatalf("call = %+v, %v", got.v, got.err)
		}
		if n := c.dropped.Load(); n != int64(len(hostile)+2) {
			t.Fatalf("dropped %d events, want %d", n, len(hostile)+2)
		}
		select {
		case n := <-c.Notifications():
			t.Fatalf("a hostile notification was delivered: %+v", n)
		default:
		}
		for _, secret := range []string{"evil", clientPK, walletPK} {
			if strings.Contains(logs.String(), secret) {
				t.Fatalf("the log mentions %q", secret)
			}
		}
	})

	t.Run("a second pair on the same relay", func(t *testing.T) {
		r := newRig(t)
		w1 := r.startWallet()
		w2 := r.startWalletFor(r.wallet2, r.client2)
		c1 := r.dial(fastTune)
		c2 := r.dialAs(r.client2, r.wallet2, fastTune, nil)

		res1 := async(func() (any, error) { return c1.GetInfo(context.Background()) })
		req1 := w1.next()
		res2 := async(func() (any, error) { return c2.GetBudget(context.Background()) })
		req2 := w2.next()
		w2.reply(req2, "get_budget", `{"used_budget":1,"total_budget":2}`)
		if got := await(t, res2); got.err != nil {
			t.Fatalf("pair 2: %v", got.err)
		}
		// Pair 2's traffic never reached client 1: nothing was dropped there and its call still waits.
		w1.reply(req1, "get_info", infoJSON)
		if got := await(t, res1); got.err != nil {
			t.Fatalf("pair 1: %v", got.err)
		}
		if n := c1.dropped.Load(); n != 0 {
			t.Fatalf("client 1 saw %d foreign events", n)
		}
		if n := c2.dropped.Load(); n != 0 {
			t.Fatalf("client 2 saw %d foreign events", n)
		}
	})
}

// The relay drops the connection after the request is stored; the wallet answers while the client is
// away; the call completes from the window's backlog and the wallet saw the request once.
func TestReconnectWhilePending(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)
	if c.ready.Load() != 1 {
		t.Fatalf("ready = %d", c.ready.Load())
	}

	res := async(func() (any, error) { return c.GetInfo(context.Background()) })
	req := w.next() // the relay stored the request and the wallet has it

	r.gate.hold()   // new connections wait in the HTTP handler
	r.cL.closeAll() // cut the client's connection
	r.gate.wait(t)  // the client noticed and is dialling again; it is disconnected until release
	notDone(t, res, "the call ended with the connection")

	w.reply(req, "get_info", infoJSON) // answered while the client is disconnected
	r.gate.release()

	got := await(t, res)
	if got.err != nil || got.v.(*Info).Network != "regtest" {
		t.Fatalf("call = %+v, %v", got.v, got.err)
	}
	noResend(t, c, w, 1)
}

// A request published, then a disconnect before the response, with the wallet answering only after the
// client is back: the live path, not the backlog.
func TestReconnectThenLiveResponse(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)
	res := async(func() (any, error) { return c.GetInfo(context.Background()) })
	req := w.next()
	r.cL.closeAll()
	eventually(t, "the reconnect", func() bool { return c.ready.Load() >= 2 })
	w.reply(req, "get_info", infoJSON)
	if got := await(t, res); got.err != nil {
		t.Fatalf("call: %v", got.err)
	}
	noResend(t, c, w, 1)
}

// A call made while the client is disconnected waits for the connection and is then sent once.
func TestCallWhileDisconnected(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)
	r.gate.hold()
	r.cL.closeAll()
	r.gate.wait(t)
	res := async(func() (any, error) { return c.GetInfo(context.Background()) })
	notDone(t, res, "nothing can have answered")
	if n := w.received.Load(); n != 0 {
		t.Fatalf("a request was published with no connection: %d", n)
	}
	r.gate.release()
	req := w.next()
	w.reply(req, "get_info", infoJSON)
	if got := await(t, res); got.err != nil {
		t.Fatalf("call: %v", got.err)
	}
	noResend(t, c, w, 1)
}

func TestRelayRefused(t *testing.T) {
	f := newFakeRelay(t, false)
	f.refuse = "blocked: not today"
	c, _ := f.client(fastTune, nil)
	start := time.Now()
	_, err := c.GetBudget(context.Background())
	var rr *RelayRefusedError
	if !errors.As(err, &rr) || rr.Reason != "blocked: not today" {
		t.Fatalf("want *RelayRefusedError, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the refusal was not immediate")
	}
}

func notifPlain(hash string) string {
	return `{"notification_type":"payment_received","notification":` + txJSONFor(hash, "incoming", "settled") + `}`
}

func TestNotifications(t *testing.T) {
	t.Run("delivered once across a reconnect", func(t *testing.T) {
		r := newRig(t)
		w := r.startWallet()
		c := r.dial(fastTune)
		n1 := w.notifEvent(notifPlain(hashA))
		if o := w.publish(n1); !o.ok {
			t.Fatal(o.msg)
		}
		select {
		case n := <-c.Notifications():
			if n.EventID != n1.ID || n.Type != NotifPaymentReceived || n.Tx.PaymentHash != hashA {
				t.Fatalf("notification = %+v", n)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no notification")
		}
		r.cL.closeAll()
		eventually(t, "the reconnect", func() bool { return c.ready.Load() >= 2 })
		// The backlog replayed n1 before the EOSE that made the session ready: it was dropped.
		if len(c.Notifications()) != 0 {
			t.Fatal("a replayed notification was delivered twice")
		}
		n2 := w.notifEvent(notifPlain(strings.Repeat("cd", 32)))
		if o := w.publish(n2); !o.ok {
			t.Fatal(o.msg)
		}
		select {
		case n := <-c.Notifications():
			if n.EventID != n2.ID {
				t.Fatalf("got %s, want the second notification", n.EventID)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no second notification")
		}
		if len(c.Notifications()) != 0 {
			t.Fatal("an extra notification")
		}
	})

	t.Run("a full channel drops the newest and logs a fixed line", func(t *testing.T) {
		f := newFakeRelay(t, false)
		var logs syncBuf
		c, v := f.client(fastTune, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		w := &fakeWallet{t: t, key: v.wallet, clientPK: v.client.PublicKeyHex(), conv: v.conv}
		var ids []string
		for i := 0; i < notifQueue+1; i++ {
			e := w.notifEvent(notifPlain(fmt.Sprintf("%064x", i+1)))
			ids = append(ids, e.ID)
			f.pushEvent(e)
		}
		eventually(t, "the overflow to be logged", func() bool {
			return strings.Contains(logs.String(), "notification dropped: channel full")
		})
		if len(c.Notifications()) != notifQueue {
			t.Fatalf("channel holds %d, want %d", len(c.Notifications()), notifQueue)
		}
		for i := 0; i < notifQueue; i++ {
			if n := <-c.Notifications(); n.EventID != ids[i] {
				t.Fatalf("notification %d out of order or missing", i)
			}
		}
		if len(c.Notifications()) != 0 {
			t.Fatal("the newest notification was kept, not dropped")
		}
		if strings.Contains(logs.String(), ids[notifQueue]) || strings.Contains(logs.String(), "coffee") {
			t.Fatal("the log carries event content or an id")
		}
	})

	t.Run("the dedup memory is bounded", func(t *testing.T) {
		f := newFakeRelay(t, false)
		tune := fastTune
		tune.seenMax = 4
		c, v := f.client(tune, nil)
		w := &fakeWallet{t: t, key: v.wallet, clientPK: v.client.PublicKeyHex(), conv: v.conv}
		evs := make([]*nostr.Event, 6)
		for i := range evs {
			evs[i] = w.notifEvent(notifPlain(fmt.Sprintf("%064x", i+1)))
		}
		deliver := func(e *nostr.Event) bool {
			before := len(c.notif)
			c.onNotification(e)
			if len(c.notif) == before {
				return false
			}
			<-c.notif
			return true
		}
		for _, e := range evs {
			if !deliver(e) {
				t.Fatal("a fresh notification was not delivered")
			}
		}
		if deliver(evs[5]) || deliver(evs[2]) { // the last four are remembered
			t.Fatal("a remembered notification was delivered again")
		}
		if !deliver(evs[0]) { // the oldest was forgotten
			t.Fatal("the forgotten notification was refused")
		}
	})
}

func TestConcurrentCalls(t *testing.T) {
	const n = 20
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)

	hashes := make([]string, n)
	results := make([]<-chan result, n)
	for i := range hashes {
		hashes[i] = fmt.Sprintf("%064x", i+1)
		h := hashes[i]
		results[i] = async(func() (any, error) { return c.LookupInvoice(context.Background(), h) })
	}
	reqs := make([]walletReq, n)
	for i := range reqs {
		reqs[i] = w.next()
	}
	rng := mrand.New(mrand.NewSource(7)) // seeded: the order is random but reproducible
	rng.Shuffle(n, func(i, j int) { reqs[i], reqs[j] = reqs[j], reqs[i] })
	for _, req := range reqs {
		var p struct {
			PaymentHash string `json:"payment_hash"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			t.Fatal(err)
		}
		w.reply(req, "lookup_invoice", txJSONFor(p.PaymentHash, "incoming", "settled"))
	}
	for i, ch := range results {
		got := await(t, ch)
		if got.err != nil || got.v.(*Transaction).PaymentHash != hashes[i] {
			t.Fatalf("call %d got %+v, %v", i, got.v, got.err)
		}
	}
}

func TestClose(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)
	res := async(func() (any, error) { return c.GetInfo(context.Background()) })
	w.next() // the call is pending
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := await(t, res); !errors.Is(got.err, ErrClosed) {
		t.Fatalf("pending call: %v", got.err)
	}
	if _, err := c.GetBudget(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("later call: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, ok := <-c.Notifications(); ok {
		t.Fatal("the notification channel is still open")
	}
}

func TestCloseWhileDisconnected(t *testing.T) {
	r := newRig(t)
	c := r.dial(fastTune)
	r.gate.hold()
	r.cL.closeAll()
	r.gate.wait(t)
	res := async(func() (any, error) { return c.GetInfo(context.Background()) })
	notDone(t, res, "not connected")
	if err := c.Close(); err != nil { // must not wait for the blocked dial
		t.Fatal(err)
	}
	if got := await(t, res); !errors.Is(got.err, ErrClosed) {
		t.Fatalf("pending call: %v", got.err)
	}
}

func TestExpirationTag(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{RelayURL: r.clientURL, WalletPubKey: r.wallet.PublicKeyHex(), Key: r.client, Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res := async(func() (any, error) { return c.GetInfo(context.Background()) })
	req := w.next()
	var exp string
	for _, tg := range req.ev.Tags {
		if tg[0] == "expiration" {
			exp = tg[1]
		}
	}
	if want := fmt.Sprint(req.ev.CreatedAt + 15); exp != want {
		t.Fatalf("expiration = %q, want %q", exp, want)
	}
	w.reply(req, "get_info", infoJSON)
	if got := await(t, res); got.err != nil {
		t.Fatal(got.err)
	}
}

// PayInvoiceUntil's request expires exactly at the time given, whatever the client's timeout; one that would
// already have expired when built is refused before anything is published (ErrExpiredBeforeSend); nwc sets no
// upper bound of its own.
func TestPayInvoiceUntil(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{RelayURL: r.clientURL, WalletPubKey: r.wallet.PublicKeyHex(), Key: r.client,
		Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	expTag := func(ev *nostr.Event) string {
		for _, tg := range ev.Tags {
			if tg[0] == "expiration" {
				return tg[1]
			}
		}
		return ""
	}
	now := time.Now().Truncate(time.Second)
	for _, until := range []time.Time{now.Add(7 * time.Minute), now.Add(2 * time.Hour)} {
		res := async(func() (any, error) { return c.PayInvoiceUntil(context.Background(), "lnbcrt10n1pabc", 0, until) })
		req := w.next()
		if got, want := expTag(req.ev), fmt.Sprint(until.Unix()); got != want {
			t.Fatalf("expiration = %q, want %q", got, want)
		}
		w.reply(req, "pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":0}`)
		if got := await(t, res); got.err != nil {
			t.Fatal(got.err)
		}
	}
	before := w.received.Load()
	for _, until := range []time.Time{time.Now().Truncate(time.Second), time.Now().Add(-time.Second), {}} {
		_, err := c.PayInvoiceUntil(context.Background(), "lnbcrt10n1pabc", 0, until)
		if !errors.Is(err, ErrExpiredBeforeSend) {
			t.Fatalf("expires %v: want ErrExpiredBeforeSend, got %v", until, err)
		}
	}
	// A sentinel call: a refused request, had it been published, would have reached the wallet before it.
	res := async(func() (any, error) { return c.GetInfo(context.Background()) })
	req := w.next()
	if req.Method != "get_info" {
		t.Fatalf("the wallet received %q", req.Method)
	}
	w.reply(req, "get_info", infoJSON)
	if got := await(t, res); got.err != nil {
		t.Fatal(got.err)
	}
	if n := w.received.Load(); n != before+1 {
		t.Fatalf("the wallet received %d requests, want %d (only the GetInfo)", n-before, 1)
	}
}

// Randomised order of the first and the second copy: whichever the relay delivers first completes the
// call, and the other is dropped, over many seeded rounds.
func TestDuplicateRounds(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)
	rng := mrand.New(mrand.NewSource(11))
	for i := 0; i < 10; i++ {
		before := c.dropped.Load()
		res := async(func() (any, error) { return c.GetBudget(context.Background()) })
		req := w.next()
		copies := rng.Intn(3) + 1
		for j := 0; j < copies; j++ {
			n, _ := rand.Int(rand.Reader, big.NewInt(1000))
			w.reply(req, "get_budget", fmt.Sprintf(`{"used_budget":%d,"total_budget":%d}`, j, 1000+n.Int64()))
		}
		if got := await(t, res); got.err != nil {
			t.Fatal(got.err)
		}
		eventually(t, "the extra copies to be dropped", func() bool { return c.dropped.Load() == before+int64(copies-1) })
	}
}

// wait blocks until a connection attempt reaches the held handler.
func (g *gate) wait(t *testing.T) {
	t.Helper()
	select {
	case <-g.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the client did not try to reconnect")
	}
}

// noResend proves the client wrote the original request only once. A resend would carry a fresh id and
// reach the wallet before anything sent later on the same connection, so one more call, answered by the
// wallet, is a sentinel: the wallet must have seen exactly the original and the sentinel.
func noResend(t *testing.T, c *Client, w *fakeWallet, original int64) {
	t.Helper()
	eventually(t, "the new session", func() bool { return c.ready.Load() >= 2 })
	res := async(func() (any, error) { return c.GetBudget(context.Background()) })
	for {
		req := w.next()
		if req.Method != "get_budget" {
			t.Fatalf("the wallet got a resent %s", req.Method)
		}
		w.reply(req, "get_budget", `{"used_budget":1,"total_budget":2}`)
		break
	}
	if got := await(t, res); got.err != nil {
		t.Fatalf("sentinel call: %v", got.err)
	}
	if n := w.received.Load(); n != original+1 {
		t.Fatalf("the wallet received %d requests, want %d (the original once, plus the sentinel)", n, original+1)
	}
}

// LookupInvoiceByString sends exactly the key "invoice" and returns the row.
func TestLookupInvoiceByString(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)
	res := async(func() (any, error) { return c.LookupInvoiceByString(context.Background(), "lnbcrt10n1pabc") })
	req := w.next()
	if req.Method != "lookup_invoice" {
		t.Fatalf("method %q", req.Method)
	}
	var p map[string]json.RawMessage
	if err := json.Unmarshal(req.Params, &p); err != nil || len(p) != 1 || string(p["invoice"]) != `"lnbcrt10n1pabc"` {
		t.Fatalf("params %s, %v", req.Params, err)
	}
	w.reply(req, "lookup_invoice", txJSONFor(hashA, "outgoing", "settled"))
	got := await(t, res)
	tx, _ := got.v.(*Transaction)
	if got.err != nil || tx == nil || tx.State != StateSettled || tx.Type != TypeOutgoing {
		t.Fatalf("got %+v, %v", got.v, got.err)
	}
}

func TestLookupInvoiceByStringWalletError(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)
	res := async(func() (any, error) { return c.LookupInvoiceByString(context.Background(), "lnbcrt10n1pabc") })
	req := w.next()
	if o := w.publish(w.respEvent(req, `{"result_type":"lookup_invoice","error":{"code":"NOT_FOUND","message":"nope"}}`)); !o.ok {
		t.Fatal("relay refused")
	}
	var we *WalletError
	if got := await(t, res); !errors.As(got.err, &we) || we.Code != "NOT_FOUND" {
		t.Fatalf("want a NOT_FOUND wallet error, got %v", got.err)
	}
}

func TestLookupInvoiceByStringNoResponse(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	tune := fastTune
	tune.timeout = 150 * time.Millisecond
	c := r.dial(tune)
	if _, err := c.LookupInvoiceByString(context.Background(), "lnbcrt10n1pabc"); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("want ErrNoResponse, got %v", err)
	}
	w.next()
}

func TestLookupInvoiceByStringValidation(t *testing.T) {
	r := newRig(t)
	w := r.startWallet()
	c := r.dial(fastTune)
	for _, s := range []string{"", strings.Repeat("a", 8193), "ln\n", "ln\x7f", "lnbé", "ln\x00"} {
		if _, err := c.LookupInvoiceByString(context.Background(), s); err == nil {
			t.Errorf("%q: want an error", s)
		}
	}
	if n := w.received.Load(); n != 0 {
		t.Fatalf("the wallet received %d requests, want 0", n)
	}
}
