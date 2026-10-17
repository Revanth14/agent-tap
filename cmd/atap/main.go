// Command atap is the agent-tap daemon and CLI: start runs the daemon; init,
// uninit and status wire Claude Code to it and back.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Revanth14/agent-tap/internal/adapters/claudecode"
	"github.com/Revanth14/agent-tap/internal/config"
	"github.com/Revanth14/agent-tap/internal/demo"
	"github.com/Revanth14/agent-tap/internal/install"
	"github.com/Revanth14/agent-tap/internal/server"
	"github.com/Revanth14/agent-tap/internal/telemetry"
)

// version is set at build time by the Makefile from git describe.
var version = "dev"

const usage = `usage:
  atap start  [--port N] [--log-wire] [--demo]   run the daemon
  atap init   [--port N]   install the LaunchAgent, then route Claude Code through it
  atap uninit [--port N]   undo init: settings first, then the LaunchAgent
  atap status [--port N]   show wiring, daemon health and what it has seen
  atap version             print the version`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "atap:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "start":
		return start(args[1:])
	case "init", "uninit", "status":
		return manage(args[0], args[1:])
	case "version", "--version":
		fmt.Println("atap", version)
		return nil
	}
	return errors.New(usage)
}

// manage runs init, uninit or status for the current user.
func manage(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	port := fs.Int("port", config.DefaultPort, "daemon port")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	env, err := install.System(*port)
	if err != nil {
		return err
	}
	switch cmd {
	case "init":
		return install.Init(env, os.Stdout)
	case "uninit":
		return install.Uninit(env, os.Stdout)
	default:
		return install.Status(env, os.Stdout)
	}
}

func start(args []string) error {
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.IntVar(&cfg.Port, "port", cfg.Port, "listen port")
	logWire := fs.Bool("log-wire", false, "log request/response metadata to stderr (never credentials or bodies)")
	demoMode := fs.Bool("demo", false, "also play simulated Claude Code traffic, for inspecting the UI")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *demoMode {
		// Keep simulated sessions out of the real history, so trying the
		// demo before atap init leaves nothing behind in the dashboard.
		cfg.DBPath = filepath.Join(filepath.Dir(cfg.DBPath), "demo.db")
	}

	store, err := telemetry.OpenStore(cfg.DBPath, 0)
	if err != nil {
		return err
	}
	defer store.Close()
	pruned, err := store.Prune(time.Now().Add(-cfg.Retention))
	if err != nil {
		return err
	}
	restored, err := store.Load()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "atap: %s (pruned %d rows older than %d days, restored %d sessions)\n",
		cfg.DBPath, pruned, int(cfg.Retention.Hours()/24), len(restored.Sessions))
	hub := telemetry.NewHub(store)

	var wire io.Writer
	if *logWire {
		wire = os.Stderr
	}
	rec := claudecode.NewRecorder(store, hub)
	rec.Restore(restored)
	proxy, err := claudecode.NewProxy(cfg.AnthropicUpstream, rec, wire)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		return err
	}
	// No WriteTimeout: SSE responses stay open for the whole generation.
	srv := &http.Server{
		Handler:           server.New(ln.Addr().String(), proxy, hub, rec.Routes()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln) }()
	fmt.Fprintf(os.Stderr, "atap: proxying http://%s -> %s\n", ln.Addr(), cfg.AnthropicUpstream)
	if *demoMode {
		go func() {
			if err := demo.Run(ctx, rec, demo.Options{Speed: 1, Every: time.Minute}); err != nil {
				errc <- fmt.Errorf("demo: %w", err)
			}
		}()
		fmt.Fprintln(os.Stderr, "atap: playing demo traffic (sessions demo-vscode-N and demo-cli-N)")
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
