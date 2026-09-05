//! hive-sandbox is the platform daemon: it hosts WASM guest apps, serves their
//! dynamic REST and MCP surfaces, runs workflows, and drives agent harnesses.
//!
//! One process serves every role by default (D7). The role flags exist from
//! day one so a heavy agent run can be split off from interactive traffic
//! later without a code change.

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, anyhow, bail};
use clap::Parser;
use hive_blob::Catalog;
use hive_bus::Bus;
use hive_chat::{Hub, Worker};
use hive_harness::{ImagePins, PinError, PodmanLauncher, Supervisor};
use hive_httpapi::Options;
use hive_sandbox::{BlobConfig, blob_driver, unix_listener};
use hive_store::{AppData, BootstrapConfig, Chat, GuestBlobs, GuestEvents, Store};
use hive_wasmhost::{Deps, Host};
use tokio_util::sync::CancellationToken;

/// Set at build time by the image builds (`HIVE_SANDBOX_VERSION`); falls back
/// to the crate version for a plain `cargo build`.
const VERSION: &str = match option_env!("HIVE_SANDBOX_VERSION") {
    Some(v) => v,
    None => env!("CARGO_PKG_VERSION"),
};

fn parse_duration(s: &str) -> Result<Duration, String> {
    humantime::parse_duration(s).map_err(|e| e.to_string())
}

/// A repeatable flag that also takes comma-separated values, because the
/// supervisor passes the allowlist as one environment variable rather than N
/// container arguments.
fn split_list(values: &[String]) -> Vec<String> {
    values
        .iter()
        .flat_map(|v| v.split(','))
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(String::from)
        .collect()
}

#[derive(Parser, Debug)]
#[command(name = "hive-sandbox", version = VERSION, about = "The hive-sandbox platform daemon")]
struct Args {
    /// Listen address for the HTTP surface.
    #[arg(long, default_value = ":7979")]
    addr: String,
    /// Also serve the API on this unix socket path.
    #[arg(long, env = "HIVE_SANDBOX_UNIX_SOCKET")]
    unix_socket: Option<String>,
    /// Serve REST, MCP and SSE.
    #[arg(long, default_value_t = true, action = clap::ArgAction::Set)]
    serve_api: bool,
    /// Claim and execute workflow steps.
    #[arg(long, default_value_t = true, action = clap::ArgAction::Set)]
    run_workflows: bool,
    /// Postgres connection string.
    #[arg(long, env = "HIVE_SANDBOX_DATABASE_URL")]
    database_url: Option<String>,
    /// Apply pending migrations at boot.
    #[arg(long, default_value_t = true, action = clap::ArgAction::Set)]
    migrate: bool,
    /// Blob backend: disk or s3.
    #[arg(long, env = "HIVE_SANDBOX_BLOB_DRIVER", default_value = "disk")]
    blob_driver: String,
    /// Blob root for the disk driver.
    #[arg(
        long,
        env = "HIVE_SANDBOX_BLOB_ROOT",
        default_value = "/var/lib/hive/blobs"
    )]
    blob_root: String,
    /// This deployment serves plain HTTP: session cookies are sent without
    /// Secure. Off by default, on purpose (D26).
    #[arg(long, env = "HIVE_SANDBOX_PLAIN_HTTP", default_value_t = false, num_args = 0..=1, default_missing_value = "true", action = clap::ArgAction::Set)]
    plain_http: bool,
    /// Answer chat turns with hosted agent runs.
    #[arg(long, default_value_t = true, action = clap::ArgAction::Set)]
    run_chat: bool,
    /// The image lockfile scripts/harness-build.sh writes.
    #[arg(long, env = "HIVE_SANDBOX_HARNESS_PINS", default_value = hive_harness::DEFAULT_PINS_PATH)]
    harness_pins: String,
    /// The egress proxy image pin scripts/egress-build.sh writes.
    #[arg(long, env = "HIVE_SANDBOX_EGRESS_PIN", default_value = hive_harness::DEFAULT_EGRESS_PIN_PATH)]
    egress_pin: String,
    /// Podman binary a harness run is launched with.
    #[arg(long, env = "HIVE_SANDBOX_PODMAN", default_value = "podman")]
    podman: String,
    /// Directory holding one workspace per conversation.
    #[arg(
        long,
        env = "HIVE_SANDBOX_CHAT_WORKSPACES",
        default_value = "/var/lib/hive/workspaces"
    )]
    chat_workspaces: PathBuf,
    /// How many chat turns run at once.
    #[arg(long, default_value_t = 2)]
    chat_concurrency: usize,
    /// Wall clock one chat turn gets.
    #[arg(long, default_value = "10m", value_parser = parse_duration)]
    chat_deadline: Duration,
    /// Persist wasmtime's compilation cache here. Unset keeps it in memory.
    #[arg(long, env = "HIVE_SANDBOX_WASM_CACHE")]
    wasm_cache: Option<PathBuf>,
    /// Run the allowlisting egress proxy for a harness run (D12.6).
    #[arg(long, default_value_t = false, num_args = 0..=1, default_missing_value = "true", action = clap::ArgAction::Set)]
    run_egress_proxy: bool,
    /// Listen address for the egress proxy.
    #[arg(long, default_value = ":3128")]
    egress_addr: String,
    /// host[:port] a run may reach; repeatable, or comma-separated. Absence is
    /// deny. HIVE_SANDBOX_EGRESS_ALLOW adds to it.
    #[arg(long)]
    egress_allow: Vec<String>,
    /// Resolver to query directly, bypassing resolv.conf; repeatable or
    /// comma-separated.
    #[arg(long)]
    egress_dns: Vec<String>,
    /// Permit RFC1918, loopback and link-local destinations. Off by default:
    /// this is the SSRF and DNS-rebinding control.
    #[arg(long, default_value_t = false, num_args = 0..=1, default_missing_value = "true", action = clap::ArgAction::Set)]
    egress_allow_private: bool,
}

