//! The allowlisting proxy a harness run reaches the internet through (D12.6,
//! D12.10).
//!
//! A harness container has no route out. Its only path to the network is this
//! proxy, sitting on a per-run internal Podman network, and the proxy answers
//! one question: may this run reach this host on this port. Absence of an
//! allowlist entry is deny, which is invariant 1 applied to egress.
//!
//! The trifecta rule (D10) is why this exists. A harness has private data
//! through MCP, untrusted content from files and the web, and egress. Egress is
//! the leg that is cheap to bound, so it is bounded per run rather than per
//! deployment.

mod allowlist;
mod proxy;
mod resolve;

pub use allowlist::{
    Allowlist, AllowlistError, DEFAULT_PORTS, Denied, PRIVATE_PREFIX, Rule, is_private,
    parse_allowlist,
};
pub use proxy::{DENY_HEADER, Proxy, ProxyConfig};
pub use resolve::{DnsResolver, Resolver, SystemResolver, normalize_dns_servers};
