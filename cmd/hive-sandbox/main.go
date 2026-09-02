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
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/blob"
	"github.com/bees-roadhouse/hive-sandbox/internal/bus"
	"github.com/bees-roadhouse/hive-sandbox/internal/chat"
	"github.com/bees-roadhouse/hive-sandbox/internal/egress"
	"github.com/bees-roadhouse/hive-sandbox/internal/harness"
	"github.com/bees-roadhouse/hive-sandbox/internal/httpapi"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/wasmhost"
	"github.com/bees-roadhouse/hive-sandbox/internal/webui"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	addr         string
	unixSocket   string
	serveAPI     bool
	runWorkflows bool

	databaseURL string
	migrate     bool

	blob blobConfig

	runChat         bool
	harnessPins     string
	podman          string
	chatWorkspaces  string
	chatConcurrency int
	chatDeadline    time.Duration

	runEgressProxy     bool
	egressAddr         string
	egressAllow        stringList
	egressAllowPrivate bool
	egressDNS          stringList
}

// needsDatabase reports whether the enabled roles read or write platform state.
//
// The egress proxy deliberately does not: it runs inside a harness container
// beside the run it is fencing, with no reason to reach Postgres and no
// credentials to reach it with. Requiring a connection string there would make
// every run depend on the database being up in order to be *denied* network
// access, which is backwards.
func (c config) needsDatabase() bool { return c.serveAPI || c.runWorkflows || c.runChat }

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
	flag.StringVar(&cfg.unixSocket, "unix-socket", os.Getenv("HIVE_SANDBOX_UNIX_SOCKET"),
		"also serve the API on this unix socket path (env HIVE_SANDBOX_UNIX_SOCKET)")
	flag.BoolVar(&cfg.serveAPI, "serve-api", true, "serve REST, MCP and SSE")
	flag.BoolVar(&cfg.runWorkflows, "run-workflows", true, "claim and execute workflow steps")
	flag.StringVar(&cfg.databaseURL, "database-url", os.Getenv("HIVE_SANDBOX_DATABASE_URL"),
		"Postgres connection string (or HIVE_SANDBOX_DATABASE_URL)")
	flag.BoolVar(&cfg.migrate, "migrate", true, "apply pending migrations at boot")
	flag.StringVar(&cfg.blob.driver, "blob-driver", envOr("HIVE_SANDBOX_BLOB_DRIVER", "disk"),
		"blob backend: disk or s3 (env HIVE_SANDBOX_BLOB_DRIVER)")
	flag.StringVar(&cfg.blob.root, "blob-root", envOr("HIVE_SANDBOX_BLOB_ROOT", "/var/lib/hive/blobs"),
		"blob root for the disk driver (env HIVE_SANDBOX_BLOB_ROOT)")
	flag.BoolVar(&cfg.runChat, "run-chat", true, "answer chat turns with hosted agent runs")
	flag.StringVar(&cfg.harnessPins, "harness-pins", envOr("HIVE_SANDBOX_HARNESS_PINS", harness.DefaultPinsPath),
		"the image lockfile scripts/harness-build.sh writes (env HIVE_SANDBOX_HARNESS_PINS)")
	flag.StringVar(&cfg.podman, "podman", envOr("HIVE_SANDBOX_PODMAN", "podman"),
		"podman binary a harness run is launched with (env HIVE_SANDBOX_PODMAN)")
	flag.StringVar(&cfg.chatWorkspaces, "chat-workspaces",
		envOr("HIVE_SANDBOX_CHAT_WORKSPACES", "/var/lib/hive/workspaces"),
		"directory holding one workspace per conversation (env HIVE_SANDBOX_CHAT_WORKSPACES)")
	flag.IntVar(&cfg.chatConcurrency, "chat-concurrency", 2, "how many chat turns run at once")
	flag.DurationVar(&cfg.chatDeadline, "chat-deadline", 10*time.Minute, "wall clock one chat turn gets")
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

	if !cfg.serveAPI && !cfg.runWorkflows && !cfg.runChat && !cfg.runEgressProxy {
		return errors.New("no role enabled: pass -serve-api, -run-workflows, -run-chat, -run-egress-proxy, or a combination")
	}
	if cfg.needsDatabase() && cfg.databaseURL == "" {
		return errors.New("no database: pass -database-url or set HIVE_SANDBOX_DATABASE_URL")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("starting", "version", version,
		"serve_api", cfg.serveAPI,
		"run_workflows", cfg.runWorkflows,
		"run_chat", cfg.runChat,
		"run_egress_proxy", cfg.runEgressProxy)

	var (
		st        *store.Store
		eventer   *bus.Bus
		catalog   *blob.Catalog
		chatLayer *store.Chat
		hub       = chat.NewHub()
		wake      func()
		wg        sync.WaitGroup
	)
	defer wg.Wait()

	if cfg.needsDatabase() {
		var err error
		st, err = store.Open(ctx, cfg.databaseURL)
		if err != nil {
			return err
		}
		defer st.Close()

		if err := prepare(ctx, st, cfg); err != nil {
			return err
		}

		// The guest data layer and the runtime that calls it. Built here
		// rather than lazily because a daemon that comes up and only discovers
		// at first guest call that it has no blob backend has already told an
		// orchestrator it was ready.
		driver, dErr := blobDriver(cfg.blob)
		if dErr != nil {
			return dErr
		}
		var cErr error
		catalog, cErr = blob.NewCatalog(st.Pool(), driver)
		if cErr != nil {
			return cErr
		}
		appData, aErr := store.NewAppData(st, catalog, slog.Default())
		if aErr != nil {
			return aErr
		}
		guestEvents, eErr := store.NewGuestEvents(st)
		if eErr != nil {
			return eErr
		}
		guestBlobs, bErr := store.NewGuestBlobs(st, catalog, slog.Default())
		if bErr != nil {
			return bErr
		}

		// KV and Sanitizer stay stubbed: both are unbuilt, and a nil field
		// resolves to a stub that answers StatusUnimplemented rather than
		// crashing. Storage, Blob and Events are real.
		host, hErr := wasmhost.New(ctx, wasmhost.Config{}, wasmhost.Deps{
			Storage: appData,
			Blob:    guestBlobs,
			Events:  guestEvents,
		})
		if hErr != nil {
			return hErr
		}
		defer func() {
			// Its own context: ctx is already cancelled by the time this runs,
			// and a Close that inherits a dead context tears down without
			// draining.
			closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := host.Close(closeCtx); err != nil {
				slog.Error("wasm host close", "err", err)
			}
		}()
		slog.Info("wasm host ready", "blob_driver", driver.Name())

		eventer = bus.New(st.Pool(), bus.Config{Logger: slog.Default()})
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := eventer.Run(ctx); err != nil {
				slog.Error("bus stopped", "err", err)
			}
		}()

		var chatErr error
		if chatLayer, chatErr = store.NewChat(st); chatErr != nil {
			return chatErr
		}
		if cfg.runChat {
			worker, wErr := chatWorker(cfg, st, chatLayer, hub)
			if wErr != nil {
				return wErr
			}
			if worker != nil {
				wake = worker.Kick
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := worker.Run(ctx); err != nil {
						slog.Error("chat worker stopped", "err", err)
					}
				}()
			}
		}
	}

	var servers []*http.Server
	// Buffered for every listener that can report: egress, api, api-unix. An
	// unbuffered send from a listener nobody is reading would leak its
	// goroutine on the shutdown path.
	errCh := make(chan error, 3)

	if cfg.runEgressProxy {
		proxySrv, err := egressServer(cfg)
		if err != nil {
			return err
		}
		servers = append(servers, proxySrv)
		go listen(proxySrv, "egress-proxy", cfg.egressAddr, errCh)
	}

	if cfg.serveAPI {
		mux := httpapi.New(st, eventer, httpapi.Options{
			Version: version, Blobs: catalog, Chat: chatLayer, Hub: hub, Wake: wake,
		})
		// The browser client, at the root. Two patterns that no API route
		// shares, so a file can never shadow an endpoint.
		webui.Mount(mux)
		apiSrv := &http.Server{
			Addr:              cfg.addr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			// Deliberately no WriteTimeout: an SSE response is meant to stay
			// open. The stream sets its own per-write deadline instead.
		}
		servers = append(servers, apiSrv)
		go listen(apiSrv, "api", cfg.addr, errCh)

		// The SAME server on a second listener, so one Shutdown drains both and
		// the socket cannot outlive the port it is meant to mirror.
		//
		// Invariant 13: a harness container runs --network=none with this file
		// bind-mounted, because on rootless Podman an --internal network has no
		// gateway and cannot reach the host at all. Without this the harness has
		// no route to the API and the failure looks like a bug inside the run.
		if cfg.unixSocket != "" {
			ln, err := unixListener(ctx, cfg.unixSocket)
			if err != nil {
				return err
			}
			go serve(apiSrv, "api-unix", ln, errCh)
		}
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

// chatWorker builds the turn worker, or returns nil when there is nothing to
// run turns on.
//
// No pins file is a warning rather than a boot failure: a development daemon,
// the end-to-end suite and a fresh install all come up before anyone has built
// a harness image, and a chat that queues turns nothing answers is visible in
// the thread ("waiting for an agent") while a daemon that refuses to start is
// visible nowhere. A pins file that exists and cannot be read IS a failure.
func chatWorker(cfg config, st *store.Store, chatLayer *store.Chat, hub *chat.Hub) (*chat.Worker, error) {
	pins, err := harness.LoadPins(cfg.harnessPins)
	if errors.Is(err, fs.ErrNotExist) {
		slog.Warn("chat worker disabled: no harness image pins; run scripts/harness-build.sh",
			"path", cfg.harnessPins)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// A turn reaches the daemon over the socket and nothing else (invariant
	// 13), so a daemon that answers turns has to have one.
	if cfg.unixSocket == "" {
		return nil, errors.New("-run-chat needs -unix-socket: a harness run reaches the API only through it")
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "daemon"
	}
	sup := &harness.Supervisor{Launcher: &harness.PodmanLauncher{Binary: cfg.podman}}
	worker, err := chat.NewWorker(st, chatLayer, sup, hub, chat.Config{
		Name:          host + "/" + strconv.Itoa(os.Getpid()),
		Pins:          pins,
		DaemonSocket:  cfg.unixSocket,
		WorkspaceRoot: cfg.chatWorkspaces,
		Deadline:      cfg.chatDeadline,
		Concurrency:   cfg.chatConcurrency,
		Logger:        slog.Default(),
	})
	if err != nil {
		return nil, err
	}
	slog.Info("chat worker ready", "pins", cfg.harnessPins, "workspaces", cfg.chatWorkspaces,
		"concurrency", cfg.chatConcurrency)
	return worker, nil
}

// prepare brings the schema up to date and seeds root, in that order.
func prepare(ctx context.Context, st *store.Store, cfg config) error {
	if cfg.migrate {
		applied, err := store.Migrate(ctx, st.Pool())
		if err != nil {
			return err
		}
		if len(applied) > 0 {
			slog.Info("migrated", "versions", applied)
		}
	}

	// A blocked month is degraded, not down: writes still land in the DEFAULT
	// partition, so this logs and carries on rather than failing every boot.
	blocked, err := store.EnsureEventPartitions(ctx, st.Pool(), 2)
	if err != nil {
		return err
	}
	if len(blocked) > 0 {
		slog.Warn("event partitions blocked; rows in the default partition are in the way",
			"months", blocked)
	}

	return bootstrapFromEnv(ctx, st)
}

func listen(srv *http.Server, role, addr string, errCh chan<- error) {
	slog.Info("listening", "role", role, "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- fmt.Errorf("%s: %w", role, err)
	}
}

// serve is listen for a listener we already hold. ListenAndServe cannot express
// a unix socket, and the socket has to be bound and chmod'ed before anything is
// served over it.
func serve(srv *http.Server, role string, ln net.Listener, errCh chan<- error) {
	slog.Info("listening", "role", role, "addr", ln.Addr().String())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

// bootstrapFromEnv seeds the root actor out of band (D19.1). No API path
// creates the first actor, so config and environment are the only ways in.
//
// HIVE_SANDBOX_BOOTSTRAP_TOKEN is how an operator gets a first credential
// without one existing to authenticate the request that would create it. It is
// idempotent, it cannot mint a second root (the schema caps that) and it cannot
// mint a second org (Bootstrap caps that).
func bootstrapFromEnv(ctx context.Context, st *store.Store) error {
	handle := strings.TrimSpace(os.Getenv("HIVE_SANDBOX_BOOTSTRAP_HANDLE"))
	if handle == "" {
		return nil
	}

	org := strings.TrimSpace(os.Getenv("HIVE_SANDBOX_BOOTSTRAP_ORG"))
	res, err := st.BootstrapInTx(ctx, store.BootstrapConfig{
		RootHandle: handle,
		RootName:   handle,
		OrgHandle:  org,
		OrgName:    org,
	})
	if err != nil {
		return err
	}
	if res.Created {
		slog.Info("bootstrapped", "root", res.RootActorID, "org", res.OrgActorID)
	}

	token := os.Getenv("HIVE_SANDBOX_BOOTSTRAP_TOKEN")
	if token == "" {
		return nil
	}
	if err := store.EnsureBootstrapCredential(ctx, st.Pool(), res.RootActorID, token); err != nil {
		return err
	}
	slog.Info("bootstrap credential present", "actor", res.RootActorID)
	return nil
}

// envOr is a flag default that an environment variable can override, so the
// same binary configures the same way from a shell and from a compose file.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
