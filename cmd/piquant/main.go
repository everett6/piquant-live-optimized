// Command piquant is the entry point for the PiQuant research system.
//
// Subcommands:
//
//	setup      guided first-run configuration (role detection, tokens, peering)
//	doctor     read-only diagnostics for the current board
//	dashboard  the read-only control panel served by the Pi 3B
//	agent      the status endpoint the research engine exposes to the panel
//	secrets    manage the local credential store
//	version    print build information
//
// The application is an offline research and forecasting system. It never
// connects to an exchange account, never submits an order, and needs no
// financial-account credential; market data is public and replay is local.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

// Build-time information. Override with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "0.2.0-dev"
	commit  = "unknown"
	builtAt = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "setup":
		err = runSetup(ctx, args)
	case "doctor":
		err = runDoctor(ctx, args)
	case "dashboard":
		err = runDashboard(ctx, args)
	case "agent":
		err = runAgent(ctx, args)
	case "dry-run", "dryrun":
		err = runDryRun(ctx, args)
	case "secrets":
		err = runSecrets(args)
	case "version", "-v", "--version":
		printVersion()
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "piquant: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "piquant %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `piquant - offline quantitative research system

  piquant setup       guided first-run configuration
  piquant doctor      check this board and its network peer
  piquant dashboard   serve the read-only control panel (Raspberry Pi 3B)
  piquant agent       expose engine status to the panel (Raspberry Pi 5)
  piquant dry-run     launch 14-day autonomous paper money dry run with live public feeds
  piquant secrets     manage the local credential store
  piquant version     print build information

Run "piquant <command> -h" for the options of one command.
`)
}

func printVersion() {
	fmt.Printf("piquant %s\ncommit %s\nbuilt %s (%s/%s, %s)\n",
		version, commit, builtAt, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// defaultDataDir is where captures and results live for this user.
func defaultDataDir() string {
	if d := os.Getenv("PIQUANT_DATA_DIR"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/piquant-data"
	}
	return "data"
}

// defaultLogDir is where the services write their logs.
func defaultLogDir() string {
	if d := os.Getenv("PIQUANT_LOG_DIR"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/.local/state/piquant/logs"
	}
	return "logs"
}

// openLog points the standard logger at a file, keeping stderr as a fallback so
// a missing directory can never silence the process.
func openLog(dir, name string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create the log directory %s: %w", dir, err)
	}
	f, err := os.OpenFile(dir+"/"+name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s: %w", dir+"/"+name, err)
	}
	return f, nil
}

// waitForShutdown blocks until ctx is cancelled.
func waitForShutdown(ctx context.Context) {
	<-ctx.Done()
	time.Sleep(50 * time.Millisecond) // let in-flight writes finish
}
