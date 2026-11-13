package rates

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satbooth/satbooth/rates/ratestest"
)

func TestClientNoRedirect(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer target.Close()
	src := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/rates", http.StatusFound)
	}))
	defer src.Close()

	b, err := get(context.Background(), NewClient(src.Client().Transport), src.URL)
	if !errors.Is(err, ErrTransport) || b != nil {
		t.Fatalf("a 302: %d bytes, err %v, want ErrTransport", len(b), err)
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("the redirect target was requested %d times", n)
	}
	if c := NewClient(nil); c.CheckRedirect == nil || !errors.Is(c.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Fatal("CheckRedirect does not return http.ErrUseLastResponse")
	}
}

func TestClientSizeCap(t *testing.T) {
	size := MaxResponse
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", size)))
	}))
	defer srv.Close()
	c := NewClient(srv.Client().Transport)

	b, err := get(context.Background(), c, srv.URL)
	if err != nil || len(b) != MaxResponse {
		t.Fatalf("MaxResponse bytes: %d bytes, err %v", len(b), err)
	}
	size = MaxResponse + 1
	b, err = get(context.Background(), c, srv.URL)
	if !errors.Is(err, ErrTooLarge) || b != nil {
		t.Fatalf("MaxResponse + 1 bytes: %d bytes, err %v, want ErrTooLarge", len(b), err)
	}
}

func TestClientTimeout(t *testing.T) {
	fake := ratestest.New(t)
	fake.Set("yadio", "USD", "76000")
	c := NewClient(fake.Transport())
	c.Timeout = 200 * time.Millisecond
	fake.Delay("yadio", 2*c.Timeout)
	start := time.Now()
	rs, err := Yadio(c).Fetch(context.Background(), AssetBTC, []string{"USD"}, at2030)
	if !errors.Is(err, ErrTransport) || len(rs) != 0 {
		t.Fatalf("%d rates, err %v, want ErrTransport", len(rs), err)
	}
	if d := time.Since(start); d > c.Timeout+time.Second {
		t.Fatalf("the fetch took %v", d)
	}
}

func TestClientHeaders(t *testing.T) {
	got := make(chan http.Header, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if _, err := get(context.Background(), NewClient(srv.Client().Transport), srv.URL); err != nil {
		t.Fatal(err)
	}
	h := <-got
	if v := h.Values("User-Agent"); len(v) != 1 || v[0] != "SatBooth" {
		t.Errorf("User-Agent %q", v)
	}
	if v := h.Values("Accept"); len(v) != 1 || v[0] != "application/json" {
		t.Errorf("Accept %q", v)
	}
	// Go's own default is Accept-Encoding (gzip, decoded transparently); nothing else is sent.
	for k := range h {
		switch k {
		case "User-Agent", "Accept", "Accept-Encoding":
		default:
			t.Errorf("unexpected header %s", k)
		}
	}
	if UserAgent != "SatBooth" {
		t.Errorf("UserAgent = %q", UserAgent)
	}
}

func TestClientStatus(t *testing.T) {
	for _, code := range []int{429, 500, 404, 201} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte(`{"error":"` + ratestest.Sentinel + `"}`))
		}))
		b, err := get(context.Background(), NewClient(srv.Client().Transport), srv.URL)
		srv.Close()
		var se *StatusError
		if !errors.Is(err, ErrStatus) || !errors.As(err, &se) || se.Code != code || b != nil {
			t.Fatalf("%d: %d bytes, err %v", code, len(b), err)
		}
		if strings.Contains(err.Error(), ratestest.Sentinel) {
			t.Fatalf("%d: the body reached the error: %v", code, err)
		}
	}
	// Through the fake: a provider's failing status carries the code and none of the body.
	fake := ratestest.New(t)
	fake.Fail("kraken", 500)
	_, err := Kraken(NewClient(fake.Transport())).Fetch(context.Background(), AssetBTC, []string{"USD"}, at2030)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 500 || strings.Contains(err.Error(), ratestest.Sentinel) {
		t.Fatalf("err %v", err)
	}
}

