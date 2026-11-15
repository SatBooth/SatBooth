package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/setup"
)

func TestUsageAndBadFiles(t *testing.T) {
	for name, args := range map[string][]string{
		"no flags":     {},
		"no key":       {"-cert", "c"},
		"extra":        {"-cert", "c", "-key", "k", "x"},
		"bad flag":     {"-nope"},
		"missing file": {"-cert", "/nonexistent/c", "-key", "/nonexistent/k"},
	} {
		var b bytes.Buffer
		if code := run(context.Background(), args, &b); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
		if strings.Contains(b.String(), "panic") {
			t.Errorf("%s: %s", name, b.String())
		}
	}
}

func TestServesAndStops(t *testing.T) {
	caCert, caKey, err := setup.NewCA(rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := setup.IssueStubCert(caCert, caKey, rand.New(rand.NewSource(2)))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "c.pem"), cert, 0o600)
	os.WriteFile(filepath.Join(dir, "k.pem"), key, 0o600)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caCert)
	// A free port: listen, note it, close, and give it to the command.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuf
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"-listen", addr, "-cert", filepath.Join(dir, "c.pem"), "-key", filepath.Join(dir, "k.pem")}, &out)
	}()
	var conn *tls.Conn
	for i := 0; i < 100; i++ {
		if conn, err = tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: "hubstub"}); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("GET /api/internal/info HTTP/1.1\r\nHost: hubstub\r\nConnection: close\r\n\r\n"))
	b, _ := io.ReadAll(conn)
	if !strings.Contains(string(b), `"healthy":true`) {
		t.Errorf("%q", b)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop")
	}
	if !strings.Contains(out.String(), "path=/api/internal/info") {
		t.Errorf("the request is not logged: %s", out.String())
	}
}

// syncBuf is a buffer the server goroutine and the test may both use.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
