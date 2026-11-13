package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/descriptor"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/monero"
	"github.com/satbooth/satbooth/monero/monerotest"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/rates"
	"github.com/satbooth/satbooth/rates/ratestest"
	"github.com/satbooth/satbooth/refund/refundtest"
	"github.com/satbooth/satbooth/status"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/webhook"
	"github.com/satbooth/satbooth/xmrrpc"
)

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

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeWallet is a scripted wallet: invoices stay pending until settle is called.
type fakeWallet struct {
	mu      sync.Mutex
	seq     int
	invs    map[string]*nwc.Transaction
	notifs  chan nwc.Notification
	entered chan struct{} // a MakeInvoice call announces itself here (buffered)
	block   chan struct{} // when non-nil, MakeInvoice waits for it to close or for its context
	methods []string      // when non-nil, the methods get_info reports
	infoErr error         // when non-nil, get_info fails
	network string        // the network get_info states: "regtest", the rig's, unless a test sets it ("" states none)
}

func newFakeWallet() *fakeWallet {
	return &fakeWallet{invs: map[string]*nwc.Transaction{}, notifs: make(chan nwc.Notification, 16), entered: make(chan struct{}, 16),
		network: "regtest"}
}

// GetInfo answers like an invoice connection: no pay_invoice (Run's start-up check asks for it).
func (w *fakeWallet) GetInfo(context.Context) (*nwc.Info, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.infoErr != nil {
		return nil, w.infoErr
	}
	if w.methods != nil {
		return &nwc.Info{Network: w.network, Methods: w.methods}, nil
	}
	return &nwc.Info{Network: w.network,
		Methods: []string{"make_invoice", "lookup_invoice", "list_transactions", "get_info", "get_budget"}}, nil
}

func (w *fakeWallet) Notifications() <-chan nwc.Notification { return w.notifs }

func (w *fakeWallet) MakeInvoice(ctx context.Context, p nwc.MakeInvoiceParams) (*nwc.Transaction, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	w.mu.Lock()
	block := w.block
	w.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	sum := sha256.Sum256([]byte(fmt.Sprintf("service test invoice %d", w.seq)))
	h := hex.EncodeToString(sum[:])
	tx := &nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StatePending, Invoice: refundtest.InvoiceFor(approval.Regtest, w.seq, p.AmountMsat),
		Description: p.Description, PaymentHash: h, AmountMsat: p.AmountMsat}
	w.invs[h] = tx
	c := *tx
	return &c, nil
}

func (w *fakeWallet) LookupInvoice(_ context.Context, hash string) (*nwc.Transaction, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	tx, ok := w.invs[hash]
	if !ok {
		return nil, &nwc.WalletError{Code: "NOT_FOUND", Message: "x"}
	}
	c := *tx
	return &c, nil
}

func (w *fakeWallet) settleAll() (hashes []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now().Unix()
	for h, tx := range w.invs {
		tx.State, tx.SettledAt = nwc.StateSettled, &now
		hashes = append(hashes, h)
		w.notifs <- nwc.Notification{Type: nwc.NotifPaymentReceived, Tx: *tx}
	}
	return
}

func (w *fakeWallet) invoices() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, tx := range w.invs {
		out = append(out, tx.Invoice, tx.PaymentHash)
	}
	return out
}

type hookEvent struct {
	header string
	body   []byte
}

type rig struct {
	t         *testing.T
	cfg       *Config
	dir       string
	wallet    *fakeWallet
	logs      *syncBuf
	pubURL    string
	prvURL    string
	pubLn     net.Listener
	prvLn     net.Listener
	cancel    context.CancelFunc
	done      chan error
	reload    chan struct{}
	hooks     chan hookEvent
	keysPath  string
	stopOnce  sync.Once
	doneErr   error
	doneAlive bool
}

// startRig starts the service with a fake wallet, in-memory listeners on loopback and a webhook
// receiver. The rig stops the service at the end of the test.
func startRig(t *testing.T, extra ...Option) *rig {
	t.Helper()
	return startRigWith(t, nil, extra...)
}

// startRigWith is startRig with a change to the configuration map before it is parsed.
func startRigWith(t *testing.T, mod func(dir string, m map[string]any), extra ...Option) *rig {
	t.Helper()
	r := &rig{t: t, dir: t.TempDir(), wallet: newFakeWallet(), logs: &syncBuf{}, hooks: make(chan hookEvent, 16), reload: make(chan struct{}, 1)}
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		select {
		case r.hooks <- hookEvent{req.Header.Get(webhook.HeaderName), b}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(recv.Close)
	var err error
	if r.pubLn, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if r.prvLn, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	r.pubURL, r.prvURL = "http://"+r.pubLn.Addr().String(), "http://"+r.prvLn.Addr().String()
	keyPath, apiPath, whPath := writeFixtures(t, r.dir)
	r.keysPath = apiPath
	m := goodConfig()
	m["checkout_base_url"] = r.pubURL
	m["data_dir"] = filepath.Join(r.dir, "data")
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	m["webhooks"] = []any{map[string]any{"name": "main", "url": recv.URL, "secret_file": whPath}}
	if mod != nil {
		mod(r.dir, m)
	}
	if r.cfg, err = ParseConfig(enc(t, m)); err != nil {
		t.Fatal(err)
	}
	opts := append([]Option{WithWallet(r.wallet), WithListeners(r.pubLn, r.prvLn), WithReload(r.reload),
		WithLogger(slog.New(slog.NewJSONHandler(r.logs, nil)))}, extra...)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan error, 1)
	go func() { r.done <- Run(ctx, r.cfg, opts...) }()
	t.Cleanup(func() { r.stop() })
	eventually(t, "the service to start", func() bool { return strings.Contains(r.logs.String(), logStarted) })
	return r
}

// stop cancels the service and waits for Run, once.
func (r *rig) stop() error {
	r.stopOnce.Do(func() {
		r.cancel()
		select {
		case r.doneErr = <-r.done:
		case <-time.After(30 * time.Second):
			r.t.Error("Run did not return")
		}
	})
	return r.doneErr
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 20 * time.Second}

func do(t testing.TB, method, url, key string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

type orderResp struct {
	ID          string  `json:"id"`
	State       string  `json:"state"`
	PaidAt      *string `json:"paid_at"`
	CheckoutURL string  `json:"checkout_url"`
}

func (r *rig) createOrder(ref string) orderResp {
	r.t.Helper()
	body := fmt.Sprintf(`{"account_id":"acct1","merchant_ref":%q,"lightning_msat":21000000}`, ref)
	code, b := do(r.t, "POST", r.prvURL+"/v1/orders", testAPIKey, []byte(body))
	if code != http.StatusCreated {
		r.t.Fatalf("create: %d %s", code, b)
	}
	var o orderResp
	if err := json.Unmarshal(b, &o); err != nil {
		r.t.Fatal(err)
	}
	return o
}

func (r *rig) getOrder(id string) orderResp {
	r.t.Helper()
	code, b := do(r.t, "GET", r.prvURL+"/v1/orders/"+id, testReadKey, nil)
	if code != http.StatusOK {
		r.t.Fatalf("get: %d %s", code, b)
	}
	var o orderResp
	if err := json.Unmarshal(b, &o); err != nil {
		r.t.Fatal(err)
	}
	return o
}

func TestEndToEnd(t *testing.T) {
	r := startRig(t)
	o := r.createOrder("ref-1")
	if o.State != "new" || !strings.HasPrefix(o.CheckoutURL, r.pubURL+"/c/") {
		t.Fatalf("bad order: %+v", o)
	}
	// The customer opens the page and presses pay.
	if code, _ := do(t, "GET", o.CheckoutURL, "", nil); code != http.StatusOK {
		t.Fatalf("page: %d", code)
	}
	if code, _ := do(t, "POST", o.CheckoutURL+"/pay", "", nil); code != http.StatusSeeOther {
		t.Fatalf("pay: %d", code)
	}
	_, page := do(t, "GET", o.CheckoutURL, "", nil)
	token := strings.TrimPrefix(o.CheckoutURL, r.pubURL+"/c/")
	invoices := r.wallet.invoices()
	if len(invoices) != 2 { // invoice and payment hash of the one invoice
		t.Fatalf("want one invoice, got %v", invoices)
	}
	if !bytes.Contains(page, []byte(`src="/c/`+token+`/qr.svg"`)) {
		t.Fatalf("page has no QR image: %s", page)
	}
	if !bytes.Contains(page, []byte(`id="invoice" type="text" readonly="readonly" value="`+invoices[0]+`"`)) {
		t.Fatalf("page does not show the invoice in its field: %s", page)
	}
	// The wallet settles; the reconciler sees it.
	settling := time.Now()
	if hashes := r.wallet.settleAll(); len(hashes) != 1 {
		t.Fatalf("want one invoice, got %d", len(hashes))
	}
	eventually(t, "the order to be paid", func() bool { return r.getOrder(o.ID).State == "paid" })
	// paid_at is the time the reconciler recorded the payment: after the settlement started (the API
	// gives whole seconds) and not after now; never 1970 or a far future from a unit slip.
	if p := r.getOrder(o.ID).PaidAt; p == nil {
		t.Fatal("paid without paid_at")
	} else if at, err := time.Parse(time.RFC3339, *p); err != nil || at.Before(settling.Truncate(time.Second)) || at.After(time.Now()) {
		t.Fatalf("paid_at %q (err %v), want between %v and now", *p, err, settling)
	}
	// The webhook arrives, signed.
	select {
	case h := <-r.hooks:
		secret, _ := hex.DecodeString(testWebhook)
		if err := webhook.Verify(secret, h.header, h.body, time.Now(), webhook.RecommendedWindow); err != nil {
			t.Fatalf("webhook signature: %v", err)
		}
		var p struct {
			Type    string `json:"type"`
			OrderID string `json:"order_id"`
		}
		if err := json.Unmarshal(h.body, &p); err != nil || p.OrderID != o.ID {
			t.Fatalf("webhook body: %s", h.body)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no webhook")
	}
	if err := r.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Logs carry no secret, invoice, payment hash or order id.
	logs := r.logs.String()
	for _, bad := range append(r.wallet.invoices(), testAPIKey, testReadKey, testWebhook, o.ID, token) {
		if strings.Contains(logs, bad) {
			t.Fatalf("logs contain a forbidden value %q", bad[:6])
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
	}
}

func TestListenersServeOnlyTheirOwnRoutes(t *testing.T) {
	r := startRig(t)
	o := r.createOrder("ref-routes")
	token := strings.TrimPrefix(o.CheckoutURL, r.pubURL+"/c/")
	// The private listener refuses the checkout routes, whatever the credentials.
	for _, p := range []string{"/c/" + token, "/c/" + token + "/qr.svg", "/static/style.css", "/"} {
		for _, key := range []string{"", testAPIKey} {
			if code, _ := do(t, "GET", r.prvURL+p, key, nil); code != http.StatusNotFound {
				t.Errorf("private GET %s key=%v: %d", p, key != "", code)
			}
		}
	}
	if code, _ := do(t, "POST", r.prvURL+"/c/"+token+"/pay", testAPIKey, nil); code != http.StatusNotFound {
		t.Errorf("private POST pay: %d", code)
	}
	// The public listener refuses the API routes.
	if code, _ := do(t, "GET", r.pubURL+"/v1/orders/"+o.ID, testReadKey, nil); code != http.StatusNotFound {
		t.Errorf("public GET order: %d", code)
	}
	body := []byte(`{"account_id":"acct1","merchant_ref":"x","lightning_msat":21000000}`)
	if code, _ := do(t, "POST", r.pubURL+"/v1/orders", testAPIKey, body); code != http.StatusNotFound {
		t.Errorf("public POST order: %d", code)
	}
	// And the right routes still work on the right listeners.
	if code, _ := do(t, "GET", r.pubURL+"/static/style.css", "", nil); code != http.StatusOK {
		t.Errorf("public style: %d", code)
	}
}

func TestBodyLimitThroughTheService(t *testing.T) {
	r := startRig(t)
	big := append([]byte(`{"account_id":"acct1","merchant_ref":"`), bytes.Repeat([]byte("a"), MaxBodyBytes+1)...)
	big = append(big, []byte(`","lightning_msat":21000000}`)...)
	code, b := do(t, "POST", r.prvURL+"/v1/orders", testAPIKey, big)
	if code != http.StatusBadRequest || !bytes.Contains(b, []byte("body_too_large")) {
		t.Fatalf("oversized body: %d %s", code, b)
	}
	// The public pay route reads no body at all, but a huge one must not be buffered either.
	o := r.createOrder("ref-body")
	if code, _ := do(t, "POST", o.CheckoutURL+"/pay", "", bytes.Repeat([]byte("x"), 4*MaxBodyBytes)); code == http.StatusInternalServerError {
		t.Fatalf("oversized pay body: %d", code)
	}
}

func TestServerLimits(t *testing.T) {
	s := newServer(http.NotFoundHandler(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if s.ReadHeaderTimeout != 5*time.Second || s.ReadTimeout != 10*time.Second || s.WriteTimeout != 15*time.Second ||
		s.IdleTimeout != 60*time.Second || s.MaxHeaderBytes != 4<<10 {
		t.Fatalf("server limits: %+v", s)
	}
	// net/http alone would let about 12 KiB through; guard makes the cap 8 KiB: 7 KiB of headers pass, 9 KiB
	// get 431, and 32 KiB are refused by net/http itself.
	var seen int
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { seen++ })
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = newServer(h, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts.Start()
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL, nil)
	req.Header.Set("X-Pad", strings.Repeat("a", 7<<10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("7 KiB header refused: %d", resp.StatusCode)
	}
	req.Header.Set("X-Pad", strings.Repeat("a", 9<<10))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge || seen != 1 {
		t.Fatalf("9 KiB header: status %d, handler ran %d times", resp.StatusCode, seen)
	}
	req.Header.Set("X-Pad", strings.Repeat("a", 32<<10))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge || seen != 1 {
		t.Fatalf("32 KiB header: status %d, handler ran %d times", resp.StatusCode, seen)
	}
}

func TestConcurrencyCap(t *testing.T) {
	release := make(chan struct{})
	var inside sync.WaitGroup
	inside.Add(MaxConcurrent)
	h := guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			inside.Done()
			<-release
		}
		_, _ = io.WriteString(w, "ok")
	}))
	var wg sync.WaitGroup
	codes := make(chan int, MaxConcurrent)
	for i := 0; i < MaxConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/hold", nil))
			codes <- rec.Code
		}()
	}
	inside.Wait() // all 256 slots are taken
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/other", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != busyBody {
		t.Fatalf("257th request: %d %q", rec.Code, rec.Body.String())
	}
	close(release)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Fatalf("held request: %d", c)
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/other", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("after release: %d", rec.Code)
	}
}

