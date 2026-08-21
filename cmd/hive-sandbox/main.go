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
	"strings"
	"syscall"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/egress"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	addr         string
	serveAPI     bool
	runWorkflows bool

	runEgressProxy     bool
	egressAddr         string
	egressAllow        stringList
	egressAllowPrivate bool
	egressDNS          stringList
}

// stringList collects a repeatable flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	// Comma-separated too, because the supervisor passes the allowlist as one
	// environment variable rather than N container arguments.
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			*s = append(*s, trimmed)
		}
	}
	return nil
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
	flag.BoolVar(&cfg.runEgressProxy, "run-egress-proxy", false,
		"run the allowlisting egress proxy for a harness run (D12.6)")
	flag.StringVar(&cfg.egressAddr, "egress-addr", ":3128", "listen address for the egress proxy")
	flag.Var(&cfg.egressAllow, "egress-allow",
		"host[:port] a run may reach; repeatable, or comma-separated. Absence is deny.")
	flag.Var(&cfg.egressDNS, "egress-dns",
		"resolver to query directly, bypassing resolv.conf; repeatable or comma-separated")
	flag.BoolVar(&cfg.egressAllowPrivate, "egress-allow-private", false,
		"permit RFC1918, loopback and link-local destinations. Off by default: this is the SSRF and DNS-rebinding control.")
	flag.Parse()

	if *showVersion {
		fmt.Println(version) //nolint:forbidigo // --version prints to stdout by design
		return nil
	}

	// The supervisor injects the allowlist as one variable rather than N
	// container arguments.
	if fromEnv := os.Getenv("HIVE_SANDBOX_EGRESS_ALLOW"); fromEnv != "" {
		if err := cfg.egressAllow.Set(fromEnv); err != nil {
			return fmt.Errorf("HIVE_SANDBOX_EGRESS_ALLOW: %w", err)
		}
	}

	if !cfg.serveAPI && !cfg.runWorkflows && !cfg.runEgressProxy {
		return errors.New("no role enabled: pass -serve-api, -run-workflows, -run-egress-proxy, or a combination")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("starting", "version", version,
		"serve_api", cfg.serveAPI,
		"run_workflows", cfg.runWorkflows,
		"run_egress_proxy", cfg.runEgressProxy)

	var servers []*http.Server
	errCh := make(chan error, 2)

	if cfg.runEgressProxy {
		proxySrv, err := egressServer(cfg)
		if err != nil {
			return err
		}
		servers = append(servers, proxySrv)
		go listen(proxySrv, "egress-proxy", cfg.egressAddr, errCh)
	}

	if cfg.serveAPI {
		apiSrv := &http.Server{
			Addr:              cfg.addr,
			Handler:           newMux(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		servers = append(servers, apiSrv)
		go listen(apiSrv, "api", cfg.addr, errCh)
	}

	if len(servers) == 0 {
		// A workflow-only process has no listener yet; the runner lands in
		// internal/workflow and will block here instead.
		<-ctx.Done()
		return nil
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var shutdownErr error
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
	}
	return shutdownErr
}

func listen(srv *http.Server, role, addr string, errCh chan<- error) {
	slog.Info("listening", "role", role, "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- fmt.Errorf("%s: %w", role, err)
	}
}

// egressServer builds the proxy from flags.
//
// An empty allowlist is permitted and denies everything. That is on purpose: a
// run that declared no egress should get a proxy that refuses, not a proxy that
// fails to start and takes the run down with an error that looks like a bug.
func egressServer(cfg config) (*http.Server, error) {
	allow, err := egress.ParseAllowlist(cfg.egressAllow)
	if err != nil {
		return nil, err
	}
	allow.AllowPrivateDestinations = cfg.egressAllowPrivate

	rules := make([]string, 0, len(allow.Rules()))
	for _, rule := range allow.Rules() {
		rules = append(rules, rule.String())
	}
	slog.Info("egress allowlist",
		"rules", strings.Join(rules, " "),
		"count", len(rules),
		"allow_private", cfg.egressAllowPrivate,
		"dns", strings.Join(egress.NormalizeDNSServers(cfg.egressDNS), " "))

	proxy := &egress.Proxy{
		Allow:      allow,
		RunID:      os.Getenv("HIVE_SANDBOX_RUN_ID"),
		DNSServers: egress.NormalizeDNSServers(cfg.egressDNS),
	}
	return proxy.Server(cfg.egressAddr), nil
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