impl Args {
    /// Whether the enabled roles read or write platform state.
    ///
    /// The egress proxy deliberately does not: it runs inside a harness
    /// container beside the run it is fencing, with no reason to reach
    /// Postgres and no credentials to reach it with. Requiring a connection
    /// string there would make every run depend on the database being up in
    /// order to be DENIED network access, which is backwards.
    fn needs_database(&self) -> bool {
        self.serve_api || self.run_workflows || self.run_chat
    }
}

fn main() {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("tokio runtime");
    if let Err(e) = rt.block_on(run()) {
        tracing::error!(err = %e, "fatal");
        eprintln!("hive-sandbox: {e:#}");
        std::process::exit(1);
    }
}

/// `:7979` means every interface, the way Go's net.Listen read it.
fn listen_addr(addr: &str) -> String {
    match addr.strip_prefix(':') {
        Some(port) => format!("0.0.0.0:{port}"),
        None => addr.to_string(),
    }
}

async fn run() -> anyhow::Result<()> {
    let mut args = Args::parse();
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::from_default_env()
                .add_directive(tracing::Level::INFO.into()),
        )
        .with_target(false)
        // Colour only on a terminal. In a container the log is read by a
        // program (the harness greps the proxy's log for its readiness line),
        // and escape codes between `role`, `=` and the value would make that
        // line unmatchable while looking fine to a person.
        .with_ansi(std::io::IsTerminal::is_terminal(&std::io::stdout()))
        .init();

    // The supervisor injects the allowlist as one variable rather than N
    // container arguments.
    if let Ok(from_env) = std::env::var("HIVE_SANDBOX_EGRESS_ALLOW") {
        args.egress_allow.push(from_env);
    }
    let egress_allow = split_list(&args.egress_allow);
    let egress_dns = split_list(&args.egress_dns);

    if !args.serve_api && !args.run_workflows && !args.run_chat && !args.run_egress_proxy {
        bail!(
            "no role enabled: pass --serve-api, --run-workflows, --run-chat, --run-egress-proxy, or a combination"
        );
    }
    if args.needs_database() && args.database_url.as_deref().unwrap_or("").is_empty() {
        bail!("no database: pass --database-url or set HIVE_SANDBOX_DATABASE_URL");
    }

    tracing::info!(
        version = VERSION,
        serve_api = args.serve_api,
        run_workflows = args.run_workflows,
        run_chat = args.run_chat,
        run_egress_proxy = args.run_egress_proxy,
        "starting"
    );
    if args.plain_http && args.serve_api {
        // At every boot, not once: the flag outlives the person who set it.
        tracing::warn!(
            "plain HTTP: session cookies are not Secure and cross the network in the clear; anything on the path can read a credential. Put TLS in front and drop --plain-http."
        );
    }

    let cancel = CancellationToken::new();
    let mut tasks: Vec<tokio::task::JoinHandle<()>> = Vec::new();

    let mut store: Option<Store> = None;
    let mut bus: Option<Bus> = None;
    let mut catalog: Option<Arc<Catalog>> = None;
    let mut chat: Option<Arc<Chat>> = None;
    let mut host: Option<Host> = None;
    let hub = Hub::default();
    let mut wake: Option<Arc<dyn Fn() + Send + Sync>> = None;

    if args.needs_database() {
        let st = Store::open(args.database_url.as_deref().unwrap_or(""))
            .await
            .context("open store")?;
        prepare(&st, &args).await?;

        // The guest data layer and the runtime that calls it. Built here
        // rather than lazily because a daemon that comes up and only discovers
        // at first guest call that it has no blob backend has already told an
        // orchestrator it was ready.
        let driver = blob_driver(&BlobConfig {
            driver: args.blob_driver.clone(),
            root: args.blob_root.clone(),
        })
        .await
        .context("blob driver")?;
        let driver_name = driver.name();
        let cat = Arc::new(Catalog::new(st.pool().clone(), driver));
        let deps = Deps {
            storage: Arc::new(AppData::new(st.clone(), cat.clone())),
            blob: Arc::new(GuestBlobs::new(st.clone(), cat.clone())),
            events: Arc::new(GuestEvents::new(st.clone())),
            // KV and Sanitizer stay stubbed: both are unbuilt, and the stub
            // answers Unimplemented rather than crashing.
            ..Deps::default()
        };
        let h = Host::new(
            hive_wasmhost::Config {
                cache_dir: args.wasm_cache.clone(),
                ..Default::default()
            },
            deps,
        )
        .await
        .map_err(|e| anyhow!("wasm host: {e}"))?;
        tracing::info!(blob_driver = driver_name, "wasm host ready");
        host = Some(h);

        let b = Bus::new(st.pool().clone(), hive_bus::Config::default());
        {
            let run = b.clone();
            let c = cancel.clone();
            tasks.push(tokio::spawn(async move { run.run(c).await }));
        }
        bus = Some(b);

        let chat_layer = Arc::new(Chat::new(st.clone()));
        if args.run_chat
            && let Some(worker) = chat_worker(&args, &st, chat_layer.clone(), hub.clone())?
        {
            let w = worker.clone();
            wake = Some(Arc::new(move || w.kick()));
            let c = cancel.clone();
            tasks.push(tokio::spawn(async move { worker.run(c).await }));
        }
        chat = Some(chat_layer);
        catalog = Some(cat);
        store = Some(st);
    }

    // Every listener reports into one channel; the first error ends the
    // process. Buffered for every listener that can report so a send from a
    // listener nobody is reading cannot hang the shutdown path.
    let (err_tx, mut err_rx) = tokio::sync::mpsc::channel::<anyhow::Error>(4);
    let mut listeners = 0;

    if args.run_egress_proxy {
        let proxy = egress_proxy(&args, &egress_allow, &egress_dns)?;
        let addr = listen_addr(&args.egress_addr);
        let listener = tokio::net::TcpListener::bind(&addr)
            .await
            .with_context(|| format!("egress-proxy: listen on {addr}"))?;
        // Display, not Debug: the harness reads `role=egress-proxy` off this
        // container's log as its readiness signal, and a quoted field is not
        // that line.
        tracing::info!(role = %"egress-proxy", addr = %addr, "listening");
        let c = cancel.clone();
        tasks.push(tokio::spawn(async move { proxy.serve(listener, c).await }));
        listeners += 1;
    }

    let mut socket_guard = None;
    if args.serve_api {
        let app = hive_httpapi::router(
            store.clone(),
            bus.clone(),
            Options {
                version: VERSION.to_string(),
                blobs: catalog.clone(),
                chat: chat.clone(),
                hub: Some(hub.clone()),
                wake: wake.clone(),
                plain_http: args.plain_http,
            },
        )
        // The browser client, at the root. Two patterns that no API route
        // shares, so a file can never shadow an endpoint.
        .merge(hive_webui::router());

        let addr = listen_addr(&args.addr);
        let listener = tokio::net::TcpListener::bind(&addr)
            .await
            .with_context(|| format!("api: listen on {addr}"))?;
        tracing::info!(role = %"api", addr = %addr, "listening");
        {
            let app = app.clone();
            let c = cancel.clone();
            let tx = err_tx.clone();
            tasks.push(tokio::spawn(async move {
                if let Err(e) = axum::serve(listener, app)
                    .with_graceful_shutdown(async move { c.cancelled().await })
                    .await
                {
                    let _ = tx.send(anyhow!("api: {e}")).await;
                }
            }));
        }
        listeners += 1;

        // The SAME router on a second listener, so one shutdown drains both
        // and the socket cannot outlive the port it is meant to mirror.
        //
        // Invariant 13: a harness container runs --network=none with this file
        // bind-mounted, because on rootless Podman an --internal network has
        // no gateway and cannot reach the host at all. Without this the
        // harness has no route to the API and the failure looks like a bug
        // inside the run.
        if let Some(path) = args.unix_socket.as_deref().filter(|p| !p.is_empty()) {
            let mut sock = unix_listener(path).await?;
            let listener = sock.take().expect("fresh socket");
            tracing::info!(role = %"api-unix", addr = %path, "listening");
            let c = cancel.clone();
            let tx = err_tx.clone();
            tasks.push(tokio::spawn(async move {
                if let Err(e) = axum::serve(listener, app)
                    .with_graceful_shutdown(async move { c.cancelled().await })
                    .await
                {
                    let _ = tx.send(anyhow!("api-unix: {e}")).await;
                }
            }));
            socket_guard = Some(sock);
            listeners += 1;
        }
    }
    drop(err_tx);

    let mut outcome = Ok(());
    if listeners == 0 {
        // A workflow-only process has no listener yet; the runner lands in
        // a workflow crate and will block here instead.
        shutdown_signal().await;
    } else {
        tokio::select! {
            Some(e) = err_rx.recv() => outcome = Err(e),
            _ = shutdown_signal() => {}
        }
    }

    tracing::info!("shutting down");
    cancel.cancel();
    for t in tasks {
        let _ = tokio::time::timeout(Duration::from_secs(15), t).await;
    }
    if let Some(h) = host {
        h.close().await;
    }
    drop(socket_guard);
    if let Some(st) = store {
        st.close().await;
    }
    outcome
}

async fn shutdown_signal() {
    let ctrl_c = tokio::signal::ctrl_c();
    let mut term = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
        .expect("SIGTERM handler");
    tokio::select! {
        _ = ctrl_c => {}
        _ = term.recv() => {}
    }
}

/// Brings the schema up to date and seeds root, in that order.
async fn prepare(st: &Store, args: &Args) -> anyhow::Result<()> {
    if args.migrate {
        let applied = hive_store::migrate(st.pool()).await.context("migrate")?;
        if !applied.is_empty() {
            tracing::info!(versions = ?applied, "migrated");
        }
    }
    // A blocked month is degraded, not down: writes still land in the DEFAULT
    // partition, so this logs and carries on rather than failing every boot.
    let mut conn = st.conn().await?;
    let blocked = hive_store::ensure_event_partitions(&mut conn, 2).await?;
    drop(conn);
    if !blocked.is_empty() {
        tracing::warn!(months = ?blocked, "event partitions blocked; rows in the default partition are in the way");
    }
    bootstrap_from_env(st).await
}

/// Seeds the root actor out of band (D19.1). No API path creates the first
/// actor, so config and environment are the only ways in.
///
/// HIVE_SANDBOX_BOOTSTRAP_TOKEN is how an operator gets a first credential
/// without one existing to authenticate the request that would create it. It
/// is idempotent, it cannot mint a second root (the schema caps that) and it
/// cannot mint a second org (bootstrap caps that).
async fn bootstrap_from_env(st: &Store) -> anyhow::Result<()> {
    let handle = std::env::var("HIVE_SANDBOX_BOOTSTRAP_HANDLE")
        .unwrap_or_default()
        .trim()
        .to_string();
    if handle.is_empty() {
        return Ok(());
    }
    let org = std::env::var("HIVE_SANDBOX_BOOTSTRAP_ORG")
        .unwrap_or_default()
        .trim()
        .to_string();
    let res = st
        .bootstrap_in_tx(&BootstrapConfig {
            root_handle: handle.clone(),
            root_name: handle,
            org_handle: org.clone(),
            org_name: org,
        })
        .await
        .context("bootstrap")?;
    if res.created {
        tracing::info!(root = %res.root_actor_id, org = ?res.org_actor_id, "bootstrapped");
    }
    let token = std::env::var("HIVE_SANDBOX_BOOTSTRAP_TOKEN").unwrap_or_default();
    if token.is_empty() {
        return Ok(());
    }
    let mut conn = st.conn().await?;
    hive_store::ensure_bootstrap_credential(&mut conn, res.root_actor_id, &token)
        .await
        .context("bootstrap credential")?;
    tracing::info!(actor = %res.root_actor_id, "bootstrap credential present");
    Ok(())
}

fn hostname() -> String {
    std::fs::read_to_string("/proc/sys/kernel/hostname")
        .ok()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .or_else(|| std::env::var("HOSTNAME").ok())
        .unwrap_or_else(|| "daemon".into())
}

/// Builds the turn worker, or `None` when there is nothing to run turns on.
///
/// No pins file is a warning rather than a boot failure: a development
/// daemon, the end-to-end suite and a fresh install all come up before anyone
/// has built a harness image, and a chat that queues turns nothing answers is
/// visible in the thread ("waiting for an agent") while a daemon that refuses
/// to start is visible nowhere. A pins file that exists and cannot be read IS
/// a failure.
fn chat_worker(
    args: &Args,
    st: &Store,
    chat: Arc<Chat>,
    hub: Hub,
) -> anyhow::Result<Option<Arc<Worker>>> {
    let pins = match ImagePins::load(&args.harness_pins) {
        Ok(p) => p,
        Err(PinError::NotFound(path)) => {
            tracing::warn!(path = %path, "chat worker disabled: no harness image pins; run scripts/harness-build.sh");
            return Ok(None);
        }
        Err(e) => return Err(e.into()),
    };
    // A turn reaches the daemon over the socket and nothing else (invariant
    // 13), so a daemon that answers turns has to have one.
    let socket = args
        .unix_socket
        .clone()
        .filter(|p| !p.is_empty())
        .ok_or_else(|| {
            anyhow!("--run-chat needs --unix-socket: a harness run reaches the API only through it")
        })?;
    // The egress image is optional here: a run with no allowlist gets no
    // proxy, and the launcher refuses a proxied run without an image.
    let egress_image = match hive_harness::EgressPin::load(&args.egress_pin) {
        Ok(p) => p.reference(),
        Err(PinError::NotFound(_)) => String::new(),
        Err(e) => return Err(e.into()),
    };
    let launcher = PodmanLauncher {
        binary: args.podman.clone(),
        egress_image,
        ..Default::default()
    };
    let sup = Arc::new(Supervisor::new(Arc::new(launcher)));
    let worker = Worker::new(
        st.clone(),
        chat,
        sup,
        Some(hub),
        hive_chat::Config {
            name: format!("{}/{}", hostname(), std::process::id()),
            pins,
            daemon_socket: socket,
            workspace_root: args.chat_workspaces.clone(),
            deadline: args.chat_deadline,
            concurrency: args.chat_concurrency,
            poll_interval: Duration::ZERO,
        },
    )?;
    tracing::info!(pins = %args.harness_pins, workspaces = %args.chat_workspaces.display(), concurrency = args.chat_concurrency, "chat worker ready");
    Ok(Some(Arc::new(worker)))
}

