package hubstub

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/setup"
)

func serveOnce(h http.Handler, method, host, target string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "https://"+host+target, strings.NewReader(body))
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeStrict(t *testing.T, b []byte, v any) {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		t.Fatalf("does not decode into the hub's struct: %v\n%s", err, b)
	}
}

func newHandler() (http.Handler, *bytes.Buffer) {
	var logs bytes.Buffer
	return Handler(slog.New(slog.NewTextHandler(&logs, nil))), &logs
}

// Each answer the hub needs, on every host the stub serves, decodes into the hub's own struct.
func TestEndpointsDecodeIntoTheHubsStructs(t *testing.T) {
	h, _ := newHandler()
	for _, host := range []string{"hubstub", "getalby.com", "api.getalby.com"} {
		t.Run(host, func(t *testing.T) {
			rec := serveOnce(h, "GET", host, "/api/internal/info", "")
			if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("info: %d %q", rec.Code, rec.Header().Get("Content-Type"))
			}
			var info hubInfo
			decodeStrict(t, rec.Body.Bytes(), &info)
			// api/api.go:2128-2133: Health fails on an error and raises an alarm unless Healthy.
			if !info.Healthy || len(info.Incidents) != 0 || info.Incidents == nil {
				t.Errorf("info is not healthy with an empty incident list: %+v", info)
			}
			if info.Hub.LatestVersion != "1.24.7" || info.Hub.SwapProvider != "" {
				t.Errorf("hub block: %+v", info.Hub)
			}

			rec = serveOnce(h, "GET", host, "/api/internal/channel_suggestions", "")
			var sug []hubChannelPeerSuggestion
			decodeStrict(t, rec.Body.Bytes(), &sug)
			if rec.Code != 200 || sug == nil || len(sug) != 0 {
				t.Errorf("channel_suggestions: %d %s", rec.Code, rec.Body)
			}

			rec = serveOnce(h, "GET", host, "/api/rates", "")
			cur := map[string]hubCurrency{}
			decodeStrict(t, rec.Body.Bytes(), &cur)
			if rec.Code != 200 || len(cur) != 0 {
				t.Errorf("rates: %d %s", rec.Code, rec.Body)
			}

			rec = serveOnce(h, "GET", host, "/api/stories", "")
			var st []hubStory
			decodeStrict(t, rec.Body.Bytes(), &st)
			if rec.Code != 200 || st == nil || len(st) != 0 {
				t.Errorf("stories: %d %s", rec.Code, rec.Body)
			}
		})
	}
}

// Table: status by host and path. Anything not listed is a 404.
func TestStatusTable(t *testing.T) {
	h, _ := newHandler()
	for _, c := range []struct {
		method, host, target string
		want                 int
	}{
		{"GET", "getalby.com", "/api/rates/usd", 404}, // no rate: the UI shows no fiat amount
		{"GET", "getalby.com", "/api/rates/usd.json", 404},
		{"GET", "hubstub", "/api/tx/abcd", 503}, // MEMPOOL_API=https://hubstub/api
		{"GET", "hubstub", "/api/v1/fees/recommended", 503},
		{"GET", "hubstub", "/api/v1/lightning/nodes/02aa", 503},
		{"GET", "mempool.space", "/api/tx/abcd/status", 503},
		{"GET", "mempool.space", "/", 503},
		{"GET", "mempool.space", "/anything", 503},
		{"GET", "api.boltz.exchange", "/v2/swap/submarine", 503},
		{"POST", "api.boltz.exchange", "/v2/swap/reverse", 503},
		{"GET", "api.satsrouting.exchange", "/", 503},
		{"GET", "hubstub", "/v2/swap/submarine", 503}, // BOLTZ_API=https://hubstub
		{"POST", "hubstub", "/v2/swap/reverse", 503},
		{"GET", "hubstub", "/v2/", 503},
		{"GET", "getalby.com", "/api/internal/lsp/alby/bitcoin/v1/get_info", 404},
		{"GET", "api.getalby.com", "/internal/lsp/alby/bitcoin/v1/get_info", 404},
		{"GET", "api.getalby.com", "/users", 404},
		{"POST", "getalby.com", "/api/internal/info", 405},
		{"DELETE", "getalby.com", "/api/stories", 405},
		{"GET", "megalithic.me", "/", 404},
		{"POST", "megalithic.me", "/rebalance", 404},
		{"GET", "vss.getalbypro.com", "/vss/getValue", 404},
		{"GET", "hubstub", "/", 404},
		{"GET", "hubstub", "/api", 404},
		{"GET", "hubstub", "/api/internal/other", 404},
		{"GET", "example.com", "/api/internal/info", 404}, // a name the certificate does not cover
		{"GET", "hubstub", "/api/internal/info/", 404},
		{"GET", "HUBSTUB:443", "/api/internal/info", 200},
		{"HEAD", "hubstub", "/api/internal/info", 200},
	} {
		rec := serveOnce(h, c.method, c.host, c.target, "")
		if rec.Code != c.want {
			t.Errorf("%s %s%s: %d, want %d", c.method, c.host, c.target, rec.Code, c.want)
		}
		if rec.Code >= 400 {
			var e struct {
				Message string `json:"message"`
			}
			decodeStrict(t, rec.Body.Bytes(), &e)
			if e.Message == "" {
				t.Errorf("%s %s%s: no message", c.method, c.host, c.target)
			}
		}
	}
}

