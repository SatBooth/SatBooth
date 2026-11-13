package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/satbooth/satbooth/nostr"
)

// Fixed replies. Nothing a client sent is ever echoed back.
const (
	msgMalformed      = "invalid: malformed message"
	msgDuplicate      = "duplicate: already have this event"
	msgAuthNotUsed    = "restricted: AUTH is not used on this listener"
	msgTooManySubs    = "blocked: too many subscriptions"
	msgTooManyAuthed  = "blocked: too many authenticated keys"
	msgAuthTimeout    = "auth-required: authentication timed out"
	msgTooManyPending = "blocked: too many pending events"
	msgBinary         = "invalid: text frames only"
)

// queued is one element of a connection's send queue: a single frame, or a REQ's whole backlog plus its
// EOSE. A backlog is one element so that a large one cannot overflow the queue and close a healthy
// subscriber; the writer still writes each frame under its own WriteTimeout, so a stalled peer is closed.
//
// Invariants: evs elements are re-checked with Live and against the live subscription (sub, gen) just before
// each write; counted is true exactly for a backlog added to conn.pending by onReq, and only those decrement it.
type queued struct {
	frame   []byte
	sub     string
	gen     uint64 // the subscription generation these events belong to; a CLOSE or replacing REQ voids it
	evs     []*nostr.Event
	eose    bool
	counted bool
}

// conn is one client connection. The reader loop (serve) owns authed; subs is guarded by Server.mu.
type conn struct {
	s         *Server
	ws        *websocket.Conn
	l         Listener
	remote    string
	challenge string
	authed    []string

	subs    map[string][]Filter
	gens    map[string]uint64 // generation of each live subscription id; guarded by Server.mu
	nextGen uint64

	ctx    context.Context
	cancel context.CancelFunc
	out    chan queued
	authOK atomic.Bool
	// pending counts events in queued backlogs the writer has not finished. Only the reader goroutine adds.
	pending atomic.Int64
	slow    sync.Once
	dead    atomic.Bool
}

func (s *Server) serve(ws *websocket.Conn, l Listener, remote string) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &conn{
		s: s, ws: ws, l: l, remote: remote, subs: map[string][]Filter{}, gens: map[string]uint64{},
		ctx: ctx, cancel: cancel, out: make(chan queued, s.queueFrames),
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		_ = ws.Close(websocket.StatusGoingAway, "shutting down")
		cancel()
		return
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	s.log.Info("connection open", "listener", listenerName(l), "remote", remote)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.writer() }()
	go func() { defer wg.Done(); c.pinger() }()

	if l == ClientListener {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			_ = ws.CloseNow()
			cancel()
			s.mu.Lock()
			delete(s.conns, c)
			c.subs = nil
			s.mu.Unlock()
			wg.Wait()
			return
		}
		c.challenge = hex.EncodeToString(b[:])
		c.enqueue(FrameAuth(c.challenge))
		t := time.AfterFunc(s.authDeadline, c.authTimeout)
		defer t.Stop()
	}
	c.reader()

	cancel()
	s.mu.Lock()
	delete(s.conns, c)
	c.subs = nil
	s.mu.Unlock()
	_ = ws.CloseNow()
	wg.Wait()
	s.log.Info("connection closed", "listener", listenerName(l), "remote", remote)
}

// enqueue queues a frame without blocking. On a full queue the connection is closed with 1013 and
// the frame is dropped: a stalled reader never holds up the others.
func (c *conn) enqueue(f []byte) { c.push(queued{frame: f}) }

func (c *conn) push(q queued) {
	if c.ctx.Err() != nil || c.dead.Load() {
		return
	}
	select {
	case c.out <- q:
	default:
		c.slow.Do(func() {
			c.s.log.Info("closing slow connection", "listener", listenerName(c.l), "remote", c.remote)
			c.dead.Store(true) // the writer keeps draining, so the peer can still read the close frame
			go func() { _ = c.ws.Close(websocket.StatusTryAgainLater, "too slow") }()
		})
	}
}

