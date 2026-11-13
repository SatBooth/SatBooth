//go:build qualify

package qualify

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/webhook"
)

// The release image's phases (plan 10). SatBooth runs as the shipped satbooth image in a container of its own
// (dev/regtest/compose.yml, service satbooth at 172.30.78.40), and these phases drive it over HTTP only, as a merchant and
// a customer do: the merchant API, the checkout page, and the webhooks it sends to the receiver here. This file imports
// nothing from the service package and never runs the service in-process. Every stop, start, restart and one-off run of
// the container is done by scripts/qualify.sh between the phases: the qualify container has no Docker socket.
//
// The container reads its configuration and secret files once (/state/image, written by TestImagePrepare, owned by the
// image's user, 0600), so the plaintext API keys, the webhook secret and the approval seed are kept in the harness file
// /state/image-harness.json (root's, 0600), which every phase reloads. The group runs after every in-process phase:
// the container uses the same invoice and payout connections, watch-only wallet and view-only wallet, and never runs
// beside an in-process service.
const (
	imageIP        = "172.30.78.40"
	imgPublic      = imageIP + ":9101"
	imgPrivate     = imageIP + ":9102"
	imageDirName   = "image"
	imageInCtr     = "/state/image" // the same directory, as the container sees it (the state volume at /state, read-only)
	imageHarness   = "image-harness.json"
	imageReady     = 90 * time.Second
	imageOrderMsat = 2_000_000
	imageRefund    = 1_000_000
	imageOnchain   = 100_000 // one confirmation in the tiers below
	imageTTL       = 86400   // no image order expires during the run
	// imgEventPaid is the documented webhook type (docs/webhooks.md), written out here rather than taken from the store
	// package: what the image must send is the public contract, not whatever the code currently defines.
	imgEventPaid = "order.paid"
)

// imgHook is one delivery the image's receiver accepted.
type imgHook struct {
	ID, Type, OrderID string
	Seq               int
}

// imgReceiver is the merchant's webhook endpoint for the image phases. It verifies the signature with webhook.Verify,
// the reference verifier of docs/webhooks.md, and checks the body's shape against the documented fields: an id of 32
// lowercase hex characters, a type, a seq of at least 1, an order id of 32 lowercase hex characters. It answers 204.
type imgReceiver struct {
	mu     sync.Mutex
	secret []byte
	hooks  []imgHook
	srv    *http.Server
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (r *imgReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, 8<<10))
	if err != nil || req.Method != http.MethodPost ||
		webhook.Verify(r.secret, req.Header.Get(webhook.HeaderName), body, time.Now(), webhook.RecommendedWindow) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var raw struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		Seq       int    `json:"seq"`
		OrderID   string `json:"order_id"`
		CreatedAt int64  `json:"created_at"`
	}
	dec.DisallowUnknownFields()
	if dec.Decode(&raw) != nil || !isHex32(raw.ID) || raw.Type == "" || raw.Seq < 1 || !isHex32(raw.OrderID) || raw.CreatedAt <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p := imgHook{ID: raw.ID, Type: raw.Type, OrderID: raw.OrderID, Seq: raw.Seq}
	r.mu.Lock()
	r.hooks = append(r.hooks, p)
	r.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// count is the number of deliveries of typ for the order, and of those with seq 1.
func (r *imgReceiver) count(orderID, typ string) (all, seq1 int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.hooks {
		if h.OrderID == orderID && h.Type == typ {
			all++
			if h.Seq == 1 {
				seq1++
			}
		}
	}
	return all, seq1
}

// ids are the event ids of the deliveries of typ with seq 1 for the order, in the order received.
func (r *imgReceiver) ids(orderID, typ string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, h := range r.hooks {
		if h.OrderID == orderID && h.Type == typ && h.Seq == 1 {
			out = append(out, h.ID)
		}
	}
	return out
}

// imgHarness is the harness file: what the phases share. It is regtest-only, like everything under /state.
type imgHarness struct {
	WriteKey, ReadKey, RefundWriteKey, RefundReadKey string
	WebhookSecret                                    string // hex, as in the container's webhook.secret
	WebhookSecret2                                   string // hex, the second endpoint's own secret; empty: one endpoint
	ApprovalSeed                                     string
	BudgetBeforeMsat                                 int64
	RefundMsat, RefundFeeMsat                        int64
	PaidOrder                                        string
	RestartOrder, RestartToken, RestartInvoice       string
	BackupStates                                     map[string]string // order id -> state when TestImageBackupBefore ended
	AfterBackupOrder                                 string            // TestDeployPublic's order made after its backup
}

