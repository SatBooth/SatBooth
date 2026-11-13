//go:build qualify

package qualify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/memlock"
	"github.com/satbooth/satbooth/relay"
	"github.com/satbooth/satbooth/service"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/webhook"
)

// The Lightning phases start the whole SatBooth service in-process, in the qualify container, against
// relay-satbooth and the invoice connection from the bootstrap. The merchant's hub plays the customer's
// wallet (POST /api/payments). Every phase ends with the safety check: one ledger transfer and one order.paid
// delivered per paid order, none for an unpaid one.
const (
	qualifyIP   = "172.30.78.30"
	hookAddr    = qualifyIP + ":9100"
	publicAddr  = qualifyIP + ":9101"
	privateAddr = qualifyIP + ":9102"
	orderMsat   = 2_000_000 // 2,000 sat
	paidBound   = 10 * time.Second
	quietPeriod = 12 * time.Second // longer than the reconciler's 10 s idle round
	hookBound   = 45 * time.Second
	acctID      = "acct-qualify"
)

// logBuf collects the service's log lines (fixed lines; they hold no secrets) for the test log.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// hookEvent is one delivery the receiver accepted, with its raw body (a priced order's carries pricing).
type hookEvent struct {
	ID, Type, OrderID string
	Seq               int
	At                time.Time
	Body              []byte
}

// receiver is the merchant's webhook endpoint: it verifies every request with webhook.Verify and answers 204.
type receiver struct {
	mu     sync.Mutex
	secret []byte
	events []hookEvent
	bad    int
	srv    *http.Server
	now    func() time.Time // the clock of the service whose signatures it verifies (nil: time.Now)
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, 8<<10))
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	if err != nil || req.Method != http.MethodPost ||
		webhook.Verify(r.secret, req.Header.Get(webhook.HeaderName), body, now, webhook.RecommendedWindow) != nil {
		r.bad++
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var p struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		OrderID string `json:"order_id"`
		Seq     int    `json:"seq"`
	}
	if json.Unmarshal(body, &p) != nil || p.Seq < 1 || p.ID != store.EventID(p.Type, p.OrderID, p.Seq) {
		r.bad++
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.events = append(r.events, hookEvent{ID: p.ID, Type: p.Type, OrderID: p.OrderID, Seq: p.Seq, At: now, Body: body})
	w.WriteHeader(http.StatusNoContent)
}

func (r *receiver) count(orderID, typ string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.OrderID == orderID && e.Type == typ {
			n++
		}
	}
	return n
}

// countSeq is count for one sequence number.
func (r *receiver) countSeq(orderID, typ string, seq int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.OrderID == orderID && e.Type == typ && e.Seq == seq {
			n++
		}
	}
	return n
}

// bodyOf returns the raw body of the first delivery of typ for the order, or nil.
func (r *receiver) bodyOf(orderID, typ string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.OrderID == orderID && e.Type == typ {
			return append([]byte(nil), e.Body...)
		}
	}
	return nil
}

func (r *receiver) badCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bad
}

// forOrder is the number of webhooks received for one order, of any type.
func (r *receiver) forOrder(orderID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.OrderID == orderID {
			n++
		}
	}
	return n
}

func (r *receiver) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// lnEnv is one phase's world: the hubs, the service's files under /state/lightning/<name>, and the receiver.
type lnEnv struct {
	t        *testing.T
	name     string
	st       *state
	sat, mer *hubAPI
	root     string
	dataDir  string
	readKey  string
	writeKey string
	secret   []byte
	cfgPath  string
	rcv      *receiver
	hc       *http.Client
	clock    func() time.Time  // the service's clock (nil: time.Now); setClock sets it with the receiver's
	rateRT   http.RoundTripper // the rate providers' transport (the fake rate server's); nil: the real one
	opts     []service.Option  // more options for Run (the refund phases' payout wallet)
}

// setClock gives the service and the webhook receiver the clock now.
func (e *lnEnv) setClock(now func() time.Time) {
	e.clock = now
	e.rcv.mu.Lock()
	e.rcv.now = now
	e.rcv.mu.Unlock()
}

// merchantDown is set by a phase that runs while the script has stopped the merchant's hub (TestRefundFailed): the
// environment then does not check the merchant hub's token.
var merchantDown bool

// newLnEnv prepares the service's files (fresh API keys and webhook secret every process, kept in the test
// volume and never printed), the receiver, and the hub clients. The data directory persists between phases.
func newLnEnv(t *testing.T, name string) *lnEnv { return newLnEnvRelay(t, name, "") }

// newLnEnvRelay is newLnEnv with the service's wallet relay URL set to relayURL (empty: relay-satbooth's own).
func newLnEnvRelay(t *testing.T, name, relayURL string) *lnEnv {
	t.Helper()
	return newEnvAt(t, name, filepath.Join(stateDir(t), "lightning", name), relayURL, nil, nil, nil)
}

