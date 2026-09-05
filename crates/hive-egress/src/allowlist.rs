//! What a run may reach.

use std::fmt;
use std::net::{IpAddr, Ipv4Addr};

/// The ports an entry without one permits. Deliberately short: an agent that
/// needs 8443 should say 8443.
pub const DEFAULT_PORTS: [u16; 2] = [80, 443];

/// Marks an entry as permitted to resolve to a non-public address:
/// `private:printer.home.example.com`. Spelled out rather than inferred,
/// because "this name is allowed to point inside the network" is exactly the
/// sentence someone should have to write down.
pub const PRIVATE_PREFIX: &str = "private:";

/// One allowlist entry.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Rule {
    /// A hostname, an IP literal, or (with `wildcard`) the dotted suffix of a
    /// `*.` form.
    pub host: String,
    /// Ports the rule permits. Empty means `DEFAULT_PORTS`.
    pub ports: Vec<u16>,
    wildcard: bool,
    /// Lets THIS rule reach a non-public address. Per rule, not per
    /// allowlist: a global flag meant that allowing one LAN printer widened
    /// the SSRF guard for every other entry.
    allow_private: bool,
    /// Set when `host` parsed as an IP literal, so an explicit address entry
    /// matches without a DNS round trip.
    ip: Option<IpAddr>,
}

impl Rule {
    fn permits_port(&self, port: u16) -> bool {
        if self.ports.is_empty() {
            DEFAULT_PORTS.contains(&port)
        } else {
            self.ports.contains(&port)
        }
    }

    fn matches_host(&self, host: &str) -> bool {
        if self.wildcard {
            // "*.example.com" covers sub.example.com and deeper, and
            // deliberately not example.com. Allowing the apex by implication
            // is how an allowlist ends up broader than the person writing it
            // believed.
            return host.ends_with(&self.host);
        }
        if let Some(ip) = self.ip {
            return host.parse::<IpAddr>().is_ok_and(|c| same_ip(ip, c));
        }
        host == self.host
    }
}

impl fmt::Display for Rule {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let host = if self.wildcard {
            format!("*{}", self.host)
        } else {
            self.host.clone()
        };
        if self.ports.is_empty() {
            return f.write_str(&host);
        }
        let parts: Vec<String> = self
            .ports
            .iter()
            .map(|p| join_host_port(&host, *p))
            .collect();
        f.write_str(&parts.join(","))
    }
}

fn join_host_port(host: &str, port: u16) -> String {
    if host.contains(':') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    }
}

/// A 4-byte address and its 4-in-6 form are the same address.
fn same_ip(a: IpAddr, b: IpAddr) -> bool {
    canonical(a) == canonical(b)
}

fn canonical(ip: IpAddr) -> IpAddr {
    match ip {
        IpAddr::V6(v6) => v6
            .to_ipv4_mapped()
            .map(IpAddr::V4)
            .unwrap_or(IpAddr::V6(v6)),
        v4 => v4,
    }
}

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("allowlist entry {entry:?}: {reason}")]
pub struct AllowlistError {
    pub entry: String,
    pub reason: String,
}

/// A refusal by policy, as opposed to a network failure. The distinction is
/// load-bearing: a policy denial is a 403 and means the allowlist did its
/// job, while an unreachable upstream is a 502 and means something is broken.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("{0}")]
pub struct Denied(pub String);

/// Decides what a run may reach. The default denies everything, which is the
/// point: a proxy that starts up empty is safe, not broken.
#[derive(Clone, Debug, Default)]
pub struct Allowlist {
    rules: Vec<Rule>,
    /// Permits RFC1918, loopback, link-local and ULA destinations for every
    /// rule. Off by default. This is the DNS-rebinding and SSRF control:
    /// without it, allowlisting `metrics.example.com` is enough to reach
    /// 169.254.169.254, because the attacker controls what the name resolves
    /// to. Turning it on is a deliberate widening; the per-rule prefix is the
    /// narrow alternative.
    pub allow_private_destinations: bool,
}

/// Builds an allowlist from entries.
///
/// ```text
/// example.com            host on ports 80 and 443
/// example.com:8443       host on one port
/// *.example.com          subdomains of example.com, NOT example.com itself
/// 192.0.2.10             an address literal
/// private:printer.lan    a name that may resolve somewhere private
/// ```
///
/// An empty list is valid and denies everything.
pub fn parse_allowlist<S: AsRef<str>>(entries: &[S]) -> Result<Allowlist, AllowlistError> {
    let mut list = Allowlist::default();
    for raw in entries {
        let entry = raw.as_ref().trim();
        if entry.is_empty() {
            continue;
        }
        let rule = parse_rule(entry).map_err(|reason| AllowlistError {
            entry: raw.as_ref().to_string(),
            reason,
        })?;
        list.rules.push(rule);
    }
    Ok(list)
}

