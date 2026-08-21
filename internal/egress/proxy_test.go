package egress_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/egress"
)

// stubResolver points names wherever the test wants. Real DNS in a unit test
// would make the rebinding guard untestable, which is the one behaviour most
// worth proving.
type stubResolver map[string][]string

func (s stubResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	addrs, ok := s[strings.ToLower(host)]
	if !ok {
		return nil, fmt.Errorf("no such host: %s", host)
	}
	out := make([]net.IPAddr, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, net.IPAddr{IP: net.ParseIP(a)})
	}
	return out, nil
}

// quietLogger keeps expected denials out of the test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startProxy runs a proxy on a loopback port and returns its URL.
func startProxy(t *testing.T, p *egress.Proxy) *url.URL {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := p.Server("")
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	proxyURL, err := url.Parse("http://" + listener.Addr().String())
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	return proxyURL
}

func proxyClient(proxyURL *url.URL) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the test origin is self-signed
		},
	}
}

func originHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse origin url: %v", err)
	}
	port := 80
	if p := parsed.Port(); p != "" {
		if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
			t.Fatalf("parse origin port: %v", err)
		}
	}
	return parsed.Hostname(), port
}

func TestProxyForwardsAnAllowedHost(t *testing.T) {
	t.Parallel()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "origin saw %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(origin.Close)
	_, port := originHostPort(t, origin.URL)

	allow := mustParse(t, fmt.Sprintf("api.example.test:%d", port))
	// The test origin is on loopback, so the guard has to be relaxed here. It
	// gets its own test below, with a name that resolves somewhere it should
	// not.
	allow.AllowPrivateDestinations = true

	proxyURL := startProxy(t, &egress.Proxy{
		Allow:    allow,
		RunID:    "run-allowed",
		Logger:   quietLogger(),
		Resolver: stubResolver{"api.example.test": {"127.0.0.1"}},
	})

	res, err := proxyClient(proxyURL).Get(fmt.Sprintf("http://api.example.test:%d/hello", port))
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (deny: %q)", res.StatusCode, res.Header.Get(egress.DenyHeader))
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "origin saw GET /hello") {
		t.Errorf("body = %q, want the origin's response", body)
	}
}

func TestProxyDeniesAnUnlistedHost(t *testing.T) {
	t.Parallel()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(origin.Close)
	_, port := originHostPort(t, origin.URL)

	allow := mustParse(t, fmt.Sprintf("allowed.example.test:%d", port))
	allow.AllowPrivateDestinations = true

	proxyURL := startProxy(t, &egress.Proxy{
		Allow:  allow,
		RunID:  "run-denied",
		Logger: quietLogger(),
		Resolver: stubResolver{
			"allowed.example.test":    {"127.0.0.1"},
			"exfiltrate.example.test": {"127.0.0.1"},
		},
	})

	res, err := proxyClient(proxyURL).Get(fmt.Sprintf("http://exfiltrate.example.test:%d/steal", port))
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
	// The run's own logs should say why, not just show a 403.
	if reason := res.Header.Get(egress.DenyHeader); !strings.Contains(reason, "allowlist") {
		t.Errorf("deny reason = %q, want it to mention the allowlist", reason)
	}
}

// CONNECT is the path that matters: everything an agent CLI does over https
// goes through it, and the destination host is all the proxy can see.
func TestProxyTunnelsAllowedConnect(t *testing.T) {
	t.Parallel()

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "tunnelled")
	}))
	t.Cleanup(origin.Close)
	_, port := originHostPort(t, origin.URL)

	allow := mustParse(t, fmt.Sprintf("api.example.test:%d", port))
	allow.AllowPrivateDestinations = true

	proxyURL := startProxy(t, &egress.Proxy{
		Allow:    allow,
		RunID:    "run-connect",
		Logger:   quietLogger(),
		Resolver: stubResolver{"api.example.test": {"127.0.0.1"}},
	})

	res, err := proxyClient(proxyURL).Get(fmt.Sprintf("https://api.example.test:%d/secure", port))
	if err != nil {
		t.Fatalf("CONNECT through proxy: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	body, _ := io.ReadAll(res.Body)
	if string(body) != "tunnelled" {
		t.Errorf("body = %q, want %q", body, "tunnelled")
	}
}

func TestProxyDeniesUnlistedConnect(t *testing.T) {
	t.Parallel()

	allow := mustParse(t, "allowed.example.test")
	allow.AllowPrivateDestinations = true

	proxyURL := startProxy(t, &egress.Proxy{
		Allow:    allow,
		RunID:    "run-connect-denied",
		Logger:   quietLogger(),
		Resolver: stubResolver{"blocked.example.test": {"127.0.0.1"}},
	})

	res, err := proxyClient(proxyURL).Get("https://blocked.example.test/secure")
	if err == nil {
		_ = res.Body.Close()
		t.Fatal("expected the CONNECT to be refused")
	}
	// Go surfaces a refused CONNECT as a transport error carrying the status.
	if !strings.Contains(err.Error(), "403") && !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("error = %v, want a 403 from the proxy", err)
	}
}

