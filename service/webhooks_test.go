package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/webhook"
)

// waitLong bounds the waits for a delivery on a fake clock: a pass can be a second or more apart, and each pass writes
// to SQLite, which a loaded machine can stall for many seconds.
const waitLong = 60 * time.Second

// shortFallback is the deliverers' fallback interval of every test that runs the service on a fake clock: a clock that
// is moved while the service runs is noticed only at the next timer, so the "no further sends" waits would otherwise
// check nothing. The tests that wait for a delivery after a payment run on the real clock with the production
// intervals: they are the end-to-end check that the store's wake-up works.
const shortFallback = 200 * time.Millisecond

func eventuallyWithin(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// hookSink is a webhook receiver of the tests: it records the id and the signature header of every request. With hold
// set it keeps each request open until released (or until the sender gives up), so a test can stop the service
// mid-request. status is the answer (204 by default).
type hookSink struct {
	name    string
	srv     *httptest.Server
	mu      sync.Mutex
	got     map[string]int
	headers map[string]string
	bodies  map[string][]byte
	status  int
	hold    chan struct{}
	entered chan struct{}
}

func newSink(t *testing.T, name string) *hookSink {
	t.Helper()
	s := &hookSink{name: name, got: map[string]int{}, headers: map[string]string{}, bodies: map[string][]byte{}, status: http.StatusNoContent, entered: make(chan struct{}, 64)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var p struct {
			ID string `json:"id"`
		}
		b, _ := io.ReadAll(io.LimitReader(req.Body, 1<<16))
		_ = json.Unmarshal(b, &p)
		s.mu.Lock()
		s.got[p.ID]++
		s.bodies[p.ID] = b
		s.headers[p.ID] = req.Header.Get(webhook.HeaderName)
		status, hold := s.status, s.hold
		s.mu.Unlock()
		if hold != nil {
			s.entered <- struct{}{}
			select {
			case <-hold:
			case <-req.Context().Done():
			}
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// holdOpen makes the sink keep every request open; the returned function releases them (and runs at the test's end).
func (s *hookSink) holdOpen(t *testing.T) {
	t.Helper()
	hold := make(chan struct{})
	s.mu.Lock()
	s.hold = hold
	s.mu.Unlock()
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(hold) }) })
}

func (s *hookSink) setStatus(code int) {
	s.mu.Lock()
	s.status = code
	s.mu.Unlock()
}

func (s *hookSink) count(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got[id]
}

func (s *hookSink) header(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headers[id]
}

func (s *hookSink) body(id string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[id]
}

// secret is the endpoint's own signing key: 32 bytes derived from its name.
func (s *hookSink) secret() []byte {
	h := sha256.Sum256([]byte("sink secret " + s.name))
	return h[:]
}

