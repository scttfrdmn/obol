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
	var cfg config
	fs.StringVar(&cfg.sock, "socket", "/run/obol/obold.sock", "path to the Unix listen socket")
	fs.StringVar(&cfg.dir, "state-dir", "/var/lib/obol", "budget state directory (per-account WAL + snapshot)")
	fs.StringVar(&cfg.configPath, "config", "", "multi-account config (JSON); omit for the single-budget flags below")
	fs.BoolVar(&cfg.sync, "sync", true, "fdatasync the WAL on every append (production: true)")
	// Bootstrap parameters, used only when the state dir has no snapshot yet.
	fs.BoolVar(&cfg.create, "create", false, "create a fresh budget if none exists in -state-dir")
	fs.Int64Var(&cfg.rate, "rate", 1, "flat cost per second (units/sec) for a freshly created budget")
	fs.Int64Var(&cfg.b0, "balance", 0, "initial allocation for a freshly created budget")
	fs.DurationVar(&cfg.window, "window", 30*24*time.Hour, "budget window length for a freshly created budget")
	// TRES cost weights (SEAM_DESIGN §5). All-zero (default) = flat rate: cost is
	// the budget's -rate × walltime. Set any weight to bill per allocated resource.
	fs.Int64Var(&cfg.weights.PerCPU, "tres-per-cpu", 0, "cost per allocated CPU-second (0 = flat rate)")
	fs.Int64Var(&cfg.weights.PerGPU, "tres-per-gpu", 0, "cost per allocated GPU-second")
	fs.Int64Var(&cfg.weights.PerMem, "tres-per-mem", 0, "cost per allocated MB-second")
	if err := fs.Parse(os.Args[1:]); err != nil {
		log.Fatalf("obold: %v", err)
	}

	if err := run(cfg); err != nil {
		log.Fatalf("obold: %v", err)
	}
}

// config holds the parsed obold flags.
type config struct {
	sock, dir    string
	configPath   string
	sync, create bool
	rate, b0     int64
	window       time.Duration
	weights      daemon.Weights
}

// nowClock is the daemon's wall clock as epoch seconds, fed into transitions so
// the kernel stays clock-free.
func nowClock() budget.Seconds { return time.Now().Unix() }

// run builds the budget registry (multi-account via -config, else a single
// budget from the flags), binds the socket, and serves until signalled.
func run(cfg config) error {
	reg, parsed, err := buildRegistry(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = reg.Close() }()

	srv := daemon.NewWithRegistry(reg, nowClock, cfg.weights)
	// Node-type pricing (issue #65), if configured.
	nc, err := daemon.BuildNodeCost(parsed)
	if err != nil {
		return err
	}
	srv.SetNodeCost(nc)

	if err := os.MkdirAll(filepath.Dir(cfg.sock), 0o750); err != nil {
		return fmt.Errorf("socket dir: %w", err)
	}
	_ = os.Remove(cfg.sock) // clear a stale socket from an unclean prior exit
	ln, err := net.Listen("unix", cfg.sock)
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

	log.Printf("obold %s serving on %s (state %s)", version, cfg.sock, cfg.dir)
	if err := srv.Serve(ln); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// buildRegistry constructs the budget registry. With -config it loads the
// multi-account config and opens/creates a budget per account under
// <state-dir>/<name>/. Without -config it falls back to the single-budget flags
// (one account named "default"), preserving all pre-multi-account behavior.
func buildRegistry(cfg config) (*daemon.Registry, *daemon.Config, error) {
	if cfg.configPath != "" {
		c, err := daemon.LoadConfig(cfg.configPath)
		if err != nil {
			return nil, nil, err
		}
		reg, err := daemon.NewRegistry(c, cfg.dir, cfg.sync, nowClock)
		return reg, c, err
	}
	// Single-budget back-compat: synthesize a one-account config named "default".
	// -create is implied (the flags ARE the bootstrap); state lives directly in
	// -state-dir/default/ so recovery works across restarts.
	if !cfg.create {
		// Preserve the old "must pass -create" guard when the dir is empty: try to
		// open first, and if that fails, require -create.
		if _, err := budget.OpenBudget(filepath.Join(cfg.dir, "default"), cfg.sync); err != nil {
			return nil, nil, fmt.Errorf("no budget in %s/default: %w (pass -create or -config)", cfg.dir, err)
		}
	}
	one := &daemon.Config{Accounts: []daemon.AccountConfig{{
		Name: "default", Balance: cfg.b0, Rate: cfg.rate, Window: cfg.window.String(),
	}}}
	reg, err := daemon.NewRegistry(one, cfg.dir, cfg.sync, nowClock)
	return reg, one, err
}