type imgEnv struct {
	t        *testing.T
	pub, prv string // the container's public and private listeners
	st       *state
	h        *imgHarness
	path     string
	sat, mer *hubAPI
	hc       *http.Client
	rcv      *imgReceiver
	rcv2     *imgReceiver // endpoint "second" (/hook2, its own secret); nil when the harness has one
	priv     ed25519.PrivateKey
}

func harnessPath(t testing.TB) string { return filepath.Join(stateDir(t), imageHarness) }

func (e *imgEnv) save() { writeJSON0600(e.t, e.path, e.h) }

// newImgEnv loads the state and the harness file, starts the webhook receiver and, when wait is set, waits until the
// container serves its checkout page.
func newImgEnv(t *testing.T, wait bool) *imgEnv {
	t.Helper()
	return newImgEnvAt(t, harnessPath(t), imgPublic, imgPrivate, wait)
}

// newImgEnvAt is newImgEnv for a container with its own harness file and listeners (TestDeployPublic's). A harness
// without an approval seed (no refunds) leaves priv unset.
func newImgEnvAt(t *testing.T, harness, pub, prv string, wait bool) *imgEnv {
	t.Helper()
	st, err := loadState(stateDir(t))
	if err != nil {
		t.Fatalf("state (did TestBootstrap run?): %v", shownErr(err))
	}
	e := &imgEnv{t: t, st: st, path: harness, h: &imgHarness{}, pub: pub, prv: prv}
	if err := readJSONFile(e.path, e.h); err != nil {
		t.Fatalf("harness file (did its Prepare phase run?): %v", shownErr(err))
	}
	e.sat = newHubAPI(st.Hubs[hubSatbooth].URL, st.Hubs[hubSatbooth].Token)
	e.mer = newHubAPI(st.Hubs[hubMerchant].URL, st.Hubs[hubMerchant].Token)
	e.sat.ensureToken(t)
	e.mer.ensureToken(t)
	e.hc = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if e.h.ApprovalSeed != "" {
		seed, err := hex.DecodeString(e.h.ApprovalSeed)
		if err != nil || len(seed) != ed25519.SeedSize {
			t.Fatal("harness file: bad approval seed")
		}
		e.priv = ed25519.NewKeyFromSeed(seed)
	}
	secret, err := hex.DecodeString(e.h.WebhookSecret)
	if err != nil {
		t.Fatal("harness file: bad webhook secret")
	}
	e.rcv = &imgReceiver{secret: secret}
	mux := http.NewServeMux()
	mux.Handle("/hook", e.rcv)
	if e.h.WebhookSecret2 != "" {
		secret2, err := hex.DecodeString(e.h.WebhookSecret2)
		if err != nil {
			t.Fatal("harness file: bad second webhook secret")
		}
		e.rcv2 = &imgReceiver{secret: secret2}
		mux.Handle("/hook2", e.rcv2)
	}
	ln, err := net.Listen("tcp", hookAddr)
	if err != nil {
		t.Fatalf("webhook receiver: %v", shownErr(err))
	}
	e.rcv.srv = &http.Server{Handler: mux}
	go e.rcv.srv.Serve(ln)
	t.Cleanup(func() { e.rcv.srv.Close() })
	if wait {
		e.waitReady()
	}
	return e
}

// waitReady polls the checkout page's stylesheet: SatBooth opens its listeners only after every start-up check passed.
func (e *imgEnv) waitReady() {
	e.t.Helper()
	began := time.Now()
	poll(e.t, "the satbooth container serves", imageReady, 500*time.Millisecond, func() (bool, error) {
		resp, err := e.hc.Get("http://" + e.pub + "/static/style.css")
		if err != nil {
			return false, err
		}
		resp.Body.Close()
		return resp.StatusCode == 200, fmt.Errorf("status %d", resp.StatusCode)
	})
	e.t.Logf("MEASURED image ready_seconds=%.1f at %s (from the phase's start; the container was started by qualify.sh)", time.Since(began).Seconds(), e.pub)
}

// ---- HTTP: the merchant API and the checkout page of the container ----

func (e *imgEnv) call(method, path, key string, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+e.prv+path, rd)
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
	return resp.StatusCode, raw
}

// decode decodes an API answer strictly: the views are allowlists, so an unknown field fails.
func (e *imgEnv) decode(raw []byte, v any) {
	e.t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		e.t.Fatalf("unexpected response shape: %v", shownErr(err))
	}
}

func (e *imgEnv) create(body map[string]any) orderJSON {
	e.t.Helper()
	body["account_id"] = acctID
	body["expires_in"] = imageTTL
	code, raw := e.call("POST", "/v1/orders", e.h.WriteKey, body)
	if code != 201 {
		e.t.Fatalf("create order: status %d", code)
	}
	var o orderJSON
	e.decode(raw, &o)
	if o.State != "new" || o.PaidAt != nil || !strings.HasPrefix(o.CheckoutURL, "http://"+e.pub+"/c/") {
		e.t.Fatalf("created order looks wrong: state %q", o.State)
	}
	return o
}