func (c *conn) writer() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case q := <-c.out:
			if !c.writeQueued(q) {
				_ = c.ws.CloseNow()
				return
			}
		}
	}
}

func (c *conn) write(f []byte) bool {
	ctx, cancel := context.WithTimeout(c.ctx, WriteTimeout)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, f) == nil
}

// writeQueued writes one queue element in order. Events are re-checked with Live as they go out.
func (c *conn) writeQueued(q queued) bool {
	if q.counted {
		defer c.pending.Add(-int64(len(q.evs))) // finished or abandoned
	}
	if q.frame != nil && !c.write(q.frame) {
		return false
	}
	for _, e := range q.evs {
		if c.s.win.Live(e) && c.subCurrent(q.sub, q.gen) && !c.write(FrameEvent(q.sub, e)) {
			return false
		}
	}
	return !q.eose || !c.subCurrent(q.sub, q.gen) || c.write(FrameEOSE(q.sub))
}

// subCurrent reports whether gen is still the live generation of subscription id.
func (c *conn) subCurrent(id string, gen uint64) bool {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	g, ok := c.gens[id]
	return ok && g == gen
}

// dropSub removes a subscription. The caller holds Server.mu.
func (c *conn) dropSub(id string) {
	delete(c.subs, id)
	delete(c.gens, id)
}

// authTimeout closes a private-listener connection that has not authenticated in time.
func (c *conn) authTimeout() {
	if c.authOK.Load() || c.ctx.Err() != nil {
		return
	}
	c.s.log.Info("authentication timed out", "listener", listenerName(c.l), "remote", c.remote)
	c.dead.Store(true)
	c.write(FrameNotice(msgAuthTimeout))
	_ = c.ws.Close(websocket.StatusPolicyViolation, "authentication timed out")
}

func (c *conn) pinger() {
	t := time.NewTicker(PingInterval)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(c.ctx, PingTimeout)
			err := c.ws.Ping(ctx)
			cancel()
			if err != nil && c.ctx.Err() == nil {
				_ = c.ws.CloseNow()
				return
			}
		}
	}
}

func (c *conn) reader() {
	for {
		typ, data, err := c.ws.Read(c.ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			c.enqueue(FrameNotice(msgBinary))
			continue
		}
		c.dispatch(data)
	}
}

func (c *conn) dispatch(data []byte) {
	m, err := ParseClientMessage(data)
	if err != nil {
		c.parseFailed(m, err)
		return
	}
	switch m.Label {
	case "EVENT":
		c.onEvent(m.Event)
	case "REQ":
		c.onReq(m)
	case "CLOSE":
		c.s.mu.Lock()
		if c.subs != nil {
			c.dropSub(m.SubID)
		}
		c.s.mu.Unlock()
	case "AUTH":
		c.onAuth(m.Event)
	}
}

// parseFailed answers a frame the parser refused. Only fixed strings go back.
func (c *conn) parseFailed(m *ClientMessage, err error) {
	if errors.Is(err, ErrBadSubID) {
		c.refused("NOTICE", 0, ErrBadSubID.Error())
		c.enqueue(FrameNotice(ErrBadSubID.Error()))
		return
	}
	if m != nil && m.Label == "REQ" {
		msg := msgMalformed
		var ue *UnsupportedFieldError
		switch {
		case errors.As(err, &ue):
			msg = ue.Error()
		case errors.Is(err, ErrTooManyFilters):
			msg = ErrTooManyFilters.Error()
		}
		c.closeSub(m.SubID, msg)
		return
	}
	c.refused("NOTICE", 0, msgMalformed)
	c.enqueue(FrameNotice(msgMalformed))
}

// refused logs a refusal by prefix and kind, never the content.
func (c *conn) refused(frame string, kind int, msg string) {
	prefix := msg
	for i := 0; i < len(msg); i++ {
		if msg[i] == ':' {
			prefix = msg[:i]
			break
		}
	}
	c.s.log.Info("refused", "listener", listenerName(c.l), "remote", c.remote, "frame", frame, "prefix", prefix, "kind", kind)
}