func TestGuardBodyLimit(t *testing.T) {
	var got int
	var rerr error
	h := guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		got, rerr = len(b), err
	}))
	for _, n := range []int{0, 1, MaxBodyBytes - 1, MaxBodyBytes, MaxBodyBytes + 1, 10 * MaxBodyBytes} {
		got, rerr = 0, nil
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", bytes.NewReader(make([]byte, n))))
		if n <= MaxBodyBytes && (rerr != nil || got != n) {
			t.Errorf("%d bytes: read %d, err %v", n, got, rerr)
		}
		if n > MaxBodyBytes && rerr == nil {
			t.Errorf("%d bytes: no error", n)
		}
	}
}

func TestReloadKeys(t *testing.T) {
	r := startRig(t)
	sigs := make(chan os.Signal, 4)
	var exited []int
	supDone := make(chan struct{})
	go func() {
		defer close(supDone)
		Supervise(sigs, r.cancel, r.reload, func(c int) { exited = append(exited, c) })
	}()
	defer func() { close(sigs); <-supDone }()
	create := func(key string) int {
		code, _ := do(t, "POST", r.prvURL+"/v1/orders", key, []byte(`{"account_id":"acct1","merchant_ref":"ref-reload","lightning_msat":21000000}`))
		return code
	}
	if create(testAPIKey2) != http.StatusUnauthorized {
		t.Fatal("the new key works before the reload")
	}
	replace := func(content []byte) {
		tmp := r.keysPath + ".new"
		if err := os.WriteFile(tmp, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, r.keysPath); err != nil {
			t.Fatal(err)
		}
	}
	replace(keysJSON([3]string{"merchant", "orders:write", testAPIKey2}))
	sigs <- syscall.SIGHUP
	eventually(t, "the reload", func() bool { return strings.Contains(r.logs.String(), logReloaded) })
	if code := create(testAPIKey2); code != http.StatusCreated {
		t.Fatalf("new key after reload: %d", code)
	}
	if create(testAPIKey) != http.StatusUnauthorized {
		t.Fatal("the old key still works after the reload")
	}
	// A bad file keeps the keys in force and logs one fixed line.
	replace([]byte(`{"keys":[`))
	sigs <- syscall.SIGHUP
	eventually(t, "the failed reload", func() bool { return strings.Contains(r.logs.String(), logReloadFailed) })
	if code := create(testAPIKey2); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("key after a failed reload: %d", code)
	}
	if strings.Contains(r.logs.String(), testAPIKey2) {
		t.Fatal("a key reached the log")
	}
	// SIGTERM stops the service.
	sigs <- syscall.SIGTERM
	select {
	case err := <-r.done:
		r.doneErr = err
		r.stopOnce.Do(func() {})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("SIGTERM did not stop the service")
	}
}

func TestSupervise(t *testing.T) {
	sigs := make(chan os.Signal, 8)
	reload := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 4)
	exits := make(chan int, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		Supervise(sigs, func() { cancelled <- struct{}{} }, reload, func(c int) { exits <- c })
	}()
	sigs <- syscall.SIGHUP
	sigs <- syscall.SIGHUP // dropped: one reload is already pending
	sigs <- syscall.SIGUSR1
	sigs <- syscall.SIGINT
	<-cancelled
	select {
	case <-exits:
		t.Fatal("exited on the first signal")
	default:
	}
	if len(reload) != 1 {
		t.Fatalf("pending reloads: %d", len(reload))
	}
	sigs <- syscall.SIGTERM
	if c := <-exits; c != 1 {
		t.Fatalf("exit code %d", c)
	}
	<-done
	if len(cancelled) != 0 {
		t.Fatal("cancel called twice")
	}
}

// inFlight starts a pay request that blocks in the wallet and returns when the wallet has it.
func inFlight(t *testing.T, r *rig) chan int {
	t.Helper()
	o := r.createOrder("ref-inflight")
	block := make(chan struct{})
	r.wallet.mu.Lock()
	r.wallet.block = block
	r.wallet.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-block:
		default:
			close(block)
		}
	})
	res := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("POST", o.CheckoutURL+"/pay", nil)
		resp, err := noRedirect.Do(req)
		if err != nil {
			res <- -1
			return
		}
		resp.Body.Close()
		res <- resp.StatusCode
	}()
	select {
	case <-r.wallet.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the wallet")
	}
	return res
}

func refuses(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		c.Close()
		return false
	}
	return true
}

func TestShutdownLetsInFlightRequestsFinish(t *testing.T) {
	r := startRig(t)
	res := inFlight(t, r)
	r.cancel()
	// The listeners stop accepting at once, while the request is still being served.
	eventually(t, "the public listener to stop accepting", func() bool { return refuses(r.pubLn.Addr().String()) })
	eventually(t, "the private listener to stop accepting", func() bool { return refuses(r.prvLn.Addr().String()) })
	select {
	case <-r.done:
		t.Fatal("Run returned with a request in flight")
	case <-time.After(200 * time.Millisecond):
	}
	r.wallet.mu.Lock()
	close(r.wallet.block)
	r.wallet.block = nil
	r.wallet.mu.Unlock()
	start := time.Now()
	if err := r.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("shutdown took %v after the request finished", time.Since(start))
	}
	if code := <-res; code != http.StatusSeeOther {
		t.Fatalf("in-flight request: %d", code)
	}
}

func TestShutdownWithAStuckRequestTakesTheGrace(t *testing.T) {
	r := startRig(t, withGrace(400*time.Millisecond))
	res := inFlight(t, r)
	start := time.Now()
	if err := r.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 5*time.Second {
		t.Fatalf("shutdown took %v with a 400 ms grace", d)
	}
	<-res
	if !strings.Contains(r.logs.String(), logForcedClose) {
		t.Fatal("the forced close was not logged")
	}
}

// The real grace: a stuck request holds the shutdown for 10 s, not longer.
func TestShutdownWithinTenSeconds(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 10 s")
	}
	r := startRig(t)
	inFlight(t, r)
	start := time.Now()
	r.cancel()
	err := r.stop()
	d := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d < 9*time.Second || d > 12*time.Second {
		t.Fatalf("shutdown took %v, want about 10 s", d)
	}
}

func TestStartupRefusals(t *testing.T) {
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	m := goodConfig()
	m["data_dir"] = filepath.Join(dir, "data")
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	m["webhooks"] = []any{map[string]any{"name": "main", "url": "http://127.0.0.1:9/h", "secret_file": whPath}}
	cfg, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(c *Config){
		"client key": func(c *Config) { c.NWC.ClientKeyFile = filepath.Join(dir, "nope") },
		"api keys":   func(c *Config) { c.APIKeysFile = filepath.Join(dir, "nope") },
		"webhook secret": func(c *Config) {
			c.Webhooks = []WebhookEndpoint{{Name: "main", URL: "http://127.0.0.1:9/h", SecretFile: filepath.Join(dir, "nope")}}
		},
	} {
		c := *cfg
		mod(&c)
		err := Run(context.Background(), &c, WithWallet(newFakeWallet()), WithLogger(slog.New(slog.NewJSONHandler(io.Discard, nil))))
		if err == nil || !strings.HasPrefix(err.Error(), "service: ") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A Config built by struct literal never went through ParseConfig: Run refuses a count the worker guard cannot hold,
	// before it opens anything.
	for name, n := range map[string]int{"no endpoints": 0, "nine endpoints": MaxWebhookEndpoints + 1} {
		c := *cfg
		c.Webhooks = nil
		for i := 0; i < n; i++ {
			c.Webhooks = append(c.Webhooks, WebhookEndpoint{Name: fmt.Sprintf("ep%d", i), URL: fmt.Sprintf("http://127.0.0.1:9/h%d", i), SecretFile: whPath})
		}
		t.Run(name, func(t *testing.T) {
			startRefuses(t, &c, "webhooks", "webhooks must list 1 to 8 endpoints", WithWallet(newFakeWallet()))
			if _, err := os.Stat(c.DataDir); !os.IsNotExist(err) {
				t.Errorf("the data directory was made: %v", err)
			}
		})
	}
}

// A Config built by struct literal is held to the list's rules too: two endpoints on one URL, or an alert URL equal to
// an endpoint's, are refused before anything opens, with texts that name positions and carry no value.
func TestRunAppliesTheEndpointRulesToAStructLiteral(t *testing.T) {
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	m := goodConfig()
	m["data_dir"] = filepath.Join(dir, "data")
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	m["webhooks"] = []any{map[string]any{"name": "main", "url": "http://127.0.0.1:9/h", "secret_file": whPath}}
	cfg, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		mod    func(c *Config)
		reason string
		text   string
	}{
		"one URL twice": {func(c *Config) {
			c.Webhooks = append(c.Webhooks, WebhookEndpoint{Name: "other", URL: "HTTP://127.0.0.1:9/h", SecretFile: whPath + "2"})
		}, "webhooks", "webhooks[1].url repeats webhooks[0].url"},
		"alert URL of an endpoint": {func(c *Config) {
			c.Alerts = &WebhookConfig{URL: c.Webhooks[0].URL, SecretFile: whPath + "2"}
		}, "alerts", "alerts.url must differ from webhooks[0].url"},
	} {
		c := *cfg
		c.Webhooks = slices.Clone(cfg.Webhooks)
		tc.mod(&c)
		t.Run(name, func(t *testing.T) {
			_, logs := startRefuses(t, &c, tc.reason, tc.text, WithWallet(newFakeWallet()))
			if strings.Contains(logs, "127.0.0.1:9") {
				t.Errorf("a URL in the logs: %s", logs)
			}
			if _, err := os.Stat(c.DataDir); !os.IsNotExist(err) {
				t.Errorf("the data directory was made: %v", err)
			}
		})
	}
}

