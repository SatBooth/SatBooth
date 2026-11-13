package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/nip44"
	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/refund/refundtest"
	"github.com/satbooth/satbooth/relay"
	"github.com/satbooth/satbooth/webhook"
)

// countListener counts the connections accepted and still open.
type countListener struct {
	net.Listener
	open atomic.Int64
}

type countConn struct {
	net.Conn
	l    *countListener
	once sync.Once
}

func (c *countConn) Close() error {
	c.once.Do(func() { c.l.open.Add(-1) })
	return c.Conn.Close()
}

func (l *countListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.open.Add(1)
		return &countConn{Conn: c, l: l}, nil
	}
	return c, err
}

// hubWallet plays the Alby Hub on the relay's hub listener: it answers make_invoice and lookup_invoice,
// and publishes a payment_received notification when an invoice is settled.
type hubWallet struct {
	t      *testing.T
	key    *nostr.SecretKey
	client string
	conv   [32]byte
	ws     *websocket.Conn
	mu     sync.Mutex
	seq    int
	invs   map[string]*nwc.Transaction
	wmu    sync.Mutex
}

func (w *hubWallet) publish(kind int, tags [][]string, plaintext string) {
	payload, err := nip44.Encrypt(w.conv, plaintext)
	if err != nil {
		w.t.Error(err)
		return
	}
	e := &nostr.Event{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: payload}
	if err := nostr.Sign(e, w.key); err != nil {
		w.t.Error(err)
		return
	}
	b, _ := json.Marshal(e)
	w.wmu.Lock()
	defer w.wmu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.ws.Write(ctx, websocket.MessageText, append(append([]byte(`["EVENT",`), b...), ']')); err != nil {
		w.t.Errorf("wallet write: %v", err)
	}
}

func (w *hubWallet) respond(req *nostr.Event, resultType string, result any) {
	b, _ := json.Marshal(map[string]any{"result_type": resultType, "result": result})
	w.publish(nostr.KindNWCResponse, [][]string{{"p", w.client}, {"e", req.ID}}, string(b))
}