// TestClientErrorsCarryNoURL: a transport failure's error is the bare class, never Go's *url.Error,
// whose text holds the URL (an operator's custom URL may carry a token in its query).
func TestClientErrorsCarryNoURL(t *testing.T) {
	fake := ratestest.New(t)
	fake.Fail("custom", 500)
	c := NewClient(fake.Transport())
	const secretish = "token=NEVER-IN-AN-ERROR"
	for _, u := range []string{
		"https://unknown.example.invalid/rates?" + secretish, // refused by the fake
		"https://rates.example.invalid/rates?" + secretish,   // the fake answers 500
		"://bad?" + secretish,                                // the request cannot be built
	} {
		_, err := Custom(c, u).Fetch(context.Background(), AssetBTC, []string{"USD"}, at2030)
		if err == nil {
			t.Fatalf("%s: no error", u)
		}
		if strings.Contains(err.Error(), "NEVER-IN-AN-ERROR") || strings.Contains(err.Error(), "example.invalid") {
			t.Fatalf("the URL reached the error: %v", err)
		}
		var ue interface{ Timeout() bool }
		if errors.As(err, &ue) {
			t.Fatalf("an underlying error is wrapped: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Custom(c, "https://rates.example.invalid/?"+secretish).Fetch(ctx, AssetBTC, []string{"USD"}, at2030)
	if !errors.Is(err, ErrTransport) || strings.Contains(err.Error(), "NEVER-IN-AN-ERROR") {
		t.Fatalf("a cancelled fetch: %v", err)
	}
}

func TestClientNoProxy(t *testing.T) {
	c := NewClient(nil)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport %T", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("the transport takes a proxy")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion < 0x0303 {
		t.Fatal("TLS below 1.2 is allowed")
	}
	if !tr.ForceAttemptHTTP2 || tr.MaxResponseHeaderBytes != 8<<10 || tr.IdleConnTimeout != 30*time.Second {
		t.Fatalf("ForceAttemptHTTP2 %v, MaxResponseHeaderBytes %d, IdleConnTimeout %v",
			tr.ForceAttemptHTTP2, tr.MaxResponseHeaderBytes, tr.IdleConnTimeout)
	}
	if c.Timeout != FetchTimeout || FetchTimeout != 10*time.Second {
		t.Fatalf("Timeout %v", c.Timeout)
	}
	if MaxResponse != 256<<10 {
		t.Fatalf("MaxResponse %d", MaxResponse)
	}
	// Two clients never share a transport.
	if NewClient(nil).Transport == c.Transport {
		t.Fatal("a shared transport")
	}
	rt := http.RoundTripper(&http.Transport{})
	if NewClient(rt).Transport != rt {
		t.Fatal("the given round tripper is not used")
	}
}

// TestClientRefusesPlainHTTP: a URL that is not https is ErrTransport before any request, whatever the
// round tripper would do with it (the fake would happily serve it).
func TestClientRefusesPlainHTTP(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	b, err := get(context.Background(), NewClient(srv.Client().Transport), srv.URL)
	if !errors.Is(err, ErrTransport) || b != nil || hits.Load() != 0 {
		t.Fatalf("plain http: %d bytes, err %v, %d requests", len(b), err, hits.Load())
	}

	fake := ratestest.New(t)
	fake.Set("custom", "USD", "1")
	c := NewClient(fake.Transport())
	for _, u := range []string{"http://rates.example.invalid/", "ftp://rates.example.invalid/", "//rates.example.invalid/", "rates.example.invalid"} {
		rs, err := Custom(c, u).Fetch(context.Background(), AssetBTC, []string{"USD"}, at2030)
		wantErr(t, rs, err, ErrTransport)
	}
	if n := fake.Hits("custom"); n != 0 {
		t.Fatalf("%d requests for a URL that is not https", n)
	}
	// The same host over https reaches the fake.
	if rs, err := Custom(c, "https://rates.example.invalid/").Fetch(context.Background(), AssetBTC, []string{"USD"}, at2030); err != nil || len(rs) != 1 {
		t.Fatalf("https: %d rates, err %v", len(rs), err)
	}
}
