package relay

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/satbooth/satbooth/nostr"
)

// srvConn is a test WebSocket client with a reader goroutine, so a quiet period can be tested without
// cancelling a Read (which would close the connection).
type srvConn struct {
	t      *testing.T
	c      *websocket.Conn
	frames chan string
	done   chan struct{}
	errMu  sync.Mutex
	err    error
}

func srvDial(t *testing.T, url string) *srvConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return srvWrap(t, c)
}

func srvWrap(t *testing.T, c *websocket.Conn) *srvConn {
	c.SetReadLimit(1 << 20)
	x := &srvConn{t: t, c: c, frames: make(chan string, 4096), done: make(chan struct{})}
	go func() {
		defer close(x.done)
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				x.errMu.Lock()
				x.err = err
				x.errMu.Unlock()
				return
			}
			x.frames <- string(b)
		}
	}()
	t.Cleanup(func() { c.CloseNow() })
	return x
}

func (x *srvConn) send(b []byte) {
	x.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := x.c.Write(ctx, websocket.MessageText, b); err != nil {
		x.t.Fatalf("send: %v", err)
	}
}

func (x *srvConn) tryRecv(d time.Duration) (string, bool) {
	select {
	case f := <-x.frames:
		return f, true
	case <-time.After(d):
		return "", false
	}
}

func (x *srvConn) recv(d time.Duration) string {
	x.t.Helper()
	f, ok := x.tryRecv(d)
	if !ok {
		x.errMu.Lock()
		err := x.err
		x.errMu.Unlock()
		x.t.Fatalf("no frame within %v (conn error: %v)", d, err)
	}
	return f
}

// none asserts that no frame arrives within d.
func (x *srvConn) none(d time.Duration) {
	x.t.Helper()
	if f, ok := x.tryRecv(d); ok {
		x.t.Fatalf("unexpected frame: %.200s", f)
	}
}

// closed waits for the connection to end and returns the close status (-1 when there was none).
func (x *srvConn) closed(d time.Duration) websocket.StatusCode {
	x.t.Helper()
	select {
	case <-x.done:
		return websocket.CloseStatus(x.err)
	case <-time.After(d):
		x.t.Fatal("connection not closed")
		return 0
	}
}

// srvEnv is a running server with both listeners on loopback.
type srvEnv struct {
	t              *testing.T
	cfg            *Config
	clock          *winClock
	s              *Server
	hubAddr, cAddr string
	hubSrv, cSrv   *httptest.Server
	hubL, cL       *gateListener
	hook           func(*Server)
}

// gateConn is the server side of an accepted connection. While stalled, Write blocks, so the relay's writer
// is stuck by construction, whatever the OS socket buffers hold. Reads are never gated.
type gateConn struct {
	net.Conn
	mu     sync.Mutex
	gate   chan struct{} // non-nil while stalled; closed by release
	wdl    time.Time
	closed chan struct{}
	once   sync.Once
}

func (c *gateConn) stall() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gate == nil {
		c.gate = make(chan struct{})
	}
}

func (c *gateConn) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gate != nil {
		close(c.gate)
		c.gate = nil
	}
}

func (c *gateConn) Write(b []byte) (int, error) {
	for {
		c.mu.Lock()
		g, dl := c.gate, c.wdl
		c.mu.Unlock()
		if g == nil {
			return c.Conn.Write(b)
		}
		var expired <-chan time.Time
		if !dl.IsZero() {
			t := time.NewTimer(time.Until(dl))
			defer t.Stop()
			expired = t.C
		}
		select {
		case <-g: // released: look again
		case <-c.closed:
			return 0, net.ErrClosed
		case <-expired:
			return 0, os.ErrDeadlineExceeded
		}
	}
}

