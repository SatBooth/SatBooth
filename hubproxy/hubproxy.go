// Package hubproxy is the reverse proxy in front of Alby Hub's web UI. It forwards every request to the hub, and
// replaces the Content-Security-Policy of every response with one that lets the operator's browser load and contact
// nothing but the page's own origin: the hub's own policy names Alby's hosts and relays, so a page of the hub could
// otherwise show images from, and talk to, servers outside the bundle. No other header, and no body, is changed.
package hubproxy

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// Policy is the policy of every response: self only. Images may also be data: URIs (the UI draws QR codes and icons
// inline); frames, plugins, <base> and cross-origin form posts are refused.
const Policy = "default-src 'self'; img-src 'self' data:; connect-src 'self'; frame-src 'none'; object-src 'none'; " +
	"base-uri 'none'; form-action 'self'"

// ParseUpstream accepts only an http URL on a loopback address with a port and no path: the proxy is for the hub in
// the same network namespace, and must never forward to anything else.
func ParseUpstream(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" ||
		u.Fragment != "" || u.Port() == "" {
		return nil, errors.New("hubproxy: the upstream must be http://<loopback address>:<port>")
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("hubproxy: the upstream must be a loopback address")
	}
	return &url.URL{Scheme: "http", Host: u.Host}, nil
}

// Handler proxies to the upstream with the default transport, which ignores the proxy environment variables.
func Handler(upstream *url.URL, log *slog.Logger) http.Handler {
	rt := http.DefaultTransport.(*http.Transport).Clone()
	rt.Proxy = nil
	// The hub answers within this or the proxy gives a 502; a streaming response, once begun, is not bound by it.
	rt.ResponseHeaderTimeout = 60 * time.Second
	return NewProxy(upstream, rt, log)
}

// NewProxy is Handler with the transport given. Streaming responses are flushed as they arrive, and an upgrade (a
// websocket) is passed through as a tunnel.
func NewProxy(upstream *url.URL, rt http.RoundTripper, log *slog.Logger) http.Handler {
	p := httputil.NewSingleHostReverseProxy(upstream)
	p.Transport = rt
	p.FlushInterval = -1
	p.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del("Content-Security-Policy")
		resp.Header.Del("Content-Security-Policy-Report-Only")
		resp.Header.Set("Content-Security-Policy", Policy)
		return nil
	}
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Warn("upstream error", "method", clip(r.Method), "path", clip(r.URL.Path), "err", err.Error())
		w.Header().Set("Content-Security-Policy", Policy)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("The hub is not answering.\n"))
	}
	return p
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
