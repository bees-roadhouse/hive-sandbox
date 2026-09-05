//! The proxy end to end over loopback. Ported from proxy_test.go.

use std::collections::HashMap;
use std::net::{IpAddr, SocketAddr};
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use hive_egress::{Allowlist, DENY_HEADER, Proxy, ProxyConfig, Resolver, parse_allowlist};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{TcpListener, TcpStream};
use tokio_util::sync::CancellationToken;

/// Points names wherever the test wants. Real DNS in a unit test would make
/// the rebinding guard untestable.
struct StubResolver(HashMap<String, Vec<IpAddr>>);

impl StubResolver {
    fn resolving(entries: &[(&str, &str)]) -> Arc<dyn Resolver> {
        Arc::new(StubResolver(
            entries
                .iter()
                .map(|(h, ip)| (h.to_string(), vec![ip.parse().unwrap()]))
                .collect(),
        ))
    }
}

#[async_trait]
impl Resolver for StubResolver {
    async fn lookup(&self, host: &str) -> Result<Vec<IpAddr>, String> {
        self.0
            .get(&host.to_ascii_lowercase())
            .cloned()
            .ok_or_else(|| format!("no such host: {host}"))
    }
}

fn must_parse(entries: &[&str]) -> Allowlist {
    parse_allowlist(entries).expect("allowlist")
}

/// Runs a proxy on a loopback port and returns its address.
async fn start_proxy(cfg: ProxyConfig) -> (SocketAddr, CancellationToken) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let cancel = CancellationToken::new();
    let proxy = Arc::new(Proxy::new(cfg));
    let c = cancel.clone();
    tokio::spawn(async move { proxy.serve(listener, c).await });
    (addr, cancel)
}

/// An origin that answers every request with a fixed status and a body naming
/// the method and path.
async fn start_origin(status: u16) -> SocketAddr {
    use axum::routing::any;
    let app = axum::Router::new().fallback(any(move |req: axum::extract::Request| async move {
        let body = format!("origin saw {} {}", req.method(), req.uri().path());
        (axum::http::StatusCode::from_u16(status).unwrap(), body)
    }));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    addr
}

fn client(proxy: SocketAddr) -> reqwest::Client {
    reqwest::Client::builder()
        .proxy(reqwest::Proxy::http(format!("http://{proxy}")).unwrap())
        .timeout(Duration::from_secs(15))
        .build()
        .unwrap()
}

fn cfg(allow: Allowlist, run_id: &str, resolver: Option<Arc<dyn Resolver>>) -> ProxyConfig {
    ProxyConfig {
        allow: Arc::new(allow),
        run_id: run_id.into(),
        resolver,
        ..ProxyConfig::default()
    }
}

/// Ported from `TestProxyForwardsAnAllowedHost`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn proxy_forwards_an_allowed_host() {
    let origin = start_origin(200).await;
    let mut allow = must_parse(&[&format!("api.example.test:{}", origin.port())]);
    // The test origin is on loopback, so the guard is relaxed here. It gets
    // its own test below.
    allow.allow_private_destinations = true;
    let (proxy, _c) = start_proxy(cfg(
        allow,
        "run-allowed",
        Some(StubResolver::resolving(&[(
            "api.example.test",
            "127.0.0.1",
        )])),
    ))
    .await;
    let res = client(proxy)
        .get(format!("http://api.example.test:{}/hello", origin.port()))
        .send()
        .await
        .expect("GET through proxy");
    assert_eq!(
        res.status(),
        200,
        "deny: {:?}",
        res.headers().get(DENY_HEADER)
    );
    assert!(res.text().await.unwrap().contains("origin saw GET /hello"));
}

/// Ported from `TestProxyDeniesAnUnlistedHost`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn proxy_denies_an_unlisted_host() {
    let origin = start_origin(418).await;
    let mut allow = must_parse(&[&format!("allowed.example.test:{}", origin.port())]);
    allow.allow_private_destinations = true;
    let (proxy, _c) = start_proxy(cfg(
        allow,
        "run-denied",
        Some(StubResolver::resolving(&[
            ("allowed.example.test", "127.0.0.1"),
            ("exfiltrate.example.test", "127.0.0.1"),
        ])),
    ))
    .await;
    let res = client(proxy)
        .get(format!(
            "http://exfiltrate.example.test:{}/steal",
            origin.port()
        ))
        .send()
        .await
        .unwrap();
    assert_eq!(res.status(), 403);
    let reason = res
        .headers()
        .get(DENY_HEADER)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    assert!(reason.contains("allowlist"), "deny reason = {reason:?}");
}

/// Speaks CONNECT by hand, so nothing but the proxy's own behaviour is
/// measured. Returns the status line and the tunnel.
async fn connect(proxy: SocketAddr, target: &str) -> (String, BufReader<TcpStream>) {
    let conn = TcpStream::connect(proxy).await.unwrap();
    let mut reader = BufReader::new(conn);
    reader
        .get_mut()
        .write_all(format!("CONNECT {target} HTTP/1.1\r\nHost: {target}\r\n\r\n").as_bytes())
        .await
        .unwrap();
    let mut status = String::new();
    reader.read_line(&mut status).await.unwrap();
    // Drain the headers. Without this the blank line terminating the response
    // is still buffered and reads exactly like a tunnel that carried data.
    loop {
        let mut line = String::new();
        reader.read_line(&mut line).await.unwrap();
        if line.trim().is_empty() {
            break;
        }
    }
    (status, reader)
}

/// Ported from `TestProxyTunnelsAllowedConnect`: CONNECT is the path that
/// matters; the destination host is all the proxy can see. The tunnel here
/// carries plain HTTP, because TLS inside it is the client's business and the
/// bytes are opaque to the proxy either way.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn proxy_tunnels_allowed_connect() {
    let origin = start_origin(200).await;
    let mut allow = must_parse(&[&format!("api.example.test:{}", origin.port())]);
    allow.allow_private_destinations = true;
    let (proxy, _c) = start_proxy(cfg(
        allow,
        "run-connect",
        Some(StubResolver::resolving(&[(
            "api.example.test",
            "127.0.0.1",
        )])),
    ))
    .await;
    let target = format!("api.example.test:{}", origin.port());
    let (status, mut tunnel) = connect(proxy, &target).await;
    assert!(status.contains("200"), "CONNECT status = {status:?}");
    tunnel
        .get_mut()
        .write_all(b"GET /secure HTTP/1.1\r\nHost: api.example.test\r\nConnection: close\r\n\r\n")
        .await
        .unwrap();
    let mut body = String::new();
    tunnel.read_to_string(&mut body).await.unwrap();
    assert!(
        body.contains("origin saw GET /secure"),
        "tunnelled response = {body:?}"
    );
}

/// Ported from `TestProxyDeniesUnlistedConnect`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn proxy_denies_unlisted_connect() {
    let mut allow = must_parse(&["allowed.example.test"]);
    allow.allow_private_destinations = true;
    let (proxy, _c) = start_proxy(cfg(
        allow,
        "run-connect-denied",
        Some(StubResolver::resolving(&[(
            "blocked.example.test",
            "127.0.0.1",
        )])),
    ))
    .await;
    let (status, _) = connect(proxy, "blocked.example.test:443").await;
    assert!(
        status.contains("403"),
        "CONNECT status = {status:?}, want a 403 from the proxy"
    );
}

/// Ported from `TestProxyRefusesAllowlistedNameResolvingToLoopback`: the
/// rebinding case, end to end through the real dial path.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn proxy_refuses_allowlisted_name_resolving_to_loopback() {
    let origin = start_origin(200).await;
    // allow_private_destinations stays false. That is the control.
    let allow = must_parse(&[&format!("rebind.example.test:{}", origin.port())]);
    let (proxy, _c) = start_proxy(cfg(
        allow,
        "run-rebind",
        Some(StubResolver::resolving(&[(
            "rebind.example.test",
            "127.0.0.1",
        )])),
    ))
    .await;
    let res = client(proxy)
        .get(format!("http://rebind.example.test:{}/", origin.port()))
        .send()
        .await
        .unwrap();
    assert_eq!(
        res.status(),
        403,
        "the guard let an allowlisted name reach loopback"
    );
    let reason = res
        .headers()
        .get(DENY_HEADER)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    assert!(
        reason.contains("not a public address"),
        "deny reason = {reason:?}"
    );
}

