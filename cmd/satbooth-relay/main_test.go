package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// syncBuffer lets the test read stderr while run's logger writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// exampleConfig copies the example config with free loopback ports.
func exampleConfig(t *testing.T) (path, hub, client string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "relay", "testdata", "config-example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	hub, client = freeAddr(t), freeAddr(t)
	m["hub_listen"] = hub
	m["client_listen"] = client
	m["client_relay_url"] = "ws://" + client
	out, _ := json.Marshal(m)
	path = filepath.Join(t.TempDir(), "relay.json")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, hub, client
}

func canDial(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func TestRunExample(t *testing.T) {
	path, hub, client := exampleConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"-config", path}, &stderr) }()

	deadline := time.Now().Add(5 * time.Second)
	for !(canDial(hub) && canDial(client)) {
		if time.Now().After(deadline) {
			t.Fatalf("listeners did not come up; stderr: %s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d, stderr: %s", code, stderr.String())
		}
	case <-time.After(shutdownBudget + 3*time.Second):
		t.Fatal("run did not return after cancel")
	}
	if canDial(hub) {
		t.Error("hub listener still accepts after shutdown")
	}
	if canDial(client) {
		t.Error("client listener still accepts after shutdown")
	}
}

func TestRunBadConfig(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"hub_listen":"0.0.0.0:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"invalid config": {"-config", bad},
		"missing file":   {"-config", filepath.Join(t.TempDir(), "absent.json")},
		"no flag":        {},
		"unknown flag":   {"-x"},
		"extra argument": {"-config", bad, "more"},
	}
	for name, args := range cases {
		var stderr bytes.Buffer
		if code := run(context.Background(), args, &stderr); code != 2 {
			t.Errorf("%s: exit code %d, want 2", name, code)
		}
		if !strings.Contains(stderr.String(), "config") && !strings.Contains(stderr.String(), "usage") && name != "unknown flag" {
			t.Errorf("%s: no error on stderr: %q", name, stderr.String())
		}
	}
}

func TestRunPortInUse(t *testing.T) {
	path, hub, _ := exampleConfig(t)
	l, err := net.Listen("tcp", hub)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"-config", path}, &stderr); code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
}