func (w *hubWallet) loop() {
	for {
		_, data, err := w.ws.Read(context.Background())
		if err != nil {
			return
		}
		var parts []json.RawMessage
		if json.Unmarshal(data, &parts) != nil || len(parts) < 3 {
			continue
		}
		var label string
		_ = json.Unmarshal(parts[0], &label)
		if label != "EVENT" {
			continue
		}
		ev, err := nostr.ParseEvent(parts[2])
		if err != nil {
			continue
		}
		plain, err := nip44.Decrypt(w.conv, ev.Content)
		if err != nil {
			continue
		}
		var m struct {
			Method string `json:"method"`
			Params struct {
				Amount      int64  `json:"amount"`
				Description string `json:"description"`
				PaymentHash string `json:"payment_hash"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(plain), &m) != nil {
			continue
		}
		w.mu.Lock()
		switch m.Method {
		case "make_invoice":
			w.seq++
			sum := sha256.Sum256([]byte(fmt.Sprintf("relay test invoice %d", w.seq)))
			h := hex.EncodeToString(sum[:])
			exp := time.Now().Add(20 * time.Minute).Unix()
			tx := &nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StatePending, Invoice: refundtest.InvoiceFor(approval.Regtest, w.seq, m.Params.Amount),
				Description: m.Params.Description, PaymentHash: h, AmountMsat: m.Params.Amount, CreatedAt: time.Now().Unix(), ExpiresAt: &exp}
			w.invs[h] = tx
			c := *tx
			w.mu.Unlock()
			w.respond(ev, "make_invoice", c)
		case "lookup_invoice":
			tx, ok := w.invs[strings.ToLower(m.Params.PaymentHash)]
			var c nwc.Transaction
			if ok {
				c = *tx
			}
			w.mu.Unlock()
			if ok {
				w.respond(ev, "lookup_invoice", c)
			} else {
				b, _ := json.Marshal(map[string]any{"result_type": "lookup_invoice", "error": map[string]string{"code": "NOT_FOUND", "message": "x"}})
				w.publish(nostr.KindNWCResponse, [][]string{{"p", w.client}, {"e", ev.ID}}, string(b))
			}
		case "get_info":
			w.mu.Unlock()
			w.respond(ev, "get_info", map[string]any{"network": "regtest",
				"methods": []string{"make_invoice", "lookup_invoice", "list_transactions", "get_info", "get_budget"}})
		default:
			w.mu.Unlock()
		}
	}
}

func (w *hubWallet) settleAll() int {
	w.mu.Lock()
	now := time.Now().Unix()
	var txs []nwc.Transaction
	for _, tx := range w.invs {
		tx.State, tx.SettledAt = nwc.StateSettled, &now
		txs = append(txs, *tx)
	}
	w.mu.Unlock()
	for _, tx := range txs {
		b, _ := json.Marshal(map[string]any{"notification_type": "payment_received", "notification": tx})
		w.publish(nostr.KindNWCNotification, [][]string{{"p", w.client}}, string(b))
	}
	return len(txs)
}

// TestEndToEndThroughARelay: Run dials a real nwc client at an in-process relay.Server, the scripted
// hub on the hub listener makes and settles the invoice, and the order is paid and the webhook sent.
// It also shows the client's connection is closed on shutdown.
func TestEndToEndThroughARelay(t *testing.T) {
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	clientKey, err := nostr.LoadSecretKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer clientKey.Zero()
	walletKey, err := nostr.GenerateSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	defer walletKey.Zero()

	hubL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	clientL := &countListener{Listener: cl}
	relayURL := "ws://" + cl.Addr().String()
	rcfg, err := relay.ParseConfig([]byte(fmt.Sprintf(`{"hub_listen":%q,"client_listen":%q,"client_relay_url":%q,"pairs":[{"wallet":%q,"client":%q}]}`,
		hubL.Addr().String(), cl.Addr().String(), relayURL, walletKey.PublicKeyHex(), clientKey.PublicKeyHex())))
	if err != nil {
		t.Fatal(err)
	}
	rsrv := relay.NewServer(rcfg, nil, nil)
	hubSrv := httptest.NewUnstartedServer(rsrv.Handler(relay.HubListener))
	hubSrv.Listener.Close()
	hubSrv.Listener = hubL
	hubSrv.Start()
	cliSrv := httptest.NewUnstartedServer(rsrv.Handler(relay.ClientListener))
	cliSrv.Listener.Close()
	cliSrv.Listener = clientL
	cliSrv.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		rsrv.Shutdown(ctx)
		hubSrv.Close()
		cliSrv.Close()
	})

	// The scripted hub.
	conv, err := nip44.ConversationKey(walletKey, clientKey.PublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	ws, _, err := websocket.Dial(wctx, "ws://"+hubL.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)
	hub := &hubWallet{t: t, key: walletKey, client: clientKey.PublicKeyHex(), conv: conv, ws: ws, invs: map[string]*nwc.Transaction{}}
	req := fmt.Sprintf(`["REQ","w",{"kinds":[23194],"#p":[%q]}]`, walletKey.PublicKeyHex())
	if err := ws.Write(wctx, websocket.MessageText, []byte(req)); err != nil {
		t.Fatal(err)
	}
	go hub.loop()

	// The service.
	hooks := make(chan hookEvent, 4)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [4096]byte
		n, _ := r.Body.Read(b[:])
		select {
		case hooks <- hookEvent{r.Header.Get(webhook.HeaderName), append([]byte(nil), b[:n]...)}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer recv.Close()
	pubL, _ := net.Listen("tcp", "127.0.0.1:0")
	prvL, _ := net.Listen("tcp", "127.0.0.1:0")
	pubURL, prvURL := "http://"+pubL.Addr().String(), "http://"+prvL.Addr().String()
	m := goodConfig()
	m["checkout_base_url"] = pubURL
	m["data_dir"] = filepath.Join(dir, "data")
	m["nwc"] = map[string]any{"relay_url": relayURL, "wallet_pubkey": walletKey.PublicKeyHex(), "client_key_file": keyPath, "timeout_seconds": 30, "network": "regtest"}
	m["api_keys_file"] = apiPath
	m["webhooks"] = []any{map[string]any{"name": "main", "url": recv.URL, "secret_file": whPath}}
	cfg, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, WithListeners(pubL, prvL), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	}()
	eventually(t, "the service to start", func() bool { return strings.Contains(logs.String(), logStarted) })
	if clientL.open.Load() != 1 {
		t.Fatalf("want one client connection to the relay, got %d", clientL.open.Load())
	}

	body := `{"account_id":"acct1","merchant_ref":"relay-ref","lightning_msat":21000000}`
	code, b := do(t, "POST", prvURL+"/v1/orders", testAPIKey, []byte(body))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	var o orderResp
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, "POST", o.CheckoutURL+"/pay", "", nil); code != http.StatusSeeOther {
		t.Fatalf("pay: %d", code)
	}
	if n := hub.settleAll(); n != 1 {
		t.Fatalf("hub holds %d invoices, want 1", n)
	}
	eventually(t, "the order to be paid", func() bool {
		_, b := do(t, "GET", prvURL+"/v1/orders/"+o.ID, testReadKey, nil)
		var g orderResp
		return json.Unmarshal(b, &g) == nil && g.State == "paid"
	})
	select {
	case h := <-hooks:
		secret, _ := hex.DecodeString(testWebhook)
		if err := webhook.Verify(secret, h.header, h.body, time.Now(), webhook.RecommendedWindow); err != nil {
			t.Fatalf("webhook signature: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no webhook")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return")
	}
	// Shutdown closed the nwc client, so the relay sees its connection gone.
	eventually(t, "the client connection to close", func() bool { return clientL.open.Load() == 0 })
	for _, bad := range []string{testAPIKey, testReadKey, testWebhook, "lnbcrt"} {
		if strings.Contains(logs.String(), bad) {
			t.Fatalf("logs contain %q", bad)
		}
	}
}
