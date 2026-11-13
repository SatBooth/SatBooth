//go:build qualify

package qualify

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/checkout"
	"github.com/satbooth/satbooth/descriptor"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/qr"
	"github.com/satbooth/satbooth/service"
	"github.com/satbooth/satbooth/store"
)

// The on-chain phases run the whole service in-process against the regtest bitcoind, as the Lightning phases do.
// Unlike them, every on-chain phase shares one environment (/state/onchain, one data directory): the address index
// must be continuous across the phases, and later phases read orders made by earlier ones. Each phase ends with the
// safety check over EVERY order made so far (the registry, orders.json):
//   - the events in the store against the expected range per type, seq contiguous, each delivered with exactly one
//     successful attempt, and the receiver of this process seeing exactly the deliveries made in it;
//   - the ledger transfers against the chain: per output the forward and reversal transfers of every epoch, paired,
//     and the net per address equal to the merchant's own keyed wallet's getreceivedbyaddress at the order's depth;
//   - the ledger accounts equal to the sums of those transfers;
//   - every ALERT line expected, in the expected number, and no other.
// A phase that cannot observe its property fails: nothing here logs and passes.

const (
	ocName         = "onchain"
	merchantWallet = "merchant"
	watchWallet    = "satbooth-watch"
	watchTrWallet  = "satbooth-watch-tr"
	ocBound        = 45 * time.Second
	ocPollSeconds  = 2               // the service's poll_seconds in these phases
	ocQuiet        = 8 * time.Second // four polls of 2 s: a negative check has to wait; it is not a synchronisation

	satT0 = 5_000      // up to 10,000 sat: 0 confirmations (paid from the mempool)
	satT1 = 100_000    // up to 1,000,000 sat: 1 confirmation
	satT2 = 2_000_000  // up to 10,000,000 sat: 3 confirmations
	satT3 = 12_000_000 // above: 6 confirmations
)

// The ALERT lines of the on-chain rail (onchain/settle.go, onchain/service.go): the safety check compares them
// exactly, so a change of wording fails here.
const (
	alReorg     = "ALERT: an on-chain payment was reverted by a reorg"
	alPaidAgain = "ALERT: a reverted on-chain payment confirmed again"
	alLate      = "ALERT: a late on-chain payment arrived after the order's expiry"
	alUnderpaid = "ALERT: an order was underpaid at expiry"
	// alUnderpaidWindow is the other line of an order.underpaid: the whole amount seen on time, not confirmed enough at
	// the window end.
	alUnderpaidWindow = "ALERT: an order was underpaid at the end of its watch window; its payment is not yet confirmed enough and stays tracked"
	// alPaidAfterFinal is expected explicitly by the phases that cause it.
	alPaidAfterFinal = "ALERT: an order was paid after its order.expired or order.underpaid event; the payment is recorded and the order reads paid"
	alOverpaid       = "ALERT: an order was overpaid; refund the excess"
	alBehind         = "ALERT: wallet next_index is behind SatBooth's record; addresses are not issued"
	// alWalletBehind is onchain/watch.go's logWalletBehind, a WARN: bitcoind's wallet is a little behind its node, so any
	// phase that bumps, diverts or evicts a payment may log it, any number of times, with nothing wrong.
	alWalletBehind = "bitcoind's wallet has not yet processed a transaction in the node's mempool that replaces a payment; the order is judged when it has"
)

var eventTypes = []string{store.EventPaid, store.EventExpired, store.EventUnderpaid, store.EventLate, store.EventReverted, store.EventPaidAgain}

type rng struct{ Lo, Hi int }

// ocOrder is one order of the registry with what the safety check expects of it.
type ocOrder struct {
	ID       string         `json:"id"`
	Token    string         `json:"token"`
	Tag      string         `json:"tag"`
	Address  string         `json:"address"`
	Sat      int64          `json:"sat"`
	Required int            `json:"required"`
	Expiry   int64          `json:"expiry"`
	Ev       map[string]rng `json:"ev"`   // events that must exist (Lo) and may exist (Hi) whatever the time
	Post     map[string]int `json:"post"` // events that follow the order's expiry in any service running past it
	Emitted  bool           `json:"emitted"`
	TxID     string         `json:"txid,omitempty"` // the payment a later phase reads (the stuck pair)
	// The Monero side (qualify/monero_test.go): the issued subaddress, the amount and its confirmations.
	Sub  string `json:"subaddress,omitempty"`
	Pico int64  `json:"pico,omitempty"`
	XReq int    `json:"xreq,omitempty"`
}

type registry struct {
	Orders []*ocOrder `json:"orders"`
}

type ocSetup struct {
	Descriptor string `json:"descriptor"` // the merchant's wpkh descriptor, as the service is configured with
	TrDesc     string `json:"tr_descriptor"`
}

type ocEnv struct {
	*lnEnv
	btc      *bitcoindRPC
	setup    ocSetup
	reg      registry
	t0       time.Time
	runStart time.Time
	snap     map[evKey]bool
	// lateLine and againLine are the ALERT lines that go with an order.late and an order.paid_again; empty: the
	// on-chain ones (alLate, alPaidAgain). The Monero environments set the Monero rail's.
	lateLine, againLine string
}

type evKey struct {
	order, typ string
	seq        int
}