func (c *gateConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.wdl = t
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *gateConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.wdl = t
	c.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

func (c *gateConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// gateListener records the connections it accepts, in order.
type gateListener struct {
	net.Listener
	mu    sync.Mutex
	conns []*gateConn
}

func (l *gateListener) Accept() (net.Conn, error) {
	nc, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	c := &gateConn{Conn: nc, closed: make(chan struct{})}
	l.mu.Lock()
	l.conns = append(l.conns, c)
	l.mu.Unlock()
	return c, nil
}

// last returns the connection accepted most recently. The server accepts before it answers the WebSocket
// handshake, so right after srvDial returns it is the server side of that dial.
func (l *gateListener) last() *gateConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conns[len(l.conns)-1]
}

// stallHub stops the relay's writes on the hub connection dialled last; the returned func lets them go on.
func (e *srvEnv) stallHub() (release func()) {
	g := e.hubL.last()
	g.stall()
	return g.release
}

func srvStart(t *testing.T, hook func(*Server)) *srvEnv {
	t.Helper()
	cfg, err := ParseConfig(marshal(t, validMap(t, 2)))
	if err != nil {
		t.Fatal(err)
	}
	e := &srvEnv{t: t, cfg: cfg, clock: &winClock{t: polNow}, hook: hook}
	e.up("127.0.0.1:0", "127.0.0.1:0")
	t.Cleanup(e.stop)
	return e
}

func srvListen(t *testing.T, addr string) net.Listener {
	var l net.Listener
	var err error
	for i := 0; i < 50; i++ { // the old port may need a moment after a restart
		if l, err = net.Listen("tcp", addr); err == nil {
			return l
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("listen %s: %v", addr, err)
	return nil
}

func (e *srvEnv) up(hubAddr, cAddr string) {
	e.s = NewServer(e.cfg, nil, e.clock.now)
	if e.hook != nil {
		e.hook(e.s)
	}
	e.hubSrv = httptest.NewUnstartedServer(e.s.Handler(HubListener))
	e.hubSrv.Listener.Close()
	e.hubL = &gateListener{Listener: srvListen(e.t, hubAddr)}
	e.hubSrv.Listener = e.hubL
	e.hubSrv.Start()
	e.cSrv = httptest.NewUnstartedServer(e.s.Handler(ClientListener))
	e.cSrv.Listener.Close()
	e.cL = &gateListener{Listener: srvListen(e.t, cAddr)}
	e.cSrv.Listener = e.cL
	e.cSrv.Start()
	e.hubAddr, e.cAddr = e.hubL.Addr().String(), e.cL.Addr().String()
}

func (e *srvEnv) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	e.s.Shutdown(ctx)
	e.hubSrv.Close()
	e.cSrv.Close()
}

func (e *srvEnv) restart() {
	e.stop()
	e.up(e.hubAddr, e.cAddr)
}

func (e *srvEnv) hubURL() string { return "ws://" + e.hubAddr }
func (e *srvEnv) cURL() string   { return "ws://" + e.cAddr }
func (e *srvEnv) wallet(i int) string {
	return testKey(e.t, polW(i))
}
func (e *srvEnv) client(i int) string { return testKey(e.t, polC(i)) }
func (e *srvEnv) nowUnix() int64      { return e.clock.now().Unix() }

func srvSecret(t testing.TB, n int) *nostr.SecretKey {
	k, err := nostr.ParseSecretKeyHex(fmt.Sprintf("%064x", n))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// srvAuthEvent answers a challenge as key n for relayURL.
func (e *srvEnv) authEvent(n int, relayURL, challenge string) *nostr.Event {
	a, err := nostr.NewAuth(srvSecret(e.t, n), relayURL, challenge, e.clock.now())
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func srvEventFrame(e *nostr.Event) []byte { return []byte(`["EVENT",` + string(eventJSON(e)) + `]`) }
func srvAuthFrame(e *nostr.Event) []byte  { return []byte(`["AUTH",` + string(eventJSON(e)) + `]`) }

// challenge reads the AUTH frame a client connection starts with.
func srvChallenge(t *testing.T, x *srvConn) string {
	t.Helper()
	f := x.recv(2 * time.Second)
	if !strings.HasPrefix(f, `["AUTH","`) || len(f) != len(`["AUTH",""]`)+64 {
		t.Fatalf("expected an AUTH challenge of 64 characters, got %s", f)
	}
	return f[len(`["AUTH","`) : len(f)-2]
}

// dialClient connects to the private listener and authenticates as client i.
func (e *srvEnv) dialClient(i int) *srvConn {
	e.t.Helper()
	x := srvDial(e.t, e.cURL())
	ch := srvChallenge(e.t, x)
	a := e.authEvent(polC(i), e.cfg.ClientRelayURL, ch)
	x.send(srvAuthFrame(a))
	if id, ok, msg := frameOK(e.t, x.recv(2*time.Second)); id != a.ID || !ok || msg != "" {
		e.t.Fatalf("AUTH answer: %v %q", ok, msg)
	}
	return x
}

func (e *srvEnv) subResponses(x *srvConn, sub string, i int) {
	e.t.Helper()
	x.send([]byte(`["REQ","` + sub + `",{"kinds":[23195,23197],"#p":["` + e.client(i) + `"]}]`))
}

func (e *srvEnv) subRequests(x *srvConn, sub string, i int) {
	e.t.Helper()
	x.send([]byte(`["REQ","` + sub + `",{"kinds":[23194],"#p":["` + e.wallet(i) + `"]}]`))
}

func srvPayload(n, size int) string {
	const a = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := []byte(strings.Repeat("A", size))
	b[0], b[1], b[2] = a[n%64], a[(n/64)%64], a[(n/4096)%64]
	return string(b)
}

// request builds a signed 23194 from client i to wallet j; exp 0 omits the expiration.
func (e *srvEnv) request(i, j, salt int, exp int64) *nostr.Event {
	tags := polTags(nostr.KindNWCRequest, e.wallet(j))
	if exp != 0 {
		tags = append(tags, []string{"expiration", strconv.FormatInt(exp, 10)})
	}
	return polSigned(e.t, polC(i), nostr.KindNWCRequest, e.nowUnix(), tags, srvPayload(salt, 132))
}

// response builds a signed 23195 from wallet i to client j.
func (e *srvEnv) response(i, j, salt int) *nostr.Event {
	return polSigned(e.t, polW(i), nostr.KindNWCResponse, e.nowUnix(), polTags(nostr.KindNWCResponse, e.client(j)), srvPayload(salt, 132))
}

func (e *srvEnv) publishOn(x *srvConn, ev *nostr.Event) (bool, string) {
	e.t.Helper()
	x.send(srvEventFrame(ev))
	id, ok, msg := frameOK(e.t, x.recv(3*time.Second))
	if id != ev.ID {
		e.t.Fatalf("OK for another event")
	}
	return ok, msg
}

func srvWantFrame(t *testing.T, got string, sub string, ev *nostr.Event) {
	t.Helper()
	if want := string(FrameEvent(sub, ev)); got != want {
		t.Fatalf("frame\n got %.300s\nwant %.300s", got, want)
	}
}

func TestHubToClientRoundTrip(t *testing.T) {
	e := srvStart(t, nil)
	hub := newHubClient(t, e.hubURL(), e.wallet(0))
	hub.connect(nil)
	if evs := hub.eose(); len(evs) != 0 {
		t.Fatalf("empty relay served %d events", len(evs))
	}
	cl := e.dialClient(0)
	e.subResponses(cl, "r", 0)
	if f := cl.recv(2 * time.Second); f != `["EOSE","r"]` {
		t.Fatal(f)
	}
	req := e.request(0, 0, 1, e.nowUnix()+60)
	if ok, msg := e.publishOn(cl, req); !ok || msg != "" {
		t.Fatalf("request refused: %q", msg)
	}
	got := hub.next(2 * time.Second)
	if got == nil || got.ID != req.ID {
		t.Fatal("hub did not receive the request")
	}
	resp := e.response(0, 0, 2)
	if ok, msg := hub.publish(resp); !ok || msg != "" {
		t.Fatalf("response refused: %q", msg)
	}
	srvWantFrame(t, cl.recv(2*time.Second), "r", resp)
}

func TestAuthRequired(t *testing.T) {
	e := srvStart(t, nil)
	x := srvDial(t, e.cURL())
	ch := srvChallenge(t, x)
	req := e.request(0, 0, 1, e.nowUnix()+60)

	// Before AUTH: events and REQs are refused with auth-required.
	if ok, msg := e.publishOn(x, req); ok || !strings.HasPrefix(msg, "auth-required:") {
		t.Fatalf("event before AUTH: %v %q", ok, msg)
	}
	e.subResponses(x, "r", 0)
	if f := x.recv(2 * time.Second); !strings.HasPrefix(f, `["CLOSED","r","auth-required:`) {
		t.Fatalf("REQ before AUTH: %s", f)
	}
	// Wrong relay URL, wrong challenge, an unpaired key: all refused, and the connection stays unauthed.
	bad := map[string]struct {
		ev     *nostr.Event
		prefix string
	}{
		"wrong relay":     {e.authEvent(polC(0), "ws://192.0.2.99:1", ch), "invalid:"},
		"wrong challenge": {e.authEvent(polC(0), e.cfg.ClientRelayURL, strings.Repeat("0", 64)), "invalid:"},
		"unpaired key":    {e.authEvent(polOutsider, e.cfg.ClientRelayURL, ch), "restricted:"},
		"wallet key":      {e.authEvent(polW(0), e.cfg.ClientRelayURL, ch), "restricted:"},
	}
	for name, b := range bad {
		a := b.ev
		if ok, msg := e.publishAuth(x, a); ok || !strings.HasPrefix(msg, b.prefix) {
			t.Fatalf("%s: %v %q", name, ok, msg)
		}
		if ok, msg := e.publishOn(x, req); ok || !strings.HasPrefix(msg, "auth-required:") {
			t.Fatalf("event after %s AUTH: %v %q", name, ok, msg)
		}
	}
	// The right answer works.
	good := e.authEvent(polC(0), e.cfg.ClientRelayURL, ch)
	if ok, msg := e.publishAuth(x, good); !ok || msg != "" {
		t.Fatalf("good AUTH: %v %q", ok, msg)
	}
	if ok, msg := e.publishOn(x, req); !ok || msg != "" {
		t.Fatalf("event after AUTH: %v %q", ok, msg)
	}
	// A challenge belongs to one connection.
	y := srvDial(t, e.cURL())
	srvChallenge(t, y)
	if ok, _ := e.publishAuth(y, good); ok {
		t.Fatal("an AUTH answer for another connection's challenge was accepted")
	}
	// The hub listener does not use AUTH, and sends no challenge.
	h := srvDial(t, e.hubURL())
	h.none(100 * time.Millisecond)
	if ok, msg := e.publishAuth(h, good); ok || msg != "restricted: AUTH is not used on this listener" {
		t.Fatalf("hub AUTH: %v %q", ok, msg)
	}
}

func (e *srvEnv) publishAuth(x *srvConn, a *nostr.Event) (bool, string) {
	e.t.Helper()
	x.send(srvAuthFrame(a))
	id, ok, msg := frameOK(e.t, x.recv(2*time.Second))
	if id != a.ID {
		e.t.Fatal("OK for another event")
	}
	return ok, msg
}

func TestCrossPairIsolation(t *testing.T) {
	e := srvStart(t, nil)
	c0, c1 := e.dialClient(0), e.dialClient(1)
	h0, h1 := srvDial(t, e.hubURL()), srvDial(t, e.hubURL())
	e.subRequests(h0, "a", 0)
	e.subRequests(h1, "b", 1)
	e.subResponses(c0, "r", 0)
	e.subResponses(c1, "r", 1)
	for _, x := range []*srvConn{h0, h1, c0, c1} {
		if f := x.recv(2 * time.Second); !strings.HasPrefix(f, `["EOSE"`) {
			t.Fatal(f)
		}
	}
	// A client cannot address the other pair's wallet, or read the other pair's responses.
	if ok, msg := e.publishOn(c0, e.request(0, 1, 1, 0)); ok || !strings.HasPrefix(msg, "restricted:") {
		t.Fatalf("cross request: %v %q", ok, msg)
	}
	c0.send([]byte(`["REQ","x",{"kinds":[23195],"#p":["` + e.client(1) + `"]}]`))
	if f := c0.recv(2 * time.Second); !strings.HasPrefix(f, `["CLOSED","x","restricted:`) {
		t.Fatal(f)
	}
	// A wallet cannot answer the other pair's client.
	h0.send(srvEventFrame(e.response(0, 1, 1)))
	if _, ok, msg := frameOK(t, h0.recv(2*time.Second)); ok || !strings.HasPrefix(msg, "restricted:") {
		t.Fatalf("cross response: %v %q", ok, msg)
	}
	// Normal traffic reaches only its own pair.
	r1 := e.request(1, 1, 2, 0)
	if ok, _ := e.publishOn(c1, r1); !ok {
		t.Fatal("pair 1 request refused")
	}
	srvWantFrame(t, h1.recv(2*time.Second), "b", r1)
	h0.none(200 * time.Millisecond)
	p0 := e.response(0, 0, 3)
	h0.send(srvEventFrame(p0))
	frameOK(t, h0.recv(2*time.Second))
	srvWantFrame(t, c0.recv(2*time.Second), "r", p0)
	c1.none(200 * time.Millisecond)
}

// srvStepClock returns a fixed time until armed; after that the first call returns the fixed time and
// every later call returns it plus step.
type srvStepClock struct {
	mu    sync.Mutex
	t     time.Time
	step  time.Duration
	armed bool
	calls int
}

func (c *srvStepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		return c.t
	}
	c.calls++
	if c.calls == 1 {
		return c.t
	}
	return c.t.Add(c.step)
}

func TestExpiredRequestNeverDelivered(t *testing.T) {
	e := srvStart(t, nil)
	cl := e.dialClient(0)
	hub := srvDial(t, e.hubURL())
	e.subRequests(hub, "1:", 0)
	hub.recv(2 * time.Second)

	// Already expired at receipt: refused, nobody sees it.
	exp := e.request(0, 0, 1, e.nowUnix())
	if ok, msg := e.publishOn(cl, exp); ok || msg != "invalid: event has expired" {
		t.Fatalf("expired at receipt: %v %q", ok, msg)
	}
	hub.none(200 * time.Millisecond)

	// Expires after the receipt check and before the broadcast (the clock advances inside Add and fan-out).
	// Receipt must pass (the expiration is one second ahead of the receipt time).
	e.stop()
	sc := &srvStepClock{t: polNow, step: 2 * time.Second}
	e.clock = &winClock{t: polNow}
	e.hook = func(s *Server) {
		s.now = sc.now
		s.win = NewWindow(s.c.lifetime(), s.c.WindowMaxEvents, s.c.WindowMaxBytes, sc.now)
	}
	e.up(e.hubAddr, e.cAddr)
	cl = e.dialClient(0)
	hub = srvDial(t, e.hubURL())
	e.subRequests(hub, "1:", 0)
	hub.recv(2 * time.Second)
	mid := e.request(0, 0, 2, polNow.Unix()+1)
	sc.mu.Lock()
	sc.armed = true
	sc.mu.Unlock()
	ok, msg := e.publishOn(cl, mid)
	if !ok || msg != "" {
		t.Fatalf("mid-flight event: %v %q (the receipt check passes: expiration is one second ahead)", ok, msg)
	}
	hub.none(300 * time.Millisecond)
	// And a later REQ never serves it either.
	hub2 := srvDial(t, e.hubURL())
	e.subRequests(hub2, "2:", 0)
	if f := hub2.recv(2 * time.Second); f != `["EOSE","2:"]` {
		t.Fatalf("expired event served on REQ: %.200s", f)
	}
	if sc.calls < 2 {
		t.Fatal("the clock never advanced past the receipt check")
	}
}

func TestStalledReader(t *testing.T) {
	e := srvStart(t, func(s *Server) { s.queueFrames = 4 })
	sender := e.dialClient(0)
	stalled := srvDial(t, e.hubURL())
	// The relay's writes to the stalled client block from the start; a healthy one reads everything.
	release := e.stallHub()
	stalled.send([]byte(`["REQ","1:",{"kinds":[23194],"#p":["` + e.wallet(0) + `"]}]`))
	healthy := srvDial(t, e.hubURL())
	e.subRequests(healthy, "1:", 0)
	if f := healthy.recv(2 * time.Second); f != `["EOSE","1:"]` {
		t.Fatal(f)
	}
	// Both subscriptions must be registered before the first publish.
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		e.s.mu.Lock()
		subs := 0
		for c := range e.s.conns {
			if c.l == HubListener {
				subs += len(c.subs)
			}
		}
		e.s.mu.Unlock()
		if subs == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hub subscriptions are not registered")
		}
	}
	const n = 120
	var got atomic.Int32
	var mu sync.Mutex
	arrived := map[string]time.Time{}
	go func() {
		for f := range healthy.frames {
			if strings.HasPrefix(f, `["EVENT"`) {
				mu.Lock()
				arrived[f[len(`["EVENT","1:",{"id":"`):][:64]] = time.Now()
				mu.Unlock()
			}
			got.Add(1) // after the arrival is recorded: the main goroutine waits on this count
		}
	}()
	big := 80000
	published := map[string]time.Time{}
	for i := 0; i < n; i++ {
		ev := polSigned(t, polC(0), nostr.KindNWCRequest, e.nowUnix(), polTags(nostr.KindNWCRequest, e.wallet(0)), srvPayload(i+10, big))
		published[ev.ID] = time.Now()
		if ok, msg := e.publishOn(sender, ev); !ok {
			t.Fatalf("publish %d: %q", i, msg)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for got.Load() < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got.Load() != n {
		t.Fatalf("healthy subscriber got %d of %d: a stalled reader held it up", got.Load(), n)
	}
	// Every broadcast reached the healthy subscriber within 1 s.
	mu.Lock()
	for id, p := range published {
		if a, ok := arrived[id]; !ok || a.Sub(p) > time.Second {
			t.Errorf("event %.8s: arrived=%v after %v", id, ok, a.Sub(p))
		}
	}
	mu.Unlock()
	// The stalled connection is closed with 1013 once the relay can write to it again.
	release()
	if st := stalled.closed(8 * time.Second); st != websocket.StatusTryAgainLater {
		t.Fatalf("stalled reader close status = %d, want 1013", st)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e.s.mu.Lock()
		n := len(e.s.conns)
		e.s.mu.Unlock()
		if n == 2 { // sender and healthy; the stalled one is gone
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the stalled connection is still registered")
}

func TestHubReconnectSince(t *testing.T) {
	e := srvStart(t, nil)
	cl := e.dialClient(0)
	hub := newHubClient(t, e.hubURL(), e.wallet(0))
	hub.connect(nil)
	hub.eose()
	r1 := e.request(0, 0, 1, 0)
	e.publishOn(cl, r1)
	if g := hub.next(2 * time.Second); g == nil || g.ID != r1.ID {
		t.Fatal("r1 not delivered")
	}
	// The hub drops one second after r1. Requests arrive while it is away: one in the same second as the
	// drop (created_at == since, so it is replayed), one later.
	e.clock.add(time.Second)
	dropAt := e.nowUnix()
	hub.drop()
	r2 := e.request(0, 0, 2, 0)
	e.publishOn(cl, r2)
	e.clock.add(5 * time.Second)
	r3 := e.request(0, 0, 3, 0)
	e.publishOn(cl, r3)
	hub.connect(&dropAt)
	evs := hub.eose()
	if len(evs) != 2 || evs[0].ID != r2.ID || evs[1].ID != r3.ID {
		t.Fatalf("replay = %d events, want exactly r2 then r3 (r1 is older than since)", len(evs))
	}
	for _, ev := range evs {
		if ev.CreatedAt < dropAt {
			t.Fatalf("replayed an event older than since")
		}
	}
	// Live delivery continues, and nothing is delivered twice.
	r4 := e.request(0, 0, 4, 0)
	e.publishOn(cl, r4)
	if g := hub.next(2 * time.Second); g == nil || g.ID != r4.ID {
		t.Fatal("r4 not delivered after the reconnect")
	}
	if g := hub.next(300 * time.Millisecond); g != nil {
		t.Fatalf("extra delivery %.8s", g.ID)
	}
}

func TestDuplicatePublish(t *testing.T) {
	e := srvStart(t, nil)
	cl := e.dialClient(0)
	e.subResponses(cl, "r", 0)
	cl.recv(2 * time.Second)
	hub := newHubClient(t, e.hubURL(), e.wallet(0))
	hub.connect(nil)
	hub.eose()
	resp := e.response(0, 0, 1)
	if ok, msg := hub.publish(resp); !ok || msg != "" {
		t.Fatalf("first: %v %q", ok, msg)
	}
	if ok, msg := hub.publish(resp); !ok || msg != "duplicate: already have this event" {
		t.Fatalf("second: %v %q", ok, msg)
	}
	srvWantFrame(t, cl.recv(2*time.Second), "r", resp)
	cl.none(300 * time.Millisecond)
	// Same for a request.
	req := e.request(0, 0, 2, 0)
	e.publishOn(cl, req)
	if ok, msg := e.publishOn(cl, req); !ok || msg != "duplicate: already have this event" {
		t.Fatalf("dup request: %v %q", ok, msg)
	}
	if g := hub.next(2 * time.Second); g == nil || g.ID != req.ID {
		t.Fatal("request not delivered")
	}
	if g := hub.next(300 * time.Millisecond); g != nil {
		t.Fatal("request delivered twice")
	}
}

// srvEventOfSize builds a signed 23194 from client 0 whose JSON is exactly size bytes.
//
// Sign refuses an event over MaxEventJSON, so a larger one is a signed event at the limit with its content
// padded afterwards: its signature is stale, which does not matter because it is refused before any check.
func (e *srvEnv) eventOfSize(size int) *nostr.Event {
	if size > nostr.MaxEventJSON {
		ev := e.eventOfSize(nostr.MaxEventJSON)
		ev.Content += strings.Repeat("A", size-nostr.MaxEventJSON)
		if len(eventJSON(ev)) != size {
			e.t.Fatalf("event size %d, want %d", len(eventJSON(ev)), size)
		}
		return ev
	}
	ev := polSigned(e.t, polC(0), nostr.KindNWCRequest, e.nowUnix(), polTags(nostr.KindNWCRequest, e.wallet(0)), srvPayload(1, 132))
	pad := size - len(eventJSON(ev))
	ev = polSigned(e.t, polC(0), nostr.KindNWCRequest, e.nowUnix(), polTags(nostr.KindNWCRequest, e.wallet(0)), srvPayload(1, 132+pad))
	if len(eventJSON(ev)) != size {
		e.t.Fatalf("event size %d, want %d", len(eventJSON(ev)), size)
	}
	return ev
}

func TestSizeEdges(t *testing.T) {
	e := srvStart(t, nil)
	cl := e.dialClient(0)
	// An event of exactly MaxEventJSON is read and judged (its payload is over the NWC grammar's limit, so
	// the verdict is a refusal, but it is an OK for that id, not a malformed-message notice).
	max := e.eventOfSize(nostr.MaxEventJSON)
	cl.send(srvEventFrame(max))
	if id, ok, _ := frameOK(t, cl.recv(3*time.Second)); id != max.ID || ok {
		t.Fatalf("max event: id match %v ok %v", id == max.ID, ok)
	}
	// One byte more is a malformed message and gets a NOTICE with no OK: the id of an event that was not
	// parsed is not trusted. The connection survives.
	over := e.eventOfSize(nostr.MaxEventJSON + 1)
	cl.send(srvEventFrame(over))
	if f := cl.recv(3 * time.Second); f != `["NOTICE","invalid: malformed message"]` {
		t.Fatalf("oversize event: %.100s", f)
	}
	// A frame of exactly MaxMessage bytes is served.
	req := `["REQ","s",{"kinds":[23195],"#p":["` + e.client(0) + `"]}`
	pad := strings.Repeat(" ", MaxMessage-len(req)-1)
	cl.send([]byte(req + pad + "]"))
	if f := cl.recv(3 * time.Second); f != `["EOSE","s"]` {
		t.Fatalf("MaxMessage REQ: %.100s", f)
	}
	// One byte more: the connection is closed with 1009.
	cl.send([]byte(req + pad + " ]"))
	if st := cl.closed(3 * time.Second); st != websocket.StatusMessageTooBig {
		t.Fatalf("close status = %d, want 1009", st)
	}
}

func TestMalformedKeepsConnection(t *testing.T) {
	e := srvStart(t, nil)
	cl := e.dialClient(0)
	h64 := strings.Repeat("a", 64)
	cases := []struct{ in, want string }{
		{`{`, `["NOTICE","invalid: malformed message"]`},
		{`[]`, `["NOTICE","invalid: malformed message"]`},
		{`["FOO"]`, `["NOTICE","invalid: malformed message"]`},
		{`["EVENT",{}]`, `["NOTICE","invalid: malformed message"]`},
		{`["REQ","bad id",{}]`, `["NOTICE","invalid: bad subscription id"]`},
		{`["CLOSE",""]`, `["NOTICE","invalid: bad subscription id"]`},
		{`["REQ","s",{"ids":["` + h64 + `"]}]`, `["CLOSED","s","invalid: unsupported filter field ids"]`},
		{`["REQ","s",{"kinds":[1.5]}]`, `["CLOSED","s","invalid: malformed message"]`},
		{`["REQ","s"]`, `["CLOSED","s","invalid: malformed message"]`},
		{`["REQ","s",{}]x`, `["CLOSED","s","invalid: malformed message"]`},
		{`["REQ","s",{},{},{},{},{}]`, `["CLOSED","s","blocked: too many filters"]`},
	}
	for _, c := range cases {
		cl.send([]byte(c.in))
		if f := cl.recv(2 * time.Second); f != c.want {
			t.Errorf("%s -> %s, want %s", c.in, f, c.want)
		}
	}
	// A binary frame gets a notice.
	if err := cl.c.Write(context.Background(), websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if f := cl.recv(2 * time.Second); !strings.HasPrefix(f, `["NOTICE"`) {
		t.Errorf("binary: %s", f)
	}
	// The connection still works, and is still authenticated.
	e.subResponses(cl, "ok", 0)
	if f := cl.recv(2 * time.Second); f != `["EOSE","ok"]` {
		t.Fatalf("after garbage: %s", f)
	}
	if ok, _ := e.publishOn(cl, e.request(0, 0, 9, 0)); !ok {
		t.Fatal("publish after garbage refused")
	}
}

func TestLegacyDropped(t *testing.T) {
	e := srvStart(t, nil)
	cl := e.dialClient(0)
	cl.send([]byte(`["REQ","r",{"kinds":[23195,23197],"#p":["` + e.client(0) + `"]}]`))
	cl.recv(2 * time.Second)
	hub := newHubClient(t, e.hubURL(), e.wallet(0))
	hub.connect(nil)
	hub.eose()
	legacy := polSigned(t, polW(0), nostr.KindNWCNotificationNIP04, e.nowUnix(), polTags(nostr.KindNWCNotificationNIP04, e.client(0)), polContent(nostr.KindNWCNotificationNIP04))
	if ok, msg := hub.publish(legacy); !ok || msg != "" {
		t.Fatalf("legacy: %v %q", ok, msg)
	}
	cl.none(300 * time.Millisecond)
	e.s.mu.Lock()
	held := e.s.win.Query([]Filter{{}})
	e.s.mu.Unlock()
	if len(held) != 0 {
		t.Fatalf("the window holds %d events after a dropped legacy notification", len(held))
	}
	// A refusal is never stored either.
	bad := e.response(0, 0, 1)
	polBadSig(bad)
	if ok, _ := hub.publish(bad); ok {
		t.Fatal("bad signature accepted")
	}
	if held := e.s.win.Query([]Filter{{}}); len(held) != 0 {
		t.Fatal("a refused event was stored")
	}
}

func TestLimits(t *testing.T) {
	e := srvStart(t, nil)
	var conns []*srvConn
	for i := 0; i < MaxConnsPerListener; i++ {
		conns = append(conns, srvDial(t, e.hubURL()))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, resp, err := websocket.Dial(ctx, e.hubURL(), nil); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("33rd connection: %v %v", resp, err)
	}
	// The other listener has its own count.
	e.dialClient(0)
	// A closed connection frees its slot.
	conns[0].c.CloseNow()
	conns[0].closed(2 * time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, _, err := websocket.Dial(ctx, e.hubURL(), nil)
		if err == nil {
			c.CloseNow()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot not freed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	h := conns[1]
	for i := 1; i <= MaxSubsPerConn; i++ {
		h.send([]byte(fmt.Sprintf(`["REQ","%d",{"kinds":[23194],"#p":["%s"]}]`, i, e.wallet(0))))
		if f := h.recv(2 * time.Second); f != fmt.Sprintf(`["EOSE","%d"]`, i) {
			t.Fatalf("sub %d: %s", i, f)
		}
	}
	h.send([]byte(`["REQ","17",{"kinds":[23194],"#p":["` + e.wallet(0) + `"]}]`))
	if f := h.recv(2 * time.Second); f != `["CLOSED","17","blocked: too many subscriptions"]` {
		t.Fatal(f)
	}
	// Replacing an existing id is not a new subscription.
	h.send([]byte(`["REQ","16",{"kinds":[23194],"#p":["` + e.wallet(0) + `"]}]`))
	if f := h.recv(2 * time.Second); f != `["EOSE","16"]` {
		t.Fatal(f)
	}
	f := `{"kinds":[23194],"#p":["` + e.wallet(0) + `"]}`
	h.send([]byte(`["REQ","five",` + strings.Repeat(f+",", 4) + f + `]`))
	if g := h.recv(2 * time.Second); g != `["CLOSED","five","blocked: too many filters"]` {
		t.Fatal(g)
	}
	h.send([]byte(`["REQ","16",` + strings.Repeat(f+",", 3) + f + `]`))
	if g := h.recv(2 * time.Second); g != `["EOSE","16"]` {
		t.Fatalf("4 filters must pass: %s", g)
	}
}

func TestReplaceSubscription(t *testing.T) {
	e := srvStart(t, nil)
	c0, c1 := e.dialClient(0), e.dialClient(1)
	h := srvDial(t, e.hubURL())
	e.subRequests(h, "s", 0)
	h.recv(2 * time.Second)
	e.subRequests(h, "s", 1) // replaces: wallet 0 requests no longer arrive
	h.recv(2 * time.Second)
	e.publishOn(c0, e.request(0, 0, 1, 0))
	h.none(200 * time.Millisecond)
	r := e.request(1, 1, 2, 0)
	e.publishOn(c1, r)
	srvWantFrame(t, h.recv(2*time.Second), "s", r)
	h.none(200 * time.Millisecond)
	// CLOSE ends it.
	h.send([]byte(`["CLOSE","s"]`))
	time.Sleep(100 * time.Millisecond)
	e.publishOn(c1, e.request(1, 1, 3, 0))
	h.none(200 * time.Millisecond)
}

func TestShutdown(t *testing.T) {
	e := srvStart(t, nil)
	xs := []*srvConn{srvDial(t, e.hubURL()), srvDial(t, e.hubURL()), e.dialClient(0)}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go e.s.Shutdown(ctx)
	for _, x := range xs {
		if st := x.closed(time.Second); st != websocket.StatusGoingAway {
			t.Fatalf("close status = %d, want 1001", st)
		}
	}
	if time.Since(start) > time.Second {
		t.Fatal("shutdown took over a second")
	}
	// A new connection is refused while closing.
	cx, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if c, _, err := websocket.Dial(cx, e.hubURL(), nil); err == nil {
		c.CloseNow()
		t.Fatal("connection accepted after shutdown")
	}
}

func TestRestartEmptyWindow(t *testing.T) {
	e := srvStart(t, nil)
	hub := newHubClient(t, e.hubURL(), e.wallet(0))
	hub.connect(nil)
	hub.eose()
	cl := e.dialClient(0)
	e.subResponses(cl, "r", 0)
	cl.recv(2 * time.Second)
	r1 := e.request(0, 0, 1, 0)
	e.publishOn(cl, r1)
	if g := hub.next(2 * time.Second); g == nil || g.ID != r1.ID {
		t.Fatal("r1 not delivered")
	}
	dropAt := e.nowUnix()
	e.restart()
	hub.c.closed(3 * time.Second)
	cl.closed(3 * time.Second)

	// The hub resubscribes with since; nothing from before the restart comes back.
	hub.pending = nil
	hub.connect(&dropAt)
	if evs := hub.eose(); len(evs) != 0 {
		t.Fatalf("%d events survived the restart", len(evs))
	}
	// SatBooth reconnects, gets a fresh challenge and authenticates again.
	cl = e.dialClient(0)
	e.subResponses(cl, "r", 0)
	if f := cl.recv(2 * time.Second); f != `["EOSE","r"]` {
		t.Fatal(f)
	}
	r2 := e.request(0, 0, 2, 0)
	if ok, _ := e.publishOn(cl, r2); !ok {
		t.Fatal("request refused after the restart")
	}
	if g := hub.next(2 * time.Second); g == nil || g.ID != r2.ID {
		t.Fatal("r2 not delivered")
	}
	if g := hub.next(200 * time.Millisecond); g != nil {
		t.Fatal("an old request was delivered")
	}
	resp := e.response(0, 0, 3)
	if ok, _ := hub.publish(resp); !ok {
		t.Fatal("response refused")
	}
	srvWantFrame(t, cl.recv(2*time.Second), "r", resp)
}

// A backlog larger than the send queue is one queue element: the peer, reading late, gets all of it, then
// EOSE, then live events, and is not closed with 1013.
func TestBigBacklog(t *testing.T) {
	e := srvStart(t, nil)
	cl := e.dialClient(0)
	const n = 600
	for i := 0; i < n; i++ {
		ev := polSigned(t, polC(0), nostr.KindNWCRequest, e.nowUnix(), polTags(nostr.KindNWCRequest, e.wallet(0)), srvPayload(i+100, 4000))
		if ok, msg := e.publishOn(cl, ev); !ok {
			t.Fatalf("publish %d: %q", i, msg)
		}
	}
	hub := srvDial(t, e.hubURL())
	release := e.stallHub()
	e.subRequests(hub, "1:", 0)
	time.Sleep(300 * time.Millisecond) // a slow link: the peer reads late
	release()
	got := 0
	for {
		f, ok := hub.tryRecv(5 * time.Second)
		if !ok {
			t.Fatalf("got %d events and no EOSE", got)
		}
		if f == `["EOSE","1:"]` {
			break
		}
		if frameLabel(f) != "EVENT" {
			t.Fatalf("unexpected frame %.100s", f)
		}
		got++
	}
	if got != n {
		t.Fatalf("backlog delivered %d of %d", got, n)
	}
	live := e.request(0, 0, 5000, 0)
	e.publishOn(cl, live)
	srvWantFrame(t, hub.recv(3*time.Second), "1:", live)
}

func TestAuthDeadline(t *testing.T) {
	e := srvStart(t, func(s *Server) { s.authDeadline = 300 * time.Millisecond })
	idle := srvDial(t, e.cURL())
	srvChallenge(t, idle)
	authed := e.dialClient(0)
	hub := srvDial(t, e.hubURL())
	if f := idle.recv(2 * time.Second); f != `["NOTICE","auth-required: authentication timed out"]` {
		t.Fatal(f)
	}
	if st := idle.closed(2 * time.Second); st != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %d, want 1008", st)
	}
	// An authenticated connection and an idle hub connection stay open past the deadline.
	time.Sleep(200 * time.Millisecond)
	e.subResponses(authed, "r", 0)
	if f := authed.recv(2 * time.Second); f != `["EOSE","r"]` {
		t.Fatal(f)
	}
	e.subRequests(hub, "1:", 0)
	if f := hub.recv(2 * time.Second); f != `["EOSE","1:"]` {
		t.Fatal(f)
	}
}

// A refused REQ with an existing id ends the old subscription: NIP-01 says the REQ replaces it.
func TestRefusedReplacementClosesOld(t *testing.T) {
	e := srvStart(t, nil)
	cl := e.dialClient(0)
	h := srvDial(t, e.hubURL())
	f := `{"kinds":[23194],"#p":["` + e.wallet(0) + `"]}`
	cases := map[string]string{
		"policy":      `["REQ","s",{"kinds":[1]}]`,
		"unsupported": `["REQ","s",{"ids":[]}]`,
		"5 filters":   `["REQ","s",` + strings.Repeat(f+",", 4) + f + `]`,
		"malformed":   `["REQ","s",{"kinds":[1.5]}]`,
	}
	salt := 7000
	for name, bad := range cases {
		salt++
		h.send([]byte(`["REQ","s",` + f + `]`))
		for g := h.recv(2 * time.Second); g != `["EOSE","s"]`; g = h.recv(2 * time.Second) {
			if frameLabel(g) != "EVENT" { // the backlog of earlier rounds comes first
				t.Fatal(g)
			}
		}
		h.send([]byte(bad))
		if g := h.recv(2 * time.Second); !strings.HasPrefix(g, `["CLOSED","s",`) {
			t.Fatalf("%s: %s", name, g)
		}
		e.publishOn(cl, e.request(0, 0, salt, 0))
		h.none(200 * time.Millisecond)
	}
}

// A peer that sends REQs without reading cannot queue more than WindowMaxEvents backlog events.
func TestPendingBacklogCap(t *testing.T) {
	e := srvStart(t, func(s *Server) { s.c.WindowMaxEvents = 50 })
	cl := e.dialClient(0)
	const n = 30
	for i := 0; i < n; i++ {
		ev := polSigned(t, polC(0), nostr.KindNWCRequest, e.nowUnix(), polTags(nostr.KindNWCRequest, e.wallet(0)), srvPayload(i+10, 87000))
		if ok, msg := e.publishOn(cl, ev); !ok {
			t.Fatalf("publish %d: %q", i, msg)
		}
	}
	hub := srvDial(t, e.hubURL())
	release := e.stallHub()    // the relay's writer is stuck, so all 30 stay pending
	e.subRequests(hub, "a", 0) // 30 pending: fits
	for i := 0; i < 10; i++ {
		e.subRequests(hub, fmt.Sprintf("b%d", i), 0) // each would push the count past 50
	}
	time.Sleep(300 * time.Millisecond)
	e.s.mu.Lock()
	var peak int64
	for c := range e.s.conns {
		if c.l == HubListener && c.pending.Load() > peak {
			peak = c.pending.Load()
		}
	}
	e.s.mu.Unlock()
	if peak == 0 || peak > 50 {
		t.Fatalf("pending = %d, want 1..50", peak)
	}
	release()
	events, blocked, eose := 0, 0, 0
	for blocked < 10 {
		f, ok := hub.tryRecv(10 * time.Second)
		if !ok {
			t.Fatalf("events %d, EOSE %d, blocked %d", events, eose, blocked)
		}
		switch {
		case frameLabel(f) == "EVENT":
			events++
		case frameLabel(f) == "EOSE":
			eose++
		case strings.Contains(f, `"blocked: too many pending events"`):
			blocked++
		default:
			t.Fatalf("unexpected frame %.100s", f)
		}
	}
	if events != n || eose != 1 {
		t.Fatalf("a got %d events and %d EOSE, want %d and 1", events, eose, n)
	}
	// The connection stays open, and the count returns to zero.
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.s.mu.Lock()
		var total int64
		for c := range e.s.conns {
			total += c.pending.Load()
		}
		e.s.mu.Unlock()
		if total == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending stuck at %d", total)
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.subRequests(hub, "later", 0)
	if g := hub.recv(5 * time.Second); frameLabel(g) != "EVENT" {
		t.Fatalf("after the burst: %.100s", g)
	}
}

// TestExpiredWhileQueued: a request accepted and queued while live, whose expiration passes while the hub's
// writer is stuck behind a full socket, is never written.
func TestExpiredWhileQueued(t *testing.T) {
	e := srvStart(t, func(s *Server) { s.queueFrames = 64 })
	cl := e.dialClient(0)
	hub := srvDial(t, e.hubURL())
	e.subRequests(hub, "1:", 0)
	if f := hub.recv(2 * time.Second); f != `["EOSE","1:"]` {
		t.Fatal(f)
	}
	release := e.stallHub() // the relay's writer is stuck, with the requests queued behind it
	for i := 0; i < 40; i++ {
		ev := polSigned(t, polC(0), 23194, e.nowUnix(), polTags(23194, e.wallet(0)), srvPayload(i+10, 80000))
		if ok, msg := e.publishOn(cl, ev); !ok {
			t.Fatalf("publish %d: %q", i, msg)
		}
	}
	exp := e.request(0, 0, 999, e.nowUnix()+1)
	if ok, msg := e.publishOn(cl, exp); !ok {
		t.Fatalf("exp: %q", msg)
	}
	e.clock.add(5 * time.Second) // the expiration passes while the frame is queued
	release()
	// The 40 live requests were queued behind the stuck writer and all arrive; the expired one does not.
	events := 0
	for {
		f, ok := hub.tryRecv(2 * time.Second)
		if !ok {
			break
		}
		if strings.Contains(f, `"id":"`+exp.ID+`"`) {
			t.Fatal("expired request delivered after its expiration passed")
		}
		if frameLabel(f) == "EVENT" {
			events++
		}
	}
	if events != 40 {
		t.Fatalf("got %d of the 40 queued live requests: the connection was lost, so the test proves nothing", events)
	}
}

// srvWaitHub waits until cond holds for the hub connection, under the server lock, or fails the test.
func srvWaitHub(t *testing.T, e *srvEnv, what string, cond func(c *conn) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.s.mu.Lock()
		ok := false
		for c := range e.s.conns {
			if c.l == HubListener && cond(c) {
				ok = true
			}
		}
		e.s.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the relay never processed %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// srvPendingZero waits for every connection's pending counter to return to 0.
func srvPendingZero(t *testing.T, e *srvEnv) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.s.mu.Lock()
		var n int64
		for c := range e.s.conns {
			n += c.pending.Load()
		}
		e.s.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending stuck at %d", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// srvStalledHub returns a hub connection whose writer is stuck with a window of requests behind it, so that a
// REQ's backlog sits in the queue, and the func that unsticks the writer.
func srvStalledHub(t *testing.T, e *srvEnv) (*srvConn, func()) {
	cl := e.dialClient(0)
	hub := srvDial(t, e.hubURL())
	e.subRequests(hub, "1:", 0)
	if f := hub.recv(2 * time.Second); f != `["EOSE","1:"]` {
		t.Fatal(f)
	}
	release := e.stallHub()
	for i := 0; i < 40; i++ {
		ev := polSigned(t, polC(0), 23194, e.nowUnix(), polTags(23194, e.wallet(0)), srvPayload(i+10, 80000))
		if ok, msg := e.publishOn(cl, ev); !ok {
			t.Fatalf("publish %d: %q", i, msg)
		}
	}
	return hub, release
}

// srvDrainSub reads until quiet and returns the frames that belong to subscription sub.
func srvDrainSub(hub *srvConn, sub string) []string {
	var got []string
	for {
		f, ok := hub.tryRecv(1500 * time.Millisecond)
		if !ok {
			return got
		}
		if strings.HasPrefix(f, `["EVENT","`+sub+`"`) || strings.HasPrefix(f, `["EOSE","`+sub+`"`) {
			got = append(got, f[:min(len(f), 16)])
		}
	}
}

func TestCloseCancelsQueuedBacklog(t *testing.T) {
	e := srvStart(t, func(s *Server) { s.queueFrames = 64 })
	hub, release := srvStalledHub(t, e)
	e.subRequests(hub, "b", 0) // its backlog queues behind the stuck writer
	hub.send([]byte(`["CLOSE","b"]`))
	srvWaitHub(t, e, "the CLOSE", func(c *conn) bool { // "1:" and "b" were registered, and "b" is gone
		_, open := c.subs["b"]
		return c.nextGen == 2 && !open
	})
	release()
	if got := srvDrainSub(hub, "b"); len(got) != 0 {
		t.Fatalf("frames for a closed subscription: %v", got)
	}
	srvPendingZero(t, e)
}

func TestReplacingReqCancelsOldBacklog(t *testing.T) {
	e := srvStart(t, func(s *Server) { s.queueFrames = 64 })
	hub, release := srvStalledHub(t, e)
	e.subRequests(hub, "b", 0)
	e.subRequests(hub, "b", 1)                                                          // replaces: matches nothing, so only its EOSE may arrive
	srvWaitHub(t, e, "the replacing REQ", func(c *conn) bool { return c.nextGen == 3 }) // "1:", "b", "b" again
	release()
	got := srvDrainSub(hub, "b")
	if len(got) != 1 || !strings.HasPrefix(got[0], `["EOSE","b"`) {
		t.Fatalf("frames for sub b = %v, want only the replacement's EOSE", got)
	}
	srvPendingZero(t, e)
}
