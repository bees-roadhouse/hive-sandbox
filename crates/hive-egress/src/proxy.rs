//! The forward proxy: absolute-form requests for http, CONNECT for https.
//! There is no TLS interception ... for CONNECT the destination host is all
//! that is knowable, and that is what the allowlist is expressed in.

use std::net::{IpAddr, SocketAddr};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use bytes::Bytes;
use http::{HeaderValue, Method, Request, Response, StatusCode, Uri, header};
use http_body_util::{BodyExt, Empty, Full, combinators::BoxBody};
use hyper::body::Incoming;
use hyper_util::rt::TokioIo;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio_util::sync::CancellationToken;

use crate::allowlist::{Allowlist, Denied, Rule};
use crate::resolve::{Resolver, SystemResolver};

/// Names the reason on a refused request, so a run's own logs say why rather
/// than only showing a 403.
pub const DENY_HEADER: &str = "x-hive-sandbox-deny";

pub const DEFAULT_DIAL_TIMEOUT: Duration = Duration::from_secs(10);
/// Closes a CONNECT tunnel that has carried no bytes in either direction for
/// this long. Generous, because an agent holding a streaming response open is
/// normal; what it stops is the unbounded case, a tunnel neither side ever
/// closes.
pub const DEFAULT_TUNNEL_IDLE_TIMEOUT: Duration = Duration::from_secs(600);
/// Bounds a refusal to a header-sized line.
const MAX_REASON_BYTES: usize = 200;

type Body = BoxBody<Bytes, hyper::Error>;

#[derive(Clone)]
pub struct ProxyConfig {
    /// Decides what may be reached. The default denies everything.
    pub allow: Arc<Allowlist>,
    /// Attached to every decision log line.
    pub run_id: String,
    /// Defaults to the system resolver.
    pub resolver: Option<Arc<dyn Resolver>>,
    pub dial_timeout: Duration,
    pub tunnel_idle_timeout: Duration,
}

impl Default for ProxyConfig {
    fn default() -> Self {
        ProxyConfig {
            allow: Arc::new(Allowlist::default()),
            run_id: String::new(),
            resolver: None,
            dial_timeout: DEFAULT_DIAL_TIMEOUT,
            tunnel_idle_timeout: DEFAULT_TUNNEL_IDLE_TIMEOUT,
        }
    }
}

/// An allowlisting forward proxy. The default configuration denies
/// everything: a proxy that comes up misconfigured must be useless rather than
/// open.
pub struct Proxy {
    cfg: ProxyConfig,
    resolver: Arc<dyn Resolver>,
}

impl Proxy {
    pub fn new(cfg: ProxyConfig) -> Proxy {
        let resolver = cfg
            .resolver
            .clone()
            .unwrap_or_else(|| Arc::new(SystemResolver));
        Proxy { cfg, resolver }
    }

    /// Accepts connections until `cancel` fires.
    pub async fn serve(self: Arc<Self>, listener: TcpListener, cancel: CancellationToken) {
        loop {
            let accepted = tokio::select! {
                _ = cancel.cancelled() => return,
                a = listener.accept() => a,
            };
            let (stream, _) = match accepted {
                Ok(a) => a,
                Err(e) => {
                    tracing::warn!(err = %e, "egress accept");
                    tokio::time::sleep(Duration::from_millis(50)).await;
                    continue;
                }
            };
            let proxy = self.clone();
            tokio::spawn(async move {
                let io = TokioIo::new(stream);
                let svc = hyper::service::service_fn(move |req| {
                    let proxy = proxy.clone();
                    async move { Ok::<_, hyper::Error>(proxy.handle(req).await) }
                });
                let conn = hyper::server::conn::http1::Builder::new()
                    .preserve_header_case(true)
                    .title_case_headers(true)
                    .serve_connection(io, svc)
                    .with_upgrades();
                if let Err(e) = conn.await {
                    tracing::debug!(err = %e, "egress connection");
                }
            });
        }
    }

    /// One request.
    pub async fn handle(self: Arc<Self>, req: Request<Incoming>) -> Response<Body> {
        if req.method() == Method::CONNECT {
            self.handle_connect(req).await
        } else {
            self.handle_forward(req).await
        }
    }

    async fn handle_connect(self: Arc<Self>, req: Request<Incoming>) -> Response<Body> {
        let target = req
            .uri()
            .authority()
            .map(|a| a.to_string())
            .or_else(|| {
                req.headers()
                    .get(header::HOST)
                    .and_then(|h| h.to_str().ok())
                    .map(String::from)
            })
            .unwrap_or_default();
        let Ok((host, port)) = split_host_port(&target, 443) else {
            return self.deny("CONNECT", &target, 0, "malformed CONNECT target");
        };
        let Some(rule) = self.cfg.allow.match_rule(&host, port).cloned() else {
            return self.deny("CONNECT", &host, port, "host not on the run's allowlist");
        };
        // Dial before answering: once the 200 is on the wire there is no
        // response left to report a failure through.
        let upstream = match self.dial(&rule, &host, port).await {
            Ok(u) => u,
            Err(e) => return self.reject("CONNECT", &host, port, e),
        };
        tracing::info!(run_id = %self.cfg.run_id, method = "CONNECT", host, port, "egress allowed");
        let idle = self.cfg.tunnel_idle_timeout;
        tokio::spawn(async move {
            match hyper::upgrade::on(req).await {
                Ok(upgraded) => tunnel(TokioIo::new(upgraded), upstream, idle).await,
                Err(e) => tracing::debug!(err = %e, "egress upgrade"),
            }
        });
        Response::builder()
            .status(StatusCode::OK)
            .body(empty())
            .expect("static response")
    }

    async fn handle_forward(self: Arc<Self>, req: Request<Incoming>) -> Response<Body> {
        let uri = req.uri().clone();
        let method = req.method().to_string();
        let Some(scheme) = uri.scheme_str() else {
            // A relative URI means something addressed the proxy as an origin
            // server. Nothing legitimate does that here.
            let host = req
                .headers()
                .get(header::HOST)
                .and_then(|h| h.to_str().ok())
                .unwrap_or("")
                .to_string();
            return self.deny(
                &method,
                &host,
                0,
                "not a proxy request: expected an absolute URI",
            );
        };
        if scheme != "http" {
            return self.deny(
                &method,
                uri.host().unwrap_or(""),
                0,
                &format!("scheme {scheme} must use CONNECT"),
            );
        }
        let authority = uri.authority().map(|a| a.to_string()).unwrap_or_default();
        let Ok((host, port)) = split_host_port(&authority, 80) else {
            return self.deny(&method, &authority, 0, "malformed target");
        };
        let Some(rule) = self.cfg.allow.match_rule(&host, port).cloned() else {
            return self.deny(&method, &host, port, "host not on the run's allowlist");
        };

        // Every dial goes through the guard, and there is no shared connection
        // pool: a later request under a stricter rule must never reuse a
        // connection opened under a looser one (invariant 14).
        let upstream = match self.dial(&rule, &host, port).await {
            Ok(u) => u,
            Err(e) => return self.reject(&method, &host, port, e),
        };
        let (mut sender, conn) =
            match hyper::client::conn::http1::handshake(TokioIo::new(upstream)).await {
                Ok(x) => x,
                Err(e) => return self.fail_upstream(&method, &host, port, &e.to_string()),
            };
        tokio::spawn(async move {
            let _ = conn.await;
        });

        // Origin-form to the origin, with the hop-by-hop headers dropped: they
        // are ours, not the origin's.
        let (mut parts, body) = req.into_parts();
        let path = uri.path_and_query().map(|p| p.as_str()).unwrap_or("/");
        parts.uri = path
            .parse::<Uri>()
            .unwrap_or_else(|_| Uri::from_static("/"));
        for h in [
            "proxy-connection",
            "proxy-authorization",
            "connection",
            "keep-alive",
            "transfer-encoding",
            "te",
            "trailer",
            "upgrade",
        ] {
            parts.headers.remove(h);
        }
        if !parts.headers.contains_key(header::HOST)
            && let Ok(v) = HeaderValue::from_str(&authority)
        {
            parts.headers.insert(header::HOST, v);
        }
        let outbound = Request::from_parts(parts, body);
        let resp = match sender.send_request(outbound).await {
            Ok(r) => r,
            Err(e) => return self.fail_upstream(&method, &host, port, &e.to_string()),
        };
        tracing::info!(run_id = %self.cfg.run_id, method, host, port, status = resp.status().as_u16(), "egress allowed");
        resp.map(|b| b.boxed())
    }

    /// Resolves a host and connects to an address the allowlist permits.
    ///
    /// Resolution happens once here and the connection is made to the resolved
    /// IP, never by handing the name back to the stack. Checking a name and
    /// then dialling it again leaves a window where the second lookup returns
    /// something else, which is exactly how a rebinding attack turns an
    /// allowlisted host into a request against the loopback interface.
    async fn dial(&self, rule: &Rule, host: &str, port: u16) -> Result<TcpStream, DialError> {
        let candidates: Vec<IpAddr> = match host.parse::<IpAddr>() {
            Ok(ip) => vec![ip],
            Err(_) => tokio::time::timeout(self.cfg.dial_timeout, self.resolver.lookup(host))
                .await
                .map_err(|_| DialError::Upstream(format!("resolve {host}: timed out")))?
                .map_err(|e| DialError::Upstream(format!("resolve {host}: {e}")))?,
        };
        if candidates.is_empty() {
            return Err(DialError::Upstream(format!("resolve {host}: no addresses")));
        }
        let mut last: Option<DialError> = None;
        for ip in candidates {
            if let Err(d) = self.cfg.allow.permits_addr(rule, ip) {
                last = Some(DialError::Denied(d));
                continue;
            }
            match tokio::time::timeout(
                self.cfg.dial_timeout,
                TcpStream::connect(SocketAddr::new(ip, port)),
            )
            .await
            {
                Ok(Ok(s)) => return Ok(s),
                Ok(Err(e)) => last = Some(DialError::Upstream(e.to_string())),
                Err(_) => last = Some(DialError::Upstream(format!("dial {ip}:{port}: timed out"))),
            }
        }
        Err(last.unwrap_or(DialError::Upstream("no usable address".into())))
    }

    /// Refuses a request by policy and says why, once, in both the log and the
    /// response.
    fn deny(&self, method: &str, host: &str, port: u16, reason: &str) -> Response<Body> {
        // Some reasons quote the request's own host or scheme, so this string
        // is partly attacker-controlled. Sanitised once here covers the header
        // (where a stray CR would be response splitting) and the body.
        let reason = sanitize_reason(reason);
        tracing::warn!(run_id = %self.cfg.run_id, method, host, port, reason, "egress denied");
        text_response(
            StatusCode::FORBIDDEN,
            &reason,
            &format!("hive-sandbox egress denied: {reason}\n"),
        )
    }

    /// Reports that an allowed destination could not be reached. Deliberately
    /// not a 403: the allowlist saying yes and the network saying no are
    /// different facts.
    fn fail_upstream(&self, method: &str, host: &str, port: u16, err: &str) -> Response<Body> {
        tracing::error!(run_id = %self.cfg.run_id, method, host, port, err, "egress upstream failed");
        text_response(
            StatusCode::BAD_GATEWAY,
            "upstream unreachable",
            &format!(
                "hive-sandbox egress: upstream unreachable: {}\n",
                sanitize_reason(err)
            ),
        )
    }

    fn reject(&self, method: &str, host: &str, port: u16, err: DialError) -> Response<Body> {
        match err {
            DialError::Denied(d) => self.deny(method, host, port, &d.0),
            DialError::Upstream(e) => self.fail_upstream(method, host, port, &e),
        }
    }
}

enum DialError {
    Denied(Denied),
    Upstream(String),
}

fn text_response(status: StatusCode, deny: &str, body: &str) -> Response<Body> {
    Response::builder()
        .status(status)
        .header(
            DENY_HEADER,
            HeaderValue::from_str(deny).unwrap_or(HeaderValue::from_static("denied")),
        )
        .header(header::CONTENT_TYPE, "text/plain; charset=utf-8")
        .header("x-content-type-options", "nosniff")
        .body(
            Full::new(Bytes::from(body.to_string()))
                .map_err(|never| match never {})
                .boxed(),
        )
        .expect("static response")
}

fn empty() -> Body {
    Empty::<Bytes>::new()
        .map_err(|never| match never {})
        .boxed()
}

/// Reduces a reason to printable ASCII, bounded. Not cosmetic: the reason can
/// embed a hostname taken straight off the request, and a control character in
/// it would be header injection on the way out.
fn sanitize_reason(reason: &str) -> String {
    reason
        .chars()
        .map(|c| if (' '..='~').contains(&c) { c } else { '?' })
        .take(MAX_REASON_BYTES)
        .collect()
}

/// Accepts "host", "host:port" and "[::1]:port".
fn split_host_port(target: &str, default_port: u16) -> Result<(String, u16), String> {
    let target = target.trim();
    if target.is_empty() {
        return Err("empty target".into());
    }
    if let Some(rest) = target.strip_prefix('[') {
        let (host, after) = rest.split_once(']').ok_or("malformed address")?;
        let port = match after.strip_prefix(':') {
            Some(p) => p.parse::<u16>().map_err(|_| format!("bad port {p:?}"))?,
            None => default_port,
        };
        if port == 0 {
            return Err("bad port".into());
        }
        return Ok((host.to_ascii_lowercase(), port));
    }
    if target.matches(':').count() == 1
        && let Some((host, p)) = target.rsplit_once(':')
    {
        let port = p.parse::<u16>().map_err(|_| format!("bad port {p:?}"))?;
        if port == 0 || host.is_empty() {
            return Err("bad target".into());
        }
        return Ok((host.to_ascii_lowercase(), port));
    }
    Ok((target.to_ascii_lowercase(), default_port))
}

/// Copies bytes both ways until either side is done or the tunnel goes idle.
/// Idleness is measured by activity, not by total duration: a long download
/// and a long-polling stream both keep resetting it.
async fn tunnel<C>(client: C, upstream: TcpStream, idle: Duration)
where
    C: tokio::io::AsyncRead + tokio::io::AsyncWrite + Send + Unpin + 'static,
{
    let active = Arc::new(AtomicBool::new(false));
    let (mut cr, mut cw) = tokio::io::split(client);
    let (mut ur, mut uw) = upstream.into_split();

    async fn pump<R, W>(mut r: R, mut w: W, active: Arc<AtomicBool>)
    where
        R: tokio::io::AsyncRead + Unpin,
        W: tokio::io::AsyncWrite + Unpin,
    {
        let mut buf = vec![0u8; 32 << 10];
        loop {
            match r.read(&mut buf).await {
                Ok(0) | Err(_) => break,
                Ok(n) => {
                    active.store(true, Ordering::Relaxed);
                    if w.write_all(&buf[..n]).await.is_err() {
                        break;
                    }
                }
            }
        }
        // Half-close so the far side sees end-of-stream rather than waiting.
        let _ = w.shutdown().await;
    }

    let a = active.clone();
    let up = tokio::spawn(async move { pump(&mut cr, &mut uw, a).await });
    let a = active.clone();
    let down = tokio::spawn(async move { pump(&mut ur, &mut cw, a).await });

    let mut watchdog = tokio::time::interval(idle);
    watchdog.tick().await;
    let mut finished = 0;
    let (mut up, mut down) = (Some(up), Some(down));
    while finished < 2 {
        tokio::select! {
            _ = async { if let Some(h) = up.as_mut() { let _ = h.await; } }, if up.is_some() => { up = None; finished += 1; }
            _ = async { if let Some(h) = down.as_mut() { let _ = h.await; } }, if down.is_some() => { down = None; finished += 1; }
            _ = watchdog.tick() => {
                // Swap-and-test: any byte since the last tick resets the clock.
                if active.swap(false, Ordering::Relaxed) {
                    continue;
                }
                // Dropping the tasks drops both halves, which closes both
                // sockets.
                if let Some(h) = up.take() { h.abort(); }
                if let Some(h) = down.take() { h.abort(); }
                return;
            }
        }
    }
}
