// Package hubstub answers, from inside the hub host, the requests Alby Hub makes to Alby's, mempool.space's, Boltz's
// and Megalith's servers, so that the hub never needs the internet. It serves TLS for the names of the certificate that
// setup issues (setup.StubNames), the hub resolves each of them to the stub, and it trusts only the CA that signed it.
//
// What it answers is what the hub needs to stay healthy and nothing more: a document that says "healthy, no incident,
// no update" (the hub's health check fails without one), an empty list of channel suggestions, no currencies and no
// stories. Everything the hub asks for that needs a real service (mempool data, swaps, fiat rates, channel purchases)
// is refused with a JSON message and a status the hub's UI shows as an error, never as data. It logs the method, the
// host and the path of every request, and never a body, a query or a header.
package hubstub

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// Hosts, by what the stub does for them. The list is the hosts getAlby/hub v1.24.7 contacts by name at its defaults,
// plus the stub's own name (setup.StubNames); a request for any other host is refused.
const (
	hostStub         = "hubstub"
	hostAlby         = "getalby.com"
	hostAlbyAPI      = "api.getalby.com"
	hostMempool      = "mempool.space"
	hostBoltz        = "api.boltz.exchange"
	hostBoltzRouting = "api.satsrouting.exchange"
)

// info is the answer to GET /api/internal/info, in the wire form GetInfo decodes (alby/alby_service.go:199-219). api.Health
// (api/api.go:2125-2133) returns an error when the request fails and raises the Alby-service alarm unless healthy is
// true, so healthy is true with no incidents. latest_version is the pinned hub's, without the "v": the update banner
// compares it with the running version (frontend/src/hooks/useBanner.tsx), and it is hidden anyway (HIDE_UPDATE_BANNER).
// account_available false says no Alby account can be linked.
const info = `{"hub":{"latest_version":"1.24.7","latest_release_notes":"","swap_provider":""},` +
	`"status":"operational","healthy":true,"account_available":false,"incidents":[]}`

// A list GetChannelPeerSuggestions decodes into []ChannelPeerSuggestion (alby/alby_service.go:135-173): empty, so the
// hub picks no liquidity source from it (lnclient/ldk/ldk.go:174).
const suggestions = `[]`

// GetCurrencies decodes a JSON object of currencies (alby/alby_service.go:25-67): none, so no fiat unit is offered.
const currencies = `{}`

// GetStories decodes a JSON array of stories (alby/alby_oauth_service.go:1422-1462): none.
const stories = `[]`

// albyPrivateAPI is the prefix of Alby's own API paths other than the three documents the stub answers.
const albyPrivateAPI = "/api/internal"

// Handler is the stub's whole behaviour.
func Handler(log *slog.Logger) http.Handler { return &handler{log: log} }

type handler struct{ log *slog.Logger }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, path := cleanHost(r.Host), r.URL.Path
	h.log.Info("request", "method", clip(r.Method), "host", clip(host), "path", clip(path))

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch {
	case !knownHost(host):
		refuse(w, http.StatusNotFound, "not found")
	case host == hostMempool || host == hostBoltz || host == hostBoltzRouting:
		refuse(w, http.StatusServiceUnavailable, "this service is not available from this host")
	case isAlbyDoc(host, path):
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			refuse(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodHead {
			return
		}
		switch path {
		case "/api/internal/info":
			w.Write([]byte(info))
		case "/api/internal/channel_suggestions":
			w.Write([]byte(suggestions))
		case "/api/rates":
			w.Write([]byte(currencies))
		default:
			w.Write([]byte(stories))
		}
	case strings.HasPrefix(path, albyPrivateAPI+"/") || strings.HasPrefix(path, "/api/rates") ||
		strings.HasPrefix(path, "/api/stories"):
		refuse(w, http.StatusNotFound, "not found") // Alby's other paths: rates of one currency, orders, accounts
	case path == "/v2" || strings.HasPrefix(path, "/v2/"):
		refuse(w, http.StatusServiceUnavailable, "swaps are not available") // BOLTZ_API=https://hubstub
	case strings.HasPrefix(path, "/api/"):
		refuse(w, http.StatusServiceUnavailable, "mempool data is not available") // MEMPOOL_API=https://hubstub/api
	default:
		refuse(w, http.StatusNotFound, "not found")
	}
}

func isAlbyDoc(host, path string) bool {
	if host != hostStub && host != hostAlby && host != hostAlbyAPI {
		return false
	}
	switch path {
	case "/api/internal/info", "/api/internal/channel_suggestions", "/api/rates", "/api/stories":
		return true
	}
	return false
}

func knownHost(host string) bool {
	switch host {
	case hostStub, hostAlby, hostAlbyAPI, hostMempool, hostBoltz, hostBoltzRouting,
		"megalithic.me", "electrum.getalbypro.com", "vss.getalbypro.com":
		return true
	}
	return false
}

// refuse answers with a JSON message. The messages are the constants above, with nothing to quote.
func refuse(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write([]byte(`{"message":"` + msg + `"}`))
}

// cleanHost is the lower-case host of a Host header, without a port or a trailing dot.
func cleanHost(h string) string {
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// clip bounds a logged value; slog quotes control characters and invalid bytes, so one request is one line.
func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// Server is the stub's TLS listener and HTTP server.
type Server struct {
	srv  *http.Server
	addr string
	cfg  *tls.Config
}

// NewServer loads the certificate and key (PEM files, the stub's two mounted files) and refuses a pair that does not
// match or cannot be read. The certificate is read once; after a renewal the stub is restarted.
func NewServer(addr, certFile, keyFile string, h http.Handler) (*Server, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, errors.New("hubstub: cannot load the certificate and key: " + err.Error())
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
	return &Server{addr: addr, cfg: cfg, srv: &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}}, nil
}

// Listen opens the TLS listener.
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return nil, err
	}
	return tls.NewListener(ln, s.cfg), nil
}

// Serve runs until Close or Shutdown.
func (s *Server) Serve(ln net.Listener) error { return s.srv.Serve(ln) }

// Close stops the server at once.
func (s *Server) Close() error { return s.srv.Close() }

// Shutdown stops it after the requests in flight.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }
