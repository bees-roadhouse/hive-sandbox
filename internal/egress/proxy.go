package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for a zero-value Proxy.
const (
	DefaultDialTimeout       = 10 * time.Second
	DefaultReadHeaderTimeout = 15 * time.Second
	DefaultResponseTimeout   = 5 * time.Minute

	// DefaultTunnelIdleTimeout closes a CONNECT tunnel that has carried no bytes
	// in either direction for this long.
	//
	// Generous, because an agent holding a streaming response open is normal and
	// cutting it would be worse than the leak. What it stops is the unbounded
	// case: a tunnel that neither side ever closes, which previously ran until
	// the harness deadline killed the container ... so a proxy outliving its run,
	// or serving a client that is not a harness, held sockets forever.
	DefaultTunnelIdleTimeout = 10 * time.Minute
)

// DenyHeader names the reason on a refused request, so a run's own logs say
// why rather than only showing a 403.
const DenyHeader = "X-Hive-Sandbox-Deny"

// Proxy is an allowlisting forward proxy.
//
// It handles the two things a CLI's HTTP client actually does through a proxy:
// absolute-form requests for http, and CONNECT for https. There is no TLS
// interception ... for CONNECT the destination host is all that is knowable, and
// that is what the allowlist is expressed in.
//
// The zero value denies everything. That is deliberate: a proxy that comes up
// misconfigured must be useless rather than open.
type Proxy struct {
	// Allow decides what may be reached. A nil Allow denies everything.
	Allow *Allowlist

	// RunID is attached to every decision log line.
	RunID string

	// Logger defaults to slog.Default().
	Logger *slog.Logger

	// Resolver defaults to one built from DNSServers, or net.DefaultResolver
	// when those are empty. An interface so a test can point a name at an
	// address it controls, which is the only honest way to exercise the
	// rebinding guard.
	Resolver Resolver

	// DNSServers are "host:port" resolvers to query directly, bypassing
	// resolv.conf.
	//
	// The proxy runs attached to an --internal Podman network, which points
	// resolv.conf at that network's aardvark-dns. Aardvark has no upstream to
	// forward to there, so it answers NXDOMAIN for every external name ... and
	// Go's resolver treats NXDOMAIN as authoritative and never tries the next
	// server, so adding --dns to the container does not help. Resolving here,
	// explicitly, is the fix, and it makes which resolvers the platform's
	// egress uses a decision rather than whatever the runtime wrote.
	DNSServers []string

	resolverOnce  sync.Once
	builtResolver Resolver

	DialTimeout time.Duration

	// TunnelIdleTimeout bounds an idle CONNECT tunnel. Zero uses
	// DefaultTunnelIdleTimeout.
	TunnelIdleTimeout time.Duration
}

func (p *Proxy) tunnelIdleTimeout() time.Duration {
	if p.TunnelIdleTimeout > 0 {
		return p.TunnelIdleTimeout
	}
	return DefaultTunnelIdleTimeout
}

func (p *Proxy) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// Resolver is the name-to-address lookup the proxy uses. *net.Resolver
// satisfies it.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

func (p *Proxy) resolver() Resolver {
	if p.Resolver != nil {
		return p.Resolver
	}

	p.resolverOnce.Do(func() {
		p.builtResolver = newResolver(p.DNSServers, p.dialTimeout())
	})
	return p.builtResolver
}

// newResolver queries the given servers directly, in order, falling back to the
// system resolver when none are configured.
func newResolver(servers []string, timeout time.Duration) Resolver {
	normalized := NormalizeDNSServers(servers)
	if len(normalized) == 0 {
		return net.DefaultResolver
	}

	return &net.Resolver{
		// The pure-Go resolver is what honours a custom Dial; cgo's would
		// ignore it and go back to resolv.conf.
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialer := net.Dialer{Timeout: timeout}
			var lastErr error
			for _, server := range normalized {
				conn, err := dialer.DialContext(ctx, network, server)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = errors.New("no DNS servers configured")
			}
			return nil, lastErr
		},
	}
}

// NormalizeDNSServers adds the default DNS port to bare addresses and drops
// empties.
func NormalizeDNSServers(servers []string) []string {
	out := make([]string, 0, len(servers))
	for _, raw := range servers {
		server := strings.TrimSpace(raw)
		if server == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(server); err != nil {
			server = net.JoinHostPort(strings.Trim(server, "[]"), "53")
		}
		out = append(out, server)
	}
	return out
}

func (p *Proxy) dialTimeout() time.Duration {
	if p.DialTimeout > 0 {
		return p.DialTimeout
	}
	return DefaultDialTimeout
}

