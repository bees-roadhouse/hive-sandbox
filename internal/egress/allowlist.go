// Package egress is the allowlisting proxy a harness run reaches the internet
// through (D12.6, D12.10).
//
// A harness container has no route out. Its only path to the network is this
// proxy, sitting on a per-run internal Podman network, and the proxy answers one
// question: may this run reach this host on this port. Absence of an allowlist
// entry is deny, which is invariant 1 applied to egress.
//
// The trifecta rule (D10) is why this exists. A harness has private data through
// MCP, untrusted content from files and the web, and egress. Egress is the leg
// that is cheap to bound, so it is bounded per run rather than per deployment.
package egress

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// DefaultPorts are the ports an entry without one permits. Deliberately short:
// an agent that needs 8443 should say 8443.
var DefaultPorts = []int{80, 443}

// Rule is one allowlist entry.
type Rule struct {
	// Host is a hostname, an IP literal, or a "*." wildcard.
	Host string

	// Ports the rule permits. Empty means DefaultPorts.
	Ports []int

	// wildcard is set for "*.example.com" forms.
	wildcard bool

	// ip is set when Host parsed as an IP literal, so an explicit address entry
	// can be matched without a DNS round trip.
	ip net.IP
}

// Allowlist decides what a run may reach. The zero value denies everything,
// which is the point: a proxy that starts up empty is safe, not broken.
type Allowlist struct {
	rules []Rule

	// AllowPrivateDestinations permits RFC1918, loopback, link-local and ULA
	// destinations. Off by default.
	//
	// This is the DNS-rebinding and SSRF control. Without it, allowlisting
	// `metrics.example.com` is enough to reach 169.254.169.254 or the host's
	// own services, because the attacker controls what the name resolves to.
	// Turning it on is for LAN targets a run legitimately needs (tools.md's
	// local browser driver), and it is a deliberate widening.
	AllowPrivateDestinations bool
}

// ParseAllowlist builds an allowlist from entries.
//
// Accepted forms:
//
//	example.com            host on ports 80 and 443
//	example.com:8443       host on one port
//	*.example.com          subdomains of example.com, NOT example.com itself
//	*.example.com:8443     subdomains on one port
//	192.0.2.10             an address literal
//	192.0.2.10:9000        an address literal on one port
//
// An empty list is valid and denies everything.
func ParseAllowlist(entries []string) (*Allowlist, error) {
	list := &Allowlist{}

	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}

		rule, err := parseRule(entry)
		if err != nil {
			return nil, fmt.Errorf("allowlist entry %q: %w", raw, err)
		}
		list.rules = append(list.rules, rule)
	}

	return list, nil
}

func parseRule(entry string) (Rule, error) {
	host := entry
	var ports []int

	// SplitHostPort would reject a bare hostname and mangle a bare IPv6, so
	// only split when there is a port to split off.
	if h, p, err := net.SplitHostPort(entry); err == nil {
		port, convErr := strconv.Atoi(p)
		if convErr != nil {
			return Rule{}, fmt.Errorf("port %q is not a number", p)
		}
		if port < 1 || port > 65535 {
			return Rule{}, fmt.Errorf("port %d is out of range", port)
		}
		host = h
		ports = []int{port}
	} else if strings.HasPrefix(entry, "[") {
		// A bracketed IPv6 with no port: strip the brackets.
		host = strings.TrimSuffix(strings.TrimPrefix(entry, "["), "]")
	}

	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return Rule{}, errors.New("no host")
	}

	rule := Rule{Ports: ports}

	switch {
	case host == "*":
		// An allowlist that allows everything is almost certainly a mistake,
		// and if it is not, it should be spelled out host by host.
		return Rule{}, errors.New(`"*" would allow every host; list them explicitly`)

	case strings.HasPrefix(host, "*."):
		suffix := host[1:] // keep the leading dot
		if strings.ContainsAny(suffix[1:], "*") || suffix == "." {
			return Rule{}, errors.New("malformed wildcard")
		}
		rule.Host = suffix
		rule.wildcard = true

	case strings.Contains(host, "*"):
		return Rule{}, errors.New("a wildcard is only supported as a leading \"*.\" label")

	default:
		rule.Host = host
		rule.ip = net.ParseIP(host)
	}

	return rule, nil
}