// Without a wallet override Run dials the relay; with nothing listening it fails quickly, with an
// error that holds no secret.
func TestDialFailure(t *testing.T) {
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relayAddr := free.Addr().String()
	free.Close()
	m := goodConfig()
	m["data_dir"] = filepath.Join(dir, "data")
	m["nwc"] = map[string]any{"relay_url": "ws://" + relayAddr, "wallet_pubkey": goodPub, "client_key_file": keyPath, "network": "regtest"}
	m["api_keys_file"] = apiPath
	m["webhooks"] = []any{map[string]any{"name": "main", "url": "http://127.0.0.1:9/h", "secret_file": whPath}}
	cfg, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	start := time.Now()
	err = Run(context.Background(), cfg, WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	if err == nil || time.Since(start) > 20*time.Second {
		t.Fatalf("Run: %v after %v", err, time.Since(start))
	}
	for _, bad := range []string{testWebhook, testAPIKey} {
		if strings.Contains(err.Error(), bad) || strings.Contains(logs.String(), bad) {
			t.Fatal("a secret reached the error or the log")
		}
	}
	// data_dir was created 0700.
	fi, statErr := os.Stat(filepath.Join(dir, "data"))
	if statErr != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("data_dir: %v %v", fi, statErr)
	}
}

// Run binds the configured addresses when it is given no listeners.
func TestRunBindsConfiguredAddresses(t *testing.T) {
	var addrs [2]string
	for i := range addrs {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addrs[i] = l.Addr().String()
		l.Close()
	}
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	m := goodConfig()
	m["public_listen"], m["private_listen"] = addrs[0], addrs[1]
	m["checkout_base_url"] = "http://" + addrs[0]
	m["data_dir"] = filepath.Join(dir, "data")
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	m["webhooks"] = []any{map[string]any{"name": "main", "url": "http://127.0.0.1:9/h", "secret_file": whPath}}
	cfg, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, WithWallet(newFakeWallet()), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	}()
	eventually(t, "start", func() bool { return strings.Contains(logs.String(), logStarted) })
	if code, _ := do(t, "GET", "http://"+addrs[0]+"/static/style.css", "", nil); code != http.StatusOK {
		t.Fatalf("public: %d", code)
	}
	if code, _ := do(t, "GET", "http://"+addrs[1]+"/v1/orders/x", testReadKey, nil); code != http.StatusNotFound {
		t.Fatalf("private: %d", code)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The ports are taken: a second Run on them fails cleanly.
	l, err := net.Listen("tcp", addrs[0])
	if err != nil {
		t.Fatalf("port not released: %v", err)
	}
	defer l.Close()
	err = Run(context.Background(), cfg, WithWallet(newFakeWallet()), WithLogger(slog.New(slog.NewJSONHandler(io.Discard, nil))))
	if err == nil {
		t.Fatal("Run on a taken port succeeded")
	}
}

// A handler panic reaches net/http's error log; the service's log must stay JSON with none of the
// panic text.
func TestHandlerPanicLogsAFixedLine(t *testing.T) {
	logs := &syncBuf{}
	const secret = "PANIC-VALUE-9f3a"
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = newServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(secret) }), slog.New(slog.NewJSONHandler(logs, nil)))
	ts.Start()
	defer ts.Close()
	if resp, err := http.Get(ts.URL); err == nil {
		resp.Body.Close()
	}
	eventually(t, "the error line", func() bool { return strings.Contains(logs.String(), logHTTPError) })
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		if m["msg"] != logHTTPError || len(m) != 3 { // time, level, msg
			t.Fatalf("unexpected line: %q", line)
		}
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "goroutine") {
		t.Fatal("panic text reached the log")
	}
}

func TestDataDirMode(t *testing.T) {
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	mk := func(data string) *Config {
		m := goodConfig()
		m["data_dir"] = data
		m["nwc"].(map[string]any)["client_key_file"] = keyPath
		m["api_keys_file"] = apiPath
		m["webhooks"] = []any{map[string]any{"name": "main", "url": "http://127.0.0.1:9/h", "secret_file": whPath}}
		c, err := ParseConfig(enc(t, m))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	run := func(c *Config) (error, string) {
		logs := &syncBuf{}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			// Not eventually(t, ...): its t.Fatalf must not run off the test goroutine. On a timeout the cancel
			// still stops Run, and the caller's assertions on the result report the failure.
			deadline := time.Now().Add(15 * time.Second)
			for !strings.Contains(logs.String(), logStarted) && ctx.Err() == nil && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
		}()
		defer cancel()
		pub, _ := net.Listen("tcp", "127.0.0.1:0")
		prv, _ := net.Listen("tcp", "127.0.0.1:0")
		defer pub.Close()
		defer prv.Close()
		err := Run(ctx, c, WithWallet(newFakeWallet()), WithListeners(pub, prv), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
		return err, logs.String()
	}
	loose := filepath.Join(dir, "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o755, 0o750, 0o701, 0o770} {
		if err := os.Chmod(loose, mode); err != nil {
			t.Fatal(err)
		}
		err, logs := run(mk(loose))
		if err == nil || err.Error() != "service: data_dir must not be accessible to group or others (mode 0700)" || !strings.Contains(logs, `"reason":"data_dir"`) {
			t.Errorf("mode %o: %v / %s", mode, err, logs)
		}
	}
	tight := filepath.Join(dir, "tight")
	if err := os.Mkdir(tight, 0o700); err != nil {
		t.Fatal(err)
	}
	if err, logs := run(mk(tight)); err != nil {
		t.Fatalf("0700 directory refused: %v %s", err, logs)
	}
	file := filepath.Join(dir, "file")
	os.WriteFile(file, nil, 0o600)
	if err, _ := run(mk(file)); err == nil {
		t.Fatal("a file as data_dir accepted")
	}
}

// Every startup failure logs one JSON line with a fixed reason and no value.
func TestStartupFailureIsLogged(t *testing.T) {
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	relayAddr := free.Addr().String()
	free.Close()
	m := goodConfig()
	m["data_dir"] = filepath.Join(dir, "data")
	m["nwc"] = map[string]any{"relay_url": "ws://" + relayAddr, "wallet_pubkey": goodPub, "client_key_file": keyPath, "network": "regtest"}
	m["api_keys_file"] = apiPath
	m["webhooks"] = []any{map[string]any{"name": "main", "url": "http://127.0.0.1:9/h", "secret_file": whPath}}
	cfg, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	if err := Run(context.Background(), cfg, WithLogger(slog.New(slog.NewJSONHandler(logs, nil)))); err == nil {
		t.Fatal("Run succeeded")
	}
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		if m["msg"] == logStartFailed {
			lines = append(lines, m)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("want one startup line, got %s", logs.String())
	}
	if r, _ := lines[0]["reason"].(string); !strings.HasPrefix(r, "wallet_") || strings.Contains(logs.String(), relayAddr) {
		t.Fatalf("bad line: %v", lines[0])
	}
}

func TestDialReason(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&nwc.RelayRefusedError{Reason: "x"}, "wallet_relay_refused"},
		{fmt.Errorf("w: %w", &nwc.RelayRefusedError{}), "wallet_relay_refused"},
		{context.DeadlineExceeded, "wallet_timeout"},
		{nwc.ErrNoResponse, "wallet_timeout"},
		{nwc.ErrClosed, "wallet_closed"},
		{context.Canceled, "wallet_closed"},
		{io.EOF, "wallet_other"},
	} {
		if got := dialReason(tc.err); got != tc.want {
			t.Errorf("%v: %s want %s", tc.err, got, tc.want)
		}
	}
}

// pressGivenUp presses pay on a new order while the wallet blocks MakeInvoice, then gives the press
// up (the browser went away): the flight goes on without a request waiting for it.
func pressGivenUp(t *testing.T, r *rig) (orderResp, chan struct{}) {
	t.Helper()
	o := r.createOrder("ref-flight")
	block := make(chan struct{})
	r.wallet.mu.Lock()
	r.wallet.block = block
	r.wallet.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { close(block) }) }
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "POST", o.CheckoutURL+"/pay", nil)
		resp, err := noRedirect.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		res <- err
	}()
	select {
	case <-r.wallet.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the press never reached the wallet")
	}
	cancel()
	<-res
	rel := make(chan struct{})
	go func() { <-rel; release() }()
	return o, rel
}

// Shutdown waits, within the grace, for a Pay flight whose presser is gone, before it closes the
// store: the invoice the flight made is stored.
func TestShutdownWaitsForPayFlights(t *testing.T) {
	r := startRig(t)
	o, release := pressGivenUp(t, r)
	stopped := make(chan error, 1)
	go func() { stopped <- r.stop() }()
	select {
	case <-stopped:
		t.Fatal("Run returned with a Pay flight in progress")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("Run: %v", err)
	}
	st, err := store.Open(context.Background(), filepath.Join(r.cfg.DataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	invs, err := st.InvoicesForOrder(context.Background(), o.ID)
	if err != nil || len(invs) != 1 {
		t.Fatalf("%d invoices stored (%v), want the flight's one", len(invs), err)
	}
	if strings.Contains(r.logs.String(), logFlightsAbandoned) {
		t.Fatal("a flight that ended was reported abandoned")
	}
}

// A flight that outlasts the grace is abandoned: shutdown stays bounded, and says so.
func TestShutdownAbandonsAStuckPayFlight(t *testing.T) {
	r := startRig(t, withGrace(400*time.Millisecond))
	pressGivenUp(t, r)
	start := time.Now()
	if err := r.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 5*time.Second {
		t.Fatalf("shutdown took %v with a 400 ms grace", d)
	}
	if !strings.Contains(r.logs.String(), logFlightsAbandoned) {
		t.Fatal("the abandoned flight was not logged")
	}
}

// --- the on-chain rail ---

const testRPCPassword = "rpc-password-for-tests-9f8e7d"

// onchainMod adds an on-chain block to the rig's configuration, with a password file in dir.
func onchainMod(rpcURL string) func(dir string, m map[string]any) {
	return func(dir string, m map[string]any) {
		pw := filepath.Join(dir, "rpc.pass")
		if err := os.WriteFile(pw, []byte(testRPCPassword+"\n"), 0o600); err != nil {
			panic(err)
		}
		o := onchainBlock()
		o["rpc_url"] = rpcURL
		o["rpc_password_file"] = pw
		o["poll_seconds"] = 2
		m["onchain"] = o
	}
}

func testNode(t *testing.T) *onchaintest.Node {
	t.Helper()
	d, err := descriptor.Parse("wpkh(" + testTpub + "/0/*)")
	if err != nil {
		t.Fatal(err)
	}
	return onchaintest.New(onchaintest.Normalise(d.Canonical), 7)
}

type mixedResp struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	CheckoutURL string `json:"checkout_url"`
	Onchain     *struct {
		State   string  `json:"state"`
		Address *string `json:"address"`
	} `json:"onchain"`
}

func (r *rig) mixed(id string) mixedResp {
	r.t.Helper()
	code, b := do(r.t, "GET", r.prvURL+"/v1/orders/"+id, testReadKey, nil)
	if code != http.StatusOK {
		r.t.Fatalf("get: %d %s", code, b)
	}
	var o mixedResp
	if err := json.Unmarshal(b, &o); err != nil {
		r.t.Fatal(err)
	}
	return o
}

func TestOnchainEndToEnd(t *testing.T) {
	node := testNode(t)
	r := startRigWith(t, onchainMod("http://127.0.0.1:18443"), WithRPC(node))
	body := `{"account_id":"acct1","merchant_ref":"mixed-1","lightning_msat":21000000,"onchain_sat":50000}`
	code, b := do(t, "POST", r.prvURL+"/v1/orders", testAPIKey, []byte(body))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	var o mixedResp
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	if o.Onchain == nil || o.Onchain.State != "waiting" || o.Onchain.Address != nil {
		t.Fatalf("a new order: %s", b)
	}
	if code, _ := do(t, "POST", o.CheckoutURL+"/pay", "", nil); code != http.StatusSeeOther {
		t.Fatalf("pay: %d", code)
	}
	got := r.mixed(o.ID)
	if got.Onchain == nil || got.Onchain.Address == nil {
		t.Fatalf("no address after the press: %+v", got)
	}
	node.Pay(*got.Onchain.Address, 50000)
	if st := r.mixed(o.ID).State; st != "new" {
		t.Fatalf("paid before it was mined: %s", st)
	}
	node.Mine(1) // the first tier is 1 confirmation
	eventually(t, "the order to be paid", func() bool { return r.mixed(o.ID).State == "paid" })
	if s := r.mixed(o.ID).Onchain.State; s != "paid" {
		t.Fatalf("onchain.state %s", s)
	}
	// One order.paid at the receiver, and no second one after more passes.
	count := func() (n int) {
		for {
			select {
			case h := <-r.hooks:
				var p struct {
					Type    string `json:"type"`
					OrderID string `json:"order_id"`
				}
				if err := json.Unmarshal(h.body, &p); err == nil && p.Type == "order.paid" && p.OrderID == o.ID {
					n++
				}
			default:
				return n
			}
		}
	}
	n := 0
	eventually(t, "the order.paid webhook", func() bool { n += count(); return n > 0 })
	node.Mine(6)
	time.Sleep(5 * time.Second) // two passes
	if n += count(); n != 1 {
		t.Fatalf("order.paid delivered %d times, want 1", n)
	}
	start := time.Now()
	if err := r.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("shutdown with the watcher running took %v", d)
	}
	// No secret, address, descriptor or order id in the logs.
	for _, bad := range []string{testRPCPassword, *got.Onchain.Address, testTpub[:20], o.ID} {
		if strings.Contains(r.logs.String(), bad) {
			t.Fatalf("logs contain a forbidden value %q", bad[:6])
		}
	}
}

