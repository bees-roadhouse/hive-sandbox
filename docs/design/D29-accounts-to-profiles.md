# D29: accounts bind to profiles, and the layers between them are named

**Inputs**, relayed from Nate on 2026-09-02, late: we should be able to map
Claude subscriptions (accounts) to multiple Claude profiles. With a warning
attached: **a subscription's MCP servers come through authenticated.** A
profile signed in with an account gets every connector that account carries,
with that account's credentials behind them. Be careful; define the isolation
layers between personal, work, and other orgs. This entry records the
requirement, the warning, and the shape the invariants force. It is not a
finished design; the open items are at the end.

## What is true on the host today

- A profile is one `CLAUDE_CONFIG_DIR`: its own `.credentials.json` (a real
  file, opened `O_NOFOLLOW`), settings, `CLAUDE.md`, memory, sessions. Profile
  and account are one-to-one by construction, and the only way to share an
  account is to log the second profile in by hand.
- The account is the boundary the harness enforces. A claude.ai connector
  (BookStack, Microsoft 365, Gmail, Home Assistant, ...) is attached to the
  account, arrives authenticated, and is offered to every session on that
  account regardless of which profile opened it.
- The personal account on this machine carries **both** knowledge bases and
  the work connectors. Pia (personal) and Apis (work) are therefore separated
  by *instruction* ("never mix the two"), not by mechanism. That is the leak
  the warning is about, and it is already the state of play.

## Decision: accounts are a record, binding is a grant, domains must match

- **An account is a platform record**: the subscription identity (email,
  plan, organisation) and a **trust domain**: `personal`, `dtc`, and one per
  other organisation. Domains are the isolation unit. The record holds no
  token, ever; D27's rule stands (definitions never credentials). How the
  token reaches a profile directory is `claude-profile`'s job on the host; the
  platform records *that* a profile is bound to an account and enforces the
  rule below.
- **Binding is many profiles to one account**, one account per profile at a
  time, recorded as an edge on the D27 graph so it is visible, revocable, and
  journaled like every other grant.
- **A profile's domain must equal its account's domain.** A cross-domain
  binding is refused, not warned about. The generated `CLAUDE.md` (D28) names
  the profile's domain in its header so a session can see which side of the
  house it is on without inferring it.
- **An account that spans domains is the defect**, not something to route
  around. The personal account carrying DTC connectors means that account is
  cross-domain; the fix is on the account side (one account per domain, or
  org-scoped connectors), and it is Nate's to make. Until it is made, Pia and
  Apis rely on the instruction layer, and this entry is where that weakness
  is written down rather than assumed away.

## The layers, named, weakest last

1. **Account.** What the harness will hand a session: connectors, org data,
   billing. Enforced by the harness. The only layer that actually stops a
   tool call.
2. **Profile.** Config dir, instructions, memory, sessions. Separates
   *context*, not access: a profile cannot forget what its account offers.
3. **Workspace.** Which checkouts, which git identity, which knowledge base
   the profile is pointed at. Separates *where work lands*.
4. **Instruction.** "Never mix." Softest; fails silently; the one everything
   depends on today.

A design that adds a layer 5 (the platform filtering an account's connectors
per profile) is tempting and is **not** taken here: the platform does not sit
between the harness and claude.ai, so a filter it applied would be advisory,
and an advisory filter is layer 4 with better marketing.

## What the briefing does about it (D28)

The wake-up briefing states the profile's domain and account, and, where the
host can read it, lists the connectors the session was started with. Anything
outside the profile's domain is flagged at the top of the briefing, above the
open threads. A visible mismatch is a leak someone will fix; a silent one is
a leak someone will use.

## What lost

- *Per-profile connector filtering in the platform*: advisory only, see above.
- *Sharing a profile directory between models or accounts*: the harness
  refuses symlinked credentials and duplicates manually added connectors
  (recorded on the host on 2026-08). One directory, one account.
- *Treating "other orgs" as one domain*: an MSP touches many tenants; they
  are separate domains or the model means nothing.

## Left open, deliberately

- Whether a session can read its account's connector list on the host at all;
  if not, the briefing can name the domain but not audit the connectors.
- Sub-domains under `dtc` for client tenants, and whether a profile can hold
  several at once or must be one per tenant.
- The login mechanics: whether `claude-profile` copies a credential file into
  a newly bound profile or drives a fresh OAuth login per profile. The former
  is faster and the latter is auditable; this entry does not pick.