// ServeHTTP implements the proxy.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleForward(w, r)
}

// deny refuses a request by policy and says why, once, in both the log and the
// response.
func (p *Proxy) deny(w http.ResponseWriter, r *http.Request, host string, port int, reason string) {
	// Some reasons quote the request's own host or scheme, so this string is
	// partly attacker-controlled. Sanitising once here covers the header (where
	// a stray CR would be response splitting) and the body together.
	reason = sanitizeReason(reason)

	p.logger().Warn("egress denied",
		"run_id", p.RunID,
		"method", r.Method,
		"host", host,
		"port", port,
		"reason", reason,
	)
	w.Header().Set(DenyHeader, reason)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	// The client is being refused; a failed write to it changes nothing.
	_, _ = w.Write([]byte("hive-sandbox egress denied: " + reason + "\n"))
}

// maxReasonBytes keeps a refusal short enough for a header and stops a long
// hostname turning into a long response.
const maxReasonBytes = 200

// sanitizeReason reduces a reason to printable ASCII.
//
// Not cosmetic: the reason can embed a hostname taken straight off the request,
// and a control character in it would be header injection on the way out.
func sanitizeReason(reason string) string {
	var b strings.Builder
	for _, r := range reason {
		switch {
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
		if b.Len() >= maxReasonBytes {
			break
		}
	}
	return b.String()
}

// failUpstream reports that an allowed destination could not be reached.
//
// Deliberately not a 403. The allowlist saying yes and the network saying no
// are different facts, and a proxy that returns the same status for both makes
// its own misconfiguration indistinguishable from correct enforcement.
func (p *Proxy) failUpstream(w http.ResponseWriter, r *http.Request, host string, port int, err error) {
	p.logger().Error("egress upstream failed",
		"run_id", p.RunID,
		"method", r.Method,
		"host", host,
		"port", port,
		"err", err,
	)
	w.Header().Set(DenyHeader, "upstream unreachable")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusBadGateway)
	// The error text can quote the request's host, so it gets the same
	// treatment. The full error is in the log line above.
	_, _ = w.Write([]byte("hive-sandbox egress: upstream unreachable: " + sanitizeReason(err.Error()) + "\n"))
}

// reject routes a dial failure to the right status.
func (p *Proxy) reject(w http.ResponseWriter, r *http.Request, host string, port int, err error) {
	var denied *DeniedError
	if errors.As(err, &denied) {
		p.deny(w, r, host, port, denied.Reason)
		return
	}
	p.failUpstream(w, r, host, port, err)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := splitHostPort(r.Host, 443)
	if err != nil {
		p.deny(w, r, r.Host, 0, "malformed CONNECT target")
		return
	}

	rule, allowed := p.Allow.Match(host, port)
	if !allowed {
		p.deny(w, r, host, port, "host not on the run's allowlist")
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "proxy cannot hijack", http.StatusInternalServerError)
		return
	}

	// Dial before hijacking: once the connection is hijacked there is no
	// ResponseWriter left to report a failure through.
	upstream, err := p.dial(r.Context(), rule, host, port)
	if err != nil {
		p.reject(w, r, host, port, err)
		return
	}

	client, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		p.logger().Error("hijack failed", "run_id", p.RunID, "err", err)
		return
	}

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}

	p.logger().Info("egress allowed",
		"run_id", p.RunID, "method", "CONNECT", "host", host, "port", port)

	// Anything the client pipelined behind the CONNECT is already in the
	// buffered reader and would be lost by reading the socket directly.
	go tunnel(client, buffered, upstream, p.tunnelIdleTimeout())
}

