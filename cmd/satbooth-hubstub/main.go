// Command satbooth-hubstub answers the requests Alby Hub makes to Alby's, mempool.space's, Boltz's and Megalith's
// servers, locally, over TLS (see package hubstub). It runs until SIGINT or SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/satbooth/satbooth/hubstub"
)

const shutdownBudget = 5 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stderr))
}

// run returns the exit code: 0 after a clean shutdown, 1 when serving fails, 2 for a usage or certificate error.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("satbooth-hubstub", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", ":443", "address to listen on")
	cert := fs.String("cert", "", "path of the PEM certificate")
	key := fs.String("key", "", "path of the PEM private key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *cert == "" || *key == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: satbooth-hubstub -cert <path> -key <path> [-listen <addr>]")
		return 2
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	srv, err := hubstub.NewServer(*listen, *cert, *key, hubstub.Handler(logger))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ln, err := srv.Listen()
	if err != nil {
		fmt.Fprintf(stderr, "listen: %v\n", err)
		return 1
	}
	logger.Info("listening", "addr", ln.Addr().String())
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
