// Command satbooth-relay is SatBooth's NWC-only Nostr relay. It reads one JSON configuration file
// (-config <path>), listens on two addresses and runs until SIGINT or SIGTERM.
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

	"github.com/satbooth/satbooth/relay"
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

// run returns the exit code: 0 after a clean shutdown, 1 when serving fails, 2 for a usage or configuration error.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("satbooth-relay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "path of the JSON configuration file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: satbooth-relay -config <path>")
		return 2
	}
	cfg, err := relay.LoadConfig(*path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	srv := relay.NewServer(cfg, logger, time.Now)

	var lc net.ListenConfig
	hubLn, err := lc.Listen(ctx, "tcp", cfg.HubListen)
	if err != nil {
		fmt.Fprintf(stderr, "listen on hub_listen: %v\n", err)
		return 1
	}
	clientLn, err := lc.Listen(ctx, "tcp", cfg.ClientListen)
	if err != nil {
		_ = hubLn.Close()
		fmt.Fprintf(stderr, "listen on client_listen: %v\n", err)
		return 1
	}

	hubHTTP := &http.Server{Handler: srv.Handler(relay.HubListener), ReadHeaderTimeout: readHeaderTimeout}
	clientHTTP := &http.Server{Handler: srv.Handler(relay.ClientListener), ReadHeaderTimeout: readHeaderTimeout}
	errc := make(chan error, 2)
	go func() { errc <- hubHTTP.Serve(hubLn) }()
	go func() { errc <- clientHTTP.Serve(clientLn) }()
	logger.Info("serving", "hub", hubLn.Addr().String(), "client", clientLn.Addr().String())

	code := 0
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "serve: %v\n", err)
			code = 1
		}
	}

	// Stop accepting on both listeners, then close the WebSocket connections that Shutdown does not track
	// (hijacked connections), all inside one budget.
	sctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()
	_ = hubHTTP.Shutdown(sctx)
	_ = clientHTTP.Shutdown(sctx)
	srv.Shutdown(sctx)
	return code
}
