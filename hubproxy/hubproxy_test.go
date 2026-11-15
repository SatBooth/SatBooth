package hubproxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const hubPolicy = "default-src 'self'; img-src 'self' https://uploads.getalby-assets.com https://getalby.com; " +
	"connect-src 'self' https://api.getalby.com wss://relay.getalby.com; frame-src https://www.youtube-nocookie.com"

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func proxyTo(t *testing.T, upstream http.Handler) (*httptest.Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	px := httptest.NewServer(Handler(u, quiet()))
	t.Cleanup(px.Close)
	return up, px
}

// Every kind of response the UI is served: the policy is ours and only ours; every other header, the status and the
// body are the hub's.
func TestPolicyReplacesTheHubsOnEveryResponse(t *testing.T) {
	type resp struct {
		status int
		ctype  string
		body   string
		csp    []string // the hub's, if any
		extra  map[string]string
	}
	cases := map[string]resp{
		"html":         {200, "text/html; charset=utf-8", "<html></html>", []string{hubPolicy}, map[string]string{"X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff"}},
		"js":           {200, "text/javascript", "console.log(1)", []string{hubPolicy}, map[string]string{"Cache-Control": "max-age=31536000"}},
		"css":          {200, "text/css", "a{}", []string{hubPolicy}, nil},
		"json":         {200, "application/json", `{"a":1}`, []string{hubPolicy}, map[string]string{"Set-Cookie": "s=1; HttpOnly"}},
		"svg":          {200, "image/svg+xml", "<svg/>", nil, nil},
		"png":          {200, "image/png", "\x89PNG", []string{hubPolicy}, nil},
		"not found":    {404, "application/json", `{"message":"Not Found"}`, []string{hubPolicy}, nil},
		"server err":   {500, "text/plain", "boom", nil, nil},
		"no content":   {204, "", "", []string{hubPolicy}, nil},
		"two policies": {200, "text/html", "x", []string{hubPolicy, "default-src *"}, nil},
		"empty policy": {200, "text/html", "x", []string{""}, nil},
		"relaxed":      {200, "text/html", "x", []string{"default-src *; script-src 'unsafe-inline' 'unsafe-eval'"}, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, px := proxyTo(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, v := range c.csp {
					w.Header().Add("Content-Security-Policy", v)
				}
				w.Header().Set("Content-Security-Policy-Report-Only", "default-src 'none'; report-uri https://example.com/r")
				if c.ctype != "" {
					w.Header().Set("Content-Type", c.ctype)
				}
				for k, v := range c.extra {
					w.Header().Set(k, v)
				}
				w.Header().Set("Etag", `"v1"`)
				w.WriteHeader(c.status)
				io.WriteString(w, c.body)
			}))
			r, err := http.Get(px.URL + "/some/path?x=1")
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			body, _ := io.ReadAll(r.Body)
			if r.StatusCode != c.status || string(body) != c.body {
				t.Errorf("status %d body %q, want %d %q", r.StatusCode, body, c.status, c.body)
			}
			if got := r.Header.Values("Content-Security-Policy"); !reflect.DeepEqual(got, []string{Policy}) {
				t.Errorf("policy %q", got)
			}
			if r.Header.Get("Content-Security-Policy-Report-Only") != "" {
				t.Error("the hub's report-only policy survived")
			}
			want := map[string]string{"Etag": `"v1"`}
			for k, v := range c.extra {
				want[k] = v
			}
			if c.ctype != "" {
				want["Content-Type"] = c.ctype
			}
			for k, v := range want {
				if r.Header.Get(k) != v {
					t.Errorf("header %s = %q, want %q", k, r.Header.Get(k), v)
				}
			}
			for k := range r.Header {
				switch k {
				case "Content-Security-Policy", "Date", "Content-Length", "Etag", "Content-Type":
				default:
					if _, ok := c.extra[k]; !ok {
						t.Errorf("an extra header %s", k)
					}
				}
			}
		})
	}
}

// The request reaches the hub as it was sent: method, path, query, body, headers.
func TestRequestPassesUnchanged(t *testing.T) {
	var got *http.Request
	var body []byte
	_, px := proxyTo(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
	}))
	req, _ := http.NewRequest("POST", px.URL+"/api/unlock?a=b%20c&d=%2F", strings.NewReader(`{"unlockPassword":"x"}`))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "s=1")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	if _, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	if got.Method != "POST" || got.URL.RequestURI() != "/api/unlock?a=b%20c&d=%2F" || string(body) != `{"unlockPassword":"x"}` {
		t.Errorf("%s %s %q", got.Method, got.URL.RequestURI(), body)
	}
	for k, v := range map[string]string{"Authorization": "Bearer tok", "Content-Type": "application/json", "Cookie": "s=1",
		"Origin": "http://127.0.0.1:8080"} {
		if got.Header.Get(k) != v {
			t.Errorf("%s = %q", k, got.Header.Get(k))
		}
	}
}

