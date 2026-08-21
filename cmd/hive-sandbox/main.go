// Command hive-sandbox is the platform daemon: it hosts WASM guest apps, serves
// their dynamic REST and MCP surfaces, runs workflows, and drives agent
// harnesses.
//
// One process serves every role by default (D7). The role flags exist from day
// one so a heavy agent run can be split off from interactive traffic later
// without a code change.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	addr         string
	serveAPI     bool
	runWorkflows bool
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.StringVar(&cfg.addr, "addr", ":7979", "listen address for the HTTP surface")
	flag.BoolVar(&cfg.serveAPI, "serve-api", true, "serve REST, MCP and SSE")
	flag.BoolVar(&cfg.runWorkflows, "run-workflows", true, "claim and execute workflow steps")
	flag.Parse()

	if *showVersion {
		fmt.Println(version) //nolint:forbidigo // --version prints to stdout by design
		return nil
	}

	if !cfg.serveAPI && !cfg.runWorkflows {
		return errors.New("no role enabled: pass -serve-api, -run-workflows, or both")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("starting", "version", version, "serve_api", cfg.serveAPI, "run_workflows", cfg.runWorkflows)

	if !cfg.serveAPI {
		// A workflow-only process has no listener yet; the runner lands in
		// internal/workflow and will block here instead.
		<-ctx.Done()
		return nil
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           newMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Liveness only. Readiness needs Postgres and the bus, so it lands with them.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A failed write here means the client went away mid-response. Nothing
		// to do about it and nothing worth logging on a liveness probe.
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q}`+"\n", version)
	})

	return mux
}