// endpoint writes the sink's secret file in dir (mode 0600) and returns its place in the configuration.
func (s *hookSink) endpoint(t testing.TB, dir string) WebhookEndpoint {
	t.Helper()
	path := filepath.Join(dir, s.name+".secret")
	if err := os.WriteFile(path, []byte(hex.EncodeToString(s.secret())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return WebhookEndpoint{Name: s.name, URL: s.srv.URL, SecretFile: path}
}

// webhooksMod makes the configuration's endpoints the sinks, in order.
func webhooksMod(t *testing.T, sinks ...*hookSink) func(dir string, m map[string]any) {
	return func(dir string, m map[string]any) {
		list := make([]any, 0, len(sinks))
		for _, s := range sinks {
			e := s.endpoint(t, dir)
			list = append(list, map[string]any{"name": e.Name, "url": e.URL, "secret_file": e.SecretFile})
		}
		m["webhooks"] = list
	}
}

// withSinks is the configuration cfg with the sinks as its endpoints.
func withSinks(t *testing.T, cfg *Config, dir string, sinks ...*hookSink) *Config {
	c := *cfg
	c.Webhooks = nil
	for _, s := range sinks {
		c.Webhooks = append(c.Webhooks, s.endpoint(t, dir))
	}
	return &c
}

// testClock is a clock a test moves.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

// phase is one start of the service on a data directory, stopped by stop (also at the end of the test).
type phase struct {
	t      *testing.T
	cancel context.CancelFunc
	done   chan error
	logs   *syncBuf
	once   sync.Once
	err    error
}

func startPhase(t *testing.T, cfg *Config, opts ...Option) *phase {
	t.Helper()
	pub, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	prv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &phase{t: t, logs: &syncBuf{}, done: make(chan error, 1)}
	var ctx context.Context
	ctx, p.cancel = context.WithCancel(context.Background())
	opts = append([]Option{WithWallet(newFakeWallet()), WithListeners(pub, prv), WithLogger(slog.New(slog.NewJSONHandler(p.logs, nil)))}, opts...)
	go func() { p.done <- Run(ctx, cfg, opts...) }()
	t.Cleanup(func() { p.stop() })
	deadline := time.Now().Add(waitLong)
	for !strings.Contains(p.logs.String(), logStarted) {
		select {
		case err := <-p.done:
			t.Fatalf("the service did not start: %v\n%s", err, p.logs.String())
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the service did not start")
		}
	}
	return p
}

func (p *phase) stop() error {
	p.once.Do(func() {
		p.cancel()
		select {
		case p.err = <-p.done:
		case <-time.After(30 * time.Second):
			p.t.Error("Run did not return")
		}
	})
	return p.err
}

// seedExpired adds an order.expired event for the order to the stopped service's store, created at the given time, and
// returns the event's id. It leaves the clock at at.
func seedExpired(t *testing.T, dataDir string, clk *testClock, orderID string, at time.Time) string {
	t.Helper()
	clk.Set(at)
	st, err := store.Open(context.Background(), filepath.Join(dataDir, "store.db"), store.WithClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if added, err := st.AddEvent(context.Background(), store.Event{Type: store.EventExpired, OrderID: orderID}); err != nil || !added {
		t.Fatalf("seeding an event: %v %v", added, err)
	}
	return store.EventID(store.EventExpired, orderID, 1)
}

// payOrder creates an order, pays it through the checkout page and the fake wallet, and waits until it reads paid.
func (r *rig) payOrder(ref string) orderResp {
	r.t.Helper()
	o := r.createOrder(ref)
	if code, _ := do(r.t, "POST", o.CheckoutURL+"/pay", "", nil); code != http.StatusSeeOther {
		r.t.Fatalf("pay: %d", code)
	}
	r.wallet.settleAll()
	eventuallyWithin(r.t, "the order to be paid", func() bool { return r.getOrder(o.ID).State == "paid" })
	return o
}

func sinkNames(sinks []*hookSink) string {
	var n []string
	for _, s := range sinks {
		n = append(n, s.name)
	}
	return strings.Join(n, ",")
}

// A paid order's event reaches every endpoint, signed with that endpoint's own secret and no other.
func TestPaidEventReachesEveryEndpoint(t *testing.T) {
	a, b := newSink(t, "alpha"), newSink(t, "beta")
	r := startRigWith(t, webhooksMod(t, a, b))
	o := r.payOrder("two-receivers")
	id := store.EventID(store.EventPaid, o.ID, 1)
	eventuallyWithin(t, "both receivers to get the event", func() bool { return a.count(id) == 1 && b.count(id) == 1 })
	for _, s := range []*hookSink{a, b} {
		other := a
		if s == a {
			other = b
		}
		if err := webhook.Verify(s.secret(), s.header(id), s.body(id), time.Now(), webhook.RecommendedWindow); err != nil {
			t.Errorf("%s: signature with its own secret: %v", s.name, err)
		}
		if err := webhook.Verify(other.secret(), s.header(id), s.body(id), time.Now(), webhook.RecommendedWindow); err == nil {
			t.Errorf("%s: its signature verifies with %s's secret", s.name, other.name)
		}
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

// A start with a third endpoint sends it the undelivered events of the last 24 hours (by the clock) and none older,
// and sends nothing again to the two that had answered.
func TestAddedEndpointCatchesUpTwentyFourHours(t *testing.T) {
	a, b, c := newSink(t, "alpha"), newSink(t, "beta"), newSink(t, "gamma")
	clk := &testClock{}
	n := time.Now().Truncate(time.Second)
	clk.Set(n)
	r := startRigWith(t, webhooksMod(t, a, b), WithClock(clk.Now), withDeliveryFallback(shortFallback))
	oOut, oIn, oNew := r.createOrder("out"), r.createOrder("in"), r.createOrder("new")
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	// The ages are by the clock of the start that sees them: out is 25 hours old at n, in 22 hours, new 2 hours.
	eOut := seedExpired(t, r.cfg.DataDir, clk, oOut.ID, n.Add(-25*time.Hour))
	eIn := seedExpired(t, r.cfg.DataDir, clk, oIn.ID, n.Add(-22*time.Hour))
	eNew := seedExpired(t, r.cfg.DataDir, clk, oNew.ID, n.Add(-2*time.Hour))

	// alpha and beta, first seen at n, are sent the last 24 hours: in and new, not out.
	clk.Set(n)
	p := startPhase(t, r.cfg, WithClock(clk.Now), withDeliveryFallback(shortFallback))
	eventuallyWithin(t, "alpha and beta to get in and new", func() bool {
		return a.count(eIn) == 1 && a.count(eNew) == 1 && b.count(eIn) == 1 && b.count(eNew) == 1
	})
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}

	// An hour later gamma is added: it is sent the 24 hours before its own first pass (in and new), not out.
	clk.Set(n.Add(time.Hour))
	p = startPhase(t, withSinks(t, r.cfg, r.dir, a, b, c), WithClock(clk.Now), withDeliveryFallback(shortFallback))
	eventuallyWithin(t, "gamma to get in and new", func() bool { return c.count(eIn) == 1 && c.count(eNew) == 1 })
	time.Sleep(1500 * time.Millisecond) // past the next timer (200 ms) and the gap (1 s) of every deliverer
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*hookSink{a, b, c} {
		if s.count(eOut) != 0 || s.count(eIn) != 1 || s.count(eNew) != 1 {
			t.Errorf("%s got out %d, in %d, new %d; want 0, 1, 1", s.name, s.count(eOut), s.count(eIn), s.count(eNew))
		}
	}
	if strings.Contains(p.logs.String(), webhook.GiveUpMessage) {
		t.Error("an abandoned line for an event that was never listed")
	}
}

// An endpoint removed from the list runs no deliverer, and its recorded attempts stay. Added back, it continues where it
// stopped: it is sent what it had not been sent and nothing it had been sent.
func TestRemovedEndpointAddedBack(t *testing.T) {
	a, b := newSink(t, "alpha"), newSink(t, "beta")
	clk := &testClock{}
	n := time.Now().Truncate(time.Second)
	clk.Set(n)
	r := startRigWith(t, webhooksMod(t, a, b), WithClock(clk.Now), withDeliveryFallback(shortFallback))
	o1, o2, o3 := r.createOrder("one"), r.createOrder("two"), r.createOrder("three")
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	e1 := seedExpired(t, r.cfg.DataDir, clk, o1.ID, n.Add(-3*time.Hour))
	clk.Set(n)
	p := startPhase(t, r.cfg, WithClock(clk.Now), withDeliveryFallback(shortFallback))
	eventuallyWithin(t, "both to get e1", func() bool { return a.count(e1) == 1 && b.count(e1) == 1 })
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}

	// beta is removed; e2 is created meanwhile.
	e2 := seedExpired(t, r.cfg.DataDir, clk, o2.ID, n.Add(-2*time.Hour))
	clk.Set(n)
	p = startPhase(t, withSinks(t, r.cfg, r.dir, a), WithClock(clk.Now), withDeliveryFallback(shortFallback))
	eventuallyWithin(t, "alpha to get e2", func() bool { return a.count(e2) == 1 })
	time.Sleep(1500 * time.Millisecond)
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if b.count(e2) != 0 {
		t.Fatal("the removed endpoint was sent an event")
	}
	st, err := store.Open(context.Background(), filepath.Join(r.cfg.DataDir, "store.db"), store.WithClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	if at, err := st.AttemptsFor(context.Background(), "beta", e2); err != nil || len(at) != 0 {
		t.Fatalf("beta has attempts for e2: %v %v", at, err)
	}
	if at, err := st.AttemptsFor(context.Background(), "beta", e1); err != nil || len(at) != 1 || !at[0].OK {
		t.Fatalf("beta's recorded attempt for e1 is gone: %v %v", at, err)
	}
	st.Close()

	// e3 is created while beta is still out; then beta is back.
	e3 := seedExpired(t, r.cfg.DataDir, clk, o3.ID, n.Add(-time.Hour))
	clk.Set(n)
	p = startPhase(t, r.cfg, WithClock(clk.Now), withDeliveryFallback(shortFallback))
	eventuallyWithin(t, "beta to get e2 and e3, alpha e3", func() bool { return b.count(e2) == 1 && b.count(e3) == 1 && a.count(e3) == 1 })
	time.Sleep(1500 * time.Millisecond)
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if got := []int{a.count(e1), a.count(e2), a.count(e3), b.count(e1), b.count(e2), b.count(e3)}; fmt.Sprint(got) != "[1 1 1 1 1 1]" {
		t.Fatalf("deliveries alpha e1 e2 e3, beta e1 e2 e3: %v, want all 1", got)
	}
}

// After the 24 hours of an event, one endpoint's last attempt fails and its abandoned line is logged once, naming the
// endpoint; a later restart neither repeats the line nor sends the event again, and the endpoint that had answered is not
// touched.
func TestAbandonedLineIsNotRepeatedAfterARestart(t *testing.T) {
	a, b := newSink(t, "alpha"), newSink(t, "beta")
	a.setStatus(http.StatusInternalServerError)
	clk := &testClock{}
	n := time.Now().Truncate(time.Second)
	clk.Set(n)
	r := startRigWith(t, webhooksMod(t, a, b), WithClock(clk.Now), withDeliveryFallback(shortFallback))
	o := r.createOrder("abandoned")
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	e := seedExpired(t, r.cfg.DataDir, clk, o.ID, n)

	clk.Set(n)
	p := startPhase(t, r.cfg, WithClock(clk.Now), withDeliveryFallback(shortFallback))
	eventuallyWithin(t, "the first attempts", func() bool { return a.count(e) == 1 && b.count(e) == 1 })
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.logs.String(), webhook.GiveUpMessage) {
		t.Fatal("abandoned before its 24 hours")
	}

	// 25 hours on: alpha's last attempt is made, fails, and the line is written once, with the endpoint.
	clk.Set(n.Add(25 * time.Hour))
	p = startPhase(t, r.cfg, WithClock(clk.Now), withDeliveryFallback(shortFallback))
	eventuallyWithin(t, "the abandoned line", func() bool { return strings.Contains(p.logs.String(), webhook.GiveUpMessage) })
	time.Sleep(1500 * time.Millisecond)
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(p.logs.String(), webhook.GiveUpMessage); got != 1 {
		t.Fatalf("%d abandoned lines, want 1", got)
	}
	var line map[string]any
	for _, l := range strings.Split(p.logs.String(), "\n") {
		if strings.Contains(l, webhook.GiveUpMessage) {
			if err := json.Unmarshal([]byte(l), &line); err != nil {
				t.Fatal(err)
			}
		}
	}
	if line["endpoint"] != "alpha" {
		t.Fatalf("the abandoned line names %v, want alpha", line["endpoint"])
	}
	if a.count(e) != 2 || b.count(e) != 1 {
		t.Fatalf("alpha %d, beta %d attempts; want 2 and 1", a.count(e), b.count(e))
	}

	// Another restart, later: nothing is sent and nothing is logged.
	clk.Set(n.Add(30 * time.Hour))
	p = startPhase(t, r.cfg, WithClock(clk.Now), withDeliveryFallback(shortFallback))
	time.Sleep(1500 * time.Millisecond)
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.logs.String(), webhook.GiveUpMessage) {
		t.Fatal("the abandoned line was repeated after a restart")
	}
	if a.count(e) != 2 || b.count(e) != 1 {
		t.Fatalf("after the restart alpha %d, beta %d attempts; want 2 and 1", a.count(e), b.count(e))
	}
}

// Eight endpoints held mid-request do not hold the shutdown past the grace the docs give.
func TestShutdownWithEightEndpointsMidRequest(t *testing.T) {
	var sinks []*hookSink
	for i := 0; i < MaxWebhookEndpoints; i++ {
		s := newSink(t, fmt.Sprintf("ep%d", i))
		s.holdOpen(t)
		sinks = append(sinks, s)
	}
	r := startRigWith(t, webhooksMod(t, sinks...))
	r.payOrder("eight")
	for _, s := range sinks {
		select {
		case <-s.entered:
		case <-time.After(waitLong):
			t.Fatalf("%s never got a request (%s)\n%s", s.name, sinkNames(sinks), r.logs.String())
		}
	}
	start := time.Now()
	if err := r.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The documented bound is ShutdownGrace; the deliverers' own request timeout is as long, so a bound well under it is
	// what shows that the held requests are cancelled by the shutdown and not left to time out.
	if d := time.Since(start); d >= ShutdownGrace/2 {
		t.Fatalf("shutdown took %v with eight requests in flight; the bound is %v, and cancelling must take well under half of it", d, ShutdownGrace)
	}
}

// secretsConfig is a configuration whose endpoints' secret files hold the given contents (hex, as the files are written)
// and, with alert not empty, an alerts block whose secret file holds alert. The data directory does not exist yet.
func secretsConfig(t *testing.T, alert string, contents ...string) *Config {
	t.Helper()
	dir := t.TempDir()
	keyPath, apiPath, _ := writeFixtures(t, dir)
	m := goodConfig()
	m["data_dir"] = filepath.Join(dir, "data")
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	var list []any
	for i, c := range contents {
		p := filepath.Join(dir, fmt.Sprintf("wh%d.secret", i))
		if err := os.WriteFile(p, []byte(c+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		list = append(list, map[string]any{"name": fmt.Sprintf("ep%d", i), "url": fmt.Sprintf("http://127.0.0.1:9/h%d", i), "secret_file": p})
	}
	m["webhooks"] = list
	if alert != "" {
		p := filepath.Join(dir, "alerts.secret")
		if err := os.WriteFile(p, []byte(alert+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		m["alerts"] = map[string]any{"url": "http://127.0.0.1:9/alerts", "secret_file": p}
	}
	c, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Two endpoints whose secrets are one HMAC key (equal, or equal but for trailing zero bytes), and an endpoint whose secret
// is the alerts', are refused by CheckFiles and at start with a text that names the keys by position and no value;
// nothing is started and no data directory is made.
func TestEqualSecretsAreRefused(t *testing.T) {
	k := func(i int) string { return strings.Repeat(fmt.Sprintf("%02x", i), 32) }
	cases := []struct {
		name    string
		cfg     *Config
		reason  string
		text    string
		secrets []string
	}{
		{"equal", secretsConfig(t, "", k(1), k(2), k(3), k(2)), "webhook_secret_file",
			"webhooks[1].secret_file and webhooks[3].secret_file hold the same secret", []string{k(1), k(2), k(3)}},
		{"zero padded", secretsConfig(t, "", k(4), k(5)+"00"+"00", k(5)), "webhook_secret_file",
			"webhooks[1].secret_file and webhooks[2].secret_file hold the same secret", []string{k(4), k(5)}},
		{"alerts equal to the second", secretsConfig(t, k(7), k(6), k(7)), "alerts_secret_file",
			"alerts.secret_file must differ from webhooks[1].secret_file: use a secret of its own", []string{k(6), k(7)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.CheckFiles()
			if err == nil || err.Error() != "config: "+tc.text {
				t.Fatalf("CheckFiles: %v, want %q", err, tc.text)
			}
			err, logs := startRefuses(t, tc.cfg, tc.reason, tc.text, WithWallet(newFakeWallet()))
			for _, s := range tc.secrets {
				if strings.Contains(err.Error()+logs, s) {
					t.Fatal("a secret reached the error or the log")
				}
			}
			if _, statErr := os.Stat(tc.cfg.DataDir); !os.IsNotExist(statErr) {
				t.Fatalf("the data directory was made: %v", statErr)
			}
		})
	}
	// Distinct secrets pass.
	if err := secretsConfig(t, k(9), k(1), k(2), k(3)).CheckFiles(); err != nil {
		t.Fatalf("distinct secrets refused: %v", err)
	}
}

// The list's rules, in the order the checks run. A text names a key by its 0-based position (as in the file) and never
// carries a value: every URL below holds a marker that no text may contain.
func TestWebhooksListRules(t *testing.T) {
	const marker = "URL-MARKER-8f3a"
	ep := func(name, path string) map[string]any {
		return map[string]any{"name": name, "url": "https://hooks.example.com/" + marker + "/" + path, "secret_file": "/etc/satbooth/" + name + ".secret"}
	}
	list := func(es ...any) func(m map[string]any) { return func(m map[string]any) { m["webhooks"] = es } }
	many := func(n int) []any {
		var es []any
		for i := 0; i < n; i++ {
			es = append(es, ep(fmt.Sprintf("e%d", i), fmt.Sprintf("p%d", i)))
		}
		return es
	}
	with1 := func(i int, key string, v any) func(m map[string]any) {
		return func(m map[string]any) {
			es := many(3)
			es[i].(map[string]any)[key] = v
			m["webhooks"] = es
		}
	}
	cases := []struct {
		name string
		mod  func(m map[string]any)
		want string
	}{
		{"none", list(), "config: webhooks must list 1 to 8 endpoints"},
		{"absent", func(m map[string]any) { delete(m, "webhooks") }, "config: webhooks must list 1 to 8 endpoints"},
		{"null", func(m map[string]any) { m["webhooks"] = nil }, "config: webhooks must list 1 to 8 endpoints"},
		{"nine", list(many(9)...), "config: webhooks must list 1 to 8 endpoints"},
		{"duplicate name", with1(2, "name", "e0"), "config: webhooks[2].name repeats webhooks[0].name"},
		{"duplicate url", with1(2, "url", "https://hooks.example.com/"+marker+"/p1"), "config: webhooks[2].url repeats webhooks[1].url"},
		{"duplicate url, other spelling", with1(1, "url", "HTTPS://Hooks.Example.com:443/"+marker+"/p0"), "config: webhooks[1].url repeats webhooks[0].url"},
		{"duplicate file", with1(1, "secret_file", "/etc/satbooth/e0.secret"), "config: webhooks[1].secret_file repeats webhooks[0].secret_file"},
		{"empty name", with1(1, "name", ""), "config: webhooks[1].name must be 1 to 64 characters"},
		{"space in name", with1(0, "name", "a b"), "config: webhooks[0].name must be 1 to 64 characters"},
		{"slash in name", with1(0, "name", "a/b"), "config: webhooks[0].name must be 1 to 64 characters"},
		{"long name", with1(2, "name", strings.Repeat("n", 65)), "config: webhooks[2].name must be 1 to 64 characters"},
		{"non-ASCII name", with1(0, "name", "café"), "config: webhooks[0].name must be 1 to 64 characters"},
		{"bad scheme", with1(1, "url", "ftp://hooks.example.com/"+marker), "config: webhooks[1].url must be a http or https URL"},
		{"user info", with1(2, "url", "https://u:p@hooks.example.com/"+marker), "config: webhooks[2].url must be a http or https URL"},
		{"no url", with1(0, "url", ""), "config: webhooks[0].url must be a http or https URL"},
		{"no secret file", with1(1, "secret_file", ""), "config: webhooks[1].secret_file is missing"},
		{"old key", func(m map[string]any) {
			m["webhook"] = map[string]any{"url": "https://hooks.example.com/" + marker, "secret_file": "/etc/satbooth/webhook.secret"}
			delete(m, "webhooks")
		}, "config: webhook is replaced by webhooks (a list of endpoints)"},
		{"old key beside the new", func(m map[string]any) {
			m["webhook"] = map[string]any{"url": "https://hooks.example.com/" + marker, "secret_file": "/etc/satbooth/webhook.secret"}
		}, "config: webhook is replaced by webhooks (a list of endpoints)"},
		{"old key, any value", func(m map[string]any) { m["webhook"] = "https://hooks.example.com/" + marker }, "config: webhook is replaced by webhooks (a list of endpoints)"},
		{"alerts url equal to an endpoint's", func(m map[string]any) {
			list(many(3)...)(m)
			m["alerts"] = map[string]any{"url": "https://hooks.example.com:443/" + marker + "/p2", "secret_file": "/etc/satbooth/alerts.secret"}
		}, "config: alerts.url must differ from webhooks[2].url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := goodConfig()
			list(many(3)...)(m)
			tc.mod(m)
			_, err := ParseConfig(enc(t, m))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("%q, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "hooks.example.com") {
				t.Fatalf("the message carries a URL: %q", err)
			}
		})
	}
	// Eight is the most; one is the least; a different alerts URL is fine.
	for _, n := range []int{1, 8} {
		m := goodConfig()
		list(many(n)...)(m)
		m["alerts"] = map[string]any{"url": "https://monitor.example.com/alert", "secret_file": "/etc/satbooth/alerts.secret"}
		if _, err := ParseConfig(enc(t, m)); err != nil {
			t.Errorf("%d endpoints: %v", n, err)
		}
	}
	// Inside an entry the generic texts every block gets, with no value: an unknown key, a value of the wrong type, an entry
	// that is not an object, and the list itself an object.
	const shape = "config: unknown, duplicate or misspelt key, or a value of the wrong type"
	const notObject = "config: not a valid configuration object"
	good := func() map[string]any {
		return map[string]any{"name": "zq7", "url": "https://zq7.example.com/x", "secret_file": "/zq7"}
	}
	withKey := func(k string, v any) any { e := good(); e[k] = v; return e }
	for name, c := range map[string]struct {
		webhooks any
		want     string
	}{
		"extra key":         {[]any{withKey("events", 1)}, shape},
		"url is a list":     {[]any{withKey("url", []any{"zq7"})}, shape},
		"url is an object":  {[]any{withKey("url", map[string]any{"zq7": 1})}, shape},
		"name is a number":  {[]any{withKey("name", 5)}, notObject},
		"entry is a string": {[]any{"zq7"}, notObject},
		"list is an object": {map[string]any{"zq7": 1}, shape},
	} {
		m := goodConfig()
		m["webhooks"] = c.webhooks
		_, err := ParseConfig(enc(t, m))
		if err == nil || err.Error() != c.want || strings.Contains(strings.ToLower(err.Error()), "zq7") {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// The old key in a file that is not one valid JSON value is not recognised: the generic text, and still no value.
	_, err := ParseConfig([]byte(`{"webhook":{"url":"https://zq7.example.com"}} x`))
	if err == nil || err.Error() != shape {
		t.Errorf("old key in a malformed file: %v, want %q", err, shape)
	}
}

// For names valid and invalid, the configuration check, store.ValidEndpoint and webhook.NewEventDeliverers (given a
// valid URL and secret) agree: a configuration that ParseConfig accepts is never refused when the deliverers are built.
func TestWebhookNameRuleAgreesEverywhere(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"a", true}, {"main", true}, {"A.b_c-9", true}, {"-", true}, {".", true}, {"_", true}, {strings.Repeat("x", 64), true},
		{"", false}, {strings.Repeat("x", 65), false}, {"a b", false}, {"a/b", false}, {"a:b", false}, {"a,b", false},
		{"caf\u00e9", false}, {"\u0661", false}, {"a\nb", false}, {"a\x00b", false}, {" a", false}, {"a ", false},
		{"a\u200b", false}, {"A\uff21", false},
	} {
		m := goodConfig()
		m["webhooks"] = []any{map[string]any{"name": tc.name, "url": "https://h.example.com/x", "secret_file": "/s"}}
		_, parseErr := ParseConfig(enc(t, m))
		_, buildErr := webhook.NewEventDeliverers([]webhook.Endpoint{{Name: tc.name, URL: "https://h.example.com/x", Secret: make([]byte, 32)}}, st, time.Now, nil, nil)
		if got := store.ValidEndpoint(tc.name); got != tc.ok {
			t.Errorf("%q: the store says %v, want %v", tc.name, got, tc.ok)
		}
		if (parseErr == nil) != tc.ok || (buildErr == nil) != tc.ok {
			t.Errorf("%q: want accepted=%v; ParseConfig: %v; NewEventDeliverers: %v", tc.name, tc.ok, parseErr, buildErr)
		}
	}
}

// A refusal for a bad name, a duplicate name, a duplicate URL and a duplicate secret file is the fixed text and echoes no
// value (every value below carries the marker). The property test covers the same rules over random lists.
func TestWebhookRefusalsDoNotEchoValues(t *testing.T) {
	e := func(name, url, file string) map[string]any {
		return map[string]any{"name": name, "url": url, "secret_file": file}
	}
	for name, c := range map[string]struct {
		list []any
		want string
	}{
		"bad name":       {[]any{e("zq7 name", "https://zq7a.example.com/p", "/zq7/a")}, "config: webhooks[0].name must be 1 to 64 characters of A-Z a-z 0-9 . _ -"},
		"duplicate name": {[]any{e("zq7", "https://zq7a.example.com/p", "/zq7/a"), e("zq7", "https://zq7b.example.com/p", "/zq7/b")}, "config: webhooks[1].name repeats webhooks[0].name"},
		"duplicate url": {[]any{e("zq7a", "https://zq7a.example.com/p?zq7=1", "/zq7/a"), e("zq7b", "HTTPS://ZQ7A.example.com:443/p?zq7=1", "/zq7/b")},
			"config: webhooks[1].url repeats webhooks[0].url"},
		"duplicate file": {[]any{e("zq7a", "https://zq7a.example.com/p", "/zq7/a"), e("zq7b", "https://zq7b.example.com/p", "/zq7/a")},
			"config: webhooks[1].secret_file repeats webhooks[0].secret_file"},
	} {
		m := goodConfig()
		m["webhooks"] = c.list
		_, err := ParseConfig(enc(t, m))
		if err == nil || err.Error() != c.want || strings.Contains(strings.ToLower(err.Error()), "zq7") {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// An endpoint removed and added back is the same endpoint (plan 13), not a new one. An event that was created while it
// was out and is more than 24 hours old when it comes back is sent to it exactly once (its one last attempt, which fails
// here, so its abandoned line appears exactly once, naming it); an endpoint that is new at that same start is not sent
// the event at all.
func TestReAddedEndpointGetsItsOneLastAttempt(t *testing.T) {
	a, b, g := newSink(t, "alpha"), newSink(t, "beta"), newSink(t, "gamma")
	clk := &testClock{}
	n := time.Now().Truncate(time.Second)
	clk.Set(n)
	r := startRigWith(t, webhooksMod(t, a, b), WithClock(clk.Now), withDeliveryFallback(shortFallback))
	o := r.createOrder("old")
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	// beta ran once (its first pass is at n), then is out while the event is created.
	e := seedExpired(t, r.cfg.DataDir, clk, o.ID, n.Add(-time.Hour))
	clk.Set(n)
	p := startPhase(t, withSinks(t, r.cfg, r.dir, a), WithClock(clk.Now), withDeliveryFallback(shortFallback))
	eventuallyWithin(t, "alpha to get the event", func() bool { return a.count(e) == 1 })
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	// 25 hours on the event is 26 hours old. beta (failing) is back, and gamma is new.
	b.setStatus(http.StatusInternalServerError)
	clk.Set(n.Add(25 * time.Hour))
	p = startPhase(t, withSinks(t, r.cfg, r.dir, a, b, g), WithClock(clk.Now), withDeliveryFallback(shortFallback))
	eventuallyWithin(t, "beta's abandoned line", func() bool { return strings.Contains(p.logs.String(), webhook.GiveUpMessage) })
	time.Sleep(1500 * time.Millisecond)
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if got := b.count(e); got != 1 {
		t.Fatalf("beta was sent the old event %d times, want its one last attempt", got)
	}
	if got := strings.Count(p.logs.String(), webhook.GiveUpMessage); got != 1 {
		t.Fatalf("%d abandoned lines, want 1", got)
	}
	for _, l := range strings.Split(p.logs.String(), "\n") {
		if strings.Contains(l, webhook.GiveUpMessage) && !strings.Contains(l, `"endpoint":"beta"`) {
			t.Fatalf("the abandoned line does not name beta: %s", l)
		}
	}
	if g.count(e) != 0 {
		t.Fatal("an endpoint new at this start was sent an event older than its 24 hours")
	}
	if a.count(e) != 1 {
		t.Fatalf("alpha was sent the event %d times", a.count(e))
	}
}