/// Ported from `TestProxyDistinguishesDenialFromUnreachable`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn proxy_distinguishes_denial_from_unreachable() {
    let allow = must_parse(&["unreachable.example.test:80"]);
    let (proxy, _c) = start_proxy(ProxyConfig {
        dial_timeout: Duration::from_secs(2),
        // RFC 5737 documentation range: routable-looking, never answers.
        ..cfg(
            allow,
            "run-unreachable",
            Some(StubResolver::resolving(&[(
                "unreachable.example.test",
                "203.0.113.9",
            )])),
        )
    })
    .await;
    let res = client(proxy)
        .get("http://unreachable.example.test/")
        .send()
        .await
        .unwrap();
    assert_eq!(
        res.status(),
        502,
        "the allowlist permitted this host, the network refused it"
    );
}

/// Ported from `TestProxyRefusesCloudMetadataAddress`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn proxy_refuses_cloud_metadata_address() {
    let allow = must_parse(&["metadata.example.test"]);
    let (proxy, _c) = start_proxy(ProxyConfig {
        dial_timeout: Duration::from_secs(2),
        ..cfg(
            allow,
            "run-metadata",
            Some(StubResolver::resolving(&[(
                "metadata.example.test",
                "169.254.169.254",
            )])),
        )
    })
    .await;
    let res = client(proxy)
        .get("http://metadata.example.test/latest/meta-data/")
        .send()
        .await
        .unwrap();
    assert_eq!(res.status(), 403, "want 403 for a link-local destination");
}

/// Ported from `TestProxyWithNoAllowlistDeniesEverything`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn proxy_with_no_allowlist_denies_everything() {
    let (proxy, _c) = start_proxy(ProxyConfig {
        run_id: "run-empty".into(),
        ..ProxyConfig::default()
    })
    .await;
    let res = client(proxy)
        .get("http://api.anthropic.com/v1/messages")
        .send()
        .await
        .unwrap();
    assert_eq!(res.status(), 403, "want 403 from a proxy with no allowlist");
}

/// Ported from `TestProxyRefusesOriginFormRequests`: answering one would make
/// the proxy a confused deputy.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn proxy_refuses_origin_form_requests() {
    let (proxy, _c) = start_proxy(cfg(
        must_parse(&["api.example.test"]),
        "run-origin-form",
        None,
    ))
    .await;
    // A direct request, not one routed through the client's proxy setting.
    let res = reqwest::Client::new()
        .get(format!("http://{proxy}/healthz"))
        .send()
        .await
        .unwrap();
    assert_eq!(res.status(), 403);
}

/// Ported from `TestIdleTunnelIsClosed`: a tunnel neither side ever closes
/// was bounded only by the harness deadline killing the container.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn idle_tunnel_is_closed() {
    // An origin that accepts the tunnel and then says nothing at all.
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    let (accepted_tx, mut accepted_rx) = tokio::sync::mpsc::channel::<TcpStream>(1);
    tokio::spawn(async move {
        if let Ok((conn, _)) = listener.accept().await {
            let _ = accepted_tx.send(conn).await;
            std::future::pending::<()>().await;
        }
    });
    let mut allow = must_parse(&[&format!("silent.example.test:{port}")]);
    allow.allow_private_destinations = true;
    let (proxy, _c) = start_proxy(ProxyConfig {
        tunnel_idle_timeout: Duration::from_millis(500),
        ..cfg(
            allow,
            "run-idle-tunnel",
            Some(StubResolver::resolving(&[(
                "silent.example.test",
                "127.0.0.1",
            )])),
        )
    })
    .await;
    let (status, mut tunnel) = connect(proxy, &format!("silent.example.test:{port}")).await;
    assert!(status.contains("200"), "CONNECT status = {status:?}");
    let _held = tokio::time::timeout(Duration::from_secs(5), accepted_rx.recv())
        .await
        .expect("the proxy never dialled the origin");

    // Neither side sends anything. The idle bound has to be what ends it.
    let start = tokio::time::Instant::now();
    let mut buf = [0u8; 1];
    let read = tokio::time::timeout(Duration::from_secs(10), tunnel.read(&mut buf)).await;
    let elapsed = start.elapsed();
    match read {
        Err(_) => panic!("the tunnel was still open after {elapsed:?}; the idle bound never fired"),
        Ok(Ok(0)) | Ok(Err(_)) => {}
        Ok(Ok(_)) => panic!("read returned data from a silent tunnel"),
    }
    assert!(
        elapsed < Duration::from_secs(5),
        "tunnel closed after {elapsed:?}, want roughly the 500ms idle bound"
    );
}
