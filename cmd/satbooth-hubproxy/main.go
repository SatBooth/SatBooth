// Command satbooth-hubproxy is the reverse proxy in front of Alby Hub's web UI: it forwards to the hub in the same
// network namespace and replaces the response's Content-Security-Policy with a self-only one (see package hubproxy).
// It runs until SIGINT or SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/satbooth/satbooth/hubproxy"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownBudget    = 5 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stderr))
}

// run returns the exit code: 0 after a clean shutdown, 1 when serving fails, 2 for a usage error.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("satbooth-hubproxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "address to listen on")
	upstream := fs.String("upstream", "", "the hub's address, http://<loopback address>:<port>")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *listen == "" || *upstream == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: satbooth-hubproxy -listen <addr> -upstream http://127.0.0.1:<port>")
		return 2
	}
	u, err := hubproxy.ParseUpstream(*upstream)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, "listen: %v\n", err)
		return 1
	}
	srv := &http.Server{Handler: hubproxy.Handler(u, logger), ReadHeaderTimeout: readHeaderTimeout, IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: 64 << 10}
	logger.Info("listening", "addr", ln.Addr().String(), "upstream", u.String())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		fmt.Fprintf(stderr, "serve: %v\n", err)
		return 1
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		srv.Close()
	}
	return 0
}
