// Command satbooth is the SatBooth service. It reads one JSON configuration file (-config <path>),
// serves the checkout page on the public listener and the merchant API on the private one, and runs
// until SIGINT or SIGTERM. SIGHUP reloads the API keys. A second SIGINT or SIGTERM exits at once.
//
// satbooth backup -config <path> -to <dir> copies the stopped service's store and ledger into the new
// directory dir as one matched pair, with a manifest.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/satbooth/satbooth/memlock"
	"github.com/satbooth/satbooth/service"
)

func main() {
	sigs := make(chan os.Signal, 8)
	service.Notify(sigs)
	os.Exit(run(context.Background(), os.Args[1:], os.Stderr, sigs, os.Exit, memlock.Protect))
}

// run returns the exit code: 0 after a clean shutdown, 1 when the service fails, 2 for a usage or
// configuration error. exit is called (with 1) on a second stop signal. protect is memlock.Protect; it
// runs after the configuration is parsed (allow_swap lives there) and before any secret file is opened.
func run(ctx context.Context, args []string, stderr io.Writer, sigs <-chan os.Signal, exit func(int), protect func() (memlock.Status, error)) int {
	if len(args) > 0 && args[0] == "backup" {
		return backup(ctx, args[1:], stderr, sigs)
	}
	fs := flag.NewFlagSet("satbooth", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "path of the JSON configuration file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: satbooth -config <path>")
		return 2
	}
	// Everything after the flags is logged as slog JSON with fixed messages; the error texts carry no values.
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	cfg, err := service.ReadConfig(*path)
	if err != nil {
		logger.Error("configuration refused", "error", err.Error())
		return 2
	}
	if err := service.Lock(cfg, logger, protect); err != nil {
		logger.Error("satbooth failed", "error", err.Error())
		return 1
	}
	if err := cfg.CheckFiles(); err != nil {
		logger.Error("configuration refused", "error", err.Error())
		return 2
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reload := make(chan struct{}, 1)
	go service.Supervise(sigs, cancel, reload, exit)
	if err := service.Run(ctx, cfg, service.WithReload(reload), service.WithLogger(logger)); err != nil {
		logger.Error("satbooth failed", "error", err.Error())
		return 1
	}
	return 0
}

// backup runs satbooth backup and returns the exit code: 0 after a backup, 1 when it fails, 2 for a usage or
// configuration error. It opens no secret file and needs no memory lock: it reads only the configuration and the
// data directory. Everything after the flags is one slog JSON line with a fixed message: the success line carries
// the generation, a failure the backup's fixed error text. A stop signal cancels the backup, which then fails and
// leaves no manifest.
func backup(ctx context.Context, args []string, stderr io.Writer, sigs <-chan os.Signal) int {
	fs := flag.NewFlagSet("satbooth backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "path of the JSON configuration file")
	to := fs.String("to", "", "the backup directory to create")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" || *to == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: satbooth backup -config <path> -to <dir>")
		return 2
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	cfg, err := service.ReadConfig(*path)
	if err != nil {
		logger.Error("configuration refused", "error", err.Error())
		return 2
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A signal that came before the backup starts cancels it too, without waiting for the goroutine below.
	select {
	case <-sigs:
		cancel()
	default:
	}
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()
	gen, err := service.Backup(ctx, cfg, *to, time.Now)
	if err != nil {
		logger.Error("backup failed", "error", err.Error())
		return 1
	}
	logger.Info("backup written", "generation", gen)
	return 0
}