func (e *imgEnv) ref(tag string) string { return tag + "-" + randHex(e.t)[:12] }

func (e *imgEnv) order(id string) orderJSON {
	e.t.Helper()
	code, raw := e.call("GET", "/v1/orders/"+id, e.h.ReadKey, nil)
	if code != 200 {
		e.t.Fatalf("get order: status %d", code)
	}
	var o orderJSON
	e.decode(raw, &o)
	return o
}

func (e *imgEnv) page(token string) (int, string) {
	e.t.Helper()
	resp, err := e.hc.Get("http://" + e.pub + "/c/" + token)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b)
}

// press is the customer pressing Pay (form is the rail field, empty for a one-rail order) until the page shows the
// payment detail that re matches.
func (e *imgEnv) press(token, form string, re interface{ FindStringSubmatch(string) []string }) string {
	e.t.Helper()
	var got string
	poll(e.t, "payment detail on the page", 60*time.Second, time.Second, func() (bool, error) {
		resp, err := e.hc.Post("http://"+e.pub+"/c/"+token+"/pay", "application/x-www-form-urlencoded", strings.NewReader(form))
		if err != nil {
			return false, err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			return false, fmt.Errorf("pay: status %d", resp.StatusCode)
		}
		code, page := e.page(token)
		m := re.FindStringSubmatch(page)
		if code != 200 || m == nil || !strings.Contains(page, "Waiting for payment") {
			return false, errors.New("no payment detail on the page yet")
		}
		got = m[1]
		return true, nil
	})
	return got
}

func (e *imgEnv) waitState(id, want string, since time.Time, bound time.Duration) (time.Duration, orderJSON) {
	e.t.Helper()
	var o orderJSON
	poll(e.t, "order "+want, bound, 250*time.Millisecond, func() (bool, error) {
		o = e.order(id)
		return o.State == want, fmt.Errorf("state %s", o.State)
	})
	return time.Since(since), o
}

func (e *imgEnv) waitHook(orderID, typ string) {
	e.t.Helper()
	poll(e.t, typ+" delivered", hookBound, 250*time.Millisecond, func() (bool, error) {
		n, _ := e.rcv.count(orderID, typ)
		return n >= 1, errors.New("not delivered yet")
	})
	if _, n := e.rcv.count(orderID, typ); n != 1 {
		e.t.Errorf("order %s: %d deliveries of %s seq 1 in this phase, want 1", orderID, n, typ)
	}
	if e.rcv2 == nil {
		return
	}
	// The second endpoint gets the same event. Its receiver answers 400 to a request signed with any other secret, so a
	// counted delivery was signed with this endpoint's own secret.
	poll(e.t, typ+" delivered to the second endpoint", hookBound, 250*time.Millisecond, func() (bool, error) {
		n, _ := e.rcv2.count(orderID, typ)
		return n >= 1, errors.New("not delivered yet")
	})
	if _, n := e.rcv2.count(orderID, typ); n != 1 {
		e.t.Errorf("order %s: %d deliveries of %s seq 1 to the second endpoint, want 1", orderID, n, typ)
	}
	// Both endpoints are sent the same event, so the id they received is the same.
	if a, b := e.rcv.ids(orderID, typ), e.rcv2.ids(orderID, typ); len(a) != 1 || len(b) != 1 || a[0] != b[0] {
		e.t.Errorf("order %s: the event ids of %s differ between the endpoints: %v and %v", orderID, typ, a, b)
	}
}

// pageHas polls the checkout page until it carries want.
func (e *imgEnv) pageHas(token, want string) {
	e.t.Helper()
	poll(e.t, "page says "+want, 20*time.Second, 500*time.Millisecond, func() (bool, error) {
		code, page := e.page(token)
		return code == 200 && strings.Contains(page, want), fmt.Errorf("status %d", code)
	})
}

// lnOrder creates a Lightning order and presses Pay; it returns the order and the invoice.
func (e *imgEnv) lnOrder(tag string) (orderJSON, string) {
	e.t.Helper()
	o := e.create(map[string]any{"merchant_ref": e.ref(tag), "lightning_msat": imageOrderMsat})
	return o, e.press(tokenOf(o), "", invoiceRe)
}

