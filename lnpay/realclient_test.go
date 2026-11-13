package lnpay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/nip44"
	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/refund/refundtest"
	"github.com/satbooth/satbooth/relay"
	"github.com/satbooth/satbooth/store"
)

// rcWalletKey is the secret key of BIP-340 test vector 1; rcClientKey is the secret key 3 (BIP-340 test vector 0).
const (
	rcClientKey = "0000000000000000000000000000000000000000000000000000000000000003"
	rcWalletKey = "b7e151628aed2a6abf7158809cf4f3c762e7160f38b4da56a784d9045190cfef"
	preimage    = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// rcWallet is a scripted hub on the in-process relay: it answers make_invoice and lookup_invoice and
// can publish a payment_received notification. With mute set it reads requests and answers none.
type rcWallet struct {
	t      *testing.T
	key    *nostr.SecretKey
	client string
	conv   [32]byte
	ws     *websocket.Conn

	mu     sync.Mutex
	mute   bool
	seq    int
	txs    map[string]*nwc.Transaction
	makes  int
	descs  []string
	oksMap map[string]chan bool
}

type rcRig struct {
	t      *testing.T
	wallet *rcWallet
	client *nwc.Client
}

func key(t *testing.T, h string) *nostr.SecretKey {
	t.Helper()
	k, err := nostr.ParseSecretKeyHex(h)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newRCRig(t *testing.T) *rcRig {
	t.Helper()
	ck, wk := key(t, rcClientKey), key(t, rcWalletKey)
	hubL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	clientURL := "ws://" + cl.Addr().String()
	cfgJSON := fmt.Sprintf(`{"hub_listen":%q,"client_listen":%q,"client_relay_url":%q,"pairs":[{"wallet":%q,"client":%q}]}`,
		hubL.Addr().String(), cl.Addr().String(), clientURL, wk.PublicKeyHex(), ck.PublicKeyHex())
	cfg, err := relay.ParseConfig([]byte(cfgJSON))
	if err != nil {
		t.Fatal(err)
	}
	srv := relay.NewServer(cfg, nil, nil)
	hubSrv := httptest.NewUnstartedServer(srv.Handler(relay.HubListener))
	hubSrv.Listener.Close()
	hubSrv.Listener = hubL
	hubSrv.Start()
	cSrv := httptest.NewUnstartedServer(srv.Handler(relay.ClientListener))
	cSrv.Listener.Close()
	cSrv.Listener = cl
	cSrv.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		hubSrv.Close()
		cSrv.Close()
	})

	conv, err := nip44.ConversationKey(wk, ck.PublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://"+hubL.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(1 << 20)
	w := &rcWallet{t: t, key: wk, client: ck.PublicKeyHex(), conv: conv, ws: ws, txs: map[string]*nwc.Transaction{},
		oksMap: map[string]chan bool{}}
	t.Cleanup(func() { ws.CloseNow() })
	req := fmt.Sprintf(`["REQ","w",{"kinds":[23194],"#p":[%q]}]`, wk.PublicKeyHex())
	if err := ws.Write(ctx, websocket.MessageText, []byte(req)); err != nil {
		t.Fatal(err)
	}
	eose := make(chan struct{})
	go w.loop(eose)
	select {
	case <-eose:
	case <-ctx.Done():
		t.Fatal("wallet: no EOSE")
	}

	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dcancel()
	c, err := nwc.Dial(dctx, nwc.Config{RelayURL: clientURL, WalletPubKey: wk.PublicKeyHex(), Key: ck, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &rcRig{t: t, wallet: w, client: c}
}

func (w *rcWallet) loop(eose chan struct{}) {
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
			var id string
			var ok bool
			if len(parts) >= 3 && json.Unmarshal(parts[1], &id) == nil && json.Unmarshal(parts[2], &ok) == nil {
				w.mu.Lock()
				ch := w.oksMap[id]
				w.mu.Unlock()
				if ch != nil {
					ch <- ok
				}
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
			go w.handle(ev, m.Method, m.Params)
		}
	}
}

func (w *rcWallet) txJSON(tx *nwc.Transaction) string {
	b, _ := json.Marshal(map[string]any{"type": tx.Type, "state": tx.State, "invoice": tx.Invoice, "description": tx.Description,
		"description_hash": "", "preimage": preimage, "payment_hash": tx.PaymentHash, "amount": tx.AmountMsat, "fees_paid": 0,
		"created_at": 1800000000, "expires_at": 1800001200, "settled_at": nil})
	return string(b)
}

func (w *rcWallet) handle(req *nostr.Event, method string, params json.RawMessage) {
	w.mu.Lock()
	if method == "make_invoice" {
		w.makes++
	}
	if w.mute {
		w.mu.Unlock()
		return
	}
	var result string
	switch method {
	case "make_invoice":
		var p struct {
			Amount      int64  `json:"amount"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(params, &p)
		w.seq++
		sum := sha256.Sum256([]byte(fmt.Sprintf("rc invoice %d", w.seq)))
		h := hex.EncodeToString(sum[:])
		tx := &nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StatePending, Invoice: refundtest.InvoiceFor(approval.Regtest, w.seq, p.Amount), Description: p.Description,
			PaymentHash: h, AmountMsat: p.Amount}
		w.txs[h] = tx
		w.descs = append(w.descs, p.Description)
		result = w.txJSON(tx)
	case "lookup_invoice":
		var p struct {
			PaymentHash string `json:"payment_hash"`
		}
		_ = json.Unmarshal(params, &p)
		tx, ok := w.txs[p.PaymentHash]
		if !ok {
			w.mu.Unlock()
			w.publish(w.build(nostr.KindNWCResponse, [][]string{{"p", w.client}, {"e", req.ID}},
				`{"result_type":"lookup_invoice","error":{"code":"NOT_FOUND","message":"not found"}}`))
			return
		}
		result = w.txJSON(tx)
	default:
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	w.publish(w.build(nostr.KindNWCResponse, [][]string{{"p", w.client}, {"e", req.ID}},
		fmt.Sprintf(`{"result_type":%q,"result":%s}`, method, result)))
}

func (w *rcWallet) build(kind int, tags [][]string, plain string) *nostr.Event {
	payload, err := nip44.Encrypt(w.conv, plain)
	if err != nil {
		w.t.Error(err)
	}
	e := &nostr.Event{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: payload}
	if err := nostr.Sign(e, w.key); err != nil {
		w.t.Error(err)
	}
	return e
}

func (w *rcWallet) publish(e *nostr.Event) {
	ch := make(chan bool, 1)
	w.mu.Lock()
	w.oksMap[e.ID] = ch
	w.mu.Unlock()
	b, _ := json.Marshal(e)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.ws.Write(ctx, websocket.MessageText, append(append([]byte(`["EVENT",`), b...), ']')); err != nil {
		return
	}
	select {
	case <-ch:
	case <-ctx.Done():
	}
}

// settle marks the invoice settled at the hub and, with notify, publishes a payment_received
// notification.
func (w *rcWallet) settle(hash string, notify bool) {
	w.mu.Lock()
	tx := w.txs[hash]
	tx.State = nwc.StateSettled
	body := w.txJSON(tx)
	w.mu.Unlock()
	if notify {
		w.publish(w.build(nostr.KindNWCNotification, [][]string{{"p", w.client}},
			`{"notification_type":"payment_received","notification":`+body+`}`))
	}
}

func (w *rcWallet) setMute(m bool) {
	w.mu.Lock()
	w.mute = m
	w.mu.Unlock()
}

func (w *rcWallet) makeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.makes
}

func newRealEnv(t *testing.T, pollRate float64) (*env, *rcRig) {
	t.Helper()
	rig := newRCRig(t)
	e := newEnv(t, func(e *env) { e.cfg.PollRate = pollRate })
	e.svc.w = rig.client
	return e, rig
}

// The real nwc client on the in-process relay: Pay, then a payment found by the poller.
func TestRealClientHappyPath(t *testing.T) {
	e, rig := newRealEnv(t, 1000)
	o := e.order("acct", 7_000_000)
	inv := e.mustPay(o)
	rig.wallet.mu.Lock()
	descs := append([]string(nil), rig.wallet.descs...)
	rig.wallet.mu.Unlock()
	if len(descs) != 1 || descs[0] != "SatBooth order "+o.ID {
		t.Fatalf("descriptions %q", descs)
	}
	if inv.AmountMsat != 7_000_000 || !validHash(inv.PaymentHash) {
		t.Fatalf("invoice %+v", inv)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	rig.wallet.settle(inv.PaymentHash, false)
	eventually(t, "the poller to record the payment", func() bool { return e.eventCount(store.EventPaid, o.ID) == 1 })
	cancel()
	<-done
	if e.transfers(o) != 1 || e.merchantCredits() != 7_000_000 || e.eventCount(store.EventPaid, o.ID) != 1 {
		t.Fatalf("not exactly once: transfers %d credits %d paid events %d", e.transfers(o), e.merchantCredits(), e.eventCount(store.EventPaid, o.ID))
	}
}

// A real payment_received notification, repeated, with the poller running: one record.
func TestRealClientNotification(t *testing.T) {
	e, rig := newRealEnv(t, 1000)
	o := e.order("acct", 7_000_000)
	inv := e.mustPay(o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	for i := 0; i < 3; i++ {
		rig.wallet.settle(inv.PaymentHash, true)
	}
	eventually(t, "the order to be paid", func() bool { return e.eventCount(store.EventPaid, o.ID) == 1 })
	cancel()
	<-done
	if e.transfers(o) != 1 || e.merchantCredits() != 7_000_000 || e.eventCount(store.EventPaid, o.ID) != 1 {
		t.Fatalf("not exactly once: transfers %d credits %d paid events %d", e.transfers(o), e.merchantCredits(), e.eventCount(store.EventPaid, o.ID))
	}
}

// A wallet that never answers: Pay ends with ErrUnknown when the caller's deadline passes, the
// attempt counts, and nothing is stored (the flight itself ends at the nwc timeout).
func TestRealClientTimeout(t *testing.T) {
	e, rig := newRealEnv(t, 1000)
	rig.wallet.setMute(true)
	o := e.order("acct", 7_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := e.svc.Pay(ctx, o.Token)
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("got %v", err)
	}
	if len(e.open_(o)) != 0 {
		t.Fatal("stored")
	}
	if n, _, _ := e.st.AttemptsSince(context.Background(), epoch); n != 1 {
		t.Fatalf("%d attempts", n)
	}
	// The flight itself goes on (detached from the presser) until the client's own timeout.
	eventually(t, "the wallet to have seen the request", func() bool { return rig.wallet.makeCount() == 1 })
}