/// Splits `host[:port]`, `[v6]:port` and bare `[v6]` forms. `None` port when
/// there is none to split off.
fn split_host_port(entry: &str) -> Result<(String, Option<u16>), String> {
    if let Some(rest) = entry.strip_prefix('[') {
        let Some((host, after)) = rest.split_once(']') else {
            return Err("malformed bracketed address".into());
        };
        if after.is_empty() {
            return Ok((host.to_string(), None));
        }
        let Some(port) = after.strip_prefix(':') else {
            return Err("malformed bracketed address".into());
        };
        return Ok((host.to_string(), Some(parse_port(port)?)));
    }
    // More than one colon and no brackets is a bare IPv6 literal, not a port.
    if entry.matches(':').count() == 1
        && let Some((host, port)) = entry.rsplit_once(':')
    {
        return Ok((host.to_string(), Some(parse_port(port)?)));
    }
    Ok((entry.to_string(), None))
}

fn parse_port(p: &str) -> Result<u16, String> {
    let n: u32 = p
        .parse()
        .map_err(|_| format!("port {p:?} is not a number"))?;
    if !(1..=65535).contains(&n) {
        return Err(format!("port {n} is out of range"));
    }
    Ok(n as u16)
}

fn parse_rule(entry: &str) -> Result<Rule, String> {
    let mut allow_private = false;
    let mut entry = entry;
    if let Some(rest) = entry.strip_prefix(PRIVATE_PREFIX) {
        allow_private = true;
        entry = rest.trim();
        if entry.is_empty() {
            return Err(format!("no host after the {PRIVATE_PREFIX:?} prefix"));
        }
    }
    let (host, port) = split_host_port(entry)?;
    let host = host.trim_end_matches('.').to_ascii_lowercase();
    if host.is_empty() {
        return Err("no host".into());
    }
    let ports = port.map(|p| vec![p]).unwrap_or_default();
    let mut rule = Rule {
        host: String::new(),
        ports,
        wildcard: false,
        allow_private,
        ip: None,
    };
    if host == "*" {
        // An allowlist that allows everything is almost certainly a mistake,
        // and if it is not, it should be spelled out host by host.
        return Err("\"*\" would allow every host; list them explicitly".into());
    }
    if let Some(suffix) = host.strip_prefix("*.") {
        if suffix.is_empty() || suffix.contains('*') {
            return Err("malformed wildcard".into());
        }
        rule.host = format!(".{suffix}");
        rule.wildcard = true;
        return Ok(rule);
    }
    if host.contains('*') {
        return Err("a wildcard is only supported as a leading \"*.\" label".into());
    }
    rule.ip = host.parse::<IpAddr>().ok();
    // Naming a private address literally IS the opt-in. Writing 192.168.1.50
    // in an allowlist and having it silently never match was the other half
    // of this bug: the entry looked effective and was inert.
    if rule.ip.is_some_and(is_private) {
        rule.allow_private = true;
    }
    rule.host = host;
    Ok(rule)
}

impl Allowlist {
    pub fn rules(&self) -> &[Rule] {
        &self.rules
    }

    pub fn is_empty(&self) -> bool {
        self.rules.is_empty()
    }

    /// Whether host:port is allowed by name. It says nothing about where the
    /// name resolves; `permits_addr` is the other half.
    pub fn permits(&self, host: &str, port: u16) -> bool {
        self.match_rule(host, port).is_some()
    }

    /// The rule that permits host:port, if any. The rule matters to the
    /// second check: whether a resolved address may be dialled is a question
    /// about the entry that allowed the name, not about the list as a whole.
    pub fn match_rule(&self, host: &str, port: u16) -> Option<&Rule> {
        let host = host.trim_end_matches('.').to_ascii_lowercase();
        let host = host.trim_start_matches('[').trim_end_matches(']');
        self.rules
            .iter()
            .find(|r| r.permits_port(port) && r.matches_host(host))
    }

    /// Whether a resolved address may be dialled under a rule. Separate from
    /// `permits` because the two failures are different: a name that is not
    /// on the list is a misconfiguration, and a name on the list that resolves
    /// somewhere private is an attack or an accident that looks like one.
    pub fn permits_addr(&self, rule: &Rule, ip: IpAddr) -> Result<(), Denied> {
        if !is_private(ip) {
            return Ok(());
        }
        if self.allow_private_destinations || rule.allow_private {
            return Ok(());
        }
        Err(Denied(format!("destination {ip} is not a public address")))
    }
}

/// Everything a run has no business reaching by resolving a public name:
/// loopback, RFC1918, link-local (where cloud metadata services live), ULA,
/// carrier-grade NAT (also container runtimes), multicast and the
/// unspecified address. A 4-in-6 address is judged as its 4-byte self.
pub fn is_private(ip: IpAddr) -> bool {
    match canonical(ip) {
        IpAddr::V4(v4) => {
            v4.is_loopback()
                || v4.is_private()
                || v4.is_unspecified()
                || v4.is_link_local()
                || v4.is_multicast()
                || v4.is_broadcast()
                || is_cgnat(v4)
        }
        IpAddr::V6(v6) => {
            v6.is_loopback()
                || v6.is_unspecified()
                || v6.is_multicast()
                || v6.is_unicast_link_local()
                || v6.is_unique_local()
        }
    }
}

/// 100.64.0.0/10.
fn is_cgnat(v4: Ipv4Addr) -> bool {
    let o = v4.octets();
    o[0] == 100 && (64..=127).contains(&o[1])
}