func ocRoot(t testing.TB) string {
	root := filepath.Join(stateDir(t), ocName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// tier is one confirmation tier of the service's configuration.
func tier(upTo int64, confs int) service.TierConfig {
	return service.TierConfig{UpToSat: upTo, Confirmations: &confs}
}

// ocTiers are the tiers of every on-chain phase: a 0 tier for small orders (satT0), then the defaults' sizes (satT1 to
// satT3 keep 1, 3 and 6 confirmations).
func ocTiers() []service.TierConfig {
	return []service.TierConfig{tier(10_000, 0), tier(1_000_000, 1), tier(10_000_000, 3), tier(0, 6)}
}

// newOcEnv prepares the shared on-chain environment: the service's files, its RPC password file (0600, written from
// the environment, never printed), the registry of orders made so far.
func newOcEnv(t *testing.T, needSetup bool) *ocEnv {
	t.Helper()
	return newOcEnvAt(t, ocName, ocRoot(t), needSetup, nil)
}

// newOcEnvIn is newOcEnv(t, true) for another environment at /state/<name>, with its own data directory and registry,
// the same watch-only wallet and descriptor, and the service's clock (and the receiver's) shifted by shift.
func newOcEnvIn(t *testing.T, name string, shift time.Duration) *ocEnv {
	t.Helper()
	root := filepath.Join(stateDir(t), name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	e := newOcEnvAt(t, name, root, true, nil)
	if shift != 0 {
		e.setClock(func() time.Time { return time.Now().Add(shift) })
		e.t0 = e.now()
	}
	return e
}

// now is the service's clock.
func (e *ocEnv) now() time.Time {
	if e.clock != nil {
		return e.clock()
	}
	return time.Now()
}

// newOcEnvAt prepares an on-chain environment in root; rates, when set, adds fiat pricing to its configuration.
func newOcEnvAt(t *testing.T, name, root string, needSetup bool, rates *service.RatesConfig) *ocEnv {
	t.Helper()
	return newOcEnvWith(t, name, root, needSetup, rates, nil)
}

// newOcEnvWith is newOcEnvAt with the Monero rail configured when monero is set (the Monero phases' environments,
// whose registry, events and on-chain side this harness checks; qualify/monero_test.go adds the Monero side).
func newOcEnvWith(t *testing.T, name, root string, needSetup bool, rates *service.RatesConfig, monero *service.MoneroConfig) *ocEnv {
	t.Helper()
	var su ocSetup
	if err := readJSONFile(filepath.Join(ocRoot(t), "setup.json"), &su); err != nil && needSetup {
		t.Fatalf("setup file (did TestOnchainSetup run?): %v", shownErr(err))
	}
	pass := os.Getenv("SATBOOTH_RPC_PASSWORD")
	if pass == "" || os.Getenv("SATBOOTH_RPC_USER") == "" {
		t.Fatal("SATBOOTH_RPC_USER or SATBOOTH_RPC_PASSWORD is not set")
	}
	passFile := filepath.Join(root, "rpc.pass")
	if err := os.WriteFile(passFile, []byte(pass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var oc *service.OnchainConfig
	if su.Descriptor != "" {
		oc = &service.OnchainConfig{RPCURL: bitcoindURL, RPCUser: os.Getenv("SATBOOTH_RPC_USER"), RPCPasswordFile: passFile,
			Wallet: watchWallet, Descriptor: su.Descriptor, Network: "regtest", PollSeconds: ocPollSeconds,
			Confirmations: ocTiers()}
	}
	e := &ocEnv{lnEnv: newEnvAt(t, name, root, "", oc, rates, monero), btc: newBitcoindRPC(t), setup: su, t0: time.Now()}
	if err := readJSONFile(filepath.Join(root, "orders.json"), &e.reg); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return e
}

// height, blockHash and newAddr fail the test on an error: a swallowed error would make a negative check pass.
func (e *ocEnv) height() int {
	e.t.Helper()
	h, err := e.btc.getBlockCount()
	if err != nil || h < 1 {
		e.t.Fatalf("getblockcount: %d, %v", h, shownErr(err))
	}
	return h
}

func (e *ocEnv) blockHash(height int) string {
	e.t.Helper()
	h, err := e.btc.getBlockHash(height)
	if err != nil {
		e.t.Fatalf("getblockhash %d: %v", height, shownErr(err))
	}
	return h
}

func (e *ocEnv) newAddr() string {
	e.t.Helper()
	a, err := e.btc.getNewAddress()
	if err != nil {
		e.t.Fatalf("getnewaddress: %v", shownErr(err))
	}
	return a
}

func (e *ocEnv) saveReg() { writeJSON0600(e.t, filepath.Join(e.root, "orders.json"), e.reg) }

func (e *ocEnv) tagged(tag string) *ocOrder {
	e.t.Helper()
	var found *ocOrder
	for _, o := range e.reg.Orders {
		if o.Tag == tag {
			if found != nil {
				e.t.Fatalf("two orders tagged %q", tag)
			}
			found = o
		}
	}
	if found == nil {
		e.t.Fatalf("no order tagged %q (did its Before phase run?)", tag)
	}
	return found
}

func (e *ocEnv) byID(id string) *ocOrder {
	for _, o := range e.reg.Orders {
		if o.ID == id {
			return o
		}
	}
	e.t.Fatalf("order %s is not in the registry", id)
	return nil
}

// snapshot reads the events of every registered order from the store (the service must not be running).
func (e *ocEnv) snapshot() map[evKey]bool {
	snap := map[evKey]bool{}
	ctx := context.Background()
	p := filepath.Join(e.dataDir, "store.db")
	if _, err := os.Stat(p); err != nil {
		return snap
	}
	st, err := store.Open(ctx, p)
	if err != nil {
		e.t.Fatalf("snapshot: store: %v", shownErr(err))
	}
	defer st.Close()
	for _, o := range e.reg.Orders {
		evs, err := st.EventsForOrder(ctx, o.ID)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, ev := range evs {
			snap[evKey{o.ID, ev.Type, ev.Seq}] = true
		}
	}
	return snap
}

// start takes the event snapshot and starts the service.
func (e *ocEnv) start() *svcRun {
	e.t.Helper()
	e.snap = e.snapshot()
	e.runStart = time.Now()
	return e.lnEnv.start()
}

// ---- orders and payments ----

var addrRe = regexp.MustCompile(`id="address"[^>]*value="(bcrt1[a-z0-9]+)"`)

// newOrder creates an order (on-chain sat, and Lightning msat when above 0) and registers it.
func (e *ocEnv) newOrder(tag string, sat, msat, expiresIn int64, ev map[string]rng, post map[string]int) (*ocOrder, orderJSON) {
	t := e.t
	t.Helper()
	body := map[string]any{"account_id": acctID, "merchant_ref": tag + "-" + randHex(t)[:12], "onchain_sat": sat, "expires_in": expiresIn}
	if msat > 0 {
		body["lightning_msat"] = msat
	}
	var o orderJSON
	if code := e.api("POST", "/v1/orders", e.writeKey, body, &o); code != 201 {
		t.Fatalf("create order: status %d", code)
	}
	if o.State != "new" || o.Onchain == nil || o.Onchain.State != "waiting" || o.Onchain.Address != nil || o.OnchainSat != sat {
		t.Fatalf("created order looks wrong: state %q, onchain %+v", o.State, o.Onchain)
	}
	exp, err := time.Parse(time.RFC3339, o.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil {
		ev = map[string]rng{}
	}
	ro := &ocOrder{ID: o.ID, Token: tokenOf(o), Tag: tag, Sat: sat, Required: o.Onchain.RequiredConfirmations, Expiry: exp.Unix(), Ev: ev, Post: post}
	e.reg.Orders = append(e.reg.Orders, ro)
	e.saveReg()
	return ro, o
}

// press is the customer pressing Pay: POST /c/{token}/pay, then the page, which carries the address (and the invoice
// of a mixed order). The address is checked against the API's.
func (e *ocEnv) press(ro *ocOrder, wantInvoice bool) (addr, invoice string) {
	t := e.t
	t.Helper()
	poll(t, "address on the page", 30*time.Second, time.Second, func() (bool, error) {
		resp, err := e.hc.Post("http://"+publicAddr+"/c/"+ro.Token+"/pay", "application/x-www-form-urlencoded", nil)
		if err != nil {
			return false, err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			return false, fmt.Errorf("pay: status %d", resp.StatusCode)
		}
		code, page := e.page(ro.Token)
		m := addrRe.FindStringSubmatch(page)
		if code != 200 || m == nil || !strings.Contains(page, "Waiting for payment") {
			return false, errors.New("no address on the page yet")
		}
		addr = m[1]
		if im := invoiceRe.FindStringSubmatch(page); im != nil {
			invoice = im[1]
		}
		if wantInvoice && invoice == "" {
			return false, errors.New("no invoice on the page yet")
		}
		return true, nil
	})
	if o := e.order(ro.ID); o.Onchain == nil || o.Onchain.Address == nil || *o.Onchain.Address != addr {
		t.Fatalf("the API's address differs from the page's for order %s", ro.ID)
	}
	if ro.Address != "" && ro.Address != addr {
		t.Fatalf("order %s was given a second address", ro.ID)
	}
	ro.Address = addr
	e.saveReg()
	return addr, invoice
}

// pay sends sat from the miner wallet, replaceable, and returns the txid and the time it was sent.
func (e *ocEnv) pay(addr string, sat int64) (string, time.Time) {
	e.t.Helper()
	txid, err := e.btc.sendRBF(minerWallet, addr, btcrpc.FormatBTC(sat))
	if err != nil {
		e.t.Fatalf("sendtoaddress: %v", shownErr(err))
	}
	return txid, time.Now()
}

func (e *ocEnv) mine(n int) {
	e.t.Helper()
	if err := e.btc.mine(n); err != nil {
		e.t.Fatalf("mining %d: %v", n, shownErr(err))
	}
}

// waitOC polls the API until cond holds; it returns how long it took from since and the order.
func (e *ocEnv) waitOC(id, what string, since time.Time, bound time.Duration, cond func(orderJSON) bool) (time.Duration, orderJSON) {
	e.t.Helper()
	var d time.Duration
	var last orderJSON
	poll(e.t, what, bound, 100*time.Millisecond, func() (bool, error) {
		last = e.order(id)
		if cond(last) {
			d = time.Since(since)
			return true, nil
		}
		oc := "none"
		if last.Onchain != nil {
			oc = fmt.Sprintf("%+v", *last.Onchain)
		}
		return false, fmt.Errorf("state %s, onchain %s", last.State, oc)
	})
	return d, last
}

func (e *ocEnv) pageHas(token, want string) {
	e.t.Helper()
	poll(e.t, "page says "+want, 20*time.Second, 500*time.Millisecond, func() (bool, error) {
		code, page := e.page(token)
		if code == 200 && strings.Contains(page, want) {
			return true, nil
		}
		return false, fmt.Errorf("status %d, page lacks %q", code, want)
	})
}

// ---- finishing a run: stop, safety check, ALERT check ----

// quiesce lets the delivery loop finish an event that an order's expiry has just caused.
func (e *ocEnv) quiesce() {
	now := time.Now()
	for _, o := range e.reg.Orders {
		if len(o.Post) == 0 {
			continue
		}
		exp := time.Unix(o.Expiry, 0)
		if now.After(exp.Add(-time.Second)) && now.Before(exp.Add(15*time.Second)) {
			time.Sleep(exp.Add(10 * time.Second).Sub(now))
			now = time.Now()
		}
	}
}

// finish stops the service and runs the safety check and the ALERT check. alerts are the ALERT lines the phase
// expects (reorg, overpaid, behind); the lines that belong to events added in this run (underpaid, late, paid_again)
// are expected exactly as often as those events were added.
func (e *ocEnv) finish(s *svcRun, alerts map[string]rng) {
	t := e.t
	t.Helper()
	e.quiesce()
	s.stopQuiet()
	runEnd := time.Now()
	added := e.safety(e.runStart, runEnd)
	e.checkAlerts(s.logs.String(), alerts, added)
	e.saveReg()
}

func (e *ocEnv) checkAlerts(logs string, explicit map[string]rng, added map[string]int) {
	t := e.t
	t.Helper()
	// The ALERT lines are logged at ERROR; a key given with its level ("WARN ...") is kept as it is.
	want := map[string]rng{}
	for k, v := range explicit {
		if strings.HasPrefix(k, "WARN ") {
			want[k] = v
			continue
		}
		want["ERROR "+k] = v
	}
	// The two lines of an order.underpaid (at expiry, at the window end) together are as many as the order.underpaid
	// events added in the run, unless the phase passes either line: then its explicit ranges replace this rule.
	_, exU := explicit[alUnderpaid]
	_, exW := explicit[alUnderpaidWindow]
	derivedUnderpaid := !exU && !exW
	if _, ok := want["WARN "+alWalletBehind]; !ok {
		want["WARN "+alWalletBehind] = rng{0, 1 << 30}
	}
	want["ERROR "+cmp.Or(e.lateLine, alLate)] = rng{added[store.EventLate], added[store.EventLate]}
	want["ERROR "+cmp.Or(e.againLine, alPaidAgain)] = rng{added[store.EventPaidAgain], added[store.EventPaidAgain]}
	// Every record at ERROR or WARN is counted by its message, whatever its wording: the ALERT lines, but also "settle
	// failed", "scan failed", "track failed", "bitcoind call failed for an output", "on-chain status could not be read"
	// and the rest (onchain/watch.go, service/service.go). Any of them that a phase did not expect fails it. A line that
	// is not a JSON log record, or that names a startup failure, counts too.
	got := map[string]int{}
	attrs := map[string][]string{} // the fixed attributes of each counted record (a method and a code, an order id), for a failure
	// The five records before each counted one, each as its time, level, message and `at` (a fixed word) only: a
	// failure shows what the service did just before, and never prints a string from outside (onchain/watch.go's logging
	// rule).
	var before []string
	for _, l := range strings.Split(logs, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var m struct {
			Level  string `json:"level"`
			Msg    string `json:"msg"`
			Method string `json:"method"`
			Code   any    `json:"code"`
			Order  string `json:"order"`
			Time   string `json:"time"`
			At     string `json:"at"`
		}
		err := json.Unmarshal([]byte(l), &m)
		switch {
		case err != nil || m.Msg == "":
			got[l]++
		case m.Level == "ERROR" || m.Level == "WARN" || strings.Contains(m.Msg, "ALERT") || strings.Contains(m.Msg, "startup failed") || strings.Contains(m.Msg, "unexpectedly"):
			k := m.Level + " " + m.Msg
			got[k]++
			attrs[k] = append(attrs[k], fmt.Sprintf("method=%q code=%v order=%q time=%q at=%q; the records before it: %v",
				m.Method, m.Code, m.Order, m.Time, m.At, before))
		}
		if err == nil && m.Msg != "" {
			before = append(before, fmt.Sprintf("{time=%q level=%q msg=%q at=%q}", m.Time, m.Level, m.Msg, m.At))
			if len(before) > 5 {
				before = before[1:]
			}
		}
	}
	underpaidLine := func(msg string) bool { return msg == "ERROR "+alUnderpaid || msg == "ERROR "+alUnderpaidWindow }
	if derivedUnderpaid {
		if n, a := got["ERROR "+alUnderpaid]+got["ERROR "+alUnderpaidWindow], added[store.EventUnderpaid]; n != a {
			t.Errorf("the underpaid ALERT lines were logged %d times in all, %d order.underpaid events were added", n, a)
		}
	}
	for msg, n := range got {
		if _, ok := want[msg]; !ok && !(derivedUnderpaid && underpaidLine(msg)) {
			t.Errorf("unexpected log line (%d times): %s %v", n, msg, attrs[msg])
		}
	}
	for msg, r := range want {
		if n := got[msg]; n < r.Lo || n > r.Hi {
			t.Errorf("%q logged %d times, want %d to %d", msg, n, r.Lo, r.Hi)
		}
	}
	t.Logf("ALERTS onchain=%s observed=%v", e.name, got)
}

// safety runs after the service has stopped. It returns, per event type, the number of events added in this run.
func (e *ocEnv) safety(runStart, runEnd time.Time) map[string]int {
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

	added := map[string]int{}
	inProcess := 0
	var sumFwd, sumRev uint64
	started := e.t0.Truncate(time.Second)
	for _, o := range e.reg.Orders {
		// ---- events ----
		evs, err := st.EventsForOrder(ctx, o.ID)
		if err != nil {
			t.Fatal(err)
		}
		seqs := map[string][]int{}
		for _, ev := range evs {
			seqs[ev.Type] = append(seqs[ev.Type], ev.Seq)
			if !e.snap[evKey{o.ID, ev.Type, ev.Seq}] {
				added[ev.Type]++
			}
			atts, err := st.AttemptsFor(ctx, store.MainEndpoint, ev.ID)
			if err != nil {
				t.Fatal(err)
			}
			okAll, okHere := 0, 0
			for _, a := range atts {
				if a.OK {
					okAll++
					if !a.At.Before(started) {
						okHere++
					}
				}
			}
			if okAll != 1 {
				t.Errorf("order %s: %s seq %d has %d successful deliveries, want exactly 1", o.ID, ev.Type, ev.Seq, okAll)
			}
			if n := e.rcv.countSeq(o.ID, ev.Type, ev.Seq); n != okHere {
				t.Errorf("order %s: the receiver saw %s seq %d %d times, the store records %d successful deliveries in this process", o.ID, ev.Type, ev.Seq, n, okHere)
			}
			inProcess += okHere
		}
		emittedNow := o.Emitted || runEnd.Sub(maxTime(runStart, time.Unix(o.Expiry, 0))) >= 15*time.Second
		for _, typ := range eventTypes {
			r := o.Ev[typ]
			if p := o.Post[typ]; p > 0 {
				if runEnd.After(time.Unix(o.Expiry, 0)) || o.Emitted {
					r.Hi += p
				}
				if emittedNow {
					r.Lo += p
				}
			}
			s := seqs[typ]
			slices.Sort(s)
			if len(s) < r.Lo || len(s) > r.Hi {
				t.Errorf("order %s (%s): %d %s events, want %d to %d", o.ID, o.Tag, len(s), typ, r.Lo, r.Hi)
			}
			for i, q := range s {
				if q != i+1 {
					t.Errorf("order %s: %s seqs %v are not contiguous from 1", o.ID, typ, s)
					break
				}
			}
		}
		if n := len(seqs[store.EventPaid]); n > 1 {
			t.Errorf("order %s: order.paid %d times", o.ID, n)
		}
		if len(seqs[store.EventPaidAgain]) > len(seqs[store.EventReverted]) {
			t.Errorf("order %s: more order.paid_again than order.reverted", o.ID)
		}
		o.Emitted = emittedNow

		// ---- ledger against the chain ----
		raw, err := hex.DecodeString(o.ID)
		if err != nil {
			t.Fatal(err)
		}
		ud, err := ledger.U128FromBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		txos, err := st.TXOsForOrder(ctx, o.ID)
		if err != nil {
			t.Fatal(err)
		}
		var net int64
		seen := map[store.TXOKey]bool{}
		for _, x := range txos {
			k := store.TXOKey{TxID: x.TxID, Vout: x.Vout}
			if seen[k] {
				t.Errorf("order %s: output %s:%d stored twice", o.ID, x.TxID, x.Vout)
			}
			seen[k] = true
			amt := ledger.U128From64(uint64(x.AmountSat))
			live := false
			for ep := uint32(0); ep < 64; ep++ {
				f, found, err := lg.LookupTransfer(ctx, onchain.PaidTransferID(x.TxID, x.Vout, ep))
				if err != nil {
					t.Fatal(err)
				}
				if !found {
					break
				}
				if live {
					t.Errorf("order %s: output %s:%d has a forward of epoch %d after an unreversed one", o.ID, x.TxID, x.Vout, ep)
				}
				// Code 2 is a counted payment (first seen before the expiry), code 4 a late one (onchain/ledger.go).
				wantCode := uint16(2)
				if !x.FirstSeen.Before(time.Unix(o.Expiry, 0)) {
					wantCode = 4
				}
				if f.Code != wantCode {
					t.Errorf("order %s: forward of %s:%d epoch %d has code %d, want %d (first seen %s, expiry %s)", o.ID, x.TxID, x.Vout, ep, f.Code, wantCode, x.FirstSeen.UTC().Format(time.RFC3339), time.Unix(o.Expiry, 0).UTC().Format(time.RFC3339))
				}
				if f.Amount != amt || f.UserData128 != ud || f.DebitAccountID != onchain.OnchainWallet || f.CreditAccountID != onchain.MerchantOnchain || f.Flags != 0 {
					t.Errorf("order %s: forward of %s:%d epoch %d has other details", o.ID, x.TxID, x.Vout, ep)
				}
				sumFwd += f.Amount.Lo
				rv, rfound, err := lg.LookupTransfer(ctx, onchain.ReversalTransferID(x.TxID, x.Vout, ep))
				if err != nil {
					t.Fatal(err)
				}
				if rfound {
					if rv.Amount != f.Amount || rv.UserData128 != ud || rv.DebitAccountID != onchain.MerchantOnchain || rv.CreditAccountID != onchain.OnchainWallet {
						t.Errorf("order %s: reversal of %s:%d epoch %d does not mirror its forward", o.ID, x.TxID, x.Vout, ep)
					}
					sumRev += rv.Amount.Lo
					continue
				}
				live = true
				net += x.AmountSat
			}
		}
		if o.Address != "" {
			got, err := e.btc.getReceivedByAddress(merchantWallet, o.Address, o.Required)
			if err != nil {
				t.Fatalf("getreceivedbyaddress: %v", shownErr(err))
			}
			if got != net {
				t.Errorf("order %s (%s): the ledger holds %d sat net, the merchant wallet sees %d sat at %d confirmations", o.ID, o.Tag, net, got, o.Required)
			}
		}
	}
	if n := e.rcv.total(); n != inProcess {
		t.Errorf("the receiver saw %d requests, the store records %d successful deliveries in this process", n, inProcess)
	}
	if n := e.rcv.badCount(); n != 0 {
		t.Errorf("the receiver refused %d requests (bad signature or payload)", n)
	}
	pend, err := st.UndeliveredEvents(ctx, store.MainEndpoint, time.Unix(0, 0), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 0 {
		t.Errorf("%d events are undelivered", len(pend))
	}
	for _, c := range []struct {
		name          string
		acct          ledger.U128
		debit, credit uint64
	}{
		{"wallet", onchain.OnchainWallet, sumFwd, sumRev},
		{"merchant", onchain.MerchantOnchain, sumRev, sumFwd},
	} {
		b, err := lg.Balance(ctx, c.acct)
		if err != nil {
			t.Fatal(err)
		}
		if b.DebitsPosted != ledger.U128From64(c.debit) || b.CreditsPosted != ledger.U128From64(c.credit) {
			t.Errorf("ledger account %s: debits %s credits %s, want %d and %d", c.name, b.DebitsPosted, b.CreditsPosted, c.debit, c.credit)
		}
	}
	t.Logf("SAFETY onchain=%s orders=%d forward_sat=%d reversed_sat=%d added_events=%v events_received_here=%d", e.name, len(e.reg.Orders), sumFwd, sumRev, added, e.rcv.total())
	return added
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// withStore opens the store (the service must not be running).
func (e *ocEnv) withStore(fn func(ctx context.Context, st *store.Store)) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(e.dataDir, "store.db"))
	if err != nil {
		e.t.Fatal(err)
	}
	defer st.Close()
	fn(ctx, st)
}

func compact(raw json.RawMessage, max int) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	if b.Len() > max {
		return b.String()[:max] + "..."
	}
	return b.String()
}

// ---- the phases ----

// TestOnchainSetup: the merchant's wallet, the watch-only wallets and the tr experiment.
func TestOnchainSetup(t *testing.T) {
	root := ocRoot(t)
	btc := newBitcoindRPC(t)
	if err := btc.loadWallet(minerWallet); err != nil {
		t.Fatalf("loadwallet miner: %v", shownErr(err))
	}
	var bal json.Number
	if err := btc.call(minerWallet, "getbalance", nil, &bal); err != nil {
		t.Fatal(err)
	}
	if sat, err := btcrpc.ParseBTC(bal.String()); err != nil || sat < 300_000_000 {
		t.Fatalf("the miner wallet holds %s BTC, the phases need at least 3 (err %v)", bal, shownErr(err))
	}
	t.Logf("MEASURED onchain=setup miner_balance_btc=%s", bal)

	// The keyed merchant wallet: its external wpkh and tr descriptors (private keys not listed) are the merchant's.
	if err := btc.createWalletKeys(merchantWallet); err != nil {
		t.Fatalf("createwallet merchant: %v", shownErr(err))
	}
	rows, err := btc.listDescriptors(merchantWallet)
	if err != nil {
		t.Fatal(err)
	}
	var wpkh, tr string
	for _, r := range rows {
		if r.Active && !r.Internal && strings.HasPrefix(r.Desc, "wpkh(") {
			wpkh = r.Desc
		}
		if r.Active && !r.Internal && strings.HasPrefix(r.Desc, "tr(") {
			tr = r.Desc
		}
	}
	if wpkh == "" || tr == "" {
		t.Fatalf("the merchant wallet has no active external wpkh and tr descriptors (%d rows)", len(rows))
	}
	pw, err := descriptor.Parse(wpkh)
	if err != nil {
		t.Fatalf("the wpkh descriptor of Core 32.1 does not parse: %v", shownErr(err))
	}
	pt, err := descriptor.Parse(tr)
	if err != nil {
		t.Fatalf("the tr descriptor of Core 32.1 does not parse: %v", shownErr(err))
	}
	t.Logf("NOTE the keyed wallet %q and the watch-only wallet %q share the /0/* chain here ONLY so that this test can confirm receipts with getreceivedbyaddress. That is exactly the reuse docs/bitcoind.md forbids for a deployment (a dedicated account). The keyed wallet never issues an address of its own.", merchantWallet, watchWallet)
	// The regtest vector for descriptor/testdata/vectors.json: public data of a throwaway wallet.
	vec := map[string]any{"input": wpkh, "func": pw.Func, "origin": pw.Origin, "testnet": pw.Testnet, "canonical": pw.Canonical}
	vecTr := map[string]any{"input": tr, "func": pt.Func, "origin": pt.Origin, "testnet": pt.Testnet, "canonical": pt.Canonical}
	vb, err := json.Marshal([]any{vec, vecTr})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("REGTEST_VECTOR %s", vb)
	if err := os.WriteFile(filepath.Join(root, "vector.json"), vb, 0o600); err != nil {
		t.Fatal(err)
	}

	// The operator's wallet for the service: watch-only, blank, descriptor imported active (Ruling F).
	if err := btc.createWalletWatch(watchWallet); err != nil {
		t.Fatalf("createwallet %s: %v", watchWallet, shownErr(err))
	}
	if err := btc.importDescriptor(watchWallet, wpkh); err != nil {
		t.Fatalf("importdescriptors: %v", shownErr(err))
	}
	var di json.RawMessage
	if err := btc.call("", "getdescriptorinfo", map[string]any{"descriptor": pw.Canonical}, &di); err != nil {
		t.Fatal(err)
	}
	t.Logf("getdescriptorinfo of the canonical descriptor: %s", compact(di, 600))
	wrows, err := btc.listDescriptors(watchWallet)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range wrows {
		t.Logf("listdescriptors %s: active=%v internal=%v range=%v next_index=%d next=%d", watchWallet, r.Active, r.Internal, r.Range, r.NextIndex, r.Next)
	}
	if len(wrows) != 1 || !wrows[0].Active || wrows[0].NextIndex != 0 {
		t.Errorf("the watch wallet should hold exactly one active descriptor at next_index 0: %+v", wrows)
	}

	// tr() on a watch-only wallet. hdkeypath and the range after each getnewaddress.
	if err := btc.createWalletWatch(watchTrWallet); err != nil {
		t.Fatal(err)
	}
	if err := btc.importDescriptor(watchTrWallet, tr); err != nil {
		t.Fatalf("importdescriptors tr: %v", shownErr(err))
	}
	for i := 0; i < 3; i++ {
		var addr string
		if err := btc.call(watchTrWallet, "getnewaddress", map[string]any{"label": "", "address_type": "bech32m"}, &addr); err != nil {
			t.Fatalf("getnewaddress bech32m: %v", shownErr(err))
		}
		ai, err := btc.getAddressInfo(watchTrWallet, addr)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("m/86h/1h/0h/0/%d", i)
		t.Logf("OBSERVED tr getnewaddress #%d: hdkeypath=%q ismine=%v parent_desc_is_import=%v", i, shown(ai.HDKeyPath, want), ai.IsMine, ai.ParentDesc == tr)
		if ai.HDKeyPath != want {
			t.Errorf("tr address %d has hdkeypath %s, want %q", i, shown(ai.HDKeyPath, want), want)
		}
		rs, err := btc.listDescriptors(watchTrWallet)
		if err != nil || len(rs) != 1 {
			t.Fatalf("listdescriptors tr: %d rows, err %v", len(rs), shownErr(err))
		}
		t.Logf("OBSERVED tr listdescriptors after call #%d: range=%v next_index=%d next=%d", i, rs[0].Range, rs[0].NextIndex, rs[0].Next)
		// A range of [0, next+999] was expected. Core 32.1 shows the inclusive last index of the keypool of 1000, which is
		// next_index+998 (the first call leaves the import's [0, 999] as it is); recorded in REPORT.md.
		if len(rs[0].Range) != 2 || rs[0].Range[0] != 0 || rs[0].Range[1] != rs[0].NextIndex+998 {
			t.Errorf("tr range %v after call #%d, want [0, next_index+998] with next_index %d", rs[0].Range, i, rs[0].NextIndex)
		}
	}

	writeJSON0600(t, filepath.Join(root, "setup.json"), ocSetup{Descriptor: pw.Canonical, TrDesc: pt.Canonical})
	// The first start proves the configuration is accepted: Check passes against the real wallet.
	e := newOcEnv(t, true)
	s := e.start()
	e.finish(s, nil)
}

// TestOnchainPay: three orders at the three tiers, paid from the miner, mined one block at a time, and a mixed order
// whose unified QR is checked.
func TestOnchainPay(t *testing.T) {
	e := newOcEnv(t, true)
	s := e.start()
	paid := map[string]rng{store.EventPaid: {1, 1}}
	type tiered struct {
		ro   *ocOrder
		sat  int64
		req  int
		txid string
		sent time.Time
	}
	var ts []*tiered
	for _, c := range []struct {
		sat int64
		req int
	}{{satT1, 1}, {satT2, 3}, {satT3, 6}} {
		ro, o := e.newOrder(fmt.Sprintf("pay-t%d", c.req), c.sat, 0, 86400, paid, nil)
		if o.Onchain.RequiredConfirmations != c.req {
			t.Fatalf("order of %d sat needs %d confirmations, want %d", c.sat, o.Onchain.RequiredConfirmations, c.req)
		}
		addr, _ := e.press(ro, false)
		code, page := e.page(ro.Token)
		if code != 200 || !strings.Contains(page, addr) || !strings.Contains(page, "Send exactly "+btcrpc.FormatBTC(c.sat)+" BTC") {
			t.Fatalf("page of the %d sat order lacks its address or amount", c.sat)
		}
		ts = append(ts, &tiered{ro: ro, sat: c.sat, req: c.req})
	}
	// The mixed order: an address and an invoice, one unified QR.
	mixed, _ := e.newOrder("pay-mixed", satT1, orderMsat, 86400, nil, nil)
	maddr, minv := e.press(mixed, true)
	uri, err := checkout.PaymentURI(maddr, satT1, minv)
	if err != nil {
		t.Fatalf("PaymentURI of the page's fields: %v", shownErr(err))
	}
	if len(uri) > qr.MaxInput {
		t.Fatalf("the unified URI is %d bytes, over qr.MaxInput %d", len(uri), qr.MaxInput)
	}
	resp, err := e.hc.Get("http://" + publicAddr + "/c/" + mixed.Token + "/qr.svg")
	if err != nil {
		t.Fatal(err)
	}
	var svg bytes.Buffer
	_, rerr := svg.ReadFrom(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		t.Fatalf("reading qr.svg: %v", shownErr(rerr))
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("qr.svg of the mixed order: status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	wantSVG, err := qr.SVG(uri)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(svg.Bytes(), wantSVG) {
		t.Fatalf("the served QR is not the QR of PaymentURI(address, amount, invoice) (%d vs %d bytes)", svg.Len(), len(wantSVG))
	}
	if _, page := e.page(mixed.Token); !strings.Contains(page, strings.ReplaceAll(uri, "&", "&amp;")) {
		t.Error("the page's wallet link is not the unified URI")
	}
	if err := os.WriteFile(filepath.Join(e.root, "unified.svg"), svg.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURED onchain=pay unified_qr_bytes=%d uri_bytes=%d (limit %d); the SVG is saved as unified.svg; the phone-wallet scan is a manual step", svg.Len(), len(uri), qr.MaxInput)

	for _, x := range ts {
		x.txid, x.sent = e.pay(*e.order(x.ro.ID).Onchain.Address, x.sat)
	}
	for _, x := range ts {
		d, o := e.waitOC(x.ro.ID, "seen", x.sent, ocBound, func(o orderJSON) bool {
			return o.Onchain.State == "seen" && o.Onchain.ReceivedSat == x.sat
		})
		if o.State != "new" || o.Onchain.Confirmations != 0 || o.Onchain.ConfirmedSat != 0 {
			t.Errorf("tier %d: seen order reads %+v", x.req, o)
		}
		e.pageHas(x.ro.Token, fmt.Sprintf("Seen, 0 of %d confirmations", x.req))
		t.Logf("MEASURED onchain=pay tier=%d send_to_seen_seconds=%.2f (poll interval 2 s)", x.req, d.Seconds())
	}
	for n := 1; n <= 6; n++ {
		mined := time.Now()
		e.mine(1)
		for _, x := range ts {
			if n >= x.req {
				d, o := e.waitOC(x.ro.ID, fmt.Sprintf("tier %d paid at block %d", x.req, n), mined, ocBound, func(o orderJSON) bool {
					return o.State == "paid" && o.PaidAt != nil && o.Onchain.State == "paid" && o.Onchain.ConfirmedSat == x.sat
				})
				if n == x.req {
					t.Logf("MEASURED onchain=pay tier=%d confirmations=%d tier_block_to_paid_seconds=%.2f", x.req, n, d.Seconds())
				}
				if o.Onchain.Overpaid {
					t.Errorf("tier %d reads overpaid", x.req)
				}
				continue
			}
			_, o := e.waitOC(x.ro.ID, fmt.Sprintf("tier %d at %d of %d", x.req, n, x.req), mined, ocBound, func(o orderJSON) bool {
				return o.Onchain.Confirmations == n
			})
			if o.State != "new" || o.Onchain.State != "seen" || o.Onchain.ConfirmedSat != 0 {
				t.Errorf("tier %d at %d of %d confirmations already reads %s / %s", x.req, n, x.req, o.State, o.Onchain.State)
			}
			e.pageHas(x.ro.Token, fmt.Sprintf("Seen, %d of %d confirmations", n, x.req))
		}
	}
	for _, x := range ts {
		e.waitHook(x.ro.ID, store.EventPaid, hookBound)
		e.pageHas(x.ro.Token, ">Paid<")
	}
	if o := e.order(mixed.ID); o.State != "new" {
		t.Errorf("the unpaid mixed order reads %s", o.State)
	}
	time.Sleep(ocQuiet)
	e.finish(s, nil)
}

// viewTx records the wallet's view of a transaction.
func (e *ocEnv) viewTx(label, wallet, txid string) (confirmations int, known bool) {
	t := e.t
	var raw json.RawMessage
	if err := e.btc.call(wallet, "gettransaction", map[string]any{"txid": txid}, &raw); err != nil {
		t.Logf("OBSERVED tx %s gettransaction(%s...): %v", label, txid[:8], shownErr(err))
		return 0, false
	}
	var v struct {
		Confirmations int      `json:"confirmations"`
		Conflicts     []string `json:"walletconflicts"`
		MempoolConfl  []string `json:"mempoolconflicts"`
		ReplacedBy    string   `json:"replaced_by_txid"`
		Replaces      string   `json:"replaces_txid"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("gettransaction answer: %v", shownErr(err))
	}
	t.Logf("OBSERVED tx %s gettransaction(%s...) in %s: confirmations=%d walletconflicts=%d mempoolconflicts=%d replaced_by=%v replaces=%v", label, txid[:8], wallet, v.Confirmations, len(v.Conflicts), len(v.MempoolConfl), v.ReplacedBy != "", v.Replaces != "")
	return v.Confirmations, true
}

func (e *ocEnv) viewSince(label, wallet string, txids ...string) {
	t := e.t
	var r struct {
		Transactions []struct {
			TxID          string   `json:"txid"`
			Category      string   `json:"category"`
			Confirmations int      `json:"confirmations"`
			Address       string   `json:"address"`
			Conflicts     []string `json:"walletconflicts"`
		} `json:"transactions"`
		Removed []struct {
			TxID string `json:"txid"`
		} `json:"removed"`
	}
	if err := e.btc.call(wallet, "listsinceblock", map[string]any{"include_removed": true, "include_watchonly": true}, &r); err != nil {
		t.Fatal(err)
	}
	for _, id := range txids {
		for _, tx := range r.Transactions {
			if tx.TxID == id {
				lists := 0 // how many of the other txids given are in its walletconflicts
				for _, other := range txids {
					if other != id && slices.Contains(tx.Conflicts, other) {
						lists++
					}
				}
				t.Logf("OBSERVED tx %s listsinceblock in %s: %s... category=%s confirmations=%d walletconflicts=%d (of them the other txids given: %d)", label, wallet, id[:8], tx.Category, tx.Confirmations, len(tx.Conflicts), lists)
			}
		}
	}
	t.Logf("OBSERVED tx %s listsinceblock in %s: %d entries, %d removed", label, wallet, len(r.Transactions), len(r.Removed))
}

// TestOnchainRBF: a fee bump that keeps the output, and one that takes the output away.
func TestOnchainRBF(t *testing.T) {
	e := newOcEnv(t, true)
	s := e.start()
	paid := map[string]rng{store.EventPaid: {1, 1}}

	// Case 1: sendRBF, then bumpfee keeping the output. The order is paid once, by the replacement.
	r1, _ := e.newOrder("rbf-keep", satT1, 0, 86400, paid, nil)
	a1, _ := e.press(r1, false)
	txA, sent := e.pay(a1, satT1)
	e.waitOC(r1.ID, "seen", sent, ocBound, func(o orderJSON) bool { return o.Onchain.State == "seen" })
	txB, err := e.btc.bumpFee(minerWallet, txA, nil)
	if err != nil {
		t.Fatalf("bumpfee keeping the output: %v", shownErr(err))
	}
	e.viewTx("keep A", watchWallet, txA)
	e.viewTx("keep B", watchWallet, txB)
	e.viewSince("keep before mining", watchWallet, txA, txB)
	time.Sleep(ocQuiet)
	if o := e.order(r1.ID); o.Onchain.ReceivedSat != satT1 || o.State != "new" {
		t.Errorf("after the bump the order reads received %d sat, state %s; want %d and new (the replaced output must not count twice)", o.Onchain.ReceivedSat, o.State, satT1)
	}
	mined := time.Now()
	e.mine(1)
	e.waitOC(r1.ID, "paid", mined, ocBound, func(o orderJSON) bool { return o.State == "paid" && o.Onchain.ConfirmedSat == satT1 })
	if c, ok := e.viewTx("keep A after B mined", watchWallet, txA); !ok || c != -1 {
		t.Errorf("the replaced transaction A reads %d confirmations (known %v) after B was mined, want -1", c, ok)
	}
	if c, ok := e.viewTx("keep B after B mined", watchWallet, txB); !ok || c != 1 {
		t.Errorf("the replacement B reads %d confirmations (known %v), want 1", c, ok)
	}
	e.viewSince("keep after mining", watchWallet, txA, txB)
	e.waitHook(r1.ID, store.EventPaid, hookBound)

	// Case 2: bumpfee with the outputs option takes the customer's output away: the order stays unpaid.
	r2, _ := e.newOrder("rbf-divert", satT1, 0, 86400, nil, nil)
	a2, _ := e.press(r2, false)
	txC, sent := e.pay(a2, satT1)
	e.waitOC(r2.ID, "seen", sent, ocBound, func(o orderJSON) bool { return o.Onchain.State == "seen" })
	m1, m2 := e.newAddr(), e.newAddr()
	txD, fallback, bumpErr, err := e.btc.divert(minerWallet, txC, a2, m1, m2, 20_000)
	if err != nil {
		t.Fatalf("replacing the payment (bumpfee refused: %v): %v", shownErr(bumpErr), shownErr(err))
	}
	if bumpErr != nil {
		t.Logf("OBSERVED rbf bumpfee with the outputs option was refused by Core 32.1: %v; fell back to a hand-built conflicting spend (sendrawtransaction)", shownErr(bumpErr))
	} else {
		t.Logf("OBSERVED rbf bumpfee with the outputs option was accepted by Core 32.1 (fallback used: %v)", fallback)
	}
	e.viewTx("divert C", watchWallet, txC)
	e.viewSince("divert before mining", watchWallet, txC, txD)
	e.waitOC(r2.ID, "back to waiting", sent, ocBound, func(o orderJSON) bool {
		return o.Onchain.State == "waiting" && o.Onchain.ReceivedSat == 0
	})
	e.mine(1)
	time.Sleep(ocQuiet)
	e.viewTx("divert C after D mined", watchWallet, txC)
	e.viewSince("divert after mining", watchWallet, txC)
	if o := e.order(r2.ID); o.State != "new" || o.Onchain.ReceivedSat != 0 {
		t.Errorf("the diverted order reads %s with %d sat received", o.State, o.Onchain.ReceivedSat)
	}
	var c1 struct{ Confirmations int }
	var raw json.RawMessage
	if err := e.btc.call(minerWallet, "gettransaction", map[string]any{"txid": txC}, &raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &c1); err != nil {
		t.Fatal(err)
	}
	if c1.Confirmations != -1 {
		t.Errorf("the replaced transaction reads %d confirmations in the miner wallet after its replacement was mined, want -1", c1.Confirmations)
	}

	e.finish(s, nil)
	// The store's marks: the replaced outputs are dead, the replacement of the first case is mined.
	e.withStore(func(ctx context.Context, st *store.Store) {
		check := func(o *ocOrder, wantTxos int, liveKind string) {
			xs, err := st.TXOsForOrder(ctx, o.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(xs) != wantTxos {
				t.Errorf("order %s: %d stored outputs, want %d", o.Tag, len(xs), wantTxos)
			}
			dead, live := 0, 0
			for _, x := range xs {
				m, ok, err := st.CurrentMark(ctx, store.TXOKey{TxID: x.TxID, Vout: x.Vout})
				if err != nil || !ok {
					t.Fatalf("mark of %s: %v", x.TxID, shownErr(err))
				}
				switch m.Kind {
				case store.MarkDead:
					dead++
				case liveKind:
					live++
				default:
					t.Errorf("order %s: output marked %s", o.Tag, m.Kind)
				}
			}
			t.Logf("OBSERVED rbf order %s: %d stored outputs, %d dead, %d %s", o.Tag, len(xs), dead, live, liveKind)
		}
		check(r1, 2, store.MarkMined)
		check(r2, 1, "none")
	})
}

// TestOnchainReorg: a payment reverted by a double spend in a longer chain, and a plain reorg.
func TestOnchainReorg(t *testing.T) {
	e := newOcEnv(t, true)
	s := e.start()

	// R1: pay, mine 1 (paid), invalidate the block, double-spend the payment, mine 2.
	r1, _ := e.newOrder("reorg-double-spend", satT1, 0, 86400,
		map[string]rng{store.EventPaid: {1, 1}, store.EventReverted: {1, 1}}, nil)
	a1, _ := e.press(r1, false)
	txA, sent := e.pay(a1, satT1)
	e.waitOC(r1.ID, "seen", sent, ocBound, func(o orderJSON) bool { return o.Onchain.State == "seen" })
	mined := time.Now()
	e.mine(1)
	e.waitOC(r1.ID, "paid", mined, ocBound, func(o orderJSON) bool { return o.State == "paid" })
	e.waitHook(r1.ID, store.EventPaid, hookBound)
	height := e.height()
	blk := e.blockHash(height)
	if err := e.btc.invalidateBlock(blk); err != nil {
		t.Fatal(err)
	}
	m1, m2 := e.newAddr(), e.newAddr()
	var txB string
	poll(t, "the wallet re-adds the disconnected transaction to the mempool", 20*time.Second, 500*time.Millisecond, func() (bool, error) {
		var bumpErr error
		var err error
		txB, _, bumpErr, err = e.btc.divert(minerWallet, txA, a1, m1, m2, 20_000)
		if err != nil {
			return false, fmt.Errorf("%v (bumpfee: %v)", shownErr(err), shownErr(bumpErr))
		}
		return true, nil
	})
	reorgAt := time.Now()
	e.mine(2)
	e.waitHook(r1.ID, store.EventReverted, hookBound)
	d, o := e.waitOC(r1.ID, "reverted: new and waiting", reorgAt, ocBound, func(o orderJSON) bool {
		return o.State == "new" && o.Onchain.State == "waiting" && o.Onchain.ConfirmedSat == 0
	})
	t.Logf("MEASURED onchain=reorg double_spend_mined_to_order_new_seconds=%.2f (order created with expires_in 86400; reads %s / %s)", d.Seconds(), o.State, o.Onchain.State)
	e.viewTx("reorg A", watchWallet, txA)
	e.viewTx("reorg B", watchWallet, txB)
	var r struct {
		Removed []struct {
			TxID string `json:"txid"`
		} `json:"removed"`
	}
	if err := e.btc.call(watchWallet, "listsinceblock", map[string]any{"blockhash": blk, "include_removed": true}, &r); err != nil {
		t.Fatal(err)
	}
	inRemoved := slices.ContainsFunc(r.Removed, func(x struct {
		TxID string `json:"txid"`
	}) bool {
		return x.TxID == txA
	})
	t.Logf("OBSERVED reorg listsinceblock(the invalidated block's hash, include_removed) in %s: removed=%d (the payment's txid among them: %v)", watchWallet, len(r.Removed), inRemoved)
	if !inRemoved {
		t.Error("listsinceblock from the invalidated block does not list the payment's transaction as removed")
	}
	// Reconsider the invalidated block: the chain with the double spend is longer, so nothing changes.
	tipBefore := e.blockHash(height + 1)
	if err := e.btc.reconsiderBlock(blk); err != nil {
		t.Fatal(err)
	}
	tipAfter := e.blockHash(height + 1)
	time.Sleep(ocQuiet)
	t.Logf("OBSERVED reorg reconsiderblock: the block at height %d is the same after (%v); the order reads %s / %s", height+1, tipBefore == tipAfter, e.order(r1.ID).State, e.order(r1.ID).Onchain.State)
	if tipBefore != tipAfter {
		t.Error("reconsiderblock moved the active chain")
	}
	if o := e.order(r1.ID); o.State != "new" || o.Onchain.State != "waiting" {
		t.Errorf("after reconsiderblock the order reads %s / %s", o.State, o.Onchain.State)
	}

	// R2: a plain reorg; the transaction is mined again in the new chain.
	r2, _ := e.newOrder("reorg-plain", satT1, 0, 86400,
		map[string]rng{store.EventPaid: {1, 1}, store.EventReverted: {0, 1}, store.EventPaidAgain: {0, 1}}, nil)
	a2, _ := e.press(r2, false)
	_, sent = e.pay(a2, satT1)
	e.waitOC(r2.ID, "seen", sent, ocBound, func(o orderJSON) bool { return o.Onchain.State == "seen" })
	mined = time.Now()
	e.mine(1)
	e.waitOC(r2.ID, "paid", mined, ocBound, func(o orderJSON) bool { return o.State == "paid" })
	e.waitHook(r2.ID, store.EventPaid, hookBound)
	blk2 := e.blockHash(e.height())
	if err := e.btc.invalidateBlock(blk2); err != nil {
		t.Fatal(err)
	}
	reorgAt = time.Now()
	e.mine(2)
	d, _ = e.waitOC(r2.ID, "paid after the plain reorg", reorgAt, ocBound, func(o orderJSON) bool {
		return o.State == "paid" && o.Onchain.Confirmations == 2
	})
	t.Logf("MEASURED onchain=reorg plain_reorg_mined_to_paid_reads_seconds=%.2f", d.Seconds())
	time.Sleep(2 * ocQuiet)
	nRev, nAgain := e.rcv.count(r2.ID, store.EventReverted), e.rcv.count(r2.ID, store.EventPaidAgain)
	t.Logf("OBSERVED reorg plain reorg: order.reverted delivered %d, order.paid_again delivered %d (0 and 0 when no pass ran between the invalidation and the new block)", nRev, nAgain)
	if nRev != nAgain {
		t.Errorf("order.reverted %d times but order.paid_again %d times", nRev, nAgain)
	}
	// What happened is now fixed: later safety checks reject a different, unpaired state.
	r2.Ev[store.EventReverted], r2.Ev[store.EventPaidAgain] = rng{nRev, nRev}, rng{nAgain, nAgain}

	// R3: a reorg observed in between: the watcher reverts the payment while the transaction waits in the mempool, and
	// the next block makes it order.paid_again.
	r3, _ := e.newOrder("reorg-observed", satT1, 0, 86400,
		map[string]rng{store.EventPaid: {1, 1}, store.EventReverted: {1, 1}, store.EventPaidAgain: {1, 1}}, nil)
	a3, _ := e.press(r3, false)
	_, sent = e.pay(a3, satT1)
	e.waitOC(r3.ID, "seen", sent, ocBound, func(o orderJSON) bool { return o.Onchain.State == "seen" })
	e.mine(1)
	e.waitOC(r3.ID, "paid", time.Now(), ocBound, func(o orderJSON) bool { return o.State == "paid" })
	e.waitHook(r3.ID, store.EventPaid, hookBound)
	blk3 := e.blockHash(e.height())
	if err := e.btc.invalidateBlock(blk3); err != nil {
		t.Fatal(err)
	}
	reorgAt = time.Now()
	d, _ = e.waitOC(r3.ID, "reverted", reorgAt, ocBound, func(o orderJSON) bool { return o.State == "new" && o.Onchain.ConfirmedSat == 0 })
	t.Logf("MEASURED onchain=reorg invalidateblock_to_order_new_seconds=%.2f", d.Seconds())
	e.waitHook(r3.ID, store.EventReverted, hookBound)
	mined = time.Now()
	e.mine(1)
	d, _ = e.waitOC(r3.ID, "paid again", mined, ocBound, func(o orderJSON) bool { return o.State == "paid" && o.Onchain.ConfirmedSat == satT1 })
	t.Logf("MEASURED onchain=reorg block_to_paid_again_seconds=%.2f", d.Seconds())
	e.waitHook(r3.ID, store.EventPaidAgain, hookBound)
	time.Sleep(ocQuiet)
	// One reversal in R1, one in R3, and one in R2 exactly when R2's order.reverted was delivered.
	e.finish(s, map[string]rng{alReorg: {2 + nRev, 2 + nRev}})
}

// TestOnchainPartial: 60 % then 40 %, 120 %, and an order with a short expiry that is paid 60 % (read by LateAfter).
func TestOnchainPartial(t *testing.T) {
	e := newOcEnv(t, true)
	s := e.start()

	r1, _ := e.newOrder("partial-split", satT1, 0, 86400, map[string]rng{store.EventPaid: {1, 1}}, nil)
	a1, _ := e.press(r1, false)
	e.pay(a1, satT1*60/100)
	e.mine(1)
	_, o := e.waitOC(r1.ID, "60 % confirmed", time.Now(), ocBound, func(o orderJSON) bool { return o.Onchain.ConfirmedSat == satT1*60/100 })
	if o.State != "new" || o.Onchain.State != "seen" {
		t.Errorf("60 %% paid reads %s / %s, want new / seen", o.State, o.Onchain.State)
	}
	e.pay(a1, satT1*40/100)
	mined := time.Now()
	e.mine(1)
	d, o := e.waitOC(r1.ID, "paid by the sum", mined, ocBound, func(o orderJSON) bool { return o.State == "paid" })
	if o.Onchain.ConfirmedSat != satT1 || o.Onchain.Overpaid {
		t.Errorf("60 %% + 40 %% reads confirmed %d, overpaid %v", o.Onchain.ConfirmedSat, o.Onchain.Overpaid)
	}
	t.Logf("MEASURED onchain=partial second_part_block_to_paid_seconds=%.2f", d.Seconds())
	e.waitHook(r1.ID, store.EventPaid, hookBound)

	r2, _ := e.newOrder("partial-over", satT1, 0, 86400, map[string]rng{store.EventPaid: {1, 1}}, nil)
	a2, _ := e.press(r2, false)
	e.pay(a2, satT1*120/100)
	mined = time.Now()
	e.mine(1)
	d, o = e.waitOC(r2.ID, "overpaid", mined, ocBound, func(o orderJSON) bool { return o.State == "paid" && o.Onchain.Overpaid })
	if o.Onchain.ConfirmedSat != satT1*120/100 {
		t.Errorf("120 %% reads confirmed %d", o.Onchain.ConfirmedSat)
	}
	t.Logf("MEASURED onchain=partial overpaid_block_to_paid_seconds=%.2f", d.Seconds())
	e.waitHook(r2.ID, store.EventPaid, hookBound)
	e.pageHas(r2.Token, "The merchant will refund the extra.")

	r3, _ := e.newOrder("partial-short", satT1, 0, 300, nil, map[string]int{store.EventUnderpaid: 1})
	a3, _ := e.press(r3, false)
	e.pay(a3, satT1*60/100)
	e.mine(1)
	e.waitOC(r3.ID, "60 % confirmed", time.Now(), ocBound, func(o orderJSON) bool { return o.Onchain.ConfirmedSat == satT1*60/100 })
	time.Sleep(ocQuiet)
	e.finish(s, map[string]rng{alOverpaid: {1, 1}})
}

// TestOnchainLateBefore: an order with a short expiry and nothing paid.
func TestOnchainLateBefore(t *testing.T) {
	e := newOcEnv(t, true)
	s := e.start()
	r, _ := e.newOrder("late", satT1, 0, 300, nil, map[string]int{store.EventExpired: 1})
	e.press(r, false)
	if e.order(r.ID).State != "new" {
		t.Fatal("the order is not new")
	}
	e.finish(s, nil)
}

// TestOnchainRestartBefore: an order paid and mined once (it needs 3), then the service stops.
func TestOnchainRestartBefore(t *testing.T) {
	e := newOcEnv(t, true)
	s := e.start()
	r, _ := e.newOrder("restart", satT2, 0, 86400, nil, nil)
	a, _ := e.press(r, false)
	e.pay(a, satT2)
	e.mine(1)
	_, o := e.waitOC(r.ID, "1 of 3", time.Now(), ocBound, func(o orderJSON) bool { return o.Onchain.Confirmations == 1 })
	if o.State != "new" {
		t.Fatalf("the order reads %s at 1 of 3", o.State)
	}
	e.finish(s, nil)
}

// TestOnchainRestartAfter: two more blocks while the service is down; the new service pays on its first pass.
func TestOnchainRestartAfter(t *testing.T) {
	e := newOcEnv(t, true)
	r := e.tagged("restart")
	r.Ev = map[string]rng{store.EventPaid: {1, 1}}
	e.mine(2)
	// The first pass runs at Run start (onchain/watch.go Run). Its step 3 refreshes every stored output that is in its
	// order's watch window and not settled (store.WatchedTXOs: settled outputs are left out, in any process) with
	// gettransaction, paced at PollRate (default 5 a second, trackAll's ticker), and step 4 settles the order in the same
	// pass: P = outputs / PollRate after Run starts. The poll ticker fires ocPollSeconds after Run starts, so a second
	// pass starts at max(P, ocPollSeconds) and would settle at about max(P, ocPollSeconds) + P. The assertion is that the
	// restart catches up within the second pass's settle time plus one poll (a bound that had to tell the
	// first pass from the second came within 0.4 s of it as the phases' outputs grew). Whether the first pass did it is
	// logged (first_pass_bound_seconds: 1.5 P + the measured start time + 0.5 s).
	outputs := 0
	e.withStore(func(ctx context.Context, st *store.Store) {
		xs, err := st.WatchedTXOs(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		outputs = len(xs)
	})
	pass := time.Duration(float64(outputs) / onchain.DefaultPollRate * float64(time.Second))
	second := max(pass, ocPollSeconds*time.Second) + pass
	launched := time.Now()
	s := e.start()
	startDur := time.Since(launched)
	d, _ := e.waitOC(r.ID, "paid on the first pass", launched, ocBound, func(o orderJSON) bool { return o.State == "paid" && o.Onchain.Confirmations == 3 })
	firstBound := pass*3/2 + startDur + 500*time.Millisecond
	bound := second + startDur + ocPollSeconds*time.Second
	t.Logf("MEASURED onchain=restart launch_to_paid_seconds=%.2f first_pass_seconds=%.2f first_pass_bound_seconds=%.2f on_first_pass=%v bound_seconds=%.2f second_pass_would_settle_at_about=%.2f start_seconds=%.2f watched_outputs=%d poll_seconds=%d (mined while the service was down; the first pass tracks every unsettled output in its window at %.0f a second)",
		d.Seconds(), pass.Seconds(), firstBound.Seconds(), d <= firstBound, bound.Seconds(), second.Seconds(), startDur.Seconds(), outputs, ocPollSeconds, onchain.DefaultPollRate)
	if d > bound {
		t.Errorf("paid %.2f s after the launch, later than the restart's catch-up allows (%.2f s: the second pass's settle at about %.2f s for %d outputs, plus one poll)", d.Seconds(), bound.Seconds(), second.Seconds(), outputs)
	}
	e.waitHook(r.ID, store.EventPaid, hookBound)
	time.Sleep(ocQuiet)
	e.finish(s, nil)
}

type indexRecord struct {
	Recorded int64 `json:"recorded"`
}

// TestOnchainIndexBefore: an address is issued and the highest recorded index is saved; the script then kills bitcoind
// three times with SIGKILL.
func TestOnchainIndexBefore(t *testing.T) {
	e := newOcEnv(t, true)
	s := e.start()
	r, _ := e.newOrder("index-before", satT1, 0, 86400, nil, nil)
	e.press(r, false)
	e.finish(s, nil)
	var max int64
	e.withStore(func(ctx context.Context, st *store.Store) {
		var ok bool
		var err error
		if max, ok, err = st.MaxAddressIndex(ctx); err != nil || !ok {
			t.Fatalf("MaxAddressIndex: %v %v", ok, shownErr(err))
		}
	})
	rows, err := e.btc.listDescriptors(watchWallet)
	if err != nil || len(rows) != 1 {
		t.Fatalf("listdescriptors: %d rows, %v", len(rows), shownErr(err))
	}
	t.Logf("OBSERVED index before the kills: recorded max index %d, wallet next_index %d, range %v", max, rows[0].NextIndex, rows[0].Range)
	if rows[0].NextIndex <= max {
		t.Fatalf("the wallet's next_index %d is not above the recorded %d before any kill", rows[0].NextIndex, max)
	}
	writeJSON0600(t, filepath.Join(e.root, "index.json"), indexRecord{Recorded: max})
}

// TestOnchainIndexAfter: after three SIGKILLs of bitcoind, three new orders get three new, distinct indexes above the
// record. Then the hazard: re-importing the descriptor without next_index makes the service refuse to start.
func TestOnchainIndexAfter(t *testing.T) {
	e := newOcEnv(t, true)
	var rec indexRecord
	if err := readJSONFile(filepath.Join(e.root, "index.json"), &rec); err != nil {
		t.Fatalf("index record (did IndexBefore run?): %v", shownErr(err))
	}
	s := e.start()
	var rs []*ocOrder
	for i := 0; i < 3; i++ {
		r, _ := e.newOrder(fmt.Sprintf("index-after-%d", i), satT1, 0, 86400, nil, nil)
		e.press(r, false) // not paid: the hazard below only shows while the addresses stay unpaid
		rs = append(rs, r)
	}
	e.finish(s, nil)
	var max int64
	e.withStore(func(ctx context.Context, st *store.Store) {
		seen := map[int64]bool{}
		prev := rec.Recorded
		for _, r := range rs {
			a, err := st.AddressForOrder(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if a.Index <= prev || seen[a.Index] {
				t.Errorf("order %s has index %d after %d (recorded %d, distinct %v)", r.Tag, a.Index, prev, rec.Recorded, !seen[a.Index])
			}
			seen[a.Index] = true
			prev = a.Index
			ai, err := e.btc.getAddressInfo(watchWallet, a.Address)
			if err != nil || !ai.IsMine || !strings.HasSuffix(ai.HDKeyPath, fmt.Sprintf("/0/%d", a.Index)) {
				t.Errorf("bitcoind's view of %s: %+v %v", r.Tag, ai, shownErr(err))
			}
			if a.Index > max {
				max = a.Index
			}
		}
	})
	t.Logf("OBSERVED index after three SIGKILLs of bitcoind: recorded %d, new indexes up to %d, three distinct and strictly above", rec.Recorded, max)

	// The hazard: re-import the same descriptor with the range it needs (a smaller range is refused) and no next_index.
	rows, err := e.btc.listDescriptors(watchWallet)
	if err != nil || len(rows) != 1 || len(rows[0].Range) != 2 {
		t.Fatalf("listdescriptors: %+v %v", rows, shownErr(err))
	}
	desc, rangeEnd := rows[0].Desc, rows[0].Range[1]
	t.Logf("OBSERVED index before the hazard: range %v next_index %d", rows[0].Range, rows[0].NextIndex)
	if err := e.btc.importDescriptorOpts(watchWallet, desc, map[string]any{"range": []int64{0, rangeEnd}}); err != nil {
		t.Fatalf("re-importing without next_index: %v", shownErr(err))
	}
	rows, _ = e.btc.listDescriptors(watchWallet)
	t.Logf("OBSERVED index after re-import without next_index: range %v next_index %d (the wallet would issue an old address)", rows[0].Range, rows[0].NextIndex)
	if rows[0].NextIndex > max {
		t.Fatalf("the re-import did not move next_index back (%d, record %d): the hazard cannot be observed", rows[0].NextIndex, max)
	}
	bad, err := e.tryStart()
	if err == nil {
		bad.stopQuiet()
		t.Fatal("the service started with a wallet whose next_index is behind its record")
	}
	n := strings.Count(bad.logs.String(), alBehind)
	if n != 1 {
		t.Errorf("the refusal logged the ErrIndexBehind ALERT %d times, want 1: %s", n, firstLines(bad.logs.String(), 5))
	}
	t.Logf("OBSERVED index the service refused to start: %v; ALERT lines %d", shownErr(err), n)

	// Recovery: next_index = the record + 1.
	if err := e.btc.importDescriptorOpts(watchWallet, desc, map[string]any{"range": []int64{0, rangeEnd}, "next_index": max + 1}); err != nil {
		t.Fatalf("re-importing with next_index: %v", shownErr(err))
	}
	rows, _ = e.btc.listDescriptors(watchWallet)
	t.Logf("OBSERVED index after re-import with next_index %d: range %v next_index %d", max+1, rows[0].Range, rows[0].NextIndex)
	s = e.start()
	r, _ := e.newOrder("index-recovered", satT1, 0, 86400, nil, nil)
	e.press(r, false)
	e.finish(s, nil)
	e.withStore(func(ctx context.Context, st *store.Store) {
		a, err := st.AddressForOrder(ctx, r.ID)
		if err != nil || a.Index <= max {
			t.Errorf("the recovered service issued index %d, want above %d (%v)", a.Index, max, shownErr(err))
		}
	})
}

// TestOnchainLateAfter: after the short expiry, the unpaid order reads expired, then a payment makes it late and never
// paid; the 60 % order is underpaid, then late after its top-up.
func TestOnchainLateAfter(t *testing.T) {
	e := newOcEnv(t, true)
	late, part := e.tagged("late"), e.tagged("partial-short")
	for _, o := range []*ocOrder{late, part} {
		if wait := time.Until(time.Unix(o.Expiry, 0).Add(time.Second)); wait > 0 {
			t.Logf("waiting %.0f s for the expiry of %s", wait.Seconds(), o.Tag)
			time.Sleep(wait)
		}
	}
	snap := e.snapshot()
	s := e.start()
	hadExpired := snap[evKey{late.ID, store.EventExpired, 1}]
	hadUnderpaid := snap[evKey{part.ID, store.EventUnderpaid, 1}]
	e.waitOC(late.ID, "expired", e.runStart, 3*time.Minute, func(o orderJSON) bool { return o.State == "expired" })
	e.waitOC(part.ID, "underpaid", e.runStart, 3*time.Minute, func(o orderJSON) bool { return o.State == "underpaid" })
	if !hadExpired {
		e.waitHook(late.ID, store.EventExpired, hookBound)
	}
	if !hadUnderpaid {
		e.waitHook(part.ID, store.EventUnderpaid, hookBound)
	}
	e.pageHas(late.Token, ">Expired<")
	e.pageHas(part.Token, ">Underpaid<")

	// Late payments: the first seen is after the expiry. Their order.late comes once they are mined (Ruling Z2).
	late.Ev = map[string]rng{store.EventLate: {1, 1}}
	part.Ev = map[string]rng{store.EventLate: {1, 1}}
	_, sent := e.pay(late.Address, satT1)
	e.waitOC(late.ID, "late seen", sent, ocBound, func(o orderJSON) bool { return o.Onchain.LateSat == satT1 })
	e.pay(part.Address, satT1*40/100)
	// Ruling Z2: the API reads late while the payments are in the mempool, but order.late waits for their record
	// (mined at the tier), so no webhook may come before the block.
	time.Sleep(ocQuiet)
	for _, o := range []*ocOrder{late, part} {
		if n := e.rcv.countSeq(o.ID, store.EventLate, 1); n != 0 {
			t.Errorf("%s: order.late arrived %d times before its payment was mined", o.Tag, n)
		}
	}
	mined := time.Now()
	e.mine(1)
	for _, o := range []*ocOrder{late, part} {
		d, got := e.waitOC(o.ID, o.Tag+" late", mined, ocBound, func(g orderJSON) bool {
			if g.State == "paid" {
				t.Fatalf("%s reads paid: a late payment must never pay an expired order", o.Tag)
			}
			return g.State == "late"
		})
		t.Logf("MEASURED onchain=late tag=%s block_to_late_seconds=%.2f late_sat=%d", o.Tag, d.Seconds(), got.Onchain.LateSat)
		e.waitHook(o.ID, store.EventLate, hookBound)
		e.pageHas(o.Token, ">Late<")
	}
	time.Sleep(ocQuiet)
	for _, o := range []*ocOrder{late, part} {
		if g := e.order(o.ID); g.State != "late" || g.PaidAt != nil {
			t.Errorf("%s reads %s after the quiet period", o.Tag, g.State)
		}
	}
	e.finish(s, nil)
}

// TestOnchainWhitelist: the satbooth user may call only the runtime methods.
func TestOnchainWhitelist(t *testing.T) {
	sb := newRPCAs(t, "SATBOOTH_RPC_USER", "SATBOOTH_RPC_PASSWORD")
	var info struct {
		Chain string `json:"chain"`
	}
	if err := sb.call("", "getblockchaininfo", nil, &info); err != nil || info.Chain != "regtest" {
		t.Fatalf("getblockchaininfo as satbooth: %v (chain %s)", shownErr(err), shown(info.Chain, "regtest", "main", "test", "signet"))
	}
	// The 11th method: getmempoolinfo answers, with a boolean loaded, through the whitelist.
	var mi struct {
		Loaded *bool `json:"loaded"`
	}
	if err := sb.call("", "getmempoolinfo", nil, &mi); err != nil || mi.Loaded == nil {
		t.Fatalf("getmempoolinfo as satbooth: %v (loaded %v)", shownErr(err), mi.Loaded)
	}
	t.Logf("OBSERVED whitelist getmempoolinfo as satbooth: loaded %v", *mi.Loaded)
	// The twelfth method: gettxspendingprevout with mempool_only answers (HTTP 200) one element for a made-up outpoint,
	// without a spendingtxid.
	zero := strings.Repeat("0", 64)
	if res, err := sb.spendingPrevout([]prevoutRef{{TxID: zero, Vout: 0}}); err != nil {
		t.Errorf("gettxspendingprevout as satbooth: %v, want an answer (HTTP 200)", shownErr(err))
	} else if len(res) != 1 || res[0].TxID != zero || res[0].Vout != 0 || res[0].SpendingTxID != "" {
		t.Errorf("gettxspendingprevout as satbooth for one made-up outpoint answered %+v, want one element for it with no spendingtxid", res)
	} else {
		t.Logf("OBSERVED whitelist gettxspendingprevout as satbooth (mempool_only): one element, no spendingtxid")
	}
	addr, err := newBitcoindRPC(t).getNewAddress()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		wallet, method string
		params         any
	}{
		{minerWallet, "sendtoaddress", map[string]any{"address": addr, "amount": 0.0001}},
		{"", "createwallet", map[string]any{"wallet_name": "forbidden"}},
		{watchWallet, "importdescriptors", map[string]any{"requests": []any{}}},
		{"", "stop", nil}, // last: if the whitelist were broken this would shut bitcoind down, and the test would fail
	} {
		err := sb.call(c.wallet, c.method, c.params, nil)
		var he *httpError
		if !errors.As(err, &he) || he.Status != http.StatusForbidden {
			t.Errorf("%s as satbooth: %v, want HTTP 403", c.method, shownErr(err))
			continue
		}
		t.Logf("OBSERVED whitelist %s as satbooth: HTTP %d", c.method, he.Status)
	}
	var a string
	if err := sb.call(minerWallet, "getnewaddress", nil, &a); err != nil || a == "" {
		t.Errorf("getnewaddress on the miner wallet as satbooth: %v", shownErr(err))
	} else {
		t.Logf("OBSERVED whitelist getnewaddress on the miner wallet (not the watch wallet) as satbooth succeeds: the whitelist is per method, not per wallet; recorded, not relied on")
	}
	// The full user is unaffected by the whitelist (rpcwhitelistdefault=0).
	if _, err := newBitcoindRPC(t).getBlockCount(); err != nil {
		t.Errorf("the full user after the whitelist: %v", shownErr(err))
	}
	// The safety check, as every phase ends with it (no service runs in this phase).
	e := newOcEnv(t, true)
	e.snap = e.snapshot()
	now := time.Now()
	e.safety(now, now)
	e.saveReg()
}