// closeSub answers a REQ with CLOSED and removes any subscription already using the id: a REQ replaces
// it (NIP-01), so a refused replacement must not leave the old one running.
func (c *conn) closeSub(id, msg string) {
	c.refused("CLOSED", 0, msg)
	c.s.mu.Lock()
	if c.subs != nil {
		c.dropSub(id)
	}
	c.s.mu.Unlock()
	c.enqueue(FrameClosed(id, msg))
}

func (c *conn) policyAuthed() []string {
	if c.l == HubListener {
		return nil
	}
	return c.authed
}

func (c *conn) onEvent(e *nostr.Event) {
	s := c.s
	v := s.pol.Event(c.l, c.policyAuthed(), e, s.now(), s.c.lifetime())
	switch {
	case v.Drop:
		c.s.log.Info("dropped legacy notification", "listener", listenerName(c.l))
		c.enqueue(FrameOK(e.ID, true, ""))
		return
	case !v.Accept:
		c.refused("OK", e.Kind, v.Msg)
		c.enqueue(FrameOK(e.ID, false, v.Msg))
		return
	}
	size := len(eventJSON(e))
	s.mu.Lock()
	res := s.win.Add(e, size)
	if res == AddAdded {
		s.broadcast(e)
	}
	s.mu.Unlock()
	if res == AddDuplicate {
		c.enqueue(FrameOK(e.ID, true, msgDuplicate))
		return
	}
	c.enqueue(FrameOK(e.ID, true, ""))
}

func (c *conn) onReq(m *ClientMessage) {
	s := c.s
	if ok, msg := s.pol.Filters(c.l, c.policyAuthed(), m.Filters); !ok {
		c.closeSub(m.SubID, msg)
		return
	}
	s.mu.Lock()
	if c.subs == nil { // the connection is gone
		s.mu.Unlock()
		return
	}
	if _, replacing := c.subs[m.SubID]; !replacing && len(c.subs) >= MaxSubsPerConn {
		s.mu.Unlock()
		c.closeSub(m.SubID, msgTooManySubs)
		return
	}
	// Backlog, EOSE and registration are one step under the lock: a live event lands either in the
	// backlog or after the subscription exists, never in neither.
	c.dropSub(m.SubID)
	evs := s.win.Query(m.Filters)
	if c.pending.Load()+int64(len(evs)) > int64(s.c.WindowMaxEvents) {
		// A peer that sends REQs without reading would otherwise queue a whole window per REQ.
		s.mu.Unlock()
		c.refused("CLOSED", 0, msgTooManyPending)
		c.enqueue(FrameClosed(m.SubID, msgTooManyPending))
		return
	}
	c.pending.Add(int64(len(evs)))
	c.nextGen++
	c.push(queued{sub: m.SubID, gen: c.nextGen, evs: evs, eose: true, counted: true})
	c.subs[m.SubID] = m.Filters
	c.gens[m.SubID] = c.nextGen
	s.mu.Unlock()
}

func (c *conn) onAuth(e *nostr.Event) {
	s := c.s
	if c.l == HubListener {
		c.refused("OK", e.Kind, msgAuthNotUsed)
		c.enqueue(FrameOK(e.ID, false, msgAuthNotUsed))
		return
	}
	allowed := func(pub string) bool { _, ok := s.c.PairByClient(pub); return ok }
	if err := nostr.CheckAuth(e, s.c.ClientRelayURL, c.challenge, s.now(), allowed); err != nil {
		msg := "invalid: AUTH refused"
		if errors.Is(err, nostr.ErrAuthDenied) {
			msg = "restricted: AUTH refused"
		}
		c.refused("OK", e.Kind, msg)
		c.enqueue(FrameOK(e.ID, false, msg))
		return
	}
	if !inStrings(c.authed, e.PubKey) {
		if len(c.authed) >= MaxAuthed {
			c.refused("OK", e.Kind, msgTooManyAuthed)
			c.enqueue(FrameOK(e.ID, false, msgTooManyAuthed))
			return
		}
		c.authed = append(c.authed, e.PubKey)
	}
	c.authOK.Store(true)
	c.enqueue(FrameOK(e.ID, true, ""))
}