func (p *Proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() {
		// A relative URI means something addressed the proxy as an origin
		// server. Nothing legitimate does that here.
		p.deny(w, r, r.Host, 0, "not a proxy request: expected an absolute URI")
		return
	}
	if r.URL.Scheme != "http" {
		p.deny(w, r, r.URL.Hostname(), 0, "scheme "+r.URL.Scheme+" must use CONNECT")
		return
	}

	host, port, err := splitHostPort(r.URL.Host, 80)
	if err != nil {
		p.deny(w, r, r.URL.Host, 0, "malformed target")
		return
	}
	rule, allowed := p.Allow.Match(host, port)
	if !allowed {
		p.deny(w, r, host, port, "host not on the run's allowlist")
		return
	}

	outbound := r.Clone(r.Context())
	outbound.RequestURI = ""
	// Hop-by-hop headers are ours, not the origin's.
	outbound.Header = r.Header.Clone()
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive",
		"Transfer-Encoding", "TE", "Trailer", "Upgrade"} {
		outbound.Header.Del(h)
	}

	resp, err := p.roundTripperFor(rule).RoundTrip(outbound)
	if err != nil {
		p.reject(w, r, host, port, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	p.logger().Info("egress allowed",
		"run_id", p.RunID, "method", r.Method, "host", host, "port", port, "status", resp.StatusCode)

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// roundTripperFor builds a transport whose dialer is bound to one rule.
//
// Per request rather than cached, deliberately. A shared transport would pool
// connections across rules, so a later request under a stricter rule could
// reuse a connection opened under a looser one ... which is the address check
// being skipped by the connection pool.
func (p *Proxy) roundTripperFor(rule Rule) *http.Transport {
	return &http.Transport{
		// Every dial goes through the guard, so a redirect cannot reach an
		// address the rule would refuse.
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, port, err := splitHostPort(addr, 80)
			if err != nil {
				return nil, err
			}
			return p.dial(ctx, rule, host, port)
		},
		ResponseHeaderTimeout: DefaultResponseTimeout,
		DisableKeepAlives:     true,
	}
}

// dial resolves a host and connects to an address the allowlist permits.
//
// Resolution happens once here and the connection is made to the resolved IP,
// never by handing the name back to the stack. Checking a name and then dialling
// it again leaves a window where the second lookup returns something else, which
// is exactly how a rebinding attack turns an allowlisted host into a request
// against the loopback interface.
func (p *Proxy) dial(ctx context.Context, rule Rule, host string, port int) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, p.dialTimeout())
	defer cancel()

	var candidates []net.IP
	if literal := net.ParseIP(host); literal != nil {
		candidates = []net.IP{literal}
	} else {
		addrs, err := p.resolver().LookupIPAddr(dialCtx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
		for _, a := range addrs {
			candidates = append(candidates, a.IP)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}

	dialer := net.Dialer{Timeout: p.dialTimeout()}
	var lastErr error
	for _, ip := range candidates {
		if err := p.Allow.PermitsAddr(rule, ip); err != nil {
			lastErr = err
			continue
		}
		conn, err := dialer.DialContext(dialCtx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		if err != nil {
			lastErr = err
			continue
		}
		return conn, nil
	}

	if lastErr == nil {
		lastErr = errors.New("no usable address")
	}
	return nil, lastErr
}

// tunnel copies bytes both ways until either side is done or the tunnel goes
// idle.
//
// The idle bound is the point. Without it a tunnel neither side ever closes
// runs forever, and the only thing that ended one was the harness deadline
// killing the whole container ... which is not a bound the proxy owns, and is
// no bound at all for a proxy that outlives its run.
//
// Idleness is measured by activity, not by total duration: a long download and
// a long-polling stream both keep resetting it, so the timeout only fires on a
// tunnel that has genuinely stopped.
func tunnel(client net.Conn, buffered io.Reader, upstream net.Conn, idle time.Duration) {
	defer func() { _ = client.Close() }()
	defer func() { _ = upstream.Close() }()

	var active atomic.Bool
	done := make(chan struct{}, 2)

	copyWithActivity := func(dst io.Writer, src io.Reader, closeWrite any) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				active.Store(true)
				if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		// Half-close so the far side sees end-of-stream rather than waiting.
		if cw, ok := closeWrite.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}

	go copyWithActivity(upstream, buffered, upstream)
	go copyWithActivity(client, upstream, client)

	// Watch for a tunnel that has stopped moving bytes. Closing both sides
	// unblocks the two copies, which then finish normally.
	watchdog := time.NewTicker(idle)
	defer watchdog.Stop()

	finished := 0
	for finished < 2 {
		select {
		case <-done:
			finished++
		case <-watchdog.C:
			// Swap-and-test: any byte since the last tick resets the clock.
			if active.Swap(false) {
				continue
			}
			_ = client.Close()
			_ = upstream.Close()
			// Both copies will now fail their reads and report in.
		}
	}
}

// splitHostPort accepts "host", "host:port" and "[::1]:port".
func splitHostPort(target string, defaultPort int) (string, int, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", 0, errors.New("empty target")
	}

	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		host = strings.TrimSuffix(strings.TrimPrefix(target, "["), "]")
		if host == "" {
			return "", 0, errors.New("empty host")
		}
		return strings.ToLower(host), defaultPort, nil
	}

	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("bad port %q", portText)
	}
	return strings.ToLower(host), port, nil
}

// Server wraps a Proxy in an http.Server with timeouts set.
func (p *Proxy) Server(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           p,
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
	}
}