// customerPays pays the invoice from the merchant's hub (the customer) and checks both hubs' rows: one settled
// incoming row at SatBooth's hub, one settled outgoing row at the merchant's.
func (e *imgEnv) customerPays(bolt11 string, msat int64) {
	e.t.Helper()
	tx, err := e.mer.payInvoice(bolt11)
	if err != nil {
		e.t.Fatalf("the customer's payment failed: %v", shownErr(err))
	}
	if tx.State != "settled" {
		e.t.Fatalf("the customer's payment is %s, want settled", shownState(tx.State))
	}
	e.oneRow(e.sat, "SatBooth's", tx.PaymentHash, "incoming", msat)
	e.oneRow(e.mer, "the merchant's", tx.PaymentHash, "outgoing", msat)
}

// oneRow requires exactly one settled row of typ and msat at the hub for the payment hash, and returns its fee.
func (e *imgEnv) oneRow(h *hubAPI, name, hash, typ string, msat int64) int64 {
	e.t.Helper()
	rs, err := h.rowsFor(hash)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Type != typ || rs[0].State != "settled" || int64(rs[0].AmountMsat) != msat {
		e.t.Errorf("%s hub: rows %v, want one %s settled of %d msat", name, rowStates(rs), typ, msat)
		return 0
	}
	return int64(rs[0].FeesMsat)
}

// payLN is a whole Lightning payment: order, press, pay, paid, the order.paid webhook. It returns the order id.
func (e *imgEnv) payLN(tag string) string {
	e.t.Helper()
	o, inv := e.lnOrder(tag)
	began := time.Now()
	e.customerPays(inv, imageOrderMsat)
	d, _ := e.waitState(o.ID, "paid", began, 30*time.Second)
	e.t.Logf("MEASURED image lightning %s pay_to_paid_seconds=%.2f", tag, d.Seconds())
	e.waitHook(o.ID, imgEventPaid)
	e.pageHas(tokenOf(o), ">Paid<")
	return o.ID
}

// payoutBudget reads the payout connection's budget on a connection of its own. The container must not be running:
// it holds the payout connection.
func payoutBudget(t *testing.T, st *state) *nwc.Budget {
	t.Helper()
	k := st.key(t, "payout")
	c, err := nwc.Dial(ctxFor(t, time.Minute), nwc.Config{RelayURL: st.RelayURLs[hubSatbooth], WalletPubKey: st.Wallets["payout"], Key: k, Timeout: 70 * time.Second})
	if err != nil {
		t.Fatalf("dial payout: %v", shownErr(err))
	}
	defer c.Close()
	b, err := c.GetBudget(ctxFor(t, time.Minute))
	if err != nil || b == nil || !b.Set {
		t.Fatalf("payout budget: %+v %v", b, shownErr(err))
	}
	t.Logf("MEASURED image payout_budget used_msat=%d total_msat=%d renewal=%s", b.UsedMsat, b.TotalMsat, shownRenewal(b.RenewalPeriod))
	return b
}

// ---- phases ----

// TestBuildFlags: the qualify binary, which runs the in-process phases, is built with the release image's flags
// (deploy/image/Dockerfile): -trimpath, CGO_ENABLED=0 and the vendored modules, on Linux. -trimpath and CGO_ENABLED are
// build settings that debug.ReadBuildInfo records. -mod=vendor is not one: it is read from GOFLAGS, which the go command
// ran with, and from the dependencies, which carry a go.sum hash in a module-mode build and none in a vendor build.
func TestBuildFlags(t *testing.T) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("no build information in the test binary")
	}
	set := map[string]string{}
	for _, s := range bi.Settings {
		set[s.Key] = s.Value
	}
	for k, want := range map[string]string{"-trimpath": "true", "CGO_ENABLED": "0", "GOOS": "linux"} {
		if set[k] != want {
			t.Errorf("build setting %s = %q, want %q", k, set[k], want)
		}
	}
	flags := strings.Fields(os.Getenv("GOFLAGS"))
	for _, want := range []string{"-mod=vendor", "-trimpath"} {
		if !slices.Contains(flags, want) {
			t.Errorf("GOFLAGS lacks %s", want)
		}
	}
	if len(bi.Deps) == 0 {
		t.Fatal("no dependencies in the build information")
	}
	for _, d := range bi.Deps {
		if d.Sum != "" {
			t.Errorf("dependency %s carries a go.sum hash: not a vendor build", d.Path)
		}
	}
	t.Logf("MEASURED build go=%s -trimpath=%s CGO_ENABLED=%s GOOS=%s GOARCH=%s deps=%d (none with a go.sum hash) GOFLAGS=%q",
		bi.GoVersion, set["-trimpath"], set["CGO_ENABLED"], set["GOOS"], set["GOARCH"], len(bi.Deps), strings.Join(flags, " "))
}

