# Egress proxy

The allowlisting proxy a harness run reaches the internet through (D12.6,
D12.10, D23). It completes the `NetworkProxied` seam that shipped fail-closed in
`a22b658`.

## The shape

```
   harness container                     proxy container
   ┌──────────────────┐                  ┌──────────────────────┐
   │ no route out     │  eth0            │ eth0  internal net   │
   │ HTTP(S)_PROXY=── │──────────────────│                      │
   │                  │  internal net    │ eth1  uplink ────────│──► allowed hosts
   └──────────────────┘  (--internal)    └──────────────────────┘
```

A run gets its **own** internal Podman network, named after the run, and its own
proxy. Both are created before the agent starts and removed when it ends.

The harness has **no default route**. That is what makes this enforcement rather
than configuration: an agent that ignores `HTTP_PROXY` does not escape, it
fails. `TestEgressProxyEnforcesTheAllowlist` asserts exactly that, by trying to
reach a bare IP with `--noproxy '*'` and requiring it to fail.

The proxy sits on the internal network *and* an uplink, so it is the only thing
in the run that can reach anything, and it reaches only what the run allowlisted.

## Using it

```go
spec := harness.RunSpec{
    RunID:       runID,
    Runtime:     harness.RuntimeClaude,
    Network:     harness.NetworkProxied,
    EgressAllow: []string{"api.anthropic.com", "*.githubusercontent.com", "registry.npmjs.org:443"},
    // ... workspace, limits, deadline
}

pin, err := harness.LoadEgressPin(harness.DefaultEgressPinPath)
launcher := &harness.PodmanLauncher{EgressImage: pin.Ref()}
```

Build the image first:

```powershell
.\scripts\egress-build.ps1
```

```bash
./scripts/egress-build.sh
```

It is the same `hive-sandbox` binary running its `-run-egress-proxy` role, on a
distroless base with no shell. One codebase, one place the allowlist logic
lives.

## Allowlist syntax

| Entry | Means |
| --- | --- |
| `example.com` | that host, on ports 80 and 443 |
| `example.com:8443` | that host, on that port only |
| `*.example.com` | subdomains, **not** `example.com` itself |
| `192.0.2.10` | an address literal |

Absence is deny. `NetworkProxied` with an empty `EgressAllow` is an **error**,
not a permissive default ... wanting no egress at all is `NetworkNone`, and
saying so is free.

A wildcard is only valid as a leading `*.` label, and a bare `*` is refused
outright: an allowlist that allows everything should be spelled out or not
written.

## Two controls, not one

**Is this host on the list** is answered before anything is dialled.

**Is this address one a run may reach** is answered after resolution, against
the address actually being connected to. Loopback, RFC1918, link-local
(`169.254.169.254` is the classic target), ULA and carrier-grade NAT are all
refused unless `EgressAllowPrivate` is set.

The second control exists because the first is not enough. Allowlisting
`metrics.example.com` says nothing about where that name resolves, and whoever
controls the DNS answer controls the destination. So the proxy resolves once and
**dials the resolved address**, never handing the name back to the stack ...
checking a name and then dialling it again leaves a window where the second
lookup returns something else.

`EgressAllowPrivate` exists because LAN targets are a real need (tools.md's local
browser driver). It is a deliberate widening and off by default.

## 403 versus 502

A policy denial is **403**. An allowed host that could not be reached is **502**.

They were briefly the same status, and it cost real time: a DNS failure inside
the proxy looked exactly like a correctly enforced allowlist. The refusing path
returns `*egress.DeniedError` so the two cannot be conflated again, and
`TestProxyDistinguishesDenialFromUnreachable` holds the line.

Both carry `X-Hive-Sandbox-Deny` with the reason, and every decision is logged
with the run id, so a run's own logs say why rather than only showing a status.

## The DNS thing

The proxy resolves through servers it is told about (`-egress-dns`, from
`harness.DefaultEgressDNS`), **in its own code**, bypassing `resolv.conf`
entirely.

It has to. A container attached to an `--internal` network gets its
`resolv.conf` pointed at that network's `aardvark-dns`, and aardvark on an
internal network has no upstream to forward to, so every external name comes
back NXDOMAIN even though the uplink interface routes perfectly well:

```
ERROR egress upstream failed host=example.com
      err="resolve example.com: lookup example.com on 10.89.6.1:53: no such host"
```

**Podman's `--dns` flag does not fix this**, which was the first thing tried.
Aardvark still goes first in `resolv.conf`, it answers NXDOMAIN, and Go's
resolver treats NXDOMAIN as authoritative and never falls through to the next
server. A libc-based client in the same container appears to work, which makes
the failure look intermittent when it is not.

So the proxy builds its own `net.Resolver` with `PreferGo: true` and a `Dial`
that goes straight to the configured servers. That also turns "which DNS does
this platform's egress use" into a decision rather than whatever the container
runtime happened to write.

Worth noting how this was found: the 403/502 split above is what surfaced it. As
one status it looked like the allowlist correctly refusing a host that was on
it, which is a contradiction nobody reads carefully at first. As a 502 with the
resolver's own error attached, it took one log line.

## What is not built

**Per-run proxy credentials.** The proxy has no authentication because it does
not need any: it is on a network only one run can reach. If a proxy ever becomes
shared between runs, it needs to identify the caller before it can apply the
right allowlist, and that is a different design.

**TLS interception.** Deliberately never. For CONNECT the destination host is
all that is knowable, and that is what the allowlist is expressed in. Terminating
TLS to filter URLs would put the platform in the position of holding every
run's plaintext.