// Lightning pays second: a mixed order paid on-chain, then its invoice settled. lnpay's completion calls the on-chain
// rail's LightningPaid (the wiring in Run), which logs the both-rails ALERT once; the receiver gets order.paid once.
// Not TestOnchainEndToEnd, whose log check forbids the order id that this ALERT names.
func TestBothRailsLightningSecond(t *testing.T) {
	node := testNode(t)
	r := startRigWith(t, onchainMod("http://127.0.0.1:18443"), WithRPC(node))
	body := `{"account_id":"acct1","merchant_ref":"mixed-both","lightning_msat":21000000,"onchain_sat":50000}`
	code, b := do(t, "POST", r.prvURL+"/v1/orders", testAPIKey, []byte(body))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	var o mixedResp
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, "POST", o.CheckoutURL+"/pay", "", nil); code != http.StatusSeeOther {
		t.Fatalf("pay: %d", code)
	}
	got := r.mixed(o.ID)
	if got.Onchain == nil || got.Onchain.Address == nil {
		t.Fatalf("no address after the press: %+v", got)
	}
	node.Pay(*got.Onchain.Address, 50000)
	node.Mine(1)
	eventually(t, "the order to be paid on-chain", func() bool { return r.mixed(o.ID).Onchain.State == "paid" })
	const alert = `"msg":"ALERT: an order was paid on both rails"`
	if strings.Contains(r.logs.String(), alert) {
		t.Fatalf("the ALERT before Lightning paid:\n%s", r.logs)
	}
	if hashes := r.wallet.settleAll(); len(hashes) != 1 {
		t.Fatalf("want one invoice, got %d", len(hashes))
	}
	eventually(t, "the both-rails ALERT", func() bool { return strings.Contains(r.logs.String(), alert) })
	count := func() (n int) {
		for {
			select {
			case h := <-r.hooks:
				var p struct {
					Type    string `json:"type"`
					OrderID string `json:"order_id"`
				}
				if err := json.Unmarshal(h.body, &p); err == nil && p.Type == "order.paid" && p.OrderID == o.ID {
					n++
				}
			default:
				return n
			}
		}
	}
	n := 0
	eventually(t, "the order.paid webhook", func() bool { n += count(); return n > 0 })
	// Two more watcher passes over the order, each seen when the API reads the next block's confirmation: neither
	// repeats the ALERT or order.paid.
	confs := func() int {
		code, b := do(t, "GET", r.prvURL+"/v1/orders/"+o.ID, testReadKey, nil)
		var v struct {
			Onchain *struct {
				Confirmations int `json:"confirmations"`
			} `json:"onchain"`
		}
		if code != http.StatusOK || json.Unmarshal(b, &v) != nil || v.Onchain == nil {
			t.Fatalf("get: %d %s", code, b)
		}
		return v.Onchain.Confirmations
	}
	for want := 2; want <= 3; want++ {
		node.Mine(1)
		eventually(t, "a pass after the next block", func() bool { return confs() >= want })
	}
	if n += count(); n != 1 {
		t.Fatalf("order.paid delivered %d times, want 1", n)
	}
	if err := r.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	logs := r.logs.String()
	if c := strings.Count(logs, alert); c != 1 {
		t.Fatalf("the ALERT %d times, want 1:\n%s", c, logs)
	}
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, alert) && !strings.Contains(line, `"order":"`+o.ID+`"`) {
			t.Fatalf("the ALERT does not name the order: %s", line)
		}
	}
	for _, bad := range []string{testRPCPassword, *got.Onchain.Address, testTpub[:20]} {
		if strings.Contains(logs, bad) {
			t.Fatalf("logs contain a forbidden value %q", bad[:6])
		}
	}
}

// Without an on-chain block the service has no rail: the resolver's Oc is a plain nil, and an order with an
// on-chain amount is refused.
func TestNoOnchainBlock(t *testing.T) {
	if res := newResolver(nil, nil, nil, nil, time.Now, slog.Default()); res.HasOnchain() || res.Oc != nil || res.HasMonero() || res.Xm != nil {
		t.Fatal("a nil *onchain.Service or *monero.Service made a non-nil resolver Oc or Xm (typed nil)")
	}
	r := startRig(t)
	body := `{"account_id":"acct1","merchant_ref":"mixed-2","lightning_msat":21000000,"onchain_sat":50000}`
	code, b := do(t, "POST", r.prvURL+"/v1/orders", testAPIKey, []byte(body))
	if code < 400 || !strings.Contains(string(b), "onchain_disabled") {
		t.Fatalf("create with onchain_sat and no rail: %d %s", code, b)
	}
}

func onchainStartConfig(t *testing.T, rpcURL string, mod func(o map[string]any)) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	m := goodConfig()
	m["data_dir"] = filepath.Join(dir, "data")
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	m["webhooks"] = []any{map[string]any{"name": "main", "url": "http://127.0.0.1:9/h", "secret_file": whPath}}
	onchainMod(rpcURL)(dir, m)
	if mod != nil {
		mod(m["onchain"].(map[string]any))
	}
	cfg, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

func reasonOf(t *testing.T, logs string) string {
	t.Helper()
	for _, l := range strings.Split(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == logStartFailed {
			r, _ := m["reason"].(string)
			return r
		}
	}
	return ""
}