// A streaming response arrives as it is written, not when it ends.
func TestStreamingIsFlushed(t *testing.T) {
	release := make(chan struct{})
	_, px := proxyTo(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: two\n\n")
	}))
	r, err := http.Get(px.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	buf := make([]byte, 11)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(r.Body, buf); done <- err }()
	select {
	case err := <-done:
		if err != nil || string(buf) != "data: one\n\n" {
			t.Fatalf("%v %q", err, buf)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event did not arrive before the stream ended")
	}
	close(release)
	rest, _ := io.ReadAll(r.Body)
	if string(rest) != "data: two\n\n" {
		t.Errorf("rest %q", rest)
	}
}

// A websocket upgrade passes through, and messages go both ways.
func TestWebsocketUpgradePasses(t *testing.T) {
	_, px := proxyTo(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if c.Write(r.Context(), typ, append([]byte("echo:"), b...)) != nil {
				return
			}
		}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(px.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if resp.StatusCode != 101 {
		t.Errorf("status %d", resp.StatusCode)
	}
	for _, m := range []string{"a", "hello", strings.Repeat("z", 20000)} {
		if err := c.Write(ctx, websocket.MessageText, []byte(m)); err != nil {
			t.Fatal(err)
		}
		_, b, err := c.Read(ctx)
		if err != nil || string(b) != "echo:"+m {
			t.Fatalf("%v %.20q", err, b)
		}
	}
}

// A hub that is down gives a 502 with the policy and no detail of the address.
func TestHubDown(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(up.URL)
	up.Close()
	var logs bytes.Buffer
	px := httptest.NewServer(Handler(u, slog.New(slog.NewTextHandler(&logs, nil))))
	defer px.Close()
	r, err := http.Get(px.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 502 || r.Header.Get("Content-Security-Policy") != Policy || strings.Contains(string(b), u.Host) {
		t.Errorf("%d %q %q", r.StatusCode, r.Header.Get("Content-Security-Policy"), b)
	}
}

func TestParseUpstream(t *testing.T) {
	for _, ok := range []string{"http://127.0.0.1:8081", "http://127.0.0.1:8081/", "http://[::1]:9"} {
		if _, err := ParseUpstream(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "https://127.0.0.1:8081", "http://example.com:80", "http://10.0.0.1:80", "http://127.0.0.1",
		"http://127.0.0.1:80/x", "http://u@127.0.0.1:80", "http://127.0.0.1:80?x=1", "http://localhost:80", "127.0.0.1:80",
		"http://127.0.0.1:80#f"} {
		if _, err := ParseUpstream(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

type recorder struct {
	url  *url.URL
	host string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.url, r.host = req.URL, req.URL.Host
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Security-Policy": {"default-src *"}},
		Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

func pathCase(t *testing.T, path, query string) {
	t.Helper()
	up, _ := url.Parse("http://127.0.0.1:8081")
	rec := &recorder{}
	h := NewProxy(up, rec, quiet())
	req := &http.Request{Method: "GET", URL: &url.URL{Path: path, RawQuery: query}, Header: http.Header{}, Body: http.NoBody,
		Host: "127.0.0.1:8080", RemoteAddr: "192.0.2.1:1"}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || w.Header().Get("Content-Security-Policy") != Policy {
		t.Fatalf("%q: %d %q", path, w.Code, w.Header().Get("Content-Security-Policy"))
	}
	if rec.host != "127.0.0.1:8081" || rec.url.Path != path || rec.url.RawQuery != query {
		t.Fatalf("%q?%q went to %q path %q query %q", path, query, rec.host, rec.url.Path, rec.url.RawQuery)
	}
}

// Seeded and native fuzz of the path and query: the request always goes to the upstream's address, with the path as
// given, and the answer always carries the policy.
func TestPathsSeeded(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	fixed := []string{"/", "//evil.example/x", "/..//a", "/%2e%2e/", "/a b", "/api/../x", "///", "/\x00", "/@evil.example"}
	for i := 0; i < 3000; i++ {
		p := fixed[r.Intn(len(fixed))]
		if r.Intn(2) == 0 {
			b := make([]byte, r.Intn(300))
			for j := range b {
				b[j] = byte(r.Intn(256))
			}
			p = "/" + string(b)
		}
		pathCase(t, p, string(append([]byte(nil), fixed[r.Intn(len(fixed))]...)))
	}
}

func FuzzPath(f *testing.F) {
	f.Add("/api/info", "a=b")
	f.Add("//evil.example/x", "")
	f.Add("/@evil", "u=http://evil")
	f.Fuzz(func(t *testing.T, path, query string) {
		if !strings.HasPrefix(path, "/") {
			return // a request target is an absolute path
		}
		pathCase(t, path, query)
	})
}