/// Builds the proxy from flags.
///
/// An empty allowlist is permitted and denies everything. That is on purpose:
/// a run that declared no egress should get a proxy that refuses, not a proxy
/// that fails to start and takes the run down with an error that looks like a
/// bug.
fn egress_proxy(
    args: &Args,
    allow: &[String],
    dns: &[String],
) -> anyhow::Result<Arc<hive_egress::Proxy>> {
    let mut allowlist =
        hive_egress::parse_allowlist(allow).map_err(|e| anyhow!("--egress-allow: {e}"))?;
    allowlist.allow_private_destinations = args.egress_allow_private;
    let dns = hive_egress::normalize_dns_servers(dns);
    tracing::info!(
        rules = %allowlist.rules().iter().map(|r| r.to_string()).collect::<Vec<_>>().join(" "),
        count = allowlist.rules().len(),
        allow_private = args.egress_allow_private,
        dns = %dns.join(" "),
        "egress allowlist"
    );
    let resolver: Option<Arc<dyn hive_egress::Resolver>> = if dns.is_empty() {
        None
    } else {
        Some(Arc::new(
            hive_egress::DnsResolver::new(&dns, Duration::from_secs(5))
                .map_err(|e| anyhow!("--egress-dns: {e}"))?,
        ))
    };
    Ok(Arc::new(hive_egress::Proxy::new(
        hive_egress::ProxyConfig {
            allow: Arc::new(allowlist),
            run_id: std::env::var("HIVE_SANDBOX_RUN_ID").unwrap_or_default(),
            resolver,
            ..Default::default()
        },
    )))
}