// newEnvAt is newLnEnvRelay with the service's files in root and, if oc is set, the on-chain rail configured, if
// rates is set, fiat pricing (whose providers a phase reaches through rateRT, the fake rate server), and if monero is
// set, the Monero rail (against the real wallet-satbooth: there is no wallet-rpc seam in these phases).
func newEnvAt(t *testing.T, name, root, relayURL string, oc *service.OnchainConfig, rates *service.RatesConfig, monero *service.MoneroConfig) *lnEnv {
	t.Helper()
	st, err := loadState(stateDir(t))
	if err != nil {
		t.Fatalf("state (did TestBootstrap run?): %v", shownErr(err))
	}
	e := &lnEnv{t: t, name: name, st: st}
	e.sat = newHubAPI(st.Hubs[hubSatbooth].URL, st.Hubs[hubSatbooth].Token)
	e.mer = newHubAPI(st.Hubs[hubMerchant].URL, st.Hubs[hubMerchant].Token)
	e.sat.ensureToken(t)
	if !merchantDown {
		e.mer.ensureToken(t)
	}
	e.hc = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	e.root = root
	if err := os.MkdirAll(e.root, 0o700); err != nil {
		t.Fatal(err)
	}
	e.dataDir = filepath.Join(e.root, "data")

	e.writeKey, e.readKey = randHex(t), randHex(t)
	keys := map[string]any{"keys": []map[string]string{
		{"id": "qualify-write", "capability": "orders:write", "sha256": sha256Hex(e.writeKey)},
		{"id": "qualify-read", "capability": "orders:read", "sha256": sha256Hex(e.readKey)},
	}}
	apiFile := filepath.Join(e.root, "api-keys.json")
	writeJSON0600(t, apiFile, keys)
	secretHex := randHex(t)
	// The file holds the secret as hex; the service signs with the decoded bytes (webhook.LoadSecret).
	var derr error
	if e.secret, derr = hex.DecodeString(secretHex); derr != nil {
		t.Fatal(derr)
	}
	secretFile := filepath.Join(e.root, "webhook.secret")
	if err := os.WriteFile(secretFile, []byte(secretHex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if relayURL == "" {
		relayURL = st.RelayURLs[hubSatbooth]
	}
	cfg := service.Config{
		PublicListen: publicAddr, PrivateListen: privateAddr, CheckoutBaseURL: "http://" + publicAddr,
		DataDir: e.dataDir,
		NWC: service.NWCConfig{RelayURL: relayURL, WalletPubKey: st.Wallets["invoice"],
			ClientKeyFile: st.KeyFiles["invoice"], TimeoutSeconds: 10, Network: "regtest"},
		APIKeysFile: apiFile,
		Webhooks:    []service.WebhookEndpoint{{Name: "main", URL: "http://" + hookAddr + "/hook", SecretFile: secretFile}},
		Limits:      service.LimitsConfig{DailyMsat: 1_000_000_000, MaxMsat: 100_000_000},
		// The shortest allowed, so the expiry phase waits as little as the service permits.
		InvoiceExpiryMinutes: 15,
		Onchain:              oc,
		Rates:                rates,
		Monero:               monero,
	}
	e.cfgPath = filepath.Join(e.root, "config.json")
	writeJSON0600(t, e.cfgPath, cfg)

	e.rcv = &receiver{secret: e.secret}
	ln, err := net.Listen("tcp", hookAddr)
	if err != nil {
		t.Fatalf("webhook receiver: %v", shownErr(err))
	}
	e.rcv.srv = &http.Server{Handler: e.rcv}
	go e.rcv.srv.Serve(ln)
	t.Cleanup(func() { e.rcv.srv.Close() })
	return e
}

func randHex(t testing.TB) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func writeJSON0600(t testing.TB, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// svcRun is a started service.
type svcRun struct {
	t      *testing.T
	cancel context.CancelFunc
	done   chan error
	logs   *logBuf
	once   sync.Once
}

// start runs the service exactly as cmd/satbooth does: ReadConfig, Lock, CheckFiles, Run.
func (e *lnEnv) start() *svcRun {
	e.t.Helper()
	s, err := e.tryStart()
	if err != nil {
		e.t.Fatalf("%v\n%s", shownErr(err), s.logs.String())
	}
	return s
}

// tryStart is start that reports a service which ends while starting (a failed startup check) as an error. The
// returned run is never nil: its logs hold what the service wrote.
func (e *lnEnv) tryStart() (*svcRun, error) {
	t := e.t
	t.Helper()
	logs := &logBuf{}
	lg := slog.New(slog.NewJSONHandler(logs, nil))
	cfg, err := service.ReadConfig(e.cfgPath)
	if err != nil {
		t.Fatalf("ReadConfig: %v", shownErr(err))
	}
	if err := service.Lock(cfg, lg, memlock.Protect); err != nil {
		t.Fatalf("Lock: %v", shownErr(err))
	}
	if err := cfg.CheckFiles(); err != nil {
		t.Fatalf("CheckFiles: %v", shownErr(err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &svcRun{t: t, cancel: cancel, done: make(chan error, 1), logs: logs}
	opts := []service.Option{service.WithLogger(lg)}
	if e.clock != nil {
		opts = append(opts, service.WithClock(e.clock))
	}
	if e.rateRT != nil {
		opts = append(opts, service.WithRateTransport(e.rateRT))
	}
	opts = append(opts, e.opts...)
	go func() { s.done <- service.Run(ctx, cfg, opts...) }()
	t.Cleanup(func() { s.stopQuiet() })
	began := time.Now()
	end := time.Now().Add(60 * time.Second)
	for {
		select {
		case err := <-s.done:
			s.once.Do(func() {}) // the caller owns the failure; the cleanup must not report it again
			return s, fmt.Errorf("the service ended while starting: %w", err)
		default:
		}
		resp, err := e.hc.Get("http://" + publicAddr + "/static/style.css")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(end) {
			s.stopQuiet()
			return s, errors.New("the service did not answer within 60 s")
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("MEASURED lightning=%s service_start_seconds=%.1f", e.name, time.Since(began).Seconds())
	if strings.Contains(logs.String(), "memory locked") {
		t.Logf("MEASURED lightning=%s memory=locked (mlockall and core dumps off, service.Lock with memlock.Protect, allow_swap false)", e.name)
	} else {
		t.Fatalf("memory was not locked: %s", firstLines(logs.String(), 3))
	}
	return s, nil
}

func firstLines(s string, n int) string {
	l := strings.SplitN(s, "\n", n+1)
	if len(l) > n {
		l = l[:n]
	}
	return strings.Join(l, " | ")
}

// stop cancels Run, waits for it to return nil, and fails on any ALERT line in the service's log.
func (s *svcRun) stop() {
	s.t.Helper()
	s.stopQuiet()
	logs := s.logs.String()
	s.t.Logf("service log (%d lines)", strings.Count(logs, "\n"))
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "ALERT") || strings.Contains(l, "startup failed") || strings.Contains(l, "unexpectedly") {
			s.t.Errorf("service log line: %s", l)
		}
	}
}

func (s *svcRun) stopQuiet() {
	s.once.Do(func() {
		s.cancel()
		select {
		case err := <-s.done:
			if err != nil {
				s.t.Errorf("Run returned %v", shownErr(err))
			}
		case <-time.After(60 * time.Second):
			s.t.Error("Run did not return within 60 s of the cancel")
		}
	})
}

// ---- HTTP helpers ----

// ocJSON is the API's on-chain object (api.onchainView).
type ocJSON struct {
	State                 string  `json:"state"`
	Address               *string `json:"address"`
	RequiredConfirmations int     `json:"required_confirmations"`
	Confirmations         int     `json:"confirmations"`
	ReceivedSat           int64   `json:"received_sat"`
	ConfirmedSat          int64   `json:"confirmed_sat"`
	LateSat               int64   `json:"late_sat"`
	Overpaid              bool    `json:"overpaid"`
	Stale                 bool    `json:"stale"`
}

// xmJSON is the API's Monero object (api.moneroView).
type xmJSON struct {
	State                 string  `json:"state"`
	Address               *string `json:"address"`
	RequiredConfirmations int     `json:"required_confirmations"`
	Confirmations         int     `json:"confirmations"`
	ReceivedPiconero      int64   `json:"received_piconero"`
	ConfirmedPiconero     int64   `json:"confirmed_piconero"`
	LatePiconero          int64   `json:"late_piconero"`
	Overpaid              bool    `json:"overpaid"`
	Stale                 bool    `json:"stale"`
}

// pricingJSON is the API's pricing object (api.pricingView): exactly its members, for DisallowUnknownFields. rates
// has one member per asset priced ("BTC", "XMR").
type pricingJSON struct {
	Price         string `json:"price"`
	Currency      string `json:"currency"`
	MarginPercent string `json:"margin_percent"`
	QuotedAt      string `json:"quoted_at"`
	Rates         map[string]struct {
		Rate    string `json:"rate"`
		Sources []struct {
			Provider string `json:"provider"`
			Rate     string `json:"rate"`
			At       string `json:"at"`
		} `json:"sources"`
	} `json:"rates"`
}

type orderJSON struct {
	OnchainSat     int64 `json:"onchain_sat"`
	MoneroPiconero int64 `json:"monero_piconero"`
	Lightning      *struct {
		State string `json:"state"`
		// Plan 9: the service always hands the API its order figures, so a Lightning order carries them.
		PaidMsat       *int64 `json:"paid_msat"`
		RefundedMsat   *int64 `json:"refunded_msat"`
		RefundOpenMsat *int64 `json:"refund_open_msat"`
	} `json:"lightning"`
	Onchain     *ocJSON      `json:"onchain"`
	Monero      *xmJSON      `json:"monero"`
	ID          string       `json:"id"`
	MerchantRef string       `json:"merchant_ref"`
	AccountID   string       `json:"account_id"`
	Msat        int64        `json:"lightning_msat"`
	State       string       `json:"state"`
	CreatedAt   string       `json:"created_at"`
	ExpiresAt   string       `json:"expires_at"`
	PaidAt      *string      `json:"paid_at"`
	CheckoutURL string       `json:"checkout_url"`
	Pricing     *pricingJSON `json:"pricing"` // null for an order with explicit amounts
}

func (e *lnEnv) api(method, path, key string, body any, out any) int {
	e.t.Helper()
	code, raw, _ := e.call(method, path, key, body)
	if out != nil && code < 300 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields() // the response is an allowlist: a new field would fail here
		if err := dec.Decode(out); err != nil {
			e.t.Fatalf("%s %s: unexpected response shape: %v", method, path, shownErr(err))
		}
	}
	return code
}

// apiErr is api for a call expected to fail: the status, the error code of the body ({"error":"<code>"}; "" for a
// success) and the Retry-After header.
func (e *lnEnv) apiErr(method, path, key string, body any) (status int, code, retryAfter string) {
	e.t.Helper()
	status, raw, h := e.call(method, path, key, body)
	if status >= 300 {
		code = e.errCode(method, path, status, raw)
	}
	return status, code, h.Get("Retry-After")
}

// errCode decodes an error answer: exactly {"error":"<code>"}, with a code; any other shape fails the phase.
func (e *lnEnv) errCode(method, path string, status int, raw []byte) string {
	e.t.Helper()
	var eb struct {
		Error string `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&eb); err != nil || eb.Error == "" {
		e.t.Fatalf("%s %s: status %d with an error body of another shape", method, path, status)
	}
	return eb.Error
}

// call makes one API request and returns the status, the body and the headers.
func (e *lnEnv) call(method, path, key string, body any) (int, []byte, http.Header) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, merr := json.Marshal(body)
		if merr != nil {
			e.t.Fatal(merr)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+privateAddr+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, shownErr(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, resp.Header
}

func (e *lnEnv) createOrder(ref string, msat, expiresIn int64) orderJSON {
	e.t.Helper()
	var o orderJSON
	body := map[string]any{"account_id": acctID, "merchant_ref": ref, "lightning_msat": msat, "expires_in": expiresIn}
	if code := e.api("POST", "/v1/orders", e.writeKey, body, &o); code != 201 {
		e.t.Fatalf("create order: status %d", code)
	}
	if o.State != "new" || o.PaidAt != nil || !strings.HasPrefix(o.CheckoutURL, "http://"+publicAddr+"/c/") {
		e.t.Fatalf("created order looks wrong: state %q", o.State)
	}
	return o
}

// createFiatOrder creates a priced order: price (decimal text, sent verbatim) in currency, on rails (nil: the
// configured rails). It returns the order on 201 or 200, the status, and the error code of a failure.
func (e *lnEnv) createFiatOrder(ref, price, currency string, rails []string, expiresIn int64) (orderJSON, int, string) {
	e.t.Helper()
	body := map[string]any{"account_id": acctID, "merchant_ref": ref, "price": price, "currency": currency, "expires_in": expiresIn}
	if rails != nil {
		body["rails"] = rails
	}
	status, raw, _ := e.call("POST", "/v1/orders", e.writeKey, body)
	var o orderJSON
	if status >= 300 {
		return o, status, e.errCode("POST", "/v1/orders", status, raw)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		e.t.Fatalf("create priced order: unexpected response shape: %v", shownErr(err))
	}
	return o, status, ""
}

func (e *lnEnv) order(id string) orderJSON {
	e.t.Helper()
	var o orderJSON
	if code := e.api("GET", "/v1/orders/"+id, e.readKey, nil, &o); code != 200 {
		e.t.Fatalf("get order: status %d", code)
	}
	return o
}

func tokenOf(o orderJSON) string { return o.CheckoutURL[strings.LastIndex(o.CheckoutURL, "/")+1:] }

var invoiceRe = regexp.MustCompile(`value="(ln[a-z0-9]+)"`)

func (e *lnEnv) page(token string) (int, string) {
	e.t.Helper()
	resp, err := e.hc.Get("http://" + publicAddr + "/c/" + token)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b)
}

// pressPay is the customer pressing "Pay with Lightning": POST /c/{token}/pay (303 to the page), then the page,
// which carries the invoice.
func (e *lnEnv) pressPay(token string) string {
	e.t.Helper()
	resp, err := e.hc.Post("http://"+publicAddr+"/c/"+token+"/pay", "application/x-www-form-urlencoded", nil)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("pay: status %d, want 303", resp.StatusCode)
	}
	code, page := e.page(token)
	m := invoiceRe.FindStringSubmatch(page)
	if code != 200 || m == nil || !strings.Contains(page, "Waiting for payment") {
		e.t.Fatalf("after pay the page has status %d and no invoice", code)
	}
	return m[1]
}

// checkQR fetches /c/{token}/qr.svg and checks it is a well-formed SVG.
func (e *lnEnv) checkQR(token string) {
	e.t.Helper()
	resp, err := e.hc.Get("http://" + publicAddr + "/c/" + token + "/qr.svg")
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/svg+xml" {
		e.t.Fatalf("qr.svg: status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	dec := xml.NewDecoder(bytes.NewReader(b))
	root := ""
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			e.t.Fatalf("qr.svg is not well-formed XML: %v", shownErr(err))
		}
		if se, ok := tok.(xml.StartElement); ok && root == "" {
			root = se.Name.Local
		}
	}
	if root != "svg" || !bytes.Contains(b, []byte("</svg>")) {
		e.t.Fatalf("qr.svg root element %q", root)
	}
	e.t.Logf("qr.svg: well-formed SVG of %d bytes", len(b))
}

// customerPays pays the invoice from the merchant's hub and returns how long the hub took to answer.
func (e *lnEnv) customerPays(bolt11 string) time.Duration {
	e.t.Helper()
	began := time.Now()
	tx, err := e.mer.payInvoice(bolt11)
	d := time.Since(began)
	if err != nil {
		e.t.Fatalf("the customer's payment failed: %v", shownErr(err))
	}
	if tx.State != "settled" {
		e.t.Fatalf("the customer's payment is %s, want settled", shownState(tx.State))
	}
	return d
}

// waitOrder polls the API until the order reads want. It returns how long it took from since.
func (e *lnEnv) waitOrder(id, want string, since time.Time, bound time.Duration) time.Duration {
	e.t.Helper()
	var d time.Duration
	poll(e.t, "order "+want, bound, 100*time.Millisecond, func() (bool, error) {
		o := e.order(id)
		if o.State == want {
			d = time.Since(since)
			return true, nil
		}
		return false, fmt.Errorf("state %s", o.State)
	})
	return d
}

func (e *lnEnv) waitHook(orderID, typ string, bound time.Duration) {
	e.t.Helper()
	poll(e.t, typ+" delivered", bound, 250*time.Millisecond, func() (bool, error) {
		return e.rcv.count(orderID, typ) >= 1, errors.New("not delivered yet")
	})
}

// settleQuiet lets the service run one more idle round, so a duplicate would show up in the safety check.
func (e *lnEnv) settleQuiet() { time.Sleep(quietPeriod) } // a negative check has to wait; it is not a synchronisation

// ---- phase files ----

type lnFile struct {
	OrderID   string `json:"order_id"`
	Token     string `json:"token"`
	Invoice   string `json:"invoice"`
	Hash      string `json:"hash"`
	Msat      int64  `json:"msat"`
	ExpiresAt int64  `json:"order_expires_at"`
}

func (e *lnEnv) save(f *lnFile) { writeJSON0600(e.t, filepath.Join(e.root, "phase.json"), f) }

func (e *lnEnv) load() *lnFile {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.root, "phase.json"))
	if err != nil {
		e.t.Fatalf("phase file (did the Before phase run?): %v", shownErr(err))
	}
	var f lnFile
	if err := json.Unmarshal(b, &f); err != nil {
		e.t.Fatal(err)
	}
	return &f
}

// ---- the safety check ----

// want says what the safety check expects of one order.
type want struct {
	orderID string
	msat    int64
	paid    bool // a ledger transfer and an order.paid delivery
	expired bool // an order.expired delivery and no transfer
}

// safety runs after the service has stopped (it opens the ledger and the store, which the service holds while it
// runs). Per paid order: one transfer with the order's amount, one successful order.paid attempt in the store and, if
// the delivery was in this process, exactly one request at the receiver. Per unpaid order: no transfer, no events.
// Ledger-wide: the merchant account holds exactly the paid orders' amounts.
func (e *lnEnv) safety(wants []want) {
	t := e.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	lg, err := ledger.Open(ctx, filepath.Join(e.dataDir, "ledger.db"))
	if err != nil {
		t.Fatalf("safety: ledger: %v", shownErr(err))
	}
	defer lg.Close()
	st, err := store.Open(ctx, filepath.Join(e.dataDir, "store.db"))
	if err != nil {
		t.Fatalf("safety: store: %v", shownErr(err))
	}
	defer st.Close()

	var sum uint64
	for _, w := range wants {
		invs, err := st.InvoicesForOrder(ctx, w.orderID)
		if err != nil {
			t.Fatal(err)
		}
		transfers := 0
		for _, inv := range invs {
			tr, found, err := lg.LookupTransfer(ctx, lnpay.PaidTransferID(inv.PaymentHash))
			if err != nil {
				t.Fatal(err)
			}
			if found {
				transfers++
				if tr.Amount != ledger.U128From64(uint64(w.msat)) {
					t.Errorf("order %s: transfer amount %s, want %d", w.orderID, tr.Amount, w.msat)
				}
			}
		}
		wantTransfers := 0
		if w.paid {
			wantTransfers = 1
			sum += uint64(w.msat)
		}
		if transfers != wantTransfers {
			t.Errorf("order %s: %d ledger transfers, want %d", w.orderID, transfers, wantTransfers)
		}
		for _, typ := range []string{store.EventPaid, store.EventExpired} {
			atts, err := st.AttemptsFor(ctx, store.MainEndpoint, store.EventID(typ, w.orderID, 1))
			if err != nil {
				t.Fatal(err)
			}
			ok := 0
			for _, a := range atts {
				if a.OK {
					ok++
				}
			}
			wantOK := 0
			if (typ == store.EventPaid && w.paid) || (typ == store.EventExpired && w.expired) {
				wantOK = 1
			}
			if ok != wantOK {
				t.Errorf("order %s: %d successful %s deliveries recorded, want %d", w.orderID, ok, typ, wantOK)
			}
			if wantOK == 0 && len(atts) != 0 {
				t.Errorf("order %s: %d %s attempts for an event that must not exist", w.orderID, len(atts), typ)
			}
			// Every phase delivers its events in the process that checks, so the receiver saw each exactly once.
			if n := e.rcv.count(w.orderID, typ); n != wantOK {
				t.Errorf("order %s: the receiver saw %s %d times, want %d", w.orderID, typ, n, wantOK)
			}
		}
	}
	pend, err := st.UndeliveredEvents(ctx, store.MainEndpoint, time.Unix(0, 0), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 0 {
		t.Errorf("%d events are undelivered", len(pend))
	}
	if n := e.rcv.badCount(); n != 0 {
		t.Errorf("the receiver refused %d requests (bad signature or payload)", n)
	}
	for _, acct := range []ledger.U128{lnpay.MerchantLightning, lnpay.LightningHub} {
		b, err := lg.Balance(ctx, acct)
		if err != nil {
			t.Fatal(err)
		}
		posted := b.CreditsPosted
		if acct == lnpay.LightningHub {
			posted = b.DebitsPosted
		}
		if posted != ledger.U128From64(sum) {
			t.Errorf("ledger account %s posted %s msat, want %d", acct, posted, sum)
		}
	}
	t.Logf("SAFETY lightning=%s orders=%d paid_transfers=%d merchant_msat=%d events_received_here=%d", e.name, len(wants), countPaid(wants), sum, e.rcv.total())
}

func countPaid(ws []want) int {
	n := 0
	for _, w := range ws {
		if w.paid {
			n++
		}
	}
	return n
}

// hubOnce checks both hubs' own records for the payment: one settled incoming row at SatBooth's hub, one settled
// outgoing row at the merchant's.
func (e *lnEnv) hubOnce(hash string) {
	t := e.t
	t.Helper()
	for name, h := range map[string]*hubAPI{"satbooth": e.sat, "merchant": e.mer} {
		rows, err := h.rowsFor(hash)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].State != "settled" {
			t.Errorf("%s hub: %d rows for the payment hash (states %v), want one settled", name, len(rows), rowStates(rows))
		}
	}
}

func rowStates(rs []hubTx) []string {
	var s []string
	for _, r := range rs {
		s = append(s, shownType(r.Type)+"/"+shownState(r.State))
	}
	return s
}

// hashes reads the payment hash of the order's invoice from the store (after the service has stopped).
func (e *lnEnv) hashes(orderID string) []string {
	t := e.t
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(e.dataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	invs, err := st.InvoicesForOrder(ctx, orderID)
	if err != nil {
		t.Fatal(err)
	}
	var hs []string
	for _, i := range invs {
		hs = append(hs, i.PaymentHash)
	}
	return hs
}

// ---- the phases ----

// TestLightningPay: create an order, press pay, the customer pays from the merchant's hub, the order reads paid
// within 10 s, the order.paid webhook arrives and verifies. A second order stays unpaid.
func TestLightningPay(t *testing.T) {
	e := newLnEnv(t, "pay")
	s := e.start()

	o := e.createOrder("pay-"+randHex(t)[:12], orderMsat, 3600)
	tok := tokenOf(o)
	if code, page := e.page(tok); code != 200 || !strings.Contains(page, "Ready to pay") || invoiceRe.MatchString(page) {
		t.Fatalf("the first page: status %d, expected the pay button and no invoice", code)
	}
	began := time.Now()
	invoice := e.pressPay(tok)
	t.Logf("MEASURED lightning=pay press_pay_to_invoice_seconds=%.2f", time.Since(began).Seconds())
	if again := e.pressPay(tok); again != invoice {
		t.Fatal("pressing pay twice made a second invoice")
	}
	e.checkQR(tok)
	if e.order(o.ID).State != "new" {
		t.Fatal("the unpaid order is not new")
	}

	other := e.createOrder("pay-unpaid-"+randHex(t)[:12], orderMsat, 3600)
	e.pressPay(tokenOf(other))

	paidAt := time.Now()
	payDur := e.customerPays(invoice)
	d := e.waitOrder(o.ID, "paid", paidAt, paidBound)
	t.Logf("MEASURED lightning=pay customer_payment_call_seconds=%.2f pay_to_order_paid_seconds=%.2f (the notification subscription is live; a plain poll round of 2 open invoices at 5 lookups a second is as fast, so the path is not told apart from outside)", payDur.Seconds(), d.Seconds())
	got := e.order(o.ID)
	if got.PaidAt == nil {
		t.Fatal("a paid order has no paid_at")
	}
	if _, page := e.page(tok); !strings.Contains(page, ">Paid<") {
		t.Fatal("the checkout page does not say Paid")
	}
	hookBegan := time.Now()
	e.waitHook(o.ID, store.EventPaid, hookBound)
	t.Logf("MEASURED lightning=pay order_paid_to_webhook_seconds=%.2f", time.Since(hookBegan).Seconds())
	if e.order(other.ID).State != "new" {
		t.Fatal("the unpaid order changed state")
	}
	e.settleQuiet()
	s.stop()

	hs := e.hashes(o.ID)
	if len(hs) != 1 {
		t.Fatalf("%d invoices for the paid order, want 1", len(hs))
	}
	e.hubOnce(hs[0])
	e.safety([]want{{orderID: o.ID, msat: orderMsat, paid: true}, {orderID: other.ID, msat: orderMsat}})
}

// TestLightningReplay: a paid order is seen again by a restarted service, which finds its invoice still open with the
// transfer already in the ledger and completes it again without a lookup (reconcileInv's paidTransfer branch,
// completePaid is idempotent). The hub does not report a second time. The races between the notification and the poll
// are covered with the fake wallet (lnpay TestPaidExactlyOnce, TestPaymentRecordedConcurrentlyOnce). Live here: the hub
// refuses the customer's second payment of the same invoice, and pressing pay on a paid order makes no invoice. One
// transfer and one delivery remain.
func TestLightningReplay(t *testing.T) {
	e := newLnEnv(t, "replay")
	s := e.start()
	o := e.createOrder("replay-"+randHex(t)[:12], orderMsat, 3600)
	invoice := e.pressPay(tokenOf(o))
	paidAt := time.Now()
	e.customerPays(invoice)
	d := e.waitOrder(o.ID, "paid", paidAt, paidBound)
	t.Logf("MEASURED lightning=replay pay_to_order_paid_seconds=%.2f", d.Seconds())
	e.waitHook(o.ID, store.EventPaid, hookBound)
	s.stop()

	// The second pass over the paid, still open invoice: a new service on the same data.
	s = e.start()
	if e.order(o.ID).State != "paid" {
		t.Fatal("the order is not paid after the restart")
	}
	e.settleQuiet()

	// The customer pays the same invoice again: the hub refuses, nothing moves.
	if tx, err := e.mer.payInvoice(invoice); err == nil {
		t.Fatalf("a second payment of the same invoice was accepted (state %s)", shownState(tx.State))
	} else {
		t.Logf("MEASURED lightning=replay second_customer_payment_refused=%v", errString(err))
	}
	// Pressing pay on a paid order makes no invoice.
	resp, err := e.hc.Post("http://"+publicAddr+"/c/"+tokenOf(o)+"/pay", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/c/"+tokenOf(o) {
		t.Errorf("pressing pay on a paid order: status %d, location %q, want 303 to the page", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, page := e.page(tokenOf(o)); invoiceRe.MatchString(page) {
		t.Error("the paid order's page shows an invoice")
	}
	e.settleQuiet()
	s.stop()

	hs := e.hashes(o.ID)
	if len(hs) != 1 {
		t.Fatalf("%d invoices for the paid order, want 1", len(hs))
	}
	e.hubOnce(hs[0])
	e.safety([]want{{orderID: o.ID, msat: orderMsat, paid: true}})
}

// TestLightningExpireBefore makes an order whose invoice nobody pays. The invoice lives 15 minutes and the
// closing lookup is made 2 minutes after its expiry, so TestLightningExpireAfter runs last in the script.
func TestLightningExpireBefore(t *testing.T) {
	e := newLnEnv(t, "expire")
	s := e.start()
	o := e.createOrder("expire-"+randHex(t)[:12], orderMsat, 300)
	invoice := e.pressPay(tokenOf(o))
	exp, err := time.Parse(time.RFC3339, o.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	e.save(&lnFile{OrderID: o.ID, Token: tokenOf(o), Invoice: invoice, Msat: orderMsat, ExpiresAt: exp.Unix()})
	if e.order(o.ID).State != "new" {
		t.Fatal("the order is not new")
	}
	s.stop()
	e.safety([]want{{orderID: o.ID, msat: orderMsat}})
}

// TestLightningExpireAfter: the order is long past its expiry but its invoice is open until its 15 minutes and the
// 2 minute grace have passed: the order reads new until the closing lookup, then expired, and order.expired is
// delivered once.
func TestLightningExpireAfter(t *testing.T) {
	e := newLnEnv(t, "expire")
	f := e.load()
	// The invoice's own expiry from the store (the service is not running yet), plus lnpay's grace: the earliest
	// moment a closing lookup may close it, and so the earliest the order may read expired.
	ctx := context.Background()
	st0, err := store.Open(ctx, filepath.Join(e.dataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	invs, err := st0.InvoicesForOrder(ctx, f.OrderID)
	st0.Close()
	if err != nil || len(invs) != 1 {
		t.Fatalf("invoices of the order: %d (err %v), want 1", len(invs), shownErr(err))
	}
	earliest := invs[0].ExpiresAt.Add(lnpay.DefaultGrace)
	orderExpiry := time.Unix(f.ExpiresAt, 0)
	if !orderExpiry.Before(earliest) {
		t.Fatal("the order expires after its invoice is closable: nothing to observe")
	}
	// The property needs a window in which the order is past its expiry and its invoice is not yet closable.
	if left := time.Until(earliest); left < 10*time.Second {
		t.Fatalf("phase started too late to observe the property: %.0f s left before the invoice may be closed", left.Seconds())
	}
	s := e.start()
	began := time.Now()
	sawNew := false
	poll(t, "order expired", 25*time.Minute, time.Second, func() (bool, error) {
		state := e.order(f.OrderID).State
		now := time.Now()
		switch state {
		case "new":
			if now.Before(orderExpiry) {
				return false, errors.New("order not past its expiry yet")
			}
			if now.Before(earliest) {
				sawNew = true // past the order's expiry, invoice not closable yet, and still new
			}
			return false, errors.New("new")
		case "expired":
			if now.Before(earliest) {
				t.Errorf("the order read expired %.0f s before the earliest closing lookup", earliest.Sub(now).Seconds())
			}
			return true, nil
		}
		t.Fatalf("unexpected state %q", state)
		return false, nil
	})
	t.Logf("MEASURED lightning=expire after_start_to_expired_seconds=%.1f invoice_to_expired_seconds=%.1f read_new_after_order_expiry=%v",
		time.Since(began).Seconds(), time.Since(invs[0].CreatedAt).Seconds(), sawNew)
	if !sawNew {
		t.Errorf("the order was never seen new between its own expiry and the invoice's earliest closure")
	}
	e.waitHook(f.OrderID, store.EventExpired, hookBound)
	if _, page := e.page(f.Token); !strings.Contains(page, ">Expired<") {
		t.Error("the checkout page does not say Expired")
	}
	e.settleQuiet()
	s.stop()

	// The closure is the lookup's, after the invoice's expiry plus the grace.
	st, err := store.Open(ctx, filepath.Join(e.dataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	cl, err := st.ClosuresForOrder(ctx, f.OrderID)
	st.Close()
	if err != nil || len(cl) != 1 || cl[0].Reason != store.CloseLookup {
		t.Fatalf("closures %v (err %v), want one by lookup", cl, shownErr(err))
	}
	if cl[0].At.Before(earliest) {
		t.Errorf("the invoice was closed before its expiry plus the grace")
	}
	hs := e.hashes(f.OrderID)
	if len(hs) != 1 {
		t.Fatalf("%d invoices, want 1", len(hs))
	}
	if row, err := e.sat.transaction(hs[0]); err == nil {
		t.Logf("MEASURED lightning=expire hub_row_state=%s", shownState(row.State))
	}
	e.safety([]want{{orderID: f.OrderID, msat: orderMsat, expired: true}})
}

// TestLightningRelayLossBefore makes the order and invoice, then stops the service. The script stops
// relay-satbooth.
func TestLightningRelayLossBefore(t *testing.T) {
	e := newLnEnv(t, "relayloss")
	s := e.start()
	o := e.createOrder("relayloss-"+randHex(t)[:12], orderMsat, 3600)
	invoice := e.pressPay(tokenOf(o))
	e.save(&lnFile{OrderID: o.ID, Token: tokenOf(o), Invoice: invoice, Msat: orderMsat})
	s.stop()
	e.safety([]want{{orderID: o.ID, msat: orderMsat}})
}

// TestLightningRelayLossDuring: relay-satbooth is down; the customer pays through the merchant's hub, which
// needs no SatBooth relay.
func TestLightningRelayLossDuring(t *testing.T) {
	e := newLnEnv(t, "relayloss")
	f := e.load()
	// Precondition: relay-satbooth really is down.
	host := strings.TrimPrefix(e.st.RelayURLs[hubSatbooth], "ws://")
	if c, err := net.DialTimeout("tcp", host, 3*time.Second); err == nil {
		c.Close()
		t.Fatal("relay-satbooth still accepts connections: the script did not stop it")
	}
	d := e.customerPays(f.Invoice)
	t.Logf("MEASURED lightning=relayloss customer_payment_call_seconds=%.2f (relay down)", d.Seconds())
}

// TestLightningRelayLossAfter: the relay is back with an empty window and the service starts: the poller finds the
// payment that no notification announced.
func TestLightningRelayLossAfter(t *testing.T) {
	e := newLnEnv(t, "relayloss")
	f := e.load()
	began := time.Now()
	s := e.start()
	d := e.waitOrder(f.OrderID, "paid", began, 3*time.Minute)
	t.Logf("MEASURED lightning=relayloss service_start_to_order_paid_seconds=%.1f (poll path; the hub's notification was lost with the relay)", d.Seconds())
	e.waitHook(f.OrderID, store.EventPaid, hookBound)
	e.settleQuiet()
	s.stop()
	hs := e.hashes(f.OrderID)
	if len(hs) != 1 {
		t.Fatalf("%d invoices, want 1", len(hs))
	}
	e.hubOnce(hs[0])
	e.safety([]want{{orderID: f.OrderID, msat: orderMsat, paid: true}})
}

// TestLightningRestartBefore: the order and invoice exist, SatBooth goes down, and the customer pays while it is
// down. Nothing is recorded yet.
func TestLightningRestartBefore(t *testing.T) {
	e := newLnEnv(t, "restart")
	s := e.start()
	o := e.createOrder("restart-"+randHex(t)[:12], orderMsat, 3600)
	invoice := e.pressPay(tokenOf(o))
	s.stop()
	d := e.customerPays(invoice)
	t.Logf("MEASURED lightning=restart customer_payment_call_seconds=%.2f (SatBooth down)", d.Seconds())
	e.save(&lnFile{OrderID: o.ID, Token: tokenOf(o), Invoice: invoice, Msat: orderMsat})
	// The hubs have the payment; SatBooth's records must not.
	e.safety([]want{{orderID: o.ID, msat: orderMsat}})
}

// TestLightningRestartAfter: a new service on the same data finds the payment and records it once.
func TestLightningRestartAfter(t *testing.T) {
	e := newLnEnv(t, "restart")
	f := e.load()
	began := time.Now()
	s := e.start()
	d := e.waitOrder(f.OrderID, "paid", began, paidBound+30*time.Second)
	t.Logf("MEASURED lightning=restart service_start_to_order_paid_seconds=%.1f (poll path)", d.Seconds())
	e.waitHook(f.OrderID, store.EventPaid, hookBound)
	e.settleQuiet()
	s.stop()
	hs := e.hashes(f.OrderID)
	if len(hs) != 1 {
		t.Fatalf("%d invoices, want 1", len(hs))
	}
	e.hubOnce(hs[0])
	e.safety([]want{{orderID: f.OrderID, msat: orderMsat, paid: true}})
}

// ---- relay loss seen by a running service ----

// cutProxyAddr is where the in-process proxy listens: the qualify container's own address, on the relay's port. The
// relay checks the URL signed into the client's AUTH against its client_relay_url byte for byte, so for this phase the
// script gives relay-satbooth the proxy's URL as its client_relay_url (TestLightningRelayCutPrepare, and back with
// TestLightningRelayCutRestore).
const (
	cutProxyAddr = qualifyIP + ":7448"
	cutProxyURL  = "ws://" + cutProxyAddr
)

func relayConfigPath(t testing.TB) string { return filepath.Join(stateDir(t), "relay-satbooth.json") }

func setRelayClientURL(t testing.TB, url string) {
	t.Helper()
	b, err := os.ReadFile(relayConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	var cfg relay.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ClientRelayURL = url
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relay.ParseConfig(out); err != nil {
		t.Fatalf("relay config: %v", shownErr(err))
	}
	if err := os.WriteFile(relayConfigPath(t), out, 0o644); err != nil { // public data, the relay container reads it
		t.Fatal(err)
	}
}

// TestLightningRelayCutPrepare: relay-satbooth's client URL becomes the proxy's; the script then recreates the relay.
func TestLightningRelayCutPrepare(t *testing.T) { setRelayClientURL(t, cutProxyURL) }

// TestLightningRelayCutRestore: back to the hub's own URL; the script recreates the relay.
func TestLightningRelayCutRestore(t *testing.T) {
	st, err := loadState(stateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	setRelayClientURL(t, st.RelayURLs[hubSatbooth])
}

// cutProxy is a TCP proxy to relay-satbooth that can cut every live connection and refuse new ones, then accept again.
type cutProxy struct {
	t      *testing.T
	target string
	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	opened int
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	p := &cutProxy{t: t, target: target, conns: map[net.Conn]struct{}{}}
	p.listen()
	t.Cleanup(p.cut)
	return p
}

func (p *cutProxy) listen() {
	ln, err := net.Listen("tcp", cutProxyAddr)
	if err != nil {
		p.t.Fatalf("proxy listen: %v", shownErr(err))
	}
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.opened++
			p.mu.Unlock()
			go p.serve(c)
		}
	}()
}

func (p *cutProxy) serve(c net.Conn) {
	up, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		c.Close()
		return
	}
	p.mu.Lock()
	if p.ln == nil { // cut while dialling
		p.mu.Unlock()
		c.Close()
		up.Close()
		return
	}
	p.conns[c], p.conns[up] = struct{}{}, struct{}{}
	p.mu.Unlock()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) { io.Copy(dst, src); dst.Close(); src.Close(); done <- struct{}{} }
	go pipe(up, c)
	go pipe(c, up)
	<-done
	<-done
	p.mu.Lock()
	delete(p.conns, c)
	delete(p.conns, up)
	p.mu.Unlock()
}

// cut stops accepting (new connections are refused) and closes every live connection.
func (p *cutProxy) cut() {
	p.mu.Lock()
	ln := p.ln
	p.ln = nil
	conns := p.conns
	p.conns = map[net.Conn]struct{}{}
	p.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for c := range conns {
		c.Close()
	}
}

func (p *cutProxy) restore() { p.listen() }

func (p *cutProxy) connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns) / 2
}

// pressPayRetry presses pay until an invoice exists: right after the relay was recreated the hub has not resubscribed
// yet, so a first make_invoice can go unanswered (the page then says busy and the customer presses again).
func (e *lnEnv) pressPayRetry(token string, bound time.Duration) string {
	e.t.Helper()
	end := time.Now().Add(bound)
	for {
		resp, err := e.hc.Post("http://"+publicAddr+"/c/"+token+"/pay", "application/x-www-form-urlencoded", nil)
		if err != nil {
			e.t.Fatal(err)
		}
		resp.Body.Close()
		if _, page := e.page(token); invoiceRe.MatchString(page) {
			return invoiceRe.FindStringSubmatch(page)[1]
		}
		if time.Now().After(end) {
			e.t.Fatal("no invoice within the bound")
		}
		time.Sleep(2 * time.Second) // a retry period for the customer's button, not a synchronisation
	}
}

// TestLightningRelayCut: the service stays up while its connection to relay-satbooth is cut (every live connection
// closed, new ones refused) and the customer pays through the merchant's hub, whose own relay (relay-merchant, in the
// merchant hub's network namespace) does not go through the proxy. While cut, for 25 s (many poll intervals: 5
// lookups a second, each failing), the order must read new, its invoice must stay open, the log must hold no ALERT and
// no order.paid may exist. After the proxy is restored the service's client reconnects and the poller finds the payment.
func TestLightningRelayCut(t *testing.T) {
	st, err := loadState(stateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	target := strings.TrimPrefix(st.RelayURLs[hubSatbooth], "ws://")
	proxy := newCutProxy(t, target)
	e := newLnEnvRelay(t, "relaycut", cutProxyURL)
	s := e.start()
	if proxy.connections() < 1 {
		t.Fatal("the service is not connected through the proxy")
	}
	o := e.createOrder("relaycut-"+randHex(t)[:12], orderMsat, 3600)
	tok := tokenOf(o)
	invoice := e.pressPayRetry(tok, 90*time.Second)

	proxy.cut()
	if n := proxy.connections(); n != 0 {
		t.Fatalf("%d connections survived the cut", n)
	}
	if c, err := net.DialTimeout("tcp", cutProxyAddr, 2*time.Second); err == nil {
		c.Close()
		t.Fatal("the cut proxy still accepts connections")
	}
	failedBefore := strings.Count(s.logs.String(), "invoice lookup failed")
	e.customerPays(invoice)

	cutFor := 25 * time.Second
	t.Logf("MEASURED lightning=relaycut customer_paid_while_cut; watching for %s", cutFor)
	watchEnd := time.Now().Add(cutFor)
	for time.Now().Before(watchEnd) {
		if got := e.order(o.ID).State; got != "new" {
			t.Fatalf("while the relay is cut the order reads %q, want new", got)
		}
		if _, page := e.page(tok); !strings.Contains(page, invoice) {
			t.Fatal("while the relay is cut the order's invoice is no longer open")
		}
		if l := s.logs.String(); strings.Contains(l, "ALERT") {
			t.Fatalf("an ALERT line while the relay is cut")
		}
		if n := e.rcv.count(o.ID, store.EventPaid); n != 0 {
			t.Fatalf("order.paid delivered while the relay is cut (%d)", n)
		}
		time.Sleep(time.Second) // a fixed observation period of a negative check
	}
	failed := strings.Count(s.logs.String(), "invoice lookup failed") - failedBefore
	t.Logf("MEASURED lightning=relaycut failed_lookups_while_cut=%d", failed)
	if failed < 1 {
		t.Error("the poller made no failing lookup while the relay was cut")
	}

	proxy.restore()
	restored := time.Now()
	d := e.waitOrder(o.ID, "paid", restored, 3*time.Minute)
	t.Logf("MEASURED lightning=relaycut restore_to_order_paid_seconds=%.1f", d.Seconds())
	e.waitHook(o.ID, store.EventPaid, hookBound)
	e.settleQuiet()
	s.stop()
	hs := e.hashes(o.ID)
	if len(hs) != 1 {
		t.Fatalf("%d invoices, want 1", len(hs))
	}
	e.hubOnce(hs[0])
	e.safety([]want{{orderID: o.ID, msat: orderMsat, paid: true}})
	ctx := context.Background()
	st2, err := store.Open(ctx, filepath.Join(e.dataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	cl, err := st2.ClosuresForOrder(ctx, o.ID)
	st2.Close()
	if err != nil || len(cl) != 0 {
		t.Errorf("closures %v (err %v), want none for a paid order inside its expiry", cl, shownErr(err))
	}
}
