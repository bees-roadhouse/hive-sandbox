# D26: five open items, argued and settled

**Decided** 2026-09-02, by Pia at Nate's instruction ("figure the open items
out yourselves ... record a decision to move forward with"), relayed by a
second session. Argued in one session against the alternatives rather than
between agents: every one of these turns on a constraint already in the repo,
and the record below shows the options that lost, which is what a second
voice would have added.

## 1. Where the default instance's own repository lives

**Decision.** The instance repository is separate from the platform
repository. At first boot the daemon takes its URL out of band
(`HIVE_SANDBOX_INSTANCE_REPO`, beside the bootstrap handle and token, D19.1)
and registers it as the first source, owned by the root principal, tracking
`main`, apps under `apps/<slug>/`. **With no URL, the daemon initialises a
bare repository in its own data directory and that is the instance
repository**: a fresh install works with no forge at all, which is the
household default. A forge can be adopted later by adding it as the remote and
pushing the history up; PR-gated mode needs a forge and says so.

**What lost.** *The platform repository doubles as the instance repository*
(apps under this repo): it publishes the household's apps with the platform,
makes every household edit a platform commit, and every other instance a fork.
*The daemon as a git host* (serving the repository over HTTP or SSH): a second
product with its own authentication for what a bare repository in the data
directory plus an optional remote already gives. Its kernel, the local bare
repository when no forge exists, is kept.

## 2. Whether the host builds modules or verifies a CI-built artifact

**Decision.** **The host builds.** A build is a harness run with a toolchain
image instead of an agent image: source at the tracked commit mounted
read-only, `--network=none` unless the manifest declares dependencies, in
which case fetches go through the egress allowlist like any other run. The
build row records the source commit, the toolchain image digest and the
resulting module hash, and the registry installs the content-addressed module
exactly as it does today. A build cache is keyed on the app directory's tree
hash **and** the toolchain image digest (invariant 14: a cache that omitted
the toolchain would serve a module the current toolchain would not produce).

**What lost.** *Verify-only, a CI-built artifact named by hash in the
manifest*: the household default has no CI, so the default instance could
not deploy; a hash is identity, not provenance, and a host that only verifies
is trusting a third party's compiler for code it will run with capabilities,
which is invariant 3's shape one step removed; and the builder loop (#27), the
first proof this platform has to give, needs a build the host can run
locally. Kept as a *cache source* only if it ever matters: an artifact whose
hash matches what the host would build is a build skipped, never a build
trusted. *Both as two paths*: two code paths drift. The relayed suggestion
called verify-only the halfway house; it is dropped rather than kept beside.

## 3. Where a source's checkout lives, so it is never guest-reachable

**Decision.** `<data>/sources/<source id>/repo.git`, a bare clone per source,
keyed by the source's uuid and never by a name derived from the URL or the
owner (invariant 14 again; the per-install schema name was derived from the
app alone once). A build materialises a detached worktree under
`<data>/builds/<build id>/` and that directory alone is mounted, read-only, at
`/src` in the build container; nothing under `sources/` is ever mounted or
served. The daemon checks at boot that `sources/`, `builds/`, the chat
workspaces and the disk blob root are pairwise disjoint and refuses to start
otherwise, because everything decidable at boot is decided at boot. Fetch
credentials come from the vault per fetch and are never written into the
clone's configuration, for the reason `docs/vault.md` gives: anything written
into a persisted file is captured and restored into every future run.

**What lost.** *A working clone per source*: a writable tree the builder
loop would be tempted to build in place, and one that a stray bind mount
exposes whole. *Git objects in Postgres*: solves nothing git does not already
solve and makes every fetch a custom transport.

## 4. Who an agent message is from

**Decision.** **One AI actor per (runtime, principal)**, and the status quo
(agent messages attributed to the conversation's author) is a defect to fix,
not a state to hold. The actor is created lazily inside the chat layer the
first time a principal starts a conversation with a runtime, by the person who
is that principal or, for an org, by an admin of it: that is exactly who
`actors_creation_policy` (D19.2) permits, because creating an AI that can act
for a principal confers authority and only that principal's person may confer
it. An org member who is not an admin is told the org has no agent for that
runtime yet. Handle `<runtime>@<principal handle>`, persona the runtime name.

From then on the worker acts as that actor for that principal, which is
invariant 2's shape without bending: agent messages and run rows carry the AI
actor as author and the conversation's owner as principal. **Every run gets
its own credential** for that actor, minted at run start and revoked at run
end, so a call the agent makes back to the daemon over the socket is
attributed the same way and a leaked token dies with its run.

**What lost.** *Hold the status quo until "a per-runtime actor exists"*: it
records "Nate said this" for text an AI produced, and every later reader (a
search, an export, another turn reading the thread as context) inherits the
lie, which is the one distinction invariant 2 exists to keep. *One
platform-wide actor per runtime*: an AI actor has exactly one principal
(D13.9), so a global "claude" could act for nobody. *One actor per
conversation*: an actor explosion, and the identity that matters is "which
agent for whom", not "which thread".

Implemented next in the Go tree, because that daemon runs until parity, and
carried into the Rust port as a test before the chat crate exists.

## 5. The session cookie's `Secure` flag

**Decision.** **`Secure` by default.** A deployment that serves plain HTTP
says so once, in configuration (`-plain-http`, env `HIVE_SANDBOX_PLAIN_HTTP`),
and the daemon then omits `Secure` and logs a warning at every boot naming
what that means. No scheme sniffing and no `X-Forwarded-Proto`: the security
property of the cookie comes from the deployment, decided by its operator,
never from a request the network can shape. `SameSite=Strict` and `HttpOnly`
are unconditional. Over plain HTTP without the flag the browser drops the
cookie and the operator sees a sign-in page that never goes away, which is
the moment the choice is supposed to be made.

**What lost.** *Follow the request's scheme* (the state until now): decides a
security property from attacker-influenced input, is silent, and hides the
moment an operator should have chosen. *Always Secure, no exception*: makes
the household deployment impossible until there is a TLS story, and the
desktop doc puts TLS termination in a later phase. *A trusted-proxy header*:
a proxy that terminates TLS and says so is a legitimate deployment, but it is
the same decision as `-plain-http` seen from the other side and is expressed
by the same flag being **absent**; a header adds a second place to be wrong.