// The log line holds the method, the host and the path, and nothing of the body, the query or the headers.
func TestLogsCarryNoBodies(t *testing.T) {
	h, logs := newHandler()
	req := httptest.NewRequest("POST", "https://api.boltz.exchange/v2/swap/reverse?invoice=lnbc1secretquery", strings.NewReader("invoice=lnbc1secretbody"))
	req.Header.Set("Authorization", "Bearer secrettoken")
	req.Header.Set("User-Agent", "AlbyHub/v1.24.7")
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := logs.String()
	for _, bad := range []string{"secretbody", "secretquery", "secrettoken", "lnbc1"} {
		if strings.Contains(out, bad) {
			t.Errorf("the log holds %q: %s", bad, out)
		}
	}
	for _, want := range []string{"POST", "api.boltz.exchange", "/v2/swap/reverse"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q: %s", want, out)
		}
	}
	if strings.Count(strings.TrimRight(out, "\n"), "\n") != 0 {
		t.Errorf("more than one line: %q", out)
	}
}

// Seeded fuzz of host and path: any bytes, one status of the set, one log line, no control characters in the log.
func hostPathCase(t *testing.T, h http.Handler, logs *bytes.Buffer, method, host, path string) {
	t.Helper()
	logs.Reset()
	req := &http.Request{Method: method, Host: host, URL: &url.URL{Path: path}, Header: http.Header{},
		Body: http.NoBody, RemoteAddr: "192.0.2.1:1"}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	switch rec.Code {
	case 200, 404, 405, 503:
	default:
		t.Fatalf("%q %q %q: status %d", method, host, path, rec.Code)
	}
	out := strings.TrimSuffix(logs.String(), "\n")
	if strings.ContainsAny(out, "\r\n\x00\x1b") || out == "" {
		t.Fatalf("%q %q: log line %q", host, path, out)
	}
	if len(out) > 1024 {
		t.Fatalf("log line of %d bytes", len(out))
	}
	if rec.Body.Len() > 4096 {
		t.Fatalf("answer of %d bytes", rec.Body.Len())
	}
}

func TestHostPathSeeded(t *testing.T) {
	h, logs := newHandler()
	r := rand.New(rand.NewSource(11))
	hosts := append(setup.StubNames(), "", "example.com", "[::1]:443", "a:b:c", "HUBSTUB", "hubstub.")
	paths := []string{"/api/internal/info", "/api/stories", "/api/tx/1", "/v2/x", "/", "", "//", "/api//internal/info", "/api/internal/info/..", "/api/%2e"}
	junk := func() string {
		b := make([]byte, r.Intn(3000))
		for i := range b {
			b[i] = byte(r.Intn(256))
		}
		return string(b)
	}
	for i := 0; i < 3000; i++ {
		host, path := hosts[r.Intn(len(hosts))], paths[r.Intn(len(paths))]
		switch r.Intn(4) {
		case 0:
			host = junk()
		case 1:
			path = junk()
		case 2:
			path += junk()[:r.Intn(8)]
		}
		hostPathCase(t, h, logs, []string{"GET", "POST", "HEAD", "PUT", "X\nY", ""}[r.Intn(6)], host, path)
	}
}

