package relay

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/satbooth/satbooth/nostr"
)

// Server is the WebSocket side of the relay: it joins the parser, the window and the admission rules.
// One Server serves both listeners (each Handler call returns the handler of one), and they share one window.
type Server struct {
	c   *Config
	log *slog.Logger
	now func() time.Time
	pol *Policy
	win *Window

	// queueFrames is the per-connection send queue; tests shrink it.
	queueFrames  int
	authDeadline time.Duration

	// mu guards the registry (conns, each conn's subs, counts, closing) and makes "store then
	// broadcast" atomic against "query then subscribe", so a subscriber sees every event once.
	mu      sync.Mutex
	conns   map[*conn]struct{}
	counts  map[Listener]int
	closing bool
	wg      sync.WaitGroup
}

// NewServer returns a server with an empty window. now is the injected clock; nil means time.Now.
func NewServer(c *Config, logger *slog.Logger, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Server{
		c: c, log: logger, now: now, pol: NewPolicy(c),
		win:         NewWindow(c.lifetime(), c.WindowMaxEvents, c.WindowMaxBytes, now),
		queueFrames: QueueFrames, authDeadline: AuthDeadline,
		conns: map[*conn]struct{}{}, counts: map[Listener]int{},
	}
}

func listenerName(l Listener) string {
	if l == HubListener {
		return "hub"
	}
	return "client"
}

// Handler returns the HTTP handler of one listener.
func (s *Server) Handler(l Listener) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The connection limit is checked before Accept, so a refused client costs no upgrade.
		s.mu.Lock()
		if s.closing || s.counts[l] >= MaxConnsPerListener {
			s.mu.Unlock()
			s.log.Info("refused connection", "listener", listenerName(l), "remote", r.RemoteAddr, "reason", "blocked: too many connections")
			http.Error(w, "too many connections", http.StatusServiceUnavailable)
			return
		}
		s.counts[l]++
		s.wg.Add(1)
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.counts[l]--
			s.mu.Unlock()
			s.wg.Done()
		}()

		ws, err := websocket.Accept(w, r, nil) // writes its own HTTP error on failure
		if err != nil {
			return
		}
		ws.SetReadLimit(MaxMessage)
		s.serve(ws, l, r.RemoteAddr)
	})
}

// Shutdown closes every connection with status 1001 and waits for the handlers, until ctx ends.
func (s *Server) Shutdown(ctx context.Context) {
	s.mu.Lock()
	s.closing = true
	cs := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	var cw sync.WaitGroup
	for _, c := range cs {
		cw.Add(1)
		go func() {
			defer cw.Done()
			done := make(chan struct{})
			go func() {
				_ = c.ws.Close(websocket.StatusGoingAway, "shutting down")
				close(done)
			}()
			select {
			case <-done:
			case <-ctx.Done():
				_ = c.ws.CloseNow()
			}
		}()
	}
	cw.Wait()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// broadcast sends e to every subscription that matches it. The caller holds s.mu. It checks Live for every
// recipient frame: an event can expire between receipt and now, and AddAdded is not permission to send.
func (s *Server) broadcast(e *nostr.Event) {
	for c := range s.conns {
		for id, fs := range c.subs {
			if !matchesAny(fs, e) {
				continue
			}
			if !s.win.Live(e) {
				return // expired: nobody gets it
			}
			c.push(queued{sub: id, gen: c.gens[id], evs: []*nostr.Event{e}})
		}
	}
}

func matchesAny(fs []Filter, e *nostr.Event) bool {
	for i := range fs {
		if fs[i].Matches(e) {
			return true
		}
	}
	return false
}