// Ruling I: a bitcoind that fails the startup check is a startup failure, inside the dial bound, with the reason
// "onchain" and nothing of bitcoind's answer.
func TestOnchainCheckFailureIsFatal(t *testing.T) {
	node := testNode(t)
	node.Update(func(s *onchaintest.State) { s.PrivateKeysEnabled = true })
	cfg, _ := onchainStartConfig(t, "http://127.0.0.1:18443", nil)
	logs := &syncBuf{}
	start := time.Now()
	err := Run(context.Background(), cfg, WithWallet(newFakeWallet()), WithRPC(node), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	if err == nil || time.Since(start) > dialBound {
		t.Fatalf("Run: %v after %v", err, time.Since(start))
	}
	if got := reasonOf(t, logs.String()); got != "onchain" {
		t.Fatalf("reason %q, logs %s", got, logs.String())
	}
}

// The real client against a port where nothing listens: connection refused is a fast startup failure.
func TestOnchainUnreachableBitcoindIsFatal(t *testing.T) {
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := free.Addr().String()
	free.Close()
	cfg, _ := onchainStartConfig(t, "http://"+addr, nil)
	logs := &syncBuf{}
	start := time.Now()
	err := Run(context.Background(), cfg, WithWallet(newFakeWallet()), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	if err == nil || time.Since(start) > dialBound {
		t.Fatalf("Run: %v after %v", err, time.Since(start))
	}
	if got := reasonOf(t, logs.String()); got != "onchain" {
		t.Fatalf("reason %q, logs %s", got, logs.String())
	}
	for _, bad := range []string{testRPCPassword, addr} {
		if strings.Contains(logs.String(), bad) || strings.Contains(err.Error(), bad) {
			t.Fatal("a secret or an address reached the error or the log")
		}
	}
}

func TestOnchainPasswordFileRefusals(t *testing.T) {
	for name, mod := range map[string]func(path string){
		"missing": func(p string) { os.Remove(p) },
		"loose":   func(p string) { os.Chmod(p, 0o644) },
		"symlink": func(p string) {
			real := p + ".real"
			os.Rename(p, real)
			if err := os.Symlink(real, p); err != nil {
				panic(err)
			}
		},
	} {
		cfg, _ := onchainStartConfig(t, "http://127.0.0.1:9", nil)
		mod(cfg.Onchain.RPCPasswordFile)
		logs := &syncBuf{}
		if err := Run(context.Background(), cfg, WithWallet(newFakeWallet()), WithLogger(slog.New(slog.NewJSONHandler(logs, nil)))); err == nil {
			t.Fatalf("%s: Run succeeded", name)
		}
		if got := reasonOf(t, logs.String()); got != "rpc_password_file" {
			t.Fatalf("%s: reason %q", name, got)
		}
	}
}

// One worker more than workerCount is refused (the fatal channel holds workerCount errors), and a Run with every worker on starts
// without tripping that guard: the number Run starts is what workerCount must cover.
func TestWorkerCountCoversRun(t *testing.T) {
	_, worker, _ := newWorkers(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	for i := 0; i < workerCount; i++ {
		worker("w", func() error { return nil }, func(error) bool { return true })
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a worker beyond workerCount was accepted")
			}
		}()
		worker("w", func() error { return nil }, func(error) bool { return true })
	}()
	// Every worker on, the rate fetcher, the Monero watcher, the alert deliverer and eight webhook deliverers included:
	// newWorkers panics if Run starts more than workerCount.
	w := monerotest.New(7)
	var sinks []*hookSink
	for i := 0; i < MaxWebhookEndpoints; i++ {
		sinks = append(sinks, newSink(t, fmt.Sprintf("ep%d", i)))
	}
	all := mods(webhooksMod(t, sinks...), onchainMod("http://127.0.0.1:18443"), ratesMod(nil), moneroMod(w.Primary()), payoutRig(), alertsMod("http://127.0.0.1:9/alerts"))
	startRigWith(t, all, WithRPC(testNode(t)), WithWalletRPC(w), WithRateTransport(rateFake(t).Transport()), WithPayoutWallet(newPayoutWallet()))
	if workerCount != 16 {
		t.Fatalf("workerCount %d, want 16: Lightning, two listeners, the rate fetcher, the two watchers, the refund service and the alert deliverer (8), and one deliverer for each of MaxWebhookEndpoints (8) endpoints", workerCount)
	}
}

// Every worker failing at once, with nobody reading the channel yet, all return: none blocks on the send.
func TestAllWorkersFailingDoNotBlock(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	workers, worker, fatal := newWorkers(log)
	for i := 0; i < workerCount; i++ {
		worker("boom", func() error { return errors.New("x") }, func(error) bool { return false })
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a failing worker blocked on the fatal channel")
	}
	if len(fatal) != workerCount {
		t.Fatalf("%d fatal errors, want %d", len(fatal), workerCount)
	}
}

// Both listeners dying while the watcher runs ends the service with an error, and the shutdown finishes in time.
func TestOnchainWorkersFailingShutDown(t *testing.T) {
	node := testNode(t)
	r := startRigWith(t, onchainMod("http://127.0.0.1:18443"), WithRPC(node))
	r.pubLn.Close()
	r.prvLn.Close()
	start := time.Now()
	select {
	case err := <-r.done:
		r.doneErr, r.doneAlive = err, true
		r.stopOnce.Do(func() {})
		if err == nil || time.Since(start) > 10*time.Second {
			t.Fatalf("Run: %v after %v", err, time.Since(start))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
}

type failingRail struct{ status.Onchain }

func (failingRail) Rail(context.Context, store.Order) (onchain.Rail, error) {
	return onchain.Rail{}, errors.New("secret-detail 192.0.2.1")
}

func TestLoggedRailLogsOneFixedLine(t *testing.T) {
	logs := &syncBuf{}
	l := loggedRail{failingRail{}, slog.New(slog.NewJSONHandler(logs, nil))}
	if _, err := l.Rail(context.Background(), store.Order{ID: "order-id-1", AmountSat: 1}); err == nil {
		t.Fatal("error swallowed")
	}
	out := strings.TrimSpace(logs.String())
	if strings.Count(out, "\n") != 0 || !strings.Contains(out, logRailFailed) || strings.Contains(out, "secret-detail") || strings.Contains(out, "order-id-1") {
		t.Fatalf("log: %q", out)
	}
	logs2 := &syncBuf{}
	l = loggedRail{failingRail{}, slog.New(slog.NewJSONHandler(logs2, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l.Onchain = ctxRail{}
	l.Rail(ctx, store.Order{})
	if logs2.String() != "" {
		t.Fatalf("a cancelled request was logged: %s", logs2.String())
	}
}

type ctxRail struct{ status.Onchain }

func (ctxRail) Rail(context.Context, store.Order) (onchain.Rail, error) {
	return onchain.Rail{}, context.Canceled
}

// Without an on-chain block nothing settles an order with an on-chain amount, so the service refuses to
// start while the store holds one inside its watch window without a final event; the operator restores the block. With
// that order final, or out of its window, it starts.
func TestNoOnchainBlockWithOpenOnchainOrdersIsRefused(t *testing.T) {
	node := testNode(t)
	r := startRigWith(t, onchainMod("http://127.0.0.1:18443"), WithRPC(node))
	body := `{"account_id":"acct1","merchant_ref":"chain-1","onchain_sat":50000}`
	code, b := do(t, "POST", r.prvURL+"/v1/orders", testAPIKey, []byte(body))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	var o mixedResp
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	dataDir := r.cfg.DataDir
	cfg := *r.cfg
	cfg.Onchain = nil
	run := func(now time.Time) (error, string) {
		logs := &syncBuf{}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		pub, _ := net.Listen("tcp", "127.0.0.1:0")
		prv, _ := net.Listen("tcp", "127.0.0.1:0")
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, &cfg, WithWallet(newFakeWallet()), WithListeners(pub, prv), WithClock(func() time.Time { return now }), withDeliveryFallback(shortFallback),
				WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
		}()
		for {
			select {
			case err := <-done:
				return err, logs.String()
			case <-time.After(50 * time.Millisecond):
				if strings.Contains(logs.String(), logStarted) {
					cancel()
					return <-done, logs.String()
				}
			}
		}
	}
	err, logs := run(time.Now())
	if err == nil || err.Error() != "service: "+msgOpenOnchain || reasonOf(t, logs) != "onchain" {
		t.Fatalf("started with an open on-chain order and no onchain block: %v\n%s", err, logs)
	}
	if strings.Contains(logs, o.ID) || strings.Contains(err.Error(), o.ID) {
		t.Fatal("the refusal names the order")
	}
	// Out of its watch window: it starts.
	if err, logs := run(time.Now().Add(store.WatchWindow + 2*time.Hour)); !strings.Contains(logs, logStarted) {
		t.Fatalf("refused past the window: %v\n%s", err, logs)
	}
	// With a final event: it starts.
	st, err := store.Open(context.Background(), filepath.Join(dataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddEvent(context.Background(), store.Event{Type: store.EventExpired, OrderID: o.ID}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err, logs := run(time.Now()); !strings.Contains(logs, logStarted) {
		t.Fatalf("refused with the order final: %v\n%s", err, logs)
	}
	// An on-time payment still unconfirmed at the window end stays watched until it is settled, whatever the order's
	// events: the guard refuses past the window, also after order.underpaid, and lets go once the output is settled.
	pastWindow := time.Now().Add(store.WatchWindow + 2*time.Hour)
	if st, err = store.Open(context.Background(), filepath.Join(dataDir, "store.db")); err != nil {
		t.Fatal(err)
	}
	idx, _, err := st.MaxAddressIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stuckAddr := "bcrt1q" + strings.Repeat("s", 38)
	if err := st.AddAddress(context.Background(), store.Address{OrderID: o.ID, Address: stuckAddr, Index: idx + 1}); err != nil && !errors.Is(err, store.ErrAddressTaken) {
		t.Fatal(err)
	}
	a, err := st.AddressForOrder(context.Background(), o.ID)
	if err != nil {
		t.Fatal(err)
	}
	stuck := strings.Repeat("5", 64)
	if _, err := st.RecordScan(context.Background(), []store.TXO{{TxID: stuck, Address: a.Address, AmountSat: 50_000}},
		store.Cursor{BlockHash: strings.Repeat("6", 64), Height: 1}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	refused := func(name string) {
		t.Helper()
		err, logs := run(pastWindow)
		if err == nil || err.Error() != "service: "+msgOpenOnchain || reasonOf(t, logs) != "onchain" {
			t.Fatalf("%s: started with a stuck on-chain payment and no onchain block: %v\n%s", name, err, logs)
		}
	}
	refused("a stuck output past the window")
	if st, err = store.Open(context.Background(), filepath.Join(dataDir, "store.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddEvent(context.Background(), store.Event{Type: store.EventUnderpaid, OrderID: o.ID}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	refused("after order.underpaid")
	if st, err = store.Open(context.Background(), filepath.Join(dataDir, "store.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSettled(context.Background(), store.TXOKey{TxID: stuck}, 7); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err, logs := run(pastWindow); !strings.Contains(logs, logStarted) {
		t.Fatalf("refused with the stuck output settled: %v\n%s", err, logs)
	}
}

// testRateToken is in the query of the tests' custom rate URL: it must never reach a log or an error.
const testRateToken = "rate-token-for-tests-5a4b3c"

// testCustomURL is the tests' custom rate URL: the fake's custom host, with the token in its query.
const testCustomURL = "https://rates.example.invalid/satbooth?token=" + testRateToken

// ratesMod adds the example rates block, its custom URL in a 0600 rates.custom_url_file, changed by mod.
func ratesMod(mod func(r map[string]any)) func(dir string, m map[string]any) {
	return ratesModURL(testCustomURL, mod)
}

// ratesModURL is ratesMod with another URL in the custom URL file.
func ratesModURL(u string, mod func(r map[string]any)) func(dir string, m map[string]any) {
	return func(dir string, m map[string]any) {
		p := filepath.Join(dir, "rates.url")
		if err := os.WriteFile(p, []byte(u+"\n"), 0o600); err != nil {
			panic(err)
		}
		r := ratesBlock()
		delete(r, "custom_url")
		r["custom_url_file"] = p
		if mod != nil {
			mod(r)
		}
		m["rates"] = r
	}
}

// rateFake is the fake rate server with the four providers at 76000, 76050, 75950 and 76025 in USD and EUR: their
// median (the lower mean of the middle two) is 76012.5, and the spread is inside 2 %.
func rateFake(t *testing.T) *ratestest.Server {
	fake := ratestest.New(t)
	for p, v := range map[string]string{"kraken": "76000", "coinbase": "76050", "yadio": "75950", "custom": "76025"} {
		fake.Set(p, "USD", v)
		fake.Set(p, "EUR", v)
	}
	return fake
}

type pricedResp struct {
	ID         string `json:"id"`
	AmountMsat int64  `json:"lightning_msat"`
	AmountSat  int64  `json:"onchain_sat"`
	Pricing    *struct {
		Price         string `json:"price"`
		Currency      string `json:"currency"`
		MarginPercent string `json:"margin_percent"`
		Rates         map[string]struct {
			Rate    string `json:"rate"`
			Sources []struct {
				Provider string `json:"provider"`
				Rate     string `json:"rate"`
			} `json:"sources"`
		} `json:"rates"`
	} `json:"pricing"`
}

const pricedBody = `{"account_id":"acct1","merchant_ref":"priced-1","price":"10.00","currency":"USD"}`

func TestRunPricesAnOrder(t *testing.T) {
	t.Run("four sources", func(t *testing.T) { runPricesAnOrder(t, rateFake(t), 4) })
	// The three-source path, made certain: the custom API down, so the quote records three sources and the price is
	// their median (76000), not the four's.
	t.Run("three sources", func(t *testing.T) {
		fake := rateFake(t)
		fake.Fail("custom", http.StatusServiceUnavailable)
		runPricesAnOrder(t, fake, 3)
	})
}

// runPricesAnOrder prices an order over fake; wantSources is the number of sources the quote must record (orders priced
// on fewer, before the rest reached the cache, are passed over).
func runPricesAnOrder(t *testing.T, fake *ratestest.Server, wantSources int) {
	r := startRigWith(t, ratesMod(nil), WithRateTransport(fake.Transport()))
	// The first fetch runs in the background: until it lands, a priced order is 503. With wantSources set, an order
	// priced before that many sources reached the cache is passed over and another one made (a new merchant_ref).
	var o, g pricedResp
	deadline := time.Now().Add(10 * time.Second)
	for i := 1; ; i++ {
		if i > 1 {
			time.Sleep(20 * time.Millisecond)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no price with the wanted sources within 10 s")
		}
		body := strings.Replace(pricedBody, `"priced-1"`, fmt.Sprintf(`"priced-%d"`, i), 1)
		code, b := do(t, "POST", r.prvURL+"/v1/orders", testAPIKey, []byte(body))
		if code == http.StatusServiceUnavailable {
			continue
		}
		if code != http.StatusCreated {
			t.Fatalf("priced create: %d %s", code, b)
		}
		o = pricedResp{}
		if err := json.Unmarshal(b, &o); err != nil {
			t.Fatal(err)
		}
		code, b = do(t, "GET", r.prvURL+"/v1/orders/"+o.ID, testReadKey, nil)
		g = pricedResp{}
		if code != http.StatusOK || json.Unmarshal(b, &g) != nil || g.Pricing == nil {
			t.Fatalf("get: %d %s", code, b)
		}
		if len(g.Pricing.Rates["BTC"].Sources) >= wantSources {
			break
		}
	}
	p := g.Pricing
	if p.Price != "10.00" || p.Currency != "USD" || len(p.Rates) != 1 || p.MarginPercent != "1" {
		t.Fatalf("pricing %+v", *p)
	}
	// The order is priced as soon as min_sources (2) of the four sources are in the cache, which under load can be before
	// all four are: the rate is the median of the sources the quote records, each one of the fake's values.
	fakeRates := map[string]string{"kraken": "76000", "coinbase": "76050", "yadio": "75950", "custom": "76025"}
	btc := p.Rates["BTC"]
	if len(btc.Sources) != wantSources {
		t.Fatalf("%d sources, want %d", len(btc.Sources), wantSources)
	}
	var recorded []rates.Rate
	now := time.Now()
	for _, s := range btc.Sources {
		if fakeRates[s.Provider] == "" || s.Rate != fakeRates[s.Provider] {
			t.Fatalf("source %s at %s", s.Provider, s.Rate)
		}
		v, _ := rates.ParseDecimal(s.Rate)
		recorded = append(recorded, rates.Rate{Provider: s.Provider, Currency: "USD", Value: v, At: now})
	}
	q, err := rates.Aggregate(rates.AssetBTC, "USD", recorded, now, rates.Policy{MinSources: 2, MaxSpreadBps: 200, MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if btc.Rate != q.Rate.String() {
		t.Fatalf("rate %s, want the median of the recorded sources %s", btc.Rate, q.Rate.String())
	}
	if wantSources == 4 && btc.Rate != "76012.5" {
		t.Fatalf("rate %s of the four sources, want 76012.5", btc.Rate)
	}
	if wantSources == 3 && btc.Rate != "76000" {
		t.Fatalf("rate %s of the three built-ins, want 76000", btc.Rate)
	}
	price, _ := rates.ParseDecimal("10.00")
	sats, err := rates.Sats(price, q.Rate, 100)
	if err != nil {
		t.Fatal(err)
	}
	if o.AmountMsat != sats*1000 || o.AmountSat != 0 {
		t.Fatalf("lightning_msat %d onchain_sat %d, want %d and 0", o.AmountMsat, o.AmountSat, sats*1000)
	}
	r.createOrder("explicit-1") // explicit amounts still work beside fiat pricing
	if strings.Contains(r.logs.String(), testRateToken) {
		t.Fatal("the custom URL's token reached the log")
	}
}

func TestRunWithoutRatesBlock(t *testing.T) {
	r := startRig(t)
	code, b := do(t, "POST", r.prvURL+"/v1/orders", testAPIKey, []byte(pricedBody))
	if code != http.StatusBadRequest || !strings.Contains(string(b), `"fiat_disabled"`) {
		t.Fatalf("priced create without the rates block: %d %s", code, b)
	}
	r.createOrder("explicit-1")
}

// Every provider failing from the start: the service starts anyway, explicit orders work, and priced ones are 503 at
// once and stay 503 across fetch passes. No provider body and no token reaches the log.
func TestRunWithProvidersDown(t *testing.T) {
	fake := rateFake(t)
	all := []string{"kraken", "coinbase", "yadio", "custom"}
	for _, p := range all {
		fake.Fail(p, http.StatusServiceUnavailable)
	}
	start := time.Now()
	r := startRigWith(t, ratesMod(func(rb map[string]any) { rb["fetch_seconds"] = 10 }), WithRateTransport(fake.Transport()))
	r.createOrder("explicit-1")
	unavailable := func() {
		t.Helper()
		code, b := do(t, "POST", r.prvURL+"/v1/orders", testAPIKey, []byte(pricedBody))
		if code != http.StatusServiceUnavailable || !strings.Contains(string(b), `"rate_unavailable"`) {
			t.Fatalf("priced create with every provider down: %d %s", code, b)
		}
	}
	unavailable()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the first priced answer came %v after the start", d)
	}
	// Three passes: the first at once, then two ticks of 10 s.
	for deadline := time.Now().Add(40 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		done := true
		for _, p := range all {
			done = done && fake.Hits(p) >= 3
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fewer than three passes in 40 s")
		}
	}
	unavailable()
	logs := r.logs.String()
	if !strings.Contains(logs, "rate fetch failed") {
		t.Fatalf("no fetch failure was logged: %s", logs)
	}
	for _, bad := range []string{ratestest.Sentinel, testRateToken} {
		if strings.Contains(logs, bad) {
			t.Fatalf("%q reached the log", bad)
		}
	}
}

// A custom URL at a host the fake does not serve: the fake's re-router refuses the dial, and http.Client wraps that error
// in a *url.Error whose text holds the whole URL, token included. None of it reaches the log.
func TestRunCustomURLNeverLogged(t *testing.T) {
	fake := rateFake(t)
	r := startRigWith(t, ratesModURL("https://unserved.example.invalid/satbooth?token="+testRateToken, nil), WithRateTransport(fake.Transport()))
	eventually(t, "the custom fetch failure", func() bool {
		for _, l := range strings.Split(r.logs.String(), "\n") {
			if strings.Contains(l, `"rate fetch failed"`) && strings.Contains(l, `"provider":"custom"`) && strings.Contains(l, `"class":"transport"`) {
				return true
			}
		}
		return false
	})
	for _, bad := range []string{testRateToken, "unserved", "satbooth?"} {
		if strings.Contains(r.logs.String(), bad) {
			t.Fatalf("%q reached the log", bad)
		}
	}
}

// A rates block that never went through ParseConfig (a Config built by struct literal, as qualify builds one, or changed
// after parsing) is validated by Run: each bad block is a startup failure with the reason "rates", never a fetcher that
// lets the custom API price alone, a margin silently 0, or a panic on an unknown provider.
func TestRatesStartFailure(t *testing.T) {
	good := func() *RatesConfig {
		return &RatesConfig{Providers: []string{"kraken", "coinbase"}, CustomURL: "https://rates.example.invalid/satbooth",
			Currencies: []string{"USD"}, MarginPercent: "1", MinSources: 2, MaxSpreadPercent: "2", MaxAgeSeconds: 300, FetchSeconds: 60}
	}
	cases := map[string]func(r *RatesConfig){
		"fetch above max_age / 2":    func(r *RatesConfig) { r.FetchSeconds = r.MaxAgeSeconds },
		"custom_url alone, min 1":    func(r *RatesConfig) { r.Providers = nil; r.MinSources = 1 },
		"an unknown provider":        func(r *RatesConfig) { r.Providers = []string{"bitstamp", "kraken"} },
		"a malformed margin":         func(r *RatesConfig) { r.MarginPercent = "abc" },
		"an empty margin":            func(r *RatesConfig) { r.MarginPercent = "" },
		"min_sources 0 (no default)": func(r *RatesConfig) { r.MinSources = 0 },
		"a http custom_url":          func(r *RatesConfig) { r.CustomURL = "http://rates.example.invalid/satbooth" },
		"a custom_url with a query":  func(r *RatesConfig) { r.CustomURL = testCustomURL },
		"custom_url and its file":    func(r *RatesConfig) { r.CustomURLFile = "/nonexistent/rates.url" },
	}
	// The good literal starts: the failures below are the changes, not the base.
	if err := good().validate(); err != nil {
		t.Fatalf("the base block: %v", err)
	}
	fake := rateFake(t)
	for name, mod := range cases {
		cfg, _ := onchainStartConfig(t, "http://127.0.0.1:9", nil)
		cfg.Onchain = nil
		cfg.Rates = good()
		mod(cfg.Rates)
		logs := &syncBuf{}
		err := Run(context.Background(), cfg, WithWallet(newFakeWallet()), WithRateTransport(fake.Transport()),
			WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
		if err == nil {
			t.Fatalf("%s: Run succeeded", name)
		}
		if got := reasonOf(t, logs.String()); got != "rates" {
			t.Fatalf("%s: reason %q, logs %s", name, got, logs.String())
		}
		hits := 0
		for _, p := range []string{"kraken", "coinbase", "yadio", "custom"} {
			hits += fake.Hits(p)
		}
		if hits != 0 {
			t.Fatalf("%s: a provider was asked before the block was refused", name)
		}
	}
}

// The custom URL file is loaded by Run in-process: the custom provider is asked at the URL it holds, token included,
// and the token never reaches the log. A file Run cannot load is a startup failure with the reason custom_url_file,
// before any provider is asked, and neither the token nor the path is logged.
func TestRunCustomURLFile(t *testing.T) {
	fake := rateFake(t)
	r := startRigWith(t, ratesMod(nil), WithRateTransport(fake.Transport()))
	eventually(t, "the custom provider asked", func() bool { return fake.Hits("custom") > 0 })
	if reqs := fake.Requests("custom"); len(reqs) == 0 || !strings.Contains(reqs[0], "token="+testRateToken) {
		t.Fatalf("the custom provider was asked %q, want the file's query", reqs)
	}
	if strings.Contains(r.logs.String(), testRateToken) {
		t.Fatal("the token reached the log")
	}

	for name, write := range map[string]func(p string){
		"mode 0644": func(p string) { os.WriteFile(p, []byte(testCustomURL+"\n"), 0o644); os.Chmod(p, 0o644) },
		"empty":     func(p string) { os.WriteFile(p, nil, 0o600) },
		"http":      func(p string) { os.WriteFile(p, []byte("http://rates.example.invalid/?token="+testRateToken), 0o600) },
		"missing":   func(p string) {},
	} {
		fake := rateFake(t)
		cfg, dir := onchainStartConfig(t, "http://127.0.0.1:9", nil)
		cfg.Onchain = nil
		p := filepath.Join(dir, "rates-"+strings.ReplaceAll(name, " ", "-")+".url")
		write(p)
		cfg.Rates = &RatesConfig{Providers: []string{"kraken", "coinbase"}, CustomURLFile: p, Currencies: []string{"USD"},
			MarginPercent: "1", MinSources: 2, MaxSpreadPercent: "2", MaxAgeSeconds: 300, FetchSeconds: 60}
		logs := &syncBuf{}
		err := Run(context.Background(), cfg, WithWallet(newFakeWallet()), WithRateTransport(fake.Transport()),
			WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
		if err == nil {
			t.Fatalf("%s: Run succeeded", name)
		}
		if got := reasonOf(t, logs.String()); got != "custom_url_file" {
			t.Fatalf("%s: reason %q, logs %s", name, got, logs.String())
		}
		for _, p := range []string{"kraken", "coinbase", "yadio", "custom"} {
			if fake.Hits(p) != 0 {
				t.Fatalf("%s: %s was asked before the file was refused", name, p)
			}
		}
		for _, bad := range []string{testRateToken, dir, "example.invalid"} {
			if strings.Contains(logs.String(), bad) || strings.Contains(err.Error(), bad) {
				t.Fatalf("%s: %q reached the log or the error", name, bad)
			}
		}
	}
}

// --- the Monero rail ---

// testWalletPassword is the wallet-rpc password in the tests' 0600 file: it must never reach a log or an error.
const testWalletPassword = "wallet-rpc-password-for-tests-1a2b3c"

// mods applies each change in turn.
func mods(fs ...func(dir string, m map[string]any)) func(dir string, m map[string]any) {
	return func(dir string, m map[string]any) {
		for _, f := range fs {
			f(dir, m)
		}
	}
}

// moneroMod adds a monero block for the wallet whose primary address is addr, with a password file in dir.
func moneroMod(addr string) func(dir string, m map[string]any) {
	return func(dir string, m map[string]any) {
		pw := filepath.Join(dir, "wallet-rpc.pass")
		if err := os.WriteFile(pw, []byte(testWalletPassword+"\n"), 0o600); err != nil {
			panic(err)
		}
		x := moneroBlock()
		x["address"] = addr
		x["rpc_password_file"] = pw
		x["poll_seconds"] = 2
		m["monero"] = x
	}
}

type railResp struct {
	State         string  `json:"state"`
	Address       *string `json:"address"`
	Confirmations int     `json:"confirmations"`
}

type xmrResp struct {
	ID          string    `json:"id"`
	State       string    `json:"state"`
	CheckoutURL string    `json:"checkout_url"`
	Onchain     *railResp `json:"onchain"`
	Monero      *railResp `json:"monero"`
}

func (r *rig) xmrCreate(body string) xmrResp {
	r.t.Helper()
	code, b := do(r.t, "POST", r.prvURL+"/v1/orders", testAPIKey, []byte(body))
	if code != http.StatusCreated {
		r.t.Fatalf("create: %d %s", code, b)
	}
	var o xmrResp
	if err := json.Unmarshal(b, &o); err != nil {
		r.t.Fatal(err)
	}
	return o
}

func (r *rig) xmr(id string) xmrResp {
	r.t.Helper()
	code, b := do(r.t, "GET", r.prvURL+"/v1/orders/"+id, testReadKey, nil)
	if code != http.StatusOK {
		r.t.Fatalf("get: %d %s", code, b)
	}
	var o xmrResp
	if err := json.Unmarshal(b, &o); err != nil {
		r.t.Fatal(err)
	}
	return o
}

// press posts the checkout form naming rail, as the page's picker does, and returns the redirect's target.
func press(t *testing.T, checkoutURL, rail string) string {
	t.Helper()
	req, err := http.NewRequest("POST", checkoutURL+"/pay", strings.NewReader("rail="+rail))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("press rail=%s: %d", rail, resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

// paidHooks counts the order.paid deliveries for id waiting at the receiver.
func (r *rig) paidHooks(id string) (n int) {
	for {
		select {
		case h := <-r.hooks:
			var p struct {
				Type    string `json:"type"`
				OrderID string `json:"order_id"`
			}
			if err := json.Unmarshal(h.body, &p); err == nil && p.Type == "order.paid" && p.OrderID == id {
				n++
			}
		default:
			return n
		}
	}
}

const xmrHalf = 500_000_000_000 // 0.5 XMR: the first tier, 1 confirmation

func TestRunPaysMonero(t *testing.T) {
	w := monerotest.New(7)
	node := testNode(t)
	r := startRigWith(t, mods(onchainMod("http://127.0.0.1:18443"), moneroMod(w.Primary())), WithRPC(node), WithWalletRPC(w))

	// A Monero-only explicit order: the picker on the public page offers Monero, the press names it.
	o := r.xmrCreate(fmt.Sprintf(`{"account_id":"acct1","merchant_ref":"xmr-1","monero_piconero":%d}`, xmrHalf))
	if o.Monero == nil || o.Monero.State != "waiting" || o.Monero.Address != nil || o.Onchain != nil {
		t.Fatalf("a new Monero order: %+v", o)
	}
	code, page := do(t, "GET", o.CheckoutURL, "", nil)
	if code != http.StatusOK || !strings.Contains(string(page), `name="rail" value="monero"`) {
		t.Fatalf("page: %d, no Monero form:\n%s", code, page)
	}
	if to := press(t, o.CheckoutURL, "monero"); strings.Contains(to, "busy") {
		t.Fatalf("press: %s", to)
	}
	got := r.xmr(o.ID)
	if got.Monero == nil || got.Monero.Address == nil || !strings.HasPrefix(*got.Monero.Address, "8") {
		t.Fatalf("no subaddress after the press: %+v", got)
	}
	sub := *got.Monero.Address
	w.Pay(sub, xmrHalf)
	w.Mine(1)
	eventually(t, "the Monero order to be paid", func() bool { return r.xmr(o.ID).State == "paid" })
	if s := r.xmr(o.ID).Monero.State; s != "paid" {
		t.Fatalf("monero.state %s", s)
	}
	n := 0
	eventually(t, "the order.paid webhook", func() bool { n += r.paidHooks(o.ID); return n > 0 })
	// Two more passes over the order, each seen when the API reads the next block's confirmation: no second order.paid.
	for want := 2; want <= 3; want++ {
		w.Mine(1)
		eventually(t, "a pass after the next block", func() bool { return r.xmr(o.ID).Monero.Confirmations >= want })
	}
	if n += r.paidHooks(o.ID); n != 1 {
		t.Fatalf("order.paid delivered %d times, want 1", n)
	}

	// A mixed on-chain + Monero order paid on Monero: the order is paid, the on-chain rail still waits; an on-chain
	// payment after it logs the both-rails ALERT, and the receiver still has one order.paid.
	m := r.xmrCreate(fmt.Sprintf(`{"account_id":"acct1","merchant_ref":"mixed-xmr","onchain_sat":50000,"monero_piconero":%d}`, xmrHalf))
	if code, page := do(t, "GET", m.CheckoutURL, "", nil); code != http.StatusOK || !strings.Contains(string(page), `name="rail" value="monero"`) ||
		!strings.Contains(string(page), `name="rail" value="bitcoin"`) {
		t.Fatalf("the mixed page has no picker: %d\n%s", code, page)
	}
	press(t, m.CheckoutURL, "bitcoin")
	press(t, m.CheckoutURL, "monero")
	gm := r.xmr(m.ID)
	if gm.Onchain == nil || gm.Onchain.Address == nil || gm.Monero == nil || gm.Monero.Address == nil {
		t.Fatalf("the mixed order after both presses: %+v", gm)
	}
	w.Pay(*gm.Monero.Address, xmrHalf)
	w.Mine(1)
	eventually(t, "the mixed order to be paid on Monero", func() bool { return r.xmr(m.ID).State == "paid" })
	gm = r.xmr(m.ID)
	if gm.Monero.State != "paid" || gm.Onchain.State != "waiting" {
		t.Fatalf("monero.state %s onchain.state %s, want paid and waiting", gm.Monero.State, gm.Onchain.State)
	}
	const alert = `"msg":"ALERT: an order was paid on both rails"`
	if strings.Contains(r.logs.String(), alert) {
		t.Fatal("the both-rails ALERT before the on-chain payment")
	}
	nm := 0
	eventually(t, "the mixed order's order.paid", func() bool { nm += r.paidHooks(m.ID); return nm > 0 })
	node.Pay(*gm.Onchain.Address, 50000)
	node.Mine(1)
	// Each rail reads the other (SetPeer both ways), and each logs the ALERT once: the on-chain rail as its payment meets
	// the Monero one, the Monero rail as it sees money on its peer.
	eventually(t, "the both-rails ALERT from both rails", func() bool { return strings.Count(r.logs.String(), alert) == 2 })
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, alert) && !strings.Contains(line, `"order":"`+m.ID+`"`) {
			t.Fatalf("the ALERT does not name the order: %s", line)
		}
	}
	if nm += r.paidHooks(m.ID); nm != 1 {
		t.Fatalf("the mixed order's order.paid delivered %d times, want 1", nm)
	}

	node.Mine(1)
	w.Mine(1)
	eventually(t, "a pass after the next block", func() bool { return r.xmr(m.ID).Onchain.Confirmations >= 2 })
	start := time.Now()
	if err := r.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("shutdown with both watchers running took %v", d)
	}
	if c := strings.Count(r.logs.String(), alert); c != 2 {
		t.Fatalf("the ALERT %d times, want 2 (once per rail)", c)
	}
	for _, bad := range []string{testWalletPassword, testRPCPassword, sub, w.Primary(), *gm.Monero.Address} {
		if strings.Contains(r.logs.String(), bad) {
			t.Fatalf("logs contain a forbidden value %q", bad[:6])
		}
	}
}

func moneroStartConfig(t *testing.T, url, addr string) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	m := goodConfig()
	m["data_dir"] = filepath.Join(dir, "data")
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	m["webhooks"] = []any{map[string]any{"name": "main", "url": "http://127.0.0.1:9/h", "secret_file": whPath}}
	moneroMod(addr)(dir, m)
	m["monero"].(map[string]any)["wallet_rpc_url"] = url
	cfg, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

// A wallet that fails the startup check is a startup failure inside the dial bound, with the reason "monero" and the
// fixed text of the check's sentinel (here: a wallet that can sign), and nothing of the password.
func TestMoneroCheckFailureIsFatal(t *testing.T) {
	w := monerotest.New(7)
	w.Full(true)
	cfg, _ := moneroStartConfig(t, "http://127.0.0.1:18083", w.Primary())
	logs := &syncBuf{}
	start := time.Now()
	err := Run(context.Background(), cfg, WithWallet(newFakeWallet()), WithWalletRPC(w), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	if err == nil || time.Since(start) > dialBound {
		t.Fatalf("Run: %v after %v", err, time.Since(start))
	}
	if got := reasonOf(t, logs.String()); got != "monero" {
		t.Fatalf("reason %q, logs %s", got, logs.String())
	}
	if !strings.Contains(logs.String(), `"cause":"`+monero.ErrNotViewOnly.Error()+`"`) || !strings.Contains(err.Error(), monero.ErrNotViewOnly.Error()) {
		t.Fatalf("the operator is not told the cause: %v\n%s", err, logs.String())
	}
	if strings.Contains(logs.String(), testWalletPassword) || strings.Contains(err.Error(), testWalletPassword) {
		t.Fatal("the password reached the log or the error")
	}
	// A failed call keeps its step's number (4: the height), and nothing of the call's error.
	w = monerotest.New(7)
	w.Fail("get_height", fmt.Errorf("%w: get_height: secret-detail 192.0.2.1", xmrrpc.ErrTransport))
	cfg, _ = moneroStartConfig(t, "http://127.0.0.1:18083", w.Primary())
	logs = &syncBuf{}
	err = Run(context.Background(), cfg, WithWallet(newFakeWallet()), WithWalletRPC(w), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	const cause = "monero: wallet-rpc is unavailable: step 4"
	if err == nil || err.Error() != "service: the Monero startup check failed: "+cause || !strings.Contains(logs.String(), `"cause":"`+cause+`"`) ||
		strings.Contains(logs.String(), "secret-detail") {
		t.Fatalf("Run: %v\n%s", err, logs.String())
	}
}

// Each of the check's sentinels is named by its own fixed text; anything else, a wrapped wallet-rpc answer included,
// by a fixed text that carries none of it.
func TestMoneroCheckCause(t *testing.T) {
	for _, e := range []error{monero.ErrNoWallet, monero.ErrLogin, monero.ErrAccount, monero.ErrRestricted, monero.ErrAddress,
		monero.ErrNotViewOnly, monero.ErrVersion, monero.ErrHeight, monero.ErrIndexBehind, monero.ErrSubaddress, monero.ErrUnavailable} {
		if got := moneroCause(fmt.Errorf("%w: step 3: secret-detail 192.0.2.1", e)); got != e.Error() {
			t.Errorf("%v: cause %q", e, got)
		}
	}
	if got := moneroCause(errors.New("secret-detail 192.0.2.1")); strings.Contains(got, "secret") || got == "" {
		t.Errorf("an unknown error: cause %q", got)
	}
}

// The real client against a port where nothing listens: a fast startup failure, the reason "monero", the cause
// "unavailable", and neither the password nor the address in the log or the error.
func TestMoneroUnreachableWalletRPCIsFatal(t *testing.T) {
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := free.Addr().String()
	free.Close()
	cfg, _ := moneroStartConfig(t, "http://"+addr, monerotest.New(7).Primary())
	logs := &syncBuf{}
	start := time.Now()
	err := Run(context.Background(), cfg, WithWallet(newFakeWallet()), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	if err == nil || time.Since(start) > dialBound {
		t.Fatalf("Run: %v after %v", err, time.Since(start))
	}
	if got := reasonOf(t, logs.String()); got != "monero" {
		t.Fatalf("reason %q, logs %s", got, logs.String())
	}
	if !strings.Contains(logs.String(), `"cause":"`+monero.ErrUnavailable.Error()+`: step 1"`) {
		t.Fatalf("no cause: %s", logs.String())
	}
	for _, bad := range []string{testWalletPassword, addr} {
		if strings.Contains(logs.String(), bad) || strings.Contains(err.Error(), bad) {
			t.Fatal("a secret or an address reached the error or the log")
		}
	}
}

func TestMoneroPasswordFileRefusals(t *testing.T) {
	for name, mod := range map[string]func(path string){
		"missing": func(p string) { os.Remove(p) },
		"loose":   func(p string) { os.Chmod(p, 0o644) },
		"empty":   func(p string) { os.WriteFile(p, nil, 0o600) },
	} {
		cfg, _ := moneroStartConfig(t, "http://127.0.0.1:9", monerotest.New(7).Primary())
		mod(cfg.Monero.RPCPasswordFile)
		logs := &syncBuf{}
		err := Run(context.Background(), cfg, WithWallet(newFakeWallet()), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
		if err == nil {
			t.Fatalf("%s: Run succeeded", name)
		}
		if got := reasonOf(t, logs.String()); got != "monero_password_file" {
			t.Fatalf("%s: reason %q", name, got)
		}
	}
}

// A monero block that never went through ParseConfig is validated by Run: a startup failure with the reason "monero"
// before wallet-rpc is asked anything.
func TestMoneroInvalidBlockAtRun(t *testing.T) {
	for name, mod := range map[string]func(x *MoneroConfig){
		"a subaddress":         func(x *MoneroConfig) { x.Address = "8" + x.Address[1:] },
		"poll_rate NaN":        func(x *MoneroConfig) { x.PollRate = math.NaN() },
		"poll_rate +Inf":       func(x *MoneroConfig) { x.PollRate = math.Inf(1) },
		"a ws URL":             func(x *MoneroConfig) { x.WalletRPCURL = "ws://127.0.0.1:1" },
		"no password file":     func(x *MoneroConfig) { x.RPCPasswordFile = "" },
		"a tier without confs": func(x *MoneroConfig) { x.Confirmations = []MoneroTierConfig{{UpToPiconero: 0}} },
	} {
		w := monerotest.New(7)
		cfg, _ := moneroStartConfig(t, "http://127.0.0.1:9", w.Primary())
		mod(cfg.Monero)
		logs := &syncBuf{}
		if err := Run(context.Background(), cfg, WithWallet(newFakeWallet()), WithWalletRPC(w), WithLogger(slog.New(slog.NewJSONHandler(logs, nil)))); err == nil {
			t.Fatalf("%s: Run succeeded", name)
		}
		if got := reasonOf(t, logs.String()); got != "monero" {
			t.Fatalf("%s: reason %q", name, got)
		}
		if c := w.Calls("get_version"); c != 0 {
			t.Fatalf("%s: wallet-rpc asked before the block was refused", name)
		}
	}
}

// Both listeners dying while both watchers and the rate fetcher run ends the service with an error, within 10 s.
func TestAllWorkersFailingShutDown(t *testing.T) {
	w := monerotest.New(7)
	r := startRigWith(t, mods(onchainMod("http://127.0.0.1:18443"), ratesMod(nil), moneroMod(w.Primary())),
		WithRPC(testNode(t)), WithWalletRPC(w), WithRateTransport(rateFake(t).Transport()))
	r.pubLn.Close()
	r.prvLn.Close()
	start := time.Now()
	select {
	case err := <-r.done:
		r.doneErr, r.doneAlive = err, true
		r.stopOnce.Do(func() {})
		if err == nil || time.Since(start) > 10*time.Second {
			t.Fatalf("Run: %v after %v", err, time.Since(start))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
}

type failingMonero struct{ status.Monero }

func (failingMonero) Rail(context.Context, store.Order) (rail.View, error) {
	return rail.View{}, errors.New("secret-detail 192.0.2.1")
}

type ctxMonero struct{ status.Monero }

func (ctxMonero) Rail(context.Context, store.Order) (rail.View, error) {
	return rail.View{}, context.Canceled
}

// A failed Monero Rail read logs one fixed line, as the on-chain one; a cancelled request none.
func TestLoggedMoneroRailLogsOneFixedLine(t *testing.T) {
	logs := &syncBuf{}
	l := loggedMoneroRail{failingMonero{}, slog.New(slog.NewJSONHandler(logs, nil))}
	if _, err := l.Rail(context.Background(), store.Order{ID: "order-id-1", AmountPiconero: 1}); err == nil {
		t.Fatal("error swallowed")
	}
	out := strings.TrimSpace(logs.String())
	if strings.Count(out, "\n") != 0 || !strings.Contains(out, logMoneroRailFailed) || strings.Contains(out, "secret-detail") || strings.Contains(out, "order-id-1") {
		t.Fatalf("log: %q", out)
	}
	logs2 := &syncBuf{}
	l = loggedMoneroRail{ctxMonero{}, slog.New(slog.NewJSONHandler(logs2, nil))}
	l.Rail(context.Background(), store.Order{})
	if logs2.String() != "" {
		t.Fatalf("a cancelled request was logged: %s", logs2.String())
	}
	// newResolver wraps the Monero rail.
	if _, ok := newResolver(nil, nil, nil, &monero.Service{}, time.Now, slog.Default()).Xm.(loggedMoneroRail); !ok {
		t.Fatal("the resolver's Xm is not wrapped")
	}
}

// Without a monero block nothing settles an order with a Monero amount, so the service refuses to start while the store
// holds one inside its watch window without a final event. Out of its window, it starts.
func TestNoMoneroBlockWithOpenMoneroOrdersIsRefused(t *testing.T) {
	w := monerotest.New(7)
	r := startRigWith(t, moneroMod(w.Primary()), WithWalletRPC(w))
	o := r.xmrCreate(fmt.Sprintf(`{"account_id":"acct1","merchant_ref":"xmr-open","monero_piconero":%d}`, xmrHalf))
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	cfg := *r.cfg
	cfg.Monero = nil
	run := func(now time.Time) (error, string) {
		logs := &syncBuf{}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		pub, _ := net.Listen("tcp", "127.0.0.1:0")
		prv, _ := net.Listen("tcp", "127.0.0.1:0")
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, &cfg, WithWallet(newFakeWallet()), WithListeners(pub, prv), WithClock(func() time.Time { return now }), withDeliveryFallback(shortFallback),
				WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
		}()
		for {
			select {
			case err := <-done:
				return err, logs.String()
			case <-time.After(50 * time.Millisecond):
				if strings.Contains(logs.String(), logStarted) {
					cancel()
					return <-done, logs.String()
				}
			}
		}
	}
	err, logs := run(time.Now())
	if err == nil || err.Error() != "service: "+msgOpenMonero || reasonOf(t, logs) != "monero" {
		t.Fatalf("started with an open Monero order and no monero block: %v\n%s", err, logs)
	}
	if strings.Contains(logs, o.ID) || strings.Contains(err.Error(), o.ID) {
		t.Fatal("the refusal names the order")
	}
	if err, logs := run(time.Now().Add(store.WatchWindow + 2*time.Hour)); !strings.Contains(logs, logStarted) {
		t.Fatalf("refused past the window: %v\n%s", err, logs)
	}
}

// Bitfinex configured is fetched; with a monero block XMR is asked for too.
func TestRatesBitfinex(t *testing.T) {
	fake := rateFake(t)
	fake.Set("bitfinex", "USD", "76010")
	fake.SetAsset("kraken", "XMR", "USD", "160")
	fake.SetAsset("bitfinex", "XMR", "USD", "161")
	w := monerotest.New(7)
	r := startRigWith(t, mods(ratesMod(func(rb map[string]any) {
		rb["providers"], rb["currencies"] = []any{"kraken", "bitfinex"}, []any{"USD"}
	}), moneroMod(w.Primary())), WithWalletRPC(w), WithRateTransport(fake.Transport()))
	eventually(t, "bitfinex asked for BTC and XMR", func() bool {
		var btc, xmr bool
		for _, q := range fake.Requests("bitfinex") {
			btc = btc || strings.Contains(q, "tBTCUSD")
			xmr = xmr || strings.Contains(q, "tXMRUSD")
		}
		return btc && xmr
	})
	if fake.Hits("bitfinex") < 1 {
		t.Fatal("bitfinex not fetched")
	}
	// A priced Monero order is quoted from the XMR rates.
	var code int
	var b []byte
	body := `{"account_id":"acct1","merchant_ref":"priced-xmr","price":"10.00","currency":"USD","rails":["monero"]}`
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if code, b = do(t, "POST", r.prvURL+"/v1/orders", testAPIKey, []byte(body)); code != http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no XMR price within 10 s: %d %s", code, b)
		}
	}
	if code != http.StatusCreated || !strings.Contains(string(b), `"XMR"`) {
		t.Fatalf("priced Monero create: %d %s", code, b)
	}
}

// Ruling 8-A: the startup reach rule is per asset. EUR with kraken and bitfinex starts with a monero block (bitfinex
// quotes XMR in USD only, so EUR is unreachable for XMR, which is no startup error); COP is still refused for BTC.
func TestRatesReachPerAsset(t *testing.T) {
	fake := rateFake(t)
	w := monerotest.New(7)
	r := startRigWith(t, mods(ratesMod(func(rb map[string]any) {
		rb["providers"], rb["currencies"] = []any{"kraken", "bitfinex"}, []any{"EUR"}
		delete(rb, "custom_url_file")
	}), moneroMod(w.Primary())), WithWalletRPC(w), WithRateTransport(fake.Transport()))
	body := `{"account_id":"acct1","merchant_ref":"priced-xmr-eur","price":"10.00","currency":"EUR","rails":["monero"]}`
	if code, b := do(t, "POST", r.prvURL+"/v1/orders", testAPIKey, []byte(body)); code != http.StatusBadRequest || !strings.Contains(string(b), "invalid_currency") {
		t.Fatalf("a Monero order in EUR: %d %s", code, b)
	}
	cfg := *r.cfg
	rc := *cfg.Rates
	rc.Currencies = []string{"COP"}
	cfg.Rates = &rc
	if err := rc.validate(); err == nil || err.Error() != msgRatesReachBoth {
		t.Fatalf("COP with kraken and bitfinex: %v", err)
	}
	if _, err := ParseConfig(withRates(t, func(rb map[string]any) {
		rb["providers"], rb["currencies"] = []any{"kraken", "bitfinex"}, []any{"COP"}
		delete(rb, "custom_url")
	})); err == nil || err.Error() != msgRatesReachBoth {
		t.Fatalf("COP with kraken and bitfinex parsed: %v", err)
	}
}

// Lightning pays second on a Lightning + Monero order paid on Monero and settled deep (one tier of 1 confirmation: the
// watcher no longer reads the order after one more block): only lnpay's completion, through the fan-out in Run, reaches
// the Monero rail (the on-chain rail is configured too, so the fan-out has both), which logs the both-rails ALERT once.
func TestBothRailsLightningSecondMonero(t *testing.T) {
	w := monerotest.New(7)
	oneTier := func(dir string, m map[string]any) {
		m["monero"].(map[string]any)["confirmations"] = []any{map[string]any{"up_to_piconero": 0, "confirmations": 1}}
	}
	r := startRigWith(t, mods(onchainMod("http://127.0.0.1:18443"), moneroMod(w.Primary()), oneTier), WithRPC(testNode(t)), WithWalletRPC(w))
	o := r.xmrCreate(fmt.Sprintf(`{"account_id":"acct1","merchant_ref":"ln-xmr","lightning_msat":21000000,"monero_piconero":%d}`, xmrHalf))
	press(t, o.CheckoutURL, "bitcoin")
	press(t, o.CheckoutURL, "monero")
	got := r.xmr(o.ID)
	if got.Monero == nil || got.Monero.Address == nil || len(r.wallet.invoices()) != 2 {
		t.Fatalf("after both presses: %+v, %d invoice values", got, len(r.wallet.invoices()))
	}
	txid := w.Pay(*got.Monero.Address, xmrHalf)
	w.Mine(1)
	eventually(t, "the order to be paid on Monero", func() bool { return r.xmr(o.ID).State == "paid" })
	n := 0
	eventually(t, "the order.paid webhook", func() bool { n += r.paidHooks(o.ID); return n > 0 })
	w.Mine(3)
	// The payment settled deep: the watcher stops reading it. Wait until its get_transfer_by_txid count holds still
	// across two passes (each pass starts with one get_height).
	if w.TxCalls(txid) == 0 {
		t.Fatal("the watcher never read the payment")
	}
	lastTx, mark := -1, 0
	eventually(t, "the watcher to drop the settled payment", func() bool {
		tx, passes := w.TxCalls(txid), w.Calls("get_height")
		if tx != lastTx {
			lastTx, mark = tx, passes
			return false
		}
		return passes >= mark+2
	})
	const alert = `"msg":"ALERT: an order was paid on both rails"`
	if strings.Contains(r.logs.String(), alert) {
		t.Fatal("the ALERT before Lightning paid")
	}
	if hashes := r.wallet.settleAll(); len(hashes) != 1 {
		t.Fatalf("want one invoice, got %d", len(hashes))
	}
	eventually(t, "the both-rails ALERT", func() bool { return strings.Contains(r.logs.String(), alert) })
	after := w.Calls("get_height")
	eventually(t, "another full pass", func() bool { return w.Calls("get_height") >= after+2 })
	if err := r.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c := strings.Count(r.logs.String(), alert); c != 1 {
		t.Fatalf("the ALERT %d times, want 1", c)
	}
	if n += r.paidHooks(o.ID); n != 1 {
		t.Fatalf("order.paid delivered %d times, want 1", n)
	}
}

// The fan-out tells every rail, also after one fails, and returns the first error.
func TestFanOutTellsEveryRail(t *testing.T) {
	var calls []string
	hook := func(name string, err error) lnpay.OnLightningPaid {
		return func(_ context.Context, o store.Order) error {
			calls = append(calls, name+" "+o.ID)
			return err
		}
	}
	e1, e2 := errors.New("first"), errors.New("second")
	err := fanOut([]lnpay.OnLightningPaid{hook("onchain", e1), hook("monero", e2)})(context.Background(), store.Order{ID: "o1"})
	if err != e1 || strings.Join(calls, ",") != "onchain o1,monero o1" {
		t.Fatalf("err %v, calls %v", err, calls)
	}
	calls = nil
	if err := fanOut([]lnpay.OnLightningPaid{hook("onchain", nil), hook("monero", e2)})(context.Background(), store.Order{ID: "o2"}); err != e2 || len(calls) != 2 {
		t.Fatalf("err %v, calls %v", err, calls)
	}
	calls = nil
	if err := fanOut([]lnpay.OnLightningPaid{hook("onchain", nil), hook("monero", nil)})(context.Background(), store.Order{ID: "o3"}); err != nil || len(calls) != 2 {
		t.Fatalf("err %v, calls %v", err, calls)
	}
}

// The lock file must be a regular file of the effective user with no group or other bits: a symbolic link (to any
// file) and a group-readable file are refused, and a correct existing file is locked.
func TestLockFileShape(t *testing.T) {
	const shape = "the data directory's lock file must be a regular file of this user, mode 0600"
	t.Run("symbolic link", func(t *testing.T) {
		dir, other := t.TempDir(), t.TempDir()
		target := filepath.Join(other, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "satbooth.lock")); err != nil {
			t.Fatal(err)
		}
		if f, err := lockDataDir(dir); err == nil || err.Error() != shape {
			if f != nil {
				f.Close()
			}
			t.Fatalf("a symbolic link: %v", err)
		}
	})
	t.Run("group-readable", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "satbooth.lock")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o640); err != nil {
			t.Fatal(err)
		}
		if f, err := lockDataDir(dir); err == nil || err.Error() != shape {
			if f != nil {
				f.Close()
			}
			t.Fatalf("a group-readable file: %v", err)
		}
	})
	t.Run("correct existing file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "satbooth.lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := lockDataDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	})
}

// One SatBooth process per data directory: a second Run on a data directory another Run holds is refused before it
// opens anything, and starts once the first has returned.
func TestOneProcessPerDataDir(t *testing.T) {
	r := startRig(t)
	second := func() (error, string) {
		logs := &syncBuf{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { // no t off the test goroutine: on the deadline it cancels, and the caller sees Run's answer
			deadline := time.Now().Add(15 * time.Second)
			for !strings.Contains(logs.String(), logStarted) && ctx.Err() == nil && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
		}()
		pub, _ := net.Listen("tcp", "127.0.0.1:0")
		prv, _ := net.Listen("tcp", "127.0.0.1:0")
		defer pub.Close()
		defer prv.Close()
		err := Run(ctx, r.cfg, WithWallet(newFakeWallet()), WithListeners(pub, prv),
			WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
		return err, logs.String()
	}
	err, logs := second()
	if err == nil || err.Error() != "service: another SatBooth process holds the data directory" ||
		!strings.Contains(logs, `"reason":"data_dir_lock"`) {
		t.Fatalf("second Run while the first holds the data directory: %v / %s", err, logs)
	}
	if strings.Contains(logs, logStarted) {
		t.Fatal("the second Run started")
	}
	fi, err := os.Stat(filepath.Join(r.cfg.DataDir, "satbooth.lock"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file: %v %v", fi, err)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if err, logs := second(); err != nil {
		t.Fatalf("Run after the first returned: %v / %s", err, logs)
	}
}