func FuzzHostPath(f *testing.F) {
	f.Add("hubstub", "/api/internal/info")
	f.Add("getalby.com:443", "/api/stories")
	f.Add("mempool.space", "/api/tx/\x00")
	f.Add("a\r\nb", "/v2/\n")
	h, logs := newHandler()
	f.Fuzz(func(t *testing.T, host, path string) {
		hostPathCase(t, h, logs, "GET", host, path)
	})
}

func writeCerts(t *testing.T) (dir string, pool *x509.CertPool) {
	t.Helper()
	caCert, caKey, err := setup.NewCA(rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := setup.IssueStubCert(caCert, caKey, rand.New(rand.NewSource(2)))
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	for n, b := range map[string][]byte{"stub-cert.pem": cert, "stub-key.pem": key} {
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pool = x509.NewCertPool()
	pool.AppendCertsFromPEM(caCert)
	return dir, pool
}

// TLS is served from the two files, for every name of the certificate: a client trusting only the CA gets the answer.
func TestServesTLSFromTheFiles(t *testing.T) {
	dir, pool := writeCerts(t)
	h, _ := newHandler()
	srv, err := NewServer("127.0.0.1:0", filepath.Join(dir, "stub-cert.pem"), filepath.Join(dir, "stub-key.pem"), h)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	for _, name := range setup.StubNames() {
		conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: name})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		req, _ := http.NewRequest("GET", "https://"+name+"/api/internal/info", nil)
		req.Close = true
		if err := req.Write(conn); err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(io.LimitReader(conn, 4096))
		conn.Close()
		if !strings.HasPrefix(string(b), "HTTP/1.1 ") {
			t.Errorf("%s: %q", name, b)
		}
		if alby := name == "hubstub" || name == "getalby.com" || name == "api.getalby.com"; alby !=
			(strings.HasPrefix(string(b), "HTTP/1.1 200") && strings.Contains(string(b), `"healthy":true`)) {
			t.Errorf("%s: unexpected answer %q", name, b)
		}
	}
	// A name the certificate does not cover fails verification, and so does a client that trusts no CA of ours.
	if _, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "blockstream.info"}); err == nil {
		t.Error("a foreign name verified")
	}
	if _, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "hubstub"}); err == nil {
		t.Error("verified without our CA")
	}
	// Old protocol versions are refused.
	if _, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "hubstub", MaxVersion: tls.VersionTLS11}); err == nil {
		t.Error("TLS 1.1 accepted")
	}
}

func TestNewServerRefusesBadFiles(t *testing.T) {
	dir, _ := writeCerts(t)
	h, _ := newHandler()
	for name, c := range map[string][2]string{
		"missing":  {filepath.Join(dir, "no"), filepath.Join(dir, "stub-key.pem")},
		"swapped":  {filepath.Join(dir, "stub-key.pem"), filepath.Join(dir, "stub-cert.pem")},
		"mismatch": {filepath.Join(dir, "stub-cert.pem"), filepath.Join(dir, "stub-cert.pem")},
	} {
		if _, err := NewServer("127.0.0.1:0", c[0], c[1], h); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The hosts the stub knows are the hosts of the certificate, no more and no fewer.
func TestKnownHostsAreTheCertificatesNames(t *testing.T) {
	for _, n := range setup.StubNames() {
		if !knownHost(n) {
			t.Errorf("%s is in the certificate and unknown to the stub", n)
		}
	}
	for _, n := range []string{"relay.getalby.com", "blockstream.info", "www.getalby.com", ""} {
		if knownHost(n) {
			t.Errorf("%s is known", n)
		}
	}
	n := 0
	for _, h := range []string{hostStub, hostAlby, hostAlbyAPI, hostMempool, hostBoltz, hostBoltzRouting, "megalithic.me",
		"electrum.getalbypro.com", "vss.getalbypro.com"} {
		if knownHost(h) {
			n++
		}
	}
	if n != len(setup.StubNames()) {
		t.Errorf("%d known hosts, %d names", n, len(setup.StubNames()))
	}
}
