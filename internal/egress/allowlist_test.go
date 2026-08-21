package egress_test

import (
	"net"
	"testing"

	"github.com/bees-roadhouse/hive-sandbox/internal/egress"
)

func mustParse(t *testing.T, entries ...string) *egress.Allowlist {
	t.Helper()

	list, err := egress.ParseAllowlist(entries)
	if err != nil {
		t.Fatalf("ParseAllowlist(%v): %v", entries, err)
	}
	return list
}

func TestAllowlistMatching(t *testing.T) {
	t.Parallel()

	list := mustParse(t,
		"api.anthropic.com",
		"*.githubusercontent.com",
		"registry.npmjs.org:443",
		"192.0.2.10",
		"internal.example.com:8443",
	)

	tests := []struct {
		host string
		port int
		want bool
		why  string
	}{
		{"api.anthropic.com", 443, true, "exact host, default port"},
		{"api.anthropic.com", 80, true, "exact host, other default port"},
		{"api.anthropic.com", 8080, false, "a default-port entry does not imply every port"},
		{"API.Anthropic.COM", 443, true, "host matching is case-insensitive"},
		{"api.anthropic.com.", 443, true, "a trailing dot is the same name"},

		{"evil-api.anthropic.com", 443, false, "prefix must not match by substring"},
		{"api.anthropic.com.evil.test", 443, false, "suffix must not match by substring"},

		{"objects.githubusercontent.com", 443, true, "wildcard covers a subdomain"},
		{"a.b.githubusercontent.com", 443, true, "wildcard covers deeper subdomains"},
		{"githubusercontent.com", 443, false, "wildcard deliberately excludes the apex"},
		{"notgithubusercontent.com", 443, false, "wildcard must match on a label boundary"},

		{"registry.npmjs.org", 443, true, "explicit port matches"},
		{"registry.npmjs.org", 80, false, "an explicit port excludes the other default"},

		{"192.0.2.10", 443, true, "address literal"},
		{"192.0.2.11", 443, false, "a different address"},

		{"internal.example.com", 8443, true, "non-default port when asked for"},
		{"internal.example.com", 443, false, "and only that port"},

		{"unlisted.example.com", 443, false, "absence of an entry is deny"},
	}

	for _, tc := range tests {
		if got := list.Permits(tc.host, tc.port); got != tc.want {
			t.Errorf("Permits(%q, %d) = %v, want %v ... %s", tc.host, tc.port, got, tc.want, tc.why)
		}
	}
}

func TestEmptyAllowlistDeniesEverything(t *testing.T) {
	t.Parallel()

	empty := mustParse(t)
	if !empty.Empty() {
		t.Error("an allowlist with no entries should report Empty")
	}
	if empty.Permits("api.anthropic.com", 443) {
		t.Error("an empty allowlist permitted a host")
	}

	// The zero value is what a misconfigured proxy comes up with, so it has to
	// be useless rather than open.
	var nilList *egress.Allowlist
	if nilList.Permits("api.anthropic.com", 443) {
		t.Error("a nil allowlist permitted a host")
	}
	if err := nilList.PermitsAddr(net.ParseIP("93.184.216.34")); err == nil {
		t.Error("a nil allowlist permitted an address")
	}
}

func TestAllowlistRejectsDangerousEntries(t *testing.T) {
	t.Parallel()

	for _, entry := range []string{
		"*",             // would allow everything
		"*.*.com",       // wildcard in a second position
		"exam*ple.com",  // wildcard mid-label
		"example.com:0", // port out of range
		"example.com:99999",
		"example.com:http", // non-numeric port
		":443",             // no host
	} {
		if _, err := egress.ParseAllowlist([]string{entry}); err == nil {
			t.Errorf("ParseAllowlist(%q) succeeded; it should be refused", entry)
		}
	}
}

// The rebinding control. A name being on the allowlist says nothing about where
// it resolves, and an attacker who controls the DNS answer controls that.
func TestPermitsAddrRejectsNonPublicDestinations(t *testing.T) {
	t.Parallel()

	list := mustParse(t, "metrics.example.com")

	for _, addr := range []string{
		"127.0.0.1",        // loopback
		"::1",              // loopback v6
		"10.1.2.3",         // RFC1918
		"192.168.4.5",      // RFC1918
		"172.16.0.1",       // RFC1918
		"169.254.169.254",  // cloud metadata, the classic SSRF target
		"fd00::1",          // unique local
		"fe80::1",          // link-local
		"100.64.1.1",       // carrier-grade NAT, also container runtimes
		"0.0.0.0",          // unspecified
		"::ffff:127.0.0.1", // v4-mapped loopback
	} {
		if err := list.PermitsAddr(net.ParseIP(addr)); err == nil {
			t.Errorf("PermitsAddr(%s) allowed a non-public destination", addr)
		}
	}

	if err := list.PermitsAddr(net.ParseIP("93.184.216.34")); err != nil {
		t.Errorf("PermitsAddr on a public address: %v", err)
	}
}

// LAN targets are a real need (a local browser driver), so the widening exists.
// It has to be explicit.
func TestAllowPrivateDestinationsIsOptIn(t *testing.T) {
	t.Parallel()

	list := mustParse(t, "printer.home.example.com")
	if err := list.PermitsAddr(net.ParseIP("192.168.1.50")); err == nil {
		t.Fatal("private destinations allowed without opting in")
	}

	list.AllowPrivateDestinations = true
	if err := list.PermitsAddr(net.ParseIP("192.168.1.50")); err != nil {
		t.Errorf("private destination still refused after opting in: %v", err)
	}
}

func TestNormalizeDNSServers(t *testing.T) {
	t.Parallel()

	got := egress.NormalizeDNSServers([]string{"1.1.1.1", " 9.9.9.9:5353 ", "", "[2606:4700:4700::1111]", "  "})
	want := []string{"1.1.1.1:53", "9.9.9.9:5353", "[2606:4700:4700::1111]:53"}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}
