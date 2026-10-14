// Command atap is the agent-tap daemon and CLI.
//
// Milestone 2: only `atap start`, running the proxy, the recorder, and the
// state API. The store is memory-only until the retention work in Milestone 3.
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
	"syscall"
	"time"

	"github.com/Revanth14/agent-tap/internal/adapters/claudecode"
	"github.com/Revanth14/agent-tap/internal/config"
	"github.com/Revanth14/agent-tap/internal/server"
	"github.com/Revanth14/agent-tap/internal/telemetry"
)

const usage = "usage: atap start [--port N] [--log-wire]"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "atap:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "start" {
		return errors.New(usage)
	}
	return start(args[1:])
}

func start(args []string) error {
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.IntVar(&cfg.Port, "port", cfg.Port, "listen port")
	logWire := fs.Bool("log-wire", false, "log request/response metadata to stderr (never credentials or bodies)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		return err
	}
	defer store.Close()
	hub := telemetry.NewHub(store)

	var wire io.Writer
	if *logWire {
		wire = os.Stderr
	}
	rec := claudecode.NewRecorder(store, hub)
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
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	fmt.Fprintf(os.Stderr, "atap: proxying http://%s -> %s\n", ln.Addr(), cfg.AnthropicUpstream)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