// The rebinding case, end to end through the real dial path: the name is on the
// allowlist and resolves to loopback. Allowlisting a name must not become
// permission to reach the host it happens to point at.
func TestProxyRefusesAllowlistedNameResolvingToLoopback(t *testing.T) {
	t.Parallel()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "this must never be reached")
	}))
	t.Cleanup(origin.Close)
	_, port := originHostPort(t, origin.URL)

	// Note: AllowPrivateDestinations stays false. That is the control.
	allow := mustParse(t, fmt.Sprintf("rebind.example.test:%d", port))

	proxyURL := startProxy(t, &egress.Proxy{
		Allow:    allow,
		RunID:    "run-rebind",
		Logger:   quietLogger(),
		Resolver: stubResolver{"rebind.example.test": {"127.0.0.1"}},
	})

	res, err := proxyClient(proxyURL).Get(fmt.Sprintf("http://rebind.example.test:%d/", port))
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 403; the guard let an allowlisted name reach loopback (body %q)",
			res.StatusCode, body)
	}
	if reason := res.Header.Get(egress.DenyHeader); !strings.Contains(reason, "not a public address") {
		t.Errorf("deny reason = %q, want it to name the address policy", reason)
	}
}

// A policy denial and an unreachable upstream must not share a status code.
// When they did, a DNS failure inside the proxy was indistinguishable from a
// correctly enforced allowlist, which is a bad hour to spend.
func TestProxyDistinguishesDenialFromUnreachable(t *testing.T) {
	t.Parallel()

	// Allowed by name, and the name resolves to an address that is public (so
	// the guard passes) but has nothing listening.
	allow := mustParse(t, "unreachable.example.test:80")

	proxyURL := startProxy(t, &egress.Proxy{
		Allow:       allow,
		RunID:       "run-unreachable",
		Logger:      quietLogger(),
		DialTimeout: 2 * time.Second,
		// RFC 5737 documentation range: routable-looking, never answers.
		Resolver: stubResolver{"unreachable.example.test": {"203.0.113.9"}},
	})

	res, err := proxyClient(proxyURL).Get("http://unreachable.example.test/")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502; the allowlist permitted this host, the network refused it",
			res.StatusCode)
	}
}

// Metadata endpoints are the canonical SSRF target, and 169.254.169.254 is
// reachable from a container that has any route at all.
func TestProxyRefusesCloudMetadataAddress(t *testing.T) {
	t.Parallel()

	allow := mustParse(t, "metadata.example.test")

	proxyURL := startProxy(t, &egress.Proxy{
		Allow:       allow,
		RunID:       "run-metadata",
		Logger:      quietLogger(),
		DialTimeout: 2 * time.Second,
		Resolver:    stubResolver{"metadata.example.test": {"169.254.169.254"}},
	})

	res, err := proxyClient(proxyURL).Get("http://metadata.example.test/latest/meta-data/")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a link-local destination", res.StatusCode)
	}
}

// A proxy that comes up with nothing configured must be useless, not open.
func TestProxyWithNoAllowlistDeniesEverything(t *testing.T) {
	t.Parallel()

	proxyURL := startProxy(t, &egress.Proxy{RunID: "run-empty", Logger: quietLogger()})

	res, err := proxyClient(proxyURL).Get("http://api.anthropic.com/v1/messages")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 from a proxy with no allowlist", res.StatusCode)
	}
}

// Addressing the proxy as if it were an origin server is not a proxy request,
// and answering it would make the proxy a confused deputy.
func TestProxyRefusesOriginFormRequests(t *testing.T) {
	t.Parallel()

	proxyURL := startProxy(t, &egress.Proxy{
		Allow:  mustParse(t, "api.example.test"),
		RunID:  "run-origin-form",
		Logger: quietLogger(),
	})

	// A direct request, not one routed through the client's proxy setting.
	res, err := (&http.Client{Timeout: 10 * time.Second}).Get(proxyURL.String() + "/healthz")
	if err != nil {
		t.Fatalf("direct GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", res.StatusCode)
	}
}
