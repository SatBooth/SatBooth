package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/hubproxy"
)

func TestUsage(t *testing.T) {
	for name, args := range map[string][]string{
		"none": {}, "no upstream": {"-listen", ":1"}, "extra": {"-listen", ":1", "-upstream", "http://127.0.0.1:2", "x"},
		"bad flag": {"-x"}, "not loopback": {"-listen", ":1", "-upstream", "http://10.0.0.1:2"},
		"https": {"-listen", ":1", "-upstream", "https://127.0.0.1:2"},
	} {
		var b bytes.Buffer
		if code := run(context.Background(), args, &b); code != 2 || strings.Contains(b.String(), "panic") {
			t.Errorf("%s: exit %d %s", name, code, b.String())
		}
	}
}

func TestServesAndStops(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src *")
		io.WriteString(w, "hub")
	}))
	defer hub.Close()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"-listen", addr, "-upstream", hub.URL}, io.Discard) }()
	var r *http.Response
	var err error
	for i := 0; i < 100; i++ {
		if r, err = http.Get("http://" + addr + "/"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	if string(b) != "hub" || r.Header.Get("Content-Security-Policy") != hubproxy.Policy {
		t.Errorf("%q %q", b, r.Header.Get("Content-Security-Policy"))
	}
	cancel()
	select {
	case c := <-done:
		if c != 0 {
			t.Errorf("exit %d", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop")
	}
}
