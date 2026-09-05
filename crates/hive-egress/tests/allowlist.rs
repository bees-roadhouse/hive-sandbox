//! Ported from allowlist_test.go.

use std::net::IpAddr;

use hive_egress::{Allowlist, PRIVATE_PREFIX, normalize_dns_servers, parse_allowlist};

fn must_parse(entries: &[&str]) -> Allowlist {
    parse_allowlist(entries).unwrap_or_else(|e| panic!("parse_allowlist({entries:?}): {e}"))
}

fn ip(s: &str) -> IpAddr {
    s.parse().unwrap()
}

/// Ported from `TestAllowlistMatching`.
#[test]
fn allowlist_matching() {
    let list = must_parse(&[
        "api.anthropic.com",
        "*.githubusercontent.com",
        "registry.npmjs.org:443",
        "192.0.2.10",
        "internal.example.com:8443",
    ]);
    let cases: &[(&str, u16, bool, &str)] = &[
        ("api.anthropic.com", 443, true, "exact host, default port"),
        (
            "api.anthropic.com",
            80,
            true,
            "exact host, other default port",
        ),
        (
            "api.anthropic.com",
            8080,
            false,
            "a default-port entry does not imply every port",
        ),
        (
            "API.Anthropic.COM",
            443,
            true,
            "host matching is case-insensitive",
        ),
        (
            "api.anthropic.com.",
            443,
            true,
            "a trailing dot is the same name",
        ),
        (
            "evil-api.anthropic.com",
            443,
            false,
            "prefix must not match by substring",
        ),
        (
            "api.anthropic.com.evil.test",
            443,
            false,
            "suffix must not match by substring",
        ),
        (
            "objects.githubusercontent.com",
            443,
            true,
            "wildcard covers a subdomain",
        ),
        (
            "a.b.githubusercontent.com",
            443,
            true,
            "wildcard covers deeper subdomains",
        ),
        (
            "githubusercontent.com",
            443,
            false,
            "wildcard deliberately excludes the apex",
        ),
        (
            "notgithubusercontent.com",
            443,
            false,
            "wildcard must match on a label boundary",
        ),
        ("registry.npmjs.org", 443, true, "explicit port matches"),
        (
            "registry.npmjs.org",
            80,
            false,
            "an explicit port excludes the other default",
        ),
        ("192.0.2.10", 443, true, "address literal"),
        ("192.0.2.11", 443, false, "a different address"),
        (
            "internal.example.com",
            8443,
            true,
            "non-default port when asked for",
        ),
        ("internal.example.com", 443, false, "and only that port"),
        (
            "unlisted.example.com",
            443,
            false,
            "absence of an entry is deny",
        ),
    ];
    for (host, port, want, why) in cases {
        assert_eq!(
            list.permits(host, *port),
            *want,
            "permits({host:?}, {port}) ... {why}"
        );
    }
}

/// Ported from `TestEmptyAllowlistDeniesEverything`.
#[test]
fn empty_allowlist_denies_everything() {
    let empty = must_parse(&[]);
    assert!(empty.is_empty());
    assert!(!empty.permits("api.anthropic.com", 443));
    let zero = Allowlist::default();
    assert!(
        !zero.permits("api.anthropic.com", 443),
        "a default allowlist permitted a host"
    );
}

/// Ported from `TestAllowlistRejectsDangerousEntries`.
#[test]
fn allowlist_rejects_dangerous_entries() {
    for entry in [
        "*",
        "*.*.com",
        "exam*ple.com",
        "example.com:0",
        "example.com:99999",
        "example.com:http",
        ":443",
    ] {
        assert!(
            parse_allowlist(&[entry]).is_err(),
            "parse_allowlist({entry:?}) succeeded; it should be refused"
        );
    }
}

/// Ported from `TestPermitsAddrRejectsNonPublicDestinations`: the rebinding
/// control.
#[test]
fn permits_addr_rejects_non_public_destinations() {
    let list = must_parse(&["metrics.example.com"]);
    let rule = list
        .match_rule("metrics.example.com", 443)
        .expect("the rule under test matches its own host")
        .clone();
    for addr in [
        "127.0.0.1",
        "::1",
        "10.1.2.3",
        "192.168.4.5",
        "172.16.0.1",
        "169.254.169.254",
        "fd00::1",
        "fe80::1",
        "100.64.1.1",
        "0.0.0.0",
        "::ffff:127.0.0.1",
    ] {
        assert!(
            list.permits_addr(&rule, ip(addr)).is_err(),
            "permits_addr({addr}) allowed a non-public destination"
        );
    }
    // RFC 5737 documentation range: reserved, but not private.
    list.permits_addr(&rule, ip("192.0.2.1"))
        .expect("a public address");
}

/// Ported from `TestPrivateDestinationsArePermittedPerRule`.
#[test]
fn private_destinations_are_permitted_per_rule() {
    let list = must_parse(&[
        &format!("{PRIVATE_PREFIX}printer.home.example.test"),
        "api.example.test",
    ]);
    let printer = list
        .match_rule("printer.home.example.test", 443)
        .unwrap()
        .clone();
    let api = list.match_rule("api.example.test", 443).unwrap().clone();
    let lan = ip("192.168.1.50");
    list.permits_addr(&printer, lan)
        .expect("the rule that asked for a private destination was refused");
    assert!(
        list.permits_addr(&api, lan).is_err(),
        "allowing one LAN host widened the guard for a rule that never asked"
    );
    assert!(
        list.permits_addr(&api, ip("169.254.169.254")).is_err(),
        "a public rule reached the metadata service"
    );
    for rule in [&printer, &api] {
        list.permits_addr(rule, ip("192.0.2.1"))
            .expect("a public address was refused");
    }
}

/// Ported from `TestAnExplicitPrivateLiteralIsItsOwnOptIn`.
#[test]
fn an_explicit_private_literal_is_its_own_opt_in() {
    let list = must_parse(&["192.168.1.50"]);
    let rule = list
        .match_rule("192.168.1.50", 443)
        .expect("an address literal did not match itself")
        .clone();
    list.permits_addr(&rule, ip("192.168.1.50"))
        .expect("an explicitly named private address was refused");
}

/// Ported from `TestAllowPrivateDestinationsStillWidensEverything`.
#[test]
fn allow_private_destinations_still_widens_everything() {
    let mut list = must_parse(&["api.example.test"]);
    let rule = list.match_rule("api.example.test", 443).unwrap().clone();
    assert!(
        list.permits_addr(&rule, ip("192.168.1.50")).is_err(),
        "private allowed without opting in"
    );
    list.allow_private_destinations = true;
    list.permits_addr(&rule, ip("192.168.1.50"))
        .expect("still refused after the list-wide opt-in");
}

/// Ported from `TestNormalizeDNSServers`.
#[test]
fn normalize_dns_servers_adds_the_port() {
    let got = normalize_dns_servers(&[
        "1.1.1.1",
        " 9.9.9.9:5353 ",
        "",
        "[2606:4700:4700::1111]",
        "  ",
    ]);
    assert_eq!(
        got,
        vec!["1.1.1.1:53", "9.9.9.9:5353", "[2606:4700:4700::1111]:53"]
    );
}
