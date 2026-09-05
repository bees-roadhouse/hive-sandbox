//! Name resolution the proxy controls.

use std::net::{IpAddr, SocketAddr};
use std::time::Duration;

use async_trait::async_trait;

/// The name-to-address lookup the proxy uses. A trait so a test can point a
/// name at an address it controls, which is the only honest way to exercise
/// the rebinding guard.
#[async_trait]
pub trait Resolver: Send + Sync {
    async fn lookup(&self, host: &str) -> Result<Vec<IpAddr>, String>;
}

/// The operating system's resolver.
pub struct SystemResolver;

#[async_trait]
impl Resolver for SystemResolver {
    async fn lookup(&self, host: &str) -> Result<Vec<IpAddr>, String> {
        let addrs = tokio::net::lookup_host((host, 0))
            .await
            .map_err(|e| e.to_string())?;
        Ok(addrs.map(|a| a.ip()).collect())
    }
}

/// Queries the given servers directly, bypassing resolv.conf.
///
/// The proxy runs attached to an `--internal` Podman network, which points
/// resolv.conf at that network's aardvark-dns. Aardvark has no upstream to
/// forward to there, so it answers NXDOMAIN for every external name.
/// Resolving here, explicitly, is the fix, and it makes which resolvers the
/// platform's egress uses a decision rather than whatever the runtime wrote.
pub struct DnsResolver {
    inner: hickory_resolver::TokioResolver,
}

impl DnsResolver {
    /// `servers` are `host[:port]`; a missing port is 53.
    pub fn new(servers: &[String], timeout: Duration) -> Result<DnsResolver, String> {
        use hickory_resolver::config::{NameServerConfigGroup, ResolverConfig};
        use hickory_resolver::name_server::TokioConnectionProvider;

        let mut ips = Vec::new();
        let mut port = 53u16;
        for s in normalize_dns_servers(servers) {
            let addr: SocketAddr = s.parse().map_err(|e| format!("dns server {s:?}: {e}"))?;
            ips.push(addr.ip());
            port = addr.port();
        }
        if ips.is_empty() {
            return Err("no DNS servers configured".into());
        }
        let group = NameServerConfigGroup::from_ips_clear(&ips, port, true);
        let config = ResolverConfig::from_parts(None, vec![], group);
        let mut builder = hickory_resolver::Resolver::builder_with_config(
            config,
            TokioConnectionProvider::default(),
        );
        builder.options_mut().timeout = timeout;
        Ok(DnsResolver {
            inner: builder.build(),
        })
    }
}

#[async_trait]
impl Resolver for DnsResolver {
    async fn lookup(&self, host: &str) -> Result<Vec<IpAddr>, String> {
        let lookup = self
            .inner
            .lookup_ip(host)
            .await
            .map_err(|e| e.to_string())?;
        Ok(lookup.iter().collect())
    }
}

/// Adds the default DNS port to bare addresses and drops empties.
pub fn normalize_dns_servers<S: AsRef<str>>(servers: &[S]) -> Vec<String> {
    let mut out = Vec::new();
    for raw in servers {
        let s = raw.as_ref().trim();
        if s.is_empty() {
            continue;
        }
        if s.parse::<SocketAddr>().is_ok() {
            out.push(s.to_string());
            continue;
        }
        let host = s.trim_start_matches('[').trim_end_matches(']');
        if host.contains(':') {
            out.push(format!("[{host}]:53"));
        } else {
            out.push(format!("{host}:53"));
        }
    }
    out
}