// TestImagePrepare writes the container's configuration and secret files and the harness file, and reads the payout
// budget before the container ever runs. It needs TestOnchainSetup and TestMoneroSetup (the descriptor and the merchant's
// Monero address). The configuration is written as an operator writes it, from plain JSON: the container's own parser
// checks it. It has every rail but fiat pricing (whose providers are https services the closed network cannot reach).
func TestImagePrepare(t *testing.T) {
	st, err := loadState(stateDir(t))
	if err != nil {
		t.Fatalf("state (did TestBootstrap run?): %v", shownErr(err))
	}
	var oc ocSetup
	if err := readJSONFile(filepath.Join(stateDir(t), ocName, "setup.json"), &oc); err != nil || oc.Descriptor == "" {
		t.Fatalf("on-chain setup (did TestOnchainSetup run?): %v", shownErr(err))
	}
	var xm xmSetup
	if err := readJSONFile(filepath.Join(stateDir(t), xmName, "setup.json"), &xm); err != nil || xm.Merchant == "" {
		t.Fatalf("Monero setup (did TestMoneroSetup run?): %v", shownErr(err))
	}
	rpcUser, rpcPass := os.Getenv("SATBOOTH_RPC_USER"), os.Getenv("SATBOOTH_RPC_PASSWORD")
	if rpcUser == "" || rpcPass == "" {
		t.Fatal("SATBOOTH_RPC_USER or SATBOOTH_RPC_PASSWORD is not set")
	}
	xmUser, xmPass := xmLogin(t)
	var seed [ed25519.SeedSize]byte
	if _, err := rand.Read(seed[:]); err != nil {
		t.Fatal(err)
	}
	h := &imgHarness{WriteKey: randHex(t), ReadKey: randHex(t), RefundWriteKey: randHex(t), RefundReadKey: randHex(t),
		WebhookSecret: randHex(t), WebhookSecret2: randHex(t), ApprovalSeed: hex.EncodeToString(seed[:])}
	priv := ed25519.NewKeyFromSeed(seed[:])
	if h.WebhookSecret == h.WebhookSecret2 {
		t.Fatal("the two webhook secrets are equal")
	}
	h.BudgetBeforeMsat = payoutBudget(t, st).UsedMsat

	invoiceKey, err := os.ReadFile(st.KeyFiles["invoice"])
	if err != nil {
		t.Fatal(err)
	}
	payoutKey, err := os.ReadFile(st.KeyFiles["payout"])
	if err != nil {
		t.Fatal(err)
	}
	keys, err := json.Marshal(map[string]any{"keys": []map[string]string{
		{"id": "qualify-write", "capability": "orders:write", "sha256": sha256Hex(h.WriteKey)},
		{"id": "qualify-read", "capability": "orders:read", "sha256": sha256Hex(h.ReadKey)},
		{"id": "qualify-refunds-write", "capability": "refunds:write", "sha256": sha256Hex(h.RefundWriteKey)},
		{"id": "qualify-refunds-read", "capability": "refunds:read", "sha256": sha256Hex(h.RefundReadKey)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string][]byte{
		"nwc.key": invoiceKey, "payout.key": payoutKey, "api-keys.json": keys,
		"webhook.secret": []byte(h.WebhookSecret + "\n"), "webhook2.secret": []byte(h.WebhookSecret2 + "\n"), "rpc.pass": []byte(rpcPass + "\n"), "xmr-rpc.pass": []byte(xmPass + "\n"),
	}
	in := func(name string) string { return imageInCtr + "/" + name }
	tier := func(key string, upTo int64, confs int) map[string]any {
		return map[string]any{key: upTo, "confirmations": confs}
	}
	relayURL := st.RelayURLs[hubSatbooth]
	cfg := map[string]any{
		"public_listen": imgPublic, "private_listen": imgPrivate, "checkout_base_url": "http://" + imgPublic,
		"data_dir": "/data",
		"nwc": map[string]any{"relay_url": relayURL, "wallet_pubkey": st.Wallets["invoice"], "client_key_file": in("nwc.key"),
			"timeout_seconds": 10, "network": "regtest"},
		"api_keys_file": in("api-keys.json"),
		"webhooks": []any{
			map[string]any{"name": "main", "url": "http://" + hookAddr + "/hook", "secret_file": in("webhook.secret")},
			map[string]any{"name": "second", "url": "http://" + hookAddr + "/hook2", "secret_file": in("webhook2.secret")}},
		"limits":                 map[string]any{"daily_msat": 1_000_000_000, "max_msat": 100_000_000},
		"invoice_expiry_minutes": 15,
		"onchain": map[string]any{"rpc_url": bitcoindURL, "rpc_user": rpcUser, "rpc_password_file": in("rpc.pass"),
			"wallet": watchWallet, "descriptor": oc.Descriptor, "network": "regtest", "poll_seconds": ocPollSeconds,
			"confirmations": []any{tier("up_to_sat", 10_000, 0), tier("up_to_sat", 1_000_000, 1), tier("up_to_sat", 10_000_000, 3),
				tier("up_to_sat", 0, 6)}},
		"monero": map[string]any{"wallet_rpc_url": walletSatbooth, "rpc_user": xmUser, "rpc_password_file": in("xmr-rpc.pass"),
			"address": xm.Merchant, "network": "regtest", "poll_seconds": xmPollSeconds,
			"confirmations": []any{tier("up_to_piconero", 1_000_000_000_000, 1), tier("up_to_piconero", 10_000_000_000_000, 3),
				tier("up_to_piconero", 0, 10)}},
		"payout": map[string]any{"relay_url": relayURL, "wallet_pubkey": st.Wallets["payout"], "client_key_file": in("payout.key"),
			"timeout_seconds": 70, "network": "regtest",
			"approval_keys": []string{hex.EncodeToString(priv.Public().(ed25519.PublicKey))},
			"max_msat":      100_000_000, "daily_msat": 400_000_000},
	}
	if err := writeImageFiles(filepath.Join(stateDir(t), imageDirName), "satbooth-image.json", secrets, cfg); err != nil {
		t.Fatalf("writing the container's files: %v", shownErr(err))
	}
	writeJSON0600(t, harnessPath(t), h)
	names := []string{"satbooth-image.json"}
	for n := range secrets {
		names = append(names, n)
	}
	slices.Sort(names)
	t.Logf("image files written to %s, owner %d, mode 0600: %s", imageInCtr, imageUID, strings.Join(names, " "))
}

// TestImageStart: the container started (qualify.sh) and serves, so every start-up check passed against the real hubs,
// bitcoind and wallet-rpc: SatBooth opens its listeners only after them (qualify.sh checks the log lines). The API takes
// the keys from the image's files and refuses others; each rail takes an order.
func TestImageStart(t *testing.T) {
	e := newImgEnv(t, true)
	if code, _ := e.call("GET", "/v1/orders/"+randHex(t)[:32], e.h.ReadKey, nil); code != 404 {
		t.Errorf("an unknown order with the read key: status %d, want 404", code)
	}
	if code, _ := e.call("GET", "/v1/orders/"+randHex(t)[:32], randHex(t), nil); code != 401 {
		t.Errorf("an unknown key: status %d, want 401", code)
	}
	ln := e.create(map[string]any{"merchant_ref": e.ref("image-start-ln"), "lightning_msat": imageOrderMsat})
	oc := e.create(map[string]any{"merchant_ref": e.ref("image-start-oc"), "onchain_sat": imageOnchain})
	xm := e.create(map[string]any{"merchant_ref": e.ref("image-start-xm"), "monero_piconero": xmT1})
	if ln.Lightning == nil || oc.Onchain == nil || oc.Onchain.RequiredConfirmations != 1 || xm.Monero == nil || xm.Monero.RequiredConfirmations != 1 {
		t.Fatalf("the rails' orders look wrong: lightning %v, on-chain %+v, Monero %+v", ln.Lightning != nil, oc.Onchain, xm.Monero)
	}
	if code, _ := e.page(tokenOf(ln)); code != 200 {
		t.Errorf("checkout page: status %d", code)
	}
	t.Log("the container serves the API and the checkout page with all three rails and refunds configured")
}

func TestImageLightningPay(t *testing.T) {
	e := newImgEnv(t, true)
	e.h.PaidOrder = e.payLN("image-ln")
	e.save()
}

func TestImageOnchainPay(t *testing.T) {
	e := newImgEnv(t, true)
	btc := newBitcoindRPC(t)
	o := e.create(map[string]any{"merchant_ref": e.ref("image-oc"), "onchain_sat": imageOnchain})
	addr := e.press(tokenOf(o), "", addrRe)
	if got := e.order(o.ID); got.Onchain == nil || got.Onchain.Address == nil || *got.Onchain.Address != addr {
		t.Fatalf("the API's address differs from the page's")
	}
	if _, err := btc.sendRBF(minerWallet, addr, btcrpc.FormatBTC(imageOnchain)); err != nil {
		t.Fatalf("sendtoaddress: %v", shownErr(err))
	}
	poll(t, "on-chain seen", ocBound, 250*time.Millisecond, func() (bool, error) {
		x := e.order(o.ID)
		return x.Onchain.State == "seen" && x.Onchain.ReceivedSat == imageOnchain, fmt.Errorf("on-chain %+v", *x.Onchain)
	})
	mined := time.Now()
	if err := btc.mine(1); err != nil {
		t.Fatal(err)
	}
	d, _ := e.waitState(o.ID, "paid", mined, ocBound)
	t.Logf("MEASURED image onchain block_to_paid_seconds=%.2f (poll 2 s)", d.Seconds())
	e.waitHook(o.ID, imgEventPaid)
	got, err := btc.getReceivedByAddress(merchantWallet, addr, 1)
	if err != nil || got != imageOnchain {
		t.Errorf("the merchant's keyed wallet received %d sat at the address (%v), want %d", got, shownErr(err), imageOnchain)
	}
}

func TestImageMoneroPay(t *testing.T) {
	e := newImgEnv(t, true)
	var su xmSetup
	if err := readJSONFile(filepath.Join(stateDir(t), xmName, "setup.json"), &su); err != nil {
		t.Fatal(err)
	}
	x := &xmEnv{su: su, cw: &custWallet{}, ocEnv: &ocEnv{lnEnv: &lnEnv{t: t}}}
	o := e.create(map[string]any{"merchant_ref": e.ref("image-xm"), "monero_piconero": xmT1})
	sub := e.press(tokenOf(o), "rail=monero", subRe)
	if got := e.order(o.ID); got.Monero == nil || got.Monero.Address == nil || *got.Monero.Address != sub {
		t.Fatalf("the API's subaddress differs from the page's")
	}
	x.payXM(sub, xmT1, 0)
	poll(t, "Monero seen", xmBound, time.Second, func() (bool, error) {
		y := e.order(o.ID)
		return y.Monero.State == "seen" && y.Monero.ReceivedPiconero == xmT1, fmt.Errorf("Monero %+v", *y.Monero)
	})
	mined := time.Now()
	x.mineXM(1)
	d, _ := e.waitState(o.ID, "paid", mined, xmBound)
	t.Logf("MEASURED image monero block_to_paid_seconds=%.2f (wallet-rpc refreshes every 20 s)", d.Seconds())
	e.waitHook(o.ID, imgEventPaid)
	var sum int64
	for _, r := range x.merchantReceipts()[sub] {
		sum += int64(r.Amount)
	}
	if sum != xmT1 {
		t.Errorf("the merchant's full wallet received %d piconero at the subaddress, want %d", sum, xmT1)
	}
}

// TestImageRefund: a claim of half a paid order settles; both hubs' rows show the amount paid once.
func TestImageRefund(t *testing.T) {
	e := newImgEnv(t, true)
	orderID := e.payLN("image-refund")
	inv, err := e.mer.makeInvoice(imageRefund/1000, "image refund")
	if err != nil || inv.Invoice == "" {
		t.Fatalf("the claimant's invoice: %v", shownErr(err))
	}
	var r approval.Refund
	if _, err := rand.Read(r.RefundID[:]); err != nil {
		t.Fatal(err)
	}
	ob, err := hex.DecodeString(orderID)
	if err != nil || len(ob) != 16 {
		t.Fatalf("order id %q", orderID)
	}
	copy(r.OrderID[:], ob)
	r.Network, r.KeyID = approval.Regtest, approval.KeyID(e.priv.Public().(ed25519.PublicKey))
	r.AmountMsat, r.ExpiresAt = imageRefund, time.Now().Add(time.Hour).Unix()
	id := hex.EncodeToString(r.RefundID[:])
	began := time.Now()
	code, raw := e.call("POST", "/v1/refunds", e.h.RefundWriteKey, map[string]string{"approval": approval.SignRefund(r, e.priv), "invoice": inv.Invoice})
	if code != 202 {
		t.Fatalf("claim: status %d, want 202", code)
	}
	var v refundJSON
	e.decode(raw, &v)
	if v.ID != id || v.OrderID != orderID || v.AmountMsat != imageRefund || v.State != "claimed" {
		t.Fatalf("claim: view %+v", v)
	}
	poll(t, "refund settled", refundBound, 250*time.Millisecond, func() (bool, error) {
		code, raw := e.call("GET", "/v1/refunds/"+id, e.h.RefundReadKey, nil)
		if code != 200 {
			return false, fmt.Errorf("status %d", code)
		}
		e.decode(raw, &v)
		return v.State == "settled", fmt.Errorf("state %s reason %q", v.State, v.Reason)
	})
	t.Logf("MEASURED image refund claim_to_settled_seconds=%.1f", time.Since(began).Seconds())
	fee := e.oneRow(e.sat, "SatBooth's", inv.PaymentHash, "outgoing", imageRefund)
	e.oneRow(e.mer, "the merchant's", inv.PaymentHash, "incoming", imageRefund)
	o := e.order(orderID)
	if l := o.Lightning; l == nil || l.RefundedMsat == nil || *l.RefundedMsat != imageRefund || l.RefundOpenMsat == nil || *l.RefundOpenMsat != 0 {
		t.Errorf("order figures after the refund: %+v", o.Lightning)
	}
	e.h.RefundMsat, e.h.RefundFeeMsat = imageRefund, fee
	e.save()
}

// TestImageSecondContainerRefused runs after qualify.sh started a second satbooth container on the same data volume and
// saw it exit 1 with "another SatBooth process holds the data directory": the first container still serves, reads and
// writes.
func TestImageSecondContainerRefused(t *testing.T) {
	e := newImgEnv(t, true)
	if o := e.order(e.h.PaidOrder); o.State != "paid" {
		t.Errorf("the paid order reads %s", o.State)
	}
	e.create(map[string]any{"merchant_ref": e.ref("image-after-second"), "lightning_msat": imageOrderMsat})
}

// TestImageRestartBefore opens an invoice; qualify.sh then stops the container (exit 0) and starts it again.
func TestImageRestartBefore(t *testing.T) {
	e := newImgEnv(t, true)
	o, inv := e.lnOrder("image-restart")
	e.h.RestartOrder, e.h.RestartToken, e.h.RestartInvoice = o.ID, tokenOf(o), inv
	e.save()
}

// TestImageRestartAfter: the restarted container shows the same invoice, takes its payment and reads paid.
func TestImageRestartAfter(t *testing.T) {
	e := newImgEnv(t, true)
	if o := e.order(e.h.RestartOrder); o.State != "new" {
		t.Fatalf("the open order reads %s after the restart", o.State)
	}
	if _, page := e.page(e.h.RestartToken); !strings.Contains(page, e.h.RestartInvoice) {
		t.Fatal("the page no longer carries the invoice opened before the restart")
	}
	began := time.Now()
	e.customerPays(e.h.RestartInvoice, imageOrderMsat)
	d, _ := e.waitState(e.h.RestartOrder, "paid", began, 30*time.Second)
	t.Logf("MEASURED image restart pay_to_paid_seconds=%.2f", d.Seconds())
	e.waitHook(e.h.RestartOrder, imgEventPaid)
}

// TestImageBackupBefore makes orders (one paid, two open) and records the state of every image order made so far;
// qualify.sh then stops the container, backs up, restores the backup and starts it.
func TestImageBackupBefore(t *testing.T) {
	e := newImgEnv(t, true)
	paid := e.payLN("image-backup")
	open1, _ := e.lnOrder("image-backup-open")
	open2 := e.create(map[string]any{"merchant_ref": e.ref("image-backup-new"), "onchain_sat": imageOnchain})
	e.h.BackupStates = map[string]string{}
	for _, id := range []string{e.h.PaidOrder, e.h.RestartOrder, paid, open1.ID, open2.ID} {
		e.h.BackupStates[id] = e.order(id).State
	}
	e.save()
	t.Logf("recorded %d orders for the backup", len(e.h.BackupStates))
}

// TestImageBackupAfter: after a restore of one backup's pair, the container serves the same orders in the same states,
// and takes a new one.
func TestImageBackupAfter(t *testing.T) {
	e := newImgEnv(t, true)
	if len(e.h.BackupStates) == 0 {
		t.Fatal("no orders recorded (did TestImageBackupBefore run?)")
	}
	for id, want := range e.h.BackupStates {
		if got := e.order(id).State; got != want {
			t.Errorf("order %s reads %s after the restore, want %s", id, got, want)
		}
	}
	e.create(map[string]any{"merchant_ref": e.ref("image-after-restore"), "lightning_msat": imageOrderMsat})
}

// TestImageBudget runs once the container is stopped for good: the payout budget moved by exactly the image's refund
// and its fee.
func TestImageBudget(t *testing.T) {
	st, err := loadState(stateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	var h imgHarness
	if err := readJSONFile(harnessPath(t), &h); err != nil {
		t.Fatal(err)
	}
	if h.RefundMsat != imageRefund {
		t.Fatalf("the harness file records a refund of %d msat, want %d (did TestImageRefund run to its end?)", h.RefundMsat, imageRefund)
	}
	b := payoutBudget(t, st)
	if want := h.BudgetBeforeMsat + h.RefundMsat + h.RefundFeeMsat; b.UsedMsat != want {
		t.Errorf("payout used budget %d, want %d (before %d + refund %d + fee %d)", b.UsedMsat, want, h.BudgetBeforeMsat, h.RefundMsat, h.RefundFeeMsat)
	}
	t.Logf("MEASURED image payout_budget before_msat=%d after_msat=%d total_msat=%d", h.BudgetBeforeMsat, b.UsedMsat, b.TotalMsat)
}
