# The credential vault

Status: **design, not built.** Written down now because several things already
in the tree assume it exists, and because the shape it needs is constrained by
decisions already made.

## What it is for

An agent run needs credentials it must not be able to keep. A Claude Code
harness needs an API key. A tool an agent calls needs a token for a third
party. A workflow step needs to authenticate to something. Today there is
nowhere for any of that to live, so the only options are an environment
variable set by whoever launched the daemon — visible to every run — or a file
in an image.

Both are wrong in the same way: the credential outlives the reason it was
handed over.

## Why this cannot be "just mount the secrets"

Two things already in this repo make the naive version dangerous.

**Mutable harness images.** The plan is for a run to be able to install tools
and for that environment to be snapshotted so a user can revert. A credential
written into the container filesystem is therefore captured in the snapshot,
restored into every future session, and carried anywhere that image is pushed.
The leak has no symptom and a very long tail.

The mitigation has to be structural rather than procedural: credentials reach a
run through **tmpfs or the environment, never a layer**. `podman commit` does
not capture a tmpfs mount, so a snapshot is clean by construction and not by
remembering.

**Invariant 2.** A credential names whose authority is being spent. "Nate's
API key" and "an AI acting for Nate, using Nate's API key" are different facts,
and the vault has to preserve the distinction it is handed rather than
flattening both to "someone had the key". A lease is issued to an ACTOR acting
for a PRINCIPAL, and both belong on the row.

## Shape

A vault entry is owned, versioned and leased:

- **Owned** by a principal (`owner_kind`, `owner_id`), so absence of scope is
  deny like everything else. There is no global secret.
- **Versioned**, so rotation is adding a version rather than overwriting. A run
  that is mid-flight keeps working, and "which version did that run use" stays
  answerable.
- **Leased** rather than read. A run does not fetch a secret; it is granted a
  time-boxed lease that the daemon injects, and the lease is recorded against
  the run. Revocation then means expiring leases rather than hunting for copies.

The value is encrypted at rest with a key the daemon derives at boot and never
writes. The sibling `hive` repo already does this with scrypt; there is no
reason to invent a second scheme.

## Pluggable, and pluggable through WASM

The vault is a **seam, not a store**. The default backend keeps entries in
Postgres, encrypted, because a self-hosted deployment should need nothing else.
But the interesting credentials for a household already live somewhere:

- **1Password** — `op` is already how this fleet reads secrets at launch, and
  service accounts exist for exactly this
- **Bitwarden** — same shape, and the self-hosted case matters for people who
  will not put a vault in someone else's cloud

Out-of-the-box support for both is the goal. Neither should be a special case
in the daemon.

**The backend is a WASM guest**, on the same ABI as every other app. That
follows from what the ABI already guarantees rather than from a wish to be
uniform:

- a guest holds no sockets and no ambient state (invariant 5), so a vault
  backend cannot quietly cache a secret somewhere the host cannot see
- every capability response is `{trust, data}` (invariant 12), so a secret that
  came from outside is marked as such and cannot silently reach instruction
  position
- egress for a backend that talks to a remote vault goes through the
  allowlisting proxy, so "the 1Password backend" can reach 1Password and
  nothing else — a property no in-process Go client can offer

The host contract is small enough to be worth keeping small: resolve a
reference to a lease, and report what the lease is for. Anything more and the
backend starts making authorization decisions, which belong to the one
enforcement point (invariant 1).

## What has to be true before this is built

- Leases are recorded against `agent_runs`, so "what did this run have access
  to" is answerable after the fact. That table exists as of 0002.
- The egress allowlist can be widened per run, since a remote-vault backend
  needs to reach exactly one host.
- Nothing is mounted into a harness image. See above; this is the one that is
  easy to get wrong late, when the snapshot feature makes it tempting.

## Open questions

- Does a lease survive a run's restart, or does a reclaimed run get a new one?
  Invariant 10 says a reclaimed money-spending run lands `indeterminate` and
  does not re-fire, which suggests the lease should not be reissued
  automatically either.
- Can an agent request a credential it was not given, and be denied — or should
  it not be able to name one at all? The second is safer and less useful.
