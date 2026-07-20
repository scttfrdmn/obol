// Command obold is the budget sidecar daemon. It holds the budget kernel and
// serves the wire protocol (GATE/BIND/SETTLE) to Slurm's job_submit shim over a
// local Unix socket; the obol CLI talks to it over the same socket.
//
// See docs/SEAM_DESIGN.md. The protocol is internal/wire; the server is
// internal/daemon.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/scttfrdmn/obol/internal/budget"
	"github.com/scttfrdmn/obol/internal/daemon"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("obold %s\n", version)
		return
	}

	fs := flag.NewFlagSet("obold", flag.ExitOnError)
	sock := fs.String("socket", "/run/obol/obold.sock", "path to the Unix listen socket")
	dir := fs.String("state-dir", "/var/lib/obol", "budget state directory (WAL + snapshot)")
	sync := fs.Bool("sync", true, "fdatasync the WAL on every append (production: true)")
	// Bootstrap parameters, used only when the state dir has no snapshot yet.
	create := fs.Bool("create", false, "create a fresh budget if none exists in -state-dir")
	rate := fs.Int64("rate", 1, "cost per second (units/sec) for a freshly created budget")
	b0 := fs.Int64("balance", 0, "initial allocation for a freshly created budget")
	window := fs.Duration("window", 30*24*time.Hour, "budget window length for a freshly created budget")
	if err := fs.Parse(os.Args[1:]); err != nil {
		log.Fatalf("obold: %v", err)
	}

	if err := run(*sock, *dir, *sync, *create, *rate, *b0, *window); err != nil {
		log.Fatalf("obold: %v", err)
	}
}

// run opens/creates the budget, binds the socket, and serves until signalled.
// Returning errors (rather than log.Fatal-ing inline) lets deferred cleanup run.
func run(sock, dir string, sync, create bool, rate, b0 int64, window time.Duration) error {
	bd, err := openOrCreate(dir, sync, create, rate, b0, window)
	if err != nil {
		return err
	}
	defer func() { _ = bd.Close() }()

	// The daemon supplies now from its own clock; the kernel stays clock-free.
	srv := daemon.New(bd, func() budget.Seconds { return time.Now().Unix() })

	if err := os.MkdirAll(filepath.Dir(sock), 0o750); err != nil {
		return fmt.Errorf("socket dir: %w", err)
	}
	_ = os.Remove(sock) // clear a stale socket from an unclean prior exit
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// Graceful shutdown: close the listener so Serve returns, then Close flushes.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		log.Println("obold: shutting down")
		_ = ln.Close()
	}()

	log.Printf("obold %s serving on %s (state %s)", version, sock, dir)
	if err := srv.Serve(ln); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// openOrCreate recovers an existing budget from dir, or creates a fresh one when
// -create is set and no snapshot exists. Without -create, a missing budget is a
// fatal misconfiguration rather than a silent empty budget.
func openOrCreate(dir string, sync, create bool, rate, b0 int64, window time.Duration) (*budget.Budget, error) {
	bd, err := budget.OpenBudget(dir, sync)
	if err == nil {
		return bd, nil
	}
	if !create {
		return nil, fmt.Errorf("open budget in %s: %w (pass -create to bootstrap)", dir, err)
	}
	secs := budget.Seconds(window.Seconds())
	return budget.NewDurable(dir, rate, b0, 0, secs, sync)
}