// Rules returns the parsed rules, for logging and tests.
func (a *Allowlist) Rules() []Rule { return a.rules }

// Empty reports whether the allowlist permits nothing.
func (a *Allowlist) Empty() bool { return a == nil || len(a.rules) == 0 }

// Permits reports whether host:port is allowed by name. It says nothing about
// where the name resolves; [CheckAddr] is the other half.
func (a *Allowlist) Permits(host string, port int) bool {
	if a == nil {
		return false
	}

	host = strings.ToLower(strings.TrimSuffix(host, "."))
	// A bracketed literal reaches here from a request URI.
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")

	for _, rule := range a.rules {
		if !rule.permitsPort(port) {
			continue
		}
		if rule.matchesHost(host) {
			return true
		}
	}
	return false
}

func (r Rule) permitsPort(port int) bool {
	if len(r.Ports) == 0 {
		for _, p := range DefaultPorts {
			if p == port {
				return true
			}
		}
		return false
	}
	for _, p := range r.Ports {
		if p == port {
			return true
		}
	}
	return false
}

func (r Rule) matchesHost(host string) bool {
	if r.wildcard {
		// "*.example.com" covers sub.example.com and deeper, and deliberately
		// not example.com. Allowing the apex by implication is how an allowlist
		// ends up broader than the person writing it believed.
		return strings.HasSuffix(host, r.Host)
	}
	if r.ip != nil {
		if candidate := net.ParseIP(host); candidate != nil {
			return r.ip.Equal(candidate)
		}
		return false
	}
	return host == r.Host
}

// DeniedError is a refusal by policy, as opposed to a network failure.
//
// The distinction is load-bearing: a policy denial is a 403 and means the
// allowlist did its job, while an unreachable upstream is a 502 and means
// something is broken. Collapsing them into one status made a DNS failure
// inside the proxy look exactly like a correctly-enforced allowlist, which cost
// an hour the first time it happened.
type DeniedError struct {
	Reason string
}

func (e *DeniedError) Error() string { return e.Reason }

// PermitsAddr reports whether a resolved address may be dialled.
//
// Separate from [Permits] because the two failures are different: a name that
// is not on the list is a misconfiguration, and a name on the list that resolves
// somewhere private is an attack or an accident that looks like one.
func (a *Allowlist) PermitsAddr(ip net.IP) error {
	if a == nil {
		return &DeniedError{Reason: "no allowlist configured"}
	}
	if a.AllowPrivateDestinations {
		return nil
	}

	if isPrivate(ip) {
		return &DeniedError{Reason: fmt.Sprintf("destination %s is not a public address", ip)}
	}
	return nil
}

// isPrivate covers everything a run has no business reaching by resolving a
// public name: loopback, RFC1918, link-local (which is where cloud metadata
// services live), ULA, and the unspecified address.
func isPrivate(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// No IPv4-mapped re-check here, deliberately. It looks necessary and is
	// dead code: net.IP.Equal treats a 4-byte address and its 4-in-6 form as
	// equal, and IsLoopback, IsPrivate and the link-local checks all normalise
	// internally, so ::ffff:127.0.0.1 is already caught above. A re-check that
	// cannot fire, with a comment claiming it is the guard, is worse than no
	// comment at all.
	// 100.64.0.0/10, carrier-grade NAT, also used by container runtimes.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}

// String renders a rule the way it would be written in configuration.
func (r Rule) String() string {
	host := r.Host
	if r.wildcard {
		host = "*" + r.Host
	}
	if len(r.Ports) == 0 {
		return host
	}
	parts := make([]string, 0, len(r.Ports))
	for _, p := range r.Ports {
		parts = append(parts, net.JoinHostPort(host, strconv.Itoa(p)))
	}
	return strings.Join(parts, ",")
}
